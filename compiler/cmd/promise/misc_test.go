package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirSize(t *testing.T) {
	tmp := t.TempDir()
	// Write two 100-byte files.
	os.WriteFile(filepath.Join(tmp, "a.txt"), make([]byte, 100), 0644)
	os.MkdirAll(filepath.Join(tmp, "sub"), 0755)
	os.WriteFile(filepath.Join(tmp, "sub", "b.txt"), make([]byte, 200), 0644)

	size := dirSize(tmp)
	if size != 300 {
		t.Fatalf("expected 300, got %d", size)
	}
}

func TestPrintVersionWithLdflags(t *testing.T) {
	// When version is set via -ldflags, printVersion uses it.
	old := version
	version = "2026.0-abc1234"
	defer func() { version = old }()

	// Capture stdout.
	r, w, _ := os.Pipe()
	oldStdout := os.Stdout
	os.Stdout = w
	printVersion()
	w.Close()
	os.Stdout = oldStdout

	var buf [256]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])
	if output != "promise version 2026.0-abc1234\n" {
		t.Fatalf("expected 'promise version 2026.0-abc1234\\n', got %q", output)
	}
}

func TestPrintVersionWithCommit(t *testing.T) {
	// When both version and commit are set, printVersion appends "(commit <sha>)".
	oldV, oldC := version, commit
	version = "2026.0"
	commit = "0123456789abcdef0123456789abcdef01234567"
	defer func() { version = oldV; commit = oldC }()

	r, w, _ := os.Pipe()
	oldStdout := os.Stdout
	os.Stdout = w
	printVersion()
	w.Close()
	os.Stdout = oldStdout

	var buf [256]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])
	want := "promise version 2026.0 (commit 0123456789abcdef0123456789abcdef01234567)\n"
	if output != want {
		t.Fatalf("expected %q, got %q", want, output)
	}
}

func TestPrintVersionFallback(t *testing.T) {
	// When version is empty, printVersion falls back to embedded catalog epoch.
	old := version
	version = ""
	defer func() { version = old }()

	r, w, _ := os.Pipe()
	oldStdout := os.Stdout
	os.Stdout = w
	printVersion()
	w.Close()
	os.Stdout = oldStdout

	var buf [256]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])
	if !strings.HasPrefix(output, "promise version ") {
		t.Fatalf("expected output starting with 'promise version ', got %q", output)
	}
	// Should not be "unknown" since we have an embedded catalog.
	if strings.Contains(output, "unknown") {
		t.Fatal("expected a real epoch, got 'unknown'")
	}
}

func TestFormatSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1 KB"},
		{1024 * 1024, "1 MB"},
		{67 * 1024 * 1024, "67 MB"},
	}
	for _, tt := range tests {
		got := formatSize(tt.bytes)
		if got != tt.want {
			t.Errorf("formatSize(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}
