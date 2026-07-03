package common

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// release_publish_install.go implements `bin/release publish-install` — the
// TEMPORARY private-repo staging helper for the end-to-end install gate (T0803).
// It builds the host's thin + full compiler variants, gzips them to the
// published asset names, computes a (multi-host-merge-aware) SHA256SUMS over the
// `.gz` assets, and uploads the assets + the install scripts to an R2 bucket
// under a `dist/` prefix (the same wrangler mechanism `publish-blobs` uses).
//
// This is a manual TESTING/staging path, not part of release validation: the
// install gate now installs straight from GitHub releases (T0804). To test a
// staged bucket, install from it with `PROMISE_BASE_URL=<mirror>/dist install.sh`
// (the install scripts honor that override; see scripts/install.sh).

// installDistPrefix is the R2 key prefix the dist assets live under. Installing
// from the staged bucket points the install scripts' PROMISE_BASE_URL at it.
const installDistPrefix = "dist"

// installAssetName returns the published gzip asset name for a target+variant,
// matching ASSET_NAME in scripts/install.sh / install.ps1 EXACTLY (the install
// scripts and the gate verify against this name, so any drift breaks the
// checksum lookup). variant is "" (thin) or "full". Windows assets carry a
// `.exe` before the `.gz`.
func installAssetName(target, variant string) string {
	goos, arch, _ := strings.Cut(target, "-")
	suffix := ""
	if variant == "full" {
		suffix = "-full"
	}
	name := "promise-" + goos + "-" + arch + suffix
	if goos == "windows" {
		name += ".exe"
	}
	return name + ".gz"
}

