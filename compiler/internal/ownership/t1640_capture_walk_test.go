package ownership

import "testing"

// T1640 — the go-block capture WALK is now load-bearing for soundness.
//
// Before T1640, `checkGoBlockCaptures` (sema/sendable.go) only produced
// diagnostics: an arm it failed to visit merely let a `not_sendable` value slip
// across the boundary. Now the same walk also populates `Info.GoCaptures`, which
// is the ONLY record ownership has of what a `go { … }` block captured — a
// GoExpr carries no capture list. R4 (the capture MOVES the closure) and R5
// (only an owning binding may cross) both key off it, while codegen's
// `collectBlockIdents` walks the block independently and transfers the env
// regardless.
//
// So a syntactic shape the sema walk misses but the codegen walk visits is a
// silent use-after-free: codegen moves the heap env into the coroutine frame and
// clears the outer drop flag, and ownership — never told about the capture —
// happily allows the defining scope to keep calling the closure against memory
// the goroutine now owns.
//
// These tests pin one shape per arm using the R4 signal: if the capture was
// recorded, the post-spawn call is `use of moved variable 'f'`; if the arm was
// missed, the program compiles clean and miscompiles at runtime.

// goCaptureWalkCase is a single syntactic shape in which the closure `f` is
// referenced ONLY through the arm under test.
type goCaptureWalkCase struct {
	name  string
	decls string // extra top-level declarations
	body  string // statements inside the `go { … }` block
}

// buildGoCaptureWalkSrc wraps a case body in a spawn-then-use program. `f` is
// referenced exactly once outside the block, AFTER the spawn, so the expected
// diagnostic can only come from R4 having marked the capture Moved.
func buildGoCaptureWalkSrc(tc goCaptureWalkCase) string {
	return tc.decls + `
		test() {
			done := channel[int](1);
			int base = 1;
			f := |int x| -> x + base;
			go {
` + tc.body + `
			};
			print_line("{f(2)}");
		}
	`
}

func TestT1640GoBlockCaptureWalkArms(t *testing.T) {
	cases := []goCaptureWalkCase{
		{
			// walkStmt: *ast.ClassicForStmt — the closure is reached only through
			// the loop condition, so InitValue/Cond/UpdateValue must all be walked.
			name: "classic_for_condition",
			body: `
				for int i = 0; f(i) < 5; i = i + 1 { }
				done.send(1);
			`,
		},
		{
			// walkStmt: *ast.WhileUnwrapStmt — `while v := opt { … }`. The closure
			// is in the loop body, reachable only via walkBlock(st.Body).
			name: "while_unwrap_body",
			body: `
				int? o = 1;
				while v := o {
					done.send(f(v));
					o = none;
				}
			`,
		},
		{
			// walkStmt: *ast.SelectStmt, receive-case body.
			name: "select_case_body",
			body: `
				select {
					v := <-done:
						done.send(f(v!));
				}
			`,
		},
		{
			// walkStmt: *ast.SelectStmt, send-case VALUE expression.
			name: "select_send_value",
			body: `
				select {
					done.send(f(1)):
						print_line("sent");
				}
			`,
		},
		{
			// walkStmt: *ast.SelectStmt, `default:` arm — a separate loop from the
			// case bodies, so it needs its own coverage.
			name: "select_default_body",
			body: `
				select {
					v := <-done:
						print_line("{v}");
					default:
						done.send(f(1));
				}
			`,
		},
		{
			// walkExpr: *ast.ArrayRepeatLit — `[value; count]`.
			name: "array_repeat_literal",
			body: `
				int[3] a = [f(1); 3];
				done.send(a[0]);
			`,
		},
		{
			// walkExpr: *ast.MapLit — a map literal VALUE. This is the dispatch-
			// table shape a subscription API builds inside the goroutine.
			name: "map_literal_value",
			body: `
				map[string, int] m = {"k": f(1)};
				done.send(m["k"]!);
			`,
		},
		{
			// walkExpr: *ast.MapLit — a map literal KEY, walked separately from the
			// value.
			name: "map_literal_key",
			body: `
				map[int, string] m = {f(1): "v"};
				done.send(m.len);
			`,
		},
		{
			// walkExpr: *ast.IsExpr — the closure result feeds the subject of an
			// `is` test.
			name: "is_expression_operand",
			decls: `
				type Base { int v; }
				type Derived is Base { }
				pick(int n) Base {
					if (n > 0) { return Derived(v: n); }
					return Base(v: n);
				}
			`,
			body: `
				if (pick(f(1)) is Derived) { done.send(1); } else { done.send(0); }
			`,
		},
		{
			// walkStmt: *ast.DestructureVarDecl — `(a, b) := expr;`.
			name: "destructure_var_decl",
			decls: `
				split(int n) (int, int) { return (n, n + 1); }
			`,
			body: `
				(a, b) := split(f(1));
				done.send(a + b);
			`,
		},
		{
			// walkStmt: *ast.UseVarDecl — `use r := expr;`. The `use` binding's
			// initializer is the only place the closure appears.
			name: "use_var_decl_value",
			decls: `
				type Res {
					int v;
					close(~this) { }
				}
			`,
			body: `
				use r := Res(v: f(1));
				done.send(r.v);
			`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ownerErrs(t, buildGoCaptureWalkSrc(tc))
			expectOwnerError(t, errs, "use of moved variable 'f'")
		})
	}
}

