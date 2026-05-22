# cloud — Schema-Driven Durable State

**Status: proposed.** Module name is provisional — see `docs/cloud-persistence.md`
§12. Tracker tasks: T0456 (wire + client), T0457 (memory backend + server),
T0458 (sqlite), T0459 (community backends). Blocked on T0446 (schema), T0447
(http).

`modules/cloud` provides durable, schema-driven, multi-process shared state for
Promise programs. A client defines `` `entity ``-annotated types and performs
typed `get` / `put` / `list` / `allocate` operations through a single endpoint.
The server validates against the type's `schema.Type`, mediates references, and
is allowed to send back more data than the client asked for to preempt future
round-trips.

## Quick start

```promise
use cloud;

type Item `entity {
    string title;
    Ref[Folder]? folder;
}

type Folder `entity {
    string name;
    Inverse[Item] items `inverse(Item.folder);
}

main!() {
    use client := cloud.Client.connect("https://example.com/sync");

    client.transact!() |~txn| {
        folder := txn.create[Folder](name: "Inbox");
        item := txn.create[Item](title: "Welcome", folder: Ref.to(folder));
        // auto-commit on block exit; auto-discard on raise
    };
}
```

## API surface (summary)

- Wire types (all `` `serializable ``): `Request`, `Response`, `Op` enum
  (`Allocate` / `Put` / `Get` / `List` / `Blob`), `Value`, `EntityId`, `Stats`.
- Reference types: `Ref[T: ` `entity `]` (forward), `Inverse[T: ` `entity `]`
  (server-maintained back-ref) with `count!` / `ids` / `iter!` / `list!`
  access patterns.
- `` `inverse(...) `` field meta with cardinality validation.
- `cloud.Client` — `connect!`, `request!`, `get[T]`, `put[T]`, `list[T]`,
  `allocate[T]`.
- Block-scoped transactions: `client.transact!() |~txn| { ... }` with
  thread-local binding (`docs/cloud-persistence.md` §5).
- `Backend` structural interface for pluggable storage; first-party
  `MemoryBackend` and `SqliteBackend`.
- `cloud.Server` with schema registration and per-request transactional
  validation.
- Community backends (out-of-tree): `cloud_firestore`, `cloud_postgres`.

## Compiler dependencies

- `` `entity `` meta annotation (T0446) — synthesizes the hidden id field and
  switches encoding to id-only when an entity-typed field appears in another
  type.
- `` `inverse(...) `` meta annotation (T0456) — paired with `Ref[T]` for
  server-maintained back-references with cardinality validation.

## Full design

See [`docs/cloud-persistence.md`](../../docs/cloud-persistence.md) for the full
module specification, sync protocol, the transaction model, schema evolution
rules, and the open question on schema-source (compiled-in vs handshake upload).
