package codegen

import (
	"strings"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
)

// --- Variable tests ---

func TestVariableAllocaAndLoad(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 10;
			int y = x;
		}
	`)
	assertContains(t, ir, "alloca i64")
	assertContains(t, ir, "store i64 10")
	assertContains(t, ir, "load i64")
}

func TestStringEscapeCloseBrace(t *testing.T) {
	// B0124: \} should resolve to literal }
	ir := generateIR(t, `main() { s := "a\}b"; }`)
	assertContains(t, ir, `c"a}b"`)
}

func TestStringEscapeCloseBraceOnly(t *testing.T) {
	ir := generateIR(t, `main() { s := "\}"; }`)
	assertContains(t, ir, `c"}"`)
}

func TestStringDropFuncBody(t *testing.T) {
	ir := generateIR(t, `main() { s := "x"; }`)
	// T0093: promise_string_drop null-checks the pointer (for null fields in
	// synthesized drops), then checks bit 63 (literal flag), then conditionally frees
	assertContains(t, ir, "define void @promise_string_drop(i8* %ptr)")
	assertContains(t, ir, "icmp eq i8* %ptr, null")
	assertContains(t, ir, "icmp ne i64")
	assertContains(t, ir, "call void @pal_free(")
}

// T0061: String drop binding is registered at scope exit
func TestStringDropScopeBinding(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "hello";
		}
	`)
	// String drop binding: drop flag alloca, conditional call to promise_string_drop
	assertContains(t, ir, "strdrop.call")
	assertContains(t, ir, "strdrop.skip")
	assertContains(t, ir, "call void @promise_string_drop(")
}

// T0061: String drop flag is cleared when returning a string
func TestStringDropReturnClearsFlag(t *testing.T) {
	ir := generateIR(t, `
		make_name() string {
			s := "bob";
			return s;
		}
		main() { make_name(); }
	`)
	// The return should clear the drop flag (store i1 false) before scope cleanup
	assertContains(t, ir, "strdrop.skip")
}

// T0061: String drop flag IS cleared when passing to a function (same as user types)
func TestStringDropClearedOnFuncArg(t *testing.T) {
	ir := generateIR(t, `
		consume(string s) {}
		main() {
			s := "hello";
			consume(s);
		}
	`)
	// The string drop binding exists but flag is cleared at the call site,
	// so the conditional drop at scope exit is a no-op (skips).
	assertContains(t, ir, "strdrop.call")
	assertContains(t, ir, "call void @promise_string_drop(")
}

// T0061: String drop flag is cleared on assignment (move)
func TestStringDropClearedOnAssignment(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := "hello";
			b := a;
		}
	`)
	// Both a and b should have drop bindings
	// a's flag should be cleared (moved to b)
	assertContains(t, ir, "strdrop.call")
}

// T0061: String borrowed from struct field should NOT have active drop
func TestStringDropBorrowFromField(t *testing.T) {
	ir := generateIR(t, `
		type Person { string name; }
		main() {
			p := Person(name: "alice");
			field_val := p.name;
		}
	`)
	// field_val gets a drop binding but flag is immediately cleared (borrow from field)
	assertContains(t, ir, "strdrop.call")
}

// T0061: String borrowed from vector index should NOT have active drop
func TestStringDropBorrowFromIndex(t *testing.T) {
	ir := generateIR(t, `
		main() {
			names := string[]();
			names.push("alice");
			elem := names[0];
		}
	`)
	// elem gets a drop binding but flag is immediately cleared (borrow from vector)
	assertContains(t, ir, "strdrop.call")
}

// T0064: Vector drop binding is registered at scope exit
func TestVectorDropScopeBinding(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := int[]();
			v.push(1);
		}
	`)
	// Vector drop binding: strdrop.call block (reuses bindingDropString mechanism)
	assertContains(t, ir, "strdrop.call")
	assertContains(t, ir, "call void @Vector.drop(")
}

// B0223: Vector slice intermediate in from_bytes must be tracked as a heap temp
// and dropped at statement end (via Vector.drop).
func TestVectorSliceTempDroppedInFromBytes(t *testing.T) {
	ir := generateIR(t, `
		take(string s) {}
		main() {
			u8[] buf = u8[].filled(65u8, 10);
			take(string.from_bytes(buf[0:5]));
		}
	`)
	// The vector slice result should be dropped via Vector.drop at statement end
	assertContains(t, ir, "call void @Vector.drop(")
}

func TestPalAllocDefined(t *testing.T) {
	ir := generateIR(t, `main() { x := 42; }`)
	assertContains(t, ir, "declare noalias i8* @malloc(i64 noundef %size) nounwind willreturn")
	assertContains(t, ir, "@pal_alloc(i64 %size)")
}

func TestDropNullSafe(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		make!() Resource { return Resource(id: 1); }
		main() {
			Resource r = make() ? e { return; };
		}
	`)
	// Drop should null-check instance pointer before calling drop
	assertContains(t, ir, "drop.exec")
	assertContains(t, ir, "drop.done")
}

func TestTupleInterpolationTracksTemps(t *testing.T) {
	ir := generateIR(t, `
		main() {
			(int, int) pair = (1, 2);
			string s = "{pair}";
		}
	`)
	// B0254: convertTupleToString must track per-element convertToString results
	// as string temps so they get freed. Verify promise_string_drop is emitted.
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0937: an inline (unbound) elvis whose result is a value-struct container
// (map[K,V] / Set[T] — a 2-word {i8*, i8*} Value struct, not a bare i8*) on an
// owned-local source must register the result as a heap drop temp with a
// per-branch flag: owned (true) on the some-path where the extracted inner is
// orphaned, borrowed (false) on the none-path where the default keeps its owner.
// Without this the some-path inner leaks (the i8*-only trackElvisResultTemp path
// skips value structs).
func TestElvisMapResultDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			map[string, int]? a = {"x": 1};
			map[string, int] b = {"z": 9};
			c := (a ?: b).len;
		}
	`)
	assertContains(t, ir, "elvis.merge")
	// Per-branch live flag: true on the some-path, false on the none-path.
	assertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
	// The tracked heap temp dispatches to the synthesized Map drop.
	assertContains(t, ir, `call void @"Map[string, int].drop"`)
}

// T0937: a member-source elvis (`bx.m ?: b`) is owner-governed — the extracted
// inner aliases the owned field and the container's own drop frees it. The elvis
// result must NOT be tracked (tracking would double-free), so no per-branch
// owned-flag phi is emitted.
func TestElvisMapMemberSourceNoDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type MBox { map[string, int]? m; }
		main() {
			MBox bx = MBox(m: {"x": 1});
			map[string, int] b = {"z": 9};
			c := (bx.m ?: b).len;
		}
	`)
	assertContains(t, ir, "elvis.merge")
	assertNotContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
}

// T0937 (i8*-container gap): the i8* result path (trackElvisResultTemp, T0935)
// applies the same orphan classifier as the value-struct path. An owned-local
// string[] source is orphaned on the some-path → tracked with a per-branch flag.
// Here the none-path default `b` is ALSO an owned local, so T0936 neutralizes its
// scope-exit owner and the result owns it on the none-path too — the flag phi is
// [true, true] (the `true` on the some-incoming is what proves orphan tracking).
func TestElvisStrvecOwnedLocalDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[]? a = ["x" + "y"];
			string[] b = ["z" + "w"];
			c := (a ?: b).len;
		}
	`)
	assertContains(t, ir, "elvis.merge")
	// Per-branch live flag drives the stmt-temp Vector.drop dispatch: owned on the
	// some-path (orphaned inner) and on the none-path (neutralized local default).
	assertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ true, %elvis\.none`)
}

// T0937 (i8*-container gap): a member-source string[] elvis (`bx.v ?: b`) aliases
// the owned field; the container's drop frees it. The inline result must NOT be
// tracked (tracking double-freed the elements → use-after-free crash before the
// gate was applied to the i8* path), so no per-branch flag phi is emitted.
func TestElvisStrvecMemberSourceNoDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type SVBox { string[]? v; }
		main() {
			SVBox bx = SVBox(v: ["x" + "y"]);
			string[] b = ["z" + "w"];
			c := (bx.v ?: b).len;
		}
	`)
	assertContains(t, ir, "elvis.merge")
	assertNotContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
}

// T0937: an index-source elvis (`v[i] ?: b`) is owner-governed — the extracted
// inner aliases the container element, which the container's own drop frees. The
// result must NOT be tracked (tracking would double-free), so no per-branch
// owned-flag phi is emitted. Exercises the IndexExpr arm of
// elvisSomeInnerOrphaned (the ident/member tests cover the other arms).
func TestElvisMapIndexSourceNoDropFlag(t *testing.T) {
	ir := generateIR(t, `
		main() {
			(map[string, int]?)[] v = [];
			v.push({"x": 1});
			map[string, int] b = {"z": 9};
			c := (v[0] ?: b).len;
		}
	`)
	assertContains(t, ir, "elvis.merge")
	assertNotContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
}

// T0937 (T0924 subsumption): a heap *user* type (a droppable non-value type, also
// a 2-word {i8*, i8*} Value struct) used inline on an owned-local source goes
// through the SAME trackElvisResultHeap path as Map/Set — distinct from the i8*
// container path. It must emit the per-branch owned-flag phi and dispatch the
// tracked temp to the type's own synthesized drop (@HVal.drop). This proves the
// heap-user representation arm fires (the Map tests only cover the container arm
// of the shared function); without it the some-path inner leaks (T0924).
func TestElvisHeapUserResultDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type HVal { string s; }
		main() {
			HVal? a = HVal(s: "a" + "b");
			HVal b = HVal(s: "c" + "d");
			c := (a ?: b).s.len;
		}
	`)
	assertContains(t, ir, "elvis.merge")
	// Per-branch live flag: owned (true) on some, borrowed (false) on none.
	assertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
	// The tracked heap temp dispatches to the user type's own drop.
	assertContains(t, ir, "call void @HVal.drop")
}

// T0933/T0940: `m := a ?: b` with BORROWED-PARAMETER operands. The binding's drop
// flag must be driven by a per-path phi — false on the some path (the inner belongs
// to the caller-owned param `a`, not the binding) AND false on the none path (the
// default `b` is a borrowed param whose owner is the caller, so `m` borrows it —
// owning it would double-free the caller's buffer at scope exit, the bound none-path
// SEGV T0940 fixes). This overrides the unconditional `store i1 true` that
// maybeRegisterDrop emits. (T0940 generalizes T0933's heap-user-only flag and
// corrects the none-path: it owns only when the default's owner was neutralized.)
func TestElvisBoundBorrowedParamFlag(t *testing.T) {
	ir := generateIR(t, `
		type HVal { string s; }
		f(HVal? a, HVal b) { m := a ?: b; }
		main() { HVal? a = HVal(s: "a" + "b"); HVal c = HVal(s: "c" + "d"); f(a, c); }
	`)
	// The bound override phi borrows on both paths (both operands caller-owned).
	assertContainsMatch(t, ir, `phi i1 \[ false, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
	// The override store follows maybeRegisterDrop's unconditional 1.
	assertContains(t, ir, "store i1 true, i1* %m.dropflag")
	assertContainsMatch(t, ir, `store i1 %[0-9]+, i1\* %m\.dropflag`)
}

// T0940: a STRING elvis bound result (i8* container) is covered by the per-path bound
// flag too — elvisResultDrop classifies string (with Vector) as the vecOrStr arm, so
// `m := a ?: b` with both operands borrowed params gets a phi[false, false] override of
// maybeRegisterDrop's unconditional `store i1 true`. Pre-T0940 the string-bound case
// kept the unconditional owning drop and freed the caller-owned default a second time
// (`@promise_string_drop` on a borrowed buffer → UAF). The bound flag makes `m` borrow
// on both paths, matching the Map/heap-user borrowed-param arm.
func TestElvisBoundStringBorrowsBoth(t *testing.T) {
	ir := generateIR(t, `
		f(string? a, string b) { m := a ?: b; }
		main() { string? a = "x"; string b = "y"; f(a, b); }
	`)
	// Borrows on both paths (both operands caller-owned); never the old owning shapes.
	assertContainsMatch(t, ir, `phi i1 \[ false, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
	assertNotContainsMatch(t, ir, `phi i1 \[ false, %elvis\.some[^]]*\], \[ true, %elvis\.none`)
	assertNotContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ true, %elvis\.none`)
	// The per-path flag overrides the unconditional owning drop.
	assertContains(t, ir, "store i1 true, i1* %m.dropflag")
	assertContainsMatch(t, ir, `store i1 %[0-9]+, i1\* %m\.dropflag`)
}

func TestStaticVectorDrop(t *testing.T) {
	ir := generateIR(t, `main() { x := [1, 2, 3]; }`)
	// Drop function should check bit 63 before freeing
	assertContains(t, ir, "define void @Vector.drop(")
	assertContains(t, ir, "check_static")
}

// T0579: Array field with heap user type that has no explicit drop —
// exercises the "heap user without explicit drop" branch in
// `typeNeedsFieldDrop` (must return true so the per-element `pal_free` fires).
func TestFixedArrayFieldHeapNoDropElement(t *testing.T) {
	ir := generateIR(t, `
		type _Bare { int x; }
		type _BareArr { _Bare[2] data; }
		main() {
			b := _BareArr(data: [_Bare(x: 1), _Bare(x: 2)]);
		}
	`)
	// The element type has no drop method, so the synth drop must fall back to
	// pal_free per element.
	assertContains(t, ir, "call void @pal_free")
}

// T0583: `arr[i] = newVal` on a fixed-size array of droppable elements must
// emit a drop call for the old slot value before storing the new one.
// Before the fix, `genArrayIndexAssign` did a bare `NewStore` and leaked the
// previous allocation. The IR for a heap-user element with explicit drop must
// load the old element, drop it (Type.drop + pal_free), then store the new.
func TestFixedArrayIndexAssignDropsOldHeapUser(t *testing.T) {
	ir := generateIR(t, `
		type _Box { int n; drop(~this) {} }
		main() {
			_Box[2] arr = [_Box(n: 1), _Box(n: 2)];
			_Box c = _Box(n: 3);
			arr[1] = c;
		}
	`)
	// After bounds-check OK, the IR must drop the previous element before storing.
	assertContains(t, ir, "arrassign.ok")
	assertContains(t, ir, "call void @_Box.drop")
}

// T0583: String element — overwrite must call promise_string_drop on the old slot.
func TestFixedArrayIndexAssignDropsOldString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[2] arr = ["alpha", "beta"];
			arr[1] = "gamma";
		}
	`)
	assertContains(t, ir, "arrassign.ok")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0583: String compound assignment — `arr[i] += s` must drop the old string
// after computing the concat result and before storing it.
func TestFixedArrayIndexCompoundAssignDropsOldString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[2] arr = ["foo", "bar"];
			arr[1] += "baz";
		}
	`)
	assertContains(t, ir, "arrassign.ok")
	// Compound path goes through emitStringDropOldValue.
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0583: Vector element — overwrite must call Vector.drop on the old slot.
func TestFixedArrayIndexAssignDropsOldVector(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v0 := Vector[int]();
			v0.push(10);
			v1 := Vector[int]();
			v1.push(20);
			Vector[int][2] arr = [v0, v1];
			v2 := Vector[int]();
			v2.push(30);
			arr[1] = v2;
		}
	`)
	assertContains(t, ir, "arrassign.ok")
	assertContains(t, ir, "call void @Vector.drop")
}

// T0583: Primitive element (no drop needed) — `arr[i] = val` must NOT emit a
// load-and-drop dance for the old slot. Confirms typeNeedsFieldDrop correctly
// gates the new code path: for ints, the arrassign.ok block goes directly from
// GEP to store without any intervening load of the old value.
func TestFixedArrayIndexAssignNoDropForPrimitive(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[3] arr = [1, 2, 3];
			arr[1] = 42;
		}
	`)
	assertContains(t, ir, "arrassign.ok")
	// For primitives the codegen emits: GEP into [3 x i64] then `store i64 42`
	// immediately. No old-value load or drop call appears between them.
	assertContainsMatch(t, ir,
		`getelementptr \[3 x i64\][^\n]*\n[^\n]*store i64 42`)
}

// T0590: genArrayIndex had no dup-on-read, so any read from a fixed-size array
// slot returned an alias. Combined with T0583's drop-on-overwrite, slot-to-slot
// copies and let-then-X reads on droppable elements produced double-frees.
// Tests below verify each dup branch fires.

func TestFixedArrayIndexDupsString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := "first";
			a = a + "+";
			b := "second";
			b = b + "+";
			string[2] arr = [a, b];
			string x = arr[0];
		}
	`)
	// After the array index load, genArrayIndex must call promise_string_new
	// (via dupString) so x owns an independent copy.
	assertContains(t, ir, "arridx.ok")
	assertContains(t, ir, "call i8* @promise_string_new(")
}

func TestFixedArrayIndexDupsHeapUser(t *testing.T) {
	ir := generateIR(t, `
		type _B { int n; drop(~this) {} }
		main() {
			_B[2] arr = [_B(n: 1), _B(n: 2)];
			_B x = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")
	// dupHeapValue path: pal_alloc + memcpy for the new instance.
	assertContains(t, ir, "call i8* @pal_alloc(")
	assertContains(t, ir, "call void @llvm.memcpy")
}

func TestFixedArrayIndexDupsVector(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v0 := Vector[int]();
			v0.push(1);
			v1 := Vector[int]();
			v1.push(2);
			Vector[int][2] arr = [v0, v1];
			Vector[int] x = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")
	// dupVector: pal_alloc for the new buffer + memcpy from the old one.
	assertContains(t, ir, "call i8* @pal_alloc(")
}

func TestFixedArrayIndexNoDupForPrimitive(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[2] arr = [10, 20];
			int x = arr[0];
		}
	`)
	// Primitives use the bare load — no dup helper. Check the main goroutine
	// specifically (std library code emitted in the same IR may reference
	// promise_string_new for other reasons).
	assertContains(t, ir, "arridx.ok")
	mainIR := extractFunction(ir, ".goroutine.main")
	if strings.Contains(mainIR, "promise_string_new") || strings.Contains(mainIR, "pal_alloc") {
		t.Errorf("expected main goroutine to have no dup helper for primitive array index, got:\n%s", mainIR)
	}
}

func TestFixedArrayIndexAssignSlotToSlotDupsHeapUser(t *testing.T) {
	// T0590: slot-to-slot copy (`arr[1] = arr[0]`) must dup on RHS read, then
	// drop-on-overwrite frees the previous arr[1] (T0583), then stores the dup.
	ir := generateIR(t, `
		type _B { int n; drop(~this) {} }
		main() {
			_B[2] arr = [_B(n: 1), _B(n: 2)];
			arr[1] = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")    // RHS read
	assertContains(t, ir, "arrassign.ok") // LHS assign with drop-on-overwrite
	// Must see both: drop-on-overwrite (drop of old arr[1]) and dup (clone of arr[0]).
	assertContains(t, ir, "call void @_B.drop")
	assertContains(t, ir, "call i8* @pal_alloc(")
}

// T0802: `s = obj.field` reassignment of a heap string field must clone-on-read,
// exactly like the var-decl path. Before the fix, genAssignStmt only set the
// dup-on-read flags for an IndexExpr RHS, so a MemberExpr (field-access) RHS
// aliased the field pointer into s with s's drop flag set — both s and obj's drop
// then freed the same allocation (double-free, latent on linux / SIGABRT on
// macOS). genFieldAccess emits the clone via dupString (promise_string_new).
func TestReassignStringFieldClones(t *testing.T) {
	ir := generateIR(t, `
		type _T { int id; string label; drop(~this) {} }
		probe() {
			_T t = _T(id: 1, label: "lit");
			string s = "";
			s = t.label;
		}
		main() { probe(); }
	`)
	// Isolate the user code (string literals are .rodata, so the only
	// promise_string_new in probe() is the field-read clone).
	probeIR := extractFunction(ir, "__user.probe")
	assertContains(t, probeIR, "call i8* @promise_string_new(")
}

