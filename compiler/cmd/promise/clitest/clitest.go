// Package clitest holds the helpers shared by cmd/promise's per-area test
// packages: locating the built compiler, and driving it as a bounded subprocess
// under a known PROMISE_HOME and git configuration.
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
	"errors"
	"fmt"
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
//
// The binary must also be current with the tree. Before T2137 this checked only
// that a file existed there, so a compiler predating the source under test
// answered for it and the suite reported a product defect that was really a
// missing rebuild (T2134). A stale binary now fails the test and names
// bin/build; see stale.go for why the check is deliberately no stricter than
// bin/build's own rule.
//
// PROMISE_TEST_BIN bypasses the freshness check outright: an explicitly named
// binary is the caller's declared intent, and an installed or published
// compiler is not expected to match the worktree.
func Bin(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	bin, err := resolveBin(dir, os.Getenv("PROMISE_TEST_BIN"))
	switch {
	case errors.Is(err, errNoBinary):
		// A missing binary is visibly absent and cannot produce a false
		// verdict, so it stays a skip; a stale one silently produces a wrong
		// one, which is why that case fails instead.
		t.Skip(err.Error())
	case err != nil:
		t.Fatal(err)
	}
	return bin
}

// errNoBinary means no compiler is available to test against — the skip case.
var errNoBinary = errors.New("set PROMISE_TEST_BIN or build via bin/build to run this end-to-end test")

// resolveBin is Bin's decision without the *testing.T, so every branch of it is
// reachable from a unit test: the env override, the two no-binary skips, and
// the staleness refusal.
func resolveBin(startDir, envBin string) (string, error) {
	if envBin != "" {
		if !fileExists(envBin) {
			return "", fmt.Errorf("PROMISE_TEST_BIN names a binary that does not exist: %s", envBin)
		}
		return envBin, nil
	}
	root, ok := repoRootFrom(startDir)
	if !ok {
		return "", errNoBinary
	}
	bin := filepath.Join(root, "bin", binaryName())
	if !fileExists(bin) {
		return "", errNoBinary
	}
	if err := cachedFreshnessError(root, bin); err != nil {
		return "", err
	}
	return bin, nil
}

