# Operator Method Dispatch Expansion

## Summary

Expand method-dispatched operators to eliminate remaining hardcoded operator handling in the compiler. This adds seven new operator method names and converts three existing hardcoded operators to method dispatch:

| Operator | Category | Status |
|----------|----------|--------|
| `[]`, `[]=` | Index read/write | **New** — replaces hardcoded array/slice/map indexing |
| `[:]`, `[:]=` | Slice read/write | **New** — new syntax + new operators |
| `++`, `--` | Increment/decrement | **New** — replaces hardcoded `+= 1` desugaring |
| `..`, `..=` | Range construction | **Change** — removes hardcoded int-only range check |
| `!` (unary) | Logical not | **Change** — removes hardcoded bool-only check |

All follow the same type-system-driven dispatch pattern as existing arithmetic operators (`+`, `-`, etc.). Any Named type can define any combination of these operators.

---

## Current State

**Single-index access** (`x[i]`) is supported via `IndexExpr` in the grammar and AST, but **hardcoded** in sema for three cases:

1. **Array**: `x[i]` → element type, index must be `int`
2. **Slice**: `x[i]` → element type, index must be `int`
3. **Map**: `x[key]` → `V?` (optional value), key must match `K`

This hardcoding means:
- User-defined types cannot opt into `[]` syntax
- No extension point for custom indexed containers (e.g., `Matrix[row, col]`, sorted sets)

**Slice syntax** (`x[a:b]`, `x[a:]`, `x[:b]`) does not exist — neither in the grammar nor in the AST. Range expressions (`a..b`, `a..=b`) exist but produce `range` values for iteration, not sub-collection extraction.

**Index assignment** (`x[i] = v`) works for maps in sema/codegen via special-cased logic, not method dispatch.

---

## Design

### Operator Method Names

Extend the `methodName` grammar rule with new operator symbols:

| Operator | Meaning | Signature Pattern |
|----------|---------|-------------------|
| `[]` | Index read | `(K key) V` |
| `[]=` | Index write | `(K key, V value)` |
| `[:]` | Slice read | `(int? start, int? end) Self` — optionals allow omission |
| `[:]=` | Slice write | `(int? start, int? end, Self value)` — same |
| `++` | Increment | `() Self` — returns incremented value |
| `--` | Decrement | `() Self` — returns decremented value |
| `..` | Exclusive range | `(Self end) RangeType` — already a binary op, now method-dispatched |
| `..=` | Inclusive range | `(Self end) RangeType` — same |
| `!` | Logical not | `() R` — already in grammar, sema hardcode removed |

These follow the same convention as `+`, `-`, `==`, etc. — they are method names that the compiler recognizes for operator dispatch.

### Syntax in Type Declarations

