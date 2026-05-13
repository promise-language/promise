package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseTestSummaryLine_Basic(t *testing.T) {
	output := "3634 passed, 0 failed (354 files, 1.075s)"
	s := ParseTestSummaryLine(output)
	if s == nil {
		t.Fatal("expected non-nil summary")
	}
	if s.Passed != 3634 {
		t.Errorf("Passed = %d, want 3634", s.Passed)
	}
	if s.Failed != 0 {
		t.Errorf("Failed = %d, want 0", s.Failed)
	}
	if s.Leaked != 0 {
		t.Errorf("Leaked = %d, want 0", s.Leaked)
	}
	if s.TimedOut != 0 {
		t.Errorf("TimedOut = %d, want 0", s.TimedOut)
	}
}

func TestParseTestSummaryLine_WithLeaks(t *testing.T) {
	output := "3634 passed, 0 failed, 2 leaked (354 files, 1.075s)"
	s := ParseTestSummaryLine(output)
	if s == nil {
		t.Fatal("expected non-nil summary")
	}
	if s.Passed != 3634 {
		t.Errorf("Passed = %d, want 3634", s.Passed)
	}
	if s.Leaked != 2 {
		t.Errorf("Leaked = %d, want 2", s.Leaked)
	}
}

func TestParseTestSummaryLine_WithSkippedAndLeaks(t *testing.T) {
	output := "3381 passed, 0 failed, 253 skipped, 2 leaked (343 files, 36.205s) [wasm32-wasi]"
	s := ParseTestSummaryLine(output)
	if s == nil {
		t.Fatal("expected non-nil summary")
	}
	if s.Passed != 3381 {
		t.Errorf("Passed = %d, want 3381", s.Passed)
	}
	if s.Failed != 0 {
		t.Errorf("Failed = %d, want 0", s.Failed)
	}
	if s.Leaked != 2 {
		t.Errorf("Leaked = %d, want 2", s.Leaked)
	}
}

func TestParseTestSummaryLine_WithTimedOut(t *testing.T) {
	output := "100 passed, 1 failed, 1 leaked, 2 timed out (10 files, 5.000s)"
	s := ParseTestSummaryLine(output)
	if s == nil {
		t.Fatal("expected non-nil summary")
	}
	if s.Passed != 100 {
		t.Errorf("Passed = %d, want 100", s.Passed)
	}
	if s.Failed != 1 {
		t.Errorf("Failed = %d, want 1", s.Failed)
	}
	if s.Leaked != 1 {
		t.Errorf("Leaked = %d, want 1", s.Leaked)
	}
	if s.TimedOut != 2 {
		t.Errorf("TimedOut = %d, want 2", s.TimedOut)
	}
}

func TestParseTestSummaryLine_MultilineOutput(t *testing.T) {
	output := `pass (0.001s) test_add
pass (0.002s) test_sub
FAIL (0.003s) test_broken
  panic: assertion failed

2 passed, 1 failed (0.423s)
FAILED:
  test_broken`
	s := ParseTestSummaryLine(output)
	if s == nil {
		t.Fatal("expected non-nil summary")
	}
	if s.Passed != 2 {
		t.Errorf("Passed = %d, want 2", s.Passed)
	}
	if s.Failed != 1 {
		t.Errorf("Failed = %d, want 1", s.Failed)
	}
}

func TestParseTestSummaryLine_NoMatch(t *testing.T) {
	s := ParseTestSummaryLine("no summary here")
	if s != nil {
		t.Errorf("expected nil, got %+v", s)
	}
}

func TestParseTestSummaryLine_Empty(t *testing.T) {
	s := ParseTestSummaryLine("")
	if s != nil {
		t.Errorf("expected nil, got %+v", s)
	}
}

func TestExtractFailedSection_MultiFile(t *testing.T) {
	output := `pass (0.001s) e2e/basics.pr (3 tests)
FAIL (0.005s) e2e/strings.pr (1/3 failed)
  test_split
    panic: assertion failed

568 passed, 2 failed (117 files, 30.810s)
FAILED:
  e2e/strings.pr: test_split
    panic: assertion failed
  broken.pr (compilation error)
    broken.pr:5:3: type Foo has no field 'bar'`

	got := ExtractFailedSection(output)
	if !strings.Contains(got, "e2e/strings.pr: test_split") {
		t.Errorf("expected test_split in section, got:\n%s", got)
	}
	if !strings.Contains(got, "broken.pr (compilation error)") {
		t.Errorf("expected broken.pr in section, got:\n%s", got)
	}
	// Must not include the "FAILED:" header line itself.
	if strings.HasPrefix(got, "FAILED:") {
		t.Errorf("section should not start with FAILED:, got:\n%s", got)
	}
}

