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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// nameFiles prints what a count stands for, as narration — on stderr for
// whoever is watching, where every gate's human output goes. The metric is the
// machine's half of the same answer; a reader told "3" and not WHICH three has
// to go and find them — and for a gate whose subject is repaired by a command
// they already have, that is the whole of the work. measureLatestInvariant sets
// the precedent.
func nameFiles(what string, files []string) {
	w := Narration()
	for _, f := range files {
		fmt.Fprintf(w, "%s: %s\n", what, f)
	}
}

// measureFormattedGo counts Go files gofmt would rewrite, without rewriting
// them.
func measureFormattedGo(root string) (measurement, error) {
	files, err := UnformattedGoFiles(root)
	if err != nil {
		return measurement{}, fmt.Errorf("checking Go formatting: %w", err)
	}
	nameFiles("unformatted", files)
	return measurement{
		metrics: sharedCount("go", "unformatted_files", len(files)),
		groups:  unformattedGroups(root, "gofmt", files),
	}, nil
}

// unformattedGroups is the evidence behind an unformatted count: one group,
// named for the formatter, with a failing member per file it would rewrite.
// None when there is nothing to name.
func unformattedGroups(root, formatter string, files []string) []Group {
	if len(files) == 0 {
		return nil
	}
	g := Group{Name: formatter}
	for _, f := range files {
		g.Members = append(g.Members, newMember(relToRoot(root, f), OutcomeFail, ""))
	}
	return []Group{g}
}

// measureFormattedPromise counts .pr files `promise format` would rewrite. The
// formatter lives in the compiler, so this measures the formatter the tree in
// front of it builds: the build is what makes bin/promise exist, and what makes
// it the CURRENT one rather than whichever source it was last built from.
func measureFormattedPromise(root string) (measurement, error) {
	if err := ensureGateBuild(root); err != nil {
		return measurement{incomplete: buildDidNotComplete("Promise formatting was not measured", err)}, nil
	}
	// The build having completed, bin/promise exists — but UnformattedPromiseFiles
	// answers "no files need formatting" for a missing formatter exactly as it
	// does for a clean tree, so a build that somehow left no binary would be
	// reported here as a clean zero. Checked rather than assumed: a number about
	// nothing is the failure this whole gate is being fixed for.
	if !Exists(filepath.Join(root, "bin", BinaryName())) {
		return measurement{incomplete: "the build reported success but left no " + BinaryName() + ", so Promise formatting was not measured"}, nil
	}
	return measureFormattedPromiseWith(root, UnformattedPromiseFiles)
}

// measureFormattedPromiseWith is the counting half of measureFormattedPromise,
// with the formatter's answer as a parameter so a test can stand in for it.
func measureFormattedPromiseWith(root string, unformatted func(root string) ([]string, error)) (measurement, error) {
	files, err := unformatted(root)
	if err != nil {
		return measurement{}, fmt.Errorf("checking Promise formatting: %w", err)
	}
	nameFiles("unformatted", files)
	return measurement{
		metrics: sharedCount("promise", "unformatted_files", len(files)),
		groups:  unformattedGroups(root, "promise format", files),
	}, nil
}

// measureBuilds reports whether the build completed, and how many Go packages
// fail to compile. `go build` prefixes each failing package with a "# " header
// line on stderr, so the headers are the count.
//
// The BUILD is measured, not merely performed, because a build that does not
// complete is a fact about this tree and not a gate that could not answer:
// "did not measure anything" reads as broken infrastructure and sends the
// reader to the gate's code, while build_failures = 1 reads as what it is. It
// is a metric of its own rather than an increment of unbuildable_packages
// because RunBuild can fail at parser generation, resource embedding or LLVM
// detection — none of which is a Go package failing to compile, and naming the
// wrong subsystem is the whole defect T2102 is about.
func measureBuilds(root string) (measurement, error) {
	return measureBuildsWith(root, captureSplit)
}

