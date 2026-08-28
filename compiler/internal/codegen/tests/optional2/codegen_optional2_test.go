package optional2

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T0761: RTTI cast whose subject is itself an Optional. genCastExpr used to
// treat the {i1,{i8*,i8*}} optional as a bare value struct and panic; it now
// branches to genOptionalCastExpr, which unwraps field 1 before promise_type_is.
func TestOptionalChainPolymorphicField(t *testing.T) {
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
			int? n = a?.legs;
		}
	`)
	codegentest.AssertContains(t, ir, "optchain.some")
	codegentest.AssertContains(t, ir, "optchain.none")
	// Field load produces an i64 result wrapped into the optional.
	codegentest.AssertContains(t, ir, "load i64")
}

func TestValueTypeOptional(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Point {
			int x `+"`value"+`;
			int y `+"`value"+`;
		}
		main() {
			Point? maybe = Point(x: 1, y: 2);
		}
	`)
	// Optional wraps the full value struct: { i1, %promise_Point_v }
	codegentest.AssertContains(t, ir, "{ i1, %promise_Point_v }")
}

func TestOptionalRecoveryCodegen(t *testing.T) {
	// Optional recovery: non-recovering handler wraps result as T?
	ir := codegentest.GenerateIR(t, `
		fail!() int { raise error(message: "oops"); }
		main() {
			x := fail() ? e { print_line("handled"); };
		}
	`)
	// Should wrap success value as optional some (insertvalue with i1 true)
	// and produce a phi node merging ok/error paths
	codegentest.AssertContains(t, ir, "insertvalue")
	codegentest.AssertContains(t, ir, "i1 true")
}

// T1012 (T0485 branch): an Optional-of-heap variant payload (`string? maybe`)
// destructured via `if x is V(field)` must NOT be dup'd — it is marked
// match-borrowed instead (the binding aliases the subject's payload, which the
// subject's synth enum drop owns). So no clone and no per-binding drop flag are
// emitted; only in-scope reads are sound (escape is the separate T1170 gap).
func TestT1012IfIsDestructureOptionalPayloadBorrowNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	fn := codegentest.ExtractFunction(ir, "__user.read")
	// Optional heap payload is borrow-marked, not cloned, and no drop flag is
	// registered for the `maybe` binding.
	codegentest.AssertNotContains(t, fn, "strdup.copy")
	codegentest.AssertNotContains(t, fn, "maybe.dropflag")
}

