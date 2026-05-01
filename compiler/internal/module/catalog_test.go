package module

import (
	"testing"
)

func TestParseCatalogEmpty(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.3"
`)
	cat, err := ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	if cat.Epoch != "2026.3" {
		t.Errorf("expected epoch 2026.3, got %s", cat.Epoch)
	}
	if len(cat.Modules) != 0 {
		t.Errorf("expected 0 modules, got %d", len(cat.Modules))
	}
}

func TestParseCatalogBasic(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.3"

[modules.json]
url = "https://github.com/promise-lang/json"
commit = "a1b2c3d"
description = "JSON parsing and serialization"
`)
	cat, err := ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	if cat.Epoch != "2026.3" {
		t.Errorf("expected epoch 2026.3, got %s", cat.Epoch)
	}
	if len(cat.Modules) != 1 {
		t.Fatalf("expected 1 module, got %d", len(cat.Modules))
	}
	entry := cat.Modules["json"]
	if entry == nil {
		t.Fatal("expected json entry")
	}
	if entry.Name != "json" {
		t.Errorf("expected name json, got %s", entry.Name)
	}
	if entry.URL != "https://github.com/promise-lang/json" {
		t.Errorf("expected URL, got %s", entry.URL)
	}
	if entry.Commit != "a1b2c3d" {
		t.Errorf("expected commit a1b2c3d, got %s", entry.Commit)
	}
	if entry.Description != "JSON parsing and serialization" {
		t.Errorf("expected description, got %s", entry.Description)
	}
}

func TestParseCatalogMultiple(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.3"

[modules.json]
url = "https://github.com/promise-lang/json"
commit = "a1b2c3d"

[modules.http]
url = "https://github.com/promise-lang/http"
commit = "e4f5a6b"

[modules.crypto]
url = "git@github.com:promise-lang/crypto.git"
commit = "7c8d9e0"
`)
	cat, err := ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Modules) != 3 {
		t.Fatalf("expected 3 modules, got %d", len(cat.Modules))
	}
	for _, name := range []string{"json", "http", "crypto"} {
		if cat.Modules[name] == nil {
			t.Errorf("missing module %s", name)
		}
	}
	// Verify SSH URL preserved
	if cat.Modules["crypto"].URL != "git@github.com:promise-lang/crypto.git" {
		t.Errorf("expected SSH URL, got %s", cat.Modules["crypto"].URL)
	}
}

func TestParseCatalogWithRequires(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.3"

[modules.http]
url = "https://github.com/promise-lang/http"
commit = "e4f5a6b"
requires = ["json", "crypto"]
`)
	cat, err := ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	entry := cat.Modules["http"]
	if entry == nil {
		t.Fatal("expected http entry")
	}
	if len(entry.Requires) != 2 {
		t.Fatalf("expected 2 requires, got %d", len(entry.Requires))
	}
	if entry.Requires[0] != "json" || entry.Requires[1] != "crypto" {
		t.Errorf("expected [json, crypto], got %v", entry.Requires)
	}
}

func TestParseCatalogMissingURL(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.3"

[modules.json]
commit = "a1b2c3d"
`)
	_, err := ParseCatalog(data)
	if err == nil {
		t.Fatal("expected error for missing url")
	}
	if !contains(err.Error(), "missing 'url'") {
		t.Errorf("expected 'missing url' error, got: %v", err)
	}
}

func TestParseCatalogMissingCommit(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.3"

[modules.json]
url = "https://github.com/promise-lang/json"
`)
	_, err := ParseCatalog(data)
	if err == nil {
		t.Fatal("expected error for missing commit")
	}
	if !contains(err.Error(), "missing 'commit'") {
		t.Errorf("expected 'missing commit' error, got: %v", err)
	}
}

func TestCatalogLookup(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.3"

[modules.json]
url = "https://github.com/promise-lang/json"
commit = "a1b2c3d"
`)
	cat, err := ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}

	// Found
	entry := cat.Lookup("json")
	if entry == nil {
		t.Fatal("expected json entry")
	}
	if entry.Commit != "a1b2c3d" {
		t.Errorf("expected commit a1b2c3d, got %s", entry.Commit)
	}

	// Not found
	if cat.Lookup("nonexistent") != nil {
		t.Error("expected nil for nonexistent module")
	}

	// Nil catalog
	var nilCat *Catalog
	if nilCat.Lookup("json") != nil {
		t.Error("expected nil for nil catalog")
	}
}

func TestParseCatalogComments(t *testing.T) {
	data := []byte(`
# This is a comment
[catalog]
epoch = "2026.3"

# JSON module
[modules.json]
url = "https://github.com/promise-lang/json"
commit = "a1b2c3d"
# description is optional
`)
	cat, err := ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	if cat.Lookup("json") == nil {
		t.Fatal("expected json entry")
	}
}

func TestParseCatalogUnknownKeys(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.3"
future_key = "ignored"

[modules.json]
url = "https://github.com/promise-lang/json"
commit = "a1b2c3d"
future_field = "also ignored"
`)
	cat, err := ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	if cat.Lookup("json") == nil {
		t.Fatal("expected json entry")
	}
}

func TestParseCatalogEmptyModuleName(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.3"

[modules.]
url = "https://github.com/promise-lang/json"
commit = "a1b2c3d"
`)
	_, err := ParseCatalog(data)
	if err == nil {
		t.Fatal("expected error for empty module name")
	}
	if !contains(err.Error(), "empty module name") {
		t.Errorf("expected 'empty module name' error, got: %v", err)
	}
}

func TestParseCatalogEmptyRequires(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.3"

[modules.json]
url = "https://github.com/promise-lang/json"
commit = "a1b2c3d"
requires = []
`)
	cat, err := ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	entry := cat.Lookup("json")
	if entry == nil {
		t.Fatal("expected json entry")
	}
	if len(entry.Requires) != 0 {
		t.Errorf("expected 0 requires, got %d", len(entry.Requires))
	}
}

func TestParseTOMLArray(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{`["json", "crypto"]`, []string{"json", "crypto"}},
		{`["single"]`, []string{"single"}},
		{`[]`, nil},
		{`["a","b","c"]`, []string{"a", "b", "c"}},
		{`[ "spaced" , "out" ]`, []string{"spaced", "out"}},
	}
	for _, tt := range tests {
		result := parseTOMLArray(tt.input)
		if len(result) != len(tt.expected) {
			t.Errorf("parseTOMLArray(%q): expected %v, got %v", tt.input, tt.expected, result)
			continue
		}
		for i := range result {
			if result[i] != tt.expected[i] {
				t.Errorf("parseTOMLArray(%q)[%d]: expected %q, got %q", tt.input, i, tt.expected[i], result[i])
			}
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && containsSubstring(s, sub)
}

func containsSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
