package common

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// llvm_slim.go is the build-tool counterpart of compiler/internal/blobstore's
// runtime resolver. It lets `bin/build` (and CI) obtain LLVM (opt/llc/lld, plus
// the build-only llvm-dlltool, T0833) from the per-blob brotli-11
// `deps-llvm-<version>` GitHub release — the same blobs shipped consumers fetch
// — instead of requiring a system-installed LLVM. Unlike the client-facing
// tools, llvm-dlltool stays on the build host only (it regenerates the Windows
// winlink import libs at build time; clients never invoke it). The
// cache layout mirrors PrebuiltsCacheRoot's flat per-target dir but lives under
// `<cacheRoot>/llvm-slim/<version>/<target>/` so it never collides with the
// existing `<cacheRoot>/llvm/<version>/<target>/` archive-extract cache.
//
// EnsureLLVMBlobs is the single entry point. It resolves the host's pinned LLVM
// against `tools/build/blobs.json` and falls back to the upstream tarball
// (via FetchPrebuilt) when the catalog has no entry for the host — so a
// maintainer can land this change before backfilling every (dep, target) cell.

// slimPlanFile is one entry in EnsureLLVMBlobs's per-target plan: an extracted
// output name plus the catalog metadata that pinpoints its hosted blob.
type slimPlanFile struct {
	Out string
	BE  *BlobEntry
}

// EnsureLLVMBlobs returns a host-stable per-target directory whose flat
// contents are `opt`/`llc`/`lld` (plus the build-only `llvm-dlltool`, T0833)
// per prebuilts.toml `out` names for the given target. The build host fetches
// every prebuilts.toml file — including build-only tools — into the slim cache;
// only the client embed/manifest paths exclude them (TargetEntry.ClientFiles).
// Uses the catalog's brotli-11 slim blobs when present; falls
// back to the upstream tarball (~700 MB) when blobs.json has no entry for the
// host, printing a one-line `note:` so a maintainer can backfill via
// `bin/release publish-blobs`.
//
// Safe for concurrent invocation: a per-target flock at <cacheDir>/.lock
// serializes mutators and `tools.ok` is content-addressed so the fast path is
// lock-free for cache hits.
//
// On darwin, fetched Mach-O binaries get install_name_tool + codesign applied
// in-place (§5.1): the CAS stays raw upstream bytes (deterministic hash), the
// loadable copy in the cache is patched.
func EnsureLLVMBlobs(root, target string) (string, error) {
	pm, err := LoadPrebuiltsManifest(root)
	if err != nil {
		return "", fmt.Errorf("load prebuilts manifest: %w", err)
	}
	entry := pm.Binaries["llvm"]
	if entry == nil {
		return "", fmt.Errorf("prebuilts manifest: missing [binaries.llvm]")
	}
	tEntry := entry.Targets[target]
	if tEntry == nil {
		return "", fmt.Errorf("prebuilts manifest: no [binaries.llvm.targets.%s]", target)
	}
	if tEntry.Unsupported != "" {
		return "", fmt.Errorf("target %s is not supported: %s", target, tEntry.Unsupported)
	}

	catalog, err := LoadBlobsCatalog(root)
	if err != nil {
		return "", err
	}

	// Build per-file plan against the catalog. A single missing file is treated
	// as a complete miss — half the toolchain from blobs and half from upstream
	// would race brotli decompression against tarball extraction and produce a
	// confusing partial cache.
	plan := make([]slimPlanFile, 0, len(tEntry.Files))
	for _, f := range tEntry.Files {
		be, ok := catalog.Lookup("llvm", entry.Version, target, f.Out)
		if !ok {
			fmt.Printf("note: no slim blob hosted for llvm/%s/%s/%s — falling back to upstream tarball (~700 MB). Publish via `bin/release publish-blobs --dependency llvm --host %s`.\n",
				entry.Version, target, f.Out, target)
			return FetchPrebuilt(pm, "llvm", target)
		}
		plan = append(plan, slimPlanFile{Out: f.Out, BE: be})
	}

	cacheRoot, err := PrebuiltsCacheRoot()
	if err != nil {
		return "", err
	}
	cacheDir := filepath.Join(cacheRoot, "llvm-slim", entry.Version, target)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}

	digest := slimToolsDigest(entry.Version, target, plan)

	// Fast path — lock-free cache hit.
	if cachedSlimDigestOK(cacheDir, digest, plan) {
		return cacheDir, nil
	}

	unlock, err := acquireCacheLock(cacheDir, fmt.Sprintf("llvm-slim/%s/%s fetch", entry.Version, target))
	if err != nil {
		return "", err
	}
	defer unlock()

	if cachedSlimDigestOK(cacheDir, digest, plan) {
		return cacheDir, nil
	}

	// Wipe tools.ok early so a crash mid-fetch can't leave a half-populated
	// cache that looks valid against `digest`.
	_ = os.Remove(filepath.Join(cacheDir, toolsOKFile))

	tag := DepsReleaseTag("llvm", entry.Version)
	suffix, err := assetSuffix(compressionBrotli)
	if err != nil {
		return "", err
	}
	fetcher := defaultBlobFetcher
	for _, sf := range plan {
		assetName := sf.BE.SHA256 + suffix
		brPath := filepath.Join(cacheDir, assetName)
		outPath := filepath.Join(cacheDir, sf.Out)

		if err := fetcher.FetchAsset(tag, assetName, brPath); err != nil {
			return "", fmt.Errorf("fetch %s (%s): %w", sf.Out, assetName, err)
		}
		if err := decompressAndVerify(brPath, outPath, sf.BE.Compression, sf.BE.SHA256); err != nil {
			_ = os.Remove(brPath)
			return "", fmt.Errorf("decompress %s: %w", sf.Out, err)
		}
		_ = os.Remove(brPath)
		if runtime.GOOS == "darwin" && strings.HasPrefix(target, "darwin-") {
			patchAndSignMachO(outPath)
		}
	}

	if err := os.WriteFile(filepath.Join(cacheDir, toolsOKFile), []byte(digest+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", toolsOKFile, err)
	}
	return cacheDir, nil
}

