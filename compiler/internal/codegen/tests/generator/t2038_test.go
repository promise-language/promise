package generator

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// coroutineOf returns the IR of the generator coroutine the factory wrapper
// wrapperName calls. The wrapper allocates the yield slot and calls
// @.generator.N — N is unique per generator function, but depends on how many
// generators precede it, so it is read off the call rather than hard-coded.
func coroutineOf(t *testing.T, ir, wrapperName string) string {
	t.Helper()
	wrapper := codegentest.ExtractFunction(ir, wrapperName)
	if wrapper == "" {
		t.Fatalf("expected %s in IR", wrapperName)
	}
	callIdx := strings.Index(wrapper, "@.generator.")
	if callIdx < 0 {
		t.Fatalf("expected %s to call a generator coroutine", wrapperName)
	}
	parenIdx := strings.Index(wrapper[callIdx:], "(")
	if parenIdx < 0 {
		t.Fatal("malformed coroutine call")
	}
	coroName := wrapper[callIdx+1 : callIdx+parenIdx] // ".generator.N"
	gen := strings.Index(ir, "define i8* @"+coroName+"(")
	if gen < 0 {
		t.Fatalf("expected coroutine %s in IR", coroName)
	}
	rest := ir[gen:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

// block returns the text of the basic block whose label starts with prefix, up to
// the next blank line (llir separates blocks with one).
func block(t *testing.T, fn, prefix string) string {
	t.Helper()
	start := strings.Index(fn, "\n"+prefix)
	if start < 0 {
		t.Fatalf("expected a %s block", prefix)
	}
	b := fn[start+1:]
	if end := strings.Index(b, "\n\n"); end >= 0 {
		b = b[:end]
	}
	return b
}

const t2038FreshGenerator = `
	fresh(int n) stream[string] {
		int i = 0;
		while i < n {
			yield "s{i}";
			i += 1;
		}
	}
`

// T2038: the consumer's for-in binding OWNS each yielded element — it has a drop
// flag, a move out of it (`last = s`) clears the flag, and the element is dropped
// at the end of the iteration otherwise.
func TestT2038_GeneratorForInBindingOwnsElement(t *testing.T) {
	ir := codegentest.GenerateIR(t, t2038FreshGenerator+`
		keep_last(int n) int {
			string last = "";
			for s in fresh(n) {
				last = s;
			}
			return last.len;
		}
		read_only(int n) int {
			int total = 0;
			for s in fresh(n) {
				total += s.len;
			}
			return total;
		}
		main() {}
	`)
	keep := codegentest.ExtractFunction(ir, "__user.keep_last")
	codegentest.AssertContains(t, keep, "%s.dropflag = alloca i1")
	codegentest.AssertContains(t, keep, "store i1 true, i1* %s.dropflag")
	codegentest.AssertContains(t, keep, "store i1 false, i1* %s.dropflag")

	read := codegentest.ExtractFunction(ir, "__user.read_only")
	codegentest.AssertContains(t, read, "%s.dropflag = alloca i1")
	codegentest.AssertContains(t, read, "call void @promise_string_drop(")
}

// T2038: `yield "s{i}"` hands the concatenated string to the consumer and drops
// the statement's other temps (the int→string intermediate) BEFORE suspending, so
// an abandoned generator cannot strand them. Nothing is dropped on resume.
func TestT2038_YieldDrainsTempsBeforeSuspend(t *testing.T) {
	ir := codegentest.GenerateIR(t, t2038FreshGenerator+`
		main() {
			for s in fresh(2) {
				break;
			}
		}
	`)
	coro := coroutineOf(t, ir, "__user.fresh")
	concat := strings.Index(coro, "@promise_string_concat(")
	if concat < 0 {
		t.Fatal("expected the yielded string to be concatenated")
	}
	suspend := strings.Index(coro[concat:], "@llvm.coro.suspend(token none, i1 false)")
	if suspend < 0 {
		t.Fatal("expected the yield's suspend after the concat")
	}
	codegentest.AssertContains(t, coro[concat:concat+suspend], "call void @promise_string_drop(")
	codegentest.AssertNotContains(t, block(t, coro, "yield.resume"), "@promise_string_drop(")
	// A fresh value cannot alias a param: no borrowed-param clone is emitted.
	codegentest.AssertNotContains(t, coro, "alias.borrow.clone")
}

// T2038: `yield s` moves the local — its drop flag is cleared before the suspend,
// so neither the destroy path nor the scope end frees what the consumer owns.
func TestT2038_YieldMovesLocal(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		moved(int n) stream[string] {
			int i = 0;
			while i < n {
				string s = "l{i}";
				yield s;
				i += 1;
			}
		}
		main() {
			for s in moved(2) {}
		}
	`)
	coro := coroutineOf(t, ir, "__user.moved")
	// The block that stores into the yield slot and suspends.
	slot := strings.Index(coro, "load i8*, i8** %yield_slot.addr")
	if slot < 0 {
		t.Fatal("expected the yield to store into the yield slot")
	}
	start := strings.LastIndex(coro[:slot], "\n\n")
	yieldBlock := coro[start:slot]
	codegentest.AssertContains(t, yieldBlock, "store i1 false, i1* %s.dropflag")
}

// T2038: `yield*` over a vector hands each element to the consumer as an owned
// copy — the vector still owns (and later drops) its own elements.
func TestT2038_YieldStarVectorCopiesElements(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		each() stream[string] {
			string[] v = ["a{1}", "b{2}"];
			yield* v;
		}
		main() {
			for s in each() {}
		}
	`)
	coro := coroutineOf(t, ir, "__user.each")
	codegentest.AssertContains(t, block(t, coro, "yieldstar.vec.yield"), "strdup.copy")
	codegentest.AssertContains(t, coro, "@promise_string_new(")
}

// T2038: a `yield*` over a TEMP vector keeps that temp alive across every
// suspend; abandoning the generator mid-delegation must drop it in the per-yield
// destroy block, since the statement end that normally frees it is never reached.
func TestT2038_YieldStarTempDroppedOnDestroy(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		make() string[] {
			string[] v = ["a{1}", "b{2}"];
			return v;
		}
		each() stream[string] {
			yield* make();
		}
		main() {
			for s in each() {
				break;
			}
		}
	`)
	coro := coroutineOf(t, ir, "__user.each")
	codegentest.AssertContains(t, block(t, coro, "yield.cleanup"), "tmp.drop")
}

// T2038: `yield o!` over a BORROWED optional param must hand the consumer a copy
// when the value aliases the caller's argument (§6.2 duplicate-on-alias, applied
// at the yield because a consumer has no call site to apply it).
func TestT2038_YieldBorrowedParamClonesOnAlias(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		unwrap(string? o) stream[string] {
			yield o!;
		}
		main() {
			string? o = "o{1}";
			for s in unwrap(o) {}
		}
	`)
	coro := coroutineOf(t, ir, "__user.unwrap")
	codegentest.AssertContains(t, block(t, coro, "alias.borrow.clone"), "strdup.copy")
	codegentest.AssertContains(t, coro, "@promise_string_new(")
}