// T0802 control: a reassignment whose RHS is NOT a field read must not emit the
// field-clone helper — proves the dup is driven by the MemberExpr RHS, not always.
func TestReassignNonFieldRhsNoClone(t *testing.T) {
	ir := generateIR(t, `
		type _T { int id; string label; drop(~this) {} }
		probe() {
			_T t = _T(id: 1, label: "lit");
			string s = "";
			s = "other";
		}
		main() { probe(); }
	`)
	probeIR := extractFunction(ir, "__user.probe")
	if strings.Contains(probeIR, "call i8* @promise_string_new(") {
		t.Errorf("expected no field-clone for non-field RHS reassignment, got:\n%s", probeIR)
	}
}

// T0590 coverage additions: confirm each remaining dup branch in genArrayIndex
// actually emits the per-type dup helper. The original 6 tests covered string,
// heap user, Optional<heap user>, Vector, and the primitive negative case; the
// tests below fill in tuple, Optional[string], heap-user-no-drop (_Bare),
// channel, Arc, and Weak.

func TestFixedArrayIndexDupsTuple(t *testing.T) {
	// Droppable tuple element: dup must walk the tuple's droppable inner
	// (string) so both slots own independent string allocations.
	ir := generateIR(t, `
		main() {
			(string, int)[2] arr = [("first", 1), ("second", 2)];
			(string, int) t = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")
	// dupTupleValue: emits a per-field dup; for the string sub-field this
	// is promise_string_new. With static-literal flag, copy-on-write may
	// route through promise_vector_cow style — but the dup-on-read path
	// always emits promise_string_new for the inner string.
	assertContains(t, ir, "call i8* @promise_string_new(")
}

func TestFixedArrayIndexDupsHeapUserNoDrop(t *testing.T) {
	// _Bare: heap user with no explicit drop / no synth drop.
	// dup-on-read must still fire via the isHeapUserNoDropPalFree branch
	// (otherwise both slots alias one pal_free'd allocation).
	ir := generateIR(t, `
		type _Bare { int x; }
		main() {
			_Bare[2] arr = [_Bare(x: 1), _Bare(x: 2)];
			_Bare x = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")
	// dupHeapValue path: pal_alloc + memcpy
	assertContains(t, ir, "call i8* @pal_alloc(")
	assertContains(t, ir, "call void @llvm.memcpy")
}

func TestFixedArrayIndexDupsWeak(t *testing.T) {
	// Weak element: dupWeak emits the type-specific Weak clone helper.
	ir := generateIR(t, `
		main() {
			Ref[int] keep0 = Ref[int](10);
			Ref[int] keep1 = Ref[int](20);
			Weak[int] w0 = keep0.downgrade();
			Weak[int] w1 = keep1.downgrade();
			Weak[int][2] arr = [w0, w1];
			Weak[int] x = arr[0];
		}
	`)
	assertContains(t, ir, "arridx.ok")
	// dupWeak emits an atomic refcount on the weak count.
	if !strings.Contains(ir, "atomicrmw add") && !strings.Contains(ir, "promise_weak") {
		t.Errorf("expected Weak dup helper in IR; got:\n%s", ir)
	}
}

func TestStringBoxInStructFieldUsesStringBoxDrop(t *testing.T) {
	// T1280: a string boxed as a structural interface and stored into a struct field must
	// still carry the stringbox typeinfo, so when the struct drops, the enclosing type's
	// field-drop routes the boxed string through __promise_structural_drop → the box
	// drop_fn (frees the cloned string + box). Confirms the RTTI drop site is uniform for
	// the struct-field escape path (not just return/local), matching the runtime e2e test.
	ir := generateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		show_str(string s) Showable { return s; }
		type Holder { Showable item; }
		main() { Holder h = Holder(item: show_str("x")); }
	`)
	assertContains(t, ir, "@promise_typeinfo_stringbox")
	assertContains(t, ir, "@__promise_string_box_drop")
	assertContains(t, ir, "bitcast ({ i8*, i8*, i8*, i32, i32 }* @promise_typeinfo_stringbox to i8*)")
}

// T0893: a borrowing method whose body is bare `return this` must clone the
// receiver instance so the returned owned value does not alias the receiver's
// heap allocation (otherwise one binding's scope-drop frees memory the other
// still reads). The clone shows up as a heapdup block in the method body.
func TestReturnThisClonesBorrowedReceiver(t *testing.T) {
	ir := generateIR(t, `
		type BB { int v; dup() BB { return this; } }
		main() { d := BB(v: 11); m := d.dup(); }
	`)
	assertContains(t, extractDefine(ir, "BB.dup"), "heapdup")
}

// T0893: a `~this` (owned/moved-in) receiver returning `this` is a genuine
// ownership transfer — cloning would copy needlessly and leak the moved-in
// instance. The method body must NOT contain a heapdup clone.
func TestReturnThisOwnedReceiverDoesNotClone(t *testing.T) {
	ir := generateIR(t, `
		type BB { int v; consume(~this) BB { return this; } }
		main() { d := BB(v: 11); m := d.consume(); }
	`)
	assertNotContains(t, extractDefine(ir, "BB.consume"), "heapdup")
}

// T0893: a method returning a borrow (`T&`) of `this` hands back a reference into
// existing storage, not an owned copy — it must NOT clone.
func TestReturnThisBorrowReturnDoesNotClone(t *testing.T) {
	ir := generateIR(t, `
		type BB { int v; ref() BB& { return this; } }
		main() { d := BB(v: 11); r := d.ref(); }
	`)
	assertNotContains(t, extractDefine(ir, "BB.ref"), "heapdup")
}

// T0963: an operator returning a borrowed string operand as an owned value must
// clone it (cloneOwnedReturnAlias). Operator dispatch borrows operands, so the
// returned value would otherwise alias the caller's still-live operand — both
// bindings would free / mutate the same string instance. The operator body must
// dup via promise_string_new (dupString).
func TestOperatorReturnsBorrowedStringOperandClones(t *testing.T) {
	ir := generateIR(t, `
		type PickSecond { int z; %(string other) string { return other; } }
		main() { s := "x"; p := PickSecond(z: 1); r := p % s; }
	`)
	assertContains(t, extractDefine(ir, `"PickSecond.%"`), "call i8* @promise_string_new")
}

// T0963: an operator returning a borrow (`string&`) of its operand hands back a
// reference into existing storage, not an owned copy — it must NOT dup.
func TestOperatorReturnsBorrowedStringRefDoesNotClone(t *testing.T) {
	ir := generateIR(t, `
		type PickSecond { int z; %(string other) string& { return other; } }
		main() { s := "x"; p := PickSecond(z: 1); r := p % s; }
	`)
	assertNotContains(t, extractDefine(ir, `"PickSecond.%"`), "call i8* @promise_string_new")
}

// B0250: Assigning the result of a method that returns `this` must clear the
// receiver's drop flag to prevent double-free (both variables share the same instance).
func TestReturnThisClearsReceiverDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { int value; self() Wrapper { return this; } }
		main() { w := Wrapper(value: 42); w2 := w.self(); }
	`)
	// Should emit a runtime instance-pointer comparison and conditional drop flag clear
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
}

// T0347: Chained self-returning calls on a local must walk through the chain
// to find the IdentExpr root and clear its drop flag (not just direct calls).
func TestReturnThisChainedClearsRootDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { int value; self() Wrapper { return this; } }
		main() { w := Wrapper(value: 1); r := w.self().self(); }
	`)
	// chainOriginExpr should walk past the inner call and reach `w`,
	// emitting the receiver alias-clear blocks for the chained case.
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
}

// T0347: `r := this.method()` inside a method must emit a different alias-clear
// path that targets the new binding's drop flag (since `this` itself has no
// drop flag — it's borrowed). Distinct block labels: this.alias.{clear,skip}.
func TestReturnThisRootedClearsBindingDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int n;
			iter() Inner { return this; }
			use_self() int {
				r := this.iter();
				return r.n;
			}
		}
		main() { i := Inner(n: 11); v := i.use_self(); }
	`)
	// New helper emits this.alias.clear / this.alias.skip blocks.
	assertContains(t, ir, "this.alias.clear")
	assertContains(t, ir, "this.alias.skip")
}

// T0347: Chained `r := this.iter().iter()` inside a method also emits the
// this-alias clear path (chainOriginExpr walks the chain to ThisExpr root).
func TestReturnThisRootedChainedClearsBindingDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int n;
			iter() Inner { return this; }
			use_self() int {
				r := this.iter().iter();
				return r.n;
			}
		}
		main() { i := Inner(n: 13); v := i.use_self(); }
	`)
	assertContains(t, ir, "this.alias.clear")
	assertContains(t, ir, "this.alias.skip")
}

// T0582: paren-wrapped receiver `(w).self()` must walk through chainOriginExpr
// to the IdentExpr root `w` and emit the B0250 receiver alias-clear blocks.
// Without the paren-peel in chainOriginExpr, the chain origin would be a
// ParenExpr and the switch would miss → no alias-clear → runtime double-free.
func TestParenReceiverClearsReceiverDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { int v; self() Wrapper { return this; } }
		main() { w := Wrapper(v: 1); w2 := (w).self(); }
	`)
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
}

// T0582: paren-wrapped receiver under a chain `(w).self().self()` — chain origin
// must still resolve to `w` after the paren-peel.
func TestParenReceiverChainedClearsReceiverDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { int v; self() Wrapper { return this; } }
		main() { w := Wrapper(v: 1); r := (w).self().self(); }
	`)
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
}

// T0582: paren around an inner call `(w.self()).self()` — the outer chain step
// peels the ParenExpr to reach the inner call, then walks back to `w`.
func TestChainedParenInnerCallClearsReceiverDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { int v; self() Wrapper { return this; } }
		main() { w := Wrapper(v: 1); r := (w.self()).self(); }
	`)
	assertContains(t, ir, "return.this.clear")
	assertContains(t, ir, "return.this.skip")
}

// T1029: a discarded free-function call whose result aliases an owned-local arg
// must clear the RESULT TEMP's drop flag (discard.alias.clear), leaving the source
// local's drop flag armed so the aliased allocation is freed once at scope exit.
// Before the fix the source local's flag was cleared and the temp was freed at
// statement end → the still-live local dangled (use-after-free).
func TestDiscardedAliasArgClearsTempNotSource(t *testing.T) {
	ir := generateIR(t, `
		type Node { int v; }
		ident_node(Node n) Node { return n; }
		run() int { n := Node(v: 5); ident_node(n); return n.v; }
		main() { x := run(); }
	`)
	body := extractFunction(ir, "__user.run")
	// The result temp is cleared on the alias path.
	assertContains(t, body, "discard.alias.clear")
	assertContains(t, body, "discard.alias.skip")
	// The source local's drop flag must NOT be cleared inside the discarded call —
	// the local remains the single owner.
	assertNotContains(t, body, "store i1 false, i1* %n.dropflag")
}

// T1029: the i8* result path (vector/string) in genExpr — distinct from the
// heap-user-type path in trackHeapUserTypeResult — must also emit the result-temp
// clear (discard.alias.clear) for a discarded call whose i8* result aliases an
// owned-local arg, so the source local stays the single owner.
func TestDiscardedAliasArgClearsTempI8PtrPath(t *testing.T) {
	ir := generateIR(t, `
		ident_vec(int[] v) int[] { return v; }
		ident_str(string s) string { return s; }
		run_vec() int { xs := [1, 2]; xs.push(3); ident_vec(xs); return xs.len; }
		run_str() int { a := "hi"; s := a + "!"; ident_str(s); return s.len; }
		main() { x := run_vec(); y := run_str(); }
	`)
	vec := extractFunction(ir, "__user.run_vec")
	assertContains(t, vec, "discard.alias.clear")
	assertContains(t, vec, "discard.alias.skip")
	str := extractFunction(ir, "__user.run_str")
	assertContains(t, str, "discard.alias.clear")
	assertContains(t, str, "discard.alias.skip")
}

// T0891: a return-this aliased result bound to a local (`m := d.dup()`) must NOT
// be NLL-early-dropped. The result aliases the still-live source `d`; the
// receiver-alias-clear (B0250) leaves `m`'s drop flag set (clearing `d`'s), so an
// early free of `m` after its last use would free the shared instance and make
// `d`'s later read a use-after-free. The signature of the (now-suppressed) early
// drop is `emitEarlyDrops` clearing `m`'s flag in the normal body flow —
// `store i1 false, i1* %m.dropflag` — which must be absent. The alias-clear must
// still fire, and `m`'s flag-guarded scope-exit free must remain.
func TestReturnThisAliasNoEarlyDrop(t *testing.T) {
	ir := generateIR(t, `
		type BB { int v; dup() BB { return this; } }
		main() {
			d := BB(v: 11);
			m := d.dup();
			a := m.v;
			b := d.v;
		}
	`)
	// The B0250 receiver alias-clear must still fire — `m` becomes sole owner.
	assertContains(t, ir, "return.this.clear")
	// No NLL early drop of `m`: emitEarlyDrops would force-clear its flag in the
	// straight-line body after `a := m.v`. Absence proves the suppression (T0891).
	assertNotContains(t, ir, "store i1 false, i1* %m.dropflag")
	// `m` still has a flag-guarded free at scope exit (no leak, exactly one free).
	assertContains(t, ir, "%m.dropflag")
}

// T1207: a borrowed Mutex arg passed to a helper that locks it and escapes the
// guard into a `~`-container param must NOT be NLL-early-dropped at the call.
// The callee stores `m.lock()`'s guard into `~vec`, so the caller's `v` (dropped
// at scope exit, after `m`) holds a guard back-pointing into `m`'s Mutex. An
// early `Mutex[int].drop(m)` right after `push_g(v, m)` — signalled by a
// force-clear `store i1 false, i1* %m.dropflag` in the straight-line body —
// would free the Mutex handle before `v`'s element drop unlocks it (UAF). The
// hidden-lock suppression in lastuse.go must remove that early clear, deferring
// `m`'s single flag-guarded drop to scope exit (safe LIFO: guard → vec → m).
func TestT1207HiddenLockEscapeNoEarlyDrop(t *testing.T) {
	ir := generateIR(t, `
		push_g(Vector[MutexGuard[int]] ~vec, Mutex[int] m) { vec.push(m.lock()); }
		main() {
			m := Mutex[int](17);
			v := Vector[MutexGuard[int]]();
			push_g(v, m);
			int n = v.len;
		}
	`)
	// No NLL early drop of `m`: the force-clear that precedes an emitted early
	// drop must be absent (present without the fix — verified against T1207 repro).
	assertNotContains(t, ir, "store i1 false, i1* %m.dropflag")
	// `m` still has its flag-guarded scope-exit drop (exactly one free, no leak).
	assertContains(t, ir, "%m.dropflag")
	assertContains(t, ir, `call void @"Mutex[int].drop"`)
}

// B0165: Batch test main waits for worker threads to finish init, then resets
// alloc count to 0 so scheduler allocations don't leak into per-test counts.
func TestBatchTestResetsAllocCount(t *testing.T) {
	result := compileResult(t, `
		myTest() `+"`test"+` { }
	`)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(`myTest() ` + "`test" + ` { }`)
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		file, _ := ast.Build("test.pr", tree)
		return file
	}())
	result.GenerateTestMain(info.Tests, nil)
	ir := result.Module.String()
	// Spin-wait on sched.ready_count
	assertContains(t, ir, "sched_ready_spin")
	assertContains(t, ir, "sched_ready_done")
	// Reset alloc count to 0 via atomic exchange
	assertContains(t, ir, "atomicrmw xchg i64* @__promise_alloc_count, i64 0 monotonic")
}

// B0231/B0315: Batch test leak check uses spin-wait for goroutine drain
// (condvar approach had a lost-wakeup race on ARM64).
func TestBatchTestLeakCheckDrainSpinWait(t *testing.T) {
	result := compileResult(t, `
		myTest() `+"`test"+` { }
	`)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(`myTest() ` + "`test" + ` { }`)
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		file, _ := ast.Build("test.pr", tree)
		return file
	}())
	result.GenerateTestMain(info.Tests, nil)
	ir := result.Module.String()
	// B0315: Drain check uses spin-wait with periodic wake_m nudge
	assertContains(t, ir, "leak_check_myTest")
	assertContains(t, ir, "drain_done_myTest")
	assertContains(t, ir, "drain_slow_myTest")
	assertContains(t, ir, "drain_gs_myTest")
	assertContains(t, ir, "drain_wait_myTest")
	assertContains(t, ir, "drain_nudge_myTest")
	assertContains(t, ir, "drain_sleep_myTest")
	// B0315: wake_m nudge to prevent lost-wakeup race
	assertContains(t, ir, "call void @promise_sched_wake_m()")
	// B0320: both fast-path and slow-path reads should use Acquire ordering
	assertContains(t, ir, "i64 0 acquire")
}

// T0020: Leak detection emits alloc count tracking in pal_alloc/pal_free
// and per-test leak checks in the test main.
func TestLeakDetectionAllocTracking(t *testing.T) {
	result := compileResult(t, `
		myTest() `+"`test"+` { }
	`)
	ir := result.Module.String()

	// pal_alloc should track allocations via __promise_alloc_count
	assertContains(t, ir, "@__promise_alloc_count = global i64 0")
	// pal_alloc atomically increments on successful malloc
	assertContains(t, ir, "atomicrmw add i64* @__promise_alloc_count, i64 1 monotonic")
	// pal_free atomically decrements on non-null free
	assertContains(t, ir, "atomicrmw sub i64* @__promise_alloc_count, i64 1 monotonic")
}

// T0020: Leak detection in test main snapshots alloc count before/after each test.
func TestLeakDetectionInTestMain(t *testing.T) {
	result := compileResult(t, `
		myTest() `+"`test"+` { }
	`)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(`myTest() ` + "`test" + ` { }`)
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		file, _ := ast.Build("test.pr", tree)
		return file
	}())
	result.GenerateTestMain(info.Tests, nil)
	ir := result.Module.String()

	// Leak check blocks: snapshot before test, check delta after
	assertContains(t, ir, "leak_check_myTest")
	assertContains(t, ir, "print_leak_detail_myTest")
	// Leak message string constants
	assertContains(t, ir, `c"  leak: "`)
	assertContains(t, ir, `c" allocations not freed\0A"`)
	// Leaked counter in summary call
	assertContains(t, ir, "call void @promise_test_summary(i32")
}

// T0067: Tests with allow_leaks don't increment leaked counter.
func TestAllowLeaksDoesNotIncrementLeakedCounter(t *testing.T) {
	src := `myTest() ` + "`test(allow_leaks: true)" + ` { }`
	result := compileResult(t, src)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(src)
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		file, _ := ast.Build("test.pr", tree)
		return file
	}())
	result.GenerateTestMain(info.Tests, nil)
	ir := result.Module.String()

	// Should have leak check blocks
	assertContains(t, ir, "leak_check_myTest")
	assertContains(t, ir, "print_leak_detail_myTest")
	// Should have stale tag warning for allow_leaks
	assertContains(t, ir, "allow_leaks")
	assertContains(t, ir, "tag can be removed")
	// allow_leaks: no_leak_detail block and ignored counter
	assertContains(t, ir, "no_leak_detail_myTest")
	// Summary includes ignored parameter (T0067)
	assertContains(t, ir, "i32 %ignored")
}

// T0067: Tests without allow_leaks increment leaked counter and exit code includes leaks.
func TestNoAllowLeaksIncrementsLeakedCounter(t *testing.T) {
	src := `myTest() ` + "`test" + ` { }`
	result := compileResult(t, src)
	info, _ := sema.Check(func() *ast.File {
		input := antlr.NewInputStream(src)
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		file, _ := ast.Build("test.pr", tree)
		return file
	}())
	result.GenerateTestMain(info.Tests, nil)
	ir := result.Module.String()

	// Exit code should use OR of failed and leaked (T0067)
	assertContains(t, ir, "or i1")
	// Should NOT have stale tag warning (no allow_leaks)
	assertNotContains(t, ir, "tag can be removed")
}

