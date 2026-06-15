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

	"github.com/andybalholm/brotli"
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

// TestUnbrotliBytesRoundTrip verifies unbrotliBytes decompresses what brotli
// produces and rejects non-brotli input.
func TestUnbrotliBytesRoundTrip(t *testing.T) {
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
	}
	return names
}

// TestViewPublishAtomic verifies publishViewDir is all-or-nothing: on success the
// viewDir contains exactly the populated files and no .tmp-* sibling lingers; on a
// populate error the viewDir is never created and the temp dir is cleaned up.
func TestViewPublishAtomic(t *testing.T) {
	parent := t.TempDir()
	viewDir := filepath.Join(parent, "view")

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
	// No temp-dir residue should remain in the parent.
	assertNoTmpResidue(t, parent, viewDir)

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

// TestViewPublishRenameFailure verifies the publish-failure branch: when the
// final rename(2) cannot complete (here the destination's parent dir does not
// exist), publishViewDir returns a wrapped "publish view" error AND still removes
// the staged temp dir — a failed publish must never leak a half-built sibling.
func TestViewPublishRenameFailure(t *testing.T) {
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
