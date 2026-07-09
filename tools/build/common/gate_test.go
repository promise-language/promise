package common

import (
	"encoding/json"
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

// firstTest returns the t-th test record across all groups in order (helper for
// the grouped go-test parser tests).
func flatTests(groups []TestFileGroup) []TestRecord {
	var out []TestRecord
	for _, g := range groups {
		out = append(out, g.Tests...)
	}
	return out
}

func TestParseGoTestGroups_MixedResults(t *testing.T) {
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
	groups := ParseGoTestGroups(output)
	if len(groups) != 2 {
		t.Fatalf("len(groups) = %d, want 2", len(groups))
	}
	// Group 0: bar package with TestFoo (pass) + TestBar (fail).
	if groups[0].File != "github.com/user/pkg/bar" || len(groups[0].Tests) != 2 {
		t.Fatalf("group 0: %+v", groups[0])
	}
	if groups[0].Tests[0].Test != "TestFoo" || groups[0].Tests[0].Status != "pass" || groups[0].Tests[0].Elapsed != 0.001 {
		t.Errorf("group0 test0: %+v", groups[0].Tests[0])
	}
	if groups[0].Tests[1].Test != "TestBar" || groups[0].Tests[1].Status != "fail" {
		t.Errorf("group0 test1: %+v", groups[0].Tests[1])
	}
	if !strings.Contains(groups[0].Tests[1].Context, "expected 1, got 2") ||
		!strings.Contains(groups[0].Tests[1].Context, "extra context") {
		t.Errorf("group0 test1 context = %q", groups[0].Tests[1].Context)
	}
	// Group 1: foo package with TestBaz (pass).
	if groups[1].File != "github.com/user/pkg/foo" || groups[1].Tests[0].Test != "TestBaz" || groups[1].Tests[0].Status != "pass" {
		t.Errorf("group 1: %+v", groups[1])
	}
}

// TestParseGoTestGroups_RepoRelative verifies the repo module prefix is stripped
// so go-test file identity is repo-relative, matching the Promise test gates.
func TestParseGoTestGroups_RepoRelative(t *testing.T) {
	output := `--- PASS: TestX (0.00s)
ok  	github.com/promise-language/promise/compiler/internal/codegen	0.05s`
	groups := ParseGoTestGroups(output)
	if len(groups) != 1 || groups[0].File != "compiler/internal/codegen" {
		t.Fatalf("repo-relative strip failed: %+v", groups)
	}
}

// TestParseGoTestGroups_Skip maps Go SKIP to the gate's excluded status.
func TestParseGoTestGroups_Skip(t *testing.T) {
	output := `--- SKIP: TestSkipped (0.00s)
    foo_test.go:3: not on this platform
ok  	example.com/pkg	0.01s`
	groups := ParseGoTestGroups(output)
	if len(groups) != 1 || len(groups[0].Tests) != 1 || groups[0].Tests[0].Status != "excluded" {
		t.Fatalf("SKIP not mapped to excluded: %+v", groups)
	}
}

func TestParseGoTestGroups_AllPass(t *testing.T) {
	output := `--- PASS: TestA (0.00s)
ok  	example.com/pkg1	0.05s
--- PASS: TestB (0.01s)
ok  	example.com/pkg2	0.06s`
	groups := ParseGoTestGroups(output)
	if len(groups) != 2 {
		t.Fatalf("len(groups) = %d, want 2", len(groups))
	}
	if groups[0].File != "example.com/pkg1" || groups[1].File != "example.com/pkg2" {
		t.Errorf("packages: %q, %q", groups[0].File, groups[1].File)
	}
	for _, e := range flatTests(groups) {
		if e.Status != "pass" {
			t.Errorf("test %q status = %q", e.Test, e.Status)
		}
	}
}

func TestParseGoTestGroups_Empty(t *testing.T) {
	if groups := ParseGoTestGroups(""); len(groups) != 0 {
		t.Errorf("len = %d, want 0", len(groups))
	}
}

func TestParseGoTestGroups_Subtests(t *testing.T) {
	output := `--- PASS: TestFoo (0.00s)
--- PASS: TestFoo/sub1 (0.00s)
--- PASS: TestFoo/sub2 (0.00s)
ok  	example.com/pkg	0.01s`
	tests := flatTests(ParseGoTestGroups(output))
	if len(tests) != 3 {
		t.Fatalf("len = %d, want 3", len(tests))
	}
	if tests[1].Test != "TestFoo/sub1" || tests[2].Test != "TestFoo/sub2" {
		t.Errorf("subtest names: %q, %q", tests[1].Test, tests[2].Test)
	}
}

func TestParseGoTestGroups_ContextTerminatedByPackage(t *testing.T) {
	// Package summary directly follows context (no bare FAIL line).
	output := `--- FAIL: TestY (0.01s)
    y_test.go:5: boom
FAIL	example.com/pkg	0.02s`
	groups := ParseGoTestGroups(output)
	if len(groups) != 1 || len(groups[0].Tests) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	if !strings.Contains(groups[0].Tests[0].Context, "boom") {
		t.Errorf("context = %q", groups[0].Tests[0].Context)
	}
	if groups[0].File != "example.com/pkg" {
		t.Errorf("file = %q", groups[0].File)
	}
}

func TestParseGoTestGroups_ContextTerminatedByNextTest(t *testing.T) {
	// A new --- PASS/FAIL line arrives while collectingContext is still true.
	output := `--- FAIL: TestA (0.01s)
    a_test.go:1: err
--- PASS: TestB (0.02s)
ok  	example.com/pkg	0.03s`
	tests := flatTests(ParseGoTestGroups(output))
	if len(tests) != 2 {
		t.Fatalf("len = %d, want 2", len(tests))
	}
	if !strings.Contains(tests[0].Context, "err") {
		t.Errorf("test 0 context = %q", tests[0].Context)
	}
	if tests[1].Context != "" {
		t.Errorf("test 1 should have no context: %q", tests[1].Context)
	}
}

func TestParseGoTestGroups_EOFWhileCollectingContext(t *testing.T) {
	// EOF reached while still collecting context (no package summary at all).
	output := `--- FAIL: TestZ (0.01s)
    z_test.go:9: dangling context`
	tests := flatTests(ParseGoTestGroups(output))
	if len(tests) != 1 {
		t.Fatalf("len = %d, want 1", len(tests))
	}
	if !strings.Contains(tests[0].Context, "dangling context") {
		t.Errorf("context = %q", tests[0].Context)
	}
}

func TestParseGoTestGroups_FailContext(t *testing.T) {
	output := `--- FAIL: TestX (0.01s)
    x_test.go:10: line1
    x_test.go:11: line2
FAIL
FAIL	example.com/pkg	0.02s`
	tests := flatTests(ParseGoTestGroups(output))
	if len(tests) != 1 {
		t.Fatalf("len = %d, want 1", len(tests))
	}
	if !strings.Contains(tests[0].Context, "line1") || !strings.Contains(tests[0].Context, "line2") {
		t.Errorf("context = %q", tests[0].Context)
	}
}

// runnerScannerCap mirrors the gate runner's per-line bufio.Scanner limit
// (cmd/runner/actions.go in the tracker repo). The gate's stdout must never
// carry a line at/above this, or the runner's drain aborts (bufio.ErrTooLong)
// and the unbuffered pipe deadlocks the gate to its wall-clock timeout (T0777).
const runnerScannerCap = 1024 * 1024

// maxJSONLine returns the longest single line of v's indented JSON encoding —
// what the gate prints to stdout and the runner reads line by line.
func maxJSONLine(t *testing.T, v any) int {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	max := 0
	for _, ln := range strings.Split(string(data), "\n") {
		if len(ln) > max {
			max = len(ln)
		}
	}
	return max
}

// TestParseGoTestGroups_ContextBounded is the T0777 regression guard: a failing
// test that dumps a huge body (e.g. assertContains printing the full IR) must
// NOT produce a context that JSON-encodes onto a line large enough to deadlock
// the runner drain. The context is bounded and marked truncated; the resulting
// GateOutput's longest line stays well under the runner's scanner cap.
func TestParseGoTestGroups_ContextBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("--- FAIL: TestBigIR (0.50s)\n")
	for i := 0; i < 40000; i++ { // ~2MB of indented "IR" if left unbounded
		b.WriteString("    ")
		b.WriteString(strings.Repeat("x", 48))
		b.WriteString("\n")
	}
	b.WriteString("FAIL\n")
	b.WriteString("FAIL\tgithub.com/promise-language/promise/compiler/internal/codegen\t0.50s\n")

	groups := ParseGoTestGroups(b.String())
	tests := flatTests(groups)
	if len(tests) != 1 {
		t.Fatalf("len = %d, want 1", len(tests))
	}
	ctx := tests[0].Context
	if len(ctx) > maxContextBytes+len("\n… (truncated; full output in the gate log)") {
		t.Errorf("context not bounded: %d bytes", len(ctx))
	}
	if !strings.Contains(ctx, "truncated") {
		t.Errorf("expected truncation marker, got %q", ctx[:min(80, len(ctx))])
	}
	if !strings.HasPrefix(ctx, "x") {
		t.Errorf("context should keep the head of the failure, got %q", ctx[:min(40, len(ctx))])
	}

	out := &GateOutput{Target: "darwin-arm64", Metrics: map[string]float64{}, Files: groups, Complete: "go-tests"}
	if got := maxJSONLine(t, out); got >= runnerScannerCap {
		t.Errorf("gate JSON has a %d-byte line (>= runner cap %d): would deadlock the runner drain", got, runnerScannerCap)
	}
}

