package codegen

import (
	"strings"
	"testing"
)

// T1288: Unwrapping an Optional[structural-interface] via an if-let/while-let
// binding must free the unwrapped {vtable, instance} box at scope exit. The
// unwrap-binding drop site previously treated the structural view as trivially
// droppable (maybeRegisterDrop skips structural interfaces) and registered no
// drop when the source was a fresh owned temp (a call/operator result with no
// surviving owner), so the boxed heap instance leaked.
//
// The fix registers an RTTI-dispatched structural free for the binding (routing
// the instance ptr through __promise_structural_drop's drop_fn_ptr → concrete
// drop, else pal_free), gated to fresh-owned-temp sources. An IdentExpr source
// keeps its own optional-drop owner, so the binding must NOT get a second drop
// (that would double-free) — evidenced by the absence of a binding drop flag.

// A fresh-owned-temp source (a call result) must give the unwrap binding its own
// drop flag and an RTTI-dispatched structural drop.
func TestT1288IfUnwrapStructuralTempGetsStructuralDrop(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural { to_string() string `+"`"+`abstract; }
		type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
		show(int n) Showable { return Widget(id: n); }
		mkbox(int n) Showable? { Showable s = show(n); return s; }
		unwrap_temp() { if bx := mkbox(3) { print_line(bx.to_string()); } }
	`)
	fn := extractFunction(ir, "__user.unwrap_temp")
	if fn == "" {
		t.Fatalf("could not extract @__user.unwrap_temp from IR:\n%s", ir)
	}
	// Binding drop flag registered (maybeRegisterStructuralParamFree path).
	assertContains(t, fn, "%bx.dropflag")
	// RTTI-dispatched drop of the box: the free path loads the instance's
	// typeinfo drop_fn_ptr (struct.drop.call) or falls back to pal_free
	// (struct.drop.free) — the __promise_structural_drop routing.
	if !strings.Contains(fn, "struct.drop.call") || !strings.Contains(fn, "struct.drop.free") {
		t.Fatalf("expected RTTI structural drop (struct.drop.call/free) for unwrapped box, got:\n%s", fn)
	}
}

// A while-let unwrap over a fresh owned temp (v.pop()) must likewise register the
// binding's structural free so each iteration frees its popped box.
func TestT1288WhileUnwrapStructuralTempGetsStructuralDrop(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural { to_string() string `+"`"+`abstract; }
		type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
		show(int n) Showable { return Widget(id: n); }
		drain() {
			Showable[] v = [];
			v.push(show(1));
			while wx := v.pop() { print_line(wx.to_string()); }
		}
	`)
	fn := extractFunction(ir, "__user.drain")
	if fn == "" {
		t.Fatalf("could not extract @__user.drain from IR:\n%s", ir)
	}
	assertContains(t, fn, "%wx.dropflag")
	if !strings.Contains(fn, "struct.drop.call") || !strings.Contains(fn, "struct.drop.free") {
		t.Fatalf("expected RTTI structural drop (struct.drop.call/free) for popped box, got:\n%s", fn)
	}
}

// A binary-operator source (`r + 5`) is a fresh owned temp — isFreshOwnedStructuralRHS
// accepts BinaryExpr, so the unwrap binding must get its own RTTI-dispatched drop.
// Covers the BinaryExpr acceptance branch at the unwrap-binding drop site (the
// CallExpr form is covered above).
func TestT1288IfUnwrapStructuralBinopTempGetsStructuralDrop(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural { to_string() string `+"`"+`abstract; }
		type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
		show(int n) Showable { return Widget(id: n); }
		type Reg { int base; +(int n) Showable? { return show(this.base + n); } }
		unwrap_binop() { Reg r = Reg(base: 30); if bx := r + 5 { print_line(bx.to_string()); } }
	`)
	fn := extractFunction(ir, "__user.unwrap_binop")
	if fn == "" {
		t.Fatalf("could not extract @__user.unwrap_binop from IR:\n%s", ir)
	}
	assertContains(t, fn, "%bx.dropflag")
	if !strings.Contains(fn, "struct.drop.call") || !strings.Contains(fn, "struct.drop.free") {
		t.Fatalf("expected RTTI structural drop (struct.drop.call/free) for unwrapped box, got:\n%s", fn)
	}
}

// Negative: an IdentExpr source (a stored Showable? local) keeps its own
// optional-drop owner. The unwrap binding must NOT register its own drop, else
// the box is double-freed. Evidenced by the binding having no drop flag of its
// own (the source local's opt.dropflag remains the sole owner).
func TestT1288IfUnwrapStructuralIdentSourceNoBindingDrop(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural { to_string() string `+"`"+`abstract; }
		type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
		show(int n) Showable { return Widget(id: n); }
		mkbox(int n) Showable? { Showable s = show(n); return s; }
		unwrap_ident() {
			Showable? opt = mkbox(7);
			if bx := opt { print_line(bx.to_string()); }
		}
	`)
	fn := extractFunction(ir, "__user.unwrap_ident")
	if fn == "" {
		t.Fatalf("could not extract @__user.unwrap_ident from IR:\n%s", ir)
	}
	// The source local owns and drops the box; the binding must not get its own
	// drop flag (which would double-free at scope exit).
	assertNotContains(t, fn, "%bx.dropflag")
	// The source local retains its optional-drop ownership.
	assertContains(t, fn, "%opt.dropflag")
}
