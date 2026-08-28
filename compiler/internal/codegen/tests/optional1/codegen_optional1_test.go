package optional1

import (
	"fmt"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T0761: RTTI cast whose subject is itself an Optional. genCastExpr used to
// treat the {i1,{i8*,i8*}} optional as a bare value struct and panic; it now
// branches to genOptionalCastExpr, which unwraps field 1 before promise_type_is.
func TestOptionalSubjectForceCast(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		main() {
			Base b = Der(name: "x");
			Base? ob = b;
			d := ob as! Der;
			_ := d.tag();
		}
	`)
	// Compiled without panic, took the optional-subject path (present/panic
	// blocks) and queried RTTI after unwrapping the inner value struct.
	codegentest.AssertContains(t, ir, "optcast.present")
	codegentest.AssertContains(t, ir, "optcast.nonepanic")
	codegentest.AssertContains(t, ir, "call i32 @promise_type_is(")
}

func TestOptionalSubjectOptionalCast(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		main() {
			Base b = Der(name: "x");
			Base? ob = b;
			Der? d = ob as Der;
			if d { }
		}
	`)
	// Optional-subject `as` path: presence check, RTTI check, some/none merge.
	codegentest.AssertContains(t, ir, "optcast.check")
	codegentest.AssertContains(t, ir, "optcast.some")
	codegentest.AssertContains(t, ir, "optcast.none")
	codegentest.AssertContains(t, ir, "optcast.merge")
	codegentest.AssertContains(t, ir, "call i32 @promise_type_is(")
}

// T0850: an RTTI optional cast whose subject is a BORROWED optional (`T?&`,
// here `Ref[Base?].borrow`) used to crash codegen — the non-optional RTTI path
// fed the loaded `{i1,{i8*,i8*}}` optional to wrapOptional → insertvalue/store
// type mismatch panic. The fix peels the SharedRef/MutRef and routes through
// genOptionalCastExpr (borrowSource): it must no longer panic, must emit the
// optcast blocks (proving it took the optional-subject path), and must dup the
// inner (heapdup.copy) since the borrow aliases the Arc's external-owned payload.
func TestBorrowedOptionalSubjectCast(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		main() {
			Base b = Der(name: "x");
			Base? ob = b;
			a := Ref[Base?](ob);
			Der? d = a.borrow as Der;
			if d { }
		}
	`)
	// Routed through the optional-subject `as` path (not the bare-value path).
	codegentest.AssertContains(t, ir, "optcast.check")
	codegentest.AssertContains(t, ir, "optcast.some")
	codegentest.AssertContains(t, ir, "optcast.none")
	codegentest.AssertContains(t, ir, "optcast.merge")
	codegentest.AssertContains(t, ir, "call i32 @promise_type_is(")
	// The borrowed inner is duped into an owned copy before the RTTI dispatch.
	codegentest.AssertContains(t, codegentest.ExtractDefine(ir, ".goroutine.main"), "heapdup.copy")
}

// T0850: a forced borrowed-optional cast (`Ref[T?].borrow as! U`) takes the
// optional-subject force path (panic on none/mismatch, return the duped inner).
func TestBorrowedOptionalSubjectForceCast(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		main() {
			Base b = Der(name: "x");
			Base? ob = b;
			a := Ref[Base?](ob);
			d := a.borrow as! Der;
			_ := d.tag();
		}
	`)
	codegentest.AssertContains(t, ir, "optcast.present")
	codegentest.AssertContains(t, ir, "optcast.nonepanic")
	codegentest.AssertContains(t, ir, "optcast.mismatch")
	codegentest.AssertContains(t, codegentest.ExtractDefine(ir, ".goroutine.main"), "heapdup.copy")
}

// T0761: an Optional cast whose subject is a container element (`v[i]`) aliases
// the vector's bucket, so genOptionalCastExpr must dup the inner — otherwise both
// the cast result and the vector free it (double-free) / the result leaks. The
// dup emits a heapdup.copy block before the cast's RTTI check.
func TestOptionalSubjectIndexCastDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		main() {
			Base?[] v = [];
			Base b = Der(name: "x");
			v.push(b);
			d := v[0] as! Der;
			_ := d.tag();
		}
	`)
	// Aliasing source is duped into an owned copy before the RTTI dispatch.
	// Scoped to @main: the stdAll clone funcs always emit heapdup.copy, so a
	// whole-IR check would be trivially true.
	codegentest.AssertContains(t, codegentest.ExtractDefine(ir, ".goroutine.main"), "heapdup.copy")
	codegentest.AssertContains(t, ir, "optcast.present")
}

// T0761: a scalar optional subject (`int? as f64`) has a bare scalar inner, not a
// value struct — genOptionalCastExpr must take the scalar path (emitScalarCast),
// not the RTTI path (which would extractvalue a non-aggregate and panic).
func TestOptionalSubjectScalarCast(t *testing.T) {
	// Force: unwrap (panic on none) then sitofp the inner int to f64.
	irForce := codegentest.GenerateIR(t, `
		main() {
			int? x = 5;
			f := x as! f64;
			_ := f;
		}
	`)
	codegentest.AssertContains(t, irForce, "optcast.present")
	codegentest.AssertContains(t, irForce, "sitofp") // scalar conversion, not an RTTI dispatch
	// Optional: present → some(convert); absent → none.
	irOpt := codegentest.GenerateIR(t, `
		main() {
			int? x = 5;
			f64? f = x as f64;
			if f { }
		}
	`)
	codegentest.AssertContains(t, irOpt, "optcast.some")
	codegentest.AssertContains(t, irOpt, "optcast.none")
	codegentest.AssertContains(t, irOpt, "sitofp") // scalar conversion, not an RTTI dispatch
}

// T0761: an Optional cast whose subject is an OWNED-LOCAL member field
// (`h.slot as Der`, h a local — not `this`). This is the MemberExpr arm that
// returns the *non*-aliasing/*non*-owned verdict in all three helpers:
// optionalCastSourceAliasesExternalOwner=false (no dup), optionalCastResultOwnsInner=false
// (no heap temp), and neutralizeOptionalCastSource clears the owner's field flag
// on the match path. Distinct from the borrowed-`this` and ident shapes.
func TestOptionalSubjectOwnedMemberCast(t *testing.T) {
	src := `
		type Base { string name; tag(this) string ` + "`" + `abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		type Holder { Base? slot; drop(~this) {} }
		main() {
			Base b = Der(name: "x");
			Holder h = Holder(slot: b);
			%s
		}
	`
	// Optional: owned-member source is neutralized (not duped) on match.
	// (heapdup.copy is scoped to @main — the stdAll clone funcs always emit it.)
	irOpt := codegentest.GenerateIR(t, fmt.Sprintf(src, `Der? d = h.slot as Der; if d { }`))
	codegentest.AssertContains(t, irOpt, "optcast.check")
	codegentest.AssertContains(t, irOpt, "optcast.some")
	codegentest.AssertContains(t, irOpt, "call i32 @promise_type_is(")
	codegentest.AssertNotContains(t, codegentest.ExtractDefine(irOpt, ".goroutine.main"), "heapdup.copy") // owned-local member is NOT duped
	// Force: same source shape via the `as!` path.
	irForce := codegentest.GenerateIR(t, fmt.Sprintf(src, `d := h.slot as! Der; _ := d.tag();`))
	codegentest.AssertContains(t, irForce, "optcast.present")
	codegentest.AssertContains(t, irForce, "optcast.nonepanic")
	codegentest.AssertNotContains(t, codegentest.ExtractDefine(irForce, ".goroutine.main"), "heapdup.copy")
}

// T0761: an Optional cast whose subject is a borrowed-`this.field` inside a
// `this` method. The MemberExpr arm here takes the aliasing/owned-true verdict:
// the caller still owns the field, so the inner is duped (heapdup.copy) and the
// `as` path registers it as a heap temp. Mirrors the index-source dup path but
// through the member shape.
func TestOptionalSubjectBorrowedThisCast(t *testing.T) {
	src := `
		type Base { string name; tag(this) string ` + "`" + `abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		type Holder {
			Base? slot;
			%s
			drop(~this) {}
		}
		main() {
			Base b = Der(name: "x");
			Holder h = Holder(slot: b);
			_ := h.probe();
		}
	`
	// Force through borrowed this: inner is duped before the RTTI dispatch.
	// (heapdup.copy is scoped to the probe method — stdAll clone funcs also emit it.)
	irForce := codegentest.GenerateIR(t, fmt.Sprintf(src,
		`probe(this) string { c := this.slot as! Der; return c.tag(); }`))
	codegentest.AssertContains(t, codegentest.ExtractFunction(irForce, "Holder.probe"), "heapdup.copy")
	codegentest.AssertContains(t, irForce, "optcast.present")
	// Optional through borrowed this: duped AND registered as a heap temp (result
	// owns the duped inner; freed on present+mismatch).
	irOpt := codegentest.GenerateIR(t, fmt.Sprintf(src,
		`probe(this) bool { Der? d = this.slot as Der; if d { return true; } return false; }`))
	codegentest.AssertContains(t, codegentest.ExtractFunction(irOpt, "Holder.probe"), "heapdup.copy")
	codegentest.AssertContains(t, irOpt, "optcast.check")
	codegentest.AssertContains(t, irOpt, "optcast.some")
}

// T0761: an Optional `as` cast on a call-result TEMP source. The temp owns its
// inner outright (no source binding to neutralize), so optionalCastResultOwnsInner
// returns true via the default arm and the `as` path registers the inner as a heap
// temp inside checkBlock — freed on present+mismatch, claimed by the binding on
// match. (The only other Go `as` test uses an ident source, which skips this block.)
func TestOptionalSubjectTempOptionalCast(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		make_opt(string move n) Base? { Base s = Der(name: n); return s; }
		main() {
			Der? d = make_opt("x") as Der;
			if d { }
		}
	`)
	codegentest.AssertContains(t, ir, "optcast.check")
	codegentest.AssertContains(t, ir, "optcast.some")
	codegentest.AssertContains(t, ir, "optcast.none")
	// Temp source is owned outright — no dup (scoped to @main; stdAll clone funcs
	// emit heapdup.copy elsewhere), but heap-temp tracked for present+mismatch.
	codegentest.AssertNotContains(t, codegentest.ExtractDefine(ir, ".goroutine.main"), "heapdup.copy")
}

