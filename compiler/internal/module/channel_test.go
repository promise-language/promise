package module

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateChannelDefault(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// No channel file → default "stable".
	ch, err := UpdateChannel()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ch != ChannelStable {
		t.Fatalf("expected default %q, got %q", ChannelStable, ch)
	}
}

func TestUpdateChannelEmptyFileDefaults(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	// An empty/whitespace channel file falls back to "stable".
	if err := os.WriteFile(filepath.Join(tmp, "channel"), []byte("  \n"), 0644); err != nil {
		t.Fatal(err)
	}
	ch, err := UpdateChannel()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ch != ChannelStable {
		t.Fatalf("expected %q for empty file, got %q", ChannelStable, ch)
	}
}

func TestWriteUpdateChannelRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	for _, name := range []string{ChannelNext, ChannelStable} {
		if err := WriteUpdateChannel(name); err != nil {
			t.Fatalf("WriteUpdateChannel(%q): %v", name, err)
		}
		ch, err := UpdateChannel()
		if err != nil {
			t.Fatalf("UpdateChannel after writing %q: %v", name, err)
		}
		if ch != name {
			t.Fatalf("round-trip mismatch: wrote %q, read %q", name, ch)
		}
	}
}

func TestWriteUpdateChannelRejectsInvalid(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PROMISE_HOME", tmp)

	err := WriteUpdateChannel("beta")
	if err == nil {
		t.Fatal("expected error for invalid channel name")
	}
	// Nothing should have been persisted.
	if _, statErr := os.Stat(filepath.Join(tmp, "channel")); !os.IsNotExist(statErr) {
		t.Fatalf("expected no channel file after invalid write, stat err: %v", statErr)
	}
}
