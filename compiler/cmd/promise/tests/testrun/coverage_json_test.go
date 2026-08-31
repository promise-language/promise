package testrun

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// coverageJSONSource has one test that exercises a branch and one that does
// not, so the file cannot report either 0% or 100% — a stream that reported a
// coverage record but measured nothing would still look plausible otherwise.
const coverageJSONSource = `
classify(int n) string {
  if (n > 0) { return "positive"; }
  if (n < 0) { return "negative"; }
  return "zero";
}

test_positive() ` + "`test" + ` {
  assert(classify(1) == "positive", "positive branch");
}

test_zero() ` + "`test" + ` {
  assert(classify(0) == "zero", "zero branch");
}
`

// jsonLine is the union of the two record kinds the --json stream carries.
type jsonLine struct {
	Kind    string `json:"kind"`
	File    string `json:"file"`
	Test    string `json:"test"`
	Status  string `json:"status"`
	Covered int    `json:"covered"`
	Total   int    `json:"total"`
}

// End-to-end: `promise test --json -coverage` must put block coverage on the
// machine-readable stream. Before this, -coverage only ever reached the human
// aggregator, which --json bypasses entirely — so the coverage gate had to
// scrape human output to get a number at all.
func TestCoverageRecordsOnJSONStream(t *testing.T) {
	t.Parallel()
	promiseBin := clitest.Bin(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "coverage_json_test.pr")
	if err := os.WriteFile(src, []byte(coverageJSONSource), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(promiseBin, "test", "--json", "-coverage", src)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("promise test --json -coverage: %v\nstderr:\n%s", err, stderr.String())
	}

	var covRecords []jsonLine
	tests := map[string]string{}
	for _, line := range bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec jsonLine
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("stdout line is not JSON: %q (%v)", line, err)
		}
		if rec.Kind == "coverage" {
			covRecords = append(covRecords, rec)
			continue
		}
		tests[rec.Test] = rec.Status
	}

	// The test records must be unaffected — stripping the coverage trailer
	// must not eat the results the other gates read from this same stream.
	if got := len(tests); got != 2 {
		t.Errorf("got %d test records %v, want 2", got, tests)
	}
	for _, name := range []string{"test_positive", "test_zero"} {
		if tests[name] != "pass" {
			t.Errorf("test %s = %q, want pass", name, tests[name])
		}
	}

	if len(covRecords) != 1 {
		t.Fatalf("got %d coverage records, want 1: %+v", len(covRecords), covRecords)
	}
	rec := covRecords[0]
	if rec.Total <= 0 {
		t.Errorf("coverage record reports %d total blocks, want > 0: %+v", rec.Total, rec)
	}
	if rec.Covered <= 0 || rec.Covered >= rec.Total {
		t.Errorf("covered = %d of %d; the file has one unexercised branch, so it must be a partial measurement",
			rec.Covered, rec.Total)
	}
	if filepath.Base(rec.File) != "coverage_json_test.pr" {
		t.Errorf("coverage record file = %q, want the test source", rec.File)
	}
}
