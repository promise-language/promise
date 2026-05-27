package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
