package codegen

import (
	"bytes"
	"strings"
	"testing"
)

// === Enum Tests ===

func TestEnumLayout(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		main() { }
	`)
	// Four-struct layout for enum
	assertContains(t, ir, "%promise_Color_t = type {}")
	assertContains(t, ir, "%promise_Color_m = type { %promise_Color_t* }")
	assertContains(t, ir, "%promise_Color_i = type { %promise_Color_m* }")
	assertContains(t, ir, "%promise_Color_v = type { i8*, %promise_Color_i*, i32 }")
}

func TestEnumLayoutData(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		main() { }
	`)
	assertContains(t, ir, "%promise_Shape_t = type {}")
	assertContains(t, ir, "%promise_Shape_m = type { %promise_Shape_t* }")
	assertContains(t, ir, "%promise_Shape_i = type { %promise_Shape_m* }")
	// Value struct: vtable, instance ptr, tag, data bytes
	assertContains(t, ir, "%promise_Shape_v = type { i8*, %promise_Shape_i*, i32,")
	// Internal struct: tag + data area
	assertContains(t, ir, "%promise_Shape_enum = type { i32,")
}

func TestEnumFieldlessVariant(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Green;
		}
		main() { }
	`)
	// Green is tag 1
	assertContains(t, ir, "store i32 1")
}

func TestEnumDataConstructor(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		test() {
			Shape s = Shape.Circle(3.14);
		}
		main() { }
	`)
	// Should store tag (Circle = 0)
	assertContains(t, ir, "store i32 0")
	// Should store double field via GEP + bitcast
	assertContains(t, ir, "store double")
	assertContains(t, ir, "bitcast")
}

func TestEnumMatchFieldless(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "switch i32")
	// Should have arm blocks
	assertContains(t, ir, "match.arm0")
	assertContains(t, ir, "match.arm1")
	assertContains(t, ir, "match.arm2")
	// Merge block with phi
	assertContains(t, ir, "match.end")
}

func TestEnumMatchDestructure(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "switch i32")
	// Should bitcast + GEP to load variant data
	assertContains(t, ir, "bitcast")
	assertContains(t, ir, "load double")
}

func TestEnumMatchShortDestructure(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "match.arm0")
	assertContains(t, ir, "match.arm1")
}

func TestEnumMatchWildcard(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "match.arm0")
	assertContains(t, ir, "match.arm1")
}

func TestEnumMatchNameBinding(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "alloca i32")
}

// T1155: a match-arm pattern binding that reuses the scrutinee's name must be
// scoped to the arm only. Before the fix, codegen left c.locals[scrutinee]
// pointing at the destructured (string) alloca, so a later `match` on the same
// name evaluated its subject against the wrong alloca and emitted garbage /
// self-recursive control flow → runtime stack overflow. The correct IR loads the
// enum subject from the parameter alloca (%b.addr) for BOTH matches.
func TestEnumMatchScrutineeShadow(t *testing.T) {
	ir := generateIR(t, `
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
	ir := generateIR(t, `
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
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "i32 0, label %match.arm0")
	assertContains(t, ir, "i32 1, label %match.arm1")
	assertContains(t, ir, "i32 2, label %match.arm2")
}

func TestEnumMatchBlock(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "match.arm0")
}

func TestMatchIntLiteral(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "icmp eq")
	assertContains(t, ir, "match.arm")
	assertContains(t, ir, "match.next")
}

func TestEnumHeaderFieldless(t *testing.T) {
	result := compileResult(t, `
		enum Color { Red, Green, Blue }
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	assertContains(t, header, "promise_Color_t")
	assertContains(t, header, "promise_Color_m")
	assertContains(t, header, "promise_Color_i")
	assertContains(t, header, "promise_Color_v")
	// Value struct should have tag field
	assertContains(t, header, "int32_t")
}

func TestEnumDataFieldlessVariant(t *testing.T) {
	// Exercises zeroinitializer path: fieldless variant in a data enum
	ir := generateIR(t, `
		enum Result { Ok(int value), None }
		test() {
			Result r = Result.None;
		}
		main() { }
	`)
	// None is tag 1, built via zeroinitializer + insertvalue (not alloca with partial store)
	assertContains(t, ir, "insertvalue %promise_Result_enum zeroinitializer, i32 1, 0")
	// Internal struct should exist for the data enum
	assertContains(t, ir, "%promise_Result_enum = type { i32,")
}

func TestEnumAsFunctionParam(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "define i1 @__user.is_red(i32 %c)")
	// Param should be alloca'd as i32
	assertContains(t, ir, "alloca i32")
	assertContains(t, ir, "switch i32")
}

func TestEnumAsFunctionReturn(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		get_green() Color {
			return Color.Green;
		}
		main() { }
	`)
	// Enum return should use i32
	assertContains(t, ir, "define i32 @__user.get_green()")
	assertContains(t, ir, "ret i32 1")
}

func TestMatchValueNameBinding(t *testing.T) {
	ir := generateIR(t, `
		test() int {
			int x = 42;
			return match x {
				val => val + 1,
			};
		}
		main() { }
	`)
	// Name binding in value match: alloca + store the subject
	assertContains(t, ir, "alloca i64")
	assertContains(t, ir, "add i64")
}

func TestEnumDestructureUnderscoreSkip(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "load i64")
}

func TestEnumHeaderData(t *testing.T) {
	result := compileResult(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		main() { }
	`)

	var buf bytes.Buffer
	if err := GenerateHeader(&buf, result.Layouts, result.EnumLayouts, result.Externs); err != nil {
		t.Fatalf("GenerateHeader error: %v", err)
	}
	header := buf.String()

	assertContains(t, header, "promise_Shape_t")
	assertContains(t, header, "promise_Shape_v")
	// Data enum value struct should have tag and data fields
	assertContains(t, header, "int32_t              tag;")
	assertContains(t, header, "uint8_t              data[16];")
}

func TestTypedHandlerNoMatchPropagation(t *testing.T) {
	ir := generateIR(t, `
		type IoError is error { int code; }
		type ParseError is error { int line; }
		fail!() void { raise ParseError(message: "parse", line: 1); }
		handler!() void {
			fail() ? e is IoError { };
		}
		main() { handler() ? e { }; }
	`)
	// Nomatch should propagate (re-wrap error and return)
	assertContains(t, ir, "error.typed.nomatch")
	assertContains(t, ir, "promise_type_is")
}

func TestTupleDestructure(t *testing.T) {
	ir := generateIR(t, `
		pair() (int, int) { return (1, 2); }
		main() { (a, b) := pair(); }
	`)
	// Should use extractvalue to destructure
	assertContains(t, ir, "extractvalue { i64, i64 }")
}

// T0441: 3-element tuple destructure (grammar now accepts N>=2 names).
func TestTupleDestructureThreeElements(t *testing.T) {
	ir := generateIR(t, `
		triple() (int, int, int) { return (1, 2, 3); }
		main() { (a, b, c) := triple(); }
	`)
	assertContains(t, ir, "extractvalue { i64, i64, i64 }")
	assertContains(t, ir, "%a = alloca i64")
	assertContains(t, ir, "%b = alloca i64")
	assertContains(t, ir, "%c = alloca i64")
}

func TestTupleDestructureSkip(t *testing.T) {
	ir := generateIR(t, `
		pair() (int, int) { return (1, 2); }
		main() { (_, b) := pair(); }
	`)
	// Should extract second element but skip first
	assertContains(t, ir, "extractvalue { i64, i64 }")
	// b should be allocated
	assertContains(t, ir, "%b = alloca i64")
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
	ir := generateIR(t, `
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
	assertContains(t, extractFunction(ir, "__user.fresh"), "env.tmp.drop")
	aliased := extractFunction(ir, "__user.aliased")
	assertContains(t, aliased, "define") // guard: the body was actually found
	assertNotContains(t, aliased, "env.tmp.drop")
}

func TestIsEnumVariant(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		test() {
			Color c = Color.Red;
			bool b = c is Red;
		}
		main() { }
	`)
	// Fieldless enum: value IS the tag, compare with icmp eq
	assertContains(t, ir, "icmp eq i32")
}

func TestIsEnumVariantData(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		test() {
			Shape s = Shape.Circle(radius: 3.14);
			bool b = s is Circle;
		}
		main() { }
	`)
	// Data-carrying enum: extract tag from struct, then compare
	assertContains(t, ir, "extractvalue")
	assertContains(t, ir, "icmp eq i32")
}

