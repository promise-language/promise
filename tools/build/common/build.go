package common

import (
	"crypto/sha256"
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
func RunBuild(root string, args []string) error {
	start := time.Now()
	release := slices.Contains(args, "--release")
	generate := slices.Contains(args, "--generate")

	compilerDir := filepath.Join(root, "compiler")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}

	// 1. Git hooks
	RunSetup(root)

	// 2. Quick up-to-date check (skip for --release/--generate)
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

	// 3. Generate parser (skip if up to date, unless --generate is passed)
	fmt.Println("Checking parser...")
	if err := GenerateParser(root, generate); err != nil {
		return fmt.Errorf("generate parser: %w", err)
	}

	// 4. Embed resources
	fmt.Println("Embedding resources...")
	if err := EmbedResources(root); err != nil {
		return fmt.Errorf("embed resources: %w", err)
	}

	// 5. musl CRT (Linux only)
	if IsLinux() {
		fmt.Println("Embedding musl CRT...")
		if err := EmbedMuslCRT(root); err != nil {
			return fmt.Errorf("musl CRT: %w", err)
		}
	}

	// 6. Verify LLVM
	fmt.Println("Detecting LLVM...")
	llvm, err := FindLLVM()
	if err != nil {
		return err
	}
	fmt.Printf("  LLVM %d: opt=%s lld=%s\n", llvm.Version, llvm.OptPath, llvm.LLDPath)

	// 7. Release: bundle LLVM tools (Linux/macOS only — Windows has no embed support yet)
	buildTags := ""
	if release {
		if IsWindows() {
			return fmt.Errorf("--release builds are not supported on Windows (no LLVM embedding support)")
		}
		fmt.Println("Bundling LLVM tools for release...")
		if err := BundleLLVM(root, llvm); err != nil {
			return fmt.Errorf("bundle LLVM: %w", err)
		}
		buildTags = "-tags=embed_llvm"
	}

	// 8. Compute version
	version, err := BuildVersion(root, release)
	if err != nil {
		return fmt.Errorf("version: %w", err)
	}
	ldflags := "-X main.version=" + version

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
