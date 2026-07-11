package ownership

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/types"
)

// TestT1096ExtractEnumOrigin exercises every arm of extractEnumOrigin directly.
// The unwrap arms (SharedRef/MutRef) and the nil fall-throughs are not reachable
// from the ownership test harness's mini-stdlib (it has no Ref/Arc, and a plain
// enum parameter types as a bare *types.Enum), so a focused unit test pins the
// helper's parity with codegen's extractEnum for all type shapes. The
// SharedRef-around-enum shape is additionally covered end-to-end by
// tests/e2e/enum_getter_closure_test.pr (`a.borrow.adder`).
func TestT1096ExtractEnumOrigin(t *testing.T) {
	enumTN := types.NewTypeName(types.Pos{}, "E", nil)
	enum := types.NewEnum(enumTN, nil)

	namedTN := types.NewTypeName(types.Pos{}, "S", nil)
	named := types.NewNamed(namedTN, nil)

	cases := []struct {
		name string
		in   types.Type
		want *types.Enum
	}{
		{"bare enum", enum, enum},
		{"instance of enum", types.NewInstance(enum, []types.Type{types.TypInt}), enum},
		{"instance of named (non-enum)", types.NewInstance(named, []types.Type{types.TypInt}), nil},
		{"shared ref of enum", types.NewSharedRef(enum), enum},
		{"mut ref of enum", types.NewMutRef(enum), enum},
		{"shared ref of enum instance", types.NewSharedRef(types.NewInstance(enum, []types.Type{types.TypInt})), enum},
		{"shared ref of non-enum", types.NewSharedRef(types.TypInt), nil},
		{"non-enum scalar", types.TypInt, nil},
		{"named type", named, nil},
	}
	for _, tc := range cases {
		if got := extractEnumOrigin(tc.in); got != tc.want {
			t.Errorf("extractEnumOrigin(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// T1096: closureAggregateBorrowSource's owned-return getter exclusion previously
// only handled struct (Named) getters via extractNamedType, which returns nil for
// enum-origin types. An enum getter returning a fresh OWNED closure was therefore
// misclassified as a borrow, and a returned/re-stored local was falsely rejected —
// diverging from codegen's isGetterCallExpr, which additionally excludes enum
// getters via extractEnum(...).LookupGetter. These tests lock in the restored
// lockstep: an enum getter's closure is owned by the local and may be returned.

func TestT1096EnumGetterClosureReturnAccepted(t *testing.T) {
	ownerOK(t, `
enum E {
  A, B,
  get adder() -> int {
    int base = 41;
    return move || -> base + 1;
  }
}
make_adder(E e) (() -> int) {
  f := e.adder;
  return f;
}
main() {}
`)
}

func TestT1096GenericEnumGetterClosureReturnAccepted(t *testing.T) {
	ownerOK(t, `
enum E[T] {
  A, B,
  get adder() -> int {
    int base = 41;
    return move || -> base + 1;
  }
}
make_adder(E[int] e) (() -> int) {
  f := e.adder;
  return f;
}
main() {}
`)
}

// Control case: the struct (Named) arm keeps working — pins the parity that the
// enum arm now mirrors.
func TestT1096StructGetterClosureReturnAccepted(t *testing.T) {
	ownerOK(t, `
type T {
  int dummy;
  get adder() -> int {
    int base = 41;
    return move || -> base + 1;
  }
}
make_adder(T t) (() -> int) {
  f := t.adder;
  return f;
}
main() {}
`)
}
