package common

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestBuildInstallGateOutput verifies the aggregation of the platform script's
// two artifacts (tests.jsonl + phases.json) into the standard envelope: per-test
// metrics from the JSONL, per-phase _ok metrics from phases.json, and the file
// grouping — all without running the script.
func TestBuildInstallGateOutput(t *testing.T) {
	work := t.TempDir()

	// Two tests in one file: one pass, one fail. Build the fixture with
	// json.Marshal (not string concatenation) so the OS-native path is escaped
	// correctly — on Windows the path contains backslashes (e.g. C:\Users\...),
	// which would form invalid JSON escapes inside a raw string literal (T0823).
	// The path must be under work/src (the srcDir the suite ran in) so
	// buildInstallGateOutput can relativize it — T0902.
	f := filepath.Join(work, "src", "tests", "e2e", "basics.pr")
	mk := func(test, status, ctx string, elapsed float64) string {
		b, _ := json.Marshal(jsonlRecord{
			File: f, Test: test, Status: status, Elapsed: elapsed, Context: ctx,
		})
		return string(b)
	}
	jsonl := mk("add", "pass", "", 0.01) + "\n" +
		mk("broken", "fail", "assertion failed", 0.02) + "\n"
	if err := os.WriteFile(filepath.Join(work, "tests.jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatal(err)
	}
	// fetch/install/sanity passed, test failed.
	phases := `{"fetch":"pass","install":"pass","sanity":"pass","test":"fail"}`
	if err := os.WriteFile(filepath.Join(work, "phases.json"), []byte(phases), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := buildInstallGateOutput("darwin-arm64", "thin", work)
	if err != nil {
		t.Fatal(err)
	}

	if out.Target != "darwin-arm64" || out.Complete != "install-thin" {
		t.Fatalf("envelope target/complete = %q/%q", out.Target, out.Complete)
	}
	checks := map[string]float64{
		"install_thin_test_count":    1,
		"install_thin_test_failures": 1,
		"install_thin_fetch_ok":      1,
		"install_thin_install_ok":    1,
		"install_thin_sanity_ok":     1,
		"install_thin_test_ok":       0,
	}
	for k, want := range checks {
		if got := out.Metrics[k]; got != want {
			t.Errorf("metric %s = %v, want %v", k, got, want)
		}
	}
	// File grouping: one group (relativized), two tests.
	if len(out.Files) != 1 {
		t.Fatalf("expected 1 file group, got %d", len(out.Files))
	}
	if out.Files[0].File != "tests/e2e/basics.pr" {
		t.Errorf("group file = %q, want tests/e2e/basics.pr", out.Files[0].File)
	}
	if len(out.Files[0].Tests) != 2 {
		t.Errorf("expected 2 tests in group, got %d", len(out.Files[0].Tests))
	}
}

// TestInstallFixtureBackslashPath proves the T0823 fix without a Windows host:
// a record whose file path contains backslashes (as it would on Windows) must
// survive the json.Marshal → ParseTestJSONL round-trip. The old raw-concat
// fixture produced invalid JSON escapes (e.g. \U, \t) for such paths, so
// ParseTestJSONL silently dropped every line and the metrics/groups came out 0.
func TestInstallFixtureBackslashPath(t *testing.T) {
	win := `C:\Users\runner\tests\e2e\basics.pr`
	b, err := json.Marshal(jsonlRecord{File: win, Test: "add", Status: "pass", Elapsed: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	recs := ParseTestJSONL(string(b) + "\n")
	if len(recs) != 1 {
		t.Fatalf("expected 1 record from backslash-path fixture, got %d", len(recs))
	}
	if recs[0].File != win {
		t.Errorf("round-tripped file = %q, want %q", recs[0].File, win)
	}
}

// TestBuildInstallGateOutputMissingArtifacts: a phase that never ran (no
// phases.json, no tests.jsonl — e.g. fetch failed before the script wrote them)
// reports all phases as not-ok and zero test counts rather than crashing.
func TestBuildInstallGateOutputMissingArtifacts(t *testing.T) {
	work := t.TempDir() // empty — no artifacts

	out, err := buildInstallGateOutput("linux-amd64", "full", work)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range installPhasesFor("full") {
		if got := out.Metrics["install_full_"+p+"_ok"]; got != 0 {
			t.Errorf("missing-artifact phase %s_ok = %v, want 0", p, got)
		}
	}
	if got := out.Metrics["install_full_test_count"]; got != 0 {
		t.Errorf("install_full_test_count = %v, want 0", got)
	}
	if len(out.Files) != 0 {
		t.Errorf("expected no file groups, got %d", len(out.Files))
	}
}

// TestReadInstallPhases tolerates missing and malformed files.
func TestReadInstallPhases(t *testing.T) {
	if m := readInstallPhases(filepath.Join(t.TempDir(), "nope.json")); len(m) != 0 {
		t.Errorf("missing file: want empty map, got %v", m)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte("{not json"), 0o644)
	if m := readInstallPhases(bad); len(m) != 0 {
		t.Errorf("malformed file: want empty map, got %v", m)
	}
}

// TestRunGateInstallRejectsBadVariant: the variant is validated before any
// script runs, so a bad/absent variant errors immediately. Covers every arg
// shape that must fail before any network/exec work: absent variant, an unknown
// variant value, --system with no variant, a dangling --variant with no value,
// and an unrecognized flag.
func TestRunGateInstallRejectsBadVariant(t *testing.T) {
	root := t.TempDir()
	bad := [][]string{
		nil,
		{"--variant", "bogus"},
		{"--system"},
		{"--variant"},                    // dangling flag, no value
		{"-variant"},                     // dash-form dangling, no value
		{"--bogus"},                      // unknown flag
		{"--variant", "thin", "--extra"}, // valid variant but trailing junk
	}
	for _, args := range bad {
		if err := runGateInstall(root, args); err == nil {
			t.Errorf("runGateInstall(%v) = nil, want error", args)
		}
	}
}

// TestEnvWith verifies the sandbox env-override semantics: an override REPLACES
// any existing entry for the same key (dropping the original regardless of its
// position) and brand-new keys are appended. This is the mechanism that isolates
// the clean-slate sandbox's HOME/PROMISE_HOME/PATH from the dev environment, so
// an override that failed to drop the original would leak the real environment.
func TestEnvWith(t *testing.T) {
	base := []string{"HOME=/real/home", "PATH=/real/bin", "KEEP=1"}
	got := envWith(base, map[string]string{
		"HOME":         "/sandbox",          // replaces existing
		"PROMISE_HOME": "/sandbox/.promise", // brand new
	})

	m := map[string]string{}
	homeCount := 0
	for _, e := range got {
		k, v, _ := strings.Cut(e, "=")
		if k == "HOME" {
			homeCount++
		}
		m[k] = v
	}
	if homeCount != 1 {
		t.Errorf("HOME appears %d times, want exactly 1 (original must be dropped)", homeCount)
	}
	if m["HOME"] != "/sandbox" {
		t.Errorf("HOME = %q, want /sandbox (override must win)", m["HOME"])
	}
	if m["PROMISE_HOME"] != "/sandbox/.promise" {
		t.Errorf("PROMISE_HOME = %q, want /sandbox/.promise (new key appended)", m["PROMISE_HOME"])
	}
	if m["PATH"] != "/real/bin" || m["KEEP"] != "1" {
		t.Errorf("untouched keys altered: PATH=%q KEEP=%q", m["PATH"], m["KEEP"])
	}

	// A key that is a strict prefix of a base key (HOME vs HOMEBREW) must NOT
	// match — envWith drops on "key=" prefix, so HOMEBREW survives a HOME override.
	got = envWith([]string{"HOMEBREW=/brew", "HOME=/real"}, map[string]string{"HOME": "/new"})
	var names []string
	for _, e := range got {
		k, _, _ := strings.Cut(e, "=")
		names = append(names, k)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "HOME,HOMEBREW" {
		t.Errorf("prefix collision: got keys %v, want [HOME HOMEBREW]", names)
	}

	// Empty override set returns the base entries unchanged.
	if got := envWith([]string{"A=1", "B=2"}, nil); len(got) != 2 {
		t.Errorf("empty override: got %d entries, want 2", len(got))
	}
}

// TestDownloadFile exercises the install-script fetch path: a 200 writes the
// body to dest; a non-200 status is surfaced as an error and no partial file is
// relied upon; an unreachable URL errors.
func TestDownloadFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/install.sh":
			_, _ = w.Write([]byte("#!/bin/sh\necho ok\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "install.sh")
	if err := downloadFile(srv.URL+"/install.sh", dest); err != nil {
		t.Fatalf("downloadFile success path: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || !strings.Contains(string(data), "echo ok") {
		t.Fatalf("downloaded body = %q (err %v), want the script", data, err)
	}

	// Non-200 → error.
	if err := downloadFile(srv.URL+"/missing", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Error("downloadFile(404) = nil, want error")
	}

	// Unreachable host → transport error.
	if err := downloadFile("http://127.0.0.1:1/install.sh", filepath.Join(t.TempDir(), "y")); err == nil {
		t.Error("downloadFile(unreachable) = nil, want error")
	}
}

// TestDownloadFileCreateFails: when the server returns 200 but the destination
// path is in a non-existent directory, os.Create fails and downloadFile must
// surface that error rather than silently succeeding.
func TestDownloadFileCreateFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	// Parent directory does not exist → os.Create returns an error.
	dest := filepath.Join(t.TempDir(), "nonexistent-subdir", "install.sh")
	if err := downloadFile(srv.URL+"/install.sh", dest); err == nil {
		t.Fatal("downloadFile with uncreateable dest = nil, want error")
	}
}

// TestBuildInstallGateOutputReadWarning: when tests.jsonl exists but is not
// readable (e.g. it's a directory — EISDIR, which is not os.IsNotExist), the
// function must log a warning and continue, returning a valid output with zero
// test counts rather than failing. This covers the non-NotExist read-error branch.
func TestBuildInstallGateOutputReadWarning(t *testing.T) {
	work := t.TempDir()
	// Create tests.jsonl as a directory so os.ReadFile returns EISDIR — an error
	// that is definitely not os.IsNotExist.
	if err := os.Mkdir(filepath.Join(work, "tests.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	// phases.json: all pass
	phases := `{"fetch":"pass","install":"pass","sanity":"pass","test":"pass"}`
	if err := os.WriteFile(filepath.Join(work, "phases.json"), []byte(phases), 0o644); err != nil {
		t.Fatal(err)
	}

	// Must succeed: the warning is printed to stderr but doesn't fail the gate.
	out, err := buildInstallGateOutput("linux-amd64", "thin", work)
	if err != nil {
		t.Fatalf("buildInstallGateOutput with EISDIR tests.jsonl = error %v, want nil", err)
	}
	// No test records parsed → test_count == 0.
	if got := out.Metrics["install_thin_test_count"]; got != 0 {
		t.Errorf("install_thin_test_count = %v, want 0 (no records readable)", got)
	}
	// Phases still recorded correctly.
	if got := out.Metrics["install_thin_fetch_ok"]; got != 1 {
		t.Errorf("install_thin_fetch_ok = %v, want 1", got)
	}
}

// TestBuildInstallGateOutputBuildError: when tests.jsonl contains a record whose
// file path is outside srcDir (work/src), BuildGateOutput returns a hard error and
// buildInstallGateOutput must propagate it. This exercises the
// `return nil, fmt.Errorf("buildInstallGateOutput: %w", err)` branch.
func TestBuildInstallGateOutputBuildError(t *testing.T) {
	work := t.TempDir()
	// Record whose file is NOT under work/src — it escapes srcDir, so relToBase
	// returns a hard error.
	badJSONL := `{"file":"/tmp/outside/x_test.pr","test":"main","status":"pass","elapsed":0.01}` + "\n"
	if err := os.WriteFile(filepath.Join(work, "tests.jsonl"), []byte(badJSONL), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := buildInstallGateOutput("linux-amd64", "thin", work)
	if err == nil {
		t.Fatal("buildInstallGateOutput with escaping file path = nil, want error")
	}
	if !strings.Contains(err.Error(), "buildInstallGateOutput") {
		t.Errorf("error %q does not mention buildInstallGateOutput", err.Error())
	}
}

// TestInstallTestFailures verifies the test-phase verdict logic: because
// `promise test --json` exits 0 even when tests fail, the gate must count
// failures from the records. Any status other than "pass"/"excluded" (fail,
// leak, timeout, memory, not-run) is a failure; blank/malformed lines are
// skipped. This is the fix for the gate previously reporting "all phases passed"
// off the (always-0) --json exit code.
func TestInstallTestFailures(t *testing.T) {
	jsonl := `{"file":"a.pr","test":"ok","status":"pass","elapsed":0.01}
{"file":"a.pr","test":"skipped","status":"excluded","elapsed":0}
{"file":"a.pr","test":"broke","status":"fail","elapsed":0.02,"context":"boom"}
{"file":"a.pr","test":"leaky","status":"leak","elapsed":0.01}
{"file":"a.pr","test":"slow","status":"timeout","elapsed":10}
{"file":"a.pr","test":"hungry","status":"memory","elapsed":0.5}
{"file":"a.pr","test":"missed","status":"not-run","elapsed":0}

{not valid json}
`
	if got := installTestFailures([]byte(jsonl)); got != 5 {
		t.Errorf("installTestFailures = %d, want 5 (fail+leak+timeout+memory+not-run; pass/excluded/blank/malformed excluded)", got)
	}

	allPass := `{"file":"a.pr","test":"x","status":"pass","elapsed":0.01}
{"file":"a.pr","test":"y","status":"excluded","elapsed":0}
`
	if got := installTestFailures([]byte(allPass)); got != 0 {
		t.Errorf("installTestFailures(all pass/excluded) = %d, want 0", got)
	}

	if got := installTestFailures(nil); got != 0 {
		t.Errorf("installTestFailures(empty) = %d, want 0", got)
	}
}

// TestIsFullGitSHA: the install gate treats the binary's `version --commit`
// output as provenance only when it is a bare 40-char lowercase-hex SHA. Anything
// else — empty (unstamped build), a "promise version <v>" line (binary predating
// --commit support), an uppercase or wrong-length string — must be rejected so
// the gate fails with the accurate "no provenance" error rather than feeding junk
// to git cat-file (T0854).
func TestIsFullGitSHA(t *testing.T) {
	ok := []string{
		"0123456789abcdef0123456789abcdef01234567",
		"ffffffffffffffffffffffffffffffffffffffff",
	}
	for _, s := range ok {
		if !isFullGitSHA(s) {
			t.Errorf("isFullGitSHA(%q) = false, want true", s)
		}
	}
	bad := []string{
		"",                       // unstamped build
		"promise version 2026.0", // pre-stamp binary fell through to printVersion
		"0123456789abcdef0123456789abcdef0123456",   // 39 chars
		"0123456789abcdef0123456789abcdef012345678", // 41 chars
		"0123456789ABCDEF0123456789abcdef01234567",  // uppercase hex
		"0123456789abcdefg123456789abcdef01234567",  // non-hex char
		"unknown", // GitSHA's failure sentinel
	}
	for _, s := range bad {
		if isFullGitSHA(s) {
			t.Errorf("isFullGitSHA(%q) = true, want false", s)
		}
	}
}

// TestInstallPhasesFor: the full variant adds an "offline" phase (self-contained
// compile+run under network blackhole) that the thin variant omits, since thin
// fetches blobs on first compile and makes no offline guarantee.
func TestInstallPhasesFor(t *testing.T) {
	if got := strings.Join(installPhasesFor("thin"), ","); got != "fetch,install,sanity,test" {
		t.Errorf("thin phases = %q, want fetch,install,sanity,test", got)
	}
	if got := strings.Join(installPhasesFor("full"), ","); got != "fetch,install,sanity,test,offline" {
		t.Errorf("full phases = %q, want fetch,install,sanity,test,offline", got)
	}
}
