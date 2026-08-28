package regress4

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1385 / §17.2 explicit-return style: `return <expr>` inside a `go {}` / `go! {}`
// block produces the GOROUTINE's result. genReturnStmt's coroutine branch used to
// handle only the bare form, so a `return <expr>` either fell into the enclosing
// function's `ret` path or stored nothing. It now evaluates the value, moves it out
// of the coroutine frame, and stores it into G.result_ptr via the shared
// storeGoResultAgg helper before branching to the final suspend.

// Non-failable block: the returned value — not a zero — reaches the result buffer.
func TestT1385_GoBlockExplicitReturnStoresValue(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			t := go { return 42; };
			v := <-t;
		}
	`)
	goro := codegentest.ExtractGoroutineBody(t, ir)

	if !strings.Contains(goro, "go.store_result") {
		t.Fatalf("expected the explicit-return exit to store into G.result_ptr (go.store_result):\n%s", goro)
	}
	// The stored value must be the returned 42, not the T1392 bare-return zero.
	if !strings.Contains(goro, "store i64 42") {
		t.Errorf("expected the RETURNED value (42) to be stored, not a zero:\n%s", goro)
	}
	if !strings.Contains(goro, "final.suspend") {
		t.Errorf("expected the explicit-return exit to branch to the coroutine final.suspend:\n%s", goro)
	}
	// The coroutine ramp returns i8*; the value must be stored, never `ret`ed.
	if strings.Contains(goro, "ret i64") {
		t.Errorf("the go-block explicit-return path must not `ret` the value from the coroutine ramp:\n%s", goro)
	}
}

// Failable block: the returned value is wrapped into the {ok, value, err}
// aggregate before the store, exactly like the trailing-value exit.
func TestT1385_FailableGoBlockExplicitReturnStoresOkAggregate(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		produce!(int x) int { if x < 0 { raise error("neg"); } return x; }
		test() {
			t := go! {
				base := produce(1)?^;
				return base + 41;
			};
			v := (<-t)?!;
		}
	`)
	goro := codegentest.ExtractGoroutineBody(t, ir)

	if !strings.Contains(goro, "go.store_result") {
		t.Fatalf("expected the explicit-return exit to store into G.result_ptr (go.store_result):\n%s", goro)
	}
	// wrapOk builds the aggregate by inserting the value into an {i1, T, i8*}.
	if !strings.Contains(goro, "{ i1, i64, i8* }") {
		t.Errorf("expected the returned value to be wrapped into the failable aggregate:\n%s", goro)
	}
	if strings.Contains(goro, "ret { i1, i64, i8* }") {
		t.Errorf("the go!-block explicit-return path must not `ret` the aggregate from the coroutine ramp:\n%s", goro)
	}
}

// Fire-and-forget: no result buffer exists, so nothing is stored — and the
// returned heap value must be freed by the coroutine body's cleanup rather than
// claimed out of it (claiming it with no sink would leak).
func TestT1385_FireAndForgetGoBlockReturnDiscardsAndFrees(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			go {
				s := "a" + "b";
				return s;
			};
		}
	`)
	goro := codegentest.ExtractGoroutineBody(t, ir)

	if strings.Contains(goro, "go.store_result") {
		t.Errorf("a fire-and-forget go block has no result buffer; it must not store a result:\n%s", goro)
	}
	if !strings.Contains(goro, "@promise_string_drop") {
		t.Errorf("the discarded heap return value must still be freed inside the coroutine:\n%s", goro)
	}
	if !strings.Contains(goro, "final.suspend") {
		t.Errorf("expected the explicit-return exit to branch to the coroutine final.suspend:\n%s", goro)
	}
}

// goroutineBodies returns every `.goroutine.N` coroutine definition in the IR,
// in emission order. extractGoroutineBody only yields the first one, which is not
// enough for the nested-block and generator cases below, where the point IS that
// each coroutine carries its own result store.
func goroutineBodies(t *testing.T, ir string) []string {
	t.Helper()
	var bodies []string
	rest := ir
	for {
		i := strings.Index(rest, "define i8* @.goroutine.")
		if i < 0 {
			break
		}
		rest = rest[i:]
		end := strings.Index(rest, "\n}\n")
		if end < 0 {
			bodies = append(bodies, rest)
			break
		}
		bodies = append(bodies, rest[:end])
		rest = rest[end:]
	}
	if len(bodies) == 0 {
		t.Fatalf("no goroutine coroutine definitions found in IR:\n%s", ir)
	}
	return bodies
}

// A heap value returned from an awaited block moves out: its drop flag is cleared
// so the coroutine's scope cleanup does not free the value the receiver now owns.
func TestT1385_GoBlockExplicitReturnHeapValueMovesOut(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			t := go {
				s := "a" + "b";
				return s;
			};
			v := <-t;
		}
	`)
	goro := codegentest.ExtractGoroutineBody(t, ir)

	if !strings.Contains(goro, "go.store_result") {
		t.Fatalf("expected the explicit-return exit to store into G.result_ptr:\n%s", goro)
	}
	// clearDropFlag stores 0 into the local's drop flag at the move site.
	if !strings.Contains(goro, "store i1 false, i1* %s.drop") {
		t.Errorf("expected the returned local's drop flag to be cleared at the move site:\n%s", goro)
	}
}

