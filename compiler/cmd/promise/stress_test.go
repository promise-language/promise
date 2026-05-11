package main

import (
	"fmt"
	"math"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// ---------------------------------------------------------------------------
// extractCrashReason
// ---------------------------------------------------------------------------

func TestExtractCrashReason_PanicOnStderr(t *testing.T) {
	reason := extractCrashReason("", "panic: assertion failed: expected 3, got 4\ngoroutine 1:\n", nil)
	if reason != "panic: assertion failed: expected 3, got 4" {
		t.Errorf("got %q", reason)
	}
}

func TestExtractCrashReason_PanicOnStdout(t *testing.T) {
	reason := extractCrashReason("PASS (0.001s) test_foo\npanic: bad thing\n", "", nil)
	if reason != "panic: bad thing" {
		t.Errorf("got %q", reason)
	}
}

func TestExtractCrashReason_PreferStderr(t *testing.T) {
	reason := extractCrashReason("panic: stdout msg\n", "panic: stderr msg\n", nil)
	if reason != "panic: stderr msg" {
		t.Errorf("expected stderr panic to be preferred, got %q", reason)
	}
}

func TestExtractCrashReason_LongPanicTruncated(t *testing.T) {
	longMsg := "panic: " + strings.Repeat("x", 200)
	reason := extractCrashReason("", longMsg+"\n", nil)
	if len(reason) > 120 {
		t.Errorf("expected truncation, got len=%d", len(reason))
	}
	if !strings.HasSuffix(reason, "...") {
		t.Errorf("expected ... suffix, got %q", reason)
	}
}

func TestExtractCrashReason_FatalError(t *testing.T) {
	reason := extractCrashReason("", "fatal error: runtime panic\n", nil)
	if reason != "fatal error: runtime panic" {
		t.Errorf("got %q", reason)
	}
}

func TestExtractCrashReason_NoContext(t *testing.T) {
	reason := extractCrashReason("", "", nil)
	if reason != "crash" {
		t.Errorf("got %q", reason)
	}
}

func TestExtractCrashReason_NoPanicWithOutput(t *testing.T) {
	reason := extractCrashReason("some output\n", "some stderr\n", nil)
	if reason != "crash" {
		t.Errorf("got %q", reason)
	}
}

func TestExtractCrashReason_ExitCodeFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal tests require Unix")
	}
	// Run a process that exits with code 42 — no panic, no signal
	cmd := exec.Command("sh", "-c", "exit 42")
	err := cmd.Run()
	reason := extractCrashReason("", "", err)
	if reason != "exit code 42" {
		t.Errorf("got %q", reason)
	}
}

func TestExtractCrashReason_SignalOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal tests require Unix")
	}
	// Run a process that kills itself with SIGSEGV — no panic output
	cmd := exec.Command("sh", "-c", "kill -SEGV $$")
	err := cmd.Run()
	reason := extractCrashReason("", "", err)
	if reason != "SIGSEGV" {
		t.Errorf("got %q", reason)
	}
}

func TestExtractCrashReason_PanicPlusSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal tests require Unix")
	}
	cmd := exec.Command("sh", "-c", "kill -ABRT $$")
	err := cmd.Run()
	reason := extractCrashReason("", "panic: null pointer\n", err)
	if !strings.Contains(reason, "panic: null pointer") {
		t.Errorf("expected panic message, got %q", reason)
	}
	if !strings.Contains(reason, "SIGABRT") {
		t.Errorf("expected signal name, got %q", reason)
	}
}

// ---------------------------------------------------------------------------
// extractSignal
// ---------------------------------------------------------------------------

func TestExtractSignal_NilError(t *testing.T) {
	if sig := extractSignal(nil); sig != "" {
		t.Errorf("got %q", sig)
	}
}

func TestExtractSignal_NonExitError(t *testing.T) {
	if sig := extractSignal(fmt.Errorf("random")); sig != "" {
		t.Errorf("got %q", sig)
	}
}

