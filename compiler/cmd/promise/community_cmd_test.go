package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/module"
)

// runSubprocess re-execs this test binary running only test `name` with envVar
// set, capturing combined output. Used to exercise os.Exit validation paths: the
// matching test re-enters its `if os.Getenv(...)=="1"` guard, runs the
// exit-calling code, and the parent asserts on output + non-zero exit.
func runSubprocess(t *testing.T, name, envVar string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$")
	cmd.Env = append(os.Environ(), envVar)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// makeCommunityCatalogRepo creates a git repo to act as the community catalog
// (clonable via PROMISE_COMMUNITY_CATALOG), with the given modules.toml and a set
// of index/<name>.json files. Returns its path.
func makeCommunityCatalogRepo(t *testing.T, modulesTOML string, indexFiles map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--initial-branch=main")
	run("config", "user.email", "t@t.com")
	run("config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "modules.toml"), []byte(modulesTOML), 0644); err != nil {
		t.Fatal(err)
	}
	if len(indexFiles) > 0 {
		idxDir := filepath.Join(dir, "index")
		if err := os.MkdirAll(idxDir, 0755); err != nil {
			t.Fatal(err)
		}
		for name, body := range indexFiles {
			if err := os.WriteFile(filepath.Join(idxDir, name), []byte(body), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	run("add", ".")
	run("commit", "-m", "catalog")
	return dir
}

// makeTaggedModuleRepo creates a bare git "remote" carrying a verifiable module
// (good=true → compiles + tests pass) tagged epoch-<epoch>. Returns (bareDir,
// headCommit).
func makeTaggedModuleRepo(t *testing.T, name, epoch string, good bool) (string, string) {
	t.Helper()
	bareDir := filepath.ToSlash(shortRepoDir(t))
	workDir := shortRepoDir(t)
	run := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(bareDir, "init", "--bare", ".")
	run(workDir, "clone", bareDir, ".")
	writeMod(t, workDir, name, good)
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

// --- resolveCommunity (name-resolution, §9.9 step 4) ---

func TestResolveCommunityVerified(t *testing.T) {
	setupGitTestEnv(t)
	t.Setenv("PROMISE_HOME", t.TempDir())
	epoch := "2026.1"
	modulesTOML := "[modules.foo]\nurl = \"https://github.com/promise-community/foo\"\n"
	index := map[string]string{
		epoch + ".json": `{"epoch":"` + epoch + `","modules":{"foo":{"commit":"abc123def456abc123def456abc123def456abcd","tag":"epoch-2026.1"}}}`,
	}
	cat := makeCommunityCatalogRepo(t, modulesTOML, index)
	t.Setenv("PROMISE_COMMUNITY_CATALOG", cat)

	url, commit, found, err := resolveCommunity("foo", epoch)
	if err != nil || !found {
		t.Fatalf("resolveCommunity: url=%q commit=%q found=%v err=%v", url, commit, found, err)
	}
	if url != "https://github.com/promise-community/foo" {
		t.Errorf("url = %q", url)
	}
	if commit != "abc123def456abc123def456abc123def456abcd" {
		t.Errorf("commit = %q", commit)
	}
}

func TestResolveCommunityNoIndexForEpoch(t *testing.T) {
	setupGitTestEnv(t)
	t.Setenv("PROMISE_HOME", t.TempDir())
	modulesTOML := "[modules.foo]\nurl = \"https://github.com/promise-community/foo\"\n"
	// Only an older epoch is recorded; the project asks for 2026.3 → §9.10.
	index := map[string]string{
		"2026.1.json": `{"epoch":"2026.1","modules":{"foo":{"commit":"c1","tag":"epoch-2026.1"}}}`,
	}
	cat := makeCommunityCatalogRepo(t, modulesTOML, index)
	t.Setenv("PROMISE_COMMUNITY_CATALOG", cat)

	_, _, found, err := resolveCommunity("foo", "2026.3")
	if !found {
		t.Fatal("expected found=true (listed but no compatible version)")
	}
	nce, ok := err.(*module.NoCompatibleVersionError)
	if !ok {
		t.Fatalf("expected NoCompatibleVersionError, got %T: %v", err, err)
	}
	if nce.HighestVerifiedEpoch != "2026.1" {
		t.Errorf("HighestVerifiedEpoch = %q", nce.HighestVerifiedEpoch)
	}
}

func TestResolveCommunityNotListed(t *testing.T) {
	setupGitTestEnv(t)
	t.Setenv("PROMISE_HOME", t.TempDir())
	cat := makeCommunityCatalogRepo(t, "[modules.other]\nurl = \"https://github.com/promise-community/other\"\n", nil)
	t.Setenv("PROMISE_COMMUNITY_CATALOG", cat)

	url, commit, found, err := resolveCommunity("foo", "2026.1")
	if found || err != nil || url != "" || commit != "" {
		t.Fatalf("expected not-found: url=%q commit=%q found=%v err=%v", url, commit, found, err)
	}
}

// TestPkgUpdateCommunityModule: a [require] entry whose URL is in the
// promise-community org re-resolves through the FRESH community index (§9.9) — it
// is re-pinned to the index commit with no module fetch and no local test run (so
// no compiler binary is needed). This exercises the TierCommunity routing in
// runPkgUpdate.
func TestPkgUpdateCommunityModule(t *testing.T) {
	setupGitTestEnv(t)
	t.Setenv("PROMISE_HOME", t.TempDir())
	epoch := compilerEpochForTest(t)
	url := "https://github.com/promise-community/foo"
	oldCommit := "1111111111111111111111111111111111111111"
	newCommit := "2222222222222222222222222222222222222222"
	modulesTOML := "[modules.foo]\nurl = \"" + url + "\"\n"
	index := map[string]string{
		epoch + ".json": `{"epoch":"` + epoch + `","modules":{"foo":{"commit":"` + newCommit + `","tag":"epoch-` + epoch + `"}}}`,
	}
	cat := makeCommunityCatalogRepo(t, modulesTOML, index)
	t.Setenv("PROMISE_COMMUNITY_CATALOG", cat)

	projDir := t.TempDir()
	toml := "[module]\nname = \"proj\"\nepoch = \"" + epoch + "\"\n\n[require]\n\"" + url + "\" = \"" + oldCommit + "\"\n"
	os.WriteFile(filepath.Join(projDir, "promise.toml"), []byte(toml), 0644)
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(projDir)

	out := captureStdout(t, func() {
		captureStderr(func() {
			runPkgUpdate(nil)
		})
	})
	if !strings.Contains(out, "Updated 1") {
		t.Errorf("expected the community entry to update, got: %s", out)
	}
	content, _ := os.ReadFile(filepath.Join(projDir, "promise.toml"))
	if !strings.Contains(string(content), newCommit) {
		t.Errorf("promise.toml should be re-pinned to the community index commit, got: %s", content)
	}
}

// --- runAdd community integration (§9.9): pins the indexed commit, no local run ---

func TestAddCommunityModule(t *testing.T) {
	setupGitTestEnv(t)
	t.Setenv("PROMISE_HOME", t.TempDir())
	epoch := compilerEpochForTest(t)
	commit := "abc123def456abc123def456abc123def456abcd"
	modulesTOML := "[modules.mymod]\nurl = \"https://github.com/promise-community/mymod\"\n"
	index := map[string]string{
		epoch + ".json": `{"epoch":"` + epoch + `","modules":{"mymod":{"commit":"` + commit + `","tag":"epoch-` + epoch + `"}}}`,
	}
	cat := makeCommunityCatalogRepo(t, modulesTOML, index)
	t.Setenv("PROMISE_COMMUNITY_CATALOG", cat)

	projDir := t.TempDir()
	os.WriteFile(filepath.Join(projDir, "promise.toml"),
		[]byte("[module]\nname = \"proj\"\nepoch = \""+epoch+"\"\n"), 0644)
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(projDir)

	out := captureStdout(t, func() {
		captureStderr(func() {
			runAdd([]string{"mymod"})
		})
	})
	if !strings.Contains(out, "Added") || !strings.Contains(out, "community") {
		t.Errorf("expected community 'Added', got: %s", out)
	}
	content, _ := os.ReadFile(filepath.Join(projDir, "promise.toml"))
	if !strings.Contains(string(content), commit) {
		t.Errorf("promise.toml should pin community commit, got: %s", content)
	}
}

// TestAddEmbeddedShadowsCommunity: a name in the embedded catalog is resolved
// there and the community catalog is never consulted (PROMISE_COMMUNITY_CATALOG
// points at a path that would error if fetched).
func TestAddEmbeddedShadowsCommunity(t *testing.T) {
	t.Setenv("PROMISE_COMMUNITY_CATALOG", "/nonexistent/community/catalog/repo")
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(dir)
	os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"test\"\nepoch = \"2026.0\"\n"), 0644)

	withCatalog([]byte(testCatalogTOML), func() {
		out := captureStdout(t, func() {
			captureStderr(func() {
				runAdd([]string{"io"}) // embedded (built-in) — shadows community
			})
		})
		if !strings.Contains(out, "is built-in") {
			t.Errorf("expected embedded shadow ('is built-in'), got: %s", out)
		}
	})
}

// --- verifyLocalModuleCompat (check-epoch building block) ---

func TestVerifyLocalModuleCompat(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	setupGitTestEnv(t)
	t.Setenv("PROMISE_HOME", t.TempDir())
	bin := findPromiseBinary(t)
	epoch := compilerEpochForTest(t)

	good := t.TempDir()
	writeMod(t, good, "lib", true)
	if ok, reason, err := verifyLocalModuleCompat(bin, good, epoch); err != nil || !ok {
		t.Fatalf("good module: ok=%v reason=%q err=%v", ok, reason, err)
	}

	bad := t.TempDir()
	writeMod(t, bad, "lib", false)
	if ok, reason, err := verifyLocalModuleCompat(bin, bad, epoch); err != nil || ok || reason == "" {
		t.Fatalf("bad module should fail: ok=%v reason=%q err=%v", ok, reason, err)
	}

	// A module with no `_test.pr` is accepted compile-only (§9.9 policy change):
	// emit-ir verifies the source compiles; a warning is printed to stderr.
	notests := t.TempDir()
	os.WriteFile(filepath.Join(notests, "promise.toml"), []byte("[module]\nname = \"n\"\nepoch = \"2026.0\"\n"), 0644)
	os.WriteFile(filepath.Join(notests, "n.pr"), []byte("v() int `public { return 1; }\n"), 0644)
	if ok, reason, err := verifyLocalModuleCompat(bin, notests, epoch); err != nil || !ok {
		t.Errorf("no-tests module with valid source should be compile-only compatible: ok=%v reason=%q err=%v", ok, reason, err)
	}
}

func TestRunPackageCheckEpoch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	setupGitTestEnv(t)
	t.Setenv("PROMISE_HOME", t.TempDir())
	testVerifyCompilerBin = findPromiseBinary(t)
	defer func() { testVerifyCompilerBin = "" }()
	epoch := compilerEpochForTest(t)

	modDir := t.TempDir()
	writeMod(t, modDir, "lib", true)
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(modDir)

	out := captureStdout(t, func() {
		captureStderr(func() {
			runPackageCheckEpoch([]string{epoch})
		})
	})
	if !strings.Contains(out, "✓ compatible") || !strings.Contains(out, "git tag epoch-"+epoch) {
		t.Errorf("expected pass + tag hint, got: %s", out)
	}
}

// --- build-index (CI matrix builder) ---

func TestRunPackageBuildIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	setupGitTestEnv(t)
	t.Setenv("PROMISE_HOME", t.TempDir())
	testVerifyCompilerBin = findPromiseBinary(t)
	defer func() { testVerifyCompilerBin = "" }()
	epoch := compilerEpochForTest(t)

	bareDir, headCommit := makeTaggedModuleRepo(t, "mymod", epoch, true)

	catalogDir := t.TempDir()
	os.WriteFile(filepath.Join(catalogDir, "modules.toml"),
		[]byte("[modules.mymod]\nurl = \""+bareDir+"\"\n"), 0644)

	out := captureStdout(t, func() {
		captureStderr(func() {
			runPackageBuildIndex([]string{catalogDir, epoch})
		})
	})
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

// TestResolveCommunityNoModulesToml: a community catalog repo that has no
// modules.toml yet must resolve to not-found (an empty catalog) so resolution
// falls cleanly through to the ad-hoc path rather than erroring. Covers the
// os.IsNotExist arm of fetchCommunityCatalog.
func TestResolveCommunityNoModulesToml(t *testing.T) {
	setupGitTestEnv(t)
	t.Setenv("PROMISE_HOME", t.TempDir())

	// A valid git repo with content but no modules.toml.
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--initial-branch=main")
	run("config", "user.email", "t@t.com")
	run("config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("catalog\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "no modules.toml yet")
	t.Setenv("PROMISE_COMMUNITY_CATALOG", dir)

	url, commit, found, err := resolveCommunity("foo", "2026.1")
	if found || err != nil || url != "" || commit != "" {
		t.Fatalf("expected clean not-found: url=%q commit=%q found=%v err=%v", url, commit, found, err)
	}
}

// TestResolveCommunityByURLNotListed: a URL that is not (or no longer) in the
// community catalog returns found=false so `pkg update` falls back to the generic
// epoch-tag engine path rather than erroring.
func TestResolveCommunityByURLNotListed(t *testing.T) {
	setupGitTestEnv(t)
	t.Setenv("PROMISE_HOME", t.TempDir())
	cat := makeCommunityCatalogRepo(t, "[modules.foo]\nurl = \"https://github.com/promise-community/foo\"\n", nil)
	t.Setenv("PROMISE_COMMUNITY_CATALOG", cat)

	commit, found, err := resolveCommunityByURL("https://github.com/promise-community/gone", "2026.1")
	if found || err != nil || commit != "" {
		t.Fatalf("expected not-found: commit=%q found=%v err=%v", commit, found, err)
	}
}

// TestTagForCommitNoMatch: when no epoch-* tag in the repo points at the resolved
// commit (a stable/HEAD fallback), tagForCommit returns "" rather than an
// unrelated tag. The bare URL has no reachable tags, so ListRepoTags errors and
// the function returns "".
func TestTagForCommitNoMatch(t *testing.T) {
	setupGitTestEnv(t)
	t.Setenv("PROMISE_HOME", t.TempDir())
	if got := tagForCommit("/nonexistent/repo/path", "deadbeef"); got != "" {
		t.Errorf("tagForCommit(no tags) = %q, want \"\"", got)
	}
}

// --- validation / usage error paths (exercised via subprocess, since they exit) ---

// TestCheckEpochRejectsNext: `pkg check-epoch next` is rejected — "next" is a
// toolchain channel, not an epoch (§4.3).
func TestCheckEpochRejectsNext(t *testing.T) {
	if os.Getenv("TEST_CHECKEPOCH_NEXT") == "1" {
		// A promise.toml must exist to reach the epoch validation.
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "promise.toml"), []byte("[module]\nname=\"m\"\nepoch=\"2026.0\"\n"), 0644)
		os.Chdir(dir)
		runPackageCheckEpoch([]string{module.ChannelNext})
		return
	}
	out, err := runSubprocess(t, "TestCheckEpochRejectsNext", "TEST_CHECKEPOCH_NEXT=1")
	if err == nil {
		t.Fatal("expected non-zero exit for 'next'")
	}
	if !strings.Contains(out, "toolchain channel") {
		t.Errorf("expected toolchain-channel rejection, got: %s", out)
	}
}