// --- T1658: arms codegen walks that the sema walk used to miss ---
//
// Each of these three shapes is visited by codegen's `collectBlockIdents` — the
// heap env IS moved into the coroutine frame and the outer drop flag IS cleared
// — but `checkGoBlockCaptures` did not visit them, so ownership never learned of
// the capture and R4 never marked it Moved. The program compiled clean and the
// post-spawn call read a freed env (`f(2)` evaluated to garbage rather than 12,
// because `base` came from the freed env). T1658 added the three mirroring arms.

// T1658 — `walkStmt`'s `*ast.Block` arm: a bare nested block inside a
// `go { … }` block must not hide the captures in it.
func TestT1640GoBlockCaptureWalkBareBlock(t *testing.T) {
	errs := ownerErrs(t, buildGoCaptureWalkSrc(goCaptureWalkCase{
		body: `
			{
				done.send(f(1));
			}
		`,
	}))
	expectOwnerError(t, errs, "use of moved variable 'f'")
}

// T1658 — the `*ast.MatchExpr` arm must walk `arm.Guard` as well as `arm.Body`
// and `arm.Block`. The closure appears ONLY in the guard here; referencing it
// from an arm body as well would mask a regression.
func TestT1640GoBlockCaptureWalkMatchArmGuard(t *testing.T) {
	errs := ownerErrs(t, buildGoCaptureWalkSrc(goCaptureWalkCase{
		body: `
			int r = match 1 {
				1 if f(1) > 5 => 100,
				_ => 0,
			};
			done.send(r);
		`,
	}))
	expectOwnerError(t, errs, "use of moved variable 'f'")
}

// --- T1698: the arm PATTERN, missed by BOTH walkers ---

// `match true { <expr> => … }` puts an arbitrary expression in pattern position.
// Neither walker visited it, so codegen left the name out of the coroutine arg
// pack and panicked `undefined variable` instead of transferring the env; this
// pins the ownership half — with the pattern walked, R4 marks the capture Moved
// exactly as it does for the guard.
func TestT1698GoBlockCaptureWalkMatchArmPattern(t *testing.T) {
	cases := []goCaptureWalkCase{
		{
			// *ast.ExpressionMatchPattern — the multi-way dispatch form.
			name: "expression_pattern",
			body: `
				int r = match true {
					f(1) > 1 => 100,
					_ => 0,
				};
				done.send(r);
			`,
		},
		{
			// *ast.LiteralMatchPattern — a string literal pattern carries
			// interpolation parts, so it too can reach an outer binding. The
			// subject `s` is block-local, so `f` is reached ONLY through the
			// pattern.
			name: "literal_pattern_interpolation",
			body: `
				string s = "11";
				int r = match s {
					"{f(1)}" => 100,
					_ => 0,
				};
				done.send(r);
			`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ownerErrs(t, buildGoCaptureWalkSrc(tc))
			expectOwnerError(t, errs, "use of moved variable 'f'")
		})
	}
}

