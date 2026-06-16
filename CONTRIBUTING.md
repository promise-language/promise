# Contributing to Promise

This guide is for people who want to work **on the Promise project** — the
compiler, standard library, build tooling, and tests — rather than write
programs *in* Promise. If you just want to use the language, start with the
[Language Guide](docs/language-guide.md) instead.

Promise is a statically-typed language whose compiler is a single Go binary
(`bin/promise`). It parses with ANTLR4, type-checks and runs ownership analysis,
then emits LLVM IR and links a native (or WASM) binary. There is no C runtime —
the scheduler, allocator, and PAL are all codegen-emitted LLVM IR. See
[docs/runtime-architecture.md](docs/runtime-architecture.md) for the big picture.

> **Before you start:** by contributing you agree to license your work under the
> project's **Contributor License Agreement** — the CLA bot prompts you
> automatically on your first pull request (see [INDIVIDUAL_CLA.md](INDIVIDUAL_CLA.md),
> or [CORPORATE_CLA.md](CORPORATE_CLA.md) if you're contributing on behalf of an
> employer). You keep your copyright; the CLA grants the project a license. All
> participation is governed by our [Code of Conduct](CODE_OF_CONDUCT.md).

## 1. Prerequisites

- **Go 1.25+** (the only thing needed to build the build tools)
- **Java** — used by ANTLR4 to regenerate the parser
- **clang** and **LLVM 22+** (`opt`, `llc`, `lld`)
- **wasmtime** and **Node.js** — only for the WASM test targets

Run `bin/prereqs` at any time to check your toolchain and see exactly what is
missing:

```sh
bin/prereqs
```

## 2. Bootstrap (once per clone)

```sh
./make
```

`./make` compiles every build tool under `tools/` into `bin/` and enables the
project's git hooks. It is the **only** entry point you bootstrap with, and it is
idempotent and fast — re-run it whenever you pull changes under `tools/` (each
`bin/` tool embeds a hash of its source and refuses to run when stale, telling
you to re-run `./make`).

