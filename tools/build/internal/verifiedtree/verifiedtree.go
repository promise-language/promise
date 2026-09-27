// Package verifiedtree is a 1:1 copy of forge's primitives/verifiedtree, taken
// because this project may not depend on it.
//
// WHY THE COPY EXISTS. The blessing belongs to the `integration` gate: only the
// party that measures the whole may record that it passed, and this project
// supplies that gate — the workspace does not, so it cannot bless. T2170 wired
// the record through `github.com/promise-language/flow/pkg/verifiedtree`, a
// façade over forge. Both modules are private, so `./make` could not build on
// any machine without the maintainer's credentials: it worked on a developer
// box with a warm module cache and a keychain, and failed at bootstrap on every
// GitHub runner, breaking ci.yml and both release.yml jobs (T2240, T2241).
// Making either module public or vendoring it is not available, and the
// dependency should not have been there — this project consumes every other
// piece of flow as a SUPPLIED BINARY (tool-guard, precommit-guard, do, issue,
// workspace), never as a Go module.
//
// WHY IT IS COPIED RATHER THAN REWRITTEN. The fact has more than one reader and
// no reader may trust another's arithmetic: this project's verify writes the
// record, and the workspace-supplied precommit-guard — built from forge —
// refuses any commit whose staged tree differs from it. A paraphrase that
// differed in any detail would be a permanent silent refusal, exactly as forge's
// own doc warns: verify writes one thing, the guard reads another, always finds
// it absent, and the named recovery (run bin/verify) cannot clear a check verify
// does not participate in. So this is copied, not reimplemented, and it must
// stay byte-identical in behaviour to forge's.
//
// TWO MECHANICAL CHANGES, AND NO OTHERS: the `forge/primitives` import is
// dropped, and `primitives.VerifiedTreeRecord` becomes the VerifiedTreeRecord
// constant below with the identical value. Everything else — the tree-id
// algorithm, the record format, the atomic write, the error shapes — is forge's
// code unchanged.
//
// IF YOU CHANGE ANYTHING HERE, YOU HAVE FORKED THE FORMAT. The reading end is
// in another repository and will not change with you.
//
// ---- what follows is forge's own documentation, unaltered ----
//
// Package verifiedtree is the tree blessing: how the tree id a green verify
// records is computed, how the record is written, and how a party reading it
// decides whether a tree is blessed (docs/project-tools.md, One
// implementation). It is the one implementation of the tree id, the record and
// the check; no project computes a tree id for blessing or for execution
// identity, nor reads or writes the record, except through it.
//
// IT EXISTS BECAUSE THE FACT HAS MORE THAN ONE READER AND NO READER MAY TRUST
// ANOTHER'S ARITHMETIC. A project's bin/verify writes the record, a commit
// guard refuses any commit whose staged tree differs from it, and a party
// deciding whether the gate needs to run at all compares it against the
// worktree. Those are programs in different repositories answering one
// question — was this tree verified? — and while each computed its own tree id
// they could disagree with nothing able to reconcile them.
//
// THE POLICY STAYS WITH THE CALLER. This package knows nothing about verify
// pipelines, commits or runs: it takes a checkout root and answers about trees
// and the record. Whether a missing checkout is a failure or a reported no-op,
// what a refusal says to a person, and when to bless are the caller's.
package verifiedtree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// VerifiedTreeRecord is where a project's verify records the blessed tree, and
// where a commit guard reads it. forge holds the original as
// primitives.VerifiedTreeRecord; this is the same value, spelled here because
// that module is unreachable from this one.
//
// Drift between the two ends is a permanent, silent refusal rather than a
// visible failure: verify writes one path, the guard reads another and always
// finds it absent, and the guard's named recovery — run bin/verify — cannot
// clear a check that verify does not participate in.
//
// The value is a slash-separated path relative to a repository root, which is
// how both ends carry it in prose and how a .gitignore names it. A caller joins
// it onto the root and converts it for the host:
// filepath.Join(root, filepath.FromSlash(VerifiedTreeRecord)).
const VerifiedTreeRecord = ".workspace/verified-tree"

// ErrNotACheckout is returned for a root that is not inside a git checkout.
// There is no tree to hash there and no commit to gate, so a caller decides
// what that means — a verify outside a checkout reports it and carries on,
// while a guard has nothing to guard.
var ErrNotACheckout = errors.New("not a git checkout")

// ErrRecordNotIgnored refuses a bless in a checkout that does not ignore the
// record's path. An unignored record is inside its own subject: writing it
// changes the tree it names, so the tree it blesses stops existing the moment
// it is blessed, and every commit is then refused with nothing the caller can
// do about it. Refusing by name at the bless is the only moment the cause is
// still visible.
var ErrRecordNotIgnored = errors.New("the verified-tree record's path is not ignored by this checkout")

