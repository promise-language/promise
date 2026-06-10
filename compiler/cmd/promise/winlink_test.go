package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// T0772: tests for the self-generated Windows zero-dependency link surface
// discovery/extraction. These mirror musl_test.go — winLink{ArchDir,Complete,
// Valid} and findWindowsLinkSurface follow exactly the findMuslCRT pattern,
// but resolve the embedded import libs (kernel32/advapi32/ws2_32/ucrtbase)
// instead of the musl CRT objects. The embedded FS is host-unconditional, so
// these run on every platform.

func TestWinLinkArchDir(t *testing.T) {
	// Only x86_64 is generated today; every supported triple maps to the same
	// arch subdir (arm64 is rejected earlier, in findWindowsLinkSurface).
	for _, target := range []string{
		"x86_64-pc-windows-msvc",
		"x86_64-unknown-windows-msvc",
	} {
		if got := winLinkArchDir(target); got != "windows-amd64" {
			t.Errorf("winLinkArchDir(%q) = %q, want %q", target, got, "windows-amd64")
		}
	}
}

func TestWinLinkCompleteEmpty(t *testing.T) {
	dir := t.TempDir()
	if winLinkComplete(dir) {
		t.Error("empty dir: expected false")
	}
}

func TestWinLinkCompletePartial(t *testing.T) {
	dir := t.TempDir()
	// Create only some of the required import libs.
	for _, name := range []string{"kernel32.lib", "advapi32.lib"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if winLinkComplete(dir) {
		t.Error("partial dir: expected false")
	}
}

func TestWinLinkCompleteAll(t *testing.T) {
	dir := t.TempDir()
	for _, name := range winLinkFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if !winLinkComplete(dir) {
		t.Error("complete dir: expected true")
	}
}

func TestWinLinkValidMissingFiles(t *testing.T) {
	// An empty directory should never be valid.
	dir := t.TempDir()
	if winLinkValid(dir, "windows-amd64") {
		t.Error("empty dir: expected false")
	}
}

// extractEmbeddedWinLink writes every embedded import lib for arch into dir.
func extractEmbeddedWinLink(t *testing.T, dir, arch string) {
	t.Helper()
	prefix := "resources/winlink/" + arch
	for _, name := range winLinkFiles {
		data, err := embeddedWinLink.ReadFile(prefix + "/" + name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWinLinkValidWithEmbedded(t *testing.T) {
	if !hasEmbeddedWinLink {
		t.Skip("no embedded Windows link surface in this binary")
	}
	const arch = "windows-amd64"
	dir := t.TempDir()
	extractEmbeddedWinLink(t, dir, arch)

	if !winLinkValid(dir, arch) {
		t.Error("correctly extracted embedded import libs: expected true")
	}

	// Corrupt one lib — the size mismatch must make the cache invalid so it
	// gets re-extracted rather than handed stale to lld-link.
	if err := os.WriteFile(filepath.Join(dir, "kernel32.lib"), []byte("corrupted"), 0644); err != nil {
		t.Fatal(err)
	}
	if winLinkValid(dir, arch) {
		t.Error("after corrupting kernel32.lib: expected false")
	}
}

func TestWinLinkValidCorrectArchMissingFiles(t *testing.T) {
	if !hasEmbeddedWinLink {
		t.Skip("no embedded Windows link surface in this binary")
	}
	// Correct arch but no files on disk — Stat fails, so invalid.
	dir := t.TempDir()
	if winLinkValid(dir, "windows-amd64") {
		t.Error("correct arch, missing files: expected false")
	}
}

func TestWinLinkValidWrongArch(t *testing.T) {
	if !hasEmbeddedWinLink {
		t.Skip("no embedded Windows link surface in this binary")
	}
	// A dir named with an arch not in the embedded FS should return false even
	// when it contains the right file names (ReadDir of the prefix fails).
	dir := t.TempDir()
	for _, name := range winLinkFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if winLinkValid(dir, "nonexistent-arch") {
		t.Error("unknown arch: expected false")
	}
}

func TestFindWindowsLinkSurfaceRejectsArm64(t *testing.T) {
	for _, target := range []string{
		"aarch64-pc-windows-msvc",
		"arm64-pc-windows-msvc",
	} {
		if _, err := findWindowsLinkSurface(target); err == nil {
			t.Errorf("findWindowsLinkSurface(%q): expected error for arm64, got nil", target)
		}
	}
}

func TestFindWindowsLinkSurfaceExtractsEmbedded(t *testing.T) {
	if !hasEmbeddedWinLink {
		t.Skip("no embedded Windows link surface in this binary")
	}
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	dir, err := findWindowsLinkSurface("x86_64-pc-windows-msvc")
	if err != nil {
		t.Fatalf("findWindowsLinkSurface failed: %v", err)
	}

	// Should have extracted into the cache dir under PROMISE_HOME.
	wantCache := filepath.Join(tmp, "cache", "winlink", "windows-amd64")
	if dir != wantCache {
		t.Errorf("link surface dir = %q, want %q", dir, wantCache)
	}
	if !winLinkComplete(dir) {
		t.Errorf("extracted dir %q is missing import libs", dir)
	}

	// Second call should be served from the now-valid cache (idempotent).
	dir2, err := findWindowsLinkSurface("x86_64-pc-windows-msvc")
	if err != nil {
		t.Fatalf("second findWindowsLinkSurface failed: %v", err)
	}
	if dir2 != dir {
		t.Errorf("second call returned %q, want %q", dir2, dir)
	}
}

func TestFindWindowsLinkSurfacePrefersSibling(t *testing.T) {
	// The first discovery step is a winlink/ dir sitting next to the promise
	// binary (os.Executable()). It must win over the installed location and the
	// embedded-extraction fallback. Stage both a sibling and an install copy and
	// assert the sibling is chosen.
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}
	siblingDir := filepath.Join(filepath.Dir(exe), "winlink", "windows-amd64")
	if err := os.MkdirAll(siblingDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Clean up the dir we planted next to the test binary so it can't leak into
	// any other test in this package (the sibling step runs before install/cache).
	t.Cleanup(func() { os.RemoveAll(filepath.Join(filepath.Dir(exe), "winlink")) })
	for _, name := range winLinkFiles {
		if err := os.WriteFile(filepath.Join(siblingDir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)
	// Also stage a complete install copy — the sibling must still win.
	installDir := filepath.Join(tmp, "lib", "winlink", "windows-amd64")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range winLinkFiles {
		if err := os.WriteFile(filepath.Join(installDir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	dir, err := findWindowsLinkSurface("x86_64-pc-windows-msvc")
	if err != nil {
		t.Fatalf("findWindowsLinkSurface failed: %v", err)
	}
	if dir != siblingDir {
		t.Errorf("link surface dir = %q, want sibling %q (sibling must beat install)", dir, siblingDir)
	}
}

func TestFindWindowsLinkSurfaceInstalledLocation(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// Pre-stage a complete installed location; it must win over extraction.
	installDir := filepath.Join(tmp, "lib", "winlink", "windows-amd64")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range winLinkFiles {
		if err := os.WriteFile(filepath.Join(installDir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	dir, err := findWindowsLinkSurface("x86_64-pc-windows-msvc")
	if err != nil {
		t.Fatalf("findWindowsLinkSurface failed: %v", err)
	}
	if dir != installDir {
		t.Errorf("link surface dir = %q, want installed %q", dir, installDir)
	}
}

func TestBuildWindowsLinkArgs(t *testing.T) {
	if !hasEmbeddedWinLink {
		t.Skip("no embedded Windows link surface in this binary")
	}
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	objs := []string{"a.obj", "b.obj"}
	args := buildWindowsLinkArgs("x86_64-pc-windows-msvc", objs, "out.exe")

	joined := strings.Join(args, " ")
	// Zero-dep entry point replaces the MSVC CRT's mainCRTStartup.
	if !containsArg(args, "/entry:__promise_start") {
		t.Errorf("missing /entry:__promise_start; args = %v", args)
	}
	if containsSubstr(args, "mainCRTStartup") {
		t.Errorf("must not reference mainCRTStartup; args = %v", args)
	}
	if !containsArg(args, "/out:out.exe") {
		t.Errorf("missing /out:out.exe; args = %v", args)
	}
	if !containsSubstr(args, "/libpath:") {
		t.Errorf("missing /libpath:; args = %v", args)
	}
	// Every self-generated import lib must be named, and no MSVC static CRT lib.
	for _, name := range winLinkFiles {
		if !containsArg(args, name) {
			t.Errorf("missing import lib %q; args = %v", name, args)
		}
	}
	for _, banned := range []string{"libcmt.lib", "libvcruntime.lib", "libucrt.lib"} {
		if containsArg(args, banned) {
			t.Errorf("must not link MSVC static CRT lib %q; args = %v", banned, args)
		}
	}
	// Object files are passed through, after the libs.
	for _, obj := range objs {
		if !containsArg(args, obj) {
			t.Errorf("missing object file %q; args = %v", obj, args)
		}
	}
	_ = joined
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func containsSubstr(args []string, sub string) bool {
	for _, a := range args {
		if strings.Contains(a, sub) {
			return true
		}
	}
	return false
}
