package module

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCommunityModules(t *testing.T) {
	src := `# community catalog
[modules.foo]
url = "https://github.com/promise-community/foo"
description = "A foo"

[modules.bar]
url = "https://github.com/promise-community/bar.git"
`
	cc, err := ParseCommunityModules([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cc.Modules) != 2 {
		t.Fatalf("expected 2 modules, got %d", len(cc.Modules))
	}
	foo := cc.Lookup("foo")
	if foo == nil || foo.URL != "https://github.com/promise-community/foo" || foo.Description != "A foo" {
		t.Errorf("foo entry wrong: %+v", foo)
	}
	if cc.Lookup("missing") != nil {
		t.Error("expected nil for missing module")
	}

	// LookupByURL normalizes scheme/.git so the bar entry matches a bare host URL.
	if e := cc.LookupByURL("github.com/promise-community/bar"); e == nil || e.Name != "bar" {
		t.Errorf("LookupByURL bar: %+v", e)
	}
}

func TestParseCommunityModulesMissingURL(t *testing.T) {
	if _, err := ParseCommunityModules([]byte("[modules.foo]\ndescription = \"x\"\n")); err == nil {
		t.Fatal("expected error for entry missing url")
	}
}

func TestParseCompatIndexAndVerified(t *testing.T) {
	src := `{
  "epoch": "2026.1",
  "modules": {
    "foo": {"commit": "abc123", "tag": "epoch-2026.1"},
    "empty": {"commit": ""}
  }
}`
	idx, err := ParseCompatIndex([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if idx.Epoch != "2026.1" {
		t.Errorf("epoch = %q", idx.Epoch)
	}
	if e, ok := idx.Verified("foo"); !ok || e.Commit != "abc123" || e.Tag != "epoch-2026.1" {
		t.Errorf("Verified(foo) = %+v, %v", e, ok)
	}
	if _, ok := idx.Verified("empty"); ok {
		t.Error("entry with empty commit should not be Verified")
	}
	if _, ok := idx.Verified("missing"); ok {
		t.Error("missing module should not be Verified")
	}
	// nil index is safe.
	var nilIdx *CompatIndex
	if _, ok := nilIdx.Verified("x"); ok {
		t.Error("nil index Verified should be false")
	}
}

func TestModuleTier(t *testing.T) {
	cases := []struct {
		url  string
		want Tier
	}{
		{"", TierEmbedded},
		{"   ", TierEmbedded},
		{"https://github.com/promise-language/json", TierFirstParty},
		{"github.com/promise-language/json.git", TierFirstParty},
		{"https://github.com/Promise-Language/Json", TierFirstParty}, // case-insensitive org
		{"https://github.com/promise-community/foo", TierCommunity},
		{"git://github.com/promise-community/foo.git", TierCommunity},
		{"https://gitlab.com/someone/parser", TierAdHoc},
		{"github.com/someone/promise-community-fake", TierAdHoc}, // not the org
	}
	for _, c := range cases {
		if got := ModuleTier(c.url); got != c.want {
			t.Errorf("ModuleTier(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

func TestCommunityCatalogURLOverride(t *testing.T) {
	t.Setenv("PROMISE_COMMUNITY_CATALOG", "")
	if got := CommunityCatalogURL(); got != DefaultCommunityCatalogURL {
		t.Errorf("default = %q, want %q", got, DefaultCommunityCatalogURL)
	}
	t.Setenv("PROMISE_COMMUNITY_CATALOG", "https://mirror.example/catalog")
	if got := CommunityCatalogURL(); got != "https://mirror.example/catalog" {
		t.Errorf("override = %q", got)
	}
}

func TestLoadCompatIndexAndHighest(t *testing.T) {
	dir := t.TempDir()
	idxDir := filepath.Join(dir, "index")
	if err := os.MkdirAll(idxDir, 0755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(idxDir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("2026.0.json", `{"epoch":"2026.0","modules":{"foo":{"commit":"c0","tag":"epoch-2026.0"}}}`)
	write("2026.1.json", `{"epoch":"2026.1","modules":{"foo":{"commit":"c1","tag":"epoch-2026.1"},"bar":{"commit":"b1","tag":"epoch-2026.1"}}}`)
	write("notepoch.json", `{"epoch":"x","modules":{}}`) // ignored (non-numeric)

	// Present epoch.
	idx, err := LoadCompatIndex(dir, "2026.1")
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := idx.Verified("bar"); !ok || e.Commit != "b1" {
		t.Errorf("bar = %+v, %v", e, ok)
	}
	// Absent epoch → (nil, nil).
	idx, err = LoadCompatIndex(dir, "2030.9")
	if err != nil || idx != nil {
		t.Errorf("absent epoch: idx=%v err=%v", idx, err)
	}

	if epochs, _ := IndexedEpochs(dir); len(epochs) != 2 || epochs[0] != "2026.0" || epochs[1] != "2026.1" {
		t.Errorf("IndexedEpochs = %v", epochs)
	}

	// foo is recorded for both; highest is 2026.1.
	if ep, tag := HighestIndexedEpoch(dir, "foo"); ep != "2026.1" || tag != "epoch-2026.1" {
		t.Errorf("HighestIndexedEpoch(foo) = %q %q", ep, tag)
	}
	// bar only in 2026.1.
	if ep, _ := HighestIndexedEpoch(dir, "bar"); ep != "2026.1" {
		t.Errorf("HighestIndexedEpoch(bar) = %q", ep)
	}
	// unknown module → "".
	if ep, _ := HighestIndexedEpoch(dir, "nope"); ep != "" {
		t.Errorf("HighestIndexedEpoch(nope) = %q", ep)
	}
}

// TestTierString covers the human-readable rendering of every tier, including
// the default (ad-hoc) arm.
func TestTierString(t *testing.T) {
	cases := []struct {
		t    Tier
		want string
	}{
		{TierEmbedded, "embedded"},
		{TierFirstParty, "first-party"},
		{TierCommunity, "community"},
		{TierAdHoc, "ad-hoc"},
		{Tier(99), "ad-hoc"}, // unknown value falls through to the default arm
	}
	for _, c := range cases {
		if got := c.t.String(); got != c.want {
			t.Errorf("Tier(%d).String() = %q, want %q", c.t, got, c.want)
		}
	}
}

// TestCommunityLookupNil covers the nil-receiver guards on both lookups (a nil
// catalog is the "no community catalog fetched" state, which must not panic).
func TestCommunityLookupNil(t *testing.T) {
	var cc *CommunityCatalog
	if cc.Lookup("foo") != nil {
		t.Error("nil catalog Lookup should be nil")
	}
	if cc.LookupByURL("https://github.com/promise-community/foo") != nil {
		t.Error("nil catalog LookupByURL should be nil")
	}
	// Non-nil catalog, URL not listed → nil (the LookupByURL not-found arm).
	real := &CommunityCatalog{Modules: map[string]*CommunityEntry{
		"foo": {Name: "foo", URL: "https://github.com/promise-community/foo"},
	}}
	if real.LookupByURL("https://github.com/promise-community/other") != nil {
		t.Error("unlisted URL should not match")
	}
}

// TestParseCommunityModulesErrors covers the malformed-input paths: an unterminated
// section header, an empty module name, and a body line with no '='.
func TestParseCommunityModulesErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"unterminated section", "[modules.foo\nurl = \"x\"\n", "invalid section header"},
		{"empty module name", "[modules.]\nurl = \"x\"\n", "empty module name"},
		{"malformed body line", "[modules.foo]\nurl = \"x\"\ngarbage line\n", "expected key = value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseCommunityModules([]byte(c.src)); err == nil ||
				!strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want containing %q", err, c.want)
			}
		})
	}

	// A non-`modules.` section is tolerated (current=nil): its keys are ignored
	// and parsing succeeds with no modules.
	cc, err := ParseCommunityModules([]byte("[meta]\nversion = \"1\"\n"))
	if err != nil {
		t.Fatalf("non-module section should parse: %v", err)
	}
	if len(cc.Modules) != 0 {
		t.Errorf("expected no modules, got %d", len(cc.Modules))
	}
}

// TestParseCompatIndexInvalidJSON covers the json.Unmarshal error arm.
func TestParseCompatIndexInvalidJSON(t *testing.T) {
	if _, err := ParseCompatIndex([]byte("{not json")); err == nil ||
		!strings.Contains(err.Error(), "parsing compat index") {
		t.Errorf("expected parse error, got %v", err)
	}
	// A valid payload with no "modules" key gets a non-nil (empty) map.
	idx, err := ParseCompatIndex([]byte(`{"epoch":"2026.1"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idx.Modules == nil {
		t.Error("Modules should be initialized to a non-nil map")
	}
}

// TestIndexedEpochsAndHighestNoDir covers the missing-index-directory arms: both
// IndexedEpochs and HighestIndexedEpoch must degrade to empty rather than error.
func TestIndexedEpochsAndHighestNoDir(t *testing.T) {
	dir := t.TempDir() // no index/ subdirectory
	epochs, err := IndexedEpochs(dir)
	if err != nil || epochs != nil {
		t.Errorf("IndexedEpochs(no dir) = %v, %v; want nil, nil", epochs, err)
	}
	if ep, tag := HighestIndexedEpoch(dir, "foo"); ep != "" || tag != "" {
		t.Errorf("HighestIndexedEpoch(no dir) = %q %q; want empty", ep, tag)
	}
}

// TestIndexedEpochsSortsManyOutOfOrder exercises the full insertion-sort inner
// loop (sortEpochsAsc) with more than two out-of-order epochs and confirms a
// non-numeric *.json filename is skipped.
func TestIndexedEpochsSortsManyOutOfOrder(t *testing.T) {
	dir := t.TempDir()
	idxDir := filepath.Join(dir, "index")
	if err := os.MkdirAll(idxDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Written out of ascending order; one cross-year boundary; one non-epoch file.
	for _, name := range []string{"2027.0.json", "2026.10.json", "2026.2.json", "2026.1.json", "README.json"} {
		body := `{"epoch":"x","modules":{}}`
		if err := os.WriteFile(filepath.Join(idxDir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	epochs, err := IndexedEpochs(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2026.1", "2026.2", "2026.10", "2027.0"}
	if len(epochs) != len(want) {
		t.Fatalf("epochs = %v, want %v", epochs, want)
	}
	for i := range want {
		if epochs[i] != want[i] {
			t.Errorf("epochs[%d] = %q, want %q (full = %v)", i, epochs[i], want[i], epochs)
		}
	}
}

// TestHighestIndexedEpochSkipsCorrupt covers the load-error continue arm: a
// malformed index/<epoch>.json must be skipped, with the highest *valid* epoch
// still reported, rather than aborting the scan.
func TestHighestIndexedEpochSkipsCorrupt(t *testing.T) {
	dir := t.TempDir()
	idxDir := filepath.Join(dir, "index")
	if err := os.MkdirAll(idxDir, 0755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(idxDir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("2026.1.json", `{"epoch":"2026.1","modules":{"foo":{"commit":"c1","tag":"epoch-2026.1"}}}`)
	write("2026.2.json", `{not valid json`) // corrupt — must be skipped

	if ep, tag := HighestIndexedEpoch(dir, "foo"); ep != "2026.1" || tag != "epoch-2026.1" {
		t.Errorf("HighestIndexedEpoch(foo) = %q %q; want 2026.1/epoch-2026.1 (corrupt 2026.2 skipped)", ep, tag)
	}
}

func TestSaveCompatIndexRoundTrip(t *testing.T) {
	dir := t.TempDir()
	idx := &CompatIndex{Epoch: "2026.2", Modules: map[string]IndexEntry{
		"foo": {Commit: "deadbeef", Tag: "epoch-2026.2"},
	}}
	if err := SaveCompatIndex(dir, idx); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCompatIndex(dir, "2026.2")
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := got.Verified("foo"); !ok || e.Commit != "deadbeef" {
		t.Errorf("round-trip = %+v, %v", e, ok)
	}
}
