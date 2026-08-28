package drop1

import (
	"strings"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
)

// --- Variable tests ---

func TestVariableAllocaAndLoad(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int x = 10;
			int y = x;
		}
	`)
	codegentest.AssertContains(t, ir, "alloca i64")
	codegentest.AssertContains(t, ir, "store i64 10")
	codegentest.AssertContains(t, ir, "load i64")
}

func TestStringEscapeCloseBrace(t *testing.T) {
	// B0124: \} should resolve to literal }
	ir := codegentest.GenerateIR(t, `main() { s := "a\}b"; }`)
	codegentest.AssertContains(t, ir, `c"a}b"`)
}

func TestStringEscapeCloseBraceOnly(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { s := "\}"; }`)
	codegentest.AssertContains(t, ir, `c"}"`)
}

func TestStringDropFuncBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { s := "x"; }`)
	// T0093: promise_string_drop null-checks the pointer (for null fields in
	// synthesized drops), then checks bit 63 (literal flag), then conditionally frees
	codegentest.AssertContains(t, ir, "define void @promise_string_drop(i8* %ptr)")
	codegentest.AssertContains(t, ir, "icmp eq i8* %ptr, null")
	codegentest.AssertContains(t, ir, "icmp ne i64")
	codegentest.AssertContains(t, ir, "call void @pal_free(")
}

// T0061: String drop binding is registered at scope exit
func TestStringDropScopeBinding(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			s := "hello";
		}
	`)
	// String drop binding: drop flag alloca, conditional call to promise_string_drop
	codegentest.AssertContains(t, ir, "strdrop.call")
	codegentest.AssertContains(t, ir, "strdrop.skip")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop(")
}

// T0061: String drop flag is cleared when returning a string
func TestStringDropReturnClearsFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make_name() string {
			s := "bob";
			return s;
		}
		main() { make_name(); }
	`)
	// The return should clear the drop flag (store i1 false) before scope cleanup
	codegentest.AssertContains(t, ir, "strdrop.skip")
}

// T0061: String drop flag IS cleared when passing to a function (same as user types)
func TestStringDropClearedOnFuncArg(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		consume(string s) {}
		main() {
			s := "hello";
			consume(s);
		}
	`)
	// The string drop binding exists but flag is cleared at the call site,
	// so the conditional drop at scope exit is a no-op (skips).
	codegentest.AssertContains(t, ir, "strdrop.call")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop(")
}

// T0061: String drop flag is cleared on assignment (move)
func TestStringDropClearedOnAssignment(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			a := "hello";
			b := a;
		}
	`)
	// Both a and b should have drop bindings
	// a's flag should be cleared (moved to b)
	codegentest.AssertContains(t, ir, "strdrop.call")
}

// T0061: String borrowed from struct field should NOT have active drop
func TestStringDropBorrowFromField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Person { string name; }
		main() {
			p := Person(name: "alice");
			field_val := p.name;
		}
	`)
	// field_val gets a drop binding but flag is immediately cleared (borrow from field)
	codegentest.AssertContains(t, ir, "strdrop.call")
}

// T0061: String borrowed from vector index should NOT have active drop
func TestStringDropBorrowFromIndex(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			names := string[]();
			names.push("alice");
			elem := names[0];
		}
	`)
	// elem gets a drop binding but flag is immediately cleared (borrow from vector)
	codegentest.AssertContains(t, ir, "strdrop.call")
}

// T0064: Vector drop binding is registered at scope exit
func TestVectorDropScopeBinding(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			v := int[]();
			v.push(1);
		}
	`)
	// Vector drop binding: strdrop.call block (reuses bindingDropString mechanism)
	codegentest.AssertContains(t, ir, "strdrop.call")
	codegentest.AssertContains(t, ir, "call void @Vector.drop(")
}

// B0223: Vector slice intermediate in from_bytes must be tracked as a heap temp
// and dropped at statement end (via Vector.drop).
func TestVectorSliceTempDroppedInFromBytes(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		take(string s) {}
		main() {
			u8[] buf = u8[].filled(65u8, 10);
			take(string.from_bytes(buf[0:5]));
		}
	`)
	// The vector slice result should be dropped via Vector.drop at statement end
	codegentest.AssertContains(t, ir, "call void @Vector.drop(")
}

func TestPalAllocDefined(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { x := 42; }`)
	codegentest.AssertContains(t, ir, "declare noalias i8* @malloc(i64 noundef %size) nounwind willreturn")
	codegentest.AssertContains(t, ir, "@pal_alloc(i64 %size)")
}

func TestDropNullSafe(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "drop.exec")
	codegentest.AssertContains(t, ir, "drop.done")
}

