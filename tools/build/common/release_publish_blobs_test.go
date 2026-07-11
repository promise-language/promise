package common

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── publish-blobs ───────────────────────────────────────────────────────────

// stubReleaseUploader records EnsureRelease/UploadAsset calls and answers
// ListAssets from an in-memory set. Lets tests cover the publish-blobs flow
// without shelling out to `gh`.
type stubReleaseUploader struct {
	mu             sync.Mutex
	ensuredTags    []string
	ensuredTitles  map[string]string
	uploadedAssets map[string][]string // tag → asset basenames
	hosted         map[string]map[string]bool
}

func newStubReleaseUploader() *stubReleaseUploader {
	return &stubReleaseUploader{
		ensuredTitles:  map[string]string{},
		uploadedAssets: map[string][]string{},
		hosted:         map[string]map[string]bool{},
	}
}

func (s *stubReleaseUploader) EnsureRelease(tag, title, notes string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensuredTags = append(s.ensuredTags, tag)
	s.ensuredTitles[tag] = title
	return nil
}

func (s *stubReleaseUploader) ListAssets(tag string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hosted[tag] == nil {
		return nil, nil
	}
	var out []string
	for k := range s.hosted[tag] {
		out = append(out, k)
	}
	return out, nil
}

func (s *stubReleaseUploader) UploadAsset(tag, localPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := filepath.Base(localPath)
	s.uploadedAssets[tag] = append(s.uploadedAssets[tag], base)
	if s.hosted[tag] == nil {
		s.hosted[tag] = map[string]bool{}
	}
	s.hosted[tag][base] = true
	return nil
}

// stubBlobMirror records R2 Put calls so the --r2-bucket path is exercised
// without shelling out to `npx wrangler`.
type stubBlobMirror struct {
	mu   sync.Mutex
	puts map[string]string // R2 key → uploaded file basename
	// getContent simulates objects already in the bucket: R2 key → file content.
	// A Get for a key not present returns found=false (the "start fresh" path).
	getContent map[string]string
}

func (s *stubBlobMirror) Put(key, localPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.puts == nil {
		s.puts = map[string]string{}
	}
	s.puts[key] = filepath.Base(localPath)
	return nil
}

