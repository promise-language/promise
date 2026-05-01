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

## Feature 2: Method-Level Generics

**Goal**: `map[R]((T) -> R transform) Stream[R]` — methods with their own type parameters.

### Current State

- Grammar already supports `typeParams?` on `methodDecl`
- AST has `MethodDecl.TypeParams`, `Signature` has `typeParams`
- But sema never processes method type params, and codegen has no method-level monomorphization

### Implementation Steps

| Step | Phase | Change |
|------|-------|--------|
| B1 | sema/decl | `resolveMethodSignature`: process `md.TypeParams`, create TypeParam objects, set on signature |
| B2 | sema/info | Add `MethodInstance{Owner, Method, TypeArgs, Sig}` tracking |
| B3 | sema/expr | Generic method instantiation: `stream.map[string]` → member access returns generic Signature, index expr instantiates, record `MethodInstance` |
| B4 | sema/check | Put method type params in scope during body checking |
| B5 | codegen/mono | Method-level monomorphization: names like `Stream__int.map__string`, combined substitution |
| B6 | codegen/expr | `genMethodCall`: detect generic method instance, dispatch to mono name |
| B7 | tests | Sema, codegen, e2e tests |

**Restriction**: Generic methods cannot be virtual/abstract (same as C++ virtual templates). Direct dispatch only.

---

## Implementation Order

Phase A first (generic inheritance), then Phase B (method generics), then Phase C (integration: `Stream[T].map[R]` on `Range[int]`).

### Core Challenge

**Substitution composition**: `Range[int].map[string](fn)` requires three levels:
1. Range's `T=int`
2. Stream's `T=Range's T=int`
3. Method's `R=string`
