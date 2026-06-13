package common

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// RunGate dispatches bin/gate subcommands. Subcommands output structured JSON
// gate values to stdout; progress messages go to stderr.
func RunGate(root string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: bin/gate <subcommand> [flags]\nSubcommands:\n  test        run Promise tests and output JSON gate values\n  wasm-test   run only WASM target tests and output JSON gate values\n  wasm-size   compile WASM canaries and report binary sizes\n  go-test     run Go tests and output JSON gate values\n  stress      run stress tests and output JSON gate values\n  coverage    run coverage analysis and output JSON gate values\n  install     run the end-to-end install gate (--variant {thin|full} [--system])\n  schema      print the test-output JSON schema (see docs/gate-system.md)")
	}
	switch args[0] {
	case "test":
		return runGateTest(root, args[1:])
	case "install":
		return runGateInstall(root, args[1:])
	case "wasm-test":
		return runGateWasmTests(root, args[1:])
	case "wasm-size":
		return runGateWasmSize(root, args[1:])
	case "go-test":
		return runGateGoTest(root, args[1:])
	case "stress":
		return runGateStress(root, args[1:])
	case "coverage":
		return runGateCoverage(root, args[1:])
	case "schema":
		return runGateSchema()
	default:
		return fmt.Errorf("unknown subcommand %q\nSubcommands: test, wasm-test, wasm-size, go-test, stress, coverage, install, schema", args[0])
	}
}

// runGateSchema prints the test-output JSON schema so the tracker (or anyone
// ingesting the gate output) can read the contract without running a gate. The
// contract is embedded (GateOutputSchema) rather than read from a doc file, so
// the command is independent of the working directory and never depends on a
// doc path existing on disk. The human-facing narrative lives in
// docs/gate-system.md ("Gate Output Schema"). T0763.
func runGateSchema() error {
	fmt.Print(GateOutputSchema)
	return nil
}

// runGateTest runs Promise tests and writes structured JSON gate values to stdout.
// Test progress is written to stderr so stdout is clean JSON.
func runGateTest(root string, args []string) error {
	args = NormalizeArgs(args)
	shared := slices.Contains(args, "-shared")

	// bin/gate test is host-only. wasm tests are `bin/gate wasm-test` — a
	// separate single-target gate. Reject any unknown argument (incl. -wasm).
	for _, arg := range args {
		switch arg {
		case "-shared", "-local":
		default:
			return fmt.Errorf("usage: bin/gate test [-shared] (use `bin/gate wasm-test` for wasm)")
		}
	}

	if !shared {
		if err := SetupLocalCache(root); err != nil {
			return fmt.Errorf("setup local cache: %w", err)
		}
	}

	// Build compiler first. Redirect stdout→stderr so build progress lines
	// don't contaminate the JSON we emit to stdout at the end.
	savedStdout := os.Stdout
	os.Stdout = os.Stderr
	buildErr := RunBuild(root, nil)
	os.Stdout = savedStdout
	if buildErr != nil {
		return fmt.Errorf("build: %w", buildErr)
	}

	hostTarget := strings.ToLower(runtime.GOOS) + "-" + runtime.GOARCH

	// Run host tests with --json. The runner streams one JSON record per
	// eligible test to stdout (captured here) and human progress to stderr.
	// T0763: this stream is the single authoritative source of test identity —
	// no human-output scraping.
	fmt.Fprintf(os.Stderr, "Running promise tests (%s)...\n", hostTarget)
	hostJSONL, hostErr := RunPromiseTestsJSON(root, "")

	// Build the single-target host envelope (relativizes file paths against the
	// repo root, groups by file, derives metrics from the records).
	out, err := BuildGateOutput(root, hostTarget, "host", "promise-tests", hostJSONL)
	if err != nil {
		return fmt.Errorf("build gate output: %w", err)
	}

	// Write gate-values.json sidecar so bin/commitgate can read the metrics.
	gv := &GateValues{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Platform:  hostTarget,
		Values:    out.Metrics,
	}
	if err := WriteGateValues(root, gv); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write gate values: %v\n", err)
	}

	// Output the two-level GateOutput JSON to stdout (machine-readable).
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gate output: %w", err)
	}
	fmt.Println(string(data))

	if hostErr != nil {
		return fmt.Errorf("promise tests (host) failed")
	}
	return nil
}

