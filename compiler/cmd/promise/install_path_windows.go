//go:build windows

package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// addToUserPath adds dir to the current user's PATH (HKCU\Environment) on Windows,
// idempotently and preserving the value's registry type (REG_EXPAND_SZ by default
// so %VAR% entries still expand). It returns whether a change was made.
//
// We deliberately do NOT shell out to `setx`: setx truncates values at 1024 chars,
// and the common `setx PATH "%PATH%;..."` idiom copies the merged System+User PATH
// into the User scope (unbounded growth) or, in PowerShell, writes the literal
// text "%PATH%" (T0863). A direct registry write avoids both.
//
// After a successful change we broadcast WM_SETTINGCHANGE — the same thing .NET's
// Environment.SetEnvironmentVariable(..., User) does — so Explorer and the shells
// it later spawns refresh PATH without a logoff. It is best-effort: a brand-new
// terminal reads the registry regardless, and snapshot-env hosts like VS Code
// ignore the broadcast (they must be fully restarted).
func addToUserPath(dir string) (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false, fmt.Errorf("open HKCU\\Environment: %w", err)
	}
	defer k.Close()

	existing, valType, err := k.GetStringValue("Path")
	if err != nil && err != registry.ErrNotExist {
		return false, fmt.Errorf("read User Path: %w", err)
	}

	updated, changed := computeUpdatedPath(existing, dir)
	if !changed {
		return false, nil
	}

	// Preserve REG_SZ if that is how Path was stored; otherwise write REG_EXPAND_SZ
	// (the Windows default for Path, and what a missing value should become).
	if valType == registry.SZ {
		if err := k.SetStringValue("Path", updated); err != nil {
			return false, fmt.Errorf("write User Path: %w", err)
		}
	} else {
		if err := k.SetExpandStringValue("Path", updated); err != nil {
			return false, fmt.Errorf("write User Path: %w", err)
		}
	}

	broadcastEnvChange()
	return true, nil
}

// broadcastEnvChange sends WM_SETTINGCHANGE("Environment") to all top-level
// windows so running processes that listen (notably Explorer) refresh their
// environment. Best-effort — failures are ignored.
func broadcastEnvChange() {
	const (
		hwndBroadcast    = 0xffff
		wmSettingChange  = 0x001A
		smtoAbortIfHung  = 0x0002
		broadcastTimeout = 5000 // ms
	)
	env, err := windows.UTF16PtrFromString("Environment")
	if err != nil {
		return
	}
	proc := windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")
	var result uintptr
	// SendMessageTimeoutW(HWND_BROADCAST, WM_SETTINGCHANGE, 0, "Environment",
	//                     SMTO_ABORTIFHUNG, timeout, &result)
	_, _, _ = proc.Call(
		uintptr(hwndBroadcast),
		uintptr(wmSettingChange),
		0,
		uintptr(unsafe.Pointer(env)),
		uintptr(smtoAbortIfHung),
		uintptr(broadcastTimeout),
		uintptr(unsafe.Pointer(&result)),
	)
}
