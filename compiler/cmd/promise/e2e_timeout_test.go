package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// T0742: an E2E snapshot test that times out must emit the canonical TIMEOUT
// subprocess line ("TIMEOUT (Xs) name [target]" + "  timeout: exceeded Xs limit"),
// NOT the legacy "FAIL (timeout) name [target]" shape that the parent then
// lifts into the gate test identity as a phantom ledger. These tests lock the
// child-side wire format and the parent's classification of it.

func locatePromiseBin(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("PROMISE_TEST_BIN"); bin != "" {
		return bin
	}
	// go test cwd = compiler/cmd/promise → repo root is three levels up.
	candidate := filepath.Join("..", "..", "..", "bin", "promise")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	t.Skip("set PROMISE_TEST_BIN or build via bin/build to run this end-to-end test")
	return ""
}

// writeInfLoopE2E writes a Promise E2E test that loops forever, with the given
// per-test timeout annotation (e.g. "1s"). The expected output is irrelevant —
// the program never reaches it. The unique tag in the source defeats the
// per-source test-binary cache: stale cached metadata can override the
// timeout annotation (cache stores ProcessTimeoutNs = timeout + 30s buffer),
// which would otherwise stretch each test by 30 seconds.
func writeInfLoopE2E(t *testing.T, dir, name, timeoutAnnot, unique string) string {
	t.Helper()
	src := "// " + unique + "\n" +
		"main() `test(expected: \"unreachable\", timeout: \"" + timeoutAnnot + "\") {\n" +
		"  i := 0;\n" +
		"  while true {\n" +
		"    i = i + 1;\n" +
		"  }\n" +
		"  print_line(\"unreachable\");\n" +
		"}\n"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestE2ETimeoutEmitsCanonicalTimeoutLine is the child-side guard: running an
// e2e file alone (single file → executeE2EBinary path) on timeout must emit
// the TIMEOUT (Xs) line with the bare test name, plus the indented
// "timeout: exceeded Xs limit" context. It must NOT emit the legacy
// "FAIL (timeout) ..." line (which would propagate into the gate's Test
// identity field as a phantom ledger entry).
func TestE2ETimeoutEmitsCanonicalTimeoutLine(t *testing.T) {
	promiseBin := locatePromiseBin(t)

	dir, err := os.MkdirTemp("", "e2e_timeout_")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	file := writeInfLoopE2E(t, dir, "infloop_test.pr", "1s",
		fmt.Sprintf("canonical-line-test-%d-%d", os.Getpid(), time.Now().UnixNano()))

	cmd := exec.Command(promiseBin, "test", file)
	output, runErr := cmd.CombinedOutput()
	combined := string(output)

	if runErr == nil {
		t.Fatalf("expected non-zero exit on timeout, got success.\nOutput:\n%s", combined)
	}

	// Canonical TIMEOUT line: "TIMEOUT (Xs) infloop_test" (no target suffix on
	// the host run). Use a regex to ignore exact elapsed.
	tre := regexp.MustCompile(`^TIMEOUT \(\d+\.\d+s\) infloop_test\b`)
	var sawTimeoutLine bool
	for _, line := range strings.Split(combined, "\n") {
		if tre.MatchString(line) {
			sawTimeoutLine = true
			break
		}
	}
	if !sawTimeoutLine {
		t.Errorf("expected canonical TIMEOUT line; got:\n%s", combined)
	}

	if !strings.Contains(combined, "timeout: exceeded 1s limit") {
		t.Errorf("expected indented 'timeout: exceeded 1s limit' detail; got:\n%s", combined)
	}

	// Regression guard: the legacy "FAIL (timeout) ..." shape must never
	// reappear — that's the phantom-ledger root cause from T0742.
	if regexp.MustCompile(`\bFAIL \(timeout\)`).MatchString(combined) {
		t.Errorf("legacy 'FAIL (timeout) ...' shape resurfaced; got:\n%s", combined)
	}
}

// TestE2ETimeoutParentClassifiesAsTimedOut is the parent-side guard: running
// the e2e file alongside a sibling file forces the multi-file runTestFiles
// path. The parent must parse the child's TIMEOUT line via timeoutLineRe,
// count it as fileTimedOut, and emit
// "FAIL (Xs) infloop_test.pr (1 timed out)" — which the gate parser
// (tools/build/common/health_report.go) then maps to outcome=TIMEOUT with
// stable file-level identity. The trivial sibling must still pass.
func TestE2ETimeoutParentClassifiesAsTimedOut(t *testing.T) {
	promiseBin := locatePromiseBin(t)

	dir, err := os.MkdirTemp("", "e2e_timeout_parent_")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	infloop := writeInfLoopE2E(t, dir, "infloop_test.pr", "1s",
		fmt.Sprintf("parent-classify-test-%d-%d", os.Getpid(), time.Now().UnixNano()))

	trivial := filepath.Join(dir, "trivial_test.pr")
	trivialSrc := "test_trivial() `test {\n  assert(1 + 1 == 2, \"math\");\n}\n"
	if err := os.WriteFile(trivial, []byte(trivialSrc), 0644); err != nil {
		t.Fatal(err)
	}

	// -timeout 5 caps the parent's per-file wait so the test finishes quickly;
	// the test's own `timeout: "1s"` annotation still drives the actual trip.
	cmd := exec.Command(promiseBin, "test", "-timeout", "5", infloop, trivial)
	output, runErr := cmd.CombinedOutput()
	combined := string(output)

	if runErr == nil {
		t.Fatalf("expected non-zero exit on timeout, got success.\nOutput:\n%s", combined)
	}

	// Parent classification: "(1 timed out)" suffix on the FAIL line.
	parentRe := regexp.MustCompile(`^FAIL \(\d+\.\d+s\) infloop_test\.pr \(1 timed out\)`)
	var sawParentLine bool
	for _, line := range strings.Split(combined, "\n") {
		if parentRe.MatchString(line) {
			sawParentLine = true
			break
		}
	}
	if !sawParentLine {
		t.Errorf("expected parent to classify child timeout as '(1 timed out)'; got:\n%s", combined)
	}

	// The parent must NOT misclassify as a generic FAIL ("(1/1 failed)" or
	// "(compilation error)") — those would route through refineFailOutcome
	// in the gate parser as FAIL, not TIMEOUT.
	if regexp.MustCompile(`infloop_test\.pr \(\d+/\d+ failed\)`).MatchString(combined) {
		t.Errorf("parent misclassified timeout as generic FAIL; got:\n%s", combined)
	}
	if strings.Contains(combined, "infloop_test.pr (compilation error)") {
		t.Errorf("parent misclassified timeout as compilation error; got:\n%s", combined)
	}

	// Sibling trivial test must still pass — timeout in one file must not
	// take out unrelated files.
	if !strings.Contains(combined, "pass") || !strings.Contains(combined, "trivial_test.pr") {
		t.Errorf("expected sibling trivial_test.pr to pass; got:\n%s", combined)
	}
}
