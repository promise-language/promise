package main

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
const defaultReleaseRepo = "promise-language/promise"

// syncConfig holds the resolved release API configuration.
type syncConfig struct {
	apiBase string // e.g., "https://api.github.com/repos/promise-language/promise"
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

// describe returns a human-readable name for the release source, used in the
// "checking ..." status line and in error messages so a failure says *where* it
// was looking, not just that it failed.
func (c syncConfig) describe() string {
	const ghPrefix = "https://api.github.com/repos/"
	if rest, ok := strings.CutPrefix(c.apiBase, ghPrefix); ok {
		return "github.com/" + rest
	}
	return c.apiBase
}

// tokenHint returns advice about GITHUB_TOKEN tailored to whether one is already
// set — shown on 404/auth failures, the common case when the repository is
// private.
func tokenHint() string {
	if os.Getenv("GITHUB_TOKEN") != "" {
		return "GITHUB_TOKEN is set — confirm it has 'repo' scope and read access to this repository."
	}
	return "If the repository is private, set GITHUB_TOKEN to a personal access token with 'repo' scope."
}

// githubError builds a contextual error for a non-2xx GitHub response. It surfaces
// the URL that was requested, the HTTP status, any message GitHub returned in the
// body, and an actionable hint for the most common failures (private repo, bad
// token, rate limit).
func githubError(url string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var payload struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &payload)

	var b strings.Builder
	fmt.Fprintf(&b, "request to %s failed with HTTP %d", url, resp.StatusCode)
	if payload.Message != "" {
		fmt.Fprintf(&b, " (%s)", payload.Message)
	}

	switch resp.StatusCode {
	case http.StatusNotFound:
		b.WriteString("\n  The repository or release could not be found. Likely causes:")
		b.WriteString("\n    - the repository is private and the request is unauthenticated or lacks access, or")
		b.WriteString("\n    - the release/epoch does not exist yet.")
		fmt.Fprintf(&b, "\n  %s", tokenHint())
	case http.StatusUnauthorized:
		b.WriteString("\n  Authentication failed — GITHUB_TOKEN is missing or invalid.")
	case http.StatusForbidden:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			b.WriteString("\n  GitHub API rate limit exceeded.")
			if os.Getenv("GITHUB_TOKEN") == "" {
				b.WriteString(" Set GITHUB_TOKEN to raise the limit.")
			}
		} else {
			b.WriteString("\n  Access forbidden — the token may lack permission for this repository.")
		}
	}
	return errors.New(b.String())
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
	URL                string `json:"url"` // API asset URL — required for authenticated (private-repo) downloads
	BrowserDownloadURL string `json:"browser_download_url"`
}

// runUpdate implements `promise update` and its subverbs (T0825) — self-update
// of the toolchain (§2.6). The update channel (what `update` follows) is
// orthogonal to the active epoch (which compiler runs builds): the channel is
// persisted in <PromiseHome>/channel and selected with `update channel`, while
// the active epoch is selected with `promise use`. Dependency `[require]` pin
// updates live under `promise pkg update` (T0770).
//
// Usage:
//
//	promise update                        follow the channel: install its latest + activate
//	promise update check [--json]         report whether an update is available (no changes)
//	promise update channel                print the current update channel
//	promise update channel <stable|next>  set the channel and immediately follow it
func runUpdate(args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "check":
			runUpdateCheck(args[1:])
			return
		case "channel":
			runUpdateChannel(args[1:])
			return
		default:
			// `update` no longer takes an epoch argument — a specific epoch is now
			// `promise use <epoch>` (which downloads on demand). This removes the old
			// update-vs-sync target ambiguity.
			fmt.Fprintf(os.Stderr, "promise update no longer takes an epoch argument.\n"+
				"  promise update                        follow the update channel (install + activate latest)\n"+
				"  promise update check [--json]         report whether an update is available\n"+
				"  promise update channel [stable|next]  show or set the update channel\n"+
				"  promise use <epoch>                   activate a specific epoch (downloads on demand)\n")
			os.Exit(1)
		}
	}
	doUpdate()
}

