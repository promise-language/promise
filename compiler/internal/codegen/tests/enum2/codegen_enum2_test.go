package enum2

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// === Enum Tests ===

func TestDropEnumVariantVectorElements(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "define void @Value.drop(")
	codegentest.AssertContains(t, ir, "enum.drop.List")
	codegentest.AssertContains(t, ir, "vecdrop.head") // element drop loop in variant field drop
}

// T0567: Explicit drop(~this) on enum — user-defined drop is emitted and called
func TestDropExplicitEnumMethod(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "define void @Color.drop(")
	codegentest.AssertContains(t, ir, "call void @Color.drop(")
	codegentest.AssertContains(t, ir, "enum.drop.call")
}

// T0604: Explicit drop(~this) on enum with droppable variant fields —
// variant field cleanup (switch on tag, drop per-variant) is emitted after the user body.
func TestDropExplicitEnumVariantFieldCleanup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "define void @Container.drop(")
	codegentest.AssertContains(t, ir, "enum.drop.field.Data")
	codegentest.AssertContains(t, ir, "enum.drop.field.done")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop(")
}

// T1108: When a variable with a drop binding is moved into an enum-constructor
// temp that is passed to a BORROW param, the moved-in payload's only owner is
// the enum temp (the source ident's drop flag was cleared at the move). The
// borrow callee does not consume it, so the caller MUST drop the enum temp at
// statement end to free the payload — otherwise it leaks. (Was B0252's
// TestEnumCtorTempSkippedWhenMovedDroppable, which encoded that leak.)
func TestEnumCtorTempMovedDroppableBorrowDrops(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "define void @Holder.drop")
	// The borrowed enum ctor temp must be dropped at statement end so the
	// moved-in Resource is freed (zero-leak policy).
	codegentest.AssertContains(t, ir, "enum.ctor.drop")
}

// T1108: Non-ident expressions (e.g., function calls) returning droppable values
// moved into an enum-constructor temp that is then passed to a BORROW param must
// be dropped at statement end. The call result's only owner is the enum temp;
// the borrow callee does not consume it, so skipping the drop would leak the
// result. (Was B0286's TestEnumCtorTempSkippedForNonIdentDroppableArg — the IR
// test that masked the leak it introduced.)
func TestEnumCtorTempNonIdentDroppableBorrowDrops(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "define void @Holder.drop")
	codegentest.AssertContains(t, ir, "enum.ctor.drop")
}

// T1108: Enum variant with a non-ident arg of a synth-drop type (contains a
// string field) passed to a BORROW param must likewise be dropped at statement
// end. (Was B0286's TestEnumCtorTempSkippedForNonIdentSynthDropArg.)
func TestEnumCtorTempNonIdentSynthDropBorrowDrops(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "define void @Wrapper.drop")
	codegentest.AssertContains(t, ir, "enum.ctor.drop")
}

// B0293: Enum variable reassignment must clear enumCtorTemps to prevent double-drop.
// Without the fix, the enum ctor temp drop fires at statement end AND the variable's
// scope-exit drop fires, causing use-after-free on the variant's heap data.
func TestEnumCtorTempClearedOnReassign(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper { string name; int value; }
		enum Container[T] { Holding(T item), Empty, }
		test() {
			c := Container[Wrapper].Holding(Wrapper(name: "first", value: 1));
			c = Container[Wrapper].Holding(Wrapper(name: "second", value: 2));
		}
	`)
	// The reassignment path should NOT have enum.ctor.drop blocks —
	// ownership transferred to the variable, ctor temps must be cleared.
	codegentest.AssertNotContains(t, ir, "enum.ctor.drop")
}

func TestBuildMatchPhiMixedArms(t *testing.T) {
	// Match expression where some arms produce values and at least one arm
	// has an early return. buildMatchPhi must handle missing predecessors
	// by inserting null placeholders for arms that branch to merge without values.
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "phi i64")
	codegentest.AssertContains(t, ir, "match.end")
}

func TestBuildMatchPhiStatementOnly(t *testing.T) {
	// Match used as a statement (no arm produces a value) — no PHI needed
	ir := codegentest.GenerateIR(t, `
		test(int n) {
			match n {
				1 => { int x = 10; },
				_ => { int y = 20; },
			};
		}
		main() { }
	`)
	// Should have match arms but the merge block shouldn't have a PHI
	codegentest.AssertContains(t, ir, "match.arm")
	codegentest.AssertContains(t, ir, "match.end")
}

func TestEnumMatchPhiWithEarlyReturn(t *testing.T) {
	// Enum match where one arm returns early (doesn't branch to merge).
	// buildMatchPhi must skip non-merging arms to avoid PHI predecessor mismatch.
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "phi i64")
}

// T0421: Fieldless enum interpolation emits switch on tag → variant name string.
func TestStringInterpolationEnumFieldless(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Color { Red, Green, Blue }
		main() {
			Color c = Color.Green;
			string s = "{c}";
		}
	`)
	codegentest.AssertContains(t, ir, "switch i32")
	codegentest.AssertContains(t, ir, "enum.interp.Red")
	codegentest.AssertContains(t, ir, "enum.interp.Green")
	codegentest.AssertContains(t, ir, "enum.interp.Blue")
	codegentest.AssertContains(t, ir, "enum.interp.merge")
	codegentest.AssertContains(t, ir, `"Red"`)
	codegentest.AssertContains(t, ir, `"Green"`)
	codegentest.AssertContains(t, ir, `"Blue"`)
}

