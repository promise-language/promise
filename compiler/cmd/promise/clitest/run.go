package clitest

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// MinBudget is the floor a compiler invocation always gets, and was the whole
// budget before T2207. MaxBudget is the ceiling: a wedged child still has to be
// reported in a time a person will wait for, and the slowest real invocation
// measured here is 30s on an idle host and 93s under a full `go test ./...`, so
// ten minutes is already far past anything that is merely slow.
const (
	MinBudget = 3 * time.Minute
	MaxBudget = 10 * time.Minute
)

// waitDelay bounds how long Wait blocks on the capture pipes after the child
// itself is gone, so a surviving grandchild cannot hold a test open past its
// budget. See RunWithin for why killing the process group is not worth it.
const waitDelay = 5 * time.Second

// packageStart is when this test binary began. Go starts its own -test.timeout
// clock slightly later, inside m.Run, so a budget measured from here is
// conservative by however long TestMain's warm-up took — which is the direction
// that cannot overrun the deadline.
var packageStart = time.Now()

// Budget bounds one compiler invocation driven by Run.
//
// It is a backstop, not a timing assertion: a wedged child must cost this test
// rather than the package deadline, where it surfaces as a goroutine dump with
// no hint of which invocation hung. That purpose is defined entirely by the
// package deadline, so the budget is derived from it rather than chosen
// independently — a fixed number is either below what a busy host needs or above
// the deadline it exists to pre-empt, and three minutes was the first (T2207).
// On a 24-core linux host a full `go test ./...` already left the slowest of
// these invocations under 2x of it; a slower host loses tests that were only
// slow, and reports them as product failures.
//
// It is sized for compile + run on a WARM home — SharedHome pays for toolchain
// materialization before any test starts, so no child under it can spend its
// budget on a 375 MB copy (T2133).
func Budget() time.Duration {
	return budgetWithin(packageDeadline(), time.Since(packageStart))
}

// budgetWithin is Budget's decision without the clock, so every branch of it is
// reachable from a table test.
//
// Half of what the deadline has left: the backstop always fires first, which is
// the only reason it exists, and the other half is still there for every peer
// test in the package. Near the end of a deadline the floor wins and the budget
// can outlast it — that is exactly today's behaviour, and giving a late test
// less than every earlier one got would be a worse trade.
func budgetWithin(deadline, elapsed time.Duration) time.Duration {
	if deadline <= 0 { // `go test -timeout 0`, or before testing.Init registered it
		return MaxBudget
	}
	return min(max((deadline-elapsed)/2, MinBudget), MaxBudget)
}

// packageDeadline is the -test.timeout `go test` gave this binary, or 0 when it
// gave none.
//
// It is read from a FLAG on this process's own command line, never from the
// environment: docs/org/engineering-guide.md §"No hidden effects" — an
// environment variable is never an input, and a budget that moved with an
// inherited one would make two identical command lines mean different things.
// `go test -timeout` is therefore the lever that raises this, and it is visible
// in the invocation that used it.
func packageDeadline() time.Duration { return deadlineFrom(flag.Lookup("test.timeout")) }

// deadlineFrom is packageDeadline's decision without the process-global flag
// set, so each of its three "nothing to read" branches is reachable from a table
// test rather than only from a testing.Init that has not run yet.
//
// All three answer 0, which budgetWithin reads as "no deadline to be half of".
// The flag is absent before testing.Init registers it; a Value that is not a
// Getter, or one whose Get is not a Duration, would mean the testing package had
// redefined -test.timeout as something else, and guessing at a duration there is
// worse than declining to.
func deadlineFrom(f *flag.Flag) time.Duration {
	if f == nil {
		return 0
	}
	g, ok := f.Value.(flag.Getter)
	if !ok {
		return 0
	}
	d, _ := g.Get().(time.Duration)
	return d
}

