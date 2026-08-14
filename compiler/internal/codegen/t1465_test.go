package codegen

import (
	"testing"
)

// T1465: emitViewMethodAdapter appended each synthesized trailing default to the
// forwarding call RAW — it never ran coerceCallArgs, the adaptation an ordinary
// call site applies (optional wrapping T → T?, structural-view coercion/boxing,
// subtype vtable swap). A `T? p = <bare T expr>` default was stored into the
// callee's {i1, T} optional slot without the some()-wrap, so the callee read
// `none` and silently computed the wrong answer. The fix routes the defaults
// through coerceCallArgs before adaptViewDefaultArg.

const t1465ViewIface = `
type Vw ` + "`structural" + ` {
  o(this) int ` + "`abstract" + `;
}
`

// The repro: an optional param defaulting to a BARE (non-optional) expression.
// The adapter must wrap it as some(v) — an insertvalue setting the presence bit
// to true — not store the value raw with the bit left false.
func TestT1465ViewAdapterWrapsBareOptionalDefault(t *testing.T) {
	ir := generateIR(t, t1465ViewIface+`
type Impl {
  int k;
  o(this, string? s = "a" + "b") int `+"`public"+` {
    if (s is string) { return 1; }
    return 0;
  }
}
main() {
  Impl im = Impl(k: 1);
  print_line("{im.o()}");
  Vw w = im;
  print_line("{w.o()}");
}
`)
	adapter := extractDefine(ir, "Impl.o$view_adapt_as_Vw")
	if adapter == "" {
		t.Fatalf("no view adapter emitted\n%s", ir)
	}
	// coerceCallArgs wraps the bare string into an optional: the presence bit is
	// set to true via insertvalue. Storing it raw would leave the bit false (the
	// zeroed slot the callee reads as `none`).
	assertContains(t, adapter, "insertvalue { i1, i8* } undef, i1 true, 0")
}

// A concrete default flowing into an OPTIONAL STRUCTURAL param exercises the full
// coerceCallArgs path the raw-append bypassed: coerceToOptionalElem (T1298) BOXES
// the concrete into the interface's element view (vtable swap) AND wrapOptional
// some()-wraps it. adaptViewDefaultArg alone did the box but never the wrap, so the
// callee read `none`. The adapter must contain both the view-vtable swap and the
// presence-bit-true wrap of the boxed element.
func TestT1465ViewAdapterBoxesAndWrapsOptionalStructuralDefault(t *testing.T) {
	ir := generateIR(t, `
type Vw `+"`structural"+` {
  o(this) int `+"`abstract"+`;
}
type Greeter `+"`structural"+` {
  greet(this) int `+"`abstract"+`;
}
type Hi {
  int n;
  greet(this) int `+"`public"+` => this.n;
}
type OptStructImpl {
  int k;
  o(this, Greeter? g = Hi(n: 9)) int `+"`public"+` => g!.greet();
}
main() {
  OptStructImpl im = OptStructImpl(k: 1);
  Vw w = im;
  print_line("{w.o()}");
}
`)
	adapter := extractDefine(ir, "OptStructImpl.o$view_adapt_as_Vw")
	if adapter == "" {
		t.Fatalf("no view adapter emitted\n%s", ir)
	}
	// The concrete Hi is boxed into the Greeter element view — its vtable pointer
	// is swapped to the Hi→Greeter view vtable (structural-interface coercion).
	assertContains(t, adapter, "promise_vtable_Hi_as_Greeter")
	// ...and the boxed element is some()-wrapped: the optional presence bit is true.
	assertContains(t, adapter, "insertvalue { i1, { i8*, i8* } } undef, i1 true, 0")
}

// A no-default optional trailing param must still arrive as `none`: coerceCallArgs
// sees the TypNone sema type and takes the none→T? path, leaving the zeroed slot.
func TestT1465ViewAdapterNoDefaultOptionalStaysNone(t *testing.T) {
	ir := generateIR(t, t1465ViewIface+`
type ImplNone {
  int k;
  o(this, string? s) int `+"`public"+` {
    if (s is string) { return 1; }
    return 0;
  }
}
main() {
  ImplNone im = ImplNone(k: 1);
  print_line("{im.o()}");
  Vw w = im;
  print_line("{w.o()}");
}
`)
	adapter := extractDefine(ir, "ImplNone.o$view_adapt_as_Vw")
	if adapter == "" {
		t.Fatalf("no view adapter emitted\n%s", ir)
	}
	// No default → the optional slot is zeroed (`none`); no heap default allocated.
	assertContains(t, adapter, "zeroinitializer")
	assertNotContains(t, adapter, "promise_string_concat")
}
