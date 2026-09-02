package common

import (
	"context"
	"errors"
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

// ErrLockTimeout is returned by RunVerify when --lock-timeout elapses before the
// host verify lock could be acquired. It is NOT a verification failure — it
// means another verify held the lock for the whole wait. Callers (the tracker
// runner) detect it (via errors.Is, or the EX_TEMPFAIL exit code / the
// VERIFY_LOCK_TIMEOUT stderr marker the verify binary prints) to retry for a
// turn rather than treating the run as failed.
var ErrLockTimeout = errors.New("verify lock acquisition timed out")

// lockRetryDelay is how often acquireVerifyLockIn re-polls for the lock while
// waiting under a bounded --lock-timeout.
const lockRetryDelay = 500 * time.Millisecond

// RunVerify orchestrates the full pre-commit verification pipeline:
// format → build → vet → test. All steps are internal calls (no subprocess).
// Flags: -shared (use ~/.promise), -wasm (include wasm32-wasi),
// -wasm-web (include wasm32-web via Node), -clean (clear caches),
// -push (git push on success).
// Default cache is local (.promise-home/); -local is accepted for clarity.
func RunVerify(root string, args []string) error {
	args = NormalizeArgs(args)
	var shared, wasm, wasmWeb, clean, push bool
	// lockTimeout bounds how long to wait for the host verify lock. 0 (the
	// default, flag absent) waits UNBOUNDED — bin/verify is run on a variety of
	// machines where any hardcoded timeout would be wrong; bounding the wait is
	// the caller's choice via --lock-timeout (the tracker runner sets it so a
	// lost turn can be retried).
	var lockTimeout time.Duration

	// Parse args. --lock-timeout takes a duration value (NormalizeArgs has
	// already split --lock-timeout=10m into "-lock-timeout" "10m").
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-local":
		case "-shared":
			shared = true
		case "-wasm":
			wasm = true
		case "-wasm-web":
			wasmWeb = true
		case "-clean":
			clean = true
		case "-push":
			push = true
		case "-lock-timeout":
			if i+1 >= len(args) {
				return fmt.Errorf("-lock-timeout requires a duration value (e.g. --lock-timeout=10m)")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil || d < 0 {
				return fmt.Errorf("-lock-timeout: invalid duration %q (use Go duration syntax, e.g. 10m)", args[i])
			}
			lockTimeout = d
		default:
			return fmt.Errorf("usage: bin/verify [--shared] [--wasm] [--wasm-web] [--clean] [--push] [--lock-timeout=<dur>]")
		}
	}

	// Acquire global lock to serialize concurrent verify runs.
	unlock, err := acquireVerifyLock(root, lockTimeout)
	if err != nil {
		if errors.Is(err, ErrLockTimeout) {
			return err
		}
		return fmt.Errorf("acquire verify lock: %w", err)
	}
	defer unlock()

	// Clean caches first if requested. Done before SetupLocalCache so that
	// the local home is recreated empty, and before any build/test work so
	// the run starts from a known state.
	if clean {
		if err := cleanLocked(root, CleanOptions{Shared: shared}); err != nil {
			return fmt.Errorf("clean: %w", err)
		}
	}

	// Default to local cache; -shared opts into ~/.promise
	if !shared {
		if err := SetupLocalCache(root); err != nil {
			return fmt.Errorf("setup local cache: %w", err)
		}
	}

	start := time.Now()
	promiseBin := filepath.Join(root, "bin", BinaryName())

	// 1. Format Go
	Progress().Println("Formatting go...")
	if err := FormatGo(root); err != nil {
		return fmt.Errorf("format go: %w", err)
	}
	if Interrupted() {
		return errInterrupted
	}

	// 2. Format Promise (if binary exists from a prior build)
	if Exists(promiseBin) {
		Progress().Println("Formatting promise...")
		if err := FormatPromiseFiles(root, promiseBin); err != nil {
			return fmt.Errorf("format promise: %w", err)
		}
		fmt.Println()
	}
	if Interrupted() {
		return errInterrupted
	}

	// 3. Build
	Progress().Println("Building compiler...")
	if err := RunBuild(root, nil); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	if Interrupted() {
		return errInterrupted
	}

	// 4. Vet
	Progress().Println("Vetting go...")
	if err := RunVet(root); err != nil {
		return fmt.Errorf("vet: %w", err)
	}
	if Interrupted() {
		return errInterrupted
	}

	// 5. (Cache clearing now happens up front via Clean.)

	// 6-8b. Go suites, then (only if they all passed) the Promise suites.
	res, err := runVerifyTestPhases(root, wasm, wasmWeb, verifySuites{
		goTests:      RunGoTests,
		toolsTests:   RunToolsGoTests,
		flowsTests:   RunFlowsGoTests,
		promiseTests: RunPromiseTests,
	})
	if err != nil {
		return err
	}
	failures := res.failures
	hostOutput, wasmOutput, wasmWebOutput := res.hostOutput, res.wasmOutput, res.wasmWebOutput
	hostElapsed, wasmElapsed, wasmWebElapsed := res.hostElapsed, res.wasmElapsed, res.wasmWebElapsed
	flowsSkipped, flowsModPresent := res.flowsSkipped, res.flowsModPresent

	// 9. Summary — always printed, even on failure.
	hostTarget := hostTargetName()
	elapsed := time.Since(start)
	mins := int(elapsed.Minutes())
	secs := int(elapsed.Seconds()) % 60

	Progress().Println()
	Progress().Println("====================================================")
	Progress().Println("  Verify Summary")
	Progress().Println("----------------------------------------------------")
	Progress().Printf("  Host target:  %s\n", hostTarget)
	if res.promiseSkipped {
		// A Go suite failed, so the Promise phases never ran and hostElapsed is
		// genuinely 0 — say that, rather than printing a misleading "FAILED (0s)"
		// for a phase that never happened. The FAILED: line below names which
		// suite stopped the run, and the same wording covers the WASM rows.
		Progress().Printf("  Host tests:   not run (go tests failed)\n")
	} else if slices.Contains(failures, "promise tests (host)") {
		Progress().Printf("  Host tests:   FAILED (%s)\n", hostElapsed.Round(time.Millisecond))
	} else {
		Progress().Printf("  Host tests:   passed (%s)\n", hostElapsed.Round(time.Millisecond))
	}
	if flowsSkipped && !flowsModPresent {
		Progress().Printf("  Flows tests:  skipped (flows/ absent)\n")
	} else if flowsSkipped {
		Progress().Printf("  Flows tests:  skipped (SDK absent)\n")
	} else if slices.Contains(failures, "flows go tests") {
		Progress().Printf("  Flows tests:  FAILED\n")
	} else {
		Progress().Printf("  Flows tests:  passed\n")
	}
	if wasm {
		if res.promiseSkipped {
			Progress().Printf("  WASM tests:   not run (go tests failed)\n")
		} else if slices.Contains(failures, "promise tests (wasm32-wasi)") {
			Progress().Printf("  WASM tests:   FAILED (%s)\n", wasmElapsed.Round(time.Millisecond))
		} else {
			Progress().Printf("  WASM tests:   passed (%s)\n", wasmElapsed.Round(time.Millisecond))
		}
	}
	if wasmWeb {
		if res.promiseSkipped {
			Progress().Printf("  WASM-web:     not run (go tests failed)\n")
		} else if slices.Contains(failures, "promise tests (wasm32-web)") {
			Progress().Printf("  WASM-web:     FAILED (%s)\n", wasmWebElapsed.Round(time.Millisecond))
		} else {
			Progress().Printf("  WASM-web:     passed (%s)\n", wasmWebElapsed.Round(time.Millisecond))
		}
	}
	Progress().Printf("  Total time:   %dm%02ds\n", mins, secs)
	Progress().Println("====================================================")

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
		if wasmWeb {
			if s := ExtractFailedSection(wasmWebOutput); s != "" {
				sections = append(sections, failureSection{"wasm32-web", s})
			}
		}
		if len(sections) > 0 {
			Progress().Println("----------------------------------------------------")
			Progress().Println("  Failed Tests")
			for _, fs := range sections {
				Progress().Println("----------------------------------------------------")
				Progress().Printf("[%s]\n", fs.label)
				Progress().Println(fs.section)
			}
		}

		Progress().Printf("FAILED: %s\n", strings.Join(failures, ", "))
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
	if wasmWeb {
		if s := ParseTestSummaryLine(wasmWebOutput); s != nil {
			gv.Values["wasm_web_test_count"] = float64(s.Passed)
			gv.Values["wasm_web_leak_count"] = float64(s.Leaked)
			gv.Values["wasm_web_test_failures"] = float64(s.Failed)
		}
	}
	if err := WriteGateValues(root, gv); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write gate values: %v\n", err)
	}

	if push {
		Progress().Println("Pushing to remote...")
		if err := RunIn(root, "git", "push"); err != nil {
			return err
		}
	}

	Progress().Println("✅ OK to commit")
	return nil
}