// T0761: a paren-wrapped Optional cast source (`(oo) as Der`). The `as` move
// path's neutralizeOptionalCastSource must peel the ParenExpr before clearing the
// underlying ident's present flag (otherwise the source's drop double-frees the
// inner). Compiles cleanly and takes the optcast path.
func TestOptionalSubjectParenSourceCast(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		main() {
			Base b = Der(name: "x");
			Base? oo = b;
			Der? d = (oo) as Der;
			if d { }
		}
	`)
	codegentest.AssertContains(t, ir, "optcast.check")
	codegentest.AssertContains(t, ir, "optcast.some")
	codegentest.AssertContains(t, ir, "call i32 @promise_type_is(")
}

func TestBangUnwrapInsideHandler(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		parse!(string s) int { return 0; }
		foo() {
			parse("x") ? e { int v = parse("0")?!; };
		}
		main() { }
	`)
	// Should have both handler and panic-on-unwrap blocks
	codegentest.AssertContains(t, ir, "error.handler")
	codegentest.AssertContains(t, ir, "error.panic")
}

// --- Part B: Optional tests ---

func TestOptionalNone(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { int? x = none; }`)
	// Should alloca { i1, i64 } and zero-initialize
	codegentest.AssertContains(t, ir, "alloca { i1, i64 }")
	codegentest.AssertContains(t, ir, "zeroinitializer")
}

func TestOptionalSome(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { int? x = 42; }`)
	// Should alloca { i1, i64 } and wrap: { true, 42 }
	codegentest.AssertContains(t, ir, "alloca { i1, i64 }")
	codegentest.AssertContains(t, ir, "insertvalue { i1, i64 }")
	codegentest.AssertContains(t, ir, "i1 true")
}

func TestOptionalStringNone(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { string? x = none; }`)
	codegentest.AssertContains(t, ir, "alloca { i1, i8* }")
	codegentest.AssertContains(t, ir, "zeroinitializer")
}

func TestOptionalVariable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int? x = 42;
			int? y = x;
		}
	`)
	// Should load/store { i1, i64 } struct
	codegentest.AssertContains(t, ir, "load { i1, i64 }")
	codegentest.AssertContains(t, ir, "store { i1, i64 }")
}

// T1160: a callee that is neither an ident, a member, nor a generic instantiation —
// here a force-unwrapped optional closure — materializes its fat pointer from an
// expression, so isClosureValueCallee falls through to its terminal `return true`.
// The callee's env may own the closure it hands back, so the result is left alone.
func TestClosureUnwrappedOptionalCalleeResultNotTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		call_optional((() -> () -> int)? o) { o!(); }
		main() {}
	`)
	body := codegentest.ExtractFunction(ir, "__user.call_optional")
	codegentest.AssertContains(t, body, "define") // guard: the body was actually found
	codegentest.AssertNotContains(t, body, "env.tmp.drop")
}

func TestFunctionTypedFieldOptionalReturn(t *testing.T) {
	// Critical for _FnIter[T] pattern where _next is () -> T?
	ir := codegentest.GenerateIR(t, `
		type Supplier {
			() -> int? produce;
			next() int? { return this.produce(); }
		}
		main() {
			s := Supplier(produce: || -> int? { return 42; });
		}
	`)
	codegentest.AssertContains(t, ir, "@Supplier.next")
	// Should call through indirect call path (fat pointer)
	codegentest.AssertContains(t, ir, "extractvalue { i8*, i8* }")
}

// ================================================================
// Stage 8h — Optional Patterns, String Interpolation & Expression Completeness
// ================================================================

// --- Part A: If-unwrap ---

func TestIfUnwrap(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int? x = 42;
			if val := x {
				int y = val + 1;
			}
		}
	`)
	codegentest.AssertContains(t, ir, "extractvalue")
	codegentest.AssertContains(t, ir, "ifunwrap.then")
	codegentest.AssertContains(t, ir, "ifunwrap.end")
}

func TestIfUnwrapElse(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int? x = none;
			if val := x {
				int y = val;
			} else {
				int z = 0;
			}
		}
	`)
	codegentest.AssertContains(t, ir, "ifunwrap.then")
	codegentest.AssertContains(t, ir, "ifunwrap.else")
	codegentest.AssertContains(t, ir, "ifunwrap.end")
}

// --- Part B: While-unwrap ---

func TestWhileUnwrap(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int? x = 42;
			while val := x {
				break;
			}
		}
	`)
	codegentest.AssertContains(t, ir, "whileunwrap.header")
	codegentest.AssertContains(t, ir, "whileunwrap.body")
	codegentest.AssertContains(t, ir, "whileunwrap.exit")
	codegentest.AssertContains(t, ir, "extractvalue")
}

func TestWhileUnwrapBreak(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int? x = 10;
			while val := x {
				break;
			}
		}
	`)
	// break should jump to exit block
	codegentest.AssertContains(t, ir, "br label %whileunwrap.exit")
}

// --- Part C: Optional chaining ---

func TestOptionalChain(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Dog { int age; }
		main() {
			Dog? d = Dog(age: 3);
			int? a = d?.age;
		}
	`)
	codegentest.AssertContains(t, ir, "optchain.some")
	codegentest.AssertContains(t, ir, "optchain.none")
	codegentest.AssertContains(t, ir, "optchain.merge")
}

