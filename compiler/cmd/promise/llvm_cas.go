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

	if !allowFetch {
		// No-fetch probe: only usable if every blob is already cached.
		for _, e := range entries {
			if !store.Has(e.SHA256) {
				return "", nil
			}
		}
	}

	if err := os.MkdirAll(viewDir, 0o755); err != nil {
		return "", err
	}

	resolver := blobstore.NewResolver(store, m)
	defer resolver.Close()
	for _, e := range entries {
		var blobPath string
		if store.Has(e.SHA256) {
			blobPath = store.BlobPath(e.SHA256)
		} else {
			p, rerr := resolver.Resolve(e.Name)
			if rerr != nil {
				return "", rerr // surface offline / broken-release error
			}
			blobPath = p
		}
		dst := filepath.Join(viewDir, strings.TrimPrefix(e.Name, llvmEntryPrefix))
		if err := materializeViewFile(blobPath, dst); err != nil {
			return "", err
		}
	}

	if err := makeLLDAliases(viewDir); err != nil {
		return "", err
	}

	llvmViewDir = viewDir
	return viewDir, nil
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
	if muslCRTComplete(viewDir) {
		return viewDir, nil
	}
	store, err := blobstore.NewStore()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(viewDir, 0o755); err != nil {
		return "", err
	}
	resolver := blobstore.NewResolver(store, m)
	defer resolver.Close()
	for _, f := range muslCRTFiles {
		entry, _ := m.Lookup("musl-" + f)
		var blobPath string
		if store.Has(entry.SHA256) {
			blobPath = store.BlobPath(entry.SHA256)
		} else {
			p, rerr := resolver.Resolve("musl-" + f)
			if rerr != nil {
				return "", rerr
			}
			blobPath = p
		}
		dst := filepath.Join(viewDir, f)
		os.Remove(dst)
		copyFile(blobPath, dst, 0o644)
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
