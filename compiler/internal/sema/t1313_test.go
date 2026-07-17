package sema

import "testing"

// T1313: binding a generator (`stream[T]`) value to a variable segfaulted at
// runtime because codegen routed the raw coroutine {handle, slot} value through
// the structural-drop path, calling a garbage vtable pointer. Storing a
// generator value is a deferred feature; sema now rejects it cleanly.

const t1313StoreMsg = "a generator value cannot be stored in a variable"

func TestT1313RejectInferredBind(t *testing.T) {
	errs := checkErrs(t, `
		gen(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		main() {
			s := gen(3);
		}
	`)
	expectError(t, errs, t1313StoreMsg)
}

func TestT1313RejectDiscardBind(t *testing.T) {
	errs := checkErrs(t, `
		gen(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		main() {
			_ := gen(3);
		}
	`)
	expectError(t, errs, t1313StoreMsg)
}

func TestT1313RejectTypedBind(t *testing.T) {
	errs := checkErrs(t, `
		gen(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		main() {
			stream[int] s = gen(3);
		}
	`)
	expectError(t, errs, t1313StoreMsg)
}

func TestT1313RejectReassignment(t *testing.T) {
	// A stream-typed parameter is the only way to obtain a stream lvalue after
	// this change; reassigning it must also be rejected.
	errs := checkErrs(t, `
		gen(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		take(stream[int] s) {
			s = gen(3);
		}
		main() {}
	`)
	expectError(t, errs, t1313StoreMsg)
}

func TestT1313RejectTypedDiscardBind(t *testing.T) {
	// The typed-decl path with a `_` name skips the binding-insert branch;
	// the store must still be rejected (covers the `s.Name == "_"` typed case).
	errs := checkErrs(t, `
		gen(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		main() {
			stream[int] _ = gen(3);
		}
	`)
	expectError(t, errs, t1313StoreMsg)
}

func TestT1313RejectBindThenReference(t *testing.T) {
	// The original failing form from the item: bind then bare-reference discard.
	errs := checkErrs(t, `
		gen(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		run() {
			s := gen(3);
			s;
		}
		main() {}
	`)
	expectError(t, errs, t1313StoreMsg)
}

// TestT1313InferredBindSuppressesUndefinedCascade locks in the reason the reject
// path still inserts the binding: a later reference to the rejected local must
// NOT produce a cascading "undefined variable" error on top of the store error.
func TestT1313InferredBindSuppressesUndefinedCascade(t *testing.T) {
	errs := checkErrs(t, `
		gen(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		run() {
			s := gen(3);
			int x = s + 1;
		}
		main() {}
	`)
	expectError(t, errs, t1313StoreMsg)
	// `s` is inserted, so the reference resolves — no "undefined" cascade.
	expectNoErrorContaining(t, errs, "undefined")
	expectNoErrorContaining(t, errs, "undeclared")
}

// TestT1313TypedBindSuppressesUndefinedCascade is the typed-decl counterpart:
// the typed reject path (name != "_") also inserts the binding.
func TestT1313TypedBindSuppressesUndefinedCascade(t *testing.T) {
	errs := checkErrs(t, `
		gen(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		run() {
			stream[int] s = gen(3);
			int x = s + 1;
		}
		main() {}
	`)
	expectError(t, errs, t1313StoreMsg)
	expectNoErrorContaining(t, errs, "undefined")
	expectNoErrorContaining(t, errs, "undeclared")
}

func TestT1313InlineConsumeStillAccepted(t *testing.T) {
	checkOK(t, `
		gen(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		main() {
			int sum = 0;
			for x in gen(3) { sum = sum + x; }
		}
	`)
}

func TestT1313YieldDelegationStillAccepted(t *testing.T) {
	checkOK(t, `
		gen(int n) stream[int] {
			int i = 0;
			while i < n { yield i; i = i + 1; }
		}
		wrap(int n) stream[int] {
			yield * gen(n);
		}
		main() {}
	`)
}

func TestT1313NonStreamBindNotRejected(t *testing.T) {
	// A generator whose collected result is `int[]` (not a stream) must NOT be
	// rejected — guards against over-rejection on non-stream values.
	errs := checkErrs(t, `
		collect!(int n) int[] {
			int[] out = [];
			int i = 0;
			while i < n { out.push(i); i = i + 1; }
			return out;
		}
		run!() {
			int[] r = collect(3)?!;
		}
		main() {}
	`)
	expectNoErrorContaining(t, errs, t1313StoreMsg)
}
