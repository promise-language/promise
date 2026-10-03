package common

import (
	"fmt"
	"os"
	"path/filepath"
)

// CleanOptions configures what Clean removes.
type CleanOptions struct {
	// Quiet suppresses informational progress lines.
	Quiet bool
}

// CleanTarget returns the directory Clean removes: <root>/.promise-home, the
// worktree's own Promise home. It is the only home a worktree command uses, so
// it is the only one Clean addresses — the machine-global ~/.promise belongs to
// the installed CLI, and nothing run from a worktree touches it (#102).
func CleanTarget(root string) string {
	return filepath.Join(root, ".promise-home")
}

// Clean puts build state back to pristine: it removes the repo-local
// .promise-home/ — tmp/, cache/ and anything else under it. The next build
// re-fetches and re-extracts what it needs.
//
// It never runs `go clean -testcache`: that stamps the host-global
// $GOCACHE/testexpire.txt and expires every saved Go test result in every clone.
// A run that must not reuse saved results asks its own `go test` for that
// instead (goTestFlags).
//
// Clean takes the host-scope exclusion (acquireVerifyLock) before removing
// anything, so it never clears a home a verify is measuring in. Callers that
// already hold it (e.g. RunVerify with --clean) must use cleanLocked instead: a
// second acquisition re-enters only when the checkout has an arena record, and
// in a checkout with none it queues behind its own holder forever.
func Clean(root string, opts CleanOptions) error {
	unlock, err := acquireVerifyLock(root, 0)
	if err != nil {
		return fmt.Errorf("acquire verify lock: %w", err)
	}
	defer unlock()
	return cleanLocked(root, opts)
}

// cleanLocked performs the clean without taking the host-scope exclusion.
// Must only be called by callers that already hold it.
func cleanLocked(root string, opts CleanOptions) error {
	target := CleanTarget(root)

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
// can be a pure unit test rather than a real clean, which removes a home
// (T2084).
func parseCleanArgs(args []string) (CleanOptions, error) {
	args = NormalizeArgs(args)
	var opts CleanOptions
	for _, arg := range args {
		switch arg {
		case "-local":
			// explicit local — no-op: .promise-home/ is the only target
		case "-quiet":
			opts.Quiet = true
		default:
			return CleanOptions{}, fmt.Errorf("usage: bin/clean [--local] [--quiet]")
		}
	}
	return opts, nil
}

// RunClean is the bin/clean CLI entry. It clears the repo-local .promise-home/.
func RunClean(root string, args []string) error {
	opts, err := parseCleanArgs(args)
	if err != nil {
		return err
	}
	return Clean(root, opts)
}
