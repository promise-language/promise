package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDoctorDefaultOutput(t *testing.T) {
	output := captureStdout(t, func() {
		runDoctor(nil)
	})

	if !strings.Contains(output, "Promise doctor") {
		t.Error("missing header")
	}
	if !strings.Contains(output, "Promise installation") {
		t.Error("missing installation check")
	}
	if !strings.Contains(output, "LLVM toolchain") {
		t.Error("missing LLVM check")
	}
	if !strings.Contains(output, "PATH") {
		t.Error("missing PATH check")
	}
}

func TestDoctorJSON(t *testing.T) {
	output := captureStdout(t, func() {
		runDoctor([]string{"-json"})
	})

	var report doctorReport
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, output)
	}
	if len(report.Checks) < 7 {
		t.Errorf("expected at least 7 checks, got %d", len(report.Checks))
	}

	// Verify all checks have valid status values
	for _, c := range report.Checks {
		switch c.Status {
		case "ok", "warning", "error":
		default:
			t.Errorf("check %q has invalid status %q", c.Name, c.Status)
		}
	}
}

func TestDoctorFixFlag(t *testing.T) {
	// Java is a dev-only check, reachable only via -dev.
	output := captureStdout(t, func() {
		runDoctor([]string{"-dev", "-fix"})
	})

	// Java is typically missing in CI — the fix hint should appear with -fix
	if strings.Contains(output, "Java") && strings.Contains(output, "[!]") {
		if !strings.Contains(output, "Fix:") {
			t.Error("expected Fix hint with -fix flag for missing Java")
		}
	}
}

func TestDoctorDefaultExcludesDevChecks(t *testing.T) {
	// A fresh end-user install must not warn about compiler-development tools.
	output := captureStdout(t, func() {
		runDoctor(nil)
	})
	for _, name := range []string{"Java", "wasmtime", "node"} {
		if strings.Contains(output, name) {
			t.Errorf("default doctor output should not mention dev-only check %q", name)
		}
	}
}

func TestDoctorDevFlagIncludesDevChecks(t *testing.T) {
	output := captureStdout(t, func() {
		runDoctor([]string{"-dev"})
	})
	for _, name := range []string{"Java (optional", "wasmtime (optional", "node (optional"} {
		if !strings.Contains(output, name) {
			t.Errorf("-dev doctor output should mention dev-only check %q", name)
		}
	}
}

func TestDoctorDevGatingJSON(t *testing.T) {
	// Assert gating at the structured-report level (not just rendered text):
	// dev-only checks must be entirely absent from report.Checks by default, so
	// a missing Java/Node/wasmtime never inflates report.Warnings on a fresh
	// end-user install (T0819). With -dev they appear, in addition to the
	// default set (i.e. the dev flag is purely additive).
	devNames := map[string]bool{
		"Java (optional — compiler development only)": true,
		"wasmtime (optional — wasm32-wasi target)":    true,
		"node (optional — wasm32-web target tests)":   true,
	}

	parse := func(args []string) doctorReport {
		t.Helper()
		out := captureStdout(t, func() { runDoctor(args) })
		var r doctorReport
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("invalid JSON for args %v: %v\noutput: %s", args, err, out)
		}
		return r
	}

	def := parse([]string{"-json"})
	for _, c := range def.Checks {
		if devNames[c.Name] {
			t.Errorf("default report should not contain dev-only check %q", c.Name)
		}
	}

	dev := parse([]string{"-json", "-dev"})
	seen := map[string]bool{}
	for _, c := range dev.Checks {
		if devNames[c.Name] {
			seen[c.Name] = true
		}
	}
	for name := range devNames {
		if !seen[name] {
			t.Errorf("-dev report should contain dev-only check %q", name)
		}
	}

	// Additive: -dev adds exactly the three dev checks, nothing removed.
	if got, want := len(dev.Checks), len(def.Checks)+len(devNames); got != want {
		t.Errorf("-dev check count = %d, want default(%d)+%d = %d", got, len(def.Checks), len(devNames), want)
	}
}

