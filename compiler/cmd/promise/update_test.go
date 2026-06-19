package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/module"
)

func TestResolveSyncConfigDefault(t *testing.T) {
	t.Setenv("PROMISE_RELEASE_URL", "")
	t.Setenv("PROMISE_RELEASE_REPO", "")
	cfg := resolveSyncConfig()
	if cfg.apiBase != "https://api.github.com/repos/promise-language/promise" {
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

// --- T0818 follow-up: source description, contextual GitHub errors, and
// authenticated (private-repo) asset downloads. ---

func TestDescribe(t *testing.T) {
	cases := []struct{ apiBase, want string }{
		// github.com API base renders as a friendly owner/repo.
		{"https://api.github.com/repos/promise-language/promise", "github.com/promise-language/promise"},
		// A custom (e.g. GitHub Enterprise) base is passed through verbatim.
		{"https://git.corp.com/api/v3/repos/myorg/promise", "https://git.corp.com/api/v3/repos/myorg/promise"},
	}
	for _, c := range cases {
		got := syncConfig{apiBase: c.apiBase}.describe()
		if got != c.want {
			t.Errorf("describe(%q) = %q, want %q", c.apiBase, got, c.want)
		}
	}
}

func TestTokenHint(t *testing.T) {
	t.Run("no token suggests setting one", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "")
		if got := tokenHint(); !strings.Contains(got, "set GITHUB_TOKEN") {
			t.Fatalf("expected hint to suggest setting a token, got: %s", got)
		}
	})
	t.Run("token set acknowledges it", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "x")
		if got := tokenHint(); !strings.Contains(got, "GITHUB_TOKEN is set") {
			t.Fatalf("expected hint to acknowledge the token, got: %s", got)
		}
	})
}

// fakeResp builds a minimal *http.Response for githubError tests.
func fakeResp(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestGithubError404NoToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	url := "https://api.github.com/repos/promise-language/promise/releases"
	err := githubError(url, fakeResp(http.StatusNotFound, `{"message":"Not Found"}`, nil))
	msg := err.Error()
	for _, want := range []string{url, "HTTP 404", "Not Found", "private", "set GITHUB_TOKEN"} {
		if !strings.Contains(msg, want) {
			t.Errorf("404 error missing %q; got:\n%s", want, msg)
		}
	}
}

func TestGithubError404WithToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")
	err := githubError("https://api.github.com/x", fakeResp(http.StatusNotFound, `{"message":"Not Found"}`, nil))
	if !strings.Contains(err.Error(), "GITHUB_TOKEN is set") {
		t.Fatalf("expected token-set hint, got:\n%s", err.Error())
	}
}

func TestGithubErrorRateLimit(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", "0")
	err := githubError("https://api.github.com/x", fakeResp(http.StatusForbidden, `{"message":"API rate limit exceeded"}`, h))
	msg := err.Error()
	if !strings.Contains(msg, "rate limit") || !strings.Contains(msg, "Set GITHUB_TOKEN to raise") {
		t.Fatalf("expected rate-limit hint, got:\n%s", msg)
	}
}

func TestGithubErrorForbiddenNonRateLimit(t *testing.T) {
	err := githubError("https://api.github.com/x", fakeResp(http.StatusForbidden, `{"message":"Resource not accessible"}`, nil))
	if !strings.Contains(err.Error(), "Access forbidden") {
		t.Fatalf("expected forbidden hint, got:\n%s", err.Error())
	}
}

func TestGithubErrorUnauthorized(t *testing.T) {
	err := githubError("https://api.github.com/x", fakeResp(http.StatusUnauthorized, `{"message":"Bad credentials"}`, nil))
	if !strings.Contains(err.Error(), "Authentication failed") {
		t.Fatalf("expected auth hint, got:\n%s", err.Error())
	}
}

