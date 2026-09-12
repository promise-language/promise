package common

// bin/run — the judging layer for this project's gates (gate_contract.go).
//
// It is a DIFFERENT PROGRAM from bin/gate on purpose: the party under judgement
// must not hold what judges it. So the terms live beside the gates but outside
// them — person-edited caps in tools/gates/thresholds.json and ratcheting
// baselines in tools/gates/baselines.json — and only this layer reads either.
//
// Two modes, and the difference is who ran the gate:
//
//   - `bin/run <gate>` measures (by executing bin/gate, the same process
//     boundary a runner crosses) and then judges, for a person at a terminal.
//     No decision rests on it.
//   - `bin/run <gate> --verdict` judges an envelope it is GIVEN on stdin and
//     spawns nothing. This is what the flow SDK asks: it spawned the gate, so
//     it is the runner, and the runner may not come from the tree.
//
// Both reach the verdict through judge(), so they cannot disagree.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Direction is the sense in which a measurement is compared to its cap. The
// set is closed: an unknown value is refused when the manifest is loaded.
type Direction string

const (
	AtMost  Direction = "at_most"  // value must not exceed cap (counts of bad things)
	AtLeast Direction = "at_least" // value must not fall below cap (floors such as free space)
)

// Threshold is one entry in the thresholds manifest.
type Threshold struct {
	Direction Direction `json:"direction"`
	Cap       float64   `json:"cap"`
}

// ThresholdsFile is the thresholds manifest, repo-relative. It sits with the
// project's other gate configuration (tools/gates/, beside baselines.json and
// edit_gates.json — docs/gate-system.md) and is versioned with the tree it
// judges, so a commit carries the terms it was judged on and any machine
// reaches the same verdict for that commit, offline.
//
// It is a distinct artefact from the gates on purpose: a gate holding its own
// thresholds can be passed by editing the gate.
var ThresholdsFile = filepath.Join("tools", "gates", "thresholds.json")

// loadThresholds reads the manifest. An absent file is an error — a project
// with a judge must have one — and so is an unknown direction.
func loadThresholds(root string) (map[string]Threshold, error) {
	data, err := os.ReadFile(filepath.Join(root, ThresholdsFile))
	if err != nil {
		return nil, fmt.Errorf("loading thresholds manifest: %w", err)
	}
	var manifest map[string]Threshold
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parsing thresholds manifest: %w", err)
	}
	for name, t := range manifest {
		if t.Direction != AtMost && t.Direction != AtLeast {
			return nil, fmt.Errorf("thresholds manifest: metric %q has unknown direction %q (must be %q or %q)",
				name, t.Direction, AtMost, AtLeast)
		}
	}
	return manifest, nil
}

// term is one applied judgement, carried back in the verdict so anyone holding
// the envelope and this tree can recompute it. Kind separates the two sources,
// which move for different reasons and by different hands.
type term struct {
	Kind      string  `json:"kind"`      // "cap" — person-edited; "baseline" — ratcheted
	Direction string  `json:"direction"` // at_most/at_least for a cap; up/down/exact for a baseline
	Value     float64 `json:"value"`
}

// judge compares one envelope against the two kinds of terms this project
// holds, and returns only the ones it actually applied: a verdict must travel
// with what it was reached from, and terms the measurement never mentioned had
// no part in it.
//
// A cap is declared by a person and changes only when one edits it. A baseline
// is derived — the best a metric has been — and ratchets in its declared
// direction. A metric carrying both must satisfy both.
//
// AN INCOMPLETE RUN IS STILL JUDGED. What an incomplete run may not do is move
// a floor: honest numbers that understate the subject would lower a bar for a
// reason that is not about the code, and a ratchet by construction never moves
// back. Refusing the verdict instead would make every gate that measures less
// than everything — and `integration` is one, by design — permanently
// unpassable, which is indistinguishable at the call site from a broken gate.
// This judge never moves a baseline at all; the commit gate does that, on a
// complete passing run.
func judge(env Envelope, caps map[string]Threshold, baselines map[string]Baseline) (acceptable bool, terms map[string]term, detail string) {
	terms = map[string]term{}
	var failed []string
	for _, m := range env.Metrics {
		if t, capped := caps[m.Name]; capped {
			terms[m.Name] = term{Kind: "cap", Direction: string(t.Direction), Value: t.Cap}
			if !withinCap(m, t) {
				failed = append(failed, fmt.Sprintf("%s is %s, cap %s %s", m.Name, m.String(), t.Direction, formatCap(t.Cap)))
			}
			continue
		}
		// Enforced baselines only: a Pending entry has no value to compare
		// against yet, and an Informational one is tracked and never blocks.
		b, tracked := baselines[m.Name]
		if !tracked || b.Value == nil || b.Direction == "" || b.Type == "informational" {
			continue
		}
		terms[m.Name] = term{Kind: "baseline", Direction: b.Direction, Value: *b.Value}
		if !checkRatchet(b.Direction, *b.Value, m.Number()) {
			failed = append(failed, fmt.Sprintf("%s is %s, baseline %s %s", m.Name, m.String(), b.Direction, formatCap(*b.Value)))
		}
	}
	incomplete := ""
	if env.Incomplete != "" {
		incomplete = "; this run measured less than a full one, so no baseline may move from it: " + env.Incomplete
	}
	switch {
	case len(failed) > 0:
		return false, terms, strings.Join(failed, "; ") + incomplete
	case len(terms) == 0:
		return true, terms, "nothing here is judged: no metric this gate reported has a cap or a baseline" + incomplete
	default:
		return true, terms, "every judged metric is within its terms" + incomplete
	}
}