// T0421: Data enum interpolation extracts tag from field 0 and emits switch.
func TestStringInterpolationEnumData(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Shape { Circle(f64 radius), Rect(f64 w, f64 h) }
		main() {
			Shape s = Shape.Circle(1.0);
			string x = "{s}";
		}
	`)
	codegentest.AssertContains(t, ir, "switch i32")
	codegentest.AssertContains(t, ir, "enum.interp.Circle")
	codegentest.AssertContains(t, ir, "enum.interp.Rect")
	codegentest.AssertContains(t, ir, `"Circle"`)
	codegentest.AssertContains(t, ir, `"Rect"`)
}

func TestNegatedNarrowingCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string? cc = "hello";
			if !cc {
				string s = "none";
			} else {
				string s = cc;
			}
		}
	`)
	codegentest.AssertContains(t, ir, "narrow.then")
	codegentest.AssertContains(t, ir, "narrow.else")
}

func TestCompoundNarrowingCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int? a = 1;
			string? b = "hi";
			if a && b {
				int x = a;
				string y = b;
			}
		}
	`)
	codegentest.AssertContains(t, ir, "narrow.check")
	codegentest.AssertContains(t, ir, "narrow.then")
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
	ir := codegentest.GenerateIR(t, `
		make_adder(int x)() -> int { return move || -> x + 1; }
		caller() {
			(() -> int, int) t = (make_adder(6), 1);
			(g, n) := t;
			g();
		}
	`)
	// The destructured closure `g` gets a bindingFreeEnv: null-check then either
	// call the per-closure env-drop fn (deep) or pal_free (shallow).
	codegentest.AssertContains(t, ir, "env.free")
	codegentest.AssertContains(t, ir, "env.shallow_free")
	codegentest.AssertContains(t, ir, "call void @pal_free(")
	// The source tuple's drop flag is cleared after the destructure so its
	// scope-exit field walk does not double-free the moved-out env.
	codegentest.AssertContains(t, ir, "store i1 false, i1* %t.dropflag")
}

func TestValueTypeDestructureIsPattern(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "bitcast %promise_Point_i* @promise_rtti_Point to i8*")
	// Field extraction uses extractvalue on the value struct (fields at index 1, 2)
	codegentest.AssertContains(t, ir, "extractvalue %promise_Point_v")
}

// --- Failable getter codegen ---

func TestMatchMixedVoidAndValueArms(t *testing.T) {
	// Match where some arms produce a value and some call a void function.
	// buildMatchPhi must filter void-typed values before constructing the PHI.
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "phi i64")
	codegentest.AssertContains(t, ir, "match.end")
}

func TestMatchAllVoidArms(t *testing.T) {
	// Match used as statement where all arms are void (no arm produces a value).
	// buildMatchPhi should return nil (no PHI node needed).
	ir := codegentest.GenerateIR(t, `
		test(int n) {
			match n {
				1 => { print_line("one"); },
				2 => { print_line("two"); },
				_ => { print_line("other"); },
			};
		}
		main() { }
	`)
	codegentest.AssertContains(t, ir, "match.arm")
	codegentest.AssertContains(t, ir, "match.end")
}

// B0126: match with block body containing if/else as expression.
// genBlockValue must capture the if/else result via genIfStmtValue.
func TestMatchBlockIfElseExpr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "phi")
	codegentest.AssertContains(t, ir, "match.end")
	codegentest.AssertContains(t, ir, "if.then")
	codegentest.AssertContains(t, ir, "if.end")
}

// B0126: single wildcard arm with block body containing if/else-if chain.
func TestMatchBlockIfElseIfChain(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "phi")
	codegentest.AssertContains(t, ir, "if.then")
	codegentest.AssertContains(t, ir, "if.end")
}

// B0135: if/else void inside a match block body must not produce phi void.
func TestMatchBlockIfElseVoidNoPhi(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "if.then")
	codegentest.AssertNotContains(t, ir, "phi void")
}

func TestEnumMethodDecl(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "@Color.describe(i8* %this)")
	codegentest.AssertContains(t, ir, "call i8* @Color.describe(")
}

func TestEnumGetterDecl(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "define i32 @Color.opposite(i8* %this)")
	codegentest.AssertContains(t, ir, "call i32 @Color.opposite(")
}

func TestEnumMethodOnDataEnum(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "define i1 @Shape.is_point(i8* %this)")
	codegentest.AssertContains(t, ir, "call i1 @Shape.is_point(")
}

func TestEnumMethodCallsMethod(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "@Level.rank(i8* %this)")
	codegentest.AssertContains(t, ir, "@Level.gt(i8* %this, i32 %other)")
}

func TestEnumMethodVoid(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum State { On, Off,
			log(this) { print_line("x"); }
		}
		main() { State.On.log(); }
	`)
	codegentest.AssertContains(t, ir, "define void @State.log(i8* %this)")
}