func TestDoctorCheckInstallation(t *testing.T) {
	c := doctorCheckInstallation()
	if c.Status != "ok" {
		t.Errorf("expected ok, got %s: %s", c.Status, c.Summary)
	}
	if !c.Required {
		t.Error("installation check should be required")
	}
	if c.Summary == "" {
		t.Error("expected non-empty summary")
	}
	// Should mention version
	if !strings.Contains(c.Summary, "Version:") {
		t.Error("summary should contain version")
	}
}

func TestDoctorCheckLLVM(t *testing.T) {
	c := doctorCheckLLVM()
	// Status depends on environment — just verify structure
	if c.Name != "LLVM toolchain" {
		t.Errorf("unexpected name: %s", c.Name)
	}
	if !c.Required {
		t.Error("LLVM check should be required")
	}
	if c.Summary == "" {
		t.Error("expected non-empty summary")
	}
}

func TestDoctorCheckMuslCRT(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("musl CRT check is Linux-only")
	}
	c := doctorCheckMuslCRT()
	if c.Name != "musl CRT (Linux static linking)" {
		t.Errorf("unexpected name: %s", c.Name)
	}
}

func TestDoctorCheckBuildCache(t *testing.T) {
	c := doctorCheckBuildCache()
	if c.Name != "Build cache" {
		t.Errorf("unexpected name: %s", c.Name)
	}
	// Build cache should always be accessible
	if c.Status == "error" {
		t.Errorf("unexpected error: %s", c.Summary)
	}
}

func TestDoctorCheckPromiseHome(t *testing.T) {
	c := doctorCheckPromiseHome()
	if c.Name != "PROMISE_HOME" {
		t.Errorf("unexpected name: %s", c.Name)
	}
	if c.Summary == "" {
		t.Error("expected non-empty summary")
	}
}

func TestDoctorCheckJava(t *testing.T) {
	c := doctorCheckJava()
	if c.Name != "Java (optional — compiler development only)" {
		t.Errorf("unexpected name: %s", c.Name)
	}
	if c.Required {
		t.Error("Java check should not be required")
	}
}

func TestDoctorCheckWasmtime(t *testing.T) {
	c := doctorCheckWasmtime()
	if c.Name != "wasmtime (optional — wasm32-wasi target)" {
		t.Errorf("unexpected name: %s", c.Name)
	}
	if c.Required {
		t.Error("wasmtime check should not be required")
	}
	if c.Summary == "" {
		t.Error("expected non-empty summary")
	}
	switch c.Status {
	case "ok", "warning":
	default:
		t.Errorf("unexpected status: %s", c.Status)
	}
}

func TestDoctorCheckNode(t *testing.T) {
	c := doctorCheckNode()
	if c.Name != "node (optional — wasm32-web target tests)" {
		t.Errorf("unexpected name: %s", c.Name)
	}
	if c.Required {
		t.Error("node check should not be required")
	}
	if c.Summary == "" {
		t.Error("expected non-empty summary")
	}
	switch c.Status {
	case "ok", "warning":
	default:
		t.Errorf("unexpected status: %s", c.Status)
	}
}

func TestDoctorCheckEpochs(t *testing.T) {
	c := doctorCheckEpochs()
	if c.Name != "Epochs" {
		t.Errorf("unexpected name: %s", c.Name)
	}
	if c.Required {
		t.Error("epochs check should not be required")
	}
	if c.Summary == "" {
		t.Error("expected non-empty summary")
	}
}

func TestDoctorCheckEpochsEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMISE_HOME", dir)
	c := doctorCheckEpochs()
	if c.Status != "ok" {
		t.Errorf("expected ok with no epochs, got %s", c.Status)
	}
	if !strings.Contains(c.Summary, "No epochs installed") {
		t.Errorf("expected 'No epochs installed' summary, got %s", c.Summary)
	}
}

