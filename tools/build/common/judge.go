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
// direction. A metric carrying both must satisfy both: a cap looser than the
// baseline beside it would otherwise forgive exactly the regression the ratchet
// exists to catch. The verdict records the term that DECIDED it — the one that
// failed, or the cap when both hold — because one metric carries one term on
// the wire and it has to be the one the answer rests on.
//
// AN INCOMPLETE RUN IS NOT ACCEPTABLE. A verdict is a claim about a subject,
// and a run that did not measure its whole subject has no grounds to make one:
// "the part I measured was fine" is not "this is safe to land", and the
// difference is invisible at the call site precisely where it matters most —
// a build that died, a suite that never ran, a tree that moved mid-measurement.
// Reporting that as acceptable turns the one gate a landing decision rests on
// into a coin flip weighted by how much of it happened to run.
//
// The cost is real and is the right cost: a gate that cannot measure its
// subject blocks, loudly, naming what it could not measure, instead of passing
// a change nothing checked. What an incomplete run ALSO may not do is move a
// floor — honest numbers that understate the subject would lower a bar for a
// reason that is not about the code, and a ratchet by construction never moves
// back. This judge never moves a baseline at all, and nothing else in this
// repository does either: advancing one is the workspace's job.
//
// AN UNJUDGED MEASUREMENT IS NOT A PASS EITHER. A gate whose metrics carry
// neither a cap nor a baseline has reported numbers nobody set a rule for, and
// answering "acceptable" to that is a guess wearing a verdict's clothes.
func judge(env Envelope, caps map[string]Threshold, baselines map[string]Baseline) (acceptable bool, terms map[string]term, detail string) {
	terms = map[string]term{}
	var failed []string
	for _, m := range env.Metrics {
		t, capped := caps[m.Name]
		// Enforced baselines only: a Pending entry has no value to compare
		// against yet, and an Informational one is tracked and never blocks.
		b, tracked := baselines[m.Name]
		enforced := tracked && b.Value != nil && b.Direction != "" && b.Type != "informational"

		capFailed := capped && !withinCap(m, t)
		baseFailed := enforced && !checkRatchet(b.Direction, *b.Value, m.Number())

		// Each term is built only when it exists, so neither can be recorded
		// from a map miss's zero value if this priority list is ever reordered.
		var capTerm, baseTerm term
		if capped {
			capTerm = term{Kind: "cap", Direction: string(t.Direction), Value: t.Cap}
		}
		if enforced {
			baseTerm = term{Kind: "baseline", Direction: b.Direction, Value: *b.Value}
		}
		// The verdict carries the term that decided it: the one that failed, or
		// the cap when both hold — it is the requirement, while the baseline
		// beside it only records how far the metric has come.
		switch {
		case capFailed:
			terms[m.Name] = capTerm
		case baseFailed:
			terms[m.Name] = baseTerm
		case capped:
			terms[m.Name] = capTerm
		case enforced:
			terms[m.Name] = baseTerm
		default:
			continue // no term of either kind — this metric is not judged
		}
		if capFailed {
			failed = append(failed, fmt.Sprintf("%s is %s, cap %s %s", m.Name, m.String(), t.Direction, formatCap(t.Cap)))
		}
		if baseFailed {
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
	case env.Incomplete != "":
		return false, terms, "this run did not measure its whole subject, so it cannot report the change safe to land: " + env.Incomplete
	case len(terms) == 0:
		return false, terms, "no metric this gate reported carries a cap or a baseline, so nothing was judged — an unjudged measurement is not a pass"
	default:
		return true, terms, "every judged metric is within its terms"
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

// metricWithinTerm reports whether m satisfies the term it was judged on. One
// spelling, because two readers of the same verdict — bin/run's per-metric table
// and bin/verify's per-part summary — must agree about which metric failed.
func metricWithinTerm(m Metric, t term) bool {
	if t.Kind == "cap" {
		return withinCap(m, Threshold{Direction: Direction(t.Direction), Cap: t.Value})
	}
	return checkRatchet(t.Direction, t.Value, m.Number())
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
	// The flow's path: it spawned the gate, so this is the first party in the
	// tree that knows the measurement passed. Before the write, and on stderr if
	// it fails, so the one object this prints on stdout is unaffected either way.
	reportBlessing(root, env, acceptable)
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
	acceptable, _, detail := judge(env, caps, baselines)
	reportBlessing(root, env, acceptable)
	if !acceptable {
		return fmt.Errorf("%s: %s", name, detail)
	}
	return nil
}

// reportBlessing records the blessing a passing verdict earns, and tells the
// operator on stderr when it could not.
//
// HERE, AND NOT IN THE GATE, because only this layer holds a verdict: a gate
// reports numbers and is deliberately incapable of knowing whether they are
// acceptable. Both of this layer's modes record — the by-hand `bin/run <gate>`
// and the `--verdict` mode the flow asks for after running the gate itself —
// which is what makes one passing `integration` measurement sufficient for the
// commit guard whoever ran it, instead of only when bin/verify did (T2170).
//
// It never changes the verdict. bin/run answers "is this measurement
// acceptable"; a blessing that could not be written does not make an acceptable
// measurement unacceptable, it means the commit guard will ask for another run.
func reportBlessing(root string, env Envelope, acceptable bool) {
	if err := blessIfPassed(root, env, acceptable); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
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
			if metricWithinTerm(m, t) {
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

// metaBuilderName is the one cmd/ directory that is not a command: ./make
// builds the others and is not itself installed into bin/, so it can collide
// with nothing and belongs in no listing.
const metaBuilderName = "make"

// SupportedCommands lists the commands this project BUILDS: one directory per
// command under tools/build/cmd, minus the meta-builder. Every one of them,
// not a filtered subset.
//
// The filter this used to apply — flow's closed {setup, verify, cleanup} — was
// answering a different question than the one the listing is asked. Flow's set
// is closed because flow decides WHEN each runs and has no place to run a
// fourth. The workspace asks this list something else entirely: which names in
// bin/ belong to the project's own builder. It uses the answer to refuse a
// release that would install over one of them, and to decide which recorded
// names it may delete — so a project that under-reports gets its own tools
// silently overwritten by `workspace setup`, or removed by `workspace update`,
// with the one-name-one-builder refusal that exists to prevent exactly that
// never firing (workspace tool-contract.md §5).
//
// It reports what the project builds, not what is built: bin/ is empty in a
// fresh clone, and the claim has to exist before either builder has run.
func SupportedCommands(root string) []string {
	entries, err := os.ReadDir(filepath.Join(root, "tools", "build", "cmd"))
	if err != nil {
		// No command directory is a project with no commands. That is what the
		// caller needs to know, and it is not an error to report here.
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != metaBuilderName {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// CommandGateCollisions returns the names that are both a command this project
// builds and a gate it answers, sorted as SupportedCommands sorts.
//
// `bin/run <name>` dispatches both kinds, so such a name would mean one of two
// things. It is an error rather than a precedence rule: precedence would make
// the shadowed name silently unreachable while both still appeared in --list.
func CommandGateCollisions(root string) []string {
	gates := ContractGateNames()
	var both []string
	for _, c := range SupportedCommands(root) {
		if slices.Contains(gates, c) {
			both = append(both, c)
		}
	}
	return both
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
	// A command is dispatched before a gate is looked for, because the two
	// share one namespace and a name cannot be both (CommandGateCollisions).
	// `run <name>` dispatching only gates would list sixteen commands it
	// refuses to run, which is a listing that describes a different tool.
	if name := firstPositional(args); name != "" && slices.Contains(SupportedCommands(root), name) {
		return runProjectCommand(root, name, args)
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

// firstPositional returns the first argument that is not a flag, or "". None of
// this tool's own flags takes a value, so the first word not starting with "-"
// is the name: there is no `-flag value` pair for this to mistake a value for.
func firstPositional(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// runProjectCommand execs bin/<name>, passing through every argument but the
// name, and makes the child's exit status this process's.
//
// It measures and judges NOTHING: no envelope is read and no terms are loaded.
// A `run` that interpreted a command's output would be deciding something no
// gate measured, which is the whole separation the gate/judge split exists for.
func runProjectCommand(root, name string, args []string) error {
	rest := make([]string, 0, len(args))
	dropped := false
	for _, a := range args {
		if !dropped && a == name {
			dropped = true
			continue
		}
		rest = append(rest, a)
	}
	bin := filepath.Join(root, "bin", name+ExeSuffix())
	if !Exists(bin) {
		return fmt.Errorf("this project builds %s, but bin/%s is not there — run ./make",
			name, name+ExeSuffix())
	}
	cmd := exec.Command(bin, rest...)
	cmd.Dir = root
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
