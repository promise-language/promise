package main

import (
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// locatePromiseBin is the locator for the compiler binary used by the few tests
// in package main that drive it as a subprocess. Most such tests live in
// cmd/promise/tests/... and use clitest.Bin directly; the ones here stayed
// because they also assert on unexported runner internals, which nothing
// outside package main can reach.
//
// It delegates rather than locating the binary itself: a second locator is a
// second chance to accept a compiler that predates the tree, which is the whole
// of T2137. clitest.Bin returns an absolute path and refuses a stale binary, so
// a test that runs it from a temp working directory still finds it and cannot
// silently assert on yesterday's compiler.
func locatePromiseBin(t *testing.T) string {
	t.Helper()
	return clitest.Bin(t)
}
