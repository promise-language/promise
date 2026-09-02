package pkgmgr

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
	"github.com/promise-language/promise/compiler/internal/module"
)

// makeTaggedModuleRepo creates a bare git "remote" carrying a verifiable module
// (good=true → compiles + tests pass) tagged epoch-<epoch>. Returns (bareDir,
// headCommit).
func makeTaggedModuleRepo(t *testing.T, cli *clitest.Env, name, epoch string, good bool) (string, string) {
	t.Helper()
	bareDir := filepath.ToSlash(clitest.ShortRepoDir(t))
	workDir := clitest.ShortRepoDir(t)
	run := func(dir string, args ...string) { cli.Git(t, dir, args...) }
	run(bareDir, "init", "--bare", ".")
	run(workDir, "clone", bareDir, ".")
	clitest.WriteModule(t, workDir, name, epoch, good)
	run(workDir, "add", ".")
	run(workDir, "commit", "-m", "init")
	run(workDir, "tag", "epoch-"+epoch)
	run(workDir, "push", "origin", "HEAD", "--tags")
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return bareDir, strings.TrimSpace(string(out))
}

func TestRunPackageCheckEpoch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	t.Parallel()
	cli := clitest.NewEnv(t)
	epoch := cli.CompilerEpoch(t)

	modDir := t.TempDir()
	clitest.WriteModule(t, modDir, "lib", epoch, true)
	out := cli.PromiseOK(t, modDir, "package", "check-epoch", epoch)
	if !strings.Contains(out, "✓ compatible") || !strings.Contains(out, "git tag epoch-"+epoch) {
		t.Errorf("expected pass + tag hint, got: %s", out)
	}
}

func TestRunPackageBuildIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	t.Parallel()
	cli := clitest.NewEnv(t)
	epoch := cli.CompilerEpoch(t)

	bareDir, headCommit := makeTaggedModuleRepo(t, cli, "mymod", epoch, true)

	catalogDir := t.TempDir()
	os.WriteFile(filepath.Join(catalogDir, "modules.toml"),
		[]byte("[modules.mymod]\nurl = \""+bareDir+"\"\n"), 0644)

	out := cli.PromiseOK(t, t.TempDir(), "package", "build-index", catalogDir, epoch)
	if !strings.Contains(out, "✓ mymod") {
		t.Errorf("expected '✓ mymod', got: %s", out)
	}

	idx, err := module.LoadCompatIndex(catalogDir, epoch)
	if err != nil || idx == nil {
		t.Fatalf("index: %v", err)
	}
	e, ok := idx.Verified("mymod")
	if !ok || e.Commit != headCommit {
		t.Errorf("indexed commit = %+v (want %s)", e, headCommit)
	}
	if e.Tag != "epoch-"+epoch {
		t.Errorf("indexed tag = %q", e.Tag)
	}

	matrix, _ := os.ReadFile(filepath.Join(catalogDir, "matrix.md"))
	if !strings.Contains(string(matrix), "mymod") || !strings.Contains(string(matrix), "✓") {
		t.Errorf("matrix.md missing module/✓: %s", matrix)
	}
}

// TestRunPackageBuildIndexReport: a module that fails verification is absent from
// the index and listed as unsupported; with -report the run does not exit-fail
// (the §9.10 pre-release nudge).
func TestRunPackageBuildIndexReport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	t.Parallel()
	cli := clitest.NewEnv(t)
	epoch := cli.CompilerEpoch(t)

	bareDir, _ := makeTaggedModuleRepo(t, cli, "brokenmod", epoch, false) // fails to compile

	catalogDir := t.TempDir()
	os.WriteFile(filepath.Join(catalogDir, "modules.toml"),
		[]byte("[modules.brokenmod]\nurl = \""+bareDir+"\"\n"), 0644)

	out := cli.PromiseOK(t, t.TempDir(), "package", "build-index", catalogDir, epoch, "-report")
	if !strings.Contains(out, "✗ brokenmod") || !strings.Contains(out, "pre-release report") {
		t.Errorf("expected ✗ + report note, got: %s", out)
	}
	idx, err := module.LoadCompatIndex(catalogDir, epoch)
	if err != nil || idx == nil {
		t.Fatalf("index: %v", err)
	}
	if _, ok := idx.Verified("brokenmod"); ok {
		t.Error("failed module should be absent from the index")
	}
}
