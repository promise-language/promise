# Module System Proposal — Global Catalog with Mono-Versioning

This proposal replaces Sections 4 and 21 of the language design doc with a fundamentally different model: a **mono-versioned global catalog** where every module exists at exactly one version per catalog release, and all modules are guaranteed to work together.

---

## 1. Motivation

### 1.1 The Version Hell Problem

Every modern package ecosystem suffers from version management complexity:

- **npm**: Diamond dependencies, phantom `node_modules` trees, "works on my machine" failures, lockfile merge conflicts, multiple versions of the same package in one build.
- **Cargo/Rust**: Better than npm but still requires manual version resolution, `Cargo.lock` churn, and ecosystem-wide breakage from semver-incompatible releases.
- **Go modules**: Minimum version selection helps, but major version suffixes (`/v2`, `/v3`) fragment the ecosystem and create import path confusion.
- **Python/pip**: No isolation by default, global state conflicts, `requirements.txt` vs `pyproject.toml` vs `setup.py` — three ways to do the same thing.

All of these systems share a fundamental assumption: **each package versions independently.** This means the user (or their AI agent) must solve a constraint satisfaction problem at build time: "find a set of versions where everything is compatible." This problem is NP-complete in general, and even when solvable, produces builds that no one has ever tested in that exact combination.

### 1.2 Why This Matters for AI Agents

Promise is designed for AI-agent efficiency. Version management is the single worst failure mode for AI-generated code:

1. **Version guessing.** An AI trained on code from 2024 generates `import foo v2.3` but the latest is `v4.1` with a completely different API. The code compiles against the wrong version or doesn't compile at all.
2. **Compatibility reasoning.** "Does `json v3` work with `http v2`?" requires knowledge that exists nowhere in the source code — it's in changelogs, GitHub issues, and tribal knowledge. AI agents cannot reliably answer this.
3. **Configuration overhead.** Writing `Cargo.toml`, `package.json`, or `go.mod` is boilerplate that adds zero information but creates opportunities for errors. An AI agent generating a self-contained program shouldn't need to also generate a dependency manifest.
4. **Non-determinism.** Two agents generating the same logical program may produce different dependency versions, leading to different behavior. This violates "one obvious way."

### 1.3 The Insight: Version the World, Not the Parts

Operating system distributions solved this decades ago. Debian stable, Ubuntu LTS, and NixOS all ship a **curated set of packages tested together as a unit**. You don't pick individual package versions — you pick a distribution release, and everything is guaranteed to work.

Promise applies this model to a programming language ecosystem:

- There is **one catalog** containing every available module.
- The catalog has **releases** (epochs). Each epoch is a snapshot where every module has exactly one version, and all modules have been tested together.
- When you `promise sync`, you get a compiler + standard library + access to every catalog module, all at the same epoch.
- Your project declares which epoch it targets. That's it. No dependency versions, no lockfiles, no resolution.

---

## 2. Design Principles

Every decision in this proposal is evaluated against Promise's core principles:

| Principle | How the catalog model serves it |
|-----------|-------------------------------|
| **Self-contained readability** | `use io` is unambiguous — there's exactly one `io` module at your epoch. No version in the import means no version to get wrong. |
| **Explicit over implicit** | The epoch is explicit in `promise.mod`. Everything else follows deterministically from it. No implicit resolution, no heuristics. |
| **Minimal context needed** | An AI agent only needs to know the epoch to generate correct imports. The catalog is finite, documented, and stable within an epoch. |
| **One obvious way** | There is exactly one version of each module. No version ranges, no "compatible" vs "latest" vs "pinned." No lockfiles to maintain. |
| **No hidden effects** | Build output is fully determined by source code + epoch. No resolver surprises, no phantom dependency updates. |

---

## 3. The Global Catalog

### 3.1 What It Is

The **catalog** is a curated, tested, mono-versioned set of modules that constitutes the entire Promise module ecosystem. It includes:

- **The standard library** (`io`, `math`, `strings`, `crypto`, `http`, `json`, etc.)
- **Community modules** that have been submitted, reviewed, and accepted
- **The compiler itself** — the `promise` binary is versioned with the catalog

The catalog is a single git repository. Each commit represents a state of the world where every module has a specific version. Tagged commits are **epochs** — stable release points.

```
promise-catalog/
  catalog.toml              # module registry: names → source repos + commits
  tests/                    # cross-module integration tests
    io_json_test.pr
    http_crypto_test.pr
    ...
```

The standard library source lives in the compiler repository (as it does today via `std/`) and is embedded in the compiler binary. The catalog references it by pinning a compiler commit — when you sync to an epoch, the compiler binary includes the matching std. Community modules live in their own repositories and are referenced by commit hash in `catalog.toml`.

### 3.2 `catalog.toml`

The catalog manifest maps module names to their source locations and pinned commits:

```toml
[catalog]
epoch = "2026.3"
compiler = "b4e7f2a"         # compiler repo commit for this epoch

[modules.json]
repo = "github.com/promise-lang/json"
commit = "a1b2c3d"
description = "JSON parsing and serialization"

[modules.http]
repo = "github.com/promise-lang/http"
commit = "e4f5a6b"
description = "HTTP client and server"
requires = ["json", "crypto"]

[modules.crypto]
repo = "github.com/promise-lang/crypto"
commit = "7c8d9e0"
description = "Cryptographic primitives"
```

Key properties:
- **Flat namespace.** Module names are simple identifiers (`json`, `http`, `crypto`). No URLs, no paths, no version numbers in names.
- **Pinned commits.** Each module points to an exact commit hash. No ranges, no "latest", no resolution.
- **Declared dependencies.** Each module lists which other catalog modules it requires. These are validated by the catalog CI — circular dependencies are rejected.

### 3.3 What's NOT in the Catalog

The catalog is curated, not exhaustive. Not every Promise module needs to be in it. Modules outside the catalog are called **external modules** and are imported via URL-based `use` declarations with per-module commit pinning (see Section 9).

The catalog aims to cover the "90% use case" — the modules that most programs need. Think of it as a comprehensive standard library rather than a package registry.

---

## 4. Epochs

### 4.1 What Is an Epoch?

An **epoch** is a tagged release of the catalog. It is a single identifier (not a semantic version) that uniquely determines:

- The version of the `promise` compiler
- The version of every standard library module
- The version of every community module in the catalog
- The set of cross-module integration tests that passed

Epoch names follow the format `YYYY.N` where `YYYY` is the year and `N` is a sequential release number within that year:

```
2026.1    — first stable release of 2026
2026.2    — second release (may include breaking changes)
2026.3    — third release
2027.1    — first release of 2027
```

### 4.2 Epoch Guarantees

Within an epoch, the following are guaranteed:

1. **All catalog modules compile.** Every module builds successfully with the epoch's compiler.
2. **All unit tests pass.** Every module's own test suite passes.
3. **All integration tests pass.** Cross-module tests covering common interaction patterns pass.
4. **API stability.** The epoch is immutable once tagged. No silent updates, no "re-releases."

### 4.3 Epoch Channels

There are two channels at any given time:

| Channel | Description |
|---------|-------------|
| `stable` | The latest tagged epoch. Production-ready. |
| `next` | The upcoming epoch, under active development. May have breaking changes from `stable`. Used for testing migration before the cut. |

There is no `nightly` or `beta`. The `next` channel serves the purpose of both — it's where you test upcoming changes. When `next` passes all tests and is deemed ready, it becomes the new `stable` and a new `next` begins.

### 4.4 Breaking Changes Between Epochs

Epochs **may contain breaking changes.** This is by design — it's the mechanism by which the ecosystem evolves. However:

- Breaking changes are **coordinated.** If module `http` changes its API, all catalog modules that depend on `http` are updated in the same epoch. Users see zero breakage within an epoch.
- Breaking changes are **documented.** Each epoch has a migration guide listing what changed and how to update your code.
- Breaking changes are **atomic.** You never see "half-migrated" state. You're either on epoch `2026.2` (old API) or `2026.3` (new API). Both work completely.

This is radically simpler than semver, where a breaking change in one package can cascade unpredictably through the dependency graph and requires each downstream maintainer to independently update.

---

## 5. Import Syntax

### 5.1 `use` Declarations

The `use` keyword imports a module by its catalog name:

```promise
use io
use json
use http
```

That's it. No URLs, no version numbers, no string literals. The module name is a bare identifier that maps to a catalog entry. The epoch (declared in `promise.mod`) determines which version of each module you get.

**Qualified access** — all references to imported names use the module name as a prefix:

```promise
use io
use json

main() {
    data := json.parse('{"name": "Alice"}')
    io.println(data)
}
```

### 5.2 Aliasing

If a module name is inconvenient, alias it with `as`:

```promise
use json as j

main() {
    data := j.parse("{}")
}
```

### 5.3 No Unqualified Imports

There is no `from json use parse` or `use json { parse }`. Every reference is qualified with the module name (or alias). This serves two purposes:

1. **Self-contained readability.** When you see `json.parse(...)` anywhere in the file, you know exactly where `parse` comes from without scanning import lists.
2. **One obvious way.** There's no choice between qualified and unqualified imports. No style debates, no inconsistency across codebases.

### 5.4 External Module Imports (URL-based)

For modules **not in the catalog** — private libraries, experimental packages, pre-catalog prototypes — use URL-based imports with an explicit alias:

```promise
use parser "github.com/someone/promise-parser"
use internal "git.corp.com/team/internal-utils"

main() {
    parser.parse("input")
    internal.do_thing()
}
```