func TestDoctorCheckEpochsInstalled(t *testing.T) {
	dir := t.TempDir()
	epochsDir := filepath.Join(dir, "epochs")
	if err := os.MkdirAll(filepath.Join(epochsDir, "2026.0"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(epochsDir, "2026.1"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "active"), []byte("2026.1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROMISE_HOME", dir)
	c := doctorCheckEpochs()
	if c.Status != "ok" {
		t.Errorf("expected ok, got %s", c.Status)
	}
	if !strings.Contains(c.Summary, "2 installed") {
		t.Errorf("expected '2 installed' summary, got %s", c.Summary)
	}
	hasActive := false
	hasMarker := false
	for _, d := range c.Details {
		if strings.Contains(d, "Active: 2026.1") {
			hasActive = true
		}
		if strings.HasPrefix(d, "* 2026.1") {
			hasMarker = true
		}
	}
	if !hasActive {
		t.Error("expected 'Active:' detail")
	}
	if !hasMarker {
		t.Error("expected '*' marker on active epoch")
	}
}

func TestDoctorCheckPath(t *testing.T) {
	c := doctorCheckPath()
	if c.Name != "PATH" {
		t.Errorf("unexpected name: %s", c.Name)
	}
	if c.Summary == "" {
		t.Error("expected non-empty summary")
	}
}

func TestDoctorStatusString(t *testing.T) {
	tests := []struct {
		s    doctorStatus
		want string
	}{
		{doctorOK, "ok"},
		{doctorWarn, "warning"},
		{doctorErr, "error"},
	}
	for _, tt := range tests {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("doctorStatus(%d).String() = %q, want %q", tt.s, got, tt.want)
		}
	}
}

func TestDoctorCheckPromiseHomeSet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROMISE_HOME", dir)
	c := doctorCheckPromiseHome()
	if c.Status != "ok" {
		t.Errorf("expected ok for valid dir, got %s: %s", c.Status, c.Summary)
	}
	if !strings.Contains(c.Summary, "Set to:") {
		t.Error("summary should mention Set to:")
	}
}

func TestDoctorCheckPromiseHomeInvalid(t *testing.T) {
	t.Setenv("PROMISE_HOME", "/nonexistent/promise/home/path")
	c := doctorCheckPromiseHome()
	if c.Status != "warning" {
		t.Errorf("expected warning for non-existent dir, got %s", c.Status)
	}
	if len(c.Details) == 0 || !strings.Contains(c.Details[0], "does not exist") {
		t.Error("expected 'does not exist' detail")
	}
}

func TestDoctorCheckPromiseHomeNotDir(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, []byte("x"), 0644)
	t.Setenv("PROMISE_HOME", f)
	c := doctorCheckPromiseHome()
	if c.Status != "error" {
		t.Errorf("expected error for non-directory path, got %s", c.Status)
	}
}

func TestDoctorCheckPathNotOnPath(t *testing.T) {
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", "/usr/bin"+sep+"/bin")
	c := doctorCheckPath()
	if c.Status != "warning" {
		t.Errorf("expected warning when binary dir not on PATH, got %s", c.Status)
	}
	if !strings.Contains(c.Summary, "not on PATH") {
		t.Error("expected 'not on PATH' in summary")
	}
	if c.Fix == "" {
		t.Error("expected fix hint")
	}
}

func TestDoctorPrintReportWithIssues(t *testing.T) {
	report := doctorReport{
		Checks: []doctorCheck{
			{Name: "A", Status: "ok", Summary: "fine"},
			{Name: "B", Status: "error", Required: true, Summary: "broken", Fix: "fix it"},
			{Name: "C", Status: "warning", Summary: "meh"},
		},
		Errors:   1,
		Warnings: 1,
	}
	output := captureStdout(t, func() {
		printDoctorReport(report, doctorFlags{fix: true})
	})
	if !strings.Contains(output, "[✓]") {
		t.Error("missing ok icon")
	}
	if !strings.Contains(output, "[✗]") {
		t.Error("missing error icon")
	}
	if !strings.Contains(output, "[!]") {
		t.Error("missing warning icon")
	}
	if !strings.Contains(output, "1 error(s)") {
		t.Error("missing error count in summary")
	}
	if !strings.Contains(output, "1 warning(s)") {
		t.Error("missing warning count in summary")
	}
	if !strings.Contains(output, "Fix: fix it") {
		t.Error("missing fix hint")
	}
}

