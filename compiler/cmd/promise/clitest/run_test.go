package clitest

import (
	"flag"
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
		TimedOut: true, Budget: MinBudget, Elapsed: MinBudget,
	}
	d := killed.Detail()
	for _, want := range []string{"KILLED", MinBudget.String(), "stdout:  <empty>"} {
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
		Stderr: "boom\n", Budget: MinBudget, Elapsed: 2 * time.Second,
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
	case "pwd":
		dir, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "getwd: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprint(os.Stdout, dir)
	}
	os.Exit(0)
}

// helperEnv is the environment that puts the re-executed binary into one mode.
func helperEnv(mode, arg string) []string {
	return []string{helperModeEnv + "=" + mode, helperArgEnv + "=" + arg}
}

// helperArgs re-runs only TestHelperProcess in the child.
var helperArgs = []string{"-test.run=^TestHelperProcess$"}

// guardModeEnv selects which of the guards' FAILING paths the re-executed binary
// should drive. Those paths end in t.Fatal, so they cannot be exercised against
// this test's own *testing.T — the only way to watch one fire is to be a
// different process from the one it fails.
const guardModeEnv = "CLITEST_GUARD_MODE"

// guardArgs re-runs only TestGuardHelperProcess in the child.
var guardArgs = []string{"-test.run=^TestGuardHelperProcess$", "-test.v"}

// TestGuardHelperProcess is not a test either. Run normally it returns
// immediately; run with guardModeEnv set it drives one guard's failing path
// against a REAL *testing.T and lets the harness report it, which is what the
// parent below re-executes it to observe. It must not os.Exit — the whole point
// is that the testing harness gets to print the failure.
func TestGuardHelperProcess(t *testing.T) {
	switch os.Getenv(guardModeEnv) {
	case "":
		return
	case "require-ran":
		// A killed record, as a saturated host produces. RequireRan must end the
		// test here rather than hand this back to an assertion that would read
		// the empty streams as a broken subject.
		Result{
			Args: []string{"test", "x.pr"}, ExitCode: -1, TimedOut: true,
			Budget: MaxBudget, Deadline: 30 * time.Minute, Elapsed: MaxBudget,
		}.RequireRan(t)
		t.Error("RequireRan returned for a killed child")
	case "exit-mismatch":
		// The rejection runExpectingExit still owns after T2207: a child that
		// really ran and reported a status nobody asked for.
		RunFailing(t, os.Args[0], helperEnv("exit", "0"), helperArgs...)
		t.Error("RunFailing returned for a child that exited 0")
	}
}

// TestGuardsEndTheTestAndSayWhichOneFired is the end-to-end half of T2207's
// deliverable. The wording tests pin what killedMessage BUILDS; this pins that
// it actually reaches the reader — that the guard fails the test, and that a
// budget kill and an ordinary status mismatch are told apart at the top of the
// report rather than both reading as "expected exit 1, got -1".
//
// RequireRan and runExpectingExit still read 75%/80% in this package's coverage
// profile, and that is an artifact rather than a gap: their last statement runs
// in the re-executed CHILD, whose lines no profile of the parent can attribute.
// Breaking either t.Fatal reddens this test.
func TestGuardsEndTheTestAndSayWhichOneFired(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode     string
		want     []string // must appear in the failing child's output
		unwanted []string
	}{{
		mode: "require-ran",
		want: []string{
			"FAIL",                        // the guard ended the test
			"backstop",                    // naming the budget, not the subject
			"not the behavior under test", //
			"raise -timeout to raise it",  // and the lever (T2207)
		},
		// The headline that sent readers at the wrong subsystem.
		unwanted: []string{"expected exit"},
	}, {
		mode: "exit-mismatch",
		want: []string{"FAIL", "expected exit 1, got 0"},
		// An ordinary mismatch is not a host problem and must not claim to be.
		unwanted: []string{"backstop"},
	}} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			r := RunWithin(t, os.Args[0], []string{guardModeEnv + "=" + tc.mode},
				time.Minute, guardArgs...)
			if r.TimedOut {
				t.Fatalf("the guard helper did not finish:%s", r.Detail())
			}
			if r.ExitCode == 0 {
				t.Fatalf("the %s guard did not fail its test:%s", tc.mode, r.Detail())
			}
			out := r.Combined()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("the %s guard's report dropped %q:%s", tc.mode, want, r.Detail())
				}
			}
			for _, unwanted := range tc.unwanted {
				if strings.Contains(out, unwanted) {
					t.Errorf("the %s guard's report claimed %q:%s", tc.mode, unwanted, r.Detail())
				}
			}
		})
	}
}

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

