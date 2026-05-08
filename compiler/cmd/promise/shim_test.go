package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShimEnvAddsNoShim(t *testing.T) {
	t.Setenv("PROMISE_NO_SHIM", "")
	env := shimEnv()
	found := false
	for _, e := range env {
		if e == "PROMISE_NO_SHIM=1" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected PROMISE_NO_SHIM=1 in env")
	}
}

func TestShimEnvReplacesExisting(t *testing.T) {
	t.Setenv("PROMISE_NO_SHIM", "0")
	env := shimEnv()
	count := 0
	for _, e := range env {
		if strings.HasPrefix(e, "PROMISE_NO_SHIM=") {
			count++
			if e != "PROMISE_NO_SHIM=1" {
				t.Fatalf("expected PROMISE_NO_SHIM=1, got %s", e)
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 PROMISE_NO_SHIM entry, got %d", count)
	}
}

func TestShimExcludedCommands(t *testing.T) {
	for _, cmd := range []string{"install", "sync", "epochs", "use", "init", "remove"} {
		if !shimExcludedCommands[cmd] {
			t.Errorf("expected %q to be excluded from shim dispatch", cmd)
		}
	}
	for _, cmd := range []string{"build", "run", "test", "exec", "format"} {
		if shimExcludedCommands[cmd] {
			t.Errorf("expected %q to NOT be excluded from shim dispatch", cmd)
		}
	}
}

func TestResolveDesiredEpochEnvOverride(t *testing.T) {
	t.Setenv("PROMISE_EPOCH", "custom-epoch")
	t.Setenv("PROMISE_HOME", t.TempDir())
	epoch := resolveDesiredEpoch()
	if epoch != "custom-epoch" {
		t.Fatalf("expected custom-epoch, got %s", epoch)
	}
}

func TestResolveDesiredEpochFromActive(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_EPOCH", "")
	t.Setenv("PROMISE_HOME", tmp)

	// Write active epoch file.
	if err := os.WriteFile(filepath.Join(tmp, "active"), []byte("2026.3\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Run from a directory with no promise.toml.
	origDir, _ := os.Getwd()
	noConfigDir := t.TempDir()
	os.Chdir(noConfigDir)
	defer os.Chdir(origDir)

	epoch := resolveDesiredEpoch()
	if epoch != "2026.3" {
		t.Fatalf("expected 2026.3, got %s", epoch)
	}
}

func TestResolveDesiredEpochFromConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_EPOCH", "")
	t.Setenv("PROMISE_HOME", tmp)

	// Create a promise.toml with an epoch.
	projectDir := t.TempDir()
	toml := `[module]
name = "test"
epoch = "2025.1"
`
	if err := os.WriteFile(filepath.Join(projectDir, "promise.toml"), []byte(toml), 0644); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	os.Chdir(projectDir)
	defer os.Chdir(origDir)

	epoch := resolveDesiredEpoch()
	if epoch != "2025.1" {
		t.Fatalf("expected 2025.1, got %s", epoch)
	}
}

func TestResolveDesiredEpochAbsolutePath(t *testing.T) {
	// When PROMISE_EPOCH is an absolute path, resolveDesiredEpoch returns it as-is.
	absPath := filepath.Join(t.TempDir(), "bin", "promise")
	t.Setenv("PROMISE_EPOCH", absPath)
	t.Setenv("PROMISE_HOME", t.TempDir())

	epoch := resolveDesiredEpoch()
	if epoch != absPath {
		t.Fatalf("expected %s, got %s", absPath, epoch)
	}
}

func TestResolveDesiredEpochDevEpoch(t *testing.T) {
	// "dev" is a valid epoch name (not a path).
	t.Setenv("PROMISE_EPOCH", "dev")
	t.Setenv("PROMISE_HOME", t.TempDir())

	epoch := resolveDesiredEpoch()
	if epoch != "dev" {
		t.Fatalf("expected dev, got %s", epoch)
	}
}

func TestHasShimMarkerAtPresent(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".promise.shim"), []byte("shim\n"), 0644)
	if !hasShimMarkerAt(dir) {
		t.Fatal("expected hasShimMarkerAt to return true when marker exists")
	}
}

func TestHasShimMarkerAtAbsent(t *testing.T) {
	dir := t.TempDir()
	if hasShimMarkerAt(dir) {
		t.Fatal("expected hasShimMarkerAt to return false when no marker exists")
	}
}

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
	version = "2026.3-abc1234"
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
	if output != "promise version 2026.3-abc1234\n" {
		t.Fatalf("expected 'promise version 2026.3-abc1234\\n', got %q", output)
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