// B0235: Vector[Enum] index assignment drops old enum element before storing new value.
func TestVectorEnumIndexAssignDropsOld(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "call void @Value.drop(")
}

// T0741 Part A: an enum variant whose payload is a closure must drop the
// closure's env in the synthesized enum drop. variantFieldNeedsDrop now returns
// true for *types.Signature, so emitVariantFieldDrop's closure case runs.
func TestDropEnumVariantClosurePayload(t *testing.T) {
	ir := generateIR(t, `
		enum Callback {
			holds(() -> int cb),
			empty,
		}
		main() {
			s := "enum" + " payload";
			c := Callback.holds(cb: move || -> s.len);
		}
	`)
	assertContains(t, ir, "define void @Callback.drop")
	assertContains(t, ir, "closure.env.free")
	assertContains(t, ir, "env.deep_drop")
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
	ir := generateIR(t, `
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
	assertContains(t, ir, "enumdup.holds")
	assertContains(t, ir, "store { i8*, i8* } zeroinitializer")
}

// T1109: A Weak[T] variant field in a container must dup via dupWeak (atomic
// weak-count increment, emits weakdup.inc), the symmetric sibling of the Arc
// branch. Like Arc, Weak's LLVM value is a bare i8*, so it must NOT reach
// dupHeapValue. Guards the weak branch of emitVariantFieldDup.
func TestDupEnumVariantWeakRefcount(t *testing.T) {
	ir := generateIR(t, `
		enum WHolder { One(Weak[int] w, int n) }
		main() {
			a := Ref[int](99);
			m := Map[int, WHolder]();
			m[1] = WHolder.One(a.downgrade(), 3);
		}
	`)
	assertContains(t, ir, "enumdup.One")
	assertContains(t, ir, "weakdup.inc")
}

// T0073: Match expression with to_string in an arm — temp claimed by phi
func TestStringTempInMatchArm(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "r.dropflag")
}

// B0236: Match destructure of droppable enum dups heap user type fields.
func TestMatchDupHeapUserType(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "heapdup.copy")
	assertContains(t, ir, "call i8* @pal_alloc(")
}

// B0236: Match destructure of droppable enum dups vector fields.
func TestMatchDupVector(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "vecdup.copy")
}

// B0284: Map with non-cloneable value types (enum with drops but no `clone`)
// must NOT be cloned via Map.clone() — the clone would be shallow, causing
// double-free when both original and clone drop shared enum heap data.
func TestMatchDupMapNotClonedNonCloneableValues(t *testing.T) {
	ir := generateIR(t, `
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
	ir := generateIR(t, `
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
	assertContains(t, ir, "Map[string, int].clone")
}

// B0284: Map with cloneable enum values CAN be cloned — the enum has `clone`
// so the clone's internal match-dup will deep-copy via enum clone.
func TestMatchDupMapClonedWithCloneableEnumValues(t *testing.T) {
	ir := generateIR(t, ""+
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
	ir := generateIR(t, `
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
	assertContains(t, ir, "Map[string, Color].clone")
}

// B0244: Match destructure of droppable enum clones enum-typed fields via clone().
func TestMatchDupEnumClone(t *testing.T) {
	ir := generateIR(t, ""+
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
	assertContains(t, ir, "Inner.clone")
	assertContains(t, ir, "enum.clone.tmp")
}

// B0285: Synthesized enum clone method must NOT double-clone fields.
// The match inside clone() destructures variant fields, and without suppression
// the match-dup mechanism also clones them — causing double work and leaked
// intermediate clones. For recursive types this causes stack overflow.
func TestEnumCloneNoDoubleClone(t *testing.T) {
	ir := generateIR(t, ""+
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
	ir := generateIR(t, `
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
	assertContains(t, ir, "strdup.copy")
	// B0242: Dup'd string has a drop flag for arm-scope cleanup (unconsumed → dropped)
	assertContains(t, ir, "k.dropflag")
}

// B0242: Dup'd match binding consumed as arm result — drop flag must be cleared.
// Without clearDropFlag, arm-scope cleanup would drop the value, causing
// use-after-free on the match PHI result.
func TestMatchDupStringConsumedByPHI(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "strdup.copy")
	assertContains(t, ir, "k.dropflag")
	// The drop flag must be cleared (store i1 false) before arm-scope cleanup
	assertContains(t, ir, "store i1 false")
}

// B0253: Match on borrowed enum with clone-able enum field must deep-clone.
// This is the pattern underlying JsonValue.get(this): match on this loads a
// shallow copy of the enum value. Extracted enum fields (like JsonValue from
// Slot.Used inside Map.[]) must be cloned, not shallow-copied, so the returned
// value is independent of the original map storage.
func TestMatchDupCloneableEnumFieldOnBorrow(t *testing.T) {
	ir := generateIR(t, ""+
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
	assertContains(t, ir, "Inner.clone")
	assertContains(t, ir, "enum.clone.tmp")
}

// Temp enum receiver from a CallExpr should be dropped after a borrow method call.
// When movedDroppable causes enumCtorTemps to skip tracking, the method call
// path must explicitly drop the temp to prevent leaking the enum's heap data.
func TestEnumTempMethodReceiverDrop(t *testing.T) {
	ir := generateIR(t, ""+
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
	assertContains(t, ir, "Holder.drop")
}

// Return of a droppable enum from a dup'd vector index must clone the value.
// Without cloning, scope cleanup drops the dup'd vector (and its elements),
// leaving the returned shallow enum copy with dangling heap pointers.
func TestReturnEnumFromVectorIndexClone(t *testing.T) {
	ir := generateIR(t, ""+
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
	assertContains(t, ir, "Inner.clone")
}

// B0242: Dup'd match binding consumed via if-expression arm result.
// clearResultDropFlags must recurse into IfExpr branches.
func TestMatchDupStringConsumedViaIf(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "strdup.copy")
	assertContains(t, ir, "k.dropflag")
	// clearResultDropFlags walks into IfExpr and clears k's drop flag
	assertContains(t, ir, "store i1 false")
}

// B0242: Dup'd match binding consumed via tuple literal (e.g., vector push).
// genTupleLit must clear the drop flag for ident elements so arm-scope cleanup
// doesn't free the string that is now owned by the tuple/vector.
func TestMatchDupStringConsumedViaTuple(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "strdup.copy")
	assertContains(t, ir, "k.dropflag")
	// genTupleLit clears the drop flag when k is consumed by the tuple
	assertContains(t, ir, "store i1 false")
}

// B0268: Vector[(int, FieldlessEnum)] must NOT reference an enum drop.
// Fieldless enums (no variant fields) have no drop function — emitting a call
// to a non-existent drop causes linker errors (undefined symbol).
func TestVectorTupleFieldlessEnumNoDrop(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		main() {
			(int, Color)[] v = [(1, Color.Red), (2, Color.Green)];
		}
	`)
	// Fieldless enum in tuple should NOT generate a Color.drop call or declaration
	assertNotContains(t, ir, "Color.drop")
}

// T0371: Destructuring a tuple-with-heap-fields into named locals registers
// a drop binding per field so each local owns and frees its piece. Without
// these per-field drops, the string would leak after destructure.
func TestT0371DestructureRegistersFieldDrops(t *testing.T) {
	ir := generateIR(t, `
		main() {
			(a, b) := (1, "a" + "b");
		}
	`)
	// b is a destructured string local — should have a drop flag and call drop.
	assertContains(t, ir, "b.dropflag")
	assertContains(t, ir, "promise_string_drop")
}

// T0371: Enum variant with tuple-of-string field. Sema's fieldTypeHasDrop and
// codegen's variantFieldNeedsDrop must both recurse into tuples so the synth
// enum drop walks the tuple field and frees the inner string.
func TestT0371EnumWithTupleStringHasDrop(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "Pair.drop")
	assertContains(t, ir, "promise_string_drop")
}

// T0371: Destructuring a borrowed tuple (e.g., from a for-in loop variable)
// must NOT register drops for the destructured locals — they are borrows of
// the container's elements, and adding drops would double-free with the
// container's element walk.
func TestT0371DestructureBorrowSourceNoDrop(t *testing.T) {
	ir := generateIR(t, `
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
	assertNotContains(t, ir, "bx.dropflag")
}

// T0371: A tuple containing an enum constructor with heap variant data must
// claim the enum-ctor temp's drop flag so the tuple is the unique owner. Tests
// the savedEnumTemps loop in genTupleLit. Without it, the enum's variant
// string would be freed at stmt end while the tuple still references it.
func TestT0371TupleClaimsEnumCtorTemp(t *testing.T) {
	ir := generateIR(t, `
		enum Color {
			Red,
			Tagged(string label),
		}
		main() {
			(int, Color) t = (1, Color.Tagged(label: "a" + "b"));
		}
	`)
	// Tuple var t has its own tuple-walk drop binding.
	assertContains(t, ir, "t.dropflag")
	assertContains(t, ir, "tupdrop.exec")
	// The savedEnumTemps loop emits a drop-flag-clear store for the enum ctor
	// temp during element evaluation. The tuple-walk then drops the enum.
	assertContains(t, ir, "Color.drop")
}

// T0102: Enum with string variant gets synthesized drop
func TestDropSynthesizedEnumString(t *testing.T) {
	ir := generateIR(t, `
		enum Value {
			Text(string s),
			Number(int n),
		}
		main() {
			v := Value.Text("hello");
		}
	`)
	assertContains(t, ir, "define void @Value.drop")
	assertContains(t, ir, "enum.drop.Text")
	assertContains(t, ir, "call void @promise_string_drop(")
	assertContains(t, ir, "v.dropflag")
}

// T0102: Fieldless enum does NOT get synthesized drop
func TestDropSynthesizedEnumFieldless(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue, }
		main() {
			c := Color.Red;
		}
	`)
	assertNotContains(t, ir, "Color.drop")
}

// T0102: Enum with vector variant gets synthesized drop
func TestDropSynthesizedEnumVector(t *testing.T) {
	ir := generateIR(t, `
		enum Value {
			Items(int[] items),
			Single(int n),
		}
		main() {
			v := Value.Single(42);
		}
	`)
	assertContains(t, ir, "define void @Value.drop")
	assertContains(t, ir, "enum.drop.Items")
	assertContains(t, ir, "call void @Vector.drop(")
}

// T0102: Enum with user type variant gets synthesized drop
func TestDropSynthesizedEnumUserType(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "define void @Holder.drop")
	assertContains(t, ir, "enum.drop.Has")
	assertContains(t, ir, "call void @Resource.drop(")
}

// B0212: Vector[Enum] scope-exit drops enum elements (each element's synthesized drop is called).
func TestDropVectorEnumElements(t *testing.T) {
	ir := generateIR(t, `
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
	assertContains(t, ir, "vecdrop.head")
	assertContains(t, ir, "call void @Value.drop(")
}

// B0212: Enum variant with vector field drops enum elements in the vector.
func TestDropEnumVariantVectorElements(t *testing.T) {
	ir := generateIR(t, `
		enum Value {
			Null,
			Str(string s),
			List(Value[] items),
		}
		main() {
			v := Value.List([Value.Str("inner")]);
		}
	`)
	// Value.drop for List variant should drop vector elements before freeing buffer
	assertContains(t, ir, "define void @Value.drop(")
	assertContains(t, ir, "enum.drop.List")
	assertContains(t, ir, "vecdrop.head") // element drop loop in variant field drop
}

// T0567: Explicit drop(~this) on enum — user-defined drop is emitted and called
func TestDropExplicitEnumMethod(t *testing.T) {
	ir := generateIR(t, `
		enum Color {
			Red,
			Green,
			drop(~this) {}
		}
		main() {
			c := Color.Red;
		}
	`)
	// The user's drop method should be declared and called at scope exit
	assertContains(t, ir, "define void @Color.drop(")
	assertContains(t, ir, "call void @Color.drop(")
	assertContains(t, ir, "enum.drop.call")
}

// T0604: Explicit drop(~this) on enum with droppable variant fields —
// variant field cleanup (switch on tag, drop per-variant) is emitted after the user body.
func TestDropExplicitEnumVariantFieldCleanup(t *testing.T) {
	ir := generateIR(t, `
		enum Container {
			Data(string name),
			Empty,
			drop(~this) {}
		}
		main() {
			c := Container.Data(name: "test");
		}
	`)
	// The user's drop method should contain variant field cleanup blocks
	assertContains(t, ir, "define void @Container.drop(")
	assertContains(t, ir, "enum.drop.field.Data")
	assertContains(t, ir, "enum.drop.field.done")
	assertContains(t, ir, "call void @promise_string_drop(")
}

// T1108: When a variable with a drop binding is moved into an enum-constructor
// temp that is passed to a BORROW param, the moved-in payload's only owner is
// the enum temp (the source ident's drop flag was cleared at the move). The
// borrow callee does not consume it, so the caller MUST drop the enum temp at
// statement end to free the payload — otherwise it leaks. (Was B0252's
// TestEnumCtorTempSkippedWhenMovedDroppable, which encoded that leak.)
func TestEnumCtorTempMovedDroppableBorrowDrops(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			string name;
			drop(~this) { }
		}
		enum Holder {
			Has(Resource r),
			Empty,
		}
		consume(Holder h) { }
		main() {
			r := Resource(name: "test");
			consume(Holder.Has(r));
		}
	`)
	// The enum should have a synthesized drop
	assertContains(t, ir, "define void @Holder.drop")
	// The borrowed enum ctor temp must be dropped at statement end so the
	// moved-in Resource is freed (zero-leak policy).
	assertContains(t, ir, "enum.ctor.drop")
}

// T1108: Non-ident expressions (e.g., function calls) returning droppable values
// moved into an enum-constructor temp that is then passed to a BORROW param must
// be dropped at statement end. The call result's only owner is the enum temp;
// the borrow callee does not consume it, so skipping the drop would leak the
// result. (Was B0286's TestEnumCtorTempSkippedForNonIdentDroppableArg — the IR
// test that masked the leak it introduced.)
func TestEnumCtorTempNonIdentDroppableBorrowDrops(t *testing.T) {
	ir := generateIR(t, `
		type Resource {
			string name;
			drop(~this) { }
		}
		enum Holder {
			Has(Resource r),
			Empty,
		}
		make_resource() Resource {
			return Resource(name: "test");
		}
		consume(Holder h) { }
		main() {
			consume(Holder.Has(r: make_resource()));
		}
	`)
	assertContains(t, ir, "define void @Holder.drop")
	assertContains(t, ir, "enum.ctor.drop")
}

// T1108: Enum variant with a non-ident arg of a synth-drop type (contains a
// string field) passed to a BORROW param must likewise be dropped at statement
// end. (Was B0286's TestEnumCtorTempSkippedForNonIdentSynthDropArg.)
func TestEnumCtorTempNonIdentSynthDropBorrowDrops(t *testing.T) {
	ir := generateIR(t, `
		type Info {
			string label;
		}
		enum Wrapper {
			Wrap(Info data),
			None,
		}
		make_info() Info {
			return Info(label: "hello");
		}
		consume(Wrapper w) { }
		main() {
			consume(Wrapper.Wrap(data: make_info()));
		}
	`)
	assertContains(t, ir, "define void @Wrapper.drop")
	assertContains(t, ir, "enum.ctor.drop")
}

// B0293: Enum variable reassignment must clear enumCtorTemps to prevent double-drop.
// Without the fix, the enum ctor temp drop fires at statement end AND the variable's
// scope-exit drop fires, causing use-after-free on the variant's heap data.
func TestEnumCtorTempClearedOnReassign(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { string name; int value; }
		enum Container[T] { Holding(T item), Empty, }
		test() {
			c := Container[Wrapper].Holding(Wrapper(name: "first", value: 1));
			c = Container[Wrapper].Holding(Wrapper(name: "second", value: 2));
		}
	`)
	// The reassignment path should NOT have enum.ctor.drop blocks —
	// ownership transferred to the variable, ctor temps must be cleared.
	assertNotContains(t, ir, "enum.ctor.drop")
}

func TestBuildMatchPhiMixedArms(t *testing.T) {
	// Match expression where some arms produce values and at least one arm
	// has an early return. buildMatchPhi must handle missing predecessors
	// by inserting null placeholders for arms that branch to merge without values.
	ir := generateIR(t, `
		test(int n) int {
			int result = match n {
				1 => 10,
				2 => 20,
				_ => 0,
			};
			return result;
		}
		main() { }
	`)
	// PHI node should exist in the merge block with values from all arms
	assertContains(t, ir, "phi i64")
	assertContains(t, ir, "match.end")
}

func TestBuildMatchPhiStatementOnly(t *testing.T) {
	// Match used as a statement (no arm produces a value) — no PHI needed
	ir := generateIR(t, `
		test(int n) {
			match n {
				1 => { int x = 10; },
				_ => { int y = 20; },
			};
		}
		main() { }
	`)
	// Should have match arms but the merge block shouldn't have a PHI
	assertContains(t, ir, "match.arm")
	assertContains(t, ir, "match.end")
}

func TestEnumMatchPhiWithEarlyReturn(t *testing.T) {
	// Enum match where one arm returns early (doesn't branch to merge).
	// buildMatchPhi must skip non-merging arms to avoid PHI predecessor mismatch.
	ir := generateIR(t, `
		enum Op { Add(int a, int b), Neg(int a) }
		eval(Op op) int {
			return match op {
				Add(a, b) => a + b,
				Neg(a) => 0 - a,
			};
		}
		main() { }
	`)
	// Both arms produce values; PHI should merge them
	assertContains(t, ir, "phi i64")
}

// T0421: Fieldless enum interpolation emits switch on tag → variant name string.
func TestStringInterpolationEnumFieldless(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue }
		main() {
			Color c = Color.Green;
			string s = "{c}";
		}
	`)
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "enum.interp.Red")
	assertContains(t, ir, "enum.interp.Green")
	assertContains(t, ir, "enum.interp.Blue")
	assertContains(t, ir, "enum.interp.merge")
	assertContains(t, ir, `"Red"`)
	assertContains(t, ir, `"Green"`)
	assertContains(t, ir, `"Blue"`)
}

