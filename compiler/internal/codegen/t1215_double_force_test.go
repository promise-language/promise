package codegen

import (
	"regexp"
	"testing"
)

// T1215: a nested-Optional double force-unwrap (`r!!` where `r: T??`) used inline
// as a method receiver / field access must NOT register the extracted innermost
// value as an owned statement temp. The outermost owner `r` keeps a recursive
// nested-optional drop binding that already frees the innermost allocation at
// scope exit, so the `!!`-extracted inner is an ALIAS, not a transferred owner.
// Pre-fix the extracted inner was tracked as an owned temp and dropped at
// statement end AS WELL — a double-free. For a native handle (Mutex) the second
// drop releases an already-freed pal_mutex → `segmentation fault at 0x0`; for a
// string/vector/heap-user-type it is a plain double-free.
//
// Fixed IR signature: inside the outer force's `unwrap.ok` block the extracted
// inner (`extractvalue { i1, i8* } %N, 1`) flows DIRECTLY into the receiver/field
// bitcast — no intervening `store i8* %N, i8**` + `store i1 true` owned-temp
// registration. isNestedOwnerGovernedUnwrapSource gates the three temp-tracking
// sites (genOptionalForceUnwrap, the string/vector case in genExpr, and
// trackHeapUserTypeResult).

// The forced inner of a `Mutex[int]??` double-force is bitcast straight to the
// Mutex instance layout `{ i8*, i8*, i8*, i8*, i8 }` for the `.lock()` receiver —
// with no owned-temp store between the extract and the use.
var t1215NativeDirectUse = regexp.MustCompile(`extractvalue \{ i1, i8\* \} %\d+, 1\s*\n\s*%\d+ = bitcast i8\* %\d+ to \{ i8\*, i8\*, i8\*, i8\*, i8 \}\*`)

// The owned-temp double-tracking smoking gun: the extracted force result stored
// into a temp slot and armed with `store i1 true`. Must NOT appear for the
// double-force receiver.
var t1215OwnedTempStore = regexp.MustCompile(`store i8\* %\d+, i8\*\* %\d+\s*\n\s*store i1 true, i1\* %\d+\s*\n\s*store i8\* %\d+, i8\*\* %\d+`)

// Native single-owner handle: `Mutex[int]??` → `r!!.lock().borrow`.
func TestT1215NativeDoubleForceNoOwnedTemp(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			Mutex[int]? inner = Mutex[int](5);
			Mutex[int]?? r = inner;
			int v = r!!.lock().borrow;
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if !t1215NativeDirectUse.MatchString(fn) {
		t.Fatalf("expected the `r!!` force result to be consumed directly as the lock receiver (no owned-temp store between extract and use):\n%s", fn)
	}
	if t1215OwnedTempStore.MatchString(fn) {
		t.Fatalf("`r!!` extracted inner Mutex must NOT be registered as an owned statement temp (would double-free with r's nested-optional scope-exit drop):\n%s", fn)
	}
}

// Heap string: `string?? r` → `r!!.len`. The forced inner flows straight into the
// string-instance bitcast for the `.len` read; no `promise_string_drop` at
// statement end (only the owner's scope-exit + the `.clone()` constructor temp).
var t1215StringDirectUse = regexp.MustCompile(`extractvalue \{ i1, i8\* \} %\d+, 1\s*\n\s*%\d+ = bitcast i8\* %\d+ to \{ i8\*, i64, \[0 x i8\] \}\*`)

func TestT1215StringDoubleForceNoOwnedTemp(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			string? inner = "hello".clone();
			string?? r = inner;
			int v = r!!.len;
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if !t1215StringDirectUse.MatchString(fn) {
		t.Fatalf("expected the `r!!` string force result to be consumed directly for the `.len` read (no owned-temp store between extract and use):\n%s", fn)
	}
	if t1215OwnedTempStore.MatchString(fn) {
		t.Fatalf("`r!!` extracted inner string must NOT be registered as an owned statement temp (would double-free with r's nested-optional scope-exit drop):\n%s", fn)
	}
}

