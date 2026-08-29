package testrun

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
	"time"
)

// T0680 Part 2 — end-to-end runtime proof that the WASM cooperative scheduler
// enforces the per-test `-timeout` in-binary (promise_sched_coop_step checks
// nanotime() >= deadline after each resumed G and returns 2/stop), NOT just via
// the outer process backstop.
//
// The IR shape is locked by internal/codegen/t0680_test.go; this test drives an
// actual wasm32-wasi binary under wasmtime and asserts an *infinite-progress
// livelock* — one that keeps completing goroutines forever — reports TIMEOUT at
// its own per-test deadline rather than at the process-level backstop (which is
// per-test-timeout + 30s).

// requireWasmtime skips the test when wasmtime is not installed (the wasm32-wasi
// binary cannot be executed without it).
func requireWasmtime(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("wasmtime"); err != nil {
		t.Skip("wasmtime not installed — skipping wasm32-wasi runtime timeout test")
	}
}

// inBinaryTimeoutRe captures the per-test duration the binary itself reported on
// a TIMEOUT outcome, e.g. "TIMEOUT (1.003s) test_livelock [wasm32-wasi]". The
// batch-budget kill prints "TIMEOUT (-)" instead, so a match is already proof
// that the in-binary deadline — not the backstop — is what fired.
var inBinaryTimeoutRe = regexp.MustCompile(`TIMEOUT \((\d+\.\d+)s\) (\w+)`)

// assertInBinaryTimeout asserts that testName timed out through the in-binary
// per-test deadline (promise_sched_coop_step observing nanotime() >= deadline)
// rather than falling through to the process-level backstop. regressed names the
// enforcement path the caller is guarding.
//
// Promptness is measured from the duration the BINARY reports, not from the wall
// clock of the whole `promise test` invocation. That invocation also does a
// wasm32-wasi cross-compile, which on a loaded machine routinely takes longer
// than the deadline itself by an order of magnitude — timing it proves nothing
// about enforcement and fails the test merely for being run alongside others.
func assertInBinaryTimeout(t *testing.T, combined, testName, regressed string) {
	t.Helper()

	m := inBinaryTimeoutRe.FindStringSubmatch(combined)
	if m == nil || m[2] != testName {
		t.Errorf("expected in-binary per-test TIMEOUT line for %s; got:\n%s", testName, combined)
		return
	}
	if !strings.Contains(combined, "1 timed out") {
		t.Errorf("expected '1 timed out' summary from in-binary enforcement; got:\n%s", combined)
	}
	if strings.Contains(combined, "batch budget") {
		t.Errorf("run hit the process-level batch budget instead of the in-binary "+
			"deadline — %s; got:\n%s", regressed, combined)
	}

	// The deadline is annotated at 1s. Enforcement granularity is one resumed G,
	// so the reported duration overshoots a little; 20s is a wide margin that
	// still catches a deadline that only fires once the backstop is in sight.
	held, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("unparseable TIMEOUT duration %q", m[1])
	}
	if held > 20 {
		t.Errorf("%s ran %ss before the in-binary deadline fired — expected it "+
			"promptly (<20s); enforcement likely regressed to the process backstop",
			testName, m[1])
	}
}

// TestT0680_WasmLivelockReportsTimeoutInBinary — a batch test that spawns and
// awaits a fresh goroutine forever (infinite progress: every iteration unwinds
// to the cooperative scheduler loop) must be interrupted by the in-binary
// deadline at ~1s and reported as TIMEOUT. Critically, the deadline the binary
// reports must be its own ~1s one, not the process backstop (1s + 30s = 31s):
// backstop-only enforcement prints a synthetic "TIMEOUT (-)" line instead.
func TestT0680_WasmLivelockReportsTimeoutInBinary(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)
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

	cmd := exec.Command(promiseBin, "test", "--target", "wasm32-wasi", file)
	output, runErr := cmd.CombinedOutput()
	combined := string(output)

	if runErr == nil {
		t.Fatalf("expected non-zero exit on timeout, got success.\nOutput:\n%s", combined)
	}

	// The in-binary path (not the batch budget) must have fired: a real TIMEOUT
	// line for the test, plus the "1 timed out" summary. The batch-budget kill
	// instead prints a synthetic "TIMEOUT (-) <name>" line whose context names
	// the budget rather than the test's own deadline (T1639).
	assertInBinaryTimeout(t, combined, "test_livelock", "Part 2 enforcement regressed")
}

