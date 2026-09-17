package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/promise-language/promise/compiler/internal/casmetrics"
)

// What this run cost the content-addressed store, reported by the run itself
// (T2143) — so a developer or an agent sees a cold materialization without
// having to run a gate, and so the number that tripled at T2133 is on screen at
// the commit that trips it rather than eighteen days later.
//
// The counters are NOT propagated from child to parent over the output stream.
// Every `promise` process in a run is the same binary, so they all append to
// the one ledger beside it (casmetrics), and the parent reading that ledger IS
// the aggregation — including for children that chose a different PROMISE_HOME,
// which is exactly the case a per-home tally cannot see.

// storeWindow is what the store had already cost when this run began. A run
// reports its own delta against it rather than the running total, and it is
// passed to the runners rather than kept in a package var so the window a
// number describes is visible at the call site.
type storeWindow struct{ before casmetrics.Ledger }

// openStoreWindow snapshots the ledger. Cheap: one small read, or none at all
// when nothing has ever written one.
//
// A child of the multi-file runner opens nothing and reads nothing: its parent's
// window already covers everything it does, and a run of eight hundred files
// would otherwise pay eight hundred reads to produce eight hundred deltas
// nobody looks at.
func openStoreWindow() storeWindow {
	if runningUnderTestParent() {
		return storeWindow{}
	}
	return storeWindow{before: casmetrics.Read()}
}

// storeCost is what happened inside a window.
type storeCost struct {
	NetworkBytes      int64    `json:"network_bytes"`
	MaterializedBytes int64    `json:"materialized_bytes"`
	Materializations  int      `json:"materializations"`
	Names             []string `json:"names,omitempty"`
	// Available is false when the ledger cannot be written where this binary
	// lives (a read-only install). The zeros then mean "not measured", not "cost
	// nothing", and a reader must not confuse the two.
	Available bool `json:"available"`
}

// cost closes the window and returns the delta.
func (w storeWindow) cost() storeCost { return w.costAgainst(casmetrics.Read()) }

// costAgainst is cost with the closing reading supplied, so what a window
// reports can be pinned without a ledger to drive it.
//
// A counter that went DOWN means the ledger was emptied under this run — a gate
// or a bin/verify opening its own window in the same worktree while this was
// measuring. The delta is then meaningless, and reporting it as a negative (or
// clamping it to a tidy zero) would both be inventions: the run is reported as
// unmeasured, which is the one honest answer.
func (w storeWindow) costAgainst(after casmetrics.Ledger) storeCost {
	c := storeCost{
		NetworkBytes:      after.NetworkBytes - w.before.NetworkBytes,
		MaterializedBytes: after.MaterializedBytes - w.before.MaterializedBytes,
		Materializations:  after.Materializations - w.before.Materializations,
		Names:             newNames(w.before.Names, after.Names),
		Available:         after.Available,
	}
	if c.NetworkBytes < 0 || c.MaterializedBytes < 0 || c.Materializations < 0 {
		return storeCost{Available: false}
	}
	return c
}

// newNames returns what appeared in after that was not already in before, so a
// report names the causes of THIS run rather than every view the home holds.
func newNames(before, after []string) []string {
	had := make(map[string]bool, len(before))
	for _, n := range before {
		had[n] = true
	}
	var out []string
	for _, n := range after {
		if !had[n] {
			out = append(out, n)
		}
	}
	return out
}

// quiet reports whether there is nothing worth a line of a human's tail.
func (c storeCost) quiet() bool {
	return !c.Available || (c.NetworkBytes == 0 && c.MaterializedBytes == 0 && c.Materializations == 0)
}

// line is the one human line, or "" when the run cost the store nothing.
//
// Suppressed at zero deliberately: the test output format is read by tailing it
// (CLAUDE.md), and a counter that is zero on every warm run would cost every
// warm run a line to say so. The --json stream carries the zeros for anything
// that needs them, and a gate reads the ledger itself.
func (c storeCost) line() string {
	if c.quiet() {
		return ""
	}
	parts := []string{fmt.Sprintf("%d %s", c.Materializations, plural(c.Materializations, "materialization"))}
	if len(c.Names) > 0 {
		parts[0] += " (" + strings.Join(c.Names, ", ") + ")"
	}
	parts = append(parts,
		formatSize(c.MaterializedBytes)+" written",
		formatSize(c.NetworkBytes)+" fetched")
	return "store: " + strings.Join(parts, ", ")
}

// casRecord is the store-cost line of the --json stream. Like the coverage
// record it carries a "kind" and no test identity, so a reader keying records
// on (file, test) skips it and one stream serves both — and unlike the human
// line it is emitted ALWAYS, zeros included, because a metric that appears only
// when non-zero cannot be judged against a baseline of zero.
type casRecord struct {
	Kind string `json:"kind"` // always "cas"
	storeCost
}

func writeCASRecord(w io.Writer, c storeCost) {
	data, err := json.Marshal(casRecord{Kind: "cas", storeCost: c})
	if err != nil {
		return
	}
	fmt.Fprintln(w, string(data))
}

// recordMaterializedIfWritten records ONE population of a named tree, and only
// when something was actually written.
//
// It is the shape of the sites that write file by file rather than publishing a
// tree in one move: there, "there was nothing to write" and "a population that
// cost no bytes" are different facts, and only the second is a materialization.
// A tree published in one move says `AddMaterialized(name, n, 1)` directly —
// a Linux view of symlinks legitimately costs zero bytes and is still one.
func recordMaterializedIfWritten(name string, written int64) {
	if written <= 0 {
		return
	}
	casmetrics.AddMaterialized(name, written, 1)
}

// printStoreCost closes a window and prints its one human line, if it has one.
// A child of the multi-file runner prints nothing: its parent's window already
// covers it, and a line here would land in the output that parent parses.
func printStoreCost(w storeWindow) { printStoreCostAgainst(w, casmetrics.Read()) }

// printStoreCostAgainst is printStoreCost with the closing reading supplied, so
// what reaches the summary block can be pinned without a ledger to drive it.
func printStoreCostAgainst(w storeWindow, after casmetrics.Ledger) {
	if runningUnderTestParent() {
		return
	}
	if line := w.costAgainst(after).line(); line != "" {
		progress.Println(line)
	}
}
