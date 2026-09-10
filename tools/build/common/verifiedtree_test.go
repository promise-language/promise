package common

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The contract, tested from the recording end: the id verify records must
// equal the id the guard computes over the real index after `git add -A`,
// because that agreement is the whole of what the two ends share.
//
// The workspace's TestRecordPathAgreesWithGuard is not carried: the guard end
// lives in that repository, and pinning its spelling is that repository's test.

// vtGit runs git in dir with a fixed identity, failing the test on error.
func vtGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// vtInit is a fresh git repo with no .gitignore at all.
func vtInit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	vtGit(t, dir, "init")
	vtGit(t, dir, "config", "user.name", "test")
	vtGit(t, dir, "config", "user.email", "test@test")
	return dir
}

// vtRepo is vtInit plus the .gitignore entry every project is required to have
// for .workspace/ (the workspace's tool-contract §3), so the record this
// package writes is never itself part of the tree it records.
func vtRepo(t *testing.T) string {
	t.Helper()
	dir := vtInit(t)
	writeFile(t, dir, ".gitignore", ".workspace/\n")
	return dir
}

// recordedTree reads back the id recordVerifiedTree wrote.
func recordedTree(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".workspace", "verified-tree"))
	if err != nil {
		t.Fatalf("read the verified-tree record: %v", err)
	}
	return strings.TrimSpace(string(data))
}

