// Tests lifted back out of tests/string (T1776): they reach into codegen's
// unexported internals, so they have to be compiled as part of package
// codegen. They use the private helper copies in codegen_helpers_test.go.
package codegen

import (
	"testing"
)

// TestFormatDurationNs verifies the compile-time duration formatter used for
// TIMEOUT context messages (T1199).
func TestFormatDurationNs(t *testing.T) {
	cases := []struct {
		ns   int64
		want string
	}{
		{0, "0ns"},
		{1, "1ns"},
		{999, "999ns"},
		{-1, "-1ns"},
		{-1_000_000_000, "-1s"},
		{1_000, "1us"},
		{1_500, "1.5us"},
		{999_999, "999.9us"},
		{1_000_000, "1ms"},
		{1_500_000, "1.5ms"},
		{999_999_999, "999.9ms"},
		{1_000_000_000, "1s"},
		{2_000_000_000, "2s"},
		{1_500_000_000, "1.5s"},
		{500_000_000, "500ms"},
		{10_000_000, "10ms"},
		{100_000, "100us"},
	}
	for _, tc := range cases {
		got := formatDurationNs(tc.ns)
		if got != tc.want {
			t.Errorf("formatDurationNs(%d) = %q, want %q", tc.ns, got, tc.want)
		}
	}
}
