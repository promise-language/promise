package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/types"
)

// T0689: parseMemoryLimitArg unit tests — size grammar, opt-out, error cases.

func TestParseMemoryLimitArg(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		// Opt-out
		{"0", 0, false},
		// Binary multipliers (KB/MB/GB use 1024 by convention)
		{"1B", 1, false},
		{"1KB", 1024, false},
		{"1MB", 1 << 20, false},
		{"2GB", 2 << 30, false},
		{"512MB", 512 << 20, false},
		// Explicit binary suffixes
		{"1KiB", 1024, false},
		{"1MiB", 1 << 20, false},
		{"1GiB", 1 << 30, false},
		// Case-insensitive
		{"256mb", 256 << 20, false},
		{"4gb", 4 << 30, false},
		// Errors
		{"", 0, true},
		{"100", 0, true},    // missing unit
		{"abc", 0, true},    // not a number
		{"-1MB", 0, true},   // negative
		{"1XB", 0, true},    // unknown unit
		{"1MB1KB", 0, true}, // garbage
	}
	for _, c := range cases {
		got, err := parseMemoryLimitArg(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseMemoryLimitArg(%q) = %d, nil; want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMemoryLimitArg(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseMemoryLimitArg(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestParseMemoryLimitArg_RejectsBareNumbers confirms that bare numeric strings
// (other than "0") are rejected with a unit-required message.
func TestParseMemoryLimitArg_RejectsBareNumbers(t *testing.T) {
	_, err := parseMemoryLimitArg("100")
	if err == nil {
		t.Fatal("expected error for bare number")
	}
	if !strings.Contains(err.Error(), "unit") {
		t.Errorf("error message should mention unit requirement; got: %v", err)
	}
}

// TestMemoryLimitHarnessReportsMemlimit is an end-to-end test: compile and run
// a Promise test program that deliberately overruns a low memory limit; assert
// the harness reports a MEMLIMIT outcome and exits non-zero. Uses the binary
// produced by `bin/build` (PROMISE_TEST_BIN env override) and skips if not set
// — keeps the unit-test path fast while still allowing CI to opt in.
func TestMemoryLimitHarnessReportsMemlimit(t *testing.T) {
	promiseBin := os.Getenv("PROMISE_TEST_BIN")
	if promiseBin == "" {
		// Fall back to repo's bin/promise relative to PWD.
		// go test runs with cwd = the test's package dir, which is
		// compiler/cmd/promise — so the binary is at ../../../bin/promise.
		candidate := filepath.Join("..", "..", "..", "bin", "promise")
		if _, err := os.Stat(candidate); err == nil {
			promiseBin = candidate
		} else {
			t.Skip("set PROMISE_TEST_BIN or build via bin/build to run this end-to-end test")
		}
	}

	tmp, err := os.CreateTemp("", "memlimit_runaway_*.pr")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	source := `test_runaway() ` + "`test(memory_limit: \"1MB\")" + ` {
  v := Vector[int]();
  for i in 0..1000000 {
    v.push(i);
  }
}
`
	if _, err := tmp.WriteString(source); err != nil {
		t.Fatal(err)
	}
	tmp.Close()

	cmd := exec.Command(promiseBin, "test", tmp.Name())
	output, runErr := cmd.CombinedOutput()
	combined := string(output)

	if runErr == nil {
		t.Fatalf("expected non-zero exit, got success.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "MEMLIMIT") {
		t.Errorf("expected output to contain 'MEMLIMIT'.\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "memory limit") {
		t.Errorf("expected output to mention 'memory limit'.\nOutput:\n%s", combined)
	}
}

// T0738: buildChildTestArgs must forward -memory-limit to child `promise test`
// processes in multi-file runs, gated on memoryLimitExplicit. Without this the
// children silently fall back to their own 2 GiB default.

// hasConsecutive reports whether args contains a, immediately followed by b.
func hasConsecutive(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}

func TestBuildChildTestArgs_ForwardsMemoryLimit(t *testing.T) {
	cfg := testTimeoutConfig{
		defaultTimeout:      60 * time.Second,
		scale:               1.0,
		compileTimeout:      10 * time.Minute,
		defaultMemoryBytes:  8 << 20, // 8 MiB
		memoryLimitExplicit: true,
	}
	args := buildChildTestArgs(cfg, "", false, false)
	if !hasConsecutive(args, "-memory-limit", "8388608B") {
		t.Errorf("expected '-memory-limit 8388608B' in child args; got %v", args)
	}
}

func TestBuildChildTestArgs_OmitsMemoryLimitWhenNotExplicit(t *testing.T) {
	cfg := testTimeoutConfig{
		defaultTimeout:      60 * time.Second,
		scale:               1.0,
		compileTimeout:      10 * time.Minute,
		defaultMemoryBytes:  defaultMemoryLimitBytes, // 2 GiB default, but not explicit
		memoryLimitExplicit: false,
	}
	args := buildChildTestArgs(cfg, "", false, false)
	for _, a := range args {
		if a == "-memory-limit" {
			t.Errorf("expected no -memory-limit when not explicit; got %v", args)
		}
	}
}

func TestBuildChildTestArgs_ForwardsOptOut(t *testing.T) {
	// `-memory-limit 0` (opt-out) must round-trip as "0B" so the child also
	// disables the per-test limit instead of applying its own 2 GiB default.
	cfg := testTimeoutConfig{
		defaultTimeout:      60 * time.Second,
		scale:               1.0,
		compileTimeout:      10 * time.Minute,
		defaultMemoryBytes:  0,
		memoryLimitExplicit: true,
	}
	args := buildChildTestArgs(cfg, "", false, false)
	if !hasConsecutive(args, "-memory-limit", "0B") {
		t.Errorf("expected '-memory-limit 0B' (opt-out) in child args; got %v", args)
	}
	// And "0B" must parse back to 0 (byte-exact round-trip).
	got, err := parseMemoryLimitArg("0B")
	if err != nil || got != 0 {
		t.Errorf("parseMemoryLimitArg(\"0B\") = %d, %v; want 0, nil", got, err)
	}
}

// TestBuildChildTestArgs_ForwardsAllFlags locks the full child-arg contract:
// every non-default config flag must be forwarded, in order, so a future
// refactor of buildChildTestArgs can't silently drop one (e.g. -target or
// -coverage). Asserts the exact slice when all branches are taken.
func TestBuildChildTestArgs_ForwardsAllFlags(t *testing.T) {
	cfg := testTimeoutConfig{
		defaultTimeout:      30 * time.Second,
		scale:               2.0,
		min:                 1 * time.Second,
		max:                 10 * time.Second,
		compileTimeout:      5 * time.Minute, // != 10m default, so forwarded
		defaultMemoryBytes:  1 << 20,         // 1 MiB
		memoryLimitExplicit: true,
	}
	args := buildChildTestArgs(cfg, "wasm32-wasi", true /*coverage*/, true /*timePhases*/)
	want := []string{
		"test", "-timeout", "30s",
		"-timeout-scale", "2",
		"-timeout-min", "1s",
		"-timeout-max", "10s",
		"-target", "wasm32-wasi",
		"-coverage",
		"-time-phases",
		"-compile-timeout", "5m0s",
		"-memory-limit", "1048576B",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("child args mismatch:\n got: %v\nwant: %v", args, want)
	}
}

// TestBuildChildTestArgs_DefaultCompileTimeoutOmitted confirms the default 10m
// compile timeout is NOT forwarded (only non-default values are), so children
// fall back to their own identical default.
func TestBuildChildTestArgs_DefaultCompileTimeoutOmitted(t *testing.T) {
	cfg := testTimeoutConfig{
		defaultTimeout: 60 * time.Second,
		scale:          1.0,
		compileTimeout: 10 * time.Minute, // the default — must be omitted
	}
	args := buildChildTestArgs(cfg, "", false, false)
	for _, a := range args {
		if a == "-compile-timeout" {
			t.Errorf("default compile timeout should not be forwarded; got %v", args)
		}
	}
}

// TestMemoryLimitForwardedToMultiFileRun is an end-to-end test locking the
// T0738 fix: a runaway test (no memory_limit: annotation, so it depends on the
// CLI flag) plus a trivial test are run together via the multi-file
// runTestFiles path (two file paths). With -memory-limit 8MB forwarded to the
// children, the runaway trips MEMLIMIT. Pre-fix this passed (children ran at
// the 2 GiB default), so the test guards the forwarding specifically.
func TestMemoryLimitForwardedToMultiFileRun(t *testing.T) {
	promiseBin := os.Getenv("PROMISE_TEST_BIN")
	if promiseBin == "" {
		candidate := filepath.Join("..", "..", "..", "bin", "promise")
		if _, err := os.Stat(candidate); err == nil {
			promiseBin = candidate
		} else {
			t.Skip("set PROMISE_TEST_BIN or build via bin/build to run this end-to-end test")
		}
	}

	dir, err := os.MkdirTemp("", "memlimit_multifile_")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// Runaway: ~32 MiB net (4M ints × 8 bytes), well over the 8 MB CLI cap, and
	// crucially NO memory_limit: annotation — so only the forwarded CLI flag
	// can trip it.
	runaway := filepath.Join(dir, "runaway_test.pr")
	runawaySrc := "test_runaway() `test {\n" +
		"  v := Vector[int]();\n" +
		"  for i in 0..4000000 {\n" +
		"    v.push(i);\n" +
		"  }\n" +
		"  assert(v.len == 4000000, \"pushed all\");\n" +
		"}\n"
	if err := os.WriteFile(runaway, []byte(runawaySrc), 0644); err != nil {
		t.Fatal(err)
	}

	trivial := filepath.Join(dir, "trivial_test.pr")
	if err := os.WriteFile(trivial, []byte("test_trivial() `test {\n  assert(1 + 1 == 2, \"math\");\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Two file paths → multi-file runTestFiles path (spawns one child per file).
	cmd := exec.Command(promiseBin, "test", "-memory-limit", "8MB", runaway, trivial)
	output, runErr := cmd.CombinedOutput()
	combined := string(output)

	if runErr == nil {
		t.Fatalf("expected non-zero exit (runaway should trip the memory limit), got success.\nOutput:\n%s", combined)
	}
	// The multi-file aggregator reports a child memlimit trip as a
	// "(memory limit exceeded)" FAIL context line plus a "memlimit" summary
	// counter (the uppercase MEMLIMIT token is the single-file format).
	if !strings.Contains(combined, "memory limit exceeded") && !strings.Contains(combined, "memlimit") {
		t.Errorf("expected forwarded -memory-limit to trip a memory-limit outcome in the child.\nOutput:\n%s", combined)
	}
}

// TestComputeTestMemoryLimits exercises the per-test limit resolution:
// CLI default + per-test annotation overrides + opt-out (cfg=0, no annotation).
func TestComputeTestMemoryLimits(t *testing.T) {
	mkFunc := func(name string) *types.Func {
		return types.NewFunc(types.Pos{}, name, nil)
	}
	tests := []*types.Func{mkFunc("a"), mkFunc("b"), mkFunc("c")}

	// Case 1: CLI default 1 MiB, no annotations — every test gets 1 MiB.
	cfg := testTimeoutConfig{defaultMemoryBytes: 1 << 20}
	info := &sema.Info{}
	got := computeTestMemoryLimits(tests, info, cfg)
	if got == nil {
		t.Fatal("expected non-nil map when defaultMemoryBytes > 0")
	}
	for _, tt := range tests {
		if got[tt.Name()] != 1<<20 {
			t.Errorf("default: %s = %d, want %d", tt.Name(), got[tt.Name()], 1<<20)
		}
	}

	// Case 2: annotation overrides default.
	info.TestMemoryLimits = map[string]string{"b": "256MB"}
	got = computeTestMemoryLimits(tests, info, cfg)
	if got["a"] != 1<<20 {
		t.Errorf("a: %d, want default %d", got["a"], 1<<20)
	}
	if got["b"] != 256<<20 {
		t.Errorf("b: %d, want 256MB %d", got["b"], 256<<20)
	}
	if got["c"] != 1<<20 {
		t.Errorf("c: %d, want default %d", got["c"], 1<<20)
	}

	// Case 3: CLI = 0 AND no annotations → nil (accounting disabled entirely).
	cfg = testTimeoutConfig{defaultMemoryBytes: 0}
	info = &sema.Info{}
	got = computeTestMemoryLimits(tests, info, cfg)
	if got != nil {
		t.Errorf("expected nil when defaultMemoryBytes=0 and no annotations; got %v", got)
	}

	// Case 4: CLI = 0 but at least one annotation → all tests in the map,
	// the unannotated ones get 0 (no per-test limit) but accounting remains on.
	info.TestMemoryLimits = map[string]string{"b": "512MB"}
	got = computeTestMemoryLimits(tests, info, cfg)
	if got == nil {
		t.Fatal("expected non-nil map when an annotation exists")
	}
	if got["a"] != 0 {
		t.Errorf("a: %d, want 0 (no annotation, default=0)", got["a"])
	}
	if got["b"] != 512<<20 {
		t.Errorf("b: %d, want 512MB %d", got["b"], 512<<20)
	}
}

// TestParseMemoryLimitArg_Overflow confirms the overflow guard in
// parseMemoryLimitArg rejects values that would exceed int64.
func TestParseMemoryLimitArg_Overflow(t *testing.T) {
	// 16 EiB is 2^64 bytes; expressing it in GiB (1<<30) overflows int64.
	_, err := parseMemoryLimitArg("99999999999GB")
	if err == nil {
		t.Error("expected overflow error for huge value")
	}
}

// TestTestTimeoutConfigCacheStringIncludesMemLimit ensures the cache key changes
// when -memory-limit changes, so tests rebuild after the flag flips between
// values. Per T0689 the per-binary memory limit is baked into the test binary's
// generated main at codegen time.
func TestTestTimeoutConfigCacheStringIncludesMemLimit(t *testing.T) {
	c1 := testTimeoutConfig{defaultMemoryBytes: 0}
	c2 := testTimeoutConfig{defaultMemoryBytes: 1 << 20}
	c3 := testTimeoutConfig{defaultMemoryBytes: 2 << 30}

	s1 := c1.cacheString()
	s2 := c2.cacheString()
	s3 := c3.cacheString()

	if s1 == s2 {
		t.Errorf("cache keys for memlimit=0 and memlimit=1MB should differ; both = %q", s1)
	}
	if s2 == s3 {
		t.Errorf("cache keys for memlimit=1MB and memlimit=2GB should differ; both = %q", s2)
	}
	for _, s := range []string{s1, s2, s3} {
		if !strings.Contains(s, "memlimit:") {
			t.Errorf("cache key %q missing 'memlimit:' segment", s)
		}
	}
}
