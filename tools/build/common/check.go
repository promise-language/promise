package common

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// This file holds the ONE implementation of what "checked" means in this
// project: which Go modules are swept, which packages within them, the analyzer
// invocation, and which diagnostics are this project's to act on. bin/check
// reports that answer and the checked:go gate counts it, from here — a tool and
// the gate that measures the same property are one implementation in two modes,
// and repair-versus-measure is the only permitted difference between them
// (docs/gate-system.md). Two spellings of "what we check" drift, and when they
// do the project has two contradictory answers to the same question and no way
// to tell which is right: that is how checked:go came to count 77 diagnostics in
// a tree this tool called clean (T2104).

// GoModules returns every Go module in this tree, in sweep order, together with
// a reason when one that IS present had to be left out.
//
// `./...` is module-scoped, so one invocation at the root would silently skip
// the others — honest numbers about part of the subject, which is an incomplete
// run that does not know it is incomplete. compiler/ and tools/build/ are
// always here; flows/ is fetched on demand and needs flow-sdk/ beside it to
// resolve, so present without it the module is left out and NAMED. An empty
// reason is a complete sweep, and a gate must be able to tell the two apart: a
// baseline never moves from an incomplete run.
func GoModules(root string) (dirs []string, incomplete string) {
	dirs = []string{filepath.Join(root, "compiler")}
	if tools := filepath.Join(root, "tools", "build"); Exists(filepath.Join(tools, "go.mod")) {
		dirs = append(dirs, tools)
	}
	if flows := filepath.Join(root, "flows"); Exists(filepath.Join(flows, "go.mod")) {
		if Exists(filepath.Join(root, "flow-sdk", "go.mod")) {
			dirs = append(dirs, flows)
		} else {
			incomplete = "flows/ was not measured — flow-sdk/ is not present beside it (run ./make to fetch)"
		}
	}
	return dirs, incomplete
}

// generatedGoPackage names this project's generated Go tree, relative to its
// module: compiler/internal/parser, which ANTLR writes from the grammar.
//
// It is the one home of that name. A finding there is not actionable by the
// author of a change — the fix is to change PromiseParser.g4 and regenerate, and
// ANTLR's output has 77 `unreachable code` sites by construction. Generated code
// is excluded from being CHECKED and from coverage; it is never excluded from
// being BUILT, where it has to compile like anything else.
const generatedGoPackage = "internal/parser"

// isGeneratedGoPackage reports whether an import path names that tree.
func isGeneratedGoPackage(importPath string) bool {
	return strings.HasSuffix(importPath, "/"+generatedGoPackage)
}

// isGeneratedGoFile reports whether a diagnostic's file lies in that tree.
//
// Both forms of the rule exist, and the FILE form is the one that holds. go vet
// reports diagnostics for the dependencies of the packages it is given, not only
// for those packages: with compiler/internal/parser left out of the package
// list, `bin/gate checked:go` still reported all 77 of its `unreachable code`
// findings, because every package in that list imports it. Whether they surface
// at all varies with the host's Go release and with build-cache state — the same
// tree measured 0, then 78, then 0 again here, and 77 on basex861. A metric that
// moves with that describes the host rather than the tree (T2104), so the
// exclusion is applied where it does not move: to the diagnostics.
func isGeneratedGoFile(file string) bool {
	dir := path.Dir(filepath.ToSlash(file))
	return dir == generatedGoPackage || strings.HasSuffix(dir, "/"+generatedGoPackage)
}

// excludeGeneratedGoPackages drops generated packages from a `go list` listing,
// preserving order. A blank or empty listing yields none rather than a list with
// an empty entry in it.
func excludeGeneratedGoPackages(goList string) []string {
	var pkgs []string
	for _, pkg := range strings.Split(goList, "\n") {
		pkg = strings.TrimSpace(pkg)
		if pkg != "" && !isGeneratedGoPackage(pkg) {
			pkgs = append(pkgs, pkg)
		}
	}
	return pkgs
}

// GoCheckUnit is one module's check: the directory to run in, and the exact
// `go` argument list. Args never contains "./..." — naming the packages IS the
// selection, and a mode that re-spelled it would be checking something else.
type GoCheckUnit struct {
	Dir  string
	Args []string
}

// GoCheckUnits is the selection both modes run. Neither builds a list of its
// own.
func GoCheckUnits(root string) ([]GoCheckUnit, string, error) {
	dirs, incomplete := GoModules(root)
	var units []GoCheckUnit
	for _, dir := range dirs {
		pkgs, err := checkedGoPackages(dir)
		if err != nil {
			return nil, "", err
		}
		units = append(units, GoCheckUnit{Dir: dir, Args: append([]string{"vet"}, pkgs...)})
	}
	return units, incomplete, nil
}

