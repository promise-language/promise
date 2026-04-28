// Package pal provides a Platform Abstraction Layer that emits platform-specific
// LLVM IR functions into the module. All OS interactions (write, exit) go through
// PAL so the compiler can target different platforms by swapping implementations.
package pal

import (
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
)

// PAL emits platform-specific LLVM IR functions into the module.
type PAL interface {
	// EmitWrite defines @pal_write(i32 %fd, i8* %buf, i64 %len) → i64
	EmitWrite(module *ir.Module) *ir.Func
	// EmitExit defines @pal_exit(i32 %code) → void [noreturn]
	EmitExit(module *ir.Module) *ir.Func
	// EmitAlloc defines @pal_alloc(i64 %size) → i8*
	EmitAlloc(module *ir.Module) *ir.Func
	// EmitFree defines @pal_free(i8* %ptr) → void
	EmitFree(module *ir.Module) *ir.Func
	// EmitRealloc defines @pal_realloc(i8* %ptr, i64 %size) → i8*
	EmitRealloc(module *ir.Module) *ir.Func
}

// ForTarget returns a PAL implementation for the given LLVM target triple.
func ForTarget(triple string) PAL {
	switch {
	case strings.Contains(triple, "windows"):
		return &WindowsPAL{}
	case strings.Contains(triple, "wasm"):
		return &WasmPAL{}
	default:
		return &PosixPAL{}
	}
}

// emitLibcAlloc declares libc @malloc and defines @pal_alloc as a wrapper.
// Shared by all PALs that use libc for allocation.
func emitLibcAlloc(module *ir.Module) *ir.Func {
	// declare noalias i8* @malloc(i64 noundef) nounwind willreturn
	mallocSize := ir.NewParam("size", irtypes.I64)
	mallocSize.Attrs = append(mallocSize.Attrs, enum.ParamAttrNoUndef)
	mallocFn := module.NewFunc("malloc", irtypes.I8Ptr, mallocSize)
	mallocFn.ReturnAttrs = append(mallocFn.ReturnAttrs, enum.ReturnAttrNoAlias)
	mallocFn.FuncAttrs = append(mallocFn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)

	// define noalias i8* @pal_alloc(i64 %size) nounwind willreturn
	fn := module.NewFunc("pal_alloc", irtypes.I8Ptr,
		ir.NewParam("size", irtypes.I64))
	fn.ReturnAttrs = append(fn.ReturnAttrs, enum.ReturnAttrNoAlias)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock("entry")
	ret := entry.NewCall(mallocFn, fn.Params[0])
	entry.NewRet(ret)

	return fn
}

// emitLibcFree declares libc @free and defines @pal_free as a wrapper.
func emitLibcFree(module *ir.Module) *ir.Func {
	// declare void @free(i8* nocapture noundef) nounwind willreturn
	freePtr := ir.NewParam("ptr", irtypes.I8Ptr)
	freePtr.Attrs = append(freePtr.Attrs, enum.ParamAttrNoCapture, enum.ParamAttrNoUndef)
	freeFn := module.NewFunc("free", irtypes.Void, freePtr)
	freeFn.FuncAttrs = append(freeFn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)

	// define void @pal_free(i8* %ptr) nounwind willreturn
	fn := module.NewFunc("pal_free", irtypes.Void,
		ir.NewParam("ptr", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock("entry")
	entry.NewCall(freeFn, fn.Params[0])
	entry.NewRet(nil)

	return fn
}

// emitLibcRealloc declares libc @realloc and defines @pal_realloc as a wrapper.
func emitLibcRealloc(module *ir.Module) *ir.Func {
	// declare noalias i8* @realloc(i8* nocapture noundef, i64 noundef) nounwind willreturn
	reallocPtr := ir.NewParam("ptr", irtypes.I8Ptr)
	reallocPtr.Attrs = append(reallocPtr.Attrs, enum.ParamAttrNoCapture, enum.ParamAttrNoUndef)
	reallocSz := ir.NewParam("size", irtypes.I64)
	reallocSz.Attrs = append(reallocSz.Attrs, enum.ParamAttrNoUndef)
	reallocFn := module.NewFunc("realloc", irtypes.I8Ptr, reallocPtr, reallocSz)
	reallocFn.ReturnAttrs = append(reallocFn.ReturnAttrs, enum.ReturnAttrNoAlias)
	reallocFn.FuncAttrs = append(reallocFn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)

	// define noalias i8* @pal_realloc(i8* %ptr, i64 %size) nounwind willreturn
	fn := module.NewFunc("pal_realloc", irtypes.I8Ptr,
		ir.NewParam("ptr", irtypes.I8Ptr),
		ir.NewParam("size", irtypes.I64))
	fn.ReturnAttrs = append(fn.ReturnAttrs, enum.ReturnAttrNoAlias)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock("entry")
	ret := entry.NewCall(reallocFn, fn.Params[0], fn.Params[1])
	entry.NewRet(ret)

	return fn
}
