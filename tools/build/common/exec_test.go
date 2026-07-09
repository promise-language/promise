package common

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// erroringWriter is an io.Writer that always fails. Used to exercise the
// fwd-write error paths in lineFilterWriter.
type erroringWriter struct{}

func (erroringWriter) Write([]byte) (int, error) { return 0, errors.New("forced fwd error") }

// teeStubSource is a tiny cross-platform program used as the subprocess in the
// RunTee* tests. It replaces the `echo`/`sh -c` sample commands, which are not
// executables on a bare Windows PATH — PowerShell has no `sh`/`echo` on PATH, so
// the shell forms failed `bin/test` on windows-amd64 with `exec: "sh":
// executable file not found` (T1225). Args are processed left to right: `-line X`
// prints X and a newline, `-raw X` prints X with no newline, `-exit N` sets the
// exit code (applied after all output).
const teeStubSource = `package main

import (
	"fmt"
	"os"
	"strconv"
)

func main() {
	code := 0
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-line":
			if i+1 < len(args) {
				fmt.Print(args[i+1] + "\n")
				i++
			}
		case "-raw":
			if i+1 < len(args) {
				fmt.Print(args[i+1])
				i++
			}
		case "-exit":
			if i+1 < len(args) {
				code, _ = strconv.Atoi(args[i+1])
				i++
			}
		}
	}
	os.Exit(code)
}
`

// teeStubName is the base name (sans exe suffix) of the compiled tee stub. The
// RunTee* "wraps command name" tests assert the error mentions it.
const teeStubName = "teestub"

var (
	teeStubOnce sync.Once
	teeStubPath string
	teeStubErr  error
)

// teeStub compiles teeStubSource to a host-native executable and returns its
// path. The compile is cached so it happens once per test binary. The temp dir
// is intentionally not removed — the executable must outlive this call so the
// tests can exec it; the OS reclaims the temp dir after the run.
func teeStub(t *testing.T) string {
	t.Helper()
	teeStubOnce.Do(func() {
		dir, err := os.MkdirTemp("", "teestub-")
		if err != nil {
			teeStubErr = err
			return
		}
		src := filepath.Join(dir, "main.go")
		if err := os.WriteFile(src, []byte(teeStubSource), 0o644); err != nil {
			teeStubErr = err
			return
		}
		exe := filepath.Join(dir, teeStubName+ExeSuffix())
		cmd := exec.Command("go", "build", "-o", exe, src)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			teeStubErr = fmt.Errorf("go build tee stub: %w", err)
			return
		}
		teeStubPath = exe
	})
	if teeStubErr != nil {
		t.Fatalf("build tee stub: %v", teeStubErr)
	}
	return teeStubPath
}

func TestRunTee_CapturesOutput(t *testing.T) {
	out, err := RunTee("", teeStub(t), "-line", "hello tee")
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello tee" {
		t.Errorf("RunTee = %q, want %q", out, "hello tee")
	}
}

func TestRunTee_ErrorReturnsCaptured(t *testing.T) {
	// Command that prints then exits non-zero — partial output should be returned.
	out, err := RunTee("", teeStub(t), "-line", "partial", "-exit", "1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if out != "partial" {
		t.Errorf("RunTee output on error = %q, want %q", out, "partial")
	}
}

func TestRunTee_ErrorWrapsCommandName(t *testing.T) {
	_, err := RunTee("", teeStub(t), "-exit", "2")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), teeStubName) {
		t.Errorf("error %q does not mention command name", err.Error())
	}
}

// TestRunTeeFiltered_DropsFilteredLines verifies that runTeeFilteredTo writes
// every stdout line into the captured output but only forwards lines for
// which keep returns true.
func TestRunTeeFiltered_DropsFilteredLines(t *testing.T) {
	var fwd bytes.Buffer
	keep := func(line string) bool { return !strings.HasPrefix(line, "drop:") }
	out, err := runTeeFilteredTo(&fwd, "", teeStub(t), keep, "-line", "drop:a", "-line", "keep:b", "-line", "drop:c")
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
	out, err := runTeeFilteredTo(&fwd, "", teeStub(t), keep, "-line", "drop:a", "-raw", "keep:b")
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
	out, err := runTeeFilteredTo(&fwd, "", teeStub(t), keep, "-line", "partial", "-exit", "1")
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
	out, err := runTeeFilteredTo(erroringWriter{}, "", teeStub(t), keep, "-line", "line1")
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
	out, err := runTeeFilteredTo(erroringWriter{}, "", teeStub(t), keep, "-raw", "partial")
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
	out, err := RunTeeStderrFiltered("", teeStub(t), keep, "-line", "via-public-wrapper")
	if err != nil {
		t.Fatal(err)
	}
	if out != "via-public-wrapper" {
		t.Errorf("captured = %q, want %q", out, "via-public-wrapper")
	}
}
