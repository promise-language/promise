package blobstore

import (
	"archive/zip"
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// makeZip builds an in-memory zip archive with the given path→content members.
func makeZip(members map[string][]byte) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range members {
		w, _ := zw.Create(name)
		w.Write(content)
	}
	zw.Close()
	return buf.Bytes()
}

// TestResolveMemberWrapperDirTolerance verifies the resolver finds an
// archive_path beneath the single top-level wrapper dir that upstream LLVM
// tarballs use (e.g. LLVM-22/bin/opt for archive_path "bin/opt").
func TestResolveMemberWrapperDirTolerance(t *testing.T) {
	opt := []byte("opt-under-wrapper")
	archive := makeTar(map[string][]byte{"LLVM-22.1.0/bin/opt": opt})
	archiveHash := sha256hex(archive)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	}))
	defer srv.Close()

	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: sha256hex(opt), Size: int64(len(opt)), Kind: KindMachOLLVM,
		Sources: []Source{{Archive: srv.URL + "/llvm.tar", ArchivePath: "bin/opt", ArchiveSHA256: archiveHash}},
	})
	p, err := Resolve(store, m, "llvm-opt")
	if err != nil {
		t.Fatalf("resolve through wrapper dir: %v", err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, opt) {
		t.Fatal("wrong bytes extracted from wrapper-dir archive")
	}
}

// TestResolveMemberNotFound verifies a missing archive_path produces an error
// (not a silent success) so a broken manifest is surfaced.
func TestResolveMemberNotFound(t *testing.T) {
	archive := makeTar(map[string][]byte{"bin/llc": []byte("llc")})
	archiveHash := sha256hex(archive)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	}))
	defer srv.Close()

	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: sha256hex([]byte("opt")), Size: 3, Kind: KindBlob,
		Sources: []Source{{Archive: srv.URL + "/llvm.tar", ArchivePath: "bin/opt", ArchiveSHA256: archiveHash}},
	})
	_, err := Resolve(store, m, "llvm-opt")
	if err == nil {
		t.Fatal("expected error when archive member is absent")
	}
	if !strings.Contains(err.Error(), "promise doctor") {
		t.Fatalf("member-not-found should hard-error suggesting doctor: %v", err)
	}
}

// TestResolveArchiveSHA256Mismatch verifies an archive whose bytes don't match
// the asserted archive_sha256 is rejected before extraction (cheap defense,
// §4.3) and produces a hard error (not the offline message — it's a content,
// not a network, failure).
func TestResolveArchiveSHA256Mismatch(t *testing.T) {
	archive := makeTar(map[string][]byte{"bin/opt": []byte("opt")})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	}))
	defer srv.Close()

	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: sha256hex([]byte("opt")), Size: 3, Kind: KindBlob,
		// archive_sha256 deliberately wrong.
		Sources: []Source{{Archive: srv.URL + "/llvm.tar", ArchivePath: "bin/opt", ArchiveSHA256: sha256hex([]byte("WRONG ARCHIVE"))}},
	})
	_, err := Resolve(store, m, "llvm-opt")
	if err == nil {
		t.Fatal("expected archive sha256 mismatch error")
	}
	if err.Error() == OfflineError {
		t.Fatalf("archive content mismatch must not surface as offline error: %v", err)
	}
	if !strings.Contains(err.Error(), "promise doctor") {
		t.Fatalf("archive mismatch should hard-error suggesting doctor: %v", err)
	}
	// Nothing cached: neither the blob nor the (unverified) archive.
	if store.Has(sha256hex([]byte("opt"))) {
		t.Fatal("blob must not be cached after archive mismatch")
	}
	if entries, _ := os.ReadDir(store.archivesDir()); len(entries) != 0 {
		t.Fatal("mismatched archive must not be persistently cached")
	}
}