// T0106: emitFieldDrops frees field instances with explicit drop
func TestFieldDropFreesInstance(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner field;
			drop(~this) { }
		}
		main() {
			o := Outer(field: Inner(id: 1));
		}
	`)
	// Outer.drop should call Inner.drop on the field AND pal_free the field instance
	assertContains(t, ir, "call void @Inner.drop")
	// pal_free should appear in Outer.drop for the Inner field instance
	outerDrop := extractFunction(ir, "Outer.drop")
	if !strings.Contains(outerDrop, "call void @pal_free(") {
		t.Errorf("Outer.drop should pal_free the Inner field instance\nOuter.drop IR:\n%s", outerDrop)
	}
}

// T0106: String move via IdentExpr propagates ownership at runtime
func TestStringMoveDropFlagPropagation(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := "hello" + " world";
			b := a;
		}
	`)
	// a.dropflag should be loaded (saved) before clearing
	// b.dropflag should be set from the saved value (not unconditionally cleared)
	assertContains(t, ir, "a.dropflag")
	assertContains(t, ir, "b.dropflag")
	// Both should have string drop calls (conditional on flags)
	assertContains(t, ir, "promise_string_drop")
}

// T0135: Constructor allocation tracked as heap temp for auto-propagation cleanup
func TestConstructorAllocHeapTemp(t *testing.T) {
	ir := generateIR(t, `
		fail!(string s) int {
			raise error(message: s);
		}
		type H { int v; }
		process!() {
			h := H(v: fail("x"));
		}
	`)
	// Constructor allocation should be tracked and freed on error path
	assertContains(t, ir, "auto.propagate")
	assertContains(t, ir, "err.heap.drop")
}

// --- Drop method tests ---

// Basic: drop() called at scope exit
func TestDropBasicScopeExit(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			r := Resource(id: 1);
			int x = r.id;
		}
	`)
	assertContains(t, ir, "call void @Resource.drop")
	assertContains(t, ir, "r.dropflag")
}

// B0159: Explicit drop methods auto-free instance memory via pal_free
func TestDropExplicitFreesInstance(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			r := Resource(id: 1);
		}
	`)
	// The drop function body should end with pal_free to free the instance struct
	assertContains(t, ir, "define void @Resource.drop")
	assertContains(t, ir, "call void @pal_free(i8* %this)")
}

// Move to function arg clears drop flag, adds condBr
func TestDropNotCalledWhenMoved(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		main() {
			r := Resource(id: 1);
			consume(r);
		}
	`)
	assertContains(t, ir, "store i1 false, i1*")
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
}

// Return triggers drop before ret
func TestDropWithReturn(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		make() int {
			r := Resource(id: 42);
			return r.id;
		}
		main() {
			int v = make();
		}
	`)
	assertContains(t, ir, "call void @Resource.drop")
	assertContains(t, ir, "define i64 @__user.make")
}

// Mixed use + drop bindings both fire
func TestDropAndUseOrdering(t *testing.T) {
	ir := generateIR(t, `
		type Closeable {
			int id;
			close() { }
		}
		type Droppable {
			int id;
			drop(~this) { }
		}
		main() {
			use c := Closeable(id: 1);
			d := Droppable(id: 2);
			int x = c.id + d.id;
		}
	`)
	assertContains(t, ir, "call void @Closeable.close")
	assertContains(t, ir, "call void @Droppable.drop")
}

// Nested type: outer drop() triggers field drops
func TestDropFieldAutoCleanup(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
			drop(~this) { }
		}
		main() {
			o := Outer(inner: Inner(id: 1));
			int x = o.inner.id;
		}
	`)
	assertContains(t, ir, "call void @Outer.drop")
	assertContains(t, ir, "call void @Inner.drop")
}

// Returning a droppable variable clears its flag
func TestDropReturnMoveClearsFlag(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		make() Resource {
			r := Resource(id: 42);
			return r;
		}
		main() {
			Resource v = make();
		}
	`)
	assertContains(t, ir, "store i1 false, i1*")
}

// Conditional move: moved in if-then only → drop flag condBr after merge
func TestDropConditionalMove(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		main() {
			r := Resource(id: 1);
			if true {
				consume(r);
			}
		}
	`)
	// Drop flag cleared in then-branch
	assertContains(t, ir, "store i1 false, i1*")
	// Conditional drop at scope exit (flag may be true or false)
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
}

// Conditional move with else: moved in both branches → flag cleared in both
func TestDropConditionalMoveBothBranches(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		consume(Resource r) { }
		other(Resource r) { }
		main() {
			r := Resource(id: 1);
			if true {
				consume(r);
			} else {
				other(r);
			}
		}
	`)
	// Flag should be cleared in both branches
	count := strings.Count(ir, "store i1 false, i1*")
	if count < 2 {
		t.Errorf("expected at least 2 'store i1 false' (both branches), got %d", count)
	}
}

// Nested scopes: inner scope drop happens before outer
func TestDropNestedScopes(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			r1 := Resource(id: 1);
			if true {
				r2 := Resource(id: 2);
				int x = r2.id;
			}
			int y = r1.id;
		}
	`)
	// Both should have drop flags and calls
	assertContains(t, ir, "r1.dropflag")
	assertContains(t, ir, "r2.dropflag")
	// Two drop calls (one for inner, one for outer)
	count := strings.Count(ir, "call void @Resource.drop")
	if count < 2 {
		t.Errorf("expected at least 2 drop calls (inner + outer scope), got %d\nIR:\n%s", count, ir)
	}
}

// While loop: droppable var inside loop body should be dropped per iteration
func TestDropInWhileLoop(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			int i = 0;
			while i < 3 {
				r := Resource(id: i);
				int x = r.id;
				i += 1;
			}
		}
	`)
	// Drop should be emitted inside the loop body
	assertContains(t, ir, "call void @Resource.drop")
	assertContains(t, ir, "r.dropflag")
}

// Infinite loop with break: drop cleanup happens at break
func TestDropInLoopWithBreak(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			for {
				r := Resource(id: 1);
				int x = r.id;
				break;
			}
		}
	`)
	// Drop call should be present (at break cleanup)
	assertContains(t, ir, "call void @Resource.drop")
}

// Loop with continue: drop fires at end of iteration and at continue
func TestDropInLoopWithContinue(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			int i = 0;
			while i < 5 {
				r := Resource(id: i);
				i += 1;
				if i == 3 {
					continue;
				}
				int x = r.id;
			}
		}
	`)
	// Drop calls should exist (at continue and normal scope exit)
	assertContains(t, ir, "call void @Resource.drop")
}

// Move into method call clears drop flag
func TestDropMoveToMethodCall(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		type Container {
			int id;
			take(Resource r) { }
		}
		main() {
			c := Container(id: 0);
			r := Resource(id: 1);
			c.take(r);
		}
	`)
	// r's drop flag should be cleared after method call
	assertContains(t, ir, "store i1 false, i1*")
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
}

// Move into constructor field clears drop flag
func TestDropMoveToConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
		}
		main() {
			r := Inner(id: 1);
			o := Outer(inner: r);
			int x = o.inner.id;
		}
	`)
	// r's drop flag should be cleared when moved into constructor
	assertContains(t, ir, "store i1 false, i1*")
}

// Move into ident assignment clears drop flag
func TestDropMoveToIdentAssign(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			Resource a = Resource(id: 1);
			Resource b = Resource(id: 2);
			b = a;
			int x = b.id;
		}
	`)
	// a's drop flag should be cleared after the assignment to b
	assertContains(t, ir, "store i1 false, i1*")
}

// Move into member assignment clears drop flag (bug #2 fix)
func TestDropMoveToMemberAssign(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
		}
		main() {
			o := Outer(inner: Inner(id: 0));
			r := Inner(id: 1);
			o.inner = r;
		}
	`)
	// r's drop flag should be cleared after the member assignment
	assertContains(t, ir, "store i1 false, i1*")
}

// Multiple droppable vars: each gets its own flag and cleanup
func TestDropMultipleVariables(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			a := Resource(id: 1);
			b := Resource(id: 2);
			c := Resource(id: 3);
			int x = a.id + b.id + c.id;
		}
	`)
	assertContains(t, ir, "a.dropflag")
	assertContains(t, ir, "b.dropflag")
	assertContains(t, ir, "c.dropflag")
	count := strings.Count(ir, "call void @Resource.drop")
	if count < 3 {
		t.Errorf("expected at least 3 drop calls, got %d\nIR:\n%s", count, ir)
	}
}

// Multiple droppable fields: all cleaned up after user drop() body
func TestDropMultipleFieldsAutoCleanup(t *testing.T) {
	ir := generateIR(t, `
		type FileHandle {
			int fd;
			drop(~this) { }
		}
		type Connection {
			FileHandle read_handle;
			FileHandle write_handle;
			drop(~this) { }
		}
		main() {
			c := Connection(read_handle: FileHandle(fd: 3), write_handle: FileHandle(fd: 4));
		}
	`)
	assertContains(t, ir, "call void @Connection.drop")
	// FileHandle.drop should be called for both fields inside Connection.drop
	count := strings.Count(ir, "call void @FileHandle.drop")
	if count < 2 {
		t.Errorf("expected at least 2 FileHandle.drop calls (one per field), got %d\nIR:\n%s", count, ir)
	}
}

// Vector field drop: types with Vector fields should emit Vector.drop (B0157)
func TestDropVectorField(t *testing.T) {
	ir := generateIR(t, `
		type Holder {
			int[] items;
			drop(~this) {}
		}
		main() {
			h := Holder(items: [1, 2, 3]);
		}
	`)
	// Container fields get dropped via emitFieldDrops in drop() body
	assertContains(t, ir, "call void @Vector.drop")
	assertContains(t, ir, "%h.dropflag")
	assertContains(t, ir, "call void @Holder.drop")
}

// Standalone vector variables do NOT get scope-exit drop yet (needs ownership tracking)
// T0064: Standalone vector variables now get drop flags
func TestDropVectorStandaloneHasDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] my_vec = [1, 2, 3];
			int x = my_vec.len;
		}
	`)
	assertContains(t, ir, "%my_vec.dropflag")
	assertContains(t, ir, "call void @Vector.drop(")
}

// Non-droppable type: no drop flag or call generated for that variable
func TestDropNotGeneratedForNonDroppable(t *testing.T) {
	ir := generateIR(t, `
		type Simple {
			int id;
		}
		main() {
			my_simple := Simple(id: 1);
			int x = my_simple.id;
		}
	`)
	// B0164: Non-droppable heap types now get bindingFree with a drop flag for pal_free
	assertContains(t, ir, "%my_simple.dropflag")
	assertNotContains(t, ir, "Simple.drop")
	assertContains(t, ir, "call void @pal_free")
}

// Copy type: no drop flag even if fields exist
func TestDropNotGeneratedForCopyType(t *testing.T) {
	ir := generateIR(t, `
		type Point `+"`"+`copy {
			int x;
			int y;
		}
		main() {
			p := Point(x: 1, y: 2);
			int v = p.x;
		}
	`)
	assertNotContains(t, ir, "%p.dropflag")
	assertNotContains(t, ir, "Point.drop")
}

// Droppable var in typed var decl
func TestDropTypedVarDecl(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			Resource r = Resource(id: 1);
			int x = r.id;
		}
	`)
	assertContains(t, ir, "call void @Resource.drop")
	assertContains(t, ir, "r.dropflag")
}

// Drop in a function that takes and returns a droppable:
// the parameter itself doesn't get a drop flag (it's the caller's responsibility)
func TestDropParameterNotFlagged(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		passthrough(Resource zres) int {
			return zres.id;
		}
		main() {
			int x = passthrough(Resource(id: 1));
		}
	`)
	assertContains(t, ir, "define i64 @__user.passthrough")
	// The callee does NOT create a drop flag for non-~ params.
	// The caller retains ownership and drops at scope exit.
	assertNotContains(t, ir, "zres.dropflag")
}

// Drop with use in loop triggers both close and drop at scope boundaries
func TestDropAndUseInLoop(t *testing.T) {
	ir := generateIR(t, `
		type Closeable {
			int id;
			close() { }
		}
		type Droppable {
			int id;
			drop(~this) { }
		}
		main() {
			d := Droppable(id: 1);
			int i = 0;
			while i < 3 {
				use c := Closeable(id: i);
				int x = c.id + d.id;
				i++;
			}
		}
	`)
	assertContains(t, ir, "call void @Closeable.close")
	assertContains(t, ir, "call void @Droppable.drop")
}

// Move in function call clears flag — std call variant
func TestDropMoveToStdCall(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		take(Resource r) { }
		main() {
			r := Resource(id: 1);
			take(r);
		}
	`)
	assertContains(t, ir, "store i1 false, i1*")
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
}

// Move to index assignment clears flag
func TestDropMoveToIndexAssign(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			arr := [Resource(id: 0)];
			r := Resource(id: 1);
			arr[0] = r;
		}
	`)
	assertContains(t, ir, "store i1 false, i1*")
}

// B0195: Vector[string] index assign dups new value for independent ownership
func TestVectorStringIndexAssignDup(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[] v = ["hello", "world"];
			v[0] = "replaced";
		}
	`)
	// Should dup the new value so vector owns an independent copy (like push)
	assertContains(t, ir, "call i8* @promise_string_new")
}

// B0204: Vector[string] index assign drops old element before storing new value
func TestVectorStringIndexAssignDropsOld(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[] v = ["hello", "world"];
			v[0] = "replaced";
		}
	`)
	// Should drop old string element before storing the new one
	assertContains(t, ir, "call void @promise_string_drop")
}

// B0350: Map[K,string] index assign from borrow param dups value
func TestMapStringIndexAssignBorrowParamDup(t *testing.T) {
	ir := generateIR(t, `
		store(map[string, string]~ m, string key, string value) {
			m[key] = value;
		}
		main() {
			map[string, string] m = {:};
			store(m, "k", "v");
		}
	`)
	// The borrow param 'value' should be duped before storing in map
	assertContains(t, ir, "call i8* @promise_string_new")
}

// B0350: Owned local string assigned to map should NOT produce extra dup —
// clearDropFlag transfers ownership, so no dup is needed.
func TestMapStringIndexAssignOwnedNoDup(t *testing.T) {
	ir := generateIR(t, `
		make_val() string { return "hello"; }
		store_owned(map[string, string]~ m) {
			string v = make_val();
			m["k"] = v;
		}
		main() {
			map[string, string] m = {:};
			store_owned(m);
		}
	`)
	// v is an owned local (has drop flag) — clearDropFlag transfers ownership.
	// B0350 dup must NOT fire for owned locals.
	fnIR := extractFunction(ir, "store_owned")
	if strings.Contains(fnIR, "call i8* @promise_string_new") {
		t.Error("owned local string should not be duped in map index assign")
	}
}

// B0235: Map overwrite should drop old Slot enum element.
func TestMapOverwriteDropsOldSlot(t *testing.T) {
	ir := generateIR(t, `
		main() {
			map[string, string] m = {:};
			m["a"] = "one";
			m["a"] = "two";
		}
	`)
	// Map.[]= should call Slot.drop on old element before storing new.
	// The Slot enum has synthesized drop at mono time (Slot[string, string]).
	assertContains(t, ir, `call void @"Slot[string, string].drop"(`)
}

// B0204: Vector[string] index read dups when stored in variable
func TestVectorStringIndexReadDup(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[] v = ["hello", "world"];
			string s = v[0];
		}
	`)
	// Should dup the string read from vector index (dup-on-read)
	assertContains(t, ir, "call i8* @promise_string_new")
}

// T0383 / T0438: assigning `outer[0] = a.borrow` for non-Copy element T is
// rejected at sema (no implicit `T& → T` decay). The codegen dup-on-borrow
// path being tested here is unreachable under Option A; users must
// `.clone()` to obtain an owned independent copy.

// T0383: Vector[Vector[T]] index assign drops the old element before the
// store, preventing leak of the previously-pushed buffer.
func TestT0383VectorIndexAssignDropsOldHeapElement(t *testing.T) {
	ir := generateIR(t, `
		main() {
			outer := string[][]();
			inner := string[]();
			inner.push("init");
			outer.push(inner);

			v2 := string[]();
			v2.push("hello");
			outer[0] = v2;
		}
	`)
	// The drop-old path loads the old slot value and calls Vector.drop on it
	// inside the indexassign.ok block (before the new-value store).
	assertContains(t, ir, "call void @Vector.drop(")
}

// T0383: Vector[Vector[T]] index read dups when stored in a variable
// (mirror of B0204 for nested heap vectors). Without this, drop-on-write
// at vec[i] would create a use-after-free through the aliased local.
func TestT0383VectorIndexReadDupsHeapElement(t *testing.T) {
	ir := generateIR(t, `
		main() {
			outer := string[][]();
			inner := string[]();
			inner.push("init");
			outer.push(inner);
			t := outer[0];
		}
	`)
	// Dup-on-read emits a vecdup.copy block via dupVector for the read path.
	assertContains(t, ir, "vecdup.copy")
}

// T0388: push(h.containerField) where h is a droppable owned type must dup the
// field so that both h.drop and v's element walk own independent copies.
// genVectorMethodCall detects the MemberExpr arg and sets dupContainerFieldAccess;
// genFieldAccess then dups the vector when the owner has HasDrop() true.
func TestT0388PushVectorFieldFromDroppableOwnerDups(t *testing.T) {
	ir := generateIR(t, `
		type Container {
			int[] data;
			drop(~this) {}
		}
		test() {
			c := Container(data: [1, 2, 3]);
			v := int[][]();
			v.push(c.data);
		}
	`)
	// genFieldAccess must emit vecdup.copy so v's element and Container.data are
	// independent — otherwise Container.drop and v's Vector.drop double-free the buffer.
	assertContains(t, ir, "vecdup.copy")
}

// T0398: `b := v[0]` where v is Vector[heap-user-type-with-drop] must deep-clone
// the element via cloneHeapElement so b holds an independent instance.
// Without the dup, b's drop binding and v's element walk double-free the same pointer.
// genInferredVarDecl sets dupHeapUserFieldAccess; genVectorIndex calls cloneHeapElement
// which falls back to dupHeapValue (pal_alloc + memcpy) when there is no clone method.
func TestT0398VectorHeapElementReadDupsOnVarDecl(t *testing.T) {
	ir := generateIR(t, `
		type Item { int n; drop(~this) {} }
		test() {
			v := Item[]();
			v.push(Item(n: 1));
			b := v[0];
		}
	`)
	// cloneHeapElement → dupHeapValue: allocate a new instance and memcpy the data.
	assertContains(t, ir, "call i8* @pal_alloc")
	assertContains(t, ir, "call void @llvm.memcpy")
}

// T0898: `b := v[0]` where v is Vector[no-drop heap user type] must dup-on-read.
// These types lack drop()/synth-drop (so isDroppableHeapUserType excludes them)
// and lack clone(), so genVectorIndex dups via dupHeapValue (alloc+memcpy →
// heapdup.copy block), not cloneHeapElement. Without this the new drop-on-
// overwrite in genVectorIndexAssign would free a slot still aliased by b.
func TestT0898VectorNoDropHeapElementReadDupsOnVarDecl(t *testing.T) {
	ir := generateIR(t, `
		type Bare { int n; dup() Bare { return this; } }
		probe() {
			v := Bare[]();
			v.push(Bare(n: 1));
			b := v[0];
		}
		main() { probe(); }
	`)
	// dupHeapValue emits a heapdup.copy block (alloc + memcpy). Scope to @probe:
	// the stdAll clone funcs also emit heapdup.copy.
	assertContains(t, extractFunction(ir, "__user.probe"), "heapdup.copy")
}

// T0898: `v[i] = X` where v is Vector[no-drop heap user type] must drop the old
// element before storing. emitVariantFieldDrop's B0218 branch null-checks +
// pal_frees the old instance (varfield.free block). Without this the overwrite
// leaks the previous element.
func TestT0898VectorNoDropHeapElementOverwriteDrops(t *testing.T) {
	ir := generateIR(t, `
		type Bare { int n; dup() Bare { return this; } }
		probe() {
			v := Bare[]();
			v.push(Bare(n: 1));
			v[0] = Bare(n: 2);
		}
		main() { probe(); }
	`)
	fn := extractFunction(ir, "__user.probe")
	assertContains(t, fn, "varfield.free")
	assertContains(t, fn, "call void @pal_free")
}

// T0898: `h.f = X` where f is a no-drop heap user-type field must drop the old
// field value before storing. The T0410/T0908 droppable-field branch (broadened
// to admit isHeapUserNoDropPalFree) emits the null + same-pointer guard
// (field.userdrop block) followed by emitVariantFieldDrop's B0218 pal_free.
// Without this the overwrite leaks the field's previous instance.
func TestT0898MemberNoDropHeapFieldOverwriteDrops(t *testing.T) {
	ir := generateIR(t, `
		type Bare { int n; dup() Bare { return this; } }
		type Holder { Bare f; }
		probe() {
			h := Holder(f: Bare(n: 1));
			h.f = Bare(n: 2);
		}
		main() { probe(); }
	`)
	fn := extractFunction(ir, "__user.probe")
	assertContains(t, fn, "field.userdrop")
	assertContains(t, fn, "call void @pal_free")
}

// T0403: `f(v[0])` where v is Vector[heap-user-type-with-drop] and f takes a `~T`
// param must deep-clone the element via cloneHeapElement so the callee receives an
// independent instance. Without the dup, the callee's `~T` drop and v's element
// walk double-free the same pointer. maybeEnableDupForMutRefArg sets
// dupHeapUserFieldAccess for IndexExpr against Vector[heap-user-type] passed to ~T;
// genVectorIndex's existing consume-branch then clones via cloneHeapElement.
// Sibling of T0398 (var-decl-site).
func TestT0403VectorHeapElementCallArgDups(t *testing.T) {
	ir := generateIR(t, `
		type Item { int n; drop(~this) {} }
		take(Item move b) {}
		test() {
			v := Item[]();
			v.push(Item(n: 1));
			take(v[0]);
		}
	`)
	// cloneHeapElement → dupHeapValue: allocate a new instance and memcpy the data.
	assertContains(t, ir, "call i8* @pal_alloc")
	assertContains(t, ir, "call void @llvm.memcpy")
}

// T0412: vec[i] = (...) for Vector[(droppable, ...)] must drop the old tuple
// element via emitVariantFieldDrop before storing the new value. Without this,
// the previous tuple's heap fields (string instance) leak.
func TestT0412VectorIndexAssignDropsOldTuple(t *testing.T) {
	ir := generateIR(t, `
		test() {
			outer := (string, int)[]();
			outer.push(("a" + "", 1));
			outer[0] = ("b" + "", 2);
		}
	`)
	// emitVariantFieldDrop's tuple branch walks fields via ExtractValue and
	// calls promise_string_drop on the string element. The drop must appear
	// inside the indexassign.ok block (not just at scope exit).
	assertContains(t, ir, "indexassign.ok")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0412: vec[i] = vec[j] for Vector[(droppable, ...)] must dup the RHS read
// via dupTupleValue so the new slot holds an independent clone. Without this,
// Part 1's drop-old would free heap fields still aliased by another slot.
func TestT0412VectorIndexAssignDupsTupleOnVecToVec(t *testing.T) {
	ir := generateIR(t, `
		test() {
			outer := (string, int)[]();
			outer.push(("a" + "", 1));
			outer.push(("b" + "", 2));
			outer[0] = outer[1];
		}
	`)
	// dupTupleValue emits promise_string_new to clone the string element on read.
	assertContains(t, ir, "call i8* @promise_string_new")
}

// T0489: c.tup_field = (...) for a droppable tuple field must drop the old
// field's heap contents via emitVariantFieldDrop before storing the new value.
// Without this, the previous tuple's string instance leaks.
func TestT0489MemberAssignDropsOldTuple(t *testing.T) {
	ir := generateIR(t, `
		type T0489C { (string, int) f; drop(~this) {} }
		test() {
			c := T0489C(f: ("a" + "", 1));
			c.f = ("b" + "", 2);
		}
	`)
	// emitVariantFieldDrop's tuple branch walks fields via ExtractValue and
	// calls promise_string_drop on the string element. Drop must appear in the
	// function body (not just at scope exit T0489C.drop).
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0489: c.tup_field = vec[i] for a droppable tuple field must dup the RHS
// read via dupTupleValue before storing. Without this, the field and vec[i]
// alias the same heap contents, causing a silent double-free at scope exit.
func TestT0489MemberAssignDupsTupleOnVecToField(t *testing.T) {
	ir := generateIR(t, `
		type T0489D { (string, int) f; drop(~this) {} }
		test() {
			v := (string, int)[]();
			v.push(("a" + "", 1));
			c := T0489D(f: ("first" + "", 1));
			c.f = v[0];
		}
	`)
	// dupTupleValue emits promise_string_new to clone the string element on read.
	assertContains(t, ir, "call i8* @promise_string_new")
}

// Reassignment of droppable variable emits drop on old value
func TestDropOnReassignment(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		test() {
			r := Resource(id: 1);
			r = Resource(id: 2);
		}
		main() {}
	`)
	// Should have drop.call and drop.skip blocks for the reassignment drop
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
	// Should reset drop flag after reassignment
	assertContains(t, ir, "store i1 true")
}

