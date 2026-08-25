# Promise UI Module — Design Proposal

**Status:** Draft / RFC — architecture under discussion
**Module:** `ui`
**Targets:** native desktop (linux, macos, windows), `wasm`, and terminal

---

## 1. Overview

[#1-overview](#1-overview)

The `ui` module provides retained-mode UI rendering for Promise programs, targeting a
desktop window, the browser DOM, and a terminal from a single source of truth.

Three properties drive the entire design:

1. **One layout engine, multiple backends.** Layout is computed in Promise on every
   target. The browser's own layout engine is not used. A DOM node is positioned
   absolutely at a rect that Promise computed. This guarantees that a given `View`
   produces byte-identical geometry on every backend, and it makes the whole engine
   testable without a display, a GPU, or a headless browser.

2. **A closed layout catalog with an open composition surface.** Applications may
   define new components freely; they may not define new *layout behaviour*. These are
   separable, and the design separates them (§1.3). The seal is compiler-enforced, not
   conventional, and it is the single most important constraint in this document: it
   bounds what an AI agent writing UI code is able to express.

3. **A flat arena, not a pointer tree.** Promise's only reference field is `Ref[T]`, a
   refcounted handle; plain borrows are stack-only and scope-scoped, and every field
   access dispatches through a vtable. A conventional parent/child object graph fights
   all three. A flat `Node[]` arena addressed by integer indices fits the language,
   serialises trivially for tests, and turns the hot paths into linear scans.

The model is Flutter's — constraints go down, sizes come up, the parent sets position —
with a single-pass, O(n) layout and no constraint solver. The state model is Elm's —
`Model -> View`, `update(Model, Message) -> Model` — with event handlers represented as
inert `Message` **values** rather than closures.

### 1.1 Three Kinds of Extensibility

[#11-three-kinds-of-extensibility](#11-three-kinds-of-extensibility)

"Closed catalog" sounds like one property. It is three, with very different risk
profiles, and conflating them makes the design far more restrictive than it needs to
be:

| # | Extension | Example | v1 status |
| --- | --- | --- | --- |
| 1 | **New composition** | `Card`, `Toolbar`, `Avatar` | **Open.** A `Component` subclass with a `build()` that returns catalog primitives. |
| 2 | **New painting** | sparkline, gauge, chart | **Open question (§16.1).** A self-measuring leaf that emits display commands but participates in no layout protocol. |
| 3 | **New layout algorithm** | a novel multi-child sizing rule | **Closed.** Requires a reviewed diff to `view.pr`. |

Category 1 is what "custom component" means in practice — the overwhelming majority of
real components are arrangements of existing primitives, not new sizing rules. Keeping
it open costs the design nothing and removes the main ergonomic objection to a sealed
catalog.

Category 3 is the one worth fighting for. A user-supplied `performLayout` hook is the
single change that would dissolve every guarantee in this document, because agents
route everything through the most general mechanism available.

Category 2 sits in between and is genuinely low-risk — it is `Image` with a callback —
but it is deferred to an open question rather than assumed.

### 1.2 Goals

[#12-goals](#12-goals)

- Identical layout output across every backend, verified by test.
- A primitive catalog small enough for an agent to hold entirely in context, fully
  documented through `` `doc `` and discoverable via `promise doc ui -signatures`.
- Layout cost proportional to the changed region, not to tree size.
- No layout state reachable from application code. Applications produce `View` values
  and consume `Message` values; everything else is module-private.
- Zero steady-state heap allocation during layout.

### 1.3 Non-goals

[#13-non-goals](#13-non-goals)

- **No general-purpose custom layout.** There is no `CustomLayout` primitive and no
  user-supplied `performLayout` hook. A missing layout is a signal to add a primitive
  to the catalog through review, not to open an extension point.
- **No intrinsic sizing.** Flutter's `getMinIntrinsicWidth` family is opt-in and can go
  quadratic; agents reach for it exactly when they should not. Sizing that depends on
  content is confined to `Text` and `Image`, which measure directly.
- **No implicit theme inheritance.** `Theme` is a value passed explicitly. Implicit
  ambient context lookup is action-at-a-distance and makes agent-written UI hard to
  review.
- **No animation in v1.** Deferred until the layout core is stable.
- **No accessibility tree in v1.** The web backend emits real DOM text nodes, so it
  inherits a usable baseline; a first-class semantics tree is future work.

---

## 2. Module Structure

[#2-module-structure](#2-module-structure)

`ui` ships as a **catalog module whose source lives in its own repository**. The
catalog already supports this: an entry carrying `url` + `commit` is fetched from git,
while an entry without them is embedded from `modules/<name>/`
(see `compiler/internal/module/catalog.go`). Two modules already use the remote form.

```toml
# catalog.toml
[modules.ui]
url = "https://github.com/promise-language/ui.git"
commit = "<pinned>"
description = "Retained-mode UI: closed layout catalog, one layout engine, DOM/native/terminal backends"
```

Consumers then write a plain catalog import, with no `[require]` entry:

```promise
use ui;
```

**Why this rather than an external module.** The obvious objection to a catalog entry
is that the catalog is epoch-pinned while this module will churn. That does not apply
to remote entries: they are pinned **per module by commit**, independently of the
catalog epoch, so bumping `ui` is a one-line diff to `catalog.toml`.

The deciding factor is discoverability. Catalog modules are enumerated by
`promise doc` and `promise help`, so an agent can find `ui` without being told it
exists. An external module requires `promise package add` plus a sourced
`use ui "github.com/promise-language/ui"`, which an agent can only write if it already
knows the module is there. That directly contradicts the project's "minimal context
needed" goal, and it is the whole reason this module exists.

Shipping the catalog entry before the implementation is also normal practice here:
`term`, `markdown`, `toml`, `yaml`, `msgpack`, `ai`, `auth`, `cloud`, `mcp`,
`sandbox`, and `schema` are all catalog entries whose modules are not yet built.

Internal file layout (flat, per Promise convention — no `src/`):

```
ui/
├── promise.toml
├── view.pr           # View hierarchy, NodeKind, Component — THE CATALOG
├── geometry.pr       # Constraints, EdgeInsets, Color (Size/Rect/Point come from std)
├── arena.pr          # Node, Arena, build
├── layout.pr         # the layout pass
├── resolve.pr        # position resolution + display list emission
├── text.pr           # shaping, measurement, ShapeCache
├── hit.pr            # hit testing
├── app.pr            # the Elm loop, Channel[Event] plumbing
├── backend.pr        # Backend structural interface + DisplayList
├── backend_dom.pr    # WebBackend
├── backend_native.pr # DesktopBackend
├── backend_term.pr   # TerminalBackend
├── backend_record.pr # RecordingBackend (testing)
└── *_test.pr
```

### 2.1 The Export Surface

[#21-the-export-surface](#21-the-export-surface)

Top-level declarations in Promise are private by default, so the seal is mostly a
matter of *not* writing `` `public ``:

| Declaration | Exported? | Reason |
| --- | --- | --- |
| `View` | yes | applications name it in signatures and return it |
| `Component`, `Composite`, `component()` | yes | **the** application extension point (§3.1) |
| Catalog primitives (`Text`, `Row`, …) | yes | the catalog |
| `Style`, `EdgeInsets`, `Color`, `Alignment`, `Axis` | yes | configuration values |
| `Size[T]`, `Rect[T]`, `Point[T]` | — | not `ui`'s; re-exported from `std.geometry` (§4.1) |
| `NodeKind` | yes | no longer needs hiding — `` `sealed `` is the seal (§3.1.1) |
| `Constraints` | no | layout-internal; exposing it invites layout code outside `ui` |
| `Node`, `Arena` | no | the render tree is not application data |
| `DisplayCommand`, `Backend` | yes | applications select and pass a backend (§10) |
| `WebBackend`, `DesktopBackend`, `TerminalBackend`, `RecordingBackend` | yes | one is chosen at the call site |
| `run`, `Event`, `App`, `Handlers` | yes | the application entry point |

`` `sealed `` on `View` is what makes the catalog closed, and it is enforced by the
compiler rather than by convention. See §3.1.1.

---

## 3. The Component Catalog

[#3-the-component-catalog](#3-the-component-catalog)

### 3.1 The `View` Hierarchy

[#31-the-view-hierarchy](#31-the-view-hierarchy)

`View` is an abstract base type with exactly one abstract method, returning a
**module-private** enum:

```promise
enum NodeKind `copy `public {
  Composite,
  Text, Box, Row, Column, Stack, Padding, Align, Scroll, Image, Button, Input, Spacer,
}

type View `sealed `public `doc("A UI element. Closed catalog — see the ui constructors.") {
  kind(this) NodeKind `abstract;
}
```

`` `sealed `` (§5.4 of the language design) is the whole enforcement: `View` is
extensible inside `ui` and closed outside it. `NodeKind` is therefore free to be
`` `public `` — backends, tests, and diagnostics can all name it without opening the
catalog.

Catalog primitives are `is View` and each reports its own kind:

```promise
type Text is View `public `doc("A run of text.") {
  string content `public;
  Style style `public;
  kind(this) NodeKind { return NodeKind.Text; }
}

type Padding is View `public `doc("Insets its child.") {
  EdgeInsets insets `public;
  View child `public;
  kind(this) NodeKind { return NodeKind.Padding; }
}

type Row is View `public `doc("Horizontal flex container.") {
  RowChild[] children `public;
  Style style `public;
  kind(this) NodeKind { return NodeKind.Row; }
}
// ... Box, Column, Stack, Align, Scroll, Image, Button, Input, Spacer
```

**`Component` sits outside the hierarchy, not inside it.** The seal is *transitive* —
no subtype of a `` `sealed `` type may be `` `open ``, at any depth — so `Component`
cannot be an open `is View`. Making it one would let an application produce an external
subtype of `View`, which is exactly what the seal forbids. Instead `Component` is an
independent abstract type, and one catalog primitive carries it:

```promise
type Component `public `doc("Application-defined composite component.") {
  build(this) View `public `abstract;      // abstract ⇒ open, and no seal to violate
}

type Composite is View `public `doc("A View wrapping an application component.") {
  Component component `public;
  kind(this) NodeKind { return NodeKind.Composite; }
}

component(Component move c) View `public
    `doc("Wrap an application component as a View.");
```

Applications write `ui.component(Card(title: "…"))` where a `View` is expected. That is
one extra call versus `Component is View`, and it buys a fully sealed hierarchy rather
than a hierarchy with a hole in it.

**Why this seals the catalog.** `View`'s subtype set is fixed at the twelve primitives
plus `Composite`, enforced by `` `sealed ``. An application can define any number of
components, and every one must bottom out in catalog primitives, because the only way
into a `View` is `ui.component`. It cannot introduce a `NodeKind` case, therefore it
cannot introduce a layout rule.

Exhaustive dispatch stays sound for the reason that matters: every application component
reports `NodeKind.Composite`, so the *kind* set is closed even though the set of
`Component` subtypes is open. `Composite` is the single point where the closed world
hands off to the open one.

**Why not `enum View`.** An enum is the obvious way to express a closed catalog, since
enums are closed by construction. It does not compile — Promise rejects
directly-recursive enums:

```
view.pr:5:19: recursive enum View: variant Box field 'child' of type View? reaches View
without container indirection — use View[], map[K, View], or Set[View] to break the cycle
```

That rejection hits `Box`, `Padding`, `Align`, and `Scroll` — every single-child
primitive. Working around it means typing each child slot as `View[]`, which costs a
heap `Vector` allocation per `Padding`, in direct conflict with the zero-allocation
goal in §1.2. The `` `sealed `` class hierarchy has none of that problem (`View child`
and `View? child` are ordinary heap references), seals just as tightly, and is strictly
more expressive: the enum offered no extension point at all, forcing category-1
extensions through the same review gate as category-3.

The catalog is twelve primitives plus `Composite`. Adding a thirteenth is a change to
`view.pr` — a reviewable diff in one file, and the natural future home for an
`` `anchor `` once that feature lands.

### 3.1.1 How `` `sealed `` and `` `open `` Apply Here

[#311-how-sealed-and-open-apply-here](#311-how-sealed-and-open-apply-here)

The inheritance rules in §5.4 of the language design map onto this module cleanly, and
between them they account for every extensibility decision it makes:

| Type | Marking | Effect |
| --- | --- | --- |
| `View` | `` `sealed `` | Abstract; extensible only inside `ui`. The catalog seal. |
| `Text`, `Row`, `Padding`, `Composite`, … | none | Concrete → globally sealed by default. Nobody can subclass `Text` to override its behaviour, and the transitivity rule forbids marking one `` `open ``. |
| `Component` | none | Abstract (`build()`) → implicitly open. The application extension point. Outside the `View` hierarchy, so no seal to violate. |
| `Backend` | `` `structural `` | Implicitly open — backends are meant to be written by anyone (§10). |

Two consequences worth stating explicitly:

- **No annotation is needed on the primitives.** Sealed-by-default already prevents an
  application from subclassing `Row` and overriding its behaviour. That protection is
  free, and the transitivity rule makes it non-negotiable inside a `` `sealed ``
  hierarchy — which is the correct outcome here.
- **`NodeKind` is public.** Because `` `sealed `` does the enforcing, the tag enum does
  not have to be hidden to keep the catalog closed. Backends, golden-test serialisers,
  and diagnostics can all name `NodeKind` freely.

The one shape this rules out is a `View` subtype that applications may extend. That is
what `Composite` + `Component` exists to provide, and routing it through a wrapper rather
than a hole in the hierarchy is what keeps `match v.kind()` exhaustive over a genuinely
closed set.

### 3.2 Making Illegal Nesting Unrepresentable

[#32-making-illegal-nesting-unrepresentable](#32-making-illegal-nesting-unrepresentable)

Flutter's single worst ergonomic failure is the runtime error
`BoxConstraints forces an infinite height`, produced by nesting an unbounded child
inside an unbounded parent. Promise has no refinement types and no macros, so this
cannot be solved with a predicate on `Constraints`. It is solved instead with
**distinct types at the composition points**.

**Scroll content.** `Scroll` does not accept a `View`. It accepts a `ScrollContent`,
which can only be produced by constructors that yield an intrinsically-sized subtree:

```promise
enum ScrollContent `public {
  Fixed(View child, Size size),
  List(ListItem[] items),
  Grid(ListItem[] items, int columns, Size cell),
}

// The only ways to build one:
scroll_fixed(View move child, Size size) ScrollContent `public;
scroll_list(ListItem[] move items) ScrollContent `public;
scroll_grid(ListItem[] move items, int columns, Size cell) ScrollContent `public;
```

`ScrollContent` remains an enum, and legally so: it reaches `View` only through a heap
class reference, which breaks the cycle the recursion check looks for. (Verified —
§17.)

A `Column` is a `View`, not a `ScrollContent`. "Column inside a ListView" is not a
runtime error with a good message — it is a type error, because there is no
conversion. The agent's only path forward is `scroll_list`, which is the correct
answer.

**Flex children.** `Row` and `Column` do not accept `View[]`. They accept `RowChild[]`,
which forces the author to state flex intent:

```promise
type RowChild `public {
  View child;
  f64 flex;          // 0.0 = fixed, > 0.0 = expands
  Alignment cross;
}

fixed(View move child) RowChild `public;
flex(View move child, f64 factor) RowChild `public;
```

This removes the "why isn't my widget filling the space" class of question entirely:
the answer is always visible at the call site.

**Nested unbounded axes.** A `Scroll` on the vertical axis whose `ScrollContent` is
itself a vertical `Scroll` remains constructible and is rejected at build time with a
source-located diagnostic. This is the one residual case the type system does not
catch; it is rare, cheap to detect during `build`, and not worth further type
machinery.

### 3.3 Composition by Application Code

[#33-composition-by-application-code](#33-composition-by-application-code)

There are two ways to compose, and both are unrestricted.

**A plain function**, when the arrangement needs no identity:

```promise
card(string title, View move body) View {
  return ui.Box(
    style: card_style(),
    child: ui.Column(children: [
      ui.fixed(ui.Text(content: title, style: heading_style())),
      ui.fixed(ui.Spacer(extent: 8.0)),
      ui.flex(move body, 1.0),
    ], style: Style()),
  );
}
```

**A `Component` subclass**, when the component has fields, is reused, or wants to be a
named type in signatures:

```promise
type Card is ui.Component `doc("A titled card with a body.") {
  string title;
  View body;

  build(this) ui.View {
    return ui.Box(
      style: card_style(),
      child: ui.Column(children: [
        ui.fixed(ui.Text(content: this.title, style: heading_style())),
        ui.fixed(ui.Spacer(extent: 8.0)),
        ui.flex(this.body, 1.0),
      ], style: Style()),
    );
  }
}
```

A `Component` is not itself a `View`, so it is wrapped where one is expected:

```promise
ui.Column(children: [
  ui.fixed(ui.component(Card(title: "Today", body: chart))),
  ui.fixed(ui.Text(content: "footer", style: Style())),
], style: Style())
```

Either way, the component introduces a name and a default arrangement; it cannot
introduce a new layout rule. That is exactly the intended level of freedom, and it is
the shape Flutter's `StatelessWidget` proved ergonomic.

Prefer the function form for one-off arrangements and the `Component` form when the
component has fields, is reused across call sites, or needs to be named in a signature.

### 3.4 Handlers Are Values, Not Closures

[#34-handlers-are-values-not-closures](#34-handlers-are-values-not-closures)

Promise closures capture non-`Copy` values by `move`, transferring ownership and
invalidating the outer binding. A callback-style API (`on_click: || -> ...`) would
therefore make every interactive component an ownership negotiation with the enclosing
model.

Handlers are instead **message identifiers**. The `View` tree stores an integer slot
index; the application-specific `M` values live in a side table built alongside it:

```promise
type Handlers[M] `public {
  M[] slots;

  add(~this, M move msg) int {
    int i = this.slots.len;
    this.slots.push(move msg);
    return i;
  }
}
```

```promise
view(Model m, Handlers[Message]~ h) View {
  return ui.Button(label: "Increment", style: Style(), on_click: h.add(Message.Increment));
}
```

This buys three things at once. The render tree stays free of the `M` type parameter,
so the arena and the entire layout engine monomorphise **once** rather than per
application. Handlers are inert data, so no effect can be smuggled into a view
function. And event routing becomes an integer lookup.

---

## 4. Geometry and Style

[#4-geometry-and-style](#4-geometry-and-style)

### 4.1 Reuse `std.geometry`

[#41-reuse-stdgeometry](#41-reuse-stdgeometry)

`ui` defines **no** point, size, or rectangle types. `modules/std/geometry.pr` already
provides them as generic pure value types, and the module uses those directly:

```promise
type Point[T] `public { T x `value; T y `value; }
type Size[T]  `public { T width `value; T height `value; }
type Rect[T]  `public { T x `value; T y `value; T width `value; T height `value;
  get origin Point[T];
  get size Size[T];
}
```

So the engine's working types are `Size[f64]`, `Rect[f64]`, and `Point[f64]`.

**No separate `Offset` type.** A parent-relative offset is a displacement rather than a
position, and some UI toolkits give that its own type. Promise's std already has
`Point[T]` with identical shape, and introducing a second two-`f64` type would be two
names for one thing — against the "one obvious way" principle. Offsets are `Point[f64]`,
and the field is named `offset` where the distinction matters.

Note the field names are `width` and `height`, not `w`/`h`, per the full-English-words
rule (§9.3a of the language design). This document follows them.

**Additions `ui` needs that belong in `std.geometry`, not here:** `Rect.contains(Point[T])`,
`Rect.intersect(Rect[T])`, and `==` on all three. These are generally useful and should be
added alongside the existing types rather than duplicated in `ui`.

### 4.2 `ui`-Local Value Types

[#42-ui-local-value-types](#42-ui-local-value-types)

The remaining geometry types have no std equivalent and are defined here. All are pure
value types — every field carries `` `value ``, so they are stack-allocated, automatically
`` `copy ``, and embedded inline wherever they appear.

```promise
type EdgeInsets `public {
  f64 left `value;
  f64 top `value;
  f64 right `value;
  f64 bottom `value;

  horizontal(this) f64 { return this.left + this.right; }
  vertical(this) f64   { return this.top + this.bottom; }
}

type Color `public {
  u8 red `value;
  u8 green `value;
  u8 blue `value;
  u8 alpha `value;
}
```

> **Open question (§16.10):** `Color` is also needed by the planned `term` module, which
> sketches its own. One shared `Color` in `std` would serve both; deciding this before
> either module is written avoids a needless incompatibility.

`Constraints` is module-private:

```promise
type Constraints {
  f64 min_width `value;
  f64 max_width `value;
  f64 min_height `value;
  f64 max_height `value;

  is_tight(this) bool {
    return this.min_width == this.max_width && this.min_height == this.max_height;
  }

  deflate(this, EdgeInsets i) Self {
    return Self(
      min_width: 0.0,
      max_width: max(0.0, this.max_width - i.horizontal()),
      min_height: 0.0,
      max_height: max(0.0, this.max_height - i.vertical()),
    );
  }

  constrain(this, Size[f64] s) Size[f64] {
    return Size[f64](width: clamp(s.width, this.min_width, this.max_width),
                     height: clamp(s.height, this.min_height, this.max_height));
  }

  ==(Constraints o) bool { ... }   // required — used as the memo key
}
```

Three language constraints to respect here:

- **Pure value types cannot have `is` parents.** They are leaf types. No
  `type Constraints is Something`, no shared abstract geometry base.
- **Hybrid types are not implemented.** A type's fields must be either *all* `` `value ``
  or *none*. This is why `Node` (§5) carries no `` `value `` annotations at all, even
  though several of its fields are themselves value types.
- **Receivers are bare `this` (shared) or `~this` (mutable).** `&this` is no longer
  valid syntax and is rejected outright by sema.

`Style` is likewise a pure value type, which requires every field to be `` `copy ``:

```promise
enum Alignment `copy { Start, Center, End }     // `copy is REQUIRED — see below
enum Axis `copy { Horizontal, Vertical }

type Style `public {
  Color background `value;
  Color foreground `value;
  EdgeInsets padding `value;
  f64 corner_radius `value;
  f64 font_size `value;
  Alignment align `value;
  u32 font_id `value;        // index into the font registry, not a handle
  u32 flags `value;          // bold, italic, clip, etc. — bitfield
}
```

**Payload-free enums as value-type fields:** they are **not** `` `copy `` by default —

```
value field Style.align must be a copy type, got Alignment
```

— but marking the enum `` `copy `` makes it a legal `` `value `` field. So `Style` holds
`Alignment` and `Axis` directly; no `u32` packing and no conversion at use sites.
Verified in §17.

Note `font_id` and `flags`: a pure value type cannot hold a `string` or any other move
type, so anything nameable is interned to an integer. Font registration happens once at
startup.

One consequence worth stating: payload-free enums do **not** get `==` for free
(`operator == not defined on type NodeKind`). Compare them with `match`, or give the
enum an explicit `==`.

---

## 5. The Arena Render Tree

[#5-the-arena-render-tree](#5-the-arena-render-tree)

### 5.1 Why a Flat Arena

[#51-why-a-flat-arena](#51-why-a-flat-arena)

Three properties of Promise make a conventional render-object graph a poor fit:

- **Plain borrows cannot be stored in fields.** Parent and child pointers would require
  `Ref[T]` throughout, adding refcount traffic to the hottest data structure in the
  system and making cycles (parent ↔ child) a leak.
- **Borrows are scope-scoped, not last-use.** A `Node~` binding keeps the value borrowed
  until the end of its enclosing block, so the natural "borrow the parent, walk the
  children, write back" shape produces constant conflicts.
- **All field access is vtable-dispatched.** A polymorphic `RenderNode` hierarchy means
  an indirect call per field read in the layout inner loop, recoverable only if
  devirtualisation fires.

A flat array of a single concrete `Node` type sidesteps all three: indices instead of
pointers, no borrows held across calls, one monomorphic type with devirtualisable
access.

Note that this is *not* in tension with `View` being a class hierarchy (§3.1). The two
are separate representations: `View` is the description an application writes and is
walked exactly once per frame, in `build`; `Node` is the engine's working set and is
walked in every hot path. Virtual dispatch is fine in the former and unacceptable in
the latter.

### 5.2 Structure

[#52-structure](#52-structure)

```promise
type Node {
  NodeKind kind;
  int parent;          // NONE for the root
  int first_child;     // index of the first child, NONE if leaf
  int child_count;

  Style style;
  int text_slot;       // index into Arena.strings, NONE if not textual
  int handler_slot;    // index into Handlers[M].slots, NONE if inert
  f64 flex;            // 0.0 for non-flex children
  u32 flags;           // axis, alignment, clip — packed

  Size[f64] size;           // OUTPUT: computed by layout
  Point[f64] offset;       // OUTPUT: set by the parent, relative to the parent
  Constraints last_in; // memo key: the constraints that produced `size`
  bool dirty;
  bool boundary;       // true if last_in was tight — a relayout boundary
}

type Arena {
  Node[] nodes;
  string[] strings;
  ShapeCache shapes;   // survives arena rebuilds — see §7
}
```

**Indices are `int`, and `NONE` is `-1`.** `u32` with `NONE = 0xFFFFFFFF` would be the
tighter encoding, but vector indexing requires `int`
(`index type mismatch: expected int, got u32`), so every arena access would need an
`as!` cast. `int` with `-1` is simpler and matches the language.

### 5.3 The Two Invariants

[#53-the-two-invariants](#53-the-two-invariants)

Everything downstream depends on these, and `build` is the only code that must maintain
them:

> **I1. A parent's index is strictly less than every descendant's index.**
>
> **I2. A node's children occupy a contiguous index range** `[first_child, first_child + child_count)`.

Consequences:

- A forward linear scan of `nodes` is a valid pre-order traversal.
- A backward linear scan is reverse pre-order — painter's-algorithm top-most-first,
  which is exactly what hit testing wants (§9).
- Position resolution needs no recursion and no explicit stack (§6.3).
- Sibling iteration in the flex algorithm is a linear index range, not a linked-list
  walk.

### 5.4 Build

[#54-build](#54-build)

`build` is the one place the `View` hierarchy is consumed. It dispatches on `kind()`,
expands `Composite` nodes by calling `build()`, and lowers everything else to a `Node`.
It reserves a node's entire child block before descending into any child, which is what
establishes I2:

```promise
build(Arena~ a, View v, int parent) int {
  // Composite: expand and recurse. Application components never reach the arena.
  if v.kind() == NodeKind.Composite {
    Composite c = v as! Composite;
    return build(a, c.component.build(), parent);
  }

  int self = a.nodes.len;
  a.nodes.push(node_for(v, parent));

  int first = a.nodes.len;
  int count = child_count_of(v);
  a.nodes[self].first_child = if count == 0 { NONE } else { first };
  a.nodes[self].child_count = count;

  // Reserve the whole sibling block first (maintains I2) ...
  for i in 0..count { a.nodes.push(blank(self)); }
  // ... then fill each subtree in place.
  fill_children(a, v, first);

  return self;
}
```

`node_for` and `child_count_of` match on `v.kind()` and downcast to the concrete
primitive (`v as! Text`, `v as! Row`, …) to read its fields. The 1:1 correspondence
between `NodeKind` and primitive type is maintained inside `view.pr` alone.

A `Component` whose `build()` returns another `Component` recurses until it reaches a
primitive. This terminates for the same reason Flutter's does — a cycle is an
application bug, and a depth counter in debug builds turns it into a diagnostic rather
than a stack overflow.

**Build is the only phase that grows the arena. Layout never does.**

This is not a stylistic preference. `Vector.push` can reallocate the backing store; if
layout could push while an index-derived borrow was live, the result is precisely the
dangling-pointer scenario Promise's shared-borrow-mutation rule exists to prevent.
Confining growth to one phase means layout only ever mutates in place, and the invariant
is easy to state in review: *no `push` in `layout.pr`.*

---

## 6. The Layout Pass

[#6-the-layout-pass](#6-the-layout-pass)

### 6.1 Protocol

[#61-protocol](#61-protocol)

Constraints go down; sizes come up; the parent writes the child's offset. One pass, each
node visited once, O(n). No constraint solver, no reflow iteration.

`layout` is a **free function taking `Arena~`**, not a method on `Arena`. Reborrowing a
parameter on each recursive call is the pattern Promise's borrow checker handles most
predictably; a `~this` receiver recursing into itself is not.

```promise
layout(Arena~ a, int i, Constraints c) Size[f64] {
  // Memo check — the cheap exit for clean subtrees.
  if !a.nodes[i].dirty && a.nodes[i].last_in == c {
    return a.nodes[i].size;
  }

  // Copy every scalar we need OUT of the arena before any recursive call.
  NodeKind k = a.nodes[i].kind;
  int first  = a.nodes[i].first_child;
  int n      = a.nodes[i].child_count;
  Style style   = a.nodes[i].style;

  Size[f64] s = match k {
    NodeKind.Text    => { layout_text(a, i, style, c) },
    NodeKind.Box     => { layout_box(a, first, n, style, c) },
    NodeKind.Padding => { layout_padding(a, first, style, c) },
    NodeKind.Row     => { layout_flex(a, first, n, style, c, Axis.Horizontal) },
    NodeKind.Column  => { layout_flex(a, first, n, style, c, Axis.Vertical) },
    NodeKind.Stack   => { layout_stack(a, first, n, style, c) },
    NodeKind.Align   => { layout_align(a, first, style, c) },
    NodeKind.Scroll  => { layout_scroll(a, first, style, c) },
    NodeKind.Image   => { layout_image(a, i, style, c) },
    NodeKind.Spacer  => { layout_spacer(a, i, style, c) },
    _                => { c.constrain(Size[f64](width: 0.0, height: 0.0)) },
  };

  // Write back only after all child work is complete.
  a.nodes[i].size     = s;
  a.nodes[i].last_in  = c;
  a.nodes[i].dirty    = false;
  a.nodes[i].boundary = c.is_tight();
  return s;
}
```

`NodeKind.Composite` never appears here — `build` expanded it away.

### 6.2 Per-Kind Rules

[#62-per-kind-rules](#62-per-kind-rules)

**Padding** — child receives `c.deflate(insets)`. Own size is the child's size plus the
insets, re-constrained. Child offset is `(left, top)`.

**Box** — if it has a child, the child receives `c` loosened (`min` zeroed) and the box
takes the child's size; if childless, it takes `c.max` where finite, else `c.min`.

**Align** — child receives `c` loosened. Own size is `c.max` (an `Align` fills what it
is given). Child offset is derived from the alignment fraction: `(c.max_width - child.width) * fx`.

**Row / Column** — three linear passes over `[first, first + n)`:

```promise
layout_flex(Arena~ a, int first, int n, Style style, Constraints c, Axis axis) Size[f64] {
  f64 max_main  = main_of(c, axis);
  f64 max_cross = cross_of(c, axis);

  // Pass 1 — inflexible children get a loose main axis.
  f64 used = 0.0;
  f64 total_flex = 0.0;
  f64 max_child_cross = 0.0;

  for j in first..(first + n) {
    f64 fl = a.nodes[j].flex;
    if fl > 0.0 { total_flex = total_flex + fl; continue; }

    Constraints cc = loose_main(axis, max_main, max_cross);
    Size[f64] s = layout(a, j, cc);
    used = used + main_of_size(s, axis);
    max_child_cross = max(max_child_cross, cross_of_size(s, axis));
  }

  // Pass 2 — flexible children get a TIGHT main axis share.
  f64 free = max(0.0, max_main - used);
  for j in first..(first + n) {
    f64 fl = a.nodes[j].flex;
    if fl == 0.0 { continue; }

    f64 share = free * (fl / total_flex);
    Constraints cc = tight_main(axis, share, max_cross);
    Size[f64] s = layout(a, j, cc);
    max_child_cross = max(max_child_cross, cross_of_size(s, axis));
  }

  // Pass 3 — position. No layout work, just offsets.
  f64 cursor = 0.0;
  for j in first..(first + n) {
    Size[f64] s = a.nodes[j].size;
    f64 cross_pos = align_cross(a.nodes[j], s, max_child_cross, axis);
    a.nodes[j].offset = offset_for(axis, cursor, cross_pos);
    cursor = cursor + main_of_size(s, axis);
  }

  return c.constrain(size_for(axis, cursor, max_child_cross));
}
```

Contiguous sibling ranges are what make this three linear scans rather than three
pointer walks.

> **Open question (§16.3):** pass 1 gives every inflexible child the *full* remaining
> main axis as its loose maximum. A `Text` child in a `Row` therefore wraps at the whole
> row width and can starve the flex children of space. Flutter has the same sharp edge.
> The alternatives — a pre-measure pass, or an explicit `Shrink` wrapper — need to be
> settled before `layout_flex` is written.

**Stack** — every child receives `c` loosened; own size is the max over children,
constrained. Children are positioned by their own alignment. Painted in index order, so
later children are on top — consistent with the hit-test order in §9.

**Scroll** — the scroll axis is given `INFINITY` as its max; the cross axis is given
tight `c.max_cross`. The `Scroll` node's own size is `c.max` (it fills its slot). The
child's offset carries `-scroll_position` on the scroll axis. The overflow extent is
retained for scrollbar geometry and clamping.

**Text** — leaf. Shapes against `c.max_width` and returns the shaped block extent. See §7.

**Image** — leaf. Uses the declared `Size` constrained by `c`; the intrinsic pixel size
is not consulted, so image decode never blocks layout.

**Spacer** — leaf. `extent` on the main axis, zero on the cross axis.

### 6.3 Position Resolution

[#63-position-resolution](#63-position-resolution)

Layout produces only sizes and parent-relative offsets. Absolute rects come from a
single forward sweep — correct **only** because of I1:

```promise
resolve(Arena a, Rect[f64][]~ out) {
  out[0] = Rect[f64](x: 0.0, y: 0.0, width: a.nodes[0].size.width, height: a.nodes[0].size.height);

  for i in 1..a.nodes.len {
    Rect[f64] p   = out[a.nodes[i].parent];   // parent index < i, so already resolved
    Point[f64] o = a.nodes[i].offset;
    Size s   = a.nodes[i].size;
    out[i] = Rect[f64](x: p.x + o.x, y: p.y + o.y, width: s.width, height: s.height);
  }
}
```

No recursion, no stack, no branching. The display list is emitted in the same sweep,
maintaining a clip stack keyed by depth.

### 6.4 The Display List

[#64-the-display-list](#64-the-display-list)

A flat command buffer — the sole interface between the engine and any backend:

```promise
enum DisplayCommand `public {
  FillRect(Rect[f64] r, Color c, f64 radius),
  DrawTextRun(Rect[f64] r, int shaped_id, Color c, u32 font_id, f64 size),
  DrawImage(Rect[f64] r, int image_id),
  PushClip(Rect[f64] r),
  PopClip,
  PushTransform(f64 dx, f64 dy),
  PopTransform,
}
```

Seven commands. A backend that implements these seven correctly renders any `View` the
catalog can express, which is what keeps backend cost bounded.

---

## 7. Text

[#7-text](#7-text)

**This is the highest-risk component in the proposal and should be prototyped first.**
If text measurement is not identical across backends, layouts diverge and the core
promise of the design fails — and no amount of care elsewhere recovers it.

The requirement: shaping and line breaking must be **byte-identical** everywhere.
Calling `CanvasRenderingContext2D.measureText` on the web fails this immediately — it
depends on the browser, the platform font stack, and hinting settings.

Fonts are embedded rather than looked up, for the same reason: a system font lookup is a
platform-dependent input, and a platform-dependent input to layout is a divergence
waiting to happen. Applications may register additional embedded fonts at startup and
receive a `font_id`.

```promise
get inter_regular u8[] `embed("fonts/Inter-Regular.ttf", compress: true)
    `doc("Default UI font — embedded so metrics never depend on the host.");
```

`ShapeCache` is keyed by `(text_slot, font_id, font_size, flags, max_width)` and stores the
shaped glyph run plus its measured extent. It **survives arena rebuilds** — this is what
makes the rebuild-on-structural-change strategy (§8.2) affordable, since shaping
dominates and is unaffected by tree shape.

### 7.1 The Unresolved Part

[#71-the-unresolved-part](#71-the-unresolved-part)

The appealing answer is to compile HarfBuzz plus a UAX #14 line breaker to a WASM blob,
embed it, and run **the same blob** everywhere. That is not implementable: **Promise has
no WASM interpreter for native targets.** What it would actually produce is HarfBuzz
linked via `` `extern `` C symbols natively and HarfBuzz as a WASM module on the web —
same source, two different executions, which is precisely the guarantee this section
exists to establish.

The options, none yet chosen:

1. **Two builds of the same HarfBuzz source**, and treat cross-build metric equality as
   a tested property rather than a structural one. Cheapest; the risk is floating-point
   and codegen differences surfacing as rare, hard-to-diagnose layout drift.
2. **Ship a small WASM interpreter** in the native backend so the blob genuinely is the
   same. Structurally sound, and a large undertaking with an ongoing maintenance cost.
3. **Implement shaping in Promise** — a TrueType/OpenType parser plus a deliberately
   restricted subset (horizontal advances, kerning, a bounded GPOS/GSUB subset), with a
   UAX #14 line breaker. Deterministic by construction, no FFI on any target, no
   interpreter. Bounded but real work, and it caps script coverage in v1.

Option 3 deserves serious consideration despite looking like the most work: it is the
only one where cross-backend identity is a *property of the design* rather than a thing
to be tested for. It also fits the project's "prefer Promise over IR" philosophy. The
cost is honest and up front — v1 would support a defined script subset rather than
claiming universal coverage it cannot verify.

This must be settled before anything else is built.

---

## 8. Invalidation

[#8-invalidation](#8-invalidation)

### 8.1 Dirty Propagation and Relayout Boundaries

[#81-dirty-propagation-and-relayout-boundaries](#81-dirty-propagation-and-relayout-boundaries)

Marking a node dirty walks `parent` indices upward and stops at the first node whose
`boundary` is true — a node that last received *tight* constraints, and whose size
therefore cannot change no matter what happens beneath it:

```promise
mark_dirty(Arena~ a, int i) {
  int cur = i;
  while cur != NONE {
    if a.nodes[cur].dirty { return; }         // already on a dirty path
    a.nodes[cur].dirty = true;
    if a.nodes[cur].boundary { return; }      // size is pinned — stop here
    cur = a.nodes[cur].parent;
  }
}
```

Layout then re-runs from the root, but the memo check at the top of `layout` returns
immediately for every clean subtree. Cost is proportional to the dirty region.

### 8.2 Value Change vs. Structural Change

[#82-value-change-vs-structural-change](#82-value-change-vs-structural-change)

**Value change** — a label's text, a colour, a flex factor, with tree shape unchanged.
Diff the new `View` against the arena in a single forward walk, patch the differing
fields in place, `mark_dirty` each patched node. No allocation.

**Structural change** — a different number of children, a different variant, a reordered
list. Indices shift, so patching in place is not possible.

The recommendation is to **not fight this: rebuild the arena wholesale.**
`nodes.clear()` retains capacity, so a rebuild appends N flat structs into
already-allocated memory — no allocation in steady state. Crucially, `ShapeCache` is not
cleared, so the expensive work (shaping) is reused across the rebuild.

This is a deliberate rejection of Flutter's `Element` tree. Element exists to diff
immutable widget configs against persistent state and preserve identity across rebuilds.
Its cost is a third tree, a keying system, and a large class of identity-related bugs.
Here the persistent state that actually matters — shaped text and decoded images — lives
in content-keyed side caches that do not care about tree identity at all. Rebuilding a
flat array is cheaper than maintaining Element, and the absence of the concept removes a
whole category of things an agent can get wrong.

Real UIs perform far more value changes than structural ones, so the common path is the
in-place one regardless.

---

## 9. Hit Testing and Events

[#9-hit-testing-and-events](#9-hit-testing-and-events)

Hit testing is a backward linear scan. By I1, a later index was painted later and is
therefore on top, so the first match wins:

```promise
hit(Arena a, Rect[f64][] rects, Point[f64] p) int {
  int i = a.nodes.len - 1;
  while i >= 0 {
    if a.nodes[i].handler_slot != NONE
        && rects[i].contains(p)
        && clip_chain_contains(a, rects, i, p) {
      return i;
    }
    i = i - 1;
  }
  return NONE;
}
```

`clip_chain_contains` walks `parent` indices upward, checking the point against every
ancestor that sets the clip flag. Cheap, because the walk is short and the rects are
already resolved.

The router maps the hit node's `handler_slot` through `Handlers[M].slots` to a `Message` and
sends it on the event channel.

### 9.1 The Application Loop

[#91-the-application-loop](#91-the-application-loop)

```promise
type App[M, Model] `public {
  Model model;
  (Model, M) -> Model update;
  (Model, Handlers[M]~) -> View view;
}

run[M, Model](App[M, Model] move app, Backend~ backend) `public {
  Channel[Event] events = Channel[Event](capacity: 64);
  backend.start(events);          // backend pushes platform events

  Arena a = Arena();
  Rect[f64][] rects = [];
  loop {
    Handlers[M] h = Handlers[M]();
    View v = (app.view)(app.model, h);

    a.nodes.clear();
    build(a, v, NONE);
    layout(a, 0, viewport_constraints());
    resolve(a, rects);
    backend.present(display_list_from(a, rects));

    Event e = <-events;
    int target = route(a, rects, e);
    if target != NONE {
      app.model = (app.update)(app.model, h.slots[target]);
    }
  }
}
```

The backend is a parameter (§10), so which one runs is one visible line at the call
site rather than an invisible target-selection rule.

`Channel` is one of the `` `interior `` types, so a backend holding a shared borrow of
the channel can still send on it. This is the sanctioned mechanism for the
backend → engine event path and avoids any need for interior mutability elsewhere.

---

## 10. Backends

[#10-backends](#10-backends)

A backend is any type satisfying the `Backend` structural interface. Backends are
**independent types, not target-filtered variants of one type**:

```promise
type Backend `public `structural `doc("Consumer of a display list.") {
  start(~this, Channel[Event] events) `abstract;
  execute(~this, DisplayCommand cmd) `abstract;
  present(~this, DisplayCommand[] list) `abstract;
}

type WebBackend      `public `target(wasm)  { ... }   // backend_dom.pr
type DesktopBackend  `public `target(!wasm) { ... }   // backend_native.pr
type TerminalBackend `public                { ... }   // backend_term.pr — every target
type RecordingBackend `public               { ... }   // backend_record.pr — testing
```

`` `target `` appears only on the backends that genuinely need platform syscalls, and
because their **names differ**, each compiles to distinct globals. This matters: two
same-named types distinguished only by `` `target `` fail in codegen with
`redefinition of global '@promise_vtable_..._Backend'` — sema filters the declaration but
codegen still emits its vtable and typeinfo. That is a real compiler bug worth tracking,
but this design does not depend on it being fixed.

Three consequences beyond avoiding that bug:

- **`TerminalBackend` needs no `` `target `` at all** — it is stdout, and compiles on
  every target including wasm.
- **Several backends coexist in one binary**, so cross-backend equality is an in-process
  assertion rather than a cross-target CI diff (§12.2).
- **Selection is explicit.** `run(app, DesktopBackend())` states which backend is in use
  at one reviewable line, which is more in keeping with §1.3's "no hidden effects" than
  selection by invisible target magic.

Every backend implements the same seven display commands plus `start`/`present`.

### 10.1 Web Backend

[#101-web-backend](#101-web-backend)

Emits absolutely-positioned DOM elements at Promise-computed rects. The browser's layout
engine is never consulted — every element is `position: absolute` with explicit
`left/top/width/height`, and text nodes are set to `white-space: pre` with explicit line
breaks from our own line breaker.

The reason to use DOM rather than canvas is everything canvas throws away: real text
selection, IME and input-method support, screen-reader access, browser find-in-page, and
native scrolling behaviour where we opt into it.

Bindings come from `promise bind webidl` rather than hand-written FFI.

> **Validate the generated binding surface early.** If `promise bind webidl` does not
> cover element creation, style property assignment, and event listener registration
> cleanly, the cost of this backend changes materially, and that should be discovered
> early rather than late.

DOM nodes are pooled and reused across frames keyed by arena index, so a rebuild does not
churn the document.

### 10.2 Native Backend

[#102-native-backend](#102-native-backend)

Consumes the same display list into a rasteriser. Window creation, the event loop, and
the swap chain are platform-specific and filtered by `` `target ``. Glyph atlases are
keyed by `(font_id, size, glyph_id)` and populated from the same shaped runs the web
backend uses.

### 10.3 Terminal Backend

[#103-terminal-backend](#103-terminal-backend)

Quantises rects to a character grid and renders into a cell buffer. `FillRect` becomes a
background-colour span, `DrawTextRun` becomes clipped cell text, `DrawImage` becomes a
placeholder block, and clips become rectangle intersection.

It is by far the cheapest backend to build and the first one that can prove the display
list is a sufficient interface. It is also the natural consumer of the planned `term`
catalog module (raw mode, cell buffer, styled text, input events, cursor control), so
the two should be designed together.

Geometry is still computed in `f64` and quantised only at emission, so the layout arena
remains identical to the other backends and golden tests still compare unrounded values.

### 10.4 Recording Backend

[#104-recording-backend](#104-recording-backend)

Appends every `DisplayCommand` to a vector and renders nothing. This is what makes §12 test
the *display list* — the thing backends actually consume — rather than only the layout
arena, with no window, GPU, or wasmtime involved.

---

## 11. Ownership Discipline

[#11-ownership-discipline](#11-ownership-discipline)

Two rules make the whole engine compile without arguing with the borrow checker. They
belong in the module's contributing notes, because they are non-obvious and violations
produce confusing errors far from the cause:

> **R1. Never hold a `Node~` binding across a recursive call or any arena mutation.**
> Copy the scalars you need into locals, do the work, write back by index.
>
> Borrows in Promise are scope-scoped rather than last-use: a stored `Node~` binding
> keeps the arena borrowed until the end of its enclosing block, not until its final
> read. The "copy out, work, write back" shape in §6.1 is not defensive style — it is
> the only shape that compiles.

> **R2. Pass `Arena~` as a parameter, never as a `~this` receiver, in recursive code.**
> Reborrowing a parameter per call is handled predictably; self-recursion through a
> receiver is not.

Two supporting notes:

- **No borrows in fields, anywhere.** The arena is index-addressed precisely so that no
  structure ever needs to hold a reference to another. If something appears to need a
  stored reference, it needs an index.
- **`Ref[T]` only at the boundary.** The shape cache and font registry are shared across
  frames and are legitimate `Ref[T]` holders. Nothing in `layout.pr` should contain one.

---

## 12. Testing

[#12-testing](#12-testing)

Because `View` is inert description and layout produces a serialisable arena, the entire
engine is testable without a window, a GPU, or a headless browser.

### 12.1 Golden Layout Trees

[#121-golden-layout-trees](#121-golden-layout-trees)

The arena serialises to one line per node: `index parent kind x,y wxh`.

```promise
main() `test(expected: "0 - Column 0,0 400x600\n1 0 Text 8,8 384x20\n2 0 Row 8,36 384x40\n") {
  Arena a = Arena();
  Rect[f64][] rects = [];
  build(a, sample_view(), NONE);
  layout(a, 0, Constraints(min_width: 400.0, max_width: 400.0, min_height: 600.0, max_height: 600.0));
  resolve(a, rects);
  print(serialize(a, rects));
}
```

Deterministic pass/fail, readable diffs, no pixel comparison.

### 12.2 Cross-Backend Identity

[#122-cross-backend-identity](#122-cross-backend-identity)

Because backends are ordinary types (§10), this is an in-process assertion rather than a
cross-target CI string diff: build one display list, feed it to two backends, compare
what each recorded.

```promise
cross_backend_identity() `test {
  DisplayCommand[] list = display_list_for(sample_view(), viewport());

  RecordingBackend a = RecordingBackend();
  RecordingBackend b = RecordingBackend();
  replay(a, list);
  replay(b, list);
  assert(a.commands == b.commands);
}
```

The same golden should additionally be compiled for `wasm` and for the host and produce
the same string — that is what guards the central claim, and it fails loudly the moment
text metrics drift. It should run in CI on every commit for both targets.

### 12.3 Stress

[#123-stress](#123-stress)

`promise test -stress` catches nondeterminism in shaping and in cache eviction — exactly
the failure mode that would otherwise show up as an intermittent golden-test diff much
later.

### 12.4 Invariant Assertions

[#124-invariant-assertions](#124-invariant-assertions)

Debug builds assert I1 and I2 after every `build`, assert that `layout` did not change
`nodes.len`, and assert that no `Node` in the arena has `kind == NodeKind.Composite`.
These four checks catch the majority of arena-corruption bugs at their source rather than
three phases downstream.

---

## 13. Documentation as the Agent Contract

[#13-documentation-as-the-agent-contract](#13-documentation-as-the-agent-contract)

Every exported declaration carries `` `doc ``, including per-parameter docs:

```promise
scroll(Axis axis `doc("Scroll direction. The cross axis is sized by the parent."),
       ScrollContent move content `doc("Build with scroll_list or scroll_fixed — a bare View will not typecheck."),
       Style style) View `public
    `doc("A scrollable region. Fills its parent on both axes.");
```

`promise doc ui -signatures` should be a complete, compact, always-in-sync API surface an
agent can read in one pass before writing any UI code. Given that `` `doc `` is
AST-attached rather than comment-based, it cannot drift from the signature it documents —
which is the property that makes it worth treating as the contract rather than as
commentary.

Where a diagnostic can name the fix, it should. `Column` passed where `ScrollContent` is
expected should say *"use `scroll_list` for a scrolling list of items"*, not just report a
type mismatch. The same applies to the seal: an attempt to subclass `View` directly will
report that `View` is `` `sealed `` outside `ui`, which is accurate but does not say what
to do instead. It should add *"extend `ui.Component` and wrap it with `ui.component()`;
new layout primitives require a change to the `ui` module."*

---

## 14. Milestones

[#14-milestones](#14-milestones)

**Deferred.** Sequencing is not worth fixing until the architecture is agreed. Two things
are already clear and will survive whatever order is chosen:

- **Text (§7) gates everything.** It is the only open question whose resolution
  invalidates the architecture rather than merely delaying it, and §7.1 must be settled
  before any other work begins.
- **The terminal backend is the cheapest first backend**, and proves the display list is
  a sufficient interface at a fraction of the cost of DOM or a rasteriser.

---

## 15. Rejected Alternatives

[#15-rejected-alternatives](#15-rejected-alternatives)

**Browser layout on web, Promise layout on desktop.** Cheapest to build, and it destroys
the central guarantee. CSS flexbox and this constraint model disagree in dozens of edge
cases (percentage resolution against indefinite sizes, min-content behaviour, baseline
alignment); reconciling them is unbounded work and every divergence is a bug report.

**CanvasKit / full canvas rendering on web.** Gives pixel-identical output, and gives up
text selection, IME, screen readers, and find-in-page. The absolutely-positioned DOM
approach preserves those and, because we compute layout ourselves anyway, costs little
more.

**`enum View` as the catalog.** The obvious encoding of a closed set, and it does not
compile — Promise rejects directly-recursive enums, which hits every single-child
primitive (§3.1). The
workaround (`View[]` for every child slot) allocates a `Vector` per `Padding`. The sealed
class hierarchy is legal, allocation-free for single children, and seals more tightly
because `NodeKind` privacy is compiler-enforced while still leaving a composition door
open.

**A `RenderObject` inheritance hierarchy for the arena.** Idiomatic in most languages, but
in the *engine* each field read would be a vtable dispatch and each parent link would need
a `Ref[T]`. The `NodeKind` tag with a `match` in one function is faster, simpler to
serialise, and easier to review. Note this is not in tension with §3.1: virtual dispatch
is fine in `View`, which is walked once per frame, and unacceptable in `Node`, which is
walked in every hot path.

**Flutter's Element tree.** Solves an identity-preservation problem we do not have,
because our persistent state is content-keyed rather than tree-keyed. See §8.2.

**Refinement types for constraint boundedness.** Attractive, and unavailable — Promise has
no refinement types and no macros to synthesise them. Distinct types at the composition
points (§3.2) achieve the same guarantee with the type system that exists.

**Closure-based event handlers.** Fights Promise's `move`-capture semantics at every
component boundary, forces `M` through the render tree, and reopens the door to effects in
view code. Message values cost one integer indirection and close all three.

**One `Backend` type selected by `` `target ``.** Fails in codegen today (§10), prevents
backends from coexisting, forces the terminal backend to carry a target filter it does not
need, and turns cross-backend testing into a cross-target CI diff. Independent types
satisfying a structural interface are better on every axis.

**Shipping as an external module.** Rests on a misreading — remote catalog entries are
pinned per module by commit, not by the catalog epoch (§2). An external module also
forfeits `promise doc` discoverability, which is the module's whole purpose.

---

## 16. Open Questions

[#16-open-questions](#16-open-questions)

1. **Custom painting (extensibility category 2, §1.1).** Should the catalog include a
   `Canvas(Size size, int painter_id)` primitive backed by a registry of painters
   returning `DisplayCommand[]`? It is a self-measuring leaf that participates in no layout
   protocol — `Image` with a callback — and it is what most "the catalog doesn't have
   this" cases (sparkline, gauge, badge, chart) actually need. The friction of registering
   a painter and threading an integer id is the "discouraged" part. **Open: in or out for
   v1?**
2. **Text strategy (§7.1).** Two HarfBuzz builds, a bundled WASM interpreter, or shaping
   implemented in Promise. Blocking everything.
3. **Flex pass-1 main-axis constraint (§6.2).** Whether inflexible children get the full
   remaining main axis (Flutter's behaviour, with Flutter's sharp edge) or something
   tighter.
4. **Sub-pixel positioning.** Fractional rects give better typography and worse DOM
   snapping. Proposal: layout in `f64`, round to device pixels only at display-list
   emission, so golden tests compare unrounded geometry. The terminal backend quantises
   far more aggressively and confirms this split is the right one.
5. **Font fallback.** Embedding one font is tractable; a fallback chain across scripts must
   resolve identically on every backend or §12.2 fails. Possibly restrict v1 to an
   explicit, ordered, embedded fallback list.
6. **Arena capacity policy.** `clear()` retaining capacity means a single very large frame
   pins memory permanently. Some shrink heuristic is likely needed.
7. **`promise bind webidl` coverage** for the DOM subset the web backend needs (element
   creation, style assignment, event listeners).
8. **Hybrid types.** If `` `value `` and instance fields become mixable, `Node` could place
   `size`, `offset`, and `last_in` in the value struct. Worth revisiting for cache
   behaviour, but the design must not depend on it.
9. **A shared `Color` (§4.2).** The planned `term` module sketches its own `Color`. One
   in `std` would serve both. Worth settling before either module is written.
10. **`` `anchor `` integration.** Once anchoring lands, `view.pr` is the natural anchor
    target — the catalog becomes a human-approved interface. `` `sealed `` is already the
    enforcement; the anchor would add human review on top of it.

---

## 17. Verified Against the Compiler

[#17-verified-against-the-compiler](#17-verified-against-the-compiler)

Everything in this section was executed against `bin/promise` rather than inferred from
documentation.

### 17.1 Confirmed Working

[#171-confirmed-working](#171-confirmed-working)

| Claim | Section |
| --- | --- |
| `Component` outside the `View` hierarchy: app subclasses it across a module boundary, wraps via `component()`, and lowers correctly through `kind()` + in-module downcast | §3.1, §5.4 |
| `ScrollContent` enum reaching `View` through a class reference — legal, cycle broken by the heap reference | §3.2 |
| Value types with `Self` returns, `==` operators, and bare `this` receivers | §4 |
| Payload-free enum as a `` `value `` field, **when the enum is marked `` `copy ``** | §4 |
| `Arena~` mutable-borrow parameters; `a.nodes[i].size = ...` in-place field assignment through a vector index | §5, §6 |
| `match` on a `NodeKind` with block-bodied arms containing declarations, loops, and downcasts | §6.1 |
| `Handlers[M]` generic side table with `add(~this, M move msg)` | §3.4 |
| Function-typed fields — `(Model, M) -> Model update;` on a generic type | §9.1 |
| `` `structural `` `Backend` with `` `abstract `` methods; independent types satisfying it; `Backend~` parameter accepting any of them | §10 |
| `` `embed("...", compress: true) `` on a module-level getter | §7 |
| `promise doc <module> -signatures` and `promise bind webidl` both exist | §13, §10.1 |
| Remote catalog entries (`url` + `commit`) — two modules already use the form | §2 |

### 17.2 Confirmed Broken or Constrained

[#172-confirmed-broken-or-constrained](#172-confirmed-broken-or-constrained)

| Finding | Consequence |
| --- | --- |
| **Directly-recursive enums are rejected** (T0628) | `enum View` is not viable; §3.1 is a class hierarchy |
| **`` `target `` on two same-named types fails in codegen** — `redefinition of global '@promise_vtable_..._Backend'`; sema filters the declaration but codegen still emits its vtable and typeinfo. Never exercised in-tree — `` `target `` is used only on functions | Real compiler bug. §10's design avoids it by using distinct type names, so it is not blocking |
| **Vector indices must be `int`, not `u32`** | Arena uses `int` indices and `NONE = -1` (§5.2) |
| **`for-in` takes no type annotation** — `for i in 0..n`, not `for int i in 0..n` | All loops in this document corrected |
| **`&this` is rejected** — bare `this` is the shared-borrow receiver | Noted in §4; `promise help` still teaches the old form |
| **Payload-free enums are not `` `copy `` by default** | Mark `Alignment`/`Axis` `` `copy `` (§4) |
| **Payload-free enums have no `==` by default** | Compare with `match`, or define `==` explicitly |
| **No WASM interpreter for native targets** | §7's "same blob on both targets" is not implementable as written; see §7.1 |
| **`` `sealed `` is transitive** — no subtype of a sealed type may be `` `open ``, at any depth | `Component` cannot be `is View`; it is an independent abstract type reached through the `Composite` primitive (§3.1) |
| **`` `abstract `` is a method meta only** — `type T `abstract {}` fails with ``meta `abstract cannot be applied to type`` | Abstractness comes from declaring an abstract method; the language design's type-level form is not implemented |
| **Cross-module downcast panics codegen** — `b as! mod.Leaf` and `b as mod.Leaf` both crash with ``panic: codegen: unsupported cast target type *ast.QualifiedTypeRef`` (`expr_cast.go:701`) | Real compiler bug. Not blocking: all downcasting happens inside `ui`, where type names are unqualified. It does block application-side downcasting of a `View` |
| **`` `sealed `` / sealed-by-default are design-approved but unimplemented** (T1537) | The seal is specified, not yet enforced; §3.1's structure is verified, the seal itself is not |
| **`cmd` is not in the approved abbreviation dictionary** (§9.3a) | `DisplayCommand`, not `DisplayCmd` |
| **`std.geometry` already defines `Point[T]`/`Size[T]`/`Rect[T]`** with `width`/`height`, not `w`/`h` | `ui` defines no geometry types of its own and no `Offset` (§4.1) |

### 17.3 Not Yet Verified

[#173-not-yet-verified](#173-not-yet-verified)

- The layout pass itself — no part of §6 has been run.
- `ShapeCache` behaviour and any text measurement at all (§7).
- Whether `promise bind webidl` covers the DOM subset the web backend needs (§16.7).
- Arena rebuild allocation behaviour — that `nodes.clear()` retains capacity in practice
  (§8.2).
- Borrow-checker behaviour of the recursive `layout(Arena~ a, ...)` shape under real
  recursion depth (§11, R1/R2). This is the assumption most likely to require rework, and
  a small spike would settle it cheaply.
