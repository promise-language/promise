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
// whose path escapes the destination dir (zip-slip defense). Covers the three
// rejection branches: parent-relative (`..`), deep parent escape, and a
// unix-absolute entry. The last is the T0820 case: "/etc/passwd" is not
// filepath.IsAbs on Windows (no drive letter) yet Clean yields a rooted
// "\etc\passwd", so the os.IsPathSeparator(name[0]) guard catches it.
func TestExtractZipRejectsEscapingPath(t *testing.T) {
	cases := []struct {
		name      string
		entryName string
	}{
		{"parent escape", "../escape.txt"},
		{"deep parent escape", "sub/../../../../etc/passwd"},
		{"absolute unix", "/etc/passwd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			w, _ := zw.Create(tc.entryName)
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
				t.Fatalf("expected escaping-path rejection for %q, got: %v", tc.entryName, err)
			}
			// Confirm the payload was not written outside dest.
			outside := filepath.Join(filepath.Dir(dest), "etc", "passwd")
			if _, statErr := os.Stat(outside); statErr == nil {
				t.Errorf("file written outside dest at %s — escape!", outside)
			}
		})
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
	// The holder hint is written to the sibling .owner file (NOT the flock'd
	// lock file itself — on Windows that carries a mandatory byte-0 lock; T0830)
	// so a waiter can name who it's blocked on.
	data, _ := os.ReadFile(filepath.Join(store.Root(), ".lock.owner"))
	if !strings.Contains(string(data), "first-holder") {
		t.Fatalf("owner file should record holder, got %q", string(data))
	}

	got := make(chan struct{})
	started := make(chan struct{})
	go func() {
		close(started)
		unlock2, err := store.Lock("second-holder")
		if err == nil {
			unlock2()
		}
		close(got)
	}()

	// Wait for the goroutine to be scheduled and about to attempt the lock, so
	// the negative window below measures real blocking rather than a not-yet-run
	// goroutine.
	<-started

	// Negative-confirmation window: the second locker must NOT acquire while the
	// first holds it. Correctness of "blocked while held" is guaranteed by the OS
	// mandatory lock (store.go), not by this timer — the window only catches an
	// erroneous early acquire.
	select {
	case <-got:
		t.Fatal("second Lock acquired while first still held")
	case <-time.After(150 * time.Millisecond):
	}

	unlock1()
	// Failure-detection ceiling only: on success this returns the instant `got`
	// closes (~150 ms). The generous 10 s absorbs Windows CI lock-wake +
	// scheduler + file-I/O latency under load.
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("second Lock did not acquire after first released")
	}
}

// TestFileLock_TryContendsAndReleases exercises the fileLock primitive directly
// (the layer under Store.Lock that TestLockContention drives end-to-end). Two
// independent open file descriptions on the same path must contend: while the
// first holds the exclusive lock, the second's tryLock reports contention
// (false, nil) — never an error and never a false acquire. After the first
// unlocks, the second's tryLock succeeds. This pins the per-handle exclusivity
// guarantee deterministically, with no goroutine or timer.
func TestFileLock_TryContendsAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")

	a := newFileLock(path)
	gotA, err := a.tryLock()
	if err != nil {
		t.Fatalf("first tryLock: %v", err)
	}
	if !gotA {
		t.Fatal("first tryLock should acquire an uncontended lock")
	}

	b := newFileLock(path)
	gotB, err := b.tryLock()
	if err != nil {
		t.Fatalf("contended tryLock should report (false, nil), got err: %v", err)
	}
	if gotB {
		t.Fatal("second tryLock acquired while first still held")
	}

	if err := a.unlock(); err != nil {
		t.Fatalf("unlock first: %v", err)
	}

	gotB, err = b.tryLock()
	if err != nil {
		t.Fatalf("tryLock after release: %v", err)
	}
	if !gotB {
		t.Fatal("second tryLock should acquire after first released")
	}
	if err := b.unlock(); err != nil {
		t.Fatalf("unlock second: %v", err)
	}
}

// TestFileLock_UnlockBeforeOpen verifies unlock is a no-op when the lock was
// never opened (l.f == nil) — the early-return guard. Store.Lock's returned
// closure must be safe even on a fileLock that never reached flockTry.
func TestFileLock_UnlockBeforeOpen(t *testing.T) {
	l := newFileLock(filepath.Join(t.TempDir(), ".lock"))
	if err := l.unlock(); err != nil {
		t.Fatalf("unlock before open should be a no-op, got %v", err)
	}
}