// TestCheckEpochRejectsNonNumeric: a non-"YYYY.N" epoch arg is rejected up front.
func TestCheckEpochRejectsNonNumeric(t *testing.T) {
	if os.Getenv("TEST_CHECKEPOCH_BAD") == "1" {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "promise.toml"), []byte("[module]\nname=\"m\"\nepoch=\"2026.0\"\n"), 0644)
		os.Chdir(dir)
		runPackageCheckEpoch([]string{"garbage"})
		return
	}
	out, err := runSubprocess(t, "TestCheckEpochRejectsNonNumeric", "TEST_CHECKEPOCH_BAD=1")
	if err == nil {
		t.Fatal("expected non-zero exit for non-numeric epoch")
	}
	if !strings.Contains(out, "not a numeric epoch") {
		t.Errorf("expected numeric-epoch rejection, got: %s", out)
	}
}

// TestCheckEpochNoPromiseToml: run outside a module root (no promise.toml) exits
// with a clear "run from the module root" error before any verification.
func TestCheckEpochNoPromiseToml(t *testing.T) {
	if os.Getenv("TEST_CHECKEPOCH_NOTOML") == "1" {
		os.Chdir(t.TempDir()) // empty dir, no promise.toml
		runPackageCheckEpoch([]string{"2026.1"})
		return
	}
	out, err := runSubprocess(t, "TestCheckEpochNoPromiseToml", "TEST_CHECKEPOCH_NOTOML=1")
	if err == nil {
		t.Fatal("expected non-zero exit without promise.toml")
	}
	if !strings.Contains(out, "no promise.toml") {
		t.Errorf("expected no-promise.toml error, got: %s", out)
	}
}

