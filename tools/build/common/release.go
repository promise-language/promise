package common

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/andybalholm/brotli"
)

// release.go implements `bin/release` — the forge driver that produces the
// release artifacts the consumer side (T0769/T0770) reads: the dependency blobs,
// the embedded runtime manifest with ranked acquisition sources, the thin/full
// compiler variants, and the manifest-integrity gate. See T0773 and
// docs/release-automation.md §2 (build-order).
//
// Hosting (authoritative, T0773 planning): prebuilt blobs are published as
// **GitHub release assets** on github.com/promise-language/promise, named by
// their content sha256. Each manifest entry's primary source is that release
// asset; the pinned upstream vendor archive (e.g. the LLVM tarball) is a ranked
// fallback so a not-yet-published release still resolves. A future CDN/R2 mirror
// (T0523) is a non-breaking add — ranked sources + PROMISE_BLOB_MIRROR already
// support it.

// releaseAssetBase is the GitHub release-download URL prefix. Asset names are the
// blob's content sha256 (content-addressed → unchanged dep reuses the same asset
// across releases, no re-upload).
const releaseAssetBase = "https://github.com/promise-language/promise/releases/download"

const releaseUsage = `usage: bin/release <subcommand> [flags]

subcommands:
  blobs --host <target> --out <dir>
        collect the host's dependency blobs (host LLVM tools) into <dir>.
  manifest <blobsdir> --host <target> --pack <dir> --out <manifest> [--tag <tag>]
        hash+size each blob, pack hash-named artifacts into <dir>, and write the
        embedded manifest with ranked sources to <manifest>.
  manifest --from-catalog --host <target> --out <manifest>
        project tools/build/blobs.json into a per-epoch runtime manifest. No
        blobs need to be staged locally — sha/size/sources come from the catalog,
        and the deps-<dep>-<version> tag is derived from blobs.json (no --tag).
  publish-blobs --dependency <dep> --host <target> [--dry-run] [--no-upload]
        produce (extract + brotli-11 compress + hash), record in blobs.json, and
        upload to the deps-<dep>-<version> release. Idempotent: blobs already in
        the catalog with the matching hash are skipped.
  fetch-blobs --manifest <m> --out <dir> [--keep-compressed]
        download + brotli-decompress each manifest entry's primary blob source
        into <dir>. --keep-compressed leaves the <sha>.br alongside (for
        verify-manifest).
  build --variant {thin|full} --manifest <m> --out <bin> [--blobs <dir>] [--host <target>]
        build the compiler with the manifest embedded; compile the Promise stub
        with the just-built compiler and embed it back. full also pre-stages host
        LLVM blobs for offline use.
  verify-manifest <manifest>... --against <dir>
        fail the release if any manifest entry's packaged artifact in <dir> does
        not yield matching sha256 bytes.`

// RunRelease dispatches a `bin/release` subcommand.
func RunRelease(root string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", releaseUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "blobs":
		return runReleaseBlobs(root, rest)
	case "manifest":
		return runReleaseManifest(root, rest)
	case "publish-blobs":
		return runReleasePublishBlobs(root, rest)
	case "fetch-blobs":
		return runReleaseFetchBlobs(root, rest)
	case "build":
		return runReleaseBuild(root, rest)
	case "verify-manifest":
		return runReleaseVerifyManifest(root, rest)
	case "-h", "--help", "help":
		fmt.Println(releaseUsage)
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q\n%s", sub, releaseUsage)
	}
}

// githubAssetURL is the published release-asset URL for a blob of the given
// content hash under the given release tag.
func githubAssetURL(tag, hash string) string {
	return releaseAssetBase + "/" + tag + "/" + hash
}

// defaultReleaseTag derives the release tag (epoch-<epoch>) from catalog.toml.
func defaultReleaseTag(root string) (string, error) {
	epoch, err := ParseEpoch(root)
	if err != nil {
		return "", err
	}
	return "epoch-" + epoch, nil
}

// splitPositionalFlags partitions args into positionals and flag tokens. Every
// `bin/release` flag takes a value (none are boolean), so a `-flag` token without
// an `=` consumes the following token as its value. This lets a subcommand accept
// positionals interleaved with flags (e.g. `manifest <blobsdir> --host x`).
func splitPositionalFlags(args []string) (positionals, flags []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positionals = append(positionals, a)
	}
	return positionals, flags
}

