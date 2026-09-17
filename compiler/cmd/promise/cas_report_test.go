package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/casmetrics"
)

// TestStoreCostLineIsSilentWhenTheRunCostNothing: the test output is read by
// tailing it, so a counter that is zero on every warm run must not cost every
// warm run a line to say so (CLAUDE.md "Test output format").
func TestStoreCostLineIsSilentWhenTheRunCostNothing(t *testing.T) {
	t.Parallel()
	c := storeCost{Available: true}
	if line := c.line(); line != "" {
		t.Errorf("a zero-cost run printed %q, want nothing", line)
	}
}

// TestStoreCostLineNamesItsCause: a non-zero reading says what materialized, so
// the line points at the change that caused it rather than only at the fact
// that something did.
func TestStoreCostLineNamesItsCause(t *testing.T) {
	t.Parallel()
	c := storeCost{
		NetworkBytes:      2 * 1024 * 1024,
		MaterializedBytes: 375 * 1024 * 1024,
		Materializations:  3,
		Names:             []string{"llvm-view", "crt-view"},
		Available:         true,
	}
	line := c.line()
	for _, want := range []string{"store:", "3 materializations", "llvm-view", "crt-view", "375 MB written", "2 MB fetched"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q is missing %q", line, want)
		}
	}
}

// TestStoreCostLineSingularMaterialization pins the wording for the ordinary
// cold run, which populates exactly one view.
func TestStoreCostLineSingularMaterialization(t *testing.T) {
	t.Parallel()
	c := storeCost{Materializations: 1, Names: []string{"crt-view"}, Available: true}
	if got := c.line(); !strings.Contains(got, "1 materialization (crt-view)") {
		t.Errorf("line = %q, want a singular materialization", got)
	}
}

// TestStoreCostLineSilentWhenNotMeasured: where the ledger cannot be written —
// a read-only install — the zeros mean "not measured", so the human path says
// nothing rather than claiming a clean run. The --json record carries the
// distinction for anything that needs it.
func TestStoreCostLineSilentWhenNotMeasured(t *testing.T) {
	t.Parallel()
	c := storeCost{NetworkBytes: 5, Materializations: 1, Available: false}
	if line := c.line(); line != "" {
		t.Errorf("an unmeasured run printed %q, want nothing", line)
	}
}

// TestCASRecordAlwaysCarriesItsZeros: a metric emitted only when non-zero
// cannot be judged against a baseline of zero, so the --json record is written
// on every run with every field present.
func TestCASRecordAlwaysCarriesItsZeros(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	writeCASRecord(&buf, storeCost{Available: true})

	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
		t.Fatalf("record is not one JSON object: %v (%q)", err, buf.String())
	}
	if got["kind"] != "cas" {
		t.Errorf("kind = %v, want \"cas\"", got["kind"])
	}
	for _, field := range []string{"network_bytes", "materialized_bytes", "materializations", "available"} {
		if _, ok := got[field]; !ok {
			t.Errorf("record omits %q: %s", field, buf.String())
		}
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Error("record is not newline-terminated, so it cannot share the JSONL stream")
	}
}

// TestCASRecordIsSkippedByTheTestRecordReader: the store record shares the
// --json stream with the per-test records, and a reader keying on (file, test)
// must pass over it exactly as it does the coverage record.
func TestCASRecordIsSkippedByTheTestRecordReader(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	writeCASRecord(&buf, storeCost{NetworkBytes: 1, Available: true})

	var rec testRecord
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("the store record does not parse as a record at all: %v", err)
	}
	if rec.Test != "" || rec.Status != "" {
		t.Errorf("the store record carries test identity (%q/%q) and would be counted as a test", rec.Test, rec.Status)
	}
}

// TestStoreWindowReportsOnlyItsOwnDelta: a run reports what IT cost, not what
// the home has ever cost, so a second run over a warm home reads zero even
// though the ledger still names what the first one did.
func TestStoreWindowReportsOnlyItsOwnDelta(t *testing.T) {
	t.Parallel()
	before := storeWindow{before: ledgerReading(100, 200, 2, "llvm-view")}
	got := before.costAgainst(ledgerReading(150, 260, 3, "llvm-view", "crt-view"))
	if got.NetworkBytes != 50 {
		t.Errorf("NetworkBytes = %d, want 50", got.NetworkBytes)
	}
	if got.MaterializedBytes != 60 {
		t.Errorf("MaterializedBytes = %d, want 60", got.MaterializedBytes)
	}
	if got.Materializations != 1 {
		t.Errorf("Materializations = %d, want 1", got.Materializations)
	}
	if len(got.Names) != 1 || got.Names[0] != "crt-view" {
		t.Errorf("Names = %v, want only the one this window added", got.Names)
	}
}

