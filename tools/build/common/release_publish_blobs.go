package common

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/andybalholm/brotli"
)

// release_publish_blobs.go implements the local-only producer + fetch paths for
// T0797's dep-bump blob workflow:
//
//   bin/release publish-blobs --dependency <dep> --host <target>
//       extract upstream archive → brotli-11 compress each file → hash →
//       record in tools/build/blobs.json → upload to deps-<dep>-<version>.
//       Idempotent: a 4-tuple already in the catalog with the same
//       (sha256,size) is left alone (no re-upload, no recompress).
//
//   bin/release fetch-blobs --manifest <m> --out <dir> [--keep-compressed]
//       download each manifest entry's PRIMARY blob source (a deps release
//       asset), brotli-decompress to <out>/<file>, verify uncompressed sha256.
//       With --keep-compressed the raw `<sha>.br` is left alongside (used by
//       the integrity gate in `release.yml`'s `publish` job, which calls
//       `verify-manifest --against` over the compressed bytes).
//
// The point: per-epoch `release.yml` no longer downloads upstream LLVM (~700 MB)
// or runs brotli-11 (~10 min/host). It projects the catalog (`manifest
// --from-catalog`) and fetches pre-hosted blobs by sha256.

// ── publish-blobs ───────────────────────────────────────────────────────────

// releaseUploader abstracts the GitHub-release side of `publish-blobs` so tests
// can substitute a stub. The default implementation shells out to `gh`.
type releaseUploader interface {
	// EnsureRelease creates the deps release if missing; idempotent. notes is
	// the release body shown on the GitHub Releases page.
	EnsureRelease(tag, title, notes string) error
	// ListAssets returns the asset filenames already attached to tag. An
	// unknown tag is signaled by a nil slice + nil error (caller will create).
	ListAssets(tag string) ([]string, error)
	// UploadAsset attaches localPath to the release at tag. Skipping an
	// already-attached same-named asset (idempotency under retries) is the
	// implementation's responsibility.
	UploadAsset(tag, localPath string) error
}

// defaultReleaseUploader is the production releaseUploader — `gh` CLI.
var defaultReleaseUploader releaseUploader = &ghCLIUploader{}

// ghCLIUploader is the production releaseUploader.
type ghCLIUploader struct{}

func (ghCLIUploader) EnsureRelease(tag, title, notes string) error {
	if err := exec.Command("gh", "release", "view", tag).Run(); err == nil {
		return nil // already exists
	}
	cmd := exec.Command("gh", "release", "create", tag, "--title", title, "--notes", notes)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh release create %s: %w", tag, err)
	}
	return nil
}

func (ghCLIUploader) ListAssets(tag string) ([]string, error) {
	out, err := exec.Command("gh", "release", "view", tag, "--json", "assets", "--jq", ".assets[].name").Output()
	if err != nil {
		// `gh release view` exits non-zero for an unknown tag — return nil
		// so the caller knows to create one before uploading.
		return nil, nil
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l := strings.TrimSpace(line); l != "" {
			names = append(names, l)
		}
	}
	return names, nil
}

func (ghCLIUploader) UploadAsset(tag, localPath string) error {
	// `--clobber=false` is the default; we want gh to refuse to overwrite an
	// existing asset (the catalog's "already hosted → skip" path handles
	// dedup before we get here).
	var errBuf bytes.Buffer
	cmd := exec.Command("gh", "release", "upload", tag, localPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &errBuf)
	if err := cmd.Run(); err != nil {
		// Content-addressed blobs: same <sha>.br name always means same bytes,
		// so an existing asset is a genuine no-op. Implement the idempotency
		// the interface contract promises (lines 51-54).
		if strings.Contains(errBuf.String(), "asset under the same name already exists") {
			return nil
		}
		return fmt.Errorf("gh release upload %s %s: %w", tag, localPath, err)
	}
	return nil
}

