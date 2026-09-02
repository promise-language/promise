package common

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Run executes a command with stdout/stderr connected to the terminal.
// Returns an error if the command fails.
func Run(name string, args ...string) error {
	return RunIn("", name, args...)
}

// RunIn executes a command in the given directory.
func RunIn(dir string, name string, args ...string) error {
	return runIn(dir, name, args, func() string {
		return name + " " + strings.Join(args, " ")
	})
}

// RunInBrief is RunIn for commands with very large argument lists: on failure the
// error names the command and the argument count instead of joining every argument
// into the message (a 700-file `promise format` batch would otherwise produce a
// ~30,000-character error that buries the real cause).
func RunInBrief(dir string, name string, args ...string) error {
	return runIn(dir, name, args, func() string {
		return fmt.Sprintf("%s (%d args)", name, len(args))
	})
}

// runIn is the shared body of RunIn/RunInBrief. describe is called only on
// failure, so callers never pay to render the command line on the happy path.
func runIn(dir string, name string, args []string, describe func() string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := runTracked(cmd); err != nil {
		return fmt.Errorf("%s: %w", describe(), err)
	}
	return nil
}

// RunOutput executes a command and returns its stdout as a string.
// Stderr is connected to the terminal.
func RunOutput(name string, args ...string) (string, error) {
	return RunOutputIn("", name, args...)
}

