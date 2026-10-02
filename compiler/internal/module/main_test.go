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
