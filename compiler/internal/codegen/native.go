package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// nativeEmitter generates LLVM IR for a native operator method.
// left is the receiver value; right is the operand (nil for unary operations).
type nativeEmitter func(b *ir.Block, left, right value.Value) value.Value

// nativeOps maps (TypeCategory, operator name) → LLVM instruction emitter.
// This table IS the backend's implementation of the `native` annotation contract.
var nativeOps map[TypeCategory]map[string]nativeEmitter

func init() {
	nativeOps = map[TypeCategory]map[string]nativeEmitter{
		CatSignedInt:   signedIntOps(),
		CatUnsignedInt: unsignedIntOps(),
		CatFloat:       floatOps(),
		CatBool:        boolOps(),
	}
}

func signedIntOps() map[string]nativeEmitter {
	return map[string]nativeEmitter{
		"+": func(b *ir.Block, l, r value.Value) value.Value { return b.NewAdd(l, r) },
		"-": binaryOrUnary(
			func(b *ir.Block, l, r value.Value) value.Value { return b.NewSub(l, r) },
			func(b *ir.Block, l, _ value.Value) value.Value {
				zero := constant.NewInt(l.Type().(*irtypes.IntType), 0)
				return b.NewSub(zero, l)
			},
		),
		"*":  func(b *ir.Block, l, r value.Value) value.Value { return b.NewMul(l, r) },
		"/":  func(b *ir.Block, l, r value.Value) value.Value { return b.NewSDiv(l, r) },
		"%":  func(b *ir.Block, l, r value.Value) value.Value { return b.NewSRem(l, r) },
		"==": func(b *ir.Block, l, r value.Value) value.Value { return b.NewICmp(enum.IPredEQ, l, r) },
		"!=": func(b *ir.Block, l, r value.Value) value.Value { return b.NewICmp(enum.IPredNE, l, r) },
		"<":  func(b *ir.Block, l, r value.Value) value.Value { return b.NewICmp(enum.IPredSLT, l, r) },
		">":  func(b *ir.Block, l, r value.Value) value.Value { return b.NewICmp(enum.IPredSGT, l, r) },
		"<=": func(b *ir.Block, l, r value.Value) value.Value { return b.NewICmp(enum.IPredSLE, l, r) },
		">=": func(b *ir.Block, l, r value.Value) value.Value { return b.NewICmp(enum.IPredSGE, l, r) },
	}
}

func unsignedIntOps() map[string]nativeEmitter {
	return map[string]nativeEmitter{
		"+": func(b *ir.Block, l, r value.Value) value.Value { return b.NewAdd(l, r) },
		"-": binaryOrUnary(
			func(b *ir.Block, l, r value.Value) value.Value { return b.NewSub(l, r) },
			func(b *ir.Block, l, _ value.Value) value.Value {
				zero := constant.NewInt(l.Type().(*irtypes.IntType), 0)
				return b.NewSub(zero, l)
			},
		),
		"*":  func(b *ir.Block, l, r value.Value) value.Value { return b.NewMul(l, r) },
		"/":  func(b *ir.Block, l, r value.Value) value.Value { return b.NewUDiv(l, r) },
		"%":  func(b *ir.Block, l, r value.Value) value.Value { return b.NewURem(l, r) },
		"==": func(b *ir.Block, l, r value.Value) value.Value { return b.NewICmp(enum.IPredEQ, l, r) },
		"!=": func(b *ir.Block, l, r value.Value) value.Value { return b.NewICmp(enum.IPredNE, l, r) },
		"<":  func(b *ir.Block, l, r value.Value) value.Value { return b.NewICmp(enum.IPredULT, l, r) },
		">":  func(b *ir.Block, l, r value.Value) value.Value { return b.NewICmp(enum.IPredUGT, l, r) },
		"<=": func(b *ir.Block, l, r value.Value) value.Value { return b.NewICmp(enum.IPredULE, l, r) },
		">=": func(b *ir.Block, l, r value.Value) value.Value { return b.NewICmp(enum.IPredUGE, l, r) },
	}
}

func floatOps() map[string]nativeEmitter {
	return map[string]nativeEmitter{
		"+": func(b *ir.Block, l, r value.Value) value.Value { return b.NewFAdd(l, r) },
		"-": binaryOrUnary(
			func(b *ir.Block, l, r value.Value) value.Value { return b.NewFSub(l, r) },
			func(b *ir.Block, l, _ value.Value) value.Value { return b.NewFNeg(l) },
		),
		"*":  func(b *ir.Block, l, r value.Value) value.Value { return b.NewFMul(l, r) },
		"/":  func(b *ir.Block, l, r value.Value) value.Value { return b.NewFDiv(l, r) },
		"%":  func(b *ir.Block, l, r value.Value) value.Value { return b.NewFRem(l, r) },
		"==": func(b *ir.Block, l, r value.Value) value.Value { return b.NewFCmp(enum.FPredOEQ, l, r) },
		"!=": func(b *ir.Block, l, r value.Value) value.Value { return b.NewFCmp(enum.FPredONE, l, r) },
		"<":  func(b *ir.Block, l, r value.Value) value.Value { return b.NewFCmp(enum.FPredOLT, l, r) },
		">":  func(b *ir.Block, l, r value.Value) value.Value { return b.NewFCmp(enum.FPredOGT, l, r) },
		"<=": func(b *ir.Block, l, r value.Value) value.Value { return b.NewFCmp(enum.FPredOLE, l, r) },
		">=": func(b *ir.Block, l, r value.Value) value.Value { return b.NewFCmp(enum.FPredOGE, l, r) },
	}
}

func boolOps() map[string]nativeEmitter {
	return map[string]nativeEmitter{
		"==": func(b *ir.Block, l, r value.Value) value.Value { return b.NewICmp(enum.IPredEQ, l, r) },
		"!=": func(b *ir.Block, l, r value.Value) value.Value { return b.NewICmp(enum.IPredNE, l, r) },
		"!": func(b *ir.Block, l, _ value.Value) value.Value {
			return b.NewXor(l, constant.NewInt(irtypes.I1, 1))
		},
	}
}

// binaryOrUnary wraps two emitters for an operator that has both binary and unary forms.
// When right is nil, the unary emitter is called.
func binaryOrUnary(binary, unary nativeEmitter) nativeEmitter {
	return func(b *ir.Block, left, right value.Value) value.Value {
		if right == nil {
			return unary(b, left, right)
		}
		return binary(b, left, right)
	}
}

// lookupNativeOp finds the native emitter for (category, operator).
// Returns nil if no native implementation exists.
func lookupNativeOp(cat TypeCategory, op string) nativeEmitter {
	catOps, ok := nativeOps[cat]
	if !ok {
		return nil
	}
	return catOps[op]
}
