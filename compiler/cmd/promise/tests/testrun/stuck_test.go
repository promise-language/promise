package testrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// T1639: a wedged test binary used to be neither timed out nor correctly
// reported. The per-test deadline did not cover the post-test goroutine drain,
// and when the process backstop eventually fired the failure was blamed on
// compilation — which had long since finished.
// stuckGoroutineSource is a batch-test file whose first test returns
// immediately while leaving one goroutine parked on a channel nobody will ever
// send to. Before the fix this wedged the drain spin-wait forever.
const stuckGoroutineSource = "" +
	"a_leaves_blocked_goroutine() `test(timeout: \"2s\") {\n" +
	"  channel[int] never = channel[int](capacity: 0);\n" +
	"  go { int? v = <-never; };\n" +
	"  assert(true, \"body completes immediately\");\n" +
	"}\n" +
	"\n" +
	"b_follows_on() `test(timeout: \"2s\") {\n" +
	"  channel[int] c = channel[int](capacity: 1);\n" +
	"  go { c.send(7); };\n" +
	"  int? v = <-c;\n" +
	"  assert(v! == 7, \"follow-on test still runs\");\n" +
	"}\n"

// End-to-end: the drain deadline fires at the per-test budget, names the test,
// explains that the body returned but a goroutine did not exit, and — via each
// test's own pre-test outstanding snapshot — lets the next test run normally.
func TestStuckGoroutineReportsNamedTimeout(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "stuck_test.pr")
	if err := os.WriteFile(src, []byte(stuckGoroutineSource), 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	out, runErr := exec.Command(promiseBin, "test", "-timeout", "2s", src).CombinedOutput()
	elapsed := time.Since(start)
	combined := string(out)

	if runErr == nil {
		t.Fatalf("expected non-zero exit for a timed-out test.\nOutput:\n%s", combined)
	}
	// The whole point: this used to cost the full 10-minute compile backstop.
	// The budget here is compile + 2s drain + slack, not a timing assertion on
	// the deadline itself.
	if elapsed > 3*time.Minute {
		t.Errorf("run took %s — the drain deadline did not bound the wedge.\nOutput:\n%s", elapsed, combined)
	}
	if !strings.Contains(combined, "TIMEOUT (") {
		t.Errorf("expected a TIMEOUT outcome.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "a_leaves_blocked_goroutine") {
		t.Errorf("expected the wedging test to be named.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "goroutine did not exit within") {
		t.Errorf("expected the stuck-goroutine TIMEOUT context.\nOutput:\n%s", combined)
	}
	// The pre-test snapshot: the second test must not be charged for the
	// first test's abandoned goroutine.
	if !strings.Contains(combined, "pass (") || !strings.Contains(combined, "b_follows_on") {
		t.Errorf("expected the follow-on test to still run and pass.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "1 timed out") {
		t.Errorf("expected a summary line with a timed-out bucket.\nOutput:\n%s", combined)
	}
}

// End-to-end: when the multi-file parent's backstop kills a child that had
// already started running its binary, the label must say so — not
// "compilation timeout".
// stuckVariantsSource covers the two shapes the singular case in
// TestStuckGoroutineReportsNamedTimeout does not: more than one abandoned
// goroutine (plural wording, counted against the running baseline), and a test
// that both failed and abandoned a goroutine (FAIL wins over TIMEOUT).
const stuckVariantsSource = "" +
	"a_abandons_two() `test(timeout: \"2s\") {\n" +
	"  channel[int] never = channel[int](capacity: 0);\n" +
	"  go { int? v = <-never; };\n" +
	"  go { int? v = <-never; };\n" +
	"  assert(true, \"body completes immediately\");\n" +
	"}\n" +
	"\n" +
	"b_fails_and_abandons() `test(timeout: \"2s\") {\n" +
	"  channel[int] never = channel[int](capacity: 0);\n" +
	"  go { int? v = <-never; };\n" +
	"  assert(1 == 2, \"deliberate failure\");\n" +
	"}\n"

func TestStuckGoroutineWordingAndFailPrecedence(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "stuck_variants_test.pr")
	if err := os.WriteFile(src, []byte(stuckVariantsSource), 0o644); err != nil {
		t.Fatal(err)
	}

	out, runErr := exec.Command(promiseBin, "test", "-timeout", "2s", src).CombinedOutput()
	combined := string(out)
	if runErr == nil {
		t.Fatalf("expected non-zero exit.\nOutput:\n%s", combined)
	}

	// Plural wording, and the count is this test's own goroutines.
	if !strings.Contains(combined, "2 goroutines did not exit within") {
		t.Errorf("expected plural stuck-goroutine wording with a count of 2.\nOutput:\n%s", combined)
	}
	// The TIMEOUT is reported against the drain deadline, not the body's
	// (near-zero) run time.
	if m := regexp.MustCompile(`TIMEOUT \((\d+\.\d+)s\) a_abandons_two`).FindStringSubmatch(combined); m == nil {
		t.Errorf("expected a TIMEOUT line for a_abandons_two.\nOutput:\n%s", combined)
	} else if secs, _ := strconv.ParseFloat(m[1], 64); secs < 1.5 {
		t.Errorf("TIMEOUT elapsed = %ss, want ~the 2s drain budget — the body-only time is being reported", m[1])
	}

	// A test that failed *and* abandoned a goroutine stays a FAIL: the
	// assertion message is the actionable diagnostic.
	if !strings.Contains(combined, "FAIL (") || !strings.Contains(combined, "b_fails_and_abandons") {
		t.Errorf("expected b_fails_and_abandons to be reported.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "deliberate failure") {
		t.Errorf("expected the assertion message on the FAIL path.\nOutput:\n%s", combined)
	}
	if regexp.MustCompile(`TIMEOUT \([\d.-]+\S*\) b_fails_and_abandons`).MatchString(combined) {
		t.Errorf("a failing test was reclassified as a TIMEOUT.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "0 passed, 1 failed, 1 timed out") {
		t.Errorf("expected one failure and one timeout in the summary.\nOutput:\n%s", combined)
	}
}

// A roster with target-excluded tests must not count them as "did not report":
// they were compiled and deliberately not run, so folding them into the missing
// tally would blame the wedge on tests that never had a chance to report.
// timedOutBaselineSource is the regression that made every wedge cascade: a
// test that times out with its body (and its goroutine) still live never
// accounts for the goroutines it created. With an absolute created==completed
// drain, every later test would then wait out its own full deadline for
// goroutines that were never theirs — one wedge turning the whole file into a
// sequence of timeouts. Each test's own pre-test snapshot absorbs the deficit.
const timedOutBaselineSource = "" +
	"a_times_out_holding_a_goroutine() `test(timeout: \"1s\") {\n" +
	"  channel[int] never = channel[int](capacity: 0);\n" +
	"  go { int? v = <-never; };\n" +
	"  sleep(Duration.from_millis(60000));\n" +
	"}\n" +
	"\n" +
	"b_uses_goroutines() `test(timeout: \"10s\") {\n" +
	"  channel[int] c = channel[int](capacity: 1);\n" +
	"  go { c.send(7); };\n" +
	"  int? v = <-c;\n" +
	"  assert(v! == 7, \"follow-on test still runs\");\n" +
	"}\n" +
	"\n" +
	"c_uses_goroutines() `test(timeout: \"10s\") {\n" +
	"  channel[int] c = channel[int](capacity: 1);\n" +
	"  go { c.send(9); };\n" +
	"  int? v = <-c;\n" +
	"  assert(v! == 9, \"and so does the one after it\");\n" +
	"}\n"

func TestTimedOutTestDoesNotStallLaterDrains(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "baseline_test.pr")
	if err := os.WriteFile(src, []byte(timedOutBaselineSource), 0o644); err != nil {
		t.Fatal(err)
	}

	out, runErr := exec.Command(promiseBin, "test", src).CombinedOutput()
	combined := string(out)
	if runErr == nil {
		t.Fatalf("expected non-zero exit for the timed-out test.\nOutput:\n%s", combined)
	}

	if !regexp.MustCompile(`TIMEOUT \([\d.]+s\) a_times_out_holding_a_goroutine`).MatchString(combined) {
		t.Errorf("expected the first test to be reported as a per-test TIMEOUT.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "1 passed, 0 failed, 2 timed out") &&
		!strings.Contains(combined, "2 passed, 0 failed, 1 timed out") {
		t.Errorf("unexpected summary shape.\nOutput:\n%s", combined)
	}
	// The point: both later tests pass, and neither is charged for the
	// abandoned goroutine.
	for _, name := range []string{"b_uses_goroutines", "c_uses_goroutines"} {
		if !regexp.MustCompile(`pass \([\d.]+s\) ` + name).MatchString(combined) {
			t.Errorf("%s did not pass — the abandoned goroutine stalled its drain.\nOutput:\n%s", name, combined)
		}
		if regexp.MustCompile(`(?m)^(TIMEOUT|LEAK) \([\d.-]+\S*\) ` + name).MatchString(combined) {
			t.Errorf("%s inherited the earlier test's deficit.\nOutput:\n%s", name, combined)
		}
	}
	if strings.Contains(combined, "goroutine did not exit within") ||
		strings.Contains(combined, "goroutines did not exit within") {
		t.Errorf("a later test was blamed for the abandoned goroutine.\nOutput:\n%s", combined)
	}
}
