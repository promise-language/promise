package common

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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

	// 5. Verify LLVM
	fmt.Println("Detecting LLVM...")
	llvm, err := FindLLVM()
	if err != nil {
		return err
	}
	fmt.Printf("  LLVM %d: opt=%s lld=%s\n", llvm.Version, llvm.OptPath, llvm.LLDPath)

	// 6. Release: bundle LLVM tools (Linux/macOS only — Windows has no embed support yet)
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

	// 7. Compute version
	version, err := BuildVersion(root, release)
	if err != nil {
		return fmt.Errorf("version: %w", err)
	}
	ldflags := "-X main.version=" + version

	// 8. Build
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

	// 9. Write hash sidecar
	hash, err := binarySHA256(binaryPath)
	if err != nil {
		return fmt.Errorf("hash binary: %w", err)
	}
	hashFile := filepath.Join(binDir, ".promise.hash")
	if err := os.WriteFile(hashFile, []byte(hash+"\n"), 0o644); err != nil {
		return fmt.Errorf("write hash: %w", err)
	}

	elapsed := time.Since(start).Round(time.Millisecond)
	if info, err := os.Stat(binaryPath); err == nil {
		size := float64(info.Size()) / (1024 * 1024)
		fmt.Printf("Built %s (%.1f MB) in %s\n", BinaryName(), size, elapsed)
	} else {
		fmt.Printf("Built %s in %s\n", BinaryName(), elapsed)
	}

	return nil
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
