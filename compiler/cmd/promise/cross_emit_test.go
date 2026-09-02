package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen"
)

// emitIRForTarget runs `promise emit-ir -target <triple>` on src and returns the
// generated module text. A non-zero exit fails the test with the compiler's own
// stderr, since that is the only useful diagnostic here.
func emitIRForTarget(t *testing.T, bin, src, triple string) string {
	t.Helper()
	args := []string{"emit-ir"}
	if triple != "" {
		args = append(args, "-target", triple)
	}
	args = append(args, src)
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("emit-ir -target %q failed: %v\n%s", triple, err, out)
	}
	return string(out)
}

// TestEmitIRCrossTargets is T0533 Part 2's smoke test: the same source emitted
// at several targets from a single host must produce distinct modules, each
// carrying its own target triple and its own PAL externs.
//
// Emitting IR needs no sysroot, CRT or linker, so this works from any host —
// which is exactly why it is the part of cross-compilation that can be pinned
// by a unit test rather than by the cross-build matrix (T0537).
func TestEmitIRCrossTargets(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping CLI integration test in short mode")
	}
	bin := locatePromiseBin(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "hello.pr")
	if err := os.WriteFile(src, []byte("main() { print_line(\"hello\"); }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		triple string
		// wantExterns are symbols only this platform's PAL emits.
		wantExterns []string
		// bannedExterns must not appear — they belong to a different platform.
		bannedExterns []string
	}{
		{
			triple:        "x86_64-pc-windows-msvc",
			wantExterns:   []string{"GetStdHandle", "WriteFile"},
			bannedExterns: []string{"fd_write"},
		},
		{
			triple:        "x86_64-unknown-linux-musl",
			bannedExterns: []string{"GetStdHandle", "fd_write"},
		},
		{
			triple:        "wasm32-wasi",
			wantExterns:   []string{"fd_write"},
			bannedExterns: []string{"GetStdHandle"},
		},
	}

	seen := make(map[string]string) // IR text -> triple that produced it
	for _, c := range cases {
		t.Run(c.triple, func(t *testing.T) {
			ir := emitIRForTarget(t, bin, src, c.triple)

			// The module must declare the triple it was asked for.
			wantLine := "target triple = \"" + c.triple + "\""
			if !strings.Contains(ir, wantLine) {
				t.Errorf("missing %s", wantLine)
			}
			for _, sym := range c.wantExterns {
				if !strings.Contains(ir, sym) {
					t.Errorf("expected PAL extern %q in the module", sym)
				}
			}
			for _, sym := range c.bannedExterns {
				if strings.Contains(ir, sym) {
					t.Errorf("unexpected foreign PAL extern %q", sym)
				}
			}

			// Distinct targets must not collapse to the same module — that
			// would mean the triple never reached codegen.
			if other, dup := seen[ir]; dup {
				t.Errorf("byte-identical IR to %s", other)
			}
			seen[ir] = c.triple
		})
	}
	if len(seen) != len(cases) {
		t.Errorf("expected %d distinct IR modules, got %d", len(cases), len(seen))
	}
}

// TestEmitIRAcceptsUnlinkableTarget pins the split between "can emit IR for" and
// "can link for": a cross target whose sysroot does not ship yet is rejected by
// `build` but must still be accepted by `emit-ir`. Before this split the two
// shared one check, so no cross-target IR could be produced at all.
func TestEmitIRAcceptsUnlinkableTarget(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping CLI integration test in short mode")
	}
	bin := locatePromiseBin(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "hello.pr")
	if err := os.WriteFile(src, []byte("main() { print_line(\"hi\"); }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const triple = "x86_64-pc-windows-msvc"
	if isSupportedTarget(triple) {
		t.Skip("this release can link for " + triple + "; nothing to distinguish")
	}

	// emit-ir accepts it.
	ir := emitIRForTarget(t, bin, src, triple)
	if !strings.Contains(ir, "target triple = \""+triple+"\"") {
		t.Errorf("emit-ir did not honour -target %s", triple)
	}

	// build rejects it, and says why without calling it invalid.
	out, err := exec.Command(bin, "build", "-target", triple, src).CombinedOutput()
	if err == nil {
		t.Fatalf("build -target %s unexpectedly succeeded:\n%s", triple, out)
	}
	msg := string(out)
	if !strings.Contains(msg, "cannot be built by this release") {
		t.Errorf("build error should explain the payload is missing, got:\n%s", msg)
	}
	if strings.Contains(msg, "invalid target") {
		t.Errorf("a known target must not be reported as invalid:\n%s", msg)
	}
}

// TestEmitIRDefaultTargetIsHost guards the no-flag path: omitting -target must
// still stamp the host triple, not leave the module target-less.
func TestEmitIRDefaultTargetIsHost(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping CLI integration test in short mode")
	}
	bin := locatePromiseBin(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "hello.pr")
	if err := os.WriteFile(src, []byte("main() { print_line(\"hi\"); }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ir := emitIRForTarget(t, bin, src, "")
	want := "target triple = \"" + codegen.HostTargetTriple() + "\""
	if !strings.Contains(ir, want) {
		t.Errorf("emit-ir with no -target: missing %s", want)
	}
}
