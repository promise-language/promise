package common

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestManifestGoldenFixtureProducer is a cross-module schema sentinel. It reads
// the same golden fixture the blobstore consumer tests, confirming the producer
// struct (runtimeManifest) can round-trip everything the fixture describes and
// that runtimeManifestSchema matches. Update the fixture whenever schema or
// validation rules change — failing to do so breaks this test.
func TestManifestGoldenFixtureProducer(t *testing.T) {
	root, err := FindRoot()
	if err != nil {
		t.Skipf("find root: %v", err)
	}
	fixturePath := filepath.Join(root, "compiler", "internal", "blobstore", "testdata", "manifest_golden.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}

	// Unmarshal into the producer struct — confirms JSON field tags match.
	var m runtimeManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal into runtimeManifest: %v", err)
	}

	// Schema constant must match the fixture value.
	if m.Schema != runtimeManifestSchema {
		t.Errorf("fixture schema=%d, runtimeManifestSchema=%d; bump both constants and update the fixture together", m.Schema, runtimeManifestSchema)
	}
	if len(m.Entries) == 0 {
		t.Fatal("golden fixture has no entries")
	}

	// Full producer validation must accept the fixture.
	if err := m.validate(); err != nil {
		t.Fatalf("runtimeManifest.validate(golden): %v", err)
	}

	// Re-marshal and verify the top-level JSON keys survive the round-trip.
	out, err := json.Marshal(&m)
	if err != nil {
		t.Fatalf("json.Marshal runtimeManifest: %v", err)
	}
	var check map[string]any
	if err := json.Unmarshal(out, &check); err != nil {
		t.Fatalf("unmarshal re-marshalled: %v", err)
	}
	for _, key := range []string{"schema", "epoch", "entries"} {
		if _, ok := check[key]; !ok {
			t.Errorf("re-marshalled manifest missing top-level key %q", key)
		}
	}

	// Verify source fields survive: at least one blob source with compression
	// and one archive source must round-trip through runtimeSource.
	var sawBlobWithCompression, sawArchive bool
	for _, e := range m.Entries {
		for _, s := range e.Sources {
			if s.Blob != "" && s.Compression != "" {
				sawBlobWithCompression = true
			}
			if s.Archive != "" && s.ArchivePath != "" {
				sawArchive = true
			}
		}
	}
	if !sawBlobWithCompression {
		t.Error("no blob source with compression in fixture — add one to exercise the Compression field")
	}
	if !sawArchive {
		t.Error("no archive source in fixture — add one to exercise Archive/ArchivePath/ArchiveSHA256 fields")
	}
}