func TestTupleInterpolationTracksTemps(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			(int, int) pair = (1, 2);
			string s = "{pair}";
		}
	`)
	// B0254: convertTupleToString must track per-element convertToString results
	// as string temps so they get freed. Verify promise_string_drop is emitted.
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// T0937: an inline (unbound) elvis whose result is a value-struct container
// (map[K,V] / Set[T] — a 2-word {i8*, i8*} Value struct, not a bare i8*) on an
// owned-local source must register the result as a heap drop temp with a
// per-branch flag: owned (true) on the some-path where the extracted inner is
// orphaned, borrowed (false) on the none-path where the default keeps its owner.
// Without this the some-path inner leaks (the i8*-only trackElvisResultTemp path
// skips value structs).
func TestElvisMapResultDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			map[string, int]? a = {"x": 1};
			map[string, int] b = {"z": 9};
			c := (a ?: b).len;
		}
	`)
	codegentest.AssertContains(t, ir, "elvis.merge")
	// Per-branch live flag: true on the some-path, false on the none-path.
	codegentest.AssertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
	// The tracked heap temp dispatches to the synthesized Map drop.
	codegentest.AssertContains(t, ir, `call void @"Map[string, int].drop"`)
}

// T0937: a member-source elvis (`bx.m ?: b`) is owner-governed — the extracted
// inner aliases the owned field and the container's own drop frees it. The elvis
// result must NOT be tracked (tracking would double-free), so no per-branch
// owned-flag phi is emitted.
func TestElvisMapMemberSourceNoDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MBox { map[string, int]? m; }
		main() {
			MBox bx = MBox(m: {"x": 1});
			map[string, int] b = {"z": 9};
			c := (bx.m ?: b).len;
		}
	`)
	codegentest.AssertContains(t, ir, "elvis.merge")
	codegentest.AssertNotContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
}

// T0937 (i8*-container gap): the i8* result path (trackElvisResultTemp, T0935)
// applies the same orphan classifier as the value-struct path. An owned-local
// string[] source is orphaned on the some-path → tracked with a per-branch flag.
// Here the none-path default `b` is ALSO an owned local, so T0936 neutralizes its
// scope-exit owner and the result owns it on the none-path too — the flag phi is
// [true, true] (the `true` on the some-incoming is what proves orphan tracking).
func TestElvisStrvecOwnedLocalDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string[]? a = ["x" + "y"];
			string[] b = ["z" + "w"];
			c := (a ?: b).len;
		}
	`)
	codegentest.AssertContains(t, ir, "elvis.merge")
	// Per-branch live flag drives the stmt-temp Vector.drop dispatch: owned on the
	// some-path (orphaned inner) and on the none-path (neutralized local default).
	codegentest.AssertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ true, %elvis\.none`)
}

// T0937 (i8*-container gap): a member-source string[] elvis (`bx.v ?: b`) aliases
// the owned field; the container's drop frees it. The inline result must NOT be
// tracked (tracking double-freed the elements → use-after-free crash before the
// gate was applied to the i8* path), so no per-branch flag phi is emitted.
func TestElvisStrvecMemberSourceNoDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type SVBox { string[]? v; }
		main() {
			SVBox bx = SVBox(v: ["x" + "y"]);
			string[] b = ["z" + "w"];
			c := (bx.v ?: b).len;
		}
	`)
	codegentest.AssertContains(t, ir, "elvis.merge")
	codegentest.AssertNotContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
}

// T0937: an index-source elvis (`v[i] ?: b`) is owner-governed — the extracted
// inner aliases the container element, which the container's own drop frees. The
// result must NOT be tracked (tracking would double-free), so no per-branch
// owned-flag phi is emitted. Exercises the IndexExpr arm of
// elvisSomeInnerOrphaned (the ident/member tests cover the other arms).
func TestElvisMapIndexSourceNoDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			(map[string, int]?)[] v = [];
			v.push({"x": 1});
			map[string, int] b = {"z": 9};
			c := (v[0] ?: b).len;
		}
	`)
	codegentest.AssertContains(t, ir, "elvis.merge")
	codegentest.AssertNotContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
}

// T0937 (T0924 subsumption): a heap *user* type (a droppable non-value type, also
// a 2-word {i8*, i8*} Value struct) used inline on an owned-local source goes
// through the SAME trackElvisResultHeap path as Map/Set — distinct from the i8*
// container path. It must emit the per-branch owned-flag phi and dispatch the
// tracked temp to the type's own synthesized drop (@HVal.drop). This proves the
// heap-user representation arm fires (the Map tests only cover the container arm
// of the shared function); without it the some-path inner leaks (T0924).
func TestElvisHeapUserResultDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type HVal { string s; }
		main() {
			HVal? a = HVal(s: "a" + "b");
			HVal b = HVal(s: "c" + "d");
			c := (a ?: b).s.len;
		}
	`)
	codegentest.AssertContains(t, ir, "elvis.merge")
	// Per-branch live flag: owned (true) on some, borrowed (false) on none.
	codegentest.AssertContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
	// The tracked heap temp dispatches to the user type's own drop.
	codegentest.AssertContains(t, ir, "call void @HVal.drop")
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
	ir := codegentest.GenerateIR(t, `
		type HVal { string s; }
		f(HVal? a, HVal b) { m := a ?: b; }
		main() { HVal? a = HVal(s: "a" + "b"); HVal c = HVal(s: "c" + "d"); f(a, c); }
	`)
	// The bound override phi borrows on both paths (both operands caller-owned).
	codegentest.AssertContainsMatch(t, ir, `phi i1 \[ false, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
	// The override store follows maybeRegisterDrop's unconditional 1.
	codegentest.AssertContains(t, ir, "store i1 true, i1* %m.dropflag")
	codegentest.AssertContainsMatch(t, ir, `store i1 %[0-9]+, i1\* %m\.dropflag`)
}

// T0940: a STRING elvis bound result (i8* container) is covered by the per-path bound
// flag too — elvisResultDrop classifies string (with Vector) as the vecOrStr arm, so
// `m := a ?: b` with both operands borrowed params gets a phi[false, false] override of
// maybeRegisterDrop's unconditional `store i1 true`. Pre-T0940 the string-bound case
// kept the unconditional owning drop and freed the caller-owned default a second time
// (`@promise_string_drop` on a borrowed buffer → UAF). The bound flag makes `m` borrow
// on both paths, matching the Map/heap-user borrowed-param arm.
func TestElvisBoundStringBorrowsBoth(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		f(string? a, string b) { m := a ?: b; }
		main() { string? a = "x"; string b = "y"; f(a, b); }
	`)
	// Borrows on both paths (both operands caller-owned); never the old owning shapes.
	codegentest.AssertContainsMatch(t, ir, `phi i1 \[ false, %elvis\.some[^]]*\], \[ false, %elvis\.none`)
	codegentest.AssertNotContainsMatch(t, ir, `phi i1 \[ false, %elvis\.some[^]]*\], \[ true, %elvis\.none`)
	codegentest.AssertNotContainsMatch(t, ir, `phi i1 \[ true, %elvis\.some[^]]*\], \[ true, %elvis\.none`)
	// The per-path flag overrides the unconditional owning drop.
	codegentest.AssertContains(t, ir, "store i1 true, i1* %m.dropflag")
	codegentest.AssertContainsMatch(t, ir, `store i1 %[0-9]+, i1\* %m\.dropflag`)
}

func TestStaticVectorDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { x := [1, 2, 3]; }`)
	// Drop function should check bit 63 before freeing
	codegentest.AssertContains(t, ir, "define void @Vector.drop(")
	codegentest.AssertContains(t, ir, "check_static")
}

// T0579: Array field with heap user type that has no explicit drop —
// exercises the "heap user without explicit drop" branch in
// `typeNeedsFieldDrop` (must return true so the per-element `pal_free` fires).
func TestFixedArrayFieldHeapNoDropElement(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _Bare { int x; }
		type _BareArr { _Bare[2] data; }
		main() {
			b := _BareArr(data: [_Bare(x: 1), _Bare(x: 2)]);
		}
	`)
	// The element type has no drop method, so the synth drop must fall back to
	// pal_free per element.
	codegentest.AssertContains(t, ir, "call void @pal_free")
}

// T0583: `arr[i] = newVal` on a fixed-size array of droppable elements must
// emit a drop call for the old slot value before storing the new one.
// Before the fix, `genArrayIndexAssign` did a bare `NewStore` and leaked the
// previous allocation. The IR for a heap-user element with explicit drop must
// load the old element, drop it (Type.drop + pal_free), then store the new.
func TestFixedArrayIndexAssignDropsOldHeapUser(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _Box { int n; drop(~this) {} }
		main() {
			_Box[2] arr = [_Box(n: 1), _Box(n: 2)];
			_Box c = _Box(n: 3);
			arr[1] = c;
		}
	`)
	// After bounds-check OK, the IR must drop the previous element before storing.
	codegentest.AssertContains(t, ir, "arrassign.ok")
	codegentest.AssertContains(t, ir, "call void @_Box.drop")
}

// T0583: String element — overwrite must call promise_string_drop on the old slot.
func TestFixedArrayIndexAssignDropsOldString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string[2] arr = ["alpha", "beta"];
			arr[1] = "gamma";
		}
	`)
	codegentest.AssertContains(t, ir, "arrassign.ok")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// T0583: String compound assignment — `arr[i] += s` must drop the old string
// after computing the concat result and before storing it.
func TestFixedArrayIndexCompoundAssignDropsOldString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string[2] arr = ["foo", "bar"];
			arr[1] += "baz";
		}
	`)
	codegentest.AssertContains(t, ir, "arrassign.ok")
	// Compound path goes through emitStringDropOldValue.
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// T0583: Vector element — overwrite must call Vector.drop on the old slot.
func TestFixedArrayIndexAssignDropsOldVector(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "arrassign.ok")
	codegentest.AssertContains(t, ir, "call void @Vector.drop")
}

// T0583: Primitive element (no drop needed) — `arr[i] = val` must NOT emit a
// load-and-drop dance for the old slot. Confirms typeNeedsFieldDrop correctly
// gates the new code path: for ints, the arrassign.ok block goes directly from
// GEP to store without any intervening load of the old value.
func TestFixedArrayIndexAssignNoDropForPrimitive(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int[3] arr = [1, 2, 3];
			arr[1] = 42;
		}
	`)
	codegentest.AssertContains(t, ir, "arrassign.ok")
	// For primitives the codegen emits: GEP into [3 x i64] then `store i64 42`
	// immediately. No old-value load or drop call appears between them.
	codegentest.AssertContainsMatch(t, ir,
		`getelementptr \[3 x i64\][^\n]*\n[^\n]*store i64 42`)
}

