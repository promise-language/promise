# Documentation System Proposal

## Motivation

Promise is designed for AI-agent efficiency. Documentation is the primary way an agent learns what a module does without reading every line of source. The `doc()` meta tag already exists on all major constructs — types, fields, methods, functions, enums. What's missing is **tooling to extract and present it** in a form agents can consume.

An AI agent working with a Promise codebase needs to answer: *"What does this module export, and how do I use each thing?"* The answer should come from a single command, not from reading source files and mentally filtering out implementation details.

## Design Principles

1. **One command, full picture** — `promise doc` prints everything an agent needs to use a module
2. **Stdout-first** — agents read stdout; file output is optional
3. **Declaration-centric** — show signatures with doc strings, not prose narratives
4. **No duplication** — doc strings live in source only; tooling extracts, never copies
5. **Structured enough to parse, readable enough for humans** — markdown with consistent heading hierarchy

## The `promise doc` Command

### Basic Usage

```bash
# Document a single file
promise doc server.pr

# Document a module directory
promise doc ./networking/

# Document recursively
promise doc ./networking/...

# Document installed module
promise doc io

# Write to file instead of stdout
promise doc -o api.md ./networking/

# Document only public API (exported symbols)
promise doc -public ./networking/

# Include private symbols too (default for single files)
promise doc -all ./networking/
```

### Compiler Pipeline Position

`promise doc` runs the compiler frontend through the **first two sema passes** (Declare + Define) — enough to resolve types, signatures, inheritance, doc strings, and generic constraints. It does **not** run the Check or Verify passes, and does not run ownership analysis or codegen. This means `promise doc` is fast and tolerates incomplete method bodies (useful for documenting work-in-progress code).

### Output Format

Given this source file:

```promise
type Resource {
    string name;

    drop(~this) `doc("Release underlying handle.") {
        // cleanup
    }
}

type HttpClient is Resource
    `doc("HTTP client with connection pooling and retry support.") `public {

    int maxRetries `doc("Maximum retry attempts before failing.") `public;
    int timeout `doc("Request timeout in milliseconds.") `public;
    string baseUrl;

    new(~this, string baseUrl, int maxRetries = 3, int timeout = 5000) `public {
        this.baseUrl = baseUrl;
        this.maxRetries = maxRetries;
        this.timeout = timeout;
    }

    get(this, string url `doc("Relative URL path.")) Response!
        `doc("Perform a GET request. Raises on network failure or non-2xx status.")
        `public {
        // ...
    }

    post(this, string url, Bytes body) Response!
        `doc("Perform a POST request with the given body.")
        `public {
        // ...
    }

    close(~this) `doc("Close connection pool and release resources.") `public {
        // ...
    }

    get is_connected bool `doc("Whether the client has an active connection.") `public
        => this.baseUrl.len > 0;
}

type Parseable `structural `doc("Types that can be constructed from a string representation.") `public {
    parse(string data) Self! `abstract `factory;
}

enum Result[T] `doc("Outcome of an operation that may fail.") `public {
    Ok(T value) `doc("Successful result carrying the value."),
    Err(string message) `doc("Failure with an error message."),
}

enum HttpMethod `doc("Standard HTTP methods.") `copy `public {
    GET,
    POST,
    PUT,
    DELETE,
    PATCH,
}

