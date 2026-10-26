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
// asks for a measurement, and --list asks what this project provides. RunGate
// (gate.go) holds the command line; this file holds what it reaches.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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

// The units a metric may carry. A count carries none.
const (
	unitBytes   = "bytes"
	unitPercent = "percent"
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
// whole number would report the two runs as identical. The unit is a word, not
// "%", so a reader keying on it compares names rather than punctuation.
func Percent(name string, v float64) Metric {
	return Metric{Name: name, Type: MetricFloat, Float: v, Unit: unitPercent}
}

// sharedCount reports one count twice: under the name every language's gate
// shares, and under this toolchain's own. A composition sums the shared name
// across its parts — `tested` reports one failed_tests for Go and Promise
// together — while each language's figure stays readable by itself.
func sharedCount(toolchain, name string, n int) []Metric {
	return []Metric{Count(name, n), Count(toolchain+"_"+name, n)}
}

// sharedPercent is sharedCount for a proportion. Proportions are never summed,
// so its shared name must come from exactly one part of any composition.
func sharedPercent(toolchain, name string, v float64) []Metric {
	return []Metric{Percent(name, v), Percent(toolchain+"_"+name, v)}
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

// MetricSpec is one metric a gate DECLARES: its name and what kind of number it
// is, without a value. The listing carries a gate's declarations, so a reader
// can write terms for a gate it has never run; and every leaf's output is held
// to them (checkAgainstDeclaration), so the listing cannot come to describe a
// gate other than the one that runs.
type MetricSpec struct {
	Name string     `json:"name"`
	Type MetricType `json:"type"`
	Unit string     `json:"unit,omitempty"`
}

// specOf is what a measured metric would have had to be declared as.
func specOf(m Metric) MetricSpec { return MetricSpec{Name: m.Name, Type: m.Type, Unit: m.Unit} }

// describe renders a spec's kind for a message: "int", "int in bytes".
func (s MetricSpec) describe() string {
	if s.Unit == "" {
		return string(s.Type)
	}
	return string(s.Type) + " in " + s.Unit
}

// The declaration constructors mirror the metric constructors, so a gate's
// declaration reads like the calls that emit what it declares.
func countSpecs(names ...string) []MetricSpec { return specs(MetricInt, "", names) }
func bytesSpecs(names ...string) []MetricSpec { return specs(MetricInt, unitBytes, names) }

// sharedCountSpecs declares what sharedCount emits, for each name.
func sharedCountSpecs(toolchain string, names ...string) []MetricSpec {
	var out []MetricSpec
	for _, n := range names {
		out = append(out, countSpecs(n, toolchain+"_"+n)...)
	}
	return out
}

// sharedPercentSpecs declares what sharedPercent emits, for each name.
func sharedPercentSpecs(toolchain string, names ...string) []MetricSpec {
	var out []MetricSpec
	for _, n := range names {
		out = append(out, specs(MetricFloat, unitPercent, []string{n, toolchain + "_" + n})...)
	}
	return out
}

func specs(t MetricType, unit string, names []string) []MetricSpec {
	out := make([]MetricSpec, 0, len(names))
	for _, n := range names {
		out = append(out, MetricSpec{Name: n, Type: t, Unit: unit})
	}
	return out
}

// Group is evidence under one name — a test file, a Go package — beside the
// numbers: the metrics that belong to it, and the members it holds. The
// metrics say how many; the members say which, so a reader told "3 failing"
// is handed the three rather than sent to find them.
type Group struct {
	Name    string   `json:"name"`
	Metrics []Metric `json:"metrics,omitempty"`
	Members []Member `json:"members,omitempty"`
}

// Member is one thing a group holds — a test — and what became of it.
type Member struct {
	Name    string  `json:"name"`
	Outcome string  `json:"outcome"`
	Detail  string  `json:"detail,omitempty"`
	Elapsed float64 `json:"elapsed,omitempty"`
}

// The outcomes a member may have. The set is closed, and is the runner's own
// record vocabulary (`promise test --json`), so a record maps onto a member
// without a translation that could drift.
const (
	OutcomePass     = "pass"
	OutcomeFail     = "fail"
	OutcomeTimeout  = "timeout"
	OutcomeLeak     = "leak"
	OutcomeMemory   = "memory"
	OutcomeExcluded = "excluded"
	OutcomeNotRun   = "not-run"
)

// memberOutcomes is every outcome above, for the check that holds a leaf's
// members to the vocabulary.
var memberOutcomes = []string{
	OutcomePass, OutcomeFail, OutcomeTimeout, OutcomeLeak, OutcomeMemory, OutcomeExcluded, OutcomeNotRun,
}

// validOutcome reports whether o is one of memberOutcomes.
func validOutcome(o string) bool { return slices.Contains(memberOutcomes, o) }

// newMember is a member with its detail bounded: one test's output, a whole IR
// dump included, cannot swell the envelope past what a runner drains
// (clampContext). An empty detail is left out of the wire.
func newMember(name, outcome, detail string) Member {
	return Member{Name: name, Outcome: outcome, Detail: clampContext(strings.TrimSpace(detail))}
}

// relToRoot names a path under root as a person addresses it from there, and
// leaves any other path as it is.
func relToRoot(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return path
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
	// Groups is the evidence behind the metrics, when a gate has any. Nothing
	// judges it: the verdict rests on the metrics alone.
	Groups []Group `json:"groups,omitempty"`
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

// measurement is what one leaf gate measured: its metrics, the evidence behind
// them, and why it measured less than a full run when it did.
type measurement struct {
	metrics    []Metric
	groups     []Group
	incomplete string
}

// contractGateDef is either a leaf that measures, or a composition of other
// gates. A composition's parts stay separately runnable, which is what lets a
// step fixing one area re-run that area rather than the whole set.
type contractGateDef struct {
	summary string
	measure func(root string) (measurement, error)
	parts   []string
	// metrics is what a leaf declares it reports. A composition declares
	// nothing of its own: it reports what its parts do (declaredMetrics).
	metrics []MetricSpec
	// treeMetrics is the part of a leaf's declaration that depends on what the
	// tree holds — size:wasm reports one metric per canary file — and so is read
	// from the tree rather than spelled. Nil for every other gate.
	treeMetrics func(root string) []MetricSpec
	// remedy is what a person does about a number this LEAF reports: one
	// sentence naming the command to run, and the one that re-measures. A
	// composition has none of its own; a metric's remedy is that of the leaves
	// declaring it.
	remedy string
	// serialize is the exclusion the gate must hold while it measures: "host"
	// for one whose numbers depend on what else the machine is doing, "" for
	// none. The listing declares it, so an orchestrator takes it before spawning
	// the gate; runContractGate takes it too, so a hand-run gate queues with them.
	serialize string
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

// serializeHost is the host-scope exclusion: one holder on the machine at a
// time, the one bin/verify holds (acquireHostScope).
const serializeHost = "host"

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
		metrics: sharedCountSpecs("go", "unformatted_files"),
		remedy:  "run `bin/format` (gofmt -w); `bin/run formatted:go` re-measures",
	},
	"formatted:promise": {
		summary: "Promise files `promise format` would rewrite",
		measure: measureFormattedPromise,
		metrics: sharedCountSpecs("promise", "unformatted_files"),
		remedy:  "run `bin/format` (`promise format`); `bin/run formatted:promise` re-measures",
	},
	"formatted": {
		summary: "files either formatter would rewrite",
		parts:   []string{"formatted:go", "formatted:promise"},
	},
	"builds": {
		summary: "Go packages that fail to compile",
		measure: measureBuilds,
		metrics: append(countSpecs("build_failures"), sharedCountSpecs("go", "unbuildable_packages")...),
		remedy:  "fix what each member names, rebuilding the compiler to confirm; `bin/run builds` re-measures",
	},
	"checked:go": {
		summary: "go vet diagnostics",
		measure: measureCheckedGo,
		metrics: sharedCountSpecs("go", "check_findings"),
		remedy:  "fix each finding `bin/check` reports (go vet); `bin/run checked:go` re-measures",
	},
	"checked:promise": {
		summary: "Promise semantic and ownership diagnostics",
		measure: measureCheckedPromise,
		metrics: append(sharedCountSpecs("promise", "check_findings"),
			countSpecs("promise_check_failures", "promise_check_warnings", "promise_check_units")...),
		remedy: "fix each diagnostic `bin/promise check` reports; `bin/run checked:promise` re-measures",
	},
	"checked": {
		summary: "diagnostics from every language's checker",
		parts:   []string{"checked:go", "checked:promise"},
	},
	// The suites hold the host scope: a suite run beside another verify or
	// runner on the same machine reports that machine's load as timeouts.
	"tested:go": {
		summary:   "failing tests in every Go module's suite",
		measure:   measureTestedGo,
		metrics:   sharedCountSpecs("go", "failed_tests", "failed_packages", "test_count"),
		remedy:    "fix the failing tests, re-running them with `bin/test go`; `bin/run tested:go` re-measures",
		serialize: serializeHost,
	},
	"tested:promise": {
		summary:   "failing tests in the host Promise suite",
		measure:   measureTestedPromise,
		metrics:   suiteSummarySpecs("promise"),
		remedy:    "fix the failing tests, re-running them with `bin/test promise`; `bin/run tested:promise` re-measures",
		serialize: serializeHost,
	},
	"tested": {
		summary:   "failing tests in the host suites",
		parts:     []string{"tested:go", "tested:promise"},
		serialize: serializeHost,
	},
	// integration is what a landing decision rests on: the whole, measured at
	// once, with nothing repaired on the way. Its parts stay separately
	// runnable — a step fixing one failing area re-runs that area, not the set.
	integrationGate: {
		summary:   "everything that must hold before a change may land (host)",
		parts:     []string{"formatted", "builds", "checked", "tested"},
		serialize: serializeHost,
	},
	// fit measures the machine, not the code: whether it may be given work at
	// all. Deliberately NOT part of integration — a machine that cannot build
	// is not a change that may not land — and it never holds the host scope,
	// because it must answer on a machine whose holder is wedged.
	"fit": {
		summary:         "free space where this project's work writes, and whether its filesystem lets a removed file's name go",
		measure:         measureFit,
		metrics:         fitMetricSpecs(),
		remedy:          "free space on the filesystem each group names (`bin/clean` empties this worktree's caches); `bin/run fit` re-measures",
		measuresMachine: true,
	},

	// The measurements too slow for the landing path, each separately
	// addressable so a schedule — or a step fixing one of them — can ask for
	// exactly one. Deliberately NOT parts of integration: the wasm suite alone
	// runs longer than every host gate combined.
	"tested:wasm": {
		summary:   "failing tests in the wasm32-wasi suite",
		measure:   measureTestedWasm,
		metrics:   suiteSummarySpecs(wasmSuite.prefix),
		remedy:    "fix the failing tests, re-running them with `bin/test promise --wasm`; `bin/run tested:wasm` re-measures",
		serialize: serializeHost,
	},
	"tested:wasm-web": {
		summary:   "failing tests in the wasm32-web suite, under Node",
		measure:   measureTestedWasmWeb,
		metrics:   suiteSummarySpecs(wasmWebSuite.prefix),
		remedy:    "fix the failing tests, re-running them with `bin/test promise --wasm-web`; `bin/run tested:wasm-web` re-measures",
		serialize: serializeHost,
	},
	"tested:stress": {
		summary:   "tests that do not agree with themselves across repeated runs",
		measure:   measureTestedStress,
		metrics:   countSpecs("stress_flaky_count", "stress_iterations"),
		remedy:    "make each flaky test agree with itself, repeating it with `bin/stress`; `bin/run tested:stress` re-measures",
		serialize: serializeHost,
	},
	"covered": {
		summary: "how much of each language's source the suites reach",
		measure: measureCovered,
		metrics: append(sharedPercentSpecs("go", "statement_coverage"),
			sharedPercentSpecs("promise", "block_coverage")...),
		remedy:    "test what `bin/coverage` reports unreached; `bin/run covered` re-measures",
		serialize: serializeHost,
	},

	// Outside flow's closed vocabulary, and listed anyway: the SDK skips a name
	// it does not recognise, and a listing that omitted these would describe a
	// machine that does not exist.
	//
	// The instance half is carried even though the concept is ours, because
	// each of these measures ONE of several things it could: the canaries are
	// built for wasm32-wasi and nothing stops a later `size:native` or
	// `size:wasm-web`, and the install is of the thin variant beside a `full`
	// one. A bare `size` would claim to measure what this project produces
	// while measuring one target's canaries — and the name would have to change
	// the day the second instance arrives, which is the rename an instance
	// suffix exists to avoid.
	"size:wasm": {
		summary:     "what the wasm32-wasi canaries compile to, per canary",
		measure:     measureSizeWasm,
		treeMetrics: sizeWasmMetricSpecs,
		remedy:      "shrink what the grown canary pulls in, rebuilding it with `bin/promise build -target wasm32-wasi -release`; `bin/run size:wasm` re-measures",
	},
	"install:thin": {
		summary:         "installing a published thin release, end to end",
		measure:         measureInstall("thin"),
		metrics:         installMetricSpecs("thin"),
		remedy:          "fix the release the failing phase names and re-publish it with `bin/release`; `bin/run install:thin` re-measures",
		measuresMachine: true,
	},
	// The full variant also proves the installed toolchain compiles offline.
	"install:full": {
		summary:         "installing a published full release, end to end, and compiling with the network down",
		measure:         measureInstall("full"),
		metrics:         installMetricSpecs("full"),
		remedy:          "fix the release the failing phase names and re-publish it with `bin/release`; `bin/run install:full` re-measures",
		measuresMachine: true,
	},
	"latest-invariant": {
		summary:         "whether `releases/latest` resolves to an epoch-* release",
		measure:         measureLatestInvariant,
		metrics:         countSpecs("latest_is_epoch"),
		remedy:          "demote the release `latest` resolves to with `gh release edit <tag> --prerelease`; `bin/run latest-invariant` re-measures",
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

// gateSerialize is the exclusion a gate declares it must hold while it
// measures: "host", or "" for none.
func gateSerialize(name string) string { return contractGates[name].serialize }

// leafMetrics is what a LEAF itself declares: its spelled list, then whatever
// it reads from the tree. Nil for a composition, which declares nothing of its
// own. root is the tree a tree-dependent declaration is read from.
func leafMetrics(root, name string) []MetricSpec {
	def := contractGates[name]
	out := slices.Clone(def.metrics)
	if def.treeMetrics != nil {
		out = append(out, def.treeMetrics(root)...)
	}
	return out
}

// declaredMetrics is every metric gate name's envelope carries: a leaf's own
// declaration; for a composition, the union over its parts in part order, a
// name more than one part reports listed once — the composition sums it
// (MeasureContractGateParts). A gate whose subject is the tree also carries
// what the run cost the store, which runContractGate adds around the whole
// measurement (storeMetricSpecs). Empty, never nil, for a name that is not a
// gate.
func declaredMetrics(root, name string) []MetricSpec {
	def, ok := contractGates[name]
	if !ok {
		return []MetricSpec{}
	}
	out := ownMetrics(root, name)
	if !def.measuresMachine {
		out = appendNewSpecs(out, storeMetricSpecs)
	}
	return out
}

// ownMetrics is declaredMetrics without the store's two.
func ownMetrics(root, name string) []MetricSpec {
	def := contractGates[name]
	if def.measure != nil {
		return leafMetrics(root, name)
	}
	out := []MetricSpec{}
	for _, part := range def.parts {
		out = appendNewSpecs(out, ownMetrics(root, part))
	}
	return out
}

// appendNewSpecs appends each of add whose name into does not already hold.
func appendNewSpecs(into, add []MetricSpec) []MetricSpec {
	for _, s := range add {
		if !slices.ContainsFunc(into, func(have MetricSpec) bool { return have.Name == s.Name }) {
			into = append(into, s)
		}
	}
	return into
}

// unknownContractGate is the one refusal both bin/gate and bin/run give for a
// name this project does not measure, so a caller that mistyped is told the
// same thing whichever program it typed it at. A usage error — the command line
// named nothing that exists — and it points at the listing rather than copying
// it, which would bury the one fact the caller needs.
func unknownContractGate(name string) error {
	return &UsageError{Tool: "gate", Problems: []string{
		fmt.Sprintf("no gate named %q in this project; `gate --list` names them", name),
	}}
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
	// Every gate but the machine ones measures the tree AS IT IS NOW, so the
	// build comes first — gate_build.go says why, and RunBuild's own quick
	// check makes it cost about 0.1s on an already-built tree. The outcome is
	// not consulted here: each measurement that reads build artifacts asks
	// ensureGateBuild again (it is idempotent) and reports the failure as a
	// number or a reason of its own, rather than having it swallowed here as
	// "could not measure".
	if !def.measuresMachine {
		_ = ensureGateBuild(root)
	}
	env := Envelope{
		SchemaVersion: EnvelopeSchemaVersion,
		Gate:          name,
		Target:        HostTarget(),
		Metrics:       []Metric{},
	}
	if def.measure != nil {
		start := time.Now()
		m, err := def.measure(root)
		elapsed := time.Since(start)
		if err != nil {
			return Envelope{}, nil, err
		}
		if m, err = checkAgainstDeclaration(root, name, m); err != nil {
			return Envelope{}, nil, err
		}
		if m.metrics != nil {
			env.Metrics = m.metrics
		}
		env.Groups, env.Incomplete = m.groups, m.incomplete
		return env, []PartResult{{
			Gate:       name,
			Metrics:    env.Metrics,
			Incomplete: env.Incomplete,
			Elapsed:    elapsed,
		}}, nil
	}
	// A composition. Each part is measured by the same path a caller asking for
	// that part alone would take, so the whole cannot disagree with its parts
	// about how anything is measured. A name more than one part reports is
	// summed into one: the envelope says each thing once, and `tested` reporting
	// failed_tests twice would leave a reader to add them up.
	var reasons []string
	var parts []PartResult
	for _, part := range def.parts {
		sub, subParts, err := MeasureContractGateParts(root, part)
		if err != nil {
			return Envelope{}, nil, fmt.Errorf("%s: %w", part, err)
		}
		if env.Metrics, err = sumMetrics(env.Metrics, sub.Metrics); err != nil {
			return Envelope{}, nil, fmt.Errorf("%s: %w", part, err)
		}
		env.Groups = append(env.Groups, sub.Groups...)
		parts = append(parts, subParts...)
		if sub.Incomplete != "" {
			reasons = append(reasons, part+": "+sub.Incomplete)
		}
	}
	env.Incomplete = strings.Join(reasons, "; ")
	return env, parts, nil
}

// sumMetrics adds each of add into into: a name into does not hold is
// appended, one it holds is summed. Only whole counts and sizes sum — a
// proportion added to a proportion is not one — and two parts reporting one
// name as different kinds of number are measuring different things under it.
// Either is a definition bug, refused rather than resolved.
func sumMetrics(into, add []Metric) ([]Metric, error) {
	for _, m := range add {
		i := slices.IndexFunc(into, func(have Metric) bool { return have.Name == m.Name })
		if i < 0 {
			into = append(into, m)
			continue
		}
		have := into[i]
		switch {
		case specOf(have) != specOf(m):
			return nil, fmt.Errorf("%q is reported by more than one part, as %s and as %s, so the two cannot be summed",
				m.Name, specOf(have).describe(), specOf(m).describe())
		case m.Type != MetricInt:
			return nil, fmt.Errorf("%q is a %s reported by more than one part; only whole numbers are summed", m.Name, m.Type)
		}
		into[i].Int += m.Int
	}
	return into, nil
}

// checkAgainstDeclaration holds one leaf's output to what it declares.
//
// A metric the leaf does not declare, or reports as another kind of number than
// it declares, is a DEFINITION bug: the listing would describe a gate other than
// the one that ran, and terms written against the listing would judge the wrong
// thing. That is an error, not a number. So is a member outcome outside the
// vocabulary.
//
// A declared metric the leaf did not report, from a run that claims to be
// complete, makes the run incomplete instead: whoever read the listing was
// promised that number, and its absence must not pass for nothing being wrong.
// A run that is already incomplete has said why it measured less.
func checkAgainstDeclaration(root, name string, m measurement) (measurement, error) {
	declared := leafMetrics(root, name)
	reported := map[string]bool{}
	for _, metric := range m.metrics {
		i := slices.IndexFunc(declared, func(s MetricSpec) bool { return s.Name == metric.Name })
		switch {
		case i < 0:
			return measurement{}, fmt.Errorf("reported %q, which this gate does not declare", metric.Name)
		case declared[i] != specOf(metric):
			return measurement{}, fmt.Errorf("reported %q as %s, which this gate declares as %s",
				metric.Name, specOf(metric).describe(), declared[i].describe())
		case reported[metric.Name]:
			return measurement{}, fmt.Errorf("reported %q twice", metric.Name)
		}
		reported[metric.Name] = true
	}
	for _, g := range m.groups {
		for _, member := range g.Members {
			switch {
			case member.Name == "":
				return measurement{}, fmt.Errorf("reported a member of %s with no name", g.Name)
			case !validOutcome(member.Outcome):
				return measurement{}, fmt.Errorf("reported %s in %s with outcome %q, which is not one of %s",
					member.Name, g.Name, member.Outcome, strings.Join(memberOutcomes, ", "))
			case member.Elapsed < 0:
				return measurement{}, fmt.Errorf("reported %s in %s with a negative elapsed time", member.Name, g.Name)
			}
		}
	}
	if m.incomplete == "" {
		var missing []string
		for _, s := range declared {
			if !reported[s.Name] {
				missing = append(missing, s.Name)
			}
		}
		if len(missing) > 0 {
			m.incomplete = "these declared metrics were not reported: " + strings.Join(missing, ", ")
		}
	}
	return m, nil
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

// runContractGate is `bin/gate <name> --envelope`, once RunGate has accepted
// the command line: it measures, and writes the one envelope to stdout.
//
// Stdout carries that envelope and nothing else, so for the whole measurement
// os.Stdout points at the narration instead: the build, a blob fetch, and
// anything else in this process that prints with fmt.Println cannot put a line
// in front of the object, however deep in the call it sits. The envelope is
// written once, whole, to the stream the caller handed in — the real stdout —
// so a run killed part-way leaves output that does not parse, and every error
// path leaves stdout untouched.
func runContractGate(root, name string, stdout io.Writer) error {
	def, known := contractGates[name]
	if !known {
		return unknownContractGate(name)
	}
	saved := os.Stdout
	os.Stdout = Narration()
	defer func() { os.Stdout = saved }()

	// A gate that declares the host scope holds it for the whole measurement.
	// An orchestrator that read the declaration has already taken it for this
	// arena, so this re-enters rather than queues; run by hand, the gate queues
	// with every verify and runner on the machine instead of measuring their
	// load. Taken after the redirect, so the line saying who it waits for is
	// narration too.
	if def.serialize == serializeHost {
		release, err := acquireHostScope(root)
		if err != nil {
			return fmt.Errorf("%s: taking the host-scope exclusion: %w", name, err)
		}
		defer release()
	}

	// What this RUN costs the content-addressed store, added once, around
	// everything (T2143). It belongs here and not in MeasureContractGate: it is
	// a property of the whole process, a composition's parts all draw on the
	// same store, and the window is a side effect no function call should carry.
	// A machine gate is the exception — fit must answer on a machine that
	// cannot build, and so neither warms a toolchain nor opens a window.
	measured := !def.measuresMachine
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
	out, err := encodeEnvelope(env)
	if err != nil {
		return fmt.Errorf("could not encode the %s envelope: %w", name, err)
	}
	// One write, so a run killed part-way leaves output that does not parse.
	_, err = stdout.Write(append(out, '\n'))
	return err
}

// maxEnvelopeBytes bounds what one envelope may weigh. A runner reads a gate's
// stdout whole, and evidence is the one part that grows without limit: a tree
// where everything fails names every test, with its output.
const maxEnvelopeBytes = 1 << 20

// evidenceCut is one step down encodeEnvelope's ladder: each member's detail
// held to detailLines lines and detailBytes bytes (0 drops it), and each group
// to its first members (-1 keeps them all).
type evidenceCut struct{ detailLines, detailBytes, members int }

// evidenceLadder gives up evidence in the order it matters least: the tail of
// each detail first — its head is the assertion or the panic — then the
// details, then the members past the first of each group. The last step keeps
// no member at all, so the ladder always ends within the bound: the metrics
// that are judged are a few dozen numbers.
func evidenceLadder() []evidenceCut {
	return []evidenceCut{{10, 1024, -1}, {3, 256, -1}, {0, 0, -1}, {0, 0, 50}, {0, 0, 5}, {0, 0, 0}}
}

// encodeEnvelope marshals env within maxEnvelopeBytes, going down
// evidenceLadder until it fits. The metrics — what is judged — and the groups'
// own metrics are never touched.
func encodeEnvelope(env Envelope) ([]byte, error) {
	out, err := json.Marshal(env)
	full := env.Groups
	for _, cut := range evidenceLadder() {
		if err != nil || len(out) <= maxEnvelopeBytes {
			break
		}
		env.Groups = trimEvidence(full, cut)
		out, err = json.Marshal(env)
	}
	if err == nil && len(out) > maxEnvelopeBytes {
		return nil, fmt.Errorf("the envelope is %d bytes with no evidence left, past the %d a runner reads", len(out), maxEnvelopeBytes)
	}
	return out, err
}

// trimEvidence is groups cut as cut says. A copy: the measurement the caller
// holds keeps everything.
func trimEvidence(groups []Group, cut evidenceCut) []Group {
	out := make([]Group, len(groups))
	for i, g := range groups {
		members := slices.Clone(g.Members)
		if cut.members >= 0 && len(members) > cut.members {
			members = members[:cut.members]
		}
		for j := range members {
			if cut.detailBytes == 0 {
				members[j].Detail = ""
			} else {
				members[j].Detail = clampText(members[j].Detail, cut.detailLines, cut.detailBytes)
			}
		}
		g.Members = members
		out[i] = g
	}
	return out
}

// listedGate is one row of `bin/gate --list -json`.
type listedGate struct {
	Name    string       `json:"name"`
	Summary string       `json:"summary"`
	Metrics []MetricSpec `json:"metrics"`
	// Requires is what a gate needs in place before it can be asked for. This
	// project declares nothing there yet; the field is present and empty so a
	// reader never has to tell "requires nothing" from "did not say".
	Requires  []string `json:"requires"`
	Serialize string   `json:"serialize,omitempty"`
}

// writeGateList prints the gates this project provides: one name per line for
// a person, or one JSON object.
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
// a silent discovery failure rather than a loud parse error. Each row carries
// what the gate reports (declaredMetrics), so terms can be written against a
// gate nobody has run yet, and the exclusion it holds, so a runner takes it
// before spawning the gate.
func writeGateList(w io.Writer, root string, mode OutputMode) error {
	names := ContractGateNames()
	if mode == ModeHuman {
		var b bytes.Buffer
		for _, n := range names {
			b.WriteString(n + "\n")
		}
		_, err := w.Write(b.Bytes())
		return err
	}
	gates := make([]listedGate, 0, len(names))
	for _, n := range names {
		gates = append(gates, listedGate{
			Name:      n,
			Summary:   ContractGateSummary(n),
			Metrics:   declaredMetrics(root, n),
			Requires:  []string{},
			Serialize: gateSerialize(n),
		})
	}
	return writeJSONLine(w, struct {
		Gates []listedGate `json:"gates"`
	}{Gates: gates})
}

// HostTarget names the platform a run's measurements speak for: Go's own
// os/arch pair, the spelling flow names targets by.
func HostTarget() string {
	return runtime.GOOS + "/" + runtime.GOARCH
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

// captureSplitTee is captureSplit with the child's output COPIED to the
// narration as it arrives, and captured as well.
//
// For the measurements that take minutes. A gate that holds a twenty-minute
// suite's output until it is over is indistinguishable from a wedged one while
// it runs, and when it finally reports `go_failed_tests = 3` the three names
// are in a buffer nobody prints — the reader is told a number and sent to find
// the tests themselves. OUR stdout carries the envelope and nothing else, so
// the copy is narration: on stderr for whoever is watching, and nowhere for a
// run nobody is.
func captureSplitTee(dir, name string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	narration := Narration()
	cmd.Stdout = io.MultiWriter(narration, &out)
	cmd.Stderr = io.MultiWriter(narration, &errBuf)
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