// T0590: genArrayIndex had no dup-on-read, so any read from a fixed-size array
// slot returned an alias. Combined with T0583's drop-on-overwrite, slot-to-slot
// copies and let-then-X reads on droppable elements produced double-frees.
// Tests below verify each dup branch fires.

func TestFixedArrayIndexDupsString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "arridx.ok")
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new(")
}

func TestFixedArrayIndexDupsHeapUser(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _B { int n; drop(~this) {} }
		main() {
			_B[2] arr = [_B(n: 1), _B(n: 2)];
			_B x = arr[0];
		}
	`)
	codegentest.AssertContains(t, ir, "arridx.ok")
	// dupHeapValue path: pal_alloc + memcpy for the new instance.
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(")
	codegentest.AssertContains(t, ir, "call void @llvm.memcpy")
}

func TestFixedArrayIndexDupsVector(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			v0 := Vector[int]();
			v0.push(1);
			v1 := Vector[int]();
			v1.push(2);
			Vector[int][2] arr = [v0, v1];
			Vector[int] x = arr[0];
		}
	`)
	codegentest.AssertContains(t, ir, "arridx.ok")
	// dupVector: pal_alloc for the new buffer + memcpy from the old one.
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(")
}

func TestFixedArrayIndexNoDupForPrimitive(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int[2] arr = [10, 20];
			int x = arr[0];
		}
	`)
	// Primitives use the bare load — no dup helper. Check the main goroutine
	// specifically (std library code emitted in the same IR may reference
	// promise_string_new for other reasons).
	codegentest.AssertContains(t, ir, "arridx.ok")
	mainIR := codegentest.ExtractFunction(ir, ".goroutine.main")
	if strings.Contains(mainIR, "promise_string_new") || strings.Contains(mainIR, "pal_alloc") {
		t.Errorf("expected main goroutine to have no dup helper for primitive array index, got:\n%s", mainIR)
	}
}

func TestFixedArrayIndexAssignSlotToSlotDupsHeapUser(t *testing.T) {
	// T0590: slot-to-slot copy (`arr[1] = arr[0]`) must dup on RHS read, then
	// drop-on-overwrite frees the previous arr[1] (T0583), then stores the dup.
	ir := codegentest.GenerateIR(t, `
		type _B { int n; drop(~this) {} }
		main() {
			_B[2] arr = [_B(n: 1), _B(n: 2)];
			arr[1] = arr[0];
		}
	`)
	codegentest.AssertContains(t, ir, "arridx.ok")    // RHS read
	codegentest.AssertContains(t, ir, "arrassign.ok") // LHS assign with drop-on-overwrite
	// Must see both: drop-on-overwrite (drop of old arr[1]) and dup (clone of arr[0]).
	codegentest.AssertContains(t, ir, "call void @_B.drop")
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(")
}

// T0802: `s = obj.field` reassignment of a heap string field must clone-on-read,
// exactly like the var-decl path. Before the fix, genAssignStmt only set the
// dup-on-read flags for an IndexExpr RHS, so a MemberExpr (field-access) RHS
// aliased the field pointer into s with s's drop flag set — both s and obj's drop
// then freed the same allocation (double-free, latent on linux / SIGABRT on
// macOS). genFieldAccess emits the clone via dupString (promise_string_new).
func TestReassignStringFieldClones(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	probeIR := codegentest.ExtractFunction(ir, "__user.probe")
	codegentest.AssertContains(t, probeIR, "call i8* @promise_string_new(")
}

// T0802 control: a reassignment whose RHS is NOT a field read must not emit the
// field-clone helper — proves the dup is driven by the MemberExpr RHS, not always.
func TestReassignNonFieldRhsNoClone(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _T { int id; string label; drop(~this) {} }
		probe() {
			_T t = _T(id: 1, label: "lit");
			string s = "";
			s = "other";
		}
		main() { probe(); }
	`)
	probeIR := codegentest.ExtractFunction(ir, "__user.probe")
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
	ir := codegentest.GenerateIR(t, `
		main() {
			(string, int)[2] arr = [("first", 1), ("second", 2)];
			(string, int) t = arr[0];
		}
	`)
	codegentest.AssertContains(t, ir, "arridx.ok")
	// dupTupleValue: emits a per-field dup; for the string sub-field this
	// is promise_string_new. With static-literal flag, copy-on-write may
	// route through promise_vector_cow style — but the dup-on-read path
	// always emits promise_string_new for the inner string.
	codegentest.AssertContains(t, ir, "call i8* @promise_string_new(")
}

