package sema

import "testing"

// T1350: calling `.iter()` on a generator (`stream[T]`) value compiled cleanly
// but segfaulted at runtime — codegen routed the raw coroutine {handle, slot}
// value through virtual dispatch as a structural {vtable, instance} box, reading
// the coroutine handle as a garbage vtable pointer. A generator's iterator is
// not a first-class value (deferred: B0024); sema now rejects the call cleanly,
// mirroring T1313 (store to var) / T1314 (stream param).

const t1350IterMsg = "cannot call `.iter()` on a generator value"

// gen is the shared generator factory used by the repros below.
const t1350Gen = `
	gen(int n) stream[int] {
		int i = 0;
		while i < n { yield i; i = i + 1; }
	}
`

func TestT1350RejectCollect(t *testing.T) {
	// Repro A: collect the generator-backed iterator.
	errs := checkErrs(t, t1350Gen+`
		main() {
			int[] r = gen(3).iter().collect();
		}
	`)
	expectError(t, errs, t1350IterMsg)
}

func TestT1350RejectBoundIterForIn(t *testing.T) {
	// Repro B: bind the iterator (Iterator[int], not stream) and for-in it.
	errs := checkErrs(t, t1350Gen+`
		main() {
			Iterator[int] it = gen(3).iter();
			int n = 0;
			for _ in it { n = n + 1; }
		}
	`)
	expectError(t, errs, t1350IterMsg)
	// The `return nil` on the rejected `.iter()` call must suppress cascades:
	// the Iterator[int] binding stays defined so the later for-in does not pile on.
	expectNoErrorContaining(t, errs, "undefined")
	expectNoErrorContaining(t, errs, "undeclared")
	expectNoErrorContaining(t, errs, "no method")
}

func TestT1350RejectBareCall(t *testing.T) {
	errs := checkErrs(t, t1350Gen+`
		main() {
			gen(3).iter();
		}
	`)
	expectError(t, errs, t1350IterMsg)
}

func TestT1350RejectChainedCombinator(t *testing.T) {
	errs := checkErrs(t, t1350Gen+`
		main() {
			int[] r = gen(3).iter().take(2).collect();
		}
	`)
	expectError(t, errs, t1350IterMsg)
}

func TestT1350AcceptGeneratorForIn(t *testing.T) {
	// The working path — direct for-in consumption — must stay accepted.
	checkOK(t, t1350Gen+`
		main() {
			int sum = 0;
			for x in gen(3) { sum = sum + x; }
		}
	`)
}

func TestT1350AcceptYieldDelegate(t *testing.T) {
	// yield * over a generator must stay accepted (does not call `.iter()`).
	checkOK(t, t1350Gen+`
		gen2(int n) stream[int] {
			yield * gen(n);
		}
		main() {
			int sum = 0;
			for x in gen2(3) { sum = sum + x; }
		}
	`)
}

func TestT1350AcceptVectorIter(t *testing.T) {
	// The critical non-stream boundary: `.iter()` on a Vector receiver must be
	// untouched (AsStream matches only stream[T], not Vector/Range/Iterator).
	checkOK(t, `
		main() {
			int[] v = [1, 2, 3];
			int[] r = v.iter().collect();
		}
	`)
}

func TestT1350AcceptVectorIterZip(t *testing.T) {
	checkOK(t, `
		main() {
			int[] a = [1, 2, 3];
			int[] b = [4, 5, 6];
			for pair in a.iter().zip[int](b.iter()) { }
		}
	`)
}

func TestT1350AcceptUserTypeIterMethod(t *testing.T) {
	// Guard against over-rejection: the reject keys off AsStream (only stream[T]
	// Instances), NOT the method name. A user type with its own method returning
	// a collection must stay accepted.
	checkOK(t, `
		type Box {
			int v;
			items() int[] { return [this.v]; }
		}
		main() {
			b := Box(v: 7);
			int[] r = b.items();
		}
	`)
}

func TestT1350AcceptGenericStreamRejectedForUserElem(t *testing.T) {
	// The reject generalizes over the stream element type — a generator of a
	// user type is rejected the same as stream[int].
	errs := checkErrs(t, `
		type Item { int v; }
		gitems(int n) stream[Item] {
			int i = 0;
			while i < n { yield Item(v: i); i = i + 1; }
		}
		main() {
			gitems(3).iter().collect();
		}
	`)
	expectError(t, errs, t1350IterMsg)
}
