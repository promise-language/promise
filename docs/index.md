# Documentation Index

This is the map of `docs/`. It is the one file in the root that is not a specification —
everything else there is.

## How to read this tree

A file's directory determines whether it binds: everything in the `docs/` root is a specification, while `proposals/`, `research/` and `archive/` are not. [normative.md](normative.md) has the rules — what makes a document binding, the header every specification carries, why none of them contains a status section, and the one-fact-one-home rule that keeps two of them from disagreeing.

---

## Language

- [language-design.md](language-design.md) — Full language specification: types, ownership, errors, generics, modules, concurrency. §6 is the normative ownership & memory model.
- [language-guide.md](language-guide.md) — Concise reference for writing correct Promise code.
- [annotations.md](annotations.md) — Normative reference for every annotation: what each one means, its targets and parameters, and the one-declaration rule that keeps a property from being declared twice.
- [memory-model.md](memory-model.md) — What may allocate: the closed set of variable-size heap primitives, by-value versus behind-a-handle ownership, and why every other container is composition.
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
- [normative.md](normative.md) — How normative documents work: location as the binding rule, the tag header, why status lives in the tracker, and the ban on one fact having two homes.
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
