//go:build windows

package common

import (
	"sync/atomic"
	"syscall"
)

var (
	kernel32WER        = syscall.NewLazyDLL("kernel32.dll")
	procSetErrorMode   = kernel32WER.NewProc("SetErrorMode")
	procSetConsoleCtrl = kernel32WER.NewProc("SetConsoleCtrlHandler")
)

// interrupted is set to 1 by the ctrl handler when the user presses Ctrl+C
// or Ctrl+Break. Checked by Interrupted() between pipeline steps.
var interrupted atomic.Int32

func toolCtrlHandler(ctrlType uint) uintptr {
	if ctrlType <= 1 { // CTRL_C_EVENT or CTRL_BREAK_EVENT
		interrupted.Store(1)
		return 1
	}
	return 0
}

// Interrupted returns true if the user has pressed Ctrl+C or Ctrl+Break.
func Interrupted() bool {
	return interrupted.Load() != 0
}

func init() {
	// Suppress Windows Error Reporting crash dialogs for this process and all
	// children. Without this, a crashing child process (e.g., opt.exe, a test
	// binary) pops a WER dialog that blocks the parent — especially visible in
	// PowerShell where the entire pipeline stalls.
	const (
		semFailCriticalErrors = 0x0001
		semNoGPFaultErrorBox  = 0x0002
	)
	procSetErrorMode.Call(uintptr(semFailCriticalErrors | semNoGPFaultErrorBox))

	// Install a ctrl handler that intercepts CTRL_C and CTRL_BREAK events.
	// Sets the interrupted flag so the pipeline exits between steps, while
	// preventing the default handler from killing the process immediately.
	// Unlike NULL handler, a callback is NOT inherited by child processes.
	procSetConsoleCtrl.Call(syscall.NewCallback(toolCtrlHandler), 1)
}
