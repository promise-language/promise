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
			args:         []string{"--target", "wasm32-wasi", "main.pr"},
			wantFilename: "main.pr",
			wantTarget:   "wasm32-wasi",
		},
		{
			name:            "release flag",
			args:            []string{"--release", "main.pr"},
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
			args:         []string{"main.pr", "--target", "x86_64-unknown-linux-gnu"},
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
