# Memory Management Review: Current State, Gaps, and Path to Rust-Class Safety

## Context

Promise aims for Rust-inspired ownership semantics with Dart-like syntax. The memory management system has been under intensive development — over 100 tracker items (T0061–T0137, B0167–B0255) have been resolved, bringing leak counts from ~2,150 test failures down to ~20 remaining `allow_leaks` tags across 10 files. This review assesses what's implemented, what's missing, and what's needed for a complete, efficient, safe memory management system.

---

## 1. WHAT'S IMPLEMENTED AND WORKING

### Ownership Core (Solid)
- **Single ownership**: every value has one owner, enforced in `compiler/internal/ownership/`
- **Move semantics**: non-copy values moved on assignment/call/return; use-after-move detected
- **Move resurrection**: reassignment revives a moved variable
- **Copy types**: primitives, references, `value` types, and explicit `` `copy `` types bypass move semantics
- **Drop flags**: per-variable `i1` alloca, cleared at 8+ move sites in codegen — conditional drops work correctly

### Borrowing (Functional, with Limits)
- **Shared (`&`) and mutable (`~`) borrows** with conflict detection (mut+any = error)
- **Call-scoped borrows**: expire at statement boundary
- **Variable-scoped borrows**: promoted when stored in a variable
- **Disjoint field borrows** (B0037 done): `&f.x` and `&f.y` don't conflict
- **Move-while-borrowed**: detected and rejected
- **Return reference safety**: returning ref to local variable is an error
- **Stored references in structs** (B0034 done): can store `T&` in fields

### Drop/Cleanup Infrastructure (Comprehensive)
- **Scope cleanup stack**: 8 binding kinds (close, drop, dropString, dropEnum, dropOptional, free, freeEnv, generator) — LIFO at scope exit, return, break, continue, raise
- **Synthesized drops**: auto-generated for types with non-copy fields, including generic instances at mono time
- **Enum variant drops** (T0102): switch on tag, per-variant field cleanup
- **Optional drops** (T0101): check has-value flag before dropping inner
- **Closure env drops** (B0221): env struct header has drop function pointer, recursive cleanup
- **String literals**: `.rodata` with bit 63 literal flag — zero malloc, `promise_string_drop` checks before free
- **Vector literals**: `.rodata` with bit 63 static flag + copy-on-write on mutation
- **RTTI drop dispatch** (B0226): typeinfo stores drop function pointer for polymorphic cleanup
- **Statement-level temp tracking** (T0073): intermediate strings/vectors/channels freed at statement end
- **`use` bindings**: deterministic `close()` at scope exit with error propagation rules
- **Leak detection in tests** (T0020): alloc count tracking, `allow_leaks` annotation, stale tag detection

### Container Cleanup (Done)
- Vector, Map, Set, Channel all have proper scope-exit drops
- Vector[string] element drops before container drop (B0189)
- Map slot enum data drops on destruction/overwrite (B0232, B0235)
- Container field reassignment drops old value (B0219)

---

## 2. ACTIVE BUGS AND REMAINING LEAKS

### Critical Path: T0066 (Zero-Leak Policy) → T0109 → B0228

| ID | Status | What | Impact |
|----|--------|------|--------|
| **B0228** (high) | open | Panicked goroutine leaks ALL heap resources — `longjmp` to scheduler bypasses scope cleanup entirely | Blocks T0109, blocks T0066 |
| **T0109** (high) | in_progress | Final zero-leak enforcement — remove all `allow_leaks` tags | Blocked by B0228 |
| **T0066** (critical) | open | Drive all memory leaks to zero | Blocked by T0109 |

### Open Memory Bugs

| ID | Priority | What |
|----|----------|------|
| **B0253** (high) | open | `JsonValue.get()` on parsed JSON causes double-free (enum/map drop interaction) |
| **B0252** (medium) | open | Segfault during cleanup when main uses JsonValue enum with map fields |
| **B0255** (medium) | open | `string.to_string()` uses `this + ""` hack — no mechanism for returning owned copy of `this` |
| **B0254** (low) | open | `convertTupleToString` leaks individual element results |

### Current `allow_leaks` Tags: 20 across 10 files

```
tests/std/set_test.pr                          5 occurrences
tests/std/string_literal_test.pr               1
tests/e2e/flat_map_test.pr                     2
tests/e2e/serializable_test.pr                 4
tests/e2e/this_capture_test.pr                 1
tests/e2e/named_args_test.pr                   1
tests/e2e/discard_optional_drop_test.pr        1
tests/concurrency/panic_recovery_batch_test.pr 2
tests/catalog/path_test.pr                     1
modules/json/json_test.pr                      2
```

---

## 3. GAPS: WHAT'S MISSING FOR RUST-CLASS MEMORY SAFETY

### Gap 1: Goroutine Panic Cleanup (B0228) — CRITICAL

**Problem**: When a goroutine panics, `promise_panic` calls `longjmp` back to the scheduler, completely bypassing all scope cleanup (drop bindings, close calls, env frees). Every heap allocation in the goroutine leaks. Every `use`-bound resource stays open.

**Rust comparison**: Rust's `panic!` unwinds the stack, running all `Drop` implementations. Even `panic=abort` at least terminates the process cleanly. Promise's `longjmp` is equivalent to neither — it silently leaks.

**Planned fix** (T0143–T0151): Replace `longjmp` with TLS panic flag propagation. After every function call, check TLS flag → if set, run scope cleanup → propagate up. This is the single largest remaining memory safety gap.

**Impact**: Blocks zero-leak enforcement (T0066). Any program using goroutines + error conditions has potential resource leaks.

### Gap 2: Non-Lexical Lifetimes (B0035) — IN PROGRESS, MEDIUM

**Problem**: Borrows currently use statement-level granularity, not last-use. A borrow lives until end of statement/scope even if the borrowed reference is never used again.

```promise
main() {
  s := "hello";
  ref := &s;          // borrow starts
  println(ref);       // last use of ref
  modify(~s);         // ERROR: s still borrowed (ref in scope)
  // Rust NLL: this would work because ref's last use was above
}
```

**Rust comparison**: Rust 2021 has full NLL — borrows end at last use point. This is a significant ergonomic improvement that reduces false borrow conflicts.

**Impact**: Users must restructure code to work around false conflicts. Not a safety issue (over-conservative is safe), but a significant usability gap.

### Gap 3: Explicit Lifetime Annotations (B0033) — OPEN, MEDIUM

**Problem**: No syntax for explicit lifetime parameters (`'a`). The design spec (§6.3) shows the syntax:
```promise
longest['a](string &'a a, string &'a b) string &'a { ... }
```
But the grammar and compiler don't implement it.

