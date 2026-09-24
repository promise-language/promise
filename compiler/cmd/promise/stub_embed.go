package main

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// stubVersionSidecar is the file, written next to the installed stub, that
// records the installed stub's contract version. The forward-only update
// decision (distribution.md#what-install-does step 4) reads THIS file — it never executes the stub, since a
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

// copyFileAtomic copies src to dst via a temp file in dst's directory followed
// by a rename, exactly as writeFileAtomic does — but streamed, so the source
// never lands in the Go heap.
//
// The toolchain blobs this moves are ~125-200 MB apiece, and reading one whole
// is a ~200 MB allocation per tool, three tools per view, on the path a cold
// PROMISE_HOME takes before it can compile anything (T2133). io.Copy's fixed
// buffer costs the same wall time with none of the heap.
func copyFileAtomic(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(dst)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	// No settled check (T2132): proving dst already holds src's bytes means reading
	// a toolchain blob back in full, the cost this function exists to avoid.
	return renameWithRetry(tmpName, dst, nil)
}

// writeFileAtomic writes data to path via a temp file in the same directory
// followed by a rename, so a reader never observes a half-written file (T0722).
//
// Concurrent writers of identical bytes both succeed (T2132). On Windows the
// replace step of a rename fails with ERROR_ACCESS_DENIED while any other handle
// to the destination lives — a peer writer's replace in flight, or an antivirus
// or the indexer scanning the file that just landed — so the loser of that race
// asks whether path already holds exactly the bytes it was told to write and
// reports success when it does: its whole postcondition is met and there is
// nothing left for it to do. A destination that is missing or different is still
// an error, so a wrong file is still repaired.
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
	return renameWithRetry(tmpName, path, func() bool { return fileHasBytes(path, data) })
}

// fileHasBytes reports whether path already holds exactly want. The size check
// comes first because it is handle-free (GetFileAttributesEx on Windows), so a
// destination that is plainly wrong costs no open handle — which matters here,
// since a handle on the destination is the very thing that makes a concurrent
// MoveFileEx fail. Size alone is not the answer: a same-size file with different
// bytes is still wrong.
func fileHasBytes(path string, want []byte) bool {
	info, err := os.Stat(path)
	if err != nil || info.Size() != int64(len(want)) {
		return false
	}
	got, err := os.ReadFile(path)
	return err == nil && bytes.Equal(got, want)
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
//
// settled reports whether dst already holds what this rename was for, so a race
// lost to someone who produced exactly that outcome is success rather than a
// failure (T2132); nil when the caller has no way to tell, and then the rename
// error is the whole answer. Answering that way means the rename did *not*
// happen, so src is still on disk and removing it stays the caller's job — as it
// already is on the error path.
func renameWithRetry(src, dst string, settled func() bool) error {
	return renameRetrying(os.Rename, isRetryableRenameError, renameBackoff, settled, src, dst)
}

// renameRetrying is the testable core of renameWithRetry with its dependencies
// (the rename syscall, the retryable-error predicate, the backoff schedule, and
// the settled check) injected, so the retry/exhaustion path — unreachable on
// non-Windows where isRetryableRenameError is always false — can be exercised on
// any platform.
//
// settled is consulted only after a *retryable* failure: a non-retryable error is
// a real one, and a destination that happens to be right must never mask it.
func renameRetrying(rename func(src, dst string) error, retryable func(error) bool, backoff func(int) time.Duration, settled func() bool, src, dst string) error {
	var err error
	for i := 0; i < renameAttempts; i++ {
		if err = rename(src, dst); err == nil {
			return nil
		}
		if !retryable(err) {
			return err
		}
		if settled != nil && settled() {
			return nil // someone else already produced exactly this outcome
		}
		if i < renameAttempts-1 { // no point sleeping after the final attempt
			time.Sleep(backoff(i))
		}
	}
	return err
}

// linkPointsAt reports whether path is a symlink to target — the one question
// ensureSymlink asks of an entry it did not create itself.
func linkPointsAt(path, target string) bool {
	got, err := os.Readlink(path)
	return err == nil && got == target
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
	if linkPointsAt(path, target) {
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
	if linkPointsAt(path, target) {
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
//
// If that rename loses a Windows sharing race to a process that produced the same
// link, the outcome is already there and the loser reports success (T2132) — the
// same answer ensureSymlink gives a lost EEXIST race above.
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
		err := renameWithRetry(tmpName, path, func() bool { return linkPointsAt(path, target) })
		// The temp link is gone once the rename moved it, and still there when the
		// rename failed — or when another process got there first with the same link
		// (T2132), which answers the rename without consuming the name. Removing it
		// either way is what leaves no debris behind.
		os.Remove(tmpName)
		return err
	}
	return fmt.Errorf("no unused temporary name available in %s after %d attempts", dir, symlinkNameAttempts)
}

// writeStubAndSidecar atomically installs the embedded stub binary and its
// version sidecar into stubBinDir (T0770 distribution.md#what-install-does step 4 / T0722). Both files are
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
