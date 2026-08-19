package codegen

import (
	"strings"
	"testing"
)

// T1588: using a `~` (mut-ref) parameter after a point in the same body that
// lazily synthesizes a structural default method panicked codegen with
// `undefined variable "x"`.
//
// The mut-ref param binding lives in c.mutRefPtrs / c.mutRefTypes (never in
// c.locals). Resolving the call's owner runs ensureDefaultMethodsSynthesized,
// which generates the synthesized method body mid-statement via
// saveState/restoreState — and compilerState did not carry mutRefPtrs, so the
// binding was cleared by defineMethodFunc and never restored. A local receiver
// never caught this because c.locals *is* snapshotted.
//
// Value-ness and generics are incidental; both heap and value shapes failed.

// (a) Value newtype: `~this` default method inherited from a structural parent,
// called on a mut-ref parameter. The receiver must be the incoming pointer
// param, not a spilled copy.
func TestT1588_ValueNewtypeMutRefParamStructuralDefault(t *testing.T) {
	ir := generateIR(t, `
		type Counter `+"`structural"+` {
			int c `+"`value"+`;
			bump(~this) { this.c = 21; }
		}
		type Tick is Counter { }
		apply(Tick ~x) { x.bump(); }
		main() { Tick t = Tick(c: 1); apply(t); }
	`)
	body := requireFuncT1588(t, ir, "__user.apply")
	assertMutRefParamNotCopied(t, body, "x", "__user.apply")
	if !strings.Contains(body, "@Tick.bump(") {
		t.Fatalf("expected the synthesized `@Tick.bump` call in @__user.apply:\n%s", body)
	}
}

// (b) Heap newtype: same shape, other branch of genMethodCall — also reached
// after the owner-resolution call that clobbered the state.
func TestT1588_HeapNewtypeMutRefParamStructuralDefault(t *testing.T) {
	ir := generateIR(t, `
		type Bumper `+"`structural"+` {
			int c;
			bump(~this) { this.c = 21; }
		}
		type H is Bumper { }
		apply(H ~x) { x.bump(); }
		main() { H h = H(c: 1); apply(h); }
	`)
	body := requireFuncT1588(t, ir, "__user.apply")
	assertMutRefParamNotCopied(t, body, "x", "__user.apply")
	if !strings.Contains(body, "@H.bump(") {
		t.Fatalf("expected the synthesized `@H.bump` call in @__user.apply:\n%s", body)
	}
}

// (c) Through a generic bound — the structural parent is the type-param bound.
func TestT1588_GenericBoundMutRefParamStructuralDefault(t *testing.T) {
	ir := generateIR(t, `
		type Counter `+"`structural"+` {
			int c `+"`value"+`;
			bump(~this) { this.c = 21; }
		}
		type Tick is Counter { }
		apply[T: Counter](T ~x) { x.bump(); }
		main() { Tick t = Tick(c: 1); apply(t); }
	`)
	body := requireFuncT1588(t, ir, "apply[Tick]")
	assertMutRefParamNotCopied(t, body, "x", "apply[Tick]")
	if !strings.Contains(body, "@Tick.bump(") {
		t.Fatalf("expected the synthesized `@Tick.bump` call in @apply[Tick]:\n%s", body)
	}
}

// (d) The general case the fix actually restores: a mut-ref param used *after*
// an unrelated structural-default call earlier in the same body. Nothing about
// the second use involves inheritance — the binding just has to survive the
// nested synthesis triggered by the first statement.
func TestT1588_MutRefParamSurvivesUnrelatedDefaultSynthesis(t *testing.T) {
	ir := generateIR(t, `
		type Greeter `+"`structural"+` {
			int n;
			greet(this) int { return this.n; }
		}
		type Hello is Greeter { }
		type Plain { int v; }
		apply(Plain ~p, Hello h) {
			g := h.greet();
			p.v = g;
		}
		main() { Plain p = Plain(v: 0); Hello h = Hello(n: 7); apply(p, h); }
	`)
	body := requireFuncT1588(t, ir, "__user.apply")
	assertMutRefParamNotCopied(t, body, "p", "__user.apply")
	if !strings.Contains(body, "@Hello.greet(") {
		t.Fatalf("expected the synthesized `@Hello.greet` call in @__user.apply:\n%s", body)
	}
}

