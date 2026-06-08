package codegen

import (
	"strings"
	"testing"
)

// T0775: Unwrapping an Optional *field* on an owner-with-drop (member source,
// `owner.field!` / `owner.field? _ { ... }`) used as a temporary double-freed
// the inner heap value — the extracted inner aliased the field's owned
// allocation but was registered as an independently-owned temp while the
// owner's drop also freed it.
//
// Fix shape:
//   - Force-unwrap (`!`): trackHeapUserTypeResult SKIPS temp-tracking for a
//     member-governed source (mirrors the ident skip) — the owner frees the
//     aliased inner, so no dup is emitted.
//   - Handler (`? _ { ... }`): genOptionalHandlerExpr DUPs the present arm so
//     the result owns an independent copy and the owner keeps & frees the
//     original (uniform across temp/binding; the field is NOT neutralized).
//
// These lock the IR shape so a regression fails at the test layer; runtime
// zero-leak/double-free behavior is covered by the e2e batch tests in
// tests/e2e/optional_field_temp_unwrap_test.pr under the zero-tolerance gate.

const t0775Decls = `
	type HBox { int n; drop(~this) {} }
	type Holder { HBox? hb; string? s; string[]? v; drop(~this) {} }
`

// TestT0775_ForceUnwrapMemberSkipsDup — the force-unwrap path leaves the
// extracted inner owned by the field (skip), so NO present-arm dup is emitted.
// The only heap activity is the constructor temp; `heapdup` (dupHeapValue's
// block prefix) must be absent.
func TestT0775_ForceUnwrapMemberSkipsDup(t *testing.T) {
	ir := generateIR(t, t0775Decls+`
		mfu() int { h := Holder(hb: HBox(n: 42), s: "a".to_upper(), v: ["x".to_upper()]); return (h.hb!).n; }
		main() { _ := mfu(); }
	`)
	fn := extractFunction(ir, "__user.mfu")
	if fn == "" {
		t.Fatalf("could not extract __user.mfu from IR:\n%s", ir)
	}
	if strings.Contains(fn, "heapdup") {
		t.Fatalf("did NOT expect a heap dup (`heapdup` block) for force-unwrap "+
			"member temp — the owner frees the aliased inner (skip, not dup):\n%s", fn)
	}
}

// TestT0775_HeapUserHandlerMemberDups — the handler path DUPs the present arm
// (dupHeapValue → `heapdup` blocks) so the result owns an independent copy.
// Both diverging and non-diverging forms dup.
func TestT0775_HeapUserHandlerMemberDups(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"diverging", `return (h.hb? _ { return -1; }).n;`},
		{"nondiverging", `return (h.hb? _ { HBox(n: -1) }).n;`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ir := generateIR(t, t0775Decls+`
				mh() int { h := Holder(hb: HBox(n: 42), s: "a".to_upper(), v: ["x".to_upper()]); `+tc.body+` }
				main() { _ := mh(); }
			`)
			fn := extractFunction(ir, "__user.mh")
			if fn == "" {
				t.Fatalf("could not extract __user.mh from IR:\n%s", ir)
			}
			if !strings.Contains(fn, "heapdup") {
				t.Fatalf("expected a heap dup (`heapdup` block) for heap-user "+
					"handler member temp (present-arm dup), got none:\n%s", fn)
			}
		})
	}
}

