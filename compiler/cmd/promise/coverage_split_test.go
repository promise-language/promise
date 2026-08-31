package main

import "testing"

// A coverage-mode child prints its per-file report as a trailer after the test
// results. The runner must lift the totals out and hand the caller output that
// still looks exactly like a non-coverage run, since both the human aggregator
// and buildTestRecords parse it afterwards.
func TestSplitCoverageSectionExtractsTotalsAndStrips(t *testing.T) {
	const output = `pass (0.001s) test_one
pass (0.002s) test_two

2 passed (0.003s)

=== Coverage ===
  tests/a_test.pr                                    80.0%	(8/10 blocks)

total: 80.0% (8/10 blocks)`

	clean, covered, total, ok := splitCoverageSection(output)
	if !ok {
		t.Fatal("no coverage section found")
	}
	if covered != 8 || total != 10 {
		t.Errorf("got %d/%d blocks, want 8/10", covered, total)
	}
	if want := "pass (0.001s) test_one\npass (0.002s) test_two\n\n2 passed (0.003s)\n"; clean != want {
		t.Errorf("clean output = %q, want %q", clean, want)
	}
}

// A child that died before printing its trailer reported nothing — distinct
// from a file that genuinely measured zero blocks, which would drag the total
// down if folded in.
func TestSplitCoverageSectionAbsentIsNotZero(t *testing.T) {
	clean, covered, total, ok := splitCoverageSection("FAIL (0.001s) test_one\n  panic: assertion failed")
	if ok {
		t.Errorf("reported a measurement (%d/%d) where there was none", covered, total)
	}
	if clean != "FAIL (0.001s) test_one\n  panic: assertion failed" {
		t.Errorf("output was modified: %q", clean)
	}
}

// Non-coverage output must pass through byte-identical: every multi-file run
// hits this path, and only the coverage runs have a trailer to remove.
func TestSplitCoverageSectionPassesThroughPlainOutput(t *testing.T) {
	const output = "pass (0.001s) test_one\n\n1 passed (0.001s)\n"
	clean, _, _, ok := splitCoverageSection(output)
	if ok {
		t.Error("found a coverage section in plain output")
	}
	if clean != output {
		t.Errorf("clean = %q, want unchanged %q", clean, output)
	}
}
