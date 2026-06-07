package common

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testBlobEntry returns a minimally-valid blob entry — handy for tests that
// want to vary one field at a time.
func testBlobEntry(dep, version, target, name string) BlobEntry {
	return BlobEntry{
		Dependency:     dep,
		Version:        version,
		Target:         target,
		Name:           name,
		SHA256:         "deadbeef" + name,
		Size:           42,
		Compression:    compressionBrotli,
		CompressedSize: 21,
	}
}

func TestLoadBlobsCatalogMissingFileBootstraps(t *testing.T) {
	root := t.TempDir()
	// File does not exist → empty-but-valid catalog (schema set, no entries).
	c, err := LoadBlobsCatalog(root)
	if err != nil {
		t.Fatalf("LoadBlobsCatalog (missing) should succeed, got: %v", err)
	}
	if c.Schema != BlobsCatalogSchema {
		t.Fatalf("Schema = %d, want %d", c.Schema, BlobsCatalogSchema)
	}
	if len(c.Blobs) != 0 {
		t.Fatalf("empty catalog should have 0 blobs, got %d", len(c.Blobs))
	}
}

func TestLoadBlobsCatalogBadJSON(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "tools", "build", "blobs.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBlobsCatalog(root); err == nil {
		t.Fatal("expected parse error on malformed JSON")
	}
}

func TestLoadBlobsCatalogBadSchema(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "tools", "build", "blobs.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schema":999,"blobs":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBlobsCatalog(root); err == nil {
		t.Fatal("expected schema validation error")
	}
}