// runGateWasmTests runs only WASM target tests and writes structured JSON gate values to stdout.
// Unlike "test --wasm", this does not run host-target tests.
func runGateWasmTests(root string, args []string) error {
	args = NormalizeArgs(args)
	shared := slices.Contains(args, "-shared")

	for _, arg := range args {
		switch arg {
		case "-shared", "-local":
		default:
			return fmt.Errorf("usage: bin/gate wasm-test [-shared]")
		}
	}

	if Which("wasmtime") == "" {
		return fmt.Errorf("wasmtime not found — install with: bin/prereqs -wasm")
	}

	if !shared {
		if err := SetupLocalCache(root); err != nil {
			return fmt.Errorf("setup local cache: %w", err)
		}
	}

	// Build compiler first. Redirect stdout→stderr so build progress lines
	// don't contaminate the JSON we emit to stdout at the end.
	savedStdout := os.Stdout
	os.Stdout = os.Stderr
	buildErr := RunBuild(root, nil)
	os.Stdout = savedStdout
	if buildErr != nil {
		return fmt.Errorf("build: %w", buildErr)
	}

	hostTarget := strings.ToLower(runtime.GOOS) + "-" + runtime.GOARCH

	// Run wasm tests with --json (single-target wasm gate). The runner streams
	// one JSON record per eligible test to stdout; human progress to stderr.
	fmt.Fprintf(os.Stderr, "Running promise tests (wasm32-wasi)...\n")
	wasmJSONL, wasmErr := RunPromiseTestsJSON(root, "wasm32-wasi")

	// Build the single-target wasm envelope.
	out, err := BuildGateOutput(root, "wasm32-wasi", "wasm", "wasm-test", wasmJSONL)
	if err != nil {
		return fmt.Errorf("build gate output: %w", err)
	}

	// Write gate-values.json sidecar so bin/commitgate can read the metrics.
	// Platform is the host (where the gate ran); the metrics carry the wasm_
	// prefix and the envelope's target records the test target.
	gv := &GateValues{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Platform:  hostTarget,
		Values:    out.Metrics,
	}
	if err := WriteGateValues(root, gv); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write gate values: %v\n", err)
	}

	// Output the two-level GateOutput JSON to stdout (machine-readable).
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gate output: %w", err)
	}
	fmt.Println(string(data))

	if wasmErr != nil {
		return fmt.Errorf("promise tests (wasm32-wasi) failed")
	}
	return nil
}

// runGateGoTest runs Go unit tests and writes structured JSON gate values to stdout.
func runGateGoTest(root string, args []string) error {
	args = NormalizeArgs(args)
	shared := slices.Contains(args, "-shared")

	for _, arg := range args {
		switch arg {
		case "-shared", "-local":
		default:
			return fmt.Errorf("usage: bin/gate go-test [-shared]")
		}
	}

	if !shared {
		if err := SetupLocalCache(root); err != nil {
			return fmt.Errorf("setup local cache: %w", err)
		}
	}

	// Build compiler first (stdout→stderr).
	savedStdout := os.Stdout
	os.Stdout = os.Stderr
	buildErr := RunBuild(root, nil)
	os.Stdout = savedStdout
	if buildErr != nil {
		return fmt.Errorf("build: %w", buildErr)
	}

	hostTarget := strings.ToLower(runtime.GOOS) + "-" + runtime.GOARCH
	compilerDir := filepath.Join(root, "compiler")

	fmt.Fprintf(os.Stderr, "Running go tests...\n")
	output, testErr := RunTeeStderr(compilerDir, "go", "test", "-v", "-count=1", "./...")

	passed, failed := ParseGoTestOutput(output)
	goFiles := ParseGoTestGroups(output)

	gv := &GateValues{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Platform:  hostTarget,
		Values:    make(map[string]float64),
	}
	gv.Values["go_test_count"] = float64(passed)
	gv.Values["go_test_failures"] = float64(failed)

	if err := WriteGateValues(root, gv); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write gate values: %v\n", err)
	}

	// Output the two-level GateOutput JSON to stdout (machine-readable).
	out := &GateOutput{Target: hostTarget, Metrics: gv.Values, Files: goFiles, Complete: "go-tests"}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gate output: %w", err)
	}
	fmt.Println(string(data))

	if testErr != nil {
		return fmt.Errorf("go tests failed")
	}
	return nil
}

