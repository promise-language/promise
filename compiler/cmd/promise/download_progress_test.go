package main

import (
	"os"
	"strings"
	"testing"
)

func TestPrettyLabel(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"llvm-opt", "opt (LLVM)"},
		{"llvm-libLLVM.dylib", "libLLVM.dylib (LLVM)"},
		{"musl-crt1.o", "crt1.o (musl)"},
		{"archive LLVM-22.tar.xz", "archive LLVM-22.tar.xz"},
		{"plain", "plain"},
	}
	for _, tt := range tests {
		if got := prettyLabel(tt.in); got != tt.want {
			t.Errorf("prettyLabel(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMBProgress(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{500, "500 B"},
		{2048, "2 KB"},
		{1572864, "1.5 MB"}, // 1.5 * 1024 * 1024
	}
	for _, tt := range tests {
		if got := mbProgress(tt.bytes); got != tt.want {
			t.Errorf("mbProgress(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

// TestTTYProgressRender drives the sink the way blobstore would and asserts the
// final line carries the label, a percentage, and ends with a newline (Done).
func TestTTYProgressRender(t *testing.T) {
	var buf strings.Builder
	p := newTTYProgress(&buf)
	p.Start("llvm-opt", 1000)
	p.done = 500 // simulate halfway without waiting on the throttle
	p.render()
	p.Advance(500) // → 1000/1000
	p.Done()

	out := buf.String()
	if !strings.Contains(out, "opt (LLVM)") {
		t.Errorf("output missing pretty label: %q", out)
	}
	if !strings.Contains(out, "100%") {
		t.Errorf("output missing final 100%%: %q", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("Done should end the line with a newline: %q", out)
	}
	if !strings.Contains(out, "\r") {
		t.Errorf("progress should use carriage returns for in-place updates: %q", out)
	}
}

// TestTTYProgressUnknownTotal exercises the total<=0 branch (no percentage).
func TestTTYProgressUnknownTotal(t *testing.T) {
	var buf strings.Builder
	p := newTTYProgress(&buf)
	p.Start("archive foo.tar.xz", -1)
	p.done = 4096
	p.render()
	p.Done()
	out := buf.String()
	if strings.Contains(out, "%") {
		t.Errorf("unknown total should not render a percentage: %q", out)
	}
	if !strings.Contains(out, "archive foo.tar.xz") {
		t.Errorf("output missing label: %q", out)
	}
}

// TestIsCharDevice confirms a pipe (the read end) is not treated as a terminal,
// so download prompts/progress stay off in scripts and pipelines.
func TestIsCharDevice(t *testing.T) {
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer rd.Close()
	defer wr.Close()
	if isCharDevice(rd) {
		t.Error("a pipe should not be reported as a character device")
	}
}
