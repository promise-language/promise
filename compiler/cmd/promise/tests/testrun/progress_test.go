package testrun

// End-to-end coverage for T1888's render modes. The in-process printer tests in
// package main pin the display layer over a fixed result set; these pin what
// the built binary actually writes to stdout and stderr, which is the only
// place the whole chain — flag parsing, mode resolution, the multi-file
// parent's own `pass` lines, and the `-progress full` it forces on its children
// — is observable at once.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

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

// progressRun is one invocation's separated streams.
type progressRun struct {
	stdout, stderr string
	err            error
}

// runProgress invokes the built compiler with stdout and stderr captured
// separately — CombinedOutput would interleave the transient line into the
// stream this test has to prove stays clean.
func runProgress(t *testing.T, bin string, env []string, args ...string) progressRun {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	err := cmd.Run()
	return progressRun{stdout: out.String(), stderr: errb.String(), err: err}
}

// summaryRe matches the multi-file grand summary and the single-file batch
// summary alike — the block the item's invariant pins as mode-independent.
var progressSummaryRe = regexp.MustCompile(`(?m)^\d+ passed, \d+ failed.*$`)

func summaryLines(t *testing.T, out string) string {
	t.Helper()
	m := progressSummaryRe.FindAllString(out, -1)
	if len(m) == 0 {
		t.Fatalf("no summary line in output:\n%s", out)
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

	full := runProgress(t, bin, nil, "test", "-progress", "full", dir)
	plain := runProgress(t, bin, nil, "test", "-progress", "plain", dir)
	tty := runProgress(t, bin, nil, "test", "-progress", "tty", dir)

	for name, r := range map[string]progressRun{"full": full, "plain": plain, "tty": tty} {
		if r.err == nil {
			t.Fatalf("%s: expected a non-zero exit for the failing test\n%s", name, r.stdout)
		}
	}

	// 1. `full` is the machine-readable stream: the passing file is named.
	if got := passLines(full.stdout); len(got) != 1 || !strings.Contains(got[0], "alpha_test.pr") {
		t.Errorf("full mode pass lines = %v, want one naming alpha_test.pr\n%s", got, full.stdout)
	}

	// 2. Neither quiet mode prints a passing line on stdout.
	for name, r := range map[string]progressRun{"plain": plain, "tty": tty} {
		if got := passLines(r.stdout); len(got) != 0 {
			t.Errorf("%s mode leaked passing lines on stdout: %v", name, got)
		}
	}

	// 3. tty stdout is byte-identical to plain stdout apart from the timings
	//    baked into each line, so compare the lines with timings elided.
	if got, want := elideTimings(tty.stdout), elideTimings(plain.stdout); got != want {
		t.Errorf("tty stdout differs from plain:\n tty:\n%s\nplain:\n%s", got, want)
	}

	// 4. THE INVARIANT: the summary is the same in every mode.
	fs := elideTimings(summaryLines(t, full.stdout))
	ps := elideTimings(summaryLines(t, plain.stdout))
	ts := elideTimings(summaryLines(t, tty.stdout))
	if fs != ps || ps != ts {
		t.Errorf("summary differs between modes:\n full: %q\nplain: %q\n  tty: %q", fs, ps, ts)
	}
	// alpha contributes two passes, beta one pass and one failure.
	if !strings.Contains(fs, "3 passed, 1 failed (2 files") {
		t.Errorf("summary = %q, want it to report 3 passed, 1 failed over 2 files", fs)
	}

	// 5. Failures persist verbatim in every mode, with their FAILED: section.
	for name, r := range map[string]progressRun{"full": full, "plain": plain, "tty": tty} {
		for _, want := range []string{"FAIL (", "beta_test.pr", "beta_broken", "FAILED:"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("%s mode dropped %q from stdout:\n%s", name, want, r.stdout)
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

	full := runProgress(t, bin, nil, "test", "-progress", "full", src)
	plain := runProgress(t, bin, nil, "test", "-progress", "plain", src)
	tty := runProgress(t, bin, nil, "test", "-progress", "tty", src)

	if got := passLines(full.stdout); len(got) != 1 || !strings.Contains(got[0], "beta_ok") {
		t.Errorf("full mode pass lines = %v, want one naming beta_ok\n%s", got, full.stdout)
	}
	for name, r := range map[string]progressRun{"plain": plain, "tty": tty} {
		if got := passLines(r.stdout); len(got) != 0 {
			t.Errorf("%s mode leaked passing lines: %v", name, got)
		}
		if !strings.Contains(r.stdout, "FAIL (") || !strings.Contains(r.stdout, "beta_broken") {
			t.Errorf("%s mode dropped the failure line:\n%s", name, r.stdout)
		}
		// The panic context under a FAIL is not a progress line and must stay.
		if !strings.Contains(r.stdout, "deliberate failure") {
			t.Errorf("%s mode dropped the assertion context:\n%s", name, r.stdout)
		}
	}
	fs := elideTimings(summaryLines(t, full.stdout))
	ps := elideTimings(summaryLines(t, plain.stdout))
	ts := elideTimings(summaryLines(t, tty.stdout))
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

	full := runProgress(t, bin, nil, "test", "-progress", "full", src)
	plain := runProgress(t, bin, nil, "test", "-progress", "plain", src)
	if full.err != nil {
		t.Fatalf("snapshot test should pass: %v\n%s\n%s", full.err, full.stdout, full.stderr)
	}
	if plain.err != nil {
		t.Fatalf("snapshot test should pass: %v\n%s\n%s", plain.err, plain.stdout, plain.stderr)
	}
	if got := passLines(full.stdout); len(got) != 1 || !strings.HasPrefix(got[0], "PASS (") {
		t.Errorf("full mode pass lines = %v, want one \"PASS (…)\"\n%s", got, full.stdout)
	}
	if got := passLines(plain.stdout); len(got) != 0 {
		t.Errorf("plain mode leaked the snapshot pass line: %v", got)
	}
	if a, b := elideTimings(summaryLines(t, full.stdout)), elideTimings(summaryLines(t, plain.stdout)); a != b {
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

	// Env alone: full turns the pass lines back on even though stdout is a pipe.
	envFull := runProgress(t, bin, []string{"PROMISE_PROGRESS=full"}, "test", src)
	if len(passLines(envFull.stdout)) == 0 {
		t.Errorf("PROMISE_PROGRESS=full did not restore pass lines:\n%s", envFull.stdout)
	}
	// Flag beats env in both directions.
	flagPlain := runProgress(t, bin, []string{"PROMISE_PROGRESS=full"}, "test", "-progress", "plain", src)
	if got := passLines(flagPlain.stdout); len(got) != 0 {
		t.Errorf("-progress plain lost to PROMISE_PROGRESS=full: %v", got)
	}
	flagFull := runProgress(t, bin, []string{"PROMISE_PROGRESS=plain"}, "test", "-progress", "full", src)
	if len(passLines(flagFull.stdout)) == 0 {
		t.Errorf("-progress full lost to PROMISE_PROGRESS=plain:\n%s", flagFull.stdout)
	}
	// The default with a piped stdout and no env is the quiet form.
	auto := runProgress(t, bin, []string{"PROMISE_PROGRESS="}, "test", src)
	if got := passLines(auto.stdout); len(got) != 0 {
		t.Errorf("the piped default printed pass lines: %v", got)
	}
	// An unrecognized env value falls through to detection rather than failing.
	garbage := runProgress(t, bin, []string{"PROMISE_PROGRESS=quiet"}, "test", src)
	if garbage.err != nil {
		t.Errorf("an unrecognized PROMISE_PROGRESS must not be fatal: %v\n%s", garbage.err, garbage.stderr)
	}
	if got := passLines(garbage.stdout); len(got) != 0 {
		t.Errorf("garbage env should fall back to detection (plain); got %v", got)
	}
	// Every mode agrees on the summary.
	want := elideTimings(summaryLines(t, envFull.stdout))
	for name, r := range map[string]progressRun{
		"flagPlain": flagPlain, "flagFull": flagFull, "auto": auto, "garbage": garbage,
	} {
		if got := elideTimings(summaryLines(t, r.stdout)); got != want {
			t.Errorf("%s summary = %q, want %q", name, got, want)
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
		r := runProgress(t, bin, nil, "test", "-progress", bad, "nonexistent_test.pr")
		if r.err == nil {
			t.Errorf("-progress %q was accepted", bad)
			continue
		}
		if !strings.Contains(r.stderr, "-progress requires one of: auto, full, plain, tty") {
			t.Errorf("-progress %q: stderr = %q, want the accepted-spellings message", bad, r.stderr)
		}
	}
	// "auto" is accepted and means "detect" — under a pipe, that is plain.
	dir := t.TempDir()
	src := filepath.Join(dir, "auto_test.pr")
	if err := os.WriteFile(src, []byte(progressPassingSource), 0o644); err != nil {
		t.Fatal(err)
	}
	r := runProgress(t, bin, []string{"PROMISE_PROGRESS="}, "test", "-progress", "auto", src)
	if r.err != nil {
		t.Fatalf("-progress auto should be accepted: %v\n%s", r.err, r.stderr)
	}
	if got := passLines(r.stdout); len(got) != 0 {
		t.Errorf("-progress auto under a pipe printed pass lines: %v", got)
	}
	// And the usage line advertises the flag.
	usage := runProgress(t, bin, nil, "test")
	if !strings.Contains(usage.stderr, "-progress auto|full|plain|tty") {
		t.Errorf("usage does not document -progress: %q", usage.stderr)
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
		r := runProgress(t, bin, env, "run", src)
		if r.err != nil {
			t.Fatalf("env %v: %v\n%s\n%s", env, r.err, r.stdout, r.stderr)
		}
		if strings.TrimSpace(r.stdout) != "hello" {
			t.Errorf("env %v: stdout = %q, want \"hello\"", env, r.stdout)
		}
		if strings.ContainsAny(r.stdout, "\r\x1b") {
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

	base := runProgress(t, bin, []string{"PROMISE_PROGRESS="}, "test", "--json", dir)
	for _, env := range [][]string{{"PROMISE_PROGRESS=plain"}, {"PROMISE_PROGRESS=tty"}, {"PROMISE_PROGRESS=full"}} {
		r := runProgress(t, bin, env, "test", "--json", dir)
		if got, want := elideJSONVolatile(r.stdout), elideJSONVolatile(base.stdout); got != want {
			t.Errorf("env %v changed the JSONL stream:\n got: %s\nwant: %s", env, got, want)
		}
		if strings.ContainsAny(r.stdout, "\r\x1b") {
			t.Errorf("env %v leaked a carriage return or escape into the JSONL stream: %q", env, r.stdout)
		}
	}
	// Sanity: the stream really does carry both outcomes, so an all-empty
	// comparison above could not have passed vacuously.
	for _, want := range []string{`"beta_ok"`, `"beta_broken"`, `"pass"`, `"fail"`} {
		if !strings.Contains(base.stdout, want) {
			t.Errorf("JSONL stream is missing %s:\n%s", want, base.stdout)
		}
	}
}

// elideJSONVolatile blanks the per-record fields that legitimately differ
// between two runs of the same suite: durations and absolute paths.
var jsonVolatileRe = regexp.MustCompile(`"(elapsed|duration_ms|duration|file)":\s*("[^"]*"|[0-9.]+)`)

func elideJSONVolatile(s string) string {
	return jsonVolatileRe.ReplaceAllString(s, `"$1":X`)
}