// Result is one invocation: what was run, how it ended, and its separated
// streams.
//
// Stdout and stderr are captured apart because a message built from one of them
// cannot say why a child produced nothing. A test that reads only stdout reports
// "the output is wrong" when the truth is "there was no run", and throws away the
// exit status and stderr — the only two places the cause could be (T2119).
type Result struct {
	Args, Env      []string
	Stdout, Stderr string
	Err            error
	ExitCode       int // -1: killed by the budget, or never started
	TimedOut       bool
	Budget         time.Duration
	Deadline       time.Duration // the -test.timeout Budget was derived from; 0 = none
	Elapsed        time.Duration
}

// Combined is the two streams as one, for assertions that do not care which
// stream a line arrived on.
func (r Result) Combined() string { return r.Stdout + r.Stderr }

// Detail is the whole record of one invocation — see Result for why a failure
// message built from stdout alone is worse than no message at all.
func (r Result) Detail() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n  command: promise %s\n", strings.Join(r.Args, " "))
	fmt.Fprintf(&b, "  env:     %v\n", r.Env)
	if r.TimedOut {
		fmt.Fprintf(&b, "  exit:    KILLED — exceeded the %s budget after %s\n",
			r.Budget, r.Elapsed.Round(time.Millisecond))
	} else {
		fmt.Fprintf(&b, "  exit:    code %d (%v) after %s\n",
			r.ExitCode, r.Err, r.Elapsed.Round(time.Millisecond))
	}
	fmt.Fprintf(&b, "  stdout:  %s\n", quoteStream(r.Stdout))
	fmt.Fprintf(&b, "  stderr:  %s\n", quoteStream(r.Stderr))
	return b.String()
}