func TestOptionalChainNone(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Dog { int age; }
		main() {
			Dog? d = none;
			int? a = d?.age;
		}
	`)
	codegentest.AssertContains(t, ir, "optchain.some")
	codegentest.AssertContains(t, ir, "optchain.none")
	codegentest.AssertContains(t, ir, "phi")
}

// T1297: a fixed-array literal with an Optional element type Some-wraps each
// non-optional element into the {i1,T} slot via insertvalue.
func TestT1297_FixedArrayOptionalElementWrap(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { int?[3] x = [1, none, 3]; }
	`)
	// Elements are wrapped into the { i1, i64 } optional element struct.
	codegentest.AssertContains(t, ir, "insertvalue { i1, i64 }")
	// Slot type is the optional element struct.
	codegentest.AssertContains(t, ir, "alloca [3 x { i1, i64 }]")
}

// T1297: a vector literal with an Optional element type takes the heap path
// (not the static .rodata path) so each element can be Some-wrapped, even when
// every element is a compile-time constant.
func TestT1297_OptionalElementVectorTakesHeapPath(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { int?[] x = [1, 2, 3]; }
	`)
	// Heap allocation (not a .rodata static vector global).
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(")
	// Elements Some-wrapped into the { i1, i64 } optional element struct.
	codegentest.AssertContains(t, ir, "insertvalue { i1, i64 }")
}

// T0579: Array field with Optional<HeapUser> element — exact repro from the
// bug. The Optional inside the array carries the full value struct, not
// bare i8*.
func TestFixedArrayFieldOptionalHeapUserElement(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _BoxA { int n; drop(~this) {} }
		type _HolderArr { _BoxA?[2] data; }
		main() {
			_BoxA? a = _BoxA(n: 7);
			_BoxA? b = _BoxA(n: 8);
			_HolderArr h = _HolderArr(data: [a, b]);
		}
	`)
	// Field slot should hold {i1, value_struct} per element, not {i1, i8*}.
	codegentest.AssertContains(t, ir, "[2 x { i1, { i8*, i8* } }]")
}

// T0583: Optional<HeapUser> element — overwrite must run the Optional drop
// dispatcher on the previous slot, dropping the inner instance when present.
func TestFixedArrayIndexAssignDropsOldOptionalHeapUser(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _Box { int n; drop(~this) {} }
		main() {
			_Box? a = _Box(n: 1);
			_Box? b = _Box(n: 2);
			_Box?[2] arr = [a, b];
			_Box? c = _Box(n: 3);
			arr[1] = c;
		}
	`)
	codegentest.AssertContains(t, ir, "arrassign.ok")
	// The Optional's drop dispatcher checks presence then drops the inner _Box.
	codegentest.AssertContains(t, ir, "call void @_Box.drop")
}

// T0599: bare-T RHS assigned to an Optional<T> fixed-array slot. The
// IndexExpr-LHS path in genAssignStmt had no Optional-wrap (the MemberExpr
// and IdentExpr paths did), so genArrayIndexAssign's NewStore got a bare T
// against a {i1, T}* slot and panicked with "store operands are not
// compatible". Pre-fix, generateIR() here panics and fails the test; post-fix
// the bare _Box is wrapped into the {i1, {i8*, i8*}} Optional before the store.
func TestFixedArrayIndexAssignBareToOptionalHeapUser(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _Box { int n; drop(~this) {} }
		main() {
			_Box? a = _Box(n: 1);
			_Box? b = _Box(n: 2);
			_Box?[2] arr = [a, b];
			arr[0] = _Box(n: 99);
		}
	`)
	codegentest.AssertContains(t, ir, "arrassign.ok")
	// The bare _Box ctor result is wrapped into the Optional struct (present
	// flag set, then the value-struct inserted) before the slot store.
	codegentest.AssertContains(t, ir, "insertvalue { i1, { i8*, i8* } } undef, i1 true, 0")
}

// T0599: bare string literal assigned to a string? fixed-array slot — the
// string-temp claim must run BEFORE the Optional wrap (val identity changes
// after wrapOptional). Pre-fix this panicked with "store operands are not
// compatible: src=i8*; dst={i1,i8*}*".
func TestFixedArrayIndexAssignBareToOptionalString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string? oa = "a";
			string? ob = "b";
			string?[2] arr = [oa, ob];
			arr[0] = "new";
		}
	`)
	codegentest.AssertContains(t, ir, "arrassign.ok")
	codegentest.AssertContains(t, ir, "insertvalue { i1, i8* } undef, i1 true, 0")
}

func TestFixedArrayIndexDupsOptionalHeapUser(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _B { int n; drop(~this) {} }
		main() {
			_B? a = _B(n: 1);
			_B? b = _B(n: 2);
			_B?[2] arr = [a, b];
			_B? x = arr[0];
		}
	`)
	codegentest.AssertContains(t, ir, "arridx.ok")
	// Optional[heap-user] dup path: extract inner, dupHeapValue, insert back.
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(")
	codegentest.AssertContains(t, ir, "call void @llvm.memcpy")
}

func TestFixedArrayIndexDupsOptionalString(t *testing.T) {
	// Optional[string]: extract inner, dup the string, insert back.
	ir := codegentest.GenerateIR(t, `
		main() {
			a := "first";
			a = a + "+";
			b := "second";
			b = b + "+";
			string? oa = a;
			string? ob = b;
			string?[2] arr = [oa, ob];
			string? x = arr[0];
		}
	`)
	codegentest.AssertContains(t, ir, "arridx.ok")
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new(")
}

// --- Part D: is/as expression tests ---

func TestIsPresent(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int? x = 42;
			bool b = x is present;
		}
	`)
	// Should extract the i1 flag from the optional struct
	codegentest.AssertContains(t, ir, "extractvalue { i1, i64 }")
}

func TestOptionalHandlerRecovery(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int? x = none;
			int y = x ? _ { 0; };
		}
	`)
	codegentest.AssertContains(t, ir, "opt.none")
	codegentest.AssertContains(t, ir, "opt.some")
	codegentest.AssertContains(t, ir, "opt.merge")
}

func TestOptionalForceUnwrapBang(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int? x = 42;
			int y = x!;
		}
	`)
	codegentest.AssertContains(t, ir, "unwrap.ok")
	codegentest.AssertContains(t, ir, "unwrap.panic")
	codegentest.AssertContains(t, ir, "promise_panic")
}

// T0817: Directly invoking a force-unwrapped optional closure `o!()` must
// compile (no "unsupported callee type *ast.OptionalUnwrapExpr" panic) and
// emit an indirect call through the materialized {fn, env} fat pointer.
func TestT0817OptionalUnwrapClosureCall(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			s := "cap" + "tured";
			(() -> int)? o = move || -> s.len;
			int n = o!();
		}
	`)
	// Indirect call through a loaded function pointer (fat-pointer dispatch),
	// not a named direct call.
	codegentest.AssertContains(t, ir, "call i64 %")
}

