// Package casmetrics accounts for what a run costs the content-addressed store:
// bytes pulled over the wire into it, bytes written exploding its content (and
// the compiler's own embedded artifacts) into usable on-disk form, and the
// distinct Promise homes those explosions landed in.
//
// WHY THIS EXISTS. Both quantities are side effects of the TREE's shape rather
// than of the work being asked for, and both multiply by however many isolated
// caches a change decides to create. T2133 was exactly that: one warm 375 MB
// LLVM view materialization became three cold ones per test run, nothing
// measured it, and it rode trunk for 18 days before surfacing as a timeout in a
// progress-rendering test that had nothing to do with caching.
//
// WHERE THE LEDGER LIVES, AND WHY IT IS NOT IN PROMISE_HOME. The counters have
// to survive an arbitrary fan-out — a gate spawns `go test`, which spawns N
// compilers; `promise test` spawns M children — and the regression this exists
// to catch is precisely children choosing their own PROMISE_HOME. A ledger
// inside a home therefore cannot see it: three private homes write three
// ledgers nobody reads. The only anchor every one of those processes shares,
// without an environment variable (docs/org/cli-guide.md §2: a tool reads no
// environment variable to decide what it does), is the compiler binary they all
// are — so the ledger sits beside it, next to the bin/.promise.hash sidecar
// bin/build already writes. In a worktree that is bin/.promise-cas.jsonl, which
// is gitignored and therefore outside common.WorktreeHash.
//
// WHO OPENS A WINDOW. Nothing here does. The compiler only ever appends; a
// measurement window is a property of a RUN, so it is the runner that empties
// the ledger before it measures — the gates and bin/verify, from
// tools/build/common, once their build and toolchain warm-up are done, so what
// they go on to measure is what the measured phase cost rather than how warm
// the machine happened to be. `promise test` reports its own cost as a DELTA
// for the same reason: it may itself be running inside somebody's window.
//
// WHY APPEND-ONLY. One O_APPEND write per event is atomic on every platform we
// target (POSIX guarantees the offset update; Go maps O_APPEND to Windows'
// FILE_APPEND_DATA), so concurrent compilers need no lock and a crashed writer
// can lose at most its own trailing line. Nothing is written on the warm path:
// a cache hit appends nothing, which is what makes a zero reading mean "this
// run cost the store nothing" rather than "nobody looked".
//
// That is also what keeps the file small enough to fold on every read: a home
// already named is not named again, so a suite of ten thousand compiles against
// one warm home leaves a single line, and a runner empties it whenever it opens
// a window. A ledger that does grow is itself the finding — it means the tree is
// materializing per compile.
package casmetrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// LedgerName is the ledger's file name. It sits in the directory holding the
// running compiler binary; the same rule is spelled in tools/build/common
// (casLedgerPath) because the tools are a separate Go module, and both
// spellings are pinned by golden tests.
const LedgerName = ".promise-cas.jsonl"

// Event kinds. The spelling is part of the on-disk format both readers parse.
const (
	eventNetwork     = "network"
	eventMaterialize = "materialize"
	eventHome        = "home"
)

// event is one line of the ledger. Each field is omitted when zero so a line
// stays readable to a person opening the file to see what a run did.
type event struct {
	Event string `json:"event"`
	Name  string `json:"name,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`
	Count int    `json:"count,omitempty"`
	Path  string `json:"path,omitempty"`
}

// Ledger is the fold of every event in the file.
type Ledger struct {
	// NetworkBytes is what came off the wire, counted as transferred rather
	// than as the manifest says it should be — a cache hit must read zero.
	NetworkBytes int64
	// MaterializedBytes is what was written making delivered content usable.
	// A symlink or a hardlink costs none of it, which is why Linux reads near
	// zero where macOS copies a whole toolchain.
	MaterializedBytes int64
	// Materializations is how many distinct view/tree populations occurred.
	Materializations int
	// Homes are the distinct PROMISE_HOME values whose toolchain surface was
	// touched, sorted. This is the count that does not move with how warm the
	// host happened to be, so it is the one a gate can enforce.
	Homes []string
	// Names are the distinct things that were materialized, sorted, so a
	// non-zero reading names its own cause.
	Names []string
	// Available is false when the ledger cannot be written here (a read-only
	// install directory). A reader must say "not measured" rather than report
	// these zeros, which would be indistinguishable from a clean run.
	Available bool
}

