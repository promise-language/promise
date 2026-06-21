package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestPrintIndexGroupedOrder verifies the index renders the documented groups in
// order, each with a header, to the provided writer (T1006).
func TestPrintIndexGroupedOrder(t *testing.T) {
	var buf strings.Builder
	printIndex(&buf)
	out := buf.String()

	last := -1
	for _, g := range groupOrder {
		idx := strings.Index(out, g)
		if idx < 0 {
			t.Errorf("index missing group header %q", g)
			continue
		}
		if idx < last {
			t.Errorf("group %q rendered out of order", g)
		}
		last = idx
	}

	// The footer pointer must guide users to the overview / per-command help.
	if !strings.Contains(out, "promise help") || !strings.Contains(out, "--help") {
		t.Errorf("index missing help pointer footer, got:\n%s", out)
	}
}

// TestFindNode covers resolution of top-level commands, nested subcommands,
// unknown first tokens, and extra (unmatched) trailing tokens.
func TestFindNode(t *testing.T) {
	tests := []struct {
		path        []string
		wantOK      bool
		wantMatched []string
	}{
		{[]string{"build"}, true, []string{"build"}},
		{[]string{"package", "add"}, true, []string{"package", "add"}},
		{[]string{"package", "check-upgrade"}, true, []string{"package", "check-upgrade"}},
		{[]string{"update", "channel"}, true, []string{"update", "channel"}},
		{[]string{"bogus"}, false, nil},
		// Empty path resolves nothing (the len(path)==0 guard).
		{nil, false, nil},
		{[]string{}, false, nil},
		// Extra token that is not a subcommand stops the walk (matched < path).
		{[]string{"build", "extra"}, true, []string{"build"}},
		{[]string{"package", "nope"}, true, []string{"package"}},
	}
	for _, tc := range tests {
		node, matched, ok := findNode(tc.path)
		if ok != tc.wantOK {
			t.Errorf("findNode(%v) ok = %v, want %v", tc.path, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if strings.Join(matched, " ") != strings.Join(tc.wantMatched, " ") {
			t.Errorf("findNode(%v) matched = %v, want %v", tc.path, matched, tc.wantMatched)
		}
		if node == nil {
			t.Errorf("findNode(%v) returned nil node", tc.path)
		}
	}
}

// TestHandleHelpToStdout verifies the central interceptor handles -h/-help/--help
// (already normalized) on commands and subcommands, and `promise help <path>`,
// writing to stdout and reporting handled=true.
func TestHandleHelpToStdout(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string // substring expected in stdout
	}{
		{"command help flag", []string{"build", "-help"}, "promise build"},
		{"command short flag", []string{"build", "-h"}, "promise build"},
		{"group bare-ish flag", []string{"package", "-help"}, "Subcommands:"},
		{"subcommand help flag", []string{"package", "add", "-help"}, "promise package add"},
		{"help router leaf", []string{"help", "package", "add"}, "promise package add"},
		{"help router group", []string{"help", "package"}, "Subcommands:"},
		{"rich renderer doc", []string{"doc", "-help"}, "usage: promise doc"},
		{"rich renderer targets", []string{"targets", "-help"}, "usage: promise targets"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var handled bool
			out := captureStdout(t, func() {
				handled = handleHelp(tc.args)
			})
			if !handled {
				t.Fatalf("handleHelp(%v) = false, want true", tc.args)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("handleHelp(%v) stdout missing %q, got:\n%s", tc.args, tc.want, out)
			}
		})
	}
}

// TestHandleHelpDeclines verifies handleHelp returns false (defers to normal
// dispatch) when there's no help flag, and when the leading token is unknown so
// the unknown-command error path can run.
func TestHandleHelpDeclines(t *testing.T) {
	cases := [][]string{
		{"build", "file.pr"}, // no help flag
		{"bogus", "-help"},   // unknown command + help flag → let dispatch error
		{"-ast", "file.pr"},  // legacy flag form
		nil,                  // empty
	}
	for _, args := range cases {
		if handleHelp(args) {
			t.Errorf("handleHelp(%v) = true, want false", args)
		}
	}
}

