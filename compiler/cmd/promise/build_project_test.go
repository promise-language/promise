package main

import (
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
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "42" {
		t.Errorf("output = %q, want %q", got, "42")
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

// TestBindWebIdlJsValueDocParses is the end-to-end regression for T0717: an IDL
// whose interface has a union-typed attribute flips HasJsValue, so `promise bind
// webidl` emits the JsValue enum. That enum carries `doc annotations that, prior
// to the fix, were written in the invalid space form on a preceding line — a
// *fatal parse error* on line 1 that masked the whole file. This drives the real
// CLI path (bind → emit-ir) and asserts those ANTLR parse diagnostics are gone.
//
// It deliberately does NOT require a clean (exit 0) compile: the JsValue enum is
// still mis-lowered as a resource handle (T0723), a separate codegen layer, so
// emit-ir currently exits non-zero with a *sema* error. This test stays green
// both now and once T0723 lands (when there are no diagnostics at all).
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
