# Promise Formatter (`promise format`)

> Canonical formatter for Promise source code. No configuration. No options. One input produces one output. An AI agent can match `promise format` output without running the tool because the rules are simple and deterministic.

---

## 1. Design Principles

**One canonical form.** No `.fmtrc`, no style options, no per-project overrides. Every valid Promise program has exactly one formatted representation.

**No line width limit.** No 80-col, no 100-col, no wrapping heuristics. One statement = one line. The only line breaks are structural (blocks, declarations, match arms). Line wrapping is the most complex and least deterministic part of every formatter — AI agents produce shorter/longer lines unpredictably, and wrapping rules force them to simulate a layout engine. We skip all of that.

**No alignment.** No aligning `=` signs, no column-aligning comments, no tabular formatting. Alignment is fragile — adding one field breaks all lines.

**Minimal comment reformatting.** Comments pass through mostly verbatim. Their position (end-of-line vs own-line) is preserved. Content inside comments is never modified. The only change: `//foo` is normalized to `// foo` (ensures space after `//`), but `///` and `//!` are left alone.

**Optimized for AI agents.** Minimal rules, maximum determinism. An agent that knows the rules can generate already-formatted code in one shot.

---

## 2. Complete Rule Set

### 2.1 Indentation

- **2 spaces** per indent level. No tabs.
- No trailing whitespace on any line.
- File ends with exactly one newline.
- No more than one consecutive blank line anywhere.

### 2.2 Blocks & Braces

- Opening `{` on same line as its construct, one space before: `if x {`.
- Closing `}` on own line at the construct's indent level.
- `} else {` stays on same line (no newline between `}` and `else`).
- `}` followed by `.method()` stays on same line (chaining).
- `}` followed by `?`, `!`, `)`, `,`, `;` stays on same line (postfix/delimiters).
- Blank lines between top-level declarations are preserved from source (up to 1 blank line).
- No blank lines immediately after `{` or before `}`.

### 2.3 Spacing

- One space around binary operators: `a + b`, `x == y`, `a && b`, `x := 5`, `a = b`, `a & b`, `a | b`.
- No space around unary operators: `-x`, `!flag`, `~obj`, `<-ch`, `&ref`.
- Unary vs binary disambiguation: `-`, `+`, `*`, `&`, `~`, `!` are **unary** if the preceding token is not value-producing (e.g., after `=`, `(`, `,`, keywords). They are **binary** if the preceding token produces a value (e.g., after identifiers, literals, `)`, `]`).
- `&` and `~` as ownership modifiers in parameters: no space after — `Reader ~r`, `Writer &w`, `(~this)`.
- No space between function/method name and `(`: `foo(x)`, `obj.method()`.
- Space between control keywords and `(`: `if (x)`, `for (...)`, `return (a, b)`.
- No space inside `()`, `[]` delimiters.
- One space after `,`: `foo(a, b, c)`.
- No space before `:`, one space after `:` — `Foo(x: 1, y: 2)`.
- One space before and after `->` and `=>`.
- Lambda params: `|int x, int y|` — no space inside pipes against params.
- Meta annotations: backtick touches keyword, space separates: `` `native `public `doc("...") ``.
- No space around `.` and `?.` (member access).
- No space around `..` and `..=` (range operators).
- `++` and `--` (postfix): no space before.
- `?` and `!` after value-producing tokens (postfix): no space — `x?`, `result!`.

### 2.4 Semicolons & Newlines

- Semicolons produce a newline after them (except in classic `for` headers: `for i := 0; i < n; i++ {`).
- Commas produce a newline only if the source had a newline after them (preserves inline vs multi-line style).
- Colons produce a newline only if the source had a newline after them (select cases vs named args).

### 2.5 Strings & Literals

- String literals (regular, raw `r"..."`, triple-quoted `"""..."""`) pass through verbatim — content is never modified.
- Char literals pass through verbatim.
- Numeric literals (including suffixes like `i32`, `u8`, `f64` and prefixes like `0x`, `0b`) pass through verbatim.

### 2.6 Lambdas

- Pipe `|` is detected as lambda start when the preceding token is NOT value-producing. Otherwise it's bitwise OR.
- `move` keyword before pipes: `move |x| -> x`.

---

## 3. Implementation

### 3.1 Approach: Token-Based Reformatter

Uses a custom lexer that tokenizes Promise source into a flat token stream (including comments and newlines as explicit tokens). The reformatter walks the token stream and re-emits with canonical spacing and indentation. This avoids coupling to the ANTLR4 grammar (which uses `-> skip` for comments) and keeps the formatter simple and fast.

```
input.pr → custom lexer → token stream → reformatter → formatted output
```

Key design decisions:
- **No parse tree needed** — operates purely on tokens, making it robust against partial/invalid source.
- **Source newline preservation** — commas, colons, and semicolons only produce newlines if the source had them, avoiding the need to track block context (match vs function call, etc.).
- **2-token lookback** — `prev` and `prevPrev` tokens enable unary/binary disambiguation and lambda pipe detection without a full parser.

### 3.2 Key Files

- `compiler/internal/formatter/formatter.go` — custom lexer, reformatter, spacing rules (~1045 lines)
- `compiler/internal/formatter/formatter_test.go` — table-driven tests (~130 cases)
- `compiler/cmd/promise/fmt.go` — CLI wiring (stdin/file/dir modes, `-w`/`--check`/`--diff`)
- `compiler/cmd/promise/main.go` — `case "format"` command dispatch

### 3.3 CLI

```
promise format [options] [files/dirs...]

  -w          Write to source file instead of stdout
  --check     Exit 1 if any file not formatted (CI mode)
  --diff      Show line-by-line diff of changes

No args = stdin → stdout. Directory args recurse for *.pr files.
```

### 3.4 Status

**Implemented:**
- Full token-based formatter with all spacing rules
- CLI with `-w`, `--check`, `--diff` modes
- All 27 `std/*.pr` files formatted and verified idempotent
- All 184+ test files verified idempotent
- ~130 unit tests covering all language constructs

**Not yet implemented:**
- Import sorting (Phase 2)
- Trailing comma normalization in match/enum (Phase 2)
- Integration into `bin/verify.sh` via `--check` (Phase 3)

---

## 4. Test Strategy

- **Unit tests:** ~130 table-driven cases in `formatter_test.go` covering every token type, operator, keyword, and language construct.
- **Idempotency:** `fmt(fmt(x)) == fmt(x)` for every `.pr` file in the repo (all std/ + tests/ + modules/).
- **Corpus validation:** All `std/*.pr` files formatted in place — the formatter defines the canonical style.
