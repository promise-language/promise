//go:build windows

package blobstore

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockByteOffsetHigh places the single advisory lock byte at offset 4 GiB
// (OffsetHigh = 1). LockFileEx byte-range locks are mandatory on Windows: any
// read/write to a locked range from a *different* handle fails with
// ERROR_LOCK_VIOLATION. Store.Lock writes a short holder hint at offset 0 from a
// separate handle, so the lock byte must live well beyond any real content to
// avoid colliding with it. Unix flock is whole-file advisory and ignores this.
const lockByteOffsetHigh = 1

func lockOverlapped() *windows.Overlapped {
	return &windows.Overlapped{OffsetHigh: lockByteOffsetHigh}
}

func flockTry(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, lockOverlapped())
	if err == nil {
		return true, nil
	}
	if err == windows.ERROR_LOCK_VIOLATION || err == windows.ERROR_IO_PENDING {
		return false, nil
	}
	return false, err
}

func flockBlock(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, lockOverlapped())
}

func flockUnlock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, lockOverlapped())
}
