# Annotations

> **Tag:** `annotations` — remaining work to complete this document: `mcp__tracker__list --tag annotations`

An annotation is a backtick-prefixed modifier on a declaration: `` `public ``, `` `copy ``,
`` `test(timeout: "5s") ``. This document is the **normative reference for what each one means**,
and the complete list — an annotation absent from §6 is not part of the language.

Syntax — placement, the parameter grammar, and why the backtick — is §8.1 of
[language-design.md](language-design.md), and the models the annotations serve are §5 (types), §6
(ownership) and §17 (concurrency). Those sections define the models; this document defines the
annotations, and neither restates the other.

## 1. The one-declaration rule

**A property is declared in exactly one place: an annotation on the declaration it belongs to.**
Every compiler decision about that property reads the declared flag.

The rule exists because the alternative has already cost correctness. Re-enumerating a property as
a hardcoded list of type identities inside the compiler declares the same thing twice, the copies
drift, and the wrong one wins: a hardcoded "these types are sharable" list once contradicted
`modules/std/task.pr` and let a program past the type checker that the backend could not compile.

Three corollaries:

1. **Native and source-declared types are governed identically.** A `` `native `` type's
   annotations are not decorative. A property a native type cannot derive structurally — having no
   Promise-level fields to derive from — is carried by its explicit annotation, and that annotation
   is what every decision consults.
2. **No decision may test a type's identity or its name to recover a property.** Neither
   `origin == types.TypTask` nor `obj.Name() == "Set"` is an acceptable stand-in for a flag.
   Identity-by-string is worse still: a rename, or a user type of the same name, silently changes
   behavior.

   The compiler does legitimately know a handful of library types — it binds `{:}` to one,
   constructs another for `..`, and depends on a third's shape for `for`-`in`. Those are
   **declared** with `` `builtin(role) `` (§10) rather than found by name, and they are bounded by
   what the knowledge may drive: **identity may bind syntax to a type, construct values of it, or
   check the compiler's own assumptions about it — never recover a property that the type's own
   structure determines.** The test is a user's own `MyMap`: writing one should cost exactly the
   `{:}` literal and nothing else, because every capability, drop rule and safety check must
   behave identically for a library container and a user's.
3. **Adding an annotation to a type is sufficient** to give that type the property. If a new
   primitive needs a compiler edit to gain a documented capability, that capability is hardcoded
   somewhere, and that is a defect.

`` `interior `` is the pattern done right end to end: `recvIsInterior`
(`compiler/internal/sema/expr.go`) resolves the receiver's underlying type and returns
`tt.IsInterior()`, with no list of primitives anywhere.

## 2. The set is closed

**Both the annotations and their parameters are a closed set.** §6 is the complete list of
annotation names; each entry's *Parameters* field is the complete list for that annotation. There
are **no user-defined annotations**, and no annotation accepts a parameter it does not declare.
Writing an unknown name is *"unknown meta annotation"*; an undeclared named parameter, a positional
beyond the declared arity, a missing required positional, a duplicate, and a value of the wrong
form are each a compile error. A parameter the author wrote is never silently discarded.

Closedness is what makes annotations readable. An open set defeats the purpose of the whole
mechanism: a reader who meets `` `retry(3) `` on a declaration cannot know whether it changes
codegen, is consumed by some generator, or does nothing at all, and must go looking outside the
file to find out — which is precisely the action-at-a-distance the language exists to avoid. It is
worse for a generating agent, which cannot tell a real capability from a typo, so a
silently-accepted misspelling becomes a silently-missing guarantee. A closed set means the
compiler rejects the typo and this document answers every question the reader can have.

The cost is real and accepted: a project cannot attach its own metadata to a declaration, and a
capability that turns out to be worth having has to be specified here and implemented rather than
prototyped in user code. That is the intended trade — the annotation namespace is part of the
language, not an extension point.

## 3. How to read an entry

Each entry states the same six things, in the same order. The last is load-bearing: an annotation
whose *"Read by"* is honestly "nothing" is not implemented, however precisely its effect is
described — and an annotation that cannot be implemented does not belong in this document.

| Field | Meaning |
|---|---|
| **Targets** | Which declaration kinds accept it. Must match `builtinMetas` in `compiler/internal/sema/meta.go`. |
| **Parameters** | Positional and named parameters. Must match `metaParamSpecs` in `compiler/internal/sema/metaparams.go`. An undeclared parameter is a compile error, never silently discarded. |
| **Effect** | The compiler decision it changes. |
| **Derivation** | Whether the property is also derived structurally, and whether this annotation asserts, denies, or overrides that derivation. |
| **Interactions** | Contradictions, implications, and mutual exclusions with other annotations. |
| **Read by** | The predicate or pass that consumes it. A new consumer in a new place is a change to this document, not an implementation detail. |

## 4. The axes

Annotations that are routinely confused for one another govern genuinely different relations. The
three below are independent, and a type may sit anywhere in their product.

| Axis | Question | Annotations |
|---|---|---|
| **Duplication** | May a second, independent value be produced from this one? | `` `copy ``, `` `clone ``, `` `single_owner `` |
| **Aliasing** | How many references may reach one value at once, and from how many goroutines? | `` `sharable ``, `` `not_sharable ``, `` `interior `` |
| **Transfer** | May a value move across a goroutine boundary? | `` `sendable ``, `` `not_sendable ``, `` `confined `` |