// T2038: a failable factory resumes eagerly, so a discarded call leaves its first
// yielded value in the slot — the discard must drop it before the destroy.
func TestT2038_DiscardedFailableGeneratorDropsPendingValue(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		ffresh!(int n) stream[string] {
			int i = 0;
			while i < n {
				yield "f{i}";
				i += 1;
			}
		}
		main() {
			ffresh(3)?!;
		}
	`)
	pending := block(t, ir, "discard.gen.pending")
	codegentest.AssertContains(t, pending, "call void @promise_string_drop(")
}

// T2038: a closure cannot be copied, so `yield*` over a closure vector the
// generator owns MOVES each one out, leaving the empty closure the vector's drop
// skips — while a copyable element is copied instead (TestT2038_YieldStarVector...).
func TestT2038_YieldStarOwnedClosuresMovedOut(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		fns() stream[() -> int] {
			(() -> int)[] v = [|| -> 1, || -> 2];
			yield* v;
		}
		main() {
			for f in fns() {
				f();
			}
		}
	`)
	coro := coroutineOf(t, ir, "__user.fns")
	codegentest.AssertContains(t, block(t, coro, "yieldstar.vec.yield"), "store { i8*, i8* } zeroinitializer")
}

// T2038: a generic generator decides its yield-site copies per instantiation —
// the element type is substituted before the borrowed-param alias check and the
// `yield*` element copy, so `T = string` copies exactly as a concrete `string`
// generator does.
func TestT2038_GenericGeneratorCopiesPerInstantiation(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		generic_unwrap[T](T? o) stream[T] {
			yield o!;
		}
		generic_delegate[T](T[] items) stream[T] {
			T[] v = items.clone();
			yield* v;
		}
		main() {
			string? o = "g{1}";
			for s in generic_unwrap(o) {}
			string[] names = ["a{1}"];
			for s in generic_delegate(names) {}
		}
	`)
	unwrap := coroutineOf(t, ir, `"generic_unwrap[string]"`)
	codegentest.AssertContains(t, block(t, unwrap, "alias.borrow.clone"), "strdup.copy")
	delegate := coroutineOf(t, ir, `"generic_delegate[string]"`)
	codegentest.AssertContains(t, block(t, delegate, "yieldstar.vec.yield"), "strdup.copy")
}

// T2038: a discarded failable generator of VALUE elements still has its first
// yield in the slot, but there is nothing to drop — only the destroy is emitted.
func TestT2038_DiscardedFailableValueGeneratorNoPendingDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		fints!(int n) stream[int] {
			int i = 0;
			while i < n {
				yield i;
				i += 1;
			}
		}
		main() {
			fints(3)?!;
		}
	`)
	codegentest.AssertContains(t, ir, "discard.gen.cleanup")
	codegentest.AssertNotContains(t, ir, "discard.gen.pending")
}

