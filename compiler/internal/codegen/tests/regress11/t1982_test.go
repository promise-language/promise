package regress11

import (
	"regexp"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1982: codegen keys a variable's drop flag and drop binding by its bare NAME
// (c.dropFlags / c.dropBindings). Those entries used to outlive the construct that
// declared the name, so a later, independent variable of the same name found the
// dead one's entry: maybeRegisterStructuralFree/ParamFree's "already registered"
// guard then registered NO drop for it (the structural view box it owns leaked),
// and the name-keyed reassignment path dropped the dead variable's type instead
// (invalid IR). Every naming scope now restores the maps on exit
// (saveDropNames/restoreDropNames), and a pattern binding — which may shadow an
// outer local — clears its names for the arm.

const t1982Prelude = `
	type D ` + "`structural" + ` { get rd int ` + "`abstract" + `; }
	type C { int n; get rd int => this.n * 2; }
	mk(int n) D { return C(n: n); }
	enum S { ok(D d), missing }
`

// dFlagAllocaRe matches the entry-block drop-flag alloca of a binding named `d`
// (uniqueLocalName suffixes the second and later ones: %d.dropflag.1, ...).
var dFlagAllocaRe = regexp.MustCompile(`(?m)^\s*%d\.dropflag(\.\d+)? = alloca i1$`)

func t1982CountDFlags(t *testing.T, fn string) int {
	t.Helper()
	return len(dFlagAllocaRe.FindAllString(fn, -1))
}

// Two matches binding the same name each own a clone of the view box, so each gets
// its own drop flag. Before the fix the second match reused the first's stale
// entry and got none → one leaked box per extra match.
func TestT1982RepeatedMatchBindingNameGetsItsOwnDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1982Prelude+`
		same_name() int {
			s := S.ok(d: mk(n: 1));
			total := 0;
			match s { ok(d) => { total = total + d.rd; }, missing => { total = 0; }, }
			match s { ok(d) => { total = total + d.rd; }, missing => { total = 0; }, }
			return total;
		}
		main() { }
	`)
	fn := codegentest.ExtractDefine(ir, "__user.same_name")
	if fn == "" {
		t.Fatalf("T1982: could not find @__user.same_name in IR:\n%s", ir)
	}
	if got := t1982CountDFlags(t, fn); got != 2 {
		t.Fatalf("T1982: expected one drop flag per `ok(d)` binding (2), got %d:\n%s", got, fn)
	}
}

// A pattern binding that shadows an outer local of the same name must register its
// own drop rather than mistake the outer local's entry for its own.
func TestT1982MatchBindingShadowingOuterLocalGetsItsOwnDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1982Prelude+`
		shadowed() int {
			d := mk(n: 5);
			s := S.ok(d: mk(n: 1));
			total := 0;
			match s { ok(d) => { total = d.rd; }, missing => { total = 0; }, }
			return total + d.rd;
		}
		main() { }
	`)
	fn := codegentest.ExtractDefine(ir, "__user.shadowed")
	if fn == "" {
		t.Fatalf("T1982: could not find @__user.shadowed in IR:\n%s", ir)
	}
	if got := t1982CountDFlags(t, fn); got != 2 {
		t.Fatalf("T1982: expected drop flags for the outer local and the arm binding (2), got %d:\n%s", got, fn)
	}
}

// Inside a goroutine, a select without `default` generates each case body twice
// (ready path and wake-after-parking path). The second copy of a body-local
// declaration must get its own drop, not the first copy's leftover entry.
func TestT1982SelectCaseBodyCopiesEachGetADrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1982Prelude+`
		main() {
			ch := channel[int](1);
			done := channel[int](1);
			go {
				select {
					v := <-ch:
						d := mk(n: v!);
						done.send(d.rd);
				}
			};
			ch.send(5);
			int got = (<-done)!;
		}
	`)
	coro := codegentest.ExtractGoroutineCoro(t, ir)
	if got := t1982CountDFlags(t, coro); got != 2 {
		t.Fatalf("T1982: expected a drop flag for each generated copy of the case body (2), got %d:\n%s", got, coro)
	}
}