// T1170: an Optional-of-heap variant payload (`string? maybe`) that ESCAPES the
// narrowing scope (here via `return`) must be deep-cloned on the read/escape side
// (genIdentExpr, gated on matchBorrowedIdents + the dup flag genReturnStmt sets),
// so the escaped Optional owns an independent inner string and survives the
// subject's synth enum drop. The clone lowers through dupString → `strdup.copy`.
func TestT1170OptionalPayloadEscapeDupsOnReturn(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Box { Has(string? maybe), Nothing }
		esc() string? {
			Box b = Box.Has(maybe: "a" + "b");
			if b is Has(maybe) { return maybe; }
			return none;
		}
		main() { s := esc(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.esc")
	// The escaping Optional[string] payload is cloned via dupString.
	codegentest.AssertContains(t, fn, "strdup.copy")
}

// T1170 zero-copy control: an in-scope read of an Optional-of-heap payload (no
// escape → no dup flag set) must NOT clone. This is the proof that the escape
// dup is gated on an owning sink and in-scope borrows stay zero-copy.
func TestT1170OptionalPayloadInScopeNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	fn := codegentest.ExtractFunction(ir, "__user.read")
	codegentest.AssertNotContains(t, fn, "strdup.copy")
}

// T1170: an Optional-of-heap payload stored to an escaping OUTER local
// (`out = maybe`, whole-Optional ident RHS) must be cloned on read. This
// exercises the genAssignStmt IdentExpr-RHS branch (isVariantPayloadBorrowShape)
// — distinct from the array-element RHS branch covered above.
func TestT1170OptionalPayloadEscapeDupsOnStore(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Box { Has(string? maybe), Nothing }
		esc() string? {
			Box b = Box.Has(maybe: "a" + "b");
			string? out = none;
			if b is Has(maybe) { out = maybe; }
			return out;
		}
		main() { s := esc(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.esc")
	codegentest.AssertContains(t, fn, "strdup.copy")
}

// T1170: an Optional-of-heap payload passed to a consuming (~/move) param
// (`consume(move maybe)`) escapes into the callee and must be cloned so the
// subject's synth enum drop doesn't free the value the callee now owns. This
// exercises the maybeEnableDupForMutRefArg T1170 branch.
func TestT1170OptionalPayloadEscapeConsumingArg(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	fn := codegentest.ExtractFunction(ir, "__user.esc")
	codegentest.AssertContains(t, fn, "strdup.copy")
}

// T1170: an Optional-of-heap payload used to initialize an owned constructor
// field (`W(held: maybe)`) escapes via the returned instance and must be cloned.
// This exercises the maybeEnableDupForConstructorArg T1170 branch.
func TestT1170OptionalPayloadEscapeConstructorField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	fn := codegentest.ExtractFunction(ir, "__user.esc")
	codegentest.AssertContains(t, fn, "strdup.copy")
}

// T1170: the escape dup fires uniformly for the `match` path (not just `if is`),
// since both populate matchBorrowedIdents. A match arm returning an
// Optional-of-heap payload must clone on read.
func TestT1170OptionalPayloadEscapeDupsOnMatch(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	fn := codegentest.ExtractFunction(ir, "__user.esc")
	codegentest.AssertContains(t, fn, "strdup.copy")
}

// T1174: an Optional-of-heap-user-type variant payload (`Row? maybe`) that
// ESCAPES `if is`/`match` (return / store-to-outer / consuming arg / constructor
// field) must be deep-cloned via dupBorrowedHeapUserPayload — otherwise the
// bound alias points into the subject's variant payload, which the subject's
// synth enum drop frees at scope exit (UAF / SIGSEGV). The clone lowers to a
// dupHeapValue `heapdup.copy` block in the escaping function.
func TestT1174OptionalHeapUserPayloadEscapeDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Row { string name; }
		enum Box { Has(Row? maybe), Nothing }
		esc() Row? {
			Box b = Box.Has(maybe: Row(name: "a" + "b"));
			if b is Has(maybe) { return maybe; }
			return none;
		}
		main() { r := esc(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.esc")
	// Escaping the borrowed Optional[Row] payload deep-clones the inner heap value.
	codegentest.AssertContains(t, fn, "heapdup.copy")
}

// T1174 over-application guard: an in-scope-only Optional[heap-user] binding must
// stay a zero-copy borrow (no dup) — the subject outlives the narrowing and its
// synth enum drop frees the payload exactly once. The dup is gated to explicit
// escape sites, so no `heapdup.copy` is emitted here (preserving the T0512
// nested-Optional zero-copy invariant). An over-eager dup would also leak.
func TestT1174OptionalHeapUserPayloadInScopeNoDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	fn := codegentest.ExtractFunction(ir, "__user.rd")
	codegentest.AssertNotContains(t, fn, "heapdup.copy")
}

// T1174: `v.push(maybe)` moves a match-borrowed Optional[heap-user] payload into
// a vector. Push is a native special-case that bypasses the escape-site dups, so
// it must deep-clone the Optional[heap-user] element in maybeDupPushElement's
// Optional branch — otherwise the vector slot aliases the subject's variant
// payload and double-frees when both drop. The clone lowers to a dupHeapValue
// `heapdup.copy` block. Also covers the pre-existing Vector[Row?] slice path,
// which shares the same maybeDupPushElement branch.
func TestT1174OptionalHeapUserPushDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	fn := codegentest.ExtractFunction(ir, "__user.esc")
	codegentest.AssertContains(t, fn, "heapdup.copy")
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
	ir := codegentest.GenerateIR(t, `
		type P { int x; }
		enum Box { Has(P? maybe), Nothing }
		esc() P? {
			Box b = Box.Has(maybe: P(x: 42));
			if b is Has(maybe) { return maybe; }
			return none;
		}
		main() { r := esc(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.esc")
	codegentest.AssertContains(t, fn, "heapdup.copy")
}

// T1178 (non-array sibling preserved): the fix skips the read-side dup ONLY for the
// array shape. An Optional[heap-user] variant payload (T1174) is NOT covered by
// arrayElemNeedsEscapeDup, so genIdentExpr's gate (!isArr => true) still lets
// dupBorrowedHeapUserPayload deep-clone it on escape. Guards against the fix
// over-reaching and suppressing the Optional[user] clone (which would be a UAF, not
// a leak). The escaped Optional's inner heap instance is cloned exactly once.
func TestT1178OptionalHeapUserPayloadStillClones(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Row { string name; }
		enum Box { Some(Row? maybe), Empty }
		esc() Row? {
			Box b = Box.Some(maybe: Row(name: "a" + "b"));
			if b is Some(maybe) { return maybe; }
			return none;
		}
		main() { r := esc(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.esc")
	// dupBorrowedHeapUserPayload's optionalHeapDupElem branch still fires.
	codegentest.AssertContains(t, fn, "heapdup.copy")
}

// T0392: Synth drop must recurse into Optional[heap-user-type] field, dropping the
// inner heap allocation. Without this, `Holder { Box? data }` leaks the Box.
func TestSynthDropRecursesIntoHeapUserOptionalField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0392Box { int n; drop(~this) {} }
		type T0392Holder { T0392Box? data; }
		main() {
			h := T0392Holder(data: T0392Box(n: 7));
		}
	`)
	holderDrop := codegentest.ExtractFunction(ir, "T0392Holder.drop")
	if holderDrop == "" {
		t.Fatal("expected T0392Holder.drop in IR")
	}
	// optfield drop block conditional on the present flag.
	codegentest.AssertContains(t, holderDrop, "optfield.drop")
	codegentest.AssertContains(t, holderDrop, "optfield.skip")
	// Inner Box.drop must be invoked for present values.
	codegentest.AssertContains(t, holderDrop, "call void @T0392Box.drop")
	// Heap user type without synth drop also requires pal_free of the instance.
	codegentest.AssertContains(t, holderDrop, "call void @pal_free")
}

// T0392: Synth drop must recurse into nested Optional T?? fields, visiting both
// the outer and inner has-value flags before dropping.
func TestSynthDropRecursesIntoNestedOptionalField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0392Box2 { int n; drop(~this) {} }
		type T0392Holder2 { T0392Box2?? data; }
		main() {
			T0392Box2? inner = T0392Box2(n: 1);
			h := T0392Holder2(data: inner);
		}
	`)
	holderDrop := codegentest.ExtractFunction(ir, "T0392Holder2.drop")
	if holderDrop == "" {
		t.Fatal("expected T0392Holder2.drop in IR")
	}
	// Two pairs of optfield branches — outer Optional and inner Optional.
	if got := strings.Count(holderDrop, "optfield.drop"); got < 2 {
		t.Errorf("expected at least 2 optfield.drop blocks (outer + inner), got %d", got)
	}
	// Inner Box.drop must still be called for the doubly-wrapped value.
	codegentest.AssertContains(t, holderDrop, "call void @T0392Box2.drop")
}

// T0392: Force-unwrap of an Optional[heap-user-type] field neutralizes the
// owner's flag so the holder's drop doesn't double-free the inner instance now
// owned by the new local.
func TestForceUnwrapOfHeapUserOptionalFieldNeutralizes(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, mainFn, "store i1 false")
}

func TestT1073ArrayLitForceUnwrapNeutralizesSource(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1073Box { string name; drop(~this) {} }
		arr(T1073Box? move o) T1073Box[] { return [o!]; }
		main() {}
	`)
	fn := codegentest.ExtractFunction(ir, "__user.arr")
	if fn == "" {
		t.Fatal("expected __user.arr in IR")
	}
	codegentest.AssertContains(t, fn, "unwrap.ok")
	codegentest.AssertContains(t, fn, codegentest.T1073NeutralizeSig)
}

func TestT1073TupleLitForceUnwrapNeutralizesSource(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1073Box { string name; drop(~this) {} }
		tup(T1073Box? move o) (T1073Box, int) { return (o!, 1); }
		main() {}
	`)
	fn := codegentest.ExtractFunction(ir, "__user.tup")
	if fn == "" {
		t.Fatal("expected __user.tup in IR")
	}
	codegentest.AssertContains(t, fn, codegentest.T1073NeutralizeSig)
}

func TestT1073MapLitForceUnwrapNeutralizesSource(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1073Box { string name; drop(~this) {} }
		mp(T1073Box? move o) map[int, T1073Box] { return {1: o!}; }
		main() {}
	`)
	fn := codegentest.ExtractFunction(ir, "__user.mp")
	if fn == "" {
		t.Fatal("expected __user.mp in IR")
	}
	codegentest.AssertContains(t, fn, codegentest.T1073NeutralizeSig)
}

