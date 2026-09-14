package common

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestPromiseTestTimeoutArgs verifies that the per-test timeout flags carry a
// 3× scale on WASM targets (T1334) while host runs keep the bare 10s default.
func TestPromiseTestTimeoutArgs(t *testing.T) {
	cases := []struct {
		target string
		want   []string
	}{
		{"", []string{"-timeout", "10"}},
		{"wasm32-wasi", []string{"-timeout", "10", "-timeout-scale", "3"}},
		{"wasm32-web", []string{"-timeout", "10", "-timeout-scale", "3"}},
	}
	for _, c := range cases {
		got := promiseTestTimeoutArgs(c.target)
		if !slices.Equal(got, c.want) {
			t.Errorf("promiseTestTimeoutArgs(%q) = %v, want %v", c.target, got, c.want)
		}
	}
}

// TestRunFlowsGoTests_NoFlowsModule verifies that RunFlowsGoTests skips without
// error when flows/go.mod does not exist.
func TestRunFlowsGoTests_NoFlowsModule(t *testing.T) {
	root := t.TempDir()
	skipped, err := RunFlowsGoTests(root)
	if err != nil {
		t.Fatalf("RunFlowsGoTests: unexpected error: %v", err)
	}
	if !skipped {
		t.Error("RunFlowsGoTests: expected skipped=true when flows/go.mod absent")
	}
}

// TestRunFlowsGoTests_NoSDK verifies that RunFlowsGoTests skips when flows/go.mod
// exists but flow-sdk/go.mod does not.
func TestRunFlowsGoTests_NoSDK(t *testing.T) {
	root := t.TempDir()
	flowsDir := filepath.Join(root, "flows")
	if err := os.MkdirAll(flowsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flowsDir, "go.mod"), []byte("module example.com/flows\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// flow-sdk/ is absent — should skip with warning, not error.
	skipped, err := RunFlowsGoTests(root)
	if err != nil {
		t.Fatalf("RunFlowsGoTests: unexpected error: %v", err)
	}
	if !skipped {
		t.Error("RunFlowsGoTests: expected skipped=true when flow-sdk/go.mod absent")
	}
}

// TestRunToolsGoTests_TrivialModule verifies that RunToolsGoTests succeeds on a
// minimal Go module placed at tools/build/ inside a temp root. Using a temp
// module (rather than the real repo) avoids infinite recursion: running
// go test ./... on the real tools/build would re-invoke this test, spawning an
// unbounded chain of go test subprocesses.
func TestRunToolsGoTests_TrivialModule(t *testing.T) {
	root := t.TempDir()
	toolsDir := filepath.Join(root, "tools", "build")
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolsDir, "go.mod"), []byte("module example.com/tools\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Trivial test file so go test ./... has something to run.
	if err := os.WriteFile(filepath.Join(toolsDir, "noop_test.go"), []byte("package tools\n\nimport \"testing\"\n\nfunc TestNoop(t *testing.T) { t.Log(\"noop\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunToolsGoTests(root); err != nil {
		t.Fatalf("RunToolsGoTests: %v", err)
	}
}

// TestRunGoTests_TrivialModule is the same shape for verify's other Go phase.
// Both now assemble their command line from one goTestArgs(), which the
// tested:go gate reads too — so this is what says the shared spelling still
// runs the compiler module, not only the tools one.
func TestRunGoTests_TrivialModule(t *testing.T) {
	root := t.TempDir()
	compilerDir := filepath.Join(root, "compiler")
	if err := os.MkdirAll(compilerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(compilerDir, "go.mod"), []byte("module example.com/compiler\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(compilerDir, "noop_test.go"), []byte("package compiler\n\nimport \"testing\"\n\nfunc TestNoop(t *testing.T) { t.Log(\"noop\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunGoTests(root); err != nil {
		t.Fatalf("RunGoTests: %v", err)
	}
}

// TestGoTestFlags_CleanRunsUncached pins what replaced `go clean -testcache` in
// a --clean run: -count=1 on that run's own `go test`, ahead of the package
// pattern — and nothing at all for an ordinary run, so the ordinary command
// stays exactly the goTestArgs() the tested:go gate compares against.
func TestGoTestFlags_CleanRunsUncached(t *testing.T) {
	if got := goTestFlags(false); len(got) != 0 {
		t.Errorf("goTestFlags(false) = %v, want none", got)
	}
	if got, want := goTestArgs(goTestFlags(false)...), goTestArgs(); !slices.Equal(got, want) {
		t.Errorf("an ordinary run's go test = %v, want the gate's %v", got, want)
	}
	args := goTestArgs(goTestFlags(true)...)
	if n := len(args); n < 2 || args[n-2] != "-count=1" || args[n-1] != "./..." {
		t.Errorf("a --clean run's go test = %v, want it to end in -count=1 ./...", args)
	}
}

// TestRunTest_CleanWithSharedIsRefused: RunTest has no separate parser, so the
// refusal is pinned at the entry point, where it must come before the clean,
// the cache setup and the build. HOME is redirected, so a regression that took
// the verify lock or touched ~/.promise shows up in this test's own home.
func TestRunTest_CleanWithSharedIsRefused(t *testing.T) {
	home := cleanTestHome(t)
	for _, args := range [][]string{
		{"--shared", "--clean"},
		{"go", "--clean", "--shared"},
	} {
		if err := RunTest(t.TempDir(), args); !errors.Is(err, errCleanWithShared) {
			t.Errorf("RunTest(%v) = %v, want errCleanWithShared", args, err)
		}
	}
	if promise := filepath.Join(home, ".promise"); Exists(promise) {
		t.Errorf("a refused run must not take the verify lock or touch the shared home; %s exists", promise)
	}
}

// argsStubSource is a stand-in for bin/promise that prints the argument list it
// was given, one per line, so a test can assert on the child command line the
// Promise test phases assemble.
const argsStubSource = `package main

import (
	"fmt"
	"os"
)

func main() {
	for _, a := range os.Args[1:] {
		fmt.Println(a)
	}
}
`

// promiseArgsStubRoot builds a fake repo root whose bin/<promise> prints its
// arguments, and returns the root.
func promiseArgsStubRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "main.go")
	if err := os.WriteFile(src, []byte(argsStubSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(root, "bin", BinaryName()), src)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build args stub: %v", err)
	}
	return root
}

// TestRunPromiseTests_ForwardsProgressMode is the T1888 wire: the outermost
// process is the only one that can see the user's terminal, so every child
// `promise test` must be told the mode explicitly rather than sniffing a pipe.
// Asserted on the real command line, not on promiseTestProgressArgs alone.
func TestRunPromiseTests_ForwardsProgressMode(t *testing.T) {
	root := promiseArgsStubRoot(t)
	want := Progress().Mode().String()

	for _, tc := range []struct {
		name string
		run  func(string, string) (string, error)
	}{
		{"RunPromiseTests", RunPromiseTests},
		{"RunPromiseTestsCapture", RunPromiseTestsCapture},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, target := range []string{"", "wasm32-wasi"} {
				out, err := tc.run(root, target)
				if err != nil {
					t.Fatalf("target %q: %v\n%s", target, err, out)
				}
				args := strings.Split(out, "\n")
				if i := indexOfArg(args, "-progress"); i < 0 || i+1 >= len(args) {
					t.Fatalf("target %q: no -progress in child args %v", target, args)
				} else if args[i+1] != want {
					t.Errorf("target %q: forwarded -progress %q, want %q", target, args[i+1], want)
				}
				// The rest of the command line is unchanged by T1888.
				if args[0] != "test" {
					t.Errorf("target %q: first arg = %q, want \"test\"", target, args[0])
				}
				if (target != "") != (indexOfArg(args, "-target") >= 0) {
					t.Errorf("target %q: -target presence = %v", target, indexOfArg(args, "-target") >= 0)
				}
			}
		})
	}
}