// TestPersistentArchiveCacheReuse verifies that when a blob is missing but its
// asserted archive is already in archives/sha256/<hash>, the resolver re-extracts
// from the cached archive without any network access (§4.2 archive reuse).
func TestPersistentArchiveCacheReuse(t *testing.T) {
	opt := []byte("opt-binary-reuse")
	archive := makeTar(map[string][]byte{"bin/opt": opt})
	archiveHash := sha256hex(archive)
	var downloads int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&downloads, 1)
		w.Write(archive)
	}))

	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: sha256hex(opt), Size: int64(len(opt)), Kind: KindBlob,
		Sources: []Source{{Archive: srv.URL + "/llvm.tar", ArchivePath: "bin/opt", ArchiveSHA256: archiveHash}},
	})

	// First pass: downloads the archive, caches both blob and archive.
	if _, err := Resolve(store, m, "llvm-opt"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if !store.HasArchive(archiveHash) {
		t.Fatal("archive should be persistently cached")
	}
	// Simulate GC removing the blob (but keeping the archive) + the network dying.
	if err := os.Remove(store.BlobPath(sha256hex(opt))); err != nil {
		t.Fatal(err)
	}
	srv.Close()

	// Second pass: blob is a miss but the archive is on disk → no network needed.
	p, err := Resolve(store, m, "llvm-opt")
	if err != nil {
		t.Fatalf("second resolve should reuse cached archive offline: %v", err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, opt) {
		t.Fatal("wrong bytes after archive-cache reuse")
	}
	if n := atomic.LoadInt32(&downloads); n != 1 {
		t.Fatalf("expected exactly 1 archive download, got %d", n)
	}
}

// TestResolveFromZipArchive verifies a blob can be extracted from a .zip source
// (format detected by magic bytes, not extension).
func TestResolveFromZipArchive(t *testing.T) {
	tool := []byte("zip-tool-bytes")
	archive := makeZip(map[string][]byte{"bin/lld.exe": tool})
	archiveHash := sha256hex(archive)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	}))
	defer srv.Close()

	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-lld.exe", SHA256: sha256hex(tool), Size: int64(len(tool)), Kind: KindBlob,
		Sources: []Source{{Archive: srv.URL + "/llvm.zip", ArchivePath: "bin/lld.exe", ArchiveSHA256: archiveHash}},
	})
	p, err := Resolve(store, m, "llvm-lld.exe")
	if err != nil {
		t.Fatalf("resolve from zip: %v", err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, tool) {
		t.Fatal("wrong bytes extracted from zip archive")
	}
}

// TestExtractZipRejectsEscapingPath verifies the zip extractor refuses members
// whose path escapes the destination dir (zip-slip defense).
func TestExtractZipRejectsEscapingPath(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("../escape.txt")
	w.Write([]byte("pwned"))
	zw.Close()

	dir := t.TempDir()
	archivePath := filepath.Join(dir, "evil.zip")
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isZip(archivePath) {
		t.Fatal("crafted file should be detected as zip")
	}
	dest := filepath.Join(dir, "out")
	err := extractZip(archivePath, dest)
	if err == nil || !strings.Contains(err.Error(), "escaping path") {
		t.Fatalf("expected escaping-path rejection, got: %v", err)
	}
}

// TestResolveHTTP404IsBrokenReleaseNotOffline verifies a non-200 response is
// treated as a broken-release/hard error (suggesting promise doctor), distinct
// from the offline error reserved for transport failures.
func TestResolveHTTP404IsBrokenReleaseNotOffline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: sha256hex([]byte("x")), Size: 1, Kind: KindBlob,
		Sources: []Source{{Blob: srv.URL + "/opt"}},
	})
	_, err := Resolve(store, m, "llvm-opt")
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if err.Error() == OfflineError {
		t.Fatalf("404 should not be the offline error: %v", err)
	}
	if !strings.Contains(err.Error(), "promise doctor") {
		t.Fatalf("404 should hard-error suggesting doctor: %v", err)
	}
}

// TestNegativeCacheAcrossPass verifies a source whose bytes mismatch is fetched
// only once within a resolver pass, even when two entries reference it.
func TestNegativeCacheAcrossPass(t *testing.T) {
	good := []byte("good")
	goodHash := sha256hex(good)
	var badHits int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&badHits, 1)
		w.Write([]byte("WRONG"))
	}))
	defer bad.Close()
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(good)
	}))
	defer goodSrv.Close()

	store := newTestStore(t)
	// Two distinct deps that both list the SAME bad source first, then good.
	m := mustManifest(t,
		ManifestEntry{Name: "a", SHA256: goodHash, Size: int64(len(good)), Kind: KindBlob,
			Sources: []Source{{Blob: bad.URL + "/x"}, {Blob: goodSrv.URL + "/x"}}},
		ManifestEntry{Name: "b", SHA256: goodHash, Size: int64(len(good)), Kind: KindBlob,
			Sources: []Source{{Blob: bad.URL + "/x"}, {Blob: goodSrv.URL + "/x"}}},
	)
	r := NewResolver(store, m)
	defer r.Close()
	if _, err := r.Resolve("a"); err != nil {
		t.Fatalf("resolve a: %v", err)
	}
	if _, err := r.Resolve("b"); err != nil {
		t.Fatalf("resolve b: %v", err)
	}
	// The bad source is negative-cached after the first failure, so the second
	// resolve skips it entirely.
	if n := atomic.LoadInt32(&badHits); n != 1 {
		t.Fatalf("expected bad source hit once (negative-cached), got %d", n)
	}
}

