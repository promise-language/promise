package common

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fakeBuild stands in for RunBuild, recording every call. The real one compiles
// this whole repository, and running it from inside `go test ./...` in
// tools/build — which IS what tested:go runs — would build the very tree
// another process may be measuring.
type fakeBuild struct {
	err   error
	roots []string
	args  [][]string
	// print is written to os.Stdout when the build runs, which is how the
	// stdout-redirect pin proves the redirect is real.
	print string
}

func (f *fakeBuild) run(root string, args []string) error {
	f.roots = append(f.roots, root)
	f.args = append(f.args, args)
	if f.print != "" {
		fmt.Println(f.print)
	}
	return f.err
}

// No test in this package runs the real build, and this is what makes that true
// rather than remembered. RunBuild compiles the whole repository — minutes, a
// network fetch of the ANTLR jar, a write to bin/ and to the gate-values
// sidecar — and `go test ./...` in tools/build IS what tested:go runs, so a
// test that built would be building the very tree another process is measuring.
// A test that wants a build installs its own (stubGateBuild); one that reaches
// a measurement without doing so is told exactly that, here, rather than
// wandering off into a full build nobody asked for.
func init() {
	runGateBuild = func(string, []string) error {
		panic("a test reached the gate's build without stubbing it — call stubGateBuild(t, …); the real RunBuild compiles this repository")
	}
}

// stubGateBuild installs fake as the build for one test and resets the
// once-per-process state around it, so the order tests run in cannot decide
// what any of them measures.
func stubGateBuild(t *testing.T, fake *fakeBuild) {
	t.Helper()
	savedFunc, savedState := runGateBuild, gateBuild
	runGateBuild, gateBuild = fake.run, &onceBuild{}
	t.Cleanup(func() { runGateBuild, gateBuild = savedFunc, savedState })
}

// stubMachineGates stands in for the two measurements whose subject is a
// published release rather than this tree. Unstubbed they reach the network and
// one of them installs a toolchain — inside `go test ./...`, which is the very
// thing tested:go measures.
func stubMachineGates(t *testing.T) {
	t.Helper()
	savedPhases, savedLatest := installPhases, latestIsEpoch
	installPhases = func(root, work, variant, channel string, system bool) error { return nil }
	latestIsEpoch = func() error { return nil }
	t.Cleanup(func() { installPhases, latestIsEpoch = savedPhases, savedLatest })
}

// buildFails is the stub every gate-shape test below uses. A FAILING build
// makes the whole table affordable: each measurement that reads build
// artifacts short-circuits to a reason without spawning a child, so a pin
// about which gates build costs no compiling at all.
func buildFails() *fakeBuild { return &fakeBuild{err: errors.New("no LLVM found")} }

// A composition must not build once per part. integration expands to six
// measuring gates, and six builds of the same tree would be five full compiles
// spent learning nothing — which is also how two of them could end up
// describing different trees.
func TestContractGate_BuildsOnceInProcess(t *testing.T) {
	fake := buildFails()
	stubGateBuild(t, fake)

	if _, err := MeasureContractGate(t.TempDir(), "integration"); err != nil {
		t.Fatalf("integration: %v", err)
	}
	if gateBuild.runs != 1 {
		t.Errorf("the build ran %d times, want exactly 1 for the whole composition", gateBuild.runs)
	}
}

// No flags: RunBuild's own quick up-to-date check is skipped for -release and
// -generate, so passing either would make every gate run pay for a full build
// of an already-built tree.
func TestGateBuild_PassesNoFlags(t *testing.T) {
	fake := buildFails()
	stubGateBuild(t, fake)
	root := t.TempDir()

	if _, err := MeasureContractGate(root, "builds"); err != nil {
		t.Fatal(err)
	}
	if len(fake.args) != 1 {
		t.Fatalf("the build ran %d times, want 1", len(fake.args))
	}
	if fake.args[0] != nil {
		t.Errorf("the build was passed %v, want no flags at all — RunBuild's quick up-to-date check does not apply to -release/-generate", fake.args[0])
	}
	if fake.roots[0] != root {
		t.Errorf("the build ran against %q, want the root being measured (%q)", fake.roots[0], root)
	}
}