func TestOptionalForceUnwrapAsBang(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int? x = 42;
			int y = x as! int;
		}
	`)
	// Should extractvalue to check flag, then extractvalue to get inner value
	codegentest.AssertContains(t, ir, "unwrap.ok")
	codegentest.AssertContains(t, ir, "unwrap.panic")
	codegentest.AssertContains(t, ir, "promise_panic")
}

// B0301: Optional force-unwrap used as a constructor argument must neutralize
// the source optional's present flag to prevent double-free.
func TestOptionalForceUnwrapConstructorArg(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Inner { int x; }
		type Outer { Inner inner; }
		main() {
			Inner? opt = Inner(x: 1);
			Outer o = Outer(inner: opt!);
		}
	`)
	// After unwrap.ok, the source optional's present flag (field 0) must be
	// set to false. Look for "store i1 false" targeting the optional's GEP
	// at field 0 after the unwrap block.
	codegentest.AssertContains(t, ir, "unwrap.ok")
	// The constructor should store the unwrapped value into the Outer instance
	codegentest.AssertContains(t, ir, "store { i8*, i8* }")
}

func TestIsPresentStringOptional(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string? x = "hello";
			bool b = x is present;
			bool c = x is absent;
		}
	`)
	// Should extractvalue on { i1, i8* } optional
	codegentest.AssertContains(t, ir, "extractvalue { i1, i8* }")
}

func TestIsTypeOnOptionalPrimitive(t *testing.T) {
	// B0029: `is` type check on optional primitives should not try to extract vtable.
	// Before the fix, this panicked with "bitcast i64 to {i8*}*".
	ir := codegentest.GenerateIR(t, `
		main() {
			int? x = 42;
			bool b = x is int;
		}
	`)
	// Should extract i1 flag from {i1, i64} — presence check only
	codegentest.AssertContains(t, ir, "extractvalue { i1, i64 }")
}

func TestIsTypeOnOptionalUserType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal {
			string name;
			speak() string `+"`"+`abstract;
		}
		type Dog is Animal {
			speak() string { return "Woof"; }
		}
		main() {
			Animal d = Dog(name: "Rex");
			Animal? a = d;
			bool b = a is Dog;
		}
	`)
	// Should use RTTI check on unwrapped value, guarded by presence flag
	codegentest.AssertContains(t, ir, "promise_type_is")
}

func TestIsTypeOnOptionalString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string? s = "hello";
			bool b = s is string;
		}
	`)
	// String optional: {i1, i8*} — should extract flag only, no RTTI
	codegentest.AssertContains(t, ir, "extractvalue { i1, i8* }")
}

func TestIsTypeOnOptionalEnum(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color { Red, Green, Blue }
		main() {
			Color? c = Color.Red;
			bool b = c is Color;
		}
	`)
	// Enum optional: should extract flag only, no RTTI
	codegentest.AssertContains(t, ir, "extractvalue")
}

func TestIsTypeOnOptionalBoolFalse(t *testing.T) {
	// false is a valid present value — is bool must return true
	ir := codegentest.GenerateIR(t, `
		main() {
			bool? b = false;
			bool ok = b is bool;
		}
	`)
	codegentest.AssertContains(t, ir, "extractvalue { i1, i8 }")
}

func TestStructuralAdapterExtraOptionalParam(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Printable `+"`"+`structural {
			print() string `+"`"+`abstract;
		}
		type Doc {
			print(int? indent) string { return "doc"; }
		}
		main() {
			Printable p = Doc();
			string s = p.print();
		}
	`)
	// Adapter thunk should be generated
	codegentest.AssertContains(t, ir, "Doc.print$view_adapt")
	codegentest.AssertContains(t, ir, "@promise_vtable_Doc_as_Printable")
}

func TestStructuralAdapterNonOptionalToOptionalReturn(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Finder `+"`"+`structural {
			find() int? `+"`"+`abstract;
		}
		type Always {
			find() int { return 42; }
		}
		main() {
			Finder f = Always();
		}
	`)
	codegentest.AssertContains(t, ir, "Always.find$view_adapt")
}

func TestVectorOptionalStructuralElementDropAndClone(t *testing.T) {
	// T1291: a Vector[structural-interface?] (Optional-wrapped structural element)
	// must (a) drop each present heap-boxed element through __promise_structural_drop
	// at vector drop — the drop gate (typeNeedsFieldDrop) now recognizes a non-value
	// structural inner — and (b) deep-clone each present box on clone via
	// __promise_structural_clone so the clone owns independent boxes, otherwise the
	// now-active element drop double-frees the aliased boxes.
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		type Widget {
			int id;
			to_string() string { return this.id.to_string(); }
		}
		show(int n) Showable { return Widget(id: n); }
		main() {
			Showable?[] v = [];
			v.push(show(1));
			Showable?[] c = v.clone();
		}
	`)
	// The RTTI structural drop/clone helpers are emitted and referenced for the
	// Optional-wrapped structural element.
	codegentest.AssertContains(t, ir, "@__promise_structural_drop")
	codegentest.AssertContains(t, ir, "@__promise_structural_clone")
	codegentest.AssertContains(t, ir, "call i8* @__promise_structural_clone")
}

func TestVectorOptionalStructuralElementReadDupsBox(t *testing.T) {
	// T1291: reading an element out of a Vector[structural?] into an owning
	// Optional local (`z := v[i]`) must deep-clone the present box on read. The
	// element drop is now active, so an aliased read would double-free with the
	// vector's element walk. genVectorIndex must consume the dup-on-read flag and
	// route the present inner through __promise_structural_clone.
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		type Widget {
			int id;
			to_string() string { return this.id.to_string(); }
		}
		show(int n) Showable { return Widget(id: n); }
		main() {
			Showable?[] v = [];
			v.push(show(1));
			z := v[0];
		}
	`)
	// The index read deep-clones the boxed instance rather than aliasing it.
	codegentest.AssertContains(t, ir, "call i8* @__promise_structural_clone")
}

func TestT1341_FixedArrayOptionalStructuralClone(t *testing.T) {
	// T1341: reading an Optional[structural-interface] element out of a fixed-size
	// array (`Showable? g = a[0]`) must deep-clone the {vtable, instance} box on
	// read. Pre-fix genArrayIndex had no dup-on-read branch for Optional[structural],
	// so the extracted optional aliased the array's box and both its owning drop and
	// the array's element drop freed it via __promise_structural_drop -> double free.
	// Mirrors the genVectorIndex T1291 branch.
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			tag() string `+"`"+`abstract;
		}
		type Widget {
			int id;
			tag() string { return "w"; }
		}
		main() {
			Showable?[2] a = [Widget(id: 1), Widget(id: 2)];
			Showable? g = a[0];
		}
	`)
	// The fixed-array index read deep-clones the boxed instance rather than aliasing it.
	codegentest.AssertContains(t, ir, "call i8* @__promise_structural_clone")
}

func TestOptionalParamWrapping(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		foo(int? x) int {
			if x is present { return x; }
			return 0;
		}
		main() {
			int r = foo(4);
		}
	`)
	// The call to foo(4) should pass {i1, i64} not bare i64
	codegentest.AssertContains(t, ir, "call i64 @__user.foo({ i1, i64 }")
	codegentest.AssertNotContains(t, ir, "call i64 @__user.foo(i64 ")
}

func TestOptionalParamOmittedNoneZeroinit(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		foo(int? x) int {
			if x is present { return x; }
			return 0;
		}
		main() {
			int r = foo();
		}
	`)
	// Omitted optional param should pass {i1, i64} zeroinitializer, not bare i1 false
	codegentest.AssertContains(t, ir, "call i64 @__user.foo({ i1, i64 }")
	codegentest.AssertNotContains(t, ir, "call i64 @__user.foo(i1 ")
}

func TestOptionalParamWrappingMethodCall(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Calc {
			add(int? bonus) int {
				if bonus is present { return bonus; }
				return 0;
			}
		}
		main() {
			Calc c = Calc();
			int r = c.add(bonus: 10);
		}
	`)
	// Method call should wrap 10 as {i1, i64}
	codegentest.AssertContains(t, ir, "call i64 @Calc.add(i8*")
	codegentest.AssertNotContains(t, ir, "call i64 @Calc.add(i8*, i64 ")
}