// --- The all-return trailing if/else, and genGoBlock's fall-through default ---

// Both arms of the block's trailing if/else `return`, so genBlockValue finds no
// trailing value — but genIfStmtValue still leaves a live (predecessor-less)
// merge block behind. Each arm must store ITS value, and the merge must store the
// defined default rather than fall through to the final suspend with an
// undefined G.result_ptr (the T1392 defect class).
func TestT1385_AllReturnIfElseStoresEachArmAndDefinesTheFallThrough(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			c := true;
			t := go {
				if c { return 7; } else { return 2; }
			};
			v := <-t;
		}
	`)
	goro := codegentest.ExtractGoroutineBody(t, ir)

	if !strings.Contains(goro, "store i64 7") || !strings.Contains(goro, "store i64 2") {
		t.Errorf("expected BOTH arms' returned values to be stored:\n%s", goro)
	}
	// Three stores: the two arms plus the merge-block default.
	if n := strings.Count(goro, "go.store_result."); n < 6 { // each block: label + branch reference
		t.Errorf("expected three result stores (two arms + fall-through default), saw %d label/ref mentions:\n%s", n, goro)
	}
	if !strings.Contains(goro, "store i64 0") {
		t.Errorf("expected the fall-through merge block to store the defined default (zero):\n%s", goro)
	}
	if strings.Contains(goro, "ret i64") {
		t.Errorf("the coroutine ramp returns i8*; no arm may `ret` its value:\n%s", goro)
	}
}

// Same shape in a FAILABLE block: the fall-through default is the
// {ok, zero, null} aggregate, not the raw zero — storeGoResultDefault's failable
// branch with a non-void result type.
func TestT1385_FailableAllReturnIfElseFallThroughStoresOkAggregate(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		produce!(int x) int { if x < 0 { raise error("neg"); } return x; }
		test() {
			c := true;
			t := go! {
				base := produce(1)?^;
				if c { return base + 6; } else { return base + 1; }
			};
			v := (<-t)?!;
		}
	`)
	goro := codegentest.ExtractGoroutineBody(t, ir)

	if !strings.Contains(goro, "go.store_result") {
		t.Fatalf("expected result stores on the failable all-return if/else:\n%s", goro)
	}
	// wrapOk on the fall-through default builds {false, 0, null} — the zero value
	// slot plus the null error slot is what makes the buffer DEFINED.
	if !strings.Contains(goro, "i64 0, 1") {
		t.Errorf("expected the fall-through default to wrap the result type's zero into the ok aggregate:\n%s", goro)
	}
	if strings.Contains(goro, "ret { i1, i64, i8* }") {
		t.Errorf("the failable coroutine ramp must not `ret` the aggregate:\n%s", goro)
	}
}

// --- `go! { return <bare failable call>; }` — the auto-propagated return value ---

// The returned expression is a bare failable call in a failable scope: its error
// propagates into the task and its success value becomes the result. genReturnStmt
// must unwrap it (genAutoPropagateValue) BEFORE the cleanup and the ok-wrap, so
// both the error and the success exits reach the same result buffer.
func TestT1385_FailableAutoPropagatedReturnValueTakesBothExits(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		produce!(int x) int { if x < 0 { raise error("neg"); } return x; }
		test() {
			t := go! { return produce(5); };
			v := (<-t)?!;
		}
	`)
	goro := codegentest.ExtractGoroutineBody(t, ir)

	// The auto-propagate split: an error arm and a success arm.
	if !strings.Contains(goro, "auto.propagate") || !strings.Contains(goro, "auto.ok") {
		t.Fatalf("expected the returned bare failable call to be auto-propagated:\n%s", goro)
	}
	// BOTH arms store into G.result_ptr — the error aggregate and the ok aggregate.
	if n := strings.Count(goro, "go.store_result."); n < 4 {
		t.Errorf("expected both the auto-propagate error and ok arms to store a result, saw %d label/ref mentions:\n%s", n, goro)
	}
	if strings.Contains(goro, "ret { i1, i64, i8* }") {
		t.Errorf("the failable coroutine ramp must not `ret` the aggregate:\n%s", goro)
	}
}

// --- Nested go blocks: each coroutine stores into its OWN result buffer ---

func TestT1385_NestedGoBlocksEachStoreTheirOwnResult(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			t := go {
				inner := go { return 5; };
				return (<-inner) + 1;
			};
			v := <-t;
		}
	`)
	bodies := goroutineBodies(t, ir)
	// One coroutine per block — the nested block is NOT inlined into its parent.
	if len(bodies) < 2 {
		t.Fatalf("expected a separate coroutine body for each of the two nested blocks, got %d", len(bodies))
	}
	stores := 0
	for _, b := range bodies {
		if strings.Contains(b, "go.store_result") {
			stores++
		}
	}
	if stores < 2 {
		t.Errorf("expected BOTH nested go-block coroutines to store their own result, only %d did:\n%s",
			stores, strings.Join(bodies, "\n----\n"))
	}
	// The inner block's literal is stored somewhere; the outer stores the sum.
	joined := strings.Join(bodies, "\n")
	if !strings.Contains(joined, "store i64 5") {
		t.Errorf("expected the inner block's returned literal to be stored:\n%s", joined)
	}
}

