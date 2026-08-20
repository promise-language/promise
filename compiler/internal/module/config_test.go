package module

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `
[module]
name = "myapp"
epoch = "2026.0"

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
	if cfg.Epoch != "2026.0" {
		t.Errorf("Epoch = %q, want %q", cfg.Epoch, "2026.0")
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
epoch = "2026.0"
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
epoch = "2026.0"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := ParseConfig(path)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
}

func TestParseConfigRejectsEpochNext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `
[module]
name = "hello"
epoch = "next"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := ParseConfig(path)
	if err == nil {
		t.Fatal("expected error for epoch = \"next\"")
	}
	if !strings.Contains(err.Error(), "next") || !strings.Contains(err.Error(), "numeric epoch") {
		t.Errorf("unexpected error: %v", err)
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
epoch = "2026.0"
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

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// Already normalized
		{"github.com/someone/parser", "github.com/someone/parser"},
		// Strip https scheme
		{"https://github.com/someone/parser", "github.com/someone/parser"},
		// Strip http scheme
		{"http://github.com/someone/parser", "github.com/someone/parser"},
		// Strip git scheme
		{"git://github.com/someone/parser", "github.com/someone/parser"},
		// Strip trailing .git
		{"github.com/someone/parser.git", "github.com/someone/parser"},
		// Strip scheme + .git
		{"https://github.com/someone/parser.git", "github.com/someone/parser"},
		// Strip trailing slashes
		{"github.com/someone/parser/", "github.com/someone/parser"},
		{"github.com/someone/parser///", "github.com/someone/parser"},
		// Lowercase host only (preserve path case)
		{"GitHub.COM/Someone/Parser", "github.com/Someone/Parser"},
		{"GITHUB.COM/user/MyLib", "github.com/user/MyLib"},
		// Combined
		{"HTTPS://GitHub.COM/User/Repo.git/", "github.com/User/Repo"},
		// Host only (no path)
		{"GITHUB.COM", "github.com"},
		// Strip ssh scheme
		{"ssh://git@github.com/someone/parser", "git@github.com/someone/parser"},
		{"SSH://git@github.com/someone/parser.git", "git@github.com/someone/parser"},
		// Corporate git servers
		{"git.corp.com/team/utils", "git.corp.com/team/utils"},
		{"https://git.corp.com/team/utils.git", "git.corp.com/team/utils"},
	}
	for _, tt := range tests {
		got := NormalizeURL(tt.input)
		if got != tt.want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizeURLIdempotent(t *testing.T) {
	urls := []string{
		"github.com/someone/parser",
		"https://GITHUB.COM/User/Repo.git/",
		"git.corp.com/team/utils",
	}
	for _, url := range urls {
		first := NormalizeURL(url)
		second := NormalizeURL(first)
		if first != second {
			t.Errorf("NormalizeURL not idempotent: %q → %q → %q", url, first, second)
		}
	}
}

func TestParseConfigUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `
[module]
name = "myapp"
epoch = "2026.0"
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

func TestSetRequireNewSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := "[module]\nname = \"myapp\"\nepoch = \"2026.0\"\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetRequire(path, "github.com/foo/bar", "abc123"); err != nil {
		t.Fatal(err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Require["github.com/foo/bar"]; got != "abc123" {
		t.Errorf("Require[foo/bar] = %q, want %q", got, "abc123")
	}
}

func TestSetRequireExistingSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `[module]
name = "myapp"
epoch = "2026.0"

[require]
"github.com/foo/bar" = "old_hash"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetRequire(path, "github.com/foo/bar", "new_hash"); err != nil {
		t.Fatal(err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Require["github.com/foo/bar"]; got != "new_hash" {
		t.Errorf("Require[foo/bar] = %q, want %q", got, "new_hash")
	}
}

func TestSetRequireAppendToExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `[module]
name = "myapp"
epoch = "2026.0"

[require]
"github.com/foo/bar" = "hash1"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetRequire(path, "github.com/baz/qux", "hash2"); err != nil {
		t.Fatal(err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Require["github.com/foo/bar"]; got != "hash1" {
		t.Errorf("Require[foo/bar] = %q, want %q", got, "hash1")
	}
	if got := cfg.Require["github.com/baz/qux"]; got != "hash2" {
		t.Errorf("Require[baz/qux] = %q, want %q", got, "hash2")
	}
}

func TestSetRequireNormalizedMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `[module]
name = "myapp"
epoch = "2026.0"

[require]
"https://github.com/foo/bar.git" = "old_hash"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// Different URL form for same repo — should replace existing
	if err := SetRequire(path, "github.com/foo/bar", "new_hash"); err != nil {
		t.Fatal(err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	// The old key was replaced in-place, so the new key form is used
	if got := cfg.Require["github.com/foo/bar"]; got != "new_hash" {
		t.Errorf("Require[foo/bar] = %q, want %q", got, "new_hash")
	}
}

func TestIsCatalogImport(t *testing.T) {
	if !IsCatalogImport("") {
		t.Error("empty path should be catalog import")
	}
	if IsCatalogImport("./local") {
		t.Error("local path should not be catalog import")
	}
	if IsCatalogImport("github.com/foo/bar") {
		t.Error("remote URL should not be catalog import")
	}
}

func TestSetRequirePreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `[module]
name = "myapp"
epoch = "2026.0"

# My dependencies
[require]
"github.com/foo/bar" = "hash1"

[replace]
"github.com/foo/bar" = "../bar"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetRequire(path, "github.com/baz/qux", "hash2"); err != nil {
		t.Fatal(err)
	}

	// Verify [replace] is preserved
	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Replace["github.com/foo/bar"]; got != "../bar" {
		t.Errorf("Replace[foo/bar] = %q, want %q", got, "../bar")
	}
	if got := cfg.Require["github.com/baz/qux"]; got != "hash2" {
		t.Errorf("Require[baz/qux] = %q, want %q", got, "hash2")
	}
}

func TestParseConfigNamedRequire(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `
[module]
name = "myapp"
epoch = "2026.0"

[require.parser]
url = "https://github.com/alice/parser"
commit = "a1b2c3d"

[require.utils]
url = "https://github.com/bob/utils"
commit = "e4f5a6b"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.NamedRequire) != 2 {
		t.Fatalf("NamedRequire has %d entries, want 2", len(cfg.NamedRequire))
	}
	p := cfg.NamedRequire["parser"]
	if p == nil {
		t.Fatal("NamedRequire[parser] is nil")
	}
	if p.URL != "https://github.com/alice/parser" {
		t.Errorf("parser URL = %q, want %q", p.URL, "https://github.com/alice/parser")
	}
	if p.Commit != "a1b2c3d" {
		t.Errorf("parser Commit = %q, want %q", p.Commit, "a1b2c3d")
	}
	u := cfg.NamedRequire["utils"]
	if u == nil {
		t.Fatal("NamedRequire[utils] is nil")
	}
	if u.URL != "https://github.com/bob/utils" {
		t.Errorf("utils URL = %q, want %q", u.URL, "https://github.com/bob/utils")
	}
	if u.Commit != "e4f5a6b" {
		t.Errorf("utils Commit = %q, want %q", u.Commit, "e4f5a6b")
	}
}

func TestParseConfigNamedRequireMissingURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `
[module]
name = "myapp"
epoch = "2026.0"

[require.parser]
commit = "a1b2c3d"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := ParseConfig(path)
	if err == nil {
		t.Fatal("expected error for missing url")
	}
	if !strings.Contains(err.Error(), "missing 'url'") {
		t.Errorf("error = %q, want to contain 'missing url'", err.Error())
	}
}

func TestParseConfigNamedRequireMissingCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `
[module]
name = "myapp"
epoch = "2026.0"

[require.parser]
url = "https://github.com/alice/parser"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := ParseConfig(path)
	if err == nil {
		t.Fatal("expected error for missing commit")
	}
	if !strings.Contains(err.Error(), "missing 'commit'") {
		t.Errorf("error = %q, want to contain 'missing commit'", err.Error())
	}
}

func TestParseConfigNamedRequireEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `
[module]
name = "myapp"
epoch = "2026.0"

[require.]
url = "https://github.com/alice/parser"
commit = "a1b2c3d"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := ParseConfig(path)
	if err == nil {
		t.Fatal("expected error for empty name")
	}
	if !strings.Contains(err.Error(), "empty name") {
		t.Errorf("error = %q, want to contain 'empty name'", err.Error())
	}
}

func TestParseConfigNamedRequireBothMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `
[module]
name = "myapp"
epoch = "2026.0"

[require.parser]
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := ParseConfig(path)
	if err == nil {
		t.Fatal("expected error for missing url and commit")
	}
	if !strings.Contains(err.Error(), "missing 'url' and 'commit'") {
		t.Errorf("error = %q, want to contain \"missing 'url' and 'commit'\"", err.Error())
	}
}

func TestParseConfigNamedRequireUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `
[module]
name = "myapp"
epoch = "2026.0"

[require.parser]
url = "https://github.com/alice/parser"
commit = "a1b2c3d"
future_key = "ignored"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.NamedRequire["parser"]
	if p == nil {
		t.Fatal("NamedRequire[parser] is nil")
	}
	if p.URL != "https://github.com/alice/parser" {
		t.Errorf("parser URL = %q, want %q", p.URL, "https://github.com/alice/parser")
	}
	if p.Commit != "a1b2c3d" {
		t.Errorf("parser Commit = %q, want %q", p.Commit, "a1b2c3d")
	}
}

func TestParseConfigNamedRequireSectionReset(t *testing.T) {
	// Verify that switching from [require.NAME] to [replace] resets context
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `
[module]
name = "myapp"
epoch = "2026.0"

[require.parser]
url = "https://github.com/alice/parser"
commit = "a1b2c3d"

