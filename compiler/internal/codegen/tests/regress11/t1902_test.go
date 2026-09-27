package regress11

import (
	"regexp"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1902: a structural box's typeinfo header used to carry typeID 0 and no parents,
// whatever it held. `promise_type_is` reads field 3, found 0, and answered false for
// every `is`/`as` whose subject was a boxed primitive, string or opaque container —
// while the same question about the unboxed value answered correctly. The header now
// carries the payload's own identity, from the same typeIdentityIDs the concrete
// type's own typeinfo uses, so one RTTI walk serves boxed and unboxed alike.

// typeinfoID returns (typeID, numParents) read out of a typeinfo global's initializer.
func typeinfoID(t *testing.T, ir, global string) (string, string) {
	t.Helper()
	// Field 3 is the type ID and field 4 the parent count; both are the last two
	// scalar i32s before the optional [N x i32] parent array.
	re := regexp.MustCompile(regexp.QuoteMeta(global) + ` = constant [^\n]*i32 (\d+), i32 (\d+)`)
	m := re.FindStringSubmatch(ir)
	if m == nil {
		t.Fatalf("no typeinfo global %s in IR", global)
	}
	return m[1], m[2]
}

func TestT1902_PrimitiveBoxCarriesConcreteTypeID(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		main() {
			Showable s = 42;
			bool ok = s is int;
		}
	`)
	boxID, _ := typeinfoID(t, ir, `@promise_typeinfo_box$int$flat`)
	intID, _ := typeinfoID(t, ir, `@promise_typeinfo___mod_std_int`)
	if boxID != intID {
		t.Fatalf("boxed int must answer `is int`: box typeID %s, int typeID %s", boxID, intID)
	}
}

func TestT1902_StringBoxCarriesConcreteTypeID(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		main() {
			Showable s = "hi";
			bool ok = s is string;
		}
	`)
	boxID, _ := typeinfoID(t, ir, `@promise_typeinfo_box$string`)
	strID, _ := typeinfoID(t, ir, `@promise_typeinfo___mod_std_string`)
	if boxID != strID {
		t.Fatalf("boxed string must answer `is string`: box typeID %s, string typeID %s", boxID, strID)
	}
}

func TestT1902_ContainerBoxCarriesMonoTypeIDAndOriginParent(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			Cloneable c = [1, 2, 3];
			bool ok = c is Vector[int];
		}
	`)
	boxID, boxParents := typeinfoID(t, ir, `@"promise_typeinfo_box$Vector[int]"`)
	monoID, _ := typeinfoID(t, ir, `@"promise_typeinfo_Vector[int]"`)
	if boxID != monoID {
		t.Fatalf("boxed Vector[int] must answer `is Vector[int]`: box typeID %s, mono typeID %s", boxID, monoID)
	}
	// The origin's ID is listed as a parent so a bare `is Vector` matches too —
	// exactly what emitMonoTypeInfoGlobals records for the unboxed instance.
	if boxParents == "0" {
		t.Fatalf("box header must list the generic origin as a parent, got numParents=0")
	}
}

// Two primitives of the same LLVM size must not share one header: the flat box was
// keyed by SIZE before T1902, so `int` and `i64` (both 16-byte boxes) would have
// answered each other's `is` checks.
func TestT1902_SameSizedPrimitivesGetDistinctBoxHeaders(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Showable `+"`"+`structural {
			to_string() string `+"`"+`abstract;
		}
		show(Showable s) string { return s.to_string(); }
		main() {
			int a = 1;
			i64 b = 2;
			show(a);
			show(b);
		}
	`)
	if !strings.Contains(ir, `@promise_typeinfo_box$int$flat = constant`) ||
		!strings.Contains(ir, `@promise_typeinfo_box$i64$flat = constant`) {
		t.Fatalf("int and i64 boxes must get their own headers; the size-keyed header conflated them")
	}
	intID, _ := typeinfoID(t, ir, `@promise_typeinfo_box$int$flat`)
	i64ID, _ := typeinfoID(t, ir, `@promise_typeinfo_box$i64$flat`)
	if intID == i64ID {
		t.Fatalf("int and i64 box headers must carry distinct type IDs, both %s", intID)
	}
	// One shared flat clone per SIZE is still correct — a byte copy does not depend
	// on which concrete it copies.
	if !strings.Contains(ir, `@__promise_flat_box_clone_16`) {
		t.Fatalf("expected the size-keyed flat clone to stay shared")
	}
}