`./make` also builds the optional **flow** binaries used by the project's tracker
automation; those live in two git submodules (`flow-sdk/`, `flow/`). Access to
them is optional — if they can't be fetched, `./make` warns and skips them, and
everything below still works. See [§12](#12-the-flows-automation-optional).

## 3. Build the compiler

```sh
bin/build              # debug build  → bin/promise
bin/build --release    # release build (embedded LLVM tools, LTO)
```

**Never run `go build` directly.** It skips ANTLR parser generation and resource
embedding and produces a broken binary. Always go through `bin/build`.

Debug and release use different backend pipelines (debug: `opt → llc → .o →
ld --gc-sections`; release: `opt → .bc → lld --lto`). Details and the full tool
inventory are in [docs/build-tools.md](docs/build-tools.md).

## 4. Run Promise code

```sh
bin/promise run file.pr                 # build + run
bin/promise exec 'print_line("hi")'     # run a snippet
bin/promise build file.pr               # build only
bin/promise emit-ir file.pr             # print the LLVM IR
```

## 5. Test

```sh
bin/test                 # build + run all tests (Go + Promise)
bin/test go              # Go unit tests only
bin/test promise         # Promise tests only
bin/test --wasm          # also run the wasm32-wasi target (wasmtime)
bin/test --wasm-web      # also run the wasm32-web target (Node.js)
bin/test --clean         # clear caches first
```

Narrower loops while iterating:

```sh
# A single Go package or test (run from compiler/)
go test ./internal/codegen/ -count=1
go test ./internal/codegen/ -run TestDropBasicScopeExit -v -count=1

# A single Promise test file
bin/promise test tests/e2e/basics.pr
```

Coverage and flaky-test detection:

```sh
bin/coverage             # Go + Promise coverage across the project
bin/stress               # run the suite repeatedly to surface flaky tests
```

Significant changes should add tests at **both** levels — Go unit tests that
assert IR/diagnostic shape (`generateIR`/`assertContains`, `checkErrs`/
`expectError`, `ownerOK`/`ownerErrs`) and Promise-level tests that assert runtime
behavior (prefer batch `` `test `` functions with `assert()`).

## 6. Verify before committing

```sh
bin/verify --wasm
```

This is the canonical pre-commit check: it formats Go and Promise code, runs
`go vet`, and executes the full test suite including the WASM target. **Always
run it before committing**, and do not commit if it fails. Add `--shared` to use
the shared `~/.promise` cache instead of the per-clone `.promise-home/`.

## 7. Gates & git hooks

`./make` points git at `.githooks/`, whose `pre-commit` hook delegates to
`bin/precommit`. The project enforces quality with a layered **gate system**:

- **Edit gates** (sub-second, on every file edit) — e.g. block `allow_leaks:
  true` from entering `.pr` files.
- **Commit gates** (after verify) — the leak count must not increase and the test
  count must not decrease.
- **Periodic / platform gates** — stress, coverage, binary size, and
  cross-platform verification, scheduled via the tracker.

A standing rule worth internalizing early: **zero memory leaks.** The repo has 0
leaks and 0 `allow_leaks` tags; any leak in verify output is a regression, and
`allow_leaks: true` is never an acceptable fix. The full design is in
[docs/gate-system.md](docs/gate-system.md).

If a `bin/` tool reports it is "stale," just run `./make` — that's the guard that
keeps you from running outdated tooling.

## 8. Caches and a clean slate

Build artifacts and test caches live in `.promise-home/` (per clone, the
default) or `~/.promise` (shared, via `--shared`). To get back to a pristine
state:

```sh
bin/clean                # wipe .promise-home/ + Go test cache
bin/clean --shared       # wipe ~/.promise instead
```

## 9. Repository layout

```
compiler/        The compiler: grammar/, ast/, internal/{sema,ownership,codegen,...}
modules/         Standard library (modules/std/) + catalog modules (io, json, net, ...)
tests/           Cross-cutting integration and e2e tests
examples/        Runnable example programs (also run as tests)
tools/           Build tooling (a separate Go module → compiled to bin/)
bin/             Compiled build tools + the compiler (bin/promise)
flows/           Tracker-automation flow binaries (optional; see §12)
docs/            Design docs and references — start at docs/index.md
```

## 10. Conventions

- **Never `go build` the compiler directly** — use `bin/build`.
- When you change `modules/std/*.pr`, run `bin/build` to re-embed the updated
  stdlib.
- Public declarations carry `` `doc `` annotations; APIs use full English words
  (with the approved abbreviation dictionary).
- Prefer implementing stdlib behavior in Promise over hand-written IR; reach for
  `native` only when direct memory/runtime access is unavoidable.
- **Don't work around compiler/language/test-infra bugs.** File them instead (see
  below) and keep module/test code honest.

Repository-wide conventions — including the exact build/test command reference and
the implementation philosophy — also live in [CLAUDE.md](CLAUDE.md) (written for
AI agents, but equally useful to human contributors). For style specifics see
[docs/code-style.md](docs/code-style.md) and [docs/formatting.md](docs/formatting.md).

## 11. Bugs, tasks, and follow-ups

Bugs and work items are tracked in the project's **tracker** (an MCP server), which
auto-assigns stable IDs by type (`B0001` bugs, `T0001` tasks, `D0001` deferred).
Reference those IDs from code comments and commit messages. When you hit a
compiler bug or language limitation, file it rather than hacking around it.

**Reporting from outside the core team:** the tracker is the maintainers' internal
workflow and isn't publicly accessible. If you don't have tracker access, open a
regular **[GitHub Issue](https://github.com/promise-language/promise/issues)**
instead — use the bug-report or feature-request template — and a maintainer will
triage it into the tracker. A GitHub Issue is always a fine way to report a bug or
propose a change.

## 12. The flows automation (optional)

The `flows/` module and the `flow/` + `flow-sdk/` submodules implement the
project's **tracker resolution automation** (the `do` flow that drives an item
through plan → implement → review → coverage → commit → push → summary →
inspect). They are **not** part of building or testing the compiler and are not
covered by `bin/verify`. `./make` checks the submodules out automatically and
builds `bin/flow/do`; if you lack access to the submodule hosts, the build skips
them with a warning and the compiler workflow is unaffected. The `flows/` module
has its own Go tests (`cd flows && go test ./...`).

## Where to go next

- [docs/index.md](docs/index.md) — the full documentation index
- [docs/build-tools.md](docs/build-tools.md) — build system architecture and tool inventory
- [docs/gate-system.md](docs/gate-system.md) — regression-prevention gates
- [docs/runtime-architecture.md](docs/runtime-architecture.md) — pipeline, PAL, M:N scheduler
- [docs/debugging.md](docs/debugging.md) — debugging the compiler and generated code
- [docs/creating-modules.md](docs/creating-modules.md) — proposing and shipping a catalog module
- [docs/language-design.md](docs/language-design.md) — full language specification
