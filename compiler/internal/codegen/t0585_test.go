package codegen

import (
	"strings"
	"testing"
)

// T0585: Wrapping borrowed Optional into wider Optional via intermediate
// local double-frees when the wrapped local is consumed via nested if-let
// without being moved into the return path. The fix has two parts in
// codegen/stmt.go:
//   (1) genTypedVarDecl propagates the RHS's drop-flag value (or 0 for a
//       borrowed source) into the wrapped LHS's drop flag.
//   (2) genIfUnwrapStmt / genWhileUnwrapStmt propagate the source's drop-flag
//       value into the unwrapped binding's drop flag *only when the source
//       has a flag*. Source with no flag is ambiguous (borrowed param vs
//       owned-via-auto-move-wrap at call site) and we can't distinguish at
//       the callee — see the bug filed alongside T0585 for the residual
//       direct-if-let-on-borrowed-param case.

// TestT0585_WrapBorrowedParamClearsBindingDropFlag — `_Box?? b = a;` where
// `a` is a Borrowed (non-`~`) `_Box?` parameter. The wrap site must emit a
// post-maybeRegisterOptionalDrop store of `i1 false` into `b.dropflag` so
// scope exit (and downstream unwrap consumption) treats `b` as a borrow.
func TestT0585_WrapBorrowedParamClearsBindingDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type _Box { int n; }
		_caller(_Box? a) {
			_Box?? b = a;
		}
		main() {}
	`)
	body := extractFunction(ir, "__user._caller")
	if body == "" {
		t.Fatalf("expected __user._caller in IR")
	}
	// The wrap-site fix emits two consecutive stores into b.dropflag in the
	// entry path: `store i1 true` (from maybeRegisterOptionalDrop's init) then
	// `store i1 false` (T0585's propagation — RHS is a borrowed param with no
	// drop flag, so the propagation stores the constant 0).
	idx := strings.Index(body, "%b.dropflag = alloca i1")
	if idx < 0 {
		t.Fatalf("expected %%b.dropflag alloca in IR:\n%s", body)
	}
	tail := body[idx:]
	if !strings.Contains(tail, "store i1 true, i1* %b.dropflag") {
		t.Errorf("expected initial maybeRegisterOptionalDrop store of true into b.dropflag:\n%s", body)
	}
	if !strings.Contains(tail, "store i1 false, i1* %b.dropflag") {
		t.Errorf("expected T0585 propagation store of false into b.dropflag (borrowed-source wrap):\n%s", body)
	}
}

// TestT0585_WrapOwnedLocalPropagatesDropFlag — `_Box?? b = a;` where `a` is
// an owned local (drop flag = 1). The wrap site must load `a.dropflag` and
// store its value into `b.dropflag` (preserving owned ownership state).
func TestT0585_WrapOwnedLocalPropagatesDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type _Box { int n; }
		_helper() {
			_Box? a = _Box(n: 1);
			_Box?? b = a;
		}
		main() { _helper(); }
	`)
	body := extractFunction(ir, "__user._helper")
	if body == "" {
		t.Fatalf("expected __user._helper in IR")
	}
	// For owned RHS, T0585's propagation does `%load = load i1, i1* %a.dropflag`
	// followed by `store i1 %load, i1* %b.dropflag`. The load is the runtime
	// value of a's flag at this point (which is 1 in the success path); the
	// existing clearDropFlag(a) at line ~851 of stmt.go runs before the store.
	if !strings.Contains(body, "%b.dropflag = alloca i1") {
		t.Fatalf("expected %%b.dropflag alloca in IR:\n%s", body)
	}
	if !strings.Contains(body, "load i1, i1* %a.dropflag") {
		t.Errorf("expected T0585 to load a.dropflag for owned-source wrap propagation:\n%s", body)
	}
}

