package sema

import (
	"testing"
)

// --- T1731: structural(protocol: true) annotation tests ---

func TestProtocolAnnotationOnNonAbstractType(t *testing.T) {
	errs := checkErrs(t, `
		type Foo `+"`"+`structural(protocol: true) {
			greet(this) string { return "hi"; }
		}
		main() {}
	`)
	expectError(t, errs, "`structural(protocol: true) requires at least one `abstract method")
}

func TestProtocolAnnotationOnAbstractType(t *testing.T) {
	checkOK(t, `
		type Foo `+"`"+`structural(protocol: true) {
			greet(this) string `+"`"+`abstract;
		}
		main() {}
	`)
}

func TestProtocolAnnotationOnMethod(t *testing.T) {
	errs := checkErrs(t, `
		type Bar {
			x(this) int `+"`"+`structural(protocol: true) { return 1; }
		}
		main() {}
	`)
	expectError(t, errs, "`structural(protocol: true) can only be applied to a type, not a method")
}

func TestProtocolAnnotationOnEnum(t *testing.T) {
	errs := checkErrs(t, `
		enum Baz `+"`"+`structural(protocol: true) { A, B }
		main() {}
	`)
	expectError(t, errs, "`structural(protocol: true) can only be applied to a type, not an enum")
}

func TestProtocolOptOutOnMethod(t *testing.T) {
	// protocol: false on a method should be accepted without error
	checkOK(t, `
		type Foo {
			greet(this) string `+"`"+`structural(protocol: false) { return "hi"; }
		}
		main() {}
	`)
}

// --- T1731: protocol near-miss check tests ---

func TestProtocolNearMissNameHitNotSatisfied(t *testing.T) {
	// A type with a method name matching a protocol but wrong signature → error
	errs := checkErrsWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			greet!(this) string `+"`"+`abstract;
		}`,
		`type Foo {
			greet(this) int { return 1; }
		}
		main() {}
		`)
	expectError(t, errs, "matching protocol Proto")
}

func TestProtocolNearMissNameHitSatisfied(t *testing.T) {
	// A type with a method that satisfies the protocol → no error
	checkOKWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			greet(this) string `+"`"+`abstract;
		}`,
		`type Foo {
			greet(this) string { return "hi"; }
		}
		main() {}
		`)
}

func TestProtocolNearMissExplainedByOtherInterface(t *testing.T) {
	// A type with a method name matching a protocol but satisfying a different
	// interface with the same method name → no error (clause 3)
	checkOKWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			greet!(this) string `+"`"+`abstract;
		}
		type LocalIface {
			greet(this) int `+"`"+`abstract;
		}`,
		`type Foo is LocalIface {
			greet(this) int { return 42; }
		}
		main() {}
		`)
}

func TestProtocolNearMissOptOut(t *testing.T) {
	// A method opted out with structural(protocol: false) → no error
	checkOKWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			greet!(this) string `+"`"+`abstract;
		}`,
		`type Foo {
			greet(this) int `+"`"+`structural(protocol: false) { return 1; }
		}
		main() {}
		`)
}

func TestProtocolNearMissTypeOptOut(t *testing.T) {
	// A type opted out with structural(protocol: false) → no error on any method
	checkOKWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			greet!(this) string `+"`"+`abstract;
		}`,
		`type Foo `+"`"+`structural(protocol: false) {
			greet(this) int { return 1; }
		}
		main() {}
		`)
}

func TestProtocolNearMissRelaxedMatchPasses(t *testing.T) {
	// Relaxed matching: extra defaulted params, non-failable for failable → satisfies
	checkOKWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			greet!(this) string `+"`"+`abstract;
		}`,
		`type Foo {
			greet(this, string prefix = "hi") string { return prefix; }
		}
		main() {}
		`)
}

func TestProtocolNearMissAbstractTypeSkipped(t *testing.T) {
	// Abstract types are not checked (they declare contracts, not implementations)
	checkOKWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			greet!(this) string `+"`"+`abstract;
		}`,
		`type MyIface {
			greet(this) int `+"`"+`abstract;
		}
		main() {}
		`)
}

func TestProtocolNearMissEnumOptOut(t *testing.T) {
	// Enum opted out with structural(protocol: false) → no error
	checkOKWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			greet!(this) string `+"`"+`abstract;
		}`,
		`enum MyEnum {
			A, B,
			greet(this) int `+"`"+`structural(protocol: false) { return 1; }
		}
		main() {}
		`)
}

