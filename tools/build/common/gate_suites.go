package common

// The gates that measure this project beyond the host integration path.
//
// Every one of them existed as a measurement before it existed as a contract
// gate — as one of bin/gate's older subcommands, run on a schedule for as long
// as there have been schedules. What they lacked was a NAME an orchestrator
// could discover. A gate reachable only by knowing its subcommand is a gate only
// whoever wrote the schedule can ask for, and `bin/gate --list` claiming the
// project has twelve gates while it measures nineteen is a listing that
// describes a different machine than the one that exists.
//
// Three of these names — `size`, `install`, `latest-invariant` — are outside
// flow's closed concept vocabulary, and that is not a defect: a project has
// gates the flow knows nothing about, and the SDK skips a name it does not
// recognise rather than refusing the listing (flow's gates-and-commands.md).
// They are listed because it is the listing's job to say what can be addressed.
//
// None of them is part of `integration`. That composition is what a landing
// decision waits on, and these are the measurements too slow to put on that
// path — the wasm suite alone is longer than the entire host suite. They stay
// separately runnable, which is the whole point of naming them.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// measureTestedWasm runs the Promise suite against wasm32-wasi.
//
// The runtime is a precondition rather than a result: a host that cannot obtain
// wasmtime has not been told anything about this tree, and reporting zero
// failures for a suite that never ran is the false clean result an incomplete
// reason exists to prevent.
func measureTestedWasm(root string) (measurement, error) {
	return measureTargetSuite(root, wasmSuite, RunPromiseTestsJSON)
}

// measureTestedWasmWeb runs the Promise suite against wasm32-web, under Node.
func measureTestedWasmWeb(root string) (measurement, error) {
	return measureTargetSuite(root, wasmWebSuite, RunPromiseTestsJSON)
}

// targetSuite names one cross-compiled suite: which target to build for, which
// runtime has to be obtainable to run it, and the toolchain prefix its metrics
// carry beside the names every suite shares (suiteMetrics).
type targetSuite struct {
	target  string
	runtime string
	prefix  string
}

// The two cross-target suites, as their gates measure and declare them.
var (
	wasmSuite    = targetSuite{target: "wasm32-wasi", runtime: "wasmtime", prefix: "wasm"}
	wasmWebSuite = targetSuite{target: "wasm32-web", runtime: "node", prefix: "wasm_web"}
)

// findRuntime obtains a target's runtime for this host. It is the one step in
// these measurements whose subject is the MACHINE rather than the tree, which
// is exactly why it is a seam: a test that stubs the runner precisely so it
// needs no toolchain must not then be decided by what this machine happens to
// carry (T2166, the same class as T2116).
//
// The product binding is EnsureWasmRuntime, so what it asks is no longer "does
// this host have one" but "can the PINNED one be staged" (T2169). The
// difference is the whole point: the first question is answered by whatever
// somebody installed, and a suite whose result turns on that describes the host
// as much as the tree.
var findRuntime = EnsureWasmRuntime

// measureTargetSuite is the body both cross-target suites share, with the
// runner as a parameter so a test can pin which record becomes which metric
// without a WASM toolchain or a four-minute suite.
func measureTargetSuite(root string, s targetSuite, runSuite suiteRunner) (measurement, error) {
	if _, err := findRuntime(root, s.runtime); err != nil {
		return measurement{incomplete: fmt.Sprintf(
			"the pinned %s could not be staged (%v), so the %s suite did not run", s.runtime, err, s.target)}, nil
	}
	if err := ensureGateBuild(root); err != nil {
		return measurement{incomplete: buildDidNotComplete("the "+s.target+" suite did not run", err)}, nil
	}
	// The record stream, read exactly as the host suite's is: the suite's own
	// human output on OUR stdout would sit in the middle of the envelope.
	jsonl, runErr := runSuite(root, s.target)
	return suiteMeasurement(root, s.prefix, "the "+s.target+" suite", jsonl, runErr)
}

