package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/module"
)

// compilerEpochForTest returns the epoch this test binary's embedded catalog
// implements — used so add/update tests pin a project epoch that matches the
// verifying compiler (verification must run under the project epoch).
func compilerEpochForTest(t *testing.T) string {
	t.Helper()
	e, err := module.CompilerEpoch(embeddedCatalog)
	if err != nil || e == "" {
		t.Skipf("cannot determine compiler epoch: %v", err)
	}
	return e
}

// testCatalogTOML is a minimal catalog fixture for testing.
const testCatalogTOML = `[catalog]
epoch = "2026.0"

[modules.json]
url = "https://github.com/promise-language/json"
commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
description = "JSON parsing and serialization"

[modules.io]
description = "Console and file I/O"

[modules.strings]
url = "https://github.com/promise-language/strings"
commit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
description = "Extended string utilities"
`

// withCatalog temporarily sets embeddedCatalog for the duration of fn.
func withCatalog(data []byte, fn func()) {
	saved := embeddedCatalog
	embeddedCatalog = data
	defer func() { embeddedCatalog = saved }()
	fn()
}

// --- runSearch tests ---

func TestSearchMatchByName(t *testing.T) {
	withCatalog([]byte(testCatalogTOML), func() {
		out := captureStdout(t, func() {
			captureStderr(func() {
				runSearch([]string{"json"})
			})
		})
		if !strings.Contains(out, "json") {
			t.Errorf("expected output to contain 'json', got: %s", out)
		}
		if !strings.Contains(out, "1 matching") {
			t.Errorf("expected '1 matching' in output, got: %s", out)
		}
	})
}

func TestSearchMatchByDescription(t *testing.T) {
	withCatalog([]byte(testCatalogTOML), func() {
		out := captureStdout(t, func() {
			captureStderr(func() {
				runSearch([]string{"parsing"})
			})
		})
		if !strings.Contains(out, "json") {
			t.Errorf("expected output to contain 'json' (matched by description), got: %s", out)
		}
	})
}

func TestSearchMultipleMatches(t *testing.T) {
	withCatalog([]byte(testCatalogTOML), func() {
		out := captureStdout(t, func() {
			captureStderr(func() {
				runSearch([]string{"string"})
			})
		})
		// "strings" module should match by name, "json" should not
		if !strings.Contains(out, "strings") {
			t.Errorf("expected output to contain 'strings', got: %s", out)
		}
	})
}

func TestSearchNoMatch(t *testing.T) {
	withCatalog([]byte(testCatalogTOML), func() {
		stderr := captureStderr(func() {
			captureStdout(t, func() {
				runSearch([]string{"nonexistent"})
			})
		})
		if !strings.Contains(stderr, "No modules matching") {
			t.Errorf("expected 'No modules matching' on stderr, got: %s", stderr)
		}
	})
}

func TestSearchEmptyCatalog(t *testing.T) {
	// When embeddedCatalog is nil, runSearch calls os.Exit(1).
	// Test via subprocess.
	if os.Getenv("TEST_SEARCH_EMPTY_CATALOG") == "1" {
		withCatalog(nil, func() {
			runSearch([]string{"json"})
		})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestSearchEmptyCatalog")
	cmd.Env = append(os.Environ(), "TEST_SEARCH_EMPTY_CATALOG=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit")
	}
	if !strings.Contains(string(out), "no catalog available") {
		t.Errorf("expected 'no catalog available' in output, got: %s", string(out))
	}
}

// setupGitTestEnv configures the test environment for git operations.
// It creates a temporary git global config with user identity and safe.directory=*,
// then sets GIT_CONFIG_GLOBAL, GIT_CONFIG_SYSTEM, and GIT_TERMINAL_PROMPT via t.Setenv
// so all git commands (including those inside runAdd/runUpdate) inherit the correct settings.
func setupGitTestEnv(t *testing.T) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "gitconfig")
	configContent := "[user]\n\temail = test@test.com\n\tname = Test\n[safe]\n\tdirectory = *\n"
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("setupGitTestEnv: write git config: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", configPath)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}

// --- runAdd tests ---

