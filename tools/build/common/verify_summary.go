package common

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TargetSummary holds test counts for a single target (e.g., host or wasm).
type TargetSummary struct {
	Passed    int     `json:"passed"`
	Failed    int     `json:"failed"`
	Leaked    int     `json:"leaked"`
	TimedOut  int     `json:"timed_out"`
	ElapsedMs float64 `json:"elapsed_ms"`
}

var testSummaryRe = regexp.MustCompile(
	`^(\d+) passed, (\d+) failed(?:, (\d+) skipped)?(?:, (\d+) leaked)?(?:, (\d+) timed out)?`,
)

// ParseTestSummaryLine extracts test counts from raw test output by finding
// the summary line (e.g., "568 passed, 0 failed, 0 leaked (117 files, 30.810s)").
// Returns nil if no summary line is found.
func ParseTestSummaryLine(output string) *TargetSummary {
	for _, line := range strings.Split(output, "\n") {
		m := testSummaryRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		passed, _ := strconv.Atoi(m[1])
		failed, _ := strconv.Atoi(m[2])
		var leaked, timedOut int
		if m[3] != "" {
			// skipped — not tracked in baselines
		}
		if m[4] != "" {
			leaked, _ = strconv.Atoi(m[4])
		}
		if m[5] != "" {
			timedOut, _ = strconv.Atoi(m[5])
		}
		return &TargetSummary{
			Passed:   passed,
			Failed:   failed,
			Leaked:   leaked,
			TimedOut: timedOut,
		}
	}
	return nil
}

// GateValues holds flat named metric values written by verify as a sidecar file.
// Keys are metric names (e.g. "host_test_count"); values are float64.
// The commit gate reads this directly — no translation layer.
type GateValues struct {
	Timestamp string             `json:"timestamp"`
	Platform  string             `json:"platform"`
	Values    map[string]float64 `json:"values"`
}

const gateValuesFile = "gate-values.json"

// gateValuesPath returns the path to the gate values sidecar file.
func gateValuesPath(root string) string {
	return filepath.Join(root, ".promise-home", gateValuesFile)
}

// WriteGateValues writes the gate values sidecar to .promise-home/.
func WriteGateValues(root string, gv *GateValues) error {
	path := gateValuesPath(root)
	data, err := json.MarshalIndent(gv, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gate values: %w", err)
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// ReadGateValues reads the gate values sidecar. Returns an error if the file
// is missing or stale (older than maxAge).
func ReadGateValues(root string, maxAge time.Duration) (*GateValues, error) {
	path := gateValuesPath(root)
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("no gate values found — run bin/verify first")
	}
	if maxAge > 0 && time.Since(info.ModTime()) > maxAge {
		return nil, fmt.Errorf("gate values are stale (%s old) — run bin/verify again",
			time.Since(info.ModTime()).Round(time.Second))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read gate values: %w", err)
	}
	var gv GateValues
	if err := json.Unmarshal(data, &gv); err != nil {
		return nil, fmt.Errorf("parse gate values: %w", err)
	}
	return &gv, nil
}
