package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMuslArchDir(t *testing.T) {
	t.Parallel()
	tests := []struct {
		target string
		want   string
	}{
		{"aarch64-unknown-linux-musl", "aarch64-linux-musl"},
		{"aarch64-unknown-linux-gnu", "aarch64-linux-musl"},
		{"aarch64-linux-musl", "aarch64-linux-musl"},
		{"x86_64-unknown-linux-musl", "x86_64-linux-musl"},
		{"x86_64-pc-linux-gnu", "x86_64-linux-musl"},
		{"x86_64-unknown-linux-gnu", "x86_64-linux-musl"},
	}
	for _, tt := range tests {
		got := muslArchDir(tt.target)
		if got != tt.want {
			t.Errorf("muslArchDir(%q) = %q, want %q", tt.target, got, tt.want)
		}
	}
}

// TestMuslManifestName pins the arch-qualified runtime-manifest name format.
// The producer (MuslManifestName in tools/build/common/musl_slim.go) lives in a
// separate Go module and duplicates this format by necessity — a silent drift
// here makes every musl blob unresolvable, and the failure mode is a fall-
// through to the embedded CRT rather than a loud error, so pin it.
func TestMuslManifestName(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		arch, file, want string
	}{
		{"x86_64-linux-musl", "crt1.o", "musl-x86_64-linux-musl-crt1.o"},
		{"aarch64-linux-musl", "libc.a", "musl-aarch64-linux-musl-libc.a"},
	} {
		if got := muslManifestName(tt.arch, tt.file); got != tt.want {
			t.Errorf("muslManifestName(%q, %q) = %q, want %q", tt.arch, tt.file, got, tt.want)
		}
	}
	// Arches must not collide — the reason the name is qualified at all.
	if muslManifestName("x86_64-linux-musl", "crt1.o") == muslManifestName("aarch64-linux-musl", "crt1.o") {
		t.Error("arch-qualified names collided across arches")
	}
}

func TestMuslCRTCompleteEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if muslCRTComplete(dir) {
		t.Error("empty dir: expected false")
	}
}

func TestMuslCRTCompletePartial(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create only some of the required files.
	for _, name := range []string{"crt1.o", "crti.o"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if muslCRTComplete(dir) {
		t.Error("partial dir: expected false")
	}
}

func TestMuslCRTCompleteAll(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range muslCRTFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if !muslCRTComplete(dir) {
		t.Error("complete dir: expected true")
	}
}

func TestMuslCRTValidMissingFiles(t *testing.T) {
	t.Parallel()
	// An empty directory should never be valid.
	dir := t.TempDir()
	if muslCRTValid(dir) {
		t.Error("empty dir: expected false")
	}
}

func TestMuslCRTValidWithEmbedded(t *testing.T) {
	t.Parallel()
	if !hasEmbeddedMuslCRT {
		t.Skip("no embedded CRT on this platform")
	}

	// Determine the arch dir name matching the embedded FS.
	arch := "x86_64-linux-musl"
	if runtime.GOARCH == "arm64" {
		arch = "aarch64-linux-musl"
	}

	// Extract all embedded CRT files to a temp dir named after the arch.
	base := t.TempDir()
	crtDir := filepath.Join(base, arch)
	if err := os.MkdirAll(crtDir, 0755); err != nil {
		t.Fatal(err)
	}
	prefix := "resources/crt/" + arch
	for _, name := range muslCRTFiles {
		data, err := embeddedMuslCRT.ReadFile(prefix + "/" + name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(crtDir, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}

	if !muslCRTValid(crtDir) {
		t.Error("correctly extracted embedded CRT: expected true")
	}

	// Corrupt one file — size mismatch should make it invalid.
	if err := os.WriteFile(filepath.Join(crtDir, "libc.a"), []byte("corrupted"), 0644); err != nil {
		t.Fatal(err)
	}
	if muslCRTValid(crtDir) {
		t.Error("after corrupting libc.a: expected false")
	}
}

func TestMuslCRTValidCorrectArchMissingFiles(t *testing.T) {
	t.Parallel()
	if !hasEmbeddedMuslCRT {
		t.Skip("no embedded CRT on this platform")
	}
	// Dir named with the correct arch but no files inside — Stat will fail.
	arch := "x86_64-linux-musl"
	if runtime.GOARCH == "arm64" {
		arch = "aarch64-linux-musl"
	}
	base := t.TempDir()
	crtDir := filepath.Join(base, arch)
	os.MkdirAll(crtDir, 0755)
	if muslCRTValid(crtDir) {
		t.Error("correct arch, missing files: expected false")
	}
}

func TestMuslCRTValidWrongArch(t *testing.T) {
	t.Parallel()
	if !hasEmbeddedMuslCRT {
		t.Skip("no embedded CRT on this platform")
	}
	// A dir named with an arch not in the embedded FS should return false.
	base := t.TempDir()
	crtDir := filepath.Join(base, "nonexistent-arch-linux-musl")
	os.MkdirAll(crtDir, 0755)
	for _, name := range muslCRTFiles {
		os.WriteFile(filepath.Join(crtDir, name), []byte("x"), 0644)
	}
	if muslCRTValid(crtDir) {
		t.Error("unknown arch dir: expected false")
	}
}