// measureBuildsWith is measureBuilds with the child call as a parameter, so a
// test can pin how a failing `go build` is reported without spawning one.
func measureBuildsWith(root string, capture captureFunc) (measurement, error) {
	if err := ensureGateBuild(root); err != nil {
		// The sweep is OMITTED rather than reported as zero: it did not run,
		// and a zero here would be a number about nothing.
		return measurement{
			metrics:    []Metric{Count("build_failures", 1)},
			groups:     []Group{{Name: "build", Members: []Member{newMember("RunBuild", OutcomeFail, err.Error())}}},
			incomplete: buildDidNotComplete("the per-package sweep did not run", err),
		}, nil
	}
	n := 0
	var groups []Group
	dirs, incomplete := GoModules(root)
	for _, dir := range dirs {
		// `./...`, deliberately, where checked:go names its packages: generated
		// code is excluded from being CHECKED because a diagnostic in it is not
		// the author's to act on, but it still has to COMPILE like anything
		// else. Excluding it here would hide a broken build.
		_, stderr, err := capture(dir, "go", "build", "./...")
		failures := packageBuildFailures(stderr)
		if err != nil && len(failures) == 0 {
			// It failed and named no package: the failure is about the
			// toolchain or the module, not a package in this tree.
			return measurement{}, fmt.Errorf("go build in %s: %w: %s", dir, err, firstRealLine(stderr))
		}
		n += len(failures)
		if len(failures) > 0 {
			groups = append(groups, Group{Name: relToRoot(root, dir), Members: failures})
		}
	}
	return measurement{
		metrics:    append(sharedCount("go", "unbuildable_packages", n), Count("build_failures", 0)),
		groups:     groups,
		incomplete: incomplete,
	}, nil
}

// packageBuildFailures reads `go build`'s stderr: each failing package is a
// "# <pkg>" header with the compiler's lines under it, and becomes a member
// carrying those lines.
func packageBuildFailures(stderr string) []Member {
	var members []Member
	var pkg string
	var lines []string
	flush := func() {
		if pkg != "" {
			members = append(members, newMember(pkg, OutcomeFail, strings.Join(lines, "\n")))
		}
	}
	for _, line := range strings.Split(stderr, "\n") {
		if name, ok := strings.CutPrefix(line, "# "); ok {
			flush()
			pkg, lines = strings.TrimSpace(name), nil
			continue
		}
		if pkg != "" && strings.TrimSpace(line) != "" && !isToolchainNotice(line) {
			lines = append(lines, line)
		}
	}
	flush()
	return members
}

// measureCheckedGo counts the go vet findings bin/check reports. Counting is
// this mode's ONLY addition: GoCheckFindings is the answer, and the tool's only
// addition to the same call is printing it and exiting non-zero.
//
// The two used to reach that answer separately, and the gate's spelling had no
// exclusion at all — so it counted diagnostics in the generated parser that
// bin/check had always left out, and the verdict came to depend on which Go
// release (and which build cache) the host had (T2104).
func measureCheckedGo(root string) (measurement, error) {
	return measureCheckedGoWith(root, captureSplit)
}

// measureCheckedGoWith is measureCheckedGo with the child call as a parameter,
// the seam a test uses to stand in for `go vet` rather than spawn it.
func measureCheckedGoWith(root string, capture captureFunc) (measurement, error) {
	if err := ensureGateBuild(root); err != nil {
		return measurement{incomplete: buildDidNotComplete("go vet was not run", err)}, nil
	}
	findings, incomplete, err := GoCheckFindings(root, capture)
	if err != nil {
		return measurement{}, err
	}
	// A group per module the findings came from, a member per finding: File is
	// relative to that module, so the group is what locates it.
	var groups []Group
	index := map[string]int{}
	for _, d := range findings {
		i, ok := index[d.Dir]
		if !ok {
			i = len(groups)
			index[d.Dir] = i
			groups = append(groups, Group{Name: relToRoot(root, d.Dir)})
		}
		groups[i].Members = append(groups[i].Members, newMember(d.File, OutcomeFail, d.Text))
	}
	return measurement{
		metrics:    sharedCount("go", "check_findings", len(findings)),
		groups:     groups,
		incomplete: incomplete,
	}, nil
}

