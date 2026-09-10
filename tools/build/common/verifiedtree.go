package common

// This file is the writing end of the verified-tree contract (the workspace's
// docs/tool-contract.md §8): bin/verify records the tree it blessed at
// .workspace/verified-tree, and bin/precommit-guard — installed by `workspace
// setup`, never built here — refuses a commit whose staged tree differs. The
// reading end lives in the workspace repository (precommitguard/verifiedtree.go),
// so the record's path is spelled at both ends — the contract, not duplicated
// logic: the two ends compute different things.
//
// This is the content-addressed replacement for the time-based
// verify-before-commit rule the migration retires (T2003): the gate compares
// content against content, so a tree blessed last week and untouched is still
// blessed, and a tree edited five seconds after verify is not. promise's own
// gate stopped consulting the clock at T1962 — bin/commitgate compares a
// WorktreeHash — so this record is a *second* content identity of nearly the
// same tree, in the format the external reading end fixes. The two are not
// interchangeable, and the difference bites: WorktreeHash excludes
// tools/gates/baselines.json because a passing commit gate rewrites it, and a
// plain git tree id cannot (T2082).
//
// It is also what keeps the formatting guarantee (docs/gate-system.md
// Class 2): verify repairs — FormatGo, FormatPromiseFiles — before it records,
// and the guard demands the staged tree equal the recorded one, so unformatted
// content cannot be committed by construction.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// verifiedTreeRecord is where verify records the tree it blessed: one git
// tree object id, newline terminated, in the gitignored per-checkout
// .workspace/ directory. Mirrored in the workspace's precommitguard package.
const verifiedTreeRecord = ".workspace/verified-tree"

// recordPath is where the record lives in the checkout at repoRoot.
func recordPath(repoRoot string) string {
	return filepath.Join(repoRoot, filepath.FromSlash(verifiedTreeRecord))
}

// clearVerifiedTree removes the record. Verify calls it before its first step
// so a run that dies mid-way leaves nothing blessed and an in-flight verify
// blesses nothing. An absent record is not an error.
func clearVerifiedTree(repoRoot string) error {
	err := os.Remove(recordPath(repoRoot))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// recordVerifiedTree writes the tree id of the content verify just blessed.
// It runs only after every other step has passed, so the id is of the
// post-repair tree (verify repairs — gofmt -w, promise format — before it
// measures).
//
// The tree is computed over a temporary index so the real index is untouched,
// and it is exactly what `git add -A` would stage: seeded from a copy of the
// real index, because that is the tracked set the real `git add -A` starts
// from. Ignore rules apply only to untracked paths, so any other seed gets
// the ignored-and-tracking-state-differs cases wrong — an empty seed drops a
// tracked-but-ignored file, and a HEAD seed both drops one force-added but
// not yet committed and keeps one just `git rm --cached`ed — recording a tree
// no `git add -A` can stage, a mismatch re-running verify cannot repair.
//
// Outside a git checkout, recording is a reported no-op rather than a verify
// failure: there is no commit to gate there, and the guard still refuses on
// the absent record.
func recordVerifiedTree(repoRoot string) error {
	if _, err := gitWithIndex(repoRoot, "", "rev-parse", "--git-dir"); err != nil {
		Progress().Println("    not a git checkout — no verified-tree record to write")
		return nil
	}
	if err := ensureRecordIgnored(repoRoot); err != nil {
		return err
	}

	tmpDir, err := os.MkdirTemp("", "verified-tree-")
	if err != nil {
		return fmt.Errorf("creating temp index dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	index := filepath.Join(tmpDir, "index")

	// Seed from a copy of the real index; a repo before its first add has no
	// index file yet, and an empty seed is exactly its tracked set.
	realIndex, err := gitWithIndex(repoRoot, "", "rev-parse", "--git-path", "index")
	if err != nil {
		return fmt.Errorf("locating the index: %w", err)
	}
	if !filepath.IsAbs(realIndex) {
		realIndex = filepath.Join(repoRoot, realIndex)
	}
	if data, err := os.ReadFile(realIndex); err == nil {
		if err := os.WriteFile(index, data, 0o600); err != nil {
			return fmt.Errorf("seeding temp index: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("seeding temp index: %w", err)
	}
	if _, err := gitWithIndex(repoRoot, index, "add", "-A"); err != nil {
		return fmt.Errorf("staging into temp index: %w", err)
	}
	tree, err := gitWithIndex(repoRoot, index, "write-tree")
	if err != nil {
		return fmt.Errorf("computing verified tree: %w", err)
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