// Force-`as!` cast chain: `(r as! Mutex[int]?)!.lock()`. The outer force's source
// is a ParenExpr wrapping a force CastExpr of the ident — exercising the ParenExpr
// and CastExpr(Force) peel arms of isNestedOwnerGovernedUnwrapSource (the plain
// `r!!` tests only hit the OptionalUnwrapExpr arm). Still ident-governed, so the
// extracted inner must flow straight into the lock receiver with no owned-temp store.
func TestT1215ForceCastChainNoOwnedTemp(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			Mutex[int]? inner = Mutex[int](5);
			Mutex[int]?? r = inner;
			int v = (r as! Mutex[int]?)!.lock().borrow;
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if !t1215NativeDirectUse.MatchString(fn) {
		t.Fatalf("expected the force-cast-chain `(r as! Mutex[int]?)!` result to be consumed directly as the lock receiver (no owned-temp store between extract and use):\n%s", fn)
	}
	if t1215OwnedTempStore.MatchString(fn) {
		t.Fatalf("force-cast-chain extracted inner Mutex must NOT be registered as an owned statement temp (r's nested-optional drop already frees it):\n%s", fn)
	}
}

// Member-source double-force: the outer force's base is an owner-governed member
// (`h.m` where `h: Holder` has a drop and `m: Mutex[int]??`), exercising the
// isOwnerGovernedMemberOptionalUnwrapSource branch of isNestedOwnerGovernedUnwrapSource
// (the ident-source tests above only hit isIdentOptionalUnwrapSource). Holder's
// drop recursively frees the nested-optional field, so the `!!`-extracted inner is
// an alias and must flow straight into the lock receiver with no owned-temp store.
func TestT1215MemberSourceDoubleForceNoOwnedTemp(t *testing.T) {
	ir := generateIR(t, `
		type Holder {
			Mutex[int]?? m;
			drop(~this) {}
		}
		demo() {
			Mutex[int]? inner = Mutex[int](5);
			Holder h = Holder(m: move inner);
			int v = h.m!!.lock().borrow;
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if !t1215NativeDirectUse.MatchString(fn) {
		t.Fatalf("expected the member-source `h.m!!` force result to be consumed directly as the lock receiver (no owned-temp store between extract and use):\n%s", fn)
	}
	if t1215OwnedTempStore.MatchString(fn) {
		t.Fatalf("member-source `h.m!!` extracted inner Mutex must NOT be registered as an owned statement temp (Holder's drop already frees the nested-optional field):\n%s", fn)
	}
}

// Fall-through guard: when the outer force's base is NOT owner-governed (a call
// result returning `Mutex[int]??`), isNestedOwnerGovernedUnwrapSource must return
// false so the extracted inner IS tracked as an owned statement temp — the call
// transferred ownership and nothing else drops it, so skipping the temp would leak.
// This pins the negative side of the guard: the owned-temp store MUST be present.
func TestT1215CallResultDoubleForceKeepsOwnedTemp(t *testing.T) {
	ir := generateIR(t, `
		make_nested() Mutex[int]?? {
			Mutex[int]? inner = Mutex[int](5);
			return inner;
		}
		demo() {
			int v = make_nested()!!.lock().borrow;
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if !t1215OwnedTempStore.MatchString(fn) {
		t.Fatalf("call-result `make_nested()!!` transfers ownership; the extracted inner MUST be tracked as an owned statement temp (else a leak), so the owned-temp store must appear:\n%s", fn)
	}
}

// Regression guard for the TRANSFER case: binding the force to a local
// (`Mutex[int] g = r!!`) DOES neutralize `r`'s drop (single owner), so `g` legitimately
// owns and drops the handle exactly once. The double-force-source guard must not
// perturb this path — the bound local still takes an owning drop.
func TestT1215BoundDoubleForceStillOwns(t *testing.T) {
	ir := generateIR(t, `
		demo() {
			Mutex[int]? inner = Mutex[int](5);
			Mutex[int]?? r = inner;
			Mutex[int] g = r!!;
			int v = g.lock().borrow;
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	// The bound `g` owns the handle; its scope-exit drop must still be emitted.
	if !regexp.MustCompile(`call void @"Mutex\[int\].drop"`).MatchString(fn) {
		t.Fatalf("bound `Mutex[int] g = r!!` must still emit an owning Mutex drop:\n%s", fn)
	}
}