**Ownership is not one of the axes.** Every value has exactly one owner — the binding responsible
for running its `drop()` — and a move transfers it. No annotation changes that; the axes describe
what may be *derived from* an owned value, *reach* it, or *hold* it.

The distinction that matters most in practice: **aliasing is not duplication.** `Ref[T]` hands out
N references to *one* `T` and never produces a second one, so a type may be both `` `sharable ``
and `` `single_owner ``. Sharing a handle and copying a handle are different operations, and only
the second is what `` `single_owner `` denies.

**Layout is not an axis either.** Where a field lives is decided by the four-struct model (§5.2),
and the only annotation that moves one is `` `value ``. Padding, alignment and struct packing are
not expressible.

## 5. Assertions and denials

An annotation either **asserts** a property the compiler cannot verify, or **denies** one it would
otherwise derive. The two carry opposite risks, and that decides who may write them.

| | Meaning | Sound because | May be written on |
|---|---|---|---|
| **Assertion** | "trust me, this holds" | nothing — the compiler takes it on faith | `` `native `` declarations only |
| **Denial** | "do not trust the derivation" | it only ever *removes* a capability | any declaration |

**An assertion is restricted to `` `native ``.** Asserting a property the compiler cannot check is
sound only when the implementation being vouched for is the one the compiler itself emits — that
is exactly what `` `native `` means. A type written in Promise cannot make such a guarantee, and
does not need to: its properties are derived from its fields, so the derivation already has the
answer. Writing an assertion on it could only restate what is already true, or claim something the
fields contradict. The assertions are `` `sendable ``, `` `sharable `` and `` `interior ``.

**A denial is available everywhere**, because denying is always safe: it takes a capability away,
and no unsoundness follows from a type being treated as less capable than it is. It is also the
only way to express knowledge the fields do not carry — a type holding a thread-affine handle in
an `int` field derives as sendable, and its author is the only one who can say it must not cross a
goroutine boundary. The denials are `` `not_sendable ``, `` `not_sharable `` and
`` `single_owner ``.

`` `confined `` reads like an assertion but belongs with the denials, because it is
**self-enforcing**: the claim that a value never crosses a goroutine boundary is not taken on
faith, it is checked — the boundary rules reject any crossing — so the plain reference count it
licenses can never be reached by a value that violated it.

## 6. Index

The complete set. Anything not listed here is not an annotation; see §16.

| Annotation | Targets | Parameters | Effect |
|---|---|---|---|
| `` `copy `` | types, enums | — | Bitwise copy on assignment |
| `` `clone `` | types, enums | — | Synthesize a deep `clone() Self` |
| `` `single_owner `` | types, enums | — | Move-only handle; nothing may duplicate it |
| `` `sendable `` | types, enums | — | Assert: may move across a goroutine boundary |
| `` `sharable `` | types, enums | — | Assert: a reference may be aliased across goroutines |
| `` `not_sendable `` | types, enums | — | Deny sendability that fields would derive |
| `` `not_sharable `` | types, enums | — | Deny sharability that fields would derive |
| `` `confined `` | types, enums | — | Thread-confined; opts into the plain reference count |
| `` `interior `` | types (`` `native `` only) | — | Mutating methods callable through a shared borrow |
| `` `value `` | fields | — | Field lives in the value struct, copied with it |
| `` `raw `` | fields | — | Field types an LLVM type identifier directly |
| `` `abstract `` | methods | — | No body; a subtype must implement it |
| `` `structural `` | types, methods, enums | `protocol` (bool) | Satisfiable without `is`; `protocol: true` reserves requirement names |
| `` `open `` | types | — | Concrete type may be an `is` parent |
| `` `sealed `` | abstract/structural types | — | Hierarchy closed outside the declaring module |
| `` `native `` | types, methods | — | No Promise body; provided by the backend |
| `` `builtin `` | types, enums | `role` (identifier, required) | Fills a role the compiler depends on |
| `` `final `` | fields | — | Immutable after construction |
| `` `factory `` | methods | — | Receiver-less constructor with `` `mono `` placement |
| `` `global `` | methods | — | Namespaced function; no `this`, no `Self` |
| `` `mono `` | methods | — | Per-monomorphization function; no `this`, `Self` available |
| `` `public `` | types, enums, fields, methods, functions | — | Export from the module |
| `` `doc `` | any, parameters | `text` (string, required) | Attach documentation to the AST node |
| `` `deprecated `` | any, parameters | `message` (string, optional) | Mark deprecated |
| `` `test `` | functions | `expected`, `exclude`, `timeout`, `memory_limit` | Declare a test |
| `` `extern `` | functions | `symbol` (string, required) | Function linked by symbol name |
| `` `wasm_import `` | functions | `module`, `name` (strings, required) | Bind to a WASM host import |
| `` `target `` | types, enums, functions | `condition` (required) | Compile-time platform filtering |
| `` `embed `` | module-level getters | `path` (string, required); `compress` (bool) | Embed a file at compile time |
| `` `lifetime `` | parameters, functions, methods | `name` (identifier, required) | Explicit lifetime name |
| `` `serializable `` | types, enums | `tag` (string) | Synthesize `encode`/`decode` |
| `` `key `` | fields, variants | `name` (string, required) | Serialized name |
| `` `skip `` | fields | — | Omit from serialization |
| `` `include_none `` | fields | — | Emit the field when its value is none |
| `` `required `` | fields | — | Must be present when decoding |
| `` `flatten `` | fields | — | Inline the nested type's fields into the parent |

