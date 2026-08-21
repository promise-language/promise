package sema

import "testing"

// T1640 — arm-by-arm coverage of `checkGoBlockCaptures`'s block walk.
//
// The walk has two jobs and one traversal: it gates non-sendable captures AND
// records `Info.GoCaptures`, which is the only record ownership has of what a
// `go { … }` block captured (a GoExpr carries no capture list). An arm the walk
// fails to visit therefore fails BOTH jobs at once — a `not_sendable` value
// crosses the boundary, and ownership's R4/R5 closure env rules never run on the
// capture, which codegen's independent walk transfers regardless.
//
// These pin the arms with no other coverage, using the sendability diagnostic as
// the signal that `record` was reached. The ownership-side consequence (R4 marks
// the capture Moved) is pinned in parallel by
// `TestT1640GoBlockCaptureWalkArms` in compiler/internal/ownership.

// goWalkArmSrc wraps a body in a program whose only cross-scope reference is the
// `not_sendable` handle `h`, reachable only through the arm under test.
func goWalkArmSrc(decls, body string) string {
	return "type Handle `not_sendable" + ` {
			int fd;
		}
		` + decls + `
		main() {
			done := channel[int](1);
			h := Handle(fd: 3);
			go {
` + body + `
			};
		}
	`
}

func TestT1640GoBlockWalkArmsRejectNonSendable(t *testing.T) {
	cases := []struct{ name, decls, body string }{
		// walkStmt: *ast.ClassicForStmt — init/cond/update/body.
		{name: "classic_for_cond", body: `
			for int i = 0; i < h.fd; i = i + 1 { }
			done.send(1);
		`},
		{name: "classic_for_init", body: `
			for int i = h.fd; i < 5; i = i + 1 { }
			done.send(1);
		`},
		{name: "classic_for_body", body: `
			for int i = 0; i < 2; i = i + 1 { done.send(h.fd); }
		`},
		// walkStmt: *ast.WhileUnwrapStmt — `while v := opt { … }`.
		{name: "while_unwrap_value", body: `
			int? o = h.fd;
			while v := o { done.send(v); o = none; }
		`},
		{name: "while_unwrap_body", body: `
			int? o = 1;
			while v := o { done.send(h.fd); o = none; }
		`},
		// walkStmt: *ast.SelectStmt — each of its three sub-walks.
		{name: "select_case_body", body: `
			select {
				v := <-done:
					print_line("{v!} {h.fd}");
			}
		`},
		{name: "select_send_value", body: `
			select {
				done.send(h.fd):
					print_line("sent");
			}
		`},
		{name: "select_default_body", body: `
			select {
				v := <-done:
					print_line("{v!}");
				default:
					done.send(h.fd);
			}
		`},
		// walkExpr: *ast.ArrayRepeatLit — `[value; count]`, value and count.
		{name: "array_repeat_value", body: `
			int[3] a = [h.fd; 3];
			done.send(a[0]);
		`},
		// walkExpr: *ast.MapLit — key and value are walked separately.
		{name: "map_literal_value", body: `
			map[string, int] m = {"k": h.fd};
			done.send(m["k"]!);
		`},
		{name: "map_literal_key", body: `
			map[int, string] m = {h.fd: "v"};
			done.send(m.len);
		`},
		// walkExpr: *ast.IsExpr — the subject of an `is` test.
		{name: "is_expression_operand", decls: `
			type Base { int v; }
			type Derived is Base { }
			pick(int n) Base {
				if (n > 0) { return Derived(v: n); }
				return Base(v: n);
			}
		`, body: `
			if (pick(h.fd) is Derived) { done.send(1); } else { done.send(0); }
		`},
		// walkStmt: *ast.DestructureVarDecl.
		{name: "destructure_var_decl", decls: `
			split(int n) (int, int) { return (n, n + 1); }
		`, body: `
			(a, b) := split(h.fd);
			done.send(a + b);
		`},
		// walkStmt: *ast.UseVarDecl.
		{name: "use_var_decl_value", decls: `
			type Res {
				int v;
				close(~this) { }
			}
		`, body: `
			use r := Res(v: h.fd);
			done.send(r.v);
		`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := checkErrs(t, goWalkArmSrc(tc.decls, tc.body))
			expectError(t, errs, "cannot send non-sendable variable 'h'")
		})
	}
}

