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

// goEvents is a `go test -json` stream, one event per line.
func goEvents(events ...string) string { return strings.Join(events, "\n") + "\n" }

// passingGoTest is a module whose one package passed, with no tests to count.
var passingGoTest = goEvents(`{"Action":"pass","Package":"example.com/x","Elapsed":0.01}`)

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

	if _, err := measureTestedGoWith(root, fake.capture); err != nil {
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
	if _, err := measureTestedGoWith(root, fake.capture); err != nil {
		t.Fatal(err)
	}

	// -json changes what it prints, not what it runs: it is the form that says
	// which test each line belongs to.
	want := append([]string{"go", "test", "-timeout", "30m"}, goTestConcurrencyArgs()...)
	want = append(want, "-json", "./...")
	for i, got := range fake.argv {
		if !slices.Equal(got, want) {
			t.Errorf("module %d ran %v, want %v (RunGoTests / RunToolsGoTests, plus -json)", i, got, want)
		}
	}
}

// Failures are summed across modules, the way go_unbuildable_packages and
// go_check_findings already are: one suite's numbers alone are not the tree's.
func TestTestedGo_SumsAcrossModules(t *testing.T) {
	root, modules := goModulesRoot(t)
	fake := &fakeGoTest{
		stdout: map[string]string{
			modules[0]: goEvents(
				`{"Action":"fail","Package":"example.com/a","Test":"TestOne"}`,
				`{"Action":"fail","Package":"example.com/a","Test":"TestTwo"}`,
				`{"Action":"fail","Package":"example.com/a","Elapsed":0.012}`,
			),
			modules[1]: goEvents(
				`{"Action":"fail","Package":"example.com/b","Test":"TestThree"}`,
				`{"Action":"fail","Package":"example.com/b","Elapsed":0.004}`,
			),
		},
		// go test exits non-zero when a test fails, which is a successful
		// measurement of a failing tree — never an error from the gate.
		errs: map[string]error{modules[0]: errors.New("exit status 1"), modules[1]: errors.New("exit status 1")},
	}

	meas, err := measureTestedGoWith(root, fake.capture)
	metrics, incomplete := meas.metrics, meas.incomplete
	if err != nil {
		t.Fatalf("failing tests are a measurement, not a failure to measure: %v", err)
	}
	if incomplete != "" {
		t.Errorf("incomplete = %q, want empty", incomplete)
	}
	want := map[string]int64{
		"failed_tests": 3, "go_failed_tests": 3,
		"failed_packages": 2, "go_failed_packages": 2,
		"test_count": 0, "go_test_count": 0,
	}
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

	meas, err := measureTestedGoWith(root, fake.capture)
	metrics, incomplete := meas.metrics, meas.incomplete
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

	_, err := measureTestedGoWith(root, fake.capture)
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
	// measureTestedGo builds before it measures, and this fixture is not a tree
	// anything could build. What is under test is how `go test` output is read.
	stubGateBuild(t, &fakeBuild{})
	root := t.TempDir()
	writeTrivialModule(t, filepath.Join(root, "compiler"), "compiler", true)
	writeTrivialModule(t, filepath.Join(root, "tools", "build"), "build", false)

	meas, err := measureTestedGo(root)
	metrics, incomplete := meas.metrics, meas.incomplete
	if err != nil {
		t.Fatalf("a module that ran and failed a test is a measurement: %v", err)
	}
	if incomplete != "" {
		t.Errorf("incomplete = %q, want empty", incomplete)
	}
	want := map[string]int64{
		"failed_tests": 1, "go_failed_tests": 1,
		"failed_packages": 1, "go_failed_packages": 1,
		"test_count": 1, "go_test_count": 1,
	}
	if len(metrics) != len(want) {
		t.Fatalf("got %d metrics %+v, want %d", len(metrics), metrics, len(want))
	}
	for _, m := range metrics {
		if m.Int != want[m.Name] {
			t.Errorf("%s = %d, want %d — the real `go test` output is not parsed as expected", m.Name, m.Int, want[m.Name])
		}
	}
}

// A test that runs `go test` on a fixture and prints what that child said —
// its `--- FAIL:` lines included — has not failed: in a -json stream that text
// is the output of the test that printed it, and its own result is what counts.
// In -v text it was indistinguishable from a failure, which is how a passing
// tools suite reported a failing test.
func TestParseGoTestJSON_AChildsOutputIsNotATestResult(t *testing.T) {
	run := parseGoTestJSON(goEvents(
		`{"Action":"run","Package":"example.com/p","Test":"TestRunsAFixture"}`,
		`{"Action":"output","Package":"example.com/p","Test":"TestRunsAFixture","Output":"--- FAIL: TestIt (0.00s)\n"}`,
		`{"Action":"output","Package":"example.com/p","Test":"TestRunsAFixture","Output":"FAIL\texample.com/build\t0.002s\n"}`,
		`{"Action":"pass","Package":"example.com/p","Test":"TestRunsAFixture","Elapsed":0.4}`,
		`{"Action":"pass","Package":"example.com/p","Test":"TestRunsAFixture/sub","Elapsed":0.1}`,
		`{"Action":"pass","Package":"example.com/p","Elapsed":0.5}`,
	))
	if run.failedTests != 0 || run.failedPackages != 0 || run.passedTests != 1 || len(run.groups) != 0 {
		t.Errorf("run = %+v, want one passing top-level test and nothing failed", run)
	}
}

