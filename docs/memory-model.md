# Memory Model

> **Tag:** `memory-model` — remaining work to complete this document: `mcp__tracker__list --tag memory-model`

What may allocate, who owns what is allocated, and how every other type is built from a small closed
set of primitives that can.

This document owns *allocation*. Two neighbours own the rest, and neither restates the other:
**layout** — how a type's fields are arranged across the four structs — is §5.2 of
[language-design.md](language-design.md); **ownership** — who may hold, borrow or move a value, and
when it is dropped — is §6 of the same document.

## 1. Fixed and variable allocation

Every allocation in a Promise program is one of two kinds, and the distinction is not *where* the
memory lives but *whether its size is known when the code is compiled*.

- **Fixed-size.** The size follows from the type. A value struct, an instance struct, an array
  `[N]T`, a tuple, an optional — the compiler computes a size and emits it. A heap allocation can
  be fixed-size: `Ref[T]` heap-allocates, but its allocation is `sizeof T` plus its counters, a
  compile-time constant. Allocating is not the interesting capability.
- **Variable-size.** The size cannot be computed at compile time, because it is a property of the
  *value* rather than of the type. Two values of the same type may differ in size, and a single
  value's size may change over its lifetime as it grows. Only the running program knows it — an
  element count, a capacity, a byte length.

**Variable-size allocation is the capability that must be primitive.** A type written in Promise
has fields, and every field has a type with a size; there is no way to spell "a run of `n` elements"
for a runtime `n` except by holding something that already provides it.

## 2. The variable-size primitives are a closed set

Exactly three types own an allocation whose size is not known at compile time:

| Type | What varies | Buffer held |
|---|---|---|
| `Vector[T]` | element count and capacity | **by value** |
| `Channel[T]` | buffer capacity, fixed at creation from a runtime argument | behind a handle |
| `string` | byte length | behind a handle |

All three are `` `native ``, necessarily: the compiler emits their allocation, growth and release,
because Promise source cannot express any of it. **No fourth type may join this set without a
compiler change** — and a change here is a change to this document first.

Everything else that appears to allocate variably does so by holding one of these three.

## 3. By value versus behind a handle

The column that matters most in §2 is the last one. It is not about allocation at all — it is about
what happens when the *owner* is duplicated, and it is the distinction the rest of the language
keys on.

- **By value** — `Vector[T]`. Duplicating a vector duplicates its buffer, and therefore duplicates
  every element in it. This is why a vector needs machinery that other types do not: growth
  (realloc), insertion (push), and literal lowering all move or copy element *values*, and each of
  those is a place where an element could be duplicated.
- **Behind a handle** — `Channel[T]`, `string`, and the non-allocating handles `Ref[T]`, `Weak[T]`,
  `Task[T]`, `Mutex[T]`, `MutexGuard[T]`. Duplicating the handle does not touch the payload: it
  bumps a reference count, or is refused outright. One payload, many handles.

A container's capabilities follow from this. A by-value container's capabilities are those of its
elements — a `Vector[T]` is cloneable when `T` is, sendable when `T` is, and holds a single-owner
handle only where duplication is provably absent. A behind-a-handle container's capabilities are
its own, because its payload is never copied: `Ref[Task[T]]` is cloneable although `Task[T]` is
not, since cloning the `Ref` produces a second handle to one task rather than a second task.

## 4. Everything else is composition

```
Map[K, V]   =  Vector (its bucket array)  +  hashing and probing
Set[T]      =  Map[T, bool]
```

A user's container is composed the same way, from the same three primitives plus `Ref[T]` for
shared ownership. There is no privileged middle tier between the primitives and ordinary Promise
code: `Map` and `Set` are ordinary Promise types that happen to ship in the standard library, and
the standard library is a module like any other.

Two consequences, and they are the reason this section exists:

- **No type outside §2 requires the compiler to know its name.** A composed container's behaviour
  is derivable from its fields and type arguments, because its allocation behaviour is entirely
  inherited from the primitives it holds. A compiler decision that tests for `Map` or `Set` by
  identity is therefore not merely inelegant — it is asserting a property that the type's own
  fields already determine, which is what [annotations.md](annotations.md) §1 forbids.
- **A composed container that misbehaves indicates a general defect, not a missing special case.**
  If `Map` needs an exception to be handled correctly, then every user container of the same shape
  is being handled incorrectly, silently. Special-casing the standard library's containers does not
  fix such a bug; it conceals it in the one place it would have been noticed.

## 5. Ownership of an allocation

Every allocation has exactly one owner at a time, and the owner is responsible for releasing it.
That is §6's subject; what belongs here is where the obligation sits for each of the three
primitives.

- A by-value buffer is released when its owner is dropped. A `Vector[T]` dropped at scope exit
  releases its buffer, and drops each element first if the element type requires it.
- A behind-a-handle payload is released when the *last* handle is dropped, which is what the
  reference count is counting. A `Channel[T]` with two live handles releases nothing when the
  first is dropped.
- A `string` is the one primitive that may not own its bytes at all: a literal's bytes live in
  `.rodata` for the life of the program, and dropping such a string releases nothing. Whether a
  given string owns heap bytes or refers to static ones is carried in the value itself, so the
  question is answered at run time rather than by the type.
