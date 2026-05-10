package common

import (
	"fmt"
	"path/filepath"
	"slices"
	"time"
)

// RunTest builds the compiler and runs test suites.
// Modes: "go" (Go unit tests), "promise" (Promise tests), "all" (default).
// Flags: --wasm (include wasm32-wasi), --clean (clear caches first).
func RunTest(root string, args []string) error {
	start := time.Now()

	suite := "all"
	wasm := slices.Contains(args, "--wasm")
	clean := slices.Contains(args, "--clean")

	for _, arg := range args {
		switch arg {
		case "go", "promise", "all":
			suite = arg
		case "--wasm", "--clean":
			// already handled
		default:
			return fmt.Errorf("usage: bin/test [go|promise|all] [--wasm] [--clean]")
		}
	}

	compilerDir := filepath.Join(root, "compiler")
	promiseBin := filepath.Join(root, "bin", BinaryName())

	// Build first
	fmt.Println("Building...")
	if err := RunBuild(root, nil); err != nil {
		return fmt.Errorf("build: %w", err)
	}

	// Clean caches if requested
	if clean {
		fmt.Println("Clearing go test cache...")
		RunIn(compilerDir, "go", "clean", "-testcache")
		fmt.Println("Clearing promise test cache...")
		RunSilent(promiseBin, "clean")
	}

	// Go tests
	if suite == "go" || suite == "all" {
		fmt.Println("\nRunning go tests...")
		if err := RunIn(compilerDir, "go", "test", "./..."); err != nil {
			return fmt.Errorf("go tests: %w", err)
		}
	}

	// Promise tests
	if suite == "promise" || suite == "all" {
		fmt.Println("\nRunning promise tests (host)...")
		if err := Run(promiseBin, "test", "-timeout", "10", "tests/...", "modules/...", "examples/..."); err != nil {
			return fmt.Errorf("promise tests (host): %w", err)
		}

		if wasm {
			if Which("wasmtime") == "" {
				return fmt.Errorf("wasmtime not found — install with: bin/prereqs --wasm")
			}
			fmt.Println("\nRunning promise tests (wasm32-wasi)...")
			if err := Run(promiseBin, "test", "-timeout", "10", "-target", "wasm32-wasi",
				"tests/...", "modules/...", "examples/..."); err != nil {
				return fmt.Errorf("promise tests (wasm32-wasi): %w", err)
			}
		}
	}

	elapsed := time.Since(start).Round(time.Millisecond)
	fmt.Printf("\nAll tests passed (%s)\n", elapsed)
	return nil
}

// RunGoTests runs only Go unit tests. Used by verify.
func RunGoTests(root string) error {
	compilerDir := filepath.Join(root, "compiler")
	return RunIn(compilerDir, "go", "test", "./...")
}

// RunPromiseTests runs Promise tests for the given target (empty = host).
func RunPromiseTests(root, target string) error {
	promiseBin := filepath.Join(root, "bin", BinaryName())
	args := []string{"test", "-timeout", "10", "tests/...", "modules/...", "examples/..."}
	if target != "" {
		args = append([]string{"test", "-timeout", "10", "-target", target}, "tests/...", "modules/...", "examples/...")
	}
	return Run(promiseBin, args...)
}