// TestLockContention verifies a second Lock blocks until the first is released
// and that the holder identity hint is recorded in the lock file.
func TestLockContention(t *testing.T) {
	store := newTestStore(t)
	unlock1, err := store.Lock("first-holder")
	if err != nil {
		t.Fatal(err)
	}
	// The holder hint is written so a waiter can name who it's blocked on.
	data, _ := os.ReadFile(filepath.Join(store.Root(), ".lock"))
	if !strings.Contains(string(data), "first-holder") {
		t.Fatalf("lock file should record holder, got %q", string(data))
	}

	got := make(chan struct{})
	go func() {
		unlock2, err := store.Lock("second-holder")
		if err == nil {
			unlock2()
		}
		close(got)
	}()

	// The second locker must NOT acquire while the first holds it.
	select {
	case <-got:
		t.Fatal("second Lock acquired while first still held")
	case <-time.After(150 * time.Millisecond):
	}

	unlock1()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("second Lock did not acquire after first released")
	}
}

// TestNetErrorWrap verifies the netError wrapper unwraps to its cause and that
// isNetworkError recognizes it (the offline-vs-broken-release decision hinge).
func TestNetErrorWrap(t *testing.T) {
	cause := errors.New("dial tcp: connection refused")
	ne := &netError{cause}
	if ne.Error() != cause.Error() {
		t.Fatalf("Error() = %q, want %q", ne.Error(), cause.Error())
	}
	if !errors.Is(ne, cause) {
		t.Fatal("netError should unwrap to its cause")
	}
	if !isNetworkError(ne) {
		t.Fatal("isNetworkError should recognize a netError")
	}
	if isNetworkError(cause) {
		t.Fatal("a plain error is not a network error")
	}
}

// TestValidateRejectsMissingNameAndArchivePath covers the remaining validate
// branches: an entry with no name, and an archive source without archive_path.
func TestValidateRejectsMissingNameAndArchivePath(t *testing.T) {
	cases := map[string]string{
		"missing name":    `{"schema":1,"entries":[{"sha256":"ff","sources":[{"blob":"u"}]}]}`,
		"archive no path": `{"schema":1,"entries":[{"name":"a","sha256":"ff","sources":[{"archive":"u"}]}]}`,
	}
	for name, js := range cases {
		if _, err := ParseManifest([]byte(js)); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

// TestNewStoreRootsUnderPromiseHome verifies NewStore roots the CAS at
// <PromiseHome>/cache.
func TestNewStoreRootsUnderPromiseHome(t *testing.T) {
	store, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if !strings.HasSuffix(filepath.Clean(store.Root()), filepath.Join("cache")) {
		t.Fatalf("store root should end in /cache, got %q", store.Root())
	}
}

// TestIntegrityTelemetryOptIn verifies the integrity-mismatch telemetry stub is
// silent by default and only emits when the explicit opt-in env var is set (no
// hidden effects, §4.4 design candidate).
func TestIntegrityTelemetryOptIn(t *testing.T) {
	// Default (env unset): the function must do nothing and not panic.
	reportIntegrityMismatch("llvm-opt", "https://x/opt", "aa", "bb", "2026.0")
	// Opt-in: exercises the emit branch.
	t.Setenv(integrityTelemetryEnv, "1")
	reportIntegrityMismatch("llvm-opt", "https://x/opt", "aa", "bb", "2026.0")
}

// TestStageBlobThenResolveIsCASHit verifies a staged (full-variant) blob is a
// CAS hit on resolution — no network — matching the install-staging acceptance
// criterion.
func TestStageBlobThenResolveIsCASHit(t *testing.T) {
	raw := []byte("pre-staged opt binary")
	store := newTestStore(t)
	hash, err := store.StageBlob(raw)
	if err != nil {
		t.Fatal(err)
	}
	// A dead server: any download attempt would fail, proving the CAS hit path.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: hash, Size: int64(len(raw)), Kind: KindBlob,
		Sources: []Source{{Blob: deadURL + "/opt"}},
	})
	p, err := Resolve(store, m, "llvm-opt")
	if err != nil {
		t.Fatalf("staged blob should resolve as CAS hit: %v", err)
	}
	if p != store.BlobPath(hash) {
		t.Fatalf("resolved path %q != CAS path", p)
	}
}
