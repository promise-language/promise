package common

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initGitRepo creates an empty git repo at dir with one commit, returning the
// full HEAD SHA. Shared by the provenance-stamping tests (T0854).
func initGitRepo(t *testing.T, dir string) string {
	t.Helper()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("config", "user.name", "test")
	git("config", "user.email", "test@test")
	if err := os.WriteFile(dir+"/seed.txt", []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "seed.txt")
	git("commit", "-m", "seed")

	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return string(out[:len(out)-1]) // strip trailing newline
}

// TestGitSHAFull: the function returns HEAD's full 40-char hex SHA in a git repo
// (matching `git rev-parse HEAD`), and the SHA must satisfy isFullGitSHA so the
// install gate accepts it as valid provenance extracted from
// `promise version --json`'s commit field (T0854, T1125).
func TestGitSHAFull(t *testing.T) {
	dir := t.TempDir()
	want := initGitRepo(t, dir)

	got := GitSHAFull(dir)
	if got != want {
		t.Errorf("GitSHAFull = %q, want %q", got, want)
	}
	if len(got) != 40 {
		t.Errorf("GitSHAFull returned %d chars, want 40: %q", len(got), got)
	}
	// The stamp must be a form the install gate's provenance check accepts.
	if !isFullGitSHA(got) {
		t.Errorf("isFullGitSHA(%q) = false; install gate would reject a real HEAD SHA", got)
	}
}

// TestGitSHAFull_NotARepo: outside a git repo GitSHAFull returns "" (not the
// "unknown" sentinel of the short GitSHA), so the install gate treats a binary
// built without git provenance as "no provenance" rather than feeding a sentinel
// to git cat-file (T0854).
func TestGitSHAFull_NotARepo(t *testing.T) {
	// Clear TMPDIR so t.TempDir() uses /tmp rather than .promise-home/tmp.
	// When bin/verify sets TMPDIR to the repo-internal .promise-home/tmp,
	// t.TempDir() lands inside the repo and GitSHAFull would find the repo's
	// .git by walking up the directory tree.
	t.Setenv("TMPDIR", "")
	dir := t.TempDir() // empty, no `git init`
	if got := GitSHAFull(dir); got != "" {
		t.Errorf("GitSHAFull(non-repo) = %q, want \"\"", got)
	}
}

// TestGitSHA: the short-SHA helper returns a 7-char lowercase-hex string in a
// git repo (a prefix of GitSHAFull), and the "unknown" sentinel outside one.
func TestGitSHA(t *testing.T) {
	dir := t.TempDir()
	full := initGitRepo(t, dir)

	got := GitSHA(dir)
	if len(got) != 7 {
		t.Errorf("GitSHA in repo = %q (%d chars), want 7", got, len(got))
	}
	for _, c := range got {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("GitSHA returned non-hex char %q in %q", string(c), got)
		}
	}
	if !strings.HasPrefix(full, got) {
		t.Errorf("GitSHA %q is not a prefix of GitSHAFull %q", got, full)
	}
}

func TestGitSHA_NotARepo(t *testing.T) {
	// Clear TMPDIR so t.TempDir() uses /tmp rather than .promise-home/tmp.
	// Same issue as TestGitSHAFull_NotARepo — when TMPDIR points inside the
	// repo, GitSHA finds the repo's .git by walking up.
	t.Setenv("TMPDIR", "")
	if got := GitSHA(t.TempDir()); got != "unknown" {
		t.Errorf("GitSHA(non-repo) = %q, want \"unknown\"", got)
	}
}

// TestBuildVersion: dev mode returns "epoch-sha7", release mode returns just
// "epoch". A missing catalog.toml causes an error.
func TestBuildVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "catalog.toml"), []byte("[catalog]\nepoch = \"2026.1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, dir)

	// Release mode: just the epoch.
	ver, err := BuildVersion(dir, true)
	if err != nil {
		t.Fatalf("BuildVersion(release) error: %v", err)
	}
	if ver != "2026.1" {
		t.Errorf("BuildVersion(release) = %q, want \"2026.1\"", ver)
	}

	// Dev mode: "epoch-sha7".
	ver, err = BuildVersion(dir, false)
	if err != nil {
		t.Fatalf("BuildVersion(dev) error: %v", err)
	}
	const prefix = "2026.1-"
	if !strings.HasPrefix(ver, prefix) || len(ver) != len(prefix)+7 {
		t.Errorf("BuildVersion(dev) = %q, want %q<7-char-sha>", ver, prefix)
	}

	// Missing catalog.toml → error propagated from ParseEpoch.
	if _, err := BuildVersion(t.TempDir(), false); err == nil {
		t.Error("BuildVersion with no catalog.toml = nil, want error")
	}
}

// TestReleaseVersion: a numeric epoch tag stamps the version from the TAG NAME,
// overriding whatever catalog.toml holds — the year-rollover promote-same-hash
// case (T1195). Non-epoch / epoch-next / empty / malformed refs fall back to the
// catalog.toml epoch.
func TestReleaseVersion(t *testing.T) {
	dir := t.TempDir()
	// catalog.toml holds the PRIOR-year dev epoch (the promote-same-hash state).
	if err := os.WriteFile(filepath.Join(dir, "catalog.toml"), []byte("[catalog]\nepoch = \"2026.4\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		tag  string
		want string
	}{
		// The bug: rollover tag must win over the prior-year catalog epoch.
		{"rollover", "epoch-2027.0", "2027.0"},
		// Same-year tag agrees with the catalog epoch.
		{"same-year", "epoch-2026.4", "2026.4"},
		// epoch-next → fall back to catalog (the dev epoch).
		{"epoch-next", "epoch-next", "2026.4"},
		// Non-epoch ref (workflow_dispatch branch) → fall back.
		{"branch", "main", "2026.4"},
		// Empty ref → fall back.
		{"empty", "", "2026.4"},
		// Malformed epoch tag → fall back rather than error.
		{"malformed", "epoch-2027.x", "2026.4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReleaseVersion(dir, tc.tag)
			if err != nil {
				t.Fatalf("ReleaseVersion(%q) error: %v", tc.tag, err)
			}
			if got != tc.want {
				t.Errorf("ReleaseVersion(%q) = %q, want %q", tc.tag, got, tc.want)
			}
		})
	}
}

// TestEpochFromTag: the tag-name parser normalizes numeric epoch tags and
// rejects everything else (so ReleaseVersion falls back to catalog.toml).
func TestEpochFromTag(t *testing.T) {
	cases := []struct {
		tag    string
		want   string
		wantOK bool
	}{
		{"epoch-2027.0", "2027.0", true},
		{"  epoch-2026.4  ", "2026.4", true}, // surrounding whitespace trimmed
		{"epoch-2027.00", "2027.0", true},    // normalized via epoch.String()
		{"epoch-next", "", false},
		{"main", "", false},
		{"", "", false},
		{"epoch-", "", false},
		{"epoch-2027", "", false},   // missing .N
		{"epoch-2027.x", "", false}, // non-numeric N
		{"epoch-x.0", "", false},    // non-numeric year
	}
	for _, tc := range cases {
		got, ok := epochFromTag(tc.tag)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("epochFromTag(%q) = (%q, %v), want (%q, %v)", tc.tag, got, ok, tc.want, tc.wantOK)
		}
	}
}
