package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runPkg dispatch (T0770): the package-manager namespace that now hosts the
// dependency [require]-pin updater moved out of bare `update`. Bare `update`
// is toolchain self-update (update.go); `pkg update` is the old behavior.

// TestRunPkgNoArgs: `promise pkg` with no subcommand prints usage and exits 1.
func TestRunPkgNoArgs(t *testing.T) {
	if os.Getenv("TEST_PKG_NO_ARGS") == "1" {
		runPkg(nil)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunPkgNoArgs")
	cmd.Env = append(os.Environ(), "TEST_PKG_NO_ARGS=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for `pkg` with no subcommand")
	}
	if !strings.Contains(string(out), "usage: promise pkg") {
		t.Errorf("expected pkg usage message, got: %s", string(out))
	}
}

// TestRunPkgUnknownSubcommand: an unrecognized subcommand prints an error and
// exits 1 (does not silently fall through to dependency updating).
func TestRunPkgUnknownSubcommand(t *testing.T) {
	if os.Getenv("TEST_PKG_UNKNOWN") == "1" {
		runPkg([]string{"bogus"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunPkgUnknownSubcommand")
	cmd.Env = append(os.Environ(), "TEST_PKG_UNKNOWN=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for unknown pkg subcommand")
	}
	s := string(out)
	if !strings.Contains(s, "unknown pkg subcommand: bogus") {
		t.Errorf("expected unknown-subcommand error, got: %s", s)
	}
}

// TestRunPkgUpdateDispatch: `pkg update` routes to runPkgUpdate. With a
// promise.toml that has no [require] entries, runPkgUpdate reports that and
// returns normally (no exit), so this exercises the dispatch in-process.
func TestRunPkgUpdateDispatch(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
		[]byte("[module]\nname = \"test\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		captureStderr(func() {
			runPkg([]string{"update"})
		})
	})
	if !strings.Contains(out, "No [require] entries") {
		t.Errorf("expected `pkg update` to route to runPkgUpdate, got: %s", out)
	}
}

// TestRunUpdateRejectsEpochArg: `promise update` (toolchain self-update) no
// longer takes an epoch argument (T0825) — a specific epoch is now
// `promise use <epoch>`. A stray positional arg is a usage error that points at
// `promise use` and exits 1.
func TestRunUpdateRejectsEpochArg(t *testing.T) {
	if os.Getenv("TEST_TOOLCHAIN_UPDATE_USAGE") == "1" {
		runUpdate([]string{"2026.0"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunUpdateRejectsEpochArg")
	cmd.Env = append(os.Environ(), "TEST_TOOLCHAIN_UPDATE_USAGE=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for `update` with an epoch arg")
	}
	s := string(out)
	if !strings.Contains(s, "no longer takes an epoch argument") {
		t.Errorf("expected epoch-arg rejection message, got: %s", s)
	}
	if !strings.Contains(s, "promise use <epoch>") {
		t.Errorf("expected pointer to `promise use`, got: %s", s)
	}
}
