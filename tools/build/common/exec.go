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
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
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

// RunTee executes a command in the given directory, streaming stdout to
// os.Stdout in real-time while also capturing and returning it as a string.
// Stderr remains connected to os.Stderr.
func RunTee(dir, name string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
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
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(buf.String()), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(buf.String()), nil
}

// lineFilterWriter buffers stdout into lines and forwards each complete line
// to fwd only when keep(line) returns true. Every byte written is also copied
// verbatim into capture, regardless of filter result.
type lineFilterWriter struct {
	fwd     io.Writer
	capture *bytes.Buffer
	keep    func(string) bool
	pending []byte
}

func (w *lineFilterWriter) Write(p []byte) (int, error) {
	w.capture.Write(p)
	w.pending = append(w.pending, p...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			break
		}
		line := w.pending[:i]
		if w.keep(string(line)) {
			if _, err := w.fwd.Write(w.pending[:i+1]); err != nil {
				return len(p), err
			}
		}
		w.pending = w.pending[i+1:]
	}
	return len(p), nil
}

// flush drains any trailing partial line through the filter.
func (w *lineFilterWriter) flush() error {
	if len(w.pending) == 0 {
		return nil
	}
	if w.keep(string(w.pending)) {
		if _, err := w.fwd.Write(w.pending); err != nil {
			return err
		}
	}
	w.pending = nil
	return nil
}

// runTeeFilteredTo runs cmd in dir; captures stdout to the returned string.
// Each complete line of stdout for which keep(line) returns true is also
// written to fwd. Lines for which keep returns false are dropped from fwd
// but still appear in the captured output. Stderr is connected to os.Stderr.
func runTeeFilteredTo(fwd io.Writer, dir, name string, keep func(string) bool, args ...string) (string, error) {
	var capture bytes.Buffer
	w := &lineFilterWriter{fwd: fwd, capture: &capture, keep: keep}
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = w
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	flushErr := w.flush()
	if runErr != nil {
		return strings.TrimSpace(capture.String()), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), runErr)
	}
	if flushErr != nil {
		return strings.TrimSpace(capture.String()), fmt.Errorf("%s %s: flush: %w", name, strings.Join(args, " "), flushErr)
	}
	return strings.TrimSpace(capture.String()), nil
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
