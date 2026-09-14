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
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
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
	// checked has no :promise instance yet, and that is a missing tool rather
	// than a decision. `promise check` takes ONE FILE and most .pr files are
	// not checkable alone: a file of a multi-file module reports undefined
	// names that its own module defines (modules/std/vector.pr does not see
	// _FnIter in iter.pr), and the negative fixtures under tests/modules/ are
	// invalid on purpose. A per-file count therefore measures how the files are
	// arranged, not whether the code is sound — 50 of 942 here, none of them a
	// defect. Promise's semantic analysis IS exercised: every test compiles its
	// module graph under `tested:promise`. Checking a PROJECT without building
	// it needs `promise check <project>`, which the compiler does not have.
	"checked": {
		summary: "diagnostics from every language's checker",
		parts:   []string{"checked:go"},
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
	"integration": {
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

// MeasureContractGate runs one gate and returns what it measured. The error
// means the measurement could not be OBTAINED; it never means "the numbers are
// bad" — three failing tests is a successful run of the gate that counts them.
func MeasureContractGate(root, name string) (Envelope, error) {
	def, ok := contractGates[name]
	if !ok {
		return Envelope{}, unknownContractGate(name)
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
		metrics, incomplete, err := def.measure(root)
		if err != nil {
			return Envelope{}, err
		}
		if metrics == nil {
			metrics = []Metric{}
		}
		return Envelope{
			SchemaVersion: EnvelopeSchemaVersion,
			Gate:          name,
			Target:        HostTarget(),
			Metrics:       metrics,
			Incomplete:    incomplete,
		}, nil
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
	for _, part := range def.parts {
		sub, err := MeasureContractGate(root, part)
		if err != nil {
			return Envelope{}, fmt.Errorf("%s: %w", part, err)
		}
		env.Metrics = append(env.Metrics, sub.Metrics...)
		if sub.Incomplete != "" {
			reasons = append(reasons, part+": "+sub.Incomplete)
		}
	}
	env.Incomplete = strings.Join(reasons, "; ")
	return env, nil
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
	env, err := MeasureContractGate(root, name)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
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
	return writeJSONLine(w, struct {
		Gates []string `json:"gates"`
	}{Gates: names})
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
