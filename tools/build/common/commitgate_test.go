package common

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setupGateTest creates a temp directory with baselines.json and a verify
// summary sidecar, returning the root path.
func setupGateTest(t *testing.T, baselines Baselines, summary *VerifySummary) string {
	t.Helper()
	root := t.TempDir()

	// Write baselines.json.
	gatesDir := filepath.Join(root, "tools", "gates")
	os.MkdirAll(gatesDir, 0o755)
	data, _ := json.MarshalIndent(baselines, "", "  ")
	data = append(data, '\n')
	os.WriteFile(filepath.Join(gatesDir, "baselines.json"), data, 0o644)

	// Write verify summary sidecar.
	promiseHome := filepath.Join(root, ".promise-home")
	os.MkdirAll(promiseHome, 0o755)
	summary.Timestamp = time.Now().UTC().Format(time.RFC3339)
	sdata, _ := json.MarshalIndent(summary, "", "  ")
	sdata = append(sdata, '\n')
	os.WriteFile(filepath.Join(promiseHome, "last-verify.json"), sdata, 0o644)

	return root
}

func TestCheckCommitGate_AllMatch(t *testing.T) {
	baselines := Baselines{
		"linux-amd64": {
			"host_test_count":    {Value: 100, Direction: "up", Updated: "2026-04-06"},
			"host_leak_count":    {Value: 0, Direction: "down", Updated: "2026-04-06"},
			"host_test_failures": {Value: 0, Direction: "exact", Updated: "2026-04-06"},
		},
	}
	summary := &VerifySummary{
		Targets: map[string]TargetSummary{
			"linux-amd64": {Passed: 100, Failed: 0, Leaked: 0},
		},
	}
	root := setupGateTest(t, baselines, summary)

	err := CheckCommitGate(root)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func TestCheckCommitGate_Improvement(t *testing.T) {
	baselines := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: 100, Direction: "up", Updated: "2026-04-06"},
		},
	}
	summary := &VerifySummary{
		Targets: map[string]TargetSummary{
			"linux-amd64": {Passed: 110, Failed: 0, Leaked: 0},
		},
	}
	root := setupGateTest(t, baselines, summary)

	err := CheckCommitGate(root)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}

	// Verify baselines were updated.
	updated, err := LoadBaselines(root)
	if err != nil {
		t.Fatalf("load updated baselines: %v", err)
	}
	bl := updated["linux-amd64"]["host_test_count"]
	if bl.Value != 110 {
		t.Errorf("baseline value = %d, want 110", bl.Value)
	}
}

func TestCheckCommitGate_RegressionBlocked(t *testing.T) {
	baselines := Baselines{
		"linux-amd64": {
			"host_test_count": {Value: 100, Direction: "up", Updated: "2026-04-06"},
		},
	}
	summary := &VerifySummary{
		Targets: map[string]TargetSummary{
			"linux-amd64": {Passed: 95, Failed: 0, Leaked: 0},
		},
	}
	root := setupGateTest(t, baselines, summary)

	err := CheckCommitGate(root)
	if err == nil {
		t.Fatal("expected error for regression, got nil")
	}
}

func TestCheckCommitGate_ExactMetricRegression(t *testing.T) {
	baselines := Baselines{
		"linux-amd64": {
			"host_test_failures": {Value: 0, Direction: "exact", Updated: "2026-04-06"},
		},
	}
	summary := &VerifySummary{
		Targets: map[string]TargetSummary{
			"linux-amd64": {Passed: 100, Failed: 1, Leaked: 0},
		},
	}
	root := setupGateTest(t, baselines, summary)

	err := CheckCommitGate(root)
	if err == nil {
		t.Fatal("expected error for exact metric regression, got nil")
	}
}

func TestCheckCommitGate_LeakIncrease(t *testing.T) {
	baselines := Baselines{
		"linux-amd64": {
			"host_leak_count": {Value: 0, Direction: "down", Updated: "2026-04-06"},
		},
	}
	summary := &VerifySummary{
		Targets: map[string]TargetSummary{
			"linux-amd64": {Passed: 100, Failed: 0, Leaked: 2},
		},
	}
	root := setupGateTest(t, baselines, summary)

	err := CheckCommitGate(root)
	if err == nil {
		t.Fatal("expected error for leak increase, got nil")
	}
}

func TestCheckCommitGate_UnknownPlatformSkips(t *testing.T) {
	baselines := Baselines{
		"darwin-arm64": {
			"host_test_count": {Value: 100, Direction: "up", Updated: "2026-04-06"},
		},
	}
	summary := &VerifySummary{
		Targets: map[string]TargetSummary{
			"linux-amd64": {Passed: 50, Failed: 0, Leaked: 0},
		},
	}
	root := setupGateTest(t, baselines, summary)

	err := CheckCommitGate(root)
	if err != nil {
		t.Fatalf("expected nil for unknown platform, got: %v", err)
	}
}

func TestCheckCommitGate_StaleSummary(t *testing.T) {
	root := t.TempDir()

	// Write baselines.
	gatesDir := filepath.Join(root, "tools", "gates")
	os.MkdirAll(gatesDir, 0o755)
	baselines := Baselines{"linux-amd64": {"host_test_count": {Value: 100, Direction: "up"}}}
	data, _ := json.MarshalIndent(baselines, "", "  ")
	os.WriteFile(filepath.Join(gatesDir, "baselines.json"), data, 0o644)

	// Write a summary with an old mtime.
	promiseHome := filepath.Join(root, ".promise-home")
	os.MkdirAll(promiseHome, 0o755)
	summaryPath := filepath.Join(promiseHome, "last-verify.json")
	summary := &VerifySummary{Timestamp: "2020-01-01T00:00:00Z", Targets: map[string]TargetSummary{}}
	sdata, _ := json.MarshalIndent(summary, "", "  ")
	os.WriteFile(summaryPath, sdata, 0o644)

	// Set mtime to 20 minutes ago to trigger staleness.
	old := time.Now().Add(-20 * time.Minute)
	os.Chtimes(summaryPath, old, old)

	err := CheckCommitGate(root)
	if err == nil {
		t.Fatal("expected stale summary error, got nil")
	}
}

func TestCheckRatchet(t *testing.T) {
	tests := []struct {
		dir      string
		baseline int
		actual   int
		want     bool
	}{
		{"up", 100, 100, true},
		{"up", 100, 110, true},
		{"up", 100, 95, false},
		{"down", 5, 5, true},
		{"down", 5, 3, true},
		{"down", 5, 8, false},
		{"exact", 0, 0, true},
		{"exact", 0, 1, false},
		{"exact", 5, 5, true},
		{"unknown", 0, 99, true},
	}
	for _, tt := range tests {
		got := checkRatchet(tt.dir, tt.baseline, tt.actual)
		if got != tt.want {
			t.Errorf("checkRatchet(%q, %d, %d) = %v, want %v",
				tt.dir, tt.baseline, tt.actual, got, tt.want)
		}
	}
}
