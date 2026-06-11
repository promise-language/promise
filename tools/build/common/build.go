package common

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// RunBuild executes the full compiler build pipeline.
// This is the main implementation — called by bin/build and internally
// by other tools (e.g., verify, test) without spawning a subprocess.
// Flags: -release, -generate, -shared (use ~/.promise), -local (default, no-op).
func RunBuild(root string, args []string) error {
	start := time.Now()
	args = NormalizeArgs(args)
	release := slices.Contains(args, "-release")
	generate := slices.Contains(args, "-generate")

	for _, arg := range args {
		switch arg {
		case "-release", "-generate", "-local", "-shared":
		default:
			return fmt.Errorf("usage: bin/build [-release] [-generate] [-shared]")
		}
	}

	// Default to local cache when called as CLI (args != nil).
	// When called internally by verify/test (args == nil), caller handles cache.
	if args != nil {
		shared := slices.Contains(args, "-shared")
		if !shared {
			if err := SetupLocalCache(root); err != nil {
				return fmt.Errorf("setup local cache: %w", err)
			}
		}
	}

	compilerDir := filepath.Join(root, "compiler")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}

	// 1. Quick up-to-date check (skip for --release/--generate)
	// Git hooks are configured by ./make as part of bootstrap, not here.
	if !release && !generate {
		if version, err := BuildVersion(root, false); err == nil {
			if isBinaryUpToDate(root, binDir, version) {
				binaryPath := filepath.Join(binDir, BinaryName())
				if info, err := os.Stat(binaryPath); err == nil {
					size := float64(info.Size()) / (1024 * 1024)
					fmt.Printf("%s up to date (%.1f MB, version: %s)\n", BinaryName(), size, version)
				}
				return nil
			}
		}
	}

	// 2. Generate parser (skip if up to date, unless --generate is passed)
	fmt.Println("Checking parser...")
	if err := GenerateParser(root, generate); err != nil {
		return fmt.Errorf("generate parser: %w", err)
	}

	// 3. Embed resources
	fmt.Println("Embedding resources...")
	if err := EmbedResources(root); err != nil {
		return fmt.Errorf("embed resources: %w", err)
	}

	// 4. musl CRT (Linux only)
	if IsLinux() {
		fmt.Println("Embedding musl CRT...")
		if err := EmbedMuslCRT(root); err != nil {
			return fmt.Errorf("musl CRT: %w", err)
		}
	}

	// 5. Project per-host runtime dependency manifest from blobs.json (T0798).
	// Done for BOTH debug and release builds — without it, a debug binary on a
	// machine with no system LLVM would have no way to self-fetch at runtime
	// (the resolver's last-resort step needs manifest entries). A catalog miss
	// for the host (blobs not yet published) is non-fatal: EmbedResources's
	// placeholder is left in place so the build still completes.
	target := CurrentBuildTarget()
	prebuiltsManifest, perr := LoadPrebuiltsManifest(root)
	if perr != nil {
		return fmt.Errorf("load prebuilts manifest: %w", perr)
	}
	epoch, eerr := ParseEpoch(root)
	if eerr != nil {
		return fmt.Errorf("parse epoch for runtime manifest: %w", eerr)
	}
	manifestFromCatalog := false
	{
		m, err := BuildRuntimeManifestFromCatalog(root, target, epoch)
		switch {
		case err == nil:
			out := filepath.Join(root, "compiler", "cmd", "promise", "resources", "manifest.json")
			if werr := writeRuntimeManifest(out, m); werr != nil {
				return fmt.Errorf("write runtime manifest: %w", werr)
			}
			manifestFromCatalog = true
		case errors.Is(err, errCatalogMissForHost):
			fmt.Printf("  blobs.json has no entry for %s — runtime manifest stays empty (placeholder); set up via `bin/release publish-blobs --dependency llvm --host %s`\n",
				target, target)
		default:
			return fmt.Errorf("project runtime manifest from catalog: %w", err)
		}
	}

	// 6. Verify LLVM (host scan still used by debug runtime; T0520 will retire this)
	fmt.Println("Detecting LLVM...")
	llvm, err := FindLLVM(root)
	if err != nil {
		if !release {
			return err
		}
		// Release builds don't need a host LLVM — they fetch the pinned
		// upstream tarball / slim blobs into the prebuilts cache. Continue.
		fmt.Printf("  no host LLVM found (%v); release build will use prebuilts\n", err)
	} else {
		fmt.Printf("  LLVM %d: opt=%s lld=%s\n", llvm.Version, llvm.OptPath, llvm.LLDPath)
	}

	// 7. Release: fetch + bundle LLVM tools. Prefer the slim brotli blobs
	// (~3-4× smaller than the upstream tarball, T0798); fall back to the full
	// tarball when blobs.json has no entry for the host.
	buildTags := ""
	if release {
		llvmCacheDir, lerr := EnsureLLVMBlobs(root, target)
		if lerr != nil {
			return fmt.Errorf("fetch LLVM blobs: %w", lerr)
		}
		fmt.Println("Bundling LLVM tools for release...")
		if err := BundleLLVM(root, prebuiltsManifest, llvmCacheDir); err != nil {
			return fmt.Errorf("bundle LLVM: %w", err)
		}
		// When the catalog projection above didn't run (host has no blobs yet),
		// fall back to the bootstrap producer so a release build still ships a
		// usable manifest. The catalog path is preferred because it embeds the
		// brotli-blob source URL (smaller download for users).
		if !manifestFromCatalog {
			fmt.Println("Generating runtime dependency manifest (bootstrap, no catalog)...")
			if err := GenerateRuntimeManifest(root, prebuiltsManifest, llvmCacheDir, target, epoch); err != nil {
				return fmt.Errorf("generate runtime manifest: %w", err)
			}
		}
		buildTags = "-tags=embed_llvm"
	}

	// 8. Compute version
	version, err := BuildVersion(root, release)
	if err != nil {
		return fmt.Errorf("version: %w", err)
	}
	ldflags := "-X main.version=" + version + " -X main.commit=" + GitSHAFull(root)

	// 9. Build
	binaryPath := filepath.Join(binDir, BinaryName())
	fmt.Printf("Building %s (version: %s)...\n", BinaryName(), version)

	buildArgs := []string{"build", "-buildvcs=false"}
	if buildTags != "" {
		buildArgs = append(buildArgs, buildTags)
	}
	buildArgs = append(buildArgs, "-ldflags", ldflags, "-o", binaryPath, "./cmd/promise")

	if err := RunIn(compilerDir, "go", buildArgs...); err != nil {
		return fmt.Errorf("go build: %w", err)
	}

	// 10. Write hash sidecar
	hash, err := binarySHA256(binaryPath)
	if err != nil {
		return fmt.Errorf("hash binary: %w", err)
	}
	hashFile := filepath.Join(binDir, ".promise.hash")
	if err := os.WriteFile(hashFile, []byte(hash+"\n"), 0o644); err != nil {
		return fmt.Errorf("write hash: %w", err)
	}

	// 11. Write buildinfo for up-to-date check
	infoFile := filepath.Join(binDir, ".promise.buildinfo")
	os.WriteFile(infoFile, []byte(version+"\n"), 0o644)

	// 12. Invalidate gate values — compiler changed, prior verify results are stale
	InvalidateGateValues(root)

	elapsed := time.Since(start).Round(time.Millisecond)
	if info, err := os.Stat(binaryPath); err == nil {
		size := float64(info.Size()) / (1024 * 1024)
		fmt.Printf("Built %s (%.1f MB) in %s\n", BinaryName(), size, elapsed)
	} else {
		fmt.Printf("Built %s in %s\n", BinaryName(), elapsed)
	}

	return nil
}

