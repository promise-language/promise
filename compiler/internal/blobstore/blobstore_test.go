package blobstore

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// newTestStore returns a Store rooted at a temp dir.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStoreAt(filepath.Join(t.TempDir(), "cache"))
}

func mustManifest(t *testing.T, entries ...ManifestEntry) *Manifest {
	t.Helper()
	m := &Manifest{Schema: ManifestSchema, Epoch: "2026.0", Entries: entries}
	if err := m.validate(); err != nil {
		t.Fatalf("manifest invalid: %v", err)
	}
	return m
}

func TestParseManifestRoundTrip(t *testing.T) {
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: sha256hex([]byte("x")), Size: 1, Kind: KindBlob,
		Sources: []Source{{Blob: "https://example/opt"}},
	})
	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Lookup("llvm-opt"); !ok {
		t.Fatal("lookup failed after round-trip")
	}
	if _, ok := got.Lookup("missing"); ok {
		t.Fatal("unexpected lookup hit")
	}
}

func TestParseManifestRejectsBad(t *testing.T) {
	cases := map[string]string{
		"bad schema":   `{"schema":99,"entries":[]}`,
		"no sources":   `{"schema":1,"entries":[{"name":"a","sha256":"ff"}]}`,
		"no sha":       `{"schema":1,"entries":[{"name":"a","sources":[{"blob":"u"}]}]}`,
		"empty source": `{"schema":1,"entries":[{"name":"a","sha256":"ff","sources":[{}]}]}`,
		"dup name":     `{"schema":1,"entries":[{"name":"a","sha256":"ff","sources":[{"blob":"u"}]},{"name":"a","sha256":"ee","sources":[{"blob":"v"}]}]}`,
	}
	for name, js := range cases {
		if _, err := ParseManifest([]byte(js)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestResolveBlobCASHit(t *testing.T) {
	content := []byte("the opt binary")
	hash := sha256hex(content)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write(content)
	}))
	defer srv.Close()

	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: hash, Size: int64(len(content)), Kind: KindBlob,
		Sources: []Source{{Blob: srv.URL + "/opt"}},
	})

	p, err := Resolve(store, m, "llvm-opt")
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	got, _ := os.ReadFile(p)
	if !bytes.Equal(got, content) {
		t.Fatal("blob content mismatch")
	}
	if p != store.BlobPath(hash) {
		t.Fatalf("blob not at CAS path: %s", p)
	}

	// Second resolve is a CAS hit — no re-download.
	if _, err := Resolve(store, m, "llvm-opt"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("expected 1 download, got %d", n)
	}
}

func TestResolveMismatchFallsThrough(t *testing.T) {
	good := []byte("good bytes")
	goodHash := sha256hex(good)
	var badHits, goodHits int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&badHits, 1)
		w.Write([]byte("WRONG"))
	}))
	defer bad.Close()
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&goodHits, 1)
		w.Write(good)
	}))
	defer goodSrv.Close()

	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: goodHash, Size: int64(len(good)), Kind: KindBlob,
		Sources: []Source{{Blob: bad.URL + "/opt"}, {Blob: goodSrv.URL + "/opt"}},
	})

	p, err := Resolve(store, m, "llvm-opt")
	if err != nil {
		t.Fatalf("resolve should fall through to good source: %v", err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, good) {
		t.Fatal("did not get good bytes")
	}
	if badHits != 1 || goodHits != 1 {
		t.Fatalf("expected one hit each, got bad=%d good=%d", badHits, goodHits)
	}
}

// TestResolveStallFallsThrough covers the download stall watchdog: a source that
// sends headers + a partial body then wedges (holds the connection open with no
// further bytes) must not hang the build. The watchdog cancels it and fetch()
// falls through to the next ranked source. Regression for the untimed http.Get
// that blocked the whole test suite on a slow/half-open connection over a VPN.
func TestResolveStallFallsThrough(t *testing.T) {
	prev := downloadStallTimeout
	downloadStallTimeout = 150 * time.Millisecond
	defer func() { downloadStallTimeout = prev }()

	release := make(chan struct{})
	var stallHits, goodHits int32
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&stallHits, 1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("x")) // partial body, then wedge until the test ends
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	// Order matters: close(release) must run before stall.Close() (which waits for
	// the in-flight handler), else the handler — blocked on <-release — deadlocks
	// Close. Deferred LIFO runs close(release) first.
	defer stall.Close()
	defer close(release)

	good := []byte("good bytes")
	goodHash := sha256hex(good)
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&goodHits, 1)
		w.Write(good)
	}))
	defer goodSrv.Close()

	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: goodHash, Size: int64(len(good)), Kind: KindBlob,
		Sources: []Source{{Blob: stall.URL + "/opt"}, {Blob: goodSrv.URL + "/opt"}},
	})

	p, err := Resolve(store, m, "llvm-opt")
	if err != nil {
		t.Fatalf("resolve should fall through past the stalled source: %v", err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, good) {
		t.Fatal("did not get good bytes after stall fallthrough")
	}
	if stallHits == 0 || goodHits == 0 {
		t.Fatalf("expected both sources hit, got stall=%d good=%d", stallHits, goodHits)
	}
}

