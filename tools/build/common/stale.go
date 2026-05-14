package common

import (
	"fmt"
	"os"
	"path/filepath"
)

// CheckStale compares the compiled-in source hash against the current source
// hash. If they differ, it prints an error and exits. Call this at the start
// of every tool's main().
func CheckStale(compiledHash string) {
	if compiledHash == "dev" {
		// Running via "go run" — skip staleness check.
		return
	}
	root, err := FindRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	currentHash, err := ToolsSourceHash(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if compiledHash != currentHash {
		if IsWindows() {
			abs := filepath.Join(root, "make.cmd")
			fmt.Fprintf(os.Stderr, "tools source has changed — run: .\\make.cmd (or %s)\n", abs)
		} else {
			abs := filepath.Join(root, "make")
			fmt.Fprintf(os.Stderr, "tools source has changed — run: ./make (or %s)\n", abs)
		}
		os.Exit(1)
	}
}