// T1073: a copy/scalar inner (`int?`) is NOT consumed by force-unwrap, so its
// source optional must NOT be neutralized (it stays usable). neutralizeForceUnwrapElem
// self-gates on typeNeedsFieldDrop, so no present-flag clear is emitted here.
func TestT1073ArrayLitScalarForceUnwrapNoNeutralize(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		arr(int? move o) int[] { return [o!]; }
		main() {}
	`)
	fn := codegentest.ExtractFunction(ir, "__user.arr")
	if fn == "" {
		t.Fatal("expected __user.arr in IR")
	}
	// int? optional layout is `{ i1, i64 }`, not `{ i1, { i8*, i8* } }`; and no
	// present-flag clear should be emitted for the (copy) source.
	codegentest.AssertNotContains(t, fn, "i32 0, i32 0\n\tstore i1 false")
}

// T1073: a paren-wrapped force-unwrap `[(o!)]` must still neutralize the source.
// Exercises the ParenExpr-peel loop in isForceUnwrapElem — codegen sees through
// ParenExpr at genExpr but the AST-shape dispatch here must peel it too.
func TestT1073ArrayLitParenWrappedForceUnwrapNeutralizesSource(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1073Box { string name; drop(~this) {} }
		arr(T1073Box? move o) T1073Box[] { return [(o!)]; }
		main() {}
	`)
	fn := codegentest.ExtractFunction(ir, "__user.arr")
	if fn == "" {
		t.Fatal("expected __user.arr in IR")
	}
	codegentest.AssertContains(t, fn, codegentest.T1073NeutralizeSig)
}

// T1073: force-unwrap of a droppable map *key* `{o!: 1}` must neutralize the
// source optional (the map's drop frees keys via []=), mirroring the map-value
// path. Exercises the entry.Key neutralize call site in genMapLit.
func TestT1073MapLitKeyForceUnwrapNeutralizesSource(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T1073Key {
			string name;
			drop(~this) {}
			get hash int { return 7; }
			== (T1073Key other) bool { return this.name == other.name; }
		}
		mk(T1073Key? move o) map[T1073Key, int] { return {o!: 1}; }
		main() {}
	`)
	fn := codegentest.ExtractFunction(ir, "__user.mk")
	if fn == "" {
		t.Fatal("expected __user.mk in IR")
	}
	codegentest.AssertContains(t, fn, codegentest.T1073NeutralizeSig)
}

// T0392: Synth drop must call pal_free for heap user types WITHOUT a drop method
// (B0211 case). The inner has no drop function but the heap allocation must be freed.
func TestSynthDropOptionalNoDropHeapUserField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0392RawBox { int n; }
		type T0392RawHolder { T0392RawBox? data; }
		main() {
			h := T0392RawHolder(data: T0392RawBox(n: 7));
		}
	`)
	holderDrop := codegentest.ExtractFunction(ir, "T0392RawHolder.drop")
	if holderDrop == "" {
		t.Fatal("expected T0392RawHolder.drop in IR")
	}
	// optfield branches conditional on the present flag.
	codegentest.AssertContains(t, holderDrop, "optfield.drop")
	// pal_free must still happen for raw heap user types with no explicit drop.
	codegentest.AssertContains(t, holderDrop, "call void @pal_free")
	// No call to a drop method since the type doesn't define one.
	codegentest.AssertNotContains(t, holderDrop, "call void @T0392RawBox.drop")
}

// T0392: Synth drop must use the synth drop function for heap user types WITH
// synth drop (e.g., string field). The synth drop calls pal_free internally,
// so the optional path must NOT call pal_free again.
func TestSynthDropOptionalSynthDropHeapUserField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0392SynBox { string s; }
		type T0392SynHolder { T0392SynBox? data; }
		main() {
			h := T0392SynHolder(data: T0392SynBox(s: "x"));
		}
	`)
	holderDrop := codegentest.ExtractFunction(ir, "T0392SynHolder.drop")
	if holderDrop == "" {
		t.Fatal("expected T0392SynHolder.drop in IR")
	}
	// optfield branches conditional on the present flag.
	codegentest.AssertContains(t, holderDrop, "optfield.drop")
	// Synth drop is invoked — calls _Box.drop which itself calls pal_free.
	codegentest.AssertContains(t, holderDrop, "call void @T0392SynBox.drop")
}

// T0392: Force-unwrap of a string/vector optional field must NOT trigger
// MemberExpr neutralization — genFieldAccess already dups at access time, so
// neutralizing would leak the original. Verified by counting store-i1-false
// instructions: the heap-user case does ONE extra store (the neutralization
// flag clear) compared to the string case.
func TestForceUnwrapStringOptionalFieldNoExtraStore(t *testing.T) {
	stringIR := codegentest.GenerateIR(t, `
		type T0392StrHolder { string? name; drop(~this) {} }
		main() {
			h := T0392StrHolder(name: "world");
			s := h.name!;
		}
	`)
	heapIR := codegentest.GenerateIR(t, `
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
	ir := codegentest.GenerateIR(t, `
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
	getInner := codegentest.ExtractFunction(ir, "T0392MHolder.get_inner")
	if getInner == "" {
		t.Fatal("expected T0392MHolder.get_inner in IR")
	}
}

// T0101: Optional field in type with synthesized drop
func TestOptionalFieldInSynthDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper { string? opt; }
		main() {
			Wrapper w = Wrapper(opt: "hello");
		}
	`)
	// Wrapper gets synthesized drop that checks optional field
	codegentest.AssertContains(t, ir, "define void @Wrapper.drop")
	codegentest.AssertContains(t, ir, "optfield.drop")
	codegentest.AssertContains(t, ir, "optfield.skip")
}

// T0111: Optional local with droppable inner type gets scope-exit drop
func TestOptionalLocalStringDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string? s = "hello";
		}
	`)
	// Optional local should get a drop binding with optdrop blocks
	codegentest.AssertContains(t, ir, "optdrop.check")
	codegentest.AssertContains(t, ir, "optdrop.inner")
	codegentest.AssertContains(t, ir, "promise_string_drop")
}

// T0111: Force unwrap of optional identifier clears drop flag
func TestOptionalForceUnwrapClearsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string? s = "hello";
			string val = s!;
		}
	`)
	// Should have optdrop blocks (drop registered for s)
	codegentest.AssertContains(t, ir, "optdrop.check")
	// The drop flag should be cleared (store i1 false) after unwrap
	codegentest.AssertContains(t, ir, "store i1 false")
}

