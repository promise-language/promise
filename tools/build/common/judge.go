package common

// The judging layer for this project's gates (gate_contract.go), which bin/run
// carries (run.go).
//
// It is a DIFFERENT PROGRAM from bin/gate on purpose: the party under judgement
// must not hold what judges it. So the terms live beside the gates but outside
// them (terms.go), and only this layer reads them. It spawns nothing: a verdict
// is reached from the envelope it is handed and the terms files, so anyone
// holding both reaches the same one.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// appliedTerm is one metric's judgement as the verdict carries it: the
// direction, and each bound that applied — a cap, a baseline, or both — so a
// reader holding the envelope can re-check the verdict without the files.
type appliedTerm struct {
	Direction Direction    `json:"direction"`
	Cap       *json.Number `json:"cap,omitempty"`
	Baseline  *json.Number `json:"baseline,omitempty"`
}

// failures names each bound m is outside of, in the words a verdict uses:
// "cap at_most 0". Empty when m is within every bound.
func (t appliedTerm) failures(m Metric) []string {
	var out []string
	if t.Cap != nil && !holds(m, t.Direction, *t.Cap) {
		out = append(out, fmt.Sprintf("cap %s %s", t.Direction, *t.Cap))
	}
	if t.Baseline != nil && !holds(m, t.Direction, *t.Baseline) {
		out = append(out, fmt.Sprintf("baseline %s %s", t.Direction, *t.Baseline))
	}
	return out
}

// describe is every bound of the term, for a person reading a metric beside it.
func (t appliedTerm) describe() []string {
	var out []string
	if t.Cap != nil {
		out = append(out, fmt.Sprintf("cap %s %s", t.Direction, *t.Cap))
	}
	if t.Baseline != nil {
		out = append(out, fmt.Sprintf("baseline %s %s", t.Direction, *t.Baseline))
	}
	return out
}

// Verdict is what --verdict prints: one JSON object, whole. Thresholds is never
// omitted and never null — a verdict travels with the terms it was reached
// from, and flow refuses one that carries none.
type Verdict struct {
	Acceptable bool                   `json:"acceptable"`
	Thresholds map[string]appliedTerm `json:"thresholds"`
	Detail     string                 `json:"detail,omitempty"`
}

// maxEvidence is how many members a refusal's detail names before it counts
// the rest: enough to start on, few enough to read.
const maxEvidence = 5

// storeRemedy is what a person does about the two metrics every tree gate
// carries for the content-addressed store (casledger.go), which no leaf declares.
const storeRemedy = "a run that fetched store blobs or exploded a second toolchain home asked for content the warm-up did not stage: find what the tree now needs from `PROMISE_CACHE_DEBUG=1 bin/test`, and re-measure"

// judge applies every term the envelope's metrics carry and says what came of
// it. Every cap and every baseline a metric has is applied, and all must hold:
// a cap looser than the baseline beside it would otherwise forgive exactly the
// regression the ratchet exists to catch.
//
// AN INCOMPLETE RUN IS NOT ACCEPTABLE. A run that did not measure its whole
// subject has no grounds for a verdict: "the part I measured was fine" is not
// "this is safe to land", and the difference is invisible exactly where it
// matters most — a build that died, a suite that never ran, a tree that moved
// mid-measurement.
//
// A refusal's detail says three things, in order: the judgement (metric, value
// and term), the evidence (the members that did not pass, up to maxEvidence),
// and the remedy each gate states once in its own definition.
func judge(root string, env Envelope, terms Terms) Verdict {
	v := Verdict{Acceptable: true, Thresholds: map[string]appliedTerm{}}
	var judgements []string
	var failing []string
	for _, m := range env.Metrics {
		t, ok := termFor(m.Name, env.Target, terms)
		if !ok {
			continue // no term of either kind: this metric is not judged
		}
		v.Thresholds[m.Name] = t
		failed := t.failures(m)
		if len(failed) == 0 {
			continue
		}
		failing = append(failing, m.Name)
		judgements = append(judgements, fmt.Sprintf("%s is %s, %s%s", m.Name, m.String(), strings.Join(failed, " and "), whereFrom(env.Groups, m.Name)))
	}
	if env.Incomplete != "" {
		judgements = append(judgements, "this run measured less than a full one, so it cannot be acceptable: "+env.Incomplete)
	}
	if len(judgements) == 0 {
		v.Detail = "every judged metric is within its terms"
		return v
	}
	v.Acceptable = false
	var b strings.Builder
	evidence, rest := refusalEvidence(env.Groups)
	for i, j := range judgements {
		b.WriteString(j)
		if i == len(judgements)-1 && len(evidence) == 0 {
			b.WriteString(", and the gate reported nothing about why")
		}
		b.WriteString(".\n")
	}
	for _, e := range evidence {
		b.WriteString(e + "\n")
	}
	if rest > 0 {
		fmt.Fprintf(&b, "… and %d more that did not pass.\n", rest)
	}
	for _, r := range remedies(root, env.Gate, failing, env.Incomplete) {
		b.WriteString(capitalize(r) + ".\n")
	}
	v.Detail = strings.TrimRight(b.String(), "\n")
	return v
}

