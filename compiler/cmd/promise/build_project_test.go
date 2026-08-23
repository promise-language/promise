package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// findPromiseBinary locates the bin/promise binary built by bin/build.
// Skips the test if the binary is not present (e.g. fresh checkout where
// `./make` hasn't run yet).
func findPromiseBinary(t *testing.T) string {
	t.Helper()
	// Walk up from the test's source directory to the repo root.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		bin := filepath.Join(dir, "bin", "promise")
		if runtime.GOOS == "windows" {
			bin = filepath.Join(dir, "bin", "promise.exe")
		}
		if _, err := os.Stat(bin); err == nil {
			return bin
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skipf("bin/promise binary not found — run bin/build first")
	return ""
}

// TestBuildProjectMultiFile verifies that `promise build .` in a directory
// with a promise.toml and multiple .pr files compiles them all together and
// names the binary after the [module].name field.
func TestBuildProjectMultiFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := findPromiseBinary(t)

	dir := t.TempDir()
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
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := findPromiseBinary(t)

	dir := t.TempDir()
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
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := findPromiseBinary(t)

	dir := t.TempDir()
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
	if testing.Short() {
		t.Skip("skipping run integration test in short mode")
	}
	bin := findPromiseBinary(t)

	dir := t.TempDir()
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
	if testing.Short() {
		t.Skip("skipping emit-ir integration test in short mode")
	}
	bin := findPromiseBinary(t)

	dir := t.TempDir()
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
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := findPromiseBinary(t)

	dir := t.TempDir()
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
	if testing.Short() {
		t.Skip("skipping run integration test in short mode")
	}
	bin := findPromiseBinary(t)

	dir := t.TempDir()
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
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := findPromiseBinary(t)

	dir := t.TempDir()
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
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := findPromiseBinary(t)

	dir := t.TempDir()
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
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := findPromiseBinary(t)

	dir := t.TempDir()
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
	if testing.Short() {
		t.Skip("skipping run integration test in short mode")
	}
	bin := findPromiseBinary(t)

	dir := t.TempDir()
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
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	bin := findPromiseBinary(t)

	for _, cmdName := range []string{"build", "run", "emit-ir"} {
		t.Run(cmdName, func(t *testing.T) {
			dir := t.TempDir()
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
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	bin := findPromiseBinary(t)

	for _, cmdName := range []string{"build", "run"} {
		t.Run(cmdName, func(t *testing.T) {
			dir := t.TempDir()
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
	if testing.Short() {
		t.Skip("skipping emit-ir integration test in short mode")
	}
	bin := findPromiseBinary(t)

	dir := t.TempDir()
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
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := findPromiseBinary(t)

	dir := t.TempDir()
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
	if testing.Short() {
		t.Skip("skipping bind+emit-ir integration test in short mode")
	}
	bin := findPromiseBinary(t)

	dir := t.TempDir()
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
	if testing.Short() {
		t.Skip("skipping gc removal-notice integration test in short mode")
	}
	bin := findPromiseBinary(t)

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
	if testing.Short() {
		t.Skip("skipping fetch removal-notice integration test in short mode")
	}
	bin := findPromiseBinary(t)

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
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := findPromiseBinary(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "main.pr")
	// `use path;` is never referenced — an unused import.
	if err := os.WriteFile(src,
		[]byte("use path;\n\nmain() {\n  print_line(\"hi\");\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "run", src)
	cmd.Dir = dir
	// Isolate the build cache so this compile is a guaranteed miss — sema (where
	// the warning is emitted) is skipped on a cache hit, so a shared cache would
	// make the warning assertion flaky.
	cmd.Env = append(os.Environ(), "PROMISE_HOME="+filepath.Join(dir, "home"))
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
	if testing.Short() {
		t.Skip("skipping build integration test in short mode")
	}
	bin := findPromiseBinary(t)

	dir := t.TempDir()
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
