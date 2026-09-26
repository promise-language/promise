package common

// The gates this project provides, and the one output each prints.
//
// A gate MEASURES and never judges: it reports numbers and stops. Whether a
// number is acceptable needs thresholds the gate deliberately does not hold —
// a gate carrying its own thresholds can be made to pass by editing the gate,
// and when the subject is a change written by an agent, the agent can edit it.
// The thresholds live in thresholds.json and only bin/run reads them (judge.go).
//
// A gate also never REPAIRS its subject: no formatting, no staging. It does
// bring the build up to date first (gate_build.go), because a measurement of
// artifacts nobody produced is a measurement of a tree nobody proposed — and
// where that build regenerates the tracked parser, the runner's tracked-tree
// diff reports it, correctly, against the change that moved the grammar
// without regenerating.
//
// `--envelope` and `--list` are flags of the bin/gate BINARY, not part of any
// gate's name and not modifiers of one: the runner appends --envelope when it
// asks for a measurement, and --list asks what this project provides.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MetricType is what kind of number a measurement is. The set is closed.
type MetricType string

const (
	// MetricInt counts or sizes things: a whole number of them.
	MetricInt MetricType = "int"
	// MetricFloat measures a quantity that is not whole.
	MetricFloat MetricType = "float"
)

// Metric is one number a gate measured. The type travels with the value, and a
// whole number is held as an integer rather than a float that happens to be
// whole: a metric whose type changed mid-history measured something else.
type Metric struct {
	Name string
	Type MetricType
	// Exactly one of these carries the value, chosen by Type.
	Int   int64
	Float float64
	Unit  string
}

// Count is a measurement of how many. Whole by construction, and unitless.
func Count(name string, n int) Metric {
	return Metric{Name: name, Type: MetricInt, Int: int64(n)}
}

// Size is a measurement of how much, in whole units. Bytes are whole, carry a
// unit, and outrun what an int holds on a 32-bit host.
func Size(name string, n int64, unit string) Metric {
	return Metric{Name: name, Type: MetricInt, Int: n, Unit: unit}
}

// Percent is a proportion, out of a hundred. Held as a float because it is one:
// coverage that moved from 71.4 to 71.9 is a real change, and rounding it to a
// whole number would report the two runs as identical.
func Percent(name string, v float64) Metric {
	return Metric{Name: name, Type: MetricFloat, Float: v, Unit: "%"}
}

// Number is the value as a float, for comparison against a threshold. Widening
// is safe only in the judge, which compares and never stores.
func (m Metric) Number() float64 {
	if m.Type == MetricInt {
		return float64(m.Int)
	}
	return m.Float
}

// String renders the value in its own type.
func (m Metric) String() string {
	if m.Type == MetricInt {
		return strconv.FormatInt(m.Int, 10)
	}
	return strconv.FormatFloat(m.Float, 'f', 1, 64)
}

// metricWire is the envelope form of a Metric: one "value" field, with the type
// beside it so a reader knows which kind of number it is looking at.
type metricWire struct {
	Name  string          `json:"name"`
	Type  MetricType      `json:"type"`
	Value json.RawMessage `json:"value"`
	Unit  string          `json:"unit,omitempty"`
}

func (m Metric) MarshalJSON() ([]byte, error) {
	w := metricWire{Name: m.Name, Type: m.Type, Unit: m.Unit}
	switch m.Type {
	case MetricInt:
		w.Value = json.RawMessage(strconv.FormatInt(m.Int, 10))
	case MetricFloat:
		w.Value = json.RawMessage(strconv.FormatFloat(m.Float, 'f', -1, 64))
	default:
		return nil, fmt.Errorf("metric %q has no type", m.Name)
	}
	return json.Marshal(w)
}

func (m *Metric) UnmarshalJSON(b []byte) error {
	var w metricWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	m.Name, m.Type, m.Unit = w.Name, w.Type, w.Unit
	switch w.Type {
	case MetricInt:
		// A whole number arriving with a fractional part is not one. Absorbed,
		// it would be a type change nothing recorded.
		if err := json.Unmarshal(w.Value, &m.Int); err != nil {
			return fmt.Errorf("metric %q is declared %s but its value is not: %w", w.Name, w.Type, err)
		}
	case MetricFloat:
		if err := json.Unmarshal(w.Value, &m.Float); err != nil {
			return fmt.Errorf("metric %q is declared %s but its value is not: %w", w.Name, w.Type, err)
		}
	default:
		return fmt.Errorf("metric %q has an unknown type %q", w.Name, w.Type)
	}
	return nil
}

