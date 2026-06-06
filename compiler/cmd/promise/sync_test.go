package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveSyncConfigDefault(t *testing.T) {
	t.Setenv("PROMISE_RELEASE_URL", "")
	t.Setenv("PROMISE_RELEASE_REPO", "")
	cfg := resolveSyncConfig()
	if cfg.apiBase != "https://api.github.com/repos/nicois/promise" {
		t.Fatalf("unexpected apiBase: %s", cfg.apiBase)
	}
}

func TestResolveSyncConfigRepoOverride(t *testing.T) {
	t.Setenv("PROMISE_RELEASE_URL", "")
	t.Setenv("PROMISE_RELEASE_REPO", "myorg/my-promise")
	cfg := resolveSyncConfig()
	if cfg.apiBase != "https://api.github.com/repos/myorg/my-promise" {
		t.Fatalf("unexpected apiBase: %s", cfg.apiBase)
	}
}

func TestResolveSyncConfigURLOverride(t *testing.T) {
	t.Setenv("PROMISE_RELEASE_URL", "https://git.corp.com/api/v3/repos/myorg/promise")
	t.Setenv("PROMISE_RELEASE_REPO", "ignored")
	cfg := resolveSyncConfig()
	if cfg.apiBase != "https://git.corp.com/api/v3/repos/myorg/promise" {
		t.Fatalf("unexpected apiBase: %s", cfg.apiBase)
	}
}

func TestResolveSyncConfigURLTrailingSlash(t *testing.T) {
	t.Setenv("PROMISE_RELEASE_URL", "https://example.com/api/")
	cfg := resolveSyncConfig()
	if cfg.apiBase != "https://example.com/api" {
		t.Fatalf("unexpected apiBase: %s", cfg.apiBase)
	}
}

func TestPlatformBinaryName(t *testing.T) {
	name := platformBinaryName()
	expected := fmt.Sprintf("promise-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		expected += ".exe"
	}
	if name != expected {
		t.Fatalf("expected %s, got %s", expected, name)
	}
}

// T0796: published assets are gzip-compressed. platformAssetName must append
// .gz to the runtime binary name so findAssets matches the published name.
func TestPlatformAssetName(t *testing.T) {
	asset := platformAssetName()
	expected := platformBinaryName() + ".gz"
	if asset != expected {
		t.Fatalf("expected %s, got %s", expected, asset)
	}
	if !strings.HasSuffix(asset, ".gz") {
		t.Fatalf("asset name must end in .gz, got %s", asset)
	}
}

// T0796: downloadAndInstall pipes the download through gunzipFile to recover
// the runtime binary — verify it round-trips arbitrary bytes correctly.
func TestGunzipFile(t *testing.T) {
	tmpDir := t.TempDir()
	gzPath := filepath.Join(tmpDir, "input.gz")
	binPath := filepath.Join(tmpDir, "output")

	original := []byte("the quick brown fox jumps over the lazy dog\x00\x01\x02")

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(original); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gzPath, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	if err := gunzipFile(gzPath, binPath); err != nil {
		t.Fatalf("gunzipFile: %v", err)
	}
	got, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("round-trip mismatch: got %q, want %q", got, original)
	}
}

