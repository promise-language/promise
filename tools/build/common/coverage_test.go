package common

import (
	"os"
	"path/filepath"
	"testing"
)

// bin/coverage runs `promise test -coverage`, which compiles into a Promise
// home, so it pins the worktree's home and temp directory before anything runs,
// whatever the caller exported (#102). The root has no compiler, so the Promise
// half fails to start and is reported as a warning — the pin is what is under
// test, not the coverage.
func TestRunCoveragePinsWorktreeHome(t *testing.T) {
	isolateLocalCacheEnv(t)
	t.Setenv("PROMISE_HOME", t.TempDir())
	t.Setenv("PROMISE_CACHE", t.TempDir())

	root := t.TempDir()
	if err := RunCoverage(root, []string{"promise", "absent.pr"}); err != nil {
		t.Fatalf("RunCoverage: %v", err)
	}
	home := filepath.Join(root, ".promise-home")
	if got := os.Getenv("PROMISE_HOME"); got != home {
		t.Errorf("PROMISE_HOME = %q, want the worktree home %q", got, home)
	}
	if v, ok := os.LookupEnv("PROMISE_CACHE"); ok {
		t.Errorf("PROMISE_CACHE = %q, want it unset", v)
	}
	for _, name := range tempDirVars {
		if got, want := os.Getenv(name), filepath.Join(home, "tmp"); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// Asking for one package's coverage has to run the suite that exercises it,
// which since T1776 lives in sibling packages beneath it — while the
// measurement stays scoped to the package the caller named.
func TestGoCoverageScopeNamedPackage(t *testing.T) {
	for _, arg := range []string{"./internal/codegen", "./internal/codegen/", "./internal/codegen/..."} {
		coverPkgs, testPkgs, err := goCoverageScope("", arg)
		if err != nil {
			t.Fatalf("%s: %v", arg, err)
		}
		if coverPkgs != "./internal/codegen" {
			t.Errorf("%s: coverPkgs = %q, want ./internal/codegen", arg, coverPkgs)
		}
		if testPkgs != "./internal/codegen/..." {
			t.Errorf("%s: testPkgs = %q, want ./internal/codegen/...", arg, testPkgs)
		}
	}
}

// A package with no sibling test packages must still measure itself: the
// recursive pattern resolves to just that package, so the extra reach costs
// nothing where there is nothing to reach.
func TestGoCoverageScopeLeafPackage(t *testing.T) {
	coverPkgs, testPkgs, err := goCoverageScope("", "./internal/types")
	if err != nil {
		t.Fatal(err)
	}
	if coverPkgs != "./internal/types" || testPkgs != "./internal/types/..." {
		t.Errorf("got (%q, %q), want (./internal/types, ./internal/types/...)", coverPkgs, testPkgs)
	}
}