// Envelope is what a contract gate prints on stdout: one JSON object, written
// whole, so a run killed part-way leaves output that does not parse — which is
// how the runner tells "measured nothing" from "measured and reported".
// The field names follow BASE's envelope (schema_version, target, metrics,
// incomplete_reason) so a reader of one project's envelope can read another's.
// `gate` is this project's addition: the judge refuses to apply one gate's
// terms to another gate's numbers, and that check needs the name in the object.
type Envelope struct {
	SchemaVersion int    `json:"schema_version"`
	Gate          string `json:"gate"`
	// Target is what these measurements speak for. One run reports one target.
	Target  string   `json:"target"`
	Metrics []Metric `json:"metrics"`
	// Incomplete names why this run measured less than a full one, and is empty
	// when it did not. Completeness is the absence of a reason, so there is no
	// separate flag that could disagree with it. A baseline never moves from an
	// incomplete run (judge.go).
	Incomplete string `json:"incomplete_reason,omitempty"`
	// Tree is the identity of the content these measurements speak for: the git
	// tree id it would stage as (blessing.go). Present only when the identity
	// before and after the measurement agreed, so a tree that moved while it was
	// being measured carries none — and a judge holding this envelope can tell
	// "this is about the content in front of me" from "this is about content
	// that no longer exists". Absent on a gate whose subject is the machine.
	//
	// This project's own field, like `gate`: the judging layer blesses from it,
	// and nothing outside this repository reads it.
	Tree string `json:"tree,omitempty"`
}

// EnvelopeSchemaVersion is the wire version of the envelope above.
const EnvelopeSchemaVersion = 1

// contractGateDef is either a leaf that measures, or a composition of other
// gates. A composition's parts stay separately runnable, which is what lets a
// step fixing one area re-run that area rather than the whole set.
type contractGateDef struct {
	summary string
	measure func(root string) ([]Metric, string, error)
	parts   []string
	// measuresMachine marks the gate whose subject is the HOST rather than the
	// tree, and which therefore does not build first (gate_build.go). The
	// default — build — is the safe one, so a gate added later inherits it
	// rather than silently measuring whatever artifacts happen to be on disk.
	measuresMachine bool
}

// integrationGate is the one gate a landing decision — and a blessing — may
// rest on. Named once because three layers test for it: the composition below,
// bin/verify's test phase, and blessIfPassed, which refuses to bless anything
// else (blessing.go).
const integrationGate = "integration"

