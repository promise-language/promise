package common

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// CleanOptions configures what Clean removes.
type CleanOptions struct {
	// Shared targets the shared ~/.promise/cache instead of the repo-local
	// .promise-home/. Off by default — Clean leaves the shared home alone unless
	// the operator asks, and even then removes only its cache.
	Shared bool
	// Quiet suppresses informational progress lines.
	Quiet bool
}

// errCleanWithShared refuses --clean combined with --shared on bin/test and
// bin/verify. A test run never clears the shared home — clearing
// ~/.promise/cache is an operator's explicit `bin/clean --shared` — and --clean
// clears only the repo-local .promise-home, which a --shared run does not use.
var errCleanWithShared = errors.New("--clean cannot be combined with --shared: bin/test and bin/verify never clear the shared ~/.promise (run bin/clean --shared for that), and --clean clears only the repo-local .promise-home, which a --shared run does not use")

// CleanTarget returns the directory Clean removes: <root>/.promise-home by
// default, ~/.promise/cache with shared.
//
// The shared target is the cache subtree, never ~/.promise itself. The shared
// home is also the install root — epochs/, bin/ and active are the installed
// toolchain (T1925) — and it holds the verify lock Clean takes, which Windows
// will not let the holder delete while it has it open (T2095).
func CleanTarget(root string, shared bool) (string, error) {
	if shared {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(h, ".promise", "cache"), nil
	}
	return filepath.Join(root, ".promise-home"), nil
}

// Clean puts build state back to pristine. By default it removes the repo-local
// .promise-home/ — tmp/, cache/ and anything else under it. With opts.Shared it
// removes ~/.promise/cache instead, leaving the installed toolchain and the
// verify lock in place; the next build re-fetches and re-extracts what it needs.
//
// It never runs `go clean -testcache`: that stamps the host-global
// $GOCACHE/testexpire.txt and expires every saved Go test result in every clone.
// A run that must not reuse saved results asks its own `go test` for that
// instead (goTestFlags).
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
	target, err := CleanTarget(root, opts.Shared)
	if err != nil {
		return fmt.Errorf("resolve promise home: %w", err)
	}

	log := func(format string, args ...any) {
		if !opts.Quiet {
			fmt.Printf(format+"\n", args...)
		}
	}

	if !Exists(target) {
		log("Nothing to clear: %s does not exist", target)
		return nil
	}
	log("Clearing %s", target)
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("remove %s: %w", target, err)
	}
	return nil
}

// parseCleanArgs parses bin/clean's flags and does nothing else — no lock, no
// filesystem, no subprocess. It is separate from RunClean so that flag coverage
// can be a pure unit test rather than a real clean: `--shared` resolves to the
// host's ~/.promise/cache and removes it (T2084).
func parseCleanArgs(args []string) (CleanOptions, error) {
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
			return CleanOptions{}, fmt.Errorf("usage: bin/clean [--local|--shared] [--quiet]")
		}
	}
	return opts, nil
}

// RunClean is the bin/clean CLI entry. Defaults to the repo-local
// .promise-home/. Pass --shared to clear ~/.promise/cache instead.
func RunClean(root string, args []string) error {
	opts, err := parseCleanArgs(args)
	if err != nil {
		return err
	}
	return Clean(root, opts)
}
