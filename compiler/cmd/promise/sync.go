package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"djabi.dev/go/promise_lang/internal/module"
)

// Default GitHub release repository. Override with PROMISE_RELEASE_REPO env var
// (e.g., "myorg/promise-mirror") or PROMISE_RELEASE_URL for the full API base
// (e.g., "https://git.corp.com/api/v3/repos/myorg/promise").
const defaultReleaseRepo = "nicois/promise"

// syncConfig holds the resolved release API configuration.
type syncConfig struct {
	apiBase string // e.g., "https://api.github.com/repos/nicois/promise"
}

// resolveSyncConfig determines the GitHub release API base URL.
func resolveSyncConfig() syncConfig {
	if url := os.Getenv("PROMISE_RELEASE_URL"); url != "" {
		return syncConfig{apiBase: strings.TrimRight(url, "/")}
	}
	repo := defaultReleaseRepo
	if r := os.Getenv("PROMISE_RELEASE_REPO"); r != "" {
		repo = r
	}
	return syncConfig{apiBase: "https://api.github.com/repos/" + repo}
}

// ghRelease represents the subset of GitHub Release API response we need.
type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Prerelease bool      `json:"prerelease"`
	Draft      bool      `json:"draft"`
	Assets     []ghAsset `json:"assets"`
}

