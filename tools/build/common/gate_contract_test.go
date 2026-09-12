package common

import (
	"bytes"
	"encoding/json"
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

// Every metric integration's parts report must carry a term — a person-edited
// cap or an enforced baseline — or integration passes on numbers nothing looks
// at. Which of the two a metric gets is the project's call: absolutes are caps,
// and anything that ratchets is a baseline.
func TestIntegration_PartMetricsAreJudged(t *testing.T) {
	caps, baselines, err := projectTerms("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"unformatted_go_files", "unformatted_promise_files",
		"unbuildable_go_packages", "vet_findings",
		"go_test_failures", "go_test_packages_failed",
		"host_test_failures", "host_leak_count",
	} {
		if _, capped := caps[name]; capped {
			continue
		}
		b, tracked := baselines[name]
		if !tracked || b.Value == nil || b.Direction == "" || b.Type == "informational" {
			t.Errorf("%s has neither a cap nor an enforced baseline, so nothing judges it", name)
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
