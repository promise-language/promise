package common

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fakeReleaseRoot creates a temp repo root with the minimal catalog.toml and
// prebuilts.toml that the release subcommands read, plus a blobs dir holding the
// given {out-name: content} extracted LLVM blobs for the "linux-amd64" target.
func fakeReleaseRoot(t *testing.T, blobs map[string]string) (root, blobsDir string) {
	t.Helper()
	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "catalog.toml"), []byte("epoch = \"2026.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolsBuild := filepath.Join(root, "tools", "build")
	if err := os.MkdirAll(toolsBuild, 0o755); err != nil {
		t.Fatal(err)
	}
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
]
`
	if err := os.WriteFile(filepath.Join(toolsBuild, "prebuilts.toml"), []byte(prebuilts), 0o644); err != nil {
		t.Fatal(err)
	}
	blobsDir = filepath.Join(root, "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range blobs {
		if err := os.WriteFile(filepath.Join(blobsDir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, blobsDir
}

func TestSplitPositionalFlags(t *testing.T) {
	pos, flags := splitPositionalFlags([]string{"blobsdir", "--host", "linux-amd64", "--pack", "p", "--out", "o"})
	if !reflect.DeepEqual(pos, []string{"blobsdir"}) {
		t.Fatalf("positionals = %v", pos)
	}
	want := []string{"--host", "linux-amd64", "--pack", "p", "--out", "o"}
	if !reflect.DeepEqual(flags, want) {
		t.Fatalf("flags = %v", flags)
	}
	// --flag=value form keeps the value attached (no extra token consumed).
	pos, flags = splitPositionalFlags([]string{"--out=o", "x"})
	if !reflect.DeepEqual(pos, []string{"x"}) || !reflect.DeepEqual(flags, []string{"--out=o"}) {
		t.Fatalf("eq form: pos=%v flags=%v", pos, flags)
	}
}

func TestReleaseManifest(t *testing.T) {
	root, blobsDir := fakeReleaseRoot(t, map[string]string{"opt": "OPT", "llc": "LLC"})
	packDir := filepath.Join(root, "pack")
	manifestOut := filepath.Join(root, "out", "manifest.json")

	if err := runReleaseManifest(root, []string{
		blobsDir, "--host", "linux-amd64", "--pack", packDir, "--out", manifestOut, "--tag", "epoch-2026.0",
	}); err != nil {
		t.Fatal(err)
	}

	m, err := loadRuntimeManifest(manifestOut) // also asserts schema-lockstep validation
	if err != nil {
		t.Fatalf("produced manifest fails validation: %v", err)
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
	wantHash := sha256Hex([]byte("OPT"))
	if opt.SHA256 != wantHash || opt.Size != 3 {
		t.Fatalf("opt sha256/size = %s/%d (want %s/3)", opt.SHA256, opt.Size, wantHash)
	}
	if opt.Kind != "blob" {
		t.Fatalf("linux opt kind = %q, want blob", opt.Kind)
	}
	// Ranked sources: GitHub release-asset blob first (brotli-compressed, so its
	// URL carries the .br suffix and the source declares the codec), upstream
	// archive second (uncompressed, no codec).
	if len(opt.Sources) != 2 {
		t.Fatalf("expected 2 ranked sources, got %d", len(opt.Sources))
	}
	wantBlob := githubAssetURL("epoch-2026.0", wantHash) + ".br"
	if opt.Sources[0].Blob != wantBlob {
		t.Fatalf("source[0].Blob = %q, want %q", opt.Sources[0].Blob, wantBlob)
	}
	if opt.Sources[0].Compression != compressionBrotli {
		t.Fatalf("source[0].Compression = %q, want %q", opt.Sources[0].Compression, compressionBrotli)
	}
	s1 := opt.Sources[1]
	if s1.Archive != "https://example.test/LLVM-22.1.0-Linux-X64.tar.xz" || s1.ArchivePath != "bin/opt" ||
		s1.ArchiveSHA256 != "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0" ||
		s1.Compression != "" {
		t.Fatalf("source[1] = %+v", s1)
	}

	// packDir holds the brotli-compressed upload artifact named "<hash>.br";
	// decompressing it must reproduce the uncompressed content sha256.
	packed := filepath.Join(packDir, wantHash+".br")
	got, err := hashBlobArtifact(packed, compressionBrotli)
	if err != nil {
		t.Fatalf("packed artifact missing/undecodable: %v", err)
	}
	if got != wantHash {
		t.Fatalf("packed artifact decompresses to %s, want %s", got, wantHash)
	}
}

func TestReleaseManifestCachingIdempotent(t *testing.T) {
	root, blobsDir := fakeReleaseRoot(t, map[string]string{"opt": "OPT", "llc": "LLC"})
	packDir := filepath.Join(root, "pack")
	manifestOut := filepath.Join(root, "manifest.json")
	args := []string{blobsDir, "--host", "linux-amd64", "--pack", packDir, "--out", manifestOut}

	if err := runReleaseManifest(root, args); err != nil {
		t.Fatal(err)
	}
	packed := filepath.Join(packDir, sha256Hex([]byte("OPT"))+".br")
	info1, err := os.Stat(packed)
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(manifestOut)
	if err != nil {
		t.Fatal(err)
	}

	// Re-run: unchanged content → identical hash → the packed artifact is left
	// untouched (no re-upload) and the manifest is byte-identical.
	if err := runReleaseManifest(root, args); err != nil {
		t.Fatal(err)
	}
	info2, err := os.Stat(packed)
	if err != nil {
		t.Fatal(err)
	}
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Fatalf("packed artifact was rewritten (mtime changed) — caching skip failed")
	}
	second, err := os.ReadFile(manifestOut)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("manifest not reproducible across runs")
	}
}

func TestReleaseVerifyManifestGood(t *testing.T) {
	root, blobsDir := fakeReleaseRoot(t, map[string]string{"opt": "OPT", "llc": "LLC"})
	packDir := filepath.Join(root, "pack")
	manifestOut := filepath.Join(root, "manifest.json")
	if err := runReleaseManifest(root, []string{blobsDir, "--host", "linux-amd64", "--pack", packDir, "--out", manifestOut}); err != nil {
		t.Fatal(err)
	}
	if err := runReleaseVerifyManifest(root, []string{manifestOut, "--against", packDir}); err != nil {
		t.Fatalf("verify-manifest should pass for a good packDir, got: %v", err)
	}
}

func TestReleaseVerifyManifestCorrupt(t *testing.T) {
	root, blobsDir := fakeReleaseRoot(t, map[string]string{"opt": "OPT", "llc": "LLC"})
	packDir := filepath.Join(root, "pack")
	manifestOut := filepath.Join(root, "manifest.json")
	if err := runReleaseManifest(root, []string{blobsDir, "--host", "linux-amd64", "--pack", packDir, "--out", manifestOut}); err != nil {
		t.Fatal(err)
	}
	// Replace one packed artifact with VALID brotli of different content: it
	// decompresses cleanly but to bytes that no longer hash to the manifest's
	// content address — the integrity gate must catch the mismatch.
	corrupt := filepath.Join(packDir, sha256Hex([]byte("OPT"))+".br")
	tampered := filepath.Join(root, "tampered")
	if err := os.WriteFile(tampered, []byte("TAMPERED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := compressFileBrotli(tampered, corrupt); err != nil {
		t.Fatal(err)
	}
	err := runReleaseVerifyManifest(root, []string{manifestOut, "--against", packDir})
	if err == nil {
		t.Fatal("verify-manifest must fail on a corrupted artifact")
	}
	if !containsAll(err.Error(), "llvm-opt", "mismatch") {
		t.Fatalf("error should name the entry and the mismatch, got: %v", err)
	}
}

// TestReleaseVerifyManifestInvalidBrotli covers the OTHER compressed-artifact
// failure mode: a packaged .br asset that is not valid brotli at all (truncated
// upload, wrong codec) must fail the gate with a decode error, not silently pass.
func TestReleaseVerifyManifestInvalidBrotli(t *testing.T) {
	root, blobsDir := fakeReleaseRoot(t, map[string]string{"opt": "OPT", "llc": "LLC"})
	packDir := filepath.Join(root, "pack")
	manifestOut := filepath.Join(root, "manifest.json")
	if err := runReleaseManifest(root, []string{blobsDir, "--host", "linux-amd64", "--pack", packDir, "--out", manifestOut}); err != nil {
		t.Fatal(err)
	}
	garbage := filepath.Join(packDir, sha256Hex([]byte("OPT"))+".br")
	if err := os.WriteFile(garbage, []byte("not brotli at all"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := runReleaseVerifyManifest(root, []string{manifestOut, "--against", packDir})
	if err == nil {
		t.Fatal("verify-manifest must fail on an undecodable .br artifact")
	}
	if !containsAll(err.Error(), "llvm-opt") {
		t.Fatalf("error should name the entry, got: %v", err)
	}
}

// TestHashBlobArtifact unit-tests the integrity gate's content hasher directly,
// covering the three codec branches: uncompressed ("" / "none") hashes the raw
// bytes, "brotli" hashes the decompressed bytes (so a compressed artifact maps
// back to its uncompressed content address), and an unknown codec is rejected.
func TestHashBlobArtifact(t *testing.T) {
	dir := t.TempDir()
	content := []byte("the opt binary")
	want := sha256Hex(content)

	raw := filepath.Join(dir, "raw")
	if err := os.WriteFile(raw, content, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, codec := range []string{"", "none"} {
		got, err := hashBlobArtifact(raw, codec)
		if err != nil {
			t.Fatalf("codec %q: %v", codec, err)
		}
		if got != want {
			t.Fatalf("codec %q: hash = %s, want %s", codec, got, want)
		}
	}

	// A brotli artifact hashes to the UNCOMPRESSED content address.
	comp := filepath.Join(dir, "comp.br")
	if err := compressFileBrotli(raw, comp); err != nil {
		t.Fatal(err)
	}
	got, err := hashBlobArtifact(comp, compressionBrotli)
	if err != nil {
		t.Fatalf("brotli: %v", err)
	}
	if got != want {
		t.Fatalf("brotli artifact hashes to %s, want uncompressed %s", got, want)
	}

	// Unknown codec is rejected loudly.
	if _, err := hashBlobArtifact(raw, "lz4"); err == nil {
		t.Fatal("expected error for unknown codec")
	}
}

// TestCompressFileBrotliErrors covers compressFileBrotli's failure paths: a
// missing source and an unwritable destination both surface an error rather than
// producing a silent partial artifact.
func TestCompressFileBrotliErrors(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := compressFileBrotli(filepath.Join(dir, "does-not-exist"), filepath.Join(dir, "out.br")); err == nil {
		t.Fatal("expected error opening a missing source")
	}
	// Destination inside a nonexistent directory → os.Create fails.
	if err := compressFileBrotli(src, filepath.Join(dir, "no-such-dir", "out.br")); err == nil {
		t.Fatal("expected error creating dst under a missing directory")
	}
}

func TestReleaseVerifyManifestMissing(t *testing.T) {
	root, blobsDir := fakeReleaseRoot(t, map[string]string{"opt": "OPT", "llc": "LLC"})
	packDir := filepath.Join(root, "pack")
	manifestOut := filepath.Join(root, "manifest.json")
	if err := runReleaseManifest(root, []string{blobsDir, "--host", "linux-amd64", "--pack", packDir, "--out", manifestOut}); err != nil {
		t.Fatal(err)
	}
	// Verify against an empty dir — no artifact present for any entry → fail.
	empty := filepath.Join(root, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runReleaseVerifyManifest(root, []string{manifestOut, "--against", empty}); err == nil {
		t.Fatal("verify-manifest must fail when no packaged artifact is present")
	}
}

func TestReleaseBuildValidation(t *testing.T) {
	root := t.TempDir()
	// Bad variant.
	if err := runReleaseBuild(root, []string{"--variant", "bogus", "--manifest", "m", "--out", "o"}); err == nil {
		t.Fatal("expected error for bad variant")
	}
	// full requires --blobs.
	if err := runReleaseBuild(root, []string{"--variant", "full", "--manifest", "m", "--out", "o"}); err == nil {
		t.Fatal("expected error for full without --blobs")
	}
	// Cross-target is gated (a clearly-non-host target).
	if err := runReleaseBuild(root, []string{"--variant", "thin", "--manifest", "m", "--out", "o", "--host", "plan9-foo"}); err == nil {
		t.Fatal("expected error for cross-target build")
	}
}

func TestRunReleaseUnknownSubcommand(t *testing.T) {
	if err := RunRelease(t.TempDir(), []string{"frobnicate"}); err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
	if err := RunRelease(t.TempDir(), nil); err == nil {
		t.Fatal("expected usage error for no subcommand")
	}
}

func TestReleaseBlobsRequiresOut(t *testing.T) {
	if err := runReleaseBlobs(t.TempDir(), []string{"--host", "linux-amd64"}); err == nil {
		t.Fatal("expected error when --out is missing")
	}
}

// TestReleaseVerifyManifestArchiveSource exercises the archive-source verify path
// (hashArchiveMember + verifyManifestEntry's archive branch), which the
// bootstrap producer (GenerateRuntimeManifest) emits but the blob-source tests
// above never reach. A present archive whose extracted member hashes correctly
// passes; a tampered member must fail loudly.
func TestReleaseVerifyManifestArchiveSource(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "catalog.toml"), []byte("epoch = \"2026.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Build a real .tar.gz holding the member at a flat top-level path so
	// resolveInnerRoot's Layout-1 probe succeeds.
	tgz, err := makeTarGzContent(map[string]string{"bin/opt": "OPT"})
	if err != nil {
		t.Fatal(err)
	}
	against := filepath.Join(root, "against")
	if err := os.MkdirAll(against, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(against, "llvm.tar.gz"), tgz, 0o644); err != nil {
		t.Fatal(err)
	}

	manifest := filepath.Join(root, "manifest.json")
	mkManifest := func(sha string) {
		m := runtimeManifest{Schema: runtimeManifestSchema, Epoch: "2026.0", Entries: []runtimeManifestEntry{{
			Name:   "llvm-opt",
			SHA256: sha,
			Size:   3,
			Kind:   "blob",
			Sources: []runtimeSource{{
				Archive:     "https://example.test/llvm.tar.gz",
				ArchivePath: "bin/opt",
			}},
		}}}
		if err := writeRuntimeManifest(manifest, &m); err != nil {
			t.Fatal(err)
		}
	}

	// Correct member hash → pass.
	mkManifest(sha256Hex([]byte("OPT")))
	if err := runReleaseVerifyManifest(root, []string{manifest, "--against", against}); err != nil {
		t.Fatalf("archive-source verify should pass, got: %v", err)
	}

	// Manifest claims a different hash than the archive member yields → fail.
	mkManifest(sha256Hex([]byte("DIFFERENT")))
	err = runReleaseVerifyManifest(root, []string{manifest, "--against", against})
	if err == nil {
		t.Fatal("archive-source verify must fail on a member sha256 mismatch")
	}
	if !containsAll(err.Error(), "llvm-opt", "mismatch") {
		t.Fatalf("error should name the entry and the mismatch, got: %v", err)
	}
}

// TestHashArchiveMemberMissingPath guards the archive-source contract: an archive
// source without archive_path is rejected (mirrors validate()'s rule, but at the
// hashing layer).
func TestHashArchiveMemberMissingPath(t *testing.T) {
	if _, err := hashArchiveMember("anything.tar.gz", ""); err == nil {
		t.Fatal("expected error for archive source missing archive_path")
	}
}

// TestRuntimeManifestValidate covers validate()'s rejection branches — the
// schema-lockstep gate that guarantees a manifest this tool emits can never fail
// to parse in the compiler at runtime.
func TestRuntimeManifestValidate(t *testing.T) {
	okSource := runtimeSource{Blob: "https://example.test/abc"}
	cases := []struct {
		name string
		m    runtimeManifest
		want string
	}{
		{"bad schema", runtimeManifest{Schema: 999}, "unsupported manifest schema"},
		{"missing name", runtimeManifest{Schema: 1, Entries: []runtimeManifestEntry{
			{SHA256: "h", Sources: []runtimeSource{okSource}},
		}}, "missing name"},
		{"duplicate name", runtimeManifest{Schema: 1, Entries: []runtimeManifestEntry{
			{Name: "dup", SHA256: "h", Sources: []runtimeSource{okSource}},
			{Name: "dup", SHA256: "h", Sources: []runtimeSource{okSource}},
		}}, "duplicate name"},
		{"missing sha256", runtimeManifest{Schema: 1, Entries: []runtimeManifestEntry{
			{Name: "n", SHA256: "  ", Sources: []runtimeSource{okSource}},
		}}, "missing sha256"},
		{"no sources", runtimeManifest{Schema: 1, Entries: []runtimeManifestEntry{
			{Name: "n", SHA256: "h"},
		}}, "no sources"},
		{"empty source", runtimeManifest{Schema: 1, Entries: []runtimeManifestEntry{
			{Name: "n", SHA256: "h", Sources: []runtimeSource{{}}},
		}}, "neither blob nor archive"},
		{"archive without path", runtimeManifest{Schema: 1, Entries: []runtimeManifestEntry{
			{Name: "n", SHA256: "h", Sources: []runtimeSource{{Archive: "a.tgz"}}},
		}}, "archive without archive_path"},
		{"compression on archive", runtimeManifest{Schema: 1, Entries: []runtimeManifestEntry{
			{Name: "n", SHA256: "h", Sources: []runtimeSource{{Archive: "a.tgz", ArchivePath: "bin/x", Compression: "brotli"}}},
		}}, "compression only applies to blob"},
		{"unknown codec", runtimeManifest{Schema: 1, Entries: []runtimeManifestEntry{
			{Name: "n", SHA256: "h", Sources: []runtimeSource{{Blob: "u", Compression: "lz4"}}},
		}}, "unknown compression codec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.m.validate()
			if err == nil {
				t.Fatalf("expected validation error %q, got nil", tc.want)
			}
			if !containsAll(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}

	// A well-formed manifest with all source shapes validates clean: a plain
	// blob, a brotli-compressed blob, and an (uncompressed) archive.
	good := runtimeManifest{Schema: 1, Entries: []runtimeManifestEntry{
		{Name: "blobby", SHA256: "h", Sources: []runtimeSource{okSource}},
		{Name: "compressed", SHA256: "h", Sources: []runtimeSource{{Blob: "u.br", Compression: compressionBrotli}}},
		{Name: "archivey", SHA256: "h", Sources: []runtimeSource{{Archive: "a.tgz", ArchivePath: "bin/x"}}},
	}}
	if err := good.validate(); err != nil {
		t.Fatalf("well-formed manifest should validate, got: %v", err)
	}
}

// TestLoadRuntimeManifestErrors covers the read/parse failure paths.
func TestLoadRuntimeManifestErrors(t *testing.T) {
	if _, err := loadRuntimeManifest(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing manifest file")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRuntimeManifest(bad); err == nil {
		t.Fatal("expected error for malformed manifest JSON")
	}
	// Parses as JSON but fails the schema-lockstep validation.
	stale := filepath.Join(t.TempDir(), "stale.json")
	if err := os.WriteFile(stale, []byte(`{"schema":999,"entries":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRuntimeManifest(stale); err == nil {
		t.Fatal("expected validation error for stale schema")
	}
}