// T0421: Data enum interpolation extracts tag from field 0 and emits switch.
func TestStringInterpolationEnumData(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		main() {
			Shape s = Shape.Circle(1.0);
			string x = "{s}";
		}
	`)
	assertContains(t, ir, "switch i32")
	assertContains(t, ir, "enum.interp.Circle")
	assertContains(t, ir, "enum.interp.Rect")
	assertContains(t, ir, `"Circle"`)
	assertContains(t, ir, `"Rect"`)
}

func TestNegatedNarrowingCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string? cc = "hello";
			if !cc {
				string s = "none";
			} else {
				string s = cc;
			}
		}
	`)
	assertContains(t, ir, "narrow.then")
	assertContains(t, ir, "narrow.else")
}

func TestCompoundNarrowingCodegen(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int? a = 1;
			string? b = "hi";
			if a && b {
				int x = a;
				string y = b;
			}
		}
	`)
	assertContains(t, ir, "narrow.check")
	assertContains(t, ir, "narrow.then")
}

// T1233 (Stage 1 — the original leak in the bug's repro): a capturing closure
// destructured OUT of an owned tuple takes ownership of its heap env, but
// maybeRegisterDrop no-ops on *types.Signature (extractNamed returns nil), so
// nothing freed the env → 1 allocation leaked per closure. genDestructureVarDecl
// now routes signature-typed destructured locals through maybeRegisterEnvFree,
// giving the destructured closure a bindingFreeEnv (the `env.free`/deep-drop-or-
// pal_free arm). The source tuple's own drop flag is cleared right after the
// destructure so its field walk does NOT also free the moved-out env (a would-be
// double free). This Go test pins that IR shape; the sibling Promise leak test
// `_t1233_destructure_closure` proves the runtime single-free. Every OTHER arm of
// the fix already had a Go IR test — this closes the destructure-arm gap.
func TestT1233DestructuredClosureFromTupleFreesEnv(t *testing.T) {
	ir := generateIR(t, `
		make_adder(int x)() -> int { return move || -> x + 1; }
		caller() {
			(() -> int, int) t = (make_adder(6), 1);
			(g, n) := t;
			g();
		}
	`)
	// The destructured closure `g` gets a bindingFreeEnv: null-check then either
	// call the per-closure env-drop fn (deep) or pal_free (shallow).
	assertContains(t, ir, "env.free")
	assertContains(t, ir, "env.shallow_free")
	assertContains(t, ir, "call void @pal_free(")
	// The source tuple's drop flag is cleared after the destructure so its
	// scope-exit field walk does not double-free the moved-out env.
	assertContains(t, ir, "store i1 false, i1* %t.dropflag")
}

func TestValueTypeDestructureIsPattern(t *testing.T) {
	ir := generateIR(t, `
		type Point {
			int x `+"`value"+`;
			int y `+"`value"+`;
		}
		check(Point p) int {
			if p is Point(px, py) { return px + py; }
			return 0;
		}
		main() {
			check(Point(x: 3, y: 4));
		}
	`)
	// Should use RTTI global for the is-check
	assertContains(t, ir, "bitcast %promise_Point_i* @promise_rtti_Point to i8*")
	// Field extraction uses extractvalue on the value struct (fields at index 1, 2)
	assertContains(t, ir, "extractvalue %promise_Point_v")
}

// --- Failable getter codegen ---

func TestMatchMixedVoidAndValueArms(t *testing.T) {
	// Match where some arms produce a value and some call a void function.
	// buildMatchPhi must filter void-typed values before constructing the PHI.
	ir := generateIR(t, `
		test(int n) int {
			int result = match n {
				1 => 10,
				2 => { print_line("side effect"); 20; },
				_ => 0,
			};
			return result;
		}
		main() { }
	`)
	assertContains(t, ir, "phi i64")
	assertContains(t, ir, "match.end")
}

func TestMatchAllVoidArms(t *testing.T) {
	// Match used as statement where all arms are void (no arm produces a value).
	// buildMatchPhi should return nil (no PHI node needed).
	ir := generateIR(t, `
		test(int n) {
			match n {
				1 => { print_line("one"); },
				2 => { print_line("two"); },
				_ => { print_line("other"); },
			};
		}
		main() { }
	`)
	assertContains(t, ir, "match.arm")
	assertContains(t, ir, "match.end")
}

// B0126: match with block body containing if/else as expression.
// genBlockValue must capture the if/else result via genIfStmtValue.
func TestMatchBlockIfElseExpr(t *testing.T) {
	ir := generateIR(t, `
		classify(int n) string {
			return match n {
				3 => "small",
				_ => {
					if n < 100 {
						"medium";
					} else {
						"large";
					}
				},
			};
		}
		main() { }
	`)
	assertContains(t, ir, "phi")
	assertContains(t, ir, "match.end")
	assertContains(t, ir, "if.then")
	assertContains(t, ir, "if.end")
}

// B0126: single wildcard arm with block body containing if/else-if chain.
func TestMatchBlockIfElseIfChain(t *testing.T) {
	ir := generateIR(t, `
		classify(int n) string {
			return match n {
				_ => {
					if n < 10 {
						"tiny";
					} else if n < 100 {
						"small";
					} else {
						"big";
					}
				},
			};
		}
		main() { }
	`)
	assertContains(t, ir, "phi")
	assertContains(t, ir, "if.then")
	assertContains(t, ir, "if.end")
}

// B0135: if/else void inside a match block body must not produce phi void.
func TestMatchBlockIfElseVoidNoPhi(t *testing.T) {
	ir := generateIR(t, `
		test(int n) {
			match n {
				1 => {
					if n > 0 {
						print_line("a");
					} else {
						print_line("b");
					}
				},
				_ => { print_line("c"); },
			};
		}
		main() { }
	`)
	assertContains(t, ir, "if.then")
	assertNotContains(t, ir, "phi void")
}

func TestEnumMethodDecl(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue,
			describe(this) string {
				match this {
					Color.Red => { return "red"; },
					_ => { return "other"; },
				}
			}
		}
		main() { string s = Color.Red.describe(); }
	`)
	assertContains(t, ir, "@Color.describe(i8* %this)")
	assertContains(t, ir, "call i8* @Color.describe(")
}

