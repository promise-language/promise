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
			if err := Run(promiseBin, fmtArgs...); err != nil {
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

// FormatGo formats all Go files under compiler/ using go/format.Source().
// On Windows, it preserves original line endings to avoid spurious diffs
// when git is configured with core.autocrlf=true.
func FormatGo(root string) error {
	compilerDir := filepath.Join(root, "compiler")
	return filepath.WalkDir(compilerDir, func(path string, d fs.DirEntry, err error) error {
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
	})
}

// EmbedFormattedResources re-embeds resources after formatting so the next
// build includes the formatted source. This is what verify does between
// format and build.
func EmbedFormattedResources(root string) error {
	return EmbedResources(root)
}