func TestExtractSignal_NormalExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal tests require Unix")
	}
	cmd := exec.Command("sh", "-c", "exit 1")
	err := cmd.Run()
	if sig := extractSignal(err); sig != "" {
		t.Errorf("normal exit should have no signal, got %q", sig)
	}
}

func TestExtractSignal_RealSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal tests require Unix")
	}
	cmd := exec.Command("sh", "-c", "kill -SEGV $$")
	err := cmd.Run()
	if sig := extractSignal(err); sig != "SIGSEGV" {
		t.Errorf("got %q", sig)
	}
}

// ---------------------------------------------------------------------------
// signalName
// ---------------------------------------------------------------------------

func TestSignalName_Known(t *testing.T) {
	tests := []struct {
		sig  syscall.Signal
		want string
	}{
		{syscall.SIGSEGV, "SIGSEGV"},
		{syscall.SIGABRT, "SIGABRT"},
		{syscall.SIGBUS, "SIGBUS"},
		{syscall.SIGFPE, "SIGFPE"},
		{syscall.SIGILL, "SIGILL"},
		{syscall.SIGKILL, "SIGKILL"},
		{syscall.SIGTRAP, "SIGTRAP"},
	}
	for _, tt := range tests {
		if got := signalName(tt.sig); got != tt.want {
			t.Errorf("signalName(%v) = %q, want %q", tt.sig, got, tt.want)
		}
	}
}

func TestSignalName_Unknown(t *testing.T) {
	// Signal 63 is not in the known list on any platform
	got := signalName(syscall.Signal(63))
	if !strings.Contains(got, "63") {
		t.Errorf("expected signal number in fallback, got %q", got)
	}
	if !strings.HasPrefix(got, "signal ") {
		t.Errorf("expected 'signal N (desc)' format, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// extractExitCode
// ---------------------------------------------------------------------------

func TestExtractExitCode_NilError(t *testing.T) {
	if code := extractExitCode(nil); code != -1 {
		t.Errorf("got %d", code)
	}
}

func TestExtractExitCode_NonExitError(t *testing.T) {
	if code := extractExitCode(fmt.Errorf("random")); code != -1 {
		t.Errorf("got %d", code)
	}
}

func TestExtractExitCode_RealExitError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exit code 42 test requires Unix sh")
	}
	cmd := exec.Command("sh", "-c", "exit 42")
	err := cmd.Run()
	if code := extractExitCode(err); code != 42 {
		t.Errorf("got %d", code)
	}
}

func TestExtractExitCode_SignalKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal tests require Unix")
	}
	cmd := exec.Command("sh", "-c", "kill -KILL $$")
	err := cmd.Run()
	// Signal-killed processes return -1 from ExitCode()
	if code := extractExitCode(err); code != -1 {
		t.Errorf("expected -1 for signal kill, got %d", code)
	}
}

// ---------------------------------------------------------------------------
// buildCrashContext
// ---------------------------------------------------------------------------

func TestBuildCrashContext_EmptyStderr(t *testing.T) {
	ctx := buildCrashContext("", nil)
	if ctx != "crash (no context available)" {
		t.Errorf("got %q", ctx)
	}
}

func TestBuildCrashContext_WithStderr(t *testing.T) {
	ctx := buildCrashContext("panic: bad thing\ngoroutine 1 [running]:\nmain.foo()\n", nil)
	if !strings.Contains(ctx, "stderr:") {
		t.Errorf("expected stderr section, got %q", ctx)
	}
	if !strings.Contains(ctx, "panic: bad thing") {
		t.Errorf("expected panic message, got %q", ctx)
	}
	if !strings.Contains(ctx, "goroutine 1") {
		t.Errorf("expected stack trace, got %q", ctx)
	}
}

func TestBuildCrashContext_WithExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exit code test requires Unix sh")
	}
	cmd := exec.Command("sh", "-c", "exit 2")
	err := cmd.Run()
	ctx := buildCrashContext("some error output\n", err)
	if !strings.Contains(ctx, "exit code: 2") {
		t.Errorf("expected exit code, got %q", ctx)
	}
	if !strings.Contains(ctx, "stderr:") {
		t.Errorf("expected stderr, got %q", ctx)
	}
}

