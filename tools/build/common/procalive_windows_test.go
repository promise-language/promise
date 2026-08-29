//go:build windows

package common

// processAlive reports whether pid still exists. parentAlive already answers
// exactly this question on Windows, so reuse it rather than opening a second
// handle path that could drift from it.
func processAlive(pid int) bool {
	return parentAlive(pid)
}
