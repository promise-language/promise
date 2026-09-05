package sema

import (
	"fmt"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// isSingleOwnerInstance reports whether t is a generic instance whose origin
// is marked `single_owner (move-only handle, no clone()). T1413: replaces
// the hardcoded IsAnyTask || IsMutex || IsMutexGuard identity checks with
// the annotation-driven flag on the origin Named type.
func isSingleOwnerInstance(t *types.Instance) bool {
	switch origin := t.Origin().(type) {
	case *types.Named:
		return origin.IsSingleOwner()
	case *types.Enum:
		return origin.IsSingleOwner()
	}
	return false
}

// isSingleOwnerType reports whether typ is a `single_owner type — either a
// direct Named/Enum with the flag, or an Instance whose origin has it.
func isSingleOwnerType(typ types.Type) bool {
	switch t := typ.(type) {
	case *types.Instance:
		return isSingleOwnerInstance(t)
	case *types.Named:
		return t.IsSingleOwner()
	case *types.Enum:
		return t.IsSingleOwner()
	}
	return false
}

// cloneInstanceOpDesc is the OpDesc tag for a deferred `clone-generic
// instantiation requirement recorded by validateCloneInstance when a field /
// variant field's substituted type still contains a TypeParam inside a generic
// body (T1201). Used for dedup and the propagateCloneReqs re-validation edge.
const cloneInstanceOpDesc = "`clone type instantiation"

// isCloneableField returns true if a field type can be cloned:
// either it's a copy type (bitwise copy) or it has a clone() method.
func isCloneableField(typ types.Type) bool {
	if typ == nil {
		return false
	}
	// Copy types are always cloneable (bitwise copy)
	if isCopyField(typ) {
		return true
	}
	switch t := typ.(type) {
	case *types.Named:
		return t.LookupMethod("clone") != nil
	case *types.Enum:
		return t.LookupMethod("clone") != nil
	case *types.Instance:
		// Check the origin type for clone method
		switch origin := t.Origin().(type) {
		case *types.Named:
			return origin.LookupMethod("clone") != nil
		case *types.Enum:
			return origin.LookupMethod("clone") != nil
		}
		return false
	case *types.Optional:
		return isCloneableField(t.Elem())
	case *types.TypeParam:
		// Generic type params — validated at instantiation
		return true
	case *types.Signature:
		// Function types cannot be cloned (closure environments)
		return false
	case *types.SharedRef, *types.MutRef:
		// References cannot be cloned
		return false
	}
	return false
}

// validateCloneType checks that all fields of a `clone type are cloneable.
// Called as a deferred pass after all types are defined (so clone() methods are registered).
func (c *Checker) validateCloneType(named *types.Named, d *ast.TypeDecl) {
	for _, f := range named.AllFields() {
		if !isCloneableField(f.Type()) {
			c.errorf(d.Pos(), "type %s is marked `clone but field '%s' has type %s which is not cloneable (must be `copy or have a clone() method)",
				d.Name, f.Name(), f.Type())
		}
	}
}

// validateCloneEnum checks that all variant fields of a `clone enum are cloneable.
func (c *Checker) validateCloneEnum(enum *types.Enum, d *ast.EnumDecl) {
	for _, v := range enum.Variants() {
		for _, f := range v.Fields() {
			if !isCloneableField(f.Type()) {
				c.errorf(d.Pos(), "enum %s is marked `clone but variant %s has field type %s which is not cloneable",
					d.Name, v.Name(), f.Type())
			}
		}
	}
}

// validateCloneTypes runs after all types are defined to validate clone field types.
// This is deferred because field types may have clone() methods defined later in the file.
func (c *Checker) validateCloneTypes(file *ast.File) {
	for _, decl := range file.Decls {
		if c.info.FilteredDecls[decl] {
			continue
		}
		switch d := decl.(type) {
		case *ast.TypeDecl:
			obj := c.declScope.Lookup(d.Name)
			if obj == nil {
				continue
			}
			tn, ok := obj.(*types.TypeName)
			if !ok {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok {
				continue
			}
			if named.IsClone() {
				c.validateCloneType(named, d)
			}
		case *ast.EnumDecl:
			obj := c.declScope.Lookup(d.Name)
			if obj == nil {
				continue
			}
			tn, ok := obj.(*types.TypeName)
			if !ok {
				continue
			}
			enum, ok := tn.Type().(*types.Enum)
			if !ok {
				continue
			}
			if enum.IsClone() {
				c.validateCloneEnum(enum, d)
			}
		}
	}
}

// isAutoCloneTypeArg reports whether a concrete type substituted into a
// TypeParam-containing field of a `clone generic can be deep-cloned by codegen's
// AutoClone path (genAutoCloneExpr → cloneByType). It differs from
// isCloneableField in one way: isCloneableField models the CONCRETE-field synth
// (synthesizeCloneMethod: a field is cloned via a bitwise copy or a direct
// clone() method), whereas the AutoClone path — the ONLY path a TypeParam field
// takes — ALSO deep-clones structurally through Optional/Tuple/Array
// (T0605/T0607/T0662/T0667): a `(T,int)` / `[N]T` / `T?` field is cloneable when
// its element(s) are, even though a tuple/array has no clone() method of its own.
// Leaf cloneability (copy, a clone() method, a cloneable native container)
// delegates to isCloneableField. Reusing isCloneableField directly would falsely
// reject the T0667 tuple / T0662 array TypeArg shapes. (T0666)
func isAutoCloneTypeArg(typ types.Type) bool {
	switch t := typ.(type) {
	case *types.Optional:
		return isAutoCloneTypeArg(t.Elem())
	case *types.Tuple:
		for _, e := range t.Elems() {
			if !isAutoCloneTypeArg(e) {
				return false
			}
		}
		return true
	case *types.Array:
		return isAutoCloneTypeArg(t.Elem())
	}
	return isCloneableField(typ)
}

// validateCloneInstance enforces at each generic instantiation site that a
// `clone type/enum instantiated with concrete type args keeps all fields
// cloneable. isCloneableField optimistically returns true for a bare
// *types.TypeParam ("validated at instantiation", above) — this realizes that
// deferral: once T is bound, every field / variant-field whose declared type
// contains a TypeParam is re-checked with the substitution applied. A
// non-cloneable concrete arg (a non-`clone/non-`copy type/enum with heap data
// and no clone() method, e.g. Box[Heapy]) is rejected here, mirroring the
// concrete-field diagnostics of validateCloneType/validateCloneEnum. Without
// this, codegen's AutoClone path (genAutoCloneExpr → cloneByType →
// isAutoCloneBitCopy) bit-copies the field and double-frees the shared heap
// payload at drop. (T0666)
func (c *Checker) validateCloneInstance(pos ast.Pos, origin types.Type, typeArgs []types.Type) {
	switch t := origin.(type) {
	case *types.Named:
		if !t.IsClone() || len(t.TypeParams()) == 0 {
			return
		}
		subst := types.BuildSubstMap(t.TypeParams(), typeArgs)
		// AllFields() includes fields inherited from a generic parent whose
		// declared type references the PARENT's type params (a distinct
		// *TypeParam object). Merge the parent-arg substitution so an inherited
		// bare-TypeParam field (`Sub[T] is Base[T] { T val; }`, Sub[Heapy]) is
		// re-checked too — without this it stays "still generic" and slips the
		// gate, leaking the shared heap payload at drop (mirrors codegen's
		// mergeParentSubst). (T0666)
		c.mergeParentSubstSema(t, subst)
		deferred := false
		for _, f := range t.AllFields() {
			if !types.ContainsTypeParam(f.Type()) {
				continue // concrete field already checked by validateCloneType
			}
			concrete := types.Substitute(f.Type(), subst)
			if types.ContainsTypeParam(concrete) {
				deferred = true // still generic (nested generic body) — deferred
				continue
			}
			if !isAutoCloneTypeArg(concrete) {
				c.errorf(pos, "cannot instantiate `clone type %s: type argument makes field '%s' have type %s which is not cloneable (must be `copy or have a clone() method)",
					t.Obj().Name(), f.Name(), concrete)
			}
		}
		if deferred {
			// T1201: a `clone generic instantiated over a still-unbound TypeParam
			// field inside another generic body — defer the T0666 cloneability
			// check to the concrete call edge (mirrors
			// validateSingleOwnerContainerInstance/T0616). recordCloneReq is a
			// no-op outside a generic body.
			c.recordCloneReq(types.NewInstance(t, typeArgs), pos, cloneInstanceOpDesc)
		}
	case *types.Enum:
		if !t.IsClone() || len(t.TypeParams()) == 0 {
			return
		}
		subst := types.BuildSubstMap(t.TypeParams(), typeArgs)
		deferred := false
		for _, v := range t.Variants() {
			for _, f := range v.Fields() {
				if !types.ContainsTypeParam(f.Type()) {
					continue
				}
				concrete := types.Substitute(f.Type(), subst)
				if types.ContainsTypeParam(concrete) {
					deferred = true
					continue
				}
				if !isAutoCloneTypeArg(concrete) {
					c.errorf(pos, "cannot instantiate `clone enum %s: type argument makes variant %s field type %s which is not cloneable (must be `copy or have a clone() method)",
						t.Obj().Name(), v.Name(), concrete)
				}
			}
		}
		if deferred {
			// T1201: same deferral as the Named branch for a generic enum body.
			c.recordCloneReq(types.NewInstance(t, typeArgs), pos, cloneInstanceOpDesc)
		}
	}
}

// firstSingleOwnerHandle returns the first single-owner native handle
// (Task[T], Mutex[T], MutexGuard[T]) found in typ, searching transitively
// through Instance type arguments, Optional, Tuple, and Array element types.
// Returns nil if typ contains no single-owner handle. (T0545)
//
// These handles are LLVM `i8*` native handles with no clone() method, not
// `copy, and move-only on assignment — `someTask.clone()` is already a sema
// error. A type that transitively contains one is therefore non-cloneable.
// Recursion deliberately does NOT descend into *types.Named fields: a user
// type with a handle field is already covered by validateCloneType (`clone
// types) or by "no clone() method" (plain types). *types.TypeParam → nil:
// generic bodies are checked with unbound params; concrete call sites are
// guarded by the codegen backstop.
func firstSingleOwnerHandle(typ types.Type) types.Type {
	switch t := typ.(type) {
	case *types.Instance:
		if isSingleOwnerInstance(t) {
			return t
		}
		for _, ta := range t.TypeArgs() {
			if off := firstSingleOwnerHandle(ta); off != nil {
				return off
			}
		}
	case *types.Optional:
		return firstSingleOwnerHandle(t.Elem())
	case *types.Tuple:
		for _, e := range t.Elems() {
			if off := firstSingleOwnerHandle(e); off != nil {
				return off
			}
		}
	case *types.Array:
		return firstSingleOwnerHandle(t.Elem())
	}
	return nil
}

// firstStream returns the first generator Stream (`stream[T]`) type found in
// typ — as the type itself or nested within an Optional/Tuple/Array — or nil.
// A stream value is a raw coroutine {handle, slot} pair with no vtable and no
// valid structural-drop path; storing it anywhere that later drops it segfaults
// (T1315, sibling of T1313). It is therefore a non-storable type.
//
// Recursion deliberately does NOT descend into a non-stream *types.Instance's
// type arguments: every nested generic annotation is independently validated
// through resolveInstance -> rejectStreamTypeArg, so recursing here would
// double-report. A stream only ever surfaces at a check as a *direct* type
// (value-flow: literal element / constructor arg) or a *direct* type argument
// (annotation), plus the non-generic Optional/Tuple/Array wrappers that are not
// routed through resolveInstance. *types.TypeParam -> nil: generic bodies are
// checked with unbound params, so a Stream only surfaces at concrete
// instantiation (mirrors firstSingleOwnerHandle).
func firstStream(typ types.Type) types.Type {
	switch t := typ.(type) {
	case *types.Instance:
		if _, ok := types.AsStream(t); ok {
			return t
		}
	case *types.Optional:
		return firstStream(t.Elem())
	case *types.Tuple:
		for _, e := range t.Elems() {
			if s := firstStream(e); s != nil {
				return s
			}
		}
	case *types.Array:
		return firstStream(t.Elem())
	}
	return nil
}

// rejectStreamTypeArg rejects a generic/container instance whose type arguments
// contain a generator Stream (`stream[T]`) — e.g. stream[int][], Box[stream[int]],
// stream[int]?, map[K, stream[int]]. A stream is non-storable (T1315). Called
// beside validateSingleOwnerContainerInstance at every generic-instantiation
// site so both explicit annotations and inferred constructor args are covered.
func (c *Checker) rejectStreamTypeArg(pos ast.Pos, typeArgs []types.Type) {
	for _, ta := range typeArgs {
		if s := firstStream(ta); s != nil {
			c.errorf(pos, "a generator value cannot be stored: %s used as a type argument to a container or generic type; consume it directly with a for-in loop or delegate with `yield *`", s)
			return
		}
	}
}

// ownsElementsByValue reports whether duplicating a value of typ also duplicates
// the elements it holds — the "by value" column of docs/memory-model.md §3, as
// opposed to the behind-a-handle types (Ref/Weak/Channel/Task/Mutex/MutexGuard/
// string) whose dup is a refcount bump or a refusal and never touches the
// payload.
//
// It is DERIVED, not a list of names. The base case is the `duplicates_elements
// annotation, which only Vector carries — the sole `native primitive that owns a
// variable-size buffer by value (§2) and so the only one with no Promise-level
// fields to derive the property from. A fixed-size array owns its elements the
// same way. Every other by-value container reaches the property through a FIELD:
// Map holds Slot[K, V][], Set holds Map[T, bool], and a user's own MyVec[T]
// holds T[]. None of them is named here, which is the point — annotations.md §1
// forbids recovering a property by testing a type's identity, and memory-model.md
// §4 explains why it is unnecessary. (T1926)
//
// The walk follows FIELDS ONLY, never TypeArgs. That is what makes the
// behind-a-handle cases fall out for free: the handle types are `native with zero
// Promise-level fields, so the walk stops at them and `Holder { Ref[Vector[int]] r; }`
// correctly does not own its elements by value.
//
// This is the yes/no form of the one walk collectByValueBuffers implements;
// duplicatingContainerElemTypes asks the same walk WHICH buffers it found, so
// the base case is stated once. Passing a nil sink makes the walk stop at the
// first buffer.
func ownsElementsByValue(typ types.Type) bool {
	return collectByValueBuffersIn(typ, newByValueWalk(nil))
}

// FirstNestedSingleOwnerHandle is the exported entry point for cross-package
// use (codegen / ownership) of the deep single-owner-handle predicate. (T0623)
func FirstNestedSingleOwnerHandle(typ types.Type) types.Type {
	return firstNestedSingleOwnerHandle(typ, nil)
}

// firstNestedSingleOwnerHandle is like firstSingleOwnerHandle but ALSO recurses
// into *types.Named fields and *types.Enum variant fields (cycle-guarded). It
// gates the IMPLICIT structural-clone contexts — dupHeapValue /
// dupHeapValueFields / dupEnumElementInPlace — which shallow-copy a nested
// single-owner-handle pointer (Task/Mutex/MutexGuard) and double-free at drop
// when both the source and the structural copy are dropped. Unlike
// firstSingleOwnerHandle (deliberately shallow, relied on by
// nestedContainerSingleOwnerHandle / reportContainerSingleOwnerNesting), this
// predicate sees through user-type and enum field nesting. (T0482/T0619)
//
// Recursion mirrors firstSingleOwnerHandle (Instance TypeArgs, Optional, Tuple,
// Array) PLUS: *types.Named → each AllFields() type; *types.Enum → each variant
// field type; generic *types.Instance over a user Named/Enum origin → the
// origin's fields/variants under the type-arg substitution. The `native
// container/handle origins (Vector, Ref, Weak, Channel, Task, Mutex,
// MutexGuard, string) need no special case: they declare zero Promise-level
// fields, so the field walk over them is a no-op and a cloneable container
// holding a handle yields nil through the TypeArgs path alone (T1926 —
// previously an identity list, isStdNativeContainerNamed). `seen` cycle-guards
// on the Named/Enum pointer so recursive types (Node{Node? next},
// JsonValue) terminate. Returns non-nil only for Task/Mutex/MutexGuard.
//
// T0675 (audit): the closed set of implicit clone-or-move sites this predicate
// family gates is: (1) enum match-destructure move-out — codegen zero-inits the
// moved-out variant slot and suppresses the subject drop (T0623/T0633,
// nullSubjectHandleSlot); and (2) the ownership by-value-read reject of a
// container/aggregate element that transitively nests a handle — `b := src[i]`,
// `f(src[i])`, `v[i]`, `arr[i]`, `m[k]!` (T1113, rejectIndexExprSingleOwnerMove
// → FirstFieldNestedSingleOwnerHandle). Single-owner handles have no clone
// semantics, so a by-value duplicate is rejected rather than deep-cloned.
func firstNestedSingleOwnerHandle(typ types.Type, seen map[types.Type]bool) types.Type {
	if typ == nil {
		return nil
	}
	if seen == nil {
		seen = make(map[types.Type]bool)
	}
	switch t := typ.(type) {
	case *types.Instance:
		if isSingleOwnerInstance(t) {
			return t
		}
		// TypeArgs recursion — same as firstSingleOwnerHandle (covers a handle
		// appearing as a direct container element, e.g. Vector[Task[T]]).
		for _, ta := range t.TypeArgs() {
			if off := firstNestedSingleOwnerHandle(ta, seen); off != nil {
				return off
			}
		}
		// Recurse a generic type/enum origin's fields/variants under the type-arg
		// substitution (e.g. Holder[string] whose Task[int] field is concrete,
		// not reachable via TypeArgs). The `native handles have no Promise-level
		// fields, so this is a no-op for them.
		switch origin := t.Origin().(type) {
		case *types.Named:
			if seen[origin] {
				return nil
			}
			seen[origin] = true
			subst := types.BuildSubstMap(origin.TypeParams(), t.TypeArgs())
			for _, f := range origin.AllFields() {
				if off := firstNestedSingleOwnerHandle(types.Substitute(f.Type(), subst), seen); off != nil {
					return off
				}
			}
		case *types.Enum:
			if seen[origin] {
				return nil
			}
			seen[origin] = true
			subst := types.BuildSubstMap(origin.TypeParams(), t.TypeArgs())
			for _, v := range origin.Variants() {
				for _, f := range v.Fields() {
					if off := firstNestedSingleOwnerHandle(types.Substitute(f.Type(), subst), seen); off != nil {
						return off
					}
				}
			}
		}
	case *types.Named:
		if seen[t] {
			return nil
		}
		seen[t] = true
		for _, f := range t.AllFields() {
			if off := firstNestedSingleOwnerHandle(f.Type(), seen); off != nil {
				return off
			}
		}
	case *types.Enum:
		if seen[t] {
			return nil
		}
		seen[t] = true
		for _, v := range t.Variants() {
			for _, f := range v.Fields() {
				if off := firstNestedSingleOwnerHandle(f.Type(), seen); off != nil {
					return off
				}
			}
		}
	case *types.Optional:
		return firstNestedSingleOwnerHandle(t.Elem(), seen)
	case *types.Tuple:
		for _, e := range t.Elems() {
			if off := firstNestedSingleOwnerHandle(e, seen); off != nil {
				return off
			}
		}
	case *types.Array:
		return firstNestedSingleOwnerHandle(t.Elem(), seen)
	}
	return nil
}

// FirstFieldNestedSingleOwnerHandle is like FirstNestedSingleOwnerHandle but is
// purpose-built for the by-value container READ gate (T1113), where the unsound
// surface is a single-owner handle (Task/Mutex/MutexGuard) reached ONLY through
// a user-type field or enum variant field. It differs from
// firstNestedSingleOwnerHandle in exactly one way: NO type's TypeArgs are
// recursed, so a container origin is reached only through its fields. That is
// what distinguishes the unsound shallow-copy surface (an enum/struct whose
// variant/field is a Mutex) from sound refcounted nesting (Ref[Mutex],
// Channel[Task], enum{Ref[Mutex]}), whose element dup is a refcount increment,
// not a shallow alias: the handle types are `native with zero Promise-level
// fields, so the walk simply stops at them.
//
// firstNestedSingleOwnerHandle cannot be reused for the read gate: its
// unconditional TypeArgs recursion flags Ref[Mutex] (a false positive — Ref's
// dup is sound). The direct-handle container cases (Vector[Task], Map[K,Mutex])
// are intentionally NOT flagged here either — they are already rejected by the
// caller via isSingleOwnerNativeType on the index RESULT type, and a nested
// container (Vector[Vector[Task]]) is gated at declaration by
// nestedContainerSingleOwnerHandle / the emitVectorElementCloneLoop runtime backstop
// (never silent corruption). Returns nil when no field/variant-nested handle
// is present. (T1113)
func FirstFieldNestedSingleOwnerHandle(typ types.Type) types.Type {
	return firstFieldNestedSingleOwnerHandle(typ, nil)
}

func firstFieldNestedSingleOwnerHandle(typ types.Type, seen map[types.Type]bool) types.Type {
	if typ == nil {
		return nil
	}
	if seen == nil {
		seen = make(map[types.Type]bool)
	}
	switch t := typ.(type) {
	case *types.Instance:
		// A direct Task/Mutex/MutexGuard read result IS reported (so callers can
		// special-case it if desired), but the caller's isSingleOwnerNativeType
		// branch handles those first in practice.
		if isSingleOwnerInstance(t) {
			return t
		}
		switch origin := t.Origin().(type) {
		case *types.Named:
			// TypeArgs are never recursed here — Ref[Mutex] must yield nil
			// (refcounted dup is sound). A generic type's fields are walked
			// under the type-arg substitution; the `native handles have none,
			// so the walk stops at them without naming them (T1926).
			if seen[origin] {
				return nil
			}
			seen[origin] = true
			subst := types.BuildSubstMap(origin.TypeParams(), t.TypeArgs())
			for _, f := range origin.AllFields() {
				if off := firstFieldNestedSingleOwnerHandle(types.Substitute(f.Type(), subst), seen); off != nil {
					return off
				}
			}
		case *types.Enum:
			if seen[origin] {
				return nil
			}
			seen[origin] = true
			subst := types.BuildSubstMap(origin.TypeParams(), t.TypeArgs())
			for _, v := range origin.Variants() {
				for _, f := range v.Fields() {
					if off := firstFieldNestedSingleOwnerHandle(types.Substitute(f.Type(), subst), seen); off != nil {
						return off
					}
				}
			}
		}
	case *types.Named:
		if seen[t] {
			return nil
		}
		seen[t] = true
		for _, f := range t.AllFields() {
			if off := firstFieldNestedSingleOwnerHandle(f.Type(), seen); off != nil {
				return off
			}
		}
	case *types.Enum:
		if seen[t] {
			return nil
		}
		seen[t] = true
		for _, v := range t.Variants() {
			for _, f := range v.Fields() {
				if off := firstFieldNestedSingleOwnerHandle(f.Type(), seen); off != nil {
					return off
				}
			}
		}
	case *types.Optional:
		return firstFieldNestedSingleOwnerHandle(t.Elem(), seen)
	case *types.Tuple:
		for _, e := range t.Elems() {
			if off := firstFieldNestedSingleOwnerHandle(e, seen); off != nil {
				return off
			}
		}
	case *types.Array:
		return firstFieldNestedSingleOwnerHandle(t.Elem(), seen)
	}
	return nil
}

// firstNestedClosure returns the first closure (*types.Signature) found in typ,
// searching transitively through Instance type arguments, user-type fields, enum
// variant fields, Optional, Tuple, and Array element types (cycle-guarded).
// Returns nil if typ contains no closure. (T0813)
//
// A closure value is a fat pointer {fn, env} whose env is a heap struct that may
// own captured strings/vectors/nested closures. The env CANNOT be deep-cloned —
// the captured frame is opaque. Native container clone()/filled() (and the
// heap-user-type dup path) shallow-copy element bytes, aliasing the same env
// pointer into both the source and the clone → double-free / silently-empty
// clone at drop. A type that transitively contains a closure is therefore
// non-cloneable. The recursion shape exactly mirrors firstNestedSingleOwnerHandle
// (Instance TypeArgs + Named/Enum fields under the type-arg subst, Optional,
// Tuple, Array; the `native handles declare no fields, so the walk stops at
// them without naming them). Mirroring the handle predicate means a refcounted
// container of a closure (e.g. Ref[() -> int]) is conservatively rejected too —
// acceptable, since cloning a closure-containing container is semantically
// meaningless.
//
// Unlike the single-owner-handle predicate, recursion STOPS at any user
// type/enum that provides its own clone() method: the native dup path
// (cloneHeapElement / emitVariantFieldDup) calls that clone() instead of
// shallow-copying, and a hand-written clone() is responsible for reconstructing
// its own closure fields (a closure env can't be deep-copied, but the author
// can rebuild the closure). So a clone()-bearing type is cloneable regardless of
// what it transitively holds — mirroring isCloneableField. (A single-owner
// handle has no such escape: no clone() can duplicate a Task, so that predicate
// always descends.)
func firstNestedClosure(typ types.Type, seen map[types.Type]bool) *types.Signature {
	if typ == nil {
		return nil
	}
	if seen == nil {
		seen = make(map[types.Type]bool)
	}
	switch t := typ.(type) {
	case *types.Signature:
		return t
	case *types.Instance:
		for _, ta := range t.TypeArgs() {
			if sig := firstNestedClosure(ta, seen); sig != nil {
				return sig
			}
		}
		switch origin := t.Origin().(type) {
		case *types.Named:
			if origin.LookupMethod("clone") != nil || seen[origin] {
				return nil
			}
			seen[origin] = true
			subst := types.BuildSubstMap(origin.TypeParams(), t.TypeArgs())
			for _, f := range origin.AllFields() {
				if sig := firstNestedClosure(types.Substitute(f.Type(), subst), seen); sig != nil {
					return sig
				}
			}
		case *types.Enum:
			if origin.LookupMethod("clone") != nil || seen[origin] {
				return nil
			}
			seen[origin] = true
			subst := types.BuildSubstMap(origin.TypeParams(), t.TypeArgs())
			for _, v := range origin.Variants() {
				for _, f := range v.Fields() {
					if sig := firstNestedClosure(types.Substitute(f.Type(), subst), seen); sig != nil {
						return sig
					}
				}
			}
		}
	case *types.Named:
		if t.LookupMethod("clone") != nil || seen[t] {
			return nil
		}
		seen[t] = true
		for _, f := range t.AllFields() {
			if sig := firstNestedClosure(f.Type(), seen); sig != nil {
				return sig
			}
		}
	case *types.Enum:
		if t.LookupMethod("clone") != nil || seen[t] {
			return nil
		}
		seen[t] = true
		for _, v := range t.Variants() {
			for _, f := range v.Fields() {
				if sig := firstNestedClosure(f.Type(), seen); sig != nil {
					return sig
				}
			}
		}
	case *types.Optional:
		return firstNestedClosure(t.Elem(), seen)
	case *types.Tuple:
		for _, e := range t.Elems() {
			if sig := firstNestedClosure(e, seen); sig != nil {
				return sig
			}
		}
	case *types.Array:
		return firstNestedClosure(t.Elem(), seen)
	}
	return nil
}

// FirstFieldNestedClosure is the exported entry point for the by-value container
// READ gate (T1230). It is to firstNestedClosure what
// FirstFieldNestedSingleOwnerHandle is to firstNestedSingleOwnerHandle: it finds
// a closure (*types.Signature) reached ONLY through a user-type field, enum
// variant field, Optional/Tuple/Array — never through a type's TypeArgs, so a
// behind-a-handle origin (Ref/Weak/Channel/Task/Mutex/string) is fully opaque.
// This is what distinguishes the unsound shallow-copy surface (a
// struct/enum whose field is a closure, e.g. `Fn { () -> int f; }`) from sound
// refcounted nesting (`Ref[() -> int]`), whose element dup is a refcount
// increment, not a shallow alias of the fat pointer's env. Like
// firstNestedClosure, recursion STOPS at any user type/enum that provides its own
// clone() method (a hand-written clone can rebuild the closure). Returns nil when
// no field/variant-nested closure is present. (T1230)
func FirstFieldNestedClosure(typ types.Type) *types.Signature {
	return firstFieldNestedClosure(typ, nil, true)
}

// FirstFieldNestedClosureDeep is like FirstFieldNestedClosure but treats `typ` as
// if it were a struct/enum FIELD (nested), so a by-value container of closures at
// the TOP of `typ` is recursed into rather than treated as opaque.
// heapTypeSafeToDup uses this per field: a field
// `Vector[() -> int]` must make the containing struct un-dup-safe (its deep-copy
// would zero the closure env → SEGV, T1260), whereas a BARE top-level container
// read (`vv[0]` → Vector[() -> int]) has its own owned null-dup path (T1045) and
// must stay dup-owned, so FirstFieldNestedClosure keeps it opaque. (T1260)
func FirstFieldNestedClosureDeep(typ types.Type) *types.Signature {
	return firstFieldNestedClosure(typ, nil, false)
}

// firstFieldNestedClosure walks `typ` for a closure reached through struct/enum
// fields, Optional/Tuple/Array elements, or (when not at top level) value-copying
// container TypeArgs. `topLevel` is true only for the outermost queried type: a
// value-copying container at top level is treated as opaque (its own null-dup
// path owns the read), while the same container reached via a field is recursed
// into (a struct-field deep-copy would zero the closure env). All recursive calls
// descend into fields/elements, so they pass topLevel=false. (T1260)
func firstFieldNestedClosure(typ types.Type, seen map[types.Type]bool, topLevel bool) *types.Signature {
	if typ == nil {
		return nil
	}
	if seen == nil {
		seen = make(map[types.Type]bool)
	}
	switch t := typ.(type) {
	case *types.Signature:
		return t
	case *types.Instance:
		switch origin := t.Origin().(type) {
		case *types.Named:
			// A container that owns its elements by value: its clone()/dup
			// DEEP-copies elements, so a closure element WOULD be unsoundly cloned
			// (its env zeroed → null fat pointer → SEGV on invoke). When reached via
			// a struct/enum FIELD (topLevel=false), recurse into TypeArgs so a struct
			// holding `Vector[() -> int]` is judged un-dup-safe (read as a borrow),
			// matching the direct `.clone()` sema rejection. At TOP LEVEL, keep it
			// opaque: a bare container read has its own owned null-dup path (T1045)
			// and must not be flipped to a borrow (that would leak the duped
			// container). Only genuinely behind-a-handle types (Ref/Weak/Channel/
			// Task/Mutex/string) stay opaque at every level — and they are opaque
			// because they have no fields, not because they are named. (T1260/T1926)
			//
			// TypeArgs are recursed BEFORE `seen` is consulted or marked: the field
			// walk below marks seen[origin], and a struct with two fields of the same
			// container origin (`Pair { MyVec[int] a; MyVec[() -> int] b; }`) must
			// still judge the second on its own type arguments.
			//
			// The question here is the yes/no one — does duplicating this deep-copy
			// ANY buffer — not duplicatingContainerElemTypes' "which type args does a
			// buffer hold". Erring wide costs at most a read reclassified as a borrow
			// on a type argument the container never stores; erring narrow would zero
			// a live closure env.
			if ownsElementsByValue(t) {
				if topLevel {
					return nil
				}
				for _, ta := range t.TypeArgs() {
					if sig := firstFieldNestedClosure(ta, seen, false); sig != nil {
						return sig
					}
				}
				// Fall through to the field walk: a by-value container may hold a
				// closure in a field rather than a type argument.
			}
			// Behind-a-handle origin: opaque, because Ref[()->int] must yield nil
			// (refcounted dup is a count bump, not a shallow env alias) — and the
			// handle types declare no Promise-level fields, so the walk below stops
			// at them on its own. A clone()-bearing type also stops recursion (its
			// clone rebuilds the closure).
			if origin.LookupMethod("clone") != nil || seen[origin] {
				return nil
			}
			seen[origin] = true
			subst := types.BuildSubstMap(origin.TypeParams(), t.TypeArgs())
			for _, f := range origin.AllFields() {
				if sig := firstFieldNestedClosure(types.Substitute(f.Type(), subst), seen, false); sig != nil {
					return sig
				}
			}
		case *types.Enum:
			if origin.LookupMethod("clone") != nil || seen[origin] {
				return nil
			}
			seen[origin] = true
			subst := types.BuildSubstMap(origin.TypeParams(), t.TypeArgs())
			for _, v := range origin.Variants() {
				for _, f := range v.Fields() {
					if sig := firstFieldNestedClosure(types.Substitute(f.Type(), subst), seen, false); sig != nil {
						return sig
					}
				}
			}
		}
	case *types.Named:
		if t.LookupMethod("clone") != nil || seen[t] {
			return nil
		}
		seen[t] = true
		for _, f := range t.AllFields() {
			if sig := firstFieldNestedClosure(f.Type(), seen, false); sig != nil {
				return sig
			}
		}
	case *types.Enum:
		if t.LookupMethod("clone") != nil || seen[t] {
			return nil
		}
		seen[t] = true
		for _, v := range t.Variants() {
			for _, f := range v.Fields() {
				if sig := firstFieldNestedClosure(f.Type(), seen, false); sig != nil {
					return sig
				}
			}
		}
	case *types.Optional:
		return firstFieldNestedClosure(t.Elem(), seen, false)
	case *types.Tuple:
		for _, e := range t.Elems() {
			if sig := firstFieldNestedClosure(e, seen, false); sig != nil {
				return sig
			}
		}
	case *types.Array:
		return firstFieldNestedClosure(t.Elem(), seen, false)
	}
	return nil
}

// nestedContainerSingleOwnerHandle returns the single-owner handle that makes
// typ an unsound container element, or nil. typ qualifies when it is itself a
// *container* — a fixed-size Array, or an instance that holds one of its type
// arguments in a by-value buffer (duplicatingContainerElemTypes) — and the
// handle sits in an element the container would DUPLICATE. Such a container,
// used as another container's element/key/value, forces the outer container's
// literal-lowering / push-dup / clone / realloc paths to duplicate the inner
// handle — unsound (double-free at drop). A *direct* handle element
// (Vector[Task[T]]) is fine (T0508 move-only collection), and an
// Optional/Tuple wrapping a handle is NOT a container and has its own drop
// handling (T0558), so neither triggers the nesting rule. (T0545)
//
// The handle is returned rather than a bool so the diagnostic names the one the
// rule actually objects to. Searching the whole instance instead would report
// the first handle in type-argument order, which in
// `MyVec[A, B] { B[] xs; A other; }` is the `A` the container merely holds —
// a legal direct element, and not why the type was rejected. (T1926)
func nestedContainerSingleOwnerHandle(typ types.Type) types.Type {
	switch t := typ.(type) {
	case *types.Array:
		return firstSingleOwnerHandle(t)
	case *types.Instance:
		// Judge the reported elements, not the whole instance: a type argument
		// the container does NOT hold in a buffer is not one it duplicates, so a
		// handle there is the permitted direct-element case (T1926).
		for _, et := range duplicatingContainerElemTypes(t.Origin(), t.TypeArgs()) {
			if off := firstSingleOwnerHandle(et); off != nil {
				return off
			}
		}
	}
	return nil
}

// checkContainerNotCloneable reports an error if any of the supplied container
// element/key/value types transitively contains a single-owner handle, which
// makes the container non-cloneable / non-fillable. opName is the verb used in
// the message ("cloned" or "filled"). Returns true if an error was emitted.
// (T0545)
func (c *Checker) checkContainerNotCloneable(pos ast.Pos, containerType types.Type, elemTypes []types.Type, opName string) bool {
	for _, et := range elemTypes {
		// Deep predicate (T0482/T0619): a container element that transitively
		// owns a single-owner handle through a user-type field or enum variant
		// (e.g. Vector[Holder] where Holder{Task[int]}, Vector[Box] where
		// Box.Has(Task)) is non-cloneable too — the native clone path
		// shallow-copies the handle pointer and double-frees at drop.
		if off := firstNestedSingleOwnerHandle(et, nil); off != nil {
			c.errorf(pos, "%s cannot be %s: it contains %s, a single-owner handle with no clone() semantics (single-owner handles are move-only)",
				containerType, opName, off)
			return true
		}
		// T0813: a container element that transitively owns a closure
		// (*types.Signature) is non-cloneable too — the env (captured frame) is
		// opaque and cannot be deep-cloned, so the native clone path shallow-
		// copies the env pointer and double-frees (struct field) / silently
		// empties the clone (enum variant) at drop.
		if sig := firstNestedClosure(et, nil); sig != nil {
			c.errorf(pos, "%s cannot be %s: it contains a closure field (%s), and closure environments cannot be duplicated",
				containerType, opName, sig)
			return true
		}
	}
	return false
}

// checkPushNestedHandleArg rejects `vec.push(arg)` when vec's element type
// transitively owns a single-owner handle through a user-type field or enum
// variant (e.g. Vector[Holder] where Holder{Task[int]}) AND arg is an
// implicit-clone (non-consuming) source. Codegen's push dup decision
// (expr.go:4866-4934) deep-copies an *ast.IndexExpr source (always-dup, T0376)
// via dupHeapValue → dupHeapValueFields, which shallow-copies the nested
// handle pointer → double-free at drop. A *direct* handle element
// (Vector[Task[T]]) is deliberately NOT gated here — fresh-temp pushes are the
// T0508 move-only model and borrowed-param pushes are already rejected by
// T0556/T0586 ownership. The borrowed-ident implicit-clone source is likewise
// already covered by T0586 (a plain heap user type owning a handle is
// non-alias-safe), so only the IndexExpr gap remains. (T0482)
func (c *Checker) checkPushNestedHandleArg(e *ast.CallExpr) {
	mem, ok := e.Callee.(*ast.MemberExpr)
	if !ok || mem.Field != "push" || len(e.Args) != 1 {
		return
	}
	recv := c.info.Types[mem.Target]
	if ref, ok := recv.(*types.MutRef); ok {
		recv = ref.Elem()
	}
	if ref, ok := recv.(*types.SharedRef); ok {
		recv = ref.Elem()
	}
	elem, ok := types.AsVector(recv)
	if !ok {
		return
	}
	// Direct single-owner handle elements are out of scope (T0508/T0556).
	if isSingleOwnerType(elem) {
		return
	}
	off := firstNestedSingleOwnerHandle(elem, nil)
	if off == nil {
		return
	}
	// Implicit-clone source: an index expression is always element-duped by
	// codegen (T0376), so the pushed copy shares the source's handle pointer.
	if _, isIdx := e.Args[0].Value.(*ast.IndexExpr); isIdx {
		c.errorf(e.Args[0].Value.Pos(),
			"cannot push %s: it transitively contains %s, a single-owner handle with no clone() semantics — indexing copies the element (move-only handles cannot be duplicated); move a freshly-constructed value instead",
			elem, off)
	}
}

// checkDestructureNoHandleField rejects a match destructure that binds out a
// variant field whose type transitively owns a single-owner handle
// (Task/Mutex/MutexGuard) when the subject is NOT a movable owned local —
// i.e. when move-out is not possible, so structural-copy would double-free.
//
// T0623 relaxes the T0482 conservative gate: when the subject is an owned
// local ident (not a borrowed `&E`/`E~`, not a non-ident expression), the
// binding TAKES OWNERSHIP of the handle (move-out semantics implemented in
// ownership + codegen). For those forms, no error is emitted here. A `_`
// binding never copies and is always safe. subst is the subject's type-arg
// substitution for a generic enum instance (may be nil).
func (c *Checker) checkDestructureNoHandleField(pos ast.Pos, subject ast.Expr, subjectType types.Type, v *types.Variant, bindings []string, subst map[*types.TypeParam]types.Type) {
	if v == nil {
		return
	}
	n := len(bindings)
	if n > v.NumFields() {
		n = v.NumFields()
	}
	movable := c.subjectIsMovableOwnedLocal(subject, subjectType)
	for i := 0; i < n; i++ {
		if bindings[i] == "_" {
			continue
		}
		ft := v.Fields()[i].Type()
		if subst != nil {
			ft = types.Substitute(ft, subst)
		}
		off := firstNestedSingleOwnerHandle(ft, nil)
		if off == nil {
			continue
		}
		if movable {
			// T0623: move-out — binding takes ownership; ownership marks the
			// subject as Moved and codegen clears the subject's drop flag.
			continue
		}
		c.errorf(pos,
			"cannot destructure variant %s: subject must be an owned local to move out %s (binding '%s') — single-owner handles are move-only; assign to a local before matching, or use '_' to skip the field",
			v.Name(), off, bindings[i])
	}
}

// subjectIsMovableOwnedLocal reports whether subject is an owned-local ident
// whose static type permits move-out (not a borrow). Used by T0623 to gate the
// relaxed-destructure rule. The owner-resolution mirrors the codegen / ownership
// idiom — a borrow (`&E`/`E~`) cannot be moved out of (its parent owner still
// drops it), but a plain ident binding `e := ...` can. Non-ident subjects
// (function-call return, field access, this) keep the T0482-style reject.
func (c *Checker) subjectIsMovableOwnedLocal(subject ast.Expr, subjectType types.Type) bool {
	if subject == nil {
		return false
	}
	id, ok := subject.(*ast.IdentExpr)
	if !ok {
		return false
	}
	if id.Name == "_" {
		return false
	}
	// Reject borrowed subjects.
	switch subjectType.(type) {
	case *types.SharedRef, *types.MutRef:
		return false
	}
	// T0998: a bare `T name` or `T~ name` parameter is a borrow (its argument
	// belongs to the caller), and the receiver `this`/`~this` is never owned —
	// a single-owner handle cannot be moved out of any of them. Only a `move`
	// parameter (RefMut) or a plain owned local is movable.
	if c.curFunc != nil {
		if c.curFunc.Recv() != nil && id.Name == "this" {
			return false
		}
		for _, p := range c.curFunc.Params() {
			if p.Name() == id.Name {
				return p.Ref() == types.RefMut // movable only if it is a `move` parameter
			}
		}
	}
	return true
}

// enumDestructureSubst returns the variant lookup and type-arg substitution for a
// destructure pattern over subjectType. (T0482)
func enumDestructureSubst(subjectType types.Type, enum *types.Enum) map[*types.TypeParam]types.Type {
	// T1018: strip borrows so a borrowed generic enum subject still yields the
	// concrete type-arg substitution for the handle-field double-free gate.
	if inst, ok := stripRef(subjectType).(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Enum); ok && origin == enum {
			return types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
		}
	}
	return nil
}

// reportContainerSingleOwnerNesting reports an error if elemType is itself a
// by-value container (or Array) that transitively contains a single-owner
// handle. A *direct* handle element is permitted (T0508 move-only
// collections), and Optional/Tuple wrapping a handle is handled separately
// (T0558) — only a nested *container* forces an unsound duplicate. (T0545)
func (c *Checker) reportContainerSingleOwnerNesting(pos ast.Pos, elemType types.Type) {
	if elemType == nil {
		return
	}
	off := nestedContainerSingleOwnerHandle(elemType)
	if off == nil {
		return
	}
	c.errorf(pos, "%s cannot be a container element: it transitively contains %s, a single-owner handle (single-owner handles may only appear as direct container elements, not nested inside another container)",
		elemType, off)
}

// duplicatingContainerElemTypes returns the element/key/value types an outer
// container would have to duplicate along with a value of this origin, or nil
// when it would duplicate none of them.
//
// Derived, so no container is named. A type argument is reported when it sits
// inside a **by-value buffer** the type reaches through its fields — the same
// base case ownsElementsByValue rests on: the `duplicates_elements annotation
// (only Vector carries it) or a fixed-size array. Vector reports its own type
// args because it *is* the buffer; Map reports K and V because both occur in
// Slot[K, V][] _buckets; Set reports T through Map[T, bool] _map; and a user's
// own MyVec[T] { T[] items; } or enum Bag[T] { Items(T[] xs) } report T through
// exactly the same walk. That is the objective — the standard library's
// containers are no more special than anyone else's (memory-model.md §4).
// (T0545/T1926)
//
// It reports the args held in a buffer rather than *all* of them, because those
// are different questions. `Job[T] { string[] tags; T handle; }` reaches a
// buffer, but T is not in it: duplicating a Job duplicates the tags, not the
// handle, so Job[Task[int]] is as legitimate a container element as
// `Box[T] { T handle; }` — the T0508 move-only collection. Blaming every type
// arg of any type that happens to own a vector would reject that shape with a
// message about a nesting that never happened.
//
// Fields are walked UNDER the type-arg substitution, so a buffer only the
// substitution produces is found too: in `Wrapper[T] { T inner; }` at
// T = Vector[Task[int]] the field *is* the buffer and *is* the type arg, so the
// arg occurs in it and is reported.
func duplicatingContainerElemTypes(origin types.Type, typeArgs []types.Type) []types.Type {
	if len(typeArgs) == 0 {
		return nil
	}
	// The origin is itself the buffer, so every type argument is an element it
	// duplicates. This is the `duplicates_elements base case; there is no
	// instance node below to record, and no fields to walk.
	if n, ok := origin.(*types.Named); ok && n.DuplicatesElements() {
		return typeArgs
	}
	var buffers []types.Type
	collectByValueBuffers(origin, typeArgs, newByValueWalk(&buffers))
	if len(buffers) == 0 {
		return nil
	}
	var out []types.Type
	for _, ta := range typeArgs {
		for _, buf := range buffers {
			if typeOccursIn(ta, buf) {
				out = append(out, ta)
				break
			}
		}
	}
	return out
}

// byValueWalk is the state of one by-value buffer traversal. It carries two
// separate guards because they answer two different questions. (T1926)
//
// `path` is TERMINATION: an origin is marked while it sits on the current path
// and unmarked on the way back out. It is what stops a recursive type, including
// one whose every level is a fresh instantiation — `Rec[T] { Rec[Vector[T]]? next; }`
// is accepted by sema today, so an instantiation-keyed guard alone would descend
// forever.
//
// `done` is COST: an (origin, type args) pair explored to completion is never
// explored again, so a type graph in which several fields reach the same type
// (`L0 { L1 a; L1 b; }`) is walked once per instantiation instead of once per
// PATH that reaches it — the difference between linear and 2^depth. It is keyed
// on the instantiation rather than the origin alone, which is what lets two
// fields of the same generic origin at different arguments
// (`Two[T] { MyVec[int] a; MyVec[T] b; }`) each be judged on their own
// arguments instead of the second inheriting the first's answer.
//
// An answer the `path` guard truncated holds only where the same truncation
// applies, so each memo entry records the origins that cut it (`deps`) and is
// reused only while all of them are still on the path. That keeps a *cyclic*
// branchy graph linear too — in `L0 { L1 a; L1 b; } … Ln { L0? back; }` every
// level is cut by `L0` and every reuse happens under `L0`, so refusing to
// memoize anything that was cut would put the 2^depth walk straight back.
// An origin is dropped from its own `deps`: a cut that names the node being
// finished is the cycle closing on itself, and its answer is complete.
type byValueWalk struct {
	// out collects every buffer found, or is nil for the yes/no question
	// (ownsElementsByValue), in which case the walk stops at the first one.
	out  *[]types.Type
	path map[types.Type]bool
	done map[types.Type][]byValueMemo
	// cuts accumulates the origins truncated within the subtree currently being
	// walked; each node saves the parent's set, starts a fresh one, and merges
	// on the way back out.
	cuts []types.Type
}

// byValueMemo is one completed (type args → found) answer for an origin, valid
// while every origin in deps is on the walk's path.
type byValueMemo struct {
	args  []types.Type
	found bool
	deps  []types.Type
}

// newByValueWalk leaves both maps nil: ownsElementsByValue asks a fresh walk per
// generic instance it meets, and the common answer — a field that IS a
// `duplicates_elements instance — is settled before either map is touched.
// Reading a nil map is fine; enter and record fill them in on first write.
func newByValueWalk(out *[]types.Type) *byValueWalk {
	return &byValueWalk{out: out}
}

// enter marks origin as being on the current path, returning false when it is
// already there (the termination guard) and recording the truncation.
func (w *byValueWalk) enter(origin types.Type) bool {
	if w.path[origin] {
		w.cuts = appendUnique(w.cuts, origin)
		return false
	}
	if w.path == nil {
		w.path = make(map[types.Type]bool)
	}
	w.path[origin] = true
	return true
}

func (w *byValueWalk) leave(origin types.Type) { delete(w.path, origin) }

// lookup returns a memoized answer for this exact instantiation, if one applies
// to the current path, along with the truncations it rests on.
func (w *byValueWalk) lookup(origin types.Type, typeArgs []types.Type) (*byValueMemo, bool) {
	for i := range w.done[origin] {
		m := &w.done[origin][i]
		if !identicalTypeArgs(m.args, typeArgs) {
			continue
		}
		usable := true
		for _, d := range m.deps {
			if !w.path[d] {
				usable = false
				break
			}
		}
		if usable {
			return m, true
		}
	}
	return nil, false
}

func (w *byValueWalk) record(origin types.Type, typeArgs []types.Type, found bool, deps []types.Type) {
	if w.done == nil {
		w.done = make(map[types.Type][]byValueMemo)
	}
	w.done[origin] = append(w.done[origin], byValueMemo{args: typeArgs, found: found, deps: deps})
}

func appendUnique(list []types.Type, t types.Type) []types.Type {
	for _, e := range list {
		if e == t {
			return list
		}
	}
	return append(list, t)
}

// addBuffer records a buffer, unless the walk only wants the yes/no answer.
func (w *byValueWalk) addBuffer(buf types.Type) {
	if w.out != nil {
		*w.out = append(*w.out, buf)
	}
}

// identicalTypeArgs compares two type-argument lists element-wise.
func identicalTypeArgs(a, b []types.Type) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !types.Identical(a[i], b[i]) {
			return false
		}
	}
	return true
}

