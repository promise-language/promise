package codegen

import (
	"strings"
	"testing"
)

// T1289: Follow-up to T1288. Unwrapping an Optional[structural-interface] via an
// if-let/while-let binding must free the boxed instance for two more fresh-owned
// source shapes that isFreshOwnedStructuralRHS previously missed:
//   - a user-defined non-native `[]` operator (`if x := r[5]`) — IndexExpr
//   - a getter *call* (`if x := r.slot`) — MemberExpr
// Both return a fresh owned box with no surviving owner, so the binding must get
// its own RTTI-dispatched structural drop. A plain field-access MemberExpr is a
// borrow and must NOT be freed (that would double-free) — distinguished via sema
// (isGetterCallExpr vs a field access).

// A user-defined `[]` operator source (`r[5]`) is a fresh owned temp — the
// unwrap binding must get its own drop flag and RTTI-dispatched structural drop.
func TestT1289IfUnwrapStructuralIndexTempGetsStructuralDrop(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural { to_string() string `+"`"+`abstract; }
		type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
		show(int n) Showable { return Widget(id: n); }
		type Reg { int base; [](int i) Showable? { return show(this.base + i); } }
		unwrap_index() { Reg r = Reg(base: 10); if bx := r[5] { print_line(bx.to_string()); } }
	`)
	fn := extractFunction(ir, "__user.unwrap_index")
	if fn == "" {
		t.Fatalf("could not extract @__user.unwrap_index from IR:\n%s", ir)
	}
	assertContains(t, fn, "%bx.dropflag")
	if !strings.Contains(fn, "struct.drop.call") || !strings.Contains(fn, "struct.drop.free") {
		t.Fatalf("expected RTTI structural drop (struct.drop.call/free) for indexed box, got:\n%s", fn)
	}
}

// A getter-call source (`r.slot`) is a fresh owned temp — the unwrap binding
// must get its own drop flag and RTTI-dispatched structural drop.
func TestT1289IfUnwrapStructuralGetterTempGetsStructuralDrop(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural { to_string() string `+"`"+`abstract; }
		type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
		show(int n) Showable { return Widget(id: n); }
		type Reg { int base; get slot Showable? { return show(this.base); } }
		unwrap_getter() { Reg r = Reg(base: 20); if bx := r.slot { print_line(bx.to_string()); } }
	`)
	fn := extractFunction(ir, "__user.unwrap_getter")
	if fn == "" {
		t.Fatalf("could not extract @__user.unwrap_getter from IR:\n%s", ir)
	}
	assertContains(t, fn, "%bx.dropflag")
	if !strings.Contains(fn, "struct.drop.call") || !strings.Contains(fn, "struct.drop.free") {
		t.Fatalf("expected RTTI structural drop (struct.drop.call/free) for getter box, got:\n%s", fn)
	}
}

// while-let over a user-`[]` source must likewise register the binding's
// structural free so each iteration frees its box.
func TestT1289WhileUnwrapStructuralIndexTempGetsStructuralDrop(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural { to_string() string `+"`"+`abstract; }
		type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
		show(int n) Showable { return Widget(id: n); }
		type Reg { int base; [](int i) Showable? { return show(this.base + i); } }
		drain_index() {
			Reg r = Reg(base: 10);
			int i = 0;
			while wx := r[i] { print_line(wx.to_string()); i = i + 1; if i > 2 { break; } }
		}
	`)
	fn := extractFunction(ir, "__user.drain_index")
	if fn == "" {
		t.Fatalf("could not extract @__user.drain_index from IR:\n%s", ir)
	}
	assertContains(t, fn, "%wx.dropflag")
	if !strings.Contains(fn, "struct.drop.call") || !strings.Contains(fn, "struct.drop.free") {
		t.Fatalf("expected RTTI structural drop (struct.drop.call/free) for indexed box, got:\n%s", fn)
	}
}

// Negative: a plain field-access MemberExpr source (a struct field of type
// Showable?) is a borrow — the field's owner drops the box, so the unwrap
// binding must NOT register its own drop (which would double-free). Evidenced by
// the binding having no drop flag of its own.
func TestT1289IfUnwrapStructuralFieldSourceNoBindingDrop(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural { to_string() string `+"`"+`abstract; }
		type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
		show(int n) Showable { return Widget(id: n); }
		type Holder { Showable? item; }
		unwrap_field() {
			Holder h = Holder(item: show(9));
			if bx := h.item { print_line(bx.to_string()); }
		}
	`)
	fn := extractFunction(ir, "__user.unwrap_field")
	if fn == "" {
		t.Fatalf("could not extract @__user.unwrap_field from IR:\n%s", ir)
	}
	// The field's owner (h) drops the box; the binding must not get its own drop
	// flag (which would double-free at scope exit).
	assertNotContains(t, fn, "%bx.dropflag")
}

// Negative: a *native* container index source (`v[0]` on a Vector[Showable?]) is
// a borrow into container storage — the container owns and drops the boxed
// element. isFreshOwnedStructuralRHS's IndexExpr branch must return false via
// isUserIndexExpr==false (the `[]` is native), so the unwrap binding gets no
// drop flag / structural free (which would double-free against the vector's own
// element drop). This exercises the IndexExpr false branch distinct from the
// user-`[]` true branch above.
func TestT1289IfUnwrapStructuralNativeIndexSourceNoBindingDrop(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural { to_string() string `+"`"+`abstract; }
		type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
		show(int n) Showable { return Widget(id: n); }
		unwrap_native_index() {
			v := Vector[Showable?]();
			v.push(show(1));
			if bx := v[0] { print_line(bx.to_string()); }
		}
	`)
	fn := extractFunction(ir, "__user.unwrap_native_index")
	if fn == "" {
		t.Fatalf("could not extract @__user.unwrap_native_index from IR:\n%s", ir)
	}
	// Native indexing borrows the element; the binding must not free it (the
	// vector's element drop owns it). No binding drop flag, no structural drop.
	assertNotContains(t, fn, "%bx.dropflag")
	if strings.Contains(fn, "struct.drop.call") || strings.Contains(fn, "struct.drop.free") {
		t.Fatalf("native-index borrow must not register a structural drop for the binding, got:\n%s", fn)
	}
}

// while-let over a getter-call source must likewise register the binding's
// structural free (genWhileUnwrapStmt's MemberExpr/getter path) so each
// iteration frees its box. Complements the if-let getter test above and the
// while-let index test.
func TestT1289WhileUnwrapStructuralGetterTempGetsStructuralDrop(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural { to_string() string `+"`"+`abstract; }
		type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
		show(int n) Showable { return Widget(id: n); }
		type Reg { int base; get slot Showable? { return show(this.base); } }
		drain_getter() {
			Reg r = Reg(base: 5);
			int i = 0;
			while wx := r.slot { print_line(wx.to_string()); i = i + 1; if i > 2 { break; } }
		}
	`)
	fn := extractFunction(ir, "__user.drain_getter")
	if fn == "" {
		t.Fatalf("could not extract @__user.drain_getter from IR:\n%s", ir)
	}
	assertContains(t, fn, "%wx.dropflag")
	if !strings.Contains(fn, "struct.drop.call") || !strings.Contains(fn, "struct.drop.free") {
		t.Fatalf("expected RTTI structural drop (struct.drop.call/free) for getter box, got:\n%s", fn)
	}
}
