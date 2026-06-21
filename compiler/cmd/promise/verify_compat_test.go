package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/module"
)

// gitRun runs a git command in dir, failing the test on error.
func gitRun(t *testing.T, dir string, args ...string) string {
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

// writeMod writes a minimal module (promise.toml + impl + a `test` file) into dir.
// When good is false the test file fails to compile (a parse error), making the
// commit incompatible under any epoch.
func writeMod(t *testing.T, dir, name string, good bool) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \""+name+"\"\nepoch = \"2026.0\"\n"), 0644)
	os.WriteFile(filepath.Join(dir, name+".pr"),
		[]byte(name+"_value() int `public { return 1; }\n"), 0644)
	body := "check() `test {\n  assert(" + name + "_value() == 1, \"ok\");\n}\n"
	if !good {
		// Genuine parse error → compile failure → incompatible (§9.9).
		body = "check() `test {\n  assert(" + name + "_value() == , \"x\");\n}\n"
	}
	os.WriteFile(filepath.Join(dir, name+"_test.pr"), []byte(body), 0644)
}

// makeWorkRepo initializes a non-bare git repo (usable as a local clone source)
// and returns its path. Configured so commits succeed in CI sandboxes.
func makeWorkRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "init", "--initial-branch=main")
	gitRun(t, dir, "config", "user.email", "t@t.com")
	gitRun(t, dir, "config", "user.name", "T")
	return dir
}

// TestVerifyModuleCompatCacheHit asserts a previously cached verdict short-circuits
// verification — no git fetch, no compiler run (compilerBin is intentionally bogus).
func TestVerifyModuleCompatCacheHit(t *testing.T) {
	t.Setenv("PROMISE_HOME", t.TempDir())
	url, commit, epoch := "github.com/you/cached", "abc123def456abc123def456abc123def456abcd", "2026.1"

	if err := module.SaveCompat(&module.CompatVerdict{URL: url, Commit: commit, Epoch: epoch, Compatible: true}); err != nil {
		t.Fatal(err)
	}
	ok, reason, err := verifyModuleCompat("/nonexistent/compiler", url, commit, epoch, map[string]bool{})
	if err != nil || !ok || reason != "" {
		t.Fatalf("cache hit (compatible): ok=%v reason=%q err=%v", ok, reason, err)
	}

	// A cached *incompatible* verdict is returned verbatim with its reason.
	if err := module.SaveCompat(&module.CompatVerdict{URL: url, Commit: commit, Epoch: epoch, Compatible: false, FailReason: "boom"}); err != nil {
		t.Fatal(err)
	}
	ok, reason, err = verifyModuleCompat("/nonexistent/compiler", url, commit, epoch, map[string]bool{})
	if err != nil || ok || reason != "boom" {
		t.Fatalf("cache hit (incompatible): ok=%v reason=%q err=%v", ok, reason, err)
	}
}

// TestVerifyModuleCompatNoTests: a module that compiles but carries no `_test.pr`
// cannot be verified empirically (§9.9) → incompatible, with the verdict cached.
// No compiler run is needed (it fails before the test step), so compilerBin is bogus.
func TestVerifyModuleCompatNoTests(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local repo paths contain ':' which is invalid in Windows cache paths")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("PROMISE_HOME", t.TempDir())

	repo := makeWorkRepo(t)
	os.WriteFile(filepath.Join(repo, "promise.toml"), []byte("[module]\nname = \"notests\"\nepoch = \"2026.0\"\n"), 0644)
	os.WriteFile(filepath.Join(repo, "notests.pr"), []byte("notests_value() int `public { return 1; }\n"), 0644)
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "init")
	commit := gitRun(t, repo, "rev-parse", "HEAD")

	ok, reason, err := verifyModuleCompat("/nonexistent/compiler", repo, commit, "2026.0", map[string]bool{})
	if err != nil {
		t.Fatalf("verifyModuleCompat: %v", err)
	}
	if ok {
		t.Error("a module with no test functions must be incompatible")
	}
	if !strings.Contains(reason, "no `test` functions") {
		t.Errorf("reason = %q, want the no-tests explanation", reason)
	}
	// The negative verdict must be cached for repeat resolution.
	if v, found := module.LookupCompat(repo, commit, "2026.0"); !found || v.Compatible {
		t.Errorf("expected cached incompatible verdict, got %+v found=%v", v, found)
	}
}

