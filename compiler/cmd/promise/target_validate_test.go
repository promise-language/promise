package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen"
)

func TestIsSupportedTarget(t *testing.T) {
	cases := []struct {
		target string
		want   bool
	}{
		{"", true},
		{codegen.HostTargetTriple(), true},
		{"wasm32-wasi", true},
		{"wasm32-web", true},
		// Display short names are NOT accepted as input — downstream tools
		// expect canonical triples.
		{"linux-x86_64", false},
		{"darwin-arm64", false},
		// Bogus values.
		{"foo", false},
		{"linux", false},
		{"x86_64", false},
	}
	for _, c := range cases {
		if got := isSupportedTarget(c.target); got != c.want {
			t.Errorf("isSupportedTarget(%q) = %v, want %v", c.target, got, c.want)
		}
	}
}

func TestInvalidTargetMessage(t *testing.T) {
	msg := invalidTargetMessage("foo")

	for _, want := range []string{
		"error: invalid target 'foo'",
		"supported targets:",
		codegen.HostTargetTriple(),
		"(native)",
		"wasm32-wasi",
		"wasm32-web",
		"Run `promise targets` for details.",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("invalidTargetMessage missing %q\nfull message:\n%s", want, msg)
		}
	}

	// Exactly one (native) marker — the host row.
	if got := strings.Count(msg, "(native)"); got != 1 {
		t.Errorf("expected exactly one (native) marker, got %d\nfull message:\n%s", got, msg)
	}

	// Message must end with a newline so the caller writes a clean block.
	if !strings.HasSuffix(msg, "\n") {
		t.Errorf("expected trailing newline, got %q", msg)
	}
}

// TestInvalidTargetRejectedByBuild exercises the full CLI path: invoking
// `promise build -target foo` against a real source file must fail with our
// formatted error before any module loading runs (no "redeclared in this
// scope" leakage from std/platform.pr).
func TestInvalidTargetRejectedByBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI integration test in short mode")
	}
	bin := findPromiseBinary(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "main.pr")
	if err := os.WriteFile(src, []byte("main() { print_line(\"hi\"); }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	for _, sub := range []string{"build", "run", "emit-ir", "test"} {
		t.Run(sub, func(t *testing.T) {
			args := []string{sub, "-target", "totallybogus", src}
			cmd := exec.Command(bin, args...)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("expected non-zero exit for %s with bad target, got success\noutput:\n%s", sub, out)
			}
			combined := string(out)
			if !strings.Contains(combined, "error: invalid target 'totallybogus'") {
				t.Errorf("%s: missing formatted invalid-target error\noutput:\n%s", sub, combined)
			}
			if strings.Contains(combined, "redeclared in this scope") {
				t.Errorf("%s: leaked spurious std redeclaration error\noutput:\n%s", sub, combined)
			}
		})
	}
}

// TestInvalidTargetRejectedByExec covers `promise exec` separately because
// it accepts inline source rather than a file path.
func TestInvalidTargetRejectedByExec(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI integration test in short mode")
	}
	bin := findPromiseBinary(t)

	cmd := exec.Command(bin, "exec", "-target", "totallybogus", "print_line(\"hi\")")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit, got success\noutput:\n%s", out)
	}
	combined := string(out)
	if !strings.Contains(combined, "error: invalid target 'totallybogus'") {
		t.Errorf("missing formatted invalid-target error\noutput:\n%s", combined)
	}
	if strings.Contains(combined, "redeclared in this scope") {
		t.Errorf("leaked spurious std redeclaration error\noutput:\n%s", combined)
	}
}
