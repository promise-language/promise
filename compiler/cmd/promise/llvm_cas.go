package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/promise-language/promise/compiler/internal/blobstore"
	"github.com/promise-language/promise/compiler/internal/module"
)

// llvmEntryPrefix is the manifest logical-name prefix for LLVM tool blobs
// ("llvm-opt", "llvm-llc", "llvm-lld", "llvm-libLLVM.dylib", ...).
const llvmEntryPrefix = "llvm-"

// llvmLibEnvKeyRuntime returns the dynamic-library search env var for the host:
// DYLD_LIBRARY_PATH on macOS, LD_LIBRARY_PATH on Linux, none on Windows (LLVM
// tools there are statically linked).
func llvmLibEnvKeyRuntime() string {
	switch runtime.GOOS {
	case "darwin":
		return "DYLD_LIBRARY_PATH"
	case "windows":
		return ""
	default:
		return "LD_LIBRARY_PATH"
	}
}

var (
	llvmViewMu  sync.Mutex
	llvmViewDir string // set once the per-target view dir is built
)

// prefetchNoPrompt suppresses resolveLLVMView's interactive "Download now?"
// confirmation (the progress bar still shows). Set by `promise install`, which
// pre-fetches the toolchain as part of setup — the user already opted in by
// running the installer, so re-asking would be redundant.
var prefetchNoPrompt bool

// prefetchHostToolchain downloads + materializes the host LLVM toolchain into
// the CAS at install time, so the first compile is instant instead of blocking
// for minutes. Best-effort: a network failure is non-fatal (the toolchain still
// fetches lazily on first use). Returns the view dir, or "" when the manifest
// carries no LLVM entries or the fetch could not complete.
func prefetchHostToolchain() string {
	prefetchNoPrompt = true
	defer func() { prefetchNoPrompt = false }()
	viewDir, err := resolveLLVMView(true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: could not pre-fetch the LLVM toolchain now (%v).\n      It will download automatically on your first build.\n", err)
		return ""
	}
	return viewDir
}

