package sema

import (
	"sync"

	"djabi.dev/go/promise_lang/internal/types"
)

var builtinsOnce sync.Once

// initBuiltins populates the built-in Named types with their native operator methods.
// This is called once before checking user code. All operator methods are marked native.
func initBuiltins() {
	builtinsOnce.Do(func() {
		// Numeric types: arithmetic + comparison + unary negate
		numericTypes := []*types.Named{
			types.TypInt,
			types.TypI8, types.TypI16, types.TypI32, types.TypI64,
			types.TypUint,
			types.TypU8, types.TypU16, types.TypU32, types.TypU64,
			types.TypF32, types.TypF64,
		}

		arithOps := []string{"+", "-", "*", "/", "%"}
		cmpOps := []string{"==", "!=", "<", ">", "<=", ">="}

		for _, nt := range numericTypes {
			// Binary arithmetic: T.op(T) -> T
			for _, op := range arithOps {
				addBinaryOp(nt, op, nt, nt)
			}
			// Binary comparison: T.op(T) -> bool
			for _, op := range cmpOps {
				addBinaryOp(nt, op, nt, types.TypBool)
			}
			// Unary negate: T.-() -> T
			addUnaryOp(nt, "-", nt)
		}

		// bool: logical + equality + unary not
		addBinaryOp(types.TypBool, "&&", types.TypBool, types.TypBool)
		addBinaryOp(types.TypBool, "||", types.TypBool, types.TypBool)
		addBinaryOp(types.TypBool, "==", types.TypBool, types.TypBool)
		addBinaryOp(types.TypBool, "!=", types.TypBool, types.TypBool)
		addUnaryOp(types.TypBool, "!", types.TypBool)

		// string: concatenation + equality + comparison
		addBinaryOp(types.TypString, "+", types.TypString, types.TypString)
		addBinaryOp(types.TypString, "==", types.TypString, types.TypBool)
		addBinaryOp(types.TypString, "!=", types.TypString, types.TypBool)
		addBinaryOp(types.TypString, "<", types.TypString, types.TypBool)
		addBinaryOp(types.TypString, ">", types.TypString, types.TypBool)
		addBinaryOp(types.TypString, "<=", types.TypString, types.TypBool)
		addBinaryOp(types.TypString, ">=", types.TypString, types.TypBool)

		// char: equality + comparison
		addBinaryOp(types.TypChar, "==", types.TypChar, types.TypBool)
		addBinaryOp(types.TypChar, "!=", types.TypChar, types.TypBool)
		addBinaryOp(types.TypChar, "<", types.TypChar, types.TypBool)
		addBinaryOp(types.TypChar, ">", types.TypChar, types.TypBool)
		addBinaryOp(types.TypChar, "<=", types.TypChar, types.TypBool)
		addBinaryOp(types.TypChar, ">=", types.TypChar, types.TypBool)

		// Container len fields and native methods
		populateContainerTypes()

		// Iter[T] and Stream[T] abstract methods
		populateIterStream()
	})
}

// addBinaryOp adds a native binary operator method to a Named type.
// sig: recv.op(param) -> result
func addBinaryOp(recv *types.Named, op string, paramType, resultType *types.Named) {
	recv.AddMethod(types.NewMethod(
		types.Pos{},
		op,
		types.NewSignature(
			types.NewParam("this", recv, types.RefNone),
			[]*types.Param{types.NewParam("other", paramType, types.RefNone)},
			resultType,
			false,
		),
		types.PlaceValue,
		false, // not abstract
		true,  // native
	))
}

// addUnaryOp adds a native unary operator method to a Named type.
// sig: recv.op() -> result
func addUnaryOp(recv *types.Named, op string, resultType *types.Named) {
	recv.AddMethod(types.NewMethod(
		types.Pos{},
		op,
		types.NewSignature(
			types.NewParam("this", recv, types.RefNone),
			nil,
			resultType,
			false,
		),
		types.PlaceValue,
		false, // not abstract
		true,  // native
	))
}

// addNativeMethod adds a native method to a Named type.
func addNativeMethod(recv *types.Named, name string, sig *types.Signature) {
	recv.AddMethod(types.NewMethod(types.Pos{}, name, sig, types.PlaceInstance, false, true))
}

