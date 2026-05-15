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

var errInterrupted = fmt.Errorf("interrupted by Ctrl+C")

// RunVerify orchestrates the full pre-commit verification pipeline:
// format → build → vet → test. All steps are internal calls (no subprocess).
// Flags: -shared (use ~/.promise), -wasm (include wasm target), -clean (clear caches), -push (git push on success).
// Default cache is local (.promise-home/); -local is accepted for clarity.
func RunVerify(root string, args []string) error {
	args = NormalizeArgs(args)
	shared := slices.Contains(args, "-shared")
	wasm := slices.Contains(args, "-wasm")
	clean := slices.Contains(args, "-clean")
	push := slices.Contains(args, "-push")

	// Validate args
	for _, arg := range args {
		switch arg {
		case "-local", "-shared", "-wasm", "-clean", "-push":
		default:
			return fmt.Errorf("usage: bin/verify [-shared] [-wasm] [-clean] [-push]")
		}
	}

	// Acquire global lock to serialize concurrent verify runs
	unlock, err := acquireVerifyLock(root)
	if err != nil {
		return fmt.Errorf("acquire verify lock: %w", err)
	}
	defer unlock()

	// Default to local cache; -shared opts into ~/.promise
	if !shared {
		promiseHome := filepath.Join(root, ".promise-home")
		if clean {
			os.RemoveAll(filepath.Join(promiseHome, "tmp"))
		}
		if err := SetupLocalCache(root); err != nil {
			return fmt.Errorf("setup local cache: %w", err)
		}
	}

	start := time.Now()
	promiseBin := filepath.Join(root, "bin", BinaryName())

	// 1. Format Go
	fmt.Println("Formatting go...")
	if err := FormatGo(root); err != nil {
		return fmt.Errorf("format go: %w", err)
	}
	if Interrupted() {
		return errInterrupted
	}

	// 2. Format Promise (if binary exists from a prior build)
	if Exists(promiseBin) {
		fmt.Println("Formatting promise...")
		if err := FormatPromiseFiles(root, promiseBin); err != nil {
			return fmt.Errorf("format promise: %w", err)
		}
		fmt.Println()
	}
	if Interrupted() {
		return errInterrupted
	}

	// 3. Build
	fmt.Println("Building compiler...")
	if err := RunBuild(root, nil); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	if Interrupted() {
		return errInterrupted
	}

	// 4. Vet
	fmt.Println("Vetting go...")
	if err := RunVet(root); err != nil {
		return fmt.Errorf("vet: %w", err)
	}
	if Interrupted() {
		return errInterrupted
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
	if Interrupted() {
		return errInterrupted
	}

	// 7. Promise tests (host)
	hostTarget := strings.ToLower(runtime.GOOS) + "-" + runtime.GOARCH
	fmt.Println("\nRunning promise tests (host)...")
	hostStart := time.Now()
	hostOutput, hostErr := RunPromiseTests(root, "")
	if hostErr != nil {
		failures = append(failures, "promise tests (host)")
	}
	hostElapsed := time.Since(hostStart)
	if Interrupted() {
		return errInterrupted
	}

	// 8. Promise tests (wasm)
	var wasmOutput string
	var wasmElapsed time.Duration
	if wasm {
		if Which("wasmtime") == "" {
			return fmt.Errorf("wasmtime not found — install from https://wasmtime.dev/ or: winget install BytecodeAlliance.Wasmtime")
		}
		fmt.Println("\nRunning promise tests (wasm32-wasi)...")
		wasmStart := time.Now()
		var wasmErr error
		wasmOutput, wasmErr = RunPromiseTests(root, "wasm32-wasi")
		if wasmErr != nil {
			failures = append(failures, "promise tests (wasm32-wasi)")
		}
		wasmElapsed = time.Since(wasmStart)
	}

	// 9. Summary — always printed, even on failure.
	elapsed := time.Since(start)
	mins := int(elapsed.Minutes())
	secs := int(elapsed.Seconds()) % 60

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
		// Consolidated per-test failure detail — host first, WASM second.
		// This re-states the FAILED: section from each target's output so that
		// agents tail-reading the last ~40 lines see all failures, not just the
		// final target's output.
		type failureSection struct{ label, section string }
		var sections []failureSection
		if s := ExtractFailedSection(hostOutput); s != "" {
			sections = append(sections, failureSection{hostTarget, s})
		}
		if wasm {
			if s := ExtractFailedSection(wasmOutput); s != "" {
				sections = append(sections, failureSection{"wasm32-wasi", s})
			}
		}
		if len(sections) > 0 {
			fmt.Println("----------------------------------------------------")
			fmt.Println("  Failed Tests")
			for _, fs := range sections {
				fmt.Println("----------------------------------------------------")
				fmt.Printf("[%s]\n", fs.label)
				fmt.Println(fs.section)
			}
		}

		fmt.Printf("FAILED: %s\n", strings.Join(failures, ", "))
		return fmt.Errorf("%s failed", strings.Join(failures, ", "))
	}

	// 10. Write gate values sidecar for commit gate.
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
	if err := WriteGateValues(root, gv); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write gate values: %v\n", err)
	}

	if push {
		fmt.Println("Pushing to remote...")
		if err := RunIn(root, "git", "push"); err != nil {
			return err
		}
	}

	fmt.Println("✅ OK to commit")
	return nil
}

// acquireVerifyLock acquires an OS-level file lock to serialize concurrent
// verify runs. The lock is automatically released by the OS if the process
// dies, so there is no risk of orphaned locks.
// Returns an unlock function that must be deferred.
func acquireVerifyLock(root string) (func(), error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return func() {}, nil
	}

	lockDir := filepath.Join(home, ".promise")
	os.MkdirAll(lockDir, 0o755)
	lockPath := filepath.Join(lockDir, "verify.lock")

	return acquireVerifyLockIn(lockPath, root)
}

func acquireVerifyLockIn(lockPath, root string) (func(), error) {
	fl := flock.New(lockPath)

	// Try non-blocking first to detect contention.
	locked, err := fl.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	if !locked {
		// Read the lock holder's repo directory before blocking.
		msg := "Waiting for another verify run to finish..."
		if data, err := os.ReadFile(lockPath); err == nil {
			if dir := strings.TrimSpace(string(data)); dir != "" {
				msg = fmt.Sprintf("Waiting for verify run in %s to finish...", dir)
			}
		}
		fmt.Println(msg)
		if err := fl.Lock(); err != nil {
			return nil, fmt.Errorf("acquire lock: %w", err)
		}
	}

	// Record our repo directory for other waiters.
	os.WriteFile(lockPath, []byte(root+"\n"), 0o644)

	return func() {
		os.WriteFile(lockPath, nil, 0o644)
		fl.Unlock()
	}, nil
}
