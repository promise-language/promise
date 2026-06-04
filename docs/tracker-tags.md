# Tracker Tag Namespace

> The canonical tag vocabulary for the `tracker` MCP server, and the rules for applying it. **Consult this file before tagging a tracker item.** Tags are how items are found (`mcp__tracker__list --tag`, gate/report filters), so a consistent, low-cardinality vocabulary matters more than expressiveness. When in doubt, reuse an existing canonical tag rather than coin a new one.

The tracker is the sole source of truth for bugs/tasks/plans (see CLAUDE.md “Bug & Task Tracking”). This doc governs the **tags** field only.

---

## 1. Rules

1. **Casing — lowercase `kebab-case` only.** No camelCase, no code symbols (`dupHeapValue`, `enumCtorTemps`), no spaces. Subsystem nouns are **singular** (`enum`, `channel`, `map`, `optional`, `closure`, `generator`, `operator`), never plural.
2. **Count — 2 to 5 tags per item.** Fewer than 2 under-files it; more than 5 dilutes the signal. If you want a sixth tag, drop the weakest.
3. **Mandatory facets:**
   - **Every item** carries **at least one Component/subsystem _or_ Area tag** (§2.1 / §2.3) — what part of the system it touches.
   - **Every bug** additionally carries **exactly one Quality/kind tag** (§2.4) — what _kind_ of defect it is (`memory-leak`, `double-free`, `crash`, `flaky`, `correctness`, `performance`, …).
   - **Platform** (§2.2) tags are added **only** when the item is genuinely platform-specific.
4. **Never tag what the status field already says.** Do **not** use `duplicate`, `wontfix`, `regression`, `preexisting`, `needs-answer` as tags — those are statuses. The only sanctioned process tag is `needs-attention`.
5. **Don’t put item IDs in tags.** `T0066`, `b0250`, `duplicate-of-T0573` are relationships, not tags — express them via the body, `blocked_by`, or `parent_id`. The one tolerated form is a single `<id>-followup` tag on a direct follow-up item.
6. **Prefer the canonical spelling (§3).** If the concept already has a canonical tag, use it; don’t add a synonym.

---

## 2. The namespace by facet

Pick tags from these facets. Lists are the established vocabulary — extend only when nothing fits.

### 2.1 Component / subsystem (compiler internals)
`codegen` · `sema` · `parser` · `grammar` · `ownership` · `types` · `formatter` · `scheduler` · `runtime` · `pal` · `reactor` · `linker` · `monomorphization` · `rtti`

Feature subsystems (singular nouns): `vector` · `map` · `set` · `string` · `optional` · `enum` · `tuple` · `array` · `slice` · `match` · `closure` · `lambda` · `iterator` · `generator` · `channel` · `task` · `goroutine` · `coroutine` · `arc` · `mutex` · `mutexguard` · `weak` · `operator` · `constructor` · `getter` · `indexing` · `structural` · `for-in`

### 2.2 Platform
`windows` · `macos` · `linux` · `wasm` (+ wasm sub-area tags where needed: `wasm32-web`, `wasi`, `wit`, `component-model`, `browser`)

### 2.3 Area / initiative
`build` · `build-tools` · `ci` · `gate` · `gate-system` · `distribution` · `release` · `install` · `prebuilts` · `cross-compile` · `cross-platform` · `cache` · `infrastructure` · `modules` · `catalog` · `cli` · `docs` · `hooks` · `flow` · `health`

### 2.4 Quality / kind
`memory-leak` · `double-free` · `drop` · `memory` (memory-model/layout/allocator only — **not** leaks) · `crash` · `panic` · `hang` · `flaky` · `test-failure` (deterministic) · `correctness` · `performance` · `robustness` · `hardening` · `safety` · `security` · `cleanup` · `dx` · `agent-ux` · `api` · `style` · `latent` · `test-infra` · `testing` · `coverage`

### 2.5 Process / meta
`needs-attention` (only) — everything else lives in the status field, `blocked_by`, or `parent_id`.

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

When filing, ask: **one subsystem-or-area tag? (bugs) one kind tag? platform only if platform-specific? all canonical spellings? ≤ 5 total?** If yes, file it.
