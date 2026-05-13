package main

import (
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
