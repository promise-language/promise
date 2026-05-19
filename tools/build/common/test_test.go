package common

import "testing"

// TestIsPromisePassLine verifies that pass-result lines are detected and that
// failure/summary/context lines are not. Used by gate runners (T0323) to
// suppress passing-test lines from the user-facing console while still
// recording them in the JSON output.
func TestIsPromisePassLine(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"pass (0.001s) e2e/hello.pr", true},
		{"pass (0.004s) e2e/basics.pr (3 tests)", true},
		{"pass (0.001s) test_add", true},
		{"pass (12.345s) tests/concurrency/go_nested.pr", true},

		{"FAIL (0.005s) e2e/strings.pr (1/3 failed)", false},
		{"LEAK (0.001s) test_leaky", false},
		{"TIMEOUT (0.100s) test_stuck", false},
		{"  test_split", false},
		{"    panic: assertion failed", false},
		{"568 passed, 2 failed, 3 leaked (117 files, 30.810s)", false},
		{"FAILED:", false},
		{"", false},
		{"Building...", false},
		{"Running promise tests (linux-amd64)...", false},
		{"  passing", false}, // indented, not at column 0
	}
	for _, tc := range cases {
		got := IsPromisePassLine(tc.line)
		if got != tc.want {
			t.Errorf("IsPromisePassLine(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}
