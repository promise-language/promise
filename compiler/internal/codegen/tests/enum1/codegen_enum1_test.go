package enum1

import (
	"bytes"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen"
	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// === Enum Tests ===

func TestEnumLayout(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color { Red, Green, Blue }
		main() { }
	`)
	// Four-struct layout for enum
	codegentest.AssertContains(t, ir, "%promise_Color_t = type {}")
	codegentest.AssertContains(t, ir, "%promise_Color_m = type { %promise_Color_t* }")
	codegentest.AssertContains(t, ir, "%promise_Color_i = type { %promise_Color_m* }")
	codegentest.AssertContains(t, ir, "%promise_Color_v = type { i8*, %promise_Color_i*, i32 }")
}

func TestEnumLayoutData(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		main() { }
	`)
	codegentest.AssertContains(t, ir, "%promise_Shape_t = type {}")
	codegentest.AssertContains(t, ir, "%promise_Shape_m = type { %promise_Shape_t* }")
	codegentest.AssertContains(t, ir, "%promise_Shape_i = type { %promise_Shape_m* }")
	// Value struct: vtable, instance ptr, tag, data bytes
	codegentest.AssertContains(t, ir, "%promise_Shape_v = type { i8*, %promise_Shape_i*, i32,")
	// Internal struct: tag + data area
	codegentest.AssertContains(t, ir, "%promise_Shape_enum = type { i32,")
}

func TestEnumFieldlessVariant(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Green;
		}
		main() { }
	`)
	// Green is tag 1
	codegentest.AssertContains(t, ir, "store i32 1")
}

func TestEnumDataConstructor(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		test() {
			Shape s = Shape.Circle(3.14);
		}
		main() { }
	`)
	// Should store tag (Circle = 0)
	codegentest.AssertContains(t, ir, "store i32 0")
	// Should store double field via GEP + bitcast
	codegentest.AssertContains(t, ir, "store double")
	codegentest.AssertContains(t, ir, "bitcast")
}

func TestEnumMatchFieldless(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Red;
			x := match c {
				Color.Red => 1,
				Color.Green => 2,
				Color.Blue => 3,
			};
		}
		main() { }
	`)
	// Should use switch on i32 tag
	codegentest.AssertContains(t, ir, "switch i32")
	// Should have arm blocks
	codegentest.AssertContains(t, ir, "match.arm0")
	codegentest.AssertContains(t, ir, "match.arm1")
	codegentest.AssertContains(t, ir, "match.arm2")
	// Merge block with phi
	codegentest.AssertContains(t, ir, "match.end")
}

func TestEnumMatchDestructure(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		test() f64 {
			Shape s = Shape.Circle(3.14);
			return match s {
				Shape.Circle(r) => r,
				Shape.Rect(w, h) => w,
			};
		}
		main() { }
	`)
	// Should switch on tag
	codegentest.AssertContains(t, ir, "switch i32")
	// Should bitcast + GEP to load variant data
	codegentest.AssertContains(t, ir, "bitcast")
	codegentest.AssertContains(t, ir, "load double")
}

func TestEnumMatchShortDestructure(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Result { Ok(int value), Err(int code) }
		test() int {
			Result r = Result.Ok(42);
			return match r {
				Ok(v) => v,
				Err(c) => c,
			};
		}
		main() { }
	`)
	// Short destructure should also produce switch
	codegentest.AssertContains(t, ir, "switch i32")
	codegentest.AssertContains(t, ir, "match.arm0")
	codegentest.AssertContains(t, ir, "match.arm1")
}

func TestEnumMatchWildcard(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color { Red, Green, Blue }
		test() int {
			Color c = Color.Red;
			return match c {
				Color.Red => 1,
				_ => 0,
			};
		}
		main() { }
	`)
	// Switch with default case
	codegentest.AssertContains(t, ir, "switch i32")
	codegentest.AssertContains(t, ir, "match.arm0")
	codegentest.AssertContains(t, ir, "match.arm1")
}

