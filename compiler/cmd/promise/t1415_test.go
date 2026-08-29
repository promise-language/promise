package main

import (
	"errors"
	"fmt"

	"os/exec"

	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/promise/compiler/internal/module"
)

// T1415: a test process that dies mid-batch must never report success. The
// runner reconciles the child's result lines against the compile-time roster
// and synthesizes an INCOMPLETE outcome for every test that never reported.

func TestUnreportedTests(t *testing.T) {
	roster := []rosterEntry{
		{Name: "a"},
		{Name: "skipped", Excluded: true},
		{Name: "b"},
		{Name: "c"},
	}

	// Declaration order preserved; excluded entries never count as missing.
	got := unreportedTests(roster, map[string]bool{"a": true})
	want := []string{"b", "c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("unreportedTests = %v, want %v", got, want)
	}

	// All eligible tests reported → nothing missing (the excluded one is not
	// expected to report).
	if got := unreportedTests(roster, map[string]bool{"a": true, "b": true, "c": true}); len(got) != 0 {
		t.Errorf("unreportedTests with full coverage = %v, want empty", got)
	}

	// Nothing reported → every eligible test is missing.
	if got := unreportedTests(roster, map[string]bool{}); strings.Join(got, ",") != "a,b,c" {
		t.Errorf("unreportedTests with no results = %v, want a,b,c", got)
	}

	// A nil roster (unknown test inventory) disables the check.
	if got := unreportedTests(nil, map[string]bool{}); len(got) != 0 {
		t.Errorf("unreportedTests(nil) = %v, want empty", got)
	}

	eligible, skipped := rosterCounts(roster)
	if eligible != 3 || skipped != 1 {
		t.Errorf("rosterCounts = (%d, %d), want (3, 1)", eligible, skipped)
	}
}

func TestJoinCapped(t *testing.T) {
	t.Parallel()
	names := []string{"a", "b", "c"}
	if got := joinCapped(names, 8); got != "a, b, c" {
		t.Errorf("joinCapped = %q", got)
	}
	if got := joinCapped(names, 2); got != "a, b, ... and 1 more" {
		t.Errorf("joinCapped truncated = %q", got)
	}
}

// The runner now synthesizes a summary line for an incomplete run, so the
// --json reconciliation must key off the INCOMPLETE line (not the absence of a
// summary) to attribute the abort to the first unreported test.
func TestBuildTestRecordsIncomplete(t *testing.T) {
	t.Parallel()
	output := strings.Join([]string{
		batchRoster("a", false, "b", false, "c", false),
		"pass (0.001s) a",
		"INCOMPLETE (-) b",
		"  incomplete: process exited (status 0) without reporting a result - 2 of 3 tests did not run: b, c",
		"",
		"1 passed, 0 failed, 2 incomplete (0.021s)",
	}, "\n")

	recs := buildTestRecords("tests/i_test.pr", output, true)
	if len(recs) != 3 {
		t.Fatalf("want 3 records, got %d: %+v", len(recs), recs)
	}
	if s := statusOf(recs, "a"); s != "pass" {
		t.Errorf("a: want pass, got %s", s)
	}
	if s := statusOf(recs, "b"); s != "fail" {
		t.Errorf("b (first unreported): want fail, got %s", s)
	}
	if s := statusOf(recs, "c"); s != "not-run" {
		t.Errorf("c: want not-run, got %s", s)
	}
	for _, r := range recs {
		if r.Test == "b" && !strings.Contains(r.Context, "without reporting") &&
			!strings.Contains(r.Context, "before reporting") {
			t.Errorf("b context missing detail: %q", r.Context)
		}
	}
}

// The child's synthesized summary must parse with the multi-file parent's
// summary regex, including the trailing incomplete bucket.
func TestParentSummaryParsesIncomplete(t *testing.T) {
	t.Parallel()
	m := parentSummaryRe.FindStringSubmatch("1 passed, 0 failed, 2 incomplete (0.021s)")
	if m == nil {
		t.Fatal("parent summary regex did not match an incomplete summary")
	}
	if m[1] != "1" || m[2] != "0" {
		t.Errorf("passed/failed = %q/%q, want 1/0", m[1], m[2])
	}
	if m[9] != "2" {
		t.Errorf("incomplete group = %q, want 2", m[9])
	}

	// The full vocabulary still parses in canonical order.
	full := "1 passed, 2 failed, 3 skipped, 4 leaked, 5 timed out, 6 allowed leaks, 7 stale allow_leaks, 8 memlimit, 9 incomplete (1.0s)"
	m = parentSummaryRe.FindStringSubmatch(full)
	if m == nil {
		t.Fatal("parent summary regex did not match the full summary vocabulary")
	}
	for i, want := range []string{"", "1", "2", "3", "4", "5", "6", "7", "8", "9"} {
		if i == 0 {
			continue
		}
		if m[i] != want {
			t.Errorf("group %d = %q, want %q", i, m[i], want)
		}
	}
}