func TestEnumGetterDecl(t *testing.T) {
	ir := generateIR(t, `
		enum Color { Red, Green, Blue,
			get opposite Color {
				match this {
					Color.Red => { return Color.Green; },
					Color.Green => { return Color.Blue; },
					Color.Blue => { return Color.Red; },
				}
			}
		}
		main() { Color c = Color.Red.opposite; }
	`)
	assertContains(t, ir, "define i32 @Color.opposite(i8* %this)")
	assertContains(t, ir, "call i32 @Color.opposite(")
}

func TestEnumMethodOnDataEnum(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Point,
			is_point(this) bool {
				match this {
					Shape.Point => { return true; },
					_ => { return false; },
				}
			}
		}
		main() { bool b = Shape.Point.is_point(); }
	`)
	assertContains(t, ir, "define i1 @Shape.is_point(i8* %this)")
	assertContains(t, ir, "call i1 @Shape.is_point(")
}

func TestEnumMethodCallsMethod(t *testing.T) {
	ir := generateIR(t, `
		enum Level { Low, High,
			rank(this) int {
				match this {
					Level.Low => { return 1; },
					Level.High => { return 2; },
				}
			}
			gt(this, Level other) bool {
				return this.rank() > other.rank();
			}
		}
		main() { bool b = Level.High.gt(Level.Low); }
	`)
	// Both methods declared with i8* receiver
	assertContains(t, ir, "@Level.rank(i8* %this)")
	assertContains(t, ir, "@Level.gt(i8* %this, i32 %other)")
}

func TestEnumMethodVoid(t *testing.T) {
	ir := generateIR(t, `
		enum State { On, Off,
			log(this) { print_line("x"); }
		}
		main() { State.On.log(); }
	`)
	assertContains(t, ir, "define void @State.log(i8* %this)")
}

func TestEnumGetterOnDataEnum(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Point,
			get has_area bool {
				match this {
					Shape.Circle(r) => { return true; },
					Shape.Point => { return false; },
				}
			}
		}
		main() { bool b = Shape.Circle(radius: 1.0).has_area; }
	`)
	assertContains(t, ir, "define i1 @Shape.has_area(i8* %this)")
	assertContains(t, ir, "call i1 @Shape.has_area(")
}

