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

// TestParseTestArgs_EveryFlagAndSuiteSetsItsOption covers the whole command
// line as a pure parse, which is the point of having a parser at all: every
// flag of bin/test used to be provable only by running the pipeline it selects
// (T2091).
func TestParseTestArgs_EveryFlagAndSuiteSetsItsOption(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want testOptions
	}{
		{"none", nil, testOptions{suite: "default"}},
		{"go", []string{"go"}, testOptions{suite: "go"}},
		{"promise", []string{"promise"}, testOptions{suite: "promise"}},
		{"tools", []string{"tools"}, testOptions{suite: "tools"}},
		{"all", []string{"all"}, testOptions{suite: "all"}},
		{"shared", []string{"--shared"}, testOptions{suite: "default", shared: true}},
		{"wasm", []string{"--wasm"}, testOptions{suite: "default", wasm: true}},
		{"wasm-web", []string{"--wasm-web"}, testOptions{suite: "default", wasmWeb: true}},
		{"clean", []string{"--clean"}, testOptions{suite: "default", clean: true}},
		{"local is the default said out loud", []string{"--local"}, testOptions{suite: "default"}},
		{"single dash", []string{"-wasm"}, testOptions{suite: "default", wasm: true}},
		{"repeated flag", []string{"--clean", "--clean"}, testOptions{suite: "default", clean: true}},
		{"last suite wins", []string{"go", "promise"}, testOptions{suite: "promise"}},
		// --local is a no-op, not the opposite of --shared: it does not unset a
		// --shared given alongside it. Pinned rather than left to be
		// rediscovered by someone who writes the pair expecting the last one to
		// win, as it does for the positional mode one line above.
		{"local does not unset shared", []string{"--shared", "--local"}, testOptions{suite: "default", shared: true}},
		{
			"all together",
			[]string{"all", "--shared", "--wasm", "--wasm-web"},
			testOptions{suite: "all", shared: true, wasm: true, wasmWeb: true},
		},
		{
			"all together, cleaning",
			[]string{"all", "--wasm", "--wasm-web", "--clean"},
			testOptions{suite: "all", wasm: true, wasmWeb: true, clean: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTestArgs(tc.args)
			if err != nil {
				t.Fatalf("parseTestArgs(%v) = %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("parseTestArgs(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

// TestParseTestArgs_CleanWithSharedIsRefused is the pure-parse half of the
// refusal: no lock, no filesystem, no build reached before it is decided.
//
// The message must NAME `bin/clean --shared`, as every other refusal in this
// tree names its replacement. Someone who typed `--shared --clean` wanted the
// shared cache cleared, and that is still a thing they can have — by an
// operator's explicit command rather than as a side effect of a test run. A
// bare "cannot be combined" leaves them to discover that on their own.
func TestParseTestArgs_CleanWithSharedIsRefused(t *testing.T) {
	for _, args := range [][]string{
		{"--shared", "--clean"},
		{"go", "--clean", "--shared"},
	} {
		got, err := parseTestArgs(args)
		if !errors.Is(err, errCleanWithShared) {
			t.Errorf("parseTestArgs(%v) = %v, want errCleanWithShared", args, err)
			continue
		}
		if !strings.Contains(err.Error(), "bin/clean --shared") {
			t.Errorf("parseTestArgs(%v) = %q, want it to name bin/clean --shared", args, err)
		}
		if got != (testOptions{}) {
			t.Errorf("a rejected command line must yield zero options, got %+v", got)
		}
	}
}

// TestParseTestArgs_Rejections covers the error paths. The last case is an
// ordering pin: a mistyped argument is reported as a mistype even when the rest
// of the line also happens to be a refused combination, so the message names
// the mistake its author actually made.
func TestParseTestArgs_Rejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"--unknown"}},
		{"unknown suite", []string{"nope"}},
		{"typo alongside a suite", []string{"go", "--wsam"}},
		// NormalizeArgs splits --wasm=1 into "-wasm" "1"; bin/test has no
		// valued flag, so the value is left over as an unknown argument.
		{"value on a boolean flag", []string{"--wasm=1"}},
		{"a typo is not a refused combination", []string{"--shared", "--clean", "--bogus"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTestArgs(tc.args)
			if err == nil {
				t.Fatalf("parseTestArgs(%v) = %+v, want an error", tc.args, got)
			}
			if err.Error() != testUsage {
				t.Errorf("parseTestArgs(%v) = %q, want exactly the usage line", tc.args, err)
			}
			if got != (testOptions{}) {
				t.Errorf("a rejected command line must yield zero options, got %+v", got)
			}
		})
	}
}

// TestTestOptions_SuiteSelectsItsPhases pins which phases each mode runs. The
// mapping is otherwise only exercised by a full bin/test run, which builds a
// compiler first — so a mode quietly losing a suite would be caught by nothing
// short of noticing the missing output.
func TestTestOptions_SuiteSelectsItsPhases(t *testing.T) {
	for _, tc := range []struct {
		suite                          string
		compiler, tools, promiseSuites bool
	}{
		{"default", true, false, true},
		{"go", true, false, false},
		{"promise", false, false, true},
		{"tools", false, true, false},
		{"all", true, true, true},
	} {
		t.Run(tc.suite, func(t *testing.T) {
			opts := testOptions{suite: tc.suite}
			if got := opts.runsCompiler(); got != tc.compiler {
				t.Errorf("%q runsCompiler = %v, want %v", tc.suite, got, tc.compiler)
			}
			if got := opts.runsTools(); got != tc.tools {
				t.Errorf("%q runsTools = %v, want %v", tc.suite, got, tc.tools)
			}
			if got := opts.runsPromise(); got != tc.promiseSuites {
				t.Errorf("%q runsPromise = %v, want %v", tc.suite, got, tc.promiseSuites)
			}
		})
	}
}

// TestRunTest_CleanWithSharedIsRefused is the wiring pin for the refusal
// parseTestArgs decides: RunTest parses before the clean, the cache setup and
// the build, so a refused command line has no side effect. HOME is redirected,
// so a regression that took the verify lock or touched ~/.promise shows up in
// this test's own home.
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

// TestRunTest_UnknownFlagReturnsUsageError is the wiring pin for the refusal's
// other half, the shape bin/clean already has: a mistyped command line returns
// the usage error without cleaning, staging a cache or building a compiler.
// Without it, only the --shared --clean pair was pinned as reaching RunTest's
// parse-first early return, and a typo is by far the likelier way in.
func TestRunTest_UnknownFlagReturnsUsageError(t *testing.T) {
	home := cleanTestHome(t)
	root := t.TempDir()
	err := RunTest(root, []string{"--wsam"})
	if err == nil {
		t.Fatal("expected a usage error for an unknown flag, got nil")
	}
	if err.Error() != testUsage {
		t.Errorf("got %q, want %q", err.Error(), testUsage)
	}
	// Nothing ran: no local home staged in the root, no shared home touched.
	if local := filepath.Join(root, ".promise-home"); Exists(local) {
		t.Errorf("a refused run must not stage a cache; %s exists", local)
	}
	if promise := filepath.Join(home, ".promise"); Exists(promise) {
		t.Errorf("a refused run must not touch the shared home; %s exists", promise)
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
