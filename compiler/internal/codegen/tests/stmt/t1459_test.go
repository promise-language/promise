package stmt

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// emitIterNext has three callers: genForInCustomIter and genForInCustomStream
// (both covered below) and genYieldDelegateIterator. The third cannot reach the
// direct branch at all — sema only lets a value statically typed `Iterator[T]`
// through `yield *`, and that type is structural, so needsVtable is always true
// there. T1945 tracks the sema gap; until it closes, `yield *` has no
// direct-dispatch case to test.
//
// T1459: emitIterNext's direct-dispatch branch mangled the callee from the
// RECEIVER's type name, but no <Child>.<method> function is emitted for a
// plainly inherited method (T1551) — so iterating a subtype that inherits
// next() panicked with "codegen: undeclared method FastCounter.next". The
// vtable branch above it does not catch this either: a leaf, non-abstract
// child has needsVtable() == false. The call must be mangled against the type
// that DECLARES next().
func TestT1459_InheritedNextDispatchesToDeclaringParent(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type CountIter {
			int n;
			next(~this) int? {
				if this.n >= 3 { return none; }
				this.n = this.n + 1;
				return this.n;
			}
		}
		type FastCounter is CountIter {
			new(~this) { this.n = 0; }
		}
		run() int {
			FastCounter f = FastCounter();
			int sum = 0;
			for v in f { sum += v; }
			return sum;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "run")
	codegentest.AssertContains(t, body, "iter.header")
	codegentest.AssertContains(t, body, "@CountIter.next")
	// The child never declares next(), so no such function exists to call.
	codegentest.AssertNotContains(t, body, "@FastCounter.next")
}

// T1459: the iter() (stream) path funnels through the same emitIterNext, so a
// subtype inheriting iter() panicked identically ("undeclared method
// SChild.iter") from genForInCustomStream.
func TestT1459_InheritedIterDispatchesToDeclaringParent(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type SIter {
			int n;
			next(~this) int? {
				if this.n >= 3 { return none; }
				this.n = this.n + 1;
				return this.n;
			}
		}
		type SBase {
			int seed;
			iter(~this) SIter { return SIter(n: this.seed); }
		}
		type SChild is SBase {
			new(~this) { this.seed = 0; }
		}
		run() int {
			SChild s = SChild();
			int sum = 0;
			for v in s { sum += v; }
			return sum;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "run")
	codegentest.AssertContains(t, body, "iter.header")
	codegentest.AssertContains(t, body, "@SBase.iter")
	codegentest.AssertNotContains(t, body, "@SChild.iter")
}

// T1459: the baseline the fix must not disturb — when the receiver declares
// next() itself, resolveDirectDispatchOwner still resolves to the receiver's
// own name, exactly as resolveTypeName did before. Pins that swapping the
// helper in cannot silently reroute the common path to some parent.
func TestT1459_OwnNextStillDispatchesToItself(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type CountUp {
			int n;
			next(~this) int? {
				if this.n >= 3 { return none; }
				this.n = this.n + 1;
				return this.n;
			}
		}
		run() int {
			CountUp c = CountUp(n: 0);
			int sum = 0;
			for v in c { sum += v; }
			return sum;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "run")
	codegentest.AssertContains(t, body, "iter.header")
	codegentest.AssertContains(t, body, "@CountUp.next")
}

// T1459: the shape users actually write — a base that declares `is Iterator[T]`
// and a leaf child that inherits next(). The structural ancestor declares next()
// `abstract, so resolveStructuralOwnerBy must NOT treat it as a per-concrete
// default (that would mangle a never-synthesized @Child.next); the concrete
// non-structural parent owns the implementation. The child is a leaf, so this is
// still the DIRECT branch — the exact combination that panicked.
func TestT1459_InheritedNextFromIteratorConformingParent(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type IBase is Iterator[int] {
			int n;
			next(~this) int? {
				if this.n >= 3 { return none; }
				this.n = this.n + 1;
				return this.n;
			}
		}
		type IChild is IBase {
			new(~this) { this.n = 0; }
		}
		run() int {
			IChild c = IChild();
			int sum = 0;
			for v in c { sum += v; }
			return sum;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "run")
	codegentest.AssertContains(t, body, "@IBase.next")
	// Iterator[int]'s next() is `abstract, so nothing may be synthesized under
	// the leaf's own name — that function is never emitted.
	codegentest.AssertNotContains(t, body, "@IChild.next")
}