func (s *stubBlobMirror) Get(key, localPath string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.getContent[key]
	if !ok {
		return false, nil
	}
	if err := os.WriteFile(localPath, []byte(content), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// withStubUploader swaps the package-level GitHub uploader for the duration of
// the test, restoring it on cleanup. It ALSO stubs the R2 mirror: --r2-bucket
// defaults to "prebuilts", so without this an upload test would shell out to
// `npx wrangler`. Tests asserting R2 behavior install their own mirror stub on
// top (LIFO cleanup restores this one first).
func withStubUploader(t *testing.T) *stubReleaseUploader {
	t.Helper()
	stub := newStubReleaseUploader()
	prev := defaultReleaseUploader
	defaultReleaseUploader = stub
	t.Cleanup(func() { defaultReleaseUploader = prev })
	prevMirror := newBlobMirror
	newBlobMirror = func(string) blobMirror { return &stubBlobMirror{} }
	t.Cleanup(func() { newBlobMirror = prevMirror })
	return stub
}

// publishBlobsTestRoot builds a temp repo root with prebuilts.toml + a fake
// PrebuiltsCacheRoot containing the extracted blobs (bypassing FetchPrebuilt's
// network step). Returns the root + the env override needed to redirect the
// prebuilts cache + the {out → uncompressed sha} map.
func publishBlobsTestRoot(t *testing.T, blobs map[string]string) (root string, shas map[string]string) {
	t.Helper()
	root, _ = fakeReleaseRoot(t, blobs)
	// The cache must be laid out the way FetchPrebuilt would: <cache>/<dep>/
	// <version>/<target>/<files>.
	cacheRoot := filepath.Join(root, "prebuilts-cache")
	cacheDir := filepath.Join(cacheRoot, "llvm", "22.1.0", "linux-amd64")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shas = map[string]string{}
	for name, content := range blobs {
		dst := filepath.Join(cacheDir, name)
		if err := os.WriteFile(dst, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		shas[name] = sha256Hex([]byte(content))
	}
	// Write the cache sentinels (archive.ok + tools.ok) so FetchPrebuilt's
	// fast path skips the network entirely.
	digest := manifestToolsDigest(&TargetEntry{
		SHA256: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0",
		Files: []PrebuiltFile{
			{Src: "bin/opt", Out: "opt"},
			{Src: "bin/llc", Out: "llc"},
		},
	})
	if err := os.WriteFile(filepath.Join(cacheDir, "tools.ok"), []byte(digest+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROMISE_PREBUILTS_CACHE", cacheRoot)
	return root, shas
}

func TestPublishBlobsHappyPath(t *testing.T) {
	stub := withStubUploader(t)
	root, shas := publishBlobsTestRoot(t, map[string]string{"opt": "OPT_BYTES", "llc": "LLC_BYTES"})

	if err := runReleasePublishBlobs(root, []string{"--host", "linux-amd64"}); err != nil {
		t.Fatalf("publish-blobs: %v", err)
	}

	// Catalog now has 2 entries (one per file).
	cat, err := LoadBlobsCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Blobs) != 2 {
		t.Fatalf("catalog has %d blobs, want 2", len(cat.Blobs))
	}
	tag := DepsReleaseTag("llvm", "22.1.0")
	for name, sha := range shas {
		be, ok := cat.Lookup("llvm", "22.1.0", "linux-amd64", name)
		if !ok {
			t.Fatalf("catalog missing entry for %s", name)
		}
		if be.SHA256 != sha {
			t.Errorf("%s sha = %s, want %s", name, be.SHA256, sha)
		}
		if be.Compression != compressionBrotli {
			t.Errorf("%s compression = %q", name, be.Compression)
		}
		if be.Size <= 0 || be.CompressedSize <= 0 || be.CompressedSHA256 == "" {
			t.Errorf("%s metadata incomplete: %+v", name, be)
		}
		// Source provenance is populated (T0836).
		if be.Source == nil {
			t.Errorf("%s: Source is nil — expected provenance to be populated", name)
		} else {
			wantURL := "https://example.test/LLVM-22.1.0-Linux-X64.tar.xz"
			if be.Source.ArchiveURL != wantURL {
				t.Errorf("%s: Source.ArchiveURL = %q, want %q", name, be.Source.ArchiveURL, wantURL)
			}
			wantSHA := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0"
			if be.Source.ArchiveSHA256 != wantSHA {
				t.Errorf("%s: Source.ArchiveSHA256 = %q, want %q", name, be.Source.ArchiveSHA256, wantSHA)
			}
			wantMember := map[string]string{"opt": "bin/opt", "llc": "bin/llc"}
			if got := be.Source.Member; got != wantMember[name] {
				t.Errorf("%s: Source.Member = %q, want %q", name, got, wantMember[name])
			}
		}
		// Upload happened with the asset name `<sha>.br`.
		want := sha + ".br"
		if !sliceContains(stub.uploadedAssets[tag], want) {
			t.Errorf("expected upload of %s to %s, got: %v", want, tag, stub.uploadedAssets[tag])
		}
	}
	if len(stub.ensuredTags) == 0 {
		t.Error("EnsureRelease was never called")
	}
}

func TestPublishBlobsIdempotent(t *testing.T) {
	stub := withStubUploader(t)
	root, _ := publishBlobsTestRoot(t, map[string]string{"opt": "OPT", "llc": "LLC"})

	// First run uploads.
	if err := runReleasePublishBlobs(root, []string{"--host", "linux-amd64"}); err != nil {
		t.Fatal(err)
	}
	firstUploads := len(stub.uploadedAssets[DepsReleaseTag("llvm", "22.1.0")])
	if firstUploads != 2 {
		t.Fatalf("first run uploaded %d, want 2", firstUploads)
	}

	// Second run: catalog hits + assets hosted → no new uploads.
	if err := runReleasePublishBlobs(root, []string{"--host", "linux-amd64"}); err != nil {
		t.Fatal(err)
	}
	if got := len(stub.uploadedAssets[DepsReleaseTag("llvm", "22.1.0")]); got != firstUploads {
		t.Fatalf("second run uploaded %d, want still %d (dedup failed)", got, firstUploads)
	}
}

func TestPublishBlobsNoUpload(t *testing.T) {
	stub := withStubUploader(t)
	root, _ := publishBlobsTestRoot(t, map[string]string{"opt": "OPT", "llc": "LLC"})
	if err := runReleasePublishBlobs(root, []string{"--host", "linux-amd64", "--no-upload"}); err != nil {
		t.Fatal(err)
	}
	// Catalog updated but no upload.
	cat, err := LoadBlobsCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Blobs) == 0 {
		t.Fatal("expected catalog write under --no-upload")
	}
	if len(stub.uploadedAssets) != 0 || len(stub.ensuredTags) != 0 {
		t.Fatalf("--no-upload still talked to gh: ensures=%v uploads=%v", stub.ensuredTags, stub.uploadedAssets)
	}
	// Source provenance must be populated even when --no-upload skips the gh
	// upload step — the catalog write happens before the upload decision (T0836).
	for _, be := range cat.Blobs {
		if be.Source == nil {
			t.Errorf("--no-upload: Source is nil for %s — provenance must be set regardless of upload mode", be.Name)
		} else if be.Source.ArchiveURL == "" || be.Source.ArchiveSHA256 == "" || be.Source.Member == "" {
			t.Errorf("--no-upload: Source has empty fields for %s: %+v", be.Name, be.Source)
		}
	}
}

func TestPublishBlobsDryRun(t *testing.T) {
	stub := withStubUploader(t)
	root, _ := publishBlobsTestRoot(t, map[string]string{"opt": "OPT", "llc": "LLC"})
	if err := runReleasePublishBlobs(root, []string{"--host", "linux-amd64", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	// --dry-run writes nothing.
	if Exists(blobsCatalogPath(root)) {
		t.Fatal("--dry-run must not write blobs.json")
	}
	if len(stub.uploadedAssets) != 0 {
		t.Fatalf("--dry-run uploaded: %v", stub.uploadedAssets)
	}
}

func TestPublishBlobsRequiresHost(t *testing.T) {
	_ = withStubUploader(t)
	root, _ := fakeReleaseRoot(t, map[string]string{"opt": "OPT"})
	if err := runReleasePublishBlobs(root, []string{}); err == nil {
		t.Fatal("expected error when --host is missing")
	}
}

func TestPublishBlobsBadDependency(t *testing.T) {
	_ = withStubUploader(t)
	root, _ := fakeReleaseRoot(t, map[string]string{"opt": "OPT"})
	if err := runReleasePublishBlobs(root, []string{"--host", "linux-amd64", "--dependency", "not-a-thing"}); err == nil {
		t.Fatal("expected error for unknown dependency")
	}
}

// TestPublishBlobsBadTarget exercises the "dependency exists, target does not"
// branch — distinct from BadDependency and not exercised by the happy-path
// tests, which only use linux-amd64.
func TestPublishBlobsBadTarget(t *testing.T) {
	_ = withStubUploader(t)
	root, _ := fakeReleaseRoot(t, map[string]string{"opt": "OPT"})
	err := runReleasePublishBlobs(root, []string{"--host", "freebsd-amd64", "--dependency", "llvm"})
	if err == nil {
		t.Fatal("expected error for unknown target")
	}
	if !strings.Contains(err.Error(), "freebsd-amd64") {
		t.Fatalf("error should name the missing target, got: %v", err)
	}
}

// TestPublishBlobsUnsupportedTarget pins the Unsupported-target branch. The
// prebuilts.toml shape supports declaring an unbuildable target (set
// Unsupported = "<reason>"); publish-blobs must refuse it instead of attempting
// to fetch a non-existent archive.
func TestPublishBlobsUnsupportedTarget(t *testing.T) {
	_ = withStubUploader(t)
	root, _ := fakeReleaseRoot(t, map[string]string{"opt": "OPT"})
	// Append an unsupported target to the existing prebuilts.toml.
	prebuiltsPath := filepath.Join(root, "tools", "build", "prebuilts.toml")
	cur, err := os.ReadFile(prebuiltsPath)
	if err != nil {
		t.Fatal(err)
	}
	addition := "\n[binaries.llvm.targets.linux-arm64]\nunsupported = \"deferred — see docs/release-automation.md\"\n"
	if err := os.WriteFile(prebuiltsPath, append(cur, []byte(addition)...), 0o644); err != nil {
		t.Fatal(err)
	}
	err = runReleasePublishBlobs(root, []string{"--host", "linux-arm64", "--dependency", "llvm"})
	if err == nil {
		t.Fatal("expected error for unsupported target")
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("error should mention unsupported, got: %v", err)
	}
}

// TestPublishBlobsBadFlag rejects an unknown CLI flag before any side effects.
// The flag.Parse failure path is otherwise unreachable from happy-path tests.
func TestPublishBlobsBadFlag(t *testing.T) {
	_ = withStubUploader(t)
	root, _ := fakeReleaseRoot(t, map[string]string{"opt": "OPT"})
	if err := runReleasePublishBlobs(root, []string{"--nonsense"}); err == nil {
		t.Fatal("expected flag.Parse error on an unknown flag")
	}
}

// errReleaseUploader fails each method, exercising the publish-blobs error
// surface that the friendly stub never triggers.
type errReleaseUploader struct {
	failList   bool
	failEnsure bool
	failUpload bool
}

func (e *errReleaseUploader) EnsureRelease(tag, title, notes string) error {
	if e.failEnsure {
		return fmt.Errorf("simulated EnsureRelease failure for %s", tag)
	}
	return nil
}

func (e *errReleaseUploader) ListAssets(tag string) ([]string, error) {
	if e.failList {
		return nil, fmt.Errorf("simulated ListAssets failure for %s", tag)
	}
	return nil, nil
}

func (e *errReleaseUploader) UploadAsset(tag, localPath string) error {
	if e.failUpload {
		return fmt.Errorf("simulated UploadAsset failure for %s/%s", tag, filepath.Base(localPath))
	}
	return nil
}

// TestPublishBlobsListAssetsError pins the early-exit when `gh release view`
// fails for a reason other than the unknown-tag case the production uploader
// silently swallows. Without this branch covered, a transient gh failure would
// surface as a "wrote no blobs" success instead of a hard error.
func TestPublishBlobsListAssetsError(t *testing.T) {
	prev := defaultReleaseUploader
	defaultReleaseUploader = &errReleaseUploader{failList: true}
	t.Cleanup(func() { defaultReleaseUploader = prev })

	root, _ := publishBlobsTestRoot(t, map[string]string{"opt": "OPT"})
	err := runReleasePublishBlobs(root, []string{"--host", "linux-amd64"})
	if err == nil {
		t.Fatal("expected publish-blobs to surface a ListAssets failure")
	}
	if !strings.Contains(err.Error(), "list assets") {
		t.Fatalf("error should mention list assets, got: %v", err)
	}
}

// TestPublishBlobsEnsureReleaseError pins the EnsureRelease failure path: a
// half-published deps release (some assets uploaded, others not) is recoverable
// on a re-run, but only if EnsureRelease's failure short-circuits before any
// UploadAsset attempts.
func TestPublishBlobsEnsureReleaseError(t *testing.T) {
	prev := defaultReleaseUploader
	defaultReleaseUploader = &errReleaseUploader{failEnsure: true}
	t.Cleanup(func() { defaultReleaseUploader = prev })

	root, _ := publishBlobsTestRoot(t, map[string]string{"opt": "OPT"})
	err := runReleasePublishBlobs(root, []string{"--host", "linux-amd64"})
	if err == nil {
		t.Fatal("expected publish-blobs to fail when EnsureRelease errors")
	}
	if !strings.Contains(err.Error(), "EnsureRelease") {
		t.Fatalf("error should reference EnsureRelease, got: %v", err)
	}
}

// TestPublishBlobsUploadAssetError pins UploadAsset failure: a failure on any
// asset must abort the run so the maintainer notices, instead of partially
// uploading and reporting success.
func TestPublishBlobsUploadAssetError(t *testing.T) {
	prev := defaultReleaseUploader
	defaultReleaseUploader = &errReleaseUploader{failUpload: true}
	t.Cleanup(func() { defaultReleaseUploader = prev })

	root, _ := publishBlobsTestRoot(t, map[string]string{"opt": "OPT"})
	err := runReleasePublishBlobs(root, []string{"--host", "linux-amd64"})
	if err == nil {
		t.Fatal("expected publish-blobs to fail when UploadAsset errors")
	}
	if !strings.Contains(err.Error(), "UploadAsset") {
		t.Fatalf("error should reference UploadAsset, got: %v", err)
	}
}

// TestGhCLIUploaderAlreadyExists pins the idempotency fix: when `gh release
// upload` exits non-zero with "asset under the same name already exists",
// UploadAsset must return nil (same-name content-addressed asset = same bytes
// = genuine no-op). Without the fix it would propagate the error and abort
// the whole publish-blobs run, leaving R2 out of sync.
func TestGhCLIUploaderAlreadyExists(t *testing.T) {
	writeFakeGH(t, "#!/bin/sh\necho 'asset under the same name already exists: [sha.br]' >&2\nexit 1\n")
	if err := (ghCLIUploader{}).UploadAsset("some-tag", "/tmp/sha.br"); err != nil {
		t.Fatalf("UploadAsset must return nil for 'already exists', got: %v", err)
	}
}

// TestPublishBlobsGHAlreadyExistsStillMirrorsR2 is the end-to-end regression
// test for T0834: when GitHub returns "asset under the same name already
// exists", the whole publish-blobs run must still succeed AND the R2 mirror
// Put must still be called for every blob. Before the fix, the error aborted
// the loop before mirror.Put was reached.
func TestPublishBlobsGHAlreadyExistsStillMirrorsR2(t *testing.T) {
	// Fake gh: view exits 0 with empty output (so ListAssets returns no assets
	// → blob enters the upload path), upload exits 1 with "already exists".
	writeFakeGH(t, "#!/bin/sh\n"+
		"if [ \"$1\" = \"release\" ]; then\n"+
		"  case \"$2\" in\n"+
		"    view)   echo \"\"; exit 0;;\n"+
		"    upload) echo \"asset under the same name already exists\" >&2; exit 1;;\n"+
		"  esac\n"+
		"fi\n"+
		"exit 0\n")

	prev := defaultReleaseUploader
	defaultReleaseUploader = &ghCLIUploader{}
	t.Cleanup(func() { defaultReleaseUploader = prev })

	mirror := &stubBlobMirror{}
	prevMirror := newBlobMirror
	newBlobMirror = func(string) blobMirror { return mirror }
	t.Cleanup(func() { newBlobMirror = prevMirror })

	root, shas := publishBlobsTestRoot(t, map[string]string{"opt": "OPT_BYTES", "llc": "LLC_BYTES"})
	if err := runReleasePublishBlobs(root, []string{"--host", "linux-amd64"}); err != nil {
		t.Fatalf("publish-blobs must succeed when GitHub returns 'already exists': %v", err)
	}
	for _, sha := range shas {
		asset := sha + ".br"
		if mirror.puts[asset] != asset {
			t.Errorf("R2 mirror Put not called for %q (puts=%v)", asset, mirror.puts)
		}
	}
}

// TestGhCLIUploaderSuccess exercises the success path of ghCLIUploader.UploadAsset:
// when `gh release upload` exits 0 the function must return nil.
func TestGhCLIUploaderSuccess(t *testing.T) {
	writeFakeGH(t, "#!/bin/sh\nexit 0\n")
	if err := (ghCLIUploader{}).UploadAsset("some-tag", "/tmp/sha.br"); err != nil {
		t.Fatalf("UploadAsset must return nil on success, got: %v", err)
	}
}

// TestGhCLIUploaderRealError exercises the non-idempotent error path: when `gh
// release upload` exits non-zero with an error that is NOT "already exists"
// (e.g. a network failure), UploadAsset must propagate the error so the caller
// notices the partial publish.
func TestGhCLIUploaderRealError(t *testing.T) {
	writeFakeGH(t, "#!/bin/sh\necho 'network timeout' >&2\nexit 1\n")
	err := (ghCLIUploader{}).UploadAsset("some-tag", "/tmp/sha.br")
	if err == nil {
		t.Fatal("UploadAsset must return an error for a real gh failure")
	}
	if !strings.Contains(err.Error(), "gh release upload") {
		t.Fatalf("error should mention 'gh release upload', got: %v", err)
	}
}

// errBlobMirror is a blobMirror stub whose Put always fails, to exercise the
// R2 mirror error return in runReleasePublishBlobs.
type errBlobMirror struct{}

func (errBlobMirror) Put(key, localPath string) error {
	return fmt.Errorf("simulated R2 Put failure for %s", key)
}
func (errBlobMirror) Get(key, localPath string) (bool, error) { return false, nil }

// TestPublishBlobsR2PutError pins that a mirror.Put failure aborts the run and
// surfaces a hard error — the caller needs to know the mirrors are out of sync
// so they can retry, instead of silently succeeding with R2 missing the blob.
func TestPublishBlobsR2PutError(t *testing.T) {
	_ = withStubUploader(t) // stubs gh uploader (succeeds) + default mirror
	// Override the mirror with one that always fails.
	prevMirror := newBlobMirror
	newBlobMirror = func(string) blobMirror { return errBlobMirror{} }
	t.Cleanup(func() { newBlobMirror = prevMirror })

	root, _ := publishBlobsTestRoot(t, map[string]string{"opt": "OPT", "llc": "LLC"})
	err := runReleasePublishBlobs(root, []string{"--host", "linux-amd64"})
	if err == nil {
		t.Fatal("expected publish-blobs to fail when R2 mirror.Put errors")
	}
	if !strings.Contains(err.Error(), "R2 Put") {
		t.Fatalf("error should mention 'R2 Put', got: %v", err)
	}
}

// TestPublishBlobsR2Mirror pins the --r2-bucket path (defaulting to "prebuilts"):
// each blob uploaded to the GitHub deps release is ALSO mirrored to R2 as a FLAT
// CAS object keyed by <sha>.br (no path), which is what the resolver's flat blob
// mirror (rewriteBlobSource) fetches. With no --r2-bucket on the args, the
// default must apply.
func TestPublishBlobsR2Mirror(t *testing.T) {
	stub := withStubUploader(t) // also stubs the mirror; we override it below to capture puts
	mirror := &stubBlobMirror{}
	var gotBucket string
	prevMirror := newBlobMirror
	newBlobMirror = func(bucket string) blobMirror { gotBucket = bucket; return mirror }
	t.Cleanup(func() { newBlobMirror = prevMirror })

	root, shas := publishBlobsTestRoot(t, map[string]string{"opt": "OPT_BYTES", "llc": "LLC_BYTES"})
	if err := runReleasePublishBlobs(root, []string{"--host", "linux-amd64"}); err != nil {
		t.Fatalf("publish-blobs: %v", err)
	}
	if gotBucket != "prebuilts" {
		t.Fatalf("default R2 bucket = %q, want prebuilts", gotBucket)
	}
	tag := DepsReleaseTag("llvm", "22.1.0")
	for name, sha := range shas {
		asset := sha + ".br"
		// Mirrored to R2 as a flat CAS object: key == <sha>.br, no path.
		if got := mirror.puts[asset]; got != asset {
			t.Errorf("%s: expected flat R2 put %q, got puts=%v", name, asset, mirror.puts)
		}
		// Still uploaded to GitHub too (dual publish, not either/or).
		if !sliceContains(stub.uploadedAssets[tag], asset) {
			t.Errorf("%s: GitHub upload missing, got %v", name, stub.uploadedAssets[tag])
		}
	}
}

// TestPublishBlobsR2Disabled pins that --r2-bucket="" turns OFF mirroring: the
// mirror is never constructed (no `npx wrangler` call), so a maintainer without
// wrangler can still publish to GitHub alone.
func TestPublishBlobsR2Disabled(t *testing.T) {
	_ = withStubUploader(t)
	called := false
	prevMirror := newBlobMirror
	newBlobMirror = func(string) blobMirror { called = true; return &stubBlobMirror{} }
	t.Cleanup(func() { newBlobMirror = prevMirror })

	root, _ := publishBlobsTestRoot(t, map[string]string{"opt": "OPT", "llc": "LLC"})
	if err := runReleasePublishBlobs(root, []string{"--host", "linux-amd64", "--r2-bucket", ""}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal(`--r2-bucket="" must disable R2 mirroring (newBlobMirror must not be constructed)`)
	}
}

// ── manifest --from-catalog ────────────────────────────────────────────────

func TestReleaseManifestFromCatalogHit(t *testing.T) {
	root, _ := fakeReleaseRoot(t, map[string]string{})
	// Seed the catalog with both files the prebuilts target lists.
	cat := &BlobsCatalog{Schema: 1, Blobs: []BlobEntry{
		{Dependency: "llvm", Version: "22.1.0", Target: "linux-amd64", Name: "opt",
			SHA256: "1111111111111111111111111111111111111111111111111111111111111111",
			Size:   100, Compression: compressionBrotli, CompressedSize: 50},
		{Dependency: "llvm", Version: "22.1.0", Target: "linux-amd64", Name: "llc",
			SHA256: "2222222222222222222222222222222222222222222222222222222222222222",
			Size:   200, Compression: compressionBrotli, CompressedSize: 99},
	}}
	if err := WriteBlobsCatalog(root, cat); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "manifest.json")
	if err := runReleaseManifest(root, []string{"--from-catalog", "--host", "linux-amd64", "--out", out}); err != nil {
		t.Fatalf("manifest --from-catalog: %v", err)
	}
	m, err := loadRuntimeManifest(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(m.Entries))
	}
	byName := map[string]runtimeManifestEntry{}
	for _, e := range m.Entries {
		byName[e.Name] = e
	}
	opt := byName["llvm-opt"]
	if opt.SHA256 != "1111111111111111111111111111111111111111111111111111111111111111" || opt.Size != 100 {
		t.Fatalf("opt entry wrong: %+v", opt)
	}
	if len(opt.Sources) != 3 {
		t.Fatalf("expected 3 ranked sources (github blob, mirror blob, archive), got %d", len(opt.Sources))
	}
	wantBlob := releaseAssetBase + "/deps-llvm-22.1.0/" + opt.SHA256 + ".br"
	if opt.Sources[0].Blob != wantBlob {
		t.Fatalf("Sources[0].Blob = %q, want %q", opt.Sources[0].Blob, wantBlob)
	}
	if opt.Sources[0].Compression != compressionBrotli {
		t.Fatalf("Sources[0].Compression = %q", opt.Sources[0].Compression)
	}
	// Secondary blob source: the prebuilts.promise-lang.org mirror.
	wantMirror := blobMirrorBase + "/" + opt.SHA256 + ".br"
	if opt.Sources[1].Blob != wantMirror {
		t.Fatalf("Sources[1].Blob (mirror) = %q, want %q", opt.Sources[1].Blob, wantMirror)
	}
	// Archive fallback is the upstream tarball.
	if opt.Sources[2].Archive == "" || opt.Sources[2].ArchivePath == "" {
		t.Fatalf("archive fallback missing: %+v", opt.Sources[2])
	}
}

func TestReleaseManifestFromCatalogMissingEntry(t *testing.T) {
	root, _ := fakeReleaseRoot(t, map[string]string{})
	// Empty catalog — manifest projection must fail with an actionable error.
	out := filepath.Join(root, "manifest.json")
	err := runReleaseManifest(root, []string{"--from-catalog", "--host", "linux-amd64", "--out", out})
	if err == nil {
		t.Fatal("expected error when catalog has no entries")
	}
	if !strings.Contains(err.Error(), "publish-blobs") {
		t.Fatalf("error should reference publish-blobs as the remediation, got: %v", err)
	}
}

func TestReleaseManifestFromCatalogRejectsPositional(t *testing.T) {
	root, _ := fakeReleaseRoot(t, map[string]string{})
	if err := runReleaseManifest(root, []string{"--from-catalog", "blobsdir", "--host", "linux-amd64", "--out", "o.json"}); err == nil {
		t.Fatal("--from-catalog must reject a positional <blobsdir>")
	}
}

func TestReleaseManifestFromCatalogRejectsPack(t *testing.T) {
	root, _ := fakeReleaseRoot(t, map[string]string{})
	if err := runReleaseManifest(root, []string{"--from-catalog", "--host", "linux-amd64", "--pack", "p", "--out", "o.json"}); err == nil {
		t.Fatal("--from-catalog must reject --pack (nothing to pack locally)")
	}
}

// TestReleaseManifestFromCatalogRejectsTag pins the invariant that --tag has
// no override role under --from-catalog. The blobs live at the catalog-derived
// `deps-<dep>-<version>` tag; any other tag would point the manifest's URLs at
// a release that does not host them and `fetch-blobs` would 404 on every
// entry. The CLI surfaces a loud error rather than silently producing an
// unfetchable manifest — protects release.yml from accidentally re-introducing
// the legacy `--tag ${{ github.ref_name }}` override.
// TestReleaseManifestFromCatalogBadCodec covers BlobAssetURL's error path
// surfacing through runReleaseManifestFromCatalog. A catalog entry whose
// `compression` field somehow holds an unknown codec (a corrupted commit, a
// downgrade of the catalog file with the loader bypassed) MUST fail loudly at
// projection time, not silently produce a manifest whose blob URLs are
// suffix-less and will 404. The bypass is needed because `WriteBlobsCatalog`
// validates the codec — so we hand-author the JSON.
func TestReleaseManifestFromCatalogBadCodec(t *testing.T) {
	root, _ := fakeReleaseRoot(t, map[string]string{})
	// Hand-author a catalog with an invalid codec (skips Validate()).
	path := blobsCatalogPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schema":1,"blobs":[
{"dependency":"llvm","version":"22.1.0","target":"linux-amd64","name":"opt","sha256":"abc","size":1,"compression":"lz4"},
{"dependency":"llvm","version":"22.1.0","target":"linux-amd64","name":"llc","sha256":"def","size":1,"compression":"lz4"}
]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "manifest.json")
	err := runReleaseManifest(root, []string{"--from-catalog", "--host", "linux-amd64", "--out", out})
	if err == nil {
		t.Fatal("expected error when catalog entry has an unknown codec")
	}
	// LoadBlobsCatalog runs Validate during load, so the failure surfaces from
	// there. Either way, the message must mention the codec to be actionable.
	if !strings.Contains(err.Error(), "compression") && !strings.Contains(err.Error(), "lz4") {
		t.Fatalf("error should mention compression / codec, got: %v", err)
	}
}

// TestReleaseManifestRequiresOut pins the unified `--out is required` check
// added with T0797: the check runs BEFORE the mode split so neither classic
// nor --from-catalog mode silently produces no output file.
func TestReleaseManifestRequiresOut(t *testing.T) {
	root, blobsDir := fakeReleaseRoot(t, map[string]string{"opt": "OPT", "llc": "LLC"})
	// Classic mode: positional <blobsdir> + --pack, no --out.
	err := runReleaseManifest(root, []string{blobsDir, "--host", "linux-amd64", "--pack", filepath.Join(root, "p")})
	if err == nil {
		t.Fatal("classic manifest must require --out")
	}
	if !strings.Contains(err.Error(), "--out") {
		t.Fatalf("error should mention --out, got: %v", err)
	}
	// --from-catalog mode: same requirement.
	err = runReleaseManifest(root, []string{"--from-catalog", "--host", "linux-amd64"})
	if err == nil {
		t.Fatal("--from-catalog must require --out")
	}
	if !strings.Contains(err.Error(), "--out") {
		t.Fatalf("error should mention --out, got: %v", err)
	}
}

// TestReleaseManifestRequiresPack pins the secondary `--pack is required`
// check on classic mode after the unified --out check. Without it, a workflow
// that drops --pack would crash inside the packing loop.
func TestReleaseManifestRequiresPack(t *testing.T) {
	root, blobsDir := fakeReleaseRoot(t, map[string]string{"opt": "OPT", "llc": "LLC"})
	err := runReleaseManifest(root, []string{blobsDir, "--host", "linux-amd64", "--out", filepath.Join(root, "out.json")})
	if err == nil {
		t.Fatal("classic manifest must require --pack")
	}
	if !strings.Contains(err.Error(), "--pack") {
		t.Fatalf("error should mention --pack, got: %v", err)
	}
}

func TestReleaseManifestFromCatalogRejectsTag(t *testing.T) {
	root, _ := fakeReleaseRoot(t, map[string]string{})
	err := runReleaseManifest(root, []string{"--from-catalog", "--host", "linux-amd64", "--out", filepath.Join(root, "m.json"), "--tag", "epoch-2026.0"})
	if err == nil {
		t.Fatal("--from-catalog must reject --tag (override would yield an unfetchable manifest)")
	}
	if !strings.Contains(err.Error(), "--tag is not supported") {
		t.Fatalf("error should explain why --tag is rejected, got: %v", err)
	}
}

// ── fetch-blobs ────────────────────────────────────────────────────────────

// stubBlobFetcher serves bytes from an in-memory map keyed by asset name (the
// `<sha>.br` basename), so fetch-blobs runs without `gh` or HTTP.
type stubBlobFetcher struct {
	assets map[string][]byte // asset name → file bytes
	calls  int               // number of FetchAsset invocations (cache-skip assertions)
}

func (s *stubBlobFetcher) FetchAsset(tag, asset, dst string) error {
	s.calls++
	data, ok := s.assets[asset]
	if !ok {
		return os.ErrNotExist
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

func withStubFetcher(t *testing.T, assets map[string][]byte) *stubBlobFetcher {
	t.Helper()
	stub := &stubBlobFetcher{assets: assets}
	prev := defaultBlobFetcher
	defaultBlobFetcher = stub
	t.Cleanup(func() { defaultBlobFetcher = prev })
	return stub
}

// brotliBytes returns the brotli-11 compressed form of `data`. Used to build
// the test fetcher's served bytes.
func brotliBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "in")
	dst := filepath.Join(dir, "out.br")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := compressFileBrotli(src, dst); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFetchBlobsHappyPath(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "m.json")

	contentOpt := []byte("OPT_FETCH")
	contentLlc := []byte("LLC_FETCH")
	shaOpt := sha256Hex(contentOpt)
	shaLlc := sha256Hex(contentLlc)
	brOpt := brotliBytes(t, contentOpt)
	brLlc := brotliBytes(t, contentLlc)

	m := runtimeManifest{Schema: runtimeManifestSchema, Epoch: "2026.0", Entries: []runtimeManifestEntry{
		{Name: "llvm-opt", SHA256: shaOpt, Size: int64(len(contentOpt)), Kind: "blob",
			Sources: []runtimeSource{{Blob: releaseAssetBase + "/deps-llvm-22.1.0/" + shaOpt + ".br", Compression: compressionBrotli}}},
		{Name: "llvm-llc", SHA256: shaLlc, Size: int64(len(contentLlc)), Kind: "blob",
			Sources: []runtimeSource{{Blob: releaseAssetBase + "/deps-llvm-22.1.0/" + shaLlc + ".br", Compression: compressionBrotli}}},
	}}
	if err := writeRuntimeManifest(manifestPath, &m); err != nil {
		t.Fatal(err)
	}

	withStubFetcher(t, map[string][]byte{
		shaOpt + ".br": brOpt,
		shaLlc + ".br": brLlc,
	})

	outDir := filepath.Join(dir, "out")
	if err := runReleaseFetchBlobs(dir, []string{"--manifest", manifestPath, "--out", outDir}); err != nil {
		t.Fatalf("fetch-blobs: %v", err)
	}
	// Files written with the manifest-name minus "llvm-" prefix.
	for name, want := range map[string]string{"opt": "OPT_FETCH", "llc": "LLC_FETCH"} {
		got, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
		// Compressed asset deleted by default.
		if Exists(filepath.Join(outDir, sha256Hex([]byte(want))+".br")) {
			t.Errorf("%s.br was not cleaned up (no --keep-compressed)", name)
		}
	}
}

func TestFetchBlobsKeepCompressed(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "m.json")
	content := []byte("OPT_KEEP")
	sha := sha256Hex(content)
	br := brotliBytes(t, content)
	m := runtimeManifest{Schema: runtimeManifestSchema, Epoch: "2026.0", Entries: []runtimeManifestEntry{
		{Name: "llvm-opt", SHA256: sha, Size: int64(len(content)), Kind: "blob",
			Sources: []runtimeSource{{Blob: releaseAssetBase + "/deps-llvm-22.1.0/" + sha + ".br", Compression: compressionBrotli}}},
	}}
	if err := writeRuntimeManifest(manifestPath, &m); err != nil {
		t.Fatal(err)
	}
	withStubFetcher(t, map[string][]byte{sha + ".br": br})

	outDir := filepath.Join(dir, "out")
	if err := runReleaseFetchBlobs(dir, []string{"--manifest", manifestPath, "--out", outDir, "--keep-compressed"}); err != nil {
		t.Fatal(err)
	}
	if !Exists(filepath.Join(outDir, sha+".br")) {
		t.Error("--keep-compressed must leave the <sha>.br alongside the decompressed file")
	}
	if !Exists(filepath.Join(outDir, "opt")) {
		t.Error("decompressed file missing")
	}
}

