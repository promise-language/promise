package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/module"
)

func TestDirSize(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	// Write two 100-byte files.
	os.WriteFile(filepath.Join(tmp, "a.txt"), make([]byte, 100), 0644)
	os.MkdirAll(filepath.Join(tmp, "sub"), 0755)
	os.WriteFile(filepath.Join(tmp, "sub", "b.txt"), make([]byte, 200), 0644)

	size := dirSize(tmp)
	if size != 300 {
		t.Fatalf("expected 300, got %d", size)
	}
}

func TestPrintVersionWithLdflags(t *testing.T) {
	// When version is set via -ldflags, printVersion uses it. With no channel
	// file and no commit stamp, the line carries only the (stable) channel (T1101).
	t.Setenv("PROMISE_HOME", t.TempDir())
	oldV, oldC := version, commit
	version = "2026.0-abc1234"
	commit = ""
	defer func() { version = oldV; commit = oldC }()

	output := captureStdout(t, printVersion)
	if output != "promise version 2026.0-abc1234 (channel stable)\n" {
		t.Fatalf("expected 'promise version 2026.0-abc1234 (channel stable)\\n', got %q", output)
	}
}

func TestPrintVersionWithCommit(t *testing.T) {
	// On stable channel, commit SHA is suppressed even when the binary was built
	// with one — epoch version string is the stable identity (T1127).
	t.Setenv("PROMISE_HOME", t.TempDir())
	oldV, oldC := version, commit
	version = "2026.0"
	commit = "0123456789abcdef0123456789abcdef01234567"
	defer func() { version = oldV; commit = oldC }()

	output := captureStdout(t, printVersion)
	want := "promise version 2026.0 (channel stable)\n"
	if output != want {
		t.Fatalf("expected %q, got %q", want, output)
	}
}

func TestPrintVersionNextChannelBuild(t *testing.T) {
	// On the next channel, printVersion surfaces the recorded build-id — the
	// same identity `update check` compares against — shortened and labeled
	// "build <sha7>" so it lines up with update check (T1101).
	t.Setenv("PROMISE_HOME", t.TempDir())
	if err := module.WriteUpdateChannel(module.ChannelNext); err != nil {
		t.Fatalf("WriteUpdateChannel: %v", err)
	}
	const buildID = "ea91ebde5f6cc303e472b2fb6d6bf15938f741c5208636822aaf75ebf046f3c9"
	if err := module.WriteEpochBuildID(module.ChannelNext, buildID); err != nil {
		t.Fatalf("WriteEpochBuildID: %v", err)
	}
	oldV, oldC := version, commit
	version = "2026.1"
	commit = "0123456789abcdef0123456789abcdef01234567"
	defer func() { version = oldV; commit = oldC }()

	output := captureStdout(t, printVersion)
	want := "promise version 2026.1 (channel next, commit 0123456, build ea91ebd)\n"
	if output != want {
		t.Fatalf("expected %q, got %q", want, output)
	}
}

func TestPrintVersionNextChannelNoBuild(t *testing.T) {
	// On the next channel before any build has been downloaded, no build-id is
	// recorded — ReadEpochBuildID errors and the build segment is omitted (rather
	// than printing an empty/garbage hash). The channel is still surfaced (T1101).
	t.Setenv("PROMISE_HOME", t.TempDir())
	if err := module.WriteUpdateChannel(module.ChannelNext); err != nil {
		t.Fatalf("WriteUpdateChannel: %v", err)
	}
	oldV, oldC := version, commit
	version = "2026.1"
	commit = "0123456789abcdef0123456789abcdef01234567"
	defer func() { version = oldV; commit = oldC }()

	info := gatherVersionInfo()
	if info.Channel != module.ChannelNext {
		t.Fatalf("expected channel %q, got %q", module.ChannelNext, info.Channel)
	}
	if info.Build != "" {
		t.Fatalf("expected empty build when no build-id recorded, got %q", info.Build)
	}

	output := captureStdout(t, printVersion)
	want := "promise version 2026.1 (channel next, commit 0123456)\n"
	if output != want {
		t.Fatalf("expected %q, got %q", want, output)
	}
}

