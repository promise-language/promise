package regress11

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1395: When a generic function dispatches a structural-interface method
// through a type param (e.g. scan[T: Parse] calling T.parse(r)), the generic
// body is type-checked against the interface's ABSTRACT signature, so e.Args is
// frozen at the interface arity. The concrete override may carry EXTRA trailing
// defaulted/optional params (a supported conformance shape — see
// identicalSignaturesWithSelf in types/equal.go). padTrailingDefaultArgs must
// fill those trailing args at the concrete dispatch site so the concrete method
// is not invoked with too few arguments (reading garbage for the missing param).

// TestT1395_ExtraIntDefaultFilledInGenericDispatch — a concrete parse with a
// trailing `int extra = 7` default, reached through scan[Widget], must be called
// with the default constant 7 filled in (not an undef/garbage arg).
func TestT1395_ExtraIntDefaultFilledInGenericDispatch(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Widget {
		  int v;
		  parse!(Reader ~r, int extra = 7) Widget `+"`factory"+` {
		    return Widget(v: 100 + extra);
		  }
		}
		run() int {
		  Widget w = scan[Widget]("ignored")?!;
		  return w.v;
		}
		main() { run(); }
	`)
	// The monomorphized scan[Widget] body must call the concrete Widget.parse
	// passing the trailing default `i64 7` — before the fix the call was emitted
	// with only the reader arg and the callee read a garbage `extra`.
	body := codegentest.ExtractDefine(ir, "scan[Widget]")
	if body == "" {
		t.Fatalf("expected scan[Widget] in IR")
	}
	if !strings.Contains(body, "@Widget.parse") {
		t.Errorf("expected scan[Widget] to call @Widget.parse:\n%s", body)
	}
	if !strings.Contains(body, "i64 7") {
		t.Errorf("expected the trailing default `i64 7` to be passed at the concrete dispatch:\n%s", body)
	}
}

// TestT1395_OptionalNoDefaultFilledAsNone — a concrete parse with a trailing
// optional param `int? tag` (no default) is an allowed conformance shape; the
// generic dispatch must fill it with a none (zeroinitialized optional), not
// crash in wrapOptional on an untyped synthetic node.
func TestT1395_OptionalNoDefaultFilledAsNone(t *testing.T) {
	// generateIR runs the full Compile; before the TypNone registration fix the
	// synthetic NoneLit had no recorded type and coerceCallArgs panicked in
	// wrapOptional (insertvalue elem type mismatch) — this call would crash.
	ir := codegentest.GenerateIR(t, `
		type Tagged {
		  int base;
		  parse!(Reader ~r, int? tag) Tagged `+"`factory"+` {
		    int t = tag ?: 5;
		    return Tagged(base: t * 2);
		  }
		}
		run() int {
		  Tagged v = scan[Tagged]("ignored")?!;
		  return v.base;
		}
		main() { run(); }
	`)
	body := codegentest.ExtractDefine(ir, "scan[Tagged]")
	if body == "" {
		t.Fatalf("expected scan[Tagged] in IR")
	}
	if !strings.Contains(body, "@Tagged.parse") {
		t.Errorf("expected scan[Tagged] to call @Tagged.parse:\n%s", body)
	}
}

// TestT1395_MatchingAritySignatureUnchanged — when the concrete method's arity
// matches the interface (the common case — every shipped std parse), the args
// are already complete and padTrailingDefaultArgs is a no-op fast path. Pins
// that generic dispatch to an exact-arity parse still works (no spurious extra
// args synthesized).
func TestT1395_MatchingAritySignatureUnchanged(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Plain {
		  int v;
		  parse!(Reader ~r) Plain `+"`factory"+` {
		    return Plain(v: 42);
		  }
		}
		run() int {
		  Plain p = scan[Plain]("ignored")?!;
		  return p.v;
		}
		main() { run(); }
	`)
	body := codegentest.ExtractDefine(ir, "scan[Plain]")
	if body == "" {
		t.Fatalf("expected scan[Plain] in IR")
	}
	if !strings.Contains(body, "@Plain.parse") {
		t.Errorf("expected scan[Plain] to call @Plain.parse:\n%s", body)
	}
}

