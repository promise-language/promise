package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T0763: machine-readable per-test output for `promise test --json`.
//
// In JSON mode the multi-file parent emits newline-delimited JSON (one
// testRecord per line) to stdout, while human progress stays on stderr. The
// gate consumes this stream as the single authoritative source of test
// identity and status — no scraping of the human-readable compact output.
//
// Each child (single-file `promise test <file>`) prints a roster marker line
// up front listing every eligible test and whether it is excluded for the
// current target. The parent combines that roster with the per-test result
// lines from the child's captured output to produce a record for EVERY
// eligible test, including ones that never ran (status "not-run") because an
// earlier test aborted the process. Identity is always (repo-relative file,
// test) and never varies with outcome.

// testRecord is one line of the --json stream: the result of a single test.
type testRecord struct {
	File    string  `json:"file"`
	Test    string  `json:"test"`
	Status  string  `json:"status"` // pass | fail | timeout | leak | memory | excluded | not-run
	Elapsed float64 `json:"elapsed"`
	Context string  `json:"context,omitempty"`
}

// rosterMarkerPrefix tags the single roster line a child emits in --json mode.
// It is parsed (and never echoed) by the parent; plain `promise test` runs do
// not emit it, so humans never see it.
const rosterMarkerPrefix = "##PROMISE_ROSTER_V1##"

// rosterEntry names one eligible test and whether it is excluded for the
// current target (a `test(exclude: ...)` match → compiled but not run → status
// "excluded"). `target(...)` declaration-level exclusion removes the test from
// compilation entirely, so it never appears in the roster.
type rosterEntry struct {
	Name     string `json:"name"`
	Excluded bool   `json:"excluded"`
}

// rosterMarker is the payload of the roster line. Kind is "batch" (one record
// per `test fn) or "e2e" (a single `main()` snapshot test).
type rosterMarker struct {
	Kind  string        `json:"kind"`
	Tests []rosterEntry `json:"tests"`
}

// testNames returns the names of a list of test functions, in declaration
// order — the roster order used for not-run attribution.
func testNames(tests []*types.Func) []string {
	names := make([]string, len(tests))
	for i, t := range tests {
		names[i] = t.Name()
	}
	return names
}

// emitRoster prints the roster marker to stdout when jsonMode is set. names is
// the full set of test functions; testExcludes maps a test name to its
// `test(exclude: ...) identifiers (batch); e2eExclude is the file-level
// `target(...) exclude set for e2e snapshot tests. target is the resolved
// triple. Called by the child before it runs anything.
func emitRoster(kind string, names []string, testExcludes map[string][]string, e2eExclude []string, target string) {
	if !childRoster {
		return
	}
	ti := sema.ParseTargetInfo(target)
	m := rosterMarker{Kind: kind}
	for _, n := range names {
		excluded := false
		if kind == "e2e" {
			excluded = isTestExcluded(target, e2eExclude)
		} else {
			for _, ex := range testExcludes[n] {
				if sema.MatchTargetIdent(ti, ex) {
					excluded = true
					break
				}
			}
		}
		m.Tests = append(m.Tests, rosterEntry{Name: n, Excluded: excluded})
	}
	data, err := json.Marshal(m)
	if err != nil {
		return // best-effort: a missing roster only loses not-run/excluded detail
	}
	fmt.Println(rosterMarkerPrefix + string(data))
}

var (
	jsonPassRe     = regexp.MustCompile(`^pass \(([\d.]+)s\) (\S+)`)
	jsonFailRe     = regexp.MustCompile(`^FAIL \(([\d.]+)s\) (\S+)`)
	jsonLeakRe     = regexp.MustCompile(`^LEAK \(([\d.]+)s\) (\S+)`)
	jsonTimeoutRe  = regexp.MustCompile(`^TIMEOUT \(([\d.]+)s\) (\S+)`)
	jsonMemlimitRe = regexp.MustCompile(`^MEMLIMIT \(`)
	jsonSummaryRe  = regexp.MustCompile(`^\d+ passed, \d+ failed`)
	jsonE2EPassRe  = regexp.MustCompile(`^PASS \(([\d.]+)s\)`)
	jsonE2EFailRe  = regexp.MustCompile(`^FAIL \(([\d.]+)s\)`)
	jsonE2ETimeRe  = regexp.MustCompile(`^TIMEOUT \(([\d.]+)s\)`)
)

