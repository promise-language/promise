package buildrun

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// TestBuildProjectMultiFile verifies that `promise build .` in a directory
// with a promise.toml and multiple .pr files compiles them all together and
// names the binary after the [module].name field.
func TestBuildProjectMultiFile(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"myapp\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.pr"),
		[]byte("main!() {\n  h := Helper(value: 7);\n  print_line(\"{h.value}\");\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "helper.pr"),
		[]byte("type Helper { int value; }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "build", ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	binaryName := "myapp"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binPath := filepath.Join(dir, binaryName)
	if _, err := os.Stat(binPath); err != nil {
		t.Fatalf("expected binary at %s, got: %v\noutput: %s", binPath, err, out)
	}

	// Run the produced binary and verify output.
	runCmd := exec.Command(binPath)
	runOut, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running %s failed: %v\n%s", binPath, err, runOut)
	}
	if got := strings.TrimSpace(string(runOut)); got != "7" {
		t.Errorf("output = %q, want %q", got, "7")
	}
}

// TestBuildProjectOutputOverride verifies that -o overrides the project
// name as the binary name.
func TestBuildProjectOutputOverride(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"myapp\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.pr"),
		[]byte("main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	customName := "custom"
	if runtime.GOOS == "windows" {
		customName += ".exe"
	}

	cmd := exec.Command(bin, "build", "-o", customName, ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	if _, err := os.Stat(filepath.Join(dir, customName)); err != nil {
		t.Errorf("expected custom-named binary at %s, got: %v", customName, err)
	}
	// And the project-name binary should NOT exist.
	myappName := "myapp"
	if runtime.GOOS == "windows" {
		myappName += ".exe"
	}
	if _, err := os.Stat(filepath.Join(dir, myappName)); err == nil {
		t.Errorf("did not expect project-named binary at %s when -o was given", myappName)
	}
}

// TestBuildProjectExcludesTestFiles verifies that *_test.pr files are not
// merged into the program when running `promise build .`.
func TestBuildProjectExcludesTestFiles(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"app\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.pr"),
		[]byte("main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// A test file with deliberately broken syntax — if it gets merged into
	// the build, compilation will fail.
	if err := os.WriteFile(filepath.Join(dir, "main_test.pr"),
		[]byte("this is not valid promise code\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "build", ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build failed (test files should not be part of build): %v\n%s", err, out)
	}
}

// TestRunProjectMultiFile verifies that `promise run .` against a project
// directory compiles all .pr files together and runs the resulting binary.
// This exercises the runRun project-mode branch (cache key + label resolve).
func TestRunProjectMultiFile(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping run integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"runme\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.pr"),
		[]byte("main!() { h := Helper(value: 42); print_line(\"{h.value}\"); }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "helper.pr"),
		[]byte("type Helper { int value; }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "run", ".")
	cmd.Dir = dir
	// Capture stdout and stderr separately: the program's output goes to
	// stdout, while diagnostics (the epoch-pin warning when this compiler's
	// epoch differs from the project pin, the project note on a cache miss)
	// go to stderr. Asserting on stdout alone keeps the exact-match robust to
	// any stderr noise.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "42" {
		t.Errorf("stdout = %q, want %q", got, "42")
	}
}

// TestEmitIRProjectMultiFile verifies that `promise emit-ir .` against a
// project directory emits IR covering all .pr files, exercising the
// runEmitIR project-mode branch.
func TestEmitIRProjectMultiFile(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"emitme\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.pr"),
		[]byte("main() { h := Helper(value: 1); }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "helper.pr"),
		[]byte("type Helper { int value; }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "emit-ir", ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("emit-ir failed: %v\n%s", err, out)
	}
	output := string(out)
	// Both files contributed: Helper from helper.pr and main from main.pr.
	if !strings.Contains(output, "Helper") {
		t.Errorf("expected IR to reference Helper from helper.pr; got:\n%s", output)
	}
	if !strings.Contains(output, "main") {
		t.Errorf("expected IR to reference main; got:\n%s", output)
	}
}

// TestBuildFileInsideProject verifies that `promise build main.pr` (a concrete
// file argument, not `.`) run inside a project directory is an ERROR (T1603):
// naming a file that belongs to a project silently compiled something other than
// what the user typed, so the shared resolver now rejects it and points the user
// at the project. No project binary is produced.
func TestBuildFileInsideProject(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"insideproj\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.pr"),
		[]byte("main!() {\n  h := Helper(value: 7);\n  print_line(\"{h.value}\");\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "helper.pr"),
		[]byte("type Helper { int value; }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "build", "main.pr")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected build of a file inside a project to fail; got success:\n%s", out)
	}
	if !strings.Contains(string(out), "belongs to the project at") {
		t.Errorf("expected 'belongs to the project at' error; got:\n%s", out)
	}
	if !strings.Contains(string(out), "hint: build the project directly:") {
		t.Errorf("expected 'hint: build the project directly:' naming the run command; got:\n%s", out)
	}

	// No project binary must be produced.
	binaryName := "insideproj"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	if _, err := os.Stat(filepath.Join(dir, binaryName)); err == nil {
		t.Errorf("project binary %s should not exist after a rejected build", binaryName)
	}
}

// TestRunFileInsideProject verifies that `promise run main.pr` inside a project
// is an ERROR, and — the core drift fix (T1603) — that a cold run and a warm run
// fail *identically*. Previously the "belongs to the project" note printed on a
// cold run (via buildToFile) but not on a cache-hit run (runRun resolved
// silently). Routing both the cache-key path and the compile path through the one
// shared resolver means the same command can no longer produce different output
// depending on cache state.
func TestRunFileInsideProject(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping run integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"runinside\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.pr"),
		[]byte("main!() { h := Helper(value: 42); print_line(\"{h.value}\"); }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "helper.pr"),
		[]byte("type Helper { int value; }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var first string
	for i := 0; i < 2; i++ {
		cmd := exec.Command(bin, "run", "main.pr")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("run #%d: expected running a file inside a project to fail; got success:\n%s", i, out)
		}
		if !strings.Contains(string(out), "belongs to the project at") {
			t.Errorf("run #%d: expected 'belongs to the project at' error; got:\n%s", i, out)
		}
		if !strings.Contains(string(out), "hint: run the project directly:") {
			t.Errorf("run #%d: expected 'hint: run the project directly:'; got:\n%s", i, out)
		}
		if i == 0 {
			first = string(out)
		} else if string(out) != first {
			// Cold and warm runs must be byte-for-byte identical — this is the drift
			// the shared resolver eliminates.
			t.Errorf("cold and warm run output diverged:\ncold:\n%s\nwarm:\n%s", first, out)
		}
	}
}

// TestBuildFileNoProjectStillSingleFile guards the common no-project case: a
// standalone .pr file with no promise.toml anywhere up the tree still
// single-file-compiles and produces a binary named after the file (T0927).
func TestBuildFileNoProjectStillSingleFile(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "solo.pr"),
		[]byte("main() { print_line(\"hi\"); }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "build", "solo.pr")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("single-file build failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "belongs to the project") {
		t.Errorf("did not expect project note for a standalone file; got:\n%s", out)
	}

	binaryName := "solo"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	if _, err := os.Stat(filepath.Join(dir, binaryName)); err != nil {
		t.Fatalf("expected file-named binary at %s, got: %v\noutput: %s", binaryName, err, out)
	}
}

// TestBuildNonexistentFileInsideProject guards the edge case where the named
// file does not exist: the build must fail with a clear file-not-found error
// rather than silently building the enclosing project for a bogus name (T0927).
func TestBuildNonexistentFileInsideProject(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"bogusname\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.pr"),
		[]byte("main() { print_line(\"hi\"); }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "build", "typo.pr")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected build of nonexistent file to fail; got success:\n%s", out)
	}
	if strings.Contains(string(out), "belongs to the project") {
		t.Errorf("nonexistent file must not be claimed to belong to the project; got:\n%s", out)
	}
	if !strings.Contains(string(out), "typo.pr") {
		t.Errorf("expected error to name the missing file typo.pr; got:\n%s", out)
	}
	// The project binary must not have been produced.
	binaryName := "bogusname"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	if _, err := os.Stat(filepath.Join(dir, binaryName)); err == nil {
		t.Errorf("project binary %s should not exist after a failed build", binaryName)
	}
}

// TestBuildFileInProjectSubdir exercises the multi-level walk-up in
// findEnclosingProjectDir: the target file lives in a subdirectory and the
// promise.toml is an ancestor, so the search loop must iterate past the file's
// own directory before finding the project root. Under T1603 a file inside a
// project is an error, so this asserts the walk-up still detects the ancestor
// project and rejects the build. Every other test places promise.toml in the
// same directory as the file, leaving the walk-up loop body uncovered.
func TestBuildFileInProjectSubdir(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"subproj\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.pr"),
		[]byte("main!() { h := Helper(value: 9); print_line(\"{h.value}\"); }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "helper.pr"),
		[]byte("type Helper { int value; }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Run from the project root with a subdirectory-relative file path so the
	// walk-up must climb from src/ to the root to find promise.toml.
	cmd := exec.Command(bin, "build", filepath.Join("src", "main.pr"))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected build of a subdir file inside a project to fail; got success:\n%s", out)
	}
	if !strings.Contains(string(out), "belongs to the project at") {
		t.Errorf("expected 'belongs to the project at' error for subdir file; got:\n%s", out)
	}

	// No project binary must be produced.
	binaryName := "subproj"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	if _, err := os.Stat(filepath.Join(dir, binaryName)); err == nil {
		t.Errorf("project binary %s should not exist after a rejected build", binaryName)
	}
}

// TestRunFileNoProjectStillSingleFile is the run-side analogue of
// TestBuildFileNoProjectStillSingleFile: `promise run file.pr` with no
// promise.toml anywhere up the tree must still single-file-compile and execute,
// without the project note (T0927).
func TestRunFileNoProjectStillSingleFile(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping run integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "solo.pr"),
		[]byte("main() { print_line(\"solo-ok\"); }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "run", "solo.pr")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("single-file run failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "belongs to the project") {
		t.Errorf("did not expect project note for a standalone run; got:\n%s", out)
	}
	if !strings.Contains(string(out), "solo-ok") {
		t.Errorf("expected program output 'solo-ok'; got:\n%s", out)
	}
}