func TestBuildCrashContext_WithSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal tests require Unix")
	}
	cmd := exec.Command("sh", "-c", "kill -SEGV $$")
	err := cmd.Run()
	ctx := buildCrashContext("panic: segfault\n", err)
	if !strings.Contains(ctx, "signal: SIGSEGV") {
		t.Errorf("expected signal info, got %q", ctx)
	}
	if !strings.Contains(ctx, "stderr:") {
		t.Errorf("expected stderr, got %q", ctx)
	}
	// Signal-killed → exit code is -1 → no exit code line
	if strings.Contains(ctx, "exit code:") {
		t.Errorf("signal kill should not show exit code, got %q", ctx)
	}
}

// ---------------------------------------------------------------------------
// lastNLines
// ---------------------------------------------------------------------------

func TestLastNLines_Basic(t *testing.T) {
	result := lastNLines("line1\nline2\nline3\nline4\nline5\n", 3)
	if result != "line3\nline4\nline5" {
		t.Errorf("got %q", result)
	}
}

func TestLastNLines_FewLines(t *testing.T) {
	result := lastNLines("line1\nline2\n", 10)
	if result != "line1\nline2" {
		t.Errorf("got %q", result)
	}
}

func TestLastNLines_Empty(t *testing.T) {
	if r := lastNLines("", 5); r != "" {
		t.Errorf("got %q", r)
	}
	if r := lastNLines("\n\n\n", 5); r != "" {
		t.Errorf("got %q for whitespace-only", r)
	}
}

func TestLastNLines_SkipsEmptyLines(t *testing.T) {
	result := lastNLines("line1\n\n\nline2\n\nline3\n", 2)
	if result != "line2\nline3" {
		t.Errorf("got %q", result)
	}
}

func TestLastNLines_ExactCount(t *testing.T) {
	result := lastNLines("a\nb\nc\n", 3)
	if result != "a\nb\nc" {
		t.Errorf("got %q", result)
	}
}

func TestLastNLines_SingleLine(t *testing.T) {
	result := lastNLines("only\n", 5)
	if result != "only" {
		t.Errorf("got %q", result)
	}
}

// ---------------------------------------------------------------------------
// indent
// ---------------------------------------------------------------------------

func TestIndent_Basic(t *testing.T) {
	result := indent("line1\nline2\nline3", "  ")
	if result != "  line1\n  line2\n  line3" {
		t.Errorf("got %q", result)
	}
}

func TestIndent_SingleLine(t *testing.T) {
	result := indent("hello", ">>> ")
	if result != ">>> hello" {
		t.Errorf("got %q", result)
	}
}

// ---------------------------------------------------------------------------
// testStats methods
// ---------------------------------------------------------------------------

func TestTestStats_Total(t *testing.T) {
	st := &testStats{passes: 3, fails: 2}
	if st.total() != 5 {
		t.Errorf("got %d", st.total())
	}
}

func TestTestStats_PassRate(t *testing.T) {
	st := &testStats{passes: 3, fails: 1}
	if got := st.passRate(); got != 0.75 {
		t.Errorf("got %f", got)
	}
}

func TestTestStats_PassRate_ZeroTotal(t *testing.T) {
	st := &testStats{}
	if got := st.passRate(); got != 1.0 {
		t.Errorf("zero total should return 1.0, got %f", got)
	}
}

func TestTestStats_Mean(t *testing.T) {
	st := &testStats{timings: []float64{1.0, 2.0, 3.0}}
	if got := st.mean(); got != 2.0 {
		t.Errorf("got %f", got)
	}
}

func TestTestStats_Mean_Empty(t *testing.T) {
	st := &testStats{}
	if got := st.mean(); got != 0 {
		t.Errorf("got %f", got)
	}
}

