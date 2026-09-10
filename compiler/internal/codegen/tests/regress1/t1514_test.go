package regress1

import (
	"regexp"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1514/T1985: `stream[T]` satisfies isStructuralView, so maybeTrackIterTemp used
// to register a generator factory's result as an iterator temp keyed on
// `extractvalue %call, 1` — which for a coroutine value is the YIELD SLOT, not an
// _FnIter instance. That aimed __promise_iter_cleanup at memory the generator's own
// cleanup paths already own. for-in papered over it by clearing the drop flag of
// EVERY pending heap temp (T0088), orphaning the ones it did not own; the discard
// and `yield *` paths had no such clear and double-freed the slot (T1985).
//
// maybeTrackIterTemp is reached from a METHOD call and from a generic free
// function with INFERRED type args (genInferredGenericCall); every driver below
// uses the method form, and the inferred-generic form is covered at runtime by
// tests/e2e/generators_test.pr::test_t1985_discarded_inferred_generic_generator_no_crash.

// Every driver below is the free function `drive` — statement-temp tracking is
// only enabled in a free function body — so its body is extracted with the
// package's existing `t1467Body` (t1467_test.go) rather than a second copy of
// the same `__user.`-prefix-tolerant lookup.

// t1514DefineContaining returns the whole `define ... { ... }` that contains
// needle. A generator body lives in a compiler-named `.generator.N` coroutine
// function, so it cannot be looked up by the source-level name.
func t1514DefineContaining(t *testing.T, ir, needle string) string {
	t.Helper()
	idx := strings.Index(ir, needle)
	if idx < 0 {
		t.Fatalf("%q not found in IR", needle)
	}
	start := strings.LastIndex(ir[:idx], "\ndefine ")
	if start < 0 {
		t.Fatalf("no enclosing define for %q", needle)
	}
	end := strings.Index(ir[idx:], "\n}\n")
	if end < 0 {
		t.Fatalf("unterminated define for %q", needle)
	}
	return ir[start+1 : idx+end]
}

// t1514ForInEnvFlagSources returns, for each `_forinenv` scope binding promoted
// out of the argument window (T1515's promoteEnvTempsToScopeFrom), the env-temp
// drop flag its own flag was COPIED from — the pair
//
//	%94 = load i1, i1* %21
//	store i1 %94, i1* %_forinenv.dropflag
//
// yields "%21". A promotion that inherits a flag someone already zeroed frees
// nothing, so these are the flags a disarm must not have touched.
func t1514ForInEnvFlagSources(t *testing.T, body string) []string {
	t.Helper()
	storeRe := regexp.MustCompile(`store i1 (%[\w.]+), i1\* %_forinenv[\w.]*\.dropflag`)
	var srcs []string
	for _, m := range storeRe.FindAllStringSubmatch(body, -1) {
		loadRe := regexp.MustCompile(regexp.QuoteMeta(m[1]) + ` = load i1, i1\* (%[\w.]+)`)
		lm := loadRe.FindStringSubmatch(body)
		if lm == nil {
			t.Fatalf("no load feeding %s into a _forinenv flag in:\n%s", m[1], body)
		}
		srcs = append(srcs, lm[1])
	}
	if len(srcs) == 0 {
		t.Fatalf("no _forinenv promotion emitted in:\n%s", body)
	}
	return srcs
}

// A closure argument to a generator METHOD must keep its owner. The T0100/B0213
// env claim (claimAllEnvTemps) disarms every pending closure-env temp, because a
// combinator like `.map(f)` stores the lambda inside the returned _FnIter and
// frees it from there; it is gated on the call having tracked a new iterator temp,
// which a `stream[T]` result no longer does (see maybeTrackIterTemp). A coroutine
// only BORROWS its closure parameter, so had the claim still fired, T1515's
// `_forinenv` scope binding would inherit a zeroed flag and free nothing — the
// leak this shape used to have.
func TestT1514GeneratorMethodClosureArgEnvNotClaimed(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
type GenMapper {
  string tag;
  mapped(this, int n, (int) -> string f) stream[string] {
    int i = 0;
    while (i < n) { yield "{f(i)}{this.tag}"; i += 1; }
  }
}
wrap_borrowing((int) -> string f) (int) -> string {
  return |int i| -> "{i}";
}
drive() {
  GenMapper m = GenMapper(tag: "-" + "m");
  string suffix = "-" + "s";
  string acc = "";
  for s in m.mapped(2, wrap_borrowing(move |int i| -> "{i}{suffix}")) { acc = acc + s; }
  print_line(acc);
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	call := strings.Index(body, "@GenMapper.mapped(")
	if call < 0 {
		t.Fatalf("no @GenMapper.mapped call in:\n%s", body)
	}
	// The window runs from the factory call to the promotion that reads the flags:
	// claimAllEnvTemps would have emitted its blanket `store i1 false` here.
	promote := strings.Index(body[call:], "%_forinenv")
	if promote < 0 {
		t.Fatalf("no _forinenv promotion after the factory call in:\n%s", body)
	}
	window := body[call : call+promote]
	for _, src := range t1514ForInEnvFlagSources(t, body) {
		if strings.Contains(window, "store i1 false, i1* "+src+"\n") {
			t.Fatalf("generator factory call disarms closure-env temp flag %s before it is promoted:\n%s", src, window)
		}
	}
}

// t1514EnclosingBlock returns the basic block of `body` that contains needle: from its
// label line through the line holding needle.
func t1514EnclosingBlock(t *testing.T, body, needle string) string {
	t.Helper()
	idx := strings.Index(body, needle)
	if idx < 0 {
		t.Fatalf("%q not found in:\n%s", needle, body)
	}
	start := strings.LastIndex(body[:idx], ":\n")
	if start < 0 {
		start = 0
	} else {
		start = strings.LastIndex(body[:start], "\n") + 1
	}
	return body[start : idx+len(needle)]
}

// Iterating a generator METHOD must not register an iterator temp at all — the
// coroutine is owned by bindingGenerator. Pre-fix the driver contained a
// __promise_iter_cleanup call aimed at the yield slot.
func TestT1514GeneratorMethodForInHasNoIterCleanup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
type Src {
  string tag;
  items(this, int n) stream[string] {
    int i = 0;
    while (i < n) { yield "{i}{this.tag}"; i += 1; }
  }
}
drive() {
  Src src = Src(tag: "-" + "m");
  string acc = "";
  for s in src.items(2) { acc = acc + s; }
  print_line(acc);
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertNotContains(t, body, "@__promise_iter_cleanup")
	// The generator's own cleanup is still emitted.
	codegentest.AssertContains(t, body, "@__promise_gen_destroy")
}

// A DISCARDED generator method call is freed exactly once — by
// dropDiscardedGenerator (T1306), never by __promise_iter_cleanup.
func TestT1514DiscardedGeneratorMethodFreedOnceByGeneratorCleanup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
type Src {
  string tag;
  items(this, int n) stream[string] {
    int i = 0;
    while (i < n) { yield "{i}{this.tag}"; i += 1; }
  }
}
drive() {
  Src src = Src(tag: "-" + "d");
  src.items(2);
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertContains(t, body, "discard.gen.cleanup")
	codegentest.AssertNotContains(t, body, "@__promise_iter_cleanup")
}

// `yield *` over a generator METHOD: same single-owner property, in a generator body.
func TestT1514YieldDelegateGeneratorMethodHasNoIterCleanup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
type Src {
  string tag;
  items(this, int n) stream[string] {
    int i = 0;
    while (i < n) { yield "{i}{this.tag}"; i += 1; }
  }
}
outer(int n) stream[string] {
  Src src = Src(tag: "-" + "y");
  yield * src.items(n);
}
drive() {
  string acc = "";
  for s in outer(2) { acc = acc + s; }
  print_line(acc);
}
main() { drive(); }
`)
	// `outer` is a generator, so its body is a compiler-named `.generator.N`
	// coroutine — find it by the delegated call it contains.
	body := t1514DefineContaining(t, ir, "call { i8*, i8* } @Src.items(")
	codegentest.AssertNotContains(t, body, "@__promise_iter_cleanup")
	codegentest.AssertContains(t, body, "yieldstar.check")
}

// The reported leak: an inline constructor a nested BORROWING call consumed is an
// intermediate — it is not the argument value, so T1467's promotion does not
// re-home it. It must keep its statement lifetime and be dropped at the end of the
// for-in statement, i.e. AFTER the generator is destroyed. Pre-fix the blanket
// clear zeroed its flag and nothing freed it.
func TestT1514HeapIntermediateArgDroppedAfterGeneratorDestroy(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
type Box { string s; }
rebox(Box b) Box { return Box(s: "-" + "w"); }
boxed(int n, Box b) stream[string] {
  int i = 0;
  while (i < n) { yield "{i}{b.s}"; i += 1; }
}
drive() {
  string acc = "";
  for s in boxed(2, rebox(Box(s: "-" + "i"))) { acc = acc + s; }
  print_line(acc);
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	// The block that hands the coroutine to the loop must not disarm anything:
	// T0088 emitted a `store i1 false` there for EVERY pending heap temp, which is
	// what orphaned the intermediate.
	handoff := t1514EnclosingBlock(t, body, "i8** %gen.handle")
	if strings.Contains(handoff, "store i1 false") {
		t.Fatalf("generator handoff block still clears a heap-temp drop flag:\n%s", handoff)
	}
	// The intermediate keeps its statement lifetime: dropped at the END of the
	// for-in statement, i.e. after the generator is destroyed.
	codegentest.AssertContains(t, body, "heap.drop")
	codegentest.AssertContains(t, body, "call void @Box.drop")
	destroy := strings.LastIndex(body, "@__promise_gen_destroy")
	drop := strings.LastIndex(body, "call void @Box.drop")
	if drop < destroy {
		t.Fatalf("intermediate Box.drop must be drained after the generator destroy (statement end), got drop@%d destroy@%d\n%s", drop, destroy, body)
	}
}

// T1983: a TEMP receiver of a generator method must keep its statement lifetime.
// The T0130 receiver claim fires on any structural-view result because a combinator
// (`.map(f)`) adopts the receiver into the returned _FnIter's _parent chain and
// frees it there; a coroutine adopts nothing — it only borrows `this` — so
// claiming disarmed the receiver temp and left it with no owner. Pre-fix the call
// was followed by two `heap.claim` disarms; the drain blocks were still emitted
// but ran with the flags at 0, which is why the ordering assertion above cannot
// see this shape and the window is inspected instead.
func TestT1514GeneratorMethodTempReceiverNotClaimed(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
type Src {
  string tag;
  items(this, int n) stream[string] {
    int i = 0;
    while (i < n) { yield "{i}{this.tag}"; i += 1; }
  }
}
make_src() Src { return Src(tag: "-" + "t"); }
drive() {
  string acc = "";
  for s in make_src().items(2) { acc = acc + s; }
  print_line(acc);
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	call := strings.Index(body, "@Src.items(")
	if call < 0 {
		t.Fatalf("no @Src.items call in:\n%s", body)
	}
	handoff := strings.Index(body[call:], "i8** %gen.handle")
	if handoff < 0 {
		t.Fatalf("no generator handle store after the @Src.items call:\n%s", body)
	}
	window := body[call : call+handoff]
	if strings.Contains(window, "heap.claim") {
		t.Fatalf("receiver heap temp is disarmed after the generator factory call:\n%s", window)
	}
	// It reaches the ordinary drain instead, after the coroutine is destroyed.
	codegentest.AssertContains(t, body, "call void @Src.drop")
}

// The FAILABLE factory reaches genForInGenerator through
// unwrapFailableGeneratorResult, which runs between the call and the loop — the
// one for-in window the deleted blanket clear sat behind. A bare failable factory
// in a non-failable function takes that branch (T0284 auto-panic), and it must
// have the same two properties as the plain shape: no iterator temp aimed at the
// coroutine, and an intermediate that still reaches the statement-end drain.
func TestT1514FailableGeneratorForInKeepsIntermediateStatementLifetime(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
type Box { string s; }
rebox(Box b) Box { return Box(s: "-" + "w"); }
boxed!(int n, Box b) stream[string] {
  int i = 0;
  while (i < n) { if (i > 5) { raise error("x"); } yield "{i}{b.s}"; i += 1; }
}
drive() {
  string acc = "";
  for s in boxed(2, rebox(Box(s: "-" + "i"))) { acc = acc + s; }
  print_line(acc);
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	codegentest.AssertNotContains(t, body, "@__promise_iter_cleanup")
	handoff := t1514EnclosingBlock(t, body, "i8** %gen.handle")
	if strings.Contains(handoff, "store i1 false") {
		t.Fatalf("failable generator handoff block still clears a heap-temp drop flag:\n%s", handoff)
	}
	codegentest.AssertContains(t, body, "call void @Box.drop")
	destroy := strings.LastIndex(body, "@__promise_gen_destroy")
	drop := strings.LastIndex(body, "call void @Box.drop")
	if drop < destroy {
		t.Fatalf("intermediate Box.drop must be drained after the generator destroy, got drop@%d destroy@%d\n%s", drop, destroy, body)
	}
}

// A generator METHOD on a TEMP receiver whose argument list also holds an
// intermediate: the two mechanisms this change touches (the T0130 receiver claim
// gate and the removed for-in disarm) meet on one statement, and both owners must
// survive to the drain. Two distinct `Box`/receiver drops, both after the destroy.
func TestT1514TempReceiverAndIntermediateBothDrainedAfterDestroy(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
type Box { string s; }
rebox(Box b) Box { return Box(s: "-" + "w"); }
type BoxMapper {
  string tag;
  boxed(this, int n, Box b) stream[string] {
    int i = 0;
    while (i < n) { yield "{i}{b.s}{this.tag}"; i += 1; }
  }
}
make_mapper() BoxMapper { return BoxMapper(tag: "-" + "m"); }
drive() {
  string acc = "";
  for s in make_mapper().boxed(2, rebox(Box(s: "-" + "i"))) { acc = acc + s; }
  print_line(acc);
}
main() { drive(); }
`)
	body := t1467Body(t, ir)
	call := strings.Index(body, "@BoxMapper.boxed(")
	if call < 0 {
		t.Fatalf("no @BoxMapper.boxed call in:\n%s", body)
	}
	handoff := strings.Index(body[call:], "i8** %gen.handle")
	if handoff < 0 {
		t.Fatalf("no generator handle store after the factory call:\n%s", body)
	}
	if window := body[call : call+handoff]; strings.Contains(window, "heap.claim") ||
		strings.Contains(window, "store i1 false") {
		t.Fatalf("factory-call window disarms a pending heap temp:\n%s", window)
	}
	destroy := strings.LastIndex(body, "@__promise_gen_destroy")
	for _, drop := range []string{"call void @Box.drop", "call void @BoxMapper.drop"} {
		at := strings.LastIndex(body, drop)
		if at < 0 {
			t.Fatalf("no %q in:\n%s", drop, body)
		}
		if at < destroy {
			t.Fatalf("%q must be drained after the generator destroy, got drop@%d destroy@%d\n%s", drop, at, destroy, body)
		}
	}
}
