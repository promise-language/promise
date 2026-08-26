# Documentation Index

This is the map of `docs/`. It is the one file in the root that is not a specification —
everything else there is.

## How to read this tree

| Location | What a file there is | Binding? |
|----------|----------------------|----------|
| `docs/` root | A **specification**: what the project *should* be — the intended end state. It never records current state, progress, or phasing. | **Yes.** Work that contradicts a root doc must stop and be resolved — amend the doc, adjust the item, or reject it — not shipped as a quiet deviation. |
| `docs/proposals/` | An end state that has **not been ratified** — a draft, an RFC, a direction still under discussion. | No. Ratifying one means `git mv` into the root (and giving it a tag). |
| `docs/archive/` | An end state that has been **superseded or delivered** — kept for history. | No. |
| `docs/research/` | Background analysis feeding a decision — an assessment, not a design. | No. |

**Where progress lives.** A root doc has no status section, because status is not a property of
a specification. Each one instead declares a **tag** on the line under its title, and the gap
between the end state and today is the set of open tracker items carrying that tag:

> **Tag:** `large-integers` — remaining work to complete this document: `mcp__tracker__list --tag large-integers`

See [tags.md](tags.md) §2.6 for the document-tag facet and how it composes with the other tags.

**The one exception.** A doc may keep an inline "not yet implemented" marker where the gap
*changes what compiles today* — [language-design.md](language-design.md) says hybrid types fit
the four-struct model and, in the same breath, that mixing `` `value `` and instance fields is
currently a compile error. Deleting that would make the spec describe a program the compiler
rejects. Such a marker must name its tracker item (`tracked as T1723`) so it stays a pointer
rather than becoming a status section again — and it is for user-visible language semantics
only, never for how far along an internal migration is.

**A marker expires with its item.** Naming the item is what makes the marker removable: when
that item closes, the gap it described is gone and the marker must go with it, in the same
change that closes the item. A marker whose item is already closed is worse than no marker —
it tells a reader the compiler rejects something it now accepts. Sweep for stragglers with:

```sh
grep -rn 'tracked as T[0-9]' docs/*.md      # then check each item's status
```

---

## Language

- [language-design.md](language-design.md) — Full language specification: types, ownership, errors, generics, modules, concurrency. §6 is the normative ownership & memory model.
- [language-guide.md](language-guide.md) — Concise reference for writing correct Promise code.
- [code-style.md](code-style.md) — Conventions for Promise source: field/getter naming, `` `final ``, comments.
- [large-integers.md](large-integers.md) — Native `i128`/`u128`, `i256`/`u256`, `i512`/`u512` primitive types backed by LLVM `iN`.

## Compiler and Runtime

- [runtime-architecture.md](runtime-architecture.md) — PAL abstraction, build pipeline (opt/llc/lld/musl), M:N scheduler, all in codegen-emitted LLVM IR.
- [formatting.md](formatting.md) — Canonical code formatter with zero configuration.

## Standard Library and Modules

- [standard-library.md](standard-library.md) — Stdlib design: small orthogonal modules, implementation in Promise over IR.
- [module-system.md](module-system.md) — Mono-versioned global catalog with atomic epoch releases.
- [platform-modules.md](platform-modules.md) — Platform-facing stdlib boundary and module layout under `modules/`.
- [creating-modules.md](creating-modules.md) — Step-by-step guide for proposing, implementing, and shipping new catalog modules.
- [community-catalog.md](community-catalog.md) — The community module tier: a decentralized git registry, its CI, and per-epoch compatibility records.
- [io.md](io.md) — File I/O contract: atomic replace, forcing data to stable storage, advisory locking.
- [serialization.md](serialization.md) — Encode/Decode architecture for agent-friendly serialization.
- [schema.md](schema.md) — Type-driven schema generation: compile-time descriptors with stable content-addressed identity.

## Platform Targets

- [installing.md](installing.md) — Installing the compiler: platform install scripts, `~/.promise/` layout, `promise update`.
- [distribution.md](distribution.md) — Install model: thin/full binaries, content-addressed dependency store, the Promise stub, epoch dispatch.
- [release-automation.md](release-automation.md) — GitHub release pipeline: prebuilt blobs, hash-embedded manifest, thin/full + stub builds, publishing.
- [windows-support.md](windows-support.md) — Native MSVC ABI, Windows SDK, self-contained compiler binary.
- [wasm-bindings.md](wasm-bindings.md) — WIT/WebIDL ingestion for safe WASM host bindings.
- [web-apps.md](web-apps.md) — Promise on the web: the `wasm32-web` reactor execution model, host→guest delivery, bounded pumps, and event channels.
- [size-optimization.md](size-optimization.md) — Binary size across targets: canaries, the size gate, `promise size`, and the optimization ladder. WASM is covered today.

## Infrastructure

- [../CONTRIBUTING.md](../CONTRIBUTING.md) — Contributor/maintainer onboarding: build the compiler, run tests, verify, and gates.
- [build-tools.md](build-tools.md) — Build tooling architecture and the `bin/` tool inventory.
- [gate-system.md](gate-system.md) — Four-class regression prevention gates (tests, memory, stability, size, performance).
- [tags.md](tags.md) — Canonical tag vocabulary and tagging rules for the `tracker` MCP server.
- [platform-documentation.md](platform-documentation.md) — `promise doc` system for extracting `doc()` meta tags.

## Vision

- [ai-platform.md](ai-platform.md) — Promise as an AI-centric platform: MCP servers, agent orchestration, sandboxed execution.
- [cloud-persistence.md](cloud-persistence.md) — Durable, schema-driven, multi-process shared state.

---

## Proposals — not binding

An end state under discussion. Ratifying one moves it into the root above.

- [proposals/debugging.md](proposals/debugging.md) — Source-level debugging: threading `Pos` through codegen as DWARF metadata so `lldb`/`gdb` can break by line.
- [proposals/ui.md](proposals/ui.md) — Draft / RFC: the `ui` module across native desktop, WASM, and terminal.

## Research — not binding

Background analysis feeding a decision.

- [research/liquid-haskell-refinement-types.md](research/liquid-haskell-refinement-types.md) — Assessment of whether Liquid Haskell-style refinement types should map onto Promise's type system.

## Archive — superseded or delivered

- [archive/stages.md](archive/stages.md) — Compiler implementation roadmap. All open items migrated to the tracker.
- [archive/binding-architecture.md](archive/binding-architecture.md) — C binding via extern ABI coercion and generated headers. The `extern` ABI coercion it introduced is live and documented in [runtime-architecture.md](runtime-architecture.md); header generation was built, never wired to the CLI, and its motivating C runtime no longer exists.
- [archive/epoch-versioned-installs.md](archive/epoch-versioned-installs.md) — Phased plan for side-by-side epoch installs. Delivered, and its layout and dispatch model superseded by [distribution.md](distribution.md) §1.3, §2.5, §4.
- [archive/generic-inheritance-method-generics.md](archive/generic-inheritance-method-generics.md) — Generic inheritance and method-level generics.
- [archive/phase3-remote-modules.md](archive/phase3-remote-modules.md) — Remote module fetching via git.
- [archive/subscript-slice-operators.md](archive/subscript-slice-operators.md) — Operator method dispatch expansion for subscript and slice.