func TestEnumGetterOnDataEnum(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "define i1 @Shape.has_area(i8* %this)")
	codegentest.AssertContains(t, ir, "call i1 @Shape.has_area(")
}

func TestIsDestructureEnumCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Shape { Circle(f64 radius), Point }
		main() {
			Shape s = Shape.Circle(radius: 5.0);
			if s is Circle(r) {
				print_line("{r}");
			}
		}
	`)
	// Should generate tag comparison for the enum variant check
	codegentest.AssertContains(t, ir, "icmp eq i32")
	// Should have the destructure blocks
	codegentest.AssertContains(t, ir, "isdestr.then")
	codegentest.AssertContains(t, ir, "isdestr.end")
}

func TestIsDestructureNamedTypeCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "call i32 @promise_type_is(")
	// Should have the destructure blocks
	codegentest.AssertContains(t, ir, "isdestr.then")
	codegentest.AssertContains(t, ir, "isdestr.end")
}

func TestIsDestructureElseCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "isdestr.then")
	codegentest.AssertContains(t, ir, "isdestr.else")
	codegentest.AssertContains(t, ir, "isdestr.end")
}

func TestIsDestructureUnderscoreCodegen(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Pair { V(int a, int b) }
		main() {
			Pair p = Pair.V(a: 1, b: 2);
			if p is V(_, y) {
				print_line("{y}");
			}
		}
	`)
	codegentest.AssertContains(t, ir, "isdestr.then")
	// Should still produce the tag check
	codegentest.AssertContains(t, ir, "icmp eq i32")
}

func TestIsDestructureAsExprCodegen(t *testing.T) {
	// When used as a plain expression (not if condition), should just produce the bool check
	ir := codegentest.GenerateIR(t, `
		enum Opt { Some(int v), None }
		main() {
			Opt o = Opt.Some(v: 42);
			bool b = o is Some(x);
			if b { print_line("yes"); }
		}
	`)
	// Should NOT have isdestr blocks (handled by genIsDestructurePattern, not genIfDestructureIsStmt)
	codegentest.AssertNotContains(t, ir, "isdestr.then")
	// But should still have the tag comparison
	codegentest.AssertContains(t, ir, "icmp eq i32")
}

