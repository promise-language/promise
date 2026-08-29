// Package clitest holds the helpers shared by cmd/promise's per-area test
// packages: locating the built compiler, and driving it as a subprocess under
// an isolated PROMISE_HOME and git configuration.
//
// It exists because cmd/promise's tests are split across packages, for the same
// reason codegen's are (T1776). 859 tests in one package took six minutes of
// wall clock here, and two thirds of that work — the tests that only run the
// built binary and assert on its output — needed nothing from package main at
// all. They were serialised behind it purely by living in the same directory.
//
// A test may only live in one of those area packages if it is black-box: a
// package main test can call runAdd or read embeddedCatalog directly, and one
// out here cannot, because Go will not let anything import package main. Tests
// that genuinely need the compiler's internals stay in package main; the ones
// that shell out to the binary move here, where they run as their own process
// and their own build-cache entry.
package clitest

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// Bin returns the path to the built promise binary, or skips the test when
// there is none. PROMISE_TEST_BIN wins when set; otherwise the repo root is
// found by walking up from the test's working directory, so this works at any
// package depth.
func Bin(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("PROMISE_TEST_BIN"); bin != "" {
		return bin
	}
	name := "promise"
	if runtime.GOOS == "windows" {
		name = "promise.exe"
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if bin := filepath.Join(dir, "bin", name); fileExists(bin) {
			return bin
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skip("set PROMISE_TEST_BIN or build via bin/build to run this end-to-end test")
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Env drives the built promise binary as a subprocess, with its own
// PROMISE_HOME and an isolated git configuration carried in each command's
// environment rather than the process's.
//
// Why not call runAdd/runPkgUpdate in-process: those need the working directory
// and the environment to point at the fixture, and os.Chdir and t.Setenv are
// process-global. A test that uses them cannot run beside any other, which is
// what made the package-manager tests the serial floor of this package (T1776).
// Passing dir and env per command instead costs a fork and buys parallelism.
//
// It also removes the need for the testVerifyCompilerBin hook: that exists
// because an in-process test's os.Executable() is the test binary rather than a
// compiler. A subprocess of the real binary is a real compiler, so the
// production code takes its normal path.
type Env struct {
	bin  string
	home string
	env  []string
}

// NewEnv builds an isolated environment around the compiler binary.
func NewEnv(t *testing.T) *Env {
	t.Helper()
	bin := Bin(t)
	home := t.TempDir()
	gitconfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(gitconfig,
		[]byte("[user]\n\temail = test@test.com\n\tname = Test\n[safe]\n\tdirectory = *\n"), 0644); err != nil {
		t.Fatalf("write git config: %v", err)
	}
	return &Env{bin: bin, home: home, env: append(os.Environ(),
		"PROMISE_HOME="+home,
		"GIT_CONFIG_GLOBAL="+gitconfig,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
	)}
}

// Run executes any command in dir under the isolated environment, failing the
// test if it does not succeed.
func (e *Env) Run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = e.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v in %s: %v\n%s", name, args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Git runs a git command in dir, returning its trimmed output.
func (e *Env) Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return e.Run(t, dir, "git", args...)
}

// Promise runs the compiler in dir and returns its combined output along with
// the exit error, for tests that assert on failure as well as success.
func (e *Env) Promise(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Dir = dir
	cmd.Env = e.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// PromiseOK runs the compiler in dir and fails the test if it does not succeed.
func (e *Env) PromiseOK(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := e.Promise(t, dir, args...)
	if err != nil {
		t.Fatalf("promise %v in %s: %v\n%s", args, dir, err, out)
	}
	return out
}

var catalogEpochRe = regexp.MustCompile(`epoch ([0-9]+\.[0-9]+)`)

// CompilerEpoch returns the epoch the binary's embedded catalog implements, so
// add/update tests can pin a project epoch that matches the verifying compiler
// (verification must run under the project epoch).
//
// package main reads embeddedCatalog for this; out here the binary's own
// `catalog list` header is the equivalent, and is the more honest source — it
// is the epoch of the compiler actually under test rather than of whatever
// catalog this test binary happened to embed.
func (e *Env) CompilerEpoch(t *testing.T) string {
	t.Helper()
	out, err := e.Promise(t, t.TempDir(), "catalog", "list")
	if err != nil {
		t.Skipf("cannot determine compiler epoch: %v\n%s", err, out)
	}
	return parseCatalogEpoch(t, out)
}

// CompilerEpoch is the Env-free form, for a test that drives the binary itself
// rather than through an Env.
func CompilerEpoch(t *testing.T) string {
	t.Helper()
	cmd := exec.Command(Bin(t), "catalog", "list")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "PROMISE_HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("cannot determine compiler epoch: %v\n%s", err, out)
	}
	return parseCatalogEpoch(t, string(out))
}

func parseCatalogEpoch(t *testing.T, out string) string {
	t.Helper()
	m := catalogEpochRe.FindStringSubmatch(out)
	if m == nil {
		t.Skipf("cannot determine compiler epoch from catalog listing:\n%s", out)
	}
	return m[1]
}

// MakeWorkRepo creates an initialised git working repo with a committer
// identity, rooted at a short path (see ShortRepoDir).
func MakeWorkRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(ShortRepoDir(t), "repo")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	GitRun(t, dir, "init", "--initial-branch=main")
	GitRun(t, dir, "config", "user.email", "t@t.com")
	GitRun(t, dir, "config", "user.name", "T")
	return dir
}

// WriteModule writes a minimal module (promise.toml + impl + a `test` file) into
// dir under the given epoch. When good is false the test file fails to compile
// (a parse error), making the commit incompatible under any epoch.
func WriteModule(t *testing.T, dir, name, epoch string, good bool) {
	t.Helper()
	write(t, filepath.Join(dir, "promise.toml"),
		"[module]\nname = \""+name+"\"\nepoch = \""+epoch+"\"\n")
	write(t, filepath.Join(dir, name+".pr"),
		name+"_value() int `public { return 1; }\n")
	body := "check() `test {\n  assert(" + name + "_value() == 1, \"ok\");\n}\n"
	if !good {
		// Genuine parse error → compile failure → incompatible (§9.9).
		body = "check() `test {\n  assert(" + name + "_value() == , \"x\");\n}\n"
	}
	write(t, filepath.Join(dir, name+"_test.pr"), body)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ShortRepoDir returns a short, unique, auto-removed directory for a local git
// repo that a test uses as a module "remote" URL. The module cache mirrors a
// module's URL into its own directory tree (see URLToCachePath), so a deep
// t.TempDir() path used as the URL would be doubled into the cache and blow past
// git's ~260-char path buffers on Windows ("Filename too long" / "$GIT_DIR too
// big"). Rooting these repos at a short path keeps the mirrored cache path
// within limits. Elsewhere t.TempDir() has no such limit, so it is used
// unchanged.
func ShortRepoDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		return t.TempDir()
	}
	base := filepath.Join(os.Getenv("SystemDrive")+`\`, "pt")
	if err := os.MkdirAll(base, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", base, err)
	}
	dir, err := os.MkdirTemp(base, "r")
	if err != nil {
		t.Fatalf("mkdtemp under %s: %v", base, err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// GitRun runs a git command in dir with prompting disabled, failing the test on
// error. Tests that already hold an Env should prefer Env.Git, which also
// isolates the git configuration; this is for building fixture repos before an
// Env exists.
func GitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// IsolateHome gives the calling test package its own PROMISE_HOME for the whole
// run, and is meant to be the body of a package's TestMain.
//
// Without it these packages share the developer's real cache with package main,
// whose test binary is not bin/promise: module.CompilerChanged stamps
// os.Executable(), so a test binary always looks like a freshly installed
// compiler and ensureCacheValid answers by wiping cache/llvm-view with an
// unsynchronized RemoveAll. Run serially that only cost a re-extraction, which
// is why it went unnoticed; run beside these packages it deletes a peer
// process's staging dir mid-write and the peer's build fails with a bare
// "no such file or directory".
//
// Isolation is also what makes them fast. Sharing one home serialises every
// concurrent compiler behind the same view lock and build-cache entries; with a
// home apiece the packages stop queueing behind each other.
//
// The wipe racing a peer is a real defect in the compiler rather than in these
// tests — two promise binaries sharing one PROMISE_HOME, say a dev build and an
// installed one, ping-pong the stamp and can kill each other's view
// materialization. It is the hazard T1616 removed for embedded modules and left
// in place for the LLVM and CRT views.
func IsolateHome(m interface{ Run() int }) int {
	home, err := os.MkdirTemp("", "promise-clitest-home-")
	if err != nil {
		panic("clitest: cannot create isolated PROMISE_HOME: " + err.Error())
	}
	if err := os.Setenv("PROMISE_HOME", home); err != nil {
		panic("clitest: cannot set PROMISE_HOME: " + err.Error())
	}
	code := m.Run()
	os.RemoveAll(home)
	return code
}

// MakeSubdirRepo builds a bare git repo with NO promise.toml at its root and one
// Promise module per entry in mods (name → repo-relative subdir) — the T1524
// shape: a repo that is not itself Promise-primary but contains Promise modules.
// Returns the bare repo path and the commit SHA.
func MakeSubdirRepo(t *testing.T, mods map[string]string) (bareRepo, commit string) {
	t.Helper()

	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatal(err)
	}
	GitRun(t, work, "init", "--initial-branch=main")
	GitRun(t, work, "config", "user.email", "test@test.com")
	GitRun(t, work, "config", "user.name", "Test")

	// Root marker for a non-Promise-primary repo — deliberately no promise.toml.
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte("module example.com/base\n"), 0644); err != nil {
		t.Fatal(err)
	}

	for name, sub := range mods {
		dir := filepath.Join(work, filepath.FromSlash(sub))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
			[]byte("[module]\nname = \""+name+"\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		src := "greet() string `public {\n  return \"" + name + "\";\n}\n"
		if err := os.WriteFile(filepath.Join(dir, name+".pr"), []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
	}

	GitRun(t, work, "add", ".")
	GitRun(t, work, "commit", "-m", "initial")
	commit = GitRun(t, work, "rev-parse", "HEAD")

	bareRepo = filepath.Join(t.TempDir(), "base.git")
	GitRun(t, "", "clone", "--bare", "--quiet", work, bareRepo)
	return bareRepo, commit
}
