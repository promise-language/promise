package common

import (
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
	root := t.TempDir()
	work := t.TempDir()

	// Two tests in one file: one pass, one fail.
	f := filepath.Join(root, "tests", "e2e", "basics.pr")
	jsonl := `{"file":"` + f + `","test":"add","status":"pass","elapsed":0.01}
{"file":"` + f + `","test":"broken","status":"fail","elapsed":0.02,"context":"assertion failed"}
`
	if err := os.WriteFile(filepath.Join(work, "tests.jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatal(err)
	}
	// fetch/install/sanity passed, test failed.
	phases := `{"fetch":"pass","install":"pass","sanity":"pass","test":"fail"}`
	if err := os.WriteFile(filepath.Join(work, "phases.json"), []byte(phases), 0o644); err != nil {
		t.Fatal(err)
	}

	out := buildInstallGateOutput(root, "darwin-arm64", "thin", work)

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

// TestBuildInstallGateOutputMissingArtifacts: a phase that never ran (no
// phases.json, no tests.jsonl — e.g. fetch failed before the script wrote them)
// reports all phases as not-ok and zero test counts rather than crashing.
func TestBuildInstallGateOutputMissingArtifacts(t *testing.T) {
	root := t.TempDir()
	work := t.TempDir() // empty — no artifacts

	out := buildInstallGateOutput(root, "linux-amd64", "full", work)
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
		{"--variant"},          // dangling flag, no value
		{"-variant"},           // dash-form dangling, no value
		{"--bogus"},            // unknown flag
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
		"HOME":        "/sandbox",      // replaces existing
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
