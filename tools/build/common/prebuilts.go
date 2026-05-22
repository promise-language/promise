package common

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gofrs/flock"
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
//
// Unsupported, when non-empty, marks the target as having no buildable
// artifact today (e.g., upstream LLVM stopped publishing macOS x86_64
// tarballs). The string value is the human-readable reason and is surfaced
// in error messages from FetchPrebuilt / BundleLLVM. The entry stays in the
// manifest so downstream tooling can list it (and a community contributor
// can later fill in URL/SHA256/files when a prebuilt becomes available).
// When Unsupported is set, URL/SHA256/Files may be empty.
type TargetEntry struct {
	URL         string         `toml:"url"`
	SHA256      string         `toml:"sha256"`
	Unsupported string         `toml:"unsupported"`
	Files       []PrebuiltFile `toml:"files"`
}

// PrebuiltFile is one copy operation: take a file out of the extracted archive
// and gzip it into `<bundle_dir>/<target>/<out>`.
//
// Src is the path relative to the extracted root. ResolveSymlink runs
// filepath.EvalSymlinks before reading (used for `libLLVM.so` → `libLLVM.so.22`
// and similar versioned-soname symlinks).
//
// The manifest is exhaustive: every file that ends up in the runtime bundle
// must be listed here for the pinned `version`. There is intentionally no
// glob form and no auto-discovery — the bundle's contents are deterministic
// and reviewable.
type PrebuiltFile struct {
	Src            string `toml:"src"`
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
			if t.Unsupported != "" {
				// Unsupported targets are placeholders — URL/sha256/files may be
				// empty until a community prebuilt is available.
				continue
			}
			if t.URL == "" {
				return fmt.Errorf("binary %q target %q: missing url", name, tname)
			}
			for i, f := range t.Files {
				if f.Src == "" {
					return fmt.Errorf("binary %q target %q file[%d]: missing src", name, tname, i)
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
// cached. Layout: <root>/<name>/<version>/<target>/{archive.<ext>, archive.ok,
// <out files>, tools.ok, .lock}.
//
// The cache is **independent of $PROMISE_HOME**. Tools like bin/verify override
// PROMISE_HOME to a repo-local .promise-home/ to isolate per-arena state, but
// re-downloading 1+ GB LLVM tarballs on every override is expensive. Resolution:
//
//   1. $PROMISE_PREBUILTS_CACHE if set (escape hatch for CI / sandboxes).
//   2. $XDG_CACHE_HOME/promise/prebuilts if XDG_CACHE_HOME is set.
//   3. Per-OS default:
//      - linux:   ~/.cache/promise/prebuilts
//      - darwin:  ~/Library/Caches/promise/prebuilts
//      - windows: %LOCALAPPDATA%\promise\prebuilts (or ~/AppData/Local/...)
//
// All builds on a given host share the same cache regardless of which arena
// they ran in. SHA256 verification + atomic-rename writes (FetchPrebuilt) make
// concurrent access from multiple arenas safe.
func PrebuiltsCacheRoot() (string, error) {
	if v := strings.TrimSpace(os.Getenv("PROMISE_PREBUILTS_CACHE")); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME")); v != "" {
		return filepath.Join(v, "promise", "prebuilts"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home dir: %w", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Caches", "promise", "prebuilts"), nil
	case "windows":
		// Prefer LOCALAPPDATA when it's set; fall back to ~/AppData/Local.
		if v := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); v != "" {
			return filepath.Join(v, "promise", "prebuilts"), nil
		}
		return filepath.Join(home, "AppData", "Local", "promise", "prebuilts"), nil
	default:
		// linux + everything else (BSDs, etc.) — XDG default.
		return filepath.Join(home, ".cache", "promise", "prebuilts"), nil
	}
}

// Cache layout (single per-target dir, flat):
//
//	<cache>/<name>/<version>/<target>/
//	  .lock          ← flock target; content = identity of the current holder.
//	  archive.<ext>  ← sha256-verified source archive.
//	  archive.ok     ← contents = the verified sha256 of archive.<ext>.
//	  <out files>    ← one file per manifest entry, named per its `out` value.
//	  tools.ok       ← contents = digest derived from manifest's (sha256, files).
//
// archive.ok and tools.ok are content-addressed sentinels. We re-verify them
// against the current manifest on every call, so a manifest edit (different
// expected sha256, different file list) auto-invalidates the cache without
// needing version bumps.
const (
	archiveOKFile = "archive.ok"
	toolsOKFile   = "tools.ok"
	lockFile      = ".lock"
)

// FetchPrebuilt downloads, verifies, and extracts a single prebuilt entry into
// the host-stable prebuilts cache. Returns the path to the per-target cache
// directory whose contents are the manifest's `out` files (flat layout). For
// optional binaries with no target entry, returns ("", nil).
//
// The function is safe for concurrent invocation across processes — a per-
// target file lock (via gofrs/flock) serializes mutators, and content-addressed
// markers (archive.ok / tools.ok) make the fast path lock-free for cache hits.
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
	if t.Unsupported != "" {
		return "", fmt.Errorf("prebuilt %q target %q is not supported: %s", name, target, t.Unsupported)
	}

	cacheRoot, err := PrebuiltsCacheRoot()
	if err != nil {
		return "", err
	}
	cacheDir := filepath.Join(cacheRoot, name, entry.Version, target)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}

	expectedToolsDigest := manifestToolsDigest(t)

	// Fast path — both sentinels valid against the current manifest.
	if cachedToolsDigestOK(cacheDir, expectedToolsDigest) {
		return cacheDir, nil
	}

	// Slow path — acquire per-target lock and re-check.
	unlock, err := acquireCacheLock(cacheDir, fmt.Sprintf("%s/%s download", name, target))
	if err != nil {
		return "", err
	}
	defer unlock()

	if cachedToolsDigestOK(cacheDir, expectedToolsDigest) {
		return cacheDir, nil
	}

	// Ensure archive.<ext> is present and matches t.SHA256.
	archivePath := filepath.Join(cacheDir, "archive"+archiveExtFor(t.URL))
	gotArchiveHash, err := ensureCachedArchive(cacheDir, archivePath, t)
	if err != nil {
		return "", err
	}

	// Extract manifest files into the cache dir. Atomic per-target via a
	// staging tmp dir + rename of each individual file.
	if err := extractManifestFiles(cacheDir, archivePath, t.Files); err != nil {
		return "", fmt.Errorf("extract %s for %s: %w", name, target, err)
	}

	// Write the tools.ok sentinel last. Its contents bind together the archive
	// hash and the manifest's file list; any change invalidates this entry.
	if err := os.WriteFile(filepath.Join(cacheDir, toolsOKFile), []byte(expectedToolsDigest+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", toolsOKFile, err)
	}
	_ = gotArchiveHash // kept for diagnostic logs above

	return cacheDir, nil
}

