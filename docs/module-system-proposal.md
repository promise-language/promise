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
| **Explicit over implicit** | The epoch is explicit in `promise.toml`. Everything else follows deterministically from it. No implicit resolution, no heuristics. |
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

The catalog manifest (`catalog.toml`) lives in the **compiler repository** alongside the compiler source and standard library. It is embedded into the compiler binary via `go:embed`, just like `std/*.pr`. This means a compiler binary IS an epoch — it contains the compiler, the standard library, and the catalog manifest that pins every community module. Tagged compiler commits are **epochs** — stable release points.

```
promise_lang/compiler/
  resources/
    catalog.toml            # module registry: names → source repos + commits
  std/                      # standard library (embedded in binary)
  internal/                 # compiler source
  cmd/promise/              # CLI entry point
  tests/                    # cross-module integration tests
    integration/
      io_json_test.pr
      http_crypto_test.pr
```

The standard library source lives in `std/` and is embedded in the compiler binary. Community modules live in their own repositories and are referenced by URL + commit hash in `catalog.toml`. There is no separate catalog repository — the compiler repo is the single source of truth for what constitutes an epoch.

### 3.2 `catalog.toml`

The catalog manifest maps module names to their source locations and pinned commits:

```toml
[catalog]
epoch = "2026.3"

[modules.json]
url = "https://github.com/promise-lang/json"
commit = "a1b2c3d"
description = "JSON parsing and serialization"

[modules.http]
url = "https://github.com/promise-lang/http"
commit = "e4f5a6b"
description = "HTTP client and server"

[modules.crypto]
url = "git@github.com:promise-lang/crypto.git"
commit = "7c8d9e0"
description = "Cryptographic primitives"
```

Key properties:
- **Flat namespace.** Module names are simple identifiers (`json`, `http`, `crypto`). No URLs, no paths, no version numbers in names.
- **Fetch-ready URLs.** The `url` field stores the full git-fetchable URL including protocol and authentication info (e.g., `https://github.com/...`, `git@github.com:...`, `ssh://git@git.corp.com/...`). This is the URL passed directly to `git clone` — not the normalized canonical form used for identity/deduplication (which strips schemes and suffixes). The catalog entry is the source of truth for *how* to fetch each module.
- **Pinned commits.** Each module points to an exact commit hash. No ranges, no "latest", no resolution.
- **Implicit dependencies.** Catalog modules declare dependencies via `use` declarations in their source code, not in catalog.toml. The compiler resolves them transitively at build time. The catalog CI validates that all inter-module dependencies form a DAG (no cycles).
- **Self-contained.** Catalog modules may only depend on other catalog modules — never on remote or local modules. The catalog is a closed world: every dependency in the graph is tested, versioned, and shipped together. This is enforced at build time (catalog modules with `[require]` entries are rejected) and by the catalog CI pipeline (see Section 8.1).

### 3.3 What's NOT in the Catalog

The catalog is curated, not exhaustive. Not every Promise module needs to be in it. Modules outside the catalog are **sourced modules** — either local (path-based) or remote (URL-based) — imported via `use alias "location"` declarations (see Section 9).

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

### 5.1 `use` Declarations (Module Imports)

The `use` keyword at file scope imports a module by its catalog name:

**Note on keyword reuse:** Promise also uses `use` inside function bodies for scoped resource bindings (`use x = File.open("path")`). There is no ambiguity — module imports (`useDecl`) appear at file scope before declarations, while resource bindings (`useBinding`) appear inside statement blocks. The grammar separates them structurally.

```promise
use io
use json
use http
```

That's it. No URLs, no version numbers, no string literals. The module name is a bare identifier that maps to a catalog entry. The epoch (declared in `promise.toml`) determines which version of each module you get.

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

### 5.4 Sourced Module Imports (Local and Remote)

For modules **not in the catalog** — project-local modules, private libraries, experimental packages, pre-catalog prototypes — use a sourced import with an explicit alias and a location string:

```promise
// Local modules (relative path)
use models "./libs/models"
use auth "../shared/auth"

// Remote modules (URL — git repository)
use parser "github.com/someone/promise-parser"
use internal "git.corp.com/team/internal-utils"

main() {
    user := models.User(name: "Alice")
    parser.parse("input")
    internal.do_thing()
}
```

Both local and remote sourced imports use the same syntax: `use alias "location"`. The alias is mandatory and is the only way to reference the module's exports. The compiler disambiguates local vs remote based on a simple prefix rule:

**A location string is local if it starts with a path prefix. Everything else is remote.** The detection rule:

| Prefix | Kind | Example |
|--------|------|---------|
| `./`, `../` | Local (relative) | `"./libs/models"`, `"../shared/auth"` |
| `/` | Local (absolute, Unix) | `"/opt/shared/auth"` |
| `C:\`, `d:/`, etc. | Local (absolute, Windows) | `"C:\projects\shared\auth"` |
| Everything else | Remote (git URL) | `"github.com/someone/parser"`, `"https://git.corp.com/team/utils"` |

More precisely: a location string is local if it starts with `./`, `../`, `/`, or a drive letter followed by `:` (matching `[A-Za-z]:`). Everything else is remote. This is unambiguous — `github.com/...` doesn't match any local prefix. A bare name like `"models"` would be classified as remote, fail to resolve as a git URL, and produce a clear error:

```
error: invalid module path "models" — not a valid git URL
  hint: did you mean `use models;` (catalog) or `use models "./models";` (local)?
```

**Relative paths (`./`, `../`) are the norm.** Absolute paths (`/...`, `C:\...`) work but are non-portable across machines — they should be treated as development-only conveniences (like `[replace]`) and not committed to source control. The compiler emits a warning for absolute local imports:

```
warning: absolute local import path "/opt/shared/auth" is non-portable
  hint: use a relative path or import as a remote module for reproducible builds
```

**Local modules** point to a directory containing its own `promise.toml`. The path is always relative to the project's module root (the directory containing the project's `promise.toml`), not the importing source file. This means every source file in the project uses the same path to reference the same local module, regardless of which subdirectory the source file lives in. Subdirectories without a `promise.toml` are organizational — their `.pr` files belong to the parent module.

**Remote modules** point to a git repository containing a `promise.toml` at its root. They must be pinned to a specific git commit hash in the project's `promise.toml` `[require]` section (see Section 6.2). The compiler clones (or fetches from cache) the repository at the pinned commit. If a source file references a remote URL that has no `[require]` entry, the compiler errors:

```
error: remote module "github.com/someone/parser" has no pin in promise.toml
  hint: run `promise pin "github.com/someone/parser"` to add one
