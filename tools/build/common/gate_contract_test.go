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

func TestGateList_JSON(t *testing.T) {
	var out bytes.Buffer
	if err := writeGateList(&out, true); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Gates []string `json:"gates"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("--list --json is not one JSON object: %v\n%s", err, out.String())
	}
	if !slices.Equal(got.Gates, ContractGateNames()) {
		t.Errorf("gates = %v, want %v", got.Gates, ContractGateNames())
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
	for _, c := range got.Commands {
		if !slices.Contains(CommandNames, c) {
			t.Errorf("commands include %q, which is not in the closed set %v", c, CommandNames)
		}
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
	"cas_network_bytes":   "what a run pulls over the wire into the store SHOULD be enforced at exactly zero, and is not yet: a real sweep fetches ~70 MB because every private PROMISE_HOME the Go suite builds starts with an empty CAS (T2150). Tracked until that is fixed; promoting it is then a value and a direction in each target block.",
	"cas_home_count":      "one home per run is the end state and the tree is at 29 (T2150), so an enforced term today would fail every run rather than the changes that add one. Tracked until then. Note when promoting: bin/verify does not pass -count=1, so its Go phase reports anywhere from 1 to 29 depending on the test cache — which is why the store metrics stay out of its gate values, and must, or the commit gate ratchets the baseline down to a cached run and fails the next full one.",
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
// an envelope reporting failures passes with no term applied, because the
// baseline for that metric lives under a different target. This is precisely
// what `bin/run tested:go` did on windows-amd64 at 5600e002.
//
// TestIntegration_PartMetricsAreJudged asserts the repository's data has no
// such hole. This asserts what the hole COSTS, which is what makes that data
// assertion worth keeping: without it the verdict is not merely unjudged, it is
// affirmatively "acceptable".
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
			wantAcceptable: true, // seven failing tests, and the verdict is yes
			wantTerm:       false,
		},
		"seeded on this host": {
			baselines:      `{"` + HostTarget() + `": {"go_test_failures": ` + seeded + `}}`,
			wantAcceptable: false,
			wantTerm:       true,
		},
		"tracked here but informational": {
			// The third state, and it judges exactly as little as no entry at
			// all — which is why the data test refuses it as a term.
			baselines:      `{"` + HostTarget() + `": {"go_test_failures": {"type": "informational"}}}`,
			wantAcceptable: true,
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
