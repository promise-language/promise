package common

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// flowsRoot is a temp tree with a flows/ module in it, and flow-sdk/ beside it
// only when asked for.
func flowsRoot(t *testing.T, withSDK bool) string {
	t.Helper()
	root := t.TempDir()
	mods := []string{"flows"}
	if withSDK {
		mods = append(mods, "flow-sdk")
	}
	for _, name := range mods {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		mod := "module example.com/" + name + "\n\ngo 1.21\n"
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// No flows/ at all (main without the flows branch) is a COMPLETE sweep: there is
// nothing absent to report, and a reason that could never be discharged would
// freeze every baseline the gates using this list carry.
func TestGoModules_NoFlowsModuleIsComplete(t *testing.T) {
	dirs, incomplete := GoModules(t.TempDir())
	if incomplete != "" {
		t.Errorf("incomplete = %q, want empty when flows/ is simply absent", incomplete)
	}
	for _, dir := range dirs {
		if strings.HasSuffix(dir, "flows") {
			t.Errorf("flows/ in %v with no flows/go.mod", dirs)
		}
	}
}

// flows/ present WITHOUT flow-sdk/ cannot be resolved, so it is left out — and
// NAMED. The tool prints that as a warning and a gate reports it as
// incomplete_reason; silently measuring less is the failure this distinction
// exists to prevent.
func TestGoModules_FlowsWithoutSDKIsNamed(t *testing.T) {
	root := flowsRoot(t, false)
	dirs, incomplete := GoModules(root)
	if incomplete == "" {
		t.Error("incomplete is empty, want a reason naming flows/")
	}
	if !strings.Contains(incomplete, "flows/") {
		t.Errorf("incomplete = %q, want it to name flows/", incomplete)
	}
	if slices.Contains(dirs, filepath.Join(root, "flows")) {
		t.Errorf("flows/ swept without flow-sdk/: %v", dirs)
	}
}

// With flow-sdk/ beside it, flows/ is a module like any other and the sweep is
// complete again.
func TestGoModules_FlowsWithSDKIsSwept(t *testing.T) {
	root := flowsRoot(t, true)
	dirs, incomplete := GoModules(root)
	if incomplete != "" {
		t.Errorf("incomplete = %q, want empty when flow-sdk/ is present", incomplete)
	}
	if !slices.Contains(dirs, filepath.Join(root, "flows")) {
		t.Errorf("flows/ missing from %v", dirs)
	}
}

// The generated parser is machine output: regenerated from the grammar rather
// than edited, so a diagnostic in it is not the change author's to act on.
// Excluded from checking and from coverage, from this one function.
func TestExcludeGeneratedGoPackages_DropsTheGeneratedParser(t *testing.T) {
	const goList = `github.com/promise-language/promise/compiler/cmd/promise
github.com/promise-language/promise/compiler/internal/codegen
github.com/promise-language/promise/compiler/internal/codegen/tests/drop1
github.com/promise-language/promise/compiler/internal/parser
github.com/promise-language/promise/compiler/internal/sema
`
	want := []string{
		"github.com/promise-language/promise/compiler/cmd/promise",
		"github.com/promise-language/promise/compiler/internal/codegen",
		"github.com/promise-language/promise/compiler/internal/codegen/tests/drop1",
		"github.com/promise-language/promise/compiler/internal/sema",
	}
	if got := excludeGeneratedGoPackages(goList); !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A blank or empty listing must not silently produce a list with an empty entry
// in it (which `go vet` would read as a package named "", and `go test` as an
// instruction to instrument nothing).
func TestExcludeGeneratedGoPackages_Empty(t *testing.T) {
	if got := excludeGeneratedGoPackages("\n  \n"); len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}

// go vet prints paths relative to the module directory, so the file form of the
// rule matches on the directory. The package form matches an import path, which
// is absolute — spelling one rule for both is what keeps them from disagreeing.
func TestIsGeneratedGoFile(t *testing.T) {
	for _, tc := range []struct {
		file string
		want bool
	}{
		{"internal/parser/promise_parser.go", true},
		{"compiler/internal/parser/promise_lexer.go", true},
		{filepath.FromSlash("internal/parser/promise_parser.go"), true},
		{"internal/sema/check.go", false},
		{"internal/parsers/thing.go", false},
		{"internal/parser.go", false},
		{"cmd/promise/main.go", false},
	} {
		if got := isGeneratedGoFile(tc.file); got != tc.want {
			t.Errorf("isGeneratedGoFile(%q) = %v, want %v", tc.file, got, tc.want)
		}
	}
}

// go vet output is diagnostics interleaved with the "# package" headers that
// group them, and a load or type error arrives with a "vet: " prefix. All three
// shapes have to be told apart, because the headers are not findings and the
// prefixed one is.
func TestParseGoDiagnostics(t *testing.T) {
	const out = `# github.com/promise-language/promise/compiler/internal/sema
internal/sema/check.go:12:2: unreachable code
internal/parser/promise_parser.go:1175:2: unreachable code

vet: internal/sema/decl.go:8:1: missing return
not a diagnostic at all
`
	diags := ParseGoDiagnostics(out)
	want := []GoDiagnostic{
		{File: "internal/sema/check.go", Text: "internal/sema/check.go:12:2: unreachable code"},
		{File: "internal/parser/promise_parser.go", Text: "internal/parser/promise_parser.go:1175:2: unreachable code"},
		{File: "internal/sema/decl.go", Text: "vet: internal/sema/decl.go:8:1: missing return"},
	}
	if !slices.Equal(diags, want) {
		t.Errorf("got %+v,\nwant %+v", diags, want)
	}
}

// go vet prints each path relative to the module it ran in, so a bare list of
// findings from a multi-module sweep names files that resolve from nowhere. The
// header says which module, and a second module's findings get their own.
func TestRenderCheckFindings_NamesTheModuleEachPathIsRelativeTo(t *testing.T) {
	root := filepath.FromSlash("/repo")
	findings := []GoDiagnostic{
		{Dir: filepath.Join(root, "compiler"), File: "internal/sema/check.go", Text: "internal/sema/check.go:12:2: unreachable code"},
		{Dir: filepath.Join(root, "compiler"), File: "internal/sema/decl.go", Text: "internal/sema/decl.go:3:1: unreachable code"},
		{Dir: filepath.Join(root, "tools", "build"), File: "common/x.go", Text: "common/x.go:9:2: unreachable code"},
	}
	want := "# compiler\n" +
		"internal/sema/check.go:12:2: unreachable code\n" +
		"internal/sema/decl.go:3:1: unreachable code\n" +
		"# tools/build\n" +
		"common/x.go:9:2: unreachable code\n"
	if got := renderCheckFindings(root, findings); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// checkModuleRoot writes a temp tree with a compiler/ module in it: a generated
// internal/parser package, and an internal/sema package that imports it. Each
// package carries an unreachable-code site only when asked for, so a test can
// place the finding on either side of the exclusion.
//
// The real `go vet` is what runs over this. A stand-in could not prove the thing
// under test: that go vet reports findings in DEPENDENCIES, which is why naming
// packages is not enough to exclude one.
func checkModuleRoot(t *testing.T, parserDead, semaDead bool) string {
	t.Helper()
	root := t.TempDir()
	dead := func(yes bool) string {
		if !yes {
			return ""
		}
		return "\nfunc Dead() {\n\treturn\n\tprintln(\"dead\")\n}\n"
	}
	files := map[string]string{
		"go.mod": "module example.com/compiler\n\ngo 1.25.6\n",
		"internal/parser/parser.go": "// Code generated from Probe.g4 by ANTLR. DO NOT EDIT.\n\n" +
			"package parser\n\nfunc Parse() string { return \"ok\" }\n" + dead(parserDead),
		"internal/sema/sema.go": "package sema\n\nimport \"example.com/compiler/internal/parser\"\n\n" +
			"func Check() string { return parser.Parse() }\n" + dead(semaDead),
	}
	for rel, body := range files {
		full := filepath.Join(root, "compiler", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// vetSaying stands in for the `go vet` child, answering every module with the
// same canned stderr and the non-zero exit go vet gives when it finds something.
// Staging the leak with a real go vet is not possible to do reliably — whether
// it surfaces depends on build-cache state — so the output it produces when it
// does is what this hands to the code under test.
func vetSaying(stderr string) captureFunc {
	return func(dir, name string, args ...string) (string, string, error) {
		return "", stderr, errSaidSomething
	}
}

var errSaidSomething = errors.New("exit status 1")

// The bug: a diagnostic in the generated parser must not reach the answer, and
// it arrives in a run whose package list does not contain the parser at all —
// go vet reports findings in dependencies, and every package in that list
// imports the parser. Leaving the package out of the command line does not leave
// its findings out of the count (T2104); dropping them by file does.
func TestGoCheckFindings_DropsDiagnosticsInTheGeneratedParser(t *testing.T) {
	root, err := RootForTests()
	if err != nil {
		t.Skip("not inside the promise repo:", err)
	}
	const vetOutput = `# github.com/promise-language/promise/compiler/internal/sema
internal/parser/promise_parser.go:1175:2: unreachable code
internal/parser/promise_lexer.go:42:2: unreachable code
internal/sema/check.go:12:2: unreachable code
`
	findings, _, err := GoCheckFindings(root, vetSaying(vetOutput))
	if err != nil {
		t.Fatalf("findings are a measurement, not a failure to measure: %v", err)
	}
	// One per module swept: the authored one survives, both generated ones do not.
	modules, _ := GoModules(root)
	if len(findings) != len(modules) {
		t.Fatalf("got %d findings %+v, want the %d authored ones", len(findings), findings, len(modules))
	}
	for _, f := range findings {
		if isGeneratedGoFile(f.File) {
			t.Errorf("generated-file finding reached the answer: %+v", f)
		}
		if !strings.Contains(f.Text, "unreachable code") {
			t.Errorf("finding text %q does not carry what go vet said", f.Text)
		}
	}
}

// go vet exits non-zero when it finds something, and a tree with findings in it
// is a SUCCESSFUL measurement of a bad tree. Reporting that as an error would
// make the gate unable to answer in exactly the case it exists for.
func TestGoCheckFindings_FindingsAreNotAFailureToMeasure(t *testing.T) {
	root, err := RootForTests()
	if err != nil {
		t.Skip("not inside the promise repo:", err)
	}
	findings, _, err := GoCheckFindings(root, vetSaying("internal/sema/check.go:12:2: unreachable code\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) == 0 {
		t.Error("the finding go vet reported did not survive")
	}
}

// A run that failed and named no position did not happen — a load error, a
// broken module, a missing toolchain. That is not "zero findings", and the
// reader needs to know which module it was.
func TestGoCheckFindings_ARunThatNamedNoPositionIsAnError(t *testing.T) {
	root, err := RootForTests()
	if err != nil {
		t.Skip("not inside the promise repo:", err)
	}
	_, _, err = GoCheckFindings(root, vetSaying("go: cannot find main module\nsecond line\n"))
	if err == nil {
		t.Fatal("a run that never happened was reported as zero findings")
	}
	if !strings.Contains(err.Error(), "go: cannot find main module") {
		t.Errorf("error %q carries no detail", err)
	}
	if strings.Contains(err.Error(), "second line") {
		t.Errorf("error %q quotes more than the first line", err)
	}
}

// The other half: the exclusion must not be "drop everything". An authored
// package's finding is exactly what this is for, and it survives with its file
// and its text intact so a reader can act on it.
func TestGoCheckFindings_KeepsFindingsInAuthoredPackages(t *testing.T) {
	root := checkModuleRoot(t, true, true)
	findings, _, err := GoCheckFindings(root, captureSplit)
	if err != nil {
		t.Fatalf("GoCheckFindings: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("got %d findings %+v, want exactly the one in internal/sema", len(findings), findings)
	}
	if !strings.Contains(findings[0].File, filepath.FromSlash("internal/sema")) {
		t.Errorf("finding names %q, want the authored package", findings[0].File)
	}
	if !strings.Contains(findings[0].Text, "unreachable code") {
		t.Errorf("finding text %q does not carry what go vet said", findings[0].Text)
	}
}

// A tree with nothing wrong in it is not "one finding I could not parse": the
// fixture with no dead code at all must come back empty.
func TestGoCheckFindings_CleanTreeIsEmpty(t *testing.T) {
	root := checkModuleRoot(t, false, false)
	findings, _, err := GoCheckFindings(root, captureSplit)
	if err != nil {
		t.Fatalf("GoCheckFindings: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("got %+v, want none", findings)
	}
}

// The selection is the point: a unit that carried "./..." would be checking
// whatever the toolchain lists, exclusion and all. Asserted against the real
// repository, where the generated parser actually exists.
func TestGoCheckUnits_ExcludesTheGeneratedParser(t *testing.T) {
	root, err := RootForTests()
	if err != nil {
		t.Skip("not inside the promise repo:", err)
	}
	units, _, err := GoCheckUnits(root)
	if err != nil {
		t.Fatalf("GoCheckUnits: %v", err)
	}
	if len(units) == 0 {
		t.Fatal("no units, so every pin here would be vacuous")
	}

	sawSema := false
	for _, u := range units {
		if len(u.Args) == 0 || u.Args[0] != "vet" {
			t.Fatalf("%s: argv %v, want it to start with vet", u.Dir, u.Args)
		}
		for _, arg := range u.Args[1:] {
			if arg == "./..." {
				t.Errorf("%s: argv contains ./..., which re-spells the selection: %v", u.Dir, u.Args)
			}
			if isGeneratedGoPackage(arg) {
				t.Errorf("%s: generated package %q is being checked", u.Dir, arg)
			}
			if strings.HasSuffix(arg, "/compiler/internal/sema") {
				sawSema = true
			}
		}
	}
	if !sawSema {
		t.Error("compiler/internal/sema is not in the selection, so the exclusion above proves nothing")
	}
}

// RunCheck against the real repository, for the one thing every fixture leaves
// unproven: that the units it builds are an argv `go vet` actually accepts, and
// that this repository is checked.
func TestRunCheck_RealRepo(t *testing.T) {
	root, err := RootForTests()
	if err != nil {
		t.Skip("not inside the promise repo:", err)
	}
	if err := RunCheck(root); err != nil {
		t.Fatalf("RunCheck: %v", err)
	}
}

// --- the tool mode's own behaviour ---

// bin/check's failure path: the findings reach the reader, grouped under the
// module whose paths they are relative to, and the error names how many there
// were. Green is not the only thing a check tool has to get right — what it says
// when it is red is the whole reason to run it.
func TestRunCheck_ReportsFindingsAndFails(t *testing.T) {
	root := checkModuleRoot(t, false, true)

	var err error
	stderr := captureStderr(t, func() { err = RunCheck(root) })

	if err == nil {
		t.Fatal("RunCheck returned nil for a tree with a finding in it")
	}
	if !strings.Contains(err.Error(), "1 go vet diagnostic") {
		t.Errorf("error %q does not say how many findings there were", err)
	}
	if strings.Contains(err.Error(), "diagnostics") {
		t.Errorf("error %q pluralized a single finding", err)
	}
	if !strings.Contains(stderr, "# compiler") {
		t.Errorf("stderr %q names no module, so its paths resolve from nowhere", stderr)
	}
	// Normalize the haystack, not the needle: go vet prints paths with the
	// platform's separator, so on Windows this is internal\sema and the
	// forward-slash needle could never match. What the assertion is about is
	// that the finding names the file, not which slash spells it (T2094).
	if !strings.Contains(filepath.ToSlash(stderr), "internal/sema") {
		t.Errorf("stderr %q does not name the file", stderr)
	}
	if !strings.Contains(stderr, "unreachable code") {
		t.Errorf("stderr %q does not carry what go vet said", stderr)
	}
}

// A clean tree prints nothing and returns nil. A check tool that chattered on
// success would train its reader to stop looking at its output.
func TestRunCheck_CleanTreeIsSilentAndGreen(t *testing.T) {
	root := checkModuleRoot(t, true, false) // findings only in the generated parser

	var err error
	stderr := captureStderr(t, func() { err = RunCheck(root) })

	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}
	if stderr != "" {
		t.Errorf("RunCheck wrote %q on a clean tree", stderr)
	}
}

// Plural agreement, through the tool rather than as a unit: two findings, two
// modules, one message. pluralDiagnostics exists only to be read by a person, so
// the assertion is on what they read.
func TestRunCheck_PluralizesSeveralFindings(t *testing.T) {
	root := checkModuleRoot(t, false, true)
	// A second module in the same tree, with its own finding.
	tools := filepath.Join(root, "tools", "build")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tools, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/tools\n\ngo 1.25.6\n")
	write("x.go", "package tools\n\nfunc Dead() {\n\treturn\n\tprintln(\"dead\")\n}\n")

	var err error
	stderr := captureStderr(t, func() { err = RunCheck(root) })

	if err == nil {
		t.Fatal("RunCheck returned nil for a tree with findings in two modules")
	}
	if !strings.Contains(err.Error(), "2 go vet diagnostics") {
		t.Errorf("error %q, want it to count 2 and pluralize", err)
	}
	for _, want := range []string{"# compiler", "# tools/build"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q is missing the %q header", stderr, want)
		}
	}
}

// flows/ present without flow-sdk/ is a module that could not be measured, and
// the tool says so rather than quietly checking less than it claims to.
func TestRunCheck_WarnsWhenAModuleCouldNotBeMeasured(t *testing.T) {
	root := checkModuleRoot(t, false, false)
	flows := filepath.Join(root, "flows")
	if err := os.MkdirAll(flows, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flows, "go.mod"), []byte("module example.com/flows\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var err error
	stderr := captureStderr(t, func() { err = RunCheck(root) })

	if err != nil {
		t.Fatalf("an unmeasurable module is not a failed check: %v", err)
	}
	if !strings.Contains(stderr, "flows/") {
		t.Errorf("stderr %q does not name the module that was left out", stderr)
	}
}

// --- failures to measure at all ---

// A directory that is not a Go module cannot be listed, and that is not "no
// packages to check": the error names the directory, so the reader is sent to
// the tree rather than to this code.
func TestCheckedGoPackages_ARootThatIsNotAModuleIsAnError(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "compiler"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := GoCheckUnits(root)
	if err == nil {
		t.Fatal("a directory with no go.mod was listed without error")
	}
	if !strings.Contains(err.Error(), "go list in") {
		t.Errorf("error %q does not say what failed", err)
	}
	// And the failure propagates rather than being measured as zero findings.
	if _, _, err := GoCheckFindings(root, vetSaying("")); err == nil {
		t.Error("GoCheckFindings reported a tree it could not list as clean")
	}
	if err := RunCheck(root); err == nil {
		t.Error("RunCheck reported a tree it could not list as clean")
	}
}

// A module whose every package is generated leaves nothing to check. Returning
// an empty argv instead would run `go vet` with no packages, which vets the
// current directory — a different subject, silently.
func TestCheckedGoPackages_AModuleOfOnlyGeneratedCodeIsAnError(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "compiler", "internal", "parser")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "compiler", "go.mod"), []byte("module example.com/compiler\n\ngo 1.25.6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "parser.go"), []byte("package parser\n\nfunc Parse() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := GoCheckUnits(root)
	if err == nil {
		t.Fatal("a module with nothing but generated code produced a unit")
	}
	if !strings.Contains(err.Error(), "no packages found to check") {
		t.Errorf("error %q does not say what was wrong", err)
	}
}

// A line with three colon-separated fields whose middle one is not a number is
// not a position — package paths and prose reach stderr too, and counting one
// as a finding would put a number in the gate that nothing can be done about.
func TestParseGoDiagnostics_IgnoresLinesThatOnlyLookLikePositions(t *testing.T) {
	const out = `example.com/a:b:c is not a position
go: downloading example.com/x v1.2.3
internal/sema/check.go:12:2: unreachable code
`
	diags := ParseGoDiagnostics(out)
	if len(diags) != 1 {
		t.Fatalf("got %d diagnostics %+v, want only the real one", len(diags), diags)
	}
	if diags[0].File != "internal/sema/check.go" {
		t.Errorf("File = %q", diags[0].File)
	}
}