// checkedGoPackages lists one module's packages, minus the generated ones.
//
// The listing is CAPTURED rather than streamed, so its diagnostic reaches the
// error rather than only the terminal: `go list` is where a package that cannot
// be loaded stops both modes, and "go list ./...: exit status 1" on its own
// names nothing to act on (T2102).
func checkedGoPackages(dir string) ([]string, error) {
	out, stderr, err := captureSplit(dir, "go", "list", "./...")
	if err != nil {
		return nil, fmt.Errorf("go list in %s: %w: %s", dir, err, firstRealLine(stderr))
	}
	pkgs := excludeGeneratedGoPackages(out)
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages found to check in %s", dir)
	}
	return pkgs, nil
}

// GoDiagnostic is one line of `go vet` output that names a position.
type GoDiagnostic struct {
	// Dir is the module the run that reported this was in. ParseGoDiagnostics
	// leaves it empty — go vet's output does not name it — and GoCheckFindings
	// fills it, because File alone does not locate the file: it is relative to
	// this directory.
	Dir string
	// File is the path as go printed it — relative to Dir.
	File string
	// Text is the whole line, which is what a reader needs to act on it.
	Text string
}

// ParseGoDiagnostics picks the diagnostics out of `go vet` output: the lines
// naming a file and a position, as distinct from the "# package" headers that
// group them.
func ParseGoDiagnostics(s string) []GoDiagnostic {
	var diags []GoDiagnostic
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// A type or load error arrives as "vet: file:line:col: message"; the
		// position is what makes it a diagnostic either way.
		text := line
		file := strings.TrimPrefix(line, "vet: ")
		// file:line:col: message — a diagnostic names a position.
		parts := strings.SplitN(file, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if _, err := strconv.Atoi(parts[1]); err != nil {
			continue
		}
		diags = append(diags, GoDiagnostic{File: parts[0], Text: text})
	}
	return diags
}

// GoCheckFindings runs the check over every Go module and returns the
// diagnostics this project's authors can act on. This is THE answer: bin/check
// prints it and fails on it, checked:go counts it, and neither does anything
// else to reach it.
//
// capture is the seam — captureSplit in both real modes, a stand-in in tests.
func GoCheckFindings(root string, capture captureFunc) (findings []GoDiagnostic, incomplete string, err error) {
	units, incomplete, err := GoCheckUnits(root)
	if err != nil {
		return nil, "", err
	}
	for _, u := range units {
		_, stderr, runErr := capture(u.Dir, "go", u.Args...)
		diags := ParseGoDiagnostics(stderr)
		if runErr != nil && len(diags) == 0 {
			// It failed and named no position: the failure is about the
			// toolchain or the module, not about code in this tree.
			return nil, "", fmt.Errorf("go vet in %s: %w: %s", u.Dir, runErr, firstLine(stderr))
		}
		for _, d := range diags {
			if !isGeneratedGoFile(d.File) {
				d.Dir = u.Dir
				findings = append(findings, d)
			}
		}
	}
	return findings, incomplete, nil
}

// RunCheck is bin/check, and bin/verify's check step.
//
// This is the REPAIR mode of the pair whose measure mode is the checked:go
// gate. `go vet` has no general -fix, so there is nothing to repair today and
// the repair mode degenerates to the check-only run: it reports the findings and
// exits non-zero. What it must not become is a second, subtly different
// measurement — hence the shared GoCheckFindings above, whose count IS the
// gate's vet_findings.
func RunCheck(root string) error {
	findings, incomplete, err := GoCheckFindings(root, captureSplit)
	if err != nil {
		return err
	}
	if incomplete != "" {
		fmt.Fprintf(os.Stderr, "warning: %s\n", incomplete)
	}
	if n := len(findings); n > 0 {
		fmt.Fprint(os.Stderr, renderCheckFindings(root, findings))
		return fmt.Errorf("%d go vet %s", n, pluralDiagnostics(n))
	}
	return nil
}

// renderCheckFindings groups findings under the module they were found in, the
// way go vet groups its own under the package that produced them.
//
// The header is not decoration: go vet prints each path relative to the module
// it ran in, so `internal/sema/check.go` is not openable from the repo root and
// names no module of the several swept. A name a tool prints has to be one the
// reader can act on.
func renderCheckFindings(root string, findings []GoDiagnostic) string {
	var b strings.Builder
	lastDir := ""
	for _, f := range findings {
		if f.Dir != lastDir {
			where := f.Dir
			if rel, err := filepath.Rel(root, f.Dir); err == nil {
				where = filepath.ToSlash(rel)
			}
			fmt.Fprintf(&b, "# %s\n", where)
			lastDir = f.Dir
		}
		fmt.Fprintf(&b, "%s\n", f.Text)
	}
	return b.String()
}

func pluralDiagnostics(n int) string {
	if n == 1 {
		return "diagnostic"
	}
	return "diagnostics"
}