// llvmTargetEntry resolves the LLVM prebuilts entry for a target, erroring on a
// missing or unsupported target.
func llvmTargetEntry(root, target string) (*PrebuiltsManifest, *TargetEntry, error) {
	pm, err := LoadPrebuiltsManifest(root)
	if err != nil {
		return nil, nil, fmt.Errorf("load prebuilts manifest: %w", err)
	}
	llvm := pm.Binaries["llvm"]
	if llvm == nil {
		return nil, nil, fmt.Errorf("prebuilts manifest: missing [binaries.llvm]")
	}
	t := llvm.Targets[target]
	if t == nil {
		return nil, nil, fmt.Errorf("prebuilts manifest: no [binaries.llvm.targets.%s]", target)
	}
	if t.Unsupported != "" {
		return nil, nil, fmt.Errorf("target %s is not supported: %s", target, t.Unsupported)
	}
	return pm, t, nil
}

// runReleaseBlobs collects the host's dependency blobs into --out (§2 step 1).
//
// Today that is the host LLVM toolchain (opt/llc/lld), fetched into the prebuilts
// cache by FetchAll and copied out raw+executable. Each file is named by its
// runtime `out` name — an extracted blob ready to hash by `manifest`.
//
// musl CRT, macOS SDK stubs, and Windows UCRT stubs are NOT collected here: they
// have no public blob host nor upstream-archive source yet and stay
// embedded-delivered (musl) / are owned by the cross-compile track (T0530/T0531/
// T0532). Emitting them into the manifest would activate an unsatisfiable runtime
// fetch path. They join once their hosting lands.
func runReleaseBlobs(root string, args []string) error {
	fs := flag.NewFlagSet("blobs", flag.ContinueOnError)
	host := fs.String("host", CurrentBuildTarget(), "target to collect blobs for")
	out := fs.String("out", "", "output directory for collected blobs (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("blobs: --out is required\n%s", releaseUsage)
	}

	pm, tEntry, err := llvmTargetEntry(root, *host)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}

	fmt.Printf("Collecting LLVM blobs for %s...\n", *host)
	extracted, err := FetchAll(pm, *host, []string{"llvm"})
	if err != nil {
		return fmt.Errorf("fetch prebuilts: %w", err)
	}
	cacheDir := extracted["llvm"]
	if cacheDir == "" {
		return fmt.Errorf("no LLVM prebuilt fetched for %s", *host)
	}
	for _, f := range tEntry.Files {
		src := filepath.Join(cacheDir, f.Out)
		dst := filepath.Join(*out, f.Out)
		if err := copyFilePreservingMode(src, dst); err != nil {
			return fmt.Errorf("copy blob %s: %w", f.Out, err)
		}
		printSize(dst)
	}
	fmt.Printf("Collected %d blobs into %s\n", len(tEntry.Files), *out)
	return nil
}