// TestFetchBlobsCachedSkipsDownload pins T0844: a second fetch into the same
// --out dir must NOT re-download when the decompressed output already matches
// the manifest sha. The second run installs an EMPTY fetcher whose FetchAsset
// would error (os.ErrNotExist) on any call — so success proves the download was
// skipped, and calls==0 makes it exact.
func TestFetchBlobsCachedSkipsDownload(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "m.json")
	content := []byte("OPT_CACHED")
	sha := sha256Hex(content)
	br := brotliBytes(t, content)
	m := runtimeManifest{Schema: runtimeManifestSchema, Epoch: "2026.0", Entries: []runtimeManifestEntry{
		{Name: "llvm-opt", SHA256: sha, Size: int64(len(content)), Kind: "blob",
			Sources: []runtimeSource{{Blob: releaseAssetBase + "/deps-llvm-22.1.0/" + sha + ".br", Compression: compressionBrotli}}},
	}}
	if err := writeRuntimeManifest(manifestPath, &m); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")

	// First run populates the cache.
	first := withStubFetcher(t, map[string][]byte{sha + ".br": br})
	if err := runReleaseFetchBlobs(dir, []string{"--manifest", manifestPath, "--out", outDir}); err != nil {
		t.Fatalf("first fetch-blobs: %v", err)
	}
	if first.calls != 1 {
		t.Fatalf("first run made %d FetchAsset calls, want 1", first.calls)
	}

	// Second run with an EMPTY fetcher: a cache miss would call FetchAsset and
	// fail with os.ErrNotExist. Success + calls==0 proves the download was skipped.
	second := withStubFetcher(t, map[string][]byte{})
	if err := runReleaseFetchBlobs(dir, []string{"--manifest", manifestPath, "--out", outDir}); err != nil {
		t.Fatalf("second fetch-blobs (cached) must succeed without download: %v", err)
	}
	if second.calls != 0 {
		t.Fatalf("cached run made %d FetchAsset calls, want 0", second.calls)
	}
	got, err := os.ReadFile(filepath.Join(outDir, "opt"))
	if err != nil || string(got) != string(content) {
		t.Fatalf("cached output = %q (err %v), want %q", got, err, content)
	}
}