func TestBuildRosterExclusions(t *testing.T) {
	excludes := map[string][]string{
		"onWindows": {"windows"},
		"onPosix":   {"posix"},
		"onArm":     {"aarch64"},
		"unknown":   {"plan9"}, // unknown identifier never matches
		"multi":     {"windows", "macos"},
	}
	names := []string{"always", "onWindows", "onPosix", "onArm", "unknown", "multi"}

	cases := []struct {
		target string
		want   map[string]bool // name → excluded
	}{
		{"x86_64-pc-windows-msvc", map[string]bool{
			"onWindows": true, "multi": true,
		}},
		{"aarch64-apple-macosx14.0.0", map[string]bool{
			"onPosix": true, "onArm": true, "multi": true,
		}},
		{"x86_64-unknown-linux-musl", map[string]bool{
			"onPosix": true,
		}},
		{"wasm32-wasi", map[string]bool{}},
	}
	for _, tc := range cases {
		roster := buildRoster("batch", names, excludes, nil, tc.target)
		if len(roster) != len(names) {
			t.Fatalf("%s: roster has %d entries, want %d", tc.target, len(roster), len(names))
		}
		for i, e := range roster {
			if e.Name != names[i] {
				t.Errorf("%s: entry %d = %q, want %q (declaration order)", tc.target, i, e.Name, names[i])
			}
			if e.Excluded != tc.want[e.Name] {
				t.Errorf("%s: %s excluded = %v, want %v", tc.target, e.Name, e.Excluded, tc.want[e.Name])
			}
		}
		// The completeness check must never demand a result from an excluded test.
		missing := unreportedTests(roster, map[string]bool{})
		eligible, skipped := rosterCounts(roster)
		if len(missing) != eligible || eligible+skipped != len(names) {
			t.Errorf("%s: missing=%d eligible=%d skipped=%d (of %d)",
				tc.target, len(missing), eligible, skipped, len(names))
		}
	}
}

// e2e rosters carry a single "main" entry gated by the file-level `target(...)`
// exclude set, not by per-test excludes.
func TestBuildRosterE2E(t *testing.T) {
	roster := buildRoster("e2e", []string{"main"}, nil, []string{"windows"}, "x86_64-pc-windows-msvc")
	if len(roster) != 1 || roster[0].Name != "main" || !roster[0].Excluded {
		t.Errorf("windows e2e roster = %+v, want main excluded", roster)
	}
	roster = buildRoster("e2e", []string{"main"}, nil, []string{"windows"}, "x86_64-unknown-linux-musl")
	if len(roster) != 1 || roster[0].Excluded {
		t.Errorf("linux e2e roster = %+v, want main eligible", roster)
	}
	// Per-test excludes are ignored for the e2e kind.
	roster = buildRoster("e2e", []string{"main"}, map[string][]string{"main": {"linux"}}, nil,
		"x86_64-unknown-linux-musl")
	if roster[0].Excluded {
		t.Error("e2e roster honored a batch test exclude")
	}
}

// A cache hit with no usable meta must report usable=false so the caller
// recompiles instead of running with the completeness check silently disabled.
func TestCachedBatchRoster(t *testing.T) {
	if _, ok := cachedBatchRoster(nil, "x86_64-unknown-linux-musl"); ok {
		t.Error("nil meta reported a usable roster")
	}
	if _, ok := cachedBatchRoster(&module.CacheMeta{}, "x86_64-unknown-linux-musl"); ok {
		t.Error("meta with no tests reported a usable roster")
	}
	meta := &module.CacheMeta{
		Tests:        []string{"a", "b"},
		TestExcludes: map[string][]string{"b": {"linux"}},
	}
	roster, ok := cachedBatchRoster(meta, "x86_64-unknown-linux-musl")
	if !ok {
		t.Fatal("populated meta reported an unusable roster")
	}
	if len(roster) != 2 || roster[0].Name != "a" || roster[0].Excluded || !roster[1].Excluded {
		t.Errorf("cached roster = %+v, want [a eligible, b excluded]", roster)
	}
}