// isBinaryUpToDate returns true if the compiler binary exists, was built with
// the given version, and no source file has been modified since the binary was
// built. This lets RunBuild skip the entire pipeline when nothing has changed.
func isBinaryUpToDate(root, binDir, version string) bool {
	binaryPath := filepath.Join(binDir, BinaryName())
	binaryInfo, err := os.Stat(binaryPath)
	if err != nil {
		return false
	}

	// Check stored version matches (catches new commits)
	infoFile := filepath.Join(binDir, ".promise.buildinfo")
	stored, err := os.ReadFile(infoFile)
	if err != nil || strings.TrimSpace(string(stored)) != version {
		return false
	}

	binaryMtime := binaryInfo.ModTime()

	// Check compiler Go sources (excluding generated resource copies)
	compilerDir := filepath.Join(root, "compiler")
	skipDirs := map[string]bool{
		filepath.Join(compilerDir, "cmd", "promise", "resources"):      true,
		filepath.Join(compilerDir, "internal", "testutil", "testdata"): true,
	}
	if anySourceNewer(compilerDir, skipDirs, binaryMtime) {
		return false
	}

	// Check module and example sources
	for _, dir := range []string{
		filepath.Join(root, "modules"),
		filepath.Join(root, "examples"),
	} {
		if anySourceNewer(dir, nil, binaryMtime) {
			return false
		}
	}

	// Check individual config files
	for _, f := range []string{
		filepath.Join(root, "catalog.toml"),
		filepath.Join(root, "docs", "language-guide.md"),
	} {
		if info, err := os.Stat(f); err == nil && info.ModTime().After(binaryMtime) {
			return false
		}
	}

	return true
}

// anySourceNewer returns true if any file under dir (excluding skipDirs) has
// a modification time after the given threshold.
func anySourceNewer(dir string, skipDirs map[string]bool, than time.Time) bool {
	if !Exists(dir) {
		return false
	}
	found := false
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return filepath.SkipDir
		}
		if d.IsDir() && skipDirs[path] {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(than) {
			found = true
			return filepath.SkipDir
		}
		return nil
	})
	return found
}

func binarySHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
