package ownership

import "testing"

// T1436: a while-let consumes its optional source exactly like if-let —
// genWhileUnwrapStmt clears the source ident's drop flag and drops the
// unwrapped payload per iteration. Ownership must therefore mark the source
// Moved so a later unwrap of the same source is rejected as a use-after-move
// instead of reading the freed payload at runtime (SEGV at the field offset).
// Before the fix, checkWhileUnwrapStmt never called tryMove, so the while-let
// path accepted what the if-let path already rejected.

// The issue's nested repro shape: an inner while-let consumes an owned `T?`
// binding, and a later if-let on the same binding is a use-after-move.
func TestT1436NestedWhileLetThenIfLetIsMoveError(t *testing.T) {
	errs := ownerErrs(t, `
		type Row { string name; }
		f() {
			Row? inner = Row(name: "deep3".to_string());
			Row?? outer = inner;
			while first := outer {
				while r := first { break; }
				if r2 := first { assert(r2.name == "deep3", "d"); }
				break;
			}
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'first'")
}

// Flat variant: a top-level while-let over an owned `T?` local, followed by an
// if-let on the same local.
func TestT1436FlatWhileLetThenIfLetIsMoveError(t *testing.T) {
	errs := ownerErrs(t, `
		type Row { string name; }
		f() {
			Row? o = Row(name: "x".to_string());
			while r := o { break; }
			if r2 := o { assert(r2.name == "x", "x"); }
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'o'")
}

// Two consecutive while-lets on the same owned optional local are also a
// use-after-move (symmetric with the if-let/if-let case).
func TestT1436WhileLetThenWhileLetIsMoveError(t *testing.T) {
	errs := ownerErrs(t, `
		type Row { string name; }
		f() {
			Row? o = Row(name: "x".to_string());
			while r := o { break; }
			while r2 := o { break; }
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'o'")
}

// Positive: a while-let body that reassigns the source re-inits it, so a
// terminating loop stays legal — tryMove must not over-reject this.
func TestT1436WhileLetReassignInBodyOK(t *testing.T) {
	ownerOK(t, `
		type Row { string name; }
		f() {
			Row? o = Row(name: "x".to_string());
			while r := o {
				o = none;
			}
		}
	`)
}

// Positive: a copy-type optional source (`int?`) is a tryMove no-op — copy
// values are never moved out — so repeated unwraps of the same source stay
// legal. Guards against the fix over-rejecting the whole copy-optional category.
func TestT1436WhileLetCopyOptionalReuseOK(t *testing.T) {
	ownerOK(t, `
		f() {
			int? o = 5;
			while r := o { o = none; }
			if r2 := o { assert(r2 == 5, "5"); }
		}
	`)
}

// Positive: a call/getter source is not an ident, so tryMove is a no-op and the
// common `while x := producer.next()` drain form stays legal.
func TestT1436WhileLetCallSourceOK(t *testing.T) {
	ownerOK(t, `
		type Row { string name; }
		type Producer {
			int n;
			next(this) Row? {
				if this.n > 0 { return Row(name: "x".to_string()); }
				return none;
			}
		}
		f() {
			p := Producer(n: 1);
			while r := p.next() { break; }
		}
	`)
}
