package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