// TestFileLock_BlockingAcquireAfterRelease drives the blocking lock() path (not
// just tryLock): one holder, a waiter that blocks in lock() until the holder
// releases. Mirrors Store.Lock's contended branch at the primitive level.
func TestFileLock_BlockingAcquireAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")

	holder := newFileLock(path)
	got, err := holder.tryLock()
	if err != nil || !got {
		t.Fatalf("holder tryLock: got=%v err=%v", got, err)
	}

	acquired := make(chan error, 1)
	started := make(chan struct{})
	waiter := newFileLock(path)
	go func() {
		close(started)
		acquired <- waiter.lock() // blocks until holder releases
	}()
	<-started

	select {
	case <-acquired:
		t.Fatal("waiter acquired blocking lock while holder still held")
	case <-time.After(150 * time.Millisecond):
	}

	if err := holder.unlock(); err != nil {
		t.Fatalf("holder unlock: %v", err)
	}

	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("waiter blocking lock: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waiter did not acquire after holder released")
	}
	if err := waiter.unlock(); err != nil {
		t.Fatalf("waiter unlock: %v", err)
	}
}

// TestStoreLock_WaitingMessageNamesHolder verifies the contended branch of
// Store.Lock reads the sibling .owner file and the second locker still
// acquires once the first releases — covering the "Waiting for <holder> to
// finish..." path that reads the identity hint.
func TestStoreLock_WaitingMessageNamesHolder(t *testing.T) {
	store := newTestStore(t)
	unlock1, err := store.Lock("alpha-holder")
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan struct{})
	started := make(chan struct{})
	go func() {
		close(started)
		unlock2, err := store.Lock("beta-holder")
		if err == nil {
			unlock2()
		}
		close(got)
	}()
	<-started

	select {
	case <-got:
		t.Fatal("second Lock acquired while first still held")
	case <-time.After(150 * time.Millisecond):
	}

	unlock1()
	select {
	case <-got:
	case <-time.After(10 * time.Second):
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

// TestExtractArchiveColonPath verifies extractArchive works when the archive's
// directory path contains a colon (simulating a Windows C:\... drive prefix).
// GNU tar 1.35 misparses the -f operand as [user@]host:path when it contains a
// colon; the fix passes only filepath.Base(archive) with cmd.Dir set to the
// archive's directory (T0809).
func TestExtractArchiveColonPath(t *testing.T) {
	parent := t.TempDir()
	colonDir := filepath.Join(parent, "C:")
	if err := os.Mkdir(colonDir, 0o755); err != nil {
		t.Skipf("filesystem does not support colon in directory name: %v", err)
	}
	content := []byte("tool-binary-data")
	archiveBytes := makeTar(map[string][]byte{"bin/tool": content})
	archivePath := filepath.Join(colonDir, sha256hex(archiveBytes))
	if err := os.WriteFile(archivePath, archiveBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(parent, "out")
	if err := extractArchive(archivePath, dst); err != nil {
		t.Fatalf("extractArchive with colon in path: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "bin", "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("wrong bytes after extraction with colon-containing path")
	}
}

// TestExtractArchiveRelativePathBranch exercises the common tar branch (T0772
// refinement of T0809): extractArchive runs tar with cwd=dst and the archive
// passed as a forward-slash path *relative* to dst, with no -C. On Windows the
// temp paths carry a `C:` drive letter, so this validates the colon-free
// relative-path invocation on the real host where GNU tar 1.35 would otherwise
// misparse the drive letter as a remote host. Runs on every platform.
func TestExtractArchiveRelativePathBranch(t *testing.T) {
	parent := t.TempDir()
	content := []byte("tool-binary-data")
	archiveBytes := makeTar(map[string][]byte{"bin/tool": content, "lib/data.txt": []byte("xyz")})
	// Archive lives in a sibling of dst so the relative path includes a `..`
	// component — the realistic CAS layout (archives/ next to the extract dir).
	archiveDir := filepath.Join(parent, "archives")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(archiveDir, sha256hex(archiveBytes))
	if err := os.WriteFile(archivePath, archiveBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(parent, "out")
	if err := extractArchive(archivePath, dst); err != nil {
		t.Fatalf("extractArchive: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "bin", "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("wrong bytes after extraction")
	}
	if _, err := os.ReadFile(filepath.Join(dst, "lib", "data.txt")); err != nil {
		t.Fatalf("second member not extracted: %v", err)
	}
}

// TestExtractArchiveCreatesDst verifies extractArchive creates a not-yet-existing
// destination directory before invoking tar (cmd.Dir = dst requires it to exist).
func TestExtractArchiveCreatesDst(t *testing.T) {
	parent := t.TempDir()
	archiveBytes := makeTar(map[string][]byte{"f": []byte("data")})
	archivePath := filepath.Join(parent, sha256hex(archiveBytes))
	if err := os.WriteFile(archivePath, archiveBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	// dst does not exist yet, and neither do its parents.
	dst := filepath.Join(parent, "a", "b", "out")
	if err := extractArchive(archivePath, dst); err != nil {
		t.Fatalf("extractArchive into missing dst: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(dst, "f")); err != nil {
		t.Fatalf("member not extracted into created dst: %v", err)
	}
}

// TestExtractArchiveTarFailure verifies that a corrupt (non-zip) file passed
// to extractArchive surfaces the tar exit-status error rather than silently
// succeeding or panicking.
func TestExtractArchiveTarFailure(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "bad.tar")
	if err := os.WriteFile(corrupt, []byte("this is not a valid tar archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out")
	err := extractArchive(corrupt, dst)
	if err == nil {
		t.Fatal("expected error from extractArchive with corrupt tar, got nil")
	}
}

// TestIsZipShortFile verifies isZip returns false for a file shorter than the
// 4-byte magic prefix — exercises the io.ReadFull short-read error path.
func TestIsZipShortFile(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short.bin")
	if err := os.WriteFile(short, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isZip(short) {
		t.Fatal("short file should not be detected as zip")
	}
}

// TestExtractZipCorruptData verifies extractZip rejects non-zip bytes and
// returns the zip.OpenReader error (not a nil or panic).
func TestExtractZipCorruptData(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "notazip.zip")
	if err := os.WriteFile(bad, []byte("definitely not a zip file"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := extractZip(bad, filepath.Join(dir, "out"))
	if err == nil {
		t.Fatal("expected error from extractZip with non-zip content, got nil")
	}
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

// TestStoreLock_WritesOwner verifies holder identity is recorded in the sibling
// <lock>.owner file, NOT the flock'd lock file itself (on Windows the file lock
// is a mandatory byte-0 lock that would silently drop the write; T0830).
func TestStoreLock_WritesOwner(t *testing.T) {
	root := t.TempDir()
	store := &Store{root: root}

	unlock, err := store.Lock("my-hint")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ownerPath := filepath.Join(root, ".lock.owner")
	data, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "my-hint" {
		t.Errorf("owner file = %q, want %q", got, "my-hint")
	}
}

// TestStoreLock_ClearsOnUnlock verifies the .owner sibling is removed on unlock
// (T0830).
func TestStoreLock_ClearsOnUnlock(t *testing.T) {
	root := t.TempDir()
	store := &Store{root: root}

	unlock, err := store.Lock("my-hint")
	if err != nil {
		t.Fatal(err)
	}
	unlock()

	ownerPath := filepath.Join(root, ".lock.owner")
	if _, err := os.Stat(ownerPath); !os.IsNotExist(err) {
		t.Errorf("owner file should be removed after unlock, stat err = %v", err)
	}
}

// TestLock_PathTargetedRoundTrip verifies the exported path-targeted Lock helper
// (T0920): it creates the parent dir, records + clears the holder, and the flock
// round-trips so a re-Lock after unlock succeeds non-blocking.
func TestLock_PathTargetedRoundTrip(t *testing.T) {
	// lockPath nested under a not-yet-existing dir → Lock must MkdirAll the parent.
	lockPath := filepath.Join(t.TempDir(), "sub", "view.lock")

	unlock, err := Lock(lockPath, "holder-a", "waiting...")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(lockPath + ".owner"); err != nil {
		t.Fatalf("owner file: %v", err)
	} else if got := strings.TrimSpace(string(data)); got != "holder-a" {
		t.Errorf("owner = %q, want %q", got, "holder-a")
	}
	unlock()

	if _, err := os.Stat(lockPath + ".owner"); !os.IsNotExist(err) {
		t.Errorf("owner file should be removed after unlock, stat err = %v", err)
	}

	// The flock must be fully released: a second acquisition succeeds immediately.
	unlock2, err := Lock(lockPath, "holder-b", "waiting...")
	if err != nil {
		t.Fatalf("re-Lock after unlock: %v", err)
	}
	unlock2()
}
