# Source-Level Debugging — Design Proposal

## 1. Problem

Promise currently emits LLVM IR with no debug metadata. When a compiled binary is loaded in `lldb` or `gdb`, the debugger sees only raw assembly — no source file names, no line numbers, no variable names, no type information. This makes interactive debugging impossible: you cannot set breakpoints by line, inspect local variables by name, or step through Promise source code.

The infrastructure for source positions already exists — every AST node carries `Pos{File, Line, Column}` and `End` positions. This information is used by sema for error reporting but is discarded during codegen. The proposal is to thread it through to LLVM IR as DWARF debug metadata.

---

## 2. Background: How LLVM Debug Info Works

LLVM uses metadata nodes to carry debug information through the optimization pipeline. The key constructs:

- **`!DICompileUnit`** — one per source file, describes the compilation unit (language, file, producer)
- **`!DIFile`** — source file reference (filename + directory)
- **`!DISubprogram`** — one per function, carries name, linkage name, file, line, scope
- **`!DILocation`** — attached to instructions via `!dbg` metadata, carries line + column + scope
- **`!DILocalVariable`** — describes a local variable (name, type, scope, line)
- **`!DIBasicType`** / **`!DIDerivedType`** / **`!DICompositeType`** — type descriptions for the debugger
- **`@llvm.dbg.declare`** / **`@llvm.dbg.value`** — intrinsic calls that bind an alloca (or SSA value) to a `!DILocalVariable`

When `opt` runs with debug info present, it preserves `!dbg` attachments through transformations. The linker (via LTO) and `llc` emit DWARF sections (`.debug_info`, `.debug_line`, `.debug_abbrev`, etc.) into the final binary. Debuggers read these sections to map addresses back to source.

---

## 3. Constraint: `llir/llvm` Library

The compiler uses `github.com/llir/llvm v0.3.6`, a pure-Go LLVM IR library. This library:

- Supports named metadata (`!llvm.dbg.cu`) and metadata attachment on instructions
- Has `ir.InstCall` and other instruction types with a `Metadata` field
- Can emit metadata nodes via `ir.Module.NamedMetadataDefs`

However, it does **not** have high-level debug info builder APIs (no `DIBuilder` equivalent). Debug metadata must be constructed manually as raw metadata nodes and attached to instructions. This is feasible — DWARF metadata in LLVM IR is just structured metadata — but requires careful construction.

**Alternative**: If `llir/llvm` proves too limiting, an escape hatch is available: post-process the IR text output. After `module.String()` produces the `.ll` text, a pass can inject `!dbg` metadata lines and metadata definitions. This is crude but reliable — the IR text format is stable and well-documented.

---

## 4. Phased Implementation Plan

### Phase 1 — Line-Level Debug Info (High Impact, Medium Effort)

**Goal**: Breakpoints by file:line work in lldb/gdb. Stack traces show Promise source locations.

**What to implement**:

1. **`--debug` flag on `promise build` / `promise run` / `promise test`**
   - When present, enables debug info generation
   - Ensures `opt` preserves debug metadata (LLVM's `-O1` retains `!dbg` by default)
   - Skips `llvm-strip` in the link step
   - Debug builds still use `-O1` for `opt` — this is **required** because LLVM coroutine intrinsics (`@llvm.coro.begin`, `@llvm.coro.suspend`, etc.) are only lowered at `-O1` and above. Since Promise's M:N scheduler is built on LLVM coroutines, `-O0` would produce broken binaries. LLVM's `-O1` preserves debug metadata through transformations.
   - Without `--debug`, behavior is unchanged (no debug overhead)

2. **Compile unit and file metadata**
   - At codegen start, emit a `!DICompileUnit` for the main source file
   - For each unique `ast.Pos.File` encountered, emit a `!DIFile`
   - Language ID: use `DW_LANG_C99` (0x000c) initially — debuggers understand C types well. A custom `DW_LANG_lo_user`-range ID can be registered later
   - Producer string: `"promise <version>"`

3. **Subprogram metadata for every function**
   - When declaring each LLVM function, emit a `!DISubprogram` with:
     - `name`: the Promise function/method name (e.g., `greet`, `Dog.speak`)
     - `linkageName`: the mangled LLVM name (e.g., `Dog.speak`, `sort__int`)
     - `file`: the `!DIFile` for the function's source file
     - `line`: the function's declaration line from `ast.FuncDecl.Pos().Line`
     - `scope`: the `!DICompileUnit` (or parent `!DISubprogram` for nested functions)
     - `type`: a `!DISubroutineType` (can start with empty/placeholder types)

4. **Location metadata on instructions**
   - Thread `ast.Pos` through `genStmt()` and `genExpr()` — maintain a "current location" on the compiler context
   - At each statement boundary, update the current location from the statement's `Pos()`
   - Attach `!dbg !N` (referencing a `!DILocation`) to key instructions:
     - Function calls (most important — these are where breakpoints land)
     - Store instructions (variable assignments)
     - Branch instructions (control flow)
     - Return instructions
   - Not every instruction needs a location — LLVM propagates locations through unlabeled instructions

5. **IR text post-processing approach** (if `llir/llvm` metadata support is insufficient)
   - After `module.String()`, scan the output for function definitions and instruction patterns
   - Inject `!dbg` references and append metadata definitions at the end of the IR
   - This is a string transformation: reliable, testable, and decoupled from the IR library

**Result**: After Phase 1, a developer can:
```bash
promise build --debug main.pr -o main
lldb main
(lldb) breakpoint set --file main.pr --line 42
(lldb) run
(lldb) bt   # shows Promise source locations in backtrace
```

### Phase 2 — Variable Debug Info (Medium Impact, Medium Effort)

**Goal**: `lldb` shows local variable names and values when stopped at a breakpoint.

**What to implement**:

1. **`@llvm.dbg.declare` for local variables**
   - When emitting an `alloca` for a local variable (in `genVarDecl`), also emit:
     ```llvm
     call void @llvm.dbg.declare(metadata ptr %x, metadata !N, metadata !DIExpression())
     ```
   - Where `!N` is a `!DILocalVariable` with the Promise variable name, line, and type
   - This tells the debugger "the alloca at `%x` corresponds to the source variable named `x`"

2. **`@llvm.dbg.declare` for function parameters**
   - Same pattern for function parameters that are stored to allocas

3. **Basic type descriptions**
   - `!DIBasicType` for primitives: `int` → `{name: "int", size: 64, encoding: DW_ATE_signed}`
   - `f64` → `{name: "f64", size: 64, encoding: DW_ATE_float}`
   - `bool` → `{name: "bool", size: 8, encoding: DW_ATE_boolean}`
   - `string` → pointer type (details deferred to Phase 3)

4. **Pointer/reference types**
   - `!DIDerivedType(tag: DW_TAG_pointer_type, baseType: !N)` for borrowed/owned references

**Result**: After Phase 2:
```
(lldb) frame variable
(int) x = 42
(string) name = <pointer>   # full string display comes in Phase 3
(f64) pi = 3.14159
```

### Phase 3 — Rich Type Descriptions (Lower Impact, Higher Effort)

**Goal**: Debugger understands Promise's composite types — structs, enums, arrays.

**What to implement**:

1. **Struct types** (`!DICompositeType` with `DW_TAG_structure_type`)
   - Map Promise user types to DWARF struct descriptions
   - Each field → `!DIDerivedType(tag: DW_TAG_member, name: "field", type: !, offset: N)`
   - Handle the four-struct model: describe the Instance struct layout (what the debugger will see in memory), with the vtable/rtti pointers as hidden members

2. **Enum types** (`!DICompositeType` with `DW_TAG_union_type` or discriminated union)
   - Tag field + data payload
   - Variant names as enumerator values

3. **String type**
   - Describe the string representation (pointer + length) so the debugger can display string values
   - Consider a `.natvis` (MSVC) / `.lldbinit` type summary for pretty-printing

4. **Array/Vector/Map types**
   - Describe container layouts for debugger inspection
   - Pretty-printers for common container types

5. **Generic type names**
   - Monomorphized types should carry their full name: `Vector[int]` not `Vector__int`
   - Use `!DICompositeType` `name` field with the Promise-syntax name

**Result**: After Phase 3:
```
(lldb) frame variable user
(User) user = {
  name = "Alice"
  age = 30
  bio = none
}
(lldb) p scores["alice"]
(int) 100
```

### Phase 4 — Debugger Integration (Lower Priority)

**Goal**: Quality-of-life improvements for the debugging experience.

1. **LLDB type summaries and synthetic children**
   - Ship a `.lldbinit` or Python formatters that teach lldb how to display Promise types
   - String → show the string content, not raw pointer
   - `Option[T]` → show `some(value)` or `none`
   - `Vector[T]` → show elements as array
   - `map[K,V]` → show entries

2. **DAP (Debug Adapter Protocol) support**
   - Implement a thin DAP server wrapping lldb
   - Enables debugging in VS Code, Zed, and other DAP-compatible editors
   - `promise debug main.pr` launches the DAP server

3. **Source maps for inlined/monomorphized code**
   - When a generic function is monomorphized, the debug info should point back to the generic source
   - When a function is inlined, LLVM preserves the `!DILocation` chain — verify this works correctly with Promise's inlining

4. **Coroutine / goroutine debugging**
   - The M:N scheduler uses LLVM coroutines — suspended goroutines have split stacks
   - Extend debugger support to show goroutine state (requires custom lldb commands or a debug runtime)

---

## 5. Implementation Details

### 5.1 Compiler Context Additions

```go
// In compiler struct (codegen/compiler.go)
type compiler struct {
    // ... existing fields ...

    // Debug info state (nil when --debug not passed)
    debugInfo    *debugInfoBuilder
}

type debugInfoBuilder struct {
    diCompileUnit  int         // metadata ID for the compile unit
    diFiles        map[string]int  // file path → metadata ID
    diSubprograms  map[string]int  // function name → metadata ID
    diTypes        map[string]int  // type name → metadata ID
    nextMetaID     int
    metadataDefs   []string    // accumulated metadata definitions
    currentLoc     ast.Pos     // current source location for !dbg emission
}
```

### 5.2 Location Tracking in Codegen

The key change is threading source positions through code generation:

```go
// Called at every statement boundary
func (d *debugInfoBuilder) setLocation(pos ast.Pos) {
    d.currentLoc = pos
}

// Called after emitting key instructions
func (d *debugInfoBuilder) attachLocation(inst string) string {
    if !d.currentLoc.IsValid() {
        return inst
    }
    locID := d.getOrCreateLocation(d.currentLoc)
    return inst + fmt.Sprintf(", !dbg !%d", locID)
}
```

In `genStmt()`, before processing each statement:
```go
if c.debugInfo != nil {
    c.debugInfo.setLocation(stmt.Pos())
}
```

### 5.3 IR Post-Processing Approach

If direct metadata attachment via `llir/llvm` is too complex, the fallback is text-based injection:

1. Codegen emits IR normally via `module.String()`
2. A `debugInject(irText string, debugMeta *debugInfoBuilder) string` function:
   - Parses function definitions to find their names
   - Inserts `!dbg !N` on call/store/branch/ret instructions within each function
   - Appends all `!DICompileUnit`, `!DIFile`, `!DISubprogram`, `!DILocation` definitions at the end
   - Adds `!llvm.dbg.cu = !{!0}` named metadata
3. The modified IR text is written to the `.ll` file and passed to `opt`

This approach has the advantage of being entirely decoupled from the IR library — it's a pure text transformation that can be tested independently.

### 5.4 Build Flag Integration

```
promise build --debug main.pr          # debug build
promise build main.pr             # release build (no debug info, default)
promise run --debug main.pr            # debug build + run
promise test --debug tests/...         # debug build for tests
```

The `--debug` flag:
- Enables `debugInfoBuilder` creation in codegen
- Passes `-O1` to `opt` (required for coroutine lowering; preserves debug metadata)
- Skips any stripping in the link step
- Sets `DWARF_VERSION=5` metadata in the compile unit (DWARF5 is well-supported by modern lldb/gdb)

### 5.5 Impact on Caching

Debug builds should use a separate cache partition:
- Cache key includes whether `--debug` is set
- Debug `.bc` files are not mixed with release `.bc` files
- This is a simple boolean addition to `BuildCacheKey()`

### 5.6 Impact on Binary Size

Debug info significantly increases binary size (typically 2-5x). This is expected and acceptable:
- Debug builds are for development, not distribution
- `promise build` (without `--debug`) is unchanged
- A future `promise build --release` could add `-strip-debug` to LTO for explicit stripping

---

## 6. Testing Strategy

### Unit Tests (Go)

- **IR shape tests**: Verify that `--debug` mode emits `!dbg` metadata on instructions, `!DISubprogram` for functions, `!DICompileUnit` at module level
- **Location accuracy**: Verify that function declaration lines match AST positions
- **Variable mapping**: Verify `@llvm.dbg.declare` calls reference correct variable names

### Integration Tests

- **Breakpoint test**: Compile a simple program with `--debug`, run under lldb with a scripted breakpoint, verify it stops at the expected Promise source line
- **Backtrace test**: Trigger a panic, verify the stack trace shows Promise file:line locations
- **Variable inspection**: Stop at a breakpoint, verify `frame variable` shows correct local names and values

### Regression Tests

- **Optimization preservation**: Verify that debug info survives `opt -O1` without breaking semantics
- **Cache separation**: Verify debug and release builds produce different cache keys

---

## 7. Priorities and Dependencies

| Phase | Effort | Impact | Dependencies |
|-------|--------|--------|-------------|
| 1 — Line info | 2-3 weeks | High — enables breakpoints and stack traces | None |
| 2 — Variables | 1-2 weeks | Medium — enables variable inspection | Phase 1 |
| 3 — Rich types | 2-3 weeks | Medium — enables struct/enum inspection | Phase 2 |
| 4 — Debugger UX | Ongoing | Lower — quality of life | Phase 1+ |

**Phase 1 is the critical path.** It delivers the most value (breakpoints, backtraces) and unblocks all subsequent phases. The IR post-processing approach (Section 5.3) is recommended as the starting implementation — it avoids fighting the `llir/llvm` library and can be replaced later if the library adds metadata support.

---

## 8. Open Questions

1. **Language ID**: Should Promise register a custom DWARF language ID (`DW_LANG_lo_user + N`), or reuse `DW_LANG_C99`? The latter works out of the box with all debuggers; the former is more correct but requires debugger configuration.

2. **Optimization level for debug builds**: `-O1` is the minimum viable optimization level because LLVM coroutine lowering passes do not run at `-O0`. This means debug builds will have some instruction reordering and variable elimination compared to a true `-O0` build. In practice `-O1` preserves most debug info well — variables may occasionally be reported as `<optimized out>` at certain points. If finer control is needed, specific pass lists (coroutine lowering + minimal optimization) could be explored.

3. **Module debug info**: When compiling multi-module programs, each module's IR needs its own `!DICompileUnit`. The caching system must account for this — a module compiled with `--debug` and without `--debug` produces different `.bc` files.

4. **Coroutine split stacks**: LLVM coroutines split the stack frame across suspend points. Debug info for local variables that span a suspend point may be incorrect or missing. This needs investigation — it may require coroutine-aware debug metadata.

5. **WASM debugging**: WASM has its own debug format (DWARF-in-WASM). The `wasm-ld` linker handles this, but testing is needed to verify source maps work in browser DevTools and `wasmtime` debugger.

6. **`llir/llvm` metadata support**: The library's metadata capabilities need a spike to determine if direct metadata attachment is feasible, or if IR post-processing is the only viable path. This spike should be the first task before Phase 1 implementation begins.
