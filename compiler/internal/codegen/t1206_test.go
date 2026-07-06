package codegen

import (
	"strings"
	"testing"
)

// T1206: an OUTER match arm whose body is a nested mixed-ownership conditional
// (owned-local ident + fresh clone) must NOT have the owned-local's drop flag
// cleared unconditionally in the outer merge block. Since T1107 the nested
// conditional already clears its result idents PATH-CONDITIONALLY (inside the
// branch that selects them), so clearResultDropFlags no longer recurses into a
// nested if/match. Before the fix the owned-local's drop flag was cleared twice —
// once conditionally in the inner if's then-block AND once unconditionally in the
// outer merge — orphaning it on the path where the nested if cloned instead.
func TestNestedMatchArmOwnedLocalNoOrphaningClear(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		borrow_len(string p) Holder { return Holder(n: p.len); }
		run(bool a, bool b) {
			local := "hello".clone();
			r := borrow_len(match a {
				true => if b { local } else { "world".clone() },
				_ => "zz".clone(),
			});
		}
	`)
	fn := extractFunction(ir, "__user.run")
	// Exactly one clear of local.dropflag — the conditional one in the inner if's
	// then-block where local is actually moved. The removed recursion previously
	// added a second, unconditional clear in the merge block.
	n := strings.Count(fn, "store i1 false, i1* %local.dropflag")
	if n != 1 {
		t.Fatalf("expected exactly 1 conditional clear of local.dropflag, got %d:\n%s", n, fn)
	}
	// The owned-local is still dropped at scope end (its drop path survives).
	assertContains(t, fn, "call void @promise_string_drop")
}

// T1206: a value-producing `if` in STATEMENT position (the last statement of a
// block, e.g. nested inside an outer if's then-block) is lowered by
// genIfStmtValue, which pre-T1107 never registered its owned phi as a statement
// temp — so its selected clone leaked when passed to a borrow param. It now mirrors
// genIfExpr's trackMergeResultTemp, emitting a per-path ownership flag phi.
func TestNestedIfStmtValueOwnedResultTracked(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		borrow_len(string p) Holder { return Holder(n: p.len); }
		run(bool a, bool b) {
			r := borrow_len(if a { if b { "hi".clone() } else { "world".clone() } } else { "zz".clone() });
		}
	`)
	fn := extractFunction(ir, "__user.run")
	// The nested statement-position if now carries a T1107-style ownership flag phi.
	assertContains(t, fn, "phi i1 [ true,")
	assertContains(t, fn, "call void @promise_string_drop")
}

// T1206: resolving the result type of a value-producing `if` in STATEMENT position
// must recurse through the branch shape. Here the outer match arm's block ends in a
// value-if whose THEN body's last statement is itself another value-if (no direct
// ExprStmt to read the type from) — so blockResultType must recurse via its IfStmt
// case into ifStmtValueResultType to resolve the owned-string result. Without the
// recursion trackMergeResultTemp gets a nil type, silently skips registration, and
// the selected clone leaks. Mirrors the runtime `resolve_type_recursively` case.
func TestStatementIfValueResultTypeRecursion(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		borrow_len(string p) Holder { return Holder(n: p.len); }
		run(bool a, bool b, bool c, bool d) {
			r := borrow_len(match a {
				true => {
					y := 1;
					if b {
						z := 2;
						if d { "hi".clone() } else { "world".clone() }
					} else if c {
						"zz".clone()
					} else {
						"qq".clone()
					}
				},
				_ => "other".clone(),
			});
		}
	`)
	fn := extractFunction(ir, "__user.run")
	// The nested statement-position if resolves its owned-string type and registers
	// a T1107-style ownership flag phi, so the selected clone is freed.
	assertContains(t, fn, "phi i1 [ true,")
	assertContains(t, fn, "call void @promise_string_drop")
}

// T1206: genBlockValue must propagate the statement-position if's ownership so the
// ENCLOSING conditional registers its merge phi as owned too (chain continues). An
// owned-local selected through the nested if is dropped exactly once — the drop
// path exists and there is no redundant unconditional clear.
func TestNestedIfStmtValueOwnedLocalChain(t *testing.T) {
	ir := generateIR(t, `
		type Holder { int n; }
		borrow_len(string p) Holder { return Holder(n: p.len); }
		run(bool a, bool b) {
			local := "hello".clone();
			r := borrow_len(if a { if b { local } else { "world".clone() } } else { "zz".clone() });
		}
	`)
	fn := extractFunction(ir, "__user.run")
	// local's drop flag is cleared only in the branch that actually moves it.
	n := strings.Count(fn, "store i1 false, i1* %local.dropflag")
	if n != 1 {
		t.Fatalf("expected exactly 1 conditional clear of local.dropflag, got %d:\n%s", n, fn)
	}
	assertContains(t, fn, "phi i1 [ true,")
}
