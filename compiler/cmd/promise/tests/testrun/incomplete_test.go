package testrun

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// incompleteSource is a batch-test file whose middle test exits the process
// with status 0 — the exact failure mode T1415 describes. The raw extern hits
// the PAL bridge directly, so no catalog module import is needed.
const incompleteSource = "_exit_now(int code) `extern(\"promise_os_exit\");\n" + `
a_first() ` + "`test" + ` {
  assert(1 + 1 == 2, "sanity");
}

b_dies() ` + "`test" + ` {
  _exit_now(0);
}

c_after() ` + "`test" + ` {
  assert(2 + 2 == 4, "sanity2");
}
`

// End-to-end: the single-file runner must report INCOMPLETE, name every test
// that never reported, print a summary, and exit non-zero.
func TestIncompleteHarnessReportsFailure(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "incomplete_test.pr")
	if err := os.WriteFile(src, []byte(incompleteSource), 0o644); err != nil {
		t.Fatal(err)
	}

	out, runErr := exec.Command(promiseBin, "test", src).CombinedOutput()
	combined := string(out)

	if runErr == nil {
		t.Fatalf("expected non-zero exit for a truncated run.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "INCOMPLETE") {
		t.Errorf("expected an INCOMPLETE outcome.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "b_dies") {
		t.Errorf("expected the killing test 'b_dies' to be named.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "c_after") {
		t.Errorf("expected the un-run test 'c_after' to be listed.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, " incomplete (") {
		t.Errorf("expected a summary line with an incomplete bucket.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "pass (") {
		t.Errorf("expected the completed test to still be reported.\nOutput:\n%s", combined)
	}
}

// End-to-end: the multi-file parent must classify the file as FAIL with the
// incomplete count, name it in the FAILED: section, and exit non-zero.
func TestIncompleteMultiFileReportsFailure(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "incomplete_test.pr"), []byte(incompleteSource), 0o644); err != nil {
		t.Fatal(err)
	}
	ok := "ok_one() `test {\n  assert(1 == 1, \"ok\");\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "ok_test.pr"), []byte(ok), 0o644); err != nil {
		t.Fatal(err)
	}

	out, runErr := exec.Command(promiseBin, "test", dir).CombinedOutput()
	combined := string(out)

	if runErr == nil {
		t.Fatalf("expected non-zero exit for a truncated run.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "(2 incomplete)") {
		t.Errorf("expected the file line to report '(2 incomplete)'.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "FAILED:") || !strings.Contains(combined, "incomplete_test.pr: b_dies") {
		t.Errorf("expected the file+test in the FAILED: section.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, ", 2 incomplete (2 files") {
		t.Errorf("expected the grand summary to carry the incomplete bucket.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "pass (") || !strings.Contains(combined, "ok_test.pr") {
		t.Errorf("expected the healthy file to still pass.\nOutput:\n%s", combined)
	}
}

// --- roster construction -------------------------------------------------
//
// The roster is what the completeness check reconciles against, so a wrongly
// un-excluded entry turns every target-excluded test in the suite into a false
// INCOMPLETE. These lock the exclusion semantics per roster kind.

func TestIncompleteNotReportedForExcludedTests(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "excluded_test.pr")
	// Excluded on every supported target, so the assertion never runs anywhere.
	// Multiple targets combine with `||` — a comma starts the next NAMED annotation
	// parameter, so `exclude: macos, linux, …` would exclude macOS alone and let the
	// assertion run everywhere else (T1449).
	source := "a_first() `test {\n  assert(1 + 1 == 2, \"sanity\");\n}\n\n" +
		"b_skipped() `test(exclude: macos || linux || windows || wasm) {\n  assert(false, \"must not run\");\n}\n\n" +
		"c_after() `test {\n  assert(2 + 2 == 4, \"sanity2\");\n}\n"
	if err := os.WriteFile(src, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	out, runErr := exec.Command(promiseBin, "test", src).CombinedOutput()
	combined := string(out)
	if runErr != nil {
		t.Fatalf("excluded tests must not trip the completeness check: %v\nOutput:\n%s", runErr, combined)
	}
	if strings.Contains(combined, "INCOMPLETE") {
		t.Errorf("excluded test reported as INCOMPLETE.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "2 passed, 0 failed, 1 skipped") {
		t.Errorf("expected 2 passed / 1 skipped.\nOutput:\n%s", combined)
	}
}

// A non-zero child status is preserved as the runner's exit code, and the
// second (cache-hit) run must reach the same verdict — the cached-binary path
// builds its roster from the cache meta rather than from a fresh frontend.
func TestIncompleteFromCachedBinaryPreservesExitCode(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "exit3_test.pr")
	source := strings.Replace(incompleteSource, "_exit_now(0)", "_exit_now(3)", 1)
	if err := os.WriteFile(src, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, run := range []string{"compiled", "cached"} {
		cmd := exec.Command(promiseBin, "test", src)
		out, runErr := cmd.CombinedOutput()
		combined := string(out)

		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			t.Fatalf("%s run: expected a non-zero exit, got %v.\nOutput:\n%s", run, runErr, combined)
		}
		if exitErr.ExitCode() != 3 {
			t.Errorf("%s run: exit code = %d, want the child's 3.\nOutput:\n%s",
				run, exitErr.ExitCode(), combined)
		}
		if !strings.Contains(combined, "INCOMPLETE (-) b_dies") {
			t.Errorf("%s run: expected INCOMPLETE naming b_dies.\nOutput:\n%s", run, combined)
		}
		if !strings.Contains(combined, "status 3") {
			t.Errorf("%s run: expected the child's exit status in the context line.\nOutput:\n%s",
				run, combined)
		}
	}
}

// Coverage mode runs a different runner (runTestBinaryWithCoverage) with its own
// output path — it must reach the same verdict, and must not print a coverage
// report for a run whose tests never finished.
func TestIncompleteCoverageModeReportsFailure(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "incomplete_test.pr")
	if err := os.WriteFile(src, []byte(incompleteSource), 0o644); err != nil {
		t.Fatal(err)
	}

	out, runErr := exec.Command(promiseBin, "test", "-coverage", src).CombinedOutput()
	combined := string(out)

	if runErr == nil {
		t.Fatalf("expected non-zero exit in coverage mode.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "INCOMPLETE (-) b_dies") || !strings.Contains(combined, "c_after") {
		t.Errorf("expected INCOMPLETE naming b_dies and listing c_after.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, " incomplete (") {
		t.Errorf("expected a summary line with an incomplete bucket.\nOutput:\n%s", combined)
	}
	if strings.Contains(combined, "=== Coverage ===") {
		t.Errorf("coverage from a truncated run should not be reported.\nOutput:\n%s", combined)
	}
}

// Stress mode re-runs a compiled binary directly, so it reconciles against its
// own roster (fs.testOrder). A child that exits 0 mid-batch must be attributed
// to the test that was running, not silently counted as a clean iteration.
func TestIncompleteStressAttribution(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "incomplete_test.pr")
	if err := os.WriteFile(src, []byte(incompleteSource), 0o644); err != nil {
		t.Fatal(err)
	}

	out, runErr := exec.Command(promiseBin, "test", "-stress", "2", src).CombinedOutput()
	combined := string(out)

	if runErr == nil {
		t.Fatalf("expected non-zero exit from stress mode.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "b_dies") {
		t.Errorf("expected the killing test to be named.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "process exited before reporting a result") {
		t.Errorf("expected the exit-0 truncation reason.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "FLAKY") {
		t.Errorf("expected the failure in the stress report.\nOutput:\n%s", combined)
	}
}
