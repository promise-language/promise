# WASM Runtime Binding Architecture

> **Status: Design proposal.** No implementation yet. This document describes how Promise will ingest IDL definitions (WIT, WebIDL) and generate safe, ergonomic bindings to WASM host environments.

## Problem

Promise compiles to `wasm32-wasi` but only imports two WASI functions (`fd_write`, `proc_exit`). Everything else — file I/O, environment, networking, process execution — is stubbed. There is no mechanism for:

1. Importing additional host functions beyond what the PAL hardcodes
2. Binding to browser APIs when running WASM in the browser
3. Ingesting interface definitions (WIT, WebIDL) to generate type-safe bindings
4. Distinguishing between WASI and browser WASM targets at compile time

## Architecture Overview

```
                    IDL Sources
                   /           \
             .wit files      .webidl files
                |                |
                v                v
        +--------------------------+
        |     promise bind         |  compiler subcommand
        |  - WIT parser            |
        |  - WebIDL parser         |
        |  - Promise code generator|
        +-----------+--------------+
                    |
                    v
           Generated Catalog Modules
           (external, not embedded)
           wasi/   →  use wasi;
           web/    →  use web;
                    |
                    v
        +---------------------------+
        |  Normal Promise Compiler  |
        |  parse → sema → codegen   |
        +---------------------------+
                    |
          +---------+---------+
          |                   |
          v                   v
    wasm32-wasi           wasm32-web
    (wasmtime/wasmer)     (browser)
          |                   |
          v                   v
    program.wasm      program.wasm + program.js
```

### Core Principles

- **Generated code, not compile-time magic.** `promise bind` produces readable `.pr` files with `extern` declarations and type definitions. No IDL parsing at compile time, no hidden code generation, no magic imports. An agent can read the generated module to understand exactly what it provides.

- **External catalog modules.** The `wasi` and `web` binding modules are external catalog modules — unlike `json`, `os`, and `std` which are embedded in the compiler binary, binding modules are fetched via the module system and pinned in `catalog.toml`. This is a deliberate choice: keeping bindings external allows the ecosystem to target different WASI versions from the same compiler epoch, support community-maintained bindings for custom IDLs (game engines, cloud runtimes), and vendor-specific browser modules — all without requiring compiler releases.

- **Binding scope is the caller's choice.** What `promise bind` generates depends on the IDL it is given. One can bind the entire WASI specification into a single module, or generate multiple smaller modules each covering a subset of the runtime IDL. `promise bind` may accept options to limit which interfaces are generated, but the simplest default is to bind the full IDL and rely on LTO to eliminate unused symbols — no runtime cost for generating more than needed.

- **Reuse existing mechanisms.** The bindings use `extern` functions, `target(cond)` annotations, and the catalog module system. The only new language-level addition is the `wasm_import` meta annotation.

---

## WASM Sub-Targets

### Current State

One WASM target: `wasm32-wasi`. The `TargetInfo` struct has `OS` and `Arch` fields:

```go
// compiler/internal/sema/target.go
type TargetInfo struct {
    OS   string // "linux", "macos", "windows", "wasm"
    Arch string // "x86_64", "aarch64", "wasm32"
}
```

### Proposed: Add `Env` Field

```go
type TargetInfo struct {
    OS   string // "linux", "macos", "windows", "wasm"
    Arch string // "x86_64", "aarch64", "wasm32"
    Env  string // "wasi", "web", "" (empty for native targets)
}
```

Target triple parsing:

| Triple | OS | Arch | Env |
|--------|-----|------|-----|
| `wasm32-wasi` | `wasm` | `wasm32` | `wasi` |
| `wasm32-web` | `wasm` | `wasm32` | `web` |
| `x86_64-unknown-linux-musl` | `linux` | `x86_64` | (empty) |

### New Target Condition Identifiers

Added to `matchTargetIdent`:

| Identifier | Matches when |
|------------|-------------|
| `wasi` | `Env == "wasi"` |
| `web` | `Env == "web"` |
| `wasm` | `OS == "wasm"` (either sub-target, unchanged) |
| `posix` | `OS == "linux" \|\| OS == "macos"` (unchanged) |

Usage in binding modules:

```promise
// Available on both WASI and browser
_wasm_memory_size() int `extern("memory_size") `target(wasm);

// WASI-specific
_wasi_fd_read(int fd, int iovs, int iovs_len, int nread) int
    `extern("fd_read") `wasm_import("wasi_snapshot_preview1", "fd_read")
    `target(wasi);

// Browser-specific
_web_console_log(string msg)
    `extern("promise_web_console_log") `wasm_import("promise_env", "console_log")
    `target(web);
```

### CLI Selection

```bash
promise build --target wasm32-wasi file.pr    # WASI (default WASM)
promise build --target wasm32-web file.pr     # Browser
```

### PAL Differentiation

`ForTarget` in `pal/pal.go` returns different implementations:

```go
func ForTarget(triple string) PAL {
    switch {
    case strings.Contains(triple, "wasm") && strings.Contains(triple, "web"):
        return &WasmWebPAL{}
    case strings.Contains(triple, "wasm"):
        return &WasmPAL{}     // existing WASI PAL
    case strings.Contains(triple, "windows"):
        return &WindowsPAL{}
    default:
        return &PosixPAL{target: triple}
    }
}
```

`WasmWebPAL` imports from `"promise_env"` (the auto-generated JS glue module) instead of `"wasi_snapshot_preview1"`. For example, `EmitWrite` would import a `promise_env.write` function that the JS glue implements by writing to the browser console or a custom output target.

### Linker Differences

| Flag | `wasm32-wasi` | `wasm32-web` |
|------|--------------|-------------|
| `--export=_start` | Yes | No |
| `--export=_initialize` | No | Yes |
| `--export-memory` | No | Yes (JS needs memory access) |
| `--allow-undefined` | Yes (WASI runtime resolves) | Yes (JS glue resolves) |
| `--lto-O2` | Yes | Yes |
| Link `wasm_alloc.o` | Yes | Yes (allocator works in any WASM) |

