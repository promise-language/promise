package codegen

import (
	"strings"
	"testing"
)

// T0512: nested if-let / while-let on a borrowed Optional<Optional<HeapUser>>
// from a match destructure segfaulted (double-free → SIGSEGV at 0x0). The fix
// (codegen/stmt.go) propagates the T0485 `matchBorrowedIdents` borrow marker
// through if-let / while-let unwrap: when the unwrap source is an *ast.IdentExpr
// already marked borrowed, the unwrapped binding still aliases variant-owned
// memory (the synth enum drop walks the full nested Optional chain), so it is
// also marked borrowed for the body's lifetime. Mirrors the adjacent
// dropFlag/dropBinding save/restore and the genEnumMatch arm snapshot/revert.
//
// These tests are the Go-codegen siblings of t0585_test.go (same two functions,
// genIfUnwrapStmt / genWhileUnwrapStmt). They pin the IR-level invariant: the
// inner unwrap binding gets NO owned drop binding (no `<name>.dropflag`, no
// inner-type drop call in the driver), so the heap value is freed exactly once
// by the synthesized enum drop. Without the fix, the inner if-let/while-let
// registers a drop for the inner Row → a second free → UAF/SIGSEGV.
//
// Note on the `hadBorrowMark=true` save/revert branch: it cannot be reached
// from valid Promise source. Promise sema forbids variable shadowing, and
// `matchBorrowedIdents` only ever holds names currently in lexical scope (match
// bindings + propagated if-let/while-let bindings, all live). An if-let whose
// binding name already exists in the map would require shadowing a live name —
// rejected by sema. The branch is defensive symmetry with the adjacent
// dropFlag/dropBinding save/restore (identical rationale), so it has no test.

// TestT0512_NestedIfUnwrapBorrowedMatchBindingNoInnerDrop — the tracker repro.
// `match w { _W.Some(opt_opt) => if first := opt_opt { if r := first {...} } }`
// where the variant field is `_Row?? value` (Optional<Optional<HeapUser>>).
// opt_opt is a match-borrowed binding; the outer if-let must propagate the
// borrow to `first` so the inner `if r := first` does NOT register a drop.
// The inner Row must be freed only by the synth enum drop `@_W.drop`.
func TestT0512_NestedIfUnwrapBorrowedMatchBindingNoInnerDrop(t *testing.T) {
	ir := generateIR(t, `
		type _Row { string name; }
		enum _W { Empty, Some(_Row?? value), }
		_drive(~_W w) {
			match w {
				_W.Some(opt_opt) => {
					if first := opt_opt {
						if r := first {
							print_line(r.name);
						}
					}
				},
				_W.Empty => print_line("empty"),
			}
		}
		main() {
			_Row? inner = _Row(name: "deep".to_string());
			_Row?? outer = inner;
			_drive(_W.Some(outer));
		}
	`)
	body := extractFunction(ir, "__user._drive")
	if body == "" {
		t.Fatalf("expected __user._drive in IR")
	}
	// T0512: the inner unwrap binding `r` must NOT get an owned drop binding —
	// without the propagated borrow it would (isOwnedOptionalExpr(first)=true),
	// emitting `%r.dropflag` and an `@_Row.drop` call → double-free.
	if strings.Contains(body, "%r.dropflag") {
		t.Errorf("T0512: inner binding `r` must NOT have a drop flag (borrow propagated from match binding):\n%s", body)
	}
	if strings.Contains(body, "%first.dropflag") {
		t.Errorf("T0485/T0512: outer binding `first` must NOT have a drop flag (opt_opt is match-borrowed):\n%s", body)
	}
	if strings.Contains(body, "call void @_Row.drop") {
		t.Errorf("T0512: __user._drive must NOT drop the inner Row — the synth enum drop owns it:\n%s", body)
	}
	// The consumed `~_W w` is dropped via the synthesized enum drop, which walks
	// the nested Optional chain and frees the inner Row exactly once.
	if !strings.Contains(body, "call void @_W.drop") {
		t.Errorf("T0512: expected the synth enum drop @_W.drop to be called (sole owner of the inner Row):\n%s", body)
	}
	enumDrop := extractFunction(ir, "_W.drop")
	if enumDrop == "" {
		t.Fatalf("expected synthesized @_W.drop function in IR")
	}
	if n := strings.Count(enumDrop, "call void @_Row.drop"); n != 1 {
		t.Errorf("T0512/T0485: expected exactly one @_Row.drop call in @_W.drop (nested Optional walk frees inner once), got %d:\n%s", n, enumDrop)
	}
}