// B0309: Force unwrap in index-assignment key position neutralizes source optional.
func TestOptionalForceUnwrapIndexAssignKey(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string? key = "hello";
			map[string, int] m = {:};
			m[key!] = 42;
		}
	`)
	// The []=  call should exist (mangled as Map[string, int].[]=)
	codegentest.AssertContains(t, ir, `.[]="`)
	// B0309: present flag must be set to false after index assign (neutralize source)
	codegentest.AssertContains(t, ir, "store i1 false")
}

// B0309: Force unwrap in index-assignment value position neutralizes source optional.
func TestOptionalForceUnwrapIndexAssignValue(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string? val = "hello";
			map[int, string] m = {:};
			m[1] = val!;
		}
	`)
	codegentest.AssertContains(t, ir, `.[]="`)
	// B0309: present flag must be set to false after index assign (neutralize source)
	codegentest.AssertContains(t, ir, "store i1 false")
}

// B0312: Force unwrap in ident reassignment neutralizes source optional.
func TestOptionalForceUnwrapIdentReassign(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string? opt = "hello";
			string x = "";
			x = opt!;
		}
	`)
	// B0312: present flag must be set to false after ident reassign (neutralize source)
	codegentest.AssertContains(t, ir, "store i1 false")
}

// B0312: Force unwrap in member assignment neutralizes source optional.
func TestOptionalForceUnwrapMemberAssign(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder { string val; }
		main() {
			string? opt = "hello";
			h := Holder(val: "");
			h.val = opt!;
		}
	`)
	// B0312: present flag must be set to false after member assign (neutralize source)
	codegentest.AssertContains(t, ir, "store i1 false")
}

// T0111: Optional local with vector inner type gets scope-exit drop
func TestOptionalLocalVectorDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int[]? v = [1, 2, 3];
		}
	`)
	codegentest.AssertContains(t, ir, "optdrop.check")
	codegentest.AssertContains(t, ir, "optdrop.inner")
	codegentest.AssertContains(t, ir, "Vector.drop")
}

// T0938: Optional local with a vector inner whose elements are droppable
// (string[]?) must walk and drop elements before freeing the buffer, under a
// bit-63 static-vector guard — mirroring the non-optional emitStringDropCall
// path. Without this, only the buffer is freed and the elements leak.
func TestOptionalLocalVectorStringElementDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		dropfn_str() {
			string[] v = [];
			v.push("a");
			string[]? a = v;
		}
		main() { dropfn_str(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.dropfn_str")
	codegentest.AssertContains(t, fn, "optdrop.check")
	codegentest.AssertContains(t, fn, "optdrop.inner")
	// Static-vector guard: bit-63 mask before deciding to drop elements/buffer.
	codegentest.AssertContains(t, fn, "optvecdrop.nonstatic")
	codegentest.AssertContains(t, fn, "-9223372036854775808")
	// Element-drop loop runs before the buffer free.
	codegentest.AssertContains(t, fn, "vecdrop.head")
	codegentest.AssertContains(t, fn, "call void @promise_string_drop")
	codegentest.AssertContains(t, fn, "Vector.drop")
}

// T0938: A non-droppable element type (int[]?) must NOT emit a string element
// drop loop — only the buffer free path under the static guard.
func TestOptionalLocalVectorIntNoElementDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		dropfn_int() {
			int[] v = [];
			v.push(7);
			int[]? a = v;
		}
		main() { dropfn_int(); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.dropfn_int")
	codegentest.AssertContains(t, fn, "optvecdrop.nonstatic")
	// No element-drop loop and no string drop inside the function for non-droppable ints.
	codegentest.AssertNotContains(t, fn, "vecdrop.head")
	codegentest.AssertNotContains(t, fn, "call void @promise_string_drop")
}

// T0111: Force unwrap of optional field access dups the string via dupStringFieldAccess
func TestOptionalFieldForceUnwrapDupsString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Wrapper { string? opt; }
		main() {
			Wrapper w = Wrapper(opt: "hello");
			string val = w.opt!;
		}
	`)
	// dupStringFieldAccess mechanism dups the string during field access
	codegentest.AssertContains(t, ir, "strdup.copy")
	codegentest.AssertContains(t, ir, "promise_string_new")
}

// B0196: Discarded Vector[string].pop() must drop the inner string.
func TestDropDiscardedOptionalStringPop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			string[] v = ["a", "b", "c"];
			v.pop();
		}
	`)
	// The discarded optional string from pop() should trigger a conditional drop.
	codegentest.AssertContains(t, ir, "discard.drop")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// B0196: Discarded Vector[int].pop() should NOT emit discard drop (int is not droppable).
func TestNoDropDiscardedOptionalIntPop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			int[] v = [1, 2, 3];
			v.pop();
		}
	`)
	testFn := codegentest.ExtractFunction(ir, "__user.test")
	if strings.Contains(testFn, "discard.drop") {
		t.Fatalf("expected test function to NOT contain discard.drop\ngot:\n%s", testFn)
	}
}

// B0208: Discarded Vector[Vector[int]].pop() must drop the inner vector.
func TestDropDiscardedOptionalVectorPop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			int[][] v = int[][]();
			v.push([1, 2, 3]);
			v.pop();
		}
	`)
	codegentest.AssertContains(t, ir, "discard.drop")
	codegentest.AssertContains(t, ir, "call void @Vector.drop")
}

// B0208: Discarded Optional with user type with drop must drop inner instance.
func TestDropDiscardedOptionalUserTypePop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "discard.drop")
	codegentest.AssertContains(t, ir, "call void @Res.drop")
}

// B0211: Optional of heap user type without drop should register pal_free cleanup.
func TestOptionalHeapTypeWithoutDropFreed(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Pt {
			int x;
			int y;
		}
		test() {
			Pt? p = Pt(x: 1, y: 2);
		}
	`)
	// Should have optional drop check and pal_free
	codegentest.AssertContains(t, ir, "optdrop.check")
	codegentest.AssertContains(t, ir, "call void @pal_free")
}

// B0215: If-let unwrap should drop the inner string value at scope exit.
func TestIfUnwrapStringDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "strdrop.call")
}

