package pal

import (
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
)

// PosixPAL implements PAL for POSIX systems (macOS, Linux) using libc write/exit.
type PosixPAL struct {
	target string // LLVM target triple (needed for platform-specific constants)
}

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

// --- POSIX threading via pthreads ---

// EmitThreadCreate declares pthread_create and defines @pal_thread_create.
// Allocates 8 bytes for pthread_t, spawns a thread, returns handle.
func (p *PosixPAL) EmitThreadCreate(module *ir.Module) *ir.Func {
	palAlloc := lookupFunc(module, "pal_alloc")

	// declare i32 @pthread_create(i8*, i8*, i8*(i8*)*, i8*) nounwind
	pthreadCreate := module.NewFunc("pthread_create", irtypes.I32,
		ir.NewParam("thread", irtypes.I8Ptr),
		ir.NewParam("attr", irtypes.I8Ptr),
		ir.NewParam("start_routine", threadFnPtrType()),
		ir.NewParam("arg", irtypes.I8Ptr))
	pthreadCreate.FuncAttrs = append(pthreadCreate.FuncAttrs, enum.FuncAttrNoUnwind)

	// define i8* @pal_thread_create(i8* %fn, i8* %arg) nounwind
	fn := module.NewFunc("pal_thread_create", irtypes.I8Ptr,
		ir.NewParam("fn", irtypes.I8Ptr),
		ir.NewParam("arg", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")

	// Allocate 8 bytes for pthread_t handle
	handle := entry.NewCall(palAlloc, constant.NewInt(irtypes.I64, 8))

	// Bitcast i8* fn to thread routine function pointer
	fnPtr := entry.NewBitCast(fn.Params[0], threadFnPtrType())

	// pthread_create(handle, null, fnPtr, arg)
	entry.NewCall(pthreadCreate, handle, constant.NewNull(irtypes.I8Ptr), fnPtr, fn.Params[1])

	entry.NewRet(handle)
	return fn
}

// EmitThreadJoin declares pthread_join and defines @pal_thread_join.
// Loads pthread_t from handle, joins, frees handle.
func (p *PosixPAL) EmitThreadJoin(module *ir.Module) *ir.Func {
	palFree := lookupFunc(module, "pal_free")

	// declare i32 @pthread_join(i8*, i8**) nounwind
	// Using i8* for pthread_t (8 bytes on both macOS and Linux 64-bit)
	pthreadJoin := module.NewFunc("pthread_join", irtypes.I32,
		ir.NewParam("thread", irtypes.I8Ptr),
		ir.NewParam("retval", irtypes.NewPointer(irtypes.I8Ptr)))
	pthreadJoin.FuncAttrs = append(pthreadJoin.FuncAttrs, enum.FuncAttrNoUnwind)

	// define void @pal_thread_join(i8* %handle) nounwind
	fn := module.NewFunc("pal_thread_join", irtypes.Void,
		ir.NewParam("handle", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")

	// Load pthread_t from handle buffer (stored as i8*)
	handleAsPtr := entry.NewBitCast(fn.Params[0], irtypes.NewPointer(irtypes.I8Ptr))
	threadId := entry.NewLoad(irtypes.I8Ptr, handleAsPtr)

	// pthread_join(threadId, null)
	entry.NewCall(pthreadJoin, threadId, constant.NewNull(irtypes.NewPointer(irtypes.I8Ptr)))

	// Free handle
	entry.NewCall(palFree, fn.Params[0])

	entry.NewRet(nil)
	return fn
}

// EmitMutexInit declares pthread_mutex_init and defines @pal_mutex_init.
// Allocates 64 bytes (fits pthread_mutex_t on macOS and Linux), initializes.
func (p *PosixPAL) EmitMutexInit(module *ir.Module) *ir.Func {
	palAlloc := lookupFunc(module, "pal_alloc")

	// declare i32 @pthread_mutex_init(i8*, i8*) nounwind
	pthreadMutexInit := module.NewFunc("pthread_mutex_init", irtypes.I32,
		ir.NewParam("mutex", irtypes.I8Ptr),
		ir.NewParam("attr", irtypes.I8Ptr))
	pthreadMutexInit.FuncAttrs = append(pthreadMutexInit.FuncAttrs, enum.FuncAttrNoUnwind)

	// define i8* @pal_mutex_init() nounwind
	fn := module.NewFunc("pal_mutex_init", irtypes.I8Ptr)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")

	buf := entry.NewCall(palAlloc, constant.NewInt(irtypes.I64, 64))
	entry.NewCall(pthreadMutexInit, buf, constant.NewNull(irtypes.I8Ptr))
	entry.NewRet(buf)
	return fn
}

// EmitMutexLock declares pthread_mutex_lock and defines @pal_mutex_lock.
func (p *PosixPAL) EmitMutexLock(module *ir.Module) *ir.Func {
	pthreadMutexLock := module.NewFunc("pthread_mutex_lock", irtypes.I32,
		ir.NewParam("mutex", irtypes.I8Ptr))
	pthreadMutexLock.FuncAttrs = append(pthreadMutexLock.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_mutex_lock", irtypes.Void,
		ir.NewParam("mutex", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewCall(pthreadMutexLock, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// EmitMutexUnlock declares pthread_mutex_unlock and defines @pal_mutex_unlock.
func (p *PosixPAL) EmitMutexUnlock(module *ir.Module) *ir.Func {
	pthreadMutexUnlock := module.NewFunc("pthread_mutex_unlock", irtypes.I32,
		ir.NewParam("mutex", irtypes.I8Ptr))
	pthreadMutexUnlock.FuncAttrs = append(pthreadMutexUnlock.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_mutex_unlock", irtypes.Void,
		ir.NewParam("mutex", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewCall(pthreadMutexUnlock, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// EmitMutexDestroy declares pthread_mutex_destroy and defines @pal_mutex_destroy.
func (p *PosixPAL) EmitMutexDestroy(module *ir.Module) *ir.Func {
	palFree := lookupFunc(module, "pal_free")

	pthreadMutexDestroy := module.NewFunc("pthread_mutex_destroy", irtypes.I32,
		ir.NewParam("mutex", irtypes.I8Ptr))
	pthreadMutexDestroy.FuncAttrs = append(pthreadMutexDestroy.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_mutex_destroy", irtypes.Void,
		ir.NewParam("mutex", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewCall(pthreadMutexDestroy, fn.Params[0])
	entry.NewCall(palFree, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// EmitCondInit declares pthread_cond_init and defines @pal_cond_init.
// Allocates 64 bytes (fits pthread_cond_t on macOS and Linux).
func (p *PosixPAL) EmitCondInit(module *ir.Module) *ir.Func {
	palAlloc := lookupFunc(module, "pal_alloc")

	pthreadCondInit := module.NewFunc("pthread_cond_init", irtypes.I32,
		ir.NewParam("cond", irtypes.I8Ptr),
		ir.NewParam("attr", irtypes.I8Ptr))
	pthreadCondInit.FuncAttrs = append(pthreadCondInit.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_cond_init", irtypes.I8Ptr)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")

	buf := entry.NewCall(palAlloc, constant.NewInt(irtypes.I64, 64))
	entry.NewCall(pthreadCondInit, buf, constant.NewNull(irtypes.I8Ptr))
	entry.NewRet(buf)
	return fn
}

// EmitCondWait declares pthread_cond_wait and defines @pal_cond_wait.
func (p *PosixPAL) EmitCondWait(module *ir.Module) *ir.Func {
	pthreadCondWait := module.NewFunc("pthread_cond_wait", irtypes.I32,
		ir.NewParam("cond", irtypes.I8Ptr),
		ir.NewParam("mutex", irtypes.I8Ptr))
	pthreadCondWait.FuncAttrs = append(pthreadCondWait.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_cond_wait", irtypes.Void,
		ir.NewParam("cond", irtypes.I8Ptr),
		ir.NewParam("mutex", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewCall(pthreadCondWait, fn.Params[0], fn.Params[1])
	entry.NewRet(nil)
	return fn
}

// EmitCondSignal declares pthread_cond_signal and defines @pal_cond_signal.
func (p *PosixPAL) EmitCondSignal(module *ir.Module) *ir.Func {
	pthreadCondSignal := module.NewFunc("pthread_cond_signal", irtypes.I32,
		ir.NewParam("cond", irtypes.I8Ptr))
	pthreadCondSignal.FuncAttrs = append(pthreadCondSignal.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_cond_signal", irtypes.Void,
		ir.NewParam("cond", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewCall(pthreadCondSignal, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// EmitCondBroadcast declares pthread_cond_broadcast and defines @pal_cond_broadcast.
func (p *PosixPAL) EmitCondBroadcast(module *ir.Module) *ir.Func {
	pthreadCondBroadcast := module.NewFunc("pthread_cond_broadcast", irtypes.I32,
		ir.NewParam("cond", irtypes.I8Ptr))
	pthreadCondBroadcast.FuncAttrs = append(pthreadCondBroadcast.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_cond_broadcast", irtypes.Void,
		ir.NewParam("cond", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewCall(pthreadCondBroadcast, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// EmitCondDestroy declares pthread_cond_destroy and defines @pal_cond_destroy.
func (p *PosixPAL) EmitCondDestroy(module *ir.Module) *ir.Func {
	palFree := lookupFunc(module, "pal_free")

	pthreadCondDestroy := module.NewFunc("pthread_cond_destroy", irtypes.I32,
		ir.NewParam("cond", irtypes.I8Ptr))
	pthreadCondDestroy.FuncAttrs = append(pthreadCondDestroy.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_cond_destroy", irtypes.Void,
		ir.NewParam("cond", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	entry.NewCall(pthreadCondDestroy, fn.Params[0])
	entry.NewCall(palFree, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// EmitNumCPUs declares libc @sysconf and defines @pal_num_cpus() → i32.
// Uses _SC_NPROCESSORS_ONLN which differs between macOS (58) and Linux (84).
func (p *PosixPAL) EmitNumCPUs(module *ir.Module) *ir.Func {
	// declare i64 @sysconf(i32) nounwind
	sysconfFn := module.NewFunc("sysconf", irtypes.I64,
		ir.NewParam("name", irtypes.I32))
	sysconfFn.FuncAttrs = append(sysconfFn.FuncAttrs, enum.FuncAttrNoUnwind)

	// _SC_NPROCESSORS_ONLN: macOS=58, Linux=84
	scNprocessorsOnln := int64(84) // Linux default
	if strings.Contains(p.target, "darwin") || strings.Contains(p.target, "apple") {
		scNprocessorsOnln = 58
	}

	// define i32 @pal_num_cpus() nounwind
	fn := module.NewFunc("pal_num_cpus", irtypes.I32)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock("entry")
	n := entry.NewCall(sysconfFn, constant.NewInt(irtypes.I32, scNprocessorsOnln))
	// Clamp to at least 1
	isLess := entry.NewICmp(enum.IPredSLT, n, constant.NewInt(irtypes.I64, 1))
	clamped := entry.NewSelect(isLess, constant.NewInt(irtypes.I64, 1), n)
	result := entry.NewTrunc(clamped, irtypes.I32)
	entry.NewRet(result)
	return fn
}
