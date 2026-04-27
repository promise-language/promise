package types

import "testing"

func makeTP(name string, index int) *TypeParam {
	tn := NewTypeName(Pos{}, name, nil)
	return NewTypeParam(tn, nil, index)
}

func TestSubstTypeParam(t *testing.T) {
	tp := makeTP("T", 0)
	subst := map[*TypeParam]Type{tp: TypInt}
	result := Substitute(tp, subst)
	if result != TypInt {
		t.Errorf("expected int, got %s", result)
	}
}

func TestSubstOptional(t *testing.T) {
	tp := makeTP("T", 0)
	opt := NewOptional(tp)
	subst := map[*TypeParam]Type{tp: TypString}
	result := Substitute(opt, subst)
	ropt, ok := result.(*Optional)
	if !ok {
		t.Fatalf("expected Optional, got %T", result)
	}
	if ropt.Elem() != TypString {
		t.Errorf("expected string?, got %s", result)
	}
}

func TestSubstSlice(t *testing.T) {
	tp := makeTP("T", 0)
	sl := NewVector(tp)
	subst := map[*TypeParam]Type{tp: TypInt}
	result := Substitute(sl, subst)
	elem, ok := AsVector(result)
	if !ok {
		t.Fatalf("expected Vector instance, got %T", result)
	}
	if elem != TypInt {
		t.Errorf("expected int[], got %s", result)
	}
}

func TestSubstArray(t *testing.T) {
	tp := makeTP("T", 0)
	arr := NewArray(tp, 5)
	subst := map[*TypeParam]Type{tp: TypBool}
	result := Substitute(arr, subst)
	rarr, ok := result.(*Array)
	if !ok {
		t.Fatalf("expected Array, got %T", result)
	}
	if rarr.Elem() != TypBool || rarr.Size() != 5 {
		t.Errorf("expected bool[5], got %s", result)
	}
}

func TestSubstTuple(t *testing.T) {
	tpK := makeTP("K", 0)
	tpV := makeTP("V", 1)
	tup := NewTuple([]Type{tpK, tpV})
	subst := map[*TypeParam]Type{tpK: TypString, tpV: TypInt}
	result := Substitute(tup, subst)
	rtup, ok := result.(*Tuple)
	if !ok {
		t.Fatalf("expected Tuple, got %T", result)
	}
	if rtup.Elems()[0] != TypString || rtup.Elems()[1] != TypInt {
		t.Errorf("expected (string, int), got %s", result)
	}
}

func TestSubstSignature(t *testing.T) {
	tp := makeTP("T", 0)
	sig := NewSignature(nil, []*Param{NewParam("x", tp, RefNone)}, tp, false)
	subst := map[*TypeParam]Type{tp: TypInt}
	result := Substitute(sig, subst)
	rsig, ok := result.(*Signature)
	if !ok {
		t.Fatalf("expected Signature, got %T", result)
	}
	if rsig.Params()[0].Type() != TypInt {
		t.Errorf("expected param type int, got %s", rsig.Params()[0].Type())
	}
	if rsig.Result() != TypInt {
		t.Errorf("expected result type int, got %s", rsig.Result())
	}
}

func TestSubstSignaturePreservesCanError(t *testing.T) {
	tp := makeTP("T", 0)
	sig := NewSignature(nil, []*Param{NewParam("x", tp, RefNone)}, tp, true)
	subst := map[*TypeParam]Type{tp: TypString}
	result := Substitute(sig, subst).(*Signature)
	if !result.CanError() {
		t.Error("expected canError to be preserved")
	}
}

func TestSubstInstance(t *testing.T) {
	tp := makeTP("T", 0)
	// Instance(task, [T]) → Instance(task, [int])
	inst := NewInstance(TypTask, []Type{tp})
	subst := map[*TypeParam]Type{tp: TypInt}
	result := Substitute(inst, subst)
	rinst, ok := result.(*Instance)
	if !ok {
		t.Fatalf("expected Instance, got %T", result)
	}
	if rinst.Origin() != TypTask {
		t.Errorf("expected origin task, got %s", rinst.Origin())
	}
	if rinst.TypeArgs()[0] != TypInt {
		t.Errorf("expected type arg int, got %s", rinst.TypeArgs()[0])
	}
}

