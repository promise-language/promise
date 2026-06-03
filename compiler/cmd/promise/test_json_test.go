package main

import (
	"strings"
	"testing"
)

// roster builds a roster marker line for a batch file from name=excluded pairs.
func batchRoster(pairs ...any) string {
	var b strings.Builder
	b.WriteString(`{"kind":"batch","tests":[`)
	for i := 0; i < len(pairs); i += 2 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"name":"`)
		b.WriteString(pairs[i].(string))
		b.WriteString(`","excluded":`)
		if pairs[i+1].(bool) {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		b.WriteString("}")
	}
	b.WriteString("]}")
	return rosterMarkerPrefix + b.String()
}

func statusOf(records []testRecord, test string) string {
	for _, r := range records {
		if r.Test == test {
			return r.Status
		}
	}
	return "<missing>"
}

func TestBuildRecordsBatchAllStatuses(t *testing.T) {
	output := strings.Join([]string{
		batchRoster("ok", false, "broke", false, "skipped", true),
		"pass (0.001s) ok",
		"FAIL (0.002s) broke",
		"  panic: assertion failed: boom",
		"",
		"2 passed, 1 failed (0.003s)",
	}, "\n")

	recs := buildTestRecords("tests/x_test.pr", output, false)
	if len(recs) != 3 {
		t.Fatalf("want 3 records, got %d: %+v", len(recs), recs)
	}
	if s := statusOf(recs, "ok"); s != "pass" {
		t.Errorf("ok: want pass, got %s", s)
	}
	if s := statusOf(recs, "broke"); s != "fail" {
		t.Errorf("broke: want fail, got %s", s)
	}
	if s := statusOf(recs, "skipped"); s != "excluded" {
		t.Errorf("skipped: want excluded, got %s", s)
	}
	for _, r := range recs {
		if r.File != "tests/x_test.pr" {
			t.Errorf("record %q has file %q, want repo-relative tests/x_test.pr", r.Test, r.File)
		}
		if r.Test == "broke" && !strings.Contains(r.Context, "boom") {
			t.Errorf("broke context missing detail: %q", r.Context)
		}
	}
}

// A hard crash mid-batch: the first unseen test is attributed "fail", the rest
// "not-run". Identity (one record per roster test) is preserved.
func TestBuildRecordsCrashNotRun(t *testing.T) {
	output := strings.Join([]string{
		batchRoster("a", false, "b", false, "c", false),
		"pass (0.001s) a",
		// b crashed the process; no summary line printed.
	}, "\n")

	recs := buildTestRecords("tests/c_test.pr", output, true /* crashed */)
	if len(recs) != 3 {
		t.Fatalf("want 3 records, got %d: %+v", len(recs), recs)
	}
	if s := statusOf(recs, "a"); s != "pass" {
		t.Errorf("a: want pass, got %s", s)
	}
	if s := statusOf(recs, "b"); s != "fail" {
		t.Errorf("b (first unseen on crash): want fail, got %s", s)
	}
	if s := statusOf(recs, "c"); s != "not-run" {
		t.Errorf("c: want not-run, got %s", s)
	}
}

// MEMLIMIT aborts without naming the offending test: first unseen -> memory,
// rest -> not-run.
func TestBuildRecordsMemoryNotRun(t *testing.T) {
	output := strings.Join([]string{
		batchRoster("a", false, "b", false, "c", false),
		"pass (0.001s) a",
		"MEMLIMIT (-) <aborted>",
		"  memory limit: exceeded (test process aborted; subsequent tests not run)",
	}, "\n")

	recs := buildTestRecords("tests/m_test.pr", output, true)
	if s := statusOf(recs, "a"); s != "pass" {
		t.Errorf("a: want pass, got %s", s)
	}
	if s := statusOf(recs, "b"); s != "memory" {
		t.Errorf("b (first unseen on memlimit): want memory, got %s", s)
	}
	if s := statusOf(recs, "c"); s != "not-run" {
		t.Errorf("c: want not-run, got %s", s)
	}
}

func TestBuildRecordsLeakAndTimeout(t *testing.T) {
	output := strings.Join([]string{
		batchRoster("leaky", false, "slow", false),
		"LEAK (0.001s) leaky",
		"  leak: 1 allocations not freed",
		"TIMEOUT (0.100s) slow",
		"  timeout: exceeded 60s limit",
		"",
		"0 passed, 0 failed, 1 leaked, 1 timed out (0.2s)",
	}, "\n")

	recs := buildTestRecords("tests/l_test.pr", output, false)
	if s := statusOf(recs, "leaky"); s != "leak" {
		t.Errorf("leaky: want leak, got %s", s)
	}
	if s := statusOf(recs, "slow"); s != "timeout" {
		t.Errorf("slow: want timeout, got %s", s)
	}
}

func TestBuildRecordsE2EPass(t *testing.T) {
	output := strings.Join([]string{
		rosterMarkerPrefix + `{"kind":"e2e","tests":[{"name":"main","excluded":false}]}`,
		"PASS (0.020s)",
		"",
		"1 passed, 0 failed (0.020s)",
	}, "\n")
	recs := buildTestRecords("tests/e2e/hello.pr", output, false)
	if len(recs) != 1 || recs[0].Test != "main" || recs[0].Status != "pass" {
		t.Fatalf("want single main/pass record, got %+v", recs)
	}
}

func TestBuildRecordsE2EFail(t *testing.T) {
	output := strings.Join([]string{
		rosterMarkerPrefix + `{"kind":"e2e","tests":[{"name":"main","excluded":false}]}`,
		"FAIL (0.020s)",
		"  expected: hi",
		"  actual:   bye",
		"",
		"0 passed, 1 failed (0.020s)",
	}, "\n")
	recs := buildTestRecords("tests/e2e/hello.pr", output, false)
	if len(recs) != 1 || recs[0].Status != "fail" {
		t.Fatalf("want main/fail record, got %+v", recs)
	}
	if !strings.Contains(recs[0].Context, "expected: hi") {
		t.Errorf("missing expected/actual context: %q", recs[0].Context)
	}
}

func TestBuildRecordsE2EExcluded(t *testing.T) {
	output := rosterMarkerPrefix + `{"kind":"e2e","tests":[{"name":"main","excluded":true}]}` +
		"\nSKIP (excluded) hello"
	recs := buildTestRecords("tests/e2e/hello.pr", output, false)
	if len(recs) != 1 || recs[0].Status != "excluded" {
		t.Fatalf("want main/excluded record, got %+v", recs)
	}
}

// No roster and no results (e.g. "no tests found") -> no records at all.
func TestBuildRecordsNoTests(t *testing.T) {
	if recs := buildTestRecords("tests/empty_test.pr", "no tests found", false); recs != nil {
		t.Fatalf("want no records, got %+v", recs)
	}
}

// Missing roster but result lines present (degraded cache-meta path): recover
// per-test records from the result lines.
func TestBuildRecordsMissingRosterRecovers(t *testing.T) {
	output := "pass (0.001s) a\nFAIL (0.002s) b\n  panic: x\n\n1 passed, 1 failed (0.003s)"
	recs := buildTestRecords("tests/r_test.pr", output, false)
	if len(recs) != 2 {
		t.Fatalf("want 2 recovered records, got %d: %+v", len(recs), recs)
	}
	if statusOf(recs, "a") != "pass" || statusOf(recs, "b") != "fail" {
		t.Errorf("recovered statuses wrong: %+v", recs)
	}
}
