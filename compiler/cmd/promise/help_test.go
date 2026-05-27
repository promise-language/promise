package main

import (
	"os"
	"regexp"
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

// TestUsageCoversAllCommands ensures every top-level subcommand dispatched in
// main()'s `switch cmd { ... }` is documented in usage(). Prevents future
// commands from being silently omitted (T0691).
func TestUsageCoversAllCommands(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	// Isolate the body of `switch cmd { ... default: ... }` so we don't pick up
	// `case` clauses from nested switches inside other functions.
	srcStr := string(src)
	start := strings.Index(srcStr, "switch cmd {")
	if start < 0 {
		t.Fatal("could not find `switch cmd {` in main.go")
	}
	rest := srcStr[start:]
	end := strings.Index(rest, "\n\tdefault:")
	if end < 0 {
		t.Fatal("could not find `default:` terminator for cmd switch")
	}
	switchBody := rest[:end]

	caseRe := regexp.MustCompile(`case "([a-z][a-z0-9-]*)":`)
	matches := caseRe.FindAllStringSubmatch(switchBody, -1)
	if len(matches) == 0 {
		t.Fatal("no `case \"<cmd>\":` clauses extracted from cmd switch")
	}

	// Capture usage() output (writes to os.Stderr).
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	usage()
	w.Close()
	os.Stderr = oldStderr

	buf := make([]byte, 64*1024)
	n, _ := r.Read(buf)
	usageOut := string(buf[:n])

	// Each command must appear as the first token on some line in the
	// `Commands:` block, e.g. `  bind      Generate ...`.
	for _, m := range matches {
		cmd := m[1]
		wantPrefix := "  " + cmd + " "
		found := false
		for _, line := range strings.Split(usageOut, "\n") {
			if strings.HasPrefix(line, wantPrefix) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("usage() output missing entry for command %q (dispatched in main.go but not listed)", cmd)
		}
	}
}