// TestTargetDirWithoutProjectIsError verifies rule 2 of T1603: a directory
// argument that lacks a promise.toml is an error for every command that takes a
// source target. The old T0115 single-file auto-discovery for non-project
// directories is gone — a directory has exactly one meaning now (a project).
func TestTargetDirWithoutProjectIsError(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	bin := clitest.Bin(t)

	for _, cmdName := range []string{"build", "run", "emit-ir"} {
		t.Run(cmdName, func(t *testing.T) {
			dir := clitest.TempDir(t)
			// A lone .pr file with main() but no promise.toml — under the old
			// behavior this directory would have auto-discovered main.pr.
			if err := os.WriteFile(filepath.Join(dir, "main.pr"),
				[]byte("main() { print_line(\"hi\"); }\n"), 0644); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(bin, cmdName, ".")
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("%s .: expected a non-project directory to be rejected; got success:\n%s", cmdName, out)
			}
			if !strings.Contains(string(out), "is not a Promise project") {
				t.Errorf("%s .: expected 'is not a Promise project' error; got:\n%s", cmdName, out)
			}
			// The hint must name the command actually typed.
			if !strings.Contains(string(out), "promise "+cmdName+" file.pr") {
				t.Errorf("%s .: expected hint naming 'promise %s file.pr'; got:\n%s", cmdName, cmdName, out)
			}
		})
	}
}

// TestNoArgInNonProjectDirIsError verifies the no-arg consequence of T1603 rule
// 2: `promise build` / `promise run` with no positional argument defaults to CWD,
// which must be a project. In a non-project directory this is the same
// "not a Promise project" error as an explicit `.` argument.
func TestNoArgInNonProjectDirIsError(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	bin := clitest.Bin(t)

	for _, cmdName := range []string{"build", "run"} {
		t.Run(cmdName, func(t *testing.T) {
			dir := clitest.TempDir(t)
			if err := os.WriteFile(filepath.Join(dir, "main.pr"),
				[]byte("main() { print_line(\"hi\"); }\n"), 0644); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(bin, cmdName)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("%s (no arg): expected a non-project CWD to be rejected; got success:\n%s", cmdName, out)
			}
			if !strings.Contains(string(out), "is not a Promise project") {
				t.Errorf("%s (no arg): expected 'is not a Promise project' error; got:\n%s", cmdName, out)
			}
		})
	}
}

// TestEmitIRFileInsideProjectIsError verifies rule 1 of T1603 for emit-ir, which
// previously ignored project membership and compiled the file standalone: a file
// inside a project is now rejected here too.
func TestEmitIRFileInsideProjectIsError(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"emitproj\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.pr"),
		[]byte("main() { print_line(\"hi\"); }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "emit-ir", "main.pr")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected emit-ir of a file inside a project to fail; got success:\n%s", out)
	}
	if !strings.Contains(string(out), "belongs to the project at") {
		t.Errorf("expected 'belongs to the project at' error; got:\n%s", out)
	}
	if !strings.Contains(string(out), "hint: emit-ir the project directly:") {
		t.Errorf("expected hint naming the emit-ir command; got:\n%s", out)
	}
}

// TestBindWebIdlJsValueDocParses is the end-to-end regression for T0717: an IDL
// whose interface has a union-typed attribute flips HasJsValue, so `promise bind
// webidl` emits the JsValue enum. That enum carries `doc annotations that, prior
// to the fix, were written in the invalid space form on a preceding line — a
// *fatal parse error* on line 1 that masked the whole file. This drives the real
// CLI path (bind → emit-ir) and asserts those ANTLR parse diagnostics are gone.
//
// It deliberately scopes itself to the *parse* layer (exit code ignored): the
// clean-compile (exit 0) acceptance now lives in TestBindWebIdlUnionAttrCompilesClean
// since T0723 landed the JsValue FFI lowering. Kept as the focused parse-diagnostic
// guard so a future regression in the `doc form is attributed here, not to codegen.
func TestBindWebIdlJsValueDocParses(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "element.idl")
	idl := `[Exposed=Window]
interface Element {
	attribute (TrustedHTML or DOMString) innerHTML;
};
`
	if err := os.WriteFile(idlPath, []byte(idl), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	bindCmd := exec.Command(bin, "bind", "webidl", "-name", "idl", "-o", outDir, idlPath)
	if out, err := bindCmd.CombinedOutput(); err != nil {
		t.Fatalf("bind webidl failed: %v\n%s", err, out)
	}

	prPath := filepath.Join(outDir, "idl.pr")
	if _, err := os.Stat(prPath); err != nil {
		t.Fatalf("expected generated %s: %v", prPath, err)
	}

	emitCmd := exec.Command(bin, "emit-ir", "-target", "wasm32-web", prPath)
	out, _ := emitCmd.CombinedOutput() // exit code ignored — see T0723 note above
	output := string(out)
	// The invalid `doc form surfaced as these ANTLR diagnostics; they must be gone.
	for _, bad := range []string{"extraneous input '`'", "no viable alternative at input 'doc"} {
		if strings.Contains(output, bad) {
			t.Errorf("doc annotations failed to parse (%q):\n%s", bad, output)
		}
	}
}

// TestBindWebIdlUnionAttrCompilesClean is the end-to-end acceptance for T0723:
// `promise bind webidl` on an IDL with a union-typed (JsValue) attribute, then
// `promise emit-ir -target wasm32-web`, must compile cleanly (exit 0). Before the
// fix, JsValue was mislowered as a resource handle (`JsValue(_handle:)` /
// `value._handle`), producing two sema errors. This is the compile-clean
// counterpart to TestBindWebIdlJsValueDocParses (which only guards the parse layer).
func TestBindWebIdlUnionAttrCompilesClean(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "element.idl")
	idl := `[Exposed=Window]
interface Element {
	attribute (TrustedHTML or DOMString) innerHTML;
};
`
	if err := os.WriteFile(idlPath, []byte(idl), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	bindCmd := exec.Command(bin, "bind", "webidl", "-name", "idl", "-o", outDir, idlPath)
	if out, err := bindCmd.CombinedOutput(); err != nil {
		t.Fatalf("bind webidl failed: %v\n%s", err, out)
	}

	// `bind webidl` writes a promise.toml alongside idl.pr, so outDir is a
	// project; under T1603 a file-in-project is an error — emit-ir the project.
	emitCmd := exec.Command(bin, "emit-ir", "-target", "wasm32-web", outDir)
	out, err := emitCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("emit-ir -target wasm32-web failed (want exit 0): %v\n%s", err, out)
	}
	// Guard against the two specific T0723 sema diagnostics regressing.
	output := string(out)
	for _, bad := range []string{"cannot construct enum JsValue", "has no variant or method _handle"} {
		if strings.Contains(output, bad) {
			t.Errorf("T0723 sema error resurfaced (%q):\n%s", bad, output)
		}
	}
}

// TestGCRemovedPrintsNotice verifies the `gc` verb no longer runs a sweep but
// stays routable, exiting non-zero with a redirect to the mechanisms that
// replaced it (T1009): `remove` for exclusive-blob reclamation and
// `doctor --repair` for the full orphan sweep.
func TestGCRemovedPrintsNotice(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping gc removal-notice integration test in short mode")
	}
	bin := clitest.Bin(t)

	cmd := exec.Command(bin, "gc")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected `promise gc` to exit non-zero, got success:\n%s", out)
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected exit code 1, got %v:\n%s", err, out)
	}
	output := string(out)
	for _, want := range []string{"has been removed", "doctor --repair", "remove <epoch>"} {
		if !strings.Contains(output, want) {
			t.Errorf("gc removal notice missing %q:\n%s", want, output)
		}
	}
}

// TestFetchRemovedPrintsNotice verifies the `fetch` (and its `warm` alias) verb
// no longer pre-stages the toolchain directly but stays routable, exiting
// non-zero with a redirect to `promise install`, which now folds in toolchain
// pre-staging (T1008).
func TestFetchRemovedPrintsNotice(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping fetch removal-notice integration test in short mode")
	}
	bin := clitest.Bin(t)

	for _, verb := range []string{"fetch", "warm"} {
		cmd := exec.Command(bin, verb)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected `promise %s` to exit non-zero, got success:\n%s", verb, out)
		}
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
			t.Fatalf("expected exit code 1 for `promise %s`, got %v:\n%s", verb, err, out)
		}
		output := string(out)
		for _, want := range []string{"has been removed", "promise install"} {
			if !strings.Contains(output, want) {
				t.Errorf("`promise %s` removal notice missing %q:\n%s", verb, want, output)
			}
		}
	}
}

// TestUnusedImportWarnsButBuildsOK verifies the T1686 diagnostic-severity split
// at the CLI boundary: an unused file-scoped import is a WARNING — it is printed
// but must not fail the build (semaFatal treats sema warnings as advisory). A
// regression that made warnings fatal would break every program with a stray
// `use`.
func TestUnusedImportWarnsButBuildsOK(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	src := filepath.Join(dir, "main.pr")
	// `use path;` is never referenced — an unused import. The leading comment
	// carries this run's temp dir, which Go makes unique per invocation, so the
	// source is one no build cache can already hold — and that is what
	// guarantees the miss the assertion needs, since sema (where the warning is
	// emitted) is skipped on a hit. A private PROMISE_HOME would buy the same
	// miss by staging a whole cold toolchain for it (T2150).
	body := fmt.Sprintf("// %s\nuse path;\n\nmain() {\n  print_line(\"hi\");\n}\n", filepath.ToSlash(dir))
	if err := os.WriteFile(src, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "run", src)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run failed despite only a warning: %v\n%s", err, out)
	}
	combined := string(out)
	if !strings.Contains(combined, "warning: unused import 'path'") {
		t.Errorf("missing unused-import warning\noutput:\n%s", combined)
	}
	if !strings.Contains(combined, "hi") {
		t.Errorf("program did not run to completion\noutput:\n%s", combined)
	}
}

