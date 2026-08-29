package main

import (
	"strings"
	"testing"
	"time"
)

// TestUsesLivenessProtocolIsPositive locks the gate that decides whether the
// batch budget is armed from the binary's liveness signal or at spawn (T1815).
//
// It must stay a positive check. A negative one (!wasm && !windows) would opt
// every future target in silently, and the runner would then wait for a byte
// that target's generated main never sends — codegen gates the signal on the
// same triple, so the two must agree.
func TestUsesLivenessProtocolIsPositive(t *testing.T) {
	t.Parallel()
	cases := []struct {
		target string
		want   bool
	}{
		{"arm64-apple-macosx26.0.0", true},
		{"x86_64-apple-macosx10.15.0", true},
		{"wasm32-wasi", false},
		{"wasm32-unknown-unknown", false},
		{"x86_64-unknown-linux-musl", false},
		{"x86_64-pc-windows-msvc", false},
		// A target nobody has taught either side about yet must fall back to
		// arming at spawn, not wait forever for a signal.
		{"riscv64-unknown-freebsd", false},
	}
	for _, tc := range cases {
		if got := usesLivenessProtocol(tc.target); got != tc.want {
			t.Errorf("usesLivenessProtocol(%q) = %v, want %v", tc.target, got, tc.want)
		}
	}
}

// TestNeverAliveRunReportBlamesNoTest covers the outcome for a process killed
// before it ever reached main. Every test had exactly zero time, so unlike the
// batch-budget path this must not name one — naming the first test on the
// roster is the misattribution T1815 exists to remove.
func TestNeverAliveRunReportBlamesNoTest(t *testing.T) {
	t.Parallel()
	roster := []rosterEntry{{Name: "a_ok"}, {Name: "b_next"}, {Name: "c_last"}}
	missing := []string{"a_ok", "b_next", "c_last"}

	report := neverAliveRunReport(missing, roster, childOutcomeCounts{},
		10*time.Minute, 42*time.Second, "")

	if !strings.Contains(report, "TIMEOUT (-) <startup>") {
		t.Errorf("expected the phase, not a test, on the outcome line.\nGot:\n%s", report)
	}
	for _, name := range missing {
		if strings.Contains(report, "TIMEOUT (-) "+name) {
			t.Errorf("named %s, which never ran.\nGot:\n%s", name, report)
		}
	}
	if !strings.Contains(report, "never reached main") {
		t.Errorf("expected the reason to name the real cause.\nGot:\n%s", report)
	}
	if !strings.Contains(report, "none of its 3 tests ran") {
		t.Errorf("expected the full roster to be accounted for.\nGot:\n%s", report)
	}
	// The summary must still count them, so a truncated run can never read as
	// a pass in the tail.
	if !strings.Contains(report, "0 passed, 0 failed, 3 timed out") {
		t.Errorf("expected the summary to count every test.\nGot:\n%s", report)
	}
}
