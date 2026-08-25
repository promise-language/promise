# Tag Namespace

> **Tag:** `tags` — remaining work to complete this document: `mcp__tracker__list --tag tags`

> The canonical tag vocabulary for this repo's bugs, tasks, and issues, and the rules for applying it. **Consult this file before tagging or labelling an item.** Tags are how items are found later, so a consistent, low-cardinality vocabulary matters more than expressiveness. When in doubt, reuse an existing canonical tag rather than coin a new one.

This doc governs the **tags/labels** field only — not titles, bodies, or status.

---

## 1. Rules

1. **Casing — lowercase `kebab-case` only.** No camelCase, no code symbols (`dupHeapValue`, `enumCtorTemps`), no spaces. Subsystem nouns are **singular** (`enum`, `channel`, `map`, `optional`, `closure`, `generator`, `operator`), never plural.
2. **Count — 2 to 5 tags per item.** Fewer than 2 under-files it; more than 5 dilutes the signal. If you want a sixth tag, drop the weakest.
3. **Mandatory facets:**
   - **Every item** carries **at least one Component/subsystem _or_ Area tag** (§2.1 / §2.3) — what part of the system it touches.
   - **Every bug** additionally carries **exactly one Quality/kind tag** (§2.4) — what _kind_ of defect it is (`memory-leak`, `double-free`, `crash`, `flaky`, `correctness`, `performance`, …).
   - **Platform** (§2.2) tags are added **only** when the item is genuinely platform-specific.
   - A **Document tag** (§2.6) **does not satisfy** the subsystem/area requirement — it says *which end state the item moves toward*, not *what part of the system it touches*. An item carrying one still needs its own subsystem-or-area tag (and, if a bug, its kind tag): doc tag + subsystem + kind = 3 of the 5 allowed. The exception is a doc tag that also appears in §2.3's area list — the two names coincide, so the one tag fills both roles and is counted once.
4. **Never tag what the status field already says.** Do **not** use `duplicate`, `wontfix`, `regression`, `preexisting`, `needs-answer` as tags — those are states, not areas. The only sanctioned process tag is `needs-attention`.
5. **Don’t put item IDs in tags.** An ID like `T0066` or `duplicate-of-T0573` expresses a *relationship*, not a category — put it in the body or in the item's own blocked-by / parent field. The one tolerated form is a single `<id>-followup` tag on a direct follow-up item.
6. **Prefer the canonical spelling (§3).** If the concept already has a canonical tag, use it; don’t add a synonym.
7. **If nothing fits, coin it — and record it here in the same change (§5).** The vocabulary is meant to grow; what it must never do is grow *silently*. A tag that exists on items but not in this file is invisible to everyone who reads this file first, which is how a namespace rots into synonyms. Document tags (§2.6) are the single exception: they are defined by the contents of `docs/` root rather than by a list here, so they are already visible and must not be copied into §2.

---

## 2. The namespace by facet

Pick tags from these facets. Lists are the established vocabulary — extend only when nothing fits, and when you do, follow §5.

### 2.1 Component / subsystem (compiler internals)
`codegen` · `sema` · `parser` · `grammar` · `ownership` · `types` · `formatter` · `scheduler` · `runtime` · `pal` · `reactor` · `linker` · `monomorphization` · `rtti` · `bindgen` (the WIT/WebIDL binding generator behind `promise bind`)

Feature subsystems (singular nouns): `vector` · `map` · `set` · `string` · `optional` · `enum` · `tuple` · `array` · `slice` · `match` · `closure` · `lambda` · `iterator` · `generator` · `channel` · `task` · `goroutine` · `coroutine` · `arc` · `mutex` · `mutexguard` · `weak` · `operator` · `constructor` · `getter` · `indexing` · `structural` · `for-in`

### 2.2 Platform
`windows` · `macos` · `linux` · `wasm` (+ wasm sub-area tags where needed: `wasm32-web`, `wasi`, `wit`, `component-model`, `browser`)

### 2.3 Area / initiative
`build` · `build-tools` · `ci` · `gate` · `gate-system` · `distribution` · `release` · `install` · `prebuilts` · `cross-compile` · `cross-platform` · `cache` · `infrastructure` · `modules` · `catalog` · `stdlib` · `cli` · `docs` · `hooks` · `flow` · `health`

### 2.4 Quality / kind
`memory-leak` · `double-free` · `drop` · `memory` (memory-model/layout/allocator only — **not** leaks) · `crash` · `panic` · `hang` · `flaky` · `test-failure` (deterministic) · `correctness` · `performance` · `robustness` · `hardening` · `safety` · `security` · `cleanup` · `dx` · `agent-ux` · `api` · `style` · `latent` · `test-infra` · `testing` · `coverage`

### 2.5 Process / meta
`needs-attention` (only) — everything else belongs in the status field or in the item's blocked-by / parent relationships.

### 2.6 Document
This facet is a **rule, not a list**: one tag per normative document in `docs/` root, spelled **exactly as the file's basename** minus `.md`. `docs/language-design.md` → `language-design`.

**The enumeration is the directory, and this file deliberately does not copy it.** To see the vocabulary, list the directory:

```sh
ls docs/*.md          # every document tag, one per line, minus the .md
```

Every such doc also declares its own tag on the line under its title. That line is a *pointer* for someone reading that doc — not a second registry — so there is nothing here to keep in sync: add a doc to `docs/` root and its tag exists; `git mv` it to `proposals/` or `archive/` and its tag ceases to exist. The other facets in §2 are enumerated because nothing else defines them; this one is not, because the filesystem already does.

