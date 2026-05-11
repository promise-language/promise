package common

import "testing"

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
