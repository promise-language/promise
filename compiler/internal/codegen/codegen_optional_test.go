package codegen

import (
	"fmt"
	"strings"
	"testing"
)

// T0761: RTTI cast whose subject is itself an Optional. genCastExpr used to
// treat the {i1,{i8*,i8*}} optional as a bare value struct and panic; it now
// branches to genOptionalCastExpr, which unwraps field 1 before promise_type_is.
func TestOptionalSubjectForceCast(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "optcast.present")
	assertContains(t, ir, "optcast.nonepanic")
	assertContains(t, ir, "call i32 @promise_type_is(")
}

func TestOptionalSubjectOptionalCast(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "optcast.check")
	assertContains(t, ir, "optcast.some")
	assertContains(t, ir, "optcast.none")
	assertContains(t, ir, "optcast.merge")
	assertContains(t, ir, "call i32 @promise_type_is(")
}

// T0850: an RTTI optional cast whose subject is a BORROWED optional (`T?&`,
// here `Ref[Base?].borrow`) used to crash codegen — the non-optional RTTI path
// fed the loaded `{i1,{i8*,i8*}}` optional to wrapOptional → insertvalue/store
// type mismatch panic. The fix peels the SharedRef/MutRef and routes through
// genOptionalCastExpr (borrowSource): it must no longer panic, must emit the
// optcast blocks (proving it took the optional-subject path), and must dup the
// inner (heapdup.copy) since the borrow aliases the Arc's external-owned payload.
func TestBorrowedOptionalSubjectCast(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "optcast.check")
	assertContains(t, ir, "optcast.some")
	assertContains(t, ir, "optcast.none")
	assertContains(t, ir, "optcast.merge")
	assertContains(t, ir, "call i32 @promise_type_is(")
	// The borrowed inner is duped into an owned copy before the RTTI dispatch.
	assertContains(t, extractDefine(ir, ".goroutine.main"), "heapdup.copy")
}

// T0850: a forced borrowed-optional cast (`Ref[T?].borrow as! U`) takes the
// optional-subject force path (panic on none/mismatch, return the duped inner).
func TestBorrowedOptionalSubjectForceCast(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "optcast.present")
	assertContains(t, ir, "optcast.nonepanic")
	assertContains(t, ir, "optcast.mismatch")
	assertContains(t, extractDefine(ir, ".goroutine.main"), "heapdup.copy")
}

// T0761: an Optional cast whose subject is a container element (`v[i]`) aliases
// the vector's bucket, so genOptionalCastExpr must dup the inner — otherwise both
// the cast result and the vector free it (double-free) / the result leaks. The
// dup emits a heapdup.copy block before the cast's RTTI check.
func TestOptionalSubjectIndexCastDups(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, extractDefine(ir, ".goroutine.main"), "heapdup.copy")
	assertContains(t, ir, "optcast.present")
}

// T0761: a scalar optional subject (`int? as f64`) has a bare scalar inner, not a
// value struct — genOptionalCastExpr must take the scalar path (emitScalarCast),
// not the RTTI path (which would extractvalue a non-aggregate and panic).
func TestOptionalSubjectScalarCast(t *testing.T) {
	// Force: unwrap (panic on none) then sitofp the inner int to f64.
	irForce := generateIR(t, `
		main() {
			int? x = 5;
			f := x as! f64;
			_ := f;
		}
	`)
	assertContains(t, irForce, "optcast.present")
	assertContains(t, irForce, "sitofp") // scalar conversion, not an RTTI dispatch
	// Optional: present → some(convert); absent → none.
	irOpt := generateIR(t, `
		main() {
			int? x = 5;
			f64? f = x as f64;
			if f { }
		}
	`)
	assertContains(t, irOpt, "optcast.some")
	assertContains(t, irOpt, "optcast.none")
	assertContains(t, irOpt, "sitofp") // scalar conversion, not an RTTI dispatch
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
	irOpt := generateIR(t, fmt.Sprintf(src, `Der? d = h.slot as Der; if d { }`))
	assertContains(t, irOpt, "optcast.check")
	assertContains(t, irOpt, "optcast.some")
	assertContains(t, irOpt, "call i32 @promise_type_is(")
	assertNotContains(t, extractDefine(irOpt, ".goroutine.main"), "heapdup.copy") // owned-local member is NOT duped
	// Force: same source shape via the `as!` path.
	irForce := generateIR(t, fmt.Sprintf(src, `d := h.slot as! Der; _ := d.tag();`))
	assertContains(t, irForce, "optcast.present")
	assertContains(t, irForce, "optcast.nonepanic")
	assertNotContains(t, extractDefine(irForce, ".goroutine.main"), "heapdup.copy")
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
	irForce := generateIR(t, fmt.Sprintf(src,
		`probe(this) string { c := this.slot as! Der; return c.tag(); }`))
	assertContains(t, extractFunction(irForce, "Holder.probe"), "heapdup.copy")
	assertContains(t, irForce, "optcast.present")
	// Optional through borrowed this: duped AND registered as a heap temp (result
	// owns the duped inner; freed on present+mismatch).
	irOpt := generateIR(t, fmt.Sprintf(src,
		`probe(this) bool { Der? d = this.slot as Der; if d { return true; } return false; }`))
	assertContains(t, extractFunction(irOpt, "Holder.probe"), "heapdup.copy")
	assertContains(t, irOpt, "optcast.check")
	assertContains(t, irOpt, "optcast.some")
}

// T0761: an Optional `as` cast on a call-result TEMP source. The temp owns its
// inner outright (no source binding to neutralize), so optionalCastResultOwnsInner
// returns true via the default arm and the `as` path registers the inner as a heap
// temp inside checkBlock — freed on present+mismatch, claimed by the binding on
// match. (The only other Go `as` test uses an ident source, which skips this block.)
func TestOptionalSubjectTempOptionalCast(t *testing.T) {
	ir := generateIR(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		make_opt(string move n) Base? { Base s = Der(name: n); return s; }
		main() {
			Der? d = make_opt("x") as Der;
			if d { }
		}
	`)
	assertContains(t, ir, "optcast.check")
	assertContains(t, ir, "optcast.some")
	assertContains(t, ir, "optcast.none")
	// Temp source is owned outright — no dup (scoped to @main; stdAll clone funcs
	// emit heapdup.copy elsewhere), but heap-temp tracked for present+mismatch.
	assertNotContains(t, extractDefine(ir, ".goroutine.main"), "heapdup.copy")
}

// T0761: a paren-wrapped Optional cast source (`(oo) as Der`). The `as` move
// path's neutralizeOptionalCastSource must peel the ParenExpr before clearing the
// underlying ident's present flag (otherwise the source's drop double-frees the
// inner). Compiles cleanly and takes the optcast path.
func TestOptionalSubjectParenSourceCast(t *testing.T) {
	ir := generateIR(t, `
		type Base { string name; tag(this) string `+"`"+`abstract; }
		type Der is Base { tag(this) string { return "d"; } }
		main() {
			Base b = Der(name: "x");
			Base? oo = b;
			Der? d = (oo) as Der;
			if d { }
		}
	`)
	assertContains(t, ir, "optcast.check")
	assertContains(t, ir, "optcast.some")
	assertContains(t, ir, "call i32 @promise_type_is(")
}

func TestBangUnwrapInsideHandler(t *testing.T) {
	ir := generateIR(t, `
		parse!(string s) int { return 0; }
		foo() {
			parse("x") ? e { int v = parse("0")?!; };
		}
		main() { }
	`)
	// Should have both handler and panic-on-unwrap blocks
	assertContains(t, ir, "error.handler")
	assertContains(t, ir, "error.panic")
}

// --- Part B: Optional tests ---

func TestOptionalNone(t *testing.T) {
	ir := generateIR(t, `main() { int? x = none; }`)
	// Should alloca { i1, i64 } and zero-initialize
	assertContains(t, ir, "alloca { i1, i64 }")
	assertContains(t, ir, "zeroinitializer")
}

func TestOptionalSome(t *testing.T) {
	ir := generateIR(t, `main() { int? x = 42; }`)
	// Should alloca { i1, i64 } and wrap: { true, 42 }
	assertContains(t, ir, "alloca { i1, i64 }")
	assertContains(t, ir, "insertvalue { i1, i64 }")
	assertContains(t, ir, "i1 true")
}

func TestOptionalStringNone(t *testing.T) {
	ir := generateIR(t, `main() { string? x = none; }`)
	assertContains(t, ir, "alloca { i1, i8* }")
	assertContains(t, ir, "zeroinitializer")
}

func TestOptionalVariable(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			int? y = x;
		}
	`)
	// Should load/store { i1, i64 } struct
	assertContains(t, ir, "load { i1, i64 }")
	assertContains(t, ir, "store { i1, i64 }")
}

// T1160: a callee that is neither an ident, a member, nor a generic instantiation —
// here a force-unwrapped optional closure — materializes its fat pointer from an
// expression, so isClosureValueCallee falls through to its terminal `return true`.
// The callee's env may own the closure it hands back, so the result is left alone.
func TestClosureUnwrappedOptionalCalleeResultNotTracked(t *testing.T) {
	ir := generateIR(t, `
		call_optional((() -> () -> int)? o) { o!(); }
		main() {}
	`)
	body := extractFunction(ir, "__user.call_optional")
	assertContains(t, body, "define") // guard: the body was actually found
	assertNotContains(t, body, "env.tmp.drop")
}

func TestFunctionTypedFieldOptionalReturn(t *testing.T) {
	// Critical for _FnIter[T] pattern where _next is () -> T?
	ir := generateIR(t, `
		type Supplier {
			() -> int? produce;
			next() int? { return this.produce(); }
		}
		main() {
			s := Supplier(produce: || -> int? { return 42; });
		}
	`)
	assertContains(t, ir, "@Supplier.next")
	// Should call through indirect call path (fat pointer)
	assertContains(t, ir, "extractvalue { i8*, i8* }")
}

// ================================================================
// Stage 8h — Optional Patterns, String Interpolation & Expression Completeness
// ================================================================

// --- Part A: If-unwrap ---

func TestIfUnwrap(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			if val := x {
				int y = val + 1;
			}
		}
	`)
	assertContains(t, ir, "extractvalue")
	assertContains(t, ir, "ifunwrap.then")
	assertContains(t, ir, "ifunwrap.end")
}

func TestIfUnwrapElse(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = none;
			if val := x {
				int y = val;
			} else {
				int z = 0;
			}
		}
	`)
	assertContains(t, ir, "ifunwrap.then")
	assertContains(t, ir, "ifunwrap.else")
	assertContains(t, ir, "ifunwrap.end")
}

// --- Part B: While-unwrap ---

func TestWhileUnwrap(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			while val := x {
				break;
			}
		}
	`)
	assertContains(t, ir, "whileunwrap.header")
	assertContains(t, ir, "whileunwrap.body")
	assertContains(t, ir, "whileunwrap.exit")
	assertContains(t, ir, "extractvalue")
}

func TestWhileUnwrapBreak(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 10;
			while val := x {
				break;
			}
		}
	`)
	// break should jump to exit block
	assertContains(t, ir, "br label %whileunwrap.exit")
}

// --- Part C: Optional chaining ---

func TestOptionalChain(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		main() {
			Dog? d = Dog(age: 3);
			int? a = d?.age;
		}
	`)
	assertContains(t, ir, "optchain.some")
	assertContains(t, ir, "optchain.none")
	assertContains(t, ir, "optchain.merge")
}

func TestOptionalChainNone(t *testing.T) {
	ir := generateIR(t, `
		type Dog { int age; }
		main() {
			Dog? d = none;
			int? a = d?.age;
		}
	`)
	assertContains(t, ir, "optchain.some")
	assertContains(t, ir, "optchain.none")
	assertContains(t, ir, "phi")
}

// T1297: a fixed-array literal with an Optional element type Some-wraps each
// non-optional element into the {i1,T} slot via insertvalue.
func TestT1297_FixedArrayOptionalElementWrap(t *testing.T) {
	ir := generateIR(t, `
		main() { int?[3] x = [1, none, 3]; }
	`)
	// Elements are wrapped into the { i1, i64 } optional element struct.
	assertContains(t, ir, "insertvalue { i1, i64 }")
	// Slot type is the optional element struct.
	assertContains(t, ir, "alloca [3 x { i1, i64 }]")
}

// T1297: a vector literal with an Optional element type takes the heap path
// (not the static .rodata path) so each element can be Some-wrapped, even when
// every element is a compile-time constant.
func TestT1297_OptionalElementVectorTakesHeapPath(t *testing.T) {
	ir := generateIR(t, `
		main() { int?[] x = [1, 2, 3]; }
	`)
	// Heap allocation (not a .rodata static vector global).
	assertContains(t, ir, "call i8* @pal_alloc(")
	// Elements Some-wrapped into the { i1, i64 } optional element struct.
	assertContains(t, ir, "insertvalue { i1, i64 }")
}

// T0579: Array field with Optional<HeapUser> element — exact repro from the
// bug. The Optional inside the array carries the full value struct, not
// bare i8*.
func TestFixedArrayFieldOptionalHeapUserElement(t *testing.T) {
	ir := generateIR(t, `
		type _BoxA { int n; drop(~this) {} }
		type _HolderArr { _BoxA?[2] data; }
		main() {
			_BoxA? a = _BoxA(n: 7);
			_BoxA? b = _BoxA(n: 8);
			_HolderArr h = _HolderArr(data: [a, b]);
		}
	`)
	// Field slot should hold {i1, value_struct} per element, not {i1, i8*}.
	assertContains(t, ir, "[2 x { i1, { i8*, i8* } }]")
}

// T0583: Optional<HeapUser> element — overwrite must run the Optional drop
// dispatcher on the previous slot, dropping the inner instance when present.
func TestFixedArrayIndexAssignDropsOldOptionalHeapUser(t *testing.T) {
	ir := generateIR(t, `
		type _Box { int n; drop(~this) {} }
		main() {
			_Box? a = _Box(n: 1);
			_Box? b = _Box(n: 2);
			_Box?[2] arr = [a, b];
			_Box? c = _Box(n: 3);
			arr[1] = c;
		}
	`)
	assertContains(t, ir, "arrassign.ok")
	// The Optional's drop dispatcher checks presence then drops the inner _Box.
	assertContains(t, ir, "call void @_Box.drop")
}

// T0599: bare-T RHS assigned to an Optional<T> fixed-array slot. The
// IndexExpr-LHS path in genAssignStmt had no Optional-wrap (the MemberExpr
// and IdentExpr paths did), so genArrayIndexAssign's NewStore got a bare T
// against a {i1, T}* slot and panicked with "store operands are not
// compatible". Pre-fix, generateIR() here panics and fails the test; post-fix
// the bare _Box is wrapped into the {i1, {i8*, i8*}} Optional before the store.
func TestFixedArrayIndexAssignBareToOptionalHeapUser(t *testing.T) {
	ir := generateIR(t, `
		type _Box { int n; drop(~this) {} }
		main() {
			_Box? a = _Box(n: 1);
			_Box? b = _Box(n: 2);
			_Box?[2] arr = [a, b];
			arr[0] = _Box(n: 99);
		}
	`)
	assertContains(t, ir, "arrassign.ok")
	// The bare _Box ctor result is wrapped into the Optional struct (present
	// flag set, then the value-struct inserted) before the slot store.
	assertContains(t, ir, "insertvalue { i1, { i8*, i8* } } undef, i1 true, 0")
}

// T0599: bare string literal assigned to a string? fixed-array slot — the
// string-temp claim must run BEFORE the Optional wrap (val identity changes
// after wrapOptional). Pre-fix this panicked with "store operands are not
// compatible: src=i8*; dst={i1,i8*}*".
func TestFixedArrayIndexAssignBareToOptionalString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? oa = "a";
			string? ob = "b";
			string?[2] arr = [oa, ob];
			arr[0] = "new";
		}
	`)
	assertContains(t, ir, "arrassign.ok")
	assertContains(t, ir, "insertvalue { i1, i8* } undef, i1 true, 0")
}

