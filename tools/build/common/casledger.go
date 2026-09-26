package common

// What a gate run costs the content-addressed store (T2143).
//
// Two things move bytes that no gate measured before this: blobs pulled over
// the wire into the store because it did not have them, and bytes written
// exploding store (and embedded) content into a usable on-disk form — the
// llvm-view tools, the CRT/OpenSSL/compiler-rt/winlink trees, the WASM objects,
// the macOS SDK stub, the embedded catalog. Both are side effects of the TREE's
// shape rather than of the work asked for, and both multiply by however many
// isolated caches a change decides to create. T2133 was one warm 375 MB
// materialization becoming three cold ones per test run, unmeasured, for 18
// days.
//
// The compiler appends an event per occurrence to a ledger anchored to its own
// binary (compiler/internal/casmetrics): the worktree's scratch dir when that
// binary is at <root>/bin/<exe>, since bin/ itself is the build's to write. A
// gate empties that ledger once its build and toolchain warm-up are done, and
// reads it back when the measurement is over — so the numbers describe the
// measured phase and not how warm the machine happened to be when it started.
//
// The ledger's path rule and line format are spelled here a second time because
// the tools are a separate Go module from the compiler and cannot import it —
// the same necessity that duplicates MuslManifestName. Both spellings are
// pinned by golden tests (TestCASLedgerFormat here, TestLedgerFormat there), so
// a change to one that is not made to the other fails immediately rather than
// silently reporting zeros forever.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// casLedgerName is the ledger's file name. Keep in lockstep with
// casmetrics.LedgerName.
const casLedgerName = ".promise-cas.jsonl"

// casLedgerPath is where this repository's compiler writes its ledger: the
// worktree's scratch dir, NOT bin/ — only ./make and `workspace setup/update`
// write there (T2211). Keep in lockstep with casmetrics.LedgerRelPath.
func casLedgerPath(root string) string {
	return filepath.Join(root, ".home", "tmp", casLedgerName)
}

// removeLegacyCASLedger deletes the ledger an older compiler left in bin/, where
// it lived before T2211 moved it to the scratch dir above.
//
// It is called from the build because bin/ is gitignored — no commit can remove
// a file there — and because the build is the only writer permitted to touch
// that directory at all, which is the whole point of the item. It deletes ONE
// exact name and never sweeps: bin/ also holds the build's own sidecars, and a
// cleanup that guessed at a pattern would eat them.
//
// Delete this once no worktree in use predates T2211; a worktree is cured by a
// single build, so that is soon, and nothing else depends on it.
func removeLegacyCASLedger(binDir string) {
	os.Remove(filepath.Join(binDir, casLedgerName))
}

// casEvent is one line of the ledger. Keep in lockstep with casmetrics.event.
type casEvent struct {
	Event string `json:"event"`
	Name  string `json:"name,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`
	Count int    `json:"count,omitempty"`
	Path  string `json:"path,omitempty"`
}

// casLedgerFold is the fold of every event in the file.
type casLedgerFold struct {
	NetworkBytes      int64
	MaterializedBytes int64
	Materializations  int
	Homes             []string
	Names             []string
}

// readCASLedger folds the ledger. An absent one folds to zero: nothing has cost
// the store anything, which is the reading a warm run is supposed to produce.
func readCASLedger(root string) (casLedgerFold, error) {
	l := casLedgerFold{}
	data, err := os.ReadFile(casLedgerPath(root))
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return l, err
	}
	homes := map[string]bool{}
	names := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e casEvent
		// A torn trailing line from a killed writer is skipped rather than
		// failing the read: the rest of the run's accounting still holds.
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		switch e.Event {
		case "network":
			l.NetworkBytes += e.Bytes
		case "materialize":
			l.MaterializedBytes += e.Bytes
			l.Materializations += e.Count
			if e.Name != "" {
				names[e.Name] = true
			}
		case "home":
			if e.Path != "" {
				homes[e.Path] = true
			}
		}
	}
	l.Homes = sortedSetKeys(homes)
	l.Names = sortedSetKeys(names)
	return l, nil
}

