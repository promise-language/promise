package blobstore

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
)

// brotliBytes returns the brotli-compressed (level 11) encoding of b — the
// on-the-wire form of a compressed blob asset.
func brotliBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	bw := brotli.NewWriterLevel(&buf, brotli.BestCompression)
	if _, err := bw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestResolveBrotliBlob serves a brotli-compressed blob and asserts the resolver
// decompresses it, the CAS file holds the UNCOMPRESSED content, and its sha256
// matches the manifest entry (verification is over the decompressed bytes).
func TestResolveBrotliBlob(t *testing.T) {
	content := []byte("the opt binary — compressed in transit, verified decompressed")
	hash := sha256hex(content)
	compressed := brotliBytes(t, content)
	if len(compressed) == 0 {
		t.Fatal("compressed payload is empty")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(compressed)
	}))
	defer srv.Close()

	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: hash, Size: int64(len(content)), Kind: KindBlob,
		Sources: []Source{{Blob: srv.URL + "/opt.br", Compression: compressionBrotli}},
	})

	p, err := Resolve(store, m, "llvm-opt")
	if err != nil {
		t.Fatalf("resolve brotli blob: %v", err)
	}
	got, _ := os.ReadFile(p)
	if !bytes.Equal(got, content) {
		t.Fatal("CAS file is not the decompressed content")
	}
	if p != store.BlobPath(hash) {
		t.Fatalf("blob not at CAS path keyed by uncompressed hash: %s", p)
	}
}

// TestResolveBrotliFallsThroughToArchive proves a compressed blob source and an
// uncompressed archive fallback coexist: a failing .br source is skipped and the
// archive serves the same uncompressed content address.
func TestResolveBrotliFallsThroughToArchive(t *testing.T) {
	opt := []byte("opt-binary-via-archive")
	archive := makeTar(map[string][]byte{"bin/opt": opt})
	archiveHash := sha256hex(archive)
	badBlob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not brotli at all"))
	}))
	defer badBlob.Close()
	arcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	}))
	defer arcSrv.Close()

	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: sha256hex(opt), Size: int64(len(opt)), Kind: KindBlob,
		Sources: []Source{
			{Blob: badBlob.URL + "/opt.br", Compression: compressionBrotli},
			{Archive: arcSrv.URL + "/llvm.tar", ArchivePath: "bin/opt", ArchiveSHA256: archiveHash},
		},
	})
	p, err := Resolve(store, m, "llvm-opt")
	if err != nil {
		t.Fatalf("resolve should fall through to archive: %v", err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, opt) {
		t.Fatal("did not get archive bytes after brotli source failed")
	}
}

// TestResolveArchiveDeclinedSurfacesSentinel: when the archive-confirm hook
// declines the large fallback, fetch() returns errArchiveDeclined verbatim (not
// the "promise doctor" broken-release message) and the archive host is never
// contacted.
func TestResolveArchiveDeclinedSurfacesSentinel(t *testing.T) {
	opt := []byte("opt-binary-via-archive")
	archive := makeTar(map[string][]byte{"bin/opt": opt})
	archiveHash := sha256hex(archive)
	badBlob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer badBlob.Close()
	archiveHit := false
	arcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		archiveHit = true
		w.Write(archive)
	}))
	defer arcSrv.Close()

	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: sha256hex(opt), Size: int64(len(opt)), Kind: KindBlob,
		Sources: []Source{
			{Blob: badBlob.URL + "/opt.br", Compression: compressionBrotli},
			{Archive: arcSrv.URL + "/llvm.tar", ArchivePath: "bin/opt", ArchiveSHA256: archiveHash},
		},
	})

	r := NewResolver(store, m)
	defer r.Close()
	var prompted string
	r.SetArchiveConfirm(func(name string) bool { prompted = name; return false })

	_, err := r.Resolve("llvm-opt")
	if err == nil {
		t.Fatal("expected an error after declining the archive fallback")
	}
	if !errors.Is(err, errArchiveDeclined) {
		t.Fatalf("decline should surface errArchiveDeclined, got: %v", err)
	}
	if strings.Contains(err.Error(), "promise doctor") {
		t.Fatalf("declined fallback must not show the broken-release message: %v", err)
	}
	if prompted != "llvm.tar" {
		t.Errorf("archive-confirm should receive the tarball basename, got %q", prompted)
	}
	if archiveHit {
		t.Error("declining must not download the archive")
	}
	if store.Has(sha256hex(opt)) {
		t.Error("no CAS entry should be committed on decline")
	}
}

