package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
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
	"time"

	"github.com/andybalholm/brotli"
	"github.com/promise-language/promise/compiler/internal/blobstore"
	"github.com/promise-language/promise/compiler/internal/casmetrics"
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
// at release time without codesign; distribution.md#macos + T0769 §10 open contract). On Linux/Windows
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
	// This process is about to use this home's toolchain surface, whether or not
	// anything has to be materialized into it. Recording the home rather than
	// the population is what makes the count independent of how warm the host
	// was: one home reads 1 warm and 1 cold, while three private homes read 3
	// either way — which is the shape T2133 had and nothing measured (T2143).
	casmetrics.RegisterHome(home)
	// Key the view dir by the blob-hash set, not just the target: a compiler/LLVM
	// version bump changes the entry sha256s, yielding a fresh view dir so stale
	// tools from a previous epoch are never served from a name-only match. The CAS
	// itself is content-addressed and never stale; the view is a derived working
	// copy with content-based invalidation. (Old view dirs are reclaimed by
	// `promise doctor --repair` and `promise clean --global`; orphan `.tmp-*`
	// staging dirs left by a crashed populator are reaped opportunistically by
	// publishViewDir at the next materialization.)
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
	// The lock lives OUTSIDE the llvm-view tree (a sibling file) so
	// cleanViewsUnderLock's RemoveAll of cache/llvm-view can't delete it mid-hold.
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
		// network). Serving prebuilts here keeps the toolchain deterministic on a
		// machine that has built the compiler — the prebuilts cache holds the same
		// pinned release, so it is a local copy of the toolchain, not a host find.
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
	// rename(2). A crashed populator leaves only an orphan temp dir (reaped on a
	// later materialization by sweepStaleViewStaging, or by `promise doctor
	// --repair`), never a half-built viewDir.
	//
	// exploded accumulates what this population COSTS in bytes — which is not
	// the size of the view: Linux symlinks and Windows hardlinks write nothing,
	// where macOS has to own its bytes to patch and re-sign them. That
	// asymmetry is the signal, so each materializer reports what it wrote
	// rather than the view being measured afterwards (T2143).
	var exploded int64
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
			n, err := materializeViewFile(blobPath, dst)
			exploded += n
			if err != nil {
				return err
			}
		}
		n, aerr := makeLLDAliases(tmpDir)
		exploded += n
		if aerr != nil {
			return aerr
		}
		n, serr := writeToolchainStubs(tmpDir)
		exploded += n
		return serr
	}); err != nil {
		return "", err
	}
	// Recorded after the publish, so a population that crashed halfway is not
	// counted as one: the view it would have produced does not exist.
	casmetrics.AddMaterialized("llvm-view", exploded, 1)

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
// The prebuilt is pinned, not discovered: it is the LLVM release this compiler
// build was pinned to (bin/build wrote tools.ok only after verifying the upstream
// archive sha), and the copy placed into the view is re-signed on macOS.
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
	// functionally interchangeable.
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

// staleViewStagingAge is how old an orphan .tmp-* view-staging dir must be before
// publishViewDir reaps it. Comfortably above any live populator's lifetime (an
// LLVM toolchain download is minutes, not hours) so age-gating never races a
// concurrent populate() writing into its own fresh temp dir.
const staleViewStagingAge = 24 * time.Hour

