package main

import (
	"os"
	"os/exec"
	"path/filepath"

	"strings"
	"testing"
	"time"
)

// T1639: a wedged test binary used to be neither timed out nor correctly
// reported. The per-test deadline did not cover the post-test goroutine drain,
// and when the process backstop eventually fired the failure was blamed on
// compilation — which had long since finished.

// e2eHangSource does not finish within any backstop this test applies. Its
// per-test annotation is far above the process backstop, so the backstop is
// what kills it — after compilation completed, which is exactly the mislabelled
// case. It blocks rather than spinning so the test does not peg a core for the
// whole backstop budget.
const e2eHangSource = "" +
	"main() `test(expected: \"unreachable\", timeout: \"10m\") {\n" +
	"  sleep(Duration.from_millis(600000));\n" +
	"  print_line(\"unreachable\");\n" +
	"}\n"

// End-to-end: when the multi-file parent's backstop kills a child that had
// already started running its binary, the label must say so — not
// "compilation timeout".
func TestBackstopKillAttributedToRunPhase(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("spends the full backstop budget by construction")
	}
	promiseBin := locatePromiseBin(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hang_test.pr"), []byte(e2eHangSource), 0o644); err != nil {
		t.Fatal(err)
	}
	ok := "ok_one() `test {\n  assert(1 == 1, \"ok\");\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "ok_test.pr"), []byte(ok), 0o644); err != nil {
		t.Fatal(err)
	}

	// Warm the caches first, so the backstop budget below is spent running the
	// hung binary rather than compiling it (which would legitimately be a
	// compilation timeout).
	exec.Command(promiseBin, "test", "-compile-timeout", "10m", filepath.Join(dir, "ok_test.pr")).CombinedOutput()

	out, runErr := exec.Command(promiseBin, "test", "-compile-timeout", "40s", dir).CombinedOutput()
	combined := string(out)

	if runErr == nil {
		t.Fatalf("expected non-zero exit for a hung binary.\nOutput:\n%s", combined)
	}
	if strings.Contains(combined, "(compilation timeout)") {
		// On a machine slow enough that the child never finished compiling
		// within the backstop, "compilation timeout" is the correct label and
		// there is nothing for this test to attribute.
		t.Skip("child never reached the run phase within the backstop")
	}
	if !strings.Contains(combined, "test binary hung") {
		t.Errorf("expected the run-phase label.\nOutput:\n%s", combined)
	}
	// The marker is a protocol line, not user-visible output.
	if strings.Contains(combined, testPhaseRunMarker) {
		t.Errorf("phase marker leaked into the parent's rendered output.\nOutput:\n%s", combined)
	}
}

// --- unit: clampRunTimeout ------------------------------------------------

func TestClampRunTimeout(t *testing.T) {
	cfg := testTimeoutConfig{compileTimeout: 10 * time.Minute}
	now := time.Now()

	// A standalone run has no parent backstop to stay under, so an oversized
	// budget (an explicit `timeout:` annotation longer than the backstop) must
	// be left exactly as the user asked for.
	t.Setenv(testChildEnv, "")
	if got := clampRunTimeout(920*time.Second, cfg, "", now); got != 920*time.Second {
		t.Errorf("standalone budget = %s, want 920s (no backstop to clamp under)", got)
	}

	t.Setenv(testChildEnv, "1")

	// A budget comfortably under the backstop is left alone.
	if got := clampRunTimeout(30*time.Second, cfg, "", now); got != 30*time.Second {
		t.Errorf("small budget = %s, want 30s (should not be clamped)", got)
	}

	// Sum-of-per-test-timeouts above the backstop is clamped under it, so the
	// child times out first and can name the tests that never reported.
	got := clampRunTimeout(920*time.Second, cfg, "", now)
	if got >= 10*time.Minute {
		t.Errorf("oversized budget = %s, want strictly under the 10m backstop", got)
	}
	if want := 10*time.Minute - 15*time.Second; got > want {
		t.Errorf("oversized budget = %s, want <= %s (backstop minus slack)", got, want)
	}

	// Compile time already spent comes out of the remaining budget.
	late := now.Add(-9 * time.Minute)
	if got := clampRunTimeout(920*time.Second, cfg, "", late); got > 60*time.Second {
		t.Errorf("late-start budget = %s, want <= 1m of remaining backstop", got)
	}

	// Never below the floor, even when the backstop is already blown.
	blown := now.Add(-30 * time.Minute)
	if got := clampRunTimeout(920*time.Second, cfg, "", blown); got != 5*time.Second {
		t.Errorf("blown-backstop budget = %s, want the 5s floor", got)
	}

	// WASM gets the longer backstop, so its clamp is correspondingly looser.
	wasm := clampRunTimeout(20*time.Minute, cfg, "wasm32-wasi", now)
	if wasm <= 10*time.Minute {
		t.Errorf("wasm budget = %s, want more than the 10m non-wasm backstop", wasm)
	}
	if wasm >= 15*time.Minute {
		t.Errorf("wasm budget = %s, want strictly under the 15m wasm backstop", wasm)
	}
}

