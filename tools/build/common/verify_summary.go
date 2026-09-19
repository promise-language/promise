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

// --- The verify summary ---

// verifySummary is the block bin/verify always prints, whatever became of the
// run. It is rendered from the `integration` envelope and from nothing else
// (T2170): one row per part, named exactly as `bin/run` addresses it, so a
// reader who sees a row fail knows the command that re-measures it.
//
// A VALUE, rendered by String, so the block can be pinned by a test. It is the
// only artefact of a twenty-minute run that an agent reliably reads, and it is
// required to be byte-identical in all three progress modes
// (docs/build-tools.md § Progress rendering) — neither property survives being
// printed line by line from inside the pipeline.
type verifySummary struct {
	// Target is what the measurements speak for. One row per suite × target
	// means one target here: a run reports exactly one.
	Target   string
	Compiler string
	Parts    []PartResult
	// Terms is what the judge applied, by metric name. A part is FAILED when one
	// of its own metrics is outside its term, which is how a row can name the
	// gate to re-run rather than leaving the reader to map metrics to gates.
	Terms   map[string]term
	Failed  string
	Store   string
	Elapsed time.Duration
}

const (
	verifySummaryRule = "===================================================="
	verifySummaryThin = "----------------------------------------------------"
)

// String renders the block.
func (s verifySummary) String() string {
	width := len("Total time:")
	for _, p := range s.Parts {
		width = max(width, len(p.Gate))
	}

	var sb strings.Builder
	sb.WriteString("\n" + verifySummaryRule + "\n")
	sb.WriteString("  Verify Summary\n")
	sb.WriteString(verifySummaryThin + "\n")
	fmt.Fprintf(&sb, "  %-*s  %s\n", width, "Target:", s.Target)
	fmt.Fprintf(&sb, "  %-*s  %s\n", width, "Compiler:", s.Compiler)
	for _, p := range s.Parts {
		status, notes := s.row(p)
		fmt.Fprintf(&sb, "  %-*s  %s\n", width, p.Gate, status)
		for _, n := range notes {
			fmt.Fprintf(&sb, "  %-*s    %s\n", width, "", n)
		}
	}
	if len(s.Parts) == 0 {
		// The run never reached the measurement. Saying so beats a block of rows
		// reporting zero, which reads as a suite that ran and found nothing.
		line := "nothing was measured"
		if s.Failed != "" {
			line += " — the run stopped at: " + s.Failed
		}
		fmt.Fprintf(&sb, "  %s\n", line)
	}
	if s.Store != "" {
		fmt.Fprintf(&sb, "  %s\n", s.Store)
	}
	mins := int(s.Elapsed.Minutes())
	secs := int(s.Elapsed.Seconds()) % 60
	fmt.Fprintf(&sb, "  %-*s  %dm%02ds\n", width, "Total time:", mins, secs)
	sb.WriteString(verifySummaryRule + "\n")
	if s.Failed != "" {
		fmt.Fprintf(&sb, "FAILED: %s\n", s.Failed)
	}
	return sb.String()
}

// row is one part's status and the notes printed under it.
//
// A part that reported no metric at all did not run, and says so WITHOUT a
// duration: the seconds it took to discover it could not run are not a suite's
// runtime, and printing them — or printing `0s` — describes a measurement that
// never happened.
func (s verifySummary) row(p PartResult) (status string, notes []string) {
	var failed []string
	for _, m := range p.Metrics {
		t, judged := s.Terms[m.Name]
		if judged && !metricWithinTerm(m, t) {
			failed = append(failed, fmt.Sprintf("%s is %s, %s %s %s",
				m.Name, m.String(), t.Kind, t.Direction, formatCap(t.Value)))
		}
	}
	switch {
	case len(failed) > 0:
		status = "FAILED (" + p.Elapsed.Round(time.Millisecond).String() + ")"
	case len(p.Metrics) == 0:
		status = "not measured"
	default:
		status = "passed (" + p.Elapsed.Round(time.Millisecond).String() + ")"
	}
	notes = append(notes, failed...)
	if p.Incomplete != "" {
		notes = append(notes, p.Incomplete)
	}
	return status, notes
}

// shortIdentityLen is how much of a compiler identity a summary prints: enough
// to tell two builds apart at a glance, short enough to sit on one line.
const shortIdentityLen = 8

// compilerIdentity is the compiler this run built, as a person reads it: the
// version, and the short form of the identity `promise version -json` reports.
//
// It answers "which compiler produced these numbers" for a block that is often
// the only artefact of a twenty-minute run. READ FROM THE BINARY rather than
// computed here: tools/build cannot import compiler/internal/module, and a
// second implementation of an identity is a second answer.
//
// NON-FATAL by construction. A run that failed at the build step has no binary
// to ask, and a summary that refused to print because of that would withhold the
// failure it exists to report.
func compilerIdentity(root string) string {
	binary := BinaryName()
	out, err := RunOutputIn(root, filepath.Join(root, "bin", binary), "version", "-json")
	if err != nil {
		return "unknown (bin/" + binary + " did not answer)"
	}
	line, ok := parseCompilerIdentity(out)
	if !ok {
		return "unknown (bin/" + binary + " version -json was unreadable)"
	}
	return line
}

// parseCompilerIdentity renders what `promise version -json` printed. Separate
// from the call so the rendering is a pure test: proving it reads the JSON
// should not need a compiler on disk.
func parseCompilerIdentity(out string) (string, bool) {
	var v struct {
		Version  string `json:"version"`
		Identity string `json:"identity"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Version == "" {
		return "", false
	}
	if v.Identity == "" {
		return v.Version, true
	}
	id := v.Identity
	if len(id) > shortIdentityLen {
		id = id[:shortIdentityLen]
	}
	return v.Version + " (identity " + id + ")", true
}
