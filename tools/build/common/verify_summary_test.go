package common

import (
	"os"
	"path/filepath"
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