// ledgerReading builds a ledger fold for the delta tests above.
func ledgerReading(network, materialized int64, materializations int, names ...string) casmetrics.Ledger {
	return casmetrics.Ledger{
		NetworkBytes:      network,
		MaterializedBytes: materialized,
		Materializations:  materializations,
		Names:             names,
		Available:         true,
	}
}

// TestStoreWindowReportsUnmeasuredWhenTheLedgerWasReset: a peer gate or verify
// opening its own window in the same worktree empties the ledger, and this run's
// delta then describes nothing. Negative counters would be nonsense and a
// clamped zero would be a lie — "not measured" is the honest answer, and the
// only one a reader can act on.
func TestStoreWindowReportsUnmeasuredWhenTheLedgerWasReset(t *testing.T) {
	t.Parallel()
	w := storeWindow{before: ledgerReading(500, 900, 4, "llvm-view")}
	got := w.costAgainst(ledgerReading(0, 0, 0))
	if got.Available {
		t.Errorf("a reset ledger reported %+v as measured", got)
	}
	if got.NetworkBytes != 0 || got.MaterializedBytes != 0 || got.Materializations != 0 {
		t.Errorf("a reset ledger reported numbers: %+v", got)
	}
	if line := got.line(); line != "" {
		t.Errorf("a reset ledger printed %q", line)
	}
}

// TestChildOpensNoStoreWindow: a child of the multi-file runner measures
// nothing — the parent's window covers it — so it must not pay a ledger read
// per file, nor print a line into the output the parent re-parses.
func TestChildOpensNoStoreWindow(t *testing.T) {
	t.Setenv(testChildEnv, "1")
	if got := openStoreWindow(); got.before.Available {
		t.Errorf("a child read the ledger: %+v", got.before)
	}
	t.Setenv(testChildEnv, "")
	if got := openStoreWindow(); !got.before.Available {
		t.Error("a top-level run did not read the ledger")
	}
}

// TestPrintStoreCostWritesOneLineForACostlyRun: the human path is a single line
// after the summary block, and this is the only place it is produced.
//
// Not parallel: it swaps the package renderer, as the other renderer tests do.
func TestPrintStoreCostWritesOneLineForACostlyRun(t *testing.T) {
	out := swapProgress(t)
	t.Setenv(testChildEnv, "")

	// A window whose "before" is an unreachable-high reading would produce a
	// negative delta, so the cost is driven through a window that saw nothing
	// and a ledger that then holds something.
	printStoreCostAgainst(storeWindow{}, ledgerReading(2*1024*1024, 375*1024*1024, 3, "llvm-view"))

	got := out.String()
	for _, want := range []string{"store:", "3 materializations", "llvm-view", "375 MB written", "2 MB fetched"} {
		if !strings.Contains(got, want) {
			t.Errorf("printed %q, missing %q", got, want)
		}
	}
	if n := strings.Count(strings.TrimSpace(got), "\n"); n != 0 {
		t.Errorf("printed %d extra lines: %q", n, got)
	}
}

// TestPrintStoreCostIsSilentOnAWarmRunAndInAChild: the two ways it must write
// nothing at all — a run that cost the store nothing (a line on every warm run
// would cost every tail-read one), and a child of the multi-file runner (whose
// line would land in the output its parent re-parses).
func TestPrintStoreCostIsSilentOnAWarmRunAndInAChild(t *testing.T) {
	out := swapProgress(t)

	t.Setenv(testChildEnv, "")
	printStoreCostAgainst(storeWindow{}, ledgerReading(0, 0, 0))
	if got := out.String(); got != "" {
		t.Errorf("a warm run printed %q", got)
	}

	t.Setenv(testChildEnv, "1")
	printStoreCostAgainst(storeWindow{}, ledgerReading(99, 99, 9, "llvm-view"))
	if got := out.String(); got != "" {
		t.Errorf("a child printed %q", got)
	}
	// The production entry point, which folds the real ledger: under the child
	// guard it must reach neither the ledger nor the renderer.
	printStoreCost(openStoreWindow())
	if got := out.String(); got != "" {
		t.Errorf("printStoreCost printed %q in a child", got)
	}
}

// TestStoreWindowCostReadsTheRealLedger covers the one step the delta tests
// stub out: cost() folding the ledger beside this binary. Its numbers are
// whatever the process has done, so what is asserted is that it reads at all
// rather than reporting the "not measured" an unreadable ledger would.
func TestStoreWindowCostReadsTheRealLedger(t *testing.T) {
	if !casmetrics.Read().Available {
		t.Skip("no writable ledger beside this test binary")
	}
	if got := openStoreWindow().cost(); !got.Available {
		t.Errorf("cost() reported the ledger unreadable: %+v", got)
	}
}

// swapProgress points the package renderer at a buffer for the duration of a
// test, the way the other renderer tests do, and returns it.
func swapProgress(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	saved := progress
	t.Cleanup(func() { progress = saved })
	progress = newRenderer(progressFull, &out, &bytes.Buffer{})
	return &out
}