// TestVerifyModuleCompatInvalidToml: a fetched module whose promise.toml is invalid
// (here, the disallowed epoch = "next") is reported as incompatible with a clean
// reason rather than crashing — and never reaches the compiler.
func TestVerifyModuleCompatInvalidToml(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local repo paths contain ':' which is invalid in Windows cache paths")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("PROMISE_HOME", t.TempDir())

	repo := makeWorkRepo(t)
	os.WriteFile(filepath.Join(repo, "promise.toml"), []byte("[module]\nname = \"badtoml\"\nepoch = \"next\"\n"), 0644)
	os.WriteFile(filepath.Join(repo, "badtoml.pr"), []byte("f() int `public { return 1; }\n"), 0644)
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "init")
	commit := gitRun(t, repo, "rev-parse", "HEAD")

	ok, reason, err := verifyModuleCompat("/nonexistent/compiler", repo, commit, "2026.0", map[string]bool{})
	if err != nil {
		t.Fatalf("verifyModuleCompat: %v", err)
	}
	if ok || !strings.Contains(reason, "invalid promise.toml") {
		t.Errorf("expected invalid-promise.toml incompatibility, got ok=%v reason=%q", ok, reason)
	}
}

// TestVerifyModuleCompatTransitiveDepIncompatible: a module is incompatible when one
// of its pinned [require] deps is incompatible (§9.10 applies transitively). The dep
// here fails for a cheap reason (no tests), so neither module needs a compiler run.
func TestVerifyModuleCompatTransitiveDepIncompatible(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local repo paths contain ':' which is invalid in Windows cache paths")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("PROMISE_HOME", t.TempDir())

	// Dependency: compiles but has no tests → incompatible.
	dep := makeWorkRepo(t)
	os.WriteFile(filepath.Join(dep, "promise.toml"), []byte("[module]\nname = \"dep\"\nepoch = \"2026.0\"\n"), 0644)
	os.WriteFile(filepath.Join(dep, "dep.pr"), []byte("dep_value() int `public { return 1; }\n"), 0644)
	gitRun(t, dep, "add", ".")
	gitRun(t, dep, "commit", "-m", "init")
	depCommit := gitRun(t, dep, "rev-parse", "HEAD")

	// Parent: well-formed and has a test, but pins the broken dep via [require].
	parent := makeWorkRepo(t)
	os.WriteFile(filepath.Join(parent, "promise.toml"),
		[]byte("[module]\nname = \"parent\"\nepoch = \"2026.0\"\n\n[require]\n\""+dep+"\" = \""+depCommit+"\"\n"), 0644)
	os.WriteFile(filepath.Join(parent, "parent.pr"), []byte("parent_value() int `public { return 2; }\n"), 0644)
	os.WriteFile(filepath.Join(parent, "parent_test.pr"), []byte("ok() `test { assert(parent_value() == 2, \"ok\"); }\n"), 0644)
	gitRun(t, parent, "add", ".")
	gitRun(t, parent, "commit", "-m", "init")
	parentCommit := gitRun(t, parent, "rev-parse", "HEAD")

	ok, reason, err := verifyModuleCompat("/nonexistent/compiler", parent, parentCommit, "2026.0", map[string]bool{})
	if err != nil {
		t.Fatalf("verifyModuleCompat: %v", err)
	}
	if ok {
		t.Error("parent must be incompatible when a pinned dep is incompatible")
	}
	if !strings.Contains(reason, "transitive dependency") {
		t.Errorf("reason = %q, want a transitive-dependency explanation", reason)
	}
}

// TestProjectEpochCompilerMatchAndMismatch covers both arms of projectEpochCompiler:
// an epoch that differs from this compiler's is rejected with a `promise use` hint;
// a matching epoch returns the verifying binary.
func TestProjectEpochCompilerMatchAndMismatch(t *testing.T) {
	myEpoch, err := module.CompilerEpoch(embeddedCatalog)
	if err != nil || myEpoch == "" {
		t.Skipf("cannot determine compiler epoch: %v", err)
	}

	// Mismatch: a clearly different epoch must be refused before any work.
	if _, err := projectEpochCompiler("1999.0"); err == nil {
		t.Error("expected an error when the project epoch differs from the compiler's")
	} else if !strings.Contains(err.Error(), "verification must run under") {
		t.Errorf("unexpected mismatch error: %v", err)
	}

	// Match: returns the (test-overridden) verifying binary.
	testVerifyCompilerBin = "/path/to/epoch/compiler"
	defer func() { testVerifyCompilerBin = "" }()
	got, err := projectEpochCompiler(myEpoch)
	if err != nil {
		t.Fatalf("projectEpochCompiler(match): %v", err)
	}
	if got != "/path/to/epoch/compiler" {
		t.Errorf("projectEpochCompiler = %q, want the overridden binary", got)
	}
}