// Move-then-reassign: drop flag was cleared by move, so reassignment skips drop
func TestDropOnReassignmentAfterMove(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		consume(Resource r) {}
		test() {
			r := Resource(id: 1);
			consume(r);
			r = Resource(id: 2);
		}
		main() {}
	`)
	// The drop-before-reassign still emits condBr (checks flag), but flag is cleared
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
}

// Multiple reassignments: each reassignment should drop the old value
func TestDropOnMultipleReassignments(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		test() {
			r := Resource(id: 1);
			r = Resource(id: 2);
			r = Resource(id: 3);
		}
		main() {}
	`)
	// At least two drop.call blocks (one per reassignment)
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
	assertContains(t, ir, "store i1 true")
}

// Self-assignment should be a no-op (no drop emitted, no store)
func TestDropOnSelfAssignmentSkipped(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		test() {
			r := Resource(id: 1);
			r = r;
		}
		main() {}
	`)
	// The self-assignment is skipped entirely via return.
	// Scope exit should still emit ONE drop for r, so drop.call should exist.
	assertContains(t, ir, "drop.call")
}

// Compound assignment should NOT trigger drop-before-store
func TestDropCompoundAssignNoExtraDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int x = 10;
			x += 5;
		}
	`)
	// No drop blocks for primitive int variable x
	assertNotContains(t, ir, "%x.dropflag")
}

// Non-droppable type reassignment should not emit drop
func TestDropOnReassignmentNonDroppable(t *testing.T) {
	ir := generateIR(t, `
		type Simple { int x; }
		test() {
			my_simple := Simple(x: 1);
			my_simple = Simple(x: 2);
		}
		main() {}
	`)
	// B0164: Non-droppable heap types now get bindingFree with a drop flag for pal_free.
	// On reassignment, the old value is freed before storing the new one.
	assertContains(t, ir, "%my_simple.dropflag")
	assertNotContains(t, ir, "Simple.drop")
	assertContains(t, ir, "call void @pal_free")
}

// Reassignment inside if block: drop still emitted
func TestDropOnReassignmentInIfBlock(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		test() {
			r := Resource(id: 1);
			if true {
				r = Resource(id: 2);
			}
		}
		main() {}
	`)
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
}

// Reassignment inside loop: drop per iteration
func TestDropOnReassignmentInLoop(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int id;
			drop(~this) {}
		}
		test() {
			r := Resource(id: 0);
			for int i = 0; i < 3; i++ {
				r = Resource(id: i);
			}
		}
		main() {}
	`)
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "drop.skip")
	assertContains(t, ir, "store i1 true")
}

// Drop flag reset is i1 true (not i64 or other)
func TestDropOnReassignmentFlagResetIsI1True(t *testing.T) {
	ir := generateIR(t, `
		type R {
			int v;
			drop(~this) {}
		}
		test() {
			r := R(v: 1);
			r = R(v: 2);
		}
		main() {}
	`)
	// After emitDropCall, the flag is reset to i1 true
	assertContains(t, ir, "store i1 true")
}

// Reassignment when RHS is a moved variable clears RHS drop flag
func TestDropOnReassignmentRHSMoveClears(t *testing.T) {
	ir := generateIR(t, `
		type R {
			int v;
			drop(~this) {}
		}
		test() {
			a := R(v: 1);
			b := R(v: 2);
			a = b;
		}
		main() {}
	`)
	// Drop old a, store b into a, clear b's drop flag
	assertContains(t, ir, "drop.call")
	assertContains(t, ir, "store i1 true")
	assertContains(t, ir, "store i1 false")
}

// B0158: Type with droppable field auto-gets a synthesized drop
func TestDropSynthesizedBasic(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
		}
		main() {
			o := Outer(inner: Inner(id: 1));
		}
	`)
	// Outer gets a synthesized drop that calls Inner.drop on its field + pal_free
	assertContains(t, ir, "call void @Outer.drop")
	assertContains(t, ir, "o.dropflag")
	assertContains(t, ir, "call void @Inner.drop") // emitFieldDrops cascades
	assertContains(t, ir, "call void @pal_free(")  // frees Outer instance
}

// B0158: Cascading synthesized drop — Outer contains Middle contains Inner
func TestDropSynthesizedCascading(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Middle {
			Inner inner;
		}
		type Outer {
			Middle mid;
		}
		main() {
			o := Outer(mid: Middle(inner: Inner(id: 1)));
		}
	`)
	// All types in the chain get synthesized drops
	assertContains(t, ir, "call void @Outer.drop")
	assertContains(t, ir, "define void @Middle.drop")
}

// B0158: Synthesized drop with multiple droppable fields
func TestDropSynthesizedMultipleFields(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			int fd;
			drop(~this) { }
		}
		type Pair {
			Resource a;
			Resource b;
		}
		main() {
			p := Pair(a: Resource(fd: 1), b: Resource(fd: 2));
		}
	`)
	// Pair gets a synthesized drop function
	assertContains(t, ir, "call void @Pair.drop")
	assertContains(t, ir, "define void @Pair.drop")
}

// B0158: Type with mix of droppable and non-droppable fields
func TestDropSynthesizedMixedFields(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Mixed {
			int x;
			Inner inner;
			bool flag;
		}
		main() {
			m := Mixed(x: 1, inner: Inner(id: 2), flag: true);
		}
	`)
	assertContains(t, ir, "call void @Mixed.drop")
	assertContains(t, ir, "define void @Mixed.drop")
}

// B0158: Copy type is not auto-synthesized even with droppable-looking fields
func TestDropSynthesizedNotForCopy(t *testing.T) {
	ir := generateIR(t, `
		type Simple `+"`copy"+` {
			int x;
		}
		main() {
			my_copy := Simple(x: 1);
		}
	`)
	assertNotContains(t, ir, "Simple.drop")
	assertNotContains(t, ir, "my_copy.dropflag")
}

// B0158: No synthesized drop when no fields have drop
func TestDropSynthesizedNotNeeded(t *testing.T) {
	ir := generateIR(t, `
		type Plain {
			int x;
			bool y;
		}
		main() {
			my_plain := Plain(x: 1, y: true);
		}
	`)
	assertNotContains(t, ir, "Plain.drop")
	// B0164: Plain types now get a bindingFree with a drop flag for pal_free at scope exit.
	// No synthesized drop method, just pal_free for the heap instance.
	assertContains(t, ir, "my_plain.dropflag")
	assertContains(t, ir, "call void @pal_free")
}

// T0095: Synthesized drop drops string fields via promise_string_drop
func TestDropSynthesizedStringField(t *testing.T) {
	ir := generateIR(t, `
		type Holder {
			string name;
		}
		main() {
			h := Holder(name: "hello");
		}
	`)
	// Synthesized drop should call promise_string_drop on the string field
	assertContains(t, ir, "define void @Holder.drop")
	assertContains(t, ir, "call void @promise_string_drop(")
	assertContains(t, ir, "call void @pal_free(")
}

// B0217: Type with multiple function fields — both env pointers freed
func TestDropSynthesizedMultipleFuncFields(t *testing.T) {
	ir := generateIR(t, `
		type Transform {
			(int) -> int forward;
			(int) -> int backward;
		}
		main() {
			t := Transform(forward: |int x| -> x * 2, backward: |int x| -> x / 2);
		}
	`)
	assertContains(t, ir, "define void @Transform.drop")
	assertContains(t, ir, "funcfield.env.free")
}

// B0216: String field reassignment drops old value before storing new.
func TestStringFieldReassignDrop(t *testing.T) {
	ir := generateIR(t, `
		type Box {
			string val;
		}
		main() {
			b := Box(val: "hello");
			b.val = "world";
		}
	`)
	// Field reassignment should emit old-value drop before store
	assertContains(t, ir, "field.strdrop")
	assertContains(t, ir, "call void @promise_string_drop(")
}

// T0095: String field access on droppable type creates a dup (via promise_string_new)
func TestStringFieldAccessDup(t *testing.T) {
	ir := generateIR(t, `
		type Named {
			string name;
		}
		test() {
			n := Named(name: "world");
			string x = n.name;
		}
	`)
	// Reading n.name should dup the string to prevent double-free
	assertContains(t, ir, "call i8* @promise_string_new(")
}

// B0220: NeedsSynthDrop types (no explicit drop) also get string field dups.
// HasDrop() is true for NeedsSynthDrop types (set together in sema), so the
// T0095 dup logic covers both explicit-drop and synthesized-drop types.
func TestStringFieldAccessDupNeedsSynthDrop(t *testing.T) {
	ir := generateIR(t, `
		type Holder {
			string value;
		}
		test() {
			h := Holder(value: "original");
			string saved = h.value;
		}
	`)
	// Holder has NeedsSynthDrop (string field, no explicit drop).
	// Reading h.value should still dup to prevent use-after-free on reassign.
	assertContains(t, ir, "call i8* @promise_string_new(")
}

// B0219: Vector field reassignment drops old value before storing new.
func TestVectorFieldReassignDrop(t *testing.T) {
	ir := generateIR(t, `
		type Container {
			int[] items;
		}
		main() {
			c := Container(items: []);
			c.items = [];
		}
	`)
	// Field reassignment should emit old-value drop before store
	assertContains(t, ir, "field.vecdrop")
	assertContains(t, ir, "call void @Vector.drop(")
}

// B0219: Vector field read from droppable type creates a dup (via vecdup).
func TestVectorFieldAccessDup(t *testing.T) {
	ir := generateIR(t, `
		type Holder {
			int[] data;
		}
		test() {
			h := Holder(data: []);
			int[] x = h.data;
		}
	`)
	// Reading h.data should dup the vector to prevent double-free
	assertContains(t, ir, "vecdup.copy")
	assertContains(t, ir, "call i8* @pal_alloc(")
}

// T0095: Constructor with borrowed string param (no drop flag) dups the string
func TestConstructorDupBorrowedString(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper {
			string data;
		}
		wrap(string s) Wrapper {
			return Wrapper(data: s);
		}
		main() { }
	`)
	// Inside wrap(), s has no drop flag (non-~ param), so constructor should dup
	assertContains(t, ir, "define { i8*, i8* } @__user.wrap(i8* %s)")
	// The wrap function body should contain a dup call (promise_string_new)
	// because s is a borrowed param without a drop flag
	assertContains(t, ir, "call i8* @promise_string_new(")
}

// B0179: Shared borrow of string field must NOT dup the string.
// The borrow doesn't own the value — duping creates a temp that gets freed
// while the borrow still points to it (use-after-free / double-free).
func TestStringBorrowFieldNoDup(t *testing.T) {
	ir := generateIR(t, `
		type Pair {
			string a;
			string b;
		}
		get_ref(string s) string & {
			return s;
		}
		test() {
			p := Pair(a: "hello", b: "world");
			string & ra = get_ref(p.a);
		}
	`)
	// The test function should NOT contain a string dup — the param is a borrow.
	testFn := extractFunction(ir, "__user.test")
	assertNotContains(t, testFn, "call i8* @promise_string_new(")
}

// B0164: bindingFree emits pal_free on non-droppable heap types with multiple fields
func TestBindingFreeMultipleFields(t *testing.T) {
	ir := generateIR(t, `
		type Config {
			int port;
			bool verbose;
		}
		main() {
			c := Config(port: 8080, verbose: true);
			int p = c.port;
		}
	`)
	assertContains(t, ir, "c.dropflag")
	assertContains(t, ir, "call void @pal_free")
	assertNotContains(t, ir, "Config.drop")
}

// B0164: bindingFree works on reassignment — frees old value before storing new
func TestBindingFreeReassignment(t *testing.T) {
	ir := generateIR(t, `
		type Pair { int x; int y; }
		test() {
			p := Pair(x: 1, y: 2);
			p = Pair(x: 3, y: 4);
		}
		main() {}
	`)
	assertContains(t, ir, "p.dropflag")
	// Should have two pal_alloc (one per constructor) and free.call blocks
	assertContains(t, ir, "free.call")
	assertContains(t, ir, "call void @pal_free")
}

// B0199: Constructor call sites keep caller's drop flag for string-typed borrow
// parameters on types with HasDrop(). The constructor body strdups the string
// (genAssignment detects no drop flag on the param), so the caller must keep
// its drop flag to free the original string.
func TestConstructorBorrowParamKeepsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type Holder {
			string data;
			new(~this, string s) {
				this.data = s;
			}
		}
		test() {
			string mystr_b0199 = "hello";
			h := Holder(s: mystr_b0199);
		}
		main() {}
	`)
	// mystr_b0199's drop flag should NOT be cleared (new() borrows, not moves)
	assertNotContains(t, ir, "store i1 false, i1* %mystr_b0199.dropflag")
}

// T0073: Primitive to_string temp is dropped at statement end when not assigned
func TestStringTempDropAtStatementEnd(t *testing.T) {
	ir := generateIR(t, `
		test() {
			assert(42.to_string() == "42", "ok");
		}
		main() {}
	`)
	// Should have temp drop blocks from cleanupStmtTemps
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "tmp.skip")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0073: Primitive to_string temp is claimed when assigned to a variable
func TestStringTempClaimedOnAssign(t *testing.T) {
	ir := generateIR(t, `
		test() {
			s := 42.to_string();
			assert(s == "42", "ok");
		}
		main() {}
	`)
	// The temp is tracked then claimed — the variable's drop binding handles cleanup.
	// The temp cleanup blocks should still exist but the flag should be cleared.
	assertContains(t, ir, "s.dropflag")
	assertContains(t, ir, "call void @promise_string_drop")
}

// B0172: Temp flag reset after drop — prevents double-free in loops with match arms
func TestStringTempFlagResetInLoop(t *testing.T) {
	ir := generateIR(t, `
		type Builder {
			string result;
			add(~this, string s) { this.result = this.result + s; }
			process(~this, string s) {
				int i = 0;
				while i < s.len {
					char c = s[i];
					match c {
						'_' => { this.add("-"); },
						_ => { this.add(c.to_string()); },
					}
					i = i + 1;
				}
			}
		}
		main() {}
	`)
	// The cleanup code must reset the drop flag to 0 after dropping to prevent
	// double-free when a different match arm executes on the next loop iteration.
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "store i1 false")
}

// B0172: Temp tracking enabled in defineMethodFunc
func TestStringTempTrackingInMethodBody(t *testing.T) {
	ir := generateIR(t, `
		type Fmt {
			format(this) string { return this.to_string(); }
			to_string(this) string { return "fmt"; }
		}
		main() {}
	`)
	// Method bodies should have temp tracking enabled
	assertContains(t, ir, "tmp.drop")
}

// B0168: String concat temp is tracked and dropped at statement end
func TestStringConcatTempDrop(t *testing.T) {
	ir := generateIR(t, `
		test() {
			string name = "world";
			assert("hello " + name == "hello world", "ok");
		}
		main() {}
	`)
	// Concat result should be tracked as a temp and dropped
	assertContains(t, ir, "call i8* @promise_string_concat")
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "call void @promise_string_drop")
}

// B0168: String concat temp claimed when assigned to variable
func TestStringConcatTempClaimedOnAssign(t *testing.T) {
	ir := generateIR(t, `
		test() {
			string name = "world";
			string greeting = "hello " + name;
			assert(greeting == "hello world", "ok");
		}
		main() {}
	`)
	// Concat result is claimed (assigned to greeting), variable drop binding handles it
	assertContains(t, ir, "greeting.dropflag")
	assertContains(t, ir, "call i8* @promise_string_concat")
}

