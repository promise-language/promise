package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// withRenderer swaps the process-wide renderer for one writing into buffers,
// runs fn, and returns (stdout, transient stderr).
func withRenderer(t *testing.T, mode progressMode, fn func()) (out, status string) {
	t.Helper()
	saved := progress
	defer func() { progress = saved }()
	var o, s bytes.Buffer
	progress = newRenderer(mode, &o, &s)
	fn()
	progress.Clear()
	return o.String(), s.String()
}

// childFixture is one fixed result set covering a pass plus every failure kind
// the runner can report, each with its indented context line, terminated by the
// summary the display layer rewrites.
const childFixture = `pass (0.001s) test_add
FAIL (0.003s) test_broken
  panic: assertion failed: expected 3, got 4
pass (0.002s) test_sub
LEAK (0.001s) test_leaky
  leak: 1 allocations not freed
TIMEOUT (0.100s) test_stuck
  timeout: exceeded 60s limit
MEMLIMIT (-) <aborted>
  memory limit: exceeded (test process aborted; subsequent tests not run)
INCOMPLETE (-) test_killer
  incomplete: process exited (status 0) without reporting a result - 2 of 9 tests did not run: test_killer, test_next
pass (0.001s) test_other

2 passed, 1 failed, 1 leaked, 1 timed out (0.423s)
`

// fullGolden is exactly what the display layer printed before T1888 — the
// machine-readable stream a child must keep emitting, byte for byte.
const fullGolden = `pass (0.001s) test_add
FAIL (0.003s) test_broken
  panic: assertion failed: expected 3, got 4
pass (0.002s) test_sub
LEAK (0.001s) test_leaky
  leak: 1 allocations not freed
TIMEOUT (0.100s) test_stuck
  timeout: exceeded 60s limit
MEMLIMIT (-) <aborted>
  memory limit: exceeded (test process aborted; subsequent tests not run)
INCOMPLETE (-) test_killer
  incomplete: process exited (status 0) without reporting a result - 2 of 9 tests did not run: test_killer, test_next
pass (0.001s) test_other

2 passed, 1 failed, 1 leaked, 1 timed out (0.500s)
`

// summaryTail returns the trailing summary block — the blank line before the
// count line and everything after it. That block is what the item's invariant
// pins: it must be byte-identical in every render mode.
func summaryTail(out string) string {
	i := strings.LastIndex(out, "\n\n")
	if i < 0 {
		return out
	}
	return out[i:]
}

