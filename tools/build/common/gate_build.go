package common

// The one build a gate process performs, before it measures anything.
//
// EVERY GATE MEASURES THE TREE AS IT IS NOW, never a leftover build. That is
// not a nicety: the compiler go:embeds about a dozen generated, UNTRACKED
// inputs that only bin/build produces — compiler/cmd/promise/resources/ and
// compiler/internal/testutil/testdata/std/. Measuring without producing them
// first fails in three ways, all of them silent (T2102):
//
//   - STALE. A change edits modules/std/*.pr and does not run bin/build. `go
//     build`, `go vet` and `go test` then compile and test the PREVIOUSLY
//     embedded copy, and `promise format` runs an old formatter. The gate
//     reports numbers about a tree nobody proposed, and the change lands.
//   - MISSING. On a fresh clone the embed patterns match nothing, so `go build
//     ./...` exits 1 at package load naming no package, and `builds` returned
//     "did not measure anything" — which reads as a broken gate.
//   - WORSE THAN MISSING. Those same package-load lines have a diagnostic's
//     shape, so `checked:go` COUNTED them, reporting vet findings for a tree
//     vet had refused to look at. A number reads as a broken CHANGE where an
//     error reads as a broken gate, and sends the reader hunting for findings
//     that do not exist.
//
// The build is bin/build's own RunBuild, called IN-PROCESS. Spawning bin/build
// would measure whichever bin/build is on disk — the same staleness, one level
// up — and the gate adds no freshness rule of its own: RunBuild's quick
// up-to-date check decides, so an already-built tree costs about 0.1s.
//
// fit is the only gate that skips it, because it measures the machine rather
// than the tree and must be answerable on a machine that cannot build.
//
// THE BUILD ALSO PINS THE WORKTREE'S PROMISE HOME (#102). A gate is run as
// `bin/gate <name> --envelope` with whatever environment its caller had, and a
// caller with no PROMISE_HOME made every compiler the gate started resolve the
// machine-global ~/.promise — the warm-up, `promise check`, the whole Promise
// suite — while the Go CLI suites pinned themselves to .promise-home: one run,
// two homes, and a verdict that depended on the caller's shell. Pinning here,
// rather than in each entry point, ties it to the step every tree-measuring
// gate already takes: a gate added later builds by default, and so pins by
// default. The machine gates, which do not build, are the ones whose subject is
// not this tree's home.

import (
	"fmt"
	"os"
	"sync"
)

// runGateBuild is bin/build's entry point, as a seam a test can stand in for.
// It IS RunBuild, with RunBuild's exact signature, so a stub can assert the
// gate passes no flags.
var runGateBuild = RunBuild

// onceBuild runs the build at most once and remembers the outcome, so every
// part of a composition shares one call: `integration` measures five gates and
// builds once.
type onceBuild struct {
	once sync.Once
	err  error
	// runs counts how many times the build actually ran. Tests pin it; nothing
	// in the gate reads it.
	runs int
}

func (b *onceBuild) ensure(root string) error {
	b.once.Do(func() {
		// Before the build, so the build itself — its compiler stamp and
		// embedded-module extraction — and every process the gate starts after
		// it resolve this worktree's home and temp directory, whatever the
		// caller exported. A home that cannot be pinned is not built into:
		// every measurement that needs the build then reports it as not
		// completed, rather than measuring in a home that is not this tree's.
		if err := SetupLocalCache(root); err != nil {
			b.err = fmt.Errorf("pin the worktree Promise home: %w", err)
			return
		}
		b.runs++
		// RunBuild prints progress with fmt.Println to stdout, which carries
		// the envelope and nothing else — one JSON object, or a runner reads
		// this gate as broken.
		saved := os.Stdout
		os.Stdout = os.Stderr
		defer func() { os.Stdout = saved }()
		// NO flags. -release or -generate would skip RunBuild's quick
		// up-to-date check and make every gate run pay for a full build.
		b.err = runGateBuild(root, nil)
	})
	return b.err
}

// gateBuild is that build, for this process. A pointer, because a sync.Once
// must never be copied — and a test resetting the state between cases replaces
// this whole value.
var gateBuild = &onceBuild{}

// ensureGateBuild brings the build up to date, once, and reports whether it
// completed. Idempotent: MeasureContractGate calls it for the gate asked for,
// and each measurement that reads build artifacts calls it again to learn the
// outcome.
//
// One root per process, which is what makes "once" well defined: bin/gate is
// invoked as `bin/gate <name> --envelope` against the repository it was built
// for, and a composition passes that same root down to every part.
func ensureGateBuild(root string) error { return gateBuild.ensure(root) }

// buildDidNotComplete is the one reason every measurement that reads build
// artifacts gives when the build failed, spelled once so they cannot drift.
// The numbers are omitted rather than reported as zero: they were not measured.
func buildDidNotComplete(what string, err error) string {
	return "the build did not complete, so " + what + ": " + firstRealLine(err.Error())
}
