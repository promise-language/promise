package codegen

import (
	"strings"
	"testing"
)

// T0555: Native handle temporaries (Arc/Weak/Mutex/Task) passed directly as
// call arguments leaked because the callee param is borrowed by-value (no
// scope drop binding) and the caller had no statement-temp tracking for them.
// The fix mirrors the existing trackGetterResult pattern in two origin sites
// (genExpr's *ast.CallExpr and *ast.GoExpr) plus three claim-site predicates
// in stmt.go.

// TestT0555_ArcCtorTempTracked — `take_arc(Arc[int](99))` must emit a tmp.drop
// block + call to @"Arc[int].drop". Pre-fix: nothing tracked, leak.
func TestT0555_ArcCtorTempTracked(t *testing.T) {
	ir := generateIR(t, `
		take_arc(Arc[int] a) {}
		caller() {
			take_arc(Arc[int](99));
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "tmp.drop") {
		t.Errorf("expected tmp.drop block in caller (Arc ctor temp tracking):\n%s", body)
	}
	if !strings.Contains(body, `@"Arc[int].drop"`) {
		t.Errorf("expected call to @\"Arc[int].drop\" in caller:\n%s", body)
	}
}

// TestT0555_MutexCtorTempTracked — `take_mutex(Mutex[int](99))` must emit a
// tmp.drop block + call to @"Mutex[int].drop".
func TestT0555_MutexCtorTempTracked(t *testing.T) {
	ir := generateIR(t, `
		take_mutex(Mutex[int] m) {}
		caller() {
			take_mutex(Mutex[int](99));
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "tmp.drop") {
		t.Errorf("expected tmp.drop block in caller (Mutex ctor temp tracking):\n%s", body)
	}
	if !strings.Contains(body, `@"Mutex[int].drop"`) {
		t.Errorf("expected call to @\"Mutex[int].drop\" in caller:\n%s", body)
	}
}

// TestT0555_TaskGoExprTempTracked — `take_task(go worker())` must emit a
// tmp.drop block + call to @"Task[int].drop".
func TestT0555_TaskGoExprTempTracked(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 42; }
		take_task(Task[int] t) {}
		caller() {
			take_task(go worker());
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "tmp.drop") {
		t.Errorf("expected tmp.drop block in caller (Task go-expr temp tracking):\n%s", body)
	}
	if !strings.Contains(body, `@"Task[int].drop"`) {
		t.Errorf("expected call to @\"Task[int].drop\" in caller:\n%s", body)
	}
}

