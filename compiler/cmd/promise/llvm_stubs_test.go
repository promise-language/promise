package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestWriteToolchainStubsMarksEvenWithNothingToStub pins the fast-path
// contract: the marker distinguishes "a compiler that checked" from "a view
// staged before stubs existed", so it is written even when no tool needs one.
func TestWriteToolchainStubsMarksEvenWithNothingToStub(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "opt"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeToolchainStubs(dir); err != nil {
		t.Fatalf("writeToolchainStubs: %v", err)
	}
	marker := filepath.Join(dir, stubMarkerName)
	_, err := os.Stat(marker)
	if needsToolchainStubs() && err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	if !needsToolchainStubs() && err == nil {
		t.Error("marker written on a host that needs no stubs")
	}
}

// TestWriteToolchainStubsCoversLLD is the wiring test for T1774: given the lld
// this checkout has staged, the view dir ends up with the stub library it needs
// to load, named in the marker.
func TestWriteToolchainStubsCoversLLD(t *testing.T) {
	if !needsToolchainStubs() {
		t.Skip("toolchain stubs are a Linux concern")
	}
	lld := stagedLLDForTest(t)
	dir := t.TempDir()
	if err := os.Symlink(lld, filepath.Join(dir, "lld")); err != nil {
		t.Fatal(err)
	}
	if err := writeToolchainStubs(dir); err != nil {
		t.Fatalf("writeToolchainStubs: %v", err)
	}
	marker, err := os.ReadFile(filepath.Join(dir, stubMarkerName))
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	if !strings.Contains(string(marker), "libxml2.so.2") {
		t.Fatalf("marker = %q, want it to name libxml2.so.2", marker)
	}
	if _, err := os.Stat(filepath.Join(dir, "libxml2.so.2")); err != nil {
		t.Fatalf("stub library not generated: %v", err)
	}
}

// TestLLVMToolProbeReportsUnrunnableTool covers the doctor gap this bug exposed:
// a tool that is present but cannot execute — the shape of a missing host
// library — must be reported as such, not silently as "no version".
func TestLLVMToolProbeReportsUnrunnableTool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stand-in is POSIX-only")
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "ld.lld")
	script := "#!/bin/sh\necho 'ld.lld: error while loading shared libraries: libxml2.so.2: cannot open shared object file' >&2\nexit 127\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	v, err := llvmToolProbe(tool)
	if err == nil {
		t.Fatal("an unrunnable tool probed clean")
	}
	if v != 0 {
		t.Errorf("version = %d, want 0", v)
	}
	if !strings.Contains(err.Error(), "libxml2") {
		t.Errorf("error = %v, want the loader's own message", err)
	}
}

// stagedLLDForTest finds the lld this checkout staged, skipping when there is
// none so a fresh clone that has never built stays green.
func stagedLLDForTest(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir to search for a staged lld")
	}
	for _, pat := range []string{
		filepath.Join(home, ".promise", "cache", "llvm-view", "*", "lld"),
		filepath.Join(home, ".cache", "promise", "prebuilts", "llvm-slim", "*", "*", "lld"),
	} {
		matches, _ := filepath.Glob(pat)
		for _, m := range matches {
			if fi, serr := os.Stat(m); serr == nil && !fi.IsDir() {
				return m
			}
		}
	}
	t.Skip("no staged lld on this host (run bin/build first)")
	return ""
}