```

### 5.5 Standard Library — Just Part of the Catalog

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

### 5.6 Import Summary

The import syntax has two grammar forms, covering three semantic tiers:

| Form | Tier | Example | Resolution |
|------|------|---------|------------|
| `use name` | Catalog | `use json` | Looked up in catalog at project's epoch |
| `use alias "location"` | Local | `use models "./libs/models"` | Directory relative to `promise.toml` |
| `use alias "location"` | Remote | `use parser "github.com/someone/parser"` | Git repo, pinned by commit hash in `promise.toml` |

Local and remote imports share the same grammar rule — the compiler disambiguates based on the location string prefix (starts with `./`, `../`, `/`, or a drive letter like `C:` → local; everything else → remote).

**Why catalog modules don't use location strings:**

1. **URLs encode hosting, not identity.** If `github.com/promise-lang/json` moves to another host, catalog imports (`use json`) are unaffected.
2. **Version in the URL is redundant.** The epoch determines the version. A URL with a version creates two sources of truth.
3. **`use json` is clearer than `use json "github.com/promise-lang/json/1"`.** The URL adds no information the catalog doesn't already provide.
4. **AI agents can enumerate catalog names.** A flat namespace (`json`, `http`, `crypto`) is predictable and discoverable. URLs are not.

**Why sourced modules DO use location strings:**

1. **The compiler can't guess where they live.** Without a catalog entry, the location string is the only source of truth.
2. **Explicit alias forces readability.** `use parser "github.com/someone/promise-parser"` makes it immediately clear this is a sourced dependency — the string literal is a visual signal.
3. **Self-contained.** Each source file declares exactly where its sourced dependencies come from. No external configuration needed to understand the import.

**Alias rules:** All import aliases (whether from catalog or sourced imports) must be unique within a file. If a catalog module name collides with a needed alias, use `as` on one of them to resolve the conflict.

**Grammar:** The grammar has two import forms — bare identifier for catalog, identifier with string literal for sourced:

```antlr
useDecl
    : USE IDENT (AS IDENT)? SEMI          // catalog:  use json; / use json as j;
    | USE IDENT stringLiteral SEMI         // sourced:  use parser "github.com/...";
    ;                                      //           use models "./libs/models";
```

The existing grammar rule `USE IDENT stringLiteral SEMI` already covers sourced imports. The only addition is the bare `USE IDENT (AS IDENT)? SEMI` form for catalog imports. The two alternatives disambiguate cleanly on the third token: `stringLiteral` → sourced, `AS`/`SEMI` → catalog.

---

## 6. Module Structure

### 6.1 `promise.toml` File

Every module (including your project) has a `promise.toml` file at its root. The file uses standard [TOML](https://toml.io) format, so editors and IDEs provide syntax highlighting, validation, and completion out of the box.

```toml
[module]
name = "myapp"
epoch = "2026.3"
```

That's a complete project manifest. The `epoch` key pins the catalog version. The `name` key names the module (used as the default alias when other modules import it).

For a module that will be submitted to the catalog:

```toml
[module]
name = "json"
epoch = "2026.3"
```

The module name must be a valid Promise identifier. It must be unique within the catalog.

**Forward compatibility:** The compiler ignores unknown keys and sections in `promise.toml`. This allows the format to evolve across epochs without breaking older compilers reading newer manifests. New keys are always optional with sensible defaults.

**Catalog self-containment enforcement:** The catalog self-containment rule (catalog modules may not depend on sourced modules — see Section 3.2 and 8.1) is enforced by the **catalog CI pipeline**, not by the compiler. A module's `promise.toml` does not declare whether it is a catalog module — that status is determined by its presence in `catalog.toml`. The compiler treats all modules identically; the catalog CI adds the extra validation layer (rejecting `[require]` sections, sourced `use` imports, etc.).

### 6.2 Remote Module Pinning

Catalog modules need no version declaration — the epoch handles it. Local modules use whatever is on disk. But remote modules must be pinned to a git commit hash in `promise.toml`:

```toml
[module]
name = "myapp"
epoch = "2026.3"

[require]
"github.com/someone/promise-parser" = "a1b2c3d4e5f6"
"git.corp.com/team/internal-utils" = "def5678abc1234"
```

Each entry in `[require]` maps a remote URL to a git commit hash. This is the **only** version information in the entire project — and it only applies to remote modules.

**What lives at the remote URL:** Always a git repository containing a `promise.toml` at its root. The compiler clones the repo (or fetches into its cache) at the pinned commit. No other formats (zip, tar, registry) are supported — git is universal and commit hashes map directly to git objects.

**Why commit hashes, not semver?** Consistency with the catalog model. The catalog pins exact commits internally; remote modules follow the same principle. A commit hash is unambiguous and immutable. Semver tags can be moved or deleted. Remote module authors can use whatever versioning scheme they like (tags, semver, etc.) — Promise resolves everything to a commit hash at pin time.

**`promise pin` command** resolves human-friendly references to commit hashes:

```bash
promise pin "github.com/someone/promise-parser"              # pin to latest commit on default branch
promise pin "github.com/someone/promise-parser" v2.1.0        # resolve tag → commit hash, pin that
promise pin "github.com/someone/promise-parser" a1b2c3d       # pin to exact commit
```

### 6.3 No Lockfile (for Catalog Modules)

There is no `promise.lock` for catalog modules. The epoch **is** the lock. Two developers on the same epoch will always get identical catalog module source code, because the catalog pins exact commits per epoch.

Remote modules are pinned by commit hash directly in `promise.toml` — this serves the same purpose as a lockfile but lives in the manifest, not a separate generated file.

This eliminates an entire class of problems:
- No lockfile merge conflicts
- No "lockfile out of date" warnings
- No `promise update` that silently changes behavior
- No divergence between lockfile and manifest

### 6.4 Directory Layout

Unchanged from the existing design. Flat layout, no required `src/`:

```
myapp/
  promise.toml          # [module] name = "myapp", epoch = "2026.3"
  main.pr               # entry point
  helpers.pr            # source file
  models/
    user.pr             # organizational subdirectory (part of myapp)
  tests/
    test_main.pr        # test files
```

Sub-directories with their own `promise.toml` are separate modules, excluded from the parent:

```
myapp/
  promise.toml          # module myapp
  main.pr
  libs/
    auth/
      promise.toml      # module auth (separate, imported via use auth "./libs/auth")
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

**Test files can access private declarations.** Test files (matching `test_*.pr` or `*_test.pr` conventions) within a module are part of that module's compilation unit. They can access all declarations — public and private — because they're inside the module boundary. Only code in OTHER modules is restricted to `` `public `` declarations. This is the same approach Go uses (`_test.go` files are in the same package).

**Import alias collisions.** All import aliases must be unique within a file. If a catalog module name collides with a sourced import alias, the compiler reports an error:

```
error: duplicate import alias 'json'
  use json;                              // catalog
  use json "./libs/json";               // local — conflicts with catalog alias
  hint: use `as` to rename one: `use json as std_json;` or `use local_json "./libs/json";`
