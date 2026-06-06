package codegen

import (
	"strings"
	"testing"
)

// T0778: non-diverging handler unwrap of an ident-source `string?`/`T[]?`
// leaked the absent-runtime recovery. genBlockValue claims the recovery
// temp's drop flag, and the T0753 ident-skip on genExpr's
// *ast.ErrorHandlerExpr branch left the merged phi untracked — so on the
// absent runtime, nothing ever frees the recovery allocation.
//
// Fix: in genOptionalHandlerExpr, when the handler is non-diverging AND
// the source is an ident AND the inner type is i8* (string/vector),
// neutralize the source optional's present flag in the some-block and
// track the merged phi as an owned statement temp at the merge block.
//
// These tests lock the IR shape so a future regression on this path
// fails closed at the test layer instead of only at the
// zero-tolerance leak gate.

// neutralizeSourceSignature is the IR pattern emitted by
// neutralizeForceUnwrapSource on an ident source: a getelementptr into the
// source optional's {i1, i8*} alloca. The companion `store i1 false` to that
// pointer clears the present flag so the optional's scope drop becomes a
// no-op in the some-runtime. We match the GEP form (not the loose
// `store i1 false`, which also appears for unrelated drop-flag bookkeeping)
// to keep the assertion specific to the neutralize site.
const neutralizeSourceSignature = "getelementptr { i1, i8* }, { i1, i8* }* %o"

// TestT0778_HandlerNonDivergingIdentI8PtrTracked — the core fix. The
// some-block must contain a neutralize-source GEP into the optional's
// present flag, and a tracked temp drop must be reachable from the merge
// block so the recovery is freed on the absent-runtime path. Runtime
// zero-leak behavior is covered by the e2e batch tests in
// tests/e2e/optional_handler_unwrap_test.pr under the zero-tolerance gate.
func TestT0778_HandlerNonDivergingIdentI8PtrTracked(t *testing.T) {
	ir := generateIR(t, `
		tfn() {
			string? o = "ab".to_upper();
			n := (o? _ { "x" + "y" }).len;
		}
		main() { tfn(); }
	`)
	fn := extractFunction(ir, "__user.tfn")
	if fn == "" {
		t.Fatalf("could not extract __user.tfn from IR:\n%s", ir)
	}
	// Neutralize-source: GEP into `%o` (the source optional's {i1, i8*}
	// alloca) emitted in the opt.some block.
	if !strings.Contains(fn, neutralizeSourceSignature) {
		t.Fatalf("expected neutralize-source GEP %q in tfn (some-block "+
			"clears the source optional's present flag), got none:\n%s",
			neutralizeSourceSignature, fn)
	}
	// Tracked phi drop: promise_string_drop must be reachable from the
	// stmt-temp cleanup. Before the fix, the absent-runtime recovery would
	// leak — i.e., the merge-block tracking that wires this drop in was
	// absent.
	if !strings.Contains(fn, "call void @promise_string_drop") {
		t.Fatalf("expected `call void @promise_string_drop` (tracked phi "+
			"freed at statement end), got none:\n%s", fn)
	}
}

// TestT0778_HandlerDivergingIdentI8PtrSkipped — the gate. A diverging
// handler must NOT trigger the neutralize+track path: the phi degenerates
// to okVal aliasing the source's owned inner, and the existing T0753
// ident-skip on the ErrorHandlerExpr branch leaves it untracked — the
// optional's drop binding governs the lifetime, single-free. Adding a
// spurious neutralize on a diverging handler would still be safe (the
// optional drop becomes a no-op), but tracking the phi would double-free
// because okVal aliases the optional's inner. The gate is
// `!handlerDiverged`.
func TestT0778_HandlerDivergingIdentI8PtrSkipped(t *testing.T) {
	ir := generateIR(t, `
		tfn() int {
			string? o = "ab".to_upper();
			return (o? _ { return -1; }).len;
		}
		main() { _ := tfn(); }
	`)
	fn := extractFunction(ir, "__user.tfn")
	if fn == "" {
		t.Fatalf("could not extract __user.tfn from IR:\n%s", ir)
	}
	// Diverging handler: T0778's `!handlerDiverged` gate skips the
	// neutralize-source GEP. (The loose `store i1 false` appears for
	// unrelated drop-flag bookkeeping — e.g., the to_upper() temp's
	// entry-block init — so we match the GEP signature, which is unique
	// to the neutralize site.)
	if strings.Contains(fn, neutralizeSourceSignature) {
		t.Fatalf("did NOT expect neutralize-source GEP %q in tfn "+
			"(diverging handler must skip T0778 neutralize), got:\n%s",
			neutralizeSourceSignature, fn)
	}
}

// TestT0659_RefReturnUnwrapNoOverTrack — folded-in defensive guard for the
// post-call temp-tracking on `*ast.ErrorPropagateExpr` / `*ast.ErrorPanicExpr`
// / `*ast.ErrorHandlerExpr`. A failable borrow return (`T&`) consumed via
// `?!` / `?^` / `? e { ... }` must NOT register the i8* result as an owned
// string temp at the caller — the value aliases storage owned elsewhere.
//
// Status remains *defensive* (T0649 Part 1 removed the source-side
// allocation), so this is a structural lock: the caller bodies must
// have zero `@promise_string_new` (no source dup AND no spurious caller-
// side strdup of the borrow result) on the `?!` / `?^` paths.
func TestT0659_RefReturnUnwrapNoOverTrack(t *testing.T) {
	ir := generateIR(t, `
		type RefRet {
			string[] d;
			at!(int i) string & {
				if i < 0 { raise error("bad"); }
				return this.d[i];
			}
		}
		caller_qbang() {
			r := RefRet(d: ["aa", "bbb"]);
			x := r.at(0)?!;
			n := x.len;
		}
		_caller_qhat!() int {
			r := RefRet(d: ["aa", "bbb"]);
			return r.at(0)?^.len;
		}
		main() { caller_qbang(); _v := _caller_qhat()?!; }
	`)
	for _, name := range []string{"__user.caller_qbang", "__user._caller_qhat"} {
		fn := extractFunction(ir, name)
		if fn == "" {
			t.Fatalf("could not extract %s from IR:\n%s", name, ir)
		}
		// The caller's own body must NOT strdup the borrow result. The only
		// strdup in scope is from `at!` itself (covered by T0649's own
		// structural lock); the caller's `?!`/`?^` post-call tracking would
		// re-dup if the isRefType guard were missing.
		if got := stringNewCount(fn); got != 0 {
			t.Fatalf("expected 0 promise_string_new in %s (isRefType guard "+
				"blocks caller-side track of borrow result), got %d:\n%s",
				name, got, fn)
		}
	}
}
