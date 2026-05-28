package module

import (
	"strings"
	"testing"
)

func TestParseCatalogEmpty(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.0"
`)
	cat, err := ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	if cat.Epoch != "2026.0" {
		t.Errorf("expected epoch 2026.0, got %s", cat.Epoch)
	}
	if len(cat.Modules) != 0 {
		t.Errorf("expected 0 modules, got %d", len(cat.Modules))
	}
}

func TestParseCatalogBasic(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.0"

[modules.json]
url = "https://github.com/promise-language/json"
commit = "a1b2c3d"
description = "JSON parsing and serialization"
`)
	cat, err := ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	if cat.Epoch != "2026.0" {
		t.Errorf("expected epoch 2026.0, got %s", cat.Epoch)
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
	if entry.URL != "https://github.com/promise-language/json" {
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
epoch = "2026.0"

[modules.json]
url = "https://github.com/promise-language/json"
commit = "a1b2c3d"

[modules.http]
url = "https://github.com/promise-language/http"
commit = "e4f5a6b"

[modules.crypto]
url = "git@github.com:promise-language/crypto.git"
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
	if cat.Modules["crypto"].URL != "git@github.com:promise-language/crypto.git" {
		t.Errorf("expected SSH URL, got %s", cat.Modules["crypto"].URL)
	}
}

func TestParseCatalogCommitWithoutURL(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.0"

[modules.json]
commit = "a1b2c3d"
`)
	_, err := ParseCatalog(data)
	if err == nil {
		t.Fatal("expected error for commit without url")
	}
	if !strings.Contains(err.Error(), "missing 'url'") {
		t.Errorf("expected 'missing url' error, got: %v", err)
	}
}

func TestParseCatalogURLWithoutCommit(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.0"

[modules.json]
url = "https://github.com/promise-language/json"
`)
	_, err := ParseCatalog(data)
	if err == nil {
		t.Fatal("expected error for url without commit")
	}
	if !strings.Contains(err.Error(), "missing 'commit'") {
		t.Errorf("expected 'missing commit' error, got: %v", err)
	}
}

func TestParseCatalogEmbedded(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.0"

[modules.io]
description = "Console and file I/O"

[modules.math]
description = "Numeric functions and constants"
`)
	cat, err := ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Modules) != 2 {
		t.Fatalf("expected 2 modules, got %d", len(cat.Modules))
	}
	io := cat.Modules["io"]
	if io == nil {
		t.Fatal("expected io entry")
	}
	if !io.IsEmbedded() {
		t.Error("expected io to be embedded")
	}
	if io.Description != "Console and file I/O" {
		t.Errorf("expected description, got %s", io.Description)
	}
}

func TestParseCatalogMixed(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.0"

[modules.io]
description = "Console and file I/O"

[modules.json]
url = "https://github.com/promise-language/json"
commit = "a1b2c3d"
description = "JSON parsing and serialization"
`)
	cat, err := ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Modules) != 2 {
		t.Fatalf("expected 2 modules, got %d", len(cat.Modules))
	}
	if !cat.Modules["io"].IsEmbedded() {
		t.Error("expected io to be embedded")
	}
	if cat.Modules["json"].IsEmbedded() {
		t.Error("expected json to be external")
	}
}

func TestCatalogEntryIsEmbedded(t *testing.T) {
	embedded := &CatalogEntry{Name: "io", Description: "I/O"}
	if !embedded.IsEmbedded() {
		t.Error("expected embedded for entry without URL")
	}
	external := &CatalogEntry{Name: "json", URL: "https://example.com/json", Commit: "abc123"}
	if external.IsEmbedded() {
		t.Error("expected not embedded for entry with URL")
	}
}

func TestCatalogLookup(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.0"

[modules.json]
url = "https://github.com/promise-language/json"
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
epoch = "2026.0"

# JSON module
[modules.json]
url = "https://github.com/promise-language/json"
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
epoch = "2026.0"
future_key = "ignored"

[modules.json]
url = "https://github.com/promise-language/json"
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
epoch = "2026.0"

[modules.]
url = "https://github.com/promise-language/json"
commit = "a1b2c3d"
`)
	_, err := ParseCatalog(data)
	if err == nil {
		t.Fatal("expected error for empty module name")
	}
	if !strings.Contains(err.Error(), "empty module name") {
		t.Errorf("expected 'empty module name' error, got: %v", err)
	}
}

func TestParseCatalogInvalidSectionHeader(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.0"

[modules.json
url = "https://github.com/promise-language/json"
`)
	_, err := ParseCatalog(data)
	if err == nil {
		t.Fatal("expected error for invalid section header")
	}
	if !strings.Contains(err.Error(), "invalid section header") {
		t.Errorf("expected 'invalid section header' error, got: %v", err)
	}
}

func TestParseCatalogInvalidKeyValueLine(t *testing.T) {
	data := []byte(`
[catalog]
epoch = "2026.0"

[modules.json]
this is not valid toml
`)
	_, err := ParseCatalog(data)
	if err == nil {
		t.Fatal("expected error for invalid key=value line")
	}
	if !strings.Contains(err.Error(), "expected key = value") {
		t.Errorf("expected 'expected key = value' error, got: %v", err)
	}
}
