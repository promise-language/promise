package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"djabi.dev/go/promise_lang/internal/types"
)

// Failable extern bridge helpers.
//
// Failable externs write their result in internal form: {i1 is_error, T value, i8* error_ptr}.
// This matches the result struct of regular failable functions, so genExternCall can return
// the loaded result directly without value struct unpacking.
//
// The bridge function receives an i8* sret pointer as its first parameter. On success it
// stores {false, internal_value, null}. On failure it stores {true, zero, error_instance_ptr}.

// storeFailableSuccess stores a successful result into the failable sret pointer.
// innerVal is the internal-form success value (e.g., i8* for strings, i64 for ints).
// resultType is the failable result struct type {i1, T, i8*}.
func (c *Compiler) storeFailableSuccess(block *ir.Block, sretParam value.Value, innerVal value.Value, resultType *irtypes.StructType) {
	var agg value.Value = constant.NewUndef(resultType)
	agg = block.NewInsertValue(agg, constant.NewInt(irtypes.I1, 0), 0) // ok
	if !isVoidResult(resultType) {
		agg = block.NewInsertValue(agg, innerVal, 1) // value
	}
	errIdx := resultErrIdx(resultType)
	agg = block.NewInsertValue(agg, constant.NewNull(irtypes.I8Ptr), errIdx) // null error

	sretTyped := block.NewBitCast(sretParam, irtypes.NewPointer(resultType))
	block.NewStore(agg, sretTyped)
}

// storeFailableError stores an error result into the failable sret pointer.
// errInstancePtr is the i8* pointer to a heap-allocated error instance.
// resultType is the failable result struct type {i1, T, i8*}.
func (c *Compiler) storeFailableError(block *ir.Block, sretParam value.Value, errInstancePtr value.Value, resultType *irtypes.StructType) {
	var agg value.Value = constant.NewUndef(resultType)
	agg = block.NewInsertValue(agg, constant.NewInt(irtypes.I1, 1), 0) // error flag
	if !isVoidResult(resultType) {
		agg = block.NewInsertValue(agg, c.zeroValue(resultType.Fields[1]), 1) // zero value
	}
	errIdx := resultErrIdx(resultType)
	agg = block.NewInsertValue(agg, errInstancePtr, errIdx) // error instance

	sretTyped := block.NewBitCast(sretParam, irtypes.NewPointer(resultType))
	block.NewStore(agg, sretTyped)
}

// constructErrorFromCStr constructs a base error instance from a C string message.
// Returns an i8* pointer to the heap-allocated error instance.
//
// The error type layout:
//
//	Instance: { error_m* _variant, <string fields> message }
//	Value:    { i8* _vtable, error_i* _instance }
//
// The constructed instance has its _variant set to the error typeinfo global and
// its message field set to a Promise string created from the C string.
func (c *Compiler) constructErrorFromCStr(block *ir.Block, msgPtr value.Value, msgLen value.Value) value.Value {
	errorLayout := c.layouts[types.TypError]
	if errorLayout == nil {
		panic("codegen: error type layout not found")
	}
	instanceStructType := errorLayout.Instance.LLVMType
	instancePtrType := errorLayout.InstancePtrType

	// Compute instance size via GEP-from-null
	nullPtr := constant.NewNull(instancePtrType)
	sizePtr := block.NewGetElementPtr(instanceStructType, nullPtr,
		constant.NewInt(irtypes.I32, 1))
	sizeRaw := block.NewPtrToInt(sizePtr, c.ptrIntType())
	var size value.Value = sizeRaw
	if c.isWasm {
		size = block.NewZExt(sizeRaw, irtypes.I64)
	}

	// Allocate error instance
	rawPtr := block.NewCall(c.palAlloc, size)
	typedPtr := block.NewBitCast(rawPtr, instancePtrType)

	// Set _variant field (field 0 of instance, which is a struct containing the variant ptr)
	variantFieldPtr := block.NewGetElementPtr(instanceStructType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	variantPtrType := errorLayout.Instance.Fields[0].LLVMType.(*irtypes.PointerType)
	if tiGlobal := c.typeInfoGlobals[types.TypError]; tiGlobal != nil {
		tiPtr := block.NewBitCast(tiGlobal, variantPtrType)
		block.NewStore(tiPtr, variantFieldPtr)
	} else {
		block.NewStore(constant.NewNull(variantPtrType), variantFieldPtr)
	}

	// Create Promise string from the C string
	msgStr := block.NewCall(c.funcs["promise_string_new"], msgPtr, msgLen)

	// Set message field — index from InstanceFieldIndex
	msgFieldIdx := errorLayout.InstanceFieldIndex["message"]
	msgFieldPtr := block.NewGetElementPtr(instanceStructType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(msgFieldIdx)))

	// The message field in the instance is the string's internal type (i8*).
	// Store the string instance pointer directly.
	block.NewStore(msgStr, msgFieldPtr)

	return rawPtr // i8* error instance pointer
}

// constructErrorFromGlobalStr constructs a base error instance from a global string constant.
// msg is used as the error message. Safe to call from bridge functions (does not use c.block).
func (c *Compiler) constructErrorFromGlobalStr(block *ir.Block, msg string) value.Value {
	// Create global string constant directly (bridge-safe — doesn't use c.block)
	data := constant.NewCharArrayFromString(msg + "\x00")
	globalName := fmt.Sprintf(".str.%d", c.strCounter)
	c.strCounter++
	global := c.module.NewGlobalDef(globalName, data)
	global.Immutable = true
	msgPtr := block.NewGetElementPtr(global.ContentType, global,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	msgLen := constant.NewInt(irtypes.I64, int64(len(msg)))
	return c.constructErrorFromCStr(block, msgPtr, msgLen)
}

// Optional extern bridge helpers.
//
// Optional externs write their result in internal form: {i1 present, T value}.
// genExternCall loads the result directly without value struct unpacking.

// storeOptionalSome stores a present value into the optional sret pointer.
// innerVal is the internal-form value (e.g., i8* for strings).
// optType is the optional struct type {i1, T}.
func (c *Compiler) storeOptionalSome(block *ir.Block, sretParam value.Value, innerVal value.Value, optType *irtypes.StructType) {
	var agg value.Value = constant.NewUndef(optType)
	agg = block.NewInsertValue(agg, constant.NewInt(irtypes.I1, 1), 0) // present
	agg = block.NewInsertValue(agg, innerVal, 1)                       // value

	sretTyped := block.NewBitCast(sretParam, irtypes.NewPointer(optType))
	block.NewStore(agg, sretTyped)
}

// storeOptionalNone stores a none value into the optional sret pointer.
// optType is the optional struct type {i1, T}.
func (c *Compiler) storeOptionalNone(block *ir.Block, sretParam value.Value, optType *irtypes.StructType) {
	var agg value.Value = constant.NewUndef(optType)
	agg = block.NewInsertValue(agg, constant.NewInt(irtypes.I1, 0), 0) // absent
	agg = block.NewInsertValue(agg, c.zeroValue(optType.Fields[1]), 1) // zero value

	sretTyped := block.NewBitCast(sretParam, irtypes.NewPointer(optType))
	block.NewStore(agg, sretTyped)
}
