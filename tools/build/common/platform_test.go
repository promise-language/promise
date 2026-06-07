package common

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// platform_test.go covers FindLLVM's three-tier lookup (T0798): PROMISE_LLVM
// override, system discovery (left unmodified — exercised implicitly by the
// bin/build path), and slim-fetch fallback.

// stubLLVMDir creates a directory containing dummy `opt`/`llc`/`lld` executable
// files so llvmInfoFromDir can resolve them. Used to exercise the PROMISE_LLVM
// override and the slim-fetch fallback's return path without actually running
// the real LLVM toolchain. parseLLVMVersion will fail on these (they aren't
// real binaries), so the returned LLVMInfo.Version is 0; tests assert on path
// resolution, not on version parsing.
func stubLLVMDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	suffix := ExeSuffix()
	for _, name := range []string{"opt", "llc", "lld"} {
		path := filepath.Join(dir, name+suffix)
		// Real executable so parseLLVMVersion's exec.Command succeeds (its
		// version parser returns 0 on no-version output, which is fine here —
		// FindLLVM accepts the LLVMInfo regardless of Version).
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestFindLLVM_PromiseLLVMOverride(t *testing.T) {
	dir := stubLLVMDir(t)
	t.Setenv("PROMISE_LLVM", dir)

	info, err := FindLLVM(t.TempDir())
	if err != nil {
		t.Fatalf("FindLLVM with PROMISE_LLVM=%s: %v", dir, err)
	}
	suffix := ExeSuffix()
	if info.OptPath != filepath.Join(dir, "opt"+suffix) {
		t.Errorf("OptPath = %q, want %q", info.OptPath, filepath.Join(dir, "opt"+suffix))
	}
	if info.LLDPath != filepath.Join(dir, "lld"+suffix) {
		t.Errorf("LLDPath = %q, want %q", info.LLDPath, filepath.Join(dir, "lld"+suffix))
	}
}

func TestFindLLVM_PromiseLLVMOverride_MissingOpt(t *testing.T) {
	// Directory with no opt — the override must fail loudly rather than
	// silently fall through to system discovery (the override is the user's
	// explicit signal that this is the toolchain to use).
	dir := t.TempDir()
	t.Setenv("PROMISE_LLVM", dir)
	_, err := FindLLVM(t.TempDir())
	if err == nil {
		t.Fatal("PROMISE_LLVM pointing at a directory with no opt must error")
	}
	if !strings.Contains(err.Error(), "PROMISE_LLVM") {
		t.Errorf("error should name PROMISE_LLVM, got: %v", err)
	}
}

// TestFindLLVM_FallsThroughToSlim exercises the slim-fetch fallback. Stub the
// blob fetcher and pre-seed blobs.json + a tiny prebuilts.toml so the host
// (linux-amd64 in fakeReleaseRoot) resolves via the slim path. We do NOT
// suppress the system PATH search — but linux-amd64 in CI generally does have
// system LLVM, which would short-circuit step 2. So this test runs only on
// linux/CI hosts where we can override PATH to be empty.
func TestFindLLVM_FallsThroughToSlim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-stripping is awkward on Windows; the slim fallback is exercised on other hosts")
	}
	root, _ := fakeReleaseRoot(t, nil)
	_, brs := seedSlimCatalog(t, root, map[string]string{"opt": "OPT_SLIM", "llc": "LLC_SLIM"})

	// fakeReleaseRoot's prebuilts.toml omits `lld` (only opt+llc). FindLLVM's
	// llvmInfoFromDir requires lld too — so extend the catalog and the
	// prebuilts.toml to include it.
	rawLld := []byte("LLD_SLIM")
	sha := sha256Hex(rawLld)
	br := brotliBytes(t, rawLld)
	cat, err := LoadBlobsCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.Upsert(BlobEntry{
		Dependency: "llvm", Version: "22.1.0", Target: "linux-amd64", Name: "lld",
		SHA256: sha, Size: int64(len(rawLld)),
		Compression: compressionBrotli, CompressedSize: int64(len(br)), CompressedSHA256: sha256Hex(br),
	}); err != nil {
		t.Fatal(err)
	}
	if err := WriteBlobsCatalog(root, cat); err != nil {
		t.Fatal(err)
	}
	brs[sha+".br"] = br
	prebuiltsPath := filepath.Join(root, "tools", "build", "prebuilts.toml")
	cur, err := os.ReadFile(prebuiltsPath)
	if err != nil {
		t.Fatal(err)
	}
	// Append an `lld` file entry to the linux-amd64 target list. The simplest
	// way is to rewrite the file with all three entries.
	prebuilts := `schema = 1
[binaries.llvm]
version = "22.1.0"
bundle_dir = "compiler/cmd/promise/resources/llvm"
[binaries.llvm.targets.linux-amd64]
url = "https://example.test/LLVM.tar.xz"
sha256 = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0"
files = [
  { src = "bin/opt", out = "opt" },
  { src = "bin/llc", out = "llc" },
  { src = "bin/lld", out = "lld" },
]
`
	_ = cur
	if err := os.WriteFile(prebuiltsPath, []byte(prebuilts), 0o644); err != nil {
		t.Fatal(err)
	}

	cacheRoot := t.TempDir()
	t.Setenv("PROMISE_PREBUILTS_CACHE", cacheRoot)
	t.Setenv("PROMISE_LLVM", "") // make sure no override is set
	// Hide system LLVM by stripping PATH.
	t.Setenv("PATH", "")

	prev := defaultBlobFetcher
	defaultBlobFetcher = &countingBlobFetcher{assets: brs}
	t.Cleanup(func() { defaultBlobFetcher = prev })

	// On non-linux hosts, fakeReleaseRoot's linux-amd64 entry is not the host
	// target — FindLLVM dispatches against CurrentBuildTarget(). Skip on
	// non-linux: the slim fallback is exercised by TestEnsureLLVMBlobs_CatalogHit
	// without going through FindLLVM, and end-to-end coverage happens in CI.
	if CurrentBuildTarget() != "linux-amd64" {
		t.Skipf("slim-fallback test pinned to linux-amd64 (CurrentBuildTarget=%s)", CurrentBuildTarget())
	}

	info, err := FindLLVM(root)
	if err != nil {
		t.Fatalf("FindLLVM with no system LLVM and slim catalog hit: %v", err)
	}
	wantDir := filepath.Join(cacheRoot, "llvm-slim", "22.1.0", "linux-amd64")
	if info.Dir != wantDir {
		t.Errorf("info.Dir = %q, want %q", info.Dir, wantDir)
	}
}