// blobMirror mirrors a content-addressed blob to a SECONDARY public host
// (Cloudflare R2) at the SAME key path as the GitHub release asset, so the
// runtime's PROMISE_BLOB_MIRROR override (a scheme+host swap that preserves the
// path) resolves to it with NO manifest change. This is the public backstop
// while the GitHub repo is private (T0786) — and how the whole fetch path is
// tested without GitHub direct download.
type blobMirror interface {
	// Put uploads localPath under key. Overwrite is acceptable (content-
	// addressed: a given key always maps to the same bytes).
	Put(key, localPath string) error
	// Get downloads key into localPath. found is false (with a nil error) when
	// the object does not exist in the bucket — the caller treats that as "start
	// fresh", distinct from a real (e.g. auth/network) failure.
	Get(key, localPath string) (found bool, err error)
}

// newBlobMirror builds the production R2 mirror; tests swap it for a stub.
var newBlobMirror = func(bucket string) blobMirror { return wranglerR2Mirror{bucket: bucket} }

// wranglerR2Mirror uploads to a Cloudflare R2 bucket via `npx wrangler r2 object
// put`. The maintainer authenticates wrangler once (`wrangler login`, or the
// CLOUDFLARE_API_TOKEN + CLOUDFLARE_ACCOUNT_ID env). Blobs upload as OPAQUE
// bytes (no Content-Encoding) — the resolver downloads the raw `<sha>.br` and
// brotli-decompresses in-process; an HTTP-level `br` encoding would
// double-decompress and break the uncompressed-sha256 check.
type wranglerR2Mirror struct{ bucket string }

func (m wranglerR2Mirror) Put(key, localPath string) error {
	target := m.bucket + "/" + key
	if _, err := runWranglerR2([]string{"put", target, "--file", localPath, "--remote"}); err != nil {
		return fmt.Errorf("wrangler r2 object put %s: %w", target, err)
	}
	return nil
}

func (m wranglerR2Mirror) Get(key, localPath string) (bool, error) {
	target := m.bucket + "/" + key
	stderr, err := runWranglerR2([]string{"get", target, "--file", localPath, "--remote"})
	if err != nil {
		// A missing object is an expected, non-fatal outcome (first publish for
		// this bucket) — distinguish it from auth/network failures.
		s := strings.ToLower(stderr)
		for _, m := range []string{"does not exist", "not found", "no such key", "404"} {
			if strings.Contains(s, m) {
				return false, nil
			}
		}
		return false, fmt.Errorf("wrangler r2 object get %s: %w", target, err)
	}
	return true, nil
}

// runWranglerR2 runs `npx wrangler r2 object <args...>`, forwarding wrangler's
// output to stderr and returning its captured stderr for the caller to classify.
// On an auth-shaped failure it prints actionable login/permission guidance, so a
// maintainer on a host where wrangler is not logged in gets a clear next step
// rather than a bare non-zero exit.
func runWranglerR2(args []string) (string, error) {
	var errBuf bytes.Buffer
	cmd := exec.Command("npx", append([]string{"wrangler", "r2", "object"}, args...)...)
	cmd.Stdout = os.Stderr // keep our stdout clean; wrangler progress → stderr
	cmd.Stderr = io.MultiWriter(os.Stderr, &errBuf)
	err := cmd.Run()
	if err != nil && looksLikeWranglerAuthError(errBuf.String()) {
		fmt.Fprint(os.Stderr, wranglerAuthHelp)
	}
	return errBuf.String(), err
}