// runReleasePublishInstall implements `bin/release publish-install`. Host-only
// (cross-build is gated on T0524); the maintainer runs it once per platform,
// staging into a shared --out so SHA256SUMS accumulates all hosts' assets.
func runReleasePublishInstall(root string, args []string) error {
	fs := flag.NewFlagSet("publish-install", flag.ContinueOnError)
	host := fs.String("host", CurrentBuildTarget(), "target to build+publish install assets for (host-only; T0524)")
	out := fs.String("out", "", "staging dir for dist artifacts (default <root>/dist)")
	r2Bucket := fs.String("r2-bucket", "prebuilts", "Cloudflare R2 bucket to upload the dist/ assets to via `npx wrangler` (empty string disables upload)")
	dryRun := fs.Bool("dry-run", false, "build + stage assets but do not upload")
	noUpload := fs.Bool("no-upload", false, "build + stage assets but skip the R2 upload (testing without wrangler)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *host != CurrentBuildTarget() {
		return fmt.Errorf("publish-install: cross-target staging (%s on host %s) is not supported yet (T0524)", *host, CurrentBuildTarget())
	}

	// The published binary is stamped with HEAD's commit (T0854) so the install
	// gate can pin its test sources to the exact sources the binary was built
	// from. Refuse a dirty tree so that recorded SHA unambiguously matches the
	// built bytes.
	if status, _ := RunOutputIn(root, "git", "status", "--porcelain"); strings.TrimSpace(status) != "" {
		return fmt.Errorf("publish-install: working tree is dirty; commit or stash so the stamped build commit matches the published bytes")
	}

	outDir := *out
	if outDir == "" {
		outDir = filepath.Join(root, installDistPrefix)
	}
	binDir := filepath.Join(outDir, "bin")
	blobsDir := filepath.Join(outDir, "blobs")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}

	// When we will upload, seed the local SHA256SUMS from the bucket's current one
	// FIRST — before the (expensive) build — so (a) a host where wrangler is not
	// authenticated fails fast WITH login guidance instead of after minutes of
	// compiling, and (b) the later writeInstallSums merge PRESERVES other hosts'
	// entries (e.g. darwin's, staged from another machine) rather than clobbering
	// them when this host uploads its SHA256SUMS.
	willUpload := !*dryRun && !*noUpload && *r2Bucket != ""
	// When actually publishing, HEAD must be on origin/main and not ahead of it:
	// the install gate on another host fetches the binary's stamped build commit
	// from origin to pin its test sources to what the binary was built from
	// (T0854). A commit that is ahead of — or off — origin/main is unfetchable
	// there, so the gate could never set up. Checked before the (expensive) build
	// and the SHA256SUMS round-trip so it fails fast. Skipped for
	// --dry-run/--no-upload local builds, which publish nothing.
	if willUpload {
		if err := requireHeadOnOriginMain(root); err != nil {
			return err
		}
	}
	var mirror blobMirror
	if willUpload {
		mirror = newBlobMirror(*r2Bucket)
		sumsKey := installDistPrefix + "/SHA256SUMS"
		found, err := mirror.Get(sumsKey, filepath.Join(outDir, "SHA256SUMS"))
		if err != nil {
			return fmt.Errorf("seed SHA256SUMS from R2 (is wrangler authenticated on this host?): %w", err)
		}
		if found {
			fmt.Printf("Seeded SHA256SUMS from R2 %s/%s (other hosts' entries preserved)\n", *r2Bucket, sumsKey)
		} else {
			fmt.Printf("No existing SHA256SUMS in R2 %s/%s — starting fresh\n", *r2Bucket, sumsKey)
		}
	}

	epoch, err := ParseEpoch(root)
	if err != nil {
		return err
	}

	// 1 — project the runtime manifest for the host from the blobs catalog.
	manifest, err := BuildRuntimeManifestFromCatalog(root, *host, epoch)
	if err != nil {
		return fmt.Errorf("build manifest: %w", err)
	}
	manifestPath := filepath.Join(outDir, "manifest-"+*host+".json")
	if err := writeRuntimeManifest(manifestPath, manifest); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	// 2 — build the thin variant.
	thinBin := filepath.Join(binDir, "promise-"+*host+ExeSuffix())
	fmt.Printf("Building thin variant for %s...\n", *host)
	if err := buildReleaseVariant(root, "thin", manifestPath, thinBin, "", *host, ""); err != nil {
		return fmt.Errorf("build thin: %w", err)
	}

	// 3 — fetch the host blobs (needed to pre-stage the offline full variant).
	//     keepCompressed=true so the already-brotli-compressed <sha>.br blobs
	//     survive for bundleReleaseLLVM to embed directly (T0807) — no gzip
	//     recompress round trip.
	fmt.Printf("Fetching host LLVM blobs for the full variant...\n")
	if err := fetchManifestBlobs(manifestPath, blobsDir, true); err != nil {
		return fmt.Errorf("fetch blobs: %w", err)
	}

	// 4 — build the full variant (host blobs pre-staged).
	fullBin := filepath.Join(binDir, "promise-"+*host+"-full"+ExeSuffix())
	fmt.Printf("Building full variant for %s...\n", *host)
	if err := buildReleaseVariant(root, "full", manifestPath, fullBin, blobsDir, *host, ""); err != nil {
		return fmt.Errorf("build full: %w", err)
	}

	// 5 — gzip each binary to its published asset name.
	thinAsset := filepath.Join(outDir, installAssetName(*host, ""))
	fullAsset := filepath.Join(outDir, installAssetName(*host, "full"))
	for _, g := range []struct{ src, dst string }{{thinBin, thinAsset}, {fullBin, fullAsset}} {
		fmt.Printf("Compressing %s → %s...\n", filepath.Base(g.src), filepath.Base(g.dst))
		if err := gzipFile(g.src, g.dst); err != nil {
			return fmt.Errorf("gzip %s: %w", filepath.Base(g.src), err)
		}
		printSize(g.dst)
	}

	// 6 — compute SHA256SUMS over the .gz assets. Merges this host's two assets
	//     into the (possibly bucket-seeded) SHA256SUMS so every platform's sums
	//     coexist in one file.
	if err := writeInstallSums(outDir, []string{thinAsset, fullAsset}); err != nil {
		return fmt.Errorf("write SHA256SUMS: %w", err)
	}
	sumsPath := filepath.Join(outDir, "SHA256SUMS")

	// 7 — upload the assets + SHA256SUMS + install scripts to the dist bucket.
	if !willUpload {
		fmt.Printf("\npublish-install staged %s assets in %s (upload skipped)\n", *host, outDir)
		return nil
	}
	// Stage the install scripts into outDir before uploading, normalizing line
	// endings so every publishing host emits byte-identical artifacts regardless
	// of its core.autocrlf setting or .gitattributes checkout (T0820). install.sh
	// is forced to LF: POSIX `sh` chokes on a trailing `\r` ("set: -<CR>: invalid
	// option"), so a CRLF copy breaks every `curl … | sh` user. install.ps1 and
	// install.cmd are forced to CRLF: they are Windows scripts (cmd.exe in
	// particular is sensitive to bare-LF line endings), and forcing CRLF means a
	// Linux host (LF working tree) and a Windows host publish the same bytes.
	stagedSh := filepath.Join(outDir, "install.sh")
	if err := copyInstallScriptLF(filepath.Join(root, "scripts", "install.sh"), stagedSh); err != nil {
		return fmt.Errorf("stage install.sh: %w", err)
	}
	stagedPs1 := filepath.Join(outDir, "install.ps1")
	if err := copyInstallScriptCRLF(filepath.Join(root, "scripts", "install.ps1"), stagedPs1); err != nil {
		return fmt.Errorf("stage install.ps1: %w", err)
	}
	stagedCmd := filepath.Join(outDir, "install.cmd")
	if err := copyInstallScriptCRLF(filepath.Join(root, "scripts", "install.cmd"), stagedCmd); err != nil {
		return fmt.Errorf("stage install.cmd: %w", err)
	}
	uploads := []string{thinAsset, fullAsset, sumsPath, stagedSh, stagedPs1, stagedCmd}
	for _, p := range uploads {
		key := installDistPrefix + "/" + filepath.Base(p)
		fmt.Printf("Uploading %s → R2 %s/%s...\n", filepath.Base(p), *r2Bucket, key)
		if err := mirror.Put(key, p); err != nil {
			return fmt.Errorf("upload %s: %w", key, err)
		}
	}
	fmt.Printf("\npublish-install: uploaded %d dist objects for %s to bucket %s\n", len(uploads), *host, *r2Bucket)
	return nil
}

