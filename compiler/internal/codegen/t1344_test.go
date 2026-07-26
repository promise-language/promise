package codegen

import (
	"strings"
	"testing"
)

// T1344: a mutating combinator like `chain` takes an `Iterator[T] move other`
// param and move-captures it into the returned closure's env. The env drop must
// clean up that owned structural value, dispatching through the concrete type's
// typeinfo drop_fn_ptr (RTTI) since the concrete type is unknown at compile time
// (envDropStructural). Without it, the move-captured iterator leaks.
func TestT1344_ChainClosureEnvDropsMovedStructuralOther(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] a = [1, 2];
			int[] b = [3, 4];
			int[] r = a.iter().chain(b.iter()).collect();
			print_line(r.len.to_string());
		}
	`)
	// The chain closure's env-drop must carry a structural RTTI drop dispatch
	// (envDropStructural emits `st.rtti`/`st.drop`/`st.free` blocks that load the
	// typeinfo drop_fn_ptr and call it). Find the env_drop that references those.
	var found bool
	for _, name := range functionNamesWithSuffix(ir, ".env_drop") {
		body := extractDefine(ir, name)
		if strings.Contains(body, "st.rtti.") && strings.Contains(body, "st.drop.") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no env_drop with a structural RTTI drop dispatch (st.rtti/st.drop) found — moved `other` iterator would leak:\n%s", ir)
	}
}

// T1344: the typeinfo drop_fn_ptr for _FnIter[int] (a native, self-freeing drop)
// must point to the bare drop (which self-frees via __promise_iter_cleanup), NOT
// a `$wrap` (drop + pal_free) — wrapping a self-freeing drop double-frees the
// instance when dispatched via RTTI.
func TestT1344_FnIterTypeinfoUsesBareSelfFreeingDrop(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] a = [1, 2];
			int[] b = [3, 4];
			int[] r = a.iter().chain(b.iter()).collect();
			print_line(r.len.to_string());
		}
	`)
	ti := extractGlobalLine(ir, `promise_typeinfo__FnIter[int]`)
	if ti == "" {
		t.Fatalf("_FnIter[int] typeinfo global not found:\n%s", ir)
	}
	if strings.Contains(ti, `_FnIter[int].drop$wrap`) {
		t.Errorf("_FnIter[int] typeinfo must not reference the double-freeing drop$wrap:\n%s", ti)
	}
	if !strings.Contains(ti, `_FnIter[int].drop"`) {
		t.Errorf("_FnIter[int] typeinfo drop_fn_ptr should reference the bare self-freeing drop:\n%s", ti)
	}
	// The $wrap must not be emitted at all for a native self-freeing drop.
	assertNotContains(t, ir, `_FnIter[int].drop$wrap`)
}

// T1344 (non-native counterpart): a user structural-satisfying type with an
// EXPLICIT (non-native) drop, move-captured into a combinator, must have its
// typeinfo drop_fn_ptr point to the `$wrap` (drop body + pal_free) — the
// `explicitDrop && !dropIsNative` branch. This is the exact opposite of the
// native _FnIter case: here the wrap IS required, because a user drop does not
// free its own instance. Guards against a regression that would over-broaden the
// dropIsNative exception and drop the wrap for real user drops (→ instance leak).
func TestT1344_UserExplicitDropTypeinfoUsesWrap(t *testing.T) {
	ir := generateIR(t, `
		type OwningCounter is Iterator[int] {
			int current; int limit; int[] scratch;
			next(~this) int? {
				if this.current < this.limit { v := this.current; this.current = this.current + 1; return v; }
				return none;
			}
			drop(~this) {}
		}
		type Counter is Iterator[int] {
			int current; int limit;
			next(~this) int? {
				if this.current < this.limit { v := this.current; this.current = this.current + 1; return v; }
				return none;
			}
		}
		main() {
			a := Counter(current: 0, limit: 2);
			b := OwningCounter(current: 100, limit: 103, scratch: [1, 2, 3]);
			int[] r = a.chain(move b).collect();
			print_line(r.len.to_string());
		}
	`)
	ti := extractGlobalLine(ir, `promise_typeinfo_OwningCounter`)
	if ti == "" {
		t.Fatalf("OwningCounter typeinfo global not found:\n%s", ir)
	}
	if !strings.Contains(ti, `OwningCounter.drop$wrap`) {
		t.Errorf("OwningCounter typeinfo drop_fn_ptr should reference the $wrap (drop + pal_free) for a non-native user drop:\n%s", ti)
	}
}

// functionNamesWithSuffix returns the quoted names of every `define` whose
// symbol name ends with the given suffix (before the arg list).
func functionNamesWithSuffix(ir, suffix string) []string {
	var out []string
	for _, line := range strings.Split(ir, "\n") {
		if !strings.HasPrefix(line, "define ") {
			continue
		}
		at := strings.Index(line, "@")
		if at < 0 {
			continue
		}
		open := strings.Index(line[at:], "(")
		if open < 0 {
			continue
		}
		sym := line[at+1 : at+open]
		trimmed := strings.Trim(sym, `"`)
		if strings.HasSuffix(trimmed, suffix) {
			out = append(out, trimmed)
		}
	}
	return out
}

// extractGlobalLine returns the full text of the `@<name> = ...` global
// definition line (globals are single-line in this IR), matching the bare or
// quoted symbol form.
func extractGlobalLine(ir, name string) string {
	for _, line := range strings.Split(ir, "\n") {
		if strings.HasPrefix(line, "@"+name+" ") || strings.HasPrefix(line, `@"`+name+`" `) {
			return line
		}
	}
	return ""
}
