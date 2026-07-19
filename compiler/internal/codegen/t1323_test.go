package codegen

import (
	"strings"
	"testing"
)

// T1323: An inline enum-constructor temp with a droppable payload, passed BY VALUE
// as an argument into a call whose RESULT type is ALSO an enum, leaked the payload
// when the call result was bound with a var-decl / assignment / container-element
// store (`q := f(E.V(heapStr))`). The statement-level enum-ctor-temp clear used a
// type-based `extractEnum(resultType) != nil` gate, which misfired on the enum call
// result and cleared the by-value ARG temp's drop flag — orphaning its payload.
//
// Part B gates the clear on enumCtorTempMovesOut (RHS is SYNTACTICALLY an enum ctor,
// or a match/if producing the enum) and otherwise lets the statement-boundary drain
// drop the orphaned arg temp. Part A deep-clones a borrowed enum param returned by
// value so that drain can't double-free (the return-path alias, shared with T1317).

// A var-decl binding of a call that RETURNS an enum, whose by-value argument is an
// inline enum ctor, drains the orphaned arg temp at the statement boundary — a
// guarded drop of the enum-ctor temp (same shape as the return-path drain).
func TestT1323VarDeclCallReturningEnumDrainsArg(t *testing.T) {
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		wrap(Payload p) Payload {
			match p {
				Payload.Full(s) => { return Payload.Full(s.to_upper()); },
				Payload.Empty   => { return Payload.Empty; },
			}
		}
		bind_it() int {
			q := wrap(Payload.Full("h".to_upper()));
			match q {
				Payload.Full(s) => { return s.len; },
				Payload.Empty   => { return 0; },
			}
		}
	`)
	fn := extractFunction(ir, "__user.bind_it")
	if fn == "" {
		t.Fatalf("could not extract @__user.bind_it from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "enum.ctor.drop") || !strings.Contains(fn, "enum.ctor.skip") {
		t.Fatalf("expected enum-ctor drain guard (enum.ctor.drop/skip) at the var-decl binding of a call returning an enum, got:\n%s", fn)
	}
}

// A var-decl whose RHS is ITSELF an enum ctor moves the temp out into the binding —
// its flag is cleared, no drain guard is emitted for it.
func TestT1323VarDeclEnumCtorItselfMovedOut(t *testing.T) {
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		make_full() int {
			q := Payload.Full("x".to_upper());
			match q {
				Payload.Full(s) => { return s.len; },
				Payload.Empty   => { return 0; },
			}
		}
	`)
	fn := extractFunction(ir, "__user.make_full")
	if fn == "" {
		t.Fatalf("could not extract @__user.make_full from IR:\n%s", ir)
	}
	assertNotContains(t, fn, "enum.ctor.drop")
}

// Part A: returning a borrowed (non-`~`) enum VALUE param by value must deep-clone
// the variant payload, so the result is independent of the caller's arg temp.
// Mirrors the `return this` enum-clone (T0906) — cloneOwnedReturnAlias routes a
// droppable enum through its synthesized clone().
func TestT1323BorrowedEnumParamReturnClones(t *testing.T) {
	ir := generateIR(t, `
		type Resource { string name; drop(~this) {} }
		enum Holder { Has(Resource r), Empty, }
		consume(Holder h) Holder { return h; }
	`)
	fn := extractFunction(ir, "__user.consume")
	if fn == "" {
		t.Fatalf("could not extract @__user.consume from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "@Holder.clone") {
		t.Fatalf("expected a deep-clone (@Holder.clone) of the borrowed enum param on return, got:\n%s", fn)
	}
}

// Negative guard: a `~`/move enum param returned by value is a genuine ownership
// transfer — the callee owns it, so no clone is emitted (cloning would leak).
func TestT1323MoveEnumParamReturnNoClone(t *testing.T) {
	ir := generateIR(t, `
		type Resource { string name; drop(~this) {} }
		enum Holder { Has(Resource r), Empty, }
		consume_move(Holder move h) Holder { return h; }
	`)
	fn := extractFunction(ir, "__user.consume_move")
	if fn == "" {
		t.Fatalf("could not extract @__user.consume_move from IR:\n%s", ir)
	}
	assertNotContains(t, fn, "@Holder.clone")
}
