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
	Archive       string `json:"archive,omitempty"`
	ArchivePath   string `json:"archive_path,omitempty"`
	ArchiveSHA256 string `json:"archive_sha256,omitempty"`
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

	// Kind marks macOS LLVM blobs (patched + ad-hoc re-signed on materialize,
	// §5.1); other targets need no such handling. The runtime view builder
	// dispatches on its own host GOOS, so Kind is documentary — but keep it
	// honest per-target so the embedded manifest reflects reality.
	kind := "blob"
	if strings.HasPrefix(target, "darwin-") {
		kind = "macho-llvm"
	}

	m := runtimeManifest{Schema: runtimeManifestSchema, Epoch: epoch}
	for _, f := range tEntry.Files {
		blobPath := filepath.Join(llvmCacheDir, f.Out)
		hash, size, err := hashAndSize(blobPath)
		if err != nil {
			return fmt.Errorf("hash %s: %w", blobPath, err)
		}
		m.Entries = append(m.Entries, runtimeManifestEntry{
			Name:   "llvm-" + f.Out,
			SHA256: hash,
			Size:   size,
			Kind:   kind,
			Sources: []runtimeSource{{
				Archive:       tEntry.URL,
				ArchivePath:   f.Src,
				ArchiveSHA256: tEntry.SHA256,
			}},
		})
	}

	data, err := json.MarshalIndent(&m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	out := filepath.Join(root, "compiler", "cmd", "promise", "resources", "manifest.json")
	return os.WriteFile(out, data, 0o644)
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
