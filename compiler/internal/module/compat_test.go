package module

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCompatVerdictRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	v := &CompatVerdict{
		URL:        "https://github.com/you/foo.git",
		Commit:     "abc123",
		Epoch:      "2026.1",
		Compatible: true,
	}
	if err := SaveCompat(v); err != nil {
		t.Fatalf("SaveCompat: %v", err)
	}
	if v.CompilerHash == "" {
		t.Error("SaveCompat should stamp CompilerHash")
	}

	// URL normalization means a differently-spelled but equivalent URL hits the
	// same verdict.
	got, found := LookupCompat("github.com/you/foo", "abc123", "2026.1")
	if !found {
		t.Fatal("expected verdict to be found")
	}
	if !got.Compatible {
		t.Error("expected Compatible=true")
	}

	// A different epoch is a separate key — not found.
	if _, found := LookupCompat("github.com/you/foo", "abc123", "2026.2"); found {
		t.Error("verdict should be keyed by epoch")
	}
}

func TestLookupCompatCorruptJSON(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// A verdict file whose contents are not valid JSON must be treated as absent
	// (a corrupt cache entry never wedges resolution — "verify, never assume").
	dir := filepath.Join(tmp, "compat")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	url, commit, epoch := "github.com/you/corrupt", "cafef00d", "2026.1"
	path := filepath.Join(dir, compatKey(url, commit, epoch)+".json")
	if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, found := LookupCompat(url, commit, epoch); found {
		t.Error("corrupt verdict file should be treated as absent")
	}
}

// TestCompatVerdictCompileOnlyRoundTrip verifies the CompileOnly field (added by
// T1052) survives a SaveCompat/LookupCompat round-trip and that an incompatible
// verdict with no CompileOnly stays false (the zero value must be stable).
func TestCompatVerdictCompileOnlyRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	v := &CompatVerdict{
		URL:         "https://github.com/you/notest.git",
		Commit:      "aaabbb",
		Epoch:       "2026.1",
		Compatible:  true,
		CompileOnly: true,
		FailReason:  "module has no *_test.pr files — verified by compilation only",
	}
	if err := SaveCompat(v); err != nil {
		t.Fatalf("SaveCompat: %v", err)
	}

	got, found := LookupCompat("https://github.com/you/notest.git", "aaabbb", "2026.1")
	if !found {
		t.Fatal("expected verdict to be found")
	}
	if !got.Compatible {
		t.Error("expected Compatible=true")
	}
	if !got.CompileOnly {
		t.Error("expected CompileOnly=true to survive round-trip")
	}
	if got.FailReason == "" {
		t.Error("expected FailReason to carry the advisory message")
	}

	// A regular (test-verified) compatible verdict must NOT have CompileOnly set.
	v2 := &CompatVerdict{
		URL:        "https://github.com/you/tested.git",
		Commit:     "cccddd",
		Epoch:      "2026.1",
		Compatible: true,
	}
	if err := SaveCompat(v2); err != nil {
		t.Fatalf("SaveCompat v2: %v", err)
	}
	got2, found2 := LookupCompat("https://github.com/you/tested.git", "cccddd", "2026.1")
	if !found2 {
		t.Fatal("expected v2 to be found")
	}
	if got2.CompileOnly {
		t.Error("a test-verified verdict must not have CompileOnly set")
	}
}

// TestCompatVerdictBackwardCompatOldJSON verifies that a verdict JSON written
// without the compile_only field (omitempty zero value) is read back with
// CompileOnly=false, preserving backward compatibility with cached verdicts from
// before T1052 was merged.
func TestCompatVerdictBackwardCompatOldJSON(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// Hand-write a JSON verdict that lacks the compile_only key entirely
	// (simulating a pre-T1052 cache entry).
	dir := filepath.Join(tmp, "compat")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	url, commit, epoch := "github.com/you/oldcache", "f00df00d", "2026.1"
	oldJSON := `{
  "url": "github.com/you/oldcache",
  "commit": "f00df00d",
  "epoch": "2026.1",
  "compatible": true,
  "compiler_hash": "` + CompilerHash() + `"
}`
	path := filepath.Join(dir, compatKey(url, commit, epoch)+".json")
	if err := os.WriteFile(path, []byte(oldJSON), 0644); err != nil {
		t.Fatal(err)
	}

	got, found := LookupCompat(url, commit, epoch)
	if !found {
		t.Fatal("pre-T1052 verdict should still be found")
	}
	if !got.Compatible {
		t.Error("expected Compatible=true from old cache entry")
	}
	if got.CompileOnly {
		t.Error("old JSON without compile_only must deserialize as CompileOnly=false")
	}
}

func TestCompatVerdictCompilerHashInvalidation(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// Hand-write a verdict file with a stale compiler hash.
	dir := filepath.Join(tmp, "compat")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	stale := CompatVerdict{
		URL:          "github.com/you/bar",
		Commit:       "deadbeef",
		Epoch:        "2026.1",
		Compatible:   true,
		CompilerHash: "STALE-HASH-FROM-AN-OLDER-COMPILER",
	}
	data, _ := json.MarshalIndent(stale, "", "  ")
	path := filepath.Join(dir, compatKey(stale.URL, stale.Commit, stale.Epoch)+".json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	// A verdict from a different compiler build must be treated as absent — a
	// rebuilt compiler can flip the verdict, so "verify, never assume" re-runs.
	if _, found := LookupCompat(stale.URL, stale.Commit, stale.Epoch); found {
		t.Error("stale-compiler verdict should be ignored")
	}

	// Saving under the current compiler makes it visible again.
	if err := SaveCompat(&CompatVerdict{URL: stale.URL, Commit: stale.Commit, Epoch: stale.Epoch, Compatible: false, FailReason: "boom"}); err != nil {
		t.Fatal(err)
	}
	got, found := LookupCompat(stale.URL, stale.Commit, stale.Epoch)
	if !found {
		t.Fatal("expected fresh verdict to be found")
	}
	if got.Compatible || got.FailReason != "boom" {
		t.Errorf("unexpected verdict: %+v", got)
	}
}
