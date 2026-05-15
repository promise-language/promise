package main

import (
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
	if len(report.Checks) < 5 {
		t.Errorf("expected at least 5 checks, got %d", len(report.Checks))
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
	output := captureStdout(t, func() {
		runDoctor([]string{"-fix"})
	})

	// Java is typically missing in CI — the fix hint should appear with -fix
	if strings.Contains(output, "Java") && strings.Contains(output, "[!]") {
		if !strings.Contains(output, "Fix:") {
			t.Error("expected Fix hint with -fix flag for missing Java")
		}
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
