package clitest

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TempDir is t.TempDir for a test that RUNS BINARIES out of the directory it
// gets back — a PROMISE_HOME whose llvm-view holds llc.exe, a linked compiler,
// a compiled test binary (T2157).
//
// Windows refuses to unlink the path an image was just executed from: the image
// section outlives the process by a moment, and an antivirus scanning the file
// on close holds it a moment longer. t.TempDir's cleanup unlinks once and fails
// the test on ERROR_ACCESS_DENIED, which turns that moment into a red trunk —
// and it fails a test whose body already passed, so the failure names a
// subsystem that had nothing to do with it.
//
// So cleanup here retries for a bounded while, the same answer renameWithRetry
// gives the same operating system for the same reason. The measured lag is
// milliseconds; the budget is far longer because a loaded CI box is not the
// machine the lag was measured on.
//
// A directory that survives the whole budget is reported, not fatal: the bytes
// are a temp-dir leak the OS reclaims, while the failure would be a test that
// cannot tell anyone what it actually verified. Elsewhere — every platform but
// Windows, where unlink does not work this way — a cleanup failure is as fatal
// as Go's own.
func TempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", tempDirPattern(t.Name()))
	if err != nil {
		t.Fatalf("creating a temp dir: %v", err)
	}
	t.Cleanup(func() {
		if err := removeAllRetrying(dir); err != nil {
			if runtime.GOOS != "windows" {
				t.Errorf("TempDir cleanup: %v", err)
				return
			}
			t.Logf("TempDir cleanup gave up after %s, leaving %s behind: %v",
				removeAllBudget, dir, err)
		}
	})
	return dir
}

// removeAllBudget bounds the retry loop below. removeAllStep is the pause
// between attempts, and removeAllRetries is whether to retry at all — only
// Windows unlinks this way, so anywhere else the first error is the answer and
// retrying it just delays the report. All three are package-level so a test can
// drive the loop without sleeping through the whole budget.
var (
	removeAllBudget  = 10 * time.Second
	removeAllStep    = 50 * time.Millisecond
	removeAllRetries = runtime.GOOS == "windows"
)

// removeAllRetrying is os.RemoveAll, retried while Windows still holds a file
// open. Each pass removes everything it can, so a directory with one busy
// executable in it empties on the first pass and the retries are only about
// that one file.
func removeAllRetrying(dir string) error {
	return removeAllRetryingWith(os.RemoveAll, dir)
}

// removeAllRetryingWith is the testable core of removeAllRetrying with the
// removal injected, so the give-up path — which needs a directory nothing will
// ever release — can be exercised without arranging one. (Factored for the same
// reason as renameRetrying in the compiler's stub_embed.go.)
func removeAllRetryingWith(removeAll func(string) error, dir string) error {
	deadline := time.Now().Add(removeAllBudget)
	for {
		err := removeAll(dir)
		if err == nil {
			return nil
		}
		if !removeAllRetries || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(removeAllStep)
	}
}

// tempDirPattern turns a test name into an os.MkdirTemp pattern, so a directory
// left behind names the test that left it. Subtest names carry '/' and a test
// name can carry anything a Go identifier or a table-driven case name can, so
// everything but the safe set becomes '_'.
func tempDirPattern(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	trimmed := strings.Trim(b.String(), "_")
	if trimmed == "" {
		trimmed = "clitest"
	}
	// Keep the name well clear of MAX_PATH: what goes under this directory is a
	// Promise home, and those paths are deep (T2125).
	if len(trimmed) > 48 {
		trimmed = trimmed[:48]
	}
	return trimmed + "-*"
}