## 7. Duplication

### `` `copy ``

- **Targets** types, enums · **Parameters** — none
- **Effect** Assignment copies the value bitwise instead of moving it.
- **Derivation** Derived: a type whose fields are *all* `` `value ``-placed is a **pure value
  type** and is automatically `` `copy `` with no annotation. The annotation is for a type with
  instance fields that is nonetheless safe to copy bitwise.
- **Interactions** Every field must itself be copy — primitives, other copy types, `bool`, `char`,
  shared and mutable references, and tuples, optionals and arrays of those. `string` is **not**
  copy. A `` `copy `` type may not define `drop()`: a bitwise copy would duplicate the ownership
  claim that `drop()` releases. `` `clone `` on a `` `copy `` type is redundant and warns.
- **Read by** `isCopyField` / `validateCopyType` (`sema/meta.go`); assignment and argument
  lowering in codegen.

### `` `clone ``

- **Targets** types, enums · **Parameters** — none
- **Effect** Synthesizes a `clone() Self` method that deep-copies every field. Assignment still
  **moves** — `` `clone `` adds an explicit duplication operation, it does not change assignment.
- **Derivation** Not derived. Writing `clone() Self` by hand needs no annotation, and an explicit
  method takes precedence over the synthesized one.
- **Interactions** Requires every field to be cloneable; a field that transitively contains a
  `` `single_owner `` handle makes the type non-cloneable and the synthesis an error. The
  structural interface `Cloneable` (`modules/std/clone.pr`) is the type-level counterpart — any
  type with a `clone() Self` satisfies it, annotated or not.
- **Read by** `Named.IsClone()`; `validateCloneTypes` / `validateCloneInstance` (`sema/clone.go`).

### `` `single_owner ``

- **Targets** types, enums · **Parameters** — none
- **Effect** Declares a **move-only handle**: it has no `clone()`, and no context may duplicate it.
  The prohibition is transitive — a type that *contains* a single-owner handle is itself
  non-cloneable — and it reaches every structurally-duplicating context: slicing, `push` of a
  nested handle, nesting inside a second container, destructuring a variant out of a non-owned
  subject, and instantiating a generic whose body would copy it.
- **Derivation** Not derived; a handle's indivisibility is a property of what it names, invisible
  in its fields.
- **Interactions** **Orthogonal to `` `sharable ``.** `` `single_owner `` denies *duplication*;
  `` `sharable `` permits *aliasing*. `Ref[T]` aliases its element and never copies it, so
  `Ref[SingleOwnerT]` is well-formed: one handle, N references, dropped when the last reference
  goes. Contradicts `` `copy `` and `` `clone ``.
- **Read by** `isSingleOwnerType` / `firstNestedSingleOwnerHandle` and the container-element and
  generic-instantiation checks in `sema/clone.go`; the slice check in `sema/expr.go`.

## 8. Transfer and aliasing across goroutines

Two capabilities govern the goroutine boundary: **sendable** (values may be moved across one) and
**sharable** (a reference may be aliased across one, the type carrying whatever synchronization
that requires). What they mean, how they are derived from a type's fields, and what they require of
`Channel`/`Ref`/`Weak` element types is §6.5 of [language-design.md](language-design.md).

Both are derived structurally, so ordinary code never writes these annotations. They exist to
override the derivation for types whose safety the compiler cannot see — which is exactly the types
with no Promise-level fields to derive from: the native concurrency primitives.

### `` `sendable `` / `` `sharable ``

- **Targets** types (`` `native `` only) · **Parameters** — none
- **Effect** Assert the capability for the declaration, skipping field derivation.
- **Derivation** Overrides it — for the declaration. For a **generic** type the assertion cannot
  speak for every instantiation, so element types are still checked: a `` `sendable `` generic
  whose type argument is not sendable is rejected at the instantiation, not licensed by the tag.
  This is what keeps `Box[NonSendable]` an error while letting `Channel[T]` assert its own
  synchronization.
- **Interactions** **Requires `` `native `` (§5).** These are assertions, so only a declaration
  whose implementation the compiler emits may make one; on a type written in Promise the capability
  is derived from the fields, which already have the answer. Contradicts the matching `` `not_ ``
  form. Not accepted on an enum, which can never be `` `native ``.
