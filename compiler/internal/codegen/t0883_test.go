package codegen

import (
	"strings"
	"testing"
)

// T0883: a type may declare BOTH the prefix-unary (0-param) and binary (1-param)
// variant of the same operator symbol (e.g. negation `-()` and subtraction
// `-(Base o)`). Previously both methods mangled to `@Base.-`, so `opt` rejected
// the IR with "invalid redefinition of function 'Base.-'". The fix gives the
// unary variant a "$unary" discriminator in its IR name and vtable slot, and
// makes operator lookup arity-aware on both sema and codegen sides.

// (a) Both variants emit distinct function definitions — no redefinition.
func TestT0883_UnaryAndBinaryDistinctDefinitions(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			int v;
			-(Base o) Base { return Base(v: this.v - o.v); }
			-() Base { return Base(v: -this.v); }
		}
		main() { b := Base(v: 1); }
	`)
	if extractFunction(ir, "Base.-") == "" {
		t.Fatalf("expected binary @Base.- definition in IR:\n%s", ir)
	}
	if extractFunction(ir, "Base.-$unary") == "" {
		t.Fatalf("expected unary @Base.-$unary definition in IR:\n%s", ir)
	}
	// The two definitions must not collide: exactly one `define ... @Base.-(` and
	// exactly one `define ... @Base.-$unary(`.
	if n := strings.Count(ir, " @Base.-("); n != 1 {
		t.Fatalf("expected exactly one binary @Base.-( definition, got %d:\n%s", n, ir)
	}
	if n := strings.Count(ir, " @Base.-$unary("); n != 1 {
		t.Fatalf("expected exactly one unary @Base.-$unary( definition, got %d:\n%s", n, ir)
	}
}

// (b) Declaration order does not matter: unary declared first still produces
// both distinct definitions and dispatches each at the call site.
func TestT0883_UnaryFirstDispatch(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			int v;
			-() Base { return Base(v: -this.v); }
			-(Base o) Base { return Base(v: this.v - o.v); }
		}
		caller() {
			a := Base(v: 5);
			b := Base(v: 3);
			u := -a;
			d := a - b;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@Base.-$unary(i8*") {
		t.Fatalf("expected unary dispatch `@Base.-$unary(i8* ...)` in caller:\n%s", body)
	}
	if !strings.Contains(body, "@Base.-(i8*") {
		t.Fatalf("expected binary dispatch `@Base.-(i8* ...)` in caller:\n%s", body)
	}
}

// (d) A generic type declaring both variants drives the mono declare/define
// path: mangleMethodDeclName must apply the "$unary" discriminator to the mono
// instance name, so the unary and binary variants get distinct LLVM names
// (`@"GenBoth[int].-$unary"` vs `@"GenBoth[int].-"`) — no redefinition.
func TestT0883_GenericMonoDistinctDefinitions(t *testing.T) {
	ir := generateIR(t, `
		type GenBoth[T] {
			T v;
			int tag;
			-(GenBoth[T] o) GenBoth[T] { return GenBoth[T](v: this.v, tag: 2); }
			-() GenBoth[T] { return GenBoth[T](v: this.v, tag: 1); }
		}
		caller() {
			a := GenBoth[int](v: 5, tag: 0);
			b := GenBoth[int](v: 3, tag: 0);
			u := -a;
			d := a - b;
		}
		main() { caller(); }
	`)
	// Count `define` lines only (the names also appear at the caller's call sites).
	unaryDefs, binaryDefs := 0, 0
	for _, line := range strings.Split(ir, "\n") {
		if !strings.HasPrefix(line, "define") {
			continue
		}
		if strings.Contains(line, `@"GenBoth[int].-$unary"(`) {
			unaryDefs++
		} else if strings.Contains(line, `@"GenBoth[int].-"(`) {
			binaryDefs++
		}
	}
	if unaryDefs != 1 {
		t.Fatalf("expected exactly one unary mono definition @\"GenBoth[int].-$unary\"(, got %d:\n%s", unaryDefs, ir)
	}
	if binaryDefs != 1 {
		t.Fatalf("expected exactly one binary mono definition @\"GenBoth[int].-\"(, got %d:\n%s", binaryDefs, ir)
	}
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	if !strings.Contains(body, `@"GenBoth[int].-$unary"(i8*`) {
		t.Fatalf("expected unary mono dispatch in caller:\n%s", body)
	}
	if !strings.Contains(body, `@"GenBoth[int].-"(i8*`) {
		t.Fatalf("expected binary mono dispatch in caller:\n%s", body)
	}
}

// (c) When the type has a vtable (it has children → virtual dispatch), the unary
// and binary variants occupy distinct vtable slots.
func TestT0883_DistinctVtableSlots(t *testing.T) {
	ir := generateIR(t, `
		type Base {
			int v;
			-(Base o) Base { return Base(v: this.v - o.v); }
			-() Base { return Base(v: -this.v); }
		}
		type Child is Base {}
		main() { c := Child(v: 1); }
	`)
	// The vtable must reference both the binary and the unary function pointers.
	vtableLine := ""
	for _, line := range strings.Split(ir, "\n") {
		if strings.Contains(line, "@promise_vtable_Base = ") {
			vtableLine = line
			break
		}
	}
	if vtableLine == "" {
		t.Fatalf("expected @promise_vtable_Base global in IR:\n%s", ir)
	}
	if !strings.Contains(vtableLine, "@Base.-$unary") {
		t.Fatalf("expected unary slot @Base.-$unary in vtable:\n%s", vtableLine)
	}
	if !strings.Contains(vtableLine, "@Base.- ") && !strings.Contains(vtableLine, "@Base.-,") {
		t.Fatalf("expected binary slot @Base.- in vtable:\n%s", vtableLine)
	}
}
