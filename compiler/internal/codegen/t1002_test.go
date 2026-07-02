package codegen

import "testing"

// T1002: `match` on an owned/borrowed optional must compare the present flag
// (field 0 of the {i1, T} struct) for the `none` arm instead of panicking with
// "cannot compare match subject of type T?".

func TestMatchNoneOnOwnedOptional(t *testing.T) {
	ir := generateIR(t, `
		classify(int? x) int {
			return match x { none => 1, _ => 2 };
		}
		main() { r := classify(5); }
	`)
	// none arm lowers to !present: extract field 0 (i1) then compare eq to 0.
	assertContainsMatch(t, ir, `extractvalue \{ i1, i64 \}.*, 0`)
	assertContainsMatch(t, ir, `icmp eq i1`)
}

func TestMatchNoneOnBorrowedOptional(t *testing.T) {
	ir := generateIR(t, `
		type D { int x; }
		probe(int r) int {
			D? init = none;
			a := Ref[D?](move init);
			return match a.borrow { none => 1, _ => 2 };
		}
		main() { r := probe(0); }
	`)
	// Borrowed optional (Ref[D?].borrow yields D?&) still reduces to a present-flag
	// compare — the SharedRef/MutRef is stripped before deciding how to compare.
	assertContainsMatch(t, ir, `extractvalue.*, 0`)
	assertContainsMatch(t, ir, `icmp eq i1`)
}