// B0168: String concat temp in constructor arg is claimed (no double-free)
func TestStringConcatTempInConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Greeter { string msg; }
		test() {
			g := Greeter(msg: "hello " + "world");
		}
		main() {}
	`)
	// Should compile and run without double-free; concat temp is claimed
	assertContains(t, ir, "call i8* @promise_string_concat")
}

// B0170: String temp pushed into vector is claimed (no double-free at stmt end)
func TestStringTempClaimedOnVectorPush(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string[] v = string[]();
			v.push("a" + "b");
		}
	`)
	// Concat result should be tracked then claimed by push.
	assertContains(t, ir, "call i8* @promise_string_concat")
	assertContains(t, ir, "call i8* @promise_vector_push")
}

// T0099: to_string() on user type is tracked as a string temp and freed at stmt end.
func TestStringTempUserTypeToString(t *testing.T) {
	ir := generateIR(t, `
		type Counter {
			int n;
			to_string() string { return "count"; }
		}
		test() {
			Counter c = Counter(n: 1);
			assert(c.to_string() == "count", "ok");
		}
		main() {}
	`)
	// c.to_string() produces a temp that's freed at statement end
	assertContains(t, ir, "call i8* @Counter.to_string")
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0099: to_string() on string returns `this` (borrow) — NOT tracked as temp.
func TestStringTempStringToStringNotTracked(t *testing.T) {
	ir := generateIR(t, `
		test() {
			string s = "hello";
			assert(s.to_string() == "hello", "ok");
		}
		main() {}
	`)
	// string.to_string() returns `this` — must NOT be tracked (would double-free).
	// The test function has only one string variable `s` — its drop handles cleanup.
	// Verification: s has a drop flag (the variable's own scope cleanup).
	assertContains(t, ir, "s.dropflag")
}

// T0099: to_string() on user type assigned to variable is claimed (not freed as temp).
func TestStringTempUserTypeToStringClaimed(t *testing.T) {
	ir := generateIR(t, `
		type Tag {
			int id;
			to_string() string { return "tag"; }
		}
		test() {
			Tag t = Tag(id: 1);
			string s = t.to_string();
			assert(s == "tag", "ok");
		}
		main() {}
	`)
	// t.to_string() produces a temp that's tracked then claimed on assignment to s.
	// s has its own drop binding for cleanup.
	assertContains(t, ir, "s.dropflag")
	assertContains(t, ir, "call i8* @Tag.to_string")
}

// T0133: String slice expressions are tracked as temps and freed at statement end.
func TestStringSliceTempDrop(t *testing.T) {
	ir := generateIR(t, `
		test() {
			string s = "hello world";
			assert(s[0:5] == "hello", "ok");
		}
		main() {}
	`)
	// s[0:5] produces a heap-allocated string via native slice (promise_string_new).
	// The slice result must be tracked as a temp and freed at statement end.
	assertContains(t, ir, "call i8* @promise_string_new")
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0133: String slice assigned to variable is claimed (not double-freed).
func TestStringSliceTempClaimedOnAssign(t *testing.T) {
	ir := generateIR(t, `
		test() {
			string s = "hello world";
			string sub = s[0:5];
			assert(sub == "hello", "ok");
		}
		main() {}
	`)
	// s[0:5] is tracked as temp, then claimed when assigned to sub.
	// sub has its own drop binding for scope cleanup.
	assertContains(t, ir, "sub.dropflag")
	assertContains(t, ir, "call i8* @promise_string_new")
}

// T0124: Free function call returning string is tracked as temp and freed at stmt end.
func TestStringTempFreeFunctionCall(t *testing.T) {
	ir := generateIR(t, `
		make_greeting(string name) string {
			return "hello " + name;
		}
		test() {
			assert(make_greeting("world") == "hello world", "ok");
		}
		main() {}
	`)
	// The return value of make_greeting() should be tracked and freed
	assertContains(t, ir, "call i8* @__user.make_greeting")
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0124: Free function call returning string assigned to variable is claimed (not double-freed).
func TestStringTempFreeFunctionCallClaimed(t *testing.T) {
	ir := generateIR(t, `
		make_label(int n) string {
			return n.to_string();
		}
		test() {
			s := make_label(42);
			assert(s == "42", "ok");
		}
		main() {}
	`)
	// The call result should be tracked but claimed on assignment
	assertContains(t, ir, "call i8* @__user.make_label")
	// Drop flag is cleared (claimed) so no free at stmt end for this temp
	assertContains(t, ir, "store i1 false")
}

// B0198: String temps in if-condition must be cleaned up in the merge block.
// When the condition evaluates to false, the then-body never runs but its
// inner-statement cleanup cleared the Go tracking. The merge block must still
// emit flag-guarded cleanup IR.
func TestStringTempIfConditionFalsePath(t *testing.T) {
	ir := generateIR(t, `
		check(string s) bool {
			if s.len >= 4 && s[0:4] == "true" {
				return true;
			}
			return false;
		}
		main() { check("no"); }
	`)
	// The merge block (if.end) must contain cleanup for the condition temp:
	// load drop flag → branch to tmp.drop or tmp.skip
	assertContains(t, ir, "tmp.drop")
	assertContains(t, ir, "tmp.skip")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0082: Structural views are tested at the Promise level (e2e/structural_view_test.pr)
// because structural interface coercion requires the full std library.
// The fix: genTypedVarDecl skips clearDropFlag when LHS is a structural interface.

// B0167: Type with string field gets synthesized drop (cascading instance cleanup)
func TestSynthDropStringFieldCascade(t *testing.T) {
	ir := generateIR(t, `
		type Inner { string name; }
		type Outer { Inner inner; int x; }
		main() {
			o := Outer(inner: Inner(name: "hi"), x: 1);
		}
	`)
	// Both types get synthesized drops
	assertContains(t, ir, "define void @Inner.drop")
	assertContains(t, ir, "define void @Outer.drop")
	// Outer.drop calls Inner.drop (cascading) + pal_free
	assertContains(t, ir, "call void @Inner.drop")
	assertContains(t, ir, "call void @pal_free")
	// String fields are NOT freed by the synthesized drop (no promise_string_drop in drop body)
}

// B0167: Type with vector field gets synthesized drop
func TestSynthDropVectorField(t *testing.T) {
	ir := generateIR(t, `
		type Container { int[] items; }
		main() {
			int[] v = int[]();
			c := Container(items: v);
		}
	`)
	assertContains(t, ir, "define void @Container.drop")
	assertContains(t, ir, "call void @pal_free")
}

// B0192: Non-droppable heap user type fields inside synthesized drop get pal_free
func TestSynthDropFreesNonDroppableHeapField(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		type Wrapper { Point p; string name; }
		main() {
			w := Wrapper(p: Point(x: 1, y: 2), name: "test");
		}
	`)
	wrapperDrop := extractFunction(ir, "Wrapper.drop")
	if wrapperDrop == "" {
		t.Fatal("expected Wrapper.drop function in IR")
	}
	// Synthesized drop should free the Point instance via pal_free
	assertContains(t, wrapperDrop, "call void @pal_free(")
	// And drop the string field
	assertContains(t, wrapperDrop, "call void @promise_string_drop(")
}

// B0264: Vector[(string, int)] must drop string elements inside tuples.
func TestVectorTupleElementStringDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			(string, int)[] v = [("hello", 1), ("world", 2)];
		}
	`)
	// The vector drop loop should extract tuple field 0 (string) and call promise_string_drop
	assertContains(t, ir, "vecdrop.body")
	assertContains(t, ir, "extractvalue")
	assertContains(t, ir, "call void @promise_string_drop")
}

// T0371: genTupleLit claims the heap-tracked string concat temp so the tuple
// is the unique owner of the concat result. Without this claim, the stmt-temp
// cleanup would free the string while the tuple still references it (UAF).
func TestT0371TupleLitClaimsHeapStringTemp(t *testing.T) {
	ir := generateIR(t, `
		main() {
			(int, string) t = (1, "a" + "b");
		}
	`)
	// Concat result is tracked as a stmtTemp; genTupleLit clears its drop flag.
	assertContains(t, ir, "promise_string_concat")
	assertContains(t, ir, "store i1 false")
	// And the tuple variable t gets a tuple-walk drop binding at scope exit.
	assertContains(t, ir, "tupdrop.exec")
	assertContains(t, ir, "tupdrop.skip")
	assertContains(t, ir, "promise_string_drop")
}

// T0371: genTupleLit claims the heap-tracked user type temp so the tuple is
// the unique owner. Without this claim, the heap temp would be freed at stmt
// end and dropped again via the tuple's scope-exit walk (double-free).
func TestT0371TupleLitClaimsHeapBoxTemp(t *testing.T) {
	ir := generateIR(t, `
		type Box { int n; }
		main() {
			(int, Box) t = (1, Box(n: 5));
		}
	`)
	// Tuple variable t gets a tuple-walk drop binding.
	assertContains(t, ir, "tupdrop.exec")
	// The walk frees the heap Box via emitVariantFieldDrop's pal_free branch.
	assertContains(t, ir, "varfield.free")
	assertContains(t, ir, "@pal_free")
}

// T0371: A tuple variable with droppable fields registers a bindingDropTuple
// that walks the fields and drops each droppable one at scope exit. The drop
// flag is checked first so moves can suppress the walk.
func TestT0371TupleVarDropsFields(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "a" + "b";
			(int, string) t = (1, s);
		}
	`)
	// Tuple var has a drop flag and a tupdrop block at scope exit.
	assertContains(t, ir, "t.dropflag")
	assertContains(t, ir, "tupdrop.exec")
	assertContains(t, ir, "promise_string_drop")
}

// T0481: `(_, n) := t` with an owned tuple source must register a drop binding
// for the discarded slot under a synthetic key. Without it, the source's drop
// flag is cleared (transfer to LHS) but the heap field at `_` is orphaned.
func TestT0481DiscardRegistersDropString(t *testing.T) {
	ir := generateIR(t, `
		main() {
			t := ("a" + "b", 42);
			(_, n) := t;
		}
	`)
	// The synthetic discard binding must produce a drop flag and a string drop.
	assertContains(t, ir, "_destructure.discard")
	assertContains(t, ir, "_destructure.discard.dropflag")
	assertContains(t, ir, "promise_string_drop")
}

// T0481: Borrow-source destructure with `_` must NOT register a drop binding
// for the discarded slot — borrowed elements are owned by the container, and
// adding a drop would double-free with the container's element walk.
func TestT0481DiscardBorrowSourceNoDrop(t *testing.T) {
	ir := generateIR(t, `
		sum_seconds((string, int)[] v) int {
			int total = 0;
			for tup in v {
				(_, n) := tup;
				total = total + n;
			}
			return total;
		}
		main() {
			int n = sum_seconds([("a" + "b", 5)]);
		}
	`)
	// No drop binding for the borrowed `_` slot inside the for-in loop.
	assertNotContains(t, ir, "_destructure.discard.dropflag")
}

// B0158: Synthesized drop coexists with explicit drop (explicit takes precedence)
func TestDropExplicitTakesPrecedence(t *testing.T) {
	ir := generateIR(t, `
		type Inner {
			int id;
			drop(~this) { }
		}
		type Outer {
			Inner inner;
			drop(~this) { }
		}
		main() {
			o := Outer(inner: Inner(id: 1));
		}
	`)
	assertContains(t, ir, "call void @Outer.drop")
	assertContains(t, ir, "call void @Inner.drop")
	// Explicit drop should NOT have pal_free auto-appended (that's a separate concern)
}

// T0357: local-var string compound must drop the old value before storing
// the new concat result, with a same-pointer guard mirroring OpAssign.
func TestCompoundAssignStringDropsOld(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string a = "hello " + "first ";
			a += "world";
		}
	`)
	assertContains(t, ir, "call i8* @promise_string_concat(")
	assertContains(t, ir, "call void @promise_string_drop(")
	assertContains(t, ir, "compound.diff")
	assertContains(t, ir, "compound.merge")
}

// T0405: Vector field reassignment must drop elements before freeing the buffer.
// Verifies that genMemberAssign emits a vector element drop loop (string drop)
// before calling Vector.drop for a string[] field.
func TestFieldAssignVecDropsElements(t *testing.T) {
	ir := generateIR(t, `
		type Holder { string[] field; }
		main() {
			v1 := string[]();
			h := Holder(v1);
			v2 := string[]();
			h.field = v2;
		}
	`)
	// The field.vecdrop block must contain a string element drop loop
	assertContains(t, ir, "field.vecdrop")
	assertContains(t, ir, "call void @promise_string_drop(")
	assertContains(t, ir, "call void @Vector.drop(")
}

// T0516: B0219 dup tracking must be per-receiver so reassigns on a DIFFERENT
// instance of the same type still emit element drops correctly. With per-type
// keying, a dup of h1.field would mark "Holder.field" and cause h2.field = w
// to skip its element drop loop, leaking h2's old elements.
func TestFieldAssignVecCrossInstanceDropsElements(t *testing.T) {
	ir := generateIR(t, `
		type Holder { string[] field; }
		main() {
			h1 := Holder(string[]());
			h2 := Holder(string[]());
			v := h1.field;
			w := string[]();
			h2.field = w;
			x := string[]();
			h1.field = x;
		}
	`)
	// Both reassigns must emit Vector.drop on the buffer. The h2 reassign
	// must also emit a string element drop loop (h2 was not the duped receiver).
	assertContains(t, ir, "field.vecdrop")
	assertContains(t, ir, "call void @promise_string_drop(")
	assertContains(t, ir, "call void @Vector.drop(")
}

// T0540: `v := h.field` for a Vector field with droppable elements on a droppable
// owner must emit a deep element-dup loop (not just a shallow buffer memcpy) so
// the dup owns independent copies. Without the loop, both v and h.field alias
// element pointers and scope-end drops cause a double-free.
func TestB0219FieldAccessVecDeepDup(t *testing.T) {
	ir := generateIR(t, `
		type Holder { string[] field; }
		main() {
			v1 := string[]();
			h := Holder(v1);
			v := h.field;
		}
	`)
	// The shallow dup (vecdup.copy) must be followed by a per-element string
	// dup loop (vecdup_str.head + promise_string_new).
	assertContains(t, ir, "vecdup.copy")
	assertContains(t, ir, "vecdup_str.head")
	assertContains(t, ir, "promise_string_new")
}

func TestDiscardedIntPopNoDropBlock(t *testing.T) {
	// B0196: int pop should NOT emit discard.drop block (only strings need it).
	ir := generateIR(t, `
		main() {
			int[] v = [1, 2, 3];
			v.pop();
		}
	`)
	mainFn := extractFunction(ir, "main")
	assertNotContains(t, mainFn, "discard.drop")
}

func TestAllocatorAttributes(t *testing.T) {
	ir := generateIR(t, `
		type Foo { int x; }
		main() { f := Foo(x: 1); }
	`)
	// Underlying libc declarations still present (emitted by PAL)
	assertContains(t, ir, "declare noalias i8* @malloc(i64 noundef %size) nounwind willreturn")
	assertContains(t, ir, "declare void @free(i8* nocapture noundef %ptr) nounwind willreturn")
	assertContains(t, ir, "declare noalias i8* @realloc(i8* nocapture noundef %ptr, i64 noundef %size) nounwind willreturn")
	// PAL wrappers defined
	assertContains(t, ir, "@pal_alloc(i64 %size)")
	assertContains(t, ir, "@pal_free(i8* %ptr)")
	assertContains(t, ir, "@pal_realloc(i8* %ptr, i64 %size)")
}

// T0858: an early `return;` in main() must still run scope cleanup for heap
// locals allocated before it. The bare-return path branches to the coroutine
// final-suspend block via emitScopeCleanup — verify the string drop is emitted
// (and no bare ret void) so the early-return path can never leak.
func TestMainEarlyReturnRunsScopeCleanup(t *testing.T) {
	ir := generateIR(t, `
		main() {
			s := "abcdef".repeat(50);
			if s.len > 0 { return; }
			print_line(s);
		}
	`)
	body := extractDefine(ir, ".goroutine.main")
	assertNotContains(t, body, "ret void")
	assertContains(t, body, "br label %final.suspend")
	assertContains(t, body, "promise_string_drop")
}

// T1159: fire-and-forget `go f(...)` with a non-void heap result must DROP the
// discarded result in the coroutine body instead of running the result-store
// machinery (there is no receiver and result_ptr stays null). Contrast: the
// task-handle form stores into G.result_ptr via a `store_result:` block. Guards
// the free-function fast path (genGoCallExpr).
func TestT1159_FastPathFireAndForgetDropsResult(t *testing.T) {
	// Fire-and-forget: result discarded → body drops the string, no store block.
	ffBody := defBody(t, generateIR(t, `
		build(int x) string { return "v{x}"; }
		main() { go build(5); }
	`), "define i8* @.goroutine.0(")
	assertContains(t, ffBody, "@promise_string_drop") // discarded result dropped
	assertNotContains(t, ffBody, "store_result:")     // no result-buffer store machinery

	// Task handle: result received → body stores it, no unconditional drop.
	taskBody := defBody(t, generateIR(t, `
		build(int x) string { return "v{x}"; }
		main() { t := go build(5); r := <-t; }
	`), "define i8* @.goroutine.0(")
	assertContains(t, taskBody, "store_result:")           // result stored into G.result_ptr
	assertNotContains(t, taskBody, "@promise_string_drop") // body does not drop — receiver owns it
}

// T0731 (was TestT0688_DerivedTrailingNoDup): a value-returning go-block whose
// trailing expression is DERIVED from a borrowed heap param (e.g. `v + "!"`)
// reads that param ASYNCHRONOUSLY inside the coroutine — after the caller has
// already dropped its `"a"+"b"` stmt-temp. T0688 believed this was safe (the
// coroutine builds a fresh value), but the read of the freed buffer is a real
// UAF; the old test only "passed" because it used a .rodata literal (`"hi"`),
// which is never freed. T0731 generalizes the spawn-side dup to ALL borrowed
// heap captured params on the value-block path, so the coroutine reads its own
// private copy regardless of how the body routes the param into the result. The
// dup IS therefore emitted here.
func TestT0731_DerivedTrailingDups(t *testing.T) {
	ir := generateIR(t, `
		ngmake(string v) Task[string] {
			return go { v + "!" };
		}
		main() {
			task[string] x = ngmake("a" + "b");
			r := <-x;
		}
	`)
	ngmakeIR := extractFunction(ir, "__user.ngmake")
	// The spawning function dups its borrowed string param via
	// promise_string_new before passing the value to the goroutine ramp, so the
	// coroutine's async `v + "!"` reads the owned copy, not the freed arg temp.
	assertContains(t, ngmakeIR, "@promise_string_new(")
	assertContains(t, ngmakeIR, "call i8* @.goroutine.")
}

// T0731: the same async-read UAF triggers when the borrowed heap param is
// aliased through a goroutine-local before flowing to the trailing value
// (`s := if b { v } else { … }; s`). T0688's bare-ident detection did not fire
// (the trailing ident `s` is a coroutine-local, not a capture), so no dup was
// emitted. The generalized fix dups the borrowed param regardless of the
// trailing form.
func TestT0731_AliasedTrailingDups(t *testing.T) {
	ir := generateIR(t, `
		ngmake(string v, bool b) Task[string] {
			return go {
				s := if b { v } else { "other" };
				s
			};
		}
		main() {
			task[string] x = ngmake("a" + "b", true);
			r := <-x;
		}
	`)
	ngmakeIR := extractFunction(ir, "__user.ngmake")
	assertContains(t, ngmakeIR, "@promise_string_new(")
	assertContains(t, ngmakeIR, "call i8* @.goroutine.")
}

// T0688: regression guard — Copy-type param (int) must NOT trigger any
// dup. The eligibility predicate goElemNeedsBorrowedCaptureDup returns
// false for primitives, so the spawning function has zero dup overhead.
func TestT0688_CopyParamNoDup(t *testing.T) {
	ir := generateIR(t, `
		ngint(int v) Task[int] {
			return go { v };
		}
		main() {
			task[int] x = ngint(42);
			r := <-x;
		}
	`)
	ngintIR := extractFunction(ir, "__user.ngint")
	// No dup of any kind should be emitted (no vecdup, no heapdup, no string
	// dup). The capture is the bare i64 value, passed directly to the
	// goroutine.
	assertNotContains(t, ngintIR, "vecdup.copy")
	assertNotContains(t, ngintIR, "heapdup.copy")
	assertNotContains(t, ngintIR, "@promise_string_new(")
	assertContains(t, ngintIR, "call i8* @.goroutine.")
}

// T1196: a FIRE-AND-FORGET VALUE block — the block has a trailing value
// (goIsVoid=false) but the whole `go {…}` is a discarded statement
// (goExprFireAndForget=true). The old guard `!goIsVoid && !goExprFireAndForget`
// excluded this branch from the dup exactly as it did the void branch. T1196's
// lifted guard dups the borrowed string param here too, so the coroutine's
// async `v + "!"` reads the owned copy rather than the freed arg temp. The dup
// (promise_string_new) precedes the goroutine ramp call.
func TestT1196_FireAndForgetValueBlockBorrowedParamDups(t *testing.T) {
	ir := generateIR(t, `
		emit(string v, Channel[string] out) {
			go { out.send(v + "!"); v + "?" };
		}
		main() {
			out := channel[string](capacity: 1);
			emit("a" + "b", out);
			r := <-out;
			out.close();
		}
	`)
	emitIR := extractFunction(ir, "__user.emit")
	assertContains(t, emitIR, "@promise_string_new(")
	assertContains(t, emitIR, "call i8* @.goroutine.")
	newIdx := strings.Index(emitIR, "@promise_string_new(")
	rampIdx := strings.Index(emitIR, "call i8* @.goroutine.")
	if newIdx < 0 || rampIdx < 0 || newIdx > rampIdx {
		t.Fatalf("expected promise_string_new before goroutine ramp in emit:\n%s", emitIR)
	}
}

// T1197: the via-block go-call path (`go obj.method(arg)`, dispatched through
// genGoCallExprViaBlock rather than genGoBlock) must dup borrowed heap captured
// params — both the receiver and the args — at spawn time. Otherwise the
// coroutine async-reads them after the spawning function returns and drops its
// borrowed-arg stmt-temps (UAF / heap corruption). Here `b` (borrowed Box, a
// no-drop heap user type) and `v` (borrowed string) are both dup'd before the
// goroutine ramp call.
func TestT1197_ViaBlockBorrowedParamDups(t *testing.T) {
	ir := generateIR(t, `
		type T1197Box {
			int n;
			derive(this, string v) string { return v + "!"; }
		}
		mk(T1197Box b, string v) Task[string] {
			return go b.derive(v);
		}
		main() {
			b := T1197Box(n: 1);
			task[string] x = mk(b, "a" + "b");
			r := <-x;
		}
	`)
	mkIR := extractFunction(ir, "__user.mk")
	// The borrowed string arg `v` is dup'd via promise_string_new; the borrowed
	// heap-user receiver `b` is dup'd via a heapdup — both before the goroutine
	// ramp call.
	assertContains(t, mkIR, "@promise_string_new(")
	assertContains(t, mkIR, "heapdup.copy")
	assertContains(t, mkIR, "call i8* @.goroutine.")
}

// T1197: a borrowed Copy param (int) passed to a via-block go-call must NOT be
// dup'd — Copy types embed their data and never alias the caller's heap.
func TestT1197_ViaBlockCopyParamNoDup(t *testing.T) {
	ir := generateIR(t, `
		type T1197IntBox {
			int n;
			plus(this, int v) int { return this.n + v; }
		}
		mk(T1197IntBox b, int v) Task[int] {
			return go b.plus(v);
		}
		main() {
			b := T1197IntBox(n: 1);
			task[int] x = mk(b, 41);
			r := <-x;
		}
	`)
	mkIR := extractFunction(ir, "__user.mk")
	assertNotContains(t, mkIR, "@promise_string_new(")
	assertContains(t, mkIR, "call i8* @.goroutine.")
}

// T1197: an OWNED heap local passed to a via-block go-call must not get a
// second dup — B0354's ownership-transfer already hands the local to the
// coroutine. An extra dup would leak the original.
func TestT1197_ViaBlockLocalOwnedNoDoubleDup(t *testing.T) {
	ir := generateIR(t, `
		type T1197EchoBox {
			int n;
			echo(this, string v) string { return v; }
		}
		mk(T1197EchoBox b) Task[string] {
			s := "x" + "y";
			return go b.echo(s);
		}
		main() {
			b := T1197EchoBox(n: 1);
			task[string] x = mk(b);
			r := <-x;
		}
	`)
	mkIR := extractFunction(ir, "__user.mk")
	// The owned local `s` comes from promise_string_concat and is transferred
	// as-is; there must be no extra promise_string_new dup of it.
	assertContains(t, mkIR, "@promise_string_concat(")
	assertNotContains(t, mkIR, "@promise_string_new(")
}

// T1198: the fast path `genGoCallExpr` (bare-ident free-function `go f(v)`) must
// dup a borrowed heap value param of the spawning function — the coroutine reads
// it after the caller frees its own borrowed-arg stmt-temps. Sibling of T1197
// (via-block) on a distinct code path.
func TestT1198_FastPathBorrowedStringParamDups(t *testing.T) {
	ir := generateIR(t, `
		derive(string v, Channel[string] out) {
			out.send(v + "!");
		}
		spawn(string v, Channel[string] out) {
			go derive(v, out);
		}
		main() {
			ch := channel[string](capacity: 1);
			spawn("a" + "b", ch);
			r := <-ch;
		}
	`)
	spawnIR := extractFunction(ir, "__user.spawn")
	// The borrowed string param `v` is dup'd via promise_string_new before the
	// goroutine ramp call.
	assertContains(t, spawnIR, "@promise_string_new(")
	assertContains(t, spawnIR, "call i8* @.goroutine.")
}

// T1198: a borrowed Copy param (int) passed to a fast-path go-call must NOT be
// dup'd — Copy types embed their data and never alias the caller's heap.
func TestT1198_FastPathCopyParamNoDup(t *testing.T) {
	ir := generateIR(t, `
		consume(int v, Channel[int] out) {
			out.send(v);
		}
		spawn(int v, Channel[int] out) {
			go consume(v, out);
		}
		main() {
			ch := channel[int](capacity: 1);
			spawn(41, ch);
			r := <-ch;
		}
	`)
	spawnIR := extractFunction(ir, "__user.spawn")
	assertNotContains(t, spawnIR, "@promise_string_new(")
	assertContains(t, spawnIR, "call i8* @.goroutine.")
}

// T0084: Builder is freed after callFormatToString extracts the string
func TestCallFormatToStringBuilderDrop(t *testing.T) {
	ir := generateIR(t, `
		type Pt {
			int x;
			format!(Writer ~w) { w.write_string("pt"); }
		}
		main() { Pt p = Pt(x: 1); string s = "{p}"; }
	`)
	// After Builder.to_string, Builder.drop should be called to free the Builder
	assertContains(t, ir, "Builder.to_string")
	assertContains(t, ir, "Builder.drop")
}

// B0347: Returning `this.v[i]` where `v: Vector[string]` via a method must
// dup the inner string so the caller owns it. The return-site sets
// `dupStringFieldAccess` for Optional[string] returns; `genMethodIndex`
// consumes it when the target is a container, producing a `promise_string_new`
// call inside the method body.
func TestContainerStringIndexReturnDupsInnerString(t *testing.T) {
	ir := generateIR(t, `
		type Bag {
			string[] v;
			drop(~this) {}
			get_value(int i) string? {
				return this.v[i];
			}
		}
		main() {
			Bag b = Bag(v: string[]());
			b.v.push("value");
			string? r = b.get_value(0);
		}
	`)
	bodyStart := `define { i1, i8* } @Bag.get_value(`
	idx := strings.Index(ir, bodyStart)
	if idx < 0 {
		t.Fatalf("Bag.get_value definition not found in IR")
	}
	bodyEnd := strings.Index(ir[idx:], "\n}\n")
	if bodyEnd < 0 {
		t.Fatalf("could not find end of Bag.get_value body")
	}
	body := ir[idx : idx+bodyEnd]
	if !strings.Contains(body, "promise_string_new") {
		t.Errorf("expected promise_string_new in Bag.get_value body (dup of inner string); got:\n%s", body)
	}
}

func TestSliceAssignVectorDropsSourceBacking(t *testing.T) {
	// B0313: After slice assign, the source vector's backing array must be freed
	// via Vector.drop before clearing its drop flag.
	ir := generateIR(t, `
		main() {
			string[] src = ["hello"];
			string[] v = ["a", "b", "c"];
			v[0:1] = src;
		}
	`)
	assertContains(t, ir, `call void @"Vector[string].[:]=`)
	// B0313: Vector.drop must follow the [:]=  call (shallow free of src backing).
	assertContainsMatch(t, ir, `(?s)Vector\[string\]\.\[:\]=.*?call void @Vector\.drop`)
}

// T1233: A plain (non-generator) function's tuple-by-value param BORROWS — its
// body registers NO callee-side tuple drop flag (superseding T0406's callee-drop
// that double-freed when the caller also owned the tuple). When the arg is a
// tuple-literal TEMP with a heap field, the CALLER frees it field-wise at
// statement end via the `tuptmp.drop` block (registerTupleStmtTemp). This test
// verifies both halves: caller-side temp drop present, callee-side drop absent.
func TestT1233PlainTupleParamBorrowsCallerDropsTemp(t *testing.T) {
	ir := generateIR(t, `
		take((string, int) t) {}
		caller() {
			take(("a" + "b", 1));
		}
	`)
	// Caller drops the tuple-literal temp field-wise at statement end; the heap
	// string field's drop fires there.
	assertContains(t, ir, "tuptmp.drop")
	assertContains(t, ir, "call void @promise_string_drop(")
	// The callee `take` borrows — no tuple drop flag registered in its body.
	takeBody := ir[strings.Index(ir, "@__user.take("):]
	if idx := strings.Index(takeBody, "\ndefine "); idx >= 0 {
		takeBody = takeBody[:idx]
	}
	if strings.Contains(takeBody, "t.dropflag") {
		t.Errorf("plain tuple param should borrow, but callee body registers a tuple drop flag:\n%s", takeBody)
	}
}

// T1233: tupleArgIsCallerOwnedTemp classifies a tuple arg as an owned TEMP the
// caller must drop, or a BORROW owned elsewhere. A getter returning a tuple by
// value produces a FRESH owned tuple (MemberExpr + isGetterCallExpr) → the caller
// registers a temp drop; a plain struct field read (MemberExpr, not a getter)
// BORROWS → no caller temp. This test keeps each callee's only droppable tuple
// arg on that one branch, so `tuptmp.drop` presence/absence pins the decision.
func TestT1233GetterOwnedVsFieldReadBorrow(t *testing.T) {
	// Getter-returned tuple → owned temp → caller drops it (tuptmp.drop present).
	getterIR := generateIR(t, `
		type Box {
			string s;
			get pair(string, int) { return (this.s + "!", 5); }
		}
		take((string, int) t) {}
		caller() {
			b := Box(s: "hi");
			take(b.pair);
		}
	`)
	assertContains(t, getterIR, "tuptmp.drop")

	// Plain field read → borrow → the holder owns/drops the field, so the call
	// site registers NO caller tuple temp (no tuptmp.drop anywhere in the module).
	fieldIR := generateIR(t, `
		type Holder { (string, int) t; }
		take((string, int) t) {}
		caller() {
			h := Holder(t: ("c" + "d", 2));
			take(h.t);
		}
	`)
	assertNotContains(t, fieldIR, "tuptmp.drop")

	// User-defined non-native `[]` returns a FRESH owned tuple (IndexExpr +
	// isUserIndexExpr) → owned temp → caller drops it (tuptmp.drop present).
	userIndexIR := generateIR(t, `
		type Box {
			string s;
			[](int i)(string, int) { return (this.s + "!", i); }
		}
		take((string, int) t) {}
		caller() {
			b := Box(s: "hi");
			take(b[0]);
		}
	`)
	assertContains(t, userIndexIR, "tuptmp.drop")

	// An owned tuple VARIABLE passed to a borrow param is a borrow at the call
	// site (IdentExpr → false): it drops via its own bindingDropTuple, not a
	// caller statement temp — so no `tuptmp.drop` is registered for the arg.
	identIR := generateIR(t, `
		take((string, int) t) {}
		caller() {
			(string, int) v = ("e" + "f", 3);
			take(v);
		}
	`)
	assertNotContains(t, identIR, "tuptmp.drop")
}

// T1233: A bare discarded tuple-literal statement (dropDiscardedTuple) — the
// aggregate has no owning caller variable, so genTupleLit's claimed heap fields
// would orphan. The caller registers a statement temp so the field-wise drop
// fires at statement end. This exercises dropDiscardedTuple's happy path (the
// caller-owned-temp branch + registerTupleStmtTemp) that the arg-passing tests
// don't reach.
func TestT1233BareDiscardedTupleLiteralDrops(t *testing.T) {
	ir := generateIR(t, `
		caller() {
			("a" + "b", 1);
		}
	`)
	assertContains(t, ir, "tuptmp.drop")
	assertContains(t, ir, "call void @promise_string_drop(")
}

func TestVariadicParamDropAtScopeExit(t *testing.T) {
	// B0191: Variadic parameter vectors must be freed at scope exit.
	ir := generateIR(t, `
		sum(...int nums) int {
			int total = 0;
			for n in nums { total += n; }
			return total;
		}
		main() {
			sum(1, 2, 3);
		}
	`)
	assertContains(t, ir, "call void @Vector.drop(i8*")
}

func TestVariadicMethodParamDropAtScopeExit(t *testing.T) {
	// B0191: Variadic method parameter vectors must be freed at scope exit.
	ir := generateIR(t, `
		type Adder {
			int base;

			addAll(this, ...int values) int {
				return this.base;
			}
		}
		main() {
			a := Adder(base: 10);
			a.addAll(1, 2, 3);
		}
	`)
	assertContains(t, ir, "call void @Vector.drop(i8*")
}

func TestInstanceOwnedFuncsTracking(t *testing.T) {
	// instanceOwnedFuncs should map Box[int]'s mangled methods to "Box[int]".
	file, info := parseWithStd(t, boxWithGetMethod+`
		main() {
			b := Box[int](value: 1);
			int x = b.get();
		}
	`)
	result := Compile(file, info, "")
	c := result.compiler

	if len(c.instanceOwnedFuncs) == 0 {
		t.Fatal("expected non-empty instanceOwnedFuncs")
	}

	foundBoxInt := false
	for funcName, instName := range c.instanceOwnedFuncs {
		if instName == "Box[int]" {
			foundBoxInt = true
			if !strings.Contains(funcName, "Box[int]") {
				t.Errorf("function %q tagged as Box[int] but name doesn't contain 'Box[int]'", funcName)
			}
		}
	}
	if !foundBoxInt {
		t.Errorf("no function owned by Box[int]; instanceOwnedFuncs = %v", c.instanceOwnedFuncs)
	}
}

// T1170: a fixed-array element of a match-borrowed payload (`a[0]` where `a`
// binds `string[N]`) escaping to an outer local (`out = a[0]`) must be cloned on
// read (genArrayIndex, driven by the dup flag genAssignStmt sets for a
// borrow-marked array-index RHS), so `out` owns an independent copy that survives
// the subject's synth enum drop.
func TestT1170ArrayElementEscapeDupsOnStore(t *testing.T) {
	ir := generateIR(t, `
		enum Holder { Pair(string[2] a), Empty }
		esc() string {
			Holder h = Holder.Pair(a: ["x" + "1", "y" + "2"]);
			string out = "";
			if h is Pair(a) { out = a[0]; }
			return out;
		}
		main() { s := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	// The escaping array element string is cloned via dupString.
	assertContains(t, fn, "strdup.copy")
}

// T1171: a whole fixed-Array-of-heap-user variant payload (`Row[2] value`) that
// ESCAPES `if is`/`match` (return / store-to-outer / consuming arg / constructor
// field) must be element-wise deep-cloned via dupBorrowedHeapUserPayload's Array
// branch (arrayElemNeedsEscapeDup) — otherwise the escaped [N x {vtable,instance}]
// aggregate aliases the subject's variant payload, which the subject's synth enum
// drop frees at scope exit (UAF / SIGSEGV). Each element clone lowers to a
// dupHeapValue `heapdup.copy` block, and the aggregate is rebuilt with N
// `insertvalue`s (one per element).
func TestT1171ArrayHeapUserPayloadEscapeDups(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		enum Box { Some(Row[2] value), Empty }
		esc() Row[2] {
			Box b = Box.Some(value: [Row(name: "a" + "b"), Row(name: "c" + "d")]);
			if b is Some(value) { return value; }
			return [Row(name: "x"), Row(name: "y")];
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	// Escaping the borrowed Array[Row] payload deep-clones each element.
	assertContains(t, fn, "heapdup.copy")
	// One insertvalue per array element rebuilds the cloned aggregate at the sink.
	if n := strings.Count(fn, "insertvalue"); n < 2 {
		t.Fatalf("expected >= 2 insertvalue (one per array element), got %d\n%s", n, fn)
	}
}

// T1171 over-application guard: an in-scope-only Array[heap-user] binding must
// stay a zero-copy borrow (no dup) — the subject outlives the narrowing and its
// synth enum drop frees each element exactly once. The dup is gated to explicit
// escape sites, so no `heapdup.copy` is emitted here. An over-eager dup would also
// leak.
func TestT1171ArrayHeapUserPayloadInScopeNoDup(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		enum Box { Some(Row[2] value), Empty }
		rd() int {
			Box b = Box.Some(value: [Row(name: "a" + "b"), Row(name: "c" + "d")]);
			int out = 0;
			if b is Some(value) { out = value[0].name.len; }
			return out;
		}
		main() { x := rd(); }
	`)
	fn := extractFunction(ir, "__user.rd")
	assertNotContains(t, fn, "heapdup.copy")
}

// T1176: reading a whole fixed-Array[heap-user] struct field out by value and
// returning it (`return w.rows`) must element-wise deep-clone. genFieldAccess
// routes the array field through dupHeapFieldForEscape's array branch, which
// extractvalue/dupHeapValue/insertvalue's each element. Before the fix the
// [N x {vtable,instance}] aggregate was aliased and the owner's synth drop freed
// each element at scope exit while the returned copy still pointed in (UAF).
// The clone lowers to a per-element dupHeapValue `heapdup.copy` block.
func TestT1176ArrayHeapUserFieldEscapeDupsOnReturn(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		type Wrap { Row[2] rows; }
		esc() Row[2] {
			Wrap w = Wrap(rows: [Row(name: "de" + "ep"), Row(name: "x" + "x")]);
			return w.rows;
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	// Each escaping array element is deep-cloned via dupHeapValue.
	assertContains(t, fn, "heapdup.copy")
	// One insertvalue per element re-assembles the cloned array aggregate
	// ([2 x {vtable,instance}] — value structs are unnamed in the test harness).
	if n := strings.Count(fn, "insertvalue [2 x { i8*, i8* }]"); n < 2 {
		t.Errorf("expected >=2 insertvalue into the cloned array aggregate (one per element), got %d\n%s", n, fn)
	}
}

// T1176: a no-drop-but-pal-free element array field parity case — a heap-user
// type with only scalar fields has no synth drop but is still pal_free'd, so
// arrayElemNeedsEscapeDup's isHeapUserNoDropPalFree branch must still deep-clone each
// escaping element. A value-type element would be copied by value and route
// past arrayElemNeedsEscapeDup, so the presence of `heapdup.copy` confirms the no-drop
// heap branch is taken.
func TestT1176ArrayNoDropHeapUserFieldEscapeDups(t *testing.T) {
	ir := generateIR(t, `
		type P { int x; }
		type Wrap { P[2] cells; }
		esc() P[2] {
			Wrap w = Wrap(cells: [P(x: 11), P(x: 22)]);
			return w.cells;
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertContains(t, fn, "heapdup.copy")
}

// T1176 over-application guard: an in-scope read of a fixed-Array[heap-user]
// field element (no escape → no dup flag set) must NOT clone. This proves the
// escape dup is gated on an owning sink and in-scope borrows stay zero-copy
// (an over-eager clone would also leak).
func TestT1176ArrayHeapUserFieldInScopeNoDup(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		type Wrap { Row[2] rows; }
		rd() int {
			Wrap w = Wrap(rows: [Row(name: "de" + "ep"), Row(name: "x" + "x")]);
			return w.rows[0].name.len;
		}
		main() { x := rd(); }
	`)
	fn := extractFunction(ir, "__user.rd")
	assertNotContains(t, fn, "heapdup.copy")
}

// T1176 gate negative: escaping a value-element array field (`int[2]`) out of a
// DROPPABLE owner must NOT clone — arrayElemNeedsEscapeDup recognizes the array but its
// element is neither a droppable heap-user nor a no-drop-pal-free type, so it
// returns false (the `int[]`/value-array fall-through) and the field is copied
// by value. The owner is made droppable by its sibling string field, so the
// escape sink runs setDupFlagsForFieldAccess → arrayElemNeedsEscapeDup for real; the
// absence of `heapdup.copy` proves value arrays are left untouched.
func TestT1176ValueArrayFieldEscapeNoDup(t *testing.T) {
	ir := generateIR(t, `
		type VW { int[2] a; string s; }
		esc() int[2] {
			VW w = VW(a: [7, 8], s: "x" + "y");
			return w.a;
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertNotContains(t, fn, "heapdup.copy")
}

// T0137: String embed getter result assigned to variable — drop flag must NOT be cleared.
func TestEmbedStringGetterDrop(t *testing.T) {
	file, info := parseWithStd(t, `
		get greeting string `+"`embed(\"greeting.txt\")"+`;
		test_it() `+"`test"+` {
			string a = greeting;
		}
	`)
	for _, embed := range info.Embeds {
		embed.Data = []byte("hello")
	}
	result := Compile(file, info, "")
	ir := result.Module.String()
	// The variable should have a string drop at scope exit.
	assertContains(t, ir, "@promise_string_drop")
	// The drop flag must NOT be cleared to false immediately after being set to true.
	// Before the fix, the IR had: store i1 true, %a.dropflag; store i1 false, %a.dropflag
	assertNotContains(t, ir, "store i1 true, i1* %a.dropflag\n\tstore i1 false, i1* %a.dropflag")
}

// T0137: Bytes embed getter result assigned to variable — drop flag must NOT be cleared.
func TestEmbedBytesGetterDrop(t *testing.T) {
	file, info := parseWithStd(t, `
		get binary u8[] `+"`embed(\"data.bin\")"+`;
		test_it() `+"`test"+` {
			u8[] d = binary;
		}
	`)
	for _, embed := range info.Embeds {
		embed.Data = []byte{0xDE, 0xAD}
	}
	result := Compile(file, info, "")
	ir := result.Module.String()
	// The variable should have a vector drop at scope exit.
	assertContains(t, ir, "@Vector.drop")
	// The drop flag must NOT be cleared to false immediately after being set to true.
	assertNotContains(t, ir, "store i1 true, i1* %d.dropflag\n\tstore i1 false, i1* %d.dropflag")
}

// B0175: Heap temp claim on method receiver in chained calls
func TestHeapTempClaimOnMethodReceiver(t *testing.T) {
	ir := generateIR(t, `
		type Counter is Iterator[int] {
			int n;
			next(~this) int? {
				if this.n <= 0 { return none; }
				this.n = this.n - 1;
				return this.n;
			}
		}
		test() {
			c := Counter(n: 100);
			Iterator[int] result = c.filter(|int x| -> bool { return x % 2 == 0; }).take(3);
			int sum = 0;
			for x in result {
				sum = sum + x;
			}
		}
		main() {}
	`)
	// The chained call c.filter(...).take(3) must claim the filter result
	// (intermediate heap temp) before calling .take(3) on it.
	// Both the filter result and the take result get heap.claim blocks.
	assertContains(t, ir, "heap.claim")
}

// B0187: Reassignment of structural interface variable must claim heap temp
func TestHeapTempClaimOnReassignment(t *testing.T) {
	ir := generateIR(t, `
		type Counter is Iterator[int] {
			int n;
			next(~this) int? {
				if this.n <= 0 { return none; }
				this.n = this.n - 1;
				return this.n;
			}
		}
		test() {
			c := Counter(n: 10);
			Iterator[int] it = c.take(5);
			it = c.take(3);
			int sum = 0;
			for x in it {
				sum = sum + x;
			}
		}
		main() {}
	`)
	// The reassignment `it = c.take(3)` must generate a heap.claim block
	// to prevent the new iterator instance from being double-freed
	// (once at statement end via cleanupHeapTemps, again at scope exit via emitFreeCall).
	assertContains(t, ir, "heap.claim")
}

// B0211: Discarded constructor call for a heap user type without drop should emit pal_free.
func TestDropDiscardedHeapTypeConstructor(t *testing.T) {
	ir := generateIR(t, `
		type Pt {
			int x;
			int y;
		}
		test() {
			Pt(x: 1, y: 2);
		}
	`)
	assertContains(t, ir, "discard.heap.free")
	assertContains(t, ir, "call void @pal_free")
}

// B0211: Discarded constructor call for a heap user type WITH drop should call drop.
func TestDropDiscardedHeapTypeConstructorWithDrop(t *testing.T) {
	ir := generateIR(t, `
		type Res {
			int id;
			drop(~this) {}
		}
		test() {
			Res(id: 1);
		}
	`)
	assertContains(t, ir, "discard.heap.free")
	assertContains(t, ir, "call void @Res.drop")
}

// B0211: Discarded method call returning a heap type should NOT emit discard.heap.free
// (only constructor calls are safe to free — method returns may share instance pointers).
func TestNoDropDiscardedMethodReturnHeapType(t *testing.T) {
	ir := generateIR(t, `
		type Pt {
			int x;
			int y;
		}
		make_pt() Pt {
			return Pt(x: 1, y: 2);
		}
		test() {
			make_pt();
		}
	`)
	testFn := extractFunction(ir, "__user.test")
	if strings.Contains(testFn, "discard.heap.free") {
		t.Fatalf("expected test function to NOT contain discard.heap.free for method return\ngot:\n%s", testFn)
	}
}

// B0211: String temp should NOT be claimed when constructor will strdup (NeedsSynthDrop).
func TestStringTempNotClaimedForSynthDrop(t *testing.T) {
	ir := generateIR(t, `
		type Named {
			string name;
			new(~this, string name) {
				this.name = name;
			}
		}
		make_name() string {
			return "hello";
		}
		test() {
			Named n = Named(name: make_name());
		}
	`)
	// The string from make_name() should be freed (not claimed by constructor).
	// Look for promise_string_drop in the test function — indicates the temp is freed.
	assertContains(t, ir, "promise_string_drop")
}

// B0233: Constructor temps passed to non-~ methods should be freed at statement end.
// The constructor should NOT claim the heap temp — only downstream consumers should.
func TestConstructorTempFreedAtStmtEnd(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		check(Point p) bool { return p.x == 0; }
		main() {
			check(Point(x: 0, y: 0));
		}
	`)
	// The Point(x: 0, y: 0) temp should be freed at statement end via heap.drop,
	// NOT claimed by the constructor. The heap.drop block calls pal_free on the
	// unclaimed temp.
	assertContains(t, ir, "heap.drop")
	assertContains(t, ir, "heap.exec")
}

// B0233: Constructor temps assigned to variables should still be claimed.
func TestConstructorTempClaimedOnAssign(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		main() {
			Point p = Point(x: 1, y: 2);
		}
	`)
	// Variable assignment claims the heap temp — heap.claim block should exist.
	assertContains(t, ir, "heap.claim")
}

// B0233: Constructor temps passed to vector push should be claimed.
func TestConstructorTempClaimedOnPush(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		main() {
			Point[] v = [Point(x: 1, y: 2)];
			v.push(Point(x: 3, y: 4));
		}
	`)
	// Push claims the heap temp — heap.claim block should exist.
	assertContains(t, ir, "heap.claim")
}

// B0237: Constructor temps passed as map literal values should be claimed.
func TestConstructorTempClaimedInMapLiteral(t *testing.T) {
	ir := generateIR(t, `
		type Point { int x; int y; }
		main() {
			map[string, Point] m = { "a": Point(x: 1, y: 2) };
		}
	`)
	// Map literal initialization claims the heap temp.
	assertContains(t, ir, "heap.claim")
}

// B0280: Map literal with identifier key must clear key's drop flag too.
func TestMapLitClearsDropFlagOnKey(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string k = "mykey";
			map[string, int] m = { k: 42 };
		}
	`)
	// k is a string variable with a drop flag — it should be cleared
	assertContains(t, ir, "k.dropflag")
	assertContains(t, ir, "store i1 false, i1* %k.dropflag")
}

// T0736: A map literal whose value is a bare heap sub-expression (string concat,
// to_string(), split(), ...) registers a string/vector *stmt-temp*, not a heap
// temp. genMapLit must claim that stmt-temp (clear its drop flag) after the []=
// move — otherwise the caller's stmt-temp cleanup drops the string while the
// map's scope-exit drop drops it again → double-free ("invalid free"). The
// ident-`clearDropFlag` (B0280) and `claimHeapTemp` paths don't cover stmt-temps.
func TestT0736MapLitClaimsHeapStringValueTemp(t *testing.T) {
	ir := generateIR(t, `
		main() {
			map[string, string] m = {"k": "a" + "b"};
		}
	`)
	// Scope to the user's main goroutine body — the stdlib also defines and
	// calls Map[string, string].[]= elsewhere. The call site of .goroutine.main
	// (in the entry main) precedes its definition, so locate the definition
	// directly rather than via extractFunction.
	start := strings.Index(ir, "define i8* @.goroutine.main")
	if start < 0 {
		t.Fatal("expected .goroutine.main")
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("expected closing brace")
	}
	body := rest[:end+2]
	assertContains(t, body, "call i8* @promise_string_concat")
	// The concat value is moved into the map via []=, then its stmt-temp drop
	// flag is cleared (claimed) — the []= call is immediately followed by a
	// flag-clearing store. Without the fix the next line is instead the
	// `store { i8*, i8* } ... %m` map binding (no claim), and this fails.
	assertContainsMatch(t, body,
		`(?s)call void @"Map\[string, string\]\.\[\]="\([^\n]*\)\n\s*store i1 false, i1\*`)
}

// T0735: A map literal used directly as a borrowed function argument must
// register a stmt-temp drop binding so the map instance + its _buckets vector
// are freed at statement end. Pre-fix, genMapConstructor never called
// trackHeapTemp, so unclaimed map literal temps leaked 2 allocations.
// The fix mirrors genConstructorCallMono (T0135 + T0345): trackHeapTemp with
// palFree as the safe default, then updateConstructorTempDrop swaps in the
// type's full synth drop after new() completes.
func TestT0735_MapLitArgTracksHeapTemp(t *testing.T) {
	ir := generateIR(t, `
		borrow_map(Map[string, int] m) int { return 0; }
		main() {
			int x = borrow_map({"a": 1, "b": 2});
		}
	`)
	start := strings.Index(ir, "define i8* @.goroutine.main")
	if start < 0 {
		t.Fatal("expected .goroutine.main")
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("expected closing brace")
	}
	body := rest[:end+2]
	// After the borrow_map call, the unclaimed map temp must flow through a
	// heap.drop block that calls Map[string, int].drop (the synthesized drop
	// walks _buckets and pal_frees the instance — without the swap, just a
	// pal_free of the instance would leak the buckets vector buffer).
	assertContains(t, body, "call i64 @__user.borrow_map(")
	assertContains(t, body, "heap.drop")
	assertContains(t, body, "heap.exec")
	assertContains(t, body, `call void @"Map[string, int].drop"`)
}

// T0735: A map literal used as a method-call receiver (rvalue temp) — same
// stmt-temp drop registration must apply. `{...}.len` returns a primitive but
// the receiver map still needs cleanup.
func TestT0735_MapLitMethodReceiverTracksHeapTemp(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int n = {"a": 1, "b": 2}.len;
		}
	`)
	start := strings.Index(ir, "define i8* @.goroutine.main")
	if start < 0 {
		t.Fatal("expected .goroutine.main")
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("expected closing brace")
	}
	body := rest[:end+2]
	assertContains(t, body, `call i64 @"Map[string, int].len"`)
	assertContains(t, body, "heap.drop")
	assertContains(t, body, `call void @"Map[string, int].drop"`)
}

// T0735: Map literal bound to a local first must still work — the local's
// regular bindingDrop (registered by genAssignment) handles the cleanup, and
// claimHeapTemp at the assignment site clears the heap-temp flag so the
// instance isn't double-freed. Verifies the existing local-binding path is
// undisturbed by the new stmt-temp registration.
func TestT0735_MapLitLocalStillDropped(t *testing.T) {
	ir := generateIR(t, `
		main() {
			map[string, int] m = {"a": 1, "b": 2};
		}
	`)
	start := strings.Index(ir, "define i8* @.goroutine.main")
	if start < 0 {
		t.Fatal("expected .goroutine.main")
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("expected closing brace")
	}
	body := rest[:end+2]
	// The local m's drop still fires (via bindingDrop), so Map.drop must
	// appear in the body. The heap-temp flag should also be cleared at the
	// assignment site so the heap.drop path doesn't double-free.
	assertContains(t, body, `call void @"Map[string, int].drop"`)
	assertContains(t, body, "heap.claim")
}

// T0735: A map literal passed to a `~Map` parameter (consume-arg) must be
// claimed at the call site — the callee owns and drops, so the caller's
// heap-temp drop flag must be cleared to avoid double-free. Verifies the
// claim path on the move-arg ABI.
func TestT0735_MapLitMoveArgClaimed(t *testing.T) {
	ir := generateIR(t, `
		consume(Map[string, int] move m) int { return m.len; }
		main() {
			int x = consume({"a": 1});
		}
	`)
	start := strings.Index(ir, "define i8* @.goroutine.main")
	if start < 0 {
		t.Fatal("expected .goroutine.main")
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("expected closing brace")
	}
	body := rest[:end+2]
	assertContains(t, body, "call i64 @__user.consume(")
	// claim must fire before the call returns so the heap.drop at statement
	// end sees flag=false; callee runs Map.drop itself.
	assertContains(t, body, "heap.claim")
}

// T0610: A vector literal whose element is a moved local variable of a type
// Vector.drop's element-walk frees (heap-user-with-drop, string, droppable
// enum, Mutex/Task, nested vector) must clear the source ident's drop flag —
// otherwise the source variable's scope-exit drop AND Vector.drop's element
// walk free the same allocation (double-free / SEGV). Mirrors genTupleLit
// (B0242) / genMapLit (B0280).
func TestVectorLitMoveFromVarClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type _Box { string label; drop(~this) {} }
		main() {
			_Box b0 = _Box(label: "a");
			_Box[] v = [b0];
		}
	`)
	// b0 is moved into the vector literal — its drop flag must be cleared so
	// Vector.drop's element walk becomes the sole owner.
	assertContains(t, ir, "store i1 false, i1* %b0.dropflag")
}