// looksLikeWranglerAuthError heuristically detects a missing/invalid Cloudflare
// credential failure from wrangler's stderr, so we only print the (verbose) auth
// guidance when it is actually relevant.
func looksLikeWranglerAuthError(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, marker := range []string{
		"not authenticated",
		"cloudflare_api_token",
		"authentication error",
		"unauthorized",
		"[code: 10000]",
		"wrangler login",
		"you need to login",
		"in a non-interactive environment",
		"could not be authenticated",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// wranglerAuthHelp is printed when a wrangler R2 call fails for auth reasons.
const wranglerAuthHelp = `
wrangler could not authenticate to Cloudflare R2. To fix on this host:

  Interactive (machine with a browser):
    npx wrangler login

  Headless / CI (no browser) — set both env vars, then re-run:
    export CLOUDFLARE_API_TOKEN=<api-token>
    export CLOUDFLARE_ACCOUNT_ID=<account-id>

The API token needs R2 read+write on this account. Create one at:
  Cloudflare dashboard -> My Profile -> API Tokens -> Create Token
  Use a Custom Token granting "Workers R2 Storage: Edit" (account-scoped),
  or the "Edit Cloudflare Workers" template. Then re-run the command.
`

// publishBlobAction is the outcome row reported per file in the summary table.
type publishBlobAction string

const (
	publishActionSkipCatalog publishBlobAction = "skip-catalog" // catalog hit + asset already hosted
	publishActionUpload      publishBlobAction = "upload"       // new (or re-hosted) → uploaded
	publishActionDryRun      publishBlobAction = "dry-run"      // would upload, --dry-run
	publishActionNoUpload    publishBlobAction = "no-upload"    // catalog updated, --no-upload skipped gh
)

// runReleasePublishBlobs implements `bin/release publish-blobs`. Per-host one
// invocation: a single macOS maintainer runs the command 3× (linux/darwin/
// windows targets) to produce all platforms' blobs.
func runReleasePublishBlobs(root string, args []string) error {
	fs := flag.NewFlagSet("publish-blobs", flag.ContinueOnError)
	dep := fs.String("dependency", "llvm", "dependency name in prebuilts.toml")
	host := fs.String("host", "", "target to publish blobs for (required)")
	dryRun := fs.Bool("dry-run", false, "print what would happen, no catalog write, no upload")
	noUpload := fs.Bool("no-upload", false, "record in catalog but skip gh upload (testing without GH access)")
	r2Bucket := fs.String("r2-bucket", "prebuilts", "Cloudflare R2 bucket to also mirror each uploaded blob into via `npx wrangler`, at the SAME key path as the GitHub asset — the public backstop for PROMISE_BLOB_MIRROR while the repo is private (T0786). Empty string disables R2 mirroring.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *host == "" {
		return fmt.Errorf("publish-blobs: --host is required\n%s", releaseUsage)
	}
	pm, err := LoadPrebuiltsManifest(root)
	if err != nil {
		return fmt.Errorf("load prebuilts manifest: %w", err)
	}
	entry := pm.Binaries[*dep]
	if entry == nil {
		return fmt.Errorf("publish-blobs: dependency %q not declared in prebuilts.toml", *dep)
	}
	tEntry := entry.Targets[*host]
	if tEntry == nil {
		return fmt.Errorf("publish-blobs: %s has no target %q", *dep, *host)
	}
	if tEntry.Unsupported != "" {
		return fmt.Errorf("publish-blobs: %s/%s is unsupported: %s", *dep, *host, tEntry.Unsupported)
	}

	catalog, err := LoadBlobsCatalog(root)
	if err != nil {
		return err
	}
	version := entry.Version
	tag := DepsReleaseTag(*dep, version)

	suffix, err := assetSuffix(compressionBrotli)
	if err != nil {
		return err
	}

	uploader := defaultReleaseUploader
	var hosted map[string]bool
	if !*dryRun && !*noUpload {
		assets, err := uploader.ListAssets(tag)
		if err != nil {
			return fmt.Errorf("publish-blobs: list assets for %s: %w", tag, err)
		}
		hosted = make(map[string]bool, len(assets))
		for _, n := range assets {
			hosted[n] = true
		}
	}

	type plannedUpload struct {
		entry   BlobEntry
		action  publishBlobAction
		brPath  string
		message string
	}
	var planned []plannedUpload
	var cacheDir string

	for _, f := range tEntry.Files {
		assetName := "" // computed below per the catalog's CAS hash
		if existing, ok := catalog.Lookup(*dep, version, *host, f.Out); ok && existing.Compression == compressionBrotli {
			assetName = existing.SHA256 + suffix
			// Hosted-asset check is only meaningful when we are actually
			// going to talk to gh; in dry-run/no-upload modes we trust the
			// catalog hit alone.
			hostedAlready := *dryRun || *noUpload || hosted[assetName]
			if hostedAlready {
				planned = append(planned, plannedUpload{
					entry:   *existing,
					action:  publishActionSkipCatalog,
					message: fmt.Sprintf("catalog hit + asset present (%s)", assetName),
				})
				continue
			}
		}

		// Need to (re-)produce the blob. Lazily fetch the prebuilt cache.
		if cacheDir == "" {
			fmt.Printf("Fetching upstream %s archive for %s...\n", *dep, *host)
			c, err := FetchPrebuilt(pm, *dep, *host)
			if err != nil {
				return fmt.Errorf("fetch prebuilt %s/%s: %w", *dep, *host, err)
			}
			cacheDir = c
		}

		extracted := filepath.Join(cacheDir, f.Out)
		uncompressedSHA, size, err := hashAndSize(extracted)
		if err != nil {
			return fmt.Errorf("hash extracted %s: %w", extracted, err)
		}
		// Write the compressed artifact under its CONTENT-ADDRESSED name
		// (`<sha>.br`) so the upload basename matches the manifest's blob
		// asset URL. Reuses the cache dir so re-runs hit the same path.
		brPath := filepath.Join(cacheDir, uncompressedSHA+suffix)
		if !Exists(brPath) {
			fmt.Printf("Compressing %s with brotli-11...\n", f.Out)
			if err := compressFileBrotli(extracted, brPath); err != nil {
				return fmt.Errorf("compress %s: %w", f.Out, err)
			}
		}
		compressedSHA, compressedSize, err := hashAndSize(brPath)
		if err != nil {
			return fmt.Errorf("hash compressed %s: %w", brPath, err)
		}

		be := BlobEntry{
			Dependency:       *dep,
			Version:          version,
			Target:           *host,
			Name:             f.Out,
			SHA256:           uncompressedSHA,
			Size:             size,
			Compression:      compressionBrotli,
			CompressedSize:   compressedSize,
			CompressedSHA256: compressedSHA,
			Source: &BlobSource{
				ArchiveURL:    tEntry.URL,
				ArchiveSHA256: tEntry.SHA256,
				Member:        f.Src,
			},
		}
		if err := catalog.Upsert(be); err != nil {
			return fmt.Errorf("catalog upsert %s: %w", blobIdent(be), err)
		}

		action := publishActionUpload
		switch {
		case *dryRun:
			action = publishActionDryRun
		case *noUpload:
			action = publishActionNoUpload
		}
		planned = append(planned, plannedUpload{
			entry:   be,
			action:  action,
			brPath:  brPath,
			message: fmt.Sprintf("sha=%s size=%d compressed=%d", uncompressedSHA, size, compressedSize),
		})
	}

	if !*dryRun {
		if err := WriteBlobsCatalog(root, catalog); err != nil {
			return fmt.Errorf("write blobs.json: %w", err)
		}
	}

	if !*dryRun && !*noUpload {
		anyUpload := false
		for _, p := range planned {
			if p.action == publishActionUpload {
				anyUpload = true
				break
			}
		}
		if anyUpload {
			notes := fmt.Sprintf("Content-addressed dependency blobs for %s %s. Verifiable via tools/build/blobs.json.", *dep, version)
			title := fmt.Sprintf("%s %s dependency blobs", *dep, version)
			if err := uploader.EnsureRelease(tag, title, notes); err != nil {
				return err
			}
		}
		var mirror blobMirror
		if *r2Bucket != "" {
			mirror = newBlobMirror(*r2Bucket)
		}
		for _, p := range planned {
			if p.action != publishActionUpload {
				continue
			}
			if err := uploader.UploadAsset(tag, p.brPath); err != nil {
				return err
			}
			if mirror != nil {
				key := filepath.Base(p.brPath) // flat CAS object: <sha>.br, no path
				fmt.Printf("Mirroring %s → R2 bucket %s...\n", key, *r2Bucket)
				if err := mirror.Put(key, p.brPath); err != nil {
					return err
				}
			}
		}
	}

	fmt.Printf("\npublish-blobs summary for %s/%s/%s (tag %s):\n", *dep, version, *host, tag)
	for _, p := range planned {
		fmt.Printf("  [%s] %s — %s\n", p.action, p.entry.Name, p.message)
	}
	return nil
}

// ── fetch-blobs ─────────────────────────────────────────────────────────────

// blobFetcher abstracts asset downloads so tests can substitute a stub.
// Defaults to `gh release download` so the private-repo flow works under
// GH_TOKEN; HTTP would only work once the repo is public.
type blobFetcher interface {
	// FetchAsset downloads the named asset (e.g. "deadbeef.br") from the
	// release at tag to dst. tag is empty when the URL does not encode one
	// (the caller may fall back to a direct GET).
	FetchAsset(tag, asset, dst string) error
}

// defaultBlobFetcher is the production blobFetcher — `gh release download`
// with an HTTP fallback for non-deps-release URLs.
var defaultBlobFetcher blobFetcher = &ghCLIFetcher{}

type ghCLIFetcher struct{}

func (ghCLIFetcher) FetchAsset(tag, asset, dst string) error {
	if tag != "" {
		dir := filepath.Dir(dst)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		cmd := exec.Command("gh", "release", "download", tag, "-p", asset, "-D", dir, "--clobber")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("gh release download %s %s: %w", tag, asset, err)
		}
		// `gh` writes <dir>/<asset>; rename to dst if they differ.
		downloaded := filepath.Join(dir, asset)
		if downloaded != dst {
			if err := os.Rename(downloaded, dst); err != nil {
				return err
			}
		}
		return nil
	}
	// Non-deps URL fallback: plain HTTP.
	resp, err := http.Get(asset)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, asset)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return err
	}
	return out.Close()
}