func TestEnumMatchNameBinding(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color { Red, Green, Blue }
		test() int {
			Color c = Color.Red;
			return match c {
				Color.Red => 1,
				val => 0,
			};
		}
		main() { }
	`)
	// Name binding should create alloca for the bound variable
	codegentest.AssertContains(t, ir, "switch i32")
	codegentest.AssertContains(t, ir, "alloca i32")
}

// T1155: a match-arm pattern binding that reuses the scrutinee's name must be
// scoped to the arm only. Before the fix, codegen left c.locals[scrutinee]
// pointing at the destructured (string) alloca, so a later `match` on the same
// name evaluated its subject against the wrong alloca and emitted garbage /
// self-recursive control flow → runtime stack overflow. The correct IR loads the
// enum subject from the parameter alloca (%b.addr) for BOTH matches.
func TestEnumMatchScrutineeShadow(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Msg { Text(string body), Empty }
		f(Msg b) int {
			int la = match b { Msg.Text(b) => b.len, Msg.Empty => 0 };
			int lb = match b { Msg.Text(s) => s.len, Msg.Empty => 0 };
			return la + lb;
		}
		main() { Msg m = Msg.Text("ab"); int x = f(m); }
	`)
	fn := ir[strings.Index(ir, "define i64 @__user.f("):]
	fn = fn[:strings.Index(fn, "\n}")]
	// Both matches must load the enum subject from the param alloca %b.addr —
	// the arm binding `Msg.Text(b)` in the first match must not leak and replace
	// the scrutinee for the second match.
	if got := strings.Count(fn, "load %promise_Msg_enum, %promise_Msg_enum* %b.addr"); got != 2 {
		t.Fatalf("expected 2 loads of the enum subject from %%b.addr (one per match), got %d\n%s", got, fn)
	}
	// The fix must not introduce a recursive call to f.
	if strings.Contains(fn, "call i64 @__user.f(") {
		t.Fatalf("f must not self-recurse:\n%s", fn)
	}
}

// B0328: Bare variant names in match-as-expression on enum subject must resolve
// to EnumVariantMatchPattern, not NameMatchPattern (catch-all binding).
func TestEnumMatchBareVariantNames(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color { Red, Green, Blue }
		test() int {
			Color c = Color.Green;
			return match c {
				Red => 1,
				Green => 2,
				Blue => 3,
			};
		}
		main() { }
	`)
	// Must produce a switch with case labels (not an empty switch)
	codegentest.AssertContains(t, ir, "switch i32")
	codegentest.AssertContains(t, ir, "i32 0, label %match.arm0")
	codegentest.AssertContains(t, ir, "i32 1, label %match.arm1")
	codegentest.AssertContains(t, ir, "i32 2, label %match.arm2")
}

func TestEnumMatchBlock(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Red;
			match c {
				Color.Red => { int x = 1; },
				Color.Green => { int y = 2; },
				Color.Blue => { int z = 3; },
			};
		}
		main() { }
	`)
	// Should have switch and arm blocks (void match, no phi)
	codegentest.AssertContains(t, ir, "switch i32")
	codegentest.AssertContains(t, ir, "match.arm0")
}

func TestMatchIntLiteral(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() int {
			int n = 42;
			return match n {
				1 => 10,
				2 => 20,
				_ => 0,
			};
		}
		main() { }
	`)
	// Should use comparison chain (icmp eq), not switch
	codegentest.AssertContains(t, ir, "icmp eq")
	codegentest.AssertContains(t, ir, "match.arm")
	codegentest.AssertContains(t, ir, "match.next")
}

func TestEnumHeaderFieldless(t *testing.T) {
	result := codegentest.CompileResult(t, `
		enum Color { Red, Green, Blue }
		main() { }
	`)

	var buf bytes.Buffer
	if err := codegen.GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	codegentest.AssertContains(t, header, "promise_Color_t")
	codegentest.AssertContains(t, header, "promise_Color_m")
	codegentest.AssertContains(t, header, "promise_Color_i")
	codegentest.AssertContains(t, header, "promise_Color_v")
	// Value struct should have tag field
	codegentest.AssertContains(t, header, "int32_t")
}

func TestEnumDataFieldlessVariant(t *testing.T) {
	// Exercises zeroinitializer path: fieldless variant in a data enum
	ir := codegentest.GenerateIR(t, `
		enum Result { Ok(int value), None }
		test() {
			Result r = Result.None;
		}
		main() { }
	`)
	// None is tag 1, built via zeroinitializer + insertvalue (not alloca with partial store)
	codegentest.AssertContains(t, ir, "insertvalue %promise_Result_enum zeroinitializer, i32 1, 0")
	// Internal struct should exist for the data enum
	codegentest.AssertContains(t, ir, "%promise_Result_enum = type { i32,")
}

func TestEnumAsFunctionParam(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color { Red, Green, Blue }
		is_red(Color c) bool {
			return match c {
				Color.Red => true,
				_ => false,
			};
		}
		main() { }
	`)
	// Enum param should use i32 (fieldless enum internal type)
	codegentest.AssertContains(t, ir, "define i1 @__user.is_red(i32 %c)")
	// Param should be alloca'd as i32
	codegentest.AssertContains(t, ir, "alloca i32")
	codegentest.AssertContains(t, ir, "switch i32")
}

