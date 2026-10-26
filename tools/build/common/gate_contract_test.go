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

// `bin/gate --list` is how an orchestrator learns what this project measures,
// so it must print names and nothing else, and the SDK's two required gates
// must be among them or `claim`, `run-step` and `resolve` all refuse.
func TestGateList_PrintsGateNames(t *testing.T) {
	var out bytes.Buffer
	if err := writeGateList(&out, t.TempDir(), ModeHuman); err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(out.String())
	if !slices.Equal(got, ContractGateNames()) {
		t.Errorf("--list printed %v, want %v", got, ContractGateNames())
	}
	for _, required := range []string{"fit", "integration"} {
		if !slices.Contains(got, required) {
			t.Errorf("--list omits %q, which the SDK requires", required)
		}
	}
	// Only gates. A subcommand that measures nothing is not one.
	for _, notAGate := range []string{"schema", "list", "--list"} {
		if slices.Contains(got, notAGate) {
			t.Errorf("--list names %q, which is not a gate", notAGate)
		}
	}
}

// Everything `bin/gate --list` names must be runnable through `bin/run`, or
// the list claims a gate the machine cannot run — the failure every caller
// trusting the list discovers mid-item. Both read one registry; this is what
// keeps a name from being added to only one of them.
func TestGateList_EveryListedGateIsRunnable(t *testing.T) {
	run := runSpec("../../..")
	for _, name := range ContractGateNames() {
		inv, err := ParseInvocation(run, []string{name, "--verdict"})
		if err != nil || inv.Command != name || !inv.Flags["verdict"] || run.PassThrough(name) {
			t.Errorf("bin/gate lists %q but bin/run does not read it as a gate to judge: %+v, %v", name, inv, err)
			continue
		}
		def := contractGates[name]
		if def.measure == nil && len(def.parts) == 0 {
			t.Errorf("%q is listed but neither measures nor composes, so asking for it measures nothing", name)
		}
		if inv, err := ParseInvocation(gateSpec, []string{name, "--envelope"}); err != nil || !inv.Flags["envelope"] {
			t.Errorf("bin/gate lists %q but refuses to measure it: %v", name, err)
		}
	}
}

// A refusal quotes the spelling the caller typed. Normalising first and then
// interpolating the result tells someone who typed `--json` to try `-json`,
// correcting them in a spelling they did not use.
func TestParse_RefusalQuotesTheTypedSpelling(t *testing.T) {
	if _, err := ParseInvocation(gateSpec, []string{"fit", "--verbose"}); err == nil || !strings.Contains(err.Error(), "--verbose") {
		t.Errorf("gate: got %v, want the typed --verbose", err)
	}
	if _, err := ParseInvocation(runSpec("../../.."), []string{"fit", "--bogus"}); err == nil || !strings.Contains(err.Error(), "--bogus") {
		t.Errorf("run: got %v, want the typed --bogus", err)
	}
	// Both spellings still work, so the fix is about the message only.
	for _, spelling := range [][]string{{"fit", "--envelope"}, {"fit", "-envelope"}} {
		if inv, err := ParseInvocation(gateSpec, spelling); err != nil || !inv.Flags["envelope"] {
			t.Errorf("ParseInvocation(%q) = %+v, %v", spelling, inv, err)
		}
	}
}

