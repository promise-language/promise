package valuetype

import (
	"regexp"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1583 folded ~19 inline `named.IsStructural() && !named.IsValueType()` spellings
// into isStructuralView / isNonValueStructuralType. t1583_predicate_guard_test.go
// (package codegen) keeps the predicate from being re-inlined; this file covers the
// other half — that each folded site still answers the question BOTH ways.
//
// A conversion mistake is invisible in a one-sided test. Dropping the !IsValueType()
// half (the T1550 bug) only shows up on a value-typed structural, and converting one
// of the "do not touch" heap-user-type conjunctions only shows up on a heap type. So
// every case below is a PAIR compiled from the same program shape:
//
//	view  — a `structural interface with an `abstract member, satisfied by a heap
//	        type. Its runtime value is a {vtable, instance} fat pointer whose
//	        instance is a heap box, so the site must clone it on read and drop it
//	        through RTTI. isStructuralView → true.
//	value — a `structural type whose fields are ALL `value. It is a flat register-
//	        resident struct, automatically `copy, with no heap box and no drop.
//	        isStructuralView → false, and the site must emit nothing.
//
// The view case asserts the site's own marker fired; the value case asserts NO box
// management of any kind was emitted. A site that lost its !IsValueType() half fails
// the value case (or panics, which is the literal T1550 symptom and is reported as
// this site's failure); a site that gained a spurious IsValueType(), or that was
// converted when it should not have been, fails the view case.
//
// Sites covered here are the ones T1550 did NOT touch, i.e. exactly T1583's sweep.
// The T1550 sites themselves are covered by t1550_test.go and
// tests/value_types/structural_value_type_test.pr; the runtime (rather than IR-shape)
// half of these lives in that same .pr file under "T1583".
//
// Which of the 18 folded sites this file actually pins was established by mutation,
// one site at a time, in both directions — always-true (the T1550 bug: the predicate
// loses its !IsValueType() half) and always-false (the site stops recognising a view
// at all). This file catches:
//
//	fielddrop 602, 843 · native_handle 222, 1646 · synthdrop 952, 1238
//	expr_container 632 · enum_match 2016, 2310
//	drop_register 1000, 1079, 1393 · stmt_loop 179
//
// The other five — enum_match 2405 and 2516, expr_lambda 504 and 516, and
// drop_register 1476 — are pinned by the .pr file instead, and are the reason it
// exists rather than being redundant with this one. Their drops are emitted inline
// in the enclosing function, interleaved with that scope's own cleanup, so no IR
// marker attributes to them; what a wrong predicate produces there is a leaked or
// double-freed box, which the test harness's allocation accounting reports directly.
//
// Two footnotes worth keeping, both from that mutation sweep:
//
//   - Several sites are redundantly guarded — stmt_loop 179 gates the binding and
//     drop_register 1079 re-checks it before registering the free, so breaking
//     EITHER alone changes nothing and only breaking both regresses. That is why a
//     row can be caught by the global mutation yet by neither single-site one.
//   - The four rows marked reachable:false below cannot be caught in the always-true
//     direction at all. See the comment on that block.

// A `structural type whose fields are all `value — a flat value struct, not a view.
const t1583ValueDecls = `
type Metric ` + "`structural" + ` {
  int raw ` + "`value" + `;
  get raw_doubled int => this.raw * 2;
}
`

// A real interface (abstract member) plus a heap type satisfying it — a fat-pointer
// view over a heap box. Deliberately the same member name, so the two bodies below
// differ only in which type they name.
const t1583ViewDecls = `
type Doubler ` + "`structural" + ` {
  get raw_doubled int ` + "`abstract" + `;
}
type Counter {
  int n;
  get raw_doubled int => this.n * 2;
}
`

// cloneCall / dropCall match only real call sites. __promise_structural_clone and
// __promise_structural_drop are always *defined* in the module (they are emitted
// runtime helpers), so matching the bare name would count the definition and report
// a false positive on every program. TestT1583MarkersAreZeroWithoutStructuralTypes
// pins the floor at zero.
var (
	t1583CloneCall = regexp.MustCompile(`call i8\* @__promise_structural_clone`)
	t1583DropCall  = regexp.MustCompile(`call void @__promise_structural_drop`)
)

func t1583CountCloneCalls(t *testing.T, ir string) int {
	t.Helper()
	return len(t1583CloneCall.FindAllString(ir, -1))
}

func t1583CountDropCalls(t *testing.T, ir string) int {
	t.Helper()
	return len(t1583DropCall.FindAllString(ir, -1))
}

// The RTTI drop of a *local* binding is emitted inline as struct.drop.{call,free,done}
// blocks rather than as a call to the shared helper, so scope-exit sites need their
// own marker. Scoped to the user's main coroutine so std's own structural drops (of
// which there are many) are not counted.
func t1583CountMainStructDropBlocks(t *testing.T, ir string) int {
	t.Helper()
	return strings.Count(codegentest.UserMainBody(t, ir), "struct.drop.")
}

// The optional-FIELD drop/reassign path brackets its RTTI dispatch in
// optfield.struct.exec, distinguishing it from a local's scope-exit drop.
func t1583CountOptFieldStructExec(t *testing.T, ir string) int {
	t.Helper()
	return strings.Count(ir, "optfield.struct.exec")
}

// t1583AllMarkers is what the value shape must emit none of. Keeping the value-side
// assertion on ALL of them rather than on the row's own marker is deliberate: it
// catches a regressed site that boxes via a path this row did not anticipate, and it
// is what makes the vacuous rows (see reachable:false below) still assert something
// real.
func t1583AllMarkers(t *testing.T, ir string) map[string]int {
	t.Helper()
	return map[string]int{
		"structural clone calls":    t1583CountCloneCalls(t, ir),
		"structural drop calls":     t1583CountDropCalls(t, ir),
		"optfield.struct.exec":      t1583CountOptFieldStructExec(t, ir),
		"struct.drop blocks (main)": t1583CountMainStructDropBlocks(t, ir),
	}
}

// t1583IR compiles src, reporting a codegen panic as a value rather than letting it
// tear down the test binary. That matters more here than it usually would: the T1550
// symptom WAS a codegen panic ("store operands are not compatible: src={ i8*, i8* };
// dst=%promise_Metric_v*"), so a site that regresses is as likely to blow up as to
// emit the wrong thing. Left unrecovered, the first such site aborts the whole run
// and the remaining sites never report — which is exactly the case where knowing how
// many sites broke is most useful.
func t1583IR(t *testing.T, src string) (ir string, panicked any) {
	t.Helper()
	defer func() { panicked = recover() }()
	return codegentest.GenerateIR(t, src), nil
}

type t1583Pair struct {
	name string
	// site is the predicate T1583 folded, so a failure names the code to look at.
	site string
	// The same program shape twice: once over the flat value struct, once over the
	// fat-pointer view. Both decl sets declare the same member, so the two bodies
	// differ only in which type they name.
	valueBody string
	viewBody  string
	// count is the site's own evidence, asserted non-zero on the view shape.
	count func(t *testing.T, ir string) int
	// reachable records whether a value-typed structural can reach this site's
	// predicate AT ALL. See the comment on the false ones below.
	reachable bool
}

func TestT1583FoldedSitesAnswerBothWays(t *testing.T) {
	cases := []t1583Pair{
		{
			name: "while-unwrap binding",
			site: "stmt_loop.go genWhileUnwrapStmt",
			valueBody: `
				next(int n) Metric? { if n > 0 { return Metric(raw: n); } return none; }
				main() {
				  int total = 0; int n = 2;
				  while m := next(n: n) { total = total + m.raw_doubled; n = n - 1; }
				}`,
			viewBody: `
				next(int n) Doubler? { if n > 0 { return Counter(n: n); } return none; }
				main() {
				  int total = 0; int n = 2;
				  while m := next(n: n) { total = total + m.raw_doubled; n = n - 1; }
				}`,
			count:     t1583CountMainStructDropBlocks,
			reachable: true,
		},
		{
			name: "captured Optional[structural] env drop",
			site: "expr_lambda.go analyzeEnvCaptureDrop + stmt_drop_register.go maybeRegisterCapturedOptionalStructuralDrop",
			// The value payload is `copy, so it is auto-captured; the view payload
			// is not, hence `move`. That asymmetry IS the predicate — a value type
			// must never get an env-drop entry.
			valueBody: `main() { Metric? h = Metric(raw: 1); f := || -> h!.raw_doubled; x := f(); }`,
			viewBody:  `main() { Doubler? h = Counter(n: 1); f := move || -> h!.raw_doubled; x := f(); }`,
			count:     t1583CountMainStructDropBlocks,
			reachable: true,
		},
		{
			name:      "push / slice element dup",
			site:      "expr_container.go maybeDupPushElement",
			valueBody: `main() { Metric[] v = []; a := Metric(raw: 1); v.push(a); s := v[0:]; }`,
			viewBody:  `main() { Doubler[] v = []; v.push(Counter(n: 1)); s := v[0:]; }`,
			count:     t1583CountCloneCalls,
			reachable: true,
		},
		{
			name:      "Optional vector element dup",
			site:      "compiler_native_handle.go dupOptionalVectorElem",
			valueBody: `main() { (Metric?)[] v = [Metric(raw: 1)]; w := v.clone(); }`,
			viewBody:  `main() { (Doubler?)[] v = []; v.push(Counter(n: 1)); w := v.clone(); }`,
			count:     t1583CountCloneCalls,
			reachable: true,
		},
		{
			name: "Optional[structural] field reassign over a present value",
			site: "compiler_fielddrop.go emitOptionalFieldReassignDrop",
			valueBody: `type P { Metric? best; }
				main() { p := P(best: Metric(raw: 1)); p.best = Metric(raw: 2); }`,
			viewBody: `type P { Doubler? best; }
				main() { p := P(best: Counter(n: 1)); p.best = Counter(n: 2); }`,
			count:     t1583CountOptFieldStructExec,
			reachable: true,
		},

		// --- Sites whose value branch is unreachable by construction ---
		//
		// The four rows below assert the view side and the all-markers-zero value
		// side like the rest, but the value side is VACUOUS, and saying so is more
		// useful than letting a future reader assume otherwise.
		//
		// Evidence: with isStructuralView globally mutated to drop its
		// !IsValueType() half — i.e. with the exact T1550 bug reintroduced at every
		// folded site at once — a program exercising all of these shapes over a
		// `structural value type emits ZERO structural clone calls, drop calls,
		// optfield blocks and struct.drop blocks, while the same program over a view
		// emits hundreds. The five rows above do flip.
		//
		// The reason is that these four sites all sit inside a drop/dup walker that
		// is only synthesized for a type sema has marked droppable, and a type whose
		// fields are all `value never is. So the predicate is never consulted, and
		// the conversion there is neutral because it is unreachable — not because the
		// two spellings happen to agree.
		//
		// That makes the value-side assertion a guard on the REASON rather than on
		// the site: if value-typed structurals ever become droppable, these rows
		// start emitting and fail, which is the correct moment to re-examine whether
		// each site's isStructuralView is still the right predicate.
		{
			name: "enum variant field dup",
			site: "compiler_native_handle.go emitVariantFieldDup",
			valueBody: `enum S { ok(Metric m), missing }
				main() { S[] v = []; v.push(S.ok(m: Metric(raw: 1))); w := v.clone(); }`,
			viewBody: `enum S { ok(Doubler d), missing }
				main() { S[] v = []; v.push(S.ok(d: Counter(n: 1))); w := v.clone(); }`,
			count:     t1583CountCloneCalls,
			reachable: false,
		},
		{
			name: "enum variant field drop",
			site: "compiler_synthdrop.go variantFieldNeedsDrop + emitVariantFieldDrop",
			valueBody: `enum S { ok(Metric m), missing }
				main() { S[] v = []; v.push(S.ok(m: Metric(raw: 1))); }`,
			viewBody: `enum S { ok(Doubler d), missing }
				main() { S[] v = []; v.push(S.ok(d: Counter(n: 1))); }`,
			count:     t1583CountDropCalls,
			reachable: false,
		},
		{
			name: "match binding dup",
			site: "expr_enum_match.go typeNeedsMatchDup / cloneResolvedValue / dupMatchBinding / isAutoCloneBitCopy",
			valueBody: `enum S { ok(Metric m), missing }
				main() { s := S.ok(m: Metric(raw: 1));
				  match s { ok(m) => { x := m.raw_doubled; }, missing => {}, } }`,
			viewBody: `enum S { ok(Doubler d), missing }
				main() { s := S.ok(d: Counter(n: 1));
				  match s { ok(d) => { x := d.raw_doubled; }, missing => {}, } }`,
			count:     t1583CountCloneCalls,
			reachable: false,
		},
		{
			name: "Optional[structural] field drop at owner scope exit",
			site: "compiler_fielddrop.go emitOptionalValueDrop",
			valueBody: `type P { Metric? best; }
				main() { p := P(best: Metric(raw: 1)); x := p.best!.raw_doubled; }`,
			viewBody: `type P { Doubler? best; }
				main() { p := P(best: Counter(n: 1)); x := p.best!.raw_doubled; }`,
			count:     t1583CountOptFieldStructExec,
			reachable: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			viewIR, viewPanic := t1583IR(t, t1583ViewDecls+tc.viewBody)
			switch {
			case viewPanic != nil:
				t.Errorf("codegen panicked on the NON-value structural shape: %v\nSite: %s",
					viewPanic, tc.site)
			case tc.count(t, viewIR) == 0:
				t.Errorf("a NON-value structural is a {vtable, instance} view over a heap box, "+
					"so this site must manage the box — emitted none. Either the site stopped "+
					"selecting on isStructuralView, or the marker no longer matches what it "+
					"emits.\nSite: %s", tc.site)
			}

			valueIR, valuePanic := t1583IR(t, t1583ValueDecls+tc.valueBody)
			if valuePanic != nil {
				// The T1550 panic itself. Report it as this site's failure rather
				// than letting it kill the run.
				t.Fatalf("codegen panicked on the `structural VALUE shape: %v\nThis is the "+
					"T1550 signature — the site built a {vtable, instance} view for a flat "+
					"value struct, so its predicate has lost the !IsValueType() half.\nSite: %s",
					valuePanic, tc.site)
			}
			// What a value-side failure MEANS differs by row, so say which.
			cause := "this site has lost the !IsValueType() half of its predicate — the T1550 shape"
			if !tc.reachable {
				cause = "value-typed structurals have become droppable, so this site's predicate " +
					"is now reachable with one and needs re-examining (it was previously vacuous)"
			}
			for marker, got := range t1583AllMarkers(t, valueIR) {
				if got == 0 {
					continue
				}
				t.Errorf("a `structural type whose fields are all `value is a flat value struct "+
					"with no heap box, so no box management may be emitted for it — got %d %s.\n"+
					"Most likely cause: %s.\nSite: %s", got, marker, cause, tc.site)
			}
		})
	}
}

