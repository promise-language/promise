package main

// check.go implements `promise check` — semantic and ownership analysis with no
// codegen, over a single file, a module/project directory, or a tree of both.
//
// The UNIT, not the file, is what can be analysed. A directory with a
// promise.toml is one unit — all of its .pr files, tests included — and a .pr
// file outside any project is another. A file belonging to a multi-file module
// was never checkable on its own (modules/std/vector.pr does not see _FnIter in
// iter.pr), so a per-file count measured how the files happen to be arranged
// rather than whether the code is sound: 50 of this repo's 942 files, none of
// them a defect (T2085).
//
// A single unit is resolved by resolveTarget, the policy build/run/emit-ir
// already share (T1603), so `check` can never disagree with `build` about what
// a target names. A sweep runs one child process per unit: the frontend leaves
// through os.Exit on a fatal diagnostic, so one process cannot check many units
// and survive the first failure — the same reason the multi-file test runner
// spawns children.

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/promise-language/promise/compiler/internal/module"
)

// checkUnitRun is the unit this process is analysing, and the diagnostics it has
// printed so far. printFileErrors counts into it; exitFrontend flushes it.
//
// The counters live here rather than being re-derived from the printed text
// because the process that did the analysis is the one that knows: a parent
// scraping stderr would be a second, subtly different answer to the same
// question. Nil outside `promise check`, where nothing counts and exitFrontend
// is exactly os.Exit.
type checkUnitRun struct {
	unit     string
	errors   int
	warnings int
	reported bool
}

// activeCheck is the in-flight unit, or nil.
var activeCheck *checkUnitRun

// countCheckDiagnostic records one diagnostic the frontend FOUND. A no-op
// outside `promise check`, so the frontend's printer carries no branch of its
// own.
//
// Found, not printed: the parser's listener stops printing after 13 syntax
// errors and keeps counting, so a unit can report more than it showed. The
// count is the honest answer to "how much is wrong here", and truncating the
// display is a separate concern.
func countCheckDiagnostic(warning bool) {
	if activeCheck == nil {
		return
	}
	if warning {
		activeCheck.warnings++
		return
	}
	activeCheck.errors++
}

// countCheckDiagnostics records n error diagnostics at once — the parser's
// listener prints as it goes and reports only a total.
func countCheckDiagnostics(n int) {
	for i := 0; i < n; i++ {
		countCheckDiagnostic(false)
	}
}

// reportFailure prints the unit's result line for a run that gave up. An error
// the frontend reported without a position (a module that would not load, a
// project with no sources) still failed the unit, so a zero count reports one.
//
// Written with fmt.Printf and deliberately NOT through the progress renderer:
// the parent of a sweep parses this line out of the child's stdout, and a
// progress mode that dropped or rewrote it would leave the parent with a unit
// that reported nothing. Same for reportSuccess.
func (r *checkUnitRun) reportFailure() {
	if r.reported {
		return
	}
	r.reported = true
	if r.errors == 0 {
		r.errors = 1
	}
	fmt.Printf("FAIL %s (%s)\n", r.unit, describeDiagnostics(r.errors, r.warnings))
}

// reportSuccess prints the result line for a unit that analysed to completion.
// Warnings are not failures — the compiler builds through them — so they get
// their own outcome rather than being folded into either of the other two.
func (r *checkUnitRun) reportSuccess() {
	r.reported = true
	if r.warnings > 0 {
		fmt.Printf("warn %s (%s)\n", r.unit, describeDiagnostics(0, r.warnings))
		return
	}
	fmt.Printf("ok %s\n", r.unit)
}

// describeDiagnostics renders the count clause of a result line: "18 errors",
// "1 warning", "1 error, 2 warnings".
func describeDiagnostics(errors, warnings int) string {
	var parts []string
	if errors > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", errors, plural(errors, "error")))
	}
	if warnings > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", warnings, plural(warnings, "warning")))
	}
	if len(parts) == 0 {
		return "0 errors"
	}
	return strings.Join(parts, ", ")
}

