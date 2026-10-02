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
// compiler computes — the build and AST caches — lands under PROMISE_CACHE, and
// the run's outcome is the one it has without the variable.
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
	for _, sub := range []string{"build", "astcache"} {
		entries, err := os.ReadDir(filepath.Join(cache, sub))
		if err != nil || len(entries) == 0 {
			t.Errorf("PROMISE_CACHE/%s holds nothing after a compile (err %v)", sub, err)
		}
	}
}
