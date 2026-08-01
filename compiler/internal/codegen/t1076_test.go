package codegen

import (
	"testing"
)

// T1076: for-in over a Stream whose iter() returns a CONCRETE (non-structural)
// iterator that owns a heap resource must drop that iterator via RTTI dispatch
// (__promise_structural_drop reads typeinfo.drop_fn_ptr → runs the type's drop),
// NOT a bare pal_free. Before the fix, genForInCustomStream chose pal_free for
// the concrete branch, freeing the instance struct without running its drop() →
// the owned string leaked.
func TestT1076ForInConcreteStreamIterDrop(t *testing.T) {
	ir := generateIR(t, `
		type LeakyIter {
			string label;
			int i;
			int n;
			next(~this) int? {
				if this.n <= 0 {
					return none;
				}
				int v = this.i;
				this.i = this.i + 1;
				this.n = this.n - 1;
				return v;
			}
		}
		type LeakyStream {
			int count;
			iter() LeakyIter {
				return LeakyIter(label: "x".repeat(4), i: 0, n: this.count);
			}
		}
		main() {
			s := LeakyStream(count: 3);
			for x in s {}
		}
	`)
	// main's body runs as a coroutine (.goroutine.main); the for-in loop and its
	// iterator scope binding live there.
	fn := extractDefine(ir, ".goroutine.main")
	if fn == "" {
		t.Fatal("expected .goroutine.main to be emitted")
	}
	// The fresh concrete iterator temp (__forin_iter_tmp) must be dropped via the
	// RTTI path, not a bare pal_free.
	assertContains(t, fn, "__forin_iter_tmp")
	assertContains(t, fn, "__promise_structural_drop")
}