// resolveLLVMView materializes the host LLVM tool set from the content-addressed
// store into a per-target "view" directory (cache/llvm-view/<target>/) where the
// tools, the libLLVM dylib, and the lld-mode aliases live side by side — the
// "tools and dylib in one searchable dir" invariant the linker relies on. The
// CAS itself stays flat (blobs/sha256/<hash>); the view is the searchable layout
// pointing at it.
//
// On macOS the view holds patched + ad-hoc-signed working copies (the CAS keeps
// the raw upstream bytes, so its content hash is deterministic and reproducible
// at release time without codesign; §5.1 + §10 open contract). On Linux/Windows
// the view holds symlinks/copies of the raw blobs.
//
// allowFetch=false succeeds only when every LLVM blob is already a CAS hit (used
// for the "prefer a pre-staged toolchain over a system one" probe); allowFetch=
// true downloads missing blobs from the manifest sources (last-resort probe).
// Returns ("", nil) when the manifest carries no LLVM entries (thin placeholder).
func resolveLLVMView(allowFetch bool) (string, error) {
	llvmViewMu.Lock()
	defer llvmViewMu.Unlock()
	if llvmViewDir != "" {
		return llvmViewDir, nil
	}

	m, err := loadEmbeddedManifest()
	if err != nil || m == nil {
		return "", nil // no usable manifest → caller falls through to local probes
	}
	var entries []*blobstore.ManifestEntry
	for i := range m.Entries {
		if strings.HasPrefix(m.Entries[i].Name, llvmEntryPrefix) {
			entries = append(entries, &m.Entries[i])
		}
	}
	if len(entries) == 0 {
		return "", nil // thin placeholder — no LLVM blobs to materialize
	}

	// Wipe derived caches (incl. old view dirs) when the compiler binary changed.
	// findLLVMTool no longer reaches ensureCacheValid on the CAS path, so without
	// this a version bump would leave previous-epoch view dirs accumulating.
	// (Content-keying above already guarantees correctness; this is the reclaim.)
	ensureCacheValid()

	home, err := module.PromiseHome()
	if err != nil {
		return "", err
	}
	// Key the view dir by the blob-hash set, not just the target: a compiler/LLVM
	// version bump changes the entry sha256s, yielding a fresh view dir so stale
	// tools from a previous epoch are never served from a name-only match. The CAS
	// itself is content-addressed and never stale; the view is a derived working
	// copy with content-based invalidation. (Old view dirs are wiped by
	// CleanLLVMCache on a compiler-stamp change and otherwise reclaimed by GC.)
	viewDir := filepath.Join(home, "cache", "llvm-view", runtime.GOOS+"-"+runtime.GOARCH+"-"+blobSetKey(entries))

	store, err := blobstore.NewStore()
	if err != nil {
		return "", err
	}

	// Fast path: a previously built, complete view dir.
	if viewComplete(viewDir, entries) {
		llvmViewDir = viewDir
		return viewDir, nil
	}

	// View not yet published. Serialize population across processes so a
	// partially-built view is never observable: the first builder populates while
	// others block until publish. The fast path above (viewComplete) is reached
	// without the lock, so builds against an already-published view pay nothing.
	// The lock lives OUTSIDE the llvm-view tree (a sibling file) so CleanLLVMCache
	// (os.RemoveAll of cache/llvm-view) can't delete it mid-hold.
	lockPath := filepath.Join(home, "cache", "llvm-view.lock")
	unlock, err := blobstore.Lock(lockPath, "promise (materializing LLVM toolchain)",
		"Waiting for another process to finish staging the LLVM toolchain...")
	if err != nil {
		return "", err
	}
	defer unlock()

	// Double-checked: another process may have published the view while we waited
	// for the lock. If so, we're done — and lock-free for the rest of our lifetime.
	if viewComplete(viewDir, entries) {
		llvmViewDir = viewDir
		return viewDir, nil
	}

	if !allowFetch {
		// No-fetch probe: only usable if every blob is already cached or can be
		// materialized from the host-stable prebuilts cache (neither hits the
		// network). Serving prebuilts here — ahead of Homebrew/PATH in findLLVMTool
		// — keeps the toolchain deterministic on a machine that has built the
		// compiler.
		for _, e := range entries {
			if store.Has(e.SHA256) {
				continue
			}
			if prebuiltToolPath(strings.TrimPrefix(e.Name, llvmEntryPrefix)) != "" {
				continue
			}
			return "", nil
		}
	}

	resolver := blobstore.NewResolver(store, m)
	defer resolver.Close()

	// First-run feedback: when this fetch will hit the network, tell the user
	// what's happening (a bare `promise exec` otherwise sits with a black screen
	// for minutes while the LLVM toolchain downloads). On an interactive
	// terminal, summarize + confirm before starting, and stream a progress bar;
	// in scripts/CI (no TTY) we stay silent and just proceed as before.
	var needFetch []*blobstore.ManifestEntry
	var download int64
	for _, e := range entries {
		if store.Has(e.SHA256) {
			continue
		}
		if prebuiltToolPath(strings.TrimPrefix(e.Name, llvmEntryPrefix)) != "" {
			continue // satisfied locally from the prebuilts cache — no download
		}
		needFetch = append(needFetch, e)
		download += e.DownloadSize()
	}
	if len(needFetch) > 0 && isCharDevice(os.Stderr) {
		switch {
		case !prefetchNoPrompt && isCharDevice(os.Stdin):
			// Lazy first-compile fetch on an interactive terminal — confirm first.
			if !confirmToolchainDownload("LLVM toolchain", len(needFetch), download) {
				return "", fmt.Errorf("LLVM toolchain download declined — run again and accept, or install the -full build (offline). See `promise --help`")
			}
		default:
			// Install-time prefetch (prefetchNoPrompt), or no stdin to prompt on:
			// announce rather than ask — the user already opted in.
			fmt.Fprintf(os.Stderr, "Downloading Promise's LLVM toolchain (%d components, ~%s download); cached for future runs...\n", len(needFetch), formatSize(download))
		}
		resolver.SetProgress(newTTYProgress(os.Stderr))
		// On an interactive terminal, confirm before the ~GB upstream-archive
		// fallback (only reached if every blob host failed). No stdin to prompt
		// on → leave the hook unset so the fetch proceeds as before.
		if isCharDevice(os.Stdin) {
			resolver.SetArchiveConfirm(confirmArchiveFallback)
		}
	}

	// Materialize into a sibling temp dir, then publish all-or-nothing via
	// rename(2). A crashed populator leaves only an orphan temp dir (reclaimed by
	// GC), never a half-built viewDir.
	if err := publishViewDir(filepath.Dir(viewDir), viewDir, func(tmpDir string) error {
		for _, e := range entries {
			toolName := strings.TrimPrefix(e.Name, llvmEntryPrefix)
			var blobPath string
			switch {
			case store.Has(e.SHA256):
				// Content-verified CAS blob (a prior fetch, or embedded LLVM staged
				// by a -full build) is canonical — always prefer it.
				blobPath = store.BlobPath(e.SHA256)
			case prebuiltToolPath(toolName) != "":
				// Otherwise reuse the LLVM toolchain bin/build / ./make already staged
				// on this host instead of downloading it. The prebuilt is the same LLVM
				// release the manifest names (extracted from the same upstream
				// archive), just signed differently, so its sha differs from the CAS
				// blob — but the view is validated by presence and materializeViewFile
				// re-signs the copy on macOS, so a differently-signed prebuilt is fine.
				blobPath = prebuiltToolPath(toolName)
			default:
				p, rerr := resolver.Resolve(e.Name)
				if rerr != nil {
					return rerr // surface offline / broken-release error
				}
				blobPath = p
			}
			dst := filepath.Join(tmpDir, toolName)
			if err := materializeViewFile(blobPath, dst); err != nil {
				return err
			}
		}
		return makeLLDAliases(tmpDir)
	}); err != nil {
		return "", err
	}

	llvmViewDir = viewDir
	return viewDir, nil
}

