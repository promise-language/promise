# Generic Inheritance & Method-Level Generics

Implementation plan for two features needed for `Stream[T]` and `Iterator[T]`.

## Feature 1: Generic Inheritance

**Goal**: `type FilterIter[T] is Iter[T]` — generic type inheriting from another generic type.

### Current State

- Inheritance works for non-generic types only
- `Named.parents` is `[]*Named`; all parent-walking code uses `*Named` identity
- When `is Stream[T]` resolves, sema returns `*Instance`, which gets rejected

### Design: ParentRef

Add `ParentRef` struct to types package:

```go
type ParentRef struct {
    Named    *Named
    TypeArgs []Type  // nil for non-generic parents
}
```

Change `Named.parents` from `[]*Named` to `[]ParentRef`. Every consumer of parents updated to use the new type.

### Implementation Steps

| Step | Phase | Change |
|------|-------|--------|
| A1 | types | Add `ParentRef`, update `Named` + all parent-walking methods (`AllFields`, `LookupMethod`, `LookupGetter`, `LookupSetter`, `AllVirtualMethods`, `InheritsFrom`, `isChild`, `AssignableTo`) |
| A2 | sema/decl | `defineType`: accept `*Instance` from parent resolution, extract origin + type args into `ParentRef` |
| A3 | sema/expr | `resolveInstanceMember`: compose substitution maps when traversing generic parents |
| A4 | codegen/mono | `collectMonoInstances`: transitively discover parent instances (`Range[int]` → also needs `Stream[int]`) |
| A5 | codegen/rtti | `collectAllParentIDs` / vtable emission for mono parent type IDs |
| A6 | tests | Sema, codegen, e2e tests |

### Edge Cases

- **Reordered params**: `type Foo[A, B] is Bar[B, A]`
- **Partial application**: `type Foo[T] is Map[string, T]`
- **Non-generic child of generic parent**: `type IntStream is Stream[int]`
- **Diamond with generics**: `type D[T] is B[T], C[T]` where both inherit from `A[T]`

---

## Feature 2: Method-Level Generics (Done)

**Goal**: `map[R]((T) -> R transform) Stream[R]` — methods with their own type parameters.

### Implementation Steps (all completed)

| Step | Phase | Change | Status |
|------|-------|--------|--------|
| B1 | sema/decl | `resolveMethodSignature`: process `md.TypeParams`, create TypeParam objects, set on signature | Done |
| B2 | sema/info | Add `MethodInstance{Owner, Method, TypeArgs, Sig}` tracking | Done |
| B3 | sema/expr | Generic method instantiation: `stream.map[string]` → member access returns generic Signature, index expr instantiates, record `MethodInstance` | Done |
| B4 | sema/check | Put method type params in scope during body checking | Done |
| B5 | codegen/mono | Method-level monomorphization: names like `Box__int.convert__string`, combined substitution | Done |
| B6 | codegen/expr | `genGenericMethodCall`: detect IndexExpr+MemberExpr, dispatch to mono name | Done |
| B7 | codegen/compiler | Wire up in compilation pipeline (main + module paths) | Done |
| B8 | codegen | Skip generic methods in non-mono passes (declareTypeMethods, defineTypeMethods, mono methods) | Done |
| B9 | tests | 7 sema tests, 2 codegen IR tests, 8 e2e tests | Done |

**Key fix**: `substSignature` in `types/subst.go` updated to preserve method-level TypeParams when only type-level params are being substituted. Without this, `resolveInstanceMember` would strip method TypeParams.

**Restriction**: Generic methods cannot be virtual/abstract (same as C++ virtual templates). Direct dispatch only.

---

## Implementation Order

Phase A first (generic inheritance — done), then Phase B (method generics — done), then Phase C (integration: `Stream[T].map[R]` on `Range[int]`).

### Core Challenge

**Substitution composition**: `Range[int].map[string](fn)` requires three levels:
1. Range's `T=int`
2. Stream's `T=Range's T=int`
3. Method's `R=string`

---

## Known Bugs (Pre-existing)

| Bug | Scope | Status |
|-----|-------|--------|
| Failable `? e { ... }` handler phi node type mismatch on method calls — the phi node in the merge block has a type mismatch between the success path (value type) and the handler path (recovery value). Discovered during method generics work but affects all methods, not just generic ones. | codegen | Open |
| ~~Mono type vtable/RTTI~~ — generic instances lacked vtable and typeinfo globals, causing virtual dispatch crashes when passed as parent types. Fixed by adding `computeMonoVtableInfo`, `emitMonoVtableGlobals`, `emitMonoTypeInfoGlobals` and unified lookup helpers. | codegen/rtti | **Fixed** |
