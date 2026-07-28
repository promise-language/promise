package codegen

import (
	"strings"
	"testing"
)

// T1090: compound assignment through index/slice access must evaluate the RHS
// before the container read, giving both the [] (genMethodCompoundAssign) and
// [:] (genSliceCompoundAssign) paths one canonical order:
//
//	target → key/bounds → RHS → read → operator → write.
//
// The reorder is observable only when the RHS side-effects the target, so these
// tests pin the emitted call order in the user-main coroutine body: the RHS call
// (@__user.side) must precede the getter read call.

// userMainBody returns the body text of the user main coroutine (`.goroutine.main`),
// so ordering assertions ignore the type definitions / vtable globals earlier in
// the module (which also mention the getter symbol).
func userMainBody(t *testing.T, ir string) string {
	t.Helper()
	start := strings.Index(ir, "define i8* @.goroutine.main()")
	if start < 0 {
		t.Fatalf("could not find user main coroutine in IR")
	}
	end := strings.Index(ir[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("could not find end of user main coroutine in IR")
	}
	return ir[start : start+end]
}

func TestT1090_MethodIndexCompoundEvalsRHSBeforeRead(t *testing.T) {
	ir := generateIR(t, `
		side() int { return 5; }
		type Box {
			int x;
			[](int i) int { return this.x; }
			[]=(int i, int v) { this.x = v; }
		}
		main() {
			b := Box(x: 10);
			b[0] += side();
		}
	`)
	body := userMainBody(t, ir)
	rhs := strings.Index(body, "@__user.side()")
	read := strings.Index(body, `@"Box.[]"(`)
	if rhs < 0 || read < 0 {
		t.Fatalf("expected both side() and Box.[] calls in main; rhs=%d read=%d", rhs, read)
	}
	if rhs > read {
		t.Errorf("RHS side() must be evaluated before the [] read (T1090); rhs=%d read=%d", rhs, read)
	}
}

func TestT1090_SliceCompoundEvalsRHSBeforeRead(t *testing.T) {
	ir := generateIR(t, `
		side() int { return 5; }
		type Box {
			int x;
			[:](int? low, int? high) int { return this.x; }
			[:]=(int? low, int? high, int v) { this.x = v; }
		}
		main() {
			b := Box(x: 10);
			b[0:1] += side();
		}
	`)
	body := userMainBody(t, ir)
	rhs := strings.Index(body, "@__user.side()")
	read := strings.Index(body, `@"Box.[:]"(`)
	if rhs < 0 || read < 0 {
		t.Fatalf("expected both side() and Box.[:] calls in main; rhs=%d read=%d", rhs, read)
	}
	if rhs > read {
		t.Errorf("RHS side() must be evaluated before the [:] read (T1090); rhs=%d read=%d", rhs, read)
	}
}