// termFor is the term the metric carries on target: its cap, its baseline
// there, or both.
func termFor(name, target string, terms Terms) (appliedTerm, bool) {
	c, capped := terms.Caps[name]
	b, tracked := terms.Baselines[name]
	var t appliedTerm
	if capped {
		n := c.Cap
		t.Direction, t.Cap = c.Direction, &n
	}
	if tracked {
		if n, ok := b.at(target); ok {
			t.Direction, t.Baseline = b.Direction, &n
		}
	}
	return t, t.Cap != nil || t.Baseline != nil
}

// whereFrom names the groups that carry the metric, so "worktree_free_bytes is
// 3 GiB" is told which filesystem.
func whereFrom(groups []Group, metric string) string {
	var where []string
	for _, g := range groups {
		for _, m := range g.Metrics {
			if m.Name == metric {
				where = append(where, fmt.Sprintf("%s (%s)", g.Name, m.String()))
			}
		}
	}
	if len(where) == 0 {
		return ""
	}
	if len(where) > maxEvidence {
		where = append(where[:maxEvidence], fmt.Sprintf("%d more", len(where)-maxEvidence))
	}
	return ", in " + strings.Join(where, ", ")
}

// refusalEvidence is the first maxEvidence members that neither passed nor were
// excluded, each with its own words beneath it, and how many more there were.
func refusalEvidence(groups []Group) (lines []string, rest int) {
	shown := 0
	for _, g := range groups {
		for _, m := range g.Members {
			if m.Outcome == OutcomePass || m.Outcome == OutcomeExcluded {
				continue
			}
			if shown == maxEvidence {
				rest++
				continue
			}
			shown++
			line := fmt.Sprintf("%s: %s (%s)", g.Name, m.Name, m.Outcome)
			if m.Detail != "" {
				line += "\n    " + strings.ReplaceAll(m.Detail, "\n", "\n    ")
			}
			lines = append(lines, line)
		}
	}
	return lines, rest
}

