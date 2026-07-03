package codegen

import (
	"strings"
	"testing"
)

// TestT1187_OwnedRvalueOptionalMatchSubjectGetsDrop pins the fix: a `match` whose
// subject is an owned rvalue (a function-call return) of an Optional with a
// droppable payload must spill the subject into a temp and register an optional-
// drop binding, so the inner heap value is dropped on every match exit. The
// value-match path (genValueMatch) previously only read the present flag (T1002)
// and nothing owned the rvalue, so the payload leaked. The drop manifests as a
// `match.subject.dropflag` temp guarding the inner drop of the payload.
func TestT1187_OwnedRvalueOptionalMatchSubjectGetsDrop(t *testing.T) {
	ir := generateIR(t, `
		type D { int x; }
		make_present() D? { return D(x: 42); }
		caller() int {
			r := match make_present() { none => 1, _ => 2 };
			return r;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	if !strings.Contains(body, "match.subject") {
		t.Errorf("T1187: owned-rvalue optional match subject not spilled for drop "+
			"(no match.subject temp):\n%s", body)
	}
	// The optional-drop lowering guards the inner drop with the subject's drop
	// flag (mirrors bindingDropOptional) — for a bare heap type the inner drop is
	// pal_free (B0211). Its presence proves the payload is freed on match exit.
	if !strings.Contains(body, "match.subject.dropflag") {
		t.Errorf("T1187: owned-rvalue optional match subject not dropped "+
			"(no match.subject.dropflag) → leaks the payload:\n%s", body)
	}
	if !strings.Contains(body, "@pal_free") {
		t.Errorf("T1187: owned-rvalue optional payload not freed "+
			"(no @pal_free inner drop):\n%s", body)
	}
}

// TestT1187_PlaceOptionalMatchSubjectNoExtraDrop pins the negative: a *place*
// subject (a local ident) is owned by the local's own scope binding and must NOT
// get a second subject drop — doing so would double-free. The local is dropped
// once via its own optional-drop binding; no `match.subject` temp is registered.
func TestT1187_PlaceOptionalMatchSubjectNoExtraDrop(t *testing.T) {
	ir := generateIR(t, `
		type D { int x; }
		make_present() D? { return D(x: 42); }
		caller() int {
			v := make_present();
			r := match v { none => 1, _ => 2 };
			return r;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	if strings.Contains(body, "match.subject") {
		t.Errorf("T1187: place (local-ident) optional match subject wrongly got a "+
			"subject-drop temp → double-frees with the local's own drop:\n%s", body)
	}
}

// TestT1187_BorrowedOptionalMatchSubjectNoDrop pins that a borrowed optional
// subject (a `D? o` parameter, owned by the caller's scope, T1188) never gets a
// subject spill-drop inside the callee — its owner drops it.
func TestT1187_BorrowedOptionalMatchSubjectNoDrop(t *testing.T) {
	ir := generateIR(t, `
		type D { int x; }
		peek(D? o) int {
			return match o { none => 1, _ => 2 };
		}
		make_present() D? { return D(x: 42); }
		main() { v := make_present(); x := peek(v); }
	`)
	body := extractDefine(ir, "__user.peek")
	if body == "" {
		t.Fatalf("expected @peek in IR")
	}
	if strings.Contains(body, "match.subject") {
		t.Errorf("T1187: parameter (place) optional match subject wrongly got a "+
			"subject-drop temp → double-frees with the owner's drop:\n%s", body)
	}
}

// TestT1187_NonDroppableRvalueOptionalNoDropFlag pins the no-op branch: an owned-
// rvalue `int?` subject is still an rvalue (the spill runs) but its payload needs
// no drop, so maybeRegisterOptionalDrop registers nothing → the subject drop flag
// stays nil and genValueMatch's `subjectDropFlag != nil` guard is false. There is
// therefore no `match.subject.dropflag` and no payload drop, only a correct value.
func TestT1187_NonDroppableRvalueOptionalNoDropFlag(t *testing.T) {
	ir := generateIR(t, `
		make_int() int? { return 42; }
		caller() int {
			r := match make_int() { none => 1, _ => 2 };
			return r;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	// The optional-drop no-op leaves no drop-flag guard: an int payload is not
	// heap-allocated, so nothing must be freed on match exit.
	if strings.Contains(body, "match.subject.dropflag") {
		t.Errorf("T1187: non-droppable (int?) rvalue optional wrongly got a subject "+
			"drop flag — maybeRegisterOptionalDrop should be a no-op:\n%s", body)
	}
}
