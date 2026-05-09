package main

import (
	"os"
	"strings"
	"testing"
)

func TestRunExamplesDefault(t *testing.T) {
	output := captureStdout(t, func() {
		runExamples(nil)
	})

	// Should contain category headings.
	for _, want := range []string{
		"Basics",
		"Types",
		"Error Handling",
		"Ownership",
		"Collections",
		"Concurrency",
		"Patterns",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("example listing missing category %q", want)
		}
	}

	// Should contain specific example names.
	for _, want := range []string{
		"hello",
		"channels",
		"state_machine",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("example listing missing example %q", want)
		}
	}

	// Should contain the usage hint.
	if !strings.Contains(output, "promise examples <name>") {
		t.Error("listing missing usage hint")
	}
}

func TestRunExamplesViewSource(t *testing.T) {
	output := captureStdout(t, func() {
		runExamples([]string{"hello"})
	})

	if !strings.Contains(output, "Hello, world!") {
		t.Error("hello example missing expected content")
	}
	if !strings.Contains(output, "print_line") {
		t.Error("hello example missing print_line call")
	}
}

func TestRunExamplesHelp(t *testing.T) {
	output := captureStdout(t, func() {
		runExamples([]string{"--help"})
	})

	for _, want := range []string{
		"--run",
		"--dir",
		"promise examples",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("examples help missing %q", want)
		}
	}
}

func TestLoadExamples(t *testing.T) {
	entries := loadExamples()
	if len(entries) == 0 {
		t.Fatal("no examples found in embedded FS")
	}

	// Check we have entries from multiple categories.
	cats := map[string]bool{}
	for _, e := range entries {
		cats[e.category] = true
	}
	if len(cats) < 5 {
		t.Errorf("expected at least 5 categories, got %d", len(cats))
	}

	// Every entry should have a description (first-line comment).
	for _, e := range entries {
		if e.desc == "" {
			t.Errorf("example %s/%s has no description", e.category, e.name)
		}
	}
}

func TestCategoryDisplayName(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"01_basics", "Basics"},
		{"03_error_handling", "Error Handling"},
		{"06_functions_advanced", "Functions Advanced"},
		{"09_patterns", "Patterns"},
	}
	for _, tt := range tests {
		got := categoryDisplayName(tt.input)
		if got != tt.want {
			t.Errorf("categoryDisplayName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestExampleSlug(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"hello", "hello"},
		{"Hello", "hello"},
		{"move_and_borrow", "move_and_borrow"},
		{"move-and-borrow", "move_and_borrow"},
		{"hello.pr", "hello"},
		{" hello ", "hello"},
	}
	for _, tt := range tests {
		got := exampleSlug(tt.input)
		if got != tt.want {
			t.Errorf("exampleSlug(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFindEmbeddedExample(t *testing.T) {
	// By stem name.
	e := findEmbeddedExample("hello")
	if e == nil {
		t.Fatal("findEmbeddedExample(hello) returned nil")
	}
	if e.name != "hello" {
		t.Errorf("expected name=hello, got %q", e.name)
	}

	// Case-insensitive.
	e = findEmbeddedExample("Hello")
	if e == nil {
		t.Fatal("findEmbeddedExample(Hello) returned nil")
	}

	// Category/stem path.
	e = findEmbeddedExample("basics/hello")
	if e == nil {
		t.Fatal("findEmbeddedExample(basics/hello) returned nil")
	}
	if e.name != "hello" {
		t.Errorf("expected name=hello, got %q", e.name)
	}

	// Full category path with number prefix.
	e = findEmbeddedExample("01_basics/hello")
	if e == nil {
		t.Fatal("findEmbeddedExample(01_basics/hello) returned nil")
	}

	// Nonexistent.
	e = findEmbeddedExample("nonexistent_example_xyz")
	if e != nil {
		t.Errorf("expected nil for nonexistent example, got %+v", e)
	}
}

func TestInstallExamples(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	path, err := installExamples()
	if err != nil {
		t.Fatalf("installExamples: %v", err)
	}

	if !strings.HasSuffix(path, "examples") {
		t.Errorf("unexpected path: %s", path)
	}

	// Check a known file was extracted.
	helloPath := path + "/01_basics/hello.pr"
	data, err := os.ReadFile(helloPath)
	if err != nil {
		t.Fatalf("read extracted hello.pr: %v", err)
	}
	if !strings.Contains(string(data), "Hello, world!") {
		t.Error("extracted hello.pr missing expected content")
	}

	// Check version stamp was written.
	stamp, err := os.ReadFile(path + "/.examples-version")
	if err != nil {
		t.Fatalf("read version stamp: %v", err)
	}
	if len(stamp) == 0 {
		t.Error("version stamp is empty")
	}

	// Second call should be a no-op (version stamp match).
	path2, err := installExamples()
	if err != nil {
		t.Fatalf("second installExamples: %v", err)
	}
	if path2 != path {
		t.Errorf("path changed on second call: %q vs %q", path, path2)
	}
}

func TestFindExampleFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	path, err := installExamples()
	if err != nil {
		t.Fatalf("installExamples: %v", err)
	}

	// Find by stem.
	f := findExampleFile(path, "hello")
	if f == "" {
		t.Fatal("findExampleFile(hello) returned empty")
	}
	if !strings.HasSuffix(f, "hello.pr") {
		t.Errorf("unexpected path: %s", f)
	}

	// Find by category/stem.
	f = findExampleFile(path, "basics/hello")
	if f == "" {
		t.Fatal("findExampleFile(basics/hello) returned empty")
	}

	// Nonexistent.
	f = findExampleFile(path, "nonexistent_xyz")
	if f != "" {
		t.Errorf("expected empty for nonexistent, got %q", f)
	}
}

func TestListExampleNames(t *testing.T) {
	// Redirect stderr to capture output.
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	listExampleNames(os.Stderr)
	w.Close()
	os.Stderr = old
	buf := make([]byte, 64*1024)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "hello") {
		t.Error("listExampleNames missing 'hello'")
	}
	if !strings.Contains(output, "channels") {
		t.Error("listExampleNames missing 'channels'")
	}
}

func TestReadFirstComment(t *testing.T) {
	desc := readFirstComment("resources/examples/01_basics/hello.pr")
	if desc == "" {
		t.Error("expected non-empty description for hello.pr")
	}
	if !strings.Contains(desc, "simplest") {
		t.Errorf("unexpected description: %q", desc)
	}
}
