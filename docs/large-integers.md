# Large Integer Types

> **Tag:** `large-integers` — remaining work to complete this document: `mcp__tracker__list --tag large-integers`

---

## 1. Motivation

LLVM IR natively supports integers of any bit width up to ~16M bits via the `iN` type. Today Promise only exposes integers up to 64 bits (`i64`/`u64`). Programs that need wider integers — cryptography, hashing, fixed-point currency math, network/UUID identifiers, compression, big-number protocols — must currently fake them with arrays of `u64` or vectors of bytes plus hand-rolled long-arithmetic routines:

- **Bug-prone**: carry propagation, sign extension, shift composition, and overflow handling are all easy to get wrong.
- **Inefficient**: hand-rolled `u64[2]` arithmetic does not benefit from the carry-flag-aware codegen LLVM emits for `i128` (`adc`/`sbb` on x86-64, `adcs`/`sbcs` on ARM64), nor from `mulq`/`umulh` for double-width multiply, nor from soft-division library calls already shipped by compiler-rt.
- **Unreadable**: every cryptographic primitive becomes a soup of shifts and masks instead of straight-line modular arithmetic.

The fix is the same fix every other systems language has converged on: expose a small set of fixed-width wide integer types, and let LLVM lower the operations.

This plan optimizes for Promise's stated goals (CLAUDE.md):

- **Self-contained readability**: a reader sees `u256` and immediately knows it is a 256-bit unsigned integer with the standard operator set, just like `u64`.
- **Explicit over implicit**: widths are explicit; widening/narrowing requires `as!` (no implicit promotion across widths).
- **Minimal context**: no new mental model — the existing primitive pattern (`type uN \`native`) extends literally with one more numeral.
- **One obvious way**: a single fixed list of supported widths; anything beyond goes through a `BigInt` library type (out of scope here).

---

## 2. Scope and Maximum Width

**Proposed widths**: 128, 256, 512 (signed and unsigned). Combined with the existing 8/16/32/64 widths, the full primitive integer ladder becomes:

| Signed | Unsigned | LLVM type | Typical use |
|--------|----------|-----------|-------------|
| `i8`   | `u8`     | `i8`      | bytes |
| `i16`  | `u16`    | `i16`     | small counters, codepoints in UTF-16 |
| `i32`  | `u32`    | `i32`     | counters, file offsets ≤4GB |
| `i64`  | `u64`    | `i64`     | general integers, file offsets, timestamps |
| **`i128`** | **`u128`** | `i128` | UUIDs, IPv6 addresses, currency at 1e-18 precision, secp256k1 field components, AES round state |
| **`i256`** | **`u256`** | `i256` | SHA-256 digests as integers, BLS12-381 scalar field, Curve25519/Ed25519 scalars, Ethereum word, Poseidon hash state |
| **`i512`** | **`u512`** | `i512` | SHA-512 digests, RSA-512 chunks, BLS12-381 base field, Goldilocks field × 8 |

`int`/`uint` continue to alias `i64`/`u64` (host pointer width is always 64-bit on supported targets).

### Why cap at 512 bits?

- **Coverage**: 128/256/512 covers all common cryptographic hash widths (MD5/128, SHA-256/256, SHA-512/512), elliptic curve scalars (Curve25519/256, secp256k1/256, BLS12-381 scalar/256, BLS12-381 base/381 fits in 512), RSA chunks up to 512 bits, and standard fixed-point currency precisions. It leaves only RSA-1024+ and some pairing-cryptography intermediate values out — and those workloads already want a `BigInt` library type because constant-time arithmetic at those widths is its own discipline.
- **Code size**: each width adds a vtable, RTTI, drop/clone metadata, and per-width stdlib methods (~30 native operator stubs + `to_string`, `parse`, `format`, `encode`, `decode`, `hash`). At three new widths × two signednesses = six new types; at four widths it would be eight. Six is the sweet spot: covers the practical cases without doubling stdlib mass.
- **Compile-time cost**: LLVM `i512` operations expand to several dozen IR instructions per arithmetic op after legalization. Beyond 512, IR size and opt time grow superlinearly without paying for itself.
- **Runtime cost predictability**: fixed widths give predictable performance (everything is inlined as a fixed sequence of word-sized ops). Above 512 bits the hidden cost of every multiply or divide grows large enough that hiding it behind a primitive is misleading — a `BigInt` library type makes the cost visible and lets users opt into windowing strategies.
- **Library escape hatch**: arbitrary-precision arithmetic belongs in a `BigInt` value type backed by `Vector[u64]` (out of scope for this plan, tracked separately). The native-primitive ladder ends where the library begins.

