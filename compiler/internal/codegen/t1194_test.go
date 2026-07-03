package codegen

import (
	"strings"
	"testing"
)

// T1194: A borrow-by-default heap-user-type parameter (`f(Counter c)`) carries no
// drop binding — the caller owns the original. Reassigning it inside the body to a
// fresh owned value (plain `=`, compound `+=`, or `++`/`--`) previously either
// leaked the fresh value (plain assign) or double-freed the caller-owned original
// (compound / inc-dec, via the unconditional drop-old). The fix registers a
// function-scoped drop binding whose flag starts at 0 (borrowed original, not
// owned) only when the param is actually reassigned, and routes the inc/dec and
// compound-assign IdentExpr drop-old through a flag-aware wrapper that skips the
// drop when the flag is 0 and arms it to 1 for the fresh value. Cross-reference:
// T0959.

// The heap `tag` field makes Counter droppable (a pure value type would be skipped
// by maybeRegisterDrop and never get a flag).
const t1194Counter = `type Counter {
	string tag; int n;
	new(~this, string move tag, int n) { this.tag = tag; this.n = n; }
	drop(~this) {}
	++(this) Counter { return Counter(this.tag, this.n + 1); }
	--(this) Counter { return Counter(this.tag, this.n - 1); }
	+(this, int k) Counter { return Counter(this.tag, this.n + k); }
	get value int { return this.n; }
}
`

// `c++` on a borrow param must register a flag-0 binding (store i1 false) and
// guard the drop-old behind that flag (incdec.flagdrop blocks) so the caller-owned
// original is not double-freed and the fresh result is armed for drop.
func TestT1194IncDecBorrowParamGuardsFlag(t *testing.T) {
	ir := generateIR(t, t1194Counter+`
		bump(Counter c) { c++; }
		main() {}
	`)
	assertContains(t, ir, "store i1 false, i1* %c.dropflag")
	assertContains(t, ir, "incdec.flagdrop")
}

// `c += 1` (compound assign) on a borrow param takes the same flag-aware path.
func TestT1194CompoundAssignBorrowParamGuardsFlag(t *testing.T) {
	ir := generateIR(t, t1194Counter+`
		bump(Counter c) { c += 1; }
		main() {}
	`)
	assertContains(t, ir, "store i1 false, i1* %c.dropflag")
	assertContains(t, ir, "incdec.flagdrop")
}

// A compound update in a classic-for update clause (`for i:=0; i<n; c+=1 {}`)
// shares the inline compound path and must also be flag-guarded.
func TestT1194ClassicForCompoundUpdateBorrowParamGuardsFlag(t *testing.T) {
	ir := generateIR(t, t1194Counter+`
		bump(Counter c) { for i := 0; i < 3; c += 1 { i += 1; } }
		main() {}
	`)
	assertContains(t, ir, "store i1 false, i1* %c.dropflag")
	assertContains(t, ir, "incdec.flagdrop")
}

// Plain `c = fresh()` on a borrow param must register the flag-0 binding so the
// fresh value is tracked for drop (no leak). The existing dropBindings drop-old +
// re-arm path then owns it; a drop call for Counter must be present.
func TestT1194PlainAssignBorrowParamRegistersDrop(t *testing.T) {
	ir := generateIR(t, t1194Counter+`
		bump(Counter c) { c = Counter("x", 9); }
		main() {}
	`)
	assertContains(t, ir, "store i1 false, i1* %c.dropflag")
	assertContains(t, ir, "call void @Counter.drop")
}

// A reassignment nested inside an `if` block must still be detected by the
// conservative walker (identReassignedInBlock recurses into control-flow bodies).
func TestT1194NestedReassignDetected(t *testing.T) {
	ir := generateIR(t, t1194Counter+`
		bump(Counter c, bool b) { if b { c = Counter("x", 9); } }
		main() {}
	`)
	assertContains(t, ir, "store i1 false, i1* %c.dropflag")
}

// Negative: a borrow param that is only READ (never reassigned) must NOT get a
// drop binding — behaviour is unchanged and the caller-owned original is never
// touched. No `c.dropflag` should be emitted for it.
func TestT1194ReadOnlyBorrowParamNoBinding(t *testing.T) {
	ir := generateIR(t, t1194Counter+`
		peek(Counter c) int { return c.value; }
		main() {}
	`)
	if strings.Contains(ir, "%c.dropflag") {
		t.Fatalf("read-only borrow param must NOT get a drop binding (found %%c.dropflag):\n%s", ir)
	}
}