// runReleaseFetchBlobs implements `bin/release fetch-blobs`. Per-manifest one
// invocation: walks each entry's primary blob source, downloads the asset,
// brotli-decompresses to <out>/<name>, verifies sha256 against the manifest.
// The decompressed file is named after the manifest entry's logical name minus
// the "llvm-" prefix (matching the prebuilts.toml `out` value), so callers can
// pass the result dir directly to `bin/release build --blobs`.
func runReleaseFetchBlobs(root string, args []string) error {
	fs := flag.NewFlagSet("fetch-blobs", flag.ContinueOnError)
	manifestPath := fs.String("manifest", "", "manifest path (required)")
	out := fs.String("out", "", "output directory (required)")
	keepCompressed := fs.Bool("keep-compressed", false, "also keep the downloaded <sha>.br alongside the decompressed file (used by verify-manifest)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *manifestPath == "" || *out == "" {
		return fmt.Errorf("fetch-blobs: --manifest and --out are required\n%s", releaseUsage)
	}
	return fetchManifestBlobs(*manifestPath, *out, *keepCompressed)
}

// fetchManifestBlobs walks each manifest entry's primary blob source, downloads
// the asset, brotli-decompresses to <out>/<name>, and verifies sha256 against
// the manifest. Shared by the `bin/release fetch-blobs` CLI and the
// `publish-install` full-variant staging step (which needs the host blobs to
// pre-stage offline).
func fetchManifestBlobs(manifestPath, out string, keepCompressed bool) error {
	m, err := loadRuntimeManifest(manifestPath)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}

	fetcher := defaultBlobFetcher
	for _, e := range m.Entries {
		var blobSrc *runtimeSource
		for i := range e.Sources {
			if e.Sources[i].Blob != "" {
				blobSrc = &e.Sources[i]
				break
			}
		}
		if blobSrc == nil {
			return fmt.Errorf("fetch-blobs: entry %q has no blob source", e.Name)
		}
		assetName := path.Base(blobSrc.Blob)
		brPath := filepath.Join(out, assetName)
		fileName := strings.TrimPrefix(e.Name, "llvm-")
		dstPath := filepath.Join(out, fileName)

		// Cache pre-check: skip the (dominant-cost) network download when a valid
		// local copy already exists. The blobs are content-addressed and the
		// uncompressed bytes are sha256-verified against e.SHA256, so a matching
		// local copy is guaranteed byte-identical to a fresh fetch (T0844).
		//
		// The runtime manifest carries no compressed digest (the asset stem is the
		// UNCOMPRESSED sha256, see runtime_manifest.go), so the only sound no-network
		// validation of a cached .br is to decompress-and-verify it. The slim cache
		// (EnsureLLVMBlobs) can use a cheaper tools.ok sentinel because it records a
		// separate digest; we deliberately don't mirror that here.
		if cached, err := blobCacheHit(brPath, dstPath, blobSrc.Compression, e.SHA256, keepCompressed); err != nil {
			return fmt.Errorf("validate cached %s: %w", e.Name, err)
		} else if cached {
			fmt.Printf("  cached %s (%d bytes uncompressed, skipped download)\n", e.Name, e.Size)
			continue
		}

		if err := fetcher.FetchAsset(parseReleaseTagFromURL(blobSrc.Blob), assetName, brPath); err != nil {
			return fmt.Errorf("fetch %s: %w", e.Name, err)
		}

		if err := decompressAndVerify(brPath, dstPath, blobSrc.Compression, e.SHA256); err != nil {
			return fmt.Errorf("decompress %s: %w", e.Name, err)
		}
		if !keepCompressed {
			_ = os.Remove(brPath)
		}
		fmt.Printf("  fetched %s (%d bytes uncompressed)\n", e.Name, e.Size)
	}
	fmt.Printf("Fetched %d blobs into %s\n", len(m.Entries), out)
	return nil
}

