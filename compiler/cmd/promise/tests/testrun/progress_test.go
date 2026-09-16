package testrun

// End-to-end coverage for T1888's render modes. The in-process printer tests in
// package main pin the display layer over a fixed result set; these pin what
// the built binary actually writes to stdout and stderr, which is the only
// place the whole chain — flag parsing, mode resolution, the multi-file
// parent's own `pass` lines, and the `-progress full` it forces on its children
// — is observable at once.
//
// Every child runs through runProgressOK or runProgressFailing, which check the
// exit status before any assertion reads a stream, and every message that could
// otherwise print an empty stream ends with progressRun.detail(). These tests
// assert on what the child *renders*, and a child that never ran renders
// nothing: a message built from stdout alone then reports "the mode is wrong"
// when the truth is "there was no run", and throws away the exit status and
// stderr — the only two places the cause could be (T2119).

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// progressPassingSource is two passing batch tests — the multi-file parent
// prints one `pass (…) file.pr (2 tests)` line for it.
const progressPassingSource = `
alpha_one() ` + "`test" + ` {
  assert(1 + 1 == 2, "one");
}

alpha_two() ` + "`test" + ` {
  assert(2 + 2 == 4, "two");
}
`

// progressMixedSource has one passing and one failing test, so every mode has
// something that must persist as well as something that may be suppressed.
const progressMixedSource = `
beta_ok() ` + "`test" + ` {
  assert(3 + 3 == 6, "ok");
}

beta_broken() ` + "`test" + ` {
  assert(1 == 2, "deliberate failure");
}
`

// progressSnapshotSource is the snapshot form, whose passing line is the
// name-less "PASS (…)" spelling rather than "pass (…) name".
const progressSnapshotSource = `
main() ` + "`test(expected: \"hi\")" + ` {
  print_line("hi");
}
`

// progressBrokenSource does not parse, so the child dies before it can run a
// single test — the shape T2119's failure had: nothing on stdout, everything on
// stderr.
const progressBrokenSource = `
broken_one() ` + "`test" + ` {
  assert(1 + == 2, "unparseable");
}
`

// progressRunTimeout bounds one child invocation. These runs compile a two-test
// file; the worst observed under a saturated bin/verify was 29s (T2119). The
// budget is compile + slack — the same 3 minutes stuck_test.go allows for the
// same reason — not a timing assertion. Without it a wedged child costs the
// whole package deadline and surfaces as a goroutine dump rather than as this
// test.
const progressRunTimeout = 3 * time.Minute

// progressRun is one invocation: what was run, how it ended, and its separated
// streams.
type progressRun struct {
	args, env      []string
	stdout, stderr string
	err            error
	exitCode       int // -1: killed by the deadline, or never started
	timedOut       bool
	budget         time.Duration // the deadline this run was given
	elapsed        time.Duration
}

// detail is the whole record of one invocation — see the file comment for why a
// failure message built from stdout alone is worse than no message at all.
func (r progressRun) detail() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n  command: promise %s\n", strings.Join(r.args, " "))
	fmt.Fprintf(&b, "  env:     %v\n", r.env)
	if r.timedOut {
		fmt.Fprintf(&b, "  exit:    KILLED — exceeded the %s budget after %s\n",
			r.budget, r.elapsed.Round(time.Millisecond))
	} else {
		fmt.Fprintf(&b, "  exit:    code %d (%v) after %s\n",
			r.exitCode, r.err, r.elapsed.Round(time.Millisecond))
	}
	fmt.Fprintf(&b, "  stdout:  %s\n", quoteStream(r.stdout))
	fmt.Fprintf(&b, "  stderr:  %s\n", quoteStream(r.stderr))
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

// runProgress invokes the built compiler with stdout and stderr captured
// separately — CombinedOutput would interleave the transient line into the
// stream this test has to prove stays clean.
//
// Callers go through runProgressOK or runProgressFailing rather than calling
// this directly, so the exit status is checked before any assertion reads a
// stream.
func runProgress(t *testing.T, bin string, env []string, args ...string) progressRun {
	t.Helper()
	return runProgressWithin(t, bin, env, progressRunTimeout, args...)
}