func TestFixedArrayIndexDupsHeapUserNoDrop(t *testing.T) {
	// _Bare: heap user with no explicit drop / no synth drop.
	// dup-on-read must still fire via the isHeapUserNoDropPalFree branch
	// (otherwise both slots alias one pal_free'd allocation).
	ir := codegentest.GenerateIR(t, `
		type _Bare { int x; }
		main() {
			_Bare[2] arr = [_Bare(x: 1), _Bare(x: 2)];
			_Bare x = arr[0];
		}
	`)
	codegentest.AssertContains(t, ir, "arridx.ok")
	// dupHeapValue path: pal_alloc + memcpy
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(")
	codegentest.AssertContains(t, ir, "call void @llvm.memcpy")
}

func TestFixedArrayIndexDupsWeak(t *testing.T) {
	// Weak element: dupWeak emits the type-specific Weak clone helper.
	ir := codegentest.GenerateIR(t, `
		main() {
			Ref[int] keep0 = Ref[int](10);
			Ref[int] keep1 = Ref[int](20);
			Weak[int] w0 = keep0.downgrade();
			Weak[int] w1 = keep1.downgrade();
			Weak[int][2] arr = [w0, w1];
			Weak[int] x = arr[0];
		}
	`)
	codegentest.AssertContains(t, ir, "arridx.ok")
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
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		show_str(string s) Showable { return s; }
		type Holder { Showable item; }
		main() { Holder h = Holder(item: show_str("x")); }
	`)
	codegentest.AssertContains(t, ir, "@promise_typeinfo_stringbox")
	codegentest.AssertContains(t, ir, "@__promise_string_box_drop")
	codegentest.AssertContains(t, ir, "bitcast ({ i8*, i8*, i8*, i32, i32 }* @promise_typeinfo_stringbox to i8*)")
}

// T0893: a borrowing method whose body is bare `return this` must clone the
// receiver instance so the returned owned value does not alias the receiver's
// heap allocation (otherwise one binding's scope-drop frees memory the other
// still reads). The clone shows up as a heapdup block in the method body.
func TestReturnThisClonesBorrowedReceiver(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type BB { int v; dup() BB { return this; } }
		main() { d := BB(v: 11); m := d.dup(); }
	`)
	codegentest.AssertContains(t, codegentest.ExtractDefine(ir, "BB.dup"), "heapdup")
}

// T0893: a `~this` (owned/moved-in) receiver returning `this` is a genuine
// ownership transfer — cloning would copy needlessly and leak the moved-in
// instance. The method body must NOT contain a heapdup clone.
func TestReturnThisOwnedReceiverDoesNotClone(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type BB { int v; consume(~this) BB { return this; } }
		main() { d := BB(v: 11); m := d.consume(); }
	`)
	codegentest.AssertNotContains(t, codegentest.ExtractDefine(ir, "BB.consume"), "heapdup")
}

// T0893: a method returning a borrow (`T&`) of `this` hands back a reference into
// existing storage, not an owned copy — it must NOT clone.
func TestReturnThisBorrowReturnDoesNotClone(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type BB { int v; ref() BB& { return this; } }
		main() { d := BB(v: 11); r := d.ref(); }
	`)
	codegentest.AssertNotContains(t, codegentest.ExtractDefine(ir, "BB.ref"), "heapdup")
}

// T0963: an operator returning a borrowed string operand as an owned value must
// clone it (cloneOwnedReturnAlias). Operator dispatch borrows operands, so the
// returned value would otherwise alias the caller's still-live operand — both
// bindings would free / mutate the same string instance. The operator body must
// dup via promise_string_new (dupString).
func TestOperatorReturnsBorrowedStringOperandClones(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type PickSecond { int z; %(string other) string { return other; } }
		main() { s := "x"; p := PickSecond(z: 1); r := p % s; }
	`)
	codegentest.AssertContains(t, codegentest.ExtractDefine(ir, `"PickSecond.%"`), "call i8* @promise_string_new")
}

// T0963: an operator returning a borrow (`string&`) of its operand hands back a
// reference into existing storage, not an owned copy — it must NOT dup.
func TestOperatorReturnsBorrowedStringRefDoesNotClone(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type PickSecond { int z; %(string other) string& { return other; } }
		main() { s := "x"; p := PickSecond(z: 1); r := p % s; }
	`)
	codegentest.AssertNotContains(t, codegentest.ExtractDefine(ir, `"PickSecond.%"`), "call i8* @promise_string_new")
}

// B0250: Assigning the result of a method that returns `this` must clear the
// receiver's drop flag to prevent double-free (both variables share the same instance).
func TestReturnThisClearsReceiverDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper { int value; self() Wrapper { return this; } }
		main() { w := Wrapper(value: 42); w2 := w.self(); }
	`)
	// Should emit a runtime instance-pointer comparison and conditional drop flag clear
	codegentest.AssertContains(t, ir, "return.this.clear")
	codegentest.AssertContains(t, ir, "return.this.skip")
}

// T0347: Chained self-returning calls on a local must walk through the chain
// to find the IdentExpr root and clear its drop flag (not just direct calls).
func TestReturnThisChainedClearsRootDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper { int value; self() Wrapper { return this; } }
		main() { w := Wrapper(value: 1); r := w.self().self(); }
	`)
	// chainOriginExpr should walk past the inner call and reach `w`,
	// emitting the receiver alias-clear blocks for the chained case.
	codegentest.AssertContains(t, ir, "return.this.clear")
	codegentest.AssertContains(t, ir, "return.this.skip")
}

// T0347: `r := this.method()` inside a method must emit a different alias-clear
// path that targets the new binding's drop flag (since `this` itself has no
// drop flag — it's borrowed). Distinct block labels: this.alias.{clear,skip}.
func TestReturnThisRootedClearsBindingDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "this.alias.clear")
	codegentest.AssertContains(t, ir, "this.alias.skip")
}