// plural appends an "s" to word unless n is 1.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// exitFrontend is the frontend's door out.
//
// os.Exit skips defers, so a unit that fails would otherwise end without saying
// which unit failed or how badly — and the parent of a sweep reads exactly that
// line. Every fatal frontend exit goes through here; outside `promise check`
// this is os.Exit and nothing else.
func exitFrontend(code int) {
	if activeCheck != nil {
		activeCheck.reportFailure()
	}
	os.Exit(code)
}

// printCheckUsage renders `promise help check`.
func printCheckUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: promise check [-target triple] [-parallel N] <file.pr | dir | dir/...> ...")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Analyse Promise code — parse, semantic analysis and ownership — without")
	fmt.Fprintln(w, "generating code or linking.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Targets are resolved the way promise build resolves them:")
	fmt.Fprintln(w, "  promise check main.pr              a file that belongs to no project")
	fmt.Fprintln(w, "  promise check modules/std          a module or project, checked as one unit")
	fmt.Fprintln(w, "  promise check tests/...            every unit under a tree, recursively")
	fmt.Fprintln(w, "  promise check modules/... tests/...  several targets at once")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "A directory with a promise.toml is one unit — all of its .pr files, tests")
	fmt.Fprintln(w, "included — so a file of a multi-file module is checked with its module, never")
	fmt.Fprintln(w, "on its own. Warnings do not fail the run; errors exit non-zero.")
}

// runCheck implements `promise check`.
func runCheck(args []string) {
	targetTriple := ""
	parallelArg := ""
	res, err := parseCLIArgs("check", args, flagSpec{
		value: map[string]*string{"target": &targetTriple, "parallel": &parallelArg},
		flag:  map[string]*bool{"time-phases": &timePhases},
	}, false, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(res.positionals) == 0 {
		fmt.Fprintln(os.Stderr, "usage: promise check [-target triple] [-parallel N] <file.pr | dir | dir/...> ...")
		os.Exit(1)
	}

	parallel := runtime.NumCPU()
	if parallelArg != "" {
		n, convErr := strconv.Atoi(parallelArg)
		if convErr != nil || n < 1 {
			fmt.Fprintln(os.Stderr, "error: -parallel requires a positive integer")
			os.Exit(1)
		}
		parallel = n
	}
	checkTargetFlag(targetTriple)

	units, err := expandCheckTargets(res.positionals)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(units) == 0 {
		fmt.Fprintln(os.Stderr, "error: no .pr files found in the named targets")
		os.Exit(1)
	}

	// One unit is checked in this process: it is the whole answer, and spawning
	// a child to learn it would double the work for the command's commonest
	// form. It is also what every child of a sweep runs.
	if len(units) == 1 {
		checkOneUnit(units[0], targetTriple)
		return
	}
	runCheckSweep(units, targetTriple, parallel)
}

// checkOneUnit analyses one unit in this process and prints its result line.
// A fatal diagnostic leaves through exitFrontend, which prints the FAIL line on
// the way out, so both outcomes report themselves the same way.
func checkOneUnit(unit, targetTriple string) {
	activeCheck = &checkUnitRun{unit: unit}
	cfg, files, resolvedFile, err := resolveTarget(unit, "check", true)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		exitFrontend(1)
	}
	if cfg != nil {
		compileProjectFrontend(cfg.Dir, files, targetTriple)
	} else {
		compileFrontendForTarget(resolvedFile, targetTriple)
	}
	activeCheck.reportSuccess()
}

