package common

// Coverage for the parts of the T1888 progress plumbing the first round of
// tests left untouched: the emit adapter that keeps the old keep-predicate
// callers working, the renderer's persistent-write and mode accessors, the
// terminal-detection branch of ResolveProgressMode, and the two helpers that
// forward the resolved mode / name the host target.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestKeepEmitter_RestoresNewlineOnlyForCompleteLines pins the contract
// keepEmitter has to honour for the pre-T1888 callers: lineFilterWriter hands
// it a line with the newline already stripped, so a kept complete line must get
// one back and a kept trailing partial line must not — otherwise a command
// whose stdout has no final newline gains one it never wrote.
func TestKeepEmitter_RestoresNewlineOnlyForCompleteLines(t *testing.T) {
	var fwd bytes.Buffer
	emit := keepEmitter(&fwd, func(l string) bool { return !strings.HasPrefix(l, "drop:") })

	for _, tc := range []struct {
		line     string
		complete bool
	}{
		{"keep:a", true},
		{"drop:b", true},
		{"drop:c", false},
		{"keep:d", false},
	} {
		if err := emit(tc.line, tc.complete); err != nil {
			t.Fatalf("emit(%q, %v): %v", tc.line, tc.complete, err)
		}
	}

	if got, want := fwd.String(), "keep:a\nkeep:d"; got != want {
		t.Errorf("forwarded = %q, want %q", got, want)
	}
}

// TestKeepEmitter_PropagatesWriteError confirms a failing fwd surfaces as an
// error rather than being swallowed — lineFilterWriter turns it into the
// "flush:"-prefixed failure exec_test.go asserts on.
func TestKeepEmitter_PropagatesWriteError(t *testing.T) {
	emit := keepEmitter(erroringWriter{}, func(string) bool { return true })
	if err := emit("boom", true); err == nil {
		t.Fatal("expected the fwd write error to propagate")
	}
	// A dropped line never touches fwd, so it cannot fail.
	drop := keepEmitter(erroringWriter{}, func(string) bool { return false })
	if err := drop("boom", true); err != nil {
		t.Errorf("a dropped line must not touch fwd; got %v", err)
	}
}

// TestLineFilterWriter_EmitErrorStopsWriteButKeepsCapture covers the error
// return from Write: the byte count is still the full length (an io.Writer
// contract the exec.Cmd copier relies on) and every byte reached the capture.
func TestLineFilterWriter_EmitErrorStopsWriteButKeepsCapture(t *testing.T) {
	var capture bytes.Buffer
	boom := errors.New("emit failed")
	w := &lineFilterWriter{capture: &capture, emit: func(string, bool) error { return boom }}

	p := []byte("first\nsecond\n")
	n, err := w.Write(p)
	if n != len(p) {
		t.Errorf("Write returned n = %d, want %d", n, len(p))
	}
	if !errors.Is(err, boom) {
		t.Errorf("Write err = %v, want %v", err, boom)
	}
	if capture.String() != string(p) {
		t.Errorf("capture = %q, want %q", capture.String(), string(p))
	}
}

// TestLineFilterWriter_NilCaptureIsAllowed is the shape runInRendered uses:
// nothing needs the captured text, so capture is nil and every byte is only
// classified, never buffered.
func TestLineFilterWriter_NilCaptureIsAllowed(t *testing.T) {
	var lines []string
	w := &lineFilterWriter{emit: func(l string, complete bool) error {
		lines = append(lines, l)
		return nil
	}}
	if _, err := w.Write([]byte("a\nb\ntrail")); err != nil {
		t.Fatal(err)
	}
	if err := w.flush(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(lines, "|"); got != "a|b|trail" {
		t.Errorf("emitted %q, want %q", got, "a|b|trail")
	}
	// flush is idempotent — a second call has nothing pending.
	if err := w.flush(); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Errorf("a second flush re-emitted: %v", lines)
	}
}

