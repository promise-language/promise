package codegen

import (
	"strings"
	"testing"
)

// T1299: A getter/method/index-operator returning a non-value structural-interface
// field BY VALUE (`get val V? { return this._v; }`, `[](int i) V? { return this._v; }`)
// must deep-clone the `{vtable, instance}` view box on field-escape — else the
// returned view aliases the owner's field box, and the owner's synth drop (T1284)
// plus the caller's drop free the same box (double-free / segfault). The fix routes
// the field-escape through setDupFlagsForFieldAccess + dupHeapFieldForEscape, so the
// accessor body clones via __promise_structural_clone. Sibling of T1287 (vector
// element clone on read).

// The generic index-operator body `[](int i) V? { return this._v; }` must clone the
// returned Optional[structural] field box via __promise_structural_clone.
func TestT1299IndexOperatorClonesStructuralField(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(this, int x) int `+"`"+`abstract; }
		type Counter { int base `+"`"+`value; emit(this, int x) int { return this.base + x; } }
		type Slot[V] {
			V? _v;
			[](int i) V? { return this._v; }
			[]=(int i, V move value) { this._v = value; }
		}
		build() { Slot[Sink] s = Slot[Sink](_v: none); s[0] = Counter(base: 5); x := s[0]; }
	`)
	fn := extractDefine(ir, "Slot[Sink].[]")
	if fn == "" {
		t.Fatalf("could not extract @\"Slot[Sink].[]\" from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_clone") {
		t.Fatalf("expected the generic `[](int i) V?` body to deep-clone the returned structural field box via @__promise_structural_clone; got:\n%s", fn)
	}
}

// The generic getter body `get val V? { return this._v; }` must clone the returned
// Optional[structural] field box via __promise_structural_clone.
func TestT1299GetterClonesStructuralField(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(this, int x) int `+"`"+`abstract; }
		type Counter { int base `+"`"+`value; emit(this, int x) int { return this.base + x; } }
		type Slot[V] {
			V? _v;
			get val V? { return this._v; }
		}
		build() { Slot[Sink] s = Slot[Sink](_v: Counter(base: 5)); Sink got = s.val!; }
	`)
	fn := extractDefine(ir, "Slot[Sink].val")
	if fn == "" {
		t.Fatalf("could not extract @\"Slot[Sink].val\" from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_clone") {
		t.Fatalf("expected the generic `get val V?` body to deep-clone the returned structural field box via @__promise_structural_clone; got:\n%s", fn)
	}
}

// A non-optional structural field getter `get val V { return this._v; }` must also
// clone the returned view box.
func TestT1299NonOptionalGetterClonesStructuralField(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(this, int x) int `+"`"+`abstract; }
		type Counter { int base `+"`"+`value; emit(this, int x) int { return this.base + x; } }
		type SlotN[V] {
			V _v;
			get val V { return this._v; }
		}
		build() { SlotN[Sink] s = SlotN[Sink](_v: Counter(base: 5)); Sink got = s.val; }
	`)
	fn := extractDefine(ir, "SlotN[Sink].val")
	if fn == "" {
		t.Fatalf("could not extract @\"SlotN[Sink].val\" from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_clone") {
		t.Fatalf("expected the non-optional `get val V` body to deep-clone the returned structural field box via @__promise_structural_clone; got:\n%s", fn)
	}
}

// The inline getter-unwrap receiver (`s.val!.emit(x)`) must track the owned clone in
// the CALLER via __promise_structural_drop so it is freed exactly once at statement
// end (the owner-governed member guard must be bypassed for structural getters).
func TestT1299InlineGetterUnwrapDropsCallerClone(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(this, int x) int `+"`"+`abstract; }
		type Counter { int base `+"`"+`value; emit(this, int x) int { return this.base + x; } }
		type Slot[V] {
			V? _v;
			get val V? { return this._v; }
		}
		build() { Slot[Sink] s = Slot[Sink](_v: Counter(base: 5)); int r = s.val!.emit(2); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_drop") {
		t.Fatalf("expected the inline `s.val!.emit(2)` to drop the caller-owned structural clone via @__promise_structural_drop; got:\n%s", fn)
	}
}

// A PARENTHESIZED getter-unwrap receiver (`(s.val)!.emit(x)`) must still be
// recognized as a structural-getter member source after peeling the ParenExpr, so
// the caller-owned clone is tracked/dropped (exercises the ParenExpr peel loop in
// isStructuralGetterMemberSource).
func TestT1299ParenGetterUnwrapDropsCallerClone(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(this, int x) int `+"`"+`abstract; }
		type Counter { int base `+"`"+`value; emit(this, int x) int { return this.base + x; } }
		type Slot[V] {
			V? _v;
			get val V? { return this._v; }
		}
		build() { Slot[Sink] s = Slot[Sink](_v: Counter(base: 5)); int r = (s.val)!.emit(2); }
	`)
	fn := extractDefine(ir, "__user.build")
	if fn == "" {
		t.Fatalf("could not extract @__user.build from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_drop") {
		t.Fatalf("expected the parenthesized `(s.val)!.emit(2)` to drop the caller-owned structural clone via @__promise_structural_drop; got:\n%s", fn)
	}
}

// A plain named method returning the structural field by value
// (`peek() V? { return this._v; }`) must also deep-clone the escaping view box in
// its body — the field-escape clone path is not gated on the accessor being a
// getter/index operator.
func TestT1299PlainMethodClonesStructuralField(t *testing.T) {
	ir := generateIR(t, `
		type Sink `+"`"+`structural { emit(this, int x) int `+"`"+`abstract; }
		type Counter { int base `+"`"+`value; emit(this, int x) int { return this.base + x; } }
		type Slot[V] {
			V? _v;
			peek() V? { return this._v; }
		}
		build() { Slot[Sink] s = Slot[Sink](_v: Counter(base: 5)); Sink got = s.peek()!; }
	`)
	fn := extractDefine(ir, "Slot[Sink].peek")
	if fn == "" {
		t.Fatalf("could not extract @\"Slot[Sink].peek\" from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "__promise_structural_clone") {
		t.Fatalf("expected the plain `peek() V?` body to deep-clone the returned structural field box via @__promise_structural_clone; got:\n%s", fn)
	}
}
