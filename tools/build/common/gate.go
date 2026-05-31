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
		return fmt.Errorf("usage: bin/gate <subcommand> [flags]\nSubcommands:\n  test        run Promise tests and output JSON gate values\n  wasm-test   run only WASM target tests and output JSON gate values\n  wasm-size   compile WASM canaries and report binary sizes\n  go-test     run Go tests and output JSON gate values\n  stress      run stress tests and output JSON gate values\n  coverage    run coverage analysis and output JSON gate values")
	}
	switch args[0] {
	case "test":
		return runGateTest(root, args[1:])
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
	default:
		return fmt.Errorf("unknown subcommand %q\nSubcommands: test, wasm-test, wasm-size, go-test, stress, coverage", args[0])
	}
}

// runGateTest runs Promise tests and writes structured JSON gate values to stdout.
// Test progress is written to stderr so stdout is clean JSON.
func runGateTest(root string, args []string) error {
	args = NormalizeArgs(args)
	wasm := slices.Contains(args, "-wasm")
	shared := slices.Contains(args, "-shared")

	for _, arg := range args {
		switch arg {
		case "-wasm", "-shared", "-local":
		default:
			return fmt.Errorf("usage: bin/gate test [-wasm] [-shared]")
		}
	}

	// Fail fast on missing wasmtime before running any tests.
	if wasm && Which("wasmtime") == "" {
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

	// T0749: sidecar paths for the runner's passing-test reports. They let the
	// gate expand file-only pass entries into per-(file, test) records so the
	// tracker can correlate test identity across runs (passing batch tests are
	// otherwise recorded as file-only, unlike failing tests which carry names).
	hostReport := filepath.Join(os.TempDir(), "promise_gate_report_host.json")
	wasmReport := filepath.Join(os.TempDir(), "promise_gate_report_wasm.json")
	// Remove any stale report left by a prior hard-killed run so that if this
	// run's runner is itself killed before writing, MergePassingTestNames reads
	// nothing (a safe no-op) rather than stale (file, test) data from another run.
	os.Remove(hostReport)
	os.Remove(wasmReport)
	defer os.Remove(hostReport)
	defer os.Remove(wasmReport)

	// Run host tests — tee to stderr so stdout is clean for JSON. Filter out
	// passing-test lines from the console; the captured output retains them
	// so the JSON envelope still records every test (T0323).
	fmt.Fprintf(os.Stderr, "Running promise tests (%s)...\n", hostTarget)
	hostOutput, hostErr := RunPromiseTestsCaptureFiltered(root, "", hostReport)

	// Run wasm tests if requested.
	var wasmOutput string
	var wasmErr error
	if wasm {
		fmt.Fprintf(os.Stderr, "Running promise tests (wasm32-wasi)...\n")
		wasmOutput, wasmErr = RunPromiseTestsCaptureFiltered(root, "wasm32-wasi", wasmReport)
	}

	// Collect gate values.
	gv := &GateValues{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Platform:  hostTarget,
		Values:    make(map[string]float64),
	}
	if s := ParseTestSummaryLine(hostOutput); s != nil {
		gv.Values["host_test_count"] = float64(s.Passed)
		gv.Values["host_leak_count"] = float64(s.Leaked)
		gv.Values["host_test_failures"] = float64(s.Failed)
	}
	if wasm {
		if s := ParseTestSummaryLine(wasmOutput); s != nil {
			gv.Values["wasm_test_count"] = float64(s.Passed)
			gv.Values["wasm_leak_count"] = float64(s.Leaked)
			gv.Values["wasm_test_failures"] = float64(s.Failed)
		}
	}

	// Write gate-values.json sidecar so bin/commitgate can read it.
	if err := WriteGateValues(root, gv); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write gate values: %v\n", err)
	}

	// Build per-test entries for the gate output. T0749: merge the runner's
	// passing-test report so passing batch tests carry their (file, test) name.
	entries := MergePassingTestNames(hostTarget, ParseTestEntries(hostTarget, hostOutput), hostReport)
	if wasm {
		entries = append(entries, MergePassingTestNames("wasm32-wasi", ParseTestEntries("wasm32-wasi", wasmOutput), wasmReport)...)
	}

	// Output GateOutput JSON to stdout (machine-readable).
	out := &GateOutput{Metrics: gv.Values, Tests: entries, Complete: "promise-tests"}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gate values: %w", err)
	}
	fmt.Println(string(data))

	if hostErr != nil {
		return fmt.Errorf("promise tests (host) failed")
	}
	if wasmErr != nil {
		return fmt.Errorf("promise tests (wasm32-wasi) failed")
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

	// T0749: sidecar path for the runner's passing-test report (see runGateTest).
	wasmReport := filepath.Join(os.TempDir(), "promise_gate_report_wasm.json")
	// Remove any stale report up-front so a runner killed before writing reads
	// nothing (safe no-op) rather than stale data from another run.
	os.Remove(wasmReport)
	defer os.Remove(wasmReport)

	// Run wasm tests only — tee to stderr so stdout is clean for JSON. Filter
	// out passing-test lines from the console; captured output retains them
	// for the JSON envelope (T0323).
	fmt.Fprintf(os.Stderr, "Running promise tests (wasm32-wasi)...\n")
	wasmOutput, wasmErr := RunPromiseTestsCaptureFiltered(root, "wasm32-wasi", wasmReport)

	// Collect gate values.
	gv := &GateValues{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Platform:  hostTarget,
		Values:    make(map[string]float64),
	}
	if s := ParseTestSummaryLine(wasmOutput); s != nil {
		gv.Values["wasm_test_count"] = float64(s.Passed)
		gv.Values["wasm_leak_count"] = float64(s.Leaked)
		gv.Values["wasm_test_failures"] = float64(s.Failed)
	}

	// Write gate-values.json sidecar so bin/commitgate can read it.
	if err := WriteGateValues(root, gv); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write gate values: %v\n", err)
	}

	// Build per-test entries for the gate output. T0749: merge the runner's
	// passing-test report so passing batch tests carry their (file, test) name.
	wasmEntries := MergePassingTestNames("wasm32-wasi", ParseTestEntries("wasm32-wasi", wasmOutput), wasmReport)

	// Output GateOutput JSON to stdout (machine-readable).
	out := &GateOutput{Metrics: gv.Values, Tests: wasmEntries, Complete: "wasm-test"}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gate values: %w", err)
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
	goTests := ParseGoTestEntries(hostTarget, output)

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

	// Output GateOutput JSON to stdout (machine-readable).
	out := &GateOutput{Metrics: gv.Values, Tests: goTests, Complete: "go-tests"}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gate values: %w", err)
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

	// Output GateOutput JSON to stdout (machine-readable).
	out := &GateOutput{Metrics: gv.Values}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gate values: %w", err)
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

	// Output GateOutput JSON to stdout (machine-readable).
	out := &GateOutput{Metrics: gv.Values}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gate values: %w", err)
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