// doUpdate follows the persisted update channel: it resolves the channel's
// latest release, downloads and installs it, and the child `install`
// auto-activates the freshly installed epoch (decision #2 — otherwise a
// `promise use <epoch>` would freeze updates forever).
func doUpdate() {
	cfg := resolveSyncConfig()
	channel, err := module.UpdateChannel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Checking %s for Promise releases (channel: %s)...\n", cfg.describe(), channel)

	var release *ghRelease
	var epoch string
	switch channel {
	case module.ChannelNext:
		release, err = findNextRelease(cfg)
		epoch = "next"
	default: // stable
		release, epoch, err = findLatestStableRelease(cfg)
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

// runUpdateChannel implements `promise update channel [name]`. With no argument
// it prints the current channel; with an argument it persists the new channel
// and immediately follows it (set + install + activate, decision #4).
func runUpdateChannel(args []string) {
	if len(args) == 0 {
		ch, err := module.UpdateChannel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(ch)
		return
	}
	if len(args) > 1 {
		fmt.Fprintln(os.Stderr, "usage: promise update channel [stable|next]")
		os.Exit(1)
	}
	if err := module.WriteUpdateChannel(args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("update channel set to %s\n", args[0])
	doUpdate()
}

// updateCheckResult is the report produced by `promise update check`. Build ids
// carry the full sha256 in JSON; any human-facing rendering shortens them to 7
// chars (decision #3).
type updateCheckResult struct {
	Channel         string `json:"channel"`
	Active          string `json:"active"`
	Latest          string `json:"latest,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
	LocalBuild      string `json:"localBuild,omitempty"`
	RemoteBuild     string `json:"remoteBuild,omitempty"`
}

// runUpdateCheck implements `promise update check [--json]` — it reports whether
// an update is available on the current channel without mutating anything.
//
//	stable: compare the latest tagged epoch to the active epoch (numeric).
//	next:   compare the remote platform asset's sha256 (from the release's
//	        SHA256SUMS) to the locally recorded build-id. A rolling channel has
//	        no epoch identity, so the asset sha is the build identity.
func runUpdateCheck(args []string) {
	jsonOut := false
	for _, a := range args {
		switch a {
		case "--json", "-json":
			jsonOut = true
		default:
			fmt.Fprintln(os.Stderr, "usage: promise update check [--json]")
			os.Exit(1)
		}
	}

	cfg := resolveSyncConfig()
	channel, err := module.UpdateChannel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	active, _ := module.ActiveEpoch()

	res := updateCheckResult{Channel: channel, Active: active}

	switch channel {
	case module.ChannelNext:
		release, err := findNextRelease(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		_, shaURL, err := findAssets(release, platformAssetName())
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		// The next channel's build identity IS the platform asset's sha256, read
		// from the release's SHA256SUMS. Without it there is no way to tell new
		// from current — report that rather than silently claiming "up to date".
		if shaURL == "" {
			fmt.Fprintf(os.Stderr, "error: the %s release has no SHA256SUMS asset; cannot determine the next build identity\n", release.TagName)
			os.Exit(1)
		}
		remote, err := assetSHAFromSums(shaURL, platformAssetName())
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		local, _ := module.ReadEpochBuildID("next")
		res.Latest = "next"
		res.LocalBuild = local
		res.RemoteBuild = remote
		// An unrecorded local build (empty) counts as "update available".
		res.UpdateAvailable = remote != "" && remote != local
	default: // stable
		_, latest, err := findLatestStableRelease(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		res.Latest = latest
		res.UpdateAvailable = active == "" || module.CompareEpochs(latest, active) > 0
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		return
	}

	fmt.Printf("Update channel: %s\n", res.Channel)
	if res.Channel == module.ChannelNext {
		if res.UpdateAvailable {
			fmt.Printf("Update available: build %s -> %s\n", shortBuildID(res.LocalBuild), shortBuildID(res.RemoteBuild))
		} else {
			fmt.Printf("Up to date (build %s)\n", shortBuildID(res.LocalBuild))
		}
		return
	}
	fmt.Printf("Active epoch: %s\n", res.Active)
	if res.UpdateAvailable {
		fmt.Printf("Update available: epoch %s -> %s\n", res.Active, res.Latest)
	} else {
		fmt.Printf("Up to date (epoch %s)\n", res.Active)
	}
}

// shortBuildID renders a sha256 build identity as its first 7 hex chars
// (git-short-sha convention) for human-facing output. An empty id renders as a
// placeholder; the full hash is on-disk/comparison only.
func shortBuildID(sha string) string {
	if sha == "" {
		return "(none)"
	}
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// downloadAndInstall downloads the platform binary for a release, verifies its
// SHA256, decompresses it (assets are gzip-compressed — T0796), and runs the
// downloaded binary's `install` (which installs into epochs/<epoch>/,
// forward-updates the stub, and stages blobs). It then records the verified
// asset sha256 as the epoch's build-id (T0825). Shared by `promise update` and
// `promise use` download-on-demand.
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

	// Persist the build identity (the asset's sha256) so `update check` can
	// detect a new rolling-channel build (T0825). The .gz asset's sha is exactly
	// what SHA256SUMS lists and what `check` compares against — the same artifact
	// on both ends. Computed from the (verified) downloaded asset.
	if sum, sumErr := fileSHA256(gzPath); sumErr == nil {
		if wErr := module.WriteEpochBuildID(epoch, sum); wErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not record build id for %s: %v\n", epoch, wErr)
		}
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

	// Sort by epoch descending (numeric — lexicographic would rank "2026.10"
	// below "2026.9", the latent bug fixed in T0825).
	sort.Slice(stable, func(i, j int) bool {
		return module.CompareEpochs(
			strings.TrimPrefix(stable[i].TagName, "epoch-"),
			strings.TrimPrefix(stable[j].TagName, "epoch-"),
		) > 0
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
		return nil, fmt.Errorf("cannot reach %s (check your connection): %w", cfg.describe(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("epoch %s is not available in %s.\n"+
			"  Either the release tag does not exist, or the repository is private and you lack access.\n"+
			"  %s", epoch, cfg.describe(), tokenHint())
	}
	if resp.StatusCode != 200 {
		return nil, githubError(url, resp)
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
		return nil, fmt.Errorf("cannot reach %s (check your connection): %w", cfg.describe(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, githubError(url, resp)
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

// httpGetRaw performs a GET for binary asset downloads. It requests the raw bytes
// (Accept: application/octet-stream — required when downloading an asset from a
// private repository via its API URL) and authenticates when GITHUB_TOKEN is set.
// GitHub redirects asset downloads from api.github.com to a signed CDN URL on a
// different domain (objects.githubusercontent.com); Go's http.Client drops the
// Authorization header when a redirect target is not the same domain or a
// subdomain of the origin, so the token is never leaked to the CDN.
func httpGetRaw(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")
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
//
// When GITHUB_TOKEN is set, the API asset URL is preferred over the
// browser_download_url: downloading an asset from a *private* repository only
// works through the authenticated API URL (with Accept: application/octet-stream).
// For public repositories the browser URL is used (no token required).
func findAssets(release *ghRelease, assetName string) (assetURL, shaURL string, err error) {
	useAPI := os.Getenv("GITHUB_TOKEN") != ""
	pick := func(a ghAsset) string {
		if useAPI && a.URL != "" {
			return a.URL
		}
		return a.BrowserDownloadURL
	}
	for _, a := range release.Assets {
		switch a.Name {
		case assetName:
			assetURL = pick(a)
		case "SHA256SUMS":
			shaURL = pick(a)
		}
	}
	if assetURL == "" {
		return "", "", fmt.Errorf("no binary available for %s-%s in release %s", runtime.GOOS, runtime.GOARCH, release.TagName)
	}
	return assetURL, shaURL, nil
}

// downloadFile downloads a URL to a local file, printing progress to stderr.
func downloadFile(url, destPath string) error {
	resp, err := httpGetRaw(url)
	if err != nil {
		return fmt.Errorf("cannot download release asset (check your connection): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return githubError(url, resp)
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

// assetSHAFromSums downloads a SHA256SUMS file and returns the hex sha256 listed
// for assetName. Shared by verifySHA256 (the download checksum check) and
// `update check` (the rolling next channel's remote build identity, T0825).
func assetSHAFromSums(shaURL, assetName string) (string, error) {
	resp, err := httpGetRaw(shaURL)
	if err != nil {
		return "", fmt.Errorf("cannot download SHA256SUMS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", githubError(shaURL, resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading SHA256SUMS: %w", err)
	}

	// Parse SHA256SUMS: each line is "<hash>  <filename>" or "<hash> <filename>".
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 && parts[1] == assetName {
			return parts[0], nil
		}
	}
	return "", fmt.Errorf("no checksum found for %s in SHA256SUMS", assetName)
}

// fileSHA256 returns the hex-encoded sha256 of a file's contents.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifySHA256 downloads SHA256SUMS and verifies the binary's checksum.
func verifySHA256(shaURL, binaryPath, binaryName string) error {
	expectedHash, err := assetSHAFromSums(shaURL, binaryName)
	if err != nil {
		return err
	}

	actualHash, err := fileSHA256(binaryPath)
	if err != nil {
		return fmt.Errorf("computing checksum: %w", err)
	}

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