// TestT1200_WasmChannelLivelockReportsTimeoutInBinary — the T0680 spawn-loop case
// works because every iteration completes a goroutine, which unwinds to the
// cooperative scheduler where the per-test deadline is checked. A livelock driven
// by an UNBUFFERED-CHANNEL rendezvous between two persistent goroutines did NOT
// (T1200): the sender is a named function spawned via `go`, so its `send` runs in
// the single, non-coroutine `@__user.worker`, whose rendezvous wait took the
// thread-blocking branch — a no-op `pal_cond_wait` busy-spin on single-threaded
// WASM. It never yielded to the scheduler, so coop_step (and its deadline check)
// never ran, and the run fell through to the 31s process backstop.
//
// The fix pumps promise_sched_coop_step in the WASM non-coroutine channel-wait
// path, so the sender yields, the receiver runs, and coop_step's T0680 deadline
// check fires. This test asserts the in-binary per-test TIMEOUT (not the backstop)
// for two variants: a pure channel rendezvous and a mixed channel+task loop.
func TestT1200_WasmChannelLivelockReportsTimeoutInBinary(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)
	requireWasmtime(t)

	cases := []struct {
		name string
		body string
	}{
		{
			name: "channel_rendezvous",
			body: "worker(channel[int] c) { int i = 0; while true { c.send(i); i = i + 1; } }\n" +
				"test_livelock() `test(timeout: \"1s\") {\n" +
				"  channel[int] c = channel[int]();\n" +
				"  go worker(c);\n" +
				"  int got = 0;\n" +
				"  while true { int? _v = <-c; got = got + 1; }\n" +
				"}\n",
		},
		{
			name: "mixed_channel_and_task",
			body: "trivial() int { return 1; }\n" +
				"worker(channel[int] c) { int i = 0; while true { c.send(i); i = i + 1; } }\n" +
				"test_livelock() `test(timeout: \"1s\") {\n" +
				"  channel[int] c = channel[int]();\n" +
				"  go worker(c);\n" +
				"  int n = 0;\n" +
				"  while true {\n" +
				"    task[int] t = go trivial();\n" +
				"    int? r = <-t;\n" +
				"    int? _v = <-c;\n" +
				"    n = n + 1;\n" +
				"  }\n" +
				"}\n",
		},
		{
			// Non-coroutine `for v in ch` receiver: the receive-empty wait is in the
			// plain @__user.consume, so its cooperative pump (not the coroutine park)
			// must observe the per-test deadline. Sender is the coroutine test body.
			name: "channel_forin_receiver",
			body: "consume(channel[int] c) { for _v in c { } }\n" +
				"test_livelock() `test(timeout: \"1s\") {\n" +
				"  channel[int] c = channel[int]();\n" +
				"  go consume(c);\n" +
				"  int i = 0;\n" +
				"  while true { c.send(i); i = i + 1; }\n" +
				"}\n",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "t1200_wasm_timeout_")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)

			// Unique tag defeats the per-source test-binary cache.
			unique := fmt.Sprintf("t1200-wasm-%s-%d-%d", tc.name, os.Getpid(), time.Now().UnixNano())
			src := "// " + unique + "\n" + tc.body
			file := filepath.Join(dir, "livelock_test.pr")
			if err := os.WriteFile(file, []byte(src), 0644); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(promiseBin, "test", "--target", "wasm32-wasi", file)
			output, runErr := cmd.CombinedOutput()
			combined := string(output)

			if runErr == nil {
				t.Fatalf("expected non-zero exit on timeout, got success.\nOutput:\n%s", combined)
			}

			assertInBinaryTimeout(t, combined, "test_livelock", "T1200 channel-livelock enforcement regressed")
		})
	}
}