```

Both catalog and sourced imports can be aliased: `use json as j` for catalog, or simply choose a different alias for the sourced import.

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

1. Downloads the `promise` compiler binary for the target epoch (~61MB self-contained binary)
2. The binary embeds: compiler, standard library (`std/*.pr`), catalog manifest (`catalog.toml`), LLVM tools (compressed)
3. Does NOT download all catalog modules — they are fetched lazily from their git URLs on first `use`

Since the catalog manifest is embedded in the compiler binary, there is no separate catalog download step. One binary = one epoch.

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
    modules/                # all git-fetched modules (catalog + remote), keyed by normalized URL
      github.com/promise-lang/json/
        repo.git/           # bare clone
        a1b2c3d4e5f6/       # source checkout (first 12 chars of commit)
      github.com/someone/promise-parser/
        repo.git/
        f1e2d3c4a5b6/
    build/                  # content-addressed build cache (shared across all projects)
      a3/                   # two-level directory (first 2 hex chars of cache key)
        a3b4c5d8...o        # compiled object file
        a3b4c5d8...interface # public API hash
```

Multiple epochs can coexist. The compiler binary in `~/.promise/epochs/<epoch>/bin/promise` is used when building a project pinned to that epoch. The shim at `~/.promise/bin/promise` reads the project's `promise.toml` and dispatches to the correct epoch's compiler.

### 7.3 Project Epoch Resolution

When you run `promise build` or `promise run`, the compiler:

1. Walks up from the current directory to find `promise.toml`
2. Reads the `epoch` line
3. If the current compiler binary matches that epoch, proceeds directly
4. If not, delegates to `~/.promise/epochs/<epoch>/bin/promise`

This means you can work on multiple projects targeting different epochs without manual switching. Each project uses the compiler version that matches its epoch.

### 7.4 First-Run Experience

```bash
# Install Promise for the first time — downloads a single self-contained binary
curl -sSf https://promise-lang.dev/install.sh | sh

# The binary includes everything: compiler, std, catalog, LLVM tools
# No `promise sync` needed on first install

# Create a new project
mkdir myapp && cd myapp
promise init
# Creates promise.toml with the current stable epoch

# Write code and build — catalog modules are fetched on first use
cat > main.pr << 'EOF'
use io

main() {
    io.println("hello")
}
EOF
promise run main.pr
```

**Single-file mode (no `promise.toml` required):** For quick scripts and one-off programs, `promise run file.pr` and `promise exec '<code>'` work without a `promise.toml`. If the compiler walks up the directory tree and finds no `promise.toml`, it treats the file as an anonymous single-file module using the compiler's default epoch (the `active` epoch from `~/.promise/active`). Only catalog imports are available in this mode — sourced imports require a `promise.toml` (for `[require]` pins and path resolution root). This preserves the "write one file and run it" experience that is critical for AI agent workflows.

---

## 8. Catalog Governance & Submission

### 8.1 Inclusion Criteria

To be accepted into the catalog, a module must meet:

1. **Tests.** The module must have a test suite that passes. Coverage expectations scale with module scope — a small utility needs basic tests, a crypto library needs comprehensive tests.
2. **Catalog-only dependencies (self-containment).** A catalog module may only depend on other catalog modules — no remote modules, no local modules, no `[require]` section in its `promise.toml`. This ensures the catalog is a **closed, fully self-contained world**: every dependency in the graph is tested, versioned, and shipped as a unit. If a catalog module needs functionality from outside the catalog, that functionality must be submitted to the catalog first. This is enforced by the catalog CI pipeline, which rejects any module with sourced imports or a `[require]` section (see Section 6.1).
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

## 9. Remote Modules (Non-Catalog)

### 9.1 The Module Ecosystem

The Promise ecosystem has three tiers, covered by two grammar forms:

| Tier | Import syntax | Versioning | Compatibility guarantee | Use case |
|------|--------------|------------|------------------------|----------|
| **Catalog** | `use json` | Mono-versioned (epoch) | All modules tested together | Standard library, common community modules |
| **Local** | `use models "./libs/models"` | Whatever is on disk | Developer's responsibility | Project sub-modules, monorepo packages |
| **Remote** | `use parser "github.com/..."` | Git commit pin | Author's responsibility | Private libs, experimental code, niche tools, pre-catalog prototypes |

All tiers are first-class. Sourced modules (local and remote) have full access to the type system, generics, ownership, and everything else. The difference is purely in how they're versioned and what guarantees they carry.

### 9.2 Developing a Remote Module

A remote module is a git repository with a `promise.toml` at the root:

```
promise-parser/
  promise.toml          # [module] name = "parser", epoch = "2026.3"
  parser.pr
  lexer.pr
  tests/
    test_parser.pr
```

Its `promise.toml`:

```toml
[module]
name = "parser"
epoch = "2026.3"
```

The `epoch` key declares which catalog epoch this module is built against. This means:
- It can `use` any catalog module from that epoch
- Its users should ideally be on the same epoch (or a compatible one)

**Publishing:** Push to any git host. That's it. There's no registry to publish to (unless aiming for catalog inclusion). Anyone can use it by adding `use parser "github.com/you/promise-parser"` and pinning in their `promise.toml`.

### 9.3 Using Remote Modules

```promise
// main.pr
use parser "github.com/someone/promise-parser"
use internal "git.corp.com/team/internal-utils"

main() {
    ast := parser.parse("1 + 2")
    internal.log(ast)
}
```

```toml
# promise.toml
[module]
name = "myapp"
epoch = "2026.3"

[require]
"github.com/someone/promise-parser" = "a1b2c3d"
"git.corp.com/team/internal-utils" = "e4f5a6b"
```

The compiler:
1. Sees `use parser "github.com/someone/promise-parser"` in source
2. Looks up the URL in `promise.toml` `[require]` to find the pinned commit `a1b2c3d`
3. Clones (or uses cached) the git repo at that commit
4. Reads the remote module's `promise.toml` to confirm the module name
5. Compiles the remote module and links it

### 9.4 Remote Module Dependency Rules

Remote modules can depend on:
- **Catalog modules** — via `use json` (resolved at the remote module's declared epoch)
- **Other remote modules** — via `use other "url"` with their own `[require]` pins
- **Local modules** — via `use lib "./subdir"`

**Epoch compatibility:** When a remote module declares `epoch = "2026.3"` and your project uses `epoch = "2026.3"`, catalog dependencies align perfectly. If the epochs differ, the compiler emits a warning:

```
warning: remote module 'parser' targets epoch 2026.2, project targets 2026.3
  catalog APIs may differ — consider asking the author to update
```

The build still proceeds — epoch mismatches are warnings, not errors, because catalog modules aim for backward compatibility between adjacent epochs. But the warning makes the risk explicit.

### 9.5 Transitive Dependencies

Each module declares only its **direct** dependencies — catalog via `use name`, sourced via `use alias "location"` with `[require]` pins for remote ones. The compiler resolves transitive dependencies automatically by walking the dependency graph.

**Graph construction:**

1. Parse the top-level project's `promise.toml` and source files
2. For each sourced dependency, fetch its source (local path or git repo at pinned commit)
3. Parse that dependency's `promise.toml` and source files to discover ITS dependencies
4. Recurse until the full graph is resolved
5. Catalog dependencies are leaves — they only depend on other catalog modules (self-containment rule)

**The top-level project does NOT need to declare transitive dependencies.** If your project uses `parser`, and `parser` depends on `tokenizer`, you don't need `tokenizer` in your `[require]`. The `parser` module's own `promise.toml` handles that:

```
myapp/promise.toml         → [require] parser = "a1b2c3d"
parser/promise.toml        → [require] tokenizer = "fff111"
tokenizer/promise.toml     → (no further remote deps)
```

The compiler walks this graph and fetches `tokenizer` at `fff111` automatically. Your project never mentions `tokenizer`.

**Conflict detection:** If two paths in the graph reach the same remote URL at **different** commits, the build fails:

```
myapp depends on:
  parser "github.com/someone/parser" @ a1b2c3d
    which depends on:
      tokenizer "github.com/someone/tokenizer" @ fff111

  analyzer "github.com/other/analyzer" @ b2c3d4e
    which depends on:
      tokenizer "github.com/someone/tokenizer" @ aaa222
```

```
error: conflicting versions of remote module "github.com/someone/tokenizer"
  parser pins:   fff111
  analyzer pins: aaa222  (via github.com/other/analyzer @ b2c3d4e)
  → resolve by coordinating with module authors or pinning both to the same commit
  → or use [replace] in your promise.toml to force a specific commit
```

If they pin the **same** commit, they share one copy — compiled once, linked once.

**No diamond resolution.** This is deliberate. The catalog tier handles version unification; the sourced tier requires explicit coordination. This pushes widely-used modules toward catalog inclusion, where compatibility is guaranteed.

**Resolving conflicts with top-level `[require]`:** If two transitive dependencies pin different commits of the same URL, the top-level project can resolve the conflict by adding its own `[require]` entry for that URL. **The top-level project's `[require]` always wins over transitive pins:**

```toml
# myapp/promise.toml — force tokenizer to a specific commit
[require]
"github.com/someone/parser" = "a1b2c3d"
"github.com/other/analyzer" = "b2c3d4e"
"github.com/someone/tokenizer" = "fff111"  # overrides both transitive pins
```

This is the only mechanism for overriding transitive commits. `[replace]` is strictly for redirecting to local paths during development (see Section 9.7) — it never changes commit hashes.

**Local module transitivity:** Local modules can have their own dependencies (catalog, remote, or other local). A local module's `[require]` introduces those remote modules into the graph — the same conflict rules apply. A local module's local paths are relative to ITS own `promise.toml`, not the parent project's. A remote module's local imports (e.g., `use util "./internal/util"` within a remote repo) are resolved within its own repository and are invisible to the consuming project.

**Circular dependency detection:** Circular dependencies between any modules — catalog, local, or remote — are a compile error. The compiler detects cycles during the topological sort of the dependency graph and reports the full cycle path:

```
error: circular dependency detected
  myapp → auth ("./libs/auth") → permissions ("./perms") → myapp
```

**Dependency graph summary by tier:**

| Dependency type | Declared where | Transitive? | Conflict rule |
|----------------|---------------|-------------|---------------|
| Catalog → Catalog | Implicit (epoch) | No conflicts possible (mono-versioned) | N/A |
| Project → Catalog | `use name` | Catalog deps are self-contained | N/A |
| Project → Remote | `use alias "url"` + `[require]` | Resolved automatically from remote's `promise.toml` | Same URL, different commit → error (top-level `[require]` overrides) |
| Project → Local | `use alias "./path"` | Resolved automatically from local's `promise.toml` | Same rules as remote |
| Remote → Catalog | `use name` | Self-contained within catalog | Epoch mismatch → warning |
| Remote → Remote | `use alias "url"` + `[require]` | Walked recursively | Same URL, different commit → error |

### 9.6 Path to Catalog Inclusion

The remote module mechanism is the on-ramp to the catalog:

1. **Prototype** as a remote module in your own repo
2. **Stabilize** the API, add comprehensive tests
3. **Submit** to the catalog (PR to the catalog repo)
4. **Migrate** users: `use parser "github.com/someone/parser"` → `use parser`

When a module joins the catalog, users update one line of code per file (drop the URL string) and remove the `[require]` entry from `promise.toml`. The module name stays the same, so all qualified references (`parser.parse(...)`) are unchanged.

### 9.7 Local Development Overrides

When developing a remote module alongside a project that uses it, you don't want to push + pin on every change. Use a `[replace]` section in `promise.toml`:

```toml
[module]
name = "myapp"
epoch = "2026.3"

[require]
"github.com/someone/promise-parser" = "a1b2c3d"

[replace]
"github.com/someone/promise-parser" = "../promise-parser"
```

The `[replace]` section redirects a module to a local directory during development. **`[replace]` values are always local paths** — it is purely a path-redirection mechanism, never used for changing commit hashes (use `[require]` for commit overrides — see Section 9.5). It is **not** committed to source control (or if committed, the CI should reject it). This is the same pattern as Go's `replace` directive — a development convenience that doesn't affect the published module.

`[replace]` also works for **catalog modules**, enabling local development against a patched version:

```toml
[module]
name = "myapp"
epoch = "2026.3"

[replace]
json = "../my-json-fork"
```

When a `replace` targets a catalog module, the compiler emits a prominent warning:

```
warning: catalog module 'json' replaced with local path "../my-json-fork"
  catalog compatibility guarantees do not apply to replaced modules
```

This is the escape hatch for Section 15.4 (bleeding-edge fixes).

---

## 10. Compiler Integration

### 10.1 Compilation Pipeline

Currently, the compiler processes a single file (or directory of files) as one compilation unit, with `std/*.pr` merged in. The module system adds a layer before this:

```
promise.toml          # read epoch, identify module
    |
    v
source scan           # find all .pr files in module, collect `use` declarations
    |
    v
dependency graph      # walk transitive deps, fetch remote modules, detect conflicts
    |
    v
topological sort      # order modules so dependencies compile before dependents
    |
    v
incremental check     # hash sources + dep interfaces, skip unchanged modules
    |
    v
per-module compile    # existing pipeline (parse → sema → ownership → codegen)
    |                    only for modules whose hash changed
    v
link                  # combine all module object files → binary
```

### 10.2 Module Compilation Model

Each module is a **separate compilation unit** that produces:

1. **Object file** (`.o`) — compiled LLVM IR → machine code via `opt` + `llc`
2. **Module interface** — the public API surface needed by dependent modules during sema:
   - Public type definitions (fields, methods, signatures, inheritance)
   - Public function signatures
   - Generic type/function **templates** (full AST — needed for monomorphization in dependents)
   - RTTI type IDs

The linker (`ld.lld` on Linux, system `ld` on macOS) combines all object files:

```bash
# Conceptual steps (handled internally by `promise build`)
# 1. Compile each module to object file
promise compile-module ~/.promise/cache/modules/json/a1b2c3d/ -o /tmp/json.o
promise compile-module ./libs/models/                          -o /tmp/models.o
promise compile-module ./                                      -o /tmp/myapp.o
# 2. Link
ld.lld /tmp/myapp.o /tmp/json.o /tmp/models.o -o myapp
```

Modules without dependencies on each other can compile **in parallel** (they're at the same level in the topological sort).

### 10.3 Separate Compilation Considerations

Currently, all `.pr` files in a compilation unit share a single namespace. With modules, each module has its own namespace. This means:

- **Name mangling** must include the module name. A top-level function `parse` in module `json` becomes `json.parse` in LLVM IR, following the existing `Owner.method` mangling convention. Types become `json.JsonObject`, etc.
- **Generic monomorphization** crosses module boundaries. If your code uses `Vector[MyType]` from the standard library, the monomorphized version is emitted in YOUR module's IR, not the standard library's. This is already how it works (codegen generates specialized instances at use sites). Monomorphized symbols use `linkonce_odr` linkage so the linker deduplicates identical instantiations across modules.
- **RTTI** type IDs must be globally unique across modules. The existing string-based type IDs (`"Dog"`, `"Cat"`) need module prefixing (`"myapp.Dog"`, `"zoo.Cat"`).

### 10.4 Incremental Compilation

Fast modify-build-test loops are critical for AI agent efficiency. The module system enables fine-grained incremental compilation: **only recompile what changed, skip everything else.**

#### Cache Key

Each module's build is keyed by a content hash:

```
cache_key = hash(
    sorted(hash(source_file) for each .pr file in module),
    sorted(interface_hash(dep) for each direct dependency),
    epoch,
    compiler_version
)
```

If the cache key matches a previous build → skip compilation entirely, reuse cached `.o` file.

#### What Triggers Recompilation

| What changed | What recompiles | Why |
|---|---|---|
| A `.pr` file in user's module | User's module only | Source hash changed |
| A local module's **internal** code (private functions, method bodies) | That local module only | Its source hash changed, but its interface hash didn't → dependents skip |
| A local module's **public API** (new public type, changed signature) | That module + all dependents | Interface hash changed → dependents' cache keys changed |
| A remote module | Nothing | Immutable at pinned commit — cached forever |
| A catalog module | Nothing | Immutable at epoch — cached forever |
| Epoch change | Everything | Compiler + all catalog modules change |

#### Interface Hashing

The key optimization is distinguishing between **internal changes** (no dependent recompilation) and **API changes** (dependents must recompile). The module interface hash covers:

- Public type definitions: names, fields, parent types, method signatures
- Public function signatures: names, parameter types, return types, failable markers
- Public generic templates: full AST (because dependents monomorphize them — a body change produces different code in the dependent)
- Exported constants, enum variants

If a module's source changes but its interface hash stays the same, **no dependent recompiles.** Only the module itself is rebuilt.

#### Cache Layout

```
~/.promise/cache/                        # global cache (shared across all projects)
  build/                                 # content-addressed build cache
    a3/                                  # two-level directory (first 2 hex chars)
      a3b4c5d8...o                       # compiled object file
      a3b4c5d8...interface               # public API hash
    f1/
      f1e2d3c4...o
      f1e2d3c4...interface
  modules/                               # remote modules (keyed by URL/commit)
    github.com/someone/promise-parser/
      repo.git/                          # bare clone
      a1b2c3d4e5f6/                      # source checkout (first 12 chars of commit)
        ...
```

- **All caches are global** (`~/.promise/cache/`). Build cache keys are content-addressed (SHA-256 of impl hash + compiler hash + target + module paths), so the same module compiled with the same inputs produces the same cache key regardless of which project triggered the build.
- **`PROMISE_HOME` env var** overrides the `~/.promise/` base directory for all Promise data (caches, LLVM tools, CRT, installs). Useful when `$HOME` is unavailable or for CI environments.

#### The AI Modify-Build-Test Loop

The typical AI workflow: modify 1-3 files → build → test → check output → repeat. With incremental compilation:

```
Step 1: AI edits main.pr
Step 2: `promise build`
  a. Read promise.toml, build dependency graph              ~1ms
  b. Check dep module hashes → all cached                   ~1ms
  c. Detect changed source in user module                   ~1ms
  d. Recompile user module only (parse→sema→codegen)     ~50-200ms
  e. opt + llc on user module only                        ~50-100ms
  f. Relink all object files                              ~30-50ms
  ─────────────────────────────────────────────────
  Total                                                  ~130-350ms

Step 3: `promise test`
  (binary already built, just execute)
```

Compare to a full rebuild (first build or epoch change):

```
Full build:
  Compile all catalog deps (parallel)                     ~1-5s
  Compile user module                                     ~50-200ms
  Link                                                    ~30-50ms
  ─────────────────────────────────────────────────
  Total                                                   ~2-6s
```

**Key design decisions for fast rebuilds:**

1. **Module = compilation unit.** Changing one file doesn't recompile the whole project — only the module containing that file. For most AI-generated programs (single module), this means one compilation unit.
2. **Dependency compilation is amortized.** Catalog and remote modules compile once and cache forever (per epoch/commit). The cost is paid on first build, not on every iteration.
3. **Interface-aware caching.** A local module's internal refactoring doesn't cascade to dependents. Only public API changes propagate.
4. **Parallel module compilation.** Independent modules compile concurrently. The topological sort identifies parallelizable groups.
5. **Object file linking, not IR linking.** Each module produces a `.o` file. Relinking `.o` files is faster than re-running `opt`+`llc` on combined IR.

#### Generic Monomorphization and Caching

Generic templates (e.g., `Vector[T]`, `Map[K, V]`) cross module boundaries — they're monomorphized in the **dependent's** compilation unit, not the library's. This means:

- Cached library `.o` files contain only non-generic code and library-internal monomorphizations
- When user code uses `Vector[MyType]`, the monomorphized `Vector[MyType]` code is emitted in the user's `.o` file
- The user module's cache key includes the interface hash of dependencies, which includes generic template ASTs
- If a generic template body changes (rare for catalog/remote), dependents using that generic will recompile

This matches the current architecture (codegen generates specialized instances at use sites) and is the same approach used by Rust and C++ with templates.

---

## 11. Discovery & Tooling

### 11.1 `promise catalog` Commands

```bash
promise catalog list                    # list all modules in current epoch
promise catalog diff 2026.2 2026.3      # show what changed between epochs
promise doc json                        # show json module's API surface (see 11.3)
promise doc --search "parse"            # search module descriptions and exports
```

### 11.2 `promise doc`

```bash
promise doc json                # view json module documentation
promise doc json.parse          # view specific function docs
promise doc --search "parse"    # search module descriptions and exports
promise doc --serve             # local doc server
promise doc --export json       # machine-readable API summary (for AI agents)
```

Documentation is generated from `` `public `` declarations and doc comments in source. No separate doc format — the source is the documentation.

### 11.3 AI Agent Integration

The `--export` flag produces a structured document containing every module's public types, functions, and their signatures. An AI agent can load this as context and generate correct `use` declarations and API calls without guessing:

```bash
promise doc --export json > catalog-api.json
```

The catalog is intentionally small enough that the full API summary fits in a single context window. This is a design constraint — if the catalog grows too large for an AI to reason about, it's too large.

---

## 12. User Journeys

This section documents end-to-end workflows for the three primary personas interacting with the Promise module ecosystem.

### 12.1 End User: Download Promise, Build & Run Projects

**Goal:** A developer (human or AI agent) downloads Promise for the first time, creates a project that uses catalog and remote modules, and iterates on it.

#### First install

```bash
# Download and install Promise (self-contained ~61MB binary)
curl -sSf https://promise-lang.dev/install.sh | sh

# The install script places the binary at ~/.promise/bin/promise
# and adds it to PATH. The binary embeds:
#   - the compiler (Go binary)
#   - LLVM tools (opt, llc, lld) compressed, extracted lazily
#   - the standard library (std/*.pr)
#   - the catalog manifest (catalog.toml) for this epoch

# Verify installation
promise version
# promise 2026.3 (epoch 2026.3, linux-amd64)
```

No `promise sync` needed on first install — the binary IS the epoch. The catalog manifest is embedded, the std library is embedded, LLVM tools are embedded. One download, fully offline-capable for catalog modules (fetched lazily on first use).

#### Single-file mode (no project setup)

```bash
# Run a one-liner — no promise.toml needed
promise exec 'println("hello world")'

# Run a single file — uses the compiler's default epoch
echo 'use io; main() { io.println("hello") }' > hello.pr
promise run hello.pr

# Catalog modules work in single-file mode
echo '
use json

main() {
    obj := json.parse("{\"name\": \"Alice\"}")
    println(obj)
}
' > parse.pr
promise run parse.pr
# First run: fetches json module source from catalog URL, compiles, caches
# Subsequent runs: uses cached .o — near-instant
```

Single-file mode uses the compiler's built-in epoch. Only catalog imports are available (no `[require]` for remote modules, no path root for local modules).

#### Project setup

```bash
mkdir myapp && cd myapp
promise init
# Creates promise.toml:
#   [module]
#   name = "myapp"
#   epoch = "2026.3"
```

#### Writing code with catalog modules

```promise
// main.pr
use json
use http

main()! {
    resp := http.get("https://api.example.com/data")!
    data := json.parse(resp.body)!
    println(data["name"])
}
```

```bash
promise build
# 1. Reads promise.toml → epoch 2026.3
# 2. Scans source → needs json, http from catalog
# 3. Looks up json, http in embedded catalog.toml → gets URLs + commits
# 4. Fetches json repo at pinned commit (or uses cached checkout)
# 5. Fetches http repo at pinned commit (http requires json, crypto — fetches those too)
# 6. Compiles all modules (parallel where possible), caches .o files globally
# 7. Links → produces ./myapp binary

./myapp
```

#### Adding a remote module

```bash
# Find a community parser library (not in catalog)
# Add it to source code:
```

```promise
// main.pr
use json
use parser "github.com/someone/promise-parser"

main() {
    ast := parser.parse("1 + 2")
    println(json.serialize(ast))
}
```

```bash
# Pin it — resolves latest commit on default branch
promise pin "github.com/someone/promise-parser"
# Added to promise.toml:
#   [require]
#   "github.com/someone/promise-parser" = "a1b2c3d4e5f67890..."

promise build    # fetches, compiles, links
promise test     # run tests
```

#### The modify-build-test loop

```bash
# Edit main.pr...
promise build    # ~200ms — only recompiles myapp module, deps cached
promise test     # runs immediately — binary already built

# Edit a local module...
promise build    # ~300ms — recompiles changed module + myapp if API changed
                 # deps whose interface didn't change → dependents skip
```

#### Cleaning caches

```bash
promise clean              # remove global build cache (~/.promise/cache/build/)
promise clean --global     # remove all global caches (build + module source)
```

### 12.2 Community Module Developer: Create & Publish a Module

**Goal:** A developer creates a reusable Promise module, publishes it for others to use as a remote module, and eventually submits it to the catalog.

#### Create the module

```bash
mkdir promise-csv && cd promise-csv
promise init
# Edit promise.toml:
#   [module]
#   name = "csv"
#   epoch = "2026.3"
```

```promise
// csv.pr
use io

type Row `public {
    string[] fields `public;

    get(int index) string `public {
        return fields[index]
    }
}

parse(string input) Row[] `public {
    // ...parse CSV...
}

write(Row[] rows) string `public {
    // ...serialize to CSV...
}
```

```promise
// tests/test_csv.pr
use csv   // test files within the module access it by name

test parse_simple() {
    rows := csv.parse("a,b,c\n1,2,3")
    assert(rows.length == 2)
    assert(rows[1].get(0) == "1")
}

test roundtrip() {
    input := "name,age\nAlice,30\nBob,25"
    rows := csv.parse(input)
    output := csv.write(rows)
    assert(output == input)
}
```

```bash
# Test locally
promise test tests/

# The module can use catalog modules (json, io, etc.)
# but cannot have [require] entries if it aims for catalog inclusion
```

#### Publish as a remote module

```bash
# Push to any git host
git init && git add -A && git commit -m "Initial csv module"
git remote add origin git@github.com:yourname/promise-csv.git
git push -u origin main
```

Others can now use it:

```promise
// In someone else's project:
use csv "github.com/yourname/promise-csv"

main() {
    rows := csv.parse("a,b\n1,2")
    println(rows[0].fields)
}
```

```bash
# They pin it in their promise.toml:
promise pin "github.com/yourname/promise-csv"
```

#### Iterate on the module

```bash
# Make changes, push, tag a release
git tag v1.0.0
git push --tags

# Users update by re-pinning:
promise pin "github.com/yourname/promise-csv" v1.0.0
# Resolves tag → commit hash, updates promise.toml
```

#### Submit to the catalog

When the module is stable, well-tested, and generally useful:

1. **Ensure catalog compatibility:**
   - No `[require]` section (catalog modules can only depend on other catalog modules)
   - No sourced imports (`use alias "url"`) — only `use name` catalog imports
   - All public declarations have doc comments
   - Comprehensive test suite

2. **Submit a PR to the catalog repo:**

```bash
# The PR adds an entry to catalog.toml:
# [modules.csv]
# url = "https://github.com/yourname/promise-csv"
# commit = "a1b2c3d4e5f6..."
# description = "CSV parsing and serialization"
```

3. **Catalog CI runs automatically:**
   - Clones the module at the proposed commit
   - Builds with the `next` epoch compiler
   - Runs the module's test suite
   - Runs all existing catalog tests (regression check)
   - Validates no sourced imports, no `[require]`, doc comments present

4. **After review and merge:**
   - Module is available in the next epoch
   - Users migrate: `use csv "github.com/yourname/promise-csv"` → `use csv`
   - They remove the `[require]` entry from `promise.toml`
   - All qualified references (`csv.parse(...)`) are unchanged

### 12.3 Language Developer: Iterate on Promise or Publish an Epoch

**Goal:** A Promise language developer works on the compiler, standard library, or catalog modules, tests changes, and cuts a new epoch release.

#### Day-to-day development

```bash
cd promise_lang/compiler

# The catalog manifest lives in the repo alongside the compiler and std lib:
#   compiler/resources/catalog.toml
# It is embedded into the compiler binary via go:embed, just like std/*.pr

# Edit the compiler, std library, or catalog manifest
vim internal/codegen/stmt.go     # compiler change
vim std/vector.pr                # std library change
vim resources/catalog.toml       # update a catalog module pin

# Build the compiler
make                             # regenerate parser, embed resources, build binary

# Run compiler tests
make test                        # go test ./...

# Run Promise-level tests
./promise test tests/            # e2e tests using the built compiler
```

#### Testing a catalog module update

When a community module author submits an update (new commit) or a new module:

```bash
# 1. Update the catalog manifest
vim resources/catalog.toml
# Change: [modules.json]
#         commit = "new_commit_hash"

# 2. Rebuild compiler (embeds new catalog.toml)
make

# 3. Run the module's own tests
./promise test --module json

# 4. Run cross-module integration tests
./promise test tests/integration/

# 5. Run the full test suite to check for regressions
bin/verify.sh
```

#### Cutting a new epoch

An epoch release is a tagged commit of the compiler repo. The catalog manifest embedded in that binary IS the epoch — there is no separate catalog repository to coordinate with.

```bash
# 1. Ensure all catalog modules build and pass tests at their pinned commits
./promise catalog verify        # builds every catalog module, runs all tests

# 2. Update epoch identifier
vim resources/catalog.toml
# [catalog]
# epoch = "2026.4"

# 3. Update any std library or compiler changes
# ...

# 4. Build release binary
make release                    # embeds LLVM tools → ~61MB self-contained binary

# 5. Run full verification
bin/verify.sh                   # compiler tests + promise tests + e2e

# 6. Tag and release
git tag epoch-2026.4
git push --tags
# CI builds release binaries for linux-amd64, darwin-amd64, darwin-arm64
# Publishes to https://promise-lang.dev/download/
```

#### The epoch lifecycle

```
                    ┌─────────────────────────────────────┐
                    │         Compiler Repository          │
                    │                                      │
                    │  compiler/                            │
                    │    resources/                         │
                    │      catalog.toml  ← THE catalog     │
                    │    std/            ← std library      │
                    │    internal/       ← compiler code    │
                    │    cmd/promise/    ← CLI entry        │
                    │                                      │
                    └──────────────┬──────────────────────┘
                                   │
                          git tag epoch-2026.4
                                   │
                    ┌──────────────▼──────────────────────┐
                    │        Release Binary                 │
                    │                                      │
                    │  Embeds:                              │
                    │    catalog.toml (epoch 2026.4)        │
                    │    std/*.pr                           │
                    │    LLVM tools (gzip)                  │
                    │    musl CRT (linux)                   │
                    │                                      │
                    │  One binary = one epoch               │
                    │  No external catalog repo needed      │
                    └──────────────────────────────────────┘
```

**Key insight:** The catalog file lives in the compiler repo, not in a separate repository. This means:
- A compiler binary always knows its epoch and which modules belong to it
- There is no drift between compiler version and catalog version
- `promise sync` downloads a single binary — that binary IS the epoch
- Language developers iterate on compiler + std + catalog in one commit

#### Adding a breaking change across modules

```bash
# Example: renaming json.parse() → json.decode()

# 1. Update the json module source in its own repo
#    (coordinate with module author or fork)

# 2. Update catalog.toml to pin the new commit
vim resources/catalog.toml
# [modules.json]
# commit = "new_commit_with_decode"

# 3. Update any other catalog modules that call json.parse()
#    Pin their updated commits too

# 4. Update std library if it references the old API

# 5. Update integration tests

# 6. Build, test everything together
make && ./promise catalog verify

# 7. All changes land in one commit/PR — atomic epoch transition
```

This is the mono-versioned model in action: breaking changes are coordinated across the entire catalog in a single commit, so users never see a half-migrated state.

---

## 13. Migration from the Original Design

### 12.1 What Changes

| Aspect | Original Design | This Proposal |
|--------|----------------|---------------|
| Module identity | URL with version in path | Catalog name OR location string (two grammar forms, three tiers) |
| Manifest | `promise.mod` (custom format) | `promise.toml` (standard TOML) |
| Import syntax | `use io "github.com/std/io/1"` | `use io` (catalog) or `use alias "location"` (sourced) |
| Dependency declaration | Inferred from `use` URLs | Catalog: implicit from epoch. Remote: `[require]` in `promise.toml`. Local: on disk |
| Version resolution | Diamond deps, per-module versions | Catalog: none (mono-versioned). Remote: no diamonds allowed |
| Lockfile | `promise.lock` | None — epoch + commit pins in `promise.toml` |
| Package manager | `promise add/remove/update` | `promise sync` + `promise pin` |
| Remote deps | Fetched from URLs | Fetched from git repos, pinned by commit hash |
| Visibility | Public by default | Private by default, `` `public `` to export |

### 12.2 What Stays the Same

- Module boundary semantics (`promise.toml` marks a module root)
- Flat directory layout (no required `src/`)
- Testing conventions (`` `test `` annotation, `_test.pr` files)
- No sub-modules — every `promise.toml` is independent
- Catalog dependencies inferred from `use` in source (no dependency list for catalog modules)
- Location-string syntax retained for sourced (non-catalog) modules

---

## 14. Comparison with Other Systems

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

## 15. Risks & Mitigations

### 15.1 "What if the catalog is too small?"

**Risk:** Users need modules that aren't in the catalog.
**Mitigation:** Sourced modules (local and remote imports) provide a full-featured escape hatch with their own dependency management. The catalog doesn't need to be exhaustive — it needs to cover common needs. Remote modules can graduate to the catalog as they mature.

### 15.2 "What if catalog modules conflict?"

**Risk:** Two catalog modules want incompatible APIs from a third module.
**Mitigation:** This is exactly what the mono-versioned model prevents. Conflicts are resolved **before** the epoch is tagged, by the catalog maintainers. The CI enforces it — if tests don't pass, the epoch doesn't ship.

### 15.3 "What if a module author disappears?"

**Risk:** A catalog module's author stops maintaining it.
**Mitigation:** Because module source is pinned by commit hash, an orphaned module doesn't "break" — it just stops getting updates. The catalog team can adopt, fork, or eventually deprecate it.

### 15.4 "What if I need a bleeding-edge fix?"

**Risk:** A catalog module has a bug fix in its repo that hasn't made it into an epoch yet.
**Mitigation:** Temporarily import it as a remote module pinned to the fixed commit. When the next epoch includes the fix, switch back to catalog import. The `[replace]` section in `promise.toml` can also redirect a catalog module to a local checkout during development.

### 15.5 "Won't coordinated breaking changes slow everything down?"

**Risk:** A module can't make breaking changes because it requires updating all dependents.
**Mitigation:** Yes — but this is a feature, not a bug. Breaking changes should be expensive. They force API designers to think carefully. And when they do happen, they happen atomically — no ecosystem-wide breakage cascade.

### 15.6 "What about private/proprietary modules?"

**Risk:** Companies want private module registries.
**Mitigation:** Private modules are remote modules hosted on private git servers. `use auth "git.corp.com/team/auth-lib"` works with any git host that the developer has access to. No private catalog is needed — the sourced import mechanism handles this cleanly. For fully offline environments, local imports (`use auth "./libs/auth"`) work without any network access.

---

## 16. Implementation Plan

### Phase 1: Module Boundaries & Local Imports

- Parse `promise.toml` (TOML format: `[module]` with `name` and `epoch`)
- Extend grammar: add bare `USE IDENT (AS IDENT)? SEMI` for catalog imports alongside existing `USE IDENT stringLiteral SEMI` for sourced imports
- Resolve `use` names against standard library modules (already embedded)
- Qualified name resolution in sema (`io.println` → look up `println` in `io` module's scope)
- `` `public `` meta annotation for visibility enforcement at module boundaries
- Local module imports (`use models "./libs/models"` — path relative to `promise.toml`)
- Compile multi-module projects (project + local modules as separate compilation units, linked together)
- **Incremental compilation from day one:** content-hash-based caching per module, interface hashing to skip dependent recompilation when only internals change (see Section 10.4)

### Phase 2: Remote Modules

- Parse `[require]` and `[replace]` sections in `promise.toml`
- Git-based module fetching (clone at pinned commit)
- Module caching in `~/.promise/cache/`
- `promise pin` command for resolving tags/branches to commit hashes
- Diamond dependency detection and rejection for remote modules
- Epoch mismatch warnings

### Phase 3: Catalog Infrastructure

- Create the catalog repository with `catalog.toml` format
- Implement `promise sync` (download compiler + catalog manifest)
- Implement lazy catalog module fetching (download on first `use`)
- `promise catalog list` command
- `promise doc` for module API browsing and search
- Epoch-based compiler dispatch (project epoch → matching compiler binary)

### Phase 4: Catalog CI & Governance

- CI pipeline: build all modules, run all tests, run integration tests
- Epoch tagging workflow
- Module submission PR template and review process
- Migration guide generation for epoch transitions

### Phase 5: Tooling & Polish

- `promise catalog diff` between epochs
- `promise doc --export` for AI agent context
- `[replace]` section for local development overrides
- Parallel module compilation (independent modules in the topological sort compile concurrently)

---

## 17. Open Questions

1. **Epoch cadence.** How often should epochs be released? Monthly? Quarterly? On-demand when enough changes accumulate?

2. **Standard library boundary.** Which modules are "standard" (embedded in the compiler binary) vs "catalog" (fetched on demand)? Should the standard library be as minimal as possible (just primitives + io) or comprehensive?

3. **Module granularity.** Should `crypto` be one module or split into `crypto/hash`, `crypto/aes`, `crypto/tls`? The flat catalog namespace suggests coarser granularity with submodule-like organization within a single module.

4. **Cross-epoch compatibility.** If project A (epoch 2026.2) uses a remote module built for epoch 2026.3, should this work? Or should remote modules target the same epoch as the project?

5. **Catalog size constraint.** Is there a formal limit on catalog size (e.g., "the full API summary must fit in 200K tokens")? This would be a unique and powerful constraint for AI-first design.

6. **Editor/LSP integration.** How does the language server resolve imports? It needs access to the catalog manifest and cached module source.

7. **Offline mode.** Can you develop without network access if modules are cached? (Yes — once synced and modules are fetched, everything is local.)

8. **Catalog override scope.** Section 9.7 allows `replace` for catalog modules with a warning. Should there be additional guardrails — e.g., only allow catalog replacements in development mode, reject them in `promise build --release`?

9. ~~**Remote module transitivity.**~~ **Resolved:** Transitive dependencies are resolved automatically by walking each module's `promise.toml`. The top-level project only declares direct deps. Conflicts (same URL, different commits) are rejected. See Section 9.5.

10. **Remote module epoch range.** Should a remote module declare a range of compatible epochs (e.g., `epoch = "2026.2..2026.4"`) or just a single one? A range would reduce "epoch mismatch" warnings but add complexity.
