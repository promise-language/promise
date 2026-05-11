package common

import (
	"strings"
	"testing"
)

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