// runReleaseManifest hashes the collected blobs, packs hash-named artifacts for
// upload, and writes the embedded manifest with ranked sources (§2 steps 2–3).
//
// With --from-catalog (T0797), the mode flips: no blobs need to be staged
// locally. Sha/size/sources come straight from `tools/build/blobs.json` (the
// committed catalog of every blob hosted across versions). Per-epoch CI runs in
// seconds — no 700 MB LLVM download, no 10-minute brotli-11. The per-epoch
// runtime manifest is a projection of `blobs.json` for the epoch's pinned
// versions.
func runReleaseManifest(root string, args []string) error {
	// `--from-catalog` is the only boolean flag in the manifest CLI; the
	// shared splitPositionalFlags helper assumes every flag takes a value
	// (it consumes the next token), so peek for the boolean here and strip
	// it before splitting positionals from flags.
	fromCatalog := false
	stripped := args[:0:0]
	for _, a := range args {
		if a == "--from-catalog" || a == "-from-catalog" || a == "--from-catalog=true" || a == "-from-catalog=true" {
			fromCatalog = true
			continue
		}
		stripped = append(stripped, a)
	}
	positionals, flags := splitPositionalFlags(stripped)
	fs := flag.NewFlagSet("manifest", flag.ContinueOnError)
	host := fs.String("host", CurrentBuildTarget(), "target the blobs are for")
	pack := fs.String("pack", "", "directory to write hash-named upload artifacts (required without --from-catalog)")
	out := fs.String("out", "", "manifest output path (required)")
	tag := fs.String("tag", "", "release tag for asset URLs (default epoch-<epoch>) — not allowed with --from-catalog (tag derived from blobs.json)")
	_ = fs.Bool("from-catalog", false, "project tools/build/blobs.json into the manifest (no local blobs needed)") // documentation-only; pre-stripped above
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("manifest: --out is required\n%s", releaseUsage)
	}
	if fromCatalog {
		if len(positionals) != 0 {
			return fmt.Errorf("manifest --from-catalog: no positional <blobsdir> argument (the catalog provides hashes)\n%s", releaseUsage)
		}
		if *pack != "" {
			return fmt.Errorf("manifest --from-catalog: --pack is not supported (nothing to pack locally)\n%s", releaseUsage)
		}
		// The catalog determines the deps release tag (deps-<dep>-<version>);
		// overriding it would point the manifest's blob URLs at a release that
		// does not host the blobs, so fetch-blobs would 404 on every entry.
		// Reject rather than silently ignore so a stale --tag flag in a
		// workflow fails loudly instead of producing an unfetchable manifest.
		if *tag != "" {
			return fmt.Errorf("manifest --from-catalog: --tag is not supported (the deps-<dep>-<version> tag is derived from blobs.json)\n%s", releaseUsage)
		}
		return runReleaseManifestFromCatalog(root, *host, *out)
	}
	if len(positionals) != 1 {
		return fmt.Errorf("manifest: expected exactly one <blobsdir> argument\n%s", releaseUsage)
	}
	blobsDir := positionals[0]
	if *pack == "" {
		return fmt.Errorf("manifest: --pack and --out are required\n%s", releaseUsage)
	}
	relTag := *tag
	if relTag == "" {
		t, err := defaultReleaseTag(root)
		if err != nil {
			return err
		}
		relTag = t
	}

	_, tEntry, err := llvmTargetEntry(root, *host)
	if err != nil {
		return err
	}
	epoch, err := ParseEpoch(root)
	if err != nil {
		return err
	}

	// Published dependency blobs are brotli-compressed (T0795): the asset URL is
	// the uncompressed content <hash> plus the codec suffix, and the source
	// carries the codec so the resolver decompresses before verifying the
	// uncompressed sha256.
	suffix, err := assetSuffix(compressionBrotli)
	if err != nil {
		return err
	}
	entries, err := buildLLVMEntries(blobsDir, tEntry, *host, func(hash string) runtimeSource {
		return runtimeSource{Blob: githubAssetURL(relTag, hash) + suffix, Compression: compressionBrotli}
	})
	if err != nil {
		return err
	}

	// Pack each blob brotli-compressed under "<content hash><suffix>".
	// Content-addressed names make this idempotent across releases: an unchanged
	// dependency hashes the same, so the (already-compressed) artifact exists and
	// is left untouched — never recompressed (brotli-11 is slow; §3).
	if err := os.MkdirAll(*pack, 0o755); err != nil {
		return err
	}
	for i, f := range tEntry.Files {
		src := filepath.Join(blobsDir, f.Out)
		dst := filepath.Join(*pack, entries[i].SHA256+suffix)
		if Exists(dst) {
			continue // same hash already packed — caching skip
		}
		if err := compressFileBrotli(src, dst); err != nil {
			return fmt.Errorf("pack %s: %w", f.Out, err)
		}
	}

	m := runtimeManifest{Schema: runtimeManifestSchema, Epoch: epoch, Entries: entries}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	if err := writeRuntimeManifest(*out, &m); err != nil {
		return err
	}
	fmt.Printf("Wrote manifest with %d entries to %s (packed into %s, tag %s)\n",
		len(entries), *out, *pack, relTag)
	return nil
}

