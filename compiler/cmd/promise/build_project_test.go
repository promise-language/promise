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
// file argument, not `.`) run inside a project directory detects the enclosing
// promise.toml and builds the whole project, so sibling declarations are visible
// (T0927). The binary is named after [module].name and an informational note is
// printed to stderr.
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
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "belongs to the project") {
		t.Errorf("expected 'belongs to the project' note on stderr; got:\n%s", out)
	}

	// Binary is named after the project, not the file basename.
	binaryName := "insideproj"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binPath := filepath.Join(dir, binaryName)
	if _, err := os.Stat(binPath); err != nil {
		t.Fatalf("expected binary at %s, got: %v\noutput: %s", binPath, err, out)
	}

	runCmd := exec.Command(binPath)
	runOut, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running %s failed: %v\n%s", binPath, err, runOut)
	}
	if got := strings.TrimSpace(string(runOut)); got != "7" {
		t.Errorf("output = %q, want %q", got, "7")
	}
}

// TestRunFileInsideProject verifies that `promise run main.pr` inside a project
// builds the whole project (sibling visibility) and that the project cache key is
// aligned so a second run produces the same output (T0927, change #3).
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

	for i := 0; i < 2; i++ {
		cmd := exec.Command(bin, "run", "main.pr")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("run #%d failed: %v\n%s", i, err, out)
		}
		// CombinedOutput includes the stderr project note on the cache-miss run;
		// just assert the program's output is present.
		if !strings.Contains(string(out), "42") {
			t.Errorf("run #%d output = %q, want it to contain %q", i, string(out), "42")
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
// own directory before finding the project root (T0927). Every other test
// places promise.toml in the same directory as the file, leaving the walk-up
// loop body uncovered.
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
	if err != nil {
		t.Fatalf("subdir build failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "belongs to the project") {
		t.Errorf("expected 'belongs to the project' note for subdir file; got:\n%s", out)
	}

	// Binary is named after the project and placed at the project root.
	binaryName := "subproj"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binPath := filepath.Join(dir, binaryName)
	if _, err := os.Stat(binPath); err != nil {
		t.Fatalf("expected binary at %s, got: %v\noutput: %s", binPath, err, out)
	}

	runOut, err := exec.Command(binPath).CombinedOutput()
	if err != nil {
		t.Fatalf("running %s failed: %v\n%s", binPath, err, runOut)
	}
	if got := strings.TrimSpace(string(runOut)); got != "9" {
		t.Errorf("output = %q, want %q", got, "9")
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

	prPath := filepath.Join(outDir, "idl.pr")
	emitCmd := exec.Command(bin, "emit-ir", "-target", "wasm32-web", prPath)
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
