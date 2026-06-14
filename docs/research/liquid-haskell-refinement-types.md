# Should Promise adopt Liquid Haskell–style refinement types?

*A grounded assessment built from the Promise compiler source and design docs.*

**Date:** 2026-06-14
**Scope:** Whether — and in what form — Liquid Haskell's refinement-type approach should map onto Promise's type system, judged against the actual language rather than assumptions about it.

---

## Executive summary

Liquid Haskell (LH) layers a refinement-type checker on GHC: a type becomes a base type plus a logical predicate (`{v:Int | v > 0}`), and call sites that can't prove a precondition become compile-time errors instead of runtime crashes. Predicates live in a decidable logic discharged by an SMT solver (Z3). LH spans a continuum from cheap inferred checks (array bounds, no division by zero, no `head []`) up to full functional-correctness proofs via *refinement reflection*.

Assessed against the **real** Promise language:

- **Adopt the lightweight tier only** — array/index bounds, non-zero divisors, non-empty preconditions — implemented as a **pure-Go interval/abstract-interpretation pass in the default build**, no SMT solver embedded.
- **Push anything richer to an opt-in `--refine` pass** that fetches a solver from the on-demand cache, never in the default path.
- **Refinements strengthen success payloads; they never replace the `?`/sum-type failure channel** — which is more agent-legible and already idiomatic.
- **Reject reflection and full functional-correctness proofs** — they violate the documented "one obvious way" and "minimal context" goals.
- **For the concurrency / resource axis, the right tool is typestate / session / linear types built on Promise's existing affine ownership — not refinements.** This is the most actionable gap the analysis surfaced.

Promise's **strict evaluation** (confirmed) makes value-level refinements *sounder and cheaper* than in lazy Haskell: LH's headline termination↔refinement coupling largely evaporates.

---

## Hypothesis audit

Each hypothesis below was checked against docs and source. Citations are to real files.

