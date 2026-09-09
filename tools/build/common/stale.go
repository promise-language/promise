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
		abs, relative := MakeCommands(root)
		fmt.Fprintf(os.Stderr, "tools source has changed — rebuild before continuing: %s (or %s from the repo root)\n", abs, relative)
		os.Exit(1)
	}
}

// MakeCommands returns the two spellings of this repository's bootstrap
// script: the absolute path, which resolves from any working directory, and
// the relative form, which only works at the repo root.
//
// Every staleness message names a rebuild command, and bin/guard *enforces*
// what it names — a message suggesting a command the same binary then refuses
// is what wedged a session in T1813. Defining the two spellings once means the
// guard's message and the tools' message cannot drift apart, and it is the only
// place that has to know Windows spells them differently.
func MakeCommands(root string) (abs, relative string) {
	if IsWindows() {
		return filepath.Join(root, "make.cmd"), `.\make.cmd`
	}
	return filepath.Join(root, "make"), "./make"
}