// TestPrintChildTestOutput_RenderModes is the T1888 printer test: over one
// fixed result set, quiet drops only the pass lines, TTY leaves only failures
// behind, and every mode prints the same summary.
func TestPrintChildTestOutput_RenderModes(t *testing.T) {
	type rendered struct {
		out, status string
		counts      childOutcomeCounts
		reported    map[string]bool
		sawSummary  bool
	}
	run := func(mode progressMode) rendered {
		var r rendered
		r.out, r.status = withRenderer(t, mode, func() {
			r.counts, r.reported, r.sawSummary = printChildTestOutput(
				childFixture, 500*time.Millisecond, "", childOutputOpts{})
		})
		return r
	}

	full, plain, tty := run(progressFull), run(progressPlain), run(progressTTY)

	// 1. The machine-readable stream is unchanged.
	if full.out != fullGolden {
		t.Errorf("full mode is no longer byte-identical to the pre-T1888 output:\n got:\n%s\nwant:\n%s", full.out, fullGolden)
	}
	if full.status != "" {
		t.Errorf("full mode must not write a transient line; got %q", full.status)
	}

	// 2. Quiet mode emits only the non-pass progress lines, with their context.
	for _, line := range strings.Split(plain.out, "\n") {
		if isPassOutcomeLine(line) {
			t.Errorf("plain mode leaked a passing line: %q", line)
		}
	}
	for _, want := range []string{
		"FAIL (0.003s) test_broken",
		"  panic: assertion failed: expected 3, got 4",
		"LEAK (0.001s) test_leaky",
		"  leak: 1 allocations not freed",
		"TIMEOUT (0.100s) test_stuck",
		"  timeout: exceeded 60s limit",
		"MEMLIMIT (-) <aborted>",
		"  memory limit: exceeded (test process aborted; subsequent tests not run)",
		"INCOMPLETE (-) test_killer",
		"  incomplete: process exited (status 0) without reporting a result",
	} {
		if !strings.Contains(plain.out, want) {
			t.Errorf("plain mode dropped %q:\n%s", want, plain.out)
		}
	}
	if plain.status != "" {
		t.Errorf("plain mode must not write a transient line; got %q", plain.status)
	}

	// 3. TTY rewrites in place; only failures persist, and no ANSI is used.
	if tty.out != plain.out {
		t.Errorf("tty stdout must equal plain stdout:\n tty:\n%s\nplain:\n%s", tty.out, plain.out)
	}
	if !strings.Contains(tty.status, "\r") {
		t.Errorf("tty mode should rewrite a single line in place; got %q", tty.status)
	}
	if strings.Contains(tty.status, "\x1b") {
		t.Errorf("tty mode must not emit ANSI escapes; got %q", tty.status)
	}
	if !strings.Contains(tty.status, "pass (0.001s) test_other") {
		t.Errorf("tty mode should show the most recent pass; got %q", tty.status)
	}

	// 4. THE INVARIANT: the summary block is byte-identical in every mode.
	fs, ps, ts := summaryTail(full.out), summaryTail(plain.out), summaryTail(tty.out)
	if fs != ps || ps != ts {
		t.Errorf("summary differs between modes:\n full: %q\nplain: %q\n  tty: %q", fs, ps, ts)
	}
	if want := "\n\n2 passed, 1 failed, 1 leaked, 1 timed out (0.500s)\n"; fs != want {
		t.Errorf("summary = %q, want %q", fs, want)
	}

	// 5. Counting happens above the print, so it cannot depend on the mode.
	for name, r := range map[string]rendered{"plain": plain, "tty": tty} {
		if r.counts != full.counts {
			t.Errorf("%s counts %+v differ from full %+v", name, r.counts, full.counts)
		}
		if len(r.reported) != len(full.reported) {
			t.Errorf("%s reported %v differs from full %v", name, r.reported, full.reported)
		}
		if r.sawSummary != full.sawSummary {
			t.Errorf("%s sawSummary = %v, want %v", name, r.sawSummary, full.sawSummary)
		}
	}

	// 6. Nothing transient may reach the captured stream, in any mode.
	for name, out := range map[string]string{"full": full.out, "plain": plain.out, "tty": tty.out} {
		if strings.ContainsAny(out, "\r\x1b") {
			t.Errorf("%s mode leaked a carriage return or escape into stdout: %q", name, out)
		}
	}
}

// TestPrintChildTestOutput_TargetSuffixPreserved confirms the cross-target
// annotation still lands on both suppressed and kept outcome lines.
func TestPrintChildTestOutput_TargetSuffixPreserved(t *testing.T) {
	const in = "pass (0.001s) a\nFAIL (0.002s) b\n\n1 passed, 1 failed (0.003s)\n"
	full, _ := withRenderer(t, progressFull, func() {
		printChildTestOutput(in, time.Second, " [wasm32-wasi]", childOutputOpts{})
	})
	if !strings.Contains(full, "pass (0.001s) a [wasm32-wasi]") ||
		!strings.Contains(full, "FAIL (0.002s) b [wasm32-wasi]") {
		t.Errorf("target suffix lost:\n%s", full)
	}
	plain, _ := withRenderer(t, progressPlain, func() {
		printChildTestOutput(in, time.Second, " [wasm32-wasi]", childOutputOpts{})
	})
	if strings.Contains(plain, "pass (") {
		t.Errorf("plain mode leaked a pass line:\n%s", plain)
	}
	if !strings.Contains(plain, "FAIL (0.002s) b [wasm32-wasi]") {
		t.Errorf("plain mode dropped the annotated failure:\n%s", plain)
	}
}