| # | Hypothesis | Verdict | Grounding |
|---|---|---|---|
| **H1** | Strict, no pervasive laziness, no first-class thunk | ✅ **Confirmed** | Rust/Dart-lineage strict language (`docs/language-design.md:5`); params are by-value borrows, not thunks (`language-design.md:1487`). Stream combinators are *operation*-lazy (`language-design.md:2770`); `task[T]` is *concurrency*, not a value-level thunk (`language-design.md:3532`). No `Thunk`/`Lazy` type exists. |
| **H2** | `?` is Rust/Swift error-propagation; whitespace-sensitive; previously confused an agent | ✅ **Confirmed, with a precise nuance** | `?^` propagate, `?!` panic, `? e {}` handler (`language-design.md:1642-1706`). Whitespace rule is real: **`expr ? e {}` (space) = error handler; `expr? _ {}` (no space) = optional handler** (`language-guide.md:878`, `:293`). Documented agent-trap: **"`?!` … always means 'panic on error' — it does not propagate"** (`language-design.md:1778`). |
| **H3** | Sum types + Result/Option discipline | ✅ **Confirmed** | `enum` = ADTs incl. `Option[T]`, `Result[T,E]` (`language-design.md:1018-1036`); fallibility marked by `!` on the *function name*, desugaring to `(value, error)` (`language-design.md:1619-1631`); `T?` sugar for `Option[T]` (`language-design.md:3096`). |
| **H4** | HM-global *or* bidirectional *or* annotation-driven | ⚠️ **Corrected** — **annotation-driven + local/bidirectional, NOT global HM** | Params, returns, fields **must** be annotated; locals inferred via `:=` (`language-design.md:757`); literals default `int`/`f64` (`sema/expr.go:168-196`); generic *function* args inferred by unification (`sema/infer.go:8`) but generic *method* args are **explicit, no inference** (`language-design.md:992`). No global constraint solver. |
| **H5** | ADTs + generics + traits; nominal vs structural | ✅ **Confirmed**, hybrid | Generics `[T: Eq + Ord]` with `+`-composed constraints (`language-design.md:925-964`); interfaces via `is` are **nominal by default**, **structural opt-in** via `` `structural `` (`language-design.md:838-890`). Ownership annotations `&`/`~`/`~T` (`language-design.md:1451-1487`). |
| **H6** | goroutines + channels; any typestate/linearity/session ambitions | ✅ concurrency confirmed; ⚠️ **typestate/session: absent, no documented ambition** | M:N GMP scheduler, channels send/close, `select`, `Arc[T]` (`channel.pr`, `docs/runtime-architecture.md:238-246`). Grep for `typestate\|session\|linear\|affine` across `docs/` finds nothing in the type-system sense. Ownership is **affine (use-at-most-once)** (`ownership/`, `language-design.md:1449-1487`); single-owner move-only handles (`task[T]`, `Mutex`) exist (`language-design.md:3635`) but with no per-state method gating. |
| **H7** | Lean toolchain; bundling a big SMT solver into default build undesirable | ✅ **Confirmed** (with an ironic wrinkle) | "single Go binary" (`CLAUDE.md:1`, `language-design.md:5`); heavy deps **fetched on demand**, not embedded (`docs/distribution.md:20-27`). Wrinkle: the macOS LLVM tarball already transitively ships `libz3` (`runtime-architecture.md:139`) — Z3 is physically present there but unexposed. |
| **H8** | Agent-first rationale documented, with a concrete "legible" notion | ✅ **Confirmed, verbatim** | `CLAUDE.md:9-13`: *self-contained readability, explicit over implicit, minimal context, one obvious way* (quoted below). |

The wrong hypothesis (**H4**) and the nuanced ones (**H2**, **H6**) are reworked into the conclusions below.

---

## 1. Soundness under Promise's real evaluation strategy

**LH's termination↔refinement coupling does not apply to Promise at the value level.** LH needs it only because Haskell is lazy: a binding `x :: {v:Int | v > 0}` may be a thunk that diverges when forced, and a divergent term inhabits *every* refinement — so you could "prove" `false`. LH therefore ties refinement soundness to a totality/termination analysis. Promise is strict (H1 confirmed): by the time a value of type `{v:Int | v > 0}` exists, it has been evaluated to a value. There is no thunk to diverge. **Value-level refinements in Promise need no termination analysis at all.** This is the single biggest structural advantage Promise has over Haskell for this feature.

Separate the two LH mechanisms:

- **Value-level refinements** (lightweight tier — bounds, non-zero, non-null): *free of the termination obligation* under strictness. The predicate is a property of an already-computed value.
- **Refinement reflection** (heavyweight tier — lifting a function body into the logic to prove theorems): **re-introduces a termination obligation**, but a narrow one. Reflecting `f` asserts `f` is total; reflecting a *partial* function is unsound even in a strict language. **The obligation is scoped to reflected functions only**, not to every value-typed binding as in lazy Haskell.

**Cost structure vs. Haskell: strictly better, for an architectural reason.** In Haskell every refined binding is a potential bottom, so termination analysis runs pervasively. In Promise the obligation collapses to "the set of functions you explicitly reflect" — most of which (structural `len`/`size` measures, arithmetic) are obviously total. Promise needs a termination checker only if it adopts reflection, and then only on the reflected subset.

**Panics don't threaten this.** Vector `[]` panics on out-of-bounds (`language-design.md:3012`, `vector.pr:20`) and `?!` panics (`language-design.md:1778`). A panic is divergent *control flow*, not a value — you never get a refined value out of a panicking call, control leaves. The strict-language story works in Promise's favor here.

*(Inference: the docs contain no mention of refinement types or reflection. This section is analysis of how the documented strict semantics interact with LH's known soundness model.)*

---

## 2. Which tier to adopt

LH's continuum runs lightweight (inferred, cheap: bounds / no-div-zero / no-`head []` / totality) → heavyweight (full functional correctness; hand proofs via reflection). Against Promise's documented goals:

> **Minimal context needed**: An AI agent should be able to generate a correct Promise program without needing to understand a large framework or ecosystem. The standard library should be small, orthogonal, and predictable. — `CLAUDE.md:12`

> **One obvious way**: Avoid multiple equivalent ways to do the same thing. Reduce ambiguity … so code generation is deterministic. — `CLAUDE.md:13`

**Recommended line: adopt only the bottom of the continuum, and only where it removes a panic or an unprovable invariant from *surface syntax*, not from an invisible solver context.**

**Near-free and goal-aligned:**
- **Index/bounds refinements** on `Vector.[]`, `string.[]`, `byte_at`, `JsonValue.at` — the operations that currently **panic** (`vector.pr:20`). A refinement making `v[i]` typecheck only when `i < v.len` converts a runtime panic into a compile-time error. High value.
- **Non-zero divisors** (`divide!` raises on `b==0` today — `language-guide.md:720`).
- **Non-empty preconditions** (a `first()`/`head` analogue).
- **Exhaustiveness** — already largely handled by `match` on enums; a refinement layer adds little.
- **Structural measures** (`len`, `size`) lifted into the logic so bounds reasoning can discharge.

**Expensive or anti-goal:**
- **Refinement reflection + equational proofs** (`===`/`*** QED`). A second language layered on the first. Violates *"one obvious way"* and *"minimal context"*; re-introduces the termination obligation (§1). **Reject.**
- **Full functional-correctness specs** ("insertion sort really sorts"): high authoring cost, low generation determinism. Anti-goal.

The deciding quote is *"minimal context needed"*: anything forcing the agent to reason in an SMT proof context it can't see on the page works against the language's reason to exist.

---

## 3. Refinements vs. / with `?` and sum types — the agent-legibility tradeoff

This is where the agent-first mandate is decisive, and it cuts **against** refinements as the *primary* partiality mechanism.

Take `head []`. LH makes `head` total by **refusing to typecheck** it on a possibly-empty list — the precondition discharged in an **invisible SMT proof context**. The Promise `?`/sum-type discipline makes partiality total by **returning a value the caller must handle or propagate** — the failure mode announced *in-band*, the propagation *visible in the token stream* (`?^`, `? e {}`, the `!` on the name).

> **Self-contained readability**: Looking at a single source file should be enough to understand with certainty what it does. Avoid hidden effects, implicit behaviors, and action-at-a-distance. — `CLAUDE.md:10`

A refinement's discharge is **action-at-a-distance by construction**: whether `v[i]` compiles depends on facts proved from code that may be far away, none visible at the call. The `?`/`!` machinery is the opposite — fallibility is on the callee's name (`read_all!`), and every propagation site carries a `?`-family token. **For an agent reconstructing intent from surface syntax alone, the in-band sum-type discipline is strictly more legible.** It is the existing answer the language already chose.

The H2 nuance sharpens this: the `?` surface is *already* subtle enough to warrant an explicit "common agent mistake" warning (`?!` panics, doesn't propagate — `language-design.md:1778`) and a **whitespace-load-bearing** distinction (`expr ? e {}` error-handler vs `expr? _ {}` optional-handler — `language-guide.md:878`). Adding a second, *invisible* partiality channel would compound the reconstruction problem, not relieve it.

**Composition — where refinements earn their place.** They are not rivals with `?`; they compose, and refinements work best as a *modifier on the success payload*, never as a replacement for the failure channel:

- **Refinement riding on a success payload — yes, valuable.** `parse(s) -> Result<{v:int | v >= 0}, ParseError>` (Promise idiom: a failable `parse!` whose returned `int` is refined non-negative). The failure path stays *in-band and visible* (`!`/`?`); the refinement only strengthens what the caller learns on success, so a downstream `v[parsed]` typechecks without a second bounds check. Additive legibility.
- **Refinements pruning provably-impossible error branches — yes, narrowly.** A value refined `{v:int | v < len}` lets an otherwise-failable indexing call discharge statically, eliminating a dead `? e {}` arm. Useful, but it makes the *absence* of a handler meaningful — confine it to short, local bounds/non-zero cases.

**Recommended division of labor:** the **sum-type/`?` discipline owns the failure channel** (in-band, visible, idiomatic, agent-legible); **refinements are a strengthening annotation on payloads and on the few panic-bearing primitives** (`[]`, `byte_at`, division), removing a panic or a redundant guard. Refinements must never *signal* failure — only *prove a guard unnecessary*.

---

## 4. SMT / toolchain cost

The toolchain posture (H7) is explicit and lean:

> heavy dependencies (LLVM tools, wasm runner, CRTs, target sysroots) move from *embedded-in-the-binary* to a *content-addressed cache fetched on demand* — `docs/distribution.md:3`

> **Always embedded** (small, always needed): Compiler frontend + codegen, standard library source… **Fetched on demand** (large, target-specific): LLVM host tools… — `distribution.md:24-27`

Evaluating the four options:

- **(c) Full SMT (Z3) in the default build — reject.** Z3 is a large C++ dependency; embedding it in the always-on payload contradicts the "small, always needed" boundary and the single-Go-binary identity (`CLAUDE.md:1`). *(Ironic wrinkle: macOS LLVM already drags in `libz3` transitively — `runtime-architecture.md:139` — but that's LLVM's private copy, not a sanctioned, cross-platform, exposed solver.)*
- **(d) Skip — viable, the honest default**, since the existing `?`/sum-type story already covers partiality (§3).
- **(a) Opt-in pass / separate tool (LH's own plugin model) — strong.** Refinement checking lives in a separate `promise verify`-style tool or `--refine` pass that *may* pull Z3 from the on-demand cache, exactly as LLVM tools are fetched today. The default `build`/`run`/`test` path stays solver-free and binary-thin.
- **(b) Syntactically decidable fragment in the core path (intervals/bounds by abstract interpretation, no general solver) — strong, complementary.** The lightweight tier (§2) is largely an interval/linear-arithmetic problem. A bespoke abstract-interpretation pass over intervals — *no Z3* — discharges the high-value 80% (array bounds, div-by-zero) in the default build with zero new heavy deps.

**Recommendation: (b) in the core, (a) as the escape hatch.** Ship an interval/bounds checker in pure Go inside the compiler for the lightweight tier — it stays in the thin binary, runs in the default build, gives *local* evidence. Anything richer than quantifier-free linear arithmetic goes to an **opt-in `--refine` pass that fetches a solver on demand**, never in the default path. This is the only combination consistent with both the lean-toolchain quotes and the agent-first goal of a build that "just works" without a 50 MB solver.

---

## 5. Inference interaction

H4 corrected: Promise is **annotation-driven with local/bidirectional inference**, not global HM. Two consequences for LH-style refinement *inference* (qualifier-set predicate abstraction):

- **It cooperates rather than conflicts — there is no global unifier to fight.** LH's inference is hardest to retrofit onto a global HM solver because refinement variables and type variables get entangled. Promise resolves base types *locally* (params/returns/fields already annotated — `language-design.md:757`; generic method args explicit — `language-design.md:992`), so a refinement pass runs **after** base types are fixed, over a fully-typed tree. The refinement layer never co-solves with type inference. A genuine, if accidental, fit.
- **What must be annotated by hand:** function/method **signatures carrying refinements** (the predicate on a param or return), exactly as base types already must be annotated at those boundaries. **Local** refinements (on a `:=` binding) can be *inferred* by predicate abstraction over a qualifier set, mirroring how `:=` already infers base types. Division of labor: **annotate at module boundaries, infer locally** — the same line the language already draws for types. That alignment is the strongest "it would fit" signal in the analysis.

---

## 6. Concurrency: right tool or wrong tool?

For an agent-first *concurrent* language, the high-value correctness targets are **protocol/resource properties, not value predicates** — and LH is fundamentally sequential, silent on goroutines/channels. The in-tree evidence is striking: the partial operations that actually bite in Promise's concurrency and I/O surface are **state preconditions**, not arithmetic:

```
read_all!(~this) string { if this._fd < 0 { raise IoError(code: 9, "bad file descriptor"); } ... }
```
— `io.pr:133-145`; the identical `_fd < 0` guard repeats on `close!`, `seek!`, `read!`, `write!`, `position` (`io.pr:122-187`). Channels: send-after-close and double-close panic (`channel.pr`). These are **typestate** obligations ("handle is open," "channel not yet closed," "connection established before read") — exactly the class refinements model *badly* and typestate/session/linear types model *natively*.

**Refinements are the wrong tool for the concurrency/resource axis.** `v > 0` is not where the bugs are; `fd >= 0` and "don't send on a closed channel" are. And Promise is unusually well-positioned for the *right* tool: it already has **affine ownership** (move-tracking, use-at-most-once — `language-design.md:1449-1487`) and **single-owner move-only handles** (`task[T]`, `Mutex` — `language-design.md:3635`). Typestate is essentially affine ownership where the *type changes on each consuming operation* (`File.open` → `OpenFile`; `read` consumes-and-returns `OpenFile`; `close` consumes to nothing) — Promise is one conceptual step away. H6's finding that there are **no documented typestate/session ambitions** is the most actionable gap in this report.

**Refined ADTs + ghost/phantom state as a substitute: adequate-but-unergonomic, trending dead-end.** You *can* encode "open file" as a refined ADT with a phantom `IsOpen` field threaded through every method — but that re-encodes typestate in the wrong vocabulary, demands the heavyweight refinement tier you otherwise wouldn't ship (§2), and produces signatures an agent reconstructs *worse* than a plain `OpenFile`/`ClosedFile` pair. Against *"one obvious way"* it's a clear loss. **If Promise wants protocol safety, build typestate on the ownership system directly.**

---

## 7. Concrete recommendation

1. **Adopt the lightweight tier only**, surfaced as *interval/bounds checking* — array indices, non-zero divisors, non-empty preconditions — built as a **pure-Go abstract-interpretation pass in the default build** (option 4b). Removes real panics (`vector.pr:20`) at compile time with **zero new heavy dependency**, inside the "small, always needed" budget (`distribution.md:24`).
2. **Anything richer → opt-in `--refine` pass** fetching a solver from the on-demand cache (option 4a), never in the default path.
3. **Refinements strengthen success payloads; never replace the `?`/sum-type failure channel** (§3). The visible, in-band discipline owns partiality-as-failure; refinements only prove guards unnecessary.
4. **Reject reflection and full functional-correctness proofs** — they violate *"one obvious way"* and *"minimal context"* (`CLAUDE.md:12-13`) and re-introduce the termination obligation (§1).
5. **For the concurrency/resource axis, pursue typestate/session/linear types on the existing affine ownership system — not refinements** (§6). Higher-value, better-fitting, currently a documented blank.
6. **Surface errors with *local* evidence.** Not "refinement unsatisfiable," but `index i may reach len (v has length n at line X); call is in-bounds only when i < n`, anchored to the call site. The only way a refinement feature survives contact with *"self-contained readability"* (`CLAUDE.md:10`): the evidence must be on the page, not in the solver.

**Adoption verdict: partial and narrow.** The lightweight bounds/non-zero tier is worth adopting — it removes panics the type system currently can't see. Everything above quantifier-free linear arithmetic (reflection, full correctness) should be declined as anti-goal for an agent-first language. And the feature the concurrency axis actually wants is not LH at all — it's typestate, which Promise is structurally primed for and currently lacks.

---

## Concrete experiment

**Operation chosen:** `File.read_all!` — a real partial operation with a genuine **"handle is open"** precondition.

Actual signature, `modules/io/io.pr:133-145`:
```
read_all!(~this) string `public `doc("Reads all remaining content into a string.") {
  if this._fd < 0 { raise IoError(code: 9, message: "bad file descriptor"); }
  ...
}
```
The precondition `this._fd >= 0` (handle open) is currently enforced **at runtime** and signalled via the failable `!` channel.

### (a) LH-style refined signature

Precondition as a refinement on the receiver (phantom/ghost open-state), discharged by the solver:
```
// hypothetical refined Promise — precondition lives in the type
read_all(~this: {f: File | f.is_open}) string

