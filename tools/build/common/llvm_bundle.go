package common

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// BundleLLVM gzips LLVM tools into the resources directory for release builds.
// This is the local-tools path: it reads its file list from prebuilts.toml
// (so it stays in lock-step with --fetch) but treats `llvm.Dir` as the
// "extracted root" instead of a downloaded tarball.
func BundleLLVM(root string, llvm *LLVMInfo) error {
	if !IsDarwin() && !IsLinux() && !IsWindows() {
		fmt.Println("WARNING: LLVM bundling not supported on " + GoOS())
		return nil
	}
	if llvm.Dir == "" {
		return fmt.Errorf("LLVM directory not found for bundling")
	}

	manifest, err := LoadPrebuiltsManifest(root)
	if err != nil {
		return fmt.Errorf("load prebuilts manifest: %w", err)
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

	extractedRoot, err := localLLVMExtractedRoot(llvm)
	if err != nil {
		return err
	}

	fmt.Printf("Bundling LLVM %d tools (%s) from %s...\n", llvm.Version, target, extractedRoot)

	if err := BundleFromExtracted(extractedRoot, dst, tEntry.Files); err != nil {
		return fmt.Errorf("bundle LLVM: %w", err)
	}
	return nil
}

// localLLVMExtractedRoot returns a synthetic "extracted root" for the local
// LLVM install — a directory whose layout matches the official tarball's
// (bin/{opt,llc,lld}, lib/libLLVM.*, lib/libz3.*, lib/libzstd.*) so the same
// manifest file ops apply.
//
// On Linux/Windows this is just `llvm.Dir`. On macOS it stitches together the
// Homebrew llvm keg with the separate kegs that hold lld and libLLVM's runtime
// dylib dependencies (z3, zstd) into a single staging tree under PROMISE_HOME.
func localLLVMExtractedRoot(llvm *LLVMInfo) (string, error) {
	if !IsDarwin() {
		return llvm.Dir, nil
	}

	// On macOS, libz3 and libzstd live in their own Homebrew kegs (not the
	// llvm@22 keg). Resolve each keg the manifest requires; missing kegs are
	// a hard error so the build fails loudly with a clear message instead of
	// producing an incomplete bundle.
	overlayLibs := []string{}
	for _, keg := range []string{"lld", "z3", "zstd"} {
		dir, err := resolveBrewKeg(keg)
		if err != nil {
			return "", err
		}
		overlayLibs = append(overlayLibs, dir)
	}

	// Stable staging path keyed on the LLVM dir basename so repeated builds
	// don't accumulate staging dirs.
	stageRoot, err := stageDir("llvm-" + filepath.Base(llvm.Dir))
	if err != nil {
		return "", err
	}
	if err := mirrorStagingTree(stageRoot, llvm.Dir, overlayLibs); err != nil {
		return "", err
	}
	return stageRoot, nil
}

// resolveBrewKeg locates a Homebrew keg by name. Searches /opt/homebrew/opt/<name>
// (Apple Silicon) then /usr/local/opt/<name> (x86_64). Returns the keg's root
// (which contains lib/ and possibly bin/).
func resolveBrewKeg(name string) (string, error) {
	for _, prefix := range []string{"/opt/homebrew/opt", "/usr/local/opt"} {
		path := filepath.Join(prefix, name)
		if Exists(path) {
			return path, nil
		}
	}
	return "", fmt.Errorf("Homebrew keg %q not found in /opt/homebrew/opt or /usr/local/opt — install with `brew install %s`", name, name)
}

// stageDir returns a stable scratch directory under $PROMISE_HOME/cache/build-stage/<name>.
// Falls back to ~/.promise/cache/build-stage/<name>.
func stageDir(name string) (string, error) {
	root, err := buildStageRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func buildStageRoot() (string, error) {
	if h := os.Getenv("PROMISE_HOME"); h != "" {
		return filepath.Join(h, "cache", "build-stage"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".promise", "cache", "build-stage"), nil
}

// mirrorTreeWithLLD populates stageRoot with bin/ and lib/ symlinks from the
// LLVM install, overlaying bin/ and lib/ from each additional Homebrew keg.
// This lets the manifest see a unified layout (e.g. `bin/lld` and
// `lib/liblld*.dylib` even though Homebrew packages lld separately, plus
// `lib/libz3.*.dylib` from the z3 keg, `lib/libzstd.*.dylib` from zstd, etc).
func mirrorStagingTree(stageRoot, llvmDir string, overlays []string) error {
	for _, sub := range []string{"bin", "lib"} {
		dst := filepath.Join(stageRoot, sub)
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
	}
	if err := mirrorDir(filepath.Join(llvmDir, "bin"), filepath.Join(stageRoot, "bin")); err != nil {
		return err
	}
	if err := mirrorDir(filepath.Join(llvmDir, "lib"), filepath.Join(stageRoot, "lib")); err != nil {
		return err
	}
	for _, overlay := range overlays {
		if err := overlayDir(filepath.Join(overlay, "bin"), filepath.Join(stageRoot, "bin")); err != nil {
			return err
		}
		if err := overlayDir(filepath.Join(overlay, "lib"), filepath.Join(stageRoot, "lib")); err != nil {
			return err
		}
	}
	return nil
}

func mirrorDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		// Missing src is OK — overlayDir/manifest will surface the actual problem.
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		_ = os.Remove(d)
		if err := os.Symlink(s, d); err != nil {
			return err
		}
	}
	return nil
}

func overlayDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		d := filepath.Join(dst, e.Name())
		if Exists(d) {
			continue
		}
		s := filepath.Join(src, e.Name())
		if err := os.Symlink(s, d); err != nil {
			return err
		}
	}
	return nil
}

// BundleLLVMFromFetched is the --fetch counterpart to BundleLLVM. FetchPrebuilt
// has already populated cacheDir with the manifest's `out` files (flat layout);
// this function just gzips them into the embed dir.
// Returns nil if no LLVM was fetched (all targets optional + missing).
func BundleLLVMFromFetched(root string, manifest *PrebuiltsManifest, cacheDir string) error {
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

	fmt.Printf("Bundling fetched LLVM %s (%s) from %s...\n", llvmEntry.Version, target, cacheDir)
	return BundleFromCache(cacheDir, dst, tEntry.Files)
}

// singleTopLevelDir returns the unique top-level subdirectory of root,
// or root itself if there isn't exactly one.
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