// A block's string local must not leave its drop binding behind for a later `int`
// of the same name: reassigning the int used to emit the string drop-old path over
// an i64 slot — `icmp eq i8* %x, <int>`, which opt rejects.
func TestT1982ReassignAfterBlockStringIsNotAStringDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		reuse() int {
			{
				s := "a" + "b";
				print_line(s);
			}
			int s = 1;
			s = 2;
			return s;
		}
		main() { }
	`)
	fn := codegentest.ExtractDefine(ir, "__user.reuse")
	if fn == "" {
		t.Fatalf("T1982: could not find @__user.reuse in IR:\n%s", ir)
	}
	codegentest.AssertNotContainsMatch(t, fn, `icmp eq i8\* %[\w.]+, \d+\b`)
}

// The same stale-string-binding shape for every other construct that declares a
// name only for itself: a block's destructure, an if-expression branch block, an
// indexed vector for-in, and a map for-in's key and value. Reassigning a later
// `int` of the same name must not reach the dead string's drop-old path.
func TestT1982ReassignAfterScopedNameIsNotADrop(t *testing.T) {
	cases := []struct{ name, body string }{
		{"block_destructure", `
			{
				(s, n) := pair();
				print_line(s);
			}
			int s = 1;
			s = 2;
			return s;`},
		{"if_expression_block", `
			c := true;
			r := if c { s := "a" + "b"; s.len } else { 0 };
			int s = 1;
			s = 2;
			return r + s;`},
		{"indexed_vector_for_in", `
			total := 0;
			v := ["a" + "b"];
			for i, s in v { total = total + i + s.len; }
			int s = 1;
			s = 2;
			return total + s;`},
		{"map_for_in_key_and_value", `
			m := Map[string, string]();
			m["a" + "b"] = "c" + "d";
			total := 0;
			for k, v in m { total = total + k.len + v.len; }
			int k = 1;
			k = 2;
			int v = 3;
			v = 4;
			return total + k + v;`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ir := codegentest.GenerateIR(t, `
				pair() (string, int) { return ("a" + "b", 1); }
				reuse() int {`+tc.body+`
				}
				main() { }
			`)
			fn := codegentest.ExtractDefine(ir, "__user.reuse")
			if fn == "" {
				t.Fatalf("T1982: could not find @__user.reuse in IR:\n%s", ir)
			}
			codegentest.AssertNotContainsMatch(t, fn, `icmp eq i8\* %[\w.]+, \d+\b`)
		})
	}
}

// A typed error handler's binding must not leave its drop binding behind either: the
// reassignment of a later `int e` used to emit the dead handler binding's drop-old —
// a second @Fault.drop over the error's (by then freed) slot, which crashed at
// runtime. Exactly one drop remains: the handler's own scope exit.
func TestT1982ReassignAfterTypedHandlerBindingIsNotAnErrorDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Fault is error { }
		fail!(int n) int { if n > 0 { raise Fault(message: "f"); } return n; }
		reuse() int {
			a := fail(1)? e is Fault { 0 } else e { 1 };
			int e = 5;
			e = 6;
			return a + e;
		}
		main() { }
	`)
	fn := codegentest.ExtractDefine(ir, "__user.reuse")
	if fn == "" {
		t.Fatalf("T1982: could not find @__user.reuse in IR:\n%s", ir)
	}
	if got := strings.Count(fn, "call void @Fault.drop"); got != 1 {
		t.Fatalf("T1982: expected only the handler's own @Fault.drop (1), got %d:\n%s", got, fn)
	}
}

// A `use` binding registers only a close — no drop flag or binding — so retiring its
// name at block exit must leave nothing behind that a later structural local of the
// same name could mistake for its own registration.
func TestT1982StructuralLocalAfterUseBlockGetsItsOwnDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, t1982Prelude+`
		type Conn { int fd; close(~this) { } }
		after_use() int {
			{
				use d := Conn(fd: 1);
				print_line(d.fd.to_string());
			}
			d := mk(n: 2);
			return d.rd;
		}
		main() { }
	`)
	fn := codegentest.ExtractDefine(ir, "__user.after_use")
	if fn == "" {
		t.Fatalf("T1982: could not find @__user.after_use in IR:\n%s", ir)
	}
	codegentest.AssertContains(t, fn, "@Conn.close")
	if got := t1982CountDFlags(t, fn); got != 1 {
		t.Fatalf("T1982: expected the structural local `d` to get its own drop flag (1), got %d:\n%s", got, fn)
	}
}

// The other naming scopes whose binding can be a structural box, each followed by
// a reuse of the name: every `d` owns a box and gets its own drop flag. Before the
// fix only the first `d` in each function got one.
func TestT1982StructuralNameReuseAcrossScopesGetsEachADrop(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		flags int
	}{
		{"is_destructure_twice", `
			s := S.ok(d: mk(n: 1));
			total := 0;
			if s is ok(d) { total = total + d.rd; }
			if s is ok(d) { total = total + d.rd; }
			return total;`, 2},
		{"classic_for_init_then_local", `
			count := 0;
			for d := mk(n: 1); count < 2; count = count + 1 { print_line(d.rd.to_string()); }
			d := mk(n: 3);
			return d.rd;`, 2},
		{"select_case_default_then_local", `
			a := channel[int](1);
			total := 0;
			select {
				v := <-a:
					d := mk(n: v!);
					total = d.rd;
				default:
					d := mk(n: 2);
					total = d.rd;
			}
			d := mk(n: 3);
			return total + d.rd;`, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ir := codegentest.GenerateIR(t, t1982Prelude+`
				reuse() int {`+tc.body+`
				}
				main() { }
			`)
			fn := codegentest.ExtractDefine(ir, "__user.reuse")
			if fn == "" {
				t.Fatalf("T1982: could not find @__user.reuse in IR:\n%s", ir)
			}
			if got := t1982CountDFlags(t, fn); got != tc.flags {
				t.Fatalf("T1982: expected a drop flag per `d` (%d), got %d:\n%s", tc.flags, got, fn)
			}
		})
	}
}
