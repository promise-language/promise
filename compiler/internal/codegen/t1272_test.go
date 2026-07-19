package codegen

import (
	"strings"
	"testing"
)

// T1272: Passing an unbound temporary (call result, constructor, or module
// getter result) into a `~` (MutRef) parameter must materialize the temp AND
// register it for drop at statement end. Previously the value was materialized
// for the callee but never drop-registered — scope exit freed nothing → leak.
// A module-getter argument additionally PANICKED in genMutRefArg (the getter
// name is not an lvalue in c.locals).
//
// Fix: (1) a bare module-getter ident used as a temporary tracks every
// heap-owning result kind (not just string) via trackGetterResultByType;
// (2) genMutRefArg routes non-lvalue getter idents/members through the
// materialize-and-track path instead of the lvalue-only / panic route.
//
// This test locks the structural IR: the caller body materializing a getter's
// heap result for a `~` param must emit a drop of that temp. Runtime zero-leak
// coverage across the full producer × param-shape × call-form matrix lives in
// tests/e2e/t1272_mutref_temp_drop_test.pr (the zero-tolerance leak gate).

// TestT1272_GetterTempToMutRefDropped — a module getter returning a heap user
// type, passed as a `~` argument, must be dropped in the caller body.
func TestT1272_GetterTempToMutRefDropped(t *testing.T) {
	ir := generateIR(t, `
		type H { int x; drop(~this) {} }
		get a_h H { return H(x: 1); }
		touch(H ~h) { h.x = 2; }
		caller() { touch(a_h); }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	if !strings.Contains(body, "@H.drop") {
		t.Errorf("getter temp passed to `~` param must be dropped in caller "+
			"(T1272); no @H.drop found in @caller body:\n%s", body)
	}
}

// TestT1272_InstanceGetterMemberToMutRefDropped — an INSTANCE getter accessed as
// a member (`owner.getter`) returning a heap user type, passed as a `~` argument,
// is not an lvalue: genFieldPtr would panic on it. genMutRefArg must route it
// through isMemberGetter's named-getter branch to the materialize-and-track path,
// so the caller body drops the temp. Locks the LookupGetter arm of isMemberGetter
// that neither the bare-ident nor module-member forms reach.
func TestT1272_InstanceGetterMemberToMutRefDropped(t *testing.T) {
	ir := generateIR(t, `
		type H { int x; drop(~this) {} }
		type Src { int s; get fresh H { return H(x: 1); } drop(~this) {} }
		touch(H ~h) { h.x = 2; }
		caller() { s := Src(s: 0); touch(s.fresh); }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	if !strings.Contains(body, "@H.drop") {
		t.Errorf("instance-getter member passed to `~` param must be dropped in "+
			"caller (T1272); no @H.drop found in @caller body:\n%s", body)
	}
}

// TestT1272_EnumGetterMemberToMutRefDropped — an ENUM getter accessed as a member
// (`variant.getter`) returning a heap user type, passed as a `~` argument, must
// route through isMemberGetter's enum-getter branch to the materialize-and-track
// path so the caller drops the temp. Locks the extractEnum/LookupGetter arm.
func TestT1272_EnumGetterMemberToMutRefDropped(t *testing.T) {
	ir := generateIR(t, `
		type H { int x; drop(~this) {} }
		enum E { A, B, get fresh H { return H(x: 1); } }
		touch(H ~h) { h.x = 2; }
		caller() { e := E.A; touch(e.fresh); }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	if !strings.Contains(body, "@H.drop") {
		t.Errorf("enum-getter member passed to `~` param must be dropped in "+
			"caller (T1272); no @H.drop found in @caller body:\n%s", body)
	}
}

// TestT1272_CtorTempToMutRefDropped — a constructor temporary passed as a `~`
// argument must be dropped in the caller body.
func TestT1272_CtorTempToMutRefDropped(t *testing.T) {
	ir := generateIR(t, `
		type H { int x; drop(~this) {} }
		touch(H ~h) { h.x = 2; }
		caller() { touch(H(x: 1)); }
		main() { caller(); }
	`)
	body := extractFunction(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	if !strings.Contains(body, "@H.drop") {
		t.Errorf("constructor temp passed to `~` param must be dropped in "+
			"caller (T1272); no @H.drop found in @caller body:\n%s", body)
	}
}
