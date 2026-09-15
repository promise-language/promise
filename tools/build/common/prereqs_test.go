package common

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRunPrereqs_LLVMNeverSuggestsASystemInstall pins what `bin/prereqs` says
// when the pinned toolchain is unavailable. It used to answer "brew install
// llvm" / "apt-get install llvm-22" / "download from github", which is now a
// wrong answer twice over: installing one changes nothing (the host is never
// searched), and following the advice leaves the reader believing their build is
// pinned when it is not (T2108).
func TestRunPrereqs_LLVMNeverSuggestsASystemInstall(t *testing.T) {
	clearToolchainOverrides(t)

	// root == "" → FindLLVM has no prebuilts manifest to read and no override,
	// so the LLVM line takes its unavailable branch without any network access.
	out := captureStdout(t, func() {
		if err := RunPrereqs("", nil); err != nil {
			t.Errorf("RunPrereqs reports, it does not fail: %v", err)
		}
	})

	if !strings.Contains(out, "llvm:") {
		t.Fatalf("expected an llvm line in the report:\n%s", out)
	}
	for _, unwanted := range []string{
		"brew install llvm",
		"apt-get install llvm",
		"apt install llvm",
		"llvm/llvm-project/releases",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("prereqs must not offer a system LLVM (%q):\n%s", unwanted, out)
		}
	}
	// The line that IS printed must point at the pinned toolchain instead.
	if !strings.Contains(out, "NOT AVAILABLE") || !strings.Contains(out, "pinned") {
		t.Errorf("the unavailable line should name the pinned toolchain:\n%s", out)
	}
}

// TestRunPrereqs_ReportsOverriddenToolchain: with a full override in effect the
// report shows the operator's binaries, not a pinned path it is not using.
func TestRunPrereqs_ReportsOverriddenToolchain(t *testing.T) {
	clearToolchainOverrides(t)
	dir := stubLLVMDir(t)
	opt := filepath.Join(dir, "opt"+ExeSuffix())
	lld := filepath.Join(dir, "lld"+ExeSuffix())
	t.Setenv("PROMISE_OPT", opt)
	t.Setenv(linkerOverrideVar(), lld)

	out := captureStdout(t, func() {
		if err := RunPrereqs("", nil); err != nil {
			t.Errorf("RunPrereqs: %v", err)
		}
	})
	if !strings.Contains(out, opt) {
		t.Errorf("report should name the overridden opt %q:\n%s", opt, out)
	}
	if !strings.Contains(out, lld) {
		t.Errorf("report should name the overridden linker %q:\n%s", lld, out)
	}
}
