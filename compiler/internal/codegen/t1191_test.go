package codegen

import (
	"strings"
	"testing"
)

// TestT1191_OwnedRvalueMapIndexOptionalMatchSubjectGetsDrop pins the fix: a
// `match` whose subject is a Map/Set `[]` index read returning an optional
// (`match m[k] { none => .., _ => .. }`) with a droppable payload must spill the
// subject into a temp and register an optional-drop binding, so the owned `V?`
// the `[]` method hands back is dropped on every match exit. `subjectIsOwnedRvalueEnum`
// previously classified dup-safety on the whole `V?` wrapper (both
// typeNeedsMatchDup/enumMatchDupSafe are false for an Optional), so the read was
// misclassified as a borrow and the payload leaked. The drop manifests as a
// `match.subject.dropflag` temp guarding the inner drop of the payload (T1191).
func TestT1191_OwnedRvalueMapIndexOptionalMatchSubjectGetsDrop(t *testing.T) {
	ir := generateIR(t, `
		type D { int x; }
		make_map() Map[string, D] {
			m := Map[string, D]();
			m["k"] = D(x: 11);
			return m;
		}
		caller() int {
			m := make_map();
			r := match m["k"] { none => 1, _ => 2 };
			return r;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	if !strings.Contains(body, "match.subject") {
		t.Errorf("T1191: owned-rvalue Map-index optional match subject not spilled for "+
			"drop (no match.subject temp):\n%s", body)
	}
	// The optional-drop lowering guards the inner drop with the subject's drop
	// flag (mirrors bindingDropOptional) — for a bare heap type the inner drop is
	// pal_free (B0211). Its presence proves the payload is freed on match exit.
	if !strings.Contains(body, "match.subject.dropflag") {
		t.Errorf("T1191: owned-rvalue Map-index optional match subject not dropped "+
			"(no match.subject.dropflag) → leaks the payload:\n%s", body)
	}
	if !strings.Contains(body, "@pal_free") {
		t.Errorf("T1191: owned-rvalue Map-index optional payload not freed "+
			"(no @pal_free inner drop):\n%s", body)
	}
}

// TestT1191_MapIndexPlaceOptionalMatchSubjectNoExtraDrop pins the negative: when
// the Map-index optional is first bound to a local (`o := m["k"]`) the local owns
// it via its own optional-drop binding, so matching the *local* must NOT get a
// second subject drop (that would double-free). No `match.subject` temp is
// registered for a place subject.
func TestT1191_MapIndexPlaceOptionalMatchSubjectNoExtraDrop(t *testing.T) {
	ir := generateIR(t, `
		type D { int x; }
		make_map() Map[string, D] {
			m := Map[string, D]();
			m["k"] = D(x: 11);
			return m;
		}
		caller() int {
			m := make_map();
			o := m["k"];
			r := match o { none => 1, _ => 2 };
			return r;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	if strings.Contains(body, "match.subject") {
		t.Errorf("T1191: place (local-ident) Map-index optional match subject wrongly "+
			"got a subject-drop temp → double-frees with the local's own drop:\n%s", body)
	}
}

// TestT1191_NestedOptionalMapIndexMatchSubjectGetsDrop pins the multi-layer strip
// in subjectIsOwnedRvalueEnum: `Map[K, V?]`'s `[]` returns a doubly-optional `V??`,
// so the classifier must walk past EVERY Optional layer to reach the bottom
// droppable payload S. A single-layer strip would leave the classifier at `S?` (a
// non-droppable Optional → both typeNeedsMatchDup/enumMatchDupSafe false), drop the
// subject spill, and leak the inner string. The `match.subject` temp + inner
// free-on-exit proves the loop reached S.
func TestT1191_NestedOptionalMapIndexMatchSubjectGetsDrop(t *testing.T) {
	ir := generateIR(t, `
		type S { string name; }
		make_map() Map[string, S?] {
			m := Map[string, S?]();
			S? s = S(name: "hi");
			m["k"] = s;
			return m;
		}
		caller() int {
			m := make_map();
			r := match m["k"] { none => 1, _ => 2 };
			return r;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	if !strings.Contains(body, "match.subject") {
		t.Errorf("T1191: nested-optional (V??) Map-index match subject not spilled for "+
			"drop — the classifier stopped at an intermediate Optional layer:\n%s", body)
	}
	if !strings.Contains(body, "match.subject.dropflag") {
		t.Errorf("T1191: nested-optional Map-index match subject not dropped "+
			"(no match.subject.dropflag) → leaks the inner string payload:\n%s", body)
	}
}

// TestT1191_ParenMapIndexOptionalMatchSubjectGetsDrop pins the ParenExpr peel +
// T1191 classification working together: `match (m[k]) { .. }` must peel the
// transparent parentheses, reach the Map-index read, and still classify it as an
// owned rvalue on the optional payload — so the parens can't hide the owned read
// and reintroduce the leak.
func TestT1191_ParenMapIndexOptionalMatchSubjectGetsDrop(t *testing.T) {
	ir := generateIR(t, `
		type S { string name; }
		make_map() Map[string, S] {
			m := Map[string, S]();
			m["k"] = S(name: "hi");
			return m;
		}
		caller() int {
			m := make_map();
			r := match (m["k"]) { none => 1, _ => 2 };
			return r;
		}
		main() { x := caller(); }
	`)
	body := extractDefine(ir, "__user.caller")
	if body == "" {
		t.Fatalf("expected @caller in IR")
	}
	if !strings.Contains(body, "match.subject.dropflag") {
		t.Errorf("T1191: parenthesized Map-index optional match subject not dropped "+
			"(no match.subject.dropflag) → parens hid the owned read and leaked:\n%s", body)
	}
}
