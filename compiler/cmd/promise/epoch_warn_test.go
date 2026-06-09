package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/module"
)

// captureWarnStderr runs warnEpochMismatch in dir and returns whatever it wrote
// to stderr.
func captureWarnStderr(t *testing.T, dir string) string {
	t.Helper()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	r, w, _ := os.Pipe()
	oldStderr := os.Stderr
	os.Stderr = w
	warnEpochMismatch()
	w.Close()
	os.Stderr = oldStderr

	var buf [1024]byte
	n, _ := r.Read(buf[:])
	return string(buf[:n])
}

func writeEpochToml(t *testing.T, dir, epoch string) {
	t.Helper()
	toml := "[module]\nname = \"t\"\nepoch = \"" + epoch + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"), []byte(toml), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestWarnEpochMismatchFires(t *testing.T) {
	myEpoch, err := module.CompilerEpoch(embeddedCatalog)
	if err != nil {
		t.Skip("no embedded catalog epoch")
	}
	dir := t.TempDir()
	writeEpochToml(t, dir, myEpoch+"-different")
	t.Setenv("PROMISE_NO_EPOCH_WARN", "")
	t.Setenv("PROMISE_EPOCH", "")

	out := captureWarnStderr(t, dir)
	if !strings.Contains(out, "warning:") || !strings.Contains(out, myEpoch+"-different") {
		t.Fatalf("expected mismatch warning mentioning project epoch, got: %q", out)
	}
}

func TestWarnEpochMismatchSameEpoch(t *testing.T) {
	myEpoch, err := module.CompilerEpoch(embeddedCatalog)
	if err != nil {
		t.Skip("no embedded catalog epoch")
	}
	dir := t.TempDir()
	writeEpochToml(t, dir, myEpoch)
	t.Setenv("PROMISE_NO_EPOCH_WARN", "")
	t.Setenv("PROMISE_EPOCH", "")

	if out := captureWarnStderr(t, dir); out != "" {
		t.Fatalf("expected no warning when epochs match, got: %q", out)
	}
}

func TestWarnEpochMismatchNoConfig(t *testing.T) {
	dir := t.TempDir() // no promise.toml
	t.Setenv("PROMISE_NO_EPOCH_WARN", "")
	t.Setenv("PROMISE_EPOCH", "")
	if out := captureWarnStderr(t, dir); out != "" {
		t.Fatalf("expected no warning without a project config, got: %q", out)
	}
}

func TestWarnEpochMismatchNoEpochKey(t *testing.T) {
	// A promise.toml without an [module].epoch key pins no epoch, so there is
	// nothing to mismatch against — no warning.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"t\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROMISE_NO_EPOCH_WARN", "")
	t.Setenv("PROMISE_EPOCH", "")
	if out := captureWarnStderr(t, dir); out != "" {
		t.Fatalf("expected no warning when promise.toml pins no epoch, got: %q", out)
	}
}

func TestWarnEpochMismatchSuppressedByPromiseEpoch(t *testing.T) {
	myEpoch, err := module.CompilerEpoch(embeddedCatalog)
	if err != nil {
		t.Skip("no embedded catalog epoch")
	}
	dir := t.TempDir()
	writeEpochToml(t, dir, myEpoch+"-different")
	// PROMISE_EPOCH set means the launcher already resolved the epoch and
	// exec-replaced into this binary — suppress the warning.
	t.Setenv("PROMISE_EPOCH", myEpoch+"-different")
	t.Setenv("PROMISE_NO_EPOCH_WARN", "")
	if out := captureWarnStderr(t, dir); out != "" {
		t.Fatalf("expected suppression when PROMISE_EPOCH set, got: %q", out)
	}
}

func TestWarnEpochMismatchSuppressedByOptOut(t *testing.T) {
	myEpoch, err := module.CompilerEpoch(embeddedCatalog)
	if err != nil {
		t.Skip("no embedded catalog epoch")
	}
	dir := t.TempDir()
	writeEpochToml(t, dir, myEpoch+"-different")
	t.Setenv("PROMISE_EPOCH", "")
	t.Setenv("PROMISE_NO_EPOCH_WARN", "1")
	if out := captureWarnStderr(t, dir); out != "" {
		t.Fatalf("expected suppression with PROMISE_NO_EPOCH_WARN, got: %q", out)
	}
}

func TestWarnEpochCommandsSet(t *testing.T) {
	for _, cmd := range []string{"build", "run", "test", "check", "emit-ir", "exec", "doc"} {
		if !warnEpochCommands[cmd] {
			t.Errorf("expected %q to be a warn-eligible command", cmd)
		}
	}
	for _, cmd := range []string{"install", "use", "epochs", "remove", "update", "pkg", "version"} {
		if warnEpochCommands[cmd] {
			t.Errorf("expected %q NOT to be warn-eligible", cmd)
		}
	}
}