URL-based imports use the same `use alias "url"` syntax from the original language design. The alias is mandatory and is the only way to reference the module's exports. The URL points to a git repository containing a `promise.mod` at its root.

**Version pinning for external modules:** External modules are not epoch-managed, so they need explicit version pinning. This is declared in `promise.mod` (see Section 6.2).

### 5.5 Local Module Imports

For project-local modules (sub-directories with their own `promise.mod`), use a relative path:

```promise
use "./models"
use "./utils" as u

main() {
    user := models.User(name: "Alice")
    u.format(user)
}
```

Relative imports always start with `./` or `../` to distinguish them from catalog modules. The path points to the directory containing the target `promise.mod`. The default alias is the `module` name declared in that module's `promise.mod` — use `as` to override it. If the module name conflicts with another import in the same file, an explicit alias is required.

### 5.6 Standard Library — Just Part of the Catalog

From the user's perspective, `use io` and `use json` look and work identically — both are catalog modules resolved by the epoch. The only difference is operational: **core modules** ship embedded in the compiler binary and work offline immediately after `promise sync`, while **community modules** are fetched on first use.

Core modules (embedded in compiler):

```
io          — console I/O, file I/O, formatting
math        — numeric functions, constants
strings     — string manipulation beyond built-in methods
collections — additional data structures beyond Vector/Map
testing     — test utilities, assertions, benchmarks
sync        — synchronization primitives beyond channels
os          — environment, process, signals
```

Community catalog modules (fetched on demand):

```
crypto      — cryptographic primitives
net         — TCP/UDP sockets
http        — HTTP client/server
json        — JSON parsing/serialization
time        — clocks, durations, formatting
```

The boundary between core and community is a packaging decision, not a language one. Modules can move between tiers across epochs.

### 5.7 Two Tiers of Import

The import syntax has two forms, reflecting two tiers of the ecosystem:

| Form | Tier | Example | Resolution |
|------|------|---------|------------|
| `use name` | Catalog | `use json` | Looked up in catalog at project's epoch |
| `use alias "url"` | External | `use parser "github.com/someone/parser"` | Fetched from URL, pinned in `promise.mod` |

**Why catalog modules don't use URLs:**

1. **URLs encode hosting, not identity.** If `github.com/promise-lang/json` moves to another host, catalog imports (`use json`) are unaffected.
2. **Version in the URL is redundant.** The epoch determines the version. A URL with a version creates two sources of truth.
3. **`use json` is clearer than `use json "github.com/promise-lang/json/1"`.** The URL adds no information the catalog doesn't already provide.
4. **AI agents can enumerate catalog names.** A flat namespace (`json`, `http`, `crypto`) is predictable and discoverable. URLs are not.

**Why external modules DO use URLs:**

1. **The compiler can't guess where they live.** Without a catalog entry, the URL is the only source of truth.
2. **Explicit alias forces readability.** `use parser "github.com/someone/promise-parser"` makes it immediately clear this is an external dependency — the string literal is a visual signal.
3. **Self-contained.** Each source file declares exactly where its external dependencies come from. No external configuration needed to understand the import.

**Alias rules:** All import aliases (whether from catalog, external, or local imports) must be unique within a file. If a catalog module name collides with a needed alias, use `as` on one of them to resolve the conflict.

**Grammar change:** The current grammar rule `useDecl: USE IDENT stringLiteral SEMI` supports only the `use alias "url"` form. This must be extended to also support bare `use IDENT SEMI` for catalog imports:

```antlr
useDecl
    : USE IDENT stringLiteral SEMI        // external: use parser "url";
    | USE IDENT (AS IDENT)? SEMI          // catalog:  use json; / use json as j;
    | USE stringLiteral (AS IDENT)? SEMI  // local:    use "./models"; / use "./models" as m;
    ;
```

---

## 6. Module Structure

### 6.1 `promise.mod` File

Every module (including your project) has a `promise.mod` file at its root:

```
epoch 2026.3
module myapp
```

Two lines. That's a complete project manifest. The `epoch` line pins the catalog version. The `module` line names the module (used when other modules import it).

For a module that will be submitted to the catalog:

```
epoch 2026.3
module json
```

The module name must be a valid Promise identifier. It must be unique within the catalog.

### 6.2 External Module Pinning

Catalog modules need no version declaration — the epoch handles it. But external modules (URL-based imports) must be pinned in `promise.mod`:

```
epoch 2026.3
module myapp

require "github.com/someone/promise-parser" abc1234
require "git.corp.com/team/internal-utils" def5678
```

Each `require` line specifies a URL and a commit hash. This is the **only** version information in the entire project — and it only applies to non-catalog modules.

**Why commit hashes, not semver?** Consistency with the catalog model. The catalog pins exact commits internally; external modules follow the same principle. A commit hash is unambiguous and immutable. Semver tags can be moved or deleted.

**`promise pin` command** updates the commit hash for an external module:

```bash
promise pin "github.com/someone/promise-parser"           # pin to latest commit
promise pin "github.com/someone/promise-parser" abc1234   # pin to specific commit
```

### 6.3 No Lockfile (for Catalog Modules)

There is no `promise.lock` for catalog modules. The epoch **is** the lock. Two developers on the same epoch will always get identical catalog module source code, because the catalog pins exact commits per epoch.

External modules are pinned by commit hash directly in `promise.mod` — this serves the same purpose as a lockfile but lives in the manifest, not a separate generated file.

This eliminates an entire class of problems:
- No lockfile merge conflicts
- No "lockfile out of date" warnings
- No `promise update` that silently changes behavior
- No divergence between lockfile and manifest

### 6.4 Directory Layout

Unchanged from the existing design. Flat layout, no required `src/`:

```
myapp/
  promise.mod           # epoch 2026.3 / module myapp
  main.pr               # entry point
  helpers.pr            # source file
  models/
    user.pr             # organizational subdirectory (part of myapp)
  tests/
    test_main.pr        # test files
```

Sub-directories with their own `promise.mod` are separate modules, excluded from the parent:

```
myapp/
  promise.mod           # module myapp
  main.pr
  libs/
    auth/
      promise.mod       # module auth (separate, imported via use "./libs/auth")
      auth.pr
```

### 6.5 Visibility

Declarations are **module-private by default**. Annotate with `` `public `` to export:

```promise
type User `public {
    string name `public;
    int internal_id;        // private — not visible outside this module

    greet() string `public {
        return "Hi, I'm {name}"
    }

    validate() bool {       // private method
        return internal_id > 0
    }
}

create_user(string name) User `public {
    return User(name: name, internal_id: next_id())
}

next_id() int {             // private function — not visible outside this module
    // ...
}
```

**Why private by default (changed from original design):** Explicit exports make a module's API surface immediately obvious. An AI agent reading a module only needs to look at `` `public `` declarations to understand the API. This also means adding a new internal helper never accidentally becomes part of the public API.

This fulfills the original design's plan: "In a future revision, declarations will be **private by default**, and `` `public `` will be required to export them." The module system is that future revision.

---

## 7. `promise sync` — Toolchain Management

### 7.1 Syncing

The `promise sync` command updates the compiler and catalog to a specific epoch:

```bash
promise sync                 # sync to latest stable epoch
promise sync 2026.3          # sync to a specific epoch
promise sync next            # sync to the upcoming epoch (for testing)
```

What `promise sync` does:

1. Downloads the catalog manifest for the target epoch
2. Updates the `promise` compiler binary to the epoch's version
3. Caches the standard library source for the epoch
4. Does NOT download all catalog modules — they are fetched lazily on first `use`

### 7.2 Toolchain Directory

The current `promise install` creates `~/.promise/bin/` and `~/.promise/lib/std/`. The epoch-based layout extends this:

```
~/.promise/
  bin/promise               # shim that dispatches to the correct epoch's compiler
  active                    # file containing current default epoch name
  epochs/
    2026.2/
      bin/promise           # compiler binary for this epoch
      lib/std/              # standard library source (embedded in binary, extracted here)
      catalog.toml          # catalog manifest
    2026.3/
      ...
  cache/
    modules/
      json/
        a1b2c3d/            # cached module source at specific commit
      http/
        e4f5a6b/
```

Multiple epochs can coexist. The compiler binary in `~/.promise/epochs/<epoch>/bin/promise` is used when building a project pinned to that epoch. The shim at `~/.promise/bin/promise` reads the project's `promise.mod` and dispatches to the correct epoch's compiler.

### 7.3 Project Epoch Resolution

When you run `promise build` or `promise run`, the compiler:

1. Walks up from the current directory to find `promise.mod`
2. Reads the `epoch` line
3. If the current compiler binary matches that epoch, proceeds directly
4. If not, delegates to `~/.promise/epochs/<epoch>/bin/promise`

This means you can work on multiple projects targeting different epochs without manual switching. Each project uses the compiler version that matches its epoch.

### 7.4 First-Run Experience

```bash
# Install Promise for the first time
curl -sSf https://promise-lang.dev/install.sh | sh

# This installs a bootstrap binary that immediately syncs:
promise sync

# Create a new project
mkdir myapp && cd myapp
promise init
# Creates promise.mod with the current stable epoch

# Write code and build — catalog modules are fetched on first use
cat > main.pr << 'EOF'
use io

main() {
    io.println("hello")
}
EOF
promise run main.pr
```

---

## 8. Catalog Governance & Submission

### 8.1 Inclusion Criteria

To be accepted into the catalog, a module must meet:

