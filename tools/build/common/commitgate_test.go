package common

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fp is a helper to create *float64 values for test baselines.
func fp(v float64) *float64 { return &v }

// testPlatform returns the current runtime platform string used by CheckCommitGate.
func testPlatform() string { return runtime.GOOS + "-" + runtime.GOARCH }

// setupGateTest creates a temp directory with baselines.json and a gate values
// sidecar, returning the root path. Uses the runtime platform for baselines.
func setupGateTest(t *testing.T, baselines Baselines, gv *GateValues) string {
	t.Helper()
	root := t.TempDir()

	// Write baselines.json.
	gatesDir := filepath.Join(root, "tools", "gates")
	os.MkdirAll(gatesDir, 0o755)
	data, _ := json.MarshalIndent(baselines, "", "  ")
	data = append(data, '\n')
	os.WriteFile(filepath.Join(gatesDir, "baselines.json"), data, 0o644)

	// Write gate values sidecar.
	promiseHome := filepath.Join(root, ".promise-home")
	os.MkdirAll(promiseHome, 0o755)
	gv.Timestamp = time.Now().UTC().Format(time.RFC3339)
	if gv.Platform == "" {
		gv.Platform = testPlatform()
	}
	sdata, _ := json.MarshalIndent(gv, "", "  ")
	sdata = append(sdata, '\n')
	os.WriteFile(filepath.Join(promiseHome, gateValuesFile), sdata, 0o644)

	return root
}

