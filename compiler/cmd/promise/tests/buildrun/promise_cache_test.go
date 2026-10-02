package buildrun

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// TestPromiseCacheRelocatesDerivedCaches drives the real compiler with
// PROMISE_CACHE set alongside the package's shared PROMISE_HOME (#99): what the
// compiler computes — the build and AST caches, the extracted embedded modules
// and the compiler stamp — lands under PROMISE_CACHE, and the run's outcome is
// the one it has without the variable.
func TestPromiseCacheRelocatesDerivedCaches(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)

	src := filepath.Join(clitest.TempDir(t), "relocated_test.pr")
	// A nonce keeps the first compile a genuine miss whatever is already cached.
	body := fmt.Sprintf("// promise-cache-%d\nrelocated() `test {\n  assert(1 + 1 == 2, \"sum\");\n}\n", time.Now().UnixNano())
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(clitest.TempDir(t), "derived")

	r := clitest.RunOK(t, bin, []string{"PROMISE_CACHE=" + cache}, "test", src)
	if !strings.Contains(r.Stdout, "1 passed") {
		t.Errorf("expected the test to pass under PROMISE_CACHE:%s", r.Detail())
	}
	for _, sub := range []string{"build", "astcache", "embedded_modules"} {
		entries, err := os.ReadDir(filepath.Join(cache, sub))
		if err != nil || len(entries) == 0 {
			t.Errorf("PROMISE_CACHE/%s holds nothing after a compile (err %v)", sub, err)
		}
	}
	if _, err := os.Stat(filepath.Join(cache, ".compiler_hash")); err != nil {
		t.Errorf("the compiler stamp was not recorded under PROMISE_CACHE: %v", err)
	}
}

// TestPromiseCacheBadValuesAreRefused covers the two ways the variable can be
// wrong. Each must stop the run with an error naming what is wrong, rather than
// quietly caching somewhere else: a relative path (it would resolve against
// each process's own working directory) and a path that cannot hold a cache at
// all. `check` is enough to reach the refusal — it is raised when the std
// module is loaded, before anything is compiled or linked.
func TestPromiseCacheBadValuesAreRefused(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)

	src := filepath.Join(clitest.TempDir(t), "refused_test.pr")
	if err := os.WriteFile(src, []byte("refused() `test {\n  assert(1 + 1 == 2, \"sum\");\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notADir := filepath.Join(clitest.TempDir(t), "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, value, want string }{
		{"relative", filepath.Join("relative", "cache"), "PROMISE_CACHE must be an absolute path"},
		{"not a directory", notADir, notADir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := clitest.RunFailing(t, bin, []string{"PROMISE_CACHE=" + tc.value}, "check", src)
			if !strings.Contains(r.Combined(), tc.want) {
				t.Errorf("the refusal does not name the problem (want %q):%s", tc.want, r.Detail())
			}
		})
	}
}