1. **Tests.** The module must have a test suite that passes. Coverage expectations scale with module scope — a small utility needs basic tests, a crypto library needs comprehensive tests.
2. **No external modules.** A catalog module may only depend on other catalog modules. This ensures the entire dependency graph is within the tested, mono-versioned set. If a catalog module needs functionality from outside the catalog, that functionality must be submitted to the catalog first.
3. **API documentation.** Every `` `public `` declaration must have a doc comment. These are used to generate the catalog's API reference, which AI agents use for code generation.
4. **Naming.** The module name must be descriptive, not conflict with existing modules, and follow naming conventions (lowercase, underscores for multi-word).
5. **Scope.** The module should do one thing well. "Kitchen sink" modules that bundle unrelated functionality are split into separate modules.
6. **License.** Must use a catalog-compatible open-source license.

### 8.2 Submission Process

```
1. Developer creates a Promise module in their own repository
2. Developer submits a catalog inclusion request (PR to the catalog repo)
3. Catalog CI:
   a. Clones the module at the proposed commit
   b. Builds it with the current `next` epoch compiler
   c. Runs the module's own test suite
   d. Runs all existing catalog tests (to detect regressions)
   e. Runs integration tests if the module interacts with existing modules
4. Review by catalog maintainers (API design, naming, scope)
5. If approved, module is added to the next epoch
```

### 8.3 Updates

Module authors submit updates the same way — a PR to the catalog repo bumping the commit hash. The same CI pipeline runs. If any existing module's tests break, the update is blocked until either:

- The update is fixed to maintain compatibility, OR
- All affected downstream modules are updated in the same PR (coordinated breaking change)

### 8.4 Governance Model

The catalog is maintained by the Promise language team and community maintainers. Think of it as a curated distribution, not an open registry:

- **Core modules** (std, io, math, etc.) are maintained by the language team
- **Community modules** are maintained by their authors but reviewed by catalog maintainers
- **Orphaned modules** (author inactive) can be adopted by the community or removed from the catalog

This is deliberately more restrictive than npm/crates.io. The tradeoff is clear: fewer modules, but every module works with every other module, always. For an AI agent, a smaller but reliable catalog is far more useful than a vast but unreliable registry.

---

## 9. External Modules (Non-Catalog)

### 9.1 The Two-Tier Ecosystem

The Promise ecosystem has two tiers:

| Tier | Import syntax | Versioning | Compatibility guarantee | Use case |
|------|--------------|------------|------------------------|----------|
| **Catalog** | `use json` | Mono-versioned (epoch) | All modules tested together | Standard library, common community modules |
| **External** | `use parser "url"` | Per-module (commit pin) | Author's responsibility | Private libs, experimental code, niche tools, pre-catalog prototypes |

Both tiers are first-class. External modules are not second-class citizens — they have full access to the type system, generics, ownership, and everything else. The difference is purely in how they're versioned and what guarantees they carry.

### 9.2 Developing an External Module

An external module is a git repository with a `promise.mod` at the root:

```
promise-parser/
  promise.mod           # epoch 2026.3 / module parser
  parser.pr
  lexer.pr
  tests/
    test_parser.pr
```

Its `promise.mod`:

```
epoch 2026.3
module parser
```

The `epoch` line declares which catalog epoch this module is built against. This means:
- It can `use` any catalog module from that epoch
- Its users should ideally be on the same epoch (or a compatible one)

**Publishing:** Push to any git host. That's it. There's no registry to publish to (unless aiming for catalog inclusion). Anyone can use it by adding `use parser "github.com/you/promise-parser"` and pinning in their `promise.mod`.

### 9.3 Using External Modules

```promise
// main.pr
use parser "github.com/someone/promise-parser"
use internal "git.corp.com/team/internal-utils"

main() {
    ast := parser.parse("1 + 2")
    internal.log(ast)
}
```

```
// promise.mod
epoch 2026.3
module myapp

require "github.com/someone/promise-parser" a1b2c3d
require "git.corp.com/team/internal-utils" e4f5a6b
```

The compiler:
1. Sees `use parser "github.com/someone/promise-parser"` in source
2. Looks up the URL in `promise.mod` to find the pinned commit `a1b2c3d`
3. Fetches (or uses cached) source at that commit
4. Compiles the external module and links it

### 9.4 External Module Dependency Rules

External modules can depend on:
- **Catalog modules** — via `use json` (resolved at the external module's declared epoch)
- **Other external modules** — via `use other "url"` with their own `require` pins
- **Local modules** — via `use "./subdir"`

**Epoch compatibility:** When an external module declares `epoch 2026.3` and your project uses `epoch 2026.3`, catalog dependencies align perfectly. If the epochs differ, the compiler emits a warning:

```
warning: external module 'parser' targets epoch 2026.2, project targets 2026.3
  catalog APIs may differ — consider asking the author to update