func TestEnumAsFunctionReturn(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color { Red, Green, Blue }
		get_green() Color {
			return Color.Green;
		}
		main() { }
	`)
	// Enum return should use i32
	codegentest.AssertContains(t, ir, "define i32 @__user.get_green()")
	codegentest.AssertContains(t, ir, "ret i32 1")
}

func TestMatchValueNameBinding(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() int {
			int x = 42;
			return match x {
				val => val + 1,
			};
		}
		main() { }
	`)
	// Name binding in value match: alloca + store the subject
	codegentest.AssertContains(t, ir, "alloca i64")
	codegentest.AssertContains(t, ir, "add i64")
}

func TestEnumDestructureUnderscoreSkip(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Pair { Both(int a, int b) }
		test() int {
			Pair p = Pair.Both(1, 2);
			return match p {
				Both(_, second) => second,
			};
		}
		main() { }
	`)
	// Should still load the second field (index 1) but skip the first
	codegentest.AssertContains(t, ir, "switch i32")
	codegentest.AssertContains(t, ir, "load i64")
}

func TestEnumHeaderData(t *testing.T) {
	result := codegentest.CompileResult(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		main() { }
	`)

	var buf bytes.Buffer
	if err := codegen.GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	codegentest.AssertContains(t, header, "promise_Shape_t")
	codegentest.AssertContains(t, header, "promise_Shape_v")
	// Data enum value struct should have tag and data fields
	codegentest.AssertContains(t, header, "int32_t              tag;")
	codegentest.AssertContains(t, header, "uint8_t              data[16];")
}

func TestTypedHandlerNoMatchPropagation(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type IoError is error { int code; }
		type ParseError is error { int line; }
		fail!() void { raise ParseError(message: "parse", line: 1); }
		handler!() void {
			fail() ? e is IoError { };
		}
		main() { handler() ? e { }; }
	`)
	// Nomatch should propagate (re-wrap error and return)
	codegentest.AssertContains(t, ir, "error.typed.nomatch")
	codegentest.AssertContains(t, ir, "promise_type_is")
}

func TestTupleDestructure(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		pair() (int, int) { return (1, 2); }
		main() { (a, b) := pair(); }
	`)
	// Should use extractvalue to destructure
	codegentest.AssertContains(t, ir, "extractvalue { i64, i64 }")
}

// T0441: 3-element tuple destructure (grammar now accepts N>=2 names).
func TestTupleDestructureThreeElements(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		triple() (int, int, int) { return (1, 2, 3); }
		main() { (a, b, c) := triple(); }
	`)
	codegentest.AssertContains(t, ir, "extractvalue { i64, i64, i64 }")
	codegentest.AssertContains(t, ir, "%a = alloca i64")
	codegentest.AssertContains(t, ir, "%b = alloca i64")
	codegentest.AssertContains(t, ir, "%c = alloca i64")
}

func TestTupleDestructureSkip(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		pair() (int, int) { return (1, 2); }
		main() { (_, b) := pair(); }
	`)
	// Should extract second element but skip first
	codegentest.AssertContains(t, ir, "extractvalue { i64, i64 }")
	// b should be allocated
	codegentest.AssertContains(t, ir, "%b = alloca i64")
}

// T1160: an ENUM receiver. extractNamed yields nil for an enum, so the
// IsStructural/needsVtable guard never runs and the whole classification falls to
// typeMentionsSignature's enum arm, walking the variants' fields. Every other
// receiver test passes a Named. `Choice` holds no closure (fresh result, tracked);
// `Slot` hands back the closure its `Cb` variant owns (not tracked). The enum arm is
// exercised as an argument by TestClosureCallResultNestedAliasNotTracked, never as a
// receiver — an arm that ignored enum receivers would leak the fresh result, and one
// that reported "no closure" would double-free the borrowed one.
func TestClosureEnumReceiverResultTracking(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Choice {
			Left,
			Right,
			mk(this, int x) () -> int { return move || -> x + 1; }
		}
		enum Slot {
			Cb(() -> int f),
			Empty,
			get_cb(this) () -> int {
				match this { Slot.Cb(f) => { return f; }, Slot.Empty => { return || -> 0; }, }
			}
		}
		fresh(Choice c) { c.mk(5); }
		aliased(Slot s) { s.get_cb(); }
		main() {}
	`)
	codegentest.AssertContains(t, codegentest.ExtractFunction(ir, "__user.fresh"), "env.tmp.drop")
	aliased := codegentest.ExtractFunction(ir, "__user.aliased")
	codegentest.AssertContains(t, aliased, "define") // guard: the body was actually found
	codegentest.AssertNotContains(t, aliased, "env.tmp.drop")
}

func TestIsEnumVariant(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Red;
			bool b = c is Red;
		}
		main() { }
	`)
	// Fieldless enum: value IS the tag, compare with icmp eq
	codegentest.AssertContains(t, ir, "icmp eq i32")
}

