package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeRunSource writes the given source to <dir>/main.pr and returns the path.
func writeRunSource(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "main.pr")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestComputeRunBinaryCacheKeyStable(t *testing.T) {
	dir := t.TempDir()
	src := writeRunSource(t, dir, "main() { }\n")

	key1, ok1 := computeRunBinaryCacheKey(src, "x86_64-unknown-linux-gnu", false)
	key2, ok2 := computeRunBinaryCacheKey(src, "x86_64-unknown-linux-gnu", false)

	if !ok1 || !ok2 {
		t.Skipf("cache key computation not available (missing std hash)")
	}
	if key1 != key2 {
		t.Errorf("same inputs produced different keys: %q vs %q", key1, key2)
	}
}

func TestComputeRunBinaryCacheKeySourceChange(t *testing.T) {
	dir := t.TempDir()
	src := writeRunSource(t, dir, "main() { }\n")

	key1, ok := computeRunBinaryCacheKey(src, "x86_64-unknown-linux-gnu", false)
	if !ok {
		t.Skipf("cache key computation not available (missing std hash)")
	}

	// Change the source
	if err := os.WriteFile(src, []byte("main() { /* changed */ }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	key2, _ := computeRunBinaryCacheKey(src, "x86_64-unknown-linux-gnu", false)

	if key1 == key2 {
		t.Error("source change should produce a different cache key")
	}
}

func TestComputeRunBinaryCacheKeyTargetChange(t *testing.T) {
	dir := t.TempDir()
	src := writeRunSource(t, dir, "main() { }\n")

	keyLinux, ok := computeRunBinaryCacheKey(src, "x86_64-unknown-linux-gnu", false)
	if !ok {
		t.Skipf("cache key computation not available (missing std hash)")
	}
	keyMac, _ := computeRunBinaryCacheKey(src, "aarch64-apple-darwin24", false)

	if keyLinux == keyMac {
		t.Error("different target triples should produce different cache keys")
	}
}

func TestComputeRunBinaryCacheKeyReleaseMode(t *testing.T) {
	dir := t.TempDir()
	src := writeRunSource(t, dir, "main() { }\n")

	keyDebug, ok := computeRunBinaryCacheKey(src, "x86_64-unknown-linux-gnu", false)
	if !ok {
		t.Skipf("cache key computation not available (missing std hash)")
	}
	keyRelease, _ := computeRunBinaryCacheKey(src, "x86_64-unknown-linux-gnu", true)

	if keyDebug == keyRelease {
		t.Error("debug vs release mode should produce different cache keys")
	}
}

func TestComputeRunBinaryCacheKeyLocalDepChange(t *testing.T) {
	dir := t.TempDir()
	// Create a local module at ./dep
	depDir := filepath.Join(dir, "dep")
	if err := os.MkdirAll(depDir, 0755); err != nil {
		t.Fatal(err)
	}
	depFile := filepath.Join(depDir, "dep.pr")
	if err := os.WriteFile(depFile, []byte("`public\nhello() int { 1 }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	src := writeRunSource(t, dir, `use dep "./dep";
main() { }
`)

	key1, ok := computeRunBinaryCacheKey(src, "x86_64-unknown-linux-gnu", false)
	if !ok {
		t.Skipf("cache key computation not available (missing std hash)")
	}

	// Change the dep's source
	if err := os.WriteFile(depFile, []byte("`public\nhello() int { 2 }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	key2, ok := computeRunBinaryCacheKey(src, "x86_64-unknown-linux-gnu", false)
	if !ok {
		t.Fatal("expected cacheable after dep change")
	}

	if key1 == key2 {
		t.Error("local dep change should invalidate cache key")
	}
}

func TestComputeRunBinaryCacheKeyEmbedChange(t *testing.T) {
	dir := t.TempDir()
	embedPath := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(embedPath, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	src := writeRunSource(t, dir, "`embed(\"data.txt\")\nblob bytes;\nmain() { }\n")

	key1, ok := computeRunBinaryCacheKey(src, "x86_64-unknown-linux-gnu", false)
	if !ok {
		t.Skipf("cache key computation not available (missing std hash)")
	}

	// Change the embedded file
	if err := os.WriteFile(embedPath, []byte("goodbye"), 0644); err != nil {
		t.Fatal(err)
	}
	key2, _ := computeRunBinaryCacheKey(src, "x86_64-unknown-linux-gnu", false)

	if key1 == key2 {
		t.Error("embed file change should invalidate cache key")
	}
}

func TestComputeRunBinaryCacheKeyRemoteImportNotCacheable(t *testing.T) {
	dir := t.TempDir()
	src := writeRunSource(t, dir, `use remote "github.com/example/repo";
main() { }
`)

	_, ok := computeRunBinaryCacheKey(src, "x86_64-unknown-linux-gnu", false)
	if ok {
		t.Error("remote import should make run binary non-cacheable")
	}
}

func TestComputeRunBinaryCacheKeyMissingEmbedNotCacheable(t *testing.T) {
	dir := t.TempDir()
	src := writeRunSource(t, dir, "`embed(\"missing.txt\")\nblob bytes;\nmain() { }\n")

	_, ok := computeRunBinaryCacheKey(src, "x86_64-unknown-linux-gnu", false)
	if ok {
		t.Error("missing embed file should make run binary non-cacheable")
	}
}

func TestComputeRunBinaryCacheKeyMissingFile(t *testing.T) {
	_, ok := computeRunBinaryCacheKey("/nonexistent/file/12345.pr", "x86_64-unknown-linux-gnu", false)
	if ok {
		t.Error("missing source file should not be cacheable")
	}
}

// writeProjectFile writes a file inside a project directory, creating
// intermediate directories as needed.
func writeProjectFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestComputeProjectBinaryCacheKeyStable(t *testing.T) {
	dir := t.TempDir()
	writeProjectFile(t, dir, "promise.toml", "[module]\nname = \"app\"\nepoch = \"2026.0\"\n")
	writeProjectFile(t, dir, "main.pr", "main() {}\n")
	writeProjectFile(t, dir, "lib.pr", "type Foo {}\n")

	key1, ok1 := computeProjectBinaryCacheKey(dir, "x86_64-unknown-linux-gnu", false)
	key2, ok2 := computeProjectBinaryCacheKey(dir, "x86_64-unknown-linux-gnu", false)

	if !ok1 || !ok2 {
		t.Skipf("cache key computation not available (missing std hash)")
	}
	if key1 != key2 {
		t.Errorf("same inputs produced different keys: %q vs %q", key1, key2)
	}
}

func TestComputeProjectBinaryCacheKeySourceChange(t *testing.T) {
	dir := t.TempDir()
	writeProjectFile(t, dir, "promise.toml", "[module]\nname = \"app\"\nepoch = \"2026.0\"\n")
	writeProjectFile(t, dir, "main.pr", "main() {}\n")
	libPath := filepath.Join(dir, "lib.pr")
	writeProjectFile(t, dir, "lib.pr", "type Foo {}\n")

	key1, ok := computeProjectBinaryCacheKey(dir, "x86_64-unknown-linux-gnu", false)
	if !ok {
		t.Skipf("cache key computation not available (missing std hash)")
	}

	// Change a non-main file
	if err := os.WriteFile(libPath, []byte("type Foo { int x; }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	key2, _ := computeProjectBinaryCacheKey(dir, "x86_64-unknown-linux-gnu", false)

	if key1 == key2 {
		t.Error("source change in any project file should invalidate cache key")
	}
}

func TestComputeProjectBinaryCacheKeyDistinctFromRunBinary(t *testing.T) {
	// Project-mode keys must not collide with single-file run keys.
	dir := t.TempDir()
	writeProjectFile(t, dir, "promise.toml", "[module]\nname = \"app\"\nepoch = \"2026.0\"\n")
	writeProjectFile(t, dir, "main.pr", "main() {}\n")

	projectKey, ok := computeProjectBinaryCacheKey(dir, "x86_64-unknown-linux-gnu", false)
	if !ok {
		t.Skipf("cache key computation not available (missing std hash)")
	}
	runKey, ok := computeRunBinaryCacheKey(filepath.Join(dir, "main.pr"), "x86_64-unknown-linux-gnu", false)
	if !ok {
		t.Skipf("cache key computation not available (missing std hash)")
	}
	if projectKey == runKey {
		t.Error("project-mode key must differ from single-file run key")
	}
}

func TestComputeProjectBinaryCacheKeyEmbedChange(t *testing.T) {
	dir := t.TempDir()
	writeProjectFile(t, dir, "promise.toml", "[module]\nname = \"app\"\nepoch = \"2026.0\"\n")
	embedPath := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(embedPath, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, dir, "main.pr", "`embed(\"data.txt\")\nblob bytes;\nmain() {}\n")

	key1, ok := computeProjectBinaryCacheKey(dir, "x86_64-unknown-linux-gnu", false)
	if !ok {
		t.Skipf("cache key computation not available (missing std hash)")
	}
	if err := os.WriteFile(embedPath, []byte("goodbye"), 0644); err != nil {
		t.Fatal(err)
	}
	key2, _ := computeProjectBinaryCacheKey(dir, "x86_64-unknown-linux-gnu", false)
	if key1 == key2 {
		t.Error("embed file change should invalidate project cache key")
	}
}

func TestComputeProjectBinaryCacheKeyTargetChange(t *testing.T) {
	dir := t.TempDir()
	writeProjectFile(t, dir, "promise.toml", "[module]\nname = \"app\"\nepoch = \"2026.0\"\n")
	writeProjectFile(t, dir, "main.pr", "main() {}\n")

	keyLinux, ok := computeProjectBinaryCacheKey(dir, "x86_64-unknown-linux-gnu", false)
	if !ok {
		t.Skipf("cache key computation not available (missing std hash)")
	}
	keyMac, _ := computeProjectBinaryCacheKey(dir, "aarch64-apple-darwin24", false)
	if keyLinux == keyMac {
		t.Error("different target triples should produce different project cache keys")
	}
}

func TestComputeProjectBinaryCacheKeyReleaseMode(t *testing.T) {
	dir := t.TempDir()
	writeProjectFile(t, dir, "promise.toml", "[module]\nname = \"app\"\nepoch = \"2026.0\"\n")
	writeProjectFile(t, dir, "main.pr", "main() {}\n")

	keyDebug, ok := computeProjectBinaryCacheKey(dir, "x86_64-unknown-linux-gnu", false)
	if !ok {
		t.Skipf("cache key computation not available (missing std hash)")
	}
	keyRelease, _ := computeProjectBinaryCacheKey(dir, "x86_64-unknown-linux-gnu", true)
	if keyDebug == keyRelease {
		t.Error("debug vs release mode should produce different project cache keys")
	}
}

func TestComputeProjectBinaryCacheKeyLocalDepChange(t *testing.T) {
	dir := t.TempDir()
	writeProjectFile(t, dir, "promise.toml", "[module]\nname = \"app\"\nepoch = \"2026.0\"\n")
	// Sibling local module under ./dep
	depDir := filepath.Join(dir, "dep")
	if err := os.MkdirAll(depDir, 0755); err != nil {
		t.Fatal(err)
	}
	depFile := filepath.Join(depDir, "dep.pr")
	if err := os.WriteFile(depFile, []byte("`public\nhello() int { 1 }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, dir, "main.pr", `use dep "./dep";
main() {}
`)

	key1, ok := computeProjectBinaryCacheKey(dir, "x86_64-unknown-linux-gnu", false)
	if !ok {
		t.Skipf("cache key computation not available (missing std hash)")
	}
	if err := os.WriteFile(depFile, []byte("`public\nhello() int { 2 }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	key2, _ := computeProjectBinaryCacheKey(dir, "x86_64-unknown-linux-gnu", false)
	if key1 == key2 {
		t.Error("local dep change should invalidate project cache key")
	}
}

func TestComputeProjectBinaryCacheKeyMissingDir(t *testing.T) {
	_, ok := computeProjectBinaryCacheKey("/nonexistent/project/dir/12345", "x86_64-unknown-linux-gnu", false)
	if ok {
		t.Error("missing project dir should not be cacheable")
	}
}

func TestComputeProjectBinaryCacheInputs(t *testing.T) {
	dir := t.TempDir()
	writeProjectFile(t, dir, "promise.toml", "[module]\nname = \"app\"\nepoch = \"2026.0\"\n")
	writeProjectFile(t, dir, "main.pr", "main() {}\n")
	writeProjectFile(t, dir, "lib.pr", "type Foo {}\n")

	inputs := computeProjectBinaryCacheInputs(dir, "x86_64-unknown-linux-gnu", false)
	if inputs == nil {
		t.Skipf("cache inputs not available (missing std hash)")
	}

	// Required labels in the order they're written by the function.
	want := []string{"impl", "compiler", "std", "target", "mode"}
	if len(inputs) < len(want) {
		t.Fatalf("got %d inputs, want at least %d: %+v", len(inputs), len(want), inputs)
	}
	for i, label := range want {
		if inputs[i].Label != label {
			t.Errorf("inputs[%d].Label = %q, want %q", i, inputs[i].Label, label)
		}
		if inputs[i].Value == "" {
			t.Errorf("inputs[%d] (%s).Value is empty", i, label)
		}
	}
	// target should match what we passed in.
	for _, in := range inputs {
		if in.Label == "target" && in.Value != "x86_64-unknown-linux-gnu" {
			t.Errorf("target value = %q, want %q", in.Value, "x86_64-unknown-linux-gnu")
		}
		if in.Label == "mode" && in.Value != "debug" {
			t.Errorf("mode value = %q, want %q", in.Value, "debug")
		}
	}
}

func TestComputeProjectBinaryCacheInputsReleaseAndEmbed(t *testing.T) {
	dir := t.TempDir()
	writeProjectFile(t, dir, "promise.toml", "[module]\nname = \"app\"\nepoch = \"2026.0\"\n")
	embedPath := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(embedPath, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, dir, "main.pr", "`embed(\"data.txt\")\nblob bytes;\nmain() {}\n")

	inputs := computeProjectBinaryCacheInputs(dir, "x86_64-unknown-linux-gnu", true)
	if inputs == nil {
		t.Skipf("cache inputs not available (missing std hash)")
	}
	var foundMode, foundEmbed bool
	for _, in := range inputs {
		if in.Label == "mode" {
			foundMode = true
			if in.Value != "release" {
				t.Errorf("release mode value = %q, want %q", in.Value, "release")
			}
		}
		if in.Label == "embed" {
			foundEmbed = true
			if in.Value == "" {
				t.Error("embed value is empty")
			}
		}
	}
	if !foundMode {
		t.Error("inputs missing 'mode' label")
	}
	if !foundEmbed {
		t.Error("inputs missing 'embed' label")
	}
}

func TestComputeProjectBinaryCacheInputsMissingDir(t *testing.T) {
	inputs := computeProjectBinaryCacheInputs("/nonexistent/project/dir/12345", "x86_64-unknown-linux-gnu", false)
	if inputs != nil {
		t.Errorf("missing project dir should return nil inputs, got %+v", inputs)
	}
}

func TestParseRunArgs(t *testing.T) {
	cases := []struct {
		name            string
		args            []string
		wantFilename    string
		wantTarget      string
		wantReleaseMode bool
	}{
		{
			name:         "single file",
			args:         []string{"main.pr"},
			wantFilename: "main.pr",
		},
		{
			name:         "last-wins for multiple filenames (matches buildToFile)",
			args:         []string{"foo.pr", "bar.pr"},
			wantFilename: "bar.pr",
		},
		{
			name:         "target flag consumed before filename",
			args:         []string{"-target", "wasm32-wasi", "main.pr"},
			wantFilename: "main.pr",
			wantTarget:   "wasm32-wasi",
		},
		{
			name:            "release flag",
			args:            []string{"-release", "main.pr"},
			wantFilename:    "main.pr",
			wantReleaseMode: true,
		},
		{
			name:         "-o value is consumed (not treated as filename)",
			args:         []string{"-o", "out.bin", "main.pr"},
			wantFilename: "main.pr",
		},
		{
			name:         "flags interleaved with filename",
			args:         []string{"main.pr", "-target", "x86_64-unknown-linux-gnu"},
			wantFilename: "main.pr",
			wantTarget:   "x86_64-unknown-linux-gnu",
		},
		{
			name:         "no args produces empty filename",
			args:         []string{},
			wantFilename: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			filename, target, releaseMode := parseRunArgs(tc.args)
			if filename != tc.wantFilename {
				t.Errorf("filename = %q, want %q", filename, tc.wantFilename)
			}
			if target != tc.wantTarget {
				t.Errorf("target = %q, want %q", target, tc.wantTarget)
			}
			if releaseMode != tc.wantReleaseMode {
				t.Errorf("releaseMode = %v, want %v", releaseMode, tc.wantReleaseMode)
			}
		})
	}
}
