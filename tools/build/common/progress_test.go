package common

import (
	"bytes"
	"strings"
	"testing"
)

// TestResolveProgressMode_FlagWinsOverEnv locks the precedence the item
// requires: an explicit spelling beats PROMISE_PROGRESS, which beats detection.
func TestResolveProgressMode_FlagWinsOverEnv(t *testing.T) {
	t.Setenv(ProgressEnv, "plain")
	if got := ResolveProgressMode("full"); got != ProgressFull {
		t.Errorf("explicit flag should win over env; got %v", got)
	}
	if got := ResolveProgressMode(""); got != ProgressPlain {
		t.Errorf("env should be honoured when no flag is given; got %v", got)
	}
}

// TestResolveProgressMode_EnvSpellings covers each accepted spelling, and that
// an unrecognized one falls through to detection rather than erroring.
func TestResolveProgressMode_EnvSpellings(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want ProgressMode
	}{
		{"full", ProgressFull},
		{"FULL", ProgressFull},
		{" plain ", ProgressPlain},
		{"tty", ProgressTTY},
	} {
		if got := ResolveProgressMode(tc.in); got != tc.want {
			t.Errorf("ResolveProgressMode(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// "auto" and garbage both mean "detect". Under `go test` stdout is a pipe,
	// so detection must land on plain.
	t.Setenv(ProgressEnv, "")
	for _, in := range []string{"", "auto", "nonsense"} {
		if got := ResolveProgressMode(in); got != ProgressPlain {
			t.Errorf("ResolveProgressMode(%q) = %v, want plain (piped stdout)", in, got)
		}
	}
}

func TestProgressModeString(t *testing.T) {
	for _, tc := range []struct {
		m    ProgressMode
		want string
	}{{ProgressFull, "full"}, {ProgressPlain, "plain"}, {ProgressTTY, "tty"}} {
		if got := tc.m.String(); got != tc.want {
			t.Errorf("%d.String() = %q, want %q", tc.m, got, tc.want)
		}
	}
}

// TestIsGoTestPassLine covers the classifier that decides which `go test` lines
// are progress noise. The tab is load-bearing: a test's own stdout line that
// begins "ok " must not be swallowed.
func TestIsGoTestPassLine(t *testing.T) {
	pass := []string{
		"ok  \tgithub.com/promise/compiler/internal/sema\t0.412s",
		"ok  \tgithub.com/promise/compiler/internal/codegen\t(cached)",
		"?   \tgithub.com/promise/compiler/internal/ast\t[no test files]",
	}
	keep := []string{
		"--- FAIL: TestThing (0.01s)",
		"FAIL\tgithub.com/promise/compiler/internal/sema\t0.412s",
		"FAIL",
		"ok so far, everything checks out", // a test's own stdout
		"okay",
		"? maybe",
		"",
	}
	for _, l := range pass {
		if !isGoTestPassLine(l) {
			t.Errorf("expected pass line: %q", l)
		}
	}
	for _, l := range keep {
		if isGoTestPassLine(l) {
			t.Errorf("expected kept line: %q", l)
		}
	}
}

// TestRendererModes asserts the core invariant: persistent output is identical
// across modes, only Pass lines differ, and nothing transient ever reaches the
// persistent stream.
func TestRendererModes(t *testing.T) {
	render := func(mode ProgressMode) (out, status string) {
		var o, s bytes.Buffer
		r := NewRenderer(mode, &o, &s)
		r.Pass("ok  \tpkg/a\t0.1s\n")
		r.Pass("ok  \tpkg/b\t0.2s\n")
		r.Printf("--- FAIL: TestX\n")
		r.Pass("ok  \tpkg/c\t0.3s\n")
		r.Println("FAIL\tpkg/d\t0.4s")
		r.Clear()
		return o.String(), s.String()
	}

	fullOut, fullStatus := render(ProgressFull)
	plainOut, plainStatus := render(ProgressPlain)
	ttyOut, ttyStatus := render(ProgressTTY)

	if fullOut != "ok  \tpkg/a\t0.1s\nok  \tpkg/b\t0.2s\n--- FAIL: TestX\nok  \tpkg/c\t0.3s\nFAIL\tpkg/d\t0.4s\n" {
		t.Errorf("full mode dropped or reordered output:\n%q", fullOut)
	}
	if fullStatus != "" {
		t.Errorf("full mode must not write to the transient stream; got %q", fullStatus)
	}

	const wantPersistent = "--- FAIL: TestX\nFAIL\tpkg/d\t0.4s\n"
	if plainOut != wantPersistent {
		t.Errorf("plain stdout = %q, want %q", plainOut, wantPersistent)
	}
	if plainStatus != "" {
		t.Errorf("plain mode must not write to the transient stream; got %q", plainStatus)
	}

	// THE INVARIANT: persistent output is byte-identical between plain and tty.
	if ttyOut != plainOut {
		t.Errorf("tty stdout must equal plain stdout:\n tty: %q\nplain: %q", ttyOut, plainOut)
	}
	if !strings.Contains(ttyStatus, "\r") {
		t.Errorf("tty mode should rewrite in place; got %q", ttyStatus)
	}
	if strings.Contains(ttyStatus, "\x1b") {
		t.Errorf("tty mode must not emit ANSI escapes; got %q", ttyStatus)
	}
	// Nothing transient may reach the captured/persistent stream.
	for mode, out := range map[string]string{"full": fullOut, "plain": plainOut, "tty": ttyOut} {
		if strings.ContainsAny(out, "\r\x1b") {
			t.Errorf("%s mode leaked a carriage return or escape into stdout: %q", mode, out)
		}
	}
}

// TestRendererClearErasesExactly checks the transient line is erased with the
// right number of spaces, so a shorter follow-up never leaves a tail behind.
func TestRendererClearErasesExactly(t *testing.T) {
	var o, s bytes.Buffer
	r := NewRenderer(ProgressTTY, &o, &s)
	r.Pass("abcdef\n")
	r.Clear()
	if got, want := s.String(), "abcdef\r      \r"; got != want {
		t.Errorf("clear sequence = %q, want %q", got, want)
	}
	// A second Clear with nothing on screen is a no-op.
	s.Reset()
	r.Clear()
	if s.Len() != 0 {
		t.Errorf("redundant Clear wrote %q", s.String())
	}
}

// TestRendererFlattensTabs pins the erase arithmetic against `go test` pass
// lines, which carry two tabs: Clear() writes one space per rune, so a tab left
// intact would advance the cursor further than the erase covers.
func TestRendererFlattensTabs(t *testing.T) {
	var o, s bytes.Buffer
	r := NewRenderer(ProgressTTY, &o, &s)
	r.Pass("ok  \tpkg/a\t0.1s\n")
	line := s.String()
	if strings.ContainsAny(line, "\t") {
		t.Errorf("transient line kept a tab: %q", line)
	}
	if line != "ok   pkg/a 0.1s" {
		t.Errorf("transient line = %q, want %q", line, "ok   pkg/a 0.1s")
	}
	s.Reset()
	r.Clear()
	if got, want := s.String(), "\r"+strings.Repeat(" ", len(line))+"\r"; got != want {
		t.Errorf("clear sequence = %q, want %q", got, want)
	}
}

// TestRendererTruncatesToWidth keeps the transient line inside the terminal, so
// a single carriage return can always erase it.
func TestRendererTruncatesToWidth(t *testing.T) {
	t.Setenv("COLUMNS", "30")
	var o, s bytes.Buffer
	r := NewRenderer(ProgressTTY, &o, &s)
	r.Pass("%s\n", strings.Repeat("x", 100))
	if got := len(s.String()); got != 28 {
		t.Errorf("transient line is %d wide, want 28 (COLUMNS-2)", got)
	}
}

// TestLineFilterWriterSplitsAcrossWrites confirms a line split across two
// Write calls is classified once, as a whole line.
func TestLineFilterWriterSplitsAcrossWrites(t *testing.T) {
	var got []string
	var capture bytes.Buffer
	w := &lineFilterWriter{capture: &capture, emit: func(line string, complete bool) error {
		got = append(got, line)
		return nil
	}}
	w.Write([]byte("ok  \tpkg/"))
	w.Write([]byte("a\t0.1s\nFAIL"))
	w.Write([]byte("\tpkg/b\n"))
	w.Write([]byte("trailing"))
	if err := w.flush(); err != nil {
		t.Fatal(err)
	}
	want := []string{"ok  \tpkg/a\t0.1s", "FAIL\tpkg/b", "trailing"}
	if len(got) != len(want) {
		t.Fatalf("emitted %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
	if capture.String() != "ok  \tpkg/a\t0.1s\nFAIL\tpkg/b\ntrailing" {
		t.Errorf("capture lost bytes: %q", capture.String())
	}
}
