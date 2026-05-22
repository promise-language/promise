package common

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// BundleLLVM gzips the manifest's `out` files into the embed dir for release
// builds. cacheDir is the per-target prebuilts cache directory populated by
// FetchPrebuilt; it holds each `out` file flat with its runtime name.
//
// Returns nil with no work done when cacheDir is empty (the optional-binary,
// nothing-fetched short-circuit returned by FetchAll for missing targets).
func BundleLLVM(root string, manifest *PrebuiltsManifest, cacheDir string) error {
	if cacheDir == "" {
		return nil
	}
	target := CurrentBuildTarget()
	llvmEntry := manifest.Binaries["llvm"]
	if llvmEntry == nil {
		return fmt.Errorf("prebuilts manifest: missing [binaries.llvm] entry")
	}
	tEntry := llvmEntry.Targets[target]
	if tEntry == nil {
		return fmt.Errorf("prebuilts manifest: no [binaries.llvm.targets.%s] entry", target)
	}
	if tEntry.Unsupported != "" {
		return fmt.Errorf("LLVM bundling is not supported on %s: %s", target, tEntry.Unsupported)
	}
	dst := filepath.Join(root, llvmEntry.BundleDir, target)

	fmt.Printf("Bundling LLVM %s (%s) from %s...\n", llvmEntry.Version, target, cacheDir)
	return BundleFromCache(cacheDir, dst, tEntry.Files)
}

// singleTopLevelDir returns the unique top-level subdirectory of root, or
// root itself if the tarball didn't pack everything under a wrapper.
//
// Heuristic: only treat the single dir as a wrapper when ALL top-level entries
// are directories AND there's exactly one. If any top-level file exists, the
// tarball was packed flat and we should use root directly. (LLVM-22.1.0-* and
// similar tarballs always have a wrapper; test tarballs typically don't.)
func singleTopLevelDir(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	var dirs []string
	hasFile := false
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		} else {
			hasFile = true
		}
	}
	if len(dirs) == 1 && !hasFile {
		return filepath.Join(root, dirs[0]), nil
	}
	return root, nil
}

func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	gz, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		return err
	}
	if _, err := io.Copy(gz, in); err != nil {
		gz.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return out.Close()
}

func printSize(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	name := filepath.Base(path)
	size := float64(info.Size()) / (1024 * 1024)
	fmt.Printf("  %s: %.1f MB\n", name, size)
}