// TestCheckEpochUsage: too many positional args prints usage and exits non-zero.
func TestCheckEpochUsage(t *testing.T) {
	if os.Getenv("TEST_CHECKEPOCH_USAGE") == "1" {
		runPackageCheckEpoch([]string{"2026.1", "extra"})
		return
	}
	out, err := runSubprocess(t, "TestCheckEpochUsage", "TEST_CHECKEPOCH_USAGE=1")
	if err == nil {
		t.Fatal("expected non-zero exit for too many args")
	}
	if !strings.Contains(out, "usage: promise pkg check-epoch") {
		t.Errorf("expected usage text, got: %s", out)
	}
}

// TestBuildIndexUsage: wrong positional-arg count prints usage and exits non-zero.
func TestBuildIndexUsage(t *testing.T) {
	if os.Getenv("TEST_BUILDIDX_USAGE") == "1" {
		runPackageBuildIndex([]string{"only-one-arg"})
		return
	}
	out, err := runSubprocess(t, "TestBuildIndexUsage", "TEST_BUILDIDX_USAGE=1")
	if err == nil {
		t.Fatal("expected non-zero exit for missing args")
	}
	if !strings.Contains(out, "usage: promise pkg build-index") {
		t.Errorf("expected usage text, got: %s", out)
	}
}

