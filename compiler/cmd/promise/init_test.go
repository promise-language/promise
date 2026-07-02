package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/module"
)

func TestRunInitCreatesFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "myproject")

	// Capture stdout.
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	runInit([]string{target})

	w.Close()
	os.Stdout = old

	buf := make([]byte, 64*1024)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	// Check all three files created.
	for _, want := range []string{"promise.toml", "main.pr", "CLAUDE.md"} {
		if !strings.Contains(output, "Created "+want) {
			t.Errorf("output missing 'Created %s'", want)
		}
		if _, err := os.Stat(filepath.Join(target, want)); err != nil {
			t.Errorf("file %s not created: %v", want, err)
		}
	}

	// Check promise.toml content.
	toml, _ := os.ReadFile(filepath.Join(target, "promise.toml"))
	if !strings.Contains(string(toml), "myproject") {
		t.Errorf("promise.toml missing directory name %q", "myproject")
	}

	// T0972: the scaffolded epoch must be derived from the running compiler,
	// not a stale hardcoded literal, so a fresh project actually builds.
	wantEpoch, err := module.CompilerEpoch(embeddedCatalog)
	if err != nil || wantEpoch == "" {
		wantEpoch = "2026.1" // mirror runInit's fallback
	}
	if !strings.Contains(string(toml), fmt.Sprintf("epoch = %q", wantEpoch)) {
		t.Errorf("promise.toml epoch missing %q; got:\n%s", wantEpoch, toml)
	}
	if wantEpoch != "2026.0" && strings.Contains(string(toml), `epoch = "2026.0"`) {
		t.Errorf("promise.toml carries stale hardcoded epoch 2026.0")
	}
	// The confirmation line should advertise the same epoch.
	if !strings.Contains(output, "epoch: "+wantEpoch) {
		t.Errorf("output missing 'epoch: %s'; got:\n%s", wantEpoch, output)
	}

	// T1010: app scaffold must emit explicit main = "main.pr".
	if !strings.Contains(string(toml), `main = "main.pr"`) {
		t.Errorf("promise.toml missing explicit main = \"main.pr\" for app scaffold; got:\n%s", toml)
	}

	// Check main.pr content.
	mainPr, _ := os.ReadFile(filepath.Join(target, "main.pr"))
	for _, want := range []string{"use io;", "use os;", "main!()", "print_line", "io.Dir.list"} {
		if !strings.Contains(string(mainPr), want) {
			t.Errorf("main.pr missing %q", want)
		}
	}
	// T0699: template includes a documented public function so `promise doc`
	// against a freshly-initialized project renders non-empty output.
	for _, want := range []string{
		"greet(string name) string `public",
		"`doc(\"Returns a friendly greeting for the given name.\")",
		`greet("Promise")`,
	} {
		if !strings.Contains(string(mainPr), want) {
			t.Errorf("main.pr missing T0699 template fragment %q", want)
		}
	}

	// Check CLAUDE.md content.
	claude, _ := os.ReadFile(filepath.Join(target, "CLAUDE.md"))
	for _, want := range []string{
		"promise guide",
		"promise run",
		"promise test",
		"promise doc",
		"main!()",
		"Error Handling",
		"Module Rules",
		"Available Modules",
		"f()?^",
		"f()?!",
		"f() ? {",
	} {
		if !strings.Contains(string(claude), want) {
			t.Errorf("CLAUDE.md missing %q", want)
		}
	}
	// T0692: ensure stale/incorrect error-handling forms are not present.
	for _, unwanted := range []string{
		"do_thing()!",
		"do_thing()?",
		"try_thing()",
	} {
		if strings.Contains(string(claude), unwanted) {
			t.Errorf("CLAUDE.md contains stale error-handling example %q", unwanted)
		}
	}
}

func TestRunInitDoesNotOverwriteExistingFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "myproject2")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}

	// Pre-create main.pr and CLAUDE.md with custom content.
	os.WriteFile(filepath.Join(target, "main.pr"), []byte("custom main"), 0644)
	os.WriteFile(filepath.Join(target, "CLAUDE.md"), []byte("custom claude"), 0644)

	// Capture stdout.
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	runInit([]string{"--force", target})

	w.Close()
	os.Stdout = old

	buf := make([]byte, 64*1024)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	// promise.toml should be created.
	if !strings.Contains(output, "Created promise.toml") {
		t.Error("output missing 'Created promise.toml'")
	}

	// main.pr and CLAUDE.md should NOT appear in output (not overwritten).
	if strings.Contains(output, "Created main.pr") {
		t.Error("output should not say 'Created main.pr' when it already exists")
	}
	if strings.Contains(output, "Created CLAUDE.md") {
		t.Error("output should not say 'Created CLAUDE.md' when it already exists")
	}

	// Verify content was preserved.
	mainPr, _ := os.ReadFile(filepath.Join(target, "main.pr"))
	if string(mainPr) != "custom main" {
		t.Errorf("main.pr was overwritten: got %q", string(mainPr))
	}
	claude, _ := os.ReadFile(filepath.Join(target, "CLAUDE.md"))
	if string(claude) != "custom claude" {
		t.Errorf("CLAUDE.md was overwritten: got %q", string(claude))
	}
}