- **Read by** `isSendableType` / `isSharableType` and `validateSendableInstance`
  (`sema/sendable.go`).

### `` `not_sendable `` / `` `not_sharable ``

- **Targets** types, enums · **Parameters** — none
- **Effect** Deny the capability even though the fields would derive it.
- **Derivation** Overrides it unconditionally, including for a generic's instances: a
  `` `not_sharable `` origin is not sharable whatever its type arguments.
- **Interactions** Contradicts the matching positive form.
- **Read by** `isSendableType` / `isSharableType` (`sema/sendable.go`).

### `` `confined ``

- **Targets** types, enums · **Parameters** — none
- **Effect** Declares the type **thread-confined**: no value of it may cross a `go`/channel/`Task`
  boundary, and a `Ref`/`Weak` of it therefore uses a plain, non-atomic reference count.
- **Derivation** **Atomicity itself is not declared — the compiler decides it.** A `Ref` that never
  crosses a boundary gets a plain count; one that may be shared across goroutines gets an atomic
  one. Semantics are identical either way, so this is a transparent implementation detail (§17.3).
  `` `confined `` is the *explicit opt-in* for a type the analysis cannot see through, and it is
  sound in only one direction: an atomic count is safe everywhere, while a plain one requires proof
  that the value stays on one thread. That proof is what the annotation asserts and what the
  boundary rejection below enforces.
- **Interactions** Implies `` `not_sharable ``, and is mutually exclusive with `` `sharable ``. A
  `` `confined `` element also exempts `Ref[T]`/`Weak[T]` from the usual requirement that `T` be
  sendable and sharable — the question cannot arise for a value that never crosses a boundary.
- **Read by** `types.IsConfined`; `isSendableType` and `validateSendableInstance`
  (`sema/sendable.go`), which perform the boundary rejection; `refIsAtomic` (`codegen/sched.go`),
  which selects the counter.

### `` `interior ``

- **Targets** types · **Parameters** — none
- **Effect** Exempts the type from *"mutation through a shared borrow is rejected"* (§6.2): its
  mutating methods and setters keep a shared `this` receiver and stay callable through a `&`
  borrow. This is what lets a captured channel be sent to, and a `Mutex` be locked, through a
  shared reference.
- **Derivation** Not derived.
- **Interactions** **Requires `` `native ``.** The exemption is sound only when the implementation
  synchronizes internally, and a type written in Promise can neither express that guarantee nor
  have the compiler check it — so the annotation is limited to declarations whose body codegen
  emits. Since a native type must itself be predeclared in the compiler's universe, user code
  cannot reach this annotation at all. Not accepted on an enum, which can never be `` `native ``.
  Independent of the capability annotations, though it co-occurs with them on the same primitives.
- **Read by** `recvIsInterior` (`sema/expr.go`); the setter-receiver decision in
  `resolveMethodSignature` (`sema/decl.go`).

## 9. Field placement

A type's fields are distributed across the four-struct model (§5.2). Placement is per field, and
there are exactly two placements: the heap instance struct (the default) and the value struct.

### `` `value ``

- **Targets** fields · **Parameters** — none
- **Effect** Places the field in the **value struct**, embedded directly and copied whenever the
  value is passed or assigned — no heap allocation and no indirection. Best for small,
  frequently-accessed data.
- **Derivation** Not derived; instance placement is the default.
- **Interactions** A type whose fields are *all* `` `value `` is a **pure value type**: register
  resident, automatically `` `copy ``, no `drop()`, no failable `new()`, no `` `abstract ``
  methods. Its only state-bearing `is` parent may be another pure value type — a fieldless child is
  a layout-preserving newtype — while `` `structural `` parents add no state and are always
  permitted. Instance placement is the default and has no annotation: there is no second spelling
  for writing nothing.
- **Read by** `resolvePlacement` (`sema/decl.go`); `detectValueType`; `computeValueTypeLayout`
  (`codegen/layout.go`).

