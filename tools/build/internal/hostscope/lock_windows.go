package hostscope

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// The Windows lock is LockFileEx on a byte of the file, which the kernel
// releases when the handle closes — and it closes every handle of a process it
// ends, however the process ended. That is the property lock_flock.go's comment
// names as the reason for a kernel lock, and the reason this is not a lockfile
// with a timestamp here either.
//
// THE LOCKED BYTE IS ONE THE HOLDER RECORD NEVER OCCUPIES. A Windows byte-range
// lock is mandatory, not advisory: while it is held, every other handle —
// another process's, or another of this process's — is refused a read of the
// bytes it covers. The record lives at offset 0 of this same file, and it is
// read while the lock is held by exactly the parties that need it: the refused
// caller deciding re-entry (hostscope.go § acquireAt), Holder and Held. Locking
// byte 0 made every one of those reads fail, so a nested party in the holding
// arena read nobody and queued behind its own parent. lockByte is far past
// anything maxHolderRecord would let a reader take, and LockFileEx allows a
// range beyond the end of the file, so the lock and the record never meet.
//
// Reached through kernel32 by name rather than through a module: `syscall`
// does not export the call, and one symbol is not worth a dependency every
// machine that builds this would pay for.
var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2
	errorLockViolation      = syscall.Errno(33)
	errorIOPending          = syscall.Errno(997)
)

// The locked range: lockLength bytes at lockByte. Both halves are named once,
// because UnlockFileEx must name exactly the range LockFileEx took — an unlock
// that names another range fails, and the lock stays held until the handle
// closes.
const (
	lockByte   uint64 = 1 << 62
	lockLength        = 1
)

// lockRegion is the OVERLAPPED that places a lock or an unlock at lockByte.
func lockRegion() *syscall.Overlapped {
	return &syscall.Overlapped{Offset: uint32(lockByte & 0xFFFFFFFF), OffsetHigh: uint32(lockByte >> 32)}
}

func lockFileEx(f *os.File, flags uint32) error {
	r, _, err := procLockFileEx.Call(f.Fd(), uintptr(flags), 0, lockLength, 0, uintptr(unsafe.Pointer(lockRegion())))
	if r != 0 {
		return nil
	}
	return err
}

func tryLockExclusive(f *os.File) (granted bool, err error) {
	switch err := lockFileEx(f, lockfileExclusiveLock|lockfileFailImmediately); {
	case err == nil:
		return true, nil
	case errors.Is(err, errorLockViolation), errors.Is(err, errorIOPending):
		return false, nil
	default:
		return false, fmt.Errorf("hostscope: cannot take the host-scope exclusion on %s: %w", f.Name(), err)
	}
}

// waitLockExclusive has lock_flock.go's shape and ownership rule for the same
// reason: the blocking call has no deadline, so a caller that gives up leaves
// the grant to the cleanup goroutine, which alone can know when it lands.
func waitLockExclusive(ctx context.Context, f *os.File) error {
	granted := make(chan error, 1)
	go func() { granted <- lockFileEx(f, lockfileExclusiveLock) }()
	select {
	case err := <-granted:
		if err != nil {
			f.Close()
			return fmt.Errorf("hostscope: cannot take the host-scope exclusion on %s: %w", f.Name(), err)
		}
		return nil
	case <-ctx.Done():
		go func() {
			if err := <-granted; err == nil {
				_ = unlock(f)
			}
			f.Close()
		}()
		return fmt.Errorf("hostscope: gave up waiting for the host-scope exclusion on %s: %w", f.Name(), ctx.Err())
	}
}

func unlock(f *os.File) error {
	r, _, err := procUnlockFileEx.Call(f.Fd(), 0, lockLength, 0, uintptr(unsafe.Pointer(lockRegion())))
	if r != 0 {
		return nil
	}
	return err
}
