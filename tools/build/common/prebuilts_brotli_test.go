package common

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestBundleBrotliFromManifest verifies the full-variant bundler stages the
// already-brotli <sha>.br blob byte-identically into the embed dir as <out>.br,
// rather than gzip-recompressing (T0807).
func TestBundleBrotliFromManifest(t *testing.T) {
	root := t.TempDir()
	blobsDir := filepath.Join(root, "blobs")
	dstDir := filepath.Join(root, "embed")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// The fetched .br (named <sha>.br) sitting in the keep-compressed blobs dir.
	raw := []byte("the raw opt binary bytes")
	br := brotliBytes(t, raw)
	sha := sha256Hex(raw)
	brName := sha + ".br"
	if err := os.WriteFile(filepath.Join(blobsDir, brName), br, 0o644); err != nil {
		t.Fatal(err)
	}

	mkManifest := func(compression string) string {
		m := runtimeManifest{Schema: runtimeManifestSchema, Epoch: "2026.0", Entries: []runtimeManifestEntry{{
			Name:   "llvm-opt",
			SHA256: sha,
			Size:   int64(len(raw)),
			Kind:   "blob",
			Sources: []runtimeSource{{
				Blob:        "https://example.test/deps-llvm-22.1.0/" + brName,
				Compression: compression,
			}},
		}}}
		path := filepath.Join(root, "manifest-"+compression+".json")
		if err := writeRuntimeManifest(path, &m); err != nil {
			t.Fatal(err)
		}
		return path
	}

	files := []PrebuiltFile{{Out: "opt"}}

	// Happy path: brotli entry → byte-identical <out>.br staged.
	if err := BundleBrotliFromManifest(mkManifest(compressionBrotli), blobsDir, dstDir, files); err != nil {
		t.Fatalf("BundleBrotliFromManifest: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dstDir, "opt.br"))
	if err != nil {
		t.Fatalf("read staged opt.br: %v", err)
	}
	if !bytes.Equal(got, br) {
		t.Fatal("staged opt.br must be byte-identical to the source <sha>.br")
	}

	// Negative: a non-brotli entry is a hard error, not a silent fallback.
	err = BundleBrotliFromManifest(mkManifest("none"), blobsDir, dstDir, files)
	if err == nil {
		t.Fatal("expected error on a non-brotli compression codec")
	}
	if !containsAll(err.Error(), "llvm-opt", "brotli") {
		t.Fatalf("error should name the entry and expected codec, got: %v", err)
	}
}

// TestBundleBrotliFromManifestErrors covers the bundler's error paths: a missing
// manifest, a file with no matching manifest entry, an entry with no blob
// source, and a missing source .br on disk (T0807).
func TestBundleBrotliFromManifestErrors(t *testing.T) {
	root := t.TempDir()
	blobsDir := filepath.Join(root, "blobs")
	dstDir := filepath.Join(root, "embed")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	raw := []byte("the raw opt binary bytes")
	sha := sha256Hex(raw)
	brName := sha + ".br"

	writeManifest := func(name string, entries []runtimeManifestEntry) string {
		m := runtimeManifest{Schema: runtimeManifestSchema, Epoch: "2026.0", Entries: entries}
		p := filepath.Join(root, name)
		if err := writeRuntimeManifest(p, &m); err != nil {
			t.Fatal(err)
		}
		return p
	}

	files := []PrebuiltFile{{Out: "opt"}}

	// 1 — missing manifest file → load error.
	err := BundleBrotliFromManifest(filepath.Join(root, "does-not-exist.json"), blobsDir, dstDir, files)
	if err == nil || !containsAll(err.Error(), "load manifest") {
		t.Fatalf("expected load-manifest error, got: %v", err)
	}

	// 2 — manifest has no entry for the requested out name.
	mp := writeManifest("no-entry.json", []runtimeManifestEntry{{
		Name: "llvm-llc", SHA256: sha, Kind: "blob",
		Sources: []runtimeSource{{Blob: "https://x/" + brName, Compression: compressionBrotli}},
	}})
	err = BundleBrotliFromManifest(mp, blobsDir, dstDir, files)
	if err == nil || !containsAll(err.Error(), "no manifest entry", "opt") {
		t.Fatalf("expected no-manifest-entry error, got: %v", err)
	}

	// 3 — matching entry but no blob source (e.g. archive-only).
	mp = writeManifest("no-blob.json", []runtimeManifestEntry{{
		Name: "llvm-opt", SHA256: sha, Kind: "blob",
		Sources: []runtimeSource{{Archive: "https://x/tools.tar.xz", ArchivePath: "bin/opt"}},
	}})
	err = BundleBrotliFromManifest(mp, blobsDir, dstDir, files)
	if err == nil || !containsAll(err.Error(), "no blob source", "llvm-opt") {
		t.Fatalf("expected no-blob-source error, got: %v", err)
	}

	// 4 — valid brotli entry but the source .br is absent on disk → copy error.
	mp = writeManifest("missing-src.json", []runtimeManifestEntry{{
		Name: "llvm-opt", SHA256: sha, Kind: "blob",
		Sources: []runtimeSource{{Blob: "https://x/" + brName, Compression: compressionBrotli}},
	}})
	err = BundleBrotliFromManifest(mp, blobsDir, dstDir, files)
	if err == nil || !containsAll(err.Error(), "copy", "opt") {
		t.Fatalf("expected copy error for missing source, got: %v", err)
	}
}
