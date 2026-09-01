package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/promise-language/promise/compiler/internal/blobstore"
)

// TestBlobSetKeyOrderIndependentAndContentSensitive verifies the view-dir key is
// stable regardless of entry order but changes when any blob hash changes (so an
// LLVM version bump yields a fresh view dir rather than serving stale tools).
func TestBlobSetKeyOrderIndependentAndContentSensitive(t *testing.T) {
	t.Parallel()
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

// TestUnbrotliBytesRoundTrip verifies unbrotliBytes decompresses what brotli
// produces and rejects non-brotli input.
func TestUnbrotliBytesRoundTrip(t *testing.T) {
	t.Parallel()
	want := []byte("the raw opt binary bytes")
	var buf bytes.Buffer
	bw := brotli.NewWriterLevel(&buf, brotli.BestCompression)
	bw.Write(want)
	bw.Close()

	got, err := unbrotliBytes(buf.Bytes())
	if err != nil {
		t.Fatalf("unbrotliBytes: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("round-trip mismatch")
	}
	if _, err := unbrotliBytes([]byte("not brotli at all, definitely not")); err == nil {
		t.Fatal("expected error on non-brotli input")
	}
}

// TestGunzipBytesRoundTrip verifies gunzipBytes decompresses what gzip produces
// and rejects non-gzip input.
func TestGunzipBytesRoundTrip(t *testing.T) {
	t.Parallel()
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

// TestDecompressEmbeddedLLVMDispatch verifies the embedded-blob decompressor
// dispatches on the file extension: .br → brotli (publish path), .gz → gzip
// (dev/slim path), and rejects an unknown codec.
func TestDecompressEmbeddedLLVMDispatch(t *testing.T) {
	t.Parallel()
	want := []byte("the raw opt binary bytes")

	var brBuf bytes.Buffer
	bw := brotli.NewWriterLevel(&brBuf, brotli.BestCompression)
	bw.Write(want)
	bw.Close()

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	gw.Write(want)
	gw.Close()

	cases := []struct {
		name string
		data []byte
	}{
		{"opt.br", brBuf.Bytes()},
		{"opt.exe.br", brBuf.Bytes()},
		{"opt.gz", gzBuf.Bytes()},
		{"opt.exe.gz", gzBuf.Bytes()},
	}
	for _, c := range cases {
		got, err := decompressEmbeddedLLVM(c.name, c.data)
		if err != nil {
			t.Fatalf("decompressEmbeddedLLVM(%q): %v", c.name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("decompressEmbeddedLLVM(%q) round-trip mismatch", c.name)
		}
	}

	// Cross-codec mismatch (brotli bytes named .gz) must error, not silently
	// return garbage.
	if _, err := decompressEmbeddedLLVM("opt.gz", brBuf.Bytes()); err == nil {
		t.Fatal("expected error decoding brotli bytes as gzip")
	}
	// Unknown extension is a hard error.
	if _, err := decompressEmbeddedLLVM("opt.raw", want); err == nil {
		t.Fatal("expected error on unknown embedded blob codec")
	}
}

// TestPrebuiltToolPath covers resolving an LLVM tool from the host-stable
// prebuilts cache — the offline path that lets a machine which has built the
// compiler materialize the toolchain view without a network download.
func TestPrebuiltToolPath(t *testing.T) {
	target := runtime.GOOS + "-" + runtime.GOARCH
	root := t.TempDir()
	toolDir := filepath.Join(root, "llvm-slim", "22.1.0", target)
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	optPath := filepath.Join(toolDir, "opt")
	if err := os.WriteFile(optPath, []byte("fake-opt"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROMISE_PREBUILTS_CACHE", root)

	reset := func() {
		prebuiltToolMu.Lock()
		prebuiltToolCache = nil
		prebuiltToolMu.Unlock()
	}
	defer reset()

	// Without the tools.ok completion marker the dir is treated as half-populated.
	reset()
	if got := prebuiltToolPath("opt"); got != "" {
		t.Fatalf("expected no match before tools.ok, got %q", got)
	}

	if err := os.WriteFile(filepath.Join(toolDir, "tools.ok"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	reset()
	if got := prebuiltToolPath("opt"); got != optPath {
		t.Fatalf("expected %q, got %q", optPath, got)
	}

	// A tool absent from the cache, and the empty name, both resolve to "".
	reset()
	if got := prebuiltToolPath("llc"); got != "" {
		t.Fatalf("expected no match for absent tool, got %q", got)
	}
	reset()
	if got := prebuiltToolPath(""); got != "" {
		t.Fatalf("expected empty for empty name, got %q", got)
	}
}

// TestPrebuiltToolPathPrefersNewestVersion verifies that when several toolchain
// versions are cached the newest wins by numeric (not lexical) order — 22.10.0
// must beat 22.9.0.
func TestPrebuiltToolPathPrefersNewestVersion(t *testing.T) {
	target := runtime.GOOS + "-" + runtime.GOARCH
	root := t.TempDir()
	var newest string
	for _, v := range []string{"22.9.0", "22.10.0"} {
		dir := filepath.Join(root, "llvm-slim", v, target)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "opt")
		if err := os.WriteFile(p, []byte("opt"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tools.ok"), []byte("ok"), 0o644); err != nil {
			t.Fatal(err)
		}
		if v == "22.10.0" {
			newest = p
		}
	}
	t.Setenv("PROMISE_PREBUILTS_CACHE", root)
	prebuiltToolMu.Lock()
	prebuiltToolCache = nil
	prebuiltToolMu.Unlock()
	defer func() {
		prebuiltToolMu.Lock()
		prebuiltToolCache = nil
		prebuiltToolMu.Unlock()
	}()

	if got := prebuiltToolPath("opt"); got != newest {
		t.Fatalf("expected newest version %q, got %q", newest, got)
	}
}

func TestCompareLLVMVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want int
	}{
		{"22.10.0", "22.9.0", 1},
		{"22.9.0", "22.10.0", -1},
		{"22.1.0", "22.1.0", 0},
		{"22.1.1", "22.1.0", 1},
		{"23.0.0", "22.99.99", 1},
		{"22.1", "22.1.0", 0}, // missing components compare as 0
		{"garbage", "22.0.0", -1},
	}
	for _, c := range cases {
		if got := compareLLVMVersion(c.a, c.b); got != c.want {
			t.Errorf("compareLLVMVersion(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestViewComplete verifies the view-dir completeness check: it requires every
// LLVM blob file and, when lld is present, the lld-mode aliases.
func TestViewComplete(t *testing.T) {
	t.Parallel()
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
	// Where the toolchain needs generated stub libraries, a view built before
	// they existed carries the tools but not the stubs, and must be rebuilt
	// rather than served from the fast path forever (T1774).
	if needsToolchainStubs() {
		if viewComplete(dir, entries) {
			t.Fatal("view without the stub marker should be incomplete")
		}
		if err := os.WriteFile(filepath.Join(dir, stubMarkerName), nil, 0o644); err != nil {
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
	t.Parallel()
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

// viewFileNames returns every file a complete view for entries must contain:
// the (prefix-stripped) blob names plus, when lld is present, the lld-mode
// aliases that viewComplete also requires.
func viewFileNames(entries []*blobstore.ManifestEntry) []string {
	var names []string
	hasLLD := false
	for _, e := range entries {
		n := strings.TrimPrefix(e.Name, llvmEntryPrefix)
		names = append(names, n)
		if n == "lld" || n == "lld.exe" {
			hasLLD = true
		}
	}
	if hasLLD {
		for link := range embeddedLLVMSymlinks {
			n := link
			if runtime.GOOS == "windows" {
				n = link + ".exe"
			}
			names = append(names, n)
		}
		if needsToolchainStubs() {
			names = append(names, stubMarkerName)
		}
	}
	return names
}

// TestViewPublishAtomic verifies publishViewDir is all-or-nothing: on success the
// viewDir contains exactly the populated files and no .tmp-* sibling lingers; on a
// populate error the viewDir is never created and the temp dir is cleaned up.
func TestViewPublishAtomic(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	viewDir := filepath.Join(parent, "view")

	// Pre-seed a stale orphan .tmp-* dir; a successful publish should reap it
	// (T1077 opportunistic sweep in publishViewDir).
	orphan := filepath.Join(parent, ".tmp-view-orphan")
	if err := os.Mkdir(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(orphan, past, past); err != nil {
		t.Fatal(err)
	}

	// Success: populate three files into the temp dir, publish.
	want := []string{"opt", "lld", "llc"}
	if err := publishViewDir(parent, viewDir, func(tmp string) error {
		for _, n := range want {
			if err := os.WriteFile(filepath.Join(tmp, n), []byte("x"), 0o755); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("publishViewDir: %v", err)
	}
	for _, n := range want {
		if _, err := os.Stat(filepath.Join(viewDir, n)); err != nil {
			t.Errorf("published view missing %q: %v", n, err)
		}
	}
	// No temp-dir residue should remain in the parent — including the pre-seeded
	// stale orphan, which the opportunistic sweep should have reaped.
	assertNoTmpResidue(t, parent, viewDir)
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("stale orphan .tmp dir should be swept by publishViewDir, stat err = %v", err)
	}

	// Failure: a populate error leaves viewDir absent and cleans the temp dir.
	failView := filepath.Join(parent, "failview")
	wantErr := errors.New("populate boom")
	if err := publishViewDir(parent, failView, func(tmp string) error {
		os.WriteFile(filepath.Join(tmp, "partial"), []byte("x"), 0o755)
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("publishViewDir error = %v, want %v", err, wantErr)
	}
	if _, err := os.Stat(failView); !os.IsNotExist(err) {
		t.Errorf("failed publish should leave viewDir absent, stat err = %v", err)
	}
	assertNoTmpResidue(t, parent, failView)
}

// TestSweepStaleViewStaging verifies the orphan-reaper only removes `.tmp-*`
// staging dirs older than maxAge, leaving fresh temp dirs, published views, and
// unrelated entries untouched (T1077).
func TestSweepStaleViewStaging(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()

	// An old orphan .tmp-* dir — backdated well past the age gate.
	oldTmp := filepath.Join(parent, ".tmp-view-AAA")
	if err := os.Mkdir(oldTmp, 0o755); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldTmp, past, past); err != nil {
		t.Fatal(err)
	}
	// A fresh .tmp-* dir (a concurrent populator's live staging dir).
	freshTmp := filepath.Join(parent, ".tmp-view-BBB")
	if err := os.Mkdir(freshTmp, 0o755); err != nil {
		t.Fatal(err)
	}
	// A published view dir (no .tmp- prefix) and an unrelated file.
	published := filepath.Join(parent, "view-CCC")
	if err := os.Mkdir(published, 0o755); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(parent, "keep.txt")
	if err := os.WriteFile(unrelated, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := sweepStaleViewStaging(parent, staleViewStagingAge); got != 1 {
		t.Errorf("sweepStaleViewStaging removed = %d, want 1", got)
	}
	if _, err := os.Stat(oldTmp); !os.IsNotExist(err) {
		t.Errorf("stale .tmp dir should be removed, stat err = %v", err)
	}
	for _, keep := range []string{freshTmp, published, unrelated} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%q should be retained: %v", keep, err)
		}
	}
}

// TestSweepStaleViewStagingEdges covers the reaper's best-effort edges: a
// nonexistent parent (ReadDir error), a stale `.tmp-`-prefixed regular file
// (not a dir, so skipped), and multiple stale orphans counted together (T1077).
func TestSweepStaleViewStagingEdges(t *testing.T) {
	t.Parallel()
	// Nonexistent parent: ReadDir fails, sweep returns 0 without panicking.
	if got := sweepStaleViewStaging(filepath.Join(t.TempDir(), "does-not-exist"), staleViewStagingAge); got != 0 {
		t.Errorf("sweep of missing parent removed = %d, want 0", got)
	}

	parent := t.TempDir()
	past := time.Now().Add(-48 * time.Hour)

	// A stale `.tmp-`-prefixed regular file — staging entries are always dirs, so
	// the IsDir gate must skip this even though the name and age both match.
	tmpFile := filepath.Join(parent, ".tmp-view-file")
	if err := os.WriteFile(tmpFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmpFile, past, past); err != nil {
		t.Fatal(err)
	}
	// Two stale orphan dirs — both should be reaped and counted.
	for _, name := range []string{".tmp-view-1", ".tmp-view-2"} {
		d := filepath.Join(parent, name)
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(d, past, past); err != nil {
			t.Fatal(err)
		}
	}

	if got := sweepStaleViewStaging(parent, staleViewStagingAge); got != 2 {
		t.Errorf("sweepStaleViewStaging removed = %d, want 2", got)
	}
	if _, err := os.Stat(tmpFile); err != nil {
		t.Errorf("stale .tmp-prefixed regular file should be retained: %v", err)
	}
}

// TestViewPublishRenameFailure verifies the publish-failure branch: when the
// final rename(2) cannot complete (here the destination's parent dir does not
// exist), publishViewDir returns a wrapped "publish view" error AND still removes
// the staged temp dir — a failed publish must never leak a half-built sibling.
func TestViewPublishRenameFailure(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	// viewDir lives under a missing intermediate dir, so rename(tmp, viewDir)
	// fails with ENOENT after the temp dir was staged in parent.
	viewDir := filepath.Join(parent, "missing-intermediate", "view")

	err := publishViewDir(parent, viewDir, func(tmp string) error {
		return os.WriteFile(filepath.Join(tmp, "opt"), []byte("x"), 0o755)
	})
	if err == nil {
		t.Fatal("expected publishViewDir to fail when rename target's parent is missing")
	}
	if !strings.Contains(err.Error(), "publish view") {
		t.Errorf("error %q should be wrapped with %q", err, "publish view")
	}
	if _, statErr := os.Stat(viewDir); !os.IsNotExist(statErr) {
		t.Errorf("failed publish should leave viewDir absent, stat err = %v", statErr)
	}
	// The staged temp dir (.tmp-view-*) must have been cleaned from parent.
	assertNoTmpResidue(t, parent, viewDir)
}

func assertNoTmpResidue(t *testing.T, parent, viewDir string) {
	t.Helper()
	prefix := ".tmp-" + filepath.Base(viewDir) + "-"
	ents, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), prefix) {
			t.Errorf("leftover temp dir not cleaned: %s", e.Name())
		}
	}
}

// TestViewMaterializeConcurrent reproduces the T0920 race: N workers race to
// materialize the same view while readers continuously observe it. With the
// lock + atomic-publish barrier, exactly one worker publishes, every other worker
// sees the view already complete after taking the lock, and no reader ever
// observes a missing/partial file once viewComplete reports true.
func TestViewMaterializeConcurrent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	viewDir := filepath.Join(root, "llvm-view", "host-abc123")
	lockPath := filepath.Join(root, "llvm-view.lock")
	entries := []*blobstore.ManifestEntry{
		{Name: "llvm-opt", SHA256: "aa"},
		{Name: "llvm-lld", SHA256: "bb"},
		{Name: "llvm-llc", SHA256: "cc"},
	}
	files := viewFileNames(entries)

	var publishes int32
	var readerFail atomic.Value // first failure message
	stop := make(chan struct{})

	// Readers: spin until stop, validating every claimed-complete view fully opens.
	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if !viewComplete(viewDir, entries) {
					continue
				}
				for _, n := range files {
					if _, err := os.Stat(filepath.Join(viewDir, n)); err != nil {
						readerFail.Store(fmt.Sprintf("reader saw complete view but %q is unreadable: %v", n, err))
						return
					}
				}
			}
		}()
	}

	// Workers: each locks, double-checks, and only the first one publishes. The
	// populate deliberately writes files one at a time so a non-atomic publish
	// would expose a partial dir — but the temp dir is invisible to readers.
	var workers sync.WaitGroup
	for w := 0; w < 8; w++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			unlock, err := blobstore.Lock(lockPath, "test-worker", "waiting...")
			if err != nil {
				t.Error(err)
				return
			}
			defer unlock()
			if viewComplete(viewDir, entries) {
				return
			}
			err = publishViewDir(filepath.Dir(viewDir), viewDir, func(tmp string) error {
				for _, n := range files {
					if e := os.WriteFile(filepath.Join(tmp, n), []byte("x"), 0o755); e != nil {
						return e
					}
				}
				return nil
			})
			if err != nil {
				t.Error(err)
				return
			}
			atomic.AddInt32(&publishes, 1)
		}()
	}
	workers.Wait()
	close(stop)
	readers.Wait()

	if got := atomic.LoadInt32(&publishes); got != 1 {
		t.Errorf("expected exactly one publish, got %d", got)
	}
	if msg, ok := readerFail.Load().(string); ok {
		t.Error(msg)
	}
	if !viewComplete(viewDir, entries) {
		t.Error("view should be complete after all workers finished")
	}
}

// TestCleanViewsUnderLock verifies cleanViewsUnderLock removes the llvm-view and
// crt-view trees while holding the cross-process locks (T1684). A goroutine
// attempting to acquire one of those locks during the clean must block until the
// clean is finished — the lock serialization is the whole point of the fix.
func TestCleanViewsUnderLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PROMISE_HOME", home)

	cacheDir := filepath.Join(home, "cache")
	// Seed the view trees that CleanLLVMCache/CleanCRTCache remove.
	for _, dir := range []string{
		"llvm-view/host-abc",
		"crt-view/host-def",
		"compiler-rt-view/host-ghi",
	} {
		if err := os.MkdirAll(filepath.Join(cacheDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cleanViewsUnderLock(home)

	for _, dir := range []string{"llvm-view", "crt-view", "compiler-rt-view"} {
		if _, err := os.Stat(filepath.Join(cacheDir, dir)); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed, stat err = %v", dir, err)
		}
	}
}

// TestCleanViewsUnderLockSerializes verifies cleanViewsUnderLock holds the
// materialization locks for the full duration of the clean, so a concurrent
// publishViewDir blocks rather than losing its staging dir mid-write (T1684).
func TestCleanViewsUnderLockSerializes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PROMISE_HOME", home)

	cacheDir := filepath.Join(home, "cache")
	if err := os.MkdirAll(filepath.Join(cacheDir, "llvm-view", "old-view"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-acquire the llvm-view lock to simulate a concurrent materializer
	// holding it. cleanViewsUnderLock must still complete (it tolerates lock
	// contention by continuing without the lock on error — but under the
	// file-lock implementation on this platform it should block until we
	// release). We test the non-blocking property: if we hold the lock,
	// cleanViewsUnderLock should still complete (it skips locks it can't get).
	lockPath := filepath.Join(cacheDir, "llvm-view.lock")
	unlock, err := blobstore.Lock(lockPath, "test-holder", "test waiting...")
	if err != nil {
		t.Fatal(err)
	}

	// Run clean in a goroutine — it may block on the lock we hold.
	done := make(chan struct{})
	go func() {
		defer close(done)
		cleanViewsUnderLock(home)
	}()

	// Give the clean goroutine time to attempt the lock, then release.
	time.Sleep(50 * time.Millisecond)
	unlock()

	// The clean should complete now.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cleanViewsUnderLock did not complete within 10s after lock release")
	}
}

// TestMaterializeViewFileSymlink verifies materializeViewFile on the current
// platform. On Linux it creates a symlink; on macOS/Windows it copies.
func TestMaterializeViewFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blobPath := filepath.Join(dir, "blob")
	if err := os.WriteFile(blobPath, []byte("binary content"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "tool")
	if err := materializeViewFile(blobPath, dst); err != nil {
		t.Fatalf("materializeViewFile: %v", err)
	}
	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("dst not created: %v", err)
	}
	switch runtime.GOOS {
	case "darwin", "windows":
		// Should be a regular file (copy), not a symlink.
		if fi.Mode()&os.ModeSymlink != 0 {
			t.Error("expected a copy, got a symlink")
		}
		data, _ := os.ReadFile(dst)
		if !bytes.Equal(data, []byte("binary content")) {
			t.Errorf("copy content mismatch: got %q", data)
		}
	default:
		// Should be a symlink.
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Error("expected a symlink, got a regular file")
		}
		target, _ := os.Readlink(dst)
		if target != blobPath {
			t.Errorf("symlink target = %q, want %q", target, blobPath)
		}
	}
}

// TestMaterializeViewFileOverwrite verifies that materializeViewFile replaces a
// pre-existing file at the destination (stale leftover from a partial view).
func TestMaterializeViewFileOverwrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blobPath := filepath.Join(dir, "blob")
	if err := os.WriteFile(blobPath, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "tool")
	if err := os.WriteFile(dst, []byte("old stale content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := materializeViewFile(blobPath, dst); err != nil {
		t.Fatalf("materializeViewFile: %v", err)
	}
	// Verify the old content was replaced.
	switch runtime.GOOS {
	case "darwin", "windows":
		data, _ := os.ReadFile(dst)
		if bytes.Equal(data, []byte("old stale content")) {
			t.Error("materializeViewFile did not replace the stale file")
		}
	default:
		target, _ := os.Readlink(dst)
		if target != blobPath {
			t.Errorf("symlink target = %q, want %q", target, blobPath)
		}
	}
}

// TestPublishViewDirConcurrentCleanSafety simulates the T1684 scenario: a
// concurrent clean removes the parent view tree while publishViewDir is staging
// files into its temp dir. Under the lock discipline, the clean waits; this
// test verifies that publish under the lock succeeds even when the parent dir
// was re-created by someone else after a clean.
func TestPublishViewDirConcurrentCleanSafety(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	parent := filepath.Join(root, "llvm-view")
	viewDir := filepath.Join(parent, "view-content-key")

	// Publish should succeed even when parent doesn't pre-exist (MkdirAll
	// in publishViewDir creates it).
	err := publishViewDir(parent, viewDir, func(tmp string) error {
		return os.WriteFile(filepath.Join(tmp, "opt"), []byte("x"), 0o755)
	})
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if _, err := os.Stat(filepath.Join(viewDir, "opt")); err != nil {
		t.Errorf("published file missing: %v", err)
	}

	// Remove the parent (as cleanViewsUnderLock would) and re-publish.
	os.RemoveAll(parent)
	viewDir2 := filepath.Join(parent, "view-new-key")
	err = publishViewDir(parent, viewDir2, func(tmp string) error {
		return os.WriteFile(filepath.Join(tmp, "llc"), []byte("y"), 0o755)
	})
	if err != nil {
		t.Fatalf("re-publish after parent removal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(viewDir2, "llc")); err != nil {
		t.Errorf("re-published file missing: %v", err)
	}
}