// --- unit: parsing the synthetic TIMEOUT (-) line -------------------------

func TestTimeoutLineMatchesSyntheticOutcome(t *testing.T) {
	t.Parallel()
	// The parent's per-test TIMEOUT regex must accept "(-)" the same way the
	// MEMLIMIT and INCOMPLETE ones do, or a batch-budget kill would go
	// uncounted.
	re := timeoutOutcomeLineRe
	if m := re.FindStringSubmatch("TIMEOUT (-) a_test"); m == nil {
		t.Error("timeout line regex did not match the synthetic TIMEOUT (-) line")
	}
	if m := re.FindStringSubmatch("TIMEOUT (2.005s) a_test"); m == nil {
		t.Error("timeout line regex no longer matches a real per-test TIMEOUT line")
	}
	if m := re.FindStringSubmatch("pass (0.001s) a_test"); m != nil {
		t.Error("timeout line regex matched a pass line")
	}
}

// The rendered batch-budget report must survive the multi-file parent's own
// regexes: the outcome line, its "  timeout:" context, and the summary.
func TestTimedOutRunReport(t *testing.T) {
	roster := []rosterEntry{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	counts := childOutcomeCounts{passed: 1}

	report := timedOutRunReport([]string{"b", "c"}, roster, counts, 32*time.Second, 32*time.Second, "")
	if !strings.HasPrefix(report, "TIMEOUT (-) b\n") {
		t.Errorf("report does not name the first unreported test:\n%s", report)
	}
	if !strings.Contains(report, "2 of 3 tests did not report: b, c") {
		t.Errorf("report does not list the unreported tests:\n%s", report)
	}
	if !strings.Contains(report, "1 passed, 0 failed, 2 timed out (32.000s)") {
		t.Errorf("report summary does not count the unreported tests:\n%s", report)
	}

	// Every test reported and the process still would not exit: the wedge is in
	// teardown, so no test is blamed — but the run is still a failure.
	all := timedOutRunReport(nil, roster, childOutcomeCounts{passed: 3}, 32*time.Second, 32*time.Second, "")
	if !strings.HasPrefix(all, "TIMEOUT (-) <teardown>\n") {
		t.Errorf("teardown wedge blamed a test:\n%s", all)
	}
	if !strings.Contains(all, "every test reported but the process did not exit") {
		t.Errorf("teardown wedge lacks its explanation:\n%s", all)
	}
	if !strings.Contains(all, "3 passed, 0 failed, 1 timed out") {
		t.Errorf("teardown wedge summary is not a failure:\n%s", all)
	}

	for _, r := range []string{report, all} {
		lines := strings.Split(strings.TrimRight(r, "\n"), "\n")
		if m := timeoutOutcomeLineRe.FindStringSubmatch(lines[0]); m == nil {
			t.Errorf("parent timeout regex does not match %q", lines[0])
		}
		if !strings.HasPrefix(lines[1], "  timeout:") {
			t.Errorf("context line %q is not attached to the outcome line", lines[1])
		}
		summaryLine := lines[len(lines)-1]
		m := parentSummaryRe.FindStringSubmatch(summaryLine)
		if m == nil {
			t.Fatalf("parent summary regex does not match %q", summaryLine)
		}
		if m[5] == "" {
			t.Errorf("summary %q has no timed-out group for the parent to total", summaryLine)
		}
	}
}

// A batch-budget kill re-renders the summary with the tests that never
// reported, so the child's own (now stale) summary must not also be printed —
// an agent reading the tail would otherwise see a passing summary above a
// failing one.
func TestBatchBudgetKillDropsStaleChildSummary(t *testing.T) {
	child := strings.Join([]string{
		"pass (0.001s) a",
		"",
		"1 passed, 0 failed",
	}, "\n")

	var counts childOutcomeCounts
	var reported map[string]bool
	out := captureStdout(t, func() {
		counts, reported, _ = printChildTestOutput(child, time.Second, "", childOutputOpts{dropSummary: true})
	})
	if strings.Contains(out, "passed,") {
		t.Errorf("stale child summary was printed:\n%s", out)
	}
	if !strings.Contains(out, "pass (0.001s) a") {
		t.Errorf("per-test lines must still be printed:\n%s", out)
	}
	// Counting is unaffected — only the rendering is suppressed.
	if counts.passed != 1 || !reported["a"] {
		t.Errorf("counts = %+v, reported = %v", counts, reported)
	}

	// Without the option the summary is rewritten and printed as before.
	out = captureStdout(t, func() {
		printChildTestOutput(child, time.Second, "", childOutputOpts{})
	})
	if !strings.Contains(out, "1 passed, 0 failed (1.000s)") {
		t.Errorf("summary missing from the normal path:\n%s", out)
	}
}

func TestBuildTestRecordsSyntheticTimeout(t *testing.T) {
	t.Parallel()
	output := strings.Join([]string{
		batchRoster("a", false, "b", false, "c", false),
		"pass (0.001s) a",
		"TIMEOUT (-) b",
		"  timeout: test process exceeded the 32s batch budget - 2 of 3 tests did not report: b, c",
		"",
		"1 passed, 0 failed, 2 timed out (32.001s)",
	}, "\n")

	recs := buildTestRecords("x.pr", output, true)
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3: %+v", len(recs), recs)
	}
	byName := map[string]testRecord{}
	for _, r := range recs {
		byName[r.Test] = r
	}
	if byName["a"].Status != "pass" {
		t.Errorf("a status = %q, want pass", byName["a"].Status)
	}
	if byName["b"].Status != "timeout" {
		t.Errorf("b status = %q, want timeout", byName["b"].Status)
	}
	if !strings.Contains(byName["b"].Context, "batch budget") {
		t.Errorf("b context = %q, want the batch-budget explanation", byName["b"].Context)
	}
	if byName["c"].Status != "not-run" {
		t.Errorf("c status = %q, want not-run", byName["c"].Status)
	}
}