// sweepStaleViewStaging removes orphan `.tmp-<view>-*` staging dirs left in a view
// parent by a populator killed mid-staging (SIGKILL/crash skips publishViewDir's
// deferred RemoveAll). Only entries older than maxAge are removed, so a concurrent
// populate()'s own fresh temp dir is never touched. Best-effort: read/stat/remove
// errors are ignored (the sweep is a housekeeping side effect of materialization,
// never a reason to fail a build). Returns the count removed.
func sweepStaleViewStaging(parent string, maxAge time.Duration) int {
	ents, err := os.ReadDir(parent)
	if err != nil {
		return 0
	}
	removed := 0
	for _, ent := range ents {
		if !ent.IsDir() || !strings.HasPrefix(ent.Name(), ".tmp-") {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		if time.Since(info.ModTime()) <= maxAge {
			continue
		}
		if os.RemoveAll(filepath.Join(parent, ent.Name())) == nil {
			removed++
		}
	}
	return removed
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
	// Reap orphan `.tmp-*` staging dirs a crashed populator left behind. Done before
	// MkdirTemp so the temp dir we're about to create (age 0) is never a candidate,
	// independent of the age gate.
	sweepStaleViewStaging(parent, staleViewStagingAge)
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
	if hasLLD && needsToolchainStubs() {
		// A view materialized by a compiler that predates the compatibility
		// stubs (T1774) has the tools but not the stub libraries, and would
		// otherwise be served from the fast path forever.
		if _, err := os.Stat(filepath.Join(viewDir, stubMarkerName)); err != nil {
			return false
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

// materializeViewFile places a CAS blob into the view dir and reports how many
// bytes doing so cost. macOS gets a patched + signed working copy; Linux gets a
// symlink, and Windows — where a symlink needs admin — gets a hardlink.
//
// Only macOS needs bytes of its own: PatchAndSignMachO rewrites the file in
// place, and distribution.md#macos requires the CAS blob to stay the raw upstream bytes it was
// hashed as. A clone (copy-on-write, its own inode) satisfies both at metadata
// cost; a streamed copy is the fallback where clonefile is unavailable. A
// hardlink there would be a patch applied to the hashed content (T2133).
//
// The byte count is what the platform actually wrote, so the link and symlink
// branches report zero (T2143): a view that costs metadata is not a view that
// costs 375 MB, and conflating them would hide the difference the accounting
// exists to show.
func materializeViewFile(blobPath, dst string) (int64, error) {
	return materializeExecutable(blobPath, dst, blobstore.PatchAndSignMachO)
}

// materializeRuntimeFile places a WASM test runtime (wasmtime, node) into its
// view dir (T2169). Same placement as an LLVM tool — it is an executable the
// compiler is about to run — but the macOS step is a signature CHECK rather
// than a patch: these binaries are self-contained, so there is nothing to
// rewrite and an unconditional re-sign could only replace a good upstream
// signature with a weaker one. See blobstore.EnsureAdHocSignature.
func materializeRuntimeFile(blobPath, dst string) (int64, error) {
	return materializeExecutable(blobPath, dst, blobstore.EnsureAdHocSignature)
}

// materializeExecutable is the body both of the above share, with the macOS
// post-step as a parameter — the one thing that differs between placing a tool
// this compiler links WITH and a runtime it merely runs. Everything else (which
// platform owns bytes, which gets a link, what the accounting reports) is a
// property of the placement, not of the file.
func materializeExecutable(blobPath, dst string, signDarwin func(string)) (int64, error) {
	os.Remove(dst)
	switch runtime.GOOS {
	case "darwin":
		// A clone shares the source's blocks until one side is written, but the
		// patch+re-sign below always writes, so the bytes are owed either way.
		written := blobSize(blobPath)
		if err := blobstore.CloneFile(blobPath, dst); err != nil {
			if cerr := copyFileAtomic(blobPath, dst, viewToolPerm); cerr != nil {
				return 0, cerr
			}
		}
		// Both branches have to leave the same file behind: clonefile inherits
		// the source's mode where the copy sets one, so a blob that ever reached
		// the CAS non-executable would give a runnable tool down one path and an
		// EACCES down the other.
		if err := os.Chmod(dst, viewToolPerm); err != nil {
			return written, err
		}
		signDarwin(dst)
		return written, nil
	case "windows":
		return linkOrCopyBlob(blobPath, dst, viewToolPerm)
	default:
		return 0, os.Symlink(blobPath, dst)
	}
}

// blobSize is the size of a file this code is about to copy, for accounting.
// An unreadable source is reported as costing nothing rather than failing the
// materialization: the copy itself is about to report the real error.
func blobSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// viewToolPerm is the mode a materialized LLVM tool carries: it has to be
// executable, since the view dir is what findLLVMTool hands the driver.
const viewToolPerm = 0o755

// linkOrCopyBlob places an immutable blob — a CAS entry, or a tool from the
// host prebuilts cache — into a view dir by hardlink, falling back to a streamed
// copy when the two are not on one filesystem.
//
// Nothing ever rewrites a view file on the platforms that take this path (the
// macOS patch+re-sign is the one mutation, and it never comes here), so sharing
// the source's inode is safe and makes a cold view cost metadata instead of the
// ~900 MB Windows was copying per PROMISE_HOME (T2133). CAS blobs are committed
// 0755, so a link is already executable; perm applies only to the copy.
//
// A link keeps the source's bytes alive until the view dir is removed too, so
// the space is reclaimed when `promise doctor --repair` sweeps stale views
// rather than when gc drops the blob.
//
// Returns the bytes written: zero when the link succeeded, the source's size
// when it had to fall back to a copy (T2143).
func linkOrCopyBlob(src, dst string, perm os.FileMode) (int64, error) {
	return linkOrCopy(os.Link, src, dst, perm)
}

// linkOrCopy is linkOrCopyBlob with the link syscall injected, so the
// cross-filesystem fallback — otherwise reachable only on a host with two
// filesystems arranged just so — can be exercised anywhere. A parameter rather
// than a package var, for the same reason renameRetrying takes one: a var a
// test swaps is a var every parallel test in the package races.
func linkOrCopy(link func(src, dst string) error, src, dst string, perm os.FileMode) (int64, error) {
	os.Remove(dst)
	if err := link(src, dst); err == nil {
		return 0, nil
	}
	written := blobSize(src)
	if err := copyFileAtomic(src, dst, perm); err != nil {
		return 0, err
	}
	return written, nil
}

// makeLLDAliases creates the lld-mode aliases (ld.lld/ld64.lld/lld-link/wasm-ld)
// pointing at the materialized "lld" in the view dir, and reports the bytes
// they cost. No-op if lld is absent.
func makeLLDAliases(viewDir string) (int64, error) {
	lldName := "lld"
	if runtime.GOOS == "windows" {
		lldName = "lld.exe"
	}
	lldPath := filepath.Join(viewDir, lldName)
	if _, err := os.Stat(lldPath); err != nil {
		return 0, nil
	}
	var written int64
	for link := range embeddedLLVMSymlinks {
		name := link
		if runtime.GOOS == "windows" {
			name = link + ".exe"
		}
		linkPath := filepath.Join(viewDir, name)
		os.Remove(linkPath)
		if runtime.GOOS == "windows" {
			// Hardlink, not a copy: four aliases of a ~131 MB lld were 525 MB of
			// duplicate bytes per view, and lld is never rewritten in place.
			n, err := linkOrCopyBlob(lldPath, linkPath, viewToolPerm)
			written += n
			if err != nil {
				return written, err
			}
		} else {
			if err := os.Symlink(lldName, linkPath); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

// stageEmbeddedLLVMBlobs decompresses each embedded LLVM blob (full-variant
// builds) and stores it into the CAS by content hash (distribution.md#what-install-does step 3). The codec is
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
	var staged int64
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
		staged += int64(len(raw))
	}
	// Decompressing an embedded blob INTO the store is the same per-home cost
	// as exploding one out of it — hundreds of megabytes written because this
	// home did not have them yet — so it is accounted the same way (T2143).
	recordMaterializedIfWritten("llvm-stage", staged)
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

// targetDepManifestName is the runtime-manifest logical name for one file of a
// per-arch *target* dependency, e.g. ("musl", "aarch64-linux-musl", "crt1.o") →
// "musl-aarch64-linux-musl-crt1.o".
//
// The arch is part of the NAME, not just the manifest's identity, because these
// are target dependencies: one host manifest can legitimately carry several
// arches at once (a linux-amd64 host cross-linking for linux-arm64), and an
// unqualified "musl-crt1.o" could only ever describe one of them. Keep the
// format in lockstep with MuslManifestName / OpenSSLManifestName /
// CompilerRTManifestName in tools/build/common — those live in a separate Go
// module, so the format is duplicated by necessity (pinned by
// TestMuslManifestName, TestOpenSSLManifestName, TestCompilerRTManifestName).
func targetDepManifestName(dep, arch, file string) string {
	return dep + "-" + arch + "-" + file
}

func muslManifestName(arch, file string) string { return targetDepManifestName("musl", arch, file) }

func opensslManifestName(arch, file string) string {
	return targetDepManifestName("openssl", arch, file)
}

func compilerRTManifestName(arch, file string) string {
	return targetDepManifestName("compiler-rt", arch, file)
}

// resolveTargetDepView materializes one per-arch target dependency (musl CRT,
// static OpenSSL, compiler-rt builtins) from the CAS into a view dir at
// cache/<viewSubdir>/<arch>-<blobSetKey>/. Returns ("", nil) when the embedded
// manifest carries no entries for this dep+arch (thin placeholder / an arch this
// binary's manifest doesn't cover), so the caller falls through to its
// embedded/system probes. A fetch failure surfaces the offline /
// broken-release error.
//
// One implementation for all three: the dependencies differ only in their file
// set and cache-dir name, and every one of them wants the same atomic-publish
// barrier. `holder` names this process in the lock file; `waiting` is shown to
// a process that has to wait for it.
//
// The view dir is content-keyed on the blob set (see resolveLLVMView) so a
// version bump never serves stale files from a name-only match. The lock file
// lives OUTSIDE the view tree so cleanViewsUnderLock's RemoveAll can't delete
// it mid-hold.
//
// `local` supplies this binary's own embedded copy of a file, or nil when it
// carries none for this arch. It is the third blob source, and it is what keeps
// a cold home off the wire: without it a home whose CAS is empty downloads the
// musl CRT and the compiler-rt builtins that the running binary already
// contains — once per home, which is 55 MB across one sweep of the CLI test
// packages (T2150). resolveLLVMView has had the same third source since it was
// written (prebuiltToolPath); the target-dep path never grew one.
//
// The manifest sha is not a reason to prefer the download: the embedded copy is
// sliced from the same pinned prebuilt the manifest names, and the view is
// validated by presence, exactly as resolveLLVMView's prebuilt branch is. An
// arch this binary embeds nothing for still fetches, which is what keeps step 4
// the only path that can serve one (docs/runtime-architecture.md).
func resolveTargetDepView(dep, viewSubdir, arch string, files []string, holder, waiting string,
	local func(file string) ([]byte, bool)) (string, error) {
	return resolveDepView(viewSubdir, arch, files,
		func(file string) string { return targetDepManifestName(dep, arch, file) },
		func(blobPath, dst string) (int64, error) { return linkOrCopyBlob(blobPath, dst, targetDepPerm) },
		targetDepPerm, holder, waiting, local)
}

// targetDepPerm is the mode a materialized target-dependency file carries.
// These are inert relocatable ELF objects and static archives — read by the
// linker, never executed — so unlike viewToolPerm they need no execute bit.
const targetDepPerm = 0o644

// resolveDepView is the body resolveTargetDepView and resolveRuntimeView share:
// look the files up in the embedded manifest, key a view dir on the blob set,
// and populate it behind the same atomic-publish barrier. What differs between
// a target dependency and a host runtime is only which NAME the manifest
// carries them under (`nameFor`) and how a file is PLACED once found (`place` —
// a 0644 link for an archive the linker reads, an executable materialization
// for a runtime the compiler runs), so those are the parameters and everything
// else is one implementation.
//
// `key` distinguishes view dirs that would otherwise collide: the musl arch for
// a target dependency, the host target for a runtime.
//
// `perm` is the mode the `local` branch writes with. It is separate from
// `place` because that branch writes bytes this binary already holds rather
// than placing a blob, and a runtime written 0644 would be a view that
// materializes successfully and then cannot be executed.
// depViewDir computes where a dependency's view dir WOULD be, without
// materializing anything or touching the network. Returns ("", nil) when the
// embedded manifest carries no entry for the file set (thin placeholder, or a
// dependency this binary has no pin for).
//
// Split out of resolveDepView so the "where is it" rule has one owner: the
// no-fetch probes (`promise doctor`) need the same content-keyed path the
// populator writes to, and a second copy of the keying would be a second
// source of truth for a directory name that must match exactly.
//
// A zero-valued Dir means the manifest carries no entry for the file set.
func depViewDir(viewSubdir, key string, files []string, nameFor func(file string) string) (depView, error) {
	m, err := loadEmbeddedManifest()
	if err != nil || m == nil {
		return depView{}, nil
	}
	var entries []*blobstore.ManifestEntry
	for _, f := range files {
		e, ok := m.Lookup(nameFor(f))
		if !ok {
			return depView{}, nil // manifest doesn't carry this dep's blobs → fall through
		}
		entries = append(entries, e)
	}
	home, err := module.PromiseHome()
	if err != nil {
		return depView{}, err
	}
	return depView{
		Manifest: m,
		Dir:      filepath.Join(home, "cache", viewSubdir, key+"-"+blobSetKey(entries)),
		// The lock lives OUTSIDE the view tree so cleanViewsUnderLock's RemoveAll
		// of cache/<viewSubdir> cannot delete it mid-hold.
		LockPath: filepath.Join(home, "cache", viewSubdir+".lock"),
		Entries:  entries,
	}, nil
}

// depView is where one dependency's view lives and what it is made of — the
// answer depViewDir computes and both the populator and the no-fetch probes
// read.
type depView struct {
	Manifest *blobstore.Manifest
	Dir      string
	LockPath string
	Entries  []*blobstore.ManifestEntry
}

func resolveDepView(viewSubdir, key string, files []string,
	nameFor func(file string) string, place func(blobPath, dst string) (int64, error),
	perm os.FileMode, holder, waiting string,
	local func(file string) ([]byte, bool)) (string, error) {
	view, err := depViewDir(viewSubdir, key, files, nameFor)
	if err != nil || view.Dir == "" {
		return "", err
	}
	m, viewDir := view.Manifest, view.Dir
	// Fast path: a previously published view (lock-free).
	if depFilesPresent(viewDir, files) {
		return viewDir, nil
	}
	store, err := blobstore.NewStore()
	if err != nil {
		return "", err
	}
	// Serialize population across processes so a partially-built view is never
	// observable (same barrier as the LLVM view).
	unlock, err := blobstore.Lock(view.LockPath, holder, waiting)
	if err != nil {
		return "", err
	}
	defer unlock()
	// Double-checked after acquiring the lock.
	if depFilesPresent(viewDir, files) {
		return viewDir, nil
	}
	resolver := blobstore.NewResolver(store, m)
	defer resolver.Close()
	var exploded int64
	if err := publishViewDir(filepath.Dir(viewDir), viewDir, func(tmpDir string) error {
		for _, f := range files {
			name := nameFor(f)
			entry, _ := m.Lookup(name)
			dst := filepath.Join(tmpDir, f)
			var blobPath string
			switch source, data := chooseTargetDepSource(store.Has(entry.SHA256), local, f); source {
			case sourceStore:
				blobPath = store.BlobPath(entry.SHA256)
			case sourceEmbedded:
				// Writing the bytes we already hold costs a few MB into the same
				// all-or-nothing staging dir; fetching them costs the wire.
				if werr := os.WriteFile(dst, data, perm); werr != nil {
					return werr
				}
				exploded += int64(len(data))
				continue
			default:
				p, rerr := resolver.Resolve(name)
				if rerr != nil {
					return rerr
				}
				blobPath = p
			}
			n, err := place(blobPath, dst)
			exploded += n
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return "", err
	}
	casmetrics.AddMaterialized(viewSubdir, exploded, 1)
	return viewDir, nil
}

// targetDepSource is where one file of a target dependency comes from.
type targetDepSource int

const (
	// sourceStore is a content-verified CAS blob: canonical, always preferred.
	sourceStore targetDepSource = iota
	// sourceEmbedded is this binary's own copy of the artifact, chosen over the
	// wire for bytes already on hand (T2150).
	sourceEmbedded
	// sourceWire is a fetch from the manifest's hosts — the only path that can
	// serve an arch this binary does not embed.
	sourceWire
)

// chooseTargetDepSource is resolveDepView's three-way decision, lifted out of
// the populate loop so it can be checked on every platform.
//
// That mattered more than it looked when it was written: the only loop calling
// it then was the musl/compiler-rt one, which cannot run off Linux — no other
// target's manifest carries those blobs, so the function returned at the
// manifest lookup and the branch below was dead code on three of four
// platforms. A precedence turned on by a change verifiable only where one CI
// job runs is the shape T2152 and T2133 both had. The WASM runtimes are host
// dependencies, so since T2169 this loop runs on every platform — the seam is
// still the right place for the decision, and now the real callers exercise it
// too.
//
// The order is the whole semantic: the store first because its bytes are
// content-verified, this binary's copy next because it costs no network, and the
// wire last because it is the only one that can answer for an arch we do not
// carry.
func chooseTargetDepSource(inStore bool, local func(string) ([]byte, bool), file string) (targetDepSource, []byte) {
	if inStore {
		return sourceStore, nil
	}
	if data, ok := localTargetDepFile(local, file); ok {
		return sourceEmbedded, data
	}
	return sourceWire, nil
}

// localTargetDepFile is local(file) with a nil `local` reading as "this binary
// carries no copy of this dep+arch", so the caller does not have to spell both.
func localTargetDepFile(local func(string) ([]byte, bool), file string) ([]byte, bool) {
	if local == nil {
		return nil, false
	}
	return local(file)
}

// embeddedTargetDep adapts one of the build-tagged embed.FS resource trees into
// the `local` source resolveTargetDepView takes. present is the per-platform
// hasEmbedded* constant, so a build that embeds nothing for this dependency
// answers nil and the view falls through to the CAS/resolver as before.
//
// A file the embed FS does not carry (the OpenSSL placeholder sentinel, or an
// arch this binary was not built for) reads as absent rather than as an error:
// the point is only to skip a download for bytes already on hand.
func embeddedTargetDep(fsys embed.FS, prefix string, present bool) func(string) ([]byte, bool) {
	if !present {
		return nil
	}
	return func(file string) ([]byte, bool) {
		data, err := fsys.ReadFile(prefix + "/" + file)
		return data, err == nil
	}
}

// resolveMuslCRTView materializes the musl CRT objects from the CAS (T0530).
func resolveMuslCRTView(arch string) (string, error) {
	return resolveTargetDepView("musl", "crt-view", arch, muslCRTFiles,
		"promise (materializing musl CRT)",
		"Waiting for another process to finish staging the musl CRT...",
		embeddedTargetDep(embeddedMuslCRT, "resources/crt/"+arch, hasEmbeddedMuslCRT))
}

// resolveOpenSSLView materializes the static OpenSSL archives from the CAS
// (T1596 / #28).
func resolveOpenSSLView(arch string) (string, error) {
	return resolveTargetDepView("openssl", "openssl-view", arch, opensslFiles,
		"promise (materializing OpenSSL)",
		"Waiting for another process to finish staging OpenSSL...",
		embeddedTargetDep(embeddedOpenSSL, "resources/openssl/"+arch, hasEmbeddedOpenSSL))
}

// resolveCompilerRTView materializes the compiler-rt builtins archive from the
// CAS (T1676).
func resolveCompilerRTView(arch string) (string, error) {
	return resolveTargetDepView("compiler-rt", "compiler-rt-view", arch, compilerRTFiles,
		"promise (materializing compiler-rt builtins)",
		"Waiting for another process to finish staging the compiler-rt builtins...",
		embeddedTargetDep(embeddedCompilerRT, "resources/compiler-rt/"+arch, hasEmbeddedCompilerRT))
}

// ── WASM test runtimes (T2169) ──────────────────────────────────────────────
//
// wasmtime and node EXECUTE a compiled wasm module; they contribute nothing to
// it. They were nevertheless the last inputs this compiler took from the host's
// PATH, which meant `promise test --target wasm32-web` on a user's own project
// required a global Node install — against the zero-dependency mandate — and
// meant a suite's result could turn on which version somebody happened to have.
//
// They are now pinned dependencies like everything else, resolved by the same
// source list findLLVMTool uses and documented alongside it in
// docs/runtime-architecture.md §"WASM Runtime Sources". They are NOT embedded
// in any variant: Node alone is ~120 MB unpacked and only a run that actually
// targets wasm wants one, which docs/distribution.md classes as fetched on
// demand.

// wasmRuntimeEnvVars names the per-runtime override variable, the explicit
// bring-up/air-gap path. Same vocabulary and same meaning as llvmToolEnvVars:
// setting one is announced on every run, and a gate refuses to measure under it.
var wasmRuntimeEnvVars = map[string]string{
	"wasmtime": "PROMISE_WASMTIME",
	"node":     "PROMISE_NODE",
}

// runtimeManifestName is the runtime-manifest logical name for a WASM test
// runtime, e.g. "node" → "runtime-node".
//
// Unlike targetDepManifestName there is no arch or file component: these are
// HOST dependencies, so one host manifest describes exactly one of each, and
// the file name is this host's own spelling of it (runtimeExeName). Keep in
// lockstep with RuntimeManifestName in tools/build/common — separate Go
// modules, so the format is duplicated by necessity (pinned by
// TestRuntimeManifestName on both sides).
func runtimeManifestName(dep string) string { return "runtime-" + dep }

// runtimeExeName is a runtime's file name on this host ("node" / "node.exe").
func runtimeExeName(dep string) string { return runtimeExeNameFor(runtime.GOOS, dep) }

// runtimeExeNameFor is runtimeExeName with the host named rather than read from
// the process, so the Windows branch is asserted on every platform the suite
// runs on rather than only where Windows exists (docs/code-style.md §"Native
// and cross targets in tests"). Mirrors isHostTargetFor's split.
func runtimeExeNameFor(goos, dep string) string {
	if goos == "windows" {
		return dep + ".exe"
	}
	return dep
}

// resolveWasmRuntime returns the path to a pinned WASM test runtime, taking it
// from exactly three places — the same source list, in the same order and for
// the same reasons, as findLLVMTool:
//
//  1. the explicit `$PROMISE_WASMTIME` / `$PROMISE_NODE` override,
//  2. the host-stable prebuilts cache `bin/build` / `bin/gate` populate, so a
//     machine that has already staged the runtime never downloads it twice,
//  3. the pinned view, materialized from the content-addressed store.
//
// PATH is never consulted. When none of the three answers, the error names the
// pinned runtime and the override rather than suggesting an install — since
// this compiler no longer looks at PATH, installing one would change nothing.
func resolveWasmRuntime(dep string) (string, error) {
	envVar, ok := wasmRuntimeEnvVars[dep]
	if !ok {
		return "", fmt.Errorf("%q is not a WASM test runtime", dep)
	}
	if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
		announceToolchainOverride(envVar, v)
		return v, nil
	}
	file := runtimeExeName(dep)
	if p := prebuiltRuntimePath(dep, file); p != "" {
		return p, nil
	}
	viewDir, err := resolveRuntimeView(dep, file)
	if err != nil {
		return "", fmt.Errorf("could not obtain the pinned %s: %w\n"+
			"  Set %s to use a copy already on this machine", dep, err, envVar)
	}
	if viewDir == "" {
		return "", fmt.Errorf("this build carries no pinned %s for %s-%s\n"+
			"  Set %s to name one, or use a compiler built from a tree whose blobs.json has it",
			dep, runtime.GOOS, runtime.GOARCH, envVar)
	}
	return filepath.Join(viewDir, file), nil
}

// resolveWasmRuntimeIfLocal is resolveWasmRuntime without the fetch: it answers
// where the runtime is on THIS machine right now, and ("", nil) when obtaining
// it would need the network.
//
// `promise doctor` reports state and must not acquire tens of megabytes to do
// it, so it asks this. Same source order as resolveWasmRuntime — override,
// prebuilts cache, an already-published view — minus the one step that can go
// to the wire, which is exactly resolveLLVMView's allowFetch=false split.
func resolveWasmRuntimeIfLocal(dep string) (string, error) {
	envVar, ok := wasmRuntimeEnvVars[dep]
	if !ok {
		return "", fmt.Errorf("%q is not a WASM test runtime", dep)
	}
	if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
		return v, nil
	}
	file := runtimeExeName(dep)
	if p := prebuiltRuntimePath(dep, file); p != "" {
		return p, nil
	}
	view, err := depViewDir("runtime-view", runtime.GOOS+"-"+runtime.GOARCH,
		[]string{file}, func(string) string { return runtimeManifestName(dep) })
	if err != nil || view.Dir == "" {
		return "", err
	}
	if !depFilesPresent(view.Dir, []string{file}) {
		return "", nil // pinned, but not staged here yet
	}
	return filepath.Join(view.Dir, file), nil
}

// resolveRuntimeView materializes one WASM test runtime from the CAS into
// cache/runtime-view/<host>-<blobSetKey>/. Returns ("", nil) when the embedded
// manifest carries no entry for it, exactly as resolveTargetDepView does, so
// the caller can say which runtime this build has no pin for.
//
// `local` is nil: unlike the musl CRT and the compiler-rt builtins, this binary
// embeds no copy to skip a download with — that is the whole point of keeping
// the runtimes out of every variant.
func resolveRuntimeView(dep, file string) (string, error) {
	return resolveDepView("runtime-view", runtime.GOOS+"-"+runtime.GOARCH, []string{file},
		func(string) string { return runtimeManifestName(dep) },
		materializeRuntimeFile, viewToolPerm,
		"promise (materializing the "+dep+" runtime)",
		"Waiting for another process to finish staging the "+dep+" runtime...",
		nil)
}

// prebuiltRuntimePath returns a WASM runtime's path in the host-stable
// prebuilts cache, or "" when it is not staged there. The findPrebuiltTool
// analogue for runtimes: a machine that has run `bin/gate wasm-test` or
// `bin/prereqs -wasm` already holds the pinned bytes outside promise home, and
// downloading them a second time into the CAS would be pure waste.
//
// Pinned, not discovered: `bin/pin-prebuilts` verified the upstream archive's
// sha256 before `tools.ok` was written, so this is a local copy of the pinned
// release rather than a host find.
func prebuiltRuntimePath(dep, file string) string {
	root := prebuiltsCacheRoot()
	if root == "" {
		return ""
	}
	target := runtime.GOOS + "-" + runtime.GOARCH
	// Any cached version dir for this target; prefer the newest when several
	// checkouts pinned different ones.
	matches, _ := filepath.Glob(filepath.Join(root, dep+"-slim", "*", target, file))
	upstream, _ := filepath.Glob(filepath.Join(root, dep, "*", target, file))
	best, bestVer := "", ""
	for _, p := range append(matches, upstream...) {
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() || fi.Size() == 0 {
			continue
		}
		// tools.ok is written only after the fetch fully extracted + verified the
		// dir, so its presence rejects a half-populated cache.
		if _, err := os.Stat(filepath.Join(filepath.Dir(p), "tools.ok")); err != nil {
			continue
		}
		ver := filepath.Base(filepath.Dir(filepath.Dir(p)))
		if best == "" || compareLLVMVersion(ver, bestVer) > 0 {
			best, bestVer = p, ver
		}
	}
	return best
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