// collectByValueBuffers reports whether origin reaches a by-value buffer — a
// fixed-size array, or an instance of a `duplicates_elements type — through its
// fields (an enum's: its variant fields), under the type-arg substitution, and
// appends every such buffer to w.out. Descent stops AT a buffer: the recorded
// type already spells out everything the buffer holds, so typeOccursIn can
// answer for all of it. A memoized `found` is returned without re-appending,
// which is why w.out accumulates across the whole traversal rather than per
// node. (T1926)
func collectByValueBuffers(origin types.Type, typeArgs []types.Type, w *byValueWalk) bool {
	switch origin.(type) {
	case *types.Named, *types.Enum:
	default:
		return false
	}
	if m, ok := w.lookup(origin, typeArgs); ok {
		// A reused answer holds only where the truncations that produced it still
		// apply, so the caller inherits them. Without this the caller would be
		// recorded as if it had been walked in full, and could then be reused with
		// the cutting origin off the path — a stale `false, which for the closure
		// gate is the direction that zeroes a live env.
		for _, d := range m.deps {
			w.cuts = appendUnique(w.cuts, d)
		}
		return m.found
	}
	if !w.enter(origin) {
		return false
	}
	defer w.leave(origin)

	var fieldTypes []types.Type
	switch n := origin.(type) {
	case *types.Named:
		subst := types.BuildSubstMap(n.TypeParams(), typeArgs)
		for _, f := range n.AllFields() {
			fieldTypes = append(fieldTypes, types.Substitute(f.Type(), subst))
		}
	case *types.Enum:
		subst := types.BuildSubstMap(n.TypeParams(), typeArgs)
		for _, v := range n.Variants() {
			for _, f := range v.Fields() {
				fieldTypes = append(fieldTypes, types.Substitute(f.Type(), subst))
			}
		}
	}
	outerCuts := w.cuts
	w.cuts = nil
	found := false
	for _, ft := range fieldTypes {
		if collectByValueBuffersIn(ft, w) {
			found = true
			if w.out == nil {
				break // yes/no question — one buffer settles it
			}
		}
	}
	deps := w.cuts
	w.cuts = outerCuts
	for i, d := range deps {
		if d == origin {
			// The cycle closed on this node, so its answer is complete.
			deps = append(deps[:i:i], deps[i+1:]...)
			break
		}
	}
	w.record(origin, typeArgs, found, deps)
	for _, d := range deps {
		w.cuts = appendUnique(w.cuts, d)
	}
	return found
}

