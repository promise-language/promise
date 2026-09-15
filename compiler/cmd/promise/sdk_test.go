package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestBundledLibSystemTBDContent(t *testing.T) {
	t.Parallel()
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

	// Verify essential symbol categories are present. This is a cheap smoke
	// check, not the source of truth — TestBundledLibSystemLinksRepresentativePrograms
	// (darwin_libsystem_link_test.go) derives the actual requirement by linking
	// real compiled programs and is what catches drift (T1609).
	essentialSymbols := []string{
		"_malloc", "_free", "_realloc", // memory
		"_pthread_create", "_pthread_join", // threading
		"_write", "_read", "_exit", // I/O
		"_stat", "_lstat", "_fstat", "_rename", "_fsync", "_ftruncate", "_flock", // file metadata/ops
		"_socket", "_bind", "_listen", // networking
		"_inet_ntop", "_getentropy", // networking / entropy
		"_kqueue", "_kevent", // macOS events
		"_clock_gettime", "_memmove", "_bzero", // timing / backend-injected
		"_execv", "_setpgid", "_proc_pidinfo", // process
		"_sin", "_cos", "_sqrt", "___sincos_stret", // math
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

// TestFindMacOSSDKNeverConsultsXcrun is the regression guard for T1609: no host
// Xcode/CommandLineTools state is a build input any more, so findMacOSSDK must
// always return the bundled stub — even on a host where xcrun is present,
// licensed and would happily report a real (or, per T1609, unparseable) SDK.
// A fake xcrun that succeeds is planted on PATH; if findMacOSSDK is ever changed
// to consult it again, this test's sysroot check fails instead of the regression
// waiting for another Xcode release to surface it.
func TestFindMacOSSDKNeverConsultsXcrun(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	if runtime.GOOS != "windows" {
		fakeDir := t.TempDir()
		fakeXcrun := filepath.Join(fakeDir, "xcrun")
		if err := os.WriteFile(fakeXcrun, []byte("#!/bin/sh\necho /fake/xcode/sdk\n"), 0755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}

	info, err := findMacOSSDK()
	if err != nil {
		t.Fatalf("findMacOSSDK failed: %v", err)
	}

	if !strings.Contains(info.sysroot, filepath.Join("cache", "sdk", "macos")) {
		t.Errorf("findMacOSSDK must always return the bundled sysroot, got %q", info.sysroot)
	}
	if info.sysroot == "/fake/xcode/sdk" {
		t.Fatal("findMacOSSDK invoked xcrun from PATH — the host SDK must never be consulted (T1609)")
	}

	// Verify the TBD file exists.
	tbdPath := filepath.Join(info.sysroot, "usr", "lib", "libSystem.B.tbd")
	if _, err := os.Stat(tbdPath); err != nil {
		t.Errorf("bundled TBD file not created: %v", err)
	}
}

// TestBuildDarwinLinkArgsPlatformVersionUsesDeploymentTarget pins that, with no
// SDK version ever available (T1609 — the bundled stub reports none), both
// -platform_version arguments fall back to the same deployment-target value
// rather than one of them silently going empty. linkDarwinMulti builds this
// argument the same way from the same two (tri.minVersion, tri.minVersion)
// values, so this one pure-function call covers both call sites.
func TestBuildDarwinLinkArgsPlatformVersionUsesDeploymentTarget(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	want := "-platform_version macos 14.0.0 14.0.0"
	got := strings.Join(buildDarwinLinkArgs("arm64-apple-macosx14.0.0", "/tmp/x.o", "/tmp/x", false), " ")
	if !strings.Contains(got, want) {
		t.Errorf("platform_version args = %q, want to contain %q", got, want)
	}
}

// TestEnsureBundledSDKConcurrentCallers is the regression guard for T2120: every
// promise process sharing a PROMISE_HOME materializes the bundled SDK stub, and
// nothing serializes them, so the three check-then-act sequences in
// ensureBundledSDK each raced. The symlink one aborted the losing process's whole
// compile with "file exists"; the TBD writes were worse but quieter — os.WriteFile
// truncates in place, so a concurrent ld64.lld could read an empty or short stub.
//
// The window is a few instructions wide, so one round fires only ~half the time.
// The loop runs several rounds against a *fresh* home each — the cold-materialize
// path is the contended one, and a warm home never enters it — which makes the
// pre-fix failure reliable rather than occasional. A subprocess-based repro cannot
// do this: the interleaving is not controllable from outside (see the item's notes).
func TestEnsureBundledSDKConcurrentCallers(t *testing.T) {
	root := t.TempDir()
	full := len(bundledLibSystemTBD)

	for round := 0; round < 8; round++ {
		home := filepath.Join(root, fmt.Sprintf("home%d", round))
		if err := os.MkdirAll(home, 0755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PROMISE_HOME", home) // forbids t.Parallel
		tbdPath := filepath.Join(home, "cache", "sdk", "macos", "usr", "lib", "libSystem.B.tbd")

		// Readers stand in for a concurrent linker opening the stub: any read that
		// succeeds must see the whole file, never a truncation window.
		//
		// This half asserts a POSIX property — a rename onto a path is invisible
		// to a reader already holding the old file open — and runs on Unix only.
		// On Windows os.ReadFile opens without FILE_SHARE_DELETE, so a reader
		// spinning on the destination would block MoveFileEx itself and make the
		// *harness* the thing that fails; renameWithRetry is the Windows answer
		// there and is pinned by TestRenameRetrying*. The writer half below — the
		// EEXIST symlink race this item reports — runs on every platform.
		pollTornReads := runtime.GOOS != "windows"
		var torn atomic.Int64
		stop := make(chan struct{})
		var readers sync.WaitGroup
		for i := 0; pollTornReads && i < 4; i++ {
			readers.Add(1)
			go func() {
				defer readers.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if data, err := os.ReadFile(tbdPath); err == nil && len(data) != full {
						torn.Add(1)
					}
				}
			}()
		}

		const writers = 16
		start := make(chan struct{})
		errs := make(chan error, writers)
		var wg sync.WaitGroup
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start // release them together, as the LLVM-view lock does
				if _, err := ensureBundledSDK(); err != nil {
					errs <- err
				}
			}()
		}
		close(start)
		wg.Wait()
		close(stop)
		readers.Wait()
		close(errs)

		for err := range errs {
			t.Errorf("round %d: concurrent ensureBundledSDK failed: %v", round, err)
		}
		if n := torn.Load(); n != 0 {
			t.Errorf("round %d: %d reads of libSystem.B.tbd saw a partial file", round, n)
		}

		target, err := os.Readlink(filepath.Join(home, "cache", "sdk", "macos", "usr", "lib", "libSystem.tbd"))
		if err != nil {
			t.Fatalf("round %d: symlink not created: %v", round, err)
		}
		if target != "libSystem.B.tbd" {
			t.Fatalf("round %d: symlink target = %q, want %q", round, target, "libSystem.B.tbd")
		}
	}
}

// TestEnsureBundledSDKReplacesWrongSymlink covers the repair branch for the link
// (T2120): before the fix, *anything* at libSystem.tbd was left alone forever, so
// a link pointing at the wrong file wedged every subsequent build with no way out
// short of deleting the cache. The TBD file beside it already self-healed on a
// size mismatch; the link now behaves the same way.
func TestEnsureBundledSDKReplacesWrongSymlink(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	libDir := filepath.Join(tmp, "cache", "sdk", "macos", "usr", "lib")
	if err := os.MkdirAll(libDir, 0755); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(libDir, "libSystem.tbd")
	if err := os.Symlink("wrong.tbd", symlinkPath); err != nil {
		t.Fatal(err)
	}

	if _, err := ensureBundledSDK(); err != nil {
		t.Fatalf("ensureBundledSDK failed: %v", err)
	}

	target, err := os.Readlink(symlinkPath)
	if err != nil {
		t.Fatalf("symlink missing after repair: %v", err)
	}
	if target != "libSystem.B.tbd" {
		t.Errorf("symlink target = %q, want %q", target, "libSystem.B.tbd")
	}
}

// TestEnsureBundledSDKReplacesNonSymlinkAtLinkPath is the same repair for a
// non-symlink entry — a plain file left by an interrupted or older layout. This
// is the path that goes through the temp-symlink + rename replace, so it also
// pins that a stray .tmp-* sibling is not left behind.
func TestEnsureBundledSDKReplacesNonSymlinkAtLinkPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	libDir := filepath.Join(tmp, "cache", "sdk", "macos", "usr", "lib")
	if err := os.MkdirAll(libDir, 0755); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(libDir, "libSystem.tbd")
	if err := os.WriteFile(symlinkPath, []byte("not a symlink"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := ensureBundledSDK(); err != nil {
		t.Fatalf("ensureBundledSDK failed: %v", err)
	}

	target, err := os.Readlink(symlinkPath)
	if err != nil {
		t.Fatalf("regular file was not replaced by the symlink: %v", err)
	}
	if target != "libSystem.B.tbd" {
		t.Errorf("symlink target = %q, want %q", target, "libSystem.B.tbd")
	}

	entries, err := os.ReadDir(libDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp entry %q left behind after the replace", e.Name())
		}
	}
}