**There are no per-type or per-monomorphization fields.** Such a field would be global mutable
state — reachable from anywhere that can name the type, absent from every function signature, and
unsynchronized across goroutines — which §9.2's *No Module-Level Variables* rules out for the
language as a whole, and a field placement must not reintroduce it through a side door.
Per-monomorphization *data* is expressed as a `` `mono `` **method** returning the value
(`sprite_path() string \`mono`), which needs no storage, cannot be mutated, and is visible at
every call site.

### `` `raw ``

- **Targets** fields · **Parameters** — none
- **Effect** The field's type is an LLVM type identifier used directly, bypassing Promise's type
  layout. For native types and foreign structs whose shape is dictated from outside.
- **Interactions** Used with `` `native `` types and `` `extern `` boundaries.
- **Read by** field type resolution in `sema/decl.go`; `codegen/layout.go`.

## 10. Type shape, dispatch and construction

### `` `abstract ``

- **Targets** methods · **Parameters** — none
- **Effect** The method has no body; a subtype must implement it. A type with any abstract method
  cannot be instantiated and is implicitly **open** — its purpose is to be extended. Declaring at
  least one abstract method is also what makes a type structurally satisfiable.
- **Interactions** This is a **method** annotation: a type becomes abstract by declaring an
  abstract method, not by carrying a tag. `` `open `` on such a type is redundant and rejected.
  Abstract factories get an implicit `Self` return. A pure value type may declare none, its
  dispatch being always static. Enum methods may not be abstract.
- **Read by** method definition in `sema/decl.go`; `needsVtable` and vtable emission in
  `codegen/rtti.go`.

### `` `structural ``

- **Targets** types, methods, enums · **Parameters** `protocol` (bool)
- **Effect** On a type: the interface may be satisfied without an `is` declaration — any type with
  matching method shapes conforms. `protocol: true` additionally reserves the interface's
  requirement names, making a same-name method with an incompatible signature an error;
  `protocol: false` on a method, type or enum exempts it from that check.
- **Interactions** Implicitly open. A `` `structural `` type whose fields are all `` `value `` is a
  value type, **not** an interface view: it stays register-resident and can only be satisfied by
  its own layout-sharing value newtypes. Requires at least one abstract method to be structurally
  satisfiable.
- **Read by** `types.Implements`; structural-default synthesis in `codegen/mono.go`.

### `` `open `` / `` `sealed ``

- **Targets** `` `open `` types; `` `sealed `` abstract or structural types · **Parameters** — none
- **Effect** Concrete types are **sealed by default**: no other type may declare `is` on them.
  `` `open `` permits it. `` `sealed `` on an abstract or structural base closes the hierarchy
  *outside* the declaring `promise.toml` module while leaving it extensible within — the standard
  shape for a closed tagged union when an enum will not serve. The seal is transitive: no subtype
  of a sealed type may be `` `open ``, at any depth.
- **Interactions** Sealing is per type, not inherited — a concrete child of an open or abstract
  parent is itself sealed. `` `open `` is rejected on abstract and structural types, which are
  already open; `` `sealed `` is rejected on concrete types, which have no extensibility to scope.
  Each kind of type has exactly one annotation that flips its default, so there is never more than
  one spelling for a given transition.
- **Read by** inheritance checking in `sema/decl.go`.

### `` `native ``

- **Targets** types, methods · **Parameters** — none
- **Effect** Declares something **the compiler already knows how to implement**. There is no Promise
  body and none is needed: codegen emits the implementation, and for a type the memory layout as
  well. The declaration exists to give that built-in a name, a signature, and a place in the type
  system.
- **Not writable by user code, by design.** The annotation names an implementation the compiler
  holds. A user writing it would be naming one that does not exist — nothing would provide the
  body. A native *type* must therefore be predeclared in the universe (`types/universe.go`), and a
  declaration naming one that is not is rejected with *"native type not found in universe"*. This
  is also what bounds `` `interior ``, which requires `` `native ``.
- **Interactions** A native type has no Promise-level fields, so it can derive no capability
  structurally — its `` `sendable ``/`` `sharable ``/`` `single_owner ``/`` `interior ``
  annotations are what carry those properties, and per §1 they are read exactly like a source
  type's. Field validation of a `` `sendable `` assertion is skipped, there being no fields to
  check.
- **Read by** `defineType` / `defineMethod` (`sema/decl.go`); intrinsic dispatch in
  `codegen/compiler_native_*.go`.

`` `native `` and `` `builtin `` are the two annotations describing a compiler relationship, and
they partition it: `` `native `` says *the compiler implements this*, `` `builtin `` says *the
compiler depends on this*. A type carries at most one of them.

### `` `builtin ``

