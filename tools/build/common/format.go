package common

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// RunFormat formats Go code (gofmt) and Promise code (promise format).
// This is the main implementation — called by bin/format and internally
// by other tools (e.g., verify) without spawning a subprocess.
func RunFormat(root string, args []string) error {
	start := time.Now()

	compilerDir := filepath.Join(root, "compiler")
	promiseBin := filepath.Join(root, "bin", BinaryName())

	// 1. Format Go code
	fmt.Println("Formatting Go...")
	if err := RunIn(compilerDir, "gofmt", "-w", "."); err != nil {
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

// FormatGo runs gofmt on the compiler directory.
func FormatGo(root string) error {
	return RunIn(filepath.Join(root, "compiler"), "gofmt", "-w", ".")
}

// EmbedFormattedResources re-embeds resources after formatting so the next
// build includes the formatted source. This is what verify does between
// format and build.
func EmbedFormattedResources(root string) error {
	return EmbedResources(root)
}