// collectByValueBuffersIn is collectByValueBuffers for one already-substituted
// field type. The `native handle types need no special case here for the same
// reason they need none anywhere else: they declare no Promise-level fields, so
// the walk stops at them. (T1926)
func collectByValueBuffersIn(typ types.Type, w *byValueWalk) bool {
	switch t := typ.(type) {
	case *types.Array:
		w.addBuffer(t)
		return true
	case *types.Optional:
		return collectByValueBuffersIn(t.Elem(), w)
	case *types.Tuple:
		found := false
		for _, e := range t.Elems() {
			if collectByValueBuffersIn(e, w) {
				found = true
				if w.out == nil {
					return true
				}
			}
		}
		return found
	case *types.Instance:
		if n, ok := t.Origin().(*types.Named); ok && n.DuplicatesElements() {
			w.addBuffer(t)
			return true
		}
		return collectByValueBuffers(t.Origin(), t.TypeArgs(), w)
	case *types.Named:
		if t.DuplicatesElements() {
			w.addBuffer(t)
			return true
		}
		return collectByValueBuffers(t, nil, w)
	case *types.Enum:
		return collectByValueBuffers(t, nil, w)
	}
	return false
}

// typeOccursIn reports whether needle appears in hay — as hay itself, or nested
// anywhere inside its type arguments / element types. Used to ask which of a
// container's type arguments a by-value buffer actually holds. (T1926)
func typeOccursIn(needle, hay types.Type) bool {
	if needle == nil || hay == nil {
		return false
	}
	if types.Identical(needle, hay) {
		return true
	}
	switch t := hay.(type) {
	case *types.Instance:
		for _, ta := range t.TypeArgs() {
			if typeOccursIn(needle, ta) {
				return true
			}
		}
	case *types.Optional:
		return typeOccursIn(needle, t.Elem())
	case *types.Array:
		return typeOccursIn(needle, t.Elem())
	case *types.SharedRef:
		return typeOccursIn(needle, t.Elem())
	case *types.MutRef:
		return typeOccursIn(needle, t.Elem())
	case *types.Tuple:
		for _, e := range t.Elems() {
			if typeOccursIn(needle, e) {
				return true
			}
		}
	}
	return false
}

