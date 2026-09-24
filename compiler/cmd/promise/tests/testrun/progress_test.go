package testrun

// End-to-end coverage for T1888's render modes. The in-process printer tests in
// package main pin the display layer over a fixed result set; these pin what
// the built binary actually writes to stdout and stderr, which is the only
// place the whole chain — flag parsing, mode resolution, the multi-file
// parent's own `pass` lines, and the `-progress full` it forces on its children
// — is observable at once.
//
// Every child runs through clitest.RunOK or clitest.RunFailing, which bound it
// and check the exit status before any assertion reads a stream, and every
// message that could otherwise print an empty stream ends with Result.Detail().
// These tests assert on what the child *renders*, and a child that never ran
// renders nothing: a message built from stdout alone then reports "the mode is
// wrong" when the truth is "there was no run", and throws away the exit status
// and stderr — the only two places the cause could be (T2119).
//
// clitest.Result keeps the two streams apart, which this file needs for more
// than diagnosis: tty mode's transient line goes to stderr and the assertions
// below prove it never reaches stdout. CombinedOutput would interleave the two
// and there would be nothing left to prove.

import (
	"os"
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

// summaryRe matches the multi-file grand summary and the single-file batch
// summary alike — the block the item's invariant pins as mode-independent.
var progressSummaryRe = regexp.MustCompile(`(?m)^\d+ passed, \d+ failed.*$`)

func summaryLines(t *testing.T, r clitest.Result) string {
	t.Helper()
	m := progressSummaryRe.FindAllString(r.Stdout, -1)
	if len(m) == 0 {
		t.Fatalf("no summary line in output:%s", r.Detail())
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
	dir := clitest.TempDir(t)
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
	full := clitest.RunFailing(t, bin, nil, "test", "-progress", "full", dir)
	plain := clitest.RunFailing(t, bin, nil, "test", "-progress", "plain", dir)
	tty := clitest.RunFailing(t, bin, nil, "test", "-progress", "tty", dir)

	// 1. `full` is the machine-readable stream: the passing file is named.
	if got := passLines(full.Stdout); len(got) != 1 || !strings.Contains(got[0], "alpha_test.pr") {
		t.Errorf("full mode pass lines = %v, want one naming alpha_test.pr%s", got, full.Detail())
	}

	// 2. Neither quiet mode prints a passing line on stdout.
	for name, r := range map[string]clitest.Result{"plain": plain, "tty": tty} {
		if got := passLines(r.Stdout); len(got) != 0 {
			t.Errorf("%s mode leaked passing lines on stdout: %v%s", name, got, r.Detail())
		}
	}

	// 3. tty stdout is byte-identical to plain stdout apart from what legitimately
	//    differs between two runs of the same suite, so compare with that elided.
	if got, want := elideVolatile(tty.Stdout), elideVolatile(plain.Stdout); got != want {
		t.Errorf("tty stdout differs from plain:\n tty:\n%s\nplain:\n%s\ntty run:%s\nplain run:%s",
			got, want, tty.Detail(), plain.Detail())
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
		t.Errorf("summary = %q, want it to report 3 passed, 1 failed over 2 files%s", fs, full.Detail())
	}

	// 5. Failures persist verbatim in every mode, with their FAILED: section.
	for name, r := range map[string]clitest.Result{"full": full, "plain": plain, "tty": tty} {
		for _, want := range []string{"FAIL (", "beta_test.pr", "beta_broken", "FAILED:"} {
			if !strings.Contains(r.Stdout, want) {
				t.Errorf("%s mode dropped %q from stdout:%s", name, want, r.Detail())
			}
		}
	}

	// 6. Nothing transient may reach stdout in any mode — RunTee captures it and
	//    ExtractFailedSection re-parses it.
	for name, r := range map[string]clitest.Result{"full": full, "plain": plain, "tty": tty} {
		if strings.ContainsAny(r.Stdout, "\r\x1b") {
			t.Errorf("%s mode leaked a carriage return or escape into stdout:\n%q", name, r.Stdout)
		}
	}

	// 7. Only tty writes a transient line, and it uses no ANSI.
	if !strings.Contains(tty.Stderr, "\r") {
		t.Errorf("tty mode wrote no in-place update on stderr; got %q", tty.Stderr)
	}
	if strings.Contains(tty.Stderr, "\x1b") {
		t.Errorf("tty mode used ANSI escapes: %q", tty.Stderr)
	}
	if strings.Contains(tty.Stderr, "\n") {
		t.Errorf("the transient line must stay on one row; stderr = %q", tty.Stderr)
	}
	// And it is erased before the process exits, so a shell prompt lands clean.
	if !strings.HasSuffix(tty.Stderr, "\r") {
		t.Errorf("tty mode left the progress line on screen; stderr = %q", tty.Stderr)
	}
	for name, r := range map[string]clitest.Result{"full": full, "plain": plain} {
		if strings.Contains(r.Stderr, "\r") {
			t.Errorf("%s mode wrote an in-place update to stderr: %q", name, r.Stderr)
		}
	}
}

// elideTimings replaces every "(1.234s)" with a fixed token, so two runs of the
// same suite can be compared for everything except how long they took.
var timingRe = regexp.MustCompile(`-?\d+\.\d+s`)

func elideTimings(s string) string { return timingRe.ReplaceAllString(s, "Ts") }

// storeCostRe matches the line a run prints when it cost the content-addressed
// store something (T2143).
var storeCostRe = regexp.MustCompile(`(?m)^store: .*\n?`)

// elideVolatile is elideTimings plus that line. The store cost is volatile by
// design: of the three runs below the first materializes whatever the home
// still owed and the other two cost nothing, which is the fact the line exists
// to report and says nothing at all about render mode.
//
// On a warm home the line never appears, so leaving it in compared clean and
// reddened only on a cold one — a flake that reaches whoever runs with --clean.
func elideVolatile(s string) string { return storeCostRe.ReplaceAllString(elideTimings(s), "") }

// TestProgressModes_SingleFile covers the other printer: the per-test lines a
// single-file run streams through printChildTestOutput.
func TestProgressModes_SingleFile(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	dir := clitest.TempDir(t)
	src := filepath.Join(dir, "single_test.pr")
	if err := os.WriteFile(src, []byte(progressMixedSource), 0o644); err != nil {
		t.Fatal(err)
	}

	// The mixed fixture has a deliberately failing test, so each mode exits 1.
	full := clitest.RunFailing(t, bin, nil, "test", "-progress", "full", src)
	plain := clitest.RunFailing(t, bin, nil, "test", "-progress", "plain", src)
	tty := clitest.RunFailing(t, bin, nil, "test", "-progress", "tty", src)

	if got := passLines(full.Stdout); len(got) != 1 || !strings.Contains(got[0], "beta_ok") {
		t.Errorf("full mode pass lines = %v, want one naming beta_ok%s", got, full.Detail())
	}
	for name, r := range map[string]clitest.Result{"plain": plain, "tty": tty} {
		if got := passLines(r.Stdout); len(got) != 0 {
			t.Errorf("%s mode leaked passing lines: %v%s", name, got, r.Detail())
		}
		if !strings.Contains(r.Stdout, "FAIL (") || !strings.Contains(r.Stdout, "beta_broken") {
			t.Errorf("%s mode dropped the failure line:%s", name, r.Detail())
		}
		// The panic context under a FAIL is not a progress line and must stay.
		if !strings.Contains(r.Stdout, "deliberate failure") {
			t.Errorf("%s mode dropped the assertion context:%s", name, r.Detail())
		}
	}
	fs := elideTimings(summaryLines(t, full))
	ps := elideTimings(summaryLines(t, plain))
	ts := elideTimings(summaryLines(t, tty))
	if fs != ps || ps != ts {
		t.Errorf("single-file summary differs between modes:\n full: %q\nplain: %q\n  tty: %q", fs, ps, ts)
	}
	if !strings.Contains(tty.Stderr, "\r") {
		t.Errorf("tty mode wrote no in-place update; stderr = %q", tty.Stderr)
	}
}

// TestProgressModes_SnapshotPassSuppressed covers the second passing spelling:
// a snapshot test prints "PASS (…)" with no name, which is progress just the
// same, while its summary is not.
func TestProgressModes_SnapshotPassSuppressed(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	dir := clitest.TempDir(t)
	src := filepath.Join(dir, "snap_test.pr")
	if err := os.WriteFile(src, []byte(progressSnapshotSource), 0o644); err != nil {
		t.Fatal(err)
	}

	// The snapshot test passes, so both runs must exit 0.
	full := clitest.RunOK(t, bin, nil, "test", "-progress", "full", src)
	plain := clitest.RunOK(t, bin, nil, "test", "-progress", "plain", src)
	if got := passLines(full.Stdout); len(got) != 1 || !strings.HasPrefix(got[0], "PASS (") {
		t.Errorf("full mode pass lines = %v, want one \"PASS (…)\"%s", got, full.Detail())
	}
	if got := passLines(plain.Stdout); len(got) != 0 {
		t.Errorf("plain mode leaked the snapshot pass line: %v%s", got, plain.Detail())
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
	dir := clitest.TempDir(t)
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
	envFull := clitest.RunOK(t, bin, []string{"PROMISE_PROGRESS=full"}, "test", src)
	if len(passLines(envFull.Stdout)) == 0 {
		t.Errorf("PROMISE_PROGRESS=full did not restore pass lines:%s", envFull.Detail())
	}
	// Flag beats env in both directions.
	flagPlain := clitest.RunOK(t, bin, []string{"PROMISE_PROGRESS=full"}, "test", "-progress", "plain", src)
	if got := passLines(flagPlain.Stdout); len(got) != 0 {
		t.Errorf("-progress plain lost to PROMISE_PROGRESS=full: %v%s", got, flagPlain.Detail())
	}
	flagFull := clitest.RunOK(t, bin, []string{"PROMISE_PROGRESS=plain"}, "test", "-progress", "full", src)
	if len(passLines(flagFull.Stdout)) == 0 {
		t.Errorf("-progress full lost to PROMISE_PROGRESS=plain:%s", flagFull.Detail())
	}
	// The default with a piped stdout and no env is the quiet form.
	auto := clitest.RunOK(t, bin, []string{"PROMISE_PROGRESS="}, "test", src)
	if got := passLines(auto.Stdout); len(got) != 0 {
		t.Errorf("the piped default printed pass lines: %v%s", got, auto.Detail())
	}
	// An unrecognized env value falls through to detection rather than failing.
	garbage := clitest.RunOK(t, bin, []string{"PROMISE_PROGRESS=quiet"}, "test", src)
	if got := passLines(garbage.Stdout); len(got) != 0 {
		t.Errorf("garbage env should fall back to detection (plain); got %v%s", got, garbage.Detail())
	}
	// Every mode agrees on the summary.
	want := elideTimings(summaryLines(t, envFull))
	for name, r := range map[string]clitest.Result{
		"flagPlain": flagPlain, "flagFull": flagFull, "auto": auto, "garbage": garbage,
	} {
		if got := elideTimings(summaryLines(t, r)); got != want {
			t.Errorf("%s summary = %q, want %q%s", name, got, want, r.Detail())
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
		// A usage error is exit 1; clitest.RunFailing is what rejects it being
		// accepted (exit 0) or the child dying on the way to saying so.
		r := clitest.RunFailing(t, bin, nil, "test", "-progress", bad, "nonexistent_test.pr")
		if !strings.Contains(r.Stderr, "-progress requires one of: auto, full, plain, tty") {
			t.Errorf("-progress %q: want the accepted-spellings message:%s", bad, r.Detail())
		}
	}
	// "auto" is accepted and means "detect" — under a pipe, that is plain.
	dir := clitest.TempDir(t)
	src := filepath.Join(dir, "auto_test.pr")
	if err := os.WriteFile(src, []byte(progressPassingSource), 0o644); err != nil {
		t.Fatal(err)
	}
	r := clitest.RunOK(t, bin, []string{"PROMISE_PROGRESS="}, "test", "-progress", "auto", src)
	if got := passLines(r.Stdout); len(got) != 0 {
		t.Errorf("-progress auto under a pipe printed pass lines: %v%s", got, r.Detail())
	}
	// And the usage line advertises the flag. No target is a usage error: exit 1.
	usage := clitest.RunFailing(t, bin, nil, "test")
	if !strings.Contains(usage.Stderr, "-progress auto|full|plain|tty") {
		t.Errorf("usage does not document -progress:%s", usage.Detail())
	}
}

// TestProgressDoesNotAffectOtherCommands guards the blast radius: -progress is a
// `promise test` concept, and every other command keeps printing exactly as it
// did. `promise run` on a program with output is the cheapest witness.
func TestProgressDoesNotAffectOtherCommands(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	dir := clitest.TempDir(t)
	src := filepath.Join(dir, "hello.pr")
	if err := os.WriteFile(src, []byte("main() {\n  print_line(\"hello\");\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, env := range [][]string{nil, {"PROMISE_PROGRESS=plain"}, {"PROMISE_PROGRESS=tty"}} {
		r := clitest.RunOK(t, bin, env, "run", src)
		if strings.TrimSpace(r.Stdout) != "hello" {
			t.Errorf("env %v: stdout = %q, want \"hello\"%s", env, r.Stdout, r.Detail())
		}
		// The rewriter's signature is a bare CR (return to line start) or an
		// ANSI escape. A CR that is part of a CRLF line ending is not that —
		// it is what the Windows PAL writes for every newline — so line
		// endings are normalized away before looking for one.
		if bare := strings.ReplaceAll(r.Stdout, "\r\n", "\n"); strings.ContainsAny(bare, "\r\x1b") {
			t.Errorf("env %v: program output was rewritten: %q", env, r.Stdout)
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
	dir := clitest.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "json_test.pr"), []byte(progressMixedSource), 0o644); err != nil {
		t.Fatal(err)
	}

	// The mixed fixture has a deliberately failing test, so each run exits 1.
	base := clitest.RunFailing(t, bin, []string{"PROMISE_PROGRESS="}, "test", "--json", dir)
	for _, env := range [][]string{{"PROMISE_PROGRESS=plain"}, {"PROMISE_PROGRESS=tty"}, {"PROMISE_PROGRESS=full"}} {
		r := clitest.RunFailing(t, bin, env, "test", "--json", dir)
		if got, want := elideJSONVolatile(r.Stdout), elideJSONVolatile(base.Stdout); got != want {
			t.Errorf("env %v changed the JSONL stream:\n got: %s\nwant: %s%s", env, got, want, r.Detail())
		}
		if strings.ContainsAny(r.Stdout, "\r\x1b") {
			t.Errorf("env %v leaked a carriage return or escape into the JSONL stream: %q", env, r.Stdout)
		}
	}
	// Sanity: the stream really does carry both outcomes, so an all-empty
	// comparison above could not have passed vacuously.
	for _, want := range []string{`"beta_ok"`, `"beta_broken"`, `"pass"`, `"fail"`} {
		if !strings.Contains(base.Stdout, want) {
			t.Errorf("JSONL stream is missing %s:%s", want, base.Detail())
		}
	}
}

// elideJSONVolatile blanks what legitimately differs between two runs of the
// same suite: durations, absolute paths, and the store-cost record.
//
// The store record (T2143) is volatile by design — it reports what the run cost
// the content-addressed store, so the first run of a pair materializes a
// toolchain and the second, now warm, costs nothing. That is the fact it exists
// to report, and it says nothing about render mode, which is what this file
// asserts on.
var jsonVolatileRe = regexp.MustCompile(`"(elapsed|duration_ms|duration|file)":\s*("[^"]*"|[0-9.]+)`)

var jsonStoreRecordRe = regexp.MustCompile(`(?m)^\{"kind":"cas".*\}$`)

func elideJSONVolatile(s string) string {
	s = jsonStoreRecordRe.ReplaceAllString(s, `{"kind":"cas":X}`)
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
	dir := clitest.TempDir(t)
	src := filepath.Join(dir, "broken_test.pr")
	if err := os.WriteFile(src, []byte(progressBrokenSource), 0o644); err != nil {
		t.Fatal(err)
	}

	// The premise: a child that cannot compile writes nothing at all to stdout,
	// so an assertion that reads stdout alone has no evidence to report.
	r := clitest.RunFailing(t, bin, nil, "test", src)
	if r.Stdout != "" {
		t.Errorf("a compile failure should leave stdout empty; got %q", r.Stdout)
	}
	if !strings.Contains(r.Stderr, "broken_test.pr") {
		t.Errorf("the compile error must reach stderr:%s", r.Detail())
	}

	// So the record carries the exit status and stderr, and says "empty" out
	// loud rather than printing a blank line the reader has to interpret.
	// Whitespace is collapsed first: this pins what detail() reports, not how
	// its columns happen to be padded.
	d := r.Detail()
	compact := strings.Join(strings.Fields(d), " ")
	for _, want := range []string{"exit: code 1", "broken_test.pr", "stdout: <empty>"} {
		if !strings.Contains(compact, want) {
			t.Errorf("detail() dropped %q:\n%s", want, d)
		}
	}

	// And a killed child names its deadline rather than reporting a bare code.
	killed := clitest.Result{Args: []string{"test", "x.pr"}, ExitCode: -1,
		TimedOut: true, Budget: clitest.MinBudget}
	if d := killed.Detail(); !strings.Contains(d, "KILLED") ||
		!strings.Contains(d, clitest.MinBudget.String()) {
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
// in a clitest.TempDir(t) that guarantees a cache miss.
func TestProgressRunDeadlineKillsTheChild(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	dir := clitest.TempDir(t)
	src := filepath.Join(dir, "deadline_test.pr")
	if err := os.WriteFile(src, []byte(progressPassingSource), 0o644); err != nil {
		t.Fatal(err)
	}

	const budget = 50 * time.Millisecond
	r := clitest.RunWithin(t, bin, nil, budget, "test", src)

	if !r.TimedOut {
		t.Fatalf("the deadline did not fire:%s", r.Detail())
	}
	// A killed process has no exit code of its own.
	if r.ExitCode != -1 {
		t.Errorf("a killed child should report exit -1, got %d:%s", r.ExitCode, r.Detail())
	}
	// And the report names the budget rather than a bare exit status, so the
	// reader is not left guessing why a run produced nothing.
	d := r.Detail()
	if !strings.Contains(d, "KILLED") || !strings.Contains(d, budget.String()) {
		t.Errorf("a killed run must name the budget it exceeded:\n%s", d)
	}
}