func TestFixedArrayIndexDupsOptionalHeapUser(t *testing.T) {
	ir := generateIR(t, `
		type _B { int n; drop(~this) {} }
		main() {
			_B? a = _B(n: 1);
			_B? b = _B(n: 2);
			_B?[2] arr = [a, b];
			_B? x = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")
	// Optional[heap-user] dup path: extract inner, dupHeapValue, insert back.
	assertContains(t, ir, "call i8* @pal_alloc(")
	assertContains(t, ir, "call void @llvm.memcpy")
}

func TestFixedArrayIndexDupsOptionalString(t *testing.T) {
	// Optional[string]: extract inner, dup the string, insert back.
	ir := generateIR(t, `
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
	assertContains(t, ir, "arridx.ok")
	assertContains(t, ir, "call i8* @promise_string_new(")
}

// --- Part D: is/as expression tests ---

func TestIsPresent(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			bool b = x is present;
		}
	`)
	// Should extract the i1 flag from the optional struct
	assertContains(t, ir, "extractvalue { i1, i64 }")
}

func TestOptionalHandlerRecovery(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = none;
			int y = x ? _ { 0; };
		}
	`)
	assertContains(t, ir, "opt.none")
	assertContains(t, ir, "opt.some")
	assertContains(t, ir, "opt.merge")
}

func TestOptionalForceUnwrapBang(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			int y = x!;
		}
	`)
	assertContains(t, ir, "unwrap.ok")
	assertContains(t, ir, "unwrap.panic")
	assertContains(t, ir, "promise_panic")
}

// T0817: Directly invoking a force-unwrapped optional closure `o!()` must
// compile (no "unsupported callee type *ast.OptionalUnwrapExpr" panic) and
// emit an indirect call through the materialized {fn, env} fat pointer.
func TestT0817OptionalUnwrapClosureCall(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "cap" + "tured";
			(() -> int)? o = move || -> s.len;
			int n = o!();
		}
	`)
	// Indirect call through a loaded function pointer (fat-pointer dispatch),
	// not a named direct call.
	assertContains(t, ir, "call i64 %")
}

func TestOptionalForceUnwrapAsBang(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			int y = x as! int;
		}
	`)
	// Should extractvalue to check flag, then extractvalue to get inner value
	assertContains(t, ir, "unwrap.ok")
	assertContains(t, ir, "unwrap.panic")
	assertContains(t, ir, "promise_panic")
}

// B0301: Optional force-unwrap used as a constructor argument must neutralize
// the source optional's present flag to prevent double-free.
func TestOptionalForceUnwrapConstructorArg(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "unwrap.ok")
	// The constructor should store the unwrapped value into the Outer instance
	assertContains(t, ir, "store { i8*, i8* }")
}

func TestIsPresentStringOptional(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? x = "hello";
			bool b = x is present;
			bool c = x is absent;
		}
	`)
	// Should extractvalue on { i1, i8* } optional
	assertContains(t, ir, "extractvalue { i1, i8* }")
}

func TestIsTypeOnOptionalPrimitive(t *testing.T) {
	// B0029: `is` type check on optional primitives should not try to extract vtable.
	// Before the fix, this panicked with "bitcast i64 to {i8*}*".
	ir := generateIR(t, `
		main() {
			int? x = 42;
			bool b = x is int;
		}
	`)
	// Should extract i1 flag from {i1, i64} — presence check only
	assertContains(t, ir, "extractvalue { i1, i64 }")
}

func TestIsTypeOnOptionalUserType(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "promise_type_is")
}

func TestIsTypeOnOptionalString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? s = "hello";
			bool b = s is string;
		}
	`)
	// String optional: {i1, i8*} — should extract flag only, no RTTI
	assertContains(t, ir, "extractvalue { i1, i8* }")
}

func TestIsTypeOnOptionalEnum(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		main() {
			Color? c = Color.Red;
			bool b = c is Color;
		}
	`)
	// Enum optional: should extract flag only, no RTTI
	assertContains(t, ir, "extractvalue")
}

func TestIsTypeOnOptionalBoolFalse(t *testing.T) {
	// false is a valid present value — is bool must return true
	ir := generateIR(t, `
		main() {
			bool? b = false;
			bool ok = b is bool;
		}
	`)
	assertContains(t, ir, "extractvalue { i1, i8 }")
}

func TestStructuralAdapterExtraOptionalParam(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "Doc.print$view_adapt")
	assertContains(t, ir, "@promise_vtable_Doc_as_Printable")
}

func TestStructuralAdapterNonOptionalToOptionalReturn(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "Always.find$view_adapt")
}

func TestVectorOptionalStructuralElementDropAndClone(t *testing.T) {
	// T1291: a Vector[structural-interface?] (Optional-wrapped structural element)
	// must (a) drop each present heap-boxed element through __promise_structural_drop
	// at vector drop — the drop gate (typeNeedsFieldDrop) now recognizes a non-value
	// structural inner — and (b) deep-clone each present box on clone via
	// __promise_structural_clone so the clone owns independent boxes, otherwise the
	// now-active element drop double-frees the aliased boxes.
	ir := generateIR(t, `
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
	assertContains(t, ir, "@__promise_structural_drop")
	assertContains(t, ir, "@__promise_structural_clone")
	assertContains(t, ir, "call i8* @__promise_structural_clone")
}

func TestVectorOptionalStructuralElementReadDupsBox(t *testing.T) {
	// T1291: reading an element out of a Vector[structural?] into an owning
	// Optional local (`z := v[i]`) must deep-clone the present box on read. The
	// element drop is now active, so an aliased read would double-free with the
	// vector's element walk. genVectorIndex must consume the dup-on-read flag and
	// route the present inner through __promise_structural_clone.
	ir := generateIR(t, `
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
	assertContains(t, ir, "call i8* @__promise_structural_clone")
}

func TestT1341_FixedArrayOptionalStructuralClone(t *testing.T) {
	// T1341: reading an Optional[structural-interface] element out of a fixed-size
	// array (`Showable? g = a[0]`) must deep-clone the {vtable, instance} box on
	// read. Pre-fix genArrayIndex had no dup-on-read branch for Optional[structural],
	// so the extracted optional aliased the array's box and both its owning drop and
	// the array's element drop freed it via __promise_structural_drop -> double free.
	// Mirrors the genVectorIndex T1291 branch.
	ir := generateIR(t, `
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
	assertContains(t, ir, "call i8* @__promise_structural_clone")
}

func TestOptionalParamWrapping(t *testing.T) {
	ir := generateIR(t, `
		foo(int? x) int {
			if x is present { return x; }
			return 0;
		}
		main() {
			int r = foo(4);
		}
	`)
	// The call to foo(4) should pass {i1, i64} not bare i64
	assertContains(t, ir, "call i64 @__user.foo({ i1, i64 }")
	assertNotContains(t, ir, "call i64 @__user.foo(i64 ")
}

func TestOptionalParamOmittedNoneZeroinit(t *testing.T) {
	ir := generateIR(t, `
		foo(int? x) int {
			if x is present { return x; }
			return 0;
		}
		main() {
			int r = foo();
		}
	`)
	// Omitted optional param should pass {i1, i64} zeroinitializer, not bare i1 false
	assertContains(t, ir, "call i64 @__user.foo({ i1, i64 }")
	assertNotContains(t, ir, "call i64 @__user.foo(i1 ")
}

func TestOptionalParamWrappingMethodCall(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "call i64 @Calc.add(i8*")
	assertNotContains(t, ir, "call i64 @Calc.add(i8*, i64 ")
}

func TestOptionalParamWrappingConstructor(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "{ i1, i64 }")
}

// B0030: Optional user-defined type in constructor should use {i1, {i8*, i8*}}
func TestOptionalUserTypeFieldInConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Coord { int x; int y; }
		type Place { string name; Coord? location; }
		main() {
			Place p = Place(name: "home", location: Coord(x: 1, y: 2));
		}
	`)
	// Optional user type field should be {i1, {i8*, i8*}} not {i1, i8*}
	assertContains(t, ir, "{ i1, { i8*, i8* } }")
}

func TestOptionalUserTypeFieldNoneInConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Coord { int x; int y; }
		type Place { string name; Coord? location; }
		main() {
			Place p = Place(name: "test", location: none);
		}
	`)
	// None for optional user type should produce zeroinitializer of {i1, {i8*, i8*}}
	assertContains(t, ir, "{ i1, { i8*, i8* } } zeroinitializer")
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
	ir := generateIR(t, `
		type Row { string name; }
		take(Row? move r) {}
		test() {
			Row?[] v = [];
			v.push(Row(name: "a" + "b"));
			take(v[0]);
		}
	`)
	fn := extractFunction(ir, "__user.test")
	assertContains(t, fn, "heapdup.copy")
}

// T1175: `Holder(held: v[i])` — the constructor-field escape sibling. Same aliasing
// double-free: the new field owns the Optional value struct aliasing the vector
// slot. maybeEnableDupForConstructorArg arms the same Optional[heap-user] gate.
func TestT1175VectorOptionalHeapElementConstructorArgDups(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		type Holder { Row? held; }
		test() {
			Row?[] v = [];
			v.push(Row(name: "a" + "b"));
			h := Holder(held: v[0]);
		}
	`)
	fn := extractFunction(ir, "__user.test")
	assertContains(t, fn, "heapdup.copy")
}

// T1175: the no-drop-but-pal-free leg of the Optional[heap-user] gate. `Tag` is a
// field-less heap-user type with no drop() — it's still pal_malloc'd, so an escaped
// alias of the vector slot pal_free's the same pointer twice. optionalHeapDupElem
// recognizes both the droppable and the no-drop-pal-free inner, so f(v[i]) into a
// consuming param must still emit the deep-clone (dupHeapValue → heapdup.copy).
func TestT1175VectorOptionalNoDropHeapElementCallArgDups(t *testing.T) {
	ir := generateIR(t, `
		type Tag {}
		take(Tag? move t) {}
		test() {
			Tag?[] v = [];
			v.push(Tag());
			take(v[0]);
		}
	`)
	fn := extractFunction(ir, "__user.test")
	assertContains(t, fn, "heapdup.copy")
}

// T0397: `opt := m[k]` where the map value type is a tuple with droppable fields
// must dup the tuple's string fields so opt holds an independent copy.
// Without the dup, opt's bindingDropTuple and the map's element walk double-free
// the same string pointer. genInferredVarDecl sets dupTupleFieldAccess;
// genMethodIndex calls dupTupleValue which emits promise_string_new for string fields.
func TestT0397MapOptionalTupleIndexDupsStringField(t *testing.T) {
	ir := generateIR(t, `
		test() {
			m := map[string, (string, int)]();
			m["a"] = ("hello", 1);
			opt := m["a"];
		}
	`)
	// dupTupleValue emits promise_string_new to clone the string element.
	assertContains(t, ir, "call i8* @promise_string_new")
}

// T0397 (typed path): same dup via genTypedVarDecl's Optional[Tuple] check.
func TestT0397TypedVarDeclMapOptionalTupleDupsStringField(t *testing.T) {
	ir := generateIR(t, `
		test() {
			m := map[string, (string, int)]();
			m["a"] = ("hello", 1);
			(string, int)? opt = m["a"];
		}
	`)
	assertContains(t, ir, "call i8* @promise_string_new")
}

// T0489: c.tup_field = m[k]! must dup the RHS read via dupTupleValue before
// storing. The OptionalUnwrap-of-IndexExpr path goes through genMethodIndex's
// dupTupleFieldAccess consumer (expr.go:7514), which is a different consumer
// than the Vector path covered by TestT0489MemberAssignDupsTupleOnVecToField
// (expr.go:7654). Without this, the field and the map's stored value alias
// the same heap allocations, causing a silent double-free at scope exit.
func TestT0489MemberAssignDupsTupleOnMapUnwrapToField(t *testing.T) {
	ir := generateIR(t, `
		type T0489E { (string, int) f; drop(~this) {} }
		test() {
			m := map[string, (string, int)]();
			m["k"] = ("a" + "", 1);
			c := T0489E(f: ("first" + "", 1));
			c.f = m["k"]!;
		}
	`)
	// dupTupleValue emits promise_string_new to clone the string element on read.
	assertContains(t, ir, "call i8* @promise_string_new")
}

// T0741 Part C: an optional closure struct field must drop its env.
// emitOptionalValueDrop now has a *types.Signature case that branches on the
// has-value flag and deep-drops the inner closure's env.
func TestDropOptionalClosureField(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "define void @OptCb.drop")
	assertContains(t, ir, "optfield.drop")
	assertContains(t, ir, "closure.env.free")
	assertContains(t, ir, "env.deep_drop")
}

