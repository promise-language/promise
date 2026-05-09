package main

import (
	"os"
	"strings"
	"testing"
)

func TestPrintHelp(t *testing.T) {
	// Capture stdout.
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	printHelp()

	w.Close()
	os.Stdout = old

	buf := make([]byte, 64*1024)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	// Check key sections are present.
	for _, want := range []string{
		"Promise",
		"Quick Start",
		"Key Differences",
		"Available Modules",
		"Discovery Commands",
		"promise help",
		"promise doc",
		"promise build",
		"promise run",
		"promise test",
		"print_line",
		"?^",
		"?!",
		"use io;",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("help output missing %q", want)
		}
	}

	// Verify it's plain text (no ANSI escape codes).
	if strings.Contains(output, "\033[") {
		t.Error("help output contains ANSI escape codes — should be plain text")
	}
}