// runProgressWithin is runProgress under an explicit budget. Only the deadline's
// own test passes anything but progressRunTimeout: a backstop that is never
// exercised is a backstop nobody knows still works.
func runProgressWithin(t *testing.T, bin string, env []string, budget time.Duration, args ...string) progressRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	// The child fans out to opt/llc and to the compiled test binary. WaitDelay
	// bounds how long Wait blocks on the capture pipes once the child is gone,
	// so a surviving grandchild cannot hold this test open. Those grandchildren
	// are self-limiting — opt/llc finish in seconds, and the test binary has its
	// own per-test watchdog and batch budget (T1639) — so killing the process
	// group, which out here would need a build-tagged unix/windows pair, buys
	// nothing.
	cmd.WaitDelay = 5 * time.Second
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
	// runProgressFailing below reads as "reported a failure". Normalize, so -1
	// means "no status of its own" on every platform and an exit-1 expectation
	// can never be satisfied by a corpse (T2122).
	if timedOut {
		code = -1
	}
	return progressRun{
		args: args, env: env,
		stdout: out.String(), stderr: errb.String(),
		err: err, exitCode: code, timedOut: timedOut,
		budget: budget, elapsed: time.Since(start),
	}
}

// runProgressExpectingExit runs the compiler and requires an exact exit code. A
// child that failed to compile, was killed, or timed out fails the test here —
// where the evidence is — rather than downstream, where the only symptom is an
// empty stream and the assertion blames the render mode.
func runProgressExpectingExit(t *testing.T, bin string, env []string, want int, args ...string) progressRun {
	t.Helper()
	r := runProgress(t, bin, env, args...)
	if r.exitCode != want {
		t.Fatalf("expected exit %d, got %d:%s", want, r.exitCode, r.detail())
	}
	return r
}

// runProgressOK requires a clean exit.
func runProgressOK(t *testing.T, bin string, env []string, args ...string) progressRun {
	t.Helper()
	return runProgressExpectingExit(t, bin, env, 0, args...)
}

// runProgressFailing requires exit code 1 — `promise test` reports a failing
// suite, and a usage error, with exactly that. A killed child (-1, normalized
// by runProgressWithin so Windows' TerminateProcess exit 1 cannot masquerade as
// a reported failure), a Go panic (2) or a memory-limit abort (134) is a dead
// child rather than a reported failure, and must not satisfy a test that only
// asked for "non-zero".
func runProgressFailing(t *testing.T, bin string, env []string, args ...string) progressRun {
	t.Helper()
	return runProgressExpectingExit(t, bin, env, 1, args...)
}

// summaryRe matches the multi-file grand summary and the single-file batch
// summary alike — the block the item's invariant pins as mode-independent.
var progressSummaryRe = regexp.MustCompile(`(?m)^\d+ passed, \d+ failed.*$`)

func summaryLines(t *testing.T, r progressRun) string {
	t.Helper()
	m := progressSummaryRe.FindAllString(r.stdout, -1)
	if len(m) == 0 {
		t.Fatalf("no summary line in output:%s", r.detail())
	}
	return strings.Join(m, "\n")
}

func passLines(out string) []string {
	var got []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "pass ") || strings.HasPrefix(l, "PASS (") {
			got = append(got, l)
		}
	}
	return got
}