// T2038: a closure vector `yield*` does NOT own (a borrowed receiver's field) is
// rejected by ownership, so codegen never sees it; copyable elements from the same
// kind of source are copied, never moved out (no slot is nulled).
func TestT2038_YieldStarBorrowedFieldCopiesNotMoves(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Names {
			string[] items;
			all(this) stream[string] {
				yield* this.items;
			}
		}
		main() {
			Names n = Names(items: ["a{1}"]);
			for s in n.all() {}
		}
	`)
	coro := coroutineOf(t, ir, "Names.all")
	yieldBlock := block(t, coro, "yieldstar.vec.yield")
	codegentest.AssertContains(t, yieldBlock, "strdup.copy")
	codegentest.AssertNotContains(t, yieldBlock, "zeroinitializer")
}

// T2038: `yield (o!)` over a BORROWED optional param is still recognised as
// borrowed through the parentheses — the consumer gets a copy on alias and the
// caller's optional is never neutralized. `yield n.text!` over an owned local's
// FIELD is copied by the field access itself (the escape path's field dup), so it
// needs no alias check; the field keeps, and later drops, its own string.
func TestT2038_YieldUnwrapSourceShapes(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Note {
			string? text;
		}
		parens(string? o) stream[string] {
			yield (o!);
		}
		owned_field() stream[string] {
			Note n = Note(text: "l{1}");
			yield n.text!;
		}
		main() {
			string? o = "p{1}";
			for s in parens(o) {}
			for s in owned_field() {}
		}
	`)
	parens := coroutineOf(t, ir, "__user.parens")
	codegentest.AssertContains(t, parens, "alias.borrow.clone")
	codegentest.AssertNotContains(t, parens, "store i1 false, i1* %")

	owned := coroutineOf(t, ir, "__user.owned_field")
	codegentest.AssertNotContains(t, owned, "alias.borrow.clone")
	slot := strings.Index(owned, "load i8*, i8** %yield_slot.addr")
	if slot < 0 {
		t.Fatal("expected the yield to store into the yield slot")
	}
	codegentest.AssertContains(t, owned[:slot], "@promise_string_new(")
}
