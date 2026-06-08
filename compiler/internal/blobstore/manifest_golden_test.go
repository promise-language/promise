package blobstore

import (
	"os"
	"strings"
	"testing"
)

// TestParseManifestRejectsMalformedJSON covers the json.Unmarshal error branch in
// ParseManifest, which is distinct from the schema-validation errors tested
// elsewhere — bad JSON never reaches validate().
func TestParseManifestRejectsMalformedJSON(t *testing.T) {
	_, err := ParseManifest([]byte("{not valid json"))
	if err == nil {
		t.Fatal("expected error for malformed JSON input")
	}
	if !strings.Contains(err.Error(), "parse manifest") {
		t.Errorf("error should contain \"parse manifest\", got %q", err.Error())
	}
}

// TestManifestGoldenFixture is a cross-module schema sentinel. It reads the
// golden fixture that tools/build/common also tests, confirming the consumer
// can parse everything the producer emits. Update the fixture whenever the
// schema or validation rules change — failing to do so breaks this test.
func TestManifestGoldenFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/manifest_golden.json")
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("ParseManifest(golden): %v", err)
	}
	if m.Schema != ManifestSchema {
		t.Errorf("fixture schema = %d, ManifestSchema = %d; update the fixture or the constant", m.Schema, ManifestSchema)
	}
	if len(m.Entries) < 2 {
		t.Errorf("fixture has %d entries, want >=2 (one per Kind constant)", len(m.Entries))
	}
	// Verify both Kind constants appear so tag renames surface.
	kinds := make(map[string]bool)
	for _, e := range m.Entries {
		kinds[e.Kind] = true
	}
	for _, want := range []string{KindBlob, KindMachOLLVM} {
		if !kinds[want] {
			t.Errorf("fixture missing entry with Kind=%q; add one to exercise the constant", want)
		}
	}
	// Round-trip: Marshal -> ParseManifest must succeed.
	b, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := ParseManifest(b); err != nil {
		t.Fatalf("round-trip ParseManifest: %v", err)
	}
}
