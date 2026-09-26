package common

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// decodeOneObject mirrors the runner's parse (flow pkg/orchestrator/github):
// exactly one JSON object, not null, nothing after it.
func decodeOneObject(t *testing.T, out []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(out))
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, out)
	}
	if obj == nil {
		t.Fatalf("stdout is JSON null")
	}
	if dec.More() {
		t.Fatalf("trailing content after the object:\n%s", out)
	}
	return obj
}

func TestFitGate_RefusesWithoutEnvelope(t *testing.T) {
	var stdout bytes.Buffer
	err := runContractGate(t.TempDir(), []string{"fit"}, &stdout)
	if err == nil || !strings.Contains(err.Error(), "--envelope") {
		t.Fatalf("want a refusal naming --envelope, got %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("a refused run must print nothing on stdout, got %q", stdout.String())
	}
}

func TestFitGate_ArgsAreTheProtocol(t *testing.T) {
	for _, args := range [][]string{
		{"fit", "--envelope", "--verbose"}, // unknown flag refused, not ignored
		{"fit", "fit", "--envelope"},       // a second name refused, not dropped
		{"fitness", "--envelope"},          // unknown gate refused, not guessed
		{"--envelope"},                     // no gate named
	} {
		if _, _, err := parseContractGateArgs(args); err == nil {
			t.Errorf("parseContractGateArgs(%q): want an error", args)
		}
	}
	name, envelope, err := parseContractGateArgs([]string{"fit", "--envelope"})
	if err != nil || name != "fit" || !envelope {
		t.Fatalf("parseContractGateArgs(fit --envelope) = %q, %v, %v", name, envelope, err)
	}
}

// The runner appends --envelope and reads one object; the SDK then hands that
// object to the judge. Both filesystems are always reported.
func TestFitGate_EnvelopeIsTheOnlyOutput(t *testing.T) {
	var stdout bytes.Buffer
	if err := runContractGate(t.TempDir(), []string{"fit", "--envelope"}, &stdout); err != nil {
		t.Fatalf("fit --envelope: %v", err)
	}
	obj := decodeOneObject(t, stdout.Bytes())
	if obj["gate"] != "fit" {
		t.Errorf("gate = %v, want fit", obj["gate"])
	}
	var env Envelope
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("envelope does not round-trip: %v", err)
	}
	if env.Incomplete != "" {
		t.Fatalf("fit measured less than a full run: %s", env.Incomplete)
	}
	got := map[string]Metric{}
	for _, m := range env.Metrics {
		got[m.Name] = m
	}
	for _, name := range []string{"worktree_free_bytes", "build_cache_free_bytes"} {
		m, ok := got[name]
		if !ok {
			t.Errorf("envelope has no %s: %+v", name, env.Metrics)
			continue
		}
		if m.Type != MetricInt || m.Unit != "bytes" || m.Int <= 0 {
			t.Errorf("%s = %+v, want a positive int in bytes", name, m)
		}
	}
}