// T0814: force-unwrapping a LOCAL optional closure into a new local (`f := o!`)
// transfers the heap env from `o` to `f`. The fix has two halves:
//  1. claimEnvTemp recurses into the optional-wrapped fat pointer so the lambda's
//     env temp is claimed by `o` (no early env.tmp.drop of the lambda env), and
//  2. neutralizeForceUnwrapSource clears `o`'s present flag (`store i1 false`)
//     so `o`'s optional drop is skipped and only `f` frees the env once.
func TestUnwrapLocalOptionalClosure(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "cap" + "tured";
			(() -> int)? o = move || -> s.len;
			f := o!;
		}
	`)
	// The unwrap path is taken and the source optional's present flag is cleared.
	assertContains(t, ir, "unwrap.ok")
	assertContains(t, ir, "store i1 false")
	// f owns the env and frees it exactly once at scope exit.
	assertContains(t, ir, "env.free")
	// The optdrop for o is registered for the Signature inner case (the env is
	// freed via the closure-env path when present, not leaked).
	assertContains(t, ir, "optdrop.inner")
	assertContains(t, ir, "closure.env.free")
}

// T1234: a discarded call returning an optional closure `(() -> int)?` must drop
// its heap env. The result type is Optional(Signature); dropDiscardedOptional's
// Signature arm branches on the has-value tag, extracts the env pointer, and
// deep-drops it (env.deep_drop) when present. Without this the env leaked.
func TestDiscardOptionalClosureCallDropsEnv(t *testing.T) {
	ir := generateIR(t, `
		make_maybe(int x)(() -> int)? {
			return move || -> x + 1;
		}
		main() {
			make_maybe(5);
		}
	`)
	// The Signature arm fires: tag branch + presence-guarded env free.
	assertContains(t, ir, "discard.drop")
	assertContains(t, ir, "discard.env.free")
	// The env is deep-dropped (captured value + struct freed), not merely leaked.
	assertContains(t, ir, "env.deep_drop")
}

// T1234: discarding a *place* expression that reads an optional closure (`o;`) is
// a borrow — the binding owns and drops the env at scope exit. isBorrowingPlaceExpr
// must skip the discard-drop path here, else the env is freed twice. Verify no
// discard-drop blocks are emitted for the bare ident discard (the value is still
// dropped once, via its ordinary scope-exit binding drop).
func TestDiscardBoundOptionalClosurePlaceSkipsDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			(() -> int)? o = || -> 42;
			o;
		}
	`)
	// discard.env.free is unique to dropDiscardedOptional's Signature arm; the only
	// optional-closure discard here is the bare `o;` place, so its absence proves the
	// place-skip fired (the env is still freed once via o's scope-exit binding drop).
	assertNotContains(t, ir, "discard.env.free")
}

// B0181: Optional string field access + unwrap should dup to prevent double-free
func TestOptionalStringFieldUnwrapDup(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "call i8* @promise_string_new(")
}

// B0190: Inline optional unwrap must not track field string as a temp.
// The unwrapped i8* from `w.opt_name!` is a field reference (not a new allocation),
// so tracking it would cause the field's string to be freed at statement end.
func TestInlineOptionalUnwrapNoTempTrack(t *testing.T) {
	// This should compile without errors. At runtime, the inline unwrap
	// must not free the field string as a temp — only Wrapper.drop should.
	generateIR(t, `
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
	ir := generateIR(t, `
		type HBox { int n; drop(~this) {} }
		tfn() int {
			HBox? o = HBox(n: 5);
			return (o? _ { return -1; }).n;
		}
		main() { _ := tfn(); }
	`)
	fn := extractFunction(ir, "__user.tfn")
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
	ir := generateIR(t, `
		tfn() int {
			string? o = "abc".to_upper();
			return ((o)!).len;
		}
		main() { _ := tfn(); }
	`)
	fn := extractFunction(ir, "__user.tfn")
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
	ir := generateIR(t, `
		tfn() int {
			int[]? o = [1, 2].clone();
			return ((o)!).len;
		}
		main() { _ := tfn(); }
	`)
	fn := extractFunction(ir, "__user.tfn")
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
	ir := generateIR(t, `
		mk_str() string? { return "ab".to_upper(); }
		tfn() int {
			return (mk_str()!).len;
		}
		main() { _ := tfn(); }
	`)
	fn := extractFunction(ir, "__user.tfn")
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
	ir := generateIR(t, ""+
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
	ir := generateIR(t, `
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
	assertContains(t, ir, "define void @Holder.drop")
	// The has-value guard for the optional enum field.
	assertContains(t, ir, "optfield.drop")
	assertContains(t, ir, "optfield.skip")
	// The enum's drop is invoked from inside the has-value branch.
	assertContains(t, ir, "call void @Maybe.drop")
}

// T0572: Holder with Optional<FieldlessEnum> field — the !needsDrop short-
// circuit must fire so no spurious cleanup branches are emitted for the
// fieldless enum slot. The holder itself has a sibling droppable field
// (Resource) so its synth drop body is generated and walks all fields,
// reaching the Optional<FieldlessEnum> branch. The short-circuit ensures
// only the sibling Resource's drop appears in the holder body — no
// optfield.drop block for the Color slot.
func TestDropTypeWithOptionalFieldlessEnumFieldShortCircuits(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "define void @Holder.drop")
	// The Resource sibling is dropped — confirms the holder synth drop runs.
	assertContains(t, ir, "call void @Resource.drop")
	// Fieldless Color enum has no drop fn, so the short-circuit must skip
	// emitting any call to a Color drop. (No @Color.drop function exists.)
	assertNotContains(t, ir, "call void @Color.drop")
}

// B0293: CastExpr as! on optional must neutralize source to prevent double-free.
func TestAsBangOptionalNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		test() {
			Point? p = Point(x: 8, y: 9);
			Point q = p as! Point;
		}
	`)
	// After as! unwrap into q, the optional p's present flag must be set to false.
	// This prevents both p's optional drop and q's drop from freeing the same instance.
	assertContains(t, ir, "store i1 false")
}

// B0293: Optional handler (p? _ { fallback }) must neutralize source.
func TestOptionalHandlerNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		test() {
			Point? p = Point(x: 5, y: 6);
			Point q = p? _ { Point(x: 0, y: 0); };
		}
	`)
	// After handler unwrap into q, the optional p's present flag should be set to false.
	assertContains(t, ir, "store i1 false")
}

// B0299: Inline optional string field unwrap must not track the string as a
// statement-end temp. The owner's drop handles the string's lifetime.
// Without this fix, statement-end cleanup frees the original string from
// the field, then Wrapper.drop frees it again → double-free.
func TestOptionalFieldStringInlineUnwrapNoTempTrack(t *testing.T) {
	res := compileResult(t, `
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
	assertNotContains(t, testIR, "promise_string_new")
}

// T0540: same deep-dup requirement for the Optional[Vector] field branch.
func TestB0219OptionalVecFieldDeepDup(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "vecdup.copy")
	assertContains(t, ir, "vecdup_str.head")
	assertContains(t, ir, "promise_string_new")
}

// T0939: binding an Optional[Vector]-with-droppable-elements field to a plain
// container local must null-guard the element-clone loop. On the optional's `none`
// path field 1 is null; the dup of null is null, and the unguarded clone loop
// (loadVectorLen) would dereference it → segfault. The guard emits veccloneopt.
// blocks that skip the loop when the dup is null.
func TestT0939OptionalVecFieldCloneNullGuard(t *testing.T) {
	ir := generateIR(t, `
		type SVBox { string[]? v; drop(~this) {} }
		main() {
			bx := SVBox(v: none);
			b := string[]();
			x := bx.v ?: b;
		}
	`)
	assertContains(t, ir, "veccloneopt.do")
	assertContains(t, ir, "veccloneopt.merge")
}

// T0939 (genArrayIndex call site): the same null-guard fix also covers the
// Optional[Vector] element path of `genArrayIndex`. Indexing an array of
// `string[]?` and binding the elvis result to a plain `string[]` must emit the
// veccloneopt. guard blocks for the element-clone loop (the slot's inner buffer is
// null on the `none` path). Pins the second call site at the IR level.
func TestT0939OptionalVecArrayIndexCloneNullGuard(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[]? a0 = none;
			string[]? a1 = none;
			string[]?[2] arr = [a0, a1];
			b := string[]();
			x := arr[0] ?: b;
		}
	`)
	assertContains(t, ir, "veccloneopt.do")
	assertContains(t, ir, "veccloneopt.merge")
}

func TestDiscardedStringPopDropsOptionalInner(t *testing.T) {
	// B0196: When v.pop() result is discarded and element type is string,
	// the string inside the optional must be dropped.
	ir := generateIR(t, `
		main() {
			string[] v = [];
			v.push("hello");
			v.pop();
		}
	`)
	// Should have a discard.drop block that calls promise_string_drop
	assertContains(t, ir, "discard.drop")
	assertContains(t, ir, "discard.skip")
	assertContains(t, ir, "call void @promise_string_drop(i8*")
}

// --- Optional interpolation tests ---

func TestStringInterpolationOptionalPresent(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			string s = "{x}";
		}
	`)
	// Should branch on presence flag
	assertContains(t, ir, "interp.some")
	assertContains(t, ir, "interp.none")
	assertContains(t, ir, "interp.merge")
	// Should call int_to_string in the some branch
	assertContains(t, ir, "promise_int_to_string")
}

func TestStringInterpolationOptionalNone(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = none;
			string s = "{x}";
		}
	`)
	assertContains(t, ir, "interp.some")
	assertContains(t, ir, "interp.none")
}

func TestStringInterpolationOptionalString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? name = "Alice";
			string s = "hello {name}";
		}
	`)
	assertContains(t, ir, "interp.some")
	assertContains(t, ir, "interp.none")
}

// T0421: Optional enum interpolation emits interp.some/none wrapper + inner enum switch.
func TestStringInterpolationEnumOptional(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		main() {
			Color? c = Color.Green;
			string s = "{c}";
		}
	`)
	// Optional wrapping blocks
	assertContains(t, ir, "interp.some")
	assertContains(t, ir, "interp.none")
	// Enum switch inside the some branch
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "enum.interp.Red")
	assertContains(t, ir, "enum.interp.Green")
	assertContains(t, ir, "enum.interp.Blue")
	// "none" string for absent case
	assertContains(t, ir, `"none"`)
}

// --- Optional narrowing codegen tests ---

func TestOptionalTruthinessNarrowingCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? cc = "hello";
			if cc {
				string s = cc;
			}
		}
	`)
	// Should have narrow blocks (not regular if blocks)
	assertContains(t, ir, "narrow.then")
	assertContains(t, ir, "narrow.end")
	// In the then block, should extractvalue the inner string from the optional
	assertContains(t, ir, "extractvalue { i1, i8* }")
}

func TestIsPresentNarrowingCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			if x is present {
				int n = x;
			}
		}
	`)
	assertContains(t, ir, "narrow.then")
	// Should extract the inner i64 from { i1, i64 }
	assertContains(t, ir, "extractvalue { i1, i64 }")
}

func TestOptionalNarrowingWithElseCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x = 42;
			if x {
				int n = x;
			} else {
				int n = 0;
			}
		}
	`)
	assertContains(t, ir, "narrow.then")
	assertContains(t, ir, "narrow.else")
	assertContains(t, ir, "narrow.end")
}

func TestUninitOptionalVarCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? x;
			string s = "{x}";
		}
	`)
	assertContains(t, ir, "zeroinitializer")
	assertContains(t, ir, "interp.none")
}

func TestOptionalChainUserType(t *testing.T) {
	ir := generateIR(t, `
		type Cfg {
			int port;
		}
		main() {
			Cfg? c = Cfg(port: 8080);
			int? p = c?.port;
		}
	`)
	assertContains(t, ir, "optchain.some")
	assertContains(t, ir, "optchain.none")
}

// T1421: `opt?.getter` for a user-declared getter must emit a getter call on
// the present path (not a raw struct-field load, which read the int as a
// pointer and segfaulted at 0x8).
func TestOptionalChainUserGetter(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			int n;
			get size int => this.n;
		}
		main() {
			Box? b = Box(n: 5);
			int? s = b?.size;
		}
	`)
	assertContains(t, ir, "optchain.some")
	assertContains(t, ir, "optchain.none")
	// Present path routes through the getter method, not a bare field load.
	assertContains(t, ir, "call i64 @Box.size(")
}

// T1421: `opt?.len` for a built-in string getter must not ICE — string.len is
// synthesized inline (not a declared getter), so it must flow through the full
// member-access path.
func TestOptionalChainNativeStringLen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? name = "promise";
			int? size = name?.len;
		}
	`)
	assertContains(t, ir, "optchain.some")
	assertContains(t, ir, "optchain.none")
}

// T1421: a virtual getter reached through `?.` on a base-typed optional holding a
// subtype must dispatch through the vtable (indirect `call i8* %...`), not a
// direct call to the base method. The pre-T1421 raw-GEP path could not do vtable
// dispatch; the delegation to genMemberExpr restores it.
func TestOptionalChainVirtualGetter(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "optchain.some")
	assertContains(t, ir, "optchain.none")
	assertContains(t, ir, "@promise_vtable_Animal")
	// Dispatch is indirect through the vtable, not a direct call to Animal.sound.
	assertContains(t, ir, "call i8* %")
	assertNotContains(t, ir, "call i8* @Animal.sound(")
}

// T1421: a plain field read through `?.` on a base-typed optional holding a
// subtype must resolve against the concrete instance layout (offset load), not
// crash — the field path also flows through the delegated member access.
func TestOptionalChainPolymorphicField(t *testing.T) {
	ir := generateIR(t, `
		type Animal {
			int legs;
			get sound string => "generic";
		}
		type Dog is Animal {
			get sound string => "woof";
		}
		main() {
			Animal? a = Dog(legs: 4);
			int? n = a?.legs;
		}
	`)
	assertContains(t, ir, "optchain.some")
	assertContains(t, ir, "optchain.none")
	// Field load produces an i64 result wrapped into the optional.
	assertContains(t, ir, "load i64")
}

