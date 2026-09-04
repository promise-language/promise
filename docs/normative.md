# Normative Documents

> **Tag:** `normative` — remaining work to complete this document: `mcp__tracker__list --tag normative`

What makes a document in this repository binding, what one must contain, and the rules that keep
two of them from ever disagreeing. Its subject is the documents themselves; it is one of them, and
every rule below applies to it.

## 1. Location is the whole rule

There is no configuration file and no per-document marker. Which directory a file sits in
determines what it is:

| Location | What a file there is | Binding? |
|----------|----------------------|----------|
| `docs/` root | A **specification**: what the project *should* be — the intended end state. | **Yes.** |
| `docs/proposals/` | An end state that has **not been ratified** — a draft, an RFC, a direction still under discussion. | No. |
| `docs/archive/` | An end state that has been **superseded** — kept for history. | No. |
| `docs/research/` | Background analysis feeding a decision — an assessment, not a design. | No. |

**Ratifying a proposal is a `git mv` into the root**, plus the header of §2. Retiring a
specification is a `git mv` into `docs/archive/`. Nothing else marks the transition, so the move
*is* the decision and shows up as one reviewable diff.

[index.md](index.md) is the map of the tree and the one file in the root that is not a
specification. Every document must be listed there.

## 2. The header

A specification opens with its title, and on the line beneath it, its tag:

```markdown
# Large Integers

> **Tag:** `large-integers` — remaining work to complete this document: `mcp__tracker__list --tag large-integers`
```

The line is a blockquote, and sits directly under the title with a blank line either side.

The tag is always the file's basename, so the vocabulary is the directory listing and
[tags.md](tags.md) does not duplicate it. The line is not decoration: it is what makes §3 work.

## 3. A specification states the end state

**A specification describes what the project should be, never how far along it is.** It says
nothing about what is implemented, unimplemented, partially implemented, or implemented wrongly.
It contains no status section, no progress notes, no phasing, no "currently", "not yet",
"planned" or "implemented" — and **no inline markers naming a tracker item**, which are a status
section arriving one sentence at a time.

Status is not a property of a specification. It is a property of the *relationship* between the
specification and the implementation, and that relationship is recorded in the tracker (§7) — so
`mcp__tracker__list --tag <basename>` *is* the status section, always current, and never something
a reader has to trust prose about.

The practical test: **a specification should read identically the day before and the day after the
work that implements it.** If a sentence would have to change when an item closes, that sentence is
status and does not belong.

## 4. One fact, one home — supersession is forbidden

**A fact is specified in exactly one document.** Two specifications must never define the same
thing, and no specification may claim authority over another.

These are forbidden: *supersedes*, *takes precedence over*, *overrides*, *governs*, *this document
wins*, *the authoritative version is*. A document that needs to say one of them is proof that a
fact has two homes. The remedy is always to **delete the duplicate and cross-reference**, never to
rank the copies.

The reasons are the same ones that make duplication a defect in code:

- **Precedence is unenforceable.** Nothing checks it, so it survives only as long as someone
  remembers. A reader who reaches the stale copy first has no way to know it lost.
- **Copies drift, and the wrong one gets believed.** This is not hypothetical here: a capability
  table listed `Task[T]` as `` `sendable ``-only while the compiler treated it as sharable, and the
  disagreement was resolved in favour of the wrong side — the same failure that
  [annotations.md](annotations.md) §1 forbids one level down, where a property declared twice in the
  compiler let an uncompilable program past the type checker.
- **A precedence note is a permanent apology.** It documents the defect instead of fixing it, and it
  costs every future reader a second lookup.

This constrains specifications only. A note in `docs/archive/` saying which specification replaced a
retired document records a *retirement* — the move already happened, and nothing binding is in two
places — so it is a fact, not a precedence claim.

**How to split a subject instead.** Give each document a different *kind* of statement about it:

| | Home | Contains |
|---|---|---|
| The **model** | the section that owns the concept | What the thing means, what invariant it preserves, how it is derived |
| The **contract** | the reference for that surface | Targets, parameters, interactions, where it is read |

`` `sendable `` is the worked example: §6.5 of [language-design.md](language-design.md) defines the
capability — what crossing a goroutine boundary means, and how the capability is derived from a
type's fields — while [annotations.md](annotations.md) defines the annotation that asserts it.
Neither restates the other, and each links to the other once.

