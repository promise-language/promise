package common

import (
	"bytes"
	"fmt"
	"go/format"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RunFormat formats Go code (gofmt) and Promise code (promise format).
// This is the main implementation — called by bin/format and internally
// by other tools (e.g., verify) without spawning a subprocess.
func RunFormat(root string, args []string) error {
	start := time.Now()

	promiseBin := filepath.Join(root, "bin", BinaryName())

	// 1. Format Go code
	fmt.Println("Formatting Go...")
	if err := FormatGo(root); err != nil {
		return fmt.Errorf("gofmt: %w", err)
	}

	// 2. Format Promise code (requires bin/promise to exist)
	if !Exists(promiseBin) {
		fmt.Println("Skipping Promise format (bin/promise not found — run bin/build first)")
	} else {
		fmt.Println("Formatting Promise...")
		prFiles, err := findPromiseFiles(root)
		if err != nil {
			return fmt.Errorf("find .pr files: %w", err)
		}
		if len(prFiles) > 0 {
			fmtArgs := append([]string{"format"}, prFiles...)
			if err := RunIn(root, promiseBin, fmtArgs...); err != nil {
				return fmt.Errorf("promise format: %w", err)
			}
		}
	}

	elapsed := time.Since(start).Round(time.Millisecond)
	fmt.Printf("Formatted in %s\n", elapsed)
	return nil
}

// findPromiseFiles returns all .pr files in the project, excluding
// .git/, compiler/, and .promise-home/ directories.
func findPromiseFiles(root string) ([]string, error) {
	excludeDirs := map[string]bool{
		".git":          true,
		"compiler":      true,
		".promise-home": true,
	}

	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if excludeDirs[name] {
				return filepath.SkipDir
			}
			// Also skip hidden directories (except root)
			if strings.HasPrefix(name, ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".pr") {
			// Use path relative to root for cleaner output
			rel, _ := filepath.Rel(root, path)
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// FormatPromiseFiles is a convenience for verify — formats Promise files
// using the given promise binary path. Returns silently if no files found.
func FormatPromiseFiles(root, promiseBin string) error {
	prFiles, err := findPromiseFiles(root)
	if err != nil {
		return err
	}
	if len(prFiles) == 0 {
		return nil
	}
	fmtArgs := append([]string{"format"}, prFiles...)
	return RunIn(root, promiseBin, fmtArgs...)
}

// goFileDirs returns the directories to scan for Go source files.
// flows/ is included only when flows/go.mod is present (feature branch).
func goFileDirs(root string) []string {
	dirs := []string{
		filepath.Join(root, "compiler"),
		filepath.Join(root, "tools", "build"),
	}
	if Exists(filepath.Join(root, "flows", "go.mod")) {
		dirs = append(dirs, filepath.Join(root, "flows"))
	}
	return dirs
}

// FormatGo formats all Go files under compiler/, tools/build/, and flows/ (when present)
// using go/format.Source(). On Windows, it preserves original line endings to avoid
// spurious diffs when git is configured with core.autocrlf=true.
func FormatGo(root string) error {
	for _, dir := range goFileDirs(root) {
		if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if name == "vendor" || name == ".git" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".go") {
				return nil
			}

			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}

			// Normalize CRLF→LF before formatting so comparison is line-ending-agnostic.
			hasCRLF := bytes.Contains(src, []byte("\r\n"))
			srcLF := bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))

			out, err := format.Source(srcLF)
			if err != nil {
				// Skip files that don't parse (e.g., generated code with build tags)
				return nil
			}

			if bytes.Equal(out, srcLF) {
				return nil // already formatted
			}

			// If original had CRLF, restore CRLF in output so git doesn't see a diff.
			if hasCRLF {
				out = bytes.ReplaceAll(out, []byte("\n"), []byte("\r\n"))
			}

			perm := os.FileMode(0644)
			if fi, e := os.Stat(path); e == nil {
				perm = fi.Mode().Perm()
			}
			return os.WriteFile(path, out, perm)
		}); err != nil {
			return err
		}
	}
	return nil
}

// EmbedFormattedResources re-embeds resources after formatting so the next
// build includes the formatted source. This is what verify does between
// format and build.
func EmbedFormattedResources(root string) error {
	return EmbedResources(root)
}

// UnformattedGoFiles returns the repo-relative paths of Go files under
// compiler/, tools/build/, and flows/ (when present) that gofmt would reformat,
// WITHOUT modifying them. It runs the same go/format pass as FormatGo entirely
// in-process — no subprocess, no exit-code inspection — and just compares the
// result instead of writing it.
//
// The pre-commit gate uses this to reject commits that contain unformatted Go.
// Otherwise unformatted code reaches origin and shows up as a spurious diff the
// next time someone runs bin/verify (which reformats in place). Comparison is
// line-ending-agnostic (CRLF normalized to LF first), matching FormatGo.
func UnformattedGoFiles(root string) ([]string, error) {
	var unformatted []string
	for _, dir := range goFileDirs(root) {
		if !Exists(dir) {
			continue // skip missing dirs (e.g. a test temp repo without all dirs)
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if name == "vendor" || name == ".git" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".go") {
				return nil
			}

			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			srcLF := bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
			out, err := format.Source(srcLF)
			if err != nil {
				return nil // unparseable (e.g. generated code) — skip, mirrors FormatGo
			}
			if !bytes.Equal(out, srcLF) {
				rel, _ := filepath.Rel(root, path)
				unformatted = append(unformatted, rel)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return unformatted, nil
}

// UnformattedPromiseFiles returns the repo-relative paths of .pr files that
// `promise format` would reformat, WITHOUT modifying them. Unlike the Go check
// (run in-process via go/format), the Promise formatter lives in the compiler
// binary — a separate module that the build tools can't import — so this shells
// out to `bin/promise format -check`, exactly as bin/verify shells out to the
// formatter. Returns nil (skips) when bin/promise has not been built yet, since
// there is no way to check without the compiler (mirrors RunFormat).
func UnformattedPromiseFiles(root string) ([]string, error) {
	promiseBin := filepath.Join(root, "bin", BinaryName())
	if !Exists(promiseBin) {
		return nil, nil
	}
	prFiles, err := findPromiseFiles(root)
	if err != nil {
		return nil, err
	}
	if len(prFiles) == 0 {
		return nil, nil
	}

	// `promise format -check` prints each unformatted file to stdout and exits 1
	// when any need formatting. A non-zero exit with file output is the expected
	// "unformatted" signal, not a tool failure — so we parse stdout and only
	// surface runErr when nothing was printed (a genuine failure, e.g. a read
	// error the CLI reports before exiting non-zero).
	args := append([]string{"format", "-check"}, prFiles...)
	out, runErr := RunCaptureStdout(root, promiseBin, args...)
	out = strings.TrimSpace(out)
	if out == "" {
		if runErr != nil {
			return nil, fmt.Errorf("promise format -check: %w", runErr)
		}
		return nil, nil
	}

	var files []string
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}