// TestFetchBlobsKeepCompressedCached pins the --keep-compressed cache path: a
// second run must skip the download AND leave both the decompressed file and
// the <sha>.br intact (publish-install needs the .br to survive).
func TestFetchBlobsKeepCompressedCached(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "m.json")
	content := []byte("OPT_KEEP_CACHED")
	sha := sha256Hex(content)
	br := brotliBytes(t, content)
	m := runtimeManifest{Schema: runtimeManifestSchema, Epoch: "2026.0", Entries: []runtimeManifestEntry{
		{Name: "llvm-opt", SHA256: sha, Size: int64(len(content)), Kind: "blob",
			Sources: []runtimeSource{{Blob: releaseAssetBase + "/deps-llvm-22.1.0/" + sha + ".br", Compression: compressionBrotli}}},
	}}
	if err := writeRuntimeManifest(manifestPath, &m); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	args := []string{"--manifest", manifestPath, "--out", outDir, "--keep-compressed"}

	first := withStubFetcher(t, map[string][]byte{sha + ".br": br})
	if err := runReleaseFetchBlobs(dir, args); err != nil {
		t.Fatalf("first keep-compressed fetch: %v", err)
	}
	if first.calls != 1 {
		t.Fatalf("first run made %d FetchAsset calls, want 1", first.calls)
	}

	second := withStubFetcher(t, map[string][]byte{})
	if err := runReleaseFetchBlobs(dir, args); err != nil {
		t.Fatalf("second keep-compressed fetch (cached) must succeed: %v", err)
	}
	if second.calls != 0 {
		t.Fatalf("cached keep-compressed run made %d FetchAsset calls, want 0", second.calls)
	}
	if !Exists(filepath.Join(outDir, sha+".br")) {
		t.Error("cached keep-compressed run must preserve the <sha>.br")
	}
	if !Exists(filepath.Join(outDir, "opt")) {
		t.Error("cached keep-compressed run must preserve the decompressed file")
	}
}

