# Documentation Index

## Language

- [language-design.md](language-design.md) — Full language specification: types, ownership, errors, generics, modules, concurrency. §6 is the normative ownership & memory model.
- [language-guide.md](language-guide.md) — Concise reference for writing correct Promise code.
- [code-style.md](code-style.md) — Conventions for Promise source: field/getter naming, `` `final ``, comments.
- [large-integers.md](large-integers.md) — Plan for native i128/u128, i256/u256, i512/u512 primitive types backed by LLVM iN.

## Compiler and Runtime

- [runtime-architecture.md](runtime-architecture.md) — PAL abstraction, build pipeline (opt/llc/lld/musl), M:N scheduler, all in codegen-emitted LLVM IR.
- [formatting.md](formatting.md) — Canonical code formatter with zero configuration.
- [debugging.md](debugging.md) — Current state of debugging support (DWARF metadata not yet emitted).

## Standard Library and Modules

- [standard-library.md](standard-library.md) — Stdlib design: small orthogonal modules, implementation in Promise over IR.
- [module-system.md](module-system.md) — Mono-versioned global catalog with atomic epoch releases.
- [platform-modules.md](platform-modules.md) — Platform-facing stdlib boundary and module layout under `modules/`.
- [creating-modules.md](creating-modules.md) — Step-by-step guide for proposing, implementing, and shipping new catalog modules.
- [community-catalog.md](community-catalog.md) — The community module tier: a decentralized git registry, its CI, and per-epoch compatibility records.
- [serialization-plan.md](serialization-plan.md) — Encode/Decode architecture for agent-friendly serialization.

## Platform Targets

- [installing.md](installing.md) — Installing the compiler: platform install scripts, `~/.promise/` layout, `promise update`.
- [distribution.md](distribution.md) — Install model: thin/full binaries, content-addressed dependency store, the Promise stub, epoch dispatch.
- [release-automation.md](release-automation.md) — GitHub release pipeline: prebuilt blobs, hash-embedded manifest, thin/full + stub builds, publishing.
- [windows-support.md](windows-support.md) — Native MSVC ABI, Windows SDK, self-contained compiler binary.
- [wasm-bindings.md](wasm-bindings.md) — WIT/WebIDL ingestion for safe WASM host bindings.
- [wasm-web-callbacks.md](wasm-web-callbacks.md) — Host→guest callbacks on `wasm32-web`: the reactor execution model, bounded pumps, and event channels.
- [size-optimization.md](size-optimization.md) — Binary size tracking and regression prevention across all targets.

## Infrastructure

- [../CONTRIBUTING.md](../CONTRIBUTING.md) — Contributor/maintainer onboarding: build the compiler, run tests, verify, and gates.
- [build-tools.md](build-tools.md) — Build tooling architecture and the `bin/` tool inventory.
- [gate-system.md](gate-system.md) — Four-class regression prevention gates (tests, memory, stability, size, performance).
- [tags.md](tags.md) — Canonical tag vocabulary and tagging rules for the `tracker` MCP server.
- [epoch-versioned-installs.md](epoch-versioned-installs.md) — Side-by-side multi-epoch compiler installations.
- [platform-documentation.md](platform-documentation.md) — `promise doc` system for extracting `doc()` meta tags.

## Vision

- [ai-platform.md](ai-platform.md) — Promise as an AI-centric platform: MCP servers, agent orchestration, sandboxed execution.
- [schema.md](schema.md) — Type-driven schema generation: compile-time descriptors with stable content-addressed identity.
- [cloud-persistence.md](cloud-persistence.md) — Design proposal: durable, schema-driven, multi-process shared state.
- [ui.md](ui.md) — Design proposal (Draft / RFC): the `ui` module across native desktop, WASM, and terminal.
- [github-description.md](github-description.md) — Project summary for GitHub.

## Dormant / Historical

- [binding-architecture.md](binding-architecture.md) — C binding via extern ABI coercion and header generation (implemented but dormant).

## Archived

- [archive/stages.md](archive/stages.md) — Compiler implementation roadmap. All open items migrated to the tracker.