// populateContainerTypes registers len fields and native methods on Slice, Map, and string.
func populateContainerTypes() {
	// Slice[T] — len field
	sliceT := types.TypSlice.TypeParams()[0] // T
	types.TypSlice.AddField(types.NewField(types.Pos{}, "len", types.TypInt, types.PlaceValue, false, false))

	// Slice[T] methods
	addNativeMethod(types.TypSlice, "push", types.NewSignature(
		types.NewParam("this", types.TypSlice, types.RefNone),
		[]*types.Param{types.NewParam("elem", sliceT, types.RefNone)},
		types.TypVoid, false))
	addNativeMethod(types.TypSlice, "pop", types.NewSignature(
		types.NewParam("this", types.TypSlice, types.RefNone),
		nil,
		types.NewOptional(sliceT), false))
	addNativeMethod(types.TypSlice, "contains", types.NewSignature(
		types.NewParam("this", types.TypSlice, types.RefNone),
		[]*types.Param{types.NewParam("elem", sliceT, types.RefNone)},
		types.TypBool, false))
	addNativeMethod(types.TypSlice, "remove", types.NewSignature(
		types.NewParam("this", types.TypSlice, types.RefNone),
		[]*types.Param{types.NewParam("index", types.TypInt, types.RefNone)},
		types.TypVoid, false))

	// Map[K, V] — len field
	mapK := types.TypMap.TypeParams()[0] // K
	mapV := types.TypMap.TypeParams()[1] // V
	types.TypMap.AddField(types.NewField(types.Pos{}, "len", types.TypInt, types.PlaceValue, false, false))

	// Map[K, V] methods
	addNativeMethod(types.TypMap, "contains", types.NewSignature(
		types.NewParam("this", types.TypMap, types.RefNone),
		[]*types.Param{types.NewParam("key", mapK, types.RefNone)},
		types.TypBool, false))
	addNativeMethod(types.TypMap, "remove", types.NewSignature(
		types.NewParam("this", types.TypMap, types.RefNone),
		[]*types.Param{types.NewParam("key", mapK, types.RefNone)},
		types.TypBool, false))
	addNativeMethod(types.TypMap, "keys", types.NewSignature(
		types.NewParam("this", types.TypMap, types.RefNone),
		nil,
		types.NewSlice(mapK), false))
	addNativeMethod(types.TypMap, "values", types.NewSignature(
		types.NewParam("this", types.TypMap, types.RefNone),
		nil,
		types.NewSlice(mapV), false))

	// string — len field
	types.TypString.AddField(types.NewField(types.Pos{}, "len", types.TypInt, types.PlaceValue, false, false))

	// string methods
	addNativeMethod(types.TypString, "contains", types.NewSignature(
		types.NewParam("this", types.TypString, types.RefNone),
		[]*types.Param{types.NewParam("sub", types.TypString, types.RefNone)},
		types.TypBool, false))
	addNativeMethod(types.TypString, "starts_with", types.NewSignature(
		types.NewParam("this", types.TypString, types.RefNone),
		[]*types.Param{types.NewParam("prefix", types.TypString, types.RefNone)},
		types.TypBool, false))
	addNativeMethod(types.TypString, "ends_with", types.NewSignature(
		types.NewParam("this", types.TypString, types.RefNone),
		[]*types.Param{types.NewParam("suffix", types.TypString, types.RefNone)},
		types.TypBool, false))
	addNativeMethod(types.TypString, "index_of", types.NewSignature(
		types.NewParam("this", types.TypString, types.RefNone),
		[]*types.Param{types.NewParam("sub", types.TypString, types.RefNone)},
		types.NewOptional(types.TypInt), false))
	addNativeMethod(types.TypString, "trim", types.NewSignature(
		types.NewParam("this", types.TypString, types.RefNone),
		nil,
		types.TypString, false))
	addNativeMethod(types.TypString, "split", types.NewSignature(
		types.NewParam("this", types.TypString, types.RefNone),
		[]*types.Param{types.NewParam("sep", types.TypString, types.RefNone)},
		types.NewSlice(types.TypString), false))
}

// populateIterStream adds abstract next() methods to Iter[T] and Stream[T].
func populateIterStream() {
	// Iter[T].next() T? — returns Optional(T)
	iterT := types.TypIter.TypeParams()[0]
	types.TypIter.AddMethod(types.NewMethod(
		types.Pos{},
		"next",
		types.NewSignature(
			types.NewParam("this", types.TypIter, types.RefNone),
			nil,
			types.NewOptional(iterT),
			false,
		),
		types.PlaceInstance,
		true,  // abstract
		false, // not native
	))

	// Stream[T].next() Task[T?] — returns Instance(Task, [Optional(T)])
	streamT := types.TypStream.TypeParams()[0]
	types.TypStream.AddMethod(types.NewMethod(
		types.Pos{},
		"next",
		types.NewSignature(
			types.NewParam("this", types.TypStream, types.RefNone),
			nil,
			types.NewInstance(types.TypTask, []types.Type{types.NewOptional(streamT)}),
			false,
		),
		types.PlaceInstance,
		true,  // abstract
		false, // not native
	))
}
