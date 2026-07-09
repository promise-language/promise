package common

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunBuild_RejectsFetchFlag confirms the legacy -fetch flag is no longer
// accepted. Pre-Phase-3 it selected the upstream-tarball bundling path; after
// Phase 3 the cache-first flow is the only path, so -fetch is gone. Anyone
// scripting the old flag should get a clear "usage" error rather than silently
// running the wrong build.
func TestRunBuild_RejectsFetchFlag(t *testing.T) {
	for _, flag := range []string{"-fetch", "--fetch", "-fetch=llvm"} {
		t.Run(flag, func(t *testing.T) {
			err := RunBuild("/nonexistent", []string{flag})
			if err == nil {
				t.Fatalf("expected RunBuild(%q) to fail with usage error, got nil", flag)
			}
			if !strings.Contains(err.Error(), "usage:") {
				t.Errorf("expected usage error from RunBuild(%q), got: %v", flag, err)
			}
			if strings.Contains(err.Error(), "-fetch") {
				t.Errorf("usage error still mentions -fetch (should be removed): %v", err)
			}
		})
	}
}

// TestRunBuild_RejectsUnknownFlags ensures arbitrary unknown flags surface the
// usage error too — not just the historical -fetch we wrote a dedicated test
// for. Catches accidental flag introductions / typos.
func TestRunBuild_RejectsUnknownFlags(t *testing.T) {
	for _, flag := range []string{"-bogus", "--also-bogus", "-debug=on"} {
		t.Run(flag, func(t *testing.T) {
			err := RunBuild("/nonexistent", []string{flag})
			if err == nil {
				t.Fatalf("expected RunBuild(%q) to fail with usage error, got nil", flag)
			}
			if !strings.Contains(err.Error(), "usage:") {
				t.Errorf("expected usage error from RunBuild(%q), got: %v", flag, err)
			}
		})
	}
}

// TestBundleLLVM_NoEntry covers the manifest sanity-check branches in
// BundleLLVM that fire when the manifest is malformed for the running target.
// These are reachable from `bin/build --release` via FetchAll producing a
// cache dir for a target whose llvm entry got mangled in the manifest.
func TestBundleLLVM_NoEntry(t *testing.T) {
	t.Run("missing binaries.llvm", func(t *testing.T) {
		err := BundleLLVM(t.TempDir(), &PrebuiltsManifest{}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "missing [binaries.llvm]") {
			t.Errorf("expected missing-llvm-entry error, got: %v", err)
		}
	})

	t.Run("missing target entry", func(t *testing.T) {
		// Empty Targets map → tEntry == nil for the running target.
		m := &PrebuiltsManifest{
			Binaries: map[string]*PrebuiltEntry{
				"llvm": {Version: "1", BundleDir: "out", Targets: map[string]*TargetEntry{}},
			},
		}
		err := BundleLLVM(t.TempDir(), m, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "no [binaries.llvm.targets") {
			t.Errorf("expected missing-target-entry error, got: %v", err)
		}
	})

	t.Run("unsupported target surfaces reason", func(t *testing.T) {
		target := CurrentBuildTarget()
		m := &PrebuiltsManifest{
			Binaries: map[string]*PrebuiltEntry{
				"llvm": {
					Version:   "1",
					BundleDir: "out",
					Targets: map[string]*TargetEntry{
						target: {Unsupported: "test reason: target not supported"},
					},
				},
			},
		}
		err := BundleLLVM(t.TempDir(), m, t.TempDir())
		if err == nil {
			t.Fatal("expected unsupported error, got nil")
		}
		// The unsupported reason must be surfaced verbatim so users know why.
		if !strings.Contains(err.Error(), "test reason: target not supported") {
			t.Errorf("unsupported reason not surfaced; got: %v", err)
		}
	})
}