// stressIterations is what one scheduled stress run costs. It is fixed here
// rather than taken as a flag because a contract gate's exec line is
// `bin/gate <name> --envelope` and nothing else: a measurement whose size the
// caller chooses is not comparable with the one before it, and the flaky count
// it reports would mean a different thing each run.
const stressIterations = "100"

// measureTestedStress re-runs the suite looking for tests that do not agree
// with themselves. The flaky count is the measurement; the iteration count
// travels with it because a flaky count means nothing without knowing how many
// chances a test had to show itself.
func measureTestedStress(root string) (measurement, error) {
	return measureTestedStressWith(root, func(root string) (string, error) {
		return RunCaptureStdout(root, filepath.Join(root, "bin", BinaryName()), "test", "-timeout", "15s",
			"-stress", stressIterations, "tests/...", "modules/...")
	})
}

// measureTestedStressWith is measureTestedStress with the stress run as a
// parameter, so which report field becomes which metric can be pinned without
// running the suite a hundred times.
func measureTestedStressWith(root string, runStress func(root string) (string, error)) (measurement, error) {
	if err := ensureGateBuild(root); err != nil {
		return measurement{incomplete: buildDidNotComplete("the stress run did not happen", err)}, nil
	}
	output, _ := runStress(root)
	iters, flaky := ParseStressOutput(output)
	if iters == 0 {
		return measurement{}, fmt.Errorf("the stress run reported no iterations, so nothing was measured")
	}
	return measurement{
		metrics: []Metric{
			Count("stress_flaky_count", flaky),
			Count("stress_iterations", iters),
		},
		groups: flakyTests(output),
	}, nil
}

// flakyTests reads the report's FLAKY block — a file per line indented two
// spaces, a test per line indented four with its pass rate and timings, and
// its failures indented six — into a group per file and a member per test.
// The block is the stress report's (writeTestGroupDetailed in
// compiler/cmd/promise/stress.go); it ends at the first blank line.
func flakyTests(output string) []Group {
	var groups []Group
	in := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case stressFlakyRe.MatchString(line):
			in = true
		case !in:
		case strings.TrimSpace(line) == "":
			in = false
		case strings.HasPrefix(line, "      ") && len(groups) > 0 && len(groups[len(groups)-1].Members) > 0:
			g := &groups[len(groups)-1]
			m := &g.Members[len(g.Members)-1]
			m.Detail = clampContext(m.Detail + "\n" + strings.TrimSpace(line))
		case strings.HasPrefix(line, "    ") && len(groups) > 0:
			name, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
			g := &groups[len(groups)-1]
			g.Members = append(g.Members, newMember(name, OutcomeFail, rest))
		case strings.HasPrefix(line, "  "):
			groups = append(groups, Group{Name: strings.TrimSpace(line)})
		}
	}
	return groups
}

// measureCovered reports how much of each language's source the suites reach.
func measureCovered(root string) (measurement, error) {
	return measureCoveredWith(root, goCoverageProfile, promiseCoverageStream)
}

// goCoverageProfile runs the Go suite instrumented over every package
// goCoveragePackages names, and returns `go tool cover -func` over the profile
// it wrote — or an error when no profile was produced.
//
// A failing test still produces a complete profile, so the profile is read
// whatever the suite returned: reporting zero for a run that measured fine is a
// false collapse indistinguishable from a real one, and the failures are
// already counted by `tested`.
func goCoverageProfile(root string) (string, error) {
	compilerDir := filepath.Join(root, "compiler")
	covPkgs, err := goCoveragePackages(compilerDir)
	if err != nil {
		return "", fmt.Errorf("listing the packages to measure: %w", err)
	}
	work, err := os.MkdirTemp("", "gate-covered-")
	if err != nil {
		return "", fmt.Errorf("making the coverage gate's scratch directory: %w", err)
	}
	defer os.RemoveAll(work)
	covFile := filepath.Join(work, "coverage.out")
	if _, err := RunCaptureStdout(compilerDir, "go", gateCoverageArgv(covPkgs, covFile)...); err != nil && !Exists(covFile) {
		return "", fmt.Errorf("the Go suite wrote no profile: %w", err)
	}
	return RunOutputIn(compilerDir, "go", "tool", "cover", "-func="+covFile)
}