func TestDoctorCheckModuleCacheDefault(t *testing.T) {
	c := doctorCheckModuleCache(false)
	if c.Name != "Module cache" {
		t.Errorf("unexpected name: %s", c.Name)
	}
	if c.Summary == "" {
		t.Error("expected non-empty summary")
	}
	if len(c.Details) == 0 || !strings.Contains(c.Details[0], "Path:") {
		t.Error("expected path detail")
	}
}

func TestDoctorCheckInstallationDetails(t *testing.T) {
	c := doctorCheckInstallation()
	hasHome := false
	hasBinary := false
	for _, d := range c.Details {
		if strings.HasPrefix(d, "Home:") {
			hasHome = true
		}
		if strings.HasPrefix(d, "Binary:") {
			hasBinary = true
		}
	}
	if !hasHome {
		t.Error("expected Home detail")
	}
	if !hasBinary {
		t.Error("expected Binary detail")
	}
}

func TestDoctorCheckPathOnPath(t *testing.T) {
	// Add the test binary's directory to PATH so the "found" branch is covered.
	execPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	execDir := filepath.Dir(execPath)
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", execDir+sep+"/usr/bin"+sep+"/bin")
	c := doctorCheckPath()
	if c.Status != "ok" {
		t.Errorf("expected ok when binary dir is on PATH, got %s", c.Status)
	}
	if !strings.Contains(c.Summary, "is on PATH") {
		t.Error("expected 'is on PATH' in summary")
	}
}

func TestDoctorCheckPathOnPathCaseInsensitive(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("case-insensitive PATH match is Windows-only")
	}
	execPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	execDir := filepath.Dir(execPath)
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", strings.ToUpper(execDir)+sep+"/usr/bin"+sep+"/bin")
	c := doctorCheckPath()
	if c.Status != "ok" {
		t.Errorf("expected ok when PATH entry differs only in case, got %s", c.Status)
	}
}

func TestDoctorCheckModuleCacheNoDir(t *testing.T) {
	// Point PROMISE_HOME to a fresh temp dir with no cache/modules subdir.
	dir := t.TempDir()
	t.Setenv("PROMISE_HOME", dir)
	c := doctorCheckModuleCache(false)
	if !strings.Contains(c.Summary, "does not exist") {
		t.Errorf("expected 'does not exist' summary, got %s", c.Summary)
	}
}

func TestDoctorCheckModuleCacheNotDir(t *testing.T) {
	// Create cache/modules as a file instead of directory.
	dir := t.TempDir()
	modPath := filepath.Join(dir, "cache", "modules")
	os.MkdirAll(filepath.Join(dir, "cache"), 0755)
	os.WriteFile(modPath, []byte("x"), 0644)
	t.Setenv("PROMISE_HOME", dir)
	c := doctorCheckModuleCache(false)
	if c.Status != "error" {
		t.Errorf("expected error when module cache is a file, got %s", c.Status)
	}
	if !strings.Contains(c.Summary, "not a directory") {
		t.Errorf("expected 'not a directory' summary, got %s", c.Summary)
	}
}

func TestDoctorPrintReportNoIssues(t *testing.T) {
	report := doctorReport{
		Checks: []doctorCheck{
			{Name: "A", Status: "ok", Summary: "all good"},
		},
	}
	output := captureStdout(t, func() {
		printDoctorReport(report, doctorFlags{})
	})
	if !strings.Contains(output, "No issues found.") {
		t.Error("expected 'No issues found.' in output")
	}
}