// Option adjusts where a call writes its scratch and how it starts git. The
// defaults suit a caller with no rules of its own about either.
type Option func(*config)

type config struct {
	scratch string
	start   func(*exec.Cmd) error
}

// WithScratch creates the temporary index under dir rather than the system's
// temporary directory, for a caller that writes nothing outside its checkout.
// The directory is not made here; it is the caller's.
func WithScratch(dir string) Option {
	return func(c *config) { c.scratch = dir }
}

// WithStart runs each git child through start, which receives the command
// with its streams and environment already set and returns what running it
// returned. It is how a caller that records every child it starts keeps that
// true of the children started on its behalf.
func WithStart(start func(*exec.Cmd) error) Option {
	return func(c *config) { c.start = start }
}

func configure(opts []Option) config {
	c := config{start: (*exec.Cmd).Run}
	for _, o := range opts {
		o(&c)
	}
	return c
}

// Path is the record's path in the checkout rooted at root. The location is
// one constant, VerifiedTreeRecord — forge imports it from primitives; this
// copy spells it above, for the reason the package doc gives.
func Path(root string) string {
	return filepath.Join(root, filepath.FromSlash(VerifiedTreeRecord))
}

// TreeID is the git tree object id of what `git add -A` would stage in the
// checkout rooted at root: tracked changes plus untracked-but-not-ignored
// files. It is the content a blessing is about, and what a party holding a gate
// result keys that result to.
//
// The real index is never touched: the id is computed over a COPY of it. The
// copy is the seed rather than an empty index or HEAD, because that is the
// tracked set the real `git add -A` starts from and ignore rules apply only to
// untracked paths. An empty seed drops a tracked-but-ignored file, and a HEAD
// seed both drops one force-added but not yet committed and keeps one just
// `git rm --cached`ed.
//
// A zero-length index file is not copied: git creates the index it needs, but
// seeded with zero bytes it refuses every command with "index file smaller
// than expected", so copying one turns a recoverable state into a failure.
func TreeID(ctx context.Context, root string, opts ...Option) (string, error) {
	c := configure(opts)
	if err := requireCheckout(ctx, c, root); err != nil {
		return "", err
	}
	return treeID(ctx, c, root)
}

// StagedTreeID is `git write-tree` over root's REAL index: the tree the commit
// about to be made would record. It is what a commit-time check compares the
// record against.
//
// An index that cannot be written as a tree — an unmerged one, mid-conflict —
// is an error rather than an empty answer. A check that cannot run must fail
// rather than pass.
func StagedTreeID(ctx context.Context, root string, opts ...Option) (string, error) {
	c := configure(opts)
	if err := requireCheckout(ctx, c, root); err != nil {
		return "", err
	}
	return git(ctx, c, root, "", "write-tree")
}

// Check reports the tree the record blesses, or "" when nothing does.
//
// Absent, blank and malformed are one answer deliberately: no verify has run,
// one is in flight having cleared the record, or a write was interrupted — and
// to every reader each means nothing has blessed this tree. A record that
// exists and cannot be read is an error, not that answer.
func Check(root string) (string, error) {
	raw, err := os.ReadFile(Path(root))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return readRecord(raw), nil
}

// Blesses reports whether a record read by Check blesses tree.
//
// IT IS NOT `==`. The record is "" when nothing has blessed the tree, and a
// caller's tree id is "" when computing it failed; compared with `==` those two
// failures agree, and a checkout nothing has verified reads as blessed.
func Blesses(record, tree string) bool {
	return record != "" && record == tree
}

// Bless records root's current tree id and returns it. The write is atomic — a
// temporary file in the record's own directory, renamed over — and the
// directory is created if it is missing.
func Bless(ctx context.Context, root string, opts ...Option) (string, error) {
	return bless(ctx, configure(opts), root, "")
}

// BlessIfEqual records root's current tree id only when it equals want, and
// reports whether it did. It is what lets a party holding a green result for
// tree want record it without running anything, and without blessing content
// the checkout has moved on to since.
//
// A mismatch is not an error and leaves any existing record as it was. An
// empty want is refused: it is a tree id its caller failed to compute, and the
// unconditional form is Bless.
func BlessIfEqual(ctx context.Context, root, want string, opts ...Option) (bool, error) {
	if want == "" {
		return false, errors.New("verifiedtree: BlessIfEqual needs the tree the result is about; the unconditional form is Bless")
	}
	blessed, err := bless(ctx, configure(opts), root, want)
	return blessed != "", err
}

