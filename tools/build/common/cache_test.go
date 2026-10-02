package common

import (
	"os"
	"path/filepath"
	"testing"
)

// isolateLocalCacheEnv registers restoration of every variable SetupLocalCache
// sets, so a test that reaches it — directly, or through the gate's build —
// leaves this process's environment as it found it. t.Setenv restores the value
// seen here even after production code has called os.Setenv on the same name.
func isolateLocalCacheEnv(t *testing.T) {
	t.Helper()
	for _, name := range append([]string{"PROMISE_HOME", "PROMISE_CACHE"}, tempDirVars...) {
		t.Setenv(name, os.Getenv(name))
	}
}

// TestSetupLocalCacheClearsInheritedPromiseCache: a worktree command keeps its
// whole Promise cache in .promise-home, so a PROMISE_CACHE inherited from the
// caller (forge sets one for every child) must not split it across two roots.
func TestSetupLocalCacheClearsInheritedPromiseCache(t *testing.T) {
	isolateLocalCacheEnv(t)
	t.Setenv("PROMISE_HOME", "")
	t.Setenv("PROMISE_CACHE", filepath.Join(t.TempDir(), "inherited"))

	root := t.TempDir()
	if err := SetupLocalCache(root); err != nil {
		t.Fatalf("SetupLocalCache: %v", err)
	}
	if v, ok := os.LookupEnv("PROMISE_CACHE"); ok {
		t.Errorf("PROMISE_CACHE = %q after SetupLocalCache, want unset", v)
	}
	if got, want := os.Getenv("PROMISE_HOME"), filepath.Join(root, ".promise-home"); got != want {
		t.Errorf("PROMISE_HOME = %q, want %q", got, want)
	}
}

// TestSetupLocalCacheRedirectsTempOnEveryPlatform: the temp directory is read
// as TMPDIR on Unix and as TMP/TEMP on Windows, so redirecting only TMPDIR left
// every Windows run's temp files in the machine-global %TEMP% (#102). All three
// are set on every platform, to the one directory inside the worktree home.
func TestSetupLocalCacheRedirectsTempOnEveryPlatform(t *testing.T) {
	isolateLocalCacheEnv(t)
	elsewhere := t.TempDir()
	for _, name := range tempDirVars {
		t.Setenv(name, elsewhere)
	}

	root := t.TempDir()
	if err := SetupLocalCache(root); err != nil {
		t.Fatalf("SetupLocalCache: %v", err)
	}
	want := filepath.Join(root, ".promise-home", "tmp")
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := os.TempDir(); got != want {
		t.Errorf("os.TempDir() = %q, want %q — this platform reads its temp directory from a variable SetupLocalCache did not set", got, want)
	}
	if !Exists(want) {
		t.Errorf("%s was not created", want)
	}
}
