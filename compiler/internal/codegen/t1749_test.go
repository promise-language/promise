package codegen

import (
	"regexp"
	"strings"
	"testing"
)

// T1749: calling a `factory method on a type that has at least one subtype
// panicked in codegen with `undefined variable "Base"`. needsVtable(named) is
// true as soon as a type has children, so genMethodCall routed the call down
// genVirtualMethodCall, which tried to evaluate the *type name* `Base` as a
// receiver variable. A receiver-less member (`factory / `global / `mono) is a
// static call on the type name — there is no instance to load a vtable pointer
// from — so it can never dispatch virtually.
//
// The fix is two-sided, and both sides need a guard: the dispatch sites skip
// virtual dispatch when Sig().Recv() == nil, and AllVirtualMethods stops handing
// receiver-less members a vtable slot (a slot nothing could ever call through,
// whose only effect was to shift every later method's index).

func TestT1749FactoryOnParentTypeIsDirectCall(t *testing.T) {
	ir := generateIR(t, `
		type Base { int x;
			make(int v) Self `+"`factory"+` { return Base(x: v); }
			greet(this) string { return "base"; }
		}
		type Kid is Base { int y;
			greet(this) string { return "kid"; }
		}
		main() {
			b := Base.make(1);
			b.x.to_string();
		}
	`)

	body := extractDefine(ir, ".goroutine.main")
	if body == "" {
		t.Fatal("expected @.goroutine.main to be defined")
	}
	if !strings.Contains(body, "call { i8*, i8* } @Base.make(i64 1)") {
		t.Errorf("expected a direct call to @Base.make, got:\n%s", body)
	}
	// A virtual call materializes its callee by loading an i8* out of the vtable
	// and bitcasting it to a function pointer, then calling that register. The
	// factory is the only call in this body that could have taken that shape.
	if regexp.MustCompile(`call \{ i8\*, i8\* \} %\d+\(`).MatchString(body) {
		t.Errorf("factory call must not dispatch through a function pointer, got:\n%s", body)
	}
}

func TestT1749ReceiverlessMembersHoldNoVtableSlot(t *testing.T) {
	// `greet` is the only virtual member, so it must occupy slot 0 even though a
	// `factory and a `global method are declared ahead of it. Before the fix each
	// of those consumed a slot, so `greet` sat at index 2.
	ir := generateIR(t, `
		type Base { int x;
			make(int v) Self `+"`factory"+` { return Base(x: v); }
			tag() string `+"`global"+` { return "t"; }
			greet(this) string { return "base"; }
		}
		type Kid is Base { int y;
			greet(this) string { return "kid"; }
		}
		main() {
			Base b = Kid(x: 1, y: 2);
			b.greet();
		}
	`)

	for _, tc := range []struct{ global, impl string }{
		{"promise_vtable_Base", "@Base.greet"},
		{"promise_vtable_Kid", "@Kid.greet"},
	} {
		line := extractGlobal(ir, tc.global)
		if line == "" {
			t.Fatalf("expected @%s to be defined", tc.global)
		}
		want := "@" + tc.global + " = constant [1 x i8*] [i8* bitcast (i8* (i8*)* " + tc.impl + " to i8*)]"
		if line != want {
			t.Errorf("vtable must hold only the one virtual method:\n got: %s\nwant: %s", line, want)
		}
	}

	body := extractDefine(ir, ".goroutine.main")
	if body == "" {
		t.Fatal("expected @.goroutine.main to be defined")
	}
	// The virtual greet() call must index vtable slot 0.
	slot := regexp.MustCompile(`getelementptr i8\*, i8\*\* %\d+, i32 (\d+)`).FindStringSubmatch(body)
	if slot == nil {
		t.Fatalf("expected a vtable slot load for the virtual greet() call, got:\n%s", body)
	}
	if slot[1] != "0" {
		t.Errorf("expected greet() to dispatch through slot 0, got slot %s", slot[1])
	}
}

