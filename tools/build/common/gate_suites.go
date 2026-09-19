package common

// The gates that measure this project beyond the host integration path.
//
// Every one of them existed as a measurement before it existed as a contract
// gate: `bin/gate wasm-test`, `stress`, `coverage`, `wasm-size`, `install` and
// `latest-invariant` have been run on a schedule by the tracker for as long as
// there have been schedules. What they lacked was a NAME an orchestrator could
// discover. A gate reachable only by knowing its subcommand is a gate only
// whoever wrote the schedule can ask for, and `bin/gate --list` claiming the
// project has twelve gates while it measures nineteen is a listing that
// describes a different machine than the one that exists.
//
// Three of these names — `size`, `install`, `latest-invariant` — are outside
// flow's closed concept vocabulary, and that is not a defect: a project has
// gates the flow knows nothing about, and the SDK skips a name it does not
// recognise rather than refusing the listing (flow's gates-and-commands.md).
// They are listed because the tracker addresses them, and it is the listing's
// job to say what can be addressed.
//
// None of them is part of `integration`. That composition is what a landing
// decision waits on, and these are the measurements too slow to put on that
// path — the wasm suite alone is longer than the entire host suite. They stay
// separately runnable, which is the whole point of naming them.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// measureTestedWasm runs the Promise suite against wasm32-wasi.
//
// The runtime is a precondition rather than a result: a host with no wasmtime
// has not been told anything about this tree, and reporting zero failures for a
// suite that never ran is the false clean result an incomplete reason exists to
// prevent.
func measureTestedWasm(root string) ([]Metric, string, error) {
	return measureTargetSuite(root, targetSuite{
		target:  "wasm32-wasi",
		runtime: "wasmtime",
		prefix:  "wasm",
	}, RunPromiseTestsCapture)
}

// measureTestedWasmWeb runs the Promise suite against wasm32-web, under Node.
func measureTestedWasmWeb(root string) ([]Metric, string, error) {
	return measureTargetSuite(root, targetSuite{
		target:  "wasm32-web",
		runtime: "node",
		prefix:  "wasm_web",
	}, RunPromiseTestsCapture)
}

// targetSuite names one cross-compiled suite: which target to build for, which
// host program has to be present to run it, and the prefix its metrics carry.
// The prefix is the baselines' spelling, not a new one — these metrics have
// been ratcheted per platform for as long as the legacy subcommands have
// written them, and a gate reporting the same measurement under a new name
// would start from no history at all.
type targetSuite struct {
	target  string
	runtime string
	prefix  string
}

// findRuntime answers whether this host carries a target's runtime. It is the
// one question in these measurements whose subject is the MACHINE rather than
// the tree, which is exactly why it is a seam: a test that stubs the runner
// precisely so it needs no toolchain must not then be decided by what this
// machine happens to have on PATH (T2166, the same class as T2116). The
// product binding is Which — the probe itself is legitimate: it gates on a
// runtime that EXECUTES a built artifact without contributing to it, which
// docs/code-style.md §"Host tools in Go sources" names as one of the three
// lookups that hold, and docs/gate-system.md says `tested:wasm` needs wasmtime
// present.
var findRuntime = Which

