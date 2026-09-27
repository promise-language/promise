package regress11

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1881: a `native` method has no LLVM body — it is open-coded at each AST call site
// — so a view vtable slot for one could not be filled. T1885 fixed that for `clone`;
// every Ordered/Equal/Hashable/Format requirement on a primitive or string is native
// too, so `@promise_vtable_int_as_Ordered` was six panicking stubs. Two defects had
// to go: the missing shim, and the adapter forwarding the interface's `Self` argument
// as a raw view where the concrete method wants its own representation.

func TestT1881_OrderedOnIntGetsAdaptersNotStubs(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			Ordered o = 3;
			bool b = o < 5;
		}
	`)
	if strings.Contains(ir, "view_stub_as_Ordered") {
		t.Fatalf("Ordered slots on int must be real adapters, not panicking stubs")
	}
	if !strings.Contains(ir, `@"int.<$view_adapt_as_Ordered"`) {
		t.Fatalf("expected an adapter for int.< as Ordered")
	}
	// The shim carries the CONCRETE signature and open-codes the native operator.
	shim := codegentest.FindDefinedFunc(ir, `@"int.<$native"(`)
	if shim == "" {
		t.Fatalf("expected a synthesized int.< native shim")
	}
	if !strings.Contains(shim, "icmp slt i64") {
		t.Fatalf("the shim must emit the same instruction the AST call site does, got:\n%s", shim)
	}
}

func TestT1881_AdapterUnboxesBothReceiverAndSelfArgument(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			Ordered o = 3;
			bool b = o < 5;
		}
	`)
	adapter := codegentest.FindDefinedFunc(ir, `@"int.<$view_adapt_as_Ordered"(`)
	if adapter == "" {
		t.Fatalf("expected the int.< adapter")
	}
	// Receiver unbox (box field 1) and argument unbox both load an i64.
	if strings.Count(adapter, "load i64") < 2 {
		t.Fatalf("adapter must unbox BOTH the receiver and the Self argument, got:\n%s", adapter)
	}
	// The argument unbox is RTTI-checked — `Ordered a = 3; Ordered b = "x"; a < b`
	// type-checks, so the payload must be verified before it is reinterpreted.
	if !strings.Contains(adapter, "@promise_type_is") {
		t.Fatalf("adapter must check the Self argument's runtime type, got:\n%s", adapter)
	}
	if !strings.Contains(adapter, "viewarg.mismatch") || !strings.Contains(adapter, "@promise_panic") {
		t.Fatalf("a mismatched Self argument must panic, got:\n%s", adapter)
	}
}

// The call site has to box the operand into the slot's `{i8*, i8*}` signature — it
// used to pass the raw scalar, which is what made the adapter read garbage.
func TestT1881_CallSiteBoxesTheSelfOperand(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			Ordered o = 3;
			bool b = o < 5;
		}
	`)
	// Two boxes are allocated in main: the receiver's and the operand's. Before the
	// fix only the receiver was boxed and the literal went through as a bare i64.
	if strings.Count(ir, "@promise_typeinfo_box$int$flat") < 3 {
		t.Fatalf("both the receiver and the Self operand must be boxed before the call")
	}
	if strings.Contains(ir, `%38(i8* %34, i64 5)`) {
		t.Fatalf("the Self operand reached a view slot unboxed")
	}
}

// A SYNTHESIZED structural default with a `Self` parameter needs the adapter just as
// a declared method does: the slot's signature is the interface's, so the call site
// boxes the operand, and a slot pointed straight at the default would hand it the box.
// The two representations happen to be the same size on a 64-bit host (a view pair
// and a `{vtable, int}` value struct are both 16 bytes), so this only showed as a
// hard failure on wasm32 — where `opt` accepted the IR and the runtime rejected the
// module with `type mismatch: expected i64, found i32`.
func TestT1881_SynthesizedDefaultWithSelfParamGetsAnAdapter(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Cents is Ordered {
			int v `+"`"+`value;
			== (Self other) bool => this.v == other.v;
			< (Self other) bool => this.v < other.v;
		}
		main() {
			Ordered o = Cents(v: 3);
			bool a = o > Cents(v: 1);
			bool b = o != Cents(v: 1);
		}
	`)
	slots := vtableSlots(t, ir, "promise_vtable_Cents_as_Ordered")
	// Every slot must take the interface's `{i8*, i8*}` parameter — none may take the
	// concrete's value struct.
	if strings.Contains(slots, "%promise_Cents_v)*") {
		t.Fatalf("a slot still takes the concrete's value struct; the call site boxes:\n%s", slots)
	}
	// `>` is declared on Ordered; `!=` is inherited from Equal — the ancestor spelling
	// of `Self` is the one that used to be missed.
	for _, want := range []string{
		`@"Cents.>$view_adapt_as_Ordered"`,
		`@"Cents.!=$view_adapt_as_Ordered"`,
	} {
		if !strings.Contains(slots, want) {
			t.Fatalf("expected %s in the view vtable, got:\n%s", want, slots)
		}
	}
}

func TestT1881_NativeGettersGetShims(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Counted `+"`"+`structural {
			get len int `+"`"+`abstract;
		}
		main() {
			int[] v = [1, 2, 3];
			Counted c = v;
			int n = c.len;
			Hashable h = 3;
			int x = h.hash;
		}
	`)
	if strings.Contains(ir, "view_stub_as_Counted") {
		t.Fatalf("Vector[int].len must get a shim, not a panicking stub")
	}
	if strings.Contains(ir, "view_stub_as_Hashable") {
		t.Fatalf("int.hash must get a shim, not a panicking stub")
	}
	if !strings.Contains(ir, `$native`) {
		t.Fatalf("expected synthesized native shims")
	}
}

// A `native` member no value-level emitter covers keeps the panicking stub — the
// fallback must stay, and since T1907 its message is actually reported rather than
// swallowed into a `fatal: panic during panic recovery`.
func TestT1881_UncoveredNativeMemberKeepsThePanickingStub(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Trimmable `+"`"+`structural {
			trim() string `+"`"+`abstract;
		}
		main() {
			Trimmable s = " a ";
			string t = s.trim();
		}
	`)
	if !strings.Contains(ir, "view_stub_as_Trimmable") {
		t.Fatalf("string.trim has no value-level emitter yet, so it must keep the stub")
	}
	stub := codegentest.FindDefinedFunc(ir, "@string.trim$view_stub_as_Trimmable(")
	if stub == "" || !strings.Contains(stub, "@promise_panic") {
		t.Fatalf("the stub must panic naming the unfilled slot, got:\n%s", stub)
	}
}