// B0215: If-let unwrap from a local optional should emit string drop for the unwrapped value.
func TestIfUnwrapLocalOptionalStringDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			string? s = "hello";
			if v := s {
				int x = v.len;
			}
		}
	`)
	// The unwrapped string v must be dropped in the then-block.
	codegentest.AssertContains(t, ir, "strdrop.call")
}

// B0215: While-let unwrap should drop the inner string value at each iteration end.
func TestWhileUnwrapStringDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "strdrop.call")
}

// B0229: Optional structural interface variables should register drop for reassignment.
func TestOptionalStructuralInterfaceDropOnReassign(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "optdrop")
	// B0243: Should use RTTI-based drop dispatch (not __promise_iter_cleanup).
	// The concrete type behind the interface is unknown at compile time.
	codegentest.AssertContains(t, ir, "struct.drop")
}

// B0243: Optional structural interface drop in closure env must use RTTI dispatch,
// not __promise_iter_cleanup (which assumes _FnIter layout and segfaults on other types).
func TestOptionalStructuralInterfaceEnvDropRTTI(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "optst.rtti")
}

// B0246: If-let unwrap of Optional structural interface should NOT clear the source's
// drop flag. The unwrapped structural binding doesn't get a drop registered (no concrete
// type known at compile time), so the source must retain ownership. Its reassignment-time
// Optional drop (RTTI-based) handles cleanup.
func TestIfUnwrapOptionalStructuralNoDropFlagClear(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "optdrop.check")
	// RTTI-based drop dispatch for the structural interface inside the Optional.
	codegentest.AssertContains(t, ir, "struct.drop")
}

// B0240: Assigning none to an optional field with a heap user type should
// drop/free the old inner value before storing the new value.
func TestOptionalFieldReassignDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Location { int x; int y; }
		type Place { string name; Location? location; }
		test() {
			Place p = Place(name: "a", location: Location(x: 1, y: 2));
			p.location = none;
		}
	`)
	// The reassignment to none should emit a conditional drop for the old optional value.
	codegentest.AssertContains(t, ir, "field.optdrop")
	// Should free the inner Location instance.
	codegentest.AssertContains(t, ir, "call void @pal_free")
}

// B0240: Assigning none to an optional string field should call promise_string_drop.
func TestOptionalStringFieldReassignDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder { string? value; }
		test() {
			Holder h = Holder(value: "hello");
			h.value = none;
		}
	`)
	codegentest.AssertContains(t, ir, "field.optdrop")
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// B0240: Assigning none to an optional field with a droppable user type should
// call the drop function before freeing.
func TestOptionalDroppableFieldReassignDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "field.optdrop")
	codegentest.AssertContains(t, ir, "call void @Resource.drop")
}

// T0522 (destructure neutralization): destructuring `t!` where `t` is an
// Optional[(int, string)] local must clear t's present flag — otherwise both
// the destructured `s` and t's scope-exit optdrop will free the same heap
// string. The neutralization emits a GEP into t at index (0,0) followed by a
// `store i1 false`, which is the distinguishing IR pattern.
func TestT0522DestructureForceUnwrapNeutralizesSource(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			(int, string)? t = (1, "a" + "b");
			(n, s) := t!;
		}
	`)
	testFn := codegentest.ExtractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	// Distinctive pattern: GEP into the source Optional alloca %t selecting
	// the present-flag field (i32 0, i32 0). Without the fix, no such GEP
	// exists — operations on %t are only the initial store and the load for
	// the unwrap / scope-exit optdrop.
	codegentest.AssertContains(t, testFn, "%t, i32 0, i32 0")
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
	ir := codegentest.GenerateIR(t, `
		type _Holder { string? title; drop(~this) {} }
		_consume_opt_string(string? move s) `+"`public {}"+`
		test() {
			h := _Holder(title: "foo" + "bar");
			_consume_opt_string(h.title);
		}
	`)
	testFn := codegentest.ExtractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	codegentest.AssertContains(t, testFn, "strdup.copy")
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
	ir := codegentest.GenerateIR(t, `
		type _Holder { Vector[int]? items; drop(~this) {} }
		_consume_opt_vec(Vector[int]? move v) `+"`public {}"+`
		test() {
			h := _Holder(items: [1, 2, 3]);
			_consume_opt_vec(h.items);
		}
	`)
	testFn := codegentest.ExtractFunction(ir, "__user.test")
	if testFn == "" {
		t.Fatal("expected __user.test in IR")
	}
	codegentest.AssertContains(t, testFn, "vecdup.copy")
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
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "alias.dup")
	codegentest.AssertContains(t, ir, "alias.cont")
	// The inner Optional value is deep-cloned (present/absent split + heap clone).
	codegentest.AssertContains(t, ir, "optdup.dup")
	codegentest.AssertContains(t, ir, "heapdup.copy")
	// The source arg's drop flag must NOT be cleared (no ownership transfer).
	codegentest.AssertNotContains(t, ir, "store i1 false, i1* %a.dropflag")
}

// T0391: A nested Optional local (T??) must register a scope-exit drop binding
// so its inner heap pointer is freed. The drop emits an outer present check,
// extracts the inner Optional, then a second present check before the actual
// drop (or pal_free for heap user types without a drop method).
func TestNestedOptionalDropRecurses(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box { int n; drop(~this) {} }
		returns_double(Box? a) Box?? { return a; }
		main() {
			Box? a = Box(n: 1);
			Box?? r = returns_double(a);
		}
	`)
	// r must have its own drop flag for scope-exit cleanup.
	codegentest.AssertContains(t, ir, "%r.dropflag")
	// The optional drop chain must traverse two layers — the helper emits
	// nested optdrop.inner / optdrop.done blocks via recursion.
	codegentest.AssertContains(t, ir, "optdrop.check")
	codegentest.AssertContainsMatch(t, ir, `optdrop\.inner[\s\S]*optdrop\.inner`)
	// Bottom-level dispatch reaches Box.drop (the heap user type has a drop method).
	codegentest.AssertContains(t, ir, "call void @Box.drop")
}

// T0391: A nested Optional[string] (string??) drop reaches promise_string_drop
// at the bottom of the recursive walk via the `b.named == TypString` branch
// in emitOptionalValueDrop.
func TestNestedOptionalStringDropRecurses(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		returns_double_str(string? a) string?? { return a; }
		main() {
			string? a = "hello";
			string?? r = returns_double_str(a);
		}
	`)
	codegentest.AssertContains(t, ir, "%r.dropflag")
	// Two layers of optdrop.inner (recursive walk through string?? → string? → string).
	codegentest.AssertContainsMatch(t, ir, `optdrop\.inner[\s\S]*optdrop\.inner`)
	codegentest.AssertContains(t, ir, "call void @promise_string_drop")
}

