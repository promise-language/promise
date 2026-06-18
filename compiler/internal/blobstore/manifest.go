// Package blobstore implements the runtime consumption side of Promise's
// content-addressed dependency store (docs/distribution.md §1–§4). Heavy
// dependencies (LLVM opt/llc/lld/libLLVM, musl CRT, target sysroots) are
// delivered as content-addressed blobs fetched on demand into
// <PromiseHome>/cache/blobs/sha256/<hash> and verified against an embedded
// manifest.
//
// This package mirrors — but deliberately does NOT reuse — the build-time
// download/verify/extract/lock pattern in tools/build/common/prebuilts.go.
// That package is build-only and its cache (PrebuiltsCacheRoot) is separate
// from ~/.promise. blobstore is importable by the shipped compiler binary.
package blobstore

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ManifestSchema is the embedded-manifest schema version this loader
// understands. Bumped on an incompatible change to the JSON shape.
const ManifestSchema = 1

// Manifest is the embedded dependency manifest (always present, thin and full).
// One Entry per heavy dependency, separating content identity (Name/SHA256/Size)
// from acquisition (a ranked Sources list). See docs/distribution.md §4.1.
type Manifest struct {
	Schema  int             `json:"schema"`
	Epoch   string          `json:"epoch"`
	Entries []ManifestEntry `json:"entries"`
}

// ManifestEntry is one heavy dependency. SHA256 is the content address of the
// EXTRACTED blob — the cache key AND the integrity/trust anchor, decoupled from
// any URL. (On macOS, LLVM Mach-O blobs are patched+signed when materialized
// into the per-target view dir, NOT in the CAS — so SHA256 is over the raw
// upstream-extracted bytes; see materialize_darwin.go and the resolver.)
type ManifestEntry struct {
	Name    string   `json:"name"`    // logical name codegen asks for: "llvm-opt", "musl-crt1.o", ...
	SHA256  string   `json:"sha256"`  // content address of the extracted blob (cache key + integrity)
	Size    int64    `json:"size"`    // extracted size in bytes (cheap overshoot defense, §4.3)
	Kind    string   `json:"kind"`    // KindMachOLLVM (macOS patch/sign on materialize) or KindBlob
	Sources []Source `json:"sources"` // ranked; first source whose bytes verify wins
}

// Source kinds for ManifestEntry.Kind.
const (
	// KindMachOLLVM marks an LLVM Mach-O blob: on macOS the per-target view
	// copy is patched (install_name_tool) + ad-hoc re-signed (codesign) so it
	// can load libLLVM.dylib (§5.1). A no-op on other platforms.
	KindMachOLLVM = "macho-llvm"
	// KindBlob is an opaque blob materialized verbatim (musl CRT objects, etc.).
	KindBlob = "blob"
)

// Source is one acquisition path: either a direct Blob URL, or a path
// (ArchivePath) inside a compressed Archive. Several blobs may share one
// archive (one LLVM tarball yields opt/llc/lld/libLLVM); the resolver fetches
// such an archive once. ArchiveSHA256, when given, makes the archive safe to
// persistently cache across runs (§4.2 Archive reuse).
type Source struct {
	Blob           string `json:"blob,omitempty"`
	Compression    string `json:"compression,omitempty"`     // transport codec of the Blob asset ("" / "brotli")
	CompressedSize int64  `json:"compressed_size,omitempty"` // download (over-the-wire) byte count of the Blob asset; 0 = unknown
	Archive        string `json:"archive,omitempty"`
	ArchivePath    string `json:"archive_path,omitempty"`
	ArchiveSHA256  string `json:"archive_sha256,omitempty"`
}

// compressionBrotli is the only transport codec understood today (brotli-11 per
// blob; docs/release-automation.md §3). The content sha256 is always over the
// UNCOMPRESSED bytes — compression is purely a transport layer the resolver
// decodes before verifying. Kept in lockstep with common.compressionBrotli in
// the build tools (separate Go module).
const compressionBrotli = "brotli"

// IsArchive reports whether the source extracts a path from a compressed archive.
func (s Source) IsArchive() bool { return s.Archive != "" }

// DownloadSize returns the over-the-wire byte count for fetching this entry: the
// CompressedSize of its first (highest-ranked) blob source that records one. When
// no source carries a compressed size (e.g. an archive-only bootstrap manifest,
// or a pre-compressed-size manifest), it falls back to the uncompressed Size so
// callers always have a non-zero estimate to display.
func (e *ManifestEntry) DownloadSize() int64 {
	for _, s := range e.Sources {
		if s.Blob != "" && s.CompressedSize > 0 {
			return s.CompressedSize
		}
	}
	return e.Size
}

// ParseManifest parses embedded manifest JSON and validates it.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("validate manifest: %w", err)
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if m.Schema != ManifestSchema {
		return fmt.Errorf("unsupported manifest schema %d (want %d)", m.Schema, ManifestSchema)
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
			if s.Archive != "" && s.CompressedSize != 0 {
				return fmt.Errorf("entry %q source[%d]: compressed_size only applies to blob sources", e.Name, j)
			}
			switch s.Compression {
			case "", "none", compressionBrotli:
			default:
				return fmt.Errorf("entry %q source[%d]: unknown compression codec %q", e.Name, j, s.Compression)
			}
		}
	}
	return nil
}

// Lookup returns the entry for a logical name and whether it was found.
func (m *Manifest) Lookup(name string) (*ManifestEntry, bool) {
	for i := range m.Entries {
		if m.Entries[i].Name == name {
			return &m.Entries[i], true
		}
	}
	return nil, false
}

// Marshal renders the manifest as indented JSON (used by the build-time
// generator that writes resources/manifest.json).
func (m *Manifest) Marshal() ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}
