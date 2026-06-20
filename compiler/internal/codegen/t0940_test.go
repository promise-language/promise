package codegen

import (
	"regexp"
	"strings"
	"testing"
)

// T0940 (subsumes T0981): a BOUND elvis `m := a ?: b` whose result is a Map/Set or a
// droppable heap user type (the 2-word {i8*,i8*} value-struct representation) — or a
// single-owner native handle — must give `m` a PER-PATH drop flag rather than the
// unconditional owning drop emitted by maybeRegisterDrop. `m` owns the buffer only on
// a path whose selected operand was orphaned (some-path inner) or neutralized
// (none-path owned-local/fresh-temp default). When the selected operand keeps a real
// owner (borrowed param / container member / static) `m` must BORROW it — otherwise
// `m`'s scope-exit drop and the operand's own owner free the same buffer twice
// (Map/heap-user: SEGV / invalid free).
//
// genElvis computes the flag as a phi[someOwnsInner, noneOwned] in the elvis.merge
// block; the var-decl binding in stmt.go overwrites maybeRegisterDrop's
// `store i1 true, i1* %m.dropflag` with `store i1 %phi, i1* %m.dropflag`. These tests
// lock that IR signature for every droppable result representation.

// elvisBoundOwnsNone matches the bound per-path flag phi when BOTH operands are
// transferable (owned-local source + owned-local/fresh-temp default): m owns on both
// paths (true, true).
const elvisBoundOwnsNone = `phi i1 \[ true, %elvis\.some\.\d+ \], \[ true, %elvis\.none\.\d+ \]`

// elvisBoundBorrowsBoth matches the bound per-path flag phi when BOTH operands are
// borrowed params: m borrows on both paths (false, false) — the core double-free fix.
const elvisBoundBorrowsBoth = `phi i1 \[ false, %elvis\.some\.\d+ \], \[ false, %elvis\.none\.\d+ \]`

// elvisBoundFlagStore matches the var-decl override: maybeRegisterDrop's unconditional
// `store i1 true` immediately followed by the per-path register store into the same
// `%m.dropflag`. Its presence proves the bound drop is path-dependent, not constant.
const elvisBoundFlagStore = `store i1 true, i1\* %m\.dropflag\s*\n\s*store i1 %\d+, i1\* %m\.dropflag`

// TestT0940MapBoundBorrowedParamDefaultBorrowsNone is the core fix: `m := a ?: b`
// where both Map operands are borrowed params. Pre-fix `m.dropflag` was an
// unconditional `store i1 true`, so `m`'s scope-exit @"Map[...].drop" freed the
// caller-owned default a second time → SEGV. The fix threads a phi[false,false] into
// `%m.dropflag` so `m` borrows on both paths.
func TestT0940MapBoundBorrowedParamDefaultBorrowsNone(t *testing.T) {
	ir := generateIR(t, `
		pick(map[string, int]? a, map[string, int] b) {
			m := a ?: b;
		}
		demo() {
			map[string, int] x = map[string, int]();
			map[string, int]? a = none;
			pick(a, x);
		}
	`)
	fn := extractFunction(ir, "__user.pick")
	if fn == "" {
		t.Fatal("could not extract __user.pick")
	}
	// The bound flag borrows on both paths (both operands caller-owned).
	assertContainsMatch(t, fn, elvisBoundBorrowsBoth)
	// And it is the value stored into %m.dropflag (path-dependent, not constant true).
	assertContainsMatch(t, fn, elvisBoundFlagStore)
}

// TestT0940HeapUserBoundBorrowedParamDefaultBorrowsNone is the heap-user-type arm of
// the same fix.
func TestT0940HeapUserBoundBorrowedParamDefaultBorrowsNone(t *testing.T) {
	ir := generateIR(t, `
		type HeapBox { int[] data; }
		pick(HeapBox? a, HeapBox b) {
			m := a ?: b;
		}
		demo() {
			HeapBox x = HeapBox(data: []);
			HeapBox? a = none;
			pick(a, x);
		}
	`)
	fn := extractFunction(ir, "__user.pick")
	if fn == "" {
		t.Fatal("could not extract __user.pick")
	}
	assertContainsMatch(t, fn, elvisBoundBorrowsBoth)
	assertContainsMatch(t, fn, elvisBoundFlagStore)
	// A heap-user result must be dropped via its own drop fn, never the string helper.
	if strings.Contains(fn, "@promise_string_drop") {
		t.Errorf("heap-user bound elvis must NOT call @promise_string_drop (T0940)\n%s", fn)
	}
}