// validateSingleOwnerContainerInstance enforces the nesting rule for an
// explicitly written or inferred container instance (e.g. Vector[Vector[Task]],
// Map[K, Vector[Task]]). Called alongside validateSendableInstance. (T0545)
//
// T0616: when checking inside a generic body and the nested container's element
// references a TypeParam, defer the check to the call site via recordCloneReq
// so generic indirection (`outer[T] { Vector[Vector[T]] v; }` instantiated with
// T = Task[int]) doesn't slip past the direct nesting gate.
func (c *Checker) validateSingleOwnerContainerInstance(pos ast.Pos, origin types.Type, typeArgs []types.Type) {
	for _, et := range duplicatingContainerElemTypes(origin, typeArgs) {
		c.reportContainerSingleOwnerNesting(pos, et)
		if (c.curFuncObj != nil || c.curMethodObj != nil) &&
			isContainerWithTypeParam(et) {
			c.recordCloneReq(et, pos, "nested container element")
		}
	}
}

// isContainerWithTypeParam reports whether typ is itself a by-value *container*
// (instance or Array) whose element/key/value type expression
// references a TypeParam — meaning substitution at the call site could expose
// a single-owner handle. (T0616)
func isContainerWithTypeParam(typ types.Type) bool {
	switch t := typ.(type) {
	case *types.Array:
		return types.ContainsTypeParam(t.Elem())
	case *types.Instance:
		for _, et := range duplicatingContainerElemTypes(t.Origin(), t.TypeArgs()) {
			if types.ContainsTypeParam(et) {
				return true
			}
		}
	}
	return false
}

