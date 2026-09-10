# Normative Documents

> **Tag:** `normative` — remaining work to complete this document: `mcp__tracker__list --tag normative`

[org/normative.md](org/normative.md) defines what makes a document in this repository binding, the
header every specification carries, why none of them contains a status section, and the
one-fact-one-home rule that keeps two of them from disagreeing. It binds here, and it is never
edited here: a defect in one of its rules is filed against
[promise-language/org](https://github.com/promise-language/org).

This document is the **project-local delta** — the few facts about reconciliation that the shared
document cannot carry, because they differ from project to project. Every other rule about the
documents in this repository is [org/normative.md](org/normative.md)'s, and is cited here rather
than restated (§4 there).

## 1. Where a gap is recorded

Items live in this project's `tracker` MCP server. GitHub Issues is the public inbound surface: an
issue filed there is imported and becomes a tracker item, and the item is what the tag query
returns. The query's exact spelling is stated once, in [index.md](index.md), which is the home
[org/normative.md](org/normative.md) §2 designates for it.

Which tags an item carries is [tags.md](tags.md), where a document tag never satisfies the
subsystem requirement (§1.3, §2.6 there).

## 2. Granularity — one item per document

> **A reconciliation pass files at most one item per document — never one per gap.**

Its body is a checklist with one row per gap. A row either names the item that closes that gap, or
describes the gap in place. When someone picks a described row up, it is promoted to its own item
and the row points at it instead. The document's item closes when every row is closed, and a pass
that finds no gap files nothing at all.

This is what makes [org/normative.md](org/normative.md) §7's invariant hold at a bounded item count.
The tag query returns one item per document plus whatever rows have been promoted out of it, so the
list stays readable while every gap stays individually addressable — and a document whose pass has
not run shows up as one open item rather than as nothing at all, which is the failure mode an
honour-system invariant otherwise has. A row nobody can act on without re-deriving the gap is a row
written badly, not an argument for finer granularity.

## 3. When the pass runs — forward-only

Every ratification and amendment runs the pass over its own delta, as its own change
([org/normative.md](org/normative.md) §7). That is the whole steady-state rule, and nothing periodic
sweeps behind it.

A document ratified before this rule existed carries **one backlog item covering its whole surface**,
filed once and scheduled like any other work. There is no bulk sweep: the backlog is walked one
document at a time, and that document's item is the record that its pass has not run yet.

## 4. What a gap is filed as

The three kinds of gap — unbuilt, divergent, unspecified — are named in
[org/normative.md](org/normative.md) §7, and the tag facets in [tags.md](tags.md). This project
files them as:

| Gap kind | Item type | Tags |
|---|---|---|
| **Divergent** | `bug` | document tag + subsystem/area + exactly one quality/kind tag |
| **Unbuilt** | `task` | document tag + subsystem/area |
| **Unspecified** | `task` | document tag + subsystem/area |

A **Divergent** gap is an ordinary defect that a document happens to forbid, so it is filed as one
and *additionally* carries the document's tag. That tag is what puts it in the document's status
section; it is never the item's only area tag.

The table governs a row promoted out of a document's item (§2). The document's own item is always a
`task`, whatever kinds its rows turn out to be, tagged with the document's basename plus `normative`
and `docs` — the pass is documentation work until one of its rows is picked up.

## 5. What is checked

**Nothing verifies that a gap has an item.** There is no machine-readable notion of a gap in
general — one side is prose and the other is a compiler — so
[org/normative.md](org/normative.md) §7's invariant is upheld by review, exactly as its §8 says.
Nothing re-checks at close time that a closed item's gap is really gone, either. The review that
closes the item is the whole guarantee.

One surface is the exception, and it is project-local: [annotations.md](annotations.md) §6's
annotation table is machine-readable on both sides, so `checkAnnotationCoverage` reconciles it
against the compiler's registration tables in both directions. A divergence there is not
honour-system — an unledgered one fails the commit, and a ledger row whose divergence is gone is
itself a finding.

> **A known divergence is excused by a row in the checker's ledger, never by a marker in the
> document.**

That is what lets a check strict enough to be worth running coexist with
[org/normative.md](org/normative.md) §3's ban on inline markers: the exception has a home, and the
home is source, where a stale one is caught.