func TestIsEnumVariantData(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		test() {
			Shape s = Shape.Circle(radius: 3.14);
			bool b = s is Circle;
		}
		main() { }
	`)
	// Data-carrying enum: extract tag from struct, then compare
	codegentest.AssertContains(t, ir, "extractvalue")
	codegentest.AssertContains(t, ir, "icmp eq i32")
}

// B0235: Vector[Enum] index assignment drops old enum element before storing new value.
func TestVectorEnumIndexAssignDropsOld(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Value {
			Null,
			Str(string s),
		}
		main() {
			Value[] v = [Value.Str("hello"), Value.Null];
			v[0] = Value.Null;
		}
	`)
	// Should drop old enum element before storing the new one.
	// The drop is emitted in the indexassign.ok block, before the store.
	codegentest.AssertContains(t, ir, "call void @Value.drop(")
}

// T0741 Part A: an enum variant whose payload is a closure must drop the
// closure's env in the synthesized enum drop. variantFieldNeedsDrop now returns
// true for *types.Signature, so emitVariantFieldDrop's closure case runs.
func TestDropEnumVariantClosurePayload(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Callback {
			holds(() -> int cb),
			empty,
		}
		main() {
			s := "enum" + " payload";
			c := Callback.holds(cb: move || -> s.len);
		}
	`)
	codegentest.AssertContains(t, ir, "define void @Callback.drop")
	codegentest.AssertContains(t, ir, "closure.env.free")
	codegentest.AssertContains(t, ir, "env.deep_drop")
}

// T0741/T0813: dup-ing a closure-containing enum aggregate must NOT shallow-copy
// the closure env (that would alias one env between two droppable owners →
// double-free). emitVariantFieldDup's Signature case nulls the cloned variant's
// closure slot instead, so only the source owns the env. T0813 makes sema reject
// the explicit Vector[Cb].clone() repro outright, so this exercises the null-dup
// via a non-gated implicit dup path (vector slice → emitVectorElementCloneLoop →
// dupEnumElementInPlace → emitVariantFieldDup). Verify the dup path stores a
// zeroed fat pointer rather than copying the source closure value.
func TestDupEnumVariantClosureNullsSlot(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Cb {
			holds(() -> int cb),
			empty,
		}
		main() {
			s := "shared" + " env";
			v := Vector[Cb]();
			v.push(Cb.holds(cb: move || -> s.len));
			v2 := v[:];
		}
	`)
	// The element dup reaches the enum variant dup switch (enumdup.holds), where
	// the cloned variant's {fn,env} closure slot is zero-initialized (nulled),
	// not a copy of the source env — the memory-safe degradation for the
	// non-cloneable closure env.
	codegentest.AssertContains(t, ir, "enumdup.holds")
	codegentest.AssertContains(t, ir, "store { i8*, i8* } zeroinitializer")
}

// T1109: A Weak[T] variant field in a container must dup via dupWeak (atomic
// weak-count increment, emits weakdup.inc), the symmetric sibling of the Arc
// branch. Like Arc, Weak's LLVM value is a bare i8*, so it must NOT reach
// dupHeapValue. Guards the weak branch of emitVariantFieldDup.
func TestDupEnumVariantWeakRefcount(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum WHolder { One(Weak[int] w, int n) }
		main() {
			a := Ref[int](99);
			m := Map[int, WHolder]();
			m[1] = WHolder.One(a.downgrade(), 3);
		}
	`)
	codegentest.AssertContains(t, ir, "enumdup.One")
	codegentest.AssertContains(t, ir, "weakdup.inc")
}

// T0073: Match expression with to_string in an arm — temp claimed by phi
func TestStringTempInMatchArm(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			int n = 7;
			string r = match n {
				1 => "one",
				_ => n.to_string(),
			};
			assert(r == "7", "ok");
		}
		main() {}
	`)
	// Should compile without domination errors; the temp in the _ arm is claimed
	codegentest.AssertContains(t, ir, "r.dropflag")
}

// B0236: Match destructure of droppable enum dups heap user type fields.
func TestMatchDupHeapUserType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper {
			string name;
		}
		enum Container {
			Holding(Wrapper item),
			Empty,
		}
		test() {
			c := Container.Holding(Wrapper(name: "hello"));
			match c {
				Holding(w) => { string s = w.name; },
				Empty => { },
			}
		}
	`)
	// Extracting Wrapper from droppable enum should dup the heap instance
	codegentest.AssertContains(t, ir, "heapdup.copy")
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(")
}

// B0236: Match destructure of droppable enum dups vector fields.
func TestMatchDupVector(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Holder {
			Data(int[] items),
			None,
		}
		test() {
			h := Holder.Data([1, 2, 3]);
			match h {
				Data(v) => { int x = v.len; },
				None => { },
			}
		}
	`)
	// Extracting vector from droppable enum should dup via vecdup
	codegentest.AssertContains(t, ir, "vecdup.copy")
}