// TestFetchBlobsCorruptCacheRefetches pins the stale-cache path: a corrupt
// decompressed file (right name, wrong bytes) must NOT be mistaken for a hit —
// the run must re-fetch from the populated fetcher and end with correct content.
func TestFetchBlobsCorruptCacheRefetches(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "m.json")
	content := []byte("OPT_REAL")
	sha := sha256Hex(content)
	br := brotliBytes(t, content)
	m := runtimeManifest{Schema: runtimeManifestSchema, Epoch: "2026.0", Entries: []runtimeManifestEntry{
		{Name: "llvm-opt", SHA256: sha, Size: int64(len(content)), Kind: "blob",
			Sources: []runtimeSource{{Blob: releaseAssetBase + "/deps-llvm-22.1.0/" + sha + ".br", Compression: compressionBrotli}}},
	}}
	if err := writeRuntimeManifest(manifestPath, &m); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-write a corrupt decompressed file with the right name but wrong bytes.
	if err := os.WriteFile(filepath.Join(outDir, "opt"), []byte("CORRUPT"), 0o755); err != nil {
		t.Fatal(err)
	}

	stub := withStubFetcher(t, map[string][]byte{sha + ".br": br})
	if err := runReleaseFetchBlobs(dir, []string{"--manifest", manifestPath, "--out", outDir}); err != nil {
		t.Fatalf("fetch-blobs must re-fetch over a corrupt cache: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("corrupt-cache run made %d FetchAsset calls, want 1 (re-fetch)", stub.calls)
	}
	got, err := os.ReadFile(filepath.Join(outDir, "opt"))
	if err != nil || string(got) != string(content) {
		t.Fatalf("re-fetched output = %q (err %v), want %q", got, err, content)
	}
}