// TestResolveHeaderStallFallsThrough covers the stall watchdog firing during the
// response-header wait (before any body) — a source that accepts the connection
// but never sends a response. This exercises the Do()-error stall branch (the
// HTTP/2 case where ResponseHeaderTimeout does not apply). fetch() must fall
// through to the next source rather than hang.
func TestResolveHeaderStallFallsThrough(t *testing.T) {
	prev := downloadStallTimeout
	downloadStallTimeout = 150 * time.Millisecond
	defer func() { downloadStallTimeout = prev }()

	release := make(chan struct{})
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // never write a response until the test ends
	}))
	defer stall.Close()
	defer close(release)

	good := []byte("good bytes")
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(good)
	}))
	defer goodSrv.Close()

	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: sha256hex(good), Size: int64(len(good)), Kind: KindBlob,
		Sources: []Source{{Blob: stall.URL + "/opt"}, {Blob: goodSrv.URL + "/opt"}},
	})

	p, err := Resolve(store, m, "llvm-opt")
	if err != nil {
		t.Fatalf("resolve should fall through past the header-stalled source: %v", err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, good) {
		t.Fatal("did not get good bytes after header-stall fallthrough")
	}
}

func TestResolveAllSourcesFailHardError(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("WRONG"))
	}))
	defer bad.Close()

	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: sha256hex([]byte("expected")), Size: 8, Kind: KindBlob,
		Sources: []Source{{Blob: bad.URL + "/opt"}},
	})
	_, err := Resolve(store, m, "llvm-opt")
	if err == nil {
		t.Fatal("expected hard error")
	}
	if !strings.Contains(err.Error(), "promise doctor") || !strings.Contains(err.Error(), "llvm-opt") {
		t.Fatalf("hard error should name dep + suggest doctor: %v", err)
	}
}

func TestResolveSizeOvershootAbort(t *testing.T) {
	content := []byte("way more bytes than the manifest claims")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	store := newTestStore(t)
	// Manifest claims size 4 but server returns far more → abort before hashing.
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: sha256hex(content), Size: 4, Kind: KindBlob,
		Sources: []Source{{Blob: srv.URL + "/opt"}},
	})
	_, err := Resolve(store, m, "llvm-opt")
	if err == nil {
		t.Fatal("expected overshoot abort")
	}
}

func TestResolveOfflineError(t *testing.T) {
	store := newTestStore(t)
	// A server we immediately close → connection refused (fast, deterministic).
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: sha256hex([]byte("x")), Size: 1, Kind: KindBlob,
		Sources: []Source{{Blob: deadURL + "/opt"}},
	})
	_, err := Resolve(store, m, "llvm-opt")
	if err == nil || err.Error() != OfflineError {
		t.Fatalf("expected exact offline error, got: %v", err)
	}
}

// makeTar builds an in-memory tar archive with the given path→content members.
func makeTar(members map[string][]byte) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range members {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))})
		tw.Write(content)
	}
	tw.Close()
	return buf.Bytes()
}

func TestResolveArchiveCoalescing(t *testing.T) {
	opt := []byte("opt-binary")
	llc := []byte("llc-binary")
	lld := []byte("lld-binary")
	archive := makeTar(map[string][]byte{"bin/opt": opt, "bin/llc": llc, "bin/lld": lld})
	archiveHash := sha256hex(archive)

	var downloads int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&downloads, 1)
		w.Write(archive)
	}))
	defer srv.Close()

	store := newTestStore(t)
	mkEntry := func(name, path string, content []byte) ManifestEntry {
		return ManifestEntry{
			Name: name, SHA256: sha256hex(content), Size: int64(len(content)), Kind: KindMachOLLVM,
			Sources: []Source{{Archive: srv.URL + "/llvm.tar", ArchivePath: path, ArchiveSHA256: archiveHash}},
		}
	}
	m := mustManifest(t,
		mkEntry("llvm-opt", "bin/opt", opt),
		mkEntry("llvm-llc", "bin/llc", llc),
		mkEntry("llvm-lld", "bin/lld", lld),
	)

	r := NewResolver(store, m)
	defer r.Close()
	for _, name := range []string{"llvm-opt", "llvm-llc", "llvm-lld"} {
		p, err := r.Resolve(name)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("blob %s missing: %v", name, err)
		}
	}
	// One download served all three blobs (coalescing + materialize-all).
	if n := atomic.LoadInt32(&downloads); n != 1 {
		t.Fatalf("expected 1 archive download, got %d", n)
	}
	// Archive persistently cached (archive_sha256 present).
	if !store.HasArchive(archiveHash) {
		t.Fatal("archive should be persistently cached when archive_sha256 given")
	}
}