// resetCASLedger opens a fresh measurement window by emptying the ledger.
// Failing here is what makes the metrics unavailable rather than zero: a
// directory that cannot hold a ledger cannot hold the run's accounting either.
func resetCASLedger(root string) error {
	path := casLedgerPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0o644)
}

// casWindow is an opened measurement window.
type casWindow struct {
	root string
	open bool
	why  string // why it could not be opened, when open is false
}

// openCASWindow warms the toolchain and then empties the ledger, so what the
// gate goes on to measure is what the measured phase itself cost.
//
// The warm-up is deliberate and is what makes cas_network_bytes judgeable at
// exactly zero: a fresh clone has to stage a toolchain once, and charging the
// first run for it would make the metric describe the machine. Anything fetched
// or exploded AFTER it is work the tree asked for a second time.
func openCASWindow(root string) casWindow {
	ensureToolchainWarm(root)
	if err := resetCASLedger(root); err != nil {
		return casWindow{root: root, why: "the store ledger could not be opened (" + firstRealLine(err.Error()) + "), so the store's cost was not measured"}
	}
	return casWindow{root: root, open: true}
}

// isContractStoreMetric reports whether a name is one of the two the window
// adds to a contract envelope, for the one caller that has an envelope and
// needs to tell them from the gate's own metrics (verify's gate values).
//
// Spelled here rather than shared with Metrics through a constant: a metric
// name exists by being written into a Count/Size call, which is the only place
// the drift scanner in gate_contract_test.go can see one. So this is a second
// spelling on purpose, and TestContractStoreMetricPredicateMatchesMetrics is
// what keeps it honest — the same bargain casLedgerName already makes with
// casmetrics.LedgerName.
func isContractStoreMetric(name string) bool {
	return name == "cas_network_bytes" || name == "cas_home_count"
}

// Values folds the window into the gate values a tracker gate reports. The four
// are always present, zeros included: a counter that appears only when non-zero
// cannot be judged against a baseline of zero.
//
// An unopened window contributes nothing at all rather than four zeros, and
// says why — numbers about nothing read exactly like a clean run.
func (w casWindow) Values() (map[string]float64, string) {
	if !w.open {
		return nil, w.why
	}
	l, err := readCASLedger(w.root)
	if err != nil {
		return nil, "the store ledger could not be read (" + firstRealLine(err.Error()) + "), so the store's cost was not measured"
	}
	return map[string]float64{
		"cas_network_bytes":      float64(l.NetworkBytes),
		"cas_materialized_bytes": float64(l.MaterializedBytes),
		"cas_materializations":   float64(l.Materializations),
		"cas_home_count":         float64(len(l.Homes)),
	}, ""
}

// AddTo merges this window's values into a gate's own. Called once, last, so a
// gate's store cost covers everything it did. A window that could not be opened
// contributes no keys and says so on stderr — four zeros would read exactly
// like a clean run.
func (w casWindow) AddTo(values map[string]float64) {
	vals, incomplete := w.Values()
	if vals == nil {
		fmt.Fprintf(os.Stderr, "warning: %s\n", incomplete)
		return
	}
	for k, v := range vals {
		values[k] = v
	}
}

// Metrics folds the window into contract-gate metrics. Only the two whose end
// state is an absolute are reported here — no bytes over the wire, one home per
// run — because docs/gate-system.md requires every metric `integration` reports
// to carry an enforced term on every target, and a byte or population count is
// legitimately non-zero the first time a clone builds. The other two go to the
// tracker gates and to `promise test`, where no term is owed.
func (w casWindow) Metrics() ([]Metric, string) {
	vals, incomplete := w.Values()
	if vals == nil {
		return nil, incomplete
	}
	return []Metric{
		Size("cas_network_bytes", int64(vals["cas_network_bytes"]), "bytes"),
		Count("cas_home_count", int(vals["cas_home_count"])),
	}, ""
}