func TestAddEmbeddedModule(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(dir)

	// Create a promise.toml so runAdd doesn't fail on missing config
	os.WriteFile(filepath.Join(dir, "promise.toml"), []byte("[module]\nname = \"test\"\nepoch = \"2026.0\"\n"), 0644)

	withCatalog([]byte(testCatalogTOML), func() {
		out := captureStdout(t, func() {
			captureStderr(func() {
				runAdd([]string{"io"})
			})
		})
		if !strings.Contains(out, "is built-in") {
			t.Errorf("expected 'is built-in' for embedded module, got: %s", out)
		}
	})

	// promise.toml should be unchanged (no require added)
	content, _ := os.ReadFile(filepath.Join(dir, "promise.toml"))
	if strings.Contains(string(content), "[require]") {
		t.Error("promise.toml should not have [require] for built-in module")
	}
}

func TestAddNoPromiseToml(t *testing.T) {
	// runAdd calls os.Exit(1) when no promise.toml. Test via subprocess.
	if os.Getenv("TEST_ADD_NO_TOML") == "1" {
		dir := t.TempDir()
		os.Chdir(dir)
		withCatalog([]byte(testCatalogTOML), func() {
			runAdd([]string{"json"})
		})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestAddNoPromiseToml")
	cmd.Env = append(os.Environ(), "TEST_ADD_NO_TOML=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit")
	}
	if !strings.Contains(string(out), "no promise.toml") {
		t.Errorf("expected 'no promise.toml' in output, got: %s", string(out))
	}
}

func TestAddCatalogResolvesToURL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	setupGitTestEnv(t)
	bin := findPromiseBinary(t)
	testVerifyCompilerBin = bin
	defer func() { testVerifyCompilerBin = "" }()
	t.Setenv("PROMISE_HOME", t.TempDir())
	epoch := compilerEpochForTest(t)

	// Create a local bare git repo to act as the "remote", carrying a verifiable
	// module with an epoch-<E> tag so the epoch-aware resolver picks + verifies it.
	bareDir := filepath.ToSlash(t.TempDir())
	workDir := t.TempDir()
	projDir := t.TempDir()

	run := func(dir, name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
		}
	}

	run(bareDir, "git", "init", "--bare", ".")
	run(workDir, "git", "clone", bareDir, ".")
	writeMod(t, workDir, "mymod", true)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "init")
	run(workDir, "git", "tag", "epoch-"+epoch)
	run(workDir, "git", "push", "origin", "HEAD", "--tags")

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = workDir
	hashBytes, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	expectedHash := strings.TrimSpace(string(hashBytes))

	catalogTOML := `[catalog]
epoch = "` + epoch + `"

[modules.mymod]
url = "` + bareDir + `"
commit = "` + expectedHash + `"
description = "Test module"
`

	os.WriteFile(filepath.Join(projDir, "promise.toml"), []byte("[module]\nname = \"proj\"\nepoch = \""+epoch+"\"\n"), 0644)

	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(projDir)

	withCatalog([]byte(catalogTOML), func() {
		out := captureStdout(t, func() {
			captureStderr(func() {
				runAdd([]string{"mymod"})
			})
		})
		if !strings.Contains(out, "Added") {
			t.Errorf("expected 'Added' in output, got: %s", out)
		}
		if !strings.Contains(out, expectedHash[:12]) {
			t.Errorf("expected commit hash prefix %s in output, got: %s", expectedHash[:12], out)
		}
	})

	// Verify promise.toml was updated
	content, _ := os.ReadFile(filepath.Join(projDir, "promise.toml"))
	if !strings.Contains(string(content), bareDir) {
		t.Errorf("promise.toml should contain URL %s, got: %s", bareDir, string(content))
	}
	if !strings.Contains(string(content), expectedHash) {
		t.Errorf("promise.toml should contain commit hash, got: %s", string(content))
	}
}

// --- runPkgUpdate tests (dependency [require]-pin updating, `promise package update`) ---

func TestUpdateNoEntries(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(dir)

	os.WriteFile(filepath.Join(dir, "promise.toml"), []byte("[module]\nname = \"test\"\nepoch = \"2026.0\"\n"), 0644)

	out := captureStdout(t, func() {
		captureStderr(func() {
			runPkgUpdate(nil)
		})
	})
	if !strings.Contains(out, "No [require] entries") {
		t.Errorf("expected 'No [require] entries', got: %s", out)
	}
}

