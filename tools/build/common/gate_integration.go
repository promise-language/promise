package common

// The gates `integration` is made of, each runnable on its own.
//
// Narrowing is a requirement rather than a convenience. A failing gate usually
// fails in one area, and fixing it means iterating: change something, check,
// change again. Re-running everything each round pays for the whole suite to
// learn about the one part being worked on. So the concepts divide by language
// where a project has more than one — `checked:go` and `checked:promise` are
// separately addressable, and `checked` is both — and a step fixing Go vet
// findings runs `bin/run checked:go`.
//
// Passing the parts is not passing the whole, and only the whole may be cited:
// a fix for one area can break another, and a sequence of narrow passes at
// different moments describes no single state.
//
// Every check here is the CHECK-ONLY form. bin/verify repairs on its way to an
// answer (gofmt -w, promise format); a gate must not, because an answer about a
// tree that was repaired first is not an answer about the tree anyone proposed.
// The SDK enforces this: it diffs the tracked tree around every gate run and
// reports any difference as the gate breaking its contract.
//
// Each measurement below runs BEHIND a shared build of the tree in front of it
// — gate_build.go, which explains why, and what each one does when that build
// does not complete.

import (
	"fmt"
	"path/filepath"
)

// measureFormattedGo counts Go files gofmt would rewrite, without rewriting
// them.
func measureFormattedGo(root string) ([]Metric, string, error) {
	files, err := UnformattedGoFiles(root)
	if err != nil {
		return nil, "", fmt.Errorf("checking Go formatting: %w", err)
	}
	return []Metric{Count("unformatted_go_files", len(files))}, "", nil
}

// measureFormattedPromise counts .pr files `promise format` would rewrite. The
// formatter lives in the compiler, so this measures the formatter the tree in
// front of it builds: the build is what makes bin/promise exist, and what makes
// it the CURRENT one rather than whichever source it was last built from.
func measureFormattedPromise(root string) ([]Metric, string, error) {
	if err := ensureGateBuild(root); err != nil {
		return []Metric{}, buildDidNotComplete("Promise formatting was not measured", err), nil
	}
	// The build having completed, bin/promise exists — but UnformattedPromiseFiles
	// answers "no files need formatting" for a missing formatter exactly as it
	// does for a clean tree, so a build that somehow left no binary would be
	// reported here as a clean zero. Checked rather than assumed: a number about
	// nothing is the failure this whole gate is being fixed for.
	if !Exists(filepath.Join(root, "bin", BinaryName())) {
		return []Metric{}, "the build reported success but left no " + BinaryName() + ", so Promise formatting was not measured", nil
	}
	files, err := UnformattedPromiseFiles(root)
	if err != nil {
		return nil, "", fmt.Errorf("checking Promise formatting: %w", err)
	}
	return []Metric{Count("unformatted_promise_files", len(files))}, "", nil
}

// measureBuilds reports whether the build completed, and how many Go packages
// fail to compile. `go build` prefixes each failing package with a "# " header
// line on stderr, so the headers are the count.
//
// The BUILD is measured, not merely performed, because a build that does not
// complete is a fact about this tree and not a gate that could not answer:
// "did not measure anything" reads as broken infrastructure and sends the
// reader to the gate's code, while build_failures = 1 reads as what it is. It
// is a metric of its own rather than an increment of unbuildable_go_packages
// because RunBuild can fail at parser generation, resource embedding or LLVM
// detection — none of which is a Go package failing to compile, and naming the
// wrong subsystem is the whole defect T2102 is about.
func measureBuilds(root string) ([]Metric, string, error) {
	return measureBuildsWith(root, captureSplit)
}