// The pair assertion above is only meaningful if the markers can distinguish anything
// at all — a marker that never matches would make every value case pass for the wrong
// reason. Pin the floor: a program with no structural types anywhere emits zero of
// each marker, so a non-zero count in a value case is always attributable to the site
// under test rather than to std.
func TestT1583MarkersAreZeroWithoutStructuralTypes(t *testing.T) {
	ir := codegentest.GenerateIR(t, `main() { int x = 1; v := [1, 2, 3]; w := v.clone(); }`)

	for marker, got := range t1583AllMarkers(t, ir) {
		if got != 0 {
			t.Errorf("%s in a program with no structural types: got %d, want 0 — the value-side "+
				"assertions cannot distinguish a regressed site from std noise", marker, got)
		}
	}
}

// isStructuralView is nil-safe by construction, which is what let three sites in
// stmt_drop_register.go collapse `named == nil || !named.IsStructural() ||
// named.IsValueType()` to `!isStructuralView(named)`. extractNamed returns nil for
// types that are not Named at all, so those sites are reached with nil for a
// primitive, a closure or a tuple payload — none of which may be treated as a view.
func TestT1583NonNamedPayloadsAreNotTreatedAsViews(t *testing.T) {
	for _, src := range []string{
		// Optional of a primitive — extractNamed is nil at maybeRegisterOptionalDrop.
		`main() { int? o = 1; while x := o { o = none; } }`,
		// Optional of a closure — a Signature, not a Named.
		`main() { ((int) -> int)? f = |int x| -> x + 1; if g := f { y := g(2); } }`,
		// Optional of a tuple.
		`main() { (int, int)? p = (1, 2); if q := p { z := q.0; } }`,
	} {
		ir := codegentest.GenerateIR(t, src)
		for marker, got := range t1583AllMarkers(t, ir) {
			if got != 0 {
				t.Errorf("non-Named optional payload emitted %d %s — a nil *types.Named must "+
					"never take the view branch.\nsource: %s", got, marker, src)
			}
		}
	}
}
