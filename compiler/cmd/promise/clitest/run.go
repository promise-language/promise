package clitest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// DefaultBudget bounds one compiler invocation driven by Run.
//
// It is a backstop, not a timing assertion: a wedged child must cost this test
// rather than the package deadline, where it surfaces as a goroutine dump with
// no hint of which invocation hung. The budget is sized for compile + run on a
// WARM home — SharedHome pays for toolchain materialization before any test
// starts, so no child under it can spend its budget on a 375 MB copy, which is
// what made three minutes look tight on a saturated macOS runner (T2133).
const DefaultBudget = 3 * time.Minute

// waitDelay bounds how long Wait blocks on the capture pipes after the child
// itself is gone, so a surviving grandchild cannot hold a test open past its
// budget. See RunWithin for why killing the process group is not worth it.
const waitDelay = 5 * time.Second

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

// Run invokes the built compiler under DefaultBudget. env entries are appended
// to the test process's environment; nil leaves it inherited as-is.
func Run(t *testing.T, bin string, env []string, args ...string) Result {
	t.Helper()
	return RunWithin(t, bin, env, DefaultBudget, args...)
}

// RunWithin is Run under an explicit budget, for the deadline's own tests: a
// backstop that is never exercised is a backstop nobody knows still works.
func RunWithin(t *testing.T, bin string, env []string, budget time.Duration, args ...string) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
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
		Args: args, Env: env,
		Stdout: out.String(), Stderr: errb.String(),
		Err: err, ExitCode: code, TimedOut: timedOut,
		Budget: budget, Elapsed: time.Since(start),
	}
}

// runExpectingExit runs the compiler and requires an exact exit code. A child
// that failed to compile, was killed, or timed out fails the test here — where
// the evidence is — rather than downstream, where the only symptom is an empty
// stream and the assertion blames the wrong subsystem.
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
// how it is delivered.
func (r Result) killedMessage() string {
	if !r.TimedOut {
		return ""
	}
	return fmt.Sprintf("the child was killed by this test's %s backstop (compile + run) — "+
		"that is the host or the compile, not the behavior under test:%s",
		r.Budget, r.Detail())
}
