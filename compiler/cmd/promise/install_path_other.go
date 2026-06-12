//go:build !windows

package main

import "errors"

// addToUserPath is Windows-only. On other platforms PATH setup is left to the
// user's shell profile (the installer prints `export PATH=...`). This stub keeps
// the cross-platform install code compiling; it is never called because runInstall
// guards the call on runtime.GOOS == "windows".
func addToUserPath(string) (bool, error) {
	return false, errors.New("addToUserPath is only supported on Windows")
}