// TestT0585_IfUnwrapOwnedLocalPropagatesDropFlag — `if x := r` where `r` is
// an owned local. genIfUnwrapStmt must load `r.dropflag` and propagate its
// value into `x.dropflag` (so the existing owned-flow keeps working).
func TestT0585_IfUnwrapOwnedLocalPropagatesDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type _Box { int n; }
		_helper() {
			_Box? r = _Box(n: 1);
			if x := r {
			}
		}
		main() { _helper(); }
	`)
	body := extractFunction(ir, "__user._helper")
	if body == "" {
		t.Fatalf("expected __user._helper in IR")
	}
	thenIdx := strings.Index(body, "ifunwrap.then")
	if thenIdx < 0 {
		t.Fatalf("expected ifunwrap.then block:\n%s", body)
	}
	tail := body[thenIdx:]
	if !strings.Contains(tail, "load i1, i1* %r.dropflag") {
		t.Errorf("expected T0585 load of r.dropflag in then block:\n%s", body)
	}
}

// TestT0585_ConsumeParamHasDropFlag — `~T?` consume param. T0585's secondary
// fix extends maybeRegisterDrop to delegate Optional types to
// maybeRegisterOptionalDrop. Without this, `~T?` consume params had no drop
// flag (Optional types previously fell through maybeRegisterDrop's
// container/named checks), which both leaked when not consumed and broke the
// T0585 wrap propagation for consume-source wraps.
func TestT0585_ConsumeParamHasDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type _Box { int n; }
		_consume(~_Box? a) {}
		main() {}
	`)
	body := extractFunction(ir, "__user._consume")
	if body == "" {
		t.Fatalf("expected __user._consume in IR")
	}
	// The consume param must now have a drop flag alloca and an Optional
	// drop binding registered (call to optional_drop helpers at scope exit).
	if !strings.Contains(body, "%a.dropflag = alloca i1") {
		t.Errorf("expected %%a.dropflag alloca for ~T? consume param (T0585 maybeRegisterDrop Optional handling):\n%s", body)
	}
	if !strings.Contains(body, "store i1 true, i1* %a.dropflag") {
		t.Errorf("expected initial store of true into a.dropflag for consume param:\n%s", body)
	}
}

// TestT0585_NonIdentRhsWrapUnchanged — `_Box?? b = c;` where `c` is itself an
// IdentExpr (owned) exercises the IdentExpr path; non-IdentExpr wraps (e.g.
// a constructor in the RHS) should NOT trigger the T0585 store. The fix
// branch is gated on `*ast.IdentExpr`. This regression test guards against
// accidental over-application.
func TestT0585_NonIdentRhsWrapUnchanged(t *testing.T) {
	// We use a chained wrap where the RHS of the inner decl is a constructor.
	// The inner `_Box? c = _Box(n: 7);` is a non-wrap decl (LHS depth equals
	// RHS depth — _Box? = _Box?). Only the outer `_Box?? b = c;` triggers a
	// wrap, and its RHS is the IdentExpr `c`. Verify the IR contains exactly
	// one b.dropflag-clear pattern (from the wrap site), not two.
	ir := generateIR(t, `
		type _Box { int n; }
		_helper() {
			_Box? c = _Box(n: 7);
			_Box?? b = c;
		}
		main() { _helper(); }
	`)
	body := extractFunction(ir, "__user._helper")
	if body == "" {
		t.Fatalf("expected __user._helper in IR")
	}
	// The T0585 wrap site emits `load i1, i1* %c.dropflag` then `store i1 %X,
	// i1* %b.dropflag` (propagating c's owned flag value). Confirm the load
	// appears (positive case — wrap-from-IdentExpr).
	if !strings.Contains(body, "load i1, i1* %c.dropflag") {
		t.Errorf("expected T0585 load of c.dropflag for wrap propagation:\n%s", body)
	}
}

// TestT0585_WhileUnwrapOwnedLocalPropagatesDropFlag — `while x := r` where `r`
// is an owned local. genWhileUnwrapStmt must load `r.dropflag` and propagate
// its value into `x.dropflag` (mirror of the if-let case). Without coverage
// of Site 4, this fix path was completely untested.
func TestT0585_WhileUnwrapOwnedLocalPropagatesDropFlag(t *testing.T) {
	ir := generateIR(t, `
		type _Box { int n; }
		_helper() {
			_Box? r = _Box(n: 1);
			while x := r {
				break;
			}
		}
		main() { _helper(); }
	`)
	body := extractFunction(ir, "__user._helper")
	if body == "" {
		t.Fatalf("expected __user._helper in IR")
	}
	// genWhileUnwrapStmt loads r.dropflag in the while body block before
	// maybeRegisterDrop / clearDropFlag fire on the binding x.
	bodyIdx := strings.Index(body, "whileunwrap.body")
	if bodyIdx < 0 {
		t.Fatalf("expected whileunwrap.body block:\n%s", body)
	}
	tail := body[bodyIdx:]
	if !strings.Contains(tail, "load i1, i1* %r.dropflag") {
		t.Errorf("expected T0585 load of r.dropflag in whileunwrap.body block:\n%s", body)
	}
}

