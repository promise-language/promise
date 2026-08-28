package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promise-language/promise/compiler/internal/module"
)

// TestExtractRaceEmbeddedModule exercises the intra-process variant of the
// T1348 embedded-module extraction race: many goroutines (sharing one PID) call
// extractEmbeddedModule concurrently against a cold cache. With the old
// direct-write implementation a reader could observe a partially-populated
// cache dir; the atomic temp-dir + rename fix makes every call observe only a
// complete module.
func TestExtractRaceEmbeddedModule(t *testing.T) {
	t.Setenv("PROMISE_HOME", t.TempDir())

	// Reference content from the embedded FS for a byte-exact comparison.
	want, err := embeddedModules.ReadFile("resources/modules/net/promise.toml")
	if err != nil {
		t.Fatalf("read embedded net/promise.toml: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("embedded net/promise.toml is empty")
	}

	const n = 24
	start := make(chan struct{})
	var wg sync.WaitGroup
	fails := make([]string, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all at once to maximize contention
			dir, err := extractEmbeddedModule("net")
			if err != nil {
				fails[i] = "extractEmbeddedModule returned error: " + err.Error()
				return
			}
			// Read promise.toml immediately upon return, mirroring the caller
			// (ParseConfig) that observed a missing/partial file mid-extraction.
			got, err := os.ReadFile(filepath.Join(dir, "promise.toml"))
			if err != nil {
				fails[i] = "read extracted promise.toml: " + err.Error()
				return
			}
			if string(got) != string(want) {
				fails[i] = "extracted promise.toml content mismatch/partial"
				return
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		if fails[i] != "" {
			t.Fatalf("goroutine %d: %s", i, fails[i])
		}
	}

	// No leftover temp sibling dirs should remain in embedded_modules/.
	cacheDir, err := module.EmbeddedModuleCacheDir("net")
	if err != nil {
		t.Fatalf("EmbeddedModuleCacheDir: %v", err)
	}
	parent := filepath.Dir(cacheDir)
	siblings, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read embedded_modules dir: %v", err)
	}
	for _, s := range siblings {
		if strings.HasPrefix(s.Name(), ".net.tmp.") {
			t.Fatalf("leftover temp dir not cleaned up: %s", s.Name())
		}
	}
}

// TestExtractEmbeddedModuleStaleEmptyCacheDir covers the destination-exists
// publish path: an empty leftover cache dir (a racing or crashed cleaner that
// removed a module's files but not the directory itself) must not wedge
// extraction. On Windows this is the failure that made bin/verify die with
// "rename ...\.path.tmp.N: Access is denied." — MoveFileEx refuses to replace
// any existing directory, empty or not.
func TestExtractEmbeddedModuleStaleEmptyCacheDir(t *testing.T) {
	t.Setenv("PROMISE_HOME", t.TempDir())

	cacheDir, err := module.EmbeddedModuleCacheDir("net")
	if err != nil {
		t.Fatalf("EmbeddedModuleCacheDir: %v", err)
	}
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("pre-create empty cache dir: %v", err)
	}

	dir, err := extractEmbeddedModule("net")
	if err != nil {
		t.Fatalf("extract over empty cache dir: %v", err)
	}
	if dir != cacheDir {
		t.Fatalf("extract returned %q, want %q", dir, cacheDir)
	}
	want, err := embeddedModules.ReadFile("resources/modules/net/promise.toml")
	if err != nil {
		t.Fatalf("read embedded net/promise.toml: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "promise.toml"))
	if err != nil {
		t.Fatalf("read extracted promise.toml: %v", err)
	}
	if string(got) != string(want) {
		t.Fatal("extracted promise.toml content mismatch")
	}
}

// TestPublishExtractedModulePeerWins covers the cross-process race directly: a
// peer already published a complete module, so our own rename must fail (the
// destination exists) yet publish must report success and leave the peer's
// content untouched — a module is only ever made visible by an atomic rename,
// so any non-empty destination is complete.
func TestPublishExtractedModulePeerWins(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	cacheDir := filepath.Join(parent, "net")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "promise.toml"), []byte("peer"), 0644); err != nil {
		t.Fatal(err)
	}

	tmpDir := filepath.Join(parent, ".net.tmp.1.1")
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "promise.toml"), []byte("ours"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := publishExtractedModule(tmpDir, cacheDir); err != nil {
		t.Fatalf("publish against a populated peer dir: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(cacheDir, "promise.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "peer" {
		t.Fatalf("peer content overwritten: got %q, want %q", got, "peer")
	}
}

// TestPublishExtractedModuleGivesUp guards the retry budget's exit: an
// unpublishable staging dir (it does not exist, so every rename fails and the
// destination never appears) must return the underlying error after a bounded
// number of attempts rather than spinning forever.
func TestPublishExtractedModuleGivesUp(t *testing.T) {
	t.Parallel()
	defer func(a int, b, m time.Duration) {
		extractPublishAttempts, extractPublishBackoff, extractPublishMaxSleep = a, b, m
	}(extractPublishAttempts, extractPublishBackoff, extractPublishMaxSleep)
	extractPublishAttempts, extractPublishBackoff, extractPublishMaxSleep = 3, time.Millisecond, time.Millisecond

	parent := t.TempDir()
	err := publishExtractedModule(filepath.Join(parent, "does-not-exist"), filepath.Join(parent, "net"))
	if err == nil {
		t.Fatal("expected an error when the staging dir cannot be published")
	}
	if _, statErr := os.Stat(filepath.Join(parent, "net")); !os.IsNotExist(statErr) {
		t.Fatalf("failed publish must not leave a destination behind: stat err=%v", statErr)
	}
}

// TestExtractEmbeddedModuleUnknown covers the early error path: a module with no
// embedded source must fail cleanly (before any cache dir is touched) rather than
// creating an empty/partial directory.
func TestExtractEmbeddedModuleUnknown(t *testing.T) {
	t.Setenv("PROMISE_HOME", t.TempDir())

	dir, err := extractEmbeddedModule("no_such_embedded_module")
	if err == nil {
		t.Fatalf("expected error for unknown module, got dir=%q", dir)
	}
	if !strings.Contains(err.Error(), "no embedded source for module") {
		t.Fatalf("unexpected error message: %v", err)
	}
	if dir != "" {
		t.Fatalf("expected empty dir on error, got %q", dir)
	}

	// The bogus module must not have created a cache directory.
	cacheDir, err := module.EmbeddedModuleCacheDir("no_such_embedded_module")
	if err != nil {
		t.Fatalf("EmbeddedModuleCacheDir: %v", err)
	}
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Fatalf("cache dir should not exist for unknown module: stat err=%v", err)
	}
}

// TestExtractEmbeddedModuleIdempotent deterministically covers the fast-path
// reuse branch: a second extraction against a warm cache must return the same
// populated directory without re-extracting, and must never leave a temp sibling
// behind. The concurrent test only hits this branch nondeterministically.
func TestExtractEmbeddedModuleIdempotent(t *testing.T) {
	t.Setenv("PROMISE_HOME", t.TempDir())

	want, err := embeddedModules.ReadFile("resources/modules/net/promise.toml")
	if err != nil {
		t.Fatalf("read embedded net/promise.toml: %v", err)
	}

	first, err := extractEmbeddedModule("net")
	if err != nil {
		t.Fatalf("first extract: %v", err)
	}
	fi, err := os.Stat(filepath.Join(first, "promise.toml"))
	if err != nil {
		t.Fatalf("stat after first extract: %v", err)
	}

	// Second call must take the fast path: same dir, unchanged file (same
	// mtime — proving it was not rewritten).
	second, err := extractEmbeddedModule("net")
	if err != nil {
		t.Fatalf("second extract: %v", err)
	}
	if second != first {
		t.Fatalf("fast path returned different dir: %q vs %q", first, second)
	}
	fi2, err := os.Stat(filepath.Join(second, "promise.toml"))
	if err != nil {
		t.Fatalf("stat after second extract: %v", err)
	}
	if !fi2.ModTime().Equal(fi.ModTime()) {
		t.Fatalf("fast path re-wrote promise.toml (mtime changed)")
	}

	got, err := os.ReadFile(filepath.Join(second, "promise.toml"))
	if err != nil {
		t.Fatalf("read extracted promise.toml: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("extracted promise.toml content mismatch")
	}

	// No leftover temp sibling dirs.
	siblings, err := os.ReadDir(filepath.Dir(first))
	if err != nil {
		t.Fatalf("read embedded_modules dir: %v", err)
	}
	for _, s := range siblings {
		if strings.HasPrefix(s.Name(), ".net.tmp.") {
			t.Fatalf("leftover temp dir not cleaned up: %s", s.Name())
		}
	}
}
