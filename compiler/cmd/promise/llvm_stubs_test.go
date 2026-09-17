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
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "opt"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	written, err := writeToolchainStubs(dir)
	if err != nil {
		t.Fatalf("writeToolchainStubs: %v", err)
	}
	// Nothing to stub still writes the marker, and the marker is a real cost —
	// zero here only on a host that needs no stubs at all (T2143).
	if want := writtenBytes(t, dir); written != want {
		t.Errorf("writeToolchainStubs reported %d bytes, but wrote %d", written, want)
	}
	marker := filepath.Join(dir, stubMarkerName)
	_, err = os.Stat(marker)
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
	t.Parallel()
	if !needsToolchainStubs() {
		t.Skip("toolchain stubs are a Linux concern")
	}
	lld := stagedLLDForTest(t)
	dir := t.TempDir()
	if err := os.Symlink(lld, filepath.Join(dir, "lld")); err != nil {
		t.Fatal(err)
	}
	written, err := writeToolchainStubs(dir)
	if err != nil {
		t.Fatalf("writeToolchainStubs: %v", err)
	}
	marker, err := os.ReadFile(filepath.Join(dir, stubMarkerName))
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	// T2143: the stubs are part of what a cold Linux view COSTS, and on Linux
	// they are most of it — the tools themselves are symlinks. A byte count that
	// forgot them would understate every cold run on the one platform where the
	// rest of the view is free.
	if want := writtenBytes(t, dir); written != want {
		t.Errorf("writeToolchainStubs reported %d bytes, but wrote %d", written, want)
	}
	if written == 0 {
		t.Error("a view that generated a stub reported costing nothing")
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
// Deliberately not parallel. It asserts on the exact stderr of a stand-in tool,
// and the parallel phase of this package spawns hundreds of processes at once;
// a fork that fails under that load would surface as a different message and
// fail the test for a reason it is not about. It costs 0.00s, so running it in
// the sequential phase is free (T1778).
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

// writtenBytes is the size of everything writeToolchainStubs put in a view dir:
// the generated stub libraries and the marker. The tools the caller staged are
// excluded — a symlink or a hardlink into the CAS costs no bytes, which is the
// distinction the accounting exists to make.
func writtenBytes(t *testing.T, viewDir string) int64 {
	t.Helper()
	marker, err := os.ReadFile(filepath.Join(viewDir, stubMarkerName))
	if err != nil {
		if os.IsNotExist(err) {
			return 0 // a host that needs no stubs writes nothing at all
		}
		t.Fatalf("marker: %v", err)
	}
	total := int64(len(marker))
	for _, name := range strings.Split(string(marker), "\n") {
		if name == "" {
			continue
		}
		info, serr := os.Stat(filepath.Join(viewDir, name))
		if serr != nil {
			t.Fatalf("stub %s named in the marker is missing: %v", name, serr)
		}
		total += info.Size()
	}
	return total
}
