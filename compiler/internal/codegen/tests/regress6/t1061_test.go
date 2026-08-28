package regress6

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1061: maybeRegisterStructuralFree (via isFreshOwnedStructuralRHS) previously
// classified ANY CallExpr/UnaryExpr/BinaryExpr RHS as a fresh owned structural
// allocation and registered a bindingFree for the binding. For a BORROW-returning
// structural operator/method (`T&`) the result aliases the receiver's heap
// instance, so registering a free risks a premature/double free. The fix mirrors
// the IndexExpr/MemberExpr borrow guards: `!isBorrowedExpr`. The discriminating
// signal is that a structural binding with no owning free has no `<name>.dropflag`
// at all (structural types are excluded from maybeRegisterDrop); the over-
// registration is what created one.

// Borrow-returning structural UNARY operator — `m := -it` must NOT get a drop flag.
func TestT1061StructuralBorrowUnaryNoFree(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type SNeg `+"`structural"+` {
			int v;
			-() SNeg& { return this; }
		}
		type SNegItem is SNeg { int v; }
		main() { it := SNegItem(v: 8); m := -it; }
	`)
	codegentest.AssertNotContains(t, ir, "%m.dropflag")
}

// Borrow-returning structural BINARY operator — `m := a + b` must NOT get a flag.
func TestT1061StructuralBorrowBinaryNoFree(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type SAdd `+"`structural"+` {
			int v;
			+(SAdd other) SAdd& { return this; }
		}
		type SAddItem is SAdd { int v; }
		main() { a := SAddItem(v: 7); b := SAddItem(v: 3); m := a + b; }
	`)
	codegentest.AssertNotContains(t, ir, "%m.dropflag")
}

// Borrow-returning structural METHOD call — `m := it.borrow_self()` must NOT flag.
func TestT1061StructuralBorrowMethodNoFree(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type SBorrow `+"`structural"+` {
			int v;
			borrow_self(this) SBorrow& { return this; }
		}
		type SBorrowItem is SBorrow { int v; }
		main() { it := SBorrowItem(v: 5); m := it.borrow_self(); }
	`)
	codegentest.AssertNotContains(t, ir, "%m.dropflag")
}

// Negative control: an OWNED-returning structural operator (`SNeg`, no `&`) still
// aliases via clone-on-return but the structural binding owns the fresh box and
// MUST register the free — the fix only narrows the borrow case, so this binding
// keeps its drop flag. Guards against the fix over-broadening to owned returns.
func TestT1061StructuralOwnedUnaryStillFrees(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type SNeg `+"`structural"+` {
			int v;
			-() SNeg { return this; }
		}
		type SNegItem is SNeg { int v; }
		main() { it := SNegItem(v: 8); m := -it; }
	`)
	codegentest.AssertContains(t, ir, "%m.dropflag")
}

// The tests above all use inferred `m := <borrow>`, which routes through
// genInferredVarDecl. An EXPLICITLY ref-typed local (`SNeg& m = <borrow>`) routes
// through genTypedVarDecl — a DISTINCT maybeRegisterStructuralFree call site
// (extractNamed peels the `&`, so `named.IsStructural()` still passes and the
// borrow guard is what suppresses the free). Without the fix these registered a
// drop flag over borrowed storage at that site too; cover it directly.

// Explicit `SNeg& m = -it` — genTypedVarDecl path must NOT register a free.
func TestT1061StructuralBorrowUnaryExplicitRefNoFree(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type SNeg `+"`structural"+` {
			int v;
			-() SNeg& { return this; }
		}
		type SNegItem is SNeg { int v; }
		main() { it := SNegItem(v: 8); SNeg& m = -it; }
	`)
	codegentest.AssertNotContains(t, ir, "%m.dropflag")
}

// Explicit `SAdd& m = a + b` — genTypedVarDecl path, binary operator.
func TestT1061StructuralBorrowBinaryExplicitRefNoFree(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type SAdd `+"`structural"+` {
			int v;
			+(SAdd other) SAdd& { return this; }
		}
		type SAddItem is SAdd { int v; }
		main() { a := SAddItem(v: 7); b := SAddItem(v: 3); SAdd& m = a + b; }
	`)
	codegentest.AssertNotContains(t, ir, "%m.dropflag")
}

// Explicit `SBorrow& m = it.borrow_self()` — genTypedVarDecl path, method call.
func TestT1061StructuralBorrowMethodExplicitRefNoFree(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type SBorrow `+"`structural"+` {
			int v;
			borrow_self(this) SBorrow& { return this; }
		}
		type SBorrowItem is SBorrow { int v; }
		main() { it := SBorrowItem(v: 5); SBorrow& m = it.borrow_self(); }
	`)
	codegentest.AssertNotContains(t, ir, "%m.dropflag")
}