// contractGates is CLOSED, and holds only gates. A name absent here is refused
// rather than guessed at, because a runner asking for a gate this project does
// not have must learn that, not receive an empty measurement that reads like a
// clean result.
//
// The names come from the SDK's closed concept vocabulary: a project may leave
// a concept unprovided, but may not invent a spelling for one it has.
var contractGates = map[string]contractGateDef{
	// A concept divides into instances where this project has more than one
	// language: `checked:go` is Go's diagnostics, `checked:promise` is
	// Promise's, and `checked` is both. Each is separately runnable, which is
	// what lets a step fixing one re-measure that one.
	"formatted:go": {
		summary: "Go files gofmt would rewrite",
		measure: measureFormattedGo,
	},
	"formatted:promise": {
		summary: "Promise files `promise format` would rewrite",
		measure: measureFormattedPromise,
	},
	"formatted": {
		summary: "files either formatter would rewrite",
		parts:   []string{"formatted:go", "formatted:promise"},
	},
	"builds": {
		summary: "Go packages that fail to compile",
		measure: measureBuilds,
	},
	"checked:go": {
		summary: "go vet diagnostics",
		measure: measureCheckedGo,
	},
	"checked:promise": {
		summary: "Promise semantic and ownership diagnostics",
		measure: measureCheckedPromise,
	},
	"checked": {
		summary: "diagnostics from every language's checker",
		parts:   []string{"checked:go", "checked:promise"},
	},
	"tested:go": {
		summary: "failing tests in every Go module's suite",
		measure: measureTestedGo,
	},
	"tested:promise": {
		summary: "failing tests in the host Promise suite",
		measure: measureTestedPromise,
	},
	"tested": {
		summary: "failing tests in the host suites",
		parts:   []string{"tested:go", "tested:promise"},
	},
	// integration is what a landing decision rests on: the whole, measured at
	// once, with nothing repaired on the way. Its parts stay separately
	// runnable — a step fixing one failing area re-runs that area, not the set.
	integrationGate: {
		summary: "everything that must hold before a change may land (host)",
		parts:   []string{"formatted", "builds", "checked", "tested"},
	},
	// fit measures the machine, not the code: whether it may be given work at
	// all. Deliberately NOT part of integration — a machine that cannot build
	// is not a change that may not land.
	"fit": {
		summary:         "free space where this project's work writes",
		measure:         measureFit,
		measuresMachine: true,
	},

	// The measurements too slow for the landing path, each separately
	// addressable so a schedule — or a step fixing one of them — can ask for
	// exactly one. Deliberately NOT parts of integration: the wasm suite alone
	// runs longer than every host gate combined.
	"tested:wasm": {
		summary: "failing tests in the wasm32-wasi suite",
		measure: measureTestedWasm,
	},
	"tested:wasm-web": {
		summary: "failing tests in the wasm32-web suite, under Node",
		measure: measureTestedWasmWeb,
	},
	"tested:stress": {
		summary: "tests that do not agree with themselves across repeated runs",
		measure: measureTestedStress,
	},
	"covered": {
		summary: "how much of each language's source the suites reach",
		measure: measureCovered,
	},

	// Outside flow's closed vocabulary, and listed anyway: the SDK skips a name
	// it does not recognise, while the tracker addresses these by name. A gate
	// the flow cannot ask for is still a gate this project has, and a listing
	// that omitted it would describe a machine that does not exist.
	//
	// The instance half is carried even though the concept is ours, because
	// each of these measures ONE of several things it could: the canaries are
	// built for wasm32-wasi and nothing stops a later `size:native` or
	// `size:wasm-web`, and the install is of the thin variant beside a `full`
	// the subcommand already accepts. A bare `size` would claim to measure what
	// this project produces while measuring one target's canaries — and the
	// name would have to change the day the second instance arrives, which is
	// the rename an instance suffix exists to avoid.
	"size:wasm": {
		summary: "what the wasm32-wasi canaries compile to, per canary",
		measure: measureSizeWasm,
	},
	"install:thin": {
		summary:         "installing a published thin release, end to end",
		measure:         measureInstallThin,
		measuresMachine: true,
	},
	"latest-invariant": {
		summary:         "whether `releases/latest` resolves to an epoch-* release",
		measure:         measureLatestInvariant,
		measuresMachine: true,
	},
}

