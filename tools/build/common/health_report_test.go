package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTestOutputMultiFile(t *testing.T) {
	output := `pass (0.004s) e2e/basics.pr (3 tests)
pass (0.001s) e2e/hello.pr
FAIL (0.005s) e2e/strings.pr (1/3 failed)
  test_split
    panic: assertion failed
FAIL (0.000s) broken.pr (compilation error)
  broken.pr:5:3: type Foo has no field 'bar'

568 passed, 2 failed (117 files, 30.810s)
FAILED:
  e2e/strings.pr: test_split
    panic: assertion failed`

	results := parseTestOutput(output)

	// Multi-file: one entry per file. The trailing "FAILED:" summary block
	// (after a blank line) is ignored — indented detail is consumed inline.
	if len(results) != 4 {
		t.Fatalf("expected 4 results, got %d: %+v", len(results), results)
	}

	r := results[0]
	if r.File != "e2e/basics.pr" || r.Test != "" || r.Outcome != "pass" || r.Elapsed != 0.004 {
		t.Errorf("result[0] = %+v", r)
	}

	r = results[1]
	if r.File != "e2e/hello.pr" || r.Test != "" || r.Outcome != "pass" || r.Elapsed != 0.001 {
		t.Errorf("result[1] = %+v", r)
	}

	// Failed file: file-level identity, failure detail folded into Context.
	r = results[2]
	if r.File != "e2e/strings.pr" || r.Test != "" || r.Outcome != "FAIL" {
		t.Errorf("result[2] = %+v", r)
	}
	if r.Context != "test_split\npanic: assertion failed" {
		t.Errorf("result[2].Context = %q", r.Context)
	}

	// Compilation error: file-level entry, Context = error message.
	r = results[3]
	if r.File != "broken.pr" || r.Test != "" || r.Outcome != "FAIL" || r.Elapsed != 0.0 {
		t.Errorf("result[3] = %+v", r)
	}
	if r.Context != "broken.pr:5:3: type Foo has no field 'bar'" {
		t.Errorf("result[3].Context = %q", r.Context)
	}
}

func TestParseTestOutputSingleFile(t *testing.T) {
	output := `pass (0.001s) test_add
pass (0.002s) test_sub
LEAK (0.001s) test_leaky
  leak: 1 allocations not freed
FAIL (0.003s) test_broken
  panic: assertion failed: expected 3, got 4
TIMEOUT (0.100s) test_stuck
  timeout: exceeded 60s limit

2 passed, 1 failed, 1 leaked, 1 timed out (0.423s)`

	results := parseTestOutput(output)

	if len(results) != 5 {
		t.Fatalf("expected 5 results, got %d: %+v", len(results), results)
	}

	r := results[0]
	if r.Test != "test_add" || r.Outcome != "pass" || r.Elapsed != 0.001 {
		t.Errorf("result[0] = %+v", r)
	}

	r = results[1]
	if r.Test != "test_sub" || r.Outcome != "pass" || r.Elapsed != 0.002 {
		t.Errorf("result[1] = %+v", r)
	}

	r = results[2]
	if r.Test != "test_leaky" || r.Outcome != "LEAK" || r.Context != "leak: 1 allocations not freed" {
		t.Errorf("result[2] = %+v", r)
	}

	r = results[3]
	if r.Test != "test_broken" || r.Outcome != "FAIL" || r.Context != "panic: assertion failed: expected 3, got 4" {
		t.Errorf("result[3] = %+v", r)
	}

	r = results[4]
	if r.Test != "test_stuck" || r.Outcome != "TIMEOUT" || r.Context != "timeout: exceeded 60s limit" {
		t.Errorf("result[4] = %+v", r)
	}
}

func TestParseTestOutputMultipleFailedTests(t *testing.T) {
	output := `FAIL (0.010s) tests/e2e/errors.pr (2/5 failed)
  test_divide_by_zero
    panic: division by zero
  test_overflow
    panic: integer overflow`

	results := parseTestOutput(output)

	// Multi-file: a single file-level entry; per-test names and panic
	// contexts are folded into Context. T0742.
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(results), results)
	}

	r := results[0]
	if r.File != "tests/e2e/errors.pr" || r.Test != "" || r.Outcome != "FAIL" {
		t.Errorf("result[0] = %+v", r)
	}
	want := "test_divide_by_zero\npanic: division by zero\ntest_overflow\npanic: integer overflow"
	if r.Context != want {
		t.Errorf("result[0].Context = %q, want %q", r.Context, want)
	}
}

func TestParseTestOutputEmpty(t *testing.T) {
	results := parseTestOutput("")
	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(results))
	}
}

