package module

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeTaggedRepo builds a git repo with one commit and the given tag names. It
// returns the work-tree path (usable as a git remote URL for ls-remote) and the
// HEAD commit SHA. Each tag is created as an annotated tag (the harder case for
// ListRepoTags — annotated tags appear twice in ls-remote, raw + peeled ^{}).
func makeTaggedRepo(t *testing.T, tags ...string) (repo, head string) {
	t.Helper()
	repo = filepath.Join(t.TempDir(), "tagged")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s\n%s", args, err, out)
		}
		return string(out)
	}
	run("git", "init", "--initial-branch=main")
	run("git", "config", "user.email", "t@t.com")
	run("git", "config", "user.name", "T")
	os.WriteFile(filepath.Join(repo, "promise.toml"), []byte("[module]\nname = \"m\"\nepoch = \"2026.0\"\n"), 0644)
	run("git", "add", ".")
	run("git", "commit", "-m", "init")
	for _, tag := range tags {
		run("git", "tag", "-a", tag, "-m", tag)
	}
	head = strings.TrimSpace(run("git", "rev-parse", "HEAD"))
	return repo, head
}

func TestListRepoTagsEpochAndStable(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, head := makeTaggedRepo(t, "epoch-2026.0", "epoch-2026.1", "epoch-2026.10", "stable", "v1.0.0", "epoch-next")

	epochTags, stable, err := ListRepoTags(repo)
	if err != nil {
		t.Fatalf("ListRepoTags: %v", err)
	}

	// epoch-next and v1.0.0 must be ignored; 2026.10 must sort above 2026.1.
	wantEpochs := []string{"2026.10", "2026.1", "2026.0"}
	if len(epochTags) != len(wantEpochs) {
		t.Fatalf("got %d epoch tags, want %d: %+v", len(epochTags), len(wantEpochs), epochTags)
	}
	for i, w := range wantEpochs {
		if epochTags[i].Epoch != w {
			t.Errorf("epochTags[%d].Epoch = %q, want %q", i, epochTags[i].Epoch, w)
		}
		// Annotated tags must dereference to the underlying commit, not the tag object.
		if epochTags[i].Commit != head {
			t.Errorf("epochTags[%d].Commit = %q, want peeled commit %q", i, epochTags[i].Commit, head)
		}
	}
	if stable != head {
		t.Errorf("stable commit = %q, want %q", stable, head)
	}
}

func TestListRepoTagsNoTags(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, _ := makeTaggedRepo(t) // no tags
	epochTags, stable, err := ListRepoTags(repo)
	if err != nil {
		t.Fatalf("ListRepoTags: %v", err)
	}
	if len(epochTags) != 0 {
		t.Errorf("expected 0 epoch tags, got %+v", epochTags)
	}
	if stable != "" {
		t.Errorf("expected empty stable commit, got %q", stable)
	}
}

func TestParseEpoch(t *testing.T) {
	cases := []struct {
		in        string
		year, min int
		ok        bool
	}{
		{"2026.1", 2026, 1, true},
		{"2026.10", 2026, 10, true},
		{"2027.0", 2027, 0, true},
		{"next", 0, 0, false},
		{"2026", 0, 0, false},   // missing minor
		{"2026.x", 0, 0, false}, // non-numeric minor
		{"x.1", 0, 0, false},    // non-numeric year
		{"", 0, 0, false},
	}
	for _, c := range cases {
		y, m, ok := ParseEpoch(c.in)
		if ok != c.ok || (ok && (y != c.year || m != c.min)) {
			t.Errorf("ParseEpoch(%q) = (%d, %d, %v), want (%d, %d, %v)", c.in, y, m, ok, c.year, c.min, c.ok)
		}
	}
}