// AddToEnvelope appends the window's contract metrics to an envelope, carrying
// the reason forward when it could not be opened.
//
// Both paths that produce a judged `integration` envelope call this: the
// `bin/gate` process entry point and bin/verify. It is one function rather than
// the same six lines twice because the two envelopes may not carry different
// metrics — "verify passed" and "integration passed" are one answer about one
// tree rather than two that can differ (docs/gate-system.md), and a metric
// enforced on one path and absent from the other is exactly how they differ:
// the run that adds a private PROMISE_HOME passes verify, is blessed by it, and
// fails `bin/run integration`.
func (w casWindow) AddToEnvelope(env *Envelope) {
	metrics, incomplete := w.Metrics()
	env.Metrics = append(env.Metrics, metrics...)
	env.Incomplete = joinIncomplete(env.Incomplete, incomplete)
}

// ensureToolchainWarm materializes the toolchain into the ambient Promise home
// once, before a measurement window opens, by running the compiler the way a
// test does. Best effort: a warm-up that fails leaves the gate exactly where it
// was, and whatever it could not stage is measured as the cost it really is.
//
// `promise exec` is the honest warm-up — the same path the suites drive, so it
// materializes what they need and nothing else. clitest.warmToolchain does the
// same for the Go suites; the two are separate modules and cannot share it.
func ensureToolchainWarm(root string) {
	bin := filepath.Join(root, "bin", BinaryName())
	if !Exists(bin) {
		return
	}
	// A directory of its own, so the warm-up is never interpreted against
	// whatever project the gate happens to be run from.
	dir, err := os.MkdirTemp("", "promise-gate-warm-")
	if err != nil {
		return
	}
	defer os.RemoveAll(dir)
	cmd := exec.Command(bin, "exec", `print_line("");`)
	cmd.Dir = dir
	_ = cmd.Run()
}

func sortedSetKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Summary is what the window costs, as bin/verify prints it, or "" when the run
// cost the store nothing — the same rule `promise test` follows, and for the
// same reason: a counter that is zero on every warm run must not cost every warm
// run a line of a tail-read.
//
// One line normally. Past one home it adds a continuation line per home, because
// the count alone says a change added a home but not WHICH one, and finding that
// out again means re-running the whole sweep with the ledger open — which is
// what T2150 had to do. The paths are the diagnosis: each extra one is a
// t.TempDir() named after the test that built it. They are lines rather than a
// list because the reading this exists to serve had 29 of them, and 29 paths on
// one line is not a thing anybody reads.
//
// The numbers it JUDGES come from the gates, which measure with -count=1 and so
// report the same figure every time; see RunVerify for why this run's figure
// must not reach a ratchet.
func (w casWindow) Summary() string {
	if !w.open {
		return ""
	}
	l, err := readCASLedger(w.root)
	if err != nil {
		return ""
	}
	if l.NetworkBytes == 0 && l.MaterializedBytes == 0 && l.Materializations == 0 {
		return ""
	}
	summary := fmt.Sprintf("Store cost:   %d materialization(s) over %d home(s), %.1f MB written, %.1f MB fetched",
		l.Materializations, len(l.Homes), megabytes(l.MaterializedBytes), megabytes(l.NetworkBytes))
	if len(l.Names) > 0 {
		summary += " (" + strings.Join(l.Names, ", ") + ")"
	}
	if len(l.Homes) > 1 {
		// Indented to the column "Store cost:" puts its own value in, so the
		// paths line up under the count they explain.
		for _, home := range l.Homes {
			summary += "\n                home: " + home
		}
	}
	return summary
}

func megabytes(n int64) float64 { return float64(n) / (1024 * 1024) }
