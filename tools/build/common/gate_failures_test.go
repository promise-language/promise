package common

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The gates driven into their failures. For each, the measurement is held to
// the three things a failing gate owes whoever reads its verdict: the finding
// is measured, with evidence; the evidence is in the tool's own words; and the
// verdict names the gate's remedy. The judge's half — that every gate's
// refusal carries a metric, a member and a remedy — is
// TestEveryGateRefusalNamesFindingEvidenceAndRemedy; this is the measuring
// half, gate by gate.

// requireRefusal judges what gate measured against one term on metric — none
// when metric is "", for a run whose failure is that it measured less — and
// holds the refusal to the finding, each of words, and the gate's remedy.
func requireRefusal(t *testing.T, root, gate string, m measurement, metric string, d Direction, bound string, words ...string) {
	t.Helper()
	terms := capsOnly(map[string]capTerm{})
	want := words
	if metric != "" {
		terms = capsOnly(map[string]capTerm{metric: {Direction: d, Cap: json.Number(bound)}})
		want = append([]string{metric + " is "}, words...)
	}
	env := Envelope{SchemaVersion: EnvelopeSchemaVersion, Gate: gate, Target: HostTarget(),
		Metrics: m.metrics, Groups: m.groups, Incomplete: m.incomplete}
	v := judge(root, env, terms)
	if v.Acceptable {
		t.Fatalf("%s was judged acceptable: %+v", gate, v)
	}
	for _, w := range append(want, capitalize(contractGates[gate].remedy)) {
		if !strings.Contains(v.Detail, w) {
			t.Errorf("%s's refusal lacks %q:\n%s", gate, w, v.Detail)
		}
	}
}

func TestFormattedGo_AnUnformattedFileIsNamed(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "compiler/x.go", "package x\n\nfunc   F( ) {}\n")
	writeFile(t, root, "compiler/y.go", "package x\n\nfunc G() {}\n")
	m, err := measureFormattedGo(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := metricInts(m.metrics); got["unformatted_files"] != 1 || got["go_unformatted_files"] != 1 {
		t.Errorf("metrics = %v, want the one unformatted file counted", got)
	}
	requireRefusal(t, root, "formatted:go", m, "unformatted_files", AtMost, "0", "gofmt: compiler/x.go (fail)")
}

func TestFormattedPromise_AnUnformattedFileIsNamed(t *testing.T) {
	m, err := measureFormattedPromiseWith(t.TempDir(), func(string) ([]string, error) { return []string{"tests/a.pr"}, nil })
	if err != nil {
		t.Fatal(err)
	}
	requireRefusal(t, t.TempDir(), "formatted:promise", m, "promise_unformatted_files", AtMost, "0", "promise format: tests/a.pr (fail)")
}

// A package that does not compile is a member carrying the compiler's lines;
// a module that built adds no group.
func TestBuilds_APackageThatDoesNotCompileIsNamed(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	root, modules := goModulesRoot(t)
	fake := &fakeGoTest{
		stderr: map[string]string{modules[0]: "# example.com/compiler/internal/foo\ninternal/foo/foo.go:3:9: undefined: bar\n"},
		errs:   map[string]error{modules[0]: errors.New("exit status 1")},
	}
	m, err := measureBuildsWith(root, fake.capture)
	if err != nil {
		t.Fatal(err)
	}
	if got := metricInts(m.metrics); got["unbuildable_packages"] != 1 || got["go_unbuildable_packages"] != 1 || len(m.groups) != 1 {
		t.Errorf("metrics = %v, groups = %+v; want one package in one group", got, m.groups)
	}
	requireRefusal(t, root, "builds", m, "unbuildable_packages", AtMost, "0",
		"compiler: example.com/compiler/internal/foo (fail)\n    internal/foo/foo.go:3:9: undefined: bar")
}

