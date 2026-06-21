package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runPackage dispatch (T1007): consolidates dependency management under
// `promise package`. The old top-level verbs (pkg, add, search, pin) are kept
// as hidden deprecated aliases for one release.

// TestRunPackageNoArgs: `promise package` is a pure group — a bare invocation
// lists its subcommands to stdout and exits 0 (T1006).
func TestRunPackageNoArgs(t *testing.T) {
	out := captureStdout(t, func() {
		captureStderr(func() {
			runPackage(nil)
		})
	})
	if !strings.Contains(out, "Usage: promise package <subcommand>") {
		t.Errorf("expected package subcommand listing on stdout, got: %s", out)
	}
	for _, sub := range []string{"add", "remove", "update", "search", "pin", "check-upgrade"} {
		if !strings.Contains(out, sub) {
			t.Errorf("package listing missing subcommand %q, got: %s", sub, out)
		}
	}
}

// TestRunPackageUnknownSubcommand: an unrecognized subcommand prints an error and
// exits 1 (does not silently fall through to dependency updating).
func TestRunPackageUnknownSubcommand(t *testing.T) {
	if os.Getenv("TEST_PACKAGE_UNKNOWN") == "1" {
		runPackage([]string{"bogus"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunPackageUnknownSubcommand")
	cmd.Env = append(os.Environ(), "TEST_PACKAGE_UNKNOWN=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for unknown package subcommand")
	}
	s := string(out)
	if !strings.Contains(s, "unknown package subcommand: bogus") {
		t.Errorf("expected unknown-subcommand error, got: %s", s)
	}
}

// TestRunPackageUpdateDispatch: `package update` routes to runPkgUpdate. With a
// promise.toml that has no [require] entries, runPkgUpdate reports that and
// returns normally (no exit), so this exercises the dispatch in-process.
func TestRunPackageUpdateDispatch(t *testing.T) {
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
			runPackage([]string{"update"})
		})
	})
	if !strings.Contains(out, "No [require] entries") {
		t.Errorf("expected `package update` to route to runPkgUpdate, got: %s", out)
	}
}

// TestRunPackageRemove: `promise package remove <url>` removes an entry from
// promise.toml [require] and prints "Removed <url>".
func TestRunPackageRemove(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	toml := `[module]
name = "test"
epoch = "2026.0"

[require]
"github.com/example/foo" = "abc123"
`
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"), []byte(toml), 0644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		captureStderr(func() {
			runPackageRemove([]string{"github.com/example/foo"})
		})
	})
	if !strings.Contains(out, "Removed") {
		t.Errorf("expected 'Removed' message, got: %s", out)
	}

	// Verify the entry is gone from promise.toml
	data, err := os.ReadFile(filepath.Join(dir, "promise.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "github.com/example/foo") {
		t.Errorf("expected entry to be removed from promise.toml, got:\n%s", data)
	}
}