// TestClampContext covers the bounding helper directly.
func TestClampContext(t *testing.T) {
	if got := clampContext(""); got != "" {
		t.Errorf("empty: got %q", got)
	}
	small := "panic: assertion failed\n  expected 3, got 4"
	if got := clampContext(small); got != small {
		t.Errorf("small context altered: %q", got)
	}
	manyLines := strings.Repeat("line\n", maxContextLines+50)
	got := clampContext(manyLines)
	if n := strings.Count(got, "\n"); n > maxContextLines+1 {
		t.Errorf("line bound exceeded: %d newlines", n)
	}
	if !strings.Contains(got, "truncated") {
		t.Error("expected truncation marker on over-long context")
	}
	big := strings.Repeat("z", maxContextBytes*4)
	if got := clampContext(big); len(got) > maxContextBytes+64 {
		t.Errorf("byte bound exceeded: %d bytes", len(got))
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
	out, err := RunTeeStderr("", teeStub(t), "-line", "hello tee stderr")
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello tee stderr" {
		t.Errorf("RunTeeStderr = %q, want %q", out, "hello tee stderr")
	}
}

// TestRunTeeStderr_ErrorReturnsCaptured verifies partial output is returned on error.
func TestRunTeeStderr_ErrorReturnsCaptured(t *testing.T) {
	out, err := RunTeeStderr("", teeStub(t), "-line", "partial", "-exit", "1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if out != "partial" {
		t.Errorf("RunTeeStderr output on error = %q, want %q", out, "partial")
	}
}

// TestRunTeeStderr_ErrorWrapsCommandName verifies the error message includes the command.
func TestRunTeeStderr_ErrorWrapsCommandName(t *testing.T) {
	_, err := RunTeeStderr("", teeStub(t), "-exit", "2")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), teeStubName) {
		t.Errorf("error %q does not mention command name", err.Error())
	}
}
