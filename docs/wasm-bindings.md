# WASM Runtime Binding Architecture

> **Tag:** `wasm-bindings` — remaining work to complete this document: the query named in [docs/index.md](index.md)

> This document covers the guest→host direction. For host→guest calls — WebIDL `callback` types, `addEventListener`, timers, and the `wasm32-web` reactor execution model they require — see [web-apps.md](web-apps.md).

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

## WASM Sub Targets

A WASM sub-target is selected by an `Env` field on `TargetInfo`, alongside `OS` and `Arch`:

```go
// compiler/internal/sema/target.go
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

## The wasm import Annotation

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

## IDL to Promise Generation Pipeline

### The bind Subcommand

```bash
# Generate Promise bindings from WIT definitions
promise bind wit path/to/wasi.wit -o modules/wasi/

# Generate Promise bindings from WebIDL definitions
promise bind webidl path/to/dom.webidl -o modules/web/
```

The output is a complete catalog module directory:

```
modules/wasi/
    promise.toml          # [module] name = "wasi", epoch = "2026.0"
    wasi.pr               # all types + extern declarations + public API
    wasi_test.pr           # tests for the bindings
```

#### Generating a subset

`promise bind webidl -keep <path>` generates only what a keep list names. The list holds one entry per line; surrounding whitespace is trimmed and blank lines are skipped:

| Entry | Keeps |
|---|---|
| `I` | interface `I`; a `namespace` is listed the same way |
| `I.m` | on a kept `I`, the attribute `m`, or every operation named `m` |
| `I.constructor` | on a kept `I`, every constructor |

```bash
promise bind webidl -keep keep.txt -name web -o modules/web/ idl/
```

Entries are matched after partials and mixins are merged, so a mixin's member is listed under each interface that includes it. The list selects from the input rather than declaring it: an entry that names nothing there selects nothing and is not an error, so a list derived from browser-compat data may name far more than the bound IDL defines. A line of any other shape — an empty part, more than one `.`, inner whitespace — is an error naming its line, and every such line is reported before anything is written.

Constants, iterable, maplike and setlike declarations, and anonymous special operations have no entry of their own: they go with their interface, or a constant with its namespace, and keep every member sharing their Promise name (below). An unlisted interface is dropped with its members, unless a kept interface inherits from it: an ancestor stays, with its listed members and the names its descendants keep (below), so the `is` clause resolves. Dictionaries, enums, typedefs and callbacks are never listed.

A member's Promise name is the unit: the name it generates before any ordinal, which is what [Overloaded Operations](#overloaded-operations) and [Shadowed Members](#shadowed-members) count. So `fooBar` and `foo_bar` are one unit, and so are a constant `RESET` and an operation `reset`; all constructors are one, `constructor`, together with any operation whose name generates it. An entry, or a member that goes with its interface, keeps its whole unit, so the ordinals never depend on the list. The unit spans the inheritance chain, because a derived member is numbered after the instance members of its Promise name it inherits: a name an interface keeps — by an entry, or by a member that goes with it — is kept on each of its ancestors too, listed there or not, and a name dropped from an interface for naming a dropped definition (below) is dropped from every kept interface inheriting from it. Only instance members are inherited — statics, constants and constructors are not ([Shadowed Members](#shadowed-members)) — so neither direction reaches an ancestor that holds no instance member of that name: `I.constructor` keeps no ancestor's constructors, unless that ancestor also declares an instance operation whose name generates `constructor`, which takes them with it. Nor does the list change which JS member an import reaches: an import calls its declaring interface's own member when any interface in the input redeclares it, or when an interface in its declaring interface's inheritance chain carries `[LegacyOverrideBuiltIns]`, kept or not (§"WebIDL to Promise", interface inheritance).

The subset never leaves a dangling reference. To a fixed point, a typedef whose type names a dropped definition is dropped, and so is a dictionary whose parent is dropped or whose `required` member names one. Then an optional dictionary member that names a dropped definition is removed, and an interface or namespace member that names one is dropped together with every member sharing its Promise name. `bind` reports each of these on stderr — an interface or namespace member by its IDL name — with the definition it named. A name inside a union, `Promise<T>` or `record<K, V>` is not a reference, because each of those lowers to `JsValue`. A name that no bound IDL file defines counts as a definition the list dropped: what names it is dropped or removed in the same closure, and its report says the name is not defined in the bound IDL files rather than that the list drops it. So a keep list never has to bind a spec only because what it keeps names that spec's definitions. Without a keep list, such a name is a `bind` error (§"Supported subset").

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

#### Supported subset

Top-level constructs: `interface` (with inheritance), `interface mixin` / standalone `mixin`, `dictionary` (with inheritance), `enum`, `typedef`, `callback`, `namespace`, `partial interface`, `partial mixin`, `partial dictionary`, `partial namespace`, and `includes` statements. A namespace's members are, as WebIDL defines them, regular operations, readonly attributes and constants; any other member of a namespace or partial namespace is a parse error naming the namespace and the member's position. `partial`/`mixin`/`includes` are merged into their targets by `webidl.Merge` before IR conversion. A partial definition extends the interface, mixin, dictionary or namespace of its name, and an `includes` statement names an interface and a mixin, so each must find them among the bound IDL files. One that does not is a `bind` error naming the definition, its position and what it lacks, reported for every such definition before anything is written. Nothing in the generated module would name its members, so, as with an unbound dictionary parent (§"WebIDL to Promise", dictionary inheritance), their absence could not surface as a compile error. The remedy is binding the spec that defines the target.

A type references a definition by the name it carries once lowered (§"Type Mapping") with no typedef inlined: a use of a typedef names the typedef, a name defined outside IDL names what it lowers to (§"WebIDL to Promise", names defined outside IDL), and a name inside a union, `Promise<T>` or `record<K, V>` names nothing, because each of those binds as `JsValue`. A name is defined when the bound IDL files define an interface, dictionary, enum, typedef or callback of that name; a namespace is not a type, so its name defines nothing. Each typedef, dictionary member, interface member and namespace member whose type names a name that is not defined is a `bind` error naming the declaration, its position and the name, reported for every such declaration before anything is written: the generated module would otherwise name a type that does not exist, and fail at its self-check rather than at the cause. The remedy is binding the spec that defines the name, or a keep list, which drops what names it ([Generating a subset](#generating-a-subset)).

Member-level features:

- **Extended attributes** (`[Exposed=Window]`, `[CEReactions]`, `[RuntimeEnabled=Foo]`, …) are accepted and skipped at every position they may appear: top level, before interface/dictionary members, and **inline before a type** (e.g. `(TrustedHTML or [LegacyNullToEmptyString] DOMString)`). They are parsed for structure and, with three exceptions, do not affect generated bindings: `[LegacyOverrideBuiltIns]`, on an interface or a partial interface, and `[LegacyUnforgeable]`, on an attribute or an operation, decide which JS member an import calls (§"WebIDL to Promise", interface inheritance); and `[LegacyWindowAlias]`, on an interface definition, makes each identifier it names — `[LegacyWindowAlias=X]`, or the list form `[LegacyWindowAlias=(X, Y)]` — name that interface wherever it is used as a type (§"WebIDL to Promise", names defined outside IDL).
- **Dictionary members** support `required`, default values, and leading extended attributes. Non-`required` members map to `T?` (Promise optional).
- **Default values** cover string/number/boolean/identifier literals, the empty sequence `[]`, and the **empty dictionary `{}`** (e.g. `optional SetHTMLOptions options = {}`). Default values are parsed but not yet materialized into the generated bindings.
- **Operation members**: regular, `static`, `getter`/`setter`/`deleter`, `stringifier`, `legacycaller`, `constructor`, `iterable<…>`, and `async` operations. A named `legacycaller` binds as the regular operation of its name, in that name's overload set ([Overloaded Operations](#overloaded-operations)); an anonymous one — calling the object itself — binds no member. A named `getter`, `setter` or `deleter` likewise binds as the regular operation of its name, and its import calls that method. An anonymous one binds as a member named by its keyword, numbered in that name's overload set: `HTMLFormElement`'s indexed and named getters are `getter` and `getter_2`. Its import, `<Interface>.<keyword>`, reaches the host as WebIDL exposes it on a legacy platform object: as property access keyed by its first argument. A getter is `obj[key]`, a setter `obj[key] = value`, and a deleter `delete obj[key]`; an `unsigned long` index is passed as its unsigned value. The result is whatever that access yields. For a key the object does not support, that is `undefined`, which a nullable result receives as absent. Property access passes only a key, plus a value for a setter, and returns no setter result. So an anonymous getter or deleter that does not take exactly one argument, or a setter that does not take exactly two or that declares a result, is a `bind` error naming the operation and its position.
- **Maplike and setlike declarations.** `maplike<K, V>` and `setlike<V>`, `readonly` or not, bind as the members WebIDL declares for them, each named, numbered and imported as an attribute or operation of its name the IDL writes would be. Both declare a readonly `unsigned long` attribute `size` and `boolean has(K key)`; a maplike also declares `V? get(K key)`, absent for a key the map does not hold. Unless the declaration is `readonly`, a maplike adds `set(K key, V value)`, a setlike `add(V value)`, and both `boolean delete(K key)` and `clear()`; a setlike's key parameters are named `value`. `set` and `add` return nothing: the JS method returns the collection itself, which the caller already holds. WebIDL lets the interface write any of these four as a regular operation of its own, which overrides the one the declaration adds: `FontFaceSet`'s `FontFaceSet add(FontFace font)` is its only `add`. Their imports reach the object's own `size` getter and methods: `Counts.get` calls `obj.get(key)`. WebIDL permits one such declaration per interface, so a second, counted after partial interfaces are merged, is a `bind` error naming the interface and both positions. Iteration is not bound (#751).
- **`inherit attribute`** binds as the read-write attribute it declares. `inherit` only takes the getter of the attribute of its identifier on the nearest ancestor; the derived prototype's property is still a read-write accessor, so the declaration redeclares the inherited attribute and is named as [Shadowed Members](#shadowed-members) says. WebIDL's grammar has no `inherit readonly attribute`, so `inherit` before anything but `attribute` is a parse error.
- **Hyphenated identifiers.** A WebIDL identifier may contain `-` and begin with one, as CSSOM's dashed attribute names do (`margin-top`); `-Infinity` stays a float literal. In the Promise name a `-` is a word boundary written `_`, and a leading `-` writes nothing: `margin-top` generates `margin_top` and `-webkit-transform` generates `webkit_transform`, the Promise names of `marginTop` and `webkitTransform`, so each is one [unit](#generating-a-subset) with them. The import keeps the IDL spelling (`CSSPageDescriptors.margin-top.get`), and the glue reaches the member by it, with bracket access when the name is not a JavaScript identifier: `obj["margin-top"]`.
- **Types**: all WebIDL primitives, `sequence<T>`, `FrozenArray<T>`, `ObservableArray<T>`, `Promise<T>`, `record<K,V>`, parenthesized union types, nullable `T?`, and the typed-array/buffer family.

Known coverage gaps (tracked, would currently fail or be dropped): iteration — the `entries`, `keys`, `values` and `forEach` of an `iterable`, `maplike` or `setlike` declaration (#751), and `callback interface` (parsed then discarded, T0719). `callback` function-type aliases are parsed but not yet emitted to the IR (they need lambda types). Generated `JsValue`-bearing bindings currently emit malformed `` `doc `` annotations (T0717).

