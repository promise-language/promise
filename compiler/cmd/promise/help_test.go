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

// indexHiddenCommands is the set of dispatched commands intentionally omitted
// from the concise command index (T1006): deprecated top-level aliases (T1007)
// and the removed verbs fetch/warm (still dispatch until T1008) and gc (now
// dispatches a removal notice; T1009).
var indexHiddenCommands = map[string]bool{
	"pkg": true, "add": true, "search": true, "pin": true,
	"fetch": true, "warm": true, "gc": true,
}

// TestIndexCoversAllCommands ensures every top-level subcommand dispatched in
// main()'s `switch cmd { ... }` appears in the concise command index, except the
// explicitly hidden set. Prevents future commands from being silently omitted
// (T0691, retargeted from usage() to printIndex in T1006).
func TestIndexCoversAllCommands(t *testing.T) {
	cmds := dispatchedCommands(t)

	var buf strings.Builder
	printIndex(&buf)
	indexOut := buf.String()

	// Each visible command must appear as the first token on some line in a
	// group block (e.g. `  bind      Generate ...`).
	for cmd := range cmds {
		if indexHiddenCommands[cmd] {
			continue
		}
		wantPrefix := "  " + cmd + " "
		found := false
		for _, line := range strings.Split(indexOut, "\n") {
			if strings.HasPrefix(line, wantPrefix) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("index missing entry for command %q (dispatched in main.go but not listed)", cmd)
		}
	}
}

// TestIndexOmitsRemovedVerbs guards §5: the command index must never advertise
// fetch/warm/gc. Also a tripwire so the T1008/T1009 removals don't regress.
func TestIndexOmitsRemovedVerbs(t *testing.T) {
	var buf strings.Builder
	printIndex(&buf)
	out := buf.String()
	for _, verb := range []string{"fetch", "warm", "gc"} {
		wantPrefix := "  " + verb + " "
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, wantPrefix) {
				t.Errorf("index advertises removed verb %q (line: %q)", verb, line)
			}
		}
	}
}

// TestHelpTreeReachable verifies the agent-UX discoverability contract: every
// non-hidden node and subcommand resolves via findNode and renders non-empty
// help (T1006).
func TestHelpTreeReachable(t *testing.T) {
	check := func(path []string) {
		node, matched, ok := findNode(path)
		if !ok || len(matched) != len(path) {
			t.Errorf("findNode(%v) did not resolve fully (matched %v)", path, matched)
			return
		}
		var buf strings.Builder
		printNodeHelp(&buf, node, matched)
		if strings.TrimSpace(buf.String()) == "" {
			t.Errorf("printNodeHelp(%v) produced empty output", path)
		}
	}
	for _, n := range commandTree {
		if n.hidden {
			continue
		}
		check([]string{n.name})
		for _, s := range n.subs {
			check([]string{n.name, s.name})
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