// seenResult is a per-test outcome recovered from the child's output.
type seenResult struct {
	status  string
	elapsed float64
	context string
}

// buildTestRecords turns one child's captured output into a record per eligible
// test for the given repo-relative file. crashed indicates the child process
// exited non-zero. The function is pure (no I/O) so it is unit-testable.
//
// Edge attribution (documented in docs/gate-output.md):
//   - MEMLIMIT aborts the whole process without naming the offending test, so
//     the first roster test with no result is marked "memory" and the rest
//     "not-run".
//   - A hard crash with no summary line marks the first unseen test "fail"
//     (with the crash context) and the rest "not-run".
func buildTestRecords(relFile, output string, crashed bool) []testRecord {
	marker, seen, memlimit, sawSummary := parseChildOutput(output)
	if marker == nil {
		// Files with no eligible tests (or excluded from compilation entirely)
		// produce no records at all.
		if t := strings.TrimSpace(output); t == "" || t == "no tests found" {
			return nil
		}
		// No roster (e.g. missing cache meta, or a compile error before the
		// marker). Recover per-test records from any result lines we did see;
		// if there were none, emit a single file-level fail so the file still
		// reports. Loses not-run/excluded detail but keeps stable identity.
		if len(seen) == 0 {
			return []testRecord{{File: relFile, Test: "main", Status: "fail", Context: firstNonEmpty(output)}}
		}
		var records []testRecord
		for name, r := range seen {
			records = append(records, testRecord{File: relFile, Test: name, Status: r.status, Elapsed: r.elapsed, Context: r.context})
		}
		sort.Slice(records, func(i, j int) bool { return records[i].Test < records[j].Test })
		return records
	}

	if marker.Kind == "e2e" {
		return []testRecord{buildE2ERecord(relFile, marker, output, crashed)}
	}

	// Batch: one record per roster entry, identity stable across outcomes.
	var records []testRecord
	var unseen []string
	for _, e := range marker.Tests {
		if e.Excluded {
			records = append(records, testRecord{File: relFile, Test: e.Name, Status: "excluded"})
			continue
		}
		if r, ok := seen[e.Name]; ok {
			records = append(records, testRecord{File: relFile, Test: e.Name, Status: r.status, Elapsed: r.elapsed, Context: r.context})
			continue
		}
		unseen = append(unseen, e.Name)
		records = append(records, testRecord{File: relFile, Test: e.Name, Status: "not-run"})
	}

	// Attribute the abort to the first unseen test when the process died.
	if len(unseen) > 0 {
		first := unseen[0]
		var promote, ctx string
		switch {
		case memlimit:
			promote, ctx = "memory", "memory limit exceeded (test process aborted)"
		case crashed && !sawSummary:
			promote, ctx = "fail", "process crashed before completing"
		}
		if promote != "" {
			for i := range records {
				if records[i].Test == first && records[i].Status == "not-run" {
					records[i].Status = promote
					records[i].Context = ctx
					break
				}
			}
		}
	}
	return records
}