// T1459: a structural interface supplying a CONCRETE default next() — the middle
// branch of resolveDirectDispatchOwner, which emitIterNext newly routes through.
// It resolves to the same name the old code produced (defaults are synthesized
// per-concrete, T1559, so the callee IS the concrete implementor), which is why
// this shape worked before the fix; what it pins is that routing it through the
// helper keeps that name and still gets the default synthesized, rather than
// diverting to the interface.
func TestT1459_StructuralDefaultNextSynthesizedUnderConcrete(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Ticking `+"`structural"+` {
			step(~this) int? `+"`abstract"+`;
			next(~this) int? { return this.step(); }
		}
		type Countdown is Ticking {
			int n;
			step(~this) int? {
				if this.n <= 0 { return none; }
				this.n = this.n - 1;
				return this.n;
			}
		}
		run() int {
			Countdown c = Countdown(n: 3);
			int sum = 0;
			for v in c { sum += v; }
			return sum;
		}
		main() { }
	`)
	// The default is synthesized under the concrete implementor and calls its step().
	def := codegentest.ExtractDefine(ir, "Countdown.next")
	if def == "" {
		t.Fatalf("expected synthesized @Countdown.next to be defined:\n%s", ir)
	}
	codegentest.AssertContains(t, def, "@Countdown.step")
	// ...and the loop targets that synthesized name. The interface's own
	// @Ticking.next exists (it backs the structural view's vtable) but is never
	// the callee for a concrete receiver.
	body := codegentest.FuncBody(t, ir, "run")
	codegentest.AssertContains(t, body, "@Countdown.next")
	codegentest.AssertNotContains(t, body, "@Ticking.next")
}

// T1459: next() declared two levels up. resolveMethodOwner recurses through the
// intermediate, so the callee must be the GRANDparent — naming the intermediate
// would be just as undeclared as naming the leaf.
func TestT1459_GrandparentNextDispatchesToDeclaringAncestor(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type GBase {
			int n;
			next(~this) int? {
				if this.n >= 3 { return none; }
				this.n = this.n + 1;
				return this.n;
			}
		}
		type GMid is GBase { }
		type GLeaf is GMid {
			new(~this) { this.n = 0; }
		}
		run() int {
			GLeaf g = GLeaf();
			int sum = 0;
			for v in g { sum += v; }
			return sum;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "run")
	codegentest.AssertContains(t, body, "@GBase.next")
	codegentest.AssertNotContains(t, body, "@GMid.next")
	codegentest.AssertNotContains(t, body, "@GLeaf.next")
}

// T1459 guard in the other direction: a child that DOES override next() must
// still call its own, not be over-routed to the parent by the new owner
// resolution. resolveMethodOwner returns the child's own name here, so
// resolveDirectDispatchOwner takes its first branch and the callee is unchanged
// — this passes with or without the fix, which is exactly the point.
func TestT1459_OverriddenNextStillDispatchesToChild(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type OBase {
			int n;
			next(~this) int? {
				if this.n >= 2 { return none; }
				this.n = this.n + 1;
				return 100;
			}
		}
		type OChild is OBase {
			next(~this) int? {
				if this.n >= 2 { return none; }
				this.n = this.n + 1;
				return 1;
			}
		}
		run() int {
			OChild o = OChild(n: 0);
			int sum = 0;
			for v in o { sum += v; }
			return sum;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "run")
	codegentest.AssertContains(t, body, "@OChild.next")
	codegentest.AssertNotContains(t, body, "@OBase.next")
}

// T1459: the parent that declares next() lives in a MODULE. The callee resolves
// to a module-owned function name, which the user unit must already have declared
// — emitIterNext deliberately does not mirror forwardDeclareModuleMethod (T1740),
// so this pins that the plain lookup suffices for an inherited iterator.
func TestT1459_ModuleDeclaredParentNextDispatch(t *testing.T) {
	ir := codegentest.GenerateIRWithModule(t, "itermod", `
		type ModIter `+"`public"+` {
			int cur `+"`public"+`;
			int lim `+"`public"+`;
			next(~this) int? `+"`public"+` {
				if this.cur >= this.lim { return none; }
				int v = this.cur;
				this.cur = this.cur + 1;
				return v;
			}
		}
	`, `
		use itermod "./itermod";
		type UserChild is itermod.ModIter {
			new(~this) { this.cur = 0; this.lim = 4; }
		}
		run() int {
			UserChild c = UserChild();
			int sum = 0;
			for v in c { sum += v; }
			return sum;
		}
		main() { }
	`)
	body := codegentest.FuncBody(t, ir, "run")
	codegentest.AssertContains(t, body, "@__mod_itermod_ModIter.next")
	codegentest.AssertNotContains(t, body, "@UserChild.next")
}

// T1459 adjacent: the same child, but boxed into an `Iterator[int]` view before
// the loop. Dispatch is virtual here, so the guarantee is one level down — the
// view vtable's next() slot must point at the PARENT's function, since no
// per-concrete default may be synthesized under the child's name for a member
// the structural ancestor only declares `abstract (resolveStructuralOwnerBy's
// abstract-structural branch). A vtable slot naming @VChild.next would link
// against a function that is never emitted.
func TestT1459_ViewVtableOfInheritedNextPointsAtParent(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type VBase is Iterator[int] {
			int cur;
			int lim;
			next(~this) int? {
				if this.cur >= this.lim { return none; }
				int v = this.cur;
				this.cur = this.cur + 1;
				return v;
			}
		}
		type VChild is VBase {
			new(~this) { this.cur = 0; this.lim = 4; }
		}
		run() int {
			Iterator[int] it = VChild();
			int sum = 0;
			for v in it { sum += v; }
			return sum;
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "@VBase.next")
	codegentest.AssertNotContains(t, ir, "@VChild.next")
}