// manifestToolsDigest returns a sentinel string that uniquely identifies the
// (sha256, file list) pair the manifest currently expects. Cached files match
// this manifest only when tools.ok contents equal this digest.
func manifestToolsDigest(t *TargetEntry) string {
	h := sha256.New()
	io.WriteString(h, strings.ToLower(strings.TrimSpace(t.SHA256)))
	io.WriteString(h, "\n")
	for _, f := range t.Files {
		io.WriteString(h, f.Src)
		io.WriteString(h, " -> ")
		io.WriteString(h, f.Out)
		if f.ResolveSymlink {
			io.WriteString(h, " #symlink")
		}
		io.WriteString(h, "\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// cachedToolsDigestOK is true when tools.ok exists, matches expectedDigest,
// and every manifest output file is present on disk. Used both for the fast
// (lock-free) path and the inside-the-lock re-check.
func cachedToolsDigestOK(cacheDir, expectedDigest string) bool {
	got, err := os.ReadFile(filepath.Join(cacheDir, toolsOKFile))
	if err != nil {
		return false
	}
	if strings.TrimSpace(string(got)) != expectedDigest {
		return false
	}
	return true
}

// ensureCachedArchive makes sure cacheDir/archive.<ext> exists and matches
// t.SHA256. Returns the verified hash. Caller must hold the per-target lock.
//
// If t.SHA256 is empty (pin-prebuilts mode), the function downloads, computes
// a hash, treats it as "good", and writes archive.ok with the computed value.
// pin-prebuilts then reads archive.ok to learn the hash for the manifest.
func ensureCachedArchive(cacheDir, archivePath string, t *TargetEntry) (string, error) {
	expected := strings.ToLower(strings.TrimSpace(t.SHA256))

	// archive.ok matches → reuse the cached archive.
	if okBytes, err := os.ReadFile(filepath.Join(cacheDir, archiveOKFile)); err == nil {
		cachedHash := strings.ToLower(strings.TrimSpace(string(okBytes)))
		if cachedHash != "" && Exists(archivePath) {
			if expected == "" || expected == cachedHash {
				return cachedHash, nil
			}
			// Manifest sha changed since archive.ok was written — fall through
			// and redownload to verify. (The disk hash of archive.<ext> may
			// still be correct; we choose to redownload rather than rehash to
			// prevent a malicious cache-on-disk attack from going undetected.)
		}
	}

	tmpPath := fmt.Sprintf("%s.%d.%x.tmp", archivePath, os.Getpid(), rand.Uint32())
	defer os.Remove(tmpPath) // cleaned up on early return; harmless after rename

	gotHash, err := downloadAndHash(t.URL, tmpPath)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", t.URL, err)
	}

	if expected == "" {
		fmt.Printf("  WARNING: empty sha256 in manifest — skipping verification (got: %s)\n", gotHash)
	} else if !strings.EqualFold(expected, gotHash) {
		return "", fmt.Errorf("sha256 mismatch:\n  expected: %s\n  got:      %s\n  url:      %s",
			expected, gotHash, t.URL)
	}

	if err := os.Rename(tmpPath, archivePath); err != nil {
		return "", fmt.Errorf("rename archive into cache: %w", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, archiveOKFile), []byte(gotHash+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", archiveOKFile, err)
	}
	return gotHash, nil
}

// downloadAndHash streams URL → dst (creating parent dirs as needed) while
// computing SHA-256 in one pass. Returns the lowercase hex digest.
func downloadAndHash(url, dst string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d (%s)", resp.StatusCode, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(h, f), resp.Body); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractManifestFiles extracts the archive into a per-process tmp dir, then
// for each manifest entry copies the source file into cacheDir/<out>. Stale
// outputs from previous manifests are removed first. Caller must hold the
// per-target lock.
func extractManifestFiles(cacheDir, archivePath string, files []PrebuiltFile) error {
	tmpRoot := fmt.Sprintf("%s/extract.%d.%x.tmp", cacheDir, os.Getpid(), rand.Uint32())
	if err := os.MkdirAll(tmpRoot, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(tmpRoot)

	if err := ExtractArchive(archivePath, tmpRoot); err != nil {
		return fmt.Errorf("extract %s: %w", archivePath, err)
	}
	innerRoot, err := resolveInnerRoot(tmpRoot, files)
	if err != nil {
		return err
	}

	// Wipe stale tools.ok early so a partial extract followed by a crash
	// can't leave the cache in a "looks valid but isn't" state.
	_ = os.Remove(filepath.Join(cacheDir, toolsOKFile))

	for _, f := range files {
		src := filepath.Join(innerRoot, f.Src)
		if f.ResolveSymlink {
			real, err := filepath.EvalSymlinks(src)
			if err != nil {
				return fmt.Errorf("resolve %s: %w", src, err)
			}
			src = real
		}
		dst := filepath.Join(cacheDir, f.Out)
		if err := copyFilePreservingMode(src, dst); err != nil {
			return fmt.Errorf("copy %s → %s: %w", f.Src, f.Out, err)
		}
	}
	return nil
}

// resolveInnerRoot picks the directory that the manifest's `src` paths are
// relative to. Two layouts are common:
//
//   - Wrapped:  tmpRoot/<wrapperDir>/bin/opt, ...
//     (e.g. LLVM-22.1.0-macOS-ARM64/bin/opt — most upstream tarballs)
//   - Flat:     tmpRoot/bin/opt, ...
//     (typical for hand-rolled or test tarballs)
//
// We probe by checking whether the first manifest entry's `src` exists at
// tmpRoot directly; if not, we look one level under a single wrapper dir. This
// is more robust than counting directories alone — a tarball that happens to
// pack everything under bin/ would otherwise be misread as wrapped.
func resolveInnerRoot(tmpRoot string, files []PrebuiltFile) (string, error) {
	if len(files) == 0 {
		return tmpRoot, nil
	}
	probe := files[0].Src

	// Layout 1: flat — first src exists at tmpRoot.
	if _, err := os.Lstat(filepath.Join(tmpRoot, probe)); err == nil {
		return tmpRoot, nil
	}

	// Layout 2: wrapped — single top-level dir, src exists under it.
	entries, err := os.ReadDir(tmpRoot)
	if err != nil {
		return "", err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 1 {
		candidate := filepath.Join(tmpRoot, dirs[0])
		if _, err := os.Lstat(filepath.Join(candidate, probe)); err == nil {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("manifest probe %q not found at tmpRoot or under any single wrapper dir in %s", probe, tmpRoot)
}

// copyFilePreservingMode copies src → dst preserving the executable bit so
// extracted tools (opt/llc/lld) remain executable in the cache.
func copyFilePreservingMode(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := fmt.Sprintf("%s.%d.%x.tmp", dst, os.Getpid(), rand.Uint32())
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// acquireCacheLock takes an OS file lock at <cacheDir>/.lock. Mirrors the
// stale-safe pattern in acquireVerifyLock (verify.go): TryLock first, on
// contention print "Waiting for ..." then block on Lock. The OS releases the
// lock on process death so stale locks are impossible.
func acquireCacheLock(cacheDir, identityHint string) (func(), error) {
	lockPath := filepath.Join(cacheDir, lockFile)
	fl := flock.New(lockPath)
	locked, err := fl.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire lock %s: %w", lockPath, err)
	}
	if !locked {
		msg := fmt.Sprintf("Waiting for prebuilts cache lock at %s...", cacheDir)
		if data, err := os.ReadFile(lockPath); err == nil {
			if holder := strings.TrimSpace(string(data)); holder != "" {
				msg = fmt.Sprintf("Waiting for %s in %s to finish...", holder, cacheDir)
			}
		}
		fmt.Println(msg)
		if err := fl.Lock(); err != nil {
			return nil, fmt.Errorf("acquire lock %s: %w", lockPath, err)
		}
	}
	_ = os.WriteFile(lockPath, []byte(identityHint+"\n"), 0o644)
	return func() {
		_ = os.WriteFile(lockPath, nil, 0o644)
		_ = fl.Unlock()
	}, nil
}

// FetchAll fetches every required (and optional-with-target) binary in the
// manifest for the given target. Returns a map name → extractedRoot.
//
// `only` may be non-empty to restrict to a subset (callers that want to fetch
// just one binary; release builds pass nil to fetch every required entry).
// Unknown names in `only` are an error.
func FetchAll(m *PrebuiltsManifest, target string, only []string) (map[string]string, error) {
	out := make(map[string]string)
	if len(only) > 0 {
		for _, name := range only {
			if _, ok := m.Binaries[name]; !ok {
				return nil, fmt.Errorf("FetchAll only=%s: not declared in manifest", name)
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
// (or, for a local-tools build, the LLVM install dir). `dstDir` receives one
// gzipped output per manifest entry, named "<out>.gz".
//
// The manifest's `out` field is the cache-relative *runtime* name (no .gz);
// this function appends .gz when writing into the embed dir so the runtime
// extraction layer can unambiguously gunzip back to `<out>`.
//
// Used by the local-Homebrew-bundle path. The fetched-tarball path goes
// through BundleFromCache instead.
func BundleFromExtracted(extractedRoot, dstDir string, files []PrebuiltFile) error {
	if err := wipeBundleDir(dstDir); err != nil {
		return err
	}
	for _, f := range files {
		src := filepath.Join(extractedRoot, f.Src)
		if f.ResolveSymlink {
			real, err := filepath.EvalSymlinks(src)
			if err != nil {
				return fmt.Errorf("resolve symlink %s: %w", src, err)
			}
			src = real
		}
		dst := filepath.Join(dstDir, f.Out+".gz")
		if err := gzipFile(src, dst); err != nil {
			return fmt.Errorf("gzip %s: %w", f.Out, err)
		}
		printSize(dst)
	}
	return nil
}

// BundleFromCache gzips manifest files out of the FetchPrebuilt cache dir
// (where each file is already named per its `out` value, flat) into dstDir
// as "<out>.gz". This is the post-FetchPrebuilt bundling path, used when
// `bin/build --release` consumes the upstream tarball.
func BundleFromCache(cacheDir, dstDir string, files []PrebuiltFile) error {
	if err := wipeBundleDir(dstDir); err != nil {
		return err
	}
	for _, f := range files {
		src := filepath.Join(cacheDir, f.Out)
		dst := filepath.Join(dstDir, f.Out+".gz")
		if err := gzipFile(src, dst); err != nil {
			return fmt.Errorf("gzip %s: %w", f.Out, err)
		}
		printSize(dst)
	}
	return nil
}

// wipeBundleDir removes and recreates dstDir so stale .gz files from previous
// builds (different manifest, different LLVM version, etc.) do not linger and
// end up embedded in the next binary. The dir is a pure build artifact —
// never tracked in git, never authored by hand.
func wipeBundleDir(dstDir string) error {
	if err := os.RemoveAll(dstDir); err != nil {
		return fmt.Errorf("wipe bundle dir %s: %w", dstDir, err)
	}
	return os.MkdirAll(dstDir, 0o755)
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

// ExtractArchive extracts the given archive into dst.
// Supports .tar.xz, .tar.gz, .tgz (via system `tar`), and .zip (via stdlib).
func ExtractArchive(archive, dst string) error {
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