// expandCheckTargets turns the command's arguments into the units to check.
//
// A `.pr` file is named directly, and resolveTarget decides whether it stands
// alone; a directory is walked, and every file found is attributed to its
// nearest enclosing project — so a module is checked once, as a module, and a
// file that belongs to one is never checked apart from it. A project directory
// holding no .pr files contributes no unit: there is nothing in it to check.
func expandCheckTargets(args []string) ([]string, error) {
	seen := map[string]bool{}
	var units []string
	add := func(unit string) {
		if seen[unit] {
			return
		}
		seen[unit] = true
		units = append(units, unit)
	}

	for _, arg := range args {
		target := arg
		recursive := false
		if strings.HasSuffix(target, "/...") || target == "..." {
			recursive = true
			if target == "..." {
				target = "."
			} else {
				target = strings.TrimSuffix(target, "/...")
			}
		}

		info, err := os.Stat(target)
		if err != nil {
			// A name that does not exist is handed on as a unit so the frontend
			// reports file-not-found against the name the user typed, the way
			// resolveTarget does for build (T0927).
			add(target)
			continue
		}
		if !info.IsDir() {
			add(target)
			continue
		}
		found, err := checkUnitsInDir(target, recursive)
		if err != nil {
			return nil, err
		}
		for _, u := range found {
			add(u)
		}
	}

	sort.Strings(units)
	return units, nil
}

// checkUnitsInDir collects the units under dir: each .pr file becomes either its
// own unit or the project that owns it.
func checkUnitsInDir(dir string, recursive bool) ([]string, error) {
	projectOf := map[string]string{}
	unitFor := func(file string) string {
		parent := filepath.Dir(file)
		if proj, ok := projectOf[parent]; ok {
			if proj == "" {
				return file
			}
			return proj
		}
		proj := findEnclosingProjectDir(file)
		if proj != "" {
			// Displayed the way the user named things: an absolute path from
			// findEnclosingProjectDir would make every unit in the summary
			// unreadably long.
			if rel, err := filepath.Rel(mustAbs(dir), proj); err == nil {
				proj = filepath.Join(dir, rel)
			}
		}
		projectOf[parent] = proj
		if proj == "" {
			return file
		}
		return proj
	}

	var units []string
	if !recursive {
		// A project named directly is that one unit, whether or not any of its
		// sources sit at the top level — discoverProject decides what belongs to
		// it, and reports the project empty the way build does.
		if _, err := os.Stat(filepath.Join(dir, "promise.toml")); err == nil {
			if _, _, err := discoverProject(dir, true); err != nil {
				return nil, err
			}
			return []string{dir}, nil
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("error: %v", err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".pr") {
				continue
			}
			units = append(units, unitFor(filepath.Join(dir, e.Name())))
		}
		return units, nil
	}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Hidden directories hold caches and VCS metadata, never this
			// project's sources — the same exclusion bin/format's walk applies.
			if name := d.Name(); strings.HasPrefix(name, ".") && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".pr") {
			units = append(units, unitFor(path))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("error: %v", err)
	}
	return units, nil
}