// promiseCoverageStream runs the Promise suite with coverage and returns its
// --json record stream: one record per test and one coverage record per file.
func promiseCoverageStream(root string) (string, error) {
	return RunCaptureStdout(root, filepath.Join(root, "bin", BinaryName()), "test", "-json", "-coverage",
		"-timeout", "30", "tests/...", "modules/...")
}

// goCoverageTotalRe is the total line of `go tool cover -func`.
var goCoverageTotalRe = regexp.MustCompile(`(?m)^total:`)

// measureCoveredWith is measureCovered with both suites as parameters, so which
// profile becomes which metric — and what an absent one does — can be pinned
// without instrumenting two suites.
//
// A language's percentage is reported whenever its profile was produced, a real
// zero included: 0% of a language reached is a measurement, and leaving it out
// would make the run that reached nothing look like one that measured less.
func measureCoveredWith(root string, goCover, promiseCover func(root string) (string, error)) (measurement, error) {
	if err := ensureGateBuild(root); err != nil {
		return measurement{incomplete: buildDidNotComplete("coverage was not measured", err)}, nil
	}
	// A group per toolchain, carrying its own figure; a profile that was not
	// produced is a member saying why, in the tool's words.
	var m measurement
	goGroup := Group{Name: "go"}
	goOut, goErr := goCover(root)
	goProduced := goErr == nil && goCoverageTotalRe.MatchString(goOut)
	if goProduced {
		pct := sharedPercent("go", "statement_coverage", ParseCoverageTotal(goOut))
		m.metrics = append(m.metrics, pct...)
		goGroup.Metrics = pct[1:]
	} else {
		goGroup.Members = []Member{newMember("profile", OutcomeNotRun, errText(goErr, goOut))}
	}
	promiseGroup := Group{Name: "promise"}
	jsonl, promiseErr := promiseCover(root)
	_, total := promiseCoverageTotals(jsonl)
	promiseProduced := total > 0
	if promiseProduced {
		pct, _, _ := ParsePromiseCoverageJSONL(jsonl)
		shared := sharedPercent("promise", "block_coverage", pct)
		m.metrics = append(m.metrics, shared...)
		promiseGroup.Metrics = shared[1:]
	} else {
		promiseGroup.Members = []Member{newMember("profile", OutcomeNotRun, errText(promiseErr, "the suite reported no coverage records"))}
	}
	m.groups = []Group{goGroup, promiseGroup}
	switch {
	case !goProduced && !promiseProduced:
		return measurement{}, fmt.Errorf("neither suite produced a coverage profile, so nothing was measured")
	case !goProduced:
		m.incomplete = "the Go profile was not produced, so only Promise coverage is reported"
		if goErr != nil {
			m.incomplete += ": " + firstRealLine(goErr.Error())
		}
	case !promiseProduced:
		m.incomplete = "the Promise profile was not produced, so only Go coverage is reported"
	}
	return m, nil
}

// errText is err's text, or fallback when there is no error to quote.
func errText(err error, fallback string) string {
	if err != nil {
		return err.Error()
	}
	return fallback
}

// sizeCanaries is the canary source list size:wasm compiles: every
// tests/size/canary_*.pr, sorted. One list for the measurement and the
// declaration, so the gate cannot declare a canary it does not build, or build
// one it does not declare.
func sizeCanaries(root string) ([]string, error) {
	canaries, err := filepath.Glob(filepath.Join(root, "tests", "size", "canary_*.pr"))
	sort.Strings(canaries)
	return canaries, err
}

// canarySizeMetric is the metric one canary's size is reported under.
func canarySizeMetric(canary string) string {
	return "wasm_size_" + strings.TrimPrefix(strings.TrimSuffix(filepath.Base(canary), ".pr"), "canary_")
}