func TestPhaseMarkerGatedOnEnv(t *testing.T) {
	// The marker must never appear in interactive single-file output — only
	// when the multi-file parent asks for it.
	t.Setenv(testChildEnv, "")
	if out := captureStdout(t, emitPhaseMarker); out != "" {
		t.Errorf("marker emitted without the env var: %q", out)
	}
	t.Setenv(testChildEnv, "1")
	if out := captureStdout(t, emitPhaseMarker); strings.TrimSpace(out) != testPhaseRunMarker {
		t.Errorf("marker = %q, want %q", out, testPhaseRunMarker)
	}
}

// --- unit: the summary line a truncated batch re-renders -------------------

func TestTruncatedBatchSummary(t *testing.T) {
	t.Parallel()
	// Field order is not cosmetic: parentSummaryRe reads the buckets
	// positionally, so a summary the parent cannot parse silently drops a
	// whole file's counts out of the run total.
	got := truncatedBatchSummary(
		childOutcomeCounts{passed: 4, failed: 1, leaked: 2, timedOut: 1}, 3, 5, 6)
	want := "4 passed, 1 failed, 3 skipped, 2 leaked, 6 timed out, 6 incomplete"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	m := parentSummaryRe.FindStringSubmatch(got + " (1.000s)")
	if m == nil {
		t.Fatalf("parent summary regex does not match %q", got)
	}
	// 1 passed, 2 failed, 3 skipped, 4 leaked, 5 timed out, 6 allowed leaks,
	// 7 stale allow_leaks, 8 memlimit, 9 incomplete.
	for group, want := range map[int]string{1: "4", 2: "1", 3: "3", 4: "2", 5: "6", 9: "6"} {
		if m[group] != want {
			t.Errorf("group %d = %q, want %q (in %q)", group, m[group], want, got)
		}
	}

	// Empty buckets are omitted entirely — an all-zero tail would read as
	// meaningful counts.
	if got := truncatedBatchSummary(childOutcomeCounts{passed: 2}, 0, 0, 0); got != "2 passed, 0 failed" {
		t.Errorf("clean summary = %q, want %q", got, "2 passed, 0 failed")
	}
	// A child that already reported per-test TIMEOUTs adds the synthesized ones
	// to its own count rather than replacing it.
	if got := truncatedBatchSummary(childOutcomeCounts{timedOut: 2}, 0, 3, 0); got != "0 passed, 0 failed, 5 timed out" {
		t.Errorf("timed-out buckets = %q, want them summed", got)
	}
}