// T0610: a droppable tuple bound to a *variable* and moved into a vector
// literal must clear the source ident's drop flag — exercising the
// tupleNeedsDrop arm of the type-gate. Existing tuple-in-vector-literal
// tests only use inline tuple constructors (no ident move), so this is the
// sole IR coverage of the tupleNeedsDrop branch. Without the clear, the
// tuple field is freed by both the var's bindingDropTuple and Vector.drop's
// element walk → double-free (verified at runtime against baseline).
func TestVectorLitMoveFromVarTupleClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type _Box { string label; drop(~this) {} }
		main() {
			(int, _Box) t = (1, _Box(label: "a"));
			(int, _Box)[] v = [t];
		}
	`)
	// The tuple var has its own drop flag (bindingDropTuple, T0371).
	assertContains(t, ir, "%t.dropflag = alloca i1")
	// Moved into the vector literal — the tupleNeedsDrop arm must clear it so
	// Vector.drop's element walk becomes the sole owner of the tuple's _Box.
	assertContains(t, ir, "store i1 false, i1* %t.dropflag")
}

// T0610: a plain heap user type with NO drop method (and no droppable
// fields) moved from a variable into a vector literal must still clear the
// source ident's drop flag — exercising the "needs pal_free" arm of
// vecElemNeedsUserTypeDrop (stmt.go:3983). All other T0610 tests use a type
// WITH an explicit drop; this is the sole coverage of the pal_free-only
// element path. Without the clear, both the var's scope-exit pal_free and
// Vector.drop's element walk free the same allocation → double-free
// (verified at runtime against baseline).
func TestVectorLitMoveFromVarPlainHeapClearsDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type _Plain { int x; int y; }
		main() {
			_Plain p = _Plain(x: 1, y: 2);
			_Plain[] v = [p];
		}
	`)
	assertContains(t, ir, "%p.dropflag = alloca i1")
	assertContains(t, ir, "store i1 false, i1* %p.dropflag")
}

