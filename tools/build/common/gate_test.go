package common

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// runGate runs RunGate the way bin/gate's main does and returns what it wrote
// on stdout. RunGate records whether anyone is watching and sets PROMISE_GATE
// for the whole process; both are put back afterwards, so no other test runs
// under a state this one left behind.
func runGate(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("PROMISE_GATE", os.Getenv("PROMISE_GATE"))
	cliObserved(t, true)
	var err error
	out := captureStdout(t, func() { err = RunGate(root, args) })
	return out, err
}

// requireUsageError asserts err is a malformed invocation — exit 2 — that left
// stdout empty, and returns its text.
func requireUsageError(t *testing.T, args []string, out string, err error) string {
	t.Helper()
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("RunGate(%q) = %v, want a usage error", args, err)
	}
	if code := ExitCode(err); code != 2 {
		t.Errorf("RunGate(%q) exits %d, want 2", args, code)
	}
	if out != "" {
		t.Errorf("RunGate(%q) wrote %q on stdout; a usage error writes nothing there", args, out)
	}
	return err.Error()
}

// TestRunGate_NoArgs: a bare bin/gate asked nothing, and is answered with a
// short page — what it expects, what it is, where the rest is — as a usage
// error, so nothing reads it as a result.
func TestRunGate_NoArgs(t *testing.T) {
	for _, args := range [][]string{nil, {"-no-list"}} {
		out, err := runGate(t, t.TempDir(), args...)
		msg := requireUsageError(t, args, out, err)
		want := []string{
			"expecting a subcommand: <gate>, a gate this project answers — `gate --list` names them",
			"promise unknown",
			"measure one property of this tree",
			"run `gate -help` for all of them",
		}
		if got := strings.Split(msg, "\n"); !slices.Equal(got, want) {
			t.Errorf("RunGate(%q) said\n%s\nwant\n%s", args, msg, strings.Join(want, "\n"))
		}
	}
}

// TestRunGate_SetsPromiseGateEnv is the regression test for the T1877 runtime
// half of gate-context enforcement: bin/guard's PROMISE_GATE check is only
// effective if RunGate actually sets the marker, and it must do so
// unconditionally — before the command line is even read — so every path
// (including a bare invocation and an unknown gate) inherits it.
func TestRunGate_SetsPromiseGateEnv(t *testing.T) {
	t.Setenv("PROMISE_GATE", "")
	cliObserved(t, true)
	for _, args := range [][]string{nil, {"bogus"}, {"-bogus"}} {
		os.Unsetenv("PROMISE_GATE")
		captureStdout(t, func() { RunGate("", args) })
		if got := os.Getenv("PROMISE_GATE"); got != "1" {
			t.Fatalf("after RunGate(%q): PROMISE_GATE = %q, want %q", args, got, "1")
		}
	}
}

// TestRunGate_UnknownSubcommand: a name this project does not measure is a
// usage error that points at the listing.
func TestRunGate_UnknownSubcommand(t *testing.T) {
	out, err := runGate(t, "", "bogus")
	msg := requireUsageError(t, []string{"bogus"}, out, err)
	if want := "gate: no gate named \"bogus\" in this project; `gate --list` names them"; msg != want {
		t.Errorf("error = %q, want %q", msg, want)
	}
}

// TestRunGate_UsageErrorsWriteNothing: every malformed command line is ONE
// usage error, exit 2, with nothing on stdout — a runner reads an envelope, a
// listing, or nothing. The old subcommand names are not gates and are refused
// like any other unknown name.
func TestRunGate_UsageErrorsWriteNothing(t *testing.T) {
	clearToolchainOverrides(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-bogus", "--list"}, "unknown flag -bogus"},
		{[]string{"--list", "--list"}, "-list is given twice"},
		{[]string{"-json", "-human", "--list"}, "-json and -human choose one output mode"},
		{[]string{"fit"}, "gate: fit measures " + ContractGateSummary("fit") + ", and writes it only with --envelope; run `bin/run fit` for a result meant for a person"},
		{[]string{"fit", "-no-envelope"}, "writes it only with --envelope"},
		{[]string{"nosuch", "--envelope"}, "no gate named \"nosuch\" in this project; `gate --list` names them"},
		{[]string{"test"}, "no gate named \"test\""},
		{[]string{"fit", "--envelope", "-json"}, "--envelope writes one form only"},
		{[]string{"fit", "fit", "--envelope"}, "unexpected argument \"fit\""},
		{[]string{"--envelope"}, "unknown flag --envelope"},
	} {
		out, err := runGate(t, t.TempDir(), tc.args...)
		if msg := requireUsageError(t, tc.args, out, err); !strings.Contains(msg, tc.want) {
			t.Errorf("RunGate(%q) = %q, want it to say %q", tc.args, msg, tc.want)
		}
	}
}

