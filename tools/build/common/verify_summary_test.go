package common

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	root := initBareGitRepo(t)
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

	worktree, err := WorktreeHash(root)
	if err != nil {
		t.Fatalf("worktree hash: %v", err)
	}
	got, err := ReadGateValues(root, worktree)
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

// TestWriteGateValues_NotAGitRepo pins the failure mode of the stamp: with no
// tree identity available, no sidecar is written at all. An unstamped sidecar
// would be worse than none — ReadGateValues would have to either trust it or
// explain itself, and the commit gate would be back to guessing.
func TestWriteGateValues_NotAGitRepo(t *testing.T) {
	// Clear TMPDIR for the same reason as TestWorktreeHash_NotAGitRepo: under
	// bin/verify it points inside the real work tree, where git ls-files works.
	t.Setenv("TMPDIR", "")
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".promise-home"), 0o755)

	gv := &GateValues{Platform: "linux-amd64", Values: map[string]float64{"host_test_count": 1}}
	if err := WriteGateValues(root, gv); err == nil {
		t.Fatal("expected an error with no worktree identity available, got nil")
	}
	if _, err := os.Stat(filepath.Join(root, ".promise-home", gateValuesFile)); !os.IsNotExist(err) {
		t.Error("an unstamped gate values sidecar was written")
	}
}

func TestReadGateValues_Missing(t *testing.T) {
	root := t.TempDir()
	_, err := ReadGateValues(root, "any-hash")
	if err == nil {
		t.Fatal("expected error for missing gate values, got nil")
	}
}

func TestInvalidateGateValues(t *testing.T) {
	root := initBareGitRepo(t)
	os.MkdirAll(filepath.Join(root, ".promise-home"), 0o755)

	// Write gate values, then invalidate — file should be gone.
	gv := &GateValues{
		Timestamp: "2026-04-11T12:00:00Z",
		Platform:  "darwin-arm64",
		Values:    map[string]float64{"host_test_count": 100},
	}
	if err := WriteGateValues(root, gv); err != nil {
		t.Fatalf("write: %v", err)
	}
	InvalidateGateValues(root)
	_, err := ReadGateValues(root, "any-hash")
	if err == nil {
		t.Fatal("expected error after invalidation, got nil")
	}
}

func TestInvalidateGateValues_Missing(t *testing.T) {
	// Invalidating when no file exists should not panic or error.
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".promise-home"), 0o755)
	InvalidateGateValues(root) // should be a no-op
}

func TestInvalidateGateValues_PermissionError(t *testing.T) {
	root := initBareGitRepo(t)
	dir := filepath.Join(root, ".promise-home")
	os.MkdirAll(dir, 0o755)

	// Write a gate values file, then make directory read-only so Remove fails
	// with a permission error (not IsNotExist).
	gv := &GateValues{
		Timestamp: "2026-04-11T12:00:00Z",
		Platform:  "darwin-arm64",
		Values:    map[string]float64{"x": 1},
	}
	if err := WriteGateValues(root, gv); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Arrange for os.Remove to fail with a non-IsNotExist error so the warning
	// path runs and the file survives. The mechanism is platform-specific:
	// POSIX honors directory permissions for unlink; Windows does not (chmod
	// only toggles a per-file read-only bit), so there we instead hold an open
	// handle — os.Open's share mode omits FILE_SHARE_DELETE, making os.Remove
	// fail with a sharing violation.
	path := filepath.Join(dir, gateValuesFile)
	if runtime.GOOS == "windows" {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open to block removal: %v", err)
		}
		defer f.Close()
	} else {
		os.Chmod(dir, 0o555) // read+exec only — Remove will fail
		defer os.Chmod(dir, 0o755)
	}

	// Should not panic; prints warning to stderr.
	InvalidateGateValues(root)

	// File should still exist since removal was denied.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file should still exist after failed removal: %v", err)
	}
}

func TestReadGateValues_MalformedJSON(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".promise-home"), 0o755)

	path := filepath.Join(root, ".promise-home", gateValuesFile)
	os.WriteFile(path, []byte("not json"), 0o644)

	_, err := ReadGateValues(root, "any-hash")
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	if !strings.Contains(err.Error(), "parse gate values") {
		t.Errorf("expected 'parse gate values' in error, got: %v", err)
	}
}

