package module

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `
[module]
name = "myapp"
epoch = "2026.3"

[require]
"github.com/someone/parser" = "a1b2c3d"
"git.corp.com/team/utils" = "e4f5a6b"

[replace]
"github.com/someone/parser" = "../parser"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Name != "myapp" {
		t.Errorf("Name = %q, want %q", cfg.Name, "myapp")
	}
	if cfg.Epoch != "2026.3" {
		t.Errorf("Epoch = %q, want %q", cfg.Epoch, "2026.3")
	}
	if cfg.Require["github.com/someone/parser"] != "a1b2c3d" {
		t.Errorf("Require[parser] = %q, want %q", cfg.Require["github.com/someone/parser"], "a1b2c3d")
	}
	if cfg.Require["git.corp.com/team/utils"] != "e4f5a6b" {
		t.Errorf("Require[utils] = %q, want %q", cfg.Require["git.corp.com/team/utils"], "e4f5a6b")
	}
	if cfg.Replace["github.com/someone/parser"] != "../parser" {
		t.Errorf("Replace[parser] = %q, want %q", cfg.Replace["github.com/someone/parser"], "../parser")
	}
}

func TestParseConfigMinimal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `
[module]
name = "hello"
epoch = "2026.3"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "hello" {
		t.Errorf("Name = %q, want %q", cfg.Name, "hello")
	}
	if len(cfg.Require) != 0 {
		t.Errorf("Require should be empty, got %d entries", len(cfg.Require))
	}
}

func TestParseConfigMissingName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `
[module]
epoch = "2026.3"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := ParseConfig(path)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
}

func TestFindConfig(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "src", "pkg")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}

	content := `
[module]
name = "myapp"
epoch = "2026.3"
`
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := FindConfig(subdir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("expected to find config")
	}
	if cfg.Name != "myapp" {
		t.Errorf("Name = %q, want %q", cfg.Name, "myapp")
	}
	if cfg.Dir != dir {
		t.Errorf("Dir = %q, want %q", cfg.Dir, dir)
	}
}

func TestFindConfigNotFound(t *testing.T) {
	dir := t.TempDir()
	cfg, err := FindConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		t.Fatal("expected nil config when no promise.toml exists")
	}
}

func TestIsLocalPath(t *testing.T) {
	tests := []struct {
		path  string
		local bool
	}{
		{"./libs/models", true},
		{"../shared/auth", true},
		{"/opt/shared/auth", true},
		{"C:\\projects\\auth", true},
		{"d:/projects/auth", true},
		{"github.com/someone/parser", false},
		{"git.corp.com/team/utils", false},
		{"models", false},
	}
	for _, tt := range tests {
		got := IsLocalPath(tt.path)
		if got != tt.local {
			t.Errorf("IsLocalPath(%q) = %v, want %v", tt.path, got, tt.local)
		}
	}
}

func TestParseConfigUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `
[module]
name = "myapp"
epoch = "2026.3"
future_key = "ignored"

[future_section]
whatever = "also ignored"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "myapp" {
		t.Errorf("Name = %q, want %q", cfg.Name, "myapp")
	}
}