// TestMultiFileProgressLines covers the per-file line shapes the multi-file
// parent prints: only the `pass` forms are suppressible progress.
func TestMultiFileProgressLines(t *testing.T) {
	passing := []string{
		"pass (0.004s) e2e/basics.pr (3 tests)",
		"pass (0.001s) e2e/hello.pr",
		"pass (0.009s) test_and [wasm32-wasi]",
		"PASS (0.003s)",
		"PASS (0.003s) [wasm32-wasi]",
	}
	persisting := []string{
		"FAIL (0.005s) e2e/strings.pr (1/3 failed)",
		"FAIL (0.000s) broken.pr (compilation error)",
		"LEAK (0.001s) test_leaky",
		"TIMEOUT (-) <teardown>",
		"MEMLIMIT (-) <aborted>",
		"INCOMPLETE (-) test_killer",
		"SKIP (excluded) hello",
		"  panic: assertion failed",
		"568 passed, 2 failed, 3 leaked (117 files, 30.810s)",
		"FAILED:",
		"",
	}
	for _, l := range passing {
		if !isPassOutcomeLine(l) {
			t.Errorf("expected a passing progress line: %q", l)
		}
	}
	for _, l := range persisting {
		if isPassOutcomeLine(l) {
			t.Errorf("expected a persistent line: %q", l)
		}
	}

	// And rendering the per-file sequence keeps only the failures on stdout.
	emit := func() {
		progress.Pass("pass (%.3fs) %s (%d tests)%s\n", 0.004, "e2e/basics.pr", 3, "")
		progress.Printf("FAIL (%.3fs) %s (%d/%d failed)%s\n", 0.005, "e2e/strings.pr", 1, 3, "")
		progress.Printf("  %s\n", "test_split")
		progress.Pass("pass (%.3fs) %s%s\n", 0.001, "e2e/hello.pr", "")
		progress.Println()
		progress.Printf("%s (%d files, %.3fs)%s\n", "1 passed, 1 failed", 2, 0.010, "")
	}
	fullOut, _ := withRenderer(t, progressFull, emit)
	plainOut, plainStatus := withRenderer(t, progressPlain, emit)
	ttyOut, ttyStatus := withRenderer(t, progressTTY, emit)

	const wantPlain = "FAIL (0.005s) e2e/strings.pr (1/3 failed)\n  test_split\n\n1 passed, 1 failed (2 files, 0.010s)\n"
	if plainOut != wantPlain {
		t.Errorf("plain per-file output = %q, want %q", plainOut, wantPlain)
	}
	if ttyOut != plainOut {
		t.Errorf("tty per-file stdout must equal plain:\n tty: %q\nplain: %q", ttyOut, plainOut)
	}
	if plainStatus != "" {
		t.Errorf("plain mode wrote a transient line: %q", plainStatus)
	}
	if !strings.Contains(ttyStatus, "\r") || strings.Contains(ttyStatus, "\x1b") {
		t.Errorf("tty transient stream = %q; want carriage returns and no ANSI", ttyStatus)
	}
	// The invariant again, on the multi-file path.
	if summaryTail(fullOut) != summaryTail(plainOut) || summaryTail(plainOut) != summaryTail(ttyOut) {
		t.Errorf("per-file summary differs between modes:\n full: %q\nplain: %q\n  tty: %q",
			summaryTail(fullOut), summaryTail(plainOut), summaryTail(ttyOut))
	}
}

// TestResolveProgressMode covers precedence: explicit flag, then
// PROMISE_PROGRESS, then detection (a pipe under `go test` → plain).
func TestResolveProgressMode(t *testing.T) {
	t.Setenv(progressEnv, "plain")
	if got := resolveProgressMode("full"); got != progressFull {
		t.Errorf("explicit flag should win over env; got %v", got)
	}
	if got := resolveProgressMode(""); got != progressPlain {
		t.Errorf("env should be honoured when no flag is given; got %v", got)
	}
	t.Setenv(progressEnv, "tty")
	if got := resolveProgressMode(""); got != progressTTY {
		t.Errorf("PROMISE_PROGRESS=tty not honoured; got %v", got)
	}
	t.Setenv(progressEnv, "")
	for _, in := range []string{"", "auto", "nonsense"} {
		if got := resolveProgressMode(in); got != progressPlain {
			t.Errorf("resolveProgressMode(%q) = %v, want plain (piped stdout)", in, got)
		}
	}
}