// TestT0940MapBoundOwnedLocalDefaultOwnsNone: `m := a ?: b` with an owned-local Map
// default neutralizes `b`'s scope-exit drop flag in the elvis.none block and gives
// `m` ownership on the none-path (none incoming `true`). Exactly one owner frees the
// buffer on each path.
func TestT0940MapBoundOwnedLocalDefaultOwnsNone(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			map[string, int]? a = none;
			map[string, int] b = map[string, int]();
			m := a ?: b;
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	// none-path neutralizes the owned-local default's own drop flag.
	assertContainsMatch(t, fn, `elvis\.none\.\d+:[\s\S]*store i1 false, i1\* %b\.dropflag`)
	// The bound flag owns on both paths and is the value stored into %m.dropflag.
	assertContainsMatch(t, fn, elvisBoundOwnsNone)
	assertContainsMatch(t, fn, elvisBoundFlagStore)
	assertContains(t, fn, `call void @"Map[string, int].drop"`)
}

// TestT0940HeapUserBoundOwnedLocalDefaultNeutralized: heap-user arm — the owned-local
// default's drop flag is cleared in the elvis.none block, and the bound flag governs
// `m`'s single drop.
func TestT0940HeapUserBoundOwnedLocalDefaultNeutralized(t *testing.T) {
	ir := generateIR(t, `
		type HeapBox { int[] data; }
		demo() {
			HeapBox? a = none;
			HeapBox b = HeapBox(data: []);
			m := a ?: b;
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	assertContainsMatch(t, fn, `elvis\.none\.\d+:[\s\S]*store i1 false, i1\* %b\.dropflag`)
	assertContainsMatch(t, fn, elvisBoundOwnsNone)
	assertContainsMatch(t, fn, elvisBoundFlagStore)
	assertContains(t, fn, "call void @HeapBox.drop")
}

// TestT0940TypedDeclBoundBorrowedParamDefaultBorrowsNone locks the genVarDecl (typed)
// RHS-eval site: `map[string,int] m = a ?: b` borrows the borrowed-param default on
// the none-path identically to the inferred `:=` form.
func TestT0940TypedDeclBoundBorrowedParamDefaultBorrowsNone(t *testing.T) {
	ir := generateIR(t, `
		pick(map[string, int]? a, map[string, int] b) {
			map[string, int] m = a ?: b;
		}
		demo() {
			map[string, int] x = map[string, int]();
			map[string, int]? a = none;
			pick(a, x);
		}
	`)
	fn := extractFunction(ir, "__user.pick")
	if fn == "" {
		t.Fatal("could not extract __user.pick")
	}
	assertContainsMatch(t, fn, elvisBoundBorrowsBoth)
	assertContainsMatch(t, fn, elvisBoundFlagStore)
}

// TestT0940BoundMemberAliasDefaultBorrowsNone: `m := a ?: bx.field` where the default
// is a struct field read (container/struct-owned). The member source is not an
// orphaned local — neutralizeElvisNoneDefault cannot reach a scope drop flag, so the
// bound flag's none incoming is `false` and `m` borrows the field's buffer (the
// holder's own drop frees it once).
func TestT0940BoundMemberAliasDefaultBorrowsNone(t *testing.T) {
	ir := generateIR(t, `
		type HeapBox { int[] data; }
		type Holder { HeapBox h; }
		demo() {
			HeapBox? a = none;
			Holder bx = Holder(h: HeapBox(data: []));
			m := a ?: bx.h;
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	// some-path source is `a` (owned local none) → owns; none-path default is a member
	// read → borrows. Flag phi = [true, false].
	assertContainsMatch(t, fn, `phi i1 \[ true, %elvis\.some\.\d+ \], \[ false, %elvis\.none\.\d+ \]`)
	assertContainsMatch(t, fn, elvisBoundFlagStore)
}

// TestT0940GenericContextMapBoundResolvesDropFunc locks the typeSubst arm of
// elvisResultHeapDrop: inside a generic function the elvis result type is
// `Map[string, T]` (with TypeParam T). The monomorphized `wrap[int]` body must resolve
// the concrete @"Map[string, int].drop" and thread the per-path flag into %m.dropflag —
// proving the heap-droppable classification substituted the type param.
func TestT0940GenericContextMapBoundResolvesDropFunc(t *testing.T) {
	ir := generateIR(t, `
		wrap[T](T move seed) {
			map[string, T]? a = none;
			map[string, T] b = map[string, T](); b["k"] = seed;
			m := a ?: b;
		}
		demo() { wrap(7); }
	`)
	fn := extractDefine(ir, `"wrap[int]"`)
	if fn == "" {
		t.Fatal("could not extract monomorphized wrap[int]")
	}
	assertContainsMatch(t, fn, elvisBoundOwnsNone)
	assertContainsMatch(t, fn, elvisBoundFlagStore)
	assertContains(t, fn, `call void @"Map[string, int].drop"`)
}

// TestT0940InlineMapBorrowsOnNoneUnchanged locks "inline IR unchanged": the inline
// form `(a ?: b).len` (discarded, not bound) for a Map default must NOT neutralize the
// none-path default's drop flag and must NOT emit a bound `%m.dropflag` override —
// inline Map/Set keep borrow-on-none (T0937), the fix is gated to the bound case.
func TestT0940InlineMapBorrowsOnNoneUnchanged(t *testing.T) {
	ir := generateIR(t, `
		sink(int n) { }
		demo() {
			map[string, int]? a = none;
			map[string, int] b = map[string, int]();
			sink((a ?: b).len);
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	// Inline (non-bound): the none-path must NOT clear the default `b`'s drop flag.
	if strings.Contains(noneBlockOf(t, fn), "store i1 false, i1* %b.dropflag") {
		t.Errorf("inline (non-bound) Map elvis must NOT neutralize the none-path default — borrow-on-none preserved (T0937/T0940)\n%s", fn)
	}
	// No bound %m.dropflag override is emitted (no bound variable).
	if strings.Contains(fn, "%m.dropflag") {
		t.Errorf("inline (non-bound) Map elvis must NOT emit a bound %%m.dropflag override (T0940)\n%s", fn)
	}
}

// TestT0940BoundVecMemberSourceDupClonePromotesSome locks the boundSourceDupCloned
// promotion (genElvis: `if boundSourceDupCloned { someOwnsInner = true }`). When the
// bound var-decl set a dup-on-read flag (here dupContainerFieldAccess, set by
// setDupFlagsForFieldAccess for the `int[]` LHS) AND the some-path source is a
// member/index read, genFieldAccess CLONES the field's inner into a fresh buffer the
// binding owns — so the some-path is OWNED, not borrowed. Pre-promotion the some flag
// was `false` (borrow), so the freshly cloned buffer had no owner → leak. The fix
// flips the bound flag's some incoming to `true` for the cloned member source.
func TestT0940BoundVecMemberSourceDupClonePromotesSome(t *testing.T) {
	ir := generateIR(t, `
		type VBox { int[]? v; }
		demo() {
			VBox bx = VBox(v: none);
			int[] inner = []; inner.push(1); inner.push(2);
			bx.v = inner;
			int[] x = (bx.v ?: [0]);
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	// some-path OWNS the clone (promotion → true); none-path borrows the static `[0]`
	// literal default (no heap temp to claim → false). Pre-fix the some incoming was
	// `false`, leaking the clone.
	assertContainsMatch(t, fn, `phi i1 \[ true, %elvis\.some\.\d+ \], \[ false, %elvis\.none\.\d+ \]`)
	// And the per-path flag is the value stored into the bound var's own drop flag
	// (path-dependent override, not a constant true).
	assertContainsMatch(t, fn, `store i1 true, i1\* %x\.dropflag\s*\n\s*store i1 %\d+, i1\* %x\.dropflag`)
}

// TestT0940AssignFormDoesNotBleedBoundFlag locks the genAssignStmt defensive clear
// (`c.elvisBoundDropFlag = nil`). The assignment form `m = a ?: b` sets
// elvisResultBound, so genElvis still computes a per-path bound flag — but the
// assignment path (owned by T1013/T1014) does NOT consume it. Without the clear, that
// stale phi would bleed into the NEXT droppable var-decl's drop flag via
// consumeElvisBoundDropFlag, overwriting `n`'s legitimate unconditional owning drop →
// leak (phi false) or UAF (phi true while n borrows). The var-decl `n` must receive
// exactly one store into its drop flag (the unconditional `store i1 true`), never a
// `store i1 %<phi>` override.
func TestT0940AssignFormDoesNotBleedBoundFlag(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			map[string, int] m = map[string, int]();
			map[string, int]? a = none;
			map[string, int] b = map[string, int]();
			m = a ?: b;
			map[string, int] n = map[string, int]();
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	stores := regexp.MustCompile(`store i1 [^,]+, i1\* %n\.dropflag`).FindAllString(fn, -1)
	if len(stores) != 1 {
		t.Errorf("var-decl after assignment-form elvis must get exactly ONE store into %%n.dropflag (the unconditional owning drop); got %d: %v\n%s", len(stores), stores, fn)
	}
	// The single store must be the unconditional `true`, not a bled-in per-path phi.
	if len(stores) == 1 && !strings.Contains(stores[0], "store i1 true,") {
		t.Errorf("the bound elvis per-path flag bled into %%n.dropflag (defensive clear missing): %q\n%s", stores[0], fn)
	}
}