// --- e2e: the batch budget as the second line of defense -------------------

// optedOutHangSource opts the wedging test out of the per-test deadline
// (timeout: "0"), so nothing in the binary stops it. The batch budget — the
// backstop below the parent's — is the only thing left, and it must name the
// test that never reported rather than dying silently.
const optedOutHangSource = "" +
	"a_ok() `test(timeout: \"5s\") {\n" +
	"  assert(1 == 1, \"runs before the wedge\");\n" +
	"}\n" +
	"\n" +
	"b_hangs_forever() `test(timeout: \"0\") {\n" +
	"  sleep(Duration.from_millis(600000));\n" +
	"}\n" +
	"\n" +
	"c_never_runs() `test(timeout: \"5s\") {\n" +
	"  assert(1 == 1, \"never reached\");\n" +
	"}\n"

func TestBatchBudgetKillNamesUnreportedTests(t *testing.T) {
	t.Parallel()
	promiseBin := locatePromiseBin(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "wedge_test.pr")
	if err := os.WriteFile(src, []byte(optedOutHangSource), 0o644); err != nil {
		t.Fatal(err)
	}

	// Under the parent's env var the child clamps its own batch budget to stay
	// below the backstop, so it — not the parent — is what kills the run, and
	// it still has the roster in hand to name the tests that never reported.
	run := func(t *testing.T, extra ...string) string {
		t.Helper()
		args := append([]string{"test", "-compile-timeout", "25s"}, extra...)
		cmd := exec.Command(promiseBin, append(args, src)...)
		cmd.Env = append(os.Environ(), testChildEnv+"=1")
		start := time.Now()
		out, err := cmd.CombinedOutput()
		combined := string(out)
		if err == nil {
			t.Fatalf("expected non-zero exit for a wedged batch.\nOutput:\n%s", combined)
		}
		if elapsed := time.Since(start); elapsed > 90*time.Second {
			t.Errorf("run took %s — the batch budget did not bound the wedge.\nOutput:\n%s", elapsed, combined)
		}
		return combined
	}

	check := func(t *testing.T, combined string) {
		t.Helper()
		if !strings.Contains(combined, "TIMEOUT (-) b_hangs_forever") {
			t.Errorf("batch-budget kill did not name the test that wedged.\nOutput:\n%s", combined)
		}
		if !strings.Contains(combined, "batch budget") {
			t.Errorf("expected the batch-budget explanation.\nOutput:\n%s", combined)
		}
		if !strings.Contains(combined, "2 of 3 tests did not report: b_hangs_forever, c_never_runs") {
			t.Errorf("expected every unreported test listed.\nOutput:\n%s", combined)
		}
		// The test that did report keeps its result...
		if !strings.Contains(combined, "pass (") || !strings.Contains(combined, "a_ok") {
			t.Errorf("expected the test that completed to still be reported.\nOutput:\n%s", combined)
		}
		// ...but the child's own summary, which counted only that test, must be
		// superseded — an agent reading the tail must not see a passing summary.
		if strings.Contains(combined, "1 passed, 0 failed (") {
			t.Errorf("stale child summary survived the batch-budget kill.\nOutput:\n%s", combined)
		}
		if !strings.Contains(combined, "1 passed, 0 failed, 2 timed out") {
			t.Errorf("expected the re-rendered summary to count the unreported tests.\nOutput:\n%s", combined)
		}
	}

	t.Run("plain", func(t *testing.T) { check(t, run(t)) })

	// The whole chain: the multi-file parent spawns the child with the env var,
	// the child clamps its budget under the parent's backstop and reports the
	// synthetic TIMEOUT, and the parent parses "(-)" as a per-test timeout
	// instead of killing the child mid-run with no output.
	t.Run("under the multi-file parent", func(t *testing.T) {
		// A second file is what puts the runner on the fan-out path — a lone
		// file is run in-process, with no child and no backstop above it.
		ok := "ok_one() `test {\n  assert(1 == 1, \"ok\");\n}\n"
		if err := os.WriteFile(filepath.Join(dir, "ok_test.pr"), []byte(ok), 0o644); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		// 60s backstop: gives each child a ≈38s run budget (60s - compile_time -
		// 15s slack), which is enough for ok_one to complete on a loaded machine.
		// With 25s the budget always clamped to the 10s floor, which was too tight
		// for the test binary to initialize and run on a heavily-loaded machine.
		out, err := exec.Command(promiseBin, "test", "-compile-timeout", "60s", dir).CombinedOutput()
		combined := string(out)
		if err == nil {
			t.Fatalf("expected non-zero exit.\nOutput:\n%s", combined)
		}
		if strings.Contains(combined, "compilation timeout") {
			// The child never got out of the compile phase within the 60s
			// backstop, so there is no clamped run budget to observe. On a
			// machine that slow the label is honest and there is nothing to
			// assert. Not an error — but not a pass for this test either.
			t.Skip("child did not reach the run phase within the backstop")
		}
		// The clamp is the point: the child must expire before the backstop,
		// which would otherwise kill it mid-run with no output at all. The
		// content assertions below are the real proof; this bound only catches
		// a clamp that stopped working entirely. With a 60s backstop the total
		// elapsed is bounded at ~60s (compile_time + clamped_run ≤ backstop).
		if elapsed := time.Since(start); elapsed > 90*time.Second {
			t.Errorf("run took %s — the child did not time out before the backstop.\nOutput:\n%s", elapsed, combined)
		}
		// The parent attributes the file, names the test, and keeps the context.
		if !strings.Contains(combined, "FAIL (") || !strings.Contains(combined, "wedge_test.pr (2 timed out)") {
			t.Errorf("parent did not report the file as timed out.\nOutput:\n%s", combined)
		}
		if !strings.Contains(combined, "b_hangs_forever") {
			t.Errorf("parent did not surface the test that wedged.\nOutput:\n%s", combined)
		}
		if !strings.Contains(combined, "batch budget") {
			t.Errorf("parent dropped the child's timeout context.\nOutput:\n%s", combined)
		}
		// Totals fold in the synthetic bucket, and the other file still ran.
		if !strings.Contains(combined, "2 passed, 0 failed, 2 timed out (2 files") {
			t.Errorf("parent totals do not count the synthetic TIMEOUT.\nOutput:\n%s", combined)
		}
		if !strings.Contains(combined, "pass (") || !strings.Contains(combined, "ok_test.pr") {
			t.Errorf("the healthy file was not run.\nOutput:\n%s", combined)
		}
		// The handoff marker is protocol between parent and child — it must not
		// reach the reader, on either the passing or the failing file.
		if strings.Contains(combined, testPhaseRunMarker) {
			t.Errorf("phase marker leaked into rendered output.\nOutput:\n%s", combined)
		}
	})

	t.Run("coverage", func(t *testing.T) {
		combined := run(t, "-coverage")
		check(t, combined)
		// Coverage from a truncated run is meaningless and is not printed —
		// publishing a percentage computed from a batch that never finished
		// would read as a real measurement.
		if strings.Contains(combined, "Coverage:") {
			t.Errorf("coverage report printed for a truncated run.\nOutput:\n%s", combined)
		}
	})
}

