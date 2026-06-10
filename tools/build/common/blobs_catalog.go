package common

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// blobs_catalog.go implements `tools/build/blobs.json` — the committed,
// multi-version index of every prebuilt dependency blob the project hosts
// (T0797). The catalog is the **source of truth** for "which blobs exist and
// what each one is". It is a plain index, not a lockfile — CAS blobs are
// content-addressed, so additions never conflict (the same content always
// yields the same hash; a new version is just a new key) and there is nothing
// to synchronize/lock.
//
// Identity is a 4-tuple `(dependency, version, target, name)`. Forward lookup
// resolves the upload + projection paths; reverse lookup (by `sha256`) yields
// provenance. Hosting location is derived, not stored: release tag =
// `deps-<dependency>-<version>`, asset = `<sha256>` + suffix-from-`compression`.
//
// Role split:
//   - `tools/build/prebuilts.toml` = "the version we build against now"
//     (single pinned, drives dev/CI builds).
//   - `tools/build/blobs.json`     = "every blob hosted across versions"
//     (the catalog).
//   - runtime manifest             = projection for one epoch.

// BlobsCatalogSchema is the on-disk schema version this loader understands.
// Bumped only when the JSON shape changes incompatibly.
const BlobsCatalogSchema = 1

// BlobsCatalog is the parsed `tools/build/blobs.json`. Empty-but-valid (Schema
// set, no Blobs) when the file does not yet exist — the first `publish-blobs`
// run bootstraps it.
type BlobsCatalog struct {
	Schema int         `json:"schema"`
	Blobs  []BlobEntry `json:"blobs"`
}

// BlobSource records the upstream archive a blob was sliced from (T0836).
// All three fields are content-derived and deterministic — re-publishing a
// byte-identical blob produces an identical entry so blobs.json stays
// reproducible.
type BlobSource struct {
	ArchiveURL    string `json:"archive_url"`
	ArchiveSHA256 string `json:"archive_sha256"`
	Member        string `json:"member"`
}

// BlobEntry is one hosted prebuilt blob: a per-platform extracted artifact
// (e.g. `opt` for `linux-amd64` at `llvm 23.1.0`) identified by the 4-tuple
// `(Dependency, Version, Target, Name)`, plus the hash + size of the
// uncompressed CAS content and the transport-codec metadata for its hosted
// asset.
type BlobEntry struct {
	// Dependency is the upstream project (e.g. "llvm", "musl", "wasi-sdk").
	// NOT named `source` so it doesn't clash with the runtime manifest's
	// `sources` acquisition list.
	Dependency string `json:"dependency"`
	// Version is the upstream release identifier (e.g. "23.1.0").
	Version string `json:"version"`
	// Target is the platform identifier the blob applies to (e.g.
	// "linux-amd64") — `opt` for linux is a different blob from `opt` for
	// darwin, so identity must include it.
	Target string `json:"target"`
	// Name is the artifact/file name (e.g. "opt", "llc", "crt1.o").
	Name string `json:"name"`
	// SHA256 is the hex sha256 of the UNCOMPRESSED content — the CAS key
	// and what the runtime verifies after decompressing.
	SHA256 string `json:"sha256"`
	// Size is the uncompressed byte count.
	Size int64 `json:"size"`
	// Compression is the transport codec used to host the asset (T0795).
	// "" or "none" mean uncompressed; "brotli" is the only other codec
	// emitted today.
	Compression string `json:"compression"`
	// CompressedSize is the hosted asset's byte count (the download size).
	// Optional in the JSON shape (older entries may lack it) so it carries
	// `omitempty`.
	CompressedSize int64 `json:"compressed_size,omitempty"`
	// CompressedSHA256 is an OPTIONAL cheap pre-decompress integrity check.
	// Authoritative verification remains the uncompressed `SHA256`.
	CompressedSHA256 string `json:"compressed_sha256,omitempty"`
	// Source records the upstream archive this blob was sliced from (T0836).
	// Optional — entries written before T0836 will have nil Source.
	Source *BlobSource `json:"source,omitempty"`
}

// blobsCatalogPath returns the on-disk path of the catalog under root.
func blobsCatalogPath(root string) string {
	return filepath.Join(root, "tools", "build", "blobs.json")
}

// LoadBlobsCatalog reads `tools/build/blobs.json` under root. A missing file
// is NOT an error — it returns an empty-but-valid catalog (Schema set) so the
// first `publish-blobs` invocation bootstraps the file.
func LoadBlobsCatalog(root string) (*BlobsCatalog, error) {
	path := blobsCatalogPath(root)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &BlobsCatalog{Schema: BlobsCatalogSchema}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c BlobsCatalog
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return &c, nil
}

