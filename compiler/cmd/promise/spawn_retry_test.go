package main

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestIsTransientSpawnError verifies the classifier only treats Windows
// commit-limit / resource CreateProcess failures as retryable, and never
// misclassifies POSIX errno collisions (errno 8 == ENOEXEC) or genuine
// tool errors (T1249).
func TestIsTransientSpawnError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		goos string
		want bool
	}{
		{"nil", nil, "windows", false},
		{"errno8-windows", syscall.Errno(8), "windows", true},
		{"errno1450-windows", syscall.Errno(1450), "windows", true},
		{"errno1455-windows", syscall.Errno(1455), "windows", true},
		{
			"errno8-wrapped-execError-windows",
			&exec.Error{Name: "opt.exe", Err: syscall.Errno(8)},
			"windows", true,
		},
		{
			"errno8-wrapped-pathError-windows",
			&os.PathError{Op: "fork/exec", Path: "opt.exe", Err: syscall.Errno(8)},
			"windows", true,
		},
		{
			"message-not-enough-memory-windows",
			errors.New("fork/exec opt.exe: Not enough memory resources are available to process this command."),
			"windows", true,
		},
		{
			"message-insufficient-resources-windows",
			errors.New("CreateProcess: Insufficient system resources exist to complete the requested service."),
			"windows", true,
		},
		{
			"message-paging-file-windows",
			errors.New("The paging file is too small for this operation to complete."),
			"windows", true,
		},
		// POSIX: errno 8 is ENOEXEC — must NOT be retried on non-Windows.
		{"errno8-linux", syscall.Errno(8), "linux", false},
		{"errno8-darwin", syscall.Errno(8), "darwin", false},
		// Genuine tool failures must never be retried.
		{"tool-error-windows", errors.New("opt: bad IR: expected type"), "windows", false},
		{"exit-status-windows", errors.New("exit status 1"), "windows", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientSpawnErrorOn(tc.err, tc.goos); got != tc.want {
				t.Fatalf("isTransientSpawnErrorOn(%v, %q) = %v, want %v", tc.err, tc.goos, got, tc.want)
			}
		})
	}
}

// TestRunResilientNonTransient verifies runResilient returns a non-transient
// spawn failure immediately without spurious retries or hangs: a builder that
// points at a non-existent tool fails to spawn, and on non-Windows hosts that
// error is never classified as transient, so the call returns promptly.
func TestRunResilientNonTransient(t *testing.T) {
	calls := 0
	err := runResilient(func() *exec.Cmd {
		calls++
		return exec.Command("promise-nonexistent-tool-xyz")
	})
	if err == nil {
		t.Fatal("expected error running a non-existent tool, got nil")
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 attempt for a non-transient failure, got %d", calls)
	}
}

// successCmd / failCmd return trivially-succeeding / -failing commands portably,
// so runResilient's control flow can be exercised without depending on LLVM tools.
func successCmd() *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/c", "exit")
	}
	return exec.Command("true")
}

func failCmd() *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/c", "exit 1")
	}
	return exec.Command("false")
}

// useFastRetry swaps in a tiny backoff so retry-exhaustion tests don't sleep for
// seconds, restoring the production value afterwards.
func useFastRetry(t *testing.T) {
	t.Helper()
	saved := spawnRetryBackoff
	spawnRetryBackoff = time.Millisecond
	t.Cleanup(func() { spawnRetryBackoff = saved })
}

// injectClassifier overrides the transient-spawn classifier for the duration of a
// test, so the retry loop can be driven on any host (the real classifier is
// Windows-gated). It restores the production classifier on cleanup.
func injectClassifier(t *testing.T, f func(error) bool) {
	t.Helper()
	saved := isTransientSpawn
	isTransientSpawn = f
	t.Cleanup(func() { isTransientSpawn = saved })
}

// TestRunResilientSuccess verifies the happy path: a builder whose command
// succeeds returns nil after exactly one attempt, without consulting the
// classifier (err == nil short-circuits).
func TestRunResilientSuccess(t *testing.T) {
	injectClassifier(t, func(error) bool {
		t.Fatal("classifier must not be consulted on success")
		return false
	})
	calls := 0
	err := runResilient(func() *exec.Cmd {
		calls++
		return successCmd()
	})
	if err != nil {
		t.Fatalf("expected nil error on success, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 attempt on success, got %d", calls)
	}
}

// TestRunResilientRetriesThenSucceeds verifies a transient failure is retried and
// the eventual success is returned: the first two attempts fail (classified
// transient), the third succeeds. A fresh *exec.Cmd is built each attempt.
func TestRunResilientRetriesThenSucceeds(t *testing.T) {
	useFastRetry(t)
	injectClassifier(t, func(error) bool { return true }) // treat any failure as transient
	calls := 0
	err := runResilient(func() *exec.Cmd {
		calls++
		if calls < 3 {
			return failCmd()
		}
		return successCmd()
	})
	if err != nil {
		t.Fatalf("expected nil error after retry, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts (2 transient failures + 1 success), got %d", calls)
	}
}

// TestRunResilientExhaustsRetries verifies that a persistently transient failure
// is retried the full 6 attempts and the last error is returned (no infinite
// loop, no swallowing the error).
func TestRunResilientExhaustsRetries(t *testing.T) {
	useFastRetry(t)
	injectClassifier(t, func(error) bool { return true })
	calls := 0
	err := runResilient(func() *exec.Cmd {
		calls++
		return failCmd()
	})
	if err == nil {
		t.Fatal("expected the final transient error to be returned, got nil")
	}
	if calls != 6 {
		t.Fatalf("expected 6 attempts before giving up, got %d", calls)
	}
}

// TestIsTransientSpawnErrorHostGating verifies the exported wrapper delegates to
// the current host's GOOS: on non-Windows hosts a Windows-only errno must not be
// classified transient, guarding against the POSIX errno-8 (ENOEXEC) collision.
func TestIsTransientSpawnErrorHostGating(t *testing.T) {
	got := isTransientSpawnError(syscall.Errno(8))
	want := runtime.GOOS == "windows"
	if got != want {
		t.Fatalf("isTransientSpawnError(errno 8) = %v on %s, want %v", got, runtime.GOOS, want)
	}
}
