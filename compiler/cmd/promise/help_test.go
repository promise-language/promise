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
		"promise init",
		"promise build",
		// Project/directory build forms (T0925) — distinct from the single-file
		// `promise build file.pr` line, which "promise build" alone also matches.
		"promise build <dir>",
		"current directory",
		"promise run",
		"promise test",
		"promise targets",
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

// dispatchedCommands extracts the set of top-level subcommands handled by
// main()'s `switch cmd { ... default: ... }`. Shared by the help-coverage tests
// so both directions of drift protection read from the same source of truth.
func dispatchedCommands(t *testing.T) map[string]bool {
	t.Helper()
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

	// A single `case` can list multiple commands (e.g. `case "fetch", "warm":`).
	caseRe := regexp.MustCompile(`case ((?:"[a-z][a-z0-9-]*",?\s*)+):`)
	cmdRe := regexp.MustCompile(`"([a-z][a-z0-9-]*)"`)
	cmds := map[string]bool{}
	for _, m := range caseRe.FindAllStringSubmatch(switchBody, -1) {
		for _, c := range cmdRe.FindAllStringSubmatch(m[1], -1) {
			cmds[c[1]] = true
		}
	}
	if len(cmds) == 0 {
		t.Fatal("no `case \"<cmd>\":` clauses extracted from cmd switch")
	}
	return cmds
}

// TestUsageCoversAllCommands ensures every top-level subcommand dispatched in
// main()'s `switch cmd { ... }` is documented in usage(). Prevents future
// commands from being silently omitted (T0691).
func TestUsageCoversAllCommands(t *testing.T) {
	cmds := dispatchedCommands(t)

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

	// Each command must appear either as the first token on some line in the
	// `Commands:` block (e.g. `  bind      Generate ...`) or as an inline alias
	// (e.g. `fetch ... (alias: warm)`).
	for cmd := range cmds {
		wantPrefix := "  " + cmd + " "
		aliasMention := "(alias: " + cmd + ")"
		found := strings.Contains(usageOut, aliasMention)
		if !found {
			for _, line := range strings.Split(usageOut, "\n") {
				if strings.HasPrefix(line, wantPrefix) {
					found = true
					break
				}
			}
		}
		if !found {
			t.Errorf("usage() output missing entry for command %q (dispatched in main.go but not listed)", cmd)
		}
	}
}

// TestHelpCommandsAreDispatched verifies the reverse direction: every
// `promise <cmd>` token mentioned in printHelp() corresponds to a real command
// dispatched in main.go. printHelp() is an intentionally curated subset (it
// omits internal commands like ast/check/emit-ir), so we don't require it to
// document every command — only that it never documents a fictional or renamed
// one (T0925).
func TestHelpCommandsAreDispatched(t *testing.T) {
	dispatched := dispatchedCommands(t)

	// Capture printHelp() output (writes to os.Stdout).
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
	helpOut := string(buf[:n])

	helpCmdRe := regexp.MustCompile(`promise ([a-z][a-z0-9-]*)`)
	for _, m := range helpCmdRe.FindAllStringSubmatch(helpOut, -1) {
		cmd := m[1]
		if !dispatched[cmd] {
			t.Errorf("printHelp() documents command %q which is not dispatched in main.go", cmd)
		}
	}
}
