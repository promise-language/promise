package common

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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
	Worktree  string             `json:"worktree"`
	Values    map[string]float64 `json:"values"`
}

// The unified gate envelope is GateOutput (see gate_test_json.go): every
// subcommand emits {target, metrics, files?, complete}. Metric-only gates
// (stress, coverage, wasm-size) omit files; test-producing gates (test,
// wasm-test, go-test) populate it. T0763.

const gateValuesFile = "gate-values.json"

// gateValuesPath returns the path to the gate values sidecar file.
func gateValuesPath(root string) string {
	return filepath.Join(root, ".promise-home", gateValuesFile)
}

// WriteGateValues writes the gate values sidecar to .promise-home/, stamping it
// with the identity of the worktree the values were produced from. This is the
// only place the sidecar is created, so every producer records the identity
// without having to know about it.
func WriteGateValues(root string, gv *GateValues) error {
	worktree, err := WorktreeHash(root)
	if err != nil {
		return fmt.Errorf("worktree identity: %w", err)
	}
	stamped := *gv
	stamped.Worktree = worktree

	path := gateValuesPath(root)
	data, err := json.MarshalIndent(&stamped, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gate values: %w", err)
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// ExtractFailedSection extracts the individual failure lines from captured
// promise test output. It finds the "FAILED:" header line and returns everything
// that follows it (preserving indentation). Returns empty string if not found.
func ExtractFailedSection(output string) string {
	lines := strings.Split(output, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "FAILED:" {
			rest := strings.Join(lines[i+1:], "\n")
			return strings.TrimRight(rest, "\n")
		}
	}
	return ""
}

// InvalidateGateValues deletes the gate values sidecar file if it exists.
// Called by context-changing commands (build, make) to prevent stale gate
// values from letting a commit gate pass after code/tools changed.
func InvalidateGateValues(root string) {
	path := gateValuesPath(root)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "warning: could not remove %s: %v\n", gateValuesFile, err)
	}
}

// ReadGateValues reads the gate values sidecar and returns it only if it
// describes the worktree identified by wantWorktree (see WorktreeHash).
//
// Freshness is a content question, not a clock question: values produced from
// this exact tree stay valid at any age, and values produced from any other
// tree are rejected outright. Nothing here consults the time.
func ReadGateValues(root string, wantWorktree string) (*GateValues, error) {
	path := gateValuesPath(root)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("no gate values found — run bin/verify first")
	}
	if err != nil {
		return nil, fmt.Errorf("read gate values: %w", err)
	}
	var gv GateValues
	if err := json.Unmarshal(data, &gv); err != nil {
		return nil, fmt.Errorf("parse gate values: %w", err)
	}
	if gv.Worktree == "" {
		return nil, fmt.Errorf("gate values predate worktree identity — run bin/verify again")
	}
	if gv.Worktree != wantWorktree {
		return nil, fmt.Errorf("the worktree changed since the last verify — run bin/verify again")
	}
	return &gv, nil
}
