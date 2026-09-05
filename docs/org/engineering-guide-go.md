# Engineering Guide — Go

> **Tag:** `engineering-guide-go` — remaining work to complete this document: the query named in
> `docs/index.md`.

> **Home:** [promise-language/org](https://github.com/promise-language/org) — this document is
> distributed into each managed project as `docs/org/`. A copy is never edited in place: to
> change it, file an issue against `org`.

The [engineering guide](engineering-guide.md) applied to Go source. Nothing here contradicts it;
everything here is Go-specific form. Go has no org abbreviation dictionary; the standard library's
own names (`Len`, `Dir`, `Env`) are the vocabulary.

## Shape

- **`gofmt` output is the only form.** Nothing hand-aligned, no alternative formatters.
- **No `init()` and no package-level mutable state** — the guide's no-hidden-effects rule in Go
  form. Construction is explicit: a `New*` function returning the value, errors and all.
- **Absence is a pointer or an `ok` bool at the boundary, never a zero value with meaning.** A
  `*Config` that is nil is absent; a `Config{}` that means "no config" is a sentinel.
- **Identities are named types** — `type ItemID string` — so the compiler catches a project id
  where an item id belongs. Conversions happen at the boundary, once.
- **A quantity is `time.Duration` / `time.Time`**, never an `int` of implied units or a formatted
  string.

## Errors

- **Every error is handled or returned — never discarded with `_`.** A discarded error is a hidden
  effect.
- **Wrap with context**: `fmt.Errorf("reading manifest: %w", err)` — the chain names each boundary
  crossed, and `errors.Is`/`errors.As` still work.
- **Fail closed.** A check that cannot run reports an error; it never returns success because the
  precondition was missing.
- **When several independent checks run, report all failures** — `errors.Join` — not just the
  first.
- **No `panic` across a package boundary.** A panic is for a programming error inside the package,
  never a result.

## Concurrency and lifecycle

- **Every goroutine has a defined stop.** A goroutine with no way to end is a leak — the guide's
  silent-classes rule; goroutines, handles, and child processes are Go's leak classes.
- **`context.Context` is the first parameter of anything that waits, calls out, or can be
  cancelled** — and it is honoured, not just accepted.
- **A spawned subprocess is tracked and dies with its parent** — process groups, not orphans; the
  tooling contract owns the mechanics.

## Testing

- **Table-driven tests** for behaviour with variants; subtests named so a failure names its case.
- **Co-locate**: `*_test.go` beside the file it tests.
- **No `time.Sleep` for synchronization** — channels, `sync.WaitGroup`, or a ready signal; the
  guide's time rule applies verbatim.
