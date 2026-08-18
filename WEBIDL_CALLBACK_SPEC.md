# Spec: generic Promise→JS callback export for `wasm32-web` (WebIDL `callback` is the first consumer)

WebIDL `callback` types — needed for `addEventListener`, i.e. all of
drag/resize/pan/click — are still unimplemented in `promise bind webidl`.
But the actual missing piece underneath it is **not** a bindgen feature: it's
a general `wasm32-web` runtime capability that doesn't exist yet anywhere in
the compiler — a way for JS to call *into* a Promise closure at all. This
spec targets that general capability, with the WebIDL `callback` gap as the
first (and validating) consumer.

## 0. Why this needs to be generic, not bindgen-only

Every wasm export the compiler emits today goes one direction: Promise
calling out to JS/the host. §2 below traces the WebIDL `callback` bug down to
one root fact: nothing in `compiler/internal/codegen` can export a Promise
closure as something JS is able to call back into. That fact has nothing to
do with WebIDL or bindgen — it's a hole in the language's `wasm32-web`
backend. If it's implemented only inside `compiler/internal/bindgen` (e.g.
as a one-off trampoline generator private to `codegen.go`/`jsglue.go`), then:

- WIT bindgen (`wit_to_ir.go` shares the same IR as `webidl_to_ir.go`) has to
  reinvent it independently if/when WIT needs the same shape of thing.
- Any hand-written `wasm32-web` Promise program that wants to register a
  click handler *without* going through generated bindings — e.g. directly
  against the `web` catalog module epic in #17 — has no primitive to build
  on.
- The naming-drift failure mode that caused Bug 1 (two generators inventing
  names for the same logical thing independently) repeats itself, except
  next time between bindgen and whatever else needs callbacks.