// remedies is what to do about the failing metrics of gate: the remedy of each
// leaf declaring one of them, once each, in the gate's own order. An incomplete
// run adds the remedy of each leaf its reason names — a composition prefixes a
// part's reason with the part — or, naming none, of every leaf.
func remedies(root, gate string, failing []string, incomplete string) []string {
	leaves := leafGates(gate)
	var out []string
	add := func(r string) {
		if r != "" && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	for _, leaf := range leaves {
		for _, s := range leafMetrics(root, leaf) {
			if slices.Contains(failing, s.Name) {
				add(contractGates[leaf].remedy)
				break
			}
		}
	}
	if slices.ContainsFunc(failing, isContractStoreMetric) {
		add(storeRemedy + "; `bin/run " + gate + "` re-measures")
	}
	if incomplete != "" {
		named := false
		for _, leaf := range leaves {
			if len(leaves) == 1 || strings.Contains(incomplete, leaf+": ") {
				add(contractGates[leaf].remedy)
				named = true
			}
		}
		if !named {
			for _, leaf := range leaves {
				add(contractGates[leaf].remedy)
			}
		}
	}
	return out
}

// leafGates is every leaf a gate is made of, in declaration order: the gate
// itself when it measures.
func leafGates(name string) []string {
	def, ok := contractGates[name]
	if !ok {
		return nil
	}
	if def.measure != nil {
		return []string{name}
	}
	var out []string
	for _, part := range def.parts {
		for _, leaf := range leafGates(part) {
			if !slices.Contains(out, leaf) {
				out = append(out, leaf)
			}
		}
	}
	return out
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// readEnvelopeToJudge is an envelope this judge will reach a verdict on, or the
// reason it reaches none. Each refusal is a measurement nobody can judge: what
// arrived is not an envelope, is one of a version this judge does not read, is
// another gate's, names a metric twice, reports a metric as other than its
// gate declares it, claims to be complete while lacking a metric its gate
// declares, or carries nothing a term speaks to.
func readEnvelopeToJudge(root, name string, data []byte, terms Terms) (Envelope, error) {
	var head struct {
		SchemaVersion *int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return Envelope{}, fmt.Errorf("what arrived on stdin is not an envelope: %w", err)
	}
	switch {
	case head.SchemaVersion == nil:
		return Envelope{}, fmt.Errorf("the envelope carries no schema_version")
	case *head.SchemaVersion != EnvelopeSchemaVersion:
		return Envelope{}, fmt.Errorf("the envelope is schema_version %d, and this judge reads %d", *head.SchemaVersion, EnvelopeSchemaVersion)
	}
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, fmt.Errorf("what arrived on stdin is not an envelope: %w", err)
	}
	// Only this layer knows which gate the terms it applies belong to.
	if env.Gate != name {
		return Envelope{}, fmt.Errorf("asked to judge %q against the terms for %q; a measurement is judged against its own gate's terms", env.Gate, name)
	}
	declared := declaredMetrics(root, name)
	seen := map[string]bool{}
	termed := false
	for _, m := range env.Metrics {
		if seen[m.Name] {
			return Envelope{}, fmt.Errorf("the envelope reports %q twice", m.Name)
		}
		seen[m.Name] = true
		if i := slices.IndexFunc(declared, func(s MetricSpec) bool { return s.Name == m.Name }); i >= 0 && declared[i] != specOf(m) {
			return Envelope{}, fmt.Errorf("the envelope reports %q as %s, which %s declares as %s", m.Name, specOf(m).describe(), name, declared[i].describe())
		}
		if _, ok := termFor(m.Name, env.Target, terms); ok {
			termed = true
		}
	}
	if env.Incomplete != "" {
		return env, nil // judged, and never acceptable: the reason is the verdict's
	}
	var missing []string
	for _, s := range declared {
		if !seen[s.Name] {
			missing = append(missing, s.Name)
		}
	}
	if len(missing) > 0 {
		return Envelope{}, fmt.Errorf("the envelope claims a complete run of %s but lacks %s, which it declares", name, strings.Join(missing, ", "))
	}
	if !termed {
		return Envelope{}, fmt.Errorf("no metric of the %s envelope carries a cap or a baseline, so nothing can be judged — and an unjudged measurement is not a pass", name)
	}
	return env, nil
}

// JudgeStdin is `bin/run <gate> --verdict`: it judges an envelope this program
// did NOT produce and writes one verdict object to out. Nothing reaches out on
// any error path: a caller reads one object or none.
func JudgeStdin(root, name string, in io.Reader, out io.Writer) error {
	if !IsContractGate(name) {
		return unknownContractGate(name)
	}
	data, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("reading the envelope to judge: %w", err)
	}
	terms, err := LoadTerms(root)
	if err != nil {
		return err
	}
	env, err := readEnvelopeToJudge(root, name, data, terms)
	if err != nil {
		return err
	}
	v := judge(root, env, terms)
	var body bytes.Buffer
	if err := encodeLine(&body, v); err != nil {
		return fmt.Errorf("rendering the verdict for %s: %w", name, err)
	}
	// The flow's path: it spawned the gate, so this is the first party in the
	// tree that knows the measurement passed. Before the write, and on stderr if
	// it fails, so the one object this prints on stdout is unaffected either way.
	reportBlessing(root, env, v.Acceptable)
	_, err = out.Write(body.Bytes())
	return err
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

// renderVerdict prints each measurement beside every term it was judged on,
// then the verdict. This is the only layer that can: it holds the terms, so it
// can put a number next to what the number was judged against. A gate could
// only ever print the left-hand column.
func renderVerdict(env Envelope, v Verdict) string {
	var sb strings.Builder
	width := 0
	for _, m := range env.Metrics {
		width = max(width, len(m.Name))
	}
	for _, m := range env.Metrics {
		judged, mark := "not judged", " "
		if t, ok := v.Thresholds[m.Name]; ok {
			judged, mark = strings.Join(t.describe(), ", "), "✓"
			if len(t.failures(m)) > 0 {
				mark = "✗"
			}
		}
		fmt.Fprintf(&sb, "  %-*s  %14s  %-40s %s\n", width, m.Name, m.String()+unitSuffix(m.Unit), judged, mark)
	}
	if v.Acceptable {
		sb.WriteString("\nacceptable: " + v.Detail + "\n")
	} else {
		sb.WriteString("\nnot acceptable:\n  " + strings.ReplaceAll(v.Detail, "\n", "\n  ") + "\n")
	}
	return sb.String()
}

func unitSuffix(unit string) string {
	switch unit {
	case unitBytes:
		return " B"
	case unitPercent:
		return " %"
	}
	return ""
}