// blobCacheHit reports whether the local outputs for a manifest entry are
// already present and valid, so the network download can be skipped (T0844).
//
//   - The decompressed dstPath must exist and its sha256 must equal wantSHA.
//   - When keepCompressed is set, brPath must also exist and decompress to the
//     same wantSHA (publish-install needs the <sha>.br to survive for
//     bundleReleaseLLVM/BundleBrotliFromManifest, which copies it raw).
//
// A stale/corrupt cached file (hash mismatch, undecompressable .br) is NOT a
// hit: the offending files are removed and the caller re-fetches. A genuine
// validation error (e.g. an I/O failure) is returned to abort loudly.
func blobCacheHit(brPath, dstPath, compression, wantSHA string, keepCompressed bool) (bool, error) {
	if !Exists(dstPath) {
		return false, nil
	}
	got, err := fileSHA256(dstPath)
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(got, wantSHA) {
		// Stale decompressed output — drop it and re-fetch.
		_ = os.Remove(dstPath)
		_ = os.Remove(brPath)
		return false, nil
	}
	if !keepCompressed {
		return true, nil
	}
	// keepCompressed also requires a valid <sha>.br. The manifest has no
	// compressed digest, so validate by decompress-verify (no network). This
	// regenerates dstPath as a byte-identical side effect.
	if !Exists(brPath) {
		return false, nil
	}
	if err := decompressAndVerify(brPath, dstPath, compression, wantSHA); err != nil {
		// Corrupt/stale .br — drop both outputs and re-fetch.
		_ = os.Remove(brPath)
		_ = os.Remove(dstPath)
		return false, nil
	}
	return true, nil
}

