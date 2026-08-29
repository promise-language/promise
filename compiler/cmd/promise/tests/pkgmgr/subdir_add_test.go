package pkgmgr

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
	"github.com/promise-language/promise/compiler/internal/module"
)

// T1524: `promise package add --subdir` writes a [require.NAME] section carrying
// url, commit and subdir, and verifies the addressed module (not the repo root).
func TestAddWithSubdir(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	t.Parallel()
	cli := clitest.NewEnv(t)
	epoch := cli.CompilerEpoch(t)

	bareDir := filepath.ToSlash(clitest.ShortRepoDir(t))
	workDir := clitest.ShortRepoDir(t)
	projDir := t.TempDir()

	run := func(dir, name string, args ...string) { cli.Run(t, dir, name, args...) }

	run(bareDir, "git", "init", "--bare", ".")
	run(workDir, "git", "clone", bareDir, ".")
	// The module lives in proto/wire; the repo root has no promise.toml.
	sub := filepath.Join(workDir, "proto", "wire")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	clitest.WriteModule(t, sub, "wire", "2026.0", true)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "first")
	run(workDir, "git", "tag", "v1.0")
	run(workDir, "git", "push", "origin", "HEAD", "--tags")

	if err := os.WriteFile(filepath.Join(projDir, "promise.toml"),
		[]byte("[module]\nname = \"proj\"\nepoch = \""+epoch+"\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := cli.PromiseOK(t, projDir, "package", "add", "--subdir", "proto/wire", bareDir, "v1.0")
	if !strings.Contains(out, "use wire;") {
		t.Errorf("expected the import hint in output, got: %s", out)
	}

	cfg, err := module.ParseConfig(filepath.Join(projDir, "promise.toml"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	e := cfg.NamedRequire["wire"]
	if e == nil {
		t.Fatalf("expected a [require.wire] entry, got %+v", cfg.NamedRequire)
	}
	if e.Subdir != "proto/wire" {
		t.Errorf("subdir = %q, want %q", e.Subdir, "proto/wire")
	}
	if e.Commit == "" {
		t.Error("expected a pinned commit")
	}
	if len(cfg.Require) != 0 {
		t.Errorf("--subdir must not write a flat [require] entry, got %v", cfg.Require)
	}
}

// Two modules in one repo are pinned independently, so a project may sit on
// different commits for each. The bare repo is shared but checkouts are keyed on
// (url, commit), so both must resolve — and each must see the source from *its*
// commit, not from whichever one was fetched first (T1524).
func TestUpdateNamedSubdirEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("local repo paths contain ':' which is invalid in Windows cache paths")
	}
	t.Parallel()
	cli := clitest.NewEnv(t)
	epoch := cli.CompilerEpoch(t)

	bareDir := filepath.ToSlash(clitest.ShortRepoDir(t))
	workDir := clitest.ShortRepoDir(t)
	projDir := t.TempDir()

	run := func(dir, name string, args ...string) { cli.Run(t, dir, name, args...) }
	headOf := func(dir string) string { return cli.Git(t, dir, "rev-parse", "HEAD") }

	run(bareDir, "git", "init", "--bare", ".")
	run(workDir, "git", "clone", bareDir, ".")
	// The repo root deliberately has no promise.toml — only proto/wire does.
	sub := filepath.Join(workDir, "proto", "wire")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	clitest.WriteModule(t, sub, "wire", "2026.0", true)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "first")
	run(workDir, "git", "push", "origin", "HEAD")
	oldHash := headOf(workDir)

	// Second commit carries the epoch tag the update should move to.
	if err := os.WriteFile(filepath.Join(sub, "extra.pr"),
		[]byte("extra_value() int `public { return 9; }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "second")
	run(workDir, "git", "tag", "epoch-"+epoch)
	run(workDir, "git", "push", "origin", "HEAD", "--tags")
	headHash := headOf(workDir)

	toml := "[module]\nname = \"proj\"\nepoch = \"" + epoch + "\"\n\n" +
		"[require.wire]\nurl = \"" + bareDir + "\"\ncommit = \"" + oldHash + "\"\nsubdir = \"proto/wire\"\n"
	if err := os.WriteFile(filepath.Join(projDir, "promise.toml"), []byte(toml), 0644); err != nil {
		t.Fatal(err)
	}

	out := cli.PromiseOK(t, projDir, "package", "update")
	if !strings.Contains(out, "Updated 1 of 1") {
		t.Fatalf("expected 'Updated 1 of 1', got: %s", out)
	}

	cfg, err := module.ParseConfig(filepath.Join(projDir, "promise.toml"))
	if err != nil {
		t.Fatalf("ParseConfig after update: %v", err)
	}
	e := cfg.NamedRequire["wire"]
	if e == nil {
		t.Fatalf("[require.wire] vanished: %+v", cfg.NamedRequire)
	}
	if e.Commit != headHash {
		t.Errorf("commit = %q, want the epoch-tagged head %q", e.Commit, headHash)
	}
	if e.Subdir != "proto/wire" {
		t.Errorf("subdir = %q, want it preserved across the update", e.Subdir)
	}
}

// `promise package add --name w <url>` (no --subdir) writes a named entry for a
// repo-ROOT module: the same [require.NAME] shape, minus the subdir line. This is
// how a dependency gets a stable import name that differs from its URL.
func TestAddNamedWithoutSubdir(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("local repo paths contain ':' which is invalid in Windows cache paths")
	}
	t.Parallel()
	cli := clitest.NewEnv(t)
	epoch := cli.CompilerEpoch(t)

	bareDir := filepath.ToSlash(clitest.ShortRepoDir(t))
	workDir := clitest.ShortRepoDir(t)
	projDir := t.TempDir()

	run := func(dir, name string, args ...string) { cli.Run(t, dir, name, args...) }

	run(bareDir, "git", "init", "--bare", ".")
	run(workDir, "git", "clone", bareDir, ".")
	clitest.WriteModule(t, workDir, "dep", "2026.0", true)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "first")
	run(workDir, "git", "tag", "v1.0")
	run(workDir, "git", "push", "origin", "HEAD", "--tags")

	if err := os.WriteFile(filepath.Join(projDir, "promise.toml"),
		[]byte("[module]\nname = \"proj\"\nepoch = \""+epoch+"\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := cli.PromiseOK(t, projDir, "package", "add", "--name", "helper", bareDir, "v1.0")
	if !strings.Contains(out, "use helper;") {
		t.Errorf("expected the import hint to use the --name value, got: %s", out)
	}

	text, err := os.ReadFile(filepath.Join(projDir, "promise.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(text), "subdir") {
		t.Errorf("a root-addressed named entry must write no subdir line:\n%s", text)
	}
	cfg, err := module.ParseConfig(filepath.Join(projDir, "promise.toml"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	e := cfg.NamedRequire["helper"]
	if e == nil || e.Commit == "" || e.Subdir != "" {
		t.Fatalf("entry = %+v, want a pinned root-addressed named entry", e)
	}
	if len(cfg.Require) != 0 {
		t.Errorf("--name must not write a flat [require] entry, got %v", cfg.Require)
	}
}