func TestParseTestOutputLeakFile(t *testing.T) {
	output := `LEAK (0.003s) tests/std/vector_test.pr (1/4 leaked)
  test_vector_push
    leak: 2 allocations not freed`

	results := parseTestOutput(output)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(results), results)
	}

	r := results[0]
	if r.File != "tests/std/vector_test.pr" || r.Test != "" || r.Outcome != "LEAK" {
		t.Errorf("result[0] = %+v", r)
	}
	if r.Context != "test_vector_push\nleak: 2 allocations not freed" {
		t.Errorf("result[0].Context = %q", r.Context)
	}
}

func TestParseTestOutputWasmTarget(t *testing.T) {
	output := `pass (4.570s) tests/arrays/fixed_string_forin_test.pr (5 tests) [wasm32-wasi]
pass (3.844s) examples/04_ownership/drop.pr [wasm32-wasi]
FAIL (0.005s) tests/e2e/strings.pr (1/3 failed) [wasm32-wasi]
  test_split
    panic: assertion failed`

	results := parseTestOutput(output)

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d: %+v", len(results), results)
	}

	r := results[0]
	if r.File != "tests/arrays/fixed_string_forin_test.pr" || r.Test != "" || r.Outcome != "pass" {
		t.Errorf("result[0] = %+v", r)
	}

	r = results[1]
	if r.File != "examples/04_ownership/drop.pr" || r.Test != "" || r.Outcome != "pass" {
		t.Errorf("result[1] = %+v", r)
	}

	r = results[2]
	if r.File != "tests/e2e/strings.pr" || r.Test != "" || r.Outcome != "FAIL" {
		t.Errorf("result[2] = %+v", r)
	}
	if r.Context != "test_split\npanic: assertion failed" {
		t.Errorf("result[2].Context = %q", r.Context)
	}
}

func TestParseTestOutputNonIndentedLineResetsState(t *testing.T) {
	// Trailing non-indented noise after a FAIL block should not produce any
	// extra entries. (Indented detail is consumed inline, so subsequent
	// orphan indented lines have no anchor.)
	output := `FAIL (0.005s) e2e/strings.pr (1/3 failed)
  test_split
    panic: assertion failed
some non-indented noise
  this_should_not_be_captured`

	results := parseTestOutput(output)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(results), results)
	}
	r := results[0]
	if r.File != "e2e/strings.pr" || r.Test != "" || r.Outcome != "FAIL" {
		t.Errorf("result[0] = %+v", r)
	}
	if !strings.Contains(r.Context, "test_split") || !strings.Contains(r.Context, "panic: assertion failed") {
		t.Errorf("result[0].Context = %q", r.Context)
	}
	if strings.Contains(r.Context, "this_should_not_be_captured") {
		t.Errorf("orphan indented line leaked into Context: %q", r.Context)
	}
}

func TestParseTestEntries_TargetPropagation(t *testing.T) {
	output := `pass (0.004s) e2e/basics.pr (3 tests)
FAIL (0.005s) e2e/strings.pr (1/3 failed)
  test_split
    panic: assertion failed

568 passed, 1 failed (117 files, 30.810s)`

	entries := ParseTestEntries("linux-amd64", output)

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(entries), entries)
	}
	for _, e := range entries {
		if e.Target != "linux-amd64" {
			t.Errorf("Target = %q, want linux-amd64", e.Target)
		}
	}
	if entries[0].File != "e2e/basics.pr" || entries[0].Test != "" || entries[0].Outcome != "pass" {
		t.Errorf("entries[0] = %+v", entries[0])
	}
	if entries[1].File != "e2e/strings.pr" || entries[1].Test != "" || entries[1].Outcome != "FAIL" {
		t.Errorf("entries[1] = %+v", entries[1])
	}
}

func TestParseTestEntries_WasmTarget(t *testing.T) {
	output := `pass (1.200s) tests/e2e/hello.pr [wasm32-wasi]

1 passed, 0 failed (1 files, 1.200s)`

	entries := ParseTestEntries("wasm32-wasi", output)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Target != "wasm32-wasi" {
		t.Errorf("Target = %q, want wasm32-wasi", entries[0].Target)
	}
	if entries[0].File != "tests/e2e/hello.pr" {
		t.Errorf("File = %q, want tests/e2e/hello.pr", entries[0].File)
	}
}

func TestParseTestEntries_Empty(t *testing.T) {
	entries := ParseTestEntries("linux-amd64", "")
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}
}