// ContractGateNames returns every gate this project provides, sorted. This is
// exactly what `bin/gate --list` prints and what `bin/run <name>` can run.
func ContractGateNames() []string {
	names := make([]string, 0, len(contractGates))
	for n := range contractGates {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// IsContractGate reports whether name is a gate this project provides.
func IsContractGate(name string) bool { _, ok := contractGates[name]; return ok }

// ContractGateSummary is the one-line description of a gate, for usage text.
func ContractGateSummary(name string) string { return contractGates[name].summary }

// unknownContractGate is the one refusal both bin/gate and bin/run give for a
// name this project does not measure, so a caller that mistyped is told the
// same thing whichever program it typed it at.
func unknownContractGate(name string) error {
	return fmt.Errorf("no gate named %q in this project; gates: %s",
		name, strings.Join(ContractGateNames(), ", "))
}

// PartResult is what ONE leaf gate measured, for a caller rendering a summary
// for a person. It is not wire: the envelope carries numbers, and a reader of it
// cannot tell which part of a composition produced which number, or what each
// part cost. `bin/verify` needs both to print a row per part; `bin/gate` ignores
// it entirely.
//
// A composition flattens to one PartResult per LEAF, in declaration order, so
// every row is named exactly as `bin/run` addresses it (T1871's rule: a name a
// tool prints is a name you can type back at it).
type PartResult struct {
	Gate       string
	Metrics    []Metric
	Incomplete string
	Elapsed    time.Duration
}

// MeasureContractGate runs one gate and returns what it measured. The error
// means the measurement could not be OBTAINED; it never means "the numbers are
// bad" — three failing tests is a successful run of the gate that counts them.
//
// It has no side effects of its own beyond the shared build: what a RUN costs
// the content-addressed store is added by runContractGate, the process entry
// point, because a run is a process and not a function call. Opening that window
// here would mean every caller opened one — including a test measuring a gate
// against this very checkout, which would reset the ledger of whatever verify or
// gate was measuring at the time and silently zero its numbers.
func MeasureContractGate(root, name string) (Envelope, error) {
	env, _, err := MeasureContractGateParts(root, name)
	return env, err
}

// MeasureContractGateParts is MeasureContractGate, plus what each leaf measured
// and what it cost. ONE measurement path, so an in-process caller that wants a
// per-part summary cannot come to measure something other than what the gate
// reports — which is the whole of what T2170 is about.
func MeasureContractGateParts(root, name string) (Envelope, []PartResult, error) {
	def, ok := contractGates[name]
	if !ok {
		return Envelope{}, nil, unknownContractGate(name)
	}
	// Every gate but the machine one measures the tree AS IT IS NOW, so the
	// build comes first — gate_build.go says why, and RunBuild's own quick
	// check makes it cost about 0.1s on an already-built tree. The outcome is
	// not consulted here: each measurement that reads build artifacts asks
	// ensureGateBuild again (it is idempotent) and reports the failure as a
	// number or a reason of its own, rather than having it swallowed here as
	// "could not measure".
	if !def.measuresMachine {
		_ = ensureGateBuild(root)
	}
	if def.measure != nil {
		start := time.Now()
		metrics, incomplete, err := def.measure(root)
		elapsed := time.Since(start)
		if err != nil {
			return Envelope{}, nil, err
		}
		if metrics == nil {
			metrics = []Metric{}
		}
		env := Envelope{
			SchemaVersion: EnvelopeSchemaVersion,
			Gate:          name,
			Target:        HostTarget(),
			Metrics:       metrics,
			Incomplete:    incomplete,
		}
		return env, []PartResult{{
			Gate:       name,
			Metrics:    metrics,
			Incomplete: incomplete,
			Elapsed:    elapsed,
		}}, nil
	}
	// A composition. Each part is measured by the same path a caller asking for
	// that part alone would take, so the whole cannot disagree with its parts
	// about how anything is measured.
	env := Envelope{
		SchemaVersion: EnvelopeSchemaVersion,
		Gate:          name,
		Target:        HostTarget(),
		Metrics:       []Metric{},
	}
	var reasons []string
	var parts []PartResult
	for _, part := range def.parts {
		sub, subParts, err := MeasureContractGateParts(root, part)
		if err != nil {
			return Envelope{}, nil, fmt.Errorf("%s: %w", part, err)
		}
		env.Metrics = append(env.Metrics, sub.Metrics...)
		parts = append(parts, subParts...)
		if sub.Incomplete != "" {
			reasons = append(reasons, part+": "+sub.Incomplete)
		}
	}
	env.Incomplete = strings.Join(reasons, "; ")
	return env, parts, nil
}

// joinIncomplete adds one more reason to a run's incomplete reason. A run may
// measure less than a full one for more than a reason at a time, and a reason
// that replaced another would hide it.
func joinIncomplete(existing, add string) string {
	switch {
	case add == "":
		return existing
	case existing == "":
		return add
	default:
		return existing + "; " + add
	}
}

// parseContractGateArgs reads `<name> [--envelope]`: exactly one name, and
// whether the caller asked for an envelope. An unknown flag is refused rather
// than ignored, and a second name refused rather than dropped — two callers
// asking the same thing must not get different answers and both be right.
func parseContractGateArgs(args []string) (name string, envelope bool, err error) {
	for _, raw := range args {
		flag, isFlag := flagName(raw)
		switch {
		case isFlag && flag == "envelope":
			envelope = true
		case isFlag:
			return "", false, fmt.Errorf("use of unknown flag %q", raw)
		case name != "":
			return "", false, fmt.Errorf("unexpected argument %q; a gate is asked for by name, once", raw)
		default:
			name = raw
		}
	}
	if !IsContractGate(name) {
		return "", false, unknownContractGate(name)
	}
	return name, envelope, nil
}

// runContractGate is `bin/gate <name> [--envelope]`.
//
// Without --envelope it prints nothing on stdout and fails: a bare run that
// printed measurements and exited 0 would be read as a pass by the first script
// that wrapped it. Every error path leaves stdout untouched, so a runner reads
// one envelope or nothing.
func runContractGate(root string, args []string, stdout io.Writer) error {
	name, envelope, err := parseContractGateArgs(args)
	if err != nil {
		return err
	}
	if !envelope {
		return fmt.Errorf("refusing to measure %q without --envelope; run `bin/run %s` for a result meant for a person", name, name)
	}
	// What this RUN costs the content-addressed store, added once, around
	// everything (T2143). It belongs here and not in MeasureContractGate: it is
	// a property of the whole process, a composition's parts all draw on the
	// same store, and the window is a side effect no function call should carry.
	// fit is the exception — it measures the machine, must answer on one that
	// cannot build, and so neither warms a toolchain nor opens a window.
	// A name this project does not have opens no window either: the refusal
	// comes from MeasureContractGate below, and resetting somebody's ledger on
	// the way to an error would be the same side effect on a path that measures
	// nothing at all.
	def, known := contractGates[name]
	measured := known && !def.measuresMachine
	var store casWindow
	var beforeTree string
	if measured {
		_ = ensureGateBuild(root)
		store = openCASWindow(root)
		// The identity of the content this run is about, taken here and again
		// when the measurement is done (settledTree). AFTER the build, because
		// the build is allowed to move the tree — it regenerates the tracked
		// parser when the grammar has moved — and a snapshot from before it
		// would report every such run as a tree that changed under measurement.
		beforeTree, _ = treeIdentity(root)
	}
	env, err := MeasureContractGate(root, name)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if measured {
		store.AddToEnvelope(&env)
		// What the numbers are about. A run whose tree moved carries no
		// identity, so nothing downstream can bless content this did not see.
		tree, moved := settledTree(root, beforeTree)
		env.Tree = tree
		env.Incomplete = joinIncomplete(env.Incomplete, moved)
	}
	out, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("could not encode the %s envelope: %w", name, err)
	}
	// One write, so a run killed part-way leaves output that does not parse.
	_, err = stdout.Write(append(out, '\n'))
	return err
}

// parseListArgs recognises a listing request: `--list` (or `list`/`-list`),
// optionally with `--json`, and nothing else. ok is false when this argv is not
// a listing request at all, so the caller falls through to its own gates.
func parseListArgs(args []string) (list, jsonOut, ok bool) {
	for _, raw := range args {
		flag, isFlag := flagName(raw)
		if !isFlag {
			// The bare word too: `bin/gate list` is what a person types, and a
			// person who guessed right must not be told the gate does not exist.
			flag = raw
		}
		switch flag {
		case "list":
			list = true
		case "json":
			jsonOut = true
		default:
			return false, false, false
		}
	}
	return list, jsonOut, list || jsonOut
}

// flagName splits a leading "-" or "--" off an argument, reporting whether it
// was one. The RAW argument is what every message quotes back: a person who
// typed `--json` and is told `-json` is being corrected in a spelling they did
// not use, and the one-dash form is not even what this accepts everywhere else.
func flagName(raw string) (string, bool) {
	if after, ok := strings.CutPrefix(raw, "--"); ok {
		return after, true
	}
	if after, ok := strings.CutPrefix(raw, "-"); ok {
		return after, true
	}
	return raw, false
}

// writeGateList prints the gates this project provides: one name per line, or
// one JSON object under --json.
//
// An orchestrator must not hold a second copy of this list — a copy goes stale
// silently — so asking the entry point is the only way to learn it. Listing is
// the one mode besides a measurement that writes to stdout, and neither form
// can be mistaken for an envelope by something parsing one.
//
// The JSON form is an ARRAY OF OBJECTS, one per gate, because that is the shape
// flow's discovery reads (its gates-and-commands.md §"Which gates a project
// has"). A bare array of names parses as JSON and still answers nothing: the
// SDK unmarshals into a struct with a `name` field, gets zero gates out of a
// list of strings, and reads the repository as a machine with no gates at all —
// a silent discovery failure rather than a loud parse error. The summary rides
// along for whoever reads the listing; the name is what a caller addresses, and
// a field added here later is ignored by the SDK rather than refused.
func writeGateList(w io.Writer, jsonOut bool) error {
	names := ContractGateNames()
	if !jsonOut {
		for _, n := range names {
			if _, err := fmt.Fprintln(w, n); err != nil {
				return err
			}
		}
		return nil
	}
	type listedGate struct {
		Name    string `json:"name"`
		Summary string `json:"summary"`
	}
	gates := make([]listedGate, 0, len(names))
	for _, n := range names {
		gates = append(gates, listedGate{Name: n, Summary: ContractGateSummary(n)})
	}
	return writeJSONLine(w, struct {
		Gates []listedGate `json:"gates"`
	}{Gates: gates})
}

// HostTarget names the platform a run's measurements speak for, in the same
// spelling the baselines are keyed by.
func HostTarget() string {
	return strings.ToLower(runtime.GOOS) + "-" + runtime.GOARCH
}

// writeJSONLine marshals whole, then writes once: a failure leaves the stream
// untouched rather than half an object.
func writeJSONLine(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(body, '\n'))
	return err
}