- **Targets** types, enums · **Parameters** `role` (identifier, required)
- **Effect** Declares that this type fills a **role the compiler depends on**. Unlike `` `native ``
  the compiler does not implement it — the declaration is ordinary Promise, with fields and bodies
  — but the compiler binds syntax to it, constructs values of it, or relies on its shape.
- **Derivation** Not derived, and deliberately not inferred from the type's name. The binding is a
  statement the declaration makes, so renaming the type is a rename and nothing more.
- **Interactions** The role vocabulary is closed, and the mapping between roles and declarations is
  **one to one**. Four things are therefore compile errors: a role no type claims, a role two types
  claim, a claim naming a role the compiler does not have, and a claim from outside a catalog
  module. The last is the boundary that keeps user code from capturing `{:}`; catalog modules ship
  with the compiler and are reviewed alongside it, which is what makes them eligible — not any
  property of `std`, which is a module like any other.
- **Read by** the role table in `sema`; the syntax-lowering and construction sites that consume
  each role.

The roles, which are the complete set:

| Role | Declared by | What depends on it |
|---|---|---|
| `map` | `Map[K, V]` | `{:}` literals and the `map[K, V]` alias |
| `range` | `Range[T]` | `..` and `..=` construct one |
| `error` | `error` | the error slot of a failable result |
| `iterator` | `Iterator[T]` | the `iter` alias, and the protocol `for`-`in` requires |
| `stream` | `Stream[T]` | the `stream` alias, and asynchronous iteration |
| `embedded_file` | `EmbeddedFile` | the result of `` `embed `` on one file |
| `embedded_files` | `EmbeddedFiles` | the result of `` `embed `` on a glob |

A role is claimed for what the compiler must *do* with the type, never for what it must *know
about* it — so a type appears here only because syntax denotes it, the compiler builds one, or its
shape is depended upon. A container is not listed because the compiler wants to know how it
duplicates its elements; that is a property, and §1 puts it out of reach.

### `` `final ``

- **Targets** fields · **Parameters** — none
- **Effect** The field is immutable after construction: it may be assigned only in a `new()` or
  `` `factory `` body. A value correct at construction stays correct perpetually.
- **Read by** assignment checking in `sema/expr.go`.

### `` `factory ``

- **Targets** methods · **Parameters** — none
- **Effect** A receiver-less constructor. Implies `` `mono `` placement — all generics resolved,
  since a factory on `Box[T]` must know which `T` to build — and returns `Self` or a child. An
  abstract factory declared without a return type gets an implicit `Self` return.
- **Interactions** No `this`. Rejected on operator methods and on enum methods. Not inherited: a
  factory may be called only on its declaring type. Factories match factory-to-factory — an
  instance method does not satisfy a factory requirement, or vice versa. A `` `factory `` is the
  only code besides `new()` that may assign a `` `final `` field.
- **Read by** `sema/decl.go`; call resolution in `sema/expr.go`.

### `` `global `` / `` `mono ``

- **Targets** methods · **Parameters** — none
- **Effect** The two receiver-less method placements. `` `global `` is a namespaced function: no
  `this`, no `Self`, one for the whole type. `` `mono `` is per-monomorphization: no `this`, but
  `Self` is available and bound to the declaring type.
- **Interactions** Both are **method** placements only; there are no per-type or
  per-monomorphization fields (§9). `` `global `` is rejected on generic types and *is* inherited,
  the type being purely a namespace; `` `mono `` is **not** inherited — it binds `Self` to its
  declaring type, so calling it through a subtype would bind the wrong one. Both are rejected on
  operator methods, which always take the left operand as receiver, and on enum methods. A
  receiver-less member has no vtable slot: there is no value at the call site to read a vtable
  pointer from, so it resolves at compile time to the type that declares it.
- **Read by** `resolvePlacement` (`sema/decl.go`); static call resolution in `sema/expr.go`.

## 11. Visibility and documentation

### `` `public ``

- **Targets** types, enums, fields, methods, functions · **Parameters** — none
- **Effect** Exports the declaration from its module. Everything is module-private by default.
- **Interactions** A `` `public `` declaration must carry a `` `doc ``; omitting one is an error.
  The exported surface is what other modules — and the agents generating against them — read
  instead of the implementation, so an undocumented export defeats the purpose the annotation
  exists for. **One exemption:** a member implementing a requirement declared by a `` `structural ``
  or `` `abstract `` parent inherits that requirement's documentation and needs none of its own.
  Restating a known contract at each implementation is noise, not documentation.
- **Read by** `SetExported` (`sema/decl.go`); module import resolution.

### `` `doc ``

- **Targets** any declaration, including parameters · **Parameters** `text` (string, required)
- **Effect** Attaches documentation to the AST node. Unlike a comment it is preserved in the parsed
  tree, unambiguously bound to its declaration, and available to tooling and to agents at compile
  time.
- **The text is Markdown.** It is authored as Markdown source and consumers render it as such —
  `promise doc` emits it into a Markdown document, and an editor or agent displaying a declaration
  renders rather than escapes it. So a doc string may carry inline code spans, links, lists and
  emphasis, and characters that are Markdown syntax carry their Markdown meaning rather than being
  literal. Referring to an annotation therefore needs Markdown's own escape for a literal backtick:
  write ``` `` `public `` ``` to render `` `public ``.