func TestOptionalParamWrappingConstructor(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Widget {
			int value;
			new(~this, int? v) {
				if v is present { this.value = v; }
			}
		}
		main() {
			Widget w = Widget(v: 5);
		}
	`)
	// Constructor new() call should wrap 5 as {i1, i64}
	codegentest.AssertContains(t, ir, "{ i1, i64 }")
}

// B0030: Optional user-defined type in constructor should use {i1, {i8*, i8*}}
func TestOptionalUserTypeFieldInConstructor(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Coord { int x; int y; }
		type Place { string name; Coord? location; }
		main() {
			Place p = Place(name: "home", location: Coord(x: 1, y: 2));
		}
	`)
	// Optional user type field should be {i1, {i8*, i8*}} not {i1, i8*}
	codegentest.AssertContains(t, ir, "{ i1, { i8*, i8* } }")
}

func TestOptionalUserTypeFieldNoneInConstructor(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Coord { int x; int y; }
		type Place { string name; Coord? location; }
		main() {
			Place p = Place(name: "test", location: none);
		}
	`)
	// None for optional user type should produce zeroinitializer of {i1, {i8*, i8*}}
	codegentest.AssertContains(t, ir, "{ i1, { i8*, i8* } } zeroinitializer")
}

// T1175: `f(v[i])` where v is Vector[Optional[heap-user-type]] and f takes a
// consuming `~`/`move` param must deep-clone the element's inner heap instance —
// the returned Optional value struct aliases the vector slot, so the callee's
// consume-drop and v's element-drop otherwise free the same inner instance (UAF).
// maybeEnableDupForMutRefArg now arms dupHeapUserFieldAccess for Optional[heap-
// user] Vector elements (not just bare heap-user); genVectorIndex's T0620 branch
// (dupOptionalVectorElem) does the clone, lowering to a dupHeapValue heapdup.copy.
// Sibling of T0403 (bare heap-user element) and genArrayIndex's fixed-Array path.
func TestT1175VectorOptionalHeapElementCallArgDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Row { string name; }
		take(Row? move r) {}
		test() {
			Row?[] v = [];
			v.push(Row(name: "a" + "b"));
			take(v[0]);
		}
	`)
	fn := codegentest.ExtractFunction(ir, "__user.test")
	codegentest.AssertContains(t, fn, "heapdup.copy")
}

// T1175: `Holder(held: v[i])` — the constructor-field escape sibling. Same aliasing
// double-free: the new field owns the Optional value struct aliasing the vector
// slot. maybeEnableDupForConstructorArg arms the same Optional[heap-user] gate.
func TestT1175VectorOptionalHeapElementConstructorArgDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Row { string name; }
		type Holder { Row? held; }
		test() {
			Row?[] v = [];
			v.push(Row(name: "a" + "b"));
			h := Holder(held: v[0]);
		}
	`)
	fn := codegentest.ExtractFunction(ir, "__user.test")
	codegentest.AssertContains(t, fn, "heapdup.copy")
}

// T1175: the no-drop-but-pal-free leg of the Optional[heap-user] gate. `Tag` is a
// field-less heap-user type with no drop() — it's still pal_malloc'd, so an escaped
// alias of the vector slot pal_free's the same pointer twice. optionalHeapDupElem
// recognizes both the droppable and the no-drop-pal-free inner, so f(v[i]) into a
// consuming param must still emit the deep-clone (dupHeapValue → heapdup.copy).
func TestT1175VectorOptionalNoDropHeapElementCallArgDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Tag {}
		take(Tag? move t) {}
		test() {
			Tag?[] v = [];
			v.push(Tag());
			take(v[0]);
		}
	`)
	fn := codegentest.ExtractFunction(ir, "__user.test")
	codegentest.AssertContains(t, fn, "heapdup.copy")
}

// T0397: `opt := m[k]` where the map value type is a tuple with droppable fields
// must dup the tuple's string fields so opt holds an independent copy.
// Without the dup, opt's bindingDropTuple and the map's element walk double-free
// the same string pointer. genInferredVarDecl sets dupTupleFieldAccess;
// genMethodIndex calls dupTupleValue which emits promise_string_new for string fields.
func TestT0397MapOptionalTupleIndexDupsStringField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			m := map[string, (string, int)]();
			m["a"] = ("hello", 1);
			opt := m["a"];
		}
	`)
	// dupTupleValue emits promise_string_new to clone the string element.
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new")
}

// T0397 (typed path): same dup via genTypedVarDecl's Optional[Tuple] check.
func TestT0397TypedVarDeclMapOptionalTupleDupsStringField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			m := map[string, (string, int)]();
			m["a"] = ("hello", 1);
			(string, int)? opt = m["a"];
		}
	`)
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new")
}

// T0489: c.tup_field = m[k]! must dup the RHS read via dupTupleValue before
// storing. The OptionalUnwrap-of-IndexExpr path goes through genMethodIndex's
// dupTupleFieldAccess consumer (expr.go:7514), which is a different consumer
// than the Vector path covered by TestT0489MemberAssignDupsTupleOnVecToField
// (expr.go:7654). Without this, the field and the map's stored value alias
// the same heap allocations, causing a silent double-free at scope exit.
func TestT0489MemberAssignDupsTupleOnMapUnwrapToField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0489E { (string, int) f; drop(~this) {} }
		test() {
			m := map[string, (string, int)]();
			m["k"] = ("a" + "", 1);
			c := T0489E(f: ("first" + "", 1));
			c.f = m["k"]!;
		}
	`)
	// dupTupleValue emits promise_string_new to clone the string element on read.
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new")
}

// T0741 Part C: an optional closure struct field must drop its env.
// emitOptionalValueDrop now has a *types.Signature case that branches on the
// has-value flag and deep-drops the inner closure's env.
func TestDropOptionalClosureField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type OptCb {
			(() -> int)? cb;
		}
		make_holder(int n) OptCb {
			s := "cap" + "tured";
			return OptCb(cb: move || -> s.len + n);
		}
		main() {
			h := make_holder(5);
		}
	`)
	codegentest.AssertContains(t, ir, "define void @OptCb.drop")
	codegentest.AssertContains(t, ir, "optfield.drop")
	codegentest.AssertContains(t, ir, "closure.env.free")
	codegentest.AssertContains(t, ir, "env.deep_drop")
}

// T0814: force-unwrapping a LOCAL optional closure into a new local (`f := o!`)
// transfers the heap env from `o` to `f`. The fix has two halves:
//  1. claimEnvTemp recurses into the optional-wrapped fat pointer so the lambda's
//     env temp is claimed by `o` (no early env.tmp.drop of the lambda env), and
//  2. neutralizeForceUnwrapSource clears `o`'s present flag (`store i1 false`)
//     so `o`'s optional drop is skipped and only `f` frees the env once.
func TestUnwrapLocalOptionalClosure(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			s := "cap" + "tured";
			(() -> int)? o = move || -> s.len;
			f := o!;
		}
	`)
	// The unwrap path is taken and the source optional's present flag is cleared.
	codegentest.AssertContains(t, ir, "unwrap.ok")
	codegentest.AssertContains(t, ir, "store i1 false")
	// f owns the env and frees it exactly once at scope exit.
	codegentest.AssertContains(t, ir, "env.free")
	// The optdrop for o is registered for the Signature inner case (the env is
	// freed via the closure-env path when present, not leaked).
	codegentest.AssertContains(t, ir, "optdrop.inner")
	codegentest.AssertContains(t, ir, "closure.env.free")
}

// T1234: a discarded call returning an optional closure `(() -> int)?` must drop
// its heap env. The result type is Optional(Signature); dropDiscardedOptional's
// Signature arm branches on the has-value tag, extracts the env pointer, and
// deep-drops it (env.deep_drop) when present. Without this the env leaked.
func TestDiscardOptionalClosureCallDropsEnv(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_maybe(int x)(() -> int)? {
			return move || -> x + 1;
		}
		main() {
			make_maybe(5);
		}
	`)
	// The Signature arm fires: tag branch + presence-guarded env free.
	codegentest.AssertContains(t, ir, "discard.drop")
	codegentest.AssertContains(t, ir, "discard.env.free")
	// The env is deep-dropped (captured value + struct freed), not merely leaked.
	codegentest.AssertContains(t, ir, "env.deep_drop")
}