// TestFetchBlobsKeepCompressedMissingBrRefetches pins the keepCompressed
// no-.br branch of blobCacheHit: a valid decompressed file with NO surviving
// <sha>.br is not a hit (publish-install needs the .br), so the run must
// re-fetch and restore both outputs. We seed the cache with a plain (non-keep)
// run, which removes the .br, then re-run with --keep-compressed.
func TestFetchBlobsKeepCompressedMissingBrRefetches(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "m.json")
	content := []byte("OPT_NO_BR")
	sha := sha256Hex(content)
	br := brotliBytes(t, content)
	m := runtimeManifest{Schema: runtimeManifestSchema, Epoch: "2026.0", Entries: []runtimeManifestEntry{
		{Name: "llvm-opt", SHA256: sha, Size: int64(len(content)), Kind: "blob",
			Sources: []runtimeSource{{Blob: releaseAssetBase + "/deps-llvm-22.1.0/" + sha + ".br", Compression: compressionBrotli}}},
	}}
	if err := writeRuntimeManifest(manifestPath, &m); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")

	// Plain run leaves a valid decompressed file but removes the .br.
	first := withStubFetcher(t, map[string][]byte{sha + ".br": br})
	if err := runReleaseFetchBlobs(dir, []string{"--manifest", manifestPath, "--out", outDir}); err != nil {
		t.Fatalf("first (plain) fetch: %v", err)
	}
	if first.calls != 1 {
		t.Fatalf("first run made %d FetchAsset calls, want 1", first.calls)
	}
	if Exists(filepath.Join(outDir, sha+".br")) {
		t.Fatal("plain run must not keep the .br (precondition for this test)")
	}

	// --keep-compressed run: dst is valid but the .br is gone → must re-fetch.
	second := withStubFetcher(t, map[string][]byte{sha + ".br": br})
	if err := runReleaseFetchBlobs(dir, []string{"--manifest", manifestPath, "--out", outDir, "--keep-compressed"}); err != nil {
		t.Fatalf("keep-compressed fetch over missing .br: %v", err)
	}
	if second.calls != 1 {
		t.Fatalf("missing-.br run made %d FetchAsset calls, want 1 (re-fetch)", second.calls)
	}
	if !Exists(filepath.Join(outDir, sha+".br")) {
		t.Error("re-fetch must restore the <sha>.br")
	}
	got, err := os.ReadFile(filepath.Join(outDir, "opt"))
	if err != nil || string(got) != string(content) {
		t.Fatalf("output = %q (err %v), want %q", got, err, content)
	}
}