// --- T1731: enum near-miss error path ---

func TestProtocolNearMissEnumError(t *testing.T) {
	// Enum with method name matching protocol but wrong signature → error
	errs := checkErrsWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			greet(this) string `+"`"+`abstract;
		}`,
		`enum MyEnum {
			A, B,
			greet(this) int {
				return 1;
			}
		}
		main() {}
		`)
	expectError(t, errs, "matching protocol Proto")
}

func TestProtocolNearMissEnumSatisfied(t *testing.T) {
	// Enum with method that satisfies the protocol → no error
	checkOKWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			greet(this) string `+"`"+`abstract;
		}`,
		`enum MyEnum {
			A, B,
			greet(this) string {
				return "hello";
			}
		}
		main() {}
		`)
}

func TestProtocolNearMissEnumExplainedByOtherInterface(t *testing.T) {
	// Enum with method name matching protocol but satisfying a different
	// interface → no error (clause 3)
	checkOKWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			greet(this) string `+"`"+`abstract;
		}
		type LocalIface {
			greet(this) int `+"`"+`abstract;
		}`,
		`enum MyEnum {
			A, B,
			greet(this) int {
				return 42;
			}
		}
		main() {}
		`)
}

// --- T1731: generic protocol near-miss ---

func TestProtocolNearMissGenericSatisfied(t *testing.T) {
	// Type satisfies a generic protocol → no error
	checkOKWithStd(t,
		`type Proto[T] `+"`"+`structural(protocol: true) {
			process(this, T item) T `+"`"+`abstract;
		}`,
		`type Doubler {
			process(this, int item) int { return item * 2; }
		}
		main() {}
		`)
}

func TestProtocolNearMissGenericNotSatisfied(t *testing.T) {
	// Type has method name matching generic protocol but extra required param → error
	errs := checkErrsWithStd(t,
		`type Proto[T] `+"`"+`structural(protocol: true) {
			process(this, T item) T `+"`"+`abstract;
		}`,
		`type Bad {
			process(this, int a, int b) int { return a + b; }
		}
		main() {}
		`)
	expectError(t, errs, "matching protocol Proto")
}

// --- T1731: getter protocol method ---

func TestProtocolNearMissGetterSatisfied(t *testing.T) {
	// Type with a getter satisfying the protocol → no error
	checkOKWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			get name string `+"`"+`abstract;
		}`,
		`type Foo {
			get name string { return "foo"; }
		}
		main() {}
		`)
}

func TestProtocolNearMissGetterNotSatisfied(t *testing.T) {
	// Type with getter name matching protocol but wrong type → error
	errs := checkErrsWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			get name string `+"`"+`abstract;
		}`,
		`type Foo {
			get name int { return 1; }
		}
		main() {}
		`)
	expectError(t, errs, "matching protocol Proto")
}

// --- T1731: failable mismatch ---

func TestProtocolNearMissFailableConcrete(t *testing.T) {
	// Protocol declares non-failable; concrete is failable → error
	errs := checkErrsWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			greet(this) string `+"`"+`abstract;
		}`,
		`type Foo {
			greet!(this) string { return "hi"; }
		}
		main() {}
		`)
	expectError(t, errs, "matching protocol Proto")
}

func TestProtocolNearMissNonFailableForFailable(t *testing.T) {
	// Protocol declares failable; concrete is non-failable → satisfies (relaxed)
	checkOKWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			greet!(this) string `+"`"+`abstract;
		}`,
		`type Foo {
			greet(this) string { return "hi"; }
		}
		main() {}
		`)
}

// --- T1731: inherited protocol methods ---

func TestProtocolNearMissInheritedAbstractMethod(t *testing.T) {
	// Protocol inherits abstract method from parent; near-miss fires on inherited name
	errs := checkErrsWithStd(t,
		`type Base `+"`"+`structural {
			process(this) string `+"`"+`abstract;
		}
		type Proto is Base `+"`"+`structural(protocol: true) {
			transform(this) string `+"`"+`abstract;
		}`,
		`type Foo {
			process(this) int { return 1; }
		}
		main() {}
		`)
	expectError(t, errs, "matching protocol")
}

// --- T1731: structural interface type is skipped ---

func TestProtocolNearMissStructuralTypeSkipped(t *testing.T) {
	// Structural interface types should not be checked (they declare contracts)
	checkOKWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			greet(this) string `+"`"+`abstract;
		}`,
		`type MyStruct `+"`"+`structural {
			greet(this) int `+"`"+`abstract;
		}
		main() {}
		`)
}