// T1234: discarding a *place* expression that reads an optional closure (`o;`) is
// a borrow — the binding owns and drops the env at scope exit. isBorrowingPlaceExpr
// must skip the discard-drop path here, else the env is freed twice. Verify no
// discard-drop blocks are emitted for the bare ident discard (the value is still
// dropped once, via its ordinary scope-exit binding drop).
func TestDiscardBoundOptionalClosurePlaceSkipsDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			(() -> int)? o = || -> 42;
			o;
		}
	`)
	// discard.env.free is unique to dropDiscardedOptional's Signature arm; the only
	// optional-closure discard here is the bare `o;` place, so its absence proves the
	// place-skip fired (the env is still freed once via o's scope-exit binding drop).
	codegentest.AssertNotContains(t, ir, "discard.env.free")
}

// B0181: Optional string field access + unwrap should dup to prevent double-free
func TestOptionalStringFieldUnwrapDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper {
			string? opt_name;
		}
		test() {
			w := Wrapper(opt_name: "hello");
			string val = w.opt_name!;
		}
	`)
	// Reading w.opt_name should dup the inner string (via promise_string_new)
	// to prevent double-free between val's drop and Wrapper's synthesized drop
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new(")
}

// B0190: Inline optional unwrap must not track field string as a temp.
// The unwrapped i8* from `w.opt_name!` is a field reference (not a new allocation),
// so tracking it would cause the field's string to be freed at statement end.
func TestInlineOptionalUnwrapNoTempTrack(t *testing.T) {
	// This should compile without errors. At runtime, the inline unwrap
	// must not free the field string as a temp — only Wrapper.drop should.
	codegentest.GenerateIR(t, `
		type Wrapper {
			string? opt_name;
		}
		test() {
			w := Wrapper(opt_name: "hello");
			bool b = w.opt_name! == "hello";
		}
	`)
}

