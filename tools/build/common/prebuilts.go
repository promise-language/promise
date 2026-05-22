package common

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// PrebuiltsManifestSchema is the schema version this loader understands.
// Bumped when the on-disk format changes incompatibly.
const PrebuiltsManifestSchema = 1

// PrebuiltsManifest declares the prebuilt binaries embedded by `bin/build --release`.
// Loaded from `tools/build/prebuilts.toml`.
type PrebuiltsManifest struct {
	Schema   int                       `toml:"schema"`
	Binaries map[string]*PrebuiltEntry `toml:"binaries"`
}

// PrebuiltEntry is one binary (e.g., "llvm", "wasmtime") in the manifest.
type PrebuiltEntry struct {
	Version   string                  `toml:"version"`
	BundleDir string                  `toml:"bundle_dir"`
	Optional  bool                    `toml:"optional"`
	Targets   map[string]*TargetEntry `toml:"targets"`
}

// TargetEntry is a per-target download spec (URL + SHA256 + file ops).
type TargetEntry struct {
	URL    string         `toml:"url"`
	SHA256 string         `toml:"sha256"`
	Files  []PrebuiltFile `toml:"files"`
}

// PrebuiltFile is one copy operation: take a file (or glob) out of the
// extracted archive and gzip it into `<bundle_dir>/<target>/<out>`.
//
// Exactly one of Src or Glob must be set. ResolveSymlink runs
// filepath.EvalSymlinks before reading (used for libLLVM.so → libLLVM.so.22).
// Out may contain `{basename}` which is substituted with the matched
// filename's basename (used with Glob).
type PrebuiltFile struct {
	Src            string `toml:"src"`
	Glob           string `toml:"glob"`
	Out            string `toml:"out"`
	ResolveSymlink bool   `toml:"resolve_symlink"`
}

// LoadPrebuiltsManifest parses tools/build/prebuilts.toml under root.
func LoadPrebuiltsManifest(root string) (*PrebuiltsManifest, error) {
	path := filepath.Join(root, "tools", "build", "prebuilts.toml")
	return loadPrebuiltsManifestFile(path)
}

