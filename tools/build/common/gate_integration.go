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

import (
	"fmt"
	"os"
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
// formatter lives in the compiler, so without a built one this measures
// nothing and says so.
func measureFormattedPromise(root string) ([]Metric, string, error) {
	if !Exists(filepath.Join(root, "bin", BinaryName())) {
		return []Metric{}, "bin/promise is not built, so Promise formatting was not measured", nil
	}
	files, err := UnformattedPromiseFiles(root)
	if err != nil {
		return nil, "", fmt.Errorf("checking Promise formatting: %w", err)
	}
	return []Metric{Count("unformatted_promise_files", len(files))}, "", nil
}

// measureBuilds counts Go packages that fail to compile, and generated sources
// that no longer match what generated them. `go build` prefixes each failing
// package with a "# " header line on stderr, so the headers are the count.
//
// Staleness is MEASURED here rather than repaired anywhere, and that placement
// is the point. compiler/internal/parser/*.go is generated from the grammar and
// is TRACKED, so a change that edits PromiseParser.g4 without regenerating
// leaves the two disagreeing. Any gate that then builds would rewrite tracked
// files, and the runner — which diffs the tracked tree around every gate —
// reports that as the gate breaking its contract: a defect attributed to the
// gate, sending the reader to the gate's code rather than to their own
// uncommitted grammar change, and spending the worktree for the whole
// transition. Reported as a number instead, the same situation fails `builds`
// by name, and the remedy (`bin/build`, then commit the regenerated parser
// alongside the grammar) follows from what the metric says.
func measureBuilds(root string) ([]Metric, string, error) {
	n := 0
	dirs, incomplete := GoModules(root)
	for _, dir := range dirs {
		// `./...`, deliberately, where checked:go names its packages: generated
		// code is excluded from being CHECKED because a diagnostic in it is not
		// the author's to act on, but it still has to COMPILE like anything
		// else. Excluding it here would hide a broken build.
		_, stderr, err := captureSplit(dir, "go", "build", "./...")
		found := countPrefixed(stderr, "# ")
		if err != nil && found == 0 {
			// It failed and named no package: the failure is about the
			// toolchain or the module, not a package in this tree.
			return nil, "", fmt.Errorf("go build in %s: %w: %s", dir, err, firstLine(stderr))
		}
		n += found
	}
	return []Metric{
		Count("unbuildable_go_packages", n),
		Count("stale_generated_files", staleGeneratedFiles(root)),
	}, incomplete, nil
}

// staleGeneratedFiles reports how many generated-and-tracked sources no longer
// match their input. Today that is one thing — the ANTLR parser against the
// grammar — so the count is 0 or 1.
func staleGeneratedFiles(root string) int {
	grammarDir := filepath.Join(root, "compiler", "grammar")
	parserPkg := filepath.Join(root, "compiler", "internal", "parser")
	if !Exists(grammarDir) || parserUpToDate(grammarDir, parserPkg) {
		return 0
	}
	return 1
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
	findings, incomplete, err := GoCheckFindings(root, capture)
	if err != nil {
		return nil, "", err
	}
	return []Metric{Count("vet_findings", len(findings))}, incomplete, nil
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
			detail := firstLine(stderr)
			if detail == "" {
				detail = firstLine(stdout)
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

// measureTestedPromise runs the host Promise suite. It builds the compiler
// first, because there is no suite to measure without one. The build writes to
// bin/ and .promise-home/, both untracked — and it regenerates
// compiler/internal/parser/, which IS tracked, whenever the grammar has moved
// since that parser was generated. That is the one way this gate could modify
// its subject, so it refuses to build in that state rather than risk it.
func measureTestedPromise(root string) ([]Metric, string, error) {
	// Refuse to build over a stale generated parser: the build would rewrite
	// tracked files, which the runner reports as this gate breaking its
	// contract. `builds` reports the staleness as a number, so the tree is
	// already failing by a name that says what to do.
	if staleGeneratedFiles(root) > 0 {
		return []Metric{}, "the generated parser is stale (the grammar changed since compiler/internal/parser was generated), so the Promise suite did not run: run bin/build and commit the regenerated parser with the grammar change", nil
	}
	// Build progress would otherwise reach this process's stdout, which carries
	// the envelope and nothing else.
	savedStdout := os.Stdout
	os.Stdout = os.Stderr
	buildErr := RunBuild(root, nil)
	os.Stdout = savedStdout
	if buildErr != nil {
		return []Metric{}, "the compiler did not build, so the Promise suite did not run: " + firstLine(buildErr.Error()), nil
	}

	// Capture, never tee to stdout: RunPromiseTests forwards the suite's own
	// output to OUR stdout, which carries the envelope and nothing else — the
	// summary line landing there makes the whole stream unparseable, and the
	// runner reports that as this gate breaking its contract. The Capture form
	// sends progress to stderr, where a person watching a four-minute suite can
	// still see it.
	output, _ := RunPromiseTestsCapture(root, "")
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