// T1012: `if x is V(field)` destructuring a heap payload of a DROPPABLE enum
// must deep-clone the payload and register a drop for the binding, so an escaped
// binding (return / store-to-outer) owns an independent copy — otherwise it
// aliases the subject's payload and dangles when the subject is dropped (UAF).
func TestT1012IfIsDestructureHeapFieldDupsOnDroppableEnum(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		make() string {
			Msg m = Msg.Text(body: "a" + "b");
			if m is Text(body) { return body; }
			return "";
		}
		main() { s := make(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.make")
	// The heap payload is dup'd via cloneResolvedValue (string clone block).
	codegentest.AssertContains(t, fn, "strdup.copy")
	// The binding gets a drop flag registered (dropped on fall-through, cleared
	// on move at the return site).
	codegentest.AssertContains(t, fn, "body.dropflag")
}

// T1012 negative control: an int payload binding must NOT be cloned — value/
// numeric payloads stay zero-copy (criterion #3).
func TestT1012IfIsDestructureNumericFieldNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		grab() int {
			Msg m = Msg.Code(n: 7);
			if m is Code(n) { return n; }
			return 0;
		}
		main() { x := grab(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.grab")
	codegentest.AssertContains(t, fn, "icmp eq i32") // sanity: we extracted the real function body
	codegentest.AssertNotContains(t, fn, "strdup.copy")
	codegentest.AssertNotContains(t, fn, "n.dropflag")
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
	ir := codegentest.GenerateIR(t, `
		type Row { string name; }
		enum Box { Some(Row[2] value), Empty }
		esc() Row[2] {
			Box b = Box.Some(value: [Row(name: "a" + "b"), Row(name: "c" + "d")]);
			if b is Some(value) { return value; }
			return [Row(name: "x"), Row(name: "y")];
		}
		main() { r := esc(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.esc")
	codegentest.AssertContains(t, fn, "heapdup.copy")
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
	ir := codegentest.GenerateIR(t, `
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
	fn := codegentest.ExtractFunction(ir, "__user.esc")
	codegentest.AssertContains(t, fn, "heapdup.copy")
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
	ir := codegentest.GenerateIR(t, `
		type Shape { }
		type Named is Shape { string label; }
		make() string {
			Shape s = Named(label: "a" + "b");
			if s is Named(label) { return label; }
			return "";
		}
		main() { s := make(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.make")
	codegentest.AssertContains(t, fn, "strdup.copy")
	codegentest.AssertContains(t, fn, "label.dropflag")
}

// T1169 negative control: an int subtype field binding must NOT be cloned —
// value/numeric fields stay zero-copy (the untouched value/typeNeedsMatchDup gate).
func TestT1169IfIsDestructureNamedNumericFieldNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Shape { }
		type Named is Shape { int n; }
		grab() int {
			Shape s = Named(n: 7);
			if s is Named(n) { return n; }
			return 0;
		}
		main() { x := grab(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.grab")
	codegentest.AssertContains(t, fn, "ret i64") // sanity: we extracted the real function body
	codegentest.AssertNotContains(t, fn, "strdup.copy")
	codegentest.AssertNotContains(t, fn, "n.dropflag")
}

func TestMatchExpressionPattern(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "icmp eq")
	codegentest.AssertContains(t, ir, "match.arm")
	codegentest.AssertContains(t, ir, "match.next")
	// The modulo operation should appear in the IR
	codegentest.AssertContains(t, ir, "srem")
}

// B0237: Constructor temps stored into enum variant data should be claimed.
func TestConstructorTempClaimedInEnumVariant(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper { int x; }
		enum Slot { Empty, Used(Wrapper value) }
		main() {
			Slot s = Slot.Used(value: Wrapper(x: 42));
		}
	`)
	// Enum variant construction claims the heap temp.
	codegentest.AssertContains(t, ir, "heap.claim")
}

// B0280: Map literal values with drop flags must have flags cleared after []=.
// Without this, moved values are double-dropped at scope exit (use-after-free).
func TestMapLitClearsDropFlagOnEnumValue(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Wrapper { Val(string s); }
		main() {
			Wrapper w = Wrapper.Val(s: "hello");
			map[string, Wrapper] m = { "key": w };
		}
	`)
	// After the []= call, w's drop flag must be cleared (store i1 false)
	codegentest.AssertContains(t, ir, "w.dropflag")
	// The []= call should be followed by clearing w's drop flag
	codegentest.AssertContains(t, ir, "store i1 false, i1* %w.dropflag")
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
	ir := codegentest.GenerateIR(t, `
		enum Val { Txt(string s), Num(int n) }
		main() {
			map[string, Val] m = { "a": Val.Txt(s: "hello") };
		}
	`)
	// The enum ctor temp drop flag should be cleared (stored i1 false) BEFORE
	// statement-end cleanup. No enum.ctor.drop block should fire for this temp.
	codegentest.AssertNotContains(t, ir, "enum.ctor.drop")
}

// B0281: Enum ctor temps used as vector literal elements must be claimed.
func TestEnumCtorTempClaimedInVectorLiteral(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Val { Txt(string s), Num(int n) }
		main() {
			Val[] v = [Val.Txt(s: "hello"), Val.Num(n: 42)];
		}
	`)
	codegentest.AssertNotContains(t, ir, "enum.ctor.drop")
}

// B0290: When a heap type with a vector of droppable enums is dup'd via
// dupHeapValue → dupHeapValueFields → emitVectorElementCloneLoop, enum elements
// without clone methods should be dup'd in place (switch on tag, dup droppable fields).
func TestDupEnumElementInPlaceForVectorOfEnum(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "enumdup.Used")
	codegentest.AssertContains(t, ir, "enumdup.done")
}

func TestParenThisEnumMethodNoExtractFromPtr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertNotContains(t, ir, "extractvalue i8*")
	if codegentest.ExtractFunction(ir, "T0613Color.describe_via") == "" {
		t.Fatal("expected T0613Color.describe_via in IR")
	}
}

// T0993: a class `match { Circle c => }` type-pattern arm must dispatch on the
// runtime subtype via the promise_type_is RTTI machinery — not emit nothing and
// silently fall through to the wildcard (the merged T0992 miscompilation).
func TestMatchTypePatternEmitsRTTI(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "call i32 @promise_type_is")
}