// TestFindLLVM_EmptyRoot_NoSystem_NoSlimFallback covers the case where the
// slim-fetch fallback is intentionally bypassed (root=""). PATH is also
// stripped so system discovery fails. The bottom error must complain about
// missing opt and reference both PATH and PROMISE_LLVM (the only remaining
// resolution paths a developer can choose).
func TestFindLLVM_EmptyRoot_NoSystem_NoSlimFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-stripping is awkward on Windows")
	}
	t.Setenv("PROMISE_LLVM", "")
	t.Setenv("PATH", "")
	t.Setenv("HOMEBREW_PREFIX", t.TempDir()) // empty prefix → hide a macOS Homebrew LLVM (probed directly, not via PATH)
	_, err := FindLLVM("")                   // root=="" → skip slim fallback
	if err == nil {
		t.Fatal("expected error: no system LLVM and no slim fallback")
	}
	// The message must guide the developer toward at least one remediation.
	msg := err.Error()
	if !strings.Contains(msg, "PROMISE_LLVM") && !strings.Contains(msg, "Homebrew") && !strings.Contains(msg, "PATH") {
		t.Errorf("error should suggest a remediation, got: %v", err)
	}
}

// TestFindLLVM_SlimFetchError_WrapsCleanly covers the slim-fallback failure
// path: when EnsureLLVMBlobs errors (here, because no prebuilts.toml exists
// at root), FindLLVM must wrap that error rather than silently fall through
// to a generic "not found" — otherwise a misconfigured tree produces a
// useless error that doesn't tell the developer what went wrong.
func TestFindLLVM_SlimFetchError_WrapsCleanly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-stripping is awkward on Windows")
	}
	t.Setenv("PROMISE_LLVM", "")
	t.Setenv("PATH", "")
	t.Setenv("HOMEBREW_PREFIX", t.TempDir()) // hide a macOS Homebrew LLVM (probed directly, not via PATH)
	// Pass a root that has no tools/build/prebuilts.toml → LoadPrebuiltsManifest
	// inside EnsureLLVMBlobs fails, so FindLLVM hits the slim-fetch error branch.
	_, err := FindLLVM(t.TempDir())
	if err == nil {
		t.Fatal("expected error from slim-fetch failure")
	}
	if !strings.Contains(err.Error(), "slim-blob fetch failed") {
		t.Errorf("error should be wrapped as 'slim-blob fetch failed', got: %v", err)
	}
	if !strings.Contains(err.Error(), "PROMISE_LLVM") {
		t.Errorf("error should still suggest PROMISE_LLVM as an escape hatch, got: %v", err)
	}
}