// TestRunBoundedRunsTheChildWhereItWasTold: the working directory is what every
// Env method and GitRun carry through runBounded — a fixture repo, a project
// directory — so a child started in the wrong one would build the wrong tree
// while still passing every assertion about its output.
func TestRunBoundedRunsTheChildWhereItWasTold(t *testing.T) {
	t.Parallel()
	// TempDir rather than t.TempDir: a live process holding this as its current
	// directory is one of the things Windows refuses to unlink, and the handle
	// can outlive the exit by the same moment an image section does (T2157).
	dir := TempDir(t)
	r := runBounded(child{
		bin: os.Args[0], dir: dir, args: append(helperArgs, ""),
		env:    append(os.Environ(), helperEnv("pwd", "")...),
		budget: time.Minute,
	})
	if r.TimedOut || r.ExitCode != 0 {
		t.Fatalf("the helper did not run:%s", r.Detail())
	}
	// macOS hands out /var/folders symlinked from /private/var, so compare the
	// resolved paths rather than the strings the two sides happened to spell.
	got, err := filepath.EvalSymlinks(strings.TrimSpace(r.Stdout))
	if err != nil {
		t.Fatalf("resolve the child's cwd %q: %v", r.Stdout, err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("the child ran in %s, want the dir it was given %s", got, want)
	}
}

// TestRunBoundedInheritsTheEnvironmentWhenGivenNone: a nil env means "inherit",
// which is what RunWithin passes on for a caller that added nothing. Setting
// cmd.Env to a nil slice is exactly that in os/exec, and getting it wrong would
// hand children an EMPTY environment — no PATH, no PROMISE_HOME — which fails in
// ways that look like the compiler rather than like the harness.
func TestRunBoundedInheritsTheEnvironmentWhenGivenNone(t *testing.T) {
	t.Setenv("CLITEST_INHERIT_PROBE", "inherited")
	r := runBounded(child{
		bin: os.Args[0], args: helperArgs, env: nil, budget: time.Minute,
	})
	if r.TimedOut || r.ExitCode != 0 {
		t.Fatalf("the helper did not run:%s", r.Detail())
	}
	// The helper is in no mode (the env carries no mode), so it returns quietly;
	// what matters is that it found a usable environment to start in at all.
	if r.Err != nil {
		t.Errorf("a nil env must inherit, not empty the environment: %v:%s", r.Err, r.Detail())
	}
}

// TestDetailNamesOnlyWhatTheCallerAdded: Result.Env is the entries a caller
// passed, never the whole inherited environment runBounded actually gave the
// child. Wiring it to the latter would put a few hundred lines — and whatever
// secrets the host carries — into every failure message in the suite.
func TestDetailNamesOnlyWhatTheCallerAdded(t *testing.T) {
	t.Setenv("CLITEST_SECRET_PROBE", "must-not-be-printed")
	r := RunWithin(t, os.Args[0], helperEnv("exit", "0"), time.Minute, helperArgs...)
	if len(r.Env) != 2 || !strings.HasPrefix(r.Env[0], helperModeEnv+"=") {
		t.Errorf("Env = %v, want only the two entries the caller added", r.Env)
	}
	if d := r.Detail(); strings.Contains(d, "CLITEST_SECRET_PROBE") {
		t.Errorf("Detail() printed the inherited environment:\n%s", d)
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

// TestBudgetWithinIsAShareOfTheRunsOwnDeadline pins the derivation T2207
// replaced a fixed three minutes with. Each row is a way a suite is actually
// run, and what the backstop must be worth under it.
func TestBudgetWithinIsAShareOfTheRunsOwnDeadline(t *testing.T) {
	for _, tc := range []struct {
		name            string
		deadline, spent time.Duration
		want            time.Duration
	}{
		// goTestArgs: bin/test, bin/verify, tested:go and the coverage gate.
		{"a 30m deadline caps at the ceiling", 30 * time.Minute, 0, MaxBudget},
		// A hand-typed `go test ./...` — the command T2207 was measured under.
		{"go test's own 10m default halves to 5m", 10 * time.Minute, 0, 5 * time.Minute},
		// `go test -timeout 0`: nothing to pre-empt, so the ceiling is the bound.
		{"no deadline leaves only the ceiling", 0, 0, MaxBudget},
		{"a negative deadline is read as none", -time.Second, 0, MaxBudget},
		// What is LEFT is what is halved, so a late test cannot be promised time
		// the package no longer has.
		{"time already spent is not promised twice", 30 * time.Minute, 20 * time.Minute, 5 * time.Minute},
		// Never below what every invocation got before T2207.
		{"a nearly spent deadline still gets the floor", 30 * time.Minute, 29 * time.Minute, MinBudget},
		{"an overrun deadline still gets the floor", 10 * time.Minute, 11 * time.Minute, MinBudget},
		{"a tiny deadline still gets the floor", time.Second, 0, MinBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := budgetWithin(tc.deadline, tc.spent); got != tc.want {
				t.Errorf("budgetWithin(%s, %s) = %s, want %s", tc.deadline, tc.spent, got, tc.want)
			}
		})
	}
}

// TestBudgetWithinNeverGrowsAsTheDeadlineIsSpent pins the property
// TestBudgetReadsThisRunsDeadline brackets on, and the one that makes the
// derivation safe: a budget handed out later in a package can never be larger
// than one handed out earlier under the same deadline, so no late invocation can
// be promised time the package has already spent.
func TestBudgetWithinNeverGrowsAsTheDeadlineIsSpent(t *testing.T) {
	const deadline = 30 * time.Minute
	prev := budgetWithin(deadline, 0)
	for spent := time.Duration(0); spent <= deadline+time.Minute; spent += 30 * time.Second {
		got := budgetWithin(deadline, spent)
		if got > prev {
			t.Fatalf("budgetWithin(%s, %s) = %s, grew from %s", deadline, spent, got, prev)
		}
		if got < MinBudget || got > MaxBudget {
			t.Fatalf("budgetWithin(%s, %s) = %s, outside [%s, %s]",
				deadline, spent, got, MinBudget, MaxBudget)
		}
		prev = got
	}
}

// stringValue is a flag.Value that is also a flag.Getter, but of the wrong type
// — what -test.timeout would look like if the testing package ever redefined it
// as something other than a duration.
type stringValue string

func (s *stringValue) String() string     { return string(*s) }
func (s *stringValue) Set(v string) error { *s = stringValue(v); return nil }
func (s *stringValue) Get() any           { return string(*s) }

// plainValue is a flag.Value that is NOT a flag.Getter at all.
type plainValue struct{}

func (plainValue) String() string   { return "" }
func (plainValue) Set(string) error { return nil }

// TestDeadlineFromDeclinesToGuessWhatItCannotRead covers the three ways there is
// no deadline to read. Each answers 0, which budgetWithin turns into the ceiling
// — declining is right there, because inventing a duration would silently size
// every backstop in the package off a number nobody set.
func TestDeadlineFromDeclinesToGuessWhatItCannotRead(t *testing.T) {
	d := 30 * time.Minute
	dur := (*durationValue)(&d)
	str := stringValue("30m")

	for _, tc := range []struct {
		name string
		flag *flag.Flag
		want time.Duration
	}{
		{"no flag at all (before testing.Init)", nil, 0},
		{"a Value that is not a Getter", &flag.Flag{Name: "test.timeout", Value: plainValue{}}, 0},
		{"a Getter of the wrong type", &flag.Flag{Name: "test.timeout", Value: &str}, 0},
		{"the real thing", &flag.Flag{Name: "test.timeout", Value: dur}, 30 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deadlineFrom(tc.flag); got != tc.want {
				t.Errorf("deadlineFrom(%v) = %s, want %s", tc.flag, got, tc.want)
			}
		})
	}
}

// durationValue is the shape the testing package registers -test.timeout as: a
// flag.Getter whose Get returns a time.Duration.
type durationValue time.Duration

func (d *durationValue) String() string { return time.Duration(*d).String() }
func (d *durationValue) Set(v string) error {
	parsed, err := time.ParseDuration(v)
	*d = durationValue(parsed)
	return err
}
func (d *durationValue) Get() any { return time.Duration(*d) }

// TestBudgetReadsThisRunsDeadline: the derivation is wired to the real flag, not
// only to the pure function. `go test` passes -test.timeout on this binary's own
// command line — a flag, never an environment variable
// (docs/org/engineering-guide.md §"No hidden effects") — so a budget asked for
// inside a test sees whatever -timeout this run chose.
func TestBudgetReadsThisRunsDeadline(t *testing.T) {
	t.Parallel()
	deadline := packageDeadline()
	if deadline < 0 {
		t.Fatalf("packageDeadline() = %s, want this run's -test.timeout or 0", deadline)
	}
	// Budget reads the clock, so it cannot be compared against a second reading
	// of it. It is non-increasing in elapsed time, so bracketing the call pins
	// the derivation exactly whichever instant it saw.
	before := time.Since(packageStart)
	got := Budget()
	after := time.Since(packageStart)
	hi, lo := budgetWithin(deadline, before), budgetWithin(deadline, after)
	if got > hi || got < lo {
		t.Errorf("Budget() = %s, want the derivation from -test.timeout %s — within [%s, %s]",
			got, deadline, lo, hi)
	}
	if got < MinBudget || got > MaxBudget {
		t.Errorf("Budget() = %s, want it within [%s, %s]", got, MinBudget, MaxBudget)
	}
}

// TestRunUsesTheDerivedBudget: Run is RunWithin at the budget the run's own
// deadline yields, which is the one every test gets unless it is testing the
// backstop itself.
func TestRunUsesTheDerivedBudget(t *testing.T) {
	t.Parallel()
	r := Run(t, os.Args[0], helperEnv("exit", "0"), helperArgs...)
	if r.Budget < MinBudget || r.Budget > MaxBudget {
		t.Errorf("Budget = %s, want it within [%s, %s]", r.Budget, MinBudget, MaxBudget)
	}
	if r.Deadline != packageDeadline() {
		t.Errorf("Deadline = %s, want the -test.timeout it was derived from %s",
			r.Deadline, packageDeadline())
	}
}

// TestRunsGuardIsTransparentToAnOrdinaryFailure: Run now fails the test itself
// when the backstop killed the child, so no caller's own ExitCode check can see
// a killed record and report a busy host as a broken subject (T2207). That guard
// must fire on exactly that and nothing else — a child that ran and reported a
// failure is the normal case, and several tests here assert on its status.
//
// This is the silent direction; the firing direction is
// TestGuardsEndTheTestAndSayWhichOneFired, which watches it from another
// process because it can only end in t.Fatal.
func TestRunsGuardIsTransparentToAnOrdinaryFailure(t *testing.T) {
	t.Parallel()
	for _, code := range []int{1, 3} {
		r := Run(t, os.Args[0], helperEnv("exit", strconv.Itoa(code)), helperArgs...)
		if r.ExitCode != code {
			t.Errorf("Run changed the record: exit %d, want the child's %d:%s",
				r.ExitCode, code, r.Detail())
		}
		if msg := r.killedMessage(); msg != "" {
			t.Errorf("a child that exited %d was reported as killed:\n%s", code, msg)
		}
	}
}

// TestRunOKAndRunFailingAcceptTheirOwnStatus pins which code each one is asking
// for — that neither fails the test for the status it exists to accept. Their
// rejection path cannot end anywhere but t.Fatalf, so it is driven from a
// re-executed process instead, in TestGuardsEndTheTestAndSayWhichOneFired.
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
		TimedOut: true, Budget: MaxBudget, Deadline: 30 * time.Minute, Elapsed: MaxBudget,
	}
	msg := killed.killedMessage()
	if msg == "" {
		t.Fatal("a killed child produced no message")
	}
	for _, want := range []string{
		MaxBudget.String(),            // which budget fired
		"backstop",                    // that a budget is what fired
		"not the behavior under test", // and that it says nothing about the subject
		"KILLED",                      // the record, so the reader can see the streams
		"go test -timeout 30m0s",      // where the budget came from
		"raise -timeout to raise it",  // and the lever that moves it (T2207)
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the killed message dropped %q:\n%s", want, msg)
		}
	}
}

