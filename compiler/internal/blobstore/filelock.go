package blobstore

import "os"

// fileLock is a minimal advisory file lock with the property that matters for a
// shared cache: the OS releases it on process death, so a crashed holder can
// never leave a permanently stale lock. Implemented with stdlib syscall.Flock
// on Unix and LockFileEx on Windows (per-OS files). This avoids an external
// dependency (gofrs/flock) in the shipped compiler binary while giving the same
// guarantee as prebuilts.acquireCacheLock.
type fileLock struct {
	path string
	f    *os.File
}

func newFileLock(path string) *fileLock { return &fileLock{path: path} }

// open lazily opens the lock file (created if missing).
func (l *fileLock) open() error {
	if l.f != nil {
		return nil
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	l.f = f
	return nil
}

// tryLock attempts a non-blocking exclusive lock. Returns (true, nil) on
// success, (false, nil) on contention.
func (l *fileLock) tryLock() (bool, error) {
	if err := l.open(); err != nil {
		return false, err
	}
	return flockTry(l.f)
}

// lock blocks until the exclusive lock is acquired.
func (l *fileLock) lock() error {
	if err := l.open(); err != nil {
		return err
	}
	return flockBlock(l.f)
}

// unlock releases the lock and closes the file.
func (l *fileLock) unlock() error {
	if l.f == nil {
		return nil
	}
	err := flockUnlock(l.f)
	l.f.Close()
	l.f = nil
	return err
}