**What a document tag means.** A doc in `docs/` root states an *end state* — what the project should be — and never records how far along it is (see [index.md](index.md) for the folder rule). The gap between that end state and today is therefore not written in the doc; it is the set of open items carrying the doc's tag. `mcp__tracker__list --tag <doc>` **is** the doc's status section. Tag an item with a document tag when closing it moves the project measurably closer to what that document describes — not merely because the item touches a subsystem the doc mentions.

**Scope.** Only `docs/` root files get a tag. Files under `docs/proposals/` describe an end state that has not been ratified, and files under `docs/archive/` describe one that has been superseded or delivered; neither is something the project owes work against, so neither takes a tag. A proposal earns its tag when it is ratified — `git mv` into `docs/` root, add the tag line under its title, and move its entry into the body of [index.md](index.md), all in the one change. Nothing in *this* file changes. `docs/index.md` is the map, not a spec, and has no tag.

---

## 3. Canonical spellings (use the left, never the right)

| Use | Not | Why |
|-----|-----|-----|
| `macos` | `darwin` | user-facing platform name; more frequent |
| `build-tools` | `tooling`, `tools` | most frequent and specific |
| `monomorphization` | `mono` | spell it out |
| `infrastructure` | `infra` | more frequent |
| `docs` | `doc` | more frequent (`doc` collides with the `promise doc` command) |
| `memory-leak` | `leak` | reserve `memory` for memory-model/layout work |
| `mutexguard` | `mutex-guard` | one spelling for the guard |
| singular: `enum`, `channel`, `map`, `optional`, `closure`, `generator`, `operator` | plurals | subsystem nouns are singular |
| `crash` (umbrella) + `panic` (only the panic path) | `segfault`, `compiler-crash` | consolidate the crash family |

**Deliberately kept distinct** (do not merge):
- `formatter` (§2.1 — the formatter subsystem: lexer, reformatter, its bugs) vs `formatting` (§2.6 — the [formatting.md](formatting.md) end state). A formatter bug is `formatter`; work that closes a gap in the formatting spec is both.
- `schema` (§2.6 — the [schema.md](schema.md) end state) vs `types` (§2.1 — the compiler's type representation). Schema descriptors are built *on* `types`; they are not the same area.
- `test-infra` (the test runner/harness) vs `testing` (adding/needing test coverage on a feature) vs `test-failure` (a deterministic gate failure) vs `flaky` (intermittent failure).
- `gate` (individual gate work) vs `gate-system` (the gate framework itself).
- `memory-leak` / `leak`-family vs `memory` (layout/allocator/SSO).

---

## 4. Examples

| Item | Good tags |
|------|-----------|
| Codegen bug: drop emitted twice for enum payload | `codegen`, `drop`, `double-free` |
| Intermittent channel test failure on macOS | `scheduler`, `channel`, `flaky`, `macos` |
| New `bin/release` driver (blobs → manifest → thin/full) | `release`, `build-tools`, `prebuilts` |
| Thin/full split + runtime content-addressed dependency store | `distribution`, `runtime`, `cache` |
| Windows zero-dependency install (SDK/UCRT stubs + scripts) | `distribution`, `windows`, `install` |
| Slow monomorphization of nested generics | `codegen`, `monomorphization`, `performance` |
| Collapse the per-width `_wide_int_format_*` helpers into one generic helper | `large-integers`, `stdlib`, `generics`, `cleanup` |
| Emit DWARF line tables so `lldb` can break by source line | `codegen`, `dx` — no doc tag: [debugging.md](proposals/debugging.md) is a proposal |

---

## 5. Extending the namespace

The lists in §2 are a starting vocabulary, not a closed set. New subsystems, platforms, and defect kinds appear, and when one does it deserves a tag. Coining one is fine — **leaving it undocumented is not.**

**Add a tag when all of these hold:**

1. **Nothing in §2 fits.** Not "nothing fits perfectly" — nothing fits. A slightly-too-broad existing tag beats a new near-synonym.
2. **It is not a synonym.** Check §3 first. If the concept already has a canonical spelling, use that one; if your word is the better name, change §3's table rather than adding a second spelling.
3. **It will recur.** Tags earn their place by being searched for more than once. A label that fits exactly one item belongs in that item's title or body.
4. **It names an area or a kind, not a state or a relationship.** Statuses (rule 4) and item IDs (rule 5) are still excluded, however novel they feel.

**How to add one — a single change, both halves:**

1. Tag the item.
2. In the *same* change, append the tag to the right facet list in §2 — component/subsystem, platform, area, or quality/kind. Keep the existing grouping; these lists are ordered by affinity, not alphabetically.
   Document tags (§2.6) are the one facet you do **not** add here: the vocabulary *is* `docs/` root, so a new one appears by adding or ratifying a doc there — never by editing this file. If you find yourself pasting a doc tag into §2.6, that is the duality this facet exists to avoid.
3. If it supersedes or competes with an existing tag, add a row to §3 so the losing spelling is explicitly retired rather than left to compete.
4. Keep it conforming: lowercase `kebab-case`, singular for subsystem nouns (rule 1).

If you are unsure whether a tag clears the bar, prefer the nearest existing tag and say so in the item body. An imperfect tag from the shared vocabulary is recoverable; a private one nobody else knows to search for is not.

---

When filing, ask: **one subsystem-or-area tag? (bugs) one kind tag? platform only if platform-specific? a document tag if the item closes a gap in a `docs/` root spec? all canonical spellings? ≤ 5 total?** If a tag you need does not exist, add it here in the same change (§5). If yes, file it.