func TestTestStats_Stddev(t *testing.T) {
	// Values: 2, 4, 4, 4, 5, 5, 7, 9 → mean=5, stddev=2
	st := &testStats{timings: []float64{2, 4, 4, 4, 5, 5, 7, 9}}
	got := st.stddev()
	if math.Abs(got-2.0) > 0.01 {
		t.Errorf("expected ~2.0, got %f", got)
	}
}

func TestTestStats_Stddev_SingleTiming(t *testing.T) {
	st := &testStats{timings: []float64{5.0}}
	if got := st.stddev(); got != 0 {
		t.Errorf("single timing should have stddev 0, got %f", got)
	}
}

func TestTestStats_Stddev_Empty(t *testing.T) {
	st := &testStats{}
	if got := st.stddev(); got != 0 {
		t.Errorf("got %f", got)
	}
}

func TestTestStats_Cov(t *testing.T) {
	// mean=1.0, stddev=0.5 → cov=0.5
	st := &testStats{timings: []float64{0.5, 1.5}}
	got := st.cov()
	if math.Abs(got-0.5) > 0.01 {
		t.Errorf("expected ~0.5, got %f", got)
	}
}

func TestTestStats_Cov_ZeroMean(t *testing.T) {
	st := &testStats{}
	if got := st.cov(); got != 0 {
		t.Errorf("got %f", got)
	}
}

func TestTestStats_MinTime(t *testing.T) {
	st := &testStats{timings: []float64{3.0, 1.0, 2.0}}
	if got := st.minTime(); got != 1.0 {
		t.Errorf("got %f", got)
	}
}

func TestTestStats_MinTime_Empty(t *testing.T) {
	st := &testStats{}
	if got := st.minTime(); got != 0 {
		t.Errorf("got %f", got)
	}
}

func TestTestStats_MaxTime(t *testing.T) {
	st := &testStats{timings: []float64{1.0, 3.0, 2.0}}
	if got := st.maxTime(); got != 3.0 {
		t.Errorf("got %f", got)
	}
}

func TestTestStats_MaxTime_Empty(t *testing.T) {
	st := &testStats{}
	if got := st.maxTime(); got != 0 {
		t.Errorf("got %f", got)
	}
}

func TestTestStats_IsHighVariance(t *testing.T) {
	// Needs: total >= 5, mean > 0.005, cov > 1.0
	st := &testStats{
		passes:  5,
		timings: []float64{0.001, 0.001, 0.001, 0.001, 0.1},
	}
	if !st.isHighVariance() {
		t.Error("expected high variance")
	}
}

func TestTestStats_IsHighVariance_TooFewRuns(t *testing.T) {
	st := &testStats{
		passes:  3,
		timings: []float64{0.001, 0.001, 0.1},
	}
	if st.isHighVariance() {
		t.Error("should not be high variance with < 5 runs")
	}
}

func TestTestStats_IsHighVariance_LowMean(t *testing.T) {
	// Sub-millisecond tests should not flag as high variance
	st := &testStats{
		passes:  5,
		timings: []float64{0.001, 0.002, 0.003, 0.004, 0.005},
	}
	if st.isHighVariance() {
		t.Error("sub-millisecond tests should not be high variance")
	}
}

func TestTestStats_IsHighVariance_LowCoV(t *testing.T) {
	// Consistent timings → CoV < 1.0
	st := &testStats{
		passes:  5,
		timings: []float64{0.01, 0.01, 0.01, 0.01, 0.01},
	}
	if st.isHighVariance() {
		t.Error("consistent timings should not be high variance")
	}
}

// ---------------------------------------------------------------------------
// fileStats methods
// ---------------------------------------------------------------------------

func makeFileStats(tests ...testStats) *fileStats {
	stats := make(map[string]*testStats)
	var order []string
	for i := range tests {
		stats[tests[i].name] = &tests[i]
		order = append(order, tests[i].name)
	}
	return &fileStats{path: "test.pr", stats: stats, testOrder: order, interval: 1}
}