// runReleaseManifestFromCatalog projects `tools/build/blobs.json` into the
// per-epoch runtime manifest for `host`. Each LLVM file in prebuilts.toml is
// looked up in the catalog by (dependency, version, target, name) — the catalog
// must have an entry, since `blobs.json` is the committed source of truth for
// hosted blobs. The runtime manifest entries are named `llvm-<out>` (matching
// the bootstrap producer's convention), with the catalog-resolved deps release
// asset as the primary source and the upstream archive as the ranked fallback.
//
// The release tag is ALWAYS the catalog-derived `deps-<dep>-<version>` — the
// CLI rejects --tag in from-catalog mode because the deps release is where
// the blobs actually live; overriding would produce unfetchable URLs.
func runReleaseManifestFromCatalog(root, host, outPath string) error {
	pm, tEntry, err := llvmTargetEntry(root, host)
	if err != nil {
		return err
	}
	llvm := pm.Binaries["llvm"]
	dep, version := "llvm", llvm.Version
	tag := DepsReleaseTag(dep, version)

	catalog, err := LoadBlobsCatalog(root)
	if err != nil {
		return err
	}
	epoch, err := ParseEpoch(root)
	if err != nil {
		return err
	}

	kind := llvmKindForTarget(host)
	entries := make([]runtimeManifestEntry, 0, len(tEntry.Files))
	for _, f := range tEntry.Files {
		be, ok := catalog.Lookup(dep, version, host, f.Out)
		if !ok {
			return fmt.Errorf("blobs.json has no entry for %s/%s/%s/%s — run `bin/release publish-blobs --dependency %s --host %s` first",
				dep, version, host, f.Out, dep, host)
		}
		assetURL, err := BlobAssetURL(tag, be.SHA256, be.Compression)
		if err != nil {
			return fmt.Errorf("entry %s: %w", blobIdent(*be), err)
		}
		entries = append(entries, runtimeManifestEntry{
			Name:   "llvm-" + f.Out,
			SHA256: be.SHA256,
			Size:   be.Size,
			Kind:   kind,
			Sources: []runtimeSource{
				{Blob: assetURL, Compression: be.Compression},
				{Archive: tEntry.URL, ArchivePath: f.Src, ArchiveSHA256: tEntry.SHA256},
			},
		})
	}

	m := runtimeManifest{Schema: runtimeManifestSchema, Epoch: epoch, Entries: entries}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	if err := writeRuntimeManifest(outPath, &m); err != nil {
		return err
	}
	fmt.Printf("Projected %d entries from blobs.json into %s (tag %s)\n", len(entries), outPath, tag)
	return nil
}

// runReleaseVerifyManifest is the build-time integrity gate (§5 publish job): for
// every manifest entry it locates a packaged artifact in --against and fails the
// run on any sha256 mismatch or missing artifact, so a bogus entry never reaches
// users. The build-time counterpart to T0771's runtime CAS verify+repair.
func runReleaseVerifyManifest(root string, args []string) error {
	positionals, flags := splitPositionalFlags(args)
	fs := flag.NewFlagSet("verify-manifest", flag.ContinueOnError)
	against := fs.String("against", "", "directory of packaged artifacts to verify against (required)")
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(positionals) == 0 {
		return fmt.Errorf("verify-manifest: at least one <manifest> argument is required\n%s", releaseUsage)
	}
	if *against == "" {
		return fmt.Errorf("verify-manifest: --against is required\n%s", releaseUsage)
	}

	total := 0
	for _, mp := range positionals {
		m, err := loadRuntimeManifest(mp)
		if err != nil {
			return fmt.Errorf("%s: %w", mp, err)
		}
		for _, e := range m.Entries {
			if err := verifyManifestEntry(e, *against); err != nil {
				return fmt.Errorf("verify-manifest failed (%s): %w", mp, err)
			}
			total++
		}
	}
	fmt.Printf("verify-manifest: %d entries OK against %s\n", total, *against)
	return nil
}

// verifyManifestEntry resolves an entry against the packaged artifacts in
// againstDir. It walks the entry's ranked sources, locates the FIRST present
// artifact (a blob by its hash-named asset, or an archive by its basename), and
// hashes the relevant bytes. A present artifact whose bytes do not match the
// entry's content sha256 is a hard failure (the integrity gate); an entry with no
// present artifact for any source is also a failure (missing packaging).
func verifyManifestEntry(e runtimeManifestEntry, againstDir string) error {
	found := false
	for _, s := range e.Sources {
		switch {
		case s.Blob != "":
			artifact := filepath.Join(againstDir, path.Base(s.Blob))
			if !Exists(artifact) {
				continue
			}
			found = true
			// The packaged artifact may be transport-compressed; the integrity
			// gate hashes the DECOMPRESSED bytes against the content sha256.
			got, err := hashBlobArtifact(artifact, s.Compression)
			if err != nil {
				return fmt.Errorf("entry %q: %w", e.Name, err)
			}
			if !strings.EqualFold(got, e.SHA256) {
				return fmt.Errorf("entry %q: blob %s sha256 mismatch (want %s, got %s)",
					e.Name, path.Base(s.Blob), e.SHA256, got)
			}
			return nil
		case s.Archive != "":
			artifact := filepath.Join(againstDir, path.Base(s.Archive))
			if !Exists(artifact) {
				continue
			}
			found = true
			got, err := hashArchiveMember(artifact, s.ArchivePath)
			if err != nil {
				return fmt.Errorf("entry %q: %w", e.Name, err)
			}
			if !strings.EqualFold(got, e.SHA256) {
				return fmt.Errorf("entry %q: archive member %s sha256 mismatch (want %s, got %s)",
					e.Name, s.ArchivePath, e.SHA256, got)
			}
			return nil
		}
	}
	if !found {
		return fmt.Errorf("entry %q: no packaged artifact found in %s", e.Name, againstDir)
	}
	return nil
}

