package common

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// release_changes_test.go covers the `bin/release changes` command using the
// same fakeCutGit seam as release_cut_test.go (same package — directly accessible).

func TestReleaseChangesNoEpoch(t *testing.T) {
	g := newFakeCutGit()
	g.epochTags = nil
	g.logSubjects = []string{"feat: add foo", "fix: bar"}
	var buf bytes.Buffer
	if err := releaseChanges(g, &buf, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "2 commits (no prior stable epoch)") {
		t.Errorf("want count+no-epoch header, got: %q", out)
	}
	if !strings.Contains(out, "feat: add foo") || !strings.Contains(out, "fix: bar") {
		t.Errorf("missing subjects in output: %q", out)
	}
}

func TestReleaseChangesWithEpoch(t *testing.T) {
	g := newFakeCutGit()
	g.epochTags = []string{"epoch-2026.0"}
	g.logSubjects = []string{"feat: something", "fix: other"}
	var buf bytes.Buffer
	if err := releaseChanges(g, &buf, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "2 commits since epoch-2026.0") {
		t.Errorf("want 'N commits since epoch-2026.0', got: %q", out)
	}
	if !strings.Contains(out, "feat: something") || !strings.Contains(out, "fix: other") {
		t.Errorf("missing subjects in output: %q", out)
	}
}

func TestReleaseChangesCommitHashValid(t *testing.T) {
	g := newFakeCutGit()
	g.epochTags = []string{"epoch-2026.0"}
	g.logSubjects = []string{"fix: something"}
	g.ancestorOK = true
	var buf bytes.Buffer
	if err := releaseChanges(g, &buf, "abc1234"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "1 commits since epoch-2026.0") {
		t.Errorf("unexpected output: %q", buf.String())
	}
}

func TestReleaseChangesCommitHashNotAncestor(t *testing.T) {
	g := newFakeCutGit()
	g.ancestorOK = false
	var buf bytes.Buffer
	err := releaseChanges(g, &buf, "deadzz0")
	if err == nil {
		t.Fatal("expected error for non-ancestor commit hash, got nil")
	}
	if !strings.Contains(err.Error(), "not reachable from HEAD") {
		t.Errorf("expected 'not reachable from HEAD', got: %v", err)
	}
}

func TestReleaseChangesLogError(t *testing.T) {
	g := newFakeCutGit()
	g.logErr = errors.New("git log failed")
	var buf bytes.Buffer
	err := releaseChanges(g, &buf, "")
	if err == nil {
		t.Fatal("expected error when git log fails, got nil")
	}
	if !strings.Contains(err.Error(), "git log failed") {
		t.Errorf("expected 'git log failed', got: %v", err)
	}
}

func TestReleaseChangesEpochTagsError(t *testing.T) {
	g := newFakeCutGit()
	g.listEpochErr = errors.New("tag list failed")
	var buf bytes.Buffer
	err := releaseChanges(g, &buf, "")
	if err == nil {
		t.Fatal("expected error when tag list fails, got nil")
	}
	if !strings.Contains(err.Error(), "tag list failed") {
		t.Errorf("expected 'tag list failed', got: %v", err)
	}
}

func TestReleaseChangesZeroSubjects(t *testing.T) {
	g := newFakeCutGit()
	g.epochTags = []string{"epoch-2026.1"}
	g.logSubjects = nil
	var buf bytes.Buffer
	if err := releaseChanges(g, &buf, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "0 commits since epoch-2026.1") {
		t.Errorf("unexpected output: %q", buf.String())
	}
}

func TestResolveChangesUpperHeadError(t *testing.T) {
	g := newFakeCutGit()
	g.headErr = errors.New("HEAD unavailable")
	_, err := resolveChangesUpper(g, "")
	if err == nil {
		t.Fatal("expected error when HeadSHA fails, got nil")
	}
	if !strings.Contains(err.Error(), "resolve HEAD") {
		t.Errorf("expected 'resolve HEAD', got: %v", err)
	}
}

func TestResolveChangesUpperAncestorError(t *testing.T) {
	g := newFakeCutGit()
	g.ancestorErr = errors.New("merge-base failed")
	_, err := resolveChangesUpper(g, "abc1234")
	if err == nil {
		t.Fatal("expected error when IsAncestor fails, got nil")
	}
	if !strings.Contains(err.Error(), "ancestry check") {
		t.Errorf("expected 'ancestry check', got: %v", err)
	}
}

func TestRunReleaseChangesFlagError(t *testing.T) {
	// An unknown flag must surface a parse error without touching git.
	if err := runReleaseChanges(t.TempDir(), []string{"--bogus"}); err == nil {
		t.Fatal("an unknown flag must surface a parse error")
	}
}

func TestRunReleaseChangesFetchError(t *testing.T) {
	g := newFakeCutGit()
	g.fetchErr = errors.New("offline")
	prev := defaultCutGit
	defaultCutGit = func(string) cutGit { return g }
	t.Cleanup(func() { defaultCutGit = prev })

	if err := runReleaseChanges(t.TempDir(), []string{}); err == nil || !strings.Contains(err.Error(), "git fetch") {
		t.Fatalf("a fetch error must abort changes: %v", err)
	}
}

func TestRunReleaseChangesDispatch(t *testing.T) {
	g := newFakeCutGit()
	g.epochTags = []string{"epoch-2026.0"}
	g.logSubjects = []string{"fix: something", "feat: other"}
	prev := defaultCutGit
	defaultCutGit = func(string) cutGit { return g }
	t.Cleanup(func() { defaultCutGit = prev })

	if err := runReleaseChanges(t.TempDir(), []string{}); err != nil {
		t.Fatalf("runReleaseChanges: %v", err)
	}
}
