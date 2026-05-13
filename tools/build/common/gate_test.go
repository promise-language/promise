package common

import (
	"strings"
	"testing"
)

// TestRunGate_NoArgs verifies the usage error when no subcommand is given.
func TestRunGate_NoArgs(t *testing.T) {
	err := RunGate("", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error %q does not contain 'usage'", err.Error())
	}
}

// TestRunGate_UnknownSubcommand verifies the error for an unrecognized subcommand.
func TestRunGate_UnknownSubcommand(t *testing.T) {
	err := RunGate("", []string{"bogus"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("error %q does not contain 'unknown subcommand'", err.Error())
	}
}

// TestRunGate_TestBadFlag verifies that unrecognized flags are rejected early.
func TestRunGate_TestBadFlag(t *testing.T) {
	err := RunGate("", []string{"test", "--bogus"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error %q does not contain 'usage'", err.Error())
	}
}

// TestRunTeeStderr_CapturesOutput verifies that RunTeeStderr captures stdout.
func TestRunTeeStderr_CapturesOutput(t *testing.T) {
	out, err := RunTeeStderr("", "echo", "hello tee stderr")
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello tee stderr" {
		t.Errorf("RunTeeStderr = %q, want %q", out, "hello tee stderr")
	}
}

// TestRunTeeStderr_ErrorReturnsCaptured verifies partial output is returned on error.
func TestRunTeeStderr_ErrorReturnsCaptured(t *testing.T) {
	out, err := RunTeeStderr("", "sh", "-c", "echo partial; exit 1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if out != "partial" {
		t.Errorf("RunTeeStderr output on error = %q, want %q", out, "partial")
	}
}

// TestRunTeeStderr_ErrorWrapsCommandName verifies the error message includes the command.
func TestRunTeeStderr_ErrorWrapsCommandName(t *testing.T) {
	_, err := RunTeeStderr("", "sh", "-c", "exit 2")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "sh") {
		t.Errorf("error %q does not mention command name", err.Error())
	}
}