// TestResolveEpochAwareExplicitRefIncompatible: with an explicit ref the resolver
// does NOT walk back — a broken pinned ref surfaces a clean "not compatible" error.
func TestResolveEpochAwareExplicitRefIncompatible(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	bin := findPromiseBinary(t)
	t.Setenv("PROMISE_HOME", t.TempDir())

	work := makeWorkRepo(t)
	writeMod(t, work, "brokenref", false) // parse error → incompatible
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "broken")

	_, err := resolveEpochAware(bin, "2026.1", "brokenref", work, "HEAD", func(string) {})
	if err == nil {
		t.Fatal("expected an incompatibility error for a broken explicit ref")
	}
	if !strings.Contains(err.Error(), "not compatible with epoch") {
		t.Errorf("unexpected error: %v", err)
	}
	// An explicit ref must never produce the walk-back gate error.
	if _, ok := err.(*module.NoCompatibleVersionError); ok {
		t.Error("explicit ref should not yield a NoCompatibleVersionError (no walk-back)")
	}
}

// TestResolveEpochAwareStableFallback: with no epoch-* tags but a `stable` tag, the
// resolver pins the stable commit (after verifying it) and warns it is using stable.
func TestResolveEpochAwareStableFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	bin := findPromiseBinary(t)
	t.Setenv("PROMISE_HOME", t.TempDir())

	work := makeWorkRepo(t)
	writeMod(t, work, "stablemod", true)
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "good")
	head := gitRun(t, work, "rev-parse", "HEAD")
	gitRun(t, work, "tag", "stable")

	var warnings []string
	commit, err := resolveEpochAware(bin, "2026.1", "stablemod", work, "", func(m string) { warnings = append(warnings, m) })
	if err != nil {
		t.Fatalf("resolveEpochAware: %v", err)
	}
	if commit != head {
		t.Errorf("resolved %s, want stable commit %s", commit, head)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "'stable' tag") {
		t.Errorf("expected a 'stable' tag warning, got %v", warnings)
	}
}

// TestResolveEpochAwareHeadFallback: a module with no epoch-* and no `stable` tag
// falls back to default-branch HEAD with an explicit "unversioned" warning.
func TestResolveEpochAwareHeadFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	bin := findPromiseBinary(t)
	t.Setenv("PROMISE_HOME", t.TempDir())

	work := makeWorkRepo(t)
	writeMod(t, work, "unver", true)
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "good")
	head := gitRun(t, work, "rev-parse", "HEAD")

	var warnings []string
	commit, err := resolveEpochAware(bin, "2026.1", "unver", work, "", func(m string) { warnings = append(warnings, m) })
	if err != nil {
		t.Fatalf("resolveEpochAware: %v", err)
	}
	if commit != head {
		t.Errorf("resolved %s, want HEAD %s", commit, head)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "unversioned") {
		t.Errorf("expected an 'unversioned' warning, got %v", warnings)
	}
}

