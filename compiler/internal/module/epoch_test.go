package module

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompilerEpoch(t *testing.T) {
	data := []byte(`[catalog]
epoch = "2026.0"

[modules.std]
description = "Standard library"
`)
	epoch, err := CompilerEpoch(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if epoch != "2026.0" {
		t.Fatalf("expected 2026.0, got %s", epoch)
	}
}

func TestCompilerEpochMissing(t *testing.T) {
	data := []byte(`[catalog]
`)
	_, err := CompilerEpoch(data)
	if err == nil {
		t.Fatal("expected error for missing epoch")
	}
}

func TestCompilerEpochInvalid(t *testing.T) {
	_, err := CompilerEpoch([]byte(`[bad section`))
	if err == nil {
		t.Fatal("expected error for invalid catalog")
	}
}

func TestEpochDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	dir, err := EpochDir("2026.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(tmp, "epochs", "2026.0")
	if dir != want {
		t.Fatalf("expected %s, got %s", want, dir)
	}
}

func TestActiveEpochFromFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	if err := os.WriteFile(filepath.Join(tmp, "active"), []byte("2026.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	epoch, err := ActiveEpoch()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if epoch != "2026.0" {
		t.Fatalf("expected 2026.0, got %s", epoch)
	}
}

func TestActiveEpochFallback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// Create two epoch dirs — should pick the lexicographically last.
	for _, name := range []string{"2026.0", "2026.2"} {
		if err := os.MkdirAll(filepath.Join(tmp, "epochs", name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	epoch, err := ActiveEpoch()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if epoch != "2026.2" {
		t.Fatalf("expected 2026.2, got %s", epoch)
	}
}

func TestActiveEpochNoEpochs(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	_, err := ActiveEpoch()
	if err == nil {
		t.Fatal("expected error when no epochs installed")
	}
}

func TestWriteActiveEpoch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	if err := WriteActiveEpoch("dev"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tmp, "active"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "dev\n" {
		t.Fatalf("expected 'dev\\n', got %q", string(data))
	}
}

func TestActiveEpochRaw(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// Absent active file → ("", false, nil) with no latest-installed fallback,
	// even when an epoch directory exists.
	if err := os.MkdirAll(filepath.Join(tmp, "epochs", "2026.0"), 0755); err != nil {
		t.Fatal(err)
	}
	if epoch, had, err := ActiveEpochRaw(); err != nil || had || epoch != "" {
		t.Fatalf("absent active: got (%q, %v, %v), want (\"\", false, nil)", epoch, had, err)
	}

	// Present + non-empty → returns the trimmed value.
	if err := WriteActiveEpoch("2026.0"); err != nil {
		t.Fatal(err)
	}
	if epoch, had, err := ActiveEpochRaw(); err != nil || !had || epoch != "2026.0" {
		t.Fatalf("set active: got (%q, %v, %v), want (\"2026.0\", true, nil)", epoch, had, err)
	}

	// Whitespace-only file is treated as absent.
	if err := os.WriteFile(filepath.Join(tmp, "active"), []byte("  \n"), 0644); err != nil {
		t.Fatal(err)
	}
	if epoch, had, err := ActiveEpochRaw(); err != nil || had || epoch != "" {
		t.Fatalf("blank active: got (%q, %v, %v), want (\"\", false, nil)", epoch, had, err)
	}
}

func TestClearActiveEpoch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// Idempotent when the file is absent.
	if err := ClearActiveEpoch(); err != nil {
		t.Fatalf("clear (absent): unexpected error: %v", err)
	}

	if err := WriteActiveEpoch("2026.0"); err != nil {
		t.Fatal(err)
	}
	if err := ClearActiveEpoch(); err != nil {
		t.Fatalf("clear (present): unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "active")); !os.IsNotExist(err) {
		t.Fatalf("active file should be removed, stat err: %v", err)
	}
	// Second clear is still a no-op.
	if err := ClearActiveEpoch(); err != nil {
		t.Fatalf("clear (already removed): unexpected error: %v", err)
	}
}

func TestInstalledEpochs(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// No epochs dir → empty list, no error.
	epochs, err := InstalledEpochs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(epochs) != 0 {
		t.Fatalf("expected empty, got %v", epochs)
	}

	// Create some epoch dirs and a non-dir file.
	epochsDir := filepath.Join(tmp, "epochs")
	os.MkdirAll(filepath.Join(epochsDir, "2026.0"), 0755)
	os.MkdirAll(filepath.Join(epochsDir, "dev"), 0755)
	os.WriteFile(filepath.Join(epochsDir, "ignored-file"), []byte("x"), 0644)

	epochs, err = InstalledEpochs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(epochs) != 2 {
		t.Fatalf("expected 2 epochs, got %v", epochs)
	}
	// Sorted: "2026.0" < "dev"
	if epochs[0] != "2026.0" || epochs[1] != "dev" {
		t.Fatalf("unexpected order: %v", epochs)
	}
}

func TestInstalledEpochsFileAtPath(t *testing.T) {
	// A regular file at <PromiseHome>/epochs must produce an error rather than
	// being silently treated as "no epochs" — Go's os.ReadDir is inconsistent
	// here across platforms (returns ENOTDIR on Linux, but ([]DirEntry{}, nil)
	// on Windows due to a quirk in dir_windows.go). InstalledEpochs uses an
	// os.Stat guard to make the behavior consistent.
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	if err := os.WriteFile(filepath.Join(tmp, "epochs"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	epochs, err := InstalledEpochs()
	if err == nil {
		t.Fatalf("expected error when epochs path is a file, got %v", epochs)
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("expected 'not a directory' in error, got %q", err.Error())
	}
	if epochs != nil {
		t.Errorf("expected nil epochs on error, got %v", epochs)
	}
}

func TestActiveEpochPropagatesInstalledError(t *testing.T) {
	// When no <PromiseHome>/active file exists, ActiveEpoch falls back to
	// InstalledEpochs. If that errors (e.g., epochs path is a file), the
	// error must propagate rather than be swallowed.
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	if err := os.WriteFile(filepath.Join(tmp, "epochs"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := ActiveEpoch()
	if err == nil {
		t.Fatal("expected error when epochs path is a file")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("expected 'not a directory' in error, got %q", err.Error())
	}
}

func TestEpochDirRemoval(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// Create an epoch directory with some files.
	epochDir, _ := EpochDir("2026.2")
	os.MkdirAll(filepath.Join(epochDir, "bin"), 0755)
	os.MkdirAll(filepath.Join(epochDir, "cache", "build"), 0755)
	os.WriteFile(filepath.Join(epochDir, "bin", "promise"), []byte("binary"), 0755)
	os.WriteFile(filepath.Join(epochDir, "cache", "build", "key.o"), []byte("obj"), 0644)

	// Verify it exists.
	epochs, _ := InstalledEpochs()
	if len(epochs) != 1 || epochs[0] != "2026.2" {
		t.Fatalf("expected [2026.2], got %v", epochs)
	}

	// Remove it via os.RemoveAll (same as runRemove does).
	if err := os.RemoveAll(epochDir); err != nil {
		t.Fatalf("RemoveAll failed: %v", err)
	}

	// Verify it's gone.
	epochs, _ = InstalledEpochs()
	if len(epochs) != 0 {
		t.Fatalf("expected empty, got %v", epochs)
	}
}

func TestCompareEpochs(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// The latent lexicographic bug: "2026.10" must rank ABOVE "2026.9".
		{"2026.10", "2026.9", 1},
		{"2026.9", "2026.10", -1},
		// Year rollover.
		{"2027.0", "2026.5", 1},
		{"2026.5", "2027.0", -1},
		// Equality.
		{"2026.0", "2026.0", 0},
		// Minor compare within a year.
		{"2026.1", "2026.0", 1},
		// Non-numeric epochs fall back to string compare (never panic).
		{"next", "next", 0},
		{"2026.0", "next", strings_compare("2026.0", "next")},
	}
	for _, c := range cases {
		got := CompareEpochs(c.a, c.b)
		// Normalize sign for the non-numeric fallback case.
		if normSign(got) != normSign(c.want) {
			t.Errorf("CompareEpochs(%q, %q) = %d, want sign of %d", c.a, c.b, got, c.want)
		}
	}
}

// strings_compare mirrors strings.Compare for the fallback expectation without
// importing strings into the test's expectation literal.
func strings_compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func normSign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

func TestEpochBuildIDRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// Unrecorded build-id → empty, no error (treated as "update available").
	id, err := ReadEpochBuildID("next")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "" {
		t.Fatalf("expected empty build-id when unrecorded, got %q", id)
	}

	const sha = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if err := WriteEpochBuildID("next", sha); err != nil {
		t.Fatalf("WriteEpochBuildID: %v", err)
	}
	got, err := ReadEpochBuildID("next")
	if err != nil {
		t.Fatalf("ReadEpochBuildID: %v", err)
	}
	if got != sha {
		t.Fatalf("build-id round-trip mismatch: wrote %q, read %q", sha, got)
	}

	// The file lives under the epoch directory.
	dir, _ := EpochDir("next")
	if _, statErr := os.Stat(filepath.Join(dir, "build-id")); statErr != nil {
		t.Fatalf("expected build-id file under epoch dir: %v", statErr)
	}
}

func TestActiveEpochDevName(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// "dev" is a valid epoch name, not a path.
	if err := WriteActiveEpoch("dev"); err != nil {
		t.Fatal(err)
	}
	epoch, err := ActiveEpoch()
	if err != nil {
		t.Fatal(err)
	}
	if epoch != "dev" {
		t.Fatalf("expected dev, got %s", epoch)
	}
}
