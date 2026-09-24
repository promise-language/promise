package common

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// testUsage is the one spelling of what this tool accepts.
const testUsage = "usage: bin/test [go|promise|tools|all] [--local|--shared] [--wasm] [--wasm-web] [--clean]"

// testOptions is one bin/test run's command line, parsed.
type testOptions struct {
	// suite is the positional mode: "go" (compiler Go tests), "promise"
	// (Promise tests), "tools" (tools/build Go tests), "all" (all three), or
	// "default" — no mode given — which is the CI set: go + promise, no tools.
	suite string
	// shared uses the shared ~/.promise instead of the repo-local
	// .promise-home/. Off by default; -local says the default out loud.
	shared bool
	// wasm and wasmWeb add the wasm32-wasi and wasm32-web Promise suites to the
	// host one, under wasmtime and Node respectively.
	wasm    bool
	wasmWeb bool
	// clean wipes .promise-home/ first and runs the Go suites uncached.
	clean bool
}

// runsCompiler, runsTools and runsPromise are which phases the parsed mode
// selects. The mapping lives on the options, next to the mode that decides it,
// so a unit test can reach it: RunTest cleans, stages a cache and builds a
// compiler before the first phase, so a mapping inlined there could only be
// asserted by running the whole pipeline.
func (o testOptions) runsCompiler() bool {
	return o.suite == "default" || o.suite == "all" || o.suite == "go"
}

func (o testOptions) runsTools() bool {
	return o.suite == "all" || o.suite == "tools"
}

func (o testOptions) runsPromise() bool {
	return o.suite == "default" || o.suite == "all" || o.suite == "promise"
}

// parseTestArgs parses bin/test's command line and does nothing else — no
// clean, no cache setup, no build, no subprocess. It is separate from RunTest
// so that flag coverage can be a pure unit test rather than a real test run:
// proving that -clean parsed used to mean wiping a home and building a
// compiler, and -shared -clean used to reach the shared home's removal (T2091,
// T2095).
//
// A rejected command line yields the zero testOptions, so a caller that reads
// the options before the error cannot act on a half-parsed one.
func parseTestArgs(args []string) (testOptions, error) {
	opts := testOptions{suite: "default"}
	for _, arg := range NormalizeArgs(args) {
		switch arg {
		case "go", "promise", "tools", "all":
			opts.suite = arg
		case "-local":
			// explicit local — no-op (the default)
		case "-shared":
			opts.shared = true
		case "-wasm":
			opts.wasm = true
		case "-wasm-web":
			opts.wasmWeb = true
		case "-clean":
			opts.clean = true
		default:
			return testOptions{}, errors.New(testUsage)
		}
	}
	// After the loop, not inside it: an unknown argument means the command line
	// was mistyped, and answering a mistype with a complaint about a flag
	// combination sends its author past the mistake they actually made.
	if opts.shared && opts.clean {
		return testOptions{}, errCleanWithShared
	}
	return opts, nil
}

// RunTest builds the compiler and runs test suites. Its command line is
// testUsage, parsed by parseTestArgs.
//
// The "tools" suite is opt-in — `bin/test tools` or `bin/test all`, never the
// default CI set — because several of its tests assume a POSIX shell/toolchain
// and 8.3-free temp paths, so they are unreliable across CI runners; bin/verify
// runs them locally where that environment holds. Flows are never run here (no
// flow-sdk workspace on CI).
func RunTest(root string, args []string) error {
	start := time.Now()
	opts, err := parseTestArgs(args)
	if err != nil {
		return err
	}

	// Clean first if requested, before SetupLocalCache so the local home is
	// recreated empty. The Go suites then skip saved results (goTestFlags).
	if opts.clean {
		if err := Clean(root, CleanOptions{}); err != nil {
			return fmt.Errorf("clean: %w", err)
		}
	}

	// Default to local cache; -shared opts into ~/.promise
	if !opts.shared {
		if err := SetupLocalCache(root); err != nil {
			return fmt.Errorf("setup local cache: %w", err)
		}
	}

	// Build first
	Progress().Println("Building...")
	if err := RunBuild(root, nil); err != nil {
		return fmt.Errorf("build: %w", err)
	}

	// Compiler Go tests — the CI set.
	if opts.runsCompiler() {
		Progress().Println("\nRunning go tests (compiler)...")
		if err := RunGoTests(root, goTestFlags(opts.clean)...); err != nil {
			return fmt.Errorf("go tests (compiler): %w", err)
		}
	}

	// Tools/build Go tests — opt-in only (`bin/test tools` / `bin/test all`), not
	// part of the default CI set. See the RunTest doc comment for why.
	if opts.runsTools() {
		Progress().Println("\nRunning go tests (tools)...")
		if err := RunToolsGoTests(root, goTestFlags(opts.clean)...); err != nil {
			return fmt.Errorf("go tests (tools): %w", err)
		}
	}

	// Promise tests
	if opts.runsPromise() {
		Progress().Println("\nRunning promise tests (host)...")
		_, err := RunPromiseTests(root, "")
		if err != nil {
			return fmt.Errorf("promise tests (host): %w", err)
		}

		if opts.wasm {
			if Which("wasmtime") == "" { // path-ok: the documented wasm32-wasi test runtime
				return fmt.Errorf("wasmtime not found — install with: bin/prereqs --wasm")
			}
			Progress().Println("\nRunning promise tests (wasm32-wasi)...")
			_, err = RunPromiseTests(root, "wasm32-wasi")
			if err != nil {
				return fmt.Errorf("promise tests (wasm32-wasi): %w", err)
			}
		}

		if opts.wasmWeb {
			if Which("node") == "" { // path-ok: the documented wasm32-web test runtime (Node 20+)
				return fmt.Errorf("node not found — install Node.js 20+ (see bin/prereqs)")
			}
			Progress().Println("\nRunning promise tests (wasm32-web)...")
			_, err = RunPromiseTests(root, "wasm32-web")
			if err != nil {
				return fmt.Errorf("promise tests (wasm32-web): %w", err)
			}
		}
	}

	elapsed := time.Since(start).Round(time.Millisecond)
	Progress().Printf("\nAll tests passed (%s)\n", elapsed)
	return nil
}