// parseReleaseTagFromURL extracts the deps release tag from a release-asset
// URL. Expected shape: `<releaseAssetBase>/<tag>/<asset>`. Returns "" if the
// URL doesn't conform (the fetcher will fall back to direct HTTP).
func parseReleaseTagFromURL(u string) string {
	if !strings.HasPrefix(u, releaseAssetBase+"/") {
		return ""
	}
	rest := strings.TrimPrefix(u, releaseAssetBase+"/")
	if i := strings.Index(rest, "/"); i > 0 {
		return rest[:i]
	}
	return ""
}

// decompressAndVerify reads src (whose codec is `compression`), writes the
// decompressed bytes to dst (with executable mode — these are extracted
// binaries), and verifies the sha256 of the uncompressed bytes matches
// wantSHA. A mismatch removes dst and errors loudly so a tampered download
// never silently installs.
func decompressAndVerify(src, dst, compression, wantSHA string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	var r io.Reader = in
	switch compression {
	case "", "none":
		// raw
	case compressionBrotli:
		r = brotli.NewReader(in)
	default:
		return fmt.Errorf("unknown compression codec %q", compression)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), r); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, wantSHA) {
		os.Remove(tmp)
		return fmt.Errorf("sha256 mismatch (want %s, got %s)", wantSHA, got)
	}
	return os.Rename(tmp, dst)
}