// prebuiltsCacheRoot returns the host-stable prebuilts cache that bin/build /
// ./make populate with the LLVM toolchain (opt/llc/lld). It mirrors
// tools/build/common.PrebuiltsCacheRoot; the compiler and build tools are
// separate Go modules, so this small, stable path rule is duplicated rather than
// imported. Returns "" only when the user home dir cannot be determined.
func prebuiltsCacheRoot() string {
	if v := strings.TrimSpace(os.Getenv("PROMISE_PREBUILTS_CACHE")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME")); v != "" {
		return filepath.Join(v, "promise", "prebuilts")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Caches", "promise", "prebuilts")
	case "windows":
		if v := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); v != "" {
			return filepath.Join(v, "promise", "prebuilts")
		}
		return filepath.Join(home, "AppData", "Local", "promise", "prebuilts")
	default:
		return filepath.Join(home, ".cache", "promise", "prebuilts")
	}
}

// prebuiltToolPaths memoizes the per-process prebuilt-tool lookup so the view
// build (which asks per tool) and the pre-fetch probes don't restat the cache.
var (
	prebuiltToolMu    sync.Mutex
	prebuiltToolCache map[string]string
)

// prebuiltToolPath returns the path to a raw LLVM tool (opt/llc/lld, or the
// .exe forms on Windows) in the host-stable prebuilts cache, or "" if it is not
// available there. This lets a machine that has built the compiler materialize
// the toolchain view from local disk instead of downloading it — the whole point
// of the prebuilts cache is that these tools already live "outside promise home".
//
// The prebuilt is trusted the same way findLLVMTool already trusts a matching
// LLVM on PATH/Homebrew: it is the LLVM release this compiler build was pinned to
// (bin/build wrote tools.ok only after verifying the upstream archive sha), and
// the copy placed into the view is re-signed on macOS.
func prebuiltToolPath(toolName string) string {
	if toolName == "" {
		return ""
	}
	prebuiltToolMu.Lock()
	defer prebuiltToolMu.Unlock()
	if prebuiltToolCache == nil {
		prebuiltToolCache = map[string]string{}
	}
	if p, ok := prebuiltToolCache[toolName]; ok {
		return p
	}
	resolved := findPrebuiltTool(toolName)
	prebuiltToolCache[toolName] = resolved
	return resolved
}

func findPrebuiltTool(toolName string) string {
	root := prebuiltsCacheRoot()
	if root == "" {
		return ""
	}
	target := runtime.GOOS + "-" + runtime.GOARCH
	// Any llvm-slim version dir for this target. When several are cached from
	// different checkouts, prefer the newest — all are the pinned major and
	// functionally interchangeable (the same trust model findLLVMTool already
	// applies to any matching LLVM on PATH/Homebrew).
	matches, _ := filepath.Glob(filepath.Join(root, "llvm-slim", "*", target, toolName))
	best, bestVer := "", ""
	for _, p := range matches {
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() || fi.Size() == 0 {
			continue
		}
		// tools.ok is written only after bin/build fully extracts + verifies the
		// toolchain dir, so its presence rejects a half-populated cache.
		if _, err := os.Stat(filepath.Join(filepath.Dir(p), "tools.ok")); err != nil {
			continue
		}
		// Path shape: <root>/llvm-slim/<version>/<target>/<tool>.
		ver := filepath.Base(filepath.Dir(filepath.Dir(p)))
		if best == "" || compareLLVMVersion(ver, bestVer) > 0 {
			best, bestVer = p, ver
		}
	}
	return best
}