// TestRunGate_ListFromJSONInput: the listing can be asked for in a -json-input
// file exactly as on the command line, and -json makes it the JSON listing.
func TestRunGate_ListFromJSONInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invocation.json")
	if err := os.WriteFile(path, []byte(`{"list": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runGate(t, t.TempDir(), "-json-input", path, "-json")
	if err != nil {
		t.Fatalf("RunGate: %v", err)
	}
	var got struct {
		Gates []listedGate `json:"gates"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the listing is not JSON: %v\n%s", err, out)
	}
	if len(got.Gates) != len(ContractGateNames()) {
		t.Errorf("listed %d gates, want %d", len(got.Gates), len(ContractGateNames()))
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

// TestRunGate_RefusesUnderToolchainOverride pins the T2108 decision about what a
// verdict is allowed to describe. A gate reports what the tree does on the
// pinned toolchain; run under a hand-pointed one it would report what this
// machine does, which cannot stand as evidence about a (commit, platform, check)
// triple. So it is refused outright rather than allowed to masquerade as a
// pinned result — and refused before anything is measured, so no envelope is
// produced. It is not a usage error: the command line was fine, and the run
// could not complete.
func TestRunGate_RefusesUnderToolchainOverride(t *testing.T) {
	for _, envVar := range []string{"PROMISE_OPT", "PROMISE_LD64LLD", "PROMISE_USE_CLANG"} {
		t.Run(envVar, func(t *testing.T) {
			clearToolchainOverrides(t)
			t.Setenv(envVar, "/custom/tool")

			// tested:go is a real measurement gate: it must not begin. The build
			// is not stubbed, so reaching it would panic.
			out, err := runGate(t, "", "tested:go", "--envelope")
			if err == nil {
				t.Fatal("expected refusal when a toolchain override is in effect")
			}
			if code := ExitCode(err); code != 1 {
				t.Errorf("exit %d, want 1: the invocation was well formed", code)
			}
			if out != "" {
				t.Errorf("a refused run wrote %q on stdout", out)
			}
			if !strings.Contains(err.Error(), "toolchain override") {
				t.Errorf("error should name the override, got: %v", err)
			}
			if !strings.Contains(err.Error(), envVar) {
				t.Errorf("error should name the variable in effect (%s), got: %v", envVar, err)
			}
		})
	}
}

// TestRunGate_ListWorksUnderToolchainOverride: --list answers "which gates does
// this project provide?", which is a property of the repo, not a measurement of
// it. An override must not make the gate inventory unreadable.
func TestRunGate_ListWorksUnderToolchainOverride(t *testing.T) {
	clearToolchainOverrides(t)
	t.Setenv("PROMISE_OPT", "/custom/opt")
	out, err := runGate(t, "", "--list", "-human")
	if err != nil {
		t.Errorf("--list must still work under an override: %v", err)
	}
	if !slices.Equal(strings.Fields(out), ContractGateNames()) {
		t.Errorf("--list printed %q under an override, want the gate names", out)
	}
}

// TestGateGoTestArgv_IsTheSharedSpelling: the coverage gate runs the `go test`
// bin/test and tested:go run, not a second spelling of it.
//
// The go-test gate it was written for did not, for as long as T1817 had been
// bounding everything else: the gate
// kept the bare `go test -v -count=1 ./...` it was written with, so -p and
// -parallel both defaulted to GOMAXPROCS and multiplied — 82 concurrent
// compilers on a 12-core host — and the per-package limit stayed Go's 10m where
// verify gives 30m. That is what turned a cold toolchain materialization into
// three-minute test timeouts on macOS and Windows (T2133), and it is exactly the
// drift docs/gate-system.md's "one implementation in two modes" exists to
// prevent.
func TestGateGoTestArgv_IsTheSharedSpelling(t *testing.T) {
	got := gateGoTestArgv()
	if want := goTestArgs("-v", "-count=1"); !slices.Equal(got, want) {
		t.Errorf("gateGoTestArgv() = %v, want the shared %v", got, want)
	}
	assertBoundedGoTestArgv(t, got)
	if n := len(got); n == 0 || got[n-1] != "./..." {
		t.Errorf("gateGoTestArgv() = %v, want it to end in ./...", got)
	}
	for _, flag := range []string{"-v", "-count=1"} {
		if !slices.Contains(got, flag) {
			t.Errorf("gateGoTestArgv() dropped %s, which its output parsers read: %v", flag, got)
		}
	}
}

// TestGateCoverageArgv_IsTheSharedSpelling: the coverage gate had the same bare
// command and gets the same bounded one, with its instrumentation ahead of the
// package pattern.
func TestGateCoverageArgv_IsTheSharedSpelling(t *testing.T) {
	got := gateCoverageArgv([]string{"pkg/a", "pkg/b"}, "/tmp/cov.out")
	assertBoundedGoTestArgv(t, got)
	if !slices.Contains(got, "-coverpkg=pkg/a,pkg/b") {
		t.Errorf("gateCoverageArgv() lost its package list: %v", got)
	}
	if !slices.Contains(got, "-coverprofile=/tmp/cov.out") {
		t.Errorf("gateCoverageArgv() lost its profile path: %v", got)
	}
	if n := len(got); n == 0 || got[n-1] != "./..." {
		t.Errorf("gateCoverageArgv() = %v, want it to end in ./...", got)
	}
}

// assertBoundedGoTestArgv checks the three flags a gate must not measure
// without: the concurrency bound docs/build-tools.md §"Test Concurrency"
// requires, and verify's per-package timeout.
func assertBoundedGoTestArgv(t *testing.T, argv []string) {
	t.Helper()
	for _, flag := range []string{"-p", "-parallel", "-timeout"} {
		i := slices.Index(argv, flag)
		if i < 0 {
			t.Errorf("argv %v has no %s — the gate is measuring unbounded", argv, flag)
			continue
		}
		if i+1 >= len(argv) || argv[i+1] == "" {
			t.Errorf("argv %v has %s with no value", argv, flag)
		}
	}
}
