package codegen

import "testing"

// T1432: `Map[K, V?].[]` is declared `[](K key) V?`, so when the map's value type
// is itself optional the getter returns `V??` = `Optional[Optional[X]]`. That
// nested shape matched none of setDupFlagsForFieldAccess's arms (the single
// -Optional arm peeled only to `Optional[X]`), so no dup flag was armed and the
// getter's match-borrowed `return v` handed back the bucket's stored payload by
// alias — the read temp's cleanup and the map's own drop then freed the same
// allocation (`fatal: invalid free (bad header magic)`). The fix peels nested
// Optional layers down to the last single Optional before the shape dispatch.

// The `map[int, int[]?]` getter must deep-copy the stored vector (vecdup) — the
// regression guard. Fails pre-fix.
func TestT1432_OptionalVectorMapGetterDups(t *testing.T) {
	ir := generateIR(t, `
		main() {
			map[int, int[]?] m = {:};
			m[0] = [1, 2];
			int[] inner = m[0]!!;
		}
	`)
	fn := extractDefine(ir, "Map[int, Vector[int]?].[]")
	if fn == "" {
		t.Fatalf("Map[int, Vector[int]?].[] not found in IR")
	}
	assertContains(t, fn, "vecdup")
}

// Control: the one-layer `map[int, int[]]` getter already dupped via the
// typeNeedsMatchDup path and must keep doing so — guards the peel loop against
// disturbing the non-nested form.
func TestT1432_PlainVectorMapGetterStillDups(t *testing.T) {
	ir := generateIR(t, `
		main() {
			map[int, int[]] m = {:};
			m[0] = [1, 2];
			int[] inner = m[0]!;
		}
	`)
	fn := extractDefine(ir, "Map[int, Vector[int]].[]")
	if fn == "" {
		t.Fatalf("Map[int, Vector[int]].[] not found in IR")
	}
	assertContains(t, fn, "vecdup")
}

// The `map[int, string?]` getter reaches the dupStringFieldAccess arm through the
// same peel and must copy the stored string buffer.
func TestT1432_OptionalStringMapGetterDups(t *testing.T) {
	ir := generateIR(t, `
		main() {
			map[int, string?] m = {:};
			m[0] = "hello";
			string s = m[0]!!;
		}
	`)
	fn := extractDefine(ir, "Map[int, string?].[]")
	if fn == "" {
		t.Fatalf("Map[int, string?].[] not found in IR")
	}
	assertContains(t, fn, "strdup.copy")
	assertContains(t, fn, "@promise_string_new")
}

// The dupContainerFieldAccess arm the peel reaches covers Vector, Channel, Arc
// (`Ref[T]`) and Weak alike. Vector is asserted above; the three refcounted
// handles below each emit a distinct dup marker, and all three were absent
// pre-fix (the nested Optional armed no flag at all), so each is its own
// regression guard rather than a restatement of the Vector case.

// `map[int, Ref[int]?]`: the getter must bump the strong count (`arcdup`) so the
// read-out handle owns a reference. Pre-fix the read aliased the bucket's handle
// and its temp drop released the map's own reference — the stored `Ref` then
// pointed at a freed cell.
func TestT1432_OptionalRefMapGetterDups(t *testing.T) {
	ir := generateIR(t, `
		main() {
			map[int, Ref[int]?] m = {:};
			m[0] = Ref[int](5);
			Ref[int] a = m[0]!!;
		}
	`)
	fn := extractDefine(ir, "Map[int, Ref[int]?].[]")
	if fn == "" {
		t.Fatalf("Map[int, Ref[int]?].[] not found in IR")
	}
	assertContains(t, fn, "arcdup.inc")
}

// `map[int, channel[int]?]`: same arm, channel dup marker.
func TestT1432_OptionalChannelMapGetterDups(t *testing.T) {
	ir := generateIR(t, `
		main() {
			map[int, channel[int]?] m = {:};
			m[0] = channel[int](1);
			channel[int] c = m[0]!!;
		}
	`)
	fn := extractDefine(ir, "Map[int, Channel[int]?].[]")
	if fn == "" {
		t.Fatalf("Map[int, Channel[int]?].[] not found in IR")
	}
	assertContains(t, fn, "chdup.inc")
}

// `map[int, Weak[int]?]`: same arm, weak-count dup marker.
func TestT1432_OptionalWeakMapGetterDups(t *testing.T) {
	ir := generateIR(t, `
		main() {
			Ref[int] r = Ref[int](5);
			map[int, Weak[int]?] m = {:};
			m[0] = r.downgrade();
			Weak[int] w = m[0]!!;
		}
	`)
	fn := extractDefine(ir, "Map[int, Weak[int]?].[]")
	if fn == "" {
		t.Fatalf("Map[int, Weak[int]?].[] not found in IR")
	}
	assertContains(t, fn, "weakdup.inc")
}

// Over-arming guard: the peel classifies a nested Optional by its INNER shape, so
// a non-heap inner must still fall through to no flag. `map[int, int?]` reaches
// the peel (its getter returns `int??`) and must emit no dup at all — a spurious
// dup here would be a wasted copy at best and a bad free at worst.
func TestT1432_OptionalIntMapGetterDoesNotDup(t *testing.T) {
	ir := generateIR(t, `
		main() {
			map[int, int?] m = {:};
			m[0] = 3;
			int s = m[0]!!;
		}
	`)
	fn := extractDefine(ir, "Map[int, int?].[]")
	if fn == "" {
		t.Fatalf("Map[int, int?].[] not found in IR")
	}
	assertNotContains(t, fn, "strdup")
	assertNotContains(t, fn, "vecdup")
	assertNotContains(t, fn, "arcdup")
}