// The two other dispatch sites carrying the same guard: genGetterCall and
// genSetterCall. A `global getter/setter is receiver-less exactly as a `factory
// is, so on a type that needs a vtable both must stay direct calls.
func TestT1749GlobalGetterAndSetterOnParentAreDirectCalls(t *testing.T) {
	ir := generateIR(t, `
		type Base { int x;
			get gate int `+"`global"+` { return 7; }
			set gate(int v) `+"`global"+` { }
			greet(this) string { return "base"; }
		}
		type Kid is Base { int y;
			greet(this) string { return "kid"; }
		}
		main() {
			n := Base.gate;
			Base.gate = n;
		}
	`)

	body := extractDefine(ir, ".goroutine.main")
	if body == "" {
		t.Fatal("expected @.goroutine.main to be defined")
	}
	// Receiver-less: the call takes no arguments at all, and the setter takes
	// only the value — no instance pointer is threaded in.
	if !strings.Contains(body, "call i64 @Base.gate()") {
		t.Errorf("expected a direct, argument-less call to @Base.gate, got:\n%s", body)
	}
	if !strings.Contains(body, "call void @Base.gate$set(i64") {
		t.Errorf("expected a direct call to @Base.gate$set with only the value, got:\n%s", body)
	}
	// Neither may load a callee out of the vtable. Base's vtable holds exactly
	// one slot (greet), and this body never dispatches virtually.
	assertNotContains(t, body, "getelementptr i8*, i8**")
}

// `global members ARE inherited (§5.7 of docs/language-design.md), so a write or
// read through the CHILD's type name must mangle to the OWNER's name. Resolving
// it to the name written at the call site looks up an undeclared @Kid.gate$set.
func TestT1749InheritedGlobalAccessorsResolveToOwner(t *testing.T) {
	ir := generateIR(t, `
		type Base { int x;
			get gate int `+"`global"+` { return 7; }
			set gate(int v) `+"`global"+` { }
			greet(this) string { return "base"; }
		}
		type Kid is Base { int y;
			greet(this) string { return "kid"; }
		}
		main() {
			n := Kid.gate;
			Kid.gate = n;
		}
	`)

	body := extractDefine(ir, ".goroutine.main")
	if body == "" {
		t.Fatal("expected @.goroutine.main to be defined")
	}
	if !strings.Contains(body, "call i64 @Base.gate()") {
		t.Errorf("expected Kid.gate to call the owner's @Base.gate, got:\n%s", body)
	}
	if !strings.Contains(body, "call void @Base.gate$set(i64") {
		t.Errorf("expected Kid.gate = n to call the owner's @Base.gate$set, got:\n%s", body)
	}
	// The child never gets its own copy of an inherited `global accessor.
	assertNotContains(t, ir, "@Kid.gate")
}

// A `global getter and setter share a name but occupy DISTINCT slot keys
// (methodSlotKey appends "$set"), so together they used to consume two slots
// ahead of the real virtual members. Neither may hold one now.
func TestT1749GlobalAccessorPairHoldsNoVtableSlots(t *testing.T) {
	ir := generateIR(t, `
		type Base { int x;
			get gate int `+"`global"+` { return 7; }
			set gate(int v) `+"`global"+` { }
			get width int { return 1; }
			set width(int v) { }
		}
		type Kid is Base { int y;
			get width int { return 2; }
			set width(int v) { }
		}
		main() {
			Base b = Kid(x: 1, y: 2);
			b.width = b.width;
		}
	`)

	// Exactly two slots survive — the instance getter/setter pair.
	for _, tc := range []struct{ global, get, set string }{
		{"promise_vtable_Base", "@Base.width", "@Base.width$set"},
		{"promise_vtable_Kid", "@Kid.width", "@Kid.width$set"},
	} {
		line := extractGlobal(ir, tc.global)
		if line == "" {
			t.Fatalf("expected @%s to be defined", tc.global)
		}
		if !strings.HasPrefix(line, "@"+tc.global+" = constant [2 x i8*]") {
			t.Errorf("expected %s to hold exactly 2 slots, got: %s", tc.global, line)
		}
		if !strings.Contains(line, tc.get) || !strings.Contains(line, tc.set) {
			t.Errorf("expected %s to hold %s and %s, got: %s", tc.global, tc.get, tc.set, line)
		}
		if strings.Contains(line, ".gate") {
			t.Errorf("a `global accessor must not occupy a vtable slot, got: %s", line)
		}
	}

	body := extractDefine(ir, ".goroutine.main")
	if body == "" {
		t.Fatal("expected @.goroutine.main to be defined")
	}
	// Getter in slot 0, setter in slot 1 — the `global pair no longer shifts them.
	slots := regexp.MustCompile(`getelementptr i8\*, i8\*\* %\d+, i32 (\d+)`).FindAllStringSubmatch(body, -1)
	if len(slots) != 2 {
		t.Fatalf("expected 2 vtable slot loads (virtual getter + setter), got %d:\n%s", len(slots), body)
	}
	got := []string{slots[0][1], slots[1][1]}
	if !(got[0] == "0" && got[1] == "1") && !(got[0] == "1" && got[1] == "0") {
		t.Errorf("expected the width getter/setter to use slots 0 and 1, got %v", got)
	}
}
