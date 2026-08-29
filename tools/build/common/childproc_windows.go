//go:build windows

package common

import (
	"os/exec"
	"strconv"
)

// isolateChild is a no-op on Windows. Process groups there are a console
// notion, not a lifetime one: CREATE_NEW_PROCESS_GROUP would detach the child
// from Ctrl+C without giving us any way to kill its descendants, which is the
// opposite of the trade we want. killTree walks the tree instead.
//
// The durable Windows answer is a Job Object with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, which ties the tree's lifetime to ours in
// the kernel and so also covers the case where this tool is killed outright.
// That is still open work — see T1450.
func isolateChild(cmd *exec.Cmd) {}

// killTree terminates pid and every process descended from it. taskkill /T
// walks the tree itself and ships with every supported Windows version.
func killTree(pid int) {
	cmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	_ = cmd.Run()
}
