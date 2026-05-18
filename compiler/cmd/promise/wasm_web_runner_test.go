package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsWasmWebTarget(t *testing.T) {
	cases := []struct {
		target string
		want   bool
	}{
		{"wasm32-unknown-web", true},
		{"wasm32-web", true},
		{"wasm32-unknown-wasi", false},
		{"x86_64-pc-linux-gnu", false},
		{"aarch64-apple-macosx14.0", false},
		{"webkit-only", false}, // has "web" but not "wasm"
		{"wasm-only", false},   // has "wasm" but not "web"
		{"", false},
	}
	for _, c := range cases {
		got := isWasmWebTarget(c.target)
		if got != c.want {
			t.Errorf("isWasmWebTarget(%q) = %v, want %v", c.target, got, c.want)
		}
	}
}

func TestEmbeddedWasmWebHarnessNonEmpty(t *testing.T) {
	if len(embeddedWasmWebHarness) == 0 {
		t.Fatal("embeddedWasmWebHarness is empty — go:embed failed?")
	}
	// Sanity-check that the harness references the imports the wasm32-web PAL
	// produces. If the harness is rewritten without these names, tests will
	// trap on instantiate.
	for _, needle := range []string{"promise_env", "_initialize"} {
		if !strings.Contains(string(embeddedWasmWebHarness), needle) {
			t.Errorf("harness does not reference %q — wasm32-web binaries will not run", needle)
		}
	}
}

func TestMaterializeWebHarnessReuses(t *testing.T) {
	// Use a temp PROMISE_HOME so we don't pollute the user's cache.
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	first, err := materializeWebHarness()
	if err != nil {
		t.Fatalf("first materialize: %v", err)
	}
	if !filepath.IsAbs(first) {
		t.Fatalf("expected absolute path, got %q", first)
	}
	stat1, err := os.Stat(first)
	if err != nil {
		t.Fatalf("stat first: %v", err)
	}
	if stat1.Size() != int64(len(embeddedWasmWebHarness)) {
		t.Fatalf("size mismatch: stat=%d embed=%d", stat1.Size(), len(embeddedWasmWebHarness))
	}

	// A second call must return the same path and not rewrite the file.
	second, err := materializeWebHarness()
	if err != nil {
		t.Fatalf("second materialize: %v", err)
	}
	if second != first {
		t.Errorf("second call returned %q, want %q (reuse)", second, first)
	}
	stat2, err := os.Stat(second)
	if err != nil {
		t.Fatalf("stat second: %v", err)
	}
	if !stat2.ModTime().Equal(stat1.ModTime()) {
		t.Errorf("modtime changed: %v -> %v (file should be reused, not rewritten)", stat1.ModTime(), stat2.ModTime())
	}

	// File must live under <PROMISE_HOME>/cache/wasm/.
	wantPrefix := filepath.Join(tmp, "cache", "wasm") + string(os.PathSeparator)
	if !strings.HasPrefix(first, wantPrefix) {
		t.Errorf("path %q does not start with %q", first, wantPrefix)
	}
	if !strings.HasSuffix(filepath.Base(first), ".js") {
		t.Errorf("expected .js suffix, got %q", first)
	}
}
