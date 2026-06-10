package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestReadInstalledStubVersionMissing(t *testing.T) {
	dir := t.TempDir()
	// No sidecar present → version 0 (so a fresh install always forward-updates).
	if v := readInstalledStubVersion(dir); v != 0 {
		t.Fatalf("expected 0 for missing sidecar, got %d", v)
	}
}

func TestReadInstalledStubVersionValid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stubVersionSidecar), []byte("7\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if v := readInstalledStubVersion(dir); v != 7 {
		t.Fatalf("expected 7, got %d", v)
	}
}

func TestReadInstalledStubVersionGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stubVersionSidecar), []byte("not-a-number"), 0644); err != nil {
		t.Fatal(err)
	}
	// Unparseable sidecar → 0, never panics, never executes the stub.
	if v := readInstalledStubVersion(dir); v != 0 {
		t.Fatalf("expected 0 for garbage sidecar, got %d", v)
	}
}

// TestForwardOnlyDecision exercises the version comparison that gates a stub
// update: the installer replaces the stub only when its embedded version is
// strictly newer than the installed sidecar value (never downgrades).
func TestForwardOnlyDecision(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		installed string // sidecar contents ("" = absent)
		embedded  int
		replace   bool
	}{
		{"", 1, true},    // fresh install
		{"1", 2, true},   // newer embedded → replace
		{"2", 2, false},  // equal → keep
		{"3", 2, false},  // installed newer → never downgrade
		{"bad", 1, true}, // unreadable installed treated as 0 → replace
	}
	for _, c := range cases {
		if c.installed == "" {
			os.Remove(filepath.Join(dir, stubVersionSidecar))
		} else {
			if err := os.WriteFile(filepath.Join(dir, stubVersionSidecar), []byte(c.installed), 0644); err != nil {
				t.Fatal(err)
			}
		}
		got := c.embedded > readInstalledStubVersion(dir)
		if got != c.replace {
			t.Errorf("installed=%q embedded=%d: expected replace=%v, got %v", c.installed, c.embedded, c.replace, got)
		}
	}
}

// TestReadEmbeddedStubDevBuild: dev builds (no embed_stub tag) carry no stub,
// so readEmbeddedStub reports a clear error rather than panicking or returning
// empty bytes. Release builds (T0773) supply the per-target binary.
func TestReadEmbeddedStubDevBuild(t *testing.T) {
	if hasEmbeddedStub {
		t.Skip("build has an embedded stub; this guards the dev-build path")
	}
	_, err := readEmbeddedStub("promise")
	if err == nil {
		t.Fatal("expected an error reading an embedded stub in a dev build")
	}
	if !strings.Contains(err.Error(), "no embedded stub") {
		t.Fatalf("expected 'no embedded stub' error, got: %v", err)
	}
}

// TestWriteStubAndSidecarDevBuild: with no embedded stub, writeStubAndSidecar
// fails (because readEmbeddedStub fails) and must NOT leave a stub binary or a
// sidecar behind — a half-written launcher would be worse than none.
func TestWriteStubAndSidecarDevBuild(t *testing.T) {
	if hasEmbeddedStub {
		t.Skip("build has an embedded stub; this guards the dev-build path")
	}
	dir := t.TempDir()
	if err := writeStubAndSidecar(dir, "promise"); err == nil {
		t.Fatal("expected writeStubAndSidecar to fail without an embedded stub")
	}
	if _, err := os.Stat(filepath.Join(dir, "promise")); !os.IsNotExist(err) {
		t.Error("no stub binary should be written when the embedded stub is absent")
	}
	if _, err := os.Stat(filepath.Join(dir, stubVersionSidecar)); !os.IsNotExist(err) {
		t.Error("no sidecar should be written when the embedded stub is absent")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "thing")
	if err := writeFileAtomic(path, []byte("hello"), 0755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("expected 'hello', got %q", string(data))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows does not model Unix permission bits — os.Stat reports 0666/0444
	// based solely on the read-only attribute, so skip the exact-mode check there.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0755 {
		t.Fatalf("expected mode 0755, got %v", info.Mode().Perm())
	}
	// Overwrite atomically — no leftover temp files in the dir.
	if err := writeFileAtomic(path, []byte("world"), 0644); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "world" {
		t.Fatalf("expected 'world', got %q", string(data))
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 file (no temp leftovers), got %d", len(entries))
	}
}

