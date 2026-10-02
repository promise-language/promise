package module

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheRootDefaultsUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PROMISE_HOME", home)
	for _, v := range []string{"unset", ""} {
		if v == "unset" {
			t.Setenv("PROMISE_CACHE", "x") // registers restoration
			os.Unsetenv("PROMISE_CACHE")
		} else {
			t.Setenv("PROMISE_CACHE", v)
		}
		got, err := CacheRoot()
		if err != nil {
			t.Fatalf("PROMISE_CACHE %s: %v", v, err)
		}
		if want := filepath.Join(home, "cache"); got != want {
			t.Errorf("PROMISE_CACHE %s: CacheRoot() = %q, want %q", v, got, want)
		}
	}
}

func TestCacheRootAbsoluteOverride(t *testing.T) {
	t.Setenv("PROMISE_HOME", t.TempDir())
	cache := filepath.Join(t.TempDir(), "derived")
	t.Setenv("PROMISE_CACHE", cache)
	got, err := CacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	if got != cache {
		t.Errorf("CacheRoot() = %q, want %q", got, cache)
	}
}

// The override needs no home at all. With PROMISE_HOME unset and no user home
// to fall back on, PromiseHome cannot answer — and CacheRoot must not ask it
// when PROMISE_CACHE already says where the derived caches go. Without the
// override the same state is an error, never a cache rooted at "".
func TestCacheRootOverrideNeedsNoHome(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "derived") // before the env is blanked: TempDir may read it
	t.Setenv("PROMISE_HOME", "")
	t.Setenv("HOME", "")        // os.UserHomeDir on Unix
	t.Setenv("USERPROFILE", "") // and on Windows
	if home, err := PromiseHome(); err == nil {
		t.Fatalf("the fixture still resolves a home (%q); the test would prove nothing", home)
	}

	t.Setenv("PROMISE_CACHE", cache)
	got, err := CacheRoot()
	if err != nil {
		t.Fatalf("CacheRoot() with no home: %v", err)
	}
	if got != cache {
		t.Errorf("CacheRoot() = %q, want %q", got, cache)
	}

	t.Setenv("PROMISE_CACHE", "")
	if root, err := CacheRoot(); err == nil {
		t.Errorf("CacheRoot() with no home and no override = %q, want an error", root)
	}
}

func TestCacheRootRejectsRelative(t *testing.T) {
	t.Setenv("PROMISE_HOME", t.TempDir())
	t.Setenv("PROMISE_CACHE", filepath.Join("relative", "cache"))
	_, err := CacheRoot()
	if err == nil {
		t.Fatal("CacheRoot() accepted a relative PROMISE_CACHE")
	}
	if !strings.Contains(err.Error(), "PROMISE_CACHE") || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error %q should name PROMISE_CACHE and say it must be absolute", err)
	}
	// Every derived-cache path reports the same error rather than falling back.
	if _, err := BuildCacheDir(); err == nil {
		t.Error("BuildCacheDir() succeeded with a relative PROMISE_CACHE")
	}
}

// TestPromiseCacheRelocatesDerivedCaches: what the compiler computes moves to
// PROMISE_CACHE, and nothing is created under <home>/cache for it.
func TestPromiseCacheRelocatesDerivedCaches(t *testing.T) {
	home := t.TempDir()
	cache := filepath.Join(t.TempDir(), "derived")
	t.Setenv("PROMISE_HOME", home)
	t.Setenv("PROMISE_CACHE", cache)

	build, err := BuildCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cache, "build"); build != want {
		t.Errorf("BuildCacheDir() = %q, want %q", build, want)
	}
	stamp, err := CompilerStampPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cache, compilerStampFile); stamp != want {
		t.Errorf("CompilerStampPath() = %q, want %q", stamp, want)
	}
	emb, err := EmbeddedModuleCacheDir("std")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(emb, filepath.Join(cache, "embedded_modules")+string(filepath.Separator)) {
		t.Errorf("EmbeddedModuleCacheDir() = %q, want under %q", emb, cache)
	}
	if _, err := os.Stat(filepath.Join(home, "cache")); !os.IsNotExist(err) {
		t.Errorf("<home>/cache was created (stat err %v); derived caches belong under PROMISE_CACHE", err)
	}
}

// CleanBuildCache empties the relocated build cache and never reaches into the
// home's: a `promise clean` under PROMISE_CACHE must not take a build cache it
// is not using with it.
func TestCleanBuildCacheUnderPromiseCache(t *testing.T) {
	home := t.TempDir()
	cache := filepath.Join(t.TempDir(), "derived")
	t.Setenv("PROMISE_HOME", home)
	t.Setenv("PROMISE_CACHE", cache)

	relocated := filepath.Join(cache, "build", "ab", "abc.o")
	homes := filepath.Join(home, "cache", "build", "cd", "cde.o")
	for _, f := range []string{relocated, homes} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("obj"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := CleanBuildCache(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(cache, "build")); len(entries) != 0 {
		t.Errorf("PROMISE_CACHE/build still holds %d entries after the clean", len(entries))
	}
	if _, err := os.Stat(homes); err != nil {
		t.Errorf("the clean reached into <home>/cache/build: %v", err)
	}
}

// CleanEmbeddedModuleCache renames the tree aside inside the cache root, so the
// rename never crosses from PROMISE_CACHE into the home's filesystem.
func TestCleanEmbeddedModuleCacheUnderPromiseCache(t *testing.T) {
	home := t.TempDir()
	cache := filepath.Join(t.TempDir(), "derived")
	t.Setenv("PROMISE_HOME", home)
	t.Setenv("PROMISE_CACHE", cache)

	dir, err := EmbeddedModuleCacheDir("std")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CleanEmbeddedModuleCache(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("cache root not empty after clean: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(home, "cache")); !os.IsNotExist(err) {
		t.Errorf("<home>/cache was touched by the clean (stat err %v)", err)
	}
}

// CleanCRTCache removes extractions from PROMISE_CACHE and views from the home,
// and leaves the content-addressed store alone.
func TestCleanCRTCacheSplitsCacheRootAndHome(t *testing.T) {
	home := t.TempDir()
	cache := filepath.Join(t.TempDir(), "derived")
	t.Setenv("PROMISE_HOME", home)
	t.Setenv("PROMISE_CACHE", cache)

	gone := []string{
		filepath.Join(cache, "crt", "aarch64-linux-musl"),
		filepath.Join(cache, "compiler-rt", "aarch64-linux-musl"),
		filepath.Join(home, "cache", "crt-view", "aarch64-linux-musl-deadbeef"),
		filepath.Join(home, "cache", "compiler-rt-view", "aarch64-linux-musl-deadbeef"),
	}
	kept := filepath.Join(home, "cache", "blobs", "sha256", "deadbeef")
	for _, d := range append(gone, kept) {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := CleanCRTCache(); err != nil {
		t.Fatal(err)
	}
	for _, d := range gone {
		if _, err := os.Stat(filepath.Dir(d)); !os.IsNotExist(err) {
			t.Errorf("%s survived CleanCRTCache (stat err %v)", filepath.Dir(d), err)
		}
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("CleanCRTCache removed the store: %v", err)
	}
}