// TestRunInRendered_RoutesPassAndFailureLines is the end-to-end shape of the
// `go test` phase: passing package lines are progress (dropped when piped,
// rewritten in place on a TTY) and everything else is printed verbatim, in
// order, on the persistent stream.
func TestRunInRendered_RoutesPassAndFailureLines(t *testing.T) {
	stub := teeStub(t)
	args := []string{
		"-line", "ok  \tpkg/a\t0.1s",
		"-line", "--- FAIL: TestX (0.01s)",
		"-line", "ok  \tpkg/b\t0.2s",
		"-line", "FAIL\tpkg/c\t0.3s",
	}

	render := func(mode ProgressMode) (out, status string) {
		var o, s bytes.Buffer
		r := NewRenderer(mode, &o, &s)
		if err := runInRendered("", r, isGoTestPassLine, stub, args...); err != nil {
			t.Fatalf("runInRendered(%v): %v", mode, err)
		}
		r.Clear()
		return o.String(), s.String()
	}

	fullOut, fullStatus := render(ProgressFull)
	plainOut, plainStatus := render(ProgressPlain)
	ttyOut, ttyStatus := render(ProgressTTY)

	const wantFull = "ok  \tpkg/a\t0.1s\n--- FAIL: TestX (0.01s)\nok  \tpkg/b\t0.2s\nFAIL\tpkg/c\t0.3s\n"
	if fullOut != wantFull {
		t.Errorf("full stdout = %q, want %q", fullOut, wantFull)
	}
	if fullStatus != "" {
		t.Errorf("full mode wrote a transient line: %q", fullStatus)
	}

	const wantPersistent = "--- FAIL: TestX (0.01s)\nFAIL\tpkg/c\t0.3s\n"
	if plainOut != wantPersistent {
		t.Errorf("plain stdout = %q, want %q", plainOut, wantPersistent)
	}
	if plainStatus != "" {
		t.Errorf("plain mode wrote a transient line: %q", plainStatus)
	}
	if ttyOut != plainOut {
		t.Errorf("tty stdout must equal plain:\n tty: %q\nplain: %q", ttyOut, plainOut)
	}
	if !strings.Contains(ttyStatus, "\r") {
		t.Errorf("tty mode should rewrite in place; got %q", ttyStatus)
	}
	for mode, out := range map[string]string{"full": fullOut, "plain": plainOut, "tty": ttyOut} {
		if strings.ContainsAny(out, "\r\x1b") {
			t.Errorf("%s mode leaked a carriage return or escape into stdout: %q", mode, out)
		}
	}
}

// TestRunInRendered_TrailingPartialLine covers the `default:` arm — a final
// line with no newline is printed with Print, not Printf, so no newline the
// command never wrote is invented.
func TestRunInRendered_TrailingPartialLine(t *testing.T) {
	var o, s bytes.Buffer
	r := NewRenderer(ProgressPlain, &o, &s)
	if err := runInRendered("", r, isGoTestPassLine, teeStub(t), "-raw", "no trailing newline"); err != nil {
		t.Fatal(err)
	}
	if got, want := o.String(), "no trailing newline"; got != want {
		t.Errorf("stdout = %q, want %q (no added newline)", got, want)
	}
}

// TestRunInRendered_PropagatesExitFailure keeps the failing-suite signal: a
// non-zero exit is an error even though every line was rendered.
func TestRunInRendered_PropagatesExitFailure(t *testing.T) {
	var o, s bytes.Buffer
	r := NewRenderer(ProgressPlain, &o, &s)
	err := runInRendered("", r, isGoTestPassLine, teeStub(t), "-line", "FAIL\tpkg/a", "-exit", "1")
	if err == nil {
		t.Fatal("expected a non-zero exit to be reported as an error")
	}
	if !strings.Contains(err.Error(), teeStubName) {
		t.Errorf("error %q does not name the command", err)
	}
	if !strings.Contains(o.String(), "FAIL\tpkg/a") {
		t.Errorf("failure line was not printed: %q", o.String())
	}
}

// TestRendererPrint_ClearsThenWritesVerbatim covers Print, the arm used by the
// runner's pre-formatted multi-line reports: it must erase the transient line
// first and add nothing of its own.
func TestRendererPrint_ClearsThenWritesVerbatim(t *testing.T) {
	var o, s bytes.Buffer
	r := NewRenderer(ProgressTTY, &o, &s)
	r.Pass("pass one\n")
	r.Print("TIMEOUT (-) <teardown>\n\n0 passed (1.000s)\n")
	if got, want := o.String(), "TIMEOUT (-) <teardown>\n\n0 passed (1.000s)\n"; got != want {
		t.Errorf("Print wrote %q, want %q", got, want)
	}
	if !strings.HasSuffix(s.String(), "\r") {
		t.Errorf("Print must erase the transient line first; status = %q", s.String())
	}
	// And the transient state is reset, so a later Clear is a no-op.
	s.Reset()
	r.Clear()
	if s.Len() != 0 {
		t.Errorf("Clear after Print wrote %q", s.String())
	}
}

// TestRendererMode_RoundTripsForForwarding is what promiseTestProgressArgs
// depends on: the parent must be able to name its own mode to a child.
func TestRendererMode_RoundTripsForForwarding(t *testing.T) {
	for _, m := range []ProgressMode{ProgressFull, ProgressPlain, ProgressTTY} {
		r := NewRenderer(m, &bytes.Buffer{}, &bytes.Buffer{})
		if got := r.Mode(); got != m {
			t.Errorf("Mode() = %v, want %v", got, m)
		}
		if got := ResolveProgressMode(r.Mode().String()); got != m {
			t.Errorf("round trip through %q = %v, want %v", r.Mode().String(), got, m)
		}
	}
}