```

The build still proceeds — epoch mismatches are warnings, not errors, because catalog modules aim for backward compatibility between adjacent epochs. But the warning makes the risk explicit.

### 9.5 External Module Resolution

When multiple external modules are involved, each module's `require` pins are **its own**. There is no transitive resolution:

```
myapp depends on:
  parser "github.com/someone/parser" @ a1b2c3d
    which depends on:
      tokenizer "github.com/someone/tokenizer" @ fff111

  analyzer "github.com/other/analyzer" @ b2c3d4e
    which depends on:
      tokenizer "github.com/someone/tokenizer" @ aaa222
```

If `parser` and `analyzer` both depend on `tokenizer` at the same commit, they share one copy. If they pin **different commits** (as above: `fff111` vs `aaa222`), the compiler rejects the build:

```
error: conflicting versions of external module "github.com/someone/tokenizer"
  parser pins:   fff111
  analyzer pins: aaa222  (via github.com/other/analyzer @ b2c3d4e)
  → resolve by coordinating with module authors or pinning both to the same commit
```

**No diamond dependency resolution.** This is deliberate. The catalog tier handles version unification; the external tier requires explicit coordination. This pushes widely-used external modules toward catalog inclusion, where compatibility is guaranteed. If you can't change the pins in upstream libraries, a `replace` directive can override a transitive dependency to force a specific commit (see Section 9.7).

### 9.6 Path to Catalog Inclusion

The external module mechanism is the on-ramp to the catalog:

1. **Prototype** as an external module in your own repo
2. **Stabilize** the API, add comprehensive tests
3. **Submit** to the catalog (PR to the catalog repo)
4. **Migrate** users: `use parser "github.com/someone/parser"` → `use parser`

When a module joins the catalog, users update one line of code per file (drop the URL) and remove the `require` line from `promise.mod`. The module name stays the same, so all qualified references (`parser.parse(...)`) are unchanged.

### 9.7 Local Development Overrides

When developing an external module alongside a project that uses it, you don't want to push + pin on every change. Use a `replace` directive in `promise.mod`:

```
epoch 2026.3
module myapp

require "github.com/someone/promise-parser" a1b2c3d

replace "github.com/someone/promise-parser" => "../promise-parser"
```

The `replace` directive redirects the URL to a local directory during development. It is **not** committed to source control (or if committed, the CI should reject it). This is the same pattern as Go's `replace` directive — a development convenience that doesn't affect the published module.

`replace` also works for **catalog modules**, enabling local development against a patched version:

```
epoch 2026.3
module myapp

replace json => "../my-json-fork"
```

When a `replace` targets a catalog module, the compiler emits a prominent warning:

```
warning: catalog module 'json' replaced with local path "../my-json-fork"
  catalog compatibility guarantees do not apply to replaced modules