// B0284: Map with non-cloneable value types (enum with drops but no `clone`)
// must NOT be cloned via Map.clone() — the clone would be shallow, causing
// double-free when both original and clone drop shared enum heap data.
func TestMatchDupMapNotClonedNonCloneableValues(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum JsonNode {
			Null,
			Text(string value),
			Dict(map[string, JsonNode] fields),
		}
		test() {
			map[string, JsonNode] fields = {"k": JsonNode.Text(value: "v")};
			JsonNode obj = JsonNode.Dict(fields: fields);
			match obj {
				Dict(f) => { int x = f.len; },
				_ => { },
			}
		}
	`)
	// Check that the test function itself does not heap-dup the Map.
	// The "heapdup.copy" block may legitimately appear in other monomorphized
	// functions (e.g., EmbeddedFiles.files vector clone), so only check inside @test.
	//
	// T1129 note: JsonNode now has a synthesized clone, but `match obj` here has an
	// owned *local* subject — its arm bindings BORROW the variant payload (the local's
	// own scope binding drops it once), so no per-binding clone is emitted regardless.
	inTestFunc := false
	for _, line := range strings.Split(ir, "\n") {
		if strings.HasPrefix(line, "define ") {
			inTestFunc = strings.Contains(line, "@test(")
		}
		if inTestFunc && strings.Contains(line, "heapdup.copy") {
			t.Error("Map should not be heap-dup'd in test function (vector of droppable enum elements)")
		}
		if inTestFunc && strings.Contains(line, "Map[string, JsonNode].clone") && strings.Contains(line, "= call") {
			t.Error("match of an owned local subject borrows its bindings — should not call Map.clone()")
		}
	}
}

// B0284: Map with safely cloneable value types (primitives, strings) CAN be
// cloned via Map.clone() — the clone's internal match-dup handles these types.
func TestMatchDupMapClonedSafeValues(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Holder {
			Data(map[string, int] fields),
			Empty,
		}
		test() {
			map[string, int] m = {"a": 1, "b": 2};
			h := Holder.Data(fields: m);
			match h {
				Data(f) => { int x = f.len; },
				Empty => { },
			}
		}
	`)
	// Map[string, int] — both type args are safe (string dup'd, int is primitive)
	codegentest.AssertContains(t, ir, "Map[string, int].clone")
}

// B0284: Map with cloneable enum values CAN be cloned — the enum has `clone`
// so the clone's internal match-dup will deep-copy via enum clone.
func TestMatchDupMapClonedWithCloneableEnumValues(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"enum Status `clone {\n"+
		"  Active(string label),\n"+
		"  Inactive,\n"+
		"}\n"+
		"enum Holder {\n"+
		"  Data(map[string, Status] fields),\n"+
		"  Empty,\n"+
		"}\n"+
		"test() {\n"+
		"  map[string, Status] m = {\"a\": Status.Active(label: \"on\")};\n"+
		"  h := Holder.Data(fields: m);\n"+
		"  match h {\n"+
		"    Data(f) => { int x = f.len; },\n"+
		"    Empty => { },\n"+
		"  }\n"+
		"}\n")
	// Map[string, Status] — Status is `clone so match-dup can handle it
	found := false
	for _, line := range strings.Split(ir, "\n") {
		if strings.Contains(line, "Map[string, Status].clone") && strings.Contains(line, "= call") {
			found = true
			break
		}
	}
	if !found {
		t.Error("Map with cloneable enum values should be cloned via Map.clone()")
	}
}

// B0284: Map with fieldless enum values (no drops) CAN be cloned — bitwise copy
// is safe for enums with no heap data, so typeArgSafeForCloneDup returns true.
func TestMatchDupMapClonedFieldlessEnumValues(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color { Red, Green, Blue, }
		enum Holder {
			Data(map[string, Color] fields),
			Empty,
		}
		test() {
			map[string, Color] m = {"a": Color.Red};
			h := Holder.Data(fields: m);
			match h {
				Data(f) => { int x = f.len; },
				Empty => { },
			}
		}
	`)
	// Map[string, Color] — Color has no drops, bitwise copy is safe
	codegentest.AssertContains(t, ir, "Map[string, Color].clone")
}

// B0244: Match destructure of droppable enum clones enum-typed fields via clone().
func TestMatchDupEnumClone(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"enum Inner `clone {\n"+
		"  Value(string data),\n"+
		"  Empty,\n"+
		"}\n"+
		"enum Outer {\n"+
		"  Holding(Inner item),\n"+
		"  Nothing,\n"+
		"}\n"+
		"test() {\n"+
		"  o := Outer.Holding(item: Inner.Value(data: \"hello\"));\n"+
		"  match o {\n"+
		"    Holding(i) => { },\n"+
		"    Nothing => { },\n"+
		"  }\n"+
		"}\n")
	// Enum field extracted from droppable enum should be cloned via clone method
	codegentest.AssertContains(t, ir, "Inner.clone")
	codegentest.AssertContains(t, ir, "enum.clone.tmp")
}