func TestParseTestEntries_ContextPreserved(t *testing.T) {
	output := `FAIL (0.003s) test_broken
  panic: assertion failed: expected 3, got 4

0 passed, 1 failed (0.003s)`

	entries := ParseTestEntries("linux-amd64", output)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Context != "panic: assertion failed: expected 3, got 4" {
		t.Errorf("Context = %q", entries[0].Context)
	}
	if entries[0].Elapsed != 0.003 {
		t.Errorf("Elapsed = %v, want 0.003", entries[0].Elapsed)
	}
}

func TestParseTestEntries_LeakOutcome(t *testing.T) {
	output := `LEAK (0.003s) tests/std/vector_test.pr (1/4 leaked)
  test_vector_push
    leak: 2 allocations not freed

1 passed, 0 failed, 1 leaked (2 files, 0.004s)`

	entries := ParseTestEntries("linux-amd64", output)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Target != "linux-amd64" {
		t.Errorf("Target = %q, want linux-amd64", e.Target)
	}
	if e.Outcome != "LEAK" {
		t.Errorf("Outcome = %q, want LEAK", e.Outcome)
	}
	if e.File != "tests/std/vector_test.pr" {
		t.Errorf("File = %q, want tests/std/vector_test.pr", e.File)
	}
	if e.Test != "" {
		t.Errorf("Test should be empty for multi-file leak, got %q", e.Test)
	}
	if e.Context != "test_vector_push\nleak: 2 allocations not freed" {
		t.Errorf("Context = %q", e.Context)
	}
}

func TestParseTestEntries_TimeoutOutcome(t *testing.T) {
	output := `TIMEOUT (0.100s) test_stuck
  timeout: exceeded 60s limit

0 passed, 0 failed, 0 leaked, 1 timed out (0.100s)`

	entries := ParseTestEntries("linux-amd64", output)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Outcome != "TIMEOUT" {
		t.Errorf("Outcome = %q, want TIMEOUT", e.Outcome)
	}
	if e.Test != "test_stuck" {
		t.Errorf("Test = %q, want test_stuck", e.Test)
	}
	if e.Context != "timeout: exceeded 60s limit" {
		t.Errorf("Context = %q", e.Context)
	}
}

func TestParseTestEntries_CompilationError(t *testing.T) {
	output := `FAIL (0.000s) broken.pr (compilation error)
  broken.pr:5:3: type Foo has no field 'bar'

0 passed, 1 failed (1 files, 0.000s)`

	entries := ParseTestEntries("linux-amd64", output)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Outcome != "FAIL" {
		t.Errorf("Outcome = %q, want FAIL", e.Outcome)
	}
	if e.File != "broken.pr" {
		t.Errorf("File = %q, want broken.pr", e.File)
	}
	if e.Test != "" {
		t.Errorf("Test should be empty for compilation error, got %q", e.Test)
	}
	if e.Context != "broken.pr:5:3: type Foo has no field 'bar'" {
		t.Errorf("Context = %q", e.Context)
	}
}

// T0742 regressions —

// TestParseTestEntries_E2ETimeoutHasStableIdentity verifies that when the
// multi-file parent reports an E2E snapshot timeout, the gate entry has
// outcome=TIMEOUT with stable file-level identity (no failure description
// stuffed into Test).
func TestParseTestEntries_E2ETimeoutHasStableIdentity(t *testing.T) {
	output := `pass (3.844s) examples/04_ownership/drop.pr [wasm32-wasi]
FAIL (60.005s) examples/04_ownership/move_and_borrow.pr (1 timed out) [wasm32-wasi]
  move_and_borrow [wasm32-wasi]
    timeout: exceeded 60s limit`

	entries := ParseTestEntries("wasm32-wasi", output)

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(entries), entries)
	}
	for _, e := range entries {
		if e.Target != "wasm32-wasi" {
			t.Errorf("Target = %q, want wasm32-wasi", e.Target)
		}
		if e.Test != "" {
			t.Errorf("Test should always be empty in multi-file mode, got %q", e.Test)
		}
	}
	if entries[0].File != "examples/04_ownership/drop.pr" || entries[0].Outcome != "pass" {
		t.Errorf("entries[0] = %+v", entries[0])
	}
	if entries[1].File != "examples/04_ownership/move_and_borrow.pr" {
		t.Errorf("entries[1].File = %q", entries[1].File)
	}
	if entries[1].Outcome != "TIMEOUT" {
		t.Errorf("entries[1].Outcome = %q, want TIMEOUT", entries[1].Outcome)
	}
	if !strings.Contains(entries[1].Context, "timeout: exceeded 60s limit") {
		t.Errorf("entries[1].Context = %q", entries[1].Context)
	}
}