// buildE2ERecord classifies a single e2e/snapshot test from its output.
func buildE2ERecord(relFile string, marker *rosterMarker, output string, crashed bool) testRecord {
	name := "main"
	if len(marker.Tests) > 0 {
		if marker.Tests[0].Name != "" {
			name = marker.Tests[0].Name
		}
		if marker.Tests[0].Excluded {
			return testRecord{File: relFile, Test: name, Status: "excluded"}
		}
	}
	rec := testRecord{File: relFile, Test: name}
	lines := strings.Split(output, "\n")
	for i, line := range lines {
		switch {
		case jsonE2ETimeRe.MatchString(line):
			rec.Status = "timeout"
			rec.Elapsed = parseSeconds(jsonE2ETimeRe.FindStringSubmatch(line)[1])
			rec.Context = collectContext(lines, i+1)
			return rec
		case jsonE2EFailRe.MatchString(line):
			rec.Status = "fail"
			rec.Elapsed = parseSeconds(jsonE2EFailRe.FindStringSubmatch(line)[1])
			rec.Context = collectContext(lines, i+1)
			return rec
		case jsonE2EPassRe.MatchString(line):
			rec.Status = "pass"
			rec.Elapsed = parseSeconds(jsonE2EPassRe.FindStringSubmatch(line)[1])
			return rec
		}
	}
	// No recognizable line — treat a crash as fail, otherwise fail defensively.
	rec.Status = "fail"
	if crashed {
		rec.Context = "process crashed before completing"
	} else {
		rec.Context = firstNonEmpty(output)
	}
	return rec
}

// parseChildOutput extracts the roster marker, per-test results, and abort
// signals from a child's captured output.
func parseChildOutput(output string) (marker *rosterMarker, seen map[string]seenResult, memlimit, sawSummary bool) {
	seen = map[string]seenResult{}
	lines := strings.Split(output, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, rosterMarkerPrefix) {
			var m rosterMarker
			if json.Unmarshal([]byte(strings.TrimPrefix(line, rosterMarkerPrefix)), &m) == nil {
				marker = &m
			}
			continue
		}
		if jsonSummaryRe.MatchString(line) {
			sawSummary = true
			continue
		}
		if jsonMemlimitRe.MatchString(line) {
			memlimit = true
			continue
		}
		if m := jsonPassRe.FindStringSubmatch(line); m != nil && !strings.Contains(m[2], ".pr") {
			seen[m[2]] = seenResult{status: "pass", elapsed: parseSeconds(m[1])}
		} else if m := jsonFailRe.FindStringSubmatch(line); m != nil && !strings.Contains(m[2], ".pr") {
			seen[m[2]] = seenResult{status: "fail", elapsed: parseSeconds(m[1]), context: collectContext(lines, i+1)}
		} else if m := jsonLeakRe.FindStringSubmatch(line); m != nil && !strings.Contains(m[2], ".pr") {
			seen[m[2]] = seenResult{status: "leak", elapsed: parseSeconds(m[1]), context: collectContext(lines, i+1)}
		} else if m := jsonTimeoutRe.FindStringSubmatch(line); m != nil && !strings.Contains(m[2], ".pr") {
			seen[m[2]] = seenResult{status: "timeout", elapsed: parseSeconds(m[1]), context: collectContext(lines, i+1)}
		}
	}
	return
}

// collectContext gathers the indented context lines that follow a failure
// result line (panic/leak/timeout/expected/actual detail), joined by newlines.
func collectContext(lines []string, start int) string {
	var ctx []string
	for j := start; j < len(lines); j++ {
		if strings.HasPrefix(lines[j], "  ") {
			ctx = append(ctx, strings.TrimSpace(lines[j]))
			continue
		}
		break
	}
	return strings.Join(ctx, "\n")
}

func parseSeconds(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func firstNonEmpty(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, rosterMarkerPrefix) {
			return t
		}
	}
	return ""
}

// writeTestRecords serializes records as newline-delimited JSON to w. Each
// record is flushed on its own line so an abrupt termination loses at most a
// trailing partial line, never the whole document.
func writeTestRecords(w io.Writer, records []testRecord) {
	enc := json.NewEncoder(w)
	for _, r := range records {
		_ = enc.Encode(r) // Encode appends '\n'
	}
}

// absPath returns the absolute, forward-slash path for a test file. The runner
// deliberately does NOT try to deduce the repository root — making identity
// root-relative is the gate's job (it knows the root authoritatively), so the
// runner just emits the unambiguous absolute path. Falls back to the cleaned
// input if Abs fails.
func absPath(file string) string {
	abs, err := filepath.Abs(file)
	if err != nil {
		return filepath.ToSlash(file)
	}
	return filepath.ToSlash(abs)
}
