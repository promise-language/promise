package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInitCreatesFiles(t *testing.T) {
	// Run in a temp directory.
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(dir)

	// Capture stdout.
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	runInit()

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
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("file %s not created: %v", want, err)
		}
	}

	// Check promise.toml content.
	toml, _ := os.ReadFile(filepath.Join(dir, "promise.toml"))
	dirName := filepath.Base(dir)
	if !strings.Contains(string(toml), dirName) {
		t.Errorf("promise.toml missing directory name %q", dirName)
	}

	// Check main.pr content.
	mainPr, _ := os.ReadFile(filepath.Join(dir, "main.pr"))
	for _, want := range []string{"use io;", "use os;", "main!()", "print_line", "io.Dir.list"} {
		if !strings.Contains(string(mainPr), want) {
			t.Errorf("main.pr missing %q", want)
		}
	}

	// Check CLAUDE.md content.
	claude, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	for _, want := range []string{
		"promise guide",
		"promise run",
		"promise test",
		"promise doc",
		"main!()",
		"Error Handling",
		"Module Rules",
		"Available Modules",
	} {
		if !strings.Contains(string(claude), want) {
			t.Errorf("CLAUDE.md missing %q", want)
		}
	}
}

func TestRunInitDoesNotOverwriteExistingFiles(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(dir)

	// Pre-create main.pr and CLAUDE.md with custom content.
	os.WriteFile(filepath.Join(dir, "main.pr"), []byte("custom main"), 0644)
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("custom claude"), 0644)

	// Capture stdout.
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	runInit()

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
	mainPr, _ := os.ReadFile(filepath.Join(dir, "main.pr"))
	if string(mainPr) != "custom main" {
		t.Errorf("main.pr was overwritten: got %q", string(mainPr))
	}
	claude, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if string(claude) != "custom claude" {
		t.Errorf("CLAUDE.md was overwritten: got %q", string(claude))
	}
}
