package blobstore

import (
	"os"
	"testing"

	"github.com/promise-language/promise/compiler/internal/hometest"
)

// NewStore roots the store at <PromiseHome>/cache, so this package's tests run
// under a private home and fail if any of them reached ~/.promise (#102) — see
// internal/hometest. A test whose subject is a home still sets its own.
func TestMain(m *testing.M) { os.Exit(hometest.Pin(m)) }