// A failing test is a member in its package's group, carrying what it — and
// the subtest that failed it — printed, without go's own framing lines; a
// package that failed to build is a member too, carrying the compiler's words.
func TestParseGoTestJSON_FailuresCarryTheirOwnWords(t *testing.T) {
	run := parseGoTestJSON(goEvents(
		`{"Action":"run","Package":"github.com/promise-language/promise/compiler/internal/codegen","Test":"TestLower"}`,
		`{"Action":"output","Package":"github.com/promise-language/promise/compiler/internal/codegen","Test":"TestLower","Output":"=== RUN   TestLower\n"}`,
		`{"Action":"output","Package":"github.com/promise-language/promise/compiler/internal/codegen","Test":"TestLower/call","Output":"    lower_test.go:41: got i32, want i64\n"}`,
		`{"Action":"output","Package":"github.com/promise-language/promise/compiler/internal/codegen","Test":"TestLower/call","Output":"    --- FAIL: TestLower/call (0.10s)\n"}`,
		`{"Action":"fail","Package":"github.com/promise-language/promise/compiler/internal/codegen","Test":"TestLower/call","Elapsed":0.1}`,
		`{"Action":"output","Package":"github.com/promise-language/promise/compiler/internal/codegen","Test":"TestLower","Output":"--- FAIL: TestLower (0.12s)\n"}`,
		`{"Action":"fail","Package":"github.com/promise-language/promise/compiler/internal/codegen","Test":"TestLower","Elapsed":0.12}`,
		`{"Action":"fail","Package":"github.com/promise-language/promise/compiler/internal/codegen","Elapsed":0.2}`,
		`{"ImportPath":"example.com/broken [example.com/broken.test]","Action":"build-output","Output":"broken.go:3:1: syntax error\n"}`,
		`{"ImportPath":"example.com/broken [example.com/broken.test]","Action":"build-fail"}`,
		`{"Action":"fail","Package":"example.com/broken","Elapsed":0,"FailedBuild":"example.com/broken [example.com/broken.test]"}`,
		"not an event",
	))
	if run.failedTests != 1 || run.failedPackages != 2 {
		t.Fatalf("run = %+v, want one failing test and two failing packages", run)
	}
	got := mustJSON(t, run.groups)
	want := mustJSON(t, []Group{
		{Name: "compiler/internal/codegen", Members: []Member{{Name: "TestLower", Outcome: OutcomeFail, Detail: "lower_test.go:41: got i32, want i64\n--- FAIL: TestLower/call (0.10s)", Elapsed: 0.12}}},
		{Name: "example.com/broken", Members: []Member{{Name: "(package)", Outcome: OutcomeFail, Detail: "broken.go:3:1: syntax error"}}},
	})
	if got != want {
		t.Errorf("groups =\n%s\nwant\n%s", got, want)
	}
}

// A test that never reports a result — it hung past -timeout, or called
// os.Exit — gets no fail event of its own, only its package does. It failed
// all the same, and is counted and named with what it printed; one that hung
// is a timeout. A test that finished is not blamed.
func TestParseGoTestJSON_ATestThatNeverReportedFailed(t *testing.T) {
	const pkg = "example.com/fix/tmo"
	run := parseGoTestJSON(goEvents(
		`{"Action":"run","Package":"`+pkg+`","Test":"TestFine"}`,
		`{"Action":"pass","Package":"`+pkg+`","Test":"TestFine"}`,
		`{"Action":"run","Package":"`+pkg+`","Test":"TestHang"}`,
		`{"Action":"output","Package":"`+pkg+`","Test":"TestHang","Output":"=== RUN   TestHang\n"}`,
		`{"Action":"output","Package":"`+pkg+`","Test":"TestHang","Output":"panic: test timed out after 3s\n\trunning tests:\n\t\tTestHang (3s)\n"}`,
		`{"Action":"output","Package":"`+pkg+`","Output":"FAIL\texample.com/fix/tmo\t3.007s\n"}`,
		`{"Action":"fail","Package":"`+pkg+`","Elapsed":3.007}`,
		`{"Action":"run","Package":"example.com/fix/exit","Test":"TestExit"}`,
		`{"Action":"output","Package":"example.com/fix/exit","Test":"TestExit","Output":"exit_test.go:9: leaving\n"}`,
		`{"Action":"fail","Package":"example.com/fix/exit","Elapsed":0.01}`,
	))
	if run.failedTests != 2 || run.passedTests != 1 || run.failedPackages != 2 {
		t.Fatalf("run = %+v, want two failing tests, one passing, two failing packages", run)
	}
	got := mustJSON(t, run.groups)
	want := mustJSON(t, []Group{
		{Name: pkg, Members: []Member{{Name: "TestHang", Outcome: OutcomeTimeout,
			Detail: "panic: test timed out after 3s\nrunning tests:\nTestHang (3s)\nFAIL\texample.com/fix/tmo\t3.007s"}}},
		{Name: "example.com/fix/exit", Members: []Member{{Name: "TestExit", Outcome: OutcomeFail, Detail: "exit_test.go:9: leaving"}}},
	})
	if got != want {
		t.Errorf("groups =\n%s\nwant\n%s", got, want)
	}
}

