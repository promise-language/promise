package common

import (
	"os"
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

	// Expected: 4 results (2 pass files, 1 failed test, 1 compilation error)
	if len(results) != 4 {
		t.Fatalf("expected 4 results, got %d: %+v", len(results), results)
	}

	// pass file with suffix
	r := results[0]
	if r.File != "e2e/basics.pr" || r.Outcome != "pass" || r.Elapsed != 0.004 {
		t.Errorf("result[0] = %+v", r)
	}

	// pass file without suffix
	r = results[1]
	if r.File != "e2e/hello.pr" || r.Outcome != "pass" || r.Elapsed != 0.001 {
		t.Errorf("result[1] = %+v", r)
	}

	// failed test within file
	r = results[2]
	if r.File != "e2e/strings.pr" || r.Test != "test_split" || r.Outcome != "FAIL" {
		t.Errorf("result[2] = %+v", r)
	}
	if r.Context != "panic: assertion failed" {
		t.Errorf("result[2].Context = %q", r.Context)
	}

	// compilation error
	r = results[3]
	if r.File != "broken.pr" || r.Outcome != "FAIL" || r.Elapsed != 0.0 {
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

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d: %+v", len(results), results)
	}

	r := results[0]
	if r.File != "tests/e2e/errors.pr" || r.Test != "test_divide_by_zero" || r.Outcome != "FAIL" {
		t.Errorf("result[0] = %+v", r)
	}
	if r.Context != "panic: division by zero" {
		t.Errorf("result[0].Context = %q", r.Context)
	}

	r = results[1]
	if r.File != "tests/e2e/errors.pr" || r.Test != "test_overflow" || r.Outcome != "FAIL" {
		t.Errorf("result[1] = %+v", r)
	}
	if r.Context != "panic: integer overflow" {
		t.Errorf("result[1].Context = %q", r.Context)
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
	if r.File != "tests/std/vector_test.pr" || r.Test != "test_vector_push" || r.Outcome != "LEAK" {
		t.Errorf("result[0] = %+v", r)
	}
	if r.Context != "leak: 2 allocations not freed" {
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

	// File with both (N tests) and [target] suffixes stripped.
	r := results[0]
	if r.File != "tests/arrays/fixed_string_forin_test.pr" || r.Outcome != "pass" {
		t.Errorf("result[0] = %+v", r)
	}

	// File with only [target] suffix stripped.
	r = results[1]
	if r.File != "examples/04_ownership/drop.pr" || r.Outcome != "pass" {
		t.Errorf("result[1] = %+v", r)
	}

	// FAIL with both suffixes.
	r = results[2]
	if r.File != "tests/e2e/strings.pr" || r.Test != "test_split" || r.Outcome != "FAIL" {
		t.Errorf("result[2] = %+v", r)
	}
}

func TestParseTestOutputNonIndentedLineResetsState(t *testing.T) {
	// A non-indented, non-result line between file details should reset the parser
	// so the FAILED: summary section doesn't produce duplicate results.
	output := `FAIL (0.005s) e2e/strings.pr (1/3 failed)
  test_split
    panic: assertion failed
some non-indented noise
  this_should_not_be_captured`

	results := parseTestOutput(output)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(results), results)
	}
	if results[0].Test != "test_split" {
		t.Errorf("result[0].Test = %q", results[0].Test)
	}
}

func TestFindTrackerURLEnvVar(t *testing.T) {
	t.Setenv("PROMISE_TRACKER_URL", "http://localhost:9000")
	url := findTrackerURL("/nonexistent")
	if url != "http://localhost:9000" {
		t.Errorf("expected http://localhost:9000, got %q", url)
	}
}

func TestFindTrackerURLEnvVarTrailingSlash(t *testing.T) {
	t.Setenv("PROMISE_TRACKER_URL", "http://localhost:9000/")
	url := findTrackerURL("/nonexistent")
	if url != "http://localhost:9000" {
		t.Errorf("expected http://localhost:9000, got %q", url)
	}
}

func TestFindTrackerURLMcpJson(t *testing.T) {
	t.Setenv("PROMISE_TRACKER_URL", "")
	dir := t.TempDir()
	mcpJSON := `{"mcpServers":{"tracker":{"type":"http","url":"http://192.168.1.7:9121/mcp"}}}`
	if err := writeFile(dir+"/.mcp.json", mcpJSON); err != nil {
		t.Fatal(err)
	}
	url := findTrackerURL(dir)
	if url != "http://192.168.1.7:9121" {
		t.Errorf("expected http://192.168.1.7:9121, got %q", url)
	}
}

func TestFindTrackerURLBadJSON(t *testing.T) {
	t.Setenv("PROMISE_TRACKER_URL", "")
	dir := t.TempDir()
	if err := writeFile(dir+"/.mcp.json", "not json"); err != nil {
		t.Fatal(err)
	}
	url := findTrackerURL(dir)
	if url != "" {
		t.Errorf("expected empty, got %q", url)
	}
}

func TestFindTrackerURLMissingTrackerKey(t *testing.T) {
	t.Setenv("PROMISE_TRACKER_URL", "")
	dir := t.TempDir()
	if err := writeFile(dir+"/.mcp.json", `{"mcpServers":{}}`); err != nil {
		t.Fatal(err)
	}
	url := findTrackerURL(dir)
	if url != "" {
		t.Errorf("expected empty, got %q", url)
	}
}

func TestFindTrackerURLNone(t *testing.T) {
	t.Setenv("PROMISE_TRACKER_URL", "")
	url := findTrackerURL(t.TempDir())
	if url != "" {
		t.Errorf("expected empty, got %q", url)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
