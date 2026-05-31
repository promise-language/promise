package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// T0749: extractPassingTestNames recovers per-test names for passing batch tests
// from a single-file child's captured output, which the parent's compact summary
// otherwise discards.

func TestExtractPassingTestNames_Batch(t *testing.T) {
	out := `pass (0.009s) test_and
pass (0.001s) test_or
pass (0.002s) test_not

3 passed, 0 failed (0.012s)`
	got := extractPassingTestNames(out)
	want := []string{"test_and", "test_or", "test_not"}
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].name != w {
			t.Errorf("record[%d].name = %q, want %q", i, got[i].name, w)
		}
	}
	if got[0].elapsed != 0.009 {
		t.Errorf("record[0].elapsed = %v, want 0.009", got[0].elapsed)
	}
}

func TestExtractPassingTestNames_WasmSuffix(t *testing.T) {
	// Cross-target child output appends " [wasm32-wasi]" to each result line;
	// the single-token name capture must drop it.
	out := `pass (0.009s) test_and [wasm32-wasi]
pass (0.001s) test_or [wasm32-wasi]`
	got := extractPassingTestNames(out)
	if len(got) != 2 || got[0].name != "test_and" || got[1].name != "test_or" {
		t.Fatalf("wasm suffix not stripped: %+v", got)
	}
}

func TestExtractPassingTestNames_Snapshot(t *testing.T) {
	// Snapshot/E2E single-file output uses uppercase PASS with no test name and
	// must yield nothing (file-only identity is preserved upstream).
	out := `PASS (0.230s)

1 passed, 0 failed (0.230s)`
	if got := extractPassingTestNames(out); len(got) != 0 {
		t.Fatalf("snapshot output should yield no records, got %+v", got)
	}
}

func TestExtractPassingTestNames_IgnoresNonPass(t *testing.T) {
	out := `pass (0.001s) test_ok
FAIL (0.003s) test_broken
  panic: assertion failed
LEAK (0.001s) test_leaky
  leak: 1 allocations not freed

1 passed, 1 failed, 1 leaked (0.005s)`
	got := extractPassingTestNames(out)
	if len(got) != 1 || got[0].name != "test_ok" {
		t.Fatalf("expected only test_ok, got %+v", got)
	}
}

func TestExtractPassingTestNames_IgnoresAggregatedFileLine(t *testing.T) {
	// Defensive: a compact multi-file pass line (`pass (Xs) e2e/basics.pr (3
	// tests)`) names a .pr file, not a test function, and must never become a
	// per-test record — that would re-introduce file-as-test-name identity.
	out := `pass (0.044s) tests/std/bool_test.pr (6 tests)
pass (0.009s) test_real`
	got := extractPassingTestNames(out)
	if len(got) != 1 || got[0].name != "test_real" {
		t.Fatalf("expected only test_real (file line skipped), got %+v", got)
	}
}

func TestWriteTestReport_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	records := []reportTestRecord{
		{File: "std/bool_test.pr", Test: "test_and", Elapsed: 0.009},
		{File: "std/bool_test.pr", Test: "test_or", Elapsed: 0.001},
	}
	writeTestReport(path, records)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var rep testReport
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if len(rep.Passing) != 2 || rep.Passing[0].Test != "test_and" || rep.Passing[1].File != "std/bool_test.pr" {
		t.Fatalf("round-trip mismatch: %+v", rep.Passing)
	}
}

func TestWriteTestReport_WriteErrorIsBestEffort(t *testing.T) {
	// A path whose parent directory does not exist makes os.WriteFile fail.
	// writeTestReport is best-effort: it must warn (not panic, not exit) and
	// leave no file behind, so a sidecar write failure never fails the run.
	bad := filepath.Join(t.TempDir(), "missing-dir", "report.json")
	writeTestReport(bad, []reportTestRecord{{File: "x.pr", Test: "t", Elapsed: 0.1}})
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Fatalf("expected no file at %s after failed write, stat err = %v", bad, err)
	}
}