// B0285: Synthesized enum clone method must NOT double-clone fields.
// The match inside clone() destructures variant fields, and without suppression
// the match-dup mechanism also clones them — causing double work and leaked
// intermediate clones. For recursive types this causes stack overflow.
func TestEnumCloneNoDoubleClone(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"enum Token `clone {\n"+
		"  Word(string text),\n"+
		"  Empty,\n"+
		"}\n"+
		"test() {\n"+
		"  t := Token.Word(text: \"hello\");\n"+
		"  Token t2 = t.clone();\n"+
		"}\n")
	// Inside Token.clone(), the match destructure should NOT dup the string field —
	// the synthesized body explicitly calls .clone() on it. With match-dup suppressed,
	// there should be exactly 1 strdup block label (from the explicit .clone() call).
	// Without suppression, there would be 2 (match-dup + explicit clone).
	lines := strings.Split(ir, "\n")
	inClone := false
	strdupBlocks := 0
	for _, line := range lines {
		if strings.Contains(line, "define ") && strings.Contains(line, "Token.clone") {
			inClone = true
		} else if inClone && strings.HasPrefix(strings.TrimSpace(line), "define ") {
			break
		}
		// Count distinct strdup.copy block labels (not references)
		trimmed := strings.TrimSpace(line)
		if inClone && strings.HasPrefix(trimmed, "strdup.copy.") && strings.HasSuffix(trimmed, ":") {
			strdupBlocks++
		}
	}
	if strdupBlocks != 1 {
		t.Errorf("B0285: Token.clone() body should have exactly 1 strdup block (from explicit .clone()), got %d", strdupBlocks)
	}
}

// B0237/B0242: Match destructure of droppable enum dups string fields and
// registers them for arm-scope cleanup with a drop flag. The drop flag is
// cleared at move sites (PHI, push, etc.), so consumed bindings are not
// double-freed. Unconsumed bindings are dropped at arm-scope exit.
func TestMatchDupStringScopeCleanup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Slot {
			Empty,
			Used(string key, int value),
		}
		test() {
			s := Slot.Used(key: "hello", value: 42);
			match s {
				Used(k, v) => { int x = v; },
				Empty => { },
			}
		}
	`)
	// String field extracted from droppable enum should be dup'd
	codegentest.AssertContains(t, ir, "strdup.copy")
	// B0242: Dup'd string has a drop flag for arm-scope cleanup (unconsumed → dropped)
	codegentest.AssertContains(t, ir, "k.dropflag")
}

// B0242: Dup'd match binding consumed as arm result — drop flag must be cleared.
// Without clearDropFlag, arm-scope cleanup would drop the value, causing
// use-after-free on the match PHI result.
func TestMatchDupStringConsumedByPHI(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Slot {
			Empty,
			Used(string key, int value),
		}
		test() string {
			s := Slot.Used(key: "hello", value: 42);
			return match s {
				Used(k, v) => k,
				Empty => "none",
			};
		}
	`)
	codegentest.AssertContains(t, ir, "strdup.copy")
	codegentest.AssertContains(t, ir, "k.dropflag")
	// The drop flag must be cleared (store i1 false) before arm-scope cleanup
	codegentest.AssertContains(t, ir, "store i1 false")
}

// B0253: Match on borrowed enum with clone-able enum field must deep-clone.
// This is the pattern underlying JsonValue.get(this): match on this loads a
// shallow copy of the enum value. Extracted enum fields (like JsonValue from
// Slot.Used inside Map.[]) must be cloned, not shallow-copied, so the returned
// value is independent of the original map storage.
func TestMatchDupCloneableEnumFieldOnBorrow(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"enum Inner `clone {\n"+
		"  Text(string data),\n"+
		"  Empty,\n"+
		"}\n"+
		"enum Outer {\n"+
		"  Wrapped(Inner value),\n"+
		"  None,\n"+
		"  get_inner(this) Inner? {\n"+
		"    match this {\n"+
		"      Outer.Wrapped(v) => { return v; },\n"+
		"      _ => { return none; },\n"+
		"    }\n"+
		"  }\n"+
		"}\n"+
		"test() {\n"+
		"  o := Outer.Wrapped(value: Inner.Text(data: \"hello\"));\n"+
		"  r := o.get_inner();\n"+
		"}\n")
	// Inner enum field extracted from droppable Outer must be cloned via clone method
	codegentest.AssertContains(t, ir, "Inner.clone")
	codegentest.AssertContains(t, ir, "enum.clone.tmp")
}