// projectTerms loads both sources of terms for the platform this run measured
// on. An unreadable baselines file is not fatal: the caps still judge, and a
// metric with neither term is simply not judged.
func projectTerms(root string) (map[string]Threshold, map[string]Baseline, error) {
	caps, err := loadThresholds(root)
	if err != nil {
		return nil, nil, err
	}
	all, err := LoadBaselines(root)
	if err != nil {
		return caps, map[string]Baseline{}, nil
	}
	if b, ok := all[HostTarget()]; ok {
		return caps, b, nil
	}
	return caps, map[string]Baseline{}, nil
}

func withinCap(m Metric, t Threshold) bool {
	if t.Direction == AtLeast {
		return m.Number() >= t.Cap
	}
	return m.Number() <= t.Cap
}

// verdictWire is what --verdict prints: one JSON object, whole. Thresholds is
// never omitted and never null — the SDK refuses a verdict without the terms
// it was reached from, since nothing could re-check it.
type verdictWire struct {
	Acceptable bool            `json:"acceptable"`
	Thresholds map[string]term `json:"thresholds"`
	Detail     string          `json:"detail,omitempty"`
}

// JudgeStdin judges an envelope this program did NOT produce and writes one
// verdict object to out. Nothing reaches out on any error path: a caller reads
// one object or none.
func JudgeStdin(root, name string, in io.Reader, out io.Writer) error {
	if !IsContractGate(name) {
		return unknownContractGate(name)
	}
	data, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("reading the envelope to judge: %w", err)
	}
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("what arrived on stdin is not an envelope: %w", err)
	}
	// Only this layer knows which gate the terms it applies belong to.
	if env.Gate != name {
		return fmt.Errorf("asked to judge %q against the terms for %q; a measurement is judged against its own gate's caps", env.Gate, name)
	}
	caps, baselines, err := projectTerms(root)
	if err != nil {
		return err
	}
	acceptable, terms, detail := judge(env, caps, baselines)
	body, err := json.Marshal(verdictWire{Acceptable: acceptable, Thresholds: terms, Detail: detail})
	if err != nil {
		return fmt.Errorf("rendering the verdict for %s: %w", name, err)
	}
	_, err = out.Write(append(body, '\n'))
	return err
}

// runOneGate is the by-hand path: execute bin/gate as a process — the same
// boundary a runner crosses, so a gate broken only across it (prints to
// stdout, exits without an envelope) is broken here too — then judge and print.
func runOneGate(root, name string, stdout io.Writer) error {
	cmd := exec.Command(filepath.Join(root, "bin", "gate"+ExeSuffix()), name, "--envelope")
	cmd.Dir = root
	// The gate's progress goes straight to our stderr, not into a buffer
	// printed afterwards: a working gate and a wedged one look the same while
	// output is held.
	cmd.Stderr = os.Stderr
	out, runErr := cmd.Output()

	var env Envelope
	if err := json.Unmarshal(out, &env); err != nil {
		if runErr != nil {
			return fmt.Errorf("%s did not measure anything: %w", name, runErr)
		}
		return fmt.Errorf("%s printed something that is not an envelope: %s", name, firstLine(string(out)))
	}
	caps, baselines, err := projectTerms(root)
	if err != nil {
		return err
	}
	fmt.Fprint(stdout, renderVerdict(env, caps, baselines))
	if acceptable, _, detail := judge(env, caps, baselines); !acceptable {
		return fmt.Errorf("%s: %s", name, detail)
	}
	return nil
}

// renderVerdict prints each measurement beside the term it was judged on. This
// is the only layer that can: it holds the caps and the baselines, so it can
// put a number next to what the number was judged against. A gate could only
// ever print the left-hand column.
func renderVerdict(env Envelope, caps map[string]Threshold, baselines map[string]Baseline) string {
	var sb strings.Builder
	width := 0
	for _, m := range env.Metrics {
		width = max(width, len(m.Name))
	}
	_, terms, _ := judge(env, caps, baselines)
	for _, m := range env.Metrics {
		judged, mark := "not judged", " "
		if t, ok := terms[m.Name]; ok {
			judged, mark = t.Kind+" "+t.Direction+" "+formatCap(t.Value), "✗"
			within := checkRatchet(t.Direction, t.Value, m.Number())
			if t.Kind == "cap" {
				within = withinCap(m, Threshold{Direction: Direction(t.Direction), Cap: t.Value})
			}
			if within {
				mark = "✓"
			}
		}
		fmt.Fprintf(&sb, "  %-*s  %14s  %-28s %s\n", width, m.Name, m.String()+unitSuffix(m.Unit), judged, mark)
	}
	if env.Incomplete != "" {
		fmt.Fprintf(&sb, "\n  measured less than a full run: %s\n  no baseline may move from this run\n", env.Incomplete)
	}
	return sb.String()
}

