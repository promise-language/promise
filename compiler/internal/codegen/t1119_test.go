package codegen

import (
	"strings"
	"testing"
)

// TestT1119_OwnedRvalueMatchSubjectGetsDrop pins the Part-A fix: a `match` whose
// subject is an owned rvalue (here a function-call return) of a droppable enum
// must spill the subject into a temp and register an enum-drop binding, so the
// subject's variant payload is dropped on every match exit. The arm bindings are
// dup'd into independent copies (the droppable-enum rule), so nothing else frees
// the subject — pre-fix this leaked one Ref per match. The drop manifests as a
// `match.subject` temp + a Holder.drop call in the caller.
func TestT1119_OwnedRvalueMatchSubjectGetsDrop(t *testing.T) {
	ir := generateIR(t, `
		enum Holder { Pair(Ref[int] r, int n) }
		make() Holder { return Holder.Pair(Ref[int](9), 2); }
		caller() int {
			total := 0;
			match make() { Holder.Pair(r, n) => { total = n; } }
			return total;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	if !strings.Contains(body, "match.subject") {
		t.Errorf("T1119: owned-rvalue match subject not spilled for drop "+
			"(no match.subject temp):\n%s", body)
	}
	if !strings.Contains(body, "@Holder.drop") {
		t.Errorf("T1119: owned-rvalue match subject not dropped "+
			"(no @Holder.drop in caller) → leaks the variant payload:\n%s", body)
	}
}

// TestT1119_PlaceMatchSubjectNoExtraDrop pins the negative: a *place* subject (a
// local ident) is owned by the local's own scope binding and must NOT get a
// second subject drop — doing so would double-free. The function drops the local
// exactly once (via its own binding) and registers no `match.subject` temp.
func TestT1119_PlaceMatchSubjectNoExtraDrop(t *testing.T) {
	ir := generateIR(t, `
		enum Holder { Pair(Ref[int] r, int n) }
		make() Holder { return Holder.Pair(Ref[int](9), 2); }
		caller() int {
			v := make();
			total := 0;
			match v { Holder.Pair(r, n) => { total = n; } }
			return total;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	if strings.Contains(body, "match.subject") {
		t.Errorf("T1119: place (local-ident) match subject wrongly got a "+
			"subject-drop temp → double-frees with the local's own drop:\n%s", body)
	}
}

// TestT1119_BorrowedMatchSubjectNoDrop pins that a borrowed enum subject (a `&E`
// parameter) never gets a subject drop — its owner (the caller) drops it.
func TestT1119_BorrowedMatchSubjectNoDrop(t *testing.T) {
	ir := generateIR(t, `
		enum Holder { Pair(Ref[int] r, int n) }
		peek(Holder h) int {
			match h { Holder.Pair(r, n) => { return n; } }
		}
		make() Holder { return Holder.Pair(Ref[int](9), 2); }
		main() { v := make(); x := peek(v); }
	`)
	body := extractDefine(ir, "__user.peek")
	if body == "" {
		t.Fatalf("expected @peek in IR")
	}
	if strings.Contains(body, "match.subject") {
		t.Errorf("T1119: borrowed match subject wrongly got a subject-drop temp "+
			"→ double-frees with the owner's drop:\n%s", body)
	}
}