// What a person watching sees is the text the tests printed, not the events.
func TestGoTestNarrator_WritesTheOutputText(t *testing.T) {
	var b strings.Builder
	n := &goTestNarrator{to: &b}
	n.Write([]byte(`{"Action":"output","Output":"=== RUN   TestA\n"}` + "\n" + `{"Action":"pa`))
	n.Write([]byte(`ss","Test":"TestA"}` + "\n" + "go: downloading x\n"))
	n.Write([]byte(`{"Action":"output","Output":"ok\n"}`))
	n.flush()
	if got, want := b.String(), "=== RUN   TestA\ngo: downloading x\nok\n"; got != want {
		t.Errorf("narration = %q, want %q", got, want)
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

	_, err := measureTestedGoWith(root, fake.capture)
	if err == nil || !strings.Contains(err.Error(), "build constraints exclude all Go files") {
		t.Fatalf("err = %v, want the stdout detail", err)
	}
}

// go_check_findings is the SIZE of the list bin/check prints, over the real
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
	// checked:go builds before it measures (T2102); what is under test is that
	// the two modes agree, not the build. The stub keeps this from compiling the
	// repository from inside its own `go test ./...`.
	stubGateBuild(t, &fakeBuild{})

	findings, toolIncomplete, err := GoCheckFindings(root, captureSplit)
	if err != nil {
		t.Fatalf("bin/check mode: %v", err)
	}
	meas, err := measureCheckedGoWith(root, captureSplit)
	metrics, gateIncomplete := meas.metrics, meas.incomplete
	if err != nil {
		t.Fatalf("checked:go mode: %v", err)
	}

	got := metricInts(metrics)
	if len(got) != 2 {
		t.Fatalf("checked:go reported %+v, want check_findings and go_check_findings", metrics)
	}
	if got["go_check_findings"] != int64(len(findings)) || got["check_findings"] != int64(len(findings)) {
		t.Errorf("checked:go reported %v, but bin/check reports %d findings: %v", got, len(findings), findings)
	}
	if toolIncomplete != gateIncomplete {
		t.Errorf("bin/check says incomplete %q, checked:go says %q", toolIncomplete, gateIncomplete)
	}
}

// A gate that cannot list a module's packages has not measured zero findings —
// it has not measured. Reporting 0 there would read as a clean tree and let a
// change land on a number nobody produced.
func TestCheckedGo_ARunThatCouldNotHappenIsNotZeroFindings(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "compiler"), 0o755); err != nil {
		t.Fatal(err)
	}
	meas, err := measureCheckedGoWith(root, vetSaying(""))
	metrics := meas.metrics
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
	stubGateBuild(t, &fakeBuild{})
	env, err := MeasureContractGate(root, "checked:go")
	if err != nil {
		t.Fatalf("MeasureContractGate: %v", err)
	}
	if env.Gate != "checked:go" {
		t.Errorf("gate = %q", env.Gate)
	}
	if got := metricInts(env.Metrics); len(got) != 2 || got["check_findings"] != got["go_check_findings"] {
		t.Fatalf("metrics = %+v, want check_findings and go_check_findings", env.Metrics)
	}
	findings, _, err := GoCheckFindings(root, captureSplit)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := metricInts(env.Metrics)["go_check_findings"], int64(len(findings)); got != want {
		t.Errorf("the gate reported %d, bin/check reports %d findings: %v", got, want, findings)
	}
}

// builds sweeps the same module list, so it inherits the same duty to say when
// one of them could not be measured — a complete-looking envelope from a partial
// sweep is what moves a baseline it should not.
func TestBuilds_NamesAModuleItCouldNotMeasure(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
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

	meas, err := measureBuilds(root)
	metrics, incomplete := meas.metrics, meas.incomplete
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
	stubGateBuild(t, &fakeBuild{})
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
	meas, err := measureBuilds(root)
	incomplete := meas.incomplete
	if err != nil {
		t.Fatalf("measureBuilds: %v", err)
	}
	if incomplete != "" {
		t.Errorf("incomplete = %q, want empty: every module was measured", incomplete)
	}
}