// (f) mutRefPtrs is the field that panicked, but the same fix carries three more
// per-function binding maps through compilerState. matchBorrowedIdents (T0485 /
// T0672) is one of them, and losing it fails SILENTLY rather than loudly: a
// borrow-sourced destructured local stops being recognized as a borrow, so a
// later `if let` on it hands the unwrapped binding an OWNING drop flag and the
// map is freed twice — once by the binding, once by the struct that owns it.
//
// The destructure marks `m` borrowed; the `h.greet()` in between synthesizes a
// structural default mid-body (the same saveState/restoreState round trip as
// (a)-(d)); the `if let` afterwards must still see the mark.
func TestT1588_MatchBorrowedIdentSurvivesDefaultSynthesis(t *testing.T) {
	ir := generateIR(t, `
		type Greeter `+"`structural"+` {
			int n;
			greet(this) int { return this.n; }
		}
		type Hello is Greeter { }
		type Holder { (map[string, string]?, int) pr; }
		fn(Hello h) int {
			map[string, string] src = map[string, string]();
			map[string, string]? mo = src;
			Holder hd = Holder(pr: (mo, 5));
			(m, n) := hd.pr;
			int g = h.greet();
			if mm := m { return mm.len + g + n; }
			return n;
		}
		main() { _ := fn(Hello(n: 1)); }
	`)
	body := requireFuncT1588(t, ir, "__user.fn")
	// Guard the premise: the synthesis really happened inside @__user.fn, between
	// the destructure and the unwrap.
	if !strings.Contains(body, "@Hello.greet(") {
		t.Fatalf("expected the synthesized `@Hello.greet` call in @__user.fn:\n%s", body)
	}
	// The if-let binding must stay a borrow — an owning drop flag double-frees the
	// map with Holder.drop.
	if strings.Contains(body, "%mm.dropflag") {
		t.Errorf("if-let binding `mm` got an owning drop flag — the borrow mark on the "+
			"destructured `m` was lost across the mid-body default synthesis (T1588):\n%s", body)
	}
	if strings.Contains(body, "%m.dropflag") {
		t.Errorf("destructured local `m` got a drop flag — a struct-field-sourced destructure "+
			"must stay a borrow:\n%s", body)
	}
	if !strings.Contains(body, "call void @Holder.drop") {
		t.Errorf("expected @Holder.drop to remain the sole owner freeing the map:\n%s", body)
	}
}

// (g) borrowOptionalLocals (T1085) is the fourth carried map, and it fails
// silently the same way. An optional bound from an RTTI downcast of a borrow
// (`Sub? o = b as Sub`) aliases an instance the caller owns, so a non-diverging
// handler unwrap must NOT neutralize the source and track the merged phi. Losing
// the mark across a mid-body synthesis re-enables that neutralize → the phi and
// the external owner both free the instance.
func TestT1588_BorrowOptionalLocalSurvivesDefaultSynthesis(t *testing.T) {
	ir := generateIR(t, `
		type Greeter `+"`structural"+` {
			int n;
			greet(this) int { return this.n; }
		}
		type Hello is Greeter { }
		type Base { int x; kind(this) string { return "base"; } }
		type Sub is Base { int y; kind(this) string { return "sub"; } }
		fn(Base b, Hello h) int {
			Sub? o = b as Sub;
			int g = h.greet();
			return (o? _ { Sub(x: -1, y: g) }).y;
		}
		main() { _ := fn(Sub(x: 1, y: 2), Hello(n: 3)); }
	`)
	body := requireFuncT1588(t, ir, "__user.fn")
	if !strings.Contains(body, "@Hello.greet(") {
		t.Fatalf("expected the synthesized `@Hello.greet` call in @__user.fn:\n%s", body)
	}
	if strings.Contains(body, t1085HeapNeutralizeSig) {
		t.Errorf("the handler unwrap neutralized the borrow-holding optional %%o (GEP %q) — "+
			"the T1085 borrow mark was lost across the mid-body default synthesis, so the "+
			"merged phi double-frees the caller-owned instance (T1588):\n%s",
			t1085HeapNeutralizeSig, body)
	}
}