func TestFileStats_HasFailures(t *testing.T) {
	fs := makeFileStats(
		testStats{name: "a", passes: 5},
		testStats{name: "b", passes: 3, fails: 1},
	)
	if !fs.hasFailures() {
		t.Error("expected true")
	}
}

func TestFileStats_HasFailures_AllPass(t *testing.T) {
	fs := makeFileStats(
		testStats{name: "a", passes: 5},
		testStats{name: "b", passes: 3},
	)
	if fs.hasFailures() {
		t.Error("expected false")
	}
}

func TestFileStats_HasHighVariance(t *testing.T) {
	fs := makeFileStats(
		testStats{name: "a", passes: 5, timings: []float64{0.001, 0.001, 0.001, 0.001, 0.1}},
	)
	if !fs.hasHighVariance() {
		t.Error("expected true")
	}
}

func TestFileStats_RecalcInterval(t *testing.T) {
	// With failures: always 1
	fs := makeFileStats(testStats{name: "a", passes: 5, fails: 1})
	fs.runs = 100
	fs.recalcInterval()
	if fs.interval != 1 {
		t.Errorf("failing file should have interval 1, got %d", fs.interval)
	}

	// No failures, runs < 20: interval 1
	fs2 := makeFileStats(testStats{name: "a", passes: 10})
	fs2.runs = 10
	fs2.recalcInterval()
	if fs2.interval != 1 {
		t.Errorf("< 20 runs should have interval 1, got %d", fs2.interval)
	}

	// No failures, runs 20-49: interval 2
	fs2.runs = 30
	fs2.recalcInterval()
	if fs2.interval != 2 {
		t.Errorf("30 runs should have interval 2, got %d", fs2.interval)
	}

	// No failures, runs 50-99: interval 4
	fs2.runs = 75
	fs2.recalcInterval()
	if fs2.interval != 4 {
		t.Errorf("75 runs should have interval 4, got %d", fs2.interval)
	}

	// No failures, runs >= 100: interval 8
	fs2.runs = 200
	fs2.recalcInterval()
	if fs2.interval != 8 {
		t.Errorf("200 runs should have interval 8, got %d", fs2.interval)
	}
}

// ---------------------------------------------------------------------------
// collectTestsByCategory
// ---------------------------------------------------------------------------

func TestCollectTestsByCategory(t *testing.T) {
	files := []*fileStats{
		makeFileStats(
			testStats{name: "flaky1", passes: 5, fails: 2},
			testStats{name: "stable1", passes: 10},
		),
		makeFileStats(
			testStats{name: "highvar", passes: 5, timings: []float64{0.001, 0.001, 0.001, 0.001, 0.1}},
			testStats{name: "stable2", passes: 8},
		),
	}
	flaky, highVar, stable := collectTestsByCategory(files)
	if len(flaky) != 1 || flaky[0].name != "flaky1" {
		t.Errorf("flaky: got %d items", len(flaky))
	}
	if len(highVar) != 1 || highVar[0].name != "highvar" {
		t.Errorf("highVar: got %d items", len(highVar))
	}
	if len(stable) != 2 {
		t.Errorf("stable: got %d items, want 2", len(stable))
	}
}

func TestCollectTestsByCategory_HighVarSort(t *testing.T) {
	// Two high-variance tests: sorted by highest CoV first.
	// isHighVariance requires: total >= 5, mean > 0.005, cov > 1.0
	files := []*fileStats{
		makeFileStats(
			// Lower CoV (~1.2): mean=0.024, spike at 0.08
			testStats{name: "lowcov", passes: 5, timings: []float64{0.01, 0.01, 0.01, 0.01, 0.08}},
			// Higher CoV (~1.9): mean=0.021, spike at 0.1
			testStats{name: "highcov", passes: 5, timings: []float64{0.001, 0.001, 0.001, 0.001, 0.1}},
		),
	}
	_, highVar, _ := collectTestsByCategory(files)
	if len(highVar) != 2 {
		t.Fatalf("expected 2 high-var, got %d (lowcov: mean=%.4f cov=%.2f hv=%v, highcov: mean=%.4f cov=%.2f hv=%v)",
			len(highVar),
			files[0].stats["lowcov"].mean(), files[0].stats["lowcov"].cov(), files[0].stats["lowcov"].isHighVariance(),
			files[0].stats["highcov"].mean(), files[0].stats["highcov"].cov(), files[0].stats["highcov"].isHighVariance())
	}
	if highVar[0].name != "highcov" {
		t.Errorf("first should be highest CoV, got %q (CoV %.2f)", highVar[0].name, highVar[0].cov())
	}
}

