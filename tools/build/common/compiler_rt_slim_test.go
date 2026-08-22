package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// compiler_rt_slim_test.go covers the compiler-rt builtins prebuilt path
// (T1676): the arch/name helpers, the runtime-manifest projection, and the
// (required, not best-effort) embed step. Mirrors openssl_slim_test.go.

const testCompilerRTVersion = "21.1.2-r0"

const testCompilerRTSrc = "usr/lib/llvm21/lib/clang/21/lib/x86_64-alpine-linux-musl/libclang_rt.builtins-x86_64.a"

// compilerRTPrebuiltsTOML renders a prebuilts.toml declaring llvm (so the LLVM
// projection succeeds) plus compiler-rt at the given URL/sha.
func compilerRTPrebuiltsTOML(target, url, sha string) string {
	return `schema = 1
[binaries.llvm]
version = "22.1.0"
bundle_dir = "compiler/cmd/promise/resources/llvm"
[binaries.llvm.targets.` + target + `]
url = "https://example.test/LLVM.tar.xz"
sha256 = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0"
files = [
  { src = "bin/opt", out = "opt" },
  { src = "bin/llc", out = "llc" },
]

[binaries.compiler-rt]
version = "` + testCompilerRTVersion + `"
bundle_dir = "compiler/cmd/promise/resources/compiler-rt"
[binaries.compiler-rt.targets.` + target + `]
url = "` + url + `"
sha256 = "` + sha + `"
files = [
  { src = "` + testCompilerRTSrc + `", out = "libclang_rt.builtins.a" },
]
`
}

