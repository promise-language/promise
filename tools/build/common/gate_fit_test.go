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

// An incomplete run is still judged — refusing it outright would make every
// gate that measures less than everything permanently unpassable. What it may
// not do is move a baseline, and the verdict says so.
func TestJudge_IncompleteIsJudgedAndMovesNoBaseline(t *testing.T) {
	caps := map[string]Threshold{"worktree_free_bytes": {Direction: AtLeast, Cap: 1}}
	env := Envelope{Gate: "fit", Metrics: []Metric{Size("worktree_free_bytes", 50, "bytes")}, Incomplete: "no GOCACHE"}
	ok, _, detail := judge(env, caps, nil)
	if !ok {
		t.Errorf("an incomplete run within its caps must still pass: %s", detail)
	}
	if !strings.Contains(detail, "no baseline may move") {
		t.Errorf("detail must say no baseline may move from an incomplete run, got %q", detail)
	}
	// Still fails when a cap is missed, incomplete or not.
	env.Metrics = []Metric{Size("worktree_free_bytes", 0, "bytes")}
	if ok, _, _ := judge(env, caps, nil); ok {
		t.Error("an incomplete run that misses a cap was judged acceptable")
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
