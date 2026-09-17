package clitest

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// The compiler these tests drive is a build artifact that no `go test` produces,
// and until T2137 nothing checked that it matched the tree. A stale bin/promise
// is the worst kind of wrong: the suites do not skip and do not warn — they
// report a *product* defect, with plausible compiler output attached, for a fix
// that is present in the source and simply absent from the artifact. That is how
// a green trunk was filed as a critical "trunk red" alarm (T2134).
//
// So the locator refuses a binary older than the tree, in the shape
// common.CheckStale established for every bin/* tool: fail (never skip — a
// skipped end-to-end suite is how this goes unnoticed a second time) and name
// the rebuild command.
//
// The one invariant this file must hold: **the check is never stricter than
// bin/build's own up-to-date rule** (isBinaryUpToDate in
// tools/build/common/build.go). bin/build is what the message tells the caller
// to run, so a check that could refuse a binary bin/build calls current would
// wedge the caller between two tools — the T1813 defect in a new place. The
// inputs below therefore mirror that function's, and the two deliberate
// differences are both in the lax direction: the buildinfo version comparison is
// dropped, and _test.go files are dropped because `go build` never compiles them
// into the binary, so editing one of these suites' own tests cannot make the
// artifact stale.
//
// The lists are duplicated rather than shared because clitest is in the compiler
// module and tools/build/common is not; importing across that boundary would
// either drag the compiler's module graph into ./make's bootstrap or make the
// shipped compiler module depend on the build tools. Whoever edits either list
// keeps them in that subset relationship by hand — the comment on
// isBinaryUpToDate says so from the other side.
var (
	compilerSourceDirs = []string{
		"compiler",
		"modules",
		"examples",
		filepath.FromSlash("tools/build/winlink/def"),
	}
	compilerSourceFiles = []string{
		"catalog.toml",
		filepath.FromSlash("docs/language-guide.md"),
	}
	// Generated copies bin/build writes on its way to the binary, derived from
	// modules/ and docs/language-guide.md — which are checked directly. bin/build
	// skips them and so must this: counting a build's own outputs as its inputs
	// would make the answer depend on the order the build writes files rather
	// than on what the tree says.
	compilerSourceSkips = []string{
		filepath.FromSlash("compiler/cmd/promise/resources"),
		filepath.FromSlash("compiler/internal/testutil/testdata"),
	}
)

// sourceStamp is the newest source file found under root: its repo-relative,
// slash-separated path and its modification time.
type sourceStamp struct {
	rel string
	mod time.Time
}

// newestSource returns the most recently modified compiler input under root, or
// a zero stamp when root holds none of them.
func newestSource(root string) sourceStamp {
	var newest sourceStamp
	consider := func(rel string, info fs.FileInfo) {
		if info.ModTime().After(newest.mod) {
			newest = sourceStamp{rel: filepath.ToSlash(rel), mod: info.ModTime()}
		}
	}

	// Resolved once rather than per directory visited: the walk asks this
	// question of every directory under compiler/.
	skips := make([]string, len(compilerSourceSkips))
	for i, skip := range compilerSourceSkips {
		skips[i] = filepath.Join(root, skip)
	}

	for _, dir := range compilerSourceDirs {
		abs := filepath.Join(root, dir)
		if _, err := os.Stat(abs); err != nil {
			// A missing directory is not a source, exactly as anySourceNewer
			// treats it — an exported or partial tree must not fail the walk.
			continue
		}
		filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if slices.Contains(skips, path) {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(d.Name(), "_test.go") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			consider(rel, info)
			return nil
		})
	}

	for _, file := range compilerSourceFiles {
		if info, err := os.Stat(filepath.Join(root, file)); err == nil {
			consider(file, info)
		}
	}
	return newest
}

// freshnessError reports that bin predates the tree at root, or nil when it does
// not. A binary that cannot be stat'ed is not this check's problem — the caller
// has already established it exists.
func freshnessError(root, bin string) error {
	info, err := os.Stat(bin)
	if err != nil {
		return nil
	}
	newest := newestSource(root)
	if newest.rel == "" || !newest.mod.After(info.ModTime()) {
		return nil
	}

	// Absolute path first, bare spelling second: the recovery command a
	// staleness message names must be one the caller can run from the directory
	// they are in, and the bare form only resolves at the repo root
	// (docs/build-tools.md, "Staleness Check"). Naming the offending file is
	// what separates "the artifact is old" from "the product is broken".
	const stamp = "2006-01-02 15:04:05"
	abs, relative := buildCommands(root)
	return fmt.Errorf("bin/%s is older than the tree — rebuild before running these tests:\n"+
		"  %s   (or %s from the repo root)\n"+
		"  newest source: %s (%s) is newer than bin/%s (%s)",
		binaryName(),
		abs, relative,
		newest.rel, newest.mod.Format(stamp),
		binaryName(), info.ModTime().Format(stamp))
}

// buildCommands returns the two spellings of this repository's compiler build
// tool: the absolute path, which resolves from any working directory, and the
// relative form, which only works at the repo root.
//
// It is the clitest-side twin of common.MakeCommands, which does this for
// ./make. It cannot call that helper — it lives in the build-tools module,
// which the compiler module deliberately does not depend on — so it repeats the
// one thing that helper exists to centralize: Windows spells both differently
// (docs/windows-support.md writes `bin\build`), and a message naming a command
// the caller's shell will not run is the T1813 defect.
func buildCommands(root string) (abs, relative string) {
	return buildCommandsFor(runtime.GOOS, root)
}

// buildCommandsFor is buildCommands with the platform named rather than read
// from the host, so both spellings are exercised wherever the suite runs. The
// Windows spelling is the one that cannot be checked on the host that runs
// almost every build: T2152 was this test package asserting `bin/build`
// unconditionally, which passed on every Linux and macOS run and could not pass
// on any Windows one, and it reached trunk because nothing here ever evaluated
// the Windows branch.
//
// Only the two literals vary by platform. The absolute path is still joined
// with the *host* separator, since filepath is compiled for one platform — so
// buildCommandsFor("windows", …) off Windows yields a real Windows suffix on a
// host-shaped path, which is what makes the suffix, and not the separator,
// the part a foreign-platform caller may assert on.
func buildCommandsFor(goos, root string) (abs, relative string) {
	if goos == "windows" {
		return filepath.Join(root, "bin", "build.exe"), `bin\build`
	}
	return filepath.Join(root, "bin", "build"), "bin/build"
}

// cachedFreshnessError memoizes freshnessError per (root, binary). The walk
// costs a few milliseconds over a thousand files and every test in a package
// asks the same question, so it is answered once per test binary.
var (
	freshnessMu    sync.Mutex
	freshnessCache = map[string]error{}
)

func cachedFreshnessError(root, bin string) error {
	key := root + "\x00" + bin
	freshnessMu.Lock()
	defer freshnessMu.Unlock()
	if err, ok := freshnessCache[key]; ok {
		return err
	}
	err := freshnessError(root, bin)
	freshnessCache[key] = err
	return err
}

func binaryName() string { return "promise" + exeSuffix() }

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