// TestParseTestEntries_PassAndFailShareIdentity is the central T0742
// invariant: pass and fail emit the same (target, file, test) tuple.
func TestParseTestEntries_PassAndFailShareIdentity(t *testing.T) {
	pass := `pass (1.200s) examples/04_ownership/move_and_borrow.pr [wasm32-wasi]`
	fail := `FAIL (60.005s) examples/04_ownership/move_and_borrow.pr (1 timed out) [wasm32-wasi]
  move_and_borrow [wasm32-wasi]
    timeout: exceeded 60s limit`

	a := ParseTestEntries("wasm32-wasi", pass)
	b := ParseTestEntries("wasm32-wasi", fail)

	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("expected 1 entry each, got pass=%d fail=%d", len(a), len(b))
	}
	if a[0].Target != b[0].Target {
		t.Errorf("Target mismatch: pass=%q fail=%q", a[0].Target, b[0].Target)
	}
	if a[0].File != b[0].File {
		t.Errorf("File mismatch: pass=%q fail=%q", a[0].File, b[0].File)
	}
	if a[0].Test != b[0].Test {
		t.Errorf("Test mismatch: pass=%q fail=%q", a[0].Test, b[0].Test)
	}
	if a[0].Test != "" {
		t.Errorf("Test should be empty, got pass=%q", a[0].Test)
	}
	if a[0].Outcome != "pass" || b[0].Outcome != "TIMEOUT" {
		t.Errorf("outcomes: pass=%q fail=%q", a[0].Outcome, b[0].Outcome)
	}
}

// TestParseTestEntries_LegacyTimeoutLineNoPhantomTestName guards against the
// original phantom shape: even if a "FAIL (timeout) ..." line appears in the
// indented detail (e.g. an older subprocess version), it must not be lifted
// into the Test field.
func TestParseTestEntries_LegacyTimeoutLineNoPhantomTestName(t *testing.T) {
	output := `FAIL (0.005s) examples/04_ownership/move_and_borrow.pr (1/1 failed) [wasm32-wasi]
  FAIL (timeout) move_and_borrow [wasm32-wasi]`

	entries := ParseTestEntries("wasm32-wasi", output)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.File != "examples/04_ownership/move_and_borrow.pr" {
		t.Errorf("File = %q", e.File)
	}
	if e.Test != "" {
		t.Errorf("Test must be empty, got %q (phantom ledger regression)", e.Test)
	}
	if !strings.Contains(e.Context, "FAIL (timeout) move_and_borrow [wasm32-wasi]") {
		t.Errorf("Context = %q", e.Context)
	}
}

func TestParseTestEntries_MemlimitOutcome(t *testing.T) {
	output := `FAIL (0.500s) tests/std/big_test.pr (memory limit exceeded)
  MEMLIMIT (-) <aborted>
    memory limit: exceeded (test process aborted; subsequent tests not run)`

	entries := ParseTestEntries("linux-amd64", output)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Outcome != "MEMLIMIT" {
		t.Errorf("Outcome = %q, want MEMLIMIT", e.Outcome)
	}
	if e.Test != "" {
		t.Errorf("Test must be empty, got %q", e.Test)
	}
	if e.File != "tests/std/big_test.pr" {
		t.Errorf("File = %q", e.File)
	}
}

func TestParseTestEntries_CompilationTimeoutOutcome(t *testing.T) {
	output := `FAIL (10.000s) tests/big.pr (compilation timeout) [wasm32-wasi]`

	entries := ParseTestEntries("wasm32-wasi", output)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Outcome != "TIMEOUT" {
		t.Errorf("Outcome = %q, want TIMEOUT", e.Outcome)
	}
	if e.Test != "" {
		t.Errorf("Test must be empty, got %q", e.Test)
	}
	if e.File != "tests/big.pr" {
		t.Errorf("File = %q", e.File)
	}
}

