package module

import (
	"os"
	"testing"

	"github.com/promise-language/promise/compiler/internal/hometest"
)

// This package resolves the Promise home (PromiseHome, CacheRoot), so its
// tests run under a private one and fail if any of them reached ~/.promise
// (#102). Under a bare `go test` with PROMISE_HOME unset they otherwise
// resolved the machine-global home — and one test that unset the variable
// without restoring it did the same for every test after it, even under
// bin/test. A test whose subject is a home still sets its own.
func TestMain(m *testing.M) { os.Exit(hometest.Pin(m)) }

// TestPackageRunsUnderItsPrivateHome: the home this package's code resolves is
// the one TestMain pinned. Dropping the TestMain reddens this in every
// environment, not only under a bare `go test` that happens to reach ~/.promise.
func TestPackageRunsUnderItsPrivateHome(t *testing.T) {
	home, err := PromiseHome()
	if err != nil {
		t.Fatal(err)
	}
	if pinned := hometest.Home(); pinned == "" || home != pinned {
		t.Errorf("PromiseHome() = %q, want the private home TestMain pinned (%q)", home, pinned)
	}
}
