//go:build !windows

package module

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	// Try non-blocking first to detect contention.
	err = syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB)
	if err != nil {
		fmt.Fprint(os.Stderr, waitMsg)
		if err := syscall.Flock(int(f.Fd()), mode); err != nil {
			f.Close()
			return func() {}
		}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}