// TestBuildIndexRejectsNext: build-index for the "next" channel is rejected — it
// must be given the numeric epoch the toolchain implements.
func TestBuildIndexRejectsNext(t *testing.T) {
	if os.Getenv("TEST_BUILDIDX_NEXT") == "1" {
		runPackageBuildIndex([]string{t.TempDir(), module.ChannelNext})
		return
	}
	out, err := runSubprocess(t, "TestBuildIndexRejectsNext", "TEST_BUILDIDX_NEXT=1")
	if err == nil {
		t.Fatal("expected non-zero exit for 'next'")
	}
	if !strings.Contains(out, "toolchain channel") {
		t.Errorf("expected toolchain-channel rejection, got: %s", out)
	}
}

// TestBuildIndexRejectsNonNumeric: a non-"YYYY.N" epoch arg is rejected up front.
func TestBuildIndexRejectsNonNumeric(t *testing.T) {
	if os.Getenv("TEST_BUILDIDX_BAD") == "1" {
		runPackageBuildIndex([]string{t.TempDir(), "garbage"})
		return
	}
	out, err := runSubprocess(t, "TestBuildIndexRejectsNonNumeric", "TEST_BUILDIDX_BAD=1")
	if err == nil {
		t.Fatal("expected non-zero exit for non-numeric epoch")
	}
	if !strings.Contains(out, "not a numeric epoch") {
		t.Errorf("expected numeric-epoch rejection, got: %s", out)
	}
}

