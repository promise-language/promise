package common

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// release_build.go implements `bin/release build` — the thin/full compiler build
// with the embedded manifest, plus the Promise-stub compile-and-embed step
// (T0773 §2 steps 4–6).
//
// The build is THREE phases so the published artifact carries the stub:
//
//  1. Phase A (bootstrap): build the compiler with the manifest embedded (and
//     embed_llvm for full) but NO embedded stub.
//  2. Phase B (stub): run that bootstrap compiler to compile tools/stub/main.pr
//     for the host into resources/stub/<target>/, the source the embed_stub build
//     tag pulls in.
//  3. Phase C (final): rebuild with -tags=embed_stub (+embed_llvm for full) so the
//     final binary embeds the stub for install-time extraction.
//
// The manifest's hashes must exist before the binary is finalized (distribution.md
// §4.4) — that is why `bin/release blobs`+`manifest` run first and their output is
// passed in via --manifest.

// runReleaseBuild builds the thin or full compiler variant.
func runReleaseBuild(root string, args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	variant := fs.String("variant", "", "thin|full (required)")
	manifestPath := fs.String("manifest", "", "embedded runtime manifest path (required)")
	out := fs.String("out", "", "output binary path (required)")
	blobsDir := fs.String("blobs", "", "host blobs dir (required for full)")
	host := fs.String("host", CurrentBuildTarget(), "target (host-only for now; T0524)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *variant != "thin" && *variant != "full" {
		return fmt.Errorf("build: --variant must be thin or full\n%s", releaseUsage)
	}
	if *manifestPath == "" || *out == "" {
		return fmt.Errorf("build: --manifest and --out are required\n%s", releaseUsage)
	}
	full := *variant == "full"
	if full && *blobsDir == "" {
		return fmt.Errorf("build: --variant full requires --blobs\n%s", releaseUsage)
	}
	if *host != CurrentBuildTarget() {
		// Cross-building the compiler + stub for another target is gated on
		// cross-compilation (T0524); the `all` variant is deferred (T0774).
		return fmt.Errorf("build: cross-target builds (%s on host %s) are not supported yet (T0524)", *host, CurrentBuildTarget())
	}

	// Standard prelude (mirrors RunBuild steps 2–4).
	fmt.Println("Checking parser...")
	if err := GenerateParser(root, false); err != nil {
		return fmt.Errorf("generate parser: %w", err)
	}
	fmt.Println("Embedding resources...")
	if err := EmbedResources(root); err != nil {
		return fmt.Errorf("embed resources: %w", err)
	}
	if IsLinux() {
		fmt.Println("Embedding musl CRT...")
		if err := EmbedMuslCRT(root); err != nil {
			return fmt.Errorf("musl CRT: %w", err)
		}
	}

	// Embed the prebuilt manifest, overwriting EmbedResources' empty placeholder.
	resManifest := filepath.Join(root, "compiler", "cmd", "promise", "resources", "manifest.json")
	if err := copyFile(*manifestPath, resManifest); err != nil {
		return fmt.Errorf("embed manifest: %w", err)
	}

	// full: pre-stage the host LLVM blobs into the embed dir so the final binary
	// stages them into the CAS at install time (offline host workflow).
	if full {
		if err := bundleReleaseLLVM(root, *host, *blobsDir); err != nil {
			return err
		}
	}

	version, err := BuildVersion(root, true)
	if err != nil {
		return fmt.Errorf("version: %w", err)
	}

	// Phase A — bootstrap compiler (no embedded stub).
	bootstrap := filepath.Join(root, "bin", "promise.release-bootstrap"+ExeSuffix())
	defer os.Remove(bootstrap)
	var bootTags []string
	if full {
		bootTags = []string{"embed_llvm"}
	}
	fmt.Println("Building bootstrap compiler (phase A)...")
	if err := goBuildCompiler(root, bootstrap, version, bootTags); err != nil {
		return fmt.Errorf("phase A go build: %w", err)
	}

	// Phase B — compile the Promise stub for the host with the bootstrap compiler.
	stubDir := filepath.Join(root, "compiler", "cmd", "promise", "resources", "stub", *host)
	if err := os.MkdirAll(stubDir, 0o755); err != nil {
		return err
	}
	stubOut := filepath.Join(stubDir, BinaryName())
	stubSrc := filepath.Join("tools", "stub", "main.pr")
	fmt.Println("Compiling Promise stub with the bootstrap compiler (phase B)...")
	if err := RunIn(root, bootstrap, "build", "-release", stubSrc, "-o", stubOut); err != nil {
		return fmt.Errorf("phase B stub build: %w", err)
	}

	// Phase C — final compiler with the stub (and LLVM, if full) embedded.
	finalTags := []string{"embed_stub"}
	if full {
		finalTags = append(finalTags, "embed_llvm")
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	fmt.Printf("Building final %s compiler (phase C)...\n", *variant)
	if err := goBuildCompiler(root, *out, version, finalTags); err != nil {
		return fmt.Errorf("phase C go build: %w", err)
	}

	// Write the sha256 sidecar next to the artifact (release publishing checksum).
	hash, err := binarySHA256(*out)
	if err != nil {
		return fmt.Errorf("hash binary: %w", err)
	}
	if err := os.WriteFile(*out+".sha256", []byte(hash+"\n"), 0o644); err != nil {
		return fmt.Errorf("write sha256 sidecar: %w", err)
	}

	if info, err := os.Stat(*out); err == nil {
		fmt.Printf("Built %s variant %s (%.1f MB)\n", *variant, *out, float64(info.Size())/(1024*1024))
	}
	return nil
}

// bundleReleaseLLVM gzips the collected host LLVM blobs into the embed dir
// (resources/llvm/<target>/) for the full variant. blobsDir is the flat
// `out`-named directory produced by `bin/release blobs` — the same layout
// BundleFromCache consumes.
func bundleReleaseLLVM(root, target, blobsDir string) error {
	pm, tEntry, err := llvmTargetEntry(root, target)
	if err != nil {
		return err
	}
	llvmEntry := pm.Binaries["llvm"]
	dst := filepath.Join(root, llvmEntry.BundleDir, target)
	fmt.Printf("Bundling LLVM blobs for full variant (%s)...\n", target)
	return BundleFromCache(blobsDir, dst, tEntry.Files)
}

// goBuildCompiler runs `go build` for ./cmd/promise with the given build tags and
// version ldflag, writing the binary to outBin. Shared by all release build
// phases (mirrors RunBuild step 8).
func goBuildCompiler(root, outBin, version string, tags []string) error {
	compilerDir := filepath.Join(root, "compiler")
	buildArgs := []string{"build", "-buildvcs=false"}
	if len(tags) > 0 {
		buildArgs = append(buildArgs, "-tags="+strings.Join(tags, ","))
	}
	buildArgs = append(buildArgs, "-ldflags", "-X main.version="+version, "-o", outBin, "./cmd/promise")
	return RunIn(compilerDir, "go", buildArgs...)
}
