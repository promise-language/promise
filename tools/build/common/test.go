package common

import (
	"fmt"
	"path/filepath"
	"regexp"
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

	// Default to local cache; -shared opts into ~/.promise
	if !shared {
		if err := SetupLocalCache(root); err != nil {
			return fmt.Errorf("setup local cache: %w", err)
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

// RunGoTests runs only Go unit tests. Used by verify.
func RunGoTests(root string) error {
	compilerDir := filepath.Join(root, "compiler")
	return RunIn(compilerDir, "go", "test", "./...")
}

// RunPromiseTests runs Promise tests for the given target (empty = host).
// Returns captured stdout (even on failure) and any error.
func RunPromiseTests(root, target string) (string, error) {
	promiseBin := filepath.Join(root, "bin", BinaryName())
	args := []string{"test", "-timeout", "10", "tests/...", "modules/...", "examples/..."}
	if target != "" {
		args = append([]string{"test", "-timeout", "10", "-target", target}, "tests/...", "modules/...", "examples/...")
	}
	return RunTee(root, promiseBin, args...)
}

// RunPromiseTestsCapture is like RunPromiseTests but tees test output to stderr
// instead of stdout, keeping stdout clean for structured output (e.g. JSON).
func RunPromiseTestsCapture(root, target string) (string, error) {
	promiseBin := filepath.Join(root, "bin", BinaryName())
	args := []string{"test", "-timeout", "10", "tests/...", "modules/...", "examples/..."}
	if target != "" {
		args = append([]string{"test", "-timeout", "10", "-target", target}, "tests/...", "modules/...", "examples/...")
	}
	return RunTeeStderr(root, promiseBin, args...)
}

// promisePassLineRe matches the `pass (X.Xs) ...` result lines emitted by the
// Promise test runner. Both multi-file (`pass (0.004s) e2e/basics.pr (3 tests)`)
// and single-file (`pass (0.001s) test_add`) forms start the same way; pass
// entries are always single-line with no continuation.
var promisePassLineRe = regexp.MustCompile(`^pass\s+\(`)

// IsPromisePassLine reports whether a line is a passing-test result line from
// the Promise test runner. Used by gate runners to suppress these from the
// user-facing console while keeping them in the captured JSON output.
func IsPromisePassLine(line string) bool {
	return promisePassLineRe.MatchString(line)
}

// RunPromiseTestsCaptureFiltered is like RunPromiseTestsCapture but suppresses
// passing-test lines from the stderr stream the user sees. The full stdout is
// still captured and returned so callers can build complete JSON output.
func RunPromiseTestsCaptureFiltered(root, target string) (string, error) {
	promiseBin := filepath.Join(root, "bin", BinaryName())
	args := []string{"test", "-timeout", "10", "tests/...", "modules/...", "examples/..."}
	if target != "" {
		args = append([]string{"test", "-timeout", "10", "-target", target}, "tests/...", "modules/...", "examples/...")
	}
	keep := func(line string) bool { return !IsPromisePassLine(line) }
	return RunTeeStderrFiltered(root, promiseBin, keep, args...)
}