// TestBuildRuntimeManifestFromCatalog_PopulatesWithCatalogHit covers T0798's
// debug-build behavior: BuildRuntimeManifestFromCatalog must project an
// entry for every prebuilts.toml file when the catalog has them all, with
// the slim-blob source first and the upstream-archive fallback second. This
// is the path `bin/build` (both debug + release) takes to populate the
// embedded runtime manifest.
func TestBuildRuntimeManifestFromCatalog_PopulatesWithCatalogHit(t *testing.T) {
	root, _ := fakeReleaseRoot(t, nil)
	cat := &BlobsCatalog{Schema: BlobsCatalogSchema, Blobs: []BlobEntry{
		{Dependency: "llvm", Version: "22.1.0", Target: "linux-amd64", Name: "opt",
			SHA256: "1111111111111111111111111111111111111111111111111111111111111111",
			Size:   10, Compression: compressionBrotli, CompressedSize: 5},
		{Dependency: "llvm", Version: "22.1.0", Target: "linux-amd64", Name: "llc",
			SHA256: "2222222222222222222222222222222222222222222222222222222222222222",
			Size:   20, Compression: compressionBrotli, CompressedSize: 8},
	}}
	if err := WriteBlobsCatalog(root, cat); err != nil {
		t.Fatal(err)
	}

	m, err := BuildRuntimeManifestFromCatalog(root, "linux-amd64", "2026.0")
	if err != nil {
		t.Fatalf("BuildRuntimeManifestFromCatalog: %v", err)
	}
	if m.Schema != runtimeManifestSchema || m.Epoch != "2026.0" {
		t.Fatalf("schema/epoch = %d/%q", m.Schema, m.Epoch)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(m.Entries))
	}
	byName := map[string]runtimeManifestEntry{}
	for _, e := range m.Entries {
		byName[e.Name] = e
	}
	opt, ok := byName["llvm-opt"]
	if !ok {
		t.Fatal("missing llvm-opt entry")
	}
	if len(opt.Sources) != 3 {
		t.Fatalf("expected 3 ranked sources (github blob, mirror blob, archive), got %d", len(opt.Sources))
	}
	wantBlob := releaseAssetBase + "/deps-llvm-22.1.0/" + opt.SHA256 + ".br"
	if opt.Sources[0].Blob != wantBlob {
		t.Errorf("source[0].Blob = %q, want %q", opt.Sources[0].Blob, wantBlob)
	}
	if opt.Sources[0].Compression != compressionBrotli {
		t.Errorf("source[0].Compression = %q", opt.Sources[0].Compression)
	}
	wantMirror := blobMirrorBase + "/" + opt.SHA256 + ".br"
	if opt.Sources[1].Blob != wantMirror {
		t.Errorf("source[1].Blob (mirror) = %q, want %q", opt.Sources[1].Blob, wantMirror)
	}
	if opt.Sources[1].Compression != compressionBrotli {
		t.Errorf("source[1].Compression = %q", opt.Sources[1].Compression)
	}
	if opt.Sources[2].Archive == "" || opt.Sources[2].ArchivePath != "bin/opt" {
		t.Errorf("source[2] (archive fallback) malformed: %+v", opt.Sources[2])
	}

	// Re-serialize and re-parse to prove the projected manifest validates
	// against the same shape blobstore.Manifest will read at runtime — this
	// is what `bin/build` writes to resources/manifest.json.
	tmp := filepath.Join(t.TempDir(), "manifest.json")
	if err := writeRuntimeManifest(tmp, m); err != nil {
		t.Fatal(err)
	}
	rt, err := loadRuntimeManifest(tmp)
	if err != nil {
		t.Fatalf("emitted manifest fails its own loader: %v", err)
	}
	if got, err := json.Marshal(rt); err != nil || len(got) == 0 {
		t.Fatalf("re-emitted manifest empty (err=%v)", err)
	}
}

// TestBuildRuntimeManifestFromCatalog_MissingEntrySignalsSentinel covers the
// other side of the contract: when blobs.json has no entry for the host,
// the function returns the sentinel error so `bin/build` can fall back to
// the placeholder instead of failing the build outright (a developer who
// hasn't published blobs for their host still needs to build).
func TestBuildRuntimeManifestFromCatalog_MissingEntrySignalsSentinel(t *testing.T) {
	root, _ := fakeReleaseRoot(t, nil)
	// Empty catalog → every Lookup misses.
	_, err := BuildRuntimeManifestFromCatalog(root, "linux-amd64", "2026.0")
	if err == nil {
		t.Fatal("expected an error for empty catalog")
	}
	// The sentinel lets `bin/build` distinguish a recoverable miss (placeholder
	// fallback) from a genuine failure (e.g. malformed prebuilts.toml). The
	// sentinel is wrapped with `%w`, so errors.Is is the contract `bin/build`
	// itself uses.
	if !errors.Is(err, errCatalogMissForHost) {
		t.Errorf("error should wrap errCatalogMissForHost so bin/build can detect it, got: %v", err)
	}
	// And the message must reference publish-blobs so a maintainer knows the
	// remediation.
	if !strings.Contains(err.Error(), "publish-blobs") {
		t.Errorf("error should mention publish-blobs as remediation, got: %v", err)
	}
}

