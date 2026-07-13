package codegen

import (
	"strings"
	"testing"
)

// T1269: A borrowed heap param (bare `T[]`, passed by value; T0998) has no drop
// flag, so the return-alias safety net skipped it — an owned result aliasing it
// that escaped into owned storage double-freed. The fix records the borrow arg's
// pointer and, after the arg loop, clones the RESULT under a runtime alias guard
// (`retPtr == argPtr`) so the caller's borrow stays sole owner of its buffer.
//
// The guarded-clone diamond (alias.borrow.clone) is emitted at the call site where
// a borrowed param is forwarded to a function with a droppable return that could
// alias it — here `ident(xs)` inside `mk`, whose vector result is moved into a
// struct field.
func TestT1269BorrowParamAliasClonesResult(t *testing.T) {
	ir := generateIR(t, `
		type C { Money[] xs; }
		type Money { int cents; }
		ident(Money[] v) Money[] { return v; }
		mk(Money[] xs) C {
			Money[] s = ident(xs);
			return C(xs: move s);
		}
		main() {
			Money[] v = [Money(cents: 1)];
			C c = mk(v);
		}
	`)
	// The borrow-param clone path: a runtime alias guard branching into a deep
	// clone of the vector result.
	assertContains(t, ir, "alias.borrow.clone")
	assertContains(t, ir, "alias.borrow.cont")
	assertContains(t, ir, "vecdup.copy")
}

// T1269 guard: passing an OWNED LOCAL (drop flag present) to the same function
// takes the T1031 owned-local path (alias.dup — clone into the source's storage),
// NOT the borrow path. This confirms the borrow arm fires only for drop-flag-less
// (borrowed) sources. The stdlib itself emits some baseline alias.borrow blocks,
// so a bare NotContains is unusable — instead compare the borrow-clone block count
// against a no-forwarding baseline (identical stdlib, no user ident call): the
// owned-local program must add zero user-level borrow clones.
func TestT1269OwnedLocalSourceUsesDupNotBorrow(t *testing.T) {
	ownedLocal := generateIR(t, `
		type Money { int cents; }
		ident(Money[] v) Money[] { return v; }
		main() {
			Money[] xs = [Money(cents: 1)];
			Money[] s = ident(xs);
			print_line(xs[0].cents.to_string());
			print_line(s[0].cents.to_string());
		}
	`)
	// Same stdlib + same `ident` decl, but never forwarding a borrow to it.
	baseline := generateIR(t, `
		type Money { int cents; }
		ident(Money[] v) Money[] { return v; }
		main() {
			Money[] xs = [Money(cents: 1)];
			print_line(xs[0].cents.to_string());
		}
	`)
	// Owned-local source takes the T1031 clone-into-source path.
	assertContains(t, ownedLocal, "alias.dup")
	assertContains(t, ownedLocal, "vecdup.copy")
	// And adds no user-level borrow-clone blocks over the baseline.
	if got, want := strings.Count(ownedLocal, "alias.borrow.clone."), strings.Count(baseline, "alias.borrow.clone."); got != want {
		t.Fatalf("owned-local source added user borrow-clone blocks: got %d occurrences, baseline %d (want equal)", got, want)
	}
}

// T1269: a PLAIN heap-user-type return (a bare `Node` value struct, not a
// container) aliasing a borrowed param and moved into owned storage double-frees
// the shared instance without the fix (verified SEGV). The borrow-clone here
// routes through dupOwnedReturnValue's maybeDupPushElement→dupHeapValue arm — a
// different clone path than the vector/string/map cases — so the guarded clone
// must still fire (`alias.borrow.clone`) and deep-copy the instance (`heapdup`).
func TestT1269HeapUserTypeBorrowAliasClonesResult(t *testing.T) {
	ir := generateIR(t, `
		type Money { int cents; }
		type Node { Money[] data; }
		type Holder { Node n; }
		ident_node(Node n) Node { return n; }
		mk_holder(Node n) Holder {
			Node s = ident_node(n);
			return Holder(n: move s);
		}
		main() {
			Node o = Node(data: [Money(cents: 1)]);
			Holder h = mk_holder(o);
		}
	`)
	assertContains(t, ir, "alias.borrow.clone")
	assertContains(t, ir, "alias.borrow.cont")
	// dupHeapValue emits a heapdup.* diamond for the instance deep-copy.
	assertContains(t, ir, "heapdup")
}

// T1269: when a call forwards TWO borrowed params and the result may alias
// EITHER, the per-arg `retPtr == argPtr` checks are combined with an `or i1`
// into a single guard driving the clone branch (a one-borrow-arg call needs no
// OR). This locks the multi-arg disjunction: the branch to alias.borrow.clone
// must be guarded by an `or`-produced register, not a bare icmp.
func TestT1269TwoBorrowParamsOrAliasChecks(t *testing.T) {
	ir := generateIR(t, `
		type Money { int cents; }
		type Bag { Money[] items; }
		pick(Money[] a, Money[] b, bool w) Money[] {
			if w { return a; }
			return b;
		}
		mk(Money[] x, Money[] y, bool w) Bag {
			Money[] s = pick(x, y, w);
			return Bag(items: move s);
		}
		main() {
			Money[] p = [Money(cents: 1)];
			Money[] q = [Money(cents: 2)];
			Bag b = mk(p, q, true);
		}
	`)
	assertContains(t, ir, "alias.borrow.clone")
	// Find a `br i1 %R, label %alias.borrow.clone...` and confirm %R is defined
	// by an `%R = or i1 ...` — proving the two borrow-arg checks were OR'd.
	if !branchGuardedByOr(ir, "alias.borrow.clone") {
		t.Fatalf("expected the alias.borrow.clone branch to be guarded by an or-combined check (two borrow args); IR:\n%s", ir)
	}
}

// branchGuardedByOr reports whether some `br i1 %R, label %<clonePrefix>...`
// in the IR has its guard register %R defined by an `%R = or i1 ...` instruction.
func branchGuardedByOr(ir, clonePrefix string) bool {
	lines := strings.Split(ir, "\n")
	orDefs := map[string]bool{}
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		// Match `%N = or i1 %A, %B`
		if eq := strings.Index(ln, " = or i1 "); eq != -1 && strings.HasPrefix(ln, "%") {
			orDefs[ln[:eq]] = true
		}
	}
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "br i1 ") || !strings.Contains(ln, "%"+clonePrefix) {
			continue
		}
		// Extract the guard register: `br i1 %R, label ...`
		rest := strings.TrimPrefix(ln, "br i1 ")
		if comma := strings.Index(rest, ","); comma != -1 {
			guard := strings.TrimSpace(rest[:comma])
			if orDefs[guard] {
				return true
			}
		}
	}
	return false
}