func TestFitGate_MetricWireKeepsItsType(t *testing.T) {
	out, err := json.Marshal(Envelope{
		SchemaVersion: EnvelopeSchemaVersion,
		Gate:          "fit",
		Target:        "linux-amd64",
		Metrics:       []Metric{Size("worktree_free_bytes", 5, "bytes")},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"gate":"fit","target":"linux-amd64","metrics":[{"name":"worktree_free_bytes","type":"int","value":5,"unit":"bytes"}]}`
	if string(out) != want {
		t.Errorf("wire form\n got %s\nwant %s", out, want)
	}
	var m Metric
	if err := json.Unmarshal([]byte(`{"name":"x","type":"int","value":1.5}`), &m); err == nil {
		t.Error("an int metric with a fractional value must be refused, not absorbed")
	}
}

func writeThresholds(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, ThresholdsFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestJudge_AtLeastFloor(t *testing.T) {
	caps := map[string]Threshold{"worktree_free_bytes": {Direction: AtLeast, Cap: 100}}
	low := Envelope{Gate: "fit", Metrics: []Metric{Size("worktree_free_bytes", 99, "bytes"), Size("other", 1, "")}}
	ok, terms, detail := judge(low, caps, nil)
	if ok || !strings.Contains(detail, "worktree_free_bytes is 99, cap at_least 100") {
		t.Errorf("below the floor: ok=%v detail=%q", ok, detail)
	}
	if len(terms) != 1 || terms["worktree_free_bytes"].Value != 100 || terms["worktree_free_bytes"].Kind != "cap" {
		t.Errorf("a verdict carries exactly the terms it applied, got %v", terms)
	}
	at := Envelope{Gate: "fit", Metrics: []Metric{Size("worktree_free_bytes", 100, "bytes")}}
	if ok, _, detail := judge(at, caps, nil); !ok {
		t.Errorf("at the floor must pass: %s", detail)
	}
}

// The other sense a cap can have, and the one this repository reaches for
// first: every count of something being wrong carries an `at_most 0` ceiling in
// thresholds.json (TestCorrectnessMetricsAreCappedAtZero), because the only
// correct value for such a count is zero and a ratchet from a non-zero one is a
// standing allowance.
func TestJudge_AtMostCeiling(t *testing.T) {
	caps := map[string]Threshold{"vet_findings": {Direction: AtMost, Cap: 0}}
	over := Envelope{Gate: "checked:go", Metrics: []Metric{Count("vet_findings", 3)}}
	ok, terms, detail := judge(over, caps, nil)
	if ok || !strings.Contains(detail, "vet_findings is 3, cap at_most 0") {
		t.Errorf("above the ceiling: ok=%v detail=%q", ok, detail)
	}
	if len(terms) != 1 || terms["vet_findings"].Direction != string(AtMost) {
		t.Errorf("a verdict carries exactly the terms it applied, got %v", terms)
	}
	at := Envelope{Gate: "checked:go", Metrics: []Metric{Count("vet_findings", 0)}}
	if ok, _, detail := judge(at, caps, nil); !ok {
		t.Errorf("at the ceiling must pass: %s", detail)
	}
}

// A ratcheted metric is judged against the baseline, and only an ENFORCED one
// judges: a pending entry has no value yet, and an informational one never
// blocks.
func TestJudge_BaselineRatchet(t *testing.T) {
	zero, ten := 0.0, 10.0
	baselines := map[string]Baseline{
		"host_test_failures": {Value: &zero, Direction: "exact"},
		"host_test_count":    {Value: &ten, Direction: "up"},
		"go_test_count":      {Direction: "up"},                                        // pending: no value yet
		"host_leak_count":    {Value: &zero, Type: "informational", Direction: "down"}, // tracked, never blocks
	}
	env := Envelope{Gate: "tested", Metrics: []Metric{
		Count("host_test_failures", 1),
		Count("host_test_count", 12),
		Count("go_test_count", 5),
		Count("host_leak_count", 3),
	}}
	ok, terms, detail := judge(env, nil, baselines)
	if ok || !strings.Contains(detail, "host_test_failures is 1, baseline exact 0") {
		t.Errorf("a regression against the baseline must fail: ok=%v detail=%q", ok, detail)
	}
	if terms["host_test_failures"].Kind != "baseline" {
		t.Errorf("term kind = %q, want baseline", terms["host_test_failures"].Kind)
	}
	if _, judged := terms["go_test_count"]; judged {
		t.Error("a pending baseline was applied; it has no value to compare against")
	}
	if _, judged := terms["host_leak_count"]; judged {
		t.Error("an informational baseline blocked; it is tracked and never blocks")
	}
	// Above the floor in the ratchet's direction passes.
	up := Envelope{Gate: "tested", Metrics: []Metric{Count("host_test_count", 12)}}
	if ok, _, detail := judge(up, nil, baselines); !ok {
		t.Errorf("moving in the ratchet's direction must pass: %s", detail)
	}
}

// A metric carrying both terms must satisfy BOTH. The cap is the requirement a
// person wrote and the baseline the best the metric has been, so judging on the
// cap alone would let a cap looser than the baseline beside it forgive exactly
// the regression the ratchet exists to catch (T2192).
func TestJudge_CapAndBaselineBothApply(t *testing.T) {
	three := 3.0
	caps := map[string]Threshold{"host_leak_count": {Direction: AtMost, Cap: 10}}
	baselines := map[string]Baseline{"host_leak_count": {Value: &three, Direction: "down"}}

	// Within the loose cap, but a regression against the tighter baseline.
	env := Envelope{Gate: "tested", Metrics: []Metric{Count("host_leak_count", 5)}}
	ok, terms, detail := judge(env, caps, baselines)
	if ok || !strings.Contains(detail, "host_leak_count is 5, baseline down 3") {
		t.Errorf("the baseline must still judge a capped metric: ok=%v detail=%q", ok, detail)
	}
	if terms["host_leak_count"].Kind != "baseline" {
		t.Errorf("the verdict must carry the term that decided it, got %v", terms["host_leak_count"])
	}

	// Over the cap: the cap decides, and is what the verdict carries.
	over := Envelope{Gate: "tested", Metrics: []Metric{Count("host_leak_count", 11)}}
	ok, terms, detail = judge(over, caps, baselines)
	if ok || !strings.Contains(detail, "host_leak_count is 11, cap at_most 10") {
		t.Errorf("a missed cap must fail: ok=%v detail=%q", ok, detail)
	}
	if terms["host_leak_count"].Kind != "cap" {
		t.Errorf("term kind = %q, want cap", terms["host_leak_count"].Kind)
	}

	// Within both: acceptable, and the cap is the term recorded — it is the
	// requirement, while the baseline only records how far the metric has come.
	within := Envelope{Gate: "tested", Metrics: []Metric{Count("host_leak_count", 2)}}
	ok, terms, detail = judge(within, caps, baselines)
	if !ok {
		t.Errorf("within both terms must pass: %s", detail)
	}
	if terms["host_leak_count"].Kind != "cap" || terms["host_leak_count"].Value != 10 {
		t.Errorf("term = %v, want the cap", terms["host_leak_count"])
	}
}

// A baseline beside a cap judges only when it is ENFORCED. Statement coverage
// cannot see this: the same lines run either way, and dropping the informational
// clause while restructuring would make every gate carrying a tracked-but-not-
// enforced baseline start blocking on it (T2192).
func TestJudge_CapWithNonEnforcedBaseline(t *testing.T) {
	zero := 0.0
	caps := map[string]Threshold{"host_leak_count": {Direction: AtMost, Cap: 10}}
	env := Envelope{Gate: "tested", Metrics: []Metric{Count("host_leak_count", 5)}}

	for _, tt := range []struct {
		name     string
		baseline Baseline
	}{
		// Informational: tracked, never blocks — even though 5 > 0 would fail it.
		{"informational", Baseline{Value: &zero, Direction: "down", Type: "informational"}},
		// Pending: a direction but no value yet, so there is nothing to compare.
		{"pending", Baseline{Direction: "down"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			baselines := map[string]Baseline{"host_leak_count": tt.baseline}
			ok, terms, detail := judge(env, caps, baselines)
			if !ok {
				t.Errorf("a %s baseline must not block: %s", tt.name, detail)
			}
			if terms["host_leak_count"].Kind != "cap" {
				t.Errorf("term = %v, want the cap — only an enforced baseline judges", terms["host_leak_count"])
			}
		})
	}
}

// The term the verdict selected is the one a person sees. renderVerdict is the
// only surface that shows it, so the dual-term case is pinned here too: the
// baseline decided, so the baseline is what the line names, with the ✗.
func TestRenderVerdict_ShowsTheTermThatDecided(t *testing.T) {
	three := 3.0
	caps := map[string]Threshold{"host_leak_count": {Direction: AtMost, Cap: 10}}
	baselines := map[string]Baseline{"host_leak_count": {Value: &three, Direction: "down"}}

	// Within the loose cap, over the tighter baseline.
	over := Envelope{Gate: "tested", Metrics: []Metric{Count("host_leak_count", 5)}}
	got := renderVerdict(over, caps, baselines)
	if !strings.Contains(got, "baseline down 3") || !strings.Contains(got, "✗") {
		t.Errorf("rendered %q, want the deciding baseline term marked failed", got)
	}
	if strings.Contains(got, "cap at_most 10") {
		t.Errorf("rendered %q, want the term that decided, not the one that held", got)
	}

	// Within both: the cap is the requirement, and it is what the line names.
	within := Envelope{Gate: "tested", Metrics: []Metric{Count("host_leak_count", 2)}}
	got = renderVerdict(within, caps, baselines)
	if !strings.Contains(got, "cap at_most 10") || !strings.Contains(got, "✓") {
		t.Errorf("rendered %q, want the cap marked satisfied", got)
	}
}

// An incomplete run is NOT acceptable, however good the numbers it did report
// are. A verdict is a claim about a subject, and a run that did not measure its
// whole subject has no grounds to make one — "the part I measured was fine" is
// not "this is safe to land". The verdict names what could not be measured, so
// the reader is sent at the gap rather than at the code.
func TestJudge_IncompleteIsNotAcceptable(t *testing.T) {
	caps := map[string]Threshold{"worktree_free_bytes": {Direction: AtLeast, Cap: 1}}
	env := Envelope{Gate: "fit", Metrics: []Metric{Size("worktree_free_bytes", 50, "bytes")}, Incomplete: "no GOCACHE"}
	ok, _, detail := judge(env, caps, nil)
	if ok {
		t.Error("an incomplete run was judged acceptable; it measured less than its subject")
	}
	if !strings.Contains(detail, "no GOCACHE") {
		t.Errorf("the verdict must name what could not be measured, got %q", detail)
	}
	// A missed cap still fails, and reports the cap rather than the gap: the
	// numbers that WERE measured are real, and a reader fixing a cap miss must
	// not be sent looking for a measurement problem instead.
	env.Metrics = []Metric{Size("worktree_free_bytes", 0, "bytes")}
	ok, _, detail = judge(env, caps, nil)
	if ok {
		t.Error("an incomplete run that misses a cap was judged acceptable")
	}
	if !strings.Contains(detail, "worktree_free_bytes") {
		t.Errorf("a missed cap must be reported as such, got %q", detail)
	}
}

// The SDK refuses a verdict without "acceptable" or with null "thresholds"
// (flow parseVerdict), so both must always be present.
func TestJudge_StdinVerdictShape(t *testing.T) {
	root := writeThresholds(t, `{"worktree_free_bytes": {"direction": "at_least", "cap": 10}}`)
	var out bytes.Buffer
	in := strings.NewReader(`{"gate":"fit","metrics":[{"name":"worktree_free_bytes","type":"int","value":5,"unit":"bytes"}]}`)
	if err := JudgeStdin(root, "fit", in, &out); err != nil {
		t.Fatalf("JudgeStdin: %v", err)
	}
	obj := decodeOneObject(t, out.Bytes())
	if obj["acceptable"] != false {
		t.Errorf("acceptable = %v, want false", obj["acceptable"])
	}
	th, ok := obj["thresholds"].(map[string]any)
	if !ok {
		t.Fatalf("thresholds = %v, want an object of applied terms", obj["thresholds"])
	}
	applied, ok := th["worktree_free_bytes"].(map[string]any)
	if !ok || applied["value"] != float64(10) || applied["kind"] != "cap" {
		t.Errorf("worktree_free_bytes term = %v, want the applied cap", th["worktree_free_bytes"])
	}

	// Nothing is judged → still an object, never null.
	out.Reset()
	if err := JudgeStdin(root, "fit", strings.NewReader(`{"gate":"fit","metrics":[]}`), &out); err != nil {
		t.Fatal(err)
	}
	if th, ok := decodeOneObject(t, out.Bytes())["thresholds"].(map[string]any); !ok || len(th) != 0 {
		t.Errorf("an unjudged verdict must carry an empty thresholds object, got %s", out.String())
	}
}

func TestJudge_StdinErrorsPrintNothing(t *testing.T) {
	root := writeThresholds(t, `{}`)
	for name, tc := range map[string]struct{ gate, in string }{
		"not an envelope": {"fit", "hello"},
		"another gate":    {"fit", `{"gate":"tested","metrics":[]}`},
		"unknown gate":    {"nosuchgate", `{"gate":"nosuchgate","metrics":[]}`},
	} {
		var out bytes.Buffer
		if err := JudgeStdin(root, tc.gate, strings.NewReader(tc.in), &out); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if out.Len() != 0 {
			t.Errorf("%s: an error path printed %q", name, out.String())
		}
	}
}

func TestJudge_ManifestDirectionIsClosed(t *testing.T) {
	root := writeThresholds(t, `{"worktree_free_bytes": {"direction": "above", "cap": 1}}`)
	if _, err := loadThresholds(root); err == nil {
		t.Error("an unknown direction must be refused at load time")
	}
}