// TestCheckUpgradeWithDeps drives `promise pkg check-upgrade <E>` end-to-end through
// the real compiler binary (so epochCompilerBin resolves to a true compiler): a
// project whose only dep has a verified E-compatible version reports all-clear; a
// project whose dep is broken hits the §9.10 gate and exits non-zero.
func TestCheckUpgradeWithDeps(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	bin := findPromiseBinary(t)
	epoch := compilerEpochForTest(t)
	home := t.TempDir()

	makeDep := func(good bool) (url, commit string) {
		work := makeWorkRepo(t)
		writeMod(t, work, "dep", good)
		gitRun(t, work, "add", ".")
		gitRun(t, work, "commit", "-m", "init")
		gitRun(t, work, "tag", "epoch-"+epoch)
		return work, gitRun(t, work, "rev-parse", "HEAD")
	}

	runCheck := func(t *testing.T, depURL, depCommit string) (string, error) {
		proj := t.TempDir()
		toml := "[module]\nname = \"proj\"\nepoch = \"" + epoch + "\"\n\n[require]\n\"" + depURL + "\" = \"" + depCommit + "\"\n"
		os.WriteFile(filepath.Join(proj, "promise.toml"), []byte(toml), 0644)
		cmd := exec.Command(bin, "pkg", "check-upgrade", epoch)
		cmd.Dir = proj
		cmd.Env = append(os.Environ(), "PROMISE_HOME="+home, "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("compatible", func(t *testing.T) {
		url, commit := makeDep(true)
		out, err := runCheck(t, url, commit)
		if err != nil {
			t.Fatalf("check-upgrade should succeed for a compatible dep: %v\n%s", err, out)
		}
		if !strings.Contains(out, "compatible with epoch "+epoch) {
			t.Errorf("expected all-compatible report, got:\n%s", out)
		}
	})

	t.Run("blocked", func(t *testing.T) {
		url, commit := makeDep(false)
		out, err := runCheck(t, url, commit)
		if err == nil {
			t.Fatalf("check-upgrade should exit non-zero for a broken dep:\n%s", out)
		}
		if !strings.Contains(out, "no compatible version") {
			t.Errorf("expected a §9.10 gate report, got:\n%s", out)
		}
	})
}

// TestResolveEpochAwareWalkBack builds a repo with a passing epoch-2026.0 tag and
// a broken epoch-2026.1 tag, then verifies that a project on epoch 2026.1 resolves
// to the 2026.0 commit (the broken newer tag fails verification → step back).
func TestResolveEpochAwareWalkBack(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	bin := findPromiseBinary(t)
	t.Setenv("PROMISE_HOME", t.TempDir())

	work := filepath.Join(t.TempDir(), "walkback")
	os.MkdirAll(work, 0755)
	gitRun(t, work, "init", "--initial-branch=main")
	gitRun(t, work, "config", "user.email", "t@t.com")
	gitRun(t, work, "config", "user.name", "T")

	// Commit A — good — tag epoch-2026.0
	writeMod(t, work, "walkback", true)
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "good")
	commitGood := gitRun(t, work, "rev-parse", "HEAD")
	gitRun(t, work, "tag", "epoch-2026.0")

	// Commit B — broken — tag epoch-2026.1
	writeMod(t, work, "walkback", false)
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "broken")
	gitRun(t, work, "tag", "epoch-2026.1")

	warnings := []string{}
	warn := func(m string) { warnings = append(warnings, m) }
	commit, err := resolveEpochAware(bin, "2026.1", "walkback", work, "", warn)
	if err != nil {
		t.Fatalf("resolveEpochAware: %v", err)
	}
	if commit != commitGood {
		t.Errorf("resolved %s, want the good (2026.0) commit %s", commit, commitGood)
	}
	// It should have reported stepping back from the failing 2026.1 tag.
	steppedBack := false
	for _, w := range warnings {
		if strings.Contains(w, "epoch-2026.1") && strings.Contains(w, "stepping back") {
			steppedBack = true
		}
	}
	if !steppedBack {
		t.Errorf("expected a step-back warning for epoch-2026.1, got %v", warnings)
	}

	// Verdicts must now be cached for both commits.
	if v, ok := module.LookupCompat(work, commitGood, "2026.1"); !ok || !v.Compatible {
		t.Errorf("expected cached compatible verdict for good commit, got %+v ok=%v", v, ok)
	}
}

// TestResolveEpochAwareNoCompatible builds a repo whose only tag (epoch-2026.1) is
// broken, then asserts a project on 2026.1 hits the §9.10 gate.
func TestResolveEpochAwareNoCompatible(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	bin := findPromiseBinary(t)
	t.Setenv("PROMISE_HOME", t.TempDir())

	work := filepath.Join(t.TempDir(), "nocompat")
	os.MkdirAll(work, 0755)
	gitRun(t, work, "init", "--initial-branch=main")
	gitRun(t, work, "config", "user.email", "t@t.com")
	gitRun(t, work, "config", "user.name", "T")

	writeMod(t, work, "nocompat", false) // broken
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "broken")
	gitRun(t, work, "tag", "epoch-2026.1")

	_, err := resolveEpochAware(bin, "2026.1", "nocompat", work, "", func(string) {})
	if err == nil {
		t.Fatal("expected NoCompatibleVersionError, got nil")
	}
	nce, ok := err.(*module.NoCompatibleVersionError)
	if !ok {
		t.Fatalf("expected *NoCompatibleVersionError, got %T: %v", err, err)
	}
	if nce.HighestVerifiedEpoch != "2026.1" {
		t.Errorf("HighestVerifiedEpoch = %q, want 2026.1", nce.HighestVerifiedEpoch)
	}
}

