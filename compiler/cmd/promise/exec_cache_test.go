package main

import (
	"os"

	"path/filepath"
	"strings"
	"testing"
)

func TestComputeExecBinaryCacheKeyStable(t *testing.T) {
	src := "main() { }\n"

	key1, ok1 := computeExecBinaryCacheKey(src, "x86_64-unknown-linux-gnu")
	key2, ok2 := computeExecBinaryCacheKey(src, "x86_64-unknown-linux-gnu")

	if !ok1 || !ok2 {
		t.Skipf("cache key computation not available (missing std hash)")
	}
	if key1 != key2 {
		t.Errorf("same inputs produced different keys: %q vs %q", key1, key2)
	}
}

func TestComputeExecBinaryCacheKeySourceChange(t *testing.T) {
	key1, ok := computeExecBinaryCacheKey("main() { }\n", "x86_64-unknown-linux-gnu")
	if !ok {
		t.Skipf("cache key computation not available (missing std hash)")
	}
	// A one-character change must produce a different key.
	key2, _ := computeExecBinaryCacheKey("main() { /* changed */ }\n", "x86_64-unknown-linux-gnu")
	if key1 == key2 {
		t.Error("source change should produce a different cache key")
	}
}

func TestComputeExecBinaryCacheKeyTargetChange(t *testing.T) {
	src := "main() { }\n"
	keyLinux, ok := computeExecBinaryCacheKey(src, "x86_64-unknown-linux-gnu")
	if !ok {
		t.Skipf("cache key computation not available (missing std hash)")
	}
	keyMac, _ := computeExecBinaryCacheKey(src, "aarch64-apple-darwin24")
	if keyLinux == keyMac {
		t.Error("different target triples should produce different cache keys")
	}
}

func TestComputeExecBinaryCacheKeyDistinctFromRunBinary(t *testing.T) {
	// The "exec-binary:" prefix must yield a key distinct from a "run-binary:" key
	// built from identical source bytes.
	dir := t.TempDir()
	src := "main() { }\n"
	path := filepath.Join(dir, "main.pr")
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	execKey, ok := computeExecBinaryCacheKey(src, "x86_64-unknown-linux-gnu")
	if !ok {
		t.Skipf("cache key computation not available (missing std hash)")
	}
	runKey, ok := computeRunBinaryCacheKey(path, "x86_64-unknown-linux-gnu", false)
	if !ok {
		t.Skipf("cache key computation not available (missing std hash)")
	}
	if execKey == runKey {
		t.Error("exec-binary key must differ from run-binary key for identical source")
	}
}

func TestComputeExecBinaryCacheKeyRemoteImportNotCacheable(t *testing.T) {
	src := "use remote \"github.com/example/repo\";\nmain() { }\n"
	if _, ok := computeExecBinaryCacheKey(src, "x86_64-unknown-linux-gnu"); ok {
		t.Error("remote import should make exec binary non-cacheable")
	}
}

func TestComputeExecBinaryCacheKeyMissingEmbedNotCacheable(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	src := "`embed(\"missing.txt\")\nblob bytes;\nmain() { }\n"
	if _, ok := computeExecBinaryCacheKey(src, "x86_64-unknown-linux-gnu"); ok {
		t.Error("missing embed file should make exec binary non-cacheable")
	}
}

func TestComputeExecBinaryCacheKeyEmbedChange(t *testing.T) {
	// exec resolves embed/dep paths against the current working directory.
	dir := t.TempDir()
	chdir(t, dir)
	embedPath := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(embedPath, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	src := "`embed(\"data.txt\")\nblob bytes;\nmain() { }\n"

	key1, ok := computeExecBinaryCacheKey(src, "x86_64-unknown-linux-gnu")
	if !ok {
		t.Skipf("cache key computation not available (missing std hash)")
	}
	if err := os.WriteFile(embedPath, []byte("goodbye"), 0644); err != nil {
		t.Fatal(err)
	}
	key2, _ := computeExecBinaryCacheKey(src, "x86_64-unknown-linux-gnu")
	if key1 == key2 {
		t.Error("embed file change should invalidate cache key")
	}
}

func TestComputeExecBinaryCacheKeyLocalDepChange(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	depDir := filepath.Join(dir, "dep")
	if err := os.MkdirAll(depDir, 0755); err != nil {
		t.Fatal(err)
	}
	depFile := filepath.Join(depDir, "dep.pr")
	if err := os.WriteFile(depFile, []byte("`public\nhello() int { 1 }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	src := "use dep \"./dep\";\nmain() { }\n"

	key1, ok := computeExecBinaryCacheKey(src, "x86_64-unknown-linux-gnu")
	if !ok {
		t.Skipf("cache key computation not available (missing std hash)")
	}
	if err := os.WriteFile(depFile, []byte("`public\nhello() int { 2 }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	key2, ok := computeExecBinaryCacheKey(src, "x86_64-unknown-linux-gnu")
	if !ok {
		t.Fatal("expected cacheable after dep change")
	}
	if key1 == key2 {
		t.Error("local dep change should invalidate cache key")
	}
}

// TestComputeExecBinaryCacheInputsEmbedAndDep exercises the embed-file and
// local-dep branches of computeExecBinaryCacheInputs, which the basic
// no-dependency case does not reach. Paths resolve against the working
// directory (exec has no source file), so chdir into a temp dir first.
func TestComputeExecBinaryCacheInputsEmbedAndDep(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	depDir := filepath.Join(dir, "dep")
	if err := os.MkdirAll(depDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(depDir, "dep.pr"), []byte("`public\nhello() int { 1 }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	src := "`embed(\"data.txt\")\nblob bytes;\nuse dep \"./dep\";\nmain() { }\n"
	inputs := computeExecBinaryCacheInputs(src, "x86_64-unknown-linux-gnu")

	var foundEmbed, foundDep bool
	for _, in := range inputs {
		if strings.HasPrefix(in.Label, "embed") {
			foundEmbed = true
			if in.Value == "" {
				t.Error("embed input value is empty")
			}
		}
		if in.Label == "dep ./dep" {
			foundDep = true
			if in.Value == "" {
				t.Error("dep input value is empty")
			}
		}
	}
	if !foundEmbed {
		t.Errorf("inputs missing 'embed' label: %+v", inputs)
	}
	if !foundDep {
		t.Errorf("inputs missing 'dep ./dep' label: %+v", inputs)
	}
}

func TestComputeExecBinaryCacheInputs(t *testing.T) {
	inputs := computeExecBinaryCacheInputs("main() { }\n", "x86_64-unknown-linux-gnu")
	if inputs == nil {
		t.Fatal("expected non-nil inputs")
	}
	want := []string{"file", "compiler", "std", "target", "mode"}
	if len(inputs) < len(want) {
		t.Fatalf("got %d inputs, want at least %d: %+v", len(inputs), len(want), inputs)
	}
	for i, label := range want {
		if inputs[i].Label != label {
			t.Errorf("inputs[%d].Label = %q, want %q", i, inputs[i].Label, label)
		}
	}
	for _, in := range inputs {
		if in.Label == "target" && in.Value != "x86_64-unknown-linux-gnu" {
			t.Errorf("target value = %q, want %q", in.Value, "x86_64-unknown-linux-gnu")
		}
		if in.Label == "mode" && in.Value != "debug" {
			t.Errorf("mode value = %q, want %q", in.Value, "debug")
		}
	}
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(prev) })
}
