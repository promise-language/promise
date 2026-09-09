package testrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// srcDirModuleTest asserts inside the compiled test binary that os.src_dir is
// the module directory. It compares against os.working_dir, which the caller
// makes equal by running `promise test .` from the module directory itself — so
// a binary carrying some *other* module's baked path fails here, which is what a
// cache collision would produce. The caller passes a symlink-free directory (see
// realTempDir) so that the compiler's filepath.Abs and the binary's getcwd spell
// it the same way and the comparison is about the value, not about symlinks.
const srcDirModuleTest = `use os;

test_src_dir_is_the_module_dir() ` + "`test" + ` {
  string cwd = os.working_dir?!;
  if d := os.src_dir {
    assert(d == cwd, "src_dir " + d + " should be the module dir " + cwd);
  } else {
    assert(false, "src_dir should be present in a module test");
  }
}
`

// realTempDir returns a fresh temp directory with every symlink resolved.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return dir
}

// TestModuleTestSrcDirIsModuleDir covers the module-test frontend and its own
// cache key, the third of the three key paths T1521 had to teach about the source
// directory (run binary, single-file test, module test). Two byte-identical
// module trees in different directories run under this package's shared
// PROMISE_HOME, so they share one build cache: before the module-test key folded
// in the module directory, the second `promise test` was handed the first tree's
// binary, and the assertion inside it — src_dir must be where this module lives —
// would fail.
func TestModuleTestSrcDirIsModuleDir(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping module test integration test in short mode")
	}
	bin := clitest.Bin(t)
	epoch := clitest.CompilerEpoch(t)

	// Inherits this package's isolated PROMISE_HOME (clitest.IsolateHome, in
	// TestMain), so both invocations share one build cache — which is the point
	// when the thing under test is a cache key.
	runTest := func(dir string) (string, error) {
		cmd := exec.Command(bin, "test", ".")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	writeModule := func(dir string) {
		write := func(name, content string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}
		write("promise.toml", "[module]\nname = \"probe\"\nepoch = \""+epoch+"\"\n")
		write("probe.pr", "probe_value() int `public { return 1; }\n")
		write("probe_test.pr", srcDirModuleTest)
	}

	// Real paths, not t.TempDir()'s: on macOS that is a path under a symlink
	// (/var → /private/var), and the compiler's filepath.Abs(".") keeps the
	// spelling of the cwd it is handed while the test binary's os.working_dir
	// (getcwd) resolves it. Naming the resolved directory keeps both on one
	// spelling, so the assertion is about the value and not about symlinks.
	dirA, dirB := realTempDir(t), realTempDir(t)
	writeModule(dirA)
	writeModule(dirB)

	for _, dir := range []string{dirA, dirB} {
		out, err := runTest(dir)
		if err != nil {
			t.Fatalf("promise test . in %s: %v\n%s", dir, err, out)
		}
		if !strings.Contains(out, "1 passed, 0 failed") {
			t.Errorf("promise test . in %s did not report a clean pass:\n%s", dir, out)
		}
	}
}