func TestHttpGetRawHeaders(t *testing.T) {
	var gotAccept, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	t.Run("with token sends bearer + octet-stream", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "secret")
		resp, err := httpGetRaw(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if gotAccept != "application/octet-stream" {
			t.Errorf("Accept = %q, want application/octet-stream", gotAccept)
		}
		if gotAuth != "Bearer secret" {
			t.Errorf("Authorization = %q, want Bearer secret", gotAuth)
		}
	})

	t.Run("without token sends no authorization", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "")
		resp, err := httpGetRaw(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if gotAuth != "" {
			t.Errorf("Authorization = %q, want empty", gotAuth)
		}
	})
}

func TestFindAssetsPrefersAPIURLWithToken(t *testing.T) {
	release := &ghRelease{
		TagName: "epoch-2026.0",
		Assets: []ghAsset{
			{Name: "promise-linux-amd64", URL: "https://api.github.com/assets/1", BrowserDownloadURL: "https://example.com/dl/1"},
			{Name: "SHA256SUMS", URL: "https://api.github.com/assets/2", BrowserDownloadURL: "https://example.com/dl/2"},
		},
	}

	t.Run("with token uses API url", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "tok")
		bin, sha, err := findAssets(release, "promise-linux-amd64")
		if err != nil {
			t.Fatal(err)
		}
		if bin != "https://api.github.com/assets/1" || sha != "https://api.github.com/assets/2" {
			t.Fatalf("expected API urls, got bin=%s sha=%s", bin, sha)
		}
	})

	t.Run("without token uses browser url", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "")
		bin, sha, err := findAssets(release, "promise-linux-amd64")
		if err != nil {
			t.Fatal(err)
		}
		if bin != "https://example.com/dl/1" || sha != "https://example.com/dl/2" {
			t.Fatalf("expected browser urls, got bin=%s sha=%s", bin, sha)
		}
	})

	t.Run("token set but API url missing falls back to browser", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "tok")
		rel := &ghRelease{TagName: "epoch-2026.0", Assets: []ghAsset{
			{Name: "promise-linux-amd64", BrowserDownloadURL: "https://example.com/only-browser"},
		}}
		bin, _, err := findAssets(rel, "promise-linux-amd64")
		if err != nil {
			t.Fatal(err)
		}
		if bin != "https://example.com/only-browser" {
			t.Fatalf("expected fallback to browser url, got %s", bin)
		}
	})
}

// --- T0825: channel/update model — assetSHAFromSums, shortBuildID,
// update check, and use download-on-demand. ---

func TestAssetSHAFromSums(t *testing.T) {
	const assetName = "promise-linux-amd64.gz"
	want := "1111111111111111111111111111111111111111111111111111111111111111"
	sums := want + "  " + assetName + "\n" +
		"0000000000000000000000000000000000000000000000000000000000000000  promise-linux-amd64-full.gz\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sums))
	}))
	defer srv.Close()

	got, err := assetSHAFromSums(srv.URL, assetName)
	if err != nil {
		t.Fatalf("assetSHAFromSums: %v", err)
	}
	if got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
}

func TestAssetSHAFromSumsMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("abc  promise-other.gz\n"))
	}))
	defer srv.Close()

	_, err := assetSHAFromSums(srv.URL, "promise-linux-amd64.gz")
	if err == nil {
		t.Fatal("expected error for missing asset entry")
	}
	if !strings.Contains(err.Error(), "no checksum found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestShortBuildID(t *testing.T) {
	if got := shortBuildID(""); got != "(none)" {
		t.Errorf("empty: got %q, want (none)", got)
	}
	if got := shortBuildID("abc"); got != "abc" {
		t.Errorf("short: got %q, want abc", got)
	}
	long := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if got := shortBuildID(long); got != long[:7] {
		t.Errorf("long: got %q, want %q", got, long[:7])
	}
}

// stableReleasesServer serves a GitHub releases list with the given stable
// epoch tags (no prereleases).
func stableReleasesServer(t *testing.T, tags ...string) *httptest.Server {
	t.Helper()
	var releases []ghRelease
	for _, tag := range tags {
		releases = append(releases, ghRelease{TagName: tag})
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(releases)
	}))
}

