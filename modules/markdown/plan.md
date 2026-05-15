# Promise `markdown` Module — API Proposal

Status: planned (not yet implemented). Tracker ID: _to be assigned_.

## 1. Motivation

Markdown is the de-facto text format AI models produce and consume. Any Promise
program that talks to an LLM — documentation generators, agent tools, README
builders, code-review bots, chat front-ends — needs to both **emit** well-formed
markdown and **parse** LLM-produced markdown into a structured form it can walk
programmatically.

Today Promise can concatenate markdown strings by hand, but:

- There is no canonical way to generate markdown — every caller reinvents escape
  rules, list indentation, fence selection.
- There is no way to read AI output back in — extracting a fenced code block, a
  task list, or the cells of a table from a model response requires a custom
  regex-driven parser per use case.
- The compiler itself emits markdown (`promise doc` surfaces module docs as
  markdown), and Promise's documentation story is markdown-first. A first-party
  `markdown` module lets tooling consume its own output.

A `markdown` catalog module gives user code and Promise tooling a single
well-tested implementation that handles both directions.

Analogues in other ecosystems:

| Language | Library                                 | Notes                      |
|----------|-----------------------------------------|----------------------------|
| Rust     | [`comrak`](https://github.com/kivikakk/comrak), [`pulldown-cmark`](https://github.com/raphlinus/pulldown-cmark) | CommonMark + GFM |
| Go       | [`goldmark`](https://github.com/yuin/goldmark) | CommonMark + extensions    |
| Python   | [`markdown-it-py`](https://github.com/executablebooks/markdown-it-py) | CommonMark + GFM |
| JS/TS    | [`marked`](https://github.com/markedjs/marked), [`markdown-it`](https://github.com/markdown-it/markdown-it) | CommonMark + GFM |

All four converge on the same shape: parse markdown text into a tree of block
and inline nodes; render a tree (or an equivalent builder) back to markdown or
HTML. This module mirrors that shape.

## 2. Design Principles

1. **Document tree, not `Encoder`/`Decoder`**. Unlike `json`/`toml`/`yaml`,
   markdown is not a typed-data serialization format. Its primitives are blocks
   (heading, paragraph, code block, list) and inlines (text, emphasis, link),
   not booleans and integers. Forcing it through the `Encoder` interface would
   produce either unreadable markdown or a nonsensical mapping. Instead this
   module exposes a document-tree data model plus a builder for writing. Typed
   data serialization is explicitly out of scope (see §9).

2. **Two directions, one tree**. Parsing produces a `markdown.Document`;
   rendering consumes one. A `markdown.Builder` is a convenience for constructing
   a document without building the tree by hand. All three APIs operate on the
   same `Block` / `Inline` node types, so a round-trip is lossless on supported
   constructs (with a few documented normalisations — see §8).

3. **CommonMark 0.31 + a minimal GFM subset**. The base is CommonMark 0.31,
   which is precisely specified and the target of every modern parser. On top
   of CommonMark we add three [GFM](https://github.github.com/gfm/) features
   because they are what AI models reliably produce and what users expect:
   **tables**, **task list items**, and **strikethrough**. Setext headings,
   HTML blocks beyond raw passthrough, reference links, footnotes, and maths
   are deferred to v2.

4. **Parse is total, render is total**. `Document.parse(input)` returns a
   `markdown.Document` for any `string` input — CommonMark is defined so every
   byte sequence is a valid document. `doc.to_string()` / `doc.format(writer)`
   likewise cannot fail. No `!` on either. This makes the API cheap to use in
   pipelines and matches how every mature markdown library behaves.

5. **Self-contained**: catalog-module constraints apply — depends only on `std`,
   no other catalog imports, no `[require]`, no module-level state. No PAL
   additions.

6. **No hidden effects**: parsing and rendering are pure string→value / value→string
   functions. File I/O is the caller's job via the `io` module.

## 3. Quick Start

```
use markdown;

main() {
  // Write: generate a README fragment
  use b := markdown.Builder();
  b.heading(1, "Project");
  b.paragraph("A short intro with *emphasis* and `code`.");
  b.code_block("go", "package main\n\nfunc main() {}\n");
  b.bullet_list(["first", "second", "third"]);
  print_line(b.to_string());

  // Read: parse AI output and extract all code blocks
  string model_reply = "Here is the fix:\n\n```python\nprint('ok')\n```\n";
  markdown.Document doc = markdown.Document.parse(model_reply);
  for markdown.Block block in doc.blocks {
    if block is markdown.Block.CodeBlock(info, text) {
      print_line("found {info} block:");
      print_line(text);
    }
  }
}
```

## 4. Assumed Dependencies

From `std` (auto-imported):

- `Builder`, `Writer`, `Format` (`std/builder.pr`, `std/format.pr`, `std/io.pr`)
- `Vector[T]`, `Map[string, V]` (`std/vector.pr`, `std/map.pr`)
- `string`, `char`, `int`, `bool` + their methods (`std/*.pr`)
- `error` base type (`std/error.pr`)

No new `std` types are required. No PAL additions. No catalog-module imports.

## 5. Full API Surface

### 5.1 Errors

```
type Error is error `public `doc("A markdown error (currently only raised by strict-mode helpers). Regular parse/render never fail.") {
  int line   `doc("1-based line number where the error was detected (0 if unknown).");
  int column `doc("1-based column number (0 if unknown).");
}
```

In v1 `markdown.Error` is raised only by the strict-mode helpers in §5.6
(e.g. `require_block`). Regular `parse` and `render` are infallible.

### 5.2 `markdown.Document`

```
type Document `public `doc("A parsed markdown document: an ordered sequence of blocks.") {
  Block[] blocks `doc("The document's block children, in source order.");

  new(~this, Block[] blocks = Block[]());

  // Parse factory — the canonical way to construct a Document from markdown text.
  parse(string source) Self `factory
    `doc("Parses markdown text into a document tree. Never fails (CommonMark is total). This is the canonical entry point for reading markdown in Promise — there is no free-standing `markdown.parse(source)` function. In languages like Go, Python, or JS you would call a module-level `parse`; in Promise, parsing is a factory on the target type, which also lets it initialise `` `final `` fields that a free function could not. Call as `markdown.Document.parse(source)`.");

  // Format interface — `doc.format(writer)` renders to any Writer.
  format(this, Writer ~w) `doc("Renders the document as CommonMark + GFM markdown text. Inverse of `parse` on the supported subset.");

  // Convenience.
  to_string(this) string `doc("Renders the document to a markdown string. Equivalent to formatting into a Builder and reading its contents.");

  // Navigation helpers — see §5.6.
  find_headings(this) Heading[] `doc("Returns every Heading in document order, including those nested inside block quotes.");
  find_code_blocks(this) CodeBlock[] `doc("Returns every fenced or indented code block in document order.");
  find_links(this) Link[] `doc("Returns every inline link in document order.");
  plain_text(this) string `doc("Returns the document with all markdown syntax stripped — prose only.");
}
```

**No free-function `parse` / `render`.** Promise favours one obvious way. The
factory `Document.parse(source)` replaces the `markdown.parse(source)` pattern
from other languages, and `doc.to_string()` / `doc.format(writer)` replaces
`render(doc)` / `render_to(doc, writer)`. The factory form is also the only
way to set `` `final `` fields on construction, which a free function cannot do.

### 5.3 Block-level nodes

```
enum Block `public `doc("A block-level markdown node.") {
  Heading(int level, Inline[] content)
    `doc("ATX heading. `level` is 1..6."),

  Paragraph(Inline[] content)
    `doc("A paragraph of inline content."),

  CodeBlock(string info, string text)
    `doc("Fenced (```) or indented code. `info` is the info-string after the fence (empty for indented)."),

  BlockQuote(Block[] children)
    `doc("A blockquote: zero or more nested blocks."),

  List(bool ordered, int start, ListItem[] items)
    `doc("Ordered or unordered list. `start` is the starting number for ordered lists (1 otherwise)."),

  Table(TableAlignment[] alignments, TableRow header, TableRow[] rows)
    `doc("GFM table: column alignments, a header row, and zero or more body rows."),

  ThematicBreak
    `doc("A horizontal rule: `---`, `***`, or `___`."),

  HtmlBlock(string html)
    `doc("A raw HTML block — preserved verbatim but not parsed."),

  // Type predicates (generated-style helpers — one per variant)
  is_heading(this) bool;
  is_paragraph(this) bool;
  is_code_block(this) bool;
  is_block_quote(this) bool;
  is_list(this) bool;
  is_table(this) bool;
  is_thematic_break(this) bool;
  is_html_block(this) bool;
}
```

### 5.4 Inline nodes

```
enum Inline `public `doc("An inline-level markdown node.") {
  Text(string value)
    `doc("Literal text (already unescaped)."),

  Emphasis(Inline[] children)
    `doc("Emphasised text — rendered with a single `*`."),

  Strong(Inline[] children)
    `doc("Strongly-emphasised text — rendered with `**`."),

  Strikethrough(Inline[] children)
    `doc("GFM strikethrough — rendered with `~~`."),

  Code(string value)
    `doc("Inline code span."),

  Link(Inline[] children, string url, string title)
    `doc("Inline link `[text](url \"title\")`. `title` is empty when absent."),

  Image(string alt, string url, string title)
    `doc("Inline image `![alt](url \"title\")`. `alt` is flattened to text."),

  LineBreak
    `doc("A hard line break (two trailing spaces or backslash-newline)."),

  SoftBreak
    `doc("A soft line break — a newline inside a paragraph, rendered as a space."),

  HtmlInline(string html)
    `doc("A raw inline HTML span — preserved verbatim.");
}
```

### 5.5 Supporting types

```
type ListItem `public `doc("An item within a List block.") {
  bool? checked `doc("GFM task-list state: none = not a task item; some(false) = unchecked; some(true) = checked.");
  Block[] blocks `doc("The item's block children (usually one Paragraph, possibly followed by a nested List).");
}

type TableRow `public `doc("A row of a GFM table.") {
  TableCell[] cells;
}

type TableCell `public `doc("A single cell of a GFM table.") {
  Inline[] content;
}

enum TableAlignment `public `doc("Column alignment for a GFM table.") {
  None,
  Left,
  Center,
  Right,
}

// Projection types returned by navigation helpers (§5.2).
type Heading `public `doc("A flattened view of a Block.Heading, with plain-text title for easy use.") {
  int level;
  string text;
  Inline[] content;
}

type CodeBlock `public `doc("A flattened view of a Block.CodeBlock.") {
  string info;
  string text;
}

type Link `public `doc("A flattened view of an Inline.Link.") {
  string text;
  string url;
  string title;
}
```

### 5.6 Builder

```
type Builder `public `doc("A fluent builder for markdown output. Writes into an internal Buffer; call `to_string()` or `close()` to finish.") {
  new(~this);

  // Block-level
  heading(~this, int level, string text)
    `doc("Appends an ATX heading. Panics if level < 1 or level > 6.");
  paragraph(~this, string text)
    `doc("Appends a paragraph of plain text. The text is escaped so markdown syntax characters are literal.");
  paragraph_raw(~this, string markdown)
    `doc("Appends a paragraph of already-markdown-formatted inline content (no escaping).");
  code_block(~this, string info, string text)
    `doc("Appends a fenced code block. Picks a longer fence if `text` contains triple-backticks.");
  bullet_list(~this, string[] items)
    `doc("Appends an unordered list of plain-text items.");
  numbered_list(~this, string[] items, int start = 1)
    `doc("Appends an ordered list of plain-text items.");
  task_list(~this, TaskItem[] items)
    `doc("Appends a GFM task list.");
  block_quote(~this, string text)
    `doc("Appends a block quote from plain text.");
  table(~this, string[] headers, string[][] rows, TableAlignment[] alignments = TableAlignment[]())
    `doc("Appends a GFM table. `alignments` defaults to TableAlignment.None for every column.");
  thematic_break(~this)
    `doc("Appends a `---` rule.");
  raw(~this, string markdown)
    `doc("Appends `markdown` verbatim followed by a blank line. Use sparingly.");

  // Composition with the tree types
  append_block(~this, Block block)
    `doc("Appends a pre-built Block node.");
  append_document(~this, Document doc)
    `doc("Appends every block of `doc`.");

  // Finalize
  to_string(this) string
    `doc("Returns the accumulated markdown.");
  close(~this)!
    `doc("Called by `use` binding at scope exit. Currently a no-op; reserved.");
}

type TaskItem `public `doc("A single row for Builder.task_list.") {
  bool checked;
  string text;
}
```

### 5.7 Navigation helpers on `Document`

Discussed above in §5.2; listed here as the strict-mode counterparts that do
raise `markdown.Error`:

```
require_heading(this, int level) Heading! `public
  `doc("Returns the first top-level heading at `level`. Raises Error if absent.");

require_code_block(this, string info) CodeBlock! `public
  `doc("Returns the first code block with info-string `info`. Raises Error if absent.");
```

These are explicitly for the common AI-tooling pattern “the model must have
produced a ```json block — fail loudly if not”.

### 5.8 Free functions

Only low-level string helpers live at module scope — parse/render belong on
`Document` (see §5.2).

```
escape(string text) string `public
  `doc("Escapes markdown syntax characters in `text` so it renders literally in a paragraph.");

escape_code(string text) string `public
  `doc("Escapes backticks inside inline code. Used by Builder.code_block when choosing a fence.");
```

## 6. Usage Patterns

### 6.1 Round-trip

```
main() {
  string src = "# Hello\n\nWorld with *emphasis*.\n";
  markdown.Document doc = markdown.Document.parse(src);
  string out = doc.to_string();
  assert(out == src, "round-trip preserved");
}
```

### 6.2 Extract every code block from an LLM response

```
main() {
  string reply = load_model_reply();
  markdown.Document doc = markdown.Document.parse(reply);
  for markdown.CodeBlock cb in doc.find_code_blocks() {
    if cb.info == "promise" {
      run_snippet(cb.text);
    }
  }
}
```

### 6.3 Generate documentation for a type

```
render_type_doc(string name, string summary, string[] members) string {
  use b := markdown.Builder();
  b.heading(2, name);
  b.paragraph(summary);
  b.heading(3, "Members");
  b.bullet_list(members);
  return b.to_string();
}
```

### 6.4 Edit a document: title + task list append

```
main()! {
  string src = io.read_all("TODO.md")?;
  markdown.Document doc = markdown.Document.parse(src);

  // Promote the first heading one level if it is too deep.
  for int i in 0..doc.blocks.len {
    if doc.blocks[i] is markdown.Block.Heading(level, content) {
      if level > 1 {
        doc.blocks[i] = markdown.Block.Heading(level: 1, content: content);
      }
      break;
    }
  }

  // Append a GFM task-list block.
  doc.blocks.push(markdown.Block.List(
    ordered: false,
    start: 1,
    items: [
      markdown.ListItem(checked: some(true),  blocks: [markdown.Block.Paragraph([markdown.Inline.Text("first")])]),
      markdown.ListItem(checked: some(false), blocks: [markdown.Block.Paragraph([markdown.Inline.Text("second")])]),
    ],
  ));

  io.write_all("TODO.md", doc.to_string())?;
}
```

### 6.5 Strict extraction (fail fast on missing block)

```
parse_structured_reply(string reply) Config! {
  markdown.Document doc = markdown.Document.parse(reply);
  markdown.CodeBlock json_block = doc.require_code_block("json")?;
  return json.decode_string[Config](json_block.text)?;
}
```

## 7. Comparison Table

| Operation                | Promise `markdown`                   | Rust `comrak`                    | Go `goldmark`                         | Python `markdown-it-py`      |
|--------------------------|--------------------------------------|----------------------------------|---------------------------------------|------------------------------|
| Parse text → tree        | `markdown.Document.parse(s)`         | `parse_document(arena, s, opts)` | `md.Parser().Parse(reader)`           | `md.parse(s)`                |
| Render tree → text       | `doc.to_string()`                    | `format_commonmark(root)`        | custom renderer (HTML built-in)       | custom renderer              |
| Build fluently           | `Builder` + `heading`/`paragraph`/…  | manual `Node::new`               | manual `ast.New…`                     | `MarkdownIt().render(tokens)`|
| Extract headings         | `doc.find_headings()`                | walk arena                       | `ast.Walk(root, fn)`                  | token filter                 |
| Task lists (GFM)         | `TaskItem` variant on list items     | `GFMExtension`                   | `extension.TaskList`                  | `gfm-like` preset            |
| Tables (GFM)             | `Block.Table` first-class            | `GFMExtension`                   | `extension.Table`                     | `gfm-like` preset            |
| Error handling           | `parse` infallible; strict helpers `!`| `ParseError`-free                | infallible                            | infallible                   |
| Spec target              | CommonMark 0.31 + GFM subset         | CommonMark 0.31 + GFM            | CommonMark + extensions               | CommonMark 0.30 + plugins    |

## 8. Implementation Notes

- **Pure Promise, no PAL**. Parser is a two-phase recursive-descent over a
  `char` cursor (block phase → inline phase on each block's text span).
- **Block phase** identifies lines as one of: ATX heading, thematic break,
  fenced code block start/end, list marker, block-quote prefix, table row
  (must follow a header + separator), HTML block start, blank, paragraph
  continuation. Lazy continuation rules follow the CommonMark spec §5.
- **Inline phase** walks each paragraph/heading/table-cell's text with a
  delimiter-stack algorithm (CommonMark §17.3) for emphasis/strong/strikethrough.
  Code spans (CommonMark §6.1), links (§6.3), images (§6.4), and HTML spans
  (§6.9) are scanned before emphasis to match reference parsers.
- **Escape handling**: the parser decodes `\\`-escaped ASCII punctuation and
  numeric/named character references in textual content; the renderer re-escapes
  conservatively — only characters that would otherwise start markdown syntax
  are escaped (`*`, `_`, `` ` ``, `[`, `]`, `<`, `>`, `\\`, `#` at line start,
  `-`/`+`/`=` at line start, leading spaces that would form an indented block).
- **Fence selection** in `Builder.code_block` counts the longest run of
  backticks in `text` and picks a fence one longer (minimum three). Mirrors
  `comrak`'s behaviour.
- **Rendering normalisations** (documented departures from the input):
  - Unordered list markers normalise to `-`.
  - Emphasis delimiter normalises to `*`.
  - Code fences normalise to backticks.
  - ATX headings keep trailing `#` only if they were present in the source
    (not tracked in v1 — dropped on round-trip).
  - Reference-style links and setext headings are rendered as inline/ATX
    equivalents.
- **Data ownership**: `Document`, `Block`, `Inline`, `ListItem`,
  `TableRow`, `TableCell` all own their children. No `close()` is required;
  default `drop()` cascade handles cleanup. `Builder.close()` is reserved for
  future `Writer`-backed variants and is a no-op in v1.
- **`Builder` is one-shot**: after `to_string()` is called the internal buffer
  is unchanged and may still accumulate more content — callers can keep
  appending. This matches `std.Builder`.
- **No module-level state, no globals, no caches** — every parser invocation
  allocates its own scratch buffers.
- **WASM**: fully supported. No filesystem, threading, or PAL calls. No
  `` `target `` annotations required.
- **Complexity**: parsing is O(n) in input length for well-formed input; the
  delimiter-stack inline parser is linear in practice but can be O(n²) on
  pathological input per the CommonMark spec — we accept this, matching every
  reference parser.

## 9. Future Extensions (explicitly deferred)

- **Setext headings** (`===` / `---` underlines) on the parse side. Rendering
  always emits ATX.
- **Reference-style links** (`[text][label]` + `[label]: url`). Resolved to
  inline links at parse time in v2; the `Inline.Link` variant is unchanged.
- **Footnotes** (GFM: `[^1]`) — add a `Block.FootnoteDefinition` and an
  `Inline.FootnoteReference` variant once needed.
- **LaTeX math** (`$...$` / `$$...$$`) — add `Inline.Math` / `Block.MathBlock`.
- **Front matter** (`---\nkey: value\n---`) — add a `Document.front_matter`
  field that round-trips a YAML string; defer typed parsing to the `yaml` module.
- **Incremental parsing** — CST that preserves every byte, for editor use.
- **Typed data serialization** (`encode_string[T: Encodable]`). Out of scope:
  markdown is a document format, not a data format. Callers who want typed data
  should use `json`/`toml`/`yaml` and embed the result in a markdown code block.
- **HTML renderer** — a `render_html(doc) string` helper. Useful, but wide in
  scope (sanitisation, attributes); layers cleanly on top of this v1 AST.
- **AST walker / visitor** — a `walk(doc, fn)` helper. The `find_*` helpers in
  §5.2 cover the common cases; a general visitor is deferrable.

## 10. Review Checklist

- [ ] Every public identifier uses full English words / approved abbreviations
- [ ] `snake_case` for functions/fields, `PascalCase` for types/variants
- [ ] No function overloading (default params only)
- [ ] All `` `public `` declarations have `` `doc ``
- [ ] `Error` is `is error` with location fields
- [ ] Error-raising functions marked `!`, infallible functions unmarked
- [ ] No `` `target `` needed (pure Promise, WASM-safe)
- [ ] Quick-start compiles mentally against the spec
- [ ] Round-trip on the quick-start example reproduces the input
