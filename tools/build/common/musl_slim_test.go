package common

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// musl_slim_test.go covers the musl CRT prebuilt path (T0530): fetching the CRT
// objects from hosted blobs (or the upstream Alpine apk), staging them for
// `go:embed`, and projecting arch-qualified manifest entries so the runtime can
// resolve a CRT from the content-addressed store.

const testMuslVersion = "1.2.5-r23"

// muslPrebuiltsTOML renders a prebuilts.toml declaring llvm (for the target,
// so BuildRuntimeManifestFromCatalog's llvmTargetEntry lookup succeeds) plus
// musl at muslURL.
func muslPrebuiltsTOML(target, muslURL, muslSHA string) string {
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

[binaries.musl]
version = "` + testMuslVersion + `"
bundle_dir = "compiler/cmd/promise/resources/crt"
[binaries.musl.targets.` + target + `]
url = "` + muslURL + `"
sha256 = "` + muslSHA + `"
files = [
  { src = "usr/lib/crt1.o", out = "crt1.o" },
  { src = "usr/lib/crti.o", out = "crti.o" },
  { src = "usr/lib/crtn.o", out = "crtn.o" },
  { src = "usr/lib/libc.a", out = "libc.a" },
]
`
}

// fakeMuslRoot creates a repo root whose prebuilts.toml declares musl for
// target. The musl URL/sha are placeholders unless the caller overrides them.
func fakeMuslRoot(t *testing.T, target, muslURL, muslSHA string) string {
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
		[]byte(muslPrebuiltsTOML(target, muslURL, muslSHA)), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// seedMuslCatalog records musl blobs for target in blobs.json and returns the
// `<sha>.br` asset map a stub fetcher serves.
func seedMuslCatalog(t *testing.T, root, target string, contents map[string]string) map[string][]byte {
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
			Dependency:       "musl",
			Version:          testMuslVersion,
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

var testCRTContents = map[string]string{
	"crt1.o": "CRT1",
	"crti.o": "CRTI",
	"crtn.o": "CRTN",
	"libc.a": "LIBC",
}

// TestPrebuiltsManifest_MuslReal checks the REAL prebuilts.toml against
// MuslTargets(): every Linux target the build tools know about must declare a
// pinned musl entry with the full CRT file set. Without this, adding a target to
// linuxTargetArchDirs and forgetting prebuilts.toml would only surface as a build
// failure on that architecture's host.
func TestPrebuiltsManifest_MuslReal(t *testing.T) {
	root, err := RootForTests()
	if err != nil {
		t.Skipf("find root: %v", err)
	}
	m, err := LoadPrebuiltsManifest(root)
	if err != nil {
		t.Fatalf("LoadPrebuiltsManifest: %v", err)
	}
	musl := m.Binaries["musl"]
	if musl == nil {
		t.Fatal("prebuilts.toml has no [binaries.musl] entry — the CRT would fall back to host /usr/lib scraping")
	}
	if musl.Version == "" {
		t.Error("musl.version is empty")
	}
	for _, target := range MuslTargets() {
		te := musl.Targets[target]
		if te == nil {
			t.Errorf("missing [binaries.musl.targets.%s]", target)
			continue
		}
		if te.URL == "" {
			t.Errorf("target %s: empty url", target)
		}
		if te.SHA256 == "" {
			t.Errorf("target %s: unpinned sha256 — run `bin/pin-prebuilts -only musl`", target)
		}
		got := map[string]bool{}
		for _, f := range te.Files {
			got[f.Out] = true
		}
		// Must match muslCRTFiles in compiler/cmd/promise/main.go — a missing
		// object makes every static link fail.
		for _, want := range []string{"crt1.o", "crti.o", "crtn.o", "libc.a"} {
			if !got[want] {
				t.Errorf("target %s: no %s in files", target, want)
			}
		}
		if _, err := MuslArchDir(target); err != nil {
			t.Errorf("MuslTargets() lists %s but MuslArchDir rejects it: %v", target, err)
		}
	}
}

func TestMuslArchDir(t *testing.T) {
	for target, want := range map[string]string{
		"linux-amd64": "x86_64-linux-musl",
		"linux-arm64": "aarch64-linux-musl",
	} {
		got, err := MuslArchDir(target)
		if err != nil {
			t.Fatalf("MuslArchDir(%q): %v", target, err)
		}
		if got != want {
			t.Errorf("MuslArchDir(%q) = %q, want %q", target, got, want)
		}
	}
	// A non-Linux target must be an error, not a silent default — guessing would
	// stage one arch's CRT under another arch's name.
	if _, err := MuslArchDir("darwin-arm64"); err == nil {
		t.Error("MuslArchDir(darwin-arm64) should fail rather than default to an arch")
	}
}

func TestMuslManifestName(t *testing.T) {
	got := MuslManifestName("aarch64-linux-musl", "crt1.o")
	if got != "musl-aarch64-linux-musl-crt1.o" {
		t.Errorf("MuslManifestName = %q", got)
	}
	// The two arches must not collide — that is the whole point of qualifying.
	if MuslManifestName("x86_64-linux-musl", "crt1.o") == got {
		t.Error("arch-qualified names collided across arches")
	}
}

func TestEnsureMuslBlobs_CatalogHit(t *testing.T) {
	root := fakeMuslRoot(t, "linux-amd64", "https://example.test/musl-dev.apk", "")
	brs := seedMuslCatalog(t, root, "linux-amd64", testCRTContents)

	cacheRoot := t.TempDir()
	t.Setenv("PROMISE_PREBUILTS_CACHE", cacheRoot)
	prev := defaultBlobFetcher
	defaultBlobFetcher = &countingBlobFetcher{assets: brs}
	t.Cleanup(func() { defaultBlobFetcher = prev })

	dir, err := EnsureMuslBlobs(root, "linux-amd64")
	if err != nil {
		t.Fatalf("EnsureMuslBlobs: %v", err)
	}
	wantDir := filepath.Join(cacheRoot, "musl-slim", testMuslVersion, "linux-amd64")
	if dir != wantDir {
		t.Errorf("cache dir = %q, want %q", dir, wantDir)
	}
	for name, want := range testCRTContents {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !Exists(filepath.Join(dir, toolsOKFile)) {
		t.Error("tools.ok sentinel missing after a successful fetch")
	}
}

// TestEnsureMuslBlobs_CatalogMiss_FallsBackToAPK is the property that unblocks a
// host before any blob has been published: with an empty catalog, the CRT still
// arrives — downloaded and sliced straight out of the pinned upstream apk.
func TestEnsureMuslBlobs_CatalogMiss_FallsBackToAPK(t *testing.T) {
	apkBytes, err := os.ReadFile(muslDevAPK(t))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(apkBytes)
	}))
	t.Cleanup(srv.Close)

	root := fakeMuslRoot(t, "linux-arm64", srv.URL+"/musl-dev.apk", sha256Hex(apkBytes))
	cacheRoot := t.TempDir()
	t.Setenv("PROMISE_PREBUILTS_CACHE", cacheRoot)

	dir, err := EnsureMuslBlobs(root, "linux-arm64")
	if err != nil {
		t.Fatalf("EnsureMuslBlobs (apk fallback): %v", err)
	}
	wantDir := filepath.Join(cacheRoot, "musl", testMuslVersion, "linux-arm64")
	if dir != wantDir {
		t.Errorf("fallback returned %q, want the upstream cache dir %q", dir, wantDir)
	}
	for name, want := range testCRTContents {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s from the apk fallback: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// The slim cache must not be created when the fallback runs.
	if Exists(filepath.Join(cacheRoot, "musl-slim")) {
		t.Error("musl-slim/ should not be created when falling back to the apk")
	}
}

func TestEnsureMuslBlobs_RejectsNonLinuxTarget(t *testing.T) {
	root := fakeMuslRoot(t, "linux-amd64", "https://example.test/musl-dev.apk", "")
	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	if _, err := EnsureMuslBlobs(root, "darwin-arm64"); err == nil {
		t.Fatal("expected EnsureMuslBlobs to reject a non-Linux target")
	}
}

// TestEnsureMuslBlobs_SeparateCacheFromLLVM pins the cache-key fix that came with
// generalizing slimToolsDigest: two dependencies must never share a sentinel.
func TestEnsureMuslBlobs_SeparateCacheFromLLVM(t *testing.T) {
	plan := []slimPlanFile{{Out: "crt1.o", BE: &BlobEntry{SHA256: "deadbeef", Size: 4, Compression: compressionBrotli}}}
	if slimToolsDigest("musl", "1.0", "linux-amd64", plan) == slimToolsDigest("llvm", "1.0", "linux-amd64", plan) {
		t.Error("musl and llvm digests collide for an identical plan — a dep swap would serve a stale cache")
	}
}

// TestEmbedMuslCRT_StagesFromPrebuilt is the regression test for the reported
// failure: `bin/build` must stage the CRT from the fetched prebuilt, with no
// /usr/lib/<arch>-linux-musl on the host. It runs against the HOST target, so on
// an arm64 Alpine container it exercises exactly the path that used to fail.
func TestEmbedMuslCRT_StagesFromPrebuilt(t *testing.T) {
	if !IsLinux() {
		t.Skip("musl CRT is staged on Linux hosts only")
	}
	target := CurrentBuildTarget()
	arch, err := MuslArchDir(target)
	if err != nil {
		t.Skipf("no musl CRT for host target %s", target)
	}
	root := fakeMuslRoot(t, target, "https://example.test/musl-dev.apk", "")
	brs := seedMuslCatalog(t, root, target, testCRTContents)

	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	prev := defaultBlobFetcher
	defaultBlobFetcher = &countingBlobFetcher{assets: brs}
	t.Cleanup(func() { defaultBlobFetcher = prev })

	if err := EmbedMuslCRT(root); err != nil {
		t.Fatalf("EmbedMuslCRT: %v", err)
	}
	embedDir := filepath.Join(root, "compiler", "cmd", "promise", "resources", "crt", arch)
	for name, want := range testCRTContents {
		got, err := os.ReadFile(filepath.Join(embedDir, name))
		if err != nil {
			t.Fatalf("read staged %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("staged %s = %q, want %q", name, got, want)
		}
	}
}

// TestEmbedMuslCRT_WipesStaleFiles covers the pre-wipe: a file dropped from
// prebuilts.toml must not survive in the embed dir, where the `crt/<arch>/*`
// glob would sweep it back into the binary.
func TestEmbedMuslCRT_WipesStaleFiles(t *testing.T) {
	if !IsLinux() {
		t.Skip("musl CRT is staged on Linux hosts only")
	}
	target := CurrentBuildTarget()
	arch, err := MuslArchDir(target)
	if err != nil {
		t.Skipf("no musl CRT for host target %s", target)
	}
	root := fakeMuslRoot(t, target, "https://example.test/musl-dev.apk", "")
	brs := seedMuslCatalog(t, root, target, testCRTContents)

	embedDir := filepath.Join(root, "compiler", "cmd", "promise", "resources", "crt", arch)
	if err := os.MkdirAll(embedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(embedDir, "Scrt1.o")
	if err := os.WriteFile(stale, []byte("STALE"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	prev := defaultBlobFetcher
	defaultBlobFetcher = &countingBlobFetcher{assets: brs}
	t.Cleanup(func() { defaultBlobFetcher = prev })

	if err := EmbedMuslCRT(root); err != nil {
		t.Fatalf("EmbedMuslCRT: %v", err)
	}
	if Exists(stale) {
		t.Error("stale Scrt1.o survived the re-stage and would be embedded")
	}
}

// TestBuildRuntimeManifestFromCatalog_IncludesMusl asserts published musl blobs
// are projected with ARCH-QUALIFIED names, alongside the LLVM entries.
func TestBuildRuntimeManifestFromCatalog_IncludesMusl(t *testing.T) {
	root := fakeMuslRoot(t, "linux-amd64", "https://example.test/musl-dev.apk", "")
	seedSlimCatalog(t, root, map[string]string{"opt": "OPT", "llc": "LLC"})
	seedMuslCatalog(t, root, "linux-amd64", testCRTContents)

	m, err := BuildRuntimeManifestFromCatalog(root, "linux-amd64", "2026.0")
	if err != nil {
		t.Fatalf("BuildRuntimeManifestFromCatalog: %v", err)
	}
	byName := map[string]runtimeManifestEntry{}
	for _, e := range m.Entries {
		byName[e.Name] = e
	}
	for _, want := range []string{"llvm-opt", "llvm-llc"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("missing %s entry", want)
		}
	}
	for _, f := range []string{"crt1.o", "crti.o", "crtn.o", "libc.a"} {
		name := MuslManifestName("x86_64-linux-musl", f)
		e, ok := byName[name]
		if !ok {
			t.Fatalf("missing musl entry %s (got %v)", name, byName)
		}
		if e.SHA256 != sha256Hex([]byte(testCRTContents[f])) {
			t.Errorf("%s sha256 = %s, want the CRT content hash", name, e.SHA256)
		}
		if e.Kind != "blob" {
			t.Errorf("%s kind = %q, want blob (CRT objects are never patched or signed)", name, e.Kind)
		}
		// Ranked sources: release blob, mirror blob, upstream apk last.
		if len(e.Sources) != 3 {
			t.Fatalf("%s has %d sources, want 3", name, len(e.Sources))
		}
		if e.Sources[2].Archive == "" || e.Sources[2].ArchivePath != "usr/lib/"+f {
			t.Errorf("%s fallback source = %+v, want the upstream apk member usr/lib/%s", name, e.Sources[2], f)
		}
	}
}

// TestBuildRuntimeManifestFromCatalog_SkipsUnpublishedMusl is the best-effort
// contract: an unpublished CRT must NOT strand the LLVM entries. Making musl
// fatal here would drop the whole manifest to the empty placeholder and silently
// remove the host's ability to self-fetch LLVM.
func TestBuildRuntimeManifestFromCatalog_SkipsUnpublishedMusl(t *testing.T) {
	root := fakeMuslRoot(t, "linux-amd64", "https://example.test/musl-dev.apk", "")
	seedSlimCatalog(t, root, map[string]string{"opt": "OPT", "llc": "LLC"})
	// Deliberately do NOT seed musl blobs.

	m, err := BuildRuntimeManifestFromCatalog(root, "linux-amd64", "2026.0")
	if err != nil {
		t.Fatalf("an unpublished musl blob must not fail the projection: %v", err)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("expected only the 2 LLVM entries, got %d", len(m.Entries))
	}
	for _, e := range m.Entries {
		if strings.HasPrefix(e.Name, "musl-") {
			t.Errorf("projected an unpublished musl entry %q — the runtime would fail to resolve it", e.Name)
		}
	}
}

// TestBuildRuntimeManifestFromCatalog_PartialMuslIsAllOrNothing: a half-published
// CRT set must project NO musl entries. resolveMuslCRTView needs all four
// objects; projecting a subset would make it fall through anyway while making
// the manifest claim coverage it doesn't have.
func TestBuildRuntimeManifestFromCatalog_PartialMuslIsAllOrNothing(t *testing.T) {
	root := fakeMuslRoot(t, "linux-amd64", "https://example.test/musl-dev.apk", "")
	seedSlimCatalog(t, root, map[string]string{"opt": "OPT", "llc": "LLC"})
	seedMuslCatalog(t, root, "linux-amd64", map[string]string{"crt1.o": "CRT1", "crti.o": "CRTI"})

	m, err := BuildRuntimeManifestFromCatalog(root, "linux-amd64", "2026.0")
	if err != nil {
		t.Fatalf("BuildRuntimeManifestFromCatalog: %v", err)
	}
	for _, e := range m.Entries {
		if strings.HasPrefix(e.Name, "musl-") {
			t.Errorf("projected musl entry %q from a partial blob set", e.Name)
		}
	}
}

// TestBuildRuntimeManifestFromCatalog_NoMuslForDarwin: a macOS host's manifest
// carries no CRT entries — it has no Linux workflow to pre-stage for.
func TestBuildRuntimeManifestFromCatalog_NoMuslForDarwin(t *testing.T) {
	root := fakeMuslRoot(t, "darwin-arm64", "https://example.test/musl-dev.apk", "")
	// llvm entries for darwin-arm64 so the projection itself succeeds.
	cat, err := LoadBlobsCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"opt", "llc"} {
		raw := []byte(name)
		if err := cat.Upsert(BlobEntry{
			Dependency: "llvm", Version: "22.1.0", Target: "darwin-arm64", Name: name,
			SHA256: sha256Hex(raw), Size: int64(len(raw)), Compression: compressionBrotli,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteBlobsCatalog(root, cat); err != nil {
		t.Fatal(err)
	}

	m, err := BuildRuntimeManifestFromCatalog(root, "darwin-arm64", "2026.0")
	if err != nil {
		t.Fatalf("BuildRuntimeManifestFromCatalog: %v", err)
	}
	for _, e := range m.Entries {
		if strings.HasPrefix(e.Name, "musl-") {
			t.Errorf("darwin manifest carries musl entry %q", e.Name)
		}
	}
}