// The roster the completeness check uses and the roster the --json parent
// parses are now the same value, so the wire format must round-trip: build →
// emit → parse must preserve names, order, and exclusion flags. The marker is
// emitted only for --json children; a plain run must stay marker-free.
func TestEmitRosterRoundTrip(t *testing.T) {
	roster := buildRoster("batch", []string{"a", "b"},
		map[string][]string{"b": {"linux"}}, nil, "x86_64-unknown-linux-musl")

	out := captureStdout(t, func() { emitRoster(roster, "batch") })
	if out != "" {
		t.Errorf("plain run emitted a roster marker: %q", out)
	}

	saved := childRoster
	childRoster = true
	defer func() { childRoster = saved }()

	out = captureStdout(t, func() { emitRoster(roster, "batch") })
	if !strings.HasPrefix(out, rosterMarkerPrefix) {
		t.Fatalf("marker missing its prefix: %q", out)
	}
	marker, _, _, _, _ := parseChildOutput(out)
	if marker == nil {
		t.Fatalf("emitted marker did not parse back: %q", out)
	}
	if marker.Kind != "batch" || len(marker.Tests) != 2 {
		t.Fatalf("round-tripped marker = %+v", marker)
	}
	if marker.Tests[0].Name != "a" || marker.Tests[0].Excluded ||
		marker.Tests[1].Name != "b" || !marker.Tests[1].Excluded {
		t.Errorf("round-tripped entries = %+v, want [a eligible, b excluded]", marker.Tests)
	}
}

// --- exit status reporting -----------------------------------------------

func TestExitStatusDescription(t *testing.T) {
	t.Parallel()
	if got := exitStatusDescription(nil); got != "status 0" {
		t.Errorf("nil error = %q, want %q", got, "status 0")
	}
	if got := exitStatusDescription(errors.New("boom")); got != "boom" {
		t.Errorf("plain error = %q, want %q", got, "boom")
	}
	if got := exitStatusDescription(exitErrWithCode(t, 3)); got != "status 3" {
		t.Errorf("exit 3 = %q, want %q", got, "status 3")
	}
	// A wrapped ExitError is still recognized (errors.As, not a type assertion).
	wrapped := fmt.Errorf("running test binary: %w", exitErrWithCode(t, 7))
	if got := exitStatusDescription(wrapped); got != "status 7" {
		t.Errorf("wrapped exit 7 = %q, want %q", got, "status 7")
	}
	if runtime.GOOS != "windows" {
		got := exitStatusDescription(signalKilledErr(t))
		if !strings.Contains(got, "signal") {
			t.Errorf("signal death = %q, want it to mention the signal", got)
		}
	}
}

func TestIncompleteExitCode(t *testing.T) {
	t.Parallel()
	// A deceptive exit 0 becomes a failure.
	if got := incompleteExitCode(nil); got != 1 {
		t.Errorf("nil error = %d, want 1", got)
	}
	// A non-ExitError (spawn failure) also fails.
	if got := incompleteExitCode(errors.New("boom")); got != 1 {
		t.Errorf("plain error = %d, want 1", got)
	}
	// A real non-zero child status is preserved.
	if got := incompleteExitCode(exitErrWithCode(t, 3)); got != 3 {
		t.Errorf("exit 3 = %d, want 3", got)
	}
	if got := incompleteExitCode(fmt.Errorf("wrap: %w", exitErrWithCode(t, 3))); got != 3 {
		t.Errorf("wrapped exit 3 = %d, want 3", got)
	}
	if runtime.GOOS != "windows" {
		// A signal death has ExitCode() == -1, which is not a usable status.
		if got := incompleteExitCode(signalKilledErr(t)); got != 1 {
			t.Errorf("signal death = %d, want 1", got)
		}
	}
}

// exitErrWithCode runs a trivial child that exits with the given non-zero code
// and returns the resulting *exec.ExitError.
func exitErrWithCode(t *testing.T, code int) error {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", fmt.Sprintf("exit %d", code))
	} else {
		cmd = exec.Command("sh", "-c", fmt.Sprintf("exit %d", code))
	}
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected an *exec.ExitError, got %v", err)
	}
	return err
}

// signalKilledErr returns the error from a child killed by a signal (unix only).
func signalKilledErr(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "kill -9 $$").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected an *exec.ExitError, got %v", err)
	}
	return err
}

// --- child output printing ------------------------------------------------