// TestRouteHelpEmptyPrintsOverview verifies `promise help` (no path) routes to
// the overview, not the index.
func TestRouteHelpEmptyPrintsOverview(t *testing.T) {
	out := captureStdout(t, func() { routeHelp(nil) })
	if !strings.Contains(out, "Quick Start") {
		t.Errorf("routeHelp(nil) should print the overview, got:\n%s", out)
	}
}

// TestPrintNodeHelpSynthesizedLeaf verifies a leaf without a rich renderer gets a
// synthesized usage synopsis plus its summary.
func TestPrintNodeHelpSynthesizedLeaf(t *testing.T) {
	node, matched, ok := findNode([]string{"version"})
	if !ok {
		t.Fatal("version node not found")
	}
	var buf strings.Builder
	printNodeHelp(&buf, node, matched)
	out := buf.String()
	if !strings.Contains(out, "Usage: promise version") {
		t.Errorf("synthesized leaf help missing usage line, got:\n%s", out)
	}
	if !strings.Contains(out, node.summary) {
		t.Errorf("synthesized leaf help missing summary, got:\n%s", out)
	}
}

// TestPrintNodeHelpSynthesizedGroup verifies a pure group (no rich renderer)
// renders a usage line, every subcommand on its own line, and the
// `<subcommand> --help` pointer footer (T1006 §3).
func TestPrintNodeHelpSynthesizedGroup(t *testing.T) {
	node, matched, ok := findNode([]string{"package"})
	if !ok {
		t.Fatal("package node not found")
	}
	var buf strings.Builder
	printNodeHelp(&buf, node, matched)
	out := buf.String()

	if !strings.Contains(out, "Usage: promise package <subcommand>") {
		t.Errorf("group help missing usage line, got:\n%s", out)
	}
	if !strings.Contains(out, "Subcommands:") {
		t.Errorf("group help missing Subcommands header, got:\n%s", out)
	}
	for _, s := range node.subs {
		if !strings.Contains(out, s.name) || !strings.Contains(out, s.summary) {
			t.Errorf("group help missing subcommand %q (%q), got:\n%s", s.name, s.summary, out)
		}
	}
	if !strings.Contains(out, "Run 'promise package <subcommand> --help' for details.") {
		t.Errorf("group help missing per-subcommand help pointer, got:\n%s", out)
	}
}

// TestHelpHint verifies the shared usage-error pointer names both the overview
// command and the per-command help flag (T1006 §1 — short pointer, not a full
// reference dump).
func TestHelpHint(t *testing.T) {
	var buf strings.Builder
	helpHint(&buf)
	out := buf.String()
	if !strings.Contains(out, "promise help") || !strings.Contains(out, "--help") {
		t.Errorf("helpHint missing the expected pointer, got: %q", out)
	}
}

// TestIsHelpFlag pins the post-normalization help-flag contract: only `-h` and
// `-help` count (normalizeArgs has already collapsed `--help` → `-help`).
func TestIsHelpFlag(t *testing.T) {
	for _, a := range []string{"-h", "-help"} {
		if !isHelpFlag(a) {
			t.Errorf("isHelpFlag(%q) = false, want true", a)
		}
	}
	for _, a := range []string{"--help", "-help=1", "help", "-x", "file.pr", ""} {
		if isHelpFlag(a) {
			t.Errorf("isHelpFlag(%q) = true, want false", a)
		}
	}
}

// TestHelpFlagsCollapseViaNormalize verifies the §3 contract that `-h`, `-help`,
// and `--help` all reach the same stdout help node — the double-dash form only
// works because normalizeArgs canonicalizes it before handleHelp sees it.
func TestHelpFlagsCollapseViaNormalize(t *testing.T) {
	for _, flag := range []string{"-h", "-help", "--help"} {
		args := normalizeArgs([]string{"build", flag})
		var handled bool
		out := captureStdout(t, func() { handled = handleHelp(args) })
		if !handled {
			t.Errorf("handleHelp(normalize build %s) = false, want true", flag)
		}
		if !strings.Contains(out, "promise build") {
			t.Errorf("build %s help missing usage line, got:\n%s", flag, out)
		}
	}
}

