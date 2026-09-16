package clitest

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A checkout is identified by its own two directories, not by the presence of a
// bin/promise: before T2137 the walk matched on the binary alone, so a compiler
// installed in any parent directory of the checkout answered for it.
func TestRepoRootFromIdentifiesTheCheckout(t *testing.T) {
	root, _ := fakeRoot(t)

	t.Run("at the root", func(t *testing.T) {
		got, ok := repoRootFrom(root)
		if !ok || got != root {
			t.Fatalf("repoRootFrom(%s) = %q, %v; want %q, true", root, got, ok, root)
		}
	})

	t.Run("from a nested package directory", func(t *testing.T) {
		nested := filepath.Join(root, "compiler", "cmd", "promise", "tests", "buildrun")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		got, ok := repoRootFrom(nested)
		if !ok || got != root {
			t.Fatalf("repoRootFrom(%s) = %q, %v; want %q, true", nested, got, ok, root)
		}
	})

	t.Run("a bare bin/promise does not stop the walk", func(t *testing.T) {
		outside := t.TempDir()
		writeStamped(t, outside, "bin/"+binaryName(), binTime)
		// Not "the walk finds nothing": under bin/verify a temp directory is
		// inside the worktree (see filesystemRoot), so finding the real
		// checkout above it is the correct answer. What must not happen is the
		// pre-T2137 behaviour — stopping *here*, at a directory that holds a
		// compiler and nothing else.
		if got, ok := repoRootFrom(outside); ok && got == outside {
			t.Fatalf("a directory holding only bin/%s must not pass as a checkout, got %q", binaryName(), got)
		}
	})

	t.Run("no checkout above the start directory", func(t *testing.T) {
		start := filesystemRoot(t.TempDir())
		if got, ok := repoRootFrom(start); ok {
			t.Fatalf("repoRootFrom(%s) = %q, true; want no checkout", start, got)
		}
	})
}

// filesystemRoot returns the root of the volume the given path is on.
//
// A test that needs a directory with no checkout above it cannot use
// t.TempDir(): bin/verify redirects TMPDIR to <repo>/.promise-home/tmp
// (common.SetupLocalCache), so under verify — and only there — a temp directory
// sits inside the worktree and the upward walk legitimately finds the real
// checkout. The volume root is the one start directory with no ancestor to
// search, on either path syntax.
func filesystemRoot(path string) string {
	return filepath.VolumeName(path) + string(os.PathSeparator)
}

// PROMISE_TEST_BIN is the caller's declared intent — an installed or published
// compiler, which is not expected to match the worktree — so it is returned
// even when the tree it sits in is stale.
func TestResolveBinEnvOverrideBypassesFreshness(t *testing.T) {
	root, bin := fakeRoot(t)
	writeStamped(t, root, "compiler/internal/codegen/extern.go", binTime.Add(time.Hour))
	if err := freshnessError(root, bin); err == nil {
		t.Fatal("fixture is not stale; the bypass would prove nothing")
	}

	got, err := resolveBin(root, bin)
	if err != nil {
		t.Fatalf("PROMISE_TEST_BIN must bypass the freshness check: %v", err)
	}
	if got != bin {
		t.Errorf("resolveBin = %q, want the named binary %q", got, bin)
	}
}

func TestResolveBinEnvOverrideMustExist(t *testing.T) {
	root, _ := fakeRoot(t)
	missing := filepath.Join(root, "bin", "no-such-compiler")

	_, err := resolveBin(root, missing)
	if err == nil {
		t.Fatal("a PROMISE_TEST_BIN that names nothing must fail, not run the wrong binary")
	}
	if errors.Is(err, errNoBinary) {
		t.Error("an explicitly named binary that is absent is a caller error, not a skip")
	}
	if !strings.Contains(err.Error(), "PROMISE_TEST_BIN") || !strings.Contains(err.Error(), missing) {
		t.Errorf("message must name the variable and the path:\n%v", err)
	}
}

// No compiler to test against is a skip: it is visibly absent and cannot
// produce a false verdict.
func TestResolveBinSkipsWhenThereIsNoBinary(t *testing.T) {
	t.Run("outside any checkout", func(t *testing.T) {
		if _, err := resolveBin(filesystemRoot(t.TempDir()), ""); !errors.Is(err, errNoBinary) {
			t.Fatalf("want errNoBinary, got %v", err)
		}
	})

	t.Run("checkout that was never built", func(t *testing.T) {
		root, bin := fakeRoot(t)
		if err := os.Remove(bin); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveBin(root, ""); !errors.Is(err, errNoBinary) {
			t.Fatalf("want errNoBinary, got %v", err)
		}
	})
}