retryWithBackoff[T: Parseable](int attempts, f64 baseDelay, string data) T!
    `doc("""Retry parsing with exponential backoff.
Doubles the delay after each failed attempt.
Raises the last error on exhaustion.""")
    `public {
    // ...
}
```

The output of `promise doc -public server.pr`:

    # server.pr

    ## Types

    ### HttpClient is Resource

    HTTP client with connection pooling and retry support.

        type HttpClient is Resource {
            int maxRetries    — Maximum retry attempts before failing.
            int timeout       — Request timeout in milliseconds.

            new(~this, string baseUrl, int maxRetries = 3, int timeout = 5000)
            get(this, string url) Response!
            post(this, string url, Bytes body) Response!
            close(~this)
            drop(~this)

            get is_connected bool
        }

    #### HttpClient.new

        new(~this, string baseUrl, int maxRetries = 3, int timeout = 5000)

    #### HttpClient.get

    Perform a GET request. Raises on network failure or non-2xx status.

        get(this, string url) Response!

    Parameters:
    - `url` — Relative URL path.

    #### HttpClient.post

    Perform a POST request with the given body.

        post(this, string url, Bytes body) Response!

    #### HttpClient.close

    Close connection pool and release resources.

        close(~this)

    #### HttpClient.drop

    Release underlying handle. Called automatically at scope exit.

        drop(~this)

    #### HttpClient.is_connected (getter)

    Whether the client has an active connection.

        get is_connected bool

    ---

    ### Parseable `structural

    Types that can be constructed from a string representation.

    Structural interface — types satisfy this by implementing the required methods,
    without an explicit `is` declaration.

        type Parseable `structural {
            parse(string data) Self! `abstract `factory
        }

    #### Parseable.parse `factory

        parse(string data) Self! `factory

    ## Enums

    ### Result[T]

    Outcome of an operation that may fail.

    Variants:
    - `Ok(T value)` — Successful result carrying the value.
    - `Err(string message)` — Failure with an error message.

    ### HttpMethod `copy

    Standard HTTP methods.

    Variants: `GET`, `POST`, `PUT`, `DELETE`, `PATCH`

    ## Functions

    ### retryWithBackoff[T: Parseable]

    Retry parsing with exponential backoff.
    Doubles the delay after each failed attempt.
    Raises the last error on exhaustion.

        retryWithBackoff[T: Parseable](int attempts, f64 baseDelay, string data) T!

### Output Rules

**Types:**

1. **Heading shows inheritance** — `### HttpClient is Resource` makes the parent chain immediately visible. Multiple parents shown comma-separated: `### Widget is Drawable, Serializable`.
2. **Type summary block** — indented code block showing fields (public only in `-public` mode, with doc strings after `—`), method signatures (no bodies), getters, and `new`/`drop`. This is the "at a glance" view.
3. **Operators** — listed in a compact line after the summary block: `Operators: ==, !=, <, >, +`. Only shown if the type defines operators. An agent needs to know if a type supports comparison or arithmetic, but doesn't need per-operator sections.
4. **Per-method sections** — generated when the method has a doc string or documented parameters. Undocumented methods appear only in the summary block.
5. **`new()` constructor** — shown in the summary block. `new` has implicit Self return (not printed). If the type has no explicit `new()` but has fields, the auto-generated constructor signature is shown.
6. **`drop(~this)` destructor** — shown in the summary block. Per-method section adds "Called automatically at scope exit." to whatever doc string is provided. Agents must know a type is droppable because it affects ownership — you can't copy it, and it's consumed on move.
7. **Getters** — shown with `get` prefix in summary block and labeled "(getter)" in per-method sections. Getters are accessed like fields at call sites (`obj.is_connected`) but defined as methods.
8. **Factory methods** — marked with `` `factory `` in signatures.

**Enums:**

9. **Flat variants** — listed inline: `Variants: GET, POST, PUT, DELETE, PATCH`
10. **Payload variants** — listed as a bullet list showing field types and names. If a variant has a `doc()` annotation, the doc string follows after `—`.
11. **Enum methods** — shown the same way as type methods (summary block + per-method sections).

**Functions:**

12. **Generic constraints in headings** — `### retryWithBackoff[T: Parseable]` shows the constraint. Multiple constraints use `+`: `[T: Hashable + Equal]`. An agent needs constraints to know what operations are available on type parameters.

**General:**

13. **Signatures use source syntax** — not a separate IDL. An agent that can write Promise can read these signatures directly.
14. **Deprecated items** — heading gets `DEPRECATED` suffix with message: `### OldClient DEPRECATED("use HttpClient")`.
15. **Failable** — `!` on return type. The doc string describes failure conditions in prose.
16. **Reference modifiers shown as-is** — `~this` (mutable/consuming), `&param` (shared borrow). These are critical for correctness: `~` means the value is consumed (moved), `&` means read-only borrow. An agent generating a call must know whether it's passing ownership or borrowing.
17. **Optional types** — `T?` shown in return types and parameters. Indicates the value may be `none` and the caller must handle it (typically via `if val := expr { ... }`).
18. **Structural interfaces** — type heading shows `` `structural ``. Summary block includes a note: "Structural interface — types satisfy this by implementing the required methods, without an explicit `is` declaration." This tells an agent it doesn't need to write `type Foo is Parseable` — just implement the methods.
19. **Multi-line doc strings** — rendered as-is (line breaks preserved). Triple-quoted strings (`"""..."""`) in source become multi-line text in output.
20. **Abstract methods** — marked with `` `abstract `` in signatures. Indicates the method has no body and must be implemented by subtypes.

