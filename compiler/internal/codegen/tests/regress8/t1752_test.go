package regress8

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1752 — `_validate!`, a type invariant every construction path must satisfy
// (docs/language-design.md#constructors → Validation).
//
// The chain is emitted at the construction expression, where the concrete type
// is exactly known, so every call is static — nothing dispatches through a
// vtable or typeinfo. These tests pin the properties that are NOT observable at
// runtime and so cannot be covered by tests/e2e/validate_invariant_test.pr:
// how many calls are emitted, and in which order.

const t1752Port = `
	type Port {
	  int value ` + "`final" + `;
	  _validate!(this) {
	    if this.value < 1 { raise error("bad port"); }
	  }
	}
`

// A construction at a receiving site emits exactly one call to the chain.
func TestT1752ConstructionEmitsOneValidateCall(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1752Port+`
		main() {
		  p := Port(value: 80)?!;
		}
	`)
	if got := strings.Count(mainBody(t, ir), "call { i1, i8* } @Port._validate"); got != 1 {
		t.Fatalf("expected exactly 1 @Port._validate call in main, got %d\n%s", got, mainBody(t, ir))
	}
}

// A nested factory result is validated exactly once — by the INNER factory. The
// outer factory must not re-validate what it merely passes through, and the
// call site must not validate a factory result at all.
func TestT1752NestedFactoryValidatesOnce(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counted {
		  int n `+"`final"+`;
		  _validate!(this) {
		    if this.n < 0 { raise error("negative"); }
		  }
		  inner!(int n) Self `+"`factory"+` { return Self(n: n); }
		  outer!(int n) Self `+"`factory"+` { return Self.inner(n); }
		}
		main() {
		  c := Counted.outer(5)?!;
		}
	`)
	// The inner factory constructs and validates.
	if got := strings.Count(funcBody(t, ir, "@Counted.inner"), "@Counted._validate"); got != 1 {
		t.Errorf("inner factory should validate once, got %d", got)
	}
	// The outer factory only forwards — nothing to validate.
	if got := strings.Count(funcBody(t, ir, "@Counted.outer"), "@Counted._validate"); got != 0 {
		t.Errorf("outer factory must not re-validate a Self from another factory, got %d", got)
	}
	// A factory CALL site never validates.
	if got := strings.Count(mainBody(t, ir), "@Counted._validate"); got != 0 {
		t.Errorf("a factory call site must not validate, got %d", got)
	}
}

// A `Self` built inside its own factory defers to the factory's `return`, so
// the post-construction `final fixups complete before the chain observes
// anything. The call must therefore follow the field stores, not precede them.
func TestT1752FactoryDefersToReturn(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Gauge {
		  int reading `+"`final"+`;
		  _validate!(this) {
		    if this.reading < 0 { raise error("negative"); }
		  }
		  sample!(int v) Self `+"`factory"+` {
		    g := Self(reading: -1);
		    g.reading = v;
		    return g;
		  }
		}
		main() {
		  g := Gauge.sample(7)?!;
		}
	`)
	body := funcBody(t, ir, "@Gauge.sample")
	if got := strings.Count(body, "@Gauge._validate"); got != 1 {
		t.Fatalf("factory should validate exactly once at its return, got %d\n%s", got, body)
	}
	// The `final fixup is a store into the instance's field slot; it must come
	// first. `getelementptr ... i32 1` is the reading field (0 is _variant).
	fixup := strings.Index(body, "%promise_Gauge_i* %")
	call := strings.Index(body, "@Gauge._validate")
	if fixup < 0 || call < 0 || fixup > call {
		t.Errorf("the deferred validate must follow the `final fixup (fixup=%d call=%d)\n%s", fixup, call, body)
	}
}

// A parent's invariant runs before its child's, and both see the complete
// instance — so both calls take the SAME instance pointer, parent first.
func TestT1752InheritedChainIsParentFirst(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base {
		  int a `+"`final"+`;
		  _validate!(this) { if this.a < 0 { raise error("a"); } }
		}
		type Derived is Base {
		  int b `+"`final"+`;
		  _validate!(this) { if this.b < 0 { raise error("b"); } }
		}
		main() {
		  d := Derived(a: 1, b: 2)?!;
		}
	`)
	body := mainBody(t, ir)
	base := strings.Index(body, "@Base._validate")
	derived := strings.Index(body, "@Derived._validate")
	if base < 0 || derived < 0 {
		t.Fatalf("expected both links in the chain\n%s", body)
	}
	if base > derived {
		t.Errorf("the parent's _validate! must run first (base=%d derived=%d)\n%s", base, derived, body)
	}
}

// language-design.md#constructors: clone() does not validate — a clone is an identical copy of an instance
// that was already valid.
func TestT1752CloneDoesNotValidate(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Document `+"`clone"+` {
		  string title;
		  int pages `+"`final"+`;
		  _validate!(this) { if this.pages < 1 { raise error("no pages"); } }
		}
		main() {
		  d := Document(title: "t", pages: 2)?!;
		  c := d.clone();
		}
	`)
	if got := strings.Count(funcBody(t, ir, "@Document.clone"), "@Document._validate"); got != 0 {
		t.Errorf("clone() must not validate, got %d calls", got)
	}
}