// TestLLVMTargetEntryErrors covers llvmTargetEntry's three rejection paths:
// missing [binaries.llvm], an unknown target, and an explicitly-unsupported one.
func TestLLVMTargetEntryErrors(t *testing.T) {
	writeRoot := func(t *testing.T, prebuilts string) string {
		t.Helper()
		root := t.TempDir()
		tb := filepath.Join(root, "tools", "build")
		if err := os.MkdirAll(tb, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tb, "prebuilts.toml"), []byte(prebuilts), 0o644); err != nil {
			t.Fatal(err)
		}
		return root
	}

	// No [binaries.llvm] table at all.
	root := writeRoot(t, "schema = 1\n")
	if _, _, err := llvmTargetEntry(root, "linux-amd64"); err == nil {
		t.Fatal("expected error for missing [binaries.llvm]")
	}

	// llvm present but no entry for the requested target.
	root = writeRoot(t, "schema = 1\n[binaries.llvm]\nversion = \"22.1.0\"\nbundle_dir = \"x\"\n")
	if _, _, err := llvmTargetEntry(root, "linux-amd64"); err == nil {
		t.Fatal("expected error for unknown target")
	}

	// Target present but explicitly marked unsupported.
	root = writeRoot(t, "schema = 1\n[binaries.llvm]\nversion = \"22.1.0\"\nbundle_dir = \"x\"\n"+
		"[binaries.llvm.targets.linux-amd64]\nunsupported = \"no toolchain\"\n")
	if _, _, err := llvmTargetEntry(root, "linux-amd64"); err == nil {
		t.Fatal("expected error for unsupported target")
	}
}