func formatCap(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

func unitSuffix(unit string) string {
	if unit == "bytes" {
		return " B"
	}
	return ""
}

// parseRunArgs reads `<gate> [--verdict]`. An unknown flag is refused rather
// than ignored: a caller that meant --verdict and mistyped it must not silently
// get the measuring mode, which spawns a gate.
func parseRunArgs(args []string) (name string, verdict bool, err error) {
	for _, raw := range args {
		flag, isFlag := flagName(raw)
		switch {
		case isFlag && flag == "verdict":
			verdict = true
		case isFlag:
			return "", false, fmt.Errorf("use of unknown flag %q", raw)
		case name != "":
			return "", false, fmt.Errorf("unexpected argument %q; one gate at a time", raw)
		default:
			name = raw
		}
	}
	if !IsContractGate(name) {
		return "", false, unknownContractGate(name)
	}
	return name, verdict, nil
}

func runUsage(root string) string {
	var sb strings.Builder
	sb.WriteString("usage: bin/run <gate>              measure one gate and judge it (for a person)\n")
	sb.WriteString("       bin/run <gate> --verdict    judge the envelope on stdin; print one JSON verdict\n")
	sb.WriteString("       bin/run --list [--json]     the gates and commands this project provides\n\n")
	sb.WriteString("Gates:\n")
	for _, n := range ContractGateNames() {
		fmt.Fprintf(&sb, "  %-18s %s\n", n, ContractGateSummary(n))
	}
	// Both sources, because a reader who knows only one cannot tell why a
	// metric was judged — or why one was not judged at all.
	caps, baselines, err := projectTerms(root)
	if err != nil {
		return sb.String()
	}
	fmt.Fprintf(&sb, "\nCaps (%s, edited by hand): %s\n", ThresholdsFile, strings.Join(sortedMapKeys(caps), ", "))
	var ratcheted []string
	for name, b := range baselines {
		if b.Value != nil && b.Direction != "" && b.Type != "informational" {
			ratcheted = append(ratcheted, name)
		}
	}
	sort.Strings(ratcheted)
	fmt.Fprintf(&sb, "Baselines (%s, %s, ratcheted): %s\n", baselinesFile, HostTarget(), strings.Join(ratcheted, ", "))
	sb.WriteString("A metric with neither is reported and not judged.\n")
	return sb.String()
}

// sortedKeys is the map's keys in a stable order, so usage text reads the same
// on every run.
func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CommandNames are the commands a flow may ask this project for. The set is
// the SDK's and closed; what this project HAS is derived from the tools it
// builds, never written down twice — a second copy claims `verify` on a
// checkout that never built it.
var CommandNames = []string{"verify", "setup", "cleanup"}

// SupportedCommands lists the commands this project provides: one directory per
// command under tools/build/cmd, filtered to the closed set. A directory
// holding anything else is not a command as far as this contract is concerned.
func SupportedCommands(root string) []string {
	entries, err := os.ReadDir(filepath.Join(root, "tools", "build", "cmd"))
	if err != nil {
		// No command directory is a project with no commands. That is what the
		// caller needs to know, and it is not an error to report here.
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && slices.Contains(CommandNames, e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// writeRunList prints what this project provides: the gates — the same list
// `bin/gate --list` prints, from the same registry, so the two cannot drift —
// and the commands, which the gate entry point knows nothing about.
func writeRunList(root string, w io.Writer, jsonOut bool) error {
	gates, commands := ContractGateNames(), SupportedCommands(root)
	if jsonOut {
		return writeJSONLine(w, struct {
			Gates    []string `json:"gates"`
			Commands []string `json:"commands"`
		}{Gates: gates, Commands: orEmpty(commands)})
	}
	fmt.Fprintln(w, "gates:")
	for _, g := range gates {
		fmt.Fprintf(w, "  %s\n", g)
	}
	fmt.Fprintln(w, "commands:")
	for _, c := range commands {
		fmt.Fprintf(w, "  %s\n", c)
	}
	return nil
}

// orEmpty keeps an absent list from marshalling as null: a reader asking what
// this project provides gets an empty list, which is an answer, rather than
// null, which is not one.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// RunRun is the bin/run CLI entry.
func RunRun(root string, args []string) error {
	if slices.ContainsFunc(args, func(a string) bool { return a == "-h" || a == "-help" || a == "--help" }) {
		fmt.Print(runUsage(root))
		return nil
	}
	if list, jsonOut, ok := parseListArgs(args); ok {
		if !list {
			return fmt.Errorf("use of unknown flag; run `bin/run --list [--json]` to list what this project provides")
		}
		return writeRunList(root, os.Stdout, jsonOut)
	}
	name, verdict, err := parseRunArgs(args)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, runUsage(root))
	}
	if verdict {
		return JudgeStdin(root, name, os.Stdin, os.Stdout)
	}
	return runOneGate(root, name, os.Stdout)
}