- **Required on** every `` `public `` declaration, except one that implements an already-documented
  interface requirement — see `` `public ``.
- **Read by** `extractDoc` (`sema/meta.go`); `promise doc`; the bindgen and formatter passes.

### `` `deprecated ``

- **Targets** any declaration, including parameters · **Parameters** `message` (string, optional)
- **Effect** Marks the declaration deprecated. Using it warns, quoting the message.
- **Interactions** The message is positional — `` `deprecated("use NewThing instead") `` — and that
  is its only spelling. There is no version or date parameter: a value with no defined format and
  no consumer is decoration, and if deprecation ever needs to be scoped to a release, the scoping
  rule has to be specified before the parameter that carries it.
- **Read by** `extractDeprecated` (`sema/meta.go`); use sites in `sema/expr.go`.

## 12. Testing

### `` `test ``

- **Targets** functions · **Parameters** `expected` (string), `exclude` (target condition),
  `timeout` (duration string), `memory_limit` (size string) — all named
- **Effect** Declares a test. Without `expected` the function is a **batch test**: it asserts, and
  every such function in the files passed together compiles into a single binary. With `expected`,
  a `main()` becomes a **snapshot test**: its own binary, whose stdout is compared against the
  string. Batch tests are strongly preferred — the cost of a test run is dominated by how many
  binaries are compiled.
- **Interactions** `timeout` is a positive decimal number followed by one of `ns`, `us`, `ms`, `s`,
  `m`, `h` — `500ms`, `2s`, `1.5m`. A unit is required, a negative value is rejected, and the unit
  set is exactly those six. The CLI's `-timeout`/`-timeout-scale`/`-timeout-min`/`-timeout-max`
  modify the result. `memory_limit` is a positive decimal number followed by one of `B`, `KB`,
  `MB`, `GB` or the binary `KiB`, `MiB`, `GiB`; a unit is required, and `"0"` alone opts out.
  `exclude` takes the narrower target-condition form — an identifier or a `||` chain of
  identifiers.
- **Read by** test discovery and the harness in `cmd/promise`.

A test that leaks memory fails. There is no annotation to waive that: the repository holds zero
leaks, and a leak is a regression to be fixed rather than declared acceptable.

## 13. Foreign interfaces

### `` `extern ``

- **Targets** functions · **Parameters** `symbol` (string, required)
- **Effect** Declares a function whose body is not in Promise and whose linkage is by **symbol
  name**. The declaration gives the signature; the call is emitted against `symbol`.
- **Interactions** This is the second of the two ways a declaration reaches an implementation the
  compiler provides, and they differ in what is being named. `` `native `` names a *member the
  compiler knows* — codegen recognises it and emits an intrinsic, with no symbol involved, and it
  can only be written on a declaration already predeclared in the universe. `` `extern `` names a
  *symbol*, so it works for an ordinary free function in ordinary module source and is how the
  standard library and catalog modules call into the platform layer the backend emits. Pairs with
  `` `raw `` fields for structs whose layout is dictated from outside.
- **Read by** `sema/decl.go`; `codegen/extern.go`.

The symbol is required rather than defaulted. A default linkage name is invisible at the
declaration and silently follows a rename, which is the kind of action-at-a-distance the language
exists to avoid — every `` `extern `` states the symbol it binds.

**The set of bindable symbols is closed, and the symbol is validated against it at the
declaration.** The compiler emits the platform layer itself, so it knows the complete set of
symbols an `` `extern `` may name; that set is registered in one place, and naming anything outside
it is a compile error reported at the annotation. This is the same discipline as the annotation set
(§2) and the target-condition identifiers: a fixed vocabulary, checked where it is written.

Validation belongs at the declaration rather than at link time for two reasons. A linker
diagnostic cannot point at Promise source — it names an object file and an internal symbol, so the
reader is told where the *call* was emitted rather than where the mistake was written. And a linker
never sees a declaration that is never called, so an `` `extern `` naming a symbol that does not
exist can sit in a module indefinitely and fail only for whoever first calls it.

### `` `wasm_import ``

- **Targets** functions · **Parameters** `module` (string, required), `name` (string, required)
- **Effect** Binds the function to the WASM host import `name`, imported from host module `module`.
- **Read by** validation in `sema/decl.go`; `WasmImportMod`/`WasmImportName` in
  `codegen/layout.go`; emitted by the bindgen passes.

## 14. Compile-time

### `` `target ``