// sizeTotalMetric is the sum of every canary that compiled.
const sizeTotalMetric = "wasm_size_total"

// sizeWasmMetricSpecs declares size:wasm's metrics from the canaries the tree
// holds: one per canary, and the total.
func sizeWasmMetricSpecs(root string) []MetricSpec {
	canaries, _ := sizeCanaries(root)
	names := make([]string, 0, len(canaries)+1)
	for _, c := range canaries {
		names = append(names, canarySizeMetric(c))
	}
	return bytesSpecs(append(names, sizeTotalMetric)...)
}

// measureSizeWasm compiles the wasm32-wasi canaries and reports what each one
// costs. One target, named in the gate: a later instance measuring a native
// binary or a wasm32-web one is a sibling gate, not a change to this one.
//
// Per-canary rather than one total: a total that moved says only that something
// grew, and the canaries exist precisely to say WHICH thing — a program using
// only strings and one using the whole runtime answer different questions about
// the same change.
func measureSizeWasm(root string) (measurement, error) {
	promiseBin := filepath.Join(root, "bin", BinaryName())
	return measureSizeWasmWith(root, func(canary, wasm string) error {
		// The compiler's own words are the evidence for a canary that did not
		// compile, so its stderr is captured rather than streamed.
		_, stderr, err := captureSplit(root, promiseBin, "build", "-target", "wasm32-wasi", "-release", "-o", wasm, canary)
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr))
		}
		return nil
	})
}

// measureSizeWasmWith is measureSizeWasm with the compile as a parameter, so
// which canary becomes which metric can be pinned without a WASM toolchain.
func measureSizeWasmWith(root string, compile func(canary, wasm string) error) (measurement, error) {
	if err := ensureGateBuild(root); err != nil {
		return measurement{incomplete: buildDidNotComplete("no canary was compiled", err)}, nil
	}
	canaries, err := sizeCanaries(root)
	if err != nil {
		return measurement{}, fmt.Errorf("finding the size canaries: %w", err)
	}
	if len(canaries) == 0 {
		return measurement{}, fmt.Errorf("no size canaries found under tests/size, so nothing was measured")
	}

	outDir, err := os.MkdirTemp("", "gate-size-")
	if err != nil {
		return measurement{}, fmt.Errorf("making the size gate's scratch directory: %w", err)
	}
	defer os.RemoveAll(outDir)
	var metrics []Metric
	var groups []Group
	var total int64
	var failed []string
	for _, canary := range canaries {
		stem := strings.TrimSuffix(filepath.Base(canary), ".pr")
		wasm := filepath.Join(outDir, stem+".wasm")
		err := compile(canary, wasm)
		var info os.FileInfo
		if err == nil {
			info, err = os.Stat(wasm)
		}
		if err != nil {
			// A group per canary either way: one that did not compile carries
			// the compiler's error instead of a size.
			failed = append(failed, stem)
			groups = append(groups, Group{Name: stem, Members: []Member{newMember(relToRoot(root, canary), OutcomeFail, err.Error())}})
			continue
		}
		os.Remove(wasm)
		total += info.Size()
		size := Size(canarySizeMetric(canary), info.Size(), unitBytes)
		metrics = append(metrics, size)
		groups = append(groups, Group{Name: stem, Metrics: []Metric{size}})
	}
	if len(metrics) == 0 {
		return measurement{}, fmt.Errorf("every size canary failed to compile, so nothing was measured: %s", groups[0].Members[0].Detail)
	}
	metrics = append(metrics, Size(sizeTotalMetric, total, unitBytes))
	// A canary that did not compile is a fact about the tree, but the total it
	// is missing from is not comparable with the one before it — so the run is
	// honest about measuring less rather than letting a ratchet fall on it.
	var incomplete string
	if len(failed) > 0 {
		incomplete = "these canaries did not compile, and the total omits them: " + strings.Join(failed, ", ")
	}
	return measurement{metrics: metrics, groups: groups, incomplete: incomplete}, nil
}