// SlimLLVMCacheDir returns the host-stable slim-cache directory for the pinned
// LLVM version + target WITHOUT fetching anything. ok is false when the
// prebuilts manifest can't be read or has no [binaries.llvm] entry. The
// directory may not exist yet (nothing fetched) — callers that need a tool
// present must Exists-check it. Mirrors the cacheDir layout in EnsureLLVMBlobs.
func SlimLLVMCacheDir(root, target string) (string, bool) {
	pm, err := LoadPrebuiltsManifest(root)
	if err != nil || pm.Binaries["llvm"] == nil {
		return "", false
	}
	cacheRoot, err := PrebuiltsCacheRoot()
	if err != nil {
		return "", false
	}
	return filepath.Join(cacheRoot, "llvm-slim", pm.Binaries["llvm"].Version, target), true
}

// slimToolsDigest is the cache identity for one (version, target, [(out, sha,
// size, compression), ...]) tuple. Mirrors manifestToolsDigest's role for the
// upstream-tarball cache.
func slimToolsDigest(version, target string, plan []slimPlanFile) string {
	h := sha256.New()
	io.WriteString(h, "llvm-slim\n")
	io.WriteString(h, version)
	io.WriteString(h, "\n")
	io.WriteString(h, target)
	io.WriteString(h, "\n")
	for _, sf := range plan {
		io.WriteString(h, sf.Out)
		io.WriteString(h, " sha=")
		io.WriteString(h, strings.ToLower(strings.TrimSpace(sf.BE.SHA256)))
		io.WriteString(h, fmt.Sprintf(" size=%d", sf.BE.Size))
		io.WriteString(h, " codec=")
		io.WriteString(h, sf.BE.Compression)
		io.WriteString(h, "\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// cachedSlimDigestOK is true when tools.ok matches digest AND every planned
// output file is present on disk. Used by both the fast (lock-free) path and
// the post-lock re-check.
func cachedSlimDigestOK(cacheDir, digest string, plan []slimPlanFile) bool {
	got, err := os.ReadFile(filepath.Join(cacheDir, toolsOKFile))
	if err != nil {
		return false
	}
	if strings.TrimSpace(string(got)) != digest {
		return false
	}
	for _, sf := range plan {
		if !Exists(filepath.Join(cacheDir, sf.Out)) {
			return false
		}
	}
	return true
}
