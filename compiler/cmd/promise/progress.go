package main

// progress.go renders per-test / per-file progress lines at three levels of
// verbosity (T1888).
//
// TWIN FILE: tools/build/common/progress.go is the same design in the build
// tools module. compiler/ and tools/build are separate Go modules with no
// dependency edge between them, so this is duplicated rather than shared —
// change one, change the other.
//
// The one behaviour that varies with mode is Pass. Everything that must
// persist — failures, phase headers, every summary — goes through
// Printf/Print/Println, which are pass-throughs after Clear(). So the bytes on
// stdout are exactly the full-mode bytes minus the passing progress lines, and
// summaries are byte-identical in every mode by construction.

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// progressEnv forces a progress mode from the environment, overriding terminal
// detection (the NO_COLOR-style escape hatch).
const progressEnv = "PROMISE_PROGRESS"

// progressMode selects how passing progress lines are rendered.
type progressMode int

const (
	// progressFull prints every progress line, passing ones included. This is
	// the machine-readable stream a child `promise test` must emit, and what
	// the multi-file parent re-parses.
	progressFull progressMode = iota
	// progressPlain drops passing progress lines entirely — the pipe/CI form.
	progressPlain
	// progressTTY rewrites one transient line in place for passing progress, so
	// only failures are left behind in the scrollback.
	progressTTY
)

// String renders a mode as the spelling accepted by -progress / PROMISE_PROGRESS.
func (m progressMode) String() string {
	switch m {
	case progressPlain:
		return "plain"
	case progressTTY:
		return "tty"
	default:
		return "full"
	}
}

// validProgressMode reports whether s is an accepted -progress value.
func validProgressMode(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "auto", "full", "plain", "tty":
		return true
	}
	return false
}

// resolveProgressMode picks the render mode: an explicit spelling wins, then
// PROMISE_PROGRESS, then terminal detection on stdout. "auto", the empty
// string, and any unrecognized spelling all mean "detect".
func resolveProgressMode(flag string) progressMode {
	src := strings.TrimSpace(flag)
	if src == "" {
		src = strings.TrimSpace(os.Getenv(progressEnv))
	}
	switch strings.ToLower(src) {
	case "full":
		return progressFull
	case "plain":
		return progressPlain
	case "tty":
		return progressTTY
	}
	if isTerminal(os.Stdout) {
		return progressTTY
	}
	return progressPlain
}

// isTerminal reports whether f is attached to a character device.
//
// Deliberately stdlib-only. golang.org/x/term is not in either module's cache,
// so adding it would mean a network fetch on every clone and CI runner for one
// boolean; an x/sys ioctl shim would need build-tagged files in both modules
// for the same. ModeCharDevice answers the only question that matters here.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// stdoutWriter and stderrWriter resolve os.Stdout / os.Stderr at write time
// rather than capturing the *os.File once. Tests swap those variables to
// capture output, and a renderer built at package-init time would otherwise
// keep writing to the real terminal.
type stdoutWriter struct{}

func (stdoutWriter) Write(p []byte) (int, error) { return os.Stdout.Write(p) }

type stderrWriter struct{}

func (stderrWriter) Write(p []byte) (int, error) { return os.Stderr.Write(p) }

// renderer prints progress lines according to a progressMode.
//
// out and status MUST be different streams. bin/verify captures this process's
// *stdout*, so a carriage return written to out would land in the text
// ExtractFailedSection parses, or in a redirected log. The transient line
// therefore goes to stderr, which in a terminal is the same device and renders
// correctly, and under `2>&1 > log` is harmless overwrite noise nobody parses.
type renderer struct {
	mode    progressMode
	out     io.Writer
	status  io.Writer
	lastLen int // rune width of the transient line currently on screen
}

// newRenderer builds a renderer writing persistent output to out and the
// transient in-place line to status.
func newRenderer(mode progressMode, out, status io.Writer) *renderer {
	return &renderer{mode: mode, out: out, status: status}
}

// Clear erases the transient line, if one is on screen. Called before every
// persistent write so nothing is left stranded on a failure or summary line.
// Uses spaces and carriage returns only — no ANSI escapes — so it works on a
// Windows console without VT enabled and can never leave an escape in a log.
func (r *renderer) Clear() {
	if r == nil || r.lastLen == 0 {
		return
	}
	fmt.Fprintf(r.status, "\r%s\r", strings.Repeat(" ", r.lastLen))
	r.lastLen = 0
}

// Printf writes a persistent line.
func (r *renderer) Printf(format string, a ...any) {
	r.Clear()
	fmt.Fprintf(r.out, format, a...)
}

// Print writes persistent text verbatim.
func (r *renderer) Print(a ...any) {
	r.Clear()
	fmt.Fprint(r.out, a...)
}

// Println writes a persistent line with a trailing newline.
func (r *renderer) Println(a ...any) {
	r.Clear()
	fmt.Fprintln(r.out, a...)
}

// Pass renders a *passing* progress line — the only output whose treatment
// depends on the mode.
func (r *renderer) Pass(format string, a ...any) {
	switch r.mode {
	case progressFull:
		r.Printf(format, a...)
	case progressTTY:
		r.transient(fmt.Sprintf(format, a...))
	default: // progressPlain — passing lines carry no information; drop them.
	}
}

// transient rewrites the in-place progress line.
func (r *renderer) transient(s string) {
	s = strings.TrimRight(s, "\r\n")
	s = flattenTransient(s)
	s = truncateRunes(s, progressWidth()-2)
	r.Clear()
	fmt.Fprint(r.status, s)
	r.lastLen = utf8.RuneCountInString(s)
}

// flattenTransient makes a progress line erasable by a fixed count of spaces:
// every control character becomes one space, so the line's rune count equals
// the columns it occupies. A `go test` pass line ("ok  \tpkg\t0.4s") carries
// two tabs, each of which advances the cursor to the next 8-column stop — so
// without this Clear() writes too few spaces and leaves a tail behind, and the
// line can wrap past the width below, which no single carriage return can undo.
func flattenTransient(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// progressWidth is the terminal width to truncate the transient line to. The
// width is only needed to avoid wrapping (a wrapped line cannot be erased by a
// single carriage return), so COLUMNS-or-80 is enough and needs no ioctl.
func progressWidth() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("COLUMNS"))); err == nil && n > 20 {
		return n
	}
	return 80
}

// truncateRunes shortens s to at most n runes.
func truncateRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// progress is the process-wide renderer. It defaults to progressFull so every
// command other than `promise test` prints exactly as it always has; runTest
// replaces it once -progress / PROMISE_PROGRESS / terminal detection resolve.
var progress = newRenderer(progressFull, stdoutWriter{}, stderrWriter{})

// passOutcomeLinePrefixes are the per-test / per-file result lines that report
// a pass. "pass " is the batch-test form (single-file and multi-file alike);
// "PASS (" is the snapshot form, which has no test name.
var passOutcomeLinePrefixes = []string{"pass ", "PASS ("}

// isPassOutcomeLine reports whether a child line is a passing result line.
func isPassOutcomeLine(line string) bool {
	for _, p := range passOutcomeLinePrefixes {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}