// Temp enum receiver from a CallExpr should be dropped after a borrow method call.
// When movedDroppable causes enumCtorTemps to skip tracking, the method call
// path must explicitly drop the temp to prevent leaking the enum's heap data.
func TestEnumTempMethodReceiverDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"enum Inner `clone {\n"+
		"  Text(string data),\n"+
		"  Number(f64 value),\n"+
		"}\n"+
		"enum Holder {\n"+
		"  Items(Inner[] list),\n"+
		"  Nothing,\n"+
		"  extract(this) Inner[]? {\n"+
		"    match this {\n"+
		"      Holder.Items(items) => { return items; },\n"+
		"      _ => { return none; },\n"+
		"    }\n"+
		"  }\n"+
		"}\n"+
		"test() {\n"+
		"  Inner[] items = [Inner.Number(value: 1.0)];\n"+
		"  Inner[]? arr = Holder.Items(list: items).extract();\n"+
		"}\n")
	// The temp enum receiver should be dropped after the method call
	codegentest.AssertContains(t, ir, "Holder.drop")
}

// Return of a droppable enum from a dup'd vector index must clone the value.
// Without cloning, scope cleanup drops the dup'd vector (and its elements),
// leaving the returned shallow enum copy with dangling heap pointers.
func TestReturnEnumFromVectorIndexClone(t *testing.T) {
	ir := codegentest.GenerateIR(t, ""+
		"enum Inner `clone {\n"+
		"  Text(string data),\n"+
		"  Number(f64 value),\n"+
		"}\n"+
		"enum Holder {\n"+
		"  Items(Inner[] list),\n"+
		"  Nothing,\n"+
		"  at(this, int index) Inner? {\n"+
		"    match this {\n"+
		"      Holder.Items(items) => {\n"+
		"        if index >= 0 && index < items.len {\n"+
		"          return items[index];\n"+
		"        }\n"+
		"        return none;\n"+
		"      },\n"+
		"      _ => { return none; },\n"+
		"    }\n"+
		"  }\n"+
		"}\n"+
		"test() {\n"+
		"  Inner[] items = [Inner.Text(data: \"hello\")];\n"+
		"  h := Holder.Items(list: items);\n"+
		"  Inner? val = h.at(0);\n"+
		"}\n")
	// The returned enum value must be cloned via Inner.clone
	codegentest.AssertContains(t, ir, "Inner.clone")
}

// B0242: Dup'd match binding consumed via if-expression arm result.
// clearResultDropFlags must recurse into IfExpr branches.
func TestMatchDupStringConsumedViaIf(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Slot {
			Empty,
			Used(string key, int value),
		}
		test() string {
			s := Slot.Used(key: "hello", value: 42);
			return match s {
				Used(k, v) => if v > 0 { k } else { "neg" },
				Empty => "none",
			};
		}
	`)
	codegentest.AssertContains(t, ir, "strdup.copy")
	codegentest.AssertContains(t, ir, "k.dropflag")
	// clearResultDropFlags walks into IfExpr and clears k's drop flag
	codegentest.AssertContains(t, ir, "store i1 false")
}

// B0242: Dup'd match binding consumed via tuple literal (e.g., vector push).
// genTupleLit must clear the drop flag for ident elements so arm-scope cleanup
// doesn't free the string that is now owned by the tuple/vector.
func TestMatchDupStringConsumedViaTuple(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Slot {
			Empty,
			Used(string key, int value),
		}
		test() (string, int) {
			s := Slot.Used(key: "hello", value: 42);
			return match s {
				Used(k, v) => (k, v),
				Empty => ("none", 0),
			};
		}
	`)
	codegentest.AssertContains(t, ir, "strdup.copy")
	codegentest.AssertContains(t, ir, "k.dropflag")
	// genTupleLit clears the drop flag when k is consumed by the tuple
	codegentest.AssertContains(t, ir, "store i1 false")
}

// B0268: Vector[(int, FieldlessEnum)] must NOT reference an enum drop.
// Fieldless enums (no variant fields) have no drop function — emitting a call
// to a non-existent drop causes linker errors (undefined symbol).
func TestVectorTupleFieldlessEnumNoDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color { Red, Green, Blue }
		main() {
			(int, Color)[] v = [(1, Color.Red), (2, Color.Green)];
		}
	`)
	// Fieldless enum in tuple should NOT generate a Color.drop call or declaration
	codegentest.AssertNotContains(t, ir, "Color.drop")
}

// T0371: Destructuring a tuple-with-heap-fields into named locals registers
// a drop binding per field so each local owns and frees its piece. Without
// these per-field drops, the string would leak after destructure.
func TestT0371DestructureRegistersFieldDrops(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			(a, b) := (1, "a" + "b");
		}
	`)
	// b is a destructured string local — should have a drop flag and call drop.
	codegentest.AssertContains(t, ir, "b.dropflag")
	codegentest.AssertContains(t, ir, "promise_string_drop")
}

