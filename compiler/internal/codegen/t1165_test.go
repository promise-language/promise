package codegen

import (
	"regexp"
	"strings"
	"testing"
)

// T1165: an inline force-unwrapped user-defined `[]` read used as a droppable-enum
// method receiver (`m[k]!.label()` on a Map) deep-clones the stored value (Map.[]
// returns V? by cloning the slot). The synthesized receiver temp owns that clone
// and must be dropped after the call, else it leaks 1 allocation. The fix routes
// the three enum receiver-temp gates through freshEnumReceiverNeedsDrop, which adds
// the "user-defined non-native `[]` returns an owned value" case to isFreshEnumExpr.
// `+=` (the tracker title) was a red herring — the leak is this inline-read drop.

// receiverIsDropped reports whether the exact i8* SSA value passed as the receiver
// to `@Tag.label(i8* %X` is later handed to `@Tag.drop(i8* %X)`. This pins the
// behavior precisely: native-index reads emit unrelated vector element-drop loops
// (different pointers), so a blunt Contains/Count check can't distinguish them —
// only the receiver SSA value's own drop matters.
func receiverIsDropped(body string) bool {
	m := regexp.MustCompile(`@Tag\.label\(i8\* (%[\w.]+)`).FindStringSubmatch(body)
	if m == nil {
		return false
	}
	recv := m[1]
	dropRe := regexp.MustCompile(`@Tag\.drop\(i8\* ` + regexp.QuoteMeta(recv) + `\)`)
	return dropRe.MatchString(body)
}

// Map value read: `m["k"]!.label()` — the deep-cloned enum receiver temp is dropped
// (@Tag.drop on the receiver SSA value) after the `.label()` call. Without the fix,
// isFreshEnumExpr returned false for the IndexExpr-under-unwrap and no drop was
// emitted (leak of 1 allocation).
func TestT1165_MapEnumInlineReadDropsReceiverTemp(t *testing.T) {
	ir := generateIR(t, `
		enum Tag {
			Named(string name),
			Empty,
			label(this) string { return match this { Named(n) => n, Empty => "_", }; }
			drop(~this) {}
		}
		caller() {
			m := map[string, Tag]();
			m["k"] = Tag.Named(name: "a");
			s := m["k"]!.label();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	assertContains(t, body, `@Tag.label(i8*`)
	if !receiverIsDropped(body) {
		t.Fatalf("expected the deep-cloned `m[\"k\"]!` receiver temp to be dropped after @Tag.label (zero-leak):\n%s", body)
	}
}

// Getter receiver from a map inline read: `m["k"]!.tag` — the getter site
// (genEnumGetterAccess, site 2) also routes through freshEnumReceiverNeedsDrop.
// The deep-cloned enum receiver temp of a getter access must be dropped after the
// @Tag.tag call, else it leaks 1 allocation exactly like the method-call case.
// The method (label()) and generic-method (Opt.show()) sites are covered by the
// e2e file; this pins the third site — the getter — at the IR level.
func TestT1165_MapEnumInlineGetterDropsReceiverTemp(t *testing.T) {
	ir := generateIR(t, `
		enum Tag {
			Named(string name),
			Empty,
			get tag string { return match this { Named(n) => n, Empty => "_", }; }
			drop(~this) {}
		}
		caller() {
			m := map[string, Tag]();
			m["k"] = Tag.Named(name: "a");
			s := m["k"]!.tag;
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	reg := enumReceiverTempRegister(body)
	if reg == "" {
		t.Fatalf("expected an `enum.getter` receiver temp bitcast in @__user.caller:\n%s", body)
	}
	if !strings.Contains(body, "@Tag.tag(i8* "+reg+")") {
		t.Fatalf("expected @Tag.tag(i8* %s) on the getter temp:\n%s", reg, body)
	}
	if !enumReceiverTempDropped(body, reg, "Tag") {
		t.Fatalf("expected the deep-cloned `m[\"k\"]!` GETTER receiver temp %s to be dropped after @Tag.tag (zero-leak); genEnumGetterAccess must route through freshEnumReceiverNeedsDrop:\n%s", reg, body)
	}
}

// Negative guard: native array indexing (`arr[0].label()`) aliases container
// storage — the receiver is a borrowed slot, not an owned clone, so its temp must
// NOT be dropped (dropping it would double-free what the vector's own drop frees at
// scope exit). The caller still contains @Tag.drop loops for the vector's element
// drops, but none of them target the @Tag.label receiver SSA value. isUserIndexExpr
// returns false for native `[]`, so freshEnumReceiverNeedsDrop stays false here.
func TestT1165_NativeIndexReadNoReceiverDrop(t *testing.T) {
	ir := generateIR(t, `
		enum Tag {
			Named(string name),
			Empty,
			label(this) string { return match this { Named(n) => n, Empty => "_", }; }
			drop(~this) {}
		}
		caller() {
			arr := [Tag.Named(name: "n"), Tag.Empty];
			s := arr[0].label();
		}
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @__user.caller in IR:\n%s", ir)
	}
	assertContains(t, body, `@Tag.label(i8*`)
	if receiverIsDropped(body) {
		t.Fatalf("did not expect the borrowed native-index receiver to be dropped (would double-free the vector's element):\n%s", body)
	}
}