// printChildTestOutput is the shared seam behind both runners: it rewrites the
// summary with real wall-clock time, annotates result lines with the target
// suffix, and reports which tests produced a result (the input to the
// completeness check).
func TestPrintChildTestOutputRewritesSummaryAndReports(t *testing.T) {
	child := strings.Join([]string{
		"pass (0.001s) a",
		"FAIL (0.002s) b",
		"  panic: assertion failed",
		"LEAK (0.003s) c",
		"  leak: 1 allocations not freed",
		"TIMEOUT (0.004s) d",
		"  timeout: exceeded 60s limit",
		"",
		"1 passed, 1 failed, 2 skipped, 1 leaked, 1 timed out, 3 allowed leaks, 4 stale allow_leaks (9.999s)",
	}, "\n")

	var counts childOutcomeCounts
	var reported map[string]bool
	var sawSummary bool
	out := captureStdout(t, func() {
		counts, reported, sawSummary = printChildTestOutput(child, 1500*time.Millisecond, " [wasm32-wasi]", childOutputOpts{})
	})

	if !sawSummary {
		t.Error("sawSummary = false, want true")
	}
	want := childOutcomeCounts{passed: 1, failed: 1, leaked: 1, timedOut: 1}
	if counts != want {
		t.Errorf("counts = %+v, want %+v", counts, want)
	}
	for _, n := range []string{"a", "b", "c", "d"} {
		if !reported[n] {
			t.Errorf("test %q missing from the reported set: %v", n, reported)
		}
	}
	if len(reported) != 4 {
		t.Errorf("reported = %v, want exactly 4 entries", reported)
	}
	// The summary carries real elapsed time, the full bucket vocabulary, and
	// the target suffix.
	wantSummary := "1 passed, 1 failed, 2 skipped, 1 leaked, 1 timed out, 3 allowed leaks, 4 stale allow_leaks (1.500s) [wasm32-wasi]"
	if !strings.Contains(out, wantSummary) {
		t.Errorf("missing rewritten summary %q.\nOutput:\n%s", wantSummary, out)
	}
	if strings.Contains(out, "9.999s") {
		t.Errorf("child's own elapsed time leaked into the summary.\nOutput:\n%s", out)
	}
	// Result lines get the suffix; context lines are passed through verbatim.
	if !strings.Contains(out, "pass (0.001s) a [wasm32-wasi]") ||
		!strings.Contains(out, "TIMEOUT (0.004s) d [wasm32-wasi]") {
		t.Errorf("result lines missing the target suffix.\nOutput:\n%s", out)
	}
	if !strings.Contains(out, "  panic: assertion failed\n") ||
		strings.Contains(out, "panic: assertion failed [wasm32-wasi]") {
		t.Errorf("context line should pass through unannotated.\nOutput:\n%s", out)
	}
}

// With no target override nothing is annotated, and a child that died before
// printing a summary is reported as such (the trigger for the roster check).
func TestPrintChildTestOutputNoSummary(t *testing.T) {
	var counts childOutcomeCounts
	var reported map[string]bool
	var sawSummary bool
	out := captureStdout(t, func() {
		counts, reported, sawSummary = printChildTestOutput("pass (0.001s) a\n", time.Second, "", childOutputOpts{})
	})
	if sawSummary {
		t.Error("sawSummary = true for a truncated run")
	}
	if counts.passed != 1 || len(reported) != 1 || !reported["a"] {
		t.Errorf("counts = %+v, reported = %v", counts, reported)
	}
	if strings.TrimSpace(out) != "pass (0.001s) a" {
		t.Errorf("output = %q, want the line verbatim", out)
	}
}

// T0689: the raw PAL fatal line is suppressed only when the caller is about to
// synthesize its own MEMLIMIT block.
func TestPrintChildTestOutputDropsMemlimitFatal(t *testing.T) {
	child := "pass (0.001s) a\nfatal: memory limit exceeded (2 GiB)\n"

	out := captureStdout(t, func() { printChildTestOutput(child, time.Second, "", childOutputOpts{dropMemlimitFatal: true}) })
	if strings.Contains(out, "memory limit exceeded") {
		t.Errorf("fatal line should be dropped.\nOutput:\n%s", out)
	}
	out = captureStdout(t, func() { printChildTestOutput(child, time.Second, "", childOutputOpts{}) })
	if !strings.Contains(out, "fatal: memory limit exceeded") {
		t.Errorf("fatal line should be kept when not synthesizing MEMLIMIT.\nOutput:\n%s", out)
	}
}

func TestIsTestOutcomeLine(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"pass (0.001s) a", "FAIL (0.001s) a", "LEAK (0.001s) a",
		"TIMEOUT (0.001s) a", "MEMLIMIT (-) <aborted>", "INCOMPLETE (-) a",
	} {
		if !isTestOutcomeLine(line) {
			t.Errorf("isTestOutcomeLine(%q) = false, want true", line)
		}
	}
	for _, line := range []string{
		"  panic: boom", "1 passed, 0 failed (0.1s)", "passing (0.1s) a", "", "=== Coverage ===",
	} {
		if isTestOutcomeLine(line) {
			t.Errorf("isTestOutcomeLine(%q) = true, want false", line)
		}
	}
}

// --- end-to-end -----------------------------------------------------------

// The false-positive guard: a test excluded for the current target is compiled
// but deliberately never run, so it must not be reported as unreported. A
// regression here would fail every target-excluded test file in the suite.