// runGateStress runs stress tests and writes structured JSON gate values to stdout.
func runGateStress(root string, args []string) error {
	iterations := "100"
	for _, arg := range args {
		if _, err := strconv.Atoi(arg); err == nil {
			iterations = arg
		} else {
			return fmt.Errorf("usage: bin/gate stress [iterations]")
		}
	}

	if err := SetupLocalCache(root); err != nil {
		return fmt.Errorf("setup local cache: %w", err)
	}

	// Build compiler first (stdout→stderr).
	savedStdout := os.Stdout
	os.Stdout = os.Stderr
	buildErr := RunBuild(root, nil)
	os.Stdout = savedStdout
	if buildErr != nil {
		return fmt.Errorf("build: %w", buildErr)
	}

	hostTarget := strings.ToLower(runtime.GOOS) + "-" + runtime.GOARCH
	promiseBin := filepath.Join(root, "bin", BinaryName())

	fmt.Fprintf(os.Stderr, "Running stress tests (%s iterations)...\n", iterations)
	output, testErr := RunTeeStderr(root, promiseBin, "test", "-timeout", "15s", "-stress", iterations, "tests/...", "modules/...")

	iters, flakyCount := ParseStressOutput(output)

	gv := &GateValues{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Platform:  hostTarget,
		Values:    make(map[string]float64),
	}
	gv.Values["stress_iterations"] = float64(iters)
	gv.Values["stress_flaky_count"] = float64(flakyCount)

	if err := WriteGateValues(root, gv); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write gate values: %v\n", err)
	}

	// Output the unified GateOutput envelope (metric-only; no files).
	out := &GateOutput{Target: hostTarget, Metrics: gv.Values, Complete: "stress"}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gate output: %w", err)
	}
	fmt.Println(string(data))

	if testErr != nil {
		return fmt.Errorf("stress tests failed")
	}
	return nil
}

// runGateCoverage runs Go and Promise coverage analysis and writes structured JSON gate values.
func runGateCoverage(root string, args []string) error {
	args = NormalizeArgs(args)
	shared := slices.Contains(args, "-shared")

	for _, arg := range args {
		switch arg {
		case "-shared", "-local":
		default:
			return fmt.Errorf("usage: bin/gate coverage [-shared]")
		}
	}

	if !shared {
		if err := SetupLocalCache(root); err != nil {
			return fmt.Errorf("setup local cache: %w", err)
		}
	}

	// Build compiler first (stdout→stderr).
	savedStdout := os.Stdout
	os.Stdout = os.Stderr
	buildErr := RunBuild(root, nil)
	os.Stdout = savedStdout
	if buildErr != nil {
		return fmt.Errorf("build: %w", buildErr)
	}

	hostTarget := strings.ToLower(runtime.GOOS) + "-" + runtime.GOARCH
	compilerDir := filepath.Join(root, "compiler")
	promiseBin := filepath.Join(root, "bin", BinaryName())

	// Go coverage
	fmt.Fprintf(os.Stderr, "Running Go coverage...\n")
	covFile := filepath.Join(os.TempDir(), "promise_gate_cov.out")
	defer os.Remove(covFile)

	var goCovPct float64
	_, goTestErr := RunTeeStderr(compilerDir, "go", "test", "-coverprofile="+covFile, "-count=1", "./...")
	if goTestErr == nil && Exists(covFile) {
		coverOutput, coverErr := RunOutputIn(compilerDir, "go", "tool", "cover", "-func="+covFile)
		if coverErr == nil {
			goCovPct = ParseCoverageTotal(coverOutput)
		}
	}

	// Promise coverage
	fmt.Fprintf(os.Stderr, "Running Promise coverage...\n")
	promiseOutput, _ := RunTeeStderr(root, promiseBin, "test", "-coverage", "-timeout", "30", "tests/...", "modules/...")
	promiseCovPct := ParseCoverageTotal(promiseOutput)

	gv := &GateValues{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Platform:  hostTarget,
		Values:    make(map[string]float64),
	}
	gv.Values["go_coverage_pct"] = goCovPct
	gv.Values["promise_coverage_pct"] = promiseCovPct

	if err := WriteGateValues(root, gv); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write gate values: %v\n", err)
	}

	// Output the unified GateOutput envelope (metric-only; no files).
	out := &GateOutput{Target: hostTarget, Metrics: gv.Values, Complete: "coverage"}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gate output: %w", err)
	}
	fmt.Println(string(data))

	return nil
}

// --- Parser helpers ---

var goTestPassRe = regexp.MustCompile(`^--- PASS:`)
var goTestFailRe = regexp.MustCompile(`^--- FAIL:`)

// ParseGoTestOutput counts passed and failed tests from `go test -v` output.
func ParseGoTestOutput(output string) (passed, failed int) {
	for _, line := range strings.Split(output, "\n") {
		if goTestPassRe.MatchString(line) {
			passed++
		} else if goTestFailRe.MatchString(line) {
			failed++
		}
	}
	return
}

