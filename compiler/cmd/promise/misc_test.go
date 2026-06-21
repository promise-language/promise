package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/module"
)

func TestDirSize(t *testing.T) {
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
	// When version and commit are set, printVersion appends a shortened,
	// explicitly-labeled "commit <sha7>" after the channel (T1101).
	t.Setenv("PROMISE_HOME", t.TempDir())
	oldV, oldC := version, commit
	version = "2026.0"
	commit = "0123456789abcdef0123456789abcdef01234567"
	defer func() { version = oldV; commit = oldC }()

	output := captureStdout(t, printVersion)
	want := "promise version 2026.0 (channel stable, commit 0123456)\n"
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

func TestFormatSize(t *testing.T) {
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