// TestT0585_IfUnwrapBorrowedParamNoPropagateStore — `if x := a` where `a` is a
// borrowed (non-`~`) Optional param with no drop flag. The conservative path
// must NOT emit a propagation store of 0 into x.dropflag (which would mask the
// owned-via-auto-move-wrap call site). The binding's flag stays at its
// maybeRegisterOptionalDrop init value (1).
//
// This guards the "no-flag-source is ambiguous" decision documented in T0585's
// summary — without this test, a future refactor could regress to aggressively
// storing 0 in if-let on borrowed sources, causing leaks in the auto-move flow.
func TestT0585_IfUnwrapBorrowedParamNoPropagateStore(t *testing.T) {
	ir := generateIR(t, `
		type _Box { int n; }
		_caller(_Box? a) {
			if x := a {
			}
		}
		main() {}
	`)
	body := extractFunction(ir, "__user._caller")
	if body == "" {
		t.Fatalf("expected __user._caller in IR")
	}
	thenIdx := strings.Index(body, "ifunwrap.then")
	if thenIdx < 0 {
		t.Fatalf("expected ifunwrap.then block:\n%s", body)
	}
	// Find end of the then block (next label or function close).
	tail := body[thenIdx:]
	// The borrowed param has no a.dropflag, so the propagation `if srcFlagVal != nil`
	// branch is NOT taken — there should be no `load i1, i1* %a.dropflag`
	// in the then block. (If a future bug always loads from c.dropFlags lookup,
	// this would catch it.)
	if strings.Contains(tail, "load i1, i1* %a.dropflag") {
		t.Errorf("borrowed param has no drop flag — expected no load of %%a.dropflag in ifunwrap.then:\n%s", body)
	}
	// And there should be exactly one store of true to x.dropflag (the init
	// from maybeRegisterOptionalDrop), not a subsequent store of false.
	storesTrue := strings.Count(tail, "store i1 true, i1* %x.dropflag")
	storesFalse := strings.Count(tail, "store i1 false, i1* %x.dropflag")
	if storesTrue != 1 {
		t.Errorf("expected exactly one `store i1 true, i1* %%x.dropflag` (maybeRegister init), got %d:\n%s", storesTrue, body)
	}
	if storesFalse != 0 {
		t.Errorf("expected no `store i1 false, i1* %%x.dropflag` for borrowed-source if-let (conservative path), got %d:\n%s", storesFalse, body)
	}
}

// TestT0585_WhileUnwrapBorrowedParamNoPropagateStore — symmetric to the if-let
// borrowed-source test, for while-let. Site 4 of the T0585 fix uses the same
// conservative resolution (only propagate when source has a flag).
func TestT0585_WhileUnwrapBorrowedParamNoPropagateStore(t *testing.T) {
	ir := generateIR(t, `
		type _Box { int n; }
		_caller(_Box? a) {
			while x := a {
				break;
			}
		}
		main() {}
	`)
	body := extractFunction(ir, "__user._caller")
	if body == "" {
		t.Fatalf("expected __user._caller in IR")
	}
	bodyIdx := strings.Index(body, "whileunwrap.body")
	if bodyIdx < 0 {
		t.Fatalf("expected whileunwrap.body block:\n%s", body)
	}
	tail := body[bodyIdx:]
	if strings.Contains(tail, "load i1, i1* %a.dropflag") {
		t.Errorf("borrowed param has no drop flag — expected no load of %%a.dropflag in whileunwrap.body:\n%s", body)
	}
	storesFalse := strings.Count(tail, "store i1 false, i1* %x.dropflag")
	if storesFalse != 0 {
		t.Errorf("expected no `store i1 false, i1* %%x.dropflag` for borrowed-source while-let (conservative path), got %d:\n%s", storesFalse, body)
	}
}