// hostTargetName is the target triple label the verify summary reports.
func hostTargetName() string {
	return strings.ToLower(runtime.GOOS) + "-" + runtime.GOARCH
}

// verifySuites are the test suites runVerifyTestPhases drives, injected so the
// phase ordering (notably the Go-failure abort) is unit-testable — RunVerify
// itself takes a global lock and rebuilds the compiler, so it cannot be.
type verifySuites struct {
	goTests      func(root string) error
	toolsTests   func(root string) error
	flowsTests   func(root string) (skipped bool, err error)
	promiseTests func(root, target string) (output string, err error)
}

// verifyTestResults is everything the summary block needs from the test phases.
type verifyTestResults struct {
	failures        []string
	hostOutput      string
	wasmOutput      string
	wasmWebOutput   string
	hostElapsed     time.Duration
	wasmElapsed     time.Duration
	wasmWebElapsed  time.Duration
	flowsSkipped    bool
	flowsModPresent bool
	// promiseSkipped records that the Promise phases never ran because a Go
	// suite failed — the summary must say that rather than report a 0s failure.
	promiseSkipped bool
}

// runVerifyTestPhases runs verify's phases 6/6b/6c (Go) and 7/8/8b (Promise).
//
// All three Go suites run first, so their failures are visible together — they
// are the cheap ones. If any of them failed, the Promise suites do NOT run:
// when the compiler's own unit tests are broken there is nothing to learn from
// spending the remaining minutes on ~20k Promise tests (T1888).
//
// A returned error is an abort (Ctrl+C, missing wasmtime/node), not a test
// failure; test failures are named in the result's failures slice.
func runVerifyTestPhases(root string, wasm, wasmWeb bool, s verifySuites) (verifyTestResults, error) {
	var res verifyTestResults

	// 6. Go tests (compiler)
	Progress().Println("Running go tests...")
	if err := s.goTests(root); err != nil {
		res.failures = append(res.failures, "go tests")
	}
	if Interrupted() {
		Progress().Clear()
		return res, errInterrupted
	}

	// 6b. Tools Go tests
	Progress().Println("Running tools go tests...")
	if err := s.toolsTests(root); err != nil {
		res.failures = append(res.failures, "tools go tests")
	}
	if Interrupted() {
		Progress().Clear()
		return res, errInterrupted
	}

	// 6c. Flows Go tests (skipped when flows/go.mod or flow-sdk/go.mod absent)
	res.flowsModPresent = Exists(filepath.Join(root, "flows", "go.mod"))
	if res.flowsModPresent {
		Progress().Println("Running flows go tests...")
	}
	flowsSkipped, flowsGoErr := s.flowsTests(root)
	res.flowsSkipped = flowsSkipped
	if !flowsSkipped && flowsGoErr != nil {
		res.failures = append(res.failures, "flows go tests")
	}
	if Interrupted() {
		Progress().Clear()
		return res, errInterrupted
	}

	// A broken compiler makes the Promise suites uninformative — stop here.
	if len(res.failures) > 0 {
		res.promiseSkipped = true
		return res, nil
	}

	// 7. Promise tests (host)
	Progress().Println("\nRunning promise tests (host)...")
	hostStart := time.Now()
	hostOutput, hostErr := s.promiseTests(root, "")
	res.hostOutput = hostOutput
	if hostErr != nil {
		res.failures = append(res.failures, "promise tests (host)")
	}
	res.hostElapsed = time.Since(hostStart)
	if Interrupted() {
		Progress().Clear()
		return res, errInterrupted
	}

	// 8. Promise tests (wasm)
	if wasm {
		if Which("wasmtime") == "" {
			return res, fmt.Errorf("wasmtime not found — install from https://wasmtime.dev/ or: winget install BytecodeAlliance.Wasmtime")
		}
		Progress().Println("\nRunning promise tests (wasm32-wasi)...")
		wasmStart := time.Now()
		wasmOutput, wasmErr := s.promiseTests(root, "wasm32-wasi")
		res.wasmOutput = wasmOutput
		if wasmErr != nil {
			res.failures = append(res.failures, "promise tests (wasm32-wasi)")
		}
		res.wasmElapsed = time.Since(wasmStart)
	}

	// 8b. Promise tests (wasm32-web via Node)
	if wasmWeb {
		if Which("node") == "" {
			return res, fmt.Errorf("node not found — install Node.js 20+ (https://nodejs.org/)")
		}
		Progress().Println("\nRunning promise tests (wasm32-web)...")
		wasmWebStart := time.Now()
		wasmWebOutput, wasmWebErr := s.promiseTests(root, "wasm32-web")
		res.wasmWebOutput = wasmWebOutput
		if wasmWebErr != nil {
			res.failures = append(res.failures, "promise tests (wasm32-web)")
		}
		res.wasmWebElapsed = time.Since(wasmWebStart)
	}

	return res, nil
}