// TestResolveEpochAwareCompatibleHappyPath: a repo with a single good epoch-2026.1
// tag resolves cleanly for a project on 2026.1.
func TestResolveEpochAwareCompatibleHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	bin := findPromiseBinary(t)
	t.Setenv("PROMISE_HOME", t.TempDir())

	work := filepath.Join(t.TempDir(), "happy")
	os.MkdirAll(work, 0755)
	gitRun(t, work, "init", "--initial-branch=main")
	gitRun(t, work, "config", "user.email", "t@t.com")
	gitRun(t, work, "config", "user.name", "T")

	writeMod(t, work, "happy", true)
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "good")
	head := gitRun(t, work, "rev-parse", "HEAD")
	gitRun(t, work, "tag", "epoch-2026.1")

	commit, err := resolveEpochAware(bin, "2026.1", "happy", work, "", func(string) {})
	if err != nil {
		t.Fatalf("resolveEpochAware: %v", err)
	}
	if commit != head {
		t.Errorf("resolved %s, want %s", commit, head)
	}
}

// TestCheckUpgradeRejectsNext asserts `pkg check-upgrade next` is rejected before
// any resolution — "next" is a toolchain channel, never a project epoch (§4.3).
func TestCheckUpgradeRejectsNext(t *testing.T) {
	if os.Getenv("TEST_CHECKUP_NEXT") == "1" {
		runPackageCheckUpgrade([]string{"next"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestCheckUpgradeRejectsNext")
	cmd.Env = append(os.Environ(), "TEST_CHECKUP_NEXT=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for 'next'")
	}
	if !strings.Contains(string(out), "toolchain channel") {
		t.Errorf("expected toolchain-channel rejection, got: %s", out)
	}
}

// TestCheckUpgradeRejectsNonNumeric asserts a non-"YYYY.N" target epoch is
// rejected up front rather than mishandled downstream.
func TestCheckUpgradeRejectsNonNumeric(t *testing.T) {
	if os.Getenv("TEST_CHECKUP_BAD") == "1" {
		runPackageCheckUpgrade([]string{"garbage"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestCheckUpgradeRejectsNonNumeric")
	cmd.Env = append(os.Environ(), "TEST_CHECKUP_BAD=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for non-numeric epoch")
	}
	if !strings.Contains(string(out), "not a numeric epoch") {
		t.Errorf("expected numeric-epoch rejection, got: %s", out)
	}
}

// TestCheckUpgradeNoDeps: a project with no [require] dependencies reports the
// empty case cleanly and never needs to locate an epoch-E′ compiler.
func TestCheckUpgradeNoDeps(t *testing.T) {
	projDir := t.TempDir()
	os.WriteFile(filepath.Join(projDir, "promise.toml"),
		[]byte("[module]\nname = \"proj\"\nepoch = \"2026.1\"\n"), 0644)

	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(projDir)

	out := captureStdout(t, func() { runPackageCheckUpgrade([]string{"2026.2"}) })
	if !strings.Contains(out, "No [require] dependencies") {
		t.Errorf("expected empty-deps message, got: %s", out)
	}
}

// TestEpochCompilerBinPresent asserts epochCompilerBin returns the
// <PromiseHome>/epochs/<E>/bin/promise path when the binary is already present —
// the reproducibility guarantee that an old epoch's compiler stays installable at
// a stable location (§9.10, §7.2).
func TestEpochCompilerBinPresent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PROMISE_HOME", home)

	epochDir := filepath.Join(home, "epochs", "2026.0", "bin")
	os.MkdirAll(epochDir, 0755)
	binName := "promise"
	if runtime.GOOS == "windows" {
		binName = "promise.exe"
	}
	binPath := filepath.Join(epochDir, binName)
	os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0755)

	got, err := epochCompilerBin("2026.0")
	if err != nil {
		t.Fatalf("epochCompilerBin: %v", err)
	}
	if got != binPath {
		t.Errorf("epochCompilerBin = %q, want %q", got, binPath)
	}
}