// T0391: A nested Optional[Vector] drop reaches Vector.drop at the bottom of the
// recursive walk via the `isContainerType` branch in emitOptionalValueDrop.
func TestNestedOptionalVectorDropRecurses(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		returns_double_vec(int[]? a) int[]?? { return a; }
		main() {
			int[]? a = [1, 2, 3];
			int[]?? r = returns_double_vec(a);
		}
	`)
	codegentest.AssertContains(t, ir, "%r.dropflag")
	codegentest.AssertContainsMatch(t, ir, `optdrop\.inner[\s\S]*optdrop\.inner`)
	codegentest.AssertContains(t, ir, "call void @Vector.drop")
}

// T0391: A nested Optional[enum] drop reaches the enum drop function at the
// bottom via the `extractEnum != nil` branch in emitOptionalValueDrop. The
// inner value is an enum struct stored to a temp alloca and bitcast to i8*.
func TestNestedOptionalEnumDropRecurses(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Msg { Empty, Text(string body) }
		returns_double_enum(Msg? a) Msg?? { return a; }
		main() {
			Msg? a = Msg.Text("hi");
			Msg?? r = returns_double_enum(a);
		}
	`)
	codegentest.AssertContains(t, ir, "%r.dropflag")
	codegentest.AssertContainsMatch(t, ir, `optdrop\.inner[\s\S]*optdrop\.inner`)
	codegentest.AssertContains(t, ir, "call void @Msg.drop")
}

// T0391: while-let on T?? must register a nested Optional drop binding for the
// unwrapped element (just like if-let). Mirror of TestNestedOptionalDropRecurses
// for genWhileUnwrapStmt's nested Optional path.
func TestWhileLetNestedOptionalDropBinding(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "%x.dropflag")
	// Body of the unwrap walks through optdrop.inner blocks.
	codegentest.AssertContains(t, ir, "optdrop.check")
	codegentest.AssertContains(t, ir, "call void @Box.drop")
}

// T0620: Optional[heap-user] moved from variable into Vector[T?] literal must
// clear the source's drop flag — the vector now owns the inner payload via
// emitVectorElementDropLoop's Optional branch. Pre-T0620, this was NOT cleared
// (T0610 regression guard); now it IS cleared because Gap A is fixed.
func TestVectorLitMoveFromVarOptionalHeapClearsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type _Box { string label; drop(~this) {} }
		main() {
			_Box? a = _Box(label: "a");
			_Box?[] v = [a];
		}
	`)
	codegentest.AssertContains(t, ir, "%a.dropflag = alloca i1")
	// T0620: Gap A fix — vecElemNeedsOptionalDrop now matches, so the drop flag
	// is cleared, transferring ownership to the vector.
	codegentest.AssertContains(t, ir, "store i1 false, i1* %a.dropflag")
}

// T0620: Gap B fix — Vector[string?] drop must enter the element drop loop
// and emit the Optional drop branch (optfield.drop block). Pre-T0620, the
// emitVectorElementDropLoop guard early-returned for Optional elements.
func TestVectorOptionalStringElementDropLoop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string? a = "hello";
			string?[] v = [a];
		}
	`)
	// The element drop loop body enters emitVariantFieldDrop → emitOptionalValueDrop,
	// which creates an "optfield.drop" block for the has-value branch.
	codegentest.AssertContains(t, ir, "optfield.drop")
	// The drop loop itself is emitted (vecdrop.head/body/done blocks).
	codegentest.AssertContains(t, ir, "vecdrop.head")
}

// T0620: Drop-on-overwrite for Vector[string?] index assign — must emit
// emitVariantFieldDrop on the old element before storing the new one.
func TestVectorOptionalStringIndexAssignDropsOld(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string? a = "old";
			string?[] v = [a];
			v[0] = "new";
		}
	`)
	// The overwrite path loads the old element, drops it via emitVariantFieldDrop
	// (Optional branch → optfield.drop), then stores the new value.
	codegentest.AssertContains(t, ir, "optfield.drop")
}

// T0620: Dup-on-read for Vector[T?] exercises dupOptionalVectorElem branches.
// Reading v[i] into a variable must deep-dup the Optional inner so both
// the variable and the vector own independent copies.
func TestVectorOptionalDupOnReadBranches(t *testing.T) {
	// String branch — dupOptionalVectorElem → dupString
	ir := codegentest.GenerateIR(t, `
		main() {
			string? a = "x";
			string?[] v = [a];
			string? x = v[0];
		}
	`)
	codegentest.AssertContains(t, ir, "optdup.dup")
	codegentest.AssertContains(t, ir, "optdup.merge")

	// Heap user branch — dupOptionalVectorElem → cloneHeapElement
	ir = codegentest.GenerateIR(t, `
		type _B620 { string s; drop(~this) {} }
		main() {
			_B620? b = _B620(s: "hi");
			_B620?[] v = [b];
			_B620? x = v[0];
		}
	`)
	codegentest.AssertContains(t, ir, "optdup.dup")

	// Vector branch — dupOptionalVectorElem → dupVector
	ir = codegentest.GenerateIR(t, `
		main() {
			int[]? a = [1, 2];
			int[]?[] v = [a];
			int[]? x = v[0];
		}
	`)
	codegentest.AssertContains(t, ir, "optdup.dup")

	// Channel branch — dupOptionalVectorElem → dupChannel
	ir = codegentest.GenerateIR(t, `
		main() {
			channel[int]? ch = channel[int]();
			channel[int]?[] v = [ch];
			channel[int]? x = v[0];
		}
	`)
	codegentest.AssertContains(t, ir, "optdup.dup")

	// Arc branch — dupOptionalVectorElem → dupArc
	ir = codegentest.GenerateIR(t, `
		main() {
			Ref[int]? a = Ref[int](1);
			Ref[int]?[] v = [a];
			Ref[int]? x = v[0];
		}
	`)
	codegentest.AssertContains(t, ir, "optdup.dup")
}

// B0288: is-present on method call returning T? with droppable enum inner type
// must emit a conditional drop for the temporary.
func TestIsPresentDropsTempOptionalEnum(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "is.temp.drop")
	codegentest.AssertContains(t, ir, "is.temp.skip")
}

// B0288: is-present on ident expression must NOT emit temp drop
// (the variable's scope binding handles cleanup).
func TestIsPresentIdentNoTempDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string? s = "hello";
			bool ok = s is present;
		}
	`)
	codegentest.AssertNotContains(t, ir, "is.temp.drop")
}

