//go:build !windows

package main

import (
	"os/exec"
	"syscall"
	"time"
)

// sanitizeExitCode passes through exit codes unchanged on Unix.
func sanitizeExitCode(code int) int { return code }

// isolateProcessGroup is a no-op on Unix (only needed on Windows to prevent
// CTRL_C propagation from crashing children).
func isolateProcessGroup(cmd *exec.Cmd) {}

// detachFromConsole is a no-op on Unix.
func detachFromConsole(cmd *exec.Cmd) {}

// setupProcessGroupKill configures a command to kill its entire process tree
// on context cancellation. Without this, child processes spawned by the command
// survive after the parent is killed (e.g., test binaries outlive `promise test`).
func setupProcessGroupKill(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 3 * time.Second
}