// recordCloneReq appends a cloneability requirement to the current generic
// function or method being checked. No-op when not inside a generic body.
// The requirement is validated when the enclosing function/method is called
// with concrete type arguments (T0616).
func (c *Checker) recordCloneReq(typeExpr types.Type, pos ast.Pos, opDesc string) {
	if typeExpr == nil {
		return
	}
	req := CloneabilityRequirement{TypeExpr: typeExpr, Pos: pos, OpDesc: opDesc}
	if c.curMethodObj != nil {
		for _, r := range c.info.MethodCloneReqs[c.curMethodObj] {
			if r.OpDesc == opDesc && r.Pos == pos && types.Identical(r.TypeExpr, typeExpr) {
				return
			}
		}
		c.info.MethodCloneReqs[c.curMethodObj] = append(c.info.MethodCloneReqs[c.curMethodObj], req)
		return
	}
	if c.curFuncObj != nil {
		for _, r := range c.info.FuncCloneReqs[c.curFuncObj] {
			if r.OpDesc == opDesc && r.Pos == pos && types.Identical(r.TypeExpr, typeExpr) {
				return
			}
		}
		c.info.FuncCloneReqs[c.curFuncObj] = append(c.info.FuncCloneReqs[c.curFuncObj], req)
	}
}

// propagateCloneReqs propagates cloneability requirements transitively across
// generic call edges. When generic `f[T]` calls generic `g[T]` (or `g[h(T)]`)
// in its body, any requirement R on g must also become a requirement on f
// (after substituting g's TypeParams via the call's subst map) so that the
// eventual concrete call site for f catches single-owner-handle violations
// that arise from g's internal use.
//
// Iterates to a fixed point — when adding a requirement to f grows f's
// requirement set, callers of f need a fresh pass too. Cycles terminate
// because new requirements are deduped by (TypeExpr, OpDesc, Pos). Concrete
// substitutions that expose a single-owner handle emit one error per
// (CallPos, OpDesc, substituted-type) triple — deduped via emitted-set to
// avoid double errors when the same edge fires across multiple iterations
// (T0616).
func (c *Checker) propagateCloneReqs() {
	if len(c.info.GenericCallEdges) == 0 {
		return
	}
	emitted := make(map[string]bool)
	for iter := 0; iter < 64; iter++ {
		changed := false
		for _, edge := range c.info.GenericCallEdges {
			var calleeReqs []CloneabilityRequirement
			if edge.CalleeFunc != nil {
				calleeReqs = c.info.FuncCloneReqs[edge.CalleeFunc]
			} else if edge.CalleeMethod != nil {
				calleeReqs = c.info.MethodCloneReqs[edge.CalleeMethod]
			}
			if len(calleeReqs) == 0 {
				continue
			}
			for _, req := range calleeReqs {
				substituted := types.Substitute(req.TypeExpr, edge.Subst)
				if !types.ContainsTypeParam(substituted) {
					if off := firstSingleOwnerHandle(substituted); off != nil {
						key := edge.CallPos.String() + "|" + req.OpDesc + "|" + substituted.String()
						if !emitted[key] {
							emitted[key] = true
							c.errorf(edge.CallPos,
								"cannot instantiate generic with %s: %s is a single-owner handle, but %s (at %s) would duplicate it (single-owner handles are move-only)",
								substituted, off, req.OpDesc, req.Pos)
						}
					}
					// T0813: the concrete substitution may also expose a closure
					// field (e.g. f[T]() { Vector[T]().clone() } instantiated with
					// T = StructWithClosure) — reject at the concrete call edge,
					// mirroring the single-owner-handle case above.
					if sig := firstNestedClosure(substituted, nil); sig != nil {
						key := edge.CallPos.String() + "|closure|" + req.OpDesc + "|" + substituted.String()
						if !emitted[key] {
							emitted[key] = true
							c.errorf(edge.CallPos,
								"cannot instantiate generic with %s: it contains a closure field (%s), but %s (at %s) would duplicate the closure environment, which cannot be cloned",
								substituted, sig, req.OpDesc, req.Pos)
						}
					}
					// T1201: a `clone generic whose TypeParam field is now bound to
					// a concrete non-cloneable arg — re-run the T0666 instantiation
					// check at the call site. validateCloneInstance self-gates on
					// IsClone(), so container/handle requirements (Vector, Task,
					// etc.) fall through harmlessly.
					if inst, ok := substituted.(*types.Instance); ok {
						key := edge.CallPos.String() + "|cloneinst|" + substituted.String()
						if !emitted[key] {
							emitted[key] = true
							c.validateCloneInstance(edge.CallPos, inst.Origin(), inst.TypeArgs())
						}
					}
					continue
				}
				if c.addCloneReq(edge.CallerFunc, edge.CallerMethod,
					CloneabilityRequirement{
						TypeExpr: substituted,
						Pos:      req.Pos,
						OpDesc:   req.OpDesc,
					}) {
					changed = true
				}
			}
		}
		if !changed {
			return
		}
	}
}