func loadPrebuiltsManifestFile(path string) (*PrebuiltsManifest, error) {
	var m PrebuiltsManifest
	if _, err := toml.DecodeFile(path, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return &m, nil
}

func (m *PrebuiltsManifest) validate() error {
	if m.Schema != PrebuiltsManifestSchema {
		return fmt.Errorf("unsupported schema %d (want %d)", m.Schema, PrebuiltsManifestSchema)
	}
	if len(m.Binaries) == 0 {
		return errors.New("no [binaries.*] entries declared")
	}
	for name, entry := range m.Binaries {
		if entry == nil {
			return fmt.Errorf("binary %q: nil entry", name)
		}
		if entry.Version == "" {
			return fmt.Errorf("binary %q: missing version", name)
		}
		if entry.BundleDir == "" {
			return fmt.Errorf("binary %q: missing bundle_dir", name)
		}
		for tname, t := range entry.Targets {
			if t == nil {
				return fmt.Errorf("binary %q target %q: nil entry", name, tname)
			}
			if t.URL == "" {
				return fmt.Errorf("binary %q target %q: missing url", name, tname)
			}
			for i, f := range t.Files {
				if (f.Src == "") == (f.Glob == "") {
					return fmt.Errorf("binary %q target %q file[%d]: exactly one of src or glob must be set", name, tname, i)
				}
				if f.Out == "" {
					return fmt.Errorf("binary %q target %q file[%d]: missing out", name, tname, i)
				}
			}
		}
	}
	return nil
}

// CurrentBuildTarget returns the manifest target identifier for the running OS+arch
// (e.g., "linux-amd64", "darwin-arm64", "windows-amd64"). Matches the directory
// names already used under compiler/cmd/promise/resources/llvm/.
func CurrentBuildTarget() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

// PrebuiltsCacheRoot returns the directory under which fetched prebuilts are
// cached. Layout: <root>/<name>/<version>/<target>/{archive.<ext>,extracted/,sha256.ok}.
//
// Resolution: $PROMISE_HOME/cache/prebuilts → ~/.promise/cache/prebuilts.
// Never the repo-local .promise-home/ — re-downloading is expensive, so the
// shared cache is preferred (and is wiped only by `bin/clean --shared`).
func PrebuiltsCacheRoot() (string, error) {
	if h := os.Getenv("PROMISE_HOME"); h != "" {
		// Skip the local cache override: prebuilts are huge and shared.
		if !isLocalPromiseHome(h) {
			return filepath.Join(h, "cache", "prebuilts"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home dir: %w", err)
	}
	return filepath.Join(home, ".promise", "cache", "prebuilts"), nil
}

// isLocalPromiseHome returns true if PROMISE_HOME points at the repo-local
// .promise-home/ override (set by SetupLocalCache). Prebuilts always use the
// shared cache regardless of the local override.
func isLocalPromiseHome(h string) bool {
	return strings.HasSuffix(filepath.Clean(h), string(filepath.Separator)+".promise-home")
}

// FetchPrebuilt downloads, verifies, and extracts a single prebuilt entry.
// Returns the path to the extracted root (the parent of the archive's
// top-level directory). For optional binaries with no target entry,
// returns ("", nil).
func FetchPrebuilt(m *PrebuiltsManifest, name, target string) (string, error) {
	entry := m.Binaries[name]
	if entry == nil {
		return "", fmt.Errorf("prebuilt %q: not declared in manifest", name)
	}
	t := entry.Targets[target]
	if t == nil {
		if entry.Optional {
			return "", nil
		}
		return "", fmt.Errorf("prebuilt %q: no target entry for %q", name, target)
	}

	cacheRoot, err := PrebuiltsCacheRoot()
	if err != nil {
		return "", err
	}
	cacheDir := filepath.Join(cacheRoot, name, entry.Version, target)
	extractedDir := filepath.Join(cacheDir, "extracted")
	okMarker := filepath.Join(cacheDir, "sha256.ok")

	if Exists(okMarker) && Exists(extractedDir) {
		return extractedDir, nil
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}

	archiveExt := archiveExtFor(t.URL)
	archivePath := filepath.Join(cacheDir, "archive"+archiveExt)

	fmt.Printf("Fetching %s %s for %s...\n  %s\n", name, entry.Version, target, t.URL)
	if err := downloadFile(t.URL, archivePath); err != nil {
		return "", fmt.Errorf("download %s: %w", name, err)
	}

	gotHash, err := sha256File(archivePath)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", archivePath, err)
	}
	if t.SHA256 == "" {
		fmt.Printf("  WARNING: %s/%s has empty sha256 in manifest — skipping verification (got: %s)\n",
			name, target, gotHash)
	} else if !strings.EqualFold(strings.TrimSpace(t.SHA256), gotHash) {
		// Do not auto-retry; the cache holds the bad bytes for diagnosis.
		return "", fmt.Errorf("sha256 mismatch for %s/%s:\n  expected: %s\n  got:      %s\n  archive:  %s",
			name, target, t.SHA256, gotHash, archivePath)
	} else {
		fmt.Printf("  sha256 OK (%s)\n", gotHash[:16]+"…")
	}

	// Wipe + re-extract on each fetch to keep the tree consistent with the
	// archive that was just verified.
	if err := os.RemoveAll(extractedDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(extractedDir, 0o755); err != nil {
		return "", err
	}
	if err := extractArchive(archivePath, extractedDir); err != nil {
		return "", fmt.Errorf("extract %s: %w", archivePath, err)
	}

	if err := os.WriteFile(okMarker, []byte(gotHash+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", okMarker, err)
	}

	return extractedDir, nil
}

// FetchAll fetches every required (and optional-with-target) binary in the
// manifest for the given target. Returns a map name → extractedRoot.
//
// `only` may be non-empty to restrict to a subset (e.g., from `--fetch=llvm`).
// Unknown names in `only` are an error.
func FetchAll(m *PrebuiltsManifest, target string, only []string) (map[string]string, error) {
	out := make(map[string]string)
	if len(only) > 0 {
		for _, name := range only {
			if _, ok := m.Binaries[name]; !ok {
				return nil, fmt.Errorf("--fetch=%s: not declared in manifest", name)
			}
		}
	}
	for name, entry := range m.Binaries {
		if len(only) > 0 && !slices.Contains(only, name) {
			continue
		}
		root, err := FetchPrebuilt(m, name, target)
		if err != nil {
			return nil, err
		}
		if root == "" && entry.Optional {
			continue
		}
		out[name] = root
	}
	return out, nil
}

// BundleFromExtracted runs the manifest file ops against an extracted tree.
// `extractedRoot` is the directory containing the archive's top-level layout
// (or, for a local-tools build, the LLVM install dir). `dstDir` receives the
// gzipped outputs.
//
// The same engine drives both --fetch and the local-LLVM path: both are just
// "pick files from a tree, gzip them into the bundle dir."
func BundleFromExtracted(extractedRoot, dstDir string, files []PrebuiltFile) error {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		if err := runFileOp(extractedRoot, dstDir, f); err != nil {
			return err
		}
	}
	return nil
}

func runFileOp(extractedRoot, dstDir string, f PrebuiltFile) error {
	if f.Src != "" {
		src := filepath.Join(extractedRoot, f.Src)
		if f.ResolveSymlink {
			real, err := filepath.EvalSymlinks(src)
			if err != nil {
				return fmt.Errorf("resolve symlink %s: %w", src, err)
			}
			src = real
		}
		out := strings.ReplaceAll(f.Out, "{basename}", filepath.Base(src))
		dst := filepath.Join(dstDir, out)
		if err := gzipFile(src, dst); err != nil {
			return fmt.Errorf("gzip %s: %w", out, err)
		}
		printSize(dst)
		return nil
	}

	// Glob form. Match relative to extractedRoot.
	pattern := filepath.Join(extractedRoot, f.Glob)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob %s: %w", f.Glob, err)
	}
	for _, src := range matches {
		realSrc := src
		if f.ResolveSymlink {
			r, err := filepath.EvalSymlinks(src)
			if err != nil {
				return fmt.Errorf("resolve symlink %s: %w", src, err)
			}
			realSrc = r
		}
		out := strings.ReplaceAll(f.Out, "{basename}", filepath.Base(src))
		dst := filepath.Join(dstDir, out)
		if err := gzipFile(realSrc, dst); err != nil {
			return fmt.Errorf("gzip %s: %w", out, err)
		}
		printSize(dst)
	}
	return nil
}

// archiveExtFor returns the archive extension for a URL (e.g., ".tar.xz").
// Falls back to ".tar.xz" since that's the dominant LLVM/wasmtime format.
func archiveExtFor(url string) string {
	lower := strings.ToLower(url)
	for _, ext := range []string{".tar.xz", ".tar.gz", ".tgz", ".zip"} {
		if strings.HasSuffix(lower, ext) {
			return ext
		}
	}
	return ".tar.xz"
}

// downloadFile fetches url to dst, replacing any existing file.
// Logs progress every ~5 MB.
func downloadFile(url, dst string) error {
	tmp := dst + ".part"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "promise-build/1 (+https://github.com/p5e-ia/promise-lang)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d %s for %s", resp.StatusCode, resp.Status, url)
	}

	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer out.Close()

	pr := &progressReader{r: resp.Body, total: resp.ContentLength, label: filepath.Base(dst)}
	if _, err := io.Copy(out, pr); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	pr.flush()
	return os.Rename(tmp, dst)
}

