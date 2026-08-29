package testrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// TestMemoryLimitHarnessReportsMemlimit is an end-to-end test: compile and run
// a Promise test program that deliberately overruns a low memory limit; assert
// the harness reports a MEMLIMIT outcome and exits non-zero. Uses the binary
// produced by `bin/build` (PROMISE_TEST_BIN env override) and skips if not set
// — keeps the unit-test path fast while still allowing CI to opt in.
func TestMemoryLimitHarnessReportsMemlimit(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)

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
	// T1415: a memlimit abort also leaves tests unreported, but MEMLIMIT keeps
	// precedence — reporting INCOMPLETE here would lose the specific diagnostic.
	if strings.Contains(combined, "INCOMPLETE") {
		t.Errorf("MEMLIMIT must take precedence over INCOMPLETE.\nOutput:\n%s", combined)
	}
}

// T0738: buildChildTestArgs must forward -memory-limit to child `promise test`
// processes in multi-file runs, gated on memoryLimitExplicit. Without this the
// children silently fall back to their own 2 GiB default.

// hasConsecutive reports whether args contains a, immediately followed by b.
func TestMemoryLimitForwardedToMultiFileRun(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)

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