// The listing is read by flow's discovery, which unmarshals each entry into a
// struct with a `name` field. This test therefore reads it the way the SDK
// does — objects, not strings — because the failure the wrong shape causes is
// silent: a list of bare names parses, yields zero named gates, and the
// repository is discovered as a machine with no gates.
func TestGateList_JSON(t *testing.T) {
	var out bytes.Buffer
	if err := writeGateList(&out, "../../..", ModeJSON); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Gates []struct {
			Name      string            `json:"name"`
			Summary   string            `json:"summary"`
			Metrics   []MetricSpec      `json:"metrics"`
			Requires  []json.RawMessage `json:"requires"`
			Serialize string            `json:"serialize"`
		} `json:"gates"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("--list --json is not one JSON object: %v\n%s", err, out.String())
	}
	names := make([]string, 0, len(got.Gates))
	for _, g := range got.Gates {
		names = append(names, g.Name)
		if g.Summary != ContractGateSummary(g.Name) {
			t.Errorf("gate %q summary = %q, want %q", g.Name, g.Summary, ContractGateSummary(g.Name))
		}
		if g.Summary == "" {
			t.Errorf("gate %q lists no summary", g.Name)
		}
		// requires is present and empty, never absent: a row without it reads
		// as a gate that has not said what it needs.
		if g.Requires == nil || len(g.Requires) != 0 {
			t.Errorf("gate %q requires = %v, want []", g.Name, g.Requires)
		}
		if !slices.Equal(g.Metrics, declaredMetrics("../../..", g.Name)) {
			t.Errorf("gate %q lists metrics %v, want its declaration %v", g.Name, g.Metrics, declaredMetrics("../../..", g.Name))
		}
		if g.Serialize != gateSerialize(g.Name) {
			t.Errorf("gate %q serialize = %q, want %q", g.Name, g.Serialize, gateSerialize(g.Name))
		}
	}
	if gateSerialize(integrationGate) != serializeHost || gateSerialize("fit") != "" {
		t.Errorf("integration must hold the host scope and fit must not")
	}
	if !slices.Equal(names, ContractGateNames()) {
		t.Errorf("gates = %v, want %v", names, ContractGateNames())
	}
}

// bin/run must be able to run any gate bin/gate lists, so both read one
// registry. It also reports the commands, which bin/gate knows nothing about.
func TestRunList_GatesMatchGateListAndCommands(t *testing.T) {
	var out bytes.Buffer
	if err := writeRunList("../../..", &out, ModeJSON); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Gates    []string    `json:"gates"`
		Commands listedNames `json:"commands"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("bin/run --list --json is not one JSON object: %v\n%s", err, out.String())
	}
	if !slices.Equal(got.Gates, ContractGateNames()) {
		t.Errorf("bin/run lists gates %v, bin/gate lists %v — they must not drift", got.Gates, ContractGateNames())
	}
	for _, g := range got.Gates {
		if !IsContractGate(g) {
			t.Errorf("bin/run lists %q, which bin/run cannot run", g)
		}
	}
	if !slices.Contains(got.Commands, "verify") {
		t.Errorf("commands = %v, want verify (the one command a flow requires)", got.Commands)
	}
	// The commands are what ./make BUILDS, every one of them — the workspace
	// reads this list to refuse installing over a project tool and to decide
	// which recorded names it may delete, so a name missing here is a tool it
	// will silently overwrite or remove.
	for _, c := range got.Commands {
		if _, err := os.Stat(filepath.Join("../../..", "tools", "build", "cmd", c)); err != nil {
			t.Errorf("commands include %q, which ./make does not build: %v", c, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join("../../..", "tools", "build", "cmd"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == metaBuilderName {
			continue
		}
		if !slices.Contains(got.Commands, e.Name()) {
			t.Errorf("./make builds %q and --list does not report it — the workspace would overwrite or delete bin/%s", e.Name(), e.Name())
		}
	}
	// One namespace: `bin/run <name>` dispatches commands and gates alike.
	if both := CommandGateCollisions("../../.."); len(both) > 0 {
		t.Errorf("%v are both a command and a gate, so `bin/run %s` means one of two things", both, both[0])
	}
}

// listedNames reads the `commands` half of `bin/run --list --json` the way the
// tool contract writes it: every element a row naming one command. A bare
// string is no longer a row.
type listedNames []string

func (n *listedNames) UnmarshalJSON(data []byte) error {
	var elems []json.RawMessage
	if err := json.Unmarshal(data, &elems); err != nil {
		return err
	}
	for i, elem := range elems {
		var row struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(elem, &row); err != nil {
			return fmt.Errorf("commands[%d] is %s, not a row naming a command", i, elem)
		}
		if row.Name == "" {
			return fmt.Errorf("commands[%d] names nothing", i)
		}
		*n = append(*n, row.Name)
	}
	return nil
}

// verify declares the host-scope exclusion on its row of the JSON listing,
// exactly as flow's DiscoverCommands reads it, so a runner takes the exclusion
// before spawning verify and counts the queue as waiting, outside the run's
// allowance (#96). Every other command's row names it and declares nothing.
// The text listing is for a person and carries names only.
func TestRunList_VerifyDeclaresHostScope(t *testing.T) {
	var out bytes.Buffer
	if err := writeRunList("../../..", &out, ModeJSON); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Commands []json.RawMessage `json:"commands"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("bin/run --list --json is not one JSON object: %v\n%s", err, out.String())
	}
	var sawVerify bool
	for _, raw := range got.Commands {
		var row commandRow
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Errorf("command %s is not a row: %v", raw, err)
			continue
		}
		want := fmt.Sprintf(`{"name":%q}`, row.Name)
		if row.Name == "verify" {
			want, sawVerify = `{"name":"verify","serialize":"host"}`, true
		}
		if string(raw) != want {
			t.Errorf("command row = %s, want %s — only verify declares an exclusion", raw, want)
		}
	}
	if !sawVerify {
		t.Errorf("no verify row in %s", out.String())
	}

	out.Reset()
	if err := writeRunList("../../..", &out, ModeHuman); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "\n  verify\n") || strings.Contains(out.String(), "serialize") {
		t.Errorf("the text listing must name verify and nothing else about it:\n%s", out.String())
	}

	// An empty command set is an empty list, never null.
	if rows, err := json.Marshal(commandRows(nil)); err != nil || string(rows) != "[]" {
		t.Errorf("commandRows(nil) = %s, %v; want []", rows, err)
	}
}

// integration is a composition, and its parts stay separately runnable: that is
// what lets a step fixing one area re-run that area instead of the whole set.
func TestIntegration_PartsAreSeparatelyRunnable(t *testing.T) {
	parts := contractGates["integration"].parts
	if len(parts) == 0 {
		t.Fatal("integration measures directly; it must be a composition of addressable parts")
	}
	// A part is addressable, and either measures or divides further — a concept
	// with instances (checked → checked:go, checked:promise) is still a part a
	// step can ask for on its own.
	var addressable func(name string, depth int)
	addressable = func(name string, depth int) {
		if !IsContractGate(name) {
			t.Errorf("%q is named as a part but cannot be asked for on its own", name)
			return
		}
		def := contractGates[name]
		switch {
		case def.measure != nil:
		case len(def.parts) > 0 && depth < 4:
			for _, sub := range def.parts {
				addressable(sub, depth+1)
			}
		default:
			t.Errorf("part %q neither measures nor divides", name)
		}
	}
	for _, p := range parts {
		addressable(p, 0)
	}
	if contractGates["integration"].measure != nil {
		t.Error("integration both composes and measures; the whole must not disagree with its parts")
	}
	// fit is not part of it: a machine that cannot build is not a change that
	// may not land.
	if slices.Contains(parts, "fit") {
		t.Error("fit is part of integration; it measures the machine, not the change")
	}
}

// A composition measures its parts by recursion, so a cycle in the registry
// would be a stack overflow at the point of measurement rather than a mistake
// anyone could see. The registry is a literal, so the cycle can only arrive by
// edit — which is exactly what this catches, here rather than there.
func TestContractGates_PartGraphTerminates(t *testing.T) {
	var walk func(name string, seen []string)
	walk = func(name string, seen []string) {
		for _, s := range seen {
			if s == name {
				t.Fatalf("the part graph cycles: %v -> %s", seen, name)
			}
		}
		for _, part := range contractGates[name].parts {
			if !IsContractGate(part) {
				t.Errorf("%q names part %q, which is not a gate", name, part)
				continue
			}
			walk(part, append(seen, name))
		}
	}
	for _, name := range ContractGateNames() {
		walk(name, nil)
	}
}

// --- Parts, and the tree a measurement speaks for ---

// integrationLeaves is what `integration` expands to, in the order a reader of
// the summary sees it. Spelled here so a change to the composition has to be a
// change to this list too — a part silently added or dropped is a row a reader
// of bin/verify's summary would never miss.
var integrationLeaves = []string{
	"formatted:go", "formatted:promise", "builds",
	"checked:go", "checked:promise", "tested:go", "tested:promise",
}

// TestMeasureContractGateParts_OnePartPerLeafInOrder pins the detail bin/verify
// renders its summary from: one PartResult per LEAF gate, named exactly as
// `bin/run` addresses it.
func TestMeasureContractGateParts_OnePartPerLeafInOrder(t *testing.T) {
	stubGateBuild(t, buildFails())
	_, parts, err := MeasureContractGateParts(t.TempDir(), "integration")
	if err != nil {
		t.Fatalf("integration: %v", err)
	}
	var got []string
	for _, p := range parts {
		got = append(got, p.Gate)
	}
	if !slices.Equal(got, integrationLeaves) {
		t.Errorf("parts = %v, want %v", got, integrationLeaves)
	}
	for _, name := range got {
		if !IsContractGate(name) {
			t.Errorf("part %q is not a gate anyone can run — a summary row must be a command", name)
		}
	}
}

// TestMeasureContractGateParts_AgreeWithTheEnvelope: the parts are a VIEW of the
// same measurement, not a second one. If they could disagree, bin/verify's
// summary would describe a run other than the one it was judged on — which is
// the whole defect T2170 is about, reintroduced one level down.
func TestMeasureContractGateParts_AgreeWithTheEnvelope(t *testing.T) {
	stubGateBuild(t, buildFails())
	root := t.TempDir()
	env, parts, err := MeasureContractGateParts(root, "integration")
	if err != nil {
		t.Fatalf("integration: %v", err)
	}
	var fromParts []Metric
	var reasons int
	for _, p := range parts {
		fromParts = append(fromParts, p.Metrics...)
		if p.Incomplete != "" {
			reasons++
		}
	}
	if len(fromParts) != len(env.Metrics) {
		t.Fatalf("parts carry %d metrics, the envelope %d", len(fromParts), len(env.Metrics))
	}
	for i := range fromParts {
		if fromParts[i] != env.Metrics[i] {
			t.Errorf("metric %d: part has %+v, envelope has %+v", i, fromParts[i], env.Metrics[i])
		}
	}
	if reasons == 0 {
		t.Error("a failing build must leave a reason on the parts that could not measure")
	}

	// And the wrapper every other caller uses reports the same envelope.
	stubGateBuild(t, buildFails())
	plain, err := MeasureContractGate(root, "integration")
	if err != nil {
		t.Fatalf("integration: %v", err)
	}
	if len(plain.Metrics) != len(env.Metrics) || plain.Gate != env.Gate || plain.Incomplete != env.Incomplete {
		t.Errorf("MeasureContractGate reported %+v, want the same as MeasureContractGateParts %+v", plain, env)
	}
}

// TestRunContractGate_StampsTheTreeItMeasured: a measurement has to say what it
// is about, or the judging layer cannot tell a verdict on the content in front
// of it from a verdict on content that no longer exists (T2008).
func TestRunContractGate_StampsTheTreeItMeasured(t *testing.T) {
	root := vtRepo(t)
	// The gate pins its Promise home and temp directory inside the root it
	// measures (#102), and git's scratch for the tree identity lands in that temp
	// directory — so the fixture ignores .promise-home/, as this project's own
	// .gitignore does, or the identity would see its own scratch file.
	writeFile(t, root, ".gitignore", ".workspace/\n/.promise-home/\n")
	writeFile(t, root, "a.txt", "a\n")
	vtGit(t, root, "add", "-A")
	vtGit(t, root, "commit", "-q", "-m", "base")
	stubGateBuild(t, &fakeBuild{})

	var out bytes.Buffer
	if err := runContractGate(root, "formatted:go", &out); err != nil {
		t.Fatalf("runContractGate: %v", err)
	}
	var env Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("envelope does not parse: %v (%s)", err, out.String())
	}
	here, err := treeIdentity(root)
	if err != nil {
		t.Fatalf("treeIdentity: %v", err)
	}
	if env.Tree != here {
		t.Errorf("envelope tree = %q, want the tree it measured %q", env.Tree, here)
	}
}

// TestRunContractGate_AMachineGateCarriesNoTree: fit measures the machine, so
// there is no tree for it to speak for — and an identity stamped there would
// invite blessing a tree on the strength of a disk-space measurement.
func TestRunContractGate_AMachineGateCarriesNoTree(t *testing.T) {
	var out bytes.Buffer
	if err := runContractGate(t.TempDir(), "fit", &out); err != nil {
		t.Fatalf("runContractGate: %v", err)
	}
	var env Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("envelope does not parse: %v (%s)", err, out.String())
	}
	if env.Tree != "" {
		t.Errorf("fit carried tree %q, want none", env.Tree)
	}
}

// TestJoinIncomplete: a run may measure less than a full one for more than one
// reason, and a reason that replaced another would hide it.
func TestJoinIncomplete(t *testing.T) {
	for _, tc := range []struct{ a, b, want string }{
		{"", "", ""},
		{"one", "", "one"},
		{"", "two", "two"},
		{"one", "two", "one; two"},
	} {
		if got := joinIncomplete(tc.a, tc.b); got != tc.want {
			t.Errorf("joinIncomplete(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestSumMetrics_ACompositionSumsSharedNames: a name more than one part reports
// is said once, as the sum — `tested` reports one failed_tests, not one per
// toolchain — and a name only one part reports is carried as that part said it.
// A proportion, or one name reported as two kinds of number, is refused rather
// than added.
func TestSumMetrics_ACompositionSumsSharedNames(t *testing.T) {
	got, err := sumMetrics(
		[]Metric{Count("failed_tests", 2), Count("go_failed_tests", 2)},
		[]Metric{Count("failed_tests", 3), Count("promise_failed_tests", 3)})
	if err != nil {
		t.Fatalf("sumMetrics: %v", err)
	}
	want := map[string]int64{"failed_tests": 5, "go_failed_tests": 2, "promise_failed_tests": 3}
	if g := metricInts(got); len(got) != len(want) || fmt.Sprint(g) != fmt.Sprint(want) {
		t.Errorf("summed = %v (%d metrics), want %v", g, len(got), want)
	}
	for _, tc := range []struct {
		name     string
		into, in Metric
	}{
		{"a proportion", Percent("statement_coverage", 50), Percent("statement_coverage", 60)},
		{"two kinds of number", Count("cas_network_bytes", 1), Size("cas_network_bytes", 1, unitBytes)},
	} {
		if _, err := sumMetrics([]Metric{tc.into}, []Metric{tc.in}); err == nil {
			t.Errorf("%s reported by two parts was summed; want it refused", tc.name)
		}
	}
}

// TestCheckAgainstDeclaration_HoldsALeafToItsRow: what a leaf reports must be
// what its --list row declares — no undeclared metric, no other kind of number,
// no name twice — and every member must be one §3 admits: named, with an
// outcome from the vocabulary and no negative time. A complete run missing a
// declared metric is made incomplete rather than passing for nothing wrong.
func TestCheckAgainstDeclaration_HoldsALeafToItsRow(t *testing.T) {
	declared := sharedCount("go", "unformatted_files", 0)
	member := func(m Member) []Group { return []Group{{Name: "gofmt", Members: []Member{m}}} }
	for _, tc := range []struct {
		name string
		in   measurement
		want string
	}{
		{"an undeclared metric", measurement{metrics: append(declared, Count("stray", 1))}, `"stray", which this gate does not declare`},
		{"another kind of number", measurement{metrics: []Metric{Size("unformatted_files", 1, unitBytes), declared[1]}}, "which this gate declares as int"},
		{"a name twice", measurement{metrics: append(declared, declared[0])}, "twice"},
		{"a member with no name", measurement{metrics: declared, groups: member(Member{Outcome: OutcomeFail})}, "no name"},
		{"an outcome outside the vocabulary", measurement{metrics: declared, groups: member(Member{Name: "a.go", Outcome: "broken"})}, `outcome "broken"`},
		{"a negative elapsed", measurement{metrics: declared, groups: member(Member{Name: "a.go", Outcome: OutcomeFail, Elapsed: -1})}, "negative elapsed"},
	} {
		if _, err := checkAgainstDeclaration("", "formatted:go", tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to say %q", tc.name, err, tc.want)
		}
	}
	m, err := checkAgainstDeclaration("", "formatted:go", measurement{metrics: declared[:1]})
	if err != nil || !strings.Contains(m.incomplete, "go_unformatted_files") {
		t.Errorf("a missing declared metric: err = %v, incomplete = %q; want it named as incomplete", err, m.incomplete)
	}
	if m, err := checkAgainstDeclaration("", "formatted:go", measurement{metrics: declared, groups: member(newMember("a.go", OutcomeFail, ""))}); err != nil || m.incomplete != "" {
		t.Errorf("a complete, well-formed run: err = %v, incomplete = %q", err, m.incomplete)
	}
}

// TestEncodeEnvelope_EndsWithinTheBound: an envelope that fits is written as
// measured; one that does not gives up evidence a step at a time — the tail of
// each detail before the details, the details before the members — so a run
// with many failures keeps each one's head, and even one with thousands ends
// within the bound with its metrics whole.
func TestEncodeEnvelope_EndsWithinTheBound(t *testing.T) {
	envelope := func(members, detailBytes int) Envelope {
		head := "lower_test.go:41: got i32, want i64\n"
		g := Group{Name: "compiler/internal/codegen"}
		for i := range members {
			g.Members = append(g.Members, Member{Name: fmt.Sprintf("TestLower%d", i), Outcome: OutcomeFail,
				Detail: head + strings.Repeat("x", detailBytes)})
		}
		return Envelope{SchemaVersion: 1, Gate: "tested:go", Target: HostTarget(),
			Metrics: sharedCount("go", "failed_tests", members), Groups: []Group{g}}
	}
	decode := func(out []byte) Envelope {
		t.Helper()
		var env Envelope
		if err := json.Unmarshal(out, &env); err != nil {
			t.Fatalf("not an envelope: %v", err)
		}
		return env
	}

	small := envelope(3, 100)
	out, err := encodeEnvelope(small)
	if err != nil || mustJSON(t, decode(out).Groups) != mustJSON(t, small.Groups) {
		t.Errorf("an envelope within the bound was changed: err = %v", err)
	}

	// 400 details of 4 KB do not fit; 400 of 1 KB do, so each keeps its head.
	out, err = encodeEnvelope(envelope(400, 4000))
	if err != nil || len(out) > maxEnvelopeBytes {
		t.Fatalf("400 members: %d bytes, err = %v", len(out), err)
	}
	if got := decode(out).Groups[0].Members; len(got) != 400 || !strings.HasPrefix(got[399].Detail, "lower_test.go:41:") {
		t.Errorf("400 members: kept %d, last detail %.40q; want all, each with its head", len(got), got[len(got)-1].Detail)
	}

	out, err = encodeEnvelope(envelope(40000, 100))
	if err != nil || len(out) > maxEnvelopeBytes {
		t.Fatalf("40000 members: %d bytes, err = %v", len(out), err)
	}
	if got := decode(out); metricInts(got.Metrics)["failed_tests"] != 40000 {
		t.Errorf("the metrics were touched: %v", metricInts(got.Metrics))
	}
}