// Real toolchain, as TestCheckedGo_CountsRealVetFindings: the evidence is what
// `go vet` itself said.
func TestCheckedGo_AFindingIsNamedInVetsWords(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	root := t.TempDir()
	writeGoModule(t, filepath.Join(root, "compiler"), "compiler",
		"package compiler\n\nimport \"fmt\"\n\nfunc F() { fmt.Printf(\"%d\\n\", \"not an int\") }\n")
	m, err := measureCheckedGo(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.groups) != 1 || m.groups[0].Name != "compiler" || len(m.groups[0].Members) != 1 {
		t.Fatalf("groups = %+v, want the finding in the compiler module's group", m.groups)
	}
	requireRefusal(t, root, "checked:go", m, "check_findings", AtMost, "0", "Printf format %d")
}

func TestCheckedPromise_ADiagnosticIsNamedInTheCheckersWords(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	root := rootWithCompiler(t)
	const output = "FAIL tests/bad (1 error)\n" +
		"bad.pr:5:2: no viable alternative at input 'y'\n" +
		"  >   y\n" +
		"\n" +
		"0 checked, 0 warned, 1 failed (1 units, 1 errors, 0 warnings, 0.100s)\n"
	m, err := measureCheckedPromiseWith(root, func(string) (string, error) { return output, errors.New("exit status 1") })
	if err != nil {
		t.Fatal(err)
	}
	requireRefusal(t, root, "checked:promise", m, "promise_check_findings", AtMost, "0",
		"tests/bad: bad.pr:5:2 (fail)\n    no viable alternative at input 'y'")
}

func TestTestedGo_AFailingTestIsNamedInItsOwnWords(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	root, modules := goModulesRoot(t)
	const pkg = "github.com/promise-language/promise/compiler/internal/codegen"
	fake := &fakeGoTest{
		stdout: map[string]string{modules[0]: goEvents(
			`{"Action":"run","Package":"`+pkg+`","Test":"TestLower"}`,
			`{"Action":"output","Package":"`+pkg+`","Test":"TestLower","Output":"    lower_test.go:41: got i32, want i64\n"}`,
			`{"Action":"fail","Package":"`+pkg+`","Test":"TestLower","Elapsed":0.12}`,
			`{"Action":"fail","Package":"`+pkg+`","Elapsed":0.2}`,
		)},
		errs: map[string]error{modules[0]: errors.New("exit status 1")},
	}
	m, err := measureTestedGoWith(root, fake.capture)
	if err != nil {
		t.Fatal(err)
	}
	requireRefusal(t, root, "tested:go", m, "go_failed_tests", AtMost, "0",
		"compiler/internal/codegen: TestLower (fail)\n    lower_test.go:41: got i32, want i64")
}

func TestTestedPromise_AFailingTestIsNamedInItsOwnWords(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	root := t.TempDir()
	records := fmt.Sprintf(`{"file":%q,"test":"test_ok","status":"pass","elapsed":0.001}`+"\n"+
		`{"file":%q,"test":"test_or","status":"fail","elapsed":0.002,"context":"panic: assertion failed: a || b"}`+"\n",
		filepath.Join(root, "tests", "std", "bool_test.pr"), filepath.Join(root, "tests", "std", "bool_test.pr"))
	m, err := measureTestedPromiseWith(root, func(string, string) (string, error) { return records, errors.New("exit status 1") })
	if err != nil {
		t.Fatal(err)
	}
	if len(m.groups) != 1 || len(m.groups[0].Members) != 1 {
		t.Errorf("groups = %+v, want one member: the test that failed", m.groups)
	}
	requireRefusal(t, root, "tested:promise", m, "promise_failed_tests", AtMost, "0",
		"tests/std/bool_test.pr: test_or (fail)\n    panic: assertion failed: a || b")
}

