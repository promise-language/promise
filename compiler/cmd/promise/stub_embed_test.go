package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadInstalledStubVersionMissing(t *testing.T) {
	dir := t.TempDir()
	// No sidecar present → version 0 (so a fresh install always forward-updates).
	if v := readInstalledStubVersion(dir); v != 0 {
		t.Fatalf("expected 0 for missing sidecar, got %d", v)
	}
}

func TestReadInstalledStubVersionValid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stubVersionSidecar), []byte("7\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if v := readInstalledStubVersion(dir); v != 7 {
		t.Fatalf("expected 7, got %d", v)
	}
}

func TestReadInstalledStubVersionGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stubVersionSidecar), []byte("not-a-number"), 0644); err != nil {
		t.Fatal(err)
	}
	// Unparseable sidecar → 0, never panics, never executes the stub.
	if v := readInstalledStubVersion(dir); v != 0 {
		t.Fatalf("expected 0 for garbage sidecar, got %d", v)
	}
}

// TestForwardOnlyDecision exercises the version comparison that gates a stub
// update: the installer replaces the stub only when its embedded version is
// strictly newer than the installed sidecar value (never downgrades).
func TestForwardOnlyDecision(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		installed string // sidecar contents ("" = absent)
		embedded  int
		replace   bool
	}{
		{"", 1, true},    // fresh install
		{"1", 2, true},   // newer embedded → replace
		{"2", 2, false},  // equal → keep
		{"3", 2, false},  // installed newer → never downgrade
		{"bad", 1, true}, // unreadable installed treated as 0 → replace
	}
	for _, c := range cases {
		if c.installed == "" {
			os.Remove(filepath.Join(dir, stubVersionSidecar))
		} else {
			if err := os.WriteFile(filepath.Join(dir, stubVersionSidecar), []byte(c.installed), 0644); err != nil {
				t.Fatal(err)
			}
		}
		got := c.embedded > readInstalledStubVersion(dir)
		if got != c.replace {
			t.Errorf("installed=%q embedded=%d: expected replace=%v, got %v", c.installed, c.embedded, c.replace, got)
		}
	}
}

// TestReadEmbeddedStubDevBuild: dev builds (no embed_stub tag) carry no stub,
// so readEmbeddedStub reports a clear error rather than panicking or returning
// empty bytes. Release builds (T0773) supply the per-target binary.
func TestReadEmbeddedStubDevBuild(t *testing.T) {
	if hasEmbeddedStub {
		t.Skip("build has an embedded stub; this guards the dev-build path")
	}
	_, err := readEmbeddedStub("promise")
	if err == nil {
		t.Fatal("expected an error reading an embedded stub in a dev build")
	}
	if !strings.Contains(err.Error(), "no embedded stub") {
		t.Fatalf("expected 'no embedded stub' error, got: %v", err)
	}
}

// TestWriteStubAndSidecarDevBuild: with no embedded stub, writeStubAndSidecar
// fails (because readEmbeddedStub fails) and must NOT leave a stub binary or a
// sidecar behind — a half-written launcher would be worse than none.
func TestWriteStubAndSidecarDevBuild(t *testing.T) {
	if hasEmbeddedStub {
		t.Skip("build has an embedded stub; this guards the dev-build path")
	}
	dir := t.TempDir()
	if err := writeStubAndSidecar(dir, "promise"); err == nil {
		t.Fatal("expected writeStubAndSidecar to fail without an embedded stub")
	}
	if _, err := os.Stat(filepath.Join(dir, "promise")); !os.IsNotExist(err) {
		t.Error("no stub binary should be written when the embedded stub is absent")
	}
	if _, err := os.Stat(filepath.Join(dir, stubVersionSidecar)); !os.IsNotExist(err) {
		t.Error("no sidecar should be written when the embedded stub is absent")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "thing")
	if err := writeFileAtomic(path, []byte("hello"), 0755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("expected 'hello', got %q", string(data))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("expected mode 0755, got %v", info.Mode().Perm())
	}
	// Overwrite atomically — no leftover temp files in the dir.
	if err := writeFileAtomic(path, []byte("world"), 0644); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "world" {
		t.Fatalf("expected 'world', got %q", string(data))
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 file (no temp leftovers), got %d", len(entries))
	}
}
