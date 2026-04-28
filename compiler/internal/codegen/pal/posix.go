package pal

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
)

// PosixPAL implements PAL for POSIX systems (macOS, Linux) using libc write/exit.
type PosixPAL struct{}

// EmitWrite declares libc @write and defines @pal_write as a thin wrapper.
// Signature: @pal_write(i32 %fd, i8* %buf, i64 %len) → i64
func (p *PosixPAL) EmitWrite(module *ir.Module) *ir.Func {
	// declare i64 @write(i32, i8*, i64)
	writeFn := module.NewFunc("write", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	writeFn.FuncAttrs = append(writeFn.FuncAttrs, enum.FuncAttrNoUnwind)

	// define i64 @pal_write(i32 %fd, i8* %buf, i64 %len)
	fn := module.NewFunc("pal_write", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	entry := fn.NewBlock("entry")
	ret := entry.NewCall(writeFn, fn.Params[0], fn.Params[1], fn.Params[2])
	entry.NewRet(ret)

	return fn
}

// EmitExit declares libc @exit and defines @pal_exit as a noreturn wrapper.
// Signature: @pal_exit(i32 %code) → void [noreturn]
func (p *PosixPAL) EmitExit(module *ir.Module) *ir.Func {
	// declare void @exit(i32) noreturn
	exitFn := module.NewFunc("exit", irtypes.Void,
		ir.NewParam("code", irtypes.I32))
	exitFn.FuncAttrs = append(exitFn.FuncAttrs, enum.FuncAttrNoReturn, enum.FuncAttrNoUnwind)

	// define void @pal_exit(i32 %code) noreturn
	fn := module.NewFunc("pal_exit", irtypes.Void,
		ir.NewParam("code", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoReturn, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewCall(exitFn, fn.Params[0])
	entry.NewUnreachable()

	return fn
}

func (p *PosixPAL) EmitAlloc(module *ir.Module) *ir.Func   { return emitLibcAlloc(module) }
func (p *PosixPAL) EmitFree(module *ir.Module) *ir.Func    { return emitLibcFree(module) }
func (p *PosixPAL) EmitRealloc(module *ir.Module) *ir.Func { return emitLibcRealloc(module) }