// measureBuildsWith is measureBuilds with the child call as a parameter, so a
// test can pin how a failing `go build` is reported without spawning one.
func measureBuildsWith(root string, capture captureFunc) ([]Metric, string, error) {
	if err := ensureGateBuild(root); err != nil {
		// The sweep is OMITTED rather than reported as zero: it did not run,
		// and a zero here would be a number about nothing.
		return []Metric{Count("build_failures", 1)},
			buildDidNotComplete("the per-package sweep did not run", err), nil
	}
	n := 0
	dirs, incomplete := GoModules(root)
	for _, dir := range dirs {
		// `./...`, deliberately, where checked:go names its packages: generated
		// code is excluded from being CHECKED because a diagnostic in it is not
		// the author's to act on, but it still has to COMPILE like anything
		// else. Excluding it here would hide a broken build.
		_, stderr, err := capture(dir, "go", "build", "./...")
		found := countPrefixed(stderr, "# ")
		if err != nil && found == 0 {
			// It failed and named no package: the failure is about the
			// toolchain or the module, not a package in this tree.
			return nil, "", fmt.Errorf("go build in %s: %w: %s", dir, err, firstRealLine(stderr))
		}
		n += found
	}
	return []Metric{
		Count("unbuildable_go_packages", n),
		Count("build_failures", 0),
	}, incomplete, nil
}

// measureCheckedGo counts the go vet findings bin/check reports. Counting is
// this mode's ONLY addition: GoCheckFindings is the answer, and the tool's only
// addition to the same call is printing it and exiting non-zero.
//
// The two used to reach that answer separately, and the gate's spelling had no
// exclusion at all — so it counted diagnostics in the generated parser that
// bin/check had always left out, and the verdict came to depend on which Go
// release (and which build cache) the host had (T2104).
func measureCheckedGo(root string) ([]Metric, string, error) {
	return measureCheckedGoWith(root, captureSplit)
}

// measureCheckedGoWith is measureCheckedGo with the child call as a parameter,
// the seam a test uses to stand in for `go vet` rather than spawn it.
func measureCheckedGoWith(root string, capture captureFunc) ([]Metric, string, error) {
	if err := ensureGateBuild(root); err != nil {
		return []Metric{}, buildDidNotComplete("go vet was not run", err), nil
	}
	findings, incomplete, err := GoCheckFindings(root, capture)
	if err != nil {
		return nil, "", err
	}
	return []Metric{Count("vet_findings", len(findings))}, incomplete, nil
}

// measureCheckedPromise counts what `promise check` reports over this project's
// Promise code. Counting is this mode's ONLY addition: the sweep's summary line
// is the answer, and the tool mode is that same command run by hand.
//
// The compiler IS the checker, so this measures the one the tree in front of it
// builds — like measureFormattedPromise, and for the same reason.
func measureCheckedPromise(root string) ([]Metric, string, error) {
	return measureCheckedPromiseWith(root, RunPromiseCheckCapture)
}

// checkRunner is RunPromiseCheckCapture's shape as a parameter: the seam a test
// uses to stand in for a sweep that needs a built compiler and takes a minute.
type checkRunner func(root string) (string, error)

// measureCheckedPromiseWith is measureCheckedPromise with the sweep as a
// parameter, so which summary field becomes which metric can be pinned without
// analysing nine hundred units to find out.
func measureCheckedPromiseWith(root string, runCheck checkRunner) ([]Metric, string, error) {
	if err := ensureGateBuild(root); err != nil {
		return []Metric{}, buildDidNotComplete("the Promise check did not run", err), nil
	}
	// The build having completed, bin/promise exists — but a missing checker
	// would otherwise be reported here as a clean zero, which is the failure
	// this whole gate is being added to prevent.
	if !Exists(filepath.Join(root, "bin", BinaryName())) {
		return []Metric{}, "the build reported success but left no " + BinaryName() + ", so the Promise code was not checked", nil
	}
	output, _ := runCheck(root)
	summary := ParseCheckSummaryLine(output)
	if summary == nil {
		return nil, "", fmt.Errorf("the Promise check printed no summary line, so nothing was measured")
	}
	return []Metric{
		Count("promise_check_failures", summary.Failed),
		Count("promise_check_errors", summary.Errors),
		Count("promise_check_warnings", summary.Warnings),
		Count("promise_check_units", summary.Units),
	}, "", nil
}