// A monomorphized generic names its own instance's invariant, not the generic
// origin's — monoName's bracket form, the same mangling every other mono
// method uses.
func TestT1752GenericValidateIsMonomorphized(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Boxed[T] {
		  T item;
		  int count `+"`final"+`;
		  _validate!(this) { if this.count < 1 { raise error("count"); } }
		}
		main() {
		  b := Boxed[int](item: 7, count: 2)?!;
		}
	`)
	codegentest.AssertContains(t, mainBody(t, ir), `@"Boxed[int]._validate"`)
}

// A GENERIC child inheriting a GENERIC validated parent. Each link must be
// named against the instance it belongs to — `Base[string]._validate`, not the
// generic origin's `Base._validate`, which has no body. Getting the parent's
// type-argument resolution wrong here panics ("undeclared … (T1752)") or calls
// the wrong instance.
func TestT1752GenericParentChainIsNamedPerInstance(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base[T] {
		  T tag;
		  int lo `+"`final"+`;
		  _validate!(this) { if this.lo < 0 { raise error("lo"); } }
		}
		type Child[T] is Base[T] {
		  int hi `+"`final"+`;
		  _validate!(this) { if this.hi < this.lo { raise error("hi"); } }
		}
		main() {
		  c := Child[string](tag: "x", lo: 1, hi: 5)?!;
		}
	`)
	body := mainBody(t, ir)
	base := strings.Index(body, `@"Base[string]._validate"`)
	child := strings.Index(body, `@"Child[string]._validate"`)
	if base < 0 || child < 0 {
		t.Fatalf("expected both monomorphized links (base=%d child=%d)\n%s", base, child, body)
	}
	if base > child {
		t.Errorf("the parent's _validate! must run first (base=%d child=%d)", base, child)
	}
	// The generic ORIGIN has no body; naming it would be an undefined symbol.
	if strings.Contains(body, "@Base._validate") {
		t.Error("the chain must not name the generic origin @Base._validate")
	}
}

// An enum's invariant runs at a payload-carrying variant construction, through
// the enum-specific naming path (validateChainNamesForEnum / resolveEnumTypeName).
func TestT1752EnumVariantConstructionEmitsTheChain(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Temp {
		  Kelvin(int degrees),
		  Unspecified,
		  _validate!(this) {
		    match this {
		      Temp.Kelvin(d) => { if d < 0 { raise error("k"); } },
		      _ => {},
		    }
		  }
		}
		main() {
		  t := Temp.Kelvin(300)?!;
		}
	`)
	if got := strings.Count(mainBody(t, ir), "@Temp._validate"); got != 1 {
		t.Errorf("a payload variant construction should validate once, got %d", got)
	}
}

// A fieldless variant is a plain enumerated value, not a construction
// expression — it emits no chain at all.
func TestT1752FieldlessVariantEmitsNoChain(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Temp {
		  Kelvin(int degrees),
		  Unspecified,
		  _validate!(this) {
		    match this {
		      Temp.Kelvin(d) => { if d < 0 { raise error("k"); } },
		      _ => {},
		    }
		  }
		}
		main() {
		  t := Temp.Unspecified;
		}
	`)
	if got := strings.Count(mainBody(t, ir), "@Temp._validate"); got != 0 {
		t.Errorf("a fieldless variant must emit no chain, got %d calls", got)
	}
}

// language-design.md#constructors grants no value-type exemption. A value type has no instance pointer, so
// the receiver is a pointer to its value struct (valueStructRecvPtr).
func TestT1752ValueTypeConstructionEmitsTheChain(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Ratio {
		  int num `+"`value"+`;
		  int den `+"`value"+`;
		  _validate!(this) { if this.den == 0 { raise error("zero"); } }
		}
		main() {
		  r := Ratio(num: 1, den: 2)?!;
		}
	`)
	if got := strings.Count(mainBody(t, ir), "@Ratio._validate"); got != 1 {
		t.Errorf("a value type's construction should validate once, got %d", got)
	}
}

// The deferred-return path on a VALUE type: no instance pointer exists, so the
// chain's receiver is a pointer to the value struct.
func TestT1752ValueTypeFactoryDefersToReturn(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Ratio {
		  int num `+"`value`"+` `+"`final"+`;
		  int den `+"`value`"+` `+"`final"+`;
		  _validate!(this) { if this.den == 0 { raise error("zero"); } }
		  scaled!(int n, int d) Self `+"`factory"+` {
		    r := Self(num: 0, den: 0);
		    r.num = n;
		    r.den = d;
		    return r;
		  }
		}
		main() {
		  r := Ratio.scaled(3, 4)?!;
		}
	`)
	body := funcBody(t, ir, "@Ratio.scaled")
	if got := strings.Count(body, "@Ratio._validate"); got != 1 {
		t.Fatalf("the value-type factory should validate once at its return, got %d\n%s", got, body)
	}
	// The call site must not re-validate what the factory already did.
	if got := strings.Count(mainBody(t, ir), "@Ratio._validate"); got != 0 {
		t.Errorf("a factory call site must not validate, got %d", got)
	}
}

// mainBody returns the body of the user's main — which codegen emits as the
// `@.goroutine.main` coroutine, not `@main` (the C entry point that starts the
// scheduler). Scoping to it keeps a call count from being confused by calls
// inside the type's own methods.
func mainBody(t *testing.T, ir string) string {
	t.Helper()
	return funcBody(t, ir, "@.goroutine.main(")
}

// funcBody returns the text of the first LLVM function whose define line
// contains marker, up to its closing brace.
func funcBody(t *testing.T, ir, marker string) string {
	t.Helper()
	for _, chunk := range strings.Split(ir, "\ndefine ") {
		head := chunk
		if i := strings.IndexByte(chunk, '\n'); i >= 0 {
			head = chunk[:i]
		}
		if !strings.Contains(head, marker) {
			continue
		}
		if end := strings.Index(chunk, "\n}"); end >= 0 {
			return chunk[:end]
		}
		return chunk
	}
	t.Fatalf("no function matching %q in IR", marker)
	return ""
}
