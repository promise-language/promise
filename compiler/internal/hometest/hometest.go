// Package hometest gives a test package a Promise home of its own, and fails
// the package if its tests reached the machine-global one anyway.
//
// docs/build-tools.md §"Test Sandboxing": no test writes ~/.promise. A package
// whose code resolves a home (module.PromiseHome) and whose TestMain pins none
// resolves ~/.promise under a bare `go test` with PROMISE_HOME unset — and a
// test that unsets the variable without restoring it does the same for every
// test after it, even under bin/test (#102). Pin closes both: the package runs
// under a private temp home, and the top-level listing of ~/.promise is compared
// before and after, so a test that reaches it anyway reddens the package rather
// than silently costing the host its caches.
//
// The tools module carries the same guard for its own package
// (tools/build/common/sandbox_guard_test.go); the two are separate Go modules
// and cannot share it.
//
// It is for packages whose tests need no toolchain. A package that compiles or
// links shares the worktree's warm home through clitest.SharedHome instead,
// because a private home would stage a whole toolchain into it.
package hometest

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Pin runs m under a fresh, private PROMISE_HOME, with any inherited
// PROMISE_CACHE cleared so no derived cache lands outside it, and returns m's
// exit code — or 1, with a report on stderr, when ~/.promise moved while the
// package ran. Call it from TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(hometest.Pin(m)) }
func Pin(m interface{ Run() int }) int {
	before := sharedHomeListing()

	home, err := os.MkdirTemp("", "promise-hometest-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hometest: cannot create a private PROMISE_HOME: %v\n", err)
		return 1
	}
	os.Setenv("PROMISE_HOME", home)
	os.Unsetenv("PROMISE_CACHE")

	code := m.Run()
	os.RemoveAll(home)

	if report := damage(before, sharedHomeListing()); report != "" {
		fmt.Fprint(os.Stderr, report)
		code = 1
	}
	return code
}

// sharedHomeListing is the sorted top-level entry names of ~/.promise,
// "absent" when there is none, or "unavailable" when the host cannot name its
// user home.
//
// Depth 1 is the sensitivity/noise trade-off the tools guard makes too: it
// catches a home being created, removed, or gaining or losing a top-level
// subtree, while an installed promise writing inside cache/ cannot redden a
// package that never touched it.
func sharedHomeListing() string {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "unavailable"
	}
	entries, err := os.ReadDir(filepath.Join(userHome, ".promise"))
	if err != nil {
		return "absent"
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

// damage is the report for a listing that moved between before and after, or
// "" when it did not. Separate from Pin so the decision is itself testable,
// rather than code that only ever runs on a machine already damaged.
func damage(before, after string) string {
	if before == after {
		return ""
	}
	return fmt.Sprintf("hometest: a test reached the machine-global Promise home (~/.promise) — "+
		"give it a home of its own with t.Setenv(\"PROMISE_HOME\", t.TempDir())\n"+
		"  before: %s\n  after:  %s\n", before, after)
}