// T0347: Chained `r := this.iter().iter()` inside a method also emits the
// this-alias clear path (chainOriginExpr walks the chain to ThisExpr root).
func TestReturnThisRootedChainedClearsBindingDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "this.alias.clear")
	codegentest.AssertContains(t, ir, "this.alias.skip")
}

// T0582: paren-wrapped receiver `(w).self()` must walk through chainOriginExpr
// to the IdentExpr root `w` and emit the B0250 receiver alias-clear blocks.
// Without the paren-peel in chainOriginExpr, the chain origin would be a
// ParenExpr and the switch would miss → no alias-clear → runtime double-free.
func TestParenReceiverClearsReceiverDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper { int v; self() Wrapper { return this; } }
		main() { w := Wrapper(v: 1); w2 := (w).self(); }
	`)
	codegentest.AssertContains(t, ir, "return.this.clear")
	codegentest.AssertContains(t, ir, "return.this.skip")
}

// T0582: paren-wrapped receiver under a chain `(w).self().self()` — chain origin
// must still resolve to `w` after the paren-peel.
func TestParenReceiverChainedClearsReceiverDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper { int v; self() Wrapper { return this; } }
		main() { w := Wrapper(v: 1); r := (w).self().self(); }
	`)
	codegentest.AssertContains(t, ir, "return.this.clear")
	codegentest.AssertContains(t, ir, "return.this.skip")
}

// T0582: paren around an inner call `(w.self()).self()` — the outer chain step
// peels the ParenExpr to reach the inner call, then walks back to `w`.
func TestChainedParenInnerCallClearsReceiverDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper { int v; self() Wrapper { return this; } }
		main() { w := Wrapper(v: 1); r := (w.self()).self(); }
	`)
	codegentest.AssertContains(t, ir, "return.this.clear")
	codegentest.AssertContains(t, ir, "return.this.skip")
}

// T1029: a discarded free-function call whose result aliases an owned-local arg
// must clear the RESULT TEMP's drop flag (discard.alias.clear), leaving the source
// local's drop flag armed so the aliased allocation is freed once at scope exit.
// Before the fix the source local's flag was cleared and the temp was freed at
// statement end → the still-live local dangled (use-after-free).
func TestDiscardedAliasArgClearsTempNotSource(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Node { int v; }
		ident_node(Node n) Node { return n; }
		run() int { n := Node(v: 5); ident_node(n); return n.v; }
		main() { x := run(); }
	`)
	body := codegentest.ExtractFunction(ir, "__user.run")
	// The result temp is cleared on the alias path.
	codegentest.AssertContains(t, body, "discard.alias.clear")
	codegentest.AssertContains(t, body, "discard.alias.skip")
	// The source local's drop flag must NOT be cleared inside the discarded call —
	// the local remains the single owner.
	codegentest.AssertNotContains(t, body, "store i1 false, i1* %n.dropflag")
}

// T1029: the i8* result path (vector/string) in genExpr — distinct from the
// heap-user-type path in trackHeapUserTypeResult — must also emit the result-temp
// clear (discard.alias.clear) for a discarded call whose i8* result aliases an
// owned-local arg, so the source local stays the single owner.
func TestDiscardedAliasArgClearsTempI8PtrPath(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		ident_vec(int[] v) int[] { return v; }
		ident_str(string s) string { return s; }
		run_vec() int { xs := [1, 2]; xs.push(3); ident_vec(xs); return xs.len; }
		run_str() int { a := "hi"; s := a + "!"; ident_str(s); return s.len; }
		main() { x := run_vec(); y := run_str(); }
	`)
	vec := codegentest.ExtractFunction(ir, "__user.run_vec")
	codegentest.AssertContains(t, vec, "discard.alias.clear")
	codegentest.AssertContains(t, vec, "discard.alias.skip")
	str := codegentest.ExtractFunction(ir, "__user.run_str")
	codegentest.AssertContains(t, str, "discard.alias.clear")
	codegentest.AssertContains(t, str, "discard.alias.skip")
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
	ir := codegentest.GenerateIR(t, `
		type BB { int v; dup() BB { return this; } }
		main() {
			d := BB(v: 11);
			m := d.dup();
			a := m.v;
			b := d.v;
		}
	`)
	// The B0250 receiver alias-clear must still fire — `m` becomes sole owner.
	codegentest.AssertContains(t, ir, "return.this.clear")
	// No NLL early drop of `m`: emitEarlyDrops would force-clear its flag in the
	// straight-line body after `a := m.v`. Absence proves the suppression (T0891).
	codegentest.AssertNotContains(t, ir, "store i1 false, i1* %m.dropflag")
	// `m` still has a flag-guarded free at scope exit (no leak, exactly one free).
	codegentest.AssertContains(t, ir, "%m.dropflag")
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
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertNotContains(t, ir, "store i1 false, i1* %m.dropflag")
	// `m` still has its flag-guarded scope-exit drop (exactly one free, no leak).
	codegentest.AssertContains(t, ir, "%m.dropflag")
	codegentest.AssertContains(t, ir, `call void @"Mutex[int].drop"`)
}

// B0165: Batch test main waits for worker threads to finish init, then resets
// alloc count to 0 so scheduler allocations don't leak into per-test counts.
func TestBatchTestResetsAllocCount(t *testing.T) {
	result := codegentest.CompileResult(t, `
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
	codegentest.AssertContains(t, ir, "sched_ready_spin")
	codegentest.AssertContains(t, ir, "sched_ready_done")
	// Reset alloc count to 0 via atomic exchange
	codegentest.AssertContains(t, ir, "atomicrmw xchg i64* @__promise_alloc_count, i64 0 monotonic")
}

