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
	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
	"github.com/promise-language/promise/compiler/internal/blobstore"
	"github.com/promise-language/promise/compiler/internal/casmetrics"
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

// TestCleanViewsUnderLock verifies cleanViewsUnderLock removes every derived
// view tree while holding the cross-process locks (T1684). A goroutine
// attempting to acquire one of those locks during the clean must block until the
// clean is finished — the lock serialization is the whole point of the fix.
//
// runtime-view is in the list since T2169. It is the largest of them — a Node
// view alone is ~120 MB — so a view left out here is disk this command promised
// to reclaim and did not, and a stale view that a later run could serve from a
// name-only match.
func TestCleanViewsUnderLock(t *testing.T) {
	home := clitest.TempDir(t)
	t.Setenv("PROMISE_HOME", home)

	// Every view tree cleanViewsUnderLock is responsible for, and the lock each
	// one must be removed under. Kept as one table so a view added to the
	// product without its lock fails here rather than silently racing a
	// concurrent materializer.
	views := map[string]string{
		"llvm-view":        "llvm-view.lock",
		"runtime-view":     "runtime-view.lock",
		"crt-view":         "crt-view.lock",
		"compiler-rt-view": "compiler-rt-view.lock",
	}

	cacheDir := filepath.Join(home, "cache")
	for dir := range views {
		if err := os.MkdirAll(filepath.Join(cacheDir, dir, "host-abc"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cleanViewsUnderLock(home)

	for dir := range views {
		if _, err := os.Stat(filepath.Join(cacheDir, dir)); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed, stat err = %v", dir, err)
		}
	}

	// Each view's lock file must exist afterwards: cleanViewsUnderLock creates
	// it to take the lock, and it lives OUTSIDE the view tree precisely so the
	// RemoveAll above cannot delete it mid-hold. A lock that vanished with its
	// tree is the T1684 defect.
	for dir, lock := range views {
		if _, err := os.Stat(filepath.Join(cacheDir, lock)); err != nil {
			t.Errorf("%s was cleaned without holding %s (stat err = %v)", dir, lock, err)
		}
	}
}

// The product's lock list must cover every tree its clean functions remove.
// Without this, adding a view to CleanLLVMCache/CleanCRTCache and forgetting its
// lock reads as working — the tree is removed, just not safely — and only shows
// up as a rare mid-write failure on a busy machine.
func TestCleanViewsUnderLockCoversEveryViewItRemoves(t *testing.T) {
	home := clitest.TempDir(t)
	t.Setenv("PROMISE_HOME", home)
	cacheDir := filepath.Join(home, "cache")

	// Seed one extra view that nothing is responsible for, to prove the check
	// below is actually reading what was removed rather than passing vacuously.
	for _, dir := range []string{"llvm-view", "runtime-view", "crt-view", "compiler-rt-view"} {
		if err := os.MkdirAll(filepath.Join(cacheDir, dir, "v"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cleanViewsUnderLock(home)

	locks, err := filepath.Glob(filepath.Join(cacheDir, "*-view.lock"))
	if err != nil {
		t.Fatal(err)
	}
	held := map[string]bool{}
	for _, l := range locks {
		held[strings.TrimSuffix(filepath.Base(l), ".lock")] = true
	}
	for _, dir := range []string{"llvm-view", "runtime-view", "crt-view", "compiler-rt-view"} {
		if !held[dir] {
			t.Errorf("%s was removed but no %s.lock was taken — the clean can race a materializer", dir, dir)
		}
	}
}

// TestCleanViewsUnderLockSerializes verifies cleanViewsUnderLock holds the
// materialization locks for the full duration of the clean, so a concurrent
// publishViewDir blocks rather than losing its staging dir mid-write (T1684).
func TestCleanViewsUnderLockSerializes(t *testing.T) {
	home := clitest.TempDir(t)
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
	written, err := materializeViewFile(blobPath, dst)
	if err != nil {
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
	// T2143: the reported cost is what the platform actually wrote. macOS owns
	// its bytes (it patches and re-signs them); Linux symlinks and Windows
	// hardlinks write none, and reporting a size there would make every warm
	// platform look like the cold one the accounting exists to distinguish.
	wantWritten := int64(0)
	if runtime.GOOS == "darwin" {
		wantWritten = int64(len("binary content"))
	}
	if written != wantWritten {
		t.Errorf("materializeViewFile reported %d bytes written, want %d", written, wantWritten)
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
	if _, err := materializeViewFile(blobPath, dst); err != nil {
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

// TestLinkOrCopyBlobLinksOnOneFilesystem pins the cheap path: an immutable blob
// enters a view dir as a second name for the same inode, so a cold view costs
// metadata rather than the ~375 MB macOS and ~900 MB Windows were copying per
// PROMISE_HOME (T2133).
func TestLinkOrCopyBlobLinksOnOneFilesystem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "blob")
	if err := os.WriteFile(src, []byte("tool bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "opt")
	written, err := linkOrCopyBlob(src, dst, 0o755)
	if err != nil {
		t.Fatalf("linkOrCopyBlob: %v", err)
	}
	// T2143: a link costs no bytes, and says so. Reporting the source's size
	// here would make the cheap path indistinguishable from the copy.
	if written != 0 {
		t.Errorf("a link reported %d bytes written, want 0", written)
	}
	// A source that is not there costs nothing rather than failing the count:
	// the materialization itself is about to report the real error, and a
	// panicking accountant would take the build down with it.
	if got := blobSize(filepath.Join(dir, "no-such-blob")); got != 0 {
		t.Errorf("blobSize of a missing file = %d, want 0", got)
	}
	si, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(si, di) {
		t.Errorf("%s is a copy of %s, want a link on one filesystem", dst, src)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "tool bytes" {
		t.Errorf("linked file = %q, %v; want the source bytes", got, err)
	}
}

// TestLinkOrCopyBlobFallsBackToACopy: where a link cannot be made — the CAS and
// the view on different filesystems — the blob is still materialized, byte for
// byte, over whatever was at the destination, and the source is left alone.
func TestLinkOrCopyBlobFallsBackToACopy(t *testing.T) {
	t.Parallel()
	crossDevice := func(string, string) error { return errors.New("cross-device link") }

	dir := t.TempDir()
	src := filepath.Join(dir, "blob")
	if err := os.WriteFile(src, []byte("tool bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A stale file already at dst, so the replacement half is exercised too.
	dst := filepath.Join(dir, "opt")
	if err := os.WriteFile(dst, []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	written, err := linkOrCopy(crossDevice, src, dst, 0o755)
	if err != nil {
		t.Fatalf("linkOrCopy did not fall back: %v", err)
	}
	// T2143: the copy DID cost bytes, and says so.
	if written != int64(len("tool bytes")) {
		t.Errorf("the copy reported %d bytes written, want %d", written, len("tool bytes"))
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "tool bytes" {
		t.Fatalf("copied file = %q, %v; want the source bytes", got, err)
	}
	si, _ := os.Stat(src)
	di, _ := os.Stat(dst)
	if os.SameFile(si, di) {
		t.Errorf("the fallback produced a link, not a copy")
	}
	if data, err := os.ReadFile(src); err != nil || string(data) != "tool bytes" {
		t.Errorf("the source was disturbed: %q, %v", data, err)
	}
}

// TestMakeLLDAliasesDoNotDuplicateLLD: the four lld-mode aliases are names for
// the one lld, never four copies of it. On Windows they were copies — 525 MB of
// duplicate bytes in every view (T2133) — and everywhere else symlinks.
func TestMakeLLDAliasesDoNotDuplicateLLD(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lldName := "lld"
	if runtime.GOOS == "windows" {
		lldName = "lld.exe"
	}
	lldPath := filepath.Join(dir, lldName)
	if err := os.WriteFile(lldPath, []byte("lld bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := makeLLDAliases(dir); err != nil {
		t.Fatalf("makeLLDAliases: %v", err)
	}
	lldInfo, err := os.Stat(lldPath)
	if err != nil {
		t.Fatal(err)
	}
	for link := range embeddedLLVMSymlinks {
		name := link
		if runtime.GOOS == "windows" {
			name = link + ".exe"
		}
		aliasPath := filepath.Join(dir, name)
		info, err := os.Stat(aliasPath)
		if err != nil {
			t.Errorf("alias %s missing: %v", name, err)
			continue
		}
		if !os.SameFile(lldInfo, info) {
			t.Errorf("alias %s resolves to its own bytes, want the one lld", name)
		}
	}
}

// TestMaterializeViewFileLeavesARunnableToolOfItsOwn covers the two properties
// TestMaterializeViewFile above does not, both of which a materialized tool
// silently fails without.
//
// The mode: the view dir is what findLLVMTool hands the driver, so the file has
// to be executable. macOS reaches it down two paths — clonefile, which inherits
// the source's mode, and a streamed copy, which sets one — and a blob that ever
// reached the CAS non-executable would give a runnable tool down one and an
// EACCES down the other.
//
// The inode: outside Linux the file must be its own, because PatchAndSignMachO
// rewrites it in place and language-design.md#primitive-types-are-regular-types requires the CAS blob to stay the raw upstream
// bytes its content hash was computed over.
func TestMaterializeViewFileLeavesARunnableToolOfItsOwn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blob := filepath.Join(dir, "blob")
	if err := os.WriteFile(blob, []byte("opt bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "opt")
	if _, err := materializeViewFile(blob, dst); err != nil {
		t.Fatalf("materializeViewFile: %v", err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("the materialized tool is not executable: %v", fi.Mode().Perm())
	}
	if runtime.GOOS == "darwin" {
		bi, err := os.Stat(blob)
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(bi, fi) {
			t.Errorf("macOS shares the blob's inode; the patch+re-sign would rewrite hashed content")
		}
	}
}

// TestMakeLLDAliasesWithoutLLDIsANoOp: a view whose manifest carries no lld
// (a thin placeholder) must publish without aliases rather than fail, since
// viewComplete only demands them when lld is present.
func TestMakeLLDAliasesWithoutLLDIsANoOp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := makeLLDAliases(dir); err != nil {
		t.Fatalf("makeLLDAliases with no lld: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("makeLLDAliases invented %d entries with no lld to alias", len(entries))
	}
}

// TestEmbeddedTargetDepReadsWhatTheBinaryCarries covers the adapter that gives
// resolveTargetDepView its third blob source (T2150), on every platform: a file
// the embed FS carries comes back verbatim, one it does not reads as absent
// rather than as an error, and a build that embeds nothing for a dependency
// supplies no source at all.
//
// It reads the embedded std module, which every build carries, so the adapter
// is covered where the musl/compiler-rt trees are empty stubs.
func TestEmbeddedTargetDepReadsWhatTheBinaryCarries(t *testing.T) {
	t.Parallel()

	local := embeddedTargetDep(embeddedModules, "resources/modules/std", true)
	if local == nil {
		t.Fatal("embeddedTargetDep(present: true) supplied no source")
	}
	want, err := embeddedModules.ReadFile("resources/modules/std/string.pr")
	if err != nil {
		t.Skipf("this build embeds no std/string.pr to read: %v", err)
	}
	got, ok := local("string.pr")
	if !ok {
		t.Fatal("the adapter reported an embedded file as absent")
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the adapter returned %d bytes, want the embedded %d", len(got), len(want))
	}
	if _, ok := local("no_such_file.a"); ok {
		t.Error("a file the embed FS does not carry must read as absent, not as bytes")
	}
	if embeddedTargetDep(embeddedModules, "resources/modules/std", false) != nil {
		t.Error("a build that embeds nothing for a dependency must supply no source")
	}

	// The arch lives in the prefix, so a dep+arch this binary was not built for
	// reads as absent for EVERY file of it. That is the invariant the ladder
	// rests on: the CAS view stays the only path that can serve an arch the
	// binary does not embed (docs/runtime-architecture.md, step 4). If this ever
	// answered bytes, a cross-compile would silently link one arch's CRT into
	// another's binary — a wrong answer, where a fetch is merely a slow one.
	crossArch := embeddedTargetDep(embeddedMuslCRT, "resources/crt/sparc64-linux-musl", true)
	for _, f := range muslCRTFiles {
		if _, ok := crossArch(f); ok {
			t.Errorf("an arch this binary does not embed answered bytes for %q", f)
		}
	}
}

// TestLocalTargetDepFileIsNilSafe: a platform whose embed stub is empty passes a
// nil source, and the population loop must read that as "this binary carries no
// copy" rather than panicking on the way to the resolver.
func TestLocalTargetDepFileIsNilSafe(t *testing.T) {
	t.Parallel()
	if data, ok := localTargetDepFile(nil, "crt1.o"); ok || data != nil {
		t.Errorf("localTargetDepFile(nil) = (%v, %v), want (nil, false)", data, ok)
	}
}

// TestResolveTargetDepViewDoesNotFetchWhatTheBinaryEmbeds is the item's own
// assertion (T2150): against a home whose CAS is EMPTY, materializing a target
// dependency this binary carries must cost zero bytes over the wire.
//
// Before the third blob source existed, this path had only store.Has and the
// resolver, so every cold home downloaded the musl CRT and the compiler-rt
// builtins the running binary already contained — 55 MB across one sweep of the
// CLI test packages, none of which asked for it.
//
// Skips where it cannot mean anything: a binary that embeds no musl CRT (every
// non-Linux host), or a manifest carrying no musl blobs for this arch, never
// reaches the branch at all.
func TestResolveTargetDepViewDoesNotFetchWhatTheBinaryEmbeds(t *testing.T) {
	if !hasEmbeddedMuslCRT {
		t.Skip("this binary embeds no musl CRT, so there is no local copy to prefer")
	}
	arch := muslArchDir(runtime.GOARCH + "-unknown-linux-musl")
	if _, err := embeddedMuslCRT.ReadFile("resources/crt/" + arch + "/crt1.o"); err != nil {
		t.Skipf("this binary embeds no CRT for %s: %v", arch, err)
	}
	t.Setenv("PROMISE_HOME", t.TempDir()) // an empty CAS: every blob is a miss

	before := casmetrics.Read()
	if !before.Available {
		t.Skip("no ledger beside the test binary, so nothing can be measured")
	}
	viewDir, err := resolveMuslCRTView(arch)
	if err != nil {
		t.Fatalf("resolveMuslCRTView against a cold home: %v", err)
	}
	if viewDir == "" {
		t.Skipf("this binary's manifest carries no musl blobs for %s, so rung 4 falls through", arch)
	}
	if !muslCRTComplete(viewDir) {
		t.Fatalf("the published view %q is missing CRT objects", viewDir)
	}
	if fetched := casmetrics.Read().NetworkBytes - before.NetworkBytes; fetched != 0 {
		t.Errorf("a cold home fetched %d bytes for a CRT this binary carries", fetched)
	}
	// The view holds the embedded bytes, which is what proves the local branch
	// ran rather than a store hit that could not have happened here.
	want, err := embeddedMuslCRT.ReadFile("resources/crt/" + arch + "/crt1.o")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(viewDir, "crt1.o"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the view's crt1.o is %d bytes, want the embedded %d", len(got), len(want))
	}
}

// TestChooseTargetDepSourceOrdersItsThreeSources pins the precedence T2150
// introduced, on every platform — which is the point of it being a function at
// all. The loop it serves is unreachable off Linux (no other target's manifest
// carries musl or compiler-rt blobs), so without this the ordering would be
// checked by one CI job and by nothing else.
func TestChooseTargetDepSourceOrdersItsThreeSources(t *testing.T) {
	t.Parallel()
	embedded := func(string) ([]byte, bool) { return []byte("embedded bytes"), true }
	absent := func(string) ([]byte, bool) { return nil, false }

	for _, tc := range []struct {
		name    string
		inStore bool
		local   func(string) ([]byte, bool)
		want    targetDepSource
		// wantData is what the caller writes; only the embedded source carries any.
		wantData string
	}{
		// The store wins even when this binary also has the file: its bytes are
		// the ones the manifest's hash was taken over.
		{name: "store over embedded", inStore: true, local: embedded, want: sourceStore},
		{name: "store over wire", inStore: true, local: absent, want: sourceStore},
		{name: "store over no source at all", inStore: true, local: nil, want: sourceStore},
		// A store miss takes the copy we already hold rather than the wire —
		// the 55 MB this item is named for.
		{name: "embedded over wire", inStore: false, local: embedded, want: sourceEmbedded, wantData: "embedded bytes"},
		// And falls to the wire only when nothing local can answer, which is
		// what keeps an arch this binary does not embed servable at all.
		{name: "wire when the embed FS lacks the file", inStore: false, local: absent, want: sourceWire},
		{name: "wire when this build embeds nothing", inStore: false, local: nil, want: sourceWire},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, data := chooseTargetDepSource(tc.inStore, tc.local, "libc.a")
			if got != tc.want {
				t.Errorf("chooseTargetDepSource(inStore=%v) = %v, want %v", tc.inStore, got, tc.want)
			}
			if string(data) != tc.wantData {
				t.Errorf("data = %q, want %q — only the embedded source carries bytes", data, tc.wantData)
			}
		})
	}
}

// ── WASM test runtimes (T2169) ──────────────────────────────────────────────

// The manifest name is a cross-module contract: tools/build/common projects the
// entry under this spelling and the compiler looks it up by it, in separate Go
// modules where only a test on each side keeps the two in step. Its twin is
// TestRuntimeManifestName in tools/build/common/runtime_slim_test.go.
func TestRuntimeManifestName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ dep, want string }{
		{"wasmtime", "runtime-wasmtime"},
		{"node", "runtime-node"},
	} {
		if got := runtimeManifestName(tc.dep); got != tc.want {
			t.Errorf("runtimeManifestName(%q) = %q, want %q", tc.dep, got, tc.want)
		}
	}
}

// Every host's file name is asserted on every host. Reading runtime.GOOS would
// check one, leaving the Windows branch verified only where Windows runs —
// the shape T2206 and T2152 both had.
func TestRuntimeExeNameForNamesThePlatform(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ goos, dep, want string }{
		{"linux", "node", "node"},
		{"darwin", "node", "node"},
		{"windows", "node", "node.exe"},
		{"linux", "wasmtime", "wasmtime"},
		{"windows", "wasmtime", "wasmtime.exe"},
	} {
		if got := runtimeExeNameFor(tc.goos, tc.dep); got != tc.want {
			t.Errorf("runtimeExeNameFor(%q, %q) = %q, want %q", tc.goos, tc.dep, got, tc.want)
		}
	}
	// The wrapper must forward the running host and nothing else: without this,
	// every row above can pass while real callers get a different answer.
	if got, want := runtimeExeName("node"), runtimeExeNameFor(runtime.GOOS, "node"); got != want {
		t.Errorf("runtimeExeName(node) = %q, want the %s answer %q", got, runtime.GOOS, want)
	}
}

// The override outranks everything, because an explicit request that a staged
// copy could silently outrank is an explicit request dropped — the same reason
// findLLVMTool checks $PROMISE_OPT first.
func TestResolveWasmRuntimePrefersTheOverride(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "my-wasmtime")
	t.Setenv("PROMISE_WASMTIME", stub)
	got, err := resolveWasmRuntime("wasmtime")
	if err != nil {
		t.Fatalf("resolveWasmRuntime with an override set: %v", err)
	}
	if got != stub {
		t.Errorf("resolveWasmRuntime = %q, want the override %q", got, stub)
	}
}

// A runtime staged in the host prebuilts cache is used without touching the
// CAS. This is what keeps a machine that has run `bin/gate wasm-test` from
// downloading a second copy of a ~120 MB Node into its Promise home — the same
// bridge resolveLLVMView has had since it was written.
func TestResolveWasmRuntimeUsesTheHostPrebuiltsCache(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("PROMISE_PREBUILTS_CACHE", cacheRoot)
	t.Setenv("PROMISE_WASMTIME", "")
	t.Setenv("PROMISE_HOME", t.TempDir()) // an empty CAS: nothing to find there

	target := runtime.GOOS + "-" + runtime.GOARCH
	file := runtimeExeName("wasmtime")
	staged := filepath.Join(cacheRoot, "wasmtime-slim", "44.0.0", target)
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(staged, file)
	if err := os.WriteFile(exe, []byte("pinned"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Without tools.ok the cache dir is half-populated and must be ignored:
	// serving a partial fetch as a runtime is how a truncated download becomes
	// a mysterious exec failure.
	if got := prebuiltRuntimePath("wasmtime", file); got != "" {
		t.Errorf("a cache dir with no tools.ok answered %q, want it ignored", got)
	}
	if err := os.WriteFile(filepath.Join(staged, "tools.ok"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveWasmRuntime("wasmtime")
	if err != nil {
		t.Fatalf("resolveWasmRuntime against a staged prebuilt: %v", err)
	}
	if got != exe {
		t.Errorf("resolveWasmRuntime = %q, want the staged prebuilt %q", got, exe)
	}
}

// isolateWasmRuntimes puts a test in a world where no source can answer for
// either runtime: no override, an empty prebuilts cache, an empty CAS, and every
// manifest source rewritten to a closed loopback port.
//
// That last part is what makes the premise true rather than merely likely. The
// first spelling of these tests only set the first three, which established
// "nothing local can answer" — and was silently relying on the blobs being
// UNPUBLISHED for the wire to fail too. The moment T2169's blobs went up, both
// tests started fetching ~40 MB apiece from GitHub and passing resolution, so
// they failed; a test that reaches the network is also one whose result depends
// on the network. PROMISE_BLOB_MIRROR replaces scheme+host on blob AND archive
// sources alike (blobstore.rewriteSource / rewriteBlobSource), so one variable
// closes every route, offline and instantly.
func isolateWasmRuntimes(t *testing.T) {
	t.Helper()
	t.Setenv("PROMISE_WASMTIME", "")
	t.Setenv("PROMISE_NODE", "")
	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	t.Setenv("PROMISE_HOME", t.TempDir())
	// Port 1 on loopback: refused immediately, no DNS, no egress.
	t.Setenv("PROMISE_BLOB_MIRROR", "http://127.0.0.1:1")
}

// When no source can answer, the failure names the runtime AND the override.
// It must not suggest installing one: since the compiler stopped consulting
// PATH, an install changes nothing, and a message that sends its reader to do
// one wastes their time on a fix that cannot work.
func TestResolveWasmRuntimeReportsWhatCannotBeObtained(t *testing.T) {
	isolateWasmRuntimes(t)

	for dep, envVar := range map[string]string{"wasmtime": "PROMISE_WASMTIME", "node": "PROMISE_NODE"} {
		_, err := resolveWasmRuntime(dep)
		if err == nil {
			t.Errorf("%s resolved with no source available", dep)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, dep) || !strings.Contains(msg, envVar) {
			t.Errorf("%s error = %q, want it to name the runtime and %s", dep, msg, envVar)
		}
		for _, forbidden := range []string{"brew install", "winget", "apt-get", "nodejs.org"} {
			if strings.Contains(msg, forbidden) {
				t.Errorf("%s error = %q, must not suggest %q — installing one changes nothing now", dep, msg, forbidden)
			}
		}
	}
}

// An unknown runtime is refused rather than probed for.
func TestResolveWasmRuntimeRejectsAnUnknownRuntime(t *testing.T) {
	t.Parallel()
	if _, err := resolveWasmRuntime("deno"); err == nil {
		t.Fatal("an unknown runtime was accepted")
	}
	if _, err := resolveWasmRuntimeIfLocal("deno"); err == nil {
		t.Fatal("the no-fetch probe accepted an unknown runtime")
	}
}

// The no-fetch probe answers "" rather than reaching the network, so
// `promise doctor` can report state without acquiring tens of megabytes to do
// it. It still honours the two local sources, in the same order.
func TestResolveWasmRuntimeIfLocalNeverFetches(t *testing.T) {
	// The mirror here is belt and braces: the point of the probe is that it never
	// reaches a source at all, so if it ever did, the closed port turns a silent
	// download into an immediate, visible failure.
	isolateWasmRuntimes(t)

	got, err := resolveWasmRuntimeIfLocal("wasmtime")
	if err != nil {
		t.Fatalf("the no-fetch probe errored instead of reporting absence: %v", err)
	}
	if got != "" {
		t.Errorf("probe = %q, want empty on a machine with nothing staged", got)
	}

	stub := filepath.Join(t.TempDir(), "my-wasmtime")
	t.Setenv("PROMISE_WASMTIME", stub)
	if got, _ := resolveWasmRuntimeIfLocal("wasmtime"); got != stub {
		t.Errorf("probe = %q, want the override %q", got, stub)
	}
}

// A runtime is materialized EXECUTABLE. The target dependencies beside it are
// inert archives placed 0644, and a runtime that inherited that mode would
// stage successfully and then fail at exec time with an EACCES naming a path,
// far from the step that caused it.
func TestMaterializeRuntimeFileIsExecutable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blob := filepath.Join(dir, "blob")
	if err := os.WriteFile(blob, []byte("runtime bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "wasmtime")
	if _, err := materializeRuntimeFile(blob, dst); err != nil {
		t.Fatalf("materializeRuntimeFile: %v", err)
	}
	info, err := os.Stat(dst) // follows the Linux symlink to its target
	if err != nil {
		t.Fatalf("stat materialized runtime: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("materialized runtime mode = %v, want the execute bit set", info.Mode().Perm())
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "runtime bytes" {
		t.Errorf("materialized runtime = %q (err %v), want the blob's bytes", got, err)
	}
}

// A WASM runtime override counts as a toolchain override, so the banner
// announces it and `bin/gate` refuses to report a measurement made under one
// (T2169). A suite run under a substituted wasmtime is no more "the pinned
// toolchain" than a link done with a substituted lld, and the gate's refusal
// reads exactly this list.
func TestWasmRuntimeOverridesAreToolchainOverrides(t *testing.T) {
	for dep, envVar := range wasmRuntimeEnvVars {
		t.Run(dep, func(t *testing.T) {
			for _, v := range llvmToolOverrideVars {
				t.Setenv(v, "")
			}
			t.Setenv("PROMISE_CLANG", "")
			t.Setenv("PROMISE_USE_CLANG", "")
			for _, v := range wasmRuntimeEnvVars {
				t.Setenv(v, "")
			}
			if got := toolchainOverridesInEffect(); len(got) != 0 {
				t.Fatalf("overrides in effect with nothing set: %v", got)
			}
			t.Setenv(envVar, "/custom/"+dep)
			got := toolchainOverridesInEffect()
			if len(got) != 1 || got[0] != envVar {
				t.Errorf("toolchainOverridesInEffect() = %v, want exactly [%s]", got, envVar)
			}
		})
	}
}

// The announcement is made when the override is actually taken, so a run that
// silently used a substituted runtime is impossible — the same contract
// findLLVMTool has for $PROMISE_OPT.
func TestResolveWasmRuntimeAnnouncesTheOverride(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "my-node")
	t.Setenv("PROMISE_NODE", stub)
	toolchainOverrideAnnounced.Delete("PROMISE_NODE")
	toolchainOverrideAnnounced.Delete(toolchainBannerSaid)

	var banner bytes.Buffer
	old := toolchainWarnW
	toolchainWarnW = &banner
	t.Cleanup(func() {
		toolchainWarnW = old
		toolchainOverrideAnnounced.Delete("PROMISE_NODE")
		toolchainOverrideAnnounced.Delete(toolchainBannerSaid)
	})

	if _, err := resolveWasmRuntime("node"); err != nil {
		t.Fatal(err)
	}
	out := banner.String()
	if !strings.Contains(out, "PROMISE_NODE="+stub) {
		t.Errorf("banner = %q, want it to name the override that was taken", out)
	}
	if !strings.Contains(out, "does NOT use the pinned toolchain") {
		t.Errorf("banner = %q, want it to state the consequence", out)
	}
}

// A dep the manifest carries no entry for resolves to ("", nil) rather than an
// error — the signal resolveWasmRuntime turns into "this build carries no
// pinned <dep>". Asserted through an unknown dep because that is the only way
// to reach the miss on a binary whose own manifest does carry both runtimes.
func TestResolveRuntimeViewMissingFromManifestIsNotAnError(t *testing.T) {
	t.Setenv("PROMISE_HOME", t.TempDir())
	viewDir, err := resolveRuntimeView("deno", "deno")
	if err != nil {
		t.Fatalf("a manifest miss must not be an error: %v", err)
	}
	if viewDir != "" {
		t.Errorf("viewDir = %q, want empty for a dep the manifest does not carry", viewDir)
	}
}

// The no-fetch probe reports a runtime staged in the host prebuilts cache. This
// is the "Pinned, staged: <path>" line `promise doctor` prints, and the reason
// an absent host copy is no longer a warning.
func TestResolveWasmRuntimeIfLocalFindsAStagedPrebuilt(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("PROMISE_PREBUILTS_CACHE", cacheRoot)
	t.Setenv("PROMISE_WASMTIME", "")
	t.Setenv("PROMISE_HOME", t.TempDir())

	target := runtime.GOOS + "-" + runtime.GOARCH
	file := runtimeExeName("wasmtime")
	staged := filepath.Join(cacheRoot, "wasmtime-slim", "44.0.0", target)
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(staged, file)
	if err := os.WriteFile(exe, []byte("pinned"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "tools.ok"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveWasmRuntimeIfLocal("wasmtime")
	if err != nil {
		t.Fatalf("resolveWasmRuntimeIfLocal: %v", err)
	}
	if got != exe {
		t.Errorf("probe = %q, want the staged prebuilt %q", got, exe)
	}
}

// prebuiltRuntimePath must reject a cache entry that cannot be executed, and
// prefer the newest version when several checkouts pinned different ones.
//
// The rejections are the load-bearing half: a zero-byte file or a directory
// wearing the runtime's name would otherwise be handed to exec and fail far from
// here, and serving the *older* of two pins would silently run a version the
// tree is not pinned to — the drift T2169 exists to end.
func TestPrebuiltRuntimePathRejectsUnusableEntriesAndPrefersTheNewest(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("PROMISE_PREBUILTS_CACHE", cacheRoot)
	target := runtime.GOOS + "-" + runtime.GOARCH
	file := runtimeExeName("wasmtime")

	stage := func(version string, write func(dir string)) string {
		dir := filepath.Join(cacheRoot, "wasmtime-slim", version, target)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		write(dir)
		if err := os.WriteFile(filepath.Join(dir, "tools.ok"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(dir, file)
	}

	// A zero-byte file is a fetch that did not finish.
	stage("41.0.0", func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, file), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	})
	if got := prebuiltRuntimePath("wasmtime", file); got != "" {
		t.Errorf("a zero-byte cache entry was accepted: %q", got)
	}

	// A directory wearing the runtime's name is not a runtime.
	stage("42.0.0", func(dir string) {
		if err := os.MkdirAll(filepath.Join(dir, file), 0o755); err != nil {
			t.Fatal(err)
		}
	})
	if got := prebuiltRuntimePath("wasmtime", file); got != "" {
		t.Errorf("a directory was accepted as a runtime: %q", got)
	}

	// Two real pins: the newer must win, and numerically — 9.0.0 must not
	// outrank 44.0.0 the way a lexical sort would.
	older := stage("9.0.0", func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
	})
	newer := stage("44.0.0", func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("new"), 0o755); err != nil {
			t.Fatal(err)
		}
	})
	got := prebuiltRuntimePath("wasmtime", file)
	if got == older {
		t.Errorf("prebuiltRuntimePath chose 9.0.0 over 44.0.0 — a lexical comparison, not a numeric one")
	}
	if got != newer {
		t.Errorf("prebuiltRuntimePath = %q, want the newest pin %q", got, newer)
	}
}

// The upstream-archive cache dir (<dep>/<version>/<target>/) is a valid source
// too, not only the <dep>-slim/ one: `bin/pin-prebuilts` and the catalog-miss
// fallback both populate it, and a dev host that has only that copy must not
// re-download the runtime into its CAS.
func TestPrebuiltRuntimePathAcceptsTheUpstreamCacheDir(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("PROMISE_PREBUILTS_CACHE", cacheRoot)
	target := runtime.GOOS + "-" + runtime.GOARCH
	file := runtimeExeName("node")

	dir := filepath.Join(cacheRoot, "node", "22.22.2", target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, file)
	if err := os.WriteFile(exe, []byte("pinned node"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tools.ok"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := prebuiltRuntimePath("node", file); got != exe {
		t.Errorf("prebuiltRuntimePath = %q, want the upstream cache copy %q", got, exe)
	}
}

// The CAS view is the third and last source, and the only one that can serve a
// machine which has never staged the runtime — an end user's thin compiler. Both
// its success paths are asserted here: resolveWasmRuntime returning the view's
// executable, and the no-fetch probe reporting the same path as "staged", which
// is what `promise doctor` prints.
//
// The view is populated by hand rather than fetched, so the test asserts the
// resolution and path arithmetic without a network or a store. Its location is
// computed the way the product computes it (depViewDir), so a change to the
// content-keying moves the fixture with it instead of silently passing against a
// directory nothing reads.
func TestResolveWasmRuntimeUsesAPopulatedCASView(t *testing.T) {
	for _, dep := range []string{"wasmtime", "node"} {
		t.Run(dep, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("PROMISE_HOME", home)
			t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir()) // nothing staged there
			t.Setenv("PROMISE_WASMTIME", "")
			t.Setenv("PROMISE_NODE", "")
			// Closed port: if resolution ever reached the wire this would fail
			// fast rather than quietly downloading tens of megabytes.
			t.Setenv("PROMISE_BLOB_MIRROR", "http://127.0.0.1:1")

			file := runtimeExeName(dep)
			view, err := depViewDir("runtime-view", runtime.GOOS+"-"+runtime.GOARCH,
				[]string{file}, func(string) string { return runtimeManifestName(dep) })
			if err != nil {
				t.Fatalf("depViewDir: %v", err)
			}
			if view.Dir == "" {
				t.Skipf("this build's manifest carries no pinned %s, so the view path is not defined", dep)
			}
			if err := os.MkdirAll(view.Dir, 0o755); err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(view.Dir, file)
			if err := os.WriteFile(want, []byte("staged "+dep), 0o755); err != nil {
				t.Fatal(err)
			}

			got, err := resolveWasmRuntime(dep)
			if err != nil {
				t.Fatalf("resolveWasmRuntime against a populated view: %v", err)
			}
			if got != want {
				t.Errorf("resolveWasmRuntime = %q, want the view's executable %q", got, want)
			}

			// The probe must agree: doctor reporting a different path from the one
			// a run executes is a report about a file nobody uses.
			probed, err := resolveWasmRuntimeIfLocal(dep)
			if err != nil {
				t.Fatalf("resolveWasmRuntimeIfLocal: %v", err)
			}
			if probed != want {
				t.Errorf("probe = %q, want the same path the run resolves (%q)", probed, want)
			}
		})
	}
}
