# Calling C Libraries from Promise — Design Proposal

**Status:** Draft / RFC — architecture under discussion
**Scope:** Linux, macOS, Windows (WASM excluded)
**Amends:** [`extern`](../annotations.md#13-foreign-interfaces) and [`raw`](../annotations.md#raw)
(widens what each already permits); does not introduce a new annotation.

---

## Table of Contents

1. [Motivation & Scope](#1-motivation--scope)
2. [Building on `extern` and `raw`, Not Around Them](#2-building-on-extern-and-raw-not-around-them)
3. [Design Tenets Applied to FFI](#3-design-tenets-applied-to-ffi)
4. [New Surface — Overview](#4-new-surface--overview)
5. [`` `raw `` Structs — the Foreign Struct Layout Grammar](#5-raw-structs--the-foreign-struct-layout-grammar)
6. [`RawPtr[T]` — an Ordinary Stdlib Pointer Type](#6-rawptrt--an-ordinary-stdlib-pointer-type)
7. [`` `extern `` — Widening the Symbol Registry](#7-extern--widening-the-symbol-registry)
8. [Why No New Safety Gate Is Needed](#8-why-no-new-safety-gate-is-needed)
9. [C-ABI Type Mapping](#9-c-abi-type-mapping)
10. [Calling Convention & Unsupported Shapes](#10-calling-convention--unsupported-shapes)
11. [Ownership & the RAII Handle Pattern](#11-ownership--the-raii-handle-pattern)
12. [Error-Handling Convention](#12-error-handling-convention)
13. [Linking Model — Static-First](#13-linking-model--static-first)
14. [Platform Specifics](#14-platform-specifics)
15. [The `promise bind c` Generator](#15-the-promise-bind-c-generator)
16. [Worked Example: Binding SQLite](#16-worked-example-binding-sqlite)
17. [Diagnostics & Compile-Time Checks](#17-diagnostics--compile-time-checks)
18. [Non-Goals / Future Work](#18-non-goals--future-work)
19. [Open Questions](#19-open-questions)

---

## 1. Motivation & Scope

Promise programs occasionally need functionality that only exists as a mature C library —
SQLite is the running example throughout this document, but the same shape covers zlib,
image codecs, and similar. Rewriting these in Promise is not realistic; linking the existing,
battle-tested C code is.

**Audience.** This is a **user-facing** feature, never a mechanism reserved for the compiler's
own internals. Any project may bind a C library. A catalog module may do the same thing —
"occasionally a library might be shipped that calls something like SQLite" is an explicit
target — but a catalog module uses *exactly* the same surface a project author would, not a
privileged internal one.

**Goals:**
- Call plain-C-ABI functions exported by a real C library from Promise code, on Linux, macOS,
  and Windows.
- Keep Promise's "zero system dependencies" static-binary story intact — linking a C library
  must not turn a Promise binary into something that only runs on machines with that library
  pre-installed (see [distribution.md](../distribution.md)).
- Make every foreign call, every field of a foreign struct, and every place a C-ABI type is
  chosen **visible in the source** — no implicit coercions, no hidden allocation.
- Do this **without adding new syntax** — no new keyword, no new pointer sigil, no new block
  form. §2 explains why that constraint is load-bearing here, not just a style preference.

**Non-goals (v1):** WASM targets (§14.4); C++ libraries; dynamically loading
system-installed libraries by default (§13.4 covers the opt-in escape hatch); C function
pointers/callbacks *into* Promise (§18); variadic C functions; unions; bitfields; passing or
returning a foreign struct **by value** across the boundary (§10) — only by pointer. These are
deliberate cuts to ship a sound, useful subset first; each is revisited in §18.

## 2. Building on `extern` and `raw`, Not Around Them

[`annotations.md` §16](../annotations.md#16-not-annotations) — the ratified, binding list of
names considered and rejected — already addresses this exact territory:

> `` `unsafe `` | Nothing checked it and nothing was unlocked by it. The `unsafe { }` block and
> the raw pointer type `T*` went with it: a raw pointer is only ever needed by an implementation
> the compiler emits, and `` `native `` already names those. FFI uses `` `extern `` for the call
> and `` `raw `` for foreign struct layout.

Two things follow from taking that seriously, rather than re-proposing `` `unsafe ``/`T*` under
a different name:

**First, this proposal reuses `` `extern `` and `` `raw `` as the call and struct-layout
mechanisms — not new metas.** [`` `extern ``](../annotations.md#13-foreign-interfaces) already
declares "a function whose body is not in Promise and whose linkage is by symbol name," and
already documents that it "is how the standard library and catalog modules call into the
platform layer the backend emits." [`` `raw ``](../annotations.md#raw) already exists "for
native types and foreign structs whose shape is dictated from outside." Neither needs a new
sibling annotation for this document's purpose; §7 and §5 give both a fully worked-out grammar,
which is currently the actual gap — `` `raw ``'s contract is stated but its field-type grammar
is not fully specified anywhere yet (`IsRaw()` is plumbed through sema today but has no codegen
consumer — an **unbuilt** gap in the `annotations.md` sense, §7 of [normative.md](../normative.md)).

**Second, the rejection's own reasoning — "a raw pointer is only ever needed by an
implementation the compiler emits, and `` `native `` already names those" — is exactly the
justification this proposal needs, once the words are read literally.** A pointer usable for
FFI is genuinely only needed by an implementation the compiler emits: `RawPtr[T]` (§6) is a
generic type whose handful of pointer operations (`read`, `write`, `offset`, `cast`) are
`` `native `` methods, implemented once by the compiler, exactly the way `int`'s `+`/`-`/`==`
are `` `native `` methods implemented once by the compiler (`CLAUDE.md`: "primitives are defined
as regular `type` declarations annotated with `` `native ``... not compiler magic"). `RawPtr[T]`
is spelled as an ordinary generic type application — `RawPtr[u8]`, exactly like `Vector[T]` or
`Range[T]` — so **no new pointer syntax is introduced**; `T*` is still rejected, permanently.
What changes is only that this one `` `native `` type, like every other one in the stdlib, is
now reachable by any project's own `.pr` source, the same way `Vector[T]` already is — not that
a new capability class is invented. See §8 for the full argument that no `` `unsafe `` gate is
needed on top of this: calling a `` `native `` method already carries the "the compiler vouches
for this" contract [`annotations.md` §5](../annotations.md#5-assertions-and-denials) assigns to
every `` `native `` declaration, and `RawPtr[T]`'s methods are ordinary members of that set.

**What is a genuine amendment, not just an elaboration.** `` `extern ``'s "Interactions" bullet
currently reads as scoped to compiler-emitted platform-layer symbols only, backed by a
compiler-hardcoded closed set: *"The compiler emits the platform layer itself, so it knows the
complete set of symbols an `` `extern `` may name."* §7 proposes widening the **source** of that
closed set — not its closed-set-validated-at-declaration property, which is preserved exactly —
to include symbols a project's own `promise.toml` declares via `[native]` (§13), backed by C
source or archives the project itself vendors. `` `extern ``'s contract ("symbol name required,
validated at the declaration, not at the linker") is unchanged; only who may register a symbol
grows, from "the compiler team, in Go" to "the compiler team, in Go, **or** a project author, in
`promise.toml`."

## 3. Design Tenets Applied to FFI

- **Self-contained.** A project that binds a C library carries the library's source (or a
  pinned, checksummed artifact) *inside its own tree* — never "assumes `libsqlite3-dev` is
  installed." Reading the project is enough to know exactly what native code it links in.
- **Explicit over implicit.** Every crossing of the FFI boundary is spelled out: `` `extern ``
  on the declaration, `` `raw `` on any struct field that crosses, `RawPtr[T]` for any pointer.
  There is no automatic coercion from a Promise `string` to a C string — `CString.from(s)` makes
  the allocation visible.
- **One obvious way.** One meta for a foreign call (`` `extern ``, already established), one
  meta for foreign struct layout (`` `raw ``, already established), one pointer type
  (`RawPtr[T]`, an ordinary stdlib generic — no new syntax). One default linking path (vendored
  static source, §13.2); the escape hatches (prebuilt archives, dynamic loading) are clearly
  marked as escape hatches, not alternatives of equal standing.
- **Closed sets validated at the declaration, not the linker.** This is already the house style
  — `` `extern ``'s symbol set, the `` `target(cond) `` platform-identifier vocabulary, the
  annotation vocabulary itself all work this way (`annotations.md` §2, §13, §14). §7 and §9
  apply the identical discipline one place further: the *types* legal at an `` `extern ``
  boundary are a closed, checked set, for the same reason the symbols are.

## 4. New Surface — Overview

Nothing below is a new annotation. `RawPtr[T]` and `CString` are ordinary stdlib types (built
from annotations that already exist: `` `native ``, `` `copy ``); `[native]` is a new
`promise.toml` table (proposed as an addition to [module-system.md](../module-system.md)'s
schema, which is where a ratified version of this table would actually live — one fact, one
home).

| Construct | Kind | Purpose |
|---|---|---|
| `` `extern("symbol") `` | existing meta, widened source of valid symbols | Declares a function implemented by a linked C symbol (§7) |
| `` `raw `` | existing meta, newly given a concrete field-type grammar | A struct field laid out exactly as C would, for structs that cross the `` `extern `` boundary (§5) |
| `RawPtr[T]` | new stdlib generic type (`` `native `` `` `copy ``, no new syntax) | The only pointer type that may cross the `` `extern `` boundary (§6) |
| `CString` | new stdlib type | Owns a NUL-terminated buffer for passing Promise strings to C (§9) |
| `[native]` | new `promise.toml` table | Declares vendored C sources/archives to compile and link (§13) |
| `promise bind c` | new CLI subcommand | Generates `` `extern ``/`` `raw `` declarations from a C header (§15) |

## 5. `` `raw `` Structs — the Foreign Struct Layout Grammar

A struct that crosses the `` `extern `` boundary is built entirely from `` `raw `` fields:

```promise
type CTimeval `native {
  i64 seconds `raw;
  i64 microseconds `raw;
}
```

`` `raw `` already means, per `annotations.md`: *"The field's type is an LLVM type identifier
used directly, bypassing Promise's type layout."* What has not been specified anywhere yet is
**which Promise type references are legal to write in that position** — that is this proposal's
concrete addition, and it deliberately does **not** introduce a literal-LLVM-syntax sub-grammar
into Promise source (that would cut against "no macros... code generation is not programmable,"
`language-design.md` §1). Instead, a `` `raw `` field's type is an ordinary Promise type
reference — parsed exactly the way `sema/decl.go`'s `defineField` already parses any field type,
`` `raw `` or not — drawn from the same **closed C-ABI type set** §9 defines for `` `extern ``
signatures: a fixed-width primitive, another `` `raw ``-only struct type, or `RawPtr[U]`. This
is one closed set serving both positions (field type and parameter/return type), stated once in
§9 and cross-referenced here rather than restated.

Rules, enforced in sema (extending the existing `IsRaw()` plumbing in `types/field.go` with an
actual codegen consumer — currently absent, per §2):

1. **Every field must be `` `raw ``**, and every `` `raw `` field's type must be a C-ABI type
   (§9). A `` `raw `` type may not mix ordinary (wrapped, four-struct-model) fields with `` `raw ``
   ones — a struct crossing the boundary is either entirely native-layout or entirely ordinary
   Promise; the two representations do not interleave.
2. **The declaring type must be `` `native ``**, matching `` `raw ``'s documented pairing
   ("Used with `` `native `` types and `` `extern `` boundaries") and the same soundness
   argument `annotations.md` §5 gives every other `` `native `` assertion: a layout guarantee
   the compiler cannot derive from ordinary Promise fields is sound exactly when the compiler is
   the one emitting the implementation. This also settles the inheritance question for free —
   `` `native `` types already carry no vtable requirement beyond what the compiler itself
   assigns them, so a `` `raw `` type is automatically excluded from ordinary `is`/`` `open ``
   inheritance without a separate rule.
3. **`` `raw `` implies `` `copy ``**, exactly like any other type whose fields are all `Copy` —
   this is the general `` `copy `` derivation rule, not a special case.
4. Padding follows the platform's C ABI (System V AMD64 on Linux and macOS, Microsoft x64 on
   Windows) — fields are laid out with their natural alignment and trailing padding exactly as
   `clang` would lay out the equivalent C struct. The existing-but-rejected `` `packed ``/
   `` `align(N) `` annotations (`annotations.md` §16: "alignment is the layout model's to
   decide... a per-type override is an ABI-visible promise the language does not make") stay
   rejected for ordinary Promise types; a `` `raw `` type is exactly the documented exception —
   its whole purpose is making an ABI-visible promise dictated from outside — so non-default
   packing for a `` `raw `` type is a `` `native ``-only capability, provided the same way
   `` `raw `` field placement itself is, rather than by reviving the general-purpose annotation.
5. `` `native `` methods on the type are permitted (dispatched statically — there is no vtable),
   which is how a `` `raw `` type still gets a `drop()` (§11).

## 6. `RawPtr[T]` — an Ordinary Stdlib Pointer Type

```promise
type RawPtr[T] `native `copy {
  get is_null bool `native;                     // address == 0
  get null Self `native `global;                 // the null pointer, RawPtr[T].null

  read(this) T `native;                          // load a T from the pointed-to address
  write(this, T value) `native;                   // store a T at the pointed-to address
  offset(this, int n) Self `native;               // pointer arithmetic: address + n * sizeof(T)
  cast[U](this) RawPtr[U] `native;                // reinterpret as a pointer to a different type
  address(this) uint `native;                     // the raw integer address
}

addr_of[T](T~ value) RawPtr[T] `native;            // address of a mutable local/field
addr_of_const[T](T value) RawPtr[T] `native;       // address of a read-only borrow
```

This is deliberately shaped like `int`/`Vector[T]`/`Range[T]` already are — a generic `type`
declaration, `` `native ``, `` `copy ``, whose operations are `` `native `` methods. §2 makes the
case for why this does not reopen the rejected `T*`/`` `unsafe `` question; the rest of this
section is the type's actual contract.

- **`T` must be a C-ABI type** (§9) — a fixed-width primitive, another `RawPtr[U]`, or a
  `` `raw `` struct type. `RawPtr[string]` or `RawPtr[Vector[int]]` are rejected; there is no
  defined layout for what they would point at.
- **`.read()`/`.write()`/`.offset()` assume `T`'s C layout and do no bounds checking** — the
  same contract a raw pointer has in every systems language, and, candidly, the one place in
  Promise where a runtime check genuinely cannot exist: array indexing has a length to check
  against (and does, with a panic on violation); a `RawPtr[T]` obtained from a C library call
  carries no such bound for the compiler to check. This is not a gap in Promise's design — it is
  the actual, irreducible shape of calling into code the compiler never analyzed, and this
  document's job is to make that boundary small and visible (§11), not to pretend it away.
- **`RawPtr[T]` does not distinguish `const T*` from `T*`** (both map to `RawPtr[T]`). Noted as
  a known simplification in §18/§19 — it costs nothing at the ABI level (both are one
  pointer-sized word) and only loses a documentation-level const-correctness signal.
- `addr_of`/`addr_of_const` take a **borrow**, not a value — the borrow checker still verifies
  the referent is a real, live binding at the call site, the same rule that already governs
  `T~`/`T` parameters ([language-design.md §6.2](../language-design.md#62-borrowing-and-moving)).
  What is **not** checked past that point is how long the resulting `RawPtr[T]` is used — that is
  the caller's responsibility, exactly as for Rust's `&mut x as *mut T`, which this function pair
  mirrors on purpose.
- `RawPtr[T]` is `` `copy `` — assigning or passing one never moves anything; it is a number with
  a type tag.

## 7. `` `extern `` — Widening the Symbol Registry

The declaration form itself is unchanged from today:

```promise
sqlite3_open(RawPtr[u8] filename, RawPtr[RawPtr[Void]]~ out_db) i32 `extern("sqlite3_open");
sqlite3_close(RawPtr[Void] db) i32 `extern("sqlite3_close");
sqlite3_errmsg(RawPtr[Void] db) RawPtr[u8] `extern("sqlite3_errmsg");
```

What §2 proposes to amend is only *where the compiler's registry of valid symbols draws from*.
Today, per `annotations.md` §13, that registry is "the complete set of symbols" the compiler's
own emitted platform layer provides — compiler-hardcoded. This proposal adds a second
contributor: for a given compilation, the closed set is the compiler-provided platform layer
**union** every symbol a project's own `[native]` sources/archives (§13) actually export,
verified against the vendored object/archive's real symbol table at build time (`nm`-equivalent)
— so an `` `extern `` naming a symbol that does not exist is still, as `annotations.md` §13
requires, "reported at the declaration, not at the linker."

Everything else about `` `extern `` — required symbol (never defaulted), one-declaration-one-
symbol, `sema/decl.go` + `codegen/extern.go` as the read sites — is unchanged:

1. Every parameter and the return type must be a **C-ABI type** (§9): a fixed-width primitive,
   `RawPtr[T]`, or (rare, §10) a fixed-size array of one behind a pointer. A `` `raw `` struct
   itself (not behind a `RawPtr`) may **not** appear directly — see §10 for why structs only
   cross by pointer.
2. `Void` (new builtin marker type, used only as `RawPtr[Void]`'s type argument) represents an
   opaque C pointer whose pointee type Promise does not model — the idiomatic type for library
   handles like `sqlite3*`.
3. An `` `extern `` function is not itself `!` (failable) — the C function's own status-code or
   errno convention is whatever the library defines, unmodified. §12 covers wrapping that into
   idiomatic Promise error handling one layer up.
4. Every `` `extern `` declaration backed by a `[native]` symbol (as opposed to the compiler's
   own platform layer) must resolve against that project's `[native]` table or the build fails
   at the declaration (§17) — the same "closed set, checked at declaration" property `` `extern ``
   already has, just with a wider set of valid registries.

## 8. Why No New Safety Gate Is Needed

`RawPtr[T]`'s `read`/`write`/`offset`/`cast` and `addr_of`/`addr_of_const`, and an `` `extern ``
call, are all `` `native ``. Calling a `` `native `` declaration already means, per
`annotations.md` §5's Assertion row, "trust me, this holds — sound because the implementation
being vouched for is the one the compiler itself emits." That is precisely the contract in
force here: the compiler emits the load/store/GEP for `RawPtr[T].read()` correctly, and the
correct call instruction for an `` `extern `` symbol — the same way it correctly emits `Vector.[]`'s
bounds-checked index or `int.+`'s overflow behavior. What a `` `native `` declaration's soundness
never claimed, for `RawPtr[T]` or for any other native method, is that the *caller* used it
correctly — `Vector.[]` closes that gap itself with a runtime bounds check; `RawPtr[T]` cannot,
for the reason given in §6. Wrapping the same operations in an additional `` `unsafe ``-labeled
block would not check anything a plain `` `native `` call site doesn't already carry as an
unstated contract — it would only be a second spelling of "this is `` `native ``," which
`annotations.md` §16 already rejected once (`` `instance ``'s rejection makes the identical
argument: "an annotation for it would be a second way to write nothing").

What this means in practice, and why §11's pattern matters: **the trust boundary is the type
that declares the `` `native `` calls, not a syntactic block around them.** A project wrapping
SQLite writes exactly one type (`Sqlite`, §16) whose methods are the only place `RawPtr[Void]`
and `` `extern `` calls appear; everything built on top of `Sqlite` is ordinary, fully
borrow-checked, failable Promise, with no marker needed because there is nothing left to mark —
the unsafety is contained by which functions call the `` `native `` operations, which is exactly
as visible in the source as any other function boundary.

## 9. C-ABI Type Mapping

The single closed set referenced by §5 (`` `raw `` field types), §6 (`RawPtr[T]`'s type
argument), and §7 (`` `extern `` parameter/return types):

| Promise type | C type | Notes |
|---|---|---|
| `i8` / `u8` | `int8_t` / `uint8_t` | |
| `i16` / `u16` | `int16_t` / `uint16_t` | |
| `i32` / `u32` | `int32_t` / `uint32_t` | matches C's plain `int`/`unsigned int` on all three target platforms |
| `i64` / `u64` | `int64_t` / `uint64_t` | **not** C's `long` — see warning below |
| `f32` / `f64` | `float` / `double` | |
| `bool` | *(library-specific — see below)* | |
| `RawPtr[T]` | `T*` (or `const T*`, §6) | |
| a `` `raw `` struct type, **only behind** `RawPtr[T]` | `struct T` (only via pointer, §10) | |
| a fixed-size array `T[N]` of a C-ABI `T`, as a `` `raw `` field | `T arr[N]` | inline, not a pointer — only legal as a `` `raw `` struct field; never as a bare `` `extern `` parameter, see §10 |

**Never use `int`/`uint`/`float` (Promise's default-width aliases) in an `` `extern ``
signature or a `` `raw `` field.** Always spell out the width (`i32`, `i64`, `u64`, ...). C's own
`int`/`long`/`size_t` are *not* portable across the three target platforms (`long` is 64-bit on
Linux/macOS but 32-bit on 64-bit Windows — the LP64-vs-LLP64 split), so a binding must commit to
the exact width the C header declares, per platform if the header itself varies.

**`bool`.** C has no universal boolean convention — even C99's `stdbool.h` `bool` is not
guaranteed to be the width a given library's compiled object actually uses, since many C APIs
predate it and use a plain `int` for true/false. There is **no default mapping** — pick `i32`
(a C `int` convention) or `u8` (a `_Bool`/`stdbool` convention) by reading the library's own
header. `promise bind c` (§15) reads the header's declared type and gets this right
automatically.

**Strings.** Promise's `string` (length-prefixed, UTF-8, embeds `\0` freely — see the string
layout notes in `CLAUDE.md`) and C's NUL-terminated `char*` are fundamentally different
representations; there is no automatic coercion. A new stdlib type provides the conversion:

```promise
type CString `native {
  drop(~this) `native;                     // frees the owned NUL-terminated buffer

  from!(string s) CString `native `factory;  // copies s's bytes + a trailing \0;
                                              // raises CStringError if s contains an embedded \0
  ptr(this) RawPtr[u8] `native;              // valid only while this CString is alive
}

from_c_string!(RawPtr[u8] ptr) string `native;              // scans for the NUL
from_c_string_n(RawPtr[u8] ptr, int len) string `native;    // explicit length, preferred when the library gives one
```

`CString.ptr()` carries no runtime check that the caller keeps the `CString` binding alive
across the entire `` `extern `` call that uses the pointer — the same footgun every C-string-
owning FFI layer in every language has, made visible by living on a type whose only job is that
one pointer (§8's "trust boundary is the type" pattern), rather than hidden behind an implicit
conversion.

**Arrays and slices.** `Vector[T]`/`T[]` store their elements **inline** in the Instance struct
via a C99 flexible array member (see the Vector Literals section of `CLAUDE.md`) — the data is
already contiguous, so no copy is needed to expose it to C:

```promise
raw_data[T](Vector[T] v) RawPtr[T] `native;  // pointer to element 0; v.len gives the count
```

The returned pointer is valid only until the vector is next mutated (push/pop/resize may
reallocate, per the copy-on-write behavior in `CLAUDE.md`) — same "keep the owner alive, don't
outlive it" contract as `CString.ptr()`. There is no mutable variant in v1 (a C function that
needs to write into a Promise-owned buffer should be handed a `` `raw ``-backed buffer via
`RawPtr[u8]`/`addr_of`, not a live `Vector[T]`).

## 10. Calling Convention & Unsupported Shapes

**Calling convention.** The platform C calling convention only — System V AMD64 on Linux and
macOS, the Microsoft x64 convention on Windows. Both platforms' 64-bit ABIs use a single calling
convention (no `stdcall`/`cdecl` ambiguity the way 32-bit x86 had); Promise does not target
32-bit Windows.

**Foreign structs cross only by pointer, never by value.** This is the single biggest scope cut
in this document, and it is deliberate: correctly passing or returning a struct *by value*
requires replicating the target C compiler's ABI **classification** rules (System V AMD64
classifies each eightbyte as INTEGER/SSE/MEMORY and may pass small structs in registers; the
Microsoft x64 ABI passes anything over 8 bytes by an invisible reference to a caller-made copy;
AAPCS64 has yet another set of rules for HFAs). Getting this wrong doesn't error at compile time
— it silently corrupts data at the call boundary. Restricting v1 to "structs cross only via
`RawPtr[T]`" sidesteps ABI classification entirely (a pointer is always exactly one
register/stack slot, on every target, with no ambiguity), at the cost of requiring C APIs that
pass small PODs by value to be wrapped with an out-parameter shim. In practice this matches how
most real C **library** APIs are already shaped — opaque handles and pointer out-parameters
(`sqlite3_open`, `deflateInit`, `curl_easy_setopt`) dominate. By-value struct crossing is left as
future work (§18) once per-target ABI classification exists in codegen.

**Fixed-size arrays** (`T[N]`) may appear as a `` `raw `` struct field (laid out inline, exactly
like a C array field) but may **not** appear as a bare `` `extern `` parameter type — a C
function taking a raw array parameter is, at the ABI level, taking a pointer (arrays decay to
pointers in C function signatures), so the correct Promise-side signature is `RawPtr[T]`, not
`T[N]`.

**Unsupported in v1** (all tracked in §18):
- **Variadic C functions** (`printf`-style). LLVM's variadic call ABI needs per-call-site
  argument classification matching the platform convention; deferred rather than getting it
  subtly wrong.
- **Unions.** No Promise-side representation is defined for "these fields overlap in memory."
- **Bitfields.** Layout is compiler- and platform-defined in C itself (not even fully portable
  *between* C compilers); no attempt is made to replicate it.
- **C function pointers/callbacks into Promise** (a C library calling back into Promise code —
  e.g. `sqlite3_exec`'s callback argument, `qsort`'s comparator). This needs a way to expose a
  Promise closure as a plain C-ABI function pointer, structurally the same "new direction of
  call" problem already identified for WASM/WebIDL callback support. Out of scope here; see §18.

## 11. Ownership & the RAII Handle Pattern

The flagship idiom this document is built around: wrap a C handle in an ordinary Promise type
and let `drop()` call the library's destructor. This maps Promise's existing ownership model
onto a C resource with no new mechanism:

```promise
type Sqlite {
  RawPtr[Void] db;

  drop(~this) {
    sqlite3_close(this.db);
  }
}
```

- **Unique ownership (`~`) maps to "this Promise value uniquely owns this C handle."** Moving a
  `Sqlite` moves who is responsible for closing it, exactly like moving any other droppable
  Promise type — the move checker already prevents the double-close bug for free
  ([language-design.md §6.1](../language-design.md#61-core-rules), rule 5).
- **A shared borrow (`db: Sqlite`) or mutable borrow (`db: Sqlite~`) means "use the handle
  without taking ownership,"** exactly as for any other type.
- Every `` `extern `` call that operates on the handle stays inside the wrapping type's methods
  — callers of `Sqlite` never see `RawPtr[Void]` at all. This is the intended shape for any FFI
  wrapper: the `` `native ``-call surface is contained inside one small type (§8); everything
  built on top is ordinary, safe Promise.

## 12. Error-Handling Convention

An `` `extern `` function is never `!` — its failure signaling is whatever the C library
defines, unmodified. The idiomatic pattern is a thin, safe, `!` wrapper one layer up:

```promise
type SqliteError `is error {
  string message;
}

exec!(Sqlite~ h, string sql) {
  cs := CString.from!(move sql);
  rc := sqlite3_exec(h.db, cs.ptr(), RawPtr[Void].null, RawPtr[Void].null, RawPtr[RawPtr[u8]].null);
  if rc != 0 {
    msg := from_c_string!(sqlite3_errmsg(h.db)) ?! "sqlite3 error {rc}";
    raise SqliteError(message: move msg);
  }
}
```

This keeps the raw-status-code translation as thin as possible; every other caller in the
project uses ordinary `?`/`?^`/`?!` failable-call syntax, and the C library's own error
convention never leaks past the wrapper.

## 13. Linking Model — Static-First

Consistent with the rest of Promise's distribution story (musl-static on Linux, no dynamic
dependency on a system `libssl.so` for TLS — see [distribution.md](../distribution.md)), linking
a C library into a Promise binary defaults to **static**, and the library's code is **vendored
into the project itself** — never assumed to be pre-installed on the build or target machine.

### 13.1 `promise.toml` — the `[native]` table

Proposed as an addition to [module-system.md](../module-system.md)'s manifest schema:

```toml
[native.sqlite3]
sources = ["native/sqlite3.c"]     # compiled by the same toolchain that already builds Promise itself
headers = ["native/sqlite3.h"]     # informational: input to `promise bind c` (§15); not required at runtime

# optional, only when a platform needs library-specific extra link flags
[native.sqlite3.link]
macos = []
linux = []
windows = []
```

- **`sources`** are plain `.c` files, checked into the project tree (`native/` is a convention,
  not a requirement). They are compiled with the same `clang`/LLVM toolchain the Promise
  compiler already requires for its own build (`bin/build` already shells out to `opt`/`llc`/
  `lld`/clang-fallback per `CLAUDE.md`) — **no new external dependency is introduced** by this
  feature.
- **`headers`** are not compiled or linked; they exist only so `promise bind c` (§15) has
  something to read, and so a human can find the source of truth for a hand-written binding.
- Each `[native.<name>]` table is content-hash cached exactly like everything else in the build
  pipeline (module IR, per-instance `.bc` — see the caching sections of `CLAUDE.md`): the `.o`
  for a vendored source file is only rebuilt when that file's content or the target changes.
- Every function the vendored object/archive exports becomes a candidate for `` `extern `` to
  name (§7) — the compiler reads the object's real symbol table to build that project's widened
  registry, the same way it already knows its own compiler-emitted platform layer's symbol set.
- The resulting object files link the same way a runtime `.o` does today — into the musl-static
  binary on Linux via `--gc-sections`, into the macOS binary, and via `lld-link` on Windows.

### 13.2 Path A — Vendored C Source (preferred)

For any library that is a reasonably small, portable C codebase (the leading example: SQLite
ships as a single ~250K-line **amalgamation** file, `sqlite3.c`, specifically designed to be
dropped into another project's build), vendor the `.c`/`.h` files directly and let `[native]`
compile them. This is the **recommended default** because:

- It requires zero new infrastructure beyond "invoke the C compiler already in the build" — no
  prebuilt-binary cache, no per-platform blob hosting, no `sha256` pinning table to maintain.
- The vendored source is fully inspectable in the project's own git history — consistent with
  "self-contained by default."
- It cross-compiles for free: the same clang invocation that already targets Linux/macOS/Windows
  for Promise's own runtime picks up the vendored `.c` file identically.

### 13.3 Path B — Vendored Prebuilt Static Archives (escape hatch)

For a library whose own build is too large or too complex to compile at every consumer's build
time (autotools/CMake-heavy, code-generation steps, or simply too slow to recompile per project)
— OpenSSL is the internal precedent (`tools/build/prebuilts.toml`, "OpenSSL" in
[distribution.md](../distribution.md)) — the fallback is a **per-platform pinned static archive**
checked into the project (or hosted at a pinned URL with a `sha256`, mirroring the existing
prebuilts schema):

```toml
[native.libfoo]
[native.libfoo.targets.linux-amd64]
archive = "native/libfoo/linux-amd64/libfoo.a"
sha256 = "..."
[native.libfoo.targets.macos-arm64]
archive = "native/libfoo/macos-arm64/libfoo.a"
sha256 = "..."
[native.libfoo.targets.windows-amd64]
archive = "native/libfoo/windows-amd64/libfoo.lib"
sha256 = "..."
```

This is explicitly **not** the recommended default: it requires a human (or that project's own
CI) to produce the per-platform archives ahead of time, the way the compiler team's release CI
produces the OpenSSL prebuilts today — ordinary project authors don't have that infrastructure
for free the way the compiler repo does.

### 13.4 Non-Goal for v1 — Dynamic Loading

`dlopen`/`dlsym` (POSIX) and `LoadLibrary`/`GetProcAddress` (Windows), resolving against a
library already installed on the machine running the binary, is **not** part of this document's
default path — it breaks the "zero system dependencies" static-binary guarantee (see the musl
static-`dlopen` limitation already called out in [runtime-architecture.md](../runtime-architecture.md)),
and it cannot be verified at build time, contradicting the "checked at the declaration" property
§7 otherwise preserves for every `` `extern ``. Left as a documented **future** opt-in for the
narrow cases that genuinely need it (a vendor driver library that cannot be statically vendored)
— not designed further here; see §19.

### 13.5 Catalog Modules and `[native]`

Catalog modules are otherwise self-contained by construction — a catalog module's `promise.toml`
may not have a `[require]` section (no dependency on remote/local Promise modules; see
[module-system.md](../module-system.md) §"Self-contained"). `[native]` does **not** violate that
closed-world rule: a vendored `.c`/`.h` file (Path A) or a checked-in archive (Path B) is source
shipped *inside* the module's own tree, not an external dependency the catalog CI would need to
fetch or version separately — as self-contained as the module's own `.pr` files. The catalog CI
validation described in module-system.md's self-containment section (which rejects `[require]`
sections) would need to permit `[native]` sections while still verifying every archive's
`sha256` and every `sources` entry is actually checked into the module's own repo.

## 14. Platform Specifics

**Linux.** Vendored sources compile to `.o` and link into the musl-static binary exactly like
any other runtime object. A vendored library's own transitive libc usage (`malloc`, `memcpy`,
etc.) resolves against the same static musl libc Promise already links.

**macOS.** Static archives/objects link the same way. A library that needs an Apple framework
(rare for a portable C library) is supported via the `[native.<name>.link.macos]` extra-flags
list (e.g. `["-framework", "CoreFoundation"]`).

**Windows.** Vendored `.c` sources compile with the same clang toolchain already used for the
`opt`/`llc`/`lld-link` pipeline. `` `raw `` struct layout follows the Microsoft x64 ABI's
alignment rules, which agree with System V AMD64 for the plain-scalar-field structs this
document supports (§10 — no by-value struct passing, so the by-value classification differences
between the two ABIs never come into play; only field layout/padding matters, and that already
agrees for POD structs of primitives and pointers).

**WASM (`wasm32-wasi`, `wasm32-web`) — unsupported.** There is no notion of linking a native
`.a`/`.o` compiled from arbitrary C source into a WASM module the way this document describes
(WASM's own C-interop story is the existing WIT/WebIDL/JS-FFI path in
[wasm-bindings.md](../wasm-bindings.md), a different mechanism for a different host
environment). An `` `extern `` declaration backed by `[native]`, or a `[native]` section itself,
is rejected at build time for a WASM target with a clear diagnostic — the same "raises
`unsupported`" shape the `tls` module already uses for WASM. Library authors who need a WASM
story pair a `[native]`-based implementation with a `` `target(cond: "!wasm") ``/
`` `target(cond: "wasm") `` split, the pattern already used throughout the stdlib for
platform-specific backends.

## 15. The `promise bind c` Generator

Mirrors the existing `promise bind wit`/`promise bind webidl` subcommands
(`compiler/cmd/promise/bind.go`):

```bash
promise bind c native/sqlite3.h -o modules/sqlite/native/
```

- Parses a (deliberately bounded) subset of C: function prototypes, `struct`/`typedef`
  declarations, `enum`s, and simple integer `#define` constants.
- **Emits exactly the hand-writable forms defined in this document** — `` `extern ``
  declarations and `` `native `` `` `raw `` struct types, in the same shape a human following
  §5–§9 would write by hand. This document's hand-written grammar is the ground truth; the
  generator targets it, it does not define a separate one.
- **Explicitly out of scope for the generator** (falls back to skipping the declaration with a
  loud comment and a nonzero exit code, never a silent 0): function-like macros, variadic
  prototypes, unions, bitfields, C++ headers, complex preprocessor conditionals it cannot
  resolve. A generator that exits 0 on input it could not fully lower is worse than one that
  fails loudly, because the failure only surfaces later, at `promise build`, far from its actual
  cause — so `promise bind c` must fail its own exit code whenever it skips something the input
  header referenced elsewhere.
- Like WIT/WebIDL bindgen output, the generated `.pr` file is meant to be checked into the
  project or module repository and reviewed, not regenerated on every build.
- Ambiguities the generator cannot resolve from the header alone — most importantly `bool`'s
  actual width (§9) when a library predates `stdbool.h` — are read directly from the types the
  header itself declares, rather than guessed.

## 16. Worked Example: Binding SQLite

Layout:

```
myproject/
  promise.toml
  native/
    sqlite3.c        # vendored amalgamation source
    sqlite3.h         # vendored header (input to `promise bind c`, and for humans)
  main.pr
```

`promise.toml`:

```toml
[module]
name = "myproject"
epoch = "2026.0"

[native.sqlite3]
sources = ["native/sqlite3.c"]
headers = ["native/sqlite3.h"]
```

Low-level bindings (`sqlite_ffi.pr`, never imported directly by application code):

```promise
sqlite3_open(RawPtr[u8] filename, RawPtr[RawPtr[Void]]~ out_db) i32 `extern("sqlite3_open");
sqlite3_close(RawPtr[Void] db) i32 `extern("sqlite3_close");
sqlite3_exec(
  RawPtr[Void] db,
  RawPtr[u8] sql,
  RawPtr[Void] callback,      // unsupported callback param, always null in v1 — see §10
  RawPtr[Void] callback_arg,
  RawPtr[RawPtr[u8]]~ errmsg
) i32 `extern("sqlite3_exec");
sqlite3_errmsg(RawPtr[Void] db) RawPtr[u8] `extern("sqlite3_errmsg");
```

Safe wrapper (`sqlite.pr`, what application code actually imports):

```promise
type SqliteError `is error {
  string message;
}

type Sqlite {
  RawPtr[Void] db;

  open!(string path) Sqlite `factory {
    cpath := CString.from!(move path);
    slot := RawPtr[RawPtr[Void]].null;
    rc := sqlite3_open(cpath.ptr(), addr_of(slot));
    if rc != 0 { raise SqliteError(message: "sqlite3_open failed: {rc}"); }
    return Sqlite(db: slot.read());
  }

  exec!(~this, string sql) {
    cs := CString.from!(move sql);
    rc := sqlite3_exec(this.db, cs.ptr(), RawPtr[Void].null, RawPtr[Void].null, RawPtr[RawPtr[u8]].null);
    if rc != 0 {
      msg := from_c_string!(sqlite3_errmsg(this.db)) ?! "sqlite3 error {rc}";
      raise SqliteError(message: move msg);
    }
  }

  drop(~this) {
    sqlite3_close(this.db);
  }
}
```

Application code:

```promise
main!() {
  db := Sqlite.open!("app.db")!;
  db.exec!("CREATE TABLE IF NOT EXISTS notes (id INTEGER PRIMARY KEY, body TEXT)")!;
  // db.drop() runs automatically at scope exit — sqlite3_close, guaranteed once
}
```

Everything below `Sqlite` is ordinary, safe, borrow-checked, failable Promise — the
`` `native ``/`RawPtr`/`` `extern `` surface is exactly the handful of call sites inside
`Sqlite`'s own methods (§8, §11).

## 17. Diagnostics & Compile-Time Checks

Sema/build-time errors this proposal introduces (non-exhaustive, illustrative of the coverage
expected before shipping):

- An `` `extern `` parameter/return type that is not a C-ABI type (§9) — including a bare
  `` `raw `` struct not wrapped in `RawPtr[T]` (§10), and Promise's own `int`/`uint` (must use
  explicit-width types).
- A `` `raw `` field whose type is not a C-ABI type, or a `` `raw `` type declared without
  `` `native ``, or a `` `raw `` type mixing `` `raw `` and non-`` `raw `` fields (§5).
- A `RawPtr[T]` whose `T` is not a C-ABI type.
- An `` `extern `` declaration whose symbol resolves against neither the compiler's own platform
  layer nor any `[native]` table's exported symbols (§7) — reported at the declaration.
- A `[native]`/`` `extern `` combination reachable from a `wasm32-wasi`/`wasm32-web` build target
  without a `` `target(cond) `` split excluding it (§14).
- `promise bind c` exits non-zero (not a silent success) whenever it drops any declaration the
  input header's *used* surface actually needed (§15).

## 18. Non-Goals / Future Work

Each cut in §1/§10, with what would need to change to lift it:

- **By-value foreign struct passing/returns.** Needs per-target C ABI argument classification
  (System V AMD64 eightbyte classification, Microsoft x64, AAPCS64 HFA rules) implemented in
  codegen — real work, not a small addition. Until then, out-parameter (`RawPtr`) shims cover
  the same ground for the libraries this document targets.
- **C function pointers/callbacks into Promise.** Needs a way to expose a Promise closure as a
  plain C-ABI function pointer — structurally the same "new direction of call" problem already
  identified for WASM/WebIDL callback support, but for native targets.
- **Dynamic loading** (§13.4) as a first-class opt-in.
- **`const`-correctness on `RawPtr[T]`** (§6) — a `ConstRawPtr[T]`/`RawPtr[T]` split, purely
  documentation-level at the ABI (both are one word), but useful for catching accidental writes
  through a pointer a C API declared `const`.
- **Unions and bitfields.**
- **Variadic `` `extern `` functions.**

## 19. Open Questions

- Should `` `raw `` structs require the declaring type to also be otherwise "empty" of ordinary
  fields (as this document assumes, §5 rule 1), or should a hybrid type — some fields wrapped,
  some `` `raw `` — be legal? This document takes the stricter position (no mixing) because a
  hybrid struct has no single coherent C layout to hand to `promise bind c` or to a human reading
  the header side by side with the Promise side.
- Where does the FFI stdlib surface (`RawPtr[T]`, `CString`, `addr_of`, `from_c_string`, `Void`)
  live — `std` (auto-imported everywhere) or an explicit `use cffi;` catalog module? This
  document leans toward an explicit catalog module: this surface is exactly the kind of thing
  that should not be ambiently available in every file, consistent with `` `interior ``'s
  existing `` `native ``-only restriction rationale.
- Versioning/update story for a vendored `[native]` source or archive — does `promise package`
  tooling need a `promise native update` equivalent, or is "edit `promise.toml`, replace the
  vendored file, bump `sha256` by hand" sufficient given how infrequently a vendored copy should
  need to change?
- Should the widened `` `extern `` symbol registry (§7) require an explicit per-symbol
  allow-list in `[native]` (`symbols = ["sqlite3_open", "sqlite3_close", ...]`), rather than
  implicitly trusting every symbol the vendored object happens to export? An explicit list is
  more in the spirit of "closed set, checked at declaration," at the cost of one more line of
  manifest per bound function; this document does not take a position yet.
