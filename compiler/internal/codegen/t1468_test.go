package codegen

import (
	"strings"
	"testing"
)

// T1468: emitViewMethodAdapter named the adapter from the GENERIC type name
// (GBox), while getOrEmitViewVtable keys its cache on the per-instance mono name
// (GBox__int). Boxing two instantiations of one generic type into the same view
// was a cache miss both times, emitting the adapter twice under one LLVM name →
// "invalid redefinition of function 'GBox.tag$view_adapt'". Naming the adapter
// from the mono key gives each instantiation its own distinct adapter.
func TestT1468GenericOwnerTwoInstantiationsGetDistinctAdapters(t *testing.T) {
	ir := generateIR(t, `
type Tagger `+"`structural"+` {
  tag(this) string `+"`abstract"+`;
}
type GBox[T] {
  T item;
  tag(this, string s = "x" + "y") string `+"`public"+` => "g{s}";
}
main() {
  GBox[string] b = GBox[string](item: "i");
  Tagger t1 = b;
  print_line(t1.tag());

  GBox[int] c = GBox[int](item: 2);
  Tagger t2 = c;
  print_line(t2.tag());
}
`)
	// Each instantiation gets its own mono-keyed adapter (monoName uses bracket
	// notation: GBox[string], GBox[int]). T1477: the adapter name also carries
	// the view suffix (_as_Tagger), mirroring the vtable global name.
	assertContains(t, ir, `@"GBox[string].tag$view_adapt_as_Tagger"`)
	assertContains(t, ir, `@"GBox[int].tag$view_adapt_as_Tagger"`)
	// ...and the generic-named collision is gone entirely.
	assertNotContains(t, ir, "@GBox.tag$view_adapt")
	// No adapter is defined twice under any single name.
	if got := strings.Count(ir, `define i8* @"GBox[string].tag$view_adapt_as_Tagger"(`); got != 1 {
		t.Errorf("want exactly 1 definition of GBox[string].tag$view_adapt_as_Tagger, got %d", got)
	}
	if got := strings.Count(ir, `define i8* @"GBox[int].tag$view_adapt_as_Tagger"(`); got != 1 {
		t.Errorf("want exactly 1 definition of GBox[int].tag$view_adapt_as_Tagger, got %d", got)
	}
}
