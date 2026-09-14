package common

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// sandboxState is the machine-global state this package has destroyed before:
// the host Promise home, and the Go test cache's expiry stamp.
type sandboxState struct {
	promiseHome string
	expireStamp string
}

// sandboxSnapshot reads both watched values.
func sandboxSnapshot() sandboxState {
	return sandboxState{
		promiseHome: promiseHomeListing(),
		expireStamp: testExpireStamp(),
	}
}

// sandboxDamage returns one report for each watched value that moved between
// the two snapshots, and nothing at all when the package left the host as it
// found it. Separate from TestMain so the decision it drives — any movement
// fails the package — is itself a testable function rather than three lines
// that only ever run on a machine that has already been damaged.
func sandboxDamage(before, after sandboxState) []string {
	var reports []string
	if before.promiseHome != after.promiseHome {
		reports = append(reports, globalDamageReport(
			"the host Promise home (~/.promise) changed",
			before.promiseHome, after.promiseHome))
	}
	if before.expireStamp != after.expireStamp {
		reports = append(reports, globalDamageReport(
			"the Go test cache expiry stamp ($GOCACHE/testexpire.txt) moved",
			before.expireStamp, after.expireStamp))
	}
	return reports
}

// TestMain fails the package if a test acted on machine-global state instead of
// building its own world: the host Promise home (~/.promise) or the Go test
// cache's expiry stamp. Both were destroyed from here before T2084 — a flag test
// ran the verify pipeline with --shared --clean and deleted the installed
// toolchain, and the clean tests ran a real `go clean -testcache`, expiring every
// cached Go test result on the host, for every module and every clone sharing
// the cache.
//
// HOME and GOCACHE are deliberately NOT redirected for the whole package: tests
// that shell out to `go` would lose the warm build and module caches, which is
// most of what makes this suite fast.
func TestMain(m *testing.M) {
	before := sandboxSnapshot()

	code := m.Run()

	for _, report := range sandboxDamage(before, sandboxSnapshot()) {
		fmt.Fprint(os.Stderr, report)
		code = 1
	}
	os.Exit(code)
}

