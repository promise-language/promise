//go:build windows

package common

import "syscall"

const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
)

// parentAlive reports whether the launcher is still running.
//
// Windows does not reparent orphans, so getppid keeps naming a dead process and
// cannot answer this. Ask the kernel instead: open the parent and read its exit
// code. A pid can in principle be recycled between startup and the check, which
// would make a dead parent look alive — but the only cost of that is this safety
// net missing, so a cheap check is the right trade. The airtight Windows fix is
// a Job Object (T1450), which needs no polling at all.
func parentAlive(startPPID int) bool {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(startPPID))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)

	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
