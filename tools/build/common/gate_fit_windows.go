//go:build windows

package common

import (
	"syscall"
	"unsafe"
)

var getDiskFreeSpaceExW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// freeBytes reports the space available to the calling user on the volume
// holding path (quota-aware, like Bavail on POSIX).
func freeBytes(path string) (int64, error) {
	dir, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	if err := getDiskFreeSpaceExW.Find(); err != nil {
		return 0, err
	}
	var availableToCaller, totalBytes, totalFree uint64
	r, _, errno := syscall.SyscallN(getDiskFreeSpaceExW.Addr(),
		uintptr(unsafe.Pointer(dir)),
		uintptr(unsafe.Pointer(&availableToCaller)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 {
		return 0, errno
	}
	return int64(availableToCaller), nil
}