func TestResolveArchiveNotCachedWithoutHash(t *testing.T) {
	opt := []byte("opt-binary-2")
	archive := makeTar(map[string][]byte{"bin/opt": opt})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	}))
	defer srv.Close()

	store := newTestStore(t)
	m := mustManifest(t, ManifestEntry{
		Name: "llvm-opt", SHA256: sha256hex(opt), Size: int64(len(opt)), Kind: KindBlob,
		Sources: []Source{{Archive: srv.URL + "/a.tar", ArchivePath: "bin/opt"}}, // no archive_sha256
	})
	if _, err := Resolve(store, m, "llvm-opt"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(store.archivesDir())
	if len(entries) != 0 {
		t.Fatalf("archive should NOT be persistently cached without archive_sha256, found %d", len(entries))
	}
}

func TestMirrorRewrite(t *testing.T) {
	t.Setenv("PROMISE_BLOB_MIRROR", "https://mirror.corp/promise")
	got := rewriteSource("https://github.com/promise-language/promise/releases/x.tar")
	want := "https://mirror.corp/promise/promise-language/promise/releases/x.tar"
	if got != want {
		t.Fatalf("rewrite = %q, want %q", got, want)
	}
}

// TestMirrorRewriteBlob pins the FLAT CAS mirror for blob sources: a
// content-addressed blob's GitHub URL maps to <mirror>/<sha>.br (basename only),
// not the GitHub /owner/repo/releases/download/<tag>/ path. The mirror bucket is
// a flat sha-keyed namespace.
func TestMirrorRewriteBlob(t *testing.T) {
	t.Setenv("PROMISE_BLOB_MIRROR", "https://prebuilts.example.org")
	got := rewriteBlobSource("https://github.com/promise-language/promise/releases/download/deps-llvm-22.1.0/abc123def456.br")
	want := "https://prebuilts.example.org/abc123def456.br"
	if got != want {
		t.Fatalf("rewriteBlobSource = %q, want %q", got, want)
	}
}

func TestWriteEpochRefs(t *testing.T) {
	archiveHash := sha256hex([]byte("arc"))
	m := mustManifest(t,
		ManifestEntry{Name: "a", SHA256: sha256hex([]byte("a")), Size: 1, Kind: KindBlob,
			Sources: []Source{{Archive: "u", ArchivePath: "p", ArchiveSHA256: archiveHash}}},
		ManifestEntry{Name: "b", SHA256: sha256hex([]byte("b")), Size: 1, Kind: KindBlob,
			Sources: []Source{{Blob: "v"}}},
	)
	dir := t.TempDir()
	if err := WriteEpochRefs(dir, m); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "blobs.refs"))
	s := string(data)
	for _, want := range []string{
		"blob " + sha256hex([]byte("a")),
		"blob " + sha256hex([]byte("b")),
		"archive " + archiveHash,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("blobs.refs missing %q\n%s", want, s)
		}
	}
}

func TestStageBlobAtomicAndIdempotent(t *testing.T) {
	store := newTestStore(t)
	data := []byte("bundled blob")
	hash, err := store.StageBlob(data)
	if err != nil {
		t.Fatal(err)
	}
	if hash != sha256hex(data) {
		t.Fatalf("StageBlob hash = %s", hash)
	}
	if !store.Has(hash) {
		t.Fatal("blob not present after staging")
	}
	// Idempotent.
	if _, err := store.StageBlob(data); err != nil {
		t.Fatalf("re-stage: %v", err)
	}
	// No leftover temp files in the blobs dir.
	entries, _ := os.ReadDir(store.blobsDir())
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

// TestNoHalfWrittenEntryOnMismatch verifies an interrupted/failed fetch leaves
// no half-written CAS entry that would look valid by presence.
func TestNoHalfWrittenEntryOnMismatch(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("corrupt"))
	}))
	defer bad.Close()
	store := newTestStore(t)
	expected := sha256hex([]byte("real bytes"))
	m := mustManifest(t, ManifestEntry{
		Name: "x", SHA256: expected, Size: 10, Kind: KindBlob,
		Sources: []Source{{Blob: bad.URL + "/x"}},
	})
	_, _ = Resolve(store, m, "x")
	if store.Has(expected) {
		t.Fatal("CAS should have no entry after a mismatched fetch")
	}
	if _, err := os.Stat(store.BlobPath(expected)); err == nil {
		t.Fatal("no blob file should exist for a failed fetch")
	}
}