// addCloneReq appends req to the caller's requirement set if not already
// present (dedup on TypeExpr/OpDesc/Pos). Returns true if a new requirement
// was added. (T0616)
func (c *Checker) addCloneReq(fn *types.Func, method *types.Method, req CloneabilityRequirement) bool {
	if fn != nil {
		for _, existing := range c.info.FuncCloneReqs[fn] {
			if existing.OpDesc == req.OpDesc && existing.Pos == req.Pos &&
				types.Identical(existing.TypeExpr, req.TypeExpr) {
				return false
			}
		}
		c.info.FuncCloneReqs[fn] = append(c.info.FuncCloneReqs[fn], req)
		return true
	}
	if method != nil {
		for _, existing := range c.info.MethodCloneReqs[method] {
			if existing.OpDesc == req.OpDesc && existing.Pos == req.Pos &&
				types.Identical(existing.TypeExpr, req.TypeExpr) {
				return false
			}
		}
		c.info.MethodCloneReqs[method] = append(c.info.MethodCloneReqs[method], req)
		return true
	}
	return false
}

// typeToTypeRef converts a types.Type to an ast.TypeRef for use in synthesized AST.
// Handles common cases needed for clone method synthesis.
func typeToTypeRef(typ types.Type) ast.TypeRef {
	switch t := typ.(type) {
	case *types.Named:
		return &ast.NamedTypeRef{Name: t.Obj().Name()}
	case *types.Enum:
		return &ast.NamedTypeRef{Name: t.Obj().Name()}
	case *types.Optional:
		return &ast.OptionalTypeRef{Inner: typeToTypeRef(t.Elem())}
	case *types.Instance:
		var typeArgs []ast.TypeRef
		for _, ta := range t.TypeArgs() {
			typeArgs = append(typeArgs, typeToTypeRef(ta))
		}
		switch origin := t.Origin().(type) {
		case *types.Named:
			return &ast.NamedTypeRef{Name: origin.Obj().Name(), TypeArgs: typeArgs}
		case *types.Enum:
			return &ast.NamedTypeRef{Name: origin.Obj().Name(), TypeArgs: typeArgs}
		}
		return &ast.NamedTypeRef{Name: "any"}
	case *types.TypeParam:
		return &ast.NamedTypeRef{Name: t.Obj().Name()}
	default:
		return &ast.NamedTypeRef{Name: typ.String()}
	}
}