func TestRunInitDefaultDir(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(dir)

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	runInit([]string{}) // no args → current directory

	w.Close()
	os.Stdout = old

	buf := make([]byte, 64*1024)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "Created promise.toml") {
		t.Errorf("expected 'Created promise.toml' in output, got: %s", output)
	}
	if _, err := os.Stat(filepath.Join(dir, "promise.toml")); err != nil {
		t.Errorf("promise.toml not created in current dir: %v", err)
	}

	// Module name should be the directory base, not empty
	toml, _ := os.ReadFile(filepath.Join(dir, "promise.toml"))
	if !strings.Contains(string(toml), filepath.Base(dir)) {
		t.Errorf("promise.toml missing directory base name %q", filepath.Base(dir))
	}
}

// TestRunInitNonEmptyDirErrors verifies that init fails when the target dir is
// non-empty and --force is not given.
func TestRunInitNonEmptyDirErrors(t *testing.T) {
	if os.Getenv("TEST_INIT_NON_EMPTY") == "1" {
		dir := os.Getenv("TEST_INIT_TARGET")
		runInit([]string{dir})
		return
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "existing")
	os.MkdirAll(target, 0755)
	os.WriteFile(filepath.Join(target, "something.txt"), []byte("data"), 0644)

	cmd := exec.Command(os.Args[0], "-test.run=TestRunInitNonEmptyDirErrors")
	cmd.Env = append(os.Environ(), "TEST_INIT_NON_EMPTY=1", "TEST_INIT_TARGET="+target)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for non-empty dir without --force")
	}
	if !strings.Contains(string(out), "not empty") {
		t.Errorf("expected 'not empty' in output, got: %s", out)
	}
}

// TestRunInitTomlExistsErrors verifies that init fails when promise.toml already exists.
func TestRunInitTomlExistsErrors(t *testing.T) {
	if os.Getenv("TEST_INIT_TOML_EXISTS") == "1" {
		dir := os.Getenv("TEST_INIT_TARGET2")
		runInit([]string{"--force", dir})
		return
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "hastoml")
	os.MkdirAll(target, 0755)
	os.WriteFile(filepath.Join(target, "promise.toml"), []byte("[module]\nname = \"old\"\n"), 0644)

	cmd := exec.Command(os.Args[0], "-test.run=TestRunInitTomlExistsErrors")
	cmd.Env = append(os.Environ(), "TEST_INIT_TOML_EXISTS=1", "TEST_INIT_TARGET2="+target)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit when promise.toml already exists")
	}
	if !strings.Contains(string(out), "promise.toml already exists") {
		t.Errorf("expected 'promise.toml already exists' in output, got: %s", out)
	}
}

// TestRunInitTargetIsFile verifies that init fails when the target path is a
// file rather than a directory.
func TestRunInitTargetIsFile(t *testing.T) {
	if os.Getenv("TEST_INIT_IS_FILE") == "1" {
		target := os.Getenv("TEST_INIT_TARGET_FILE")
		runInit([]string{target})
		return
	}

	dir := t.TempDir()
	// Create a plain file at the target path.
	filePath := filepath.Join(dir, "notadir.txt")
	os.WriteFile(filePath, []byte("data"), 0644)

	cmd := exec.Command(os.Args[0], "-test.run=TestRunInitTargetIsFile")
	cmd.Env = append(os.Environ(), "TEST_INIT_IS_FILE=1", "TEST_INIT_TARGET_FILE="+filePath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit when target path is a file")
	}
	if !strings.Contains(string(out), "not a directory") {
		t.Errorf("expected 'not a directory' in output, got: %s", out)
	}
}

// TestRunInitModuleDefaultDir verifies that --module with no dir arg uses the
// current directory, naming the module after it.
func TestRunInitModuleDefaultDir(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(dir)

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	runInit([]string{"--module"}) // no dir → current directory

	w.Close()
	os.Stdout = old
	buf := make([]byte, 64*1024)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "Created promise.toml") {
		t.Errorf("expected 'Created promise.toml' in output, got: %s", output)
	}

	// promise.toml must exist and use dir base as module name.
	toml, err := os.ReadFile(filepath.Join(dir, "promise.toml"))
	if err != nil {
		t.Fatalf("promise.toml not created: %v", err)
	}
	base := filepath.Base(dir)
	if !strings.Contains(string(toml), base) {
		t.Errorf("promise.toml missing dir base name %q; got:\n%s", base, toml)
	}
	if strings.Contains(string(toml), "main =") {
		t.Errorf("promise.toml should not contain 'main =' for --module; got:\n%s", toml)
	}

	// <name>.pr must exist, not main.pr.
	libFile := filepath.Join(dir, base+".pr")
	if _, err := os.Stat(libFile); err != nil {
		t.Errorf("%s.pr not created: %v", base, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "main.pr")); err == nil {
		t.Error("main.pr should not exist for --module scaffold")
	}
}