// measureCheckedPromise counts what `promise check` reports over this project's
// Promise code. Counting is this mode's ONLY addition: the sweep's summary line
// is the answer, and the tool mode is that same command run by hand.
//
// The compiler IS the checker, so this measures the one the tree in front of it
// builds — like measureFormattedPromise, and for the same reason.
func measureCheckedPromise(root string) (measurement, error) {
	return measureCheckedPromiseWith(root, RunPromiseCheckCapture)
}

// checkRunner is RunPromiseCheckCapture's shape as a parameter: the seam a test
// uses to stand in for a sweep that needs a built compiler and takes a minute.
type checkRunner func(root string) (string, error)

// measureCheckedPromiseWith is measureCheckedPromise with the sweep as a
// parameter, so which summary field becomes which metric can be pinned without
// analysing nine hundred units to find out.
func measureCheckedPromiseWith(root string, runCheck checkRunner) (measurement, error) {
	if err := ensureGateBuild(root); err != nil {
		return measurement{incomplete: buildDidNotComplete("the Promise check did not run", err)}, nil
	}
	// The build having completed, bin/promise exists — but a missing checker
	// would otherwise be reported here as a clean zero, which is the failure
	// this whole gate is being added to prevent.
	if !Exists(filepath.Join(root, "bin", BinaryName())) {
		return measurement{incomplete: "the build reported success but left no " + BinaryName() + ", so the Promise code was not checked"}, nil
	}
	output, _ := runCheck(root)
	summary := ParseCheckSummaryLine(output)
	if summary == nil {
		return measurement{}, fmt.Errorf("the Promise check printed no summary line, so nothing was measured")
	}
	// The errors are the findings: what `checked:go` counts for Go, and what
	// `checked` sums across both.
	return measurement{
		metrics: append(sharedCount("promise", "check_findings", summary.Errors),
			Count("promise_check_failures", summary.Failed),
			Count("promise_check_warnings", summary.Warnings),
			Count("promise_check_units", summary.Units),
		),
		groups: checkSweepGroups(output),
	}, nil
}

// checkUnitRe is a sweep's result line for a unit that did not check clean:
// `FAIL modules/std (18 errors)` or `warn tests/x (1 warning)`. The sweep
// prints the unit's diagnostics after it (runCheckSweep in
// compiler/cmd/promise/check.go — the same TWIN as checkSummaryRe).
var checkUnitRe = regexp.MustCompile(`^(FAIL|warn) (\S+) \(.*\)$`)

// checkDiagnosticRe is a diagnostic's first line, `file.pr:12:5: message`; the
// indented source excerpt under it belongs to it.
var checkDiagnosticRe = regexp.MustCompile(`^(\S+:\d+:\d+): (.*)$`)

