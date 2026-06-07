package common

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// llvm_slim_test.go covers EnsureLLVMBlobs: the build-tool counterpart of the
// runtime resolver. Tests exercise the slim-fetch happy path, the lock-free
// fast path, the upstream-tarball fallback when blobs.json has no entry for
// the host, brotli corruption rejection, and concurrent fetches.

// countingBlobFetcher tracks how many times each asset is fetched, so the
// concurrency test can prove the per-target flock collapses two simultaneous
// callers into a single download.
type countingBlobFetcher struct {
	mu     sync.Mutex
	assets map[string][]byte
	hits   map[string]*int64
}

func (c *countingBlobFetcher) FetchAsset(tag, asset, dst string) error {
	c.mu.Lock()
	data, ok := c.assets[asset]
	hits := c.hits[asset]
	c.mu.Unlock()
	if !ok {
		return os.ErrNotExist
	}
	if hits != nil {
		atomic.AddInt64(hits, 1)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

// seedSlimCatalog populates blobs.json with both `opt` and `llc` entries
// matching the prebuilts.toml shape produced by fakeReleaseRoot — the catalog
// hit path EnsureLLVMBlobs uses.
func seedSlimCatalog(t *testing.T, root string, contents map[string]string) (sha256ByName, brByName map[string][]byte) {
	t.Helper()
	sha256ByName = map[string][]byte{}
	brByName = map[string][]byte{}
	cat := &BlobsCatalog{Schema: BlobsCatalogSchema}
	for name, content := range contents {
		raw := []byte(content)
		br := brotliBytes(t, raw)
		sha := sha256Hex(raw)
		if err := cat.Upsert(BlobEntry{
			Dependency:       "llvm",
			Version:          "22.1.0",
			Target:           "linux-amd64",
			Name:             name,
			SHA256:           sha,
			Size:             int64(len(raw)),
			Compression:      compressionBrotli,
			CompressedSize:   int64(len(br)),
			CompressedSHA256: sha256Hex(br),
		}); err != nil {
			t.Fatal(err)
		}
		sha256ByName[name] = []byte(sha)
		brByName[sha+".br"] = br
	}
	if err := WriteBlobsCatalog(root, cat); err != nil {
		t.Fatal(err)
	}
	return sha256ByName, brByName
}

func TestEnsureLLVMBlobs_CatalogHit(t *testing.T) {
	root, _ := fakeReleaseRoot(t, nil)
	shas, brs := seedSlimCatalog(t, root, map[string]string{"opt": "OPT_BYTES", "llc": "LLC_BYTES"})

	cacheRoot := t.TempDir()
	t.Setenv("PROMISE_PREBUILTS_CACHE", cacheRoot)
	prev := defaultBlobFetcher
	defaultBlobFetcher = &countingBlobFetcher{assets: brs}
	t.Cleanup(func() { defaultBlobFetcher = prev })

	dir, err := EnsureLLVMBlobs(root, "linux-amd64")
	if err != nil {
		t.Fatalf("EnsureLLVMBlobs: %v", err)
	}
	wantDir := filepath.Join(cacheRoot, "llvm-slim", "22.1.0", "linux-amd64")
	if dir != wantDir {
		t.Errorf("cache dir = %q, want %q", dir, wantDir)
	}
	for name, want := range map[string]string{"opt": "OPT_BYTES", "llc": "LLC_BYTES"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
		// The downloaded `<sha>.br` must be cleaned up after decompression.
		if Exists(filepath.Join(dir, string(shas[name])+".br")) {
			t.Errorf("compressed asset %s.br left behind", name)
		}
	}
	if !Exists(filepath.Join(dir, "tools.ok")) {
		t.Error("tools.ok sentinel missing after successful fetch")
	}
}

func TestEnsureLLVMBlobs_FastPath(t *testing.T) {
	root, _ := fakeReleaseRoot(t, nil)
	_, brs := seedSlimCatalog(t, root, map[string]string{"opt": "OPT", "llc": "LLC"})

	cacheRoot := t.TempDir()
	t.Setenv("PROMISE_PREBUILTS_CACHE", cacheRoot)

	hits := map[string]*int64{}
	for k := range brs {
		var n int64
		hits[k] = &n
	}
	prev := defaultBlobFetcher
	defaultBlobFetcher = &countingBlobFetcher{assets: brs, hits: hits}
	t.Cleanup(func() { defaultBlobFetcher = prev })

	// First call populates the cache.
	if _, err := EnsureLLVMBlobs(root, "linux-amd64"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	for k, n := range hits {
		if atomic.LoadInt64(n) != 1 {
			t.Fatalf("first call: %s fetched %d times, want 1", k, atomic.LoadInt64(n))
		}
	}
	// Second call must short-circuit on tools.ok.
	if _, err := EnsureLLVMBlobs(root, "linux-amd64"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	for k, n := range hits {
		if got := atomic.LoadInt64(n); got != 1 {
			t.Errorf("second call: %s fetched %d times, want still 1 (fast path failed)", k, got)
		}
	}
}

func TestEnsureLLVMBlobs_CatalogMiss_FallsThroughToTarball(t *testing.T) {
	// Don't seed blobs.json → every Lookup misses → fall through to FetchPrebuilt.
	// fakeReleaseRoot's prebuilts.toml URL points at example.test, so we need
	// to provide a real archive over httptest for FetchPrebuilt to succeed.
	root, _ := fakeReleaseRoot(t, nil)
	// Override the prebuilts URL to a local test server.
	tarBytes, err := makeTarGzContent(map[string]string{"bin/opt": "OPT_TAR", "bin/llc": "LLC_TAR"})
	if err != nil {
		t.Fatal(err)
	}
	tarSHA := sha256Hex(tarBytes)
	// Rewrite prebuilts.toml so the upstream archive points at our stub.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarBytes)
	}))
	t.Cleanup(srv.Close)
	prebuilts := `schema = 1
[binaries.llvm]
version = "22.1.0"
bundle_dir = "compiler/cmd/promise/resources/llvm"
[binaries.llvm.targets.linux-amd64]
url = "` + srv.URL + `/llvm.tar.gz"
sha256 = "` + tarSHA + `"
files = [
  { src = "bin/opt", out = "opt" },
  { src = "bin/llc", out = "llc" },
]
`
	if err := os.WriteFile(filepath.Join(root, "tools", "build", "prebuilts.toml"), []byte(prebuilts), 0o644); err != nil {
		t.Fatal(err)
	}

	cacheRoot := t.TempDir()
	t.Setenv("PROMISE_PREBUILTS_CACHE", cacheRoot)

	dir, err := EnsureLLVMBlobs(root, "linux-amd64")
	if err != nil {
		t.Fatalf("EnsureLLVMBlobs (tarball fallback): %v", err)
	}
	wantTarPath := filepath.Join(cacheRoot, "llvm", "22.1.0", "linux-amd64")
	if dir != wantTarPath {
		t.Errorf("fallback returned %q, want the upstream cache dir %q", dir, wantTarPath)
	}
	// The slim cache dir must NOT exist after a tarball fallback (the fall-
	// through happens before any llvm-slim directory is touched).
	if Exists(filepath.Join(cacheRoot, "llvm-slim")) {
		t.Error("llvm-slim/ should not be created when falling back to the tarball")
	}
	got, err := os.ReadFile(filepath.Join(dir, "opt"))
	if err != nil || string(got) != "OPT_TAR" {
		t.Errorf("opt in tarball cache = %q, want OPT_TAR (err=%v)", got, err)
	}
}

func TestEnsureLLVMBlobs_BrotliCorrupt_RejectsAndErrors(t *testing.T) {
	root, _ := fakeReleaseRoot(t, nil)
	shas, _ := seedSlimCatalog(t, root, map[string]string{"opt": "OPT", "llc": "LLC"})

	cacheRoot := t.TempDir()
	t.Setenv("PROMISE_PREBUILTS_CACHE", cacheRoot)

	// Serve invalid brotli bytes for every catalog asset.
	bad := map[string][]byte{
		string(shas["opt"]) + ".br": []byte("not brotli at all"),
		string(shas["llc"]) + ".br": []byte("also garbage"),
	}
	prev := defaultBlobFetcher
	defaultBlobFetcher = &countingBlobFetcher{assets: bad}
	t.Cleanup(func() { defaultBlobFetcher = prev })

	if _, err := EnsureLLVMBlobs(root, "linux-amd64"); err == nil {
		t.Fatal("expected EnsureLLVMBlobs to reject a garbage brotli blob")
	}
	// tools.ok must NOT be written so the next attempt re-fetches instead of
	// pretending the cache is valid.
	if Exists(filepath.Join(cacheRoot, "llvm-slim", "22.1.0", "linux-amd64", "tools.ok")) {
		t.Error("tools.ok was written despite the brotli corruption — the cache is now silently broken")
	}
}

func TestEnsureLLVMBlobs_Concurrent(t *testing.T) {
	root, _ := fakeReleaseRoot(t, nil)
	_, brs := seedSlimCatalog(t, root, map[string]string{"opt": "OPT", "llc": "LLC"})

	cacheRoot := t.TempDir()
	t.Setenv("PROMISE_PREBUILTS_CACHE", cacheRoot)

	hits := map[string]*int64{}
	for k := range brs {
		var n int64
		hits[k] = &n
	}
	prev := defaultBlobFetcher
	defaultBlobFetcher = &countingBlobFetcher{assets: brs, hits: hits}
	t.Cleanup(func() { defaultBlobFetcher = prev })

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = EnsureLLVMBlobs(root, "linux-amd64")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	// Both callers should observe a single download per blob — the per-target
	// flock serializes the slow path, and the second caller hits the
	// post-lock re-check.
	for k, n := range hits {
		if got := atomic.LoadInt64(n); got != 1 {
			t.Errorf("blob %s fetched %d times concurrently, want 1 (flock or re-check broken)", k, got)
		}
	}
}

// TestEnsureLLVMBlobs_UnsupportedTarget covers the prebuilts.toml-side error
// path — an explicit Unsupported target must fail loudly rather than silently
// short-circuit. fakeReleaseRoot doesn't include darwin-amd64 by default, so
// we extend the prebuilts.toml with an unsupported entry first.
func TestEnsureLLVMBlobs_UnsupportedTarget(t *testing.T) {
	root, _ := fakeReleaseRoot(t, nil)
	path := filepath.Join(root, "tools", "build", "prebuilts.toml")
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	addition := "\n[binaries.llvm.targets.darwin-amd64]\nunsupported = \"no upstream LLVM for this host\"\n"
	if err := os.WriteFile(path, append(cur, []byte(addition)...), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	_, err = EnsureLLVMBlobs(root, "darwin-amd64")
	if err == nil {
		t.Fatal("expected error for an unsupported target")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error should mention not supported, got: %v", err)
	}
}

// TestEnsureLLVMBlobs_MissingPrebuiltsManifest covers the error path when
// LoadPrebuiltsManifest fails (file missing). Without this guard, a busted
// build tree would surface as a baffling later error in the manifest-Lookup
// code; the explicit "load prebuilts manifest" wrap keeps the chain readable.
func TestEnsureLLVMBlobs_MissingPrebuiltsManifest(t *testing.T) {
	root := t.TempDir() // no tools/build/prebuilts.toml
	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	_, err := EnsureLLVMBlobs(root, "linux-amd64")
	if err == nil {
		t.Fatal("expected error when prebuilts.toml is missing")
	}
	if !strings.Contains(err.Error(), "load prebuilts manifest") {
		t.Errorf("error should be wrapped as 'load prebuilts manifest', got: %v", err)
	}
}

// TestEnsureLLVMBlobs_MissingTargetEntry covers the case where [binaries.llvm]
// exists but lacks an entry for the requested target. This is the failure mode
// for a brand-new host that hasn't been added to prebuilts.toml yet.
func TestEnsureLLVMBlobs_MissingTargetEntry(t *testing.T) {
	root, _ := fakeReleaseRoot(t, nil) // has linux-amd64 only
	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	_, err := EnsureLLVMBlobs(root, "freebsd-amd64") // not in prebuilts.toml
	if err == nil {
		t.Fatal("expected error for a target with no prebuilts entry")
	}
	if !strings.Contains(err.Error(), "no [binaries.llvm.targets.freebsd-amd64]") {
		t.Errorf("error should name the missing target table, got: %v", err)
	}
}

// TestEnsureLLVMBlobs_MalformedCatalog covers LoadBlobsCatalog failure: an
// existing but unparseable blobs.json must surface a clean error rather than
// pretend the catalog is empty (which would silently fall through to the
// upstream tarball, defeating the slim-blob acceptance criterion).
func TestEnsureLLVMBlobs_MalformedCatalog(t *testing.T) {
	root, _ := fakeReleaseRoot(t, nil)
	if err := os.WriteFile(filepath.Join(root, "tools", "build", "blobs.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())
	_, err := EnsureLLVMBlobs(root, "linux-amd64")
	if err == nil {
		t.Fatal("expected error for malformed blobs.json")
	}
}

// TestCachedSlimDigestOK_DigestMismatch covers the digest-version-change
// branch in cachedSlimDigestOK: tools.ok exists and the output files are
// present, but the stored digest doesn't match the expected one (e.g. the
// catalog was bumped while the cache wasn't cleared). Must return false so
// the slow re-fetch path runs.
func TestCachedSlimDigestOK_DigestMismatch(t *testing.T) {
	cacheDir := t.TempDir()
	plan := []slimPlanFile{{Out: "opt", BE: &BlobEntry{SHA256: "deadbeef", Size: 3, Compression: compressionBrotli}}}
	if err := os.WriteFile(filepath.Join(cacheDir, "opt"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a stale digest.
	if err := os.WriteFile(filepath.Join(cacheDir, toolsOKFile), []byte("stale-digest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cachedSlimDigestOK(cacheDir, "fresh-digest", plan) {
		t.Error("expected false when stored digest != expected digest")
	}
}

// TestCachedSlimDigestOK_MissingPlanFile covers the post-digest check: even
// when tools.ok matches the digest, a missing output file must invalidate
// the cache (e.g. someone manually `rm`-ed `opt` from the cache dir).
func TestCachedSlimDigestOK_MissingPlanFile(t *testing.T) {
	cacheDir := t.TempDir()
	plan := []slimPlanFile{{Out: "opt", BE: &BlobEntry{SHA256: "deadbeef", Size: 3, Compression: compressionBrotli}}}
	digest := slimToolsDigest("22.1.0", "linux-amd64", plan)
	if err := os.WriteFile(filepath.Join(cacheDir, toolsOKFile), []byte(digest+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// tools.ok matches, but the planned `opt` file is absent.
	if cachedSlimDigestOK(cacheDir, digest, plan) {
		t.Error("expected false when a planned output file is missing")
	}
}

// TestCachedSlimDigestOK_NoToolsOk covers the bottom-of-funnel case: no
// tools.ok at all (fresh cache directory).
func TestCachedSlimDigestOK_NoToolsOk(t *testing.T) {
	cacheDir := t.TempDir()
	plan := []slimPlanFile{{Out: "opt", BE: &BlobEntry{SHA256: "deadbeef", Size: 3, Compression: compressionBrotli}}}
	if cachedSlimDigestOK(cacheDir, "any-digest", plan) {
		t.Error("expected false when tools.ok is absent")
	}
}

// TestPatchAndSignMachO_Noop_OnNonDarwin pins the non-darwin build tag stub
// to a no-op: it must be safe to call on a path that doesn't exist and must
// never panic, since callers (e.g. EnsureLLVMBlobs) unconditionally call it
// inside the darwin-target branch and the build tag is the only guard.
// On darwin hosts this test is a smoke test for the real implementation
// against a missing file — install_name_tool/codesign tolerate that, and the
// function must still return.
func TestPatchAndSignMachO_Noop_OnNonDarwin(t *testing.T) {
	// Intentionally pass a path that doesn't exist — the no-op must not crash;
	// the real darwin impl is tolerant by design.
	patchAndSignMachO("/nonexistent/path/to/binary")
	// And empty string.
	patchAndSignMachO("")
}