// --- T1658: the `else` chain, which the same fix re-routed ---
//
// T1658 deleted the `walkElse` helper that used to dispatch `IfStmt.Else`. An
// `else { … }` block now reaches the walk ONLY through the newly added
// `walkStmt` `*ast.Block` arm, and an `else if` chain only through `walkStmt`
// recursing on itself. Nothing else pins that: the pre-existing `is_expression`
// case has an `else` block but reaches the closure through the `if` SUBJECT, so
// dropping the `*ast.Block` arm again would silently un-walk every `else` body
// as well as every bare block. These reach the closure through the `else` side
// and nowhere else.
func TestT1658GoBlockCaptureWalkElseArms(t *testing.T) {
	cases := []goCaptureWalkCase{
		{
			// `else { … }` — an *ast.Block reached via IfStmt.Else.
			name: "else_block",
			body: `
				if 1 > 2 {
					done.send(0);
				} else {
					done.send(f(1));
				}
			`,
		},
		{
			// `else if <cond>` — IfStmt.Else is an *ast.IfStmt, so walkStmt must
			// recurse into itself and evaluate the chained condition.
			name: "else_if_condition",
			body: `
				if 1 > 2 {
					done.send(0);
				} else if f(1) > 0 {
					done.send(1);
				} else {
					done.send(2);
				}
			`,
		},
		{
			// The tail `else` of an `else if` chain — two levels down, so both the
			// self-recursion and the *ast.Block arm have to hold at once.
			name: "else_if_tail_else",
			body: `
				if 1 > 2 {
					done.send(0);
				} else if 2 > 3 {
					done.send(1);
				} else {
					done.send(f(1));
				}
			`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ownerErrs(t, buildGoCaptureWalkSrc(tc))
			expectOwnerError(t, errs, "use of moved variable 'f'")
		})
	}
}

// A bare block nested inside another bare block, and a bare block inside a loop
// body. The `*ast.Block` arm is recursive, and a single flat case would still
// pass if it were written non-recursively (`walkBlock` on the immediate stmts
// only); these fail in that case.
func TestT1658GoBlockCaptureWalkNestedBareBlocks(t *testing.T) {
	cases := []goCaptureWalkCase{
		{
			name: "block_in_block",
			body: `
				{
					{
						done.send(f(1));
					}
				}
			`,
		},
		{
			name: "block_in_loop_body",
			body: `
				for int i = 0; i < 1; i = i + 1 {
					{
						done.send(f(1));
					}
				}
			`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ownerErrs(t, buildGoCaptureWalkSrc(tc))
			expectOwnerError(t, errs, "use of moved variable 'f'")
		})
	}
}

// The arms added by T1658/T1698 feed R5 as well as R4 — the two rules read the
// same `Info.GoCaptures` entry, so an unwalked arm silently disabled BOTH. The
// tests above pin R4 (the capture is Moved); these pin R5 (a BORROWED closure
// reaching the goroutine only through one of the new shapes is rejected rather
// than handed to a coroutine that will outlive the caller's env).
func TestT1658GoBlockCaptureWalkNewArmsFeedR5(t *testing.T) {
	cases := []struct{ name, body string }{
		{"bare_block", `
			{
				done.send(f(1));
			}
		`},
		{"match_arm_guard", `
			int r = match 1 {
				1 if f(1) > 0 => 7,
				_ => 0,
			};
			done.send(r);
		`},
		{"else_block", `
			if 1 > 2 {
				done.send(0);
			} else {
				done.send(f(1));
			}
		`},
		{"match_arm_pattern", `
			int r = match true {
				f(1) > 0 => 7,
				_ => 0,
			};
			done.send(r);
		`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ownerErrs(t, `
				spawn((int) -> int f, channel[int] done) {
					go {
`+tc.body+`
					};
				}
			`)
			expectOwnerError(t, errs, "it is a borrowed parameter, not an owned value")
		})
	}
}
