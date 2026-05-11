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

// VerifySummary holds structured test results written by verify as a sidecar
// file. The commit gate reads this to compare against baselines.
type VerifySummary struct {
	Timestamp string                   `json:"timestamp"`
	Targets   map[string]TargetSummary `json:"targets"`
}

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

const verifySummaryFile = "last-verify.json"

// verifySummaryPath returns the path to the verify summary sidecar file.
func verifySummaryPath(root string) string {
	return filepath.Join(root, ".promise-home", verifySummaryFile)
}

// WriteVerifySummary writes the verify summary sidecar to .promise-home/.
func WriteVerifySummary(root string, summary *VerifySummary) error {
	path := verifySummaryPath(root)
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal verify summary: %w", err)
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// ReadVerifySummary reads the verify summary sidecar. Returns an error if the
// file is missing or stale (older than maxAge).
func ReadVerifySummary(root string, maxAge time.Duration) (*VerifySummary, error) {
	path := verifySummaryPath(root)
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("no verify summary found — run bin/verify first")
	}
	if maxAge > 0 && time.Since(info.ModTime()) > maxAge {
		return nil, fmt.Errorf("verify summary is stale (%s old) — run bin/verify again",
			time.Since(info.ModTime()).Round(time.Second))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read verify summary: %w", err)
	}
	var summary VerifySummary
	if err := json.Unmarshal(data, &summary); err != nil {
		return nil, fmt.Errorf("parse verify summary: %w", err)
	}
	return &summary, nil
}