// B0231/B0315: Batch test leak check uses spin-wait for goroutine drain
// (condvar approach had a lost-wakeup race on ARM64).
func TestBatchTestLeakCheckDrainSpinWait(t *testing.T) {
	result := codegentest.CompileResult(t, `
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
	codegentest.AssertContains(t, ir, "leak_check_myTest")
	codegentest.AssertContains(t, ir, "drain_done_myTest")
	codegentest.AssertContains(t, ir, "drain_slow_myTest")
	codegentest.AssertContains(t, ir, "drain_gs_myTest")
	codegentest.AssertContains(t, ir, "drain_wait_myTest")
	codegentest.AssertContains(t, ir, "drain_nudge_myTest")
	codegentest.AssertContains(t, ir, "drain_sleep_myTest")
	// B0315: wake_m nudge to prevent lost-wakeup race
	codegentest.AssertContains(t, ir, "call void @promise_sched_wake_m()")
	// B0320: both fast-path and slow-path reads should use Acquire ordering
	codegentest.AssertContains(t, ir, "i64 0 acquire")
}

// T0020: Leak detection emits alloc count tracking in pal_alloc/pal_free
// and per-test leak checks in the test main.
func TestLeakDetectionAllocTracking(t *testing.T) {
	result := codegentest.CompileResult(t, `
		myTest() `+"`test"+` { }
	`)
	ir := result.Module.String()

	// pal_alloc should track allocations via __promise_alloc_count
	codegentest.AssertContains(t, ir, "@__promise_alloc_count = global i64 0")
	// pal_alloc atomically increments on successful malloc
	codegentest.AssertContains(t, ir, "atomicrmw add i64* @__promise_alloc_count, i64 1 monotonic")
	// pal_free atomically decrements on non-null free
	codegentest.AssertContains(t, ir, "atomicrmw sub i64* @__promise_alloc_count, i64 1 monotonic")
}

// T0020: Leak detection in test main snapshots alloc count before/after each test.
func TestLeakDetectionInTestMain(t *testing.T) {
	result := codegentest.CompileResult(t, `
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
	codegentest.AssertContains(t, ir, "leak_check_myTest")
	codegentest.AssertContains(t, ir, "print_leak_detail_myTest")
	// Leak message string constants
	codegentest.AssertContains(t, ir, `c"  leak: "`)
	codegentest.AssertContains(t, ir, `c" allocations not freed\0A"`)
	// Leaked counter in summary call
	codegentest.AssertContains(t, ir, "call void @promise_test_summary(i32")
}

// T0067: Tests with allow_leaks don't increment leaked counter.
func TestAllowLeaksDoesNotIncrementLeakedCounter(t *testing.T) {
	src := `myTest() ` + "`test(allow_leaks: true)" + ` { }`
	result := codegentest.CompileResult(t, src)
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
	codegentest.AssertContains(t, ir, "leak_check_myTest")
	codegentest.AssertContains(t, ir, "print_leak_detail_myTest")
	// Should have stale tag warning for allow_leaks
	codegentest.AssertContains(t, ir, "allow_leaks")
	codegentest.AssertContains(t, ir, "tag can be removed")
	// allow_leaks: no_leak_detail block and ignored counter
	codegentest.AssertContains(t, ir, "no_leak_detail_myTest")
	// Summary includes ignored parameter (T0067)
	codegentest.AssertContains(t, ir, "i32 %ignored")
}

// T0067: Tests without allow_leaks increment leaked counter and exit code includes leaks.
func TestNoAllowLeaksIncrementsLeakedCounter(t *testing.T) {
	src := `myTest() ` + "`test" + ` { }`
	result := codegentest.CompileResult(t, src)
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
	codegentest.AssertContains(t, ir, "or i1")
	// Should NOT have stale tag warning (no allow_leaks)
	codegentest.AssertNotContains(t, ir, "tag can be removed")
}

// T0106: emitFieldDrops frees field instances with explicit drop
func TestFieldDropFreesInstance(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "call void @Inner.drop")
	// pal_free should appear in Outer.drop for the Inner field instance
	outerDrop := codegentest.ExtractFunction(ir, "Outer.drop")
	if !strings.Contains(outerDrop, "call void @pal_free(") {
		t.Errorf("Outer.drop should pal_free the Inner field instance\nOuter.drop IR:\n%s", outerDrop)
	}
}

