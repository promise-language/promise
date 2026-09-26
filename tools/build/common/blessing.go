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
// THE TREE HASH IS THE FLOW'S, NOT THIS PROJECT'S. flow/pkg/verifiedtree
// defines how the id is computed, recorded and checked; this file holds only the
// POLICY — which measurement earns a blessing — which that package deliberately
// leaves to its caller. The fact has more than one reader (a project's verify
// writes the record, the workspace's commit guard reads it, a runner joins an
// earlier execution), and no reader may trust another's arithmetic: five
// hand-rolled copies of the same hash is what the primitive was created to end
// (flow#423). This project used to carry the sixth.
//
// So the two ends of the workspace's docs/tool-contract.md §8 contract now
// AGREE BY CONSTRUCTION rather than by two prose descriptions that happened to
// match: the record's path and format come from Path/Bless, not from a constant
// spelled here and again in precommitguard.
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
	"context"
	"errors"
	"fmt"

	"github.com/promise-language/flow/pkg/verifiedtree"
)

// errTreeMoved is the refusal to bless content that no longer exists: the
// measurement passed, but not on the tree standing here now. It is a distinct
// error because it is not a verification failure — nothing is wrong with the
// code — and the recovery is to measure again rather than to fix anything.
var errTreeMoved = errors.New(
	"the tree changed while it was being measured, so nothing was blessed — measure again to bless the content as it now stands")

// blessIfPassed records the blessing when, and only when, a passing
// `integration` verdict describes the tree standing here now.
//
// ONE RULE, SPELLED ONCE, for every caller: bin/verify after its own in-process
// measurement, `bin/run integration` after judging what it measured, and
// `bin/run integration --verdict` after judging an envelope the flow handed it.
// Three conditions are this project's to apply, and each exists:
//
//   - the gate is `integration`. Passing the parts is not passing the whole, and
//     only the whole may be cited (docs/gate-system.md) — a green `tested:go`
//     blesses nothing.
//   - the verdict passed. A gate reports numbers and never judges them, so the
//     blessing is the judging layer's to record and no gate can write one.
//   - the envelope carries a tree. It does not when the tree moved while it was
//     being measured (settledTree), which is the blessing half of T2008.
//
// The fourth — that the envelope's tree is still the one here — is
// BlessIfEqual's, and asking it to compare is the point: the envelope may have
// been produced by another process minutes ago, and a read-then-write here would
// be this project doing the arithmetic again. A false `ok` is not an error; the
// checkout simply moved, which is errTreeMoved's case.
//
// Outside a git checkout there is nothing to record and nothing to gate, so it
// is a reported no-op — the guard still refuses on the absent record.
func blessIfPassed(repoRoot string, env Envelope, acceptable bool) error {
	if env.Gate != integrationGate || !acceptable {
		return nil
	}
	// Is there anything to gate here at all? This read asks THAT and nothing
	// else — the comparison stays BlessIfEqual's. It is needed because an empty
	// env.Tree means two different things, and BlessIfEqual refuses one of them
	// as a caller error rather than answering it: a directory that is not a
	// checkout (nothing to record, nothing to gate) and a tree that moved while
	// it was being measured (a refusal). Handing "" straight to the primitive
	// would report the first as the second.
	here, err := treeIdentity(repoRoot)
	if err != nil {
		return fmt.Errorf("reading the tree's identity: %w", err)
	}
	if here == "" {
		Progress().Println("    not a git checkout — no blessing to record")
		return nil
	}
	if env.Tree == "" {
		return errTreeMoved
	}
	blessed, err := verifiedtree.BlessIfEqual(context.Background(), repoRoot, env.Tree)
	if err != nil {
		return fmt.Errorf("recording the blessing: %w", err)
	}
	if !blessed {
		return errTreeMoved
	}
	return nil
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

// treeIdentity is verifiedtree.TreeID with this project's answer for a root that
// is not a checkout: an empty id and no error.
//
// It computes nothing. The flow owns the hash; what is decided here is that a
// gate run outside a checkout still MEASURES — there is no commit to gate there,
// so having no identity is an answer rather than a failure, and a run that could
// not say anything about blessing has still reported every number it took.
func treeIdentity(repoRoot string) (string, error) {
	tree, err := verifiedtree.TreeID(context.Background(), repoRoot)
	if errors.Is(err, verifiedtree.ErrNotACheckout) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return tree, nil
}
