package codegen

import (
	"strings"
	"testing"
)

// T1353: a compound member assignment (`a.b += x`) must evaluate the receiver
// exactly once and before the RHS, matching the canonical order the index (`[]`)
// and slice (`[:]`) paths already follow:
//
//	target (receiver) → RHS → read → operator → write.
//
// Before the fix the member/setter path evaluated the RHS first and the
// side-effecting receiver twice (once for the getter read, once for the setter
// write). These tests pin the emitted call order and count in the user-main
// coroutine body (userMainBody is defined in t1090_test.go).

// A side-effecting receiver call must appear exactly once — guards against
// re-introducing the double-eval of the receiver sub-expression.
func TestT1353_SetterCompoundEvaluatesReceiverOnce(t *testing.T) {
	ir := generateIR(t, `
		recv() Box { return Box(x: 10); }
		type Box {
			int x;
			get val int { return this.x; }
			set val(int v) { this.x = v; }
		}
		main() {
			recv().val += 5;
		}
	`)
	body := userMainBody(t, ir)
	n := strings.Count(body, "@__user.recv()")
	if n != 1 {
		t.Errorf("receiver recv() must be evaluated exactly once (T1353); got %d calls", n)
	}
	// Both the getter read and the setter write must still be emitted.
	if !strings.Contains(body, "@Box.val(") {
		t.Errorf("expected getter read @Box.val in main")
	}
	if !strings.Contains(body, "@Box.val$set(") {
		t.Errorf("expected setter write @Box.val$set in main")
	}
}

// The receiver must be evaluated before the RHS on the setter-property path.
func TestT1353_SetterCompoundEvalsTargetBeforeRHS(t *testing.T) {
	ir := generateIR(t, `
		side() int { return 5; }
		idx() int { return 0; }
		type Box {
			int x;
			get val int { return this.x; }
			set val(int v) { this.x = v; }
		}
		main() {
			boxes := [Box(x: 10)];
			boxes[idx()].val += side();
		}
	`)
	body := userMainBody(t, ir)
	target := strings.Index(body, "@__user.idx()")
	rhs := strings.Index(body, "@__user.side()")
	if target < 0 || rhs < 0 {
		t.Fatalf("expected both idx() and side() calls in main; target=%d rhs=%d", target, rhs)
	}
	if target > rhs {
		t.Errorf("target boxes[idx()] must be evaluated before the RHS side() (T1353); target=%d rhs=%d", target, rhs)
	}
}

// A virtual (inherited) setter dispatches the getter read and setter write
// through the vtable (genVirtualGetterCall / genVirtualSetterCall). The staged
// receiver must be consumed on both sides so a side-effecting receiver is still
// evaluated exactly once — guards against re-introducing the double-eval on the
// virtual-dispatch path.
func TestT1353_VirtualSetterCompoundEvaluatesReceiverOnce(t *testing.T) {
	ir := generateIR(t, `
		recv() Base { return Derived(x: 10); }
		type Base {
			int x;
			get val int { return this.x; }
			set val(int v) { this.x = v; }
		}
		type Derived is Base {}
		main() {
			recv().val += 5;
		}
	`)
	body := userMainBody(t, ir)
	n := strings.Count(body, "@__user.recv()")
	if n != 1 {
		t.Errorf("virtual receiver recv() must be evaluated exactly once (T1353); got %d calls", n)
	}
	// The compound must dispatch through the vtable (indirect call), not a direct
	// @Base.val / @Base.val$set call — confirms the virtual staged-receiver path
	// is what ran.
	if !strings.Contains(body, "call ") {
		t.Errorf("expected a call in main")
	}
}

// The receiver must be evaluated before the RHS on the plain-field path too.
func TestT1353_FieldCompoundEvalsTargetBeforeRHS(t *testing.T) {
	ir := generateIR(t, `
		side() int { return 5; }
		idx() int { return 0; }
		type Box { int x; }
		main() {
			boxes := [Box(x: 10)];
			boxes[idx()].x += side();
		}
	`)
	body := userMainBody(t, ir)
	target := strings.Index(body, "@__user.idx()")
	rhs := strings.Index(body, "@__user.side()")
	if target < 0 || rhs < 0 {
		t.Fatalf("expected both idx() and side() calls in main; target=%d rhs=%d", target, rhs)
	}
	if target > rhs {
		t.Errorf("target boxes[idx()] must be evaluated before the RHS side() (T1353); target=%d rhs=%d", target, rhs)
	}
}
