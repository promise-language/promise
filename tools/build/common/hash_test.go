package common

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFlowFile creates parent dirs and writes data to root/rel. Test helper for
// the FlowsSourceHash cases below.
func writeFlowFile(t *testing.T, root, rel, data string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestFlowsSourceHash_DeterministicAndSensitive checks the hash is stable across
// repeated computation and changes when source in either tree changes — the two
// properties the ./make up-to-date check relies on (skip when nothing changed,
// rebuild when flows/ or flow-sdk/ changed).
func TestFlowsSourceHash_DeterministicAndSensitive(t *testing.T) {
	root := t.TempDir()
	writeFlowFile(t, root, "flows/go.mod", "module flows\n")
	writeFlowFile(t, root, "flows/do/main.go", "package main\nfunc main() {}\n")
	writeFlowFile(t, root, "flow-sdk/go.mod", "module sdk\n")
	writeFlowFile(t, root, "flow-sdk/sdk.go", "package sdk\n")

	h1, err := FlowsSourceHash(root)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == "" {
		t.Fatal("empty hash")
	}

	// Recomputing without changes is stable.
	if h2, err := FlowsSourceHash(root); err != nil {
		t.Fatal(err)
	} else if h1 != h2 {
		t.Errorf("hash not deterministic: %s != %s", h1, h2)
	}

	// A change in flows/ source changes the hash.
	writeFlowFile(t, root, "flows/do/main.go", "package main\nfunc main() { _ = 1 }\n")
	h3, err := FlowsSourceHash(root)
	if err != nil {
		t.Fatal(err)
	}
	if h3 == h1 {
		t.Error("flows/ change did not affect hash")
	}

	// A change in flow-sdk/ source changes the hash.
	writeFlowFile(t, root, "flow-sdk/sdk.go", "package sdk\nvar X = 1\n")
	h4, err := FlowsSourceHash(root)
	if err != nil {
		t.Fatal(err)
	}
	if h4 == h3 {
		t.Error("flow-sdk/ change did not affect hash")
	}

	// A prompt-template (.tmpl) edit changes the hash — flow binaries go:embed their
	// prompts, so ./make must rebuild on a prompt-only change. This MUST stay in
	// lockstep with flows/internal/srchash; if it doesn't, ./make reports "up to
	// date" while the rebuilt binary's runtime check reports "stale" (rebuild
	// deadlock).
	writeFlowFile(t, root, "flows/do/templates/implement.tmpl", "implement {{.Plan}}\n")
	h5, err := FlowsSourceHash(root)
	if err != nil {
		t.Fatal(err)
	}
	if h5 == h4 {
		t.Error("adding a .tmpl prompt template did not affect hash")
	}
	writeFlowFile(t, root, "flows/do/templates/implement.tmpl", "implement {{.Plan}} — prefer batch tests\n")
	h6, err := FlowsSourceHash(root)
	if err != nil {
		t.Fatal(err)
	}
	if h6 == h5 {
		t.Error("editing a .tmpl prompt template did not affect hash")
	}
}

// TestFlowsSourceHash_IgnoresGitAndNonSource confirms a fetched SDK's .git
// directory (which churns on every pull) and non-source files do not perturb the
// hash, so ./make does not needlessly rebuild flows after a no-op SDK fetch.
func TestFlowsSourceHash_IgnoresGitAndNonSource(t *testing.T) {
	root := t.TempDir()
	writeFlowFile(t, root, "flows/go.mod", "module flows\n")
	writeFlowFile(t, root, "flows/do/main.go", "package main\n")
	writeFlowFile(t, root, "flow-sdk/go.mod", "module sdk\n")
	writeFlowFile(t, root, "flow-sdk/sdk.go", "package sdk\n")

	base, err := FlowsSourceHash(root)
	if err != nil {
		t.Fatal(err)
	}

	// .git contents and non-source files must not affect the hash.
	writeFlowFile(t, root, "flow-sdk/.git/HEAD", "ref: refs/heads/main\n")
	writeFlowFile(t, root, "flow-sdk/README.md", "docs\n")
	writeFlowFile(t, root, "flows/do/notes.txt", "scratch\n")

	after, err := FlowsSourceHash(root)
	if err != nil {
		t.Fatal(err)
	}
	if after != base {
		t.Errorf("hash changed after adding ignored files: %s != %s", base, after)
	}
}

// TestFlowsSourceHash_MissingTreesTolerated checks an absent flows/ or flow-sdk/
// yields a stable hash rather than an error, so the up-to-date check degrades
// gracefully on a fresh clone before anything is fetched.
func TestFlowsSourceHash_MissingTreesTolerated(t *testing.T) {
	root := t.TempDir() // neither flows/ nor flow-sdk/ exist
	h, err := FlowsSourceHash(root)
	if err != nil {
		t.Fatalf("expected no error for missing trees, got %v", err)
	}
	if h == "" {
		t.Fatal("expected a stable empty-tree hash, got empty string")
	}
}
