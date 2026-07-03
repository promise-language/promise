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

// runReleaseBuild builds the thin or full compiler variant. Thin arg-parser
// around buildReleaseVariant (which both this CLI and `bin/release
// publish-install` call).
func runReleaseBuild(root string, args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	variant := fs.String("variant", "", "thin|full (required)")
	manifestPath := fs.String("manifest", "", "embedded runtime manifest path (required)")
	out := fs.String("out", "", "output binary path (required)")
	blobsDir := fs.String("blobs", "", "host blobs dir (required for full)")
	host := fs.String("host", CurrentBuildTarget(), "target (host-only for now; T0524)")
	tag := fs.String("release-tag", "", "release tag (epoch-<Y.N>); stamps the version from the tag rather than catalog.toml, T1195")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return buildReleaseVariant(root, *variant, *manifestPath, *out, *blobsDir, *host, *tag)
}

// buildReleaseVariant builds the thin|full compiler variant with the runtime
// manifest embedded (and host LLVM blobs pre-staged for full), writing the
// finished binary + sha256 sidecar to `out`. Validation lives here so both the
// `bin/release build` CLI and `publish-install` get identical guarantees.
func buildReleaseVariant(root, variant, manifestPath, out, blobsDir, host, tag string) error {
	if variant != "thin" && variant != "full" {
		return fmt.Errorf("build: --variant must be thin or full\n%s", releaseUsage)
	}
	if manifestPath == "" || out == "" {
		return fmt.Errorf("build: --manifest and --out are required\n%s", releaseUsage)
	}
	full := variant == "full"
	if full && blobsDir == "" {
		return fmt.Errorf("build: --variant full requires --blobs\n%s", releaseUsage)
	}
	if host != CurrentBuildTarget() {
		// Cross-building the compiler + stub for another target is gated on
		// cross-compilation (T0524); the `all` variant is deferred (T0774).
		return fmt.Errorf("build: cross-target builds (%s on host %s) are not supported yet (T0524)", host, CurrentBuildTarget())
	}

	// Resolve --out to an absolute path. goBuildCompiler runs `go build` with
	// cwd=<root>/compiler, so a RELATIVE -o (e.g. release.yml's `dist/bin/...`)
	// would land under compiler/ while MkdirAll + the sha256 hash resolve it from
	// the process CWD — the phase-C "open dist/bin/...: no such file" mismatch.
	outBin, err := filepath.Abs(out)
	if err != nil {
		return fmt.Errorf("resolve --out path: %w", err)
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
	if err := copyFile(manifestPath, resManifest); err != nil {
		return fmt.Errorf("embed manifest: %w", err)
	}

	// full: pre-stage the host LLVM blobs into the embed dir so the final binary
	// stages them into the CAS at install time (offline host workflow).
	if full {
		if err := bundleReleaseLLVM(root, host, blobsDir, manifestPath); err != nil {
			return err
		}
	}

	version, err := ReleaseVersion(root, tag)
	if err != nil {
		return fmt.Errorf("version: %w", err)
	}
	// Stamp the build commit so the install gate can pin test sources to the
	// exact sources this binary was built from (T0854).
	commit := GitSHAFull(root)

	// Phase A — bootstrap compiler (no embedded stub).
	bootstrap := filepath.Join(root, "bin", "promise.release-bootstrap"+ExeSuffix())
	defer os.Remove(bootstrap)
	var bootTags []string
	if full {
		bootTags = []string{"embed_llvm"}
	}
	fmt.Println("Building bootstrap compiler (phase A)...")
	if err := goBuildCompiler(root, bootstrap, version, commit, bootTags); err != nil {
		return fmt.Errorf("phase A go build: %w", err)
	}

	// Phase B — compile the Promise stub for the host with the bootstrap compiler.
	stubDir := filepath.Join(root, "compiler", "cmd", "promise", "resources", "stub", host)
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
	if err := os.MkdirAll(filepath.Dir(outBin), 0o755); err != nil {
		return err
	}
	fmt.Printf("Building final %s compiler (phase C)...\n", variant)
	if err := goBuildCompiler(root, outBin, version, commit, finalTags); err != nil {
		return fmt.Errorf("phase C go build: %w", err)
	}

	// Write the sha256 sidecar next to the artifact (release publishing checksum).
	hash, err := binarySHA256(outBin)
	if err != nil {
		return fmt.Errorf("hash binary: %w", err)
	}
	if err := os.WriteFile(outBin+".sha256", []byte(hash+"\n"), 0o644); err != nil {
		return fmt.Errorf("write sha256 sidecar: %w", err)
	}

	if info, err := os.Stat(out); err == nil {
		fmt.Printf("Built %s variant %s (%.1f MB)\n", variant, out, float64(info.Size())/(1024*1024))
	}
	return nil
}

// bundleReleaseLLVM stages the already-brotli-compressed host LLVM blobs into
// the embed dir (resources/llvm/<target>/) for the full variant, byte-identical
// to the dist CAS asset (T0807). blobsDir is the keep-compressed fetch output
// (holds the <sha>.br alongside the decompressed copy); manifestPath provides
// the out→<sha>.br mapping.
func bundleReleaseLLVM(root, target, blobsDir, manifestPath string) error {
	pm, tEntry, err := llvmTargetEntry(root, target)
	if err != nil {
		return err
	}
	llvmEntry := pm.Binaries["llvm"]
	dst := filepath.Join(root, llvmEntry.BundleDir, target)
	fmt.Printf("Bundling LLVM blobs for full variant (%s)...\n", target)
	// ClientFiles() excludes build-only tools — they are absent from the client
	// runtime manifest, so a build-only entry here would fail the manifest
	// lookup inside BundleBrotliFromManifest (T0833).
	return BundleBrotliFromManifest(manifestPath, blobsDir, dst, tEntry.ClientFiles())
}

// goBuildCompiler runs `go build` for ./cmd/promise with the given build tags,
// version, and build-commit ldflags, writing the binary to outBin. Shared by all
// release build phases (mirrors RunBuild step 8).
func goBuildCompiler(root, outBin, version, commit string, tags []string) error {
	compilerDir := filepath.Join(root, "compiler")
	buildArgs := []string{"build", "-buildvcs=false"}
	if len(tags) > 0 {
		buildArgs = append(buildArgs, "-tags="+strings.Join(tags, ","))
	}
	buildArgs = append(buildArgs, "-ldflags", "-X main.version="+version+" -X main.commit="+commit, "-o", outBin, "./cmd/promise")
	return RunIn(compilerDir, "go", buildArgs...)
}