**Rust comparison**: Explicit lifetimes are essential for complex reference patterns — storing references in structs, returning references from functions with multiple reference inputs, etc.

**Current state**: Aggressive elision handles ~95% of cases. The missing 5% blocks:
- Functions returning one of multiple input references conditionally
- Complex data structures holding references with varying lifetimes
- Self-referential patterns

**Impact**: Moderate. Most Promise code doesn't need explicit lifetimes due to its simpler use patterns. But any library code with complex borrowing patterns hits this wall.

### Gap 4: `clone` Meta Implementation — NOT IMPLEMENTED

**Problem**: The language design (§6.4) specifies `` `clone `` meta that auto-generates a `clone() Self` method for deep copying. This is not implemented.

**Rust comparison**: Rust's `#[derive(Clone)]` is fundamental — it's how you get owned copies of non-Copy types. Without it, users must write manual clone methods or use workarounds like `this + ""` for strings (B0255).

**Impact**: High for practical code. Any time you need a second owned copy of a non-Copy value, you're stuck. The `string.to_string()` hack (B0255) is a symptom of this gap.

### Gap 5: `Arc[T]` (Atomic Reference Counting) — NOT IMPLEMENTED

**Problem**: The design spec (§17.4) shows `Arc[T]` for sharing data across goroutines, but it's not implemented. There's no safe way to share heap data between goroutines without channels.