// writeFixture writes the three fixture files into a fresh directory. The
// directory is stable for the life of the test, so the three mode runs share
// one build-cache entry and only the first pays for compilation.
func writeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range map[string]string{
		"alpha_test.pr": progressPassingSource,
		"beta_test.pr":  progressMixedSource,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestProgressModes_MultiFile is the headline case: over one directory of test
// files, `full` prints every per-file pass line, `plain` prints none, `tty`
// prints none on stdout but rewrites them on stderr — and all three print a
// byte-identical summary and an identical set of failure lines.
func TestProgressModes_MultiFile(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	dir := writeFixture(t)

	// The fixture has a deliberately failing test, so each mode exits 1.
	full := runProgressFailing(t, bin, nil, "test", "-progress", "full", dir)
	plain := runProgressFailing(t, bin, nil, "test", "-progress", "plain", dir)
	tty := runProgressFailing(t, bin, nil, "test", "-progress", "tty", dir)

	// 1. `full` is the machine-readable stream: the passing file is named.
	if got := passLines(full.stdout); len(got) != 1 || !strings.Contains(got[0], "alpha_test.pr") {
		t.Errorf("full mode pass lines = %v, want one naming alpha_test.pr%s", got, full.detail())
	}

	// 2. Neither quiet mode prints a passing line on stdout.
	for name, r := range map[string]progressRun{"plain": plain, "tty": tty} {
		if got := passLines(r.stdout); len(got) != 0 {
			t.Errorf("%s mode leaked passing lines on stdout: %v%s", name, got, r.detail())
		}
	}

	// 3. tty stdout is byte-identical to plain stdout apart from the timings
	//    baked into each line, so compare the lines with timings elided.
	if got, want := elideTimings(tty.stdout), elideTimings(plain.stdout); got != want {
		t.Errorf("tty stdout differs from plain:\n tty:\n%s\nplain:\n%s\ntty run:%s\nplain run:%s",
			got, want, tty.detail(), plain.detail())
	}

	// 4. THE INVARIANT: the summary is the same in every mode.
	fs := elideTimings(summaryLines(t, full))
	ps := elideTimings(summaryLines(t, plain))
	ts := elideTimings(summaryLines(t, tty))
	if fs != ps || ps != ts {
		t.Errorf("summary differs between modes:\n full: %q\nplain: %q\n  tty: %q", fs, ps, ts)
	}
	// alpha contributes two passes, beta one pass and one failure.
	if !strings.Contains(fs, "3 passed, 1 failed (2 files") {
		t.Errorf("summary = %q, want it to report 3 passed, 1 failed over 2 files%s", fs, full.detail())
	}

	// 5. Failures persist verbatim in every mode, with their FAILED: section.
	for name, r := range map[string]progressRun{"full": full, "plain": plain, "tty": tty} {
		for _, want := range []string{"FAIL (", "beta_test.pr", "beta_broken", "FAILED:"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("%s mode dropped %q from stdout:%s", name, want, r.detail())
			}
		}
	}

	// 6. Nothing transient may reach stdout in any mode — RunTee captures it and
	//    ExtractFailedSection re-parses it.
	for name, r := range map[string]progressRun{"full": full, "plain": plain, "tty": tty} {
		if strings.ContainsAny(r.stdout, "\r\x1b") {
			t.Errorf("%s mode leaked a carriage return or escape into stdout:\n%q", name, r.stdout)
		}
	}

	// 7. Only tty writes a transient line, and it uses no ANSI.
	if !strings.Contains(tty.stderr, "\r") {
		t.Errorf("tty mode wrote no in-place update on stderr; got %q", tty.stderr)
	}
	if strings.Contains(tty.stderr, "\x1b") {
		t.Errorf("tty mode used ANSI escapes: %q", tty.stderr)
	}
	if strings.Contains(tty.stderr, "\n") {
		t.Errorf("the transient line must stay on one row; stderr = %q", tty.stderr)
	}
	// And it is erased before the process exits, so a shell prompt lands clean.
	if !strings.HasSuffix(tty.stderr, "\r") {
		t.Errorf("tty mode left the progress line on screen; stderr = %q", tty.stderr)
	}
	for name, r := range map[string]progressRun{"full": full, "plain": plain} {
		if strings.Contains(r.stderr, "\r") {
			t.Errorf("%s mode wrote an in-place update to stderr: %q", name, r.stderr)
		}
	}
}

// elideTimings replaces every "(1.234s)" with a fixed token, so two runs of the
// same suite can be compared for everything except how long they took.
var timingRe = regexp.MustCompile(`-?\d+\.\d+s`)

func elideTimings(s string) string { return timingRe.ReplaceAllString(s, "Ts") }

// TestProgressModes_SingleFile covers the other printer: the per-test lines a
// single-file run streams through printChildTestOutput.
func TestProgressModes_SingleFile(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "single_test.pr")
	if err := os.WriteFile(src, []byte(progressMixedSource), 0o644); err != nil {
		t.Fatal(err)
	}

	// The mixed fixture has a deliberately failing test, so each mode exits 1.
	full := runProgressFailing(t, bin, nil, "test", "-progress", "full", src)
	plain := runProgressFailing(t, bin, nil, "test", "-progress", "plain", src)
	tty := runProgressFailing(t, bin, nil, "test", "-progress", "tty", src)

	if got := passLines(full.stdout); len(got) != 1 || !strings.Contains(got[0], "beta_ok") {
		t.Errorf("full mode pass lines = %v, want one naming beta_ok%s", got, full.detail())
	}
	for name, r := range map[string]progressRun{"plain": plain, "tty": tty} {
		if got := passLines(r.stdout); len(got) != 0 {
			t.Errorf("%s mode leaked passing lines: %v%s", name, got, r.detail())
		}
		if !strings.Contains(r.stdout, "FAIL (") || !strings.Contains(r.stdout, "beta_broken") {
			t.Errorf("%s mode dropped the failure line:%s", name, r.detail())
		}
		// The panic context under a FAIL is not a progress line and must stay.
		if !strings.Contains(r.stdout, "deliberate failure") {
			t.Errorf("%s mode dropped the assertion context:%s", name, r.detail())
		}
	}
	fs := elideTimings(summaryLines(t, full))
	ps := elideTimings(summaryLines(t, plain))
	ts := elideTimings(summaryLines(t, tty))
	if fs != ps || ps != ts {
		t.Errorf("single-file summary differs between modes:\n full: %q\nplain: %q\n  tty: %q", fs, ps, ts)
	}
	if !strings.Contains(tty.stderr, "\r") {
		t.Errorf("tty mode wrote no in-place update; stderr = %q", tty.stderr)
	}
}

