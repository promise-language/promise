package codegen

import "testing"

// T1017: Discarding the return value of a generic/identity-style function whose
// result aliases a live local heap-vector argument (e.g. `sort(xs);`) must clear
// the *result temp's* drop flag — not the live local's. Clearing the local's flag
// (the assignment-path behavior) leaves the result temp as the sole owner; that
// temp is dropped at statement end, freeing the buffer while the local is still
// live (use-after-free → segfault).
func TestT1017DiscardedAliasClearsResultTemp(t *testing.T) {
	ir := generateIR(t, `
		type Money { int cents; }
		ident(Money[] v) Money[] { return v; }
		main() {
			Money[] xs = [Money(cents: 1)];
			ident(xs);
			print_line(xs[0].cents.to_string());
		}
	`)
	// The discard path emits alias.discard.* blocks (not the assignment-path
	// alias.clear that targets the arg's drop flag).
	assertContains(t, ir, "alias.discard.clear")
}

// T1017/T1031 guard: the assignment / reassignment path (`xs = ident(xs)`) takes
// the call-site alias path — NOT the discard path. T1031 changed the resolution
// from clearing the argument's drop flag to cloning into the source's storage
// under a runtime alias guard (so a result aliasing a still-owned source can't
// double-free). For a clonable vector that means alias.dup + vecdup.copy.
func TestT1017AssignmentUsesAliasDupNotDiscard(t *testing.T) {
	ir := generateIR(t, `
		type Money { int cents; }
		ident(Money[] v) Money[] { return v; }
		main() {
			Money[] xs = [Money(cents: 1)];
			xs = ident(xs);
			print_line(xs[0].cents.to_string());
		}
	`)
	assertContains(t, ir, "alias.dup")
	assertContains(t, ir, "vecdup.copy")
	assertNotContains(t, ir, "alias.discard")
}