// measureTargetSuite is the body both cross-target suites share, with the
// runner as a parameter so a test can pin which summary field becomes which
// metric without a WASM toolchain or a four-minute suite.
func measureTargetSuite(root string, s targetSuite, runSuite suiteRunner) ([]Metric, string, error) {
	if findRuntime(s.runtime) == "" {
		return []Metric{}, fmt.Sprintf(
			"%s is not installed, so the %s suite did not run", s.runtime, s.target), nil
	}
	if err := ensureGateBuild(root); err != nil {
		return []Metric{}, buildDidNotComplete("the "+s.target+" suite did not run", err), nil
	}
	// Capture rather than tee: the suite's own output on OUR stdout would sit
	// in the middle of the envelope and make the whole stream unparseable,
	// which the runner reports as this gate breaking its contract.
	output, _ := runSuite(root, s.target)
	summary := ParseTestSummaryLine(output)
	if summary == nil {
		return nil, "", fmt.Errorf("the %s suite printed no summary line, so nothing was measured", s.target)
	}
	return []Metric{
		Count(s.prefix+"_test_failures", summary.Failed),
		Count(s.prefix+"_leak_count", summary.Leaked),
		Count(s.prefix+"_test_count", summary.Passed),
	}, "", nil
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
func measureTestedStress(root string) ([]Metric, string, error) {
	if err := ensureGateBuild(root); err != nil {
		return []Metric{}, buildDidNotComplete("the stress run did not happen", err), nil
	}
	promiseBin := filepath.Join(root, "bin", BinaryName())
	output, _ := RunCaptureStdout(root, promiseBin, "test", "-timeout", "15s",
		"-stress", stressIterations, "tests/...", "modules/...")
	iters, flaky := ParseStressOutput(output)
	if iters == 0 {
		return nil, "", fmt.Errorf("the stress run reported no iterations, so nothing was measured")
	}
	return []Metric{
		Count("stress_flaky_count", flaky),
		Count("stress_iterations", iters),
	}, "", nil
}

// measureCovered reports how much of each language's source the suites reach.
//
// A failing test still produces a complete profile, so the percentage is read
// whatever the suites returned: reporting zero for a run that measured fine is
// a false collapse indistinguishable from a real one, and the failures are
// already counted by `tested`.
func measureCovered(root string) ([]Metric, string, error) {
	if err := ensureGateBuild(root); err != nil {
		return []Metric{}, buildDidNotComplete("coverage was not measured", err), nil
	}
	compilerDir := filepath.Join(root, "compiler")
	covPkgs, err := goCoveragePackages(compilerDir)
	if err != nil {
		return nil, "", fmt.Errorf("listing the packages to measure: %w", err)
	}
	work, err := os.MkdirTemp("", "gate-covered-")
	if err != nil {
		return nil, "", fmt.Errorf("making the coverage gate's scratch directory: %w", err)
	}
	defer os.RemoveAll(work)
	covFile := filepath.Join(work, "coverage.out")

	var goPct float64
	if _, err := RunCaptureStdout(compilerDir, "go", gateCoverageArgv(covPkgs, covFile)...); err == nil || Exists(covFile) {
		if out, err := RunOutputIn(compilerDir, "go", "tool", "cover", "-func="+covFile); err == nil {
			goPct = ParseCoverageTotal(out)
		}
	}

	promiseBin := filepath.Join(root, "bin", BinaryName())
	jsonl, _ := RunCaptureStdout(root, promiseBin, "test", "-json", "-coverage",
		"-timeout", "30", "tests/...", "modules/...")
	promisePct, _, _ := ParsePromiseCoverageJSONL(jsonl)

	var incomplete string
	switch {
	case goPct == 0 && promisePct == 0:
		return nil, "", fmt.Errorf("neither suite produced a coverage profile, so nothing was measured")
	case goPct == 0:
		incomplete = "the Go profile was not produced, so only Promise coverage is reported"
	case promisePct == 0:
		incomplete = "the Promise profile was not produced, so only Go coverage is reported"
	}
	var out []Metric
	if goPct > 0 {
		out = append(out, Percent("go_coverage_pct", goPct))
	}
	if promisePct > 0 {
		out = append(out, Percent("promise_coverage_pct", promisePct))
	}
	return out, incomplete, nil
}

// measureSizeWasm compiles the wasm32-wasi canaries and reports what each one
// costs. One target, named in the gate: a later instance measuring a native
// binary or a wasm32-web one is a sibling gate, not a change to this one.
//
// Per-canary rather than one total: a total that moved says only that something
// grew, and the canaries exist precisely to say WHICH thing — a program using
// only strings and one using the whole runtime answer different questions about
// the same change.
func measureSizeWasm(root string) ([]Metric, string, error) {
	if err := ensureGateBuild(root); err != nil {
		return []Metric{}, buildDidNotComplete("no canary was compiled", err), nil
	}
	canaries, err := filepath.Glob(filepath.Join(root, "tests", "size", "canary_*.pr"))
	if err != nil {
		return nil, "", fmt.Errorf("finding the size canaries: %w", err)
	}
	if len(canaries) == 0 {
		return nil, "", fmt.Errorf("no size canaries found under tests/size, so nothing was measured")
	}
	sort.Strings(canaries)

	outDir, err := os.MkdirTemp("", "gate-size-")
	if err != nil {
		return nil, "", fmt.Errorf("making the size gate's scratch directory: %w", err)
	}
	defer os.RemoveAll(outDir)
	promiseBin := filepath.Join(root, "bin", BinaryName())
	var metrics []Metric
	var total int64
	var failed []string
	for _, canary := range canaries {
		stem := strings.TrimSuffix(filepath.Base(canary), ".pr")
		wasm := filepath.Join(outDir, stem+".wasm")
		if _, err := RunOutputIn(root, promiseBin,
			"build", "-target", "wasm32-wasi", "-release", "-o", wasm, canary); err != nil {
			failed = append(failed, stem)
			continue
		}
		info, err := os.Stat(wasm)
		if err != nil {
			failed = append(failed, stem)
			continue
		}
		os.Remove(wasm)
		total += info.Size()
		// Built rather than spelled: one metric per canary file on disk. The
		// names this produces are listed in integrationMetricsUnjudged, which
		// is where a reader finds them — the guard that scans this package for
		// metric names can only see literals, and a literal prefix here would
		// read to it as a metric called "wasm_size_".
		metric := "wasm_size_" + strings.TrimPrefix(stem, "canary_")
		metrics = append(metrics, Size(metric, info.Size(), "bytes"))
	}
	if len(metrics) == 0 {
		return nil, "", fmt.Errorf("every size canary failed to compile, so nothing was measured")
	}
	metrics = append(metrics, Size("wasm_size_total", total, "bytes"))
	// A canary that did not compile is a fact about the tree, but the total it
	// is missing from is not comparable with the one before it — so the run is
	// honest about measuring less rather than letting a ratchet fall on it.
	var incomplete string
	if len(failed) > 0 {
		incomplete = "these canaries did not compile, and the total omits them: " + strings.Join(failed, ", ")
	}
	return metrics, incomplete, nil
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

// measureInstallThin runs the end-to-end install of a published thin release
// and reports what each phase cost.
//
// The variant is in the NAME rather than a flag, because a contract gate takes
// none: the exec line is `bin/gate install:thin --envelope`. `full` is a
// sibling gate whenever someone wants it scheduled; until then it stays the
// `--variant full` subcommand.
func measureInstallThin(root string) ([]Metric, string, error) {
	work, err := os.MkdirTemp("", "gate-install-")
	if err != nil {
		return nil, "", fmt.Errorf("making the install gate's scratch directory: %w", err)
	}
	defer os.RemoveAll(work)

	// The phase error is deliberately not returned: a failed install is a
	// MEASUREMENT of this release, reported in the phase metrics the aggregator
	// reads, and a gate that errored would say the measurement could not be
	// obtained instead.
	_ = installPhases(root, work, "thin", defaultGateChannel, false)
	out, err := buildInstallGateOutput(HostTarget(), "thin", work)
	if err != nil {
		return nil, "", fmt.Errorf("reading what the install phases recorded: %w", err)
	}
	metrics := make([]Metric, 0, len(out.Metrics))
	for _, name := range sortedMapKeys(out.Metrics) {
		metrics = append(metrics, Count(name, int(out.Metrics[name])))
	}
	if len(metrics) == 0 {
		return nil, "", fmt.Errorf("the install phases recorded no metrics, so nothing was measured")
	}
	return metrics, "", nil
}

// measureLatestInvariant checks that `releases/latest` resolves to an epoch-*
// release (T1493).
//
// Its subject is the published release channel rather than this tree, so it
// does not build first — and unlike every other gate here it asks a question
// with two answers, which is reported as a number because a gate reports
// numbers: 1 holds, 0 does not.
func measureLatestInvariant(string) ([]Metric, string, error) {
	if err := latestIsEpoch(); err != nil {
		// The reason belongs where a person will read it. The metric is the
		// machine's half of the same answer.
		fmt.Fprintf(os.Stderr, "latest-invariant: %v\n", err)
		return []Metric{Count("latest_is_epoch", 0)}, "", nil
	}
	return []Metric{Count("latest_is_epoch", 1)}, "", nil
}
