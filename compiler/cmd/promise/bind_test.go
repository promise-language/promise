package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/module"
)

// TestBindEpochMatchesCompiler verifies that generated bind module scaffolds
// (promise.toml for `promise bind wit`/`promise bind webidl`) carry the running
// compiler's epoch rather than a stale hardcoded literal (T0972). Both bind
// writers use bindEpoch(), so asserting the helper covers both call sites.
func TestBindEpochMatchesCompiler(t *testing.T) {
	want, err := module.CompilerEpoch(embeddedCatalog)
	if err != nil || want == "" {
		want = "2026.1" // mirror bindEpoch's fallback
	}
	if got := bindEpoch(); got != want {
		t.Errorf("bindEpoch() = %q, want %q", got, want)
	}
	if got := bindEpoch(); got == "2026.0" && want != "2026.0" {
		t.Errorf("bindEpoch() returned stale hardcoded epoch 2026.0")
	}
}

// wantBindEpoch mirrors bindEpoch's resolution for use as the expected value in
// the end-to-end scaffold assertions below.
func wantBindEpoch(t *testing.T) string {
	t.Helper()
	want, err := module.CompilerEpoch(embeddedCatalog)
	if err != nil || want == "" {
		want = "2026.1"
	}
	return want
}

// assertScaffoldEpoch reads the promise.toml written into dir and checks that it
// advertises the running compiler's epoch and never the stale "2026.0" literal
// that T0972 fixed.
func assertScaffoldEpoch(t *testing.T, dir, want string) {
	t.Helper()
	toml, err := os.ReadFile(filepath.Join(dir, "promise.toml"))
	if err != nil {
		t.Fatalf("promise.toml not written: %v", err)
	}
	if !strings.Contains(string(toml), fmt.Sprintf("epoch = %q", want)) {
		t.Errorf("promise.toml epoch missing %q; got:\n%s", want, toml)
	}
	if want != "2026.0" && strings.Contains(string(toml), `epoch = "2026.0"`) {
		t.Errorf("promise.toml carries stale hardcoded epoch 2026.0")
	}
}

// silenceStdout redirects os.Stdout to /dev/null for the duration of fn — the
// bind writers print generated file paths we don't care about here.
func silenceStdout(t *testing.T, fn func()) {
	t.Helper()
	old := os.Stdout
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = devnull
	defer func() {
		os.Stdout = old
		devnull.Close()
	}()
	fn()
}

// TestRunBindWitScaffoldEpoch drives `promise bind wit` end-to-end on a minimal
// WIT package and asserts the generated promise.toml carries the running
// compiler's epoch (T0972) — covering the actual toml-write call site in
// runBindWit, not just the bindEpoch helper.
func TestRunBindWitScaffoldEpoch(t *testing.T) {
	dir := t.TempDir()
	witPath := filepath.Join(dir, "thing.wit")
	if err := os.WriteFile(witPath, []byte("package example:thing@0.1.0;\n"), 0644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")

	silenceStdout(t, func() {
		runBindWit([]string{"-o", outDir, witPath})
	})

	assertScaffoldEpoch(t, outDir, wantBindEpoch(t))
}

// TestRunBindWebIdlScaffoldEpoch drives `promise bind webidl` end-to-end on a
// minimal WebIDL interface and asserts the generated promise.toml carries the
// running compiler's epoch (T0972) — covering runBindWebIdl's toml-write call
// site.
func TestRunBindWebIdlScaffoldEpoch(t *testing.T) {
	dir := t.TempDir()
	idlPath := filepath.Join(dir, "widget.webidl")
	if err := os.WriteFile(idlPath, []byte("interface Widget {};\n"), 0644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")

	silenceStdout(t, func() {
		runBindWebIdl([]string{"-o", outDir, idlPath})
	})

	assertScaffoldEpoch(t, outDir, wantBindEpoch(t))
}