[replace]
"https://github.com/alice/parser" = "../local-parser"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.NamedRequire["parser"]
	if p == nil {
		t.Fatal("NamedRequire[parser] is nil")
	}
	if p.URL != "https://github.com/alice/parser" {
		t.Errorf("parser URL = %q", p.URL)
	}
	if cfg.Replace["https://github.com/alice/parser"] != "../local-parser" {
		t.Errorf("Replace not parsed correctly after [require.NAME] section")
	}
}

func TestParseConfigMixedRequire(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `
[module]
name = "myapp"
epoch = "2026.0"

[require]
"https://github.com/someone/other" = "deadbeef"

[require.parser]
url = "https://github.com/alice/parser"
commit = "a1b2c3d"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	// URL-keyed require
	if cfg.Require["https://github.com/someone/other"] != "deadbeef" {
		t.Errorf("Require[other] = %q, want %q", cfg.Require["https://github.com/someone/other"], "deadbeef")
	}
	// Named require
	p := cfg.NamedRequire["parser"]
	if p == nil {
		t.Fatal("NamedRequire[parser] is nil")
	}
	if p.URL != "https://github.com/alice/parser" {
		t.Errorf("parser URL = %q, want %q", p.URL, "https://github.com/alice/parser")
	}
	if p.Commit != "a1b2c3d" {
		t.Errorf("parser Commit = %q, want %q", p.Commit, "a1b2c3d")
	}
}

func TestFindProjectMainWithField(t *testing.T) {
	dir := t.TempDir()
	content := "[module]\nname = \"myapp\"\nmain = \"src/app.pr\"\n"
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := FindProjectMain(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "src/app.pr" {
		t.Errorf("FindProjectMain = %q, want %q", got, "src/app.pr")
	}
}

func TestFindProjectMainNoField(t *testing.T) {
	dir := t.TempDir()
	content := "[module]\nname = \"myapp\"\nepoch = \"2026.0\"\n"
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := FindProjectMain(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("FindProjectMain = %q, want empty", got)
	}
}

func TestFindProjectMainNoToml(t *testing.T) {
	dir := t.TempDir()
	got, err := FindProjectMain(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("FindProjectMain = %q, want empty", got)
	}
}

func TestFindProjectMainWithoutName(t *testing.T) {
	dir := t.TempDir()
	content := "[module]\nmain = \"app.pr\"\n"
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := FindProjectMain(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "app.pr" {
		t.Errorf("FindProjectMain = %q, want %q", got, "app.pr")
	}
}

func TestParseConfigNamedRequireSHA256(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `
[module]
name = "myapp"
epoch = "2026.0"

[require.archive]
url = "https://example.com/mod-v1.0.tar.gz"
sha256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.NamedRequire["archive"]
	if a == nil {
		t.Fatal("NamedRequire[archive] is nil")
	}
	if a.URL != "https://example.com/mod-v1.0.tar.gz" {
		t.Errorf("archive URL = %q", a.URL)
	}
	if a.Commit != "" {
		t.Errorf("archive Commit = %q, want empty", a.Commit)
	}
	if a.SHA256 != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("archive SHA256 = %q", a.SHA256)
	}
}

func TestParseConfigNamedRequireSHA256AndCommitConflict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `
[module]
name = "myapp"
epoch = "2026.0"

[require.bad]
url = "https://github.com/alice/parser"
commit = "a1b2c3d"
sha256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := ParseConfig(path)
	if err == nil {
		t.Fatal("expected error for commit+sha256 conflict")
	}
	if !strings.Contains(err.Error(), "cannot have both") {
		t.Errorf("error = %q, want to contain 'cannot have both'", err.Error())
	}
}

func TestParseConfigNamedRequireSHA256InvalidHex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	tests := []struct {
		name   string
		sha256 string
	}{
		{"too short", "abcdef"},
		{"uppercase", "E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855"},
		{"non-hex", "g3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"too long", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855aa"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := `
[module]
name = "myapp"
epoch = "2026.0"

[require.bad]
url = "https://example.com/mod.tar.gz"
sha256 = "` + tt.sha256 + `"
`
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
			_, err := ParseConfig(path)
			if err == nil {
				t.Fatal("expected error for invalid sha256")
			}
			if !strings.Contains(err.Error(), "invalid 'sha256'") {
				t.Errorf("error = %q, want to contain \"invalid 'sha256'\"", err.Error())
			}
		})
	}
}

func TestParseConfigNamedRequireURLOnlyNoCommitNoSHA256(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `
[module]
name = "myapp"
epoch = "2026.0"

[require.bad]
url = "https://example.com/mod.tar.gz"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := ParseConfig(path)
	if err == nil {
		t.Fatal("expected error for url without commit or sha256")
	}
	if !strings.Contains(err.Error(), "missing 'commit'") {
		t.Errorf("error = %q, want to contain \"missing 'commit'\"", err.Error())
	}
}

func TestSetNamedRequireCommitUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `[module]
name = "myapp"
epoch = "2026.0"

[require.parser]
url = "https://github.com/alice/parser"
commit = "oldcommithash"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetNamedRequireCommit(path, "parser", "newcommithash"); err != nil {
		t.Fatal(err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.NamedRequire["parser"]
	if p == nil {
		t.Fatal("NamedRequire[parser] is nil")
	}
	if p.Commit != "newcommithash" {
		t.Errorf("parser Commit = %q, want %q", p.Commit, "newcommithash")
	}
	if p.URL != "https://github.com/alice/parser" {
		t.Errorf("parser URL = %q, want %q (should be preserved)", p.URL, "https://github.com/alice/parser")
	}
}

func TestSetNamedRequireCommitPreservesOther(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `[module]
name = "myapp"
epoch = "2026.0"

# A comment about dependencies
[require.parser]
url = "https://github.com/alice/parser"
commit = "oldhash"

[require.utils]
url = "https://github.com/bob/utils"
commit = "keepme"

[replace]
"https://github.com/alice/parser" = "../local-parser"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetNamedRequireCommit(path, "parser", "newhash"); err != nil {
		t.Fatal(err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	// parser should be updated
	p := cfg.NamedRequire["parser"]
	if p == nil {
		t.Fatal("NamedRequire[parser] is nil")
	}
	if p.Commit != "newhash" {
		t.Errorf("parser Commit = %q, want %q", p.Commit, "newhash")
	}

	// utils should be untouched
	u := cfg.NamedRequire["utils"]
	if u == nil {
		t.Fatal("NamedRequire[utils] is nil")
	}
	if u.Commit != "keepme" {
		t.Errorf("utils Commit = %q, want %q (should be preserved)", u.Commit, "keepme")
	}

	// replace should be untouched
	if cfg.Replace["https://github.com/alice/parser"] != "../local-parser" {
		t.Error("Replace section was modified")
	}
}

func TestSetNamedRequireCommitReadError(t *testing.T) {
	err := SetNamedRequireCommit("/nonexistent/path/promise.toml", "parser", "abc123")
	if err == nil {
		t.Fatal("expected error for nonexistent file, got nil")
	}
	if !strings.Contains(err.Error(), "cannot read") {
		t.Errorf("error = %q, want it to contain 'cannot read'", err.Error())
	}
}

func TestSetNamedRequireCommitInsertMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	// Section exists with url but no commit line, followed by another section.
	content := `[module]
name = "myapp"
epoch = "2026.0"

[require.parser]
url = "https://github.com/alice/parser"

[require.utils]
url = "https://github.com/bob/utils"
commit = "utilshash"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetNamedRequireCommit(path, "parser", "newhash"); err != nil {
		t.Fatal(err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.NamedRequire["parser"]
	if p == nil {
		t.Fatal("NamedRequire[parser] is nil after insert")
	}
	if p.Commit != "newhash" {
		t.Errorf("parser Commit = %q, want %q", p.Commit, "newhash")
	}
	// utils should be untouched
	u := cfg.NamedRequire["utils"]
	if u == nil {
		t.Fatal("NamedRequire[utils] is nil")
	}
	if u.Commit != "utilshash" {
		t.Errorf("utils Commit = %q, want %q", u.Commit, "utilshash")
	}
}

func TestSetNamedRequireCommitInsertMissingLastSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	// Section at end of file with url but no commit line.
	content := `[module]
name = "myapp"
epoch = "2026.0"

[require.parser]
url = "https://github.com/alice/parser"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetNamedRequireCommit(path, "parser", "newhash"); err != nil {
		t.Fatal(err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.NamedRequire["parser"]
	if p == nil {
		t.Fatal("NamedRequire[parser] is nil after insert")
	}
	if p.Commit != "newhash" {
		t.Errorf("parser Commit = %q, want %q", p.Commit, "newhash")
	}
	if p.URL != "https://github.com/alice/parser" {
		t.Errorf("parser URL = %q, want preserved", p.URL)
	}
}

func TestSetNamedRequireCommitNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `[module]
name = "myapp"
epoch = "2026.0"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	err := SetNamedRequireCommit(path, "nonexistent", "abc123")
	if err == nil {
		t.Fatal("expected error for missing section, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to contain 'not found'", err.Error())
	}
}

// --- RemoveRequire tests ---

func TestRemoveRequireExistingEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `[module]
name = "myapp"
epoch = "2026.0"

[require]
"github.com/foo/bar" = "abc123"
"github.com/baz/qux" = "def456"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if err := RemoveRequire(path, "github.com/foo/bar"); err != nil {
		t.Fatalf("RemoveRequire returned error: %v", err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Require["github.com/foo/bar"]; ok {
		t.Error("expected entry to be removed")
	}
	if cfg.Require["github.com/baz/qux"] != "def456" {
		t.Errorf("Require[baz/qux] = %q, want %q", cfg.Require["github.com/baz/qux"], "def456")
	}
}

func TestRemoveRequireNotPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `[module]
name = "myapp"
epoch = "2026.0"

[require]
"github.com/foo/bar" = "abc123"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// Removing a URL that isn't present is a no-op.
	if err := RemoveRequire(path, "github.com/nobody/nothing"); err != nil {
		t.Fatalf("RemoveRequire returned error for missing entry: %v", err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Require["github.com/foo/bar"] != "abc123" {
		t.Errorf("Require[foo/bar] = %q, want abc123 (should be unchanged)", cfg.Require["github.com/foo/bar"])
	}
}

func TestRemoveRequireNormalizedMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `[module]
name = "myapp"
epoch = "2026.0"

[require]
"https://github.com/foo/bar.git" = "abc123"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// Different URL form for the same repo — should still match via NormalizeURL.
	if err := RemoveRequire(path, "github.com/foo/bar"); err != nil {
		t.Fatalf("RemoveRequire returned error: %v", err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	// Both the normalized and original form should be gone.
	if len(cfg.Require) != 0 {
		t.Errorf("Require should be empty after removal, got %d entries", len(cfg.Require))
	}
}

func TestRemoveRequireLastEntryInSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `[module]
name = "myapp"
epoch = "2026.0"

[require]
"github.com/foo/bar" = "abc123"

[replace]
"github.com/foo/bar" = "../bar"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if err := RemoveRequire(path, "github.com/foo/bar"); err != nil {
		t.Fatalf("RemoveRequire returned error: %v", err)
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Require) != 0 {
		t.Errorf("Require should be empty, got %d entries", len(cfg.Require))
	}
	// [replace] section should be intact.
	if cfg.Replace["github.com/foo/bar"] != "../bar" {
		t.Errorf("Replace[foo/bar] = %q, want ../bar (should be preserved)", cfg.Replace["github.com/foo/bar"])
	}
}

func TestRemoveRequireReadError(t *testing.T) {
	err := RemoveRequire("/nonexistent/path/promise.toml", "github.com/foo/bar")
	if err == nil {
		t.Fatal("expected error for nonexistent file, got nil")
	}
	if !strings.Contains(err.Error(), "cannot read") {
		t.Errorf("error = %q, want it to contain 'cannot read'", err.Error())
	}
}

func TestRemoveRequireNoSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	content := `[module]
name = "myapp"
epoch = "2026.0"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// No [require] section at all — should be a no-op.
	if err := RemoveRequire(path, "github.com/foo/bar"); err != nil {
		t.Fatalf("RemoveRequire returned error when no [require] section: %v", err)
	}
}

// --- T1524: subdir addressing on [require.NAME] entries ---

func TestNormalizeSubdir(t *testing.T) {
	ok := []struct{ in, want string }{
		{"", ""},
		{"a", "a"},
		{"a/b", "a/b"},
		{"./a/b", "a/b"},
		{"a/b/", "a/b"},
		{"./a/b/", "a/b"},
		{`a\b`, "a/b"},
		{"proto/wire-types", "proto/wire-types"},
	}
	for _, tc := range ok {
		got, err := NormalizeSubdir(tc.in)
		if err != nil {
			t.Errorf("NormalizeSubdir(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeSubdir(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	bad := []string{"/abs", "/", "../esc", "a/../../b", "a//b", "a/b//", ".", "..", "./", "C:/x", `c:\x`, "a/./b"}
	for _, in := range bad {
		if got, err := NormalizeSubdir(in); err == nil {
			t.Errorf("NormalizeSubdir(%q) = %q, want error", in, got)
		}
	}
}

func TestParseConfigSubdir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	os.WriteFile(path, []byte(`[module]
name = "app"
epoch = "2026.1"

[require.wire]
url = "https://github.com/acme/base"
commit = "abc123"
subdir = "./proto/wire/"

[require.plain]
url = "https://github.com/acme/plain"
commit = "def456"
`), 0644)

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if got := cfg.NamedRequire["wire"].Subdir; got != "proto/wire" {
		t.Errorf("wire subdir = %q, want normalized %q", got, "proto/wire")
	}
	if got := cfg.NamedRequire["plain"].Subdir; got != "" {
		t.Errorf("plain subdir = %q, want empty (repo root)", got)
	}
}

func TestParseConfigInvalidSubdir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	os.WriteFile(path, []byte(`[module]
name = "app"

[require.wire]
url = "https://github.com/acme/base"
commit = "abc123"
subdir = "../escape"
`), 0644)

	_, err := ParseConfig(path)
	if err == nil {
		t.Fatal("expected an error for a subdir escaping the repo root")
	}
	if !strings.Contains(err.Error(), "invalid 'subdir'") {
		t.Errorf("error = %v, want it to mention invalid 'subdir'", err)
	}
}

// A '//' in a URL would collide with the subdir separator used by
// GlobalIdentityForRemote, letting two distinct modules share one identity.
func TestParseConfigRejectsDoubleSlashURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	os.WriteFile(path, []byte(`[module]
name = "app"

[require.wire]
url = "https://github.com/acme//base"
commit = "abc123"
`), 0644)

	_, err := ParseConfig(path)
	if err == nil {
		t.Fatal("expected an error for a URL with an empty path component")
	}
	if !strings.Contains(err.Error(), "invalid 'url'") {
		t.Errorf("error = %v, want it to mention invalid 'url'", err)
	}
}

// The flat [require] table needs the same guard: its key is a raw URL, and a user
// borrowing the `repo//subdir` spelling from other tooling would otherwise write a
// key whose identity aliases a [require.NAME] entry with a subdir — the two would
// then share a commit pin and an IR prefix.
func TestParseConfigRejectsDoubleSlashURLInFlatRequire(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	os.WriteFile(path, []byte(`[module]
name = "app"

[require]
"https://github.com/acme/base//proto/wire" = "abc123"
`), 0644)

	_, err := ParseConfig(path)
	if err == nil {
		t.Fatal("expected an error for a flat require URL with an empty path component")
	}
	if !strings.Contains(err.Error(), "[require] invalid url") {
		t.Errorf("error = %v, want it to mention the flat [require] url", err)
	}
	// The message should point at the supported way to address a subdir module.
	if !strings.Contains(err.Error(), "subdir") {
		t.Errorf("error = %v, want it to mention the 'subdir' field", err)
	}
}

// The guard exists to keep identities distinct: had the '//' URL been accepted, it
// would have produced exactly the identity of the named entry below.
func TestDoubleSlashURLWouldAliasSubdirIdentity(t *testing.T) {
	flat := NormalizeURL("https://github.com/acme/base//proto/wire")
	named := GlobalIdentityForRemote(NormalizeURL("https://github.com/acme/base"), "proto/wire")
	if flat != named {
		t.Fatalf("expected the collision this guard prevents; got %q vs %q", flat, named)
	}
	if err := CheckURLIdentitySafe("https://github.com/acme/base//proto/wire"); err == nil {
		t.Error("CheckURLIdentitySafe accepted a URL that aliases a subdir identity")
	}
	if err := CheckURLIdentitySafe("https://github.com/acme/base"); err != nil {
		t.Errorf("CheckURLIdentitySafe rejected an ordinary URL: %v", err)
	}
}

func TestSetNamedRequireCreatesSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	original := `# project manifest
[module]
name = "app"
epoch = "2026.1"
`
	os.WriteFile(path, []byte(original), 0644)

	if err := SetNamedRequire(path, "wire", "https://github.com/acme/base", "abc123", "proto/wire"); err != nil {
		t.Fatalf("SetNamedRequire: %v", err)
	}
	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatalf("ParseConfig after create: %v", err)
	}
	e := cfg.NamedRequire["wire"]
	if e == nil || e.URL != "https://github.com/acme/base" || e.Commit != "abc123" || e.Subdir != "proto/wire" {
		t.Fatalf("entry after create = %+v", e)
	}
	if !strings.Contains(string(mustRead(t, path)), "# project manifest") {
		t.Error("SetNamedRequire dropped the leading comment")
	}

	// Updating an existing section rewrites the fields in place.
	if err := SetNamedRequire(path, "wire", "https://github.com/acme/base", "def456", "proto/wire2"); err != nil {
		t.Fatalf("SetNamedRequire update: %v", err)
	}
	cfg, err = ParseConfig(path)
	if err != nil {
		t.Fatalf("ParseConfig after update: %v", err)
	}
	if e := cfg.NamedRequire["wire"]; e.Commit != "def456" || e.Subdir != "proto/wire2" {
		t.Fatalf("entry after update = %+v", e)
	}
	if n := strings.Count(string(mustRead(t, path)), "[require.wire]"); n != 1 {
		t.Errorf("[require.wire] appears %d times, want 1", n)
	}

	// Re-adding with no subdir removes the stale subdir line.
	if err := SetNamedRequire(path, "wire", "https://github.com/acme/base", "def456", ""); err != nil {
		t.Fatalf("SetNamedRequire clear subdir: %v", err)
	}
	cfg, err = ParseConfig(path)
	if err != nil {
		t.Fatalf("ParseConfig after clear: %v", err)
	}
	if e := cfg.NamedRequire["wire"]; e.Subdir != "" {
		t.Errorf("subdir = %q, want cleared", e.Subdir)
	}
}

// SetNamedRequireCommit must leave a sibling `subdir` line untouched — otherwise
// `promise package update` would silently re-point the entry at the repo root.
func TestSetNamedRequireCommitPreservesSubdir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	os.WriteFile(path, []byte(`[module]
name = "app"

[require.wire]
url = "https://github.com/acme/base"
commit = "abc123"
subdir = "proto/wire"

[replace]
"github.com/acme/base" = "../base"
`), 0644)

	if err := SetNamedRequireCommit(path, "wire", "999fff"); err != nil {
		t.Fatalf("SetNamedRequireCommit: %v", err)
	}
	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	e := cfg.NamedRequire["wire"]
	if e.Commit != "999fff" || e.Subdir != "proto/wire" {
		t.Fatalf("entry = %+v, want commit rewritten and subdir preserved", e)
	}
	if cfg.Replace["github.com/acme/base"] != "../base" {
		t.Error("[replace] section was disturbed")
	}
}

func TestSetNamedRequireCommitMissingSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	os.WriteFile(path, []byte("[module]\nname = \"app\"\n"), 0644)
	if err := SetNamedRequireCommit(path, "absent", "abc"); err == nil {
		t.Fatal("expected an error for a missing [require.NAME] section")
	}
}

func TestIsImportName(t *testing.T) {
	for _, s := range []string{"wire", "_x", "a1", "Wire"} {
		if !IsImportName(s) {
			t.Errorf("IsImportName(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "wire-types", "a.b", "1a", "a/b"} {
		if IsImportName(s) {
			t.Errorf("IsImportName(%q) = true, want false", s)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// T1524: `promise package add --subdir` writes named entries, so removal must be
// able to take one back out again — header, keys, and the separator that followed.
func TestRemoveNamedRequire(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	os.WriteFile(path, []byte(`# manifest
[module]
name = "app"
epoch = "2026.1"

[require.wire]
url = "https://github.com/acme/base"
commit = "abc123"
subdir = "proto/wire"

[require.types]
url = "https://github.com/acme/base"
commit = "abc123"
subdir = "proto/types"

[replace]
"github.com/acme/base" = "../base"
`), 0644)

	removed, err := RemoveNamedRequire(path, "wire")
	if err != nil {
		t.Fatalf("RemoveNamedRequire: %v", err)
	}
	if !removed {
		t.Fatal("expected the section to be found")
	}

	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatalf("ParseConfig after remove: %v", err)
	}
	if _, ok := cfg.NamedRequire["wire"]; ok {
		t.Error("[require.wire] survived removal")
	}
	if e := cfg.NamedRequire["types"]; e == nil || e.Subdir != "proto/types" {
		t.Errorf("sibling entry disturbed: %+v", e)
	}
	if cfg.Replace["github.com/acme/base"] != "../base" {
		t.Error("[replace] section was disturbed")
	}
	data := string(mustRead(t, path))
	if !strings.Contains(data, "# manifest") {
		t.Error("leading comment was dropped")
	}
	if strings.Contains(data, "proto/wire") {
		t.Errorf("removed section left content behind:\n%s", data)
	}

	// Removing again is a no-op, not an error.
	removed, err = RemoveNamedRequire(path, "wire")
	if err != nil || removed {
		t.Errorf("second remove = %v, %v; want false, nil", removed, err)
	}
}

// A section that runs to EOF is removed without eating the file's final newline.
func TestRemoveNamedRequireAtEOF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	os.WriteFile(path, []byte(`[module]
name = "app"

[require.wire]
url = "https://github.com/acme/base"
commit = "abc123"
`), 0644)

	if _, err := RemoveNamedRequire(path, "wire"); err != nil {
		t.Fatalf("RemoveNamedRequire: %v", err)
	}
	if got, want := string(mustRead(t, path)), "[module]\nname = \"app\"\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

// ParseConfig is last-wins on a duplicated key, so a write must rewrite the LAST
// assignment and drop the earlier ones — otherwise `promise package update` would
// report success while the effective pin never moved.
func TestSetNamedRequireFieldsDuplicateKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	os.WriteFile(path, []byte(`[module]
name = "app"

[require.wire]
url = "https://github.com/acme/base"
commit = "old111"
subdir = "proto/stale"
commit = "old222"
subdir = "proto/wire"
`), 0644)

	if err := SetNamedRequireCommit(path, "wire", "new333"); err != nil {
		t.Fatalf("SetNamedRequireCommit: %v", err)
	}
	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if got := cfg.NamedRequire["wire"].Commit; got != "new333" {
		t.Errorf("commit = %q, want the rewritten value", got)
	}
	data := string(mustRead(t, path))
	if n := strings.Count(data, "commit ="); n != 1 {
		t.Errorf("duplicate commit lines survived (%d):\n%s", n, data)
	}
	// The untouched key keeps its effective (last) value.
	if got := cfg.NamedRequire["wire"].Subdir; got != "proto/wire" {
		t.Errorf("subdir = %q, want the effective value preserved", got)
	}

	// Clearing a duplicated key must remove every occurrence, not just one.
	if err := SetNamedRequire(path, "wire", "https://github.com/acme/base", "new333", ""); err != nil {
		t.Fatalf("SetNamedRequire clear: %v", err)
	}
	cfg, err = ParseConfig(path)
	if err != nil {
		t.Fatalf("ParseConfig after clear: %v", err)
	}
	if got := cfg.NamedRequire["wire"].Subdir; got != "" {
		t.Errorf("subdir = %q, want every occurrence cleared", got)
	}
	if strings.Contains(string(mustRead(t, path)), "subdir") {
		t.Errorf("a subdir line survived:\n%s", mustRead(t, path))
	}
}

// `promise package add --name w <url>` (no --subdir) creates a fresh
// [require.NAME] section for a repo-root module: the empty subdir must be skipped
// entirely rather than written as `subdir = ""`, which NormalizeSubdir would then
// have to special-case on the way back in.
func TestSetNamedRequireCreatesSectionWithoutSubdir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	os.WriteFile(path, []byte("[module]\nname = \"app\"\nepoch = \"2026.1\"\n"), 0644)

	if err := SetNamedRequire(path, "wire", "https://github.com/acme/wire", "abc123", ""); err != nil {
		t.Fatalf("SetNamedRequire: %v", err)
	}
	text := string(mustRead(t, path))
	if strings.Contains(text, "subdir") {
		t.Errorf("an empty subdir must write no line, got:\n%s", text)
	}
	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	e := cfg.NamedRequire["wire"]
	if e == nil || e.URL != "https://github.com/acme/wire" || e.Commit != "abc123" || e.Subdir != "" {
		t.Fatalf("entry = %+v", e)
	}
}

// A missing manifest is a read error, not a "section not found" no-op — removal is
// idempotent only for a file that exists.
func TestRemoveNamedRequireReadError(t *testing.T) {
	removed, err := RemoveNamedRequire(filepath.Join(t.TempDir(), "absent.toml"), "wire")
	if err == nil {
		t.Fatal("expected an error for a missing promise.toml")
	}
	if removed {
		t.Error("removed = true for a file that does not exist")
	}
}

// Removing a named entry must be idempotent: a second removal reports "not found"
// without an error, and leaves the rest of the manifest intact.
func TestRemoveNamedRequireIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promise.toml")
	os.WriteFile(path, []byte(`[module]
name = "app"
epoch = "2026.0"

[require.wire]
url = "https://github.com/acme/base"
commit = "abc123"
subdir = "proto/wire"
`), 0644)

	for i, wantRemoved := range []bool{true, false} {
		removed, err := RemoveNamedRequire(path, "wire")
		if err != nil {
			t.Fatalf("removal %d: %v", i, err)
		}
		if removed != wantRemoved {
			t.Errorf("removal %d: removed = %v, want %v", i, removed, wantRemoved)
		}
	}
	cfg, err := ParseConfig(path)
	if err != nil {
		t.Fatalf("ParseConfig after double removal: %v", err)
	}
	if cfg.Name != "app" {
		t.Errorf("manifest damaged, [module] name = %q", cfg.Name)
	}
	if len(cfg.NamedRequire) != 0 {
		t.Errorf("NamedRequire = %+v, want empty", cfg.NamedRequire)
	}
}

// NormalizeSubdir is the only guard between a manifest and a path escaping the
// checkout, so the rejected spellings are pinned explicitly. Windows-style
// separators are accepted and folded, since a manifest may be hand-written there.
func TestNormalizeSubdirRejectsEscapes(t *testing.T) {
	for _, bad := range []string{
		"..",
		"../sibling",
		"proto/../../etc",
		"proto/./wire",
		"/abs/path",
		"C:/repo/proto",
		`C:\repo\proto`,
		`\\server\share`,
		"proto//wire",
		"./",
		"/",
	} {
		if got, err := NormalizeSubdir(bad); err == nil {
			t.Errorf("NormalizeSubdir(%q) = %q, want an error", bad, got)
		}
	}

	for in, want := range map[string]string{
		"":                    "",
		"proto/wire":          "proto/wire",
		"./proto/wire":        "proto/wire",
		"proto/wire/":         "proto/wire",
		`proto\wire`:          "proto/wire",
		`.\proto\wire\`:       "proto/wire",
		"a/b/c/d":             "a/b/c/d",
		"dir with spaces/mod": "dir with spaces/mod",
		"..hidden/mod":        "..hidden/mod", // a component *starting* with dots is fine
	} {
		got, err := NormalizeSubdir(in)
		if err != nil {
			t.Errorf("NormalizeSubdir(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeSubdir(%q) = %q, want %q", in, got, want)
		}
	}
}

// A normalized subdir must be a fixed point — `promise package add` normalizes
// before writing and ParseConfig normalizes on the way back in, so a second pass
// over an already-canonical value must not change it.
func TestNormalizeSubdirIdempotent(t *testing.T) {
	for _, in := range []string{"", "proto/wire", "./a/b/", `a\b\c`} {
		once, err := NormalizeSubdir(in)
		if err != nil {
			t.Fatalf("NormalizeSubdir(%q): %v", in, err)
		}
		twice, err := NormalizeSubdir(once)
		if err != nil {
			t.Fatalf("NormalizeSubdir(%q) (second pass): %v", once, err)
		}
		if once != twice {
			t.Errorf("NormalizeSubdir not idempotent for %q: %q → %q", in, once, twice)
		}
	}
}