// TestResolveBrotliDecompressionBombAbort feeds a small .br that expands well past
// the manifest size; the bounded decompression (entry.Size+1) must reject it
// rather than materialize an oversize blob (§4.3 overshoot defense over the
// decompressed stream).
func TestResolveBrotliDecompressionBombAbort(t *testing.T) {
	big := bytes.Repeat([]byte("A"), 1<<16) // 64 KiB of highly compressible data
	compressed := brotliBytes(t, big)
	if len(compressed) >= len(big) {
		t.Fatal("expected the payload to compress")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(compressed)
	}))
	defer srv.Close()

	store := newTestStore(t)
	// Manifest claims a tiny size — the decompressed stream blows past it.
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: sha256hex(big), Size: 16, Kind: KindBlob,
		Sources: []Source{{Blob: srv.URL + "/opt.br", Compression: compressionBrotli}},
	})
	_, err := Resolve(store, m, "llvm-opt")
	if err == nil {
		t.Fatal("expected decompression-bomb abort")
	}
	if store.Has(sha256hex(big)) {
		t.Fatal("no CAS entry should be committed for an aborted bomb fetch")
	}
}

// TestResolveBrotliDownloadFailureIsHardError covers the pre-decompress failure
// path in fetchBlobBrotli: when the compressed asset itself can't be fetched
// (404 / broken release), the error must propagate as a hard broken-release
// error (promise doctor), distinct from the offline error, with no CAS entry.
func TestResolveBrotliDownloadFailureIsHardError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	content := []byte("never materialized")
	hash := sha256hex(content)
	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: hash, Size: int64(len(content)), Kind: KindBlob,
		Sources: []Source{{Blob: srv.URL + "/opt.br", Compression: compressionBrotli}},
	})
	_, err := Resolve(store, m, "llvm-opt")
	if err == nil {
		t.Fatal("expected error when the compressed asset 404s")
	}
	if err.Error() == OfflineError {
		t.Fatalf("a 404 on the .br asset is a broken release, not offline: %v", err)
	}
	if !strings.Contains(err.Error(), "promise doctor") {
		t.Fatalf("download failure should hard-error suggesting doctor: %v", err)
	}
	if store.Has(hash) {
		t.Fatal("no CAS entry should be committed when the download failed")
	}
}

// TestParseManifestRejectsCompression covers validate()'s codec rules: compression
// is only valid on a blob source, and the codec value must be known.
func TestParseManifestRejectsCompression(t *testing.T) {
	cases := map[string]string{
		"compression on archive": `{"schema":1,"entries":[{"name":"a","sha256":"ff","sources":[{"archive":"u","archive_path":"bin/x","compression":"brotli"}]}]}`,
		"unknown codec":          `{"schema":1,"entries":[{"name":"a","sha256":"ff","sources":[{"blob":"u","compression":"lz4"}]}]}`,
	}
	for name, js := range cases {
		if _, err := ParseManifest([]byte(js)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// TestCompressionRoundTrips asserts the compression field survives manifest JSON
// marshal/unmarshal (the lockstep with the producer's runtimeSource).
func TestCompressionRoundTrips(t *testing.T) {
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: sha256hex([]byte("x")), Size: 1, Kind: KindBlob,
		Sources: []Source{{Blob: "https://example/opt.br", Compression: compressionBrotli}},
	})
	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"compression": "brotli"`) {
		t.Fatalf("compression not emitted in JSON:\n%s", data)
	}
	got, err := ParseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := got.Lookup("llvm-opt")
	if e.Sources[0].Compression != compressionBrotli {
		t.Fatalf("compression lost in round-trip: %q", e.Sources[0].Compression)
	}
}