### Directory/Module Output

When documenting a directory, each file becomes a top-level section. Files are sorted alphabetically. An index appears at the top listing every exported type, enum, and function — so an agent can scan the index and jump to what it needs:

    # networking

    ## Index

    - [client.pr](#clientpr) — HttpClient, Response
    - [server.pr](#serverpr) — HttpServer, Router, Middleware
    - [types.pr](#typespr) — HttpMethod, StatusCode, Header

    ---

    ## client.pr

    ### HttpClient is Resource
    ...

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-public` | on for directories | Show only `` `public `` symbols |
| `-all` | on for single files | Show all symbols including private |
| `-o PATH` | stdout | Write output to file or directory |
| `-signatures` | off | Signatures only, no doc text (compact mode) |
| `-no-index` | off | Skip index section for directories |

## Compiler Changes (all done in Phase 1)

### 1. Parameter Documentation — Done

The grammar already allows `doc()` on parameters in the AST. The semantic layer needs to propagate this.

**Add `doc` field to `types.Param`** (`types/signature.go`):

```go
type Param struct {
    name   string
    typ    Type
    ref    RefMod
    hasDef bool
    doc    string  // new: from `doc() annotation
}

func (p *Param) Doc() string       { return p.doc }
func (p *Param) SetDoc(s string)   { p.doc = s }
```

**Propagate in sema** (`sema/decl.go`): during `resolveMethodSignature` / `resolveFuncSignature`, extract `doc()` from each `ast.Param.Annotations` and call `param.SetDoc()`.

This is a small change — `ast.Param` already supports annotations and `extractDoc()` already exists.

### 2. Default Value Expressions — Done (Option A)

The doc tool reads defaults from `sema.Info.ParamDefaults` and `sema.Info.FieldDefaults`, formatting the `ast.Expr` via an `exprToString()` helper that covers literals, identifiers, and unary expressions.

### 3. Enum Variant Documentation — Done

Previously `enumVariant` in the grammar did not support meta annotations:

```
enumVariant : IDENT (LPAREN enumField (COMMA enumField)* RPAREN)? ;
```

**Add meta annotation support:**

```
enumVariant : IDENT (LPAREN enumField (COMMA enumField)* RPAREN)? metaAnnotation* ;
```

**Add `doc` field to `types.Variant`** (`types/enum.go`):

```go
type Variant struct {
    name   string
    fields []*VarField
    doc    string  // new
}

func (v *Variant) Doc() string     { return v.doc }
func (v *Variant) SetDoc(s string) { v.doc = s }
```

**Propagate in sema** (`sema/decl.go`): during `defineEnum`, extract `doc()` from variant annotations.

### 4. The `doc` Subcommand — Done

Implemented in `cmd/promise/doc.go`. Also added `sema.DeclareAndDefine()` / `sema.DeclareAndDefineWithModules()` in `sema/check.go` — runs passes 1+2 only (Declare + Define + validateConstructors), skipping Check/Verify/Ownership.

Entry point: parses the source file(s), runs sema Declare + Define passes (no Check/Verify/Ownership), then walks the resulting `Info` and scope to emit markdown.

The walker visits declarations in source order (not alphabetical), grouped by category:
1. Types (including structural interfaces)
2. Enums
3. Free functions

For each type, it emits:
- Heading with name, inheritance chain, and tags (`` `structural ``, `` `copy ``, `DEPRECATED`)
- Doc string
- Summary block (fields, methods, getters, operators)
- Per-method sections for documented methods

## Conventions for Doc Strings

The language treats `doc()` as an opaque string. These conventions are recommendations for the ecosystem, not compiler-enforced rules:

1. **First sentence is the summary** — tools may truncate to the first period for index views.
2. **Use imperative mood** — "Perform a GET request" not "Performs a GET request."
3. **Document failure conditions in prose** — "Raises on network failure" rather than a structured errors section. Name the error type if it's a specific subtype: "Raises IoError on disk failure."
4. **Parameter docs go on parameters** — `string url \`doc("...")` rather than listing params in the function doc.
5. **Keep it short** — if the signature is self-explanatory, don't add a doc string. `int count \`doc("Number of items")` adds nothing. `int ttl \`doc("Time-to-live in seconds; 0 means no expiry.")` is useful.
6. **Multi-line for complex behavior** — use triple-quoted strings for detailed descriptions:
    ```promise
    process(string input) Result!
        `doc("""Parse and validate the input string.
    Returns Result.Ok on success. Raises ValidationError if the input
    contains invalid characters or exceeds 1024 bytes.""") {
    ```
7. **Document every `` `public `` symbol** — undocumented public APIs force agents to read source. Tooling should warn on undocumented public symbols (see Lint Integration below).
8. **Mention ownership transfers** — if a method consumes `~this` or a `~param`, state this: "Consumes the connection. Cannot be used after calling close."

## Standard Library Documentation

Documenting `std/*.pr` with `doc()` is the **highest-impact item** in this proposal. Every agent interaction with Promise starts with knowing what the stdlib provides. An agent that can run `promise doc -std` and get the full standard library reference can generate correct code without reading source files.

`promise doc -std` outputs documentation for the embedded standard library (from `resources/std/*.pr`). No filesystem lookup needed — it reads from the same `go:embed` FS used by the compiler.

### Priority Order

1. **`string.pr`** — `len` (getter), operators (`+`, `==`, `<`), `contains`, `starts_with`, `ends_with`, `index_of`, `trim`, `split`, subscript `[]`, slice `[:]`, getters (`hash`, `is_empty`)
2. **`vector.pr`** — `len` (getter), `new(capacity = 16)`, `push`, `pop`, `contains`, `remove`, subscript `[]`/`[]=`, slice `[:]`/`[:]=`, getter `is_empty`
3. **`map.pr`** — `new()`, subscript `[]`/`[]=`, `contains`, `remove`, `keys`, `values`, `clear`, getters (`len`, `is_empty`); also the internal `Slot[K,V]` enum
4. **`error.pr`** — `string message` field, inheritance pattern for custom errors
5. **`channel.pr`** — `new(capacity)`, `send`, `close`, receive via `<-` operator
6. **`io.pr`** — `print(Format)`, `print_line(Format)`
7. **Structural interfaces** — `Equal` (`==`, `!=`), `Hashable` (`get hash`), `Ordered` (`<`, `>`, `<=`, `>=`; extends Equal)
8. **Numeric types** (`int.pr`, `uint.pr`, `float.pr`) — operators, range (`..`, `..=`), `hash`
9. **Other** — `iter.pr` (Iterator, Stream), `range.pr`, `task.pr`, `math.pr`, `format.pr`, `bool.pr`, `char.pr`

### Example: What `promise doc -std` Would Show for Vector

    ### Vector[T]

        type Vector[T] {
            get len int

            new(int capacity = 16)
            push(T elem)
            pop() T?
            contains(T elem) bool
            remove(int index)
            [](int index) T
            []=(int index, T value)
            [:](int? start, int? end) T[]
            [:]=(int? start, int? end, T[] value)

            get is_empty bool
        }

    #### Vector.new

        new(int capacity = 16)

    #### Vector.push

        push(T elem)

    #### Vector.pop

        pop() T?

    Returns the last element, or `none` if the vector is empty.

    ...

## Agent-Optimized Features

### 1. Signature-Only Mode (`-signatures`)

For agents that already understand Promise and just need to know what's available:

```bash
promise doc -signatures -public ./networking/
```

Output:

    type HttpClient is Resource {
        new(~this, string baseUrl, int maxRetries = 3, int timeout = 5000)
        get(this, string url) Response!
        post(this, string url, Bytes body) Response!
        close(~this)
        drop(~this)
        get is_connected bool
    }
    type Parseable `structural {
        parse(string data) Self! `abstract `factory
    }
    enum Result[T] { Ok(T value), Err(string message) }
    enum HttpMethod `copy { GET, POST, PUT, DELETE, PATCH }
    retryWithBackoff[T: Parseable](int attempts, f64 baseDelay, string data) T!

Minimal tokens. Maximum information density. An agent can consume this, understand the API surface, and start generating code immediately.

### 2. Inline Type Expansion (`-expand`)

When documenting a symbol, expand the types it references within the same module so the agent gets the full picture without follow-up lookups:

```bash
promise doc -expand HttpClient.get server.pr
```

Shows the `get` method signature AND the `Response` type it returns. The symbol must be qualified (`Type.method` or `functionName`) to avoid ambiguity.

### 3. Structured Query (`-query`)

Beyond text search, agents benefit from querying the type system:

```bash
# Find all types with drop() — tells agent which types need ownership care
promise doc -query "has:drop" ./...

# Find all failable functions
promise doc -query "failable" ./...

# Find all types implementing a structural interface
promise doc -query "implements:Parseable" ./...

# Find by name substring (fallback to text search)
promise doc -query "retry" ./...
```

This is more useful than raw `grep` because it searches structured semantic data, not source text. An agent asking "which types do I need to be careful with ownership?" can query `has:drop` instead of grepping for `drop(~this)` and hoping the formatting matches.

### 4. Lint Integration

```bash
promise doc -lint ./...
```

Reports:
- Public symbols without `doc()` annotations
- `doc()` that mentions parameters by wrong name
- Deprecated symbols without a replacement suggestion

This helps maintain documentation quality. Output is one-line-per-issue, grep-friendly:

    server.pr:12: HttpClient.post: public method missing `doc()
    server.pr:45: retryWithBackoff: doc mentions "timeout" but no such parameter exists

## Implementation Plan

### Phase 1: Core `promise doc` (MVP) — Done

1. ~~Add `doc` field to `types.Param` + accessors, propagate from AST in sema~~ — Done (`types/signature.go`, `sema/decl.go`)
2. ~~Add `doc` field to `types.Variant` + accessors, add `metaAnnotation*` to `enumVariant` grammar rule~~ — Done (`types/enum.go`, `grammar/PromiseParser.g4`, `ast/decl.go`, `ast/visit_decl.go`)
3. ~~Implement `cmd/promise/doc.go`~~ — Done (single-file documentation with full output format)
   - ~~Parse source, run sema Declare + Define (reuse `compileFrontend` with early exit)~~ — `sema.DeclareAndDefineWithModules()` in `sema/check.go`
   - ~~Walk scope: emit types (with inheritance, fields, methods, getters, operators, new/drop), enums (with payload variants), functions~~ — walks `ast.File.Decls` in source order
   - ~~Handle `-public` / `-all` filtering~~ — `-public` default, `-all` shows everything
   - ~~Handle `-o` file output~~ — Done
   - ~~Handle `-signatures` compact mode~~ — Done
4. ~~Add `doc` subcommand to `cmd/promise/main.go` command dispatch~~ — Done
5. Document `std/*.pr` with `doc()` — prioritize string, vector, map, error — **Not yet started**

Additional Phase 1 work done:
- Added `TargetParam` and `TargetVariant` meta targets in `sema/meta.go`
- Fixed `extractDoc()` to use `evalStringLit()` instead of `stringLitValue()` — triple-quoted `doc()` strings were silently ignored
- Inherited `drop` methods shown on child types (agents need this for ownership reasoning)
- Structural interface methods shown regardless of `public` filter (they define the contract)

### Phase 2: Multi-File and Std

1. Directory and recursive documentation (`promise doc dir/...`)
2. Index generation for multi-file output
3. `promise doc -std` for embedded standard library reference
4. `-expand Type.method` inline type expansion

### Phase 3: Query and Lint

1. `-query` with structured predicates (`has:drop`, `failable`, `implements:X`)
2. `-lint` for documentation quality checks
3. IDE integration (hover docs from `doc()`)

## Non-Goals

- **Doc comments (`///` or `/** */`)** — Promise uses meta annotations, not comments. Comments are for humans reading source; `doc()` is for tooling. Annotations are part of the AST, guaranteed to be preserved, and visible to the compiler. Comments are stripped during lexing.
- **Doc tests** — Promise already has `` `test `` and `` `test(expected: ...) ``. Mixing test code into doc strings adds complexity without clear benefit.
- **HTML/website generation** — Out of scope. Markdown is the universal interchange format. A static site generator can consume the markdown output if needed.
- **Automatic doc generation from signatures** — Generating "Returns an int" from `int` return type is noise. Only human-written documentation adds value.
- **JSON output** — Most agents work fine with structured markdown. If demand emerges for machine-parseable output, JSON can be added later as a `-format json` flag without changing the core architecture.