// T0993: a non-destructive enum variant narrowing (`if x is V { x.named }`)
// reads the named payload via a variant-data GEP+load. The function must
// compile (no codegen panic) and the field value must be produced.
func TestEnumNarrowVariantFieldRead(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "load double")
	codegentest.AssertContains(t, ir, "getelementptr")
}

// T1005: `if this is Variant { this.field }` narrows the `this` receiver inside
// an enum method. Exercises the previously-dead enumThisSubject branch in
// genNarrowedVariantField: the i8* receiver loads to a by-value enum and the
// variant-data field GEP is emitted. Must compile without panicking.
func TestEnumNarrowVariantFieldReadThis(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	fn := codegentest.ExtractFunction(ir, "Shape.area")
	if fn == "" {
		t.Fatal("expected Shape.area in IR")
	}
	// The narrowed `this.radius` read lowers to a variant-data field GEP + load.
	codegentest.AssertContains(t, fn, "getelementptr")
	codegentest.AssertContains(t, fn, "load double")
}

// T1011: a narrowed heap (string) variant field that ESCAPES the narrowing scope
// (here: returned) must be cloned, not aliased — otherwise the subject's synth
// enum drop frees the payload while the returned value still points into it
// (use-after-free / double-free). The escape-dup must emit a strdup.
func TestEnumNarrowVariantStringFieldEscapeDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		grab() string {
			Msg m = Msg.Text(body: "a");
			if m is Text { return m.body; }
			return "";
		}
		main() { string s = grab(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.grab")
	if fn == "" {
		t.Fatal("expected __user.grab in IR")
	}
	// dupString emits a strdup.copy block + promise_string_new at the return site.
	codegentest.AssertContains(t, fn, "strdup.copy")
	codegentest.AssertContains(t, fn, "promise_string_new")
}

// T1011 (no-regression): a purely in-scope read of a narrowed heap variant field
// stays a zero-copy borrow — no dup flag is set, so genNarrowedVariantField must
// NOT emit a strdup for the read.
func TestEnumNarrowVariantStringFieldInScopeNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		probe() int {
			Msg m = Msg.Text(body: "a");
			int n = 0;
			if m is Text { n = m.body.len; }
			return n;
		}
		main() { int x = probe(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.probe")
	if fn == "" {
		t.Fatal("expected __user.probe in IR")
	}
	codegentest.AssertNotContains(t, fn, "strdup.copy")
}

// T1011: a narrowed heap (string) variant field escaping into a CONSTRUCTOR field
// of a droppable type must be cloned. maybeEnableDupForConstructorArg routes the
// narrowed-field arg through the same dup-on-escape path as a struct field
// (narrowedVariantFieldDroppable matched=true, droppable=true).
func TestEnumNarrowVariantStringFieldCtorEscapeDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		type Sink { string held; drop(~this) {} }
		grab() Sink {
			Msg m = Msg.Text(body: "a");
			if m is Text { return Sink(held: m.body); }
			return Sink(held: "");
		}
		main() { Sink s = grab(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.grab")
	if fn == "" {
		t.Fatal("expected __user.grab in IR")
	}
	codegentest.AssertContains(t, fn, "strdup.copy")
}

// T1011: a narrowed heap (string) variant field passed to a consuming `string move`
// param must be cloned. maybeEnableDupForMutRefArg's narrowed-field branch sets
// the dup-on-escape flag — the callee takes ownership, so the value must not
// alias the subject the synth enum drop frees at scope exit.
func TestEnumNarrowVariantStringFieldConsumingParamDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		take(string move s) int { return s.len; }
		grab() int {
			Msg m = Msg.Text(body: "a");
			if m is Text { return take(m.body); }
			return 0;
		}
		main() { int n = grab(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.grab")
	if fn == "" {
		t.Fatal("expected __user.grab in IR")
	}
	codegentest.AssertContains(t, fn, "strdup.copy")
}

// T1011: binding a narrowed heap (string) variant field to a new variable
// (`b := m.body`) takes ownership, so isStringFieldDup recognizes the narrowed
// field (its narrowedVariantFieldDroppable branch) and the binding keeps its drop
// flag while genNarrowedVariantField clones the payload — without the clone the
// binding's drop would double-free with the subject's synth enum drop.
func TestEnumNarrowVariantStringFieldBoundCopyDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Msg { Text(string body), Code(int n) }
		grab() int {
			Msg m = Msg.Text(body: "a");
			int r = 0;
			if m is Text { b := m.body; r = b.len; }
			return r;
		}
		main() { int n = grab(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.grab")
	if fn == "" {
		t.Fatal("expected __user.grab in IR")
	}
	codegentest.AssertContains(t, fn, "strdup.copy")
}

// T1011 (no-regression): a non-droppable enum (no heap payload in any variant)
// narrowed to a variant whose non-heap field escapes must NOT clone —
// narrowedVariantFieldDroppable reports droppable=false (enumTargetDroppable is
// false), so the consumer skips the dup. Cloning a field the synth drop never
// frees would leak. The int field copies cleanly into the constructor.
func TestEnumNarrowVariantNonDroppableFieldNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Flag { On(int code), Off }
		type Box { int v; }
		grab() Box {
			Flag f = Flag.On(code: 3);
			if f is On { return Box(v: f.code); }
			return Box(v: 0);
		}
		main() { Box b = grab(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.grab")
	if fn == "" {
		t.Fatal("expected __user.grab in IR")
	}
	codegentest.AssertNotContains(t, fn, "strdup.copy")
}

// T0993: `match this { Subtype c => }` over a class hierarchy. genThisExpr
// returns the raw i8* instance pointer; genValueMatch must normalize it into the
// {vtable, instance} value struct before the type-pattern arm extracts the
// instance for RTTI — otherwise it emits `extractvalue` on an i8* (invalid IR
// that fails `opt`). This guards that normalization.
func TestMatchTypePatternThisReceiver(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "call i32 @promise_type_is")
	// No extractvalue on an i8* — the subject was wrapped into a value struct.
	if strings.Contains(ir, "extractvalue i8* ") {
		t.Errorf("expected no extractvalue on i8* (this subject must be normalized)\n%s", ir)
	}
}