// TestRunPackageRemoveMissing: `promise package remove` with an unknown URL exits 1.
func TestRunPackageRemoveMissing(t *testing.T) {
	if os.Getenv("TEST_PACKAGE_REMOVE_MISSING") == "1" {
		dir := t.TempDir()
		if err := os.Chdir(dir); err != nil {
			os.Exit(1)
		}
		if err := os.WriteFile(filepath.Join(dir, "promise.toml"),
			[]byte("[module]\nname = \"test\"\nepoch = \"2026.0\"\n"), 0644); err != nil {
			os.Exit(1)
		}
		runPackageRemove([]string{"github.com/nobody/nothing"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunPackageRemoveMissing")
	cmd.Env = append(os.Environ(), "TEST_PACKAGE_REMOVE_MISSING=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for remove of missing URL")
	}
	if !strings.Contains(string(out), "no [require] entry") {
		t.Errorf("expected 'no [require] entry' message, got: %s", string(out))
	}
}

// TestRunPackageSearchDispatch: `promise package search <kw>` routes to runSearch
// in-process (search just scans the embedded catalog, no network required).
func TestRunPackageSearchDispatch(t *testing.T) {
	out := captureStdout(t, func() {
		captureStderr(func() {
			runPackage([]string{"search", "json"})
		})
	})
	// The embedded catalog has a json entry (or no match is acceptable); either
	// way we should not crash and the output should be non-empty.
	_ = out // dispatch exercised; no-crash is the invariant
}

// TestRunPackageRemoveDispatch: `promise package remove <url>` routed through
// runPackage dispatch (exercising the case "remove" branch).
func TestRunPackageRemoveDispatch(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	toml := "[module]\nname = \"test\"\nepoch = \"2026.0\"\n\n[require]\n\"github.com/example/bar\" = \"dead0000\"\n"
	if err := os.WriteFile(filepath.Join(dir, "promise.toml"), []byte(toml), 0644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		captureStderr(func() {
			runPackage([]string{"remove", "github.com/example/bar"})
		})
	})
	if !strings.Contains(out, "Removed") {
		t.Errorf("expected 'Removed' message via runPackage dispatch, got: %s", out)
	}
}

// TestRunPackageAddDispatch: `promise package add` with no URL exits 1 and
// prints the add usage message — exercises the case "add" branch in runPackage.
func TestRunPackageAddDispatch(t *testing.T) {
	if os.Getenv("TEST_PACKAGE_ADD_DISPATCH") == "1" {
		runPackage([]string{"add"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunPackageAddDispatch")
	cmd.Env = append(os.Environ(), "TEST_PACKAGE_ADD_DISPATCH=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for `package add` with no args")
	}
	if !strings.Contains(string(out), "usage: promise package add") {
		t.Errorf("expected add usage message, got: %s", string(out))
	}
}

// TestRunPackagePinDispatch: `promise package pin` with no URL exits 1 and
// prints the pin usage message — exercises the case "pin" branch in runPackage.
func TestRunPackagePinDispatch(t *testing.T) {
	if os.Getenv("TEST_PACKAGE_PIN_DISPATCH") == "1" {
		runPackage([]string{"pin"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunPackagePinDispatch")
	cmd.Env = append(os.Environ(), "TEST_PACKAGE_PIN_DISPATCH=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for `package pin` with no args")
	}
	if !strings.Contains(string(out), "usage: promise package pin") {
		t.Errorf("expected pin usage message, got: %s", string(out))
	}
}

// TestRunPackageRemoveUsageError: `promise package remove` with no args exits 1.
func TestRunPackageRemoveUsageError(t *testing.T) {
	if os.Getenv("TEST_PACKAGE_REMOVE_USAGE") == "1" {
		runPackageRemove(nil)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunPackageRemoveUsageError")
	cmd.Env = append(os.Environ(), "TEST_PACKAGE_REMOVE_USAGE=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for `package remove` with no args")
	}
	if !strings.Contains(string(out), "usage: promise package remove") {
		t.Errorf("expected remove usage message, got: %s", string(out))
	}
}

// TestRunPackageRemoveNoToml: `promise package remove` in a directory without
// promise.toml exits 1 with a clear error.
func TestRunPackageRemoveNoToml(t *testing.T) {
	if os.Getenv("TEST_PACKAGE_REMOVE_NO_TOML") == "1" {
		dir := t.TempDir()
		if err := os.Chdir(dir); err != nil {
			os.Exit(1)
		}
		runPackageRemove([]string{"github.com/nobody/nothing"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunPackageRemoveNoToml")
	cmd.Env = append(os.Environ(), "TEST_PACKAGE_REMOVE_NO_TOML=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit when no promise.toml exists")
	}
	if !strings.Contains(string(out), "no promise.toml") {
		t.Errorf("expected 'no promise.toml' message, got: %s", string(out))
	}
}

// TestRunLegacySearchAlias: deprecated `promise search <kw>` alias routes to
// runSearch in-process (no exit, no network).
func TestRunLegacySearchAlias(t *testing.T) {
	out := captureStdout(t, func() {
		captureStderr(func() {
			runLegacyPackageAlias("search", []string{"io"})
		})
	})
	_ = out // dispatch exercised; no-crash is the invariant
}

// TestRunLegacyPkgUpdateAlias: deprecated `promise pkg update` alias routes to
// runPkgUpdate. With a promise.toml that has no [require] entries, runPkgUpdate
// reports that and returns normally.
func TestRunLegacyPkgUpdateAlias(t *testing.T) {
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
			runLegacyPackageAlias("pkg", []string{"update"})
		})
	})
	if !strings.Contains(out, "No [require] entries") {
		t.Errorf("expected pkg update alias to route to runPkgUpdate, got: %s", out)
	}
}

// TestRunLegacyPkgSubcommandAlias: deprecated `promise pkg <other>` (non-update
// subcommand) routes to runPackage and exits 1 with an unknown-subcommand error.
func TestRunLegacyPkgSubcommandAlias(t *testing.T) {
	if os.Getenv("TEST_LEGACY_PKG_SUBCMD") == "1" {
		runLegacyPackageAlias("pkg", []string{"bogus"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunLegacyPkgSubcommandAlias")
	cmd.Env = append(os.Environ(), "TEST_LEGACY_PKG_SUBCMD=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for deprecated pkg with unknown subcommand")
	}
	if !strings.Contains(string(out), "unknown package subcommand") {
		t.Errorf("expected unknown-subcommand error, got: %s", string(out))
	}
}

// TestRunLegacyAddAlias: deprecated `promise add` alias with no args exits 1
// with the add usage message — exercises the "add" case in runLegacyPackageAlias.
func TestRunLegacyAddAlias(t *testing.T) {
	if os.Getenv("TEST_LEGACY_ADD") == "1" {
		runLegacyPackageAlias("add", []string{})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunLegacyAddAlias")
	cmd.Env = append(os.Environ(), "TEST_LEGACY_ADD=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for deprecated add with no args")
	}
	if !strings.Contains(string(out), "usage: promise package add") {
		t.Errorf("expected add usage message, got: %s", string(out))
	}
}

// TestRunLegacyPinAlias: deprecated `promise pin` alias with no args exits 1
// with the pin usage message — exercises the "pin" case in runLegacyPackageAlias.
func TestRunLegacyPinAlias(t *testing.T) {
	if os.Getenv("TEST_LEGACY_PIN") == "1" {
		runLegacyPackageAlias("pin", []string{})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunLegacyPinAlias")
	cmd.Env = append(os.Environ(), "TEST_LEGACY_PIN=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for deprecated pin with no args")
	}
	if !strings.Contains(string(out), "usage: promise package pin") {
		t.Errorf("expected pin usage message, got: %s", string(out))
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
