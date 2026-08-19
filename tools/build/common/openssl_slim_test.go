package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openssl_slim_test.go covers the static-OpenSSL prebuilt path (T1596 / #28): the
// arch/name helpers, the openSSLPinned gate that keeps offline / not-yet-pinned
// builds green, and the runtime-manifest projection. Mirrors musl_slim_test.go.

const testOpenSSLVersion = "3.5.4-r0"

// opensslPrebuiltsTOML renders a prebuilts.toml declaring llvm (so the LLVM
// projection succeeds) plus openssl at opensslURL with opensslSHA.
func opensslPrebuiltsTOML(target, opensslURL, opensslSHA string) string {
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

[binaries.openssl]
version = "` + testOpenSSLVersion + `"
bundle_dir = "compiler/cmd/promise/resources/openssl"
[binaries.openssl.targets.` + target + `]
url = "` + opensslURL + `"
sha256 = "` + opensslSHA + `"
files = [
  { src = "usr/lib/libssl.a", out = "libssl.a" },
  { src = "usr/lib/libcrypto.a", out = "libcrypto.a" },
]
`
}

func fakeOpenSSLRoot(t *testing.T, target, opensslURL, opensslSHA string) string {
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
		[]byte(opensslPrebuiltsTOML(target, opensslURL, opensslSHA)), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

var testOpenSSLContents = map[string]string{
	"libssl.a":    "LIBSSL",
	"libcrypto.a": "LIBCRYPTO",
}

// seedOpenSSLCatalog records openssl blobs for target in blobs.json and returns
// the brotli-compressed asset bytes keyed by "<sha>.br", so a test can drive a
// countingBlobFetcher through the staging path (mirrors seedMuslCatalog).
func seedOpenSSLCatalog(t *testing.T, root, target string, contents map[string]string) map[string][]byte {
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
			Dependency:       "openssl",
			Version:          testOpenSSLVersion,
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

func TestOpenSSLArchDir(t *testing.T) {
	for target, want := range map[string]string{
		"linux-amd64": "x86_64-linux-musl",
		"linux-arm64": "aarch64-linux-musl",
	} {
		got, err := OpenSSLArchDir(target)
		if err != nil {
			t.Fatalf("OpenSSLArchDir(%q): %v", target, err)
		}
		if got != want {
			t.Errorf("OpenSSLArchDir(%q) = %q, want %q", target, got, want)
		}
	}
	if _, err := OpenSSLArchDir("darwin-arm64"); err == nil {
		t.Error("OpenSSLArchDir(darwin-arm64) should fail rather than default to an arch")
	}
}

func TestOpenSSLManifestName(t *testing.T) {
	got := OpenSSLManifestName("aarch64-linux-musl", "libssl.a")
	if got != "openssl-aarch64-linux-musl-libssl.a" {
		t.Errorf("OpenSSLManifestName = %q", got)
	}
	if OpenSSLManifestName("x86_64-linux-musl", "libssl.a") == got {
		t.Error("arch-qualified names collided across arches")
	}
}

// TestPrebuiltsManifest_OpenSSLReal checks the REAL prebuilts.toml declares an
// openssl entry with the right file set for every OpenSSLTargets() arch. The
// sha256 is intentionally NOT asserted pinned: the pinning session had no
// network, so the sha256s are blank until a maintainer runs `bin/pin-prebuilts`
// from a connected host (T1596 / #28). This logs that pending state rather than
// failing, so the structural pin is enforced now without a red build.
func TestPrebuiltsManifest_OpenSSLReal(t *testing.T) {
	root, err := FindRoot()
	if err != nil {
		t.Skipf("find root: %v", err)
	}
	m, err := LoadPrebuiltsManifest(root)
	if err != nil {
		t.Fatalf("LoadPrebuiltsManifest: %v", err)
	}
	openssl := m.Binaries["openssl"]
	if openssl == nil {
		t.Fatal("prebuilts.toml has no [binaries.openssl] entry")
	}
	if openssl.Version == "" {
		t.Error("openssl.version is empty")
	}
	for _, target := range OpenSSLTargets() {
		te := openssl.Targets[target]
		if te == nil {
			t.Errorf("missing [binaries.openssl.targets.%s]", target)
			continue
		}
		if te.URL == "" {
			t.Errorf("target %s: empty url", target)
		}
		if strings.TrimSpace(te.SHA256) == "" {
			t.Logf("target %s: sha256 UNPINNED — run `bin/pin-prebuilts` from a networked host (T1596 / #28, expected until then)", target)
		}
		got := map[string]bool{}
		for _, f := range te.Files {
			got[f.Out] = true
		}
		// Must match opensslFiles in compiler/cmd/promise/main.go.
		for _, want := range []string{"libssl.a", "libcrypto.a"} {
			if !got[want] {
				t.Errorf("target %s: no %s in files", target, want)
			}
		}
		if _, err := OpenSSLArchDir(target); err != nil {
			t.Errorf("OpenSSLTargets() lists %s but OpenSSLArchDir rejects it: %v", target, err)
		}
	}
}

func TestEnsureOpenSSLBlobs_RejectsNonLinuxTarget(t *testing.T) {
	root := fakeOpenSSLRoot(t, "linux-amd64", "https://example.test/openssl.apk", "")
	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	if _, err := EnsureOpenSSLBlobs(root, "darwin-arm64"); err == nil {
		t.Fatal("expected EnsureOpenSSLBlobs to reject a non-Linux target")
	}
}

// TestOpenSSLPinned is the gate that keeps offline / pre-pin `bin/build` green:
// EmbedOpenSSL must only attempt a fetch when OpenSSL is actually obtainable.
func TestOpenSSLPinned(t *testing.T) {
	// (a) Unpinned: blank sha256, no catalog blob → not pinned.
	root := fakeOpenSSLRoot(t, "linux-amd64", "https://example.test/openssl.apk", "")
	if openSSLPinned(root, "linux-amd64") {
		t.Error("blank sha256 + no catalog: expected not pinned")
	}

	// (b) sha256 filled → pinned (verified upstream apk available).
	rootSHA := fakeOpenSSLRoot(t, "linux-amd64", "https://example.test/openssl.apk",
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0")
	if !openSSLPinned(rootSHA, "linux-amd64") {
		t.Error("filled sha256: expected pinned")
	}

	// (c) blank sha256 but every file hosted in the catalog → pinned.
	rootCat := fakeOpenSSLRoot(t, "linux-amd64", "https://example.test/openssl.apk", "")
	seedOpenSSLCatalog(t, rootCat, "linux-amd64", testOpenSSLContents)
	if !openSSLPinned(rootCat, "linux-amd64") {
		t.Error("blank sha256 + full catalog: expected pinned")
	}

	// (d) blank sha256 + PARTIAL catalog (only libssl.a) → not pinned.
	rootPartial := fakeOpenSSLRoot(t, "linux-amd64", "https://example.test/openssl.apk", "")
	seedOpenSSLCatalog(t, rootPartial, "linux-amd64", map[string]string{"libssl.a": "LIBSSL"})
	if openSSLPinned(rootPartial, "linux-amd64") {
		t.Error("blank sha256 + partial catalog: expected not pinned")
	}
}

// TestBuildRuntimeManifestFromCatalog_IncludesOpenSSL asserts published openssl
// blobs are projected with ARCH-QUALIFIED names, alongside the LLVM entries.
func TestBuildRuntimeManifestFromCatalog_IncludesOpenSSL(t *testing.T) {
	root := fakeOpenSSLRoot(t, "linux-amd64", "https://example.test/openssl.apk", "")
	seedSlimCatalog(t, root, map[string]string{"opt": "OPT", "llc": "LLC"})
	seedOpenSSLCatalog(t, root, "linux-amd64", testOpenSSLContents)

	m, err := BuildRuntimeManifestFromCatalog(root, "linux-amd64", "2026.0")
	if err != nil {
		t.Fatalf("BuildRuntimeManifestFromCatalog: %v", err)
	}
	byName := map[string]runtimeManifestEntry{}
	for _, e := range m.Entries {
		byName[e.Name] = e
	}
	for _, f := range []string{"libssl.a", "libcrypto.a"} {
		name := OpenSSLManifestName("x86_64-linux-musl", f)
		e, ok := byName[name]
		if !ok {
			t.Fatalf("missing openssl entry %s (got %v)", name, byName)
		}
		if e.SHA256 != sha256Hex([]byte(testOpenSSLContents[f])) {
			t.Errorf("%s sha256 = %s, want the archive content hash", name, e.SHA256)
		}
		if e.Kind != "blob" {
			t.Errorf("%s kind = %q, want blob", name, e.Kind)
		}
		if len(e.Sources) != 3 {
			t.Fatalf("%s has %d sources, want 3", name, len(e.Sources))
		}
		if e.Sources[2].Archive == "" || e.Sources[2].ArchivePath != "usr/lib/"+f {
			t.Errorf("%s fallback source = %+v, want upstream apk member usr/lib/%s", name, e.Sources[2], f)
		}
	}
}

// TestBuildRuntimeManifestFromCatalog_SkipsUnpublishedOpenSSL is the best-effort
// contract: an unpublished archive set must NOT strand the LLVM (or musl)
// entries — the projection stays green and simply omits openssl.
func TestBuildRuntimeManifestFromCatalog_SkipsUnpublishedOpenSSL(t *testing.T) {
	root := fakeOpenSSLRoot(t, "linux-amd64", "https://example.test/openssl.apk", "")
	seedSlimCatalog(t, root, map[string]string{"opt": "OPT", "llc": "LLC"})
	// Deliberately do NOT seed openssl blobs.

	m, err := BuildRuntimeManifestFromCatalog(root, "linux-amd64", "2026.0")
	if err != nil {
		t.Fatalf("an unpublished openssl blob must not fail the projection: %v", err)
	}
	for _, e := range m.Entries {
		if strings.HasPrefix(e.Name, "openssl-") {
			t.Errorf("projected an unpublished openssl entry %q — the runtime would fail to resolve it", e.Name)
		}
	}
}

// TestEmbedOpenSSL_UnpinnedWritesPlaceholder is the "keeps offline / pre-pin
// `bin/build` green" contract (the whole reason EmbedOpenSSL is best-effort):
// when OpenSSL is neither pinned nor hosted, EmbedOpenSSL must NOT fail the
// build — it drops the PLACEHOLDER sentinel (so the go:embed glob still resolves)
// and returns nil. No real archive is staged, so the compiler reads OpenSSL as
// "not available" and links nothing. Exercises writeOpenSSLPlaceholder.
func TestEmbedOpenSSL_UnpinnedWritesPlaceholder(t *testing.T) {
	if !IsLinux() {
		t.Skip("OpenSSL is staged on Linux hosts only")
	}
	target := CurrentBuildTarget()
	arch, err := OpenSSLArchDir(target)
	if err != nil {
		t.Skipf("no static OpenSSL for host target %s", target)
	}
	// Blank sha256 + no catalog blobs → openSSLPinned is false.
	root := fakeOpenSSLRoot(t, target, "https://example.test/openssl.apk", "")

	if err := EmbedOpenSSL(root); err != nil {
		t.Fatalf("EmbedOpenSSL must be best-effort (return nil) when unpinned: %v", err)
	}
	embedDir := filepath.Join(root, "compiler", "cmd", "promise", "resources", "openssl", arch)
	// Placeholder present so the //go:embed directive resolves.
	if !Exists(filepath.Join(embedDir, "PLACEHOLDER")) {
		t.Error("unpinned EmbedOpenSSL did not write the PLACEHOLDER sentinel")
	}
	// No real archives — the compiler must read this as "OpenSSL not available".
	for _, name := range []string{"libssl.a", "libcrypto.a"} {
		if Exists(filepath.Join(embedDir, name)) {
			t.Errorf("unpinned EmbedOpenSSL staged a real %s — expected placeholder only", name)
		}
	}
}

// TestEmbedOpenSSL_StagesFromCatalog covers the pinned success path: with the
// archives hosted in the blob catalog, EmbedOpenSSL fetches and stages the real
// libssl.a/libcrypto.a into the embed dir (and drops no placeholder). Mirrors
// TestEmbedMuslCRT_StagesFromPrebuilt.
func TestEmbedOpenSSL_StagesFromCatalog(t *testing.T) {
	if !IsLinux() {
		t.Skip("OpenSSL is staged on Linux hosts only")
	}
	target := CurrentBuildTarget()
	arch, err := OpenSSLArchDir(target)
	if err != nil {
		t.Skipf("no static OpenSSL for host target %s", target)
	}
	root := fakeOpenSSLRoot(t, target, "https://example.test/openssl.apk", "")
	brs := seedOpenSSLCatalog(t, root, target, testOpenSSLContents)

	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	prev := defaultBlobFetcher
	defaultBlobFetcher = &countingBlobFetcher{assets: brs}
	t.Cleanup(func() { defaultBlobFetcher = prev })

	if err := EmbedOpenSSL(root); err != nil {
		t.Fatalf("EmbedOpenSSL (catalog-hosted): %v", err)
	}
	embedDir := filepath.Join(root, "compiler", "cmd", "promise", "resources", "openssl", arch)
	for name, want := range testOpenSSLContents {
		got, err := os.ReadFile(filepath.Join(embedDir, name))
		if err != nil {
			t.Fatalf("read staged %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("staged %s = %q, want %q", name, got, want)
		}
	}
	// When real archives are staged, no placeholder should linger.
	if Exists(filepath.Join(embedDir, "PLACEHOLDER")) {
		t.Error("PLACEHOLDER survived a real stage — availability probe would still see the sentinel")
	}
}

// TestEmbedOpenSSL_WipesStalePlaceholder covers the pre-wipe: a stale real
// archive left from a prior run must not survive an unpinned re-embed, where the
// `openssl/<arch>/*` glob would otherwise sweep it back into the binary and make
// a placeholder build falsely advertise OpenSSL.
func TestEmbedOpenSSL_WipesStalePlaceholder(t *testing.T) {
	if !IsLinux() {
		t.Skip("OpenSSL is staged on Linux hosts only")
	}
	target := CurrentBuildTarget()
	arch, err := OpenSSLArchDir(target)
	if err != nil {
		t.Skipf("no static OpenSSL for host target %s", target)
	}
	root := fakeOpenSSLRoot(t, target, "https://example.test/openssl.apk", "")

	embedDir := filepath.Join(root, "compiler", "cmd", "promise", "resources", "openssl", arch)
	if err := os.MkdirAll(embedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(embedDir, "libssl.a")
	if err := os.WriteFile(stale, []byte("STALE-REAL-ARCHIVE"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := EmbedOpenSSL(root); err != nil {
		t.Fatalf("EmbedOpenSSL: %v", err)
	}
	if Exists(stale) {
		t.Error("stale libssl.a survived the unpinned re-embed and would be embedded, falsely advertising OpenSSL")
	}
}
