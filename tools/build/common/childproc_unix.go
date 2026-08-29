//go:build !windows

package common

import (
	"os/exec"
	"syscall"
)

// isolateChild puts the command in its own process group, so that killTree can
// take down the command AND everything it spawned with a single signal.
//
// It also means the child no longer receives the terminal's Ctrl+C directly —
// that is why the interrupt handler forwards it explicitly (interrupt_other.go).
// Forwarding is what we want regardless: it is the same signal either way, but
// now it happens on a path we control, so an interrupt reaches the tree whether
// it came from a terminal or from anywhere else.
func isolateChild(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killTree SIGKILLs the process group led by pid — the command itself and every
// descendant that has not started a group of its own.
func killTree(pid int) {
	// Negative pid addresses the group. The child is its own group leader
	// (isolateChild), so the group id equals its pid.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// signalTree sends sig to the process group led by pid.
func signalTree(pid int, sig syscall.Signal) {
	_ = syscall.Kill(-pid, sig)
}