// TestMissingPerFileImportFailsBuild is the counterpart: referencing a module a
// sibling file imports but this file does not is a hard error (imports are
// per-file, T1686) and must fail the build with the per-file hint.
func TestMissingPerFileImportFailsBuild(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"twofile\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// a.pr imports path; main.pr references path.join without importing it.
	if err := os.WriteFile(filepath.Join(dir, "a.pr"),
		[]byte("use path;\nhelper() string `public { return path.join(\"/a\", \"b\"); }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.pr"),
		[]byte("main() { print_line(path.join(\"/c\", \"d\")); }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "build", ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected build failure for a per-file missing import, got success\noutput:\n%s", out)
	}
	combined := string(out)
	if !strings.Contains(combined, "undefined module 'path'") {
		t.Errorf("missing undefined-module error\noutput:\n%s", combined)
	}
	if !strings.Contains(combined, "imports are per-file") {
		t.Errorf("missing per-file import hint\noutput:\n%s", combined)
	}
}

// TestBindWebIdlBorrowFactoryCompilesClean is the end-to-end acceptance for
// T1510's user-visible surface: `promise bind webidl` on an IDL with a resource,
// plus a program that actually *calls* the generated non-owning factory — through
// the generated `document` root (#660) — must compile cleanly for wasm32-web.
//
// The generator-level tests pin the emitted text; only this one proves the text
// is a callable declaration — that a `global factory is legal inside a
// `structural(protocol: false) resource, that its `i32 handle` param accepts a
// plain integer literal (the seeded ambient handle the root passes), and
// that the extra `_owned` field does not break the wrapper's own construction
// sites. Wrapping a seeded handle is the whole point of the item, so a
// regression here is the failure a user hits first.
func TestBindWebIdlBorrowFactoryCompilesClean(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "dom.idl")
	idl := `interface Node {
	constructor(DOMString tag);
	attribute DOMString textContent;
};

interface Document {
	Node createElement(DOMString tag);
};
`
	if err := os.WriteFile(idlPath, []byte(idl), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	bindCmd := exec.Command(bin, "bind", "webidl", "-name", "dom", "-o", outDir, idlPath)
	if out, err := bindCmd.CombinedOutput(); err != nil {
		t.Fatalf("bind webidl failed: %v\n%s", err, out)
	}

	// A second file in the generated project (bind writes promise.toml, so outDir
	// is a project and all its .pr files merge) reaching the page the way
	// docs/wasm-bindings.md documents it: through the generated `document` root,
	// which wraps the seeded handle with the borrow factory without owning it,
	// then using an owning wrapper the bindings minted alongside it (#660).
	//
	// `Node` declares a constructor on purpose (T1974). A constructor emitted as
	// `new(~this, ...)` REPLACES field-wise construction, which both the `borrow`
	// factory and `create_element`'s `Node(_handle: ...)` return depend on — so
	// one constructor-bearing interface made the whole generated module fail to
	// compile. Every bind→compile fixture was constructor-less, which is why
	// nothing caught it.
	userPath := filepath.Join(outDir, "use_borrow.pr")
	user := `main() {
    doc := document;
    el := doc.create_element("div");
    el.text_content = "hi";
    made := Node.create("span");
    made.text_content = "there";
}
`
	if err := os.WriteFile(userPath, []byte(user), 0644); err != nil {
		t.Fatal(err)
	}

	emitCmd := exec.Command(bin, "emit-ir", "-target", "wasm32-web", outDir)
	out, err := emitCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("emit-ir -target wasm32-web failed (want exit 0): %v\n%s", err, out)
	}
	ir := string(out)
	// The borrowed wrapper must reach codegen as a real function, and the drop
	// must be the guarded form — an unconditional release is the T1510 bug.
	for _, want := range []string{"@Document.borrow(", "@Document.drop(", "@Node.borrow(", "@Node.create("} {
		if !strings.Contains(ir, want) {
			t.Errorf("generated IR is missing %q", want)
		}
	}
}

// TestBindArgNamedLikeWrapperLocalCompilesClean binds members whose arguments
// are named like the locals a generated wrapper binds its extern's result to
// (`handle`, `ref`, the canonical-ABI `tag`). Each wrapper used to redeclare
// its own parameter, and bind's self-check (docs/web-apps.md §"Bind time self
// check") rejected the module with "redeclared in this scope" (#699).
//
// The WIT fixture holds free functions only: a canonical-ABI resource wrapper
// does not type-check for an unrelated reason (#541).
func TestBindArgNamedLikeWrapperLocalCompilesClean(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind self-check integration test in short mode")
	}
	bin := clitest.Bin(t)

	cases := []struct {
		name string
		file string
		src  string
		args []string
	}{
		{
			name: "webidl",
			file: "y.idl",
			src: `interface Node {};

interface Y {
	constructor(long handle);
	Node? m(long handle);
	Node p(long handle, long handle_);
	any n(long ref);
	object? o(long ref);
	static Node s(long handle);
	static Node? u(long handle);
	static any v(long ref);
	static object? t(long ref);
};
`,
			args: []string{"bind", "webidl", "-name", "y"},
		},
		{
			name: "wit-canonical-abi",
			file: "api.wit",
			src: `package test:api;

interface greet {
  label: func(tag: u32) -> result<string, string>;
  touch: func(tag: u32) -> result<_, string>;
}

world api {
  import greet;
}
`,
			args: []string{"bind", "wit", "-canonical-abi", "-name", "greetapi"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := clitest.TempDir(t)
			srcPath := filepath.Join(dir, c.file)
			if err := os.WriteFile(srcPath, []byte(c.src), 0644); err != nil {
				t.Fatal(err)
			}
			args := slices.Concat(c.args, []string{"-o", filepath.Join(dir, "out"), srcPath})
			if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
				t.Fatalf("%s failed its self-check (want exit 0): %v\n%s", strings.Join(c.args[:2], " "), err, out)
			}
		})
	}
}

// TestBindWebIdlDocumentRootBesideMemberOfSameName pins the shape the real web
// module has (#660): webref's `Window` declares `readonly attribute Document
// document`, so the generated module holds both a `Window.document` getter and
// the module-level `document` root. Both return Document, so a member access
// resolving to the root instead would type-check and silently read the page's
// document rather than the window's; the IR is the one place the two differ.
func TestBindWebIdlDocumentRootBesideMemberOfSameName(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "dom.idl")
	idl := `interface Document {
	readonly attribute DOMString title;
};

interface Window {
	readonly attribute Document document;
};
`
	if err := os.WriteFile(idlPath, []byte(idl), 0644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	bindCmd := exec.Command(bin, "bind", "webidl", "-name", "dom", "-o", outDir, idlPath)
	if out, err := bindCmd.CombinedOutput(); err != nil {
		t.Fatalf("bind webidl failed: %v\n%s", err, out)
	}

	user := `window_document(Window w) Document {
    return w.document;
}

page_document() Document {
    return document;
}

main() {
    title := page_document().title;
}
`
	if err := os.WriteFile(filepath.Join(outDir, "use_both.pr"), []byte(user), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "emit-ir", "-target", "wasm32-web", outDir).CombinedOutput()
	if err != nil {
		t.Fatalf("emit-ir -target wasm32-web failed (want exit 0): %v\n%s", err, out)
	}
	ir := string(out)

	body := func(fn string) string {
		t.Helper()
		m := regexp.MustCompile(`(?s)\ndefine [^\n]*@[\w.]*\b` + fn + `\([^\n]*\{\n(.*?)\n\}\n`).FindStringSubmatch(ir)
		if m == nil {
			t.Fatalf("no definition of %s in the IR", fn)
		}
		return m[1]
	}
	// The root is the zero-argument getter; the member takes the receiver.
	const rootCall, memberCall = ".document()", "@Window.document("
	if b := body("window_document"); !strings.Contains(b, memberCall) || strings.Contains(b, rootCall) {
		t.Errorf("w.document must call the Window member getter, not the root:\n%s", b)
	}
	if b := body("page_document"); !strings.Contains(b, rootCall) || strings.Contains(b, memberCall) {
		t.Errorf("bare document must call the module root, not a member getter:\n%s", b)
	}
}

// TestBindWebIdlOverloadsCompileClean is the end-to-end acceptance for #263:
// `promise bind webidl` on an IDL with overloaded operations — an equal-arity
// pair differing only in type (the item's setAttribute + TrustedHTML case), an
// arity pair, and a static pair — plus a program calling every overload, must
// compile for wasm32-web. Before the fix each overload emitted the same extern,
// and the module failed with "_element_set_attribute redeclared in this scope".
func TestBindWebIdlOverloadsCompileClean(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "dom.idl")
	idl := `interface TrustedHTML {};

interface Element {
	undefined setAttribute(DOMString name, DOMString value);
	undefined setAttribute(DOMString name, TrustedHTML value);
	undefined scroll();
	undefined scroll(double x, double y);
	static Element make(long n);
	static Element make(DOMString tag);
};
`
	if err := os.WriteFile(idlPath, []byte(idl), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	bindCmd := exec.Command(bin, "bind", "webidl", "-name", "dom", "-o", outDir, idlPath)
	if out, err := bindCmd.CombinedOutput(); err != nil {
		t.Fatalf("bind webidl failed: %v\n%s", err, out)
	}

	userPath := filepath.Join(outDir, "use_overloads.pr")
	user := `main() {
    el := Element.borrow(2);
    el.set_attribute("class", "box");
    trusted := TrustedHTML.borrow(3);
    el.set_attribute_2("class", trusted);
    el.scroll();
    el.scroll_2(1.0, 2.0);
    made := Element.make(1i32);
    named := Element.make_2("div");
}
`
	if err := os.WriteFile(userPath, []byte(user), 0644); err != nil {
		t.Fatal(err)
	}

	emitCmd := exec.Command(bin, "emit-ir", "-target", "wasm32-web", outDir)
	out, err := emitCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("emit-ir -target wasm32-web failed (want exit 0): %v\n%s", err, out)
	}
	ir := string(out)
	// Each overload reaches the IR as its own import, named after the member the
	// JS glue calls plus the overload's ordinal.
	for _, want := range []string{
		"@element_set_attribute(", "@element_set_attribute_2(",
		"@element_scroll(", "@element_scroll_2(",
		"@element_make(", "@element_make_2(",
		`"wasm-import-name"="Element.setAttribute"`, `"wasm-import-name"="Element.setAttribute.2"`,
		`"wasm-import-name"="Element.scroll.2"`, `"wasm-import-name"="Element.make.2"`,
	} {
		if !strings.Contains(ir, want) {
			t.Errorf("generated IR is missing %q", want)
		}
	}
}

