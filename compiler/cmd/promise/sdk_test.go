package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBundledLibSystemTBDContent(t *testing.T) {
	// Verify the TBD constant is well-formed and contains essential symbols.
	if !strings.Contains(bundledLibSystemTBD, "tbd-version:     4") {
		t.Error("TBD should be version 4")
	}
	if !strings.Contains(bundledLibSystemTBD, "arm64-macos") {
		t.Error("TBD should target arm64-macos")
	}
	if !strings.Contains(bundledLibSystemTBD, "x86_64-macos") {
		t.Error("TBD should target x86_64-macos")
	}
	if !strings.Contains(bundledLibSystemTBD, "/usr/lib/libSystem.B.dylib") {
		t.Error("TBD should reference libSystem.B.dylib")
	}

	// Verify essential symbol categories are present.
	essentialSymbols := []string{
		"_malloc", "_free", "_realloc", // memory
		"_pthread_create", "_pthread_join", // threading
		"_write", "_read", "_exit", // I/O
		"_socket", "_bind", "_listen", // networking
		"_kqueue", "_kevent", // macOS events
		"_sin", "_cos", "_sqrt", // math
		"___error",         // errno
		"dyld_stub_binder", // dynamic linker
	}
	for _, sym := range essentialSymbols {
		if !strings.Contains(bundledLibSystemTBD, sym) {
			t.Errorf("TBD missing essential symbol: %s", sym)
		}
	}
}

func TestEnsureBundledSDKFresh(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	info, err := ensureBundledSDK()
	if err != nil {
		t.Fatalf("ensureBundledSDK failed: %v", err)
	}

	// Verify sysroot points to cache/sdk/macos.
	expectedSysroot := filepath.Join(tmp, "cache", "sdk", "macos")
	if info.sysroot != expectedSysroot {
		t.Errorf("sysroot = %q, want %q", info.sysroot, expectedSysroot)
	}
	if info.sdkVersion != "" {
		t.Errorf("sdkVersion should be empty for bundled SDK, got %q", info.sdkVersion)
	}

	// Verify TBD file was written.
	tbdPath := filepath.Join(expectedSysroot, "usr", "lib", "libSystem.B.tbd")
	data, err := os.ReadFile(tbdPath)
	if err != nil {
		t.Fatalf("TBD file not created: %v", err)
	}
	if string(data) != bundledLibSystemTBD {
		t.Error("TBD file content does not match bundled constant")
	}

	// Verify symlink was created.
	symlinkPath := filepath.Join(expectedSysroot, "usr", "lib", "libSystem.tbd")
	target, err := os.Readlink(symlinkPath)
	if err != nil {
		t.Fatalf("symlink not created: %v", err)
	}
	if target != "libSystem.B.tbd" {
		t.Errorf("symlink target = %q, want %q", target, "libSystem.B.tbd")
	}
}

func TestEnsureBundledSDKIdempotent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// First call creates everything.
	info1, err := ensureBundledSDK()
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}

	// Second call should succeed without error.
	info2, err := ensureBundledSDK()
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}

	if info1.sysroot != info2.sysroot {
		t.Errorf("sysroot changed between calls: %q vs %q", info1.sysroot, info2.sysroot)
	}
}

func TestEnsureBundledSDKRewritesOnSizeMismatch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// Pre-create the TBD file with wrong content (different size).
	libDir := filepath.Join(tmp, "cache", "sdk", "macos", "usr", "lib")
	if err := os.MkdirAll(libDir, 0755); err != nil {
		t.Fatal(err)
	}
	tbdPath := filepath.Join(libDir, "libSystem.B.tbd")
	if err := os.WriteFile(tbdPath, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}

	// ensureBundledSDK should overwrite the stale file.
	if _, err := ensureBundledSDK(); err != nil {
		t.Fatalf("ensureBundledSDK failed: %v", err)
	}

	data, err := os.ReadFile(tbdPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != bundledLibSystemTBD {
		t.Error("stale TBD file was not overwritten")
	}
}

func TestEnsureBundledSDKSkipsWriteWhenCurrent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// First call writes the file.
	if _, err := ensureBundledSDK(); err != nil {
		t.Fatal(err)
	}

	tbdPath := filepath.Join(tmp, "cache", "sdk", "macos", "usr", "lib", "libSystem.B.tbd")
	info1, err := os.Stat(tbdPath)
	if err != nil {
		t.Fatal(err)
	}

	// Second call should skip the write (same size).
	if _, err := ensureBundledSDK(); err != nil {
		t.Fatal(err)
	}

	info2, err := os.Stat(tbdPath)
	if err != nil {
		t.Fatal(err)
	}

	// ModTime should be unchanged (file was not rewritten).
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Error("TBD file was rewritten despite matching size")
	}
}

func TestFindMacOSSDKSucceeds(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-only test")
	}

	info, err := findMacOSSDK()
	if err != nil {
		t.Fatalf("findMacOSSDK failed: %v", err)
	}

	if info.sysroot == "" {
		t.Error("sysroot should not be empty")
	}
	if _, err := os.Stat(info.sysroot); err != nil {
		t.Errorf("sysroot path does not exist: %s", info.sysroot)
	}
}

func TestFindMacOSSDKFallback(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-only test")
	}

	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// Make xcrun unfindable by clearing PATH.
	t.Setenv("PATH", "/nonexistent")

	info, err := findMacOSSDK()
	if err != nil {
		t.Fatalf("findMacOSSDK should fall back to bundled SDK: %v", err)
	}

	// Should have used bundled SDK (sysroot in our temp PROMISE_HOME).
	if !strings.Contains(info.sysroot, "cache/sdk/macos") {
		t.Errorf("expected bundled SDK sysroot, got %q", info.sysroot)
	}
	if info.sdkVersion != "" {
		t.Errorf("bundled SDK should have empty sdkVersion, got %q", info.sdkVersion)
	}

	// Verify the TBD file exists.
	tbdPath := filepath.Join(info.sysroot, "usr", "lib", "libSystem.B.tbd")
	if _, err := os.Stat(tbdPath); err != nil {
		t.Errorf("bundled TBD file not created: %v", err)
	}
}