func TestGunzipFileCorrupt(t *testing.T) {
	tmpDir := t.TempDir()
	gzPath := filepath.Join(tmpDir, "bad.gz")
	binPath := filepath.Join(tmpDir, "out")

	if err := os.WriteFile(gzPath, []byte("not a gzip stream"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := gunzipFile(gzPath, binPath); err == nil {
		t.Fatal("expected error on corrupt gzip input")
	}
}

// T0796: gunzipFile must surface a clear error when the source can't be opened
// (a downloaded asset was deleted/quarantined between download and decompress).
func TestGunzipFileMissingSource(t *testing.T) {
	tmpDir := t.TempDir()
	err := gunzipFile(filepath.Join(tmpDir, "does-not-exist.gz"), filepath.Join(tmpDir, "out"))
	if err == nil {
		t.Fatal("expected error on missing source file")
	}
	if !strings.Contains(err.Error(), "opening") {
		t.Fatalf("expected wrapped 'opening' error, got: %v", err)
	}
}

// T0796: gunzipFile must surface a clear error when the destination can't be
// created (e.g. read-only tmp dir or path through a missing parent).
func TestGunzipFileBadDest(t *testing.T) {
	tmpDir := t.TempDir()
	gzPath := filepath.Join(tmpDir, "input.gz")

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write([]byte("hello"))
	_ = gw.Close()
	if err := os.WriteFile(gzPath, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	// A path through a missing parent directory makes os.Create fail.
	badDst := filepath.Join(tmpDir, "missing-parent-dir", "out")
	err := gunzipFile(gzPath, badDst)
	if err == nil {
		t.Fatal("expected error on uncreatable dest")
	}
	if !strings.Contains(err.Error(), "creating") {
		t.Fatalf("expected wrapped 'creating' error, got: %v", err)
	}
}

// T0796: gunzipFile must surface an error when the gzip stream is truncated
// mid-body (an interrupted/partial download that nevertheless has a valid
// header). Distinct from TestGunzipFileCorrupt: header is OK, body is short.
func TestGunzipFileTruncated(t *testing.T) {
	tmpDir := t.TempDir()
	gzPath := filepath.Join(tmpDir, "trunc.gz")
	binPath := filepath.Join(tmpDir, "out")

	// Build a valid gzip stream of non-trivial size, then chop off the trailer.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(bytes.Repeat([]byte("payload-bytes-"), 1024)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	// Keep header (~10 bytes) + some compressed body but drop the CRC/length
	// trailer and most of the body — gzip.Reader detects the short read.
	if err := os.WriteFile(gzPath, raw[:20], 0644); err != nil {
		t.Fatal(err)
	}
	if err := gunzipFile(gzPath, binPath); err == nil {
		t.Fatal("expected error on truncated gzip stream")
	}
}

// T0796 end-to-end: simulate the full publish→download→verify→decompress
// pipeline against an httptest server. SHA256 is computed over the .gz asset
// (the documented invariant) and verified BEFORE decompression. This ties the
// individual pieces (findAssets + downloadFile + verifySHA256 + gunzipFile)
// together so a future refactor that reorders them or changes the hashed
// artifact will fail.
func TestDownloadVerifyDecompressPipeline(t *testing.T) {
	original := []byte("this is the real promise binary payload\x00\x01\x02\x03")

	// Build the gzip-compressed asset and its sha256 (over the compressed bytes).
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(original); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	gzBytes := gzBuf.Bytes()
	h := sha256.Sum256(gzBytes)
	assetHash := hex.EncodeToString(h[:])

	const assetName = "promise-linux-amd64.gz"
	// Include a strict-prefix asset to keep the exact-match property honest end-to-end.
	sums := assetHash + "  " + assetName + "\n" +
		"0000000000000000000000000000000000000000000000000000000000000000  promise-linux-amd64-full.gz\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + assetName:
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(gzBytes)))
			_, _ = w.Write(gzBytes)
		case "/SHA256SUMS":
			_, _ = w.Write([]byte(sums))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	release := &ghRelease{
		TagName: "epoch-2026.0",
		Assets: []ghAsset{
			{Name: assetName, BrowserDownloadURL: srv.URL + "/" + assetName},
			{Name: "SHA256SUMS", BrowserDownloadURL: srv.URL + "/SHA256SUMS"},
		},
	}

	assetURL, shaURL, err := findAssets(release, assetName)
	if err != nil {
		t.Fatalf("findAssets: %v", err)
	}
	if shaURL == "" {
		t.Fatal("SHA256SUMS asset not found")
	}

	tmpDir := t.TempDir()
	gzPath := filepath.Join(tmpDir, assetName)
	if err := downloadFile(assetURL, gzPath); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}

	// Verify over the .gz (matches downloadAndInstall's order).
	if err := verifySHA256(shaURL, gzPath, assetName); err != nil {
		t.Fatalf("verifySHA256 (over .gz): %v", err)
	}

	binPath := filepath.Join(tmpDir, "promise-linux-amd64")
	if err := gunzipFile(gzPath, binPath); err != nil {
		t.Fatalf("gunzipFile: %v", err)
	}
	got, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("pipeline output mismatch: got %q, want %q", got, original)
	}
}

