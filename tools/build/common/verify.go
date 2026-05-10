package common

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// RunVerify orchestrates the full pre-commit verification pipeline:
// format → build → vet → test. All steps are internal calls (no subprocess).
// Flags: --local (use .promise-home), --wasm (include wasm target), --clean (clear caches).
func RunVerify(root string, args []string) error {
	local := slices.Contains(args, "--local")
	wasm := slices.Contains(args, "--wasm")
	clean := slices.Contains(args, "--clean")

	// Validate args
	for _, arg := range args {
		switch arg {
		case "--local", "--wasm", "--clean":
		default:
			return fmt.Errorf("usage: bin/verify [--local] [--wasm] [--clean]")
		}
	}

	// Acquire global lock to serialize concurrent verify runs
	unlock, err := acquireVerifyLock()
	if err != nil {
		return fmt.Errorf("acquire verify lock: %w", err)
	}
	defer unlock()

	// Set up local environment if requested
	if local {
		promiseHome := filepath.Join(root, ".promise-home")
		tmpDir := filepath.Join(promiseHome, "tmp")
		if clean {
			os.RemoveAll(tmpDir)
		}
		os.MkdirAll(tmpDir, 0o755)
		os.Setenv("PROMISE_HOME", promiseHome)
		os.Setenv("TMPDIR", tmpDir)
	}

	start := time.Now()
	promiseBin := filepath.Join(root, "bin", BinaryName())

	// 1. Format Go
	fmt.Println("Formatting go...")
	if err := FormatGo(root); err != nil {
		return fmt.Errorf("format go: %w", err)
	}

	// 2. Format Promise (if binary exists from a prior build)
	if Exists(promiseBin) {
		fmt.Println("Formatting promise...")
		if err := FormatPromiseFiles(root, promiseBin); err != nil {
			return fmt.Errorf("format promise: %w", err)
		}
		fmt.Println()
	}

	// 3. Build
	fmt.Println("Building compiler...")
	if err := RunBuild(root, nil); err != nil {
		return fmt.Errorf("build: %w", err)
	}

	// 4. Vet
	fmt.Println("Vetting go...")
	if err := RunVet(root); err != nil {
		return fmt.Errorf("vet: %w", err)
	}

	// 5. Clear caches if requested
	compilerDir := filepath.Join(root, "compiler")
	if clean {
		fmt.Println("Clearing go test cache...")
		RunIn(compilerDir, "go", "clean", "-testcache")
		fmt.Println("Clearing promise test cache...")
		RunSilent(promiseBin, "clean")
	}

	// 6. Go tests
	var failures []string
	fmt.Println("Running go tests...")
	if err := RunGoTests(root); err != nil {
		failures = append(failures, "go tests")
	}

	// 7. Promise tests (host)
	fmt.Println("\nRunning promise tests (host)...")
	hostStart := time.Now()
	if err := RunPromiseTests(root, ""); err != nil {
		failures = append(failures, "promise tests (host)")
	}
	hostElapsed := time.Since(hostStart)

	// 8. Promise tests (wasm)
	var wasmElapsed time.Duration
	if wasm {
		if Which("wasmtime") == "" {
			return fmt.Errorf("wasmtime not found — install with: bin/prereqs --wasm")
		}
		fmt.Println("\nRunning promise tests (wasm32-wasi)...")
		wasmStart := time.Now()
		if err := RunPromiseTests(root, "wasm32-wasi"); err != nil {
			failures = append(failures, "promise tests (wasm32-wasi)")
		}
		wasmElapsed = time.Since(wasmStart)
	}

	// 9. Summary — always printed, even on failure.
	elapsed := time.Since(start)
	mins := int(elapsed.Minutes())
	secs := int(elapsed.Seconds()) % 60
	hostTarget := strings.ToLower(runtime.GOOS) + "-" + runtime.GOARCH

	fmt.Println()
	fmt.Println("====================================================")
	fmt.Println("  Verify Summary")
	fmt.Println("----------------------------------------------------")
	fmt.Printf("  Host target:  %s\n", hostTarget)
	if slices.Contains(failures, "promise tests (host)") || slices.Contains(failures, "go tests") {
		fmt.Printf("  Host tests:   FAILED (%s)\n", hostElapsed.Round(time.Millisecond))
	} else {
		fmt.Printf("  Host tests:   passed (%s)\n", hostElapsed.Round(time.Millisecond))
	}
	if wasm {
		if slices.Contains(failures, "promise tests (wasm32-wasi)") {
			fmt.Printf("  WASM tests:   FAILED (%s)\n", wasmElapsed.Round(time.Millisecond))
		} else {
			fmt.Printf("  WASM tests:   passed (%s)\n", wasmElapsed.Round(time.Millisecond))
		}
	}
	fmt.Printf("  Total time:   %dm%02ds\n", mins, secs)
	fmt.Println("====================================================")

	if len(failures) > 0 {
		fmt.Printf("FAILED: %s\n", strings.Join(failures, ", "))
		return fmt.Errorf("%s failed", strings.Join(failures, ", "))
	}

	fmt.Println("OK to commit")
	return nil
}

// acquireVerifyLock acquires an OS-level file lock to serialize concurrent
// verify runs. The lock is automatically released by the OS if the process
// dies, so there is no risk of orphaned locks.
// Returns an unlock function that must be deferred.
func acquireVerifyLock() (func(), error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return func() {}, nil
	}

	lockDir := filepath.Join(home, ".promise")
	os.MkdirAll(lockDir, 0o755)
	lockPath := filepath.Join(lockDir, "verify.lock")

	fl := flock.New(lockPath)

	// Try non-blocking first to detect contention.
	locked, err := fl.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	if !locked {
		fmt.Println("Waiting for another verify run to finish...")
		if err := fl.Lock(); err != nil {
			return nil, fmt.Errorf("acquire lock: %w", err)
		}
	}

	return func() {
		fl.Unlock()
	}, nil
}
