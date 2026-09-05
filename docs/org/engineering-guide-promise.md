# Engineering Guide — Promise

> **Tag:** `engineering-guide-promise` — remaining work to complete this document: the query
> named in `docs/index.md`.

> **Home:** [promise-language/org](https://github.com/promise-language/org) — this document is
> distributed into each managed project as `docs/org/`. A copy is never edited in place: to
> change it, file an issue against `org`.

The [engineering guide](engineering-guide.md) applied to Promise source (`.pr` files). Nothing here
contradicts it; everything here is Promise-specific form. The abbreviation dictionary this language
uses is §9.3a of Promise's
[`docs/language-design.md`](https://github.com/promise-language/promise/blob/main/docs/language-design.md).

## Fields, getters, and construction

- **Private fields are `_`-prefixed; the public getter drops the underscore.** The underscore marks
  an implementation detail and signals that access goes through the getter. A field that is itself
  the public API is exposed directly — no `_field` + getter pair for symmetry; adding a getter is
  what justifies the underscore.
- **Construction-only fields are `` `final ``.** It prevents later mutation, documents intent where
  it is declared, and lets the compiler assume the value never changes.
- **Construct through factory methods on the type** — `Response.ok(...)`, `Server.bind(...)` — not
  free functions. A factory can set `` `final `` fields and lives with the type's other methods.
- **A getter is a cost signal.** `get name T` means side-effect-free *and* cheap — field-like,
  O(1), like `len` or `is_empty`. Anything that allocates or computes is a method even when
  parameterless: `to_string()`, `clone()`, `bytes()`. The parentheses tell a caller that work
  happens. **Interface conformance overrides this**: where a `` `structural `` interface declares a
  getter (`Hashable` declares `get hash int`), every implementor matches the form even when its own
  implementation is O(n) — a uniform shape across the hierarchy is worth more than the per-type
  signal.

## Optionals

**Absence is `T?`** — the guide's absence rule in Promise form. Reading one back:

```promise
if this._field is present { return this._field!.clone(); }
```

`if v := this._field` moves out of the field, `!= none` is not defined, and an optional carries no
members of its own. Inside an `if x is present` body a **local** is narrowed and needs no `!`; a
field is not, and still does. Narrowing does not cross `&&` or reach the statement after the `if`,
so nest the checks.

## Documentation

- **`` `doc("…") `` on every `` `public `` declaration** — types, methods, functions, getters.
- **A synthesized member is exempt, because it has no declaration to annotate.** A type marked
  `` `clone `` publishes a `clone` nobody wrote, and the annotation on the type is the
  documentation. Writing the member out by hand to have somewhere to hang a `` `doc `` trades a
  guarantee the compiler makes for a sentence a reader has to trust — prefer the annotation
  wherever it compiles, and where it does not, the hand-written member is a workaround and its
  `` `doc `` says which defect it is waiting on.

## Testing

- **Prefer batch tests** — functions tagged `` `test `` using `assert()` — over snapshot tests.
  Cost is dominated by binaries compiled, not by execution, and batch tests compile into one.
- **Co-locate**: `*_test.pr` beside the `.pr` file it tests.
