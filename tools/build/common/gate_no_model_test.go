package common

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// forbiddenImports lists substrings of an import path that name a model SDK
// or MCP client — checked against every dependency, standard-library,
// third-party, or internal, since introducing any of them anywhere in
// bin/gate's transitive graph is exactly the reintroduction this test exists
// to catch.
var forbiddenImports = []string{"anthropic", "modelcontextprotocol", "mcp-go", "mcp-sdk"}

// forbiddenSource lists patterns naming an agent entry point in gate source
// text: an invocation of the `claude` CLI, or a shell string invoking
// bin/do or bin/flow.
var forbiddenSource = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bclaude\b`),
	regexp.MustCompile(`"bin/do"`),
	regexp.MustCompile(`"bin/flow"`),
}

// matchForbiddenSource reports the first forbiddenSource pattern that
// matches line, and the matched text — or ok=false if none does. A match
// immediately followed by ".md" or ".local.md" (case-insensitively) is
// ignored: that's this repo's own CLAUDE.md/CLAUDE.local.md instructions
// file, referenced by filename in doc checks, not an agent entry point.
func matchForbiddenSource(line string) (pattern *regexp.Regexp, matchText string, ok bool) {
	for _, re := range forbiddenSource {
		if loc := re.FindStringIndex(line); loc != nil {
			rest := strings.ToLower(line[loc[1]:])
			if strings.HasPrefix(rest, ".md") || strings.HasPrefix(rest, ".local.md") {
				continue
			}
			return re, line[loc[0]:loc[1]], true
		}
	}
	return nil, "", false
}

// TestMatchForbiddenSource exercises matchForbiddenSource directly against
// synthetic lines, independent of the current (clean) state of the real gate
// source tree. TestGateSourceNeverInvokesModel only ever runs this logic
// against source that already passes, so on its own it can't prove the
// detector would actually fire on a real reintroduction — a regex typo (e.g.
// a missing word boundary, or a case-sensitivity slip) would fail silently
// and this static assertion would stop meaning anything.
func TestMatchForbiddenSource(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantMatch string
	}{
		{"claude cli invocation", `cmd := exec.Command("claude", "-p", prompt)`, "claude"},
		{"claude capitalized", `// Claude Code session`, "Claude"},
		{"bin/do literal", `sh.Run("bin/do", "resolve", id)`, `"bin/do"`},
		{"bin/flow literal", `cmd := exec.Command("bin/flow", "status")`, `"bin/flow"`},
		{"claude.md reference ignored", `// see CLAUDE.md for instructions`, ""},
		{"claude.local.md reference ignored", `// see CLAUDE.local.md`, ""},
		{"unrelated word not matched", `// this is not a claudelike identifier`, ""},
		{"clean source line", `func RunGate(root string, args []string) error {`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got, ok := matchForbiddenSource(tt.line)
			if tt.wantMatch == "" {
				if ok {
					t.Errorf("matchForbiddenSource(%q) = %q, want no match", tt.line, got)
				}
				return
			}
			if !ok || got != tt.wantMatch {
				t.Errorf("matchForbiddenSource(%q) = (%q, %v), want (%q, true)", tt.line, got, ok, tt.wantMatch)
			}
		})
	}
}

// TestGateSourceNeverInvokesModel is a static assertion, not a sandbox: a gate
// run is unattended CI (T1877) — it runs on every host, hourly, forever, with
// no human in the loop and no cost ceiling, so the ability to reach a model
// must be unreachable, not merely unused today. This scans the exact source
// set linked into the bin/gate binary for any agent entry point, so a future
// edit that adds one fails a test instead of shipping quietly.
//
// The source set is discovered with `go list -deps`, not hardcoded to
// tools/build/common + tools/build/cmd/gate: a hardcoded pair of directories
// only catches an edit to those two directories, and would silently stop
// covering the binary the moment a future refactor moved gate logic into a
// new internal package. Every third-party or standard-library dependency is
// checked by import path (an SDK import shows up there even if no source
// text is scanned); every dependency inside this module has its non-test .go
// source text scanned for the entry points an import-path check can't see —
// `exec.Command("claude", ...)`  or an inline shell string invoking bin/do or
// bin/flow.
func TestGateSourceNeverInvokesModel(t *testing.T) {
	root, err := RootForTests()
	if err != nil {
		t.Fatalf("RootForTests: %v", err)
	}
	buildDir := filepath.Join(root, "tools", "build")

	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}|{{.Dir}}|{{.Standard}}", "./cmd/gate")
	cmd.Dir = buildDir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/gate: %v", err)
	}

	const modulePrefix = "github.com/promise-language/promise/tools/build"

	var internalDirs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "|", 3)
		if len(fields) != 3 {
			t.Fatalf("unexpected `go list` output line: %q", line)
		}
		importPath, dir, standard := fields[0], fields[1], fields[2]

		lower := strings.ToLower(importPath)
		for _, forbidden := range forbiddenImports {
			if strings.Contains(lower, forbidden) {
				t.Errorf("bin/gate transitively imports %q — a gate run must never be able to reach a model", importPath)
			}
		}

		if standard == "true" || !strings.HasPrefix(importPath, modulePrefix) {
			continue // stdlib / third-party — already covered by the import-path check above
		}
		internalDirs = append(internalDirs, dir)
	}
	if len(internalDirs) == 0 {
		t.Fatal("go list -deps ./cmd/gate reported no internal packages — the dependency scan is broken, not the source tree")
	}

	for _, dir := range internalDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir(%s): %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", path, err)
			}
			for lineNum, line := range strings.Split(string(data), "\n") {
				if re, matched, ok := matchForbiddenSource(line); ok {
					t.Errorf("%s:%d: gate source reaches a model entry point (%q matched %q) — bin/gate must never be able to invoke a model",
						path, lineNum+1, re.String(), matched)
				}
			}
		}
	}
}
