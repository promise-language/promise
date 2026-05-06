package module

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCompilerEpoch(t *testing.T) {
	data := []byte(`[catalog]
epoch = "2026.3"

[modules.std]
description = "Standard library"
`)
	epoch, err := CompilerEpoch(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if epoch != "2026.3" {
		t.Fatalf("expected 2026.3, got %s", epoch)
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

	dir, err := EpochDir("2026.3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(tmp, "epochs", "2026.3")
	if dir != want {
		t.Fatalf("expected %s, got %s", want, dir)
	}
}

func TestActiveEpochFromFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	if err := os.WriteFile(filepath.Join(tmp, "active"), []byte("2026.3\n"), 0644); err != nil {
		t.Fatal(err)
	}
	epoch, err := ActiveEpoch()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if epoch != "2026.3" {
		t.Fatalf("expected 2026.3, got %s", epoch)
	}
}

func TestActiveEpochFallback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// Create two epoch dirs — should pick the lexicographically last.
	for _, name := range []string{"2026.2", "2026.3"} {
		if err := os.MkdirAll(filepath.Join(tmp, "epochs", name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	epoch, err := ActiveEpoch()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if epoch != "2026.3" {
		t.Fatalf("expected 2026.3, got %s", epoch)
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
	os.MkdirAll(filepath.Join(epochsDir, "2026.3"), 0755)
	os.MkdirAll(filepath.Join(epochsDir, "dev"), 0755)
	os.WriteFile(filepath.Join(epochsDir, "ignored-file"), []byte("x"), 0644)

	epochs, err = InstalledEpochs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(epochs) != 2 {
		t.Fatalf("expected 2 epochs, got %v", epochs)
	}
	// Sorted: "2026.3" < "dev"
	if epochs[0] != "2026.3" || epochs[1] != "dev" {
		t.Fatalf("unexpected order: %v", epochs)
	}
}