### Promise Code Generator

The shared code generator (`compiler/internal/bindgen/codegen.go`) traverses the binding IR and emits `.pr` source files. It handles:

- Type mapping (see tables below)
- Extern function declarations with `wasm_import` annotations
- Public wrapper methods that provide ergonomic APIs
- Resource types with `drop(~this)` destructors
- `doc` annotations on all public declarations

---

## Type Mapping

### WIT to Promise

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
| `resource` | `type { i32 _handle; bool _owned; drop(~this) { ... } }` | Ownership type |
| `own<R>` | `~R` (parameter) / `R` (return) | Unique ownership |
| `borrow<R>` | `&R` | Shared reference |

### WebIDL to Promise

| WebIDL Type | Promise Type | Notes |
|-------------|-------------|-------|
| `boolean` | `bool` | Direct |
| `byte` / `octet` | `i8` / `u8` | |
| `short` / `unsigned short` | `i16` / `u16` | |
| `long` / `unsigned long` | `i32` / `u32` | |
| `long long` / `unsigned long long` | `i64` / `u64` | |
| `float` / `double` | `f32` / `f64` | |
| `DOMString` / `USVString` / `CSSOMString` | `string` | CSSOM defines `CSSOMString` in prose as a `DOMString` or a `USVString`, at the implementation's choice (see below) |
| `ByteString` | `u8[]` | |
| `sequence<T>` | `T[]` | |
| `Promise<T>` | `task[T]` | Maps to Promise concurrency |
| `T?` (nullable) | `T?` | Promise optional |
| `void` / `undefined` | (void) | |
| `interface` | `type { int _js_ref; ... }` | Opaque JS handle |
| `interface I : P` | `type I is P { ... }` | Inherits `P`'s members; the handle and ownership fields are declared once, on the root of the chain; `P` carries `` `open `` (see below) |
| `WindowProxy` | `Window` | HTML defines it in prose as the proxy for a `Window` (see below) |
| `X`, of `[LegacyWindowAlias=X] interface I` | `I` | The alias names the interface it annotates (see below) |
| `namespace N` | `type N′ { ... }` | `N′` is `N` with its first letter upper-cased; every member `` `global ``; no `drop` (see below) |
| `dictionary` | `type { ... }` | Value type |
| `dictionary D : P` | `type D { ... }` | Carries `P`'s members, and its ancestors', as fields of its own; no `is` (see below) |
| `enum` | `enum { ... }` | Variants named from the values (see below); with string mapping for JS |
| `callback` | `(Args) Return` | Lambda type |
| `typedef T N` | the type `T` binds as | Inlined at each use of `N`; no type is named `N` (see below) |
| `ArrayBuffer` | `u8[]` | |
| `any` | `JsValue` | Tagged enum (see below) |
| `object` | `JsValue` | Same enum, `Object` variant |

**Names defined outside IDL.** `CSSOMString`, `WindowProxy` and each identifier a
`[LegacyWindowAlias]` names are used as types, but no IDL definition defines them: specification
prose or an extended attribute does. Each lowers to what it names, so it is the same type as that:
a derived member that spells an inherited one's type by the other name is exactly that member
([Shadowed Members](#shadowed-members)), and a keep list that drops `Window` or `DOMMatrix` drops
what names `WindowProxy` or `SVGMatrix`, reporting the definition it names
([Generating a subset](#generating-a-subset)). An alias is known only from the bound IDL, from the
interface definition carrying it. So `SVG.idl`'s `SVGMatrix`, bound without the `geometry.idl` that
declares it on `DOMMatrix`, names a definition absent from the input (§"Supported subset",
§"Generating a subset"), `SVGMatrix`; and a `WindowProxy` with no `Window` bound names one too,
`Window` — never `WindowProxy`.

**Enums.** A WebIDL enum value is any string, so the name of the variant it generates follows one
rule that every string satisfies. A word is a maximal run of ASCII letters and digits; every other
character — `-`, `_`, a space, `/`, `+`, any non-ASCII character — separates words and is dropped.
The variant is the words in order, each with its first letter upper-cased and the rest as written:
`"read-write"` is `ReadWrite`, `"text/html"` is `TextHtml` and `"application/xhtml+xml"` is
`ApplicationXhtmlXml`. A name that would begin with a digit is prefixed with `V`, so `"2d"` is
`V2d`; the prefix is not `_`, which marks a member private
([Member Level Visibility](language-design.md#member-level-visibility)). A value with no letter or
digit, `""` among them, is `Empty`. The rule spells the Promise name only: the value's string is
still what the variant stands for on the host. Two values of one enum that generate one variant —
`"a-b"` and `"a_b"`, or `""` and `"empty"` — are a `bind` error naming the enum, its position, both
values and the variant. Every value whose variant an earlier value of its enum already generates is
reported, against that first value, before anything is written.

**Typedefs.** A typedef names no Promise type. Each use of it binds as its target written in its
place, through any number of typedefs: with `typedef DOMString Name;`, `attribute Name label;` is a
`string` attribute, and with `typedef Name? MaybeName;`, a `MaybeName` parameter is a `string?`.
A use written `?` is nullable over its target, and a target that is nullable already stays
nullable once, since WebIDL allows no nullable of a nullable type. A union target binds as
`JsValue`, as a union written in place does, and a callback target as that callback (the
`callback` row): HTML's `EventHandler` is `EventHandlerNonNull?`. Being its target, a typedef is
that type wherever two types are compared, so a member redeclared with a typedef of the type it
inherits is exactly alike ([Shadowed Members](#shadowed-members)). A typedef that refers to itself,
directly or through other typedefs, cannot be inlined: it is a `bind` error naming it and its
position, reported for every typedef on the cycle before anything is written. A name inside a
union, `Promise<T>` or `record<K, V>` is not followed, because each of those binds as `JsValue`.

**Namespaces.** A namespace's members live on the namespace object, as an interface's static
members live on its interface object, so a namespace binds as a type whose every member is
`` `global ``: its regular operations, readonly attributes and constants bind as an interface's
static operations, static attributes and constants do — named, numbered
([Overloaded Operations](#overloaded-operations)) and documented alike, after its partial
namespaces are merged in. `console`'s `log` is `Console.log(...)`. Each member's import is named
by the namespace's IDL name, `console.log`, and the glue calls the namespace object, the global of
that name, as it calls an interface object for a static. The type is named by the namespace's
identifier with its first letter upper-cased — `console` is `Console`, and `CSS` stays `CSS` —
because Promise names a type in PascalCase
([Naming Conventions](language-design.md#naming-conventions)), and because a module-level root
named for a seeded namespace ([Reference Table and Ownership](#reference-table-and-ownership))
must not redeclare the type's name. A namespace whose type name is also the name of an interface,
a dictionary, an enum, or an earlier namespace's type is a `bind` error naming the namespace, its
position and that definition, reported for every such namespace before anything is written; a
typedef or a callback declares no type, so it takes no name. Nothing owns a namespace object — no
constructor or result produces one — so the type declares no `drop`. Like every wrapper it
declares the `borrow` factory, which a root over a seeded namespace object builds it with.

**Interface inheritance.** A derived interface is an `is` child of its parent, so the generated
wrappers model the IDL hierarchy (`EventTarget` → `Node` → `Element` → …). Each member — its public
wrapper, its private extern and its JS import — is generated once, on the interface that declares
it, and every derived wrapper inherits it: `Node.appendChild`'s one import serves an `Element`
too, because the JS side resolves the member on the object, through its own prototype chain, and
reaches `Node`'s (except for a redeclared member, below). A derived wrapper is accepted wherever
its parent type is expected, so passing an `Element` to a `Node`-typed parameter needs no
rewrapping of its handle. A derived interface that redeclares an
inherited member overrides it only when the two are exactly alike; any other redeclaration is a
member of its own ([Shadowed Members](#shadowed-members)). Each wrapper still declares its own
`borrow` factory — a derived type's `` `global `` of the same name hides the parent's, so
`Element.borrow` returns an `Element` — and its own guarded `drop(~this)`, which is dispatched
virtually: a derived wrapper held through a parent-typed binding releases its handle exactly once.
A parent interface is concrete (every wrapper is constructed field-wise), so it carries
`` `open `` to permit the `is` ([Sealed by Default](language-design.md#sealed-by-default)). A
parent that is not among the bound IDL files is still named in the `is` clause, so the generated
module fails to compile with `undefined type` rather than silently omitting the inherited
members.

Resolving a member on the object reaches a different member when an interface between the
declaring one and the object's real interface redeclares it, and that member's result would cross
under the declaring interface's type. So the import of a member that any interface inheriting from
the declaring one redeclares calls the declaring interface's own member instead, read from its
interface prototype object with the object as receiver:

- an attribute: `Reflect.get(Element.prototype, "className", obj)`, and
  `Reflect.set(Element.prototype, "className", value, obj)`;
- an operation: `Reflect.apply(HTMLCollection.prototype.namedItem, obj, args)`.

Its result is the one the declaring interface specifies, under the type it declares: an `Element`
wrapper over an SVG element reads its class string rather than `SVGElement`'s `SVGAnimatedString`,
and `HTMLCollection`'s `named_item` on an `HTMLFormControlsCollection` returns an `Element`, never a
`RadioNodeList`. A redeclaration counts whatever its signature, an exact one included: a derived
interface that redeclares one overload of a set replaces the whole set on its prototype, and the
overloads it leaves out must still reach the declaring interface's. It counts as a regular
attribute, a regular operation or a constant — the members an interface prototype object carries.
Redeclarations are found in the whole input, after partials and mixins are merged and before a keep
list reduces it ([Generating a subset](#generating-a-subset)); one in IDL that is not bound is
unknown, and the import resolves on the object. Static members, constants and constructors are
reached through the interface object already. A `[LegacyUnforgeable]` member lives on the object
rather than its prototype, but WebIDL forbids an inheriting interface to redeclare one, so none is
affected.

A named property can hide a member too. An interface that carries `[LegacyOverrideBuiltIns]`, on
its definition or on a partial interface — `HTMLFormElement`, `Document` and `DOMStringMap` in the
platform IDL — makes each supported named property of an object implementing it a property of the
object itself, over any member of that name on its prototype chain: a form holding
`<input name="action">` answers `form.action` with that input, and one holding a control named
`submit` has no callable `submit`. Such an object is passed to the imports of every interface in
that interface's inheritance chain: those it inherits from, through a parent-typed wrapper, and
those inheriting from it, whose objects implement it too. So every regular attribute and regular
operation of an interface in such a chain is imported in the form above as well, redeclared or not:
`Element`'s `id`, called on a form, reads the form's id whatever control is named `id`. In the
platform IDL those interfaces are `EventTarget`, `Node`, `Element`, `HTMLElement`,
`HTMLFormElement`, `Document` and `XMLDocument`, with the mixin members each includes; an
interface outside every such chain — `HTMLDivElement`, or `Window`, which shares only
`EventTarget` — resolves on the object as before. A `[LegacyUnforgeable]` member is the exception: it lives on the object, and a
named property never hides it, so its import resolves on the object (`Document.location`). The
chains are found as redeclarations are, in the whole input after merging and before a keep list.

**Dictionary inheritance.** A derived dictionary's type is flattened, not an `is` child: it
declares every member the dictionary has as a field of its own — its ancestors' first, from the
root of the chain down, then its own, each dictionary's in declaration order with its partial
dictionaries merged in, and each keeping its own `required`-ness. So
`MouseEventInit(bubbles: true, client_x: 3)` sets an inherited member and an own one alike. A
dictionary crosses the boundary by value, as one complete set of fields; it is not accepted where
an ancestor dictionary is expected — the caller builds that dictionary instead. A dictionary whose
parent, or any further ancestor, is not a dictionary among the bound IDL files is a `bind` error
naming the dictionary and the missing ancestor, and so is an inheritance cycle: a flattened type
never names its parent, so the missing members could not surface as a compile error the way an
unbound interface parent does.

#### Overloaded Operations

Promise has no overloading ([language-design.md § No Overloading](language-design.md#no-overloading)), so a WebIDL overload set becomes distinct members. An overload set is the operations of one interface that generate the same Promise name, counted after partials and mixins are merged, in that merged declaration order; a derived interface's set also counts the instance members it inherits under that name ([Shadowed Members](#shadowed-members)). The first keeps its name. The k-th (k ≥ 2) carries the suffix `_<k>` on every name derived from it, and its import appends `.<k>` to the import name:

| IDL | Promise member | Extern | Import |
|---|---|---|---|
| `undefined setAttribute(DOMString name, DOMString value);` | `set_attribute` | `_element_set_attribute` | `Element.setAttribute` |
| `undefined setAttribute(DOMString name, TrustedHTML value);` | `set_attribute_2` | `_element_set_attribute_2` | `Element.setAttribute.2` |

Each overload keeps its own signature exactly; none is widened to `JsValue` to merge it with another. The JS glue implements every import of the set by calling the same JS member (`setAttribute`) with that overload's own arguments, so the host's overload resolution — not a dispatcher in the bindings — chooses among them. The rule applies to regular, static, and constructor operations alike; a constructor's factory becomes `create_<k>` (or `create_handle_<k>`, under the [factory-name fallback](#wit-resources-to-promise-ownership)).

The suffix is an ordinal because nothing else is unique: overloads can share an arity, as the two `setAttribute`s do, and several IDL types lower to the same Promise type (`JsValue`, `i32` handles).

#### Shadowed Members

WebIDL lets a derived interface redeclare a member it inherits, with a different signature. Promise has no shadowing for instance members: a same-named member of an `is` child takes over the inherited slot and must satisfy it ([Override signatures](language-design.md#how-inheritance-maps-to-vtables)). So the overload set of a Promise name is counted across the `is` chain. A derived interface's instance member that is **exactly** an inherited instance member of its name — the same kind of member (attribute or operation), the same parameter types in order and the same result, parameter names aside — overrides it and keeps its name. Any other member of that name is numbered after the highest ordinal among the instance members the interface inherits under that name, and carries the suffixes of [Overloaded Operations](#overloaded-operations):

| IDL | Promise member | Extern | Import |
|---|---|---|---|
| `HTMLCollection`: `getter Element? namedItem(DOMString name);` | `named_item` | `_html_collection_named_item` | `HTMLCollection.namedItem` |
| `HTMLFormControlsCollection : HTMLCollection`: `getter (RadioNodeList or Element)? namedItem(DOMString name);` | `named_item_2` | `_html_form_controls_collection_named_item_2` | `HTMLFormControlsCollection.namedItem.2` |
| `Element`: `attribute DOMString className;` | `class_name` | `_element_class_name` | `Element.className.get` |
| `SVGElement : Element`: `readonly attribute SVGAnimatedString className;` | `class_name_2` | `_svg_element_class_name_2` | `SVGElement.className.get` |

The inherited member stays: `HTMLFormControlsCollection` has `named_item`, typed as `HTMLCollection` declares it, beside its own `named_item_2`; its import calls `HTMLCollection`'s own `namedItem` (§"WebIDL to Promise", interface inheritance). A renamed attribute's getter and setter share its ordinal, and its import carries none: an interface declares an attribute once, so the import is unique already, and the glue reads the accessor from its last component. An exact redeclaration is an override, so `DOMRect` re-declaring `DOMRectReadOnly`'s readonly `x` as read-write overrides the getter and adds a setter.

The test is identity, not the override rule's relaxed matching, so that a binding's names depend on the IDL and the [type mapping](#webidl-to-promise) alone: identity is an override under every version of that rule. A refinement the rule happens to admit is therefore a new member as well — a `CaptureTrack clone()` redeclaring `Track clone()` becomes `clone_2`, and a `start` that adds optional parameters becomes `start_2` — while the inherited `clone()` and `start()` still serve the derived wrapper. Static operations, static attributes, constants and constructors are `` `global ``, never inherited slots: a derived interface's static keeps its name and hides the parent's static of that name.

### JsValue the Dynamic Value Enum

WebIDL's `any` and `object` types are dynamic, but Promise doesn't need a built-in `any` type to represent them. Following the same pattern as `JsonValue` in the `json` module, the `web` binding module defines a `JsValue` enum — a tagged union that covers all JS value types using standard language primitives:

```promise
enum JsValue `public `doc("Represents a dynamic JavaScript value.") {
    Undefined,
    Null,
    Bool(bool value),
    Number(f64 value),
    Str(string value),
    Object(i32 _js_ref),
    Array(i32 _js_ref),
    Function(i32 _js_ref),

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

    get(this, string key) JsValue `public `doc("Access a property by name. Returns Undefined if not found.") {
        return _web_js_value_get(this, key);
    }

    set(this, string key, JsValue value) `public `doc("Set a property by name.") {
        _web_js_value_set(this, key, value);
    }
}
```

Primitive JS values (`boolean`, `number`, `string`, `null`, `undefined`) are extracted and stored directly in the enum variants — no JS reference needed. Complex JS values (`object`, `array`, `function`) are stored as opaque `_js_ref` handles managed by the JS glue's reference table. This avoids runtime type tagging while keeping the representation fully explicit and pattern-matchable.

**Crossing the boundary.** A `JsValue` crosses as one `i32` handle into the glue's reference table, in both directions, so an extern taking or returning one declares `i32`. The binding converts eagerly, in two private module-level functions that sit beside its externs and carry the same `` `target `` — not in methods of the enum, which carries none, so the conversions' host calls never stop the enum compiling on other targets. Lowering passes the handle an `Object`, `Array` or `Function` variant keeps; any other variant's value the glue stores in a temporary slot, which the import receiving it releases once it has read it. A `JsValue?` parameter lowers `none` as `Undefined`, so the host receives `undefined` — to WebIDL a missing optional argument, and `null` for a nullable type. Lifting asks the glue which variant the handle's value is: an object, array or function keeps the handle in its variant, while a primitive is copied into its variant and its slot released. A JS value that is none of the others — a `bigint`, a `symbol` — lifts as `Object`.

### Canonical ABI Representation

For WASI, compound types are serialized into WASM linear memory using the canonical ABI:

**Scalars**: every integer narrower than 64 bits flattens to a core `i32` (`u8`, `u16`, `u32`, `s8`, `s16`, `s32`, `char`, `bool`), 64-bit integers to `i64`, and floats to `f32`/`f64`. The extern declares that flat type; the public wrapper keeps the WIT type from the mapping table above and **converts at the boundary** — unsigned values widen zero-extended, signed ones sign-extended, `bool` lowers to 0/1 and lifts as a non-zero test, and `char` carries its code point. Promise has no implicit numeric conversion, so this conversion is not an optimization: a wrapper that returned the extern's result verbatim would not compile.

A scalar read back out of the **return area** is narrowed to its stored width first. There it is not a flat core value but a value in memory, stored in its own width (a `bool` and an `s8` occupy one byte), while the load helpers are `i32`/`i64`-wide — so the padding that follows the payload rides along in the upper bits, and the canonical ABI does not require a host to zero it. Narrowing discards it; without that step a stored `false` would test non-zero and lift as `true`.

**Strings**: `(i32 ptr, i32 len)` — UTF-8 bytes in linear memory. The generated binding:
1. Calls `cabi_realloc(0, 0, 1, len)` to allocate space
2. Copies Promise string bytes into linear memory
3. Passes `(ptr, len)` to the WASI import
4. For returns: reads `(ptr, len)`, constructs Promise string, frees allocation

**Lists**: `(i32 ptr, i32 len)` — contiguous array of canonical-ABI-encoded elements.

**Records**: Fields laid out sequentially with natural alignment. Generated code uses helper functions for serialization.

**Variants/Results/Options**: Discriminant `i32` tag followed by the active case's payload, padded to the largest variant.

**Resource handles**: `i32` index into the host's handle table. No serialization — passed directly as WASM i32 values.

### Helper Symbols and Their ABI

The canonical-ABI helpers — `cabi_realloc`, `cabi_retarea_ptr`, `cabi_load_*`/`cabi_store_*`, `cabi_string_data`/`_len`/`_from`, `cabi_vector_data`/`_len`/`_from` — are defined in the compiler's own `wasm_alloc.c` and **statically linked**, not imported. They cross the `` `extern `` boundary in the **raw platform C ABI**: scalars by value, `string` and `T[]` as their bare instance/header pointer, and scalar or pointer results returned directly.

This is a different convention from the one the platform-layer symbols the backend emits use. Those cross in Promise's value-struct bridge form — a leading `sret` result pointer, and every value parameter as a pointer to its value struct — because the backend writes both sides. It is also different from the `wasm_import` convention above, where a `string` parameter flattens to `(i32 ptr, i32 len)` and a `string` result returns as the same pair, written by the host through a trailing return pointer the caller supplies. The host allocates those bytes with `cabi_realloc`; the caller copies them into a Promise string and releases the buffer with `pal_free`, the allocator `cabi_realloc` uses. A JS host cannot know Promise's boxed-string layout, whereas `wasm_alloc.c` reads that layout deliberately.

A `T?` result, where `T` is a string or a scalar of one core value (`bool`, `char`, an integer of at most 64 bits, `f32` or `f64`), comes back through a trailing return pointer too. It points to a 16-byte area, aligned to 8, that the caller zeroes before the call. The area holds an `i32` presence flag at offset 0, 1 for a value and 0 for none. When the flag is 1, the value follows at offset 8, written as the import would return it directly: an `i32` for `bool`, `char` and every integer of 32 bits or fewer, otherwise an `i64`, `f32` or `f64`, and a string as its `(ptr, len)` pair, allocated and released as above. So the bindgen glue returns a `null` or `undefined` result as `none`, which the program tells apart from `""`, `0` or `false`.

**The helpers allocate from the program's own heap.** `cabi_realloc` and the constructing helpers `cabi_string_from`/`cabi_vector_from` allocate through the PAL allocator (`pal_alloc`/`pal_realloc`), never a second heap beside it. A block a host obtains from `cabi_realloc`, and a value a `_from` helper builds, is therefore an ordinary Promise allocation: counted by the leak check, and released with `pal_free` like any other. `cabi_realloc` is the only allocator a host calls; the PAL allocator itself is not exported.

**Which ABI a symbol carries is a property of the symbol, not of the declaration**, so it is registered once on the compiler side rather than marked up in generated binding source. Generated bindings therefore need no annotation for it, and a binding generated against an older compiler does not need regenerating when the set changes.

### Signature Mismatches Are Build Errors

**A `wasm-ld` `function signature mismatch` fails the build.** wasm-ld resolves a mismatch by replacing the call with a stub that executes `unreachable` and emitting only a warning, so a build that reports success can produce a module that traps on its first canonical call — and the trap names the call site rather than the declaration that was wrong. The compiler scans the linker's output and fails the build, naming every mismatched symbol.

This does not catch everything, which is why it is a backstop rather than the gate: on `wasm32` a pointer and an `i32` are the same type, so a mismatch such as `void(i8*, i8*)` against C's `void(int, int)` links silently. The gate is a canonical binding that is built **and run**.

---

## WIT Resources to Promise Ownership

WIT resources have lifecycle semantics that map naturally to Promise's ownership model:

| WIT Concept | Promise Mapping |
|-------------|----------------|
| `resource R` | `type R { i32 _handle; bool _owned; drop(~this) { ... } }` |
| `own<R>` (parameter) | `~R` — consumed, caller loses access |
| `own<R>` (return) | `R` — caller receives ownership |
| `borrow<R>` (parameter) | `&R` — borrowed, not consumed |
| `constructor` | `R.create(...) R` — factory returning `R`, owning the handle it mints (never `new(~this, ...)`: an explicit `new` replaces field-wise construction, which `borrow` and every resource-returning wrapper need) |
| `method` (self: borrow) | Method taking `this` (shared ref) |
| resource drop | `drop(~this)` calls host's `[resource-drop]` import **when `_owned`** |
| (generated) | `R.borrow(i32 handle) R` — non-owning wrapper around a handle someone else owns |

**Factory names.** A factory is named `create_handle` or `borrow_handle` instead when the type
already has a member of the default name: one the resource declares, other than its constructor,
or an instance operation or attribute it inherits from any ancestor — a `` `global `` factory of
that name would take over the inherited slot and must satisfy it
([Override signatures](language-design.md#how-inheritance-maps-to-vtables)). An inherited static
operation, static attribute or constant does not count: the factory hides it, as a derived
interface's `` `global `` hides its parent's ([Shadowed Members](#shadowed-members)). An
overloaded constructor's ordinal goes on whichever name applies: `create_2`, or `create_handle_2`.
There is one fallback level: a type that also has a member named `create_handle` or
`borrow_handle` fails to compile, rather than having its factory silently renamed again.

Example generated binding for a WIT filesystem resource:

```promise
type Descriptor `public {
    i32 _handle;
    bool _owned;

    borrow(i32 handle) Descriptor `public `global `doc("Wrap an existing handle without taking ownership.") {
        return Descriptor(_handle: handle, _owned: false);
    }

    create(string path) Descriptor `public `global `doc("Open a descriptor, taking ownership of the handle the host mints.") {
        handle := _wasi_descriptor_constructor(path);
        return Descriptor(_handle: handle, _owned: true);
    }

    stat!(this) DescriptorStat `public `doc("Get file metadata.") {
        return _wasi_descriptor_stat(this._handle)^;
    }

    read!(this, u64 length) u8[] `public `doc("Read bytes from the file.") {
        return _wasi_descriptor_read(this._handle, length)^;
    }

    write!(this, u8[] data) u64 `public `doc("Write bytes to the file.") {
        return _wasi_descriptor_write(this._handle, data)^;
    }

    drop(~this) {
        if this._owned {
            _wasi_descriptor_drop(this._handle);
        }
    }
}

// Low-level WASI imports (private)
_wasi_descriptor_constructor(string path) i32
    `extern("descriptor_constructor")
    `wasm_import("wasi:filesystem/types", "[constructor]descriptor");

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
    consume(move fd);               // fd moved to consume()
    fd.read(1024);              // ERROR: use of moved variable 'fd'
}
```

---

## Binding Module Structure

### Why External and Not Embedded

Embedded modules (like `std`, `json`, `os`) are baked into the compiler binary and tied to its release cycle. Binding modules are deliberately kept external because:

1. **Version independence.** A project can target WASI Preview 1, WASI Preview 2, or a custom WASI extension — all from the same compiler epoch. Different `[require]` pins in `promise.toml` select different binding versions without waiting for a compiler release.

2. **Custom IDL support.** Game engines, cloud runtimes, and embedded systems define their own host APIs via WIT or WebIDL. Users run `promise bind wit engine.wit -o libs/engine/` to generate project-local bindings. These are first-class catalog modules — no special compiler support needed.

3. **Vendor-specific modules.** Browser vendors may expose non-standard APIs. Community-maintained modules like `web_chrome` or `web_safari` can provide vendor-specific bindings without bloating the standard `web` module.

4. **Faster iteration.** Binding definitions change more frequently than the compiler. External modules can be updated, tested, and released independently.

### The wasi Module

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
    promise.toml              # [module] name = "wasi", epoch = "2026.0"
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

### The web Module

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
    promise.toml              # [module] name = "web", epoch = "2026.0"
    web.pr                    # all Web API types + functions
    web_test.pr               # tests (run via headless browser)
```

User experience:

```promise
use web;

main() `target(web) {
    doc := web.document;
    element := doc.create_element("div");
    element.set_attribute("class", "container");
    element.set_inner_text("Hello from Promise!");
    doc.body().append_child(move element);
}
```

### Generated Module Contents

The `promise bind` output for each IDL follows a consistent pattern:

1. **Type definitions** — Promise types mapped from IDL records/variants/enums/resources
2. **Private extern declarations** — low-level imports with `wasm_import` annotations
3. **Public API wrappers** — ergonomic methods that marshal between Promise types and the canonical ABI
4. **`doc` annotations** — on all public declarations. A declaration's text is the IDL documentation comment on the construct it binds: in WIT the `///` comment before the item; in WebIDL the `//` and `/* … */` comments directly before a definition or member, either before its extended attributes or between them and the declaration. A blank line between a comment and the declaration detaches it, so a file's header comment documents nothing, and neither does a comment that follows code on the same line. Specification IDL carries almost no comments — the prose that documents a member lives in the specification's text, outside the IDL block — so a WebIDL interface, namespace, dictionary, enum, attribute, operation or constant with no comment carries a generated line naming what it binds, on the interface that binds it: `` `Element.setAttribute` (WebIDL operation) ``. An attribute's getter and setter carry the same line, and so does every overload of one operation. The declarations the generator adds itself carry fixed text: the `borrow` factory, the host-object roots, the `JsValue` members, and an owning factory whose constructor has no comment, which states the ownership the factory takes.

---

## Browser JS Glue

### Two Generated Artifacts

A `wasm32-web` build can involve **two** separate generated JavaScript files. They are
independent entry points, not two halves of one loader — each fetches and instantiates
the `.wasm` itself, so a page loads exactly one of them.

| Artifact | Generated by | Contains |
|---|---|---|
| `<name>.js` — bootstrap loader | every `promise build --target wasm32-web` | instantiation + the base PAL imports |
| `<module>.js` — bindgen glue | `promise bind webidl -o <dir>` | instantiation + the base PAL imports + reference table, string marshalling, the WebIDL-derived imports, and the host half of `web.fetch` ([web-apps.md](web-apps.md) §"Host resolved operations") |

```bash
promise build --target wasm32-web app.pr
# Output:
#   app.wasm      — WASM binary
#   app.js        — bootstrap loader (auto-generated)
```

Load the bootstrap for a program with no WebIDL bindings. Load the bindgen glue for a
program that has them — it is a superset of the bootstrap. Loading both would
instantiate the module twice.

The two stay separate on purpose: each is the whole loader for one kind of program, so
neither needs the other. What they share — the base PAL imports, the exit unwind and
`_ExitSignal`, and the line buffering, all below — is generated for both from one
source, so the two cannot differ.

### Base PAL Imports

Every `wasm32-web` binary imports exactly four functions from `promise_env`, whether
or not it uses WebIDL bindings. A host that does not supply all four fails to
instantiate with a `LinkError` before any user code runs.

| Import | Signature | Behavior |
|---|---|---|
| `write(fd, ptr, len)` | `(i32, i32, i64) -> i64` | fd 2 routes to `console.error`, any other fd to `console.log` |
| `exit(code)` | `(i32) -> void` | throws to unwind — see below |
| `monotonic_nanos()` | `() -> i64` | monotonic clock in nanoseconds |
| `schedule_pump(kind, delay_ms)` | `(i32, i32) -> void` | calls `promise_web_pump` again later, by the mechanism `kind` names — [web-apps.md](web-apps.md) §"Budget and continuation" |

Every `wasm32-web` binary exports the reactor's `promise_web_pump`
([web-apps.md](web-apps.md) §"Layer 1 the exported surface"), whether or not it
ever becomes a reactor, and the pump calls `schedule_pump` when its budget runs out
with work remaining. The guest picks the
continuation's kind because it, not the host, knows why it stopped pumping.

`len` and `write`'s return value are `i64`, so they cross the boundary as **BigInt**.

Two behaviors are easy to get wrong when writing a host by hand:

- **`exit` must throw.** Throwing is the only way to abort a synchronous WASM call from
  a JS import. A program with no live registration ends `_initialize` in `pal_exit(0)`,
  so its successful run unwinds through `exit`. A reactor's `_initialize` returns
  normally — `init()` resolves with the instance alive — and a later
  `promise_web_pump` may unwind through `exit` instead. The generated code throws an
  `_ExitSignal` carrying the exit code; `init()` and every pump the loader makes
  swallow code `0` and rethrow anything else, so a panic or an explicit non-zero exit
  still surfaces to the caller rather than passing silently. After any exit the
  loader makes no further call into the instance. Both artifacts export `_ExitSignal`: a caller
  identifies an exit with `e instanceof _ExitSignal`, then reads the code from
  `e.code`. `.code` alone does not identify an exit — a `DOMException` a host import
  throws has a numeric `.code` too.
- **`write` output must be line-buffered.** `print_line` appends the line separator
  before the write reaches the PAL, and `console.log` adds its own newline — logging
  each chunk verbatim would double every line, and a `print()` without a newline would
  become its own console line. The generated code buffers per fd, flushes only complete
  lines, and flushes the remainder at exit.

### Glue Architecture

Beyond the base imports above, the bindgen glue provides:

1. **Reference table** — manages opaque JS object references (DOM elements, etc.)
2. **Memory helpers** — string/array marshalling between WASM linear memory and JS
3. **Import implementations** — one JS function per WebIDL-derived `promise_env.*` import
4. **The host half of `web.fetch`** — the hand-written imports [web-apps.md](web-apps.md) §"Host resolved operations" specifies, which no IDL describes

```javascript
// bindgen glue (auto-generated, simplified — base PAL imports omitted)

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
    const ptr = instance.exports.cabi_realloc(0, 0, 1, bytes.length);
    new Uint8Array(instance.exports.memory.buffer, ptr, bytes.length).set(bytes);
    return [ptr, bytes.length];
}

const imports = {
    promise_env: {
        // write / exit / monotonic_nanos / schedule_pump also live here — see Base PAL Imports.
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
        element_get_inner_html(ref, retptr) {
            const [ptr, len] = writeString(instance, refLoad(ref).innerHTML);
            const view = new DataView(instance.exports.memory.buffer);
            view.setInt32(retptr, ptr, true);
            view.setInt32(retptr + 4, len, true);
        },
        "[resource-drop]element"(ref) {
            refRelease(ref);
        },
        // ... more imports generated from WebIDL
    }
};

let instance;

export async function init(wasmPath) {
    const response = await fetch(wasmPath);
    const { instance: inst } = await WebAssembly.instantiateStreaming(
        response, imports
    );
    instance = inst;
    try {
        inst.exports._initialize();
    } catch (e) {
        if (!(e instanceof _ExitSignal) || e.code !== 0) throw e;
    }
    return inst.exports;
}
```

### Usage

Both artifacts export `init` as a **named** export, but their signatures differ: the
bootstrap already knows its `.wasm` path and takes an optional `importObject`, while the
bindgen glue takes the `.wasm` URL.

The bootstrap adds the `promise_env` entries of that `importObject` to the base imports,
and an entry of the same name replaces the base one. That is how a page supplies a
function a hand-declared `wasm_import` names (`examples/11_wasm/web_console.pr`). It is
not a way to combine the two artifacts: the bindgen glue exports no import object, and a
program with bindings loads the glue instead of the bootstrap. Each loader holds one
instance, so a page calls `init` once.

```html
<script type="module">
    // Bootstrap loader (promise build --target wasm32-web).
    import { init } from './app.js';
    await init();
</script>
```

```html
<script type="module">
    // Bindgen glue (promise bind webidl).
    import { init } from './web.js';
    await init('./app.wasm');
</script>
```

Both resolve to the module's exports on success and reject if the program exits
non-zero.

### Reference Table and Ownership

JS objects (DOM elements, fetch responses, etc.) cannot live in WASM linear memory. The glue layer maintains a reference table: an array indexed by `i32` handles. Handle 0 refers to no object: it is how an absent value of an interface type crosses, in both directions. A `T?` parameter passes 0 for `none`, which the glue reads as `undefined` — to WebIDL a missing optional argument, and `null` for a nullable type — and a `null` or `undefined` result comes back as 0, which the wrapper lifts as `none`.

In the Promise binding module, each Web API type wraps a `_handle` into that
table — the same field a WIT resource wraps, since one generator emits both:

```promise
type Element `public `target(web) {
    i32 _handle;
    bool _owned;

    get tag_name string `public {
        return _web_element_tag_name(this._handle);
    }

    set_attribute(this, string name, string value) `public {
        _web_element_set_attribute(this._handle, name, value);
    }

    drop(~this) {
        if this._owned {
            _web_ref_release(this._handle);
        }
    }
}
```

When an owning `~Element` is dropped in Promise, `drop(~this)` calls `_web_ref_release`, which calls `refRelease(id)` in the JS glue. This integrates with Promise's ownership model — the ownership checker prevents use-after-drop of JS references, and the destructor ensures no JS reference leaks.

#### Owned and Borrowed Wrappers

A handle is released exactly once, by the one wrapper that owns it. Ownership is
per wrapper, not per type, and is carried in the `_owned` field:

- **Owned.** Every wrapper the bindings build around a handle the host has just
  minted for the program — `R.create(...)`, the factory a `constructor` generates,
  and any method, static method, or getter returning a resource — is owning
  (`_owned: true`). Its drop releases the handle.
- **Borrowed.** `R.borrow(handle)` wraps a handle that somebody else owns. Its drop
  releases nothing, so the owner (or the host) stays responsible for the handle.
  This is the only safe way to name a handle the program did not receive ownership
  of: an owning wrapper over such a handle releases it a second time, leaving the
  real owner pointing at an empty slot.

**Seeded ambient handles.** The glue seeds three fixed slots at module load,
before any import can run — `1` = `globalThis`, `2` = `document`, `3` =
`console` — unconditionally and in that order, so the numbers mean the same thing
in every host (a host without a DOM leaves slot 2 empty rather than shifting
`console` into it). These references belong to the host and no Promise wrapper
owns them. A program reaches them through the module's **roots**, module-level
getters the generator emits beside the types:

- `document` (`web.document`), emitted when the bound WebIDL defines `Document`;
- `console` (`web.console`), emitted when it defines the `console` namespace.

`globalThis` has no root. Each root returns a borrowed wrapper over its seeded
slot, built with the type's `borrow` factory, so dropping one never releases the
host object, and a program never writes a handle number
([web-apps.md](web-apps.md) §"Reaching the DOM"):

```promise
doc := web.document;            // borrowed: dropping it releases nothing
```

The seed list and the roots are generated from one definition of which slot holds
which object, so the two cannot disagree on which slot is `document`.

The glue pins these slots: `_refRelease` ignores any handle within the seeded
range, a bound derived from the seed list rather than written out separately, so
a fourth ambient reference is pinned by adding it. Releasing an ambient reference
is always a bug, and an unpinned one would not merely blank the slot — the freed
slot would be handed to the next `_refStore`, silently aliasing `document` to an
unrelated object.

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

### Phase 1 Foundation

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

### Phase 2 WIT Parser and Code Generator

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

### Phase 3 WebIDL Parser and Browser Target

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

### Phase 4 Component Model

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
