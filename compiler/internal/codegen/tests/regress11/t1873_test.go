package regress11

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1873: a bare failable call as the entire condition of if/while/for/if-expr
// previously emitted malformed IR — the raw {i1, i1, i8*} failable struct was
// used as the branch condition. Sema now marks it for auto-propagation, and
// codegen uses genExprAutoPropagate so the struct is unwrapped before the branch.

func TestT1873_IfCondAutoPropagate(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		f!() bool { return true; }
		g!() int {
			if f() { return 1; }
			return 0;
		}
		main() {}
	`)
	body := codegentest.ExtractFunction(ir, "__user.g")
	if body == "" {
		t.Fatalf("expected @__user.g in IR:\n%s", ir)
	}
	if !strings.Contains(body, "auto.propagate") || !strings.Contains(body, "auto.ok") {
		t.Fatalf("expected auto-propagate of failable condition in if stmt:\n%s", body)
	}
}

func TestT1873_WhileCondAutoPropagate(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		f!() bool { return false; }
		g!() {
			while f() { break; }
		}
		main() {}
	`)
	body := codegentest.ExtractFunction(ir, "__user.g")
	if body == "" {
		t.Fatalf("expected @__user.g in IR:\n%s", ir)
	}
	if !strings.Contains(body, "auto.propagate") || !strings.Contains(body, "auto.ok") {
		t.Fatalf("expected auto-propagate of failable condition in while stmt:\n%s", body)
	}
}

func TestT1873_ForCondAutoPropagate(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		f!() bool { return false; }
		g!() {
			for int i = 0; f(); i++ { break; }
		}
		main() {}
	`)
	body := codegentest.ExtractFunction(ir, "__user.g")
	if body == "" {
		t.Fatalf("expected @__user.g in IR:\n%s", ir)
	}
	if !strings.Contains(body, "auto.propagate") || !strings.Contains(body, "auto.ok") {
		t.Fatalf("expected auto-propagate of failable condition in for stmt:\n%s", body)
	}
}

func TestT1873_IfExprCondAutoPropagate(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		f!() bool { return true; }
		g!() int {
			v := if f() { 1 } else { 0 };
			return v;
		}
		main() {}
	`)
	body := codegentest.ExtractFunction(ir, "__user.g")
	if body == "" {
		t.Fatalf("expected @__user.g in IR:\n%s", ir)
	}
	if !strings.Contains(body, "auto.propagate") || !strings.Contains(body, "auto.ok") {
		t.Fatalf("expected auto-propagate of failable condition in if expr:\n%s", body)
	}
}

func TestT1873_IfStmtValueCondAutoPropagate(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		f!() bool { return true; }
		g!() int {
			int x = if f() { 1; } else { 0; };
			return x;
		}
		main() {}
	`)
	body := codegentest.ExtractFunction(ir, "__user.g")
	if body == "" {
		t.Fatalf("expected @__user.g in IR:\n%s", ir)
	}
	if !strings.Contains(body, "auto.propagate") || !strings.Contains(body, "auto.ok") {
		t.Fatalf("expected auto-propagate of failable condition in if-stmt-value:\n%s", body)
	}
}

// T1873: failable method call as an if condition must also auto-propagate.
func TestT1873_MethodCallCondAutoPropagate(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Checker {
			check!(this) bool { return true; }
		}
		g!() int {
			c := Checker();
			if c.check() { return 1; }
			return 0;
		}
		main() {}
	`)
	body := codegentest.ExtractFunction(ir, "__user.g")
	if body == "" {
		t.Fatalf("expected @__user.g in IR:\n%s", ir)
	}
	if !strings.Contains(body, "auto.propagate") || !strings.Contains(body, "auto.ok") {
		t.Fatalf("expected auto-propagate of failable method call in if condition:\n%s", body)
	}
}

// T1873: if/else with failable condition — both branches must be reachable.
func TestT1873_IfElseCondAutoPropagate(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		f!() bool { return false; }
		g!() int {
			if f() { return 1; } else { return 2; }
		}
		main() {}
	`)
	body := codegentest.ExtractFunction(ir, "__user.g")
	if body == "" {
		t.Fatalf("expected @__user.g in IR:\n%s", ir)
	}
	if !strings.Contains(body, "auto.propagate") || !strings.Contains(body, "auto.ok") {
		t.Fatalf("expected auto-propagate in if/else with failable condition:\n%s", body)
	}
	// Both then and else blocks must exist.
	if !strings.Contains(body, "if.then") || !strings.Contains(body, "if.else") {
		t.Fatalf("expected both if.then and if.else blocks:\n%s", body)
	}
}

// T1873: nested failable conditions — both if conditions must auto-propagate.
func TestT1873_NestedFailableCondAutoPropagate(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		a!() bool { return true; }
		b!() bool { return false; }
		g!() int {
			if a() {
				if b() { return 2; }
				return 1;
			}
			return 0;
		}
		main() {}
	`)
	body := codegentest.ExtractFunction(ir, "__user.g")
	if body == "" {
		t.Fatalf("expected @__user.g in IR:\n%s", ir)
	}
	// Two auto-propagate sites expected (one per failable condition).
	count := strings.Count(body, "auto.propagate")
	if count < 2 {
		t.Fatalf("expected at least 2 auto.propagate blocks for nested failable conditions, got %d:\n%s", count, body)
	}
}
