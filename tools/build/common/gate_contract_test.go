package common

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// `bin/gate --list` is how an orchestrator learns what this project measures,
// so it must print names and nothing else, and the SDK's two required gates
// must be among them or `claim`, `run-step` and `resolve` all refuse.
func TestGateList_PrintsGateNames(t *testing.T) {
	var out bytes.Buffer
	if err := writeGateList(&out, false); err != nil {
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
	for _, name := range ContractGateNames() {
		gate, verdict, err := parseRunArgs([]string{name, "--verdict"})
		if err != nil || gate != name || !verdict {
			t.Errorf("bin/gate lists %q but bin/run refuses it: %v", name, err)
			continue
		}
		def := contractGates[name]
		if def.measure == nil && len(def.parts) == 0 {
			t.Errorf("%q is listed but neither measures nor composes, so asking for it measures nothing", name)
		}
		if _, _, err := parseContractGateArgs([]string{name, "--envelope"}); err != nil {
			t.Errorf("bin/gate lists %q but refuses to measure it: %v", name, err)
		}
	}
}

// A refusal quotes the spelling the caller typed. Normalising first and then
// interpolating the result tells someone who typed `--json` to try `-json`,
// correcting them in a spelling they did not use.
func TestParse_RefusalQuotesTheTypedSpelling(t *testing.T) {
	if _, _, err := parseContractGateArgs([]string{"fit", "--verbose"}); err == nil || !strings.Contains(err.Error(), `"--verbose"`) {
		t.Errorf("gate: got %v, want the typed --verbose", err)
	}
	if _, _, err := parseRunArgs([]string{"fit", "--bogus"}); err == nil || !strings.Contains(err.Error(), `"--bogus"`) {
		t.Errorf("run: got %v, want the typed --bogus", err)
	}
	// Both spellings still work, so the fix is about the message only.
	for _, spelling := range [][]string{{"fit", "--envelope"}, {"fit", "-envelope"}} {
		if _, envelope, err := parseContractGateArgs(spelling); err != nil || !envelope {
			t.Errorf("parseContractGateArgs(%q) = %v, %v", spelling, envelope, err)
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
	if err := writeGateList(&out, true); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Gates []struct {
			Name    string `json:"name"`
			Summary string `json:"summary"`
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
	}
	if !slices.Equal(names, ContractGateNames()) {
		t.Errorf("gates = %v, want %v", names, ContractGateNames())
	}
}

func TestGateList_ArgsAreRecognised(t *testing.T) {
	for _, args := range [][]string{{"--list"}, {"list"}, {"-list"}, {"--list", "--json"}, {"list", "--json"}} {
		list, _, ok := parseListArgs(args)
		if !ok || !list {
			t.Errorf("parseListArgs(%q) = list %v, ok %v; want a listing request", args, list, ok)
		}
	}
	// A gate invocation must fall through to the gates, not be read as a list.
	for _, args := range [][]string{{"fit"}, {"fit", "--envelope"}, {"test"}, {}} {
		if _, _, ok := parseListArgs(args); ok {
			t.Errorf("parseListArgs(%q) claimed a listing request", args)
		}
	}
}

// bin/run must be able to run any gate bin/gate lists, so both read one
// registry. It also reports the commands, which bin/gate knows nothing about.
func TestRunList_GatesMatchGateListAndCommands(t *testing.T) {
	var out bytes.Buffer
	if err := writeRunList("../../..", &out, true); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Gates    []string `json:"gates"`
		Commands []string `json:"commands"`
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

// integrationMetrics names what integration's parts report. It is spelled out
// rather than derived because deriving it means running the gates, which is
// minutes of compiling and testing for a list that changes a few times a year.
var integrationMetrics = []string{
	"unformatted_go_files", "unformatted_promise_files",
	"unbuildable_go_packages", "build_failures", "vet_findings",
	"promise_check_failures", "promise_check_errors", "promise_check_warnings",
	"go_test_failures", "go_test_packages_failed",
	"host_test_failures", "host_leak_count",
}

// Every metric integration's parts report must carry a term — a person-edited
// cap or an enforced baseline — or integration passes on numbers nothing looks
// at. Which of the two a metric gets is the project's call: absolutes are caps,
// and anything that ratchets is a baseline.
//
// This checks every target the baselines file knows, not just the host's. A
// term is per-target data, written where a gate has actually run, so a
// host-scoped check is blind to the platform its author is not sitting on: the
// gap then lands as a red trunk for the next person there rather than as a
// failure for whoever opened it. That is exactly how T2087 reached main —
// go_test_failures was seeded on two targets of four.
func TestIntegration_PartMetricsAreJudged(t *testing.T) {
	caps, err := loadThresholds("../../..")
	if err != nil {
		t.Fatal(err)
	}
	all, err := LoadBaselines("../../..")
	if err != nil {
		t.Fatal(err)
	}
	targets := make([]string, 0, len(all)+1)
	for target := range all {
		targets = append(targets, target)
	}
	slices.Sort(targets)
	// A host with no block of its own is checked too, or the platform most in
	// need of the seeding is the one target nothing asks about.
	if !slices.Contains(targets, HostTarget()) {
		targets = append(targets, HostTarget())
	}
	for _, target := range targets {
		for _, name := range integrationMetrics {
			if _, capped := caps[name]; capped {
				continue
			}
			b, tracked := all[target][name]
			if !tracked || b.Value == nil || b.Direction == "" || b.Type == "informational" {
				t.Errorf("%s/%s has neither a cap nor an enforced baseline, so nothing judges it there — give it a value and a direction in %s",
					target, name, baselinesFile)
			}
		}
	}
}

// fit's floors are caps: they are absolutes a person edits, not numbers that
// ratchet with history.
func TestFit_FloorsAreCaps(t *testing.T) {
	caps, err := loadThresholds("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"worktree_free_bytes", "build_cache_free_bytes"} {
		th, ok := caps[name]
		if !ok || th.Direction != AtLeast || th.Cap <= 0 {
			t.Errorf("%s = %+v, want a positive at_least floor", name, th)
		}
	}
}

// integrationMetricsUnjudged names metrics integration's parts report that
// integrationMetrics deliberately leaves out, each with the reason. An
// unexplained omission and an overlooked one look identical in a list of
// strings, and the second is exactly how T2087 happened; spelling the reason
// makes the next person's choice a decision rather than an inheritance.
var integrationMetricsUnjudged = map[string]string{
	"host_test_count":     "a suite's size is not a quality of the change. It ratchets `up` where a target carries a figure, but requiring that everywhere would fail a target for deleting a test — which is sometimes the right change.",
	"promise_check_units": "how many units the checker was given is not a quality of the change either — merging two files into a module lowers it without checking any less. It ratchets `up` where a target carries a figure; what guards against a sweep that measured nothing is the gate's refusal of a run that printed no summary.",
	"cas_network_bytes":   "what a run pulls over the wire into the store SHOULD be enforced at exactly zero. T2150 gave a cold home the embedded copy of an artifact this binary carries instead of a download, so it should now be zero; T2153 promotes it once a -count=1 gate run has said so on linux and darwin as well as windows, since a term here commits every target at once.",
	// The scheduled gates' metrics. They are judged — every one of them carries
	// a ratcheted baseline on the targets that measure it — but they are not
	// INTEGRATION metrics, and integrationMetrics demands a term on every
	// target this project ships. Requiring that here would fail linux-arm64 for
	// not running a wasm suite, a stress run or coverage, which is a fact about
	// what that platform is given to do and not a regression in any change.
	"stress_flaky_count": "tested:stress, not integration: a flaky count needs many runs to mean anything, so it is measured on a schedule and ratcheted per target rather than demanded of every one.",
	"stress_iterations":  "tested:stress reports how many chances a test had, because a flaky count without it says nothing. It is the size of the run, not a quality of the change.",
	"wasm_size_total":    "the size:wasm gate, not integration: it is ratcheted per target where the WASM canaries are built, and linux-arm64 does not build them.",
	// size:wasm reports one metric per canary under tests/size — today
	// wasm_size_minimal, _strings, _collections, _concurrency and _full. They
	// are built from the file names rather than spelled, so the scanner above
	// cannot see them; they are ratcheted per target exactly as the total is.
	"cas_home_count": "one home per run is the end state, and T2150 removed the private PROMISE_HOME the Go suite built per test (29 → 1, measured on windows-amd64). T2153 promotes it once the other targets have been measured too. Note when promoting: bin/verify does not pass -count=1, so its Go phase reports a figure that moves with the test cache — which is why the store metrics stay out of its gate values, and must, or the commit gate ratchets the baseline down to a cached run and fails the next full one.",
}

// metricNameLiteral matches the name a gate gives a metric at the only place
// one is ever spelled: the Count/Size constructors in gate source.
var metricNameLiteral = regexp.MustCompile(`\b(?:Count|Size)\("([a-z0-9_]+)"`)

// reportedMetricNames scans this package's non-test sources for every metric
// name a gate can report. Reading the source rather than running the gates is
// what makes this affordable — running them is minutes of compiling and testing
// — and it is sound because a metric only exists by being constructed here.
func reportedMetricNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range metricNameLiteral.FindAllSubmatch(src, -1) {
			seen[string(m[1])] = true
		}
	}
	if len(seen) == 0 {
		t.Fatal("scanned the package and found no metric names at all — the constructors moved, and every check built on this scan has quietly stopped checking")
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// integrationMetrics is hand-maintained, and a hand-maintained list of what a
// program reports drifts from what it reports. The drift is silent in both
// directions and both directions matter: a metric added to a part gate and not
// added here is never asked for a term on any target — T2087's defect one layer
// up, and the one the all-targets sweep cannot see — while a name left here
// after the metric was renamed away demands a baseline for a number nothing
// will ever produce, which no green run can ever satisfy.
//
// So: every metric this program can report is accounted for, as an enforced
// term, a cap, or a written reason not to judge it; and everything named here
// is a metric this program can actually report.
func TestIntegrationMetrics_AccountsForEveryMetricReported(t *testing.T) {
	caps, err := loadThresholds("../../..")
	if err != nil {
		t.Fatal(err)
	}
	reported := reportedMetricNames(t)
	for _, name := range reported {
		switch {
		case slices.Contains(integrationMetrics, name):
		case caps[name].Direction != "":
		case integrationMetricsUnjudged[name] != "":
		default:
			t.Errorf("a gate reports %q, but nothing accounts for it: add it to integrationMetrics (and give it a term on every target in %s), give it a cap, or say in integrationMetricsUnjudged why it is not judged",
				name, baselinesFile)
		}
	}
	for _, name := range integrationMetrics {
		if !slices.Contains(reported, name) {
			t.Errorf("integrationMetrics names %q, which no gate in this package reports — every target is being asked for a baseline on a number that will never arrive", name)
		}
	}
	for name := range integrationMetricsUnjudged {
		if !slices.Contains(reported, name) {
			t.Errorf("integrationMetricsUnjudged excuses %q, which no gate reports — the excuse outlived the metric", name)
		}
	}
}

// foreignTarget is a platform no host can be, so a block keyed by it is never
// the one a run reads. Spelled rather than derived: picking a real target that
// "isn't this one" makes the test's meaning depend on where it runs.
const foreignTarget = "nosuchos-nosucharch"

// writeTerms builds a root carrying both manifests the judge reads. Either body
// may be empty, meaning the file is simply absent — which is a state the judge
// has an answer for, and therefore one worth testing.
func writeTerms(t *testing.T, thresholds, baselines string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "tools", "gates")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		filepath.Base(ThresholdsFile): thresholds,
		filepath.Base(baselinesFile):  baselines,
	} {
		if body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// someCaps is a valid thresholds manifest naming a metric none of these tests
// measures, so a baseline is the only thing that can judge anything here.
const someCaps = `{"worktree_free_bytes": {"direction": "at_least", "cap": 1}}`

// Baselines are PER TARGET, and which target a run reads is the whole of
// T2087's first failure: go_test_failures carried an enforced baseline on two
// of four targets, so the same gate judged it on two hosts and silently did not
// on the others. projectTerms is where that choice is made, and every branch of
// it below answers "nothing judges this metric here" — none of them loudly.
func TestProjectTerms_AreScopedToTheHostTarget(t *testing.T) {
	host := HostTarget()

	t.Run("the host's own block is the one that judges", func(t *testing.T) {
		root := writeTerms(t, someCaps, `{
			"`+host+`":          {"mine":   {"value": 0, "direction": "exact"}},
			"`+foreignTarget+`": {"theirs": {"value": 0, "direction": "exact"}}
		}`)
		caps, baselines, err := projectTerms(root)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := caps["worktree_free_bytes"]; !ok {
			t.Error("the caps did not survive the lookup")
		}
		if _, ok := baselines["mine"]; !ok {
			t.Error("the host's own baseline is missing, so a run here is judged by nothing it should be")
		}
		if _, ok := baselines["theirs"]; ok {
			t.Error("another target's baseline reached this run — a figure measured on one machine cannot judge another")
		}
	})

	t.Run("a host with no block of its own is judged by nothing", func(t *testing.T) {
		root := writeTerms(t, someCaps, `{"`+foreignTarget+`": {"mine": {"value": 0, "direction": "exact"}}}`)
		caps, baselines, err := projectTerms(root)
		// Silence is the finding. This path is NOT an error and should not
		// become one — a new platform must be able to run a gate before anyone
		// has a figure for it — which is exactly why the gap is invisible from
		// the inside, and why the check that catches it has to be a test over
		// the whole file rather than over whatever HostTarget() says today.
		if err != nil {
			t.Fatalf("an unseeded host must still be able to run a gate: %v", err)
		}
		if len(baselines) != 0 {
			t.Errorf("baselines = %v, want none: this host has no block", baselines)
		}
		if len(caps) == 0 {
			t.Error("the caps stopped judging too — an unseeded host would then be judged by nothing at all")
		}
	})

	t.Run("an unreadable baselines file leaves the caps judging", func(t *testing.T) {
		for name, body := range map[string]string{
			"absent":      "",
			"not json":    `{`,
			"wrong shape": `[]`,
		} {
			root := writeTerms(t, someCaps, body)
			caps, baselines, err := projectTerms(root)
			if err != nil {
				t.Errorf("%s: %v — losing the baselines must not cost the caps", name, err)
				continue
			}
			if len(caps) == 0 {
				t.Errorf("%s: the caps went with the baselines", name)
			}
			if len(baselines) != 0 {
				t.Errorf("%s: baselines = %v, want none", name, baselines)
			}
		}
	})

	t.Run("an absent thresholds manifest is fatal", func(t *testing.T) {
		// The other direction, and it is not symmetric: a project with a judge
		// must have caps, so their absence is a broken tree rather than an
		// unseeded one. Judging on silently empty terms is the failure this
		// whole item is about.
		root := writeTerms(t, "", `{"`+host+`": {"mine": {"value": 0, "direction": "exact"}}}`)
		if _, _, err := projectTerms(root); err == nil {
			t.Error("a missing thresholds manifest must be refused, not absorbed into terms that judge nothing")
		}
	})
}

// The same gap, end to end, through the program that makes landing decisions:
// an envelope reporting failures, with no term applied because the baseline for
// that metric lives under a different target. This is precisely what
// `bin/run tested:go` did on windows-amd64 at 5600e002 — and there it answered
// "acceptable" over seven failing tests.
//
// It now answers no. A metric nobody set a rule for has not been cleared, and
// the verdict says which: a gate reporting numbers it holds no terms for is
// refused rather than waved through. TestIntegration_PartMetricsAreJudged
// asserts the repository's data has no such hole; this asserts that a hole,
// wherever one opens, costs a refusal and not a false pass.
func TestJudgeStdin_ABaselineOnAnotherTargetIsNoTermHere(t *testing.T) {
	const envelope = `{"schema_version":1,"gate":"tested:go","target":"nosuchos-nosucharch","metrics":[{"name":"go_test_failures","type":"int","value":7}]}`
	const seeded = `{"value": 0, "direction": "exact"}`

	for name, tc := range map[string]struct {
		baselines      string
		wantAcceptable bool
		wantTerm       bool
	}{
		"seeded only on another target": {
			baselines:      `{"` + foreignTarget + `": {"go_test_failures": ` + seeded + `}}`,
			wantAcceptable: false, // seven failing tests and no term: not judged is not a pass
			wantTerm:       false,
		},
		"seeded on this host": {
			baselines:      `{"` + HostTarget() + `": {"go_test_failures": ` + seeded + `}}`,
			wantAcceptable: false,
			wantTerm:       true,
		},
		"tracked here but informational": {
			// The third state, and it judges exactly as little as no entry at
			// all — which is why the data test refuses it as a term, and why it
			// reaches the same verdict as no entry at all: unjudged.
			baselines:      `{"` + HostTarget() + `": {"go_test_failures": {"type": "informational"}}}`,
			wantAcceptable: false,
			wantTerm:       false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := writeTerms(t, someCaps, tc.baselines)
			var out bytes.Buffer
			if err := JudgeStdin(root, "tested:go", strings.NewReader(envelope), &out); err != nil {
				t.Fatal(err)
			}
			var got verdictWire
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("the verdict is not one JSON object: %v (%q)", err, out.String())
			}
			if got.Acceptable != tc.wantAcceptable {
				t.Errorf("acceptable = %v, want %v (detail: %s)", got.Acceptable, tc.wantAcceptable, got.Detail)
			}
			if _, applied := got.Thresholds["go_test_failures"]; applied != tc.wantTerm {
				t.Errorf("a term was applied = %v, want %v — the verdict must carry exactly what it was reached from", applied, tc.wantTerm)
			}
		})
	}
}

// The sweep above walks every target the baselines FILE knows, which closes
// the gap for a platform someone has already seeded and leaves one open for a
// platform nobody has: a target with no block at all is not a target with a
// hole in it, and the loop has nothing to iterate. That is the same defect one
// step earlier — the first person on that platform meets a red trunk — and the
// file cannot detect it about itself.
//
// requiredPlatforms is the answer, because it is already the list of platforms
// this project ships and refuses to tag without (release_cut.go). Anchoring
// here means adding a platform there makes its baselines a checked obligation
// in the same change, rather than a discovery someone makes months later on the
// machine. The reasoning is the one written over requiredPlatforms itself:
// shipping something nothing gates on is how a platform silently rots.
func TestBaselines_KnowEveryPlatformThisProjectShips(t *testing.T) {
	all, err := LoadBaselines("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, platform := range requiredPlatforms {
		if len(all[platform]) == 0 {
			t.Errorf("%s is release-blocking, but %s has no block for it — the all-targets sweep has nothing to walk there, so every metric on that platform is unjudged and nothing says so until someone sits down at one",
				platform, baselinesFile)
		}
	}
	// And the other way: a block for a platform nobody ships is terms no run
	// will ever read, and the sweep spends a real failure on maintaining them.
	for platform := range all {
		if !slices.Contains(requiredPlatforms, platform) {
			t.Errorf("%s carries a block for %q, which this project does not ship — either it belongs in requiredPlatforms or the block is dead data the sweep now demands upkeep of",
				baselinesFile, platform)
		}
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
	writeFile(t, root, "a.txt", "a\n")
	vtGit(t, root, "add", "-A")
	vtGit(t, root, "commit", "-q", "-m", "base")
	stubGateBuild(t, &fakeBuild{})

	var out bytes.Buffer
	if err := runContractGate(root, []string{"formatted:go", "--envelope"}, &out); err != nil {
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
	if err := runContractGate(t.TempDir(), []string{"fit", "--envelope"}, &out); err != nil {
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