// requireHeadOnOriginMain fails unless local HEAD is contained in origin/main —
// i.e. pushed and not ahead of it. The install gate (running on a different
// host) fetches the binary's stamped build commit from origin to pin its test
// sources (T0854); a commit that is ahead of, or off, origin/main is unfetchable
// there. It fetches origin/main first so the ancestry check reflects the
// remote's real tip rather than a stale local tracking ref.
func requireHeadOnOriginMain(root string) error {
	if _, err := RunOutputIn(root, "git", "fetch", "--quiet", "origin", "main"); err != nil {
		return fmt.Errorf("publish-install: git fetch origin main (needed to verify HEAD is published): %w", err)
	}
	head, err := RunOutputIn(root, "git", "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("publish-install: git rev-parse HEAD: %w", err)
	}
	// `merge-base --is-ancestor` exits 0 when HEAD is reachable from origin/main
	// (on-branch and not ahead), non-zero otherwise.
	if err := RunSilent("git", "-C", root, "merge-base", "--is-ancestor", "HEAD", "origin/main"); err != nil {
		return fmt.Errorf("publish-install: HEAD (%s) is not on origin/main — push to main before publishing (the install gate fetches the stamped build commit from origin/main to build its test worktree)", head)
	}
	return nil
}

// copyInstallScriptLF copies src to dst stripping carriage returns (CRLF→LF;
// bare `\n` is left untouched). install.sh is published verbatim and run by
// POSIX `sh`, which rejects a trailing `\r` on `set -eu` etc. — see T0820. A
// belt-and-suspenders guard so a host with core.autocrlf=true (Windows) can
// never publish a CRLF install.sh even if .gitattributes was bypassed.
func copyInstallScriptLF(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	lf := strings.ReplaceAll(string(data), "\r", "")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, []byte(lf), 0o644)
}

// copyInstallScriptCRLF copies src to dst normalizing every line ending to CRLF
// (T0820). It first strips all `\r` (collapsing CRLF→LF) then rewrites each `\n`
// as `\r\n`, so the result is the same whether the source was checked out LF
// (Linux host) or CRLF (Windows host) — publish-install reads the working tree,
// and install.ps1/install.cmd are Windows scripts that want CRLF. Idempotent:
// applying it to already-CRLF input yields identical bytes.
func copyInstallScriptCRLF(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	lf := strings.ReplaceAll(string(data), "\r", "")
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, []byte(crlf), 0o644)
}

// writeInstallSums computes the sha256 of each .gz asset in assetPaths and
// writes/merges them into <dir>/SHA256SUMS (sha256sum format: "<sha>␣␣<name>").
// Existing lines for assets NOT in this set are PRESERVED — each host runs
// publish-install separately, so staging all three into one --out accumulates
// every platform's sums in a single file (install.sh's exact-match `awk
// '$2==name'` reads any line). Lines for the assets in this set are replaced.
func writeInstallSums(dir string, assetPaths []string) error {
	sumsPath := filepath.Join(dir, "SHA256SUMS")
	sums := map[string]string{} // asset name → sha256
	if data, err := os.ReadFile(sumsPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				sums[fields[len(fields)-1]] = fields[0]
			}
		}
	}
	for _, p := range assetPaths {
		h, _, err := hashAndSize(p)
		if err != nil {
			return err
		}
		sums[filepath.Base(p)] = h
	}
	names := make([]string, 0, len(sums))
	for n := range sums {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic ordering
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s  %s\n", sums[n], n)
	}
	return os.WriteFile(sumsPath, []byte(b.String()), 0o644)
}