// TestRunPromiseTestsJSON_HasNoProgressFlag pins the item's "--json mode is
// unaffected" constraint: the JSONL path must not gain a -progress flag, whose
// suppression would be meaningless there and whose presence would be a
// behaviour change on a gate path.
func TestRunPromiseTestsJSON_HasNoProgressFlag(t *testing.T) {
	root := promiseArgsStubRoot(t)
	out, err := RunPromiseTestsJSON(root, "")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	args := strings.Split(out, "\n")
	if i := indexOfArg(args, "-progress"); i >= 0 {
		t.Errorf("--json path gained a -progress flag: %v", args)
	}
	if indexOfArg(args, "--json") < 0 {
		t.Errorf("--json missing from %v", args)
	}
}

func indexOfArg(args []string, want string) int {
	for i, a := range args {
		if strings.TrimSpace(a) == want {
			return i
		}
	}
	return -1
}

// TestRunFlowsGoTests_TakesTheSameExtraFlags is the third module's half of the
// one-spelling rule. Before this, verify's --clean run passed -count=1 to the
// compiler and tools suites but not to flows, so flows alone replayed cached
// results on a run whose whole point was not to — a divergence in the same
// family as the one T2104 removed between bin/check and checked:go.
func TestRunFlowsGoTests_TakesTheSameExtraFlags(t *testing.T) {
	root := t.TempDir()
	flowsDir := filepath.Join(root, "flows")
	for _, mod := range []string{"flows", "flow-sdk"} {
		dir := filepath.Join(root, mod)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/"+mod+"\n\ngo 1.21\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	body := "package flows\n\nimport \"testing\"\n\nfunc TestNoop(t *testing.T) { t.Log(\"noop\") }\n"
	if err := os.WriteFile(filepath.Join(flowsDir, "noop_test.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// The flags a --clean run passes reach `go test` here too: a stale spelling
	// would make go reject the argv, or silently drop the flag.
	skipped, err := RunFlowsGoTests(root, goTestFlags(true)...)
	if err != nil {
		t.Fatalf("RunFlowsGoTests: %v", err)
	}
	if skipped {
		t.Error("flows/ and flow-sdk/ are both present, so the suite must not be skipped")
	}
}