// checkSweepGroups reads the sweep's output into a group per unit that did not
// check clean, with a member per diagnostic under its result line. A failing
// unit's diagnostics fail; a warned unit's are warnings it checked through, so
// they pass. A line under a unit that names no position — a module that would
// not load — is a member named for the unit, so no diagnostic goes unreported.
func checkSweepGroups(output string) []Group {
	var groups []Group
	var g *Group
	outcome := OutcomeFail
	var name string
	var lines []string
	flush := func() {
		if g != nil && name != "" {
			g.Members = append(g.Members, newMember(name, outcome, strings.Join(lines, "\n")))
		}
		name, lines = "", nil
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := checkUnitRe.FindStringSubmatch(line); m != nil {
			flush()
			groups = append(groups, Group{Name: m[2]})
			g = &groups[len(groups)-1]
			outcome = OutcomeFail
			if m[1] == "warn" {
				outcome = OutcomePass
			}
			continue
		}
		switch {
		case g == nil:
		case strings.HasPrefix(line, "ok ") || ParseCheckSummaryLine(line) != nil:
			// The unit's diagnostics end at the next result line or the summary.
			flush()
			g = nil
		case name != "" && strings.HasPrefix(line, " "):
			// The diagnostic's source excerpt is indented, and its first line may
			// be nothing but the indentation of an empty source line.
			lines = append(lines, line)
		case strings.TrimSpace(line) == "":
			flush()
		case checkDiagnosticRe.MatchString(line):
			flush()
			m := checkDiagnosticRe.FindStringSubmatch(line)
			name, lines = m[1], []string{m[2]}
		case name != "":
			lines = append(lines, line)
		default:
			name, lines = g.Name, []string{line}
		}
	}
	flush()
	return groups
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
func measureTestedGo(root string) (measurement, error) {
	// Here rather than in measureTestedGoWith, unlike the three measurements
	// above: that seam predates the build step and its tests drive it directly
	// with fixture roots, none of which is a tree anything could build.
	//
	// The build itself is not optional here — the compiler's own suite reads the
	// EMBEDDED std (testutil's stdAll), so without it the Go tests describe
	// whichever std was embedded last rather than the one in the tree.
	if err := ensureGateBuild(root); err != nil {
		return measurement{incomplete: buildDidNotComplete("the Go suites did not run", err)}, nil
	}
	// Teed, unlike the two sweeps above: this one takes minutes, and a run that
	// holds its output until the end is indistinguishable from a wedged one
	// while it is going — and then reports a COUNT of failures whose names are
	// in a buffer nobody prints. captureSplitTee says more about why.
	return measureTestedGoWith(root, captureGoTestJSON)
}

// measureTestedGoWith is measureTestedGo with the child call as a parameter. A
// test in this package cannot make the real one: `go test ./...` in tools/build
// IS this suite, so a test that ran it would spawn an unbounded chain of go test
// subprocesses (the hazard TestRunToolsGoTests_TrivialModule already names).
func measureTestedGoWith(root string, capture captureFunc) (measurement, error) {
	// The same command bin/verify runs (RunGoTests / RunToolsGoTests), from the
	// one place it is spelled: the gate and verify cannot disagree about what
	// "the Go suite" is. -json changes only what it prints, not what it runs,
	// and it is the one form that says which test a line belongs to: in -v
	// text, a test that runs `go test` on a fixture and prints what that child
	// said — its `--- FAIL:` lines included — is indistinguishable from a
	// failure of its own.
	argv := goTestArgs("-json")

	failedTests, failedPackages, passedTests := 0, 0, 0
	var groups []Group
	dirs, incomplete := GoModules(root)
	for _, dir := range dirs {
		stdout, stderr, testErr := capture(dir, "go", argv...)
		run := parseGoTestJSON(stdout)
		if testErr != nil && run.failedTests == 0 && run.failedPackages == 0 {
			// The run did not happen — go could not even list the packages. That
			// is not "zero failing tests"; a failing test, or a package that
			// failed to build, is reported in the stream the counts read.
			detail := firstRealLine(stderr)
			if detail == "" {
				detail = firstRealLine(stdout)
			}
			return measurement{}, fmt.Errorf("go test in %s: %w: %s", dir, testErr, detail)
		}
		failedTests += run.failedTests
		failedPackages += run.failedPackages
		passedTests += run.passedTests
		groups = append(groups, run.groups...)
	}
	var metrics []Metric
	metrics = append(metrics, sharedCount("go", "failed_tests", failedTests)...)
	metrics = append(metrics, sharedCount("go", "failed_packages", failedPackages)...)
	metrics = append(metrics, sharedCount("go", "test_count", passedTests)...)
	return measurement{metrics: metrics, groups: groups, incomplete: incomplete}, nil
}

// goTestEvent is one line of `go test -json` (cmd/test2json).
type goTestEvent struct {
	Action      string
	Package     string
	ImportPath  string // build-output and build-fail events name the package here
	Test        string
	Elapsed     float64
	Output      string
	FailedBuild string
}

// goTestRun is what one `go test -json` stream reports: top-level tests that
// passed and failed, packages that failed, and a group per package carrying a
// member for each failure in the tool's own words.
type goTestRun struct {
	passedTests, failedTests, failedPackages int
	groups                                   []Group
}

// parseGoTestJSON reads a `go test -json` stream. A test is counted at the top
// level only — a subtest's outcome is its parent's — and what a subtest printed
// is its parent's detail, so a failure carries the subtest that failed it. A
// test that started in a package that failed and never reported a result — it
// hung past -timeout, called os.Exit, or the runtime died under it — failed
// too, and is a member with what it printed: go reports no result for it, only
// the package's. A package that failed with no failing test of its own (it did
// not build, or its binary died outside any test) is a member too, under
// "(package)", carrying what go printed for it. A line that is not an event is
// not one of this stream's, and is skipped.
func parseGoTestJSON(stdout string) goTestRun {
	type pkgState struct {
		output, build strings.Builder
		tests         map[string]*strings.Builder
		started       []string        // top-level tests in the order they ran
		ended         map[string]bool // top-level tests that reported a result
		failed        bool
		failedBuild   string
		members       []Member
	}
	var run goTestRun
	var order []string
	pkgs := map[string]*pkgState{}
	pkg := func(name string) *pkgState {
		p, ok := pkgs[name]
		if !ok {
			p = &pkgState{tests: map[string]*strings.Builder{}, ended: map[string]bool{}}
			pkgs[name] = p
			order = append(order, name)
		}
		return p
	}
	for _, line := range strings.Split(stdout, "\n") {
		var e goTestEvent
		if json.Unmarshal([]byte(line), &e) != nil || e.Action == "" {
			continue
		}
		switch e.Action {
		case "build-output":
			pkg(e.ImportPath).build.WriteString(e.Output)
			continue
		case "build-fail":
			continue
		}
		p := pkg(e.Package)
		top, _, _ := strings.Cut(e.Test, "/")
		switch {
		case e.Action == "output" && e.Test == "":
			p.output.WriteString(e.Output)
		case e.Action == "output":
			b, ok := p.tests[top]
			if !ok {
				b = &strings.Builder{}
				p.tests[top] = b
			}
			b.WriteString(e.Output)
		case e.Test == "" && e.Action == "fail":
			run.failedPackages++
			p.failed, p.failedBuild = true, e.FailedBuild
		case e.Test == "":
			// The package's own pass or skip: no test result.
		case e.Test != top:
			// A subtest's own result: its parent reports the outcome.
		case e.Action == "run":
			p.started = append(p.started, top)
		case e.Action == "skip":
			p.ended[top] = true
		case e.Action == "pass":
			p.ended[top] = true
			run.passedTests++
		case e.Action == "fail":
			p.ended[top] = true
			run.failedTests++
			detail := ""
			if b := p.tests[top]; b != nil {
				detail = goTestFailureText(b.String())
			}
			m := newMember(top, OutcomeFail, detail)
			m.Elapsed = e.Elapsed
			p.members = append(p.members, m)
		}
	}
	for _, name := range order {
		p := pkgs[name]
		if name == "" {
			continue
		}
		members := p.members
		for _, top := range p.started {
			if !p.failed || p.ended[top] {
				continue
			}
			run.failedTests++
			said := ""
			if b := p.tests[top]; b != nil {
				said = b.String()
			}
			outcome := OutcomeFail
			if strings.Contains(said+p.output.String(), "panic: test timed out") {
				outcome = OutcomeTimeout
			}
			members = append(members, newMember(top, outcome, goTestFailureText(said+p.output.String())))
		}
		if p.failed && len(members) == 0 {
			detail := p.output.String()
			if p.failedBuild != "" {
				detail = pkg(p.failedBuild).build.String() + detail
			}
			members = append(members, newMember("(package)", OutcomeFail, detail))
		}
		if len(members) > 0 {
			run.groups = append(run.groups, Group{Name: goPkgToRepoRel(name), Members: members})
		}
	}
	return run
}

// goTestFailureText is what a failing test printed, without go's own framing
// lines: the "=== RUN" and "--- PASS:" lines, and the test's own "--- FAIL:",
// say which test, and the group and member already do. A failing subtest's
// "--- FAIL: TestTable/b" is kept: nothing else names which case failed.
func goTestFailureText(output string) string {
	var keep []string
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		failed, isFail := strings.CutPrefix(trimmed, "--- FAIL: ")
		failed, _, _ = strings.Cut(failed, " ")
		subtestFailed := isFail && strings.Contains(failed, "/")
		if strings.HasPrefix(trimmed, "=== ") || (strings.HasPrefix(trimmed, "--- ") && !subtestFailed) || trimmed == "" {
			continue
		}
		keep = append(keep, trimmed)
	}
	return strings.Join(keep, "\n")
}