// B0288: is-present on field access must NOT emit temp drop
// (the parent object owns the field data).
func TestIsPresentFieldNoTempDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder {
			string? value;
		}
		main() {
			Holder h = Holder(value: "hello");
			bool ok = h.value is present;
		}
	`)
	codegentest.AssertNotContains(t, ir, "is.temp.drop")
}

// B0288: is-present on method call returning string? must emit temp drop.
func TestIsPresentDropsTempOptionalString(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "is.temp.drop")
	codegentest.AssertContains(t, ir, "promise_string_drop")
}

// B0288: is-present on method call returning UserType? with drop() must emit
// temp drop (extract instance ptr, null-check, call drop, free).
func TestIsPresentDropsTempOptionalUserType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "is.temp.drop")
	codegentest.AssertContains(t, ir, "is.temp.exec")
	codegentest.AssertContains(t, ir, "Handle.drop")
}

// B0287: Optional unwrap on ident source must NOT track the unwrapped string
// as a statement temp (the optional's scope-exit drop handles it).
func TestOptionalUnwrapIdentNoStringTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string? s = "hello";
			bool eq = s! == "hello";
		}
	`)
	// The s! result should not be tracked as a string temp.
	// If it were tracked, there would be a promise_string_drop call for the temp
	// AND the optional's scope drop — double-free.
	// Count promise_string_drop calls: should be only from scope cleanup, not temp tracking.
	testFn := codegentest.ExtractFunction(ir, "main")
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
	ir := codegentest.GenerateIR(t, `
		main() {
			int? a = 7;
			int?? b = a;
		}
	`)
	// b's alloca is a double-Optional struct {i1, {i1, i64}}.
	codegentest.AssertContains(t, ir, "%b = alloca { i1, { i1, i64 } }")
	// a wrapped once into double-Optional via two insertvalues:
	// one for the outer present flag, one for the inner T? value.
	codegentest.AssertContains(t, ir, "insertvalue { i1, { i1, i64 } } undef, i1 true, 0")
	codegentest.AssertContains(t, ir, "insertvalue { i1, { i1, i64 } } %")
}

// T0373: Reassigning a T? value into a T?? local wraps the value at the
// target's depth via insertvalue.
func TestDoubleOptionalReassignWrapsOnce(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			int?? b = none;
			int? a = 5;
			b = a;
		}
	`)
	codegentest.AssertContains(t, ir, "%b = alloca { i1, { i1, i64 } }")
	codegentest.AssertContains(t, ir, "insertvalue { i1, { i1, i64 } } undef, i1 true, 0")
}

// T0373: Returning a T? expression from a T??-returning function wraps
// once at the return site.
func TestDoubleOptionalReturnWrapsOnce(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		f(int? a) int?? {
			return a;
		}
		main() {
			int?? r = f(3);
		}
	`)
	// Function signature is T? in, T?? out (no caller-side wrap needed).
	codegentest.AssertContains(t, ir, "define { i1, { i1, i64 } } @__user.f({ i1, i64 } %a)")
	// Return wraps the T? value once into T?? via insertvalue.
	codegentest.AssertContains(t, ir, "insertvalue { i1, { i1, i64 } } undef, i1 true, 0")
}

// T0373: Value-type constructors take a distinct codegen path
// (genValueTypeConstructor) from heap-type constructors. Verify the
// value-type ctor's maybeWrapOptional wraps a T? arg into a T?? field.
func TestDoubleOptionalValueCtorWrapsOnce(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type VT { int?? data `+"`value;"+` }
		main() {
			int? a = 5;
			VT v = VT(data: a);
		}
	`)
	// Value-type Value struct embeds the field directly: {vtable, {i1,{i1,i64}}}.
	codegentest.AssertContains(t, ir, "%promise_VT_v = type { i8*, { i1, { i1, i64 } } }")
	// Field arg is wrapped once before being placed in the Value struct.
	codegentest.AssertContains(t, ir, "insertvalue { i1, { i1, i64 } } undef, i1 true, 0")
}

// T0428: Force-unwrap of a call-returning string? must track the extracted string
// pointer as a statement temp so it gets freed at statement end. The temp tracking
// branch (genOptionalForceUnwrap lines 8938-8949) fires for non-ident sources.
func TestT0428CallResultStringOptForceUnwrapTracksTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		get_greet() string? { return "hello"; }
		main() {
			int n = get_greet()!.len;
		}
	`)
	// The extracted string i8* must be tracked as a stmt temp with a drop at statement end.
	codegentest.AssertContains(t, ir, "promise_string_drop")
}

// T0428: Force-unwrap of a call-returning int[]? must track the extracted vector
// pointer as a statement temp so it gets freed at statement end.
func TestT0428CallResultVectorOptForceUnwrapTracksTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		get_nums() int[]? {
			int[] v = [1, 2, 3];
			return v;
		}
		main() {
			int n = get_nums()!.len;
		}
	`)
	// The extracted vector pointer must be tracked as a stmt temp with vector drop.
	codegentest.AssertContains(t, ir, "Vector.drop")
}

// T0428 Case 1: T?? field force-unwrap — neutralizeMemberOptionalField must
// look through the inner Optional to find the named type and clear the outer flag.
func TestT0428DoubleOptionalFieldNeutralization(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertContains(t, ir, "store i1 false")
}

// T0428 Case 2: chained MemberExpr force-unwrap — neutralizeMemberOptionalField
// must walk the chain to clear the Optional's present flag.
func TestT0428ChainedMemberForceUnwrapNeutralization(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type T0428Box2 { int n; drop(~this) {} }
		type T0428Inner { T0428Box2? data; }
		type T0428Outer { T0428Inner inner; }
		main() {
			o := T0428Outer(inner: T0428Inner(data: T0428Box2(n: 5)));
			b := o.inner.data!;
		}
	`)
	// The Optional present flag must be cleared via GEP into inner.data.
	codegentest.AssertContains(t, ir, "store i1 false")
}

// T0428 Case 3A: ~this method force-unwrap — neutralizeMemberOptionalField
// must handle ThisExpr root without calling extractInstancePtr on i8*.
func TestT0428OwnedThisForceUnwrapNeutralization(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	dropFn := codegentest.ExtractFunction(ir, "T0428Holder3.drop")
	if dropFn == "" {
		t.Fatal("expected T0428Holder3.drop in IR")
	}
	// Present flag must be cleared in the drop method body.
	codegentest.AssertContains(t, dropFn, "store i1 false")
}

// T0428 Case 3B: borrowed this.field! — genOptionalForceUnwrap must dup the
// inner heap value so both the caller's synth drop and the local own independent copies.
func TestT0428BorrowedThisForceUnwrapDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	getNFn := codegentest.ExtractFunction(ir, "T0428Holder4.get_n")
	if getNFn == "" {
		t.Fatal("expected T0428Holder4.get_n in IR")
	}
	// dupHeapValue allocates new memory and memcpy's the instance.
	codegentest.AssertContains(t, getNFn, "call i8* @pal_alloc")
	codegentest.AssertContains(t, getNFn, "call void @llvm.memcpy")
}

