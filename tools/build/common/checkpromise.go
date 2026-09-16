package common

// This file is how the checked:promise gate asks the compiler, and how it reads
// the answer. It adds nothing else: the tool a person runs by hand IS the
// command spawned here — `bin/promise check` — because the compiler is the
// Promise checker and there is no bin/ wrapper to write. A tool and the gate
// that measures the same property are one implementation in two modes
// (docs/gate-system.md), and here they are literally the same process.
//
// The subject is promiseSuiteTargets(), the same list tested:promise runs. The
// two therefore measure the same code, and a target added for one is added for
// both; nothing here builds a second list of "the Promise this project owns".
//
// The counting is `promise check`'s own — the summary line it prints. Nothing
// here re-derives an outcome by scraping diagnostics: the process that did the
// analysis is the one that knows how many it found, and a second count would be
// a second answer to one question with no way to tell which is right.

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// CheckSummary is one `promise check` sweep, as its summary line reports it.
type CheckSummary struct {
	Checked  int // units that analysed clean
	Warned   int // units that analysed with warnings only
	Failed   int // units that did not check
	Units    int // units attempted
	Errors   int // error diagnostics across every unit
	Warnings int // warning diagnostics across every unit
}

// checkSummaryRe matches the one summary line a `promise check` sweep prints:
//
//	851 checked, 1 warned, 9 failed (861 units, 27 errors, 1 warning, 42.173s)
//
// TWIN: the line is produced by runCheckSweep in compiler/cmd/promise/check.go,
// a separate Go module with no dependency edge to this one — nothing but that
// note and this one binds the two. A format change that outran this regex fails
// closed (ParseCheckSummaryLine returns nil and the gate refuses to measure)
// rather than reporting a wrong number, which is the safe direction; it is
// still a break, so change one and change the other.
var checkSummaryRe = regexp.MustCompile(
	`^(\d+) checked, (\d+) warned, (\d+) failed \((\d+) units, (\d+) errors?, (\d+) warnings?, [\d.]+s\)$`)

// ParseCheckSummaryLine finds the summary line in a sweep's output. nil when
// there is none — a run that printed no summary measured nothing, and a caller
// must be able to tell that from a clean zero.
func ParseCheckSummaryLine(output string) *CheckSummary {
	for _, line := range strings.Split(output, "\n") {
		m := checkSummaryRe.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
		return &CheckSummary{
			Checked:  n(1),
			Warned:   n(2),
			Failed:   n(3),
			Units:    n(4),
			Errors:   n(5),
			Warnings: n(6),
		}
	}
	return nil
}

// promiseCheckArgs is the exact invocation the gate makes, and the one a person
// types:
//
//	bin/promise check tests/... modules/... examples/... tools/stub/...
func promiseCheckArgs() []string {
	return append([]string{"check"}, promiseSuiteTargets()...)
}

// RunPromiseCheckCapture runs the sweep for a caller whose stdout is reserved
// for structured output — the gate, whose stdout carries one envelope and
// nothing else. Progress goes to stderr, where a person watching can still see
// it. Returns the captured output even when the run exits non-zero: a failing
// check is a successful measurement.
func RunPromiseCheckCapture(root string) (string, error) {
	promiseBin := filepath.Join(root, "bin", BinaryName())
	Progress().Clear()
	return RunTeeStderr(root, promiseBin, promiseCheckArgs()...)
}
