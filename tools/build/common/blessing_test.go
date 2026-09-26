package common

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/flow/pkg/verifiedtree"
)

// What is tested here is this project's POLICY — which measurement earns a
// blessing, and who records it — not how a tree id is computed or written.
// That is flow/pkg/verifiedtree's, and it carries its own suite for it; a copy
// here would be this project asserting another module's arithmetic, which is
// the duplication the primitive was created to end (flow#423).
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

func recordedTree(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".workspace", "verified-tree"))
	if err != nil {
		t.Fatalf("read the verified-tree record: %v", err)
	}
	return strings.TrimSpace(string(data))
}

func TestRunVerifyRedRunLeavesNothingBlessed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows

	dir := t.TempDir()
	writeFile(t, dir, ".workspace/verified-tree", "stale-blessing\n")

	// The run points the cache at its own root (there is no --shared any more),
	// and does it with os.Setenv — process-global, so the values are declared
	// here to be restored for the rest of this package's tests.
	t.Setenv("PROMISE_HOME", filepath.Join(dir, ".promise-home"))
	t.Setenv("TMPDIR", filepath.Join(dir, ".promise-home", "tmp"))

	err := RunVerify(dir, []string{"--lock-timeout=30s"})
	if err == nil {
		t.Fatal("verify over an empty root should fail")
	}
	if !strings.Contains(err.Error(), "no .g4 files") {
		t.Errorf("verify should fail on the absent grammar, got: %v", err)
	}
	if Exists(AntlrJarPath(dir)) {
		t.Errorf("verify fetched the ANTLR jar into %s — no build path may reach the network (T2116)", dir)
	}
	if Exists(filepath.Join(dir, ".workspace", "verified-tree")) {
		t.Error("a red run must leave nothing blessed — the stale record survived")
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

	err := RunVerify(dir, []string{"--lock-timeout=30s"})
	if err == nil {
		t.Fatal("verify must fail when the stale record cannot be cleared")
	}
	if !strings.HasPrefix(err.Error(), "clear: ") {
		t.Errorf("verify must fail at the clear, not later, got: %v", err)
	}
}

func blessedEnvelope(tree string) Envelope {
	return Envelope{
		SchemaVersion: EnvelopeSchemaVersion,
		Gate:          integrationGate,
		Target:        HostTarget(),
		Tree:          tree,
	}
}

// TestBlessIfPassed_RecordsOnlyAPassingIntegrationVerdictOnThisTree is the whole
// rule, one case per condition. Each exists because dropping it blesses
// something nobody measured: another gate's green (passing the parts is not
// passing the whole), a red verdict, or a tree that has moved since.
//
// The green case is the one nothing covered before (T2083): only the red path
// was pinned, so a change that stopped recording altogether would have kept
// every test green and refused every commit.
func TestBlessIfPassed_RecordsOnlyAPassingIntegrationVerdictOnThisTree(t *testing.T) {
	for _, tc := range []struct {
		name       string
		gate       string
		acceptable bool
		tree       func(here string) string
		wantRecord bool
		wantErr    error
	}{
		{"green integration", integrationGate, true, func(here string) string { return here }, true, nil},
		{"red integration", integrationGate, false, func(here string) string { return here }, false, nil},
		{"green part", "tested:go", true, func(here string) string { return here }, false, nil},
		{"red part", "tested:go", false, func(here string) string { return here }, false, nil},
		{"no tree on the envelope", integrationGate, true, func(string) string { return "" }, false, errTreeMoved},
		{"a different tree", integrationGate, true, func(string) string { return "0000000000000000000000000000000000000000" }, false, errTreeMoved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := vtRepo(t)
			writeFile(t, dir, "a.txt", "a\n")
			vtGit(t, dir, "add", "-A")
			vtGit(t, dir, "commit", "-q", "-m", "base")
			here, err := treeIdentity(dir)
			if err != nil {
				t.Fatalf("treeIdentity: %v", err)
			}

			env := blessedEnvelope(tc.tree(here))
			env.Gate = tc.gate
			err = blessIfPassed(dir, env, tc.acceptable)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("blessIfPassed = %v, want %v", err, tc.wantErr)
			}
			recorded := Exists(filepath.Join(dir, ".workspace", "verified-tree"))
			if recorded != tc.wantRecord {
				t.Errorf("recorded = %v, want %v", recorded, tc.wantRecord)
			}
			if tc.wantRecord && recordedTree(t, dir) != here {
				t.Errorf("recorded %q, want the tree that was measured %q", recordedTree(t, dir), here)
			}
		})
	}
}

// TestBlessIfPassed_OutsideAGitCheckoutIsANoOp: there is no commit to gate
// there, so a run that passed reports rather than fails.
func TestBlessIfPassed_OutsideAGitCheckoutIsANoOp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	if id, err := treeIdentity(dir); err != nil || id != "" {
		t.Fatalf("precondition: %s is still inside a git checkout (id %q, err %v)", dir, id, err)
	}
	if err := blessIfPassed(dir, blessedEnvelope(""), true); err != nil {
		t.Errorf("blessIfPassed outside a checkout = %v, want a reported no-op", err)
	}
	if Exists(filepath.Join(dir, ".workspace", "verified-tree")) {
		t.Error("no record should be written outside a git checkout")
	}
}