// TestT1218_WasmMutexLivelockReportsTimeoutInBinary — sibling of the T1200
// channel case, one primitive over. A livelock where a NON-coroutine function
// (a named fn spawned via `go`) contends a held Mutex[T].lock() did NOT report
// the in-binary per-test TIMEOUT (T1218): the contested-lock wait runs in the
// single, non-coroutine `@__user.grab`, whose `cond_wait; recheck-held` loop took
// the thread-blocking branch — a no-op `pal_cond_wait` busy-spin on
// single-threaded WASM. It never yielded to the scheduler, so coop_step (and its
// T0680 deadline check) never ran, and the run fell through to the process
// backstop.
//
// The fix pumps promise_sched_coop_step in the WASM non-coroutine mutex-lock
// path, so the waiter yields to the scheduler and coop_step's per-test deadline
// check fires. This test asserts the in-binary per-test TIMEOUT (not the
// backstop): the test-body coroutine holds the mutex forever while making
// infinite scheduler progress (awaiting fresh goroutines in a loop — a bare
// `while true {}` holder would never yield on single-threaded WASM, since there
// is no sysmon to set G.preempt), and a NON-coroutine `grab` spawned via `go`
// contends the held lock. Without the fix, `grab`'s contested-lock branch is the
// no-op `pal_cond_wait` busy-spin: once `grab` is resumed it never returns to the
// scheduler, so the outer coop_run loop is stuck and the run falls through to the
// process backstop. With the fix, `grab` pumps coop_step, everything keeps
// progressing, and the deadline fires in-binary.
func TestT1218_WasmMutexLivelockReportsTimeoutInBinary(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)
	requireWasmtime(t)

	dir, err := os.MkdirTemp("", "t1218_wasm_timeout_")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// Unique tag defeats the per-source test-binary cache.
	unique := fmt.Sprintf("t1218-wasm-mutex-%d-%d", os.Getpid(), time.Now().UnixNano())
	src := "// " + unique + "\n" +
		"trivial() int { return 1; }\n" +
		"grab(Ref[Mutex[int]] c) { use guard := c.borrow.lock(); guard.borrow += 1; }\n" +
		"test_mtx() `test(timeout: \"1s\") {\n" +
		"  shared := Ref[Mutex[int]](Mutex[int](0));\n" +
		"  c := shared.clone();\n" +
		"  use held := shared.borrow.lock();\n" +
		"  go grab(c);\n" +
		"  while true { task[int] t = go trivial(); int? r = <-t; }\n" +
		"}\n"
	file := filepath.Join(dir, "mutex_livelock_test.pr")
	if err := os.WriteFile(file, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(promiseBin, "test", "--target", "wasm32-wasi", file)
	output, runErr := cmd.CombinedOutput()
	combined := string(output)

	if runErr == nil {
		t.Fatalf("expected non-zero exit on timeout, got success.\nOutput:\n%s", combined)
	}

	assertInBinaryTimeout(t, combined, "test_mtx", "T1218 mutex-livelock enforcement regressed")
}

// TestT1220_WasmSelectLivelockReportsTimeoutInBinary — sibling of T1200/T1218,
// one primitive over (`select`). A blocking `select` (no `default`) inside a
// NON-coroutine function (a named fn spawned via `go`) over two never-ready
// channels did NOT report the in-binary per-test TIMEOUT (T1220): the select ran
// in the single, non-coroutine `@__user.worker`, whose after-try branch (when no
// case is ready) silently fell through to the merge block on WASM instead of
// waiting. Before the fix the select no-oped and `worker` returned immediately,
// so the run neither timed out nor blocked as intended.
//
// The fix pumps promise_sched_coop_step in the WASM non-coroutine blocking-select
// path, so `worker` yields to the scheduler and coop_step's per-test deadline
// check fires. This test asserts the in-binary per-test TIMEOUT (not the
// backstop): the test-body coroutine makes infinite scheduler progress (awaiting
// fresh goroutines in a loop) while a NON-coroutine `worker` spawned via `go`
// blocks on a `select` over two channels the test never sends on. With the fix,
// `worker` parks in the coop pump, everything keeps progressing, and the deadline
// fires in-binary.
func TestT1220_WasmSelectLivelockReportsTimeoutInBinary(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)
	requireWasmtime(t)

	dir, err := os.MkdirTemp("", "t1220_wasm_timeout_")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// Unique tag defeats the per-source test-binary cache.
	unique := fmt.Sprintf("t1220-wasm-select-%d-%d", os.Getpid(), time.Now().UnixNano())
	src := "// " + unique + "\n" +
		"trivial() int { return 1; }\n" +
		"worker(channel[int] a, channel[int] b) {\n" +
		"  select {\n" +
		"    v := <-a:\n" +
		"      if _u := v { }\n" +
		"    v := <-b:\n" +
		"      if _u := v { }\n" +
		"  }\n" +
		"}\n" +
		"test_sel() `test(timeout: \"1s\") {\n" +
		"  channel[int] a = channel[int]();\n" +
		"  channel[int] b = channel[int]();\n" +
		"  go worker(a, b);\n" +
		"  while true { task[int] t = go trivial(); int? r = <-t; }\n" +
		"}\n"
	file := filepath.Join(dir, "select_livelock_test.pr")
	if err := os.WriteFile(file, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(promiseBin, "test", "--target", "wasm32-wasi", file)
	output, runErr := cmd.CombinedOutput()
	combined := string(output)

	if runErr == nil {
		t.Fatalf("expected non-zero exit on timeout, got success.\nOutput:\n%s", combined)
	}

	assertInBinaryTimeout(t, combined, "test_sel", "T1220 select-livelock enforcement regressed")
}