// ghAsset represents a single asset in a GitHub release.
type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// runSync implements `promise sync [epoch|next]`.
//
// Usage:
//
//	promise sync              latest stable epoch (latest non-prerelease tagged release)
//	promise sync 2026.3       specific stable epoch
//	promise sync next         latest pre-release build
func runSync(args []string) {
	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	if len(args) > 1 {
		fmt.Fprintln(os.Stderr, "usage: promise sync [epoch|next]")
		os.Exit(1)
	}

	cfg := resolveSyncConfig()

	var release *ghRelease
	var epoch string
	var err error

	switch {
	case target == "next":
		// Find the pre-release tagged epoch-next.
		release, err = findNextRelease(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		epoch = "next"

	case target == "":
		// Latest stable: find the latest non-prerelease epoch-* tag.
		release, epoch, err = findLatestStableRelease(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}

	default:
		// Specific epoch: look for tag epoch-<target>.
		epoch = target
		release, err = findSpecificRelease(cfg, target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}

	// For stable epochs (not "next"), check if already installed.
	if epoch != "next" {
		if isEpochInstalled(epoch) {
			fmt.Printf("epoch %s is already installed.\n", epoch)
			return
		}
	}

	// Determine platform binary name.
	binaryName := platformBinaryName()

	// Find the binary asset and SHA256SUMS asset in the release.
	binaryURL, shaURL, err := findAssets(release, binaryName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Download to a temp directory.
	tmpDir, err := os.MkdirTemp("", "promise-sync-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot create temp directory: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	// Download binary.
	binaryPath := filepath.Join(tmpDir, binaryName)
	fmt.Fprintf(os.Stderr, "downloading %s...\n", binaryName)
	if err := downloadFile(binaryURL, binaryPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Download and verify SHA256.
	if shaURL != "" {
		fmt.Fprintf(os.Stderr, "verifying checksum...\n")
		if err := verifySHA256(shaURL, binaryPath, binaryName); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}

	// chmod +x on Unix.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(binaryPath, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot make binary executable: %v\n", err)
			os.Exit(1)
		}
	}

	// Run `<binary> install` to install into epochs/<epoch>/.
	fmt.Fprintf(os.Stderr, "installing epoch %s...\n", epoch)
	installCmd := exec.Command(binaryPath, "install")
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	if err := installCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: install failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("epoch %s installed.\n", epoch)
}

// findNextRelease finds the pre-release tagged "epoch-next".
func findNextRelease(cfg syncConfig) (*ghRelease, error) {
	releases, err := fetchReleases(cfg)
	if err != nil {
		return nil, err
	}
	for _, r := range releases {
		if r.TagName == "epoch-next" && r.Prerelease && !r.Draft {
			return &r, nil
		}
	}
	return nil, fmt.Errorf("no pre-release 'epoch-next' found. The next channel may not be available yet")
}

// findLatestStableRelease finds the latest non-prerelease epoch-* release.
func findLatestStableRelease(cfg syncConfig) (*ghRelease, string, error) {
	releases, err := fetchReleases(cfg)
	if err != nil {
		return nil, "", err
	}

	// Collect stable epoch releases, sort by tag descending.
	var stable []ghRelease
	for _, r := range releases {
		if r.Draft || r.Prerelease {
			continue
		}
		if strings.HasPrefix(r.TagName, "epoch-") && r.TagName != "epoch-next" {
			stable = append(stable, r)
		}
	}
	if len(stable) == 0 {
		return nil, "", fmt.Errorf("no stable epoch releases found")
	}

	// Sort by tag name descending (lexicographic works for YYYY.N format).
	sort.Slice(stable, func(i, j int) bool {
		return stable[i].TagName > stable[j].TagName
	})

	tag := stable[0].TagName
	epoch := strings.TrimPrefix(tag, "epoch-")
	return &stable[0], epoch, nil
}

// findSpecificRelease finds the release tagged "epoch-<epoch>".
func findSpecificRelease(cfg syncConfig, epoch string) (*ghRelease, error) {
	tag := "epoch-" + epoch
	url := cfg.apiBase + "/releases/tags/" + tag
	resp, err := httpGetJSON(url)
	if err != nil {
		return nil, fmt.Errorf("cannot reach GitHub releases. Check your connection: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, fmt.Errorf("epoch %s is not available. Run `promise sync` for latest", epoch)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub API returned status %d for tag %s", resp.StatusCode, tag)
	}

	var release ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("parsing release response: %w", err)
	}
	return &release, nil
}

// fetchReleases fetches all releases from the GitHub API.
func fetchReleases(cfg syncConfig) ([]ghRelease, error) {
	url := cfg.apiBase + "/releases?per_page=100"
	resp, err := httpGetJSON(url)
	if err != nil {
		return nil, fmt.Errorf("cannot reach GitHub releases. Check your connection: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var releases []ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("parsing releases response: %w", err)
	}
	return releases, nil
}

// httpGetJSON performs a GET request with the Accept: application/json header
// and optional GitHub token authentication.
func httpGetJSON(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return http.DefaultClient.Do(req)
}

// platformBinaryName returns the expected release binary name for the current platform.
// Format: promise-<os>-<arch>[.exe]
func platformBinaryName() string {
	name := fmt.Sprintf("promise-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// findAssets locates the binary and SHA256SUMS assets in a release.
func findAssets(release *ghRelease, binaryName string) (binaryURL, shaURL string, err error) {
	for _, a := range release.Assets {
		switch a.Name {
		case binaryName:
			binaryURL = a.BrowserDownloadURL
		case "SHA256SUMS":
			shaURL = a.BrowserDownloadURL
		}
	}
	if binaryURL == "" {
		return "", "", fmt.Errorf("no binary available for %s-%s in release %s", runtime.GOOS, runtime.GOARCH, release.TagName)
	}
	return binaryURL, shaURL, nil
}

// downloadFile downloads a URL to a local file, printing progress to stderr.
func downloadFile(url, destPath string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("cannot reach GitHub releases. Check your connection: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("creating file: %w", err)
	}
	defer f.Close()

	totalBytes := resp.ContentLength
	written := int64(0)
	buf := make([]byte, 32*1024)
	lastPct := -1

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := f.Write(buf[:n]); wErr != nil {
				return fmt.Errorf("writing file: %w", wErr)
			}
			written += int64(n)
			if totalBytes > 0 {
				pct := int(written * 100 / totalBytes)
				if pct != lastPct && pct%10 == 0 {
					fmt.Fprintf(os.Stderr, "  %s %d/%d MB (%d%%)\n",
						filepath.Base(destPath),
						written/(1024*1024),
						totalBytes/(1024*1024),
						pct)
					lastPct = pct
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("reading response: %w", readErr)
		}
	}

	return nil
}

// verifySHA256 downloads SHA256SUMS and verifies the binary's checksum.
func verifySHA256(shaURL, binaryPath, binaryName string) error {
	// Download SHA256SUMS.
	resp, err := http.Get(shaURL)
	if err != nil {
		return fmt.Errorf("cannot download SHA256SUMS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("SHA256SUMS download failed with status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading SHA256SUMS: %w", err)
	}

	// Parse SHA256SUMS: each line is "<hash>  <filename>" or "<hash> <filename>".
	expectedHash := ""
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 && parts[1] == binaryName {
			expectedHash = parts[0]
			break
		}
	}
	if expectedHash == "" {
		return fmt.Errorf("no checksum found for %s in SHA256SUMS", binaryName)
	}

	// Compute actual hash.
	f, err := os.Open(binaryPath)
	if err != nil {
		return fmt.Errorf("opening binary for checksum: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("computing checksum: %w", err)
	}
	actualHash := hex.EncodeToString(h.Sum(nil))

	if !strings.EqualFold(actualHash, expectedHash) {
		return fmt.Errorf("checksum verification failed — download may be corrupted (expected %s, got %s)", expectedHash, actualHash)
	}

	return nil
}

// isEpochInstalled checks if an epoch has a binary installed.
func isEpochInstalled(epoch string) bool {
	epochDir, err := module.EpochDir(epoch)
	if err != nil {
		return false
	}
	binaryName := "promise"
	if runtime.GOOS == "windows" {
		binaryName = "promise.exe"
	}
	binPath := filepath.Join(epochDir, "bin", binaryName)
	_, err = os.Stat(binPath)
	return err == nil
}