// promiseHomeListing is the sorted top-level entry names of ~/.promise, or
// "absent".
//
// Depth 1 is the deliberate sensitivity/noise trade-off: it catches a RemoveAll
// of the home or of any top-level subtree, while another clone writing inside
// cache/ cannot redden this package. verify.lock and its .owner sibling are
// excluded — a concurrent bin/verify creates and removes them at any moment, and
// they are the one thing the lock protocol is allowed to touch.
func promiseHomeListing() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "unavailable"
	}
	entries, err := os.ReadDir(filepath.Join(home, ".promise"))
	if err != nil {
		return "absent"
	}
	var names []string
	for _, e := range entries {
		switch e.Name() {
		case "verify.lock", "verify.lock.owner":
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

// testExpireStamp is the mtime/size of $GOCACHE/testexpire.txt, or "absent".
// `go clean -testcache` writes that file, and every Go test result saved before
// its timestamp is then treated as expired.
//
// When neither the environment nor `go env` resolves a cache directory, this
// half of the guard reports "unavailable" rather than failing closed on a
// machine with no Go toolchain on PATH.
func testExpireStamp() string {
	cache := os.Getenv("GOCACHE")
	if cache == "" {
		out, err := RunOutputQuiet("go", "env", "GOCACHE")
		if err != nil {
			return "unavailable"
		}
		cache = strings.TrimSpace(out)
	}
	if cache == "" || cache == "off" {
		return "unavailable"
	}
	info, err := os.Stat(filepath.Join(cache, "testexpire.txt"))
	if err != nil {
		return "absent"
	}
	return fmt.Sprintf("%d/%d", info.ModTime().UnixNano(), info.Size())
}

// globalDamageReport renders why the package failed even though its tests may
// all have passed.
func globalDamageReport(what, before, after string) string {
	return fmt.Sprintf(`
=====================================================================
tools/build/common: a test acted on machine-global state
---------------------------------------------------------------------
%s
  before: %s
  after:  %s

A test states its whole world or builds it (docs/org/engineering-guide.md,
"Testing"): a temp root, a redirected HOME, a redirected GOCACHE. It never
touches ~/.promise or the shared Go caches — deleting the first cost this
machine its installed toolchain, and stamping the second costs every clone
on it a full rerun of the compiler's Go tests (T2084).

If nothing in this package is to blame, a concurrent bin/clean, bin/verify
--clean or promise install on the host can also move these.
=====================================================================
`, what, before, after)
}

// TestSandboxGuardSeesWhatItWatches pins the guard against the one failure it
// cannot report on itself: silently becoming a no-op. Each half must answer with
// the state it is pointed at, and must answer *differently* once that state
// moves — otherwise every test in this package is unwatched and nothing says so.
func TestSandboxGuardSeesWhatItWatches(t *testing.T) {
	t.Run("expiry stamp", func(t *testing.T) {
		cache := t.TempDir()
		t.Setenv("GOCACHE", cache)
		if got := testExpireStamp(); got != "absent" {
			t.Errorf("an unstamped cache reads %q, want %q", got, "absent")
		}

		stamp := filepath.Join(cache, "testexpire.txt")
		if err := os.WriteFile(stamp, []byte("2026-01-01T00:00:00Z\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		first := testExpireStamp()
		if first == "absent" || first == "unavailable" {
			t.Fatalf("a stamped cache reads %q, want its mtime/size", first)
		}

		// Re-stamping is what `go clean -testcache` does to a cache that already
		// carries one — the exact case the guard exists to catch.
		if err := os.Chtimes(stamp, time.Time{}, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if second := testExpireStamp(); second == first {
			t.Errorf("a re-stamped cache still reads %q — the guard would miss a `go clean -testcache`", second)
		}
	})

	t.Run("cache disabled", func(t *testing.T) {
		t.Setenv("GOCACHE", "off")
		if got := testExpireStamp(); got != "unavailable" {
			t.Errorf("GOCACHE=off reads %q, want %q", got, "unavailable")
		}
	})

	// With GOCACHE unset the guard has to ask the toolchain where the cache is,
	// because that is the shape of every ordinary run: nobody exports GOCACHE,
	// and a guard that answered "unavailable" there would watch nothing on the
	// machines it exists to protect.
	t.Run("cache resolved from the toolchain", func(t *testing.T) {
		t.Setenv("GOCACHE", "")
		if got := testExpireStamp(); got == "unavailable" {
			t.Error("an unset GOCACHE must fall back to `go env GOCACHE`, not give up")
		}
	})

	// A home the OS cannot name is reported as its own value rather than
	// collapsing into "absent": "absent" is a real, comparable observation
	// ("there is no ~/.promise"), and a host whose HOME is undefined has made no
	// observation at all.
	t.Run("home unresolvable", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("USERPROFILE", "") // os.UserHomeDir on Windows
		if got := promiseHomeListing(); got != "unavailable" {
			t.Errorf("an undefined HOME reads %q, want %q", got, "unavailable")
		}
	})

	t.Run("promise home", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
		if got := promiseHomeListing(); got != "absent" {
			t.Errorf("a host with no ~/.promise reads %q, want %q", got, "absent")
		}

		promise := filepath.Join(home, ".promise")
		for _, dir := range []string{"cache", "bin"} {
			if err := os.MkdirAll(filepath.Join(promise, dir), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		// A concurrent bin/verify holds the lock while this package runs, so the
		// lock pair is the one thing the listing must stay blind to.
		for _, f := range []string{"verify.lock", "verify.lock.owner"} {
			if err := os.WriteFile(filepath.Join(promise, f), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if got, want := promiseHomeListing(), "bin cache"; got != want {
			t.Errorf("listing = %q, want %q (sorted, verify lock excluded)", got, want)
		}

		// Losing a top-level subtree is what a stray `--shared --clean` does.
		if err := os.RemoveAll(filepath.Join(promise, "bin")); err != nil {
			t.Fatal(err)
		}
		if got, want := promiseHomeListing(), "cache"; got != want {
			t.Errorf("listing after a subtree was removed = %q, want %q", got, want)
		}
	})
}

// TestSandboxDamage covers the verdict the sensors feed: silence when nothing
// moved, and one named report per thing that did.
//
// TestSandboxGuardSeesWhatItWatches proves the two readings track their state;
// this proves the package is actually failed when they disagree. Without it the
// guard could be inverted, or reduced to a log line, and every test in this
// file would still pass — which is exactly the silent no-op the guard exists to
// rule out.
func TestSandboxDamage(t *testing.T) {
	intact := sandboxState{promiseHome: "bin cache", expireStamp: "1700000000/20"}

	for _, tc := range []struct {
		name        string
		after       sandboxState
		wantReports int
		wantNamed   []string
	}{
		{"nothing moved", intact, 0, nil},
		{
			"home wiped",
			sandboxState{promiseHome: "absent", expireStamp: intact.expireStamp},
			1, []string{"~/.promise", "bin cache", "absent"},
		},
		{
			"a subtree of the home lost",
			sandboxState{promiseHome: "cache", expireStamp: intact.expireStamp},
			1, []string{"~/.promise"},
		},
		{
			"test cache expired",
			sandboxState{promiseHome: intact.promiseHome, expireStamp: "1800000000/20"},
			1, []string{"testexpire.txt", "1800000000/20"},
		},
		{
			"both",
			sandboxState{promiseHome: "absent", expireStamp: "1800000000/20"},
			2, []string{"~/.promise", "testexpire.txt"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sandboxDamage(intact, tc.after)
			if len(got) != tc.wantReports {
				t.Fatalf("sandboxDamage(...) returned %d reports, want %d: %v", len(got), tc.wantReports, got)
			}
			all := strings.Join(got, "\n")
			for _, want := range tc.wantNamed {
				if !strings.Contains(all, want) {
					t.Errorf("the report should name %q, got:\n%s", want, all)
				}
			}
			for _, report := range got {
				// The reader of a red package needs the item that failed it, not
				// just a diff of two opaque strings.
				if !strings.Contains(report, "T2084") {
					t.Errorf("the report should cite the item explaining it:\n%s", report)
				}
			}
		})
	}
}