// RunOutputIn executes a command in the given directory and returns stdout.
func RunOutputIn(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// RunOutputQuietIn executes a command in the given directory returning stdout,
// with stderr discarded. Use for probes whose failure is expected and handled
// by the caller (e.g. `git cat-file -s :path` on a staged deletion, which fails
// with a `fatal:` diagnostic) so git's message does not leak to the terminal
// during an otherwise-clean commit.
func RunOutputQuietIn(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// RunBytesIn executes a command in the given directory and returns its raw
// stdout bytes, untrimmed. Use for binary output such as `git cat-file blob`,
// where RunOutputIn's TrimSpace would corrupt the content. Stderr is connected
// to the terminal.
func RunBytesIn(dir string, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

// RunTee executes a command in the given directory, streaming stdout to
// os.Stdout in real-time while also capturing and returning it as a string.
// Stderr remains connected to os.Stderr.
func RunTee(dir, name string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
	cmd.Stderr = os.Stderr
	if err := runTracked(cmd); err != nil {
		return strings.TrimSpace(buf.String()), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(buf.String()), nil
}

// RunTeeStderr executes a command in the given directory, streaming stdout to
// os.Stderr in real-time while also capturing and returning it as a string.
// Keeps os.Stdout clean for structured output (e.g. JSON). Stderr remains
// connected to os.Stderr.
func RunTeeStderr(dir, name string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = io.MultiWriter(os.Stderr, &buf)
	cmd.Stderr = os.Stderr
	if err := runTracked(cmd); err != nil {
		return strings.TrimSpace(buf.String()), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(buf.String()), nil
}

// RunCaptureStdout runs a command in dir, capturing stdout (returned even when
// the command exits non-zero) while leaving stderr connected to os.Stderr.
// Unlike RunOutputIn it preserves the captured stdout on failure — needed for
// `promise test --json`, which streams JSONL records on stdout AND exits
// non-zero when any test fails. Stdout is NOT teed anywhere, so structured
// output stays clean.
func RunCaptureStdout(dir, name string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = &buf
	cmd.Stderr = os.Stderr
	err := runTracked(cmd)
	return buf.String(), err
}

// lineFilterWriter splits stdout into lines and hands each one to emit, which
// decides whether and how it is displayed. complete is false only for a
// trailing partial line at flush time (no newline was ever written). Every byte
// written is also copied verbatim into capture when capture is non-nil,
// regardless of what emit does.
type lineFilterWriter struct {
	capture *bytes.Buffer
	emit    func(line string, complete bool) error
	pending []byte
}

func (w *lineFilterWriter) Write(p []byte) (int, error) {
	if w.capture != nil {
		w.capture.Write(p)
	}
	w.pending = append(w.pending, p...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			break
		}
		if err := w.emit(string(w.pending[:i]), true); err != nil {
			return len(p), err
		}
		w.pending = w.pending[i+1:]
	}
	return len(p), nil
}

// flush drains any trailing partial line through emit.
func (w *lineFilterWriter) flush() error {
	if len(w.pending) == 0 {
		return nil
	}
	err := w.emit(string(w.pending), false)
	w.pending = nil
	return err
}

// keepEmitter adapts a keep predicate to lineFilterWriter's emit: kept lines
// are written to fwd verbatim (with their newline when they had one), dropped
// ones go nowhere.
func keepEmitter(fwd io.Writer, keep func(string) bool) func(string, bool) error {
	return func(line string, complete bool) error {
		if !keep(line) {
			return nil
		}
		if complete {
			line += "\n"
		}
		_, err := fwd.Write([]byte(line))
		return err
	}
}

// runTeeFilteredTo runs cmd in dir; captures stdout to the returned string.
// Each complete line of stdout for which keep(line) returns true is also
// written to fwd. Lines for which keep returns false are dropped from fwd
// but still appear in the captured output. Stderr is connected to os.Stderr.
func runTeeFilteredTo(fwd io.Writer, dir, name string, keep func(string) bool, args ...string) (string, error) {
	var capture bytes.Buffer
	return runLineFiltered(&capture, keepEmitter(fwd, keep), dir, name, args...)
}

// runLineFiltered runs a command in dir with its stdout split into lines and
// routed through emit. capture, when non-nil, receives every stdout byte
// verbatim and is returned (trimmed). Stderr is connected to os.Stderr.
func runLineFiltered(capture *bytes.Buffer, emit func(string, bool) error, dir, name string, args ...string) (string, error) {
	w := &lineFilterWriter{capture: capture, emit: emit}
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = w
	cmd.Stderr = os.Stderr
	runErr := runTracked(cmd)
	flushErr := w.flush()
	captured := ""
	if capture != nil {
		captured = strings.TrimSpace(capture.String())
	}
	if runErr != nil {
		return captured, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), runErr)
	}
	if flushErr != nil {
		return captured, fmt.Errorf("%s %s: flush: %w", name, strings.Join(args, " "), flushErr)
	}
	return captured, nil
}

// runInRendered is RunIn with its stdout routed through a Renderer: lines for
// which isPass reports true are rendered as passing progress (dropped when
// piped, rewritten in place on a TTY), and every other line — failures, panics,
// summaries — is printed verbatim (T1888).
func runInRendered(dir string, r *Renderer, isPass func(string) bool, name string, args ...string) error {
	emit := func(line string, complete bool) error {
		switch {
		case isPass(line):
			r.Pass("%s\n", line)
		case complete:
			r.Printf("%s\n", line)
		default:
			r.Print(line)
		}
		return nil
	}
	_, err := runLineFiltered(nil, emit, dir, name, args...)
	return err
}

// RunTeeStderrFiltered is like RunTeeStderr but only forwards stdout lines
// for which keep(line) returns true to os.Stderr. The full stdout is still
// captured and returned for callers that need to parse it.
func RunTeeStderrFiltered(dir, name string, keep func(string) bool, args ...string) (string, error) {
	return runTeeFilteredTo(os.Stderr, dir, name, keep, args...)
}

// RunSilent executes a command discarding stdout/stderr. Returns error on failure.
func RunSilent(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// RunOutputCombined executes a command capturing both stdout and stderr.
// Use for commands like "java -version" that write to stderr.
func RunOutputCombined(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// RunOutputQuiet executes a command capturing stdout and discarding stderr.
// Use for probing commands where stderr noise is expected.
func RunOutputQuiet(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Which finds an executable in PATH, returning its full path or empty string.
func Which(name string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return path
}

// Exists returns true if the given path exists.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
