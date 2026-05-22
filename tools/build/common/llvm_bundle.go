package common

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

	dst := filepath.Join(root, llvmEntry.BundleDir, target)

	extractedRoot, err := localLLVMExtractedRoot(llvm)
	if err != nil {
		return err
	}

	fmt.Printf("Bundling LLVM %d tools (%s) from %s...\n", llvm.Version, target, extractedRoot)

	if err := BundleFromExtracted(extractedRoot, dst, tEntry.Files); err != nil {
		return fmt.Errorf("bundle LLVM: %w", err)
	}

	// Local-only post-step: macOS Homebrew's libLLVM.dylib has absolute
	// dyld refs to /opt/homebrew/lib/lib{c++,unwind}.1.dylib etc. The official
	// llvmorg tarball is self-contained and doesn't need this. So we run otool
	// against the local install and bundle whatever Homebrew dylibs it pulls in,
	// in addition to the manifest's explicit entries.
	if IsDarwin() {
		if err := bundleDarwinHomebrewDeps(extractedRoot, dst); err != nil {
			return err
		}
	}

	return nil
}

// localLLVMExtractedRoot returns a synthetic "extracted root" for the local
// LLVM install — a directory whose layout matches the official tarball's
// (bin/{opt,llc,lld}, lib/libLLVM.*) so the same manifest file ops apply.
//
// On Linux/Windows this is just `llvm.Dir`. On macOS it stitches together
// the Homebrew llvm + lld trees (Homebrew may package lld separately) into a
// staging directory under PROMISE_HOME.
func localLLVMExtractedRoot(llvm *LLVMInfo) (string, error) {
	if !IsDarwin() {
		return llvm.Dir, nil
	}

	// macOS path: Homebrew may put lld in a separate prefix.
	brewLLDDir := llvm.Dir
	lldBin := filepath.Join(llvm.Dir, "bin", "lld")
	if !Exists(lldBin) {
		for _, prefix := range []string{"/opt/homebrew/opt/lld", "/usr/local/opt/lld"} {
			if Exists(filepath.Join(prefix, "bin", "lld")) {
				brewLLDDir = prefix
				break
			}
		}
	}
	// If lld lives in the same tree as opt, no staging needed.
	if brewLLDDir == llvm.Dir {
		return llvm.Dir, nil
	}

	// Stage lld bin+lib alongside the LLVM tree via symlinks. Stable path
	// keyed on the LLVM dir basename so repeated builds don't accumulate
	// staging dirs.
	stageRoot, err := stageDir("llvm-" + filepath.Base(llvm.Dir))
	if err != nil {
		return "", err
	}
	if err := mirrorTreeWithLLD(stageRoot, llvm.Dir, brewLLDDir); err != nil {
		return "", err
	}
	return stageRoot, nil
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
// LLVM install, overlaying lld's bin/ and lib/ from a separate prefix. This
// lets the manifest see a unified `bin/lld` + `lib/liblld*.dylib` layout.
func mirrorTreeWithLLD(stageRoot, llvmDir, lldDir string) error {
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
	// Overlay lld bin/lib (only files not already present).
	if err := overlayDir(filepath.Join(lldDir, "bin"), filepath.Join(stageRoot, "bin")); err != nil {
		return err
	}
	if err := overlayDir(filepath.Join(lldDir, "lib"), filepath.Join(stageRoot, "lib")); err != nil {
		return err
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

// BundleLLVMFromFetched is the --fetch counterpart to BundleLLVM. It uses the
// extracted tree from the downloaded tarball instead of the local install.
// Returns nil if no LLVM was fetched (all targets optional + missing).
func BundleLLVMFromFetched(root string, manifest *PrebuiltsManifest, extractedRoot string) error {
	if extractedRoot == "" {
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
	dst := filepath.Join(root, llvmEntry.BundleDir, target)

	// LLVM tarballs typically extract into a single top-level dir like
	// "clang+llvm-22.1.0-x86_64-linux-gnu-ubuntu-22.04/". Resolve that.
	innerRoot, err := singleTopLevelDir(extractedRoot)
	if err != nil {
		return err
	}

	fmt.Printf("Bundling fetched LLVM %s (%s)...\n", llvmEntry.Version, target)
	return BundleFromExtracted(innerRoot, dst, tEntry.Files)
}

// singleTopLevelDir returns the unique top-level subdirectory of root,
// or root itself if there isn't exactly one.
func singleTopLevelDir(root string) (string, error) {
	entries, err := os.ReadDir(root)
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
		return filepath.Join(root, dirs[0]), nil
	}
	return root, nil
}

// bundleDarwinHomebrewDeps scans the extracted libLLVM.dylib for Homebrew-prefix
// dependencies and gzips them into dst. Local-only — fetched LLVM tarballs
// declare their dylibs explicitly in the manifest and don't have Homebrew refs.
func bundleDarwinHomebrewDeps(extractedRoot, dst string) error {
	libLLVM := filepath.Join(extractedRoot, "lib", "libLLVM.dylib")
	if !Exists(libLLVM) {
		return nil
	}
	deps, err := otoolDeps(libLLVM)
	if err != nil {
		// otool may not be installed in some environments — soft-fail so
		// non-darwin paths still bundle correctly.
		fmt.Fprintf(os.Stderr, "  warning: otool failed (skipping Homebrew dep scan): %v\n", err)
		return nil
	}
	for _, dep := range deps {
		name := filepath.Base(dep) + ".gz"
		out := filepath.Join(dst, name)
		if Exists(out) {
			continue
		}
		if err := gzipFile(dep, out); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: could not bundle %s: %v\n", dep, err)
			continue
		}
		printSize(out)
	}
	return nil
}

// otoolDeps returns Homebrew dependency paths from otool -L output,
// excluding libLLVM itself. Used as a darwin-local post-hook to capture
// the implicit dylib graph that Homebrew install paths produce.
func otoolDeps(dylib string) ([]string, error) {
	out, err := RunOutput("otool", "-L", dylib)
	if err != nil {
		return nil, err
	}
	var deps []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		path := parts[0]
		if (strings.HasPrefix(path, "/opt/homebrew/") || strings.HasPrefix(path, "/usr/local/")) &&
			!strings.Contains(path, "libLLVM") {
			deps = append(deps, path)
		}
	}
	return deps, nil
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
