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
		return fmt.Errorf("usage: bin/gate <subcommand> [flags]\nSubcommands:\n  test        run Promise tests and output JSON gate values\n  wasm-tests  run only WASM target tests and output JSON gate values\n  go-test     run Go tests and output JSON gate values\n  stress      run stress tests and output JSON gate values\n  coverage    run coverage analysis and output JSON gate values")
	}
	switch args[0] {
	case "test":
		return runGateTest(root, args[1:])
	case "wasm-tests":
		return runGateWasmTests(root, args[1:])
	case "go-test":
		return runGateGoTest(root, args[1:])
	case "stress":
		return runGateStress(root, args[1:])
	case "coverage":
		return runGateCoverage(root, args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q\nSubcommands: test, wasm-tests, go-test, stress, coverage", args[0])
	}
}

// runGateTest runs Promise tests and writes structured JSON gate values to stdout.
// Test progress is written to stderr so stdout is clean JSON.
func runGateTest(root string, args []string) error {
	wasm := slices.Contains(args, "--wasm")
	shared := slices.Contains(args, "--shared")

	for _, arg := range args {
		switch arg {
		case "--wasm", "--shared", "--local":
		default:
			return fmt.Errorf("usage: bin/gate test [--wasm] [--shared]")
		}
	}

	// Fail fast on missing wasmtime before running any tests.
	if wasm && Which("wasmtime") == "" {
		return fmt.Errorf("wasmtime not found — install with: bin/prereqs --wasm")
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

	// Run host tests — tee to stderr so stdout is clean for JSON.
	fmt.Fprintf(os.Stderr, "Running promise tests (%s)...\n", hostTarget)
	hostOutput, hostErr := RunPromiseTestsCapture(root, "")
	ReportTestHealth(root, hostTarget, hostOutput)

	// Run wasm tests if requested.
	var wasmOutput string
	var wasmErr error
	if wasm {
		fmt.Fprintf(os.Stderr, "Running promise tests (wasm32-wasi)...\n")
		wasmOutput, wasmErr = RunPromiseTestsCapture(root, "wasm32-wasi")
		ReportTestHealth(root, "wasm32-wasi", wasmOutput)
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

	// Build per-test entries for the gate output.
	entries := ParseTestEntries(hostTarget, hostOutput)
	if wasm {
		entries = append(entries, ParseTestEntries("wasm32-wasi", wasmOutput)...)
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
	shared := slices.Contains(args, "--shared")

	for _, arg := range args {
		switch arg {
		case "--shared", "--local":
		default:
			return fmt.Errorf("usage: bin/gate wasm-tests [--shared]")
		}
	}

	if Which("wasmtime") == "" {
		return fmt.Errorf("wasmtime not found — install with: bin/prereqs --wasm")
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

	// Run wasm tests only — tee to stderr so stdout is clean for JSON.
	fmt.Fprintf(os.Stderr, "Running promise tests (wasm32-wasi)...\n")
	wasmOutput, wasmErr := RunPromiseTestsCapture(root, "wasm32-wasi")
	ReportTestHealth(root, "wasm32-wasi", wasmOutput)

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

	// Build per-test entries for the gate output.
	wasmEntries := ParseTestEntries("wasm32-wasi", wasmOutput)

	// Output GateOutput JSON to stdout (machine-readable).
	out := &GateOutput{Metrics: gv.Values, Tests: wasmEntries, Complete: "wasm-tests"}
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
	shared := slices.Contains(args, "--shared")

	for _, arg := range args {
		switch arg {
		case "--shared", "--local":
		default:
			return fmt.Errorf("usage: bin/gate go-test [--shared]")
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
	out := &GateOutput{Metrics: gv.Values, Complete: "go-tests"}
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
	shared := slices.Contains(args, "--shared")

	for _, arg := range args {
		switch arg {
		case "--shared", "--local":
		default:
			return fmt.Errorf("usage: bin/gate coverage [--shared]")
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