func TestIsDestructureEnumCodegen(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Point }
		main() {
			Shape s = Shape.Circle(radius: 5.0);
			if s is Circle(r) {
				print_line("{r}");
			}
		}
	`)
	// Should generate tag comparison for the enum variant check
	assertContains(t, ir, "icmp eq i32")
	// Should have the destructure blocks
	assertContains(t, ir, "isdestr.then")
	assertContains(t, ir, "isdestr.end")
}

func TestIsDestructureNamedTypeCodegen(t *testing.T) {
	ir := generateIR(t, `
		type Animal { string name; speak(this) string `+"`"+`abstract; }
		type Dog is Animal { string breed; speak(this) string { return "woof"; } }
		main() {
			Animal a = Dog(name: "Rex", breed: "Lab");
			if a is Dog(n, b) {
				print_line(n);
			}
		}
	`)
	// Should generate RTTI type check
	assertContains(t, ir, "call i32 @promise_type_is(")
	// Should have the destructure blocks
	assertContains(t, ir, "isdestr.then")
	assertContains(t, ir, "isdestr.end")
}

func TestIsDestructureElseCodegen(t *testing.T) {
	ir := generateIR(t, `
		enum Opt { Some(int v), None }
		main() {
			Opt o = Opt.None;
			if o is Some(v) {
				print_line("{v}");
			} else {
				print_line("none");
			}
		}
	`)
	assertContains(t, ir, "isdestr.then")
	assertContains(t, ir, "isdestr.else")
	assertContains(t, ir, "isdestr.end")
}

func TestIsDestructureUnderscoreCodegen(t *testing.T) {
	ir := generateIR(t, `
		enum Pair { V(int a, int b) }
		main() {
			Pair p = Pair.V(a: 1, b: 2);
			if p is V(_, y) {
				print_line("{y}");
			}
		}
	`)
	assertContains(t, ir, "isdestr.then")
	// Should still produce the tag check
	assertContains(t, ir, "icmp eq i32")
}

func TestIsDestructureAsExprCodegen(t *testing.T) {
	// When used as a plain expression (not if condition), should just produce the bool check
	ir := generateIR(t, `
		enum Opt { Some(int v), None }
		main() {
			Opt o = Opt.Some(v: 42);
			bool b = o is Some(x);
			if b { print_line("yes"); }
		}
	`)
	// Should NOT have isdestr blocks (handled by genIsDestructurePattern, not genIfDestructureIsStmt)
	assertNotContains(t, ir, "isdestr.then")
	// But should still have the tag comparison
	assertContains(t, ir, "icmp eq i32")
}

// T1012: `if x is V(field)` destructuring a heap payload of a DROPPABLE enum
// must deep-clone the payload and register a drop for the binding, so an escaped
// binding (return / store-to-outer) owns an independent copy — otherwise it
// aliases the subject's payload and dangles when the subject is dropped (UAF).
func TestT1012IfIsDestructureHeapFieldDupsOnDroppableEnum(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		make() string {
			Msg m = Msg.Text(body: "a" + "b");
			if m is Text(body) { return body; }
			return "";
		}
		main() { s := make(); }
	`)
	fn := extractFunction(ir, "__user.make")
	// The heap payload is dup'd via cloneResolvedValue (string clone block).
	assertContains(t, fn, "strdup.copy")
	// The binding gets a drop flag registered (dropped on fall-through, cleared
	// on move at the return site).
	assertContains(t, fn, "body.dropflag")
}