func TestRunUpdateCheckStableAvailable(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)
	t.Setenv("GITHUB_TOKEN", "")
	if err := os.WriteFile(filepath.Join(tmp, "active"), []byte("2026.0\n"), 0644); err != nil {
		t.Fatal(err)
	}

	srv := stableReleasesServer(t, "epoch-2026.0", "epoch-2026.2")
	defer srv.Close()
	t.Setenv("PROMISE_RELEASE_URL", srv.URL)

	out := captureStdout(t, func() {
		captureStderr(func() { runUpdateCheck(nil) })
	})
	if !strings.Contains(out, "Update available: epoch 2026.0 -> 2026.2") {
		t.Errorf("expected stable update-available message, got: %s", out)
	}
}

func TestRunUpdateCheckStableUpToDate(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)
	t.Setenv("GITHUB_TOKEN", "")
	if err := os.WriteFile(filepath.Join(tmp, "active"), []byte("2026.2\n"), 0644); err != nil {
		t.Fatal(err)
	}

	srv := stableReleasesServer(t, "epoch-2026.0", "epoch-2026.2")
	defer srv.Close()
	t.Setenv("PROMISE_RELEASE_URL", srv.URL)

	out := captureStdout(t, func() {
		captureStderr(func() { runUpdateCheck(nil) })
	})
	if !strings.Contains(out, "Up to date (epoch 2026.2)") {
		t.Errorf("expected up-to-date message, got: %s", out)
	}
}

// TestRunUpdateCheckStableNumericOrder pins the latent-bug fix end-to-end: with
// 2026.9 active and 2026.10 published, check must report an update (lexicographic
// ordering would wrongly call 2026.9 newer).
func TestRunUpdateCheckStableNumericOrder(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)
	t.Setenv("GITHUB_TOKEN", "")
	if err := os.WriteFile(filepath.Join(tmp, "active"), []byte("2026.9\n"), 0644); err != nil {
		t.Fatal(err)
	}

	srv := stableReleasesServer(t, "epoch-2026.9", "epoch-2026.10")
	defer srv.Close()
	t.Setenv("PROMISE_RELEASE_URL", srv.URL)

	out := captureStdout(t, func() {
		captureStderr(func() { runUpdateCheck(nil) })
	})
	if !strings.Contains(out, "Update available: epoch 2026.9 -> 2026.10") {
		t.Errorf("expected 2026.10 to rank above 2026.9, got: %s", out)
	}
}

func TestRunUpdateCheckJSONShape(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)
	t.Setenv("GITHUB_TOKEN", "")
	if err := os.WriteFile(filepath.Join(tmp, "active"), []byte("2026.0\n"), 0644); err != nil {
		t.Fatal(err)
	}

	srv := stableReleasesServer(t, "epoch-2026.0", "epoch-2026.2")
	defer srv.Close()
	t.Setenv("PROMISE_RELEASE_URL", srv.URL)

	out := captureStdout(t, func() {
		captureStderr(func() { runUpdateCheck([]string{"--json"}) })
	})
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output is not valid JSON (%v): %s", err, out)
	}
	if res["channel"] != "stable" || res["active"] != "2026.0" || res["latest"] != "2026.2" {
		t.Fatalf("unexpected JSON fields: %v", res)
	}
	if res["updateAvailable"] != true {
		t.Fatalf("expected updateAvailable=true, got: %v", res)
	}
}

// nextReleasesServer serves a releases list containing an epoch-next prerelease
// whose SHA256SUMS lists remoteSHA for the current platform asset.
func nextReleasesServer(t *testing.T, remoteSHA string) *httptest.Server {
	t.Helper()
	asset := platformAssetName()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/SHA256SUMS":
			_, _ = w.Write([]byte(remoteSHA + "  " + asset + "\n"))
		default:
			releases := []ghRelease{
				{TagName: "epoch-next", Prerelease: true, Assets: []ghAsset{
					{Name: asset, BrowserDownloadURL: srv.URL + "/" + asset},
					{Name: "SHA256SUMS", BrowserDownloadURL: srv.URL + "/SHA256SUMS"},
				}},
			}
			_ = json.NewEncoder(w).Encode(releases)
		}
	}))
	return srv
}