// ledger is the accounting file at one path. Every operation below is a method
// on it, and the package-level functions are the façade that binds it to the
// running binary — so a test drives a ledger of its own instead of swapping a
// package variable every parallel test in the package would race.
type ledger struct{ path string }

// at returns the ledger in a directory.
func at(dir string) ledger { return ledger{path: filepath.Join(dir, LedgerName)} }

// beside returns the ledger next to the running binary, and whether the binary
// could be located at all (there is nothing to anchor one to otherwise).
func beside() (ledger, bool) {
	exe, err := os.Executable()
	if err != nil {
		return ledger{}, false
	}
	return at(filepath.Dir(exe)), true
}

// append writes one event. Best effort throughout: accounting must never be
// able to fail the work it is accounting for, so a read-only directory or a
// full disk silently yields an unavailable ledger rather than an error the
// caller has to handle at every materialization site.
func (l ledger) append(e event) {
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	// One Write, so one write(2): that is what makes the append atomic against
	// peer processes. Hence building the whole line before opening the file.
	_, _ = f.Write(append(line, '\n'))
	f.Close()
}

// read folds the ledger. An absent one reads as an available zero — nothing has
// cost the store anything yet — while a directory that cannot hold one reads as
// unavailable.
func (l ledger) read() Ledger {
	out := Ledger{}
	// Creating the file when absent both proves the directory is writable and
	// leaves the anchor a writer will append to. An empty ledger is a valid,
	// meaningful state, so this is not a side effect that changes any reading.
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return out
	}
	f.Close()
	out.Available = true

	data, err := os.ReadFile(l.path)
	if err != nil {
		return out
	}
	homes := map[string]bool{}
	names := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e event
		// A torn trailing line from a killed writer is skipped, not fatal: the
		// rest of the run's accounting is still worth having.
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		switch e.Event {
		case eventNetwork:
			out.NetworkBytes += e.Bytes
		case eventMaterialize:
			out.MaterializedBytes += e.Bytes
			out.Materializations += e.Count
			if e.Name != "" {
				names[e.Name] = true
			}
		case eventHome:
			if e.Path != "" {
				homes[e.Path] = true
			}
		}
	}
	out.Homes = sortedKeys(homes)
	out.Names = sortedKeys(names)
	return out
}

// registerHome appends a home unless the ledger already names it. Two processes
// racing to add the same one write two lines that read() folds to one, so the
// check is an economy rather than a correctness requirement.
func (l ledger) registerHome(home string) {
	for _, h := range l.read().Homes {
		if h == home {
			return
		}
	}
	l.append(event{Event: eventHome, Path: home})
}

// AddNetwork records bytes pulled over the wire into the store. Called once per
// completed transfer — including a failed one, whose bytes were spent all the
// same — never per read, so nothing touches the ledger mid-download.
func AddNetwork(n int64) {
	if n <= 0 {
		return
	}
	if l, ok := beside(); ok {
		l.append(event{Event: eventNetwork, Bytes: n})
	}
}

// AddMaterialized records one population of a named view or tree and the bytes
// it cost. bytes may legitimately be zero (a symlinked or hardlinked view
// writes none), and populations may be zero for a site that wrote nothing this
// time.
func AddMaterialized(name string, bytes int64, populations int) {
	if bytes <= 0 && populations <= 0 {
		return
	}
	if l, ok := beside(); ok {
		l.append(event{Event: eventMaterialize, Name: name, Bytes: bytes, Count: populations})
	}
}

// registeredHomes remembers what this process has already recorded, so the
// several toolchain lookups one compile makes cost a single read.
var (
	homeMu        sync.Mutex
	registeredSet = map[string]bool{}
)

// RegisterHome records that this process touched the toolchain surface from a
// given Promise home. Idempotent per process and per ledger.
//
// A command that never reaches the toolchain — `promise format`, `promise
// check` — never calls this, which is correct: a home that did no toolchain
// work is not one of the homes this counts.
func RegisterHome(home string) {
	if home == "" {
		return
	}
	homeMu.Lock()
	seen := registeredSet[home]
	registeredSet[home] = true
	homeMu.Unlock()
	if seen {
		return
	}
	if l, ok := beside(); ok {
		l.registerHome(home)
	}
}

// Read folds the ledger beside the running binary.
func Read() Ledger {
	if l, ok := beside(); ok {
		return l.read()
	}
	return Ledger{}
}

func sortedKeys(m map[string]bool) []string {
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
