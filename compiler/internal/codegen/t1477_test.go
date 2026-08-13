package codegen

import (
	"strings"
	"testing"
)

// T1477: emitViewMethodAdapter named the adapter from (concreteCacheKey, method)
// only, omitting the view/interface. getOrEmitViewVtable caches vtables per
// (concreteCacheKey, view) pair, so boxing one NON-GENERIC concrete into two
// DIFFERENT views that each expose a same-named adapter-requiring method was a
// cache miss both times, emitting the adapter twice under one LLVM name →
// "invalid redefinition of function 'Thing.tag$view_adapt'". Including the view
// in the name gives each (concrete, view) pair its own distinct adapter. This is
// the view/interface axis of the same defect T1468 fixed on the mono-name axis;
// it reproduces on a non-generic type and is untouched by T1468.
func TestT1477ConcreteTwoViewsGetDistinctAdapters(t *testing.T) {
	ir := generateIR(t, `
type ViewA `+"`structural"+` {
  tag(this) string `+"`abstract"+`;
}
type ViewB `+"`structural"+` {
  tag(this) string `+"`abstract"+`;
}
type Thing {
  int v;
  tag(this, string s = "x" + "y") string `+"`public"+` => "t{s}";
}
main() {
  Thing a = Thing(v: 1);
  ViewA va = a;
  print_line(va.tag());

  Thing b = Thing(v: 2);
  ViewB vb = b;
  print_line(vb.tag());
}
`)
	// Each (concrete, view) pair gets its own view-keyed adapter.
	assertContains(t, ir, "@Thing.tag$view_adapt_as_ViewA(")
	assertContains(t, ir, "@Thing.tag$view_adapt_as_ViewB(")
	// ...and the un-viewed colliding name is gone entirely.
	assertNotContains(t, ir, "@Thing.tag$view_adapt(")
	// No adapter is defined twice under any single name.
	if got := strings.Count(ir, "define i8* @Thing.tag$view_adapt_as_ViewA("); got != 1 {
		t.Errorf("want exactly 1 definition of Thing.tag$view_adapt_as_ViewA, got %d", got)
	}
	if got := strings.Count(ir, "define i8* @Thing.tag$view_adapt_as_ViewB("); got != 1 {
		t.Errorf("want exactly 1 definition of Thing.tag$view_adapt_as_ViewB, got %d", got)
	}
}

// T1477 + T1468 combined axis: a single generic instantiation boxed into two
// DIFFERENT views. The adapter name must carry BOTH the per-instance mono key
// (T1468, "GBox[string]") AND the view suffix (T1477, "_as_VA"/"_as_VB").
// Dropping either half would collide the two adapters under one LLVM name. This
// exercises the interaction that neither T1468 (one view, two instantiations)
// nor the non-generic T1477 case (one instantiation, two views) covers alone.
func TestT1477GenericInstanceTwoViewsGetDistinctAdapters(t *testing.T) {
	ir := generateIR(t, `
type VA `+"`structural"+` {
  tag(this) string `+"`abstract"+`;
}
type VB `+"`structural"+` {
  tag(this) string `+"`abstract"+`;
}
type GBox[T] {
  T item;
  tag(this, string s = "x" + "y") string `+"`public"+` => "g{s}";
}
main() {
  GBox[string] b = GBox[string](item: "i");
  VA a = b;
  print_line(a.tag());

  GBox[string] b2 = GBox[string](item: "j");
  VB bb = b2;
  print_line(bb.tag());
}
`)
	// The same instantiation boxed into two views yields two adapters whose
	// names differ only in the view suffix — both mono key and view are present.
	assertContains(t, ir, `@"GBox[string].tag$view_adapt_as_VA"`)
	assertContains(t, ir, `@"GBox[string].tag$view_adapt_as_VB"`)
	if got := strings.Count(ir, `define i8* @"GBox[string].tag$view_adapt_as_VA"(`); got != 1 {
		t.Errorf("want exactly 1 definition of GBox[string].tag$view_adapt_as_VA, got %d", got)
	}
	if got := strings.Count(ir, `define i8* @"GBox[string].tag$view_adapt_as_VB"(`); got != 1 {
		t.Errorf("want exactly 1 definition of GBox[string].tag$view_adapt_as_VB, got %d", got)
	}
}
