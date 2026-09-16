package common

import "testing"

// The summary line is the whole contract between `promise check` and the gate,
// so it is pinned byte-for-byte here: a change to either side that the other
// does not follow shows up as a failure rather than as a metric quietly
// reporting nothing.
func TestParseCheckSummaryLine(t *testing.T) {
	out := `FAIL modules/std (18 errors)
modules/std/iter.pr:9:34: cannot move-capture borrowed parameter 'pred' into a lambda

851 checked, 1 warned, 9 failed (861 units, 27 errors, 1 warning, 42.173s)
FAILED:
  modules/std (18 errors)`

	s := ParseCheckSummaryLine(out)
	if s == nil {
		t.Fatal("the summary line was not found")
	}
	want := CheckSummary{Checked: 851, Warned: 1, Failed: 9, Units: 861, Errors: 27, Warnings: 1}
	if *s != want {
		t.Errorf("summary = %+v, want %+v", *s, want)
	}
}

// Singular and plural spellings are both the real command's output — one error
// and one warning read "1 error, 1 warning".
func TestParseCheckSummaryLineSingularCounts(t *testing.T) {
	s := ParseCheckSummaryLine("4 checked, 0 warned, 1 failed (5 units, 1 error, 1 warning, 0.412s)")
	if s == nil {
		t.Fatal("the summary line was not found")
	}
	want := CheckSummary{Checked: 4, Warned: 0, Failed: 1, Units: 5, Errors: 1, Warnings: 1}
	if *s != want {
		t.Errorf("summary = %+v, want %+v", *s, want)
	}
}

// A run that printed no summary measured nothing, and the caller must be able
// to tell that from a clean zero — so this is nil, never a zero-valued summary.
func TestParseCheckSummaryLineAbsent(t *testing.T) {
	for _, out := range []string{
		"",
		"ok modules/io\n",
		"851 checked, 1 warned (861 units)", // not the summary's shape
	} {
		if s := ParseCheckSummaryLine(out); s != nil {
			t.Errorf("ParseCheckSummaryLine(%q) = %+v, want nil", out, *s)
		}
	}
}

// The subject is spelled once. checked:promise and tested:promise measure the
// same code, so a target added for one is added for both — this is what keeps
// that true.
func TestPromiseCheckArgsUseTheSuiteTargets(t *testing.T) {
	args := promiseCheckArgs()
	if len(args) == 0 || args[0] != "check" {
		t.Fatalf("args = %v, want it to start with \"check\"", args)
	}
	targets := promiseSuiteTargets()
	if len(args)-1 != len(targets) {
		t.Fatalf("args = %v, want %d targets from promiseSuiteTargets", args, len(targets))
	}
	for i, target := range targets {
		if args[i+1] != target {
			t.Errorf("args[%d] = %q, want %q", i+1, args[i+1], target)
		}
	}
}