// T1658 — three shapes codegen's `collectBlockIdents` walks but this walk does
// NOT, so a capture inside one of them is invisible to both the sendability gate
// and ownership's env rules while codegen still transfers the env. Skipped
// because the assertion is correct and the compiler is not; drop the `t.Skip`
// when T1658 lands. Full repros and the fix sketch are on T1658; the
// use-after-free consequence is pinned in
// compiler/internal/ownership/t1640_capture_walk_test.go.
func TestT1640GoBlockWalkT1658MissingArms(t *testing.T) {
	t.Skip("T1658: bare block / match-arm guard / unsafe block are not walked by checkGoBlockCaptures")
	for _, tc := range []struct{ name, body string }{
		{"bare_nested_block", `{ done.send(h.fd); }`},
		{"match_arm_guard", "int r = match 1 { 1 if h.fd > 0 => 7, _ => 0, }; done.send(r);"},
		{"unsafe_block", `unsafe { done.send(h.fd); }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := checkErrs(t, goWalkArmSrc("", tc.body))
			expectError(t, errs, "cannot send non-sendable variable 'h'")
		})
	}
}

// A `sharable`-annotated type may NOT hold a closure field: R1 made a Signature
// sendable but deliberately left it non-SHARABLE, because `&`-sharing a closure
// hands two goroutines the same heap env with no refcount. The `sendable` twin
// of this declaration is accepted (TestSignatureIsSendable), so the two
// annotations must be shown to diverge here — otherwise a later "closures are
// sendable, so relax the sibling check too" edit passes unnoticed.
func TestT1640SharableTypeWithClosureFieldRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Callback `+"`sharable"+` {
			() -> void fn;
		}
	`)
	expectError(t, errs, "has non-sharable type")
}

// Weak[T] carries the same sendable+sharable pair as Ref[T], and
// validateSendableInstance checks it on its own branch. A closure argument must
// fail the SHARABLE half there while passing the sendable half — the exact
// asymmetry R1 introduced, on an instantiation path that actually enforces it.
func TestT1640WeakOfClosureStillNotSharable(t *testing.T) {
	errs := checkErrs(t, `
		main() {
			w := Weak[(int) -> int](none);
		}
	`)
	expectError(t, errs, "not sharable")
}

// The type-ARGUMENT of a user `sendable` generic is not revalidated at
// instantiation — validateSendableInstance only covers Channel/Ref/Weak — so
// `Box[Handle]` with a `not_sendable` Handle is declared without complaint.
// That is sound for BOUNDARIES because the boundary check derives sendability
// from the substituted field types rather than trusting the annotation, and this
// test pins that backstop: the error must appear at the `go`, and must name the
// instantiated type. (The capture-site analogue has no such backstop; that
// residual is T1652.)
func TestT1640SendableGenericWithNonSendableArgCaughtAtBoundary(t *testing.T) {
	errs := checkErrs(t, `
		type Handle `+"`not_sendable"+` {
			int fd;
		}
		type Box[T] `+"`sendable"+` {
			T value;
		}
		run(Box[Handle] move b, channel[int] done) {
			done.send(b.value.fd);
		}
		main() {
			done := channel[int](1);
			b := Box[Handle](value: Handle(fd: 3));
			go run(move b, done);
		}
	`)
	expectError(t, errs, "cannot send non-sendable argument of type Box[Handle]")
}

// A `sendable` ENUM may carry a closure payload — validateSendableEnumDecl runs
// the same isSendableType, so R1 must reach the enum-variant path too. The
// pre-T1640 behaviour was a hard error, which would have blocked modelling a
// subscription's handler as an enum variant.
func TestT1640SendableEnumWithClosureVariantAccepted(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		enum Event `+"`sendable"+` {
			idle,
			handler((int) -> int fn),
		}
	`))
}

// A closure nested INSIDE a sendable field's type is reached by the recursive
// walk: `Vector[(int) -> int]` derives its sendability from the element, which
// R1 now accepts. This is the registry-field shape.
func TestT1640SendableTypeWithClosureVectorFieldAccepted(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		type Registry `+"`sendable"+` {
			((int) -> int)[] handlers;
		}
	`))
}

// R1 must not become a blanket "anything with a function in it is sendable":
// a closure-typed field is fine, but a `not_sendable` sibling field still
// poisons the type. Guards against the annotation check being loosened wholesale.
func TestT1640SendableTypeStillRejectsNonSendableSiblingField(t *testing.T) {
	errs := checkErrs(t, `
		type Handle `+"`not_sendable"+` {
			int fd;
		}
		type Registry `+"`sendable"+` {
			(int) -> int handler;
			Handle h;
		}
	`)
	expectError(t, errs, "non-sendable")
}

// A Channel whose element is a `not_sendable` type is still refused, even though
// the sibling Channel-of-closures is now allowed. The element check is the same
// isSendableType call, so this pins that R1 widened it by exactly one case.
func TestT1640ChannelOfNonSendableStillRejected(t *testing.T) {
	errs := checkErrs(t, `
		type Handle `+"`not_sendable"+` {
			int fd;
		}
		main() {
			ch := channel[Handle](1);
		}
	`)
	expectError(t, errs, "not sendable")
}

// scopeContains' nil-scope guard: a `go` block whose body scope was never
// recorded must return without walking rather than panicking. Reached here
// through the ordinary path — the guard is asserted by the absence of a crash
// on a `go` block with an empty body, whose scope is still registered but has a
// degenerate start==end range.
func TestT1640EmptyGoBlockRecordsNoCaptures(t *testing.T) {
	expectNoErrors(t, checkErrs(t, `
		main() {
			int base = 1;
			go { };
			print_line("{base}");
		}
	`))
}