// Every gate measures the tree as it is now, so every gate builds first — fit
// excepted, because it measures the MACHINE and must be answerable on one that
// cannot build. Derived from the registry rather than spelled, so a gate added
// later is covered here without anyone remembering to add it.
func TestContractGate_EveryTreeGateBuilds(t *testing.T) {
	for _, name := range ContractGateNames() {
		t.Run(name, func(t *testing.T) {
			fake := buildFails()
			stubGateBuild(t, fake)

			stubMachineGates(t)

			if _, err := MeasureContractGate(t.TempDir(), name); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			// Derived from the registry, not spelled: measuresMachine IS the
			// declaration that a gate's subject is not the tree, so a gate
			// added later is covered here without anyone remembering to.
			want := 1
			if contractGates[name].measuresMachine {
				want = 0
			}
			if gateBuild.runs != want {
				t.Errorf("%s built %d times, want %d", name, gateBuild.runs, want)
			}
		})
	}
}

// The build is bin/build's own RunBuild, called in this process. Spawning
// bin/build would measure whichever bin/build is on disk — the same staleness
// this fixes, one level up — and bin/gate's own staleness is already CheckStale's
// job. Read from the source because the alternative is proving the absence of a
// process, which no run can do.
func TestGateBuild_IsRunBuildInProcess(t *testing.T) {
	src := gateSources(t)

	// Exactly one call site, and it is the seam's default.
	if n := strings.Count(src, "runGateBuild = RunBuild"); n != 1 {
		t.Errorf("found %d `runGateBuild = RunBuild` bindings, want exactly 1: the gate's build must be RunBuild itself", n)
	}
	// RunBuild(…) appears in the legacy bin/gate subcommands too (gate.go), so
	// what is pinned is that no gate source spawns the SCRIPT.
	spawnsScript := regexp.MustCompile(`"bin/build"|"build"\+ExeSuffix|BuildScript`)
	if m := spawnsScript.FindString(src); m != "" {
		t.Errorf("gate source names the bin/build script (%s); the gate must call RunBuild in-process", m)
	}
}

// gateSources concatenates this package's non-test sources. Same idea as
// reportedMetricNames: reading the source is what makes a whole-package
// assertion affordable.
func gateSources(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(body)
	}
	if b.Len() == 0 {
		t.Fatal("scanned the package and read no source at all — every check built on this scan has quietly stopped checking")
	}
	return b.String()
}

// RunBuild prints progress with fmt.Println, and this process's stdout carries
// the envelope and nothing else: one build line in front of it and the whole
// stream stops parsing, which a runner reports as the gate breaking its
// contract rather than as a tree that needed building.
func TestGateBuild_ProgressNeverReachesStdout(t *testing.T) {
	stubGateBuild(t, &fakeBuild{print: "Building promise (version: test)..."})
	root := t.TempDir()
	writeGoModule(t, filepath.Join(root, "compiler"), "compiler", "package compiler\n")

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	savedStdout := os.Stdout
	os.Stdout = write
	var envelope strings.Builder
	gateErr := runContractGate(root, []string{"builds", "--envelope"}, &envelope)
	os.Stdout = savedStdout
	write.Close()
	leaked, _ := io.ReadAll(read)
	read.Close()

	if gateErr != nil {
		t.Fatalf("builds: %v", gateErr)
	}
	if len(leaked) != 0 {
		t.Errorf("build progress reached stdout: %q", leaked)
	}
	var env Envelope
	if err := json.Unmarshal([]byte(envelope.String()), &env); err != nil {
		t.Fatalf("the gate's stdout is not one JSON object (%v): %q", err, envelope.String())
	}
	if env.Gate != "builds" {
		t.Errorf("envelope names gate %q, want builds", env.Gate)
	}
}

// A build that does not complete is a fact about this tree, reported as a
// number by a name that says so. Returned as an error it read as "integration
// did not measure anything" — a broken gate rather than a broken change, which
// sends the reader to the wrong code.
func TestBuilds_BuildFailureIsAMeasurement(t *testing.T) {
	stubGateBuild(t, buildFails())

	metrics, incomplete, err := measureBuilds(t.TempDir())
	if err != nil {
		t.Fatalf("a build that failed is a measurement, not a failure to measure: %v", err)
	}
	if incomplete == "" {
		t.Error("incomplete is empty; the per-package sweep did not run, and a run that measured less than a full one must say so")
	}
	byName := map[string]int64{}
	for _, m := range metrics {
		byName[m.Name] = m.Int
	}
	if byName["build_failures"] != 1 {
		t.Errorf("build_failures = %d, want 1", byName["build_failures"])
	}
	if _, reported := byName["unbuildable_go_packages"]; reported {
		t.Error("unbuildable_go_packages was reported after a failed build; the sweep did not run, and a zero there is a number about nothing")
	}
}

