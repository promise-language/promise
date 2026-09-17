package clitest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestTempDirIsRemovedWhenTheTestEnds pins the part of t.TempDir that TempDir
// keeps: the directory exists during the test and is gone after it.
func TestTempDirIsRemovedWhenTheTestEnds(t *testing.T) {
	var dir string
	t.Run("inner", func(t *testing.T) {
		dir = TempDir(t)
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("TempDir did not produce a directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
			t.Fatalf("writing into the temp dir: %v", err)
		}
	})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("temp dir outlived its test: %v", err)
	}
}

// TestTempDirNamesTheTestThatMadeIt: a directory that survives cleanup is
// reported by name and left on disk, so the name has to say whose it was.
func TestTempDirNamesTheTestThatMadeIt(t *testing.T) {
	dir := TempDir(t)
	if base := filepath.Base(dir); !strings.HasPrefix(base, "TestTempDirNamesTheTestThatMadeIt-") {
		t.Errorf("temp dir %q does not name this test", base)
	}
}

func TestTempDirPatternSanitizes(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"TestFoo", "TestFoo-*"},
		{"TestFoo/sub_case", "TestFoo_sub_case-*"},
		{"TestFoo/with spaces and:colons", "TestFoo_with_spaces_and_colons-*"},
		{"///", "clitest-*"},
		{"", "clitest-*"},
		{strings.Repeat("N", 80), strings.Repeat("N", 48) + "-*"},
	} {
		if got := tempDirPattern(tc.name); got != tc.want {
			t.Errorf("tempDirPattern(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestRemoveAllRetryingOutlastsARunningExecutable is the whole reason TempDir
// exists (T2157): Windows refuses to unlink the path an image is executing
// from, so the one unlink t.TempDir does fails a test whose body already passed.
//
// The test starts a process from the directory, so the first plain RemoveAll
// must fail — that failure IS the mechanism, and asserting it keeps this test
// honest if a future Windows stops behaving this way. The process is then killed
// while removeAllRetrying is looping, so what the retry waits out is a real
// image section being released, not a sleep.
func TestRemoveAllRetryingOutlastsARunningExecutable(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows refuses to unlink a running image")
	}
	dir := TempDir(t)
	exe := filepath.Join(dir, "busy.exe")
	// ping -t runs until killed, and every Windows install has it.
	src := filepath.Join(os.Getenv("SystemRoot"), "System32", "ping.exe")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("no ping.exe to borrow: %v", err)
	}
	if err := os.WriteFile(exe, data, 0o755); err != nil {
		t.Fatalf("placing the busy executable: %v", err)
	}

	cmd := exec.Command(exe, "-t", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the busy executable: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	if err := os.RemoveAll(dir); err == nil {
		t.Skip("this Windows unlinks a running image; the retry has nothing to wait out")
	}

	killed := make(chan error, 1)
	go func() {
		// Long enough that removeAllRetrying is demonstrably looping, short
		// enough to stay far inside its budget.
		time.Sleep(4 * removeAllStep)
		killed <- cmd.Process.Kill()
	}()

	start := time.Now()
	if err := removeAllRetrying(dir); err != nil {
		t.Fatalf("removeAllRetrying gave up after %s: %v", time.Since(start), err)
	}
	if err := <-killed; err != nil {
		t.Fatalf("killing the busy executable: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("directory still there after a successful remove: %v", err)
	}
}

// TestRemoveAllRetryingReportsWhatItCannotRemove: the budget is bounded, so a
// directory nothing will ever release comes back as an error rather than
// hanging the run — and it is tried more than once on the way there.
func TestRemoveAllRetryingReportsWhatItCannotRemove(t *testing.T) {
	defer restoreRemoveAllKnobs(t)()
	removeAllBudget, removeAllStep, removeAllRetries = 30*time.Millisecond, time.Millisecond, true

	stuck := errors.New("Access is denied.")
	attempts := 0
	err := removeAllRetryingWith(func(string) error { attempts++; return stuck }, "dir")
	if !errors.Is(err, stuck) {
		t.Errorf("error = %v, want the removal's own error", err)
	}
	if attempts < 2 {
		t.Errorf("gave up after %d attempt(s); the budget buys retries", attempts)
	}
}

// TestRemoveAllRetryingDoesNotRetryOffWindows: no other platform holds an
// unlinked-but-running image, so a failure there is real and waiting on it only
// delays the report.
func TestRemoveAllRetryingDoesNotRetryOffWindows(t *testing.T) {
	defer restoreRemoveAllKnobs(t)()
	removeAllBudget, removeAllStep, removeAllRetries = time.Minute, time.Millisecond, false

	attempts := 0
	err := removeAllRetryingWith(func(string) error { attempts++; return errors.New("nope") }, "dir")
	if err == nil {
		t.Fatal("want the removal's error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want exactly 1", attempts)
	}
}

func restoreRemoveAllKnobs(t *testing.T) func() {
	t.Helper()
	budget, step, retries := removeAllBudget, removeAllStep, removeAllRetries
	return func() { removeAllBudget, removeAllStep, removeAllRetries = budget, step, retries }
}
