package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFindRepoRoot_FromWorkingDir verifies that findRepoRoot succeeds when
// invoked from a cwd inside the repo (the normal case). In `go test`, the
// test binary lives under /tmp/go-build/* so os.Executable()-based lookup
// won't find .git; the cwd fallback kicks in and succeeds because the
// test process inherits cwd from the repo.
func TestFindRepoRoot_FromWorkingDir(t *testing.T) {
	got, err := findRepoRoot()
	if err != nil {
		t.Fatalf("findRepoRoot: %v", err)
	}
	if got == "" {
		t.Fatalf("empty root")
	}
	if _, err := os.Stat(filepath.Join(got, ".git")); err != nil {
		t.Errorf("returned root %q does not contain .git: %v", got, err)
	}
}

// TestGuardBinary_EditGateFromOutsideRepo verifies the B0349 regression fix:
// the installed bin/guard must successfully load edit gates even when invoked
// with a cwd outside any git worktree. This exercises the real os.Executable()
// path (the guard binary lives at <root>/bin/guard, whose parent has .git),
// not the `go test` binary path.
func TestGuardBinary_EditGateFromOutsideRepo(t *testing.T) {
	// Locate the installed bin/guard relative to the test's cwd (the package
	// dir is <root>/tools/build/cmd/guard; <root>/bin/guard is 4 levels up).
	pkgDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := pkgDir
	for range 4 {
		root = filepath.Dir(root)
	}
	guardBin := filepath.Join(root, "bin", "guard")
	if _, err := os.Stat(guardBin); err != nil {
		t.Skipf("bin/guard not built (run ./make): %v", err)
	}

	// Invoke the real binary from a cwd outside any git worktree with an
	// Edit-shaped hook payload. Before B0349, guard would fail-closed with
	// "not inside a git repository" because loadEditGates walked cwd.
	outside := t.TempDir()
	payload := `{"tool_name":"Edit","tool_input":{"file_path":"/tmp/x.go","old_string":"a","new_string":"b"}}`

	cmd := exec.Command(guardBin)
	cmd.Dir = outside
	cmd.Stdin = strings.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("guard exited with error from cwd=%s: %v\nstdout: %s\nstderr: %s",
			outside, err, stdout.String(), stderr.String())
	}

	out := stdout.String()
	if strings.Contains(out, "not inside a git repository") {
		t.Errorf("guard failed to locate repo root from outside cwd: %s", out)
	}
	if strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("guard denied a benign edit from outside cwd: %s", out)
	}
}