func TestCollectTestsByCategory_SortOrder(t *testing.T) {
	files := []*fileStats{
		makeFileStats(
			testStats{name: "better", passes: 8, fails: 2},   // 80% pass
			testStats{name: "worse", passes: 3, fails: 7},    // 30% pass
			testStats{name: "middling", passes: 5, fails: 5}, // 50% pass
		),
	}
	flaky, _, _ := collectTestsByCategory(files)
	if len(flaky) != 3 {
		t.Fatalf("expected 3 flaky, got %d", len(flaky))
	}
	// Sorted by worst pass rate first
	if flaky[0].name != "worse" {
		t.Errorf("first should be worst pass rate, got %q", flaky[0].name)
	}
	if flaky[2].name != "better" {
		t.Errorf("last should be best pass rate, got %q", flaky[2].name)
	}
}

// ---------------------------------------------------------------------------
// failSummary
// ---------------------------------------------------------------------------

func TestFailSummary_NoFails(t *testing.T) {
	st := &testStats{passes: 5}
	if got := st.failSummary(); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestFailSummary_TimeoutOnly(t *testing.T) {
	st := &testStats{passes: 3, fails: 2, timeouts: 2, lastErr: "timeout"}
	summary := st.failSummary()
	if !strings.Contains(summary, "2 timeout") {
		t.Errorf("expected timeout count, got %q", summary)
	}
	if strings.Contains(summary, "fail") {
		t.Errorf("should not mention fail when all failures are timeouts, got %q", summary)
	}
	if !strings.Contains(summary, "(timeout)") {
		t.Errorf("expected (timeout) suffix, got %q", summary)
	}
}

func TestFailSummary_MixedTimeoutAndFail(t *testing.T) {
	st := &testStats{passes: 5, fails: 3, timeouts: 1, lastErr: "SIGSEGV"}
	summary := st.failSummary()
	if !strings.Contains(summary, "1 timeout") {
		t.Errorf("expected timeout count, got %q", summary)
	}
	if !strings.Contains(summary, "2 fail") {
		t.Errorf("expected fail count, got %q", summary)
	}
	if !strings.Contains(summary, "SIGSEGV") {
		t.Errorf("expected crash reason, got %q", summary)
	}
}

func TestFailSummary_TestFailed(t *testing.T) {
	st := &testStats{passes: 8, fails: 1, lastErr: "test failed"}
	summary := st.failSummary()
	if !strings.Contains(summary, "(test failed)") {
		t.Errorf("expected (test failed), got %q", summary)
	}
}

func TestFailSummary_CrashReason(t *testing.T) {
	st := &testStats{passes: 8, fails: 2, lastErr: "panic: null pointer (SIGSEGV)"}
	summary := st.failSummary()
	if !strings.Contains(summary, "last: panic: null pointer") {
		t.Errorf("expected last: with crash reason, got %q", summary)
	}
}

// ---------------------------------------------------------------------------
// failReason
// ---------------------------------------------------------------------------

func TestFailReason_EmptyOutput(t *testing.T) {
	if got := failReason("expected stuff", ""); got != "no output" {
		t.Errorf("got %q", got)
	}
}

func TestFailReason_LineDiff(t *testing.T) {
	got := failReason("hello\nworld", "hello\nearth")
	if !strings.Contains(got, "line 2") {
		t.Errorf("expected line 2, got %q", got)
	}
	if !strings.Contains(got, "earth") {
		t.Errorf("expected actual content, got %q", got)
	}
}

func TestFailReason_LineDiff_LongLine(t *testing.T) {
	long := strings.Repeat("x", 100)
	got := failReason("short", long)
	if !strings.Contains(got, "...") {
		t.Errorf("expected truncation, got %q", got)
	}
}

func TestFailReason_DifferentLineCount(t *testing.T) {
	got := failReason("a\nb\nc", "a\nb")
	if !strings.Contains(got, "expected 3 lines, got 2") {
		t.Errorf("got %q", got)
	}
}

func TestFailReason_IdenticalContent(t *testing.T) {
	// Lines match but something is different (shouldn't happen, but edge case)
	got := failReason("same", "same")
	if got != "output mismatch" {
		t.Errorf("got %q", got)
	}
}

// ---------------------------------------------------------------------------
// fmtDuration
// ---------------------------------------------------------------------------

func TestFmtDuration(t *testing.T) {
	tests := []struct {
		input float64
		want  string
	}{
		{0, "0ms"},
		{0.0001, "100μs"},
		{0.0005, "500μs"},
		{0.001, "1.0ms"},
		{0.050, "50.0ms"},
		{0.999, "999.0ms"},
		{1.0, "1.000s"},
		{1.5, "1.500s"},
		{60.123, "60.123s"},
	}
	for _, tt := range tests {
		got := fmtDuration(tt.input)
		if got != tt.want {
			t.Errorf("fmtDuration(%f) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// commonDir
// ---------------------------------------------------------------------------

func TestCommonDir_SingleFile(t *testing.T) {
	if got := commonDir([]string{"/a/b/c.pr"}); got != "" {
		t.Errorf("single file should return empty, got %q", got)
	}
}

func TestCommonDir_Empty(t *testing.T) {
	if got := commonDir(nil); got != "" {
		t.Errorf("nil should return empty, got %q", got)
	}
}

func TestCommonDir_SameDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		got := commonDir([]string{`C:\tmp\tests\a.pr`, `C:\tmp\tests\b.pr`})
		if got != `C:\tmp\tests` {
			t.Errorf("got %q", got)
		}
	} else {
		got := commonDir([]string{"/tmp/tests/a.pr", "/tmp/tests/b.pr"})
		if got != "/tmp/tests" {
			t.Errorf("got %q", got)
		}
	}
}

func TestCommonDir_DifferentDirs(t *testing.T) {
	if runtime.GOOS == "windows" {
		got := commonDir([]string{`C:\tmp\tests\e2e\a.pr`, `C:\tmp\tests\std\b.pr`})
		if got != `C:\tmp\tests` {
			t.Errorf("got %q", got)
		}
	} else {
		got := commonDir([]string{"/tmp/tests/e2e/a.pr", "/tmp/tests/std/b.pr"})
		if got != "/tmp/tests" {
			t.Errorf("got %q", got)
		}
	}
}

func TestCommonDir_NoCommonPrefix(t *testing.T) {
	if runtime.GOOS == "windows" {
		got := commonDir([]string{`C:\home\user\a.pr`, `C:\opt\tests\b.pr`})
		if got != `C:\` {
			t.Errorf("got %q", got)
		}
	} else {
		got := commonDir([]string{"/home/user/a.pr", "/opt/tests/b.pr"})
		if got != "/" {
			t.Errorf("got %q", got)
		}
	}
}

// ---------------------------------------------------------------------------
// buildStressReport
// ---------------------------------------------------------------------------

func makeFileStatsWithPath(path string, tests ...testStats) *fileStats {
	stats := make(map[string]*testStats)
	var order []string
	for i := range tests {
		tests[i].file = path
		stats[tests[i].name] = &tests[i]
		order = append(order, tests[i].name)
	}
	return &fileStats{path: path, stats: stats, testOrder: order, interval: 1}
}

func TestBuildStressReport_AllStable(t *testing.T) {
	files := []*fileStats{
		makeFileStatsWithPath("e2e/foo.pr",
			testStats{name: "test_a", passes: 10},
			testStats{name: "test_b", passes: 10},
		),
	}
	report := buildStressReport(50, 30*1e9, files, 2, "linux-x86_64")
	if !strings.Contains(report, "ALL STABLE: 2 tests") {
		t.Errorf("expected ALL STABLE line, got:\n%s", report)
	}
	if strings.Contains(report, "FLAKY") {
		t.Errorf("unexpected FLAKY section:\n%s", report)
	}
	if strings.Contains(report, "HIGH VARIANCE") {
		t.Errorf("unexpected HIGH VARIANCE section:\n%s", report)
	}
}

func TestBuildStressReport_HighVarianceSummary(t *testing.T) {
	// isHighVariance requires: total >= 5, mean > 0.005s, cov > 1.0
	// Use timings with a spike: 4 × 0.001s + 1 × 0.1s → mean≈0.021, high CoV
	files := []*fileStats{
		makeFileStatsWithPath("concurrency/a.pr",
			testStats{name: "test_noisy", passes: 5, timings: []float64{0.001, 0.001, 0.001, 0.001, 0.1}},
		),
	}
	report := buildStressReport(50, 30*1e9, files, 1, "linux-x86_64")

	// Must show HIGH VARIANCE summary line with count
	if !strings.Contains(report, "HIGH VARIANCE: 1 test") {
		t.Errorf("expected HIGH VARIANCE summary line, got:\n%s", report)
	}
	// Must note no failures
	if !strings.Contains(report, "no failures") {
		t.Errorf("expected 'no failures' in HIGH VARIANCE line, got:\n%s", report)
	}
	// Must NOT list individual test details
	if strings.Contains(report, "test_noisy") {
		t.Errorf("high-variance tests should not be listed individually, got:\n%s", report)
	}
	// Must NOT show FLAKY or ALL STABLE
	if strings.Contains(report, "FLAKY") {
		t.Errorf("unexpected FLAKY section:\n%s", report)
	}
	if strings.Contains(report, "ALL STABLE") {
		t.Errorf("unexpected ALL STABLE line:\n%s", report)
	}
}

func TestBuildStressReport_FlakyTests(t *testing.T) {
	files := []*fileStats{
		makeFileStatsWithPath("e2e/bar.pr",
			testStats{name: "test_flaky", passes: 8, fails: 2},
		),
	}
	report := buildStressReport(10, 5*1e9, files, 1, "linux-x86_64")

	if !strings.Contains(report, "FLAKY (1 tests)") {
		t.Errorf("expected FLAKY header, got:\n%s", report)
	}
	// Flaky tests must still be listed individually
	if !strings.Contains(report, "test_flaky") {
		t.Errorf("flaky test name should appear in report, got:\n%s", report)
	}
	if strings.Contains(report, "ALL STABLE") {
		t.Errorf("unexpected ALL STABLE line:\n%s", report)
	}
}

func TestBuildStressReport_FlakyAndHighVariance(t *testing.T) {
	files := []*fileStats{
		makeFileStatsWithPath("e2e/baz.pr",
			testStats{name: "test_broken", passes: 7, fails: 3},
			testStats{name: "test_noisy", passes: 5, timings: []float64{0.001, 0.001, 0.001, 0.001, 0.1}},
		),
	}
	report := buildStressReport(20, 10*1e9, files, 2, "linux-x86_64")

	// Both sections must appear
	if !strings.Contains(report, "FLAKY") {
		t.Errorf("expected FLAKY section, got:\n%s", report)
	}
	if !strings.Contains(report, "HIGH VARIANCE: 1 test") {
		t.Errorf("expected HIGH VARIANCE summary, got:\n%s", report)
	}
	// Flaky test listed, high-variance test not listed
	if !strings.Contains(report, "test_broken") {
		t.Errorf("flaky test should appear, got:\n%s", report)
	}
	if strings.Contains(report, "test_noisy") {
		t.Errorf("high-variance test should not be listed individually, got:\n%s", report)
	}
}