// compressFileBrotli streams src → dst brotli-compressed at the maximum quality
// (level 11; docs/release-automation.md §3). Slow but rare (only on a new
// dependency version) and content-cacheable; the resolver decompresses on fetch.
func compressFileBrotli(src, dst string) error {
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
	bw := brotli.NewWriterLevel(out, brotli.BestCompression)
	if _, err := io.Copy(bw, in); err != nil {
		bw.Close()
		return err
	}
	if err := bw.Close(); err != nil {
		return err
	}
	return out.Close()
}

// hashBlobArtifact returns the sha256 of a packaged blob artifact's UNCOMPRESSED
// content, decompressing first when the source declares a transport codec. This
// is the integrity gate's counterpart to the resolver's decompress-then-verify.
func hashBlobArtifact(artifact, compression string) (string, error) {
	switch compression {
	case "", "none":
		h, _, err := hashAndSize(artifact)
		return h, err
	case compressionBrotli:
		in, err := os.Open(artifact)
		if err != nil {
			return "", err
		}
		defer in.Close()
		h := sha256.New()
		if _, err := io.Copy(h, brotli.NewReader(in)); err != nil {
			return "", fmt.Errorf("brotli-decompress %s: %w", filepath.Base(artifact), err)
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	default:
		return "", fmt.Errorf("unknown compression codec %q", compression)
	}
}

// hashArchiveMember extracts member from archive into a temp dir (tolerating a
// single top-level wrapper dir, like the resolver does) and returns its sha256.
func hashArchiveMember(archive, member string) (string, error) {
	if member == "" {
		return "", fmt.Errorf("archive source missing archive_path")
	}
	tmp, err := os.MkdirTemp("", "release-verify-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := ExtractArchive(archive, tmp); err != nil {
		return "", fmt.Errorf("extract %s: %w", archive, err)
	}
	inner, err := resolveInnerRoot(tmp, []PrebuiltFile{{Src: member}})
	if err != nil {
		return "", err
	}
	h, _, err := hashAndSize(filepath.Join(inner, member))
	return h, err
}

// loadRuntimeManifest reads, unmarshals, and validates a manifest file written by
// runReleaseManifest. Validation mirrors blobstore.Manifest.validate (the two
// live in separate Go modules; the schema constant + JSON tags are the lockstep).
func loadRuntimeManifest(path string) (*runtimeManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m runtimeManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// validate checks the manifest against the same rules blobstore enforces at
// runtime, so a manifest this tool emits can never fail to parse in the compiler.
func (m *runtimeManifest) validate() error {
	if m.Schema != runtimeManifestSchema {
		return fmt.Errorf("unsupported manifest schema %d (want %d)", m.Schema, runtimeManifestSchema)
	}
	seen := make(map[string]bool, len(m.Entries))
	for i := range m.Entries {
		e := &m.Entries[i]
		if e.Name == "" {
			return fmt.Errorf("entry[%d]: missing name", i)
		}
		if seen[e.Name] {
			return fmt.Errorf("entry %q: duplicate name", e.Name)
		}
		seen[e.Name] = true
		if strings.TrimSpace(e.SHA256) == "" {
			return fmt.Errorf("entry %q: missing sha256", e.Name)
		}
		if len(e.Sources) == 0 {
			return fmt.Errorf("entry %q: no sources", e.Name)
		}
		for j, s := range e.Sources {
			if s.Blob == "" && s.Archive == "" {
				return fmt.Errorf("entry %q source[%d]: neither blob nor archive set", e.Name, j)
			}
			if s.Archive != "" && s.ArchivePath == "" {
				return fmt.Errorf("entry %q source[%d]: archive without archive_path", e.Name, j)
			}
			if s.Archive != "" && s.Compression != "" {
				return fmt.Errorf("entry %q source[%d]: compression only applies to blob sources", e.Name, j)
			}
			if _, err := assetSuffix(s.Compression); err != nil {
				return fmt.Errorf("entry %q source[%d]: %w", e.Name, j, err)
			}
		}
	}
	return nil
}
