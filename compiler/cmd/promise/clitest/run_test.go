package clitest

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestQuoteStreamMakesAStreamLegible pins the rendering contract Detail rests
// on: an absent stream is named rather than left blank, and a present one is
// indented so it cannot be misread as the harness's own output.
func TestQuoteStreamMakesAStreamLegible(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty is named", "", "<empty>"},
		{"one line is indented", "boom\n", "\n    boom"},
		{"trailing blank lines are trimmed", "boom\n\n\n", "\n    boom"},
		{"every line is indented", "a\nb\n", "\n    a\n    b"},
		{"interior blank lines are kept", "a\n\nb\n", "\n    a\n    \n    b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := quoteStream(tc.in); got != tc.want {
				t.Errorf("quoteStream(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDetailNamesTheBudgetWhenKilled: a child killed by the backstop must say
// so. Reporting a bare exit code there is what let a saturated host read as a
// bug in whatever the test was about (T2133).
func TestDetailNamesTheBudgetWhenKilled(t *testing.T) {
	killed := Result{
		Args: []string{"test", "x.pr"}, ExitCode: -1,
		TimedOut: true, Budget: DefaultBudget, Elapsed: DefaultBudget,
	}
	d := killed.Detail()
	for _, want := range []string{"KILLED", DefaultBudget.String(), "stdout:  <empty>"} {
		if !strings.Contains(d, want) {
			t.Errorf("Detail() dropped %q:\n%s", want, d)
		}
	}
	if strings.Contains(d, "code -1") {
		t.Errorf("a killed child must not be reported as an exit status:\n%s", d)
	}
}

// TestDetailReportsExitAndBothStreams is the ordinary half: the record carries
// the status and both streams, so a failing assertion never has to guess.
func TestDetailReportsExitAndBothStreams(t *testing.T) {
	r := Result{
		Args: []string{"test", "x.pr"}, ExitCode: 1,
		Stderr: "boom\n", Budget: DefaultBudget, Elapsed: 2 * time.Second,
	}
	compact := strings.Join(strings.Fields(r.Detail()), " ")
	for _, want := range []string{"exit: code 1", "stdout: <empty>", "boom"} {
		if !strings.Contains(compact, want) {
			t.Errorf("Detail() dropped %q:\n%s", want, r.Detail())
		}
	}
}

// TestCombinedKeepsEachStreamInOrder: Combined concatenates rather than
// interleaves, so a line of one stream can never be spliced into a line of the
// other — which is the property the Contains/line-anchored assertions using it
// rely on.
func TestCombinedKeepsEachStreamInOrder(t *testing.T) {
	r := Result{Stdout: "one\ntwo\n", Stderr: "err\n"}
	if got, want := r.Combined(), "one\ntwo\nerr\n"; got != want {
		t.Errorf("Combined() = %q, want %q", got, want)
	}
}

// The tests below drive Run/RunWithin against a child whose exit status,
// streams and lifetime this file chooses, by re-executing the test binary in
// helper mode (the os/exec pattern). A real compiler cannot be asked to hang, to
// exit 3, or to not exist — and those are exactly the cases the classification
// in RunWithin exists for.

// helperModeEnv puts a re-executed test binary into helper mode; helperArgEnv
// carries the case it should play.
const (
	helperModeEnv = "CLITEST_HELPER_MODE"
	helperArgEnv  = "CLITEST_HELPER_ARG"
)

// TestHelperProcess is not a test. Run normally it returns immediately; run with
// helperModeEnv set it behaves as the child a Run test needs and exits before
// the testing harness can print anything of its own.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		return
	}
	switch mode {
	case "exit":
		fmt.Fprint(os.Stdout, "on stdout\n")
		fmt.Fprint(os.Stderr, "on stderr\n")
		code, err := strconv.Atoi(os.Getenv(helperArgEnv))
		if err != nil {
			code = 99
		}
		os.Exit(code)
	case "hang":
		// Far longer than any budget a test here gives it — the deadline is what
		// ends this process, which is the thing under test — but still bounded,
		// so a kill that somehow failed leaves a stray child for a minute rather
		// than for an hour.
		time.Sleep(time.Minute)
	case "echo-env":
		fmt.Fprint(os.Stdout, os.Getenv(helperArgEnv))
	}
	os.Exit(0)
}

// helperEnv is the environment that puts the re-executed binary into one mode.
func helperEnv(mode, arg string) []string {
	return []string{helperModeEnv + "=" + mode, helperArgEnv + "=" + arg}
}

// helperArgs re-runs only TestHelperProcess in the child.
var helperArgs = []string{"-test.run=^TestHelperProcess$"}

// TestRunWithinCapturesStreamsApartAndTheStatus is the ordinary case: the child
// ran, so the record carries its status and each stream on its own.
func TestRunWithinCapturesStreamsApartAndTheStatus(t *testing.T) {
	t.Parallel()
	r := RunWithin(t, os.Args[0], helperEnv("exit", "0"), time.Minute, helperArgs...)
	if r.TimedOut {
		t.Fatalf("a child that exited was reported as killed:%s", r.Detail())
	}
	if r.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0:%s", r.ExitCode, r.Detail())
	}
	if r.Err != nil {
		t.Errorf("Err = %v, want nil for a clean exit", r.Err)
	}
	if !strings.Contains(r.Stdout, "on stdout") || strings.Contains(r.Stdout, "on stderr") {
		t.Errorf("stdout = %q, want only the child's stdout", r.Stdout)
	}
	if !strings.Contains(r.Stderr, "on stderr") || strings.Contains(r.Stderr, "on stdout") {
		t.Errorf("stderr = %q, want only the child's stderr", r.Stderr)
	}
	if r.Budget != time.Minute {
		t.Errorf("Budget = %s, want the one it was given", r.Budget)
	}
	if r.Elapsed <= 0 {
		t.Errorf("Elapsed = %s, want the time the child actually took", r.Elapsed)
	}
	if len(r.Args) == 0 || r.Args[0] != helperArgs[0] {
		t.Errorf("Args = %v, want what was run", r.Args)
	}
}

