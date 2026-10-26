package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// metricInts is metrics by name, for the whole-number ones a test compares.
func metricInts(metrics []Metric) map[string]int64 {
	out := map[string]int64{}
	for _, m := range metrics {
		out[m.Name] = m.Int
	}
	return out
}

// writeTermsInto writes thresholds into root's terms file, beside an empty
// baselines file.
func writeTermsInto(t *testing.T, root, thresholds string) {
	t.Helper()
	for file, body := range map[string]string{ThresholdsFile: thresholds, BaselinesFile: "{}"} {
		path := filepath.Join(root, file)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// completeIntegrationMetrics is every metric integration declares, each 0 but
// promise_failed_tests, which is failed: the envelope of a complete run.
func completeIntegrationMetrics(t *testing.T, root string, failed int) []Metric {
	t.Helper()
	var out []Metric
	for _, s := range declaredMetrics(root, integrationGate) {
		m := Metric{Name: s.Name, Type: s.Type, Unit: s.Unit}
		if s.Name == "promise_failed_tests" {
			m.Int = int64(failed)
		}
		out = append(out, m)
	}
	return out
}

// num is a term bound as the files spell one.
func num(s string) *json.Number { n := json.Number(s); return &n }

// capsOnly is terms carrying only caps.
func capsOnly(caps map[string]capTerm) Terms {
	return Terms{Caps: caps, Baselines: map[string]baselineTerm{}}
}

// writeTermsFiles builds a root carrying both terms files. An empty body leaves
// that file absent, which is a state LoadTerms has an answer for.
func writeTermsFiles(t *testing.T, thresholds, baselines string) string {
	t.Helper()
	root := t.TempDir()
	for file, body := range map[string]string{ThresholdsFile: thresholds, BaselinesFile: baselines} {
		if body == "" {
			continue
		}
		path := filepath.Join(root, file)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// repoTerms is this repository's own terms, read as bin/run reads them.
func repoTerms(t *testing.T) Terms {
	t.Helper()
	terms, err := LoadTerms("../../..")
	if err != nil {
		t.Fatalf("this repository's terms do not load: %v", err)
	}
	return terms
}

// repoTerm is the term this repository applies to name on target.
func repoTerm(t *testing.T, name, target string) appliedTerm {
	t.Helper()
	term, ok := termFor(name, target, repoTerms(t))
	if !ok {
		t.Fatalf("%s carries no term on %s", name, target)
	}
	return term
}

// shippedTargets is every platform this project ships, as an envelope names it.
func shippedTargets() []string {
	out := make([]string, 0, len(requiredPlatforms))
	for _, p := range requiredPlatforms {
		out = append(out, strings.Replace(p, "-", "/", 1))
	}
	return out
}

func TestJudge_AtLeastFloor(t *testing.T) {
	terms := capsOnly(map[string]capTerm{"worktree_free_bytes": {Direction: AtLeast, Cap: "100"}})
	low := Envelope{Gate: "fit", Metrics: []Metric{Size("worktree_free_bytes", 99, unitBytes), Count("other", 1)}}
	v := judge(t.TempDir(), low, terms)
	if v.Acceptable || !strings.Contains(v.Detail, "worktree_free_bytes is 99, cap at_least 100") {
		t.Errorf("below the floor: %+v", v)
	}
	want := map[string]appliedTerm{"worktree_free_bytes": {Direction: AtLeast, Cap: num("100")}}
	if got, _ := json.Marshal(v.Thresholds); string(got) != mustJSON(t, want) {
		t.Errorf("a verdict carries exactly the terms it applied, got %s", got)
	}
	at := Envelope{Gate: "fit", Metrics: []Metric{Size("worktree_free_bytes", 100, unitBytes)}}
	if v := judge(t.TempDir(), at, terms); !v.Acceptable {
		t.Errorf("at the floor must pass: %s", v.Detail)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The other sense a cap can have, and the one this repository reaches for
// first: every count of something being wrong carries an `at_most 0` ceiling.
func TestJudge_AtMostCeiling(t *testing.T) {
	terms := capsOnly(map[string]capTerm{"check_findings": {Direction: AtMost, Cap: "0"}})
	over := Envelope{Gate: "checked:go", Metrics: []Metric{Count("check_findings", 3)}}
	v := judge(t.TempDir(), over, terms)
	if v.Acceptable || !strings.Contains(v.Detail, "check_findings is 3, cap at_most 0") {
		t.Errorf("above the ceiling: %+v", v)
	}
	at := Envelope{Gate: "checked:go", Metrics: []Metric{Count("check_findings", 0)}}
	if v := judge(t.TempDir(), at, terms); !v.Acceptable {
		t.Errorf("at the ceiling must pass: %s", v.Detail)
	}
}

// Every cap and every baseline a metric has is applied, and all must hold: a
// cap looser than the baseline beside it would otherwise forgive exactly the
// regression the ratchet exists to catch (T2192). The verdict carries both.
func TestJudge_CapAndBaselineBothApply(t *testing.T) {
	terms := Terms{
		Caps:      map[string]capTerm{"promise_leaked_tests": {Direction: AtMost, Cap: "10"}},
		Baselines: map[string]baselineTerm{"promise_leaked_tests": {Direction: AtMost, Value: num("3")}},
	}
	env := func(n int) Envelope {
		return Envelope{Gate: "tested:promise", Target: "linux/amd64", Metrics: []Metric{Count("promise_leaked_tests", n)}}
	}
	v := judge(t.TempDir(), env(5), terms)
	if v.Acceptable || !strings.Contains(v.Detail, "promise_leaked_tests is 5, baseline at_most 3") || strings.Contains(v.Detail, "cap at_most 10") {
		t.Errorf("within the loose cap, over the baseline: %+v", v)
	}
	if got := v.Thresholds["promise_leaked_tests"]; got.Cap == nil || got.Baseline == nil {
		t.Errorf("the verdict must carry every term that applied, got %s", mustJSON(t, got))
	}
	v = judge(t.TempDir(), env(11), terms)
	if v.Acceptable || !strings.Contains(v.Detail, "cap at_most 10 and baseline at_most 3") {
		t.Errorf("over both: %+v", v)
	}
	if v := judge(t.TempDir(), env(2), terms); !v.Acceptable {
		t.Errorf("within both terms must pass: %s", v.Detail)
	}
}

// A baseline given per target judges only the envelope that names its target:
// a figure measured on one machine cannot judge another.
func TestJudge_ABaselineByTargetJudgesOnlyThatTarget(t *testing.T) {
	terms := Terms{Caps: map[string]capTerm{}, Baselines: map[string]baselineTerm{
		"promise_test_count": {Direction: AtLeast, Targets: map[string]json.Number{"linux/amd64": "10"}},
	}}
	for target, judged := range map[string]bool{"linux/amd64": true, "darwin/arm64": false} {
		env := Envelope{Gate: "tested:promise", Target: target, Metrics: []Metric{Count("promise_test_count", 5)}}
		v := judge(t.TempDir(), env, terms)
		if _, applied := v.Thresholds["promise_test_count"]; applied != judged {
			t.Errorf("%s: judged = %v, want %v", target, applied, judged)
		}
		if v.Acceptable == judged {
			t.Errorf("%s: acceptable = %v (detail %q)", target, v.Acceptable, v.Detail)
		}
	}
}

// An integer metric is compared as an integer: widened to a float, a count one
// past 2^53 would round into compliance with a cap at 2^53.
func TestJudge_AnIntegerIsComparedAsAnInteger(t *testing.T) {
	terms := capsOnly(map[string]capTerm{"cas_network_bytes": {Direction: AtMost, Cap: "9007199254740992"}})
	env := Envelope{Gate: "builds", Metrics: []Metric{Size("cas_network_bytes", 9007199254740993, unitBytes)}}
	if v := judge(t.TempDir(), env, terms); v.Acceptable {
		t.Errorf("2^53+1 passed a cap of 2^53: %s", v.Detail)
	}
}

// An incomplete run is NOT acceptable, however good the numbers it did report
// are, and the verdict relays the reason whole, so the reader is sent at the gap
// rather than at the code.
func TestJudge_IncompleteIsNotAcceptable(t *testing.T) {
	terms := capsOnly(map[string]capTerm{"worktree_free_bytes": {Direction: AtLeast, Cap: "1"}})
	reason := "the Go build cache location is unknown, so only the worktree filesystem was measured: no GOCACHE"
	env := Envelope{Gate: "fit", Metrics: []Metric{Size("worktree_free_bytes", 50, unitBytes)}, Incomplete: reason}
	v := judge(t.TempDir(), env, terms)
	if v.Acceptable || !strings.Contains(v.Detail, reason) {
		t.Errorf("an incomplete run: %+v", v)
	}
	if !strings.Contains(v.Detail, contractGates["fit"].remedy) && !strings.Contains(v.Detail, capitalize(contractGates["fit"].remedy)) {
		t.Errorf("the detail must name fit's remedy, got %q", v.Detail)
	}
}

// A refusal says three things: the judgement, the evidence — each member that
// did not pass, in the tool's own words, up to maxEvidence and a count of the
// rest — and the gate's remedy.
func TestJudge_ARefusalCarriesEvidenceAndRemedy(t *testing.T) {
	terms := capsOnly(map[string]capTerm{"go_failed_tests": {Direction: AtMost, Cap: "0"}})
	var members []Member
	for i := range maxEvidence + 2 {
		members = append(members, newMember(fmt.Sprintf("TestLower%d", i), OutcomeFail, "lower_test.go:41: got i32, want i64\nsecond line"))
	}
	members = append(members, newMember("TestFine", OutcomePass, ""), newMember("TestSkipped", OutcomeExcluded, ""))
	env := Envelope{Gate: "tested:go", Metrics: []Metric{Count("go_failed_tests", len(members)-2)},
		Groups: []Group{{Name: "compiler/internal/codegen", Members: members}}}
	v := judge(t.TempDir(), env, terms)
	for _, want := range []string{
		"go_failed_tests is 7, cap at_most 0.",
		"compiler/internal/codegen: TestLower0 (fail)\n    lower_test.go:41: got i32, want i64\n    second line",
		"… and 2 more that did not pass.",
		capitalize(contractGates["tested:go"].remedy),
	} {
		if !strings.Contains(v.Detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, v.Detail)
		}
	}
	for _, not := range []string{"TestFine", "TestSkipped", fmt.Sprintf("TestLower%d", maxEvidence)} {
		if strings.Contains(v.Detail, not) {
			t.Errorf("detail names %s, which it should not:\n%s", not, v.Detail)
		}
	}
}

// With no member to show, the detail says the gate gave no reason rather than
// leaving the reader to wonder whether evidence was dropped.
func TestJudge_NoEvidenceSaysSo(t *testing.T) {
	terms := capsOnly(map[string]capTerm{"build_failures": {Direction: AtMost, Cap: "0"}})
	env := Envelope{Gate: "builds", Metrics: []Metric{Count("build_failures", 1)}}
	if v := judge(t.TempDir(), env, terms); !strings.Contains(v.Detail, "build_failures is 1, cap at_most 0, and the gate reported nothing about why.") {
		t.Errorf("detail = %q", v.Detail)
	}
	// An incomplete run's reason is the gate's account of why: the note stays
	// with the numbers, and the reason is relayed whole after them.
	env.Incomplete = "the build did not complete"
	v := judge(t.TempDir(), env, terms)
	if !strings.Contains(v.Detail, "build_failures is 1, cap at_most 0, and the gate reported nothing about why.\n") ||
		!strings.Contains(v.Detail, ": the build did not complete.\n") {
		t.Errorf("detail = %q", v.Detail)
	}
}

// A composition's remedy is that of the leaves declaring what failed, and no
// other: a failing Go test is not fixed by running the formatter.
func TestJudge_ACompositionNamesTheRemedyOfWhatFailed(t *testing.T) {
	terms := capsOnly(map[string]capTerm{
		"go_failed_tests":   {Direction: AtMost, Cap: "0"},
		"unformatted_files": {Direction: AtMost, Cap: "0"},
		"cas_home_count":    {Direction: AtMost, Cap: "1"},
	})
	env := Envelope{Gate: integrationGate, Metrics: []Metric{Count("go_failed_tests", 1), Count("unformatted_files", 0), Count("cas_home_count", 3)}}
	v := judge("../../..", env, terms)
	if !strings.Contains(v.Detail, capitalize(contractGates["tested:go"].remedy)) {
		t.Errorf("detail lacks tested:go's remedy:\n%s", v.Detail)
	}
	if strings.Contains(v.Detail, contractGates["formatted:go"].remedy) {
		t.Errorf("detail names the formatter's remedy for a failing test:\n%s", v.Detail)
	}
	if !strings.Contains(v.Detail, "`bin/run integration` re-measures") {
		t.Errorf("a store metric's remedy must name the gate that re-measures it:\n%s", v.Detail)
	}
}

// Every leaf this project provides states a remedy naming the command that
// re-measures it, so no refusal can end without one.
func TestEveryLeafStatesItsRemedy(t *testing.T) {
	for _, name := range ContractGateNames() {
		def := contractGates[name]
		if def.measure == nil {
			continue
		}
		if !strings.Contains(def.remedy, "`bin/run "+name+"` re-measures") {
			t.Errorf("%s's remedy %q does not name `bin/run %s` as what re-measures it", name, def.remedy, name)
		}
	}
}

// Every gate driven into a failure on a metric it declares reaches a refusal
// that names the metric, the evidence it carried, and its remedy. Driving each
// measurement itself into its failure is the gate tests' (gate_*_test.go); this
// holds the verdict to the same three things for every gate at once.
func TestEveryGateRefusalNamesFindingEvidenceAndRemedy(t *testing.T) {
	for _, name := range ContractGateNames() {
		t.Run(name, func(t *testing.T) {
			for _, s := range declaredMetrics("../../..", name) {
				if s.Type != MetricInt {
					continue
				}
				terms := capsOnly(map[string]capTerm{s.Name: {Direction: AtMost, Cap: "0"}})
				env := Envelope{Gate: name, Metrics: []Metric{{Name: s.Name, Type: s.Type, Unit: s.Unit, Int: 4}},
					Groups: []Group{{Name: "the/group", Members: []Member{newMember("the-member", OutcomeFail, "what the tool said")}}}}
				v := judge("../../..", env, terms)
				for _, want := range []string{s.Name + " is 4, cap at_most 0", "the/group: the-member (fail)\n    what the tool said", "re-measures"} {
					if !strings.Contains(v.Detail, want) {
						t.Errorf("%s failing on %s: detail lacks %q:\n%s", name, s.Name, want, v.Detail)
					}
				}
				return
			}
		})
	}
}

// The verdict's wire form: acceptable, an object of applied terms — never null
// — and a detail.
func TestJudgeStdin_VerdictShape(t *testing.T) {
	root := writeTermsFiles(t, `{"worktree_free_bytes": {"direction": "at_least", "cap": 10}}`, `{}`)
	env := fitEnvelope(5)
	var out bytes.Buffer
	if err := JudgeStdin(root, "fit", strings.NewReader(env), &out); err != nil {
		t.Fatalf("JudgeStdin: %v", err)
	}
	obj := decodeOneObject(t, out.Bytes())
	if obj["acceptable"] != false || obj["detail"] == "" {
		t.Errorf("verdict = %v", obj)
	}
	th, _ := obj["thresholds"].(map[string]any)
	if got := mustJSON(t, th["worktree_free_bytes"]); got != `{"cap":10,"direction":"at_least"}` {
		t.Errorf("worktree_free_bytes term = %s, want the applied cap", got)
	}
}

// fitEnvelope is a complete fit envelope with the given worktree figure.
func fitEnvelope(worktree int64) string {
	metrics := []Metric{Size("worktree_free_bytes", worktree, unitBytes)}
	for _, s := range fitMetricSpecs()[1:] {
		metrics = append(metrics, Metric{Name: s.Name, Type: s.Type, Unit: s.Unit, Int: 1 << 40})
	}
	for i, m := range metrics {
		if m.Name == unlinkMetricName {
			metrics[i].Int = 0
		}
	}
	b, _ := json.Marshal(Envelope{SchemaVersion: EnvelopeSchemaVersion, Gate: "fit", Target: HostTarget(), Metrics: metrics})
	return string(b)
}

// Each of these is a measurement nobody can judge, and the judge says so by
// writing nothing: a caller reads one verdict or none.
func TestJudgeStdin_AnUnjudgeableEnvelopePrintsNothing(t *testing.T) {
	root := writeTermsFiles(t, `{"worktree_free_bytes": {"direction": "at_least", "cap": 10}}`, `{}`)
	for name, tc := range map[string]struct{ gate, in, want string }{
		"not JSON":             {"fit", "hello", "not an envelope"},
		"no schema_version":    {"fit", `{"gate":"fit","metrics":[]}`, "no schema_version"},
		"another version":      {"fit", `{"schema_version":2,"gate":"fit","metrics":[]}`, "schema_version 2"},
		"another gate":         {"fit", `{"schema_version":1,"gate":"tested","metrics":[]}`, `"tested"`},
		"unknown gate":         {"nosuchgate", `{"schema_version":1,"gate":"nosuchgate","metrics":[]}`, "no gate named"},
		"a metric twice":       {"fit", `{"schema_version":1,"gate":"fit","metrics":[{"name":"worktree_free_bytes","type":"int","value":1,"unit":"bytes"},{"name":"worktree_free_bytes","type":"int","value":1,"unit":"bytes"}]}`, "twice"},
		"another unit":         {"fit", `{"schema_version":1,"gate":"fit","metrics":[{"name":"worktree_free_bytes","type":"int","value":1}]}`, "declares as int in bytes"},
		"complete and lacking": {"fit", `{"schema_version":1,"gate":"fit","target":"x/y","metrics":[]}`, "lacks"},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := JudgeStdin(root, tc.gate, strings.NewReader(tc.in), &out)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one naming %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("an unjudgeable envelope printed %q", out.String())
			}
		})
	}
}

// A complete envelope none of whose metrics carries a term is not judged, and
// an unjudged measurement is not a pass: no verdict at all is written for it.
func TestJudgeStdin_NothingTermedIsNoVerdict(t *testing.T) {
	root := writeTermsFiles(t, `{}`, `{}`)
	var out bytes.Buffer
	if err := JudgeStdin(root, "fit", strings.NewReader(fitEnvelope(5)), &out); err == nil || out.Len() != 0 {
		t.Errorf("err = %v, stdout %q; want no verdict", err, out.String())
	}
}

// The terms files are read strictly: every defect is refused, naming the file
// and the entry, and nothing is judged from a file the judge cannot trust.
func TestLoadTerms_RefusesEveryDefect(t *testing.T) {
	for name, tc := range map[string]struct{ thresholds, baselines, want string }{
		"unknown key":       {`{"check_findings": {"direction": "at_most", "cap": 0, "updated": "x"}}`, `{}`, `"check_findings" has an unknown key "updated"`},
		"missing cap":       {`{"check_findings": {"direction": "at_most"}}`, `{}`, `"check_findings" is missing "cap"`},
		"unknown direction": {`{"check_findings": {"direction": "down", "cap": 0}}`, `{}`, "unknown direction"},
		"undeclared metric": {`{"vet_findings": {"direction": "at_most", "cap": 0}}`, `{}`, `"vet_findings" is a metric no gate declares`},
		"value and targets": {`{}`, `{"go_failed_tests": {"direction": "at_most", "value": 0, "targets": {"linux/amd64": 0}}}`, `both "value" and "targets"`},
		"neither":           {`{}`, `{"go_failed_tests": {"direction": "at_most"}}`, `neither "value" nor "targets"`},
		"not a target":      {`{}`, `{"go_failed_tests": {"direction": "at_most", "targets": {"linux-amd64": 0}}}`, "not an os/arch target"},
		"not a number":      {`{"check_findings": {"direction": "at_most", "cap": "0"}}`, `{}`, "is not a number"},
		"two directions":    {`{"check_findings": {"direction": "at_most", "cap": 0}}`, `{"check_findings": {"direction": "at_least", "value": 0}}`, "one metric has one direction"},
		"no thresholds":     {"", `{}`, "thresholds.json"},
		"no baselines":      {`{}`, "", "baselines.json"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadTerms(writeTermsFiles(t, tc.thresholds, tc.baselines))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// The platform-first baselines file the commit guard still requires
// (workspace#490) is read through the contract's conversion: each platform's
// figure is that target's, up is at_least, down and exact 0 are at_most, a
// retired name judges the metric it became, and informational and pending
// entries are no term.
func TestLoadTerms_ReadsThePlatformFirstBaselines(t *testing.T) {
	root := writeTermsFiles(t, `{}`, `{
		"linux-amd64":  {"host_test_count": {"value": 10, "direction": "up", "updated": "2026-01-01"},
		                 "vet_findings":    {"value": 0, "direction": "exact"},
		                 "cas_home_count":  {"value": 1, "direction": "down"},
		                 "go_coverage_pct": {"direction": "up"},
		                 "host_excluded_count": {"type": "informational"}},
		"darwin-arm64": {"host_test_count": {"value": 12, "direction": "up"}}
	}`)
	terms, err := LoadTerms(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"promise_test_count": `{"Direction":"at_least","Value":null,"Targets":{"darwin/arm64":12,"linux/amd64":10}}`,
		"go_check_findings":  `{"Direction":"at_most","Value":null,"Targets":{"linux/amd64":0}}`,
		"cas_home_count":     `{"Direction":"at_most","Value":null,"Targets":{"linux/amd64":1}}`,
	}
	if len(terms.Baselines) != len(want) {
		t.Errorf("baselines = %s, want exactly %v", mustJSON(t, terms.Baselines), want)
	}
	for name, w := range want {
		if got := mustJSON(t, terms.Baselines[name]); got != w {
			t.Errorf("%s = %s, want %s", name, got, w)
		}
	}
	for name, tc := range map[string]struct{ body, want string }{
		"undeclared":     {`{"linux-amd64": {"nosuch_metric": {"value": 0, "direction": "down"}}}`, "linux-amd64/nosuch_metric is a metric no gate declares"},
		"unknown key":    {`{"linux-amd64": {"cas_home_count": {"value": 1, "direction": "down", "floor": 2}}}`, `unknown key "floor"`},
		"exact above 0":  {`{"linux-amd64": {"cas_home_count": {"value": 1, "direction": "exact"}}}`, "exact on a non-zero value"},
		"two directions": {`{"linux-amd64": {"cas_home_count": {"value": 1, "direction": "down"}}, "darwin-arm64": {"cas_home_count": {"value": 1, "direction": "up"}}}`, "one metric has one direction"},
	} {
		if _, err := LoadTerms(writeTermsFiles(t, `{}`, tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", name, err, tc.want)
		}
	}
}

// Every retired name the platform-first file carries maps to a metric some
// gate declares, or to none: a mapping onto a name nothing reports would judge
// nothing while reading as a term.
func TestRetiredBaselineNames_NameDeclaredMetrics(t *testing.T) {
	declared := map[string]bool{}
	for _, g := range ContractGateNames() {
		for _, s := range declaredMetrics("../../..", g) {
			declared[s.Name] = true
		}
	}
	for old, metric := range retiredBaselineNames {
		if metric != "" && !declared[metric] {
			t.Errorf("%s is read as %s, which no gate declares", old, metric)
		}
		if declared[old] {
			t.Errorf("%s is retired, but a gate still declares it", old)
		}
	}
}

// An invalid terms file is no verdict either: the judge exits having written
// nothing, naming the file and the entry.
func TestJudgeStdin_AnInvalidTermsFileIsNoVerdict(t *testing.T) {
	root := writeTermsFiles(t, `{"worktree_free_bytes": {"direction": "at_least", "cap": 10, "extra": 1}}`, `{}`)
	var out bytes.Buffer
	err := JudgeStdin(root, "fit", strings.NewReader(fitEnvelope(5)), &out)
	if err == nil || !strings.Contains(err.Error(), "thresholds.json") || !strings.Contains(err.Error(), `"extra"`) || out.Len() != 0 {
		t.Errorf("err = %v, stdout %q; want a refusal naming the file and the entry, and nothing written", err, out.String())
	}
}

// --- This repository's own terms ---

// integrationUnjudged names the metrics `integration` reports that carry no
// term of their own, each with the reason. An unexplained omission and an
// overlooked one look identical in a list of strings, and the second is exactly
// how T2087 happened: go_test_failures was seeded on two targets of four.
var integrationUnjudged = map[string]string{
	"test_count":          "a suite's size is not a quality of the change; promise_test_count ratchets where a target carries a figure, and deleting a test is sometimes the right change.",
	"go_test_count":       "a suite's size is not a quality of the change, and nothing has seeded a figure for it.",
	"promise_test_count":  "a suite's size ratchets `at_least` where a target carries a figure, but requiring that everywhere would fail a target for deleting a test.",
	"promise_check_units": "how many units the checker was given is not a quality of the change — merging two files into a module lowers it without checking any less.",
	"failed_tests":        "the total of go_failed_tests and promise_failed_tests, each capped at zero: a shared cap would also judge the wasm suites on targets that do not run them.",
	"failed_packages":     "the total of go_failed_packages, which is capped at zero.",
	"leaked_tests":        "the total of promise_leaked_tests, which is capped at zero.",
}

// integrationCosts are the integration metrics that are judged but do NOT count
// something being wrong: they are what a run COST, so they ratchet instead of
// being capped.
var integrationCosts = map[string]string{
	"cas_network_bytes": "bytes a run pulled over the wire into the store — it SHOULD be zero (T2150, T2153), but an expensive run is not a defect in whichever change measured it.",
	"cas_home_count":    "how many private PROMISE_HOMEs a run materialized; the floor is a fact about how the suites are sandboxed, not a count of anything wrong.",
}

// Every metric integration reports carries a term on every platform this
// project ships, or a written reason not to. A term is per-target data, so a
// check on the host alone is blind to the platform its author is not sitting
// on, and the gap lands as a red trunk for the next person there.
func TestIntegration_EveryMetricIsJudgedOnEveryShippedTarget(t *testing.T) {
	terms := repoTerms(t)
	for _, s := range declaredMetrics("../../..", integrationGate) {
		if integrationUnjudged[s.Name] != "" {
			continue
		}
		for _, target := range shippedTargets() {
			if _, ok := termFor(s.Name, target, terms); !ok {
				t.Errorf("%s carries no term on %s, so nothing judges it there — give it a cap in %s or a baseline in %s, or say in integrationUnjudged why not",
					s.Name, target, ThresholdsFile, BaselinesFile)
			}
		}
	}
	for name := range integrationUnjudged {
		if !slices.ContainsFunc(declaredMetrics("../../..", integrationGate), func(s MetricSpec) bool { return s.Name == name }) {
			t.Errorf("integrationUnjudged excuses %q, which integration does not report — the excuse outlived the metric", name)
		}
	}
}

// Every other integration metric counts something being WRONG, and the only
// correct value for such a count is zero: its term is an `at_most 0` cap, never
// a ratchet from a non-zero figure, which would be a standing allowance (T2192).
// Nothing guards thresholds.json the way the commit gate guards baselines, so
// this test is what makes weakening a cap a visible edit.
func TestCorrectnessMetricsAreCappedAtZero(t *testing.T) {
	terms := repoTerms(t)
	for _, s := range declaredMetrics("../../..", integrationGate) {
		if integrationUnjudged[s.Name] != "" || integrationCosts[s.Name] != "" {
			continue
		}
		c, ok := terms.Caps[s.Name]
		if !ok {
			// A toolchain's share of a shared count is capped by its total.
			for _, tc := range []string{"go_", "promise_"} {
				if shared, cut := strings.CutPrefix(s.Name, tc); cut {
					c, ok = terms.Caps[shared]
				}
			}
		}
		if !ok || c.Direction != AtMost || c.Cap != "0" {
			t.Errorf("%s = %+v (capped %v), want an at_most 0 cap on it or on its shared total", s.Name, c, ok)
		}
	}
}

// fit's floors are caps: they are absolutes a person edits, not numbers that
// ratchet with history. A filesystem that keeps a removed name is unfit
// outright: there is no amount of it a suite tolerates.
func TestFit_FloorsAreCaps(t *testing.T) {
	terms := repoTerms(t)
	for _, name := range []string{"worktree_free_bytes", "cache_free_bytes"} {
		if c, ok := terms.Caps[name]; !ok || c.Direction != AtLeast || c.Cap == "0" {
			t.Errorf("%s = %+v, want a positive at_least floor", name, c)
		}
	}
	if c, ok := terms.Caps[unlinkMetricName]; !ok || c.Direction != AtMost || c.Cap != "0" {
		t.Errorf("%s = %+v, want at_most 0", unlinkMetricName, c)
	}
}

// A baseline given per target names only platforms this project ships: a block
// for one nobody ships is terms no run will ever read.
func TestBaselines_TargetOnlyShippedPlatforms(t *testing.T) {
	for name, b := range repoTerms(t).Baselines {
		for target := range b.Targets {
			if !slices.Contains(shippedTargets(), target) {
				t.Errorf("%s carries a baseline for %s, which this project does not ship", name, target)
			}
		}
	}
}

// The by-hand mode renders each metric beside every term it carries, then the
// verdict, so a person sees what each number was judged against.
func TestRenderVerdict_ShowsEveryTerm(t *testing.T) {
	terms := Terms{
		Caps:      map[string]capTerm{"promise_leaked_tests": {Direction: AtMost, Cap: "10"}},
		Baselines: map[string]baselineTerm{"promise_leaked_tests": {Direction: AtMost, Value: num("3")}},
	}
	env := Envelope{Gate: "tested:promise", Metrics: []Metric{Count("promise_leaked_tests", 5), Count("promise_test_count", 9)}}
	got := renderVerdict(env, judge(t.TempDir(), env, terms))
	for _, want := range []string{"cap at_most 10, baseline at_most 3", "✗", "not judged", "not acceptable:"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered verdict lacks %q:\n%s", want, got)
		}
	}
}
