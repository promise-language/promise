package main

import (
	"strings"
	"testing"

	flowsdk "djabi.dev/go/flow_sdk"
)

func TestBuildCommitMessage_SubjectAndTrailer(t *testing.T) {
	it := &flowsdk.Item{ID: "T0445", Title: "Add trailer", Model: "claude-opus-4-8"}
	msg := buildCommitMessage(it)

	lines := strings.Split(msg, "\n")
	if lines[0] != "T0445: Add trailer" {
		t.Errorf("subject = %q, want %q", lines[0], "T0445: Add trailer")
	}
	if len(lines) < 3 || lines[1] != "" {
		t.Errorf("expected blank line after subject, got: %q", msg)
	}
	last := lines[len(lines)-1]
	want := "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
	if last != want {
		t.Errorf("trailer = %q, want %q", last, want)
	}
}

func TestBuildCommitMessage_AlwaysHasTrailer(t *testing.T) {
	cases := []flowsdk.ModelName{"", "claude-opus-4-7", "sonnet", "x-experimental"}
	for _, m := range cases {
		it := &flowsdk.Item{ID: "T1", Title: "t", Model: m}
		msg := buildCommitMessage(it)
		if !strings.Contains(msg, "\nCo-Authored-By: ") {
			t.Errorf("Model %q: missing Co-Authored-By trailer in: %q", m, msg)
		}
		if !strings.HasSuffix(msg, " <noreply@anthropic.com>") {
			t.Errorf("Model %q: trailer should end with anthropic noreply, got: %q", m, msg)
		}
	}
}

func TestCapitalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"a", "A"},
		{"opus", "Opus"},
	}
	for _, c := range cases {
		if got := capitalize(c.in); got != c.want {
			t.Errorf("capitalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestModelDisplayName(t *testing.T) {
	cases := []struct {
		in   flowsdk.ModelName
		want string
	}{
		{"", "Claude"},
		{"claude-opus-4-7", "Claude Opus 4.7"},
		{"claude-opus-4-8", "Claude Opus 4.8"},
		{"claude-sonnet-4-6", "Claude Sonnet 4.6"},
		{"claude-haiku-4-5", "Claude Haiku 4.5"},
		{"claude-haiku-4-5-20251001", "Claude Haiku 4.5"},
		{"opus", "Claude Opus"},
		{"sonnet", "Claude Sonnet"},
		{"haiku", "Claude Haiku"},
		{"x-experimental", "Claude (x-experimental)"},
	}
	for _, c := range cases {
		got := modelDisplayName(c.in)
		if got != c.want {
			t.Errorf("modelDisplayName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