func TestValueTypeOptional(t *testing.T) {
	ir := generateIR(t, `
		type Point {
			int x `+"`value"+`;
			int y `+"`value"+`;
		}
		main() {
			Point? maybe = Point(x: 1, y: 2);
		}
	`)
	// Optional wraps the full value struct: { i1, %promise_Point_v }
	assertContains(t, ir, "{ i1, %promise_Point_v }")
}

func TestOptionalRecoveryCodegen(t *testing.T) {
	// Optional recovery: non-recovering handler wraps result as T?
	ir := generateIR(t, `
		fail!() int { raise error(message: "oops"); }
		main() {
			x := fail() ? e { print_line("handled"); };
		}
	`)
	// Should wrap success value as optional some (insertvalue with i1 true)
	// and produce a phi node merging ok/error paths
	assertContains(t, ir, "insertvalue")
	assertContains(t, ir, "i1 true")
}

// T1012 (T0485 branch): an Optional-of-heap variant payload (`string? maybe`)
// destructured via `if x is V(field)` must NOT be dup'd — it is marked
// match-borrowed instead (the binding aliases the subject's payload, which the
// subject's synth enum drop owns). So no clone and no per-binding drop flag are
// emitted; only in-scope reads are sound (escape is the separate T1170 gap).
func TestT1012IfIsDestructureOptionalPayloadBorrowNoDup(t *testing.T) {
	ir := generateIR(t, `
		enum Box { Has(string? maybe), Nothing }
		read() int {
			Box b = Box.Has(maybe: "a" + "b");
			int out = 0;
			if b is Has(maybe) {
				if s := maybe { out = s.len; }
			}
			return out;
		}
		main() { x := read(); }
	`)
	fn := extractFunction(ir, "__user.read")
	// Optional heap payload is borrow-marked, not cloned, and no drop flag is
	// registered for the `maybe` binding.
	assertNotContains(t, fn, "strdup.copy")
	assertNotContains(t, fn, "maybe.dropflag")
}

// T1170: an Optional-of-heap variant payload (`string? maybe`) that ESCAPES the
// narrowing scope (here via `return`) must be deep-cloned on the read/escape side
// (genIdentExpr, gated on matchBorrowedIdents + the dup flag genReturnStmt sets),
// so the escaped Optional owns an independent inner string and survives the
// subject's synth enum drop. The clone lowers through dupString → `strdup.copy`.
func TestT1170OptionalPayloadEscapeDupsOnReturn(t *testing.T) {
	ir := generateIR(t, `
		enum Box { Has(string? maybe), Nothing }
		esc() string? {
			Box b = Box.Has(maybe: "a" + "b");
			if b is Has(maybe) { return maybe; }
			return none;
		}
		main() { s := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	// The escaping Optional[string] payload is cloned via dupString.
	assertContains(t, fn, "strdup.copy")
}

// T1170 zero-copy control: an in-scope read of an Optional-of-heap payload (no
// escape → no dup flag set) must NOT clone. This is the proof that the escape
// dup is gated on an owning sink and in-scope borrows stay zero-copy.
func TestT1170OptionalPayloadInScopeNoDup(t *testing.T) {
	ir := generateIR(t, `
		enum Box { Has(string? maybe), Nothing }
		read() int {
			Box b = Box.Has(maybe: "a" + "b");
			int out = 0;
			if b is Has(maybe) {
				if s := maybe { out = s.len; }
			}
			return out;
		}
		main() { x := read(); }
	`)
	fn := extractFunction(ir, "__user.read")
	assertNotContains(t, fn, "strdup.copy")
}

// T1170: an Optional-of-heap payload stored to an escaping OUTER local
// (`out = maybe`, whole-Optional ident RHS) must be cloned on read. This
// exercises the genAssignStmt IdentExpr-RHS branch (isVariantPayloadBorrowShape)
// — distinct from the array-element RHS branch covered above.
func TestT1170OptionalPayloadEscapeDupsOnStore(t *testing.T) {
	ir := generateIR(t, `
		enum Box { Has(string? maybe), Nothing }
		esc() string? {
			Box b = Box.Has(maybe: "a" + "b");
			string? out = none;
			if b is Has(maybe) { out = maybe; }
			return out;
		}
		main() { s := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertContains(t, fn, "strdup.copy")
}

// T1170: an Optional-of-heap payload passed to a consuming (~/move) param
// (`consume(move maybe)`) escapes into the callee and must be cloned so the
// subject's synth enum drop doesn't free the value the callee now owns. This
// exercises the maybeEnableDupForMutRefArg T1170 branch.
func TestT1170OptionalPayloadEscapeConsumingArg(t *testing.T) {
	ir := generateIR(t, `
		enum Box { Has(string? maybe), Nothing }
		consume(string? move s) string { if x := s { return x; } return ""; }
		esc() string {
			Box b = Box.Has(maybe: "a" + "b");
			string r = "";
			if b is Has(maybe) { r = consume(move maybe); }
			return r;
		}
		main() { s := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertContains(t, fn, "strdup.copy")
}

// T1170: an Optional-of-heap payload used to initialize an owned constructor
// field (`W(held: maybe)`) escapes via the returned instance and must be cloned.
// This exercises the maybeEnableDupForConstructorArg T1170 branch.
func TestT1170OptionalPayloadEscapeConstructorField(t *testing.T) {
	ir := generateIR(t, `
		enum Box { Has(string? maybe), Nothing }
		type W { string? held; }
		esc() W {
			Box b = Box.Has(maybe: "a" + "b");
			W w = W(held: none);
			if b is Has(maybe) { w = W(held: maybe); }
			return w;
		}
		main() { w := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertContains(t, fn, "strdup.copy")
}

// T1170: the escape dup fires uniformly for the `match` path (not just `if is`),
// since both populate matchBorrowedIdents. A match arm returning an
// Optional-of-heap payload must clone on read.
func TestT1170OptionalPayloadEscapeDupsOnMatch(t *testing.T) {
	ir := generateIR(t, `
		enum Box { Has(string? maybe), Nothing }
		esc() string? {
			Box b = Box.Has(maybe: "a" + "b");
			match b {
				Has(maybe) => { return maybe; },
				Nothing => { return none; },
			}
		}
		main() { s := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertContains(t, fn, "strdup.copy")
}

// T1174: an Optional-of-heap-user-type variant payload (`Row? maybe`) that
// ESCAPES `if is`/`match` (return / store-to-outer / consuming arg / constructor
// field) must be deep-cloned via dupBorrowedHeapUserPayload — otherwise the
// bound alias points into the subject's variant payload, which the subject's
// synth enum drop frees at scope exit (UAF / SIGSEGV). The clone lowers to a
// dupHeapValue `heapdup.copy` block in the escaping function.
func TestT1174OptionalHeapUserPayloadEscapeDups(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		enum Box { Has(Row? maybe), Nothing }
		esc() Row? {
			Box b = Box.Has(maybe: Row(name: "a" + "b"));
			if b is Has(maybe) { return maybe; }
			return none;
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	// Escaping the borrowed Optional[Row] payload deep-clones the inner heap value.
	assertContains(t, fn, "heapdup.copy")
}

// T1174 over-application guard: an in-scope-only Optional[heap-user] binding must
// stay a zero-copy borrow (no dup) — the subject outlives the narrowing and its
// synth enum drop frees the payload exactly once. The dup is gated to explicit
// escape sites, so no `heapdup.copy` is emitted here (preserving the T0512
// nested-Optional zero-copy invariant). An over-eager dup would also leak.
func TestT1174OptionalHeapUserPayloadInScopeNoDup(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		enum Box { Has(Row? maybe), Nothing }
		rd() int {
			Box b = Box.Has(maybe: Row(name: "a" + "b"));
			int out = 0;
			if b is Has(maybe) { if r := maybe { out = r.name.len; } }
			return out;
		}
		main() { x := rd(); }
	`)
	fn := extractFunction(ir, "__user.rd")
	assertNotContains(t, fn, "heapdup.copy")
}

// T1174: `v.push(maybe)` moves a match-borrowed Optional[heap-user] payload into
// a vector. Push is a native special-case that bypasses the escape-site dups, so
// it must deep-clone the Optional[heap-user] element in maybeDupPushElement's
// Optional branch — otherwise the vector slot aliases the subject's variant
// payload and double-frees when both drop. The clone lowers to a dupHeapValue
// `heapdup.copy` block. Also covers the pre-existing Vector[Row?] slice path,
// which shares the same maybeDupPushElement branch.
func TestT1174OptionalHeapUserPushDups(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		enum Box { Has(Row? maybe), Nothing }
		esc() Row?[] {
			Box b = Box.Has(maybe: Row(name: "a" + "b"));
			Row?[] v = [];
			if b is Has(maybe) { v.push(maybe); }
			return v;
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertContains(t, fn, "heapdup.copy")
}

// T1174: optionalHeapDupElem admits BOTH droppable heap-user types (via
// isDroppableHeapUserType — the Row-with-string cases above) AND no-drop-but-heap
// user types (via isHeapUserNoDropPalFree — a heap type whose fields need no
// drop). This pins the second branch: a `P?` payload (`type P { int x; }`, heap-
// allocated, pal_free-only, no synth drop) escaping `if is` must still deep-clone
// the inner via dupHeapValue, else the returned alias is freed by the subject's
// synth enum drop at scope exit (UAF). A value type would be copied by value and
// route past optionalHeapDupElem, so the presence of `heapdup.copy` confirms the
// no-drop heap branch is taken.
func TestT1174OptionalNoDropHeapUserPayloadEscapeDups(t *testing.T) {
	ir := generateIR(t, `
		type P { int x; }
		enum Box { Has(P? maybe), Nothing }
		esc() P? {
			Box b = Box.Has(maybe: P(x: 42));
			if b is Has(maybe) { return maybe; }
			return none;
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertContains(t, fn, "heapdup.copy")
}

// T1178 (non-array sibling preserved): the fix skips the read-side dup ONLY for the
// array shape. An Optional[heap-user] variant payload (T1174) is NOT covered by
// arrayElemNeedsEscapeDup, so genIdentExpr's gate (!isArr => true) still lets
// dupBorrowedHeapUserPayload deep-clone it on escape. Guards against the fix
// over-reaching and suppressing the Optional[user] clone (which would be a UAF, not
// a leak). The escaped Optional's inner heap instance is cloned exactly once.
func TestT1178OptionalHeapUserPayloadStillClones(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		enum Box { Some(Row? maybe), Empty }
		esc() Row? {
			Box b = Box.Some(maybe: Row(name: "a" + "b"));
			if b is Some(maybe) { return maybe; }
			return none;
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	// dupBorrowedHeapUserPayload's optionalHeapDupElem branch still fires.
	assertContains(t, fn, "heapdup.copy")
}

// T0392: Synth drop must recurse into Optional[heap-user-type] field, dropping the
// inner heap allocation. Without this, `Holder { Box? data }` leaks the Box.
func TestSynthDropRecursesIntoHeapUserOptionalField(t *testing.T) {
	ir := generateIR(t, `
		type T0392Box { int n; drop(~this) {} }
		type T0392Holder { T0392Box? data; }
		main() {
			h := T0392Holder(data: T0392Box(n: 7));
		}
	`)
	holderDrop := extractFunction(ir, "T0392Holder.drop")
	if holderDrop == "" {
		t.Fatal("expected T0392Holder.drop in IR")
	}
	// optfield drop block conditional on the present flag.
	assertContains(t, holderDrop, "optfield.drop")
	assertContains(t, holderDrop, "optfield.skip")
	// Inner Box.drop must be invoked for present values.
	assertContains(t, holderDrop, "call void @T0392Box.drop")
	// Heap user type without synth drop also requires pal_free of the instance.
	assertContains(t, holderDrop, "call void @pal_free")
}

// T0392: Synth drop must recurse into nested Optional T?? fields, visiting both
// the outer and inner has-value flags before dropping.
func TestSynthDropRecursesIntoNestedOptionalField(t *testing.T) {
	ir := generateIR(t, `
		type T0392Box2 { int n; drop(~this) {} }
		type T0392Holder2 { T0392Box2?? data; }
		main() {
			T0392Box2? inner = T0392Box2(n: 1);
			h := T0392Holder2(data: inner);
		}
	`)
	holderDrop := extractFunction(ir, "T0392Holder2.drop")
	if holderDrop == "" {
		t.Fatal("expected T0392Holder2.drop in IR")
	}
	// Two pairs of optfield branches — outer Optional and inner Optional.
	if got := strings.Count(holderDrop, "optfield.drop"); got < 2 {
		t.Errorf("expected at least 2 optfield.drop blocks (outer + inner), got %d", got)
	}
	// Inner Box.drop must still be called for the doubly-wrapped value.
	assertContains(t, holderDrop, "call void @T0392Box2.drop")
}

// T0392: Force-unwrap of an Optional[heap-user-type] field neutralizes the
// owner's flag so the holder's drop doesn't double-free the inner instance now
// owned by the new local.
func TestForceUnwrapOfHeapUserOptionalFieldNeutralizes(t *testing.T) {
	ir := generateIR(t, `
		type T0392Box3 { int n; drop(~this) {} }
		type T0392Holder3 { T0392Box3? data; }
		main() {
			h := T0392Holder3(data: T0392Box3(n: 3));
			b := h.data!;
		}
	`)
	// Slice out the user's main goroutine body. The C-ABI @main wrapper has no
	// user code; the unwrap site lives in @.goroutine.main.
	defineMarker := "define i8* @.goroutine.main"
	start := strings.Index(ir, defineMarker)
	if start < 0 {
		t.Fatal("expected define of .goroutine.main")
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("expected closing brace for .goroutine.main")
	}
	mainFn := rest[:end+2]
	// Neutralization stores `i1 false` into the field's present flag.
	assertContains(t, mainFn, "store i1 false")
}

func TestT1073ArrayLitForceUnwrapNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		type T1073Box { string name; drop(~this) {} }
		arr(T1073Box? move o) T1073Box[] { return [o!]; }
		main() {}
	`)
	fn := extractFunction(ir, "__user.arr")
	if fn == "" {
		t.Fatal("expected __user.arr in IR")
	}
	assertContains(t, fn, "unwrap.ok")
	assertContains(t, fn, t1073NeutralizeSig)
}

func TestT1073TupleLitForceUnwrapNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		type T1073Box { string name; drop(~this) {} }
		tup(T1073Box? move o) (T1073Box, int) { return (o!, 1); }
		main() {}
	`)
	fn := extractFunction(ir, "__user.tup")
	if fn == "" {
		t.Fatal("expected __user.tup in IR")
	}
	assertContains(t, fn, t1073NeutralizeSig)
}

func TestT1073MapLitForceUnwrapNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		type T1073Box { string name; drop(~this) {} }
		mp(T1073Box? move o) map[int, T1073Box] { return {1: o!}; }
		main() {}
	`)
	fn := extractFunction(ir, "__user.mp")
	if fn == "" {
		t.Fatal("expected __user.mp in IR")
	}
	assertContains(t, fn, t1073NeutralizeSig)
}

// T1073: a copy/scalar inner (`int?`) is NOT consumed by force-unwrap, so its
// source optional must NOT be neutralized (it stays usable). neutralizeForceUnwrapElem
// self-gates on typeNeedsFieldDrop, so no present-flag clear is emitted here.
func TestT1073ArrayLitScalarForceUnwrapNoNeutralize(t *testing.T) {
	ir := generateIR(t, `
		arr(int? move o) int[] { return [o!]; }
		main() {}
	`)
	fn := extractFunction(ir, "__user.arr")
	if fn == "" {
		t.Fatal("expected __user.arr in IR")
	}
	// int? optional layout is `{ i1, i64 }`, not `{ i1, { i8*, i8* } }`; and no
	// present-flag clear should be emitted for the (copy) source.
	assertNotContains(t, fn, "i32 0, i32 0\n\tstore i1 false")
}

// T1073: a paren-wrapped force-unwrap `[(o!)]` must still neutralize the source.
// Exercises the ParenExpr-peel loop in isForceUnwrapElem — codegen sees through
// ParenExpr at genExpr but the AST-shape dispatch here must peel it too.
func TestT1073ArrayLitParenWrappedForceUnwrapNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		type T1073Box { string name; drop(~this) {} }
		arr(T1073Box? move o) T1073Box[] { return [(o!)]; }
		main() {}
	`)
	fn := extractFunction(ir, "__user.arr")
	if fn == "" {
		t.Fatal("expected __user.arr in IR")
	}
	assertContains(t, fn, t1073NeutralizeSig)
}

// T1073: force-unwrap of a droppable map *key* `{o!: 1}` must neutralize the
// source optional (the map's drop frees keys via []=), mirroring the map-value
// path. Exercises the entry.Key neutralize call site in genMapLit.
func TestT1073MapLitKeyForceUnwrapNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		type T1073Key {
			string name;
			drop(~this) {}
			get hash int { return 7; }
			== (T1073Key other) bool { return this.name == other.name; }
		}
		mk(T1073Key? move o) map[T1073Key, int] { return {o!: 1}; }
		main() {}
	`)
	fn := extractFunction(ir, "__user.mk")
	if fn == "" {
		t.Fatal("expected __user.mk in IR")
	}
	assertContains(t, fn, t1073NeutralizeSig)
}

// T0392: Synth drop must call pal_free for heap user types WITHOUT a drop method
// (B0211 case). The inner has no drop function but the heap allocation must be freed.
func TestSynthDropOptionalNoDropHeapUserField(t *testing.T) {
	ir := generateIR(t, `
		type T0392RawBox { int n; }
		type T0392RawHolder { T0392RawBox? data; }
		main() {
			h := T0392RawHolder(data: T0392RawBox(n: 7));
		}
	`)
	holderDrop := extractFunction(ir, "T0392RawHolder.drop")
	if holderDrop == "" {
		t.Fatal("expected T0392RawHolder.drop in IR")
	}
	// optfield branches conditional on the present flag.
	assertContains(t, holderDrop, "optfield.drop")
	// pal_free must still happen for raw heap user types with no explicit drop.
	assertContains(t, holderDrop, "call void @pal_free")
	// No call to a drop method since the type doesn't define one.
	assertNotContains(t, holderDrop, "call void @T0392RawBox.drop")
}

// T0392: Synth drop must use the synth drop function for heap user types WITH
// synth drop (e.g., string field). The synth drop calls pal_free internally,
// so the optional path must NOT call pal_free again.
func TestSynthDropOptionalSynthDropHeapUserField(t *testing.T) {
	ir := generateIR(t, `
		type T0392SynBox { string s; }
		type T0392SynHolder { T0392SynBox? data; }
		main() {
			h := T0392SynHolder(data: T0392SynBox(s: "x"));
		}
	`)
	holderDrop := extractFunction(ir, "T0392SynHolder.drop")
	if holderDrop == "" {
		t.Fatal("expected T0392SynHolder.drop in IR")
	}
	// optfield branches conditional on the present flag.
	assertContains(t, holderDrop, "optfield.drop")
	// Synth drop is invoked — calls _Box.drop which itself calls pal_free.
	assertContains(t, holderDrop, "call void @T0392SynBox.drop")
}

// T0392: Force-unwrap of a string/vector optional field must NOT trigger
// MemberExpr neutralization — genFieldAccess already dups at access time, so
// neutralizing would leak the original. Verified by counting store-i1-false
// instructions: the heap-user case does ONE extra store (the neutralization
// flag clear) compared to the string case.
func TestForceUnwrapStringOptionalFieldNoExtraStore(t *testing.T) {
	stringIR := generateIR(t, `
		type T0392StrHolder { string? name; drop(~this) {} }
		main() {
			h := T0392StrHolder(name: "world");
			s := h.name!;
		}
	`)
	heapIR := generateIR(t, `
		type T0392HBox { int n; drop(~this) {} }
		type T0392HHolder { T0392HBox? data; drop(~this) {} }
		main() {
			h := T0392HHolder(data: T0392HBox(n: 7));
			b := h.data!;
		}
	`)
	extractMain := func(ir string) string {
		start := strings.Index(ir, "define i8* @.goroutine.main")
		if start < 0 {
			t.Fatal("expected .goroutine.main")
		}
		rest := ir[start:]
		end := strings.Index(rest, "\n}\n")
		if end < 0 {
			t.Fatal("expected closing brace")
		}
		return rest[:end+2]
	}
	stringMain := extractMain(stringIR)
	heapMain := extractMain(heapIR)
	stringStores := strings.Count(stringMain, "store i1 false")
	heapStores := strings.Count(heapMain, "store i1 false")
	// The heap-user case neutralizes the field's present flag (one extra
	// store i1 false). The string case does not.
	if heapStores <= stringStores {
		t.Errorf("expected heap-user neutralization to add ≥1 extra store; "+
			"got string=%d heap=%d", stringStores, heapStores)
	}
}

// T0392: Force-unwrap of a `this.field` inside a method must not crash codegen.
// Currently borrowed `this` is not in c.locals, so neutralization no-ops — this
// is a bug (T0416) but the codegen path itself must remain stable.
func TestForceUnwrapThisFieldDoesNotCrashCodegen(t *testing.T) {
	// Smoke test only — verifies codegen produces IR for `this.field!` without
	// panicking. The runtime double-free (T0416) is filed separately.
	ir := generateIR(t, `
		type T0392MBox { int n; drop(~this) {} }
		type T0392MHolder {
			T0392MBox? data;
			drop(~this) {}
			get_inner(this) int {
				if b := this.data {
					return b.n;
				}
				return -1;
			}
		}
		main() {
			h := T0392MHolder(data: T0392MBox(n: 5));
			v := h.get_inner();
		}
	`)
	// Method body should be present and reference the field GEP.
	getInner := extractFunction(ir, "T0392MHolder.get_inner")
	if getInner == "" {
		t.Fatal("expected T0392MHolder.get_inner in IR")
	}
}

// T0101: Optional field in type with synthesized drop
func TestOptionalFieldInSynthDrop(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { string? opt; }
		main() {
			Wrapper w = Wrapper(opt: "hello");
		}
	`)
	// Wrapper gets synthesized drop that checks optional field
	assertContains(t, ir, "define void @Wrapper.drop")
	assertContains(t, ir, "optfield.drop")
	assertContains(t, ir, "optfield.skip")
}

// T0111: Optional local with droppable inner type gets scope-exit drop
func TestOptionalLocalStringDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? s = "hello";
		}
	`)
	// Optional local should get a drop binding with optdrop blocks
	assertContains(t, ir, "optdrop.check")
	assertContains(t, ir, "optdrop.inner")
	assertContains(t, ir, "promise_string_drop")
}

