package sema

import "testing"

// T1315: storing a generator (`stream[T]`) value in a container element, struct
// or enum field, or other aggregate storage site segfaulted at runtime — the
// raw coroutine {handle, slot} value was placed into a droppable aggregate and
// its drop routed through structural-drop, calling a garbage vtable pointer
// (same root cause as T1313). A stream is now a non-storable type: sema rejects
// it in every storage position cleanly.

// All T1315 diagnostics share this prefix; the suffix names the specific site.
const t1315StoreMsg = "a generator value cannot be stored"

// gen is the shared generator factory used by every case below.
const t1315Gen = `
	gen(int n) stream[int] {
		int i = 0;
		while i < n { yield i; i = i + 1; }
	}
`

func TestT1315RejectVectorLiteralElement(t *testing.T) {
	errs := checkErrs(t, t1315Gen+`
		main() { v := [gen(3)]; }
	`)
	expectError(t, errs, t1315StoreMsg)
}

func TestT1315RejectStructField(t *testing.T) {
	errs := checkErrs(t, t1315Gen+`
		type Holder { stream[int] s; }
		main() {}
	`)
	expectError(t, errs, t1315StoreMsg)
}

func TestT1315RejectExplicitVectorAnnotation(t *testing.T) {
	// `stream[int][]` — the vector sugar bypasses resolveInstance, so the
	// SliceTypeRef path must reject the non-storable element type.
	errs := checkErrs(t, t1315Gen+`
		main() { stream[int][] v = []; }
	`)
	expectError(t, errs, t1315StoreMsg)
}

func TestT1315RejectExplicitGenericAnnotation(t *testing.T) {
	// `Box[stream[int]]` — a stream as an explicit generic type argument.
	errs := checkErrs(t, t1315Gen+`
		type Box[T] { T x; }
		main() { Box[stream[int]] b = Box(x: 0); }
	`)
	expectError(t, errs, t1315StoreMsg)
}

func TestT1315RejectOptionalField(t *testing.T) {
	// `stream[int]?` field — Optional-of-stream, caught by firstStream recursion.
	errs := checkErrs(t, t1315Gen+`
		type Opt { stream[int]? s; }
		main() {}
	`)
	expectError(t, errs, t1315StoreMsg)
}

func TestT1315RejectEnumVariantField(t *testing.T) {
	errs := checkErrs(t, t1315Gen+`
		enum E { Wrap(stream[int] s), Nil }
		main() {}
	`)
	expectError(t, errs, t1315StoreMsg)
}

func TestT1315RejectUninitializedStreamVar(t *testing.T) {
	// `stream[int] s;` (no initializer) — the §6 annotation check, not T1313's
	// value-flow check, must reject this.
	errs := checkErrs(t, t1315Gen+`
		main() { stream[int] s; }
	`)
	expectError(t, errs, t1315StoreMsg)
}

func TestT1315RejectOptionalStreamVar(t *testing.T) {
	// `stream[int]? s;` — an optional-of-stream local holds a non-storable type
	// even as none; reject it rather than leaving a latent hole.
	errs := checkErrs(t, t1315Gen+`
		main() { stream[int]? s; }
	`)
	expectError(t, errs, t1315StoreMsg)
}

func TestT1315RejectMapAnnotationValue(t *testing.T) {
	// A map value of stream type declared with an explicit `map[K, V]`
	// annotation — the resolveNamedType path (rejectStreamTypeArg on the map's
	// type args), distinct from the map-literal value-flow path below.
	errs := checkErrs(t, t1315Gen+`
		main() { map[string, stream[int]] m = []; }
	`)
	expectError(t, errs, t1315StoreMsg)
}

func TestT1315RejectMapLiteralValue(t *testing.T) {
	// A map value of stream type inferred from a brace map literal
	// (`{"a": gen(3)}`). NewMap bypasses resolveInstance, so checkMapLit checks
	// the value type directly — this is the value-flow path, not the annotation
	// path. (Map literals use braces; the bracket form `["a": ...]` parses as a
	// vector and hits a separate parser predicate error.)
	errs := checkErrs(t, t1315Gen+`
		main() { m := {"a": gen(3)}; }
	`)
	expectError(t, errs, t1315StoreMsg)
}