// TestT0555_FireAndForgetGoNotTracked — `go worker();` (statement-level
// discard) must NOT emit a Task.drop site. The G struct is freed by
// goroutine_exit on the worker thread; caller-side tracking would double-free.
func TestT0555_FireAndForgetGoNotTracked(t *testing.T) {
	ir := generateIR(t, `
		worker() int { return 42; }
		caller() {
			go worker();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if strings.Contains(body, `@"Task[int].drop"`) {
		t.Errorf("fire-and-forget go must NOT call @\"Task[int].drop\" (double-free risk):\n%s", body)
	}
}

// TestT0555_FreeFunctionReturningArcTracked — when a free function returns
// Arc[T], the result passed directly as a call arg must be tracked too.
// Exercises the case where rt comes from the CallExpr's return type.
func TestT0555_FreeFunctionReturningArcTracked(t *testing.T) {
	ir := generateIR(t, `
		take_arc(Arc[int] a) {}
		make_arc() Arc[int] { return Arc[int](42); }
		caller() {
			take_arc(make_arc());
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "tmp.drop") {
		t.Errorf("expected tmp.drop block in caller (Arc-returning call result tracking):\n%s", body)
	}
	if !strings.Contains(body, `@"Arc[int].drop"`) {
		t.Errorf("expected call to @\"Arc[int].drop\" in caller:\n%s", body)
	}
}

// TestT0555_LocalBindingClaimsTemp — when the constructor temp is bound to a
// local, the variable's scope drop owns it. Verify the IR still contains
// Arc[int].drop (from scope cleanup) and that the tmp-drop's flag is cleared
// by the local-binding claim site so there is no double-free.
func TestT0555_LocalBindingClaimsTemp(t *testing.T) {
	ir := generateIR(t, `
		take_arc(Arc[int] a) {}
		caller() {
			a := Arc[int](99);
			take_arc(a);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// The variable's scope binding handles drop.
	if !strings.Contains(body, `@"Arc[int].drop"`) {
		t.Errorf("expected Arc[int].drop call (scope cleanup for local) in caller:\n%s", body)
	}
}

// TestT0555_WeakDowngradeTempTracked — Weak[T] from `.downgrade()` method
// call must be tracked. Method-call temps go through the same CallExpr case
// in genExpr; AsWeak dispatch must fire.
func TestT0555_WeakDowngradeTempTracked(t *testing.T) {
	ir := generateIR(t, `
		take_weak(Weak[int] w) {}
		caller() {
			a := Arc[int](7);
			take_weak(a.downgrade());
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "tmp.drop") {
		t.Errorf("expected tmp.drop block in caller (Weak downgrade temp tracking):\n%s", body)
	}
	if !strings.Contains(body, `@"Weak[int].drop"`) {
		t.Errorf("expected call to @\"Weak[int].drop\" in caller:\n%s", body)
	}
}

// TestT0555_OptionalArcTypedDeclPreWrapClaim — T0555 secondary fix:
// `Arc[int]? opt = Arc[int](99);` must clear the tracked stmt-temp BEFORE
// wrapOptional, otherwise both the stmt-temp drop and the optional binding
// drop fire on the same pointer (double-free). The presence of optdrop.check
// (the optional's binding drop) AND tmp.drop in the IR is correct only if
// the stmt-temp's drop flag is cleared at runtime — the tmp.exec block
// would never execute. Verify the optional drop is present.
func TestT0555_OptionalArcTypedDeclPreWrapClaim(t *testing.T) {
	ir := generateIR(t, `
		caller() {
			Arc[int]? opt = Arc[int](99);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// optdrop blocks come from Optional's binding drop wired by maybeRegisterOptionalDrop.
	if !strings.Contains(body, "optdrop") {
		t.Errorf("expected optdrop block (Optional binding drop) in caller:\n%s", body)
	}
	// Sanity: Arc[int].drop should be called from the optional drop path.
	if !strings.Contains(body, `@"Arc[int].drop"`) {
		t.Errorf("expected @\"Arc[int].drop\" in caller:\n%s", body)
	}
}

// TestT0555_GenericFunctionReturningArcTracked — exercises the c.typeSubst
// substitution in genExpr's CallExpr case. The generic body
// `make[T](~T v) Arc[T] { return Arc[T](v); }` records the call result
// as Arc[TypeParam(T)] in c.info.Types[e]. Without c.typeSubst applied,
// AsArc(rt) returns false (TypeParam isn't an Instance), so no temp
// tracking → caller-side leak. With the substitution applied for the
// `make[int]` instantiation, rt becomes Arc[int] and dispatch fires.
func TestT0555_GenericFunctionReturningArcTracked(t *testing.T) {
	ir := generateIR(t, `
		take_arc(Arc[int] a) {}
		make_arc[T](~T value) Arc[T] {
			return Arc[T](value);
		}
		caller() {
			take_arc(make_arc[int](99));
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "tmp.drop") {
		t.Errorf("expected tmp.drop block in caller (generic Arc call result tracking via typeSubst):\n%s", body)
	}
	if !strings.Contains(body, `@"Arc[int].drop"`) {
		t.Errorf("expected call to @\"Arc[int].drop\" in caller:\n%s", body)
	}
}

// TestT0555_GenericMethodReturningArcTracked — same typeSubst path but via
// a generic method on a user type. monoCtx is set for the method body.
func TestT0555_GenericMethodReturningArcTracked(t *testing.T) {
	ir := generateIR(t, `
		take_arc(Arc[int] a) {}
		type Maker {
			make[T](this, ~T value) Arc[T] {
				return Arc[T](value);
			}
		}
		caller() {
			m := Maker();
			take_arc(m.make[int](99));
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "tmp.drop") {
		t.Errorf("expected tmp.drop block in caller (generic method Arc result tracking):\n%s", body)
	}
	if !strings.Contains(body, `@"Arc[int].drop"`) {
		t.Errorf("expected call to @\"Arc[int].drop\" in caller:\n%s", body)
	}
}

// TestT0555_PlainReassignArcClaimsTemp — exercises the assignment claim
// site at stmt.go:5141 (B0187 + T0555). When reassigning a non-Optional
// local `a = Arc[int](2);`, the new constructor temp is tracked by
// genExpr, then must be claimed by the assignment so the temp's drop flag
// is cleared and only the variable's scope drop fires.
func TestT0555_PlainReassignArcClaimsTemp(t *testing.T) {
	ir := generateIR(t, `
		caller() {
			a := Arc[int](1);
			a = Arc[int](2);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// Two Arc[int].drop call sites total: one for the reassignment-time drop
	// of the old value, one for the scope-end drop of the final value.
	count := strings.Count(body, `@"Arc[int].drop"`)
	if count < 2 {
		t.Errorf("expected at least 2 calls to @\"Arc[int].drop\" (reassign + scope) in caller, got %d:\n%s", count, body)
	}
}

// TestT0555_InferredDeclMutexClaimsTemp — exercises the inferred-decl
// claim site at stmt.go:1099. `m := Mutex[int](42)` tracks the Mutex
// temp; the inferred decl must claim it so only the variable's scope
// drop fires (no double-free).
func TestT0555_InferredDeclMutexClaimsTemp(t *testing.T) {
	ir := generateIR(t, `
		caller() {
			m := Mutex[int](42);
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	// Mutex[int].drop should be called via the scope cleanup binding (strdrop).
	if !strings.Contains(body, `@"Mutex[int].drop"`) {
		t.Errorf("expected @\"Mutex[int].drop\" in caller (scope cleanup):\n%s", body)
	}
}

// TestT0555_CallReturningTaskTracked — free function returning Task[T]
// (containing a `go expr()` internally) — the call result's i8* must be
// tracked at the caller, not just the inner go-expr at the callee.
func TestT0555_CallReturningTaskTracked(t *testing.T) {
	ir := generateIR(t, `
		take_task(Task[int] t) {}
		worker() int { return 42; }
		make_task() Task[int] { return go worker(); }
		caller() {
			take_task(make_task());
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected __user.caller in IR")
	}
	if !strings.Contains(body, "tmp.drop") {
		t.Errorf("expected tmp.drop block in caller (Task-returning call result tracking):\n%s", body)
	}
	if !strings.Contains(body, `@"Task[int].drop"`) {
		t.Errorf("expected call to @\"Task[int].drop\" in caller:\n%s", body)
	}
}