// measureTestedGo counts failing Go tests and failing packages, across every Go
// module in the tree. Both numbers are worth having: one failing test in one
// package and forty in forty are different situations, and a single number
// cannot tell them apart.
//
// Every module, for the same reason `builds` and `checked:go` sweep them:
// `./...` is module-scoped, so measuring only compiler/ would pass a change
// that bin/verify fails on tools/build — a landing decision made on a smaller
// subject than the one that has to hold.
func measureTestedGo(root string) ([]Metric, string, error) {
	// Here rather than in measureTestedGoWith, unlike the three measurements
	// above: that seam predates the build step and its tests drive it directly
	// with fixture roots, none of which is a tree anything could build.
	//
	// The build itself is not optional here — the compiler's own suite reads the
	// EMBEDDED std (testutil's stdAll), so without it the Go tests describe
	// whichever std was embedded last rather than the one in the tree.
	if err := ensureGateBuild(root); err != nil {
		return []Metric{}, buildDidNotComplete("the Go suites did not run", err), nil
	}
	return measureTestedGoWith(root, captureSplit)
}

// measureTestedGoWith is measureTestedGo with the child call as a parameter. A
// test in this package cannot make the real one: `go test ./...` in tools/build
// IS this suite, so a test that ran it would spawn an unbounded chain of go test
// subprocesses (the hazard TestRunToolsGoTests_TrivialModule already names).
func measureTestedGoWith(root string, capture captureFunc) ([]Metric, string, error) {
	// The same command bin/verify runs (RunGoTests / RunToolsGoTests), from the
	// one place it is spelled: the gate and verify cannot disagree about what
	// "the Go suite" is.
	argv := goTestArgs()

	failedTests, failedPackages := 0, 0
	dirs, incomplete := GoModules(root)
	for _, dir := range dirs {
		stdout, stderr, testErr := capture(dir, "go", argv...)
		tests := countPrefixed(stdout, "--- FAIL:")
		packages := countPrefixed(stdout, "FAIL\t")
		if testErr != nil && tests == 0 && packages == 0 {
			// The run did not happen — a build failure in a test package, most
			// often. That is not "zero failing tests". go reports that kind of
			// failure on stderr; a failing TEST reaches stdout, which is what
			// the two counts above read.
			detail := firstRealLine(stderr)
			if detail == "" {
				detail = firstRealLine(stdout)
			}
			return nil, "", fmt.Errorf("go test in %s: %w: %s", dir, testErr, detail)
		}
		failedTests += tests
		failedPackages += packages
	}
	return []Metric{
		Count("go_test_failures", failedTests),
		Count("go_test_packages_failed", failedPackages),
	}, incomplete, nil
}

// measureTestedPromise runs the host Promise suite. There is no suite to
// measure without a compiler, and no correct suite to measure without the
// CURRENT one — so, like every other gate here, it measures behind the shared
// build (gate_build.go) rather than one of its own.
func measureTestedPromise(root string) ([]Metric, string, error) {
	// Capture, never tee to stdout: RunPromiseTests forwards the suite's own
	// output to OUR stdout, which carries the envelope and nothing else — the
	// summary line landing there makes the whole stream unparseable, and the
	// runner reports that as this gate breaking its contract. The Capture form
	// sends progress to stderr, where a person watching a four-minute suite can
	// still see it.
	return measureTestedPromiseWith(root, RunPromiseTestsCapture)
}

// suiteRunner is RunPromiseTestsCapture's shape as a parameter: the seam a test
// uses to stand in for the host Promise suite, which takes minutes to run and
// needs a built compiler.
type suiteRunner func(root, target string) (string, error)

// measureTestedPromiseWith is measureTestedPromise with the suite as a
// parameter, so which summary field becomes which metric can be pinned without
// running eleven thousand tests to find out.
func measureTestedPromiseWith(root string, runSuite suiteRunner) ([]Metric, string, error) {
	if err := ensureGateBuild(root); err != nil {
		return []Metric{}, buildDidNotComplete("the Promise suite did not run", err), nil
	}

	output, _ := runSuite(root, "")
	summary := ParseTestSummaryLine(output)
	if summary == nil {
		return nil, "", fmt.Errorf("the Promise suite printed no summary line, so nothing was measured")
	}
	return []Metric{
		Count("host_test_failures", summary.Failed),
		Count("host_leak_count", summary.Leaked),
		Count("host_test_count", summary.Passed),
	}, "", nil
}
