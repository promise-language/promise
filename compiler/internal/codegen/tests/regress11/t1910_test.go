package regress11

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1910: the user-type field dup walk carried its own copy of the per-field dispatch,
// and the copy was missing arms the variant walk had — structural-interface, enum and
// tuple fields fell through every branch and were left as the shallow memcpy. Two
// now-droppable owners then held one box / one variant payload / one tuple element,
// and the second drop died. The two walks are now one.

func TestT1910_StructuralFieldIsDeepClonedNotAliased(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Show `+"`"+`structural { to_string() string `+"`"+`abstract; }
		type Wrap { Show s; int n; }
		main() {
			string b = "p" + "q";
			Wrap w = Wrap(s: move b, n: 1);
			Wrap[] ws = [w];
			Wrap[] copy = ws.clone();
		}
	`)
	clone := codegentest.FindDefinedFunc(ir, "@Wrap.__clone(")
	if clone == "" {
		t.Fatalf("expected a synthesized Wrap clone")
	}
	if !strings.Contains(clone, "@__promise_structural_clone") {
		t.Fatalf("the structural field must be deep-cloned through RTTI, got:\n%s", clone)
	}
}

func TestT1910_EnumFieldIsDupedNotAliased(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		enum Payload { Text(string t), Num(int n) }
		type WithEnum { Payload p; int n; }
		main() {
			WithEnum w = WithEnum(p: Payload.Text("a" + "b"), n: 1);
			WithEnum[] ws = [w];
			WithEnum[] copy = ws.clone();
		}
	`)
	clone := codegentest.FindDefinedFunc(ir, "@WithEnum.__clone(")
	if clone == "" {
		t.Fatalf("expected a synthesized WithEnum clone")
	}
	// The enum arm dups the variant payload in place; a shallow copy would emit no
	// string dup at all.
	if !strings.Contains(clone, "@promise_string_new") && !strings.Contains(clone, "@promise_string_concat") &&
		!strings.Contains(clone, "dup") {
		t.Fatalf("enum field must be duped, got:\n%s", clone)
	}
}

func TestT1910_TupleFieldIsDupedNotAliased(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type WithTuple { (string, int) t; int n; }
		main() {
			WithTuple w = WithTuple(t: ("a" + "b", 2), n: 1);
			WithTuple[] ws = [w];
			WithTuple[] copy = ws.clone();
		}
	`)
	clone := codegentest.FindDefinedFunc(ir, "@WithTuple.__clone(")
	if clone == "" {
		t.Fatalf("expected a synthesized WithTuple clone")
	}
	if !strings.Contains(clone, "@promise_string_new") {
		t.Fatalf("the tuple's string element must be duped, got:\n%s", clone)
	}
}

// The arms the field walk already had must keep working — the unification adopted the
// variant walk's spellings (null-safe vector element clone, clone()-preferring nested
// heap dup), so these are the cases most at risk from the swap.
func TestT1910_ExistingFieldArmsStillDup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Inner { string s; }
		type Outer { string s; string[] v; Inner i; int n; }
		main() {
			Outer o = Outer(s: "a" + "b", v: ["c" + "d"], i: Inner(s: "e" + "f"), n: 1);
			Outer[] os = [o];
			Outer[] copy = os.clone();
		}
	`)
	clone := codegentest.FindDefinedFunc(ir, "@Outer.__clone(")
	if clone == "" {
		t.Fatalf("expected a synthesized Outer clone")
	}
	if !strings.Contains(clone, "@promise_string_new") {
		t.Fatalf("string field must still be duped, got:\n%s", clone)
	}
	if !strings.Contains(clone, "@pal_alloc") {
		t.Fatalf("vector/nested-heap fields must still allocate their own copies, got:\n%s", clone)
	}
}