func TestValidProgressMode(t *testing.T) {
	for _, ok := range []string{"auto", "full", "plain", "tty", "TTY", " full "} {
		if !validProgressMode(ok) {
			t.Errorf("%q should be accepted", ok)
		}
	}
	for _, bad := range []string{"", "quiet", "1", "on"} {
		if validProgressMode(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestProgressModeString(t *testing.T) {
	for _, tc := range []struct {
		m    progressMode
		want string
	}{{progressFull, "full"}, {progressPlain, "plain"}, {progressTTY, "tty"}} {
		if got := tc.m.String(); got != tc.want {
			t.Errorf("%d.String() = %q, want %q", tc.m, got, tc.want)
		}
	}
}

// TestRendererClearErasesExactly checks the transient line is erased with the
// right number of spaces, so a shorter follow-up leaves no tail behind.
func TestRendererClearErasesExactly(t *testing.T) {
	var o, s bytes.Buffer
	r := newRenderer(progressTTY, &o, &s)
	r.Pass("abcdef\n")
	r.Clear()
	if got, want := s.String(), "abcdef\r      \r"; got != want {
		t.Errorf("clear sequence = %q, want %q", got, want)
	}
	s.Reset()
	r.Clear()
	if s.Len() != 0 {
		t.Errorf("redundant Clear wrote %q", s.String())
	}
	if o.Len() != 0 {
		t.Errorf("Clear must never write to the persistent stream; got %q", o.String())
	}
}

// TestRendererFlattensControlChars pins the erase arithmetic: Clear() writes
// one space per rune, so any character that advances the cursor by more than
// one column (a tab) would leave a tail the carriage return cannot reach.
func TestRendererFlattensControlChars(t *testing.T) {
	var o, s bytes.Buffer
	r := newRenderer(progressTTY, &o, &s)
	r.Pass("pass (0.001s)\ta\tb\n")
	if got, want := s.String(), "pass (0.001s) a b"; got != want {
		t.Errorf("transient line = %q, want %q", got, want)
	}
}

// TestRendererTruncatesToWidth keeps the transient line inside the terminal, so
// one carriage return can always erase it.
func TestRendererTruncatesToWidth(t *testing.T) {
	t.Setenv("COLUMNS", "30")
	var o, s bytes.Buffer
	r := newRenderer(progressTTY, &o, &s)
	r.Pass("%s\n", strings.Repeat("x", 100))
	if got := len(s.String()); got != 28 {
		t.Errorf("transient line is %d wide, want 28 (COLUMNS-2)", got)
	}
}

// TestBuildChildTestArgs_AlwaysForcesFullProgress is the trap the item names:
// a child's stdout is a pipe and is re-parsed line by line, so it must always
// print the full stream regardless of how the parent renders.
func TestBuildChildTestArgs_AlwaysForcesFullProgress(t *testing.T) {
	saved := progress
	defer func() { progress = saved }()
	for _, mode := range []progressMode{progressFull, progressPlain, progressTTY} {
		progress = newRenderer(mode, &bytes.Buffer{}, &bytes.Buffer{})
		args := buildChildTestArgs(testTimeoutConfig{defaultTimeout: 10 * time.Second, scale: 1.0,
			compileTimeout: 10 * time.Minute}, "", false, false)
		if !hasConsecutive(args, "-progress", "full") {
			t.Errorf("parent mode %v: child args must contain '-progress full'; got %v", mode, args)
		}
	}
}

// TestRendererPrint_ClearsThenWritesVerbatim covers Print, the arm the runner's
// pre-formatted reports use (reportTimedOutRun / reportNeverAliveRun hand it a
// whole multi-line block): it must erase the transient line first and add
// nothing of its own.
func TestRendererPrint_ClearsThenWritesVerbatim(t *testing.T) {
	var o, s bytes.Buffer
	r := newRenderer(progressTTY, &o, &s)
	r.Pass("pass (0.001s) test_a\n")
	const report = "TIMEOUT (-) <teardown>\n  timeout: test process exceeded the 32s batch budget\n\n1 passed, 0 failed, 1 timed out (32.001s)\n"
	r.Print(report)
	if o.String() != report {
		t.Errorf("Print wrote %q, want %q", o.String(), report)
	}
	if !strings.HasSuffix(s.String(), "\r") {
		t.Errorf("Print must erase the transient line first; status = %q", s.String())
	}
	s.Reset()
	r.Clear()
	if s.Len() != 0 {
		t.Errorf("Clear after Print wrote %q", s.String())
	}
}

// TestProgressDefaultsToFull pins the package-level default. Every command
// other than `promise test` prints through this renderer without ever calling
// resolveProgressMode, so a default of anything but full would silently
// suppress output from `promise build`, `run`, `exec` and friends.
func TestProgressDefaultsToFull(t *testing.T) {
	if progress.mode != progressFull {
		t.Errorf("package-level progress mode = %v, want full", progress.mode)
	}
}

// TestResolveProgressMode_DetectsTerminal covers the detection branch: with no
// flag and no env, stdout decides. os.Stdout is swapped for the null device,
// which is a character device and therefore reads as a terminal.
func TestResolveProgressMode_DetectsTerminal(t *testing.T) {
	t.Setenv(progressEnv, "")
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	saved := os.Stdout
	os.Stdout = null
	defer func() { os.Stdout = saved }()

	for _, in := range []string{"", "auto", "nonsense"} {
		if got := resolveProgressMode(in); got != progressTTY {
			t.Errorf("resolveProgressMode(%q) = %v, want tty (stdout is a char device)", in, got)
		}
	}
	// A child is told the mode explicitly, and that must still beat detection —
	// otherwise a `promise test` child inheriting a terminal would go quiet.
	if got := resolveProgressMode("full"); got != progressFull {
		t.Errorf("explicit full lost to detection; got %v", got)
	}
}

// TestIsTerminal_CharDeviceVersusFile covers both answers of the detection
// primitive, including the stat-error path.
func TestIsTerminal_CharDeviceVersusFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("a regular file must not be reported as a terminal")
	}
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if !isTerminal(null) {
		t.Errorf("%s is a character device and should be detected as one", os.DevNull)
	}
	closed, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if runtime.GOOS != "windows" && isTerminal(closed) {
		t.Error("a closed file must not be reported as a terminal")
	}
}

// TestStdWriters_ResolveAtWriteTime pins why the renderer holds writer structs
// rather than the *os.File: the package-level renderer is built at init, long
// before anything can swap os.Stdout, and must still follow the swap.
func TestStdWriters_ResolveAtWriteTime(t *testing.T) {
	dir := t.TempDir()
	outPath, errPath := filepath.Join(dir, "out"), filepath.Join(dir, "err")
	of, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	ef, err := os.Create(errPath)
	if err != nil {
		t.Fatal(err)
	}

	r := newRenderer(progressTTY, stdoutWriter{}, stderrWriter{})
	savedOut, savedErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = of, ef
	r.Pass("pass (0.001s) test_a\n")
	r.Println("FAIL (0.002s) test_b")
	os.Stdout, os.Stderr = savedOut, savedErr
	of.Close()
	ef.Close()

	gotOut, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	gotErr, err := os.ReadFile(errPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotOut) != "FAIL (0.002s) test_b\n" {
		t.Errorf("stdout = %q, want the failure line only", gotOut)
	}
	if !strings.Contains(string(gotErr), "pass (0.001s) test_a") || !strings.Contains(string(gotErr), "\r") {
		t.Errorf("stderr = %q, want the transient line and its erase", gotErr)
	}
}

// TestRendererNilClearIsSafe covers the nil guard in Clear — the exit paths in
// runTestBinary call it unconditionally.
func TestRendererNilClearIsSafe(t *testing.T) {
	var r *renderer
	r.Clear() // must not panic
}

// TestPassOutcomeLine_SummaryAndContextNeverSuppressed is the invariant stated
// negatively: no line of a summary block, and no indented failure context, may
// ever be classified as suppressible progress. If one were, plain mode would
// print a different summary than full mode.
func TestPassOutcomeLine_SummaryAndContextNeverSuppressed(t *testing.T) {
	for _, l := range []string{
		"2 passed, 1 failed, 1 leaked, 1 timed out (0.423s)",
		"0 passed, 0 failed (0.001s)",
		"568 passed, 2 failed, 3 leaked (117 files, 30.810s)",
		"1 passed, 0 failed (1 files, 0.010s) [wasm32-wasi]",
		"FAILED:",
		"STALE ALLOW_LEAKS:",
		"=== Coverage ===",
		"total: 91.2% (100/110 blocks)",
		"  e2e/strings.pr: test_split",
		"    panic: assertion failed",
		"no test files found",
		"passed", // a bare word must not match the "pass " prefix
	} {
		if isPassOutcomeLine(l) {
			t.Errorf("summary/context line classified as suppressible progress: %q", l)
		}
	}
}
