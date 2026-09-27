package sema

import (
	"testing"
)

// T1532 / T1562: conformance to a `structural interface is a compile-time property,
// so `is` and `as` against one are answered by sema rather than by an RTTI walk that
// records only nominal `is` parents. A subject whose static type cannot satisfy the
// interface is rejected, because the check would be a constant false and there is no
// runtime concrete → view-vtable lookup to fall back on.

const structuralIdentitySrc = "type Sink `structural {\n" +
	"  emit(this, int x) int `abstract;\n" +
	"}\n" +
	"type Counter {\n" +
	"  int base `value;\n" +
	"  emit(this, int x) int => this.base + x;\n" +
	"}\n" +
	"type Plain {\n" +
	"  int k;\n" +
	"}\n"

func TestStructuralIsOnDeclaredInterfaceIsAccepted(t *testing.T) {
	errs := checkErrs(t, structuralIdentitySrc+
		"main() {\n"+
		"  Sink s = Counter(base: 5);\n"+
		"  bool ok = s is Sink;\n"+
		"}\n")
	expectNoErrors(t, errs)
}

func TestStructuralIsOnSatisfyingConcreteIsAccepted(t *testing.T) {
	errs := checkErrs(t, structuralIdentitySrc+
		"main() {\n"+
		"  Counter c = Counter(base: 5);\n"+
		"  bool ok = c is Sink;\n"+
		"}\n")
	expectNoErrors(t, errs)
}

func TestStructuralIsOnOptionalSubjectIsAccepted(t *testing.T) {
	errs := checkErrs(t, structuralIdentitySrc+
		"main() {\n"+
		"  Sink? a = Counter(base: 5);\n"+
		"  bool ok = a is Sink;\n"+
		"}\n")
	expectNoErrors(t, errs)
}

func TestStructuralIsOnNonSatisfyingSubjectIsRejected(t *testing.T) {
	errs := checkErrs(t, structuralIdentitySrc+
		"main() {\n"+
		"  Plain p = Plain(k: 1);\n"+
		"  bool ok = p is Sink;\n"+
		"}\n")
	expectError(t, errs, "does not satisfy the `structural interface Sink")
	expectError(t, errs, "compile-time property")
}

func TestStructuralCastToSatisfiedInterfaceIsAccepted(t *testing.T) {
	errs := checkErrs(t, structuralIdentitySrc+
		"main() {\n"+
		"  Counter c = Counter(base: 5);\n"+
		"  Sink s = c as! Sink;\n"+
		"  Sink? o = c as Sink;\n"+
		"}\n")
	expectNoErrors(t, errs)
}

func TestStructuralCastFromNonSatisfyingSubjectIsRejected(t *testing.T) {
	errs := checkErrs(t, structuralIdentitySrc+
		"main() {\n"+
		"  Plain p = Plain(k: 1);\n"+
		"  Sink s = p as! Sink;\n"+
		"}\n")
	expectError(t, errs, "cannot cast Plain to the `structural interface Sink")
}

// The other direction — out of a structural interface to a concrete type — is a
// genuine runtime downcast and must stay accepted.
func TestStructuralDowncastIsUnaffected(t *testing.T) {
	errs := checkErrs(t, structuralIdentitySrc+
		"main() {\n"+
		"  Sink s = Counter(base: 5);\n"+
		"  Counter? c = s as Counter;\n"+
		"}\n")
	expectNoErrors(t, errs)
}

// A generic body is checked once with its type parameters unbound, so the answer is
// deferred rather than rejected — codegen folds it per instantiation.
func TestStructuralIsInGenericBodyIsDeferredNotRejected(t *testing.T) {
	errs := checkErrs(t, structuralIdentitySrc+
		"probe[T](T v) bool {\n"+
		"  return v is Sink;\n"+
		"}\n"+
		"main() {\n"+
		"  bool ok = probe[Counter](Counter(base: 1));\n"+
		"}\n")
	expectNoErrors(t, errs)
}

// A `structural type whose fields are all `value is NOT an interface view (language-design.md#the-four-struct-model,
// T1550) — it keeps the ordinary value-type treatment, including the existing
// value-type-identity rejection rather than the new structural one.
func TestStructuralValueTypeTargetKeepsValueTypeRules(t *testing.T) {
	errs := checkErrs(t, "type Metric `structural {\n"+
		"  int v `value;\n"+
		"}\n"+
		"type Other {\n"+
		"  int w `value;\n"+
		"}\n"+
		"main() {\n"+
		"  Other o = Other(w: 1);\n"+
		"  bool ok = o is Metric;\n"+
		"}\n")
	expectNoErrorContaining(t, errs, "compile-time property")
}
