package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	// Read concurrently to avoid deadlock when output exceeds pipe buffer.
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(r)
		ch <- result{data, err}
	}()

	fn()
	w.Close()
	os.Stdout = old

	res := <-ch
	if res.err != nil {
		t.Fatal(res.err)
	}
	return string(res.data)
}

func TestRunGuideDefault(t *testing.T) {
	output := captureStdout(t, func() {
		runGuide(nil)
	})

	// Should contain the full guide content.
	for _, want := range []string{
		"# Promise Language Guide",
		"## Basics",
		"## Error Handling",
		"## Ownership",
		"## Concurrency",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("full guide missing %q", want)
		}
	}
}

func TestRunGuideSection(t *testing.T) {
	output := captureStdout(t, func() {
		runGuide([]string{"-section", "basics"})
	})

	if !strings.Contains(output, "## Basics") {
		t.Error("section output missing '## Basics' heading")
	}
	// Should NOT contain other top-level sections.
	if strings.Contains(output, "## Error Handling") {
		t.Error("section output should not contain other sections")
	}
}

func TestRunGuideSectionHyphenated(t *testing.T) {
	output := captureStdout(t, func() {
		runGuide([]string{"-section", "error-handling"})
	})

	if !strings.Contains(output, "## Error Handling") {
		t.Error("hyphenated section lookup failed")
	}
}

func TestRunGuideHelp(t *testing.T) {
	output := captureStdout(t, func() {
		runGuide([]string{"-help"})
	})

	for _, want := range []string{
		"-section",
		"-path",
		"-install",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("guide help missing %q", want)
		}
	}
}

func TestExtractSection(t *testing.T) {
	guide := "# Title\n\n## Foo\nfoo content\n\n## Bar\nbar content\n"

	foo := extractSection(guide, "foo")
	if !strings.Contains(foo, "## Foo") {
		t.Error("expected Foo heading")
	}
	if !strings.Contains(foo, "foo content") {
		t.Error("expected foo content")
	}
	if strings.Contains(foo, "## Bar") {
		t.Error("should not include Bar section")
	}

	bar := extractSection(guide, "bar")
	if !strings.Contains(bar, "## Bar") {
		t.Error("expected Bar heading")
	}
	if !strings.Contains(bar, "bar content") {
		t.Error("expected bar content")
	}

	none := extractSection(guide, "nonexistent")
	if none != "" {
		t.Error("expected empty string for nonexistent section")
	}
}

func TestSectionSlug(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"Error Handling", "error-handling"},
		{"error-handling", "error-handling"},
		{"Ownership & Borrowing", "ownership-borrowing"},
		{"Collections (auto-imported from std)", "collections"},
		{"Fixed-Size Arrays", "fixed-size-arrays"},
	}
	for _, tt := range tests {
		got := sectionSlug(tt.input)
		if got != tt.want {
			t.Errorf("sectionSlug(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestListSections(t *testing.T) {
	sections := listSections()
	if len(sections) == 0 {
		t.Fatal("no sections found in embedded guide")
	}
	// The guide should have at least these sections.
	found := map[string]bool{}
	for _, s := range sections {
		found[s] = true
	}
	for _, want := range []string{"Basics", "Error Handling", "Concurrency"} {
		if !found[want] {
			t.Errorf("missing expected section %q", want)
		}
	}
}

func TestInstallGuide(t *testing.T) {
	// Use a temp dir as PROMISE_HOME to avoid polluting real home.
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	path, err := installGuide()
	if err != nil {
		t.Fatalf("installGuide: %v", err)
	}

	if !strings.HasSuffix(path, "language-guide.md") {
		t.Errorf("unexpected path: %s", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed guide: %v", err)
	}
	if !strings.Contains(string(data), "# Promise Language Guide") {
		t.Error("installed guide missing expected content")
	}

	// Second call should be a no-op (version stamp match).
	path2, err := installGuide()
	if err != nil {
		t.Fatalf("second installGuide: %v", err)
	}
	if path2 != path {
		t.Errorf("path changed on second call: %q vs %q", path, path2)
	}
}