// T0796 negative end-to-end: if SHA256SUMS lists a hash of the *decompressed*
// binary, verification (which runs over the .gz) must fail. This pins down the
// "checksum over compressed asset" invariant — a future change that
// accidentally hashed the decompressed file would silently pass the positive
// pipeline test but break this one.
func TestDownloadVerifyRejectsDecompressedHash(t *testing.T) {
	original := []byte("payload that gets hashed wrong")

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	_, _ = gw.Write(original)
	_ = gw.Close()
	gzBytes := gzBuf.Bytes()

	// Hash of DECOMPRESSED bytes — wrong relative to the .gz asset.
	wrongHash := sha256.Sum256(original)
	const assetName = "promise-linux-amd64.gz"
	sums := hex.EncodeToString(wrongHash[:]) + "  " + assetName + "\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + assetName:
			_, _ = w.Write(gzBytes)
		case "/SHA256SUMS":
			_, _ = w.Write([]byte(sums))
		}
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	gzPath := filepath.Join(tmpDir, assetName)
	if err := downloadFile(srv.URL+"/"+assetName, gzPath); err != nil {
		t.Fatal(err)
	}
	if err := verifySHA256(srv.URL+"/SHA256SUMS", gzPath, assetName); err == nil {
		t.Fatal("expected checksum mismatch when SHA256SUMS hashes the decompressed bytes")
	}
}

func TestFindAssets(t *testing.T) {
	release := &ghRelease{
		TagName: "epoch-2026.0",
		Assets: []ghAsset{
			{Name: "promise-linux-amd64", BrowserDownloadURL: "https://example.com/promise-linux-amd64"},
			{Name: "promise-darwin-arm64", BrowserDownloadURL: "https://example.com/promise-darwin-arm64"},
			{Name: "SHA256SUMS", BrowserDownloadURL: "https://example.com/SHA256SUMS"},
		},
	}

	binaryURL, shaURL, err := findAssets(release, "promise-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	if binaryURL != "https://example.com/promise-linux-amd64" {
		t.Fatalf("unexpected binary URL: %s", binaryURL)
	}
	if shaURL != "https://example.com/SHA256SUMS" {
		t.Fatalf("unexpected SHA URL: %s", shaURL)
	}
}

func TestFindAssetsMissing(t *testing.T) {
	release := &ghRelease{
		TagName: "epoch-2026.0",
		Assets: []ghAsset{
			{Name: "promise-linux-amd64", BrowserDownloadURL: "https://example.com/promise-linux-amd64"},
		},
	}

	_, _, err := findAssets(release, "promise-darwin-arm64")
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestFindAssetsNoSHA(t *testing.T) {
	release := &ghRelease{
		TagName: "epoch-2026.0",
		Assets: []ghAsset{
			{Name: "promise-linux-amd64", BrowserDownloadURL: "https://example.com/promise-linux-amd64"},
		},
	}

	binaryURL, shaURL, err := findAssets(release, "promise-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	if binaryURL != "https://example.com/promise-linux-amd64" {
		t.Fatalf("unexpected binary URL: %s", binaryURL)
	}
	if shaURL != "" {
		t.Fatalf("expected empty SHA URL, got: %s", shaURL)
	}
}

func TestVerifySHA256(t *testing.T) {
	// Create a temp binary file.
	tmpDir := t.TempDir()
	binaryPath := filepath.Join(tmpDir, "promise-test")
	content := []byte("hello world binary content")
	if err := os.WriteFile(binaryPath, content, 0644); err != nil {
		t.Fatal(err)
	}

	// Compute expected hash.
	h := sha256.Sum256(content)
	expectedHash := hex.EncodeToString(h[:])

	// Serve SHA256SUMS via httptest.
	shaContent := fmt.Sprintf("%s  promise-test\n%s  promise-other\n", expectedHash, "0000000000000000000000000000000000000000000000000000000000000000")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(shaContent))
	}))
	defer srv.Close()

	if err := verifySHA256(srv.URL, binaryPath, "promise-test"); err != nil {
		t.Fatalf("expected verification to pass: %v", err)
	}
}

