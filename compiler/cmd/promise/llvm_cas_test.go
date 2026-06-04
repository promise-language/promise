package main

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/promise-language/promise/compiler/internal/blobstore"
)

// TestBlobSetKeyOrderIndependentAndContentSensitive verifies the view-dir key is
// stable regardless of entry order but changes when any blob hash changes (so an
// LLVM version bump yields a fresh view dir rather than serving stale tools).
func TestBlobSetKeyOrderIndependentAndContentSensitive(t *testing.T) {
	a := &blobstore.ManifestEntry{Name: "llvm-opt", SHA256: "AA"}
	b := &blobstore.ManifestEntry{Name: "llvm-llc", SHA256: "bb"}
	k1 := blobSetKey([]*blobstore.ManifestEntry{a, b})
	k2 := blobSetKey([]*blobstore.ManifestEntry{b, a})
	if k1 != k2 {
		t.Fatalf("blobSetKey must be order-independent: %q != %q", k1, k2)
	}
	// Case/whitespace in the hash is normalized (so "AA" == "aa").
	aLower := &blobstore.ManifestEntry{Name: "llvm-opt", SHA256: " aa "}
	if blobSetKey([]*blobstore.ManifestEntry{aLower, b}) != k1 {
		t.Fatal("blobSetKey should normalize hash case/whitespace")
	}
	// A changed blob hash → different key.
	bChanged := &blobstore.ManifestEntry{Name: "llvm-llc", SHA256: "cc"}
	if blobSetKey([]*blobstore.ManifestEntry{a, bChanged}) == k1 {
		t.Fatal("blobSetKey must change when a blob hash changes")
	}
	if len(k1) != 16 {
		t.Fatalf("blobSetKey should be 16 hex chars, got %d", len(k1))
	}
}

// TestGunzipBytesRoundTrip verifies gunzipBytes decompresses what gzip produces
// and rejects non-gzip input.
func TestGunzipBytesRoundTrip(t *testing.T) {
	want := []byte("the raw opt binary bytes")
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	gw.Write(want)
	gw.Close()

	got, err := gunzipBytes(buf.Bytes())
	if err != nil {
		t.Fatalf("gunzipBytes: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("round-trip mismatch")
	}
	if _, err := gunzipBytes([]byte("not gzip")); err == nil {
		t.Fatal("expected error on non-gzip input")
	}
}

// TestViewComplete verifies the view-dir completeness check: it requires every
// LLVM blob file and, when lld is present, the lld-mode aliases.
func TestViewComplete(t *testing.T) {
	dir := t.TempDir()
	entries := []*blobstore.ManifestEntry{
		{Name: "llvm-opt", SHA256: "aa"},
		{Name: "llvm-lld", SHA256: "bb"},
	}
	// Empty dir → incomplete.
	if viewComplete(dir, entries) {
		t.Fatal("empty view dir should be incomplete")
	}
	// Materialize the two blobs but NOT the lld aliases → still incomplete.
	for _, name := range []string{"opt", "lld"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if viewComplete(dir, entries) {
		t.Fatal("view without lld aliases should be incomplete")
	}
	// Add all lld-mode aliases → complete.
	for link := range embeddedLLVMSymlinks {
		name := link
		if runtime.GOOS == "windows" {
			name = link + ".exe"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if !viewComplete(dir, entries) {
		t.Fatal("view with all blobs + aliases should be complete")
	}
}

// TestViewCompleteNoLLD verifies a view without an lld entry is complete once its
// (non-lld) blobs exist — no aliases required.
func TestViewCompleteNoLLD(t *testing.T) {
	dir := t.TempDir()
	entries := []*blobstore.ManifestEntry{{Name: "llvm-opt", SHA256: "aa"}}
	if viewComplete(dir, entries) {
		t.Fatal("missing blob should be incomplete")
	}
	if err := os.WriteFile(filepath.Join(dir, "opt"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !viewComplete(dir, entries) {
		t.Fatal("single non-lld blob present should be complete")
	}
}