// T0753: the optional-handler unwrap `(o? _ { ... })` on an ident source must
// NOT register the extracted inner value as an owned heap temp. The source
// optional `o` already owns the inner allocation — its own scope drop binding
// frees it exactly once (for an owned optional) or deliberately not at all (for
// a borrow-holding optional, once T0747's isRttiCastBorrow clears o's flag).
// Tracking the extracted heap value as a statement temp double-frees at scope
// exit (`fatal: invalid free (bad header magic)`). Symmetric to the existing
// OptionalUnwrapExpr ident-skip guard (B0287/T0343).
func TestOptionalHandlerUnwrapIdentNoHeapTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type HBox { int n; drop(~this) {} }
		tfn() int {
			HBox? o = HBox(n: 5);
			return (o? _ { return -1; }).n;
		}
		main() { _ := tfn(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.tfn")
	// Legitimate HBox.drop call sites in tfn (4 total): the construction temp's
	// cleanup (dead — flag cleared when moved into o) emitted on both the normal
	// and the unwrap-panic edges (2), plus o's optional-drop emitted on each of
	// the two return edges — the some path and the handler path (2). The extracted
	// inner from the handler must add NO further drop call site — without the fix
	// it tracks a spurious owned temp, raising the count to 5.
	got := strings.Count(fn, "call void @HBox.drop")
	if got != 4 {
		t.Fatalf("expected 4 HBox.drop call sites (no spurious heap temp for the "+
			"handler-extracted inner), got %d:\n%s", got, fn)
	}
}

// T0776: `((o)!).len` on a string? ident source must NOT register the extracted
// i8* inner as an owned statement temp. The source optional `o` already owns
// the inner allocation via its scope drop binding; tracking the extracted ptr
// as a temp emits a second @promise_string_drop call site against the same
// pointer (double-free at runtime). Without the ParenExpr peel in
// genOptionalForceUnwrap, `(o)!` is a *ParenExpr (not *IdentExpr), the
// isIdent check fails, and the spurious temp tracking fires. Mirror of
// TestOptionalHandlerUnwrapIdentNoHeapTemp for the i8*-inner force-unwrap
// branch (the handler form uses a separate site that T0753 already covers).
func TestT0776ParenForceUnwrapStringNoTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		tfn() int {
			string? o = "abc".to_upper();
			return ((o)!).len;
		}
		main() { _ := tfn(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.tfn")
	if fn == "" {
		t.Fatal("expected __user.tfn in IR")
	}
	// Legitimate @promise_string_drop call sites in tfn (3): the to_upper temp's
	// cleanup (dead — flag cleared when moved into o) on the normal path (1),
	// plus o's optional-drop on each of the two return edges (some path and the
	// unwrap-panic edge) (2). The extracted inner from the paren force-unwrap
	// must add NO further drop call site — without the ParenExpr peel a
	// spurious temp tracker fires, raising the count to 4.
	got := strings.Count(fn, "call void @promise_string_drop")
	if got != 3 {
		t.Fatalf("expected 3 @promise_string_drop call sites in tfn (no spurious "+
			"temp for the paren force-unwrap extracted inner), got %d:\n%s", got, fn)
	}
}

// T0776: `((o)!).len` on an int[]? (Vector[int]) ident source must NOT register
// the extracted i8* inner as an owned statement temp. Symmetric to the string
// case; covers the TypVector branch of the type-aware temp tracker.
func TestT0776ParenForceUnwrapVectorNoTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		tfn() int {
			int[]? o = [1, 2].clone();
			return ((o)!).len;
		}
		main() { _ := tfn(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.tfn")
	if fn == "" {
		t.Fatal("expected __user.tfn in IR")
	}
	// The codegen test harness uses an un-monomorphized stdlib stub, so the call
	// target is @Vector.drop (in real builds it's Vector__int.drop). The expected
	// count mirrors the string case: 1 clone-temp cleanup + 2 optional-drops on
	// the two return edges = 3. Without the ParenExpr peel a spurious temp
	// tracker would add a 4th call site.
	got := strings.Count(fn, "call void @Vector.drop")
	if got != 3 {
		t.Fatalf("expected 3 @Vector.drop call sites in tfn (no spurious "+
			"temp for the paren force-unwrap extracted inner), got %d:\n%s", got, fn)
	}
}

// T0776 no-regression: a non-ident source (call returning string?) MUST still
// track the extracted inner as a statement temp — there is no source optional
// alloca with its own scope drop to own the allocation, so without the temp
// the inner leaks. Symmetric to the ident-tracking guard at the call site of
// `(mk_str()!).len` — the peel must not over-broaden the skip.
func TestT0776NonIdentForceUnwrapStringStillTracks(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		mk_str() string? { return "ab".to_upper(); }
		tfn() int {
			return (mk_str()!).len;
		}
		main() { _ := tfn(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.tfn")
	if fn == "" {
		t.Fatal("expected __user.tfn in IR")
	}
	// With a non-ident source there is no source optional alloca → the
	// extracted inner is the *only* owner. trackStringTemp must emit at least
	// one statement-end drop for it; count >= 1 is enough to confirm the gate
	// did not over-broaden to skip non-ident sources too.
	got := strings.Count(fn, "call void @promise_string_drop")
	if got < 1 {
		t.Fatalf("expected >=1 @promise_string_drop call site in tfn (non-ident "+
			"source must still track the extracted inner), got %d:\n%s", got, fn)
	}
}

// T0672: Destructuring a struct field of tuple type with an
// Optional[aggregate] element (`(map[K,V]?, int) pr`) then unwrapping the
// destructured local via if-let must NOT give the unwrapped binding an owning
// drop flag. `(m, n) := h.pr` sources from a MemberExpr → srcOwned=false, so
// `m`/`n` get no drop bindings and correctly alias the Holder-owned heap
// (T0371 borrow model). The bug: nothing recorded that `m` is a borrow, so a
// later `if mm := m` saw a plain ident, isOwnedOptionalExpr returned true, and
// `mm` got an owning `store i1 true, i1* %mm.dropflag` → `mm` and Holder.drop
// both freed the same map → double-free → SIGSEGV. The fix marks each
// borrow-sourced destructured local in matchBorrowedIdents (mirrors the
// T0485/T0512 match-destructure mechanism) so isOwnedOptionalExpr returns
// false and no owning drop binding is registered for `mm`; the map is freed
// exactly once, by Holder.drop. Companion e2e:
// tests/e2e/destructure_field_tuple_optional_test.pr.
func TestDestructureFieldTupleOptionalAggregateNoOwnDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"type Holder { (map[string, string]?, int) pr; }\n"+
		"test() {\n"+
		"  map[string, string] src = map[string, string]();\n"+
		"  map[string, string]? mo = src;\n"+
		"  Holder h = Holder(pr: (mo, 5));\n"+
		"  (m, n) := h.pr;\n"+
		"  if mm := m { int z = mm.len; }\n"+
		"}\n")
	fnStart := strings.Index(ir, "define void @__user.test()")
	if fnStart < 0 {
		t.Fatalf("T0672: could not find @__user.test() in IR")
	}
	fnEnd := strings.Index(ir[fnStart:], "\n}\n")
	testIR := ir[fnStart : fnStart+fnEnd+3]
	// The if-let binding `mm` must NOT get an owning drop flag — the map is
	// owned by Holder and freed by Holder.drop. An owning `mm` drop binding
	// (the buggy behavior) double-frees the map.
	if strings.Contains(testIR, "%mm.dropflag") {
		t.Errorf("T0672: if-let binding `mm` got an owning drop flag (%%mm.dropflag) — the destructured `m` aliases Holder-owned heap (MemberExpr borrow source) and must not transfer ownership; this double-frees the map with Holder.drop")
	}
	// The destructured local `m` itself must stay a borrow (field-sourced
	// destructure registers no drop binding — existing T0371 behavior).
	if strings.Contains(testIR, "%m.dropflag") {
		t.Errorf("T0672: destructured local `m` unexpectedly got a drop flag — a struct-field-sourced tuple destructure must stay a borrow (no drop binding)")
	}
	// Holder.drop must still be the sole owner that frees the map.
	if !strings.Contains(testIR, "call void @Holder.drop") {
		t.Errorf("T0672: Holder.drop must be called to free the field-owned map exactly once")
	}
}

// T0572: Holder with Optional<Enum> field where the inner enum has droppable
// variant data. emitOptionalValueDrop had no enum branch — extractNamed
// returns nil for enums, so the has-value path fell through to the default
// `return` and skipped cleanup. The new branch must guard on the optional's
// has-value flag and call the enum's drop function.
func TestDropTypeWithOptionalEnumField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource { int id; drop(~this) { } }
		enum Maybe {
			Some(Resource value),
			Nothing,
		}
		type Holder {
			Maybe? m;
		}
		main() {
			j := Maybe.Some(Resource(id: 1));
			c := Holder(m: j);
		}
	`)
	codegentest.AssertContains(t, ir, "define void @Holder.drop")
	// The has-value guard for the optional enum field.
	codegentest.AssertContains(t, ir, "optfield.drop")
	codegentest.AssertContains(t, ir, "optfield.skip")
	// The enum's drop is invoked from inside the has-value branch.
	codegentest.AssertContains(t, ir, "call void @Maybe.drop")
}

// T0572: Holder with Optional<FieldlessEnum> field — the !needsDrop short-
// circuit must fire so no spurious cleanup branches are emitted for the
// fieldless enum slot. The holder itself has a sibling droppable field
// (Resource) so its synth drop body is generated and walks all fields,
// reaching the Optional<FieldlessEnum> branch. The short-circuit ensures
// only the sibling Resource's drop appears in the holder body — no
// optfield.drop block for the Color slot.
func TestDropTypeWithOptionalFieldlessEnumFieldShortCircuits(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource { int id; drop(~this) { } }
		enum Color {
			Red,
			Green,
			Blue,
		}
		type Holder {
			Color? c;
			Resource r;
		}
		main() {
			h := Holder(c: Color.Red, r: Resource(id: 1));
		}
	`)
	codegentest.AssertContains(t, ir, "define void @Holder.drop")
	// The Resource sibling is dropped — confirms the holder synth drop runs.
	codegentest.AssertContains(t, ir, "call void @Resource.drop")
	// Fieldless Color enum has no drop fn, so the short-circuit must skip
	// emitting any call to a Color drop. (No @Color.drop function exists.)
	codegentest.AssertNotContains(t, ir, "call void @Color.drop")
}

// B0293: CastExpr as! on optional must neutralize source to prevent double-free.
func TestAsBangOptionalNeutralizesSource(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Point { int x; int y; }
		test() {
			Point? p = Point(x: 8, y: 9);
			Point q = p as! Point;
		}
	`)
	// After as! unwrap into q, the optional p's present flag must be set to false.
	// This prevents both p's optional drop and q's drop from freeing the same instance.
	codegentest.AssertContains(t, ir, "store i1 false")
}

// B0293: Optional handler (p? _ { fallback }) must neutralize source.
func TestOptionalHandlerNeutralizesSource(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Point { int x; int y; }
		test() {
			Point? p = Point(x: 5, y: 6);
			Point q = p? _ { Point(x: 0, y: 0); };
		}
	`)
	// After handler unwrap into q, the optional p's present flag should be set to false.
	codegentest.AssertContains(t, ir, "store i1 false")
}

// B0299: Inline optional string field unwrap must not track the string as a
// statement-end temp. The owner's drop handles the string's lifetime.
// Without this fix, statement-end cleanup frees the original string from
// the field, then Wrapper.drop frees it again → double-free.
func TestOptionalFieldStringInlineUnwrapNoTempTrack(t *testing.T) {
	res := codegentest.CompileResult(t, `
		type Wrapper { string? opt_name; }
		test() {
			Wrapper w = Wrapper(opt_name: "hello");
			assert(w.opt_name! == "hello", "ok");
		}
	`)
	ir := res.Module.String()
	// Extract just the test function's IR
	fnStart := strings.Index(ir, "define void @__user.test()")
	fnEnd := strings.Index(ir[fnStart:], "\n}\n")
	testIR := ir[fnStart : fnStart+fnEnd+3]
	// The inline unwrap must NOT generate a strdup/string_new call —
	// no dup is needed for inline access, and the string must not be
	// tracked as a temp (optionalFieldString flag suppresses tracking).
	codegentest.AssertNotContains(t, testIR, "promise_string_new")
}