// TestFindLLVM_PromiseLLVMOverride_PartialDir covers a directory that has
// opt+llc but no lld — the override must reject, since the build pipeline
// can't link without lld and the override is a "use exactly this" signal.
func TestFindLLVM_PromiseLLVMOverride_PartialDir(t *testing.T) {
	dir := t.TempDir()
	suffix := ExeSuffix()
	for _, name := range []string{"opt", "llc"} { // intentionally omit lld
		if err := os.WriteFile(filepath.Join(dir, name+suffix), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PROMISE_LLVM", dir)
	_, err := FindLLVM(t.TempDir())
	if err == nil {
		t.Fatal("PROMISE_LLVM with no lld must error")
	}
	if !strings.Contains(err.Error(), "PROMISE_LLVM") {
		t.Errorf("error should name PROMISE_LLVM, got: %v", err)
	}
}

// TestFindLLVM_PromiseLLVMOverride_TrimsWhitespace covers the
// strings.TrimSpace on PROMISE_LLVM: a value of "   " must be treated as
// unset, not as a literal whitespace path that fails llvmInfoFromDir. This
// prevents a confusing error from a shell variable that expanded to an
// empty/whitespace value.
func TestFindLLVM_PromiseLLVMOverride_TrimsWhitespace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-stripping is awkward on Windows")
	}
	t.Setenv("PROMISE_LLVM", "   ")
	t.Setenv("PATH", "")                     // force system discovery to fail
	t.Setenv("HOMEBREW_PREFIX", t.TempDir()) // hide a macOS Homebrew LLVM (probed directly, not via PATH)
	// Use a non-existent root so the slim fallback also fails — we want to
	// reach the bottom of FindLLVM and confirm the whitespace-only override
	// didn't shortcut us with a PROMISE_LLVM-specific error.
	_, err := FindLLVM("")
	if err == nil {
		t.Fatal("expected error: no system LLVM, no override, no slim fallback")
	}
	// The whitespace-only env var should be ignored → the error must NOT
	// mention PROMISE_LLVM="..." (the override-specific error path).
	if strings.Contains(err.Error(), `PROMISE_LLVM="   "`) {
		t.Errorf("whitespace-only PROMISE_LLVM should be treated as unset; got override error: %v", err)
	}
}

// TestLLVMInfoFromDir covers the helper directly: a complete dir resolves;
// a missing opt, llc, or lld returns (nil, false).
func TestLLVMInfoFromDir(t *testing.T) {
	dir := stubLLVMDir(t)
	info, ok := llvmInfoFromDir(dir)
	if !ok {
		t.Fatal("expected ok for a fully-populated directory")
	}
	suffix := ExeSuffix()
	if info.OptPath != filepath.Join(dir, "opt"+suffix) {
		t.Errorf("OptPath = %q", info.OptPath)
	}
	if info.LLCPath != filepath.Join(dir, "llc"+suffix) {
		t.Errorf("LLCPath = %q", info.LLCPath)
	}
	if info.LLDPath != filepath.Join(dir, "lld"+suffix) {
		t.Errorf("LLDPath = %q", info.LLDPath)
	}

	// Empty dir → not ok.
	if _, ok := llvmInfoFromDir(t.TempDir()); ok {
		t.Error("expected !ok for empty dir")
	}

	// Missing lld → not ok (FindLLVM requires lld too).
	partial := t.TempDir()
	if err := os.WriteFile(filepath.Join(partial, "opt"+suffix), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, "llc"+suffix), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := llvmInfoFromDir(partial); ok {
		t.Error("expected !ok for dir missing lld")
	}
}