var goTestResultRe = regexp.MustCompile(`^--- (PASS|FAIL|SKIP): (\S+) \((\d+\.\d+)s\)`)
var goTestPkgRe = regexp.MustCompile(`^(ok|FAIL)\s+(\S+)\s+`)

// goModulePrefix is the repository's VCS-root import prefix. Go package paths
// are import paths (e.g. github.com/promise-language/promise/compiler/internal/
// codegen); stripping this yields the repo-relative directory (compiler/
// internal/codegen), matching the file identity used for Promise tests so the
// tracker sees one path convention across all gates. T0763.
const goModulePrefix = "github.com/promise-language/promise/"

// goPkgToRepoRel strips the repo module prefix from a Go import path, leaving a
// repo-relative directory. Paths without the prefix (e.g. third-party) are kept.
func goPkgToRepoRel(pkg string) string {
	if rel, ok := strings.CutPrefix(pkg, goModulePrefix); ok {
		return rel
	}
	return pkg
}

// ParseGoTestGroups parses `go test -v` output into per-package file groups
// (repo-relative path) with one TestRecord per test function, so go-test emits
// the same two-level shape as the Promise test gates. Status is normalized to
// the gate vocabulary: pass | fail | excluded (Go SKIP). T0763.
func ParseGoTestGroups(output string) []TestFileGroup {
	lines := strings.Split(output, "\n")
	var groups []TestFileGroup
	var pending []TestRecord
	var contextLines []string
	collectingContext := false

	flushContext := func() {
		if collectingContext && len(pending) > 0 {
			// Bound the context so a failing test that dumps the full IR
			// (assertContains) can't produce a >1MB JSON line that deadlocks
			// the gate runner's drain — see clampContext (T0777).
			pending[len(pending)-1].Context = clampContext(strings.Join(contextLines, "\n"))
		}
		collectingContext = false
		contextLines = nil
	}

	for _, line := range lines {
		// Package summary line — close the group with its package path.
		if m := goTestPkgRe.FindStringSubmatch(line); m != nil {
			flushContext()
			if len(pending) > 0 {
				groups = append(groups, TestFileGroup{
					File:  goPkgToRepoRel(m[2]),
					Tests: append([]TestRecord(nil), pending...),
				})
				pending = pending[:0]
			}
			continue
		}

		// Test result line.
		if m := goTestResultRe.FindStringSubmatch(line); m != nil {
			flushContext()
			status := "pass"
			switch m[1] {
			case "FAIL":
				status = "fail"
			case "SKIP":
				status = "excluded"
			}
			elapsed, _ := strconv.ParseFloat(m[3], 64)
			pending = append(pending, TestRecord{Test: m[2], Status: status, Elapsed: elapsed})
			if m[1] == "FAIL" {
				collectingContext = true
				contextLines = nil
			}
			continue
		}

		// Collect indented context lines for failures.
		if collectingContext {
			if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "    ") {
				contextLines = append(contextLines, strings.TrimSpace(line))
			} else {
				flushContext()
			}
		}
	}

	// Flush any trailing tests with no package summary (truncated output): we
	// can't identify the package, so report them under an empty file rather than
	// dropping them silently.
	flushContext()
	if len(pending) > 0 {
		groups = append(groups, TestFileGroup{Tests: append([]TestRecord(nil), pending...)})
	}
	return groups
}

var stressIterRe = regexp.MustCompile(`(\d+) iterations over`)
var stressFlakyRe = regexp.MustCompile(`FLAKY \((\d+) tests?\):`)

// ParseStressOutput extracts iteration count and flaky test count from stress output.
func ParseStressOutput(output string) (iterations, flakyCount int) {
	if m := stressIterRe.FindStringSubmatch(output); m != nil {
		iterations, _ = strconv.Atoi(m[1])
	}
	if m := stressFlakyRe.FindStringSubmatch(output); m != nil {
		flakyCount, _ = strconv.Atoi(m[1])
	}
	return
}

var coverageTotalRe = regexp.MustCompile(`total:.*?(\d+\.\d+)%`)

// ParseCoverageTotal extracts the total coverage percentage from Go or Promise coverage output.
func ParseCoverageTotal(output string) float64 {
	if m := coverageTotalRe.FindStringSubmatch(output); m != nil {
		pct, _ := strconv.ParseFloat(m[1], 64)
		return pct
	}
	return 0
}