So this spec is split into two layers (§4): a general Layer 1 primitive
(language/runtime, lives in `codegen`/PAL, ideally surfaced as a small public
API in the `modules/web` catalog module per #17) and a thin Layer 2
(`promise bind webidl`'s `callback` support, built entirely on Layer 1).
Layer 2 is what closes the reproduction below; Layer 1 is what makes that fix
reusable instead of bindgen-private.

## 1. Reproduction (verified 2026-08-18 against `promise 2026.8-793406b`)

```bash
mkdir repro && cd repro
cat > events.webidl <<'EOF'
callback EventListener = void (MouseEvent event);
interface MouseEvent {
  attribute double clientX;
  attribute double clientY;
};
interface Element {
  void addEventListener(DOMString type, EventListener listener);
};
EOF
promise bind webidl events.webidl -o modules/web/
promise init --force
cp modules/web/mouse_event.pr .
cat > main.pr <<'EOF'
main() {}
EOF
promise build -target wasm32-web
```

Result:

```
mouse_event.pr:53:43: undefined type: EventListener
  >     add_event_listener(this, string _type, EventListener listener) `public {
                                               ^
```

`promise bind webidl` happily generates a `.pr` file that *references*
`EventListener` as a parameter type but never defines it anywhere. The bind
step itself exits 0 — the error only surfaces later, in `promise build`.

## 2. Root cause (traced into `compiler/internal/bindgen` and `webidl`)

The WebIDL parser already handles `callback` declarations correctly —
nothing needs fixing there:

- `compiler/internal/webidl/ast.go:56-63` — `Callback{Name, Return, Params,
  Doc, Pos}` — a full signature, already parsed.
- `compiler/internal/webidl/parser.go:298-323` — `parseCallback()` builds
  and returns it correctly; `file.Callbacks` is populated.

The gap starts exactly where the WebIDL AST is lowered to bindgen's shared
IR — this part is genuinely bindgen-specific and belongs entirely in Layer 2
(§4b):

- `compiler/internal/bindgen/webidl_to_ir.go:58-60` — literally:
  ```go
  // Convert callbacks as function type aliases (skipped in IR for now —
  // callbacks would require lambda types which aren't in the IR)
  ```
  `file.Callbacks` is read nowhere else in this file. It's dropped on the
  floor. Any interface member that references a callback type by name (like
  `Element.addEventListener`'s `listener` param) still gets emitted with
  that bare name — see `convertInterface`/param conversion further down in
  the same file — because nothing checks whether the referenced type
  actually exists.
- `compiler/internal/bindgen/ir.go` — `TypeRef`'s `TypeRefKind` enum
  (`BuiltinKind, NamedKind, ListKind, OptionKind, ResultKind, TupleKind,
  OwnKind, BorrowKind`) has no function-type case at all. There is
  currently no way to represent "this parameter is a callback" in the IR.
- `compiler/internal/bindgen/codegen.go` and `jsglue.go` — never asked to
  handle a callback-typed param (because it can't appear in the IR today),
  so neither emits anything for one. Whatever fix lands in the IR needs a
  matching branch in both — but that branch should call into Layer 1's
  primitive, not emit its own trampoline/handle-table logic (see §4a).

**The fundamental gap — this is the Layer 1 piece, and it is not bindgen-specific:**
nothing in the general compiler backend today emits *any* wasm export beyond
a fixed set (`memory`, `_initialize`, `cabi_realloc`, `__cabi_retarea` —
confirmed via `WebAssembly.Module.exports()` on every `wasm32-web` binary
built during the spikes, WebIDL or not). A callback fundamentally requires
the reverse direction of every other binding so far: JS calling *into* wasm,
not Promise calling out. That needs at least one genuinely new wasm export —
this is very likely the largest piece of new work here, and probably touches
`compiler/internal/codegen` (see `mangleMethodName`, and the "Key
Architecture Concepts" section of this repo's `CLAUDE.md`), not just
`compiler/internal/bindgen`. This is exactly why it should be designed and
shipped as a standalone, documented primitive rather than folded silently
into the bindgen pipeline.

## 3. What already exists and should be reused, not reinvented

- **Promise already has real closures and first-class function types.**
  This is not new language work — `(A, B) -> C` is valid syntax today:
  - `compiler/internal/ast/typeref.go:30` — `FunctionTypeRef` ("a function
    type: `(Int, Int) -> Bool`").
  - `compiler/internal/ast/expr.go:218` — `LambdaExpr`.
  - `compiler/internal/codegen/expr_lambda.go`, `codegen_closure_test.go` —
    working closure codegen with capture.
  The natural shape for a callback-typed WebIDL param is a Promise
  parameter whose type is a `FunctionTypeRef` built from the callback's
  `Return`/`Params` (already fully parsed — see §2). Don't build a parallel
  closure mechanism inside bindgen — and don't build the *general* export
  primitive inside bindgen either (see §0/§4a): it belongs in `codegen`/PAL
  where closures already live, with bindgen as a caller.
- **The JS-side handle-table pattern is already established** and, as of
  the four just-fixed bugs, working correctly: every generated `.js` has
  `_refs`/`_refStore`/`_refLoad`/`_refRelease`, and resource destructors
  use the `[resource-drop]<kebab-name>` import-naming convention (Bug 1's
  fix). Layer 1's Promise-side mirror — register a closure, get back an
  `i32` id; invoke by id; release by id — is the natural analogue for the
  new direction and should follow the same naming discipline, but as a
  general primitive (e.g. `promise_callback_register` /
  `promise_callback_invoke` / `promise_callback_release` in the runtime),
  not a bindgen-generated one-off.
- **Bug 1's root cause was two generators (`codegen.go` `.pr` output vs.
  `jsglue.go` `.js` output) independently inventing names for the same
  logical thing and drifting apart.** This is the concrete argument for
  Layer 1 living in one place: derive the trampoline export name, the
  closure-table function names, and the JS-side counterpart from **one
  shared helper** in the compiler runtime, with both `codegen.go` and
  `jsglue.go` (and any future WIT equivalent) calling it — not each
  reinventing the naming scheme.
- **The existing resource marshalling already works for the callback's own
  argument type.** In the real WebIDL (`callback EventListener = void
  (MouseEvent event);`), `MouseEvent` is itself a resource — the handle
  passed into the trampoline can reuse the exact `_refStore`/`Resource
  (_handle: n)` machinery already generated for every other resource type.
  Don't invent new marshalling for it in either layer.

## 4. Design: two layers

### 4a. Layer 1 — general capability (language/runtime, not bindgen)

A documented, minimal primitive for exporting a Promise closure so JS can
call it, independent of how the closure got there (hand-written `main.pr` or
bindgen-generated glue). Concretely:

- Lives in `compiler/internal/codegen` (the new wasm export + trampoline
  machinery) and the PAL (closure registration/invocation/release runtime
  functions), not in `compiler/internal/bindgen`.
- Should be usable **directly, by hand**, from a `wasm32-web` Promise
  program with zero bindgen involvement — e.g. surfaced as a small public
  API in the `modules/web` catalog module tracked by #17 (something like
  `web.export_callback(closure) u32` / a `Callback` handle type), so it's a
  real language capability, not bindgen plumbing that happens to be
  reachable.
- The primitive itself should be able to represent an arbitrary Promise
  closure signature (param types, return type) even though the first real
  caller (Layer 2, single-param/void-return WebIDL `callback`) only
  exercises the narrowest case. Don't hardcode "single param, void return"
  into the trampoline/handle-table design at this layer — that's a Layer 2
  scope limitation (§5), not a Layer 1 one. Hardcoding it here just moves
  the same "callbacks require a parallel mechanism" problem one level down.

### 4b. Layer 2 — WebIDL `callback` sugar, built on Layer 1

With Layer 1 in place, bindgen's job shrinks to exactly the gap traced in
§2:

- Add a function-type case to `bindgen/ir.go`'s `TypeRefKind` so a
  callback-typed param can be represented at all.
- `webidl_to_ir.go`: stop dropping `file.Callbacks` on the floor; lower each
  `Callback` to that new IR case using its already-parsed `Return`/`Params`.
- `codegen.go` / `jsglue.go`: for a callback-typed param, emit a call into
  Layer 1's primitive (register on the Promise side, wire the trampoline
  export, generate the JS-side invoke helper) instead of inventing bindgen-
  local trampoline/handle-table code.
- WIT gets this close to free: once Layer 1 exists and the IR has a
  function-type case, `wit_to_ir.go` mapping a WIT callback-shaped construct
  (if one exists) to the same IR case is a small, isolated change — worth
  checking once Layer 2 lands, per the original scope note below, but not a
  requirement of this spec.

## 5. Scope

- **Layer 1**: implement the single most general case actually needed to
  unblock Layer 2 — closures taking a fixed argument list of
  resource/primitive types, void return — but design the primitive's shape
  (export/trampoline naming, handle-table API) so multi-argument and
  non-void-return are a natural extension later, not a rewrite. Don't build
  general n-ary/return-value support now; don't paint it into a corner
  either.
- **Layer 2 (WebIDL `callback` sugar)**: `callback` declarations with a
  **single parameter, void return** — the real shape needed for
  `addEventListener(type, listener)`-style APIs (`MouseEvent`/
  `PointerEvent`/etc. in, nothing out). This covers
  click/mousedown/mousemove/mouseup/touch — the whole drag/resize/pan/click
  surface.
- `Element.addEventListener(DOMString, EventListener)` (or equivalent)
  working end-to-end: a Promise closure passed from `main.pr`, registered
  via Layer 1, and actually invoked when the browser fires the event.

**Out of scope / explicitly defer if it adds real complexity** (note as a
follow-up, don't block on it):

- Multi-argument callbacks, non-void return callbacks, `callback interface`
  (a distinct WebIDL construct — see `parser.go:296-306`, currently just
  parsed as a regular interface and discarded; separate concern from
  `callback` function-type aliases). Layer 1 should not preclude these, but
  implementing them is future work.
- `removeEventListener` / listener lifetime management beyond "don't leak
  and don't crash" — a correct `[resource-drop]`-style release path for a
  registered closure is in scope; a full event-listener-removal API is not.
- WIT `callback` support, if WIT has an equivalent construct — this spec's
  Layer 2 is WebIDL-only; per §4b, a WIT equivalent may fall out for free
  once Layer 1 + the IR function-type case exist, but verifying that is a
  bonus, not a requirement.
- A full public `modules/web` catalog API surface — Layer 1 only needs
  *enough* of a public entry point to prove it's usable outside bindgen
  (§4a); the rest of the catalog module is #17's scope, not this spec's.

## 6. Existing issue tracking (checked 2026-08-18, `promise-language/promise`)

**No open or closed issue, on the fork or upstream, tracks the `callback`
gap.** Checked all 20 issues on `promise-language/promise` (issues are
disabled on the `ssge/promise` fork — use `--repo promise-language/promise`
with `gh`). Two are related but don't cover it:

- **#17 ("Streamline wasm32-web app builds... `web` catalog module",
  internally tracked as `T1505`)** is the parent epic this belongs under —
  its stated north star is exactly "a page that loads and runs in any
  browser... no hand-editing of generated bindings," which `addEventListener`
  is squarely part of, and Layer 1's public entry point (§4a) is a natural
  fit for the `web` catalog module it's tracking. Its listed sub-blockers
  (ABI mismatch, bindgen inheritance, overload collisions, handle
  double-release, string leak) do **not** mention callbacks — this is a
  real gap in that epic's scope, not a duplicate. File the new issue as a
  child of #17.
- **#15 ("WebIDL bindgen: generated JS glue uses canonical-ABI shape...",
  internally tracked as `T1506`)** is a *different, already-written*
  implementation plan that also rewrites `compiler/internal/bindgen/jsglue.go`
  to fix the same four bugs as the `fix/*` branches in §0, but via a
  single native-ABI-matching rewrite rather than four separate surgical
  patches — it explicitly says it "subsumes GitHub #10/#11/#12/#13."
  **Check whether #15/T1506's plan has landed or is in progress before
  touching `jsglue.go`** — if both this callback work and that rewrite
  touch the same file concurrently on different branches, expect conflicts.
  Worth a quick look at whether #15's plan superseded the four merged
  `fix/*` branches, or whether they're independent efforts that happened to
  overlap.

## 7. Constraints (from this repo's own `CLAUDE.md`)

- **Run `bin/verify --wasm-web` before committing.** Formats, vets, and
  runs the full suite including wasm32-web Node tests. Do not commit if it
  fails.
- **Zero memory-leak tolerance.** No `allow_leaks`, no exceptions.
- **File a GitHub issue first**, per `CONTRIBUTING.md` — clear title,
  minimal repro (§1 above works as-is), reference it from commit messages.
  Issues #10/#11/#12/#13 (the four just-fixed bugs) are the pattern to
  follow; there's no existing issue for the callback gap (see §6) — file
  it as a child of #17.
- **Branch off `main`**, not off `local-integration-all-4-fixes` — same
  pattern as the four prior fixes (`fix/webidl-resource-drop-name-mismatch`,
  `fix/wasm-web-bootstrap-glue`, `fix/wasm-web-exit-unwind`,
  `fix/wasm-import-string-abi`). Suggested name:
  `fix/wasm-web-callback-export` (Layer 1) with Layer 2's WebIDL sugar as
  either the same branch or a stacked follow-up — whoever integrates all
  five prior fixes later can decide how to merge them together.
- **The Node test harness (`promise test -target wasm32-web`) will not
  catch a broken trampoline.** Per `PROMISE_COMPILER_BUGS.md`'s "Why the
  existing test suite misses all four" section, it stubs every
  unrecognized `promise_env` import as a no-op — it would happily "pass"
  even if the JS→wasm call direction is completely wrong. Add real Go
  unit/IR-shape tests (`bindgen_test.go` pattern) for Layer 2, plus codegen-
  level unit tests for Layer 1's trampoline emission, **and** a genuine
  (headless) browser integration test, the way Spike B's pixel
  verification did (Playwright + system Chromium at
  `/usr/bin/chromium-browser` is already proven to work in this
  environment).

## 8. Acceptance criteria

**Layer 1 (general capability):**

1. A hand-written `wasm32-web` Promise program — no bindgen involved — can
   export a closure via the Layer 1 primitive and have JS genuinely invoke
   it (verified via a browser integration test, not just "compiles").
2. The trampoline/handle-table naming is derived from one shared helper
   used by both the Promise-side and JS-side generators, per §3's Bug-1
   lesson — not duplicated logic that can drift.

**Layer 2 (WebIDL `callback` sugar / original repro):**

3. The exact repro in §1 compiles clean — no `undefined type` error.
4. A Promise closure can be passed to `Element.add_event_listener(type,
   listener)` and is genuinely invoked by the browser firing that event —
   not just "compiles," actually called.
5. Demonstrated with a real interaction, not a static render: e.g. a
   `mousedown`/`mousemove` pair on a canvas moves a rectangle, or a `click`
   increments a Promise-side counter — verified by reading back observable
   state after a real Playwright-driven browser event (pixel data via
   `getImageData`, or an exported getter), matching the rigor of Spike B's
   original pixel verification. Compiling and "not crashing" is not enough
   — that was exactly the blind spot the four prior bugs hid behind.

**Both layers:**

6. `bin/verify --wasm-web` passes, zero new leaks.
7. GitHub issue filed (as a child of #17) and referenced in commits.