// TestFetchBlobsKeepCompressedCorruptBrRefetches pins the keepCompressed
// corrupt-.br branch of blobCacheHit: a valid decompressed file alongside a
// .br that fails decompress-verify is not a hit — both outputs are dropped and
// the run re-fetches.
func TestFetchBlobsKeepCompressedCorruptBrRefetches(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "m.json")
	content := []byte("OPT_BAD_BR")
	sha := sha256Hex(content)
	br := brotliBytes(t, content)
	m := runtimeManifest{Schema: runtimeManifestSchema, Epoch: "2026.0", Entries: []runtimeManifestEntry{
		{Name: "llvm-opt", SHA256: sha, Size: int64(len(content)), Kind: "blob",
			Sources: []runtimeSource{{Blob: releaseAssetBase + "/deps-llvm-22.1.0/" + sha + ".br", Compression: compressionBrotli}}},
	}}
	if err := writeRuntimeManifest(manifestPath, &m); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Valid decompressed file (right sha) but a garbage .br that won't decompress.
	if err := os.WriteFile(filepath.Join(outDir, "opt"), content, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, sha+".br"), []byte("NOT_BROTLI"), 0o644); err != nil {
		t.Fatal(err)
	}

	stub := withStubFetcher(t, map[string][]byte{sha + ".br": br})
	if err := runReleaseFetchBlobs(dir, []string{"--manifest", manifestPath, "--out", outDir, "--keep-compressed"}); err != nil {
		t.Fatalf("keep-compressed fetch over corrupt .br: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("corrupt-.br run made %d FetchAsset calls, want 1 (re-fetch)", stub.calls)
	}
	got, err := os.ReadFile(filepath.Join(outDir, "opt"))
	if err != nil || string(got) != string(content) {
		t.Fatalf("output = %q (err %v), want %q", got, err, content)
	}
}

// TestFetchBlobsCacheValidationError pins the fileSHA256 error path in
// blobCacheHit: when dstPath exists but cannot be hashed (here it is a
// directory, which os.Stat-Exists treats as present but io.Copy cannot read),
// the validation error must abort the run loudly rather than silently
// re-fetching or skipping.
func TestFetchBlobsCacheValidationError(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "m.json")
	content := []byte("OPT_DIR")
	sha := sha256Hex(content)
	br := brotliBytes(t, content)
	m := runtimeManifest{Schema: runtimeManifestSchema, Epoch: "2026.0", Entries: []runtimeManifestEntry{
		{Name: "llvm-opt", SHA256: sha, Size: int64(len(content)), Kind: "blob",
			Sources: []runtimeSource{{Blob: releaseAssetBase + "/deps-llvm-22.1.0/" + sha + ".br", Compression: compressionBrotli}}},
	}}
	if err := writeRuntimeManifest(manifestPath, &m); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	// Make dstPath (out/opt) a directory so fileSHA256 fails to read it.
	if err := os.MkdirAll(filepath.Join(outDir, "opt"), 0o755); err != nil {
		t.Fatal(err)
	}

	stub := withStubFetcher(t, map[string][]byte{sha + ".br": br})
	err := runReleaseFetchBlobs(dir, []string{"--manifest", manifestPath, "--out", outDir})
	if err == nil {
		t.Fatal("fetch-blobs must fail when the cached output cannot be validated")
	}
	if !strings.Contains(err.Error(), "validate cached") {
		t.Fatalf("error should mention cache validation, got: %v", err)
	}
	if stub.calls != 0 {
		t.Fatalf("validation error must abort before any download, made %d calls", stub.calls)
	}
}

func TestFetchBlobsSHAMismatch(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "m.json")
	content := []byte("real bytes")
	sha := sha256Hex(content)
	// Serve DIFFERENT content under the same asset name → uncompressed sha
	// will not match the manifest's expected value → hard failure.
	wrong := brotliBytes(t, []byte("wrong bytes"))
	m := runtimeManifest{Schema: runtimeManifestSchema, Epoch: "2026.0", Entries: []runtimeManifestEntry{
		{Name: "llvm-opt", SHA256: sha, Size: int64(len(content)), Kind: "blob",
			Sources: []runtimeSource{{Blob: releaseAssetBase + "/deps-llvm-22.1.0/" + sha + ".br", Compression: compressionBrotli}}},
	}}
	if err := writeRuntimeManifest(manifestPath, &m); err != nil {
		t.Fatal(err)
	}
	withStubFetcher(t, map[string][]byte{sha + ".br": wrong})

	err := runReleaseFetchBlobs(dir, []string{"--manifest", manifestPath, "--out", filepath.Join(dir, "out")})
	if err == nil {
		t.Fatal("fetch-blobs must fail on a sha256 mismatch")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("error should mention the sha256 mismatch, got: %v", err)
	}
}

func TestFetchBlobsRequiresFlags(t *testing.T) {
	if err := runReleaseFetchBlobs(t.TempDir(), []string{}); err == nil {
		t.Fatal("expected error for missing --manifest")
	}
	if err := runReleaseFetchBlobs(t.TempDir(), []string{"--manifest", "m"}); err == nil {
		t.Fatal("expected error for missing --out")
	}
}

// TestFetchBlobsFetchAssetError pins the fetcher-error path: a download
// failure must surface as an error tagged with the failing entry's name, not
// be silently swallowed and produce a half-populated `dist/blobs` directory.
func TestFetchBlobsFetchAssetError(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "m.json")
	content := []byte("opt bytes")
	sha := sha256Hex(content)
	m := runtimeManifest{Schema: runtimeManifestSchema, Epoch: "2026.0", Entries: []runtimeManifestEntry{
		{Name: "llvm-opt", SHA256: sha, Size: int64(len(content)), Kind: "blob",
			Sources: []runtimeSource{{Blob: releaseAssetBase + "/deps-llvm-22.1.0/" + sha + ".br", Compression: compressionBrotli}}},
	}}
	if err := writeRuntimeManifest(manifestPath, &m); err != nil {
		t.Fatal(err)
	}
	// Empty stub map → FetchAsset returns os.ErrNotExist for every asset.
	withStubFetcher(t, map[string][]byte{})

	err := runReleaseFetchBlobs(dir, []string{"--manifest", manifestPath, "--out", filepath.Join(dir, "out")})
	if err == nil {
		t.Fatal("expected fetch-blobs to fail when FetchAsset errors")
	}
	if !strings.Contains(err.Error(), "llvm-opt") {
		t.Fatalf("error should name the failing entry, got: %v", err)
	}
}

// TestFetchBlobsBadManifest pins the manifest-load error path: an unreadable
// manifest must produce a "load manifest" error before any directory is
// created. Otherwise a stale `--out` dir from a previous run could be mistaken
// for the result of a successful fetch.
func TestFetchBlobsBadManifest(t *testing.T) {
	dir := t.TempDir()
	err := runReleaseFetchBlobs(dir, []string{"--manifest", filepath.Join(dir, "no-such.json"), "--out", filepath.Join(dir, "out")})
	if err == nil {
		t.Fatal("expected error for missing manifest path")
	}
	if !strings.Contains(err.Error(), "load manifest") {
		t.Fatalf("error should mention the load failure, got: %v", err)
	}
}

// TestFetchBlobsBadFlag rejects an unknown CLI flag before any side effects.
func TestFetchBlobsBadFlag(t *testing.T) {
	if err := runReleaseFetchBlobs(t.TempDir(), []string{"--nonsense"}); err == nil {
		t.Fatal("expected flag.Parse error on an unknown flag")
	}
}