func TestListRepoTagsBadURL(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// A path that is not a git repository — ls-remote must fail with a wrapped,
	// actionable error rather than returning empty results.
	_, _, err := ListRepoTags(filepath.Join(t.TempDir(), "not-a-repo"))
	if err == nil {
		t.Fatal("expected error listing tags from a non-repo path")
	}
	if !strings.Contains(err.Error(), "cannot list tags") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCandidates(t *testing.T) {
	tags := []EpochTag{
		{Epoch: "2026.0", Tag: "epoch-2026.0", Commit: "a"},
		{Epoch: "2026.1", Tag: "epoch-2026.1", Commit: "b"},
		{Epoch: "2026.10", Tag: "epoch-2026.10", Commit: "c"},
		{Epoch: "2027.0", Tag: "epoch-2027.0", Commit: "d"},
	}

	// Project on 2026.10: candidates are 2026.10, 2026.1, 2026.0 (descending),
	// 2027.0 excluded (newer than project).
	got := Candidates(tags, "2026.10")
	wantOrder := []string{"2026.10", "2026.1", "2026.0"}
	if len(got) != len(wantOrder) {
		t.Fatalf("got %d candidates, want %d: %+v", len(got), len(wantOrder), got)
	}
	for i, w := range wantOrder {
		if got[i].Epoch != w {
			t.Errorf("candidate[%d] = %q, want %q", i, got[i].Epoch, w)
		}
	}

	// Project older than all tags → no candidates.
	if c := Candidates(tags, "2025.0"); len(c) != 0 {
		t.Errorf("expected no candidates for 2025.0, got %+v", c)
	}

	// Exact match included.
	if c := Candidates(tags, "2027.0"); len(c) != 4 {
		t.Errorf("expected all 4 candidates for 2027.0, got %d", len(c))
	}
}

func TestHighestEpoch(t *testing.T) {
	tags := []EpochTag{
		{Epoch: "2026.1", Tag: "epoch-2026.1"},
		{Epoch: "2026.9", Tag: "epoch-2026.9"},
		{Epoch: "2026.10", Tag: "epoch-2026.10"},
	}
	e, tag := HighestEpoch(tags)
	if e != "2026.10" || tag != "epoch-2026.10" {
		t.Errorf("HighestEpoch = (%q, %q), want (2026.10, epoch-2026.10)", e, tag)
	}
	if e, tag := HighestEpoch(nil); e != "" || tag != "" {
		t.Errorf("HighestEpoch(nil) = (%q, %q), want empty", e, tag)
	}
}

func TestLowestEpoch(t *testing.T) {
	tags := []EpochTag{
		{Epoch: "2026.1", Tag: "epoch-2026.1"},
		{Epoch: "2026.9", Tag: "epoch-2026.9"},
		{Epoch: "2026.10", Tag: "epoch-2026.10"},
	}
	e, tag := LowestEpoch(tags)
	if e != "2026.1" || tag != "epoch-2026.1" {
		t.Errorf("LowestEpoch = (%q, %q), want (2026.1, epoch-2026.1)", e, tag)
	}
	if e, tag := LowestEpoch(nil); e != "" || tag != "" {
		t.Errorf("LowestEpoch(nil) = (%q, %q), want empty", e, tag)
	}
}

func TestNoCompatibleVersionErrorOnlyNewer(t *testing.T) {
	err := &NoCompatibleVersionError{
		Module:               "github.com/you/foo",
		Epoch:                "2026.1",
		OnlyNewerEpochs:      true,
		LowestSupportedEpoch: "2026.3",
		LowestTag:            "epoch-2026.3",
	}
	msg := err.Error()
	for _, want := range []string{
		"has no version compatible with epoch 2026.1",
		"oldest epoch tag is 2026.3 (tag epoch-2026.3)",
		"raise this project to epoch ≥ 2026.3",
		"use a fork:",
		"[replace] github.com/you/foo",
		"wait for the module to publish an epoch-2026.1 tag",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("OnlyNewer error missing %q in:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "pin this project to epoch ≤") {
		t.Errorf("OnlyNewer error should not advise pinning to ≤:\n%s", msg)
	}
}

func TestNoCompatibleVersionErrorFormat(t *testing.T) {
	err := &NoCompatibleVersionError{
		Module:               "github.com/you/foo",
		Epoch:                "2026.3",
		HighestVerifiedEpoch: "2026.1",
		HighestTag:           "epoch-2026.1",
	}
	msg := err.Error()
	for _, want := range []string{
		"has no version compatible with epoch 2026.3",
		"highest verified epoch: 2026.1   (tag epoch-2026.1)",
		"newer tags fail to build under 2026.3",
		"pin this project to epoch ≤ 2026.1",
		"use a fork:",
		"[replace] github.com/you/foo",
		"wait for the module to publish an epoch-2026.3 tag",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("NoCompatibleVersionError missing %q in:\n%s", want, msg)
		}
	}
}

func TestNoCompatibleVersionErrorNoTags(t *testing.T) {
	err := &NoCompatibleVersionError{Module: "x", Epoch: "2026.3"}
	msg := err.Error()
	if !strings.Contains(msg, "carries no epoch-* tags") {
		t.Errorf("expected no-tags phrasing, got:\n%s", msg)
	}
	if strings.Contains(msg, "highest verified epoch:") {
		t.Errorf("should not show a highest verified epoch when none exists:\n%s", msg)
	}
}