// TestKilledMessageNamesTheCeilingWhenTheRunSetNoDeadline: with `go test
// -timeout 0` there is no deadline to be half of, so the message must name the
// ceiling that bounded the child instead of a deadline that does not exist.
func TestKilledMessageNamesTheCeilingWhenTheRunSetNoDeadline(t *testing.T) {
	t.Parallel()
	killed := Result{
		Args: []string{"test", "x.pr"}, ExitCode: -1,
		TimedOut: true, Budget: MaxBudget, Deadline: 0, Elapsed: MaxBudget,
	}
	msg := killed.killedMessage()
	for _, want := range []string{"no deadline", "go test -timeout 0", MaxBudget.String()} {
		if !strings.Contains(msg, want) {
			t.Errorf("the killed message dropped %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "raise -timeout to raise it") {
		t.Errorf("a run with no deadline must not be told to raise one:\n%s", msg)
	}
}

// TestKilledMessageIsEmptyForAChildThatRan: the guard is transparent otherwise,
// whatever the child's status was.
func TestKilledMessageIsEmptyForAChildThatRan(t *testing.T) {
	t.Parallel()
	for _, code := range []int{0, 1, 134} {
		r := Result{ExitCode: code, Budget: MinBudget}
		if msg := r.killedMessage(); msg != "" {
			t.Errorf("exit %d produced a killed message:\n%s", code, msg)
		}
	}
}