func TestRecordMatchesRealStage(t *testing.T) {
	dir := vtRepo(t)
	writeFile(t, dir, "a.txt", "a\n")
	writeFile(t, dir, "sub/b.txt", "b\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "commit", "-q", "-m", "base")
	writeFile(t, dir, "a.txt", "a2\n")

	if err := recordVerifiedTree(dir); err != nil {
		t.Fatalf("recordVerifiedTree: %v", err)
	}
	vtGit(t, dir, "add", "-A")
	staged, err := RunOutputIn(dir, "git", "write-tree")
	if err != nil {
		t.Fatalf("git write-tree: %v", err)
	}
	if got := recordedTree(t, dir); got != strings.TrimSpace(staged) {
		t.Errorf("recorded %s, but git add -A stages %s — the two ends disagree", got, staged)
	}
}

func TestRecordIncludesUntracked(t *testing.T) {
	// A step whose whole output is new files must produce a committable match,
	// which is why `git stash create` (tracked modifications only) was rejected.
	dir := vtRepo(t)
	writeFile(t, dir, "a.txt", "a\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "commit", "-q", "-m", "base")
	writeFile(t, dir, "new.txt", "new\n")

	if err := recordVerifiedTree(dir); err != nil {
		t.Fatalf("recordVerifiedTree: %v", err)
	}
	names, err := RunOutputIn(dir, "git", "ls-tree", "-r", "--name-only", recordedTree(t, dir))
	if err != nil {
		t.Fatalf("git ls-tree: %v", err)
	}
	if !strings.Contains(names, "new.txt") {
		t.Errorf("untracked non-ignored file missing from recorded tree: %q", names)
	}
}

func TestRecordRespectsIgnoreRules(t *testing.T) {
	// An ignored file stays out; a tracked-but-ignored file stays in — the
	// reason the temp index is seeded rather than left empty.
	dir := vtRepo(t)
	writeFile(t, dir, ".gitignore", ".workspace/\nignored.txt\npinned.txt\n")
	writeFile(t, dir, "pinned.txt", "pinned\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "add", "-f", "pinned.txt")
	vtGit(t, dir, "commit", "-q", "-m", "base")
	writeFile(t, dir, "ignored.txt", "ignored\n")

	if err := recordVerifiedTree(dir); err != nil {
		t.Fatalf("recordVerifiedTree: %v", err)
	}
	names, err := RunOutputIn(dir, "git", "ls-tree", "-r", "--name-only", recordedTree(t, dir))
	if err != nil {
		t.Fatalf("git ls-tree: %v", err)
	}
	if strings.Contains(names, "ignored.txt") {
		t.Errorf("ignored file should not be in the recorded tree: %q", names)
	}
	if !strings.Contains(names, "pinned.txt") {
		t.Errorf("tracked-but-ignored file should be in the recorded tree: %q", names)
	}
}

func TestRecordTrackedSetFollowsIndexNotHEAD(t *testing.T) {
	// The tracked set `git add -A` starts from is the real index's, not
	// HEAD's, and the two differ exactly where ignore rules bite: an ignored
	// file force-added but not yet committed must be in the blessed tree, and
	// one just `git rm --cached`ed must be out. Seeded any other way, the
	// record is a tree no `git add -A` can stage — a permanent guard refusal
	// whose named recovery, re-running verify, reproduces it.
	dir := vtRepo(t)
	writeFile(t, dir, ".gitignore", ".workspace/\nadded.txt\ndropped.txt\n")
	writeFile(t, dir, "dropped.txt", "dropped\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "add", "-f", "dropped.txt")
	vtGit(t, dir, "commit", "-q", "-m", "base")
	writeFile(t, dir, "added.txt", "added\n")
	vtGit(t, dir, "add", "-f", "added.txt")
	vtGit(t, dir, "rm", "-q", "--cached", "dropped.txt")

	if err := recordVerifiedTree(dir); err != nil {
		t.Fatalf("recordVerifiedTree: %v", err)
	}
	vtGit(t, dir, "add", "-A")
	staged, err := RunOutputIn(dir, "git", "write-tree")
	if err != nil {
		t.Fatalf("git write-tree: %v", err)
	}
	if got := recordedTree(t, dir); got != strings.TrimSpace(staged) {
		t.Errorf("recorded %s, but git add -A stages %s — the two ends disagree", got, staged)
	}
	names, err := RunOutputIn(dir, "git", "ls-tree", "-r", "--name-only", recordedTree(t, dir))
	if err != nil {
		t.Fatalf("git ls-tree: %v", err)
	}
	if !strings.Contains(names, "added.txt") {
		t.Errorf("force-added file missing from recorded tree: %q", names)
	}
	if strings.Contains(names, "dropped.txt") {
		t.Errorf("rm --cached'ed file should not be in the recorded tree: %q", names)
	}
}

func TestRecordLeavesIndexAlone(t *testing.T) {
	dir := vtRepo(t)
	writeFile(t, dir, "a.txt", "a\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "commit", "-q", "-m", "base")
	writeFile(t, dir, "staged.txt", "staged\n")
	vtGit(t, dir, "add", "staged.txt")
	writeFile(t, dir, "unstaged.txt", "unstaged\n")

	before, err := RunOutputIn(dir, "git", "diff", "--cached", "--name-only")
	if err != nil {
		t.Fatal(err)
	}
	if err := recordVerifiedTree(dir); err != nil {
		t.Fatalf("recordVerifiedTree: %v", err)
	}
	after, err := RunOutputIn(dir, "git", "diff", "--cached", "--name-only")
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("recording disturbed the real index: before %q, after %q", before, after)
	}
}

func TestRecordIsAtomicallyReplaced(t *testing.T) {
	// A second record replaces the first and leaves no temp file behind. The
	// rename is what keeps a reader from ever seeing a half-written id, and it
	// consumes the temp file rather than copying it — so a repo verified daily
	// does not end up with one .verified-tree-* per run.
	dir := vtRepo(t)
	writeFile(t, dir, "a.txt", "a\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "commit", "-q", "-m", "base")

	if err := recordVerifiedTree(dir); err != nil {
		t.Fatalf("first record: %v", err)
	}
	first := recordedTree(t, dir)
	writeFile(t, dir, "b.txt", "b\n")
	if err := recordVerifiedTree(dir); err != nil {
		t.Fatalf("second record: %v", err)
	}
	if second := recordedTree(t, dir); second == first {
		t.Errorf("both records are %s — the second did not replace the first", second)
	}
	entries, err := os.ReadDir(filepath.Join(dir, ".workspace"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".verified-tree-") {
			t.Errorf("temp file %s survived the rename", e.Name())
		}
	}
}

func TestRecordRefusesWhenTheRecordIsNotIgnored(t *testing.T) {
	// The .gitignore entry is a precondition of the contract, not a tidiness
	// preference: without it the record is part of what `git add -A` stages,
	// so rewriting it after the tree was measured blesses an id no commit can
	// match — and re-running verify, the guard's named recovery, reproduces
	// it. Recording must say so instead of producing that state.
	dir := vtInit(t)
	writeFile(t, dir, "a.txt", "a\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "commit", "-q", "-m", "base")

	err := recordVerifiedTree(dir)
	if err == nil {
		t.Fatal("recording into a repo that does not ignore .workspace/ must fail")
	}
	if !strings.Contains(err.Error(), ".gitignore") {
		t.Errorf("the error must name the fix, got: %v", err)
	}
	if Exists(filepath.Join(dir, ".workspace", "verified-tree")) {
		t.Error("nothing should be blessed when the record path is not ignored")
	}
}

func TestClearVerifiedTree(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, ".workspace", "verified-tree")
	writeFile(t, dir, ".workspace/verified-tree", "abc\n")
	if err := clearVerifiedTree(dir); err != nil {
		t.Fatalf("clearing an existing record: %v", err)
	}
	if Exists(record) {
		t.Error("record should be gone after clear")
	}
	if err := clearVerifiedTree(dir); err != nil {
		t.Fatalf("clearing an absent record should not be an error: %v", err)
	}
}

func TestRecordOutsideGitCheckout(t *testing.T) {
	dir := t.TempDir()

	// "Outside a checkout" has to be arranged, not assumed. bin/verify points
	// TMPDIR at .promise-home/tmp inside this repository (SetupLocalCache), so
	// under a real verify run t.TempDir() sits *inside* a git checkout and git
	// finds the enclosing .git by walking up. GIT_CEILING_DIRECTORIES stops
	// that walk at this test's own temp parent, which is the only directory it
	// affects.
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))

	// Assert the precondition rather than trusting it: without this the test
	// passes whenever TMPDIR happens to fall outside a repository and fails
	// whenever it does not, which is how it read as green standalone and red
	// under bin/verify.
	if _, err := gitWithIndex(dir, "", "rev-parse", "--git-dir"); err == nil {
		t.Fatalf("precondition: %s is still inside a git checkout", dir)
	}

	if err := recordVerifiedTree(dir); err != nil {
		t.Fatalf("outside a checkout recording should be a no-op, not an error: %v", err)
	}
	if Exists(filepath.Join(dir, ".workspace", "verified-tree")) {
		t.Error("no record should be written outside a git checkout")
	}
}

// TestRunVerifyRedRunLeavesNothingBlessed pins the wiring, not the helper: the
// clear has to sit at the top of RunVerify, before any step can change the
// tree. A stale blessing surviving a failed verify is the one outcome the
// contract must never produce — the guard would honour a record describing
// content that no longer exists.
//
// The run is driven over an empty temp root, so it reddens on the very first
// step — which is the point: the clear has to precede even that one.
// HOME is redirected so acquireVerifyLock takes a private lock rather than the
// host's ~/.promise/verify.lock, which an outer bin/verify holds while these
// tests run — otherwise the run would return ErrLockTimeout before reaching
// the clear, and the test would pass or fail depending on who invoked it.
func TestRunVerifyRedRunLeavesNothingBlessed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows

	dir := t.TempDir()
	writeFile(t, dir, ".workspace/verified-tree", "stale-blessing\n")

	if err := RunVerify(dir, []string{"--shared", "--lock-timeout=30s"}); err == nil {
		t.Fatal("verify over an empty root should fail")
	}
	if Exists(filepath.Join(dir, ".workspace", "verified-tree")) {
		t.Error("a red run must leave nothing blessed — the stale record survived")
	}
}

// stagedTree is what a real `git add -A` in dir produces — the id the guard
// will compute at commit time, and therefore the one the record must equal.
func stagedTree(t *testing.T, dir string) string {
	t.Helper()
	vtGit(t, dir, "add", "-A")
	staged, err := RunOutputIn(dir, "git", "write-tree")
	if err != nil {
		t.Fatalf("git write-tree: %v", err)
	}
	return strings.TrimSpace(staged)
}

func TestRecordBeforeTheRepoHasAnIndex(t *testing.T) {
	// A clone that has never run `git add` has no .git/index file at all, so
	// the seed step finds nothing to copy. That is not a failure: an empty
	// seed is exactly that repo's tracked set, which is the one case where
	// "empty" and "the real index" agree. Untested, this path only ever runs
	// on a machine nobody is watching — a fresh clone's first verify.
	dir := vtRepo(t)
	writeFile(t, dir, "a.txt", "a\n")
	writeFile(t, dir, "sub/b.txt", "b\n")

	realIndex := filepath.Join(dir, ".git", "index")
	if Exists(realIndex) {
		t.Fatalf("precondition: %s exists, so this is not the no-index case", realIndex)
	}

	if err := recordVerifiedTree(dir); err != nil {
		t.Fatalf("recordVerifiedTree with no index yet: %v", err)
	}
	if got, want := recordedTree(t, dir), stagedTree(t, dir); got != want {
		t.Errorf("recorded %s, but git add -A stages %s — the two ends disagree", got, want)
	}
}

func TestRecordIsStableWhileTheContentIs(t *testing.T) {
	// The headline property of the content-addressed replacement: the blessing
	// survives everything that is not a content change. A tree blessed last
	// week and untouched is still blessed — so neither a rewritten-identical
	// file (new mtime, same bytes) nor an ignored build output appearing
	// afterwards may move the id. If either did, the gate would refuse a
	// commit the tree has not actually changed since, and the clock this
	// replaced would be back in a different disguise.
	dir := vtRepo(t)
	writeFile(t, dir, ".gitignore", ".workspace/\nbuild-output/\n")
	writeFile(t, dir, "a.txt", "a\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "commit", "-q", "-m", "base")

	if err := recordVerifiedTree(dir); err != nil {
		t.Fatalf("first record: %v", err)
	}
	first := recordedTree(t, dir)

	// Same bytes, different mtime — the index's stat cache is invalidated but
	// the blob is not.
	writeFile(t, dir, "a.txt", "a\n")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "a.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	// And an ignored artefact of the kind every build step leaves behind.
	writeFile(t, dir, "build-output/artifact.bin", "junk\n")

	if err := recordVerifiedTree(dir); err != nil {
		t.Fatalf("second record: %v", err)
	}
	if second := recordedTree(t, dir); second != first {
		t.Errorf("id moved without a content change: %s then %s", first, second)
	}
}

func TestRecordFormatIsOneNewlineTerminatedId(t *testing.T) {
	// The reading end is bin/precommit-guard, in another repository. Nothing
	// in this build catches a drift in the on-disk shape, so the shape is
	// pinned here: exactly one object id and one trailing newline, no
	// whitespace, no second line, no comment.
	dir := vtRepo(t)
	writeFile(t, dir, "a.txt", "a\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "commit", "-q", "-m", "base")

	if err := recordVerifiedTree(dir); err != nil {
		t.Fatalf("recordVerifiedTree: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".workspace", "verified-tree"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.HasSuffix(body, "\n") || strings.Count(body, "\n") != 1 {
		t.Fatalf("record must be exactly one newline-terminated line, got %q", body)
	}
	id := strings.TrimSuffix(body, "\n")
	if len(id) != 40 && len(id) != 64 { // sha1 or sha256 object format
		t.Fatalf("record is not an object id: %q", id)
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("record is not lowercase hex: %q", id)
		}
	}
}

func TestRecordFailsWhenTheRecordDirectoryIsBlocked(t *testing.T) {
	// .workspace/ occupied by a regular file. The record cannot be created,
	// and the only acceptable outcome is a named failure: recording is verify's
	// one hard-failing step precisely because a silent skip is
	// indistinguishable from a pass and refuses every later commit with no way
	// to tell why.
	dir := vtInit(t)
	writeFile(t, dir, ".gitignore", ".workspace\n")
	writeFile(t, dir, "a.txt", "a\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "commit", "-q", "-m", "base")
	writeFile(t, dir, ".workspace", "not a directory\n")

	err := recordVerifiedTree(dir)
	if err == nil {
		t.Fatal("recording with .workspace occupied by a file must fail")
	}
	if !strings.Contains(err.Error(), ".workspace") {
		t.Errorf("the error must name the path it could not create, got: %v", err)
	}
}

func TestRecordCleansUpWhenTheRenameFails(t *testing.T) {
	// The rename is the atomic step, and every failure exit before and after it
	// removes the temp file. Nothing else in the repo would ever notice the
	// litter — the record's directory is gitignored — so a leaked
	// .verified-tree-* per failed run would accumulate silently forever.
	dir := vtRepo(t)
	writeFile(t, dir, "a.txt", "a\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "commit", "-q", "-m", "base")
	// A directory cannot be replaced by a rename from a regular file.
	writeFile(t, dir, ".workspace/verified-tree/occupied", "x\n")

	if err := recordVerifiedTree(dir); err == nil {
		t.Fatal("recording over a directory must fail")
	}
	entries, err := os.ReadDir(filepath.Join(dir, ".workspace"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".verified-tree-") {
			t.Errorf("temp file %s survived a failed record", e.Name())
		}
	}
}

func TestRecordLeavesNoTempIndexBehind(t *testing.T) {
	// The temp index is a whole copy of the real one — on this repository a
	// multi-megabyte file. It is removed by a defer, which is exactly the kind
	// of cleanup that survives review and then quietly stops running.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp) // unix
	t.Setenv("TMP", tmp)    // windows
	t.Setenv("TEMP", tmp)

	dir := vtRepo(t)
	writeFile(t, dir, "a.txt", "a\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "commit", "-q", "-m", "base")

	if err := recordVerifiedTree(dir); err != nil {
		t.Fatalf("recordVerifiedTree: %v", err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "verified-tree-") {
			t.Errorf("temp index dir %s survived recording", e.Name())
		}
	}
}

func TestEnsureRecordIgnoredAcceptsTheSpellingItRecommends(t *testing.T) {
	// The refusal tells the reader to add "/.workspace/" — the anchored
	// spelling this repository's own .gitignore uses. If that spelling did not
	// satisfy the check, the error would send them in a circle.
	dir := vtInit(t)
	writeFile(t, dir, ".gitignore", "/.workspace/\n")
	writeFile(t, dir, "a.txt", "a\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "commit", "-q", "-m", "base")

	if err := ensureRecordIgnored(dir); err != nil {
		t.Fatalf("the spelling the error recommends must satisfy the check: %v", err)
	}
	if err := recordVerifiedTree(dir); err != nil {
		t.Fatalf("recordVerifiedTree: %v", err)
	}
	if got, want := recordedTree(t, dir), stagedTree(t, dir); got != want {
		t.Errorf("recorded %s, but git add -A stages %s", got, want)
	}
}

func TestEnsureRecordIgnoredIsSilentWhenTheProbeCannotAnswer(t *testing.T) {
	// "Not ignored" is exit 1 and nothing else. A probe that cannot answer at
	// all — here, no repository to ask — must not redden an otherwise green
	// verify: the check exists to catch a misconfigured .gitignore, not to add
	// a second way for git itself to fail the run. The no-checkout no-op in
	// recordVerifiedTree would not save it; ensureRecordIgnored is reached
	// through other paths, and the reason it returns nil has to be this one.
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))

	probe := exec.Command("git", "check-ignore", "-q", "--", verifiedTreeRecord)
	probe.Dir = dir
	err := probe.Run()
	var exit *exec.ExitError
	if err == nil || (errors.As(err, &exit) && exit.ExitCode() == 1) {
		t.Fatalf("precondition: the probe answered (%v) instead of failing to run", err)
	}

	if err := ensureRecordIgnored(dir); err != nil {
		t.Errorf("an unanswerable probe must not fail the run, got: %v", err)
	}
}

func TestGitWithIndexReportsAFailureWithNoStderr(t *testing.T) {
	// `git check-ignore -q` says "no" by exit code alone — no stdout, no
	// stderr. The fallback branch has to produce a usable message from that
	// and, in particular, must not index args[0] out of an empty slice.
	dir := vtRepo(t)
	writeFile(t, dir, "a.txt", "a\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "commit", "-q", "-m", "base")

	out, err := gitWithIndex(dir, "", "check-ignore", "-q", "--", "a.txt")
	if err == nil {
		t.Fatalf("a non-ignored path must report failure, got %q", out)
	}
	if !strings.Contains(err.Error(), "check-ignore") {
		t.Errorf("the error must name the subcommand that failed, got: %v", err)
	}
}

func TestRunVerifyRefusesWhenTheStaleBlessingCannotBeCleared(t *testing.T) {
	// Fail-closed at the top. If the previous blessing cannot be removed,
	// verify must stop before its first step rather than run to green over a
	// record it does not control — that record would then describe content
	// this run has already rewritten. The failure is named so the reader knows
	// which of verify's many steps refused.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows

	dir := t.TempDir()
	writeFile(t, dir, ".workspace/verified-tree/occupied", "x\n")

	err := RunVerify(dir, []string{"--shared", "--lock-timeout=30s"})
	if err == nil {
		t.Fatal("verify must fail when the stale record cannot be cleared")
	}
	if !strings.Contains(err.Error(), "clear verified tree") {
		t.Errorf("verify must fail at the clear, not later, got: %v", err)
	}
}

func TestRecordInALinkedWorktree(t *testing.T) {
	// A linked worktree keeps its index at .git/worktrees/<name>/index, and
	// `git rev-parse --git-path index` answers with an absolute path there
	// rather than the repo-relative ".git/index" a normal checkout gets. That
	// asymmetry is the whole reason for the IsAbs branch in the seed step: join
	// an already-absolute path onto the root and the copy silently finds
	// nothing, so the temp index is seeded empty and every tracked-but-ignored
	// file drops out of the blessed tree. Agents run this repository's flows in
	// worktrees, so this is the ordinary case there, not an exotic one.
	main := vtRepo(t)
	writeFile(t, main, ".gitignore", ".workspace/\npinned.txt\n")
	writeFile(t, main, "pinned.txt", "pinned\n")
	writeFile(t, main, "a.txt", "a\n")
	vtGit(t, main, "add", "-A")
	vtGit(t, main, "add", "-f", "pinned.txt")
	vtGit(t, main, "commit", "-q", "-m", "base")

	linked := filepath.Join(t.TempDir(), "wt")
	vtGit(t, main, "worktree", "add", "-q", "-b", "wt", linked)

	realIndex, err := gitWithIndex(linked, "", "rev-parse", "--git-path", "index")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(realIndex) {
		t.Skipf("this git answers --git-path with %q in a linked worktree; the absolute case is what this test is for", realIndex)
	}

	writeFile(t, linked, "b.txt", "b\n")
	if err := recordVerifiedTree(linked); err != nil {
		t.Fatalf("recordVerifiedTree in a linked worktree: %v", err)
	}
	if got, want := recordedTree(t, linked), stagedTree(t, linked); got != want {
		t.Errorf("recorded %s, but git add -A stages %s — the two ends disagree", got, want)
	}
	names, err := RunOutputIn(linked, "git", "ls-tree", "-r", "--name-only", recordedTree(t, linked))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(names, "pinned.txt") {
		t.Errorf("tracked-but-ignored file dropped out — the index was not seeded: %q", names)
	}
}

func TestRecordReplacesRatherThanRewritesInPlace(t *testing.T) {
	// The reader is a different process — bin/precommit-guard, mid-commit —
	// so "atomic" here is not a nicety. A rewrite in place (open with O_TRUNC,
	// then write) exposes a window in which the record is a zero-length file,
	// and the guard reading it then refuses a commit over content that is
	// perfectly well blessed. Replacement is checked by file identity rather
	// than by racing a reader: os.SameFile is false across a rename and true
	// across an in-place rewrite, on every platform, every time.
	dir := vtRepo(t)
	writeFile(t, dir, "a.txt", "a\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "commit", "-q", "-m", "base")
	record := filepath.Join(dir, ".workspace", "verified-tree")

	if err := recordVerifiedTree(dir); err != nil {
		t.Fatalf("first record: %v", err)
	}
	before, err := os.Stat(record)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, dir, "b.txt", "b\n")
	if err := recordVerifiedTree(dir); err != nil {
		t.Fatalf("second record: %v", err)
	}
	after, err := os.Stat(record)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("the record was rewritten in place — a reader can catch it empty")
	}
}
