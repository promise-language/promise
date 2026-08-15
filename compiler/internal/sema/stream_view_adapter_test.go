package sema

import (
	"strings"
	"testing"
)

// countT1486 returns how many errors reference the T1486 diagnostic — used to
// pin that the two report paths (boxing site + declaration) never both fire.
func countT1486(errs []error) int {
	n := 0
	for _, e := range errs {
		if strings.Contains(e.Error(), "T1486") {
			n++
		}
	}
	return n
}

// T1486: boxing a concrete type into an abstract view is rejected when the
// satisfying concrete method is a GENERATOR (`stream[T]`) with extra trailing
// parameters the view adapter would have to synthesize a default for. The
// adapter frees that default before the coroutine reads it lazily → UAF. The
// .rodata-vs-heap nature of the default is irrelevant (the corrected predicate).

// Structural view, heap-concatenated default — the filed repro.
func TestStreamViewAdapterRejectHeapDefaultStructural(t *testing.T) {
	errs := checkErrs(t, `
		type Streamer `+"`structural"+` {
		  items(this) stream[string] `+"`abstract"+`;
		}
		type Impl {
		  int n;
		  items(this, string sep = "-" + "x") stream[string] {
		    int i = 0;
		    while (i < this.n) { yield "{i}{sep}"; i += 1; }
		  }
		}
		box() {
		  Impl p = Impl(n: 2);
		  Streamer t = p;
		}
	`)
	expectError(t, errs, "cannot be used as Streamer")
	expectError(t, errs, "T1486")
}

// Same shape but a .rodata literal default — the corrected predicate deliberately
// ALSO rejects this (pins against re-introducing the ".rodata is fine" gate).
func TestStreamViewAdapterRejectLiteralDefaultStructural(t *testing.T) {
	errs := checkErrs(t, `
		type Streamer `+"`structural"+` {
		  items(this) stream[string] `+"`abstract"+`;
		}
		type Impl {
		  int n;
		  items(this, string sep = "-y") stream[string] {
		    int i = 0;
		    while (i < this.n) { yield "{i}{sep}"; i += 1; }
		  }
		}
		box() {
		  Impl p = Impl(n: 2);
		  Streamer t = p;
		}
	`)
	expectError(t, errs, "cannot be used as Streamer")
	expectError(t, errs, "T1486")
}

// Optional target still counts as a box (cf. T1298).
func TestStreamViewAdapterRejectOptionalTarget(t *testing.T) {
	errs := checkErrs(t, `
		type Streamer `+"`structural"+` {
		  items(this) stream[string] `+"`abstract"+`;
		}
		type Impl {
		  int n;
		  items(this, string sep = "-y") stream[string] {
		    int i = 0;
		    while (i < this.n) { yield "{i}{sep}"; i += 1; }
		  }
		}
		box() {
		  Impl p = Impl(n: 2);
		  Streamer? t = p;
		}
	`)
	expectError(t, errs, "T1486")
}

// Explicit `is` (non-structural interface) — reported at the declaration, even
// when the type is never boxed.
func TestStreamViewAdapterRejectExplicitIs(t *testing.T) {
	errs := checkErrs(t, `
		type Streamer {
		  items(this) stream[string] `+"`abstract"+`;
		}
		type Impl is Streamer {
		  int n;
		  items(this, string sep = "-" + "x") stream[string] {
		    int i = 0;
		    while (i < this.n) { yield "{i}{sep}"; i += 1; }
		  }
		}
	`)
	expectError(t, errs, "cannot be used as Streamer")
	expectError(t, errs, "T1486")
}

// Dedup: a type that BOTH explicitly `is` a STRUCTURAL view AND is boxed into it
// hits both report paths (declaration in override.go, boxing site in expr.go). The
// boxing path must recognize the explicit child and stay silent so exactly ONE
// T1486 diagnostic fires — this exercises the IsExplicitChild(...)==true branch of
// checkStreamViewBox and answers T1486's open sub-question (explicit `is` on a
// structural view reaches the adapter too, so the check must cover it once).
func TestStreamViewAdapterExplicitIsStructuralNoDup(t *testing.T) {
	errs := checkErrs(t, `
		type Streamer `+"`structural"+` {
		  items(this) stream[string] `+"`abstract"+`;
		}
		type Impl is Streamer {
		  int n;
		  items(this, string sep = "-" + "x") stream[string] {
		    int i = 0;
		    while (i < this.n) { yield "{i}{sep}"; i += 1; }
		  }
		}
		box() {
		  Impl p = Impl(n: 2);
		  Streamer t = p;
		}
	`)
	expectError(t, errs, "T1486")
	if got := countT1486(errs); got != 1 {
		t.Fatalf("expected exactly one T1486 diagnostic (dedup), got %d: %v", got, errs)
	}
}

// A GENERIC concrete type boxed into a view — `from` reaches the check as an
// *Instance, exercising the *Instance branches of UnlowerableStreamViewMethod /
// IsExplicitChild. Structural satisfaction (no explicit `is`) → reported at the
// boxing site, exactly once.
func TestStreamViewAdapterRejectGenericInstance(t *testing.T) {
	errs := checkErrs(t, `
		type Streamer `+"`structural"+` {
		  items(this) stream[string] `+"`abstract"+`;
		}
		type Impl[T] {
		  int n;
		  items(this, string sep = "-" + "x") stream[string] {
		    int i = 0;
		    while (i < this.n) { yield "{i}{sep}"; i += 1; }
		  }
		}
		box() {
		  Impl[int] p = Impl[int](n: 2);
		  Streamer t = p;
		}
	`)
	expectError(t, errs, "cannot be used as Streamer")
	if got := countT1486(errs); got != 1 {
		t.Fatalf("expected exactly one T1486 diagnostic, got %d: %v", got, errs)
	}
}

// Negative: a stream method with NO extra params boxed into a view — legal.
func TestStreamViewAdapterNoExtraParamsOK(t *testing.T) {
	errs := checkErrs(t, `
		type Streamer `+"`structural"+` {
		  items(this) stream[string] `+"`abstract"+`;
		}
		type Impl {
		  int n;
		  items(this) stream[string] {
		    int i = 0;
		    while (i < this.n) { yield "{i}"; i += 1; }
		  }
		}
		box() {
		  Impl p = Impl(n: 2);
		  Streamer t = p;
		}
	`)
	expectNoErrorContaining(t, errs, "T1486")
}

// Negative: a NON-generator method with extra defaulted params boxed into a view
// — legal (this is T1460's adapter path).
func TestStreamViewAdapterNonGeneratorExtraParamOK(t *testing.T) {
	errs := checkErrs(t, `
		type Tagger `+"`structural"+` {
		  tag(this) string `+"`abstract"+`;
		}
		type Impl {
		  int v;
		  tag(this, string sep = "-" + "x") string => "{this.v}{sep}";
		}
		box() {
		  Impl p = Impl(v: 2);
		  Tagger t = p;
		}
	`)
	expectNoErrorContaining(t, errs, "T1486")
}

// Negative: a generator method with extra defaults called DIRECTLY, never boxed
// — legal (this is T1467's call path).
func TestStreamViewAdapterDirectCallOK(t *testing.T) {
	errs := checkErrs(t, `
		type Impl {
		  int n;
		  items(this, string sep = "-" + "x") stream[string] {
		    int i = 0;
		    while (i < this.n) { yield "{i}{sep}"; i += 1; }
		  }
		}
		use_it() {
		  Impl p = Impl(n: 2);
		  for s in p.items() { }
		}
	`)
	expectNoErrorContaining(t, errs, "T1486")
}
