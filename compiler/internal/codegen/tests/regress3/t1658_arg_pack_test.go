package regress3

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1658 / T1698 — the coroutine ARG PACK for the AST shapes the two go-block
// walkers were fixed to agree on.
//
// gowalk_parity_test.go compares the two walkers' `case *ast.X` arm SETS, which
// is deliberately structural: it cannot see what an arm does with its children.
// T1698 was exactly that blind spot — both walkers had `*ast.MatchExpr`, neither
// descended into `arm.Pattern`, and a local named only by a
// `match true { <expr> => … }` pattern never entered `collectBlockIdents`'s
// result. The coroutine then had no parameter for it and codegen died with
// `undefined variable`.
//
// These pin the behaviour the parity guard cannot: for each shape, the outer
// local reached ONLY through it becomes a captured coroutine parameter and the
// caller loads and passes the real value. Runtime correctness and zero-leak for
// the same shapes is enforced by the `capture_through_*` batch tests in
// tests/concurrency/closure_send_test.pr.
func TestT1658GoBlockArgPackCoversWalkedShapes(t *testing.T) {
	cases := []struct{ name, body string }{
		{
			// T1658 — walkStmt's `*ast.Block` arm.
			name: "bare_nested_block",
			body: `{ done.send(base); }`,
		},
		{
			// T1658 — the same arm, two levels deep: it must recurse.
			name: "nested_bare_blocks",
			body: `{ { done.send(base); } }`,
		},
		{
			// T1658 — `IfStmt.Else` as an `*ast.Block`, which since the walkElse
			// helper was deleted routes through the bare-block arm.
			name: "else_block",
			body: `if 1 > 2 { done.send(0); } else { done.send(base); }`,
		},
		{
			// T1658 — `IfStmt.Else` as an `*ast.IfStmt`: walkStmt recursing on
			// itself, then evaluating the chained condition.
			name: "else_if_condition",
			body: `if 1 > 2 { done.send(0); } else if base > 1 { done.send(1); } else { done.send(2); }`,
		},
		{
			// T1658 — `ast.MatchArm.Guard`. `base` appears in no arm body, so an
			// arm-body walk alone cannot reach it.
			name: "match_arm_guard",
			body: `int r = match 1 { 1 if base > 1 => 7, _ => 0, }; done.send(r);`,
		},
		{
			// T1698 — `*ast.ExpressionMatchPattern`, the `match true { <expr> => … }`
			// multi-way dispatch form. This is the case the arm-set parity guard is
			// structurally blind to.
			name: "match_arm_expression_pattern",
			body: `int r = match true { base > 1 => 7, _ => 0, }; done.send(r);`,
		},
		{
			// T1698 — `*ast.LiteralMatchPattern`. A STRING literal pattern carries
			// interpolation parts, so the pattern itself reaches an outer local. The
			// subject `s` is block-local, so `base` is reached only through it.
			name: "match_arm_literal_pattern",
			body: `string s = "40"; int r = match s { "{base}" => 7, _ => 0, }; done.send(r);`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ir := codegentest.GenerateIR(t, `
				spawn(channel[int] done) {
					base := 40;
					go {
`+tc.body+`
					};
				}
				main() { }
			`)

			coro := codegentest.FindDefinedFunc(ir, "@.goroutine.0(")
			if coro == "" {
				t.Fatalf("expected go-block coroutine @.goroutine.0 to be defined")
			}
			sig := coro
			if nl := strings.Index(coro, "\n"); nl >= 0 {
				sig = coro[:nl]
			}
			if !strings.Contains(sig, "%base.cap") {
				t.Errorf("outer local `base`, reached only through this shape, must be "+
					"threaded into the go-block coroutine's arg pack — otherwise codegen "+
					"generates the body against a name with no alloca (`undefined "+
					"variable`, T1698) or a zero-initialized slot (T0740):\n%s", sig)
			}

			caller := codegentest.FindDefinedFunc(ir, "@__user.spawn(")
			if caller == "" {
				t.Fatalf("expected caller @__user.spawn to be defined")
			}
			if !strings.Contains(caller, "load i64, i64* %base") {
				t.Errorf("caller must load the real value of `base` before spawning, "+
					"proving the value (not a zero) is what reaches the coroutine:\n%s", caller)
			}
		})
	}
}

// The mirror of the above: a name BOUND inside the shape is a coroutine local,
// not a capture, and must not be threaded into the arg pack. Widening either
// walker to a pattern that binds (`match n { v => … }`) rather than references
// would make the caller try to load a local it does not have — the failure mode
// the `outerLocals` filter exists to prevent (T0740).
func TestT1698GoBlockArgPackExcludesArmBoundName(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		spawn(channel[int] done) {
			base := 40;
			go {
				int r = match base {
					v => v + 1,
				};
				done.send(r);
			};
		}
		main() { }
	`)

	coro := codegentest.FindDefinedFunc(ir, "@.goroutine.0(")
	if coro == "" {
		t.Fatalf("expected go-block coroutine @.goroutine.0 to be defined")
	}
	sig := coro
	if nl := strings.Index(coro, "\n"); nl >= 0 {
		sig = coro[:nl]
	}
	// The subject is an outer local — it IS a capture.
	if !strings.Contains(sig, "%base.cap") {
		t.Errorf("the match SUBJECT is an outer local and must be captured:\n%s", sig)
	}
	// The arm-bound name is not.
	if strings.Contains(sig, "%v.cap") {
		t.Errorf("`v` is bound by the match arm, not captured from the enclosing "+
			"scope; it must not appear in the coroutine arg pack:\n%s", sig)
	}
}