// TestWriteFileAtomicBadDir: when the destination directory does not exist,
// os.CreateTemp fails and writeFileAtomic returns that error (rather than
// panicking or silently succeeding) and writes nothing. writeStubAndSidecar
// relies on this error being propagated so a failed stub install aborts cleanly.
func TestWriteFileAtomicBadDir(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist", "thing")
	if err := writeFileAtomic(missing, []byte("data"), 0644); err == nil {
		t.Fatal("expected an error writing into a nonexistent directory")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("no file should be created when the directory is missing")
	}
}

// TestRenameWithRetryNonRetryableFailsFast: a non-retryable rename error (here a
// nonexistent source, which is real on every platform) must short-circuit
// immediately rather than spin through the full backoff budget (~0.55s). This
// proves the retry loop only burns wall-clock on transient Windows lock errors.
func TestRenameWithRetryNonRetryableFailsFast(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "does-not-exist")
	dst := filepath.Join(dir, "dst")
	start := time.Now()
	err := renameWithRetry(src, dst)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error renaming a nonexistent source")
	}
	if elapsed >= 50*time.Millisecond {
		t.Fatalf("non-retryable error should fail fast, took %v", elapsed)
	}
}

// errRetryable is a stand-in for the transient Windows sharing/lock errors that
// isRetryableRenameError matches; the predicate below treats only it as retryable.
var errRetryable = errors.New("sharing violation")

func retryablePredicate(err error) bool { return errors.Is(err, errRetryable) }
func noBackoff(int) time.Duration       { return 0 }

// TestRenameRetryingSucceedsAfterTransient: the loop keeps retrying while the
// rename returns a retryable error and succeeds once the (simulated) lock clears.
// This is the Windows happy path that the T0793 fix targets, exercised on any OS.
func TestRenameRetryingSucceedsAfterTransient(t *testing.T) {
	calls := 0
	rename := func(src, dst string) error {
		calls++
		if calls < 4 { // fail the first 3, succeed on the 4th
			return errRetryable
		}
		return nil
	}
	if err := renameRetrying(rename, retryablePredicate, noBackoff, "s", "d"); err != nil {
		t.Fatalf("expected success after transient errors, got %v", err)
	}
	if calls != 4 {
		t.Fatalf("expected 4 rename attempts, got %d", calls)
	}
}

// TestRenameRetryingExhausts: when every attempt returns a retryable error, the
// loop gives up after exactly renameAttempts tries and returns the last error —
// it never spins forever and never swallows the failure.
func TestRenameRetryingExhausts(t *testing.T) {
	calls := 0
	rename := func(src, dst string) error {
		calls++
		return errRetryable
	}
	err := renameRetrying(rename, retryablePredicate, noBackoff, "s", "d")
	if !errors.Is(err, errRetryable) {
		t.Fatalf("expected the last retryable error, got %v", err)
	}
	if calls != renameAttempts {
		t.Fatalf("expected exactly %d attempts on exhaustion, got %d", renameAttempts, calls)
	}
}

// TestRenameRetryingNonRetryable: a non-retryable error short-circuits after a
// single attempt — no retries, the error propagates verbatim.
func TestRenameRetryingNonRetryable(t *testing.T) {
	fatal := errors.New("no such file")
	calls := 0
	rename := func(src, dst string) error {
		calls++
		return fatal
	}
	err := renameRetrying(rename, retryablePredicate, noBackoff, "s", "d")
	if !errors.Is(err, fatal) {
		t.Fatalf("expected the non-retryable error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 attempt for a non-retryable error, got %d", calls)
	}
}

// TestRenameRetryingFirstTry: the common case — rename succeeds immediately, so
// the loop returns nil without consulting the retryable predicate or backoff.
func TestRenameRetryingFirstTry(t *testing.T) {
	calls := 0
	rename := func(src, dst string) error { calls++; return nil }
	if err := renameRetrying(rename, retryablePredicate, noBackoff, "s", "d"); err != nil {
		t.Fatalf("expected immediate success, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 attempt, got %d", calls)
	}
}

// TestCopyFileContentAndPerm: copyFile reproduces the source bytes, applies the
// requested permissions, and leaves no temp files behind in the destination dir.
func TestCopyFileContentAndPerm(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("payload"), 0644); err != nil {
		t.Fatal(err)
	}
	copyFile(src, dst, 0755)
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload" {
		t.Fatalf("expected 'payload', got %q", string(data))
	}
	// Windows does not model Unix permission bits (see TestWriteFileAtomic).
	if runtime.GOOS != "windows" {
		info, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0755 {
			t.Fatalf("expected mode 0755, got %v", info.Mode().Perm())
		}
	}
	// No leftover temp files: just src and dst remain.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("expected exactly 2 files (src, dst; no temp leftovers), got %d", len(entries))
	}
}