// progressReader logs a one-line progress update every ~5 MB of bytes read.
// Quiet for very small downloads (no Content-Length, or under 1 MB).
type progressReader struct {
	r          io.Reader
	total      int64
	label      string
	read       int64
	lastLogged int64
}

const progressStep = 5 * 1024 * 1024 // 5 MB

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	if p.total > progressStep && p.read-p.lastLogged >= progressStep {
		p.log()
		p.lastLogged = p.read
	}
	return n, err
}

func (p *progressReader) flush() {
	if p.total > progressStep && p.read != p.lastLogged {
		p.log()
	}
}

func (p *progressReader) log() {
	mb := float64(p.read) / (1024 * 1024)
	if p.total > 0 {
		pct := 100 * float64(p.read) / float64(p.total)
		totalMB := float64(p.total) / (1024 * 1024)
		fmt.Printf("  %s: %.1f / %.1f MB (%.0f%%)\n", p.label, mb, totalMB, pct)
	} else {
		fmt.Printf("  %s: %.1f MB\n", p.label, mb)
	}
}

func sha256File(path string) (string, error) {
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

// extractArchive extracts the given archive into dst.
// Supports .tar.xz, .tar.gz, .tgz (via system `tar`), and .zip (via stdlib).
func extractArchive(archive, dst string) error {
	lower := strings.ToLower(archive)
	switch {
	case strings.HasSuffix(lower, ".tar.xz"),
		strings.HasSuffix(lower, ".tar.gz"),
		strings.HasSuffix(lower, ".tgz"):
		// `tar` autodetects compression on every modern platform's tar.
		return RunIn(dst, "tar", "-xf", archive)
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(archive, dst)
	default:
		return fmt.Errorf("unsupported archive format: %s", archive)
	}
}

// extractZip extracts a .zip archive into dst using the stdlib zip reader.
// Used for Windows assets if/when any are added to the manifest.
func extractZip(archive, dst string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		name := filepath.Clean(f.Name)
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("zip: refusing to extract escaping path %q", f.Name)
		}
		path := filepath.Join(dst, name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(path, f.Mode()); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			rc.Close()
			out.Close()
			return err
		}
		rc.Close()
		if err := out.Close(); err != nil {
			return err
		}
	}
	return nil
}