func TestSubstNestedGenerics(t *testing.T) {
	tp := makeTP("T", 0)
	// Map[T, Slice(T)] → Map[int, Slice(int)]
	innerSlice := NewVector(tp)
	m := NewMap(tp, innerSlice)
	subst := map[*TypeParam]Type{tp: TypInt}
	result := Substitute(m, subst)
	rKey, rVal, ok := AsMap(result)
	if !ok {
		t.Fatalf("expected Map instance, got %T", result)
	}
	if rKey != TypInt {
		t.Errorf("expected key int, got %s", rKey)
	}
	rslElem, ok := AsVector(rVal)
	if !ok {
		t.Fatalf("expected val Slice instance, got %T", rVal)
	}
	if rslElem != TypInt {
		t.Errorf("expected slice elem int, got %s", rslElem)
	}
}

func TestSubstSharedRef(t *testing.T) {
	tp := makeTP("T", 0)
	ref := NewSharedRef(tp)
	subst := map[*TypeParam]Type{tp: TypString}
	result := Substitute(ref, subst)
	rref, ok := result.(*SharedRef)
	if !ok {
		t.Fatalf("expected SharedRef, got %T", result)
	}
	if rref.Elem() != TypString {
		t.Errorf("expected string&, got %s", result)
	}
}

func TestSubstMutRef(t *testing.T) {
	tp := makeTP("T", 0)
	ref := NewMutRef(tp)
	subst := map[*TypeParam]Type{tp: TypInt}
	result := Substitute(ref, subst)
	rref, ok := result.(*MutRef)
	if !ok {
		t.Fatalf("expected MutRef, got %T", result)
	}
	if rref.Elem() != TypInt {
		t.Errorf("expected int~, got %s", result)
	}
}

func TestSubstPointer(t *testing.T) {
	tp := makeTP("T", 0)
	ptr := NewPointer(tp)
	subst := map[*TypeParam]Type{tp: TypBool}
	result := Substitute(ptr, subst)
	rptr, ok := result.(*Pointer)
	if !ok {
		t.Fatalf("expected Pointer, got %T", result)
	}
	if rptr.Elem() != TypBool {
		t.Errorf("expected bool*, got %s", result)
	}
}

func TestSubstNoOpReturnsSamePointer(t *testing.T) {
	// Named types should be returned unchanged
	result := Substitute(TypInt, map[*TypeParam]Type{makeTP("T", 0): TypString})
	if result != TypInt {
		t.Error("expected same pointer for Named type")
	}

	// Optional(int) with no matching TypeParam
	opt := NewOptional(TypInt)
	result = Substitute(opt, map[*TypeParam]Type{makeTP("T", 0): TypString})
	if result != opt {
		t.Error("expected same pointer when no substitution applies")
	}
}

func TestSubstNilType(t *testing.T) {
	result := Substitute(nil, map[*TypeParam]Type{makeTP("T", 0): TypInt})
	if result != nil {
		t.Error("expected nil for nil input")
	}
}

func TestSubstEmptyMap(t *testing.T) {
	tp := makeTP("T", 0)
	result := Substitute(tp, nil)
	if result != tp {
		t.Error("expected unchanged TypeParam with nil subst map")
	}
}

func TestSubstSignatureWithRecv(t *testing.T) {
	tp := makeTP("T", 0)
	recv := NewParam("this", tp, RefNone)
	sig := NewSignature(recv, []*Param{NewParam("x", TypInt, RefNone)}, tp, false)
	subst := map[*TypeParam]Type{tp: TypString}
	result := Substitute(sig, subst).(*Signature)
	if result.Recv().Type() != TypString {
		t.Errorf("expected recv type string, got %s", result.Recv().Type())
	}
	if result.Result() != TypString {
		t.Errorf("expected result type string, got %s", result.Result())
	}
	// Param was int (no TypeParam), should be unchanged
	if result.Params()[0].Type() != TypInt {
		t.Errorf("expected param type int, got %s", result.Params()[0].Type())
	}
}

func TestBuildSubstMap(t *testing.T) {
	tp1 := makeTP("K", 0)
	tp2 := makeTP("V", 1)
	m := BuildSubstMap([]*TypeParam{tp1, tp2}, []Type{TypString, TypInt})
	if m[tp1] != TypString {
		t.Errorf("expected K → string")
	}
	if m[tp2] != TypInt {
		t.Errorf("expected V → int")
	}
}

func TestBuildSubstMapMismatch(t *testing.T) {
	tp1 := makeTP("T", 0)
	m := BuildSubstMap([]*TypeParam{tp1}, []Type{TypString, TypInt})
	if m != nil {
		t.Error("expected nil for mismatched lengths")
	}
}