// T1012 negative control: an int payload binding must NOT be cloned — value/
// numeric payloads stay zero-copy (criterion #3).
func TestT1012IfIsDestructureNumericFieldNoDup(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		grab() int {
			Msg m = Msg.Code(n: 7);
			if m is Code(n) { return n; }
			return 0;
		}
		main() { x := grab(); }
	`)
	fn := extractFunction(ir, "__user.grab")
	assertContains(t, fn, "icmp eq i32") // sanity: we extracted the real function body
	assertNotContains(t, fn, "strdup.copy")
	assertNotContains(t, fn, "n.dropflag")
}

// T1178: a fixed-Array[heap-user] variant payload escaping an `if is` destructure
// must be deep-cloned EXACTLY ONCE. T1176 added an arrayElemNeedsEscapeDup branch to BOTH
// setDupFlagsForFieldAccess (the sink) and dupHeapFieldForEscape (the read side).
// For a variant payload the sink already clones via dupBorrowedHeapUserPayload, so
// letting the read-side dupHeapFieldForEscape array branch ALSO fire produced a
// SECOND element-wise clone whose elements were never dropped (leak). The fix skips
// the read-side dup for the array shape (genIdentExpr gates on !arrayElemNeedsEscapeDup).
// The presence-only T1171/T1176 tests above (heapdup.copy present, >=2 insertvalue)
// pass under BOTH single- and double-clone, so they never caught the regression —
// this test pins the EXACT clone count: one per element (2 for a 2-elem array),
// not two. A double-clone re-emits the array-aggregate insertvalues, so a count of
// 4 (or any value != 2) fails here.
func TestT1178VariantPayloadArrayEscapeSingleClone(t *testing.T) {
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
	assertContains(t, fn, "heapdup.copy")
	// Exactly one insertvalue per element rebuilds the SINGLE cloned aggregate.
	// A double-clone (the T1178 regression) emits 4.
	if n := strings.Count(fn, "insertvalue [2 x { i8*, i8* }]"); n != 2 {
		t.Fatalf("expected exactly 2 array-aggregate insertvalue (single element-wise clone), got %d (double-clone = T1178 regression)\n%s", n, fn)
	}
}

// T1178 (match parity): the same single-clone invariant for the `match` escape
// path — both `if is` and `match` populate matchBorrowedIdents and route through
// the same genIdentExpr read-side gate + dupBorrowedHeapUserPayload sink. Exactly
// one clone per element, not two.
func TestT1178VariantPayloadArrayEscapeMatchSingleClone(t *testing.T) {
	ir := generateIR(t, `
		type Row { string name; }
		enum Box { Some(Row[2] value), Empty }
		esc() Row[2] {
			Box b = Box.Some(value: [Row(name: "a" + "b"), Row(name: "c" + "d")]);
			match b {
				Some(value) => { return value; },
				Empty => { return [Row(name: "x"), Row(name: "y")]; },
			}
		}
		main() { r := esc(); }
	`)
	fn := extractFunction(ir, "__user.esc")
	assertContains(t, fn, "heapdup.copy")
	if n := strings.Count(fn, "insertvalue [2 x { i8*, i8* }]"); n != 2 {
		t.Fatalf("expected exactly 2 array-aggregate insertvalue (single element-wise clone), got %d (double-clone = T1178 regression)\n%s", n, fn)
	}
}

// T1169: `if x is Subtype(field)` destructure of a HEAP SUBTYPE field (the
// class/`is`-parent form, not the enum variant form). Sibling of T1012 — a
// droppable heap-typed subtype field (string) that escapes the narrowing scope
// must be deep-cloned (via cloneResolvedValue → dupString → strdup.copy) with a
// per-binding drop flag, so it survives the subject's drop at scope exit.
func TestT1169IfIsDestructureNamedHeapFieldDupsOnDroppableSubtype(t *testing.T) {
	ir := generateIR(t, `
		type Shape { }
		type Named is Shape { string label; }
		make() string {
			Shape s = Named(label: "a" + "b");
			if s is Named(label) { return label; }
			return "";
		}
		main() { s := make(); }
	`)
	fn := extractFunction(ir, "__user.make")
	assertContains(t, fn, "strdup.copy")
	assertContains(t, fn, "label.dropflag")
}

// T1169 negative control: an int subtype field binding must NOT be cloned —
// value/numeric fields stay zero-copy (the untouched value/typeNeedsMatchDup gate).
func TestT1169IfIsDestructureNamedNumericFieldNoDup(t *testing.T) {
	ir := generateIR(t, `
		type Shape { }
		type Named is Shape { int n; }
		grab() int {
			Shape s = Named(n: 7);
			if s is Named(n) { return n; }
			return 0;
		}
		main() { x := grab(); }
	`)
	fn := extractFunction(ir, "__user.grab")
	assertContains(t, fn, "ret i64") // sanity: we extracted the real function body
	assertNotContains(t, fn, "strdup.copy")
	assertNotContains(t, fn, "n.dropflag")
}

func TestMatchExpressionPattern(t *testing.T) {
	ir := generateIR(t, `
		test() int {
			int n = 15;
			return match true {
				n % 15 == 0 => 1,
				n % 3 == 0 => 2,
				_ => 0,
			};
		}
		main() { }
	`)
	// Expression patterns compile to comparisons like literal patterns
	assertContains(t, ir, "icmp eq")
	assertContains(t, ir, "match.arm")
	assertContains(t, ir, "match.next")
	// The modulo operation should appear in the IR
	assertContains(t, ir, "srem")
}

// B0237: Constructor temps stored into enum variant data should be claimed.
func TestConstructorTempClaimedInEnumVariant(t *testing.T) {
	ir := generateIR(t, `
		type Wrapper { int x; }
		enum Slot { Empty, Used(Wrapper value) }
		main() {
			Slot s = Slot.Used(value: Wrapper(x: 42));
		}
	`)
	// Enum variant construction claims the heap temp.
	assertContains(t, ir, "heap.claim")
}

// B0280: Map literal values with drop flags must have flags cleared after []=.
// Without this, moved values are double-dropped at scope exit (use-after-free).
func TestMapLitClearsDropFlagOnEnumValue(t *testing.T) {
	ir := generateIR(t, `
		enum Wrapper { Val(string s); }
		main() {
			Wrapper w = Wrapper.Val(s: "hello");
			map[string, Wrapper] m = { "key": w };
		}
	`)
	// After the []= call, w's drop flag must be cleared (store i1 false)
	assertContains(t, ir, "w.dropflag")
	// The []= call should be followed by clearing w's drop flag
	assertContains(t, ir, "store i1 false, i1* %w.dropflag")
}

// T0559 + T0545 + T0616: Vector[Mutex|Task|MutexGuard].clone() via generic
// indirection is now rejected at sema by T0616 (deferred cloneability
// requirements propagated across generic call edges). The codegen-side
// backstop (length-guarded vecclone.unsup.panic, isSingleOwnerHandleType
// early-returns in cloneHeapElement/dupHeapValue) remains as defense-in-depth
// but is unreachable for well-formed user code. The IR-shape assertions that
// formerly pinned the backstop's emission have been removed; the sema-error
// behavior is verified by TestT0616_VectorCloneInGeneric{Task,Mutex,MutexGuard}Error
// in the sema package.

// B0281: Enum ctor temps used as map literal values must be claimed.
// Without the fix, the enum temp is dropped at statement end, double-freeing
// inner data (both the temp and the map's Slot share the same pointers).
func TestEnumCtorTempClaimedInMapLiteral(t *testing.T) {
	ir := generateIR(t, `
		enum Val { Txt(string s), Num(int n) }
		main() {
			map[string, Val] m = { "a": Val.Txt(s: "hello") };
		}
	`)
	// The enum ctor temp drop flag should be cleared (stored i1 false) BEFORE
	// statement-end cleanup. No enum.ctor.drop block should fire for this temp.
	assertNotContains(t, ir, "enum.ctor.drop")
}

// B0281: Enum ctor temps used as vector literal elements must be claimed.
func TestEnumCtorTempClaimedInVectorLiteral(t *testing.T) {
	ir := generateIR(t, `
		enum Val { Txt(string s), Num(int n) }
		main() {
			Val[] v = [Val.Txt(s: "hello"), Val.Num(n: 42)];
		}
	`)
	assertNotContains(t, ir, "enum.ctor.drop")
}

// B0290: When a heap type with a vector of droppable enums is dup'd via
// dupHeapValue → dupHeapValueFields → emitVectorElementCloneLoop, enum elements
// without clone methods should be dup'd in place (switch on tag, dup droppable fields).
func TestDupEnumElementInPlaceForVectorOfEnum(t *testing.T) {
	ir := generateIR(t, `
		enum Slot {
			Empty,
			Used(string key, string value),
		}
		type Container {
			Slot[] buckets;
			drop(~this) {}
		}
		test() {
			Container[] v = [Container(buckets: [Slot.Used(key: "a", value: "b")])];
			Container[] v2 = v.clone();
		}
	`)
	// Vector[Container].clone() → emitVectorElementCloneLoop → cloneHeapElement →
	// dupHeapValue → dupHeapValueFields → for Slot[] field → dupVector +
	// emitVectorElementCloneLoop → dupEnumElementInPlace for Slot elements.
	assertContains(t, ir, "enumdup.Used")
	assertContains(t, ir, "enumdup.done")
}

func TestParenThisEnumMethodNoExtractFromPtr(t *testing.T) {
	ir := generateIR(t, `
		enum T0613Color {
			Red, Green, Blue,
			describe(this) string {
				match this {
					T0613Color.Red => { return "red"; },
					T0613Color.Green => { return "green"; },
					T0613Color.Blue => { return "blue"; },
				}
			}
			describe_via(this) string { return (this).describe(); }
			get opposite T0613Color {
				match this {
					T0613Color.Red => { return T0613Color.Green; },
					T0613Color.Green => { return T0613Color.Blue; },
					T0613Color.Blue => { return T0613Color.Red; },
				}
			}
			opposite_via(this) T0613Color { return (this).opposite; }
		}
		main() {
			s := T0613Color.Red.describe_via();
			o := T0613Color.Red.opposite_via();
		}
	`)
	assertNotContains(t, ir, "extractvalue i8*")
	if extractFunction(ir, "T0613Color.describe_via") == "" {
		t.Fatal("expected T0613Color.describe_via in IR")
	}
}

// T0993: a class `match { Circle c => }` type-pattern arm must dispatch on the
// runtime subtype via the promise_type_is RTTI machinery — not emit nothing and
// silently fall through to the wildcard (the merged T0992 miscompilation).
func TestMatchTypePatternEmitsRTTI(t *testing.T) {
	ir := generateIR(t, `
		type Shape { area(this) f64 => 0.0; }
		type Circle is Shape { f64 r; }
		type Square is Shape { f64 s; }
		describe(Shape sh) string {
			return match sh {
				Circle c => "circle",
				Square q => "square",
				_ => "other",
			};
		}
		main() { Shape s = Circle(r: 1.0); d := describe(s); print_line(d); }
	`)
	// Each class type-pattern arm lowers to an RTTI subtype check (Circle + Square).
	if n := strings.Count(ir, "call i32 @promise_type_is"); n < 2 {
		t.Errorf("expected >=2 promise_type_is calls for type-pattern arms, got %d\n%s", n, ir)
	}
}

// T0993: a class type-pattern arm whose runtime type is the tested subtype must
// not be a no-op — the arm body's value must reach the match result.
func TestMatchTypePatternBindsSubtype(t *testing.T) {
	ir := generateIR(t, `
		type Shape { area(this) f64 => 0.0; }
		type Circle is Shape { f64 r; area(this) f64 => this.r; }
		describe(Shape sh) f64 {
			return match sh {
				Circle c => c.r,
				_ => -1.0,
			};
		}
		main() { Shape s = Circle(r: 2.0); d := describe(s); }
	`)
	assertContains(t, ir, "call i32 @promise_type_is")
}

// T0993: a non-destructive enum variant narrowing (`if x is V { x.named }`)
// reads the named payload via a variant-data GEP+load. The function must
// compile (no codegen panic) and the field value must be produced.
func TestEnumNarrowVariantFieldRead(t *testing.T) {
	ir := generateIR(t, `
		enum Shape { Circle(f64 radius), Rectangle(f64 width, f64 height) }
		main() {
			Shape s = Shape.Circle(radius: 5.0);
			f64 out = 0.0;
			if s is Circle {
				out = s.radius;
			}
		}
	`)
	// The narrowed read lowers to a variant-data field load of a double.
	assertContains(t, ir, "load double")
	assertContains(t, ir, "getelementptr")
}

// T1005: `if this is Variant { this.field }` narrows the `this` receiver inside
// an enum method. Exercises the previously-dead enumThisSubject branch in
// genNarrowedVariantField: the i8* receiver loads to a by-value enum and the
// variant-data field GEP is emitted. Must compile without panicking.
func TestEnumNarrowVariantFieldReadThis(t *testing.T) {
	ir := generateIR(t, `
		enum Shape {
			Circle(f64 radius),
			Rect(f64 w, f64 h),
			area(this) f64 {
				if this is Circle {
					return 3.0 * this.radius * this.radius;
				}
				return 0.0;
			}
		}
		main() {
			Shape s = Shape.Circle(radius: 5.0);
			f64 out = s.area();
		}
	`)
	fn := extractFunction(ir, "Shape.area")
	if fn == "" {
		t.Fatal("expected Shape.area in IR")
	}
	// The narrowed `this.radius` read lowers to a variant-data field GEP + load.
	assertContains(t, fn, "getelementptr")
	assertContains(t, fn, "load double")
}

// T1011: a narrowed heap (string) variant field that ESCAPES the narrowing scope
// (here: returned) must be cloned, not aliased — otherwise the subject's synth
// enum drop frees the payload while the returned value still points into it
// (use-after-free / double-free). The escape-dup must emit a strdup.
func TestEnumNarrowVariantStringFieldEscapeDups(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		grab() string {
			Msg m = Msg.Text(body: "a");
			if m is Text { return m.body; }
			return "";
		}
		main() { string s = grab(); }
	`)
	fn := extractFunction(ir, "__user.grab")
	if fn == "" {
		t.Fatal("expected __user.grab in IR")
	}
	// dupString emits a strdup.copy block + promise_string_new at the return site.
	assertContains(t, fn, "strdup.copy")
	assertContains(t, fn, "promise_string_new")
}