func TestUpdateSkipsSHA256Only(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(dir)

	// Create promise.toml with a SHA256-only named require (no commit field)
	toml := `[module]
name = "test"
epoch = "2026.0"

[require.archive]
url = "https://example.com/archive.tar.gz"
sha256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
`
	os.WriteFile(filepath.Join(dir, "promise.toml"), []byte(toml), 0644)

	out := captureStdout(t, func() {
		captureStderr(func() {
			runPkgUpdate(nil)
		})
	})
	if !strings.Contains(out, "skipped (non-git source)") {
		t.Errorf("expected 'skipped (non-git source)' for SHA256-only entry, got: %s", out)
	}
}

func TestUpdateNotFound(t *testing.T) {
	// runUpdate calls os.Exit(1) when target not found. Test via subprocess.
	if os.Getenv("TEST_UPDATE_NOT_FOUND") == "1" {
		dir := t.TempDir()
		os.Chdir(dir)
		os.WriteFile(filepath.Join(dir, "promise.toml"), []byte("[module]\nname = \"test\"\nepoch = \"2026.0\"\n"), 0644)
		runPkgUpdate([]string{"nonexistent"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestUpdateNotFound")
	cmd.Env = append(os.Environ(), "TEST_UPDATE_NOT_FOUND=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit")
	}
	if !strings.Contains(string(out), "no [require] entry matching") {
		t.Errorf("expected 'no [require] entry matching' in output, got: %s", string(out))
	}
}

func TestUpdateURLKeyedEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	setupGitTestEnv(t)
	testVerifyCompilerBin = findPromiseBinary(t)
	defer func() { testVerifyCompilerBin = "" }()
	t.Setenv("PROMISE_HOME", t.TempDir())
	epoch := compilerEpochForTest(t)

	// Create a local bare git repo with two module commits; the newer one carries
	// the epoch-<E> tag the resolver should re-resolve to (§9.8 step 3).
	bareDir := filepath.ToSlash(t.TempDir())
	workDir := t.TempDir()
	projDir := t.TempDir()

	run := func(dir, name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
		}
	}

	run(bareDir, "git", "init", "--bare", ".")
	run(workDir, "git", "clone", bareDir, ".")
	writeMod(t, workDir, "dep", true)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "first")
	run(workDir, "git", "push", "origin", "HEAD")

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = workDir
	firstHash, _ := cmd.Output()
	oldHash := strings.TrimSpace(string(firstHash))

	// Second commit — tagged epoch-<E>; update should re-resolve here.
	os.WriteFile(filepath.Join(workDir, "extra.pr"), []byte("extra_value() int `public { return 9; }\n"), 0644)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "second")
	run(workDir, "git", "tag", "epoch-"+epoch)
	run(workDir, "git", "push", "origin", "HEAD", "--tags")

	cmd = exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = workDir
	newHash, _ := cmd.Output()
	headHash := strings.TrimSpace(string(newHash))

	// Create promise.toml with URL-keyed require pinned to old commit
	toml := "[module]\nname = \"proj\"\nepoch = \"" + epoch + "\"\n\n[require]\n\"" + bareDir + "\" = \"" + oldHash + "\"\n"
	os.WriteFile(filepath.Join(projDir, "promise.toml"), []byte(toml), 0644)

	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(projDir)

	out := captureStdout(t, func() {
		captureStderr(func() {
			runPkgUpdate(nil)
		})
	})
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
	setupGitTestEnv(t)
	testVerifyCompilerBin = findPromiseBinary(t)
	defer func() { testVerifyCompilerBin = "" }()
	t.Setenv("PROMISE_HOME", t.TempDir())
	epoch := compilerEpochForTest(t)

	// Test updating a specific URL-keyed entry by URL
	bareDir := filepath.ToSlash(t.TempDir())
	workDir := t.TempDir()
	projDir := t.TempDir()

	run := func(dir, name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
		}
	}

	run(bareDir, "git", "init", "--bare", ".")
	run(workDir, "git", "clone", bareDir, ".")
	writeMod(t, workDir, "dep", true)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "init")
	run(workDir, "git", "tag", "epoch-"+epoch)
	run(workDir, "git", "push", "origin", "HEAD", "--tags")

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = workDir
	hashBytes, _ := cmd.Output()
	headHash := strings.TrimSpace(string(hashBytes))

	// Pin to the epoch-<E> commit already — re-resolution lands on the same commit
	// → "already up to date".
	toml := "[module]\nname = \"proj\"\nepoch = \"" + epoch + "\"\n\n[require]\n\"" + bareDir + "\" = \"" + headHash + "\"\n"
	os.WriteFile(filepath.Join(projDir, "promise.toml"), []byte(toml), 0644)

	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(projDir)

	out := captureStdout(t, func() {
		captureStderr(func() {
			runPkgUpdate([]string{bareDir})
		})
	})
	if !strings.Contains(out, "already up to date") {
		t.Errorf("expected 'already up to date', got: %s", out)
	}
}