// TestReadGateValues_AgeIsIrrelevant pins the reported symptom (T1962): values
// produced from a tree that has not changed since stay valid however old the
// sidecar is. Freshness is a content question, not a clock question.
func TestReadGateValues_AgeIsIrrelevant(t *testing.T) {
	root := initBareGitRepo(t)
	os.MkdirAll(filepath.Join(root, ".promise-home"), 0o755)
	os.WriteFile(filepath.Join(root, "source.txt"), []byte("original"), 0o644)

	gv := &GateValues{
		Timestamp: "2020-01-01T00:00:00Z",
		Platform:  "linux-amd64",
		Values:    map[string]float64{"host_test_count": 7},
	}
	if err := WriteGateValues(root, gv); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Age the sidecar by two hours — far past any window the old check used.
	path := filepath.Join(root, ".promise-home", gateValuesFile)
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(path, old, old)

	worktree, err := WorktreeHash(root)
	if err != nil {
		t.Fatalf("worktree hash: %v", err)
	}
	got, err := ReadGateValues(root, worktree)
	if err != nil {
		t.Fatalf("expected an unchanged tree to stay valid at any age, got: %v", err)
	}
	if got.Values["host_test_count"] != 7 {
		t.Errorf("host_test_count = %v, want 7", got.Values["host_test_count"])
	}
}

// TestReadGateValues_WorktreeChanged covers the case the old time window
// silently admitted: an edit made after verify, read back immediately.
func TestReadGateValues_WorktreeChanged(t *testing.T) {
	root := initBareGitRepo(t)
	os.MkdirAll(filepath.Join(root, ".promise-home"), 0o755)
	os.WriteFile(filepath.Join(root, "source.txt"), []byte("original"), 0o644)

	gv := &GateValues{Platform: "linux-amd64", Values: map[string]float64{}}
	if err := WriteGateValues(root, gv); err != nil {
		t.Fatalf("write: %v", err)
	}

	os.WriteFile(filepath.Join(root, "source.txt"), []byte("edited"), 0o644)
	worktree, err := WorktreeHash(root)
	if err != nil {
		t.Fatalf("worktree hash: %v", err)
	}
	_, err = ReadGateValues(root, worktree)
	if err == nil {
		t.Fatal("expected an error after the worktree changed, got nil")
	}
	if !strings.Contains(err.Error(), "worktree changed") {
		t.Errorf("expected 'worktree changed' in error, got: %v", err)
	}
}

// TestReadGateValues_NoRecordedWorktree rejects a sidecar written before the
// identity field existed rather than trusting it.
func TestReadGateValues_NoRecordedWorktree(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".promise-home"), 0o755)
	os.WriteFile(filepath.Join(root, ".promise-home", gateValuesFile),
		[]byte(`{"timestamp":"2020-01-01T00:00:00Z","platform":"linux-amd64","values":{}}`), 0o644)

	_, err := ReadGateValues(root, "any-hash")
	if err == nil {
		t.Fatal("expected an error for a sidecar with no worktree identity, got nil")
	}
	if !strings.Contains(err.Error(), "predate worktree identity") {
		t.Errorf("expected 'predate worktree identity' in error, got: %v", err)
	}
}

