package regress11

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T2049: a bare identifier naming a module-level getter is a CALL producing a
// fresh owned value, not a place read. Codegen's ownership classifiers switched
// on the AST shape and read `*ast.IdentExpr` as "a variable", so such a value was
// either duplicated on push (orphaning the getter's own allocation) or denied a
// statement temp entirely. Both leaked, because an `Optional[T]` result is a
// `{i1, T}` aggregate that no producer path tracks.

const t2049Decls = `
	get probe_dir string? { return "probe" + "-dir-payload"; }
	get probe_vec int[]? { int[] v = []; v.push(1); return v; }
	get probe_int int? { return 7; }
	borrow_opt(string? v) {}
	borrow_vec(int[]? v) {}
	borrow_int(int? v) {}
`

// Pushing a bare-getter optional must MOVE the getter's allocation into the
// vector, not clone it. The dup would emit the optdup present/absent split whose
// present arm calls dupString (promise_string_new); the getter's original then
// has no owner at all. Assert neither appears.
func TestT2049_BareGetterOptionalPushDoesNotDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, t2049Decls+`
		probe() { string?[] xs = []; xs.push(move probe_dir); }
		main() { probe(); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	if strings.Contains(fn, "optdup.dup") {
		t.Fatalf("bare-getter optional push must not clone the element (optdup.dup present):\n%s", fn)
	}
	if strings.Contains(fn, "@promise_string_new") {
		t.Fatalf("bare-getter optional push must not deep-copy the payload (promise_string_new present):\n%s", fn)
	}
	// The value still reaches the vector.
	codegentest.AssertContains(t, fn, "@promise_vector_push")
}

// The same push without the `move` marker takes the identical path — the dup
// decision reads the argument expression, not the marker — so it must not clone
// either. Guards the fix against being marker-sensitive (see T2205).
func TestT2049_UnmarkedBareGetterOptionalPushDoesNotDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, t2049Decls+`
		probe() { string?[] xs = []; xs.push(probe_dir); }
		main() { probe(); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	if strings.Contains(fn, "optdup.dup") {
		t.Fatalf("unmarked bare-getter optional push must not clone the element:\n%s", fn)
	}
	if strings.Contains(fn, "@promise_string_new") {
		t.Fatalf("unmarked bare-getter optional push must not deep-copy the payload:\n%s", fn)
	}
}

// The complementary half: a genuine borrowed source — a plain function parameter
// — is NOT a fresh owned value and MUST still be cloned on push, or the caller's
// value and the vector's slot would alias and double-free. Guards against the
// exemption being widened past bare module getters.
func TestT2049_FunctionParamOptionalPushStillDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, t2049Decls+`
		probe(string? p) { string?[] xs = []; xs.push(p); }
		main() { string? s = "a" + "b"; probe(s); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	codegentest.AssertContains(t, fn, "optdup.dup")
	codegentest.AssertContains(t, fn, "@promise_string_new")
}

// A borrowed-by-callee optional getter result gets a statement temp whose
// present/absent drop fires after the call returns (opttmp.drop), so the payload
// the callee only borrowed is freed exactly once.
func TestT2049_BorrowedOptionalGetterArgEmitsTempDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, t2049Decls+`
		probe() { borrow_opt(probe_dir); }
		main() { probe(); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	codegentest.AssertContains(t, fn, "opttmp.drop")
	codegentest.AssertContains(t, fn, "opttmp.skip")
	codegentest.AssertContains(t, fn, "@promise_string_drop")
}

// Same for a plain optional-returning CALL — the gap was never getter-specific
// on the borrow-argument path: an `Optional[T]` is a `{i1, T}` aggregate that
// neither the i8* nor the {i8*, i8*} tracker matches.
func TestT2049_BorrowedOptionalCallArgEmitsTempDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		mk() string? { return "a" + "b"; }
		borrow_opt(string? v) {}
		probe() { borrow_opt(mk()); }
		main() { probe(); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	codegentest.AssertContains(t, fn, "opttmp.drop")
	codegentest.AssertContains(t, fn, "@promise_string_drop")
}