// The reason must name the build failure, not just say one happened.
func TestBuilds_BuildFailureNamesTheReason(t *testing.T) {
	stubGateBuild(t, &fakeBuild{err: errors.New("generate parser: java not found")})

	_, incomplete, err := measureBuilds(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(incomplete, "java not found") {
		t.Errorf("incomplete = %q, want the build's own diagnostic", incomplete)
	}
}

// Every part that reads build artifacts declines to report numbers when the
// build failed, rather than reporting numbers about the previous build. Which
// parts those are is the point: formatted:go is deliberately NOT one, because
// gofmt needs no build and still has an honest answer.
func TestContractGate_BuildDependentPartsReportNothing(t *testing.T) {
	for _, name := range []string{"formatted:promise", "checked:go", "tested:go", "tested:promise"} {
		t.Run(name, func(t *testing.T) {
			stubGateBuild(t, buildFails())
			env, err := MeasureContractGate(t.TempDir(), name)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if len(env.Metrics) != 0 {
				t.Errorf("%s reported %+v after a failed build; those numbers are about the previous build", name, env.Metrics)
			}
			if env.Incomplete == "" {
				t.Errorf("%s reported no numbers and no reason", name)
			}
		})
	}
}

// formatted:go has an answer with or without a build, and withholding it would
// make a build failure look like a formatting failure too.
func TestFormattedGo_MeasuresWithoutABuild(t *testing.T) {
	stubGateBuild(t, buildFails())

	env, err := MeasureContractGate(t.TempDir(), "formatted:go")
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Metrics) != 1 || env.Metrics[0].Name != "unformatted_go_files" {
		t.Errorf("formatted:go reported %+v, want unformatted_go_files even though the build failed", env.Metrics)
	}
	if env.Incomplete != "" {
		t.Errorf("incomplete = %q, want empty: gofmt needs no build", env.Incomplete)
	}
}

// A build that reports success but leaves no compiler must not be reported as a
// clean formatting result. UnformattedPromiseFiles answers "nothing to reformat"
// for a MISSING formatter exactly as it does for a clean tree, so dropping the
// old bin/promise guard in favour of the build outcome alone would turn that
// into unformatted_promise_files = 0 — a number about nothing, which is the
// failure this whole item is about.
func TestFormattedPromise_ABuiltTreeWithNoCompilerIsNotACleanZero(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})

	metrics, incomplete, err := measureFormattedPromise(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 0 {
		t.Errorf("reported %+v with no formatter present; there was nothing to measure with", metrics)
	}
	if !strings.Contains(incomplete, "Promise formatting was not measured") {
		t.Errorf("incomplete = %q, want a reason naming what was not measured", incomplete)
	}
}

// And with the compiler in place it reports a number. The fixture has no .pr
// files, so UnformattedPromiseFiles returns before it ever runs the binary —
// which is what keeps this portable and free.
func TestFormattedPromise_ReportsANumberOnceBuilt(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", BinaryName()), []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}

	metrics, incomplete, err := measureFormattedPromise(root)
	if err != nil {
		t.Fatal(err)
	}
	if incomplete != "" {
		t.Errorf("incomplete = %q, want empty: the compiler is there and the tree has no .pr files", incomplete)
	}
	if len(metrics) != 1 || metrics[0].Name != "unformatted_promise_files" || metrics[0].Int != 0 {
		t.Errorf("got %+v, want unformatted_promise_files = 0", metrics)
	}
}