func TestT1315RejectMapLiteralKey(t *testing.T) {
	// A stream as a brace-map-literal key (`{gen(3): 1}`) — exercises
	// checkMapLit's keyType stream branch. (A stream also trips the Hashable/
	// Equal key constraints; the store diagnostic must still be among them.)
	errs := checkErrs(t, t1315Gen+`
		main() { m := {gen(3): 1}; }
	`)
	expectError(t, errs, t1315StoreMsg)
}

func TestT1315RejectTupleLiteralElement(t *testing.T) {
	// `(gen(3), 1)` builds a droppable tuple aggregate holding a raw
	// {handle, slot} pair. The tuple literal is a value-flow site (NewTuple
	// bypasses resolveInstance), so checkTupleLit must reject the element —
	// otherwise binding or discarding the tuple segfaults on structural-drop.
	errs := checkErrs(t, t1315Gen+`
		main() { t := (gen(3), 1); }
	`)
	expectError(t, errs, t1315StoreMsg)
}

func TestT1315RejectTupleReturnLiteral(t *testing.T) {
	// A tuple literal holding a stream, returned by value, is the same
	// non-storable aggregate — reject it at the return expression.
	errs := checkErrs(t, t1315Gen+`
		mk() (stream[int], int) { return (gen(3), 1); }
		main() {}
	`)
	expectError(t, errs, t1315StoreMsg)
}

func TestT1315RejectTupleTypeField(t *testing.T) {
	// `(stream[int], int)` field — a tuple type annotation is not routed through
	// resolveInstance, so firstStream's Tuple recursion (not the value-flow
	// checkTupleLit path) must catch the nested stream at defineField.
	errs := checkErrs(t, t1315Gen+`
		type H { (stream[int], int) t; }
		main() {}
	`)
	expectError(t, errs, t1315StoreMsg)
}

func TestT1315RejectTupleTypeVar(t *testing.T) {
	// `(stream[int], int) t;` local — the annotated variable type is a tuple, so
	// checkTypedVarDecl's firstStream must recurse into the tuple elements.
	errs := checkErrs(t, t1315Gen+`
		main() { (stream[int], int) t; }
	`)
	expectError(t, errs, t1315StoreMsg)
}

func TestT1315RejectFixedArrayStreamField(t *testing.T) {
	// `stream[int][3]` field — a fixed-size array of stream. Exercises
	// firstStream's Array recursion; the element is also rejected at
	// resolveType(ArrayTypeRef), so at least one store diagnostic is guaranteed.
	errs := checkErrs(t, t1315Gen+`
		type H { stream[int][3] a; }
		main() {}
	`)
	expectError(t, errs, t1315StoreMsg)
}

// --- Negatives: legitimate stream uses must still compile (no over-rejection) ---

func TestT1315GeneratorReturnTypeAccepted(t *testing.T) {
	checkOK(t, t1315Gen+`
		main() {}
	`)
}

func TestT1315StreamParameterRejected(t *testing.T) {
	// T1314 (landed after T1315): a `stream[int]` parameter is now rejected in
	// sema — a parameter is a stored binding, and an unconsumed stream param
	// leaks its coroutine frame + yield slot. A generator must be consumed at the
	// call site (for-in) or collected into a vector, not taken as a parameter.
	errs := checkErrs(t, t1315Gen+`
		consume(stream[int] s) int {
			int total = 0;
			for x in s { total = total + x; }
			return total;
		}
		main() {}
	`)
	expectError(t, errs, t1314ParamMsg)
}

func TestT1315InlineForInAccepted(t *testing.T) {
	checkOK(t, t1315Gen+`
		main() {
			int sum = 0;
			for x in gen(3) { sum = sum + x; }
		}
	`)
}

func TestT1315NonStreamContainerNotRejected(t *testing.T) {
	// A vector of a plain type must NOT trip the stream check.
	errs := checkErrs(t, `
		main() { v := [1, 2, 3]; }
	`)
	expectNoErrorContaining(t, errs, t1315StoreMsg)
}