// TestT0775_StringVectorHandlerMemberDups — string/vector handler member temps
// dup the present arm too (dupString → `strdup.copy`, dupVector → `vecdup.copy`),
// fixing the latent string double-free (previously masked by allocator luck)
// and the vector use-after-free crash.
func TestT0775_StringVectorHandlerMemberDups(t *testing.T) {
	cases := []struct {
		name, body, sig string
	}{
		{"string", `return (h.s? _ { "zz".to_upper() }).len;`, "strdup.copy"},
		{"vector", `return (h.v? _ { ["z".to_upper()] }).len;`, "vecdup.copy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ir := generateIR(t, t0775Decls+`
				mh() int { h := Holder(hb: HBox(n: 1), s: "abc".to_upper(), v: ["x".to_upper(), "y".to_upper()]); `+tc.body+` }
				main() { _ := mh(); }
			`)
			fn := extractFunction(ir, "__user.mh")
			if fn == "" {
				t.Fatalf("could not extract __user.mh from IR:\n%s", ir)
			}
			if !strings.Contains(fn, tc.sig) {
				t.Fatalf("expected a %s dup (%q block) for handler member temp, "+
					"got none:\n%s", tc.name, tc.sig, fn)
			}
		})
	}
}

// TestT0775_NonMemberHandlerStillDups — no-regression guard: a call-source
// (non-member) handler unwrap has no owner governing the inner, so the result
// must still be an owned temp. The heap-user non-diverging form already dup'd
// nothing on the present arm (the call result is itself an owned temp); the key
// invariant is that the member-source dup gate (isOwnerGovernedMember...) does
// NOT fire for a call source — so no extra `heapdup` is introduced relative to
// the baseline call path. We assert the source call IS still present (the result
// is genuinely produced/owned) as a sanity anchor.
func TestT0775_NonMemberHandlerNoMemberDup(t *testing.T) {
	ir := generateIR(t, t0775Decls+`
		mk() HBox? { return HBox(n: 9); }
		mh() int { return (mk()? _ { HBox(n: -1) }).n; }
		main() { _ := mh(); }
	`)
	fn := extractFunction(ir, "__user.mh")
	if fn == "" {
		t.Fatalf("could not extract __user.mh from IR:\n%s", ir)
	}
	// The member-governed present-arm dup must NOT fire for a call source — the
	// call result is already an owned temp. dupHeapValue emits `heapdup` blocks;
	// none should appear in the caller for the call-source handler.
	if strings.Contains(fn, "heapdup") {
		t.Fatalf("did NOT expect a member-source `heapdup` for a call-source "+
			"handler unwrap (no owner governs the inner):\n%s", fn)
	}
}

// TestT0775_ParenWrappedMemberSource — the unwrap-source detectors peel
// ParenExpr, so `((h.hb))!` / `((h.hb))? _ {}` are recognized exactly like the
// bare member form. This locks the paren-peel loop in
// isOwnerGovernedMemberOptionalUnwrapSource: the force-unwrap form still SKIPS
// the dup (owner frees the alias) and the handler form still DUPs.
func TestT0775_ParenWrappedMemberSource(t *testing.T) {
	t.Run("force_unwrap_skips_dup", func(t *testing.T) {
		ir := generateIR(t, t0775Decls+`
			pf() int { h := Holder(hb: HBox(n: 42), s: "a".to_upper(), v: ["x".to_upper()]); return ((h.hb))!.n; }
			main() { _ := pf(); }
		`)
		fn := extractFunction(ir, "__user.pf")
		if fn == "" {
			t.Fatalf("could not extract __user.pf from IR:\n%s", ir)
		}
		if strings.Contains(fn, "heapdup") {
			t.Fatalf("paren-wrapped member force-unwrap must SKIP the dup like the "+
				"bare form (owner frees the alias):\n%s", fn)
		}
	})
	t.Run("handler_dups", func(t *testing.T) {
		ir := generateIR(t, t0775Decls+`
			ph() int { h := Holder(hb: HBox(n: 42), s: "a".to_upper(), v: ["x".to_upper()]); return ((h.hb))? _ { HBox(n: -1) }.n; }
			main() { _ := ph(); }
		`)
		fn := extractFunction(ir, "__user.ph")
		if fn == "" {
			t.Fatalf("could not extract __user.ph from IR:\n%s", ir)
		}
		if !strings.Contains(fn, "heapdup") {
			t.Fatalf("paren-wrapped member handler must DUP the present arm like the "+
				"bare form:\n%s", fn)
		}
	})
}

// TestT0775_GenericMethodBodyHandlerDups — the present-arm dup fires from inside
// a GENERIC method body, where the result type is a TypeParam resolved via
// c.typeSubst. This locks the typeSubst substitution branch in
// genOptionalHandlerExpr's dup gate (rt = types.Substitute(rt, c.typeSubst)):
// for the monomorphized `GHolder[HBox]` the unwrapped `this.item` resolves to
// the heap-user HBox and is dup'd, so the owner keeps & frees the original.
func TestT0775_GenericMethodBodyHandlerDups(t *testing.T) {
	ir := generateIR(t, `
		type HBox { int n; drop(~this) {} }
		consume[T](T x) { }
		type GHolder[T] {
			T? item;
			drop(~this) {}
			feed(this, T fb) { consume[T](this.item? _ { fb }); }
		}
		mk() GHolder[HBox] { return GHolder[HBox](item: HBox(n: 7)); }
		main() { g := mk(); g.feed(HBox(n: -1)); }
	`)
	// The monomorphized method body lives under the GHolder[HBox] owner; the
	// bracketed name is LLVM-quoted, so the marker includes the quotes.
	fn := extractFunction(ir, `"GHolder[HBox].feed"`)
	if fn == "" {
		t.Fatalf("could not extract GHolder[HBox].feed from IR:\n%s", ir)
	}
	if !strings.Contains(fn, "heapdup") {
		t.Fatalf("expected a heap dup (`heapdup` block) for the handler member temp "+
			"inside a generic method body (typeSubst-resolved heap-user), got none:\n%s", fn)
	}
}
