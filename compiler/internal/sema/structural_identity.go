package sema

import (
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// Structural conformance is a STATIC property, so `is` and `as` against a
// `` `structural `` interface are compile-time questions (language-design.md#is-and-as-against-a-structural-interface).
//
// They used to be runtime ones, and both answered wrongly. `is` walked the RTTI
// parent chain, which records only nominal `is` parents, so `s is Sink` was false
// even when `s` was declared `Sink` (T1532). `as` ran the same walk and then handed
// back the subject's own representation without boxing, so a value-type subject
// panicked codegen and a heap subject yielded `none` (T1562) — while `promise check`
// accepted both.
//
// Teaching RTTI about structural conformance would not have fixed either: structural
// satisfaction is open-world, and crossing INTO a view needs the concrete's
// view-specific vtable, which is only known statically. So the answer is computed
// where it is actually decidable, and a subject whose static type does not satisfy
// the interface is rejected rather than silently answered.

// StructuralTarget reports whether typ is a `structural` interface that values are
// boxed into — a fat `{vtable, instance}` view. A `structural` type whose fields are
// all `value` is NOT one: it stays register-resident as its flat value struct and
// cannot be satisfied structurally at all (language-design.md#the-four-struct-model, T1550), so `is`
// and `as` against it keep their ordinary value-type treatment.
//
// Exported so codegen answers the deferred generic case with this same predicate
// rather than a second copy of it.
func StructuralTarget(typ types.Type) *types.Named {
	named := semaExtractNamed(unwrapOptional(typ))
	if named == nil || !named.IsStructural() || named.IsValueType() || !named.IsAbstract() {
		return nil
	}
	return named
}

// StructuralConformance answers whether a value of subject type satisfies the
// structural interface target.
//
// decided is false when the subject's type still contains an unbound type parameter:
// inside a generic body sema checks once with the parameters unbound, so the honest
// answer is "not yet" — codegen folds it per instantiation, where the concrete type
// is known. Callers must neither accept nor reject an undecided case.
func StructuralConformance(subject, target types.Type) (satisfies bool, decided bool) {
	targetNamed := StructuralTarget(target)
	if targetNamed == nil {
		return false, false
	}
	subject = unwrapOptional(subject)
	if subject == nil || types.ContainsTypeParam(subject) {
		return false, false
	}
	// A subject already typed as the interface trivially satisfies it — including the
	// case where it is a *different* instantiation's origin, which Implements would
	// answer by signature rather than by identity.
	if semaExtractNamed(subject) == targetNamed {
		return true, true
	}
	if inst, ok := unwrapOptional(target).(*types.Instance); ok {
		return types.ImplementsInst(subject, inst), true
	}
	return types.Implements(subject, targetNamed), true
}

// StructuralIsInfo is what codegen needs to emit an `is` against a structural
// interface without an RTTI walk. When Decided, Result is the answer; otherwise the
// subject's type still had unbound type parameters and codegen re-asks
// StructuralConformance once the substitution is known.
type StructuralIsInfo struct {
	Subject types.Type
	Target  types.Type
	Result  bool
	Decided bool
}

// recordStructuralIs handles `subject is Target` when Target is a structural
// interface, and reports whether it did. A subject that cannot satisfy the interface
// is an error: the check would be a constant false, and a runtime one is not
// representable either, since crossing into a view needs the concrete's view-specific
// vtable and there is no runtime concrete → vtable lookup.
func (c *Checker) recordStructuralIs(e ast.Expr, subject, target types.Type) bool {
	if StructuralTarget(target) == nil {
		return false
	}
	satisfies, decided := StructuralConformance(subject, target)
	if decided && !satisfies {
		c.errorf(e.Pos(), "%s does not satisfy the `structural interface %s, so this check is never true; conformance to a `structural interface is a compile-time property (assign to %s directly to widen, or test the concrete type)",
			subject, target, target)
		return true
	}
	c.info.StructuralIs[e] = &StructuralIsInfo{
		Subject: subject,
		Target:  target,
		Result:  satisfies,
		Decided: decided,
	}
	return true
}

// rejectUnrepresentableStructuralUpcast reports `expr as Target` / `as! Target` where
// Target is a structural interface the subject's static type does not satisfy.
//
// A cast TO a structural interface is a widening, resolved on the same terms as `is`:
// accepted when the static type satisfies the interface, in which case it performs
// exactly the boxing an implicit assignment performs. The other direction — out of an
// interface to a concrete type — is unaffected and stays a runtime-checked cast.
func (c *Checker) rejectUnrepresentableStructuralUpcast(pos ast.Pos, subject, target types.Type) bool {
	if StructuralTarget(target) == nil {
		return false
	}
	// Casting OUT of a structural interface (downcast) is the other direction.
	if StructuralTarget(subject) != nil {
		return false
	}
	satisfies, decided := StructuralConformance(subject, target)
	if !decided || satisfies {
		return false
	}
	c.errorf(pos, "cannot cast %s to the `structural interface %s: %s does not satisfy it, and conformance to a `structural interface is a compile-time property (a value that satisfies %s widens to it by assignment, with no cast)",
		subject, target, subject, target)
	return true
}