// The stress report's FLAKY block, as compiler/cmd/promise/stress.go writes it.
func TestTestedStress_AFlakyTestIsNamedWithItsFailures(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	const report = "10 iterations over 3.2s\n\n" +
		"FLAKY (1 tests):\n" +
		"  tests/std/race_test.pr\n" +
		"    test_race                      7/10 (70.0%)  avg: 1.0ms  σ: 0.1ms  min: 0.9ms  max: 1.2ms\n" +
		"      3 fail: assertion failed: x == 1\n" +
		"\n"
	m, err := measureTestedStressWith(t.TempDir(), func(string) (string, error) { return report, errors.New("exit status 1") })
	if err != nil {
		t.Fatal(err)
	}
	if got := metricInts(m.metrics); got["stress_flaky_count"] != 1 || got["stress_iterations"] != 10 {
		t.Errorf("metrics = %v", got)
	}
	requireRefusal(t, t.TempDir(), "tested:stress", m, "stress_flaky_count", AtMost, "0",
		"tests/std/race_test.pr: test_race (fail)", "7/10 (70.0%)", "3 fail: assertion failed: x == 1")
}

// A canary that grew is judged by its own metric; one that did not compile is
// a member carrying the compiler's words, and the total that omits it makes
// the run incomplete.
func TestSizeWasm_AGrownCanaryAndABrokenOneAreNamed(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	root := t.TempDir()
	for _, c := range []string{"canary_minimal.pr", "canary_full.pr"} {
		writeFile(t, root, "tests/size/"+c, "fun main() {}\n")
	}
	compile := func(canary, wasm string) error {
		if strings.Contains(canary, "full") {
			return errors.New("exit status 1: canary_full.pr:1:1: undefined: foo")
		}
		return os.WriteFile(wasm, make([]byte, 100), 0o644)
	}
	m, err := measureSizeWasmWith(root, compile)
	if err != nil {
		t.Fatal(err)
	}
	if got := metricInts(m.metrics); got["wasm_size_minimal"] != 100 || got["wasm_size_total"] != 100 {
		t.Errorf("metrics = %v, want the minimal canary's 100 bytes and a total of them", got)
	}
	requireRefusal(t, root, "size:wasm", m, "wasm_size_minimal", AtMost, "10",
		"canary_full: tests/size/canary_full.pr (fail)\n    exit status 1: canary_full.pr:1:1: undefined: foo",
		"these canaries did not compile, and the total omits them: canary_full")
}