```promise
type slice[T] `native {
    get len int `native;

    // Single-element access
    [](int index) T `native;
    []=(int index, T value) `native;

    // Sub-slice access — optional params allow x[a:], x[:b], x[:]
    [:](int? start, int? end) slice[T] `native;
    [:]=(int? start, int? end, slice[T] value) `native;

    push(T elem) `native;
    pop() T? `native;
    // ...
}

type string `native {
    get len int `native;

    [](int index) char `native;
    [:](int? start, int? end) string `native;

    // no []= or [:]= — strings are immutable
    // ...
}

type map[K, V] `native {
    get len int `native;

    [](K key) V? `native;
    []=(K key, V value) `native;

    // no [:] or [:]= — maps are unordered
    // ...
}
```

User-defined types can define any combination. The parameter types control what the caller can omit:

```promise
type Matrix {
    int rows;
    int cols;
    f64[] data `instance;

    // Custom index: matrix[i] returns row i as a slice
    [](int row) f64[] {
        return this.data[row * this.cols : (row + 1) * this.cols];
    }

    // Required bounds — m[1:3] works, m[1:] is a compile error
    [:](int row_start, int row_end) Matrix {
        // return sub-matrix of rows [row_start, row_end)
    }
}

type Text {
    string content `instance;

    // Optional bounds — t[1:], t[:5], t[:] all work
    [:](int? start, int? end) Text {
        int s = start ?: 0;
        int e = end ?: this.content.len;
        return Text { content: this.content[s:e] };
    }
}
```

### Desugaring Rules

The compiler transforms bracket expressions into method calls during semantic analysis:

**Index expressions** (already parsed as `IndexExpr`):

| Expression | Desugars to |
|------------|-------------|
| `x[i]` (read) | `x.[](i)` — calls `[]` method on target type |
| `x[i] = v` | `x.[]=(i, v)` — calls `[]=` method on target type |
| `x[i] += v` | `x.[]=(i, x.[](i) + v)` — load, compute, store |

**Slice expressions** (new `SliceExpr` node):

| Expression | Desugars to |
|------------|-------------|
| `x[a:b]` | `x.[:](a, b)` |
| `x[a:]` | `x.[:](a, none)` |
| `x[:b]` | `x.[:](none, b)` |
| `x[:]` | `x.[:](none, none)` |
| `x[a:b] = v` | `x.[:]=(a, b, v)` |

Omitted bounds pass `none` to the corresponding parameter. The **operator method's signature** controls whether omission is allowed:

- If `start` is `int?` (optional), the caller can write `x[:b]`. The method receives `none` and handles the default internally (typically `0`).
- If `start` is `int` (required), the caller **must** provide it — `x[:b]` is a compile error: *"slice start is required for type T"*.
- Same logic applies to `end`.

This keeps the compiler simple (no knowledge of `len` or default values) and gives types full control over their slicing semantics.

### Backward Compatibility: Builtin Container Dispatch

Currently `checkIndexExpr` hardcodes behavior for Array, Slice, and Map. After this change:

1. **Slice/Map/String**: The `[]`/`[]=`/`[:]`/`[:]=` methods are defined in `std/*.pr` files (shown above). Sema resolves them through normal method lookup — **no hardcoding needed**.

2. **Array** (`T[N]`): Fixed-size arrays are not Named types — they're structural. They keep their hardcoded sema logic. Alternatively, we can define `[]` and `[:]` on Array in the type system directly (similar to how operators are defined on primitives). For 8a simplicity, keep the hardcoded path for Array and migrate later.

3. **Generic instantiation**: `Type[Arg]` (e.g., `Slice[int]`) is distinguished from indexing by sema checking whether the target is a generic type with type parameters. This check already exists and continues to work — the colon in `x[a:b]` further disambiguates, since generic instantiation never uses `:`.

---

## Grammar Changes

### Lexer — No Changes

All needed tokens already exist: `LBRACKET`, `RBRACKET`, `COLON`, `ASSIGN`.

### Parser

**1. Add `sliceExpr` to the expression rule:**

```antlr
expression
    // Precedence 1: Postfix
    : expression DOT IDENT                                     # memberAccessExpr
    | expression QUESTION_DOT IDENT                            # optionalChainExpr
    | expression LPAREN args RPAREN                            # callExpr
    | expression LBRACKET expression RBRACKET                  # indexExpr
    | expression LBRACKET expression? COLON expression? RBRACKET  # sliceExpr
    // ... rest unchanged
    ;
```

The `sliceExpr` alternative is distinguished from `indexExpr` by the presence of `COLON`. ANTLR4 tries alternatives in order, so `sliceExpr` (with COLON) must come **after** `indexExpr` (without COLON) — ANTLR4's adaptive prediction will correctly route `x[i]` to `indexExpr` and `x[a:b]` / `x[a:]` / `x[:b]` / `x[:]` to `sliceExpr`.

**2. Extend `methodName` with subscript/slice operators:**

```antlr
methodName
    : IDENT
    | PLUS | MINUS | STAR | SLASH | PERCENT
    | EQ | NEQ | LT | GT | LTE | GTE
    | AND | OR | BANG
    | LBRACKET RBRACKET                          // []
    | LBRACKET RBRACKET ASSIGN                   // []=
    | LBRACKET COLON RBRACKET                    // [:]
    | LBRACKET COLON RBRACKET ASSIGN             // [:]=
    ;
```

---

## AST Changes

### New Node: `SliceExpr`

```go
// SliceExpr represents a slice expression: target[start:end].
// Start and End are optional (nil when omitted).
type SliceExpr struct {
    nodeBase
    Target Expr // the collection being sliced
    Start  Expr // nil for [:end] and [:]
    End    Expr // nil for [start:] and [:]
}

func (*SliceExpr) exprTag() {}
```

### Builder Changes

In `builder.go`, add `visitSliceExpr` to construct `SliceExpr` from the parse tree. The `expression?` optionals map to `nil` in the AST when absent.

---

## Sema Changes

### 1. `checkIndexExpr` — Dispatch to Method

Replace the hardcoded logic with method lookup:

```go
func (c *Checker) checkIndexExpr(e *ast.IndexExpr) types.Type {
    target := c.checkExpr(e.Target)

    // Keep: Generic instantiation check (Type[Arg])
    // ... existing generic instantiation logic unchanged ...

    index := c.checkExpr(e.Index)

    // NEW: Look up [] operator method on the target type
    if named := extractNamed(target); named != nil {
        if m := named.LookupMethod("[]"); m != nil {
            // Validate index type matches first parameter
            sig := m.Sig()
            if len(sig.Params()) >= 1 {
                paramType := sig.Params()[0].Type()
                if !types.AssignableTo(index, paramType) {
                    c.errorf(e.Index.Pos(), "index type mismatch: expected %s, got %s", paramType, index)
                }
            }
            return sig.Result()
        }
    }

    // Fallback: Array (structural, not Named)
    if arr, ok := target.(*types.Array); ok {
        if index != nil && !types.Identical(index, types.TypInt) {
            c.errorf(e.Index.Pos(), "array index must be int, got %s", index)
        }
        return arr.Elem()
    }

    c.errorf(e.Pos(), "type %s does not support indexing (no [] method)", target)
    return nil
}
```

### 2. New: `checkSliceExpr`

```go
func (c *Checker) checkSliceExpr(e *ast.SliceExpr) types.Type {
    target := c.checkExpr(e.Target)

    // Look up [:] operator method on the target type
    named := extractNamed(target)
    if named == nil {
        c.errorf(e.Pos(), "type %s does not support slicing", target)
        return nil
    }
    m := named.LookupMethod("[:]")
    if m == nil {
        c.errorf(e.Pos(), "type %s does not support slicing (no [:] method)", target)
        return nil
    }

    sig := m.Sig()
    params := sig.Params() // [0]=start, [1]=end

    // Check start: present → validate type; absent → param must be optional
    if e.Start != nil {
        startType := c.checkExpr(e.Start)
        expected := params[0].Type()
        // Unwrap optional for matching: if param is int?, accept int
        if opt, ok := expected.(*types.Optional); ok {
            expected = opt.Elem()
        }
        if !types.AssignableTo(startType, expected) {
            c.errorf(e.Start.Pos(), "slice start type mismatch: expected %s, got %s", expected, startType)
        }
    } else {
        // Start omitted — param must be optional
        if _, ok := params[0].Type().(*types.Optional); !ok {
            c.errorf(e.Pos(), "slice start is required for type %s", named)
        }
    }

    // Check end: same logic
    if e.End != nil {
        endType := c.checkExpr(e.End)
        expected := params[1].Type()
        if opt, ok := expected.(*types.Optional); ok {
            expected = opt.Elem()
        }
        if !types.AssignableTo(endType, expected) {
            c.errorf(e.End.Pos(), "slice end type mismatch: expected %s, got %s", expected, endType)
        }
    } else {
        if _, ok := params[1].Type().(*types.Optional); !ok {
            c.errorf(e.Pos(), "slice end is required for type %s", named)
        }
    }

    return sig.Result()
}
```

### 3. Update `checkAssignStmt`

Add cases for `IndexExpr` and `SliceExpr` targets:

```go
// In checkAssignStmt, when target is IndexExpr:
if idx, ok := s.Target.(*ast.IndexExpr); ok {
    targetType := c.info.Types[idx.Target]
    named := extractNamed(targetType)
    if named != nil {
        m := named.LookupMethod("[]=")
        if m == nil {
            c.errorf(s.Pos(), "type %s does not support index assignment (no []= method)", targetType)
        }
        // Validate value type against second parameter
    }
}

// When target is SliceExpr:
if sl, ok := s.Target.(*ast.SliceExpr); ok {
    targetType := c.info.Types[sl.Target]
    named := extractNamed(targetType)
    if named != nil {
        m := named.LookupMethod("[:]=")
        if m == nil {
            c.errorf(s.Pos(), "type %s does not support slice assignment (no [:]= method)", targetType)
        }
        // Validate value type against third parameter
    }
}
```

---

## Codegen Changes

### 1. `genIndexExpr` — Emit Method Call

Replace the hardcoded slice/map index logic with method dispatch:

```go
func (c *Compiler) genIndexExpr(e *ast.IndexExpr) value.Value {
    targetType := c.info.Types[e.Target]
    named := extractNamed(targetType)

    if named != nil {
        if m := named.LookupMethod("[]"); m != nil {
            if m.IsNative() {
                return c.emitNativeIndex(named, e)
            }
            return c.emitMethodCall(named, "[]", target, index)
        }
    }

    // Fallback for Array (structural)
    return c.genArrayIndex(e)
}
```

### 2. New: `genSliceExpr`

```go
func (c *Compiler) genSliceExpr(e *ast.SliceExpr) value.Value {
    target := c.genExpr(e.Target)
    named := extractNamed(c.info.Types[e.Target])
    m := named.LookupMethod("[:]")
    sig := m.Sig()

    // Generate start argument
    var start value.Value
    if e.Start != nil {
        val := c.genExpr(e.Start)
        // Wrap in optional if param type is T?
        if _, ok := sig.Params()[0].Type().(*types.Optional); ok {
            start = c.wrapOptionalPresent(val)
        } else {
            start = val
        }
    } else {
        // Omitted → pass none (already validated as optional by sema)
        start = c.genNone(sig.Params()[0].Type())
    }

    // Generate end argument (same logic)
    var end value.Value
    if e.End != nil {
        val := c.genExpr(e.End)
        if _, ok := sig.Params()[1].Type().(*types.Optional); ok {
            end = c.wrapOptionalPresent(val)
        } else {
            end = val
        }
    } else {
        end = c.genNone(sig.Params()[1].Type())
    }

    // Dispatch to [:] method (native or user-defined)
    if m.IsNative() {
        return c.emitNativeSlice(named, target, start, end)
    }
    return c.emitMethodCall(named, "[:]", target, start, end)
}
```

### 3. Index/Slice Assignment Codegen

In `genAssignStmt`, add branches for `IndexExpr` and `SliceExpr` targets that dispatch to `[]=` and `[:]=` methods respectively.

### 4. Native Emitters

Add native emitters for the built-in container operations:

| Type | Operator | Runtime Call |
|------|----------|-------------|
| `slice[T]` | `[]` | `promise_slice_get(slice, index)` |
| `slice[T]` | `[]=` | `promise_slice_set(slice, index, value)` |
| `slice[T]` | `[:]` | `promise_slice_sub(slice, opt_start, opt_end)` |
| `slice[T]` | `[:]=` | `promise_slice_sub_set(slice, opt_start, opt_end, value)` |
| `string` | `[]` | `promise_string_char_at(str, index)` |
| `string` | `[:]` | `promise_string_sub(str, opt_start, opt_end)` |
| `map[K,V]` | `[]` | `promise_map_get(map, key)` |
| `map[K,V]` | `[]=` | `promise_map_set(map, key, value)` |

---

## Runtime Changes

Add to `runtime_slice.c`:

```c
// Sub-slice: start/end are optionals { i1 present, i64 value }.
// If start is absent → 0. If end is absent → slice length.
// The runtime handles the defaults, not the compiler.
promise_slice_i* promise_slice_sub(promise_slice_i* s, promise_opt_int start, promise_opt_int end);

// Sub-slice assignment
void promise_slice_sub_set(promise_slice_i* s, promise_opt_int start, promise_opt_int end, promise_slice_i* value);
```

Add to `runtime_string.c`:

```c
// Character at index (returns Unicode codepoint)
int32_t promise_string_char_at(promise_string_i* s, int64_t index);

// Substring — same optional pattern as slice
promise_string_i* promise_string_sub(promise_string_i* s, promise_opt_int start, promise_opt_int end);
```

The optional integer type in C:

```c
typedef struct { int8_t present; int64_t value; } promise_opt_int;
```

This matches the LLVM layout for `int?`: `{ i1, i64 }` (padded to struct alignment).

---

## Std Library Updates

### `std/slice.pr`

```promise
type slice[T] `native {
    get len int `native;

    [](int index) T `native;
    []=(int index, T value) `native;
    [:](int? start, int? end) slice[T] `native;   // x[1:3], x[1:], x[:3], x[:]
    [:]=(int? start, int? end, slice[T] value) `native;

    push(T elem) `native;
    pop() T? `native;
    contains(T elem) bool `native;
    remove(int index) `native;

    get is_empty bool => this.len == 0;
}
```

### `std/string.pr`

```promise
type string `native {
    get len int `native;

    [](int index) char `native;
    [:](int? start, int? end) string `native;   // s[0:5], s[3:], s[:5], s[:]
    // No []= or [:]= — strings are immutable

    +(string other) string `native;
    ==(string other) bool `native;
    // ... rest unchanged
}
```

### `std/map.pr`

```promise
type map[K, V] `native {
    get len int `native;

    [](K key) V? `native;
    []=(K key, V value) `native;
    // No [:] or [:]= — maps are unordered

    contains(K key) bool `native;
    remove(K key) bool `native;
    keys() K[] `native;
    values() V[] `native;

    get is_empty bool => this.len == 0;
}
```

---

## Implementation Order

1. **Grammar**: Add `sliceExpr` to expression rule, extend `methodName` with `[]`, `[]=`, `[:]`, `[:]=`
2. **Lexer**: No changes needed (all tokens exist)
3. **AST**: Add `SliceExpr` node
4. **Builder**: Parse tree → `SliceExpr` AST conversion
5. **Types**: Ensure `LookupMethod` works for `[]`, `[]=`, `[:]`, `[:]=` names
6. **Std library**: Add operator methods to `std/slice.pr`, `std/string.pr`, `std/map.pr`
7. **Sema**: Refactor `checkIndexExpr` to use method dispatch; add `checkSliceExpr`; update `checkAssignStmt` for index/slice setter dispatch
8. **Codegen**: Refactor `genIndexExpr` to use method dispatch; add `genSliceExpr`; update `genAssignStmt` for `[]=`/`[:]=` dispatch
9. **Runtime**: Add `promise_slice_sub`, `promise_string_char_at`, `promise_string_sub` functions
10. **Tests**: Parser tests for slice syntax, sema tests for type checking, codegen tests for dispatch, e2e tests

---

## Test Cases

```promise
// Slice indexing and slicing
items := [1, 2, 3, 4, 5];
print(items[0]);        // 1
print(items[4]);        // 5

sub := items[1:4];      // [2, 3, 4]
head := items[:3];      // [1, 2, 3]
tail := items[2:];      // [3, 4, 5]
copy := items[:];       // [1, 2, 3, 4, 5]

items[0] = 10;          // index assignment
items[1:3] = [20, 30];  // slice assignment

// String slicing
s := "hello world";
c := s[0];              // 'h'
sub := s[0:5];          // "hello"
rest := s[6:];          // "world"

// Map indexing
m := {"a": 1, "b": 2};
v := m["a"];             // 1 (as int?)
m["c"] = 3;             // index assignment

// User-defined type with custom [] operator
type Grid {
    int width;
    int height;
    int[] cells `instance;

    [](int index) int {
        return this.cells[index];
    }
}
```

---

## Edge Cases

1. **Negative indices**: Not supported in this proposal. Types can add separate methods like `last()` or handle negative values in their `[]` implementation.

2. **Out-of-bounds**: Handled by the runtime (bounds checks in native implementations). User-defined `[]` methods handle this themselves.

3. **Chained slicing**: `x[1:5][0:2]` — works naturally since `x[1:5]` returns a slice, and `[0:2]` is another slice operation on the result.

4. **Compound assignment**: `x[i] += v` desugars to `x.[]=(i, x.[](i) + v)` — two method calls (read then write). The sema validates both `[]` and `[]=` exist, plus the `+` operator on the element type.

5. **Optional unwrap on map index**: `m["key"]` returns `V?`. The existing optional handling applies — user must unwrap with `!`, `?`, or `?:`.

6. **Generic types**: `[]` and `[:]` methods on generic types (like `slice[T]`) work through normal generic instantiation. The return type is resolved after substitution.

---

## Additional Operators to Method-Dispatch

Three other operators are currently hardcoded and should be converted to method dispatch for consistency.

### 1. Increment/Decrement (`++` / `--`)

**Current state**: The AST builder desugars `x++` into `AssignStmt { target: x, op: +=, value: IntLit{1} }` — i.e., `x += 1`. This hardcodes `int` literal `1` as the operand, so `++`/`--` only work on types that define `+(int) Self`.

**Problem**: A `Date` type with `+(Duration) Date` cannot use `date++` because `1` is `int`, not `Duration`. Similarly, an iterator, pointer, or cursor type has no natural `+ 1` semantics.

**Change**: Make `++` and `--` operator methods on Named types.

**Grammar**: Add `PLUSPLUS` and `MINUSMINUS` to `methodName`:

```antlr
methodName
    : IDENT
    | PLUS | MINUS | STAR | SLASH | PERCENT
    | EQ | NEQ | LT | GT | LTE | GTE
    | AND | OR | BANG
    | PLUSPLUS | MINUSMINUS                       // NEW
    | LBRACKET RBRACKET | LBRACKET RBRACKET ASSIGN
    | LBRACKET COLON RBRACKET | LBRACKET COLON RBRACKET ASSIGN
    ;
```

**Std library**: Add to numeric types (int, i8, ..., f32, f64):

```promise
type int `native {
    // ... existing operators ...
    ++() int `native;   // returns this + 1
    --() int `native;   // returns this - 1
}
```

**Desugaring**: The AST builder changes from:

```go
// OLD: x++ → x += 1
&AssignStmt{Target: x, Op: OpAddAssign, Value: &IntLit{Raw: "1"}}

// NEW: x++ → x = x.++()
&AssignStmt{Target: x, Op: OpAssign, Value: &CallExpr{Callee: &MemberExpr{Target: x, Field: "++"}, Args: nil}}
```

Or more simply, introduce a new AST node `IncDecExpr` that sema resolves via method lookup:

```go
// Alternative: keep IncDecStmt as its own AST node
type IncDecStmt struct {
    nodeBase
    Target Expr
    IsInc  bool // true for ++, false for --
}
```

Sema checks that `target.Type().LookupMethod("++")` (or `"--"`) exists and returns the same type. Codegen calls the method and stores the result back.

**User-defined example**:

```promise
type Date {
    int days_since_epoch `instance;

    ++() Date => Date { days_since_epoch: this.days_since_epoch + 1 };
    --() Date => Date { days_since_epoch: this.days_since_epoch - 1 };
}

// Usage:
Date d = today();
d++;  // next day
d--;  // previous day
```

**Native codegen**: For `CatSignedInt`/`CatUnsignedInt`, `++` emits `add x, 1`. For `CatFloat`, `fadd x, 1.0`. Added to the native emitter table alongside existing operators.

---

### 2. Range Operators (`..` / `..=`)

**Current state**: Hardcoded in sema (`expr.go:246-253`) — both operands must be `int`, result is always `types.TypRange`. Codegen constructs a hardcoded `{ i64 start, i64 end, i1 inclusive }` struct.

**Problem**: Cannot create ranges over other types (chars, dates, custom iterators). `'a'..'z'` or `date1..date2` don't work even though the concept is natural.

**Change**: Make `..` and `..=` operator methods on Named types.

**Grammar**: Add `DOTDOT` and `DOTDOTEQ` to `methodName`:

```antlr
methodName
    : IDENT
    | PLUS | MINUS | STAR | SLASH | PERCENT
    | EQ | NEQ | LT | GT | LTE | GTE
    | AND | OR | BANG
    | PLUSPLUS | MINUSMINUS
    | DOTDOT | DOTDOTEQ                          // NEW
    | LBRACKET RBRACKET | LBRACKET RBRACKET ASSIGN
    | LBRACKET COLON RBRACKET | LBRACKET COLON RBRACKET ASSIGN
    ;
```

**Std library**: Add to `std/int.pr` (and other integer types):

```promise
type int `native {
    // ... existing operators ...
    ..(int end) range `native;     // exclusive range: 0..10
    ..=(int end) range `native;    // inclusive range: 0..=9
}
```

Add to `std/char.pr`:

```promise
type char `native {
    // ... existing operators ...
    ..(char end) char_range `native;
    ..=(char end) char_range `native;
}
```

**Sema change**: Replace the hardcoded case in `checkBinaryExpr`:

```go
// OLD:
case ast.BinExclusiveRange, ast.BinInclusiveRange:
    if !types.Identical(left, types.TypInt) { ... }
    if !types.Identical(right, types.TypInt) { ... }
    return types.TypRange

// NEW: dispatch via method lookup like other operators
case ast.BinExclusiveRange:
    return c.checkOperator(e.Pos(), left, "..", right)
case ast.BinInclusiveRange:
    return c.checkOperator(e.Pos(), left, "..=", right)
```

**Codegen**: For native `int` ranges, the emitter constructs the same `{ start, end, inclusive }` struct as before — just routed through the native dispatch table instead of hardcoded. The return type comes from the method signature, so `char..char` can return a different type than `int..int`.

**User-defined example**:

```promise
type Date {
    int days_since_epoch `instance;

    ..(Date end) DateRange {
        return DateRange { start: this, end: end, inclusive: false };
    }
    ..=(Date end) DateRange {
        return DateRange { start: this, end: end, inclusive: true };
    }
}

// Usage:
for d in start_date..end_date {
    // iterate over dates (if DateRange implements stream[Date])
}
```

---

### 3. Unary Not (`!`)

**Current state**: Sema hardcodes a `bool`-only check (`expr.go:317-321`). Codegen already dispatches through the native ops table. `std/bool.pr` already defines `!() bool \`native`.

**Problem**: User types that represent boolean-like values (e.g., bitfields, permission flags, custom logic types) cannot define `!` even though `bool` already declares it as a method.

**Change**: Replace the hardcoded bool check with method lookup (same pattern as unary `-`).

**Sema change**:

```go
// OLD:
case ast.UnaryNot:
    if !types.Identical(operand, types.TypBool) {
        c.errorf(e.Pos(), "operator ! requires bool, got %s", operand)
    }
    return types.TypBool

// NEW: dispatch via method lookup (same as UnaryNeg)
case ast.UnaryNot:
    return c.checkUnaryOperator(e.Pos(), operand, "!")
```

**Codegen**: Already works — the native emitter for `CatBool` + `!` emits `xor x, 1`. No change needed. User-defined types would go through normal method dispatch.

**Std library**: Already correct — `std/bool.pr` defines `!() bool \`native`.

**User-defined example**:

```promise
type Permission {
    u32 flags `instance;

    !() Permission {
        return Permission { flags: ~this.flags };
    }
}

// Usage:
Permission write_only = !read_only;
```

**No grammar change needed** — `BANG` is already in `methodName`.

---

## Updated Implementation Order

1. **Grammar**: Add `sliceExpr` to expression rule; extend `methodName` with `[]`, `[]=`, `[:]`, `[:]=`, `PLUSPLUS`, `MINUSMINUS`, `DOTDOT`, `DOTDOTEQ`
2. **Lexer**: No changes needed (all tokens exist)
3. **AST**: Add `SliceExpr` node; add `IncDecStmt` node (or keep current desugaring with method call)
4. **Builder**: Parse tree → `SliceExpr`; update `VisitIncDecStmt` to desugar to method call
5. **Types**: Ensure `LookupMethod` works for all new operator names
6. **Std library**: Add `[]`/`[]=`/`[:]`/`[:]=` to slice/string/map; add `++`/`--` to numeric types; add `..`/`..=` to int/char
7. **Sema**: Refactor `checkIndexExpr` to method dispatch; add `checkSliceExpr`; change `checkUnaryExpr` for `!` to use method lookup; change range operators to use method lookup; add `checkIncDecStmt` with method lookup; update `checkAssignStmt`
8. **Codegen**: Refactor all corresponding codegen paths; add native emitters for `++`/`--` and `..`/`..=`
9. **Runtime**: Add slice/string sub-range functions
10. **Tests**: Full coverage for all new operator dispatch paths