// TestRunWithinPassesAnyExitCodeThrough: the record reports the child's own
// status, not merely whether it failed. TestIncompleteFromCachedBinaryPreserves-
// ExitCode reads exactly this to assert a truncated batch propagates the test
// binary's 3.
func TestRunWithinPassesAnyExitCodeThrough(t *testing.T) {
	t.Parallel()
	for _, want := range []int{0, 1, 3} {
		r := RunWithin(t, os.Args[0], helperEnv("exit", strconv.Itoa(want)), time.Minute, helperArgs...)
		if r.ExitCode != want {
			t.Errorf("ExitCode = %d, want the child's %d:%s", r.ExitCode, want, r.Detail())
		}
		if r.TimedOut {
			t.Errorf("exit %d was reported as a timeout:%s", want, r.Detail())
		}
	}
}

// TestRunWithinKillsAndNormalizesAWedgedChild is the case T2133 turned on: a
// child that outlives its budget is killed, reported as killed, and given exit
// -1 on EVERY platform.
//
// The normalization is not cosmetic. Windows has no signals, so Kill() is
// TerminateProcess(h, 1) and the corpse genuinely exits 1 — the same code
// RunFailing accepts as "the suite reported a failure". Without this an
// assertion asking for exit 1 would be satisfied by a child that never ran
// (T2122).
//
// Measuring time is the point here rather than a stand-in for a happens-before
// edge, which docs/code-style.md §"Test synchronization" explicitly allows: this
// is a test *of* a deadline. The margin is not fine — the budget is
// milliseconds and the child sleeps an hour.
func TestRunWithinKillsAndNormalizesAWedgedChild(t *testing.T) {
	t.Parallel()
	const budget = 50 * time.Millisecond
	r := RunWithin(t, os.Args[0], helperEnv("hang", ""), budget, helperArgs...)

	if !r.TimedOut {
		t.Fatalf("the deadline did not fire:%s", r.Detail())
	}
	if r.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 — a killed child has no status of its own:%s",
			r.ExitCode, r.Detail())
	}
	if r.Elapsed < budget {
		t.Errorf("Elapsed = %s, want at least the %s budget", r.Elapsed, budget)
	}
	d := r.Detail()
	if !strings.Contains(d, "KILLED") || !strings.Contains(d, budget.String()) {
		t.Errorf("a killed child must name the budget it exceeded:\n%s", d)
	}
	if strings.Contains(d, "exit:    code") {
		t.Errorf("a killed child must not be rendered as an exit status:\n%s", d)
	}
}

// TestRunWithinReportsAChildThatNeverStarted: a binary that is not there is not
// a timeout and not an exit status — it is an error, and the record has to say
// which, or the caller reports "empty output" for a run that never happened.
func TestRunWithinReportsAChildThatNeverStarted(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "no-such-binary")
	r := RunWithin(t, missing, nil, time.Minute, "--version")
	if r.TimedOut {
		t.Errorf("a missing binary was reported as a timeout:%s", r.Detail())
	}
	if r.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 for a child that never started:%s", r.ExitCode, r.Detail())
	}
	if r.Err == nil {
		t.Errorf("Err = nil, want the exec failure:%s", r.Detail())
	}
	if d := r.Detail(); !strings.Contains(d, "stdout:  <empty>") {
		t.Errorf("Detail() must say the streams are empty rather than print blanks:\n%s", d)
	}
}