func TestRunUpdateCheckNextAvailable(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)
	t.Setenv("GITHUB_TOKEN", "")
	if err := module.WriteUpdateChannel(module.ChannelNext); err != nil {
		t.Fatal(err)
	}
	local := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	remote := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := module.WriteEpochBuildID("next", local); err != nil {
		t.Fatal(err)
	}

	srv := nextReleasesServer(t, remote)
	defer srv.Close()
	t.Setenv("PROMISE_RELEASE_URL", srv.URL)

	out := captureStdout(t, func() {
		captureStderr(func() { runUpdateCheck(nil) })
	})
	// 7-char shortening in human output; full hashes must NOT appear.
	if !strings.Contains(out, "Update available: build "+local[:7]+" -> "+remote[:7]) {
		t.Errorf("expected shortened next update message, got: %s", out)
	}
	if strings.Contains(out, local) || strings.Contains(out, remote) {
		t.Errorf("human output must not contain full hashes, got: %s", out)
	}
}

func TestRunUpdateCheckNextUpToDate(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)
	t.Setenv("GITHUB_TOKEN", "")
	if err := module.WriteUpdateChannel(module.ChannelNext); err != nil {
		t.Fatal(err)
	}
	same := "ccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	if err := module.WriteEpochBuildID("next", same); err != nil {
		t.Fatal(err)
	}

	srv := nextReleasesServer(t, same)
	defer srv.Close()
	t.Setenv("PROMISE_RELEASE_URL", srv.URL)

	out := captureStdout(t, func() {
		captureStderr(func() { runUpdateCheck(nil) })
	})
	if !strings.Contains(out, "Up to date (build "+same[:7]+")") {
		t.Errorf("expected up-to-date next message, got: %s", out)
	}
}

func TestRunUpdateCheckNextJSONFullHash(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)
	t.Setenv("GITHUB_TOKEN", "")
	if err := module.WriteUpdateChannel(module.ChannelNext); err != nil {
		t.Fatal(err)
	}
	local := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd0"
	remote := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee1"
	if err := module.WriteEpochBuildID("next", local); err != nil {
		t.Fatal(err)
	}

	srv := nextReleasesServer(t, remote)
	defer srv.Close()
	t.Setenv("PROMISE_RELEASE_URL", srv.URL)

	out := captureStdout(t, func() {
		captureStderr(func() { runUpdateCheck([]string{"--json"}) })
	})
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output is not valid JSON (%v): %s", err, out)
	}
	// JSON carries the FULL hashes (not shortened).
	if res["localBuild"] != local || res["remoteBuild"] != remote {
		t.Fatalf("expected full build hashes in JSON, got: %v", res)
	}
	if res["updateAvailable"] != true {
		t.Fatalf("expected updateAvailable=true, got: %v", res)
	}
}