func TestBlobsCatalogValidate(t *testing.T) {
	cases := []struct {
		name string
		c    BlobsCatalog
		want string
	}{
		{"bad schema", BlobsCatalog{Schema: 999}, "unsupported catalog schema"},
		{"missing dependency", BlobsCatalog{Schema: 1, Blobs: []BlobEntry{
			{Version: "23.1.0", Target: "linux-amd64", Name: "opt", SHA256: "h", Size: 1, Compression: "brotli"},
		}}, "missing dependency"},
		{"missing version", BlobsCatalog{Schema: 1, Blobs: []BlobEntry{
			{Dependency: "llvm", Target: "linux-amd64", Name: "opt", SHA256: "h", Size: 1, Compression: "brotli"},
		}}, "missing version"},
		{"missing target", BlobsCatalog{Schema: 1, Blobs: []BlobEntry{
			{Dependency: "llvm", Version: "23.1.0", Name: "opt", SHA256: "h", Size: 1, Compression: "brotli"},
		}}, "missing target"},
		{"missing name", BlobsCatalog{Schema: 1, Blobs: []BlobEntry{
			{Dependency: "llvm", Version: "23.1.0", Target: "linux-amd64", SHA256: "h", Size: 1, Compression: "brotli"},
		}}, "missing name"},
		{"missing sha256", BlobsCatalog{Schema: 1, Blobs: []BlobEntry{
			{Dependency: "llvm", Version: "23.1.0", Target: "linux-amd64", Name: "opt", Size: 1, Compression: "brotli"},
		}}, "missing sha256"},
		{"non-positive size", BlobsCatalog{Schema: 1, Blobs: []BlobEntry{
			{Dependency: "llvm", Version: "23.1.0", Target: "linux-amd64", Name: "opt", SHA256: "h", Size: 0, Compression: "brotli"},
		}}, "size must be > 0"},
		{"unknown codec", BlobsCatalog{Schema: 1, Blobs: []BlobEntry{
			{Dependency: "llvm", Version: "23.1.0", Target: "linux-amd64", Name: "opt", SHA256: "h", Size: 1, Compression: "lz4"},
		}}, "unknown compression codec"},
		{"duplicate primary key", BlobsCatalog{Schema: 1, Blobs: []BlobEntry{
			testBlobEntry("llvm", "23.1.0", "linux-amd64", "opt"),
			testBlobEntry("llvm", "23.1.0", "linux-amd64", "opt"),
		}}, "duplicate blob key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}

	// A well-formed catalog validates: distinct targets share a sha (allowed
	// — CAS content sometimes coincides) and distinct compressed_sha256 fields
	// are tolerated.
	good := BlobsCatalog{Schema: 1, Blobs: []BlobEntry{
		testBlobEntry("llvm", "23.1.0", "linux-amd64", "opt"),
		testBlobEntry("llvm", "23.1.0", "darwin-arm64", "opt"),
		testBlobEntry("llvm", "22.1.0", "linux-amd64", "opt"),  // multi-version
		{Dependency: "musl", Version: "1.2.5", Target: "linux-amd64", Name: "crt1.o",
			SHA256: "abc", Size: 100, Compression: "none"}, // uncompressed is valid
	}}
	if err := good.Validate(); err != nil {
		t.Fatalf("well-formed catalog should validate, got: %v", err)
	}
}

func TestBlobsCatalogLookup(t *testing.T) {
	c := &BlobsCatalog{Schema: 1, Blobs: []BlobEntry{
		testBlobEntry("llvm", "23.1.0", "linux-amd64", "opt"),
		testBlobEntry("llvm", "23.1.0", "linux-amd64", "llc"),
		testBlobEntry("llvm", "22.1.0", "linux-amd64", "opt"),
	}}
	// Forward hit.
	got, ok := c.Lookup("llvm", "23.1.0", "linux-amd64", "opt")
	if !ok {
		t.Fatal("forward Lookup must hit")
	}
	if got.SHA256 != "deadbeefopt" {
		t.Fatalf("got SHA256 = %q", got.SHA256)
	}
	// Forward miss — wrong target.
	if _, ok := c.Lookup("llvm", "23.1.0", "darwin-arm64", "opt"); ok {
		t.Fatal("Lookup should miss on wrong target")
	}
	// Reverse hit.
	got, ok = c.LookupBySHA("DEADBEEFOPT") // case-insensitive
	if !ok {
		t.Fatal("LookupBySHA must hit")
	}
	if got.Name != "opt" {
		t.Fatalf("reverse lookup returned name = %q", got.Name)
	}
	// Reverse miss.
	if _, ok := c.LookupBySHA("not a hash"); ok {
		t.Fatal("LookupBySHA should miss on unknown hash")
	}
	// Empty input is rejected.
	if _, ok := c.LookupBySHA("   "); ok {
		t.Fatal("LookupBySHA must reject empty input")
	}
}

func TestBlobsCatalogUpsertReplaces(t *testing.T) {
	c := &BlobsCatalog{Schema: 1}
	e := testBlobEntry("llvm", "23.1.0", "linux-amd64", "opt")
	if err := c.Upsert(e); err != nil {
		t.Fatal(err)
	}
	if len(c.Blobs) != 1 {
		t.Fatalf("after insert, len = %d", len(c.Blobs))
	}
	// Same key + same content → replaces in place (no duplicate).
	if err := c.Upsert(e); err != nil {
		t.Fatal(err)
	}
	if len(c.Blobs) != 1 {
		t.Fatalf("idempotent re-upsert must not duplicate, len = %d", len(c.Blobs))
	}
	// Same key but updated compressed_size → allowed (transport-only metadata).
	e2 := e
	e2.CompressedSize = 99
	if err := c.Upsert(e2); err != nil {
		t.Fatal(err)
	}
	if c.Blobs[0].CompressedSize != 99 {
		t.Fatalf("Upsert did not replace transport metadata: got %d", c.Blobs[0].CompressedSize)
	}
}

func TestBlobsCatalogUpsertImmutableSHA(t *testing.T) {
	c := &BlobsCatalog{Schema: 1}
	e := testBlobEntry("llvm", "23.1.0", "linux-amd64", "opt")
	if err := c.Upsert(e); err != nil {
		t.Fatal(err)
	}
	// SAME primary key, DIFFERENT sha256 → reject (corruption sentinel: an
	// immutable CAS blob mutated under us).
	tampered := e
	tampered.SHA256 = "feeefefefefee" // different content
	err := c.Upsert(tampered)
	if err == nil {
		t.Fatal("Upsert must reject a sha256 change under a stable key")
	}
	if !strings.Contains(err.Error(), "immutable CAS") {
		t.Fatalf("error should call out the immutability invariant, got: %v", err)
	}
	// Size mutation under a stable key is the same corruption case.
	resized := e
	resized.Size = 9999
	if err := c.Upsert(resized); err == nil {
		t.Fatal("Upsert must reject a size change under a stable key")
	}
}

func TestBlobsCatalogUpsertRejectsUnknownCodec(t *testing.T) {
	c := &BlobsCatalog{Schema: 1}
	e := testBlobEntry("llvm", "23.1.0", "linux-amd64", "opt")
	e.Compression = "lz4"
	if err := c.Upsert(e); err == nil {
		t.Fatal("Upsert must reject an unknown codec")
	}
}

func TestWriteBlobsCatalogSorts(t *testing.T) {
	root := t.TempDir()
	// Insert out-of-order — write must sort by (dep, version, target, name).
	c := &BlobsCatalog{Schema: 1, Blobs: []BlobEntry{
		testBlobEntry("llvm", "23.1.0", "linux-amd64", "llc"),
		testBlobEntry("llvm", "22.1.0", "linux-amd64", "opt"),
		testBlobEntry("llvm", "23.1.0", "darwin-arm64", "opt"),
		testBlobEntry("llvm", "23.1.0", "linux-amd64", "opt"),
	}}
	if err := WriteBlobsCatalog(root, c); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(blobsCatalogPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatal("written catalog should end with a newline")
	}
	var disk BlobsCatalog
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	want := []string{"22.1.0/linux-amd64/opt", "23.1.0/darwin-arm64/opt", "23.1.0/linux-amd64/llc", "23.1.0/linux-amd64/opt"}
	if len(disk.Blobs) != len(want) {
		t.Fatalf("disk has %d entries, want %d", len(disk.Blobs), len(want))
	}
	for i, e := range disk.Blobs {
		got := e.Version + "/" + e.Target + "/" + e.Name
		if got != want[i] {
			t.Fatalf("disk[%d] = %s, want %s", i, got, want[i])
		}
	}

	// Round-trip via LoadBlobsCatalog preserves Schema + the same entries.
	rt, err := LoadBlobsCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Schema != BlobsCatalogSchema {
		t.Fatalf("round-trip schema = %d", rt.Schema)
	}
	if len(rt.Blobs) != len(want) {
		t.Fatalf("round-trip Blobs len = %d, want %d", len(rt.Blobs), len(want))
	}
}

func TestWriteBlobsCatalogRejectsInvalid(t *testing.T) {
	root := t.TempDir()
	bad := &BlobsCatalog{Schema: 1, Blobs: []BlobEntry{
		{Dependency: "llvm", Version: "23.1.0", Target: "linux-amd64", Name: "opt",
			SHA256: "h", Size: 1, Compression: "lz4"},
	}}
	if err := WriteBlobsCatalog(root, bad); err == nil {
		t.Fatal("WriteBlobsCatalog must validate before writing")
	}
	if Exists(blobsCatalogPath(root)) {
		t.Fatal("a validation failure must not leave a partial catalog file")
	}
}

// TestBlobKeyLessOrdering exercises every primary-key axis of `blobKeyLess` so
// none of the four `< / >` comparators silently rot. Without an explicit
// dependency-axis case, `WriteBlobsCatalog`'s sort order can't be trusted to
// keep multi-dependency catalogs (llvm + future musl + future wasi-sdk) in a
// stable diff order on disk.
func TestBlobKeyLessOrdering(t *testing.T) {
	cases := []struct {
		name string
		a, b BlobEntry
		want bool
	}{
		// Dependency differs — the axis with no prior coverage. `llvm` < `musl`
		// alphabetically.
		{"dep less", testBlobEntry("llvm", "23.1.0", "linux-amd64", "opt"),
			testBlobEntry("musl", "1.2.5", "linux-amd64", "crt1.o"), true},
		{"dep greater", testBlobEntry("musl", "1.2.5", "linux-amd64", "crt1.o"),
			testBlobEntry("llvm", "23.1.0", "linux-amd64", "opt"), false},
		// Version axis (string-compared, so 22.1.0 < 23.1.0).
		{"version less", testBlobEntry("llvm", "22.1.0", "linux-amd64", "opt"),
			testBlobEntry("llvm", "23.1.0", "linux-amd64", "opt"), true},
		// Target axis.
		{"target less", testBlobEntry("llvm", "23.1.0", "darwin-arm64", "opt"),
			testBlobEntry("llvm", "23.1.0", "linux-amd64", "opt"), true},
		// Name axis — final tiebreaker.
		{"name less", testBlobEntry("llvm", "23.1.0", "linux-amd64", "llc"),
			testBlobEntry("llvm", "23.1.0", "linux-amd64", "opt"), true},
		// All-equal → not less.
		{"equal", testBlobEntry("llvm", "23.1.0", "linux-amd64", "opt"),
			testBlobEntry("llvm", "23.1.0", "linux-amd64", "opt"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := blobKeyLess(tc.a, tc.b); got != tc.want {
				t.Fatalf("blobKeyLess = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWriteBlobsCatalogMultiDepSort confirms WriteBlobsCatalog's stable sort
// orders multi-dependency catalogs by dependency first, then by the rest of the
// primary key — the on-disk diff contract a future `musl` blob entry depends
// on.
func TestWriteBlobsCatalogMultiDepSort(t *testing.T) {
	root := t.TempDir()
	c := &BlobsCatalog{Schema: 1, Blobs: []BlobEntry{
		// Deliberately interleaved so a naive append-order writer would fail.
		testBlobEntry("musl", "1.2.5", "linux-amd64", "crt1.o"),
		testBlobEntry("llvm", "23.1.0", "linux-amd64", "opt"),
		testBlobEntry("musl", "1.2.5", "linux-amd64", "crti.o"),
	}}
	if err := WriteBlobsCatalog(root, c); err != nil {
		t.Fatal(err)
	}
	rt, err := LoadBlobsCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, e := range rt.Blobs {
		got = append(got, e.Dependency+"/"+e.Name)
	}
	want := []string{"llvm/opt", "musl/crt1.o", "musl/crti.o"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("disk[%d] = %s, want %s (multi-dep sort order broken)", i, got[i], want[i])
		}
	}
}

// TestLoadBlobsCatalogReadError pins LoadBlobsCatalog's non-missing-file error
// path: a path that exists but is unreadable (a directory) must produce a
// "read" error, not silently bootstrap an empty catalog (which would mask a
// catalog the user expected to load).
func TestLoadBlobsCatalogReadError(t *testing.T) {
	root := t.TempDir()
	// Make `tools/build/blobs.json` exist as a DIRECTORY — os.ReadFile fails
	// with an error that is NOT os.IsNotExist, so LoadBlobsCatalog must
	// surface it instead of returning an empty catalog.
	path := blobsCatalogPath(root)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := LoadBlobsCatalog(root)
	if err == nil {
		t.Fatal("expected error when blobs.json is unreadable (a directory)")
	}
	if !strings.Contains(err.Error(), "read") && !strings.Contains(err.Error(), "blobs.json") {
		t.Fatalf("error should mention the read failure / path, got: %v", err)
	}
}

func TestDepsReleaseTagAndBlobAssetURL(t *testing.T) {
	if got := DepsReleaseTag("llvm", "23.1.0"); got != "deps-llvm-23.1.0" {
		t.Fatalf("DepsReleaseTag = %q", got)
	}
	url, err := BlobAssetURL("deps-llvm-23.1.0", "abc123", compressionBrotli)
	if err != nil {
		t.Fatal(err)
	}
	want := releaseAssetBase + "/deps-llvm-23.1.0/abc123.br"
	if url != want {
		t.Fatalf("BlobAssetURL brotli = %q, want %q", url, want)
	}
	// Uncompressed: no suffix.
	url, err = BlobAssetURL("deps-llvm-23.1.0", "abc123", "")
	if err != nil {
		t.Fatal(err)
	}
	if url != releaseAssetBase+"/deps-llvm-23.1.0/abc123" {
		t.Fatalf("BlobAssetURL uncompressed = %q", url)
	}
	// Unknown codec fails.
	if _, err := BlobAssetURL("deps-llvm-23.1.0", "abc123", "lz4"); err == nil {
		t.Fatal("BlobAssetURL must reject unknown codec")
	}
}