// T0436 Issue 1: single-line `b := h.data!!` on a T?? field — the AST is
// OptionalUnwrapExpr(OptionalUnwrapExpr(MemberExpr)), so neutralizeForceUnwrapSource
// must look through the inner OptionalUnwrapExpr to reach the MemberExpr.
// Without the fix, the outer Optional's present flag stays true → double-free
// when the holder is dropped.
func TestT0436SingleLineDoubleUnwrapNeutralizes(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	ir := codegentest.GenerateIR(t, `
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
	ir := codegentest.GenerateIR(t, `
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
	ir := codegentest.GenerateIR(t, `
		type BoxDrop {
			int n;
			drop(~this) {}
		}
		test_no_unwrap() {
			BoxDrop? a = BoxDrop(n: 12);
		}
	`)
	// The Optional drop dispatch must call the $wrap variant.
	codegentest.AssertContains(t, ir, "@BoxDrop.drop$wrap")
	// The $wrap function itself must call both drop and pal_free.
	wrapFn := codegentest.ExtractFunction(ir, "BoxDrop.drop$wrap")
	if wrapFn == "" {
		t.Fatal("expected BoxDrop.drop$wrap in IR")
	}
	codegentest.AssertContains(t, wrapFn, "call void @BoxDrop.drop")
	codegentest.AssertContains(t, wrapFn, "call void @pal_free")
}

// T0419: Optional[T] where T has only a SYNTHESIZED drop (auto-generated because
// of droppable fields) must dispatch the bare T.drop — NOT T.drop$wrap. Synth
// drops already include pal_free; wrapping would call pal_free twice.
// This guards the `if explicitDrop` check in maybeRegisterOptionalDrop.
func TestOptionalLocalDropSynthSkipsWrap(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type SynthDropBox {
			string s;
		}
		test_synth_no_unwrap() {
			SynthDropBox? a = SynthDropBox(s: "hello");
		}
	`)
	// Premise: SynthDropBox has a synthesized drop that includes pal_free.
	synthFn := codegentest.ExtractFunction(ir, "SynthDropBox.drop")
	if synthFn == "" {
		t.Fatal("expected SynthDropBox.drop in IR")
	}
	codegentest.AssertContains(t, synthFn, "call void @promise_string_drop")
	codegentest.AssertContains(t, synthFn, "call void @pal_free")
	// The Optional drop dispatch must call the bare drop, NOT the wrapper.
	// (No SynthDropBox.drop$wrap function should be emitted at all.)
	codegentest.AssertNotContains(t, ir, "SynthDropBox.drop$wrap")
	// And the user function must dispatch directly to SynthDropBox.drop.
	userFn := codegentest.ExtractFunction(ir, "__user.test_synth_no_unwrap")
	if userFn == "" {
		t.Fatal("expected __user.test_synth_no_unwrap in IR")
	}
	codegentest.AssertContains(t, userFn, "call void @SynthDropBox.drop(")
}

// Optional cast (`this as T`) through `this`. Before the fix this panicked in
// codegen inside wrapOptional ("store operands are not compatible: src=i8*;
// dst={ i8*, i8* }*"). generateIR not panicking is itself the regression guard.
func TestThisCastOptionalNoExtractFromPtr(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	codegentest.AssertNotContains(t, ir, "extractvalue i8*")
	if codegentest.ExtractFunction(ir, "T0747OBase.is_der") == "" {
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
	ir := codegentest.GenerateIR(t, `
		type Shape { string name; area(this) f64 `+"`abstract"+`; }
		type Circle is Shape { f64 radius; area(this) f64 { return this.radius; } }
		helper(int dummy) Circle? {
			Shape s = Circle(name: "src", radius: 2.0);
			return s as Circle;
		}
		main() { _ := helper(0); }
	`)
	fn := codegentest.ExtractFunction(ir, "__user.helper")
	if fn == "" {
		t.Fatal("expected __user.helper in IR")
	}
	// The drop flag is set to the negated downcast-success flag (drop iff the
	// cast failed): `%n = xor i1 %isMatch, true` then `store i1 %n, ... s.dropflag`.
	codegentest.AssertContainsMatch(t, fn, `%\w+ = xor i1 %\w+, true\n\s*store i1 %\w+, i1\* %s\.dropflag`)
	// The conditional drop still executes at scope exit (flag is loaded) ...
	codegentest.AssertContains(t, fn, "load i1, i1* %s.dropflag")
	// ... and it is NOT an unconditional clear (that would leak on a failed
	// downcast — the pre-T0849 buggy shape).
	codegentest.AssertNotContains(t, fn, "store i1 false, i1* %s.dropflag")
}

// T0849 owning-slot sibling: `Box(c: s as Circle)` stores the conditional
// success flag into the field-init constructor's subject drop flag the same way
// (drop iff the cast failed). Before T0849 this site cleared the flag
// unconditionally (`store i1 false`) → leak on the failure path.
func TestT0849_OwningSlotOptionalCastConditionalDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
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
	fn := codegentest.ExtractFunction(ir, "__user.helper")
	if fn == "" {
		t.Fatal("expected __user.helper in IR")
	}
	// Conditional store of the negated success flag into the subject's drop flag.
	codegentest.AssertContainsMatch(t, fn, `%\w+ = xor i1 %\w+, true\n\s*store i1 %\w+, i1\* %s\.dropflag`)
	// Not an unconditional clear (the pre-T0849 leak-on-failure shape).
	codegentest.AssertNotContains(t, fn, "store i1 false, i1* %s.dropflag")
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
	ir := codegentest.GenerateIR(t, `
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
	none := codegentest.ExtractFunction(ir, "__user.none_arg")
	if none == "" {
		t.Fatal("expected __user.none_arg in IR")
	}
	// `none` → zeroinitialized `{i1,i64}` aggregate passed to the closure call.
	// Before the fix this was a bare `i1 false`, mismatching the `{i1,i64}` param.
	codegentest.AssertContains(t, none, "{ i1, i64 } zeroinitializer)")

	bare := codegentest.ExtractFunction(ir, "__user.bare_arg")
	if bare == "" {
		t.Fatal("expected __user.bare_arg in IR")
	}
	// `5` → wrapped `{i1 true, i64 5}` aggregate (insertvalue chain), not a bare i64.
	codegentest.AssertContainsMatch(t, bare, `insertvalue \{ i1, i64 \} %\w+, i64 5, 1`)
	// The closure call receives the aggregate, never a bare scalar second arg.
	codegentest.AssertContainsMatch(t, bare, `call i1 %\w+\(i8\* %\w+, \{ i1, i64 \} %\w+\)`)
}