func TestResolveBinReturnsAFreshBinary(t *testing.T) {
	root, bin := fakeRoot(t)

	got, err := resolveBin(root, "")
	if err != nil {
		t.Fatalf("want the binary, got: %v", err)
	}
	if got != bin {
		t.Errorf("resolveBin = %q, want %q", got, bin)
	}
}

// The heart of T2137: a stale binary must fail rather than skip, because a
// skipped end-to-end suite is how the stale run goes unnoticed.
func TestResolveBinRefusesAStaleBinary(t *testing.T) {
	root, _ := fakeRoot(t)
	writeStamped(t, root, "compiler/internal/codegen/extern.go", binTime.Add(time.Hour))

	_, err := resolveBin(root, "")
	if err == nil {
		t.Fatal("a binary older than the tree must be refused")
	}
	if errors.Is(err, errNoBinary) {
		t.Fatal("a stale binary must fail the test, not skip it")
	}
	if !strings.Contains(err.Error(), "compiler/internal/codegen/extern.go") {
		t.Errorf("message must name the offending source:\n%v", err)
	}
}

// A binary the caller has already established exists, and which vanishes
// between that check and the stat, is not this check's problem to report.
func TestFreshnessIgnoresAMissingBinary(t *testing.T) {
	root, _ := fakeRoot(t)
	writeStamped(t, root, "compiler/internal/codegen/extern.go", binTime.Add(time.Hour))

	if err := freshnessError(root, filepath.Join(root, "bin", "gone")); err != nil {
		t.Fatalf("want nil for a binary that is not there, got: %v", err)
	}
}

// The relative spelling has to be one the caller's shell will actually run;
// docs/windows-support.md writes it `bin\build` there.
func TestBuildCommandsSpellBothForms(t *testing.T) {
	root := t.TempDir()
	abs, relative := buildCommands(root)

	wantAbs := filepath.Join(root, "bin", "build"+exeSuffix())
	if abs != wantAbs {
		t.Errorf("absolute command = %q, want %q", abs, wantAbs)
	}
	wantRelative := "bin/build"
	if runtime.GOOS == "windows" {
		wantRelative = `bin\build`
	}
	if relative != wantRelative {
		t.Errorf("relative command = %q, want %q", relative, wantRelative)
	}
}

// TestResolveHomeIsTheWorktreeHome pins what replaced the per-package temp home
// (T2133): the shared home is the worktree's .promise-home — the one bin/build,
// bin/test, bin/verify and bin/gate populate, so it is warm and materialization
// is not charged to a test.
func TestResolveHomeIsTheWorktreeHome(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, ok := repoRootFrom(dir)
	if !ok {
		t.Skip("not running inside a checkout")
	}
	home, cleanup := resolveHome()
	defer cleanup()
	if want := filepath.Join(root, ".promise-home"); home != want {
		t.Errorf("resolveHome() = %q, want the worktree home %q", home, want)
	}
	if fi, err := os.Stat(home); err != nil || !fi.IsDir() {
		t.Errorf("resolveHome() did not create %q: %v", home, err)
	}
}

// TestResolveHomeNeverTheSharedPromiseHome is the invariant docs/build-tools.md
// §"Test Sandboxing" states: no test writes the machine-global ~/.promise. The
// ambient PROMISE_HOME is deliberately not consulted, so neither a bare
// `go test ./...` nor a --shared run (which leaves it unset, i.e. ~/.promise)
// can point these packages at the user's real cache.
func TestResolveHomeNeverTheSharedPromiseHome(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := repoRootFrom(dir); !ok {
		t.Skip("not running inside a checkout")
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no user home dir on this host")
	}
	shared := filepath.Join(userHome, ".promise")
	for _, ambient := range []string{shared, filepath.Join(t.TempDir(), "elsewhere")} {
		t.Setenv("PROMISE_HOME", ambient)
		home, cleanup := resolveHome()
		cleanup()
		if home == shared {
			t.Errorf("PROMISE_HOME=%q made resolveHome() return the shared home %q", ambient, shared)
		}
		if home == ambient {
			t.Errorf("resolveHome() followed the ambient PROMISE_HOME %q; it must not be an input", ambient)
		}
	}
}