// --- e2e: stuck-goroutine wording and precedence ---------------------------

func TestTimedOutRunReportAccountsForExcludedTests(t *testing.T) {
	roster := []rosterEntry{
		{Name: "a"},
		{Name: "skipped_here", Excluded: true},
		{Name: "b"},
		{Name: "also_skipped", Excluded: true},
		{Name: "c"},
	}
	// a reported; the excluded pair never will; b and c are the real casualties.
	report := timedOutRunReport([]string{"b", "c"}, roster, childOutcomeCounts{passed: 1},
		32*time.Second, 32*time.Second, "")

	if !strings.Contains(report, "2 of 3 tests did not report: b, c") {
		t.Errorf("excluded tests counted as eligible:\n%s", report)
	}
	if !strings.Contains(report, "1 passed, 0 failed, 2 skipped, 2 timed out") {
		t.Errorf("summary does not carry the skipped bucket:\n%s", report)
	}
	m := parentSummaryRe.FindStringSubmatch(strings.TrimSpace(report[strings.LastIndex(report, "\n1 "):]))
	if m == nil {
		t.Fatalf("parent summary regex does not match the rendered summary:\n%s", report)
	}
	if m[3] != "2" {
		t.Errorf("skipped group = %q, want 2", m[3])
	}
}

// A wedge early in a large file leaves dozens of tests unreported. The context
// line lists at most 8 and says how many more, so the tail of the output stays
// readable — the same cap the INCOMPLETE path uses.
func TestTimedOutRunReportCapsTheMissingList(t *testing.T) {
	var roster []rosterEntry
	var missing []string
	for _, n := range []string{"t01", "t02", "t03", "t04", "t05", "t06", "t07", "t08", "t09", "t10"} {
		roster = append(roster, rosterEntry{Name: n})
		missing = append(missing, n)
	}

	report := timedOutRunReport(missing, roster, childOutcomeCounts{}, time.Minute, time.Minute, "")
	if !strings.Contains(report, "10 of 10 tests did not report: t01, t02, t03, t04, t05, t06, t07, t08, ... and 2 more") {
		t.Errorf("missing list is not capped at 8 with a remainder:\n%s", report)
	}
	// The first unreported test still names the outcome line — that is the one
	// that wedged the process.
	if !strings.HasPrefix(report, "TIMEOUT (-) t01\n") {
		t.Errorf("outcome line does not name the first unreported test:\n%s", report)
	}
	if !strings.Contains(report, "0 passed, 0 failed, 10 timed out") {
		t.Errorf("summary does not count every unreported test:\n%s", report)
	}
}