// captureGoTestJSON is captureSplitTee for a `go test -json` child: the stream
// is captured whole for parseGoTestJSON, and what a person watching needs —
// the text the tests printed, not the events carrying it — is narrated as it
// arrives.
func captureGoTestJSON(dir, name string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	narration := Narration()
	text := &goTestNarrator{to: narration}
	cmd.Stdout = io.MultiWriter(text, &out)
	cmd.Stderr = io.MultiWriter(narration, &errBuf)
	err = cmd.Run()
	text.flush()
	return out.String(), errBuf.String(), err
}

// goTestNarrator writes the Output of each `go test -json` event it is given,
// whole lines at a time; a line that is not an event is written as it is.
type goTestNarrator struct {
	to      io.Writer
	pending []byte
}

func (n *goTestNarrator) Write(p []byte) (int, error) {
	n.pending = append(n.pending, p...)
	for {
		i := bytes.IndexByte(n.pending, '\n')
		if i < 0 {
			return len(p), nil
		}
		n.line(n.pending[:i])
		n.pending = n.pending[i+1:]
	}
}

func (n *goTestNarrator) flush() {
	if len(n.pending) > 0 {
		n.line(n.pending)
		n.pending = nil
	}
}

func (n *goTestNarrator) line(b []byte) {
	var e goTestEvent
	switch {
	case json.Unmarshal(b, &e) != nil:
		fmt.Fprintf(n.to, "%s\n", b)
	case e.Output != "":
		io.WriteString(n.to, e.Output)
	}
}

