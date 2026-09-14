package common

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeGoTest stands in for the `go test` child measureTestedGo spawns, recording
// which module directory each invocation was for and the argv it carried, and
// answering with canned output per directory.
//
// The real child cannot be spawned from here: `go test ./...` in tools/build IS
// this suite, so a test that ran it would spawn an unbounded chain of go test
// subprocesses — the hazard TestRunToolsGoTests_TrivialModule already names.
type fakeGoTest struct {
	stdout map[string]string
	stderr map[string]string
	errs   map[string]error

	dirs []string
	argv [][]string
}

func (f *fakeGoTest) capture(dir, name string, args ...string) (string, string, error) {
	f.dirs = append(f.dirs, dir)
	f.argv = append(f.argv, append([]string{name}, args...))
	return f.stdout[dir], f.stderr[dir], f.errs[dir]
}

// goModulesRoot builds a tree shaped like this repository — a compiler module
// and a tools/build module — and returns it beside the directories GoModules
// reports for it, which is what a run must visit.
//
// The fixture is checked for having produced more than one module, because
// every pin below is about the sweep: against a one-module root they would all
// pass without measuring anything.
func goModulesRoot(t *testing.T) (root string, modules []string) {
	t.Helper()
	root = t.TempDir()
	for _, dir := range [][]string{{"compiler"}, {"tools", "build"}} {
		full := filepath.Join(append([]string{root}, dir...)...)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		mod := "module example.com/" + dir[len(dir)-1] + "\n\ngo 1.21\n"
		if err := os.WriteFile(filepath.Join(full, "go.mod"), []byte(mod), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	modules, _ = GoModules(root)
	if len(modules) < 2 {
		t.Fatalf("the fixture produced %d module(s) %v, so every pin here would be vacuous", len(modules), modules)
	}
	return root, modules
}

const passingGoTest = "ok  \texample.com/x\t0.010s\n"

// A gate that measured one module would report honest numbers about part of its
// subject — `./...` is module-scoped, so a change breaking the tools suite would
// pass a gate bin/verify fails. The expectation is GoModules' own answer rather
// than a spelled pair, so a third module added there later cannot be dropped
// here silently: that is the trap GoModules' comment warns about.
func TestTestedGo_MeasuresEveryGoModule(t *testing.T) {
	root, modules := goModulesRoot(t)
	fake := &fakeGoTest{stdout: map[string]string{}}
	for _, dir := range modules {
		fake.stdout[dir] = passingGoTest
	}

	if _, _, err := measureTestedGoWith(root, fake.capture); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fake.dirs, modules) {
		t.Errorf("tested:go visited %v, want every module GoModules reports: %v", fake.dirs, modules)
	}
}

// The gate and bin/verify must not be able to disagree about what "the Go suite"
// is: a gate running a narrower command would answer a question nobody asked
// before landing.
func TestTestedGo_RunsWhatVerifyRuns(t *testing.T) {
	root, modules := goModulesRoot(t)
	fake := &fakeGoTest{stdout: map[string]string{}}
	for _, dir := range modules {
		fake.stdout[dir] = passingGoTest
	}
	if _, _, err := measureTestedGoWith(root, fake.capture); err != nil {
		t.Fatal(err)
	}

	want := append([]string{"go", "test", "-timeout", "30m"}, goTestConcurrencyArgs()...)
	want = append(want, "./...")
	for i, got := range fake.argv {
		if !slices.Equal(got, want) {
			t.Errorf("module %d ran %v, want %v (RunGoTests / RunToolsGoTests)", i, got, want)
		}
	}
}

// Failures are summed across modules, the way unbuildable_go_packages and
// vet_findings already are: one suite's numbers alone are not the tree's.
func TestTestedGo_SumsAcrossModules(t *testing.T) {
	root, modules := goModulesRoot(t)
	fake := &fakeGoTest{
		stdout: map[string]string{
			modules[0]: "--- FAIL: TestOne (0.00s)\n--- FAIL: TestTwo (0.00s)\nFAIL\nFAIL\texample.com/a\t0.012s\n",
			modules[1]: "--- FAIL: TestThree (0.00s)\nFAIL\nFAIL\texample.com/b\t0.004s\n",
		},
		// go test exits non-zero when a test fails, which is a successful
		// measurement of a failing tree — never an error from the gate.
		errs: map[string]error{modules[0]: errors.New("exit status 1"), modules[1]: errors.New("exit status 1")},
	}

	metrics, incomplete, err := measureTestedGoWith(root, fake.capture)
	if err != nil {
		t.Fatalf("failing tests are a measurement, not a failure to measure: %v", err)
	}
	if incomplete != "" {
		t.Errorf("incomplete = %q, want empty", incomplete)
	}
	want := map[string]int64{"go_test_failures": 3, "go_test_packages_failed": 2}
	if len(metrics) != len(want) {
		t.Fatalf("got %d metrics %+v, want %d", len(metrics), metrics, len(want))
	}
	for _, m := range metrics {
		if m.Int != want[m.Name] {
			t.Errorf("%s = %d, want %d", m.Name, m.Int, want[m.Name])
		}
	}
}

// A run that measured every module it has is a COMPLETE run. It used to declare
// itself incomplete unconditionally (T2093), and a baseline never moves from an
// incomplete run — so every go_test_* figure was frozen for as long as that
// reason was returned.
func TestTestedGo_ReportsACompleteRun(t *testing.T) {
	root, modules := goModulesRoot(t)
	fake := &fakeGoTest{stdout: map[string]string{}}
	for _, dir := range modules {
		fake.stdout[dir] = passingGoTest
	}

	metrics, incomplete, err := measureTestedGoWith(root, fake.capture)
	if err != nil {
		t.Fatal(err)
	}
	if incomplete != "" {
		t.Errorf("incomplete = %q, want empty: every module ran", incomplete)
	}
	for _, m := range metrics {
		if m.Int != 0 {
			t.Errorf("%s = %d, want 0", m.Name, m.Int)
		}
	}
}

// A run that did not happen is not "zero failing tests", and the reader needs to
// know WHICH module failed to run — the sweep makes that ambiguous otherwise.
// The detail comes from stderr, where go reports a module or toolchain failure;
// reading stdout alone left this message empty in exactly the case it exists for.
func TestTestedGo_ARunThatDidNotHappenNamesTheModule(t *testing.T) {
	root, modules := goModulesRoot(t)
	fake := &fakeGoTest{
		stdout: map[string]string{modules[0]: passingGoTest},
		stderr: map[string]string{modules[1]: "go: cannot find main module\nsecond line\n"},
		errs:   map[string]error{modules[1]: errors.New("exit status 1")},
	}

	_, _, err := measureTestedGoWith(root, fake.capture)
	if err == nil {
		t.Fatal("a module whose suite never ran was reported as zero failures")
	}
	if !strings.Contains(err.Error(), modules[1]) {
		t.Errorf("error %q does not name the module that failed to run (%s)", err, modules[1])
	}
	if !strings.Contains(err.Error(), "go: cannot find main module") {
		t.Errorf("error %q carries no detail; go reports this kind of failure on stderr", err)
	}
	if strings.Contains(err.Error(), "second line") {
		t.Errorf("error %q quotes more than the first line", err)
	}
}

// writeTrivialModule puts a one-package Go module at dir, with a single test
// that passes or fails as asked. Trivial so that running it for real costs a
// compile of two files.
func writeTrivialModule(t *testing.T, dir, pkg string, pass bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "package " + pkg + "\n\nimport \"testing\"\n\nfunc TestIt(t *testing.T) {}\n"
	if !pass {
		body = "package " + pkg + "\n\nimport \"testing\"\n\nfunc TestIt(t *testing.T) { t.Error(\"boom\") }\n"
	}
	for base, content := range map[string]string{
		"go.mod":         "module example.com/" + pkg + "\n\ngo 1.21\n",
		pkg + "_test.go": body,
	} {
		if err := os.WriteFile(filepath.Join(dir, base), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Everything above stands in for the `go test` child, which leaves one thing
// unproven: that "--- FAIL:" and "FAIL\t" are what the toolchain actually
// prints, and on stdout. Canned output written by the same hand as the parser
// agrees with itself by construction, so this runs two real trivial modules —
// one passing, one failing — through the real measureTestedGo.
//
// A temp root, never the repository: `go test ./...` here would be this suite.
func TestTestedGo_CountsRealGoTestOutput(t *testing.T) {
	root := t.TempDir()
	writeTrivialModule(t, filepath.Join(root, "compiler"), "compiler", true)
	writeTrivialModule(t, filepath.Join(root, "tools", "build"), "build", false)

	metrics, incomplete, err := measureTestedGo(root)
	if err != nil {
		t.Fatalf("a module that ran and failed a test is a measurement: %v", err)
	}
	if incomplete != "" {
		t.Errorf("incomplete = %q, want empty", incomplete)
	}
	want := map[string]int64{"go_test_failures": 1, "go_test_packages_failed": 1}
	for _, m := range metrics {
		if m.Int != want[m.Name] {
			t.Errorf("%s = %d, want %d — the real `go test` output is not parsed as expected", m.Name, m.Int, want[m.Name])
		}
	}
}

// With nothing on stderr the detail falls back to stdout rather than being
// blank: "the run did not happen" with no reason sends the reader nowhere.
func TestTestedGo_ARunThatDidNotHappenFallsBackToStdout(t *testing.T) {
	root, modules := goModulesRoot(t)
	fake := &fakeGoTest{
		stdout: map[string]string{modules[0]: "build constraints exclude all Go files\n"},
		errs:   map[string]error{modules[0]: errors.New("exit status 1")},
	}

	_, _, err := measureTestedGoWith(root, fake.capture)
	if err == nil || !strings.Contains(err.Error(), "build constraints exclude all Go files") {
		t.Fatalf("err = %v, want the stdout detail", err)
	}
}

// vet_findings is the SIZE of the list bin/check prints, over the real
// repository — not a second count reached its own way. Asserted through the two
// entry points a person and a runner actually use, so a future change that gave
// either mode a measurement of its own fails here.
//
// A hand-spelled expectation would not do: it is a third spelling, and would go
// on passing while the two it claims to describe drifted apart. That is exactly
// what happened — the tool excluded the generated parser, the gate did not, and
// the gate's verdict came to depend on the host's Go release (T2104).
func TestCheckedGo_CountsExactlyWhatBinCheckReports(t *testing.T) {
	root, err := RootForTests()
	if err != nil {
		t.Skip("not inside the promise repo:", err)
	}

	findings, toolIncomplete, err := GoCheckFindings(root, captureSplit)
	if err != nil {
		t.Fatalf("bin/check mode: %v", err)
	}
	metrics, gateIncomplete, err := measureCheckedGoWith(root, captureSplit)
	if err != nil {
		t.Fatalf("checked:go mode: %v", err)
	}

	if len(metrics) != 1 || metrics[0].Name != "vet_findings" {
		t.Fatalf("checked:go reported %+v, want one vet_findings metric", metrics)
	}
	if got, want := metrics[0].Int, int64(len(findings)); got != want {
		t.Errorf("vet_findings = %d, but bin/check reports %d findings: %v", got, want, findings)
	}
	if toolIncomplete != gateIncomplete {
		t.Errorf("bin/check says incomplete %q, checked:go says %q", toolIncomplete, gateIncomplete)
	}
}

// A gate that cannot list a module's packages has not measured zero findings —
// it has not measured. Reporting 0 there would read as a clean tree and let a
// change land on a number nobody produced.
func TestCheckedGo_ARunThatCouldNotHappenIsNotZeroFindings(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "compiler"), 0o755); err != nil {
		t.Fatal(err)
	}
	metrics, _, err := measureCheckedGoWith(root, vetSaying(""))
	if err == nil {
		t.Fatalf("got metrics %+v, want an error: the module could not be listed", metrics)
	}
}

// checked:go reaches this measurement through the contract registry, which is
// the path bin/gate takes. Without this, every pin on the measurement could hold
// while the gate name resolved somewhere else entirely.
func TestCheckedGo_IsWhatTheContractGateMeasures(t *testing.T) {
	root, err := RootForTests()
	if err != nil {
		t.Skip("not inside the promise repo:", err)
	}
	env, err := MeasureContractGate(root, "checked:go")
	if err != nil {
		t.Fatalf("MeasureContractGate: %v", err)
	}
	if env.Gate != "checked:go" {
		t.Errorf("gate = %q", env.Gate)
	}
	if len(env.Metrics) != 1 || env.Metrics[0].Name != "vet_findings" {
		t.Fatalf("metrics = %+v, want one vet_findings", env.Metrics)
	}
	findings, _, err := GoCheckFindings(root, captureSplit)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := env.Metrics[0].Int, int64(len(findings)); got != want {
		t.Errorf("the gate reported %d, bin/check reports %d findings: %v", got, want, findings)
	}
}

// builds sweeps the same module list, so it inherits the same duty to say when
// one of them could not be measured — a complete-looking envelope from a partial
// sweep is what moves a baseline it should not.
func TestBuilds_NamesAModuleItCouldNotMeasure(t *testing.T) {
	root := t.TempDir()
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	compiler := filepath.Join(root, "compiler")
	write(compiler, "go.mod", "module example.com/compiler\n\ngo 1.25.6\n")
	write(compiler, "x.go", "package compiler\n\nfunc F() {}\n")
	// flows/ with no flow-sdk/ beside it: present, unresolvable, left out.
	write(filepath.Join(root, "flows"), "go.mod", "module example.com/flows\n\ngo 1.21\n")

	metrics, incomplete, err := measureBuilds(root)
	if err != nil {
		t.Fatalf("measureBuilds: %v", err)
	}
	if incomplete == "" || !strings.Contains(incomplete, "flows/") {
		t.Errorf("incomplete = %q, want it to name flows/", incomplete)
	}
	for _, m := range metrics {
		if m.Int != 0 {
			t.Errorf("%s = %d, want 0 for a tree that builds", m.Name, m.Int)
		}
	}
}

// And the complementary half: a tree with every module it has is COMPLETE, so
// the baseline may move from it.
func TestBuilds_AWholeSweepIsComplete(t *testing.T) {
	root := t.TempDir()
	compiler := filepath.Join(root, "compiler")
	if err := os.MkdirAll(compiler, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"go.mod": "module example.com/compiler\n\ngo 1.25.6\n",
		"x.go":   "package compiler\n\nfunc F() {}\n",
	} {
		if err := os.WriteFile(filepath.Join(compiler, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, incomplete, err := measureBuilds(root)
	if err != nil {
		t.Fatalf("measureBuilds: %v", err)
	}
	if incomplete != "" {
		t.Errorf("incomplete = %q, want empty: every module was measured", incomplete)
	}
}