func fakeCompilerRTRoot(t *testing.T, target, url, sha string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "catalog.toml"), []byte("epoch = \"2026.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolsBuild := filepath.Join(root, "tools", "build")
	if err := os.MkdirAll(toolsBuild, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolsBuild, "prebuilts.toml"),
		[]byte(compilerRTPrebuiltsTOML(target, url, sha)), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

var testCompilerRTContents = map[string]string{
	"libclang_rt.builtins.a": "BUILTINS",
}

// seedCompilerRTCatalog records compiler-rt blobs for target in blobs.json and
// returns the brotli-compressed asset bytes keyed by "<sha>.br" (mirrors
// seedOpenSSLCatalog).
func seedCompilerRTCatalog(t *testing.T, root, target string, contents map[string]string) map[string][]byte {
	t.Helper()
	cat, err := LoadBlobsCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	brByAsset := map[string][]byte{}
	for name, content := range contents {
		raw := []byte(content)
		br := brotliBytes(t, raw)
		sha := sha256Hex(raw)
		if err := cat.Upsert(BlobEntry{
			Dependency:       "compiler-rt",
			Version:          testCompilerRTVersion,
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
		brByAsset[sha+".br"] = br
	}
	if err := WriteBlobsCatalog(root, cat); err != nil {
		t.Fatal(err)
	}
	return brByAsset
}

func TestCompilerRTArchDir(t *testing.T) {
	for target, want := range map[string]string{
		"linux-amd64": "x86_64-linux-musl",
		"linux-arm64": "aarch64-linux-musl",
	} {
		got, err := CompilerRTArchDir(target)
		if err != nil {
			t.Fatalf("CompilerRTArchDir(%q): %v", target, err)
		}
		if got != want {
			t.Errorf("CompilerRTArchDir(%q) = %q, want %q", target, got, want)
		}
	}
	if _, err := CompilerRTArchDir("darwin-arm64"); err == nil {
		t.Error("CompilerRTArchDir(darwin-arm64) should fail rather than default to an arch")
	}
}

func TestCompilerRTManifestName(t *testing.T) {
	got := CompilerRTManifestName("aarch64-linux-musl", "libclang_rt.builtins.a")
	if got != "compiler-rt-aarch64-linux-musl-libclang_rt.builtins.a" {
		t.Errorf("CompilerRTManifestName = %q", got)
	}
	// The `out` name is arch-neutral, so the arch prefix is the only separator.
	if CompilerRTManifestName("x86_64-linux-musl", "libclang_rt.builtins.a") == got {
		t.Error("arch-qualified names collided across arches")
	}
}

// TestPrebuiltsManifest_CompilerRTReal checks the REAL prebuilts.toml declares a
// compiler-rt entry with the right file set — and, unlike OpenSSL, that the
// sha256 is actually pinned: the archive is required by every Linux link, so an
// unpinned entry is a hard error rather than a pending state.
func TestPrebuiltsManifest_CompilerRTReal(t *testing.T) {
	root, err := FindRoot()
	if err != nil {
		t.Skipf("find root: %v", err)
	}
	m, err := LoadPrebuiltsManifest(root)
	if err != nil {
		t.Fatalf("LoadPrebuiltsManifest: %v", err)
	}
	crt := m.Binaries["compiler-rt"]
	if crt == nil {
		t.Fatal("prebuilts.toml has no [binaries.compiler-rt] entry")
	}
	if crt.Version == "" {
		t.Error("compiler-rt.version is empty")
	}
	for _, target := range CompilerRTTargets() {
		te := crt.Targets[target]
		if te == nil {
			t.Errorf("missing [binaries.compiler-rt.targets.%s]", target)
			continue
		}
		if te.URL == "" {
			t.Errorf("target %s: empty url", target)
		}
		if strings.TrimSpace(te.SHA256) == "" {
			t.Errorf("target %s: sha256 UNPINNED — the builtins archive is required by every Linux link", target)
		}
		got := map[string]bool{}
		for _, f := range te.Files {
			got[f.Out] = true
		}
		// Must match compilerRTFiles in compiler/cmd/promise/main.go.
		if !got["libclang_rt.builtins.a"] {
			t.Errorf("target %s: no libclang_rt.builtins.a in files", target)
		}
		if _, err := CompilerRTArchDir(target); err != nil {
			t.Errorf("CompilerRTTargets() lists %s but CompilerRTArchDir rejects it: %v", target, err)
		}
	}
}

func TestEnsureCompilerRTBlobs_RejectsNonLinuxTarget(t *testing.T) {
	root := fakeCompilerRTRoot(t, "linux-amd64", "https://example.test/compiler-rt.apk", "")
	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	if _, err := EnsureCompilerRTBlobs(root, "darwin-arm64"); err == nil {
		t.Fatal("expected EnsureCompilerRTBlobs to reject a non-Linux target")
	}
}

// TestBuildRuntimeManifestFromCatalog_IncludesCompilerRT asserts published
// compiler-rt blobs are projected with ARCH-QUALIFIED names.
func TestBuildRuntimeManifestFromCatalog_IncludesCompilerRT(t *testing.T) {
	root := fakeCompilerRTRoot(t, "linux-amd64", "https://example.test/compiler-rt.apk", "")
	seedSlimCatalog(t, root, map[string]string{"opt": "OPT", "llc": "LLC"})
	seedCompilerRTCatalog(t, root, "linux-amd64", testCompilerRTContents)

	m, err := BuildRuntimeManifestFromCatalog(root, "linux-amd64", "2026.0")
	if err != nil {
		t.Fatalf("BuildRuntimeManifestFromCatalog: %v", err)
	}
	byName := map[string]runtimeManifestEntry{}
	for _, e := range m.Entries {
		byName[e.Name] = e
	}
	name := CompilerRTManifestName("x86_64-linux-musl", "libclang_rt.builtins.a")
	e, ok := byName[name]
	if !ok {
		t.Fatalf("missing compiler-rt entry %s (got %v)", name, byName)
	}
	if e.SHA256 != sha256Hex([]byte(testCompilerRTContents["libclang_rt.builtins.a"])) {
		t.Errorf("%s sha256 = %s, want the archive content hash", name, e.SHA256)
	}
	if e.Kind != "blob" {
		t.Errorf("%s kind = %q, want blob", name, e.Kind)
	}
	if len(e.Sources) != 3 {
		t.Fatalf("%s has %d sources, want 3", name, len(e.Sources))
	}
	if e.Sources[2].Archive == "" || e.Sources[2].ArchivePath != testCompilerRTSrc {
		t.Errorf("%s fallback source = %+v, want upstream apk member %s", name, e.Sources[2], testCompilerRTSrc)
	}
}

// TestBuildRuntimeManifestFromCatalog_SkipsUnpublishedCompilerRT is the
// best-effort-at-projection contract: an unpublished archive must not strand the
// LLVM (or musl) entries. The compiler then falls back to its embedded copy.
func TestBuildRuntimeManifestFromCatalog_SkipsUnpublishedCompilerRT(t *testing.T) {
	root := fakeCompilerRTRoot(t, "linux-amd64", "https://example.test/compiler-rt.apk", "")
	seedSlimCatalog(t, root, map[string]string{"opt": "OPT", "llc": "LLC"})
	// Deliberately do NOT seed compiler-rt blobs.

	m, err := BuildRuntimeManifestFromCatalog(root, "linux-amd64", "2026.0")
	if err != nil {
		t.Fatalf("an unpublished compiler-rt blob must not fail the projection: %v", err)
	}
	for _, e := range m.Entries {
		if strings.HasPrefix(e.Name, "compiler-rt-") {
			t.Errorf("projected an unpublished compiler-rt entry %q — the runtime would fail to resolve it", e.Name)
		}
	}
}

// TestEmbedCompilerRT_StagesFromCatalog covers the success path: with the
// archive hosted in the blob catalog, EmbedCompilerRT fetches and stages the
// real libclang_rt.builtins.a into the embed dir.
func TestEmbedCompilerRT_StagesFromCatalog(t *testing.T) {
	if !IsLinux() {
		t.Skip("compiler-rt builtins are staged on Linux hosts only")
	}
	target := CurrentBuildTarget()
	arch, err := CompilerRTArchDir(target)
	if err != nil {
		t.Skipf("no compiler-rt builtins for host target %s", target)
	}
	root := fakeCompilerRTRoot(t, target, "https://example.test/compiler-rt.apk", "")
	brs := seedCompilerRTCatalog(t, root, target, testCompilerRTContents)

	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	prev := defaultBlobFetcher
	defaultBlobFetcher = &countingBlobFetcher{assets: brs}
	t.Cleanup(func() { defaultBlobFetcher = prev })

	if err := EmbedCompilerRT(root); err != nil {
		t.Fatalf("EmbedCompilerRT (catalog-hosted): %v", err)
	}
	embedDir := filepath.Join(root, "compiler", "cmd", "promise", "resources", "compiler-rt", arch)
	for name, want := range testCompilerRTContents {
		got, err := os.ReadFile(filepath.Join(embedDir, name))
		if err != nil {
			t.Fatalf("read staged %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("staged %s = %q, want %q", name, got, want)
		}
	}
}

// TestEmbedCompilerRT_WipesStaleArchive covers the pre-wipe: a stale archive
// from a prior run must not survive a re-embed, where the
// `compiler-rt/<arch>/*` glob would otherwise sweep the wrong bytes into the
// binary.
func TestEmbedCompilerRT_WipesStaleArchive(t *testing.T) {
	if !IsLinux() {
		t.Skip("compiler-rt builtins are staged on Linux hosts only")
	}
	target := CurrentBuildTarget()
	arch, err := CompilerRTArchDir(target)
	if err != nil {
		t.Skipf("no compiler-rt builtins for host target %s", target)
	}
	root := fakeCompilerRTRoot(t, target, "https://example.test/compiler-rt.apk", "")
	brs := seedCompilerRTCatalog(t, root, target, testCompilerRTContents)

	embedDir := filepath.Join(root, "compiler", "cmd", "promise", "resources", "compiler-rt", arch)
	if err := os.MkdirAll(embedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(embedDir, "libclang_rt.profile.a")
	if err := os.WriteFile(stale, []byte("STALE"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	prev := defaultBlobFetcher
	defaultBlobFetcher = &countingBlobFetcher{assets: brs}
	t.Cleanup(func() { defaultBlobFetcher = prev })

	if err := EmbedCompilerRT(root); err != nil {
		t.Fatalf("EmbedCompilerRT: %v", err)
	}
	if Exists(stale) {
		t.Error("stale archive survived the re-embed and would be swept into the binary")
	}
}

// TestEmbedCompilerRT_FailsWhenUnobtainable pins the REQUIRED contract, the one
// real behavioural difference from EmbedOpenSSL: with neither a pinned sha256
// nor a catalog blob, EmbedCompilerRT must FAIL rather than stage a placeholder
// and let the build produce a compiler that cannot link (T1676).
func TestEmbedCompilerRT_FailsWhenUnobtainable(t *testing.T) {
	if !IsLinux() {
		t.Skip("compiler-rt builtins are staged on Linux hosts only")
	}
	target := CurrentBuildTarget()
	if _, err := CompilerRTArchDir(target); err != nil {
		t.Skipf("no compiler-rt builtins for host target %s", target)
	}
	// Blank sha256, no catalog blobs, and an unreachable URL.
	root := fakeCompilerRTRoot(t, target, "https://example.invalid/compiler-rt.apk", "")
	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())

	if err := EmbedCompilerRT(root); err == nil {
		t.Fatal("EmbedCompilerRT must fail when the archive cannot be obtained — a silent skip reintroduces the T1676 link failure")
	}
}