// measureTestedPromise runs the host Promise suite. There is no suite to
// measure without a compiler, and no correct suite to measure without the
// CURRENT one — so, like every other gate here, it measures behind the shared
// build (gate_build.go) rather than one of its own.
func measureTestedPromise(root string) (measurement, error) {
	// The --json stream, never the human one on stdout: RunPromiseTests forwards
	// the suite's own output to OUR stdout, which carries the envelope and
	// nothing else. The stream is one record per test, which is both the counts
	// and the evidence, read the same way for every target.
	return measureTestedPromiseWith(root, RunPromiseTestsJSON)
}

// suiteRunner is RunPromiseTestsJSON's shape as a parameter: the seam a test
// uses to stand in for a Promise suite, which takes minutes to run and needs a
// built compiler.
type suiteRunner func(root, target string) (string, error)

// measureTestedPromiseWith is measureTestedPromise with the suite as a
// parameter, so which record becomes which metric can be pinned without
// running eleven thousand tests to find out.
func measureTestedPromiseWith(root string, runSuite suiteRunner) (measurement, error) {
	if err := ensureGateBuild(root); err != nil {
		return measurement{incomplete: buildDidNotComplete("the Promise suite did not run", err)}, nil
	}
	jsonl, runErr := runSuite(root, "")
	return suiteMeasurement(root, "promise", "the Promise suite", jsonl, runErr)
}