// T1011 (no-regression): a purely in-scope read of a narrowed heap variant field
// stays a zero-copy borrow — no dup flag is set, so genNarrowedVariantField must
// NOT emit a strdup for the read.
func TestEnumNarrowVariantStringFieldInScopeNoDup(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		probe() int {
			Msg m = Msg.Text(body: "a");
			int n = 0;
			if m is Text { n = m.body.len; }
			return n;
		}
		main() { int x = probe(); }
	`)
	fn := extractFunction(ir, "__user.probe")
	if fn == "" {
		t.Fatal("expected __user.probe in IR")
	}
	assertNotContains(t, fn, "strdup.copy")
}

// T1011: a narrowed heap (string) variant field escaping into a CONSTRUCTOR field
// of a droppable type must be cloned. maybeEnableDupForConstructorArg routes the
// narrowed-field arg through the same dup-on-escape path as a struct field
// (narrowedVariantFieldDroppable matched=true, droppable=true).
func TestEnumNarrowVariantStringFieldCtorEscapeDups(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		type Sink { string held; drop(~this) {} }
		grab() Sink {
			Msg m = Msg.Text(body: "a");
			if m is Text { return Sink(held: m.body); }
			return Sink(held: "");
		}
		main() { Sink s = grab(); }
	`)
	fn := extractFunction(ir, "__user.grab")
	if fn == "" {
		t.Fatal("expected __user.grab in IR")
	}
	assertContains(t, fn, "strdup.copy")
}