// TestRunUseDownloadsOnDemand: `promise use <epoch>` on a missing epoch now
// downloads from releases instead of erroring "Run: promise sync" (T0825).
// Run in a subprocess (runUse calls os.Exit). The mock server 404s the tag so
// the download fails before the exec-install step — enough to prove the attempt.
func TestRunUseDownloadsOnDemand(t *testing.T) {
	if os.Getenv("TEST_USE_ONDEMAND") == "1" {
		runUse([]string{"9999.9"})
		return
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tmp := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestRunUseDownloadsOnDemand")
	cmd.Env = append(os.Environ(),
		"TEST_USE_ONDEMAND=1",
		"PROMISE_HOME="+tmp,
		"PROMISE_RELEASE_URL="+srv.URL,
		"GITHUB_TOKEN=",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit when on-demand download fails")
	}
	s := string(out)
	if !strings.Contains(s, "downloading from") {
		t.Errorf("expected an on-demand download attempt, got: %s", s)
	}
	if !strings.Contains(s, "not available") {
		t.Errorf("expected 'not available' download failure, got: %s", s)
	}
	if strings.Contains(s, "promise sync") {
		t.Errorf("stale 'promise sync' hint should be gone, got: %s", s)
	}
}

// TestEnsureEpochPresentNoop: when the epoch's compiler binary already exists,
// ensureEpochPresent is a pure no-op — no download, no touch of the active
// pointer (T0977). Exercised directly (no subprocess) since the present path
// never calls os.Exit.
func TestEnsureEpochPresentNoop(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)
	t.Setenv("PROMISE_RELEASE_URL", "http://127.0.0.1:0") // any download would fail loudly

	// Stage a present epoch binary.
	binName := "promise"
	if runtime.GOOS == "windows" {
		binName = "promise.exe"
	}
	binDir := filepath.Join(tmp, "epochs", "2025.3", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, binName), []byte("x"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := module.WriteActiveEpoch("2026.0"); err != nil {
		t.Fatal(err)
	}

	if err := ensureEpochPresent("2025.3"); err != nil {
		t.Fatalf("present epoch should be a no-op, got: %v", err)
	}
	// Active pointer untouched.
	data, err := os.ReadFile(filepath.Join(tmp, "active"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "2026.0" {
		t.Errorf("present-epoch no-op must not change active; got: %q", string(data))
	}
}

// TestRunInstallEpochKeepsActive: `promise install <epoch>` on a missing epoch
// downloads it WITHOUT touching the active pointer (presence-only; T0977). The
// mock server 404s the tag so the download fails before the exec-install step —
// enough to prove (a) an on-demand download was attempted and (b) the pre-set
// active pointer is untouched (it is never written before the download path).
func TestRunInstallEpochKeepsActive(t *testing.T) {
	if os.Getenv("TEST_INSTALL_EPOCH") == "1" {
		runInstall([]string{"9999.9"})
		return
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "active"), []byte("2026.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunInstallEpochKeepsActive")
	cmd.Env = append(os.Environ(),
		"TEST_INSTALL_EPOCH=1",
		"PROMISE_HOME="+tmp,
		"PROMISE_RELEASE_URL="+srv.URL,
		"GITHUB_TOKEN=",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit when on-demand download fails")
	}
	s := string(out)
	if !strings.Contains(s, "downloading from") {
		t.Errorf("expected an on-demand download attempt, got: %s", s)
	}
	// The active pointer must be exactly as we set it — install never activates.
	data, rerr := os.ReadFile(filepath.Join(tmp, "active"))
	if rerr != nil {
		t.Fatalf("active file should still exist: %v", rerr)
	}
	if strings.TrimSpace(string(data)) != "2026.0" {
		t.Errorf("install <epoch> must not change active; got: %q", string(data))
	}
}

// TestRunInstallEpochRejectsNext: `promise install next` is the same category
// error as `use next` — "next" is a rolling release channel, not a concrete
// epoch (T0825/T0977). It must reject (pointing at `promise update channel
// next`) without writing the active pointer. Run in a subprocess (it exits).
func TestRunInstallEpochRejectsNext(t *testing.T) {
	if os.Getenv("TEST_INSTALL_NEXT_REJECT") == "1" {
		runInstall([]string{"next"})
		return
	}

	tmp := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestRunInstallEpochRejectsNext")
	cmd.Env = append(os.Environ(),
		"TEST_INSTALL_NEXT_REJECT=1",
		"PROMISE_HOME="+tmp,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for `install next`")
	}
	s := string(out)
	if !strings.Contains(s, "release channel, not an epoch") {
		t.Errorf("expected channel-not-epoch rejection, got: %s", s)
	}
	if !strings.Contains(s, "promise update channel next") {
		t.Errorf("expected pointer to `update channel next`, got: %s", s)
	}
	if data, rerr := os.ReadFile(filepath.Join(tmp, "active")); rerr == nil {
		t.Errorf("install next must not write the active file, got: %q", string(data))
	}
}

// TestRunInstallEpochPresentNoop: the full `promise install <epoch>` command on
// an already-present epoch is a no-op that prints the success line and leaves a
// pre-set active pointer (for a DIFFERENT epoch) untouched (T0977 acceptance:
// "install <epoch> for an installed epoch ... leaves active unchanged"). This
// drives the whole dispatch — runInstall's positional overload → runInstallEpoch
// → ensureEpochPresent's already-present short-circuit — and the success Printf
// that the download-fail tests never reach. Run in a subprocess (clean env, and
// a 127.0.0.1:0 release URL so any accidental download would fail loudly).
func TestRunInstallEpochPresentNoop(t *testing.T) {
	if os.Getenv("TEST_INSTALL_PRESENT") == "1" {
		runInstall([]string{"2025.3"})
		return
	}

	tmp := t.TempDir()
	binName := "promise"
	if runtime.GOOS == "windows" {
		binName = "promise.exe"
	}
	binDir := filepath.Join(tmp, "epochs", "2025.3", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, binName), []byte("x"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "active"), []byte("2026.0\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestRunInstallEpochPresentNoop")
	cmd.Env = append(os.Environ(),
		"TEST_INSTALL_PRESENT=1",
		"PROMISE_HOME="+tmp,
		"PROMISE_RELEASE_URL=http://127.0.0.1:0",
		"GITHUB_TOKEN=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install <present-epoch> should exit 0, got %v\n%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "Installed epoch 2025.3") || !strings.Contains(s, "active epoch unchanged") {
		t.Errorf("expected presence-only success line, got: %s", s)
	}
	if strings.Contains(s, "downloading from") {
		t.Errorf("present epoch must not trigger a download, got: %s", s)
	}
	// The active pointer must still name the OTHER epoch — install never activates.
	data, rerr := os.ReadFile(filepath.Join(tmp, "active"))
	if rerr != nil {
		t.Fatalf("active file should still exist: %v", rerr)
	}
	if strings.TrimSpace(string(data)) != "2026.0" {
		t.Errorf("install <epoch> must not change active; got: %q", string(data))
	}
}

// TestRunUsePresentSetsActive: `promise use <epoch>` on an already-present epoch
// activates it WITHOUT downloading (T0977 acceptance: "use <epoch> still ... sets
// active"). The present short-circuit in ensureEpochPresent means no network is
// touched; the WriteActiveEpoch step then flips the active pointer from a
// different epoch to this one. Run in a subprocess (clean env).
func TestRunUsePresentSetsActive(t *testing.T) {
	if os.Getenv("TEST_USE_PRESENT") == "1" {
		runUse([]string{"2025.3"})
		return
	}

	tmp := t.TempDir()
	binName := "promise"
	if runtime.GOOS == "windows" {
		binName = "promise.exe"
	}
	binDir := filepath.Join(tmp, "epochs", "2025.3", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, binName), []byte("x"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "active"), []byte("2026.0\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestRunUsePresentSetsActive")
	cmd.Env = append(os.Environ(),
		"TEST_USE_PRESENT=1",
		"PROMISE_HOME="+tmp,
		"PROMISE_RELEASE_URL=http://127.0.0.1:0",
		"GITHUB_TOKEN=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("use <present-epoch> should exit 0, got %v\n%s", err, out)
	}
	if strings.Contains(string(out), "downloading from") {
		t.Errorf("present epoch must not trigger a download, got: %s", out)
	}
	// use MUST activate the requested epoch (flipping it from 2026.0 → 2025.3).
	data, rerr := os.ReadFile(filepath.Join(tmp, "active"))
	if rerr != nil {
		t.Fatalf("active file should exist after use: %v", rerr)
	}
	if strings.TrimSpace(string(data)) != "2025.3" {
		t.Errorf("use <epoch> must set active to that epoch; got: %q", string(data))
	}
}

// TestDownloadAndInstallNoAsset: downloadAndInstall must fail fast (before any
// download or exec) when the release carries no asset for the current platform.
// This is the only downloadAndInstall path reachable without executing a real
// downloaded binary; the success tail is integration-level.
func TestDownloadAndInstallNoAsset(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	release := &ghRelease{
		TagName: "epoch-2026.0",
		Assets:  []ghAsset{{Name: "some-other-platform.gz", BrowserDownloadURL: "https://example.com/x"}},
	}
	err := downloadAndInstall(release, "2026.0")
	if err == nil {
		t.Fatal("expected error when no platform asset is present")
	}
	if !strings.Contains(err.Error(), "no binary available") {
		t.Fatalf("expected 'no binary available' error, got: %v", err)
	}
}

// TestDoUpdateNoStableRelease: bare `promise update` on the (default) stable
// channel resolves the latest stable release; when the release list has none it
// must error and exit 1 rather than proceed. Exercises doUpdate's channel read +
// stable resolution + error handling (the success tail execs a downloaded
// binary, which is integration-level). Run in a subprocess (doUpdate exits).
func TestDoUpdateNoStableRelease(t *testing.T) {
	if os.Getenv("TEST_DO_UPDATE_NO_STABLE") == "1" {
		runUpdate(nil)
		return
	}

	// A releases list with only non-epoch tags → no stable epoch found.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]ghRelease{{TagName: "v1.0.0"}})
	}))
	defer srv.Close()

	tmp := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestDoUpdateNoStableRelease")
	cmd.Env = append(os.Environ(),
		"TEST_DO_UPDATE_NO_STABLE=1",
		"PROMISE_HOME="+tmp,
		"PROMISE_RELEASE_URL="+srv.URL,
		"GITHUB_TOKEN=",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit when no stable release is found")
	}
	s := string(out)
	// The status line names the resolved channel (stable by default).
	if !strings.Contains(s, "channel: stable") {
		t.Errorf("expected stable channel in status line, got: %s", s)
	}
	if !strings.Contains(s, "no stable epoch releases found") {
		t.Errorf("expected 'no stable epoch releases found' error, got: %s", s)
	}
}