// T0371: Enum variant with tuple-of-string field. Sema's fieldTypeHasDrop and
// codegen's variantFieldNeedsDrop must both recurse into tuples so the synth
// enum drop walks the tuple field and frees the inner string.
func TestT0371EnumWithTupleStringHasDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Pair {
			Empty,
			Some((int, string) data),
		}
		main() {
			s := Pair.Some(data: (1, "a" + "b"));
		}
	`)
	// Synth drop function should be emitted for Pair (because tuple variant
	// field contains a string). Verify via the variant drop block name.
	codegentest.AssertContains(t, ir, "Pair.drop")
	codegentest.AssertContains(t, ir, "promise_string_drop")
}

// T0371: Destructuring a borrowed tuple (e.g., from a for-in loop variable)
// must NOT register drops for the destructured locals — they are borrows of
// the container's elements, and adding drops would double-free with the
// container's element walk.
func TestT0371DestructureBorrowSourceNoDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box { int n; }
		sum_first((int, Box)[] v) int {
			int total = 0;
			for tup in v {
				(idx, bx) := tup;
				total = total + bx.n;
			}
			return total;
		}
		main() {
			int n = sum_first([(1, Box(n: 5))]);
		}
	`)
	// bx is destructured from a for-in loop variable (borrow); no drop binding.
	codegentest.AssertNotContains(t, ir, "bx.dropflag")
}

// T0371: A tuple containing an enum constructor with heap variant data must
// claim the enum-ctor temp's drop flag so the tuple is the unique owner. Tests
// the savedEnumTemps loop in genTupleLit. Without it, the enum's variant
// string would be freed at stmt end while the tuple still references it.
func TestT0371TupleClaimsEnumCtorTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color {
			Red,
			Tagged(string label),
		}
		main() {
			(int, Color) t = (1, Color.Tagged(label: "a" + "b"));
		}
	`)
	// Tuple var t has its own tuple-walk drop binding.
	codegentest.AssertContains(t, ir, "t.dropflag")
	codegentest.AssertContains(t, ir, "tupdrop.exec")
	// The savedEnumTemps loop emits a drop-flag-clear store for the enum ctor
	// temp during element evaluation. The tuple-walk then drops the enum.
	codegentest.AssertContains(t, ir, "Color.drop")
}

// T0102: Enum with string variant gets synthesized drop
func TestDropSynthesizedEnumString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Value {
			Text(string s),
			Number(int n),
		}
		main() {
			v := Value.Text("hello");
		}
	`)
	codegentest.AssertContains(t, ir, "define void @Value.drop")
	codegentest.AssertContains(t, ir, "enum.drop.Text")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop(")
	codegentest.AssertContains(t, ir, "v.dropflag")
}

// T0102: Fieldless enum does NOT get synthesized drop
func TestDropSynthesizedEnumFieldless(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color { Red, Green, Blue, }
		main() {
			c := Color.Red;
		}
	`)
	codegentest.AssertNotContains(t, ir, "Color.drop")
}

// T0102: Enum with vector variant gets synthesized drop
func TestDropSynthesizedEnumVector(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Value {
			Items(int[] items),
			Single(int n),
		}
		main() {
			v := Value.Single(42);
		}
	`)
	codegentest.AssertContains(t, ir, "define void @Value.drop")
	codegentest.AssertContains(t, ir, "enum.drop.Items")
	codegentest.AssertContains(t, ir, "call void @Vector.drop(")
}

// T0102: Enum with user type variant gets synthesized drop
func TestDropSynthesizedEnumUserType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Resource {
			int id;
			drop(~this) { }
		}
		enum Holder {
			Has(Resource r),
			Empty,
		}
		main() {
			h := Holder.Empty;
		}
	`)
	codegentest.AssertContains(t, ir, "define void @Holder.drop")
	codegentest.AssertContains(t, ir, "enum.drop.Has")
	codegentest.AssertContains(t, ir, "call void @Resource.drop(")
}

// B0212: Vector[Enum] scope-exit drops enum elements (each element's synthesized drop is called).
func TestDropVectorEnumElements(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Value {
			Null,
			Str(string s),
			List(Value[] items),
		}
		main() {
			v := [Value.Str("a"), Value.Null];
		}
	`)
	// Scope-exit vector drop should iterate elements and call Value.drop
	codegentest.AssertContains(t, ir, "vecdrop.head")
	codegentest.AssertContains(t, ir, "call void @Value.drop(")
}

// B0212: Enum variant with vector field drops enum elements in the vector.