// suiteFailedStatuses are the record statuses failed_tests counts: everything
// that is neither a pass, a leak (leaked_tests) nor a test excluded for this
// target.
var suiteFailedStatuses = []string{OutcomeFail, OutcomeTimeout, OutcomeMemory, OutcomeNotRun}

// suiteMeasurement reads one Promise suite's --json records — the host suite's
// or a cross-target one's — into its counts and its evidence: a group per test
// file, a member per record that did not pass. A stream with no records at all
// measured nothing, which is not zero failures.
func suiteMeasurement(root, toolchain, what, jsonl string, runErr error) (measurement, error) {
	out, err := BuildGateOutput(root, "", toolchain, "", jsonl)
	if err != nil {
		return measurement{}, fmt.Errorf("reading %s's records: %w", what, err)
	}
	failed, leaked, passed := 0, 0, 0
	for _, file := range out.Files {
		for _, r := range file.Tests {
			switch {
			case r.Status == OutcomePass:
				passed++
			case r.Status == OutcomeLeak:
				leaked++
			case slices.Contains(suiteFailedStatuses, r.Status):
				failed++
			}
		}
	}
	groups := recordGroups(out.Files)
	if len(out.Files) == 0 {
		if runErr != nil {
			return measurement{}, fmt.Errorf("%s reported no test records (%v), so nothing was measured", what, runErr)
		}
		return measurement{}, fmt.Errorf("%s reported no test records, so nothing was measured", what)
	}
	m := measurement{metrics: suiteMetrics(toolchain, failed, leaked, passed), groups: groups}
	// A runner stopped by a signal — the machine killed it for its memory, say
	// — reported only the tests it reached: counts that read like a smaller,
	// clean suite. A runner that exits non-zero on its own has said why in its
	// records.
	if stoppedBySignal(runErr) {
		m.incomplete = fmt.Sprintf("%s was stopped (%v) before it finished, so these counts cover only the tests it reported", what, runErr)
	}
	return m, nil
}

// stoppedBySignal reports whether err is a child that did not exit but was
// ended by a signal, so that what it reported is however far it had got.
func stoppedBySignal(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && !exit.Exited()
}

// recordGroups is a suite's evidence: a group per test file, a member per
// record that did not pass, in the runner's own words. A file every test of
// which passed has nothing to say about why, and adds no group.
func recordGroups(files []TestFileGroup) []Group {
	var groups []Group
	for _, file := range files {
		g := Group{Name: file.File}
		for _, r := range file.Tests {
			if r.Status == OutcomePass {
				continue
			}
			m := newMember(r.Test, r.Status, r.Context)
			m.Elapsed = r.Elapsed
			g.Members = append(g.Members, m)
		}
		if len(g.Members) > 0 {
			groups = append(groups, g)
		}
	}
	return groups
}

// suiteMetrics is what one Promise suite reports, under the names every suite
// shares and under the toolchain's own: the host suite's and each cross-target
// suite's, from one spelling.
func suiteMetrics(toolchain string, failed, leaked, passed int) []Metric {
	var metrics []Metric
	metrics = append(metrics, sharedCount(toolchain, "failed_tests", failed)...)
	metrics = append(metrics, sharedCount(toolchain, "leaked_tests", leaked)...)
	return append(metrics, sharedCount(toolchain, "test_count", passed)...)
}

// suiteSummarySpecs declares what suiteMetrics reports.
func suiteSummarySpecs(toolchain string) []MetricSpec {
	return sharedCountSpecs(toolchain, "failed_tests", "leaked_tests", "test_count")
}
