package common

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLedger puts a ledger under root/bin, the way the compiler would.
func writeLedger(t *testing.T, root, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(casLedgerPath(root), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCASLedgerFormat pins the on-disk format this module parses. The compiler
// writes it and lives in a DIFFERENT Go module, so the shape is spelled twice
// by necessity — the same case as MuslManifestName. This test and
// casmetrics.TestLedgerFormat are what make a change to one that is not made to
// the other fail immediately, instead of leaving every gate reporting zeros.
func TestCASLedgerFormat(t *testing.T) {
	root := t.TempDir()
	writeLedger(t, root, `{"event":"network","bytes":4096}
{"event":"materialize","name":"llvm-view","bytes":375,"count":1}
{"event":"home","path":"/w/.promise-home"}
`)
	got, err := readCASLedger(root)
	if err != nil {
		t.Fatalf("readCASLedger: %v", err)
	}
	if got.NetworkBytes != 4096 {
		t.Errorf("NetworkBytes = %d, want 4096", got.NetworkBytes)
	}
	if got.MaterializedBytes != 375 {
		t.Errorf("MaterializedBytes = %d, want 375", got.MaterializedBytes)
	}
	if got.Materializations != 1 {
		t.Errorf("Materializations = %d, want 1", got.Materializations)
	}
	if len(got.Homes) != 1 || got.Homes[0] != "/w/.promise-home" {
		t.Errorf("Homes = %v, want [/w/.promise-home]", got.Homes)
	}
	if len(got.Names) != 1 || got.Names[0] != "llvm-view" {
		t.Errorf("Names = %v, want [llvm-view]", got.Names)
	}
	if casLedgerName != ".promise-cas.jsonl" {
		t.Errorf("ledger name drifted: %q", casLedgerName)
	}
	if want := filepath.Join(root, "bin", ".promise-cas.jsonl"); casLedgerPath(root) != want {
		t.Errorf("casLedgerPath = %q, want %q", casLedgerPath(root), want)
	}
}

// TestCASLedgerAbsentFoldsToZero: a run that cost the store nothing is the
// normal case, and it must read as an honest zero rather than as an error.
func TestCASLedgerAbsentFoldsToZero(t *testing.T) {
	got, err := readCASLedger(t.TempDir())
	if err != nil {
		t.Fatalf("an absent ledger errored: %v", err)
	}
	if got.NetworkBytes != 0 || got.Materializations != 0 || len(got.Homes) != 0 {
		t.Errorf("an absent ledger folded to %+v, want zeros", got)
	}
}

// TestCASLedgerSkipsTornLine: a compiler killed mid-append leaves a partial
// trailing line; the rest of the run's accounting still holds.
func TestCASLedgerSkipsTornLine(t *testing.T) {
	root := t.TempDir()
	writeLedger(t, root, "{\"event\":\"network\",\"bytes\":10}\n{\"event\":\"netw")
	got, err := readCASLedger(root)
	if err != nil {
		t.Fatalf("readCASLedger: %v", err)
	}
	if got.NetworkBytes != 10 {
		t.Errorf("NetworkBytes = %d, want 10 (the torn line skipped)", got.NetworkBytes)
	}
}

// TestCASLedgerDeduplicatesHomes: every compiler in a run registers the home it
// used, and hundreds of them registering the same one must read as one home —
// otherwise cas_home_count would measure how many files were compiled.
func TestCASLedgerDeduplicatesHomes(t *testing.T) {
	root := t.TempDir()
	writeLedger(t, root, `{"event":"home","path":"/w/.promise-home"}
{"event":"home","path":"/w/.promise-home"}
{"event":"home","path":"/w/.promise-home"}
`)
	got, _ := readCASLedger(root)
	if len(got.Homes) != 1 {
		t.Errorf("Homes = %v, want one entry", got.Homes)
	}
}

// TestCASWindowValuesAlwaysPresent: the four are reported on every run, zeros
// included — a counter that appears only when non-zero cannot be judged against
// a baseline of zero.
func TestCASWindowValuesAlwaysPresent(t *testing.T) {
	root := t.TempDir()
	if err := resetCASLedger(root); err != nil {
		t.Fatalf("resetCASLedger: %v", err)
	}
	vals, incomplete := casWindow{root: root, open: true}.Values()
	if incomplete != "" {
		t.Errorf("incomplete = %q, want none", incomplete)
	}
	for _, name := range []string{"cas_network_bytes", "cas_materialized_bytes", "cas_materializations", "cas_home_count"} {
		if _, ok := vals[name]; !ok {
			t.Errorf("%s missing from a clean run's values: %v", name, vals)
		}
	}
}

// TestCASWindowCountsHomes is the regression this whole item exists for
// (T2133): a change that gives each test package its own empty PROMISE_HOME
// turns one warm materialization into three cold ones per run. The ledger sits
// beside the COMPILER rather than inside a home, so the three are visible as
// three — and the baseline rejects them.
func TestCASWindowCountsHomes(t *testing.T) {
	root := t.TempDir()
	writeLedger(t, root, `{"event":"home","path":"/w/.promise-home"}
{"event":"home","path":"/tmp/pkg-a-home"}
{"event":"home","path":"/tmp/pkg-b-home"}
`)
	vals, _ := casWindow{root: root, open: true}.Values()
	if vals["cas_home_count"] != 3 {
		t.Fatalf("cas_home_count = %v, want 3", vals["cas_home_count"])
	}
	// The count only does its job if a term on it would reject the three. That
	// term is not enforced yet — a real sweep reports 29 homes (T2150) — so what
	// is pinned here is that the number and the ratchet agree once it is.
	if checkRatchet("down", 1, vals["cas_home_count"]) {
		t.Error("three private homes would pass a down-from-one baseline — the metric would not have caught T2133")
	}
	if !checkRatchet("down", 1, 1) {
		t.Error("the one-home case would fail its own baseline")
	}
}

// TestCASWindowUnopenedReportsNothing: a window that could not be opened
// contributes no numbers and says why. Four zeros would read exactly like a
// clean run, which is the failure mode this accounting exists to remove.
func TestCASWindowUnopenedReportsNothing(t *testing.T) {
	vals, incomplete := casWindow{root: t.TempDir(), why: "no ledger here"}.Values()
	if vals != nil {
		t.Errorf("an unopened window reported %v, want nothing", vals)
	}
	if incomplete == "" {
		t.Error("an unopened window gave no reason")
	}
	metrics, incomplete := casWindow{root: t.TempDir(), why: "no ledger here"}.Metrics()
	if len(metrics) != 0 || incomplete == "" {
		t.Errorf("Metrics() = %v / %q, want none and a reason", metrics, incomplete)
	}
}

// TestCASWindowMetricsAreTheTwoWithAnAbsoluteEndState: docs/gate-system.md
// requires every metric `integration` reports to carry an enforced term on every
// target, and a byte or population count is legitimately non-zero the first time
// a clone builds. So the contract envelope carries only the two whose end state
// is an absolute — no bytes over the wire, one home per run.
func TestCASWindowMetricsAreTheTwoWithAnAbsoluteEndState(t *testing.T) {
	root := t.TempDir()
	writeLedger(t, root, `{"event":"materialize","name":"llvm-view","bytes":400,"count":1}
{"event":"home","path":"/w/.promise-home"}
`)
	metrics, incomplete := casWindow{root: root, open: true}.Metrics()
	if incomplete != "" {
		t.Errorf("incomplete = %q, want none", incomplete)
	}
	got := map[string]float64{}
	for _, m := range metrics {
		got[m.Name] = m.Number()
	}
	if len(got) != 2 {
		t.Fatalf("contract metrics = %v, want exactly the two with an absolute end state", got)
	}
	if got["cas_network_bytes"] != 0 || got["cas_home_count"] != 1 {
		t.Errorf("contract metrics = %v, want cas_network_bytes=0 and cas_home_count=1", got)
	}
}

// TestBaselinesCarryEveryStoreMetricOnEveryTarget: a term is per-target data,
// and a metric tracked on one platform and absent on another means a landing
// decision there rests on a number nobody looks at (docs/gate-system.md). All
// four are registered on all four targets; none is enforced yet, because a real
// sweep reports 22 homes and ~55 MB fetched (T2150) and an enforced term today
// would fail every run rather than the changes that add a home. Promotion is a
// value and a direction in each block, and this test is what makes "in each
// block" mechanical rather than remembered.
func TestBaselinesCarryEveryStoreMetricOnEveryTarget(t *testing.T) {
	root, err := RootForTests()
	if err != nil {
		t.Skipf("no checkout to read baselines from: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, baselinesFile))
	if err != nil {
		t.Fatalf("read baselines: %v", err)
	}
	var baselines Baselines
	if err := json.Unmarshal(data, &baselines); err != nil {
		t.Fatalf("parse baselines: %v", err)
	}
	if len(baselines) == 0 {
		t.Fatal("baselines file names no platform")
	}
	tracked := []string{"cas_network_bytes", "cas_home_count", "cas_materialized_bytes", "cas_materializations"}
	for platform, block := range baselines {
		for _, name := range tracked {
			if _, ok := block[name]; !ok {
				t.Errorf("%s: %s is not tracked — a store metric reported everywhere and registered on only some targets is a number nobody looks at where it is missing", platform, name)
			}
		}
	}
}

// TestRunContractGateAddsTheStoreCost: the process entry point is where a RUN
// is defined, so that is where the store window opens and where the two metrics
// are appended — once, around everything, whether the gate is a leaf or a
// composition. MeasureContractGate stays free of it, so a test measuring a gate
// against this checkout cannot reset the ledger of whatever is measuring at the
// time (which is exactly what it did before, silently zeroing a live verify).
func TestRunContractGateAddsTheStoreCost(t *testing.T) {
	// A ROOT OF ITS OWN, deliberately: this path opens a measurement window, and
	// one opened against the checkout resets the ledger of whatever gate or
	// verify is running at the time. The sandbox guard in TestMain fails the
	// package if that happens; this is what keeps it from happening here.
	// formatted:go over an empty root reports zero, which is all this needs —
	// the subject is the envelope's shape, not the number.
	root := t.TempDir()
	stubGateBuild(t, &fakeBuild{})
	var out bytes.Buffer
	if err := runContractGate(root, []string{"formatted:go", "--envelope"}, &out); err != nil {
		t.Fatalf("runContractGate: %v", err)
	}
	var env Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("envelope does not parse: %v (%s)", err, out.String())
	}
	got := map[string]int{}
	for _, m := range env.Metrics {
		got[m.Name]++
	}
	for _, name := range []string{"cas_network_bytes", "cas_home_count"} {
		if got[name] != 1 {
			t.Errorf("%s appears %d times in the envelope, want exactly 1: %+v", name, got[name], env.Metrics)
		}
	}
	if got["unformatted_go_files"] != 1 {
		t.Errorf("the gate's own metric is missing: %+v", env.Metrics)
	}
}

// TestRunContractGateAddsNothingForFit: fit measures the machine, must answer on
// one that cannot build, and so neither warms a toolchain nor opens a window.
func TestRunContractGateAddsNothingForFit(t *testing.T) {
	root := t.TempDir()
	stubGateBuild(t, &fakeBuild{})
	var out bytes.Buffer
	if err := runContractGate(root, []string{"fit", "--envelope"}, &out); err != nil {
		t.Fatalf("runContractGate: %v", err)
	}
	var env Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("envelope does not parse: %v (%s)", err, out.String())
	}
	for _, m := range env.Metrics {
		if strings.HasPrefix(m.Name, "cas_") {
			t.Errorf("fit reported %s; it measures the machine, not what a run cost the store", m.Name)
		}
	}
	if _, err := os.Stat(casLedgerPath(root)); !os.IsNotExist(err) {
		t.Errorf("fit opened a store window: %v", err)
	}
}

// TestMeasureContractGateTouchesNoLedger is the other half: measuring is a
// function call with no side effect on the store accounting, so a test (or any
// caller) may measure without disturbing a run in progress.
func TestMeasureContractGateTouchesNoLedger(t *testing.T) {
	root, err := RootForTests()
	if err != nil {
		t.Skip("not inside the promise repo:", err)
	}
	stubGateBuild(t, &fakeBuild{})
	before, err := os.ReadFile(casLedgerPath(root))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := MeasureContractGate(root, "formatted:go"); err != nil {
		t.Fatalf("MeasureContractGate: %v", err)
	}
	after, err := os.ReadFile(casLedgerPath(root))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	// A peer process may legitimately APPEND while this runs; what must not
	// happen is losing what was already there, which is a window being reset.
	if !strings.HasPrefix(string(after), string(before)) {
		t.Errorf("measuring reset the live ledger:\n before %q\n after  %q", before, after)
	}
}

// TestCASWindowAddToMergesTheFourValues: the tracker gates report the store's
// cost by merging it into the values they already built, so the merge must add
// all four and disturb nothing the gate measured itself.
func TestCASWindowAddToMergesTheFourValues(t *testing.T) {
	root := t.TempDir()
	writeLedger(t, root, `{"event":"network","bytes":12}
{"event":"materialize","name":"crt-view","bytes":34,"count":2}
{"event":"home","path":"/w/.promise-home"}
`)
	values := map[string]float64{"go_test_count": 7}
	casWindow{root: root, open: true}.AddTo(values)

	want := map[string]float64{
		"go_test_count":          7,
		"cas_network_bytes":      12,
		"cas_materialized_bytes": 34,
		"cas_materializations":   2,
		"cas_home_count":         1,
	}
	for name, w := range want {
		if values[name] != w {
			t.Errorf("%s = %v, want %v", name, values[name], w)
		}
	}
	if len(values) != len(want) {
		t.Errorf("AddTo left %d values, want %d: %v", len(values), len(want), values)
	}
}

// TestCASWindowAddToContributesNothingWhenUnopened: four zeros read exactly like
// a clean run, so a window that could not be opened adds no keys at all — the
// gate then reports the metrics it did measure and none it did not.
func TestCASWindowAddToContributesNothingWhenUnopened(t *testing.T) {
	values := map[string]float64{"go_test_count": 7}
	casWindow{root: t.TempDir(), why: "no ledger here"}.AddTo(values)
	if len(values) != 1 || values["go_test_count"] != 7 {
		t.Errorf("an unopened window wrote into the gate's values: %v", values)
	}
}

// TestCASWindowSummaryIsSilentWhenTheRunCostNothing: bin/verify's summary is
// read by tailing it, so a counter that is zero on every warm run must not cost
// every warm run a line to say so.
func TestCASWindowSummaryIsSilentWhenTheRunCostNothing(t *testing.T) {
	root := t.TempDir()
	if err := resetCASLedger(root); err != nil {
		t.Fatal(err)
	}
	if line := (casWindow{root: root, open: true}).Summary(); line != "" {
		t.Errorf("a zero-cost run printed %q, want nothing", line)
	}
	if line := (casWindow{root: root, why: "unopened"}).Summary(); line != "" {
		t.Errorf("an unopened window printed %q, want nothing", line)
	}
}

// TestCASWindowSummaryNamesItsCause: a non-zero reading says how many homes
// paid, what it cost, and which trees — so the line points at the change that
// caused it rather than only at the fact that something did.
func TestCASWindowSummaryNamesItsCause(t *testing.T) {
	root := t.TempDir()
	writeLedger(t, root, `{"event":"network","bytes":74000000}
{"event":"materialize","name":"llvm-view","bytes":9961472,"count":28}
{"event":"materialize","name":"crt-view","count":28}
{"event":"home","path":"/w/.promise-home"}
{"event":"home","path":"/tmp/private-home"}
`)
	line := (casWindow{root: root, open: true}).Summary()
	for _, want := range []string{
		"Store cost:", "56 materialization(s)", "over 2 home(s)",
		"9.5 MB written", "70.6 MB fetched", "(crt-view, llvm-view)",
		// Past one home the paths themselves are the diagnosis: each extra one
		// is a t.TempDir() named after the test that built it. Without them the
		// count says a change added a home but not which, and finding out means
		// re-running the sweep with the ledger open (T2150).
		"home: /w/.promise-home", "home: /tmp/private-home",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q is missing %q", line, want)
		}
	}
}

// TestCASWindowSummaryDoesNotNameTheOneExpectedHome: the end state is one
// home, so naming it on every run that materialized anything would put a path
// nobody needs into every cold build's summary. The paths appear only once
// there is more than one, which is the case worth reading.
func TestCASWindowSummaryDoesNotNameTheOneExpectedHome(t *testing.T) {
	root := t.TempDir()
	writeLedger(t, root, `{"event":"materialize","name":"llvm-view","bytes":1048576,"count":1}
{"event":"home","path":"/w/.promise-home"}
`)
	line := (casWindow{root: root, open: true}).Summary()
	if !strings.Contains(line, "over 1 home(s)") {
		t.Errorf("line %q does not report the one home", line)
	}
	if strings.Contains(line, "home: ") {
		t.Errorf("line %q names the expected home; only extras are worth a path", line)
	}
}

// TestCASWindowRefusesARootThatCannotHoldALedger: where the window cannot be
// opened the gate is told why rather than handed zeros, which is the one case
// that must not read like a clean run. A file where bin/ should be is the
// cheapest way to make the open fail without a permission trick that root or
// Windows would skip.
func TestCASWindowRefusesARootThatCannotHoldALedger(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "bin"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := openCASWindow(root)
	if w.open {
		t.Fatal("a root whose bin/ is a file opened a window")
	}
	vals, incomplete := w.Values()
	if vals != nil {
		t.Errorf("an unopenable window reported %v", vals)
	}
	if !strings.Contains(incomplete, "not measured") {
		t.Errorf("the reason %q does not say the cost was not measured", incomplete)
	}
}

// TestCASWindowReportsAnUnreadableLedger: the ledger existing but not being
// readable is a third state — not absent, not folded — and it too must be a
// reason rather than zeros.
func TestCASWindowReportsAnUnreadableLedger(t *testing.T) {
	root := t.TempDir()
	// A directory where the ledger file should be: ReadFile fails with something
	// other than "does not exist", which is the branch under test.
	if err := os.MkdirAll(casLedgerPath(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := readCASLedger(root); err == nil {
		t.Fatal("reading a directory as a ledger succeeded")
	}
	vals, incomplete := casWindow{root: root, open: true}.Values()
	if vals != nil {
		t.Errorf("an unreadable ledger reported %v", vals)
	}
	if !strings.Contains(incomplete, "not measured") {
		t.Errorf("the reason %q does not say the cost was not measured", incomplete)
	}
	metrics, incomplete := casWindow{root: root, open: true}.Metrics()
	if len(metrics) != 0 || incomplete == "" {
		t.Errorf("Metrics() = %v / %q, want none and a reason", metrics, incomplete)
	}
}

// TestEnsureToolchainWarmSkipsAnUnbuiltTree: the warm-up is best effort and must
// not be a way for a gate to fail. With no compiler to run it does nothing at
// all — no process, no error.
func TestEnsureToolchainWarmSkipsAnUnbuiltTree(t *testing.T) {
	ensureToolchainWarm(t.TempDir()) // no bin/promise: returns without spawning
}

// TestCASWindowSummaryIsSilentOnAnUnreadableLedger: a ledger that cannot be READ
// (as opposed to one that is absent, which folds to an honest zero) must produce
// no summary line rather than a line of zeros. Values() answers the same case
// with a reason, because a gate has to distinguish "clean" from "not measured";
// the human summary has no such obligation and says nothing.
//
// A directory where the ledger file belongs is the portable way to make
// os.ReadFile fail — no permission trick that root or Windows would shrug off.
func TestCASWindowSummaryIsSilentOnAnUnreadableLedger(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(casLedgerPath(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if line := (casWindow{root: root, open: true}).Summary(); line != "" {
		t.Errorf("an unreadable ledger produced %q, want nothing", line)
	}
	// And the machine-readable side still reports it as unmeasured, so the two
	// disagree only in verbosity.
	if vals, incomplete := (casWindow{root: root, open: true}).Values(); vals != nil || incomplete == "" {
		t.Errorf("Values() = %v / %q, want no numbers and a reason", vals, incomplete)
	}
}