// A borrowed optional VECTOR temp must walk its droppable buffer, not just free
// the {i1, T} aggregate — emitOptionalValueDrop routes the inner through
// Vector.drop.
func TestT2049_BorrowedOptionalVectorArgEmitsTempDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, t2049Decls+`
		probe() { borrow_vec(probe_vec); }
		main() { probe(); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	codegentest.AssertContains(t, fn, "opttmp.drop")
	codegentest.AssertContains(t, fn, "@Vector.drop")
}

// An optional whose inner needs no drop (`int?`) must not be registered at all —
// variantFieldNeedsDrop gates the temp, so no dead drop block is emitted.
func TestT2049_NonDroppableOptionalGetterNotTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, t2049Decls+`
		probe() { borrow_int(probe_int); }
		main() { probe(); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	if strings.Contains(fn, "opttmp.drop") {
		t.Fatalf("int? needs no drop, so no optional temp should be registered:\n%s", fn)
	}
}

// A discarded bare-getter optional statement must drop its payload. The guard in
// dropDiscardedOptional treated every IdentExpr as a borrowed place and skipped
// it; a getter read is a call, so the discard path now emits the drop.
func TestT2049_DiscardedBareGetterOptionalDrops(t *testing.T) {
	ir := codegentest.GenerateIR(t, t2049Decls+`
		probe() { probe_dir; }
		main() { probe(); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	codegentest.AssertContains(t, fn, "discard.drop")
	codegentest.AssertContains(t, fn, "@promise_string_drop")
}

// The complementary half of the discard guard: a genuine place read (a local
// variable) is still skipped — its own binding drop owns the value, so emitting
// a discard drop there would double-free.
func TestT2049_DiscardedOptionalLocalStillSkipped(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		probe() { string? o = "a" + "b"; o; }
		main() { probe(); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	if strings.Contains(fn, "discard.drop") {
		t.Fatalf("a discarded optional LOCAL is a borrowed place and must not be dropped twice:\n%s", fn)
	}
}

// When a failable callee raises while a borrowed optional temp is still live,
// the error unwind must drop it too — emitStmtTempCleanupForErrorPath dispatches
// the optType temp exactly as the statement-end drain does.
func TestT2049_OptionalGetterTempDroppedOnErrorPath(t *testing.T) {
	ir := codegentest.GenerateIR(t, t2049Decls+`
		take_fail!(string? v) bool { raise error(message: "boom"); }
		probe() { bool ok = take_fail(probe_dir) ? { true }; }
		main() { probe(); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	codegentest.AssertContains(t, fn, "opttmp.drop")
	if n := strings.Count(fn, "opttmp.drop"); n < 2 {
		t.Fatalf("expected the optional temp drop on BOTH the ok and error paths, got %d:\n%s", n, fn)
	}
}

// A generator callee reads its argument lazily (the frame outlives the calling
// statement), so a statement-end drop would be a use-after-free. The registration
// must decline that shape — the T1233/T1467 rationale, and the leak T1500 tracks.
func TestT2049_GeneratorOptionalArgNotTracked(t *testing.T) {
	ir := codegentest.GenerateIR(t, t2049Decls+`
		gen(int n, string? b) stream[int] { int i = 0; while i < n { yield i; i = i + 1; } }
		probe() { for x in gen(2, probe_dir) { } }
		main() { probe(); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	if strings.Contains(fn, "opttmp.drop") {
		t.Fatalf("a generator borrows its arg lazily; a statement-end optional drop would be a UAF:\n%s", fn)
	}
}

// A local shadowing a module getter's name is a variable, not a call: it keeps
// its own binding drop and must not be reclassified as a fresh owned temp (which
// would drop it at statement end while the binding still owns it).
func TestT2049_LocalShadowingGetterNameStaysAVariable(t *testing.T) {
	ir := codegentest.GenerateIR(t, t2049Decls+`
		probe() { string? probe_dir = "sha" + "dow"; borrow_opt(probe_dir); }
		main() { probe(); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	if strings.Contains(fn, "opttmp.drop") {
		t.Fatalf("a local shadowing a getter name is a variable, not an owned temp:\n%s", fn)
	}
	// Its own binding drop is still emitted.
	codegentest.AssertContains(t, fn, "optdrop.check")
}

// The corrected IdentExpr case in tupleArgIsCallerOwnedTemp also reaches the
// T1233 tuple and T1466 array borrow-param registrations, whose bare-getter
// arguments were untracked for the identical reason. registerTupleStmtTemp now
// shares newAggregateStmtTempSlots with the optional temp, so this also covers
// that extraction.
func TestT2049_BareGetterTupleBorrowRegistersTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		get probe_tuple (string, int) { return ("tup" + "le", 1); }
		borrow_tuple((string, int) t) {}
		probe() { borrow_tuple(probe_tuple); }
		main() { probe(); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	codegentest.AssertContains(t, fn, "tuptmp.drop")
	codegentest.AssertContains(t, fn, "@promise_string_drop")
}

func TestT2049_BareGetterArrayBorrowRegistersTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		get probe_array string[2] { return ["a" + "1", "b" + "2"]; }
		borrow_array(string[2] a) {}
		probe() { borrow_array(probe_array); }
		main() { probe(); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	codegentest.AssertContains(t, fn, "arrtmp.drop")
	codegentest.AssertContains(t, fn, "@promise_string_drop")
}

// The complementary half: a plain LOCAL tuple variable still owns its own
// bindingDropTuple, so it must not also be registered as a caller temp.
func TestT2049_LocalTupleArgNotRegisteredAsTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		borrow_tuple((string, int) t) {}
		probe() { (string, int) v = ("a" + "b", 1); borrow_tuple(v); }
		main() { probe(); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	if strings.Contains(fn, "tuptmp.drop") {
		t.Fatalf("a tuple LOCAL owns its own binding drop and must not get a caller temp:\n%s", fn)
	}
}

// Both classifiers peel ParenExpr before deciding, so `(getter)` must still be
// recognized as a getter call rather than a borrowed place.
func TestT2049_ParenthesizedGetterDiscardDrops(t *testing.T) {
	ir := codegentest.GenerateIR(t, t2049Decls+`
		probe() { (probe_dir); }
		main() { probe(); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	codegentest.AssertContains(t, fn, "discard.drop")
	codegentest.AssertContains(t, fn, "@promise_string_drop")
}

func TestT2049_ParenthesizedGetterBorrowArgRegistersTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, t2049Decls+`
		probe() { borrow_opt((probe_dir)); }
		main() { probe(); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	codegentest.AssertContains(t, fn, "opttmp.drop")
}

// An inner kind with no i8* drop function of its own: the enum arm of
// emitOptionalValueDrop must be reached through the optional temp, dropping the
// variant payload rather than silently emitting nothing.
func TestT2049_OptionalEnumInnerBorrowDropsVariantPayload(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Parcel { Text(string s), Num(int n), }
		get probe_enum Parcel? { return Parcel.Text(s: "par" + "cel"); }
		borrow_enum(Parcel? v) {}
		probe() { borrow_enum(probe_enum); }
		main() { probe(); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	codegentest.AssertContains(t, fn, "opttmp.drop")
	// The enum arm stores to a temp alloca and calls the synthesized enum drop.
	codegentest.AssertContainsMatch(t, fn, `call void @Parcel\.drop`)
}

// A closure inner owns a heap env; the Signature arm must free it.
func TestT2049_OptionalClosureInnerBorrowFreesEnv(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		get probe_closure (() -> int)? { int cap = 7; return || -> cap; }
		borrow_closure((() -> int)? v) {}
		probe() { borrow_closure(probe_closure); }
		main() { probe(); }
	`)
	fn := codegentest.FuncBody(t, ir, "probe")
	codegentest.AssertContains(t, fn, "opttmp.drop")
}
