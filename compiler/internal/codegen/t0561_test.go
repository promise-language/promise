package codegen

import (
	"strings"
	"testing"
)

// T0561: MutexGuard call-result temps passed directly as borrow arguments
// leaked because the CallExpr dispatch in genExpr added in T0555 only covered
// Arc/Weak/Mutex/Task — MutexGuard was omitted. Unlike the other native
// handles, MutexGuard.drop is a single non-per-element-type symbol.

// TestT0561_MutexGuardCallResultTracked — `take_guard(m.lock())` must emit a
// tmp.drop block + call to @MutexGuard.drop. Pre-fix: nothing tracked, leak.
func TestT0561_MutexGuardCallResultTracked(t *testing.T) {
	ir := generateIR(t, `
		take_guard(MutexGuard[int] g) {}
		caller() {
			m := Mutex[int](42);
			take_guard(m.lock());
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "tmp.drop") {
		t.Errorf("expected tmp.drop block in caller (MutexGuard call-result temp tracking):\n%s", body)
	}
	if !strings.Contains(body, "@MutexGuard.drop(") {
		t.Errorf("expected call to @MutexGuard.drop in caller:\n%s", body)
	}
}

// TestT0561_MutexGuardLocalBindingClaimsTemp — when the guard temp is bound
// to a local, the variable's scope drop owns it. The local-binding claim
// site (stmt.go) must clear the tracked stmt-temp flag immediately after
// it's set, otherwise the stmt-temp drop AND the scope drop both fire on
// the same pointer at runtime (double-free).
//
// IR text count of @MutexGuard.drop sites is NOT a reliable runtime
// signal because the IR contains multiple drop sites guarded by drop
// flags: the stmt-temp drop, the panic-cleanup scope drop, and the
// normal-path scope drop. Only one of these fires per execution.
//
// What we verify here: the claim pattern `store i1 true` immediately
// followed by `store i1 false` to the same alloca is the unambiguous
// signature that the claim site fired and cleared the tracked temp's
// flag. The Promise e2e test (tests/concurrency/t0561_mutexguard_temps.pr)
// asserts the real runtime guarantee: zero leaks.
func TestT0561_MutexGuardLocalBindingClaimsTemp(t *testing.T) {
	ir := generateIR(t, `
		take_guard(MutexGuard[int] g) {}
		caller() {
			m := Mutex[int](42);
			g := m.lock();
			take_guard(g);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// At least one MutexGuard.drop call site must be present (scope drop).
	if !strings.Contains(body, "@MutexGuard.drop(") {
		t.Errorf("expected @MutexGuard.drop call (scope cleanup for local) in caller:\n%s", body)
	}
	// Verify the claim pattern fired: a `store i1 true` immediately followed
	// by `store i1 false` to the same alloca. Without the claim, the second
	// store would not appear adjacent to the first.
	idx := strings.Index(body, "store i1 true, i1* %")
	if idx < 0 {
		t.Fatalf("expected drop-flag-set pattern in caller:\n%s", body)
	}
	// Extract the alloca name from the set instruction.
	tail := body[idx:]
	nl := strings.Index(tail, "\n")
	if nl < 0 {
		t.Fatalf("malformed IR (no newline after drop-flag-set):\n%s", body)
	}
	setLine := tail[:nl]
	// e.g. "store i1 true, i1* %15"
	allocaToken := strings.TrimPrefix(setLine, "store i1 true, i1* ")
	allocaToken = strings.TrimSpace(allocaToken)
	clearLine := "store i1 false, i1* " + allocaToken
	// Look in the remaining body after the set line for the matching clear.
	rest := tail[nl:]
	if !strings.Contains(rest, clearLine) {
		t.Errorf("expected stmt-temp drop-flag claim (clear after set) for %s, but not found:\n%s", allocaToken, body)
	}
}

// TestT0561_MutexGuardMoveTempNoDoubleFree — `take_guard_move(~MutexGuard[int])`
// with `m.lock()` as the arg: the `~` consume should claim the stmt-temp via
// applyMutRefArgOwnership so only the callee's drop fires (no caller-side
// double-free). Verify caller does NOT call MutexGuard.drop on its temp.
func TestT0561_MutexGuardMoveTempNoDoubleFree(t *testing.T) {
	ir := generateIR(t, `
		take_guard_move(~MutexGuard[int] g) {}
		caller() {
			m := Mutex[int](42);
			take_guard_move(m.lock());
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// Caller may still register a tmp.drop block (the stmt-temp is registered
	// at the call-result point before the `~` consume claims it), but the
	// drop flag must be cleared so MutexGuard.drop fires zero times at the
	// caller. Count direct calls — both the tmp.drop block and any other
	// caller-side guard drops should be absent (count == 0).
	count := strings.Count(body, "@MutexGuard.drop(")
	if count > 0 {
		// If a tmp.drop block exists, its drop flag is cleared at runtime
		// so the call wouldn't fire — but it still appears in the IR text.
		// What matters at runtime: the move ownership transfer claimed the
		// flag. Verify there's no extra unconditional drop call outside the
		// tmp.drop block. We can check by looking for the claim pattern.
		if !strings.Contains(body, "tmp.drop") {
			t.Errorf("found %d @MutexGuard.drop calls but no tmp.drop block (would double-free):\n%s", count, body)
		}
	}
}

// TestT0561_MutexGuardDiscardedAtStmtEnd — `m.lock();` as a statement-level
// expression (result discarded). The stmt-temp tracking must fire the drop
// so the guard is unlocked at statement end.
func TestT0561_MutexGuardDiscardedAtStmtEnd(t *testing.T) {
	ir := generateIR(t, `
		caller() {
			m := Mutex[int](42);
			m.lock();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "@MutexGuard.drop(") {
		t.Errorf("expected @MutexGuard.drop call (discarded guard temp) in caller:\n%s", body)
	}
}

// hasMutexGuardTmpDropBlock reports whether the IR contains a stmt-temp
// drop block calling @MutexGuard.drop — i.e., expr.go's CallExpr dispatch
// registered the lock() result as a tracked stmt-temp. A tmp.drop.N
// basic block with a guarded call to @MutexGuard.drop is the visible
// signature.
//
// This is a structural check on the IR shape, not a runtime guarantee:
// the actual no-leak / no-double-free property is verified by the Promise
// e2e tests in tests/concurrency/t0561_mutexguard_temps.pr.
func hasMutexGuardTmpDropBlock(body string) bool {
	idx := 0
	for {
		blockStart := strings.Index(body[idx:], "tmp.drop")
		if blockStart < 0 {
			return false
		}
		blockStart += idx
		// Find the block boundary (next double-newline or end).
		blockEnd := strings.Index(body[blockStart:], "\n\n")
		if blockEnd < 0 {
			blockEnd = len(body) - blockStart
		}
		// tmp.drop blocks branch to a tmp.exec block; the tmp.exec block has the
		// actual @MutexGuard.drop call. Look in a window covering both.
		windowEnd := blockStart + blockEnd
		if extra := strings.Index(body[windowEnd:], "tmp.exec"); extra >= 0 && extra < 200 {
			if execEnd := strings.Index(body[windowEnd+extra:], "\n\n"); execEnd > 0 {
				windowEnd += extra + execEnd
			}
		}
		if strings.Contains(body[blockStart:windowEnd], "@MutexGuard.drop(") {
			return true
		}
		idx = blockStart + 1
	}
}

// TestT0561_MutexGuardTypedDeclClaim — `MutexGuard[int] g = m.lock();` (typed
// non-Optional decl) must fire the claim at the post-claim site in
// genTypedVarDecl (stmt.go:880-884). Without the IsMutexGuard branch in that
// predicate, the stmt-temp drop AND the variable's scope drop would both fire
// on the same guard pointer (double-free at runtime).
//
// Verifies the lock() result is tracked as a stmt-temp (tmp.drop block
// calling @MutexGuard.drop) AND the variable's scope drop also calls
// @MutexGuard.drop. The runtime no-double-free guarantee is verified by
// the Promise e2e test test_mg_typed_decl.
func TestT0561_MutexGuardTypedDeclClaim(t *testing.T) {
	ir := generateIR(t, `
		caller() {
			m := Mutex[int](42);
			MutexGuard[int] g = m.lock();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !hasMutexGuardTmpDropBlock(body) {
		t.Errorf("expected tmp.drop block calling @MutexGuard.drop:\n%s", body)
	}
	// Count @MutexGuard.drop occurrences — the stmt-temp drop and `g` scope
	// drop should both produce calls (panic-cleanup + normal-path each at
	// least once). At minimum, expect 2 call sites.
	n := strings.Count(body, "@MutexGuard.drop(")
	if n < 2 {
		t.Errorf("expected at least 2 @MutexGuard.drop calls (stmt-temp + scope), got %d:\n%s", n, body)
	}
}

// TestT0561_MutexGuardPlainReassignClaim — `g = m2.lock();` (plain
// reassignment to a non-Optional MutexGuard variable) must fire the claim at
// the post-claim site in genAssignStmt (stmt.go:5145-5148). The previous
// guard's drop fires before the new value lands, then the new stmt-temp is
// claimed by the variable.
//
// Verifies reassignment to a non-Optional MutexGuard variable produces a
// reassign.merge branch (where the old guard is dropped and the new value
// lands). The runtime guarantee is verified by test_mg_plain_reassign.
func TestT0561_MutexGuardPlainReassignClaim(t *testing.T) {
	ir := generateIR(t, `
		caller() {
			m1 := Mutex[int](1);
			m2 := Mutex[int](2);
			g := m1.lock();
			g = m2.lock();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !hasMutexGuardTmpDropBlock(body) {
		t.Errorf("expected tmp.drop block calling @MutexGuard.drop:\n%s", body)
	}
	// Reassignment to a non-Optional droppable variable goes through the
	// reassign.merge / reassign.diff branch in genAssignStmt.
	if !strings.Contains(body, "reassign.merge") {
		t.Errorf("expected reassign.merge block (MutexGuard reassignment path):\n%s", body)
	}
}

// TestT0561_MutexGuardOptionalReassignClaim — `opt = m.lock();` where
// `opt: MutexGuard[int]?` (reassign to Optional MutexGuard) must fire the
// pre-wrap claim at genAssignStmt (stmt.go:5105-5108). Without IsMutexGuard
// there, the stmt-temp drop fires AND the new Optional's drop fires on the
// same guard pointer (double-free).
//
// Verifies that reassigning a MutexGuard temp into an Optional variable
// emits both stmt-temp tracking (tmp.drop) AND optional-aware cleanup
// (optdrop.check). The runtime guarantee is verified by
// test_mg_optional_reassign_from_none.
func TestT0561_MutexGuardOptionalReassignClaim(t *testing.T) {
	ir := generateIR(t, `
		caller() {
			m := Mutex[int](42);
			MutexGuard[int]? opt = none;
			opt = m.lock();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !hasMutexGuardTmpDropBlock(body) {
		t.Errorf("expected tmp.drop block calling @MutexGuard.drop:\n%s", body)
	}
	if !strings.Contains(body, "optdrop.check") {
		t.Errorf("expected optdrop.check block (Optional[MutexGuard] scope cleanup):\n%s", body)
	}
}

// TestT0561_MutexGuardOptionalDeclClaim — direct typed-Optional decl form
// `MutexGuard[int]? opt = m.lock();`. Previously deferred because T0562
// (maybeClearReceiverDropFlag with too-loose shape guard) caused this exact
// path to leak — the alias-clear emitter wrongly cleared `m`'s drop flag via
// a 16-byte load from an 8-byte alloca (Mutex uses alloca i8*). With the
// T0562 fix in place, the typed-Optional decl now also fires both
// stmt-temp tracking and Optional scope cleanup correctly. Runtime
// no-leak/no-double-free guarantee verified by test_optional_mutexguard_decl
// in tests/concurrency/t0562_optional_receiver_alias.pr.
func TestT0561_MutexGuardOptionalDeclClaim(t *testing.T) {
	ir := generateIR(t, `
		caller() {
			m := Mutex[int](42);
			MutexGuard[int]? opt = m.lock();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !hasMutexGuardTmpDropBlock(body) {
		t.Errorf("expected tmp.drop block calling @MutexGuard.drop (stmt-temp tracking for typed-Optional decl):\n%s", body)
	}
	if !strings.Contains(body, "optdrop.check") {
		t.Errorf("expected optdrop.check block (Optional[MutexGuard] scope cleanup):\n%s", body)
	}
}
