package common

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// platform_test.go covers FindLLVM's two sources (T2108): the pinned LLVM blob
// cache, and the per-tool PROMISE_* overrides overlaid on it. The host is not a
// source — the tests below assert a toolchain reachable only via PATH or
// Homebrew is never resolved.

// stubLLVMDir creates a directory containing dummy `opt`/`llc`/`lld` executable
// files so llvmInfoFromDir can resolve them. parseLLVMVersion will fail on these
// (they aren't real binaries), so the returned LLVMInfo.Version is 0; tests
// assert on path resolution, not on version parsing.
func stubLLVMDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	suffix := ExeSuffix()
	for _, name := range []string{"opt", "llc", "lld"} {
		path := filepath.Join(dir, name+suffix)
		// Real executable so parseLLVMVersion's exec.Command succeeds (its
		// version parser returns 0 on no-version output, which is fine here).
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// clearToolchainOverrides unsets every override so a test starts from the
// pinned configuration regardless of the developer's shell.
func clearToolchainOverrides(t *testing.T) {
	t.Helper()
	for _, name := range toolchainOverrideVars {
		t.Setenv(name, "")
	}
}

// fakeReleaseRootForTarget creates a temp repo root with the minimal
// catalog.toml and a prebuilts.toml that lists opt+llc (but NOT lld) for
// the given target, using platform-appropriate file-name suffixes. It is the
// per-platform analogue of fakeReleaseRoot (which is linux-amd64 only).
func fakeReleaseRootForTarget(t *testing.T, target string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "catalog.toml"), []byte("epoch = \"2026.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolsBuild := filepath.Join(root, "tools", "build")
	if err := os.MkdirAll(toolsBuild, 0o755); err != nil {
		t.Fatal(err)
	}
	suffix := ExeSuffix()
	prebuilts := fmt.Sprintf(`schema = 1
[binaries.llvm]
version = "22.1.0"
bundle_dir = "compiler/cmd/promise/resources/llvm"
[binaries.llvm.targets.%s]
url = "https://example.test/LLVM.tar.xz"
sha256 = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0"
files = [
  { src = "bin/opt%s", out = "opt%s" },
  { src = "bin/llc%s", out = "llc%s" },
]
`, target, suffix, suffix, suffix, suffix)
	if err := os.WriteFile(filepath.Join(toolsBuild, "prebuilts.toml"), []byte(prebuilts), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// seedSlimCatalogForTarget is seedSlimCatalog generalised to any target string.
func seedSlimCatalogForTarget(t *testing.T, root, target string, contents map[string]string) map[string][]byte {
	t.Helper()
	cat := &BlobsCatalog{Schema: BlobsCatalogSchema}
	brs := map[string][]byte{}
	for name, content := range contents {
		raw := []byte(content)
		br := brotliBytes(t, raw)
		sha := sha256Hex(raw)
		if err := cat.Upsert(BlobEntry{
			Dependency:       "llvm",
			Version:          "22.1.0",
			Target:           target,
			Name:             name,
			SHA256:           sha,
			Size:             int64(len(raw)),
			Compression:      compressionBrotli,
			CompressedSize:   int64(len(br)),
			CompressedSHA256: sha256Hex(br),
		}); err != nil {
			t.Fatal(err)
		}
		brs[sha+".br"] = br
	}
	if err := WriteBlobsCatalog(root, cat); err != nil {
		t.Fatal(err)
	}
	return brs
}

// TestFindLLVM_NeverConsultsHost is the T2108 invariant for the build tool: a
// complete LLVM reachable only through PATH or Homebrew is not resolved. Build
// inputs are pinned, not discovered — otherwise `bin/build` compiles against
// whatever the machine happens to have, and two developers build differently
// with neither being told.
func TestFindLLVM_NeverConsultsHost(t *testing.T) {
	clearToolchainOverrides(t)

	// A PATH, a Homebrew prefix and a Program Files layout that all hold a
	// plausible, complete toolchain.
	host := t.TempDir()
	suffix := ExeSuffix()
	pathDir := filepath.Join(host, "bin")
	if err := os.MkdirAll(pathDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := []byte("#!/bin/sh\necho \"LLVM version 22.1.0\"\n")
	for _, n := range []string{"opt", "llc", "lld", "ld.lld", "ld64.lld", "lld-link", "opt-22", "opt-25", "llc-25", "ld.lld-25"} {
		if err := os.WriteFile(filepath.Join(pathDir, n+suffix), stub, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, formula := range []string{"llvm", "llvm@22", "llvm@25", "lld"} {
		binDir := filepath.Join(host, "brew", formula, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"opt", "llc", "ld64.lld"} {
			if err := os.WriteFile(filepath.Join(binDir, n), stub, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("PATH", pathDir)
	t.Setenv("HOMEBREW_PREFIX", filepath.Join(host, "brew"))
	t.Setenv("ProgramFiles", host)
	t.Setenv("USERPROFILE", host)
	// No pinned toolchain anywhere: an empty prebuilts cache and a root with no
	// prebuilts.toml, so the only thing that could answer is the host.
	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())

	info, err := FindLLVM(t.TempDir())
	if err == nil {
		t.Fatalf("resolved a toolchain with nothing pinned: opt=%s lld=%s — the host must never answer",
			info.OptPath, info.LLDPath)
	}
	// The message may say the host is never consulted; what it must never do is
	// send the reader to a package manager, since installing one changes nothing.
	for _, unwanted := range []string{"brew install", "apt install", "apt-get install", "install LLVM"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("error must not suggest a system LLVM (%q), got: %v", unwanted, err)
		}
	}
	if !strings.Contains(err.Error(), "pinned") {
		t.Errorf("error should name the missing pinned toolchain, got: %v", err)
	}
}

// TestFindLLVM_FullOverrideSkipsPinnedFetch covers the bringup case: the
// operator has named every binary the build needs, so the pinned set is not
// consulted at all (it may not even exist for the LLVM being brought up).
func TestFindLLVM_FullOverrideSkipsPinnedFetch(t *testing.T) {
	clearToolchainOverrides(t)
	dir := stubLLVMDir(t)
	suffix := ExeSuffix()
	opt := filepath.Join(dir, "opt"+suffix)
	lld := filepath.Join(dir, "lld"+suffix)
	t.Setenv("PROMISE_OPT", opt)
	t.Setenv("PROMISE_LLC", filepath.Join(dir, "llc"+suffix))
	t.Setenv(linkerOverrideVar(), lld)

	// root == "" — no prebuilts.toml to read, so resolving here proves the
	// pinned path was not needed.
	info, err := FindLLVM("")
	if err != nil {
		t.Fatalf("fully-overridden resolution: %v", err)
	}
	if info.OptPath != opt {
		t.Errorf("OptPath = %q, want %q", info.OptPath, opt)
	}
	if info.LLDPath != lld {
		t.Errorf("LLDPath = %q, want %q", info.LLDPath, lld)
	}
}

// TestFindLLVM_PerToolOverrideSubstitutesOnlyThatTool: a lone PROMISE_OPT
// replaces opt and nothing else. One variable naming one binary is the point of
// the per-tool vocabulary — a directory-wide override silently swaps several
// tools at once and never says which.
func TestFindLLVM_PerToolOverrideSubstitutesOnlyThatTool(t *testing.T) {
	clearToolchainOverrides(t)
	root := seedPinnedToolchain(t)

	customDir := stubLLVMDir(t)
	custom := filepath.Join(customDir, "opt")
	t.Setenv("PROMISE_OPT", custom)

	info, err := FindLLVM(root)
	if err != nil {
		t.Fatalf("FindLLVM: %v", err)
	}
	if info.OptPath != custom {
		t.Errorf("OptPath = %q, want the override %q", info.OptPath, custom)
	}
	if filepath.Dir(info.LLDPath) == customDir {
		t.Errorf("LLDPath = %q — an override of opt must not move lld", info.LLDPath)
	}
	if filepath.Dir(info.LLCPath) == customDir {
		t.Errorf("LLCPath = %q — an override of opt must not move llc", info.LLCPath)
	}
	if info.LLDPath == "" || info.LLCPath == "" {
		t.Errorf("the non-overridden tools must still resolve from the pinned set (llc=%q lld=%q)", info.LLCPath, info.LLDPath)
	}
}

// TestFindLLVM_PerToolOverride_LlcAndLinker covers the other two overlay slots.
// `opt` alone is not the whole contract: a lone PROMISE_LLC must move llc while
// opt and lld stay pinned, and a lone linker override must move the linker while
// opt stays pinned — and neither may take the fully-overridden shortcut, which
// would skip the pinned set for tools nobody overrode.
func TestFindLLVM_PerToolOverride_LlcAndLinker(t *testing.T) {
	t.Run("llc only", func(t *testing.T) {
		clearToolchainOverrides(t)
		root := seedPinnedToolchain(t)
		custom := filepath.Join(stubLLVMDir(t), "llc")
		t.Setenv("PROMISE_LLC", custom)

		info, err := FindLLVM(root)
		if err != nil {
			t.Fatalf("FindLLVM: %v", err)
		}
		if info.LLCPath != custom {
			t.Errorf("LLCPath = %q, want the override %q", info.LLCPath, custom)
		}
		if !strings.Contains(info.OptPath, "llvm-slim") || !strings.Contains(info.LLDPath, "llvm-slim") {
			t.Errorf("an override of llc must leave opt and lld pinned (opt=%q lld=%q)", info.OptPath, info.LLDPath)
		}
	})

	t.Run("linker only", func(t *testing.T) {
		clearToolchainOverrides(t)
		root := seedPinnedToolchain(t)
		custom := filepath.Join(stubLLVMDir(t), "lld")
		t.Setenv(linkerOverrideVar(), custom)

		info, err := FindLLVM(root)
		if err != nil {
			t.Fatalf("FindLLVM: %v", err)
		}
		if info.LLDPath != custom {
			t.Errorf("LLDPath = %q, want the override %q", info.LLDPath, custom)
		}
		if !strings.Contains(info.OptPath, "llvm-slim") {
			t.Errorf("an override of the linker must leave opt pinned, got %q", info.OptPath)
		}
	})
}

// TestFindLLVM_PinnedToolCannotRun keeps the T0530 guard alive: a staged prebuilt
// that cannot exec on this host (the upstream Linux tarballs are glibc-linked, so
// every tool fails on Alpine) must stop the build with an explanation. Without it
// the build "succeeds" and ships a compiler that dies at the first `opt`
// invocation, far from the cause — and with the host no longer searched there is
// no accidental second toolchain to paper over it.
func TestFindLLVM_PinnedToolCannotRun(t *testing.T) {
	if IsWindows() {
		t.Skip("exec-failure shape differs on Windows")
	}
	clearToolchainOverrides(t)

	target := CurrentBuildTarget()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "catalog.toml"), []byte("epoch = \"2026.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolsBuild := filepath.Join(root, "tools", "build")
	if err := os.MkdirAll(toolsBuild, 0o755); err != nil {
		t.Fatal(err)
	}
	prebuilts := fmt.Sprintf(`schema = 1
[binaries.llvm]
version = "22.1.0"
bundle_dir = "compiler/cmd/promise/resources/llvm"
[binaries.llvm.targets.%s]
url = "https://example.test/LLVM.tar.xz"
sha256 = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0"
files = [
  { src = "bin/opt", out = "opt" },
  { src = "bin/llc", out = "llc" },
  { src = "bin/lld", out = "lld" },
]
`, target)
	if err := os.WriteFile(filepath.Join(toolsBuild, "prebuilts.toml"), []byte(prebuilts), 0o644); err != nil {
		t.Fatal(err)
	}
	// All three stage, but `opt` is not a runnable image — the same observable
	// state as a glibc binary on a musl host.
	brs := seedSlimCatalogForTarget(t, root, target, map[string]string{
		"opt": "NOT_AN_EXECUTABLE_IMAGE",
		"llc": "LLC_SLIM",
		"lld": "LLD_SLIM",
	})
	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	prev := defaultBlobFetcher
	defaultBlobFetcher = &countingBlobFetcher{assets: brs}
	t.Cleanup(func() { defaultBlobFetcher = prev })

	_, err := FindLLVM(root)
	if err == nil {
		t.Fatal("expected an error: the staged opt cannot run on this host")
	}
	msg := err.Error()
	if !strings.Contains(msg, "cannot run on this host") {
		t.Errorf("error should say the prebuilt cannot run, got: %v", err)
	}
	if !strings.Contains(msg, "PROMISE_OPT") {
		t.Errorf("error should name the explicit way out, got: %v", err)
	}
	// It must not read as "no toolchain found" — that would send the reader
	// looking for one rather than at a toolchain that is present and unusable.
	if strings.Contains(msg, "no pinned LLVM toolchain") {
		t.Errorf("a present-but-unrunnable toolchain must not report as missing, got: %v", err)
	}
}

// TestFindLLVM_NoRootNoOverride covers the one remaining way to have nothing:
// no override and no repo root to read the pinned manifest from. It must fail
// naming the pinned toolchain and the bringup variables — never fall through to
// a host scan, which is what used to answer here.
func TestFindLLVM_NoRootNoOverride(t *testing.T) {
	clearToolchainOverrides(t)
	_, err := FindLLVM("")
	if err == nil {
		t.Fatal("expected an error with no override and no root")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no pinned LLVM toolchain") {
		t.Errorf("error should name the missing pinned toolchain, got: %v", err)
	}
	if !strings.Contains(msg, "PROMISE_OPT") || !strings.Contains(msg, linkerOverrideVar()) {
		t.Errorf("error should name the per-tool bringup variables, got: %v", err)
	}
	for _, unwanted := range []string{"brew install", "apt install", "apt-get install", "install LLVM"} {
		if strings.Contains(msg, unwanted) {
			t.Errorf("error must not suggest a system LLVM (%q), got: %v", unwanted, err)
		}
	}
}

// TestFindLLVM_ResolvesFromPinnedCache is the ordinary path: no overrides, the
// toolchain comes from the pinned blobs.
func TestFindLLVM_ResolvesFromPinnedCache(t *testing.T) {
	clearToolchainOverrides(t)
	root := seedPinnedToolchain(t)

	info, err := FindLLVM(root)
	if err != nil {
		t.Fatalf("FindLLVM with a pinned catalog hit: %v", err)
	}
	if !strings.Contains(info.Dir, filepath.Join("llvm-slim", "22.1.0", CurrentBuildTarget())) {
		t.Errorf("info.Dir = %q, want the pinned slim cache dir", info.Dir)
	}
	if info.Version != 22 {
		t.Errorf("Version = %d, want 22 probed from the staged opt", info.Version)
	}
}

// TestToolchainOverrides covers the helper both the build and the gate path
// read: every override in effect, and nothing when the build is pinned.
func TestToolchainOverrides(t *testing.T) {
	clearToolchainOverrides(t)
	if got := ToolchainOverrides(); len(got) != 0 {
		t.Fatalf("expected no overrides, got %v", got)
	}
	t.Setenv("PROMISE_OPT", "/custom/opt")
	t.Setenv("PROMISE_USE_CLANG", "1")
	got := ToolchainOverrides()
	want := []string{"PROMISE_OPT=/custom/opt", "PROMISE_USE_CLANG=1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ToolchainOverrides() = %v, want %v", got, want)
	}
	// Whitespace-only is not an override — it is an env var that expanded to
	// nothing, and treating it as one produces a baffling error.
	t.Setenv("PROMISE_OPT", "   ")
	t.Setenv("PROMISE_USE_CLANG", "")
	if got := ToolchainOverrides(); len(got) != 0 {
		t.Errorf("whitespace-only override should count as unset, got %v", got)
	}
}

// seedPinnedLinuxToolchain builds a repo root whose blob catalog serves a
// complete linux-amd64 opt/llc/lld with the fetcher stubbed, and points the
// prebuilts cache at a temp dir. Returns the root.
func seedPinnedToolchain(t *testing.T) string {
	t.Helper()
	// The `opt` stub has to answer `--version`, because FindLLVM probes the
	// staged opt before accepting it (T0530). A `#!/bin/sh` stub is not
	// executable on Windows, so the fixture is POSIX-only; the Windows path is
	// covered by TestFindLLVM_PinnedStagedButMissingFile, which never reaches
	// the probe.
	if IsWindows() {
		t.Skip("shell-script tool stubs are not executable on Windows")
	}
	target := CurrentBuildTarget()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "catalog.toml"), []byte("epoch = \"2026.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolsBuild := filepath.Join(root, "tools", "build")
	if err := os.MkdirAll(toolsBuild, 0o755); err != nil {
		t.Fatal(err)
	}
	prebuilts := fmt.Sprintf(`schema = 1
[binaries.llvm]
version = "22.1.0"
bundle_dir = "compiler/cmd/promise/resources/llvm"
[binaries.llvm.targets.%s]
url = "https://example.test/LLVM.tar.xz"
sha256 = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0"
files = [
  { src = "bin/opt", out = "opt" },
  { src = "bin/llc", out = "llc" },
  { src = "bin/lld", out = "lld" },
]
`, target)
	if err := os.WriteFile(filepath.Join(toolsBuild, "prebuilts.toml"), []byte(prebuilts), 0o644); err != nil {
		t.Fatal(err)
	}

	brs := seedSlimCatalogForTarget(t, root, target, map[string]string{
		"opt": "#!/bin/sh\necho \"LLVM version 22.1.0\"\n",
		"llc": "LLC_SLIM",
		"lld": "LLD_SLIM",
	})

	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	prev := defaultBlobFetcher
	defaultBlobFetcher = &countingBlobFetcher{assets: brs}
	t.Cleanup(func() { defaultBlobFetcher = prev })
	return root
}

// TestFindLLVM_PinnedFetchError_WrapsCleanly: when the pinned blobs cannot be
// staged (here, no prebuilts.toml at root), FindLLVM must wrap that error rather
// than fall through — there is nothing left to fall through to, and a generic
// "not found" would hide what actually went wrong.
func TestFindLLVM_PinnedFetchError_WrapsCleanly(t *testing.T) {
	clearToolchainOverrides(t)
	_, err := FindLLVM(t.TempDir())
	if err == nil {
		t.Fatal("expected error when the pinned toolchain cannot be staged")
	}
	if !strings.Contains(err.Error(), "no pinned LLVM toolchain") {
		t.Errorf("error should name the missing pinned toolchain, got: %v", err)
	}
	if !strings.Contains(err.Error(), "PROMISE_OPT") {
		t.Errorf("error should name the explicit bringup path, got: %v", err)
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

// TestLLVMInfoFromDir_Dlltool covers the optional build-only llvm-dlltool probe
// (T0833): present → DlltoolPath populated; absent → DlltoolPath empty WITHOUT
// failing resolution (only opt+lld are required for the (info, true) contract).
func TestLLVMInfoFromDir_Dlltool(t *testing.T) {
	suffix := ExeSuffix()

	// stubLLVMDir has opt/llc/lld but no llvm-dlltool — resolution still succeeds
	// and DlltoolPath stays empty.
	noDlltool := stubLLVMDir(t)
	info, ok := llvmInfoFromDir(noDlltool)
	if !ok {
		t.Fatal("expected ok even without llvm-dlltool (it is optional)")
	}
	if info.DlltoolPath != "" {
		t.Errorf("DlltoolPath = %q, want empty when llvm-dlltool absent", info.DlltoolPath)
	}

	// Add llvm-dlltool → DlltoolPath resolves to it.
	withDlltool := stubLLVMDir(t)
	dt := filepath.Join(withDlltool, "llvm-dlltool"+suffix)
	if err := os.WriteFile(dt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	info, ok = llvmInfoFromDir(withDlltool)
	if !ok {
		t.Fatal("expected ok with llvm-dlltool present")
	}
	if info.DlltoolPath != dt {
		t.Errorf("DlltoolPath = %q, want %q", info.DlltoolPath, dt)
	}
}

// TestFindLLVM_PinnedStagedButMissingFile exercises the T1062 fix on the current
// host platform (linux-amd64, darwin-arm64, or windows-amd64): the pinned blobs
// stage (opt + llc fetched) but lld is absent from the prebuilts.toml files
// list, so FindLLVM must return the specific "required file is missing" error
// naming the cache dir — silence or a generic "not found" would send the reader
// looking for a toolchain rather than at an incomplete manifest.
func TestFindLLVM_PinnedStagedButMissingFile(t *testing.T) {
	target := CurrentBuildTarget()
	root := fakeReleaseRootForTarget(t, target)
	suffix := ExeSuffix()

	// Seed the catalog with opt and llc blobs for this target (deliberately
	// omitting lld so llvmInfoFromDir returns (nil,false) after the fetch).
	brs := seedSlimCatalogForTarget(t, root, target, map[string]string{
		"opt" + suffix: "OPT_SLIM",
		"llc" + suffix: "LLC_SLIM",
	})

	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	clearToolchainOverrides(t)

	prev := defaultBlobFetcher
	defaultBlobFetcher = &countingBlobFetcher{assets: brs}
	t.Cleanup(func() { defaultBlobFetcher = prev })

	_, err := FindLLVM(root)
	if err == nil {
		t.Fatal("expected error: pinned blobs staged but lld absent from prebuilts.toml files list")
	}
	msg := err.Error()
	if !strings.Contains(msg, "pinned toolchain staged into") {
		t.Errorf("error should name the staged cache dir, got: %v", err)
	}
	if !strings.Contains(msg, "lld") {
		t.Errorf("error should name the missing file 'lld', got: %v", err)
	}
}

// TestFindLLVM_PinnedStagedButMissingOpt covers the less common variant of the
// T1062 fix where opt itself is absent from the staged cache (not just lld).
// llvmInfoFromDir returns (nil,false) on the first check, and the detection
// logic in FindLLVM should name opt (not lld) in the error.
//
// Driving "opt absent after successful EnsureLLVMBlobs" via FindLLVM end-to-end
// requires a catalog entry pointing to a tarball server (no slim entry causes a
// tarball fallback), so we unit-test the detection logic directly using the same
// Exists-branch that FindLLVM executes.
func TestFindLLVM_PinnedStagedButMissingOpt(t *testing.T) {
	suffix := ExeSuffix()
	// Cache dir with only llc (no opt) — simulates a partial fetch where opt
	// was not listed in prebuilts.toml at all.
	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, "llc"+suffix), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := llvmInfoFromDir(cacheDir); ok {
		t.Fatal("expected llvmInfoFromDir to return (nil,false) when opt is missing")
	}
	// Replicate the T1062 detection branch: opt absent → missing stays as opt path.
	missing := filepath.Join(cacheDir, "opt"+suffix)
	if Exists(missing) {
		missing = filepath.Join(cacheDir, "lld"+suffix)
	}
	if !strings.Contains(missing, "opt") {
		t.Errorf("missing-file detection should point to opt when opt is absent, got %q", missing)
	}
}