**Rust comparison**: `Arc<T>` is the backbone of safe concurrent data sharing in Rust. Without it, the only options are: move into goroutine (can't share), or use channels (copies/moves on each send).

**Impact**: Limits concurrent programming patterns. Any shared-state concurrency requires channel-based workarounds.

### Gap 6: No Interior Mutability Patterns — NOT DESIGNED

**Problem**: No equivalent to Rust's `Cell<T>`, `RefCell<T>`, `Mutex<T>`, `RwLock<T>`. These provide safe mutation through shared references — critical for shared-state concurrency and certain data structure patterns.

**Rust comparison**: Interior mutability is how Rust squares the circle of "shared XOR mutable" — you can share a `Mutex<T>` and still mutate the inner T safely.

**Impact**: High for concurrent applications. Without `Mutex[T]`, even with `Arc[T]`, there's no safe way to share mutable state.

### Gap 7: Unsafe Block Scope Too Narrow — MINIMAL

**Problem**: `unsafe` blocks only allow raw pointer types. They don't relax ownership rules, don't allow multiple mutable references, and don't provide FFI capabilities beyond pointer types.

**Rust comparison**: Rust's `unsafe` allows: dereferencing raw pointers, calling unsafe functions, accessing mutable statics, implementing unsafe traits, accessing union fields. Promise's `unsafe` is much more limited.

**Impact**: Low for most code. But blocks implementing low-level data structures (intrusive lists, arena allocators, lock-free structures) in pure Promise.

### Gap 8: No Weak References — NOT DESIGNED

**Problem**: No `Weak[T]` counterpart to `Arc[T]`. Without weak references, reference-counted cycles cause leaks.

**Rust comparison**: `Weak<Arc<T>>` prevents cycles. Critical for tree structures with parent pointers, observer patterns, caches.

**Impact**: Blocked by Gap 5 (Arc not implemented yet). Will become important once Arc exists.

### Gap 9: No Custom Allocator Support — NOT DESIGNED

**Problem**: All allocation goes through `pal_alloc`/`pal_free` (libc malloc/free). No arena allocators, pool allocators, or custom allocation strategies.

**Rust comparison**: Rust's allocator API allows `Box<T, A>`, `Vec<T, A>`, etc. Arena allocators (like `bumpalo`) are widely used for performance.

**Impact**: Performance ceiling for allocation-heavy workloads. Not a safety issue.

### Gap 10: String Ownership Model — TRANSITIONAL

**Problem**: Strings use a runtime bit 63 flag to distinguish literal (`.rodata`) vs heap-allocated. T0063 proposes a type-level distinction (like Rust's `&str` vs `String`). Current model works but has overhead:
- Every length read masks bit 63
- Every drop checks bit 63
- COW on vector mutation checks bit 63

**Rust comparison**: `&str` (borrowed slice) vs `String` (owned heap) is a compile-time distinction with zero runtime overhead.

**Impact**: Small runtime overhead per string operation. The bigger issue is that the type system can't express "this function borrows a string" vs "this function takes ownership of a string" at the string-content level (only at the reference level).

---

## 4. EFFICIENCY ASSESSMENT

### What's Already Efficient
- **Zero-alloc string/vector literals**: `.rodata` storage eliminates common allocations
- **Pure value types**: stack-allocated, no heap, no RTTI pointer in value struct
- **LTO**: whole-program optimization at link time (cross-module inlining, DCE)
- **Drop flags**: single `i1` per variable, branch-predicted well
- **Per-instance BC caching**: only recompile generic instances whose type definition changed
- **Deterministic cleanup**: no GC pauses, no finalizer queues

### Efficiency Gaps
1. **No move optimization for last-use**: Without NLL, values are sometimes copied where a move would suffice
2. **No placement new/RVO**: constructors always allocate then return; no guaranteed copy elision
3. **No small-string optimization (SSO)**: all non-literal strings heap-allocate regardless of size
4. **Bit 63 masking**: minor but measurable overhead on every string/vector length access
5. **No arena/bump allocation**: all short-lived objects go through malloc/free
6. **No escape analysis**: all non-value-type instances are heap-allocated even when they don't escape the current scope

---

## 5. PRIORITIZED ROADMAP TO RUST-CLASS MEMORY MANAGEMENT

### Tier 1: Safety-Critical (Required for Sound Memory Management)

1. **B0228 — Goroutine panic cleanup** (T0143–T0151)
   - Replace longjmp with TLS panic flag propagation
   - Without this, any panicking goroutine is a memory leak
   - Unblocks T0109 → T0066

2. **B0253/B0252 — JsonValue enum/map double-free and segfault**
   - Active correctness bugs in enum drop with complex nested types
   - Indicates potential for similar issues in other enum+map combinations

3. **T0066 completion — Zero-leak enforcement**
   - Remove remaining 20 `allow_leaks` tags
   - Enforce as hard test failure

### Tier 2: Completeness (Required for Practical Ownership Model)

4. **`clone` meta implementation**
   - Auto-generate `clone() Self` for `` `clone `` types
   - Fixes B0255 (string.to_string hack) as a side effect
   - Essential for any non-trivial program that needs copies

5. **B0035 — Non-lexical lifetimes**
   - Already in_progress
   - Last-use analysis to shrink borrow lifetimes
   - Major ergonomic improvement

6. **B0033 — Explicit lifetime annotations**
   - Grammar + sema + ownership support for `'a` syntax
   - Required for complex library APIs

7. **Arc[T] implementation**
   - Atomic reference counting for cross-goroutine sharing
   - Specified in language design but not implemented

### Tier 3: Power Features (Required for Full Rust-Class Capabilities)

8. **Interior mutability** (`Mutex[T]`, `RwLock[T]`, `Cell[T]`, `RefCell[T]`)
   - Safe mutation through shared references
   - Required for concurrent data structures

9. **Weak[T]** references
   - Prevent reference-counted cycles
   - Required alongside Arc[T]

10. **T0063 — Type-level string ownership**
    - Compile-time `&str` vs `String` distinction
    - Eliminates bit 63 runtime overhead

### Tier 4: Performance (Nice-to-Have for Competitive Efficiency)

11. **Escape analysis** — stack-allocate instances that don't escape scope
12. **Small-string optimization** — inline short strings in the value struct
13. **Custom allocator API** — arena/pool allocators for hot paths
14. **Return value optimization (RVO)** — guaranteed copy elision for constructors

---

## 6. COMPARISON SUMMARY: PROMISE vs RUST

| Feature | Rust | Promise | Gap |
|---------|------|---------|-----|
| Single ownership | Yes | Yes | None |
| Move semantics | Yes | Yes | None |
| Borrow checking (shared/mut) | Yes | Yes | None |
| Drop (destructors) | Yes | Yes | None |
| Copy trait | Yes | Yes (`copy` meta) | None |
| Clone trait | Yes (`#[derive(Clone)]`) | **Designed, not implemented** | Gap 4 |
| Non-lexical lifetimes | Yes (since 2021) | **In progress (B0035)** | Gap 2 |
| Explicit lifetime params | Yes (`'a`) | **Not implemented (B0033)** | Gap 3 |
| Panic unwinding + Drop | Yes | **Missing (B0228)** | **Gap 1 - Critical** |
| Arc/Rc (ref counting) | Yes | **Not implemented** | Gap 5 |
| Interior mutability | Yes (Cell/RefCell/Mutex) | **Not designed** | Gap 6 |
| Weak references | Yes | **Not designed** | Gap 8 |
| Unsafe (full) | Yes (5 capabilities) | Pointer types only | Gap 7 |
| Custom allocators | Yes (Allocator trait) | No | Gap 9 |
| Escape analysis | No (but stack by default) | No (heap by default) | Gap 12 |
| Zero-cost string literals | Via `&str` (type-level) | Via bit 63 (runtime flag) | Gap 10 |
| Scoped resource mgmt | No built-in (Drop only) | Yes (`use` bindings) | **Promise ahead** |
| Goroutine-safe ownership | Send/Sync traits | Move-only (no Send/Sync) | Partial |
| Deterministic test leak detection | No built-in | Yes (alloc count tracking) | **Promise ahead** |

### Bottom Line

Promise's memory management foundation is **solid and mostly complete** for single-threaded, non-panicking code paths. The ownership checker, drop infrastructure, and scope cleanup system handle the common cases correctly. The critical remaining gap is **goroutine panic cleanup (B0228)** — without it, concurrent programs have a fundamental memory safety hole. After that, the path to Rust-class capabilities requires implementing **clone, NLL, explicit lifetimes, and Arc[T]** — all designed but not yet built.
