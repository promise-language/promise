package common

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// T0763: the gate consumes the `promise test --json` stream (one JSON record
// per eligible test) as the single authoritative source of test identity and
// status. It relativizes each record's absolute file path against the repo
// root (the runner does NOT deduce the root — that is the gate's job), groups
// records by file into a two-level envelope, and derives metrics by counting
// statuses. No human-output scraping is involved, so identity is stable across
// runs and never varies with outcome.

// jsonlRecord mirrors one line of the runner's --json output.
type jsonlRecord struct {
	File    string  `json:"file"`
	Test    string  `json:"test"`
	Status  string  `json:"status"`
	Elapsed float64 `json:"elapsed"`
	Context string  `json:"context,omitempty"`
}

// TestRecord is one test's result within a file group (file lives on the group).
type TestRecord struct {
	Test    string  `json:"test"`
	Status  string  `json:"status"`
	Elapsed float64 `json:"elapsed"`
	Context string  `json:"context,omitempty"`
}

// TestFileGroup is all tests belonging to one source file.
type TestFileGroup struct {
	File  string       `json:"file"`
	Tests []TestRecord `json:"tests"`
}

// GateOutput is the JSON envelope a test gate writes to stdout. All records
// belong to a single target (the invariant: one gate invocation = one target),
// so target is stamped once at the top rather than on every record.
type GateOutput struct {
	Target   string             `json:"target"`
	Metrics  map[string]float64 `json:"metrics"`
	Files    []TestFileGroup    `json:"files,omitempty"`
	Complete string             `json:"complete,omitempty"`
}

// statusMetricSuffix maps a record status to its metric-name suffix. Statuses
// not present here are still grouped into Files but contribute no metric.
var statusMetricSuffix = map[string]string{
	"pass":     "test_count",
	"fail":     "test_failures",
	"leak":     "leak_count",
	"timeout":  "timeout_count",
	"memory":   "memory_count",
	"excluded": "excluded_count",
	"not-run":  "not_run_count",
}

// ParseTestJSONL parses newline-delimited JSON test records, skipping blank or
// malformed lines (e.g. a truncated trailing line from an abruptly terminated
// runner). Returns records in stream order.
func ParseTestJSONL(jsonl string) []jsonlRecord {
	var out []jsonlRecord
	for _, line := range strings.Split(jsonl, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r jsonlRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if r.Test == "" || r.Status == "" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// BuildGateOutput parses the runner's JSONL, relativizes file paths against
// root, groups records by file (preserving stream order), and derives metrics
// named with the given prefix (e.g. "host" → host_test_count). metricKeys lists
// every metric this prefix can emit so absent statuses are reported as 0 (a
// gate must report a stable metric set, not omit a metric just because its
// count happens to be zero this run).
func BuildGateOutput(root, target, metricPrefix, complete, jsonl string) *GateOutput {
	records := ParseTestJSONL(jsonl)

	metrics := map[string]float64{}
	for _, suffix := range statusMetricSuffix {
		metrics[metricPrefix+"_"+suffix] = 0
	}

	var files []TestFileGroup
	indexByFile := map[string]int{}
	for _, r := range records {
		rel := relToRoot(root, r.File)
		idx, ok := indexByFile[rel]
		if !ok {
			idx = len(files)
			indexByFile[rel] = idx
			files = append(files, TestFileGroup{File: rel})
		}
		files[idx].Tests = append(files[idx].Tests, TestRecord{
			Test:    r.Test,
			Status:  r.Status,
			Elapsed: r.Elapsed,
			Context: r.Context,
		})
		if suffix, ok := statusMetricSuffix[r.Status]; ok {
			metrics[metricPrefix+"_"+suffix]++
		}
	}

	return &GateOutput{
		Target:   target,
		Metrics:  metrics,
		Files:    files,
		Complete: complete,
	}
}

// relToRoot returns file relative to root with forward slashes. file is the
// absolute path emitted by the runner; if it is not under root (or Rel fails),
// the cleaned original is returned so nothing is silently dropped.
func relToRoot(root, file string) string {
	if rel, err := filepath.Rel(root, file); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(file)
}
