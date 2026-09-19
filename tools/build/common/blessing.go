package common

// Blessing: what it means for this tree to have passed, and where that is
// recorded.
//
// ONE MEASUREMENT, ONE MEANING. A tree is blessed iff `integration` measured it
// and the judge passed it (T2170). Whoever ran that measurement — `bin/verify`,
// `bin/run integration`, or the flow asking `bin/run integration --verdict` for
// a verdict on an envelope it produced — records the blessing through
// blessIfPassed below, so no invocation path changes what "blessed" means. It
// used to mean whatever `bin/verify` happened to run, which is how a tree the
// gate had passed three times was still refused as unverified.
//
// This file is the writing end of the verified-tree contract (the workspace's
// docs/tool-contract.md §8): the record lives at .workspace/verified-tree, and
// bin/precommit-guard — installed by `workspace setup`, never built here —
// refuses a commit whose staged tree differs. The reading end lives in the
// workspace repository (precommitguard/verifiedtree.go), so the record's path is
// spelled at both ends — the contract, not duplicated logic: the two ends
// compute different things.
//
// THE IDENTITY FUNCTION BELOW IS THIS PROJECT'S, FOR NOW. T2170's end state puts
// it upstream — the flow defines the tree identity and how a passing measurement
// is recorded, and every managed project calls those primitives rather than
// carrying its own. promise-language/flow has no such surface yet and this
// module depends on neither it nor forge, so the primitive lives here and the
// callers are already shaped for the swap: treeIdentity/recordBlessing/
// clearBlessing become calls, and blessIfPassed's rule stays where it is.
//
// This is the content-addressed replacement for the time-based
// verify-before-commit rule the migration retires (T2003): the gate compares
// content against content, so a tree blessed last week and untouched is still
// blessed, and a tree edited five seconds after verify is not. promise's own
// gate stopped consulting the clock at T1962 — it compares a WorktreeHash — so
// this record is a *second* content identity of nearly the same tree, in the
// format the external reading end fixes. The two are not interchangeable, and
// the difference bites: WorktreeHash excludes tools/gates/baselines.json because
// a passing commit gate rewrites it (the workspace's, since this project's own
// was deleted), and a plain git tree id cannot (T2082).
//
// It is also what keeps the formatting guarantee (docs/gate-system.md Class 2):
// verify repairs — FormatGo, FormatPromiseFiles — before it measures, and the
// guard demands the staged tree equal the recorded one, so unformatted content
// cannot be committed by construction.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// verifiedTreeRecord is where the blessing is recorded: one git tree object id,
// newline terminated, in the gitignored per-checkout .workspace/ directory.
// Mirrored in the workspace's precommitguard package.
const verifiedTreeRecord = ".workspace/verified-tree"

// errTreeMoved is the refusal to bless content that no longer exists: the
// measurement passed, but not on the tree standing here now. It is a distinct
// error because it is not a verification failure — nothing is wrong with the
// code — and the recovery is to measure again rather than to fix anything.
var errTreeMoved = errors.New(
	"the tree changed while it was being measured, so nothing was blessed — measure again to bless the content as it now stands")

// recordPath is where the record lives in the checkout at repoRoot.
func recordPath(repoRoot string) string {
	return filepath.Join(repoRoot, filepath.FromSlash(verifiedTreeRecord))
}

