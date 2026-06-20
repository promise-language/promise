# Cloud Persistence — Design Proposal

`modules/cloud` (working name — see §12) provides durable, schema-driven, multi-process
shared state for Promise programs. A client defines `` `serializable `` types and
performs typed `get` / `put` / `list` / `allocate` operations through a single endpoint.
The server validates against the type's `schema.Type` (see `docs/schema.md`), mediates
references, and is allowed to send back more data than the client asked for to preempt
future round-trips.

---

## 1. Goals and Non-Goals

### Goals

- **Schema-driven.** No client-side ORM, no hand-written JSON shapes. Every operation
  is over a Promise type the compiler already understands.
- **One endpoint, batched operations.** A client request carries multiple operations
  of mixed kinds; the server returns one response. Round-trip count is the optimization
  axis, not bandwidth.
- **Server may return more than asked.** When the server has to load related state to
  answer a request anyway (for instance, to maintain an inverse-relation invariant on
  a `put`), the affected entities go into the response so the client cache stays
  warm.
- **Wire-stable identity.** `Hash128` from `docs/schema.md` is the type / field /
  reference identity in storage. Source renames don't break stored data; pinning
  via `` `id `` lets the source change without the storage moving.
- **Pluggable backend.** The server side is an interface. In-memory, single-process
  Sqlite, and Firestore are first-party targets; community implementations can fill in
  others.

### Non-Goals

- **Not a query language.** No SQL-equivalent, no joins, no projections. Get by id,
  list by type, traverse references. Sophisticated querying is a separate concern.
- **Not a CRDT.** Concurrency is per-request atomic at the server. Conflict resolution
  is last-writer-wins or app-level — there is no built-in merge.
- **Not a sync engine.** "Sync" in §3 refers to the request/response pattern, not to
  background bidirectional replication. A library on top of `cloud` could implement
  that, but the protocol does not.
- **Not a blob store reinvention.** Large binary payloads piggyback on the same
  endpoint (§8) but the storage backend delegates to whatever native blob store the
  deployment environment provides.

---

## 2. Identity

Every type, field, and reference is identified by a `Hash128`. The composition rules
are defined in `docs/schema.md` §4. This doc only adds the operational implications:

- **Type id** identifies the schema of an entity. Two builds of the same Promise
  source produce the same type ids; this is the wire-stability the storage layer
  pins to.
- **Field id** identifies a single field in an instance. Wire-side, an instance is
  `map[Hash128, Value]` keyed by field id, not by source name. A field rename without
  `` `id `` pinning is a hard schema break.
- **Entity id** is a single 128-bit identifier — globally unique across all types
  and all instances. It is **the** persistent address of one row. Nothing else is
  part of the identity. The id can come from either side: the server can allocate
  fresh ids on demand (the typical pattern, via the `Allocate` op in §4.1), or the
  client can generate them locally (random UUIDs, hashes of payload, deterministic
  application keys) and submit them on a `Put`. The server decides per type whether
  client-supplied ids are accepted.

```promise
// EntityId is just a 128-bit value. No struct, no type+instance pair —
// the type comes from elsewhere (schema of the field that references it,
// or an explicit op parameter when one is needed).
type EntityId = Hash128;
```

The prototype encoded entity ids as `T200I12` strings on the wire — type and
instance concatenated. Promise drops the bundled type entirely. Earlier drafts of
this design (see §14 Q1) carried `{Hash128 type_id, Hash128 instance_id}` for the
same reason as the prototype: to let the server route a request to the right
storage shard without an extra index lookup.

### 2.1 Where the concrete type actually lives

Each entity has exactly one concrete type, fixed for its lifetime — the type at
creation time. Sub-typing means a `Ref[Animal]` field can hold an id whose
entity is a `Dog`, a `Cat`, or any other `is Animal` type, so the *field
schema* gives a **type bound**, not the concrete type. To know what an entity
actually is, you have one of three things in hand:

- **The entity heap value.** Every Promise instance carries a vtable / RTTI
  pointer that identifies the concrete type. If you already have the entity
  loaded (the common case after a Get, a list iteration, or a fresh `create`),
  the concrete type is right there — no lookup needed.
- **The server record.** The server stores the concrete `type_id` next to the
  payload when the entity is first written, and never lets it change. Any
  operation that touches the row (Get, Put-update, inverse maintenance) has
  the type as part of the row.
- **A round-trip.** Resolving an entity's type from a bare id requires a server
  lookup — but that lookup happens anyway, because the server has to find the
  row to check existence, fetch the data, or route the request to the right
  shard. The type comes along for free with the row lookup; it is not an
  extra round-trip.

That is the load-bearing observation: identity is one 128-bit value because
type information is either already on hand (heap value) or recovered as a
free side-effect of the lookup the server was going to do anyway.

### 2.2 Why the bundled `{type_id, instance_id}` is rejected

- **A redundant copy on the wire is a bug source.** If the client's belief about
  an entity's type drifts from the server's record (renamed type without an
  `` `id `` pin, schema rewind, a client built against a stale module epoch),
  a bundled `(type_id, instance_id)` causes silent mis-routing or a confusing
  "type mismatch" error far from the cause. With identity = id alone, the
  server checks the type against its own record and the failure mode collapses
  to "entity not found" or "wrong type for this field at decode time".