// TestBindWebIdlOverloadedConstructorsCompileClean is the end-to-end acceptance
// for #617: `promise bind webidl` on interfaces declaring two constructors — one
// plain, one beside an IDL member named `create` (the T1974 fallback) — plus a
// program calling every factory, must compile for wasm32-web. Before the fix
// both constructors emitted the same `create` factory and `_x_constructor`
// extern, and the module failed with "redeclared in this scope".
func TestBindWebIdlOverloadedConstructorsCompileClean(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "dom.idl")
	idl := `interface X {
	constructor(long n);
	constructor(DOMString tag);
};

interface Slot {
	constructor();
	constructor(long n);
	unsigned long create();
};
`
	if err := os.WriteFile(idlPath, []byte(idl), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	bindCmd := exec.Command(bin, "bind", "webidl", "-name", "dom", "-o", outDir, idlPath)
	if out, err := bindCmd.CombinedOutput(); err != nil {
		t.Fatalf("bind webidl failed: %v\n%s", err, out)
	}

	userPath := filepath.Join(outDir, "use_constructors.pr")
	user := `main() {
    a := X.create(1i32);
    b := X.create_2("div");
    s := Slot.create_handle();
    t := Slot.create_handle_2(2i32);
    n := s.create();
}
`
	if err := os.WriteFile(userPath, []byte(user), 0644); err != nil {
		t.Fatal(err)
	}

	emitCmd := exec.Command(bin, "emit-ir", "-target", "wasm32-web", outDir)
	out, err := emitCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("emit-ir -target wasm32-web failed (want exit 0): %v\n%s", err, out)
	}
	ir := string(out)
	// Each constructor reaches the IR as its own factory and its own import,
	// named after the constructor plus the overload's ordinal.
	for _, want := range []string{
		"@X.create(", "@X.create_2(",
		"@Slot.create_handle(", "@Slot.create_handle_2(",
		"@x_constructor(", "@x_constructor_2(", "@slot_constructor_2(",
		`"wasm-import-name"="X.constructor"`, `"wasm-import-name"="X.constructor.2"`,
		`"wasm-import-name"="Slot.constructor.2"`,
	} {
		if !strings.Contains(ir, want) {
			t.Errorf("generated IR is missing %q", want)
		}
	}
}

// TestBindWebIdlKeywordArgNamesCompileClean is the end-to-end acceptance for
// #650: WebIDL lets an argument be named by a keyword, as webref's
// `addEventListener(DOMString type, EventListener? callback)` does. Before the
// fix the parser rejected it and `bind` exited 1 for the whole input. `enum` is
// also a Promise keyword, so its parameter must come out sanitized as `_enum`.
func TestBindWebIdlKeywordArgNamesCompileClean(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "kw.idl")
	idl := `interface Target {
	undefined f(DOMString callback, long interface, boolean enum);
};
`
	if err := os.WriteFile(idlPath, []byte(idl), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	bindCmd := exec.Command(bin, "bind", "webidl", "-name", "kw", "-o", outDir, idlPath)
	if out, err := bindCmd.CombinedOutput(); err != nil {
		t.Fatalf("bind webidl failed: %v\n%s", err, out)
	}

	generated, err := os.ReadFile(filepath.Join(outDir, "kw.pr"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "string callback, i32 interface, bool _enum"; !strings.Contains(string(generated), want) {
		t.Errorf("generated wrapper is missing params %q:\n%s", want, generated)
	}

	userPath := filepath.Join(outDir, "use_kw.pr")
	user := `main() {
    t := Target.borrow(2);
    t.f("x", 1i32, true);
}
`
	if err := os.WriteFile(userPath, []byte(user), 0644); err != nil {
		t.Fatal(err)
	}

	emitCmd := exec.Command(bin, "emit-ir", "-target", "wasm32-web", outDir)
	out, err := emitCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("emit-ir -target wasm32-web failed (want exit 0): %v\n%s", err, out)
	}
	ir := string(out)
	for _, want := range []string{"@target_f(", `"wasm-import-name"="Target.f"`} {
		if !strings.Contains(ir, want) {
			t.Errorf("generated IR is missing %q", want)
		}
	}
}

// TestBindWebIdlHyphenatedMembersCompileClean is the end-to-end acceptance for
// #719: webref's `CSSPageDescriptors` declares dashed attributes
// (`margin-top`, `page-orientation`). Before the fix the lexer ended an
// identifier at its first `-`, and `bind` exited 1 for the whole input. Each
// name generates its Promise name with `_` for `-` and a leading `-` dropped,
// and its import keeps the IDL spelling, which the glue reaches by bracket
// access.
func TestBindWebIdlHyphenatedMembersCompileClean(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "page.idl")
	idl := `[Exposed=Window]
interface CSSPageDescriptors {
  attribute [LegacyNullToEmptyString] DOMString margin-top;
  attribute [LegacyNullToEmptyString] DOMString page-orientation;
  attribute DOMString -webkit-foo;
  undefined set-property(DOMString property-name);
};
`
	if err := os.WriteFile(idlPath, []byte(idl), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	bindCmd := exec.Command(bin, "bind", "webidl", "-name", "page", "-o", outDir, idlPath)
	if out, err := bindCmd.CombinedOutput(); err != nil {
		t.Fatalf("bind webidl failed: %v\n%s", err, out)
	}

	glue, err := os.ReadFile(filepath.Join(outDir, "page.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`_refLoad($handle)["margin-top"]`, `_refLoad($handle)["-webkit-foo"] = `, `_refLoad($handle)["set-property"](`} {
		if !strings.Contains(string(glue), want) {
			t.Errorf("generated glue is missing %q:\n%s", want, glue)
		}
	}

	userPath := filepath.Join(outDir, "use_page.pr")
	user := `main() {
    d := CSSPageDescriptors.borrow(2);
    d.margin_top = "1px";
    orientation := d.page_orientation;
    d.webkit_foo = orientation;
    d.set_property("color");
}
`
	if err := os.WriteFile(userPath, []byte(user), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(bin, "emit-ir", "-target", "wasm32-web", outDir).CombinedOutput()
	if err != nil {
		t.Fatalf("emit-ir -target wasm32-web failed (want exit 0): %v\n%s", err, out)
	}
	ir := string(out)
	for _, want := range []string{
		`"wasm-import-name"="CSSPageDescriptors.margin-top.set"`,
		`"wasm-import-name"="CSSPageDescriptors.page-orientation.get"`,
		`"wasm-import-name"="CSSPageDescriptors.-webkit-foo.set"`,
		`"wasm-import-name"="CSSPageDescriptors.set-property"`,
	} {
		if !strings.Contains(ir, want) {
			t.Errorf("generated IR is missing %q", want)
		}
	}
}

// TestBindWebIdlLegacyCallerCompilesClean is the end-to-end acceptance for #651:
// webref's `HTMLAllCollection` declares `legacycaller … item(…)`. Before the fix
// the parser rejected `legacycaller` and `bind` exited 1 for the whole input. A
// named legacycaller binds as the regular operation of its name; an anonymous
// one calls the object itself and binds no member.
func TestBindWebIdlLegacyCallerCompilesClean(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "all.idl")
	idl := `interface Element {};

interface HTMLAllCollection {
	readonly attribute unsigned long length;
	legacycaller Element? item(unsigned long index);
};

interface Callable {
	legacycaller Element? (unsigned long index);
};
`
	if err := os.WriteFile(idlPath, []byte(idl), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	bindCmd := exec.Command(bin, "bind", "webidl", "-name", "all", "-o", outDir, idlPath)
	if out, err := bindCmd.CombinedOutput(); err != nil {
		t.Fatalf("bind webidl failed: %v\n%s", err, out)
	}

	generated, err := os.ReadFile(filepath.Join(outDir, "all.pr"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(generated), "legacycaller") {
		t.Errorf("the anonymous legacycaller bound a member:\n%s", generated)
	}

	userPath := filepath.Join(outDir, "use_all.pr")
	user := `main() {
    all := HTMLAllCollection.borrow(2);
    el := all.item(0u32);
}
`
	if err := os.WriteFile(userPath, []byte(user), 0644); err != nil {
		t.Fatal(err)
	}

	emitCmd := exec.Command(bin, "emit-ir", "-target", "wasm32-web", outDir)
	out, err := emitCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("emit-ir -target wasm32-web failed (want exit 0): %v\n%s", err, out)
	}
	ir := string(out)
	for _, want := range []string{"@html_all_collection_item(", `"wasm-import-name"="HTMLAllCollection.item"`} {
		if !strings.Contains(ir, want) {
			t.Errorf("generated IR is missing %q", want)
		}
	}
}

// TestBindWebIdlDocCommentsCompileClean is the end-to-end acceptance for #659:
// IDL comments become `doc annotations and every uncommented declaration gets a
// generated one, so every `public line of the generated module carries a `doc —
// and comment text holding `"`, `\`, `{…}` and line breaks, in an LF or a CRLF
// source, still yields a module that compiles for wasm32-web. An unescaped `{`
// would open an interpolation inside the doc string.
func TestBindWebIdlDocCommentsCompileClean(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	idl := `// GENERATED CONTENT - DO NOT EDIT
// Source: a test fixture

/**
 * An element. Say "hi" \ to {name}.
 * Second line.
 */
[Exposed=Window]
interface Element {
	// The id, as in <div id="x">.
	attribute DOMString id;
	readonly attribute DOMString tagName;
	/** Sets {name} to "value". */
	undefined setAttribute(DOMString name, DOMString value);
	undefined setAttribute(DOMString name, long value);
	const unsigned short ELEMENT_NODE = 1;
};
// Options for {Element}.
dictionary Opts { long x; };
enum Mode { "open", "closed" };
`
	for _, tc := range []struct{ name, eol string }{{"LF", "\n"}, {"CRLF", "\r\n"}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := clitest.TempDir(t)
			idlPath := filepath.Join(dir, "dom.idl")
			if err := os.WriteFile(idlPath, []byte(strings.ReplaceAll(idl, "\n", tc.eol)), 0644); err != nil {
				t.Fatal(err)
			}

			outDir := filepath.Join(dir, "out")
			bindCmd := exec.Command(bin, "bind", "webidl", "-name", "dom", "-o", outDir, idlPath)
			if out, err := bindCmd.CombinedOutput(); err != nil {
				t.Fatalf("bind webidl failed: %v\n%s", err, out)
			}

			pr, err := os.ReadFile(filepath.Join(outDir, "dom.pr"))
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(pr), "\n") {
				if strings.Contains(line, "`public") && !strings.Contains(line, "`doc(\"") {
					t.Errorf("`public declaration without a `doc: %s", strings.TrimSpace(line))
				}
			}
			for _, want := range []string{
				"`doc(\"An element. Say \\\"hi\\\" \\\\ to \\{name}. Second line.\") {",
				"`doc(\"Sets \\{name} to \\\"value\\\".\") {",
				"`doc(\"`Element.setAttribute` (WebIDL operation)\") {",
			} {
				if !strings.Contains(string(pr), want) {
					t.Errorf("generated dom.pr is missing %q", want)
				}
			}

			emitCmd := exec.Command(bin, "emit-ir", "-target", "wasm32-web", outDir)
			if out, err := emitCmd.CombinedOutput(); err != nil {
				t.Fatalf("emit-ir -target wasm32-web failed (want exit 0): %v\n%s", err, out)
			}
		})
	}
}

