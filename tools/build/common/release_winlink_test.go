package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// T0772: tests for `bin/release winlink`, the generator that turns the
// license-clean .def symbol lists into MSVC-ABI import libraries via
// llvm-dlltool. The .def is the source of truth; the .lib is a reproducible
// build artifact.

// writeTestDef writes a minimal valid .def declaring one export for dll.
func writeTestDef(t *testing.T, dir, base, dll, export string) {
	t.Helper()
	content := "LIBRARY " + dll + "\nEXPORTS\n" + export + "\n"
	if err := os.WriteFile(filepath.Join(dir, base+".def"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunReleaseWinlinkGenerates(t *testing.T) {
	if Which("llvm-dlltool") == "" {
		t.Skip("llvm-dlltool not on PATH")
	}
	root := t.TempDir()
	defDir := filepath.Join(root, "def")
	outDir := filepath.Join(root, "out")
	if err := os.MkdirAll(defDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestDef(t, defDir, "kernel32", "kernel32.dll", "ExitProcess")
	writeTestDef(t, defDir, "advapi32", "advapi32.dll", "GetUserNameA")

	if err := runReleaseWinlink(root, []string{"--def-dir", defDir, "--out", outDir}); err != nil {
		t.Fatalf("runReleaseWinlink: %v", err)
	}

	for _, name := range []string{"kernel32.lib", "advapi32.lib"} {
		path := filepath.Join(outDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("generated lib %s missing: %v", name, err)
		}
		// An import lib is an ar archive — must start with the ar magic.
		if !strings.HasPrefix(string(data), "!<arch>\n") {
			t.Errorf("%s is not a valid ar archive (bad magic)", name)
		}
		if len(data) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
}

func TestRunReleaseWinlinkMissingTool(t *testing.T) {
	root := t.TempDir()
	defDir := filepath.Join(root, "def")
	if err := os.MkdirAll(defDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestDef(t, defDir, "kernel32", "kernel32.dll", "ExitProcess")
	// Point at a nonexistent tool so the "tool not found / failed" path runs.
	err := runReleaseWinlink(root, []string{
		"--llvm-dlltool", filepath.Join(root, "does-not-exist-dlltool"),
		"--def-dir", defDir,
		"--out", filepath.Join(root, "out"),
	})
	if err == nil {
		t.Fatal("expected error when llvm-dlltool path is invalid")
	}
}

func TestRunReleaseWinlinkNoDefs(t *testing.T) {
	if Which("llvm-dlltool") == "" {
		t.Skip("llvm-dlltool not on PATH")
	}
	root := t.TempDir()
	emptyDef := filepath.Join(root, "def")
	if err := os.MkdirAll(emptyDef, 0o755); err != nil {
		t.Fatal(err)
	}
	err := runReleaseWinlink(root, []string{"--def-dir", emptyDef, "--out", filepath.Join(root, "out")})
	if err == nil {
		t.Fatal("expected error when no .def files are present")
	}
	if !strings.Contains(err.Error(), "no .def files") {
		t.Errorf("error = %v, want 'no .def files'", err)
	}
}

func TestRunReleaseWinlinkBadFlag(t *testing.T) {
	if err := runReleaseWinlink(t.TempDir(), []string{"--nonexistent-flag"}); err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

// TestReleaseWinlinkSubcommandWiring verifies `winlink` is reachable through the
// RunRelease dispatcher (a bad flag is enough to prove it routed there rather
// than erroring as an unknown subcommand).
func TestReleaseWinlinkSubcommandWiring(t *testing.T) {
	err := RunRelease(t.TempDir(), []string{"winlink", "--nonexistent-flag"})
	if err == nil {
		t.Fatal("expected error from winlink subcommand")
	}
	if strings.Contains(err.Error(), "unknown") {
		t.Errorf("winlink subcommand not wired into RunRelease: %v", err)
	}
}
