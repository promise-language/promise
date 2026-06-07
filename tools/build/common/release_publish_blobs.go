package common

import (
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
	cmd := exec.Command("gh", "release", "upload", tag, localPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh release upload %s %s: %w", tag, localPath, err)
	}
	return nil
}

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
		for _, p := range planned {
			if p.action != publishActionUpload {
				continue
			}
			if err := uploader.UploadAsset(tag, p.brPath); err != nil {
				return err
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
	m, err := loadRuntimeManifest(*manifestPath)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
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
		brPath := filepath.Join(*out, assetName)
		if err := fetcher.FetchAsset(parseReleaseTagFromURL(blobSrc.Blob), assetName, brPath); err != nil {
			return fmt.Errorf("fetch %s: %w", e.Name, err)
		}

		fileName := strings.TrimPrefix(e.Name, "llvm-")
		dstPath := filepath.Join(*out, fileName)
		if err := decompressAndVerify(brPath, dstPath, blobSrc.Compression, e.SHA256); err != nil {
			return fmt.Errorf("decompress %s: %w", e.Name, err)
		}
		if !*keepCompressed {
			_ = os.Remove(brPath)
		}
		fmt.Printf("  fetched %s (%d bytes uncompressed)\n", e.Name, e.Size)
	}
	fmt.Printf("Fetched %d blobs into %s\n", len(m.Entries), *out)
	return nil
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