func TestFetchBlobsNoBlobSource(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "m.json")
	m := runtimeManifest{Schema: runtimeManifestSchema, Epoch: "2026.0", Entries: []runtimeManifestEntry{
		{Name: "llvm-opt", SHA256: "abc", Size: 1, Kind: "blob", Sources: []runtimeSource{
			{Archive: "https://example.test/llvm.tar.gz", ArchivePath: "bin/opt"},
		}},
	}}
	if err := writeRuntimeManifest(manifestPath, &m); err != nil {
		t.Fatal(err)
	}
	if err := runReleaseFetchBlobs(dir, []string{"--manifest", manifestPath, "--out", filepath.Join(dir, "out")}); err == nil {
		t.Fatal("fetch-blobs must reject a manifest entry without any blob source")
	}
}

func TestParseReleaseTagFromURL(t *testing.T) {
	if got := parseReleaseTagFromURL(releaseAssetBase + "/deps-llvm-23.1.0/abc.br"); got != "deps-llvm-23.1.0" {
		t.Fatalf("got %q", got)
	}
	if got := parseReleaseTagFromURL("https://other.example/x"); got != "" {
		t.Fatalf("non-matching URL should yield empty tag, got %q", got)
	}
	if got := parseReleaseTagFromURL(releaseAssetBase + "/"); got != "" {
		t.Fatalf("empty path should yield empty tag, got %q", got)
	}
}

// TestDecompressAndVerifyCodecs exercises both supported codecs and the
// unknown-codec rejection path.
func TestDecompressAndVerifyCodecs(t *testing.T) {
	dir := t.TempDir()
	content := []byte("RAW DATA")
	wantSHA := sha256Hex(content)

	// Raw passthrough ("" and "none").
	raw := filepath.Join(dir, "raw")
	if err := os.WriteFile(raw, content, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, codec := range []string{"", "none"} {
		dst := filepath.Join(dir, "out-"+codec)
		if err := decompressAndVerify(raw, dst, codec, wantSHA); err != nil {
			t.Fatalf("codec %q: %v", codec, err)
		}
	}

	// Brotli round-trip.
	br := filepath.Join(dir, "in.br")
	if err := compressFileBrotli(raw, br); err != nil {
		t.Fatal(err)
	}
	if err := decompressAndVerify(br, filepath.Join(dir, "out-br"), compressionBrotli, wantSHA); err != nil {
		t.Fatalf("brotli: %v", err)
	}

	// Unknown codec.
	if err := decompressAndVerify(raw, filepath.Join(dir, "ignored"), "lz4", wantSHA); err == nil {
		t.Fatal("unknown codec must be rejected")
	}
}

// TestGhCLIFetcherHTTPFallback exercises the non-deps-URL branch of the
// production fetcher — the tag is "", so it falls through to direct HTTP.
// This is reachable when a manifest source points at a non-release URL.
func TestGhCLIFetcherHTTPFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("HTTP_BYTES"))
	}))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "out")
	if err := (ghCLIFetcher{}).FetchAsset("", srv.URL+"/something", dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "HTTP_BYTES" {
		t.Fatalf("got %q", got)
	}
}

// TestRetryTransientSucceedsAfterFailures pins the core resilience win: a couple
// of transient failures (the GitHub dial-timeout / connection-reset blips that
// killed the epoch-2026.3 linux release) are absorbed, and the first success
// ends the loop. sleepFn is stubbed so the backoff doesn't actually wait.
func TestRetryTransientSucceedsAfterFailures(t *testing.T) {
	prev := sleepFn
	var slept int
	sleepFn = func(time.Duration) { slept++ }
	t.Cleanup(func() { sleepFn = prev })

	calls := 0
	err := retryTransient("download x", 4, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("boom %d", calls)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success on 3rd attempt, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("fn called %d times, want 3", calls)
	}
	if slept != 2 {
		t.Fatalf("slept %d times, want 2 (once between each of the 3 attempts)", slept)
	}
}

// TestRetryTransientExhausts verifies that when every attempt fails, the last
// cause surfaces wrapped with the attempt count, fn is called exactly `attempts`
// times (no off-by-one, no infinite loop), and there is no trailing sleep after
// the final failure.
func TestRetryTransientExhausts(t *testing.T) {
	prev := sleepFn
	var slept int
	sleepFn = func(time.Duration) { slept++ }
	t.Cleanup(func() { sleepFn = prev })

	calls := 0
	err := retryTransient("download y", 3, time.Millisecond, func() error {
		calls++
		return fmt.Errorf("still down")
	})
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if calls != 3 {
		t.Fatalf("fn called %d times, want 3", calls)
	}
	if slept != 2 {
		t.Fatalf("slept %d times, want 2 (no sleep after the final failure)", slept)
	}
	if !strings.Contains(err.Error(), "after 3 attempts") || !strings.Contains(err.Error(), "still down") {
		t.Fatalf("error should carry the attempt count and last cause, got: %v", err)
	}
}

// TestRetryTransientFirstTry pins the happy path: a first-attempt success makes
// no retry and no sleep — the resilience wrapper must be zero-cost when healthy.
func TestRetryTransientFirstTry(t *testing.T) {
	prev := sleepFn
	var slept int
	sleepFn = func(time.Duration) { slept++ }
	t.Cleanup(func() { sleepFn = prev })

	calls := 0
	if err := retryTransient("download z", 4, time.Millisecond, func() error {
		calls++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || slept != 0 {
		t.Fatalf("calls=%d slept=%d, want calls=1 slept=0", calls, slept)
	}
}

func sliceContains(s []string, want string) bool {
	for _, e := range s {
		if e == want {
			return true
		}
	}
	return false
}

// TestLooksLikeWranglerAuthError pins the auth-failure detection that gates the
// login guidance: real wrangler auth errors match (so a host where wrangler is
// not logged in gets actionable instructions), while unrelated failures (e.g. a
// missing object, a network blip) do not falsely trigger the help text.
func TestLooksLikeWranglerAuthError(t *testing.T) {
	authy := []string{
		"You are not authenticated. Please run `wrangler login`.",
		"In a non-interactive environment, it's necessary to set a CLOUDFLARE_API_TOKEN environment variable",
		"Authentication error [code: 10000]",
		"The request is unauthorized",
		"request could not be authenticated",
	}
	for _, s := range authy {
		if !looksLikeWranglerAuthError(s) {
			t.Errorf("looksLikeWranglerAuthError(%q) = false, want true", s)
		}
	}
	notAuthy := []string{
		"The specified key does not exist.",
		"NoSuchKey: the object was not found",
		"dial tcp: lookup api.cloudflare.com: no such host",
		"",
	}
	for _, s := range notAuthy {
		if looksLikeWranglerAuthError(s) {
			t.Errorf("looksLikeWranglerAuthError(%q) = true, want false", s)
		}
	}
}

// TestStubMirrorGetSeeds verifies the mirror Get contract the seed step relies
// on: a key present in the bucket is written to localPath (found=true); an
// absent key yields found=false with no file (the "start fresh" path).
func TestStubMirrorGetSeeds(t *testing.T) {
	dir := t.TempDir()
	m := &stubBlobMirror{getContent: map[string]string{
		"dist/SHA256SUMS": "abc123  promise-darwin-arm64.gz\n",
	}}

	dst := filepath.Join(dir, "SHA256SUMS")
	found, err := m.Get("dist/SHA256SUMS", dst)
	if err != nil || !found {
		t.Fatalf("Get(existing) = (%v, %v), want (true, nil)", found, err)
	}
	data, _ := os.ReadFile(dst)
	if !strings.Contains(string(data), "promise-darwin-arm64.gz") {
		t.Errorf("seeded file = %q, want the darwin entry", data)
	}

	missing := filepath.Join(dir, "missing")
	found, err = m.Get("dist/absent", missing)
	if err != nil || found {
		t.Fatalf("Get(absent) = (%v, %v), want (false, nil)", found, err)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Errorf("Get(absent) must not create the destination file")
	}
}