Post-link, the `wasm32-web` target emits a companion `.js` file (see [Browser JS Glue](#browser-js-glue) below).

---

## The `wasm_import` Annotation

### Motivation

Currently, WASM imports are hardcoded in `pal/wasm.go` using `ir.AttrPair`:

```go
fdWrite.FuncAttrs = append(fdWrite.FuncAttrs,
    ir.AttrPair{Key: "wasm-import-module", Value: "wasi_snapshot_preview1"},
    ir.AttrPair{Key: "wasm-import-name", Value: "fd_write"})
```

This works for the few PAL functions but doesn't scale to user-facing binding modules written in Promise. We need a way for Promise `extern` declarations to specify WASM import metadata.

### Design

A new meta annotation on extern functions:

```promise
_wasi_fd_write(int fd, int iovs_ptr, int iovs_len, int nwritten_ptr) int
    `extern("fd_write")
    `wasm_import("wasi_snapshot_preview1", "fd_write");
```

**Grammar**: No changes. `wasm_import` is parsed as a regular meta annotation by the existing `metaAnnotation` rule. Parameters are positional string literals.

**Sema validation**:
- `wasm_import` requires exactly two string parameters (module name, import name)
- `wasm_import` is only valid on `extern` functions
- Warning if used without `target(wasm)` (will be ignored on non-WASM targets)

**Codegen**: In `declareExterns` (extern.go), when `c.isWasm` is true:

```go
if wasmMod, wasmName := wasmImportAttrs(fd); wasmMod != "" {
    fn.FuncAttrs = append(fn.FuncAttrs,
        ir.AttrPair{Key: "wasm-import-module", Value: wasmMod},
        ir.AttrPair{Key: "wasm-import-name", Value: wasmName})
}
```

**Non-WASM targets**: The annotation is silently ignored. This allows binding modules to be compiled for any target (with `target(wasi)` filtering out the declarations entirely on non-WASM builds).

---

## IDL-to-Promise Generation Pipeline

### `promise bind` Subcommand

```bash
# Generate Promise bindings from WIT definitions
promise bind wit path/to/wasi.wit -o modules/wasi/

# Generate Promise bindings from WebIDL definitions
promise bind webidl path/to/dom.webidl -o modules/web/
```

The output is a complete catalog module directory:

```
modules/wasi/
    promise.toml          # [module] name = "wasi", epoch = "2026.3"
    wasi.pr               # all types + extern declarations + public API
    wasi_test.pr           # tests for the bindings
```

### Pipeline Architecture

```
.wit / .webidl
    │
    ▼
┌─────────────┐
│  IDL Parser  │   WIT parser or WebIDL parser
│  (per-format) │   → format-specific AST
└──────┬──────┘
       │
       ▼
┌─────────────┐
│  Binding IR  │   Shared intermediate representation
│  (universal) │   types, functions, imports
└──────┬──────┘
       │
       ▼
┌──────────────┐
│  Promise Gen  │   Emits .pr source files
│  (shared)     │   types, extern decls, wrappers
└──────┬───────┘
       │
       ▼
  .pr files + promise.toml
```

The binding IR (`compiler/internal/bindgen/ir.go`) is a shared intermediate representation that abstracts over WIT and WebIDL differences:

```go
// Binding IR types
type BindingModule struct {
    Name       string
    Types      []BindingType
    Functions  []BindingFunc
    Resources  []BindingResource
    ImportModule string  // WASM import module name
}

type BindingType struct {
    Name    string
    Kind    TypeKind  // Record, Enum, Variant, Flags, Alias
    Fields  []BindingField
}

type BindingFunc struct {
    Name       string
    Params     []BindingParam
    Results    []BindingResult
    ImportName string  // WASM import name
    Kind       FuncKind  // Free, Method, Constructor, Static
    OwnerType  string    // for methods/constructors
}

type BindingResource struct {
    Name    string
    Methods []BindingFunc
    Drop    bool  // has destructor
}
```

### WIT Parser

Hand-written recursive descent parser (`compiler/internal/wit/`). WIT is a relatively simple grammar:

```wit
// Example WIT definition
interface filesystem {
    enum descriptor-type {
        unknown, block-device, character-device, directory,
        file, symbolic-link, socket
    }

    record descriptor-stat {
        type: descriptor-type,
        size: u64,
    }

    resource descriptor {
        stat: func() -> result<descriptor-stat, error-code>
        read: func(length: u64) -> result<list<u8>, error-code>
        write: func(data: list<u8>) -> result<u64, error-code>
    }

    open-at: func(path: string, flags: open-flags) -> result<descriptor, error-code>
}
```

### WebIDL Parser

Hand-written recursive descent parser (`compiler/internal/webidl/`). More complex than WIT due to inheritance, mixins, overloading, but well-specified:

```webidl
// Example WebIDL definition
interface Element : Node {
    readonly attribute DOMString tagName;
    DOMString? getAttribute(DOMString name);
    undefined setAttribute(DOMString name, DOMString value);
    Element? querySelector(DOMString selectors);
};

interface Document : Node {
    Element? getElementById(DOMString elementId);
    Element createElement(DOMString localName);
};
```

### Promise Code Generator

The shared code generator (`compiler/internal/bindgen/codegen.go`) traverses the binding IR and emits `.pr` source files. It handles:

- Type mapping (see tables below)
- Extern function declarations with `wasm_import` annotations
- Public wrapper methods that provide ergonomic APIs
- Resource types with `drop(~this)` destructors
- `doc` annotations on all public declarations

---

## Type Mapping

### WIT → Promise

| WIT Type | Promise Type | Notes |
|----------|-------------|-------|
| `u8`, `u16`, `u32`, `u64` | `u8`, `u16`, `u32`, `u64` | Direct |
| `s8`, `s16`, `s32`, `s64` | `i8`, `i16`, `i32`, `i64` | Direct |
| `f32`, `f64` | `f32`, `f64` | Direct |
| `bool` | `bool` | Direct |
| `char` | `char` | Unicode scalar value |
| `string` | `string` | Canonical ABI: `(i32 ptr, i32 len)` in linear memory |
| `list<T>` | `T[]` | Canonical ABI: `(i32 ptr, i32 len)` |
| `option<T>` | `T?` | Promise optional |
| `result<T, E>` | failable `foo!() T` | Error type → Promise error |
| `result<_, E>` | `foo!()` | Void failable |
| `tuple<T1, T2>` | `(T1, T2)` | Promise tuple |
| `record { ... }` | `type { ... }` | Value type with `` `value `` fields |
| `variant { ... }` | `enum { ... }` | Promise enum with variant data |
| `enum { ... }` | `enum { ... }` | Fieldless Promise enum |
| `flags { ... }` | `type { ... }` | Value type with named flag constants, bitwise methods |
| `resource` | `type { int _handle; drop(~this) { ... } }` | Ownership type |
| `own<R>` | `~R` (parameter) / `R` (return) | Unique ownership |
| `borrow<R>` | `&R` | Shared reference |

### WebIDL → Promise

| WebIDL Type | Promise Type | Notes |
|-------------|-------------|-------|
| `boolean` | `bool` | Direct |
| `byte` / `octet` | `i8` / `u8` | |
| `short` / `unsigned short` | `i16` / `u16` | |
| `long` / `unsigned long` | `i32` / `u32` | |
| `long long` / `unsigned long long` | `i64` / `u64` | |
| `float` / `double` | `f32` / `f64` | |
| `DOMString` / `USVString` | `string` | |
| `ByteString` | `u8[]` | |
| `sequence<T>` | `T[]` | |
| `Promise<T>` | `task[T]` | Maps to Promise concurrency |
| `T?` (nullable) | `T?` | Promise optional |
| `void` / `undefined` | (void) | |
| `interface` | `type { int _js_ref; ... }` | Opaque JS handle |
| `dictionary` | `type { ... }` | Value type |
| `enum` | `enum { ... }` | With string mapping for JS |
| `callback` | `(Args) Return` | Lambda type |
| `ArrayBuffer` | `u8[]` | |
| `any` | `JsValue` | Tagged enum (see below) |
| `object` | `JsValue` | Same enum, `Object` variant |

### `JsValue` — Dynamic Value Enum

WebIDL's `any` and `object` types are dynamic, but Promise doesn't need a built-in `any` type to represent them. Following the same pattern as `JsonValue` in the `json` module, the `web` binding module defines a `JsValue` enum — a tagged union that covers all JS value types using standard language primitives:

```promise
enum JsValue `public `doc("Represents a dynamic JavaScript value.") {
    Undefined,
    Null,
    Bool(bool value),
    Number(f64 value),
    Str(string value),
    Object(int _js_ref),
    Array(int _js_ref),
    Function(int _js_ref),

    get is_undefined bool `public {
        match this {
            JsValue.Undefined => { return true; },
            _ => { return false; },
        }
    }

    get is_null bool `public {
        match this {
            JsValue.Null => { return true; },
            _ => { return false; },
        }
    }

    as_bool() bool? `public {
        match this {
            JsValue.Bool(v) => { return v; },
            _ => { return none; },
        }
    }

    as_number() f64? `public {
        match this {
            JsValue.Number(v) => { return v; },
            _ => { return none; },
        }
    }

    as_string() string? `public {
        match this {
            JsValue.Str(v) => { return v; },
            _ => { return none; },
        }
    }

    `doc "Access a property by name. Returns Undefined if not found."
    get(this, string key) JsValue `public {
        return _web_js_value_get(this, key);
    }

    `doc "Set a property by name."
    set(this, string key, JsValue value) `public {
        _web_js_value_set(this, key, value);
    }
}
```

Primitive JS values (`boolean`, `number`, `string`, `null`, `undefined`) are extracted and stored directly in the enum variants — no JS reference needed. Complex JS values (`object`, `array`, `function`) are stored as opaque `_js_ref` handles managed by the JS glue's reference table. This avoids runtime type tagging while keeping the representation fully explicit and pattern-matchable.

### Canonical ABI Representation

For WASI, compound types are serialized into WASM linear memory using the canonical ABI:

**Strings**: `(i32 ptr, i32 len)` — UTF-8 bytes in linear memory. The generated binding:
1. Calls `canonical_abi_realloc(0, 0, 1, len)` to allocate space
2. Copies Promise string bytes into linear memory
3. Passes `(ptr, len)` to the WASI import
4. For returns: reads `(ptr, len)`, constructs Promise string, frees allocation

**Lists**: `(i32 ptr, i32 len)` — contiguous array of canonical-ABI-encoded elements.

**Records**: Fields laid out sequentially with natural alignment. Generated code uses helper functions for serialization.

**Variants/Results/Options**: Discriminant `i32` tag followed by the active case's payload, padded to the largest variant.

**Resource handles**: `i32` index into the host's handle table. No serialization — passed directly as WASM i32 values.

---

## WIT Resources → Promise Ownership

WIT resources have lifecycle semantics that map naturally to Promise's ownership model:

| WIT Concept | Promise Mapping |
|-------------|----------------|
| `resource R` | `type R { int _handle; drop(~this) { ... } }` |
| `own<R>` (parameter) | `~R` — consumed, caller loses access |
| `own<R>` (return) | `R` — caller receives ownership |
| `borrow<R>` (parameter) | `&R` — borrowed, not consumed |
| `constructor` | Factory function returning `R` |
| `method` (self: borrow) | Method taking `this` (shared ref) |
| resource drop | `drop(~this)` calls host's `[resource-drop]` import |

Example generated binding for a WIT filesystem resource:

```promise
type Descriptor `public {
    int _handle;

    `doc "Get file metadata."
    stat!(this) DescriptorStat `public {
        return _wasi_descriptor_stat(this._handle)^;
    }

    `doc "Read bytes from the file."
    read!(this, u64 length) u8[] `public {
        return _wasi_descriptor_read(this._handle, length)^;
    }

    `doc "Write bytes to the file."
    write!(this, u8[] data) u64 `public {
        return _wasi_descriptor_write(this._handle, data)^;
    }

    drop(~this) {
        _wasi_descriptor_drop(this._handle);
    }
}

// Low-level WASI imports (private)
_wasi_descriptor_stat(int handle) DescriptorStat!
    `extern("descriptor_stat")
    `wasm_import("wasi:filesystem/types", "[method]descriptor.stat");

_wasi_descriptor_read(int handle, u64 length) u8[]!
    `extern("descriptor_read")
    `wasm_import("wasi:filesystem/types", "[method]descriptor.read");

_wasi_descriptor_write(int handle, u8[] data) u64!
    `extern("descriptor_write")
    `wasm_import("wasi:filesystem/types", "[method]descriptor.write");

_wasi_descriptor_drop(int handle)
    `extern("descriptor_drop")
    `wasm_import("wasi:filesystem/types", "[resource-drop]descriptor");
```

Promise's ownership checker prevents use-after-drop:

```promise
use wasi;

main() {
    fd := open("/tmp/hello.txt")^;
    data := fd.read(1024)^;    // ok: fd is borrowed
    consume(~fd);               // fd moved to consume()
    fd.read(1024);              // ERROR: use of moved variable 'fd'
}
```

---

## Binding Module Structure

### Why External, Not Embedded

Embedded modules (like `std`, `json`, `os`) are baked into the compiler binary and tied to its release cycle. Binding modules are deliberately kept external because:

1. **Version independence.** A project can target WASI Preview 1, WASI Preview 2, or a custom WASI extension — all from the same compiler epoch. Different `[require]` pins in `promise.toml` select different binding versions without waiting for a compiler release.

2. **Custom IDL support.** Game engines, cloud runtimes, and embedded systems define their own host APIs via WIT or WebIDL. Users run `promise bind wit engine.wit -o libs/engine/` to generate project-local bindings. These are first-class catalog modules — no special compiler support needed.

3. **Vendor-specific modules.** Browser vendors may expose non-standard APIs. Community-maintained modules like `web_chrome` or `web_safari` can provide vendor-specific bindings without bloating the standard `web` module.

4. **Faster iteration.** Binding definitions change more frequently than the compiler. External modules can be updated, tested, and released independently.

### `wasi` Module (External Catalog)

The `wasi` module lives in its own git repository and is pinned in `catalog.toml`:

```toml
# catalog.toml (in compiler)
[modules.wasi]
url = "github.com/anthropics/promise-wasi"
commit = "abc123..."
```

Module structure:

```
promise-wasi/
    promise.toml              # [module] name = "wasi", epoch = "2026.3"
    wasi.pr                   # all WASI types + functions
    wasi_test.pr              # tests
```

Single-file design: `wasi.pr` contains all WASI interfaces (filesystem, io, clocks, cli, random, etc.) in one file. This follows the "single module" approach — `use wasi;` imports everything. LTO eliminates unused symbols at link time.

A project targeting a different WASI version can override the catalog pin in its own `promise.toml`:

```toml
# project's promise.toml
[require]
"github.com/anthropics/promise-wasi" = "def789..."  # different commit = different WASI version
```

Or use a completely custom WASI binding generated from their own WIT files:

```toml
[require]
wasi = "./libs/my-wasi"  # local module generated by promise bind
```

User experience:

```promise
use wasi;

main() {
    // Filesystem
    fd := Descriptor.open("/tmp/hello.txt", OpenFlags.read_only())^;
    content := fd.read(4096)^;

    // Clocks
    now := monotonic_clock();

    // Environment
    home := environment_get("HOME")^;

    // Random
    bytes := random_bytes(32);
}
```

### `web` Module (External Catalog)

Similarly, `web` is an external catalog module:

```toml
# catalog.toml
[modules.web]
url = "github.com/anthropics/promise-web"
commit = "def456..."
```

Module structure:

```
promise-web/
    promise.toml              # [module] name = "web", epoch = "2026.3"
    web.pr                    # all Web API types + functions
    web_test.pr               # tests (run via headless browser)
```

User experience:

```promise
use web;

main() `target(web) {
    doc := document();
    element := doc.create_element("div");
    element.set_attribute("class", "container");
    element.set_inner_text("Hello from Promise!");
    doc.body().append_child(~element);
}
```

### Generated Module Contents

The `promise bind` output for each IDL follows a consistent pattern:

1. **Type definitions** — Promise types mapped from IDL records/variants/enums/resources
2. **Private extern declarations** — low-level imports with `wasm_import` annotations
3. **Public API wrappers** — ergonomic methods that marshal between Promise types and the canonical ABI
4. **`doc` annotations** — on all public declarations, generated from IDL documentation comments

---

## Browser JS Glue

### Auto-Generated Companion File

When building for `wasm32-web`, the compiler auto-generates a JavaScript file alongside the `.wasm`:

```bash
promise build --target wasm32-web app.pr
# Output:
#   app.wasm      — WASM binary
#   app.js        — JS glue module (auto-generated)
```

### Glue Architecture

The generated `app.js` provides:

1. **WASM instantiation** — loads and instantiates the `.wasm` with import bindings
2. **Import implementations** — JS functions that implement each `promise_env.*` import
3. **Reference table** — manages opaque JS object references (DOM elements, etc.)
4. **Memory helpers** — string/array marshalling between WASM linear memory and JS

```javascript
// app.js (auto-generated, simplified)

const refs = [null]; // index 0 = null sentinel
let nextRef = 1;

function refStore(obj) {
    const id = nextRef++;
    refs[id] = obj;
    return id;
}

function refLoad(id) {
    return refs[id];
}

function refRelease(id) {
    refs[id] = null;
    // TODO: free-list for slot reuse
}

function readString(memory, ptr, len) {
    return new TextDecoder().decode(
        new Uint8Array(memory.buffer, ptr, len)
    );
}

function writeString(instance, str) {
    const bytes = new TextEncoder().encode(str);
    const ptr = instance.exports.canonical_abi_realloc(0, 0, 1, bytes.length);
    new Uint8Array(instance.exports.memory.buffer, ptr, bytes.length).set(bytes);
    return [ptr, bytes.length];
}

const imports = {
    promise_env: {
        console_log(ptr, len) {
            console.log(readString(instance.exports.memory, ptr, len));
        },
        document_create_element(tag_ptr, tag_len) {
            const tag = readString(instance.exports.memory, tag_ptr, tag_len);
            return refStore(document.createElement(tag));
        },
        element_set_attribute(ref, name_ptr, name_len, val_ptr, val_len) {
            const el = refLoad(ref);
            const name = readString(instance.exports.memory, name_ptr, name_len);
            const val = readString(instance.exports.memory, val_ptr, val_len);
            el.setAttribute(name, val);
        },
        ref_release(ref) {
            refRelease(ref);
        },
        // ... more imports generated from WebIDL
    }
};

let instance;

export default async function init(wasmUrl) {
    const response = await fetch(wasmUrl || './app.wasm');
    const { instance: inst } = await WebAssembly.instantiateStreaming(
        response, imports
    );
    instance = inst;
    inst.exports._initialize();
}
```

### Usage

```html
<script type="module">
    import init from './app.js';
    await init();
</script>
```

### Reference Table and Ownership

JS objects (DOM elements, fetch responses, etc.) cannot live in WASM linear memory. The glue layer maintains a reference table: an array indexed by `i32` handles.

In the Promise binding module, each Web API type wraps a `_js_ref` handle:

```promise
type Element `public `target(web) {
    int _js_ref;

    get tag_name string `public {
        return _web_element_tag_name(this._js_ref);
    }

    set_attribute(this, string name, string value) `public {
        _web_element_set_attribute(this._js_ref, name, value);
    }

    drop(~this) {
        _web_ref_release(this._js_ref);
    }
}
```

When a `~Element` is dropped in Promise, `drop(~this)` calls `_web_ref_release`, which calls `refRelease(id)` in the JS glue. This integrates with Promise's ownership model — the ownership checker prevents use-after-drop of JS references, and the destructor ensures no JS reference leaks.

---

## Relationship to Existing PAL

The PAL remains the low-level platform abstraction for **compiler-internal runtime functions** (`pal_write`, `pal_exit`, `pal_alloc`). These are emitted unconditionally and are not user-facing.

The binding modules (`wasi`, `web`) are **user-facing Promise-level abstractions** that import additional host functions. They sit above the PAL:

```
┌──────────────────────────────────────┐
│  User Code                           │
│  use wasi; fd.read(1024)^;           │
├──────────────────────────────────────┤
│  Binding Modules (Promise)           │
│  wasi.pr: Descriptor.read() wrapper  │
│  extern + wasm_import declarations   │
├──────────────────────────────────────┤
│  Compiler Codegen                    │
│  extern.go: ABI coercion            │
│  emits wasm-import-module attrs      │
├──────────────────────────────────────┤
│  PAL (LLVM IR)                       │
│  pal_write, pal_exit, pal_alloc      │
│  hardcoded WASI/JS imports           │
└──────────────────────────────────────┘
```

**No duplication risk**: WASM runtimes deduplicate imports by `(module, name)` pair. If both the PAL and a binding module import `("wasi_snapshot_preview1", "fd_write")`, the WASM linker resolves them to the same import.

Over time, the PAL's `fd_write` and `proc_exit` imports could be refactored to use the same `wasm_import` annotation mechanism (declared in `modules/std/` with `target(wasi)`), but this is not required for correctness.

---

## Phased Implementation Roadmap

### Phase 1: Foundation

**Goal**: Enable hand-written WASM bindings using the `wasm_import` annotation. No IDL parsing yet.

**Compiler changes**:

1. **`TargetInfo.Env` field** — extend `ParseTargetInfo` for `wasm32-wasi` vs `wasm32-web`
2. **`wasi` and `web` target identifiers** — add to `matchTargetIdent` in target.go
3. **`wasm_import` annotation** — sema validation + codegen emission
4. **`WasmWebPAL`** — new PAL for browser target (imports from `promise_env`)
5. **Web linker args** — `--export=_initialize`, `--export-memory`, no `--export=_start`
6. **Post-link JS glue** — minimal hand-written template emitted after linking

**Hand-written bindings**: Write a minimal `wasi` module with a handful of functions (fd_read, fd_write, clock_time_get, random_get) to validate the end-to-end flow.

**Files to modify**:
- `compiler/internal/sema/target.go`
- `compiler/internal/codegen/extern.go`
- `compiler/internal/codegen/pal/pal.go`
- `compiler/internal/codegen/pal/wasm_web.go` (new)
- `compiler/cmd/promise/main.go`

**Files to create**:
- `modules/wasi/promise.toml` + `modules/wasi/wasi.pr` (hand-written, minimal)

### Phase 2: WIT Parser and Code Generator

**Goal**: Automated generation of WASI bindings from WIT definitions.

1. **WIT parser** — `compiler/internal/wit/` (lexer, parser, AST)
2. **Binding IR** — `compiler/internal/bindgen/ir.go` (shared intermediate representation)
3. **WIT → binding IR** — `compiler/internal/bindgen/wit_to_ir.go`
4. **Promise code generator** — `compiler/internal/bindgen/codegen.go`
5. **`promise bind wit` subcommand** — `compiler/cmd/promise/bind.go`
6. **Generate full WASI bindings** — run against canonical WASI Preview 1 WIT definitions

**Files to create**:
- `compiler/internal/wit/lexer.go`, `parser.go`, `ast.go`
- `compiler/internal/bindgen/ir.go`, `codegen.go`, `wit_to_ir.go`
- `compiler/cmd/promise/bind.go`

### Phase 3: WebIDL Parser and Browser Target

**Goal**: Browser-targeted WASM with auto-generated JS glue.

1. **WebIDL parser** — `compiler/internal/webidl/` (lexer, parser, AST)
2. **WebIDL → binding IR** — `compiler/internal/bindgen/webidl_to_ir.go`
3. **JS glue generator** — `compiler/internal/bindgen/jsglue.go`
4. **Post-link JS emission** — emit `.js` companion after `wasm-ld`
5. **`promise bind webidl` subcommand**
6. **Generate core Web API bindings** — DOM, Console, Fetch, Canvas, Storage, Timer
7. **Headless browser test runner** — run `wasm32-web` tests via Node.js or Playwright

**Files to create**:
- `compiler/internal/webidl/lexer.go`, `parser.go`, `ast.go`
- `compiler/internal/bindgen/webidl_to_ir.go`, `jsglue.go`
- `compiler/cmd/promise/bind_webidl.go`

### Phase 4: Component Model (Future)

**Goal**: Full WASI Component Model support.

1. **Component Model canonical ABI** — lift/lower functions for all types
2. **Interface composition** — WIT `world` definitions
3. **WASM component output** — binary component format (depends on `wasm-ld` support)
4. **Async/stream** — WIT `async` and `stream` → Promise `task[T]` and `Iterator[T]`

This phase depends on the Component Model specification stabilizing.

---

## Testing Strategy

### WASI Binding Tests

Test against `wasmtime` (existing infrastructure):

```bash
bin/promise test --target wasm32-wasi modules/wasi/wasi_test.pr
```

Tests verify round-trip behavior: write to a temp file via WASI bindings, read it back, assert contents match.

### Web Binding Tests

Test against Node.js (new infrastructure):

```bash
bin/promise test --target wasm32-web modules/web/web_test.pr
```

The test runner detects `wasm32-web` target and invokes `node` with the generated `.js` + `.wasm` instead of `wasmtime`. For DOM tests, a minimal DOM polyfill (e.g., `linkedom` or `happy-dom`) provides the browser APIs in Node.

### IDL Parser Tests

Go unit tests for WIT and WebIDL parsers:

```bash
go test ./internal/wit/ -v
go test ./internal/webidl/ -v
go test ./internal/bindgen/ -v
```

Parse sample IDL files, verify AST structure, verify generated Promise code compiles and type-checks.

---

## Open Questions

1. **Canonical ABI `realloc`**: The canonical ABI requires the WASM module to export a `canonical_abi_realloc` function. Should this be emitted in codegen (like `_start`) or defined in the binding module's Promise code?

2. **Async operations**: WIT `async` and browser `Promise<T>` both map to `task[T]`, but the WASM module is single-threaded. Should async operations block the cooperative scheduler, or should we integrate with the host's event loop?

3. **Error type mapping**: WIT `result<T, E>` maps to Promise failable `T!`, but Promise failable functions carry `error` (the base error type). Should we generate an `is E` error subtype for each WIT error enum, or use a single `WasiError` type with an error code?

4. **Flags representation**: WIT `flags` types are bitfields. Should they map to a value type with `has(flag)` / `set(flag)` methods, or to a set of `bool` constants, or to an integer with bitwise operations?

5. **WebIDL inheritance**: WebIDL interfaces support inheritance (`Element : Node`). Should this map to Promise `is` inheritance, or should each interface be independent with duplicated method declarations?