// T0749: under wasm32-wasi, passing batch tests must carry both file and test
// name (via the runner's -report-json sidecar), passing snapshot tests must stay
// file-only, and failing-test serialization must be unchanged.
func TestMergePassingTestNames_WasmSchema(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "report.json")
	report := `{
  "passing": [
    {"file": "std/bool_test.pr", "test": "test_and", "elapsed": 0.009},
    {"file": "std/bool_test.pr", "test": "test_or", "elapsed": 0.001}
  ]
}`
	if err := os.WriteFile(reportPath, []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}

	entries := []GateTestEntry{
		// Passing batch file: parser records it file-only (no test name).
		{Target: "wasm32-wasi", File: "std/bool_test.pr", Outcome: "pass", Elapsed: 0.012},
		// Passing snapshot file: also file-only, but absent from the report.
		{Target: "wasm32-wasi", File: "e2e/snapshot.pr", Outcome: "pass", Elapsed: 0.230},
		// A failing test already carries its name and must be left untouched.
		{Target: "wasm32-wasi", File: "e2e/strings.pr", Test: "test_split", Outcome: "FAIL", Context: "panic: assertion failed"},
	}

	got := MergePassingTestNames("wasm32-wasi", entries, reportPath)

	// 2 expanded batch entries + 1 snapshot (unchanged) + 1 FAIL (unchanged).
	if len(got) != 4 {
		t.Fatalf("expected 4 entries, got %d: %+v", len(got), got)
	}

	for i, want := range []struct {
		test    string
		elapsed float64
	}{{"test_and", 0.009}, {"test_or", 0.001}} {
		e := got[i]
		if e.File != "std/bool_test.pr" || e.Test != want.test || e.Outcome != "pass" ||
			e.Target != "wasm32-wasi" || e.Elapsed != want.elapsed {
			t.Errorf("got[%d] = %+v, want file=std/bool_test.pr test=%s pass wasm32-wasi elapsed=%v",
				i, e, want.test, want.elapsed)
		}
	}

	if snap := got[2]; snap.File != "e2e/snapshot.pr" || snap.Test != "" || snap.Outcome != "pass" {
		t.Errorf("snapshot entry must stay file-only, got %+v", snap)
	}

	if fail := got[3]; fail != entries[2] {
		t.Errorf("failing entry changed: got %+v want %+v", fail, entries[2])
	}
}

func TestMergePassingTestNames_MissingReport(t *testing.T) {
	entries := []GateTestEntry{
		{Target: "wasm32-wasi", File: "std/bool_test.pr", Outcome: "pass", Elapsed: 0.012},
	}
	got := MergePassingTestNames("wasm32-wasi", entries, filepath.Join(t.TempDir(), "missing.json"))
	if len(got) != 1 || got[0] != entries[0] {
		t.Fatalf("missing report should return entries unchanged, got %+v", got)
	}
}

func TestMergePassingTestNames_EmptyPath(t *testing.T) {
	entries := []GateTestEntry{{Target: "wasm32-wasi", File: "x.pr", Outcome: "pass"}}
	got := MergePassingTestNames("wasm32-wasi", entries, "")
	if len(got) != 1 || got[0] != entries[0] {
		t.Fatalf("empty report path should return entries unchanged, got %+v", got)
	}
}

func TestMergePassingTestNames_MalformedReport(t *testing.T) {
	// A corrupt sidecar (e.g. a runner killed mid-write) must degrade safely to
	// "no expansion", never fail the gate or panic.
	reportPath := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(reportPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := []GateTestEntry{{Target: "wasm32-wasi", File: "std/bool_test.pr", Outcome: "pass", Elapsed: 0.012}}
	got := MergePassingTestNames("wasm32-wasi", entries, reportPath)
	if len(got) != 1 || got[0] != entries[0] {
		t.Fatalf("malformed report should return entries unchanged, got %+v", got)
	}
}

func TestMergePassingTestNames_EmptyPassing(t *testing.T) {
	// A well-formed report with no passing records (e.g. an all-failing run)
	// leaves the file-only pass entry untouched rather than dropping it.
	reportPath := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(reportPath, []byte(`{"passing": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := []GateTestEntry{{Target: "wasm32-wasi", File: "std/bool_test.pr", Outcome: "pass", Elapsed: 0.012}}
	got := MergePassingTestNames("wasm32-wasi", entries, reportPath)
	if len(got) != 1 || got[0] != entries[0] {
		t.Fatalf("empty passing list should return entries unchanged, got %+v", got)
	}
}

// T0752: a file present in the report but whose gate entry is FAIL (its own
// failing sibling shadows the file-level entry) must not be expanded into pass
// entries — only file-only "pass" entries are rewritten.
func TestMergePassingTestNames_FailingFileNotExpanded(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "report.json")
	report := `{"passing": [{"file": "std/mixed_test.pr", "test": "test_ok", "elapsed": 0.003}]}`
	if err := os.WriteFile(reportPath, []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := []GateTestEntry{
		{Target: "wasm32-wasi", File: "std/mixed_test.pr", Test: "test_broken", Outcome: "FAIL", Context: "panic"},
	}
	got := MergePassingTestNames("wasm32-wasi", entries, reportPath)
	if len(got) != 1 || got[0] != entries[0] {
		t.Fatalf("FAIL entry for a reported file must be left untouched, got %+v", got)
	}
}