- **`Ref[T]` doesn't pin the concrete type anyway.** Because `T` is a bound, not
  a leaf, the bundled type id on the wire would be either the declared bound
  (useless — server already knows the field's schema) or the concrete type
  (redundant with the entity record). Either choice is dead weight.
- **Backends already need a global id index.** Cross-type references force every
  backend to resolve "what row is this id?" without knowing the type up front,
  so the type-in-id was never doing meaningful routing work that the index
  didn't already do. The server lookup is the path; the type is the
  by-product.

### 2.3 No client-side type hint on Get

A client never knows an entity's concrete type from its id alone — a
`Ref[Animal]` may hold an id whose entity is a `Dog`, and the client has no
way to know which subtype until it has the entity in hand. Any "hint" the
client could supply would be at best the declared bound (`Animal._type_id`),
which is useless to a sharded backend because the actual row lives in the
`Dog` shard, not the `Animal` shard. A bad hint is worse than no hint: it
either forces the backend to scan the wrong shard first and fall back, or
the backend trusts the hint and returns "not found" for an entity that
exists.

Therefore `Op.Get` takes the id alone. Backends are required to maintain a
global id index that maps any id to its row (and, on per-type-sharded
storage, to the shard the row lives in). The lookup the server needs to do
anyway to find the row is the same lookup that resolves the type — no
client-side hinting needed.

### 2.4 128-bit identity and the `u128` dependency

`Hash128` is defined in [docs/schema.md](schema.md) §3 as a value-type wrapper
around a single native `u128`. The wrapper exists so the type system can tell
a content hash apart from a plain numeric u128 at compile time; the in-memory
representation is one 128-bit word. Equality, hashing, and copying are
single-instruction native operations, and the wire format is just the
underlying 128 bits.

This is why cloud persistence v1 lists large integers as a prerequisite (§13):
the entire identity story (entity ids, type ids, field ids, references) rides
on `Hash128`, which only exists in its single-word form once `u128` lands per
[docs/large-integers.md](large-integers.md).

### 2.5 The `` `entity `` annotation

User code does not work with `EntityId` directly. Types that should be stored as
separate cloud rows are marked with `` `entity `` (defined in `docs/schema.md` §4.4):

```promise
type Item `entity {
    string title;
    string description;
    Ref[Folder]? folder;            // reference to another entity (§7)
}

type Folder `entity {
    string name;
    Inverse[Item] items `inverse(Item.folder);   // server-maintained back-references (§7)
}
```

`` `entity `` does three things:

1. **Implies `` `serializable ``.** The type round-trips through the
   `Encoder`/`Decoder` interfaces; encode/decode are synthesized by the compiler.
2. **Synthesizes an implicit id.** The compiler adds a hidden `Hash128` id field,
   exposed as a getter (`item.id`) and as a constructor parameter
   (`Item(id: my_uuid, title: "x")` for client-id workflows; the parameter is
   optional and defaults to a server-allocated id).
3. **Changes how the type encodes when it's a field of another type.** A
   plain `` `serializable `` value (like `Address`) gets inlined into its parent's
   encoded data. An `` `entity `` value referenced from another type encodes as
   id-only — the full row lives elsewhere in storage and is fetched on demand.

The distinction matters because it determines what counts as "one row" in the
backend: an `` `entity `` is a row, a `` `serializable `` value embedded in an entity
is part of that row. Cross-row pointers always go through the entity boundary.

References between entities are exposed in user code through two value types
defined in §7: `Ref[T]` for forward references, `Inverse[T]` for server-maintained
back-references. On the wire, both reduce to `EntityId` values.

---

## 3. Sync Protocol

One HTTP endpoint, one POST, one request, one response. The Promise-side wire types
are `` `serializable `` enums — any Promise client and any Promise-aware server agree
on the shape because it's just `schema.of[Request]()` / `schema.of[Response]()`.

### 3.1 Request

```promise
type Request `public `serializable
    `doc("A batched cloud-persistence request. All operations are processed within one server-side transaction.") {
    Op[] ops `doc("Operations in declaration order. Allocate runs first, then Put, then Get, then List, then Blob.");
    bool include_stats = false `doc("Set true to receive backend telemetry in the response.");
}

enum Op `public `serializable {
    Allocate(Hash128 type_id, int count = 1)
        `doc("Reserve `count` fresh entity ids in the given type's namespace. Server returns the allocated EntityIds (bare 128-bit values) in the response."),

    Put(EntityId id, Hash128 type_id, map[Hash128, Value]? data)
        `doc("Create or update an instance. `type_id` is carried explicitly (not part of the EntityId) so the server can validate the payload against the type's schema. `data` is `none` to delete. Server enforces schema and updates inverse relations."),

    Get(EntityId id)
        `doc("Fetch one instance by id alone. The id is globally unique across all types, so the server does not need a type hint to find the row. Server may also include related instances it had to load anyway."),

    List(Hash128 type_id, ListFilter? filter)
        `doc("Enumerate all instances of a type. Returns ids only — fetch payloads with Get."),

    Blob(BlobOp op)
        `doc("Get / set / exists for binary blobs by string key. See §8."),
}
```

`Value` is the wire encoding of a single field's value. It is the same encoding
`` `serializable `` produces — meaning `int`, `string`, `bool`, `T?`, `T[]`,
`map[K, V]`, nested objects (recursively), and `EntityId` for references. The cloud
module reuses the encode/decode the language already synthesizes; there is no
parallel value type.

### 3.2 Response

```promise
type Response `public `serializable {
    int status `doc("HTTP-style status code.");
    EntityId[] allocated `doc("Ids returned by Allocate ops, in op-declaration order.");
    map[EntityId, map[Hash128, Value]?] entities
        `doc("Instances returned by Get / List ops AND any preempted entities (§6). Value is `none` for deleted/missing.");
    map[Hash128, EntityId[]] lists
        `doc("Type id → instance id list, returned by List ops.");
    BlobResponse? blob;
    Stats? stats;
}
```

Two design notes:

- **Op order is fixed by op kind, not by request order.** Allocate runs first (so
  Puts can reference fresh ids), then Put, then Get, then List, then Blob. This
  matches the prototype's `sync.go` ordering and gives the client a deterministic
  view-of-the-world model. A request that needs cross-op ordering between two Puts
  must split into two requests.
- **`entities` carries both requested and preempted data.** The map shape lets the
  server inject any number of extra `EntityId → fields` pairs without a separate
  preempt section. Clients merge the whole map into their cache.

### 3.3 Endpoint

One HTTP route, conventionally `POST /sync`. Request and response bodies are JSON
(via `json.encode_string[Request]` / `json.decode_string[Response]`). Other transports
(gRPC, MessagePack, raw TCP) implement the same Promise types — wire format is a
deployment choice, not a protocol concern.

---

## 4. Operations in Detail

### 4.1 Allocate

```promise
Op.Allocate(type_id: Foo._type_id, count: 1)
```

Server picks fresh `EntityId` values (bare 128-bit ids) for the given type and
returns them in `response.allocated` in op order. Allocation is typically monotonic
per type — the server maintains a counter — but the protocol does not require
monotonicity; a backend that wants UUID-style ids may return random ones. The
type id is *not* embedded in the returned id; the server records the type
separately so future Gets can be answered with id alone.

Clients that want a Put-then-Get pattern (allocate id, fill payload, commit) batch
it as a single request: `Allocate` then `Put` referencing the soon-to-be-allocated
id. The server resolves placeholders in op order, so the Put sees the freshly
allocated id.

`Allocate` is one of two ways to mint an instance id; the other is client-side
generation, which skips the round-trip entirely — see §4.2.

### 4.2 Put

```promise
Op.Put(
    id: some_entity_id,
    type_id: Foo._type_id,
    data: { Foo._field_name_id: Value.String("alice"), ... },
)
```

Put creates or updates an instance. `data: none` deletes. The `id` may be one
the server allocated earlier (via `Allocate`, §4.1) or one the client generated
locally — random UUIDs, content-addressed hashes, deterministic application
keys, etc. The server enforces a per-type policy on whether client-supplied ids
are accepted; an unaccepted client-id Put returns a `client_id_not_allowed`
status without touching the backend.

`type_id` is a separate op parameter, *not* part of the `EntityId`. This is the
authoritative type for the Put: the server validates the payload against the
type's schema and, on first write of a never-seen id, records this type as the
id's permanent type. On a re-write (the id already exists), the server verifies
the incoming `type_id` matches the recorded type and rejects the op with a
`type_mismatch` status if they disagree — the type of an existing entity is
immutable.

The server:

1. Resolves the type from `type_id` against its schema registry (per
   `schema.Type.Object.fields`).
2. Validates every key in `data` is a known field id of that type.
3. Validates every value matches the field's `Type` — scalar kinds, optional vs
   required, etc.
4. Validates id ownership — see above.
5. For an existing id, confirms the recorded type equals `type_id`.
6. Updates inverse relations (§7). Any entity touched by inverse maintenance is
   added to `response.entities` so the client cache reflects reality.
7. Writes through to the storage backend.

Validation errors short-circuit the request — no partial application. Status field
in the response carries the error class; details go in a structured error type
(omitted here for brevity, modeled after `auth.AuthError`).

Client-generated ids are useful when:

- The id is derivable from the payload (content-addressed storage — store the same
  payload twice, get one entity).
- Round-trip latency matters (offline-capable clients pre-mint ids and sync later).
- An external system already owns the id space (e.g., importing rows whose primary
  key was assigned by another database).

Server-allocated ids are useful when the server is the authoritative id namespace
and locality of allocation matters (monotonic ids for index scans, sequential
backups, etc.). Most types pick one mode and stick with it.

### 4.3 Get

```promise
Op.Get(id: some_entity_id)
```

Fetch one instance by id alone. The id is globally unique across all types, so
the server does not need a type to find the row — it looks up the id in its
global index, which yields both the row and its concrete `type_id` as a single
operation. If absent or deleted, `response.entities[id] = none`.

There is no client-side type hint on Get (see §2.3): under inheritance the
client only knows the declared bound from the field's `Ref[T]`, not the
concrete type, so any hint would either be useless or actively wrong on
sharded backends.

The server is allowed (and encouraged) to include other entities it had to load
anyway — particularly the targets of references on the requested entity, when
the server already paid the load cost.

### 4.4 List

```promise
Op.List(type_id: Foo._type_id, filter: none)
```

Returns all instance ids of the type. Filter is reserved for an extension point; the
initial implementation accepts only `none` (no filter) and returns the full list.
Listing payloads is two requests: List, then Get for each id (or rely on preemption).

`ListFilter` is intentionally minimal — anything richer belongs in a query module
that builds on top of `cloud`.

---

## 5. Client Mutations and Transactions

The wire protocol (§3) is already transactional: one `Request` is one server-side
transaction. The remaining design question is how the **client** groups its
mutations into that single request — without scattering explicit transaction
parameters through every call site, while still preventing accidental partial
commits.

The protocol does not change. This section is purely about the client API on top
of the protocol.

### 5.1 Why Batching Is Not Optional

A single request maps to a single server transaction (§3, §9.2). If a client wanted
to "save" individual mutations as soon as they happen, every entity field
assignment would round-trip to the server, and a sequence like `item.title = "a";
item.body = "b"` would either be two transactions (no atomicity) or block on each
write (two latency hits).

Both are wrong. Mutations have to accumulate locally and commit as one batch.
Therefore the client API needs a notion of "currently-open transaction" that
field assignments and entity creations attach to.

### 5.2 The Two Models

There are two clean ways to give the client API a transaction handle. Both are
viable; the trade-off is verbosity vs. implicitness.

**Model A — explicit transaction passed to every call.** Every mutating method
takes a `Transaction` parameter:

```promise
txn := client.begin();
item := txn.get[Item](some_id);
item.set_title(txn, "new title");        // every mutator threads txn
new_item := txn.create[Item](title: "x", folder: ...);
txn.commit();
```

Verbose but unambiguous. Field assignment (`item.title = "x"`) does not work —
field setters cannot take a transaction parameter, so users have to call explicit
methods (`set_title`).

**Model B — block-scoped transaction with thread-local binding.** A `transact`
block opens a transaction, registers it in thread-local storage for the block's
duration, and commits on normal exit:

```promise
client.transact!() |~txn| {
    item := txn.get[Item](some_id);
    item.title = "new title";            // resolves the active txn from TLS
    new_item := txn.create[Item](title: "x");
    new_item.folder = Ref.to(item.folder_target);
    // auto-commit on block exit; auto-discard on raise
};
```

Field assignment looks like ordinary struct mutation, because the entity carries a
hidden binding to the active transaction. Cross-thread sharing is forbidden by
construction (entities are not `` `sendable ``, so the type system blocks them
from being captured into `go` blocks or `task[T]`).

### 5.3 Recommendation: Model B

Model B aligns with how Promise users will *want* to write the code:

- Field assignment on a fetched entity reads as plain Promise.
- The transaction lifetime is the block — visible at the call site, scoped, and
  impossible to forget to close.
- Failure (a `raise` inside the block) discards the in-flight changes; success
  commits via a single sync request. The `!` on `transact!()` makes the commit
  itself failable.
- Forbidding cross-thread entity sharing is already idiomatic — `Task[T]` and
  `channel[T]` require `` `sendable ``, and entities are not.

The cost is the thread-local indirection and one rule that can't be checked at
compile time: mutating an entity outside any active transaction must error at
runtime.

### 5.4 The Field-Assignment Error Problem

Field assignment in Promise can't be failable — there is no syntax for an `!` on
a setter. So when a user writes `item.title = "x"` outside a `transact` block,
the runtime has two options:

1. **Panic immediately** at the offending line. Stops execution loudly. Easy to
   diagnose. Matches how Promise handles other "this code path requires a
   precondition that is statically true everywhere it should be reachable" cases.
2. **Defer the error to commit time**, accumulating mutations in a "pending"
   buffer that gets surfaced as an error only when the user calls `commit`. This
   trades immediate diagnosis for the ability to keep going past a misuse —
   which is mostly useful when the misuse was intentional ("I wanted to mutate
   without committing"), in which case the user should have used a non-entity
   value type to begin with.

**Lean: panic.** Mutating an entity without an active transaction is a logic
error, not a recoverable condition. The panic message includes the entity type
and the field being written, plus a hint pointing at `client.transact`.

### 5.5 New-Entity Creation

Inside a `transact` block, new entities must be bound to the active transaction
so their fields are mutable:

```promise
client.transact!() |~txn| {
    folder := txn.create[Folder](name: "Inbox");
    item := txn.create[Item](title: "Welcome", folder: Ref.to(folder));
};
```

`txn.create[T](...)` is the canonical constructor for entities inside a
transaction. It allocates an id (server-side via batch `Allocate` on commit, or
client-side if the type supports client-generated ids), binds the new entity to
the txn, and queues a `Put` for commit time.

Bare `T(...)` constructors on `` `entity ``-typed types are still legal but
yield an *unbound* entity — useful for tests and in-memory work, but writes to
its fields outside a `transact` block panic per §5.4.

### 5.6 Read-Only Access Outside Transactions

Reading entity data does not require a transaction. `client.get[T](id)` outside
any `transact` block returns an entity whose fields can be read freely; only
mutating writes are gated. This keeps the common "fetch and display" path
boilerplate-free:

```promise
main!() {
    use client := cloud.Client.connect("https://example.com/sync");
    item := client.get[Item](some_id);
    print_line(item.title);                  // fine — read-only
    // item.title = "x";                      // would panic — no active transaction
}
```

### 5.7 What `Transaction` Owns

Conceptually `Transaction` is a value carrying:

- The set of `Put` ops queued so far (entity id → mutated field map).
- The set of newly-created entity ids (for inclusion in the commit's `Allocate`).
- The set of deletion ops.
- A reference to the originating `Client` (for the commit network call).

It is `~`-borrowed by the `transact` block so cross-thread capture is blocked at
compile time. On normal block exit, `transact` calls `commit` (which builds one
`Request` and round-trips). On `raise`, the transaction is dropped without
commit — local mutations are lost.

A future extension might let users construct transactions explicitly (without
the block sugar) for advanced patterns (long-lived transactions, multiple
concurrent transactions on different threads). The block form is the v1
ergonomic; the explicit form can layer on later.

---

## 6. The Preemption Pattern

The preemption rule is one sentence: **the server may include any entity in
`response.entities`, and the client merges them all into its cache.** No separate
channel, no flag, no dedicated section.

Server policies that drive preemption:

- **Inverse-relation maintenance** (§7). A `Put` on entity A that updates a
  back-pointer on B includes B in the response.
- **Reference targets on Get.** Fetching A whose schema contains
  `Reference(B._type_id)` may include B in the response if B was loaded for any
  reason on the server side.
- **Hot-path prefetch.** A backend that knows certain types are always read together
  (entity + its photos, post + its comments) can include them eagerly. This is a
  policy knob on the backend, not part of the protocol.

The client side is symmetric: it does not distinguish "I asked for this" from "the
server gave me this anyway". Both go through the same cache write path. The prototype
takes the same approach (`store/server_client.go:264-298`) and it works well — the
extra entities are observably indistinguishable from a hot cache.

---

## 7. References Between Types

References are first-class but never appear as bare `EntityId` in user code. Two
value types in `modules/cloud` cover the two directions of a relationship:

- `Ref[T]` — a forward reference. A field of type `Ref[T]?` (or `Ref[T][]`) on
  entity `A` points at entities of type `T`. Wire form: id only.
- `Inverse[T]` — a server-maintained back-reference. A field of type `Inverse[T]`
  on `A` exposes the set of `T` entities that have a `Ref[A]` field pointing back.
  Wire form: not stored on `A`; either server-side index lookup or maintained
  back-pointer (backend choice).

Both are explicit value types. Field access is local (no network). Calls that
fetch payloads are loud (`!`-bearing methods on the value).

### 7.1 `Ref[T]` — Forward References

```promise
type Ref[T: `entity] `public `value `clone `serializable
    `doc("A typed reference to another entity. Stores the target's id; fetches lazily.") {

    get id Hash128 `instance `doc("The target entity's instance id. No fetch.");
    get is_loaded bool `instance `doc("True if get will return without a round-trip.");

    get!(this) T `instance `doc("Fetch the target entity. Cache-aware: returns immediately if already loaded.");
}
```

Properties:

- `Ref[T]` is a value type — copying is a memcpy, no indirection.
- The type parameter is constrained to `` `entity ``. A `Ref[Address]` (where
  `Address` is `` `serializable `` but not `` `entity ``) is a compile error.
- `ref.id` is a getter — synchronous, no network. Returns a bare 128-bit
  `Hash128`. The id alone does not encode the target's concrete type; the
  field's schema descriptor only gives a type *bound* (a `Ref[Animal]` may
  resolve to a `Dog` or `Cat`).
- `ref.get()` is failable — it may need to round-trip. The cloud client maintains
  a cache; loaded entities are returned without a fetch. `is_loaded` lets callers
  decide whether to skip a code path that would round-trip. The resolved entity
  carries its concrete type via the standard Promise vtable/RTTI, so
  `ref.get() as? Dog` and `ref.get() is Dog` work the way they do on any
  inherited type.
- Wire format: when an entity's `Ref[T]` field is encoded, the 128-bit id goes
  on the wire and nothing else. The schema descriptor for that field is
  `Type.Reference(T._type_id)` — used by the server to validate that the
  referenced entity is in fact some subtype of `T`. The bound is *not* the
  concrete type; the concrete type comes from the entity itself, either from
  the heap value already in hand or from the type the server records alongside
  the row.

```promise
type Item `entity {
    string title;
    Ref[Folder]? folder;
}

main!() {
    use client := cloud.Client.connect("https://example.com/sync");
    item := client.get[Item](some_id);

    print_line(item.title);                  // local — no fetch
    if ref := item.folder {
        print_line("folder id: {ref.id}");   // local — getter on Ref
        if !ref.is_loaded {
            print_line("would round-trip");
        }
        folder := ref.get();                 // round-trip (or cache hit) — auto-propagates
        print_line(folder.name);             // local on the fetched object
    }
}
```

### 7.2 `Inverse[T]` — Server-Maintained Back-References

A type declares that one of its reference fields has a server-maintained inverse
on the target. This is the prototype's `R201B` pattern: a `Ref[Folder]` on `Item`
has a back-reference `Inverse[Item]` on `Folder`, maintained automatically.

```promise
type Inverse[T: `entity] `public `serializable
    `doc("Lazy collection of entities that have a Ref pointing back at this entity. Server-maintained.") {

    count!(this) int `instance `doc("Number of entities in the relation. No payloads fetched.");

    get ids Hash128[] `instance `doc("Just the target ids — no payload fetch. Cached after first access.");

    contains(this, Hash128 id) bool `instance `doc("Membership check using the cached id list.");

    iter!(this) Iterator[T] `instance `doc("Paginated iteration. Server fetches in batches as the iterator advances.");

    list!(this) T[] `instance `doc("Eager materialization. One batched fetch.");
}
```

Pairing is declared with the `` `inverse `` meta, the same shape the prototype
used:

```promise
type Item `entity {
    string title;
    Ref[Folder]? folder;
}

type Folder `entity {
    string name;
    Inverse[Item] items `inverse(Item.folder);
}
```

Notes:

- `` `inverse(Item.folder) `` is a path to the inverse field. The compiler
  validates that the target field exists, has a compatible reference type, and
  that the cardinalities match (`Ref[Folder]?` ↔ `Inverse[Item]` is one-to-many;
  `Ref[Folder][]` ↔ `Inverse[Item]` is many-to-many).
- The server updates the inverse on every `Put` to the forward side. Touched
  entities flow into `response.entities` so the client doesn't see stale state
  on either side.
- `Inverse[T]` is **not** stored on the wire as part of the parent entity's
  encoded data. The schema descriptor marks it as a synthetic field — it never
  appears in `map[Hash128, Value]` payloads. Whether the server stores back-pointers
  or computes the relation from a forward-side index is a backend choice.
- One-to-many is the v1 cardinality. Many-to-many lands in v1 too — the prototype
  punted on it (`djabi.dev/iot/sync.go:288-291`); Promise should not ship that gap.

The four access methods (`count`, `ids`, `iter`, `list`) cover the access patterns
that motivated this split in the first place. Picking the right one matters:

| Method | When to use | Round-trip cost |
|--------|-------------|-----------------|
| `count()` | Just need the size (UI badges, capacity checks) | One small request, no payloads |
| `ids` (getter) | Want to dispatch on ids without payloads (filtering, deduplication) | Cached after first access; fetched once |
| `iter()` | Streaming through a large set, processing as you go | Paginated (server-driven page size) |
| `list()` | Need the whole set and will use most of it | One batched fetch |

### 7.3 Cascading Deletes

An inverse relation does **not** imply cascade. Deleting a `Folder` does not delete
its `Item`s; it just removes the back-pointers. Cascade is a per-relation policy,
expressed as `` `inverse(Item.folder, on_delete: cascade) ``. Default is `unlink`
(the prototype's behavior).

---

## 8. Blobs

Binary payloads (images, large strings, opaque bytes) are addressed by string keys,
not by `EntityId`. A blob lifecycle is independent of the entity that references it
— the entity stores the blob's key as a `string` field, the server stores the blob in
whatever backing store the deployment provides.

```promise
enum BlobOp `public `serializable {
    Get(string[] keys),
    Set(map[string, u8[]] writes),
    Exists(string[] keys),
}

type BlobResponse `public `serializable {
    map[string, u8[]] payloads;
    map[string, bool] exists;
}
```

This piggybacks on the same `Request`/`Response` cycle. A blob fetch and an entity
fetch can share one round-trip.

---

## 9. Server Architecture

The server is a Promise program that:

1. Loads the schema for every type it serves. The schemas are `schema.Type` values
   produced by `schema.of[T]()` at the server's compile time, registered in a
   `map[Hash128, schema.Type]`.
2. Implements a single `handle_sync(Request) Response!` function that dispatches by
   op kind.
3. Delegates persistence to a `Backend` interface:

```promise
type Backend `structural `public {
    allocate!(~this, Hash128 type_id, int count) EntityId[] `abstract;
    put!(~this, EntityId id, map[Hash128, Value]? data) `abstract;
    get!(~this, EntityId id) (map[Hash128, Value])? `abstract;
    list!(~this, Hash128 type_id) EntityId[] `abstract;
    blob_get!(~this, string key) u8[]? `abstract;
    blob_put!(~this, string key, u8[] data) `abstract;
    blob_exists!(~this, string key) bool `abstract;
}
```

First-party implementations: `MemoryBackend` (testing, ephemeral), `SqliteBackend`
(single-node persistent), `FirestoreBackend` (managed, multi-region) — the last as a
community module since it pulls in Google Cloud client deps the catalog should not
depend on.

### 9.1 Validation

For every `Put`, the server walks the incoming `data` map against
`schema.Type.Object.fields`:

- Every key must be a known field id.
- Every value's encoded type must match the field's `Type`.
- Required fields must be present.
- T?-typed fields may carry `Value.None`.

Validation runs against the schema the server compiled with. A client that has an
older schema (missing a field, or with a renamed-but-unpinned field) gets a clear
error referencing the offending `Hash128`, not a stack trace.

### 9.2 Concurrency Model

One request = one server-side transaction. Within a request, ops apply atomically in
the order specified in §3.2. Across requests, last-writer-wins on the storage
backend; if stronger semantics are needed, the backend implementation provides them
(Firestore transactions, Sqlite `BEGIN IMMEDIATE`, etc.).

The protocol does not currently offer optimistic concurrency tokens (entity version
ids returned on Get, required on Put). Adding them is a forward-compatible extension:
new optional fields on `Op.Put` and `Response`.

---

## 10. Schema Evolution

Every type and field carries a `Hash128` id. The evolution model is therefore: as
long as a construct's id stays stable, the wire format is stable. Source-side
operations and their wire effects:

| Source change                              | Default wire effect             | Mitigation |
|--------------------------------------------|--------------------------------|------------|
| Add a new field with a default or `T?`     | Old data decodes fine          | None needed |
| Add a new required field                   | Old data fails to decode       | Use a default or `T?`, then make required later |
| Rename a field                             | New hash; old data orphaned    | Add `` `id("<old-hex>") `` on the renamed field |
| Rename a type                              | New hash; all instances orphaned| Add `` `id("<old-hex>") `` on the type |
| Delete a field                             | Stored data under that id is unread | Document in deprecation notes; server sweep optional |
| Change a field's type                      | Decode error at first read     | Don't. Add a new field, migrate, delete the old |
| Change a generic type argument             | New type id (different instantiation) | Treat as a new type |

The compiler can emit a warning when a `Hash128` change is detected in an incremental
build (the previous hash is in the build cache), but the warning is opt-in — many
local builds intentionally drift the hash. Production deployments should turn it on
via a build flag.

---

## 11. Lessons Carried From the Prototype

The Promis system at `~/prog/djabi_data/` is the closest existing analog. Promise
inherits:

- **Single endpoint, batched ops.** Confirmed working at scale; `core/connection.go`
  is the load-bearing file.
- **Server-included extras as the preempt mechanism.** No separate channel; `entities`
  map carries everything (`store/server_client.go:createPreload`).
- **Server-maintained inverse relations.** Strict win for client simplicity
  (`djabi.dev/iot/sync.go:245-382`).

Promise diverges:

- **Identity is content-addressed (`Hash128`), not author-assigned (`T200`/`A100`).**
  The prototype required the schema author to manually never reuse integer ids; it
  worked but was a manual discipline. Hash composition + `` `id `` overrides give the
  same evolution properties without the bookkeeping.
- **Generic instantiations get distinct ids automatically.** The prototype had no
  generics so this didn't apply; Promise does, and `Vector[int]` vs `Vector[string]`
  must have separate ids.
- **No `T<n>I<m>` string keys, and no bundled type id either.** Identity is a
  single 128-bit value. The prototype tied type and instance together so the
  server could route a request without an index; Promise drops the tie and
  requires backends to maintain a global id index (§2.3). The result is one
  fewer field on the wire and one fewer way for client/server type beliefs to
  drift.
- **Many-to-many inverse relations land in v1**, not as a TODO.
- **`Backend` is structural and pluggable from day one.** The prototype hard-coded
  Firestore + memory; Promise treats backends as community modules.

---

## 12. Module Name

This doc uses `cloud` as the catalog module name. It's accurate and short, but
generic enough that future Promise features (cloud functions, cloud blobs as a
separate module, etc.) might want it. Alternatives considered:

- **`store`** — short, matches the prototype's terminology, but collides with
  potential `std/store` semantics.
- **`persist`** — verb-form, awkward in import position (`use persist;`).
- **`data`** — too broad; data manipulation is everywhere.
- **`sync`** — too narrow; covers the protocol but not the storage and identity
  story.

I'd settle on **`cloud`** if no other features stake the same name, otherwise
**`cloud_data`**. The decision is not load-bearing; everything in this doc works
under any name.

---

## 13. Implementation Phases

### Phase 0 — Prerequisites

1. **Native large integer types** (`u128` at minimum, full ladder per
   [docs/large-integers.md](large-integers.md)). `Hash128` — and therefore every
   entity id and every reference — is a 128-bit value. With `u128` available,
   `Hash128` collapses to a `u128` alias and identity becomes a single native
   load/store/compare instead of a two-word struct. Cloud persistence v1 does
   not start until at least Phase 1 of the large-integers work (i128/u128) is
   done.
2. `modules/schema` (see `docs/schema.md`) — `Hash128`, `Origin`, `Type`, `id` meta,
   project-id resolution, and the `` `entity `` meta with hidden-id-field synthesis.
3. `modules/json` — already done.
4. `modules/http` — currently a placeholder; required for the default transport.
5. `[executable]` table support in `promise.toml` parsing.

### Phase 1 — Wire Types and Client

5. `Request` / `Response` / `Op` / `Value` / `EntityId` `` `serializable `` types.
6. `Ref[T]` and `Inverse[T]` value types with their methods (`get!`, `id`, `count!`,
   `ids`, `iter!`, `list!`). Compile-time validation of `T: ` `` `entity `` ``.
7. The `` `inverse(...) `` field meta with cardinality validation against the paired
   `Ref[T]`-typed field.
8. Client API: `cloud.Client` with `request!()`, helper methods (`get[T]`,
   `put[T]`, `list[T]`, `allocate[T]`).
9. JSON over HTTP transport.

### Phase 2 — In-Memory Backend and Server

10. `Backend` structural interface; `MemoryBackend` implementation.
11. `cloud.Server` — schema registration, validation, dispatch, inverse-relation
    maintenance.
12. `promise cloud serve` CLI for spinning up a local in-memory server.

### Phase 3 — Persistent Backend

13. `SqliteBackend` — single-node persistent. The default for non-distributed
    deployments.
14. `cloud.Server` extensions: optimistic concurrency tokens, server-side filters
    (when a use case justifies a query layer).

### Phase 4 — Community Backends

15. `cloud_firestore` (community module) — Firestore-backed, GCP-managed.
16. `cloud_postgres` (community module) — Postgres-backed.

---

## 14. Open Design Questions

**Q1 (settled): `EntityId` is a single 128-bit value.**
Identity is a bare `Hash128` (which becomes a `u128` alias once the large-integer
work in [docs/large-integers.md](large-integers.md) lands). The type id is *not*
bundled. Operations that need to know the type on creation (`Put` of a new id,
`Allocate`, `List`) carry it as an explicit op parameter; `Get` takes the id
alone — there is no client-side type hint, because under inheritance the
client only ever knows the declared `Ref[T]` bound, not the concrete type
(§2.3).

The load-bearing observation (§2.1) is that the concrete type is **never** what
identity is for — it's a property of the entity itself, encoded in the heap
value's vtable / RTTI and recorded by the server next to the row. To act on an
entity you either have it in hand (concrete type is right there) or you're
asking the server to find it (concrete type comes back with the row). A typed
`Ref[T]` gives you a *bound* on what types are valid at that field, not the
concrete leaf type — under inheritance, a `Ref[Animal]` may legitimately point
at a `Dog`, so bundling a type onto the id wouldn't have pinned anything
useful.

Earlier drafts bundled type and instance into a struct
(`{ Hash128 type_id; Hash128 instance_id }`) for the same reason the prototype
did: single-pass server-side routing without a cross-type index. That was
rejected because (a) the bound on `Ref[T]` is not the concrete type, so the
bundled id wasn't pinning what users would assume it was; (b) a duplicated
wire copy that can drift from the server's record is a bug source; (c) the
client doesn't know the concrete type anyway (only the bound), so it couldn't
even supply a correct type alongside the id on Get; (d) backends need a global
id index anyway to support cross-type references, so the bundled type never
did meaningful routing work the index didn't already do. Full rationale in §2.

Phantom-typed variants (`EntityId[T]`) were also considered and dropped —
they would force every reference field to be generic and complicate the
schema descriptor for no functional gain. Server backends may not even be
implemented in Promise, and a flat 128-bit id is trivial to model in any
other language.

**Q2 (settled): protocol versioning piggybacks on client identity.**
Earlier drafts asked whether `Request` / `Response` should carry an explicit
`protocol_version` field. They should not. Instead, every sync request carries
**client-context fields** that let the server resolve which version of which
binary it is talking to:

- **Project id** (the `[executable] id` from `docs/schema.md` §6.1) — identifies
  which executable.
- **Module epoch** — identifies the catalog version the build was made against.
- **Build commit** (optional) — exact source revision, when available.

The server uses these together to pick the right schema universe and (in model 2
of Q3 below) to scope schema uploads to a client identity. The hash of the
`Request` type itself is also implicitly a protocol version — a client/server
mismatch on `Request`'s hash surfaces as a decode error with clear field
references, preferable to a silently-misinterpreted version int.

**Q3 (open): where does the server's schema come from — compiled in, or sent by the client?**
Two viable models:
1. **Compiled in (prototype model).** The server is built with knowledge of every
   type it will store. Clients only send ids and values; field names never appear
   on the wire. Strict client/server type-version coupling — both rebuild
   together.
2. **Sent on handshake.** The client transmits its `schema.Type` graph to the
   server on first contact (or on first reference to an unknown type id). The
   server validates against the received schema and persists it for the life of
   that client identity.

The ideal long-term shape is a server that can host arbitrary clients without
recompilation, which points at model 2 — but with two refinements:

- **Schema upload is amortized, not per-request.** A naïve "send full schema with
  every request" wastes bandwidth on every round-trip. Realistic implementations
  upload once per client+version pair (keyed by Q2's project id + module epoch +
  commit) and cache server-side, with a fast pre-check (the request carries the
  schema's `Hash128`; the server says "yes, I have that schema" or "send it
  again").
- **Account-level identification.** A multi-tenant server needs to know *who* is
  uploading, not only *what schema they use*. This is access-control concern —
  the request carries an account / API key (resolved against the server's
  identity provider) so the server can authorize writes and isolate tenants.
  This is an `auth`-style concern (`docs/ai-platform.md` §4 covers the
  primitives), threaded through the request envelope.

The whole client-side handshake / upload / cache / auth shape is an
**implementation TBD** for model 2. v1 starts with model 1 because it's the
smaller scope; model 2 can layer on top without changing the wire format
(`Hash128` keys are already content-addressed regardless of where the schema
came from).
