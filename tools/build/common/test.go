package common

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// RunTest builds the compiler and runs test suites.
// Modes: "go" (Go unit tests), "promise" (Promise tests), "all" (default).
// Flags: -shared (use ~/.promise), -wasm (include wasm32-wasi),
// -wasm-web (include wasm32-web via Node), -clean (clear caches first).
// Default cache is local (.promise-home/); -local is accepted for clarity.
func RunTest(root string, args []string) error {
	start := time.Now()
	args = NormalizeArgs(args)

	suite := "all"
	shared := slices.Contains(args, "-shared")
	wasm := slices.Contains(args, "-wasm")
	wasmWeb := slices.Contains(args, "-wasm-web")
	clean := slices.Contains(args, "-clean")

	for _, arg := range args {
		switch arg {
		case "go", "promise", "all":
			suite = arg
		case "-local", "-shared", "-wasm", "-wasm-web", "-clean":
			// already handled
		default:
			return fmt.Errorf("usage: bin/test [go|promise|all] [-shared] [-wasm] [-wasm-web] [-clean]")
		}
	}

	// Clean caches first if requested, before SetupLocalCache so the local
	// home is recreated empty.
	if clean {
		if err := Clean(root, CleanOptions{Shared: shared}); err != nil {
			return fmt.Errorf("clean: %w", err)
		}
	}

	// Default to local cache; -shared opts into ~/.promise
	if !shared {
		if err := SetupLocalCache(root); err != nil {
			return fmt.Errorf("setup local cache: %w", err)
		}
	}

	// Build first
	fmt.Println("Building...")
	if err := RunBuild(root, nil); err != nil {
		return fmt.Errorf("build: %w", err)
	}

	// Go tests — the compiler and tools/build modules. CI runs `bin/test`, not
	// `bin/verify`, so bin/test must cover the tools/build suite too; otherwise a
	// tools/ regression (e.g. a Windows-only test break) sails through CI while
	// only verify catches it locally. Flows are deliberately NOT run here: they
	// need a flow-sdk workspace that CI does not set up (bin/verify runs them
	// locally where that workspace exists).
	if suite == "go" || suite == "all" {
		fmt.Println("\nRunning go tests (compiler)...")
		if err := RunGoTests(root); err != nil {
			return fmt.Errorf("go tests (compiler): %w", err)
		}

		fmt.Println("\nRunning go tests (tools)...")
		if err := RunToolsGoTests(root); err != nil {
			return fmt.Errorf("go tests (tools): %w", err)
		}
	}

	// Promise tests
	if suite == "promise" || suite == "all" {
		fmt.Println("\nRunning promise tests (host)...")
		_, err := RunPromiseTests(root, "")
		if err != nil {
			return fmt.Errorf("promise tests (host): %w", err)
		}

		if wasm {
			if Which("wasmtime") == "" {
				return fmt.Errorf("wasmtime not found — install with: bin/prereqs --wasm")
			}
			fmt.Println("\nRunning promise tests (wasm32-wasi)...")
			_, err = RunPromiseTests(root, "wasm32-wasi")
			if err != nil {
				return fmt.Errorf("promise tests (wasm32-wasi): %w", err)
			}
		}

		if wasmWeb {
			if Which("node") == "" {
				return fmt.Errorf("node not found — install Node.js 20+ (see bin/prereqs)")
			}
			fmt.Println("\nRunning promise tests (wasm32-web)...")
			_, err = RunPromiseTests(root, "wasm32-web")
			if err != nil {
				return fmt.Errorf("promise tests (wasm32-web): %w", err)
			}
		}
	}

	elapsed := time.Since(start).Round(time.Millisecond)
	fmt.Printf("\nAll tests passed (%s)\n", elapsed)
	return nil
}

// RunGoTests runs only compiler Go unit tests. Used by verify.
func RunGoTests(root string) error {
	compilerDir := filepath.Join(root, "compiler")
	// -timeout 30m: see RunTests — the codegen package exceeds Go's default
	// 10m per-package limit on slow runners (GitHub windows-amd64).
	return RunIn(compilerDir, "go", "test", "-timeout", "30m", "./...")
}

// RunToolsGoTests runs Go unit tests for the tools/build module.
func RunToolsGoTests(root string) error {
	toolsDir := filepath.Join(root, "tools", "build")
	return RunIn(toolsDir, "go", "test", "-timeout", "30m", "./...")
}

// RunFlowsGoTests runs Go unit tests for the flows module.
// Returns (skipped=true, nil) when flows/go.mod or flow-sdk/go.mod is absent.
func RunFlowsGoTests(root string) (skipped bool, err error) {
	if !Exists(filepath.Join(root, "flows", "go.mod")) {
		return true, nil
	}
	if !Exists(filepath.Join(root, "flow-sdk", "go.mod")) {
		fmt.Fprintf(os.Stderr, "warning: skipping flows tests — flow-sdk/ not present (run ./make to fetch)\n")
		return true, nil
	}
	flowsDir := filepath.Join(root, "flows")
	return false, RunIn(flowsDir, "go", "test", "-timeout", "30m", "./...")
}

// RunPromiseTests runs Promise tests for the given target (empty = host).
// Returns captured stdout (even on failure) and any error.
func RunPromiseTests(root, target string) (string, error) {
	promiseBin := filepath.Join(root, "bin", BinaryName())
	args := []string{"test", "-timeout", "10", "tests/...", "modules/...", "examples/...", "tools/stub/..."}
	if target != "" {
		args = append([]string{"test", "-timeout", "10", "-target", target}, "tests/...", "modules/...", "examples/...", "tools/stub/...")
	}
	return RunTee(root, promiseBin, args...)
}

// RunPromiseTestsCapture is like RunPromiseTests but tees test output to stderr
// instead of stdout, keeping stdout clean for structured output (e.g. JSON).
func RunPromiseTestsCapture(root, target string) (string, error) {
	promiseBin := filepath.Join(root, "bin", BinaryName())
	args := []string{"test", "-timeout", "10", "tests/...", "modules/...", "examples/...", "tools/stub/..."}
	if target != "" {
		args = append([]string{"test", "-timeout", "10", "-target", target}, "tests/...", "modules/...", "examples/...", "tools/stub/...")
	}
	return RunTeeStderr(root, promiseBin, args...)
}

// RunPromiseTestsJSON runs the Promise test suite with --json, returning the
// raw newline-delimited JSON (one record per eligible test) from stdout. Human
// progress streams to stderr. The captured JSONL is returned even when tests
// fail (non-zero exit), so the gate can always build its per-test report. When
// target is non-empty it cross-compiles for that target. T0763.
func RunPromiseTestsJSON(root, target string) (string, error) {
	promiseBin := filepath.Join(root, "bin", BinaryName())
	args := []string{"test", "-timeout", "10", "--json"}
	if target != "" {
		args = append(args, "-target", target)
	}
	args = append(args, "tests/...", "modules/...", "examples/...", "tools/stub/...")
	return RunCaptureStdout(root, promiseBin, args...)
}
