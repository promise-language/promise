package common

import "testing"

// The coverage gate and bin/coverage measure authored compiler source, not the
// 30k lines the ANTLR generator writes. The exclusion itself is
// excludeGeneratedGoPackages, tested beside the rest of the check selection in
// check_test.go — one rule, one home, one test.

// coverageStream is a `promise test --json -coverage` stream: coverage records
// and test records interleaved, as the runner emits them per file.
const coverageStream = `{"kind":"coverage","file":"/repo/tests/a_test.pr","covered":8,"total":10}
{"file":"/repo/tests/a_test.pr","test":"test_one","status":"pass","elapsed":0.001}
{"file":"/repo/tests/a_test.pr","test":"test_two","status":"fail","elapsed":0.002,"context":"panic"}
{"kind":"coverage","file":"/repo/tests/b_test.pr","covered":2,"total":10}
{"file":"/repo/tests/b_test.pr","test":"test_three","status":"pass","elapsed":0.003}
{"file":"/repo/tests/b_test.pr","test":"test_wasm_only","status":"excluded","elapsed":0}
{"file":"/repo/tests/b_test.pr","test":"test_leaky","status":"leak","elapsed":0.004}
`

func TestParsePromiseCoverageJSONL(t *testing.T) {
	pct, passed, failed := ParsePromiseCoverageJSONL(coverageStream)

	// 10 of 20 blocks across the two files.
	if pct != 50.0 {
		t.Errorf("pct = %v, want 50", pct)
	}
	if passed != 2 {
		t.Errorf("passed = %d, want 2", passed)
	}
	// fail + leak; the excluded test is not a failure.
	if failed != 2 {
		t.Errorf("failed = %d, want 2", failed)
	}
}

// Coverage records carry no test identity, so the test-record parser every
// other gate uses must step over them rather than choke or count them.
func TestParseTestJSONLSkipsCoverageRecords(t *testing.T) {
	records := ParseTestJSONL(coverageStream)
	if len(records) != 5 {
		t.Fatalf("got %d test records, want 5: %+v", len(records), records)
	}
	for _, r := range records {
		if r.Test == "" || r.Status == "" {
			t.Errorf("record with no identity leaked through: %+v", r)
		}
	}
}

// The percentage is rounded to one decimal so the metric is continuous with the
// runner's own "total: X%" report, which this replaced.
func TestParsePromiseCoverageJSONLRounding(t *testing.T) {
	const stream = `{"kind":"coverage","file":"/repo/a.pr","covered":1,"total":3}
`
	if pct, _, _ := ParsePromiseCoverageJSONL(stream); pct != 33.3 {
		t.Errorf("pct = %v, want 33.3", pct)
	}
}

// No coverage records at all (coverage never ran, or every file failed to
// compile) reports 0 rather than dividing by zero.
func TestParsePromiseCoverageJSONLNoCoverageRecords(t *testing.T) {
	const stream = `{"file":"/repo/a.pr","test":"t","status":"pass","elapsed":0.1}
`
	pct, passed, failed := ParsePromiseCoverageJSONL(stream)
	if pct != 0 || passed != 1 || failed != 0 {
		t.Errorf("got (%v, %d, %d), want (0, 1, 0)", pct, passed, failed)
	}
}
