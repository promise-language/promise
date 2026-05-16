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
