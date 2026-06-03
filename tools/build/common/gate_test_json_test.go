package common

import (
	"path/filepath"
	"strings"
	"testing"
)

// jsonlLine builds one --json record line with an absolute file path under root.
func jsonlLine(root, relFile, test, status string) string {
	abs := filepath.Join(root, filepath.FromSlash(relFile))
	return `{"file":` + quote(abs) + `,"test":` + quote(test) + `,"status":` + quote(status) + `,"elapsed":0.01}`
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
}

func TestBuildGateOutputGroupsAndRelativizes(t *testing.T) {
	root := t.TempDir()
	jsonl := strings.Join([]string{
		jsonlLine(root, "tests/std/bool_test.pr", "test_and", "pass"),
		jsonlLine(root, "tests/std/bool_test.pr", "test_or", "fail"),
		jsonlLine(root, "tests/e2e/hello.pr", "main", "pass"),
		"", // blank line tolerated
		`{not valid json`, // malformed line skipped
	}, "\n")

	out := BuildGateOutput(root, "linux-amd64", "host", "promise-tests", jsonl)

	if out.Target != "linux-amd64" {
		t.Errorf("target = %q, want linux-amd64", out.Target)
	}
	if out.Complete != "promise-tests" {
		t.Errorf("complete = %q", out.Complete)
	}
	if len(out.Files) != 2 {
		t.Fatalf("want 2 file groups, got %d: %+v", len(out.Files), out.Files)
	}
	// Order preserved: bool_test first, hello second. Paths repo-relative, slashes.
	if out.Files[0].File != "tests/std/bool_test.pr" {
		t.Errorf("file[0] = %q, want tests/std/bool_test.pr", out.Files[0].File)
	}
	if len(out.Files[0].Tests) != 2 {
		t.Fatalf("bool_test should have 2 tests, got %d", len(out.Files[0].Tests))
	}
	if out.Files[1].File != "tests/e2e/hello.pr" || out.Files[1].Tests[0].Test != "main" {
		t.Errorf("file[1] = %+v, want tests/e2e/hello.pr main", out.Files[1])
	}
}

func TestBuildGateOutputMetrics(t *testing.T) {
	root := t.TempDir()
	jsonl := strings.Join([]string{
		jsonlLine(root, "a_test.pr", "p1", "pass"),
		jsonlLine(root, "a_test.pr", "p2", "pass"),
		jsonlLine(root, "a_test.pr", "f1", "fail"),
		jsonlLine(root, "a_test.pr", "l1", "leak"),
		jsonlLine(root, "a_test.pr", "t1", "timeout"),
		jsonlLine(root, "a_test.pr", "m1", "memory"),
		jsonlLine(root, "a_test.pr", "x1", "excluded"),
		jsonlLine(root, "a_test.pr", "n1", "not-run"),
	}, "\n")

	out := BuildGateOutput(root, "linux-amd64", "host", "promise-tests", jsonl)

	want := map[string]float64{
		"host_test_count":     2,
		"host_test_failures":  1,
		"host_leak_count":     1,
		"host_timeout_count":  1,
		"host_memory_count":   1,
		"host_excluded_count": 1,
		"host_not_run_count":  1,
	}
	for k, v := range want {
		if out.Metrics[k] != v {
			t.Errorf("metric %s = %v, want %v", k, out.Metrics[k], v)
		}
	}
	// A gate must report a stable metric set: every metric present even at 0.
	if len(out.Metrics) != len(want) {
		t.Errorf("metric set size = %d, want %d: %+v", len(out.Metrics), len(want), out.Metrics)
	}
}

func TestBuildGateOutputZeroMetricsPresent(t *testing.T) {
	root := t.TempDir()
	jsonl := jsonlLine(root, "a_test.pr", "p1", "pass")
	out := BuildGateOutput(root, "wasm32-wasi", "wasm", "wasm-test", jsonl)

	// All wasm_ metrics present; only test_count is non-zero.
	if out.Metrics["wasm_test_count"] != 1 {
		t.Errorf("wasm_test_count = %v, want 1", out.Metrics["wasm_test_count"])
	}
	for _, k := range []string{"wasm_test_failures", "wasm_leak_count", "wasm_timeout_count",
		"wasm_memory_count", "wasm_excluded_count", "wasm_not_run_count"} {
		if _, ok := out.Metrics[k]; !ok {
			t.Errorf("missing zero metric %s", k)
		}
		if out.Metrics[k] != 0 {
			t.Errorf("%s = %v, want 0", k, out.Metrics[k])
		}
	}
}

func TestBuildGateOutputPathOutsideRootKept(t *testing.T) {
	root := t.TempDir()
	// A record whose file is not under root is kept verbatim (forward-slashed),
	// never silently dropped.
	jsonl := `{"file":"/somewhere/else/x_test.pr","test":"main","status":"pass","elapsed":0.01}`
	out := BuildGateOutput(root, "linux-amd64", "host", "promise-tests", jsonl)
	if len(out.Files) != 1 || out.Files[0].File != "/somewhere/else/x_test.pr" {
		t.Fatalf("outside-root path not preserved: %+v", out.Files)
	}
}