// compareLLVMVersion compares dot-separated numeric version strings ("22.1.0"),
// returning -1, 0, or 1. A non-numeric component is treated as 0 so a malformed
// directory name never outranks a well-formed one — unlike a lexical sort, this
// orders 22.10.0 after 22.9.0.
func compareLLVMVersion(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var ai, bi int
		if i < len(as) {
			ai, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bi, _ = strconv.Atoi(bs[i])
		}
		if ai != bi {
			if ai > bi {
				return 1
			}
			return -1
		}
	}
	return 0
}

// publishViewDir materializes a view directory atomically: it creates a sibling
// temp dir under parent, runs populate(tmpDir), then publishes the result with a
// single rename(2) so a partially-populated view is never observable. On any
// failure (populate error or rename) the temp dir is removed and viewDir is left
// untouched. Callers must hold the cross-process materialization lock and have
// confirmed the view is incomplete (so the pre-publish RemoveAll of viewDir can't
// race a completed reader).
func publishViewDir(parent, viewDir string, populate func(tmpDir string) error) error {
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(parent, ".tmp-"+filepath.Base(viewDir)+"-*")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			os.RemoveAll(tmpDir)
		}
	}()
	if err := populate(tmpDir); err != nil {
		return err
	}
	// Atomic publish. Remove any incomplete leftover first (safe under the caller's
	// lock + incompleteness precondition). rename(2) is atomic on POSIX; on Windows
	// the target must not exist, hence the RemoveAll.
	os.RemoveAll(viewDir)
	if err := os.Rename(tmpDir, viewDir); err != nil {
		return fmt.Errorf("publish view %s: %w", viewDir, err)
	}
	published = true
	return nil
}

// blobSetKey returns a short, order-independent content key for a set of manifest
// entries (their name+sha256), used to invalidate the derived view dir whenever
// the blob set changes (e.g. an LLVM version bump).
func blobSetKey(entries []*blobstore.ManifestEntry) string {
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, e.Name+":"+strings.ToLower(strings.TrimSpace(e.SHA256)))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])[:16]
}

// viewComplete reports whether every manifest LLVM blob already has a file in the
// view dir, plus the lld-mode aliases when lld is present (so we can skip
// re-materialization across runs without missing the aliases a crashed
// mid-materialize run might have left out).
func viewComplete(viewDir string, entries []*blobstore.ManifestEntry) bool {
	hasLLD := false
	for _, e := range entries {
		name := strings.TrimPrefix(e.Name, llvmEntryPrefix)
		if _, err := os.Lstat(filepath.Join(viewDir, name)); err != nil {
			return false
		}
		if name == "lld" || name == "lld.exe" {
			hasLLD = true
		}
	}
	if hasLLD {
		for link := range embeddedLLVMSymlinks {
			name := link
			if runtime.GOOS == "windows" {
				name = link + ".exe"
			}
			if _, err := os.Lstat(filepath.Join(viewDir, name)); err != nil {
				return false
			}
		}
	}
	return true
}

// materializeViewFile places a CAS blob into the view dir. macOS gets a patched
// + signed working copy; other platforms get a symlink (copy on Windows, which
// needs admin for symlinks).
func materializeViewFile(blobPath, dst string) error {
	os.Remove(dst)
	switch runtime.GOOS {
	case "darwin":
		copyFile(blobPath, dst, 0o755)
		blobstore.PatchAndSignMachO(dst)
		return nil
	case "windows":
		copyFile(blobPath, dst, 0o755)
		return nil
	default:
		return os.Symlink(blobPath, dst)
	}
}