// TestHelpPathsEmitNoStderr is the core stream-discipline guarantee (T1006 §1):
// requested help goes to stdout only, so `promise <cmd> --help | less` works
// without `2>&1`. Every help invocation here must leave stderr empty.
func TestHelpPathsEmitNoStderr(t *testing.T) {
	cases := [][]string{
		{"build", "-help"},
		{"package", "-help"},
		{"package", "add", "-help"},
		{"doc", "-help"},
		{"targets", "-help"},
		{"help", "package", "add"},
		{"help", "package"},
	}
	for _, args := range cases {
		var handled bool
		var stdout string
		stderr := captureStderr(func() {
			stdout = captureStdout(t, func() { handled = handleHelp(args) })
		})
		if !handled {
			t.Errorf("handleHelp(%v) = false, want true", args)
			continue
		}
		if strings.TrimSpace(stdout) == "" {
			t.Errorf("handleHelp(%v) produced no stdout", args)
		}
		if stderr != "" {
			t.Errorf("handleHelp(%v) wrote to stderr (should be stdout-only): %q", args, stderr)
		}
	}
}

// TestListDocModules covers the `promise doc` no-arg discovery body (T1006 §4):
// it lists the embedded catalog modules to the writer, and reports false when
// no catalog is embedded so the caller can surface an error instead.
func TestListDocModules(t *testing.T) {
	var buf strings.Builder
	ok := listDocModules(&buf)
	out := buf.String()
	if len(embeddedCatalog) > 0 {
		if !ok {
			t.Fatal("listDocModules returned false with a non-empty embedded catalog")
		}
		if !strings.Contains(out, "Available modules:") {
			t.Errorf("listing missing header, got:\n%s", out)
		}
		for _, mod := range []string{"std", "io", "json"} {
			if !strings.Contains(out, mod) {
				t.Errorf("listing missing module %q, got:\n%s", mod, out)
			}
		}
	}

	// Empty catalog → false, no output (the caller then errors to stderr).
	saved := embeddedCatalog
	embeddedCatalog = nil
	defer func() { embeddedCatalog = saved }()
	var buf2 strings.Builder
	if listDocModules(&buf2) {
		t.Error("listDocModules returned true with an empty catalog")
	}
	if buf2.String() != "" {
		t.Errorf("listDocModules wrote output with an empty catalog: %q", buf2.String())
	}
}

// TestUsageErrorsToStderr exercises the §1 usage-error discipline for the paths
// that call os.Exit: the diagnostic and the short pointer go to stderr, the
// process exits non-zero, and stdout stays empty. Each case re-execs this test
// binary so the os.Exit is contained.
func TestUsageErrorsToStderr(t *testing.T) {
	// Child side: dispatch on TEST_USAGE_ERROR and run the chosen exiting path.
	switch os.Getenv("TEST_USAGE_ERROR") {
	case "route-help-unknown":
		routeHelp([]string{"bogus", "topic"})
		return
	case "package-unknown":
		runPackage([]string{"frobnicate"})
		return
	case "catalog-unknown":
		runCatalog([]string{"frobnicate"})
		return
	}

	cases := []struct {
		name    string
		wantErr string // substring expected on stderr
	}{
		{"route-help-unknown", "unknown help topic"},
		{"package-unknown", "unknown package subcommand"},
		{"catalog-unknown", "unknown catalog subcommand"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=TestUsageErrorsToStderr")
			cmd.Env = append(os.Environ(), "TEST_USAGE_ERROR="+tc.name)
			var stdout, stderr strings.Builder
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			if err == nil {
				t.Fatalf("%s: expected non-zero exit", tc.name)
			}
			if !strings.Contains(stderr.String(), tc.wantErr) {
				t.Errorf("%s: stderr missing %q, got:\n%s", tc.name, tc.wantErr, stderr.String())
			}
			// Every usage error must point at help (the short pointer, §1).
			if !strings.Contains(stderr.String(), "promise help") {
				t.Errorf("%s: stderr missing help pointer, got:\n%s", tc.name, stderr.String())
			}
			if strings.TrimSpace(stdout.String()) != "" {
				t.Errorf("%s: usage error leaked to stdout: %q", tc.name, stdout.String())
			}
		})
	}
}
