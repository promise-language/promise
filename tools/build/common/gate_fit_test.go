package common

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
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

// TestFitGate_RefusesWithoutEnvelope: a bare `bin/gate fit` measures nothing
// and says how to ask — a usage error with nothing on stdout. The rest of the
// command-line protocol is TestRunGate_UsageErrorsWriteNothing's.
func TestFitGate_RefusesWithoutEnvelope(t *testing.T) {
	out, err := runGate(t, t.TempDir(), "fit")
	if msg := requireUsageError(t, []string{"fit"}, out, err); !strings.Contains(msg, "--envelope") {
		t.Fatalf("want a refusal naming --envelope, got %q", msg)
	}
}

// The runner appends --envelope and reads one object; the SDK then hands that
// object to the judge. Both filesystems are always reported.
func TestFitGate_EnvelopeIsTheOnlyOutput(t *testing.T) {
	var stdout bytes.Buffer
	if err := runContractGate(t.TempDir(), "fit", &stdout); err != nil {
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
	for _, name := range []string{"worktree_free_bytes", "cache_free_bytes"} {
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
		Target:        "linux/amd64",
		Metrics:       []Metric{Size("worktree_free_bytes", 5, "bytes")},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"gate":"fit","target":"linux/amd64","metrics":[{"name":"worktree_free_bytes","type":"int","value":5,"unit":"bytes"}]}`
	if string(out) != want {
		t.Errorf("wire form\n got %s\nwant %s", out, want)
	}
	var m Metric
	if err := json.Unmarshal([]byte(`{"name":"x","type":"int","value":1.5}`), &m); err == nil {
		t.Error("an int metric with a fractional value must be refused, not absorbed")
	}
}

// TestFitGate_AnswersWhileTheHostScopeIsHeld: fit declares no exclusion and
// takes none, because it must answer on a machine whose holder is wedged —
// that is exactly the machine a runner asks it about.
func TestFitGate_AnswersWhileTheHostScopeIsHeld(t *testing.T) {
	cleanTestHome(t)
	root := t.TempDir()
	unlock, err := acquireWithin(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	done := make(chan error, 1)
	var stdout bytes.Buffer
	go func() { done <- runContractGate(t.TempDir(), "fit", &stdout) }()
	select {
	case err := <-done:
		if err != nil || !strings.Contains(stdout.String(), `"gate":"fit"`) {
			t.Errorf("fit under a held host scope: err = %v, stdout %q", err, stdout.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("fit queued behind the host-scope holder")
	}
}