// TestPromiseTestProgressArgs_ForwardsResolvedMode is the trap the item names:
// a child's stdout is always a pipe, so the outermost process must state the
// mode explicitly rather than leaving the child to sniff.
func TestPromiseTestProgressArgs_ForwardsResolvedMode(t *testing.T) {
	args := promiseTestProgressArgs()
	if len(args) != 2 || args[0] != "-progress" {
		t.Fatalf("promiseTestProgressArgs() = %v, want [-progress <mode>]", args)
	}
	if args[1] != Progress().Mode().String() {
		t.Errorf("forwarded mode %q, want %q", args[1], Progress().Mode().String())
	}
	if !validSpelling(args[1]) {
		t.Errorf("forwarded mode %q is not a spelling `promise test -progress` accepts", args[1])
	}
	// And under `go test` stdout is a pipe, so detection lands on plain — the
	// mode a CI runner and the `bin/do` flow step actually forward.
	t.Setenv(ProgressEnv, "")
	if got := ResolveProgressMode(""); got != ProgressPlain {
		t.Errorf("detection under a pipe = %v, want plain", got)
	}
}

// validSpelling mirrors the compiler's -progress flag validation.
func validSpelling(s string) bool {
	switch s {
	case "auto", "full", "plain", "tty":
		return true
	}
	return false
}

// TestProgressIsStableAcrossCalls confirms the process-wide renderer is
// resolved once, so every phase of a run renders the same way.
func TestProgressIsStableAcrossCalls(t *testing.T) {
	if Progress() != Progress() {
		t.Error("Progress() returned two different renderers")
	}
	// The env is read only on the first resolution — a later change must not
	// silently switch modes mid-run.
	before := Progress().Mode()
	t.Setenv(ProgressEnv, "full")
	if got := Progress().Mode(); got != before {
		t.Errorf("Progress() mode changed mid-run from %v to %v", before, got)
	}
}

// TestIsTerminal_CharDeviceVersusFile covers both answers of the detection
// primitive: a regular file is not a terminal, the null device is a character
// device and therefore is one.
func TestIsTerminal_CharDeviceVersusFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if IsTerminal(f) {
		t.Error("a regular file must not be reported as a terminal")
	}

	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if !IsTerminal(null) {
		t.Errorf("%s is a character device and should be detected as one", os.DevNull)
	}

	// A closed file cannot be stat'ed — the error path must answer "not a
	// terminal" rather than panicking.
	closed, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if runtime.GOOS != "windows" && IsTerminal(closed) {
		t.Error("a closed file must not be reported as a terminal")
	}
}

// TestResolveProgressMode_DetectsTerminal covers the branch no other test
// reaches: with no flag and no env, detection on stdout decides. os.Stdout is
// swapped for the null device, which is a character device.
func TestResolveProgressMode_DetectsTerminal(t *testing.T) {
	t.Setenv(ProgressEnv, "")
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	saved := os.Stdout
	os.Stdout = null
	defer func() { os.Stdout = saved }()

	for _, in := range []string{"", "auto", "nonsense"} {
		if got := ResolveProgressMode(in); got != ProgressTTY {
			t.Errorf("ResolveProgressMode(%q) = %v, want tty (stdout is a char device)", in, got)
		}
	}
	// An explicit spelling still overrides detection.
	if got := ResolveProgressMode("plain"); got != ProgressPlain {
		t.Errorf("explicit plain lost to detection; got %v", got)
	}
}

// TestStdWriters_ResolveAtWriteTime pins why the renderer holds writer structs
// rather than the *os.File: a renderer built before a test swaps os.Stdout must
// still write where the test is looking.
func TestStdWriters_ResolveAtWriteTime(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out")
	errPath := filepath.Join(dir, "err")
	of, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	ef, err := os.Create(errPath)
	if err != nil {
		t.Fatal(err)
	}

	r := NewRenderer(ProgressTTY, stdoutWriter{}, stderrWriter{})
	savedOut, savedErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = of, ef
	r.Pass("transient\n")
	r.Println("persistent")
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
	if string(gotOut) != "persistent\n" {
		t.Errorf("stdout = %q, want %q", gotOut, "persistent\n")
	}
	if !strings.Contains(string(gotErr), "transient") || !strings.Contains(string(gotErr), "\r") {
		t.Errorf("stderr = %q, want the transient line and its erase", gotErr)
	}
}

// TestHostTargetName covers the label the verify summary prints: lowercase OS,
// a dash, the Go arch.
func TestHostTargetName(t *testing.T) {
	got := hostTargetName()
	want := strings.ToLower(runtime.GOOS) + "-" + runtime.GOARCH
	if got != want {
		t.Errorf("hostTargetName() = %q, want %q", got, want)
	}
	if strings.ToLower(got) != got {
		t.Errorf("hostTargetName() = %q, want it lowercase", got)
	}
}