// TestBuildIndexMissingModulesToml: a catalog dir with no modules.toml exits with
// a clear read error rather than panicking.
func TestBuildIndexMissingModulesToml(t *testing.T) {
	if os.Getenv("TEST_BUILDIDX_NOTOML") == "1" {
		runPackageBuildIndex([]string{t.TempDir(), "2026.1"}) // empty catalog dir
		return
	}
	out, err := runSubprocess(t, "TestBuildIndexMissingModulesToml", "TEST_BUILDIDX_NOTOML=1")
	if err == nil {
		t.Fatal("expected non-zero exit when modules.toml is absent")
	}
	if !strings.Contains(out, "cannot read") || !strings.Contains(out, "modules.toml") {
		t.Errorf("expected modules.toml read error, got: %s", out)
	}
}

// TestRunPackageBuildIndexReport: a module that fails verification is absent from
// the index and listed as unsupported; with -report the run does not exit-fail
// (the §9.10 pre-release nudge).
func TestRunPackageBuildIndexReport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	setupGitTestEnv(t)
	t.Setenv("PROMISE_HOME", t.TempDir())
	testVerifyCompilerBin = findPromiseBinary(t)
	defer func() { testVerifyCompilerBin = "" }()
	epoch := compilerEpochForTest(t)

	bareDir, _ := makeTaggedModuleRepo(t, "brokenmod", epoch, false) // fails to compile

	catalogDir := t.TempDir()
	os.WriteFile(filepath.Join(catalogDir, "modules.toml"),
		[]byte("[modules.brokenmod]\nurl = \""+bareDir+"\"\n"), 0644)

	out := captureStdout(t, func() {
		captureStderr(func() {
			runPackageBuildIndex([]string{catalogDir, epoch, "-report"})
		})
	})
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