### Why not arbitrary `iN`?

Tempting, but rejected:

- It breaks the "fixed list of obvious primitive types" principle; a reader cannot know whether `i131` is a real type without checking elsewhere.
- It bloats the universe scope and complicates type identity (every distinct width becomes a distinct named type with its own vtable, RTTI, stdlib methods).
- It encourages users to invent ad-hoc widths (`i48`, `i96`) which fragment the ABI for no real gain.
- The genuine use cases for non-power-of-two widths (e.g., bit-packed protocol fields) are better served by structs with `\`raw` LLVM-typed fields.

### Why not an integer-width *parameter* like `Int[N]`?

Promise's generics do not currently take const parameters, and adding them just for this would be a large language change. Even if added, it would surface compile-time-evaluated arithmetic on width parameters and complicate monomorphization. A short fixed ladder is clearer.

---

## 3. Language Changes

### 3.1 Type Declarations

Add six new universe-scope native types following the existing primitive pattern from §5.1 of [language-design.md](language-design.md):

```promise
type i128 `native `public {
  +(i128 other) i128 `native;
  -(i128 other) i128 `native;
  *(i128 other) i128 `native;
  /(i128 other) i128 `native;
  %(i128 other) i128 `native;
  ==(i128 other) bool `native;
  !=(i128 other) bool `native;
  <(i128 other) bool `native;
  >(i128 other) bool `native;
  <=(i128 other) bool `native;
  >=(i128 other) bool `native;
  -() i128 `native;
  ++() i128 `native;
  --() i128 `native;
  &(i128 other) i128 `native;
  |(i128 other) i128 `native;
  ^(i128 other) i128 `native;
  <<(i128 other) i128 `native;
  >>(i128 other) i128 `native;
  ~() i128 `native;
  ..(i128 end) Range[i128] `native;
  ..=(i128 end) Range[i128] `native;
  get hash int `native;
  get max i128 `public `global => /* literal */;
  get min i128 `public `global => /* literal */;

  parse!(Reader ~r) i128 `factory `public { ... }
  to_string(int base = 10, ...) string `public { ... }
  format!(Writer ~w) `public { ... }
  encode!(Encoder ~e) `public { ... }
  decode!(Decoder ~d) Self `factory `public { ... }
}
```

Same structure for `u128`, `i256`, `u256`, `i512`, `u512`. These are all **pure value types** (all storage is in a single `\`raw` LLVM `iN` field), so they automatically receive `\`copy` semantics, no heap allocation, and no `drop()`.

### 3.2 Numeric Literal Suffixes

Extend §5.3 ("Typed Numeric Literal Suffixes"): the integer suffix list grows to `i8`, `u8`, `i16`, `u16`, `i32`, `u32`, `i64`, `u64`, **`i128`, `u128`, `i256`, `u256`, `i512`, `u512`**.

```promise
x := 0xDEADBEEFCAFEBABEDEADBEEFCAFEBABEu128;
y := 1_000_000_000_000_000_000_000_000u128;       // 10^24
z := 0x_FFFF_FFFF_FFFF_FFFF_FFFF_FFFF_FFFF_FFFFu128; // 2^128 - 1
```