// T0111: Force unwrap of optional identifier clears drop flag
func TestOptionalForceUnwrapClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? s = "hello";
			string val = s!;
		}
	`)
	// Should have optdrop blocks (drop registered for s)
	assertContains(t, ir, "optdrop.check")
	// The drop flag should be cleared (store i1 false) after unwrap
	assertContains(t, ir, "store i1 false")
}

// B0309: Force unwrap in index-assignment key position neutralizes source optional.
func TestOptionalForceUnwrapIndexAssignKey(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? key = "hello";
			map[string, int] m = {:};
			m[key!] = 42;
		}
	`)
	// The []=  call should exist (mangled as Map[string, int].[]=)
	assertContains(t, ir, `.[]="`)
	// B0309: present flag must be set to false after index assign (neutralize source)
	assertContains(t, ir, "store i1 false")
}

// B0309: Force unwrap in index-assignment value position neutralizes source optional.
func TestOptionalForceUnwrapIndexAssignValue(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? val = "hello";
			map[int, string] m = {:};
			m[1] = val!;
		}
	`)
	assertContains(t, ir, `.[]="`)
	// B0309: present flag must be set to false after index assign (neutralize source)
	assertContains(t, ir, "store i1 false")
}

// B0312: Force unwrap in ident reassignment neutralizes source optional.
func TestOptionalForceUnwrapIdentReassign(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? opt = "hello";
			string x = "";
			x = opt!;
		}
	`)
	// B0312: present flag must be set to false after ident reassign (neutralize source)
	assertContains(t, ir, "store i1 false")
}

// B0312: Force unwrap in member assignment neutralizes source optional.
func TestOptionalForceUnwrapMemberAssign(t *testing.T) {
	ir := generateIR(t, `
		type Holder { string val; }
		main() {
			string? opt = "hello";
			h := Holder(val: "");
			h.val = opt!;
		}
	`)
	// B0312: present flag must be set to false after member assign (neutralize source)
	assertContains(t, ir, "store i1 false")
}

// T0111: Optional local with vector inner type gets scope-exit drop
func TestOptionalLocalVectorDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[]? v = [1, 2, 3];
		}
	`)
	assertContains(t, ir, "optdrop.check")
	assertContains(t, ir, "optdrop.inner")
	assertContains(t, ir, "Vector.drop")
}

// T0938: Optional local with a vector inner whose elements are droppable
// (string[]?) must walk and drop elements before freeing the buffer, under a
// bit-63 static-vector guard — mirroring the non-optional emitStringDropCall
// path. Without this, only the buffer is freed and the elements leak.
func TestOptionalLocalVectorStringElementDrop(t *testing.T) {
	ir := generateIR(t, `
		dropfn_str() {
			string[] v = [];
			v.push("a");
			string[]? a = v;
		}
		main() { dropfn_str(); }
	`)
	fn := extractFunction(ir, "__user.dropfn_str")
	assertContains(t, fn, "optdrop.check")
	assertContains(t, fn, "optdrop.inner")
	// Static-vector guard: bit-63 mask before deciding to drop elements/buffer.
	assertContains(t, fn, "optvecdrop.nonstatic")
	assertContains(t, fn, "-9223372036854775808")
	// Element-drop loop runs before the buffer free.
	assertContains(t, fn, "vecdrop.head")
	assertContains(t, fn, "call void @promise_string_drop")
	assertContains(t, fn, "Vector.drop")
}

// T0938: A non-droppable element type (int[]?) must NOT emit a string element
// drop loop — only the buffer free path under the static guard.
func TestOptionalLocalVectorIntNoElementDrop(t *testing.T) {
	ir := generateIR(t, `
		dropfn_int() {
			int[] v = [];
			v.push(7);
			int[]? a = v;
		}
		main() { dropfn_int(); }
	`)
	fn := extractFunction(ir, "__user.dropfn_int")
	assertContains(t, fn, "optvecdrop.nonstatic")
	// No element-drop loop and no string drop inside the function for non-droppable ints.
	assertNotContains(t, fn, "vecdrop.head")
	assertNotContains(t, fn, "call void @promise_string_drop")
}