- **Targets** types, enums, functions · **Parameters** `condition`, a required target condition
- **Effect** Filters the declaration out of compilation on non-matching targets. A filtered
  declaration is not type-checked, so a malformed annotation on it surfaces only on the targets
  that compile it — with the exception of `` `target `` itself, which is validated on the filtering
  path too.
- **Interactions** The *target condition* — its closed set of platform identifiers and the
  operators that combine them — is defined once in §8.1 of
  [language-design.md](language-design.md), and `` `test ``'s `exclude` takes the narrower form of
  the same grammar. The set is closed: an identifier outside it is a compile error, and the
  diagnostic's list is derived from the accepted set so the two cannot drift.
- **Read by** `Info.FilteredDecls` (`sema/check.go`); the identifier check in
  `sema/metaparams.go` against `ValidExcludeIdents` (`sema/target.go`).

### `` `embed ``

- **Targets** module-level getters · **Parameters** `path` (string, required); `compress` (bool)
- **Effect** Embeds the file or glob tree at `path` into the binary at compile time, exposed
  through the getter as an `EmbeddedFile` or `EmbeddedFiles`.
- **Read by** the embed pass in `sema`; `codegen`; §8.6 has the path and glob rules.

### `` `lifetime ``

- **Targets** parameters, functions, methods · **Parameters** `name`, a required identifier
- **Effect** Names a lifetime explicitly, overriding elision. The compiler uses aggressive lifetime
  elision (§6.3), so this is almost never needed.
- **Read by** `Param.SetLifetime` / `Param.Lifetime` (`types/signature.go`).

## 15. Serialization

`` `serializable `` synthesizes the `encode`/`decode` pair; the field annotations shape the result.
[serialization.md](serialization.md) and [schema.md](schema.md) are the full specifications.

### `` `serializable ``

- **Targets** types, enums · **Parameters** `tag` (string)
- **Effect** Synthesizes `encode(Encoder)` and `decode(Decoder)` from the declaration's fields. On
  an enum, `tag` names the discriminator field.
- **Read by** `processSerializableType` (`sema/decl.go`, `sema/serialize.go`).

### Field annotations

| Annotation | Parameters | Effect |
|---|---|---|
| `` `key `` | `name` (string, required) | Use `name` as the serialized name instead of the field or variant name. |
| `` `skip `` | — | Omit the field from both encoding and decoding. |
| `` `include_none `` | — | Emit the field even when its value is none, instead of omitting it. |
| `` `required `` | — | The field must be present when decoding; its absence is a decode error. |
| `` `flatten `` | — | Inline the nested type's fields directly into the parent object rather than nesting. |

- **Targets** fields, and `` `key `` also variants · **Read by** `sema/serialize.go` and the
  synthesized `encode`/`decode` bodies.

## 16. Not annotations

These are **not** part of the language. Each was considered and rejected, and the reason generalizes
— so the entries stay here to keep them from being reintroduced. A compiler that accepts one of
these names is wrong.

| Name | Why not, and what to use |
|---|---|
| `` `variant `` | There is no per-monomorphization *field* placement (§9). Per-monomorphization behavior is a `` `mono `` **method**; per-monomorphization data is that method's return value. |
| `` `packed `` | Field layout is determined by the four-struct model, not by a per-type padding directive. A foreign struct whose shape is dictated from outside uses `` `raw `` fields. |
| `` `align(N) `` | Same: alignment is the layout model's to decide. A per-type override is an ABI-visible promise the language does not make. |
| `` `inline `` | Inlining is the optimizer's decision. `opt` inlines across modules at `-O1`, and a hint the compiler is free to ignore is surface that teaches nothing. |
| `` `instance `` | Instance placement is the default (§9). An annotation for it would be a second way to write nothing, and its method form named the receiver-ful default that §9.2 already says needs no annotation. Use `` `value `` to move a field to the value struct. |
| `` `unsafe `` | Nothing checked it and nothing was unlocked by it. The `unsafe { }` block and the raw pointer type `T*` went with it: a raw pointer is only ever needed by an implementation the compiler emits, and `` `native `` already names those. FFI uses `` `extern `` for the call and `` `raw `` for foreign struct layout. |
| `` `allow_leaks `` | A leaking test is a defect, not a configuration. |

## 17. Adding an annotation

1. **Specify it here first** — an entry with all six fields of §3. If *"Read by"* cannot be filled
   in, the design is not finished; if it can only be filled in with "nothing", the annotation does
   not belong in the language.
2. Register it in `builtinMetas` (targets) and `metaParamSpecs` (parameters).
3. Store it as a flag on `types.Named`/`types.Enum`, or on the field or parameter, with a getter.
4. Set the flag from the annotation in `sema/decl.go` — on **both** the native and the
   source-declared paths, which are separate branches with separate returns.
5. Have every decision read the flag. Do not add a type-identity or type-name test; per §1 that is
   the defect this rule exists to prevent.
6. Cover it: a sema test that it is accepted on each declared target and rejected on the others, and
   a behavioral test that the flag changes what it claims to change.