// quoteStream renders a captured stream so an empty one is visibly empty rather
// than a blank line the reader has to interpret.
func quoteStream(s string) string {
	if s == "" {
		return "<empty>"
	}
	return "\n    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}

// Run invokes the built compiler under Budget, and fails the test outright if
// the backstop killed it. env entries are appended to the test process's
// environment; nil leaves it inherited as-is.
//
// The guard is part of Run rather than something each caller remembers, because
// forgetting it is invisible: a killed child comes back with empty streams and
// exit -1, and whatever the caller asserts next reports its own subject as
// broken. That is what happened to three of T2207's four victims — two through
// RunFailing's "expected exit 1, got -1" and one through a test's own ExitCode
// check. Out of Run, no caller can see a killed record at all.
func Run(t *testing.T, bin string, env []string, args ...string) Result {
	t.Helper()
	return RunWithin(t, bin, env, Budget(), args...).RequireRan(t)
}

// RunWithin is Run under an explicit budget and WITHOUT the killed-child guard:
// it is the one form that hands a killed Result back rather than failing, and so
// the only one the backstop's own tests can be written against. A backstop that
// is never exercised is a backstop nobody knows still works.
func RunWithin(t *testing.T, bin string, env []string, budget time.Duration, args ...string) Result {
	t.Helper()
	full := []string(nil)
	if env != nil {
		full = append(os.Environ(), env...)
	}
	return runBounded(child{bin: bin, args: args, env: full, shown: env, budget: budget})
}

// child is one invocation to start. Every subprocess this package spawns — Run,
// RunWithin and each Env method — is described as one and started by runBounded,
// so one budget and one killed-child report cover the whole surface rather than
// some of it.
type child struct {
	bin    string   // the executable
	dir    string   // working directory; "" inherits the test process's
	args   []string // its arguments
	env    []string // the child's WHOLE environment; nil inherits
	shown  []string // what Detail names — the entries a caller added, not the inherited ones
	budget time.Duration
}

// runBounded starts one child under its budget and records how it ended.
func runBounded(c child) Result {
	ctx, cancel := context.WithTimeout(context.Background(), c.budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bin, c.args...)
	cmd.Dir = c.dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Env = c.env
	// The child fans out to opt/llc and to the compiled test binary. Those
	// grandchildren are self-limiting — opt/llc finish in seconds, and the test
	// binary has its own per-test watchdog and batch budget (T1639) — so killing
	// the process group, which out here would need a build-tagged unix/windows
	// pair, buys nothing over bounding the pipe wait.
	cmd.WaitDelay = waitDelay
	start := time.Now()
	err := cmd.Run()
	timedOut := ctx.Err() != nil
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	// A child killed by the deadline has no exit status of its own, and the two
	// platforms disagree about how to say so. Unix reports the SIGKILL as -1:
	// the child was signalled, never exited. Windows has no signals, so Kill()
	// is TerminateProcess(h, 1) and the child genuinely *exits* 1 — the very
	// code a suite that reported failures exits with, which is what
	// RunFailing below reads as "reported a failure". Normalize, so -1 means
	// "no status of its own" on every platform and an exit-1 expectation can
	// never be satisfied by a corpse (T2122).
	if timedOut {
		code = -1
	}
	return Result{
		Args: c.args, Env: c.shown,
		Stdout: out.String(), Stderr: errb.String(),
		Err: err, ExitCode: code, TimedOut: timedOut,
		Budget: c.budget, Deadline: packageDeadline(), Elapsed: time.Since(start),
	}
}

// runExpectingExit runs the compiler and requires an exact exit code. A child
// that failed to compile fails the test here — where the evidence is — rather
// than downstream, where the only symptom is an empty stream and the assertion
// blames the wrong subsystem. A child killed by the backstop never reaches the
// comparison: Run has already failed the test naming the backstop, so this can
// no longer report a busy host as "expected exit 1, got -1" (T2207).
func runExpectingExit(t *testing.T, bin string, env []string, want int, args ...string) Result {
	t.Helper()
	r := Run(t, bin, env, args...)
	if r.ExitCode != want {
		t.Fatalf("expected exit %d, got %d:%s", want, r.ExitCode, r.Detail())
	}
	return r
}

// RunOK requires a clean exit.
func RunOK(t *testing.T, bin string, env []string, args ...string) Result {
	t.Helper()
	return runExpectingExit(t, bin, env, 0, args...)
}

// RunFailing requires exit code 1 — `promise test` reports a failing suite, and
// a usage error, with exactly that. A killed child (-1, normalized by RunWithin
// so Windows' TerminateProcess exit 1 cannot masquerade as a reported failure),
// a Go panic (2) or a memory-limit abort (134) is a dead child rather than a
// reported failure, and must not satisfy a test that only asked for "non-zero".
func RunFailing(t *testing.T, bin string, env []string, args ...string) Result {
	t.Helper()
	return runExpectingExit(t, bin, env, 1, args...)
}

// RequireRan fails the test when the child was killed by its budget, naming the
// budget rather than whatever the test was about to assert. A saturated host is
// the host; it is never evidence about the subject under test.
//
// Run applies it to every invocation, so this is only needed after RunWithin.
func (r Result) RequireRan(t *testing.T) Result {
	t.Helper()
	if msg := r.killedMessage(); msg != "" {
		t.Fatal(msg)
	}
	return r
}

// killedMessage is what RequireRan reports, or "" when the child ran.
//
// Separate from the t.Fatal so the wording can be asserted: what T2133 fixed was
// two tests reporting a saturated host as an unbounded join, so the message
// naming the backstop instead of the subject IS the deliverable, not a detail of
// how it is delivered. It also names where the backstop came from and how to
// move it, because a reader who cannot see the lever reads the number as a law
// and goes looking for the bug instead (T2207).
func (r Result) killedMessage() string {
	if !r.TimedOut {
		return ""
	}
	return fmt.Sprintf("the child was killed by this test's %s backstop (compile + run) — "+
		"that is the host or the compile, not the behavior under test. %s:%s",
		r.Budget, r.budgetOrigin(), r.Detail())
}

// budgetOrigin says which `go test` deadline the budget was half of, and so
// which flag raises it.
func (r Result) budgetOrigin() string {
	if r.Deadline <= 0 {
		return "This run set no deadline (`go test -timeout 0`), so the backstop is the " +
			MaxBudget.String() + " ceiling"
	}
	return fmt.Sprintf("The backstop is half of what `go test -timeout %s` left this package, "+
		"within [%s, %s]; raise -timeout to raise it", r.Deadline, MinBudget, MaxBudget)
}
