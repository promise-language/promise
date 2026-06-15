package common

import (
	"fmt"
	"os"
	"path/filepath"
)

// CleanOptions configures what Clean removes.
type CleanOptions struct {
	// Shared targets ~/.promise instead of the repo-local .promise-home/.
	// Off by default — Clean intentionally avoids touching global state
	// unless the caller opts in.
	Shared bool
	// Quiet suppresses informational progress lines.
	Quiet bool
}

// CleanHome returns the Promise home directory targeted by the given options.
// shared=true → ~/.promise; shared=false → <root>/.promise-home.
func CleanHome(root string, shared bool) (string, error) {
	if shared {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(h, ".promise"), nil
	}
	return filepath.Join(root, ".promise-home"), nil
}

// Clean clears Promise build/test state to put the work tree in a pristine
// state. By default operates on the repo-local .promise-home/ and:
//   - removes the entire .promise-home/ directory (tmp/, cache/, anything else)
//   - runs `go clean -testcache`
//
// With opts.Shared=true, targets ~/.promise instead. Touching the global
// home means the next release build re-extracts the embedded LLVM tools.
//
// Clean acquires the verify lock before removing anything. Callers that
// already hold the lock (e.g. RunVerify with --clean) must use cleanLocked
// instead to avoid a same-process flock deadlock.
func Clean(root string, opts CleanOptions) error {
	unlock, err := acquireVerifyLock(root, 0)
	if err != nil {
		return fmt.Errorf("acquire verify lock: %w", err)
	}
	defer unlock()
	return cleanLocked(root, opts)
}

// cleanLocked performs the clean without acquiring the verify lock.
// Must only be called by callers that already hold it.
func cleanLocked(root string, opts CleanOptions) error {
	home, err := CleanHome(root, opts.Shared)
	if err != nil {
		return fmt.Errorf("resolve promise home: %w", err)
	}

	log := func(format string, args ...any) {
		if !opts.Quiet {
			fmt.Printf(format+"\n", args...)
		}
	}

	if Exists(home) {
		log("Clearing %s", home)
		if err := os.RemoveAll(home); err != nil {
			return fmt.Errorf("remove %s: %w", home, err)
		}
	}

	log("Clearing go test cache...")
	if err := RunIn(filepath.Join(root, "compiler"), "go", "clean", "-testcache"); err != nil {
		return fmt.Errorf("go clean -testcache: %w", err)
	}

	return nil
}

// RunClean is the bin/clean CLI entry. Defaults to the repo-local
// .promise-home/. Pass --shared to operate on ~/.promise instead.
func RunClean(root string, args []string) error {
	args = NormalizeArgs(args)
	var opts CleanOptions
	for _, arg := range args {
		switch arg {
		case "-shared":
			opts.Shared = true
		case "-local":
			// explicit local — no-op (default)
		case "-quiet":
			opts.Quiet = true
		default:
			return fmt.Errorf("usage: bin/clean [--local|--shared] [--quiet]")
		}
	}
	return Clean(root, opts)
}