// TestT1395_VirtualDispatchFillsDefault — when the concrete type has a subtype
// the call goes through the vtable, whose slot signature is built from the FULL
// concrete param list. The trailing default must be filled there too, or the
// indirect call passes too few arguments.
func TestT1395_VirtualDispatchFillsDefault(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Shower `+"`structural"+` {
		  show(this) int `+"`abstract"+`;
		}
		type Parent {
		  int v;
		  show(this, int extra = 20) int { return this.v + extra; }
		}
		type Child is Parent {
		  new(~this, int v) { this.v = v; }
		}
		render[T: Shower](T x) int { return x.show(); }
		run() int {
		  Parent p = Parent(v: 2);
		  return render[Parent](p);
		}
		main() { run(); }
	`)
	body := codegentest.ExtractDefine(ir, "render[Parent]")
	if body == "" {
		t.Fatalf("expected render[Parent] in IR")
	}
	if !strings.Contains(body, "i64 20") {
		t.Errorf("expected the trailing default `i64 20` at the vtable dispatch:\n%s", body)
	}
}

// TestT1395_EnumDispatchFillsDefault — an enum satisfying the interface with an
// extra defaulted param dispatches through genEnumMethodCall, which needs the
// same padding.
func TestT1395_EnumDispatchFillsDefault(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Shower `+"`structural"+` {
		  show(this) int `+"`abstract"+`;
		}
		enum Level {
		  Low, High,

		  show(this, int extra = 5) int { return extra; }
		}
		render[T: Shower](T x) int { return x.show(); }
		run() int {
		  Level l = Level.High;
		  return render[Level](l);
		}
		main() { run(); }
	`)
	body := codegentest.ExtractDefine(ir, "render[Level]")
	if body == "" {
		t.Fatalf("expected render[Level] in IR")
	}
	if !strings.Contains(body, "@Level.show") {
		t.Errorf("expected render[Level] to call @Level.show:\n%s", body)
	}
	if !strings.Contains(body, "i64 5") {
		t.Errorf("expected the trailing default `i64 5` at the enum dispatch:\n%s", body)
	}
}

// TestT1395_IterNextDirectDispatchFillsDefault — the duck-typed for-in protocol
// supplies NO arguments at all, so emitIterNext (not the AST-driven call paths)
// has to fill a concrete next()'s trailing default. Without it `step` arrives as
// undef and the loop's advance is garbage.
func TestT1395_IterNextDirectDispatchFillsDefault(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Stepper {
		  int n;
		  next(~this, int step = 3) int? {
		    if this.n >= 9 { return none; }
		    this.n = this.n + step;
		    return this.n;
		  }
		}
		run() int {
		  Stepper s = Stepper(n: 0);
		  int sum = 0;
		  for v in s { sum = sum + v; }
		  return sum;
		}
		main() { run(); }
	`)
	body := codegentest.ExtractDefine(ir, "__user.run")
	if body == "" {
		t.Fatalf("expected __user.run in IR")
	}
	if !strings.Contains(body, "@Stepper.next") {
		t.Errorf("expected __user.run to call @Stepper.next:\n%s", body)
	}
	if !strings.Contains(body, "i64 3") {
		t.Errorf("expected next()'s trailing default `i64 3` at the for-in dispatch:\n%s", body)
	}
}

// TestT1395_IterNextVtableDispatchFillsDefault — when the iterator type has a
// subtype the for-in call goes through the vtable. emitIterNext builds that
// slot's function type itself, so the defaulted params must appear BOTH in the
// bitcast'd function type and in the argument list; a mismatch means the callee
// reads a garbage `step` (and, for a loop that advances by it, never terminates).
func TestT1395_IterNextVtableDispatchFillsDefault(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type CountIter {
		  int n;
		  next(~this, int step = 2) int? {
		    if this.n >= 6 { return none; }
		    this.n = this.n + step;
		    return this.n;
		  }
		}
		type FastCounter is CountIter {
		  new(~this) { this.n = 0; }
		}
		run() int {
		  CountIter c = CountIter(n: 0);
		  int sum = 0;
		  for v in c { sum = sum + v; }
		  return sum;
		}
		main() { run(); }
	`)
	body := codegentest.ExtractDefine(ir, "__user.run")
	if body == "" {
		t.Fatalf("expected __user.run in IR")
	}
	// The indirect call must pass the default. A raw `call ... (i8*)` slot type
	// (the pre-fix shape) would carry no i64 operand at all.
	if !strings.Contains(body, "i64 2") {
		t.Errorf("expected next()'s trailing default `i64 2` at the vtable for-in dispatch:\n%s", body)
	}
	if !strings.Contains(body, "i8*, i64") {
		t.Errorf("expected the vtable slot function type to include the defaulted param:\n%s", body)
	}
}