// Clear removes the record. A verify calls it before its first step, so a run
// that dies part way leaves nothing blessed. An absent record is success.
func Clear(root string) error {
	if err := os.Remove(Path(root)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// bless is the one implementation behind both blessing entry points. An empty
// want is the unconditional form. The returned tree id is "" exactly when want
// was given and did not match, or when nothing was written.
//
// The ignore check comes before the tree is computed, so a checkout that cannot
// hold a record is refused before anything is spent on one.
func bless(ctx context.Context, c config, root, want string) (string, error) {
	if err := requireCheckout(ctx, c, root); err != nil {
		return "", err
	}
	if err := requireRecordIgnored(ctx, c, root); err != nil {
		return "", err
	}
	tree, err := treeID(ctx, c, root)
	if err != nil {
		return "", err
	}
	if want != "" && tree != want {
		return "", nil
	}
	if err := writeRecord(root, tree); err != nil {
		return "", fmt.Errorf("writing %s: %w", VerifiedTreeRecord, err)
	}
	return tree, nil
}

// treeID is TreeID with the checkout already established.
func treeID(ctx context.Context, c config, root string) (string, error) {
	tmpDir, err := os.MkdirTemp(c.scratch, "verified-tree-")
	if err != nil {
		return "", fmt.Errorf("creating a directory for a copy of the index: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	index := filepath.Join(tmpDir, "index")

	// --git-path rather than a guess at <git-dir>/index: a linked worktree
	// keeps its own index under .git/worktrees/<name>/, and hashing the
	// superproject's would describe a tree nobody is working on.
	realIndex, err := git(ctx, c, root, "", "rev-parse", "--git-path", "index")
	if err != nil {
		return "", fmt.Errorf("locating the index: %w", err)
	}
	if !filepath.IsAbs(realIndex) {
		realIndex = filepath.Join(root, realIndex)
	}
	// Only an ABSENT index means the tracked set is empty; any other read
	// failure means it is unknown, and guessing it would name content no
	// `git add -A` here can stage.
	data, err := os.ReadFile(realIndex)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("seeding a copy of the index: %w", err)
	}
	if len(data) > 0 {
		if err := os.WriteFile(index, data, 0o600); err != nil {
			return "", fmt.Errorf("seeding a copy of the index: %w", err)
		}
	}

	if _, err := git(ctx, c, root, index, "add", "-A"); err != nil {
		return "", fmt.Errorf("staging into a copy of the index: %w", err)
	}
	tree, err := git(ctx, c, root, index, "write-tree")
	if err != nil {
		return "", fmt.Errorf("computing the tree id: %w", err)
	}
	return tree, nil
}

// readRecord reads the record's bytes as a tree id, or "" when they are not
// one. The format is one git tree object id, newline terminated, and nothing
// else.
func readRecord(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if !isObjectID(s) {
		return ""
	}
	return s
}

// isObjectID reports whether s is a lowercase hex git object id, at either hash
// length. Checking the shape keeps a torn or hand-edited record from being
// compared as though it were a tree.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// writeRecord writes tree to the record atomically: a temporary file in the
// record's own directory, then a rename. Same directory so the rename cannot
// cross a filesystem, which is the one way it stops being atomic.
func writeRecord(root, tree string) error {
	path := Path(root)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
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
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// requireCheckout turns "root is not in a git checkout" into ErrNotACheckout,
// so a caller can tell it apart from a git that failed for any other reason.
//
// Only git ANSWERING no — its fatal exit, 128 — is that answer. A git that
// could not be started, was refused by the caller's start, or was killed by a
// cancelled context has said nothing about root, and reading its failure as
// "no checkout here" turns a caller's no-op into a pass it never earned.
func requireCheckout(ctx context.Context, c config, root string) error {
	_, err := git(ctx, c, root, "", "rev-parse", "--git-dir")
	if err == nil {
		return nil
	}
	var exit *exec.ExitError
	if ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 128 {
		return fmt.Errorf("%s: %w", root, ErrNotACheckout)
	}
	return err
}

// requireRecordIgnored refuses when the checkout does not ignore the record's
// path. `git check-ignore` consults the index as well as the ignore rules, so a
// record that is TRACKED reports as not ignored — which is the answer wanted: a
// tracked record is inside the tree it would describe just as surely as an
// unignored untracked one.
func requireRecordIgnored(ctx context.Context, c config, root string) error {
	_, err := git(ctx, c, root, "", "check-ignore", "-q", "--", VerifiedTreeRecord)
	if err == nil {
		return nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return fmt.Errorf("%s: %w", VerifiedTreeRecord, ErrRecordNotIgnored)
	}
	return err
}

// git is the package's one git caller: it runs git in dir through the
// caller's start, with GIT_INDEX_FILE pointed at index when one is given, and
// returns its trimmed stdout. Stderr is captured into the error rather than
// passed through, because a caller deciding how to report a failure cannot do
// that for output that already reached the terminal. The exit error is wrapped,
// so a caller can still read the status.
func git(ctx context.Context, c config, dir, index string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if index != "" {
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
	}
	var out, errs bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errs
	if err := c.start(cmd); err != nil {
		if msg := strings.TrimSpace(errs.String()); msg != "" {
			return "", fmt.Errorf("git %s: %s: %w", args[0], msg, err)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(out.String()), nil
}
