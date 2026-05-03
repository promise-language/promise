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
	entry := fn.NewBlock(".entry")
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
	entry := fn.NewBlock(".entry")
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
// Sets explicit 2MB stack size (musl defaults to 128KB which is too small).
func (p *PosixPAL) EmitThreadCreate(module *ir.Module) *ir.Func {
	palAlloc := lookupFunc(module, "pal_alloc")

	// declare i32 @pthread_attr_init(i8*) nounwind
	pthreadAttrInit := module.NewFunc("pthread_attr_init", irtypes.I32,
		ir.NewParam("attr", irtypes.I8Ptr))
	pthreadAttrInit.FuncAttrs = append(pthreadAttrInit.FuncAttrs, enum.FuncAttrNoUnwind)

	// declare i32 @pthread_attr_setstacksize(i8*, i64) nounwind
	pthreadAttrSetStackSize := module.NewFunc("pthread_attr_setstacksize", irtypes.I32,
		ir.NewParam("attr", irtypes.I8Ptr),
		ir.NewParam("stacksize", irtypes.I64))
	pthreadAttrSetStackSize.FuncAttrs = append(pthreadAttrSetStackSize.FuncAttrs, enum.FuncAttrNoUnwind)

	// declare i32 @pthread_attr_destroy(i8*) nounwind
	pthreadAttrDestroy := module.NewFunc("pthread_attr_destroy", irtypes.I32,
		ir.NewParam("attr", irtypes.I8Ptr))
	pthreadAttrDestroy.FuncAttrs = append(pthreadAttrDestroy.FuncAttrs, enum.FuncAttrNoUnwind)

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
	entry := fn.NewBlock(".entry")

	// Allocate 8 bytes for pthread_t handle
	handle := entry.NewCall(palAlloc, constant.NewInt(irtypes.I64, 8))

	// Stack-allocate pthread_attr_t (64 bytes covers all platforms) and set
	// explicit 2MB stack size (musl defaults to 128KB, causing stack overflows)
	attrBuf := entry.NewAlloca(irtypes.NewArray(64, irtypes.I8))
	attr := entry.NewBitCast(attrBuf, irtypes.I8Ptr)
	entry.NewCall(pthreadAttrInit, attr)
	entry.NewCall(pthreadAttrSetStackSize, attr, constant.NewInt(irtypes.I64, 2*1024*1024))

	// Bitcast i8* fn to thread routine function pointer
	fnPtr := entry.NewBitCast(fn.Params[0], threadFnPtrType())

	// pthread_create(handle, attr, fnPtr, arg)
	entry.NewCall(pthreadCreate, handle, attr, fnPtr, fn.Params[1])

	// Destroy attr (stack-allocated, no free needed)
	entry.NewCall(pthreadAttrDestroy, attr)

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
	entry := fn.NewBlock(".entry")

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
	entry := fn.NewBlock(".entry")

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
	entry := fn.NewBlock(".entry")
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
	entry := fn.NewBlock(".entry")
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
	entry := fn.NewBlock(".entry")
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
	entry := fn.NewBlock(".entry")

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
	entry := fn.NewBlock(".entry")
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
	entry := fn.NewBlock(".entry")
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
	entry := fn.NewBlock(".entry")
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
	entry := fn.NewBlock(".entry")
	entry.NewCall(pthreadCondDestroy, fn.Params[0])
	entry.NewCall(palFree, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// --- POSIX file I/O (Phase D) ---

// isMacOS returns true if the target is macOS/Apple.
func (p *PosixPAL) isMacOS() bool {
	return strings.Contains(p.target, "darwin") || strings.Contains(p.target, "apple")
}

// getOrDeclareErrnoLocFn returns (or declares) the platform-specific errno
// location function: __error on macOS, __errno_location on Linux.
// Returns a function with signature () -> i32*.
func (p *PosixPAL) getOrDeclareErrnoLocFn(module *ir.Module) *ir.Func {
	name := "__errno_location"
	if p.isMacOS() {
		name = "__error"
	}
	for _, fn := range module.Funcs {
		if fn.Name() == name {
			return fn
		}
	}
	fn := module.NewFunc(name, irtypes.NewPointer(irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	return fn
}

// getOrDeclareOpendir returns (or declares) libc @opendir(i8*) → i8*.
func (p *PosixPAL) getOrDeclareOpendir(module *ir.Module) *ir.Func {
	if fn := lookupFunc(module, "opendir"); fn != nil {
		return fn
	}
	fn := module.NewFunc("opendir", irtypes.I8Ptr,
		ir.NewParam("dirname", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	return fn
}

// getOrDeclareClosedir returns (or declares) libc @closedir(i8*) → i32.
func (p *PosixPAL) getOrDeclareClosedir(module *ir.Module) *ir.Func {
	if fn := lookupFunc(module, "closedir"); fn != nil {
		return fn
	}
	fn := module.NewFunc("closedir", irtypes.I32,
		ir.NewParam("dirp", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	return fn
}

// getOrDeclareReaddir returns (or declares) libc @readdir(i8*) → i8*.
func (p *PosixPAL) getOrDeclareReaddir(module *ir.Module) *ir.Func {
	if fn := lookupFunc(module, "readdir"); fn != nil {
		return fn
	}
	fn := module.NewFunc("readdir", irtypes.I8Ptr,
		ir.NewParam("dirp", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	return fn
}

// emitNegErrnoReturn emits a block that reads errno and returns -errno.
// For i32 return type functions.
func (p *PosixPAL) emitNegErrnoReturnI32(errBlk *ir.Block, errnoLocFn *ir.Func) {
	errnoPtr := errBlk.NewCall(errnoLocFn)
	errnoVal := errBlk.NewLoad(irtypes.I32, errnoPtr)
	negErrno := errBlk.NewSub(constant.NewInt(irtypes.I32, 0), errnoVal)
	errBlk.NewRet(negErrno)
}

// emitNegErrnoReturnI64 emits a block that reads errno and returns -errno as i64.
func (p *PosixPAL) emitNegErrnoReturnI64(errBlk *ir.Block, errnoLocFn *ir.Func) {
	errnoPtr := errBlk.NewCall(errnoLocFn)
	errnoVal := errBlk.NewLoad(irtypes.I32, errnoPtr)
	errnoI64 := errBlk.NewSExt(errnoVal, irtypes.I64)
	negErrno := errBlk.NewSub(constant.NewInt(irtypes.I64, 0), errnoI64)
	errBlk.NewRet(negErrno)
}

// EmitFileOpen declares libc @open and defines @pal_file_open.
// Maps mode (0=open-rw, 1=read, 2=create, 3=append) to platform-specific O_* flags.
func (p *PosixPAL) EmitFileOpen(module *ir.Module) *ir.Func {
	// declare i32 @open(i8*, i32, ...) nounwind
	// open() is variadic: mode_t is a variadic arg, which matters on AArch64
	// Apple where variadic args are passed on the stack, not in registers.
	openFn := module.NewFunc("open", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr),
		ir.NewParam("oflag", irtypes.I32))
	openFn.Sig.Variadic = true
	openFn.FuncAttrs = append(openFn.FuncAttrs, enum.FuncAttrNoUnwind)

	// Platform-specific O_* flag constants
	var oRDWR, oRDONLY, oCreateTrunc, oCreateAppend int64
	if p.isMacOS() {
		// macOS: O_RDONLY=0, O_RDWR=2, O_CREAT=0x200, O_TRUNC=0x400, O_APPEND=0x8
		oRDONLY = 0
		oRDWR = 2
		oCreateTrunc = 2 | 0x200 | 0x400 // O_RDWR|O_CREAT|O_TRUNC
		oCreateAppend = 2 | 0x200 | 0x8  // O_RDWR|O_CREAT|O_APPEND
	} else {
		// Linux: O_RDONLY=0, O_RDWR=2, O_CREAT=0x40, O_TRUNC=0x200, O_APPEND=0x400
		oRDONLY = 0
		oRDWR = 2
		oCreateTrunc = 2 | 0x40 | 0x200  // O_RDWR|O_CREAT|O_TRUNC
		oCreateAppend = 2 | 0x40 | 0x400 // O_RDWR|O_CREAT|O_APPEND
	}

	fn := module.NewFunc("pal_file_open", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr),
		ir.NewParam("mode", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// Map mode to flags using select chain
	isRead := entry.NewICmp(enum.IPredEQ, fn.Params[1], constant.NewInt(irtypes.I32, 1))
	isCreate := entry.NewICmp(enum.IPredEQ, fn.Params[1], constant.NewInt(irtypes.I32, 2))
	isAppend := entry.NewICmp(enum.IPredEQ, fn.Params[1], constant.NewInt(irtypes.I32, 3))

	f1 := entry.NewSelect(isRead, constant.NewInt(irtypes.I32, oRDONLY), constant.NewInt(irtypes.I32, oRDWR))
	f2 := entry.NewSelect(isCreate, constant.NewInt(irtypes.I32, oCreateTrunc), f1)
	flags := entry.NewSelect(isAppend, constant.NewInt(irtypes.I32, oCreateAppend), f2)

	// open(path, flags, 0644)
	fd := entry.NewCall(openFn, fn.Params[0], flags, constant.NewInt(irtypes.I32, 0644))

	// On failure (fd < 0), return -errno instead of -1
	isErr := entry.NewICmp(enum.IPredSLT, fd, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(fd)
	return fn
}

// EmitFileRead declares libc @read and defines @pal_file_read.
func (p *PosixPAL) EmitFileRead(module *ir.Module) *ir.Func {
	// declare i64 @read(i32, i8*, i64) nounwind
	readFn := module.NewFunc("read", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("nbyte", irtypes.I64))
	readFn.FuncAttrs = append(readFn.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_file_read", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(readFn, fn.Params[0], fn.Params[1], fn.Params[2])

	// On failure (ret < 0), return -errno
	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I64, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI64(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitFileWrite declares libc @write (reuses existing declaration if present)
// and defines @pal_file_write.
func (p *PosixPAL) EmitFileWrite(module *ir.Module) *ir.Func {
	// Reuse existing write declaration if already emitted by EmitWrite
	writeFn := lookupFunc(module, "write")
	if writeFn == nil {
		writeFn = module.NewFunc("write", irtypes.I64,
			ir.NewParam("fd", irtypes.I32),
			ir.NewParam("buf", irtypes.I8Ptr),
			ir.NewParam("len", irtypes.I64))
		writeFn.FuncAttrs = append(writeFn.FuncAttrs, enum.FuncAttrNoUnwind)
	}

	fn := module.NewFunc("pal_file_write", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(writeFn, fn.Params[0], fn.Params[1], fn.Params[2])

	// On failure (ret < 0), return -errno
	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I64, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI64(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitFileClose declares libc @close and defines @pal_file_close.
func (p *PosixPAL) EmitFileClose(module *ir.Module) *ir.Func {
	closeFn := module.NewFunc("close", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))
	closeFn.FuncAttrs = append(closeFn.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_file_close", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(closeFn, fn.Params[0])

	// On failure (ret < 0), return -errno
	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitFileSeek declares libc @lseek and defines @pal_file_seek.
func (p *PosixPAL) EmitFileSeek(module *ir.Module) *ir.Func {
	lseekFn := module.NewFunc("lseek", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("offset", irtypes.I64),
		ir.NewParam("whence", irtypes.I32))
	lseekFn.FuncAttrs = append(lseekFn.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_file_seek", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("offset", irtypes.I64),
		ir.NewParam("whence", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(lseekFn, fn.Params[0], fn.Params[1], fn.Params[2])

	// On failure (ret < 0), return -errno
	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I64, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI64(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitFileStatSize defines @pal_file_stat_size using open+lseek+close.
// Avoids struct stat layout differences between macOS and Linux.
func (p *PosixPAL) EmitFileStatSize(module *ir.Module) *ir.Func {
	openFn := lookupFunc(module, "open")
	closeFn := lookupFunc(module, "close")
	lseekFn := lookupFunc(module, "lseek")

	fn := module.NewFunc("pal_file_stat_size", irtypes.I64,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	failBlk := fn.NewBlock(".fail")
	gotFdBlk := fn.NewBlock(".got_fd")

	// open(path, O_RDONLY=0, 0)
	fd := entry.NewCall(openFn, fn.Params[0], constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	isNeg := entry.NewICmp(enum.IPredSLT, fd, constant.NewInt(irtypes.I32, 0))
	entry.NewCondBr(isNeg, failBlk, gotFdBlk)

	// lseek(fd, 0, SEEK_END=2)
	size := gotFdBlk.NewCall(lseekFn, fd, constant.NewInt(irtypes.I64, 0), constant.NewInt(irtypes.I32, 2))
	gotFdBlk.NewCall(closeFn, fd)
	gotFdBlk.NewRet(size)

	p.emitNegErrnoReturnI64(failBlk, p.getOrDeclareErrnoLocFn(module))
	return fn
}

// EmitFileRemove declares libc @unlink and defines @pal_file_remove.
func (p *PosixPAL) EmitFileRemove(module *ir.Module) *ir.Func {
	unlinkFn := module.NewFunc("unlink", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	unlinkFn.FuncAttrs = append(unlinkFn.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_file_remove", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(unlinkFn, fn.Params[0])

	// On failure (ret < 0), return -errno
	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitFileExists declares libc @access and defines @pal_file_exists.
// Uses access(path, F_OK=0) to check existence.
func (p *PosixPAL) EmitFileExists(module *ir.Module) *ir.Func {
	accessFn := module.NewFunc("access", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr),
		ir.NewParam("amode", irtypes.I32))
	accessFn.FuncAttrs = append(accessFn.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_file_exists", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// access(path, F_OK=0) returns 0 on success, -1 on failure
	ret := entry.NewCall(accessFn, fn.Params[0], constant.NewInt(irtypes.I32, 0))
	isZero := entry.NewICmp(enum.IPredEQ, ret, constant.NewInt(irtypes.I32, 0))
	result := entry.NewSelect(isZero, constant.NewInt(irtypes.I32, 1), constant.NewInt(irtypes.I32, 0))
	entry.NewRet(result)
	return fn
}

// EmitFileMkdir declares libc @mkdir and defines @pal_file_mkdir.
// Uses mode 0755 (493 decimal).
func (p *PosixPAL) EmitFileMkdir(module *ir.Module) *ir.Func {
	mkdirFn := module.NewFunc("mkdir", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr),
		ir.NewParam("mode", irtypes.I32))
	mkdirFn.FuncAttrs = append(mkdirFn.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_file_mkdir", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(mkdirFn, fn.Params[0], constant.NewInt(irtypes.I32, 0755))

	// On failure (ret < 0), return -errno
	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitDirRemove declares libc @rmdir and defines @pal_dir_remove.
func (p *PosixPAL) EmitDirRemove(module *ir.Module) *ir.Func {
	rmdirFn := module.NewFunc("rmdir", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	rmdirFn.FuncAttrs = append(rmdirFn.FuncAttrs, enum.FuncAttrNoUnwind)

	fn := module.NewFunc("pal_dir_remove", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(rmdirFn, fn.Params[0])

	// On failure (ret < 0), return -errno
	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitDirExists declares libc @opendir/@closedir and defines @pal_dir_exists.
// Uses opendir(path) to test if the path is a directory.
func (p *PosixPAL) EmitDirExists(module *ir.Module) *ir.Func {
	opendirFn := p.getOrDeclareOpendir(module)
	closedirFn := p.getOrDeclareClosedir(module)

	fn := module.NewFunc("pal_dir_exists", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	yesBlk := fn.NewBlock(".yes")
	noBlk := fn.NewBlock(".no")

	dir := entry.NewCall(opendirFn, fn.Params[0])
	isNull := entry.NewICmp(enum.IPredEQ, dir, constant.NewNull(irtypes.I8Ptr))
	entry.NewCondBr(isNull, noBlk, yesBlk)

	yesBlk.NewCall(closedirFn, dir)
	yesBlk.NewRet(constant.NewInt(irtypes.I32, 1))

	noBlk.NewRet(constant.NewInt(irtypes.I32, 0))
	return fn
}

// EmitErrno defines @pal_errno using the platform-specific errno location function.
// Linux: __errno_location(), macOS: __error(). Both return i32*.
func (p *PosixPAL) EmitErrno(module *ir.Module) *ir.Func {
	errnoLocFn := p.getOrDeclareErrnoLocFn(module)

	fn := module.NewFunc("pal_errno", irtypes.I32)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	ptr := entry.NewCall(errnoLocFn)
	val := entry.NewLoad(irtypes.I32, ptr)
	entry.NewRet(val)
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
	if p.isMacOS() {
		scNprocessorsOnln = 58
	}

	// define i32 @pal_num_cpus() nounwind
	fn := module.NewFunc("pal_num_cpus", irtypes.I32)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	n := entry.NewCall(sysconfFn, constant.NewInt(irtypes.I32, scNprocessorsOnln))
	// Clamp to at least 1
	isLess := entry.NewICmp(enum.IPredSLT, n, constant.NewInt(irtypes.I64, 1))
	clamped := entry.NewSelect(isLess, constant.NewInt(irtypes.I64, 1), n)
	result := entry.NewTrunc(clamped, irtypes.I32)
	entry.NewRet(result)
	return fn
}

// --- POSIX directory listing (Phase D) ---

// EmitDirOpen declares libc @opendir and defines @pal_dir_open.
// Signature: @pal_dir_open(i8* path) → i8* (DIR* or null on error)
func (p *PosixPAL) EmitDirOpen(module *ir.Module) *ir.Func {
	opendirFn := p.getOrDeclareOpendir(module)

	// define i8* @pal_dir_open(i8* %path) nounwind
	fn := module.NewFunc("pal_dir_open", irtypes.I8Ptr,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	dirp := entry.NewCall(opendirFn, fn.Params[0])
	entry.NewRet(dirp)
	return fn
}

// EmitDirNextName declares libc @readdir and defines @pal_dir_next_name.
// Returns pointer to d_name within the dirent struct, or null when done/error.
// Clears errno before calling readdir so null+errno==0 means end-of-directory.
// Signature: @pal_dir_next_name(i8* handle) → i8* (name or null)
func (p *PosixPAL) EmitDirNextName(module *ir.Module) *ir.Func {
	readdirFn := p.getOrDeclareReaddir(module)

	errnoLocFn := p.getOrDeclareErrnoLocFn(module)

	// d_name byte offset within struct dirent: macOS=21, Linux=19
	dNameOffset := int64(19) // Linux
	if p.isMacOS() {
		dNameOffset = 21
	}

	// define i8* @pal_dir_next_name(i8* %handle) nounwind
	fn := module.NewFunc("pal_dir_next_name", irtypes.I8Ptr,
		ir.NewParam("handle", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// Clear errno before readdir so we can distinguish end-of-dir from error
	errnoPtr := entry.NewCall(errnoLocFn)
	entry.NewStore(constant.NewInt(irtypes.I32, 0), errnoPtr)

	dirent := entry.NewCall(readdirFn, fn.Params[0])

	isNull := entry.NewICmp(enum.IPredEQ, dirent, constant.NewNull(irtypes.I8Ptr))
	gotEntry := fn.NewBlock(".got_entry")
	nullBlk := fn.NewBlock(".null")
	entry.NewCondBr(isNull, nullBlk, gotEntry)

	// null result — either end-of-dir or error (errno distinguishes)
	nullBlk.NewRet(constant.NewNull(irtypes.I8Ptr))

	// Got a dirent — GEP by byte offset to d_name
	namePtr := gotEntry.NewGetElementPtr(irtypes.I8, dirent,
		constant.NewInt(irtypes.I64, dNameOffset))
	gotEntry.NewRet(namePtr)

	return fn
}

// EmitDirClose declares libc @closedir and defines @pal_dir_close.
// Signature: @pal_dir_close(i8* handle) → void
func (p *PosixPAL) EmitDirClose(module *ir.Module) *ir.Func {
	closedirFn := p.getOrDeclareClosedir(module)

	// define void @pal_dir_close(i8* %handle) nounwind
	fn := module.NewFunc("pal_dir_close", irtypes.Void,
		ir.NewParam("handle", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewCall(closedirFn, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// EmitGetEnv declares libc @getenv and defines @pal_getenv.
// Signature: @pal_getenv(i8* name) → i8* (value or null)
// Returns a pointer to the environment value string, or null if not found.
// The returned pointer refers to the process environment — caller must NOT free it.
func (p *PosixPAL) EmitGetEnv(module *ir.Module) *ir.Func {
	// declare i8* @getenv(i8* name) nounwind
	getenvFn := module.NewFunc("getenv", irtypes.I8Ptr,
		ir.NewParam("name", irtypes.I8Ptr))
	getenvFn.FuncAttrs = append(getenvFn.FuncAttrs, enum.FuncAttrNoUnwind)

	// define i8* @pal_getenv(i8* %name) nounwind
	fn := module.NewFunc("pal_getenv", irtypes.I8Ptr,
		ir.NewParam("name", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	result := entry.NewCall(getenvFn, fn.Params[0])
	entry.NewRet(result)
	return fn
}

// EmitGetCwd declares libc @getcwd and defines @pal_getcwd.
// Signature: @pal_getcwd(i8* buf, i64 len) → i8* (buf or null)
// On success returns buf filled with the cwd path. On failure returns null and sets errno.
func (p *PosixPAL) EmitGetCwd(module *ir.Module) *ir.Func {
	// declare i8* @getcwd(i8* buf, i64 size) nounwind
	getcwdFn := module.NewFunc("getcwd", irtypes.I8Ptr,
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("size", irtypes.I64))
	getcwdFn.FuncAttrs = append(getcwdFn.FuncAttrs, enum.FuncAttrNoUnwind)

	// define i8* @pal_getcwd(i8* %buf, i64 %len) nounwind
	fn := module.NewFunc("pal_getcwd", irtypes.I8Ptr,
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	result := entry.NewCall(getcwdFn, fn.Params[0], fn.Params[1])
	entry.NewRet(result)
	return fn
}
