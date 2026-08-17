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
		if err := FormatPromiseFiles(root, promiseBin); err != nil {
			return fmt.Errorf("promise format: %w", err)
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

// maxCommandLine bounds the rendered length of a single child command line.
// Windows' CreateProcessW hard-limits lpCommandLine to 32,767 characters and
// returns ERROR_FILENAME_EXCED_RANGE ("The filename or extension is too long")
// beyond that; the repo's 864 .pr files rendered 32,826 characters (T1582).
// 30,000 leaves headroom for the absolute exe path, flags, separators and
// per-argument quoting. POSIX ARG_MAX is ~2 MB, so batching there is harmless —
// it just means a couple of invocations instead of one, on every platform.
const maxCommandLine = 30000

// argCost is an upper bound on the command-line length one argument consumes:
// its own bytes, the separating space, and the two quotes Go's syscall.EscapeArg
// may add on Windows. Byte length also bounds the UTF-16 length Windows actually
// counts (a non-ASCII rune is 2–4 UTF-8 bytes but only 1–2 UTF-16 units), so the
// estimate stays conservative for non-ASCII paths. Over-estimating is safe —
// correctness only needs a bound, and it just yields slightly smaller batches.
func argCost(s string) int { return len(s) + 3 }

// commandLineBatches splits files into consecutive batches such that the command
// line for `exe fixed... batch...` stays within budget. Order is preserved and
// every input path appears in exactly one batch: a single path that cannot fit
// on its own still gets its own batch rather than being dropped, so the OS
// reports a real error instead of the file being silently skipped.
func commandLineBatches(exe string, fixed, files []string, budget int) [][]string {
	if len(files) == 0 {
		return nil
	}
	fixedCost := argCost(exe)
	for _, f := range fixed {
		fixedCost += argCost(f)
	}

	var batches [][]string
	var batch []string
	cost := fixedCost
	for _, f := range files {
		c := argCost(f)
		if len(batch) > 0 && cost+c > budget {
			batches = append(batches, batch)
			batch = nil
			cost = fixedCost
		}
		batch = append(batch, f)
		cost += c
	}
	return append(batches, batch)
}

// formatRunner runs one `promise format` batch with the given full argument
// list, returning the child's stdout. Injected so the batching logic is
// testable without a real compiler binary.
type formatRunner func(args []string) (stdout string, err error)

// formatPromiseFilesWith formats files in command-line-sized batches, aborting
// on the first batch that fails.
func formatPromiseFilesWith(promiseBin string, files []string, run formatRunner) error {
	fixed := []string{"format"}
	batches := commandLineBatches(promiseBin, fixed, files, maxCommandLine)
	for i, batch := range batches {
		if _, err := run(append(append([]string{}, fixed...), batch...)); err != nil {
			return fmt.Errorf("batch %d/%d, %d files: %w", i+1, len(batches), len(batch), err)
		}
	}
	return nil
}

// unformattedPromiseFilesWith runs `format -check` in command-line-sized batches
// and accumulates the reported paths across all of them, so the caller sees every
// unformatted file rather than only those in the first batch that reported any.
//
// Per-batch failure semantics match a single unbatched run: `promise format -check`
// prints each unformatted file to stdout and exits non-zero when any need
// formatting, so a non-zero exit *with* output is the expected "unformatted"
// signal, not a tool failure. Only an empty stdout together with a non-zero exit
// is a genuine failure (e.g. a read error the CLI reports before exiting).
func unformattedPromiseFilesWith(promiseBin string, files []string, run formatRunner) ([]string, error) {
	fixed := []string{"format", "-check"}
	batches := commandLineBatches(promiseBin, fixed, files, maxCommandLine)

	var unformatted []string
	for i, batch := range batches {
		out, runErr := run(append(append([]string{}, fixed...), batch...))
		out = strings.TrimSpace(out)
		if out == "" {
			if runErr != nil {
				return nil, fmt.Errorf("promise format -check (batch %d/%d): %w", i+1, len(batches), runErr)
			}
			continue
		}
		for line := range strings.SplitSeq(out, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				unformatted = append(unformatted, line)
			}
		}
	}
	return unformatted, nil
}

// FormatPromiseFiles is a convenience for verify — formats Promise files
// using the given promise binary path. Returns silently if no files found.
func FormatPromiseFiles(root, promiseBin string) error {
	prFiles, err := findPromiseFiles(root)
	if err != nil {
		return fmt.Errorf("find .pr files: %w", err)
	}
	if len(prFiles) == 0 {
		return nil
	}
	return formatPromiseFilesWith(promiseBin, prFiles, func(args []string) (string, error) {
		return "", RunInBrief(root, promiseBin, args...)
	})
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

	return unformattedPromiseFilesWith(promiseBin, prFiles, func(args []string) (string, error) {
		return RunCaptureStdout(root, promiseBin, args...)
	})
}
