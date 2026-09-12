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

// goModules are the Go modules in this repository. `./...` is module-scoped, so
// one invocation at the root would silently skip the other module — honest
// numbers about part of the subject, which is an incomplete run that does not
// know it is incomplete.
func goModules(root string) []string {
	dirs := []string{filepath.Join(root, "compiler")}
	if tools := filepath.Join(root, "tools", "build"); Exists(filepath.Join(tools, "go.mod")) {
		dirs = append(dirs, tools)
	}
	return dirs
}

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
	for _, dir := range goModules(root) {
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
	}, "", nil
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

// measureCheckedGo counts go vet diagnostics — the lines naming a file and a
// position, as distinct from the "# package" headers that group them.
func measureCheckedGo(root string) ([]Metric, string, error) {
	n := 0
	for _, dir := range goModules(root) {
		_, stderr, err := captureSplit(dir, "go", "vet", "./...")
		found := countDiagnostics(stderr)
		if err != nil && found == 0 {
			return nil, "", fmt.Errorf("go vet in %s: %w: %s", dir, err, firstLine(stderr))
		}
		n += found
	}
	return []Metric{Count("vet_findings", n)}, "", nil
}

// measureTestedGo counts failing Go tests and failing packages. Both are worth
// having: one failing test in one package and forty in forty are different
// situations, and a single number cannot tell them apart.
//
// Only the compiler module. The tools/build suite deletes ~/.promise and
// expires every cached Go test result on the machine when it runs outside
// bin/verify's lock (T2084), and a gate must not do that to the host it
// measures on. The run says so rather than reporting a number that looks like
// the full set.
func measureTestedGo(root string) ([]Metric, string, error) {
	compilerDir := filepath.Join(root, "compiler")
	args := append([]string{"test", "-timeout", "30m"}, goTestConcurrencyArgs()...)
	stdout, _, testErr := captureSplit(compilerDir, "go", append(args, "./...")...)
	failedTests := countPrefixed(stdout, "--- FAIL:")
	failedPackages := countPrefixed(stdout, "FAIL\t")
	if testErr != nil && failedTests == 0 && failedPackages == 0 {
		// The run did not happen — a build failure in a test package, most
		// often. That is not "zero failing tests".
		return nil, "", fmt.Errorf("go test in %s: %w: %s", compilerDir, testErr, firstLine(stdout))
	}
	return []Metric{
			Count("go_test_failures", failedTests),
			Count("go_test_packages_failed", failedPackages),
		},
		"the tools/build Go suite was not run (T2084: outside bin/verify's lock it deletes ~/.promise)",
		nil
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