// TestSettledTree_OnlyAnUnmovedTreeCarriesAnIdentity is the blessing half of
// T2008: a measurement whose subject changed under it describes a tree nobody
// can act on, so it carries no identity and is reported as having measured less
// than a full run.
func TestSettledTree_OnlyAnUnmovedTreeCarriesAnIdentity(t *testing.T) {
	dir := vtRepo(t)
	writeFile(t, dir, "a.txt", "a\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "commit", "-q", "-m", "base")
	before, err := treeIdentity(dir)
	if err != nil {
		t.Fatalf("treeIdentity: %v", err)
	}

	tree, incomplete := settledTree(dir, before)
	if tree != before || incomplete != "" {
		t.Errorf("an untouched tree = (%q, %q), want (%q, \"\")", tree, incomplete, before)
	}

	// An edit while the measurement was running.
	writeFile(t, dir, "a.txt", "edited during the run\n")
	tree, incomplete = settledTree(dir, before)
	if tree != "" {
		t.Errorf("a tree that moved must carry no identity, got %q", tree)
	}
	if !strings.Contains(incomplete, "changed while it was being measured") {
		t.Errorf("incomplete = %q, want it to say the tree changed", incomplete)
	}

	// No identity to compare against is not a failure — it is a run with
	// nothing to say about blessing, which every other measurement survives.
	if tree, incomplete := settledTree(dir, ""); tree != "" || incomplete != "" {
		t.Errorf("settledTree with no starting identity = (%q, %q), want two empties", tree, incomplete)
	}
}

// TestBlessing_IsTheSameFromVerifyAndFromTheJudge is the agreement T2170 is
// for: one passing `integration` measurement of a tree blesses it, and which
// party recorded it changes nothing. Verify judges in-process and calls
// blessIfPassed; the flow runs the gate itself and asks `bin/run integration
// --verdict`, which reaches the same rule through JudgeStdin.
func TestBlessing_IsTheSameFromVerifyAndFromTheJudge(t *testing.T) {
	dir := vtRepo(t)
	writeFile(t, dir, "a.txt", "a\n")
	// The judging layer loads this project's terms from the tree it judges. One
	// cap, satisfied, so the verdict is fixed at green and what varies is only
	// who records the blessing. It may NOT be an empty manifest: a gate whose
	// metrics carry no terms is refused, so judging nothing would fix the
	// verdict at red and this test would pass for the wrong reason.
	writeFile(t, dir, ThresholdsFile, `{"host_test_failures": {"direction": "at_most", "cap": 0}}`+"\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "commit", "-q", "-m", "base")
	here, err := treeIdentity(dir)
	if err != nil {
		t.Fatalf("treeIdentity: %v", err)
	}
	env := blessedEnvelope(here)
	env.Metrics = []Metric{Count("host_test_failures", 0)}

	// Verify's path.
	if err := blessIfPassed(dir, env, true); err != nil {
		t.Fatalf("blessIfPassed: %v", err)
	}
	fromVerify := recordedTree(t, dir)

	// The flow's path, over the same unchanged tree.
	if err := verifiedtree.Clear(dir); err != nil {
		t.Fatalf("clearing the blessing: %v", err)
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var verdict bytes.Buffer
	if err := JudgeStdin(dir, integrationGate, bytes.NewReader(body), &verdict); err != nil {
		t.Fatalf("JudgeStdin: %v", err)
	}
	if !strings.Contains(verdict.String(), `"acceptable":true`) {
		t.Fatalf("precondition: the verdict must pass, got %s", verdict.String())
	}
	fromJudge := recordedTree(t, dir)

	if fromVerify != fromJudge {
		t.Errorf("verify blessed %q and the judge blessed %q — one tree, one blessing", fromVerify, fromJudge)
	}
	if fromJudge != here {
		t.Errorf("blessed %q, want the measured tree %q", fromJudge, here)
	}
}

// TestJudgeStdin_ARedVerdictBlessesNothing: the judging layer records for a
// pass and only for a pass, whichever mode it was asked in.
func TestJudgeStdin_ARedVerdictBlessesNothing(t *testing.T) {
	dir := vtRepo(t)
	writeFile(t, dir, "a.txt", "a\n")
	// One cap the envelope's own metric cannot satisfy.
	writeFile(t, dir, ThresholdsFile, `{"host_test_failures": {"direction": "at_most", "cap": 0}}`+"\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "commit", "-q", "-m", "base")
	here, err := treeIdentity(dir)
	if err != nil {
		t.Fatalf("treeIdentity: %v", err)
	}
	env := blessedEnvelope(here)
	env.Metrics = []Metric{Count("host_test_failures", 3)}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}

	var verdict bytes.Buffer
	if err := JudgeStdin(dir, integrationGate, bytes.NewReader(body), &verdict); err != nil {
		t.Fatalf("JudgeStdin: %v", err)
	}
	if !strings.Contains(verdict.String(), `"acceptable":false`) {
		t.Fatalf("precondition: the verdict must fail, got %s", verdict.String())
	}
	if Exists(filepath.Join(dir, ".workspace", "verified-tree")) {
		t.Error("a failing verdict blessed the tree")
	}
}

// TestBlessIfPassed_ARecordThatCannotBeWrittenIsNotAMovedTree pins the
// difference between the two ways a green verdict can end with nothing
// recorded, because the recoveries are opposites.
//
// A checkout that does not ignore the record's path cannot be blessed at all:
// the record would be part of what `git add -A` stages, so writing it would
// change the very tree it names and no commit could ever match. The primitive
// refuses, and this project must SURFACE that refusal — reported as
// errTreeMoved it would read as "measure again", and measuring again reproduces
// it exactly, forever, over a defect one .gitignore line fixes. Nothing may be
// recorded either way.
func TestBlessIfPassed_ARecordThatCannotBeWrittenIsNotAMovedTree(t *testing.T) {
	dir := vtInit(t) // deliberately no .gitignore: .workspace/ is not ignored
	writeFile(t, dir, "a.txt", "a\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "commit", "-q", "-m", "base")
	here, err := treeIdentity(dir)
	if err != nil {
		t.Fatalf("treeIdentity: %v", err)
	}

	err = blessIfPassed(dir, blessedEnvelope(here), true)
	if err == nil {
		t.Fatal("a record that cannot be written must fail the run, not pass quietly")
	}
	if errors.Is(err, errTreeMoved) {
		t.Errorf("reported as a moved tree, which sends the reader to re-measure: %v", err)
	}
	if !strings.Contains(err.Error(), ".workspace/verified-tree") {
		t.Errorf("the failure must name the path it could not write, got: %v", err)
	}
	if Exists(filepath.Join(dir, ".workspace", "verified-tree")) {
		t.Error("nothing may be recorded when the record's own path is not ignored")
	}
}

// TestSettledTree_ACheckoutThatVanishedMidRunStampsNothing covers the other way
// the after-reading can fail to answer: the run began with an identity and the
// checkout is gone by the time it ends.
//
// It must report that it measured less than a full run rather than stamp the id
// it started with — a stamp there would let a later verdict bless content this
// run can no longer describe.
func TestSettledTree_ACheckoutThatVanishedMidRunStampsNothing(t *testing.T) {
	dir := vtRepo(t)
	writeFile(t, dir, "a.txt", "a\n")
	vtGit(t, dir, "add", "-A")
	vtGit(t, dir, "commit", "-q", "-m", "base")
	before, err := treeIdentity(dir)
	if err != nil || before == "" {
		t.Fatalf("precondition: treeIdentity = (%q, %v)", before, err)
	}

	// GIT_CEILING_DIRECTORIES stops the upward walk at this test's own temp
	// parent, so removing .git leaves a directory that is not a checkout rather
	// than one inside whatever encloses TMPDIR.
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("removing .git: %v", err)
	}

	tree, incomplete := settledTree(dir, before)
	if tree != "" {
		t.Errorf("a run whose checkout vanished must stamp nothing, got %q", tree)
	}
	if !strings.Contains(incomplete, "could not be read") {
		t.Errorf("incomplete = %q, want it to say the identity could not be read", incomplete)
	}
}

// TestStepRecord_BlessesOnTheRunsOwnVerdict is the wiring between the pipeline
// and the rule: verify's `record` step must hand blessIfPassed the envelope and
// the verdict THIS run produced.
//
// It is one line, and that is exactly why it is pinned. A transposed or
// hard-coded argument there — `true` for r.acceptable, an envelope from
// anywhere else — is invisible to every test of blessIfPassed itself, and it
// blesses a tree the run judged red.
func TestStepRecord_BlessesOnTheRunsOwnVerdict(t *testing.T) {
	// recorded is what the step left behind, and measured is the id the run
	// judged — a green step must record exactly that one.
	record := func(acceptable bool) (recorded, measured string, err error) {
		t.Helper()
		dir := vtRepo(t)
		writeFile(t, dir, "a.txt", "a\n")
		vtGit(t, dir, "add", "-A")
		vtGit(t, dir, "commit", "-q", "-m", "base")
		measured, err = treeIdentity(dir)
		if err != nil {
			t.Fatalf("treeIdentity: %v", err)
		}
		r := &verifyRun{root: dir, env: blessedEnvelope(measured), acceptable: acceptable}
		if err := r.stepRecord(); err != nil {
			return "", measured, err
		}
		if !Exists(filepath.Join(dir, ".workspace", "verified-tree")) {
			return "", measured, nil
		}
		return recordedTree(t, dir), measured, nil
	}

	recorded, measured, err := record(true)
	if err != nil {
		t.Fatalf("a green run's record step: %v", err)
	}
	if recorded != measured {
		t.Errorf("a green run recorded %q, want the tree it judged %q", recorded, measured)
	}

	if recorded, _, err := record(false); err != nil || recorded != "" {
		t.Errorf("a red run = (%q, %v), want it to record nothing and not fail", recorded, err)
	}
}