func TestGatherVersionInfoChannelUnreadable(t *testing.T) {
	// gatherVersionInfo tolerates an unreadable channel: `promise version` must
	// never fail just because PROMISE_HOME is broken. With the channel path made
	// unreadable (a directory, not a file), UpdateChannel errors and the channel
	// falls back to stable — version reporting still succeeds (T1101).
	home := t.TempDir()
	t.Setenv("PROMISE_HOME", home)
	if err := os.Mkdir(filepath.Join(home, "channel"), 0755); err != nil {
		t.Fatalf("mkdir channel dir: %v", err)
	}
	oldV, oldC := version, commit
	version = "2026.1"
	commit = ""
	defer func() { version = oldV; commit = oldC }()

	info := gatherVersionInfo()
	if info.Channel != module.ChannelStable {
		t.Fatalf("expected fallback to %q, got %q", module.ChannelStable, info.Channel)
	}
	if info.Build != "" {
		t.Fatalf("expected empty build on stable fallback, got %q", info.Build)
	}

	output := captureStdout(t, printVersion)
	want := "promise version 2026.1 (channel stable)\n"
	if output != want {
		t.Fatalf("expected %q, got %q", want, output)
	}
}

func TestGatherVersionInfoJSON(t *testing.T) {
	// gatherVersionInfo carries full (non-shortened) hashes; version --json
	// encodes {version, channel, commit, build} as the authoritative source (T1101).
	t.Setenv("PROMISE_HOME", t.TempDir())
	if err := module.WriteUpdateChannel(module.ChannelNext); err != nil {
		t.Fatalf("WriteUpdateChannel: %v", err)
	}
	const buildID = "ea91ebde5f6cc303e472b2fb6d6bf15938f741c5208636822aaf75ebf046f3c9"
	if err := module.WriteEpochBuildID(module.ChannelNext, buildID); err != nil {
		t.Fatalf("WriteEpochBuildID: %v", err)
	}
	oldV, oldC := version, commit
	version = "2026.1"
	commit = "0123456789abcdef0123456789abcdef01234567"
	defer func() { version = oldV; commit = oldC }()

	info := gatherVersionInfo()
	if info.Version != "2026.1" || info.Channel != module.ChannelNext {
		t.Fatalf("unexpected version/channel: %+v", info)
	}
	if info.Commit != commit {
		t.Fatalf("commit should be full SHA, got %q", info.Commit)
	}
	if info.Build != buildID {
		t.Fatalf("build should be full SHA, got %q", info.Build)
	}

	var decoded versionInfo
	if err := json.Unmarshal([]byte(captureStdout(t, func() {
		_ = json.NewEncoder(os.Stdout).Encode(gatherVersionInfo())
	})), &decoded); err != nil {
		t.Fatalf("json round-trip: %v", err)
	}
	if decoded != info {
		t.Fatalf("json mismatch: %+v vs %+v", decoded, info)
	}
}

func TestPrintVersionFallback(t *testing.T) {
	// When version is empty, printVersion falls back to embedded catalog epoch.
	t.Setenv("PROMISE_HOME", t.TempDir())
	old := version
	version = ""
	defer func() { version = old }()

	output := captureStdout(t, printVersion)
	if !strings.HasPrefix(output, "promise version ") {
		t.Fatalf("expected output starting with 'promise version ', got %q", output)
	}
	// Should not be "unknown" since we have an embedded catalog.
	if strings.Contains(output, "unknown") {
		t.Fatal("expected a real epoch, got 'unknown'")
	}
}

func TestPrintVersionUsage(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	printVersionUsage(&buf)
	out := buf.String()
	for _, want := range []string{"usage: promise version", "-commit", "-json", "Examples:"} {
		if !strings.Contains(out, want) {
			t.Errorf("printVersionUsage output missing %q", want)
		}
	}
}

func TestFormatSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1 KB"},
		{1024 * 1024, "1 MB"},
		{67 * 1024 * 1024, "67 MB"},
	}
	for _, tt := range tests {
		got := formatSize(tt.bytes)
		if got != tt.want {
			t.Errorf("formatSize(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

// llvmToolOverrideVars is every variable findLLVMTool honours, used by the
// tests below to guarantee a clean, override-free starting state.
var llvmToolOverrideVars = []string{"PROMISE_OPT", "PROMISE_LLC", "PROMISE_LLD", "PROMISE_LD64LLD", "PROMISE_WASM_LD"}

// clearToolchainOverrides unsets every override and forgets any banner already
// announced in this process, so a test starts from the pinned configuration.
func clearToolchainOverrides(t *testing.T) {
	t.Helper()
	for _, v := range llvmToolOverrideVars {
		t.Setenv(v, "")
		toolchainOverrideAnnounced.Delete(v)
	}
	toolchainOverrideAnnounced.Delete(toolchainBannerSaid)
}

// TestFindLLVMToolEnvironmentOverride verifies the per-tool environment
// overrides — the explicit new-LLVM-bringup path, and the only way a toolchain
// from outside the pinned set enters a build (T2108). Each variable names
// exactly one binary, and using one is announced.
func TestFindLLVMToolEnvironmentOverride(t *testing.T) {
	tests := []struct {
		name   string
		tool   string
		envVar string
		envVal string
	}{
		{"opt override", "opt", "PROMISE_OPT", "/custom/opt"},
		{"llc override", "llc", "PROMISE_LLC", "/custom/llc"},
		{"lld override", "ld.lld", "PROMISE_LLD", "/custom/lld"},
		{"lld-link override", "lld-link", "PROMISE_LLD", "/custom/lld"},
		{"ld64.lld override", "ld64.lld", "PROMISE_LD64LLD", "/custom/ld64.lld"},
		{"wasm-ld override", "wasm-ld", "PROMISE_WASM_LD", "/custom/wasm-ld"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearToolchainOverrides(t)
			t.Setenv(tt.envVar, tt.envVal)

			var banner bytes.Buffer
			old := toolchainWarnW
			toolchainWarnW = &banner
			t.Cleanup(func() { toolchainWarnW = old })

			got, err := findLLVMTool(tt.tool)
			if err != nil {
				t.Fatalf("findLLVMTool: %v", err)
			}
			if got != tt.envVal {
				t.Errorf("expected %q, got %q", tt.envVal, got)
			}
			// The override announces itself — loudly, naming the variable and
			// the path. An override nobody is told about is the defect T2108
			// removed, wearing a different hat.
			if !strings.Contains(banner.String(), tt.envVar+"="+tt.envVal) {
				t.Errorf("override must be announced naming variable and path, got: %q", banner.String())
			}
			// ...and exactly once per process, however many tools are resolved.
			before := banner.Len()
			if _, err := findLLVMTool(tt.tool); err != nil {
				t.Fatalf("second findLLVMTool: %v", err)
			}
			if banner.Len() != before {
				t.Errorf("banner repeated on a second lookup: %q", banner.String()[before:])
			}
		})
	}
}

// TestFindLLVMToolEnvironmentOverrideEmpty verifies that an empty environment
// variable is treated as unset and doesn't short-circuit the search (the env
// value "" is falsy and skipped, allowing resolution to continue to the pinned
// sources) — and that an empty value never announces an override.
func TestFindLLVMToolEnvironmentOverrideEmpty(t *testing.T) {
	clearToolchainOverrides(t)
	t.Setenv("PROMISE_OPT", "   ") // whitespace-only is also "unset"

	var banner bytes.Buffer
	old := toolchainWarnW
	toolchainWarnW = &banner
	t.Cleanup(func() { toolchainWarnW = old })

	got, err := findLLVMTool("opt")
	if err == nil && strings.TrimSpace(got) == "" {
		t.Fatal("an empty override must not resolve to an empty path")
	}
	if banner.Len() != 0 {
		t.Errorf("an unset override must not announce anything, got: %q", banner.String())
	}
}

// TestFindLLVMToolNeverConsultsHost is the T2108 invariant: a tool that exists
// ONLY on PATH (or in Homebrew) is not found. Build inputs are pinned, not
// discovered — a toolchain picked up from incidental host state makes one
// developer's green run everyone else's red trunk.
func TestFindLLVMToolNeverConsultsHost(t *testing.T) {
	clearToolchainOverrides(t)

	// A PATH full of plausible LLVM tools, versioned and unversioned.
	fake := t.TempDir()
	for _, n := range []string{
		"opt", "llc", "ld.lld", "ld64.lld", "lld-link", "wasm-ld",
		"opt-22", "opt-23", "opt-24", "opt-25", "llc-22", "llc-25", "ld.lld-22", "ld.lld-25",
	} {
		if err := os.WriteFile(filepath.Join(fake, n), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tools := []string{"opt", "llc", "ld.lld", "ld64.lld", "wasm-ld"}

	// Resolve once with the real environment first. Two reasons: materializing
	// the pinned view runs codesign on macOS, which needs a real PATH, so a
	// cold host would otherwise fail to resolve and make every assertion below
	// vacuous; and it gives the pinned answer to compare against.
	pinned := map[string]string{}
	for _, tool := range tools {
		if p, err := findLLVMTool(tool); err == nil {
			pinned[tool] = p
		}
	}

	t.Setenv("PATH", fake)

	for _, tool := range tools {
		got, err := findLLVMTool(tool)
		if err != nil {
			if want, ok := pinned[tool]; ok {
				t.Errorf("%s resolved to %s before PATH was replaced but fails after: "+
					"resolution must not depend on PATH at all (%v)", tool, want, err)
			}
			continue // nothing pinned provides it on this host — not finding it IS the rule
		}
		if strings.HasPrefix(got, fake) {
			t.Errorf("%s resolved from PATH (%s) — PATH must never be consulted", tool, got)
		}
		for _, brew := range []string{"/opt/homebrew/", "/usr/local/opt/"} {
			if strings.HasPrefix(got, brew) {
				t.Errorf("%s resolved from Homebrew (%s) — Homebrew must never be consulted", tool, got)
			}
		}
		if want, ok := pinned[tool]; ok && got != want {
			t.Errorf("%s resolved to %s with a decoy PATH but %s without it — "+
				"the answer must not move with the host", tool, got, want)
		}
	}
}

// TestFindLLVMToolNoPinnedToolchainFails verifies the other half of the rule:
// with no override, no sibling and no pinned toolchain, resolution FAILS with a
// diagnostic that names the missing pinned toolchain and the explicit way out.
// It must not fall through to something else on the host, and must not tell the
// reader to install a system LLVM — installing one changes nothing.
func TestFindLLVMToolNoPinnedToolchainFails(t *testing.T) {
	clearToolchainOverrides(t)

	// Every blob source rewritten to a server that has nothing: no network, and
	// a deterministic "cannot be fetched" rather than a timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not here", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PROMISE_BLOB_MIRROR", srv.URL)
	t.Setenv("PROMISE_HOME", t.TempDir())
	t.Setenv("PROMISE_PREBUILTS_CACHE", t.TempDir())

	// resolveLLVMView memoizes a resolved view for the process; a view another
	// test resolved would otherwise answer for this one.
	llvmViewMu.Lock()
	saved := llvmViewDir
	llvmViewDir = ""
	llvmViewMu.Unlock()
	t.Cleanup(func() {
		llvmViewMu.Lock()
		llvmViewDir = saved
		llvmViewMu.Unlock()
	})

	got, err := findLLVMTool("opt")
	if err == nil {
		t.Fatalf("expected failure with no pinned toolchain, got %q — "+
			"resolution must fail rather than fall through to whatever this host has", got)
	}
	// Whichever way the pinned toolchain is unavailable — no manifest entries
	// (missingPinnedToolchainError) or entries that cannot be materialized (the
	// resolver's offline / broken-release error, returned verbatim) — the
	// diagnostic must never send the reader to a package manager: installing a
	// system LLVM does not change what this build uses.
	msg := err.Error()
	for _, unwanted := range []string{"brew install", "apt install", "apt-get install", "install LLVM"} {
		if strings.Contains(msg, unwanted) {
			t.Errorf("diagnostic must not suggest installing a system LLVM (%q), got: %v", unwanted, err)
		}
	}

	// The macOS linker resolves through the same sources, and its wrapper must
	// carry the diagnostic rather than replace it with linker-specific advice
	// (it used to end in "brew install lld").
	if runtime.GOOS == "darwin" {
		_, ferr := findLLVMTool("ld64.lld")
		if ferr == nil {
			t.Fatal("ld64.lld must not resolve with nothing pinned")
		}
		_, lerr := findDarwinLinker()
		if lerr == nil {
			t.Fatal("findDarwinLinker must fail when nothing pinned provides ld64.lld")
		}
		lmsg := lerr.Error()
		// The wrapper adds macOS context and keeps the cause verbatim — it does
		// not replace the diagnostic with linker-specific advice, which is what
		// it used to do ("fix: brew install lld", plus a script that no longer
		// exists).
		if !strings.Contains(lmsg, ferr.Error()) {
			t.Errorf("linker error should carry the resolution diagnostic verbatim:\n  got:  %v\n  want it to contain: %v", lerr, ferr)
		}
		if !strings.Contains(lmsg, "required for macOS linking") {
			t.Errorf("linker error should add the macOS context, got: %v", lerr)
		}
		for _, unwanted := range []string{"brew install", "install-prereqs"} {
			if strings.Contains(lmsg, unwanted) {
				t.Errorf("linker error must not suggest a system install (%q), got: %v", unwanted, lerr)
			}
		}
	}

	// doctor reports the same state, and its remedy must be to stage the pinned
	// toolchain — not to install one.
	c := doctorCheckLLVM()
	if c.Summary == "" || !strings.Contains(c.Summary, "Missing LLVM tools") {
		t.Errorf("doctor should report the tools as missing, got summary %q", c.Summary)
	}
	if !strings.Contains(c.Fix, "promise install") && !strings.Contains(c.Fix, "doctor --repair") {
		t.Errorf("doctor fix should point at staging the pinned toolchain, got %q", c.Fix)
	}
	for _, unwanted := range []string{"brew install", "Install LLVM"} {
		if strings.Contains(c.Fix, unwanted) {
			t.Errorf("doctor fix must not suggest a system LLVM (%q), got %q", unwanted, c.Fix)
		}
	}
}

// TestFindLLVMToolNotInPinnedSet covers the path a tool takes when the pinned
// view resolves but does not contain it. `llvm-as` is the real case: it was only
// ever reachable from a system LLVM, so once the host is not searched, asking for
// it must produce the missing-pinned-toolchain diagnostic naming the view that
// was checked — not a silent fallback, and not a bare "not found".
func TestFindLLVMToolNotInPinnedSet(t *testing.T) {
	clearToolchainOverrides(t)

	// Warm the view first: this is about a tool missing FROM a resolved view,
	// so the view has to resolve.
	if _, err := findLLVMTool("opt"); err != nil {
		t.Skipf("no pinned toolchain staged on this host: %v", err)
	}

	_, err := findLLVMTool("llvm-as")
	if err == nil {
		t.Fatal("llvm-as is not in the pinned set — resolving it must fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no pinned LLVM toolchain provides it") {
		t.Errorf("expected the missing-pinned diagnostic, got: %v", err)
	}
	// The view it checked is named, so the reader can see what WAS there.
	if !strings.Contains(msg, "pinned toolchain view: no llvm-as in ") {
		t.Errorf("diagnostic should name the view it looked in, got: %v", err)
	}
	// A tool with no override variable of its own must not invent one.
	if strings.Contains(msg, "$PROMISE_LLVM_AS") {
		t.Errorf("llvm-as has no override variable; the diagnostic must not claim one: %v", err)
	}
}

// TestAnnounceToolchainOverridesAtStartup covers the startup announcer — the
// path that makes the banner appear for a command that resolves no tool itself
// (the multi-file test runner only spawns children, so without this an override
// would go unannounced for a whole `promise test` run).
func TestAnnounceToolchainOverridesAtStartup(t *testing.T) {
	clearToolchainOverrides(t)
	t.Setenv("PROMISE_LLC", "/custom/llc")
	t.Setenv("PROMISE_WASM_LD", "/custom/wasm-ld")

	var banner bytes.Buffer
	old := toolchainWarnW
	toolchainWarnW = &banner
	t.Cleanup(func() { toolchainWarnW = old })

	announceToolchainOverrides()

	out := banner.String()
	for _, want := range []string{
		"this build does NOT use the pinned toolchain",
		"PROMISE_LLC=/custom/llc",
		"PROMISE_WASM_LD=/custom/wasm-ld",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("startup banner missing %q, got:\n%s", want, out)
		}
	}
	// The consequence is stated once, not repeated under every variable.
	if n := strings.Count(out, "does NOT use the pinned toolchain"); n != 1 {
		t.Errorf("heading printed %d times, want exactly 1:\n%s", n, out)
	}
	// A variable that is not set is not announced.
	if strings.Contains(out, "PROMISE_OPT") {
		t.Errorf("an unset variable must not be announced:\n%s", out)
	}
}

// TestAnnounceToolchainOverrideSilentInTestChild pins the one case where the
// banner is deliberately suppressed. A test child's stdout/stderr is captured
// and parsed by the multi-file parent, so a banner emitted there would be read
// as test output — and the parent's own command already announced it once.
func TestAnnounceToolchainOverrideSilentInTestChild(t *testing.T) {
	clearToolchainOverrides(t)
	t.Setenv(testChildEnv, "1")

	var banner bytes.Buffer
	old := toolchainWarnW
	toolchainWarnW = &banner
	t.Cleanup(func() { toolchainWarnW = old })

	announceToolchainOverride("PROMISE_OPT", "/custom/opt")
	if banner.Len() != 0 {
		t.Errorf("a test child must stay silent, got: %q", banner.String())
	}

	// ...and is not silent otherwise, so the suppression cannot be mistaken for
	// the banner simply never firing.
	t.Setenv(testChildEnv, "")
	announceToolchainOverride("PROMISE_OPT", "/custom/opt")
	if !strings.Contains(banner.String(), "PROMISE_OPT=/custom/opt") {
		t.Errorf("outside a test child the banner must print, got: %q", banner.String())
	}
}

// TestMissingPinnedToolchainDiagnostic pins the wording of the "nothing pinned
// provides this tool" error: it must name each source consulted, say plainly
// that the host is not one of them, and give the explicit bringup path — which
// is the only supported way to build against a different LLVM (T2108).
func TestMissingPinnedToolchainDiagnostic(t *testing.T) {
	err := missingPinnedToolchainError("opt", "PROMISE_OPT", "/opt/promise/bin", "")
	msg := err.Error()
	for _, want := range []string{
		"opt not found",
		"$PROMISE_OPT",
		"/opt/promise/bin",
		"pinned toolchain view",
		"PATH and Homebrew are never consulted",
		"PROMISE_OPT=",
		platformLinkerEnvVar(),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("diagnostic missing %q, got:\n%s", want, msg)
		}
	}
	for _, unwanted := range []string{"brew install", "apt install", "install LLVM", "PROMISE_USE_CLANG"} {
		if strings.Contains(msg, unwanted) {
			t.Errorf("diagnostic must not contain %q, got:\n%s", unwanted, msg)
		}
	}
}
