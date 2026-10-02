package blobstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/promise-language/promise/compiler/internal/hometest"
)

// NewStore roots the store at <PromiseHome>/cache, so this package's tests run
// under a private home and fail if any of them reached ~/.promise (#102) — see
// internal/hometest. A test whose subject is a home still sets its own.
func TestMain(m *testing.M) { os.Exit(hometest.Pin(m)) }

// TestStoreRootsUnderThePrivateHome: the store NewStore opens is rooted in the
// home TestMain pinned. Dropping the TestMain reddens this in every
// environment, not only under a bare `go test` that happens to reach ~/.promise.
func TestStoreRootsUnderThePrivateHome(t *testing.T) {
	s, err := NewStore()
	if err != nil {
		t.Fatal(err)
	}
	pinned := hometest.Home()
	if want := filepath.Join(pinned, "cache"); pinned == "" || s.Root() != want {
		t.Errorf("NewStore().Root() = %q, want %q under the private home TestMain pinned", s.Root(), want)
	}
}