// TestRunWithinAppendsEnvToTheInheritedOne: env entries are additions, so a
// child still sees PROMISE_HOME (which SharedHome set for the whole package)
// alongside whatever the test asked for.
func TestRunWithinAppendsEnvToTheInheritedOne(t *testing.T) {
	t.Setenv("CLITEST_INHERITED", "from-the-parent")
	r := RunWithin(t, os.Args[0], helperEnv("echo-env", "chosen"), time.Minute, helperArgs...)
	if r.Stdout != "chosen" {
		t.Errorf("stdout = %q, want the env entry the caller passed", r.Stdout)
	}
	inherited := RunWithin(t, os.Args[0],
		[]string{helperModeEnv + "=echo-env", helperArgEnv + "=" + os.Getenv("CLITEST_INHERITED")},
		time.Minute, helperArgs...)
	if inherited.Stdout != "from-the-parent" {
		t.Errorf("stdout = %q, want the inherited environment to survive", inherited.Stdout)
	}
}

// TestRunUsesTheDefaultBudget: Run is RunWithin at the one budget every test
// gets unless it is testing the backstop itself.
func TestRunUsesTheDefaultBudget(t *testing.T) {
	t.Parallel()
	r := Run(t, os.Args[0], helperEnv("exit", "0"), helperArgs...)
	if r.Budget != DefaultBudget {
		t.Errorf("Budget = %s, want DefaultBudget %s", r.Budget, DefaultBudget)
	}
}

// TestRunOKAndRunFailingAcceptTheirOwnStatus pins which code each one is asking
// for. Their rejection path ends in t.Fatalf and so cannot be exercised against
// a real *testing.T; what is checked here is that neither fails the test for the
// status it exists to accept.
func TestRunOKAndRunFailingAcceptTheirOwnStatus(t *testing.T) {
	t.Parallel()
	ok := RunOK(t, os.Args[0], helperEnv("exit", "0"), helperArgs...)
	if ok.ExitCode != 0 {
		t.Errorf("RunOK returned a record with exit %d", ok.ExitCode)
	}
	failing := RunFailing(t, os.Args[0], helperEnv("exit", "1"), helperArgs...)
	if failing.ExitCode != 1 {
		t.Errorf("RunFailing returned a record with exit %d", failing.ExitCode)
	}
}

// TestRequireRanPassesAChildThatRan: the guard is transparent when the child
// finished, whatever its status — it is about the host, never about the subject.
func TestRequireRanPassesAChildThatRan(t *testing.T) {
	t.Parallel()
	r := RunWithin(t, os.Args[0], helperEnv("exit", "1"), time.Minute, helperArgs...).RequireRan(t)
	if r.ExitCode != 1 {
		t.Errorf("RequireRan changed the record: exit %d, want 1", r.ExitCode)
	}
}

// TestKilledMessageBlamesTheBackstopNotTheSubject is the wording T2133 exists
// for. Two tests used to assert a wall clock across a child's cold compile and
// report "the per-test deadline did not bound the blocked join" when a saturated
// runner blew it — naming the scheduler for a host that was merely busy. What
// replaced them says which budget fired, and says it is not evidence about the
// subject.
func TestKilledMessageBlamesTheBackstopNotTheSubject(t *testing.T) {
	t.Parallel()
	killed := Result{
		Args: []string{"test", "x.pr"}, ExitCode: -1,
		TimedOut: true, Budget: DefaultBudget, Elapsed: DefaultBudget,
	}
	msg := killed.killedMessage()
	if msg == "" {
		t.Fatal("a killed child produced no message")
	}
	for _, want := range []string{
		DefaultBudget.String(),        // which budget fired
		"backstop",                    // that a budget is what fired
		"not the behavior under test", // and that it says nothing about the subject
		"KILLED",                      // the record, so the reader can see the streams
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the killed message dropped %q:\n%s", want, msg)
		}
	}
}

// TestKilledMessageIsEmptyForAChildThatRan: the guard is transparent otherwise,
// whatever the child's status was.
func TestKilledMessageIsEmptyForAChildThatRan(t *testing.T) {
	t.Parallel()
	for _, code := range []int{0, 1, 134} {
		r := Result{ExitCode: code, Budget: DefaultBudget}
		if msg := r.killedMessage(); msg != "" {
			t.Errorf("exit %d produced a killed message:\n%s", code, msg)
		}
	}
}