func TestCheckCommitGate_AllMatch(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{
		p: {
			"host_test_count":    {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
			"host_leak_count":    {Value: fp(0), Direction: "down", Updated: "2026-04-06"},
			"host_test_failures": {Value: fp(0), Direction: "exact", Updated: "2026-04-06"},
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"host_test_count":    100,
			"host_leak_count":    0,
			"host_test_failures": 0,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func TestCheckCommitGate_Improvement(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{
		p: {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"host_test_count": 110,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}

	// Verify baselines were updated.
	updated, err := LoadBaselines(root)
	if err != nil {
		t.Fatalf("load updated baselines: %v", err)
	}
	bl := updated[p]["host_test_count"]
	if bl.Value == nil || *bl.Value != 110 {
		t.Errorf("baseline value = %v, want 110", bl.Value)
	}
}

func TestCheckCommitGate_RegressionBlocked(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{
		p: {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"host_test_count": 95,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err == nil {
		t.Fatal("expected error for regression, got nil")
	}
}

func TestCheckCommitGate_ExactMetricRegression(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{
		p: {
			"host_test_failures": {Value: fp(0), Direction: "exact", Updated: "2026-04-06"},
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"host_test_failures": 1,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err == nil {
		t.Fatal("expected error for exact metric regression, got nil")
	}
}

func TestCheckCommitGate_LeakIncrease(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{
		p: {
			"host_leak_count": {Value: fp(0), Direction: "down", Updated: "2026-04-06"},
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"host_leak_count": 2,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err == nil {
		t.Fatal("expected error for leak increase, got nil")
	}
}

func TestCheckCommitGate_UnknownPlatformCreatesEntries(t *testing.T) {
	// Baselines exist only for a different platform — current platform has no
	// entry. CheckCommitGate should auto-register gate values as informational.
	baselines := Baselines{
		"other-platform": {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"host_test_count": 50,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err != nil {
		t.Fatalf("expected nil for unknown platform auto-register, got: %v", err)
	}

	// Verify auto-registration under current platform.
	updated, err := LoadBaselines(root)
	if err != nil {
		t.Fatalf("load updated baselines: %v", err)
	}
	p := testPlatform()
	bl, ok := updated[p]["host_test_count"]
	if !ok {
		t.Fatal("expected host_test_count to be auto-registered")
	}
	if bl.Type != "informational" {
		t.Errorf("type = %q, want informational", bl.Type)
	}
}

func TestCheckCommitGate_StaleSummary(t *testing.T) {
	p := testPlatform()
	root := t.TempDir()

	// Write baselines.
	gatesDir := filepath.Join(root, "tools", "gates")
	os.MkdirAll(gatesDir, 0o755)
	baselines := Baselines{p: {"host_test_count": {Value: fp(100), Direction: "up"}}}
	data, _ := json.MarshalIndent(baselines, "", "  ")
	os.WriteFile(filepath.Join(gatesDir, "baselines.json"), data, 0o644)

	// Write gate values with an old mtime.
	promiseHome := filepath.Join(root, ".promise-home")
	os.MkdirAll(promiseHome, 0o755)
	gvPath := filepath.Join(promiseHome, gateValuesFile)
	gv := &GateValues{Timestamp: "2020-01-01T00:00:00Z", Values: map[string]float64{}}
	sdata, _ := json.MarshalIndent(gv, "", "  ")
	os.WriteFile(gvPath, sdata, 0o644)

	// Set mtime to 20 minutes ago to trigger staleness.
	old := time.Now().Add(-20 * time.Minute)
	os.Chtimes(gvPath, old, old)

	err := CheckCommitGate(root)
	if err == nil {
		t.Fatal("expected stale gate values error, got nil")
	}
}

func TestCheckRatchet(t *testing.T) {
	tests := []struct {
		dir      string
		baseline float64
		actual   float64
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
			t.Errorf("checkRatchet(%q, %v, %v) = %v, want %v",
				tt.dir, tt.baseline, tt.actual, got, tt.want)
		}
	}
}

func TestCheckCommitGate_PendingPopulated(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{
		p: {
			"host_test_count": {Direction: "up"}, // Pending: no Value
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"host_test_count": 200,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err != nil {
		t.Fatalf("expected nil error for pending populate, got: %v", err)
	}

	// Verify value was populated.
	updated, err := LoadBaselines(root)
	if err != nil {
		t.Fatalf("load updated baselines: %v", err)
	}
	bl := updated[p]["host_test_count"]
	if bl.Value == nil || *bl.Value != 200 {
		t.Errorf("baseline value = %v, want 200", bl.Value)
	}
}

func TestCheckCommitGate_InformationalIgnored(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{
		p: {
			"some_metric": {Type: "informational"},
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"some_metric": 999,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err != nil {
		t.Fatalf("expected nil for informational metric, got: %v", err)
	}
}

func TestCheckCommitGate_UnknownValueAutoRegistered(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{
		p: {
			"host_test_count": {Value: fp(100), Direction: "up", Updated: "2026-04-06"},
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"host_test_count": 100,
			"new_metric":      42,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err != nil {
		t.Fatalf("expected nil for auto-registered metric, got: %v", err)
	}

	// Verify new metric was auto-registered as informational.
	updated, err := LoadBaselines(root)
	if err != nil {
		t.Fatalf("load updated baselines: %v", err)
	}
	bl, ok := updated[p]["new_metric"]
	if !ok {
		t.Fatal("expected new_metric to be registered")
	}
	if bl.Type != "informational" {
		t.Errorf("type = %q, want informational", bl.Type)
	}
}

func TestCheckCommitGate_FloatValue(t *testing.T) {
	p := testPlatform()
	baselines := Baselines{
		p: {
			"coverage": {Value: fp(85.5), Direction: "up", Updated: "2026-04-06"},
		},
	}
	gv := &GateValues{
		Values: map[string]float64{
			"coverage": 86.2,
		},
	}
	root := setupGateTest(t, baselines, gv)

	err := CheckCommitGate(root)
	if err != nil {
		t.Fatalf("expected nil for float improvement, got: %v", err)
	}

	updated, err := LoadBaselines(root)
	if err != nil {
		t.Fatalf("load updated baselines: %v", err)
	}
	bl := updated[p]["coverage"]
	if bl.Value == nil || *bl.Value != 86.2 {
		t.Errorf("baseline value = %v, want 86.2", bl.Value)
	}
}