// TestT1395_IterNextGenericOwnerSubstitutesParamType — for a generic iterator the
// owner's type args must be substituted into the trailing params before their
// LLVM types are resolved (emitTrailingDefaultArgValues' subst path), or the
// synthesized vtable-slot/callee types disagree with the mono'd method.
func TestT1395_IterNextGenericOwnerSubstitutesParamType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Repeater[T] {
		  int left;
		  T value;
		  next(~this, int step = 1) T? {
		    if this.left <= 0 { return none; }
		    this.left = this.left - step;
		    return this.value;
		  }
		}
		run() int {
		  Repeater[int] r = Repeater[int](left: 3, value: 7);
		  int sum = 0;
		  for v in r { sum = sum + v; }
		  return sum;
		}
		main() { run(); }
	`)
	body := codegentest.ExtractDefine(ir, "__user.run")
	if body == "" {
		t.Fatalf("expected __user.run in IR")
	}
	if !strings.Contains(body, `@"Repeater[int].next"`) {
		t.Errorf("expected __user.run to call the mono'd Repeater[int].next:\n%s", body)
	}
	if !strings.Contains(body, "i64 1") {
		t.Errorf("expected the trailing default `i64 1` at the generic for-in dispatch:\n%s", body)
	}
}

// TestT1395_ViewAdapterFillsOptionalWithZero — the boxed structural-view adapter
// is synthesized with no AST argument list, so it fills trailing params via
// emitTrailingDefaultArgValues. An extra OPTIONAL param with no default is an
// allowed conformance shape and must arrive as a zeroed optional (none), not as
// an undef.
func TestT1395_ViewAdapterFillsOptionalWithZero(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Shower `+"`structural"+` {
		  show(this) int `+"`abstract"+`;
		}
		type OptShow {
		  int v;
		  show(this, int? bump) int { return this.v + (bump ?: 100); }
		}
		run() int {
		  OptShow o = OptShow(v: 1);
		  Shower s = o;
		  return s.show();
		}
		main() { run(); }
	`)
	body := codegentest.ExtractDefine(ir, "OptShow.show$view_adapt_as_Shower")
	if body == "" {
		t.Fatalf("expected the OptShow.show view adapter in IR:\n%s", ir)
	}
	if !strings.Contains(body, "@OptShow.show") {
		t.Errorf("expected the adapter to forward to @OptShow.show:\n%s", body)
	}
	if !strings.Contains(body, "zeroinitializer") {
		t.Errorf("expected the adapter to pass a zeroed optional for the extra param:\n%s", body)
	}
}

// TestT1395_ViewAdapterFillsDefaultNeverOmittedAtAnyCallSite — regression for the
// sema half of the fix. Nothing in this program omits `extra` at a source call
// site: the only place the default is ever materialized is the synthesized view
// adapter. Before defaults were type-checked at their declaration, the compound
// expression `2 * 3` had no recorded operand type and codegen panicked
// ("cannot resolve Named type from <nil> for operator *").
func TestT1395_ViewAdapterFillsDefaultNeverOmittedAtAnyCallSite(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Shower `+"`structural"+` {
		  show(this) int `+"`abstract"+`;
		}
		type Computed {
		  int v;
		  show(this, int extra = 2 * 3) int { return this.v + extra; }
		}
		run() int {
		  Computed c = Computed(v: 10);
		  Shower s = c;
		  return s.show();
		}
		main() { run(); }
	`)
	body := codegentest.ExtractDefine(ir, "Computed.show$view_adapt_as_Shower")
	if body == "" {
		t.Fatalf("expected the Computed.show view adapter in IR:\n%s", ir)
	}
	// The default is emitted unfolded (`mul i64 2, 3`) and fed to the concrete
	// call; opt folds it later. Before the fix this expression never reached
	// codegen with recorded operand types and the compile panicked.
	if !strings.Contains(body, "mul i64 2, 3") {
		t.Errorf("expected the compound default `2 * 3` to be emitted in the adapter:\n%s", body)
	}
	if !strings.Contains(body, "@Computed.show(") {
		t.Errorf("expected the adapter to forward to @Computed.show:\n%s", body)
	}
}

// TestT1395_ValueTypeDispatchFillsDefault — a pure value type has a different
// four-struct layout (fields embedded in the Value struct, no instance ptr), so
// its generic structural dispatch is a separate lowering path from the heap-type
// case above. The trailing default must be filled there too.
func TestT1395_ValueTypeDispatchFillsDefault(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Shower `+"`structural"+` {
		  show(this) int `+"`abstract"+`;
		}
		type Coord {
		  int x `+"`value"+`;
		  int y `+"`value"+`;
		  show(this, int extra = 7) int { return this.x + this.y + extra; }
		}
		render[T: Shower](T x) int { return x.show(); }
		run() int {
		  Coord c = Coord(x: 1, y: 2);
		  return render[Coord](c);
		}
		main() { run(); }
	`)
	body := codegentest.ExtractDefine(ir, "render[Coord]")
	if body == "" {
		t.Fatalf("expected render[Coord] in IR")
	}
	if !strings.Contains(body, "i64 7") {
		t.Errorf("expected the trailing default `i64 7` at the value-type dispatch:\n%s", body)
	}
}

// TestT1395_MultipleTrailingDefaultsFilledInOrder — more than one extra param
// must be padded, and in declaration order; a reversed or partial fill would
// still produce a well-typed call but the wrong values.
func TestT1395_MultipleTrailingDefaultsFilledInOrder(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Shower `+"`structural"+` {
		  show(this) int `+"`abstract"+`;
		}
		type Combo {
		  int v;
		  show(this, int a = 3, int b = 4) int { return a * 10 + b; }
		}
		render[T: Shower](T x) int { return x.show(); }
		run() int {
		  Combo c = Combo(v: 0);
		  return render[Combo](c);
		}
		main() { run(); }
	`)
	body := codegentest.ExtractDefine(ir, "render[Combo]")
	if body == "" {
		t.Fatalf("expected render[Combo] in IR")
	}
	if !strings.Contains(body, "i64 3, i64 4") {
		t.Errorf("expected both trailing defaults in declaration order (`i64 3, i64 4`):\n%s", body)
	}
}