// --- A go block nested in a GENERATOR body ---

// `c.inGenerator` stays set inside the nested block, so genReturnStmt's generator
// branch would have swallowed the `return <expr>` — storing nothing into
// G.result_ptr and handing `<-t` poison. c.coroutineReturnBlock discriminates:
// non-nil only inside the nested go block.
func TestT1385_GoBlockInsideGeneratorStoresItsOwnResult(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		gen() stream[int] {
			t := go { return 42; };
			yield <-t;
		}
		test() {
			for v in gen() { }
		}
	`)
	bodies := goroutineBodies(t, ir)
	found := false
	for _, b := range bodies {
		if strings.Contains(b, "go.store_result") && strings.Contains(b, "store i64 42") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the go block nested in the generator to store its returned value into G.result_ptr:\n%s",
			strings.Join(bodies, "\n----\n"))
	}
}

// --- `return <void expr>` in a FAILABLE block: T = Void, but a buffer exists ---

// `go! { …; return ch.send(x); }` returns a void expression, so there is no value
// to wrap — but the block still HAS an {ok, err} buffer, and the exit must define
// it (wrapOk(nil)) rather than leave it poisoned the way T1392 described.
func TestT1385_FailableVoidExplicitReturnStoresOkAggregate(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		produce!(int x) int { if x < 0 { raise error("neg"); } return x; }
		test() {
			ch := channel[int](1);
			t := go! {
				base := produce(1)?^;
				return ch.send(base);
			};
			(<-t)? e { };
		}
	`)
	goro := codegentest.ExtractGoroutineBody(t, ir)

	if !strings.Contains(goro, "go.store_result") {
		t.Fatalf("expected the void explicit-return exit to store into G.result_ptr:\n%s", goro)
	}
	// The void aggregate is the two-field {i1 ok, i8* err} — no value slot.
	if !strings.Contains(goro, "insertvalue { i1, i8* } undef, i1 false, 0") {
		t.Errorf("expected the void explicit-return exit to store the ok aggregate:\n%s", goro)
	}
	if !strings.Contains(goro, "insertvalue { i1, i8* } %") || !strings.Contains(goro, "i8* null, 1") {
		t.Errorf("expected the ok aggregate's error slot to be null (defined, not poison):\n%s", goro)
	}
}

// --- A `return` from inside a loop takes the goroutine exit ---

// The loop back-edge and the yield check make this the shape most likely to fall
// back onto a plain `ret`; the coroutine ramp returns i8*, so it must not.
func TestT1385_ReturnFromInsideLoopBranchesToFinalSuspend(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			t := go {
				for i in 0..10 {
					if i == 3 { return i * 10; }
				}
				return -1;
			};
			v := <-t;
		}
	`)
	goro := codegentest.ExtractGoroutineBody(t, ir)

	if strings.Contains(goro, "ret i64") {
		t.Errorf("a return from inside a loop must not `ret` from the coroutine ramp:\n%s", goro)
	}
	// Two exits (the in-loop one and the fall-through one) each store their value.
	if n := strings.Count(goro, "go.store_result."); n < 4 {
		t.Errorf("expected both the in-loop and the fall-through return to store a result, saw %d label/ref mentions:\n%s", n, goro)
	}
}

// --- Returning a borrowed field: the value is dup'd, not aliased ---

// `return this.tag;` hands the receiver ownership of a string the instance still
// owns, so the store must be of a DUP — otherwise `<-t` and the field alias one
// allocation and the first drop frees the other's data.
func TestT1385_ReturnBorrowedFieldDupsBeforeTheStore(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder {
			string tag;
			spawn_tag(this) string {
				t := go { return this.tag; };
				return <-t;
			}
		}
		test() {
			h := Holder(tag: "held");
			s := h.spawn_tag();
		}
	`)
	goro := codegentest.ExtractGoroutineBody(t, ir)

	// The string dup is emitted inline (a strdup.copy / strdup.merge pair), not as
	// a runtime call.
	if !strings.Contains(goro, "strdup.copy") {
		t.Errorf("expected the borrowed field to be dup'd before it escapes into the result buffer:\n%s", goro)
	}
	if !strings.Contains(goro, "go.store_result") {
		t.Errorf("expected the dup'd field value to be stored into G.result_ptr:\n%s", goro)
	}
}

