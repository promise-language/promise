package main

import (
	"os"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// This package runs under the worktree's .promise-home, like every per-area
// package under tests/ (T2133, T2150).
//
// Two things follow from it. A test that compiles or links — doctor's toolchain
// check, the darwin link tests, anything that spawns the built binary — reaches
// the one shared home instead of staging an LLVM view of its own: 375 MB of
// copies on macOS, ~900 MB on Windows, once per test that did it.
//
// And a bare `go test ./cmd/promise/` no longer leaves PROMISE_HOME unset,
// which resolved to the developer's machine-global ~/.promise — the one thing
// docs/build-tools.md §"Test Sandboxing" says no test may write. That bare run
// is the case this decides; under bin/test and bin/verify the variable is
// already set, to the same worktree home, and is kept as it stands.
//
// A test whose subject IS a home's contents (the community-catalog resolution
// tests, `doctor --repair`, the examples installer, the CRT/OpenSSL discovery
// ladders) still sets PROMISE_HOME itself and is unaffected — those homes cost
// nothing, because none of them reaches the toolchain. They keep the retrying
// clitest.TempDir that T2189 gave them; what this removes is the homes that did
// reach it, which is where both the cost and that cleanup hazard came from.
//
// Hence KeepingAmbient rather than plain SharedHome: 22 tests in this package
// re-exec the test binary (`exec.Command(os.Args[0], "-test.run=…")`) and hand
// the child the home it must read — a staged epoch tree, a channel file — in its
// environment. TestMain runs in that child too, so overwriting the variable
// there answers every one of them with the wrong home. An explicitly provided
// home is the caller's declared intent, the same rule Bin applies to
// PROMISE_TEST_BIN.
func TestMain(m *testing.M) { os.Exit(clitest.SharedHomeKeepingAmbient(m)) }