// TestRunUpdateChannelPrint: `promise update channel` (no arg) prints the
// current channel without mutating anything. Covers the no-arg branch of
// runUpdateChannel directly (the set branch calls doUpdate which exits).
func TestRunUpdateChannelPrint(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// Default with no channel file → "stable".
	out := captureStdout(t, func() {
		captureStderr(func() { runUpdateChannel(nil) })
	})
	if strings.TrimSpace(out) != module.ChannelStable {
		t.Errorf("expected %q, got %q", module.ChannelStable, strings.TrimSpace(out))
	}

	// After persisting "next" it must print "next".
	if err := module.WriteUpdateChannel(module.ChannelNext); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() {
		captureStderr(func() { runUpdateChannel(nil) })
	})
	if strings.TrimSpace(out) != module.ChannelNext {
		t.Errorf("expected %q, got %q", module.ChannelNext, strings.TrimSpace(out))
	}
}

// TestRunUpdateDispatchChannel: `promise update channel` routes through the
// runUpdate dispatcher to runUpdateChannel (covers the "channel" case of the
// dispatcher's switch, the no-arg print path being side-effect-free).
func TestRunUpdateDispatchChannel(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	out := captureStdout(t, func() {
		captureStderr(func() { runUpdate([]string{"channel"}) })
	})
	if strings.TrimSpace(out) != module.ChannelStable {
		t.Errorf("expected dispatcher to print current channel %q, got %q", module.ChannelStable, strings.TrimSpace(out))
	}
}

