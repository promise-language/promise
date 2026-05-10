//go:build windows

package common

import "syscall"

var (
	kernel32WER          = syscall.NewLazyDLL("kernel32.dll")
	procSetErrorMode     = kernel32WER.NewProc("SetErrorMode")
	procSetConsoleCtrl   = kernel32WER.NewProc("SetConsoleCtrlHandler")
)

func toolCtrlHandler(ctrlType uint) uintptr {
	if ctrlType <= 1 { // CTRL_C_EVENT or CTRL_BREAK_EVENT
		return 1
	}
	return 0
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

	// Install a ctrl handler that ignores CTRL_C and CTRL_BREAK events.
	// Unlike NULL handler, a callback is NOT inherited by child processes.
	procSetConsoleCtrl.Call(syscall.NewCallback(toolCtrlHandler), 1)
}