// TestProgressModes_SnapshotPassSuppressed covers the second passing spelling:
// a snapshot test prints "PASS (…)" with no name, which is progress just the
// same, while its summary is not.
func TestProgressModes_SnapshotPassSuppressed(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "snap_test.pr")
	if err := os.WriteFile(src, []byte(progressSnapshotSource), 0o644); err != nil {
		t.Fatal(err)
	}

	// The snapshot test passes, so both runs must exit 0.
	full := runProgressOK(t, bin, nil, "test", "-progress", "full", src)
	plain := runProgressOK(t, bin, nil, "test", "-progress", "plain", src)
	if got := passLines(full.stdout); len(got) != 1 || !strings.HasPrefix(got[0], "PASS (") {
		t.Errorf("full mode pass lines = %v, want one \"PASS (…)\"%s", got, full.detail())
	}
	if got := passLines(plain.stdout); len(got) != 0 {
		t.Errorf("plain mode leaked the snapshot pass line: %v%s", got, plain.detail())
	}
	if a, b := elideTimings(summaryLines(t, full)), elideTimings(summaryLines(t, plain)); a != b {
		t.Errorf("snapshot summary differs between modes: %q vs %q", a, b)
	}
}

// TestProgressFlagBeatsEnv pins the precedence the item asks for: an explicit
// -progress wins over PROMISE_PROGRESS, which wins over detection. The env
// override is the NO_COLOR-style escape hatch that forces a terminal run into
// the pipe rendering.
func TestProgressFlagBeatsEnv(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "env_test.pr")
	if err := os.WriteFile(src, []byte(progressPassingSource), 0o644); err != nil {
		t.Fatal(err)
	}

	// The fixture is two passing tests, so every run below must exit 0 —
	// including the unrecognized-env one, whose whole point is that an
	// unrecognized PROMISE_PROGRESS falls through to detection rather than
	// being fatal.
	//
	// Env alone: full turns the pass lines back on even though stdout is a pipe.
	envFull := runProgressOK(t, bin, []string{"PROMISE_PROGRESS=full"}, "test", src)
	if len(passLines(envFull.stdout)) == 0 {
		t.Errorf("PROMISE_PROGRESS=full did not restore pass lines:%s", envFull.detail())
	}
	// Flag beats env in both directions.
	flagPlain := runProgressOK(t, bin, []string{"PROMISE_PROGRESS=full"}, "test", "-progress", "plain", src)
	if got := passLines(flagPlain.stdout); len(got) != 0 {
		t.Errorf("-progress plain lost to PROMISE_PROGRESS=full: %v%s", got, flagPlain.detail())
	}
	flagFull := runProgressOK(t, bin, []string{"PROMISE_PROGRESS=plain"}, "test", "-progress", "full", src)
	if len(passLines(flagFull.stdout)) == 0 {
		t.Errorf("-progress full lost to PROMISE_PROGRESS=plain:%s", flagFull.detail())
	}
	// The default with a piped stdout and no env is the quiet form.
	auto := runProgressOK(t, bin, []string{"PROMISE_PROGRESS="}, "test", src)
	if got := passLines(auto.stdout); len(got) != 0 {
		t.Errorf("the piped default printed pass lines: %v%s", got, auto.detail())
	}
	// An unrecognized env value falls through to detection rather than failing.
	garbage := runProgressOK(t, bin, []string{"PROMISE_PROGRESS=quiet"}, "test", src)
	if got := passLines(garbage.stdout); len(got) != 0 {
		t.Errorf("garbage env should fall back to detection (plain); got %v%s", got, garbage.detail())
	}
	// Every mode agrees on the summary.
	want := elideTimings(summaryLines(t, envFull))
	for name, r := range map[string]progressRun{
		"flagPlain": flagPlain, "flagFull": flagFull, "auto": auto, "garbage": garbage,
	} {
		if got := elideTimings(summaryLines(t, r)); got != want {
			t.Errorf("%s summary = %q, want %q%s", name, got, want, r.detail())
		}
	}
}