// TestRunUpdateChannelInvalid: `promise update channel beta` rejects an
// unknown channel name and exits 1 without persisting anything. Run in a
// subprocess (runUpdateChannel calls os.Exit on the WriteUpdateChannel error).
func TestRunUpdateChannelInvalid(t *testing.T) {
	if os.Getenv("TEST_UPDATE_CHANNEL_INVALID") == "1" {
		runUpdateChannel([]string{"beta"})
		return
	}
	tmp := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestRunUpdateChannelInvalid")
	cmd.Env = append(os.Environ(),
		"TEST_UPDATE_CHANNEL_INVALID=1",
		"PROMISE_HOME="+tmp,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for invalid channel name")
	}
	if !strings.Contains(string(out), "invalid update channel") {
		t.Errorf("expected invalid-channel error, got: %s", string(out))
	}
	// Nothing persisted.
	if _, statErr := os.Stat(filepath.Join(tmp, "channel")); !os.IsNotExist(statErr) {
		t.Errorf("expected no channel file after invalid write, stat err: %v", statErr)
	}
}

// TestRunUpdateChannelTooManyArgs: `promise update channel a b` is a usage
// error (exactly zero or one argument). Run in a subprocess (it exits 1).
func TestRunUpdateChannelTooManyArgs(t *testing.T) {
	if os.Getenv("TEST_UPDATE_CHANNEL_ARGS") == "1" {
		runUpdateChannel([]string{"stable", "extra"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunUpdateChannelTooManyArgs")
	cmd.Env = append(os.Environ(), "TEST_UPDATE_CHANNEL_ARGS=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for too many channel args")
	}
	if !strings.Contains(string(out), "usage: promise update channel") {
		t.Errorf("expected channel usage message, got: %s", string(out))
	}
}

// TestRunUpdateCheckBadFlag: `promise update check --bogus` is a usage error.
// Run in a subprocess (runUpdateCheck exits 1 on an unknown flag).
func TestRunUpdateCheckBadFlag(t *testing.T) {
	if os.Getenv("TEST_UPDATE_CHECK_FLAG") == "1" {
		runUpdateCheck([]string{"--bogus"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunUpdateCheckBadFlag")
	cmd.Env = append(os.Environ(), "TEST_UPDATE_CHECK_FLAG=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for unknown check flag")
	}
	if !strings.Contains(string(out), "usage: promise update check") {
		t.Errorf("expected check usage message, got: %s", string(out))
	}
}

// TestRunUpdateCheckNextNoSums: on the next channel, if the epoch-next release
// has the platform asset but NO SHA256SUMS asset, `update check` cannot
// determine a build identity and must error out clearly rather than silently
// claim "up to date". Run in a subprocess (runUpdateCheck exits 1).
func TestRunUpdateCheckNextNoSums(t *testing.T) {
	if os.Getenv("TEST_UPDATE_CHECK_NOSUMS") == "1" {
		runUpdateCheck(nil)
		return
	}

	// Server: an epoch-next prerelease with the platform asset but no SHA256SUMS.
	asset := platformAssetName()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		releases := []ghRelease{
			{TagName: "epoch-next", Prerelease: true, Assets: []ghAsset{
				{Name: asset, BrowserDownloadURL: "https://example.com/" + asset},
			}},
		}
		_ = json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()

	tmp := t.TempDir()
	// Persist the next channel into the shared PROMISE_HOME the subprocess reads.
	t.Setenv("PROMISE_HOME", tmp)
	if err := module.WriteUpdateChannel(module.ChannelNext); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunUpdateCheckNextNoSums")
	cmd.Env = append(os.Environ(),
		"TEST_UPDATE_CHECK_NOSUMS=1",
		"PROMISE_HOME="+tmp,
		"PROMISE_RELEASE_URL="+srv.URL,
		"GITHUB_TOKEN=",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit when next release has no SHA256SUMS")
	}
	if !strings.Contains(string(out), "no SHA256SUMS asset") {
		t.Errorf("expected 'no SHA256SUMS asset' error, got: %s", string(out))
	}
}

// TestRunUseRejectsNext: `promise use next` is a category error — "next" is a
// rolling release channel, not a concrete epoch, and following it activates the
// real YYYY.N epoch the next branch carries (there is no epochs/next/ install).
// `use next` must reject and point at `promise update channel next` rather than
// leave a dangling active pointer (T0825). Run in a subprocess (runUse exits).
func TestRunUseRejectsNext(t *testing.T) {
	if os.Getenv("TEST_USE_NEXT_REJECT") == "1" {
		runUse([]string{"next"})
		return
	}

	tmp := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestRunUseRejectsNext")
	cmd.Env = append(os.Environ(),
		"TEST_USE_NEXT_REJECT=1",
		"PROMISE_HOME="+tmp,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for `use next`")
	}
	s := string(out)
	if !strings.Contains(s, "release channel, not an epoch") {
		t.Errorf("expected channel-not-epoch rejection, got: %s", s)
	}
	if !strings.Contains(s, "promise update channel next") {
		t.Errorf("expected pointer to `update channel next`, got: %s", s)
	}
	// Must not have written a dangling active="next" pointer.
	if data, rerr := os.ReadFile(filepath.Join(tmp, "active")); rerr == nil {
		t.Errorf("use next must not write the active file, got: %q", string(data))
	}
}
