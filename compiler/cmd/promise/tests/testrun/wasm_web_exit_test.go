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