// captureSplit runs a child with stdout and stderr captured SEPARATELY.
//
// Separate matters: a gate reading a value (a path, a count) needs stdout
// alone, because `go` writes toolchain notices to stderr and the concatenation
// names nothing. Nothing here writes to the process's own stdout, which carries
// the envelope and nothing else.
func captureSplit(dir, name string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	err = cmd.Run()
	return out.String(), errBuf.String(), err
}

// captureSplitTee is captureSplit with the child's output COPIED to our stderr
// as it arrives, and captured as well.
//
// For the measurements that take minutes. A gate that holds a twenty-minute
// suite's output until it is over is indistinguishable from a wedged one while
// it runs, and when it finally reports `go_test_failures = 3` the three names
// are in a buffer nobody prints — the reader is told a number and sent to find
// the tests themselves. judge.go makes the same argument about a gate's own
// stderr, which is where this goes: OUR stdout carries the envelope and nothing
// else, and docs/gate-system.md puts human-readable progress on stderr.
func captureSplitTee(dir, name string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stderr, &out)
	cmd.Stderr = io.MultiWriter(os.Stderr, &errBuf)
	err = cmd.Run()
	return out.String(), errBuf.String(), err
}

// captureFunc is captureSplit's shape as a parameter: the seam a gate uses when
// a test of it must stand in for a child process rather than spawn one.
type captureFunc func(dir, name string, args ...string) (stdout, stderr string, err error)