func requireFuncT1588(t *testing.T, ir, name string) string {
	t.Helper()
	// extractDefine, not extractFunction: the latter matches the first `@name(`
	// anywhere — including the call site in @main — and would hand back @main's
	// body instead.
	body := extractDefine(ir, name)
	if body == "" {
		t.Fatalf("expected @%s in IR:\n%s", name, ir)
	}
	return body
}

// assertMutRefParamNotCopied checks that a mut-ref parameter is still bound as
// the raw incoming pointer. A mut-ref param is never alloca'd, so a
// `%<name>.addr` alloca would mean the binding was rebuilt as a plain by-value
// local and mutations would not reach the caller.
func assertMutRefParamNotCopied(t *testing.T, body, param, fn string) {
	t.Helper()
	if !strings.Contains(body, "%"+param) {
		t.Fatalf("expected mut-ref param %%%s to be used in @%s:\n%s", param, fn, body)
	}
	if strings.Contains(body, "%"+param+".addr") {
		t.Fatalf("mut-ref param %%%s is alloca-backed in @%s (write-back lost):\n%s", param, fn, body)
	}
}

// (e) The mirror half of the same fix: saveState must also CLEAR the mut-ref
// binding, not just restore it.
//
// A structural-view method adapter (emitViewMethodAdapter) is a body the
// compiler builds by hand, mid-expression, while the enclosing function is being
// generated — and it fills the concrete method's trailing parameters by
// generating their DEFAULT-VALUE expressions into its own body. genIdentExpr
// consults c.mutRefPtrs before c.locals, so if saveState leaves the enclosing
// function's mut-ref bindings in place, a default expression naming the same
// identifier resolves to the ENCLOSING function's parameter pointer: the adapter
// emitted `load { i8*, i8* }, { i8*, i8* }* %base_scale` for a `%base_scale`
// belonging to @__user.apply — a cross-function reference, and the wrong type for
// the parameter it fills.
//
// Unlike (a)–(d), no define* entry point covers this path: the hand-built bodies
// (emitViewMethodAdapter, compileTestCoroutine, emitCloneFn, the extern-call
// wrapper) each keep their own reset list and never reset mutRefPtrs, which is
// why the clear belongs in saveState.
func TestT1588_ViewAdapterDefaultArgDoesNotInheritMutRefBinding(t *testing.T) {
	ir := generateIR(t, `
		get base_scale int { return 2; }
		type Shape `+"`structural"+` {
			area(this) int `+"`abstract"+`;
		}
		type Boxy {
			int w;
			area(this, int scale = base_scale) int { return this.w * scale; }
		}
		type Counter { int c; }
		apply(Counter ~base_scale) {
			Boxy b = Boxy(w: 5);
			Shape s = b;
			base_scale.c = s.area();
		}
		main() { Counter k = Counter(c: 0); apply(k); }
	`)
	adapter := extractDefine(ir, "Boxy.area$view_adapt_as_Shape")
	if adapter == "" {
		t.Fatalf("expected the structural-view adapter @Boxy.area$view_adapt_as_Shape in IR:\n%s", ir)
	}
	// The default must be the module getter call, generated inside the adapter...
	if !strings.Contains(adapter, "@__user.base_scale()") {
		t.Errorf("expected the adapter to call the module getter @__user.base_scale for the "+
			"defaulted param:\n%s", adapter)
	}
	// ...never a load through the enclosing function's mut-ref parameter pointer.
	if strings.Contains(adapter, "%base_scale") {
		t.Errorf("@Boxy.area$view_adapt_as_Shape references %%base_scale, the mut-ref parameter of "+
			"@__user.apply — saveState must clear mutRefPtrs for a hand-built nested body (T1588):\n%s",
			adapter)
	}
	// The enclosing function's own binding must still work after the adapter was
	// built mid-statement (the restore half).
	body := requireFuncT1588(t, ir, "__user.apply")
	assertMutRefParamNotCopied(t, body, "base_scale", "__user.apply")
}