**A fact whose home is source code stays there.** Which capability `Task[T]` asserts is read from
`modules/std/task.pr`; copying it into prose creates a third copy that no test can catch drifting.
Prose says where to look, not what it will say.

## 5. Cross-reference, do not copy

Link to the document that owns a fact. If a passage must be edited whenever the target changes, it
is a copy however it is worded — a paraphrase and a quotation drift identically.

Restating something to save the reader a click is the way duplication is always introduced. The
click is cheaper than the divergence.

## 6. Lifecycle

A specification has three transitions, and each is a single reviewed change.

**Ratification — proposal becomes binding.** A design begins in `docs/proposals/`, where it is not
binding, carries no tag, and may be rewritten freely. Ratifying it is one change that does three
things together: `git mv` into the root, add the §2 header, and move its entry into the body of
[index.md](index.md). That change *is* the decision, and it is reviewed as one — there is no
separate approval step and no marker recording that it happened.

**Amendment — a binding document changes.** An amendment is an ordinary reviewed diff against the
document. It lands **before or with** the change that implements it, never after: a specification
that trails its implementation has stopped describing the end state and started reporting history.
An amendment that widens the gap between the document and the implementation must file the items
that close it, in the same change (§7).

**Retirement.** `git mv` into `docs/archive/`, and move its index entry to the archive list. A
retired document keeps its content; only its location, and so its authority, changes.

## 7. Reconciliation with the implementation

A specification states the end state, so at any moment the implementation may lag it, diverge from
it, or exceed it. **None of that is written in the document.** It is carried entirely by the
tracker, under one invariant:

> **Every gap between a specification and the implementation is covered by an open tracker item
> carrying that document's tag.**

That invariant is what makes `mcp__tracker__list --tag <basename>` a *complete* status section
rather than a partial one, and it is why the document needs no markers: a reader who wants to know
what is true today runs the query, and a reader who wants to know what should be true reads the
document. Neither answer contaminates the other.

Three kinds of gap exist, and all three are recorded the same way:

| Gap | Meaning | Item describes |
|---|---|---|
| **Unbuilt** | The document specifies something that does not exist yet. | Building it. |
| **Divergent** | The implementation does something the document forbids, or does it differently. | Correcting the implementation — or, if the document is wrong, amending it under §6. |
| **Unspecified** | The implementation has surface the document does not describe. | Specifying it, or removing it. A gap in the *document* is still a gap. |

**The reconciliation pass.** After a document is ratified or amended, walk it against the
implementation and file the items that close every gap found. Run the pass as its **own change**,
separate from the document change: the document's diff then stays reviewable as a statement of
intent, and the resulting item list stays reviewable as a plan. Nothing about the pass is written
into the document.

Two rules keep the invariant true over time:

- **Closing an item is what shrinks the gap** — never an edit to the document. When the work is
  done, close the item and change nothing else; there was nothing in the document to update.
- **An item may not be closed while its gap remains.** The tag query is the only record, so closing
  an item early does not defer the gap, it erases it.

## 8. What is enforced mechanically

`CheckDocs` in `tools/build/common/docscheck.go` runs unconditionally in `RunPreCommit`, before the
staged-file scan, so these cannot be skipped or deferred:

| Check | Asserts |
|---|---|
| `checkDocLinks` | Every relative `.md` link resolves to a file that exists. Anchors are not checked, which is what keeps the false-positive rate at zero. |
| `checkDocIndex` | Every tracked `docs/**.md` is linked from `index.md`. |
| `checkCatalogCoverage` | Every directory under `modules/` has a catalog entry, and every shipped module is named in each inventory document. |

Everything else here is upheld by review. The rules most worth adding a check for are the ones a
reader cannot verify locally: §3's ban on status sections, and §4's ban on precedence language.