// B0245: Vector[UserType] drop should emit element drop loop for heap user types.
func TestVectorUserTypeElementDrop(t *testing.T) {
	ir := generateIR(t, `
		type Foo { int x; }
		main() {
			Foo[] v = [];
			v.push(Foo(x: 1));
		}
	`)
	// Should have vector element drop loop (vecdrop.head) for user type elements
	assertContains(t, ir, "vecdrop.head")
	assertContains(t, ir, "call void @pal_free(")
}

// B0245: Debug — check what IR is generated for Vector[Foo] with full std
func TestVectorUserTypeElementDropWithPush(t *testing.T) {
	ir := generateIR(t, `
		type Foo { int x; }
		test() {
			Foo[] v = [];
			v.push(Foo(x: 1));
			v.push(Foo(x: 2));
		}
	`)
	// Check for element drop loop: vecdrop.head is the loop header
	if !strings.Contains(ir, "vecdrop.head") {
		// Print the main function IR for debugging
		lines := strings.Split(ir, "\n")
		inFunc := false
		for _, line := range lines {
			if strings.Contains(line, "define") && strings.Contains(line, "@test(") {
				inFunc = true
			}
			if inFunc {
				t.Logf("%s", line)
				if line == "}" {
					break
				}
			}
		}
		t.Errorf("expected vecdrop.head element drop loop for Vector[Foo]")
	}
}

// B0257: Vector element drop loop must call both the user type's drop method
// and pal_free to free the instance memory.
func TestVectorUserTypeDropCallsFree(t *testing.T) {
	ir := generateIR(t, `
		type Res { int id; drop(~this) {} }
		test() {
			Res[] v = [];
			v.push(Res(id: 1));
		}
	`)
	// Element drop loop header must exist
	assertContains(t, ir, "vecdrop.head")
	// The loop body must call Res.drop AND pal_free (not just drop)
	assertContains(t, ir, "call void @Res.drop(")
	// pal_free must appear in the element drop loop (for the instance memory)
	assertContains(t, ir, "call void @pal_free(")
}