func GateOutputRoundTrip(t *testing.T) {
	out := &GateOutput{
		Target: "linux-amd64",
		Metrics: map[string]float64{
			"host_test_count":    3656,
			"host_test_failures": 0,
		},
		Files: []TestFileGroup{
			{File: "tests/e2e/basics.pr", Tests: []TestRecord{{Test: "main", Status: "pass", Elapsed: 0.004}}},
			{File: "tests/std/bool_test.pr", Tests: []TestRecord{
				{Test: "test_and", Status: "pass", Elapsed: 0.001},
				{Test: "test_split", Status: "fail", Elapsed: 0.005, Context: "panic: assertion failed"},
			}},
		},
		Complete: "promise-tests",
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got GateOutput
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Target != "linux-amd64" {
		t.Errorf("Target = %q, want linux-amd64", got.Target)
	}
	if got.Complete != "promise-tests" {
		t.Errorf("Complete = %q, want promise-tests", got.Complete)
	}
	if got.Metrics["host_test_count"] != 3656 {
		t.Errorf("metrics[host_test_count] = %v, want 3656", got.Metrics["host_test_count"])
	}
	if len(got.Files) != 2 {
		t.Fatalf("len(Files) = %d, want 2", len(got.Files))
	}
	if got.Files[0].File != "tests/e2e/basics.pr" || got.Files[0].Tests[0].Status != "pass" {
		t.Errorf("Files[0] = %+v", got.Files[0])
	}
	if got.Files[1].Tests[1].Test != "test_split" || got.Files[1].Tests[1].Context != "panic: assertion failed" {
		t.Errorf("Files[1].Tests[1] = %+v", got.Files[1].Tests[1])
	}
}

func GateOutputOmitEmpty(t *testing.T) {
	// Files and Complete should be omitted for a metric-only gate.
	out := &GateOutput{
		Target:  "linux-amd64",
		Metrics: map[string]float64{"stress_iterations": 100},
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	if strings.Contains(s, "files") {
		t.Errorf("expected 'files' to be omitted, got: %s", s)
	}
	if strings.Contains(s, "complete") {
		t.Errorf("expected 'complete' to be omitted, got: %s", s)
	}
}

// The two forms of the same failing run: what verify captured before T1888 and
// what it captures now that a piped `promise test` drops its passing lines. The
// summary and the FAILED: block are byte-identical between them by design.
const (
	capturedFullForm = `pass (0.004s) e2e/basics.pr (3 tests)
FAIL (0.005s) e2e/strings.pr (1/3 failed)
  test_split
    panic: assertion failed
pass (0.001s) e2e/hello.pr
LEAK (0.002s) e2e/leaky.pr (1 leaked)

568 passed, 2 failed, 3 leaked, 1 timed out (117 files, 30.810s)
FAILED:
  e2e/strings.pr: test_split
    panic: assertion failed
`
	capturedPlainForm = `FAIL (0.005s) e2e/strings.pr (1/3 failed)
  test_split
    panic: assertion failed
LEAK (0.002s) e2e/leaky.pr (1 leaked)

568 passed, 2 failed, 3 leaked, 1 timed out (117 files, 30.810s)
FAILED:
  e2e/strings.pr: test_split
    panic: assertion failed
`
)

// TestVerifyReparsers_AgreeAcrossRenderModes is the T1888 constraint on the
// consumer: the tested:promise gate re-parses the captured Promise output for
// its counts, and dropping the passing progress lines must not change them.
//
// Verify no longer re-states the suite's own FAILED: block — the suite streams
// it, so a second copy assembled from a buffer was the same text twice — so the
// counts are what is left to agree.
func TestVerifyReparsers_AgreeAcrossRenderModes(t *testing.T) {
	full := ParseTestSummaryLine(capturedFullForm)
	plain := ParseTestSummaryLine(capturedPlainForm)
	if full == nil || plain == nil {
		t.Fatalf("summary not found: full=%v plain=%v", full, plain)
	}
	if *full != *plain {
		t.Errorf("counts differ between render modes: full %+v, plain %+v", *full, *plain)
	}
	if full.Passed != 568 || full.Failed != 2 || full.Leaked != 3 || full.TimedOut != 1 {
		t.Errorf("counts = %+v, want 568/2/3/1", *full)
	}
}

// TestVerifyReparsers_RejectTransientArtifacts is the negative of the same
// constraint: the in-place rewriting goes to stderr precisely so a carriage
// return can never reach this text. If one ever did, the summary line would no
// longer start at a line boundary and the counts would silently vanish.
func TestVerifyReparsers_RejectTransientArtifacts(t *testing.T) {
	polluted := strings.Replace(capturedPlainForm,
		"\n568 passed", "\rpass (0.001s) e2e/hello.pr\r568 passed", 1)
	if s := ParseTestSummaryLine(polluted); s != nil {
		t.Errorf("a carriage-return-polluted stream parsed as %+v; the test guards that "+
			"the transient line must never reach stdout", *s)
	}
	// The clean stream still parses, so the assertion above is not vacuous.
	if s := ParseTestSummaryLine(capturedPlainForm); s == nil {
		t.Error("the clean plain-form stream must still parse")
	}
}

// TestWriteGateValues_StampIsAuthoritative pins that the writer computes the
// tree identity itself rather than trusting the caller's struct. Every
// producer in gate.go builds a GateValues literal; if a stale or hand-set
// Worktree could survive the write, the gate would validate against whatever
// the producer happened to carry.
func TestWriteGateValues_StampIsAuthoritative(t *testing.T) {
	root := initBareGitRepo(t)
	os.MkdirAll(filepath.Join(root, ".promise-home"), 0o755)
	os.WriteFile(filepath.Join(root, "source.txt"), []byte("original"), 0o644)

	gv := &GateValues{
		Platform: "linux-amd64",
		Worktree: "a-hash-from-some-other-tree",
		Values:   map[string]float64{"host_test_count": 3},
	}
	if err := WriteGateValues(root, gv); err != nil {
		t.Fatalf("write: %v", err)
	}

	worktree, err := WorktreeHash(root)
	if err != nil {
		t.Fatalf("worktree hash: %v", err)
	}
	got, err := ReadGateValues(root, worktree)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Worktree != worktree {
		t.Errorf("recorded worktree = %q, want the real identity %q", got.Worktree, worktree)
	}

	// The caller's struct is stamped on a copy, so a producer that reuses it
	// for a second write cannot carry the first tree's identity forward.
	if gv.Worktree != "a-hash-from-some-other-tree" {
		t.Errorf("caller's struct was mutated: Worktree = %q", gv.Worktree)
	}
}

// TestReadGateValues_Unreadable separates "no values yet" from "values are
// there but cannot be read": the first tells the reader to run verify, the
// second is a real IO fault that must not be reported as a missing sidecar.
func TestReadGateValues_Unreadable(t *testing.T) {
	root := t.TempDir()
	// A directory where the sidecar belongs: os.ReadFile fails with an error
	// that is not os.ErrNotExist.
	if err := os.MkdirAll(gateValuesPath(root), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, err := ReadGateValues(root, "any-hash")
	if err == nil {
		t.Fatal("expected an error for an unreadable sidecar, got nil")
	}
	if !strings.Contains(err.Error(), "read gate values") {
		t.Errorf("expected 'read gate values' in error, got: %v", err)
	}
}

// TestWriteGateValues_UnrepresentableValue covers the marshal failure that a
// metric can actually produce: NaN and ±Inf have no JSON encoding, and a
// ratio computed from an empty denominator yields one. The write must fail
// loudly rather than leave a truncated sidecar the gate would then reject
// with a misleading "parse gate values" error.
func TestWriteGateValues_UnrepresentableValue(t *testing.T) {
	root := initBareGitRepo(t)
	os.MkdirAll(filepath.Join(root, ".promise-home"), 0o755)

	gv := &GateValues{
		Platform: "linux-amd64",
		Values:   map[string]float64{"coverage": math.NaN()},
	}
	if err := WriteGateValues(root, gv); err == nil {
		t.Fatal("expected an error for a NaN metric, got nil")
	}
	if _, err := os.Stat(gateValuesPath(root)); !os.IsNotExist(err) {
		t.Error("a sidecar was written despite the marshal failure")
	}
}

// --- The verify summary block ---

// TestVerifySummary_ReportsWhatWasMeasured is the block pinned whole (T2170).
//
// It is a golden because every property that matters is a property of the TEXT:
// one row per part named as `bin/run` addresses it, each part's own duration and
// no other's, a FAILED form carrying the term it failed, and a part that never
// ran saying so instead of printing 0s. It is also required to be byte-identical
// in all three progress modes, and the way to keep that true is to have one
// rendering with one expected output.
func TestVerifySummary_ReportsWhatWasMeasured(t *testing.T) {
	s := verifySummary{
		Target:   "darwin-arm64",
		Compiler: "2026.10-8d0a3d1 (identity 12e9f923)",
		Parts: []PartResult{
			{Gate: "formatted:go", Metrics: []Metric{Count("unformatted_go_files", 0)}, Elapsed: 400 * time.Millisecond},
			{Gate: "checked:promise", Metrics: []Metric{Count("promise_check_errors", 2)}, Elapsed: 63 * time.Second},
			{Gate: "tested:promise", Incomplete: "the build did not complete, so the Promise suite did not run"},
		},
		Terms: map[string]term{
			"unformatted_go_files": {Kind: "baseline", Direction: "exact", Value: 0},
			"promise_check_errors": {Kind: "cap", Direction: "at_most", Value: 0},
		},
		Failed:  "integration",
		Store:   "CAS: 0 B over the wire",
		Elapsed: 21*time.Minute + 14*time.Second,
	}
	const want = `
====================================================
  Verify Summary
----------------------------------------------------
  Target:          darwin-arm64
  Compiler:        2026.10-8d0a3d1 (identity 12e9f923)
  formatted:go     passed (400ms)
  checked:promise  FAILED (1m3s)
                     promise_check_errors is 2, cap at_most 0
  tested:promise   not measured
                     the build did not complete, so the Promise suite did not run
  CAS: 0 B over the wire
  Total time:      21m14s
====================================================
FAILED: integration
`
	if got := s.String(); got != want {
		t.Errorf("summary =\n%s\nwant\n%s", got, want)
	}
}

// TestVerifySummary_NothingMeasuredNamesTheStepThatStopped: a run that never
// reached the measurement has no rows, and must say that rather than print a
// block of zeros — which reads as a suite that ran and found nothing.
func TestVerifySummary_NothingMeasuredNamesTheStepThatStopped(t *testing.T) {
	got := verifySummary{Target: "linux-amd64", Compiler: "unknown", Failed: "build"}.String()
	if !strings.Contains(got, "nothing was measured — the run stopped at: build") {
		t.Errorf("summary must name the step that stopped the run:\n%s", got)
	}
	if strings.Contains(got, "0s)") {
		t.Errorf("a run that measured nothing must not print durations:\n%s", got)
	}
}

// TestVerifySummary_AGreenRunNamesNoFailure: the FAILED: line is the tail
// marker, so it must not appear on a run that passed.
func TestVerifySummary_AGreenRunNamesNoFailure(t *testing.T) {
	got := verifySummary{
		Target:   "linux-amd64",
		Compiler: "unknown",
		Parts:    []PartResult{{Gate: "tested:go", Metrics: []Metric{Count("go_test_failures", 0)}, Elapsed: time.Second}},
		Elapsed:  time.Minute,
	}.String()
	if strings.Contains(got, "FAILED") {
		t.Errorf("a green run must print no FAILED marker:\n%s", got)
	}
	if !strings.Contains(got, "tested:go    passed (1s)") {
		t.Errorf("a passing part must report its own duration:\n%s", got)
	}
}

// TestParseCompilerIdentity covers the line naming the compiler that produced
// the numbers: the version, and enough of the identity to tell two builds apart.
func TestParseCompilerIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want string
		ok   bool
	}{
		{
			"version and identity",
			`{"version":"2026.10-8d0a3d1","channel":"stable","commit":"8d0a3d12","identity":"12e9f923de115e25d6271da689749f27"}`,
			"2026.10-8d0a3d1 (identity 12e9f923)", true,
		},
		{"no identity", `{"version":"2026.10-dev"}`, "2026.10-dev", true},
		{"short identity is not truncated", `{"version":"v","identity":"abc"}`, "v (identity abc)", true},
		{"not json", "promise version 2026.10", "", false},
		{"no version", `{"identity":"12e9f923"}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseCompilerIdentity(tc.out)
			if ok != tc.ok || got != tc.want {
				t.Errorf("parseCompilerIdentity(%q) = (%q, %v), want (%q, %v)", tc.out, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestCompilerIdentity_UnknownWhenThereIsNoCompiler: the line is non-fatal by
// construction. A run that failed at the build step has no binary to ask, and a
// summary that refused to print because of it would withhold the failure it
// exists to report.
func TestCompilerIdentity_UnknownWhenThereIsNoCompiler(t *testing.T) {
	got := compilerIdentity(t.TempDir())
	if !strings.HasPrefix(got, "unknown (") {
		t.Errorf("compilerIdentity with no binary = %q, want an unknown line", got)
	}
}
