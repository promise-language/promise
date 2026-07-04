package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// T0680 Part 2 — end-to-end runtime proof that the WASM cooperative scheduler
// enforces the per-test `-timeout` in-binary (promise_sched_coop_step checks
// nanotime() >= deadline after each resumed G and returns 2/stop), NOT just via
// the outer process backstop.
//
// The IR shape is locked by internal/codegen/t0680_test.go; this test drives an
// actual wasm32-wasi binary under wasmtime and asserts an *infinite-progress
// livelock* — one that keeps completing goroutines forever — reports TIMEOUT and
// the runner exits promptly (well under the process-level backstop, which is
// per-test-timeout + 30s).

// requireWasmtime skips the test when wasmtime is not installed (the wasm32-wasi
// binary cannot be executed without it).
func requireWasmtime(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("wasmtime"); err != nil {
		t.Skip("wasmtime not installed — skipping wasm32-wasi runtime timeout test")
	}
}

// TestT0680_WasmLivelockReportsTimeoutInBinary — a batch test that spawns and
// awaits a fresh goroutine forever (infinite progress: every iteration unwinds
// to the cooperative scheduler loop) must be interrupted by the in-binary
// deadline at ~1s and reported as TIMEOUT. Critically, the whole run must finish
// far below the process backstop (1s + 30s = 31s): if enforcement regressed to
// backstop-only, this would take ≥31s.
func TestT0680_WasmLivelockReportsTimeoutInBinary(t *testing.T) {
	promiseBin := locatePromiseBin(t)
	requireWasmtime(t)

	dir, err := os.MkdirTemp("", "t0680_wasm_timeout_")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// Unique tag defeats the per-source test-binary cache (stale metadata could
	// otherwise override the timeout annotation).
	unique := fmt.Sprintf("t0680-wasm-livelock-%d-%d", os.Getpid(), time.Now().UnixNano())
	src := "// " + unique + "\n" +
		"trivial() int { return 1; }\n" +
		"test_livelock() `test(timeout: \"1s\") {\n" +
		"  int n = 0;\n" +
		"  while true {\n" +
		"    task[int] t = go trivial();\n" +
		"    int? r = <-t;\n" +
		"    n = n + 1;\n" +
		"  }\n" +
		"}\n"
	file := filepath.Join(dir, "livelock_test.pr")
	if err := os.WriteFile(file, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	// Wall-clock bound: generous enough for compile + wasmtime startup, but far
	// below the 31s process backstop. If the in-binary deadline fires we finish
	// in a few seconds; if it regressed we'd blow past this bound.
	start := time.Now()
	cmd := exec.Command(promiseBin, "test", "--target", "wasm32-wasi", file)
	output, runErr := cmd.CombinedOutput()
	elapsed := time.Since(start)
	combined := string(output)

	if runErr == nil {
		t.Fatalf("expected non-zero exit on timeout, got success.\nOutput:\n%s", combined)
	}

	// The in-binary path (not the backstop) must have fired: a real TIMEOUT line
	// for the test, plus the "1 timed out" summary. The backstop instead prints
	// "TIMEOUT: tests exceeded <N>s timeout" with no per-test TIMEOUT line.
	tre := regexp.MustCompile(`TIMEOUT \(\d+\.\d+s\) test_livelock`)
	if !tre.MatchString(combined) {
		t.Errorf("expected in-binary per-test TIMEOUT line for test_livelock; got:\n%s", combined)
	}
	if !strings.Contains(combined, "1 timed out") {
		t.Errorf("expected '1 timed out' summary from in-binary enforcement; got:\n%s", combined)
	}
	if strings.Contains(combined, "tests exceeded") {
		t.Errorf("run hit the process-level backstop instead of the in-binary "+
			"deadline — Part 2 enforcement regressed; got:\n%s", combined)
	}

	// Promptness: must be well under the 31s backstop. 20s is a wide margin that
	// still catches a regression to backstop-only.
	if elapsed > 20*time.Second {
		t.Errorf("run took %s — expected prompt in-binary timeout (<20s); "+
			"enforcement likely regressed to the process backstop", elapsed)
	}
}
