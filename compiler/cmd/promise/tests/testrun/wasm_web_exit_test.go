package testrun

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// wasm32-web runs under the pinned node and the embedded harness, which reads a
// run's outcome from how _initialize ends. A batch-test binary exits with its
// verdict, so a failing batch fails the run; a program still a reactor when
// _initialize returns waits on host events the headless harness never
// delivers, so it fails as "reactor idle" rather than passing on its setup
// output (docs/web-apps.md "Testing a web application").

// writeWebTestFile writes src under dir with a tag unique to this run, so the
// per-source test-binary cache cannot answer for a binary built before the
// compiler under test.
func writeWebTestFile(t *testing.T, dir, name, src string) string {
	t.Helper()
	tag := fmt.Sprintf("// wasm-web-exit-%d-%d\n", os.Getpid(), time.Now().UnixNano())
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, []byte(tag+src), 0644); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestWasmWebBatchExitCodeIsTheVerdict(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	dir := clitest.TempDir(t)

	for _, c := range []struct {
		name     string
		src      string
		wantCode int
		wantOut  string
	}{
		{"failing_test.pr", "test_fails() `test {\n  assert(1 == 2, \"boom\");\n}\n" +
			"test_passes() `test {\n  assert(1 == 1, \"fine\");\n}\n", 1, "1 passed, 1 failed"},
		{"passing_test.pr", "test_passes() `test {\n  assert(1 == 1, \"fine\");\n}\n", 0, "1 passed"},
		// The file's own main is replaced by the batch main, whose verdict
		// still decides the exit.
		{"failing_with_main_test.pr", "main() {\n  print_line(\"main\");\n}\n" +
			"test_fails() `test {\n  assert(1 == 2, \"boom\");\n}\n", 1, "0 passed, 1 failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			file := writeWebTestFile(t, dir, c.name, c.src)
			r := clitest.Run(t, bin, nil, "test", "-target", "wasm32-web", file)
			skipIfRuntimeUnobtainable(t, r.Combined())
			if r.ExitCode != c.wantCode || !strings.Contains(r.Combined(), c.wantOut) {
				t.Errorf("exit %d, want %d with %q\n%s", r.ExitCode, c.wantCode, c.wantOut, r.Combined())
			}
		})
	}
}

func TestWasmWebReactorIdleFailsUnderTheHarness(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	dir := clitest.TempDir(t)

	// main prints its expected output, then parks on a live subscription.
	file := writeWebTestFile(t, dir, "reactor_test.pr", `_web_subscribe(i32 capacity, i32 policy) i32 `+"`extern(\"promise_web_subscribe\") `target(web)"+`;
_web_channel(i32 sub_id) Channel[i32] `+"`extern(\"promise_web_channel\") `target(web)"+`;

main() `+"`test(expected: \"ready\\n\")"+` {
  sub_id := _web_subscribe(4, 0);
  ch := _web_channel(sub_id);
  print_line("ready");
  for h in ch {
    print_line("{h}");
  }
}
`)
	r := clitest.Run(t, bin, nil, "test", "-target", "wasm32-web", file)
	skipIfRuntimeUnobtainable(t, r.Combined())
	if r.ExitCode == 0 || !strings.Contains(r.Combined(), "harness: reactor idle") {
		t.Errorf("exit %d, want a failure naming the idle reactor\n%s", r.ExitCode, r.Combined())
	}
}

// The harness runs the continuations a program schedules for itself — here the
// timer that wakes main's sleep — and fails the program as an idle reactor only
// once none is pending while a registration, the subscription, stays live.
func TestWasmWebReactorIdleAfterLastContinuation(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	dir := clitest.TempDir(t)

	file := writeWebTestFile(t, dir, "sleeping_reactor_test.pr", `_web_subscribe(i32 capacity, i32 policy) i32 `+"`extern(\"promise_web_subscribe\") `target(web)"+`;
_web_channel(i32 sub_id) Channel[i32] `+"`extern(\"promise_web_channel\") `target(web)"+`;

main() `+"`test(expected: \"ready\\nwoke\\n\")"+` {
  sub_id := _web_subscribe(4, 0);
  ch := _web_channel(sub_id);
  print_line("ready");
  sleep(Duration.from_millis(20)); // sleep-ok: the continuation that wakes it is the subject
  print_line("woke");
  for h in ch {
    print_line("{h}");
  }
}
`)
	r := clitest.Run(t, bin, nil, "test", "-target", "wasm32-web", file)
	skipIfRuntimeUnobtainable(t, r.Combined())
	if out := r.Combined(); r.ExitCode == 0 || !strings.Contains(out, "harness: reactor idle") || !strings.Contains(out, "woke") {
		t.Errorf("exit %d, want the woken program to fail as an idle reactor\n%s", r.ExitCode, out)
	}
}