// TestBindWitBorrowFactoryCompilesClean is the WIT/wasi counterpart of
// TestBindWebIdlBorrowFactoryCompilesClean. `emitResource` is shared by both
// front ends, but the surrounding module is not: the wasi path emits
// `target(wasi) declarations and `wasm_import`s into a WIT interface name, and
// nothing else compiles a `bind wit` result. Without this, the ownership fields
// and the `borrow` factory were only ever compile-checked on the web target.
func TestBindWitBorrowFactoryCompilesClean(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	witPath := filepath.Join(dir, "api.wit")
	wit := `package test:api;

interface types {
  resource descriptor {
    constructor(path: string);
    read: func(len: u32) -> u32;
    reopen: func() -> descriptor;
  }
}

world api {
  import types;
}
`
	if err := os.WriteFile(witPath, []byte(wit), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	bindCmd := exec.Command(bin, "bind", "wit", "-name", "fsapi", "-o", outDir, witPath)
	if out, err := bindCmd.CombinedOutput(); err != nil {
		t.Fatalf("bind wit failed: %v\n%s", err, out)
	}

	// Borrow an ambient handle, call a method through it, and let both a
	// method-minted and a constructor-minted (owning) wrapper drop alongside it —
	// every ownership state in one scope. The `constructor` is what T1974 broke on
	// the WIT side, exactly as it did on the WebIDL side.
	userPath := filepath.Join(outDir, "use_borrow.pr")
	user := `main() {
    d := Descriptor.borrow(3);
    n := d.read(4u32);
    fresh := d.reopen();
    minted := Descriptor.create("/tmp/x");
}
`
	if err := os.WriteFile(userPath, []byte(user), 0644); err != nil {
		t.Fatal(err)
	}

	emitCmd := exec.Command(bin, "emit-ir", "-target", "wasm32-wasi", outDir)
	out, err := emitCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("emit-ir -target wasm32-wasi failed (want exit 0): %v\n%s", err, out)
	}
	ir := string(out)
	for _, want := range []string{"@Descriptor.borrow(", "@Descriptor.drop(", "@Descriptor.create("} {
		if !strings.Contains(ir, want) {
			t.Errorf("generated IR is missing %q", want)
		}
	}
}