// synthesizeCloneMethod builds an AST MethodDecl for the clone() Self method.
// The method body constructs a new instance by passing each field through:
// - Copy fields: passed directly (constructor handles bitwise copy)
// - Non-copy fields with clone(): this.field.clone()
// - Optional non-copy fields: typed var + if-let unwrap + clone + reassign
func (c *Checker) synthesizeCloneMethod(named *types.Named, _ *ast.TypeDecl) *ast.MethodDecl {
	var stmts []ast.Stmt
	var args []*ast.Arg

	fields := named.AllFields()
	for _, f := range fields {
		fieldType := f.Type()

		// T0605: fields whose declared type contains a TypeParam cannot be
		// classified copy/non-copy at synth time (isCopyField(TypeParam) is
		// optimistically true, which would emit a bare shallow read and alias
		// the heap value → double-free at mono codegen). Defer the decision to
		// codegen via the synth-only AutoCloneExpr intrinsic, which lowers
		// type-directed once the concrete substitution is known. Concrete
		// fields keep their exact existing behavior (zero regression surface).
		if types.ContainsTypeParam(fieldType) {
			args = append(args, &ast.Arg{
				Name:  f.Name(),
				Value: &ast.AutoCloneExpr{Expr: memberExpr(&ast.ThisExpr{}, f.Name())},
			})
			continue
		}

		// Check if the field type is Optional wrapping a non-copy type
		if opt, isOpt := fieldType.(*types.Optional); isOpt && !isCopyField(opt.Elem()) {
			// Generate:
			//   T? _clone_fieldname = none;
			//   if _v := this.fieldname { _clone_fieldname = _v.clone(); }
			// Then pass _clone_fieldname in the constructor args.
			localName := "_clone_" + f.Name()

			// T? _clone_fieldname = none;
			stmts = append(stmts, &ast.TypedVarDecl{
				Type:  typeToTypeRef(opt),
				Name:  localName,
				Value: &ast.NoneLit{},
			})

			// if _v := this.fieldname { _clone_fieldname = _v.clone(); }
			stmts = append(stmts, &ast.IfStmt{
				Binding: "_v",
				Init:    memberExpr(&ast.ThisExpr{}, f.Name()),
				Body: &ast.Block{
					Stmts: []ast.Stmt{
						&ast.AssignStmt{
							Target: ident(localName),
							Op:     ast.OpAssign,
							Value:  callMember(ident("_v"), "clone"),
						},
					},
				},
			})

			args = append(args, &ast.Arg{
				Name:  f.Name(),
				Value: ident(localName),
			})
			continue
		}

		// For copy fields: pass this.field directly
		if isCopyField(fieldType) {
			args = append(args, &ast.Arg{
				Name:  f.Name(),
				Value: memberExpr(&ast.ThisExpr{}, f.Name()),
			})
			continue
		}

		// For non-copy fields with clone(): this.field.clone()
		args = append(args, &ast.Arg{
			Name:  f.Name(),
			Value: callMember(memberExpr(&ast.ThisExpr{}, f.Name()), "clone"),
		})
	}

	// return Self(field1: ..., field2: ..., ...);
	// Use "Self" instead of d.Name so generic types resolve correctly (e.g., Box[T] → Self).
	stmts = append(stmts, &ast.ReturnStmt{
		Value: &ast.CallExpr{
			Callee: ident("Self"),
			Args:   args,
		},
	})

	return &ast.MethodDecl{
		Name:       "clone",
		Receiver:   &ast.ReceiverParam{RefMod: ast.RefNone},
		ReturnType: &ast.ReturnTypeSpec{Type: &ast.NamedTypeRef{Name: "Self"}},
		Annotations: []*ast.MetaAnnotation{
			{Name: "public"},
		},
		Body: &ast.Block{Stmts: stmts},
	}
}