// The stale-parser refusal is gone: the gate now does exactly what bin/build
// does, regenerating the parser rather than declining to build over it. The
// build outcome is the only thing that can stop the Promise suite now.
func TestTestedPromise_BuildOutcomeIsTheOnlyRefusal(t *testing.T) {
	stubGateBuild(t, buildFails())

	metrics, incomplete, err := measureTestedPromise(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 0 {
		t.Errorf("reported %+v after a failed build", metrics)
	}
	if !strings.Contains(incomplete, "the Promise suite did not run") {
		t.Errorf("incomplete = %q, want the build-outcome reason", incomplete)
	}
	if strings.Contains(incomplete, "parser is stale") {
		t.Errorf("incomplete = %q; the staleness refusal was removed — the build regenerates the parser", incomplete)
	}
	if src := gateSources(t); strings.Contains(src, "staleGeneratedFiles") {
		t.Error("staleGeneratedFiles survives; after the build the count can only be 0, so it is a number that cannot move")
	}
}

// Which summary field becomes which metric. Three numbers of the same type in
// one line are exactly what a transposition hides: host_test_failures carrying
// the leak count would still be a plausible-looking envelope, and the baselines
// for both are 0, so nothing downstream would notice until a real failure was
// reported as a leak.
func TestTestedPromise_MapsTheSummaryToItsMetrics(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	const output = "some progress\n11186 passed, 3 failed, 2 leaked (117 files, 30.810s)\n"

	metrics, incomplete, err := measureTestedPromiseWith(t.TempDir(),
		func(string, string) (string, error) { return output, nil })
	if err != nil {
		t.Fatalf("failing tests are a measurement: %v", err)
	}
	if incomplete != "" {
		t.Errorf("incomplete = %q, want empty", incomplete)
	}
	want := map[string]int64{"host_test_failures": 3, "host_leak_count": 2, "host_test_count": 11186}
	if len(metrics) != len(want) {
		t.Fatalf("got %d metrics %+v, want %d", len(metrics), metrics, len(want))
	}
	for _, m := range metrics {
		if m.Int != want[m.Name] {
			t.Errorf("%s = %d, want %d", m.Name, m.Int, want[m.Name])
		}
	}
}

// A suite that printed no summary measured nothing, and zero failures is not
// what that means: an envelope of zeroes from a suite that never ran is a pass
// nobody earned.
func TestTestedPromise_NoSummaryIsNotZeroFailures(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})

	metrics, _, err := measureTestedPromiseWith(t.TempDir(),
		func(string, string) (string, error) { return "the binary crashed\n", nil })
	if err == nil {
		t.Fatalf("a suite that printed no summary measured %+v", metrics)
	}
	if !strings.Contains(err.Error(), "no summary line") {
		t.Errorf("error %q does not say what was missing", err)
	}
}

// Which summary field becomes which metric, for the Promise checker. Four
// counts of the same type in one line are what a transposition hides: a
// failures count carrying the unit count would still look like a plausible
// envelope, and nothing downstream could tell.
func TestCheckedPromise_MapsTheSummaryToItsMetrics(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	root := rootWithCompiler(t)
	const output = "FAIL modules/std (18 errors)\n\n851 checked, 1 warned, 9 failed (861 units, 27 errors, 1 warning, 42.173s)\n"

	metrics, incomplete, err := measureCheckedPromiseWith(root,
		func(string) (string, error) { return output, nil })
	if err != nil {
		t.Fatalf("diagnostics are a measurement: %v", err)
	}
	if incomplete != "" {
		t.Errorf("incomplete = %q, want empty", incomplete)
	}
	want := map[string]int64{
		"promise_check_failures": 9,
		"promise_check_errors":   27,
		"promise_check_warnings": 1,
		"promise_check_units":    861,
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

// A sweep that printed no summary checked nothing, and zero failures is not
// what that means.
func TestCheckedPromise_NoSummaryIsNotZeroFailures(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	root := rootWithCompiler(t)

	metrics, _, err := measureCheckedPromiseWith(root,
		func(string) (string, error) { return "the compiler crashed\n", nil })
	if err == nil {
		t.Fatalf("a sweep that printed no summary measured %+v", metrics)
	}
	if !strings.Contains(err.Error(), "no summary line") {
		t.Errorf("error %q does not say what was missing", err)
	}
}

// The checker IS the compiler, so a build that reported success but left no
// binary must not be reported as a clean tree — the same guard formatted:promise
// carries, for the same reason.
func TestCheckedPromise_MissingCompilerIsNotClean(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})

	metrics, incomplete, err := measureCheckedPromiseWith(t.TempDir(),
		func(string) (string, error) { t.Fatal("the sweep must not run without a compiler"); return "", nil })
	if err != nil {
		t.Fatalf("a missing binary is a reason, not an error: %v", err)
	}
	if len(metrics) != 0 {
		t.Errorf("metrics = %+v, want none", metrics)
	}
	if !strings.Contains(incomplete, "left no") {
		t.Errorf("incomplete = %q, does not say the binary is missing", incomplete)
	}
}