// T0109: Vector-producing call expressions (e.g., split()) are tracked as stmt temps.
func TestVectorCallExprStmtTempTracking(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int n = "a b c".split(" ").len;
		}
	`)
	// The vector temp from split() should be tracked and dropped.
	assertContains(t, ir, "call void @Vector.drop(")
}

// B0325: Field access on a type with explicit drop must use the $wrap function
// (drop + pal_free), not just the raw drop function.
func TestFieldAccessExplicitDropUsesWrap(t *testing.T) {
	ir := generateIR(t, `
		type Resource { int id; drop(~this) {} }
		make_resource!() Resource { return Resource(id: 42); }
		test!() {
			int v = make_resource()?!.id;
		}
	`)
	assertContains(t, ir, "heap.drop")
	// Must use the $wrap function that calls drop + pal_free
	assertContains(t, ir, "Resource.drop$wrap")
}

// === Clone annotation (T0154) ===

func TestCloneSynthesizesCloneMethod(t *testing.T) {
	ir := generateIR(t, `
		type Doc `+"`clone"+` {
			string title;
			int pages;
		}
		test() {
			d := Doc(title: "hi", pages: 1);
			d2 := d.clone();
		}
	`)
	// The synthesized clone method should exist and call promise_string_new (dupString)
	assertContains(t, ir, "Doc.clone")
}

func TestCloneStringNativeMethod(t *testing.T) {
	ir := generateIR(t, `
		test() {
			s := "hello";
			s2 := s.clone();
		}
	`)
	// string.clone() calls promise_string_new (dupString)
	assertContains(t, ir, "promise_string_new")
}

func TestCloneVectorNativeMethod(t *testing.T) {
	ir := generateIR(t, `
		test() {
			v := [1, 2, 3];
			v2 := v.clone();
		}
	`)
	// Vector.clone() calls pal_alloc (dupVector)
	assertContains(t, ir, "pal_alloc")
}

func TestCloneStringVectorDupsElements(t *testing.T) {
	ir := generateIR(t, `
		test() {
			v := ["a", "b"];
			v2 := v.clone();
		}
	`)
	// String vector clone should have the string dup loop
	assertContains(t, ir, "vecdup_str.head")
	assertContains(t, ir, "promise_string_new")
}

// B0275: Vector.clone() must deep-clone heap user type elements.
func TestCloneVectorHeapTypeCallsClone(t *testing.T) {
	ir := generateIR(t, `
		type Foo `+"`"+`clone {
			string name;
		}
		test() {
			v := [Foo(name: "a")];
			v2 := v.clone();
		}
	`)
	// Should have the clone loop calling Foo.clone
	assertContains(t, ir, "vecclone.head")
	assertContains(t, ir, "Foo.clone")
}

// B0276: dupHeapValueFields must deep-clone vector fields with droppable elements.
// When a heap type without clone() has a string[] field and is dup'd via
// dupHeapValue (e.g., as a vector element during clone), the string[] field
// must be deep-cloned, not shallow-copied.
func TestDupHeapValueFieldsDeepClonesVectorStrings(t *testing.T) {
	ir := generateIR(t, `
		type Container {
			string[] names;
			int id;
		}
		test() {
			v := [Container(names: ["a", "b"], id: 1)];
			v2 := v.clone();
		}
	`)
	// Vector[Container].clone() → emitVectorElementCloneLoop → cloneHeapElement
	// → dupHeapValue → dupHeapValueFields → should deep-clone the string[] field.
	assertContains(t, ir, "vecdup_str.head")
}

// B0289: emitVectorElementCloneLoop → cloneHeapElement must check type-arg safety
// before calling clone(). When vector elements are Map[string, NonCloneableEnum],
// Map.clone() would shallow-copy the enum values → double-free. The fix falls
// back to dupHeapValue instead.
func TestVectorCloneLoopSkipsUnsafeMapClone(t *testing.T) {
	ir := generateIR(t, `
		enum JsonNode {
			Null,
			Text(string value),
			Dict(map[string, JsonNode] fields),
		}
		test() {
			map[string, JsonNode][] maps = [{"k": JsonNode.Text(value: "v")}];
			map[string, JsonNode][] maps2 = maps.clone();
		}
	`)
	// T1129: JsonNode (recursive, Map-bearing) now has a synthesized recursive
	// clone, so Map[string, JsonNode] IS deep-cloneable. Vector[Map[...]].clone()
	// → emitVectorElementCloneLoop → cloneHeapElement now correctly routes through
	// Map[string, JsonNode].clone() (whose internal match-dup recurses via
	// @JsonNode.clone). This supersedes the old B0289 dupHeapValue fallback, whose
	// recursion guard shallow-copied the inner Map → double-free. jn3/jn4 runtime
	// tests confirm this path is leak-free.
	if extractDefine(ir, "JsonNode.clone") == "" {
		t.Errorf("T1129: expected a synthesized @JsonNode.clone for the recursive enum:\n%s", ir)
	}
	if !strings.Contains(ir, "Map[string, JsonNode].clone") {
		t.Errorf("T1129: Vector[Map[string, JsonNode]].clone() should deep-clone elements "+
			"via Map.clone() now that JsonNode is cloneable:\n%s", ir)
	}
}

// B0289: When vector elements are Map[string, int] (safe type args),
// cloneHeapElement should still call Map.clone().
func TestVectorCloneLoopCallsSafeMapClone(t *testing.T) {
	ir := generateIR(t, `
		test() {
			map[string, int][] maps = [{"a": 1}];
			map[string, int][] maps2 = maps.clone();
		}
	`)
	// Map[string, int] — both type args are safe, clone should be called.
	assertContains(t, ir, "Map[string, int].clone")
}

// B0302: Pushing a vector into a vector-of-vectors must dup the inner vector
// to ensure exclusive ownership. Without dup, filled() creates aliased pointers
// that cause double-free on the outer vector's element-level drop.
func TestVectorPushDupsDroppableElement(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[][] v = [];
			int[] inner = [1, 2, 3];
			v.push(inner);
		}
	`)
	// The push should dup the inner vector (vecdup.copy block from dupVector)
	assertContains(t, ir, "vecdup.copy")
}

// B0343: Map []= with borrow-string key must dup so the map owns the key.
func TestMapIndexAssignDupBorrowKey(t *testing.T) {
	ir := generateIR(t, `
		type Sink {
			map[string, string] m;
			put(~this, string k, string v) {
				this.m[k] = v;
			}
		}
		test() {
			Sink s = Sink(m: map[string, string]());
			s.put("a", "b");
		}
	`)
	// The key "k" (borrow param, no drop flag) must be dup'd at the []= site
	assertContains(t, ir, "strdup.copy")
}

// B0343: Map []= with owned-string key (has drop flag) clears the flag.
func TestMapIndexAssignClearKeyDropFlag(t *testing.T) {
	ir := generateIR(t, `
		test() {
			map[string, string] m = map[string, string]();
			for k, v in m {
				map[string, string] dst = map[string, string]();
				dst[k] = v;
			}
		}
	`)
	// Key k has a drop flag from B0343; dst[k] = v clears it
	assertContains(t, ir, "k.dropflag")
	assertContains(t, ir, "forin.key.drop")
}

// B0355: MemberExpr (field access) used as map key must be dup'd — the struct
// still owns the pointer, so the map needs an independent copy.
func TestMapIndexAssignDupBorrowKeyMemberExpr(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		test() {
			Row r = Row(name: "hello");
			map[string, int] m = map[string, int]();
			m[r.name] = 1;
		}
	`)
	assertContains(t, ir, "strdup.copy")
}

// B0355: MemberExpr (field access) used as map value must be dup'd — the struct
// still owns the pointer, so the map needs an independent copy.
func TestMapIndexAssignDupBorrowValueMemberExpr(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		test() {
			Row r = Row(name: "world");
			map[string, string] m = map[string, string]();
			m["k"] = r.name;
		}
	`)
	assertContains(t, ir, "strdup.copy")
}

// T0157: Weak[T] drop function has correct structure: null check, atomic decrement weak_count.
func TestWeakDropFunctionBody(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
			w := a.downgrade();
		}
	`)
	// Weak drop should null-check and decrement weak_count
	assertContains(t, ir, "define void @\"Weak[int].drop\"")
	assertContains(t, ir, "decwc:")
	assertContains(t, ir, "free:")
}

func TestWeakCloneChainFreshSSA(t *testing.T) {
	ir := generateIR(t, `
		main() {
			Ref[int] a = Ref[int](99);
			Weak[int] w = a.downgrade().clone();
		}
	`)
	// Weak clone in a chain must produce fresh SSA value
	assertContains(t, ir, "ptrtoint")
	assertContains(t, ir, "inttoptr")
	assertContains(t, ir, "%w.dropflag")
}

// T0157: Weak[T].clone() atomically increments weak_count.
func TestWeakCloneIR(t *testing.T) {
	ir := generateIR(t, `
		main() {
			a := Ref[int](42);
			w := a.downgrade();
			w2 := w.clone();
		}
	`)
	// Both w and w2 should have drop flags
	assertContains(t, ir, "%w.dropflag")
	assertContains(t, ir, "%w2.dropflag")
	// Both should call Weak[int].drop at scope exit
	assertContains(t, ir, `call void @"Weak[int].drop"(`)
}

// T0157: dupWeak — reading a Weak[T] field from a droppable type increments weak refcount.
func TestDupWeakFieldFromDroppable(t *testing.T) {
	ir := generateIR(t, `
		type Holder {
			Weak[int] w;
			drop(~this) {}
		}
		main() {
			a := Ref[int](42);
			h := Holder(w: a.downgrade());
			Weak[int] copy = h.w;
		}
	`)
	// Should produce weakdup block for weak_count increment (numeric suffix varies)
	assertContainsMatch(t, ir, `weakdup\.inc\.\d+:`)
	assertContainsMatch(t, ir, `weakdup\.merge\.\d+:`)
}

// T0838 regression guard: a handler binding of a HEAP-USER optional field must
// NOT neutralize the owner field — genOptionalHandlerExpr makes an independent
// dup (T0775), so the owner keeps & frees the original and the bound local owns
// the dup. handlerResultIsNativeHandle returns false for heap-user types, so the
// T0775 dup-and-don't-neutralize contract is preserved (no `store i1 false`).
func TestT0838HeapUserHandlerBindingPreservesDup(t *testing.T) {
	ir := generateIR(t, `
		type Payload { int v; drop(~this) {} }
		type PayloadHolder { Payload? p; drop(~this) {} }
		main() {
			h := PayloadHolder(p: Payload(v: 21));
			Payload got = h.p? _ { Payload(v: 0) };
		}
	`)
	gmain := extractDefine(ir, ".goroutine.main")
	// handlerResultIsNativeHandle is false for heap-user types, so the T0775
	// dup-and-don't-neutralize contract holds: genOptionalHandlerExpr makes an
	// independent dup (the binding's sole owner) and the owner's optional field
	// is NOT neutralized — the owner keeps & frees the original. So there is no
	// GEP-into-optional-struct present-flag clear in the goroutine body.
	assertNotContainsMatch(t, gmain,
		`getelementptr \{ i1, [^}]*\}, \{ i1, [^}]*\}\* %\d+, i32 0, i32 0\s*\n\s*store i1 false`)
}

// T0381: chained `.borrow.field` access dispatches through genMemberExpr's
// SharedRef unwrap — the inner member-access on `T&` looks up the field on
// the underlying `T`. Without the unwrap, the field-resolution path would
// fail to find the field on the SharedRef wrapper.
func TestT0381_ChainedBorrowFieldAccess(t *testing.T) {
	ir := generateIR(t, `
		type Pt { int x; }
		main() {
			a := Ref[Pt](Pt(x: 7));
			x := a.borrow.x;
		}
	`)
	// The Ref[Pt] type and its drop should appear; sema/codegen lowering
	// of `.borrow.x` would fail without the SharedRef unwrap in genMemberExpr
	// because the field 'x' is not present on the SharedRef wrapper itself.
	assertContains(t, ir, "Ref[Pt].drop")
}

// T0381: a `T&`-typed local that is later reassigned to an owned `T`
// must register its drop binding using the underlying owned type — the
// SharedRef strip in maybeRegisterDrop ensures the proper drop function
// is dispatched (e.g., per-element drops for `string[]`).
func TestT0381_BorrowLocalReassignedToOwnedDrops(t *testing.T) {
	ir := generateIR(t, `
		main() {
			v := string[]();
			v.push("hello");
			a := Ref[string[]](v);
			string[]& borrowed = a.borrow;
			borrowed = ["owned"];
		}
	`)
	// The reassignment makes `borrowed` an owned vector; on scope exit
	// we should see a call into Vector.drop (proves maybeRegisterDrop
	// saw past the SharedRef and registered an owned-vector drop).
	assertContains(t, ir, "call void @Vector.drop")
}

// T0411: Constructor field-init that reads a string field from a droppable
// owner (`Type(label: this.label)`) must dup the string so the new instance
// owns an independent copy. Without the dup, both the source's drop and the
// new instance's drop free the same buffer → double-free.
func TestT0411_ConstructorStringFieldFromThisDups(t *testing.T) {
	ir := generateIR(t, `
		type CB {
			string label;
			drop(~this) {}
			clone() CB {
				return CB(label: this.label);
			}
		}
		test_t0411_dup() {
			c := CB(label: "hi");
			c2 := c.clone();
		}
	`)
	cloneFn := extractFunction(ir, "CB.clone")
	if cloneFn == "" {
		t.Fatal("expected CB.clone in IR")
	}
	// The clone body must dup the string when initializing the new CB —
	// i.e., a call to promise_string_new must appear inside CB.clone.
	assertContains(t, cloneFn, "call i8* @promise_string_new(")
}

// T0411: Vector field auto-dup via constructor field-init from `this.field`.
// Mirrors TestT0411_ConstructorStringFieldFromThisDups but for the
// dupContainerFieldAccess path on a Vector field.
func TestT0411_ConstructorVectorFieldFromThisDups(t *testing.T) {
	ir := generateIR(t, `
		type V {
			int[] items;
			drop(~this) {}
			clone() V {
				return V(items: this.items);
			}
		}
		test_t0411_vec_dup() {
			a := V(items: [1, 2, 3]);
			b := a.clone();
		}
	`)
	cloneFn := extractFunction(ir, "V.clone")
	if cloneFn == "" {
		t.Fatal("expected V.clone in IR")
	}
	// dupContainerFieldAccess for a Vector field routes through dupVector,
	// which emits a `vecdup.copy` block label. Without the T0411 fix, the
	// field would be a direct store with no dup logic.
	assertContains(t, cloneFn, "vecdup.copy")
}

// T0847: Constructor field-init that reads a Vector element directly into an
// owning (non-borrow) field slot (`Holder(held: v[0])`) must dup-on-read.
// Without the dup, the element pointer is aliased into the new instance's
// owning field — both v (element walk in Vector.drop) and the holder (synth
// field drop) free the same instance → double-free SEGV. Mirrors T0403 (the
// ~-param call-arg path); maybeEnableDupForConstructorArg's IndexExpr branch
// sets dupHeapUserFieldAccess, consumed by genVectorIndex → cloneHeapElement.
func TestT0847_ConstructorVectorElementDups(t *testing.T) {
	ir := generateIR(t, `
		type Item { string label; drop(~this) {} }
		type Holder { Item held; drop(~this) {} }
		test_t0847() {
			v := Item[]();
			v.push(Item(label: "x"));
			h := Holder(held: v[0]);
		}
	`)
	// cloneHeapElement → dupHeapValue: allocate a new instance and memcpy the
	// data. Without the fix the element would be a direct aliasing store.
	assertContains(t, ir, "call i8* @pal_alloc")
	assertContains(t, ir, "call void @llvm.memcpy")
}

// T0847 (paren variant): a parenthesized container-element ctor arg
// `Holder(held: (v[0]))` must peel the ParenExpr to reach the IndexExpr and
// still dup-on-read. Exercises maybeEnableDupForConstructorArg's ParenExpr peel.
func TestT0847_ConstructorParenVectorElementDups(t *testing.T) {
	ir := generateIR(t, `
		type Item { string label; drop(~this) {} }
		type Holder { Item held; drop(~this) {} }
		test_t0847_paren() {
			v := Item[]();
			v.push(Item(label: "x"));
			h := Holder(held: (v[0]));
		}
	`)
	// Paren peel reaches the IndexExpr → dup-on-read fires (allocate + memcpy).
	assertContains(t, ir, "call i8* @pal_alloc")
	assertContains(t, ir, "call void @llvm.memcpy")
}

// --- T1031: aliasing a borrowed-return into a new owner dups at the call site -

// T1031: `Node b = ident(a)` where `ident(Node n) Node { return n; }` returns its
// by-value borrow param aliases a's heap instance. Because a remains owned (it is
// co-dropped at scope exit), the new owner b must receive an INDEPENDENT
// allocation — otherwise both drop the shared instance (double-free / UAF). The
// fix clones into the source local's storage at the call site, gated on a runtime
// `retPtr == argPtr` alias check. The callee itself returns the bare alias (no
// clone), so functions that relocate their param before returning (sort's COW)
// stay untouched.
func TestReturnBorrowParamDupsHeapUserType(t *testing.T) {
	ir := generateIR(t, `
		type Node { int v; drop(~this){} }
		ident(Node n) Node { return n; }
		run() Node {
			Node a = Node(v: 1);
			Node b = ident(a);
			return b;
		}
	`)
	// The callee returns the bare alias — no clone.
	callee := extractDefine(ir, "__user.ident")
	if callee == "" {
		t.Fatalf("ident callee not found in IR:\n%s", ir)
	}
	assertNotContains(t, callee, "heapdup.copy")
	// The caller clones into the source's storage under a runtime alias guard.
	caller := extractDefine(ir, "__user.run")
	if caller == "" {
		t.Fatalf("run caller not found in IR:\n%s", ir)
	}
	assertContains(t, caller, "alias.dup")
	assertContains(t, caller, "heapdup.copy")
	assertContains(t, caller, "@pal_alloc")
}

// T1031: a returned by-value string param is deep-copied at the call site.
func TestReturnBorrowParamDupsString(t *testing.T) {
	ir := generateIR(t, `
		ident(string s) string { return s; }
		run() string {
			string a = "x".repeat(2);
			string b = ident(a);
			return b;
		}
	`)
	callee := extractDefine(ir, "__user.ident")
	if callee == "" {
		t.Fatalf("ident callee not found in IR:\n%s", ir)
	}
	assertNotContains(t, callee, "strdup.copy")
	caller := extractDefine(ir, "__user.run")
	if caller == "" {
		t.Fatalf("run caller not found in IR:\n%s", ir)
	}
	assertContains(t, caller, "alias.dup")
	assertContains(t, caller, "strdup.copy")
}

// T1031: a returned by-value vector param is deep-copied at the call site.
func TestReturnBorrowParamDupsVector(t *testing.T) {
	ir := generateIR(t, `
		ident(int[] v) int[] { return v; }
		run() int[] {
			int[] a = [];
			int[] b = ident(a);
			return b;
		}
	`)
	caller := extractDefine(ir, "__user.run")
	if caller == "" {
		t.Fatalf("run caller not found in IR:\n%s", ir)
	}
	assertContains(t, caller, "alias.dup")
	assertContains(t, caller, "vecdup.copy")
}

// T1031: a moved (`move`) param is owned by the callee — the call site must NOT
// emit the aliasing clone (the source is consumed, not co-owned).
func TestReturnMovedParamNoDup(t *testing.T) {
	ir := generateIR(t, `
		type Node { int v; drop(~this){} }
		consume(Node move n) Node { return n; }
		run() Node {
			Node a = Node(v: 1);
			Node b = consume(move a);
			return b;
		}
	`)
	caller := extractDefine(ir, "__user.run")
	if caller == "" {
		t.Fatalf("run caller not found in IR:\n%s", ir)
	}
	assertNotContains(t, caller, "alias.dup")
}

// T1031/T1017: a DISCARDED call whose heap-user-type result aliases a still-live
// local takes the discard path (clearDiscardedAliasTempFlag), NOT the assignment
// clone path. Heap user-type results are tracked as heapTemps keyed by an
// extractvalue SSA value distinct from the freshly-extracted retPtr, so the
// function must scan the tracked heap temps (loading each temp's stored instance
// pointer) and clear the matching temp's flag — keeping the live local the sole
// owner, dropped once at scope exit. This exercises the heap-temp scan branch
// (the vector/stmtTemp discard case in TestT1017DiscardedAliasClearsResultTemp
// returns earlier via the direct stmtTempMap lookup).
func TestReturnBorrowParamDiscardedHeapUserTypeScansHeapTemps(t *testing.T) {
	ir := generateIR(t, `
		type Node { int v; drop(~this){} }
		ident(Node n) Node { return n; }
		run() int {
			Node n = Node(v: 11);
			ident(n);
			return n.v;
		}
	`)
	caller := extractDefine(ir, "__user.run")
	if caller == "" {
		t.Fatalf("run caller not found in IR:\n%s", ir)
	}
	// Discard path (not the assignment clone path).
	assertContains(t, caller, "alias.discard.clear")
	assertNotContains(t, caller, "alias.dup")
}
