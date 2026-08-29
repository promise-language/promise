package pkgmgr

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

func TestUpdateURLKeyedEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	t.Parallel()
	cli := clitest.NewEnv(t)
	epoch := cli.CompilerEpoch(t)

	// Create a local bare git repo with two module commits; the newer one carries
	// the epoch-<E> tag the resolver should re-resolve to (§9.8 step 3).
	bareDir := filepath.ToSlash(clitest.ShortRepoDir(t))
	workDir := clitest.ShortRepoDir(t)
	projDir := t.TempDir()

	run := func(dir, name string, args ...string) { cli.Run(t, dir, name, args...) }

	run(bareDir, "git", "init", "--bare", ".")
	run(workDir, "git", "clone", bareDir, ".")
	clitest.WriteModule(t, workDir, "dep", "2026.0", true)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "first")
	run(workDir, "git", "push", "origin", "HEAD")

	oldHash := cli.Git(t, workDir, "rev-parse", "HEAD")

	// Second commit — tagged epoch-<E>; update should re-resolve here.
	os.WriteFile(filepath.Join(workDir, "extra.pr"), []byte("extra_value() int `public { return 9; }\n"), 0644)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "second")
	run(workDir, "git", "tag", "epoch-"+epoch)
	run(workDir, "git", "push", "origin", "HEAD", "--tags")

	headHash := cli.Git(t, workDir, "rev-parse", "HEAD")

	// Create promise.toml with URL-keyed require pinned to old commit
	toml := "[module]\nname = \"proj\"\nepoch = \"" + epoch + "\"\n\n[require]\n\"" + bareDir + "\" = \"" + oldHash + "\"\n"
	os.WriteFile(filepath.Join(projDir, "promise.toml"), []byte(toml), 0644)

	out := cli.PromiseOK(t, projDir, "package", "update")
	if !strings.Contains(out, headHash[:12]) {
		t.Errorf("expected new hash %s in output, got: %s", headHash[:12], out)
	}
	if !strings.Contains(out, "Updated 1 of 1") {
		t.Errorf("expected 'Updated 1 of 1', got: %s", out)
	}

	// Verify promise.toml was updated
	content, _ := os.ReadFile(filepath.Join(projDir, "promise.toml"))
	if !strings.Contains(string(content), headHash) {
		t.Errorf("promise.toml should contain new hash %s, got: %s", headHash, string(content))
	}
}

func TestUpdateSpecificTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	t.Parallel()
	cli := clitest.NewEnv(t)
	epoch := cli.CompilerEpoch(t)

	// Test updating a specific URL-keyed entry by URL
	bareDir := filepath.ToSlash(clitest.ShortRepoDir(t))
	workDir := clitest.ShortRepoDir(t)
	projDir := t.TempDir()

	run := func(dir, name string, args ...string) { cli.Run(t, dir, name, args...) }

	run(bareDir, "git", "init", "--bare", ".")
	run(workDir, "git", "clone", bareDir, ".")
	clitest.WriteModule(t, workDir, "dep", "2026.0", true)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "init")
	run(workDir, "git", "tag", "epoch-"+epoch)
	run(workDir, "git", "push", "origin", "HEAD", "--tags")

	headHash := cli.Git(t, workDir, "rev-parse", "HEAD")

	// Pin to the epoch-<E> commit already — re-resolution lands on the same commit
	// → "already up to date".
	toml := "[module]\nname = \"proj\"\nepoch = \"" + epoch + "\"\n\n[require]\n\"" + bareDir + "\" = \"" + headHash + "\"\n"
	os.WriteFile(filepath.Join(projDir, "promise.toml"), []byte(toml), 0644)

	out := cli.PromiseOK(t, projDir, "package", "update", bareDir)
	if !strings.Contains(out, "already up to date") {
		t.Errorf("expected 'already up to date', got: %s", out)
	}
}

func TestAddWithCustomRef(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	t.Parallel()
	cli := clitest.NewEnv(t)
	epoch := cli.CompilerEpoch(t)

	// Test the len(args)==2 path with a custom ref: the ref is resolved + verified
	// with no epoch-tag walk-back (the user pinned it).
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

	expectedHash := cli.Git(t, workDir, "rev-parse", "HEAD")

	os.WriteFile(filepath.Join(projDir, "promise.toml"), []byte("[module]\nname = \"proj\"\nepoch = \""+epoch+"\"\n"), 0644)

	// A bareDir path never matches a catalog name, so it is treated as a raw URL.
	out := cli.PromiseOK(t, projDir, "package", "add", bareDir, "v1.0")
	if !strings.Contains(out, "Added") {
		t.Errorf("expected 'Added' in output, got: %s", out)
	}
	if !strings.Contains(out, expectedHash[:12]) {
		t.Errorf("expected hash %s in output, got: %s", expectedHash[:12], out)
	}

	// Verify promise.toml
	content, _ := os.ReadFile(filepath.Join(projDir, "promise.toml"))
	if !strings.Contains(string(content), expectedHash) {
		t.Errorf("promise.toml should contain commit hash, got: %s", string(content))
	}
}

func TestUpdateNamedEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	t.Parallel()
	cli := clitest.NewEnv(t)
	epoch := cli.CompilerEpoch(t)

	// Test updating a [require.NAME] entry when the epoch-<E> tag has moved forward
	bareDir := filepath.ToSlash(clitest.ShortRepoDir(t))
	workDir := clitest.ShortRepoDir(t)
	projDir := t.TempDir()

	run := func(dir, name string, args ...string) { cli.Run(t, dir, name, args...) }

	run(bareDir, "git", "init", "--bare", ".")
	run(workDir, "git", "clone", bareDir, ".")
	clitest.WriteModule(t, workDir, "dep", "2026.0", true)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "first")
	run(workDir, "git", "push", "origin", "HEAD")

	oldHash := cli.Git(t, workDir, "rev-parse", "HEAD")

	// Second commit — carries the epoch-<E> tag the update should re-resolve to.
	os.WriteFile(filepath.Join(workDir, "extra.pr"), []byte("extra_value() int `public { return 9; }\n"), 0644)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "second")
	run(workDir, "git", "tag", "epoch-"+epoch)
	run(workDir, "git", "push", "origin", "HEAD", "--tags")

	headHash := cli.Git(t, workDir, "rev-parse", "HEAD")

	// Create promise.toml with [require.mymod] pinned to old commit
	toml := "[module]\nname = \"proj\"\nepoch = \"" + epoch + "\"\n\n[require.mymod]\nurl = \"" + bareDir + "\"\ncommit = \"" + oldHash + "\"\n"
	os.WriteFile(filepath.Join(projDir, "promise.toml"), []byte(toml), 0644)

	out := cli.PromiseOK(t, projDir, "package", "update")
	if !strings.Contains(out, headHash[:12]) {
		t.Errorf("expected new hash %s in output, got: %s", headHash[:12], out)
	}
	if !strings.Contains(out, "Updated 1 of 1") {
		t.Errorf("expected 'Updated 1 of 1', got: %s", out)
	}

	// Verify promise.toml was updated
	content, _ := os.ReadFile(filepath.Join(projDir, "promise.toml"))
	if !strings.Contains(string(content), headHash) {
		t.Errorf("promise.toml should contain new hash %s, got: %s", headHash, string(content))
	}
}

func TestUpdateAlreadyCurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	t.Parallel()
	cli := clitest.NewEnv(t)
	epoch := cli.CompilerEpoch(t)

	// Create a local bare git repo
	bareDir := filepath.ToSlash(clitest.ShortRepoDir(t))
	workDir := clitest.ShortRepoDir(t)
	projDir := t.TempDir()

	run := func(dir, name string, args ...string) { cli.Run(t, dir, name, args...) }

	run(bareDir, "git", "init", "--bare", ".")
	run(workDir, "git", "clone", bareDir, ".")
	clitest.WriteModule(t, workDir, "dep", "2026.0", true)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "init")
	run(workDir, "git", "tag", "epoch-"+epoch)
	run(workDir, "git", "push", "origin", "HEAD", "--tags")

	// Get HEAD hash
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = workDir
	hashBytes, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	headHash := strings.TrimSpace(string(hashBytes))

	// Create promise.toml already pinned to the epoch-<E> commit
	toml := "[module]\nname = \"proj\"\nepoch = \"" + epoch + "\"\n\n[require]\n\"" + bareDir + "\" = \"" + headHash + "\"\n"
	os.WriteFile(filepath.Join(projDir, "promise.toml"), []byte(toml), 0644)

	out := cli.PromiseOK(t, projDir, "package", "update")
	if !strings.Contains(out, "already up to date") {
		t.Errorf("expected 'already up to date', got: %s", out)
	}
	if !strings.Contains(out, "Updated 0 of 1") {
		t.Errorf("expected 'Updated 0 of 1', got: %s", out)
	}
}