// Negative: a pure value-type borrow param reassigned to a fresh value owns no
// heap memory, so maybeRegisterDrop creates no flag and the wrapper falls back to
// the unconditional (no-op) drop-old. No `p.dropflag` should be emitted.
func TestT1194ValueTypeBorrowParamNoBinding(t *testing.T) {
	ir := generateIR(t, `
		type P { int x `+"`value"+`; }
		f(P p) { p = P(x: 1); }
		main() {}
	`)
	if strings.Contains(ir, "%p.dropflag") {
		t.Fatalf("value-type borrow param must NOT get a drop binding (found %%p.dropflag):\n%s", ir)
	}
}

// A generic heap type reassigned as a borrow param must get the flag-0 binding on
// the monomorphized instance (mono method/func define path).
func TestT1194GenericBorrowParamRegistersDrop(t *testing.T) {
	ir := generateIR(t, `
		type Box[T] {
			string tag; T v;
			new(~this, string move tag, T move v) { this.tag = tag; this.v = v; }
			drop(~this) {}
		}
		reset(Box[int] b) { b = Box[int]("x", 0); }
		main() { reset(Box[int]("a", 1)); }
	`)
	assertContains(t, ir, "store i1 false, i1* %b.dropflag")
}

// A borrow param reassigned only inside a `for-in` loop body must be detected by
// the walker's ForInStmt arm and get the flag-0 binding.
func TestT1194ForInBodyReassignDetected(t *testing.T) {
	ir := generateIR(t, t1194Counter+`
		bump(Counter c) { for i in 0..3 { c = Counter("x", i); } }
		main() {}
	`)
	assertContains(t, ir, "store i1 false, i1* %c.dropflag")
}

// A reassignment inside an infinite `for {}` loop must be detected by the
// InfiniteLoop arm.
func TestT1194InfiniteLoopBodyReassignDetected(t *testing.T) {
	ir := generateIR(t, t1194Counter+`
		bump(Counter c) { for { c = Counter("x", 1); break; } }
		main() {}
	`)
	assertContains(t, ir, "store i1 false, i1* %c.dropflag")
}

// A reassignment inside a `while` body must be detected by the WhileStmt arm.
func TestT1194WhileBodyReassignDetected(t *testing.T) {
	ir := generateIR(t, t1194Counter+`
		bump(Counter c) { i := 0; while i < 2 { c = Counter("x", i); i += 1; } }
		main() {}
	`)
	assertContains(t, ir, "store i1 false, i1* %c.dropflag")
}

// A reassignment inside a `match` statement arm block flows through the
// ExprStmt → MatchExpr path of exprReassignsIdent.
func TestT1194MatchArmReassignDetected(t *testing.T) {
	ir := generateIR(t, t1194Counter+`
		bump(Counter c, int n) { match n { 0 => { c = Counter("x", 1); }, _ => {}, } }
		main() {}
	`)
	assertContains(t, ir, "store i1 false, i1* %c.dropflag")
}

// A reassignment inside a `select` case body flows through the SelectStmt arm.
func TestT1194SelectCaseReassignDetected(t *testing.T) {
	ir := generateIR(t, t1194Counter+`
		bump(Counter c, channel[int] ch) { select { v := <-ch: c = Counter("x", v!); } }
		main() {}
	`)
	assertContains(t, ir, "store i1 false, i1* %c.dropflag")
}

// The method call-site (defineMethodFunc) registers the borrow-param reassign
// binding independently of the free-function path.
func TestT1194MethodBorrowParamRegistersDrop(t *testing.T) {
	ir := generateIR(t, t1194Counter+`
		type Holder {
			int z;
			new(~this, int z) { this.z = z; }
			drop(~this) {}
			bump(this, Counter c) { c++; }
		}
		main() {}
	`)
	assertContains(t, ir, "store i1 false, i1* %c.dropflag")
	assertContains(t, ir, "incdec.flagdrop")
}

// The generator coroutine call-site (buildGeneratorCoroutine) harvests the
// borrow-param reassign binding into paramDrops.
func TestT1194GeneratorBorrowParamRegistersDrop(t *testing.T) {
	ir := generateIR(t, t1194Counter+`
		gen(Counter c) stream[int] { c = Counter("x", 1); yield c.value; }
		main() {}
	`)
	assertContains(t, ir, "store i1 false, i1* %c.dropflag")
}

// Negative: an owned `~` param is handled by maybeRegisterDrop (flag starts at 1);
// the borrow-reassign path returns early on RefMut and must NOT emit a flag-0
// init, so no double-free obligation is layered onto the owned drop.
func TestT1194OwnedParamNoFlagZeroInit(t *testing.T) {
	ir := generateIR(t, t1194Counter+`
		bump(Counter ~c) { c = Counter("x", 9); }
		main() {}
	`)
	if strings.Contains(ir, "store i1 false, i1* %c.dropflag") {
		t.Fatalf("owned ~ param must NOT get a flag-0 init from the borrow-reassign path:\n%s", ir)
	}
}