var goTestResultRe = regexp.MustCompile(`^--- (PASS|FAIL): (\S+) \((\d+\.\d+)s\)`)
var goTestPkgRe = regexp.MustCompile(`^(ok|FAIL)\s+(\S+)\s+`)

// ParseGoTestEntries extracts per-test GateTestEntry records from `go test -v` output.
func ParseGoTestEntries(target, output string) []GateTestEntry {
	lines := strings.Split(output, "\n")
	var result []GateTestEntry
	var pending []GateTestEntry
	var contextLines []string
	collectingContext := false

	for _, line := range lines {
		// Package summary line — flush pending entries with package path.
		if m := goTestPkgRe.FindStringSubmatch(line); m != nil {
			if collectingContext && len(pending) > 0 {
				pending[len(pending)-1].Context = strings.Join(contextLines, "\n")
				collectingContext = false
				contextLines = nil
			}
			pkg := m[2]
			for i := range pending {
				pending[i].File = pkg
			}
			result = append(result, pending...)
			pending = pending[:0]
			continue
		}

		// Test result line.
		if m := goTestResultRe.FindStringSubmatch(line); m != nil {
			if collectingContext && len(pending) > 0 {
				pending[len(pending)-1].Context = strings.Join(contextLines, "\n")
				collectingContext = false
				contextLines = nil
			}
			outcome := m[1]
			if outcome == "PASS" {
				outcome = "pass"
			}
			elapsed, _ := strconv.ParseFloat(m[3], 64)
			pending = append(pending, GateTestEntry{
				Target:  target,
				Test:    m[2],
				Outcome: outcome,
				Elapsed: elapsed,
			})
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
				if len(pending) > 0 {
					pending[len(pending)-1].Context = strings.Join(contextLines, "\n")
				}
				collectingContext = false
				contextLines = nil
			}
		}
	}

	// Flush remaining pending entries.
	if collectingContext && len(pending) > 0 {
		pending[len(pending)-1].Context = strings.Join(contextLines, "\n")
	}
	result = append(result, pending...)
	return result
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