// caller
f := File.open("data.txt")?^;        // f : {File | is_open}  (refined by open's post)
content := f.read_all();             // typechecks: solver proves f.is_open here
f.close();                           // close's post: f.is_open becomes false
content2 := f.read_all();            // COMPILE ERROR: refinement is_open unsatisfiable
```

### (b) `?`-propagated `Result` — the existing idiom

```
// actual Promise — partiality is in-band on the name + return channel
read_all!(~this) string

// caller (inside a failable function)
process!(string path) string {
  f := File.open(path)?^;            // ?^ propagates open failure, visible on the page
  content := f.read_all()?^;         // ?^ propagates read failure (incl. bad-fd) in-band
  f.close()?^;
  return content;
}
```

### Which does an agent reconstruct more reliably from surface syntax alone?

**(b), decisively.** In (b) every fallible step carries a visible `?^` token and the callee's `!` announces fallibility on its name — an agent reading *only these lines* knows what can fail and where control leaves, with no external context. This is *"self-contained readability"* and *"explicit over implicit"* operating as designed (`CLAUDE.md:10-11`).

In (a), whether `f.read_all()` compiles depends on the solver having proved `f.is_open` from `open`'s and `close`'s postconditions — none visible at the call. An agent reconstructing intent from the call site alone cannot tell the second `read_all` is illegal without the invisible proof context. Textbook action-at-a-distance — the exact thing `CLAUDE.md:10` says to avoid.

### What each does at the failure site

- **(a)** Failure is a **compile-time type error** at the bad call — *if and only if* the solver can prove it. Zero runtime cost, but the diagnosis is "refinement unsatisfiable" unless the implementation invests in local-evidence messages (§7.6).
- **(b)** Failure is a **runtime `IoError`** that is propagated (`?^`), handled (`? e {}`), or panicked (`?!`) — *visibly, in-band*, at a site the reader can see.

### Which I'd ship

**Ship (b).** It is the existing idiom, the more agent-legible reconstruction target, and the "handle is open" precondition is a **typestate** property — the class §6 argues refinements model *worst*. If Promise ever wants this checked statically, the right vehicle is a typestate `OpenFile`/`ClosedFile` split built on the affine ownership system (`language-design.md:1449`), not an SMT refinement. Reserve refinements for **value-level bounds/non-zero** cases (§2), where the predicate is short, local, and visible — and where no in-band sum-type alternative already does the job better.

---

## Confidence and method

The entire refinement discussion is **inference**: the Promise docs contain no mention of refinement types, reflection, or SMT integration, and grep confirms no typestate/session/linear ambitions either. Every factual claim about Promise is cited to source; every claim about how refinements *would* interact is analysis layered on those facts, flagged as such.

Facts were gathered by reading the actual artifacts — `docs/language-design.md`, `docs/language-guide.md`, `docs/distribution.md`, `docs/runtime-architecture.md`, `CLAUDE.md`, the grammar, `compiler/internal/sema/`, `compiler/internal/ownership/`, and `modules/std/`, `modules/io/`, `modules/json/`, `modules/net/` — not from assumptions about the language.

### Key source references

- Evaluation strategy / strictness: `docs/language-design.md:5`, `:1487`, `:2770`, `:3532`
- `?` operator family + whitespace rule: `docs/language-design.md:1642-1706`, `:1778`; `docs/language-guide.md:878`, `:293`
- Sum types / failability: `docs/language-design.md:1018-1036`, `:1619-1631`, `:3096`
- Type inference: `docs/language-design.md:757`, `:992`; `compiler/internal/sema/expr.go:168-196`; `compiler/internal/sema/infer.go:8`
- Type system / interfaces / generics: `docs/language-design.md:838-890`, `:925-964`, `:1451-1487`
- Concurrency / ownership: `modules/std/channel.pr`; `docs/runtime-architecture.md:238-246`; `compiler/internal/ownership/`; `docs/language-design.md:1449-1487`, `:3635`
- Toolchain philosophy: `CLAUDE.md:1`, `:9-13`; `docs/distribution.md:3`, `:20-27`; `docs/runtime-architecture.md:139`
- Stdlib partial operations: `modules/std/vector.pr:20`; `modules/io/io.pr:122-187`; `docs/language-design.md:3012`; `docs/language-guide.md:720`