// mustAbs resolves path, falling back to the path itself when the working
// directory is gone.
//
// Not absPath (test_json.go), which returns a FORWARD-SLASH path for JSON
// identity: filepath.Rel below compares against a native path, and on Windows
// the two separators would never match.
func mustAbs(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// checkDiagCountRe pulls the two numbers out of a result line's count clause.
// The counts come from the child because the child is what counted them; the
// parent never re-derives an outcome from printed diagnostics.
var checkDiagCountRe = regexp.MustCompile(`(\d+) (errors?|warnings?)`)

// checkResult is one unit's outcome as the parent of a sweep sees it.
type checkResult struct {
	unit     string
	outcome  string // "ok", "warn", "FAIL"
	errors   int
	warnings int
	stderr   string
	done     chan struct{}
}

// runCheckSweep checks many units, one child process each, and prints one
// summary. Clean units go through progress.Pass, so a piped run carries exactly
// the failures and the summary and a terminal shows one line rewritten in place
// (T1888); the summary is byte-identical in every mode.
func runCheckSweep(units []string, targetTriple string, parallel int) {
	unlock := module.LockBuildDirShared()
	defer unlock()
	prepareEmbeddedModulesForChildren()

	progress = newRenderer(resolveProgressMode(""), stdoutWriter{}, stderrWriter{})

	selfExe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot determine executable path: %v\n", err)
		os.Exit(1)
	}

	start := time.Now()
	results := make([]checkResult, len(units))
	for i, u := range units {
		results[i].unit = u
		results[i].done = make(chan struct{})
	}
	sem := make(chan struct{}, parallel)

	for i := range units {
		go func(idx int) {
			sem <- struct{}{}
			defer func() { <-sem }()

			r := &results[idx]
			args := []string{"check"}
			if targetTriple != "" {
				args = append(args, "-target", targetTriple)
			}
			args = append(args, r.unit)
			cmd := exec.Command(selfExe, args...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			runErr := cmd.Run()
			r.stderr = strings.TrimRight(stderr.String(), "\n")
			parseCheckResult(r, stdout.String(), runErr != nil)
			close(r.done)
		}(i)
	}

	checked, warned, failed := 0, 0, 0
	totalErrors, totalWarnings := 0, 0
	var failures []string

	for i := range results {
		<-results[i].done
		r := &results[i]
		totalErrors += r.errors
		totalWarnings += r.warnings
		if r.outcome == "ok" {
			checked++
			progress.Pass("ok %s\n", r.unit)
			continue
		}
		// Both remaining outcomes are rendered the same way — the unit, its
		// counts, then the diagnostics behind them — so the two cannot come to
		// describe the same numbers differently.
		detail := fmt.Sprintf("%s (%s)", r.unit, describeDiagnostics(r.errors, r.warnings))
		progress.Printf("%s %s\n", r.outcome, detail)
		if r.stderr != "" {
			progress.Println(r.stderr)
		}
		if r.outcome == "warn" {
			warned++
			continue
		}
		// FAIL, and anything this parent does not recognize: an outcome it
		// cannot name is not one it may count as a pass.
		failed++
		failures = append(failures, detail)
	}

	// TWIN: tools/build/common/checkpromise.go's checkSummaryRe parses this exact
	// line — it is what the checked:promise gate measures. compiler/ and
	// tools/build are separate Go modules with no dependency edge between them,
	// so nothing but this note binds the two; change one, change the other.
	// TestCheckSweepSummary pins the shape on this side.
	progress.Printf("\n%d checked, %d warned, %d failed (%d units, %d %s, %d %s, %.3fs)\n",
		checked, warned, failed, len(results),
		totalErrors, plural(totalErrors, "error"),
		totalWarnings, plural(totalWarnings, "warning"),
		time.Since(start).Seconds())
	if len(failures) > 0 {
		progress.Println("FAILED:")
		for _, f := range failures {
			progress.Printf("  %s\n", f)
		}
		os.Exit(1)
	}
}

// parseCheckResult reads a child's result line into r. A child that died without
// printing one (a crash, a signal) is a failure that counts as one error: the
// unit did not check, and reporting zero would read as clean.
//
// The unit is matched by equality rather than by a pattern. A path is any
// string, and a unit named "a (b).pr" would defeat a regex that had to find
// where the name ends and the count clause begins — but the parent already
// knows the name it asked about.
func parseCheckResult(r *checkResult, stdout string, exitedNonZero bool) {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		for _, outcome := range []string{"ok", "warn", "FAIL"} {
			rest, found := strings.CutPrefix(line, outcome+" "+r.unit)
			if !found {
				continue
			}
			if rest != "" && !(strings.HasPrefix(rest, " (") && strings.HasSuffix(rest, ")")) {
				continue
			}
			r.outcome = outcome
			for _, c := range checkDiagCountRe.FindAllStringSubmatch(rest, -1) {
				n, _ := strconv.Atoi(c[1])
				if strings.HasPrefix(c[2], "error") {
					r.errors = n
				} else {
					r.warnings = n
				}
			}
			return
		}
	}
	if exitedNonZero {
		r.outcome, r.errors = "FAIL", 1
		return
	}
	// Exited 0 but said nothing recognizable: not a result this parent may
	// report as clean.
	r.outcome, r.errors = "FAIL", 1
	r.stderr = strings.TrimSpace(r.stderr + "\ncheck: the unit reported no result line")
}
