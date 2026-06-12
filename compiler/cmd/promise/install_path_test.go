package main

import "testing"

// TestComputeUpdatedPath covers the pure core of the Windows User-PATH update
// (T0863): idempotent, case-insensitive, trailing-separator-insensitive, and
// correct on an empty starting value.
func TestComputeUpdatedPath(t *testing.T) {
	const dir = `C:\Users\Me\.promise\bin`
	cases := []struct {
		name     string
		existing string
		want     string
		changed  bool
	}{
		{"empty", "", dir, true},
		{"append", `C:\Windows`, `C:\Windows;` + dir, true},
		{"already present exact", `C:\Windows;` + dir, `C:\Windows;` + dir, false},
		{"already present trailing slash", `C:\Windows;` + dir + `\`, `C:\Windows;` + dir + `\`, false},
		{"already present different case", `C:\Windows;c:\users\me\.promise\BIN`, `C:\Windows;c:\users\me\.promise\BIN`, false},
		{"ignores empty segments", `;;C:\Windows;`, `;;C:\Windows;;` + dir, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changed := computeUpdatedPath(c.existing, dir)
			if got != c.want || changed != c.changed {
				t.Fatalf("computeUpdatedPath(%q, %q) = (%q, %v); want (%q, %v)",
					c.existing, dir, got, changed, c.want, c.changed)
			}
		})
	}
}
