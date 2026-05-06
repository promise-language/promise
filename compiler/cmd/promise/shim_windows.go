//go:build windows

package main

import (
	"fmt"
	"os"
)

// shimExec launches the target binary as a child process and waits for it.
// On Windows, syscall.Exec is not available, so we use os.StartProcess.
func shimExec(binary string, args []string, env []string) {
	// Build argv: args[0] is replaced with the binary path.
	argv := make([]string, len(args))
	copy(argv, args)
	argv[0] = binary

	proc, err := os.StartProcess(binary, argv, &os.ProcAttr{
		Env:   env,
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot start epoch binary %s: %v\n", binary, err)
		os.Exit(1)
	}

	state, err := proc.Wait()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: waiting for epoch binary: %v\n", err)
		os.Exit(1)
	}

	os.Exit(state.ExitCode())
}
