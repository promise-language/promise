package common

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"
)

// RunGate dispatches bin/gate subcommands. Subcommands output structured JSON
// gate values to stdout; progress messages go to stderr.
func RunGate(root string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: bin/gate <subcommand> [flags]\nSubcommands:\n  test   run tests and output JSON gate values")
	}
	switch args[0] {
	case "test":
		return runGateTest(root, args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q\nSubcommands: test", args[0])
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

	// Output flat JSON to stdout (machine-readable).
	data, err := json.MarshalIndent(gv.Values, "", "  ")
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