// The teardown form blames no test, so the gate must see each test's own real
// result and no phantom record for the "<teardown>" placeholder. The runner's
// human-facing exit code carries the failure instead (docs/gate-system.md).
func TestBuildTestRecordsTeardownTimeoutBlamesNoTest(t *testing.T) {
	t.Parallel()
	output := strings.Join([]string{
		batchRoster("a", false, "b", false),
		"pass (0.001s) a",
		"FAIL (0.002s) b",
		"  panic: assertion failed",
		"TIMEOUT (-) <teardown>",
		"  timeout: every test reported but the process did not exit within the 32s batch budget",
		"",
		"1 passed, 1 failed, 1 timed out (32.001s)",
	}, "\n")

	recs := buildTestRecords("x.pr", output, true)
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (no phantom <teardown> test): %+v", len(recs), recs)
	}
	if recs[0].Test != "a" || recs[0].Status != "pass" {
		t.Errorf("record 0 = %+v, want a/pass", recs[0])
	}
	if recs[1].Test != "b" || recs[1].Status != "fail" {
		t.Errorf("record 1 = %+v, want b/fail", recs[1])
	}
	if !strings.Contains(recs[1].Context, "assertion failed") {
		t.Errorf("b lost its panic context: %q", recs[1].Context)
	}
}

// The parent renders its own file-level lines into the same stream the gate
// parses. A synthetic TIMEOUT naming a .pr file is a file outcome, not a test,
// and must never become a test record — the same guard every other outcome
// regex carries.
func TestSyntheticTimeoutIgnoresFileLevelLines(t *testing.T) {
	t.Parallel()
	_, seen, _, _, _ := parseChildOutput(strings.Join([]string{
		"TIMEOUT (-) modules/http/http_test.pr",
		"TIMEOUT (-) a_real_test",
	}, "\n"))

	if _, ok := seen["modules/http/http_test.pr"]; ok {
		t.Error("a file-level TIMEOUT line was recorded as a test result")
	}
	if r, ok := seen["a_real_test"]; !ok || r.status != "timeout" {
		t.Errorf("test-level synthetic TIMEOUT not recorded: %+v", seen)
	}
}
