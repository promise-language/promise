//go:build unix

package common

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// unlinkMetrics reports whether the filesystem holding the worktree keeps a
// removed file's name while the file is still open: 1 if it does, 0 if the name
// goes with the unlink.
//
// Every temp root this project's suites use lives on that filesystem —
// .promise-home/tmp (TMPDIR, pinned by SetupLocalCache) and, in an arena, the go
// tool's GOTMPDIR. On every platform but Windows the suites assume a removed
// name is gone at once: clitest.TempDir fails the test on a cleanup error there,
// and neither Go's t.TempDir nor the go tool retries. A filesystem served
// through libfuse's high-level API, ntfs-3g among them, instead renames a file
// unlinked while open to .fuse_hidden… until its last close, and NFS to .nfs….
// That close arrives a moment after the process holding the file exits, so a
// temp dir a passing test just emptied cannot be removed and the run reports a
// failure no change caused (#724). That is a machine this project's work cannot
// be given, not a change that may not land.
func unlinkMetrics(root string) ([]Metric, error) {
	return unlinkMetricsWith(root, os.Remove)
}

// unlinkMetricsWith is unlinkMetrics with the probe's directory removal
// injected, so a filesystem that keeps the name can be shown to reach the
// envelope as a 1 without mounting one.
func unlinkMetricsWith(root string, removeDir func(string) error) ([]Metric, error) {
	hides, err := hidesUnlinkedFiles(scratchDir(root), removeDir)
	if err != nil {
		return nil, err
	}
	n := 0
	if hides {
		n = 1
	}
	return []Metric{Count("worktree_hides_unlinked_files", n)}, nil
}

// hidesUnlinkedFiles is the probe. It creates a file in a fresh directory under
// scratch, unlinks it while holding it open, and then removes the directory with
// the file still open. A filesystem that drops the name at unlink removes the
// directory; one that keeps a name answers that the directory is not empty.
// Holding the file open is what makes the answer deterministic: the suites meet
// the same mechanism only in the window before a just-exited process's close
// arrives, which no probe could reproduce reliably.
//
// removeDir is the directory removal, injected so the branches a native
// filesystem never takes can be exercised without mounting another one — the
// same reason clitest's removeAllRetryingWith is factored.
func hidesUnlinkedFiles(scratch string, removeDir func(string) error) (bool, error) {
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return false, err
	}
	dir, err := os.MkdirTemp(scratch, "fit-unlink-*")
	if err != nil {
		return false, err
	}
	removed := false
	defer func() {
		// Once, best-effort, and only where the probe did not remove the
		// directory itself. On a filesystem that keeps names the hidden one can
		// outlive this too, leaving an empty directory in gitignored scratch — on
		// the machine this measurement reports unfit for exactly that.
		if !removed {
			os.RemoveAll(dir)
		}
	}()
	f, err := os.Create(filepath.Join(dir, "probe"))
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := os.Remove(f.Name()); err != nil {
		return false, err
	}
	switch err := removeDir(dir); {
	case err == nil:
		removed = true
		return false, nil
	// POSIX lets rmdir answer either for a directory that is not empty.
	case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST):
		return true, nil
	default:
		return false, fmt.Errorf("removing %s with a file unlinked in it still open: %w", dir, err)
	}
}
