//go:build windows

package main

import (
	"os/exec"
	"time"
)

// setupProcessGroupKill configures a command to kill its process on context
// cancellation. On Windows, Process.Kill terminates the process; child process
// cleanup relies on the OS job object behavior.
func setupProcessGroupKill(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 3 * time.Second
}