// repoRootFrom walks up from dir to the checkout it belongs to, identified by
// two directories every Promise checkout has. Matching on bin/promise alone —
// what this did before T2137 — would accept an unrelated installed compiler
// sitting in some parent directory, and leaves the freshness check with no tree
// to compare the binary against.
func repoRootFrom(dir string) (string, bool) {
	for {
		if fileExists(filepath.Join(dir, "compiler", "go.mod")) &&
			fileExists(filepath.Join(dir, "modules", "std")) {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Env drives the built promise binary as a subprocess, under the package's
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
	bin string
	env []string
}

// NewEnv builds an environment around the compiler binary: a git configuration
// of its own, and the package's shared PROMISE_HOME.
//
// It does NOT build a home: it inherits the one SharedHome set for the package.
// What a test out here needs isolated is its fixture project and the module
// cache entries it creates — and those are already private, because each test's
// fixture repo lives at a path of its own and the module cache mirrors a
// module's URL. What a private home would ALSO isolate is the toolchain
// surface, which is the entire cost of one: an empty home stages a whole LLVM
// view (375 MB of copies on macOS, ~900 MB on Windows, symlinks on Linux),
// re-explodes the embedded catalog, and on Linux fetches the musl CRT it
// already has, per test. That is T2133 at per-test scope, and T2150 measured it
// at 22 homes and 55 MB off the wire for one sweep of these packages.
//
// This also removes the reason T2189 had to give the home a retrying cleanup
// here: a home no test creates holds no llc.exe for Windows to refuse to
// unlink. TempDir stays for the homes that remain elsewhere; the gitconfig dir
// below never holds an executable, so it is a plain t.TempDir.
//
// A test whose SUBJECT is a home's contents still wants one of its own, and
// should say so by setting PROMISE_HOME itself — visibly, with the reason.
func NewEnv(t *testing.T) *Env {
	t.Helper()
	bin := Bin(t)
	gitconfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(gitconfig,
		[]byte("[user]\n\temail = test@users.noreply.github.com\n\tname = Test\n[safe]\n\tdirectory = *\n"), 0644); err != nil {
		t.Fatalf("write git config: %v", err)
	}
	return &Env{bin: bin, env: append(os.Environ(),
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
// rather than through an Env. Like NewEnv it runs under the package's shared
// home rather than building one (T2150).
func CompilerEpoch(t *testing.T) string {
	t.Helper()
	cmd := exec.Command(Bin(t), "catalog", "list")
	cmd.Dir = t.TempDir()
	cmd.Env = os.Environ()
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
	GitRun(t, dir, "config", "user.email", "test@users.noreply.github.com")
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
		// Genuine parse error → compile failure → incompatible (module-system.md#compatibility-and-the-community-catalog).
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

// SharedHome points the calling test package at the worktree's .promise-home for
// the whole run and warms it once before any test starts. It is meant to be the
// body of a package's TestMain.
//
// It replaced a fresh temp home per package, whose stated reason no longer
// exists. That reason was the stamp wipe: module.CompilerChanged stamps
// os.Executable(), so a test binary looked like a freshly installed compiler and
// ensureCacheValid answered by wiping cache/llvm-view out from under a peer
// mid-write. T1684 removed the wipe outright — ensureCacheValid now clears
// nothing, because every extraction cache is content-keyed and a changed binary
// simply reads a different subtree. These packages could not trigger it anyway:
// they are black-box, so the test binary itself never reads PROMISE_HOME (its
// only compiler import is internal/module, for pure functions), and every write
// to the home is made by a bin/promise CHILD — one binary, one identity.
//
// A home apiece is therefore pure cost, and on macOS and Windows it is a large
// one. The LLVM view is 375 MB of copies there (~900 MB on Windows, where the
// lld aliases are copies too) against Linux's symlinks, so three packages paid
// for it three times per run, inside whatever test happened to be first — which
// is how a saturated runner reddened as a scheduler bug (T2133). The worktree
// home is the one bin/build, bin/test, bin/verify and bin/gate already populate,
// so it is warm and resolveLLVMView's lock-free viewComplete fast path is hit
// immediately.
//
// It resolves to <root>/.promise-home rather than to the ambient PROMISE_HOME so
// that a bare `go test ./...`, and a --shared run that leaves PROMISE_HOME
// unset, can never write the machine-global ~/.promise (docs/build-tools.md
// §"Test Sandboxing"). Only the no-checkout fallback creates and removes a temp
// home; the worktree home is never deleted.
func SharedHome(m interface{ Run() int }) int { return sharedHome(m, false) }

// SharedHomeKeepingAmbient is SharedHome for a package whose tests choose a home
// themselves and pass it to a child — cmd/promise, where 22 tests re-exec the
// test binary (`exec.Command(os.Args[0], "-test.run=…")`) and hand the child the
// home it must read in its environment. TestMain runs in that child too, so
// overwriting the variable there answers every one of them with the wrong home.
//
// It differs from SharedHome in one respect: a usable already-set PROMISE_HOME
// is kept, and then deliberately not warmed — see sharedHome for why warming
// somebody else's fixture home is the very cost this avoids.
//
// The case SharedHome exists to catch is PROMISE_HOME *unset*, which resolves to
// the machine-global ~/.promise. That one is still caught here, because unset is
// not usable — and neither is ~/.promise spelled out.
func SharedHomeKeepingAmbient(m interface{ Run() int }) int { return sharedHome(m, true) }

func sharedHome(m interface{ Run() int }, keepAmbient bool) int {
	if keepAmbient && usableAmbientHome(os.Getenv("PROMISE_HOME")) {
		// A home the caller chose is a FIXTURE, not this package's home, and it
		// is deliberately not warmed. Warming it would run a compiler under it,
		// which is how a re-exec'd child ends up staging a whole toolchain into
		// its parent's temp directory — measured at 21 extra homes across one
		// sweep of cmd/promise, which is the cost T2150 exists to remove. The
		// package's own home is warmed by the branch below, and by the three
		// per-area packages that share it.
		return m.Run()
	}
	home, cleanup := resolveHome()
	if err := os.Setenv("PROMISE_HOME", home); err != nil {
		panic("clitest: cannot set PROMISE_HOME: " + err.Error())
	}
	warmToolchain(warmupBin())
	code := m.Run()
	cleanup()
	return code
}

// usableAmbientHome reports whether an inherited PROMISE_HOME may be kept.
//
// Unset is not usable: it resolves to the machine-global ~/.promise, which is
// the case SharedHome exists for. Neither is ~/.promise spelled out, and that
// one is worth checking rather than assuming — it is the product's own default,
// so it is a plausible thing for a developer to have exported, and honouring it
// would let a bare `go test` write the cache docs/build-tools.md §"Test
// Sandboxing" says no test may touch.
//
// A host whose user home cannot be determined keeps the ambient anyway. That is
// the lesser risk of the two: the re-exec'd child that needs its fixture home is
// certain, while a ~/.promise that cannot even be spelled is one the compiler
// under test could not resolve either.
func usableAmbientHome(ambient string) bool {
	userHome, err := os.UserHomeDir()
	if err != nil {
		userHome = ""
	}
	return usableAmbientHomeOn(runtime.GOOS, ambient, userHome)
}

// usableAmbientHomeOn is usableAmbientHome's decision with the host as a
// parameter, so BOTH platform arms are exercised on every host — the same shape
// as buildCommandsFor, and for the same reason. T2152 was a Windows-only branch
// that only a Windows run could reach, so no other run could notice it was
// wrong; a comparison that is case-insensitive on one platform and not on the
// other is exactly that shape again.
//
// An empty userHome means the host could not say where it is, which no path
// compares equal to, so the ambient is kept.
func usableAmbientHomeOn(goos, ambient, userHome string) bool {
	if ambient == "" {
		return false
	}
	if userHome == "" {
		return true
	}
	global := filepath.Join(userHome, ".promise")
	// Case-insensitively on Windows, where one directory answers to several
	// spellings; an exotic spelling this misses only degrades to keeping the
	// home, which is where this started.
	if goos == "windows" {
		return !strings.EqualFold(filepath.Clean(ambient), filepath.Clean(global))
	}
	return filepath.Clean(ambient) != filepath.Clean(global)
}

// resolveHome returns the package's PROMISE_HOME and the cleanup that owns it.
// Outside a checkout repoRootFrom reports none, which homeForRoot reads as the
// temp-home fallback.
func resolveHome() (home string, cleanup func()) {
	dir, err := os.Getwd()
	if err != nil {
		return homeForRoot("")
	}
	root, _ := repoRootFrom(dir)
	return homeForRoot(root)
}

// homeForRoot is resolveHome with the checkout as a parameter, so the
// no-checkout branch — the only one that creates a home of its own, and so the
// only one with anything to clean up — is reachable without a test having to
// find a directory outside every checkout on the host.
func homeForRoot(root string) (home string, cleanup func()) {
	if root != "" {
		home = filepath.Join(root, ".promise-home")
		if err := os.MkdirAll(home, 0o755); err != nil {
			panic("clitest: cannot create " + home + ": " + err.Error())
		}
		return home, func() {}
	}
	tmp, err := os.MkdirTemp("", "promise-clitest-home-")
	if err != nil {
		panic("clitest: cannot create a fallback PROMISE_HOME: " + err.Error())
	}
	return tmp, func() { os.RemoveAll(tmp) }
}

// warmupBin is the compiler to warm the home with, or "" when there is none fit
// to warm it — no checkout, nothing built, or a binary the tree has outrun
// (T2137). Each of those is Bin's to report per test, in the wording that fits;
// TestMain's only job is not to warm against them.
func warmupBin() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	bin, err := resolveBin(dir, os.Getenv("PROMISE_TEST_BIN"))
	if err != nil {
		return ""
	}
	return bin
}

// warmToolchain materializes the toolchain views and the embedded modules into
// the shared home once, before any test runs.
//
// The materialization belongs to the package, not to whichever test happens to
// reach it first. On a cold home it copies a 375 MB toolchain on macOS (~900 MB
// on Windows), and while it runs every peer promise blocks on the view lock and
// writes "Waiting for promise (materializing LLVM toolchain) to finish..." to
// its own stderr. Charged to a test that is what T2133 saw twice over: a whole
// three-minute budget spent before the subject of the test began, and a stream
// progress_test.go asserts holds exactly one line.
//
// `promise exec` is the honest warm-up — it is the same path the tests drive, so
// it materializes exactly what they need and nothing else. Best effort: a
// warm-up that fails leaves the tests exactly where they were.
func warmToolchain(bin string) {
	if bin == "" {
		return
	}
	// A directory of its own, so the warm-up is never interpreted against
	// whatever project the test binary happens to be sitting in.
	dir, err := os.MkdirTemp("", "promise-clitest-warm-")
	if err != nil {
		return
	}
	defer os.RemoveAll(dir)
	cmd := exec.Command(bin, "exec", `print_line("");`)
	cmd.Dir = dir
	_ = cmd.Run()
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
	GitRun(t, work, "config", "user.email", "test@users.noreply.github.com")
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
