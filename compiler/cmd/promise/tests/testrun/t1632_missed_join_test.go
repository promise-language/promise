package testrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// T1632 replaced sleep-as-synchronization with a completion-channel join at
// roughly sixty sites, every one of which now ends in `_ := <-done;`. That trade
// is only safe because a *missed* signal is bounded: the sweep swapped a test
// that silently passed for one that blocks, and the harness has to turn that
// block into a named failure rather than a wedged suite.
//
// stuck_test.go covers the mirror image — the body returns and a *goroutine*
// stays parked, which reports "goroutine did not exit within". Nothing covered
// the body itself parking on a receive, which is the shape the sweep can
// actually introduce and which must report the plain per-test deadline instead.
//
// The fixture is the exact hazard docs/code-style.md warns about: an error
// handler inside the `go` block returns early without signalling, so the join
// hangs on precisely the path the test was written to exercise.
const missedJoinSignalSource = "" +
	"t1632_fails!() int {\n" +
	"  raise error(message: \"expected failure\");\n" +
	"}\n" +
	"\n" +
	"a_precedes() `test(timeout: \"2s\") {\n" +
	"  assert(1 + 1 == 2, \"runs before the wedged test\");\n" +
	"}\n" +
	"\n" +
	"b_missed_join_signal() `test(timeout: \"2s\") {\n" +
	"  channel[bool] done = channel[bool](capacity: 1);\n" +
	"  go {\n" +
	"    int v = t1632_fails()? e {\n" +
	"      return; // the bug: this early exit never signals\n" +
	"    };\n" +
	"    done.send(true);\n" +
	"  };\n" +
	"  _ := <-done; // the goroutine has already exited; nobody will ever send\n" +
	"}\n" +
	"\n" +
	"c_follows_on() `test(timeout: \"2s\") {\n" +
	"  channel[int] c = channel[int](capacity: 1);\n" +
	"  go { c.send(7); };\n" +
	"  int? v = <-c;\n" +
	"  assert(v! == 7, \"follow-on test still runs\");\n" +
	"}\n"

func TestMissedJoinSignalReportsNamedTimeout(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "missed_join_test.pr")
	if err := os.WriteFile(src, []byte(missedJoinSignalSource), 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	// -progress full: the follow-on test's `pass` line is asserted below, and
	// pass lines are suppressed by default when stdout is a pipe (T1888).
	out, runErr := exec.Command(promiseBin, "test", "-progress", "full", "-timeout", "2s", src).CombinedOutput()
	elapsed := time.Since(start)
	combined := string(out)

	if runErr == nil {
		t.Fatalf("expected non-zero exit for a test wedged on its own join.\nOutput:\n%s", combined)
	}
	// A blocked receive must cost the per-test budget, not the process backstop.
	// Compile plus a 2s deadline plus slack — not an assertion on the deadline.
	if elapsed > 3*time.Minute {
		t.Errorf("run took %s — the per-test deadline did not bound the blocked join.\nOutput:\n%s", elapsed, combined)
	}
	if !strings.Contains(combined, "TIMEOUT (") {
		t.Errorf("expected a TIMEOUT outcome.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "b_missed_join_signal") {
		t.Errorf("expected the wedged test to be named.\nOutput:\n%s", combined)
	}
	// The blocked party is the test body, so the context must be the plain
	// deadline. Reporting the abandoned-goroutine wording here would send the
	// reader hunting a stray goroutine instead of the missing `done.send`.
	if !strings.Contains(combined, "timeout: exceeded 2s limit") {
		t.Errorf("expected the plain per-test deadline context.\nOutput:\n%s", combined)
	}
	if strings.Contains(combined, "goroutine did not exit within") {
		t.Errorf("body-parked wedge must not be reported as an abandoned goroutine.\nOutput:\n%s", combined)
	}
	// A wedged join must not swallow the rest of the batch: the tests either
	// side of it still run, so one missing signal costs one test, not a file.
	if !strings.Contains(combined, "a_precedes") || !strings.Contains(combined, "c_follows_on") {
		t.Errorf("expected the neighbouring tests to still run.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "2 passed") || !strings.Contains(combined, "1 timed out") {
		t.Errorf("expected a 2-passed/1-timed-out summary.\nOutput:\n%s", combined)
	}
}
