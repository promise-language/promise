package common

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"
)

// Baseline represents a single ratcheted metric with its current value and
// the direction it is allowed to move.
type Baseline struct {
	Value     int    `json:"value"`
	Direction string `json:"direction"` // "up", "down", "exact"
	Updated   string `json:"updated"`   // YYYY-MM-DD
}

// Baselines maps platform → metric name → baseline.
type Baselines map[string]map[string]Baseline

const baselinesFile = "tools/gates/baselines.json"

// LoadBaselines reads baselines.json from the repo root.
func LoadBaselines(root string) (Baselines, error) {
	path := filepath.Join(root, baselinesFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read baselines: %w", err)
	}
	var b Baselines
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parse baselines: %w", err)
	}
	return b, nil
}

// SaveBaselines writes baselines.json back with sorted keys for stable diffs.
func SaveBaselines(root string, b Baselines) error {
	path := filepath.Join(root, baselinesFile)
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal baselines: %w", err)
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// metricMapping defines how verify summary fields map to baseline metric names.
type metricMapping struct {
	metric string // baseline metric name
	value  func(host, wasm *TargetSummary) (int, bool)
}

var metricMappings = []metricMapping{
	{"host_test_count", func(h, _ *TargetSummary) (int, bool) {
		if h == nil {
			return 0, false
		}
		return h.Passed, true
	}},
	{"host_leak_count", func(h, _ *TargetSummary) (int, bool) {
		if h == nil {
			return 0, false
		}
		return h.Leaked, true
	}},
	{"host_test_failures", func(h, _ *TargetSummary) (int, bool) {
		if h == nil {
			return 0, false
		}
		return h.Failed, true
	}},
	{"wasm_test_count", func(_, w *TargetSummary) (int, bool) {
		if w == nil {
			return 0, false
		}
		return w.Passed, true
	}},
	{"wasm_test_failures", func(_, w *TargetSummary) (int, bool) {
		if w == nil {
			return 0, false
		}
		return w.Failed, true
	}},
}

// checkRatchet returns true if the new value satisfies the ratchet direction.
func checkRatchet(direction string, baseline, actual int) bool {
	switch direction {
	case "up":
		return actual >= baseline
	case "down":
		return actual <= baseline
	case "exact":
		return actual == baseline
	default:
		return true
	}
}

// ratchetVerb returns a human-readable description of the ratchet direction.
func ratchetVerb(direction string) string {
	switch direction {
	case "up":
		return "must not decrease"
	case "down":
		return "must not increase"
	case "exact":
		return "must equal"
	default:
		return "unknown direction"
	}
}

// CheckCommitGate reads the last verify summary and compares against baselines.
// Returns nil if all metrics pass. Updates baselines.json on improvements.
func CheckCommitGate(root string) error {
	// 1. Read verify summary.
	summary, err := ReadVerifySummary(root, 10*time.Minute)
	if err != nil {
		return err
	}

	// 2. Determine current platform.
	platform := runtime.GOOS + "-" + runtime.GOARCH

	// 3. Load baselines.
	baselines, err := LoadBaselines(root)
	if err != nil {
		return err
	}

	platformBaselines, ok := baselines[platform]
	if !ok {
		fmt.Printf("commit gate: no baselines for platform %s — skipping\n", platform)
		return nil
	}

	// 4. Find host and wasm summaries.
	var hostSummary, wasmSummary *TargetSummary
	if s, ok := summary.Targets[platform]; ok {
		hostSummary = &s
	}
	if s, ok := summary.Targets["wasm32-wasi"]; ok {
		wasmSummary = &s
	}

	// 5. Check each metric.
	type regression struct {
		metric    string
		baseline  int
		actual    int
		direction string
	}
	type improvement struct {
		metric   string
		baseline int
		actual   int
	}

	var regressions []regression
	var improvements []improvement
	today := time.Now().Format("2006-01-02")

	// Sort metric names for deterministic output.
	metricNames := make([]string, 0, len(platformBaselines))
	for name := range platformBaselines {
		metricNames = append(metricNames, name)
	}
	sort.Strings(metricNames)

	for _, name := range metricNames {
		bl := platformBaselines[name]
		// Find the mapping for this metric.
		var actual int
		var found bool
		for _, m := range metricMappings {
			if m.metric == name {
				actual, found = m.value(hostSummary, wasmSummary)
				break
			}
		}
		if !found {
			continue
		}

		if !checkRatchet(bl.Direction, bl.Value, actual) {
			regressions = append(regressions, regression{
				metric:    name,
				baseline:  bl.Value,
				actual:    actual,
				direction: bl.Direction,
			})
		} else if actual != bl.Value {
			improvements = append(improvements, improvement{
				metric:   name,
				baseline: bl.Value,
				actual:   actual,
			})
		}
	}

	// 6. Report regressions.
	if len(regressions) > 0 {
		fmt.Println("COMMIT GATE FAILED — quality regression detected:")
		for _, r := range regressions {
			fmt.Printf("  %s: %d → %d (%s, baseline: %d)\n",
				r.metric, r.baseline, r.actual, ratchetVerb(r.direction), r.baseline)
		}
		return fmt.Errorf("%d metric(s) regressed", len(regressions))
	}

	// 7. Report and apply improvements.
	if len(improvements) > 0 {
		fmt.Println("commit gate: baselines improved:")
		for _, imp := range improvements {
			fmt.Printf("  %s: %d → %d\n", imp.metric, imp.baseline, imp.actual)
			bl := platformBaselines[imp.metric]
			bl.Value = imp.actual
			bl.Updated = today
			platformBaselines[imp.metric] = bl
		}
		baselines[platform] = platformBaselines
		if err := SaveBaselines(root, baselines); err != nil {
			return fmt.Errorf("update baselines: %w", err)
		}
		fmt.Printf("commit gate: updated %s — stage tools/gates/baselines.json with your commit\n", baselinesFile)
	} else {
		fmt.Println("commit gate: all metrics match baselines — OK")
	}

	return nil
}