// goTestConcurrency returns the -p and -parallel values for a suite whose tests
// spawn compilers.
//
// Go's defaults are both GOMAXPROCS, and they multiply: up to NumCPU package
// binaries, each running up to NumCPU t.Parallel() tests. That is right for
// tests that are just goroutines, and wrong here — most of these tests shell out
// to `bin/promise`, a ~250 MB multi-threaded process that fans out to opt/llc of
// its own. At the defaults a 12-core host ran 156 concurrent processes and 9.4 GB
// for the compiler suite, deep into swap (T1817).
//
// So pick factors whose product is about NumCPU — the machine's actual capacity —
// rather than NumCPU each. Keeping -p ≥ 2 preserves cross-package overlap, which
// is what T1776's split was for. Measured on that host, cold test cache: 247s at
// the defaults (156 procs, 9.4 GB) versus 210s at -p 4 -parallel 4 (40 procs,
// 5.6 GB). Oversubscription was costing wall time, not buying it.
func goTestConcurrency() (p, parallel int) {
	n := runtime.NumCPU()
	p = int(math.Ceil(math.Sqrt(float64(n))))
	p = max(p, 2)
	parallel = max((n+p-1)/p, 2)
	return p, parallel
}

// goTestConcurrencyArgs renders goTestConcurrency as `go test` flags.
func goTestConcurrencyArgs() []string {
	p, parallel := goTestConcurrency()
	return []string{"-p", strconv.Itoa(p), "-parallel", strconv.Itoa(parallel)}
}

// goTestArgs is the whole `go test` command every Go module's suite runs —
// verify's phases below and the tested:go gate alike. One spelling, so the gate
// cannot come to measure something narrower than what verify requires.
//
// -timeout 30m: see RunTests — the codegen package exceeds Go's default
// 10m per-package limit on slow runners (GitHub windows-amd64).
//
// extra flags (goTestFlags) go ahead of the package pattern; with none, this is
// exactly the command the gate measures.
func goTestArgs(extra ...string) []string {
	args := append([]string{"test", "-timeout", "30m"}, goTestConcurrencyArgs()...)
	args = append(args, extra...)
	return append(args, "./...")
}

// goTestFlags is what a run adds to goTestArgs. A --clean run must not reuse
// saved test results and says so with -count=1, which affects that run alone;
// `go clean -testcache` would instead stamp the host-global
// $GOCACHE/testexpire.txt and expire every saved result in every clone on the
// machine.
func goTestFlags(clean bool) []string {
	if clean {
		return []string{"-count=1"}
	}
	return nil
}

// RunGoTests runs only compiler Go unit tests. Used by verify. extra flags are
// passed to `go test` ahead of the package pattern.
func RunGoTests(root string, extra ...string) error {
	compilerDir := filepath.Join(root, "compiler")
	return runInRendered(compilerDir, Progress(), isGoTestPassLine, "go", goTestArgs(extra...)...)
}

// RunToolsGoTests runs Go unit tests for the tools/build module. Same
// concurrency reasoning as RunGoTests: these tests drive the build tools, which
// drive the compiler.
func RunToolsGoTests(root string, extra ...string) error {
	toolsDir := filepath.Join(root, "tools", "build")
	return runInRendered(toolsDir, Progress(), isGoTestPassLine, "go", goTestArgs(extra...)...)
}

