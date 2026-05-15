package common

import (
	"strings"
	"testing"
)

// TestRunGate_NoArgs verifies the usage error when no subcommand is given.
func TestRunGate_NoArgs(t *testing.T) {
	err := RunGate("", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error %q does not contain 'usage'", err.Error())
	}
}

// TestRunGate_UnknownSubcommand verifies the error for an unrecognized subcommand.
func TestRunGate_UnknownSubcommand(t *testing.T) {
	err := RunGate("", []string{"bogus"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("error %q does not contain 'unknown subcommand'", err.Error())
	}
}

// TestRunGate_TestBadFlag verifies that unrecognized flags are rejected early.
func TestRunGate_TestBadFlag(t *testing.T) {
	err := RunGate("", []string{"test", "--bogus"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error %q does not contain 'usage'", err.Error())
	}
}

// TestRunGate_WasmTestsBadFlag verifies that unrecognized flags are rejected for wasm-test.
func TestRunGate_WasmTestsBadFlag(t *testing.T) {
	err := RunGate("", []string{"wasm-test", "--bogus"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error %q does not contain 'usage'", err.Error())
	}
}

// TestRunGate_OldWasmTestsNameRejected verifies that the old plural "wasm-tests" subcommand
// is no longer recognized — renamed to "wasm-test" by T0245.
func TestRunGate_OldWasmTestsNameRejected(t *testing.T) {
	err := RunGate("", []string{"wasm-tests"})
	if err == nil {
		t.Fatal("expected error for removed subcommand wasm-tests, got nil")
	}
	if !strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("error %q does not contain 'unknown subcommand'", err.Error())
	}
}

// TestRunGate_GoTestBadFlag verifies that unrecognized flags are rejected.
func TestRunGate_GoTestBadFlag(t *testing.T) {
	err := RunGate("", []string{"go-test", "--bogus"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error %q does not contain 'usage'", err.Error())
	}
}

// TestRunGate_StressBadFlag verifies that non-numeric args are rejected.
func TestRunGate_StressBadFlag(t *testing.T) {
	err := RunGate("", []string{"stress", "--bogus"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error %q does not contain 'usage'", err.Error())
	}
}

// TestRunGate_WasmSizeBadFlag verifies that unrecognized flags are rejected for wasm-size.
func TestRunGate_WasmSizeBadFlag(t *testing.T) {
	err := RunGate("", []string{"wasm-size", "--bogus"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error %q does not contain 'usage'", err.Error())
	}
}

// TestRunGate_CoverageBadFlag verifies that unrecognized flags are rejected.
func TestRunGate_CoverageBadFlag(t *testing.T) {
	err := RunGate("", []string{"coverage", "--bogus"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error %q does not contain 'usage'", err.Error())
	}
}

// TestParseGoTestOutput verifies counting of PASS/FAIL lines.
func TestParseGoTestOutput(t *testing.T) {
	output := `=== RUN   TestFoo
--- PASS: TestFoo (0.00s)
=== RUN   TestBar
--- PASS: TestBar (0.01s)
=== RUN   TestBaz
--- FAIL: TestBaz (0.00s)
FAIL	example.com/pkg	0.123s`
	passed, failed := ParseGoTestOutput(output)
	if passed != 2 {
		t.Errorf("passed = %d, want 2", passed)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
}

// TestParseGoTestOutput_AllPass verifies when all tests pass.
func TestParseGoTestOutput_AllPass(t *testing.T) {
	output := `--- PASS: TestA (0.00s)
--- PASS: TestB (0.00s)
ok	example.com/pkg	0.05s`
	passed, failed := ParseGoTestOutput(output)
	if passed != 2 {
		t.Errorf("passed = %d, want 2", passed)
	}
	if failed != 0 {
		t.Errorf("failed = %d, want 0", failed)
	}
}

// TestParseGoTestOutput_Empty verifies empty input returns zeros.
func TestParseGoTestOutput_Empty(t *testing.T) {
	passed, failed := ParseGoTestOutput("")
	if passed != 0 || failed != 0 {
		t.Errorf("passed=%d, failed=%d, want 0,0", passed, failed)
	}
}

func TestParseGoTestEntries_MixedResults(t *testing.T) {
	output := `=== RUN   TestFoo
--- PASS: TestFoo (0.001s)
=== RUN   TestBar
--- FAIL: TestBar (0.003s)
    bar_test.go:42: expected 1, got 2
    bar_test.go:43: extra context
FAIL
FAIL	github.com/user/pkg/bar	0.010s
=== RUN   TestBaz
--- PASS: TestBaz (0.002s)
ok  	github.com/user/pkg/foo	0.005s`
	entries := ParseGoTestEntries("linux-amd64", output)
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3", len(entries))
	}
	// Entry 0: TestFoo pass in bar package
	if entries[0].Test != "TestFoo" || entries[0].Outcome != "pass" || entries[0].File != "github.com/user/pkg/bar" {
		t.Errorf("entry 0: %+v", entries[0])
	}
	if entries[0].Target != "linux-amd64" {
		t.Errorf("entry 0 target = %q", entries[0].Target)
	}
	if entries[0].Elapsed != 0.001 {
		t.Errorf("entry 0 elapsed = %v", entries[0].Elapsed)
	}
	// Entry 1: TestBar fail with context
	if entries[1].Test != "TestBar" || entries[1].Outcome != "FAIL" {
		t.Errorf("entry 1: %+v", entries[1])
	}
	if !strings.Contains(entries[1].Context, "expected 1, got 2") {
		t.Errorf("entry 1 context = %q", entries[1].Context)
	}
	if !strings.Contains(entries[1].Context, "extra context") {
		t.Errorf("entry 1 missing second context line: %q", entries[1].Context)
	}
	// Entry 2: TestBaz pass in foo package
	if entries[2].Test != "TestBaz" || entries[2].Outcome != "pass" || entries[2].File != "github.com/user/pkg/foo" {
		t.Errorf("entry 2: %+v", entries[2])
	}
}

func TestParseGoTestEntries_AllPass(t *testing.T) {
	output := `--- PASS: TestA (0.00s)
ok  	example.com/pkg1	0.05s
--- PASS: TestB (0.01s)
ok  	example.com/pkg2	0.06s`
	entries := ParseGoTestEntries("linux-amd64", output)
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if entries[0].File != "example.com/pkg1" || entries[1].File != "example.com/pkg2" {
		t.Errorf("packages: %q, %q", entries[0].File, entries[1].File)
	}
	for i, e := range entries {
		if e.Outcome != "pass" {
			t.Errorf("entry %d outcome = %q", i, e.Outcome)
		}
	}
}

func TestParseGoTestEntries_Empty(t *testing.T) {
	entries := ParseGoTestEntries("linux-amd64", "")
	if len(entries) != 0 {
		t.Errorf("len = %d, want 0", len(entries))
	}
}

func TestParseGoTestEntries_Subtests(t *testing.T) {
	output := `--- PASS: TestFoo (0.00s)
--- PASS: TestFoo/sub1 (0.00s)
--- PASS: TestFoo/sub2 (0.00s)
ok  	example.com/pkg	0.01s`
	entries := ParseGoTestEntries("linux-amd64", output)
	if len(entries) != 3 {
		t.Fatalf("len = %d, want 3", len(entries))
	}
	if entries[1].Test != "TestFoo/sub1" || entries[2].Test != "TestFoo/sub2" {
		t.Errorf("subtest names: %q, %q", entries[1].Test, entries[2].Test)
	}
}

func TestParseGoTestEntries_ContextTerminatedByPackage(t *testing.T) {
	// Package summary directly follows context (no bare FAIL line).
	output := `--- FAIL: TestY (0.01s)
    y_test.go:5: boom
FAIL	example.com/pkg	0.02s`
	entries := ParseGoTestEntries("linux-amd64", output)
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	if !strings.Contains(entries[0].Context, "boom") {
		t.Errorf("context = %q", entries[0].Context)
	}
	if entries[0].File != "example.com/pkg" {
		t.Errorf("file = %q", entries[0].File)
	}
}

func TestParseGoTestEntries_ContextTerminatedByNextTest(t *testing.T) {
	// A new --- PASS/FAIL line arrives while collectingContext is still true.
	output := `--- FAIL: TestA (0.01s)
    a_test.go:1: err
--- PASS: TestB (0.02s)
ok  	example.com/pkg	0.03s`
	entries := ParseGoTestEntries("linux-amd64", output)
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	if !strings.Contains(entries[0].Context, "err") {
		t.Errorf("entry 0 context = %q", entries[0].Context)
	}
	if entries[1].Context != "" {
		t.Errorf("entry 1 should have no context: %q", entries[1].Context)
	}
}

func TestParseGoTestEntries_EOFWhileCollectingContext(t *testing.T) {
	// EOF reached while still collecting context (no package summary at all).
	output := `--- FAIL: TestZ (0.01s)
    z_test.go:9: dangling context`
	entries := ParseGoTestEntries("linux-amd64", output)
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	if !strings.Contains(entries[0].Context, "dangling context") {
		t.Errorf("context = %q", entries[0].Context)
	}
}

func TestParseGoTestEntries_FailContext(t *testing.T) {
	output := `--- FAIL: TestX (0.01s)
    x_test.go:10: line1
    x_test.go:11: line2
FAIL
FAIL	example.com/pkg	0.02s`
	entries := ParseGoTestEntries("linux-amd64", output)
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	if !strings.Contains(entries[0].Context, "line1") || !strings.Contains(entries[0].Context, "line2") {
		t.Errorf("context = %q", entries[0].Context)
	}
}

// TestParseStressOutput verifies parsing of stress test report.
func TestParseStressOutput(t *testing.T) {
	output := `=== Stress Test Report ===
Target: darwin-arm64
50 iterations over 45.2s

FLAKY (2 tests):
  concurrency/stress_unbuffered.pr
    test_channel_send              47/50 (94.0%)

STABLE: 45 tests across 12 files`
	iters, flaky := ParseStressOutput(output)
	if iters != 50 {
		t.Errorf("iterations = %d, want 50", iters)
	}
	if flaky != 2 {
		t.Errorf("flakyCount = %d, want 2", flaky)
	}
}

// TestParseStressOutput_NoFlaky verifies parsing when no flaky tests.
func TestParseStressOutput_NoFlaky(t *testing.T) {
	output := `=== Stress Test Report ===
Target: linux-amd64
100 iterations over 120.5s

STABLE: 60 tests across 15 files`
	iters, flaky := ParseStressOutput(output)
	if iters != 100 {
		t.Errorf("iterations = %d, want 100", iters)
	}
	if flaky != 0 {
		t.Errorf("flakyCount = %d, want 0", flaky)
	}
}

// TestParseStressOutput_SingleFlaky verifies singular "test" form.
func TestParseStressOutput_SingleFlaky(t *testing.T) {
	output := "50 iterations over 30s\n\nFLAKY (1 test):\n  foo.pr"
	iters, flaky := ParseStressOutput(output)
	if iters != 50 {
		t.Errorf("iterations = %d, want 50", iters)
	}
	if flaky != 1 {
		t.Errorf("flakyCount = %d, want 1", flaky)
	}
}

// TestParseStressOutput_Empty verifies empty input returns zeros.
func TestParseStressOutput_Empty(t *testing.T) {
	iters, flaky := ParseStressOutput("")
	if iters != 0 || flaky != 0 {
		t.Errorf("iterations=%d, flaky=%d, want 0,0", iters, flaky)
	}
}

// TestParseCoverageTotal_GoFormat verifies parsing Go tool cover output.
func TestParseCoverageTotal_GoFormat(t *testing.T) {
	output := "pkg/foo.go:42:\tFoo\t\t\t80.0%\ntotal:\t(statements)\t65.2%"
	pct := ParseCoverageTotal(output)
	if pct != 65.2 {
		t.Errorf("pct = %f, want 65.2", pct)
	}
}

// TestParseCoverageTotal_PromiseFormat verifies parsing Promise coverage output.
func TestParseCoverageTotal_PromiseFormat(t *testing.T) {
	output := "file.pr: 80.0% (4/5 blocks)\ntotal: 75.3% (42/56 blocks)"
	pct := ParseCoverageTotal(output)
	// Should match the first "total:" line
	if pct != 75.3 {
		t.Errorf("pct = %f, want 75.3", pct)
	}
}

// TestParseCoverageTotal_ZeroPercent verifies 0.0% is parsed correctly.
func TestParseCoverageTotal_ZeroPercent(t *testing.T) {
	output := "total:\t(statements)\t0.0%"
	pct := ParseCoverageTotal(output)
	if pct != 0.0 {
		t.Errorf("pct = %f, want 0.0", pct)
	}
}

// TestParseCoverageTotal_HundredPercent verifies 100.0% is parsed.
func TestParseCoverageTotal_HundredPercent(t *testing.T) {
	output := "total: 100.0% (56/56 blocks)"
	pct := ParseCoverageTotal(output)
	if pct != 100.0 {
		t.Errorf("pct = %f, want 100.0", pct)
	}
}

// TestParseCoverageTotal_NoMatch verifies empty/non-matching input returns 0.
func TestParseCoverageTotal_NoMatch(t *testing.T) {
	if pct := ParseCoverageTotal(""); pct != 0 {
		t.Errorf("empty: pct = %f, want 0", pct)
	}
	if pct := ParseCoverageTotal("no total line here"); pct != 0 {
		t.Errorf("no match: pct = %f, want 0", pct)
	}
}

// TestRunTeeStderr_CapturesOutput verifies that RunTeeStderr captures stdout.
func TestRunTeeStderr_CapturesOutput(t *testing.T) {
	out, err := RunTeeStderr("", "echo", "hello tee stderr")
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello tee stderr" {
		t.Errorf("RunTeeStderr = %q, want %q", out, "hello tee stderr")
	}
}

// TestRunTeeStderr_ErrorReturnsCaptured verifies partial output is returned on error.
func TestRunTeeStderr_ErrorReturnsCaptured(t *testing.T) {
	out, err := RunTeeStderr("", "sh", "-c", "echo partial; exit 1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if out != "partial" {
		t.Errorf("RunTeeStderr output on error = %q, want %q", out, "partial")
	}
}

// TestRunTeeStderr_ErrorWrapsCommandName verifies the error message includes the command.
func TestRunTeeStderr_ErrorWrapsCommandName(t *testing.T) {
	_, err := RunTeeStderr("", "sh", "-c", "exit 2")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "sh") {
		t.Errorf("error %q does not mention command name", err.Error())
	}
}