// --- Inside a generic function the returned value's type is substituted ---

// genGoBlock seeds currentRetType from the block's element type, which is the
// UNSUBSTITUTED type param inside a generic body; genReturnStmt substitutes it
// through typeSubst before the coercions and the store. Each monomorphized
// instance must therefore store its own concrete type.
func TestT1385_GenericGoBlockReturnStoresTheSubstitutedType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		wrap[T](T x) T {
			t := go { return x; };
			return <-t;
		}
		test() {
			a := wrap[int](7);
			b := wrap[string]("s");
		}
	`)
	bodies := goroutineBodies(t, ir)
	var storesI64, storesPtr bool
	for _, b := range bodies {
		if !strings.Contains(b, "go.store_result") {
			continue
		}
		if strings.Contains(b, "store i64 %") {
			storesI64 = true
		}
		if strings.Contains(b, "store i8* %") {
			storesPtr = true
		}
	}
	if !storesI64 || !storesPtr {
		t.Errorf("expected the int instance to store an i64 and the string instance an i8* (got i64=%v, ptr=%v):\n%s",
			storesI64, storesPtr, strings.Join(bodies, "\n----\n"))
	}
}

// --- Fire-and-forget × a BORROWED return value: dup'd, then discarded ---

// `needsDup` runs before the goSink gate, so a borrowed source (a field of the
// captured `this`) is dup'd on the way out even when there is no result buffer to
// store it into. The dup must then be freed by the coroutine's own cleanup: the
// claim*Temp calls that would hand it to the receiver are gated off for
// fire-and-forget, so leaving them ungated would leak the dup on every spawn.
func TestT1385_FireAndForgetReturnOfBorrowedFieldDupsThenFrees(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type H {
			string tag;
			spawn(this) {
				go { return this.tag; };
			}
		}
		test() {
			h := H(tag: "a" + "b");
			h.spawn();
		}
	`)
	goro := codegentest.ExtractGoroutineBody(t, ir)

	if strings.Contains(goro, "go.store_result") {
		t.Errorf("a fire-and-forget block has no result buffer; it must not store a result:\n%s", goro)
	}
	// The string dup is lowered inline (a null-check + promise_string_new copy),
	// not as a call to a dup runtime helper.
	if !strings.Contains(goro, "strdup.copy") || !strings.Contains(goro, "@promise_string_new") {
		t.Errorf("a borrowed field returned from the block must still be dup'd (the borrow is not the block's to move):\n%s", goro)
	}
	if !strings.Contains(goro, "@promise_string_drop") {
		t.Errorf("the discarded dup must be freed inside the coroutine, not leaked:\n%s", goro)
	}
}

// The same shape one step removed: a borrowed PARAMETER of the enclosing function,
// captured by the block. Pins that the dup/discard pairing does not depend on the
// borrow coming from `this`.
func TestT1385_FireAndForgetReturnOfBorrowedParamDupsThenFrees(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		spawn(string s) {
			go { return s; };
		}
		test() { spawn("a" + "b"); }
	`)
	goro := codegentest.ExtractGoroutineBody(t, ir)

	if strings.Contains(goro, "go.store_result") {
		t.Errorf("a fire-and-forget block has no result buffer; it must not store a result:\n%s", goro)
	}
	if !strings.Contains(goro, "@promise_string_drop") {
		t.Errorf("the discarded return value must be freed inside the coroutine, not leaked:\n%s", goro)
	}
}

// --- The result buffer's LLVM type follows the block's inferred T ---

// goResultBufferType types both the store and the storeGoResultDefault zero. A
// non-i64 scalar is the cheapest way to pin that the buffer is not hard-wired to
// the int shape — a double result must be stored as a double.
func TestT1385_GoBlockExplicitReturnStoresNonIntScalarWidth(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			t := go { return 1.5; };
			v := <-t;
		}
	`)
	goro := codegentest.ExtractGoroutineBody(t, ir)

	if !strings.Contains(goro, "go.store_result") {
		t.Fatalf("expected the explicit-return exit to store into G.result_ptr:\n%s", goro)
	}
	if !strings.Contains(goro, "store double ") {
		t.Errorf("expected the f64 result to be stored as a double, not coerced to the int shape:\n%s", goro)
	}
}