func TestDoctorPrintReportErrorsOnly(t *testing.T) {
	report := doctorReport{
		Checks: []doctorCheck{
			{Name: "A", Status: "error", Required: true, Summary: "bad"},
		},
		Errors: 1,
	}
	output := captureStdout(t, func() {
		printDoctorReport(report, doctorFlags{})
	})
	if !strings.Contains(output, "1 error(s)") {
		t.Error("expected error count")
	}
	if strings.Contains(output, "warning(s)") {
		t.Error("should not mention warnings when there are none")
	}
}

func TestDoctorRunNetworkFlag(t *testing.T) {
	// Cover the -network flag parsing branch in runDoctor.
	// The `-network` arg is parsed and forwarded to doctorCheckModuleCache.
	output := captureStdout(t, func() {
		runDoctor([]string{"-network"})
	})
	if !strings.Contains(output, "Module cache") {
		t.Error("missing module cache check in output")
	}
	if !strings.Contains(output, "Network:") {
		t.Error("expected Network: detail when -network is passed")
	}
}

func TestDoctorCheckJavaMissing(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")
	c := doctorCheckJava()
	if c.Status != "warning" {
		t.Errorf("expected warning when java not on PATH, got %s", c.Status)
	}
	if !strings.Contains(c.Summary, "Not found") {
		t.Errorf("expected 'Not found' in summary, got %s", c.Summary)
	}
	if c.Fix == "" {
		t.Error("expected fix hint when java is missing")
	}
}

func TestDoctorCheckWasmtimeMissing(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")
	c := doctorCheckWasmtime()
	if c.Status != "warning" {
		t.Errorf("expected warning when wasmtime not on PATH, got %s", c.Status)
	}
	if !strings.Contains(c.Summary, "Not found") {
		t.Errorf("expected 'Not found' in summary, got %s", c.Summary)
	}
	if c.Fix == "" {
		t.Error("expected fix hint when wasmtime is missing")
	}
}

func TestDoctorCheckNodeMissing(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")
	c := doctorCheckNode()
	if c.Status != "warning" {
		t.Errorf("expected warning when node not on PATH, got %s", c.Status)
	}
	if !strings.Contains(c.Summary, "Not found") {
		t.Errorf("expected 'Not found' in summary, got %s", c.Summary)
	}
	if c.Fix == "" {
		t.Error("expected fix hint when node is missing")
	}
}

func TestDoctorCheckEpochsError(t *testing.T) {
	// Make <PROMISE_HOME>/epochs a file so InstalledEpochs ReadDir fails
	// with a non-IsNotExist error, exercising the warn path.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "epochs"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROMISE_HOME", dir)
	c := doctorCheckEpochs()
	if c.Status != "warning" {
		t.Errorf("expected warning when epochs dir is unreadable, got %s: %s", c.Status, c.Summary)
	}
	if !strings.Contains(c.Summary, "Cannot list installed epochs") {
		t.Errorf("expected 'Cannot list' summary, got %s", c.Summary)
	}
	if c.Fix == "" {
		t.Error("expected fix hint on epochs error")
	}
}

func TestDoctorCheckBuildCacheError(t *testing.T) {
	// Set PROMISE_HOME to a regular file so MkdirAll inside BuildCacheDir
	// fails with "not a directory", exercising the warn path.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROMISE_HOME", f)
	c := doctorCheckBuildCache()
	if c.Status != "warning" {
		t.Errorf("expected warning when build cache cannot be created, got %s: %s", c.Status, c.Summary)
	}
	if !strings.Contains(c.Summary, "Cannot access build cache") {
		t.Errorf("expected 'Cannot access' summary, got %s", c.Summary)
	}
	if c.Fix == "" {
		t.Error("expected fix hint on build cache error")
	}
}

