package module

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx = modkernel32.NewProc("UnlockFileEx")
)

const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
)

func lockBuildDir(exclusive bool, waitMsg string) func() {
	dir, err := BuildCacheDir()
	if err != nil {
		return func() {}
	}
	lockPath := filepath.Join(dir, ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return func() {}
	}
	handle := syscall.Handle(f.Fd())
	flags := uint32(lockfileFailImmediately)
	if exclusive {
		flags |= lockfileExclusiveLock
	}
	ol := new(syscall.Overlapped)
	// Try non-blocking first.
	r1, _, _ := procLockFileEx.Call(
		uintptr(handle), uintptr(flags), 0, 1, 0, uintptr(unsafe.Pointer(ol)),
	)
	if r1 == 0 {
		// Blocking — contention detected.
		fmt.Fprint(os.Stderr, waitMsg)
		blockFlags := uint32(0)
		if exclusive {
			blockFlags = lockfileExclusiveLock
		}
		ol2 := new(syscall.Overlapped)
		r1, _, _ = procLockFileEx.Call(
			uintptr(handle), uintptr(blockFlags), 0, 1, 0, uintptr(unsafe.Pointer(ol2)),
		)
		if r1 == 0 {
			f.Close()
			return func() {}
		}
	}
	return func() {
		ol3 := new(syscall.Overlapped)
		procUnlockFileEx.Call(uintptr(handle), 0, 1, 0, uintptr(unsafe.Pointer(ol3)))
		f.Close()
	}
}