// TestHomeForRootFallsBackOutsideACheckout: with no checkout to anchor to, the
// package owns a temp home and removes it — the one branch that still behaves
// the way every package used to.
func TestHomeForRootFallsBackOutsideACheckout(t *testing.T) {
	home, cleanup := homeForRoot("")
	if !strings.Contains(filepath.Base(home), "promise-clitest-home-") {
		t.Errorf("homeForRoot(\"\") = %q, want a temp home", home)
	}
	if fi, err := os.Stat(home); err != nil || !fi.IsDir() {
		t.Errorf("the fallback home %q was not created: %v", home, err)
	}
	cleanup()
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("the fallback home %q outlived its cleanup: %v", home, err)
	}
}

// TestWarmupBinDeclinesABinaryBinWouldRefuse: TestMain must not warm against a
// compiler the tree has outrun (T2137) or one that is not there — resolveBin
// already decides that, and warmupBin's whole job is to defer to it and answer
// "" rather than to second-guess it.
func TestWarmupBinDeclinesABinaryBinWouldRefuse(t *testing.T) {
	t.Chdir(filesystemRoot(t.TempDir()))
	t.Setenv("PROMISE_TEST_BIN", "")
	if got := warmupBin(); got != "" {
		t.Errorf("warmupBin() = %q outside any checkout, want \"\"", got)
	}
	t.Setenv("PROMISE_TEST_BIN", filepath.Join(t.TempDir(), "no-such-compiler"))
	if got := warmupBin(); got != "" {
		t.Errorf("warmupBin() = %q for a PROMISE_TEST_BIN that names nothing, want \"\"", got)
	}
}

// TestWarmToolchainWithoutABinaryIsANoOp: the warm-up is best effort, and "no
// compiler fit to warm with" is Bin's to report per test. Warming must not
// panic, exit, or leave anything behind on the way to that report.
func TestWarmToolchainWithoutABinaryIsANoOp(t *testing.T) {
	warmToolchain("") // must simply return
}

// TestWarmToolchainSurvivesABinaryThatFails: a warm-up that cannot run leaves
// the tests exactly where they were rather than failing the whole package
// before the first test starts.
func TestWarmToolchainSurvivesABinaryThatFails(t *testing.T) {
	warmToolchain(filepath.Join(t.TempDir(), "no-such-binary"))
}

// suiteStub stands in for testing.M: SharedHome's contract is what the suite
// sees while it runs and what it returns afterwards, and both are observable
// without a real suite.
type suiteStub struct {
	code     int
	ran      bool
	homeSeen string
}

func (s *suiteStub) Run() int {
	s.ran = true
	s.homeSeen = os.Getenv("PROMISE_HOME")
	return s.code
}

// TestSharedHomeRunsTheSuiteUnderTheWorktreeHome pins the whole TestMain
// contract in one place: the suite runs, it runs with PROMISE_HOME pointing at
// the worktree home rather than at whatever was ambient, and its exit code is
// what SharedHome returns — a suite whose failures were swallowed would report
// a green package (T2133).
func TestSharedHomeRunsTheSuiteUnderTheWorktreeHome(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, ok := repoRootFrom(dir)
	if !ok {
		t.Skip("not running inside a checkout")
	}
	// Restored by t.Setenv's cleanup after SharedHome overwrites it.
	t.Setenv("PROMISE_HOME", filepath.Join(t.TempDir(), "ambient"))

	suite := &suiteStub{code: 7}
	if got := SharedHome(suite); got != 7 {
		t.Errorf("SharedHome returned %d, want the suite's 7", got)
	}
	if !suite.ran {
		t.Fatal("SharedHome did not run the suite")
	}
	if want := filepath.Join(root, ".promise-home"); suite.homeSeen != want {
		t.Errorf("the suite ran under PROMISE_HOME=%q, want the worktree home %q",
			suite.homeSeen, want)
	}
	// And the home it pointed at is real, so the first child does not have to
	// create it mid-flight.
	if fi, err := os.Stat(suite.homeSeen); err != nil || !fi.IsDir() {
		t.Errorf("the shared home %q was not there for the suite: %v", suite.homeSeen, err)
	}
}