func TestDoctorCheckModuleCacheNetworkUnreachable(t *testing.T) {
	// Clear PATH so `git` cannot be located; cmd.Run then fails immediately
	// without making real network calls. Exercises the unreachable branch.
	t.Setenv("PATH", "/nonexistent")
	dir := t.TempDir()
	t.Setenv("PROMISE_HOME", dir)
	c := doctorCheckModuleCache(true)
	hasUnreachable := false
	for _, d := range c.Details {
		if strings.Contains(d, "git host unreachable") {
			hasUnreachable = true
			break
		}
	}
	if !hasUnreachable {
		t.Errorf("expected 'git host unreachable' detail, got details: %v", c.Details)
	}
}

// seedCASBlob writes content into <home>/cache/blobs/sha256/<hash> with the
// given content address; mismatch=true writes a corrupt entry (filename is the
// address of `content` but the bytes on disk differ).
func seedCASBlob(t *testing.T, home, content string, mismatch bool) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	dir := filepath.Join(home, "cache", "blobs", "sha256")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := content
	if mismatch {
		body = content + "-tampered"
	}
	if err := os.WriteFile(filepath.Join(dir, hash), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestDoctorCheckCASClean(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PROMISE_HOME", home)
	seedCASBlob(t, home, "intact", false)

	c := doctorCheckCAS(doctorFlags{})
	if c.Status != "ok" {
		t.Errorf("expected ok for a clean CAS, got %s (%s)", c.Status, c.Summary)
	}
}

func TestDoctorCheckCASEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PROMISE_HOME", home)

	c := doctorCheckCAS(doctorFlags{})
	if c.Status != "ok" {
		t.Errorf("expected ok for an empty CAS, got %s (%s)", c.Status, c.Summary)
	}
	if c.Summary != "No cached dependencies" {
		t.Errorf("expected 'No cached dependencies' summary, got %q", c.Summary)
	}
}

func TestDoctorCheckCASCorruptFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PROMISE_HOME", home)
	seedCASBlob(t, home, "good", false)
	seedCASBlob(t, home, "bad", true)

	c := doctorCheckCAS(doctorFlags{})
	if c.Status != "error" {
		t.Errorf("expected error for a corrupt CAS, got %s", c.Status)
	}
	if !c.Required {
		t.Error("corrupt CAS check must be Required (CI preflight exit 1)")
	}
	if c.Fix == "" {
		t.Error("expected a fix hint pointing at --repair")
	}
}

func TestDoctorCheckCASRepairQuarantines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PROMISE_HOME", home)
	seedCASBlob(t, home, "good", false)
	badHash := seedCASBlob(t, home, "bad", true)

	c := doctorCheckCAS(doctorFlags{repair: true})
	if c.Status != "warning" {
		t.Errorf("expected warning after repair, got %s (%s)", c.Status, c.Summary)
	}
	// The corrupt entry must be gone from the live CAS.
	if _, err := os.Stat(filepath.Join(home, "cache", "blobs", "sha256", badHash)); !os.IsNotExist(err) {
		t.Error("corrupt entry should be quarantined out of the live CAS")
	}
	// Its bytes must be preserved in quarantine.
	if _, err := os.Stat(filepath.Join(home, "cache", "quarantine", "blob", badHash+".0")); err != nil {
		t.Errorf("quarantined bytes not preserved: %v", err)
	}
}