// T0111: Force unwrap of optional field access dups the string via dupStringFieldAccess
func TestOptionalFieldForceUnwrapDupsString(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { string? opt; }
		main() {
			Wrapper w = Wrapper(opt: "hello");
			string val = w.opt!;
		}
	`)
	// dupStringFieldAccess mechanism dups the string during field access
	assertContains(t, ir, "strdup.copy")
	assertContains(t, ir, "promise_string_new")
}

// B0196: Discarded Vector[string].pop() must drop the inner string.
func TestDropDiscardedOptionalStringPop(t *testing.T) {
	ir := generateIR(t, `
		test() {
			string[] v = ["a", "b", "c"];
			v.pop();
		}
	`)
	// The discarded optional string from pop() should trigger a conditional drop.
	assertContains(t, ir, "discard.drop")
	assertContains(t, ir, "call void @promise_string_drop")
}

// B0196: Discarded Vector[int].pop() should NOT emit discard drop (int is not droppable).
func TestNoDropDiscardedOptionalIntPop(t *testing.T) {
	ir := generateIR(t, `
		test() {
			int[] v = [1, 2, 3];
			v.pop();
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	if strings.Contains(testFn, "discard.drop") {
		t.Fatalf("expected test function to NOT contain discard.drop\ngot:\n%s", testFn)
	}
}

// B0208: Discarded Vector[Vector[int]].pop() must drop the inner vector.
func TestDropDiscardedOptionalVectorPop(t *testing.T) {
	ir := generateIR(t, `
		test() {
			int[][] v = int[][]();
			v.push([1, 2, 3]);
			v.pop();
		}
	`)
	assertContains(t, ir, "discard.drop")
	assertContains(t, ir, "call void @Vector.drop")
}

// B0208: Discarded Optional with user type with drop must drop inner instance.
func TestDropDiscardedOptionalUserTypePop(t *testing.T) {
	ir := generateIR(t, `
		type Res {
			int id;
			drop(~this) {}
		}
		test() {
			Res[] v = Res[]();
			v.push(Res(id: 1));
			v.pop();
		}
	`)
	assertContains(t, ir, "discard.drop")
	assertContains(t, ir, "call void @Res.drop")
}

// B0211: Optional of heap user type without drop should register pal_free cleanup.
func TestOptionalHeapTypeWithoutDropFreed(t *testing.T) {
	ir := generateIR(t, `
		type Pt {
			int x;
			int y;
		}
		test() {
			Pt? p = Pt(x: 1, y: 2);
		}
	`)
	// Should have optional drop check and pal_free
	assertContains(t, ir, "optdrop.check")
	assertContains(t, ir, "call void @pal_free")
}

// B0215: If-let unwrap should drop the inner string value at scope exit.
func TestIfUnwrapStringDrop(t *testing.T) {
	ir := generateIR(t, `
		get_optional() string? {
			return "hello";
		}
		test() {
			if v := get_optional() {
				int x = v.len;
			}
		}
	`)
	// The unwrapped string v must be dropped in the then-block.
	assertContains(t, ir, "strdrop.call")
}

// B0215: If-let unwrap from a local optional should emit string drop for the unwrapped value.
func TestIfUnwrapLocalOptionalStringDrop(t *testing.T) {
	ir := generateIR(t, `
		test() {
			string? s = "hello";
			if v := s {
				int x = v.len;
			}
		}
	`)
	// The unwrapped string v must be dropped in the then-block.
	assertContains(t, ir, "strdrop.call")
}

// B0215: While-let unwrap should drop the inner string value at each iteration end.
func TestWhileUnwrapStringDrop(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int n;
			next(~this) string? {
				if this.n <= 0 { return none; }
				this.n = this.n - 1;
				return "item";
			}
		}
		test() {
			Counter c = Counter(n: 3);
			while v := c.next() {
				int x = v.len;
			}
		}
	`)
	// The unwrapped string v must be dropped at end of each iteration.
	assertContains(t, ir, "strdrop.call")
}

// B0229: Optional structural interface variables should register drop for reassignment.
func TestOptionalStructuralInterfaceDropOnReassign(t *testing.T) {
	ir := generateIR(t, `
		type Iter is Iterator[int] {
			int val;
			next() int? { return none; }
		}
		make_iter() Iterator[int] {
			return Iter(val: 1);
		}
		main() {
			Iterator[int]? current = none;
			current = make_iter();
		}
	`)
	// B0229: Optional structural interface should have optdrop block for scope exit.
	assertContains(t, ir, "optdrop")
	// B0243: Should use RTTI-based drop dispatch (not __promise_iter_cleanup).
	// The concrete type behind the interface is unknown at compile time.
	assertContains(t, ir, "struct.drop")
}

// B0243: Optional structural interface drop in closure env must use RTTI dispatch,
// not __promise_iter_cleanup (which assumes _FnIter layout and segfaults on other types).
func TestOptionalStructuralInterfaceEnvDropRTTI(t *testing.T) {
	ir := generateIR(t, `
		type Iter is Iterator[int] {
			int val;
			next() int? { return none; }
		}
		make_iter() Iterator[int] {
			return Iter(val: 1);
		}
		wrap() () -> int? {
			Iterator[int]? current = none;
			return move || -> int? {
				current = make_iter();
				if inner := current {
					return inner.next();
				}
				return none;
			};
		}
		main() {
			() -> int? fn = wrap();
			fn();
		}
	`)
	// B0243: The env drop function should use RTTI-based dispatch for Optional[Iterator].
	// It should NOT contain __promise_iter_cleanup for the optional structural field —
	// that function assumes _FnIter memory layout and crashes on other concrete types.
	assertContains(t, ir, "optst.rtti")
}

// B0246: If-let unwrap of Optional structural interface should NOT clear the source's
// drop flag. The unwrapped structural binding doesn't get a drop registered (no concrete
// type known at compile time), so the source must retain ownership. Its reassignment-time
// Optional drop (RTTI-based) handles cleanup.
func TestIfUnwrapOptionalStructuralNoDropFlagClear(t *testing.T) {
	ir := generateIR(t, `
		type Iter is Iterator[int] {
			int val;
			next() int? { return none; }
		}
		make_iter() Iterator[int] {
			return Iter(val: 1);
		}
		wrap() () -> int? {
			Iterator[int]? current = none;
			return move || -> int? {
				current = make_iter();
				if inner := current {
					return inner.next();
				}
				return none;
			};
		}
		main() {
			() -> int? fn = wrap();
			fn();
			fn();
		}
	`)
	// B0246: The reassignment `current = make_iter()` must trigger the Optional drop
	// even after an if-let unwrap. The optdrop block should appear in the reassignment path.
	assertContains(t, ir, "optdrop.check")
	// RTTI-based drop dispatch for the structural interface inside the Optional.
	assertContains(t, ir, "struct.drop")
}

// B0240: Assigning none to an optional field with a heap user type should
// drop/free the old inner value before storing the new value.
func TestOptionalFieldReassignDrop(t *testing.T) {
	ir := generateIR(t, `
		type Location { int x; int y; }
		type Place { string name; Location? location; }
		test() {
			Place p = Place(name: "a", location: Location(x: 1, y: 2));
			p.location = none;
		}
	`)
	// The reassignment to none should emit a conditional drop for the old optional value.
	assertContains(t, ir, "field.optdrop")
	// Should free the inner Location instance.
	assertContains(t, ir, "call void @pal_free")
}

// B0240: Assigning none to an optional string field should call promise_string_drop.
func TestOptionalStringFieldReassignDrop(t *testing.T) {
	ir := generateIR(t, `
		type Holder { string? value; }
		test() {
			Holder h = Holder(value: "hello");
			h.value = none;
		}
	`)
	assertContains(t, ir, "field.optdrop")
	assertContains(t, ir, "call void @promise_string_drop")
}

// B0240: Assigning none to an optional field with a droppable user type should
// call the drop function before freeing.
func TestOptionalDroppableFieldReassignDrop(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		type Container { Resource? res; }
		test() {
			Container c = Container(res: Resource(id: 1));
			c.res = none;
		}
	`)
	assertContains(t, ir, "field.optdrop")
	assertContains(t, ir, "call void @Resource.drop")
}

// T0522 (destructure neutralization): destructuring `t!` where `t` is an
// Optional[(int, string)] local must clear t's present flag — otherwise both
// the destructured `s` and t's scope-exit optdrop will free the same heap
// string. The neutralization emits a GEP into t at index (0,0) followed by a
// `store i1 false`, which is the distinguishing IR pattern.
func TestT0522DestructureForceUnwrapNeutralizesSource(t *testing.T) {
	ir := generateIR(t, `
		test() {
			(int, string)? t = (1, "a" + "b");
			(n, s) := t!;
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	// Distinctive pattern: GEP into the source Optional alloca %t selecting
	// the present-flag field (i32 0, i32 0). Without the fix, no such GEP
	// exists — operations on %t are only the initial store and the load for
	// the unwrap / scope-exit optdrop.
	assertContains(t, testFn, "%t, i32 0, i32 0")
}

// T0522 (consume-arg Optional[string] field dup claim): when passing an
// Optional[string] field from a droppable owner to a `~` param, the inner
// string is duped and tracked as a stmt temp. The dup temp's drop flag must
// be cleared BEFORE the consume call so the stmt-end cleanup doesn't free
// the pointer the callee consumed.
//
// Distinguishing IR: between the dup's `insertvalue` reconstructing the
// Optional and the consume call, there must be a `store i1 false, i1* %flag`
// (the claim). Without the fix, only `store i1 true` precedes the call.
func TestT0522ConsumeArgOptionalStringFieldClaimsDup(t *testing.T) {
	ir := generateIR(t, `
		type _Holder { string? title; drop(~this) {} }
		_consume_opt_string(string? move s) `+"`public {}"+`
		test() {
			h := _Holder(title: "foo" + "bar");
			_consume_opt_string(h.title);
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	assertContains(t, testFn, "strdup.copy")
	callIdx := strings.Index(testFn, "@__user._consume_opt_string")
	if callIdx < 0 {
		t.Fatalf("expected consume call in test body\n%s", testFn)
	}
	// Look back ~400 chars for the strdup.merge label that contains the
	// insertvalue + claim. With the fix, both `store i1 true` (set flag) and
	// `store i1 false` (claim) precede the call. Without it, only `store i1
	// true` precedes the call.
	start := callIdx - 600
	if start < 0 {
		start = 0
	}
	preCall := testFn[start:callIdx]
	if !strings.Contains(preCall, "store i1 false, i1*") {
		t.Errorf("expected `store i1 false, i1*` (T0522 dup-temp claim) before consume call\npre-call window:\n%s", preCall)
	}
}

// T0522 (consume-arg Optional[Vector] field dup claim): same pattern as the
// string variant — the inner Vector dup must be claimed after the consume call
// returns. The dup is via `vecdup.copy` (alloc + memcpy + tag clear).
func TestT0522ConsumeArgOptionalVectorFieldClaimsDup(t *testing.T) {
	ir := generateIR(t, `
		type _Holder { Vector[int]? items; drop(~this) {} }
		_consume_opt_vec(Vector[int]? move v) `+"`public {}"+`
		test() {
			h := _Holder(items: [1, 2, 3]);
			_consume_opt_vec(h.items);
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	assertContains(t, testFn, "vecdup.copy")
	callIdx := strings.Index(testFn, "@__user._consume_opt_vec")
	if callIdx < 0 {
		t.Fatalf("expected consume call in test body\n%s", testFn)
	}
	start := callIdx - 600
	if start < 0 {
		start = 0
	}
	preCall := testFn[start:callIdx]
	if !strings.Contains(preCall, "store i1 false, i1*") {
		t.Errorf("expected `store i1 false, i1*` (T0522 dup-temp claim) before consume call\npre-call window:\n%s", preCall)
	}
}

// T0391: Returning a non-~ Optional argument from a function that returns the same
// Optional type causes the caller's drop flag to alias with the return value's
// drop binding. The alias check (extended in T0391 to recognise Optional structs)
// must clear the caller's drop flag when the inner pointers compare equal,
// preventing double-free.
func TestOptionalReturnAliasCheckClearsArgFlag(t *testing.T) {
	ir := generateIR(t, `
		type Box { int n; drop(~this) {} }
		passthrough(Box? a) Box? { return a; }
		main() {
			Box? a = Box(n: 1);
			Box? r = passthrough(a);
		}
	`)
	// Caller must emit a runtime alias check for the call result vs the arg pointer.
	// T1031: Optional[droppable] IS deep-cloned at the call site (dupOptionalVectorElem):
	// when the result aliases the still-owned source, the inner Box is cloned into the
	// source's storage so both ends are independently owned. The arg's drop flag is NOT
	// cleared — both the source and the new binding drop their own allocation once.
	assertContains(t, ir, "alias.dup")
	assertContains(t, ir, "alias.cont")
	// The inner Optional value is deep-cloned (present/absent split + heap clone).
	assertContains(t, ir, "optdup.dup")
	assertContains(t, ir, "heapdup.copy")
	// The source arg's drop flag must NOT be cleared (no ownership transfer).
	assertNotContains(t, ir, "store i1 false, i1* %a.dropflag")
}

// T0391: A nested Optional local (T??) must register a scope-exit drop binding
// so its inner heap pointer is freed. The drop emits an outer present check,
// extracts the inner Optional, then a second present check before the actual
// drop (or pal_free for heap user types without a drop method).
func TestNestedOptionalDropRecurses(t *testing.T) {
	ir := generateIR(t, `
		type Box { int n; drop(~this) {} }
		returns_double(Box? a) Box?? { return a; }
		main() {
			Box? a = Box(n: 1);
			Box?? r = returns_double(a);
		}
	`)
	// r must have its own drop flag for scope-exit cleanup.
	assertContains(t, ir, "%r.dropflag")
	// The optional drop chain must traverse two layers — the helper emits
	// nested optdrop.inner / optdrop.done blocks via recursion.
	assertContains(t, ir, "optdrop.check")
	assertContainsMatch(t, ir, `optdrop\.inner[\s\S]*optdrop\.inner`)
	// Bottom-level dispatch reaches Box.drop (the heap user type has a drop method).
	assertContains(t, ir, "call void @Box.drop")
}

// T0391: A nested Optional[string] (string??) drop reaches promise_string_drop
// at the bottom of the recursive walk via the `b.named == TypString` branch
// in emitOptionalValueDrop.
func TestNestedOptionalStringDropRecurses(t *testing.T) {
	ir := generateIR(t, `
		returns_double_str(string? a) string?? { return a; }
		main() {
			string? a = "hello";
			string?? r = returns_double_str(a);
		}
	`)
	assertContains(t, ir, "%r.dropflag")
	// Two layers of optdrop.inner (recursive walk through string?? → string? → string).
	assertContainsMatch(t, ir, `optdrop\.inner[\s\S]*optdrop\.inner`)
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0391: A nested Optional[Vector] drop reaches Vector.drop at the bottom of the
// recursive walk via the `isContainerType` branch in emitOptionalValueDrop.
func TestNestedOptionalVectorDropRecurses(t *testing.T) {
	ir := generateIR(t, `
		returns_double_vec(int[]? a) int[]?? { return a; }
		main() {
			int[]? a = [1, 2, 3];
			int[]?? r = returns_double_vec(a);
		}
	`)
	assertContains(t, ir, "%r.dropflag")
	assertContainsMatch(t, ir, `optdrop\.inner[\s\S]*optdrop\.inner`)
	assertContains(t, ir, "call void @Vector.drop")
}

// T0391: A nested Optional[enum] drop reaches the enum drop function at the
// bottom via the `extractEnum != nil` branch in emitOptionalValueDrop. The
// inner value is an enum struct stored to a temp alloca and bitcast to i8*.
func TestNestedOptionalEnumDropRecurses(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Empty, Text(string body) }
		returns_double_enum(Msg? a) Msg?? { return a; }
		main() {
			Msg? a = Msg.Text("hi");
			Msg?? r = returns_double_enum(a);
		}
	`)
	assertContains(t, ir, "%r.dropflag")
	assertContainsMatch(t, ir, `optdrop\.inner[\s\S]*optdrop\.inner`)
	assertContains(t, ir, "call void @Msg.drop")
}

// T0391: while-let on T?? must register a nested Optional drop binding for the
// unwrapped element (just like if-let). Mirror of TestNestedOptionalDropRecurses
// for genWhileUnwrapStmt's nested Optional path.
func TestWhileLetNestedOptionalDropBinding(t *testing.T) {
	ir := generateIR(t, `
		type Box { int n; drop(~this) {} }
		returns_double(Box? a) Box?? { return a; }
		main() {
			Box? a = Box(n: 1);
			Box?? r = returns_double(a);
			while x := r {
				while y := x {
					r = none;
					break;
				}
				break;
			}
		}
	`)
	// Inner while-let unwraps Box?? → Box?, so x: Box? must register an
	// Optional-drop binding (a regular drop-binding would not free the inner Box).
	assertContains(t, ir, "%x.dropflag")
	// Body of the unwrap walks through optdrop.inner blocks.
	assertContains(t, ir, "optdrop.check")
	assertContains(t, ir, "call void @Box.drop")
}

// T0620: Optional[heap-user] moved from variable into Vector[T?] literal must
// clear the source's drop flag — the vector now owns the inner payload via
// emitVectorElementDropLoop's Optional branch. Pre-T0620, this was NOT cleared
// (T0610 regression guard); now it IS cleared because Gap A is fixed.
func TestVectorLitMoveFromVarOptionalHeapClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type _Box { string label; drop(~this) {} }
		main() {
			_Box? a = _Box(label: "a");
			_Box?[] v = [a];
		}
	`)
	assertContains(t, ir, "%a.dropflag = alloca i1")
	// T0620: Gap A fix — vecElemNeedsOptionalDrop now matches, so the drop flag
	// is cleared, transferring ownership to the vector.
	assertContains(t, ir, "store i1 false, i1* %a.dropflag")
}

// T0620: Gap B fix — Vector[string?] drop must enter the element drop loop
// and emit the Optional drop branch (optfield.drop block). Pre-T0620, the
// emitVectorElementDropLoop guard early-returned for Optional elements.
func TestVectorOptionalStringElementDropLoop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? a = "hello";
			string?[] v = [a];
		}
	`)
	// The element drop loop body enters emitVariantFieldDrop → emitOptionalValueDrop,
	// which creates an "optfield.drop" block for the has-value branch.
	assertContains(t, ir, "optfield.drop")
	// The drop loop itself is emitted (vecdrop.head/body/done blocks).
	assertContains(t, ir, "vecdrop.head")
}