// countPrefixed counts lines starting with prefix.
func countPrefixed(s, prefix string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// Parsing go vet output is ParseGoDiagnostics, in check.go, beside the rule for
// which of those diagnostics this project acts on. Counting them is not a
// separate thing from reading them.

// toolchainNotices are the progress lines `go` writes to stderr before it says
// anything about the code.
var toolchainNotices = []string{
	"go: downloading ", "go: finding ", "go: extracting ",
	"go: added ", "go: upgraded ", "go: downgraded ",
}

// isToolchainNotice reports whether line is one of them.
func isToolchainNotice(line string) bool {
	for _, prefix := range toolchainNotices {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// firstRealLine is firstLine, past the toolchain's progress notices.
//
// A failed `go` run routinely leads with a download notice, so the first line
// of its stderr is not what went wrong: "go build in …: exit status 1: go:
// downloading github.com/antlr4-go/antlr/v4" is what "pattern resources/*: no
// matching files found" looked like from the outside, and it sent the reader
// to the network rather than to the missing file (T2102).
//
// When every line is a notice, the notice IS the diagnostic and is returned —
// nothing is ever dropped to nothing.
func firstRealLine(s string) string {
	first := ""
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if first == "" {
			first = line
		}
		if isToolchainNotice(line) {
			continue
		}
		return line
	}
	return first
}