// TestT0512_NestedWhileUnwrapBorrowedMatchBindingNoInnerDrop — symmetric to the
// if-let case for genWhileUnwrapStmt. `while first := opt_opt { if r := first
// {...} break; }`. The while-let must propagate opt_opt's borrow to `first` so
// the inner unwrap on `first` does not transfer ownership.
func TestT0512_NestedWhileUnwrapBorrowedMatchBindingNoInnerDrop(t *testing.T) {
	ir := generateIR(t, `
		type _Row { string name; }
		enum _W { Empty, Some(_Row?? value), }
		_drivew(~_W w) {
			match w {
				_W.Some(opt_opt) => {
					while first := opt_opt {
						if r := first {
							print_line(r.name);
						}
						break;
					}
				},
				_W.Empty => print_line("empty"),
			}
		}
		main() {
			_Row? inner = _Row(name: "deep".to_string());
			_Row?? outer = inner;
			_drivew(_W.Some(outer));
		}
	`)
	body := extractFunction(ir, "__user._drivew")
	if body == "" {
		t.Fatalf("expected __user._drivew in IR")
	}
	bodyIdx := strings.Index(body, "whileunwrap.body")
	if bodyIdx < 0 {
		t.Fatalf("expected whileunwrap.body block in __user._drivew:\n%s", body)
	}
	if strings.Contains(body, "%r.dropflag") {
		t.Errorf("T0512: inner binding `r` must NOT have a drop flag (borrow propagated through while-let):\n%s", body)
	}
	if strings.Contains(body, "%first.dropflag") {
		t.Errorf("T0485/T0512: while-let binding `first` must NOT have a drop flag (opt_opt is match-borrowed):\n%s", body)
	}
	if strings.Contains(body, "call void @_Row.drop") {
		t.Errorf("T0512: __user._drivew must NOT drop the inner Row — the synth enum drop owns it:\n%s", body)
	}
	if !strings.Contains(body, "call void @_W.drop") {
		t.Errorf("T0512: expected the synth enum drop @_W.drop to be called:\n%s", body)
	}
}

// TestT0512_OwnedNestedUnwrapStillRegistersDrop — over-application guard. A
// nested if-let on an OWNED Optional<Optional<HeapUser>> local (no match
// destructure → not in matchBorrowedIdents) must still register the inner
// drop. The T0512 propagation is gated on the source ident being match-borrowed;
// it must NOT fire for ordinary owned locals (which would leak the inner Row).
// Mirrors t0585_test.go's NonIdentRhs/NoPropagate guards.
func TestT0512_OwnedNestedUnwrapStillRegistersDrop(t *testing.T) {
	ir := generateIR(t, `
		type _Row { string name; }
		_owned() {
			_Row? a = _Row(name: "x".to_string());
			_Row?? b = a;
			if first := b {
				if r := first {
					print_line(r.name);
				}
			}
		}
		main() { _owned(); }
	`)
	body := extractFunction(ir, "__user._owned")
	if body == "" {
		t.Fatalf("expected __user._owned in IR")
	}
	// No match destructure here — `b` is an owned local. The inner if-let must
	// still register a drop binding for the unwrapped Row (propagation gated
	// on matchBorrowedIdents → does not fire). Without the drop, `r` leaks.
	if !strings.Contains(body, "%r.dropflag") {
		t.Errorf("T0512 guard: owned-local nested if-let must still register `%%r.dropflag` (propagation must not over-apply):\n%s", body)
	}
	if !strings.Contains(body, "call void @_Row.drop") {
		t.Errorf("T0512 guard: owned-local nested if-let must still drop the inner Row:\n%s", body)
	}
}