// TestBuildRuntimeManifestFromCatalog_ExcludesBuildOnly is the T0833 regression
// guard for the projection side of constraint #2: a build-only tool declared in
// prebuilts.toml must NOT appear in the client runtime manifest, and — crucially
// — its absence from blobs.json must NOT trip the catalog-miss sentinel (the
// build host hosts/uses the build-only blob out of band, not via this client
// projection). The client tools (opt/llc) are present in the catalog and project
// normally.
func TestBuildRuntimeManifestFromCatalog_ExcludesBuildOnly(t *testing.T) {
	root, _ := fakeReleaseRoot(t, nil)
	// Rewrite prebuilts.toml to add a build-only llvm-dlltool alongside opt/llc.
	prebuilts := `schema = 1
[binaries.llvm]
version = "22.1.0"
bundle_dir = "compiler/cmd/promise/resources/llvm"
[binaries.llvm.targets.linux-amd64]
url = "https://example.test/LLVM-22.1.0-Linux-X64.tar.xz"
sha256 = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0"
files = [
  { src = "bin/opt", out = "opt" },
  { src = "bin/llc", out = "llc" },
  { src = "bin/llvm-dlltool", out = "llvm-dlltool", build_only = true },
]
`
	if err := os.WriteFile(filepath.Join(root, "tools", "build", "prebuilts.toml"), []byte(prebuilts), 0o644); err != nil {
		t.Fatal(err)
	}
	// Catalog has only the client tools — llvm-dlltool is intentionally absent
	// (it would normally be hosted by publish-blobs, out of band).
	cat := &BlobsCatalog{Schema: BlobsCatalogSchema, Blobs: []BlobEntry{
		{Dependency: "llvm", Version: "22.1.0", Target: "linux-amd64", Name: "opt",
			SHA256: "1111111111111111111111111111111111111111111111111111111111111111",
			Size:   10, Compression: compressionBrotli, CompressedSize: 5},
		{Dependency: "llvm", Version: "22.1.0", Target: "linux-amd64", Name: "llc",
			SHA256: "2222222222222222222222222222222222222222222222222222222222222222",
			Size:   20, Compression: compressionBrotli, CompressedSize: 8},
	}}
	if err := WriteBlobsCatalog(root, cat); err != nil {
		t.Fatal(err)
	}

	m, err := BuildRuntimeManifestFromCatalog(root, "linux-amd64", "2026.0")
	if err != nil {
		t.Fatalf("BuildRuntimeManifestFromCatalog (build-only excluded): %v", err)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("expected 2 client entries (opt, llc), got %d", len(m.Entries))
	}
	for _, e := range m.Entries {
		if e.Name == "llvm-llvm-dlltool" {
			t.Errorf("build-only llvm-dlltool leaked into the client manifest: %+v", e)
		}
	}
}

// writeUpToDateBinary lays down a fake compiler binary plus its .promise.buildinfo
// in binDir under root, returns the binary's mtime. It uses a fixed mtime so a
// caller can place sources before/after it deterministically.
func writeUpToDateBinary(t *testing.T, root, binDir, version string) time.Time {
	t.Helper()
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(binDir, BinaryName())
	if err := os.WriteFile(binPath, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, ".promise.buildinfo"), []byte(version), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pin the binary mtime to a fixed instant so .def staleness is unambiguous.
	mtime := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(binPath, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return mtime
}

// TestIsBinaryUpToDateWinlinkDefStaleness is the build.go half of T0835: a .def
// edited after the binary was built must make isBinaryUpToDate return false, so
// RunBuild reruns the pipeline (and EmbedResources → ensureWinlinkLibs
// regenerates the embedded .lib). An older .def must stay up to date.
func TestIsBinaryUpToDateWinlinkDefStaleness(t *testing.T) {
	const version = "test-version"
	defFileRoot := func(t *testing.T) (root, defPath string) {
		root = t.TempDir()
		defDir := filepath.Join(root, filepath.FromSlash(winlinkDefDir))
		if err := os.MkdirAll(defDir, 0o755); err != nil {
			t.Fatal(err)
		}
		defPath = filepath.Join(defDir, "kernel32.def")
		if err := os.WriteFile(defPath, []byte("LIBRARY kernel32.dll\nEXPORTS\nExitProcess\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return root, defPath
	}

	t.Run("newer def invalidates", func(t *testing.T) {
		root, defPath := defFileRoot(t)
		binDir := filepath.Join(root, "bin")
		mtime := writeUpToDateBinary(t, root, binDir, version)
		newer := mtime.Add(2 * time.Second)
		if err := os.Chtimes(defPath, newer, newer); err != nil {
			t.Fatal(err)
		}
		if isBinaryUpToDate(root, binDir, version) {
			t.Error("a .def newer than the binary must invalidate the up-to-date check")
		}
	})

	t.Run("older def stays up to date", func(t *testing.T) {
		root, defPath := defFileRoot(t)
		binDir := filepath.Join(root, "bin")
		mtime := writeUpToDateBinary(t, root, binDir, version)
		older := mtime.Add(-2 * time.Second)
		if err := os.Chtimes(defPath, older, older); err != nil {
			t.Fatal(err)
		}
		if !isBinaryUpToDate(root, binDir, version) {
			t.Error("an older .def (and no other newer sources) should stay up to date")
		}
	})
}