// acquireVerifyLock acquires an OS-level file lock to serialize concurrent
// verify runs. The lock is automatically released by the OS if the process
// dies, so there is no risk of orphaned locks.
// Returns an unlock function that must be deferred.
func acquireVerifyLock(root string, lockTimeout time.Duration) (func(), error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return func() {}, nil
	}

	lockDir := filepath.Join(home, ".promise")
	os.MkdirAll(lockDir, 0o755)
	lockPath := filepath.Join(lockDir, "verify.lock")

	return acquireVerifyLockIn(lockPath, root, lockTimeout)
}

// acquireVerifyLockIn takes the host verify lock. lockTimeout <= 0 waits
// indefinitely (the default); a positive lockTimeout bounds the wait and
// returns ErrLockTimeout if the lock is still held when it elapses.
func acquireVerifyLockIn(lockPath, root string, lockTimeout time.Duration) (func(), error) {
	fl := flock.New(lockPath)
	// Holder metadata lives in a sibling file, NOT lockPath itself: on Windows
	// flock takes a mandatory byte-range lock on byte 0 of lockPath, so a
	// concurrent read/write of lockPath while the lock is held fails (the
	// repo-dir write would be silently lost and waiters couldn't read it). The
	// .owner sibling is unaffected by the lock and readable on every platform.
	ownerPath := lockPath + ".owner"

	// Try non-blocking first to detect contention.
	locked, err := fl.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	if !locked {
		// Read the lock holder's repo directory before blocking.
		msg := "Waiting for another verify run to finish..."
		if data, err := os.ReadFile(ownerPath); err == nil {
			if dir := strings.TrimSpace(string(data)); dir != "" {
				msg = fmt.Sprintf("Waiting for verify run in %s to finish...", dir)
			}
		}
		fmt.Println(msg)
		if lockTimeout > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), lockTimeout)
			defer cancel()
			ok, lerr := fl.TryLockContext(ctx, lockRetryDelay)
			if lerr != nil && !errors.Is(lerr, context.DeadlineExceeded) {
				return nil, fmt.Errorf("acquire lock: %w", lerr)
			}
			if !ok {
				return nil, ErrLockTimeout
			}
		} else if err := fl.Lock(); err != nil {
			return nil, fmt.Errorf("acquire lock: %w", err)
		}
	}

	// Record our repo directory for other waiters.
	os.WriteFile(ownerPath, []byte(root+"\n"), 0o644)

	return func() {
		os.Remove(ownerPath)
		fl.Unlock()
	}, nil
}