// T0620: Drop-on-overwrite for Vector[string?] index assign — must emit
// emitVariantFieldDrop on the old element before storing the new one.
func TestVectorOptionalStringIndexAssignDropsOld(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? a = "old";
			string?[] v = [a];
			v[0] = "new";
		}
	`)
	// The overwrite path loads the old element, drops it via emitVariantFieldDrop
	// (Optional branch → optfield.drop), then stores the new value.
	assertContains(t, ir, "optfield.drop")
}

// T0620: Dup-on-read for Vector[T?] exercises dupOptionalVectorElem branches.
// Reading v[i] into a variable must deep-dup the Optional inner so both
// the variable and the vector own independent copies.
func TestVectorOptionalDupOnReadBranches(t *testing.T) {
	// String branch — dupOptionalVectorElem → dupString
	ir := generateIR(t, `
		main() {
			string? a = "x";
			string?[] v = [a];
			string? x = v[0];
		}
	`)
	assertContains(t, ir, "optdup.dup")
	assertContains(t, ir, "optdup.merge")

	// Heap user branch — dupOptionalVectorElem → cloneHeapElement
	ir = generateIR(t, `
		type _B620 { string s; drop(~this) {} }
		main() {
			_B620? b = _B620(s: "hi");
			_B620?[] v = [b];
			_B620? x = v[0];
		}
	`)
	assertContains(t, ir, "optdup.dup")

	// Vector branch — dupOptionalVectorElem → dupVector
	ir = generateIR(t, `
		main() {
			int[]? a = [1, 2];
			int[]?[] v = [a];
			int[]? x = v[0];
		}
	`)
	assertContains(t, ir, "optdup.dup")

	// Channel branch — dupOptionalVectorElem → dupChannel
	ir = generateIR(t, `
		main() {
			channel[int]? ch = channel[int]();
			channel[int]?[] v = [ch];
			channel[int]? x = v[0];
		}
	`)
	assertContains(t, ir, "optdup.dup")

	// Arc branch — dupOptionalVectorElem → dupArc
	ir = generateIR(t, `
		main() {
			Ref[int]? a = Ref[int](1);
			Ref[int]?[] v = [a];
			Ref[int]? x = v[0];
		}
	`)
	assertContains(t, ir, "optdup.dup")
}

// B0288: is-present on method call returning T? with droppable enum inner type
// must emit a conditional drop for the temporary.
func TestIsPresentDropsTempOptionalEnum(t *testing.T) {
	ir := generateIR(t, `
		enum Val { Txt(string s), Num(int n) }
		type Box {
			Val? item;
			get_item(this) Val? {
				return this.item;
			}
		}
		main() {
			Box b = Box(item: Val.Txt(s: "hello"));
			bool ok = b.get_item() is present;
		}
	`)
	// The method call returns a temporary Val? — the enum data must be dropped.
	assertContains(t, ir, "is.temp.drop")
	assertContains(t, ir, "is.temp.skip")
}

// B0288: is-present on ident expression must NOT emit temp drop
// (the variable's scope binding handles cleanup).
func TestIsPresentIdentNoTempDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? s = "hello";
			bool ok = s is present;
		}
	`)
	assertNotContains(t, ir, "is.temp.drop")
}

// B0288: is-present on field access must NOT emit temp drop
// (the parent object owns the field data).
func TestIsPresentFieldNoTempDrop(t *testing.T) {
	ir := generateIR(t, `
		type Holder {
			string? value;
		}
		main() {
			Holder h = Holder(value: "hello");
			bool ok = h.value is present;
		}
	`)
	assertNotContains(t, ir, "is.temp.drop")
}

// B0288: is-present on method call returning string? must emit temp drop.
func TestIsPresentDropsTempOptionalString(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			string? name;
			get_name(this) string? {
				return this.name;
			}
		}
		main() {
			Box b = Box(name: "hello");
			bool ok = b.get_name() is present;
		}
	`)
	assertContains(t, ir, "is.temp.drop")
	assertContains(t, ir, "promise_string_drop")
}

// B0288: is-present on method call returning UserType? with drop() must emit
// temp drop (extract instance ptr, null-check, call drop, free).
func TestIsPresentDropsTempOptionalUserType(t *testing.T) {
	ir := generateIR(t, `
		type Handle {
			int id;
			drop(~this) {}
		}
		type Factory {
			find(this, int id) Handle? {
				if id > 0 {
					return Handle(id: id);
				}
				return none;
			}
		}
		main() {
			Factory f = Factory();
			bool ok = f.find(1) is present;
		}
	`)
	assertContains(t, ir, "is.temp.drop")
	assertContains(t, ir, "is.temp.exec")
	assertContains(t, ir, "Handle.drop")
}

// B0287: Optional unwrap on ident source must NOT track the unwrapped string
// as a statement temp (the optional's scope-exit drop handles it).
func TestOptionalUnwrapIdentNoStringTemp(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? s = "hello";
			bool eq = s! == "hello";
		}
	`)
	// The s! result should not be tracked as a string temp.
	// If it were tracked, there would be a promise_string_drop call for the temp
	// AND the optional's scope drop — double-free.
	// Count promise_string_drop calls: should be only from scope cleanup, not temp tracking.
	testFn := extractFunction(ir, "main")
	count := strings.Count(testFn, "promise_string_drop")
	// Expect at most 1 drop (the optional's scope-exit drop).
	if count > 1 {
		t.Fatalf("expected at most 1 promise_string_drop call, got %d\n%s", count, testFn)
	}
}

// T0373: Assigning a T? value into a T?? variable wraps the value once
// (lifting from single- to double-Optional). Before the fix, the wrap
// predicate skipped because the expression type was already Optional,
// leaving the T? value stored into a T?? slot → store-type panic.
func TestDoubleOptionalDeclWrapsOnce(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? a = 7;
			int?? b = a;
		}
	`)
	// b's alloca is a double-Optional struct {i1, {i1, i64}}.
	assertContains(t, ir, "%b = alloca { i1, { i1, i64 } }")
	// a wrapped once into double-Optional via two insertvalues:
	// one for the outer present flag, one for the inner T? value.
	assertContains(t, ir, "insertvalue { i1, { i1, i64 } } undef, i1 true, 0")
	assertContains(t, ir, "insertvalue { i1, { i1, i64 } } %")
}

// T0373: Reassigning a T? value into a T?? local wraps the value at the
// target's depth via insertvalue.
func TestDoubleOptionalReassignWrapsOnce(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int?? b = none;
			int? a = 5;
			b = a;
		}
	`)
	assertContains(t, ir, "%b = alloca { i1, { i1, i64 } }")
	assertContains(t, ir, "insertvalue { i1, { i1, i64 } } undef, i1 true, 0")
}

// T0373: Returning a T? expression from a T??-returning function wraps
// once at the return site.
func TestDoubleOptionalReturnWrapsOnce(t *testing.T) {
	ir := generateIR(t, `
		f(int? a) int?? {
			return a;
		}
		main() {
			int?? r = f(3);
		}
	`)
	// Function signature is T? in, T?? out (no caller-side wrap needed).
	assertContains(t, ir, "define { i1, { i1, i64 } } @__user.f({ i1, i64 } %a)")
	// Return wraps the T? value once into T?? via insertvalue.
	assertContains(t, ir, "insertvalue { i1, { i1, i64 } } undef, i1 true, 0")
}

// T0373: Value-type constructors take a distinct codegen path
// (genValueTypeConstructor) from heap-type constructors. Verify the
// value-type ctor's maybeWrapOptional wraps a T? arg into a T?? field.
func TestDoubleOptionalValueCtorWrapsOnce(t *testing.T) {
	ir := generateIR(t, `
		type VT { int?? data `+"`value;"+` }
		main() {
			int? a = 5;
			VT v = VT(data: a);
		}
	`)
	// Value-type Value struct embeds the field directly: {vtable, {i1,{i1,i64}}}.
	assertContains(t, ir, "%promise_VT_v = type { i8*, { i1, { i1, i64 } } }")
	// Field arg is wrapped once before being placed in the Value struct.
	assertContains(t, ir, "insertvalue { i1, { i1, i64 } } undef, i1 true, 0")
}

// T0428: Force-unwrap of a call-returning string? must track the extracted string
// pointer as a statement temp so it gets freed at statement end. The temp tracking
// branch (genOptionalForceUnwrap lines 8938-8949) fires for non-ident sources.
func TestT0428CallResultStringOptForceUnwrapTracksTemp(t *testing.T) {
	ir := generateIR(t, `
		get_greet() string? { return "hello"; }
		main() {
			int n = get_greet()!.len;
		}
	`)
	// The extracted string i8* must be tracked as a stmt temp with a drop at statement end.
	assertContains(t, ir, "promise_string_drop")
}

// T0428: Force-unwrap of a call-returning int[]? must track the extracted vector
// pointer as a statement temp so it gets freed at statement end.
func TestT0428CallResultVectorOptForceUnwrapTracksTemp(t *testing.T) {
	ir := generateIR(t, `
		get_nums() int[]? {
			int[] v = [1, 2, 3];
			return v;
		}
		main() {
			int n = get_nums()!.len;
		}
	`)
	// The extracted vector pointer must be tracked as a stmt temp with vector drop.
	assertContains(t, ir, "Vector.drop")
}

// T0428 Case 1: T?? field force-unwrap — neutralizeMemberOptionalField must
// look through the inner Optional to find the named type and clear the outer flag.
func TestT0428DoubleOptionalFieldNeutralization(t *testing.T) {
	ir := generateIR(t, `
		type T0428Box { int n; drop(~this) {} }
		type T0428Dbl { T0428Box?? data; }
		make_inner() T0428Box? { return T0428Box(n: 1); }
		main() {
			T0428Box? inner = make_inner();
			h := T0428Dbl(data: inner);
			b := h.data!;
		}
	`)
	// The present flag of h.data (outer Optional) must be stored false.
	// The neutralize store appears in the goroutine body, not the C main wrapper.
	assertContains(t, ir, "store i1 false")
}

// T0428 Case 2: chained MemberExpr force-unwrap — neutralizeMemberOptionalField
// must walk the chain to clear the Optional's present flag.
func TestT0428ChainedMemberForceUnwrapNeutralization(t *testing.T) {
	ir := generateIR(t, `
		type T0428Box2 { int n; drop(~this) {} }
		type T0428Inner { T0428Box2? data; }
		type T0428Outer { T0428Inner inner; }
		main() {
			o := T0428Outer(inner: T0428Inner(data: T0428Box2(n: 5)));
			b := o.inner.data!;
		}
	`)
	// The Optional present flag must be cleared via GEP into inner.data.
	assertContains(t, ir, "store i1 false")
}

// T0428 Case 3A: ~this method force-unwrap — neutralizeMemberOptionalField
// must handle ThisExpr root without calling extractInstancePtr on i8*.
func TestT0428OwnedThisForceUnwrapNeutralization(t *testing.T) {
	ir := generateIR(t, `
		type T0428Box3 { int n; drop(~this) {} }
		type T0428Holder3 {
			T0428Box3? data;
			drop(~this) {
				b := this.data!;
			}
		}
		main() {
			h := T0428Holder3(data: T0428Box3(n: 7));
		}
	`)
	dropFn := extractFunction(ir, "T0428Holder3.drop")
	if dropFn == "" {
		t.Fatal("expected T0428Holder3.drop in IR")
	}
	// Present flag must be cleared in the drop method body.
	assertContains(t, dropFn, "store i1 false")
}