// clearBlessing removes the record. Verify calls it before its first step so a
// run that dies mid-way leaves nothing blessed and an in-flight verify blesses
// nothing. An absent record is not an error.
func clearBlessing(repoRoot string) error {
	err := os.Remove(recordPath(repoRoot))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// blessIfPassed records the blessing when, and only when, a passing
// `integration` verdict describes the tree standing here now.
//
// ONE RULE, SPELLED ONCE, for every caller: bin/verify after its own in-process
// measurement, `bin/run integration` after judging what it measured, and
// `bin/run integration --verdict` after judging an envelope the flow handed it.
// Four conditions, and each exists:
//
//   - the gate is `integration`. Passing the parts is not passing the whole, and
//     only the whole may be cited (docs/gate-system.md) — a green `tested:go`
//     blesses nothing.
//   - the verdict passed. A gate reports numbers and never judges them, so the
//     blessing is the judging layer's to record and no gate can write one.
//   - the envelope carries a tree. It does not when the tree moved while it was
//     being measured (settledTree), which is the blessing half of T2008.
//   - that tree is still the one here. The envelope may have been produced by
//     another process minutes ago; an edit since then is content nothing
//     measured.
//
// Outside a git checkout there is nothing to record and nothing to gate, so it
// is a reported no-op — the guard still refuses on the absent record.
func blessIfPassed(repoRoot string, env Envelope, acceptable bool) error {
	if env.Gate != integrationGate || !acceptable {
		return nil
	}
	now, err := treeIdentity(repoRoot)
	if err != nil {
		return fmt.Errorf("reading the tree's identity: %w", err)
	}
	if now == "" {
		Progress().Println("    not a git checkout — no blessing to record")
		return nil
	}
	if env.Tree == "" || env.Tree != now {
		return errTreeMoved
	}
	return recordBlessing(repoRoot, now)
}

// settledTree is the identity to stamp on a run that began at beforeTree, and
// the reason when there is none to stamp. A measurement whose subject changed
// under it describes a tree nobody can act on: it is reported as a run that
// measured less than a full one, and it carries no identity, so nothing
// downstream can bless content it never saw.
//
// An empty beforeTree is not a failure — there was no identity to compare
// against (no git checkout, or it could not be read), and a gate that could not
// answer a question about blessing has still measured everything it measured.
func settledTree(repoRoot, beforeTree string) (tree, incomplete string) {
	if beforeTree == "" {
		return "", ""
	}
	after, err := treeIdentity(repoRoot)
	if err != nil || after == "" {
		return "", "the tree's identity could not be read after the measurement, so this run blesses nothing"
	}
	if after != beforeTree {
		return "", "the tree changed while it was being measured, so this run blesses nothing"
	}
	return beforeTree, ""
}

// treeIdentity is the id of the tracked content as it stands: exactly the tree
// `git add -A` would stage, computed over a temporary index so the real index is
// untouched.
//
// The temp index is seeded from a COPY of the real one, because that is the
// tracked set the real `git add -A` starts from. Ignore rules apply only to
// untracked paths, so any other seed gets the ignored-and-tracking-state-differs
// cases wrong — an empty seed drops a tracked-but-ignored file, and a HEAD seed
// both drops one force-added but not yet committed and keeps one just
// `git rm --cached`ed — producing a tree no `git add -A` can stage, a mismatch
// re-running verify cannot repair.
//
// Outside a git checkout it returns an empty id and no error: there is no
// identity there, which is an answer rather than a failure.
func treeIdentity(repoRoot string) (string, error) {
	if _, err := gitWithIndex(repoRoot, "", "rev-parse", "--git-dir"); err != nil {
		return "", nil
	}

	tmpDir, err := os.MkdirTemp("", "verified-tree-")
	if err != nil {
		return "", fmt.Errorf("creating temp index dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	index := filepath.Join(tmpDir, "index")

	// A repo before its first add has no index file yet, and an empty seed is
	// exactly its tracked set.
	realIndex, err := gitWithIndex(repoRoot, "", "rev-parse", "--git-path", "index")
	if err != nil {
		return "", fmt.Errorf("locating the index: %w", err)
	}
	if !filepath.IsAbs(realIndex) {
		realIndex = filepath.Join(repoRoot, realIndex)
	}
	if data, err := os.ReadFile(realIndex); err == nil {
		if err := os.WriteFile(index, data, 0o600); err != nil {
			return "", fmt.Errorf("seeding temp index: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("seeding temp index: %w", err)
	}
	if _, err := gitWithIndex(repoRoot, index, "add", "-A"); err != nil {
		return "", fmt.Errorf("staging into temp index: %w", err)
	}
	tree, err := gitWithIndex(repoRoot, index, "write-tree")
	if err != nil {
		return "", fmt.Errorf("computing verified tree: %w", err)
	}
	return tree, nil
}

// recordBlessing writes the blessed id. It runs only after a passing verdict on
// this exact content, so the id is of the post-repair tree (verify repairs —
// gofmt -w, promise format — before it measures).
func recordBlessing(repoRoot, tree string) error {
	if err := ensureRecordIgnored(repoRoot); err != nil {
		return err
	}
	record := recordPath(repoRoot)
	dir := filepath.Dir(record)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	// Atomic: temp file + rename, so no reader ever sees a half-written record.
	// The temp file is created in the record's own directory, since a rename
	// is only atomic within a filesystem.
	tmp, err := os.CreateTemp(dir, ".verified-tree-*")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(tree + "\n"); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), record); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// ensureRecordIgnored refuses to record when the record's own path is not
// gitignored. Every project the guard covers is required to carry that entry
// (the workspace's tool-contract §3), and it is load-bearing at this end: an
// un-ignored record is part of what `git add -A` stages, so overwriting it
// after the tree has been measured leaves a blessed id no commit can ever
// match — and re-running verify, the recovery the guard names, reproduces it
// exactly. Failing here by name is the difference between a one-line
// .gitignore fix and an unreadable permanent refusal.
//
// Only an unambiguous "not ignored" (exit 1) counts as broken; a probe that
// cannot answer at all is not grounds to redden an otherwise green verify.
func ensureRecordIgnored(repoRoot string) error {
	cmd := exec.Command("git", "check-ignore", "-q", "--", verifiedTreeRecord)
	cmd.Dir = repoRoot
	err := cmd.Run()
	if err == nil {
		return nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return fmt.Errorf("%s is not gitignored — add \"/.workspace/\" to .gitignore. "+
			"Until it is, the record is part of what `git add -A` stages and no commit can match the blessed tree",
			verifiedTreeRecord)
	}
	return nil
}

// gitWithIndex runs git in dir, with GIT_INDEX_FILE pointed at indexFile when
// one is given, and returns trimmed stdout. The helpers in exec.go carry no
// environment, which is the one thing this needs. Stderr is captured into the
// error so it never leaks to the terminal.
func gitWithIndex(dir, indexFile string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if indexFile != "" {
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+indexFile)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %s", args[0], msg)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}
