package main

import (
	_ "embed"
	"sync"

	"github.com/promise-language/promise/compiler/internal/blobstore"
)

// embeddedManifest is the runtime dependency manifest (docs/distribution.md §4.1),
// ALWAYS embedded — thin and full alike. Thin builds carry an empty-entries
// placeholder (host LLVM resolved from PATH/Homebrew, or fetched on first use
// once the release manifest is present); full/release builds carry real entries
// with content hashes + ranked sources. Written by the build tool's
// EmbedResources / GenerateRuntimeManifest into resources/manifest.json.
//
//go:embed resources/manifest.json
var embeddedManifest []byte

var (
	embeddedManifestOnce   sync.Once
	embeddedManifestParsed *blobstore.Manifest
	embeddedManifestErr    error
)

// loadEmbeddedManifest parses the embedded manifest once. A parse failure (or an
// empty placeholder) is non-fatal — callers fall through to their local-toolchain
// probes.
func loadEmbeddedManifest() (*blobstore.Manifest, error) {
	embeddedManifestOnce.Do(func() {
		embeddedManifestParsed, embeddedManifestErr = blobstore.ParseManifest(embeddedManifest)
	})
	return embeddedManifestParsed, embeddedManifestErr
}