// TestProgressFlagRejectsUnknownSpelling: an unrecognized -progress *value* is a
// usage error, unlike an unrecognized env value. A typo on the command line is
// the user asking for something specific and getting it wrong; silently
// choosing a mode would hide it.
func TestProgressFlagRejectsUnknownSpelling(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	for _, bad := range []string{"quiet", "1", "on", ""} {
		// A usage error is exit 1; runProgressFailing is what rejects it being
		// accepted (exit 0) or the child dying on the way to saying so.
		r := runProgressFailing(t, bin, nil, "test", "-progress", bad, "nonexistent_test.pr")
		if !strings.Contains(r.stderr, "-progress requires one of: auto, full, plain, tty") {
			t.Errorf("-progress %q: want the accepted-spellings message:%s", bad, r.detail())
		}
	}
	// "auto" is accepted and means "detect" — under a pipe, that is plain.
	dir := t.TempDir()
	src := filepath.Join(dir, "auto_test.pr")
	if err := os.WriteFile(src, []byte(progressPassingSource), 0o644); err != nil {
		t.Fatal(err)
	}
	r := runProgressOK(t, bin, []string{"PROMISE_PROGRESS="}, "test", "-progress", "auto", src)
	if got := passLines(r.stdout); len(got) != 0 {
		t.Errorf("-progress auto under a pipe printed pass lines: %v%s", got, r.detail())
	}
	// And the usage line advertises the flag. No target is a usage error: exit 1.
	usage := runProgressFailing(t, bin, nil, "test")
	if !strings.Contains(usage.stderr, "-progress auto|full|plain|tty") {
		t.Errorf("usage does not document -progress:%s", usage.detail())
	}
}

// TestProgressDoesNotAffectOtherCommands guards the blast radius: -progress is a
// `promise test` concept, and every other command keeps printing exactly as it
// did. `promise run` on a program with output is the cheapest witness.
func TestProgressDoesNotAffectOtherCommands(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "hello.pr")
	if err := os.WriteFile(src, []byte("main() {\n  print_line(\"hello\");\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, env := range [][]string{nil, {"PROMISE_PROGRESS=plain"}, {"PROMISE_PROGRESS=tty"}} {
		r := runProgressOK(t, bin, env, "run", src)
		if strings.TrimSpace(r.stdout) != "hello" {
			t.Errorf("env %v: stdout = %q, want \"hello\"%s", env, r.stdout, r.detail())
		}
		// The rewriter's signature is a bare CR (return to line start) or an
		// ANSI escape. A CR that is part of a CRLF line ending is not that —
		// it is what the Windows PAL writes for every newline — so line
		// endings are normalized away before looking for one.
		if bare := strings.ReplaceAll(r.stdout, "\r\n", "\n"); strings.ContainsAny(bare, "\r\x1b") {
			t.Errorf("env %v: program output was rewritten: %q", env, r.stdout)
		}
	}
}

// TestProgressDoesNotAffectJSONMode pins the item's "--json mode is unaffected"
// constraint. The gate reads the JSONL on stdout and the health report is built
// from those records, so the stream must be identical whatever the render mode
// is — a suppressed `pass` would silently zero out the gate's passing tests.
func TestProgressDoesNotAffectJSONMode(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "json_test.pr"), []byte(progressMixedSource), 0o644); err != nil {
		t.Fatal(err)
	}

	// The mixed fixture has a deliberately failing test, so each run exits 1.
	base := runProgressFailing(t, bin, []string{"PROMISE_PROGRESS="}, "test", "--json", dir)
	for _, env := range [][]string{{"PROMISE_PROGRESS=plain"}, {"PROMISE_PROGRESS=tty"}, {"PROMISE_PROGRESS=full"}} {
		r := runProgressFailing(t, bin, env, "test", "--json", dir)
		if got, want := elideJSONVolatile(r.stdout), elideJSONVolatile(base.stdout); got != want {
			t.Errorf("env %v changed the JSONL stream:\n got: %s\nwant: %s%s", env, got, want, r.detail())
		}
		if strings.ContainsAny(r.stdout, "\r\x1b") {
			t.Errorf("env %v leaked a carriage return or escape into the JSONL stream: %q", env, r.stdout)
		}
	}
	// Sanity: the stream really does carry both outcomes, so an all-empty
	// comparison above could not have passed vacuously.
	for _, want := range []string{`"beta_ok"`, `"beta_broken"`, `"pass"`, `"fail"`} {
		if !strings.Contains(base.stdout, want) {
			t.Errorf("JSONL stream is missing %s:%s", want, base.detail())
		}
	}
}

