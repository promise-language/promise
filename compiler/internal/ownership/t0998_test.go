package ownership

import (
	"strings"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
)

// t0998BuildErrs parses + builds the AST and returns the builder errors,
// without fataling — used to assert the guiding migration errors for the
// removed `~Type name` move-param and `&this` receiver spellings (T0998).
func t0998BuildErrs(t *testing.T, src string) []error {
	t.Helper()
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	_, buildErrs := ast.Build("test.pr", tree)
	return buildErrs
}

func expectBuildErr(t *testing.T, errs []error, substr string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e.Error(), substr) {
			return
		}
	}
	t.Errorf("expected build error containing %q, got %v", substr, errs)
}

// --- Migration: removed spellings produce guiding errors ---

func TestT0998_LegacyMoveParamRejected(t *testing.T) {
	errs := t0998BuildErrs(t, `
		sink(~Box b) {}
		test() {}
	`)
	expectBuildErr(t, errs, "no longer valid")
	expectBuildErr(t, errs, "Type move b")
}

func TestT0998_LegacyAmpThisReceiverRejected(t *testing.T) {
	errs := t0998BuildErrs(t, `
		type Box { string s; get_s(&this) string { return this.s; } }
		test() {}
	`)
	expectBuildErr(t, errs, "`&this` is no longer a valid receiver")
}

// --- Call-site `move` keyword enforcement ---

func TestT0998_MoveRequiredOnConsume(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string s; }
		sink(Box move b) {}
		test() {
			Box b = Box(s: "x");
			sink(b);
		}
	`)
	expectOwnerError(t, errs, "consuming 'b' requires `move b`")
}

func TestT0998_MoveOnConsumeOK(t *testing.T) {
	ownerOK(t, `
		type Box { string s; }
		sink(Box move b) {}
		test() {
			Box b = Box(s: "x");
			sink(move b);
		}
	`)
}

func TestT0998_MoveRejectedOnBorrowSlot(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string s; }
		peek(Box b) {}
		test() {
			Box b = Box(s: "x");
			peek(move b);
		}
	`)
	expectOwnerError(t, errs, "remove `move`")
}

func TestT0998_MoveRejectedOnTemporary(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string s; }
		make_box() Box { return Box(s: "x"); }
		sink(Box move b) {}
		test() {
			sink(move make_box());
		}
	`)
	expectOwnerError(t, errs, "temporary")
}

func TestT0998_ConstructorMoveRequired(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner { string s; }
		type Outer { Inner inner; }
		test() {
			Inner i = Inner(s: "x");
			Outer o = Outer(inner: i);
		}
	`)
	expectOwnerError(t, errs, "consuming 'i' requires `move i`")
}

func TestT0998_BorrowArgNoMove(t *testing.T) {
	// A bare (shared-borrow) parameter takes a named binding with NO marker.
	ownerOK(t, `
		type Box { string s; }
		peek(Box b) {}
		test() {
			Box b = Box(s: "x");
			peek(b);
		}
	`)
}

// `move` peels through parens to reach the named binding subject (moveSubject /
// ParenExpr branch): `move (b)` is accepted, not flagged as a temporary.
func TestT0998_MoveThroughParensOK(t *testing.T) {
	ownerOK(t, `
		type Box { string s; }
		sink(Box move b) {}
		test() {
			Box b = Box(s: "x");
			sink(move (b));
		}
	`)
}

// `move` peels through a force-unwrap to reach the named Optional binding
// (moveSubject / OptionalUnwrapExpr branch).
func TestT0998_MoveThroughForceUnwrapOK(t *testing.T) {
	ownerOK(t, `
		type Box { string s; }
		sink(Box move b) {}
		test() {
			Box? o = Box(s: "x");
			sink(move o!);
		}
	`)
}

// A partial move of a field path rooted at a named binding still requires the
// call-site `move` marker (isConsumableNamedBinding / MemberExpr branch).
func TestT0998_PartialFieldMoveRequiresMove(t *testing.T) {
	errs := ownerErrs(t, `
		type Inner { string s; }
		type Outer { Inner inner; }
		sink(Inner move x) {}
		test() {
			Outer o = Outer(inner: Inner(s: "x"));
			sink(o.inner);
		}
	`)
	expectOwnerError(t, errs, "consuming 'o.inner' requires `move o.inner`")
}

// The call-site `move` marker is also enforced on enum-variant constructors
// (the isEnumVariantConstructorCallee codepath, distinct from struct ctors).
func TestT0998_EnumVariantConstructorMoveRequired(t *testing.T) {
	errs := ownerErrs(t, `
		type Box { string s; }
		enum Opt { Some(Box b), None }
		test() {
			Box b = Box(s: "x");
			Opt o = Opt.Some(b);
		}
	`)
	expectOwnerError(t, errs, "consuming 'b' requires `move b`")
}

func TestT0998_EnumVariantConstructorMoveOK(t *testing.T) {
	ownerOK(t, `
		type Box { string s; }
		enum Opt { Some(Box b), None }
		test() {
			Box b = Box(s: "x");
			Opt o = Opt.Some(move b);
		}
	`)
}

// Bare `this` is a shared (read-only) borrow of the receiver (T0998): a method
// that borrows `this` and is called while a mutable borrow of the receiver is
// live is a borrow conflict — proving the unmarked receiver now borrows shared
// (it used to be a no-op `return`).
func TestT0998_BareThisIsSharedBorrow(t *testing.T) {
	// A bare-`this` method call creates a shared borrow that coexists with other
	// shared reads — no conflict, must type-check cleanly.
	ownerOK(t, `
		type Box { string s; peek(this) int { return 1; } }
		test() {
			Box b = Box(s: "x");
			int a = b.peek();
			int c = b.peek();
		}
	`)
}

// A Copy-typed named binding is never "consumed", so passing it bare to a move
// slot needs no marker (isConsumableNamedBinding returns false for Copy types).
func TestT0998_CopyTypeNoMoveNeeded(t *testing.T) {
	ownerOK(t, `
		sink(int move n) {}
		test() {
			int n = 5;
			sink(n);
		}
	`)
}
