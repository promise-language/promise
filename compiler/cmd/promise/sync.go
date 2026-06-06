package main

import (
	"compress/gzip"
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

	"github.com/promise-language/promise/compiler/internal/module"
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
//	promise sync 2026.0       specific stable epoch
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

	if err := downloadAndInstall(release, epoch); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("epoch %s installed.\n", epoch)
}

// runUpdate implements `promise update [epoch|next]` — self-update of the
// toolchain (§2.6). It downloads the newer compiler for the target epoch/channel
// and runs its `install`, which forward-updates the on-PATH stub and stages
// blobs. This is distinct from `promise sync` (install an additional epoch
// side-by-side): `update` advances the toolchain in place. Dependency `[require]`
// pin updates live under `promise pkg update` (T0770).
//
// Usage:
//
//	promise update          newer compiler for the active epoch (or latest stable)
//	promise update 2026.0   newer compiler for a specific epoch
//	promise update next     latest pre-release build
func runUpdate(args []string) {
	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	if len(args) > 1 {
		fmt.Fprintln(os.Stderr, "usage: promise update [epoch|next]")
		os.Exit(1)
	}

	cfg := resolveSyncConfig()

	// Default (no arg): update the currently active epoch in place.
	if target == "" {
		if active, err := module.ActiveEpoch(); err == nil && active != "" && active != "next" {
			target = active
		}
	}

	var release *ghRelease
	var epoch string
	var err error

	switch {
	case target == "next":
		release, err = findNextRelease(cfg)
		epoch = "next"
	case target == "":
		release, epoch, err = findLatestStableRelease(cfg)
	default:
		epoch = target
		release, err = findSpecificRelease(cfg, target)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "updating toolchain to epoch %s...\n", epoch)
	if err := downloadAndInstall(release, epoch); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("epoch %s installed.\n", epoch)
}

// downloadAndInstall downloads the platform binary for a release, verifies its
// SHA256, decompresses it (assets are gzip-compressed — T0796), and runs the
// downloaded binary's `install` (which installs into epochs/<epoch>/,
// forward-updates the stub, and stages blobs). Shared by `promise sync` and
// `promise update` (T0770).
func downloadAndInstall(release *ghRelease, epoch string) error {
	assetName := platformAssetName()    // promise-<os>-<arch>[.exe].gz
	runtimeName := platformBinaryName() // promise-<os>-<arch>[.exe]

	assetURL, shaURL, err := findAssets(release, assetName)
	if err != nil {
		return err
	}

	tmpDir, err := os.MkdirTemp("", "promise-update-*")
	if err != nil {
		return fmt.Errorf("cannot create temp directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	gzPath := filepath.Join(tmpDir, assetName)
	binaryPath := filepath.Join(tmpDir, runtimeName)

	fmt.Fprintf(os.Stderr, "downloading %s...\n", assetName)
	if err := downloadFile(assetURL, gzPath); err != nil {
		return err
	}

	if shaURL != "" {
		// SHA256SUMS is computed over the .gz asset (what was downloaded) — verify
		// the compressed file, not the decompressed binary.
		fmt.Fprintf(os.Stderr, "verifying checksum...\n")
		if err := verifySHA256(shaURL, gzPath, assetName); err != nil {
			return err
		}
	}

	fmt.Fprintf(os.Stderr, "decompressing...\n")
	if err := gunzipFile(gzPath, binaryPath); err != nil {
		return err
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(binaryPath, 0755); err != nil {
			return fmt.Errorf("cannot make binary executable: %w", err)
		}
	}

	fmt.Fprintf(os.Stderr, "installing epoch %s...\n", epoch)
	installCmd := exec.Command(binaryPath, "install")
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	if err := installCmd.Run(); err != nil {
		return fmt.Errorf("install failed: %w", err)
	}
	return nil
}

// gunzipFile decompresses a gzip file at src to dst.
func gunzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer in.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	defer out.Close()
	if _, err := io.Copy(out, gz); err != nil {
		return fmt.Errorf("decompressing: %w", err)
	}
	return nil
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

// platformBinaryName returns the runtime binary name for the current platform —
// what the file is called after decompression and what gets executed.
// Format: promise-<os>-<arch>[.exe]
func platformBinaryName() string {
	name := fmt.Sprintf("promise-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// platformAssetName returns the published GitHub release asset name for the
// current platform. Assets are gzip-compressed (T0796) — the .gz suffix is
// appended to the runtime binary name.
func platformAssetName() string {
	return platformBinaryName() + ".gz"
}

// findAssets locates the named release asset and the SHA256SUMS asset.
func findAssets(release *ghRelease, assetName string) (assetURL, shaURL string, err error) {
	for _, a := range release.Assets {
		switch a.Name {
		case assetName:
			assetURL = a.BrowserDownloadURL
		case "SHA256SUMS":
			shaURL = a.BrowserDownloadURL
		}
	}
	if assetURL == "" {
		return "", "", fmt.Errorf("no binary available for %s-%s in release %s", runtime.GOOS, runtime.GOARCH, release.TagName)
	}
	return assetURL, shaURL, nil
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
