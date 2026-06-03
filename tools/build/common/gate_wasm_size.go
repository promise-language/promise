package common

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
)

// runGateWasmSize compiles WASM size canary programs and reports binary sizes.
// Canaries are discovered by globbing tests/size/canary_*.pr.
func runGateWasmSize(root string, args []string) error {
	args = NormalizeArgs(args)
	shared := slices.Contains(args, "-shared")

	for _, arg := range args {
		switch arg {
		case "-shared", "-local":
		default:
			return fmt.Errorf("usage: bin/gate wasm-size [-shared]")
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
	promiseBin := filepath.Join(root, "bin", BinaryName())

	// Discover canary programs.
	pattern := filepath.Join(root, "tests", "size", "canary_*.pr")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob canaries: %w", err)
	}
	if len(matches) == 0 {
		return fmt.Errorf("no canary programs found matching %s", pattern)
	}
	sort.Strings(matches)

	// Create temp dir for WASM outputs.
	tmpDir, err := os.MkdirTemp("", "promise-wasm-size-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Compile each canary and collect sizes.
	gv := &GateValues{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Platform:  hostTarget,
		Values:    make(map[string]float64),
	}

	var totalSize int64
	var compileErr error

	fmt.Fprintf(os.Stderr, "Compiling WASM size canaries...\n")
	for _, canaryPath := range matches {
		base := filepath.Base(canaryPath)
		stem := strings.TrimSuffix(base, ".pr")
		metricStem := strings.TrimPrefix(stem, "canary_")
		wasmOut := filepath.Join(tmpDir, stem+".wasm")

		// Compile with release mode for LTO'd binary size.
		_, buildCanaryErr := RunOutputIn(root, promiseBin,
			"build", "-target", "wasm32-wasi", "-release", "-o", wasmOut, canaryPath)
		if buildCanaryErr != nil {
			fmt.Fprintf(os.Stderr, "  FAIL  %s: %v\n", stem, buildCanaryErr)
			compileErr = fmt.Errorf("canary %s failed to compile", stem)
			continue
		}

		info, statErr := os.Stat(wasmOut)
		if statErr != nil {
			fmt.Fprintf(os.Stderr, "  FAIL  %s: %v\n", stem, statErr)
			compileErr = fmt.Errorf("canary %s: stat failed", stem)
			continue
		}

		size := info.Size()
		totalSize += size
		metricName := "wasm_size_" + metricStem
		gv.Values[metricName] = float64(size)

		fmt.Fprintf(os.Stderr, "  %-25s %8d bytes  (%.1f KB)\n", stem, size, float64(size)/1024.0)
	}

	gv.Values["wasm_size_total"] = float64(totalSize)
	fmt.Fprintf(os.Stderr, "  %-25s %8d bytes  (%.1f KB)\n", "TOTAL", totalSize, float64(totalSize)/1024.0)

	// Write gate-values.json sidecar.
	if err := WriteGateValues(root, gv); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write gate values: %v\n", err)
	}

	// Output the unified GateOutput envelope (metric-only; no files).
	out := &GateOutput{Target: hostTarget, Metrics: gv.Values, Complete: "wasm-size"}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gate output: %w", err)
	}
	fmt.Println(string(data))

	if compileErr != nil {
		return compileErr
	}
	return nil
}
