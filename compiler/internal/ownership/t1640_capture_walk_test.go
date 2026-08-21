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

// --- T1658: arms codegen walks but the sema walk does NOT ---
//
// Each of these three shapes is visited by codegen's `collectBlockIdents` (so
// the heap env IS moved into the coroutine frame and the outer drop flag IS
// cleared) but missed by `checkGoBlockCaptures` (so ownership never learns of
// the capture and R4 never marks it Moved). The program compiles clean and the
// post-spawn call reads a freed env: `f(2)` evaluates to 2 instead of 12,
// because `base` reads as 0.
//
// They are skipped rather than deleted because the assertion is exactly right —
// it is the compiler that is wrong. Remove the `t.Skip` line when T1658 lands;
// no other edit should be needed.

// T1658 — `walkStmt` has no `*ast.Block` arm, so a bare nested block inside a
// `go { … }` block hides every capture in it.
func TestT1640GoBlockCaptureWalkBareBlockNotWalked(t *testing.T) {
	t.Skip("T1658: checkGoBlockCaptures' walkStmt has no *ast.Block arm — capture is never recorded")
	errs := ownerErrs(t, buildGoCaptureWalkSrc(goCaptureWalkCase{
		body: `
			{
				done.send(f(1));
			}
		`,
	}))
	expectOwnerError(t, errs, "use of moved variable 'f'")
}

// T1658 — the `*ast.MatchExpr` arm walks `arm.Body` and `arm.Block` but not
// `arm.Guard`. The closure appears ONLY in the guard here; referencing it from
// an arm body as well would mask the bug.
func TestT1640GoBlockCaptureWalkMatchArmGuardNotWalked(t *testing.T) {
	t.Skip("T1658: checkGoBlockCaptures does not walk ast.MatchArm.Guard — capture is never recorded")
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

// T1658 — `walkExpr` has no `*ast.UnsafeExpr` arm, so an `unsafe { … }` body
// inside a `go { … }` block hides every capture in it.
func TestT1640GoBlockCaptureWalkUnsafeBlockNotWalked(t *testing.T) {
	t.Skip("T1658: checkGoBlockCaptures' walkExpr has no *ast.UnsafeExpr arm — capture is never recorded")
	errs := ownerErrs(t, buildGoCaptureWalkSrc(goCaptureWalkCase{
		body: `
			unsafe {
				done.send(f(1));
			}
		`,
	}))
	expectOwnerError(t, errs, "use of moved variable 'f'")
}
