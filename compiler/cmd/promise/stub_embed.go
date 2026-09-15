package main

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// stubVersionSidecar is the file, written next to the installed stub, that
// records the installed stub's contract version. The forward-only update
// decision (§2.4 step 4) reads THIS file — it never executes the stub, since a
// stub forwards its args and an older stub predating PROMISE_STUB_VERSION would
// simply trampoline. A plain file read is the only way to honor "never
// downgrade" against a stub that may be older, broken, or missing.
const stubVersionSidecar = ".promise-stub-version"

// readInstalledStubVersion returns the version recorded in the sidecar next to
// the installed stub, or 0 when the sidecar is missing or unreadable (so a
// fresh install, or one that predates the sidecar, is always forward-updated).
// It never executes the stub.
func readInstalledStubVersion(stubBinDir string) int {
	data, err := os.ReadFile(filepath.Join(stubBinDir, stubVersionSidecar))
	if err != nil {
		return 0
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return v
}

// readEmbeddedStub returns the bytes of the embedded stub binary for this
// platform, or an error when no stub is embedded.
func readEmbeddedStub(binaryName string) ([]byte, error) {
	if !hasEmbeddedStub {
		return nil, fmt.Errorf("no embedded stub in this build")
	}
	return embeddedStub.ReadFile(stubEmbedPrefix + "/" + binaryName)
}

// writeFileAtomic writes data to path via a temp file in the same directory
// followed by a rename, so a reader never observes a half-written file (T0722).
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return renameWithRetry(tmpName, path)
}

// renameAttempts bounds the retry loop; renameBackoff yields the pause before
// attempt i+1. Both are package-level so tests can drive the loop deterministically
// (zero backoff) without sleeping through the ~0.45s production budget.
const renameAttempts = 10

func renameBackoff(i int) time.Duration { return time.Duration(i+1) * 10 * time.Millisecond }

// renameWithRetry renames src→dst, retrying briefly on transient errors.
// On Windows, MoveFileEx can fail with ERROR_ACCESS_DENIED / ERROR_SHARING_VIOLATION
// when an antivirus or the search indexer momentarily holds the file open; a short
// backoff lets the lock clear. On other platforms rename(2) is atomic and never
// retries (isRetryableRenameError always returns false there).
func renameWithRetry(src, dst string) error {
	return renameRetrying(os.Rename, isRetryableRenameError, renameBackoff, src, dst)
}

// renameRetrying is the testable core of renameWithRetry with its dependencies
// (the rename syscall, the retryable-error predicate, and the backoff schedule)
// injected, so the retry/exhaustion path — unreachable on non-Windows where
// isRetryableRenameError is always false — can be exercised on any platform.
func renameRetrying(rename func(src, dst string) error, retryable func(error) bool, backoff func(int) time.Duration, src, dst string) error {
	var err error
	for i := 0; i < renameAttempts; i++ {
		if err = rename(src, dst); err == nil {
			return nil
		}
		if !retryable(err) {
			return err
		}
		if i < renameAttempts-1 { // no point sleeping after the final attempt
			time.Sleep(backoff(i))
		}
	}
	return err
}

// symlinkNameAttempts bounds the search for an unused sibling name in
// ensureSymlink's replace path. A collision needs another process to pick the
// same pid+random name in the same directory, so one attempt effectively always
// wins; the bound exists only so a pathological directory cannot spin forever.
const symlinkNameAttempts = 8

// ensureSymlink makes path a symlink to target, idempotently and atomically:
// two processes materializing the same link concurrently both succeed, and a
// wrong or stale entry is replaced rather than silently kept (T2120). The
// Readlink fast path is the steady state; os.Symlink is the cold path; the
// replace below covers both a lost EEXIST race against a *different* target and
// a stale entry left by an older layout.
//
// Nothing here is check-then-act: symlink(2) and rename(2) are each atomic and
// each tells us which one of two racing processes won, so the answer is always
// read from the syscall rather than from a preceding Stat.
func ensureSymlink(target, path string) error {
	if got, err := os.Readlink(path); err == nil && got == target {
		return nil
	}
	err := os.Symlink(target, path)
	if err == nil {
		return nil
	}
	if !os.IsExist(err) {
		return err
	}
	// Lost the race. A concurrent process producing the identical link is the
	// expected outcome, not an error.
	if got, rerr := os.Readlink(path); rerr == nil && got == target {
		return nil
	}
	// Something else is at path — a link to the wrong target, or a leftover file.
	return replaceSymlinkRetrying(os.Symlink, target, path)
}

// replaceSymlinkRetrying is the testable core of ensureSymlink's replace path
// with the symlink syscall injected, so the name-collision retry and its
// exhaustion — unreachable in practice, since a collision needs another process
// to draw the same pid+random name in the same directory — can be exercised.
// (Factored for the same reason as renameRetrying above.)
//
// A symlink cannot be created over an existing entry, so the new link is built
// under an unused sibling name and renamed on: rename(2) over an existing entry
// is atomic, so a concurrent reader sees the old entry or the new link, never an
// absent one. symlink(2) is itself the create-if-absent primitive, so EEXIST on
// the sibling just means that name was taken — no Stat, no check-then-act.
func replaceSymlinkRetrying(symlink func(target, name string) error, target, path string) error {
	dir, base := filepath.Dir(path), filepath.Base(path)
	for attempt := 0; attempt < symlinkNameAttempts; attempt++ {
		tmpName := filepath.Join(dir, fmt.Sprintf(".tmp-%s.%d.%x", base, os.Getpid(), rand.Uint32()))
		if err := symlink(target, tmpName); err != nil {
			if os.IsExist(err) {
				continue // name taken — draw another
			}
			return err
		}
		if err := renameWithRetry(tmpName, path); err != nil {
			os.Remove(tmpName)
			return err
		}
		return nil
	}
	return fmt.Errorf("no unused temporary name available in %s after %d attempts", dir, symlinkNameAttempts)
}

// writeStubAndSidecar atomically installs the embedded stub binary and its
// version sidecar into stubBinDir (T0770 §2.4 step 4 / T0722). Both files are
// written via temp+rename so a concurrent reader never sees a partial stub or a
// version that does not match the binary on disk.
func writeStubAndSidecar(stubBinDir, binaryName string) error {
	data, err := readEmbeddedStub(binaryName)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(stubBinDir, binaryName), data, 0755); err != nil {
		return err
	}
	sidecar := []byte(strconv.Itoa(stubVersion) + "\n")
	return writeFileAtomic(filepath.Join(stubBinDir, stubVersionSidecar), sidecar, 0644)
}