// TestBindWebIdlBorrowedWrapperSkipsRelease checks the *shape* of the generated
// drop in IR rather than in the .pr text: the release import must sit behind a
// conditional branch on `_owned`, not on the drop's entry path. A regression to
// the pre-T1510 unconditional drop still emits `@Document.drop` and still passes
// every text assertion in the bindgen package once the .pr is regenerated, so
// this is the check that the guard survived lowering.
func TestBindWebIdlBorrowedWrapperSkipsRelease(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "dom.idl")
	if err := os.WriteFile(idlPath, []byte("interface Document {\n\tDOMString title();\n};\n"), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	if out, err := exec.Command(bin, "bind", "webidl", "-name", "dom", "-o", outDir, idlPath).CombinedOutput(); err != nil {
		t.Fatalf("bind webidl failed: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(outDir, "use.pr"), []byte("main() {\n    d := Document.borrow(2);\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(bin, "emit-ir", "-target", "wasm32-web", outDir).CombinedOutput()
	if err != nil {
		t.Fatalf("emit-ir -target wasm32-web failed: %v\n%s", err, out)
	}

	body, ok := irFunctionBody(string(out), "@Document.drop(")
	if !ok {
		t.Fatal("no @Document.drop definition in the generated IR")
	}
	release := strings.Index(body, "@document_drop(")
	if release < 0 {
		t.Fatalf("Document.drop never calls the release import:\n%s", body)
	}
	guard := strings.Index(body, "br i1 ")
	if guard < 0 || guard > release {
		t.Errorf("the release import is not guarded by a branch on _owned (branch at %d, release at %d):\n%s", guard, release, body)
	}
}

// TestBindWebIdlInheritanceCompilesClean is the end-to-end acceptance for
// T1507: `promise bind webidl` on an inheritance chain must produce wrappers a
// program can use the way the IDL reads — a derived wrapper reaches its
// parent's and grandparent's members, and is accepted where the parent type is
// expected without rewrapping its handle. The generator-level tests pin the
// emitted `is` clause; only compiling a caller proves the hierarchy is usable.
func TestBindWebIdlInheritanceCompilesClean(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "dom.idl")
	// RectReadOnly/Rect is a compatible redeclaration (DOMRect's shape): the child
	// re-declares the parent's readonly attribute as read-write, which overrides
	// the inherited getter and adds a setter.
	idl := `interface EventTarget {
	undefined dispatch(DOMString kind);
};

interface Node : EventTarget {
	attribute DOMString textContent;
	Node appendChild(Node node);
};

interface Element : Node {
	readonly attribute DOMString tagName;
	undefined setAttribute(DOMString name, DOMString value);
};

interface Document : Node {
	Element createElement(DOMString localName);
};

interface RectReadOnly {
	readonly attribute double x;
};

interface Rect : RectReadOnly {
	attribute double x;
};
`
	if err := os.WriteFile(idlPath, []byte(idl), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	bindCmd := exec.Command(bin, "bind", "webidl", "-name", "dom", "-o", outDir, idlPath)
	if out, err := bindCmd.CombinedOutput(); err != nil {
		t.Fatalf("bind webidl failed: %v\n%s", err, out)
	}

	userPath := filepath.Join(outDir, "use_inheritance.pr")
	user := `main() {
    doc := Document.borrow(2);
    el := doc.create_element("div");
    el.text_content = "hi";
    el.dispatch("click");
    el.set_attribute("id", "x");
    page := doc.create_element("section");
    added := page.append_child(el);
    root := doc.append_child(page);
    held := Element.borrow(4);
    print_line(held.tag_name);
    r := Rect.borrow(5);
    r.x = 1.0;
    print_line(r.x.to_string());
}
`
	if err := os.WriteFile(userPath, []byte(user), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(bin, "emit-ir", "-target", "wasm32-web", outDir).CombinedOutput()
	if err != nil {
		t.Fatalf("emit-ir -target wasm32-web failed (want exit 0): %v\n%s", err, out)
	}
	ir := string(out)
	for _, want := range []string{"@Node.append_child(", "@Element.borrow(", "@Element.drop(", "@Rect.x$set("} {
		if !strings.Contains(ir, want) {
			t.Errorf("generated IR is missing %q", want)
		}
	}
	// The inherited member's host import is declared once, by the interface that
	// declares the member. (The compiler may still specialize the inherited
	// wrapper per subtype, as `@Element.append_child`; that body calls this same
	// import.)
	if n := strings.Count(ir, "declare i32 @node_append_child("); n != 1 {
		t.Errorf("@node_append_child declared %d times, want once", n)
	}
	for _, copied := range []string{"@element_append_child(", "@document_append_child(", "@element_dispatch("} {
		if strings.Contains(ir, copied) {
			t.Errorf("IR contains %q — an inherited member's import was copied onto a derived wrapper", copied)
		}
	}
}

// TestBindWebIdlShadowedMembersCompileClean is the end-to-end acceptance for
// #612: a derived interface that redeclares an inherited member with a different
// signature — webref's HTMLFormControlsCollection.namedItem (a different
// result), SVGElement.className (an attribute of a different type), a
// different parameter type, and a constant sharing an inherited operation's
// Promise name — must bind and compile, with the redeclaration
// numbered after the inherited member it would otherwise have overridden
// (docs/wasm-bindings.md, "Shadowed Members"), while an exact redeclaration
// (Rect.x) stays an override. Before the fix bind's self-check failed with
// "cannot override inherited method 'named_item' from HTMLCollection".
func TestBindWebIdlShadowedMembersCompileClean(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "dom.idl")
	idl := `interface Element {
	attribute DOMString className;
};

interface RadioNodeList {};

interface SVGAnimatedString {};

interface SVGElement : Element {
	readonly attribute SVGAnimatedString className;
};

interface HTMLCollection {
	getter Element? namedItem(DOMString name);
};

interface HTMLFormControlsCollection : HTMLCollection {
	getter (RadioNodeList or Element)? namedItem(DOMString name);
};

interface Parent {
	undefined m(DOMString text);
	undefined reset();
};

interface Child : Parent {
	undefined m(long n);
	const long RESET = 1;
};

interface RectReadOnly {
	readonly attribute double x;
};

interface Rect : RectReadOnly {
	attribute double x;
};
`
	if err := os.WriteFile(idlPath, []byte(idl), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	bindCmd := exec.Command(bin, "bind", "webidl", "-name", "dom", "-o", outDir, idlPath)
	if out, err := bindCmd.CombinedOutput(); err != nil {
		t.Fatalf("bind webidl failed: %v\n%s", err, out)
	}

	// Each derived wrapper reaches both the inherited member, typed as its
	// parent declares it, and its own renamed one; it still upcasts.
	userPath := filepath.Join(outDir, "use_shadows.pr")
	user := `main() {
    forms := HTMLFormControlsCollection.borrow(2);
    first := forms.named_item("a");
    either := forms.named_item_2("a");
    HTMLCollection collection = forms;
    again := collection.named_item("b");
    svg := SVGElement.borrow(3);
    animated := svg.class_name_2;
    print_line(svg.class_name);
    svg.class_name = "box";
    child := Child.borrow(4);
    child.m("text");
    child.m_2(1i32);
    child.reset();
    print_line(Child.reset_2().to_string());
    r := Rect.borrow(5);
    r.x = 1.0;
    print_line(r.x.to_string());
}
`
	if err := os.WriteFile(userPath, []byte(user), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(bin, "emit-ir", "-target", "wasm32-web", outDir).CombinedOutput()
	if err != nil {
		t.Fatalf("emit-ir -target wasm32-web failed (want exit 0): %v\n%s", err, out)
	}
	ir := string(out)
	for _, want := range []string{
		"@html_form_controls_collection_named_item_2(", `"wasm-import-name"="HTMLFormControlsCollection.namedItem.2"`,
		"@html_collection_named_item(", `"wasm-import-name"="HTMLCollection.namedItem"`,
		"@svg_element_class_name_2(", `"wasm-import-name"="SVGElement.className.get"`,
		"@child_m_2(", `"wasm-import-name"="Child.m.2"`,
		"@child_reset_2(", `"wasm-import-name"="Child.RESET.2"`,
		"@Rect.x$set(",
	} {
		if !strings.Contains(ir, want) {
			t.Errorf("generated IR is missing %q", want)
		}
	}
}

// TestBindWebIdlInheritedFactoryNameCompilesClean is the end-to-end acceptance
// for #684: a derived interface whose ancestor declares an instance `create` or
// `borrow` — an operation or an attribute, on its parent or further up the
// chain — must bind and compile, with the derived wrapper's `global factories
// moved to `create_handle` / `borrow_handle` (docs/wasm-bindings.md, "WIT
// Resources to Promise Ownership"), while the inherited members stay reachable
// under their own names. Before the fix bind's self-check failed with "type C
// cannot override inherited method 'create' from P".
func TestBindWebIdlInheritedFactoryNameCompilesClean(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "dom.idl")
	idl := `interface P {
	undefined create(DOMString s);
	undefined borrow(DOMString s);
};

interface C : P {
	constructor(long x);
};

interface G {
	undefined borrow(DOMString s);
};

interface M : G {};

interface L : M {};

interface Q {
	readonly attribute DOMString borrow;
};

interface R : Q {};
`
	if err := os.WriteFile(idlPath, []byte(idl), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	bindCmd := exec.Command(bin, "bind", "webidl", "-name", "dom", "-o", outDir, idlPath)
	if out, err := bindCmd.CombinedOutput(); err != nil {
		t.Fatalf("bind webidl failed: %v\n%s", err, out)
	}

	userPath := filepath.Join(outDir, "use_factories.pr")
	user := `main() {
    made := C.create_handle(1i32);
    made.create("s");
    held := C.borrow_handle(2);
    held.borrow("t");
    leaf := L.borrow_handle(3);
    leaf.borrow("u");
    G root = leaf;
    root.borrow("v");
    attr := R.borrow_handle(4);
    text := attr.borrow;
}
`
	if err := os.WriteFile(userPath, []byte(user), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(bin, "emit-ir", "-target", "wasm32-web", outDir).CombinedOutput()
	if err != nil {
		t.Fatalf("emit-ir -target wasm32-web failed (want exit 0): %v\n%s", err, out)
	}
	ir := string(out)
	for _, want := range []string{
		"@C.create_handle(", "@C.borrow_handle(", "@L.borrow_handle(", "@R.borrow_handle(",
		"@p_create(", "@p_borrow(", "@g_borrow(", "@q_borrow(",
	} {
		if !strings.Contains(ir, want) {
			t.Errorf("generated IR is missing %q", want)
		}
	}
}

// TestBindWebIdlUnboundParentFailsLoudly is the error path: a derived interface
// whose parent is not among the bound files must not compile into a wrapper
// that silently lacks the inherited members — the failure T1507 reports. bind's
// self-check (docs/web-apps.md#bind-time-self-check) reports it at bind time;
// with -no-check the project is written and the next compile reports it.
func TestBindWebIdlUnboundParentFailsLoudly(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "element.idl")
	if err := os.WriteFile(idlPath, []byte("interface Element : Node {\n\treadonly attribute DOMString tagName;\n};\n"), 0644); err != nil {
		t.Fatal(err)
	}

	checkedDir := filepath.Join(dir, "checked")
	out, err := exec.Command(bin, "bind", "webidl", "-name", "dom", "-o", checkedDir, idlPath).CombinedOutput()
	if err == nil {
		t.Fatalf("bind webidl succeeded on a wrapper whose parent Node is unbound (want its self-check to fail)\n%s", out)
	}
	if !strings.Contains(string(out), "undefined type: Node") {
		t.Errorf("bind: want an `undefined type: Node` error naming the missing parent\noutput:\n%s", out)
	}

	outDir := filepath.Join(dir, "out")
	if out, err := exec.Command(bin, "bind", "webidl", "-no-check", "-name", "dom", "-o", outDir, idlPath).CombinedOutput(); err != nil {
		t.Fatalf("bind webidl -no-check failed: %v\n%s", err, out)
	}

	out, err = exec.Command(bin, "emit-ir", "-target", "wasm32-web", outDir).CombinedOutput()
	if err == nil {
		t.Fatalf("emit-ir succeeded on a wrapper whose parent Node is unbound (want a compile error)\n%s", out)
	}
	if !strings.Contains(string(out), "undefined type: Node") {
		t.Errorf("emit-ir: want an `undefined type: Node` error naming the missing parent\noutput:\n%s", out)
	}
}

// dictionaryParentsIDL and dictionaryChildIDL split one inheritance chain
// across two files, as the standards IDL does (EventInit in DOM, MouseEventInit
// in UI Events).
const (
	dictionaryParentsIDL = `dictionary EventInit {
	boolean bubbles = false;
	boolean cancelable = false;
};

dictionary UIEventInit : EventInit {
	long detail = 0;
};
`
	dictionaryChildIDL = `dictionary MouseEventInit : UIEventInit {
	required long clientX;
};
`
)

// TestBindWebIdlDictionaryInheritanceCompilesClean is the end-to-end acceptance
// for #613: a derived dictionary carries its ancestors' members as fields of
// its own, so a caller sets an inherited member exactly like an own one. The
// child's file is bound first, so the chain resolves across files whatever
// their order; only compiling a caller proves the inherited fields are usable.
func TestBindWebIdlDictionaryInheritanceCompilesClean(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	childPath := filepath.Join(dir, "mouse.idl")
	parentsPath := filepath.Join(dir, "events.idl")
	if err := os.WriteFile(childPath, []byte(dictionaryChildIDL), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parentsPath, []byte(dictionaryParentsIDL), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	if out, err := exec.Command(bin, "bind", "webidl", "-name", "events", "-o", outDir, childPath, parentsPath).CombinedOutput(); err != nil {
		t.Fatalf("bind webidl failed: %v\n%s", err, out)
	}

	userPath := filepath.Join(outDir, "use_dictionaries.pr")
	user := `main() {
    init := MouseEventInit(bubbles: true, detail: 2i32, client_x: 3i32);
    if init.bubbles! {
        print_line("bubbles");
    }
    print_line(init.client_x.to_string());
}
`
	if err := os.WriteFile(userPath, []byte(user), 0644); err != nil {
		t.Fatal(err)
	}

	if out, err := exec.Command(bin, "emit-ir", "-target", "wasm32-web", outDir).CombinedOutput(); err != nil {
		t.Fatalf("emit-ir -target wasm32-web failed (want exit 0): %v\n%s", err, out)
	}
}

// TestBindWebIdlTypedefsCompileClean is the end-to-end acceptance for #721: a
// typedef names no type, so each use binds as its target written in its place
// (docs/wasm-bindings.md, "WebIDL to Promise", typedefs) — a primitive, a
// union, a typedef of a typedef and of a nullable type, used nullable, as a
// dictionary field, a parameter and a return type. Before the fix every use
// kept the typedef's name and bind's self-check failed with `undefined type:
// Name`. The caller uses each member at its target's Promise type, which only
// an inlined typedef gives it.
func TestBindWebIdlTypedefsCompileClean(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "typedefs.idl")
	idl := `typedef DOMString Name;
typedef (DOMString or long) NameOrIndex;
typedef Name Label;
typedef DOMString? MaybeName;

interface Event {};
typedef Event EventAlias;

dictionary Options {
	required NameOrIndex which;
	Label label;
};

[Exposed=Window]
interface Picker {
	attribute Name name;
	attribute Name? nickname;
	attribute MaybeName alias;
	readonly attribute Label label;
	undefined pick(NameOrIndex which, optional Name hint);
	Name rename(Name to);
	EventAlias last();
	EventAlias? find(Name key);
	sequence<Name> names();
};
`
	if err := os.WriteFile(idlPath, []byte(idl), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	if out, err := exec.Command(bin, "bind", "webidl", "-name", "typedefs", "-o", outDir, idlPath).CombinedOutput(); err != nil {
		t.Fatalf("bind webidl failed: %v\n%s", err, out)
	}

	userPath := filepath.Join(outDir, "use_typedefs.pr")
	user := `main() {
    picker := Picker.borrow(1);
    string name = picker.name;
    picker.name = "renamed";
    string? nickname = picker.nickname;
    picker.nickname = none;
    string? alias = picker.alias;
    string label = picker.label;
    picker.pick(JsValue.Str(value: "first"), "hint");
    string renamed = picker.rename("to");
    Event last = picker.last();
    Event? found = picker.find("key");
    string[] names = picker.names();
    options := Options(which: JsValue.Number(value: 1.0), label: "short");
}
`
	if err := os.WriteFile(userPath, []byte(user), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(bin, "emit-ir", "-target", "wasm32-web", outDir).CombinedOutput()
	if err != nil {
		t.Fatalf("emit-ir -target wasm32-web failed (want exit 0): %v\n%s", err, out)
	}
	ir := string(out)
	for _, want := range []string{
		"@Picker.name(", "@Picker.nickname(", "@Picker.alias(", "@Picker.label(", "@Picker.pick(",
		"@Picker.rename(", "@Picker.last(", "@Picker.find(", "@Picker.names(",
	} {
		if !strings.Contains(ir, want) {
			t.Errorf("generated IR is missing %q", want)
		}
	}
}

// TestBindWebIdlDictionaryUnboundParentFailsLoudly is the error path: a
// flattened dictionary never names its parent, so an unbound one would compile
// into a type silently missing the inherited members. bind refuses it before
// writing anything, with or without its self-check, exits 1, and reports every
// such dictionary as a diagnostic line of its own.
func TestBindWebIdlDictionaryUnboundParentFailsLoudly(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	mousePath := filepath.Join(dir, "mouse.idl")
	focusPath := filepath.Join(dir, "focus.idl")
	if err := os.WriteFile(mousePath, []byte(dictionaryChildIDL), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(focusPath, []byte("dictionary FocusEventInit : UIEventInit {\n\tlong order = 0;\n};\n"), 0644); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"dictionary MouseEventInit inherits from UIEventInit, which is not a dictionary in the bound IDL files",
		"dictionary FocusEventInit inherits from UIEventInit, which is not a dictionary in the bound IDL files",
	}

	for _, check := range [][]string{nil, {"-no-check"}} {
		outDir := filepath.Join(dir, "out"+strings.Join(check, ""))
		args := append(append([]string{"bind", "webidl"}, check...), "-name", "events", "-o", outDir, mousePath, focusPath)
		out, err := exec.Command(bin, args...).CombinedOutput()
		if err == nil {
			t.Fatalf("bind %v succeeded on dictionaries whose parent UIEventInit is unbound (want an error)\n%s", check, out)
		}
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
			t.Errorf("bind %v: want exit code 1, got %v", check, err)
		}
		if got := strings.Split(strings.TrimRight(string(out), "\n"), "\n"); !slices.Equal(got, want) {
			t.Errorf("bind %v: output lines\n%q\nwant\n%q", check, got, want)
		}
		if _, err := os.Stat(filepath.Join(outDir, "events.pr")); !os.IsNotExist(err) {
			t.Errorf("bind %v wrote events.pr despite the error (stat: %v)", check, err)
		}
	}
}

// TestBindWebIdlEnumValuesCompileClean is the end-to-end acceptance for #720:
// an enum value is any string, and the variant it generates is named by a rule
// every string satisfies (docs/wasm-bindings.md, "WebIDL to Promise", enums).
// The IDL is webref html.idl's two enums whose values are not identifiers,
// verbatim. Before the fix bind's self-check failed with `extraneous input '2'
// expecting IDENT` at the `2d` variant. The caller matches every variant with
// no wildcard, so a variant misnamed, missing or extra fails to compile.
func TestBindWebIdlEnumValuesCompileClean(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "html.idl")
	idl := `enum OffscreenRenderingContextId { "2d", "bitmaprenderer", "webgl", "webgl2", "webgpu" };

enum DOMParserSupportedType {
  "text/html",
  "text/xml",
  "application/xml",
  "application/xhtml+xml",
  "image/svg+xml"
};
`
	if err := os.WriteFile(idlPath, []byte(idl), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	if out, err := exec.Command(bin, "bind", "webidl", "-name", "html", "-o", outDir, idlPath).CombinedOutput(); err != nil {
		t.Fatalf("bind webidl failed: %v\n%s", err, out)
	}

	userPath := filepath.Join(outDir, "use_enums.pr")
	user := `context_id(OffscreenRenderingContextId id) string {
    return match id {
        OffscreenRenderingContextId.V2d => "2d",
        OffscreenRenderingContextId.Bitmaprenderer => "bitmaprenderer",
        OffscreenRenderingContextId.Webgl => "webgl",
        OffscreenRenderingContextId.Webgl2 => "webgl2",
        OffscreenRenderingContextId.Webgpu => "webgpu",
    };
}

parser_type(DOMParserSupportedType type_) string {
    return match type_ {
        DOMParserSupportedType.TextHtml => "text/html",
        DOMParserSupportedType.TextXml => "text/xml",
        DOMParserSupportedType.ApplicationXml => "application/xml",
        DOMParserSupportedType.ApplicationXhtmlXml => "application/xhtml+xml",
        DOMParserSupportedType.ImageSvgXml => "image/svg+xml",
    };
}

main() {
    print_line(context_id(OffscreenRenderingContextId.V2d));
    print_line(parser_type(DOMParserSupportedType.ImageSvgXml));
}
`
	if err := os.WriteFile(userPath, []byte(user), 0644); err != nil {
		t.Fatal(err)
	}

	if out, err := exec.Command(bin, "emit-ir", "-target", "wasm32-web", outDir).CombinedOutput(); err != nil {
		t.Fatalf("emit-ir -target wasm32-web failed (want exit 0): %v\n%s", err, out)
	}
}

// TestBindWebIdlUnmatchedPartialFailsLoudly is the error path of #680: a spec
// that extends definitions it does not bind — a partial interface, a partial
// dictionary, an includes naming a missing mixin — would compile into a module
// silently lacking those members, since nothing generated names them. bind
// refuses it before writing anything, with or without its self-check, exits 1,
// and reports every such definition at its position on stderr, leaving stdout,
// which lists the files written, empty. The extending file is passed first; an
// includes whose interface is bound names only its missing mixin.
func TestBindWebIdlUnmatchedPartialFailsLoudly(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	extPath := filepath.Join(dir, "ext.idl")
	basePath := filepath.Join(dir, "base.idl")
	ext := `partial interface Element {
	undefined focus();
};
partial dictionary ScrollOptions {
	boolean smooth;
};
Node includes ParentNode;
`
	if err := os.WriteFile(extPath, []byte(ext), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(basePath, []byte("interface Node {\n\treadonly attribute DOMString nodeName;\n};\n"), 0644); err != nil {
		t.Fatal(err)
	}
	want := []string{
		extPath + ":1:9: partial interface Element: Element is not an interface in the bound IDL files",
		extPath + ":4:9: partial dictionary ScrollOptions: ScrollOptions is not a dictionary in the bound IDL files",
		extPath + ":7:1: Node includes ParentNode: ParentNode is not a mixin in the bound IDL files",
	}

	for _, check := range [][]string{nil, {"-no-check"}} {
		outDir := filepath.Join(dir, "out"+strings.Join(check, ""))
		args := append(append([]string{"bind", "webidl"}, check...), "-name", "ext", "-o", outDir, extPath, basePath)
		var stderr bytes.Buffer
		bindCmd := exec.Command(bin, args...)
		bindCmd.Stderr = &stderr
		out, err := bindCmd.Output()
		if err == nil {
			t.Fatalf("bind %v succeeded on partials and an includes whose targets are unbound (want an error)\n%s%s", check, out, stderr.String())
		}
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
			t.Errorf("bind %v: want exit code 1, got %v", check, err)
		}
		if len(out) != 0 {
			t.Errorf("bind %v: stdout should be empty, got\n%s", check, out)
		}
		if got := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n"); !slices.Equal(got, want) {
			t.Errorf("bind %v: stderr lines\n%q\nwant\n%q", check, got, want)
		}
		if _, err := os.Stat(outDir); !os.IsNotExist(err) {
			t.Errorf("bind %v wrote %s despite the error (stat: %v)", check, outDir, err)
		}
	}
}

// TestBindWebIdlKeepListCompilesClean drives `bind webidl -keep` end to end
// (#18): only the listed interfaces and members are generated, an unlisted
// ancestor of a kept interface stays so its `is` clause resolves, members a
// partial interface, a mixin or a partial dictionary (#656) adds from another
// file are filtered like the definition's own (the list applies after the
// merge), a listed member naming an unlisted interface is dropped and reported
// on stderr, and the reduced module compiles for wasm32-web.
func TestBindWebIdlKeepListCompilesClean(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "dom.idl")
	idl := `interface EventTarget {
	undefined dispatch(DOMString kind);
};

interface Node : EventTarget {
	attribute DOMString textContent;
	Node appendChild(Node node);
};

interface Element : Node {
	readonly attribute DOMString tagName;
	undefined setAttribute(DOMString name, DOMString value);
	undefined remove();
	Fancy fancy();
};

interface Document : Node {
	Element createElement(DOMString localName);
};

interface Fancy {
	undefined sparkle();
};

dictionary Opts {
	DOMString label;
};
`
	if err := os.WriteFile(idlPath, []byte(idl), 0644); err != nil {
		t.Fatal(err)
	}
	// A second spec extends Element, as the standards IDL does across files.
	extPath := filepath.Join(dir, "ext.idl")
	ext := `partial interface Element {
	undefined focus();
	undefined blur();
};

interface mixin ParentNode {
	undefined prepend(DOMString node);
	undefined replaceChildren(DOMString node);
};

Element includes ParentNode;

partial dictionary Opts {
	boolean extra;
	Fancy fancy;
};
`
	if err := os.WriteFile(extPath, []byte(ext), 0644); err != nil {
		t.Fatal(err)
	}
	keepPath := filepath.Join(dir, "keep.txt")
	keep := "Element\nElement.tagName\nElement.setAttribute\nElement.fancy\nElement.focus\nElement.prepend\nDocument\nDocument.createElement\nNode.textContent\nNode.appendChild\n"
	if err := os.WriteFile(keepPath, []byte(keep), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	var stderr bytes.Buffer
	bindCmd := exec.Command(bin, "bind", "webidl", "-keep", keepPath, "-name", "dom", "-o", outDir, idlPath, extPath)
	bindCmd.Stderr = &stderr
	if out, err := bindCmd.Output(); err != nil {
		t.Fatalf("bind webidl -keep failed: %v\n%s%s", err, out, stderr.String())
	}
	for _, want := range []string{"keep: dropped Element.fancy: it names Fancy", "keep: dropped Opts.fancy: it names Fancy"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("bind stderr does not report the dropped member (want %q)\nstderr:\n%s", want, stderr.String())
		}
	}

	pr, err := os.ReadFile(filepath.Join(outDir, "dom.pr"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(pr)
	// EventTarget is unlisted but Node inherits from it, so it stays — without
	// its unlisted member.
	for _, want := range []string{"type EventTarget", "type Node", "type Element", "type Document", "bool? extra;"} {
		if !strings.Contains(src, want) {
			t.Errorf("generated module is missing %q", want)
		}
	}
	for _, absent := range []string{"Fancy", "fancy", "sparkle", "dispatch", "remove", "blur", "replace_children"} {
		if strings.Contains(src, absent) {
			t.Errorf("generated module contains %q, which the keep list drops", absent)
		}
	}

	userPath := filepath.Join(outDir, "use_keep.pr")
	user := `main() {
    doc := Document.borrow(2);
    el := doc.create_element("div");
    el.text_content = "hi";
    el.set_attribute("id", "x");
    page := doc.create_element("section");
    added := page.append_child(el);
    held := Element.borrow(4);
    print_line(held.tag_name);
    held.focus();
    held.prepend("x");
}
`
	if err := os.WriteFile(userPath, []byte(user), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(bin, "emit-ir", "-target", "wasm32-web", outDir).CombinedOutput()
	if err != nil {
		t.Fatalf("emit-ir -target wasm32-web failed (want exit 0): %v\n%s", err, out)
	}
	ir := string(out)
	for _, want := range []string{"@Node.append_child(", "@Element.set_attribute(", "@Document.create_element(", "@Element.focus(", "@Element.prepend("} {
		if !strings.Contains(ir, want) {
			t.Errorf("generated IR is missing %q", want)
		}
	}
}

// TestBindWebIdlPartialDictionary is the end-to-end check for #656: a
// `partial dictionary` adds its members to the generated dictionary type,
// whether it sits beside the dictionary or in another input file (the
// cross-spec shape). The separate file is passed first, so the partial is
// applied to a dictionary that arrives in a later file. No operation takes the
// dictionary: passing one across the boundary is #172's marshalling work.
func TestBindWebIdlPartialDictionary(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind integration test in short mode")
	}
	bin := clitest.Bin(t)

	const dict = "dictionary Opts { DOMString label; };\n"
	const partial = "partial dictionary Opts { boolean extra; };\n"
	type idlFile struct{ name, src string }
	cases := []struct {
		name  string
		files []idlFile // in command-line order
	}{
		{"same file", []idlFile{{"pd.idl", dict + partial}}},
		{"separate file", []idlFile{{"ext.idl", partial}, {"base.idl", dict}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := clitest.TempDir(t)
			outDir := filepath.Join(dir, "out")
			args := []string{"bind", "webidl", "-name", "pd", "-o", outDir}
			for _, f := range tc.files {
				path := filepath.Join(dir, f.name)
				if err := os.WriteFile(path, []byte(f.src), 0644); err != nil {
					t.Fatal(err)
				}
				args = append(args, path)
			}
			if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
				t.Fatalf("bind webidl failed: %v\n%s", err, out)
			}

			pr, err := os.ReadFile(filepath.Join(outDir, "pd.pr"))
			if err != nil {
				t.Fatal(err)
			}
			src := string(pr)
			start := strings.Index(src, "type Opts ")
			if start < 0 {
				t.Fatalf("generated pd.pr has no `type Opts`:\n%s", src)
			}
			block := src[start:]
			if end := strings.Index(block, "\n}"); end >= 0 {
				block = block[:end]
			}
			for _, want := range []string{"string? label;", "bool? extra;"} {
				if !strings.Contains(block, want) {
					t.Errorf("type Opts is missing %q:\n%s", want, block)
				}
			}
		})
	}
}

// TestBindWebIdlKeepListInvocationErrors is the error path of `-keep` (#18): a
// keep list that cannot be read or parsed, or a `-keep` with no value or given
// twice, fails the command before anything is written.
func TestBindWebIdlKeepListInvocationErrors(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "dom.idl")
	if err := os.WriteFile(idlPath, []byte("interface Element {\n\treadonly attribute DOMString tagName;\n};\n"), 0644); err != nil {
		t.Fatal(err)
	}
	goodKeep := filepath.Join(dir, "good.txt")
	if err := os.WriteFile(goodKeep, []byte("Element\n"), 0644); err != nil {
		t.Fatal(err)
	}
	badKeep := filepath.Join(dir, "bad.txt")
	if err := os.WriteFile(badKeep, []byte("Element\nElement.\nElement.tagName\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string // after `bind webidl -o <out>`
		want string
	}{
		{"malformed entry", []string{"-keep", badKeep, idlPath}, badKeep + `:2: malformed keep entry "Element."`},
		{"missing list", []string{"-keep", filepath.Join(dir, "absent.txt"), idlPath}, "error reading keep list"},
		{"no value", []string{idlPath, "-keep"}, "-keep requires an argument"},
		{"given twice", []string{"-keep", goodKeep, "-keep", goodKeep, idlPath}, "-keep given twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outDir := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_"))
			args := append([]string{"bind", "webidl", "-o", outDir}, tc.args...)
			out, err := exec.Command(bin, args...).CombinedOutput()
			if err == nil {
				t.Fatalf("bind webidl %v succeeded (want an error)\n%s", tc.args, out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("want %q in the output\noutput:\n%s", tc.want, out)
			}
			if _, statErr := os.Stat(outDir); !os.IsNotExist(statErr) {
				t.Errorf("bind wrote %s before failing (stat: %v)", outDir, statErr)
			}
		})
	}
}

// TestBindWebIdlNamesOutsideIDLCompileClean is the end-to-end acceptance for
// #722: CSSOMString, WindowProxy and the identifiers a [LegacyWindowAlias] names
// — declared in one file as webref's geometry.idl declares them, used in
// another — lower to what they name (docs/wasm-bindings.md, "Names defined
// outside IDL") as an attribute type, a parameter, a nullable and a dictionary
// field, and bind's self-check passes. Before the fix it failed with
// `undefined type` for each of the three.
func TestBindWebIdlNamesOutsideIDLCompileClean(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind self-check integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	geometryPath := filepath.Join(dir, "geometry.idl")
	geometry := `[Exposed=(Window,Worker),
 Serializable,
 LegacyWindowAlias=SVGPoint]
interface DOMPoint {
  attribute unrestricted double x;
};

[Exposed=(Window,Worker),
 Serializable,
 LegacyWindowAlias=(SVGMatrix,WebKitCSSMatrix)]
interface DOMMatrix {
  readonly attribute unrestricted double a;
};

[Exposed=Window]
interface Window {};
`
	if err := os.WriteFile(geometryPath, []byte(geometry), 0644); err != nil {
		t.Fatal(err)
	}
	usePath := filepath.Join(dir, "use.idl")
	use := `dictionary Opts {
  CSSOMString text;
  WindowProxy view;
  required SVGMatrix transform;
};

[Exposed=Window]
interface A {
  attribute CSSOMString text;
  readonly attribute WindowProxy? opener;
  readonly attribute SVGMatrix m;
  undefined use(CSSOMString s, CSSOMString? ns, WindowProxy w, SVGPoint p, WebKitCSSMatrix k);
  SVGMatrix? getCTM();
  SVGPoint? point();
};
`
	if err := os.WriteFile(usePath, []byte(use), 0644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	out, err := exec.Command(bin, "bind", "webidl", "-name", "probe", "-o", outDir, geometryPath, usePath).CombinedOutput()
	if err != nil {
		t.Fatalf("bind webidl failed its self-check (want exit 0): %v\n%s", err, out)
	}
}

// TestBindWebIdlWindowProxyWithoutWindow: WindowProxy names Window, so with no
// Window bound the name left unresolved is Window — the definition to bind —
// and WindowProxy appears nowhere in the report (#722).
func TestBindWebIdlWindowProxyWithoutWindow(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping bind self-check integration test in short mode")
	}
	bin := clitest.Bin(t)

	dir := clitest.TempDir(t)
	idlPath := filepath.Join(dir, "in.idl")
	idl := "[Exposed=Window]\ninterface A {\n  readonly attribute WindowProxy? opener;\n};\n"
	if err := os.WriteFile(idlPath, []byte(idl), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(bin, "bind", "webidl", "-name", "probe", "-o", filepath.Join(dir, "out"), idlPath).CombinedOutput()
	if err == nil {
		t.Fatalf("bind webidl succeeded with WindowProxy used and no Window bound (want it to fail)\n%s", out)
	}
	// The paths in the output carry this test's name, so match the reports.
	if !strings.Contains(string(out), "undefined type: Window\n") {
		t.Errorf("want the report to name Window, the definition WindowProxy names\noutput:\n%s", out)
	}
	if strings.Contains(string(out), "undefined type: WindowProxy") {
		t.Errorf("the report names WindowProxy, which no definition defines; want only Window\noutput:\n%s", out)
	}
}

// irFunctionBody returns the text of the first `define` whose signature contains
// marker, up to the closing brace at column 0.
func irFunctionBody(ir, marker string) (string, bool) {
	for _, def := range strings.Split(ir, "\ndefine ") {
		if !strings.Contains(def, marker) {
			continue
		}
		if end := strings.Index(def, "\n}"); end >= 0 {
			return def[:end], true
		}
		return def, true
	}
	return "", false
}
