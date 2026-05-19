package common

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// erroringWriter is an io.Writer that always fails. Used to exercise the
// fwd-write error paths in lineFilterWriter.
type erroringWriter struct{}

func (erroringWriter) Write([]byte) (int, error) { return 0, errors.New("forced fwd error") }

func TestRunTee_CapturesOutput(t *testing.T) {
	out, err := RunTee("", "echo", "hello tee")
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello tee" {
		t.Errorf("RunTee = %q, want %q", out, "hello tee")
	}
}

func TestRunTee_ErrorReturnsCaptured(t *testing.T) {
	// Command that prints then exits non-zero — partial output should be returned.
	out, err := RunTee("", "sh", "-c", "echo partial; exit 1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if out != "partial" {
		t.Errorf("RunTee output on error = %q, want %q", out, "partial")
	}
}

func TestRunTee_ErrorWrapsCommandName(t *testing.T) {
	_, err := RunTee("", "sh", "-c", "exit 2")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "sh") {
		t.Errorf("error %q does not mention command name", err.Error())
	}
}

// TestRunTeeFiltered_DropsFilteredLines verifies that runTeeFilteredTo writes
// every stdout line into the captured output but only forwards lines for
// which keep returns true.
func TestRunTeeFiltered_DropsFilteredLines(t *testing.T) {
	var fwd bytes.Buffer
	keep := func(line string) bool { return !strings.HasPrefix(line, "drop:") }
	out, err := runTeeFilteredTo(&fwd, "", "sh", keep, "-c", "echo drop:a; echo keep:b; echo drop:c")
	if err != nil {
		t.Fatal(err)
	}
	want := "drop:a\nkeep:b\ndrop:c"
	if out != want {
		t.Errorf("captured = %q, want %q", out, want)
	}
	if got := fwd.String(); got != "keep:b\n" {
		t.Errorf("forwarded = %q, want %q", got, "keep:b\n")
	}
}

// TestRunTeeFiltered_FlushesPartialLine verifies the trailing partial line
// (no newline) is run through the filter on flush.
func TestRunTeeFiltered_FlushesPartialLine(t *testing.T) {
	var fwd bytes.Buffer
	keep := func(line string) bool { return !strings.HasPrefix(line, "drop:") }
	out, err := runTeeFilteredTo(&fwd, "", "sh", keep, "-c", "printf 'drop:a\\nkeep:b'")
	if err != nil {
		t.Fatal(err)
	}
	if out != "drop:a\nkeep:b" {
		t.Errorf("captured = %q, want %q", out, "drop:a\nkeep:b")
	}
	if got := fwd.String(); got != "keep:b" {
		t.Errorf("forwarded = %q, want %q", got, "keep:b")
	}
}

// TestRunTeeFiltered_ErrorReturnsCaptured verifies partial output is returned
// when the command exits non-zero.
func TestRunTeeFiltered_ErrorReturnsCaptured(t *testing.T) {
	var fwd bytes.Buffer
	keep := func(string) bool { return true }
	out, err := runTeeFilteredTo(&fwd, "", "sh", keep, "-c", "echo partial; exit 1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if out != "partial" {
		t.Errorf("captured on error = %q, want %q", out, "partial")
	}
}

// TestRunTeeFiltered_FwdWriteErrorMidLine verifies that a fwd.Write failure on
// a complete line is propagated as an error from runTeeFilteredTo. The captured
// buffer still receives the bytes regardless.
func TestRunTeeFiltered_FwdWriteErrorMidLine(t *testing.T) {
	keep := func(string) bool { return true }
	out, err := runTeeFilteredTo(erroringWriter{}, "", "sh", keep, "-c", "echo line1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "forced fwd error") {
		t.Errorf("error %q does not wrap forced fwd error", err.Error())
	}
	if out != "line1" {
		t.Errorf("captured = %q, want %q", out, "line1")
	}
}

// TestRunTeeFiltered_FlushFwdWriteError verifies that a fwd.Write failure
// during flush of the trailing partial line is reported with a "flush:" prefix.
// Triggered by a command whose stdout has no trailing newline so flush is the
// path that touches fwd.
func TestRunTeeFiltered_FlushFwdWriteError(t *testing.T) {
	keep := func(string) bool { return true }
	out, err := runTeeFilteredTo(erroringWriter{}, "", "sh", keep, "-c", "printf 'partial'")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "flush:") {
		t.Errorf("error %q does not contain flush: prefix", err.Error())
	}
	if !strings.Contains(err.Error(), "forced fwd error") {
		t.Errorf("error %q does not wrap forced fwd error", err.Error())
	}
	if out != "partial" {
		t.Errorf("captured = %q, want %q", out, "partial")
	}
}

// TestRunTeeStderrFiltered_DelegatesAndCaptures smoke-tests the public wrapper.
// We can't easily inspect what reaches os.Stderr from a unit test, but the
// captured-stdout return value confirms the writer chain is wired up.
func TestRunTeeStderrFiltered_DelegatesAndCaptures(t *testing.T) {
	keep := func(string) bool { return true }
	out, err := RunTeeStderrFiltered("", "sh", keep, "-c", "echo via-public-wrapper")
	if err != nil {
		t.Fatal(err)
	}
	if out != "via-public-wrapper" {
		t.Errorf("captured = %q, want %q", out, "via-public-wrapper")
	}
}