// Below a floor, the refusal says which filesystem; with the cache's location
// unknown, the run is incomplete and says why in go's own words.
func TestFit_AFloorAndAnUnknownCacheAreNamed(t *testing.T) {
	root, cache := t.TempDir(), t.TempDir()
	goEnv := func(stdout, stderr string, err error) captureFunc {
		return func(string, string, ...string) (string, string, error) { return stdout, stderr, err }
	}
	m, err := measureFitWith(root, goEnv(cache+"\n", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	requireRefusal(t, root, "fit", m, "worktree_free_bytes", AtLeast, "9223372036854775807", ", in "+root+" (")

	m, err = measureFitWith(root, goEnv("", "go: cannot find GOROOT directory: /nowhere\n", errors.New("exit status 2")))
	if err != nil {
		t.Fatal(err)
	}
	requireRefusal(t, root, "fit", m, "", "", "",
		"GOCACHE: go env GOCACHE (not-run)\n    go: cannot find GOROOT directory: /nowhere",
		"the Go build cache location is unknown")
}

func TestLatestInvariant_AWrongLatestIsNamed(t *testing.T) {
	stubMachineGates(t)
	latestIsEpoch = func() error { return errors.New(`releases/latest is "deps-llvm-22.1.0", not an epoch-* release`) }
	m, err := measureLatestInvariant("")
	if err != nil {
		t.Fatal(err)
	}
	requireRefusal(t, t.TempDir(), "latest-invariant", m, "latest_is_epoch", AtLeast, "1",
		"releases/latest: latest (fail)\n    releases/latest is \"deps-llvm-22.1.0\", not an epoch-* release")
}

// The phase that failed carries the installer's error; the ones after it
// never ran.
func TestInstall_AFailedPhaseIsNamed(t *testing.T) {
	stubMachineGates(t)
	installPhases = func(root, work, variant, channel string, system bool) error {
		writeFile(t, work, "phases.json", `{"fetch":"pass","install":"fail"}`)
		return errors.New("installer exited 1: checksum mismatch for promise-thin-linux-amd64.tar.br")
	}
	m, err := measureInstallVariant(t.TempDir(), "thin")
	if err != nil {
		t.Fatal(err)
	}
	if got := metricInts(m.metrics); got["install_thin_fetch_ok"] != 1 || got["install_thin_install_ok"] != 0 {
		t.Errorf("metrics = %v, want fetch ok and install not", got)
	}
	var outcomes []string
	for _, member := range m.groups[0].Members {
		outcomes = append(outcomes, member.Name+"="+member.Outcome)
	}
	if want := []string{"install=fail", "sanity=not-run", "test=not-run"}; m.groups[0].Name != "phases" || !slices.Equal(outcomes, want) {
		t.Errorf("phases = %v, want %v", outcomes, want)
	}
	requireRefusal(t, t.TempDir(), "install:thin", m, "install_thin_install_ok", AtLeast, "1",
		"phases: install (fail)\n    installer exited 1: checksum mismatch")
}

// A gate named for a concept reports that concept's names for each toolchain
// it measures, under the shared name and the toolchain's own, as the kind of
// number the contract fixes — and `integration` carries all of its parts'.
// The retired names are declared by no gate, so no term can be keyed on them.
func TestConceptGates_DeclareTheRequiredNames(t *testing.T) {
	count, percent, bytes := MetricSpec{Type: MetricInt}, MetricSpec{Type: MetricFloat, Unit: unitPercent}, MetricSpec{Type: MetricInt, Unit: unitBytes}
	required := map[string]map[string]MetricSpec{
		"formatted": {"unformatted_files": count, "go_unformatted_files": count, "promise_unformatted_files": count},
		"builds":    {"unbuildable_packages": count, "go_unbuildable_packages": count},
		"checked":   {"check_findings": count, "go_check_findings": count, "promise_check_findings": count},
		"tested": {"failed_tests": count, "test_count": count, "failed_packages": count, "leaked_tests": count,
			"go_failed_tests": count, "go_test_count": count, "go_failed_packages": count,
			"promise_failed_tests": count, "promise_test_count": count, "promise_leaked_tests": count},
		"covered": {"statement_coverage": percent, "go_statement_coverage": percent,
			"block_coverage": percent, "promise_block_coverage": percent},
		"fit": {"worktree_free_bytes": bytes, "cache_free_bytes": bytes},
	}
	for _, part := range contractGates[integrationGate].parts {
		for name, s := range required[part] {
			if required[integrationGate] == nil {
				required[integrationGate] = map[string]MetricSpec{}
			}
			required[integrationGate][name] = s
		}
	}
	for gate, names := range required {
		declared := declaredMetrics("../../..", gate)
		for name, want := range names {
			i := slices.IndexFunc(declared, func(s MetricSpec) bool { return s.Name == name })
			if i < 0 {
				t.Errorf("%s does not declare %s", gate, name)
				continue
			}
			if got := declared[i]; got.Type != want.Type || got.Unit != want.Unit {
				t.Errorf("%s declares %s as %s, want %s", gate, name, got.describe(), MetricSpec{Type: want.Type, Unit: want.Unit}.describe())
			}
		}
	}
	for _, gate := range ContractGateNames() {
		for _, s := range declaredMetrics("../../..", gate) {
			if slices.Contains([]string{"vet_findings", "build_cache_free_bytes", "go_coverage_pct", "unformatted_go_files", "go_test_failures", "host_test_failures"}, s.Name) {
				t.Errorf("%s still declares the retired name %s", gate, s.Name)
			}
		}
	}
}
