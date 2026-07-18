package codegen

import (
	"strings"
	"testing"
)

// T1317: An inline enum-constructor temp with a droppable payload, passed BY VALUE
// as an argument into a call that is the DIRECT operand of a `return` statement
// (`return take(Payload.Full("h".to_upper()), 3);`), leaked the payload. The
// call-arg move sites clear the enum-ctor temp's drop flag and truncate
// c.enumCtorTemps for the arg they consume; the callee dups the payload (B0232),
// so the caller still owns the original temp. But the return path never reached
// the statement-end drain (cleanupStmtLevelTemps) that frees discarded-call arg
// temps, so the payload was orphaned.
//
// The fix drains the remaining enum-ctor arg temps in genReturnStmt (mirroring
// cleanupStmtLevelTemps), gated on a SYNTACTIC check: if the return expression is
// ITSELF an enum constructor (`return Payload.Full(...)`) the temp is moved out to
// the caller and its flag is cleared; otherwise (`return f(Payload.Full(...))`)
// the by-value arg temp is drained here.

// The direct-return-of-call form emits the enum-ctor drain guard so the orphaned
// by-value arg temp is dropped before the function returns.
func TestT1317ReturnCallByValueEnumCtorArgDrains(t *testing.T) {
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		take(Payload p, int y) int {
			match p {
				Payload.Full(s) => { return s.len + y; },
				Payload.Empty   => { return y; },
			}
		}
		ret_literal() int { return take(Payload.Full("h".to_upper()), 3); }
	`)
	fn := extractFunction(ir, "__user.ret_literal")
	if fn == "" {
		t.Fatalf("could not extract @__user.ret_literal from IR:\n%s", ir)
	}
	// The by-value enum-ctor arg temp is drained on the return path: a guarded
	// drop of the enum-ctor temp (same shape as cleanupStmtLevelTemps).
	if !strings.Contains(fn, "enum.ctor.drop") || !strings.Contains(fn, "enum.ctor.skip") {
		t.Fatalf("expected enum-ctor drain guard (enum.ctor.drop/skip) on the return path, got:\n%s", fn)
	}
}

// The `return E.Variant(...)` form (the ctor IS the return value) must NOT emit a
// drop guard for that temp — it is moved out to the caller. Its drop flag is
// cleared instead (mirrors the B0267 var-decl clear), so the ctor temp is not
// double-freed.
func TestT1317ReturnEnumCtorItselfMovedOut(t *testing.T) {
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		make_full() Payload { return Payload.Full("x".to_upper()); }
	`)
	fn := extractFunction(ir, "__user.make_full")
	if fn == "" {
		t.Fatalf("could not extract @__user.make_full from IR:\n%s", ir)
	}
	// No drain guard: the returned enum-ctor temp is moved out, not dropped here.
	assertNotContains(t, fn, "enum.ctor.drop")
}

// The subtle case that a value-type check would get wrong: the returned CALL
// itself returns an enum, but the enum-ctor is a by-value ARGUMENT the callee
// borrows/dups (B0232). returnValueMovesOutEnumCtor must use a SYNTACTIC check
// (the return expr is a CallExpr, not an enum constructor) and still drain the
// arg temp here — otherwise the payload leaks.
func TestT1317ReturnCallReturningEnumStillDrainsArg(t *testing.T) {
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		relay(Payload p) Payload {
			return match p {
				Payload.Full(s) => Payload.Full(s.repeat(2)),
				Payload.Empty   => Payload.Empty,
			};
		}
		ret_relay() Payload { return relay(Payload.Full("r".to_upper())); }
	`)
	fn := extractFunction(ir, "__user.ret_relay")
	if fn == "" {
		t.Fatalf("could not extract @__user.ret_relay from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "enum.ctor.drop") || !strings.Contains(fn, "enum.ctor.skip") {
		t.Fatalf("expected enum-ctor drain guard on the return path (call returns an enum but the ctor is a by-value arg), got:\n%s", fn)
	}
}

// `return match n { ... => Payload.Full(...), ... }` — the arm ctor temps ARE the
// phi'd result moved out to the caller. The MatchExpr branch of
// returnValueMovesOutEnumCtor clears their flags; no drain guard is emitted.
func TestT1317ReturnMatchEnumArmsMovedOut(t *testing.T) {
	ir := generateIR(t, `
		enum Payload { Full(string s), Empty, }
		make_via_match(int n) Payload {
			return match n {
				0 => Payload.Full("m".to_upper()),
				_ => Payload.Empty,
			};
		}
	`)
	fn := extractFunction(ir, "__user.make_via_match")
	if fn == "" {
		t.Fatalf("could not extract @__user.make_via_match from IR:\n%s", ir)
	}
	assertNotContains(t, fn, "enum.ctor.drop")
}

// Generic enum in a mono context: the by-value arg temp is drained on the return
// path. Exercises the bare-*types.Enum generic branch of isEnumConstructorExpr.
func TestT1317ReturnGenericEnumCtorArgDrains(t *testing.T) {
	ir := generateIR(t, `
		enum Opt[T] { Some(T v), None, }
		opt_len(Opt[string] o) int {
			return match o {
				Opt.Some(v) => v.len,
				Opt.None    => 0,
			};
		}
		ret_generic() int { return opt_len(Opt[string].Some("g".to_upper())); }
	`)
	fn := extractFunction(ir, "__user.ret_generic")
	if fn == "" {
		t.Fatalf("could not extract @__user.ret_generic from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "enum.ctor.drop") || !strings.Contains(fn, "enum.ctor.skip") {
		t.Fatalf("expected enum-ctor drain guard on the return path for a generic enum arg, got:\n%s", fn)
	}
}