// synthesizeEnumCloneMethod builds an AST MethodDecl for clone() EnumName on an enum.
// Self doesn't resolve in enum method context, so the return type uses the concrete name.
// Generates a match over all variants, cloning each variant's fields:
//
//	clone() EnumName `public {
//	    match this {
//	        EnumName.Variant1 => { return EnumName.Variant1; },
//	        EnumName.Variant2(a, b) => {
//	            T _c_a = a.clone(); // B0278: explicit local to avoid codegen crash
//	            return EnumName.Variant2(a: _c_a, b: b);
//	        },
//	    }
//	}
func (c *Checker) synthesizeEnumCloneMethod(enum *types.Enum, d *ast.EnumDecl) *ast.MethodDecl {
	enumName := d.Name

	var arms []*ast.MatchArm
	for _, v := range enum.Variants() {
		if v.NumFields() == 0 {
			// Fieldless: Enum.Variant => { return Enum.Variant; }
			arms = append(arms, &ast.MatchArm{
				Pattern: &ast.EnumVariantMatchPattern{Enum: enumName, Variant: v.Name()},
				Block: &ast.Block{Stmts: []ast.Stmt{
					&ast.ReturnStmt{Value: memberExpr(ident(enumName), v.Name())},
				}},
			})
			continue
		}

		// Variant with fields — destructure and clone each field.
		var stmts []ast.Stmt
		bindings := make([]string, v.NumFields())
		var args []*ast.Arg

		for i, f := range v.Fields() {
			bindName := "_v_" + f.Name()
			if f.Name() == "" {
				bindName = fmt.Sprintf("_v_%d", i)
			}
			bindings[i] = bindName
			fieldType := f.Type()

			// T0607: a variant field whose declared type contains a TypeParam
			// can't be classified copy/non-copy at synth time —
			// isCopyField(TypeParam) and isCopyField(Optional[TypeParam])/
			// Array[TypeParam] are optimistically true, so a `T?`/`[N]T`/`T`
			// field would be a bare shallow alias and double-freed at mono
			// codegen for a droppable TypeArg. Defer to codegen via the synth-
			// only AutoCloneExpr intrinsic (mirrors synthesizeCloneMethod /
			// T0605); it lowers type-directed once the concrete substitution is
			// known. Concrete fields keep their exact existing path (zero
			// regression surface).
			if types.ContainsTypeParam(fieldType) {
				args = append(args, &ast.Arg{
					Name:  f.Name(),
					Value: &ast.AutoCloneExpr{Expr: ident(bindName)},
				})
				continue
			}

			// Optional non-copy: if-let unwrap + clone
			if opt, isOpt := fieldType.(*types.Optional); isOpt && !isCopyField(opt.Elem()) {
				localName := "_clone_" + bindName
				stmts = append(stmts, &ast.TypedVarDecl{
					Type:  typeToTypeRef(opt),
					Name:  localName,
					Value: &ast.NoneLit{},
				})
				stmts = append(stmts, &ast.IfStmt{
					Binding: "_u",
					Init:    ident(bindName),
					Body: &ast.Block{
						Stmts: []ast.Stmt{
							&ast.AssignStmt{
								Target: ident(localName),
								Op:     ast.OpAssign,
								Value:  callMember(ident("_u"), "clone"),
							},
						},
					},
				})
				args = append(args, &ast.Arg{Name: f.Name(), Value: ident(localName)})
				continue
			}

			// Copy: pass directly
			if isCopyField(fieldType) {
				args = append(args, &ast.Arg{Name: f.Name(), Value: ident(bindName)})
				continue
			}

			// Non-copy: clone into local var (B0278: inline method call in enum ctor
			// arg inside match arm block causes segfault, so use explicit local).
			localName := "_c_" + bindName
			stmts = append(stmts, &ast.TypedVarDecl{
				Type:  typeToTypeRef(fieldType),
				Name:  localName,
				Value: callMember(ident(bindName), "clone"),
			})
			args = append(args, &ast.Arg{Name: f.Name(), Value: ident(localName)})
		}

		stmts = append(stmts, &ast.ReturnStmt{
			Value: &ast.CallExpr{
				Callee: memberExpr(ident(enumName), v.Name()),
				Args:   args,
			},
		})

		arms = append(arms, &ast.MatchArm{
			Pattern: &ast.EnumDestructureMatchPattern{Enum: enumName, Variant: v.Name(), Bindings: bindings},
			Block:   &ast.Block{Stmts: stmts},
		})
	}

	body := &ast.Block{Stmts: []ast.Stmt{
		makeExprStmt(&ast.MatchExpr{
			Subject: &ast.ThisExpr{},
			Arms:    arms,
		}),
	}}

	// Build return type: EnumName (or EnumName[T, U] for generic enums).
	// Self doesn't resolve in enum method context, so use the concrete name.
	var retType ast.TypeRef
	if len(enum.TypeParams()) > 0 {
		var typeArgs []ast.TypeRef
		for _, tp := range enum.TypeParams() {
			typeArgs = append(typeArgs, &ast.NamedTypeRef{Name: tp.Obj().Name()})
		}
		retType = &ast.NamedTypeRef{Name: enumName, TypeArgs: typeArgs}
	} else {
		retType = &ast.NamedTypeRef{Name: enumName}
	}

	return &ast.MethodDecl{
		Name:       "clone",
		Receiver:   &ast.ReceiverParam{RefMod: ast.RefNone},
		ReturnType: &ast.ReturnTypeSpec{Type: retType},
		Annotations: []*ast.MetaAnnotation{
			{Name: "public"},
		},
		Body: body,
	}
}