// A build that did not complete reports no numbers and says why: a count about
// artifacts that were never produced is a number about nothing.
func TestCheckedPromise_BuildFailureReportsNoNumbers(t *testing.T) {
	stubGateBuild(t, buildFails())

	metrics, incomplete, err := measureCheckedPromiseWith(rootWithCompiler(t),
		func(string) (string, error) { t.Fatal("the sweep must not run behind a failed build"); return "", nil })
	if err != nil {
		t.Fatalf("a failed build is a reason, not an error: %v", err)
	}
	if len(metrics) != 0 {
		t.Errorf("metrics = %+v, want none", metrics)
	}
	if !strings.Contains(incomplete, "the Promise check did not run") {
		t.Errorf("incomplete = %q, does not name what was skipped", incomplete)
	}
}

// rootWithCompiler is a temp root holding a stand-in bin/promise, so the
// missing-binary guard is not what a test about something else measures.
func rootWithCompiler(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", BinaryName()), []byte("stand-in"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// writeGoModule puts a one-package module at dir whose only content is body.
func writeGoModule(t *testing.T, dir, pkg, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for base, content := range map[string]string{
		"go.mod":    "module example.com/" + pkg + "\n\ngo 1.21\n",
		pkg + ".go": body,
	} {
		if err := os.WriteFile(filepath.Join(dir, base), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// checked:go still counts a real finding once the build guard is in front of
// it. The guard is what this item added to the front of GoCheckFindings, and a
// guard that short-circuited a successful build would turn the metric off
// without anything saying so.
//
// Real toolchain, real module: what is in question is exactly what `go vet`
// prints, and canned output written by the same hand as the parser agrees with
// itself by construction.
func TestCheckedGo_CountsRealVetFindings(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	root := t.TempDir()
	writeGoModule(t, filepath.Join(root, "compiler"), "compiler",
		"package compiler\n\nimport \"fmt\"\n\nfunc F() { fmt.Printf(\"%d\\n\", \"not an int\") }\n")

	metrics, incomplete, err := measureCheckedGo(root)
	if err != nil {
		t.Fatalf("a vet finding is a measurement: %v", err)
	}
	if incomplete != "" {
		t.Errorf("incomplete = %q, want empty", incomplete)
	}
	if len(metrics) != 1 || metrics[0].Int != 1 {
		t.Errorf("got %+v, want vet_findings = 1 — the real `go vet` output is not parsed as expected", metrics)
	}
}

func TestFirstRealLine_SkipsToolchainNotices(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"the T2102 message",
			"go: downloading github.com/antlr4-go/antlr/v4 v4.13.1\ncmd/promise/main.go:145:12: pattern all:resources/examples: no matching files found\n",
			"cmd/promise/main.go:145:12: pattern all:resources/examples: no matching files found",
		},
		{"several notices", "go: finding x\ngo: extracting y\nreal: boom\n", "real: boom"},
		{"nothing but notices", "go: downloading x v1.0.0\n", "go: downloading x v1.0.0"},
		{"no notice at all", "go: cannot find main module\nsecond line\n", "go: cannot find main module"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstRealLine(c.in); got != c.want {
				t.Errorf("firstRealLine = %q, want %q", got, c.want)
			}
		})
	}
}

// A name this project does not measure is refused rather than answered with an
// empty envelope, which reads like a clean result.
func TestMeasureContractGate_RefusesAnUnknownName(t *testing.T) {
	env, err := MeasureContractGate(t.TempDir(), "no-such-gate")
	if err == nil {
		t.Fatalf("an unknown gate measured %+v", env)
	}
	if !strings.Contains(err.Error(), "no gate named") {
		t.Errorf("error %q does not say the name is unknown", err)
	}
	// No build for a gate this project does not have: the refusal is about the
	// name, and paying for a compile before giving it would be absurd.
	if gateBuild.runs != 0 {
		t.Errorf("an unknown gate built %d times, want 0", gateBuild.runs)
	}
}

// A leaf that cannot measure surfaces its reason through the envelope path —
// this is the route by which "builds: go build in …: go: downloading …" reached
// a person, and the only reason that message was useless was its content.
func TestMeasureContractGate_ALeafErrorCarriesItsDiagnostic(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	root := t.TempDir()
	writeGoModule(t, filepath.Join(root, "compiler"), "compiler",
		"package compiler\n\nimport \"embed\"\n\n//go:embed resources/*\nvar Res embed.FS\n")

	if env, err := MeasureContractGate(root, "checked:go"); err == nil {
		t.Fatalf("a package that cannot be loaded measured %+v", env.Metrics)
	} else if !strings.Contains(err.Error(), "no matching files found") {
		t.Errorf("error %q does not carry the real diagnostic", err)
	}
}

// A composition names the part that failed. Without it the reader gets one
// error for four gates and no way to tell which one to look at — the reporting
// half of T2102's "did not measure anything".
func TestMeasureContractGate_ACompositionNamesTheFailingPart(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	root := t.TempDir()
	writeGoModule(t, filepath.Join(root, "compiler"), "compiler",
		"package compiler\n\nimport \"embed\"\n\n//go:embed resources/*\nvar Res embed.FS\n")

	_, err := MeasureContractGate(root, "checked")
	if err == nil {
		t.Fatal("a composition whose part could not measure reported success")
	}
	if !strings.HasPrefix(err.Error(), "checked:go: ") {
		t.Errorf("error %q does not lead with the part that failed", err)
	}
}

// What a person actually sees when the build fails under `bin/run integration`:
// one failing number, and a reason per part that says which measurement was
// withheld. Every part but formatted:go reports nothing, and the whole is
// incomplete — so the judge cannot read it as a pass.
func TestIntegration_ABuildFailureIsReportedWhole(t *testing.T) {
	stubGateBuild(t, buildFails())

	env, err := MeasureContractGate(t.TempDir(), "integration")
	if err != nil {
		t.Fatalf("a failed build is a measurement, not a failure to measure: %v", err)
	}
	byName := map[string]int64{}
	for _, m := range env.Metrics {
		byName[m.Name] = m.Int
	}
	if byName["build_failures"] != 1 {
		t.Errorf("build_failures = %d, want 1", byName["build_failures"])
	}
	if _, reported := byName["unformatted_go_files"]; !reported {
		t.Error("formatted:go withheld its number; gofmt needs no build and still has an honest answer")
	}
	for _, withheld := range []string{"unformatted_promise_files", "vet_findings", "go_test_failures", "host_test_failures"} {
		if _, reported := byName[withheld]; reported {
			t.Errorf("%s was reported after a failed build; that number is about the previous build", withheld)
		}
	}
	for _, part := range []string{"formatted:promise", "builds", "checked:go", "tested:go", "tested:promise"} {
		if !strings.Contains(env.Incomplete, part+": ") {
			t.Errorf("incomplete does not name %s: %q", part, env.Incomplete)
		}
	}
}

// The message the item opened with: the reason printed was `go: downloading
// github.com/antlr4-go/antlr/v4`, a progress notice standing in front of the
// real error and sending the reader to the network.
func TestBuilds_ErrorSkipsTheDownloadNotice(t *testing.T) {
	stubGateBuild(t, &fakeBuild{})
	root, modules := goModulesRoot(t)
	fake := &fakeGoTest{
		stderr: map[string]string{
			modules[0]: "go: downloading github.com/antlr4-go/antlr/v4 v4.13.1\ncmd/promise/main.go:145:12: pattern all:resources/examples: no matching files found\n",
		},
		errs: map[string]error{modules[0]: errors.New("exit status 1")},
	}

	_, _, err := measureBuildsWith(root, fake.capture)
	if err == nil {
		t.Fatal("a go build that named no package was reported as zero unbuildable packages")
	}
	if !strings.Contains(err.Error(), "no matching files found") {
		t.Errorf("error %q does not carry the real diagnostic", err)
	}
	if strings.Contains(err.Error(), "go: downloading") {
		t.Errorf("error %q leads with a progress notice", err)
	}
}
