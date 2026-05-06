//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// shimExec replaces the current process with the target binary.
// On Unix, this uses syscall.Exec which never returns on success.
func shimExec(binary string, args []string, env []string) {
	// Replace argv[0] with the target binary path.
	execArgs := make([]string, len(args))
	copy(execArgs, args)
	execArgs[0] = binary

	err := syscall.Exec(binary, execArgs, env)
	// If we get here, exec failed.
	fmt.Fprintf(os.Stderr, "error: cannot exec epoch binary %s: %v\n", binary, err)
	os.Exit(1)
}
