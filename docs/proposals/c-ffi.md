# Calling C Libraries from Promise — Design Proposal

**Status:** Draft / RFC — architecture under discussion
**Scope:** Linux, macOS, Windows (WASM excluded)
**Proposed Amendments:** [annotations.md](../annotations.md) (`extern`, `raw`, and the
rationale for rejecting `unsafe`), [language-design.md](../language-design.md) (foreign
layout, §5.2.1's sole-dispatch-mechanism rule, and unchecked pointer capabilities),
[memory-model.md](../memory-model.md)
(foreign-memory obligations), [module-system.md](../module-system.md) (`[native]`),
[distribution.md](../distribution.md) (C toolchain payloads), and
[runtime-architecture.md](../runtime-architecture.md) (foreign-call handoff).
This draft does not change those binding specifications. Ratification must amend them
together; no new annotation, keyword, pointer sigil, or block syntax is proposed.

---

## Table of Contents

1. [Motivation & Scope](#1-motivation--scope)
2. [Building on `extern` and `raw`, Not Around Them](#2-building-on-extern-and-raw-not-around-them)
3. [Design Tenets Applied to FFI](#3-design-tenets-applied-to-ffi)
4. [New Surface — Overview](#4-new-surface--overview)
5. [`` `raw `` Structs — the Foreign Struct Layout Grammar](#5-raw-structs--the-foreign-struct-layout-grammar)
6. [Foreign Pointer Types (`cffi`)](#6-foreign-pointer-types-cffi)
7. [`` `extern `` — Widening the Symbol Registry](#7-extern--widening-the-symbol-registry)
8. [Safety Boundary and Unchecked Operations](#8-safety-boundary-and-unchecked-operations)
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

**Non-goals (v1):** WASM targets (§14); C++ libraries; dynamically loading
system-installed libraries (§13.4 describes a future opt-in); C function
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

This proposal retains `extern` for symbol linkage and `raw` for foreign layout, but
**does introduce a new unchecked capability**: user code can obtain and dereference foreign
pointers. Correctly emitting a load does not prove that its address is live, aligned, or
readable. The quoted rejection's rationale must therefore be amended, not treated as a
proof that this feature preserves memory safety automatically.

**`native` retains its existing restriction.** Only types and implementations already known
to the compiler may use it ([annotations.md](../annotations.md#native)). The pointer types
and conversion primitives are compiler-provided; user-defined foreign structs are not
`native`. Instead, an all-`raw` field declaration selects a layout the compiler derives
from the target C ABI (§5). It does not grant access to `interior` or other native-only
assertions, and users cannot provide arbitrary native method declarations.

**The actual amendments are explicit.** `extern` gains module-owned native dependencies
and declaration diagnostics backed by real object symbols (§7); `raw` gains a closed
field grammar and an unwrapped, compiler-derived representation (§5). The explicit
`cffi` module gains read-only and mutable pointers with caller obligations (§6, §8).
No new syntax is needed, but these are semantic changes requiring ratification of the
specifications listed above. Symbol existence and ABI compatibility are separate checks;
an object symbol table alone cannot establish the latter.

**Two ratified sentences conflict with §5 directly**, and ratification must restate both
rather than read past them. [annotations.md](../annotations.md#raw) records `raw` as
"Used with `` `native `` types and `` `extern `` boundaries", while §5 rule 2 makes foreign
layout available to an ordinary type that no user could annotate `native` in the first
place. [language-design.md](../language-design.md) §5.2.1 states that the vtable pointer
is the **sole** mechanism for field access and method dispatch, yet a foreign record has
no vtable by construction; the statically dispatched methods §5 rule 3 permits therefore
need the same kind of carve-out that receiver-less members (no slot, resolved on the type
name) and always-static pure-value-type dispatch already rely on. Neither conflict is
resolved by this draft's own wording — both are amendments the ratifying change must make
in the owning documents.

## 3. Design Tenets Applied to FFI

- **Self-contained.** A project that binds a C library carries the library's source (or a
  pinned, checksummed artifact) *inside its own tree* — never "assumes `libsqlite3-dev` is
  installed." Reading the project is enough to know exactly what native code it links in.
- **Explicit over implicit.** Every crossing of the FFI boundary is spelled out: `` `extern ``
  on the declaration, `` `raw `` on any struct field that crosses, and a `cffi` pointer type
  for any pointer. Unchecked memory operations have an `_unchecked` suffix (§8).
  There is no automatic coercion from a Promise `string` to a C string — `CString.from(s)` makes
  the allocation visible.
- **One obvious way.** One meta for a foreign call (`` `extern ``, already established), one
  meta for foreign struct layout (`` `raw ``, already established), and one pointer family
  (`ConstRawPtr[T]` for reads, `RawPtr[T]` for writes; ordinary generics). One default linking path (vendored
  static source, §13.2); the escape hatches (prebuilt archives, dynamic loading) are clearly
  marked as escape hatches, not alternatives of equal standing.
- **Closed sets validated at the declaration, not the linker.** This is already the house style
  — `` `extern ``'s symbol set, the `` `target(cond) `` platform-identifier vocabulary, the
  annotation vocabulary itself all work this way (`annotations.md` §2, §13, §14). §7 and §9
  apply the identical discipline one place further: the *types* legal at an `` `extern ``
  boundary are a closed, checked set, for the same reason the symbols are.

## 4. New Surface — Overview

Nothing below is a new annotation. The compiler-provided pointer and string conversion
types live in an explicitly imported catalog module, `cffi`, not auto-imported `std`.
`[native]` is a new
`promise.toml` table (proposed as an addition to [module-system.md](../module-system.md)'s
schema, which is where a ratified version of this table would actually live — one fact, one
home).

| Construct | Kind | Purpose |
|---|---|---|
| `` `extern("symbol") `` | existing meta, widened source of valid symbols | Declares a function implemented by a linked C symbol (§7) |
| `` `raw `` | existing meta, newly given a concrete field-type grammar | A struct field laid out exactly as C would, for structs that cross the `` `extern `` boundary (§5) |
| `ConstRawPtr[T]` / `RawPtr[T]` | new compiler-provided `cffi` generic types (`` `native `` `` `copy ``) | Read-only / mutable foreign object pointers (§6) |
| `CString` | new compiler-provided `cffi` type | Owns a NUL-terminated buffer for passing Promise strings to C (§9) |
| `CBool` | new compiler-provided `cffi` scalar | Models C `_Bool` storage and call ABI, with explicit Promise `bool` conversion (§9) |
| `Void` | compiler-provided `cffi` marker | Unsized opaque pointee; no values or dereference operations (§6) |
| `[native]` | new `promise.toml` table | Declares vendored C sources/archives to compile and link (§13) |
| `promise bind c` | new CLI subcommand | Generates `` `extern ``/`` `raw `` declarations from a C header (§15) |

## 5. `` `raw `` Structs — the Foreign Struct Layout Grammar

A struct that crosses the `` `extern `` boundary is built entirely from `` `raw `` fields:

```promise
type CTimeval {
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
reference, using the existing type-reference grammar. §9 defines one closed C-ABI mapping
with position-specific restrictions: fields may embed records and fixed arrays, while
function parameters/results may not. Scalars include `CBool`, and pointers distinguish
read-only from mutable access. Layout selection does not authorize arbitrary LLVM types.

Rules, enforced in sema and codegen using the `IsRaw()` field property:

1. **Every field must be `` `raw ``**, and every `` `raw `` field's type must be a C-ABI type
   (§9). A `` `raw `` type may not mix ordinary (wrapped, four-struct-model) fields with `` `raw ``
   ones — a struct crossing the boundary is either entirely native-layout or entirely ordinary
   Promise; the two representations do not interleave.
2. **The declaring type is not `native`.** At least one instance field, all marked `raw`,
  selects foreign layout: one inline record in declaration order, with no vtable, RTTI,
  heap-instance wrapper, or implicit allocation. Empty foreign records, recursive by-value
  fields, inheritance (`is`/`open`), and native-only assertions are rejected. Construction
  initializes every field; taking its address addresses the record, not a Promise wrapper.
3. **Foreign layout is not ownership.** V1 foreign records contain only the copyable C-ABI
  storage types in §9 and are copyable records with no `drop()`. A pointer field is borrowed
  data, never evidence of unique ownership. Resources belong in a separate ordinary,
  non-copyable Promise wrapper with `drop()` (§11). Ordinary statically dispatched methods
  with Promise bodies are permitted on foreign records, but user-written native methods
  and resource destructors are not.
4. **Construction follows the pure-value-type rules.** A foreign record keeps the implicit
  field constructor, which is what §9's output-buffer example uses, and every field must be
  given a value there (rule 2). An explicit `new` may replace it for validation or computed
  initialization, exactly as for any other type. A **failable** `new` is rejected, matching
  the same restriction on pure value types and for the same reason: a copyable record with
  no destructor has no partially-constructed state to unwind, so a raising constructor would
  promise cleanup that does not exist. Validation that can fail belongs in the ordinary
  Promise wrapper that owns the record, not in the record itself.
5. Size, field offsets, natural alignment, and trailing padding follow the exact target
  triple's C ABI (§10), checked against pinned Clang layout probes. This includes nested
  records and fixed arrays. `CTimeval` above illustrates two `i64` fields, not a portable
  declaration of every platform's `struct timeval`; bindgen resolves actual header types.
6. **Non-default packing/alignment is unsupported in v1.** Packed records, explicit alignment
  attributes, flexible array members, unions, and bitfields must be rejected by bindgen
  when required by the selected surface. No undocumented native-only packing mechanism
  exists. Use a C shim with supported scalar/pointer parameters instead.

## 6. Foreign Pointer Types (`cffi`)

These are compiler-provided types in an explicitly imported module, not ordinary stdlib
surface a user could declare: `native` is writable only on declarations already predeclared
in the universe (§2), and nothing here is auto-imported (§8). The compiler-provided `cffi`
API has two generic object-pointer types. These signatures
specify the API, not user-writable native declarations; its eventual public declarations
must carry `doc` annotations documenting the obligations below.

| Operation | `ConstRawPtr[T]` | `RawPtr[T]` |
|---|---|---|
| `get is_null bool` | yes | yes |
| `get null Self` (global) | yes | yes |
| `get address uint` | yes | yes |
| `read_unchecked() T` | yes | yes |
| `write_unchecked(T value)` | no | yes |
| `offset_unchecked(int offset) Self` | yes | yes |
| `cast_unchecked[U]()` | returns `ConstRawPtr[U]` | returns `RawPtr[U]` |
| `get as_const ConstRawPtr[T]` | unnecessary | yes |

Both types are `copy`, represented by one target-sized object pointer. Copying one neither
owns nor extends the lifetime of its pointee. Neither type is `sendable` or `sharable`:
copyability alone cannot make a C handle safe to use across goroutines.

- **Pointees:** fixed-width C scalars, either pointer type, and all-`raw` records (§9).
  `Void` is an unsized opaque marker accepted only as a pointer argument, including under
  nested pointers. It has no values; `read_unchecked`, `write_unchecked`, and
  `offset_unchecked` are unavailable for `Void`. Fixed arrays are record fields only in v1.
- **Constness:** `ConstRawPtr[T]` models `const T*`, not an immutable pointer variable.
  `RawPtr[T].as_const` explicitly weakens access. No reverse conversion or integer-to-pointer
  constructor exists in v1. Casts preserve the outer capability and obey the pointee
  legality matrix below; multi-level pointer conversions are invariant. Constness
  is shallow, as in C: reading a stored mutable pointer does not freeze its own pointee.
- **Addresses of Promise storage:** `addr_of_unchecked[T](T~ value) RawPtr[T]` and
  `addr_of_const_unchecked[T](T value) ConstRawPtr[T]` are compiler intrinsics, not addresses
  of by-value parameter copies. They require an addressable local, field, or fixed-array
  element with identical Promise-storage and C layout. An element such as `buffer.bytes[index]`
  must belong to an addressable fixed array, not a vector, slice, temporary, or user-defined
  index getter. **A bare local fixed array qualifies on its own**, with no enclosing record:
  `T[N]` of a C-ABI element type is already N contiguous elements at natural alignment, which
  is what the C array it maps to requires, so `u8[4] buffer` and an all-`raw` record holding
  `u8[4]` export identically. Wrapping a buffer in a record is a way to group it with related
  fields or give it an owner that outlives the call, never a precondition for addressing it.
  The element type still has to be a C-ABI type: `string[2]` is contiguous too, but its
  elements are Promise values with no C counterpart, so it is rejected by the layout rule
  above rather than by a separate one. Evaluate its base and index once and perform the ordinary bounds check before
  producing the pointer: `0 <= index < N`, including for address-taking. No address can be
  taken of a zero-length array element or of index `N`; a valid element pointer may be offset
  to one-past without dereferencing it. The mutable form borrows the containing storage
  mutably; the const form borrows it read-only. The pointer is to the element, not a copy or
  pointer-to-array, and is valid only while the entire owner remains alive and stable.
  Sema rejects temporaries and unsupported storage types. Codegen materializes stable storage
  (including coroutine-frame storage when needed); it must not retain stale SSA values
  across foreign writes. `CBool` (§9), not Promise `bool`, provides addressable C booleans.
- **Caller obligations:** the owner must remain alive and its address stable for every use,
  including any interval during which C retains the pointer. Reads require initialized,
  aligned, readable storage of the correct type; writes additionally require writable
  storage, exclusive access, and no conflicting Promise or C aliases. All accesses must
  obey the C effective-type and library contracts. Raw pointers do not extend compiler-
  tracked borrows; wrapper authors must uphold these obligations beyond the call site.
- **Arithmetic:** offsets must stay within the same allocation (or one past it), must not
  overflow the address space, and use the target's `sizeof(T)`. One-past and null pointers
  cannot be dereferenced. No bounds or lifetime guarantee follows from a cast or null check.

Violating an unchecked operation's preconditions can cause memory corruption, races, or
undefined behavior; runtime checks cannot prove these preconditions for arbitrary C memory.
Keeping separate read-only pointers prevents accidental writes through shared exports,
but does not make foreign memory access safe in general.

### Cast Legality

For `cast_unchecked[U]()` on a pointer with pointee `T`, the following matrix is exhaustive.
Type equality means semantic identity after resolving aliases, not equal size or layout.
A **pointer-free storage type** is a §9 scalar or an all-`raw` record whose fields, recursively
including fixed arrays and nested records, contain no `RawPtr` or `ConstRawPtr`. `Void` is
unsized and treated separately. Generic checks apply after substitution.

| Source `T` and destination `U` | Permitted? |
|---|---|
| `T` and `U` are the same type | Yes, including pointer-bearing types and `Void` |
| Both are pointer-free storage types | Yes, subject to the unchecked access obligations |
| One is `Void` and the other is pointer-free storage | Yes; erasure/restoration does not prove the allocation's actual type |
| Any other pair | No |

`RawPtr[T]` returns `RawPtr[U]`; `ConstRawPtr[T]` returns `ConstRawPtr[U]`. No cast changes
that outer capability. In particular, `RawPtr[ConstRawPtr[u8]]` cannot cast to
`RawPtr[RawPtr[u8]]`, to `RawPtr[Void]`, or to a byte pointer. `RawPtr[Void]` cannot restore
to a pointer-bearing record. A record containing const pointers cannot be reinterpreted as
one containing mutable pointers, even through `Void` or an intermediate byte view.

This deliberately limits generic allocation and memory-inspection APIs in v1. Pointer-bearing
storage must be obtained through correctly typed addresses or header-validated C functions;
use a typed C shim where the C API returns erased storage. A permitted scalar/record cast
still does not authorize incompatible effective-type access, invalid scalar representations,
or misalignment. These are caller obligations, not inferred from the legality matrix.
Constness checks constrain Promise's pointer operations, not arbitrary C: a foreign function
can still return a misqualified pointer or violate its own header contract (§8).

## 7. `` `extern `` — Widening the Symbol Registry

The annotation still names an explicit symbol; the Promise name marks the unchecked call:

```promise
use cffi as _;

sqlite3_open_unchecked(ConstRawPtr[u8] filename, RawPtr[RawPtr[Void]] out_db) i32 `extern("sqlite3_open");
sqlite3_close_unchecked(RawPtr[Void] db) i32 `extern("sqlite3_close");
sqlite3_errmsg_unchecked(RawPtr[Void] db) ConstRawPtr[u8] `extern("sqlite3_errmsg");
```

For a source module, the registry consists of the compiler-owned PAL symbols plus the
explicit `symbols` allow-lists of that module's active `[native]` entries (§13). Each listed
function must have an externally linkable definition in the corresponding object/archive;
undefined references, data symbols, and local symbols do not qualify. Missing definitions
are diagnosed at the declaration, including declarations that are never called. Importing
a Promise module does not expose its native registry to the importer.

**Existence is not type compatibility.** Each entry also supplies headers describing its
selected surface. The build uses pinned Clang with the same target and preprocessing
configuration as compilation to validate handwritten and generated declarations against
those headers. It checks arity, calling convention, scalar lowering, pointee constness, and
modeled record layout. The checked binding description includes the header/configuration
digest. For prebuilt archives, matching headers and build configuration remain an artifact
producer obligation: symbol tables cannot prove that a binary implements its declared ABI.

1. Parameters and results are fixed-width scalars, `CBool`, or either object-pointer type
  (§9). No bare records, arrays, Promise references (`T~`/`T&`), or ownership modifiers
  cross the ABI. Omitted result type means C `void`; `Void` itself has no runtime value.
2. A C out-parameter already expresses indirection: `sqlite3**` is
  `RawPtr[RawPtr[Void]]`, passed by value, not an additional Promise mutable reference.
  `RawPtr[Void]` erases opaque object identity, so wrappers must prevent mixing unrelated
  handles. Neither object-pointer type can stand in for a C function pointer.
3. An `extern` function is not `!` (failable). Status codes remain ordinary return values;
  wrappers translate them into Promise errors (§12).
4. Multiple declarations of one symbol must agree on its validated binding contract.
  Ambiguous providers, conflicting signatures, and collisions with compiler-reserved
  symbols fail before linking (§13.5). Unsupported header types require a supported C
  shim; neither handwritten bindings nor casts may bypass declaration validation.

## 8. Safety Boundary and Unchecked Operations

**Compiler implementation correctness is not caller memory safety.** `native` guarantees
that an implementation exists; it does not certify a foreign pointer's provenance, lifetime,
alignment, aliasing, or synchronization. User-supplied C executes outside Promise's memory
safety guarantees. This proposal preserves the no-new-syntax constraint, not a claim that
unchecked operations are equivalent to bounds-checked container operations.

The v1 boundary is explicit in existing source forms:

- Files using the low-level API must explicitly import `cffi`; it is never auto-imported.
- Memory accesses, pointer-producing borrows, and pointer reinterpretations use `_unchecked`
  names. User-bound `extern` declarations must also give the Promise function an
  `_unchecked` suffix, independent of the C symbol string, making foreign calls searchable
  at their call sites. This rule does not rename existing compiler-owned PAL bindings.
- A wrapper keeps raw declarations and handles private, imports only the required native
  symbols, and exposes ordinary Promise methods that validate sizes, translate failures,
  and uphold ownership. Public low-level APIs, when intentionally provided, retain the
  `_unchecked` names and document every caller precondition with `doc` annotations.

The compiler enforces the import, naming, visibility, constness, and C-ABI type rules. It
does **not** prove wrapper correctness or prevent a wrapper from hiding a bad foreign call
behind a safe-looking name. Such wrappers are trusted code and require review and tests.
Application code is memory-safe only conditional on these wrappers and the linked C code
upholding their contracts. Native code is not sandboxed by this API.

No `unsafe` block is proposed. Naming and explicit imports identify the audit surface but
are not an effect system or a lifetime proof. This limitation must be stated in the amended
language and memory specifications rather than justified by the old `native` rationale.

## 9. C-ABI Type Mapping

The closed mapping referenced by §5 (field storage), §6 (pointees), and §7 (function
parameters/results), subject to each position's restrictions:

| Promise type | C type | Notes |
|---|---|---|
| `i8` / `u8` | `int8_t` / `uint8_t` | |
| `i16` / `u16` | `int16_t` / `uint16_t` | |
| `i32` / `u32` | `int32_t` / `uint32_t` | matches C's plain `int`/`unsigned int` on all three target platforms |
| `i64` / `u64` | `int64_t` / `uint64_t` | **not** C's `long` — see warning below |
| `f32` / `f64` | `float` / `double` | |
| `CBool` | C `_Bool` / `stdbool.h` `bool` | Target-specific C storage and call lowering; not Promise `bool` |
| `RawPtr[T]` / `ConstRawPtr[T]` | `T*` / `const T*` | Object pointers only; constness applies at each pointer depth |
| an all-`` `raw `` record | `struct T` | Embedded record field or pointee only, never a bare function parameter/result (§10) |
| a fixed-size array `T[N]` of a C-ABI `T`, as a `` `raw `` field | `T arr[N]` | inline, not a pointer — only legal as a `` `raw `` struct field; never as a bare `` `extern `` parameter, see §10 |

**Never use `int`/`uint`/`float` (Promise's default-width aliases) in an `` `extern ``
signature or a `` `raw `` field.** Always spell out the width (`i32`, `i64`, `u64`, ...). C's own
`int`/`long`/`size_t` are *not* portable across the three target platforms (`long` is 64-bit on
Linux/macOS but 32-bit on 64-bit Windows — the LP64-vs-LLP64 split), so a binding must commit to
the exact width the C header declares, per platform if the header itself varies.

**Booleans.** Promise `bool` is rejected at the foreign boundary and in raw storage.
Compiler-provided, copyable `cffi.CBool` has a `from(bool value)` factory and a
`get value bool` getter; its size, alignment, valid values, and argument/result extension
follow Clang's `_Bool` ABI for the target. Conversion normalizes to zero or one. An API
using C `int` for truth values maps to `i32`, not `CBool`; a byte flag maps to `u8` only
when the header actually declares a byte. `_Bool` must not be lowered as an arbitrary
`u8` parameter just because both occupy a byte in a record.

**Other C spellings.** Bindgen resolves `long`, `size_t`, enums, and typedefs using the
selected target and header flags, then emits explicit widths. Unsupported scalar types
(such as `long double` or atomics) fail when selected. `char` fields and values follow
target signedness. Pointers to character buffers may use the explicit byte-view mapping
`RawPtr[u8]` / `ConstRawPtr[u8]` for `char*` / `const char*`, including SQLite's text API:
this preserves object-pointer ABI and byte access, not C character arithmetic semantics.

**Strings.** Promise's `string` (length-prefixed, UTF-8, embeds `\0` freely — see the string
layout notes in `CLAUDE.md`) and C's NUL-terminated `char*` are fundamentally different
representations; there is no automatic coercion. Compiler-provided `cffi.CString` owns a
NUL-terminated copy, has `drop(~this)`, and exposes this API:

| Operation | Contract |
|---|---|
| `from!(string value) CString` (factory) | Borrows and copies the bytes, appends NUL, raises `CStringError` for embedded NUL |
| `get ptr_unchecked ConstRawPtr[u8]` | Read-only pointer valid while the owner lives; no lifetime extension |
| `from_c_string_unchecked!(ConstRawPtr[u8] ptr) string` (module function) | Copies through the first NUL; raises `CStringError` for null or invalid UTF-8 |
| `from_c_string_n_unchecked!(ConstRawPtr[u8] ptr, int len) string` (module function) | Copies exactly `len` bytes; rejects negative length, null with nonzero length, or invalid UTF-8; null with zero length yields empty string |

Unbounded conversion requires a reachable terminator in readable storage. Bounded conversion
requires the entire byte range to be readable; embedded NULs are preserved. These failable
functions can detect encoding/input errors, not dangling pointers or inaccessible memory.
`CString.ptr_unchecked` cannot be made mutable, and `CString.from(path)` borrows `path`:
neither a move argument nor a call-site `!` is appropriate. A C-owned returned string is
copied, not freed; its library-specific release function remains the wrapper's responsibility.

**Arrays and slices.** `raw_data_unchecked[T](Vector[T] value) ConstRawPtr[T]` exposes
contiguous element storage without copying, but only when its Promise element size,
alignment, and stride are identical to the C representation. Eligibility is checked by
codegen, not inferred merely from membership in §9's boundary set. `Vector[bool]` is
ineligible; wrapped or otherwise differently represented elements are also rejected.

The owner must remain alive and unmodified while C uses the pointer. An empty vector is
**defined** to yield null with length zero. That is a chosen normalization rather than a
consequence of the layout, which could equally hand back a non-null address into an empty
allocation, so it binds every C API used this way to accept the null-and-zero pair.
Read-only access is essential:
vector literals may reside in read-only storage and writes would bypass copy-on-write.
There is no mutable vector export or const-removing cast in v1. For C output, use initialized
addressable scalars, a fixed array (bare local or `raw` record field, §6), or C-allocated
memory with its matching C release function. No generic foreign allocator is introduced here.

**Fixed output-buffer example.** Vendor this C function, declare the same prototype in its
header, and select `fill_bytes` through the owning native entry's `symbols` list (§13):

```c
#include <stdint.h>

void fill_bytes(uint8_t *dest, uint64_t capacity) {
    for (uint64_t index = 0; index < capacity; ++index) {
        dest[index] = (uint8_t)(index + 1);
    }
}
```

The Promise side passes the address of the first element of initialized fixed storage:

```promise
use cffi as _;

type OutputBuffer {
  u8[4] bytes `raw;
}

fill_bytes_unchecked(RawPtr[u8] dest, u64 capacity) `extern("fill_bytes");

main() {
  buffer := OutputBuffer(bytes: [0u8, 0u8, 0u8, 0u8]);
  fill_bytes_unchecked(addr_of_unchecked(buffer.bytes[0]), 4u64);
  assert(buffer.bytes[0] == 1u8);
  assert(buffer.bytes[3] == 4u8);
}
```

The record here groups the buffer with an owner that could carry other fields; it is not
required. A bare local is the shorter form of the same export and behaves identically:

```promise
main() {
  u8[4] buffer = [0u8; 4];
  fill_bytes_unchecked(addr_of_unchecked(buffer[0]), 4u64);
  assert(buffer[3] == 4u8);
}
```

The function writes only within the supplied capacity and retains no pointer. The owner
remains live and exclusively accessible for the call. `addr_of_unchecked(buffer.bytes)`
is rejected because it would require a pointer-to-array; the element form yields `RawPtr[u8]`.
For a zero-capacity call use `RawPtr[u8].null` only if the C function permits null with zero
capacity; do not attempt to form an element address in empty storage. This example is also
a required runtime fixture (§17), not a claim that the proposed API already compiles.

## 10. Calling Convention & Unsupported Shapes

**Calling convention.** Only the default C ABI for an explicitly supported target triple:

| Target | C ABI |
|---|---|
| `linux-amd64` | System V AMD64, musl |
| `linux-arm64` | AAPCS64, musl |
| `macos-amd64` | Darwin x86_64 |
| `macos-arm64` | Apple ARM64 ABI (including Darwin-specific rules) |
| `windows-amd64` | Microsoft x64, MSVC-compatible runtime |

Non-default conventions such as `vectorcall` are rejected. A target is supported only when
the toolchain/sysroot payload and ABI tests in §17 are available; the table is the intended
v1 coverage, not a claim about existing release assets. No 32-bit or Windows ARM64 target
is promised here.

**Scalar ABI lowering still matters.** Compiler declarations and call sites must match
Clang's target-specific IR lowering, including narrow signed/unsigned extension attributes
and `CBool` storage versus parameter/result representation. Typed header checks and C
round-trip probes cover these facts; LLVM `ccc` alone is not a complete ABI implementation.

**Foreign structs cross only by pointer, never by value.** This is the single biggest scope cut
in this document, and it is deliberate: correctly passing or returning a struct *by value*
requires replicating the target C compiler's ABI **classification** rules (System V AMD64
classifies each eightbyte as INTEGER/SSE/MEMORY and may pass small structs in registers; the
Microsoft x64 ABI passes anything over 8 bytes by an invisible reference to a caller-made copy;
AAPCS64 has yet another set of rules for HFAs). Getting this wrong doesn't error at compile time
— it silently corrupts data at the call boundary. Restricting v1 to "structs cross only via
either object-pointer type" avoids aggregate argument classification, but not scalar ABI
lowering, at the cost of requiring C APIs that pass small PODs by value to be wrapped with
an out-parameter shim. In practice this matches how
most real C **library** APIs are already shaped — opaque handles and pointer out-parameters
(`sqlite3_open`, `sqlite3_close`) are common. By-value struct crossing is left as
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
  Even an always-null callback argument must be supplied by a typed C shim, not modeled as
  `RawPtr[Void]`; object and function pointers are distinct contracts (§16).
- **Non-default record packing/alignment, flexible array members, and volatile/atomic
  storage accesses.** Use supported C accessor shims rather than inventing layout or
  synchronization rules on the Promise side.

## 11. Ownership & the RAII Handle Pattern

The flagship idiom this document is built around: wrap a C handle in an ordinary Promise type
and let `drop()` call the library's destructor. This maps Promise's existing ownership model
onto a C resource with no new mechanism:

```promise
use cffi as _;

type Sqlite `single_owner {
  RawPtr[Void] _db;

  drop(~this) {
    sqlite3_close_unchecked(this._db);
  }
}
```

This excerpt shows storage and destruction only; §16 supplies the explicit constructor
that replaces implicit field construction. It is not a complete public wrapper by itself.

- **Unique ownership applies to the wrapper, not to copied raw pointers.** Moving a `Sqlite`
  transfers its cleanup responsibility, but the wrapper author must prevent constructing
  two owners from the same handle, exposing the handle, or closing it early without clearing
  ownership. The move checker cannot infer these C-resource invariants.
- **Owning wrappers are `single_owner`.** A destructor alone does not prevent container
  operations from structurally duplicating a value. The existing `single_owner` capability
  prohibits `copy`, `clone`, slicing, and nested-container operations that would duplicate
  the handle, including through containing types. Raw pointers remain copyable borrowed
  values; the owning wrapper supplies the non-duplication guarantee.
- **Encapsulation includes construction.** An exported type's unprefixed members are public,
  so the handle is `_db`, not `db`, and no public getter exposes it. A failable `new(path: ...)`
  replaces the implicit field constructor: callers can acquire a database from a path, not
  adopt an arbitrary pointer. Code inside the wrapper's module is trusted; a separate
  importing module must be unable to read/write `_db` or construct an owner from a handle.
- **A shared borrow (`db: Sqlite`) or mutable borrow (`db: Sqlite~`) means "use the handle
  without taking ownership,"** exactly as for any other type.
- Every `` `extern `` call that operates on the handle stays inside the wrapping type's methods
  — callers of `Sqlite` never see `RawPtr[Void]` at all. This is the intended shape for any FFI
  wrapper: its private unchecked surface is small enough to audit (§8). Safety depends on
  that audit, not merely on enclosing the calls in a type.
- Acquisition failures must release partially initialized C resources. A destructor that
  can fail needs a policy: a failable explicit `close` where useful, plus a non-raising
  `drop` fallback whose cleanup guarantees are documented. SQLite's `sqlite3_close` can
  return `SQLITE_BUSY` if statements remain; §16 exposes no statement handles, and requires
  all statement work to finish before dropping the database.
- Allocation and deallocation must use the same allocator family. Promise's allocation
  accounting does not automatically observe C `malloc`; foreign-resource tests need C-side
  counters or suitable native instrumentation (§17).

## 12. Error-Handling Convention

An `` `extern `` function is never `!` — its failure signaling is whatever the C library
defines, unmodified. The idiomatic pattern is a thin, safe, `!` wrapper one layer up:

The complete SQLite example in §16 copies `sqlite3_errmsg` while the database handle is
live, then raises `SqliteError`. Calls inside a failable Promise function auto-propagate;
`?^` is explicit propagation, `? { ... }` handles an error, and `?!` panics. A trailing `!`
is not failable-call syntax. A failed string conversion uses `? { ... }` to select a fallback
message, not `?!`.

### Foreign-Call Execution and Thread-Local Errors

Every user-bound `extern` call is conservatively **potentially blocking**. When running on
the scheduler, codegen releases the P with `promise_sched_enter_syscall`, calls C on the
current OS thread, and reattaches with `promise_sched_exit_syscall` before Promise execution
continues. Calls outside scheduler execution use a defined no-op handoff. This behavior is
part of `extern`'s execution contract; no fast/nonblocking opt-out is introduced in v1.
The runtime must provide replacement workers when existing workers are in foreign calls.

**The handoff is paid per call, whatever the callee costs.** A conservative default is the
right one, because the compiler cannot see how long user-supplied C runs and a stalled P
blocks every goroutine queued behind it. The price is that a chatty API pays two scheduler
transitions around work that may return in nanoseconds: a row-scanning loop over an
accessor like `sqlite3_column_int` can spend more time entering and leaving the scheduler
than inside SQLite. V1 accepts that cost rather than offering an opt-out whose misuse
stalls a P silently, and the wrapper-side mitigation is to move such loops into a C shim
that returns a whole row per call. A declared nonblocking opt-out is future work (§18).

One foreign invocation stays on one OS thread, but successive invocations from a goroutine
are not guaranteed to use the same thread. Thread-affine sessions are unsupported in v1
unless a C shim owns their dedicated thread and handles synchronization entirely in C.
Raw pointers and the SQLite wrapper are not implicitly sendable/sharable. C must not call
Promise, unwind exceptions, or `longjmp` across a Promise frame. Process exit and indefinitely
blocking foreign code are not recoverable Promise errors; test process backstops still apply.

**Do not read `errno` or `GetLastError` in a subsequent Promise call.** Scheduler handoff or
runtime work could change thread-local state before that read. V1 requires a small C shim
for such APIs: invoke the operation, immediately copy its error state into a caller-provided
`i32` (`errno`) or `u32` (`GetLastError`) out-parameter, then return. Initialization of error
state, when required by the API, also happens inside that shim, after scheduler entry. The
captured value is meaningful only under the C API's documented failure conditions. This
keeps return values faithful to C without introducing an ambient Promise "last error".

## 13. Linking Model — Static-First

Consistent with the rest of Promise's distribution story (musl-static on Linux, no dynamic
dependency on a system `libssl.so` for TLS — see [distribution.md](../distribution.md)), linking
a C library into a Promise binary defaults to **static**, and the library's code is **vendored
into the project itself** — never assumed to be pre-installed on the build or target machine.

### 13.1 `promise.toml` — the `[native]` table

Proposed as an addition to [module-system.md](../module-system.md)'s manifest schema:

```toml
[native.sqlite3]
sources = ["native/sqlite3.c"]
headers = ["native/sqlite3.h"]
symbols = ["sqlite3_open", "sqlite3_close", "sqlite3_errmsg"]
enabled_targets = ["linux-amd64", "linux-arm64", "macos-amd64", "macos-arm64", "windows-amd64"]
c_standard = "c11"
include_dirs = ["native"]
defines = { SQLITE_THREADSAFE = "1", SQLITE_OMIT_LOAD_EXTENSION = "1" }

[native.sqlite3.link]
macos = []
linux = []
windows = []
```

- **`sources`** names checked-in C translation units. `headers` is required for the selected
  `symbols`: it supplies declaration validation as well as bindgen input, not merely human
  documentation. All non-sysroot includes must resolve inside the owning module tree.
  Paths are module-relative; escaping paths and symlinks are rejected.
- **`enabled_targets`** is an explicit nonempty list from §10's vocabulary. Filtering happens
  before native acquisition, compilation, or header validation. An inactive entry contributes
  no symbols or link inputs. Each enabled target must have exactly one source or archive
  provider; `sources` and target archives are mutually exclusive within an entry.
- **`symbols`** is an explicit list of function names to bind. Only these definitions enter
  the source module's registry. Headers and internal C dependencies may contain other symbols;
  the allow-list does not remove symbols needed to link the C code or hide native code from
  the process. Bindgen uses the same list as its requested surface (§15).
- **Compilation inputs** include `c_standard` (default `c11`), `include_dirs`, `defines`, and
  optional `compile_options` (an argument array, never a shell command). Clang compilation,
  header checking, and bindgen use the identical effective target/preprocessor configuration.
  Flags overriding managed targets, sysroots, compiler plugins, or unsupported ABI/layout
  features are rejected. Ambient `CPATH`, `CFLAGS`, and host SDK discovery do not affect builds.
- **`link`** contains per-OS argument arrays (`link.macos`, not a separate differently shaped
  table). Managed link validation rejects host library search paths, arbitrary runtime
  dependencies, and flags that defeat static linking on Linux. OS frameworks/import libraries
  must come from the declared target link surface, not an undeclared host installation.
- **`depends_on`**, when needed, lists other `[native.<name>]` entries in the same module.
  It makes C link dependencies explicit without exposing their symbols to Promise unless
  they are separately allow-listed. Unknown entries, cycles, and an active entry depending
  on an inactive one are errors. Dependencies are packaged with the owning module.

**Cache correctness.** The object key includes the source and complete transitive include
contents, header-search configuration (including changes that alter which header resolves),
defines, effective compiler arguments, build mode/optimization, target triple and ABI,
deployment target, compiler/resource-header/sysroot content hashes, and code-generation
options such as PIC. Dependency discovery must use the actual Clang preprocessor, not a
textual include scanner. Generated headers must already be vendored; no arbitrary build
scripts run. Time/path-dependent preprocessing must be normalized or rejected so identical
declared inputs yield identical outputs.

Header validation and generated-binding cache keys include the same configuration plus the
selected symbols and generator version. Final executable/test cache keys include all native
objects/archives, their ABI sidecars (§13.3), their order, link options, dependency graph,
and linker/runtime identities.
Changing only a header, define, sysroot, or archive must invalidate all affected downstream
artifacts, not just the C compilation cache.

### 13.2 Path A — Vendored C Source (preferred)

For any library that is a reasonably small, portable C codebase (the leading example: SQLite
ships as a single ~250K-line **amalgamation** file, `sqlite3.c`, specifically designed to be
dropped into another project's build), vendor the `.c`/`.h` files directly and let `[native]`
compile them. This is the **recommended default** because:

- The vendored source is fully inspectable in the project's own git history — consistent with
  "self-contained by default."
- The library author need not publish a separate archive per target when the code is portable
  C and all headers/configuration inputs are supplied. Platform-specific sources can use
  separate entries with disjoint `enabled_targets` lists.

**A C frontend is a new toolchain payload.** `opt`, `llc`, and `lld` do not parse C. A
developer machine that built Promise with Clang says nothing about what an installed Promise
binary can compile. Native-source builds and header-based bindgen acquire a pinned Clang
frontend, resource headers, and target C headers/sysroot through the content-addressed
distribution mechanism. They also require compatible CRTs, libc/compiler-runtime libraries,
and OS link surfaces. Existing link stubs alone do not supply C header definitions.

Each payload is checksummed and versioned with the supported target triple and minimum OS
deployment version. Thin installations fetch it on first use; full/all bundles and offline
pre-staging follow [distribution.md](../distribution.md)'s host/target rules. Missing offline
payloads produce an actionable error, never fallback to an arbitrary host Clang or SDK.
Redistribution rights for headers/runtime assets must be established before enabling a target.

Cross-compilation therefore requires an explicit host-tool/target-sysroot pair and the tests
in §17, not just a different compiler flag. Zero *user-installed* dependencies remains the
goal; this proposal adds managed dependencies and distribution work to meet it.

### 13.3 Path B — Vendored Prebuilt Static Archives (escape hatch)

For a library whose own build is too large or too complex to compile at every consumer's build
time (autotools/CMake-heavy, code-generation steps, or simply too slow to recompile per project)
— OpenSSL is the internal precedent (`tools/build/prebuilts.toml`, "OpenSSL" in
[distribution.md](../distribution.md)) — the fallback is a **per-platform pinned static archive**
checked into the module with matching headers and an ABI/build-configuration description.
Remote native-library URLs are deferred in v1; the managed compiler/sysroot acquisition
above is separate from third-party library acquisition:

```toml
[native.libfoo]
headers = ["native/libfoo/include/foo.h"]
symbols = ["foo_open", "foo_close"]
enabled_targets = ["linux-amd64", "macos-arm64", "windows-amd64"]
include_dirs = ["native/libfoo/include"]
[native.libfoo.targets.linux-amd64]
archive = "native/libfoo/linux-amd64/libfoo.a"
sha256 = "..."
abi = "native/libfoo/linux-amd64/abi.toml"
abi_sha256 = "..."
[native.libfoo.targets.macos-arm64]
archive = "native/libfoo/macos-arm64/libfoo.a"
sha256 = "..."
abi = "native/libfoo/macos-arm64/abi.toml"
abi_sha256 = "..."
[native.libfoo.targets.windows-amd64]
archive = "native/libfoo/windows-amd64/libfoo.lib"
sha256 = "..."
abi = "native/libfoo/windows-amd64/abi.toml"
abi_sha256 = "..."
```

This is explicitly **not** the recommended default: it requires a human (or that project's own
CI) to produce the per-platform archives ahead of time, the way the compiler team's release CI
produces the OpenSSL prebuilts today — ordinary project authors don't have that infrastructure
for free the way the compiler repo does.

The build validates archive checksums, object format, architecture, and declared ABI/runtime
compatibility before registry construction. V1 accepts native machine-code static archives,
not dynamic/import libraries masquerading as static libraries or arbitrary toolchain LTO
bitcode. Every non-platform dependency must be another declared, vendored native entry.
Linux archives must target the managed musl environment; Windows archives must match the
managed MSVC-compatible CRT contract. Header checks cannot detect a dishonest or mistaken
producer ABI declaration, so archive producers must run the cross-language probes in §17.

#### Required ABI Sidecar

Every archive target supplies `abi`, a module-relative path to a checked-in TOML sidecar,
and `abi_sha256`, the SHA-256 of that file's exact bytes. These are required, not optional
documentation. The sidecar binds itself to the archive and records the producer's effective
configuration. Example for the Linux archive above (hash placeholders must be replaced by
64 lowercase hexadecimal digits in a real manifest):

```toml
schema_version = 1
archive_sha256 = "..."
target_triple = "x86_64-unknown-linux-musl"
minimum_os = "linux:5.4"
runtime_profile = "promise-linux-amd64-musl"
runtime_sha256 = "..."
toolchain_profile = "promise-c-clang-22.1.0-r1"
toolchain_profile_sha256 = "..."
producer_host = "linux-amd64"
compiler_id = "clang-22.1.0"
compiler_sha256 = "..."
resource_headers_sha256 = "..."
sysroot_sha256 = "..."
headers_sha256 = "..."

[compile]
c_standard = "c11"
headers = ["native/libfoo/include/foo.h"]
include_dirs = ["native/libfoo/include"]
defines = {}
compile_options = []
```

All fields above are required in schema version 1; unknown fields or schema versions fail.
Their meaning and validation are:

| Field | Contract |
|---|---|
| `archive_sha256` | Must equal the target entry's `sha256` and the archive's actual byte digest; a sidecar cannot be reused for a different archive |
| `target_triple` | Canonical triple selected by the managed target profile; must agree with the active manifest target and every archive member's architecture/object format |
| `minimum_os` | `<os>:<version>` deployment floor, e.g. `linux:5.4`, `macos:11.0`, or `windows:10.0`; must use the target OS and must not exceed the consuming build's deployment floor |
| `runtime_profile` / `runtime_sha256` | Identifier and content digest of the managed runtime profile, including libc/CRT, compiler runtime, and linkage mode; both must match the consuming build |
| `toolchain_profile` / `toolchain_profile_sha256` | Host-independent managed C toolchain release/profile identity and its catalog-record content digest; both must match the consuming build |
| `producer_host` | Host OS/architecture on which the producer's C frontend ran; selects its compiler payload in the toolchain profile, not the archive's target |
| `compiler_id` / `compiler_sha256` | Producer frontend version identity and executable digest, recorded as provenance; must match the profile's entry for `producer_host`, not the consumer's executable |
| `resource_headers_sha256` / `sysroot_sha256` | Host-independent content digests for the selected compiler resource-header tree and target sysroot; must match the consuming profile, not host-specific packaging digests |
| `headers_sha256` | Digest of the resolved module-owned header dependency manifest, including all transitive non-sysroot includes; recomputed with Clang |
| `compile` | Fully expanded header-validation configuration, using module-relative paths and explicit defaults; must equal the native entry's effective `c_standard`, `headers`, `include_dirs`, `defines`, and `compile_options` |

`runtime_profile`, `toolchain_profile`, host identifiers, and canonical target triples come
from the managed toolchain catalog, not arbitrary strings interpreted by the linker.
Each immutable toolchain-profile record identifies one pinned frontend release/configuration
and maps supported build hosts to their permitted compiler version and executable digest,
with the targets each host payload supports. Its content digest covers the entire record.
The consumer verifies its own frontend against the entry for its own host, and verifies the
producer's recorded frontend against the entry for `producer_host`; both entries must support
the selected target. Missing entries or mismatched digests fail. The producer executable
need not be downloaded or run by the consumer. Matching provenance to the catalog does not
prove that the producer actually used that executable.

Thus one `linux-amd64` archive can be produced on Linux and consumed on macOS using distinct
host executables from the same toolchain profile. Neither executable-byte equality across
hosts nor equality of host packaging is an ABI requirement. Header-tree and target-sysroot
identities use canonical content manifests (the path/digest encoding below, relative to each
tree root); download archives and host installation prefixes are excluded from those identities.
Local build caches still include the actual executing compiler digest as required by §13.1;
archive acceptance and local cache reuse are separate decisions.

The catalog defines supported OS-version formats and deployment floors; comparisons are
component-wise numeric, never lexical. The producer must build against the declared floor,
not merely record the oldest OS on which someone happened to test it. The consuming profile
must support that floor.

The header dependency manifest is a UTF-8 JSON array of `[module-relative-path, sha256]`
pairs, sorted bytewise by slash-normalized path, serialized with no insignificant whitespace;
`headers_sha256` hashes those bytes. Each file digest hashes the actual header bytes.
Sysroot/resource headers are covered by their payload digests. Include order and flags
remain significant in `compile`; define tables compare by key/value, not TOML field order.
Producer options affecting exposed ABI (including enum size, character signedness, or
packing) must be recorded and applied consistently to the compiled library and its headers.
Unsupported layout/ABI options remain errors under §13.1, even if recorded in a sidecar.

V1 deliberately requires exact host-independent toolchain-profile and target-runtime
identities instead of maintaining an implicit compatibility whitelist across releases.
Changing build hosts within one profile does not invalidate an archive. Updating a required
profile identity requires rebuilding/revalidating the
archive and publishing its matching headers and sidecar. The sidecar is metadata, not a
build script: consumers do not execute producer commands, and undeclared native dependencies
cannot be supplied through it. Final link options and dependency ordering remain owned by
the native manifest (§13.1, §13.5).

**The cost of that choice is that an archive's usable life is bounded by the toolchain it
was built against.** Because the consuming build must match the producer's host-independent
toolchain profile, resource-header, target-sysroot, and runtime-profile digests exactly,
any compiler release that bumps one of them invalidates every Path B archive built against
the previous set. That is a hard failure at the sidecar check, not a warning and not a
degraded mode, and no consumer can clear it locally: only the producer can, by rebuilding,
re-running the §17 probes, and republishing headers and sidecar together. What does *not*
invalidate an archive is a consumer using a different supported host executable within the
same profile, which is the whole point of keeping those identities host-independent. The
exposure is worst precisely where this path gets chosen, since a library is vendored as a
prebuilt because building it is expensive or slow, and those are the producers least able
to turn a rebuild around quickly. A project that cannot accept that coupling should prefer
Path A, where the vendored source is recompiled against whatever toolchain the consumer's
compiler ships. §19 asks whether the resulting per-epoch churn is acceptable as specified.

**What the build can prove:** byte checksums; sidecar schema; agreement with the active
target/profile; parsed object architecture/format; reproducible header resolution and
configuration; and header-to-Promise ABI checks. When object formats record deployment
versions or runtime/link directives, contradictory values are rejected too.

**What remains a producer assertion:** that these bytes were actually compiled with the
declared flags, that unrecorded runtime assumptions are absent, that the archive honors
the supplied headers, and that it truly runs on its declared minimum OS. Object files do
not uniformly encode those facts; a checksummed sidecar is not an attestation. Producers
must run the ABI/runtime probes in §17 on matching target runners, including the minimum
supported OS. Missing evidence cannot be replaced by claiming symbol-table verification.

**What the catalog asserts:** that every host entry within one toolchain profile produces
ABI-identical output for a given target. Host-independent identities depend on this, and
neither the build nor the producer establishes it: both host executables verify against
their own entries, so a host payload that was patched, or built from a different source
revision, and still listed under the same profile would pass every check while diverging
in what it emits. Falsifying that claim is a catalog obligation, discharged by running the
§17 ABI probes for each host entry against every target it is listed as supporting, not
by the single cross-host fixture that exercises one library. A profile record is immutable
for this reason: correcting a host entry means publishing a new profile, which invalidates
archives built against the old one exactly as any other identity change does.

### 13.4 Non-Goal for v1 — Dynamic Loading

`dlopen`/`dlsym` (POSIX) and `LoadLibrary`/`GetProcAddress` (Windows), resolving against a
library already installed on the machine running the binary, is **not** part of this document's
default path — it breaks the "zero system dependencies" static-binary guarantee (see the musl
static-`dlopen` limitation already called out in [runtime-architecture.md](../runtime-architecture.md)),
and it cannot be verified at build time, contradicting the "checked at the declaration" property
§7 otherwise preserves for every `` `extern ``. Left as a documented **future** opt-in for the
narrow cases that genuinely need it (a vendor driver library that cannot be statically vendored).
No dynamic-loading manifest schema or runtime API is designed by this proposal.

### 13.5 Catalog Modules and `[native]`

Catalog modules are otherwise self-contained by construction — a catalog module's `promise.toml`
may not have a `[require]` section (no dependency on remote/local Promise modules; see
[module-system.md](../module-system.md) §"Self-contained"). `[native]` does **not** violate that
closed-world rule: a vendored `.c`/`.h` file (Path A) or a checked-in archive (Path B) is source
shipped *inside* the module's own tree, not an external dependency the catalog CI would need to
fetch or version separately — as self-contained as the module's own `.pr` files. The catalog CI
validation described in module-system.md's self-containment section (which rejects `[require]`
sections) would need to permit `[native]` sections while verifying every native source,
header, archive, and dependency is shipped in the module, with archive checksums verified.
Catalog policy may still reject a particular dependency for licensing or security reasons.

**Propagation and identity.** An active imported Promise module carries its active native
entries into the final link; the importing project need not copy their manifest entries.
Registry lookup remains local to the module declaring an `extern`. Resolve all target
filters before collecting this graph. Native entries are identified by resolved Promise
module identity, local entry name, and a digest of the complete effective inputs. Repeated
imports of the same resolved entry are deduplicated; identical local names in different
modules do not imply the same dependency or compatible versions.

**Symbols are process-global.** Different native versions are not silently unified. Distinct
providers defining the same externally visible strong symbol fail with both dependency
paths, even if just one was allow-listed. Header-compatible prototypes do not prove binary
interchangeability. PAL/runtime-reserved symbols cannot be overridden. Ordinary references
to managed libc/OS symbols are allowed; definitions interposing on them are not. Weak/COMDAT
coalescing follows the target linker only for identical ABI-compatible definitions; any
ambiguity about the provider of a selected FFI symbol is an error. Users resolve collisions
by choosing one dependency version or vendoring a symbol-prefixed build, not by link order.

**Link order is deterministic.** Direct objects precede static libraries; dependent entries
precede their dependencies. Stable ordering is derived from module identity and the declared
dependency graph. Linkers requiring archive rescans receive groups/repeated scans preserving
that graph's semantics; Windows and Darwin use their equivalent archive-resolution behavior.
Unresolved transitive symbols are build errors naming the owning entry. Native machine objects
link in both debug and release builds; cross-language LTO is not required in v1.

## 14. Platform Specifics

**Linux.** Vendored sources compile to `.o` and link into the musl-static binary exactly like
any other runtime object. A vendored library's own transitive libc usage (`malloc`, `memcpy`,
etc.) resolves against the same static musl libc Promise already links.

**macOS.** Use the distinct Darwin x86_64 and Apple ARM64 ABIs and their managed SDK/deployment
targets. Vendored libraries link statically; normal OS-provided `libSystem` and explicitly
declared frameworks remain system dependencies under Promise's macOS distribution contract.
For example, `link.macos = ["-framework", "CoreFoundation"]` is valid only if that framework
is available in the selected managed SDK/link surface.

**Windows.** C compilation needs the managed Clang frontend and MSVC-compatible C headers,
not only `opt`/`llc`/`lld-link` and generated `.lib` link stubs. Header typedefs and record
layout must be evaluated for Microsoft x64: a C `long` field is not interchangeable with
its LP64 counterpart on Linux/macOS. Foreign allocations must be returned to their matching
allocator; managed CRT compatibility does not authorize freeing them through Promise.

**WASM (`wasm32-wasi`, `wasm32-web`) is a scope exclusion, not a technical impossibility.**
Clang can compile C to WASM objects and archives and link them into WASM modules. Supporting
that requires separate target ABI, libc/host-import, memory, and toolchain contracts, which
this v1 does not define. WIT/WebIDL/JS bindings in [wasm-bindings.md](../wasm-bindings.md)
remain available but are not claimed to be the only possible C interoperability mechanism.

The mere presence of a `[native]` table must not reject a WASM build. `enabled_targets`
excludes its native entries before acquisition or compilation, and source authors pair
native declarations/imports with `target(cond: "!wasm")` and a WASM fallback with
`target(cond: "wasm")`. Target filtering precedes declaration validation. An `extern`
that survives filtering and requires an unavailable native symbol is a compile-time error,
even if never called; an excluded declaration is not checked. This is not a runtime
`unsupported` exception. A fallback needs no C toolchain or inactive native payload.

## 15. The `promise bind c` Generator

Mirrors the existing `promise bind wit`/`promise bind webidl` subcommands
(`compiler/cmd/promise/bind.go`):

```bash
promise bind c --native sqlite3 --target linux-amd64 -o sqlite_ffi.pr
```

- **Use the pinned Clang frontend, not a handwritten C parser.** `--native` selects the
  owning manifest entry; `--target` is required and must be enabled there. The entry supplies
  headers, includes, defines, language mode, and ABI flags. Clang preprocesses conditionals,
  expands macros, resolves typedefs, and reports record layouts and target scalar facts.
  Bindgen consumes structured AST/layout output rather than scraping C source text.
- **A bounded lowering surface, not a bounded parser.** Generate bindings for exactly the
  entry's `symbols` plus their necessary type dependencies. Unsupported unrelated APIs in
  SQLite or system headers do not fail generation. A selected callback, variadic function,
  by-value record, packed layout, union, bitfield, or other unsupported dependency fails with
  a source location and the selected-symbol dependency path. An opaque record pointer needs
  no record definition; it may lower to `RawPtr[Void]` without inspecting hidden fields.
- Emit hand-writable Promise declarations: `_unchecked` extern names, explicit constness,
  widths, `CBool`, and all-`raw` records without `native`. Enum values needed by these types
  are evaluated through Clang; optional `constants = ["SQLITE_OK"]` in the native entry
  selects integer macro/enum constants explicitly. Function-like macros are not callable
  bindings; use a typed C shim. No selected declaration is silently omitted.
- Generation is atomic: any selected-surface failure exits nonzero and leaves existing
  output intact. Unselected unsupported APIs may be summarized separately, not emitted as
  misleading commented-out bindings. Diagnostics distinguish parse errors from unsupported
  lowering and missing target configuration.
- Generated `.pr` files are reviewed and checked in, not regenerated on every build. They
  identify the target/configuration and toolchain/header digest. Build-time header checks
  apply equally to generated and handwritten bindings (§7), preventing stale checked-in
  declarations from silently acquiring a different ABI. For multiple targets, generate each
  target separately and use existing target conditions where mappings differ; bindgen must
  not infer universal portability from a single host-target run.

## 16. Worked Example: Binding SQLite

Layout:

```
myproject/
  promise.toml
  native/
    sqlite3.c
    sqlite3.h
    sqlite_shim.c
    sqlite_shim.h
  sqlite_ffi.pr
  sqlite.pr
  main.pr
```

`promise.toml`:

```toml
[module]
name = "myproject"
epoch = "2026.0"

[native.sqlite3]
sources = ["native/sqlite3.c", "native/sqlite_shim.c"]
headers = ["native/sqlite3.h", "native/sqlite_shim.h"]
symbols = ["sqlite3_open", "sqlite3_close", "sqlite3_errmsg", "sqlite_exec_no_callback"]
enabled_targets = ["linux-amd64", "linux-arm64", "macos-amd64", "macos-arm64", "windows-amd64"]
c_standard = "c11"
include_dirs = ["native"]
defines = { SQLITE_THREADSAFE = "1", SQLITE_OMIT_LOAD_EXTENSION = "1" }
```

`native/sqlite_shim.h` declares the supported callback-free surface:

```c
#include "sqlite3.h"

int sqlite_exec_no_callback(sqlite3 *database, const char *sql);
```

`native/sqlite_shim.c` supplies typed C null pointers to the actual callback API:

```c
#include "sqlite_shim.h"

int sqlite_exec_no_callback(sqlite3 *database, const char *sql) {
    return sqlite3_exec(database, sql, 0, 0, 0);
}
```

This shim does not reinterpret an object pointer as a function pointer. It requests no
allocated SQLite error-message buffer; the wrapper reads the database-owned diagnostic
instead. The same shim pattern handles unsupported signatures and immediate thread-local
error capture without extending the Promise ABI.

Low-level bindings (`sqlite_ffi.pr`, private declarations in the same module as the wrapper):

```promise
use cffi as _;

sqlite3_open_unchecked(ConstRawPtr[u8] filename, RawPtr[RawPtr[Void]] out_db) i32 `extern("sqlite3_open");
sqlite3_close_unchecked(RawPtr[Void] db) i32 `extern("sqlite3_close");
sqlite_exec_no_callback_unchecked(
  RawPtr[Void] db,
  ConstRawPtr[u8] sql
) i32 `extern("sqlite_exec_no_callback");
sqlite3_errmsg_unchecked(RawPtr[Void] db) ConstRawPtr[u8] `extern("sqlite3_errmsg");
```

Wrapper (`sqlite.pr`, used by application code in the same module):

```promise
use cffi as _;

type SqliteError is error {
  string message;
}

type Sqlite `single_owner {
  RawPtr[Void] _db;

  new!(~this, string path) {
    cpath := CString.from(path);
    slot := RawPtr[Void].null;
    rc := sqlite3_open_unchecked(cpath.ptr_unchecked, addr_of_unchecked(slot));
    if rc != 0 {
      message := "sqlite3_open failed: {rc}";
      if !slot.is_null {
        message = from_c_string_unchecked(sqlite3_errmsg_unchecked(slot)) ? {
          "sqlite3_open failed: {rc}";
        };
        sqlite3_close_unchecked(slot);
      }
      raise SqliteError(message: move message);
    }
    this._db = slot;
  }

  exec!(~this, string sql) {
    cs := CString.from(sql);
    rc := sqlite_exec_no_callback_unchecked(this._db, cs.ptr_unchecked);
    if rc != 0 {
      msg := from_c_string_unchecked(sqlite3_errmsg_unchecked(this._db)) ? {
        "sqlite3 error {rc}";
      };
      raise SqliteError(message: move msg);
    }
  }

  drop(~this) {
    sqlite3_close_unchecked(this._db);
  }
}
```

Application code:

```promise
main!() {
  db := Sqlite(path: "app.db");
  db.exec("CREATE TABLE IF NOT EXISTS notes (id INTEGER PRIMARY KEY, body TEXT)");
}
```

The slot holds a `sqlite3*`; taking its address passes `sqlite3**`. After the call the
updated slot itself is the handle, so there is no extra `read_unchecked()`. On open failure,
SQLite may still supply a handle: its diagnostic is copied before closing it and raising.
Only successful acquisition assigns `_db`; failure cleanup does not rely on a destructor
running for an incomplete instance. The explicit `new` suppresses the field-based constructor,
so `Sqlite(_db: handle)` is not an alternate acquisition path, even inside the module.
The mutable slot borrow ends with the call, and SQLite does not retain that slot address.
Likewise, neither open nor the callback-free exec retains the `CString` input.

`single_owner` prevents structural duplication of the wrapper, including via containers.
`drop` closes the handle once because the wrapper never duplicates ownership or exposes
statements, backups, or blob handles that could make close return `SQLITE_BUSY`. This is
a wrapper invariant to test, not a general property of SQLite close. Adding such APIs
requires child-resource lifetime rules and a revised close policy. The wrapper is neither
sendable nor sharable, and its mutating method prevents concurrent shared access.

These files are proposed integration fixtures, not claims that the unimplemented `cffi`
API compiles today. When packaged as a reusable module, only the wrapper API becomes
`public`, with `doc` annotations on the exported type, constructor, and methods; raw bindings
and `_db` remain private. The public constructor accepts only a path, never a raw handle.
Cross-module access tests are required; same-module examples do not establish encapsulation.
Application
safety depends on the audited invariants above (§8). The example must become executable
as part of implementing this proposal (§17).

## 17. Diagnostics & Compile-Time Checks

Sema/build-time errors this proposal introduces (non-exhaustive, illustrative of the coverage
expected before shipping):

- An `` `extern `` parameter/return type that is not a C-ABI type (§9) — including a bare
  `` `raw `` struct not wrapped in `RawPtr[T]` (§10), and Promise's own `int`/`uint` (must use
  explicit-width types).
- A `raw` field outside the storage set, a mixed-layout or empty foreign record, unsupported
  inheritance/packing/alignment, or a foreign record with `drop()` or a failable `new` (§5).
  Arbitrary user `native` declarations remain rejected under the existing annotation contract.
- Either pointer type with an invalid pointee, a write through a read-only pointer, any
  cast forbidden by §6's matrix, or dereferencing/arithmetic on `Void` (§6). Taking a mutable address
  of a temporary, or of a binding held only by a shared (read-only) borrow, is rejected, as
  is exporting storage whose Promise layout differs from its C counterpart (§6, §9).
  Address-taking of a fixed-array element retains ordinary bounds checking; known invalid
  indices fail statically where the existing array rules require, otherwise they panic
  before any pointer is formed. Vector elements and temporary/index-getter results are
  not an alternate mutable-buffer export.
- Missing explicit `cffi` imports or missing `_unchecked` suffixes on user-bound extern
  declarations (§8). Pointer escape/lifetime correctness is not claimed as a diagnostic.
- An `extern` symbol absent from the owning module's active allow-listed definitions,
  or a header mismatch in signature, constness, calling convention, or record layout (§7).
  Reject Promise reference layers, bare aggregates, and function-pointer placeholders.
- Missing/incorrect managed toolchain payloads, archive checksum/architecture/runtime
  mismatches, escaping include paths, unknown targets, ambiguous providers, dependency
  cycles, and conflicting native/PAL symbols (§13).
  Archive targets also reject missing or malformed ABI sidecars, checksum mismatches,
  stale header/configuration digests, incompatible deployment floors, and toolchain/runtime
  profile mismatches. These checks do not verify unencoded producer assertions (§13.3).
- A `[native]`/`` `extern `` combination reachable from a `wasm32-wasi`/`wasm32-web` build target
  without a `` `target(cond) `` split excluding it (§14).
- Bindgen fails atomically with a dependency-path diagnostic when a selected symbol or its
  required type cannot be lowered. Unrelated unsupported declarations do not fail it (§15).

### Required Acceptance Tests

Implementation requires compiler-level semantic/IR tests, C ABI probes, and Promise runtime
integration tests. All advertised targets must pass these gates before being enabled:

- **ABI:** compare `sizeof`, `_Alignof`, and `offsetof` against pinned Clang for nested
  records, arrays, scalars, and pointers; round-trip signed/unsigned narrow integers,
  floats, `CBool`, nulls, and pointer out-parameters through compiled C. Check call and
  declaration attributes in IR, on AMD64 and ARM64 including Apple-specific behavior.
- **Safety rejection:** read-only writes, casts outside §6's matrix, unsupported pointees,
  callback placeholders, invalid `native`, raw destructors, by-value records, shared
  mutable exports, and `Vector[bool]` exports fail. Valid read-only literal-vector access
  succeeds without writing read-only pages or bypassing copy-on-write.
  Exercise identity casts, pointer-free scalar/record casts, and their `Void` round-trips
  with outer constness preserved. Reject direct nested qualification changes and erasure
  or byte-view casts involving pointer-bearing records/pointees, including generic cases.
  Run both of §9's output-buffer fixtures, the `raw` record and the bare local array, and
  check every byte of each; they must produce identical exports. Test read-only element addresses,
  mutable-address rejection under a shared borrow, negative/out-of-range indices, empty
  arrays, temporary arrays, vector elements, and one-time evaluation of base/index expressions.
  Bounds failures must happen before C is called; C must observe and update the original
  array element storage rather than a temporary copy.
- **Ownership:** execute §16 with a temporary database; cover successful open/exec/drop,
  invalid SQL, embedded-NUL rejection, failed opens returning null and non-null handles,
  and failed error-text conversion. Use deterministic C test doubles where failure
  injection is needed. Count acquired/released C handles and allocations explicitly;
  Promise's leak counter alone cannot establish zero C leaks. Check moving the wrapper
  closes exactly once. Reject `copy`/`clone` on the `single_owner` wrapper and duplication
  through vector slicing, containing records, and nested-container operations; nonduplicating
  moves remain valid. Package the wrapper as a public module with documented public members
  and compile a separate consumer: path construction and `exec` succeed, while `_db` reads,
  writes, `Sqlite(_db: handle)`, and access to private extern declarations fail. Count exactly
  one close after an owner moves across an ordinary function boundary.
- **Execution:** a blocking C fixture signals entry using an explicit synchronization
  primitive while another goroutine makes progress with one P; no sleeps order the test.
  Verify multiple concurrent foreign waits do not exhaust all runnable workers. Verify
  captured `errno`/`GetLastError` survives scheduler exit and runtime calls, with per-call
  out-parameters preventing cross-goroutine contamination.
- **Build/cache:** header-only changes, include-search resolution changes, defines, compiler
  and sysroot digests, mode, archive content, and link options invalidate affected results.
  Include sidecar bytes and every ABI/configuration field in invalidation tests. Reject a
  sidecar attached to the wrong archive, a stale header digest, a changed define/include
  order, unknown schema fields/versions, mismatched toolchain/runtime profiles, and an OS
  floor newer than the consumer's. Validate both success and rejection paths on all archive
  object formats; do not treat absent object metadata as proof of producer assertions.
  Exercise dependency-module propagation, duplicate imports, conflicting library versions,
  missing symbols, PAL collisions, and transitive static archive resolution.
  Accept different producer/consumer executable hashes when each matches its own host entry
  in the same toolchain profile. Reject unknown producer hosts, forged producer digests,
  unlisted consumer executables, unsupported host/target pairs, and mismatched profile digests.
  Verify local cache invalidation still uses the consumer's actual executable digest.
- **Distribution:** build native-source and archive fixtures on clean hosts without a
  preinstalled C compiler/SDK; test managed acquisition, checksums, pre-staged offline builds,
  and actionable missing-payload failures. Cross-compile each supported host/target pair;
  run target binaries on matching CI runners as well as checking object architecture.
  Produce a `linux-amd64` archive on Linux, then consume the exact same archive and sidecar
  on both Linux and macOS targeting `linux-amd64`. Use different, catalog-approved host
  compiler executables from the same profile and identical target/header content identities;
  both builds must pass validation and their binaries must pass the ABI probes on Linux.
  Do not rewrite the sidecar or require the producer's executable on the macOS consumer.
- **Bindgen/targets:** resolve typedefs, conditionals, enums, `_Bool`, and character buffers
  using the manifest configuration. Ignore unselected callbacks/variadics in SQLite headers;
  reject selected ones without replacing output. Reject stale target-specific bindings.
  Build a WASM fallback with native tables present and no native toolchain acquisition;
  fail if a native extern survives target filtering, even when unused.

These tests validate specific contracts; they do not certify arbitrary linked C code or
prove that a handwritten wrapper meets every memory/lifetime obligation.

## 18. Non-Goals / Future Work

Each cut in §1/§10, with what would need to change to lift it:

- **By-value foreign struct passing/returns.** Needs per-target C ABI argument classification
  (System V AMD64 eightbyte classification, Microsoft x64, AAPCS64 HFA rules) implemented in
  codegen — real work, not a small addition. Until then, out-parameter (`RawPtr`) shims cover
  the same ground for the libraries this document targets.
- **C function pointers/callbacks into Promise.** Needs a way to expose a Promise closure as a
  plain C-ABI function pointer — structurally the same "new direction of call" problem already
  identified for WASM/WebIDL callback support, but for native targets.
- **A declared nonblocking `extern` opt-out.** V1 hands off to the scheduler around every
  foreign call, which costs two transitions per call even for an accessor that returns in
  nanoseconds (§12). Lifting this needs a way for a declaration to assert bounded, truly
  nonblocking execution, plus a story for what happens when that assertion is wrong, since
  a mistaken one stalls a P with no diagnostic. Until then, batch such calls in a C shim.
- **Dynamic loading** (§13.4) as a first-class opt-in.
- **Lifetime-checked foreign borrows and thread-affine sessions.** Read-only/mutable pointer
  separation is required in v1; ownership provenance and lifetimes are still caller obligations.
  Any stronger static gate or thread-pinning API needs a separate language/runtime design.
- **Unions, bitfields, non-default packing/alignment, flexible array members, and
  volatile/atomic storage access.**
- **Variadic `` `extern `` functions.**
- **WASM C object linking**, with its own target/runtime contract rather than host objects.
- **Remote third-party native archives and cross-language LTO.** Neither is part of v1's
  vendored machine-object contract.

## 19. Open Questions

The draft now chooses all-`raw` layout without `native`, separate const/mutable pointers,
explicit `cffi` imports, `_unchecked` naming, required symbol allow-lists, and managed Clang
inputs. These are proposed decisions for ratification, not unresolved implementation defaults.

- **Naming versus a real capability gate.** The choice is not between the `_unchecked`
  convention and nothing; it is between a convention and a static capability or effect gate
  that the type system can reason about. The convention is where the no-new-syntax goal
  (§1) gets paid for: a compiler-mandated suffix on user declarations marks the audit
  surface but does not compose, since a wrapper may enclose an unchecked call in a
  safe-looking name and the compiler cannot tell a correct wrapper from an incorrect one
  (§8 says so outright). A capability gate would compose and could be checked, at the cost
  of the new syntax and the language-level design this draft avoids. Ratification should
  decide that trade explicitly rather than inherit it; a different answer rewrites §6–§8.
- Which host/target pairs can ship redistributable, sufficiently complete C headers and
  runtimes? §10 defines intended coverage; §13 and §17 are gates, not permission to silently
  require a system SDK. Any reduction of the ratified target matrix must be explicit.
- V1 updates replace vendored sources/headers/archives, refresh checksums, and regenerate
  affected bindings in one reviewed change. Should later package tooling automate those
  updates and security advisories? Tooling must not assume updates are infrequent or safe
  without re-running ABI and wrapper tests.
- **Is per-epoch rebuild churn acceptable for prebuilt archives?** §13.3 ties every archive
  to one exact host-independent toolchain profile and target-runtime identity, so a
  toolchain-profile or target-sysroot bump breaks every Path B consumer until the producer
  republishes. Refusing an implicit compatibility whitelist is right, since nothing would
  check it. The open question is whether a *declared* compatibility range, validated by the
  §17 probes on each target it claims, is worth specifying so an archive survives toolchain
  bumps that provably do not change the C ABI. Doing nothing is a defensible answer, but it
  should be a decision rather than a side effect, because it sets how long a published
  archive stays usable. The sidecar schema compounds the same question: with every listed
  field required and unknown fields rejected, any field added after ratification is a
  breaking change that forces every producer to republish, so the version rule should say
  whether new fields may ever be optional within a schema version, or whether each addition
  is deliberately an epoch-scale event.
- **How does ratification split this document without duplicating facts?** §13 and §17
  currently state cache-key composition, link ordering, symbol-collision policy, and
  distribution payload rules in full. Those facts are owned by
  [module-system.md](../module-system.md) and [distribution.md](../distribution.md), and
  [normative.md](../normative.md) §4 treats a fact with two homes as a defect, not a
  convenience. Stating them here is correct while this document is a non-binding proposal
  and would become a defect the moment it moves into the root. The ratifying change
  therefore has to decide, per fact, which document owns it and leave a cross-reference
  behind — planning that split before the move is cheaper than unpicking the duplication
  after it.