// T1011: a narrowed heap (string) variant field passed to a consuming `string move`
// param must be cloned. maybeEnableDupForMutRefArg's narrowed-field branch sets
// the dup-on-escape flag — the callee takes ownership, so the value must not
// alias the subject the synth enum drop frees at scope exit.
func TestEnumNarrowVariantStringFieldConsumingParamDups(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		take(string move s) int { return s.len; }
		grab() int {
			Msg m = Msg.Text(body: "a");
			if m is Text { return take(m.body); }
			return 0;
		}
		main() { int n = grab(); }
	`)
	fn := extractFunction(ir, "__user.grab")
	if fn == "" {
		t.Fatal("expected __user.grab in IR")
	}
	assertContains(t, fn, "strdup.copy")
}

// T1011: binding a narrowed heap (string) variant field to a new variable
// (`b := m.body`) takes ownership, so isStringFieldDup recognizes the narrowed
// field (its narrowedVariantFieldDroppable branch) and the binding keeps its drop
// flag while genNarrowedVariantField clones the payload — without the clone the
// binding's drop would double-free with the subject's synth enum drop.
func TestEnumNarrowVariantStringFieldBoundCopyDups(t *testing.T) {
	ir := generateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		grab() int {
			Msg m = Msg.Text(body: "a");
			int r = 0;
			if m is Text { b := m.body; r = b.len; }
			return r;
		}
		main() { int n = grab(); }
	`)
	fn := extractFunction(ir, "__user.grab")
	if fn == "" {
		t.Fatal("expected __user.grab in IR")
	}
	assertContains(t, fn, "strdup.copy")
}

// T1011 (no-regression): a non-droppable enum (no heap payload in any variant)
// narrowed to a variant whose non-heap field escapes must NOT clone —
// narrowedVariantFieldDroppable reports droppable=false (enumTargetDroppable is
// false), so the consumer skips the dup. Cloning a field the synth drop never
// frees would leak. The int field copies cleanly into the constructor.
func TestEnumNarrowVariantNonDroppableFieldNoDup(t *testing.T) {
	ir := generateIR(t, `
		enum Flag { On(int code), Off }
		type Box { int v; }
		grab() Box {
			Flag f = Flag.On(code: 3);
			if f is On { return Box(v: f.code); }
			return Box(v: 0);
		}
		main() { Box b = grab(); }
	`)
	fn := extractFunction(ir, "__user.grab")
	if fn == "" {
		t.Fatal("expected __user.grab in IR")
	}
	assertNotContains(t, fn, "strdup.copy")
}

// T0993: `match this { Subtype c => }` over a class hierarchy. genThisExpr
// returns the raw i8* instance pointer; genValueMatch must normalize it into the
// {vtable, instance} value struct before the type-pattern arm extracts the
// instance for RTTI — otherwise it emits `extractvalue` on an i8* (invalid IR
// that fails `opt`). This guards that normalization.
func TestMatchTypePatternThisReceiver(t *testing.T) {
	ir := generateIR(t, `
		type Shape {
			describe(this) string => match this {
				Circle c => "circle",
				Square s => "square",
				_ => "other",
			};
		}
		type Circle is Shape { f64 r; }
		type Square is Shape { f64 s; }
		main() { Shape a = Circle(r: 1.0); d := a.describe(); }
	`)
	assertContains(t, ir, "call i32 @promise_type_is")
	// No extractvalue on an i8* — the subject was wrapped into a value struct.
	if strings.Contains(ir, "extractvalue i8* ") {
		t.Errorf("expected no extractvalue on i8* (this subject must be normalized)\n%s", ir)
	}
}
