package codegen

import (
	"strings"
	"testing"
)

// T1287: Overwriting an element of a Vector[structural-interface] via `v[i] = x`
// must drop the previously-stored box before storing the new one — else the old
// {vtable, instance} view's heap instance leaks. genVectorIndexAssign's drop-old
// chain gained a structural branch that routes the old element through
// __promise_structural_drop (RTTI: typeinfo.drop_fn_ptr → concrete drop, else
// pal_free). The matching dup-on-read (`x := v[i]` / `a[i] = b[j]`) clones the box
// via __promise_structural_clone so aliased reads / cross-vector moves don't
// double-free. Sibling of T1284 (element drop loop) / T1292 (Map value drop).

// The overwrite site must route the old structural element box through the RTTI
// structural-drop helper.
func TestT1287IndexAssignDropsOldStructuralBox(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural { to_string() string `+"`"+`abstract; }
		type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
		build() { Showable[] v = []; v.push(Widget(id: 1)); v[0] = Widget(id: 9); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_drop") {
		t.Fatalf("expected the index-assign overwrite to drop the old structural element box via @__promise_structural_drop; got:\n%s", fn)
	}
}

// Reading a structural element into a local (`x := v[i]`) must deep-clone the box
// via __promise_structural_clone so the local owns an independent box — otherwise
// the drop-old added above would leave `x` dangling (use-after-free).
func TestT1287StructuralElementReadClonesBox(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural { to_string() string `+"`"+`abstract; }
		type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
		build() { Showable[] v = []; v.push(Widget(id: 1)); x := v[0]; v[0] = Widget(id: 9); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_clone") {
		t.Fatalf("expected `x := v[0]` to deep-clone the structural element box via @__promise_structural_clone; got:\n%s", fn)
	}
}

// Typed var-decl `Showable x = v[i]` routes through a distinct branch from the
// inferred `x := v[i]` form (genTypedVarDecl vs genInferredVarDecl); it must also
// deep-clone the box via __promise_structural_clone so the binding owns it.
func TestT1287TypedVarDeclClonesStructuralBox(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural { to_string() string `+"`"+`abstract; }
		type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
		build() { Showable[] v = []; v.push(Widget(id: 1)); Showable x = v[0]; }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_clone") {
		t.Fatalf("expected `Showable x = v[0]` to deep-clone the structural element box via @__promise_structural_clone; got:\n%s", fn)
	}
}

// Reading `v[i]` into an owning sink other than a var-decl — a `move` (consuming)
// param, a constructor field-init, or a vector-literal element — arms the same
// dup-on-read via armDupForVectorIndexArg, so each escape must deep-clone the box
// (else the sink's drop and v's element walk (T1284) free the same box).
func TestT1287EscapeSinksCloneStructuralBox(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"move_param", `consume(Showable move s) string { return s.to_string(); }
			build() { Showable[] v = []; v.push(Widget(id: 1)); string r = consume(v[0]); }`},
		{"ctor_field", `type Holder { Showable item; }
			build() { Showable[] v = []; v.push(Widget(id: 1)); Holder h = Holder(item: v[0]); }`},
		{"vector_literal", `build() { Showable[] v = []; v.push(Widget(id: 1)); Showable[] w = [v[0]]; }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ir := generateIR(t, `
				type Showable `+"`"+`structural { to_string() string `+"`"+`abstract; }
				type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
				`+tc.body)
			fn := extractDefine(ir, "__user.build")
			if fn == "" {
				t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
			}
			if !strings.Contains(fn, "__promise_structural_clone") {
				t.Fatalf("expected the %s escape to deep-clone the structural element box via @__promise_structural_clone; got:\n%s", tc.name, fn)
			}
		})
	}
}

// Cross-vector element move (`a[i] = b[j]`) must clone the read box (so `b` stays
// valid) and drop the old destination element (so it doesn't leak).
func TestT1287CrossVectorAssignClonesAndDrops(t *testing.T) {
	ir := generateIR(t, `
		type Showable `+"`"+`structural { to_string() string `+"`"+`abstract; }
		type Widget { int id; to_string() string { return "w" + this.id.to_string(); } }
		build() {
			Showable[] a = []; a.push(Widget(id: 1));
			Showable[] b = []; b.push(Widget(id: 2)); b.push(Widget(id: 3));
			a[0] = b[1];
		}
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_clone") {
		t.Fatalf("expected `a[0] = b[1]` to clone b[1]'s box via @__promise_structural_clone; got:\n%s", fn)
	}
	if !strings.Contains(fn, "__promise_structural_drop") {
		t.Fatalf("expected `a[0] = b[1]` to drop the old a[0] box via @__promise_structural_drop; got:\n%s", fn)
	}
}
