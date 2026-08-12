package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
)

// defineBoolToStringFunc emits an LLVM IR function that converts a boolean (i8)
// to its string representation ("true" or "false").
// Replaces the C runtime promise_bool_to_string.
func (c *Compiler) defineBoolToStringFunc() {
	xParam := ir.NewParam("x", irtypes.I8)
	fn := c.module.NewFunc("promise_bool_to_string", irtypes.I8Ptr, xParam)

	// Global string constants
	trueData := constant.NewCharArrayFromString("true")
	trueGlobal := c.module.NewGlobalDef(fmt.Sprintf(".str.bool.true.%d", c.strCounter), trueData)
	c.strCounter++
	trueGlobal.Immutable = true
	trueGlobal.Linkage = enum.LinkagePrivate

	falseData := constant.NewCharArrayFromString("false")
	falseGlobal := c.module.NewGlobalDef(fmt.Sprintf(".str.bool.false.%d", c.strCounter), falseData)
	c.strCounter++
	falseGlobal.Immutable = true
	falseGlobal.Linkage = enum.LinkagePrivate

	entry := fn.NewBlock(".entry")
	trueBlk := fn.NewBlock("true")
	falseBlk := fn.NewBlock("false")

	isTrue := entry.NewICmp(enum.IPredNE, xParam, constant.NewInt(irtypes.I8, 0))
	entry.NewCondBr(isTrue, trueBlk, falseBlk)

	// true block
	truePtr := trueBlk.NewGetElementPtr(trueGlobal.ContentType, trueGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	trueStr := trueBlk.NewCall(c.funcs["promise_string_new"],
		truePtr, constant.NewInt(irtypes.I64, 4))
	trueBlk.NewRet(trueStr)

	// false block
	falsePtr := falseBlk.NewGetElementPtr(falseGlobal.ContentType, falseGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	falseStr := falseBlk.NewCall(c.funcs["promise_string_new"],
		falsePtr, constant.NewInt(irtypes.I64, 5))
	falseBlk.NewRet(falseStr)

	c.funcs["promise_bool_to_string"] = fn
}

// defineIntToStringFunc emits an LLVM IR function that converts a signed int64
// to its decimal string representation. Uses a divide-by-10 loop writing digits
// backwards into a 20-byte stack buffer, then calls promise_string_new.
// Replaces the C runtime promise_int_to_string.
func (c *Compiler) defineIntToStringFunc() {
	xParam := ir.NewParam("x", irtypes.I64)
	fn := c.module.NewFunc("promise_int_to_string", irtypes.I8Ptr, xParam)

	bufType := irtypes.NewArray(20, irtypes.I8)
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	ten64 := constant.NewInt(irtypes.I64, 10)
	ascii0 := constant.NewInt(irtypes.I64, 48) // '0'

	// Global "0" constant for zero case
	zeroData := constant.NewCharArrayFromString("0")
	zeroGlobal := c.module.NewGlobalDef(fmt.Sprintf(".str.zero.%d", c.strCounter), zeroData)
	c.strCounter++
	zeroGlobal.Immutable = true
	zeroGlobal.Linkage = enum.LinkagePrivate

	// entry: allocas and zero check
	entry := fn.NewBlock(".entry")
	buf := entry.NewAlloca(bufType)
	posAlloca := entry.NewAlloca(irtypes.I64)
	entry.NewStore(constant.NewInt(irtypes.I64, 19), posAlloca)
	valAlloca := entry.NewAlloca(irtypes.I64)
	negAlloca := entry.NewAlloca(irtypes.I1)
	entry.NewStore(constant.False, negAlloca)

	isZero := entry.NewICmp(enum.IPredEQ, xParam, zero64)
	zeroCase := fn.NewBlock("zero_case")
	checkNeg := fn.NewBlock("check_neg")
	entry.NewCondBr(isZero, zeroCase, checkNeg)

	// zero_case: return "0"
	zeroPtr := zeroCase.NewGetElementPtr(zeroGlobal.ContentType, zeroGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	zeroStr := zeroCase.NewCall(c.funcs["promise_string_new"],
		zeroPtr, one64)
	zeroCase.NewRet(zeroStr)

	// check_neg: test if negative
	negate := fn.NewBlock("negate")
	startLoop := fn.NewBlock("start_loop")
	isNeg := checkNeg.NewICmp(enum.IPredSLT, xParam, zero64)
	checkNeg.NewCondBr(isNeg, negate, startLoop)

	// negate: abs = 0 - x, store abs and neg=true
	absVal := negate.NewSub(zero64, xParam)
	negate.NewStore(absVal, valAlloca)
	negate.NewStore(constant.True, negAlloca)
	digitLoop := fn.NewBlock("digit_loop")
	negate.NewBr(digitLoop)

	// start_loop: store x directly
	startLoop.NewStore(xParam, valAlloca)
	startLoop.NewBr(digitLoop)

	// digit_loop: extract digit, store to buffer
	cur := digitLoop.NewLoad(irtypes.I64, valAlloca)
	digit := digitLoop.NewURem(cur, ten64)
	ch64 := digitLoop.NewAdd(digit, ascii0)
	ch8 := digitLoop.NewTrunc(ch64, irtypes.I8)
	pos := digitLoop.NewLoad(irtypes.I64, posAlloca)
	slot := digitLoop.NewGetElementPtr(bufType, buf, zero64, pos)
	digitLoop.NewStore(ch8, slot)
	posDec := digitLoop.NewSub(pos, one64)
	digitLoop.NewStore(posDec, posAlloca)
	next := digitLoop.NewUDiv(cur, ten64)
	digitLoop.NewStore(next, valAlloca)
	more := digitLoop.NewICmp(enum.IPredNE, next, zero64)
	checkSign := fn.NewBlock("check_sign")
	digitLoop.NewCondBr(more, digitLoop, checkSign)

	// check_sign: prepend '-' if negative
	negFlag := checkSign.NewLoad(irtypes.I1, negAlloca)
	addSign := fn.NewBlock("add_sign")
	done := fn.NewBlock("done")
	checkSign.NewCondBr(negFlag, addSign, done)

	// add_sign: store '-' at current pos
	signPos := addSign.NewLoad(irtypes.I64, posAlloca)
	signSlot := addSign.NewGetElementPtr(bufType, buf, zero64, signPos)
	addSign.NewStore(constant.NewInt(irtypes.I8, 45), signSlot) // '-'
	signPosDec := addSign.NewSub(signPos, one64)
	addSign.NewStore(signPosDec, posAlloca)
	addSign.NewBr(done)

	// done: compute start and length, call promise_string_new
	finalPos := done.NewLoad(irtypes.I64, posAlloca)
	start := done.NewAdd(finalPos, one64)
	strLen := done.NewSub(constant.NewInt(irtypes.I64, 20), start)
	dataPtr := done.NewGetElementPtr(bufType, buf, zero64, start)
	result := done.NewCall(c.funcs["promise_string_new"], dataPtr, strLen)
	done.NewRet(result)

	c.funcs["promise_int_to_string"] = fn
}

// defineUintToStringFunc emits an LLVM IR function that converts an unsigned int64
// to its decimal string representation. Same as signed but without negative handling.
// This is a NEW function (not replacing an existing extern) that fixes the u64 sign bug.
func (c *Compiler) defineUintToStringFunc() {
	xParam := ir.NewParam("x", irtypes.I64)
	fn := c.module.NewFunc("promise_uint_to_string", irtypes.I8Ptr, xParam)

	bufType := irtypes.NewArray(20, irtypes.I8)
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	ten64 := constant.NewInt(irtypes.I64, 10)
	ascii0 := constant.NewInt(irtypes.I64, 48)

	// Global "0" constant for zero case
	zeroData := constant.NewCharArrayFromString("0")
	zeroGlobal := c.module.NewGlobalDef(fmt.Sprintf(".str.uzero.%d", c.strCounter), zeroData)
	c.strCounter++
	zeroGlobal.Immutable = true
	zeroGlobal.Linkage = enum.LinkagePrivate

	// entry: allocas and zero check
	entry := fn.NewBlock(".entry")
	buf := entry.NewAlloca(bufType)
	posAlloca := entry.NewAlloca(irtypes.I64)
	entry.NewStore(constant.NewInt(irtypes.I64, 19), posAlloca)
	valAlloca := entry.NewAlloca(irtypes.I64)
	entry.NewStore(xParam, valAlloca)

	isZero := entry.NewICmp(enum.IPredEQ, xParam, zero64)
	zeroCase := fn.NewBlock("zero_case")
	digitLoop := fn.NewBlock("digit_loop")
	entry.NewCondBr(isZero, zeroCase, digitLoop)

	// zero_case: return "0"
	zeroPtr := zeroCase.NewGetElementPtr(zeroGlobal.ContentType, zeroGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	zeroStr := zeroCase.NewCall(c.funcs["promise_string_new"],
		zeroPtr, one64)
	zeroCase.NewRet(zeroStr)

	// digit_loop: extract digit, store to buffer
	cur := digitLoop.NewLoad(irtypes.I64, valAlloca)
	digit := digitLoop.NewURem(cur, ten64)
	ch64 := digitLoop.NewAdd(digit, ascii0)
	ch8 := digitLoop.NewTrunc(ch64, irtypes.I8)
	pos := digitLoop.NewLoad(irtypes.I64, posAlloca)
	slot := digitLoop.NewGetElementPtr(bufType, buf, zero64, pos)
	digitLoop.NewStore(ch8, slot)
	posDec := digitLoop.NewSub(pos, one64)
	digitLoop.NewStore(posDec, posAlloca)
	next := digitLoop.NewUDiv(cur, ten64)
	digitLoop.NewStore(next, valAlloca)
	more := digitLoop.NewICmp(enum.IPredNE, next, zero64)
	done := fn.NewBlock("done")
	digitLoop.NewCondBr(more, digitLoop, done)

	// done: compute start and length, call promise_string_new
	finalPos := done.NewLoad(irtypes.I64, posAlloca)
	start := done.NewAdd(finalPos, one64)
	strLen := done.NewSub(constant.NewInt(irtypes.I64, 20), start)
	dataPtr := done.NewGetElementPtr(bufType, buf, zero64, start)
	result := done.NewCall(c.funcs["promise_string_new"], dataPtr, strLen)
	done.NewRet(result)

	c.funcs["promise_uint_to_string"] = fn
}

// declareF64ToStringFunc declares promise_f64_to_string as a stub (no body).
// The body is added later by defineF64ToStringBridge, which forwards to the
// Promise-defined _f64_to_str function in std/format.pr.
func (c *Compiler) declareF64ToStringFunc() {
	xParam := ir.NewParam("x", irtypes.Double)
	fn := c.module.NewFunc("promise_f64_to_string", irtypes.I8Ptr, xParam)
	c.funcs["promise_f64_to_string"] = fn
}

// defineF64ToStringBridge adds a body to promise_f64_to_string that calls
// the Promise-defined _f64_to_str function. Must be called after declareFuncs.
func (c *Compiler) defineF64ToStringBridge() {
	fn := c.funcs["promise_f64_to_string"]
	entry := fn.NewBlock(".entry")
	stdFn := c.funcs["_f64_to_str"]
	result := entry.NewCall(stdFn, fn.Params[0])
	entry.NewRet(result)
}

// declareF32ToStringFunc declares promise_f32_to_string as a stub (no body).
// The body is added later by defineF32ToStringBridge, which forwards to the
// Promise-defined _f32_to_str function in std/format.pr.
func (c *Compiler) declareF32ToStringFunc() {
	xParam := ir.NewParam("x", irtypes.Float)
	fn := c.module.NewFunc("promise_f32_to_string", irtypes.I8Ptr, xParam)
	c.funcs["promise_f32_to_string"] = fn
}

// defineF32ToStringBridge adds a body to promise_f32_to_string that calls
// the Promise-defined _f32_to_str function. Must be called after declareFuncs.
func (c *Compiler) defineF32ToStringBridge() {
	fn := c.funcs["promise_f32_to_string"]
	entry := fn.NewBlock(".entry")
	stdFn := c.funcs["_f32_to_str"]
	result := entry.NewCall(stdFn, fn.Params[0])
	entry.NewRet(result)
}

// defineCharToStringFunc emits an LLVM IR function that converts a Unicode
// codepoint (i32) to its UTF-8 encoded string representation.
// Uses cascading range checks with bitwise encoding.
// Replaces the C runtime promise_char_to_string.
func (c *Compiler) defineCharToStringFunc() {
	cpParam := ir.NewParam("cp", irtypes.I32)
	fn := c.module.NewFunc("promise_char_to_string", irtypes.I8Ptr, cpParam)

	bufType := irtypes.NewArray(4, irtypes.I8)
	zero32 := constant.NewInt(irtypes.I32, 0)

	entry := fn.NewBlock(".entry")
	buf := entry.NewAlloca(bufType)

	// Check: cp < 0x80?
	oneByte := fn.NewBlock("one_byte")
	check2 := fn.NewBlock("check_2")
	lt80 := entry.NewICmp(enum.IPredULT, cpParam, constant.NewInt(irtypes.I32, 0x80))
	entry.NewCondBr(lt80, oneByte, check2)

	// one_byte: buf[0] = (i8)cp; return string_new(buf, 1)
	b0_1 := oneByte.NewTrunc(cpParam, irtypes.I8)
	slot0_1 := oneByte.NewGetElementPtr(bufType, buf, zero32, zero32)
	oneByte.NewStore(b0_1, slot0_1)
	bufPtr1 := oneByte.NewGetElementPtr(bufType, buf, zero32, zero32)
	r1 := oneByte.NewCall(c.funcs["promise_string_new"], bufPtr1, constant.NewInt(irtypes.I64, 1))
	oneByte.NewRet(r1)

	// check_2: cp < 0x800?
	twoByte := fn.NewBlock("two_byte")
	check3 := fn.NewBlock("check_3")
	lt800 := check2.NewICmp(enum.IPredULT, cpParam, constant.NewInt(irtypes.I32, 0x800))
	check2.NewCondBr(lt800, twoByte, check3)

	// two_byte: buf[0] = 0xC0 | (cp >> 6); buf[1] = 0x80 | (cp & 0x3F)
	sh6_2 := twoByte.NewLShr(cpParam, constant.NewInt(irtypes.I32, 6))
	b0_2 := twoByte.NewOr(sh6_2, constant.NewInt(irtypes.I32, 0xC0))
	b0_2_8 := twoByte.NewTrunc(b0_2, irtypes.I8)
	slot0_2 := twoByte.NewGetElementPtr(bufType, buf, zero32, zero32)
	twoByte.NewStore(b0_2_8, slot0_2)

	m1_2 := twoByte.NewAnd(cpParam, constant.NewInt(irtypes.I32, 0x3F))
	b1_2 := twoByte.NewOr(m1_2, constant.NewInt(irtypes.I32, 0x80))
	b1_2_8 := twoByte.NewTrunc(b1_2, irtypes.I8)
	slot1_2 := twoByte.NewGetElementPtr(bufType, buf, zero32, constant.NewInt(irtypes.I32, 1))
	twoByte.NewStore(b1_2_8, slot1_2)

	bufPtr2 := twoByte.NewGetElementPtr(bufType, buf, zero32, zero32)
	r2 := twoByte.NewCall(c.funcs["promise_string_new"], bufPtr2, constant.NewInt(irtypes.I64, 2))
	twoByte.NewRet(r2)

	// check_3: cp < 0x10000?
	threeByte := fn.NewBlock("three_byte")
	fourByte := fn.NewBlock("four_byte")
	lt10000 := check3.NewICmp(enum.IPredULT, cpParam, constant.NewInt(irtypes.I32, 0x10000))
	check3.NewCondBr(lt10000, threeByte, fourByte)

	// three_byte: 3-byte UTF-8 encoding
	sh12_3 := threeByte.NewLShr(cpParam, constant.NewInt(irtypes.I32, 12))
	b0_3 := threeByte.NewOr(sh12_3, constant.NewInt(irtypes.I32, 0xE0))
	b0_3_8 := threeByte.NewTrunc(b0_3, irtypes.I8)
	slot0_3 := threeByte.NewGetElementPtr(bufType, buf, zero32, zero32)
	threeByte.NewStore(b0_3_8, slot0_3)

	sh6_3 := threeByte.NewLShr(cpParam, constant.NewInt(irtypes.I32, 6))
	m1_3 := threeByte.NewAnd(sh6_3, constant.NewInt(irtypes.I32, 0x3F))
	b1_3 := threeByte.NewOr(m1_3, constant.NewInt(irtypes.I32, 0x80))
	b1_3_8 := threeByte.NewTrunc(b1_3, irtypes.I8)
	slot1_3 := threeByte.NewGetElementPtr(bufType, buf, zero32, constant.NewInt(irtypes.I32, 1))
	threeByte.NewStore(b1_3_8, slot1_3)

	m2_3 := threeByte.NewAnd(cpParam, constant.NewInt(irtypes.I32, 0x3F))
	b2_3 := threeByte.NewOr(m2_3, constant.NewInt(irtypes.I32, 0x80))
	b2_3_8 := threeByte.NewTrunc(b2_3, irtypes.I8)
	slot2_3 := threeByte.NewGetElementPtr(bufType, buf, zero32, constant.NewInt(irtypes.I32, 2))
	threeByte.NewStore(b2_3_8, slot2_3)

	bufPtr3 := threeByte.NewGetElementPtr(bufType, buf, zero32, zero32)
	r3 := threeByte.NewCall(c.funcs["promise_string_new"], bufPtr3, constant.NewInt(irtypes.I64, 3))
	threeByte.NewRet(r3)

	// four_byte: 4-byte UTF-8 encoding
	sh18_4 := fourByte.NewLShr(cpParam, constant.NewInt(irtypes.I32, 18))
	b0_4 := fourByte.NewOr(sh18_4, constant.NewInt(irtypes.I32, 0xF0))
	b0_4_8 := fourByte.NewTrunc(b0_4, irtypes.I8)
	slot0_4 := fourByte.NewGetElementPtr(bufType, buf, zero32, zero32)
	fourByte.NewStore(b0_4_8, slot0_4)

	sh12_4 := fourByte.NewLShr(cpParam, constant.NewInt(irtypes.I32, 12))
	m1_4 := fourByte.NewAnd(sh12_4, constant.NewInt(irtypes.I32, 0x3F))
	b1_4 := fourByte.NewOr(m1_4, constant.NewInt(irtypes.I32, 0x80))
	b1_4_8 := fourByte.NewTrunc(b1_4, irtypes.I8)
	slot1_4 := fourByte.NewGetElementPtr(bufType, buf, zero32, constant.NewInt(irtypes.I32, 1))
	fourByte.NewStore(b1_4_8, slot1_4)

	sh6_4 := fourByte.NewLShr(cpParam, constant.NewInt(irtypes.I32, 6))
	m2_4 := fourByte.NewAnd(sh6_4, constant.NewInt(irtypes.I32, 0x3F))
	b2_4 := fourByte.NewOr(m2_4, constant.NewInt(irtypes.I32, 0x80))
	b2_4_8 := fourByte.NewTrunc(b2_4, irtypes.I8)
	slot2_4 := fourByte.NewGetElementPtr(bufType, buf, zero32, constant.NewInt(irtypes.I32, 2))
	fourByte.NewStore(b2_4_8, slot2_4)

	m3_4 := fourByte.NewAnd(cpParam, constant.NewInt(irtypes.I32, 0x3F))
	b3_4 := fourByte.NewOr(m3_4, constant.NewInt(irtypes.I32, 0x80))
	b3_4_8 := fourByte.NewTrunc(b3_4, irtypes.I8)
	slot3_4 := fourByte.NewGetElementPtr(bufType, buf, zero32, constant.NewInt(irtypes.I32, 3))
	fourByte.NewStore(b3_4_8, slot3_4)

	bufPtr4 := fourByte.NewGetElementPtr(bufType, buf, zero32, zero32)
	r4 := fourByte.NewCall(c.funcs["promise_string_new"], bufPtr4, constant.NewInt(irtypes.I64, 4))
	fourByte.NewRet(r4)

	c.funcs["promise_char_to_string"] = fn
}