// TestRunReleaseHelp covers the help/dispatch arms of RunRelease.
func TestRunReleaseHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		if err := RunRelease(t.TempDir(), []string{arg}); err != nil {
			t.Fatalf("RunRelease %q should succeed, got: %v", arg, err)
		}
	}
}

// TestReleaseManifestBadTarget exercises the llvmTargetEntry error path through
// runReleaseManifest (a non-host/unknown target aborts before any packing).
func TestReleaseManifestBadTarget(t *testing.T) {
	root, blobsDir := fakeReleaseRoot(t, map[string]string{"opt": "OPT"})
	err := runReleaseManifest(root, []string{
		blobsDir, "--host", "plan9-foo", "--pack", filepath.Join(root, "pack"),
		"--out", filepath.Join(root, "m.json"), "--tag", "epoch-2026.0",
	})
	if err == nil {
		t.Fatal("expected error for unknown --host target")
	}
}

// TestReleaseManifestArgValidation covers the positional/flag requirement checks.
func TestReleaseManifestArgValidation(t *testing.T) {
	root, blobsDir := fakeReleaseRoot(t, map[string]string{"opt": "OPT"})
	// Missing required --pack/--out.
	if err := runReleaseManifest(root, []string{blobsDir, "--host", "linux-amd64"}); err == nil {
		t.Fatal("expected error when --pack and --out are missing")
	}
	// Wrong positional count (two blobsdirs).
	if err := runReleaseManifest(root, []string{blobsDir, blobsDir, "--pack", "p", "--out", "o"}); err == nil {
		t.Fatal("expected error for more than one positional")
	}
}

// TestBundleReleaseLLVMBadTarget covers the early llvmTargetEntry failure in the
// full-variant bundling step (the rest of bundleReleaseLLVM needs real blobs and
// is exercised by the release pipeline integration run).
func TestBundleReleaseLLVMBadTarget(t *testing.T) {
	root, blobsDir := fakeReleaseRoot(t, map[string]string{"opt": "OPT"})
	if err := bundleReleaseLLVM(root, "plan9-foo", blobsDir); err == nil {
		t.Fatal("expected error bundling for an unknown target")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