// T0428 Case 3B: borrowed this.field! — genOptionalForceUnwrap must dup the
// inner heap value so both the caller's synth drop and the local own independent copies.
func TestT0428BorrowedThisForceUnwrapDup(t *testing.T) {
	ir := generateIR(t, `
		type T0428Box4 { int n; drop(~this) {} }
		type T0428Holder4 {
			T0428Box4? data;
			get_n(this) int {
				b := this.data!;
				return b.n;
			}
		}
		main() {
			h := T0428Holder4(data: T0428Box4(n: 3));
			n := h.get_n();
		}
	`)
	// The get_n method should call dupHeapValue logic: alloc + memcpy.
	getNFn := extractFunction(ir, "T0428Holder4.get_n")
	if getNFn == "" {
		t.Fatal("expected T0428Holder4.get_n in IR")
	}
	// dupHeapValue allocates new memory and memcpy's the instance.
	assertContains(t, getNFn, "call i8* @pal_alloc")
	assertContains(t, getNFn, "call void @llvm.memcpy")
}

// T0436 Issue 1: single-line `b := h.data!!` on a T?? field — the AST is
// OptionalUnwrapExpr(OptionalUnwrapExpr(MemberExpr)), so neutralizeForceUnwrapSource
// must look through the inner OptionalUnwrapExpr to reach the MemberExpr.
// Without the fix, the outer Optional's present flag stays true → double-free
// when the holder is dropped.
func TestT0436SingleLineDoubleUnwrapNeutralizes(t *testing.T) {
	ir := generateIR(t, `
		type T0436Box1 { int n; drop(~this) {} }
		type T0436Dbl1 { T0436Box1?? data; }
		make_inner1() T0436Box1?? {
			T0436Box1? inner = T0436Box1(n: 1);
			return inner;
		}
		main() {
			h := T0436Dbl1(data: make_inner1());
			b := h.data!!;
		}
	`)
	// neutralizeMemberOptionalField clears the outermost Optional's present flag
	// by GEPing into the field's full T?? layout `{ i1, { i1, T_v } }` and then
	// storing i1 false. Without the nested-unwrap walk in
	// neutralizeForceUnwrapSource, no such GEP is emitted (only loads for the
	// drop and unwrap exist) and the holder's drop double-frees the heap value.
	gepPattern := "getelementptr { i1, { i1, { i8*, i8* } } }, { i1, { i1, { i8*, i8* } } }* %"
	if !strings.Contains(ir, gepPattern) {
		t.Fatal("expected a GEP through the T?? outer-Optional struct (neutralization site)")
	}
}

// T0577: `b := (opt!);` — ParenExpr wrapping a force-unwrap. Before the fix,
// neutralizeForceUnwrapSource matched only OptionalUnwrapExpr/CastExpr/
// ErrorHandlerExpr at its outer switch and fell through for *ast.ParenExpr,
// so the source optional's present flag was never cleared and its scope-exit
// drop re-freed the inner value (double-free → segfault). The fix peels
// ParenExpr at the top of the function before the switch.
func TestT0577ParenForceUnwrapNeutralizes(t *testing.T) {
	ir := generateIR(t, `
		type T0577Box { int n; drop(~this) {} }
		main() {
			T0577Box? opt = T0577Box(n: 1);
			b := (opt!);
		}
	`)
	// Locate the unwrap.ok block emitted for `(opt!)` and confirm the present-flag
	// store appears in that block. Without the paren peel, no GEP+store i1 false
	// is emitted in unwrap.ok — only the b store and drop-flag set — and the
	// optional's scope drop later double-frees.
	unwrapOKIdx := strings.Index(ir, "\nunwrap.ok")
	if unwrapOKIdx < 0 {
		t.Fatal("expected an unwrap.ok block in IR")
	}
	rest := ir[unwrapOKIdx:]
	endIdx := strings.Index(rest, "\n\n")
	if endIdx < 0 {
		endIdx = len(rest)
	}
	block := rest[:endIdx]
	if !strings.Contains(block, "getelementptr { i1, { i8*, i8* } }, { i1, { i8*, i8* } }* %opt") {
		t.Fatalf("expected GEP into opt's Optional struct in unwrap.ok block (neutralization site); got:\n%s", block)
	}
	if !strings.Contains(block, "store i1 false") {
		t.Fatalf("expected `store i1 false` neutralization store in unwrap.ok block; got:\n%s", block)
	}
}

// T0577 mirror: `b := (opt)!;` — ParenExpr inside the OptionalUnwrap's `.Expr`.
// The outer switch matches OptionalUnwrapExpr, but `inner` is then ParenExpr,
// so the inner T0436-style chain walk (which previously only peeled
// OptionalUnwrapExpr) must also peel ParenExpr to reach IdentExpr. Without
// the inner peel, neither the IdentExpr nor MemberExpr arm fires, the present
// flag is never cleared, and the optional's scope-exit drop double-frees.
//
// IR shape note: this form goes through the heap-claim path rather than the
// `unwrap.ok` flag-store path used by `(opt!)`, so we assert the present-flag
// GEP+store appears anywhere in main rather than locating a specific block.
func TestT0577InnerParenForceUnwrapNeutralizes(t *testing.T) {
	ir := generateIR(t, `
		type T0577Box2 { int n; drop(~this) {} }
		main() {
			T0577Box2? opt = T0577Box2(n: 1);
			b := (opt)!;
		}
	`)
	// The exact pattern emitted by the IdentExpr arm of neutralizeForceUnwrapSource:
	// GEP through the Optional struct on %opt followed by `store i1 false`.
	// Without the inner ParenExpr peel, no such GEP/store pair is emitted on %opt.
	gep := "getelementptr { i1, { i8*, i8* } }, { i1, { i8*, i8* } }* %opt, i32 0, i32 0"
	if !strings.Contains(ir, gep) {
		t.Fatalf("expected GEP into opt's Optional struct (neutralization site) — inner ParenExpr peel did not reach IdentExpr arm")
	}
	// Confirm the GEP is followed by a `store i1 false` neutralization.
	gepIdx := strings.Index(ir, gep)
	tail := ir[gepIdx:]
	endIdx := strings.Index(tail, "\n\n")
	if endIdx < 0 {
		endIdx = len(tail)
	}
	region := tail[:endIdx]
	if !strings.Contains(region, "store i1 false") {
		t.Fatalf("expected `store i1 false` after GEP into %%opt; got:\n%s", region)
	}
}

// T0419: Optional[T] with explicit user drop must dispatch the scope-exit drop
// through T.drop$wrap (which calls drop + pal_free), not the bare T.drop
// (which only runs the user body and leaks the heap allocation).
func TestOptionalLocalDropExplicitUserDropWrap(t *testing.T) {
	ir := generateIR(t, `
		type BoxDrop {
			int n;
			drop(~this) {}
		}
		test_no_unwrap() {
			BoxDrop? a = BoxDrop(n: 12);
		}
	`)
	// The Optional drop dispatch must call the $wrap variant.
	assertContains(t, ir, "@BoxDrop.drop$wrap")
	// The $wrap function itself must call both drop and pal_free.
	wrapFn := extractFunction(ir, "BoxDrop.drop$wrap")
	if wrapFn == "" {
		t.Fatal("expected BoxDrop.drop$wrap in IR")
	}
	assertContains(t, wrapFn, "call void @BoxDrop.drop")
	assertContains(t, wrapFn, "call void @pal_free")
}

// T0419: Optional[T] where T has only a SYNTHESIZED drop (auto-generated because
// of droppable fields) must dispatch the bare T.drop — NOT T.drop$wrap. Synth
// drops already include pal_free; wrapping would call pal_free twice.
// This guards the `if explicitDrop` check in maybeRegisterOptionalDrop.
func TestOptionalLocalDropSynthSkipsWrap(t *testing.T) {
	ir := generateIR(t, `
		type SynthDropBox {
			string s;
		}
		test_synth_no_unwrap() {
			SynthDropBox? a = SynthDropBox(s: "hello");
		}
	`)
	// Premise: SynthDropBox has a synthesized drop that includes pal_free.
	synthFn := extractFunction(ir, "SynthDropBox.drop")
	if synthFn == "" {
		t.Fatal("expected SynthDropBox.drop in IR")
	}
	assertContains(t, synthFn, "call void @promise_string_drop")
	assertContains(t, synthFn, "call void @pal_free")
	// The Optional drop dispatch must call the bare drop, NOT the wrapper.
	// (No SynthDropBox.drop$wrap function should be emitted at all.)
	assertNotContains(t, ir, "SynthDropBox.drop$wrap")
	// And the user function must dispatch directly to SynthDropBox.drop.
	userFn := extractFunction(ir, "__user.test_synth_no_unwrap")
	if userFn == "" {
		t.Fatal("expected __user.test_synth_no_unwrap in IR")
	}
	assertContains(t, userFn, "call void @SynthDropBox.drop(")
}

// Optional cast (`this as T`) through `this`. Before the fix this panicked in
// codegen inside wrapOptional ("store operands are not compatible: src=i8*;
// dst={ i8*, i8* }*"). generateIR not panicking is itself the regression guard.
func TestThisCastOptionalNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
		type T0747OBase {
			int n;
			whoami(this) string `+"`abstract"+`;
			is_der(this) bool {
				T0747ODerived? o = this as T0747ODerived;
				if o { return true; }
				return false;
			}
		}
		type T0747ODerived is T0747OBase { whoami(this) string { return "d"; } }
		main() { T0747OBase b = T0747ODerived(n: 7); _ := b.is_der(); }
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	if extractFunction(ir, "T0747OBase.is_der") == "" {
		t.Fatal("expected T0747OBase.is_der in IR")
	}
}

// T0849: the optional `as` form (Force == false, result `Circle?`) is a
// *conditional* move: the subject is aliased into the result only on a
// successful downcast; on failure the result is None and the subject must still
// be dropped. genReturnStmt routes the cast subject through
// consumeCastSubjectDropFlag, which — for the non-Force form — stores `!isMatch`
// into the subject's drop flag instead of clearing it unconditionally. So the
// subject's scope-exit drop fires iff the downcast failed: no double-free on
// success (was SEGV), no leak on failure. The IR signature is an
// `xor i1 %isMatch, true` feeding a `store ..., i1* %s.dropflag`, NOT an
// unconditional `store i1 false`.
func TestT0849_ReturnOptionalCastConditionalSubjectDrop(t *testing.T) {
	ir := generateIR(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape { f64 radius; area(this) f64 { return this.radius; } }
		helper(int dummy) Circle? {
			Shape s = Circle(name: "src", radius: 2.0);
			return s as Circle;
		}
		main() { _ := helper(0); }
	`)
	fn := extractFunction(ir, "__user.helper")
	if fn == "" {
		t.Fatal("expected __user.helper in IR")
	}
	// The drop flag is set to the negated downcast-success flag (drop iff the
	// cast failed): `%n = xor i1 %isMatch, true` then `store i1 %n, ... s.dropflag`.
	assertContainsMatch(t, fn, `%\w+ = xor i1 %\w+, true\n\s*store i1 %\w+, i1\* %s\.dropflag`)
	// The conditional drop still executes at scope exit (flag is loaded) ...
	assertContains(t, fn, "load i1, i1* %s.dropflag")
	// ... and it is NOT an unconditional clear (that would leak on a failed
	// downcast — the pre-T0849 buggy shape).
	assertNotContains(t, fn, "store i1 false, i1* %s.dropflag")
}

// T0849 owning-slot sibling: `Box(c: s as Circle)` stores the conditional
// success flag into the field-init constructor's subject drop flag the same way
// (drop iff the cast failed). Before T0849 this site cleared the flag
// unconditionally (`store i1 false`) → leak on the failure path.
func TestT0849_OwningSlotOptionalCastConditionalDrop(t *testing.T) {
	ir := generateIR(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape { f64 radius; area(this) f64 { return this.radius; } }
		type Box { Circle? c; }
		helper(int dummy) bool {
			Shape s = Circle(name: "src", radius: 2.0);
			b := Box(c: s as Circle);
			return true;
		}
		main() { _ := helper(0); }
	`)
	fn := extractFunction(ir, "__user.helper")
	if fn == "" {
		t.Fatal("expected __user.helper in IR")
	}
	// Conditional store of the negated success flag into the subject's drop flag.
	assertContainsMatch(t, fn, `%\w+ = xor i1 %\w+, true\n\s*store i1 %\w+, i1\* %s\.dropflag`)
	// Not an unconditional clear (the pre-T0849 leak-on-failure shape).
	assertNotContains(t, fn, "store i1 false, i1* %s.dropflag")
}

// T0849 (wasm exposure): a closure call must coerce its arguments to the
// signature's parameter types the same way a regular call does. An optional
// param `int?` typed the indirect-call function pointer as `(i8*, {i1, i64})`,
// but the bare `none` / `5` argument was passed uncoerced as the scalar
// discriminant. The resulting type-mismatched call was tolerated by the x86
// backend but lowered to invalid WebAssembly. The fix routes closure args
// through coerceCallArgs (optional wrapping), so the call passes the full
// `{i1, i64}` aggregate.
func TestT0849ClosureCallCoercesOptionalArg(t *testing.T) {
	ir := generateIR(t, `
		none_arg() bool {
			apply := |int? x| -> bool { return true; };
			return apply(none);
		}
		bare_arg() bool {
			apply := |int? x| -> bool { return true; };
			return apply(5);
		}
		main() { _ := none_arg(); _ := bare_arg(); }
	`)
	none := extractFunction(ir, "__user.none_arg")
	if none == "" {
		t.Fatal("expected __user.none_arg in IR")
	}
	// `none` → zeroinitialized `{i1,i64}` aggregate passed to the closure call.
	// Before the fix this was a bare `i1 false`, mismatching the `{i1,i64}` param.
	assertContains(t, none, "{ i1, i64 } zeroinitializer)")

	bare := extractFunction(ir, "__user.bare_arg")
	if bare == "" {
		t.Fatal("expected __user.bare_arg in IR")
	}
	// `5` → wrapped `{i1 true, i64 5}` aggregate (insertvalue chain), not a bare i64.
	assertContainsMatch(t, bare, `insertvalue \{ i1, i64 \} %\w+, i64 5, 1`)
	// The closure call receives the aggregate, never a bare scalar second arg.
	assertContainsMatch(t, bare, `call i1 %\w+\(i8\* %\w+, \{ i1, i64 \} %\w+\)`)
}