// makeLLDAliases creates the lld-mode aliases (ld.lld/ld64.lld/lld-link/wasm-ld)
// pointing at the materialized "lld" in the view dir. No-op if lld is absent.
func makeLLDAliases(viewDir string) error {
	lldName := "lld"
	if runtime.GOOS == "windows" {
		lldName = "lld.exe"
	}
	lldPath := filepath.Join(viewDir, lldName)
	if _, err := os.Stat(lldPath); err != nil {
		return nil
	}
	for link := range embeddedLLVMSymlinks {
		name := link
		if runtime.GOOS == "windows" {
			name = link + ".exe"
		}
		linkPath := filepath.Join(viewDir, name)
		os.Remove(linkPath)
		if runtime.GOOS == "windows" {
			copyFile(lldPath, linkPath, 0o755)
		} else {
			if err := os.Symlink(lldName, linkPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// stageEmbeddedLLVMBlobs decompresses each embedded LLVM blob (full-variant
// builds) and stores it into the CAS by content hash (§2.4 step 3). The codec is
// self-describing via the embedded file's extension: the dist-CAS publish path
// embeds the brotli <sha>.br directly (.br — T0807, byte-identical to the CAS
// asset, the smaller shipped artifact), while the dev/slim and Homebrew bundle
// paths embed .gz. The decompressed bytes hash to the manifest entry's sha256
// (the generator hashed the same raw files), so resolveLLVMView later finds them
// as CAS hits — no network.
func stageEmbeddedLLVMBlobs(store *blobstore.Store) error {
	if !hasEmbeddedLLVM {
		return nil
	}
	prefix := llvmEmbedPrefix
	for _, name := range llvmEmbeddedFiles() {
		data, err := embeddedLLVM.ReadFile(prefix + "/" + name)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", name, err)
		}
		raw, err := decompressEmbeddedLLVM(name, data)
		if err != nil {
			return fmt.Errorf("decompress %s: %w", name, err)
		}
		if _, err := store.StageBlob(raw); err != nil {
			return fmt.Errorf("stage %s: %w", name, err)
		}
	}
	return nil
}

// decompressEmbeddedLLVM decompresses an embedded LLVM blob, dispatching on the
// file's extension (the codec is self-describing): .br → brotli (publish path),
// .gz → gzip (dev/slim + Homebrew bundle paths). An unknown extension is a hard
// error so a new bundler codec can't silently ship an undecodable blob.
func decompressEmbeddedLLVM(name string, data []byte) ([]byte, error) {
	switch {
	case strings.HasSuffix(name, ".br"):
		return unbrotliBytes(data)
	case strings.HasSuffix(name, ".gz"):
		return gunzipBytes(data)
	default:
		return nil, fmt.Errorf("unknown embedded blob codec for %q", name)
	}
}

// resolveMuslCRTView materializes the musl CRT objects from the CAS into a
// per-arch view dir (cache/crt-view/<arch>/). Returns ("", nil) when the
// manifest carries no musl entries (thin placeholder / cross-arch), so the
// caller falls through to its embedded/system probes. A fetch failure surfaces
// the offline / broken-release error.
func resolveMuslCRTView(arch string) (string, error) {
	m, err := loadEmbeddedManifest()
	if err != nil || m == nil {
		return "", nil
	}
	var entries []*blobstore.ManifestEntry
	for _, f := range muslCRTFiles {
		e, ok := m.Lookup("musl-" + f)
		if !ok {
			return "", nil // manifest doesn't carry musl blobs → fall through
		}
		entries = append(entries, e)
	}
	home, err := module.PromiseHome()
	if err != nil {
		return "", err
	}
	// Content-key the view dir on the blob set (see resolveLLVMView) so a CRT
	// version bump never serves stale objects from a name-only match.
	viewDir := filepath.Join(home, "cache", "crt-view", arch+"-"+blobSetKey(entries))
	// Fast path: a previously published view (lock-free).
	if muslCRTComplete(viewDir) {
		return viewDir, nil
	}
	store, err := blobstore.NewStore()
	if err != nil {
		return "", err
	}
	// Serialize population across processes (same atomic-publish barrier as the
	// LLVM view) so a partially-built CRT view is never observable. Lock lives
	// outside the crt-view tree so CleanCRTCache can't delete it mid-hold.
	lockPath := filepath.Join(home, "cache", "crt-view.lock")
	unlock, err := blobstore.Lock(lockPath, "promise (materializing musl CRT)",
		"Waiting for another process to finish staging the musl CRT...")
	if err != nil {
		return "", err
	}
	defer unlock()
	// Double-checked after acquiring the lock.
	if muslCRTComplete(viewDir) {
		return viewDir, nil
	}
	resolver := blobstore.NewResolver(store, m)
	defer resolver.Close()
	if err := publishViewDir(filepath.Dir(viewDir), viewDir, func(tmpDir string) error {
		for _, f := range muslCRTFiles {
			entry, _ := m.Lookup("musl-" + f)
			var blobPath string
			if store.Has(entry.SHA256) {
				blobPath = store.BlobPath(entry.SHA256)
			} else {
				p, rerr := resolver.Resolve("musl-" + f)
				if rerr != nil {
					return rerr
				}
				blobPath = p
			}
			copyFile(blobPath, filepath.Join(tmpDir, f), 0o644)
		}
		return nil
	}); err != nil {
		return "", err
	}
	return viewDir, nil
}

// unbrotliBytes decompresses a brotli byte slice.
func unbrotliBytes(data []byte) ([]byte, error) {
	return io.ReadAll(brotli.NewReader(bytes.NewReader(data)))
}

// gunzipBytes decompresses a gzip byte slice.
func gunzipBytes(data []byte) ([]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	return io.ReadAll(gr)
}
