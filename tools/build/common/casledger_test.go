package common

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLedger puts a ledger in the worktree's scratch dir, the way the compiler
// would.
func writeLedger(t *testing.T, root, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(casLedgerPath(root)), 0o755); err != nil {
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
	if want := filepath.Join(root, ".home", "tmp", ".promise-cas.jsonl"); casLedgerPath(root) != want {
		t.Errorf("casLedgerPath = %q, want %q", casLedgerPath(root), want)
	}
}

// TestCASLedgerIsNotUnderBin states the operator rule as a test, so an edit that
// moves the ledger back beside the binary fails here rather than quietly
// reintroducing a writer into a directory reserved for ./make and `workspace
// setup/update` (T2211).
func TestCASLedgerIsNotUnderBin(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin") + string(filepath.Separator)
	if got := casLedgerPath(root); strings.HasPrefix(got, binDir) {
		t.Errorf("the ledger is under bin/ (%q) — only ./make and `workspace "+
			"setup/update` write there", got)
	}
}

// TestRemoveLegacyCASLedgerClearsBinAndNothingElse covers the migration step
// T2211 added to the build. Both halves matter, and the second more than the
// first: bin/ also holds the build's own sidecars, so a cleanup that ever grew
// into a pattern sweep would delete the up-to-date marker and make every
// subsequent build think itself stale.
//
// The build is the only caller because bin/ is gitignored — no commit can remove
// a file there — and it is the only writer allowed in that directory at all.
func TestRemoveLegacyCASLedgerClearsBinAndNothingElse(t *testing.T) {
	binDir := t.TempDir()
	legacy := filepath.Join(binDir, casLedgerName)
	keep := map[string]string{
		".promise.buildinfo": "2026.10-abc1234\n",
		"promise":            "the compiler itself",
		"verify":             "a tool",
	}
	if err := os.WriteFile(legacy, []byte(`{"event":"network","bytes":4096}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, contents := range keep {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(contents), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	removeLegacyCASLedger(binDir)

	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("the legacy ledger survived the build (stat err = %v) — it would "+
			"then sit in bin/ forever, since nothing tracked can remove it", err)
	}
	for name, contents := range keep {
		got, err := os.ReadFile(filepath.Join(binDir, name))
		if err != nil {
			t.Errorf("the cleanup removed bin/%s, which is the build's own: %v", name, err)
			continue
		}
		if string(got) != contents {
			t.Errorf("bin/%s reads %q, want %q", name, got, contents)
		}
	}
}

// TestRemoveLegacyCASLedgerAcceptsACleanBin: every build after the first runs
// this against a bin/ that no longer holds the file, and on a fresh clone there
// was never one. Neither may be an error — the cleanup is called for its effect
// and its result is deliberately not checked at the call site.
func TestRemoveLegacyCASLedgerAcceptsACleanBin(t *testing.T) {
	binDir := t.TempDir()
	removeLegacyCASLedger(binDir) // absent
	removeLegacyCASLedger(binDir) // still absent
	if entries, err := os.ReadDir(binDir); err != nil || len(entries) != 0 {
		t.Errorf("a clean bin/ did not stay clean: %v entries, err %v", len(entries), err)
	}
}

// TestCASLedgerIsIgnoredByGit is what keeps the blessing honest: WorktreeHash
// covers untracked-but-not-ignored files, so a ledger git could see would make
// every gate run change the tree identity that run is measuring. The coupling is
// with .gitignore rather than with any code here, which is exactly why it needs
// a test — nothing else would notice the rule being dropped.
func TestCASLedgerIsIgnoredByGit(t *testing.T) {
	root, err := RootForTests()
	if err != nil {
		t.Skipf("no repo to ask git about: %v", err)
	}
	rel, err := filepath.Rel(root, casLedgerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RunBytesIn(root, "git", "check-ignore", "-q", filepath.ToSlash(rel)); err != nil {
		t.Errorf("git does not ignore %s (%v) — a gate run would then change the "+
			"worktree hash it is measuring, and no verify could bless a tree", rel, err)
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
	// The count only does its job if the term on it rejects the three. That term
	// is `down` from 1 on every target (T2153), so this pins the number and the
	// ratchet agreeing: three private homes fail it, one passes.
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

// TestCASWindowAddToEnvelopeIsWhatBothJudgedPathsUse: `bin/gate` and bin/verify
// both produce an envelope that gets judged, and they must carry one metric set
// — "verify passed" and "integration passed" are one answer about one tree
// rather than two that can differ (docs/gate-system.md). Now that these two
// metrics are enforced (T2153), a path that omitted them would bless a tree the
// other path rejects, so what is pinned here is the append itself.
func TestCASWindowAddToEnvelopeIsWhatBothJudgedPathsUse(t *testing.T) {
	root := t.TempDir()
	writeLedger(t, root, `{"event":"network","bytes":64}
{"event":"home","path":"/w/.promise-home"}
{"event":"home","path":"/tmp/private-home"}
`)
	env := Envelope{Metrics: []Metric{Count("vet_findings", 0)}}
	casWindow{root: root, open: true}.AddToEnvelope(&env)

	got := map[string]float64{}
	for _, m := range env.Metrics {
		got[m.Name] = m.Number()
	}
	if len(got) != 3 {
		t.Fatalf("metrics = %v, want the gate's own plus the two store metrics", got)
	}
	if got["cas_network_bytes"] != 64 || got["cas_home_count"] != 2 {
		t.Errorf("metrics = %v, want cas_network_bytes=64 and cas_home_count=2", got)
	}
	if env.Incomplete != "" {
		t.Errorf("incomplete = %q, want none — the window was measured", env.Incomplete)
	}
	// Both readings fail their own term, which is the point of enforcing them:
	// a byte off the wire and a second private home are each a defect.
	if checkRatchet("exact", 0, got["cas_network_bytes"]) {
		t.Error("64 bytes off the wire passes an exact-zero baseline")
	}
	if checkRatchet("down", 1, got["cas_home_count"]) {
		t.Error("two homes passes a down-from-one baseline")
	}
}

// TestCASWindowAddToEnvelopeSaysWhyWhenUnmeasured: an unopened window adds no
// numbers and carries its reason onto the envelope, so a run that could not
// measure the store is judged as incomplete rather than as a clean zero.
func TestCASWindowAddToEnvelopeSaysWhyWhenUnmeasured(t *testing.T) {
	env := Envelope{Metrics: []Metric{Count("vet_findings", 0)}}
	casWindow{root: t.TempDir(), why: "no ledger here"}.AddToEnvelope(&env)

	if len(env.Metrics) != 1 {
		t.Errorf("metrics = %v, want only the gate's own", env.Metrics)
	}
	if !strings.Contains(env.Incomplete, "no ledger here") {
		t.Errorf("incomplete = %q, want the window's reason", env.Incomplete)
	}
}

// TestContractStoreMetricPredicateMatchesMetrics: isContractStoreMetric spells
// the two names a second time, because a metric name only exists to the drift
// scanner by being written into a Count/Size call. This is the pin that keeps
// the two spellings one set — a name added to Metrics and not to the predicate
// would silently reach verify's gate values, which is the ratchet these are
// deliberately kept out of.
func TestContractStoreMetricPredicateMatchesMetrics(t *testing.T) {
	root := t.TempDir()
	writeLedger(t, root, `{"event":"home","path":"/w/.promise-home"}`+"\n")
	metrics, _ := casWindow{root: root, open: true}.Metrics()
	if len(metrics) == 0 {
		t.Fatal("the window reported no contract metrics at all")
	}
	for _, m := range metrics {
		if !isContractStoreMetric(m.Name) {
			t.Errorf("Metrics reports %q but isContractStoreMetric does not know it — it would reach verify's gate values", m.Name)
		}
	}
	// And nothing else claims to be one: a gate's own metric wrongly matching
	// would be dropped from the sidecar that advances its baseline.
	for _, name := range []string{"host_leak_count", "vet_findings", "cas_materialized_bytes", "cas_materializations"} {
		if isContractStoreMetric(name) {
			t.Errorf("isContractStoreMetric claims %q, which Metrics does not report", name)
		}
	}
}

// TestVerifyGateValuesDropTheStoreMetrics: verify JUDGES the store metrics (the
// test above) and must not feed them to whatever advances a baseline. Its Go
// phase does not pass -count=1, so a cache-replayed run spawns no compiler and
// reports no home where a cold one reports its one; a ratchet fed from the
// first settles below every later run and then fails it (T2150/T2153).
func TestVerifyGateValuesDropTheStoreMetrics(t *testing.T) {
	root := initBareGitRepo(t)
	if err := os.MkdirAll(filepath.Join(root, ".promise-home"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &verifyRun{root: root, env: Envelope{
		Target: "darwin-arm64",
		Metrics: []Metric{
			Count("host_leak_count", 0),
			Size("cas_network_bytes", 0, "bytes"),
			Count("cas_home_count", 1),
		},
	}}
	r.writeGateValues()

	worktree, err := WorktreeHash(root)
	if err != nil {
		t.Fatal(err)
	}
	gv, err := ReadGateValues(root, worktree)
	if err != nil {
		t.Fatalf("read gate values: %v", err)
	}
	if _, ok := gv.Values["host_leak_count"]; !ok {
		t.Errorf("gate values = %v, want the gate's own metrics kept", gv.Values)
	}
	for _, name := range []string{"cas_network_bytes", "cas_home_count"} {
		if _, ok := gv.Values[name]; ok {
			t.Errorf("gate values carry %s = %v; a baseline fed from a cache-replayed run settles below every cold one",
				name, gv.Values[name])
		}
	}
}

// TestBaselinesCarryEveryStoreMetricOnEveryTarget: a term is per-target data,
// and a metric tracked on one platform and absent on another means a landing
// decision there rests on a number nobody looks at (docs/gate-system.md). All
// four are registered on all four targets: the two with an absolute end state
// carry enforced terms (T2153), the two that differ by platform by construction
// are tracked. What this test makes mechanical rather than remembered is the
// "on every target" half — a metric registered on three blocks of four is
// invisible on the fourth until somebody sits down there.
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
// that must not read like a clean run. A file where the ledger's own directory
// should be is the cheapest way to make the open fail without a permission trick
// that root or Windows would skip.
func TestCASWindowRefusesARootThatCannotHoldALedger(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".home"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := openCASWindow(root)
	if w.open {
		t.Fatal("a root whose .home is a file opened a window")
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

// TestBothJudgedEnvelopesAppendTheStoreCost pins the call itself, in both
// places that build an envelope somebody judges: `bin/gate`'s process entry
// point and bin/verify's integration step.
//
// A source scan rather than a behavioural test, for the reason
// reportedMetricNames gives: exercising either path means measuring
// `integration`, which is a quarter of an hour. stepIntegration is 0%-covered
// and stays that way until its steps are injectable (T2092), so without this
// the enforcement verify gained with these terms could be deleted in one line
// and every test would still pass — and the failure it would let through is
// the one these metrics exist to catch: a change that adds a private
// PROMISE_HOME, blessed by verify, rejected by `bin/run integration`.
func TestBothJudgedEnvelopesAppendTheStoreCost(t *testing.T) {
	for _, want := range []struct{ file, fn string }{
		{"verify.go", "stepIntegration"},
		{"gate_contract.go", "runContractGate"},
	} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, want.file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", want.file, err)
		}
		var body *ast.BlockStmt
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == want.fn {
				body = fn.Body
			}
		}
		if body == nil {
			t.Errorf("%s: no func %s — this guard is looking for a function that moved", want.file, want.fn)
			continue
		}
		found := false
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "AddToEnvelope" {
				found = true
			}
			return true
		})
		if !found {
			t.Errorf("%s: %s builds a judged envelope without calling AddToEnvelope — "+
				"the store metrics would be enforced on the other path only, and the two verdicts could differ",
				want.file, want.fn)
		}
	}
}

// TestVerifyGateValuesSurvivesAnUnwritableSidecar covers the branch that keeps a
// sidecar failure warning-only. The blessing is the hard one; this is not, so a
// root that cannot take the file must leave the run intact rather than fail it.
func TestVerifyGateValuesSurvivesAnUnwritableSidecar(t *testing.T) {
	// Not a git repo, so WriteGateValues cannot stamp a worktree identity and
	// refuses — the failure TestWriteGateValues_NotAGitRepo pins from below.
	r := &verifyRun{root: t.TempDir(), env: Envelope{
		Target:  "darwin-arm64",
		Metrics: []Metric{Count("host_leak_count", 0)},
	}}
	r.writeGateValues() // must not panic, and must not fail the run
	if _, err := os.Stat(filepath.Join(r.root, ".promise-home", "gate-values.json")); err == nil {
		t.Error("a sidecar was written into a root with no worktree identity")
	}
}