func TestVerifySHA256Mismatch(t *testing.T) {
	tmpDir := t.TempDir()
	binaryPath := filepath.Join(tmpDir, "promise-test")
	if err := os.WriteFile(binaryPath, []byte("actual content"), 0644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("0000000000000000000000000000000000000000000000000000000000000000  promise-test\n"))
	}))
	defer srv.Close()

	err := verifySHA256(srv.URL, binaryPath, "promise-test")
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if !strings.Contains(err.Error(), "checksum verification failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVerifySHA256MissingEntry(t *testing.T) {
	tmpDir := t.TempDir()
	binaryPath := filepath.Join(tmpDir, "promise-test")
	if err := os.WriteFile(binaryPath, []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("abc123  promise-other\n"))
	}))
	defer srv.Close()

	err := verifySHA256(srv.URL, binaryPath, "promise-test")
	if err == nil {
		t.Fatal("expected missing entry error")
	}
	if !strings.Contains(err.Error(), "no checksum found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestIsEpochInstalled(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// Not installed.
	if isEpochInstalled("2026.0") {
		t.Fatal("expected epoch not to be installed")
	}

	// Install a fake binary.
	binaryName := "promise"
	if runtime.GOOS == "windows" {
		binaryName = "promise.exe"
	}
	binDir := filepath.Join(tmp, "epochs", "2026.0", "bin")
	os.MkdirAll(binDir, 0755)
	os.WriteFile(filepath.Join(binDir, binaryName), []byte("fake"), 0755)

	if !isEpochInstalled("2026.0") {
		t.Fatal("expected epoch to be installed")
	}
}

func TestFetchReleasesIntegration(t *testing.T) {
	// Mock GitHub API server.
	releases := []ghRelease{
		{TagName: "epoch-2026.0", Prerelease: false, Draft: false, Assets: []ghAsset{
			{Name: "promise-linux-amd64", BrowserDownloadURL: "https://example.com/a"},
		}},
		{TagName: "epoch-2026.2", Prerelease: false, Draft: false},
		{TagName: "epoch-next", Prerelease: true, Draft: false, Assets: []ghAsset{
			{Name: "promise-linux-amd64", BrowserDownloadURL: "https://example.com/b"},
		}},
		{TagName: "v1.0.0", Prerelease: false, Draft: false}, // non-epoch tag
		{TagName: "epoch-draft", Prerelease: false, Draft: true},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()

	cfg := syncConfig{apiBase: srv.URL}

	// Test findNextRelease.
	next, err := findNextRelease(cfg)
	if err != nil {
		t.Fatalf("findNextRelease: %v", err)
	}
	if next.TagName != "epoch-next" {
		t.Fatalf("expected epoch-next, got %s", next.TagName)
	}

	// Test findLatestStableRelease.
	latest, epoch, err := findLatestStableRelease(cfg)
	if err != nil {
		t.Fatalf("findLatestStableRelease: %v", err)
	}
	if epoch != "2026.2" {
		t.Fatalf("expected epoch 2026.2, got %s", epoch)
	}
	if latest.TagName != "epoch-2026.2" {
		t.Fatalf("expected tag epoch-2026.2, got %s", latest.TagName)
	}
}

func TestFindSpecificRelease(t *testing.T) {
	release := ghRelease{
		TagName: "epoch-2026.0",
		Assets: []ghAsset{
			{Name: "promise-linux-amd64", BrowserDownloadURL: "https://example.com/a"},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/tags/epoch-2026.0" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(release)
		} else if r.URL.Path == "/releases/tags/epoch-9999.9" {
			w.WriteHeader(404)
		} else {
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	cfg := syncConfig{apiBase: srv.URL}

	// Found.
	r, err := findSpecificRelease(cfg, "2026.0")
	if err != nil {
		t.Fatalf("findSpecificRelease: %v", err)
	}
	if r.TagName != "epoch-2026.0" {
		t.Fatalf("expected epoch-2026.0, got %s", r.TagName)
	}

	// Not found.
	_, err = findSpecificRelease(cfg, "9999.9")
	if err == nil {
		t.Fatal("expected error for unknown epoch")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFindNextReleaseNotFound(t *testing.T) {
	releases := []ghRelease{
		{TagName: "epoch-2026.0", Prerelease: false, Draft: false},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()

	cfg := syncConfig{apiBase: srv.URL}

	_, err := findNextRelease(cfg)
	if err == nil {
		t.Fatal("expected error when no next release exists")
	}
	if !strings.Contains(err.Error(), "epoch-next") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDownloadFile(t *testing.T) {
	content := []byte("binary content here")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.Write(content)
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	dest := filepath.Join(tmpDir, "downloaded")
	if err := downloadFile(srv.URL, dest); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("content mismatch")
	}
}
