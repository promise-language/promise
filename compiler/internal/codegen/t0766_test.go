package codegen

import (
	"testing"
)

// T0766: Coercing a user type that satisfies a structural interface with
// *chained* default methods (a default that calls a sibling default) into the
// interface view used to panic during view-vtable synthesis:
//
//	panic: codegen: no method write_string on type Tracked
//
// While synthesizing the concrete type's `write_line` (from the interface
// default), the body calls `this.write_string(...)` — a sibling default that is
// declared on the interface, not in the concrete type's own method table. The
// fix resolves such sibling-default calls through `c.selfSubst.iface` and
// ensures the per-concrete sibling default is synthesized.

// Self-contained reproduction with a local interface whose `emit_line` default
// calls the `emit_str` sibling default, which in turn calls the abstract `emit`.
// Reaching generateIR without a panic proves the fix; the asserts confirm both
// sibling defaults were synthesized on the concrete type.
func TestT0766LocalChainedDefaultSynth(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural {
			emit!(~this, u8[] ~buf) int `+"`"+`abstract;
			emit_str!(~this, string s) int {
				u8[] b = s.bytes();
				return this.emit(b);
			}
			emit_line!(~this, string s) {
				this.emit_str(s);
				this.emit_str(s);
			}
		}
		type Logger {
			int n;
			emit!(~this, u8[] ~buf) int { return buf.len; }
		}
		main() {
			Logger lg = Logger(n: 0);
			Sink s = lg;
			s.emit_line("hi")?!;
		}
	`)
	// Both default methods must be synthesized on the concrete type. The
	// sibling-default call (emit_line -> emit_str) is the one that used to panic.
	if extractFunction(ir, "Logger.emit_str") == "" {
		t.Error("expected synthesized Logger.emit_str to be emitted")
	}
	if extractFunction(ir, "Logger.emit_line") == "" {
		t.Error("expected synthesized Logger.emit_line to be emitted")
	}
	// The synthesized emit_line body must dispatch to the sibling default by name.
	assertContains(t, extractFunction(ir, "Logger.emit_line"), "@Logger.emit_str")
}

// Declaration-order variant: the *calling* default (emit_line) is declared
// BEFORE the sibling default it calls (emit_str). When emit_line's body is
// generated, emit_str has not been synthesized yet, so the sibling-default
// branch must synthesize it *fresh, re-entrantly*. This exercises the
// recursion-guard registration (the stub is registered in c.funcs before its
// body is generated) — distinct from the leader-declared-first cases above
// where ensureDefaultMethodsSynthesized only ever no-ops.
func TestT0766SiblingDefaultDeclaredAfter(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural {
			emit!(~this, u8[] ~buf) int `+"`"+`abstract;
			emit_line!(~this, string s) {
				this.emit_str(s);
				this.emit_str(s);
			}
			emit_str!(~this, string s) int {
				u8[] b = s.bytes();
				return this.emit(b);
			}
		}
		type Logger {
			int n;
			emit!(~this, u8[] ~buf) int { return buf.len; }
		}
		main() {
			Logger lg = Logger(n: 0);
			Sink s = lg;
			s.emit_line("hi")?!;
		}
	`)
	// The sibling default declared *after* its caller must still be synthesized.
	if extractFunction(ir, "Logger.emit_str") == "" {
		t.Error("expected re-entrantly-synthesized Logger.emit_str to be emitted")
	}
	el := extractFunction(ir, "Logger.emit_line")
	if el == "" {
		t.Fatal("expected synthesized Logger.emit_line to be emitted")
	}
	assertContains(t, el, "@Logger.emit_str")
	assertContains(t, ir, "@promise_vtable_Logger_as_Sink")
}

// The exact reported case: a user type implementing only stdlib Writer's
// abstract `write!`, coerced to a `Writer` view. Writer's `write_line` default
// calls the `write_string` default, which calls `write`.
func TestT0766StdWriterDefaultChain(t *testing.T) {
	ir := generateIR(t, `
		type Tracked {
			int total;
			write!(~this, u8[] ~buf) int {
				this.total = this.total + buf.len;
				return buf.len;
			}
		}
		main() {
			Tracked t = Tracked(total: 0);
			Writer w = t;
			w.write_line("hi")?!;
		}
	`)
	// Sibling defaults synthesized on the concrete type.
	if extractFunction(ir, "Tracked.write_string") == "" {
		t.Error("expected synthesized Tracked.write_string to be emitted")
	}
	wl := extractFunction(ir, "Tracked.write_line")
	if wl == "" {
		t.Fatal("expected synthesized Tracked.write_line to be emitted")
	}
	assertContains(t, wl, "@Tracked.write_string")
	// View-vtable synthesis must complete (the panic fired before this global
	// was emitted).
	assertContains(t, ir, "@promise_vtable_Tracked_as_Writer")
}