// RunFlowsGoTests runs Go unit tests for the flows module.
// Returns (skipped=true, nil) when flows/go.mod or flow-sdk/go.mod is absent.
//
// The same argv as the other two, from goTestArgs, and the same extra flags:
// tested:go sweeps flows/ when this clone has one, and a module that quietly ran
// a different command than its siblings is the drift this file's one-spelling
// rule exists to prevent. Without `extra` here, a --clean run would force the
// compiler and tools suites to re-run while flows replayed cached results.
func RunFlowsGoTests(root string, extra ...string) (skipped bool, err error) {
	if !Exists(filepath.Join(root, "flows", "go.mod")) {
		return true, nil
	}
	if !Exists(filepath.Join(root, "flow-sdk", "go.mod")) {
		fmt.Fprintf(os.Stderr, "warning: skipping flows tests — flow-sdk/ not present (run ./make to fetch)\n")
		return true, nil
	}
	flowsDir := filepath.Join(root, "flows")
	return false, runInRendered(flowsDir, Progress(), isGoTestPassLine, "go", goTestArgs(extra...)...)
}

// promiseTestTimeoutArgs returns the per-test timeout flags for the given target
// (empty = host). WASM targets execute under wasmtime/Node and, under full
// parallel suite load (~650 processes at NumCPU), a trivial test's execution can
// be CPU-starved past the tight 10s host ceiling (T1334; cf. B0108, which raised
// the WASM *compile* backstop). Scale WASM per-test timeouts 3× so both the
// default and annotated timeouts get proportional headroom. This only raises the
// kill ceiling — it adds no runtime cost to passing tests, and host runs (scale
// 1.0) are unaffected, including their cache keys.
func promiseTestTimeoutArgs(target string) []string {
	args := []string{"-timeout", "10"}
	if strings.HasPrefix(target, "wasm") { // wasm32-wasi and wasm32-web
		args = append(args, "-timeout-scale", "3")
	}
	return args
}

// promiseTestProgressArgs forwards this process's resolved render mode to a
// child `promise test`. The child's stdout is a pipe (RunTee captures it), so it
// can never detect the user's terminal itself — the outermost process is the
// only one that can, and it says so explicitly (T1888).
func promiseTestProgressArgs() []string {
	return []string{"-progress", Progress().Mode().String()}
}

// promiseSuiteTargets is WHAT "the host Promise suite" is, spelled once. verify
// runs it, bin/test runs it and the tested:promise gate measures it, so a target
// added to one is added to all three — the tool and the gate cannot come to
// disagree about the subject.
func promiseSuiteTargets() []string {
	return []string{"tests/...", "modules/...", "examples/...", "tools/stub/..."}
}

// RunPromiseTests runs Promise tests for the given target (empty = host).
// Returns captured stdout (even on failure) and any error.
func RunPromiseTests(root, target string) (string, error) {
	promiseBin := filepath.Join(root, "bin", BinaryName())
	args := append([]string{"test"}, promiseTestTimeoutArgs(target)...)
	args = append(args, promiseTestProgressArgs()...)
	if target != "" {
		args = append(args, "-target", target)
	}
	args = append(args, promiseSuiteTargets()...)
	// The child owns the transient line for the duration of the run; drop ours
	// first so the two never fight over the same screen row.
	Progress().Clear()
	return RunTee(root, promiseBin, args...)
}

// RunPromiseTestsCapture is like RunPromiseTests but tees test output to stderr
// instead of stdout, keeping stdout clean for structured output (e.g. JSON).
func RunPromiseTestsCapture(root, target string) (string, error) {
	promiseBin := filepath.Join(root, "bin", BinaryName())
	args := append([]string{"test"}, promiseTestTimeoutArgs(target)...)
	args = append(args, promiseTestProgressArgs()...)
	if target != "" {
		args = append(args, "-target", target)
	}
	args = append(args, promiseSuiteTargets()...)
	Progress().Clear()
	return RunTeeStderr(root, promiseBin, args...)
}

// RunPromiseTestsJSON runs the Promise test suite with --json, returning the
// raw newline-delimited JSON (one record per eligible test) from stdout. Human
// progress streams to stderr. The captured JSONL is returned even when tests
// fail (non-zero exit), so the gate can always build its per-test report. When
// target is non-empty it cross-compiles for that target. T0763.
func RunPromiseTestsJSON(root, target string) (string, error) {
	promiseBin := filepath.Join(root, "bin", BinaryName())
	args := append([]string{"test"}, promiseTestTimeoutArgs(target)...)
	args = append(args, "--json")
	if target != "" {
		args = append(args, "-target", target)
	}
	args = append(args, promiseSuiteTargets()...)
	return RunCaptureStdout(root, promiseBin, args...)
}