// elideJSONVolatile blanks the per-record fields that legitimately differ
// between two runs of the same suite: durations and absolute paths.
var jsonVolatileRe = regexp.MustCompile(`"(elapsed|duration_ms|duration|file)":\s*("[^"]*"|[0-9.]+)`)

func elideJSONVolatile(s string) string {
	return jsonVolatileRe.ReplaceAllString(s, `"$1":X`)
}

// TestProgressRunReportsWhyTheChildFailed pins the diagnostic contract the
// helpers in this file exist for. Under a full bin/verify one of these children
// died during its cold compile, and the assertion reported "did not restore
// pass lines" with an empty body — discarding the exit status and stderr, the
// only two places the cause could be (T2119).
func TestProgressRunReportsWhyTheChildFailed(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "broken_test.pr")
	if err := os.WriteFile(src, []byte(progressBrokenSource), 0o644); err != nil {
		t.Fatal(err)
	}

	// The premise: a child that cannot compile writes nothing at all to stdout,
	// so an assertion that reads stdout alone has no evidence to report.
	r := runProgressFailing(t, bin, nil, "test", src)
	if r.stdout != "" {
		t.Errorf("a compile failure should leave stdout empty; got %q", r.stdout)
	}
	if !strings.Contains(r.stderr, "broken_test.pr") {
		t.Errorf("the compile error must reach stderr:%s", r.detail())
	}

	// So the record carries the exit status and stderr, and says "empty" out
	// loud rather than printing a blank line the reader has to interpret.
	// Whitespace is collapsed first: this pins what detail() reports, not how
	// its columns happen to be padded.
	d := r.detail()
	compact := strings.Join(strings.Fields(d), " ")
	for _, want := range []string{"exit: code 1", "broken_test.pr", "stdout: <empty>"} {
		if !strings.Contains(compact, want) {
			t.Errorf("detail() dropped %q:\n%s", want, d)
		}
	}

	// And a killed child names its deadline rather than reporting a bare code.
	killed := progressRun{args: []string{"test", "x.pr"}, exitCode: -1,
		timedOut: true, budget: progressRunTimeout}
	if d := killed.detail(); !strings.Contains(d, "KILLED") ||
		!strings.Contains(d, progressRunTimeout.String()) {
		t.Errorf("a timed-out run must name its deadline:\n%s", d)
	}
}

// TestProgressRunDeadlineKillsTheChild exercises the backstop itself: the
// deadline fires, the child is killed, and the record says so. The synthetic
// case in TestProgressRunReportsWhyTheChildFailed pins only the rendering; this
// pins the mechanism (context, kill, timedOut) end to end.
//
// Measuring time is the point here rather than a stand-in for a happens-before
// edge, which is the case docs/code-style.md §"Test synchronization" explicitly
// allows — this is a test *of* a timeout. The margin is not fine: the budget is
// milliseconds and the work it interrupts is a cold compile of several seconds,
// in a t.TempDir() that guarantees a cache miss.
func TestProgressRunDeadlineKillsTheChild(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "deadline_test.pr")
	if err := os.WriteFile(src, []byte(progressPassingSource), 0o644); err != nil {
		t.Fatal(err)
	}

	const budget = 50 * time.Millisecond
	r := runProgressWithin(t, bin, nil, budget, "test", src)

	if !r.timedOut {
		t.Fatalf("the deadline did not fire:%s", r.detail())
	}
	// A killed process has no exit code of its own.
	if r.exitCode != -1 {
		t.Errorf("a killed child should report exit -1, got %d:%s", r.exitCode, r.detail())
	}
	// And the report names the budget rather than a bare exit status, so the
	// reader is not left guessing why a run produced nothing.
	d := r.detail()
	if !strings.Contains(d, "KILLED") || !strings.Contains(d, budget.String()) {
		t.Errorf("a killed run must name the budget it exceeded:\n%s", d)
	}
}

// TestQuoteStreamMakesAStreamLegible pins the rendering contract detail() rests
// on: an absent stream is named rather than left blank, and a present one is
// indented so it cannot be misread as the harness's own output.
func TestQuoteStreamMakesAStreamLegible(t *testing.T) {
	t.Parallel()
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
