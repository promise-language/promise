package buildrun

// Black-box tests for `promise check` (T2085). The command resolves its target
// the way build does and analyses a UNIT — a project directory, tests included,
// or a .pr file that belongs to no project — so these assert on what the built
// binary does, not on an unexported function.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// writeFiles creates dir/name for each entry, failing the test on any error.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

// runCheck runs `promise check args...` in dir and returns stdout, stderr and
// whether it exited zero. stdout and stderr are kept apart deliberately: the
// result line is stdout's contract and the diagnostics are stderr's.
func runCheck(t *testing.T, dir string, args ...string) (stdout, stderr string, ok bool) {
	t.Helper()
	cmd := exec.Command(clitest.Bin(t), append([]string{"check"}, args...)...)
	cmd.Dir = dir
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return out.String(), errBuf.String(), err == nil
}

// A multi-file module is checkable as a directory and not as one of its files:
// the whole point of the unit. modules/std/vector.pr referring to _FnIter in
// iter.pr is the shape this reproduces.
func TestCheckMultiFileModuleAsUnit(t *testing.T) {
	t.Parallel()
	dir := clitest.TempDir(t)
	writeFiles(t, dir, map[string]string{
		"promise.toml": "[module]\nname = \"shapes\"\nepoch = \"2026.0\"\n",
		"point.pr":     "type Pt `public {\n  int x;\n}\n",
		"shapes.pr":    "make_pt(int x) Pt `public {\n  return Pt(x: x);\n}\n",
	})

	stdout, stderr, ok := runCheck(t, dir, ".")
	if !ok {
		t.Fatalf("checking the module as a unit failed\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	if got := strings.TrimSpace(stdout); got != "ok ." {
		t.Errorf("result line = %q, want %q", got, "ok .")
	}
}

func TestCheckFileInsideProjectNamesTheProject(t *testing.T) {
	t.Parallel()
	dir := clitest.TempDir(t)
	writeFiles(t, dir, map[string]string{
		"promise.toml": "[module]\nname = \"shapes\"\nepoch = \"2026.0\"\n",
		"point.pr":     "type Pt `public {\n  int x;\n}\n",
		"shapes.pr":    "make_pt(int x) Pt `public {\n  return Pt(x: x);\n}\n",
	})

	stdout, stderr, ok := runCheck(t, dir, "shapes.pr")
	if ok {
		t.Fatalf("checking one file of a project should fail\nstdout: %s", stdout)
	}
	if !strings.Contains(stderr, "belongs to the project at") {
		t.Errorf("stderr should point at the project, got: %s", stderr)
	}
	if !strings.Contains(stdout, "FAIL shapes.pr") {
		t.Errorf("stdout should carry a FAIL result line, got: %q", stdout)
	}
}

// A module's tests are part of its unit: they are only analysable in their
// module's context, and nothing else in the tree can check them.
func TestCheckIncludesModuleTestFiles(t *testing.T) {
	t.Parallel()
	dir := clitest.TempDir(t)
	writeFiles(t, dir, map[string]string{
		"promise.toml": "[module]\nname = \"lib\"\nepoch = \"2026.0\"\n",
		"lib.pr":       "double(int x) int `public {\n  return x * 2;\n}\n",
		"lib_test.pr":  "test_double() `test {\n  assert(no_such_function() == 2, \"two\");\n}\n",
	})

	stdout, stderr, ok := runCheck(t, dir, ".")
	if ok {
		t.Fatalf("a broken test file must fail the unit\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	if !strings.Contains(stderr, "no_such_function") {
		t.Errorf("the test file was not analysed; stderr: %s", stderr)
	}
}

// Warnings are not failures: the compiler builds through them, so check reports
// them under their own outcome and still exits zero.
func TestCheckWarningsDoNotFail(t *testing.T) {
	t.Parallel()
	dir := clitest.TempDir(t)
	writeFiles(t, dir, map[string]string{
		"main.pr": "use path;\n\nmain() {\n  print_line(\"hi\");\n}\n",
	})

	stdout, stderr, ok := runCheck(t, dir, "main.pr")
	if !ok {
		t.Fatalf("an unused import is a warning, not a failure\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	if got := strings.TrimSpace(stdout); got != "warn main.pr (1 warning)" {
		t.Errorf("result line = %q, want %q", got, "warn main.pr (1 warning)")
	}
	if !strings.Contains(stderr, "unused import") {
		t.Errorf("the warning should still be printed; stderr: %s", stderr)
	}
}

func TestCheckErrorsExitNonZeroAndCount(t *testing.T) {
	t.Parallel()
	dir := clitest.TempDir(t)
	writeFiles(t, dir, map[string]string{
		"main.pr": "main() {\n  int a = nope_one();\n  int b = nope_two();\n}\n",
	})

	stdout, _, ok := runCheck(t, dir, "main.pr")
	if ok {
		t.Fatal("a file with errors must exit non-zero")
	}
	if got := strings.TrimSpace(stdout); got != "FAIL main.pr (2 errors)" {
		t.Errorf("result line = %q, want %q", got, "FAIL main.pr (2 errors)")
	}
}

// "Could not check" is readable from the exit code, without scraping stdout —
// the property the gate's caller needs and a person reading a script needs.
func TestCheckMissingFileExitsNonZero(t *testing.T) {
	t.Parallel()
	dir := clitest.TempDir(t)
	writeFiles(t, dir, map[string]string{"main.pr": "main() {}\n"})

	stdout, stderr, ok := runCheck(t, dir, "no/such/file.pr")
	if ok {
		t.Fatalf("a missing file must exit non-zero\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	if !strings.Contains(stderr, "error reading") {
		t.Errorf("stderr should say the file could not be read, got: %s", stderr)
	}
}

// A sweep reports every unit once and ends with one summary line, whose numbers
// are what the checked:promise gate reads.
func TestCheckSweepSummary(t *testing.T) {
	t.Parallel()
	dir := clitest.TempDir(t)
	writeFiles(t, dir, map[string]string{
		"a.pr":                  "main() {\n  print_line(\"a\");\n}\n",
		"b.pr":                  "helper() int {\n  return 1;\n}\n",
		"bad.pr":                "main() {\n  int x = missing_call();\n}\n",
		"mod/promise.toml":      "[module]\nname = \"mod\"\nepoch = \"2026.0\"\n",
		"mod/one.pr":            "type Thing `public {\n  int n;\n}\n",
		"mod/two.pr":            "make(int n) Thing `public {\n  return Thing(n: n);\n}\n",
		"nested/promise.toml":   "[module]\nname = \"nested\"\nepoch = \"2026.0\"\n",
		"nested/deep/three.pr":  "helper_in_subdir() int `public {\n  return 3;\n}\n",
		"nested/nested_test.pr": "test_helper() `test {\n  assert(helper_in_subdir() == 3, \"three\");\n}\n",
	})

	stdout, stderr, ok := runCheck(t, dir, "./...")
	if ok {
		t.Fatalf("the sweep contains a broken file, so it must exit non-zero\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	// 5 units: a.pr, b.pr, bad.pr, mod/, nested/ — a project is one unit, and a
	// .pr file under one is never a unit of its own.
	want := "4 checked, 0 warned, 1 failed (5 units, 1 error, 0 warnings"
	if !strings.Contains(stdout, want) {
		t.Errorf("summary should contain %q, got:\n%s", want, stdout)
	}
	if !strings.Contains(stdout, "FAILED:") || !strings.Contains(stdout, "bad.pr (1 error)") {
		t.Errorf("summary should name the failing unit, got:\n%s", stdout)
	}
	// The nested module's subdirectory file belongs to its module, never to a
	// unit of its own.
	if strings.Contains(stdout, "three.pr") {
		t.Errorf("a file inside a project must not be its own unit, got:\n%s", stdout)
	}
}

// A sweep counts a warned unit under its own outcome — not as a pass, not as a
// failure — and still exits zero. The warning diagnostics reach the summary's
// total, which is what promise_check_warnings is.
func TestCheckSweepWarnedUnit(t *testing.T) {
	t.Parallel()
	dir := clitest.TempDir(t)
	writeFiles(t, dir, map[string]string{
		"a.pr":     "helper_one() int {\n  return 1;\n}\n",
		"warny.pr": "use path;\n\nhelper_two() int {\n  return 2;\n}\n",
	})

	stdout, stderr, ok := runCheck(t, dir, "./...")
	if !ok {
		t.Fatalf("a warning is not a failure\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	if !strings.Contains(stdout, "1 checked, 1 warned, 0 failed (2 units, 0 errors, 1 warning") {
		t.Errorf("the warned unit should be counted under its own outcome, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "warn warny.pr (1 warning)") {
		t.Errorf("the warned unit's line should name its count, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "FAILED:") {
		t.Errorf("a warned unit must not appear in the failure list, got:\n%s", stdout)
	}
}

// A project directory with no sources is an error when it is named and no unit
// at all when it is swept — a reserved catalog name holding only a manifest
// must not fail a sweep of everything.
func TestCheckEmptyProject(t *testing.T) {
	t.Parallel()
	dir := clitest.TempDir(t)
	writeFiles(t, dir, map[string]string{
		"empty/promise.toml": "[module]\nname = \"empty\"\nepoch = \"2026.0\"\n",
		"a.pr":               "main() {\n  print_line(\"a\");\n}\n",
		"b.pr":               "helper() int {\n  return 1;\n}\n",
	})

	_, stderr, ok := runCheck(t, dir, "empty")
	if ok {
		t.Fatal("naming an empty project must be an error")
	}
	if !strings.Contains(stderr, "contains no .pr files") {
		t.Errorf("stderr should say the project is empty, got: %s", stderr)
	}

	stdout, stderr, ok := runCheck(t, dir, "./...")
	if !ok {
		t.Fatalf("an empty project must not fail a sweep\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	if !strings.Contains(stdout, "2 checked, 0 warned, 0 failed (2 units") {
		t.Errorf("the empty project should contribute no unit, got:\n%s", stdout)
	}
}

// Naming a project checks the project, not the .pr files that happen to sit at
// its top level — a module whose sources all live in a subdirectory is still
// one unit, and discoverProject is what decides what belongs to it.
func TestCheckProjectWithSourcesOnlyInSubdirectory(t *testing.T) {
	t.Parallel()
	dir := clitest.TempDir(t)
	writeFiles(t, dir, map[string]string{
		"promise.toml": "[module]\nname = \"deep\"\nepoch = \"2026.0\"\n",
		"src/one.pr":   "type Thing `public {\n  int n;\n}\n",
		"src/two.pr":   "make(int n) Thing `public {\n  return Thing(n: n);\n}\n",
	})

	stdout, stderr, ok := runCheck(t, dir, ".")
	if !ok {
		t.Fatalf("a project with no top-level sources is still one unit\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	if got := strings.TrimSpace(stdout); got != "ok ." {
		t.Errorf("result line = %q, want %q", got, "ok .")
	}
}

// A path is any string, and the parent of a sweep matches the unit it asked
// about by equality — a name carrying the same punctuation as the result line's
// count clause must not be read as a unit that reported nothing.
func TestCheckSweepUnitNameWithParentheses(t *testing.T) {
	t.Parallel()
	dir := clitest.TempDir(t)
	writeFiles(t, dir, map[string]string{
		"a (copy).pr": "helper_one() int {\n  return 1;\n}\n",
		"b.pr":        "helper_two() int {\n  return 2;\n}\n",
	})

	stdout, stderr, ok := runCheck(t, dir, "./...")
	if !ok {
		t.Fatalf("both files check clean\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	if !strings.Contains(stdout, "2 checked, 0 warned, 0 failed (2 units") {
		t.Errorf("summary should report both units clean, got:\n%s", stdout)
	}
}

func TestCheckRejectsUnknownFlag(t *testing.T) {
	t.Parallel()
	dir := clitest.TempDir(t)
	writeFiles(t, dir, map[string]string{"main.pr": "main() {}\n"})

	_, stderr, ok := runCheck(t, dir, "-nosuchflag", "main.pr")
	if ok {
		t.Fatal("an unknown flag must not be treated as a target")
	}
	if !strings.Contains(stderr, "unknown flag") {
		t.Errorf("stderr should name the unknown flag, got: %s", stderr)
	}
}

func TestCheckNoTargetsPrintsUsage(t *testing.T) {
	t.Parallel()
	dir := clitest.TempDir(t)

	_, stderr, ok := runCheck(t, dir)
	if ok {
		t.Fatal("a bare `promise check` must not exit zero")
	}
	if !strings.Contains(stderr, "usage: promise check") {
		t.Errorf("stderr should print usage, got: %s", stderr)
	}
}