```

This is the escape hatch for Section 14.4 (bleeding-edge fixes). It also supports `replace` for transitive external dependencies to resolve diamond conflicts (Section 9.5).

---

## 10. Compiler Integration

### 10.1 Compilation Pipeline Changes

Currently, the compiler processes a single file (or directory of files) as one compilation unit, with `std/*.pr` merged in. The module system adds a layer before this:

```
promise.mod           # read epoch, identify module
    |
    v
source scan           # find all .pr files in module, collect `use` declarations
    |
    v
module resolution     # map `use` names to catalog entries or local paths
    |
    v
dependency fetch      # download/cache any catalog modules not yet local
    |
    v
topological sort      # order modules so dependencies compile before dependents
    |
    v
per-module compile    # existing pipeline (parse -> sema -> ownership -> codegen)
    |
    v
link                  # combine all module .ll files via clang
```

### 10.2 Module Compilation Model

Each module compiles to its own LLVM IR file. The linker (clang) combines them:

```bash
# Conceptual compilation steps (handled internally by `promise build`)
promise compile-module ~/.promise/cache/modules/json/a1b2c3d/ -o /tmp/json.ll
promise compile-module ~/.promise/cache/modules/http/e4f5a6b/ -o /tmp/http.ll
promise compile-module ./                                     -o /tmp/myapp.ll
clang /tmp/myapp.ll /tmp/json.ll /tmp/http.ll -o myapp
```

### 10.3 Separate Compilation Considerations

Currently, all `.pr` files in a compilation unit share a single namespace. With modules, each module has its own namespace. This means:

- **Name mangling** must include the module name. A top-level function `parse` in module `json` becomes `json.parse` in LLVM IR, following the existing `Owner.method` mangling convention. Types become `json.JsonObject`, etc.
- **Generic monomorphization** crosses module boundaries. If your code uses `Vector[MyType]` from the standard library, the monomorphized version is emitted in YOUR module's IR, not the standard library's. This is already how it works (codegen generates specialized instances at use sites).
- **RTTI** type IDs must be globally unique across modules. The existing string-based type IDs (`"Dog"`, `"Cat"`) need module prefixing (`"myapp.Dog"`, `"zoo.Cat"`).

### 10.4 Incremental Compilation (Future)

Because modules are separate compilation units, the compiler can cache compiled IR:

```
~/.promise/cache/compiled/
  json/a1b2c3d/json.ll          # compiled IR, reused across all projects on this epoch
  http/e4f5a6b/http.ll
```

Catalog modules at a given epoch are immutable, so their compiled output can be cached indefinitely. Only your project code needs recompilation on changes.

**Limitation:** Generic monomorphization crosses module boundaries — `Vector[MyType]` from the standard library is specialized in the user's module, not the library's. This means cached library IR contains only non-generic code. Monomorphized instances for user types are always emitted in the user's compilation unit, which is rebuilt on every change anyway. This matches the current architecture.

---

## 11. Discovery & Tooling

### 11.1 `promise catalog` Commands

```bash
promise catalog list                    # list all modules in current epoch
promise catalog info json               # show json module's API surface
promise catalog search "parse"          # search module descriptions and exports
promise catalog diff 2026.2 2026.3      # show what changed between epochs
```

### 11.2 AI Agent Integration

The catalog is designed to be a **complete context** that an AI agent can consume:

```bash
# Generate a machine-readable API summary for all catalog modules
promise catalog export --format json > catalog-api.json
```

This produces a structured document containing every module's public types, functions, and their signatures. An AI agent can load this as context and generate correct `use` declarations and API calls without guessing.

The catalog is intentionally small enough that the full API summary fits in a single context window. This is a design constraint — if the catalog grows too large for an AI to reason about, it's too large.

### 11.3 `promise doc`

```bash
promise doc json                # view json module documentation
promise doc json.parse          # view specific function docs
promise doc --serve             # local doc server
```

Documentation is generated from `` `public `` declarations and doc comments in source. No separate doc format — the source is the documentation.

---

## 12. Migration from the Original Design

### 12.1 What Changes

| Aspect | Original Design | This Proposal |
|--------|----------------|---------------|
| Module identity | URL with version in path | Catalog name OR URL (two tiers) |
| `promise.mod` | `module github.com/acme/app/1` | `epoch 2026.3` / `module myapp` / optional `require` pins |
| Import syntax | `use io "github.com/std/io/1"` | `use io` (catalog) or `use alias "url"` (external) |
| Dependency declaration | Inferred from `use` URLs | Catalog: implicit from epoch. External: `require` in `promise.mod` |
| Version resolution | Diamond deps, per-module versions | Catalog: none (mono-versioned). External: no diamonds allowed |
| Lockfile | `promise.lock` | None — epoch + commit pins in `promise.mod` |
| Package manager | `promise add/remove/update` | `promise sync` + `promise pin` |
| External deps | Fetched from URLs | Fetched from URLs, pinned by commit hash |
| Visibility | Public by default | Private by default, `` `public `` to export |

### 12.2 What Stays the Same

- Module boundary semantics (`promise.mod` marks a module root)
- Flat directory layout (no required `src/`)
- Testing conventions (`` `test `` annotation, `_test.pr` files)
- No sub-modules — every `promise.mod` is independent
- Catalog dependencies inferred from `use` in source (no dependency list for catalog modules)
- URL-based syntax retained for external (non-catalog) modules

---

## 13. Comparison with Other Systems

| Property | npm | Go | Cargo | Elm | Nix | **Promise** |
|----------|-----|-----|-------|-----|-----|-------------|
| Versioning | Per-package semver | Per-module semver | Per-crate semver | Enforced semver | Global nixpkgs | **Global epoch** |
| Multiple versions | Yes (nested) | Yes (major suffix) | Yes (features) | No | No | **No** |
| Version conflicts | Common | Rare | Rare | None | None | **Impossible** |
| Lockfile | `package-lock.json` | `go.sum` | `Cargo.lock` | `elm.json` | `flake.lock` | **None** |
| Resolution algorithm | Complex tree | MVS | SAT solver | Elm solver | Nix eval | **Lookup table** |
| Tested together | No | No | No | Partially | Hydra CI | **Always** |
| AI-friendly | Poor | Medium | Medium | Good | Poor | **Best** |
| Ecosystem size | Huge | Large | Large | Small | Large | **Curated** |

The closest analog is **NixOS** — a mono-versioned global package set with CI testing. Promise takes the same idea but applies it at the language level with much simpler tooling (no Nix expression language, no derivations, no flakes).

---

## 14. Risks & Mitigations

### 14.1 "What if the catalog is too small?"

**Risk:** Users need modules that aren't in the catalog.
**Mitigation:** External modules (URL-based imports) provide a full-featured escape hatch with their own dependency management. The catalog doesn't need to be exhaustive — it needs to cover common needs. External modules can graduate to the catalog as they mature.

### 14.2 "What if catalog modules conflict?"

**Risk:** Two catalog modules want incompatible APIs from a third module.
**Mitigation:** This is exactly what the mono-versioned model prevents. Conflicts are resolved **before** the epoch is tagged, by the catalog maintainers. The CI enforces it — if tests don't pass, the epoch doesn't ship.

### 14.3 "What if a module author disappears?"

**Risk:** A catalog module's author stops maintaining it.
**Mitigation:** Because module source is pinned by commit hash, an orphaned module doesn't "break" — it just stops getting updates. The catalog team can adopt, fork, or eventually deprecate it.

### 14.4 "What if I need a bleeding-edge fix?"

**Risk:** A catalog module has a bug fix in its repo that hasn't made it into an epoch yet.
**Mitigation:** Temporarily import it as an external module pinned to the fixed commit. When the next epoch includes the fix, switch back to catalog import. The `replace` directive in `promise.mod` can also redirect a catalog module to a local checkout during development.

### 14.5 "Won't coordinated breaking changes slow everything down?"

**Risk:** A module can't make breaking changes because it requires updating all dependents.
**Mitigation:** Yes — but this is a feature, not a bug. Breaking changes should be expensive. They force API designers to think carefully. And when they do happen, they happen atomically — no ecosystem-wide breakage cascade.

### 14.6 "What about private/proprietary modules?"

**Risk:** Companies want private module registries.
**Mitigation:** Private modules are external modules hosted on private git servers. `use auth "git.corp.com/team/auth-lib"` works with any git host that the developer has access to. No private catalog is needed — the URL-based import mechanism handles this cleanly. For fully offline environments, local path imports (`use "./libs/auth"`) work without any network access.

---

## 15. Implementation Plan

### Phase 1: Module Boundaries & Local Imports

- Parse `promise.mod` (epoch + module name)
- Implement `use <name>` and `use alias "url"` syntax in the parser/AST
- Resolve `use` names against standard library modules (already embedded)
- Qualified name resolution in sema (`io.println` → look up `println` in `io` module's scope)
- `` `public `` meta annotation for visibility enforcement at module boundaries
- Local module imports (`use "./subdir"`)
- Compile multi-module projects (project + local modules as separate compilation units, linked together)

### Phase 2: External Modules

- Parse `require` and `replace` directives in `promise.mod`
- Git-based module fetching (clone at pinned commit)
- Module caching in `~/.promise/cache/`
- `promise pin` command for updating commit hashes
- Diamond dependency detection and rejection for external modules
- Epoch mismatch warnings

### Phase 3: Catalog Infrastructure

- Create the catalog repository with `catalog.toml` format
- Implement `promise sync` (download compiler + catalog manifest)
- Implement lazy catalog module fetching (download on first `use`)
- `promise catalog list/info/search` commands
- Epoch-based compiler dispatch (project epoch → matching compiler binary)

### Phase 4: Catalog CI & Governance

- CI pipeline: build all modules, run all tests, run integration tests
- Epoch tagging workflow
- Module submission PR template and review process
- Migration guide generation for epoch transitions

### Phase 5: Tooling & Polish

- `promise catalog diff` between epochs
- `promise catalog export` for AI agent context
- `promise doc` documentation generation
- Incremental compilation with module-level caching
- `replace` directive for local development overrides

---

## 16. Open Questions

1. **Epoch cadence.** How often should epochs be released? Monthly? Quarterly? On-demand when enough changes accumulate?

2. **Standard library boundary.** Which modules are "standard" (embedded in the compiler binary) vs "catalog" (fetched on demand)? Should the standard library be as minimal as possible (just primitives + io) or comprehensive?

3. **Module granularity.** Should `crypto` be one module or split into `crypto/hash`, `crypto/aes`, `crypto/tls`? The flat catalog namespace suggests coarser granularity with submodule-like organization within a single module.

4. **Cross-epoch compatibility.** If project A (epoch 2026.2) uses an external module built for epoch 2026.3, should this work? Or should external modules target the same epoch as the project?

5. **Catalog size constraint.** Is there a formal limit on catalog size (e.g., "the full API summary must fit in 200K tokens")? This would be a unique and powerful constraint for AI-first design.

6. **Editor/LSP integration.** How does the language server resolve imports? It needs access to the catalog manifest and cached module source.

7. **Offline mode.** Can you develop without network access if modules are cached? (Yes — once synced and modules are fetched, everything is local.)

8. **Catalog override scope.** Section 9.7 allows `replace` for catalog modules with a warning. Should there be additional guardrails — e.g., only allow catalog replacements in development mode, reject them in `promise build --release`?

9. **External module transitivity.** If external module A depends on external module B, should your project need to declare B in its own `require` list, or should A's pins be trusted transitively? Requiring explicit declaration is safer but verbose; trusting transitively is convenient but less visible.

10. **External module epoch range.** Should an external module declare a range of compatible epochs (e.g., `epoch 2026.2..2026.4`) or just a single one? A range would reduce "epoch mismatch" warnings but add complexity.