// TestRunInitModuleForce verifies that --module --force works in a non-empty directory.
func TestRunInitModuleForce(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mylib2")
	os.MkdirAll(target, 0755)
	// Populate directory with an existing file (non-empty).
	os.WriteFile(filepath.Join(target, "existing.txt"), []byte("data"), 0644)

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	runInit([]string{"--module", "--force", target})

	w.Close()
	os.Stdout = old
	buf := make([]byte, 64*1024)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	// Should succeed and create promise.toml and mylib2.pr.
	for _, want := range []string{"promise.toml", "mylib2.pr"} {
		if !strings.Contains(output, "Created "+want) {
			t.Errorf("output missing 'Created %s'; got: %s", want, output)
		}
		if _, err := os.Stat(filepath.Join(target, want)); err != nil {
			t.Errorf("file %s not created: %v", want, err)
		}
	}
	// No main.pr.
	if _, err := os.Stat(filepath.Join(target, "main.pr")); err == nil {
		t.Error("main.pr should not exist for --module scaffold")
	}
	// No main field in toml.
	toml, _ := os.ReadFile(filepath.Join(target, "promise.toml"))
	if strings.Contains(string(toml), "main =") {
		t.Errorf("promise.toml should not contain 'main =' for --module; got:\n%s", toml)
	}
}

// TestRunInitModuleNoOverwrite verifies that --module does not overwrite
// pre-existing <name>.pr or CLAUDE.md files, while still creating promise.toml.
func TestRunInitModuleNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mylib3")
	os.MkdirAll(target, 0755)

	// Pre-create lib source and CLAUDE.md with custom content.
	os.WriteFile(filepath.Join(target, "mylib3.pr"), []byte("custom lib"), 0644)
	os.WriteFile(filepath.Join(target, "CLAUDE.md"), []byte("custom claude"), 0644)

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	runInit([]string{"--module", "--force", target})

	w.Close()
	os.Stdout = old
	buf := make([]byte, 64*1024)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	// promise.toml should be created.
	if !strings.Contains(output, "Created promise.toml") {
		t.Errorf("output missing 'Created promise.toml'; got: %s", output)
	}
	// mylib3.pr and CLAUDE.md should NOT appear in output (not overwritten).
	if strings.Contains(output, "Created mylib3.pr") {
		t.Error("output should not say 'Created mylib3.pr' when it already exists")
	}
	if strings.Contains(output, "Created CLAUDE.md") {
		t.Error("output should not say 'Created CLAUDE.md' when it already exists")
	}

	// Verify custom content is preserved.
	libPr, _ := os.ReadFile(filepath.Join(target, "mylib3.pr"))
	if string(libPr) != "custom lib" {
		t.Errorf("mylib3.pr was overwritten: got %q", string(libPr))
	}
	claude, _ := os.ReadFile(filepath.Join(target, "CLAUDE.md"))
	if string(claude) != "custom claude" {
		t.Errorf("CLAUDE.md was overwritten: got %q", string(claude))
	}
}

// TestRunInitModuleFlag verifies that --module scaffolds a library (no main.pr,
// no main field in promise.toml, public stub in <name>.pr, module-style CLAUDE.md).
func TestRunInitModuleFlag(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mylib")

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	runInit([]string{"--module", target})

	w.Close()
	os.Stdout = old
	buf := make([]byte, 64*1024)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	// promise.toml and mylib.pr must be created; main.pr must not exist.
	for _, want := range []string{"promise.toml", "mylib.pr"} {
		if !strings.Contains(output, "Created "+want) {
			t.Errorf("output missing 'Created %s'", want)
		}
		if _, err := os.Stat(filepath.Join(target, want)); err != nil {
			t.Errorf("file %s not created: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "main.pr")); err == nil {
		t.Error("main.pr should not exist for --module scaffold")
	}

	// promise.toml: module name present, no main field.
	toml, _ := os.ReadFile(filepath.Join(target, "promise.toml"))
	if !strings.Contains(string(toml), "mylib") {
		t.Errorf("promise.toml missing module name; got:\n%s", toml)
	}
	if strings.Contains(string(toml), "main =") {
		t.Errorf("promise.toml should not contain 'main =' for --module; got:\n%s", toml)
	}

	// Library source: public and doc annotations, no main().
	libPr, _ := os.ReadFile(filepath.Join(target, "mylib.pr"))
	for _, want := range []string{"`public", "`doc("} {
		if !strings.Contains(string(libPr), want) {
			t.Errorf("mylib.pr missing %q", want)
		}
	}
	if strings.Contains(string(libPr), "main()") || strings.Contains(string(libPr), "main!()") {
		t.Error("mylib.pr should not contain main() or main!()")
	}

	// CLAUDE.md: no 'promise run', has 'use mylib;'.
	claude, _ := os.ReadFile(filepath.Join(target, "CLAUDE.md"))
	if strings.Contains(string(claude), "promise run") {
		t.Error("module CLAUDE.md should not contain 'promise run'")
	}
	if !strings.Contains(string(claude), "use mylib;") {
		t.Error("module CLAUDE.md should contain 'use mylib;'")
	}
}