// T0106: String move via IdentExpr propagates ownership at runtime
func TestStringMoveDropFlagPropagation(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			a := "hello" + " world";
			b := a;
		}
	`)
	// a.dropflag should be loaded (saved) before clearing
	// b.dropflag should be set from the saved value (not unconditionally cleared)
	codegentest.AssertContains(t, ir, "a.dropflag")
	codegentest.AssertContains(t, ir, "b.dropflag")
	// Both should have string drop calls (conditional on flags)
	codegentest.AssertContains(t, ir, "promise_string_drop")
}

// T0135: Constructor allocation tracked as heap temp for auto-propagation cleanup
func TestConstructorAllocHeapTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		fail!(string s) int {
			raise error(message: s);
		}
		type H { int v; }
		process!() {
			h := H(v: fail("x"));
		}
	`)
	// Constructor allocation should be tracked and freed on error path
	codegentest.AssertContains(t, ir, "auto.propagate")
	codegentest.AssertContains(t, ir, "err.heap.drop")
}

// --- Drop method tests ---

// Basic: drop() called at scope exit
func TestDropBasicScopeExit(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			r := Resource(id: 1);
			int x = r.id;
		}
	`)
	codegentest.AssertContains(t, ir, "call void @Resource.drop")
	codegentest.AssertContains(t, ir, "r.dropflag")
}

// B0159: Explicit drop methods auto-free instance memory via pal_free
func TestDropExplicitFreesInstance(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		main() {
			r := Resource(id: 1);
		}
	`)
	// The drop function body should end with pal_free to free the instance struct
	codegentest.AssertContains(t, ir, "define void @Resource.drop")
	codegentest.AssertContains(t, ir, "call void @pal_free(i8* %this)")
}

// Move to function arg clears drop flag, adds condBr
func TestDropNotCalledWhenMoved(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "store i1 false, i1*")
	codegentest.AssertContains(t, ir, "drop.call")
	codegentest.AssertContains(t, ir, "drop.skip")
}

// Return triggers drop before ret
func TestDropWithReturn(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "call void @Resource.drop")
	codegentest.AssertContains(t, ir, "define i64 @__user.make")
}

// Mixed use + drop bindings both fire
func TestDropAndUseOrdering(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "call void @Closeable.close")
	codegentest.AssertContains(t, ir, "call void @Droppable.drop")
}

// Nested type: outer drop() triggers field drops
func TestDropFieldAutoCleanup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "call void @Outer.drop")
	codegentest.AssertContains(t, ir, "call void @Inner.drop")
}

// Returning a droppable variable clears its flag
func TestDropReturnMoveClearsFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "store i1 false, i1*")
}

// Conditional move: moved in if-then only → drop flag condBr after merge
func TestDropConditionalMove(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "store i1 false, i1*")
	// Conditional drop at scope exit (flag may be true or false)
	codegentest.AssertContains(t, ir, "drop.call")
	codegentest.AssertContains(t, ir, "drop.skip")
}

// Conditional move with else: moved in both branches → flag cleared in both
func TestDropConditionalMoveBothBranches(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "r1.dropflag")
	codegentest.AssertContains(t, ir, "r2.dropflag")
	// Two drop calls (one for inner, one for outer)
	count := strings.Count(ir, "call void @Resource.drop")
	if count < 2 {
		t.Errorf("expected at least 2 drop calls (inner + outer scope), got %d\nIR:\n%s", count, ir)
	}
}

// While loop: droppable var inside loop body should be dropped per iteration
func TestDropInWhileLoop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "call void @Resource.drop")
	codegentest.AssertContains(t, ir, "r.dropflag")
}

// Infinite loop with break: drop cleanup happens at break
func TestDropInLoopWithBreak(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "call void @Resource.drop")
}

// Loop with continue: drop fires at end of iteration and at continue
func TestDropInLoopWithContinue(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "call void @Resource.drop")
}

// Move into method call clears drop flag
func TestDropMoveToMethodCall(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "store i1 false, i1*")
	codegentest.AssertContains(t, ir, "drop.call")
	codegentest.AssertContains(t, ir, "drop.skip")
}

// Move into constructor field clears drop flag
func TestDropMoveToConstructor(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "store i1 false, i1*")
}

// Move into ident assignment clears drop flag
func TestDropMoveToIdentAssign(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "store i1 false, i1*")
}

// Move into member assignment clears drop flag (bug #2 fix)
func TestDropMoveToMemberAssign(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "store i1 false, i1*")
}

// Multiple droppable vars: each gets its own flag and cleanup
func TestDropMultipleVariables(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "a.dropflag")
	codegentest.AssertContains(t, ir, "b.dropflag")
	codegentest.AssertContains(t, ir, "c.dropflag")
	count := strings.Count(ir, "call void @Resource.drop")
	if count < 3 {
		t.Errorf("expected at least 3 drop calls, got %d\nIR:\n%s", count, ir)
	}
}

// Multiple droppable fields: all cleaned up after user drop() body
func TestDropMultipleFieldsAutoCleanup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "call void @Connection.drop")
	// FileHandle.drop should be called for both fields inside Connection.drop
	count := strings.Count(ir, "call void @FileHandle.drop")
	if count < 2 {
		t.Errorf("expected at least 2 FileHandle.drop calls (one per field), got %d\nIR:\n%s", count, ir)
	}
}

// Vector field drop: types with Vector fields should emit Vector.drop (B0157)