// writeCASEpochRefs writes epochs/<epoch>/blobs.refs so LiveSet can root the
// union sweep on it (T1009).
func writeCASEpochRefs(t *testing.T, home, epoch string, lines ...string) {
	t.Helper()
	dir := filepath.Join(home, "epochs", epoch)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "blobs.refs"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// doctor --repair folds in the union-rooted sweep + staging-residue reap that
// used to live under `promise gc` (T1009): an orphan blob (referenced by no
// installed epoch) and staging residue are reclaimed, while a referenced blob
// survives. Plain doctor stays read-only.
func TestDoctorCheckCASRepairSweepsOrphans(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PROMISE_HOME", home)
	referenced := seedCASBlob(t, home, "referenced-by-epoch", false)
	orphan := seedCASBlob(t, home, "referenced-by-nobody", false)
	writeCASEpochRefs(t, home, "2026.0", "blob "+referenced)
	// Staging residue from a crashed install.
	residue := filepath.Join(home, "cache", "blobs", "sha256", ".stage-1.tmp")
	if err := os.WriteFile(residue, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Plain doctor: read-only, nothing removed.
	doctorCheckCAS(doctorFlags{})
	if _, err := os.Stat(filepath.Join(home, "cache", "blobs", "sha256", orphan)); err != nil {
		t.Fatalf("plain doctor removed the orphan blob: %v", err)
	}
	if _, err := os.Stat(residue); err != nil {
		t.Fatalf("plain doctor removed staging residue: %v", err)
	}

	// Repair: orphan + residue reclaimed, referenced blob survives.
	doctorCheckCAS(doctorFlags{repair: true})
	if _, err := os.Stat(filepath.Join(home, "cache", "blobs", "sha256", orphan)); !os.IsNotExist(err) {
		t.Error("repair should have swept the orphan blob")
	}
	if _, err := os.Stat(residue); !os.IsNotExist(err) {
		t.Error("repair should have cleared staging residue")
	}
	if _, err := os.Stat(filepath.Join(home, "cache", "blobs", "sha256", referenced)); err != nil {
		t.Errorf("repair removed an epoch-referenced blob: %v", err)
	}
}

// containsSubstr reports whether any detail line contains sub.
func detailsContain(details []string, sub string) bool {
	for _, d := range details {
		if strings.Contains(d, sub) {
			return true
		}
	}
	return false
}

// The §4.4 over-deletion fail-safe must survive the move under doctor --repair
// (T1009): when an installed epoch's blobs.refs is unreadable, LiveSet reports
// allRefsReadable=false and the sweep must keep EVERY blob — including one that
// no readable epoch references — rather than wedge that epoch's offline build.
func TestDoctorCheckCASRepairFailSafeKeepsAll(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PROMISE_HOME", home)
	orphan := seedCASBlob(t, home, "would-be-orphan", false)
	// An installed epoch whose blobs.refs is unreadable (a directory, not a file)
	// → ReadEpochRefs fails → allRefsReadable=false → fail-safe engages.
	refsDir := filepath.Join(home, "epochs", "2026.0", "blobs.refs")
	if err := os.MkdirAll(refsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	c := doctorCheckCAS(doctorFlags{repair: true})
	if _, err := os.Stat(filepath.Join(home, "cache", "blobs", "sha256", orphan)); err != nil {
		t.Errorf("fail-safe should have kept the blob despite no readable ref set: %v", err)
	}
	if !detailsContain(c.Details, "Kept all blobs") {
		t.Errorf("expected a fail-safe detail line, got %v", c.Details)
	}
}

// When the live set can't be computed at all (here: the epochs path is a plain
// file, so InstalledEpochs errors), doctor --repair must NOT delete anything —
// it reports the skip and leaves the orphan blob in place (T1009).
func TestDoctorCheckCASRepairSkipsOnLiveSetError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PROMISE_HOME", home)
	orphan := seedCASBlob(t, home, "kept-because-liveset-errored", false)
	// A regular file where the epochs directory is expected → InstalledEpochs
	// returns "not a directory" → LiveSet propagates the error.
	if err := os.WriteFile(filepath.Join(home, "epochs"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := doctorCheckCAS(doctorFlags{repair: true})
	if _, err := os.Stat(filepath.Join(home, "cache", "blobs", "sha256", orphan)); err != nil {
		t.Errorf("repair must keep blobs when the live set can't be computed: %v", err)
	}
	if !detailsContain(c.Details, "Cache reclamation skipped") {
		t.Errorf("expected a skip detail line, got %v", c.Details)
	}
}
