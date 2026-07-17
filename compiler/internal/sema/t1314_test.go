package sema

import "testing"

// T1314: passing a generator (`stream[T]`) value as a function parameter and
// never consuming it in the callee leaks the coroutine frame + yield slot.
// Nothing registers a generator cleanup binding for an incoming stream param.
// Rather than build move-tracking through for-in consumption, sema now rejects
// `stream[T]` as a parameter type — consistent with T1313's stance that a
// generator value cannot be stored (a parameter is a stored binding). Storing
// generator values ("first-class generators") is a deferred feature.

const t1314ParamMsg = "cannot be a function parameter"

func TestT1314RejectFuncStreamParam(t *testing.T) {
	errs := checkErrs(t, `
		gen(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		ignore(stream[int] s) {}
		main() {}
	`)
	expectError(t, errs, t1314ParamMsg)
}

func TestT1314RejectMethodStreamParam(t *testing.T) {
	errs := checkErrs(t, `
		gen(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		type Box {
			int x;
			take(this, stream[int] s) {}
		}
		main() {}
	`)
	expectError(t, errs, t1314ParamMsg)
}

func TestT1314RejectEnumMethodStreamParam(t *testing.T) {
	errs := checkErrs(t, `
		gen(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		enum Color {
			red,
			green,
			take(this, stream[int] s) {}
		}
		main() {}
	`)
	expectError(t, errs, t1314ParamMsg)
}

func TestT1314RejectLambdaStreamParam(t *testing.T) {
	errs := checkErrs(t, `
		gen(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		main() {
			f := |stream[int] s| { int x = 0; };
		}
	`)
	expectError(t, errs, t1314ParamMsg)
}

func TestT1314RejectVariadicStreamParam(t *testing.T) {
	errs := checkErrs(t, `
		gen(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		f(...stream[int] s) {}
		main() {}
	`)
	expectError(t, errs, t1314ParamMsg)
}

func TestT1314StreamReturnStillAccepted(t *testing.T) {
	// A `stream[T]` return type is required for every generator factory and must
	// stay legal — guard against over-rejection of stream return types.
	checkOK(t, `
		gen(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		main() {}
	`)
}

func TestT1314InlineConsumeStillAccepted(t *testing.T) {
	// Inline consumption via for-in and `yield *` remains the supported way to
	// use a generator value — these must not be rejected.
	checkOK(t, `
		gen(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		wrap(int n) stream[int] {
			yield * gen(n);
		}
		main() {
			int sum = 0;
			for x in gen(3) { sum = sum + x; }
		}
	`)
}

func TestT1314IteratorParamStillAccepted(t *testing.T) {
	// The rejection must be narrow: only the raw generator (`stream[T]`) type is
	// a leaking coroutine value. The structural `Iterator[T]` interface — the
	// intended way to pass a lazy sequence across a call boundary — must stay
	// legal, guarding against a future broadening of AsStream.
	checkOK(t, `
		take(Iterator[int] it) {}
		main() {}
	`)
}

func TestT1314CapitalStreamParamAlsoRejected(t *testing.T) {
	// `Stream[T]` (capital) and `stream[T]` denote the same generator type
	// (origin TypStream), so the capitalized spelling is rejected identically —
	// no back door around the T1314 rejection.
	errs := checkErrs(t, `
		take(Stream[int] s) {}
		main() {}
	`)
	expectError(t, errs, t1314ParamMsg)
}