// WriteBlobsCatalog writes the catalog atomically to `tools/build/blobs.json`
// under root, with entries sorted by (Dependency, Version, Target, Name) so
// diffs across `publish-blobs` runs stay stable.
func WriteBlobsCatalog(root string, c *BlobsCatalog) error {
	if err := c.Validate(); err != nil {
		return err
	}
	sorted := make([]BlobEntry, len(c.Blobs))
	copy(sorted, c.Blobs)
	sort.SliceStable(sorted, func(i, j int) bool {
		return blobKeyLess(sorted[i], sorted[j])
	})
	out := BlobsCatalog{Schema: c.Schema, Blobs: sorted}
	data, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := blobsCatalogPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// blobKeyLess orders by (Dependency, Version, Target, Name) ascending — the
// stable diff order on disk.
func blobKeyLess(a, b BlobEntry) bool {
	if a.Dependency != b.Dependency {
		return a.Dependency < b.Dependency
	}
	if a.Version != b.Version {
		return a.Version < b.Version
	}
	if a.Target != b.Target {
		return a.Target < b.Target
	}
	return a.Name < b.Name
}

// Validate checks the catalog's schema, key uniqueness, codec coverage, and
// required-field presence. Same-key entries (a duplicate of the 4-tuple) are
// rejected; identical SHA256 values under different identities are allowed
// (legal because CAS content sometimes coincides across targets).
func (c *BlobsCatalog) Validate() error {
	if c.Schema != BlobsCatalogSchema {
		return fmt.Errorf("unsupported catalog schema %d (want %d)", c.Schema, BlobsCatalogSchema)
	}
	seen := make(map[string]int, len(c.Blobs))
	for i, e := range c.Blobs {
		if strings.TrimSpace(e.Dependency) == "" {
			return fmt.Errorf("blob[%d]: missing dependency", i)
		}
		if strings.TrimSpace(e.Version) == "" {
			return fmt.Errorf("blob[%d]: missing version", i)
		}
		if strings.TrimSpace(e.Target) == "" {
			return fmt.Errorf("blob[%d]: missing target", i)
		}
		if strings.TrimSpace(e.Name) == "" {
			return fmt.Errorf("blob[%d]: missing name", i)
		}
		if strings.TrimSpace(e.SHA256) == "" {
			return fmt.Errorf("blob[%d] %s: missing sha256", i, blobIdent(e))
		}
		if e.Size <= 0 {
			return fmt.Errorf("blob[%d] %s: size must be > 0", i, blobIdent(e))
		}
		if _, err := assetSuffix(e.Compression); err != nil {
			return fmt.Errorf("blob[%d] %s: %w", i, blobIdent(e), err)
		}
		key := blobPrimaryKey(e.Dependency, e.Version, e.Target, e.Name)
		if prev, ok := seen[key]; ok {
			return fmt.Errorf("duplicate blob key %q at blob[%d] (also at blob[%d])", key, i, prev)
		}
		seen[key] = i
	}
	return nil
}

// Lookup performs the forward lookup `(dep, version, target, name) → entry`.
// Returns (entry, true) on hit; (nil, false) when no such 4-tuple is recorded.
func (c *BlobsCatalog) Lookup(dep, version, target, name string) (*BlobEntry, bool) {
	for i := range c.Blobs {
		e := &c.Blobs[i]
		if e.Dependency == dep && e.Version == version && e.Target == target && e.Name == name {
			return e, true
		}
	}
	return nil, false
}

// LookupBySHA performs the reverse lookup `sha256 → first matching entry`.
// Useful for provenance/debug ("what blob is this hash?"). A single sha may
// legally appear under multiple identities; the first match is returned.
func (c *BlobsCatalog) LookupBySHA(sha string) (*BlobEntry, bool) {
	sha = strings.ToLower(strings.TrimSpace(sha))
	if sha == "" {
		return nil, false
	}
	for i := range c.Blobs {
		e := &c.Blobs[i]
		if strings.EqualFold(e.SHA256, sha) {
			return e, true
		}
	}
	return nil, false
}

// Upsert inserts or replaces the entry whose primary key matches `e`. An
// existing entry with the SAME key but a different `(SHA256, Size)` pair is
// rejected: catalog entries are immutable per key, so a content change under a
// stable identity is the corruption sentinel ("an immutable blob mutated") and
// must be resolved by bumping the version (or refreshing prebuilts.toml).
func (c *BlobsCatalog) Upsert(e BlobEntry) error {
	if _, err := assetSuffix(e.Compression); err != nil {
		return fmt.Errorf("upsert %s: %w", blobIdent(e), err)
	}
	for i := range c.Blobs {
		cur := &c.Blobs[i]
		if cur.Dependency == e.Dependency && cur.Version == e.Version &&
			cur.Target == e.Target && cur.Name == e.Name {
			if !strings.EqualFold(cur.SHA256, e.SHA256) || cur.Size != e.Size {
				return fmt.Errorf("upsert %s: catalog entry already records sha256=%s size=%d, refusing to overwrite with sha256=%s size=%d "+
					"(immutable CAS — bump the dependency version)",
					blobIdent(e), cur.SHA256, cur.Size, e.SHA256, e.Size)
			}
			c.Blobs[i] = e
			return nil
		}
	}
	c.Blobs = append(c.Blobs, e)
	return nil
}

// DepsReleaseTag returns the GitHub release tag that hosts a dependency
// version's blobs: `deps-<dep>-<version>` (e.g. "deps-llvm-23.1.0").
func DepsReleaseTag(dep, version string) string {
	return "deps-" + dep + "-" + version
}

// BlobAssetURL returns the published asset URL for a blob: the deps release
// tag + the uncompressed CAS hash stem + the codec suffix.
func BlobAssetURL(tag, sha, compression string) (string, error) {
	suffix, err := assetSuffix(compression)
	if err != nil {
		return "", err
	}
	return releaseAssetBase + "/" + tag + "/" + sha + suffix, nil
}

// blobIdent renders a short identity tag for error messages.
func blobIdent(e BlobEntry) string {
	return e.Dependency + "/" + e.Version + "/" + e.Target + "/" + e.Name
}

// blobPrimaryKey builds the dedup key for catalog validation.
func blobPrimaryKey(dep, version, target, name string) string {
	return dep + "\x00" + version + "\x00" + target + "\x00" + name
}
