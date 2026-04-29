// Package pal provides a Platform Abstraction Layer that emits platform-specific
// LLVM IR functions into the module. All OS interactions (write, exit) go through
// PAL so the compiler can target different platforms by swapping implementations.
package pal

import (
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
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

	// Threading primitives (Phase 5)
	// EmitThreadCreate defines @pal_thread_create(i8* %fn, i8* %arg) → i8* (handle)
	EmitThreadCreate(module *ir.Module) *ir.Func
	// EmitThreadJoin defines @pal_thread_join(i8* %handle) → void
	EmitThreadJoin(module *ir.Module) *ir.Func
	// EmitMutexInit defines @pal_mutex_init() → i8*
	EmitMutexInit(module *ir.Module) *ir.Func
	// EmitMutexLock defines @pal_mutex_lock(i8* %mutex) → void
	EmitMutexLock(module *ir.Module) *ir.Func
	// EmitMutexUnlock defines @pal_mutex_unlock(i8* %mutex) → void
	EmitMutexUnlock(module *ir.Module) *ir.Func
	// EmitMutexDestroy defines @pal_mutex_destroy(i8* %mutex) → void
	EmitMutexDestroy(module *ir.Module) *ir.Func
	// EmitCondInit defines @pal_cond_init() → i8*
	EmitCondInit(module *ir.Module) *ir.Func
	// EmitCondWait defines @pal_cond_wait(i8* %cond, i8* %mutex) → void
	EmitCondWait(module *ir.Module) *ir.Func
	// EmitCondSignal defines @pal_cond_signal(i8* %cond) → void
	EmitCondSignal(module *ir.Module) *ir.Func
	// EmitCondDestroy defines @pal_cond_destroy(i8* %cond) → void
	EmitCondDestroy(module *ir.Module) *ir.Func
	// EmitCondBroadcast defines @pal_cond_broadcast(i8* %cond) → void
	EmitCondBroadcast(module *ir.Module) *ir.Func

	// Scheduler primitives (Phase 5c)
	// EmitNumCPUs defines @pal_num_cpus() → i32
	EmitNumCPUs(module *ir.Module) *ir.Func
}

// ForTarget returns a PAL implementation for the given LLVM target triple.
func ForTarget(triple string) PAL {
	switch {
	case strings.Contains(triple, "windows"):
		return &WindowsPAL{}
	case strings.Contains(triple, "wasm"):
		return &WasmPAL{}
	default:
		return &PosixPAL{target: triple}
	}
}

// lookupFunc finds an existing function in the module by name.
// Used by threading PAL methods that need to call pal_alloc/pal_free.
func lookupFunc(module *ir.Module, name string) *ir.Func {
	for _, f := range module.Funcs {
		if f.Name() == name {
			return f
		}
	}
	return nil
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

// --- Stub threading implementations (used by Windows and WASM PALs) ---
// These run the function synchronously (no real threading).
// WASM is single-threaded; Windows stubs will be replaced with real implementations later.

// threadFnPtrType returns the LLVM type for a thread routine: i8* (i8*)*
func threadFnPtrType() *irtypes.PointerType {
	return irtypes.NewPointer(irtypes.NewFunc(irtypes.I8Ptr, irtypes.I8Ptr))
}

// emitStubThreadCreate calls fn(arg) synchronously and returns null handle.
func emitStubThreadCreate(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_thread_create", irtypes.I8Ptr,
		ir.NewParam("fn", irtypes.I8Ptr),
		ir.NewParam("arg", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")

	// Bitcast i8* to function pointer and call synchronously
	fnPtr := entry.NewBitCast(fn.Params[0], threadFnPtrType())
	entry.NewCall(fnPtr, fn.Params[1])

	entry.NewRet(constant.NewNull(irtypes.I8Ptr))
	return fn
}

// emitStubThreadJoin is a no-op (function already completed synchronously).
func emitStubThreadJoin(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_thread_join", irtypes.Void,
		ir.NewParam("handle", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewRet(nil)
	return fn
}

// emitStubMutexInit allocates a dummy 1-byte handle (no real locking).
func emitStubMutexInit(module *ir.Module) *ir.Func {
	palAlloc := lookupFunc(module, "pal_alloc")
	fn := module.NewFunc("pal_mutex_init", irtypes.I8Ptr)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	handle := entry.NewCall(palAlloc, constant.NewInt(irtypes.I64, 1))
	entry.NewRet(handle)
	return fn
}

// emitStubMutexLock is a no-op (single-threaded, no contention).
func emitStubMutexLock(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_mutex_lock", irtypes.Void,
		ir.NewParam("mutex", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewRet(nil)
	return fn
}

// emitStubMutexUnlock is a no-op.
func emitStubMutexUnlock(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_mutex_unlock", irtypes.Void,
		ir.NewParam("mutex", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewRet(nil)
	return fn
}

// emitStubMutexDestroy frees the dummy handle.
func emitStubMutexDestroy(module *ir.Module) *ir.Func {
	palFree := lookupFunc(module, "pal_free")
	fn := module.NewFunc("pal_mutex_destroy", irtypes.Void,
		ir.NewParam("mutex", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewCall(palFree, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// emitStubCondInit allocates a dummy 1-byte handle.
func emitStubCondInit(module *ir.Module) *ir.Func {
	palAlloc := lookupFunc(module, "pal_alloc")
	fn := module.NewFunc("pal_cond_init", irtypes.I8Ptr)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	handle := entry.NewCall(palAlloc, constant.NewInt(irtypes.I64, 1))
	entry.NewRet(handle)
	return fn
}

// emitStubCondWait is a no-op.
func emitStubCondWait(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_cond_wait", irtypes.Void,
		ir.NewParam("cond", irtypes.I8Ptr),
		ir.NewParam("mutex", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewRet(nil)
	return fn
}

// emitStubCondSignal is a no-op.
func emitStubCondSignal(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_cond_signal", irtypes.Void,
		ir.NewParam("cond", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewRet(nil)
	return fn
}

// emitStubCondBroadcast is a no-op.
func emitStubCondBroadcast(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_cond_broadcast", irtypes.Void,
		ir.NewParam("cond", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewRet(nil)
	return fn
}

// emitStubNumCPUs returns 1 (single-threaded platforms: WASM, Windows stub).
func emitStubNumCPUs(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_num_cpus", irtypes.I32)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewRet(constant.NewInt(irtypes.I32, 1))
	return fn
}

// emitStubCondDestroy frees the dummy handle.
func emitStubCondDestroy(module *ir.Module) *ir.Func {
	palFree := lookupFunc(module, "pal_free")
	fn := module.NewFunc("pal_cond_destroy", irtypes.Void,
		ir.NewParam("cond", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewCall(palFree, fn.Params[0])
	entry.NewRet(nil)
	return fn
}