The compiler currently parses literal magnitudes via `strconv.ParseInt`/`ParseUint` (64-bit). Wide literals require an extension:

- The lexer/parser already produces the literal text; the type checker is what converts it to a numeric value. **Switch literal magnitudes to `*big.Int`** (`math/big`) inside the AST literal node.
- Range-check at type-check time against `[min, max]` of the target type (existing behavior already implements range checks for `u8`/`i8`/etc.; reuse the same machinery, just at arbitrary precision).
- Codegen emits the IR constant via `llir/llvm`'s `constant.NewIntFromString` (which accepts a base-10 or base-16 string of any width). LLVM then writes the constant into the `iN` IR type.

There is one subtle case: large literals **without** a suffix that exceed 64-bit. Today `1 << 100` would error during parse. The new rule is consistent with the suffix-required design: literals without a suffix that exceed `i64` range are a type error, suggesting the user add an `i128`/`u128`/etc. suffix or annotate the destination type.

### 3.3 Conversions and Casts

Promise already requires explicit `as!` for all numeric conversions. The wide types follow the same rule:

```promise
i128 a = 42i128;
i64 narrow = a as! i64;          // truncates to lower 64 bits
i256 wide = a as! i256;          // sign-extends to 256 bits
u128 ub = a as! u128;            // bit-reinterpret (same size)
i128 b = 12345 as! i128;         // i64 literal widened with sign extension
```

Conversion semantics:

- **Widen, same signedness**: sign-extend (signed) or zero-extend (unsigned).
- **Widen, signed → unsigned**: bit-cast (treats the negative number as a very large positive number — Rust's `as` semantics). Documented as such; no `as!?` failable form needed.
- **Widen, unsigned → signed**: zero-extend; if the high bit lands on the sign position the value reads as negative, again per `as` semantics.
- **Narrow**: truncate (lower bits).
- **Float ↔ wide int**: `i256 as! f64` rounds; `f64 as! i256` truncates toward zero. `f64` only has 53 bits of precision, so very large values lose information — this is the same trap that already exists for `f64 → i64`.

No implicit widening. The `+`, `-`, `*` etc. methods on `i128` take an `i128` parameter; mixing widths requires an explicit cast at the call site. This matches the existing rule and keeps overload resolution unambiguous.

### 3.4 Range and Iteration

`..` and `..=` produce `Range[i128]` etc., already supported by the existing `Range[T]` generic — no new work beyond the generic instantiation.

For-in loops over wide-integer ranges work but should carry a warning in the language guide: iterating an `i256` range at scale is rarely what the user intends. (No compiler diagnostic — the generic plumbing makes it work and a runaway loop is a runtime concern.)

### 3.5 `int` vs `i128` for hash output

`get hash int` continues to return `i64` even on wide-integer types. The hash interface returns `int` everywhere so that maps/sets remain uniform. The implementation folds the wide value to 64 bits (XOR-fold halves with a multiply-mix step — same as Rust's `FxHash` finisher applied to each 64-bit limb).

---

## 4. Codegen Changes

### 4.1 Universe registration

[compiler/internal/types/universe.go](../compiler/internal/types/universe.go) adds six new globals (`TypI128`, `TypU128`, `TypI256`, `TypU256`, `TypI512`, `TypU512`), each registered in the universe scope with `defNamed`.

### 4.2 Type classification

[compiler/internal/codegen/types.go](../compiler/internal/codegen/types.go) extends `classify()` to map the new types into `CatSignedInt`/`CatUnsignedInt`. `llvmNamedType()` maps:

```go
case types.TypI128, types.TypU128:
    return irtypes.NewInt(128)
case types.TypI256, types.TypU256:
    return irtypes.NewInt(256)
case types.TypI512, types.TypU512:
    return irtypes.NewInt(512)
```

`isPrimitiveScalar`, `isPrimitiveNumeric`, `isSignedType` automatically pick up the new types via `classify()`.

### 4.3 Native operator emission

The existing native-method dispatcher in codegen lowers `int.+`, `int.-`, etc. by emitting LLVM `add`, `sub`, `mul`, `sdiv`/`udiv`, `srem`/`urem`, `and`, `or`, `xor`, `shl`, `lshr`/`ashr`, `icmp` instructions. **These instructions are width-polymorphic in LLVM IR**: they accept any `iN`, signed or unsigned (signedness is per-instruction, not per-type). The dispatcher already keys on `TypeCategory` for the signed/unsigned choice, so no new code paths are required — the existing logic emits `sdiv` for signed integers and `udiv` for unsigned, regardless of width. The only gating is to make sure native-method recognition tables include the new types (one entry per `(type, op)` pair if the table is enumerated, or fallthrough by category if not).

### 4.4 Soft-arithmetic library dependencies

LLVM lowers wide-integer ops differently per target:

- **x86_64 / ARM64**:
  - `add`/`sub`/`and`/`or`/`xor`/`shl`/`lshr`/`ashr` on `i128`/`i256`/`i512` → inline multi-word sequences using carry flags. **No runtime calls.**
  - `mul` on `i128` → inline using `mulq` + `umulh`. **No runtime calls.**
  - `mul` on `i256`/`i512` → expanded as multiple double-word multiplies. **No runtime calls.**
  - `udiv`/`sdiv`/`urem`/`srem` on `i128` → inline on ARM64 (long sequence), but on x86_64 lowers to `__udivti3`/`__divti3`/`__umodti3`/`__modti3` libcalls.
  - `udiv`/`sdiv`/`urem`/`srem` on `i256`/`i512` → LLVM's `ExpandLargeDivRem` pass inline-expands these; **no libcall**.
- **WASM**: similar — small ops inline, 128-bit division lowers to the `__*ti3` libcalls.
- **Conclusion**: 128-bit division is the only operation that requires runtime support. Promise does **not** link compiler-rt or libgcc (the default target statically links musl alone), so the compiler **emits the four `__*ti3` builtins in-IR itself** on every target (`emitDivTi3` in `codegen/compiler.go`, T1399), backed by a shared shift-subtract `__promise_udivmod128` helper. The definitions use external linkage in the main IR; on the glibc dynamic path the strong definition satisfies the reference before `-lgcc`'s archive member is consulted, so there is no duplicate-symbol conflict. **The PAL needs no new entry points**, but the builtins are not free — they must be emitted, not assumed.

**Builtin signatures are target-dependent (T1414).** The emitted definition must match the calling convention LLVM's backend uses at the *call* site, which is not the same on every target:

- **x86-64 Windows** (`x86_64-pc-windows-msvc`) uses the Win64 128-bit libcall convention: both operands are passed **indirectly by pointer** (`rcx`/`rdx`), and the 128-bit result is returned in **`xmm0`**. The compiler therefore emits `define <2 x i64> @__udivti3(i128* %pa, i128* %pb)` there — element 0 is the low word, element 1 the high word (`bitcast i128 to <2 x i64>` on little-endian). A by-value `i128` definition compiles to a callee that reads the *pointers* as operands and returns into registers the caller never reads, so every `/` and `%` with a non-constant i128/u128 divisor silently yields garbage. (Constant divisors are strength-reduced to multiply+shift and never reach the libcall, which is why the breakage was partial.)
- **Every other target** — SysV x86-64, ARM64 (including `aarch64-pc-windows-msvc`), macOS, wasm — passes `i128` by value in register pairs and returns it the same way, so the plain `define i128 @__udivti3(i128 %a, i128 %b)` form is correct.

The builtin *bodies* are identical across targets; only the wrapper signature and the loads/bitcast around it differ. `win64IndirectI128Libcall()` gates the two shapes.

Regression guards:

- `TestWideIntDivBuiltins` asserts the `__*ti3` + `__promise_udivmod128` symbols are present in generated IR on both native and wasm targets.
- `TestWideIntDivBuiltinsWin64ABI` pins the emitted *signature and body* per target — symbol presence alone passes with the wrong ABI, and the signature alone would pass for a wrapper that never dereferences its pointer operands. It also asserts the internal `__promise_udivmod128` helper keeps its by-value signature everywhere (only the four public wrappers change shape) and covers both discriminating triples: `aarch64-pc-windows-msvc` (Windows, not x86-64) and `x86_64-apple-macosx*` (x86-64, not Windows).
- `TestWin64IndirectI128LibcallGate` pins the target-triple predicate directly.

Because these tests drive codegen with explicit target triples, they catch a Windows ABI regression from any host — no Windows CI runner is required.

At runtime, `tests/std/wide_int_test.pr` exercises the libcall itself with optimizer-opaque divisors (a literal divisor is strength-reduced to multiply+shift and never reaches it): `wide_div_opaque_divisor` (sign matrix), `wide_div_opaque_unsigned_edge` / `wide_div_opaque_signed_edge` (quotient zero, self-division, divide-by-one, zero dividend, divisors wider than u64, `i128.min` magnitude wrap), `wide_format_opaque_base` (the `_wide_int_format_128` digit loop that first surfaced T1414, driven at an opaque base), and `wide_div_opaque_256_512` (guards the inline-expanded widths against ever reinheriting a libcall). Note that a batch-test file whose process dies mid-run currently still reports success (T1415), so the Go signature tests — not the runtime tests — are the load-bearing guard until that lands.

### 4.5 ABI for value types

Existing rule: pure value types pass by value via their LLVM raw type. `i128` passes as one LLVM `i128` argument (which the backend may split across two registers per platform calling convention — that's a backend concern, not ours). `i256`/`i512` likely lower to indirect (memory) passing on most ABIs; LLVM handles this transparently via its calling convention layer. No special-case Promise ABI needed.

### 4.6 Constant emission

When emitting a literal, the codegen path constructs `constant.NewIntFromString(typ, str, base)` from the `*big.Int` magnitude carried by the AST literal node. This already works in `llir/llvm`.

### 4.7 RTTI, vtable, type ID

Each of the six types gets the standard four-struct layout (Type, Variant, Instance, Value) from the value-type path: a global RTTI singleton, a vtable, and a type ID assigned by `c.nextTypeID++` at universe registration time. Layout is identical to the existing primitives — just with a wider raw field.

---

## 5. Standard Library Changes

### 5.1 File layout

All six wide-integer types ship in one file: **[modules/std/wide_int.pr](../modules/std/wide_int.pr)**. This mirrors how the existing `int.pr` declares `int`/`i8`/`i16`/`i32`/`i64` in a single source file and `uint.pr` declares `uint`/`u8`/`u16`/`u32`/`u64` — the type declarations are mostly `` `native `` operator stubs (no Promise bodies), so per-type files would add cache and compile cost without adding readability. One file for all six covers `i128`, `u128`, `i256`, `u256`, `i512`, `u512`.

Each declaration contains the same surface as the entries in `i64.pr`: arithmetic/comparison/bitwise/range operators (all `\`native`), `get min`/`get max` constants, `to_string`, `format`, `parse`, `encode`, `decode`, `hash`. Implementation strategy follows CLAUDE.md's "Prefer Promise over IR" rule:

- **Operators**: `\`native` (LLVM emits the instruction directly).
- **`to_string` / `parse`**: written in Promise, calling into a small set of helpers (one per width, see §5.2) rather than per-type IR.
- **`encode` / `decode`**: serialize as a sequence of bytes (little-endian, 16/32/64 bytes for 128/256/512). The `Encoder`/`Decoder` interface gains `encode_i128`/`encode_u128`/... methods on each implementation, mirroring the existing `encode_int`/`encode_i64` shape.
- **`hash`**: `\`native`, fold to 64 bits by XORing 64-bit limbs and applying the existing `int.hash` finisher.

### 5.2 Format / parse — default base 16

Wide integers default to **base 16** for both `to_string` and `parse`. This is the only place the wide-integer surface deliberately diverges from the small-integer surface (which defaults to base 10).

Rationale:

- The dominant use of wide integers is cryptographic — hashes, addresses, UUIDs as `u128`, ECC scalars. These are universally written and read in hex.
- Hex formatting is shift-and-mask: O(width) with no division. Decimal formatting needs one full division per digit; on `i512` that's ~155 divisions per call, each a libcall. The default that's "fast and crypto-shaped" is the right default for the type.
- A `u128` rendered in hex is exactly 32 characters wide regardless of value — a stable, alignable shape — whereas the decimal width varies with magnitude.
- Decimal remains available via `to_string(base: 10)` / `parse(reader, base: 10)` for the cases where it's wanted (human-readable counters, currency at fixed-point precision, etc.).

Method signatures:

```promise
to_string(int base = 16, bool prefix = false, int? width, char fill = ' ') string `public;
parse!(Reader ~r, int base = 16) Self `factory `public;
format!(Writer ~w) `public;                       // delegates to to_string() — base 16
```

Default parameter values let `parse(reader)` and `to_string()` continue to satisfy the `Parse` and `Format` structural interfaces (which declare `parse!(Reader ~r) Self` and `to_string() string`). A caller passing only the `Reader` gets the base-16 default; an explicit base overrides. This also keeps the wide types usable as `T: Parse` in `scan[T](string s)`, so `scan[u128]("abc123…")` works without ceremony.

Implementation: a per-width helper (`_wide_int_format_128`, `_wide_int_format_256`, `_wide_int_format_512`) does the digit loop using the wide type's own arithmetic (which routes through LLVM and, for division, the soft-arith library). Base 16 takes the fast path (4-bit shifts); base 10 (and other bases) take the slow division path. Once generics-over-numeric-types are expressive enough, the three helpers collapse into one — tracked separately.

### 5.3 Hashable, Ordered, Equal, Cloneable

Each new type implements `is Hashable`, `is Ordered`, `is Equal` via the standard structural-interface mechanism. Pure value types are automatically `\`copy`, so `Cloneable` is satisfied trivially.

### 5.4 Random

[modules/std/random.pr](../modules/std/random.pr) gains `next_u128`, `next_u256`, `next_u512` returning uniformly distributed wide integers via repeated 64-bit draws.

### 5.5 Math

`math.pr` does **not** add wide-integer overloads of `min`/`max`/`abs`/etc. — these become structural-interface calls on `Ordered`, automatically reaching all integer types.

---

## 6. Catalog Module Impact

- **`json`**: `JsonEncoder`/`JsonDecoder` need to decide a JSON representation. Numbers in JSON are doubles. **Decision: encode wide integers as decimal strings** (well-established convention used by Ethereum JSON-RPC, BigInteger Java JSON libs, etc.). Add `encode_i128(i128)`, `encode_u128(u128)`, etc. methods to `Encoder` that delegate to `encode_string` on the JSON path, preserving the value losslessly. `decode_*` parses from string. Plain integer JSON tokens that fit in `i64` continue to decode through the existing path; oversized tokens raise `DecodeError`.
- **`net`/`http`**: no impact (these don't expose typed integer fields beyond i32/i64).
- **`os`**: no impact.
- **`time`**: no impact (time is i64 nanoseconds; wide-integer durations are not in scope).

---

## 7. Performance Notes

- **i128 arithmetic** (add/sub/mul) is roughly 1.5–2× the cost of i64 on x86_64 / ARM64 — usually worth it vs hand-rolled u64 pairs.
- **i128 division** is a libcall — ~30–50 cycles on modern CPUs. Tight inner loops should reuse Barrett/Montgomery reductions, same as in C/Rust.
- **i256 / i512 arithmetic** scales linearly; multiply scales quadratically (Karatsuba kicks in around 1024 bits, beyond our scope, so it doesn't apply here).
- **i256 / i512 division** is a libcall to `__udivei*`/`__divei*` — hundreds of cycles. For crypto, division is rare (modular reductions use specialized routines); for general use, this is acceptable.
- **Default `to_string` is base 16** (shifts and masks only) so the fast path is the default. Decimal output via `to_string(base: 10)` falls back to the soft-division loop — ~155 divisions for a `u512` worst case. Acceptable for diagnostics or human-readable values; users formatting in tight loops should keep the default.

These costs are documented in the language guide section that introduces wide types so users can reason about hot paths.

---

## 8. Target Compatibility

- **Linux x86_64** (static musl, no compiler-rt): supported. The compiler emits `__udivti3`/`__umodti3`/`__divti3`/`__modti3` in-IR (T1399); nothing external is required.
- **Linux ARM64**: supported. The same in-IR builtins are emitted; ARM64 may inline some 128-bit div/rem sequences directly.
- **macOS x86_64 / ARM64**: supported via the same in-IR builtins.
- **Windows x86_64 (MSVC)**: supported. The in-IR builtins are emitted here too, so 128-bit div/rem resolves without relying on the MSVC/compiler-rt runtime — but they must be emitted with the **Win64 indirect libcall ABI** (operands by pointer, `<2 x i64>` result in `xmm0`), not the by-value `i128` form the other targets use. See §4.4 and T1414.
- **Windows ARM64 (MSVC)**: supported. `aarch64-pc-windows-msvc` passes `i128` by value like SysV, so it uses the same by-value builtin signature as Linux/macOS.
- **WASM (wasm32-wasi, wasm32-web)**: supported. The in-IR builtins (plus `__multi3`) cover what the WASM toolchain does not provide.

Verification step before committing: a single test program performing `i128`, `i256`, `i512` add/mul/div on each target, observed via `bin/test --wasm --wasm-web`.

---

## 9. Open Questions

1. **Should `parse` on wide types support a leading `+` sign?** Existing `int.parse` does not. Keep consistent (no leading `+`).
2. **Should we expose `wrapping_add` / `checked_add` / `overflowing_add` (Rust-style)?** Out of scope for this plan. The current Promise position is "arithmetic wraps silently for unsigned, traps for signed in debug" (matching Rust `Wrapping` / debug-overflow conventions); wide types follow the same rule. A future tracker task can introduce a checked-arithmetic family across all integer widths uniformly.
3. **Constant-time operations for crypto?** Out of scope. LLVM's `iN` div/mul are not constant-time. Crypto code that needs constant-time must hand-craft routines using bitwise ops on `u64[]` or `u32[]` arrays — that pattern remains valid even after this plan lands. Document this clearly in the language guide.
4. **Atomic operations on wide types?** Out of scope. Atomics on `i128` are platform-specific (x86 has `cmpxchg16b`, ARM64 has `casp`); 256/512 generally lack hardware atomics. If atomics on wide types become necessary, file a separate tracker task.

(The earlier draft included "default base 16 vs 10 for `to_string`/`parse`" here; that's now a settled decision — see §5.2.)

---

## 10. Non-Goals

Explicitly **not** part of this plan:

- Arbitrary-precision `BigInt` (a separate library type, future work).
- Decimal floating point (`d64`, `d128`).
- Saturating, checked, or wrapping arithmetic operator variants.
- Compile-time arbitrary-precision constant evaluation beyond literal parsing.
- Const generic integer width parameters (`Int[N]`).
- SIMD / vector-of-wide-integer types.

These are listed so reviewers know they were considered and intentionally deferred.
