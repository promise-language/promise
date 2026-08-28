package regress1

import (
	"regexp"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1064: in `vov[i].push(f(vov))`, genVectorMethodCall used to capture the inner
// element slot (a GEP into the outer vector's heap buffer) BEFORE evaluating the
// argument. If the argument reallocated the outer vector, that buffer was freed and
// the captured slot dangled, so the post-push store-back wrote the grown inner
// pointer through the freed slot (use-after-free) → "invalid free (bad header
// magic)".
//
// The fix defers the write-back slot computation to AFTER argument evaluation,
// recomputing it from a freshly-loaded outer pointer (reusing a once-evaluated
// index). This test pins that ordering in the emitted IR: after the receiver push,
// the outer pointer %vov is re-loaded and a fresh element-slot bounds check
// (nestedpush.ok) is emitted before the grown pointer is stored back — proving the
// slot is recomputed rather than reused from before the argument ran.
func TestT1064NestedPushArgReallocsOuter(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		grow_outer(int[][] ~vov) int {
			vov.push(Vector[int](capacity: 1));
			return 7;
		}
		run() {
			int[][] vov = Vector[Vector[int]](capacity: 1);
			int[] inner = Vector[int](capacity: 2);
			inner.push(1);
			inner.push(2);
			vov.push(move inner);
			vov[0].push(grow_outer(vov));
		}
		main() {}
	`)

	body := codegentest.FuncBody(t, ir, "run")

	// The argument's reallocating call must appear before the receiver push.
	growIdx := strings.Index(body, "call i64 @__user.grow_outer(")
	if growIdx < 0 {
		t.Fatalf("no @grow_outer call in run() body:\n%s", body)
	}
	post := body[growIdx:] // everything at/after the argument evaluation

	// The receiver push is the first vector push AFTER the argument is evaluated.
	pushRe := regexp.MustCompile(`(%\w+) = call i8\* @promise_vector_push\(`)
	pm := pushRe.FindStringSubmatchIndex(post)
	if pm == nil {
		t.Fatalf("no receiver promise_vector_push after the argument call:\n%s", body)
	}
	pushResult := post[pm[2]:pm[3]]
	region := post[pm[0]:] // from the receiver push onward — the store-back region

	// Store-back recompute: the outer pointer %vov is re-loaded from its alloca
	// AFTER the argument reallocated it. Pre-fix, the slot was GEP'd once from an
	// early %vov load (before grow_outer) and reused — no reload here.
	if !regexp.MustCompile(`load i8\*, i8\*\* %vov`).MatchString(region) {
		t.Fatalf("outer pointer %%vov is not re-loaded after the receiver push — the "+
			"store-back would reuse a slot into the freed pre-realloc buffer "+
			"(T1064 regression):\n%s", body)
	}

	// A fresh element-slot bounds check after the push confirms the slot is
	// recomputed at store-back time rather than captured once before the argument.
	if !strings.Contains(region, "nestedpush.ok") {
		t.Fatalf("no nestedpush.ok bounds-check after the receiver push — the element "+
			"slot is not recomputed from the fresh outer pointer (T1064 regression):\n%s", body)
	}

	// The push result is stored back into the recomputed slot, after the reload.
	storeRe := regexp.MustCompile(`store i8\* ` + regexp.QuoteMeta(pushResult) + `, i8\*\* %\w+`)
	if !storeRe.MatchString(region) {
		t.Fatalf("push result %s is not stored back into the recomputed slot "+
			"(T1064 regression):\n%s", pushResult, body)
	}

	// Two element-slot bounds-check blocks total: the early read (to load the inner
	// pointer) and the store-back recompute. Pins that the slot is addressed twice.
	labelRe := regexp.MustCompile(`nestedpush\.ok\.\d+:`)
	if n := len(labelRe.FindAllString(body, -1)); n < 2 {
		t.Fatalf("expected >=2 nestedpush.ok bounds-check blocks (early read + "+
			"store-back recompute), got %d:\n%s", n, body)
	}
}

// T1064: the outer container of a `vov[i].push(...)` receiver can be a MUTABLE-BORROW
// parameter (`int[][] ~vov`) rather than a plain local. That reaches the mutref
// branch of indexTargetIsReallocatablePlace, which must still classify it as a
// reallocatable place so the store-back recomputes the element slot from a fresh
// (post-argument) outer pointer instead of reusing an early slot into a buffer the
// argument may have freed. This pins that the recompute is emitted for a mutref outer.
func TestT1064MutRefOuterRecompute(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		grow_via(int[][] ~vov) int {
			vov.push(Vector[int](capacity: 1));
			return 7;
		}
		do_push(int[][] ~vov) {
			vov[0].push(grow_via(vov));
		}
		main() {}
	`)

	body := codegentest.FuncBody(t, ir, "do_push")

	// The argument's reallocating call must precede the receiver push.
	growIdx := strings.Index(body, "call i64 @__user.grow_via(")
	if growIdx < 0 {
		t.Fatalf("no @grow_via call in do_push() body:\n%s", body)
	}
	region := body[growIdx:]

	// The receiver push, then a store-back that recomputes the element slot — a
	// fresh nestedpush.ok bounds-check block after the argument ran. Pre-fix, the
	// slot was captured once before grow_via and reused (dangling for a mutref outer
	// whose pointee the reborrowed push reallocated).
	pushIdx := strings.Index(region, "call i8* @promise_vector_push(")
	if pushIdx < 0 {
		t.Fatalf("no receiver promise_vector_push after the argument call:\n%s", body)
	}
	if !strings.Contains(region[pushIdx:], "nestedpush.ok") {
		t.Fatalf("no nestedpush.ok bounds-check after the receiver push — the slot is "+
			"not recomputed from a fresh outer pointer for a mutref outer (T1064 regression):\n%s", body)
	}

	// The mutref outer is loaded (it is passed by pointer to do_push), and the slot
	// is addressed twice (early read + store-back recompute).
	labelRe := regexp.MustCompile(`nestedpush\.ok\.\d+:`)
	if n := len(labelRe.FindAllString(body, -1)); n < 2 {
		t.Fatalf("expected >=2 nestedpush.ok bounds-check blocks for a mutref outer "+
			"(early read + store-back recompute), got %d:\n%s", n, body)
	}
}

// T1064: exercise the remaining outer-place classifications of
// indexTargetIsReallocatablePlace so both branches emit valid IR:
//   - a struct FIELD outer (self.rows[i].push) → MemberExpr branch → recompute slot
//   - an rvalue call outer (fresh()[i].push)   → default branch    → early slot
//
// A field outer is an addressable place (an argument could reach and reallocate
// it), so it takes the recompute path — a second bounds-check block. An rvalue
// outer is NOT a place, so it keeps a single early slot (re-evaluating the call
// would spawn a second temporary). Pins that both forms compile without panicking.
func TestT1064OuterPlaceVariants(t *testing.T) {
	// Field outer: MemberExpr branch (returns true → recompute).
	fieldIR := codegentest.GenerateIR(t, `
		grow_rows(int[][] ~rows) int {
			rows.push(Vector[int](capacity: 1));
			return 7;
		}
		type Holder {
			int[][] rows;
			new(~this) { this.rows = Vector[Vector[int]](capacity: 1); }
			run(~this) {
				int[] inner = Vector[int](capacity: 2);
				inner.push(1);
				this.rows.push(move inner);
				this.rows[0].push(grow_rows(this.rows));
			}
		}
		main() {}
	`)
	// Only Holder.run has an indexed push (grow_rows does a plain, non-indexed
	// push), so nestedpush.ok blocks across the whole IR belong to it: one for the
	// early read, one for the store-back recompute of the field outer.
	if n := len(regexp.MustCompile(`nestedpush\.ok\.\d+:`).FindAllString(fieldIR, -1)); n < 2 {
		t.Fatalf("field outer: expected >=2 nestedpush.ok blocks (early read + "+
			"store-back recompute), got %d:\n%s", n, fieldIR)
	}

	// Rvalue call outer: default branch (returns false → early slot, no recompute).
	// The key requirement is that it compiles — generateIR fails on any codegen panic.
	rvalueIR := codegentest.GenerateIR(t, `
		fresh() int[][] {
			int[][] v = Vector[Vector[int]](capacity: 1);
			int[] inner = Vector[int](capacity: 2);
			inner.push(1);
			v.push(move inner);
			return v;
		}
		go_rvalue() { fresh()[0].push(9); }
		main() {}
	`)
	if !strings.Contains(rvalueIR, "@promise_vector_push(") {
		t.Fatalf("rvalue-outer push did not emit a vector push:\n%s", rvalueIR)
	}
}
