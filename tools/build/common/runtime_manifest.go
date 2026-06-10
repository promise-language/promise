package common

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// runtimeManifestSchema must match blobstore.ManifestSchema in the compiler
// (compiler/internal/blobstore/manifest.go). The two live in separate Go
// modules, so the value is duplicated by necessity; bump both together.
const runtimeManifestSchema = 1

// runtimeManifest mirrors blobstore.Manifest's JSON shape (the consumed
// contract; T0773 supersedes this bootstrap producer).
type runtimeManifest struct {
	Schema  int                    `json:"schema"`
	Epoch   string                 `json:"epoch"`
	Entries []runtimeManifestEntry `json:"entries"`
}

type runtimeManifestEntry struct {
	Name    string          `json:"name"`
	SHA256  string          `json:"sha256"`
	Size    int64           `json:"size"`
	Kind    string          `json:"kind"`
	Sources []runtimeSource `json:"sources"`
}

type runtimeSource struct {
	Blob          string `json:"blob,omitempty"`
	Compression   string `json:"compression,omitempty"` // transport codec of the Blob asset ("" / "brotli")
	Archive       string `json:"archive,omitempty"`
	ArchivePath   string `json:"archive_path,omitempty"`
	ArchiveSHA256 string `json:"archive_sha256,omitempty"`
}

// compressionBrotli is the only transport codec emitted today (brotli-11 per
// blob; docs/release-automation.md §3). The content sha256 is always over the
// UNCOMPRESSED bytes — compression is purely a transport layer. This must stay
// in lockstep with blobstore.compressionBrotli in the compiler (separate Go
// module).
const compressionBrotli = "brotli"

// assetSuffix returns the published-asset filename suffix for a blob source's
// compression codec. The asset stem is always the uncompressed content
// <sha256>; the suffix encodes transport. An unknown codec is rejected so a
// typo fails loudly at produce/verify time rather than shipping garbage. Keep
// the map in sync with blobstore.assetSuffix in the compiler.
func assetSuffix(compression string) (string, error) {
	switch compression {
	case "", "none":
		return "", nil
	case compressionBrotli:
		return ".br", nil
	default:
		return "", fmt.Errorf("unknown compression codec %q", compression)
	}
}

// GenerateRuntimeManifest writes compiler/cmd/promise/resources/manifest.json
// for the host target from the already-fetched prebuilts (T0769 §6 bootstrap
// producer). It hashes each extracted LLVM tool (the RAW upstream bytes — macOS
// patch/sign happens at runtime on the view-dir copy, keeping the CAS hash
// deterministic, §5.1/§10) and emits one entry per tool whose single source is
// the pinned upstream LLVM tarball as an `archive` + `archive_path`.
//
// llvmCacheDir is FetchAll's returned root for "llvm" (flat `out` files). When
// it is empty (no LLVM target for this host), the placeholder manifest written
// by EmbedResources is left in place.
//
// This is the bootstrap producer used by `bin/build --release`. The full forge
// driver `bin/release` (release.go) reuses buildLLVMEntries to add a ranked
// GitHub-release-asset blob source ahead of the upstream archive.
func GenerateRuntimeManifest(root string, pm *PrebuiltsManifest, llvmCacheDir, target, epoch string) error {
	if llvmCacheDir == "" {
		return nil
	}
	llvm := pm.Binaries["llvm"]
	if llvm == nil {
		return fmt.Errorf("prebuilts manifest: missing [binaries.llvm]")
	}
	tEntry := llvm.Targets[target]
	if tEntry == nil || tEntry.Unsupported != "" {
		return nil // no buildable LLVM for this host → keep placeholder
	}

	// Bootstrap producer: upstream archive is the only source (no published
	// release-asset blob yet). nil blobSource ⇒ archive-only entries.
	entries, err := buildLLVMEntries(llvmCacheDir, tEntry, target, nil)
	if err != nil {
		return err
	}
	m := runtimeManifest{Schema: runtimeManifestSchema, Epoch: epoch, Entries: entries}
	out := filepath.Join(root, "compiler", "cmd", "promise", "resources", "manifest.json")
	return writeRuntimeManifest(out, &m)
}

// llvmKindForTarget returns the manifest Kind for LLVM blobs on target. macOS
// blobs are KindMachOLLVM (patched + ad-hoc re-signed on materialize, §5.1);
// other targets are plain blobs. The runtime view builder dispatches on its own
// host GOOS, so Kind is documentary — but kept honest per-target so the embedded
// manifest reflects reality.
func llvmKindForTarget(target string) string {
	if strings.HasPrefix(target, "darwin-") {
		return "macho-llvm"
	}
	return "blob"
}

// buildLLVMEntries hashes each client-shipped LLVM tool (tEntry.ClientFiles(),
// which excludes build-only tools like llvm-dlltool) under dir (a flat directory
// of `out`-named extracted binaries — the FetchPrebuilt cache dir or a
// `bin/release blobs` output dir) and builds one manifest entry per tool, named
// "llvm-<out>" (the logical name resolveLLVMView asks for).
//
// blobSource, when non-nil, is invoked with each tool's extracted sha256 to
// produce the primary (higher-ranked) acquisition source — the published
// release-asset blob (carrying its URL and transport compression). The pinned
// upstream LLVM tarball (`archive`+`archive_path`) is always appended as the
// fallback source, so a not-yet-published release still resolves from upstream
// (this is what lets the thin bootstrap compile the stub).
func buildLLVMEntries(dir string, tEntry *TargetEntry, target string, blobSource func(hash string) runtimeSource) ([]runtimeManifestEntry, error) {
	kind := llvmKindForTarget(target)
	var entries []runtimeManifestEntry
	// Build-only tools (e.g. llvm-dlltool) are not shipped to clients, so they
	// never appear in the client runtime manifest (T0833). The pack loop in
	// runReleaseManifest iterates ClientFiles() too, keeping indices aligned.
	for _, f := range tEntry.ClientFiles() {
		blobPath := filepath.Join(dir, f.Out)
		hash, size, err := hashAndSize(blobPath)
		if err != nil {
			return nil, fmt.Errorf("hash %s: %w", blobPath, err)
		}
		var sources []runtimeSource
		if blobSource != nil {
			sources = append(sources, blobSource(hash))
		}
		sources = append(sources, runtimeSource{
			Archive:       tEntry.URL,
			ArchivePath:   f.Src,
			ArchiveSHA256: tEntry.SHA256,
		})
		entries = append(entries, runtimeManifestEntry{
			Name:    "llvm-" + f.Out,
			SHA256:  hash,
			Size:    size,
			Kind:    kind,
			Sources: sources,
		})
	}
	return entries, nil
}

// writeRuntimeManifest marshals m as indented JSON (trailing newline) to path.
func writeRuntimeManifest(path string, m *runtimeManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func hashAndSize(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
