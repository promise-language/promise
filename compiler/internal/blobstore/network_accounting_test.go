package blobstore

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/promise-language/promise/compiler/internal/casmetrics"
)

// What the resolver pulls over the wire is accounted for, always (T2143). A
// build that starts downloading on a path that should have been fully local is
// otherwise invisible until it presents as a CI timeout — the reverse direction
// of the same blindness that let T2133 ride trunk for eighteen days.
//
// These read the process-wide ledger and so are deliberately NOT parallel: they
// measure a delta across one download, and a peer test fetching at the same
// moment would be counted into it.

// networkDelta runs fetch and reports the wire bytes the ledger gained.
func networkDelta(t *testing.T, fetch func()) int64 {
	t.Helper()
	before := casmetrics.Read()
	if !before.Available {
		t.Skip("no writable ledger beside this test binary")
	}
	fetch()
	return casmetrics.Read().NetworkBytes - before.NetworkBytes
}

// TestNetworkBytesCountsWhatWasTransferred: the count is the bytes that moved,
// not what the manifest says the entry weighs. The two agree here; they do not
// on a compressed source, which the next test pins.
func TestNetworkBytesCountsWhatWasTransferred(t *testing.T) {
	content := []byte("the opt binary, fetched over the wire")
	hash := sha256hex(content)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: hash, Size: int64(len(content)), Kind: KindBlob,
		Sources: []Source{{Blob: srv.URL + "/opt"}},
	})

	got := networkDelta(t, func() {
		if _, err := Resolve(store, m, "llvm-opt"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	})
	if got != int64(len(content)) {
		t.Errorf("cas_network_bytes gained %d, want %d", got, len(content))
	}
}

// TestNetworkBytesCountsTheCompressedTransfer: a brotli source moves the
// compressed bytes, and those are what the link cost. Counting the decompressed
// size would report bytes that never crossed the network.
func TestNetworkBytesCountsTheCompressedTransfer(t *testing.T) {
	raw := []byte("the opt binary, but compressed in flight and much more compressible than it looks")
	compressed := brotliBytes(t, raw)
	hash := sha256hex(raw)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(compressed)
	}))
	defer srv.Close()

	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: hash, Size: int64(len(raw)), Kind: KindBlob,
		Sources: []Source{{Blob: srv.URL + "/opt.br", Compression: compressionBrotli}},
	})

	got := networkDelta(t, func() {
		if _, err := Resolve(store, m, "llvm-opt"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	})
	if got != int64(len(compressed)) {
		t.Errorf("cas_network_bytes gained %d, want the compressed %d (raw is %d)",
			got, len(compressed), len(raw))
	}
}

// TestNetworkBytesZeroOnACASHit: the whole point is telling a warm run from a
// cold one, so a blob already in the store must cost nothing. A metric that
// counted manifest sizes would report the same number either way.
func TestNetworkBytesZeroOnACASHit(t *testing.T) {
	content := []byte("already staged")
	hash := sha256hex(content)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a CAS hit reached the network")
	}))
	defer srv.Close()

	store := newTestStore(t)
	if _, err := store.StageBlob(content); err != nil {
		t.Fatalf("stage: %v", err)
	}
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: hash, Size: int64(len(content)), Kind: KindBlob,
		Sources: []Source{{Blob: srv.URL + "/opt"}},
	})

	got := networkDelta(t, func() {
		if _, err := Resolve(store, m, "llvm-opt"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	})
	if got != 0 {
		t.Errorf("a CAS hit reported %d wire bytes, want 0", got)
	}
}

// TestNetworkBytesCountsAFailedTransfer: bytes spent on a blob that turns out
// to be the wrong content were spent all the same. Charging only successful
// fetches would understate exactly the runs that hurt most — a broken mirror
// served repeatedly.
func TestNetworkBytesCountsAFailedTransfer(t *testing.T) {
	wrong := []byte("not what the manifest asked for")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(wrong)
	}))
	defer srv.Close()

	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: sha256hex([]byte("the real thing")), Size: int64(len(wrong)), Kind: KindBlob,
		Sources: []Source{{Blob: srv.URL + "/opt"}},
	})

	got := networkDelta(t, func() {
		if _, err := Resolve(store, m, "llvm-opt"); err == nil {
			t.Fatal("expected the hash mismatch to fail the resolve")
		}
	})
	if got != int64(len(wrong)) {
		t.Errorf("a failed transfer reported %d wire bytes, want %d", got, len(wrong))
	}
}