// T0540: same deep-dup requirement for the Optional[Vector] field branch.
func TestB0219OptionalVecFieldDeepDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type OptHolder {
			string[]? field;
			drop(~this) {}
		}
		main() {
			v1 := string[]();
			h := OptHolder(field: v1);
			v := h.field;
		}
	`)
	codegentest.AssertContains(t, ir, "vecdup.copy")
	codegentest.AssertContains(t, ir, "vecdup_str.head")
	codegentest.AssertContains(t, ir, "promise_string_new")
}

// T0939: binding an Optional[Vector]-with-droppable-elements field to a plain
// container local must null-guard the element-clone loop. On the optional's `none`
// path field 1 is null; the dup of null is null, and the unguarded clone loop
// (loadVectorLen) would dereference it → segfault. The guard emits veccloneopt.
// blocks that skip the loop when the dup is null.
func TestT0939OptionalVecFieldCloneNullGuard(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type SVBox { string[]? v; drop(~this) {} }
		main() {
			bx := SVBox(v: none);
			b := string[]();
			x := bx.v ?: b;
		}
	`)
	codegentest.AssertContains(t, ir, "veccloneopt.do")
	codegentest.AssertContains(t, ir, "veccloneopt.merge")
}

// T0939 (genArrayIndex call site): the same null-guard fix also covers the
// Optional[Vector] element path of `genArrayIndex`. Indexing an array of
// `string[]?` and binding the elvis result to a plain `string[]` must emit the
// veccloneopt. guard blocks for the element-clone loop (the slot's inner buffer is
// null on the `none` path). Pins the second call site at the IR level.
func TestT0939OptionalVecArrayIndexCloneNullGuard(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string[]? a0 = none;
			string[]? a1 = none;
			string[]?[2] arr = [a0, a1];
			b := string[]();
			x := arr[0] ?: b;
		}
	`)
	codegentest.AssertContains(t, ir, "veccloneopt.do")
	codegentest.AssertContains(t, ir, "veccloneopt.merge")
}

func TestDiscardedStringPopDropsOptionalInner(t *testing.T) {
	// B0196: When v.pop() result is discarded and element type is string,
	// the string inside the optional must be dropped.
	ir := codegentest.GenerateIR(t, `
		main() {
			string[] v = [];
			v.push("hello");
			v.pop();
		}
	`)
	// Should have a discard.drop block that calls promise_string_drop
	codegentest.AssertContains(t, ir, "discard.drop")
	codegentest.AssertContains(t, ir, "discard.skip")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop(i8*")
}

// --- Optional interpolation tests ---

func TestStringInterpolationOptionalPresent(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int? x = 42;
			string s = "{x}";
		}
	`)
	// Should branch on presence flag
	codegentest.AssertContains(t, ir, "interp.some")
	codegentest.AssertContains(t, ir, "interp.none")
	codegentest.AssertContains(t, ir, "interp.merge")
	// Should call int_to_string in the some branch
	codegentest.AssertContains(t, ir, "promise_int_to_string")
}

func TestStringInterpolationOptionalNone(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int? x = none;
			string s = "{x}";
		}
	`)
	codegentest.AssertContains(t, ir, "interp.some")
	codegentest.AssertContains(t, ir, "interp.none")
}

func TestStringInterpolationOptionalString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string? name = "Alice";
			string s = "hello {name}";
		}
	`)
	codegentest.AssertContains(t, ir, "interp.some")
	codegentest.AssertContains(t, ir, "interp.none")
}

// T0421: Optional enum interpolation emits interp.some/none wrapper + inner enum switch.
func TestStringInterpolationEnumOptional(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color { Red, Green, Blue }
		main() {
			Color? c = Color.Green;
			string s = "{c}";
		}
	`)
	// Optional wrapping blocks
	codegentest.AssertContains(t, ir, "interp.some")
	codegentest.AssertContains(t, ir, "interp.none")
	// Enum switch inside the some branch
	codegentest.AssertContains(t, ir, "switch i32")
	codegentest.AssertContains(t, ir, "enum.interp.Red")
	codegentest.AssertContains(t, ir, "enum.interp.Green")
	codegentest.AssertContains(t, ir, "enum.interp.Blue")
	// "none" string for absent case
	codegentest.AssertContains(t, ir, `"none"`)
}

// --- Optional narrowing codegen tests ---

func TestOptionalTruthinessNarrowingCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string? cc = "hello";
			if cc {
				string s = cc;
			}
		}
	`)
	// Should have narrow blocks (not regular if blocks)
	codegentest.AssertContains(t, ir, "narrow.then")
	codegentest.AssertContains(t, ir, "narrow.end")
	// In the then block, should extractvalue the inner string from the optional
	codegentest.AssertContains(t, ir, "extractvalue { i1, i8* }")
}

func TestIsPresentNarrowingCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int? x = 42;
			if x is present {
				int n = x;
			}
		}
	`)
	codegentest.AssertContains(t, ir, "narrow.then")
	// Should extract the inner i64 from { i1, i64 }
	codegentest.AssertContains(t, ir, "extractvalue { i1, i64 }")
}

func TestOptionalNarrowingWithElseCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int? x = 42;
			if x {
				int n = x;
			} else {
				int n = 0;
			}
		}
	`)
	codegentest.AssertContains(t, ir, "narrow.then")
	codegentest.AssertContains(t, ir, "narrow.else")
	codegentest.AssertContains(t, ir, "narrow.end")
}

func TestUninitOptionalVarCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int? x;
			string s = "{x}";
		}
	`)
	codegentest.AssertContains(t, ir, "zeroinitializer")
	codegentest.AssertContains(t, ir, "interp.none")
}

func TestOptionalChainUserType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Cfg {
			int port;
		}
		main() {
			Cfg? c = Cfg(port: 8080);
			int? p = c?.port;
		}
	`)
	codegentest.AssertContains(t, ir, "optchain.some")
	codegentest.AssertContains(t, ir, "optchain.none")
}

// T1421: `opt?.getter` for a user-declared getter must emit a getter call on
// the present path (not a raw struct-field load, which read the int as a
// pointer and segfaulted at 0x8).
func TestOptionalChainUserGetter(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box {
			int n;
			get size int => this.n;
		}
		main() {
			Box? b = Box(n: 5);
			int? s = b?.size;
		}
	`)
	codegentest.AssertContains(t, ir, "optchain.some")
	codegentest.AssertContains(t, ir, "optchain.none")
	// Present path routes through the getter method, not a bare field load.
	codegentest.AssertContains(t, ir, "call i64 @Box.size(")
}

// T1421: `opt?.len` for a built-in string getter must not ICE — string.len is
// synthesized inline (not a declared getter), so it must flow through the full
// member-access path.
func TestOptionalChainNativeStringLen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string? name = "promise";
			int? size = name?.len;
		}
	`)
	codegentest.AssertContains(t, ir, "optchain.some")
	codegentest.AssertContains(t, ir, "optchain.none")
}

// T1421: a virtual getter reached through `?.` on a base-typed optional holding a
// subtype must dispatch through the vtable (indirect `call i8* %...`), not a
// direct call to the base method. The pre-T1421 raw-GEP path could not do vtable
// dispatch; the delegation to genMemberExpr restores it.
func TestOptionalChainVirtualGetter(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Animal {
			int legs;
			get sound string => "generic";
		}
		type Dog is Animal {
			get sound string => "woof";
		}
		main() {
			Animal? a = Dog(legs: 4);
			string? s = a?.sound;
		}
	`)
	codegentest.AssertContains(t, ir, "optchain.some")
	codegentest.AssertContains(t, ir, "optchain.none")
	codegentest.AssertContains(t, ir, "@promise_vtable_Animal")
	// Dispatch is indirect through the vtable, not a direct call to Animal.sound.
	codegentest.AssertContains(t, ir, "call i8* %")
	codegentest.AssertNotContains(t, ir, "call i8* @Animal.sound(")
}

// T1421: a plain field read through `?.` on a base-typed optional holding a
// subtype must resolve against the concrete instance layout (offset load), not
// crash — the field path also flows through the delegated member access.
