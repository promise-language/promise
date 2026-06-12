package main

import (
	"fmt"
	"strings"
)

// computeUpdatedPath returns the PATH value with dir appended, plus whether a
// change was needed. Membership is case-insensitive and ignores a trailing
// separator — Windows path semantics — so an already-present dir is a no-op
// rather than a duplicate. An empty existing value yields just dir.
//
// This is the pure core of the Windows User-PATH update (see addToUserPath); it
// is kept platform-agnostic so it can be unit-tested on every OS.
func computeUpdatedPath(existing, dir string) (string, bool) {
	norm := func(s string) string { return strings.ToLower(strings.TrimRight(s, `\`)) }
	target := norm(dir)
	for _, p := range strings.Split(existing, ";") {
		if p != "" && norm(p) == target {
			return existing, false
		}
	}
	if existing == "" {
		return dir, true
	}
	return existing + ";" + dir, true
}

// printWindowsPathHint prints the correct, non-destructive PowerShell command to
// add dir to the User PATH. Used when the user opts out of automatic PATH setup
// or when the registry update fails. We never emit `setx PATH "%PATH%;..."`: in
// PowerShell `%PATH%` is a literal (it overwrites PATH with that text), and in
// cmd.exe `%PATH%` is the merged System+User PATH while setx writes User scope and
// truncates at 1024 chars — both corrupt the PATH (T0863).
func printWindowsPathHint(dir string) {
	fmt.Printf("\nTo add it to your PATH yourself (PowerShell):\n\n")
	fmt.Printf("  [Environment]::SetEnvironmentVariable(\"Path\", [Environment]::GetEnvironmentVariable(\"Path\",\"User\") + \";%s\", \"User\")\n", dir)
	fmt.Printf("\nThen open a new terminal (or fully quit and reopen VS Code).\n")
}