// --- T1731: protocol: true on enum method (error) ---

func TestProtocolAnnotationOnEnumMethod(t *testing.T) {
	errs := checkErrs(t, `
		enum Baz {
			A, B,
			greet(this) string `+"`"+`structural(protocol: true) { return "hi"; }
		}
		main() {}
	`)
	expectError(t, errs, "`structural(protocol: true) can only be applied to a type, not a method")
}

// --- T1731: enum type-level opt-out ---

func TestProtocolNearMissEnumTypeOptOut(t *testing.T) {
	// Entire enum opted out with structural(protocol: false) → no error
	checkOKWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			greet(this) string `+"`"+`abstract;
		}`,
		`enum MyEnum `+"`"+`structural(protocol: false) {
			A, B,
			greet(this) int {
				return 1;
			}
		}
		main() {}
		`)
}

// --- T1731: return type T? relaxation ---

func TestProtocolNearMissReturnOptionalRelaxation(t *testing.T) {
	// Protocol declares T? return; concrete returns T → satisfies
	checkOKWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			find(this) string? `+"`"+`abstract;
		}`,
		`type Foo {
			find(this) string { return "found"; }
		}
		main() {}
		`)
}

// --- T1731: wrong param type ---

func TestProtocolNearMissWrongParamType(t *testing.T) {
	// Protocol requires string param; concrete takes int → error
	errs := checkErrsWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			process(this, string data) `+"`"+`abstract;
		}`,
		`type Foo {
			process(this, int data) { }
		}
		main() {}
		`)
	expectError(t, errs, "matching protocol Proto")
}

// --- T1731: void return type matching ---

func TestProtocolNearMissVoidReturnSatisfied(t *testing.T) {
	// Protocol declares void return; concrete also void → satisfies
	checkOKWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			run(this) `+"`"+`abstract;
		}`,
		`type Foo {
			run(this) { }
		}
		main() {}
		`)
}

func TestProtocolNearMissVoidVsNonVoidReturn(t *testing.T) {
	// Protocol declares void; concrete returns string → error
	errs := checkErrsWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			run(this) `+"`"+`abstract;
		}`,
		`type Foo {
			run(this) string { return "x"; }
		}
		main() {}
		`)
	expectError(t, errs, "matching protocol Proto")
}

func TestProtocolNearMissNonVoidVsVoidReturn(t *testing.T) {
	// Protocol declares string return; concrete is void → error
	errs := checkErrsWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			run(this) string `+"`"+`abstract;
		}`,
		`type Foo {
			run(this) { }
		}
		main() {}
		`)
	expectError(t, errs, "matching protocol Proto")
}

// --- T1731: fewer params than protocol ---

func TestProtocolNearMissFewerParams(t *testing.T) {
	// Protocol requires 2 params; concrete has 1 → near-miss but the method
	// doesn't match the protocol name since param count < abstract
	errs := checkErrsWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			process(this, int a, int b) int `+"`"+`abstract;
		}`,
		`type Foo {
			process(this) int { return 0; }
		}
		main() {}
		`)
	expectError(t, errs, "matching protocol Proto")
}

// --- T1731: extra non-defaulted param blocks satisfaction ---

func TestProtocolNearMissExtraRequiredParam(t *testing.T) {
	// Concrete has extra required param beyond protocol's → doesn't satisfy
	errs := checkErrsWithStd(t,
		`type Proto `+"`"+`structural(protocol: true) {
			process(this) int `+"`"+`abstract;
		}`,
		`type Foo {
			process(this, int extra) int { return extra; }
		}
		main() {}
		`)
	expectError(t, errs, "matching protocol Proto")
}

// --- T1731: multiple protocols, different methods ---

func TestProtocolNearMissMultipleProtocols(t *testing.T) {
	// Two protocols; type satisfies one but not the other → error on the unsatisfied one
	errs := checkErrsWithStd(t,
		`type ProtoA `+"`"+`structural(protocol: true) {
			greet(this) string `+"`"+`abstract;
		}
		type ProtoB `+"`"+`structural(protocol: true) {
			farewell(this) string `+"`"+`abstract;
		}`,
		`type Foo {
			greet(this) string { return "hi"; }
			farewell(this) int { return 0; }
		}
		main() {}
		`)
	expectError(t, errs, "matching protocol ProtoB")
	expectNoErrorContaining(t, errs, "matching protocol ProtoA")
}
