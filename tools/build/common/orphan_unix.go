//go:build !windows

package common

import "os"

// parentAlive reports whether the launcher is still running.
//
// On Unix the kernel answers this for free: when a parent dies its children are
// reparented (to init, or to a subreaper), so a getppid(2) that no longer
// matches what we saw at startup IS the death notification. No handle to keep,
// and no pid to re-check after it may have been recycled.
func parentAlive(startPPID int) bool {
	return os.Getppid() == startPPID
}
