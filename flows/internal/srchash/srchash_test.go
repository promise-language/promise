package srchash

import (
	"os"
	"path/filepath"
	"testing"
)

// fixtureRoot builds a throwaway repo root with catalog.toml and the two flow
// source dirs populated, returning the root path.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("catalog.toml", "epoch = \"2026.0\"\n")
	write("flows/go.mod", "module example/flows\n")
	write("flows/do/main.go", "package main\nfunc main() {}\n")
	write("flow-sdk/go.mod", "module example/sdk\n")
	write("flow-sdk/item.go", "package sdk\n")
	return root
}

func TestHashStable(t *testing.T) {
	root := fixtureRoot(t)
	h1, err := Hash(root)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := Hash(root)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hash not stable: %s vs %s", h1, h2)
	}
	if h1 == "" {
		t.Fatal("hash is empty")
	}
}

func TestHashSensitiveToFlowsChange(t *testing.T) {
	root := fixtureRoot(t)
	before, _ := Hash(root)
	if err := os.WriteFile(filepath.Join(root, "flows/do/main.go"),
		[]byte("package main\nfunc main() { _ = 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _ := Hash(root)
	if before == after {
		t.Fatal("hash did not change after editing a flows/ file")
	}
}

func TestHashSensitiveToSDKChange(t *testing.T) {
	root := fixtureRoot(t)
	before, _ := Hash(root)
	if err := os.WriteFile(filepath.Join(root, "flow-sdk/item.go"),
		[]byte("package sdk\nvar X = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _ := Hash(root)
	if before == after {
		t.Fatal("hash did not change after editing a flow-sdk/ file")
	}
}

func TestHashSensitiveToNewFile(t *testing.T) {
	root := fixtureRoot(t)
	before, _ := Hash(root)
	if err := os.WriteFile(filepath.Join(root, "flows/do/extra.go"),
		[]byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _ := Hash(root)
	if before == after {
		t.Fatal("hash did not change after adding a flows/ file")
	}
}

// TestHashSensitiveToTemplateChange confirms a prompt-template (.tmpl) edit flips
// the hash — flow binaries go:embed their prompt templates, so a prompt-only change
// must mark a built binary stale (otherwise ./make skips the rebuild and the binary
// serves the old embedded prompts).
func TestHashSensitiveToTemplateChange(t *testing.T) {
	root := fixtureRoot(t)
	tmpl := filepath.Join(root, "flows/do/templates/implement.tmpl")
	if err := os.MkdirAll(filepath.Dir(tmpl), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmpl, []byte("implement {{.Plan}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := Hash(root)
	if err := os.WriteFile(tmpl, []byte("implement {{.Plan}} — now prefer batch tests\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _ := Hash(root)
	if before == after {
		t.Fatal("hash did not change after editing a .tmpl prompt template")
	}
}

// TestHashIgnoresNonSourceFiles confirms only .go/.tmpl/go.mod/go.sum count — a
// stray README or binary in the tree must not flip the hash.
func TestHashIgnoresNonSourceFiles(t *testing.T) {
	root := fixtureRoot(t)
	before, _ := Hash(root)
	if err := os.WriteFile(filepath.Join(root, "flows/README.md"),
		[]byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _ := Hash(root)
	if before != after {
		t.Fatal("hash changed for a non-source file")
	}
}

// TestHashSkipsGitDir confirms a .git dir under flow-sdk/ (it is a clone) does not
// contribute, even if it happens to contain a *.go-named object.
func TestHashSkipsGitDir(t *testing.T) {
	root := fixtureRoot(t)
	before, _ := Hash(root)
	if err := os.MkdirAll(filepath.Join(root, "flow-sdk/.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "flow-sdk/.git/weird.go"),
		[]byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _ := Hash(root)
	if before != after {
		t.Fatal("hash changed for a file inside .git")
	}
}

// TestHashMissingSDKChangesHash confirms deleting flow-sdk/ yields a different
// hash (so a built binary correctly detects the SDK going away).
func TestHashMissingSDKChangesHash(t *testing.T) {
	root := fixtureRoot(t)
	before, _ := Hash(root)
	if err := os.RemoveAll(filepath.Join(root, "flow-sdk")); err != nil {
		t.Fatal(err)
	}
	after, err := Hash(root)
	if err != nil {
		t.Fatalf("Hash with missing flow-sdk/ errored: %v", err)
	}
	if before == after {
		t.Fatal("hash unchanged after removing flow-sdk/")
	}
}

func TestFindRootFromCwd(t *testing.T) {
	root := fixtureRoot(t)
	sub := filepath.Join(root, "flows", "do")
	t.Chdir(sub)
	got, err := FindRoot()
	if err != nil {
		t.Fatal(err)
	}
	// macOS /tmp is a symlink to /private/tmp; compare resolved paths.
	gotR, _ := filepath.EvalSymlinks(got)
	rootR, _ := filepath.EvalSymlinks(root)
	if gotR != rootR {
		t.Fatalf("FindRoot = %s, want %s", gotR, rootR)
	}
}