// A batch binary is never a reactor, so a test body parked on a live
// subscription — which no host will ever feed — fails rather than passing.
func TestWasmWebBatchTestParkedOnLiveSubscriptionFails(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	dir := clitest.TempDir(t)

	file := writeWebTestFile(t, dir, "parked_test.pr", `_web_subscribe(i32 capacity, i32 policy) i32 `+"`extern(\"promise_web_subscribe\") `target(web)"+`;
_web_channel(i32 sub_id) Channel[i32] `+"`extern(\"promise_web_channel\") `target(web)"+`;

test_parks() `+"`test"+` {
  sub_id := _web_subscribe(4, 0);
  ch := _web_channel(sub_id);
  v := <-ch;
}
`)
	r := clitest.Run(t, bin, nil, "test", "-target", "wasm32-web", file)
	skipIfRuntimeUnobtainable(t, r.Combined())
	// The result line names the test, so it was built and run.
	if out := r.Combined(); r.ExitCode == 0 || !strings.Contains(out, "test_parks [wasm32-web]") || strings.Contains(out, "PASS") {
		t.Errorf("exit %d, want the parked test to run and fail\n%s", r.ExitCode, out)
	}
}

// A wait in a plain function body (a closure's, #717) that nothing will ever
// satisfy is a deadlock — but on wasm32-web only once no armed timer could still
// make a goroutine runnable (docs/web-apps.md "The liveness rule"). An unrelated
// goroutine sleeps first: the wait must wait for its timer and let it run, and
// only then, with nothing runnable and no timer armed, abort with the deadlock
// message and exit 2 — not abort while the timer is armed, and not wait forever
// once it has expired. One case per wait site: the shared wait pump (a channel
// receive), a task receive, and the join of a task dropped un-awaited.
func TestWasmWebPlainWaitDeadlocksOnceNoTimerIsArmed(t *testing.T) {
	t.Parallel()
	bin := clitest.Bin(t)
	dir := clitest.TempDir(t)

	const prelude = "main() {\n" +
		"  never := channel[int](1);\n" +
		"  s := go {\n" +
		"    sleep(Duration.from_millis(10)); // sleep-ok: the armed timer is the subject\n" +
		"    print_line(\"slept\");\n" +
		"  };\n"
	const epilogue = "  f(never);\n  <-s;\n}\n"
	for _, c := range []struct{ name, closure string }{
		{"channel_receive", "  f := |Channel[int] c| {\n" +
			"    v := <-c;\n" +
			"    print_line(\"received\");\n" +
			"  };\n"},
		{"task_receive", "  f := |Channel[int] c| {\n" +
			"    t := go {\n      v := <-c;\n    };\n" +
			"    <-t;\n" +
			"    print_line(\"received\");\n" +
			"  };\n"},
		{"task_drop", "  f := |Channel[int] c| {\n" +
			"    t := go {\n      v := <-c;\n    };\n" +
			"  };\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			file := writeWebTestFile(t, dir, c.name+".pr", prelude+c.closure+epilogue)
			r := clitest.Run(t, bin, nil, "run", "-target", "wasm32-web", file)
			skipIfRuntimeUnobtainable(t, r.Combined())
			if !strings.Contains(r.Stdout, "slept") {
				t.Errorf("the wait must let the sleeper wake and run before it decides there is a deadlock:%s", r.Detail())
			}
			if r.ExitCode != 2 || !strings.Contains(r.Stderr, "fatal: all goroutines are asleep - deadlock!") {
				t.Errorf("with nothing runnable and no timer armed, the wait must abort as a deadlock (exit 2):%s", r.Detail())
			}
			if strings.Contains(r.Stdout, "received") {
				t.Errorf("nothing was sent, so the wait cannot have completed:%s", r.Detail())
			}
		})
	}
}