func TestAddWithCustomRef(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	setupGitTestEnv(t)
	testVerifyCompilerBin = findPromiseBinary(t)
	defer func() { testVerifyCompilerBin = "" }()
	t.Setenv("PROMISE_HOME", t.TempDir())
	epoch := compilerEpochForTest(t)

	// Test the len(args)==2 path with a custom ref: the ref is resolved + verified
	// with no epoch-tag walk-back (the user pinned it).
	bareDir := filepath.ToSlash(t.TempDir())
	workDir := t.TempDir()
	projDir := t.TempDir()

	run := func(dir, name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
		}
	}

	run(bareDir, "git", "init", "--bare", ".")
	run(workDir, "git", "clone", bareDir, ".")
	writeMod(t, workDir, "dep", true)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "first")
	run(workDir, "git", "tag", "v1.0")
	run(workDir, "git", "push", "origin", "HEAD", "--tags")

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = workDir
	hashBytes, _ := cmd.Output()
	expectedHash := strings.TrimSpace(string(hashBytes))

	os.WriteFile(filepath.Join(projDir, "promise.toml"), []byte("[module]\nname = \"proj\"\nepoch = \""+epoch+"\"\n"), 0644)

	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(projDir)

	// No withCatalog wrapper: a bareDir path never matches a catalog name, so it
	// is treated as a raw URL — and runAdd needs the real embedded catalog to read
	// this compiler's epoch for verification.
	out := captureStdout(t, func() {
		captureStderr(func() {
			runAdd([]string{bareDir, "v1.0"})
		})
	})
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

func TestSearchUsageError(t *testing.T) {
	// runSearch with wrong number of args calls os.Exit(1)
	if os.Getenv("TEST_SEARCH_USAGE") == "1" {
		runSearch(nil)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestSearchUsageError")
	cmd.Env = append(os.Environ(), "TEST_SEARCH_USAGE=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit")
	}
	if !strings.Contains(string(out), "usage: promise package search") {
		t.Errorf("expected usage message, got: %s", string(out))
	}
}

func TestUpdateNamedEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping verify integration test in short mode")
	}
	setupGitTestEnv(t)
	testVerifyCompilerBin = findPromiseBinary(t)
	defer func() { testVerifyCompilerBin = "" }()
	t.Setenv("PROMISE_HOME", t.TempDir())
	epoch := compilerEpochForTest(t)

	// Test updating a [require.NAME] entry when the epoch-<E> tag has moved forward
	bareDir := filepath.ToSlash(t.TempDir())
	workDir := t.TempDir()
	projDir := t.TempDir()

	run := func(dir, name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
		}
	}

	run(bareDir, "git", "init", "--bare", ".")
	run(workDir, "git", "clone", bareDir, ".")
	writeMod(t, workDir, "dep", true)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "first")
	run(workDir, "git", "push", "origin", "HEAD")

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = workDir
	firstHash, _ := cmd.Output()
	oldHash := strings.TrimSpace(string(firstHash))

	// Second commit — carries the epoch-<E> tag the update should re-resolve to.
	os.WriteFile(filepath.Join(workDir, "extra.pr"), []byte("extra_value() int `public { return 9; }\n"), 0644)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "second")
	run(workDir, "git", "tag", "epoch-"+epoch)
	run(workDir, "git", "push", "origin", "HEAD", "--tags")

	cmd = exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = workDir
	newHash, _ := cmd.Output()
	headHash := strings.TrimSpace(string(newHash))

	// Create promise.toml with [require.mymod] pinned to old commit
	toml := "[module]\nname = \"proj\"\nepoch = \"" + epoch + "\"\n\n[require.mymod]\nurl = \"" + bareDir + "\"\ncommit = \"" + oldHash + "\"\n"
	os.WriteFile(filepath.Join(projDir, "promise.toml"), []byte(toml), 0644)

	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(projDir)

	out := captureStdout(t, func() {
		captureStderr(func() {
			runPkgUpdate(nil)
		})
	})
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
	setupGitTestEnv(t)
	testVerifyCompilerBin = findPromiseBinary(t)
	defer func() { testVerifyCompilerBin = "" }()
	t.Setenv("PROMISE_HOME", t.TempDir())
	epoch := compilerEpochForTest(t)

	// Create a local bare git repo
	bareDir := filepath.ToSlash(t.TempDir())
	workDir := t.TempDir()
	projDir := t.TempDir()

	run := func(dir, name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
		}
	}

	run(bareDir, "git", "init", "--bare", ".")
	run(workDir, "git", "clone", bareDir, ".")
	writeMod(t, workDir, "dep", true)
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

	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(projDir)

	out := captureStdout(t, func() {
		captureStderr(func() {
			runPkgUpdate(nil)
		})
	})
	if !strings.Contains(out, "already up to date") {
		t.Errorf("expected 'already up to date', got: %s", out)
	}
	if !strings.Contains(out, "Updated 0 of 1") {
		t.Errorf("expected 'Updated 0 of 1', got: %s", out)
	}
}

func TestAddUsageError(t *testing.T) {
	// runAdd with wrong number of args calls os.Exit(1). Test via subprocess.
	if os.Getenv("TEST_ADD_USAGE") == "1" {
		runAdd(nil)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestAddUsageError")
	cmd.Env = append(os.Environ(), "TEST_ADD_USAGE=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit")
	}
	if !strings.Contains(string(out), "usage: promise package add") {
		t.Errorf("expected usage message, got: %s", string(out))
	}
}

func TestUpdateUsageError(t *testing.T) {
	// runUpdate with more than one arg calls os.Exit(1). Test via subprocess.
	if os.Getenv("TEST_UPDATE_USAGE") == "1" {
		runPkgUpdate([]string{"a", "b"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestUpdateUsageError")
	cmd.Env = append(os.Environ(), "TEST_UPDATE_USAGE=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit")
	}
	if !strings.Contains(string(out), "usage: promise package update") {
		t.Errorf("expected usage message, got: %s", string(out))
	}
}

func TestUpdateNoPromiseToml(t *testing.T) {
	// runUpdate with no promise.toml calls os.Exit(1). Test via subprocess.
	if os.Getenv("TEST_UPDATE_NO_TOML") == "1" {
		dir := t.TempDir()
		os.Chdir(dir)
		runPkgUpdate(nil)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestUpdateNoPromiseToml")
	cmd.Env = append(os.Environ(), "TEST_UPDATE_NO_TOML=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit")
	}
	if !strings.Contains(string(out), "no promise.toml") {
		t.Errorf("expected 'no promise.toml' in output, got: %s", string(out))
	}
}

func TestAddInvalidCatalog(t *testing.T) {
	// runAdd with an invalid catalog calls os.Exit(1). Test via subprocess.
	if os.Getenv("TEST_ADD_INVALID_CATALOG") == "1" {
		dir := t.TempDir()
		os.Chdir(dir)
		os.WriteFile(filepath.Join(dir, "promise.toml"), []byte("[module]\nname = \"test\"\nepoch = \"2026.0\"\n"), 0644)
		withCatalog([]byte("[bad-header\n"), func() {
			runAdd([]string{"foo"})
		})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestAddInvalidCatalog")
	cmd.Env = append(os.Environ(), "TEST_ADD_INVALID_CATALOG=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit")
	}
	if !strings.Contains(string(out), "invalid catalog") {
		t.Errorf("expected 'invalid catalog' in output, got: %s", string(out))
	}
}