// The two gates below measure something that is NOT this tree — a published
// release, and the channel that names it — so each reaches the network and one
// of them installs a toolchain. Both are therefore seams: a unit test in this
// package must be able to ask what the gate reports without downloading a
// release, and `go test ./...` is itself what `tested:go` measures.
var (
	installPhases = runInstallPhases
	latestIsEpoch = assertLatestIsEpoch
)

// measureInstall is the end-to-end install of a published release of variant,
// reporting what each phase cost.
//
// The variant is in the NAME rather than a flag, because a contract gate takes
// none: the exec lines are `bin/gate install:thin --envelope` and
// `bin/gate install:full --envelope`.
func measureInstall(variant string) func(root string) (measurement, error) {
	return func(root string) (measurement, error) { return measureInstallVariant(root, variant) }
}

func measureInstallVariant(root, variant string) (measurement, error) {
	work, err := os.MkdirTemp("", "gate-install-")
	if err != nil {
		return measurement{}, fmt.Errorf("making the install gate's scratch directory: %w", err)
	}
	defer os.RemoveAll(work)

	// The phase error is deliberately not returned: a failed install is a
	// MEASUREMENT of this release, reported in the phase metrics the aggregator
	// reads, and a gate that errored would say the measurement could not be
	// obtained instead.
	phaseErr := installPhases(root, work, variant, defaultGateChannel, false)
	out, err := buildInstallGateOutput(HostTarget(), variant, work)
	if err != nil {
		return measurement{}, fmt.Errorf("reading what the install phases recorded: %w", err)
	}
	metrics := make([]Metric, 0, len(out.Metrics))
	for _, name := range sortedMapKeys(out.Metrics) {
		metrics = append(metrics, Count(name, int(out.Metrics[name])))
	}
	if len(metrics) == 0 {
		return measurement{}, fmt.Errorf("the install phases recorded no metrics, so nothing was measured")
	}
	// The phases first — the one that failed carries its error, and the ones
	// after it never ran — then a group per test file the installed suite ran.
	phases := Group{Name: "phases"}
	for _, p := range installPhasesFor(variant) {
		if out.Metrics[installPhaseMetric(variant, p)] == 1 {
			continue
		}
		if phaseErr != nil {
			phases.Members = append(phases.Members, newMember(p, OutcomeFail, phaseErr.Error()))
			phaseErr = nil
			continue
		}
		phases.Members = append(phases.Members, newMember(p, OutcomeNotRun, ""))
	}
	groups := []Group{phases}
	for _, file := range out.Files {
		g := Group{Name: file.File}
		for _, r := range file.Tests {
			if r.Status != OutcomePass {
				m := newMember(r.Test, r.Status, r.Context)
				m.Elapsed = r.Elapsed
				g.Members = append(g.Members, m)
			}
		}
		groups = append(groups, g)
	}
	return measurement{metrics: metrics, groups: groups}, nil
}

// measureLatestInvariant checks that `releases/latest` resolves to an epoch-*
// release (T1493).
//
// Its subject is the published release channel rather than this tree, so it
// does not build first — and unlike every other gate here it asks a question
// with two answers, which is reported as a number because a gate reports
// numbers: 1 holds, 0 does not.
func measureLatestInvariant(string) (measurement, error) {
	if err := latestIsEpoch(); err != nil {
		// The reason belongs where a person will read it — narrated, and in the
		// envelope's evidence, which names the tag `latest` resolved to. The
		// metric is the machine's half of the same answer.
		fmt.Fprintf(Narration(), "latest-invariant: %v\n", err)
		return measurement{
			metrics: []Metric{Count("latest_is_epoch", 0)},
			groups:  []Group{{Name: "releases/latest", Members: []Member{newMember("latest", OutcomeFail, err.Error())}}},
		}, nil
	}
	return measurement{metrics: []Metric{Count("latest_is_epoch", 1)}}, nil
}
