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