func TestExtractFailedSection_SingleFile(t *testing.T) {
	output := `pass (0.001s) test_add
FAIL (0.003s) test_broken
  panic: assertion failed

2 passed, 1 failed (0.423s)
FAILED:
  test_broken`

	got := ExtractFailedSection(output)
	if !strings.Contains(got, "test_broken") {
		t.Errorf("expected test_broken, got:\n%s", got)
	}
}

func TestExtractFailedSection_NoFailures(t *testing.T) {
	output := "3 passed, 0 failed (0.010s)"
	got := ExtractFailedSection(output)
	if got != "" {
		t.Errorf("expected empty string, got: %q", got)
	}
}

func TestExtractFailedSection_Empty(t *testing.T) {
	got := ExtractFailedSection("")
	if got != "" {
		t.Errorf("expected empty string for empty input, got: %q", got)
	}
}

func TestWriteReadGateValues(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".promise-home"), 0o755)

	gv := &GateValues{
		Timestamp: "2026-04-11T12:00:00Z",
		Platform:  "darwin-arm64",
		Values: map[string]float64{
			"host_test_count": 3656,
			"host_leak_count": 0,
		},
	}
	if err := WriteGateValues(root, gv); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := ReadGateValues(root, 5*time.Minute)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Platform != "darwin-arm64" {
		t.Errorf("platform = %q, want darwin-arm64", got.Platform)
	}
	if got.Values["host_test_count"] != 3656 {
		t.Errorf("host_test_count = %v, want 3656", got.Values["host_test_count"])
	}
}

func TestReadGateValues_Missing(t *testing.T) {
	root := t.TempDir()
	_, err := ReadGateValues(root, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for missing gate values, got nil")
	}
}

func TestInvalidateGateValues(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".promise-home"), 0o755)

	// Write gate values, then invalidate — file should be gone.
	gv := &GateValues{
		Timestamp: "2026-04-11T12:00:00Z",
		Platform:  "darwin-arm64",
		Values:    map[string]float64{"host_test_count": 100},
	}
	if err := WriteGateValues(root, gv); err != nil {
		t.Fatalf("write: %v", err)
	}
	InvalidateGateValues(root)
	_, err := ReadGateValues(root, 0)
	if err == nil {
		t.Fatal("expected error after invalidation, got nil")
	}
}

func TestInvalidateGateValues_Missing(t *testing.T) {
	// Invalidating when no file exists should not panic or error.
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".promise-home"), 0o755)
	InvalidateGateValues(root) // should be a no-op
}

func TestInvalidateGateValues_PermissionError(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".promise-home")
	os.MkdirAll(dir, 0o755)

	// Write a gate values file, then make directory read-only so Remove fails
	// with a permission error (not IsNotExist).
	gv := &GateValues{
		Timestamp: "2026-04-11T12:00:00Z",
		Platform:  "darwin-arm64",
		Values:    map[string]float64{"x": 1},
	}
	if err := WriteGateValues(root, gv); err != nil {
		t.Fatalf("write: %v", err)
	}
	os.Chmod(dir, 0o555) // read+exec only — Remove will fail
	defer os.Chmod(dir, 0o755)

	// Should not panic; prints warning to stderr.
	InvalidateGateValues(root)

	// File should still exist since removal was denied.
	path := filepath.Join(dir, gateValuesFile)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file should still exist after failed removal: %v", err)
	}
}

func TestReadGateValues_MalformedJSON(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".promise-home"), 0o755)

	path := filepath.Join(root, ".promise-home", gateValuesFile)
	os.WriteFile(path, []byte("not json"), 0o644)

	_, err := ReadGateValues(root, 0)
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	if !strings.Contains(err.Error(), "parse gate values") {
		t.Errorf("expected 'parse gate values' in error, got: %v", err)
	}
}

func TestReadGateValues_Stale(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".promise-home"), 0o755)

	gv := &GateValues{
		Timestamp: "2020-01-01T00:00:00Z",
		Platform:  "linux-amd64",
		Values:    map[string]float64{},
	}
	if err := WriteGateValues(root, gv); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Set mtime to 20 minutes ago.
	path := filepath.Join(root, ".promise-home", gateValuesFile)
	old := time.Now().Add(-20 * time.Minute)
	os.Chtimes(path, old, old)

	_, err := ReadGateValues(root, 10*time.Minute)
	if err == nil {
		t.Fatal("expected stale error, got nil")
	}
}
