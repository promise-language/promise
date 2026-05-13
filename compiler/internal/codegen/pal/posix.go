package pal

import (
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// PosixPAL implements PAL for POSIX systems (macOS, Linux) using libc write/exit.
type PosixPAL struct {
	target    string // LLVM target triple (needed for platform-specific constants)
	DebugFree bool   // poison-fill freed memory for UAF detection
}

// EmitWrite declares libc @write and defines @pal_write as a thin wrapper.
// Signature: @pal_write(i32 %fd, i8* %buf, i64 %len) → i64
func (p *PosixPAL) EmitWrite(module *ir.Module) *ir.Func {
	// declare i64 @write(i32, i8*, i64)
	writeFn := getOrDeclareFunc(module, "write", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))

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
	exitFn := getOrDeclareFunc(module, "exit", irtypes.Void,
		ir.NewParam("code", irtypes.I32))
	addFuncAttr(exitFn, enum.FuncAttrNoReturn)

	// define void @pal_exit(i32 %code) noreturn
	fn := module.NewFunc("pal_exit", irtypes.Void,
		ir.NewParam("code", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoReturn, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewCall(exitFn, fn.Params[0])
	entry.NewUnreachable()

	return fn
}

func (p *PosixPAL) EmitAlloc(module *ir.Module) *ir.Func { return emitLibcAlloc(module) }
func (p *PosixPAL) EmitFree(module *ir.Module) *ir.Func {
	if p.DebugFree {
		// macOS uses malloc_size(); Linux/other POSIX uses malloc_usable_size().
		sizeFn := "malloc_usable_size"
		if strings.Contains(p.target, "darwin") || strings.Contains(p.target, "apple") {
			sizeFn = "malloc_size"
		}
		return emitLibcFreeDebug(module, sizeFn)
	}
	return emitLibcFree(module)
}
func (p *PosixPAL) EmitRealloc(module *ir.Module) *ir.Func { return emitLibcRealloc(module) }

// --- POSIX threading via pthreads ---

// EmitThreadCreate declares pthread_create and defines @pal_thread_create.
// Allocates 8 bytes for pthread_t, spawns a thread, returns handle.
// Sets explicit 2MB stack size (musl defaults to 128KB which is too small).
func (p *PosixPAL) EmitThreadCreate(module *ir.Module) *ir.Func {
	palAlloc := lookupFunc(module, "pal_alloc")

	// declare i32 @pthread_attr_init(i8*) nounwind
	pthreadAttrInit := getOrDeclareFunc(module, "pthread_attr_init", irtypes.I32,
		ir.NewParam("attr", irtypes.I8Ptr))

	// declare i32 @pthread_attr_setstacksize(i8*, i64) nounwind
	pthreadAttrSetStackSize := getOrDeclareFunc(module, "pthread_attr_setstacksize", irtypes.I32,
		ir.NewParam("attr", irtypes.I8Ptr),
		ir.NewParam("stacksize", irtypes.I64))

	// declare i32 @pthread_attr_setguardsize(i8*, i64) nounwind
	// Explicit guard page (B0010) — ensures a PROT_NONE page at the bottom
	// of the thread stack. Hits SIGSEGV on stack overflow instead of silent corruption.
	pthreadAttrSetGuardSize := getOrDeclareFunc(module, "pthread_attr_setguardsize", irtypes.I32,
		ir.NewParam("attr", irtypes.I8Ptr),
		ir.NewParam("guardsize", irtypes.I64))

	// declare i32 @pthread_attr_destroy(i8*) nounwind
	pthreadAttrDestroy := getOrDeclareFunc(module, "pthread_attr_destroy", irtypes.I32,
		ir.NewParam("attr", irtypes.I8Ptr))

	// declare i32 @pthread_create(i8*, i8*, i8*(i8*)*, i8*) nounwind
	pthreadCreate := getOrDeclareFunc(module, "pthread_create", irtypes.I32,
		ir.NewParam("thread", irtypes.I8Ptr),
		ir.NewParam("attr", irtypes.I8Ptr),
		ir.NewParam("start_routine", threadFnPtrType()),
		ir.NewParam("arg", irtypes.I8Ptr))

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
	entry.NewCall(pthreadAttrSetGuardSize, attr, constant.NewInt(irtypes.I64, 4096))

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
	pthreadJoin := getOrDeclareFunc(module, "pthread_join", irtypes.I32,
		ir.NewParam("thread", irtypes.I8Ptr),
		ir.NewParam("retval", irtypes.NewPointer(irtypes.I8Ptr)))

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
	pthreadMutexInit := getOrDeclareFunc(module, "pthread_mutex_init", irtypes.I32,
		ir.NewParam("mutex", irtypes.I8Ptr),
		ir.NewParam("attr", irtypes.I8Ptr))

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
	pthreadMutexLock := getOrDeclareFunc(module, "pthread_mutex_lock", irtypes.I32,
		ir.NewParam("mutex", irtypes.I8Ptr))

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
	pthreadMutexUnlock := getOrDeclareFunc(module, "pthread_mutex_unlock", irtypes.I32,
		ir.NewParam("mutex", irtypes.I8Ptr))

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

	pthreadMutexDestroy := getOrDeclareFunc(module, "pthread_mutex_destroy", irtypes.I32,
		ir.NewParam("mutex", irtypes.I8Ptr))

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

	pthreadCondInit := getOrDeclareFunc(module, "pthread_cond_init", irtypes.I32,
		ir.NewParam("cond", irtypes.I8Ptr),
		ir.NewParam("attr", irtypes.I8Ptr))

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
	pthreadCondWait := getOrDeclareFunc(module, "pthread_cond_wait", irtypes.I32,
		ir.NewParam("cond", irtypes.I8Ptr),
		ir.NewParam("mutex", irtypes.I8Ptr))

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
	pthreadCondSignal := getOrDeclareFunc(module, "pthread_cond_signal", irtypes.I32,
		ir.NewParam("cond", irtypes.I8Ptr))

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
	pthreadCondBroadcast := getOrDeclareFunc(module, "pthread_cond_broadcast", irtypes.I32,
		ir.NewParam("cond", irtypes.I8Ptr))

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

	pthreadCondDestroy := getOrDeclareFunc(module, "pthread_cond_destroy", irtypes.I32,
		ir.NewParam("cond", irtypes.I8Ptr))

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
	return getOrDeclareFunc(module, name, irtypes.NewPointer(irtypes.I32))
}

// getOrDeclareOpendir returns (or declares) libc @opendir(i8*) → i8*.
func (p *PosixPAL) getOrDeclareOpendir(module *ir.Module) *ir.Func {
	return getOrDeclareFunc(module, "opendir", irtypes.I8Ptr,
		ir.NewParam("dirname", irtypes.I8Ptr))
}

// getOrDeclareClosedir returns (or declares) libc @closedir(i8*) → i32.
func (p *PosixPAL) getOrDeclareClosedir(module *ir.Module) *ir.Func {
	return getOrDeclareFunc(module, "closedir", irtypes.I32,
		ir.NewParam("dirp", irtypes.I8Ptr))
}

// getOrDeclareReaddir returns (or declares) libc @readdir(i8*) → i8*.
func (p *PosixPAL) getOrDeclareReaddir(module *ir.Module) *ir.Func {
	return getOrDeclareFunc(module, "readdir", irtypes.I8Ptr,
		ir.NewParam("dirp", irtypes.I8Ptr))
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
	openFn := getOrDeclareFunc(module, "open", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr),
		ir.NewParam("oflag", irtypes.I32))
	openFn.Sig.Variadic = true

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
	readFn := getOrDeclareFunc(module, "read", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("nbyte", irtypes.I64))

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
	writeFn := getOrDeclareFunc(module, "write", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))

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
	closeFn := getOrDeclareFunc(module, "close", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))

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
	lseekFn := getOrDeclareFunc(module, "lseek", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("offset", irtypes.I64),
		ir.NewParam("whence", irtypes.I32))

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
	openFn := getOrDeclareFunc(module, "open", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr),
		ir.NewParam("oflag", irtypes.I32))
	openFn.Sig.Variadic = true // open(path, oflag, ...) — mode is variadic
	closeFn := getOrDeclareFunc(module, "close", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))
	lseekFn := getOrDeclareFunc(module, "lseek", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("offset", irtypes.I64),
		ir.NewParam("whence", irtypes.I32))

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
	unlinkFn := getOrDeclareFunc(module, "unlink", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))

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
	accessFn := getOrDeclareFunc(module, "access", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr),
		ir.NewParam("amode", irtypes.I32))

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
	mkdirFn := getOrDeclareFunc(module, "mkdir", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr),
		ir.NewParam("mode", irtypes.I32))

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
	rmdirFn := getOrDeclareFunc(module, "rmdir", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))

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
	sysconfFn := getOrDeclareFunc(module, "sysconf", irtypes.I64,
		ir.NewParam("name", irtypes.I32))

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

// EmitSpawn defines @pal_spawn using fork+execvp+pipe.
// Signature: @pal_spawn(i8* program, i8** argv, i32* out_stdout_fd, i32* out_stderr_fd) → i32
// Returns child pid on success, -1 on error.
// Output params receive the read ends of stdout/stderr pipes (caller must close).
func (p *PosixPAL) EmitSpawn(module *ir.Module) *ir.Func {
	closeFn := getOrDeclareFunc(module, "close", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))

	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)
	i32PtrType := irtypes.NewPointer(irtypes.I32)

	// declare i32 @pipe(i32*) nounwind
	pipeFn := getOrDeclareFunc(module, "pipe", irtypes.I32,
		ir.NewParam("fds", i32PtrType))

	// declare i32 @fork() nounwind
	forkFn := getOrDeclareFunc(module, "fork", irtypes.I32)

	// declare i32 @dup2(i32, i32) nounwind
	dup2Fn := getOrDeclareFunc(module, "dup2", irtypes.I32,
		ir.NewParam("oldfd", irtypes.I32),
		ir.NewParam("newfd", irtypes.I32))

	// declare i32 @execvp(i8*, i8**) nounwind
	execvpFn := getOrDeclareFunc(module, "execvp", irtypes.I32,
		ir.NewParam("file", irtypes.I8Ptr),
		ir.NewParam("argv", i8PtrPtrType))

	// declare void @_exit(i32) noreturn nounwind
	exitFn := getOrDeclareFunc(module, "_exit", irtypes.Void,
		ir.NewParam("status", irtypes.I32))
	addFuncAttr(exitFn, enum.FuncAttrNoReturn)

	fn := module.NewFunc("pal_spawn", irtypes.I32,
		ir.NewParam("program", irtypes.I8Ptr),
		ir.NewParam("argv", i8PtrPtrType),
		ir.NewParam("out_stdout_fd", i32PtrType),
		ir.NewParam("out_stderr_fd", i32PtrType))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	zero32 := constant.NewInt(irtypes.I32, 0)
	one32 := constant.NewInt(irtypes.I32, 1)
	two32 := constant.NewInt(irtypes.I32, 2)
	negOne32 := constant.NewInt(irtypes.I32, -1)

	storeErrorFds := func(blk *ir.Block) {
		blk.NewStore(negOne32, fn.Params[2])
		blk.NewStore(negOne32, fn.Params[3])
	}

	entry := fn.NewBlock(".entry")

	// Allocate pipe fd arrays on stack: [2 x i32] for stdout and stderr
	stdoutFds := entry.NewAlloca(irtypes.NewArray(2, irtypes.I32))
	stderrFds := entry.NewAlloca(irtypes.NewArray(2, irtypes.I32))
	stdoutFdsPtr := entry.NewBitCast(stdoutFds, i32PtrType)
	stderrFdsPtr := entry.NewBitCast(stderrFds, i32PtrType)

	// pipe(stdout_fds)
	pipeRet1 := entry.NewCall(pipeFn, stdoutFdsPtr)
	isPipeErr1 := entry.NewICmp(enum.IPredSLT, pipeRet1, zero32)
	pipeOk1 := fn.NewBlock(".pipe_ok1")
	pipe1ErrBlk := fn.NewBlock(".pipe1_err")
	entry.NewCondBr(isPipeErr1, pipe1ErrBlk, pipeOk1)

	storeErrorFds(pipe1ErrBlk)
	pipe1ErrBlk.NewRet(negOne32)

	// pipe(stderr_fds)
	pipeRet2 := pipeOk1.NewCall(pipeFn, stderrFdsPtr)
	isPipeErr2 := pipeOk1.NewICmp(enum.IPredSLT, pipeRet2, zero32)
	pipeOk2 := fn.NewBlock(".pipe_ok2")
	pipe2ErrBlk := fn.NewBlock(".pipe2_err")
	pipeOk1.NewCondBr(isPipeErr2, pipe2ErrBlk, pipeOk2)

	// pipe2 error: close stdout pipe fds
	stdoutRdPtrErr := pipe2ErrBlk.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), stdoutFds, zero32, zero32)
	stdoutWrPtrErr := pipe2ErrBlk.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), stdoutFds, zero32, one32)
	pipe2ErrBlk.NewCall(closeFn, pipe2ErrBlk.NewLoad(irtypes.I32, stdoutRdPtrErr))
	pipe2ErrBlk.NewCall(closeFn, pipe2ErrBlk.NewLoad(irtypes.I32, stdoutWrPtrErr))
	storeErrorFds(pipe2ErrBlk)
	pipe2ErrBlk.NewRet(negOne32)

	// Load pipe fds
	stdoutReadFdPtr := pipeOk2.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), stdoutFds, zero32, zero32)
	stdoutWriteFdPtr := pipeOk2.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), stdoutFds, zero32, one32)
	stderrReadFdPtr := pipeOk2.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), stderrFds, zero32, zero32)
	stderrWriteFdPtr := pipeOk2.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), stderrFds, zero32, one32)
	stdoutReadFd := pipeOk2.NewLoad(irtypes.I32, stdoutReadFdPtr)
	stdoutWriteFd := pipeOk2.NewLoad(irtypes.I32, stdoutWriteFdPtr)
	stderrReadFd := pipeOk2.NewLoad(irtypes.I32, stderrReadFdPtr)
	stderrWriteFd := pipeOk2.NewLoad(irtypes.I32, stderrWriteFdPtr)

	// fork()
	pid := pipeOk2.NewCall(forkFn)
	isChild := pipeOk2.NewICmp(enum.IPredEQ, pid, zero32)
	childBlk := fn.NewBlock(".child")
	checkForkErr := fn.NewBlock(".check_fork_err")
	pipeOk2.NewCondBr(isChild, childBlk, checkForkErr)

	isForkErr := checkForkErr.NewICmp(enum.IPredSLT, pid, zero32)
	parentBlk := fn.NewBlock(".parent")
	forkErrBlk := fn.NewBlock(".fork_err")
	checkForkErr.NewCondBr(isForkErr, forkErrBlk, parentBlk)

	// --- Child process ---
	childBlk.NewCall(closeFn, stdoutReadFd)
	childBlk.NewCall(closeFn, stderrReadFd)
	childBlk.NewCall(dup2Fn, stdoutWriteFd, one32)
	childBlk.NewCall(dup2Fn, stderrWriteFd, two32)
	childBlk.NewCall(closeFn, stdoutWriteFd)
	childBlk.NewCall(closeFn, stderrWriteFd)
	childBlk.NewCall(execvpFn, fn.Params[0], fn.Params[1])
	childBlk.NewCall(exitFn, constant.NewInt(irtypes.I32, 127))
	childBlk.NewUnreachable()

	// --- Fork error: close all 4 pipe fds ---
	forkErrBlk.NewCall(closeFn, stdoutReadFd)
	forkErrBlk.NewCall(closeFn, stdoutWriteFd)
	forkErrBlk.NewCall(closeFn, stderrReadFd)
	forkErrBlk.NewCall(closeFn, stderrWriteFd)
	storeErrorFds(forkErrBlk)
	forkErrBlk.NewRet(negOne32)

	// --- Parent: close write ends, output read fds, return pid ---
	parentBlk.NewCall(closeFn, stdoutWriteFd)
	parentBlk.NewCall(closeFn, stderrWriteFd)
	parentBlk.NewStore(stdoutReadFd, fn.Params[2])
	parentBlk.NewStore(stderrReadFd, fn.Params[3])
	parentBlk.NewRet(pid)

	return fn
}

// EmitReadPipe defines @pal_read_pipe: reads an fd to EOF into malloc'd buffer, then closes fd.
// Signature: @pal_read_pipe(i32 fd, i8** out_buf, i64* out_len) → void
// Caller must free out_buf after use.
func (p *PosixPAL) EmitReadPipe(module *ir.Module) *ir.Func {
	palAlloc := lookupFunc(module, "pal_alloc")
	palRealloc := lookupFunc(module, "pal_realloc")
	readFn := getOrDeclareFunc(module, "read", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("nbyte", irtypes.I64))
	closeFn := getOrDeclareFunc(module, "close", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))

	fn := module.NewFunc("pal_read_pipe", irtypes.Void,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("out_buf", irtypes.NewPointer(irtypes.I8Ptr)),
		ir.NewParam("out_len", irtypes.NewPointer(irtypes.I64)))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	initCap := constant.NewInt(irtypes.I64, 4096)
	buf := entry.NewCall(palAlloc, initCap)
	capPtr := entry.NewAlloca(irtypes.I64)
	entry.NewStore(initCap, capPtr)
	bufPtr := entry.NewAlloca(irtypes.I8Ptr)
	entry.NewStore(buf, bufPtr)
	totalPtr := entry.NewAlloca(irtypes.I64)
	entry.NewStore(constant.NewInt(irtypes.I64, 0), totalPtr)
	loopBlk := fn.NewBlock(".loop")
	entry.NewBr(loopBlk)

	readOkBlk := fn.NewBlock(".read_ok")
	growBlk := fn.NewBlock(".grow")
	doneBlk := fn.NewBlock(".done")

	curCap := loopBlk.NewLoad(irtypes.I64, capPtr)
	curBuf := loopBlk.NewLoad(irtypes.I8Ptr, bufPtr)
	curTotal := loopBlk.NewLoad(irtypes.I64, totalPtr)
	space := loopBlk.NewSub(curCap, curTotal)
	readPtr := loopBlk.NewGetElementPtr(irtypes.I8, curBuf, curTotal)
	n := loopBlk.NewCall(readFn, fn.Params[0], readPtr, space)
	isDone := loopBlk.NewICmp(enum.IPredSLE, n, constant.NewInt(irtypes.I64, 0))
	loopBlk.NewCondBr(isDone, doneBlk, readOkBlk)

	newTotal := readOkBlk.NewAdd(curTotal, n)
	readOkBlk.NewStore(newTotal, totalPtr)
	isFull := readOkBlk.NewICmp(enum.IPredEQ, newTotal, curCap)
	readOkBlk.NewCondBr(isFull, growBlk, loopBlk)

	newCap := growBlk.NewMul(curCap, constant.NewInt(irtypes.I64, 2))
	growBlk.NewStore(newCap, capPtr)
	newBuf := growBlk.NewCall(palRealloc, curBuf, newCap)
	growBlk.NewStore(newBuf, bufPtr)
	growBlk.NewBr(loopBlk)

	// done: close fd, store results
	doneBlk.NewCall(closeFn, fn.Params[0])
	finalBuf := doneBlk.NewLoad(irtypes.I8Ptr, bufPtr)
	finalTotal := doneBlk.NewLoad(irtypes.I64, totalPtr)
	doneBlk.NewStore(finalBuf, fn.Params[1])
	doneBlk.NewStore(finalTotal, fn.Params[2])
	doneBlk.NewRet(nil)

	return fn
}

// EmitWaitPid defines @pal_wait_pid using waitpid with EINTR retry.
// Signature: @pal_wait_pid(i32 pid) → i32
// Returns exit code (0-255) on success, -1 on error.
func (p *PosixPAL) EmitWaitPid(module *ir.Module) *ir.Func {
	i32PtrType := irtypes.NewPointer(irtypes.I32)

	// declare i32 @waitpid(i32, i32*, i32) nounwind
	waitpidFn := getOrDeclareFunc(module, "waitpid", irtypes.I32,
		ir.NewParam("pid", irtypes.I32),
		ir.NewParam("status", i32PtrType),
		ir.NewParam("options", irtypes.I32))

	fn := module.NewFunc("pal_wait_pid", irtypes.I32,
		ir.NewParam("pid", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	zero32 := constant.NewInt(irtypes.I32, 0)

	entry := fn.NewBlock(".entry")
	statusPtr := entry.NewAlloca(irtypes.I32)
	entry.NewStore(zero32, statusPtr)
	errnoLocFn := p.getOrDeclareErrnoLocFn(module)

	waitBlk := fn.NewBlock(".waitpid")
	entry.NewBr(waitBlk)

	wpRet := waitBlk.NewCall(waitpidFn, fn.Params[0], statusPtr, zero32)
	wpFailed := waitBlk.NewICmp(enum.IPredSLT, wpRet, zero32)
	waitOkBlk := fn.NewBlock(".waitpid_ok")
	waitErrBlk := fn.NewBlock(".waitpid_err")
	waitBlk.NewCondBr(wpFailed, waitErrBlk, waitOkBlk)

	// waitpid error: retry if EINTR (errno == 4)
	errnoPtr := waitErrBlk.NewCall(errnoLocFn)
	errnoVal := waitErrBlk.NewLoad(irtypes.I32, errnoPtr)
	isEINTR := waitErrBlk.NewICmp(enum.IPredEQ, errnoVal, constant.NewInt(irtypes.I32, 4))
	waitFatalBlk := fn.NewBlock(".waitpid_fatal")
	waitErrBlk.NewCondBr(isEINTR, waitBlk, waitFatalBlk)

	waitFatalBlk.NewRet(constant.NewInt(irtypes.I32, -1))

	// WEXITSTATUS(status) = (status >> 8) & 0xFF
	status := waitOkBlk.NewLoad(irtypes.I32, statusPtr)
	shifted := waitOkBlk.NewLShr(status, constant.NewInt(irtypes.I32, 8))
	exitCode := waitOkBlk.NewAnd(shifted, constant.NewInt(irtypes.I32, 0xFF))
	waitOkBlk.NewRet(exitCode)

	return fn
}

// EmitSpawnStreaming defines @pal_spawn_streaming using fork+execvp+pipe.
// Like EmitSpawn but also creates a stdin pipe.
// Signature: @pal_spawn_streaming(i8* program, i8** argv, i32* out_stdin_fd, i32* out_stdout_fd, i32* out_stderr_fd) → i32
// Returns child pid on success, -1 on error.
// out_stdin_fd receives the write end of the stdin pipe; out_stdout/stderr_fd receive read ends.
func (p *PosixPAL) EmitSpawnStreaming(module *ir.Module) *ir.Func {
	closeFn := getOrDeclareFunc(module, "close", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))

	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)
	i32PtrType := irtypes.NewPointer(irtypes.I32)

	pipeFn := getOrDeclareFunc(module, "pipe", irtypes.I32, ir.NewParam("fds", i32PtrType))
	forkFn := getOrDeclareFunc(module, "fork", irtypes.I32)
	dup2Fn := getOrDeclareFunc(module, "dup2", irtypes.I32, ir.NewParam("oldfd", irtypes.I32), ir.NewParam("newfd", irtypes.I32))
	execvpFn := getOrDeclareFunc(module, "execvp", irtypes.I32, ir.NewParam("file", irtypes.I8Ptr), ir.NewParam("argv", i8PtrPtrType))
	exitFn := getOrDeclareFunc(module, "_exit", irtypes.Void, ir.NewParam("status", irtypes.I32))
	addFuncAttr(exitFn, enum.FuncAttrNoReturn)

	fn := module.NewFunc("pal_spawn_streaming", irtypes.I32,
		ir.NewParam("program", irtypes.I8Ptr),
		ir.NewParam("argv", i8PtrPtrType),
		ir.NewParam("out_stdin_fd", i32PtrType),
		ir.NewParam("out_stdout_fd", i32PtrType),
		ir.NewParam("out_stderr_fd", i32PtrType))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	zero32 := constant.NewInt(irtypes.I32, 0)
	one32 := constant.NewInt(irtypes.I32, 1)
	two32 := constant.NewInt(irtypes.I32, 2)
	negOne32 := constant.NewInt(irtypes.I32, -1)

	storeErrorFds := func(blk *ir.Block) {
		blk.NewStore(negOne32, fn.Params[2])
		blk.NewStore(negOne32, fn.Params[3])
		blk.NewStore(negOne32, fn.Params[4])
	}

	entry := fn.NewBlock(".entry")

	// Allocate pipe fd arrays on stack: [2 x i32] for stdin, stdout, stderr
	stdinFds := entry.NewAlloca(irtypes.NewArray(2, irtypes.I32))
	stdoutFds := entry.NewAlloca(irtypes.NewArray(2, irtypes.I32))
	stderrFds := entry.NewAlloca(irtypes.NewArray(2, irtypes.I32))
	stdinFdsPtr := entry.NewBitCast(stdinFds, i32PtrType)
	stdoutFdsPtr := entry.NewBitCast(stdoutFds, i32PtrType)
	stderrFdsPtr := entry.NewBitCast(stderrFds, i32PtrType)

	// pipe(stdin_fds)
	pipeRet0 := entry.NewCall(pipeFn, stdinFdsPtr)
	isPipeErr0 := entry.NewICmp(enum.IPredSLT, pipeRet0, zero32)
	pipeOk0 := fn.NewBlock(".pipe_ok0")
	pipe0ErrBlk := fn.NewBlock(".pipe0_err")
	entry.NewCondBr(isPipeErr0, pipe0ErrBlk, pipeOk0)

	storeErrorFds(pipe0ErrBlk)
	pipe0ErrBlk.NewRet(negOne32)

	// pipe(stdout_fds)
	pipeRet1 := pipeOk0.NewCall(pipeFn, stdoutFdsPtr)
	isPipeErr1 := pipeOk0.NewICmp(enum.IPredSLT, pipeRet1, zero32)
	pipeOk1 := fn.NewBlock(".pipe_ok1")
	pipe1ErrBlk := fn.NewBlock(".pipe1_err")
	pipeOk0.NewCondBr(isPipeErr1, pipe1ErrBlk, pipeOk1)

	// pipe1 error: close stdin pipe fds
	stdinRdPtrErr1 := pipe1ErrBlk.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), stdinFds, zero32, zero32)
	stdinWrPtrErr1 := pipe1ErrBlk.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), stdinFds, zero32, one32)
	pipe1ErrBlk.NewCall(closeFn, pipe1ErrBlk.NewLoad(irtypes.I32, stdinRdPtrErr1))
	pipe1ErrBlk.NewCall(closeFn, pipe1ErrBlk.NewLoad(irtypes.I32, stdinWrPtrErr1))
	storeErrorFds(pipe1ErrBlk)
	pipe1ErrBlk.NewRet(negOne32)

	// pipe(stderr_fds)
	pipeRet2 := pipeOk1.NewCall(pipeFn, stderrFdsPtr)
	isPipeErr2 := pipeOk1.NewICmp(enum.IPredSLT, pipeRet2, zero32)
	pipeOk2 := fn.NewBlock(".pipe_ok2")
	pipe2ErrBlk := fn.NewBlock(".pipe2_err")
	pipeOk1.NewCondBr(isPipeErr2, pipe2ErrBlk, pipeOk2)

	// pipe2 error: close stdin + stdout pipe fds
	stdinRdPtrErr2 := pipe2ErrBlk.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), stdinFds, zero32, zero32)
	stdinWrPtrErr2 := pipe2ErrBlk.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), stdinFds, zero32, one32)
	stdoutRdPtrErr2 := pipe2ErrBlk.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), stdoutFds, zero32, zero32)
	stdoutWrPtrErr2 := pipe2ErrBlk.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), stdoutFds, zero32, one32)
	pipe2ErrBlk.NewCall(closeFn, pipe2ErrBlk.NewLoad(irtypes.I32, stdinRdPtrErr2))
	pipe2ErrBlk.NewCall(closeFn, pipe2ErrBlk.NewLoad(irtypes.I32, stdinWrPtrErr2))
	pipe2ErrBlk.NewCall(closeFn, pipe2ErrBlk.NewLoad(irtypes.I32, stdoutRdPtrErr2))
	pipe2ErrBlk.NewCall(closeFn, pipe2ErrBlk.NewLoad(irtypes.I32, stdoutWrPtrErr2))
	storeErrorFds(pipe2ErrBlk)
	pipe2ErrBlk.NewRet(negOne32)

	// Load all pipe fds
	stdinReadFdPtr := pipeOk2.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), stdinFds, zero32, zero32)
	stdinWriteFdPtr := pipeOk2.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), stdinFds, zero32, one32)
	stdoutReadFdPtr := pipeOk2.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), stdoutFds, zero32, zero32)
	stdoutWriteFdPtr := pipeOk2.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), stdoutFds, zero32, one32)
	stderrReadFdPtr := pipeOk2.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), stderrFds, zero32, zero32)
	stderrWriteFdPtr := pipeOk2.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), stderrFds, zero32, one32)
	stdinReadFd := pipeOk2.NewLoad(irtypes.I32, stdinReadFdPtr)
	stdinWriteFd := pipeOk2.NewLoad(irtypes.I32, stdinWriteFdPtr)
	stdoutReadFd := pipeOk2.NewLoad(irtypes.I32, stdoutReadFdPtr)
	stdoutWriteFd := pipeOk2.NewLoad(irtypes.I32, stdoutWriteFdPtr)
	stderrReadFd := pipeOk2.NewLoad(irtypes.I32, stderrReadFdPtr)
	stderrWriteFd := pipeOk2.NewLoad(irtypes.I32, stderrWriteFdPtr)

	// fork()
	pid := pipeOk2.NewCall(forkFn)
	isChild := pipeOk2.NewICmp(enum.IPredEQ, pid, zero32)
	childBlk := fn.NewBlock(".child")
	checkForkErr := fn.NewBlock(".check_fork_err")
	pipeOk2.NewCondBr(isChild, childBlk, checkForkErr)

	isForkErr := checkForkErr.NewICmp(enum.IPredSLT, pid, zero32)
	parentBlk := fn.NewBlock(".parent")
	forkErrBlk := fn.NewBlock(".fork_err")
	checkForkErr.NewCondBr(isForkErr, forkErrBlk, parentBlk)

	// --- Child process ---
	// Close parent ends of pipes
	childBlk.NewCall(closeFn, stdinWriteFd)
	childBlk.NewCall(closeFn, stdoutReadFd)
	childBlk.NewCall(closeFn, stderrReadFd)
	// Redirect stdin/stdout/stderr
	childBlk.NewCall(dup2Fn, stdinReadFd, zero32)  // stdin read end → fd 0
	childBlk.NewCall(dup2Fn, stdoutWriteFd, one32) // stdout write end → fd 1
	childBlk.NewCall(dup2Fn, stderrWriteFd, two32) // stderr write end → fd 2
	// Close child ends (now duplicated)
	childBlk.NewCall(closeFn, stdinReadFd)
	childBlk.NewCall(closeFn, stdoutWriteFd)
	childBlk.NewCall(closeFn, stderrWriteFd)
	childBlk.NewCall(execvpFn, fn.Params[0], fn.Params[1])
	childBlk.NewCall(exitFn, constant.NewInt(irtypes.I32, 127))
	childBlk.NewUnreachable()

	// --- Fork error: close all 6 pipe fds ---
	forkErrBlk.NewCall(closeFn, stdinReadFd)
	forkErrBlk.NewCall(closeFn, stdinWriteFd)
	forkErrBlk.NewCall(closeFn, stdoutReadFd)
	forkErrBlk.NewCall(closeFn, stdoutWriteFd)
	forkErrBlk.NewCall(closeFn, stderrReadFd)
	forkErrBlk.NewCall(closeFn, stderrWriteFd)
	storeErrorFds(forkErrBlk)
	forkErrBlk.NewRet(negOne32)

	// --- Parent: close child ends, output parent ends, return pid ---
	parentBlk.NewCall(closeFn, stdinReadFd)        // close read end of stdin pipe
	parentBlk.NewCall(closeFn, stdoutWriteFd)      // close write end of stdout pipe
	parentBlk.NewCall(closeFn, stderrWriteFd)      // close write end of stderr pipe
	parentBlk.NewStore(stdinWriteFd, fn.Params[2]) // parent writes to stdin
	parentBlk.NewStore(stdoutReadFd, fn.Params[3]) // parent reads from stdout
	parentBlk.NewStore(stderrReadFd, fn.Params[4]) // parent reads from stderr
	parentBlk.NewRet(pid)

	return fn
}

// EmitKill defines @pal_kill using POSIX kill(2).
// Signature: @pal_kill(i32 pid, i32 signal) → i32
// Returns 0 on success, -1 on error.
func (p *PosixPAL) EmitKill(module *ir.Module) *ir.Func {
	killFn := getOrDeclareFunc(module, "kill", irtypes.I32,
		ir.NewParam("pid", irtypes.I32),
		ir.NewParam("sig", irtypes.I32))

	fn := module.NewFunc("pal_kill", irtypes.I32,
		ir.NewParam("pid", irtypes.I32),
		ir.NewParam("signal", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(killFn, fn.Params[0], fn.Params[1])
	entry.NewRet(ret)

	return fn
}

// EmitGetEnv declares libc @getenv and defines @pal_getenv.
// Signature: @pal_getenv(i8* name) → i8* (value or null)
// Returns a pointer to the environment value string, or null if not found.
// The returned pointer refers to the process environment — caller must NOT free it.
func (p *PosixPAL) EmitGetEnv(module *ir.Module) *ir.Func {
	// declare i8* @getenv(i8* name) nounwind
	getenvFn := getOrDeclareFunc(module, "getenv", irtypes.I8Ptr,
		ir.NewParam("name", irtypes.I8Ptr))

	// define i8* @pal_getenv(i8* %name) nounwind
	fn := module.NewFunc("pal_getenv", irtypes.I8Ptr,
		ir.NewParam("name", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	result := entry.NewCall(getenvFn, fn.Params[0])
	entry.NewRet(result)
	return fn
}

// EmitSetEnv declares libc @setenv and defines @pal_setenv.
// Signature: @pal_setenv(i8* name, i8* value) → i32 (0=ok, -1=error)
func (p *PosixPAL) EmitSetEnv(module *ir.Module) *ir.Func {
	// declare i32 @setenv(i8* name, i8* value, i32 overwrite) nounwind
	setenvFn := getOrDeclareFunc(module, "setenv", irtypes.I32,
		ir.NewParam("name", irtypes.I8Ptr),
		ir.NewParam("value", irtypes.I8Ptr),
		ir.NewParam("overwrite", irtypes.I32))

	fn := module.NewFunc("pal_setenv", irtypes.I32,
		ir.NewParam("name", irtypes.I8Ptr),
		ir.NewParam("value", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	result := entry.NewCall(setenvFn, fn.Params[0], fn.Params[1],
		constant.NewInt(irtypes.I32, 1)) // overwrite = 1
	entry.NewRet(result)
	return fn
}

// EmitUnsetEnv declares libc @unsetenv and defines @pal_unsetenv.
// Signature: @pal_unsetenv(i8* name) → i32 (0=ok, -1=error)
func (p *PosixPAL) EmitUnsetEnv(module *ir.Module) *ir.Func {
	// declare i32 @unsetenv(i8* name) nounwind
	unsetenvFn := getOrDeclareFunc(module, "unsetenv", irtypes.I32,
		ir.NewParam("name", irtypes.I8Ptr))

	fn := module.NewFunc("pal_unsetenv", irtypes.I32,
		ir.NewParam("name", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	result := entry.NewCall(unsetenvFn, fn.Params[0])
	entry.NewRet(result)
	return fn
}

// EmitChdir declares libc @chdir and defines @pal_chdir.
// Signature: @pal_chdir(i8* path) → i32 (0=ok, -1=error)
func (p *PosixPAL) EmitChdir(module *ir.Module) *ir.Func {
	// declare i32 @chdir(i8* path) nounwind
	chdirFn := getOrDeclareFunc(module, "chdir", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))

	fn := module.NewFunc("pal_chdir", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	result := entry.NewCall(chdirFn, fn.Params[0])
	entry.NewRet(result)
	return fn
}

// EmitGetCwd declares libc @getcwd and defines @pal_getcwd.
// Signature: @pal_getcwd(i8* buf, i64 len) → i8* (buf or null)
// On success returns buf filled with the cwd path. On failure returns null and sets errno.
func (p *PosixPAL) EmitGetCwd(module *ir.Module) *ir.Func {
	// declare i8* @getcwd(i8* buf, i64 size) nounwind
	getcwdFn := getOrDeclareFunc(module, "getcwd", irtypes.I8Ptr,
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("size", irtypes.I64))

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

// EmitGetEnviron defines @pal_get_environ: returns pointer to the C environ global.
// Signature: @pal_get_environ() → i8** (null-terminated array of "KEY=VALUE" strings)
func (p *PosixPAL) EmitGetEnviron(module *ir.Module) *ir.Func {
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)

	// @environ = external global i8**
	environGlobal := module.NewGlobal("environ", i8PtrPtrType)
	environGlobal.Linkage = enum.LinkageExternal

	fn := module.NewFunc("pal_get_environ", i8PtrPtrType)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ptr := entry.NewLoad(i8PtrPtrType, environGlobal)
	entry.NewRet(ptr)
	return fn
}

// EmitGetUserInfo defines @pal_get_user_info using getuid+getpwuid.
// Signature: @pal_get_user_info(i8** out_name, i8** out_dir, i32* out_uid, i32* out_gid) → i32
// Returns 0 on success, -1 on error. out_name/out_dir point to static libc storage.
func (p *PosixPAL) EmitGetUserInfo(module *ir.Module) *ir.Func {
	i32PtrType := irtypes.NewPointer(irtypes.I32)

	// declare i32 @getuid() nounwind
	getuidFn := getOrDeclareFunc(module, "getuid", irtypes.I32)

	// struct passwd layout differs between Linux and macOS.
	// Linux:  { i8*, i8*, i32, i32, i8*, i8*, i8* }       — pw_dir at index 5
	// macOS:  { i8*, i8*, i32, i32, i64, i8*, i8*, i8*, i8*, i64 } — pw_dir at index 7
	var passwdType *irtypes.StructType
	var dirIndex int
	if p.isMacOS() {
		passwdType = irtypes.NewStruct(
			irtypes.I8Ptr, // 0: pw_name
			irtypes.I8Ptr, // 1: pw_passwd
			irtypes.I32,   // 2: pw_uid
			irtypes.I32,   // 3: pw_gid
			irtypes.I64,   // 4: pw_change
			irtypes.I8Ptr, // 5: pw_class
			irtypes.I8Ptr, // 6: pw_gecos
			irtypes.I8Ptr, // 7: pw_dir
			irtypes.I8Ptr, // 8: pw_shell
			irtypes.I64,   // 9: pw_expire
		)
		dirIndex = 7
	} else {
		passwdType = irtypes.NewStruct(
			irtypes.I8Ptr, // 0: pw_name
			irtypes.I8Ptr, // 1: pw_passwd
			irtypes.I32,   // 2: pw_uid
			irtypes.I32,   // 3: pw_gid
			irtypes.I8Ptr, // 4: pw_gecos
			irtypes.I8Ptr, // 5: pw_dir
			irtypes.I8Ptr, // 6: pw_shell
		)
		dirIndex = 5
	}

	passwdPtrType := irtypes.NewPointer(passwdType)

	// declare %struct.passwd* @getpwuid(i32) nounwind
	getpwuidFn := getOrDeclareFunc(module, "getpwuid", passwdPtrType,
		ir.NewParam("uid", irtypes.I32))

	fn := module.NewFunc("pal_get_user_info", irtypes.I32,
		ir.NewParam("out_name", irtypes.NewPointer(irtypes.I8Ptr)),
		ir.NewParam("out_dir", irtypes.NewPointer(irtypes.I8Ptr)),
		ir.NewParam("out_uid", i32PtrType),
		ir.NewParam("out_gid", i32PtrType))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	zero32 := constant.NewInt(irtypes.I32, 0)
	negOne32 := constant.NewInt(irtypes.I32, -1)

	entry := fn.NewBlock(".entry")
	uid := entry.NewCall(getuidFn)
	pw := entry.NewCall(getpwuidFn, uid)

	isNull := entry.NewICmp(enum.IPredEQ, pw, constant.NewNull(passwdPtrType))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".error")
	entry.NewCondBr(isNull, errBlk, okBlk)

	// Error: store defaults and return -1
	errBlk.NewStore(constant.NewNull(irtypes.I8Ptr), fn.Params[0])
	errBlk.NewStore(constant.NewNull(irtypes.I8Ptr), fn.Params[1])
	errBlk.NewStore(negOne32, fn.Params[2])
	errBlk.NewStore(negOne32, fn.Params[3])
	errBlk.NewRet(negOne32)

	// OK: extract fields
	namePtr := okBlk.NewGetElementPtr(passwdType, pw, zero32, constant.NewInt(irtypes.I32, 0))
	name := okBlk.NewLoad(irtypes.I8Ptr, namePtr)
	okBlk.NewStore(name, fn.Params[0])

	dirPtr := okBlk.NewGetElementPtr(passwdType, pw, zero32, constant.NewInt(irtypes.I32, int64(dirIndex)))
	dir := okBlk.NewLoad(irtypes.I8Ptr, dirPtr)
	okBlk.NewStore(dir, fn.Params[1])

	uidPtr := okBlk.NewGetElementPtr(passwdType, pw, zero32, constant.NewInt(irtypes.I32, 2))
	uidVal := okBlk.NewLoad(irtypes.I32, uidPtr)
	okBlk.NewStore(uidVal, fn.Params[2])

	gidPtr := okBlk.NewGetElementPtr(passwdType, pw, zero32, constant.NewInt(irtypes.I32, 3))
	gidVal := okBlk.NewLoad(irtypes.I32, gidPtr)
	okBlk.NewStore(gidVal, fn.Params[3])

	okBlk.NewRet(zero32)
	return fn
}

// EmitGetHostname defines @pal_get_hostname using gethostname(2).
// Signature: @pal_get_hostname(i8* buf, i64 len) → i8* (buf on success, null on error)
func (p *PosixPAL) EmitGetHostname(module *ir.Module) *ir.Func {
	// declare i32 @gethostname(i8*, i64) nounwind
	// Note: POSIX says size_t (i64 on 64-bit), but some systems use int. i64 works.
	gethostnameFn := getOrDeclareFunc(module, "gethostname", irtypes.I32,
		ir.NewParam("name", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))

	fn := module.NewFunc("pal_get_hostname", irtypes.I8Ptr,
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(gethostnameFn, fn.Params[0], fn.Params[1])
	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".error")
	entry.NewCondBr(isErr, errBlk, okBlk)

	errBlk.NewRet(constant.NewNull(irtypes.I8Ptr))
	okBlk.NewRet(fn.Params[0])
	return fn
}

// EmitSignalInit defines @pal_signal_init: creates a pipe and defines the signal handler.
// Signature: @pal_signal_init() → i32 (read fd on success, -1 on error)
// The write fd is stored in @__promise_signal_pipe_wr for the signal handler to use.
// Also defines @promise_signal_handler(i32) which writes the signal number byte to the pipe.
func (p *PosixPAL) EmitSignalInit(module *ir.Module) *ir.Func {
	i32PtrType := irtypes.NewPointer(irtypes.I32)
	zero32 := constant.NewInt(irtypes.I32, 0)
	one32 := constant.NewInt(irtypes.I32, 1)
	negOne32 := constant.NewInt(irtypes.I32, -1)

	pipeFn := getOrDeclareFunc(module, "pipe", irtypes.I32, ir.NewParam("fds", i32PtrType))
	writeFn := getOrDeclareFunc(module, "write", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("count", irtypes.I64))

	// Global to store the write fd (NOT TLS — shared across all threads for signal handler)
	wrFdGlobal := module.NewGlobal("__promise_signal_pipe_wr", irtypes.I32)
	wrFdGlobal.Init = negOne32

	// Define the signal handler: void @promise_signal_handler(i32 %signum)
	// Async-signal-safe: only uses write(2) and stack-local variables.
	handlerFn := module.NewFunc("promise_signal_handler", irtypes.Void,
		ir.NewParam("signum", irtypes.I32))
	handlerFn.FuncAttrs = append(handlerFn.FuncAttrs, enum.FuncAttrNoUnwind)
	{
		hEntry := handlerFn.NewBlock(".entry")
		// Truncate signum to i8 and store on stack
		sigByte := hEntry.NewTrunc(handlerFn.Params[0], irtypes.I8)
		buf := hEntry.NewAlloca(irtypes.I8)
		hEntry.NewStore(sigByte, buf)
		// Load write fd from global
		wrFd := hEntry.NewLoad(irtypes.I32, wrFdGlobal)
		// write(wr_fd, &byte, 1) — async-signal-safe
		hEntry.NewCall(writeFn, wrFd, buf, constant.NewInt(irtypes.I64, 1))
		hEntry.NewRet(nil)
	}

	// Define @pal_signal_init() → i32
	fn := module.NewFunc("pal_signal_init", irtypes.I32)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")
	// Create pipe
	pipeFds := entry.NewAlloca(irtypes.NewArray(2, irtypes.I32))
	pipeFdsPtr := entry.NewBitCast(pipeFds, i32PtrType)
	pipeRet := entry.NewCall(pipeFn, pipeFdsPtr)
	isPipeErr := entry.NewICmp(enum.IPredSLT, pipeRet, zero32)
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".error")
	entry.NewCondBr(isPipeErr, errBlk, okBlk)

	errBlk.NewRet(negOne32)

	// Load fds, store write fd in global, return read fd
	rdFdPtr := okBlk.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), pipeFds, zero32, zero32)
	wrFdPtr := okBlk.NewGetElementPtr(irtypes.NewArray(2, irtypes.I32), pipeFds, zero32, one32)
	rdFd := okBlk.NewLoad(irtypes.I32, rdFdPtr)
	wrFd := okBlk.NewLoad(irtypes.I32, wrFdPtr)
	okBlk.NewStore(wrFd, wrFdGlobal)
	okBlk.NewRet(rdFd)

	return fn
}

// EmitSignalRegister defines @pal_signal_register using signal(2).
// Signature: @pal_signal_register(i32 signum) → i32 (0 on success, -1 on error)
func (p *PosixPAL) EmitSignalRegister(module *ir.Module) *ir.Func {
	// declare i8* @signal(i32, i8*) nounwind
	// signal() takes a function pointer as i8* and returns previous handler as i8*
	// Use getOrDeclareFunc to avoid duplicate declaration — EmitStackOverflowInit
	// may have already declared @signal via the same helper. (B0121)
	signalFn := getOrDeclareFunc(module, "signal", irtypes.I8Ptr,
		ir.NewParam("sig", irtypes.I32),
		ir.NewParam("handler", irtypes.I8Ptr))

	// Look up the handler function defined by EmitSignalInit
	var handlerFn *ir.Func
	for _, f := range module.Funcs {
		if f.Name() == "promise_signal_handler" {
			handlerFn = f
			break
		}
	}

	fn := module.NewFunc("pal_signal_register", irtypes.I32,
		ir.NewParam("signum", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)

	entry := fn.NewBlock(".entry")

	if handlerFn == nil {
		// Handler not defined (shouldn't happen if EmitSignalInit was called first)
		entry.NewRet(constant.NewInt(irtypes.I32, -1))
		return fn
	}

	// signal(signum, &promise_signal_handler)
	handlerPtr := entry.NewBitCast(handlerFn, irtypes.I8Ptr)
	result := entry.NewCall(signalFn, fn.Params[0], handlerPtr)

	// SIG_ERR is (void(*)(int))-1, which as i8* is -1 (all-ones pointer)
	sigErr := constant.NewIntToPtr(constant.NewInt(irtypes.I64, -1), irtypes.I8Ptr)
	isErr := entry.NewICmp(enum.IPredEQ, result, sigErr)
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".error")
	entry.NewCondBr(isErr, errBlk, okBlk)

	errBlk.NewRet(constant.NewInt(irtypes.I32, -1))
	okBlk.NewRet(constant.NewInt(irtypes.I32, 0))

	return fn
}

// EmitStackOverflowInit defines @pal_stack_overflow_init() → void
// Registers a SIGSEGV handler (and SIGBUS on macOS) that prints
// a diagnostic message to stderr and calls _exit(2).
//
// On macOS: uses sigaction(SA_ONSTACK) + sigaltstack for reliable delivery
// even when the main stack is exhausted. Prints "fatal: stack overflow".
// On Linux: uses sigaction(SA_SIGINFO) to get the fault address from
// siginfo_t.si_addr, prints "fatal: segmentation fault at 0x<hex>".
// This distinguishes null pointer dereferences from stack overflows (B0128).
func (p *PosixPAL) EmitStackOverflowInit(module *ir.Module) *ir.Func {
	writeFn := getOrDeclareFunc(module, "write", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("count", irtypes.I64))

	// declare void @_exit(i32) noreturn nounwind
	exitFn := getOrDeclareFunc(module, "_exit", irtypes.Void,
		ir.NewParam("status", irtypes.I32))
	addFuncAttr(exitFn, enum.FuncAttrNoReturn)

	isDarwin := strings.Contains(p.target, "darwin") || strings.Contains(p.target, "macos")

	// Define @pal_stack_overflow_init()
	fn := module.NewFunc("pal_stack_overflow_init", irtypes.Void)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	if isDarwin {
		// macOS: simple 1-arg handler that prints "fatal: stack overflow\n".
		msgStr := "fatal: stack overflow\n"
		msgConst := constant.NewCharArrayFromString(msgStr)
		msgGlobal := module.NewGlobal("__promise_stack_overflow_msg", msgConst.Typ)
		msgGlobal.Init = msgConst
		msgGlobal.Immutable = true

		handlerFn := module.NewFunc("__promise_sigsegv_handler", irtypes.Void,
			ir.NewParam("sig", irtypes.I32))
		handlerFn.FuncAttrs = append(handlerFn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrNoReturn)
		{
			hEntry := handlerFn.NewBlock(".entry")
			msgPtr := hEntry.NewBitCast(msgGlobal, irtypes.I8Ptr)
			stderr := constant.NewInt(irtypes.I32, 2)
			hEntry.NewCall(writeFn, stderr, msgPtr, constant.NewInt(irtypes.I64, int64(len(msgStr))))
			hEntry.NewCall(exitFn, constant.NewInt(irtypes.I32, 2))
			hEntry.NewUnreachable()
		}

		// macOS: use sigaltstack + sigaction for reliable stack overflow detection.
		// Struct layouts are stable (single libc: Apple's libSystem).
		p.emitDarwinStackOverflowInit(module, entry, handlerFn)
	} else {
		// Linux: 3-arg SA_SIGINFO handler that reads si_addr and prints
		// "fatal: segmentation fault at 0x<hex>\n" with the fault address.
		handlerFn := p.emitLinuxSigsegvHandler(module, writeFn, exitFn)

		// Setup sigaction with SA_SIGINFO.
		p.emitLinuxStackOverflowInit(module, entry, handlerFn)
	}

	return fn
}

// emitLinuxSigsegvHandler defines @__promise_sigsegv_handler(i32, i8*, i8*)
// for Linux SA_SIGINFO. Reads si_addr from siginfo_t at offset 16 (stable
// kernel ABI on x86_64/aarch64), converts to hex, and writes:
//
//	"fatal: segmentation fault at 0x<16 hex digits>\n"
//
// Async-signal-safe: only uses write(2) and _exit(2).
func (p *PosixPAL) emitLinuxSigsegvHandler(
	module *ir.Module, writeFn, exitFn *ir.Func,
) *ir.Func {
	// Hex digit lookup table
	hexStr := "0123456789abcdef"
	hexConst := constant.NewCharArrayFromString(hexStr)
	hexGlobal := module.NewGlobal("__promise_hex_digits", hexConst.Typ)
	hexGlobal.Init = hexConst
	hexGlobal.Immutable = true

	// Message prefix: "fatal: segmentation fault at 0x" (31 bytes)
	// Full message: prefix + 16 hex digits + "\n" = 48 bytes
	prefixStr := "fatal: segmentation fault at 0x"
	prefixConst := constant.NewCharArrayFromString(prefixStr)
	prefixGlobal := module.NewGlobal("__promise_segfault_prefix", prefixConst.Typ)
	prefixGlobal.Init = prefixConst
	prefixGlobal.Immutable = true

	// Define @__promise_sigsegv_handler(i32 %sig, i8* %info, i8* %ucontext)
	handlerFn := module.NewFunc("__promise_sigsegv_handler", irtypes.Void,
		ir.NewParam("sig", irtypes.I32),
		ir.NewParam("info", irtypes.I8Ptr),
		ir.NewParam("ucontext", irtypes.I8Ptr))
	handlerFn.FuncAttrs = append(handlerFn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrNoReturn)

	hEntry := handlerFn.NewBlock(".entry")

	// Allocate 48-byte message buffer on stack
	const msgLen = 48
	bufTy := irtypes.NewArray(msgLen, irtypes.I8)
	buf := hEntry.NewAlloca(bufTy)
	bufI8 := hEntry.NewBitCast(buf, irtypes.I8Ptr)

	// Copy prefix into buffer
	memcpyFn := getOrDeclareFunc(module, "memcpy", irtypes.I8Ptr,
		ir.NewParam("dest", irtypes.I8Ptr),
		ir.NewParam("src", irtypes.I8Ptr),
		ir.NewParam("n", irtypes.I64))
	prefixI8 := hEntry.NewBitCast(prefixGlobal, irtypes.I8Ptr)
	hEntry.NewCall(memcpyFn, bufI8, prefixI8,
		constant.NewInt(irtypes.I64, int64(len(prefixStr))))

	// Load si_addr from siginfo_t at byte offset 16 (kernel ABI: stable on
	// both x86_64 and aarch64 — si_signo(4) + si_errno(4) + si_code(4) +
	// pad(4) then si_addr at offset 16).
	infoParam := handlerFn.Params[1]
	addrFieldPtr := hEntry.NewGetElementPtr(irtypes.I8, infoParam,
		constant.NewInt(irtypes.I64, 16))
	addrPtrCast := hEntry.NewBitCast(addrFieldPtr, irtypes.NewPointer(irtypes.I8Ptr))
	siAddr := hEntry.NewLoad(irtypes.I8Ptr, addrPtrCast)
	addrInt := hEntry.NewPtrToInt(siAddr, irtypes.I64)

	// Convert address to 16 hex digits at buf[31..46]
	hexI8 := hEntry.NewBitCast(hexGlobal, irtypes.I8Ptr)
	for i := 0; i < 16; i++ {
		shift := int64((15 - i) * 4)
		shifted := hEntry.NewLShr(addrInt, constant.NewInt(irtypes.I64, shift))
		nibble := hEntry.NewAnd(shifted, constant.NewInt(irtypes.I64, 0xF))
		digitPtr := hEntry.NewGetElementPtr(irtypes.I8, hexI8, nibble)
		digit := hEntry.NewLoad(irtypes.I8, digitPtr)
		bufPos := hEntry.NewGetElementPtr(irtypes.I8, bufI8,
			constant.NewInt(irtypes.I64, int64(len(prefixStr)+i)))
		hEntry.NewStore(digit, bufPos)
	}

	// Store newline at position 47
	nlPos := hEntry.NewGetElementPtr(irtypes.I8, bufI8,
		constant.NewInt(irtypes.I64, msgLen-1))
	hEntry.NewStore(constant.NewInt(irtypes.I8, 0x0A), nlPos)

	// write(2, buf, 48)
	stderr := constant.NewInt(irtypes.I32, 2)
	hEntry.NewCall(writeFn, stderr, bufI8, constant.NewInt(irtypes.I64, msgLen))
	hEntry.NewCall(exitFn, constant.NewInt(irtypes.I32, 2))
	hEntry.NewUnreachable()

	return handlerFn
}

// emitLinuxStackOverflowInit emits sigaction(SIGSEGV, SA_SIGINFO) setup for Linux.
// Uses byte-array struct construction to handle the sigaction struct layout
// difference between glibc and musl:
//
//	glibc: {handler(8), sa_mask(128), sa_flags(4), pad(4), sa_restorer(8)} = 152 bytes
//	musl:  {handler(8), sa_mask(16),  sa_flags(4), pad(4), sa_restorer(8)} = 40 bytes
//
// SA_SIGINFO = 4 on Linux (all architectures).
func (p *PosixPAL) emitLinuxStackOverflowInit(
	module *ir.Module, entry *ir.Block, handlerFn *ir.Func,
) {
	sigactionFn := getOrDeclareFunc(module, "sigaction", irtypes.I32,
		ir.NewParam("sig", irtypes.I32),
		ir.NewParam("act", irtypes.I8Ptr),
		ir.NewParam("oact", irtypes.I8Ptr))

	// Struct sizes and flag offsets differ between musl and glibc.
	isMusl := strings.Contains(p.target, "musl")
	var structSize int64
	var flagsOffset int64
	if isMusl {
		structSize = 40  // {handler(8), mask(16), flags(4), pad(4), restorer(8)}
		flagsOffset = 24 // after handler(8) + mask(16)
	} else {
		structSize = 152  // {handler(8), mask(128), flags(4), pad(4), restorer(8)}
		flagsOffset = 136 // after handler(8) + mask(128)
	}

	// Allocate and zero the sigaction struct on the stack.
	actArrTy := irtypes.NewArray(uint64(structSize), irtypes.I8)
	actAlloca := entry.NewAlloca(actArrTy)
	actI8 := entry.NewBitCast(actAlloca, irtypes.I8Ptr)

	memsetFn := getOrDeclareFunc(module, "memset", irtypes.I8Ptr,
		ir.NewParam("dest", irtypes.I8Ptr),
		ir.NewParam("c", irtypes.I32),
		ir.NewParam("n", irtypes.I64))
	entry.NewCall(memsetFn, actI8, constant.NewInt(irtypes.I32, 0),
		constant.NewInt(irtypes.I64, structSize))

	// Store handler at offset 0 (same layout in both glibc and musl).
	handlerPtrPtr := entry.NewBitCast(actI8, irtypes.NewPointer(irtypes.I8Ptr))
	handlerI8 := entry.NewBitCast(handlerFn, irtypes.I8Ptr)
	entry.NewStore(handlerI8, handlerPtrPtr)

	// Store SA_SIGINFO | SA_ONSTACK at the flags offset.
	// SA_SIGINFO = 0x04, SA_ONSTACK = 0x08000000 on Linux.
	const linuxSaSiginfo = 0x04
	const linuxSaOnstack = 0x08000000
	flagsGEP := entry.NewGetElementPtr(irtypes.I8, actI8,
		constant.NewInt(irtypes.I64, flagsOffset))
	flagsPtr := entry.NewBitCast(flagsGEP, irtypes.NewPointer(irtypes.I32))
	entry.NewStore(constant.NewInt(irtypes.I32, linuxSaSiginfo|linuxSaOnstack), flagsPtr)

	// sigaction(SIGSEGV=11, &act, NULL)
	entry.NewCall(sigactionFn, constant.NewInt(irtypes.I32, 11),
		actI8, constant.NewNull(irtypes.I8Ptr))

	entry.NewRet(nil)
}

// EmitStackOverflowThreadInit defines @pal_stack_overflow_thread_init() → void
// Sets up a per-thread sigaltstack so the SIGSEGV handler can run on an
// alternate stack when the thread's main stack is exhausted.
//
// macOS stack_t: {ss_sp, ss_size, ss_flags}
// Linux stack_t: {ss_sp, ss_flags, ss_size}
func (p *PosixPAL) EmitStackOverflowThreadInit(module *ir.Module) *ir.Func {
	palAlloc := lookupFunc(module, "pal_alloc")
	sigaltstackFn := getOrDeclareFunc(module, "sigaltstack", irtypes.I32,
		ir.NewParam("ss", irtypes.I8Ptr),
		ir.NewParam("oss", irtypes.I8Ptr))

	fn := module.NewFunc("pal_stack_overflow_thread_init", irtypes.Void)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	zero32 := constant.NewInt(irtypes.I32, 0)
	const altStackSize = 65536
	altStack := entry.NewCall(palAlloc, constant.NewInt(irtypes.I64, altStackSize))

	isDarwin := strings.Contains(p.target, "darwin") || strings.Contains(p.target, "macos")

	if isDarwin {
		// macOS stack_t: {i8* ss_sp, i64 ss_size, i32 ss_flags}
		stackTTy := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64, irtypes.I32)
		ssAlloca := entry.NewAlloca(stackTTy)
		ssSp := entry.NewGetElementPtr(stackTTy, ssAlloca, zero32, zero32)
		entry.NewStore(altStack, ssSp)
		ssSize := entry.NewGetElementPtr(stackTTy, ssAlloca, zero32, constant.NewInt(irtypes.I32, 1))
		entry.NewStore(constant.NewInt(irtypes.I64, altStackSize), ssSize)
		ssFlags := entry.NewGetElementPtr(stackTTy, ssAlloca, zero32, constant.NewInt(irtypes.I32, 2))
		entry.NewStore(zero32, ssFlags)
		ssPtr := entry.NewBitCast(ssAlloca, irtypes.I8Ptr)
		entry.NewCall(sigaltstackFn, ssPtr, constant.NewNull(irtypes.I8Ptr))
	} else {
		// Linux stack_t: {i8* ss_sp, i32 ss_flags, i64 ss_size}
		// LLVM naturally pads i32 to 8 bytes before i64, matching the C layout.
		stackTTy := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I32, irtypes.I64)
		ssAlloca := entry.NewAlloca(stackTTy)
		ssSp := entry.NewGetElementPtr(stackTTy, ssAlloca, zero32, zero32)
		entry.NewStore(altStack, ssSp)
		ssFlags := entry.NewGetElementPtr(stackTTy, ssAlloca, zero32, constant.NewInt(irtypes.I32, 1))
		entry.NewStore(zero32, ssFlags)
		ssSize := entry.NewGetElementPtr(stackTTy, ssAlloca, zero32, constant.NewInt(irtypes.I32, 2))
		entry.NewStore(constant.NewInt(irtypes.I64, altStackSize), ssSize)
		ssPtr := entry.NewBitCast(ssAlloca, irtypes.I8Ptr)
		entry.NewCall(sigaltstackFn, ssPtr, constant.NewNull(irtypes.I8Ptr))
	}

	entry.NewRet(nil)
	return fn
}

// emitDarwinStackOverflowInit emits sigaltstack + sigaction setup for macOS.
// macOS struct layouts:
//
//	stack_t:         {void* ss_sp, size_t ss_size, int ss_flags}  = 24 bytes
//	struct sigaction: {void(*)(int) sa_handler, uint32_t sa_mask, int sa_flags} = 16 bytes
//	SA_ONSTACK = 0x0001, SA_RESETHAND = 0x0004, SIGSEGV = 11, SIGBUS = 10
func (p *PosixPAL) emitDarwinStackOverflowInit(
	module *ir.Module, entry *ir.Block, handlerFn *ir.Func,
) {
	palAlloc := lookupFunc(module, "pal_alloc")

	// declare i32 @sigaltstack(i8*, i8*) nounwind
	sigaltstackFn := getOrDeclareFunc(module, "sigaltstack", irtypes.I32,
		ir.NewParam("ss", irtypes.I8Ptr),
		ir.NewParam("oss", irtypes.I8Ptr))

	// declare i32 @sigaction(i32, i8*, i8*) nounwind
	sigactionFn := getOrDeclareFunc(module, "sigaction", irtypes.I32,
		ir.NewParam("sig", irtypes.I32),
		ir.NewParam("act", irtypes.I8Ptr),
		ir.NewParam("oact", irtypes.I8Ptr))

	zero32 := constant.NewInt(irtypes.I32, 0)

	// Allocate 65536 bytes for the alternate signal stack via malloc
	const altStackSize = 65536
	altStack := entry.NewCall(palAlloc, constant.NewInt(irtypes.I64, altStackSize))

	// Build stack_t on the stack: {i8* ss_sp, i64 ss_size, i32 ss_flags}
	// macOS layout: ss_sp(0), ss_size(8), ss_flags(16) — total 24 bytes
	stackTTy := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64, irtypes.I32)
	ssAlloca := entry.NewAlloca(stackTTy)
	// ss_sp = altStack
	ssSp := entry.NewGetElementPtr(stackTTy, ssAlloca, zero32, zero32)
	entry.NewStore(altStack, ssSp)
	// ss_size = 65536
	ssSize := entry.NewGetElementPtr(stackTTy, ssAlloca, zero32, constant.NewInt(irtypes.I32, 1))
	entry.NewStore(constant.NewInt(irtypes.I64, altStackSize), ssSize)
	// ss_flags = 0
	ssFlags := entry.NewGetElementPtr(stackTTy, ssAlloca, zero32, constant.NewInt(irtypes.I32, 2))
	entry.NewStore(zero32, ssFlags)

	ssPtr := entry.NewBitCast(ssAlloca, irtypes.I8Ptr)
	entry.NewCall(sigaltstackFn, ssPtr, constant.NewNull(irtypes.I8Ptr))

	// Build struct sigaction: {i8* sa_handler, i32 sa_mask, i32 sa_flags}
	// macOS layout: sa_handler(0), sa_mask(8), sa_flags(12) — total 16 bytes
	sigactTy := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I32, irtypes.I32)
	actAlloca := entry.NewAlloca(sigactTy)
	// sa_handler = handlerFn
	handlerPtr := entry.NewBitCast(handlerFn, irtypes.I8Ptr)
	saHandler := entry.NewGetElementPtr(sigactTy, actAlloca, zero32, zero32)
	entry.NewStore(handlerPtr, saHandler)
	// sa_mask = 0 (no signals blocked during handler)
	saMask := entry.NewGetElementPtr(sigactTy, actAlloca, zero32, constant.NewInt(irtypes.I32, 1))
	entry.NewStore(zero32, saMask)
	// sa_flags = SA_ONSTACK(0x0001) | SA_RESETHAND(0x0004) = 0x0005
	saFlags := entry.NewGetElementPtr(sigactTy, actAlloca, zero32, constant.NewInt(irtypes.I32, 2))
	entry.NewStore(constant.NewInt(irtypes.I32, 0x0005), saFlags)

	actPtr := entry.NewBitCast(actAlloca, irtypes.I8Ptr)

	// sigaction(SIGSEGV=11, &act, NULL)
	entry.NewCall(sigactionFn, constant.NewInt(irtypes.I32, 11), actPtr, constant.NewNull(irtypes.I8Ptr))
	// sigaction(SIGBUS=10, &act, NULL)  — macOS uses SIGBUS for some stack overflows
	entry.NewCall(sigactionFn, constant.NewInt(irtypes.I32, 10), actPtr, constant.NewNull(irtypes.I8Ptr))

	entry.NewRet(nil)
}

// --- POSIX socket primitives (T0069) ---

// EmitSocketCreate declares libc @socket and defines @pal_socket_create.
// Signature: @pal_socket_create(i32 domain, i32 type, i32 protocol) → i32 (fd or -errno)
func (p *PosixPAL) EmitSocketCreate(module *ir.Module) *ir.Func {
	socketFn := getOrDeclareFunc(module, "socket", irtypes.I32,
		ir.NewParam("domain", irtypes.I32),
		ir.NewParam("type", irtypes.I32),
		ir.NewParam("protocol", irtypes.I32))

	fn := module.NewFunc("pal_socket_create", irtypes.I32,
		ir.NewParam("domain", irtypes.I32),
		ir.NewParam("typ", irtypes.I32),
		ir.NewParam("protocol", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(socketFn, fn.Params[0], fn.Params[1], fn.Params[2])

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitSocketBind declares libc @bind and defines @pal_socket_bind.
// Signature: @pal_socket_bind(i32 fd, i8* addr, i32 addrlen) → i32 (0 or -errno)
func (p *PosixPAL) EmitSocketBind(module *ir.Module) *ir.Func {
	bindFn := getOrDeclareFunc(module, "bind", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("addr", irtypes.I8Ptr),
		ir.NewParam("addrlen", irtypes.I32))

	fn := module.NewFunc("pal_socket_bind", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("addr", irtypes.I8Ptr),
		ir.NewParam("addrlen", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(bindFn, fn.Params[0], fn.Params[1], fn.Params[2])

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitSocketListen declares libc @listen and defines @pal_socket_listen.
// Signature: @pal_socket_listen(i32 fd, i32 backlog) → i32 (0 or -errno)
func (p *PosixPAL) EmitSocketListen(module *ir.Module) *ir.Func {
	listenFn := getOrDeclareFunc(module, "listen", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("backlog", irtypes.I32))

	fn := module.NewFunc("pal_socket_listen", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("backlog", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(listenFn, fn.Params[0], fn.Params[1])

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitSocketAccept declares libc @accept and defines @pal_socket_accept.
// Signature: @pal_socket_accept(i32 fd, i8* addr, i32* addrlen) → i32 (fd or -errno)
func (p *PosixPAL) EmitSocketAccept(module *ir.Module) *ir.Func {
	i32PtrType := irtypes.NewPointer(irtypes.I32)
	acceptFn := getOrDeclareFunc(module, "accept", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("addr", irtypes.I8Ptr),
		ir.NewParam("addrlen", i32PtrType))

	fn := module.NewFunc("pal_socket_accept", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("addr", irtypes.I8Ptr),
		ir.NewParam("addrlen", i32PtrType))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(acceptFn, fn.Params[0], fn.Params[1], fn.Params[2])

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitSocketConnect declares libc @connect and defines @pal_socket_connect.
// Signature: @pal_socket_connect(i32 fd, i8* addr, i32 addrlen) → i32 (0 or -errno)
func (p *PosixPAL) EmitSocketConnect(module *ir.Module) *ir.Func {
	connectFn := getOrDeclareFunc(module, "connect", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("addr", irtypes.I8Ptr),
		ir.NewParam("addrlen", irtypes.I32))

	fn := module.NewFunc("pal_socket_connect", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("addr", irtypes.I8Ptr),
		ir.NewParam("addrlen", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(connectFn, fn.Params[0], fn.Params[1], fn.Params[2])

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitSocketSend declares libc @send and defines @pal_socket_send.
// Signature: @pal_socket_send(i32 fd, i8* buf, i64 len, i32 flags) → i64 (bytes or -errno)
func (p *PosixPAL) EmitSocketSend(module *ir.Module) *ir.Func {
	sendFn := getOrDeclareFunc(module, "send", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64),
		ir.NewParam("flags", irtypes.I32))

	fn := module.NewFunc("pal_socket_send", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64),
		ir.NewParam("flags", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(sendFn, fn.Params[0], fn.Params[1], fn.Params[2], fn.Params[3])

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I64, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI64(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitSocketRecv declares libc @recv and defines @pal_socket_recv.
// Signature: @pal_socket_recv(i32 fd, i8* buf, i64 len, i32 flags) → i64 (bytes or -errno)
func (p *PosixPAL) EmitSocketRecv(module *ir.Module) *ir.Func {
	recvFn := getOrDeclareFunc(module, "recv", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64),
		ir.NewParam("flags", irtypes.I32))

	fn := module.NewFunc("pal_socket_recv", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64),
		ir.NewParam("flags", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(recvFn, fn.Params[0], fn.Params[1], fn.Params[2], fn.Params[3])

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I64, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI64(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitSocketClose declares libc @close (reuses existing declaration) and defines @pal_socket_close.
// Signature: @pal_socket_close(i32 fd) → i32 (0 or -errno)
func (p *PosixPAL) EmitSocketClose(module *ir.Module) *ir.Func {
	closeFn := getOrDeclareFunc(module, "close", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))

	fn := module.NewFunc("pal_socket_close", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(closeFn, fn.Params[0])

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitSocketSetOpt declares libc @setsockopt and defines @pal_socket_setopt.
// Signature: @pal_socket_setopt(i32 fd, i32 level, i32 opt, i8* val, i32 len) → i32 (0 or -errno)
func (p *PosixPAL) EmitSocketSetOpt(module *ir.Module) *ir.Func {
	setsockoptFn := getOrDeclareFunc(module, "setsockopt", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("level", irtypes.I32),
		ir.NewParam("optname", irtypes.I32),
		ir.NewParam("optval", irtypes.I8Ptr),
		ir.NewParam("optlen", irtypes.I32))

	fn := module.NewFunc("pal_socket_setopt", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("level", irtypes.I32),
		ir.NewParam("opt", irtypes.I32),
		ir.NewParam("val", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(setsockoptFn, fn.Params[0], fn.Params[1], fn.Params[2], fn.Params[3], fn.Params[4])

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitSocketShutdown declares libc @shutdown and defines @pal_socket_shutdown.
// Signature: @pal_socket_shutdown(i32 fd, i32 how) → i32 (0 or -errno)
func (p *PosixPAL) EmitSocketShutdown(module *ir.Module) *ir.Func {
	shutdownFn := getOrDeclareFunc(module, "shutdown", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("how", irtypes.I32))

	fn := module.NewFunc("pal_socket_shutdown", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("how", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(shutdownFn, fn.Params[0], fn.Params[1])

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitSocketSetNonBlock defines @pal_socket_set_nonblock using fcntl.
// Does fcntl(fd, F_GETFL) | O_NONBLOCK, then fcntl(fd, F_SETFL, flags).
// Signature: @pal_socket_set_nonblock(i32 fd) → i32 (0 or -errno)
func (p *PosixPAL) EmitSocketSetNonBlock(module *ir.Module) *ir.Func {
	// fcntl is variadic: int fcntl(int fd, int cmd, ...)
	fcntlFn := getOrDeclareFunc(module, "fcntl", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("cmd", irtypes.I32))
	fcntlFn.Sig.Variadic = true

	// Platform-specific constants
	var fGetFL, fSetFL, oNonBlock int64
	fGetFL = 3 // F_GETFL (same on macOS and Linux)
	fSetFL = 4 // F_SETFL (same on macOS and Linux)
	if p.isMacOS() {
		oNonBlock = 0x4 // O_NONBLOCK on macOS
	} else {
		oNonBlock = 0x800 // O_NONBLOCK on Linux
	}

	fn := module.NewFunc("pal_socket_set_nonblock", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// flags = fcntl(fd, F_GETFL)
	flags := entry.NewCall(fcntlFn, fn.Params[0], constant.NewInt(irtypes.I32, fGetFL))
	isErr1 := entry.NewICmp(enum.IPredSLT, flags, constant.NewInt(irtypes.I32, 0))
	getOkBlk := fn.NewBlock(".get_ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr1, errBlk, getOkBlk)

	// flags |= O_NONBLOCK
	newFlags := getOkBlk.NewOr(flags, constant.NewInt(irtypes.I32, oNonBlock))
	// fcntl(fd, F_SETFL, newFlags)
	ret := getOkBlk.NewCall(fcntlFn, fn.Params[0], constant.NewInt(irtypes.I32, fSetFL), newFlags)
	isErr2 := getOkBlk.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk2 := fn.NewBlock(".err2")
	getOkBlk.NewCondBr(isErr2, errBlk2, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	p.emitNegErrnoReturnI32(errBlk2, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(constant.NewInt(irtypes.I32, 0))
	return fn
}

// EmitSocketGetError defines @pal_socket_get_error using getsockopt(SO_ERROR).
// Signature: @pal_socket_get_error(i32 fd) → i32 (errno value, 0 = no error, or -errno on failure)
func (p *PosixPAL) EmitSocketGetError(module *ir.Module) *ir.Func {
	getsockoptFn := getOrDeclareFunc(module, "getsockopt", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("level", irtypes.I32),
		ir.NewParam("optname", irtypes.I32),
		ir.NewParam("optval", irtypes.I8Ptr),
		ir.NewParam("optlen", irtypes.NewPointer(irtypes.I32)))

	// Platform-specific constants
	var solSocket, soError int64
	if p.isMacOS() {
		solSocket = 0xFFFF // SOL_SOCKET on macOS
		soError = 0x1007   // SO_ERROR on macOS
	} else {
		solSocket = 1 // SOL_SOCKET on Linux
		soError = 4   // SO_ERROR on Linux
	}

	fn := module.NewFunc("pal_socket_get_error", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// Stack-allocate i32 for error value and i32 for optlen
	errVal := entry.NewAlloca(irtypes.I32)
	entry.NewStore(constant.NewInt(irtypes.I32, 0), errVal)
	optLen := entry.NewAlloca(irtypes.I32)
	entry.NewStore(constant.NewInt(irtypes.I32, 4), optLen) // sizeof(int)

	errValPtr := entry.NewBitCast(errVal, irtypes.I8Ptr)
	ret := entry.NewCall(getsockoptFn, fn.Params[0],
		constant.NewInt(irtypes.I32, solSocket),
		constant.NewInt(irtypes.I32, soError),
		errValPtr, optLen)

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	// Return the socket error value
	result := okBlk.NewLoad(irtypes.I32, errVal)
	okBlk.NewRet(result)
	return fn
}

// EmitGetAddrInfo declares libc @getaddrinfo and defines @pal_getaddrinfo.
// Signature: @pal_getaddrinfo(i8* host, i8* port, i8* hints, i8** result) → i32 (0 or EAI_* error)
func (p *PosixPAL) EmitGetAddrInfo(module *ir.Module) *ir.Func {
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)
	getaddrinfoFn := getOrDeclareFunc(module, "getaddrinfo", irtypes.I32,
		ir.NewParam("node", irtypes.I8Ptr),
		ir.NewParam("service", irtypes.I8Ptr),
		ir.NewParam("hints", irtypes.I8Ptr),
		ir.NewParam("res", i8PtrPtrType))

	fn := module.NewFunc("pal_getaddrinfo", irtypes.I32,
		ir.NewParam("host", irtypes.I8Ptr),
		ir.NewParam("port", irtypes.I8Ptr),
		ir.NewParam("hints", irtypes.I8Ptr),
		ir.NewParam("result", i8PtrPtrType))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(getaddrinfoFn, fn.Params[0], fn.Params[1], fn.Params[2], fn.Params[3])
	entry.NewRet(ret)
	return fn
}

// EmitFreeAddrInfo declares libc @freeaddrinfo and defines @pal_freeaddrinfo.
// Signature: @pal_freeaddrinfo(i8* result) → void
func (p *PosixPAL) EmitFreeAddrInfo(module *ir.Module) *ir.Func {
	freeaddrinfoFn := getOrDeclareFunc(module, "freeaddrinfo", irtypes.Void,
		ir.NewParam("res", irtypes.I8Ptr))

	fn := module.NewFunc("pal_freeaddrinfo", irtypes.Void,
		ir.NewParam("result", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewCall(freeaddrinfoFn, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// --- POSIX high-level socket address operations (T0071) ---
//
// These functions construct sockaddr_in internally via inet_pton, avoiding complex
// C struct construction in the codegen bridge. Host must be null-terminated IPv4 string.

// EmitSocketBindAddr defines @pal_socket_bind_addr(i32 fd, i8* host, i32 port) → i32.
// Parses host via inet_pton, constructs sockaddr_in, sets SO_REUSEADDR, calls bind.
func (p *PosixPAL) EmitSocketBindAddr(module *ir.Module) *ir.Func {
	inetPtonFn := getOrDeclareFunc(module, "inet_pton", irtypes.I32,
		ir.NewParam("af", irtypes.I32),
		ir.NewParam("src", irtypes.I8Ptr),
		ir.NewParam("dst", irtypes.I8Ptr))
	setsockoptFn := getOrDeclareFunc(module, "setsockopt", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("level", irtypes.I32),
		ir.NewParam("optname", irtypes.I32),
		ir.NewParam("optval", irtypes.I8Ptr),
		ir.NewParam("optlen", irtypes.I32))
	bindFn := getOrDeclareFunc(module, "bind", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("addr", irtypes.I8Ptr),
		ir.NewParam("addrlen", irtypes.I32))
	memsetFn := getOrDeclareFunc(module, "memset", irtypes.I8Ptr,
		ir.NewParam("s", irtypes.I8Ptr),
		ir.NewParam("c", irtypes.I32),
		ir.NewParam("n", irtypes.I64))

	var solSocket, soReuseAddr int64
	if p.isMacOS() {
		solSocket = 0xFFFF
		soReuseAddr = 0x0004
	} else {
		solSocket = 1
		soReuseAddr = 2
	}

	fn := module.NewFunc("pal_socket_bind_addr", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("host", irtypes.I8Ptr),
		ir.NewParam("port", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// Allocate sockaddr_in (16 bytes) on stack, zero-initialize
	saddrArr := irtypes.NewArray(16, irtypes.I8)
	saddr := entry.NewAlloca(saddrArr)
	saddrPtr := entry.NewBitCast(saddr, irtypes.I8Ptr)
	entry.NewCall(memsetFn, saddrPtr, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I64, 16))

	// Set sin_family = AF_INET (2)
	p.emitStoreSockaddrFamily(entry, saddrPtr)

	// Set sin_port = htons(port) at offset 2
	portI16 := entry.NewTrunc(fn.Params[2], irtypes.I16)
	netPort := emitHtons(entry, portI16)
	portPtr := entry.NewBitCast(
		entry.NewGetElementPtr(irtypes.I8, saddrPtr, constant.NewInt(irtypes.I64, 2)),
		irtypes.NewPointer(irtypes.I16))
	entry.NewStore(netPort, portPtr)

	// Set sin_addr via inet_pton(AF_INET, host, &saddr[4])
	addrDst := entry.NewGetElementPtr(irtypes.I8, saddrPtr, constant.NewInt(irtypes.I64, 4))
	inetRet := entry.NewCall(inetPtonFn, constant.NewInt(irtypes.I32, 2), fn.Params[1], addrDst)
	isOk := entry.NewICmp(enum.IPredEQ, inetRet, constant.NewInt(irtypes.I32, 1))
	bindBlk := fn.NewBlock(".bind")
	errInvalBlk := fn.NewBlock(".err_inval")
	entry.NewCondBr(isOk, bindBlk, errInvalBlk)

	// Invalid address → return -EINVAL (22 on both platforms)
	errInvalBlk.NewRet(constant.NewInt(irtypes.I32, -22))

	// Set SO_REUSEADDR
	oneAlloca := bindBlk.NewAlloca(irtypes.I32)
	bindBlk.NewStore(constant.NewInt(irtypes.I32, 1), oneAlloca)
	onePtr := bindBlk.NewBitCast(oneAlloca, irtypes.I8Ptr)
	bindBlk.NewCall(setsockoptFn, fn.Params[0],
		constant.NewInt(irtypes.I32, solSocket),
		constant.NewInt(irtypes.I32, soReuseAddr),
		onePtr, constant.NewInt(irtypes.I32, 4))

	// bind(fd, &sockaddr, 16)
	bindRet := bindBlk.NewCall(bindFn, fn.Params[0], saddrPtr, constant.NewInt(irtypes.I32, 16))
	isErr := bindBlk.NewICmp(enum.IPredSLT, bindRet, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	bindBlk.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(constant.NewInt(irtypes.I32, 0))
	return fn
}

// EmitSocketConnectAddr defines @pal_socket_connect_addr(i32 fd, i8* host, i32 port) → i32.
// Parses host via inet_pton, constructs sockaddr_in, calls connect.
// Returns 0 on success, -EINPROGRESS for non-blocking sockets, or -errno on error.
func (p *PosixPAL) EmitSocketConnectAddr(module *ir.Module) *ir.Func {
	inetPtonFn := getOrDeclareFunc(module, "inet_pton", irtypes.I32,
		ir.NewParam("af", irtypes.I32),
		ir.NewParam("src", irtypes.I8Ptr),
		ir.NewParam("dst", irtypes.I8Ptr))
	connectFn := getOrDeclareFunc(module, "connect", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("addr", irtypes.I8Ptr),
		ir.NewParam("addrlen", irtypes.I32))
	memsetFn := getOrDeclareFunc(module, "memset", irtypes.I8Ptr,
		ir.NewParam("s", irtypes.I8Ptr),
		ir.NewParam("c", irtypes.I32),
		ir.NewParam("n", irtypes.I64))

	fn := module.NewFunc("pal_socket_connect_addr", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("host", irtypes.I8Ptr),
		ir.NewParam("port", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// Allocate and zero sockaddr_in (16 bytes)
	saddrArr := irtypes.NewArray(16, irtypes.I8)
	saddr := entry.NewAlloca(saddrArr)
	saddrPtr := entry.NewBitCast(saddr, irtypes.I8Ptr)
	entry.NewCall(memsetFn, saddrPtr, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I64, 16))

	// sin_family = AF_INET
	p.emitStoreSockaddrFamily(entry, saddrPtr)

	// sin_port = htons(port) at offset 2
	portI16 := entry.NewTrunc(fn.Params[2], irtypes.I16)
	netPort := emitHtons(entry, portI16)
	portPtr := entry.NewBitCast(
		entry.NewGetElementPtr(irtypes.I8, saddrPtr, constant.NewInt(irtypes.I64, 2)),
		irtypes.NewPointer(irtypes.I16))
	entry.NewStore(netPort, portPtr)

	// inet_pton(AF_INET, host, &saddr[4])
	addrDst := entry.NewGetElementPtr(irtypes.I8, saddrPtr, constant.NewInt(irtypes.I64, 4))
	inetRet := entry.NewCall(inetPtonFn, constant.NewInt(irtypes.I32, 2), fn.Params[1], addrDst)
	isOk := entry.NewICmp(enum.IPredEQ, inetRet, constant.NewInt(irtypes.I32, 1))
	connBlk := fn.NewBlock(".connect")
	errInvalBlk := fn.NewBlock(".err_inval")
	entry.NewCondBr(isOk, connBlk, errInvalBlk)

	errInvalBlk.NewRet(constant.NewInt(irtypes.I32, -22)) // -EINVAL

	// connect(fd, &sockaddr, 16)
	connRet := connBlk.NewCall(connectFn, fn.Params[0], saddrPtr, constant.NewInt(irtypes.I32, 16))
	isErr := connBlk.NewICmp(enum.IPredSLT, connRet, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	connBlk.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(constant.NewInt(irtypes.I32, 0))
	return fn
}

// EmitSocketAcceptAddr defines @pal_socket_accept_addr(i32 listen_fd) → i32.
// Calls accept(fd, NULL, NULL) — no address extraction needed.
func (p *PosixPAL) EmitSocketAcceptAddr(module *ir.Module) *ir.Func {
	acceptFn := getOrDeclareFunc(module, "accept", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("addr", irtypes.I8Ptr),
		ir.NewParam("addrlen", irtypes.NewPointer(irtypes.I32)))

	fn := module.NewFunc("pal_socket_accept_addr", irtypes.I32,
		ir.NewParam("listen_fd", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	ret := entry.NewCall(acceptFn, fn.Params[0],
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.NewPointer(irtypes.I32)))

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitSocketGetLocalPort defines @pal_socket_get_local_port(i32 fd) → i32.
// Calls getsockname, extracts sin_port from sockaddr_in at offset 2, ntohs, returns as i32.
// Returns port in host byte order on success, or -errno on failure.
func (p *PosixPAL) EmitSocketGetLocalPort(module *ir.Module) *ir.Func {
	getsocknameFn := getOrDeclareFunc(module, "getsockname", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("addr", irtypes.I8Ptr),
		ir.NewParam("addrlen", irtypes.NewPointer(irtypes.I32)))
	memsetFn := getOrDeclareFunc(module, "memset", irtypes.I8Ptr,
		ir.NewParam("s", irtypes.I8Ptr),
		ir.NewParam("c", irtypes.I32),
		ir.NewParam("n", irtypes.I64))

	fn := module.NewFunc("pal_socket_get_local_port", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	// Allocate sockaddr_in (16 bytes) on stack, zero-initialize
	saddrArr := irtypes.NewArray(16, irtypes.I8)
	saddr := entry.NewAlloca(saddrArr)
	saddrPtr := entry.NewBitCast(saddr, irtypes.I8Ptr)
	entry.NewCall(memsetFn, saddrPtr, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I64, 16))

	// Allocate addrlen = 16
	addrLen := entry.NewAlloca(irtypes.I32)
	entry.NewStore(constant.NewInt(irtypes.I32, 16), addrLen)

	// getsockname(fd, &sockaddr, &addrlen)
	ret := entry.NewCall(getsocknameFn, fn.Params[0], saddrPtr, addrLen)
	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))

	// Extract sin_port at offset 2 (i16 in network byte order)
	portPtr := okBlk.NewBitCast(
		okBlk.NewGetElementPtr(irtypes.I8, saddrPtr, constant.NewInt(irtypes.I64, 2)),
		irtypes.NewPointer(irtypes.I16))
	netPort := okBlk.NewLoad(irtypes.I16, portPtr)
	// ntohs (same as htons — byte swap)
	hostPort := emitHtons(okBlk, netPort)
	// Zero-extend i16 → i32
	result := okBlk.NewZExt(hostPort, irtypes.I32)
	okBlk.NewRet(result)
	return fn
}

// emitStoreSockaddrFamily stores AF_INET (2) into the sin_family field of a sockaddr_in.
// macOS: sin_len=16 at offset 0 (i8), sin_family at offset 1 (i8).
// Linux: sin_family at offset 0 (i16).
func (p *PosixPAL) emitStoreSockaddrFamily(block *ir.Block, saddrPtr value.Value) {
	if p.isMacOS() {
		// macOS BSD-style: sin_len (i8) at offset 0, sin_family (i8) at offset 1
		block.NewStore(constant.NewInt(irtypes.I8, 16), saddrPtr) // sin_len = sizeof(sockaddr_in)
		famPtr := block.NewGetElementPtr(irtypes.I8, saddrPtr, constant.NewInt(irtypes.I64, 1))
		block.NewStore(constant.NewInt(irtypes.I8, 2), famPtr) // AF_INET
	} else {
		// Linux: sin_family (i16) at offset 0
		famPtr := block.NewBitCast(saddrPtr, irtypes.NewPointer(irtypes.I16))
		block.NewStore(constant.NewInt(irtypes.I16, 2), famPtr) // AF_INET
	}
}

// emitHtons emits byte-swap for 16-bit port (little-endian → network byte order).
func emitHtons(block *ir.Block, port value.Value) value.Value {
	hi := block.NewLShr(port, constant.NewInt(irtypes.I16, 8))
	lo := block.NewAnd(port, constant.NewInt(irtypes.I16, 0xFF))
	loShifted := block.NewShl(lo, constant.NewInt(irtypes.I16, 8))
	return block.NewOr(hi, loShifted)
}

// --- POSIX IO reactor primitives (T0070) ---
//
// PollEvent output struct layout (16 bytes, filled by pal_reactor_poll):
//   { i8* userdata, i32 events, i32 _pad }
// events: 1=readable, 2=writable, 4=error/hangup
//
// macOS uses kqueue; Linux uses epoll.

// epollEventStructType returns the LLVM struct for Linux's struct epoll_event.
// C: struct epoll_event { u32 events; epoll_data_t data; } __attribute__((packed))
// Packed layout: <{ i32, i64 }> = 12 bytes. data holds a pointer via ptrtoint/inttoptr.
func epollEventStructType() *irtypes.StructType {
	ty := irtypes.NewStruct(irtypes.I32, irtypes.I64)
	ty.Packed = true
	return ty
}

// EmitReactorCreate defines @pal_reactor_create() → i32 (fd or -errno).
// macOS: kqueue(). Linux: epoll_create1(0).
func (p *PosixPAL) EmitReactorCreate(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_reactor_create", irtypes.I32)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	var ret *ir.InstCall
	if p.isMacOS() {
		kqueueFn := getOrDeclareFunc(module, "kqueue", irtypes.I32)
		ret = entry.NewCall(kqueueFn)
	} else {
		epollCreate1Fn := getOrDeclareFunc(module, "epoll_create1", irtypes.I32,
			ir.NewParam("flags", irtypes.I32))
		ret = entry.NewCall(epollCreate1Fn, constant.NewInt(irtypes.I32, 0))
	}

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}

// EmitReactorAdd defines @pal_reactor_add(i32 rfd, i32 fd, i8* userdata) → i32 (0 or -errno).
// Registers fd for level-triggered read+write monitoring (B0324).
// macOS: kevent() with EVFILT_READ + EVFILT_WRITE, EV_ADD.
// Linux: epoll_ctl(EPOLL_CTL_ADD, EPOLLIN|EPOLLOUT).
func (p *PosixPAL) EmitReactorAdd(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_reactor_add", irtypes.I32,
		ir.NewParam("rfd", irtypes.I32),
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("userdata", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	if p.isMacOS() {
		p.emitKqueueAdd(module, fn, entry)
	} else {
		p.emitEpollAdd(module, fn, entry)
	}

	return fn
}

// emitKqueueAdd emits the body for pal_reactor_add on macOS using kevent().
// Registers two filters: EVFILT_READ (-1) and EVFILT_WRITE (-2) with EV_ADD.
// Level-triggered (no EV_CLEAR) ensures events re-fire until consumed (B0324).
func (p *PosixPAL) emitKqueueAdd(module *ir.Module, fn *ir.Func, entry *ir.Block) {
	// struct kevent { uintptr_t ident; i16 filter; u16 flags; u32 fflags; intptr_t data; void* udata; }
	// On macOS arm64: {i64, i16, i16, i32, i64, i8*} = 32 bytes
	keventType := irtypes.NewStruct(
		irtypes.I64,   // ident (fd as uintptr)
		irtypes.I16,   // filter
		irtypes.I16,   // flags
		irtypes.I32,   // fflags
		irtypes.I64,   // data
		irtypes.I8Ptr, // udata
	)

	// declare i32 @kevent(i32 kq, struct kevent* changelist, i32 nchanges,
	//                      struct kevent* eventlist, i32 nevents,
	//                      i8* timeout) — timeout as i8* (struct timespec* or null)
	keventFn := getOrDeclareFunc(module, "kevent", irtypes.I32,
		ir.NewParam("kq", irtypes.I32),
		ir.NewParam("changelist", irtypes.NewPointer(keventType)),
		ir.NewParam("nchanges", irtypes.I32),
		ir.NewParam("eventlist", irtypes.NewPointer(keventType)),
		ir.NewParam("nevents", irtypes.I32),
		ir.NewParam("timeout", irtypes.I8Ptr))

	// Stack-allocate 2 kevent structs for changelist
	changes := entry.NewAlloca(irtypes.NewArray(2, keventType))

	// Zero-extend fd from i32 to i64 for ident field
	fdI64 := entry.NewZExt(fn.Params[1], irtypes.I64)

	// EV_ADD=1 — level-triggered (no EV_CLEAR) to prevent event loss (B0324)
	evFlags := constant.NewInt(irtypes.I16, 0x0001)

	// Event 0: EVFILT_READ = -1 (0xFFFF as i16)
	ev0 := entry.NewGetElementPtr(irtypes.NewArray(2, keventType), changes,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	ev0Ident := entry.NewGetElementPtr(keventType, ev0, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	entry.NewStore(fdI64, ev0Ident)
	ev0Filter := entry.NewGetElementPtr(keventType, ev0, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	entry.NewStore(constant.NewInt(irtypes.I16, -1), ev0Filter) // EVFILT_READ = -1
	ev0Flags := entry.NewGetElementPtr(keventType, ev0, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	entry.NewStore(evFlags, ev0Flags)
	ev0Fflags := entry.NewGetElementPtr(keventType, ev0, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 3))
	entry.NewStore(constant.NewInt(irtypes.I32, 0), ev0Fflags)
	ev0Data := entry.NewGetElementPtr(keventType, ev0, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 4))
	entry.NewStore(constant.NewInt(irtypes.I64, 0), ev0Data)
	ev0Udata := entry.NewGetElementPtr(keventType, ev0, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 5))
	entry.NewStore(fn.Params[2], ev0Udata) // userdata

	// Event 1: EVFILT_WRITE = -2 (0xFFFE as i16)
	ev1 := entry.NewGetElementPtr(irtypes.NewArray(2, keventType), changes,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	ev1Ident := entry.NewGetElementPtr(keventType, ev1, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	entry.NewStore(fdI64, ev1Ident)
	ev1Filter := entry.NewGetElementPtr(keventType, ev1, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	entry.NewStore(constant.NewInt(irtypes.I16, -2), ev1Filter) // EVFILT_WRITE = -2
	ev1Flags := entry.NewGetElementPtr(keventType, ev1, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	entry.NewStore(evFlags, ev1Flags)
	ev1Fflags := entry.NewGetElementPtr(keventType, ev1, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 3))
	entry.NewStore(constant.NewInt(irtypes.I32, 0), ev1Fflags)
	ev1Data := entry.NewGetElementPtr(keventType, ev1, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 4))
	entry.NewStore(constant.NewInt(irtypes.I64, 0), ev1Data)
	ev1Udata := entry.NewGetElementPtr(keventType, ev1, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 5))
	entry.NewStore(fn.Params[2], ev1Udata) // userdata

	// kevent(rfd, changes, 2, null, 0, null) — submit changes, no wait
	ret := entry.NewCall(keventFn, fn.Params[0], ev0,
		constant.NewInt(irtypes.I32, 2),
		constant.NewNull(irtypes.NewPointer(keventType)),
		constant.NewInt(irtypes.I32, 0),
		constant.NewNull(irtypes.I8Ptr))

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(constant.NewInt(irtypes.I32, 0))
}

// emitEpollAdd emits the body for pal_reactor_add on Linux using epoll_ctl().
func (p *PosixPAL) emitEpollAdd(module *ir.Module, fn *ir.Func, entry *ir.Block) {
	// struct epoll_event { u32 events; epoll_data_t data; } __attribute__((packed))
	// epoll_data_t is a union (8 bytes) — we use the ptr field via ptrtoint/inttoptr.
	// Packed layout: {i32 events, i64 data} = 12 bytes (no padding between fields).
	epollEventType := epollEventStructType()

	epollCtlFn := getOrDeclareFunc(module, "epoll_ctl", irtypes.I32,
		ir.NewParam("epfd", irtypes.I32),
		ir.NewParam("op", irtypes.I32),
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("event", irtypes.NewPointer(epollEventType)))

	// Stack-allocate epoll_event
	ev := entry.NewAlloca(epollEventType)

	// EPOLLIN=1, EPOLLOUT=4 — level-triggered (no EPOLLET) to prevent event loss (B0324)
	evEvents := entry.NewGetElementPtr(epollEventType, ev,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	entry.NewStore(constant.NewInt(irtypes.I32, 0x00000005), evEvents) // EPOLLIN|EPOLLOUT

	// data = ptrtoint(userdata)
	evData := entry.NewGetElementPtr(epollEventType, ev,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	dataI64 := entry.NewPtrToInt(fn.Params[2], irtypes.I64)
	entry.NewStore(dataI64, evData)

	// EPOLL_CTL_ADD = 1
	ret := entry.NewCall(epollCtlFn, fn.Params[0],
		constant.NewInt(irtypes.I32, 1), // EPOLL_CTL_ADD
		fn.Params[1], ev)

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(constant.NewInt(irtypes.I32, 0))
}

// EmitReactorRemove defines @pal_reactor_remove(i32 rfd, i32 fd) → i32 (0 or -errno).
// macOS: kevent() with EV_DELETE for both filters. Linux: epoll_ctl(EPOLL_CTL_DEL).
func (p *PosixPAL) EmitReactorRemove(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_reactor_remove", irtypes.I32,
		ir.NewParam("rfd", irtypes.I32),
		ir.NewParam("fd", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	if p.isMacOS() {
		p.emitKqueueRemove(module, fn, entry)
	} else {
		p.emitEpollRemove(module, fn, entry)
	}

	return fn
}

func (p *PosixPAL) emitKqueueRemove(module *ir.Module, fn *ir.Func, entry *ir.Block) {
	keventType := irtypes.NewStruct(irtypes.I64, irtypes.I16, irtypes.I16, irtypes.I32, irtypes.I64, irtypes.I8Ptr)
	keventFn := getOrDeclareFunc(module, "kevent", irtypes.I32,
		ir.NewParam("kq", irtypes.I32),
		ir.NewParam("changelist", irtypes.NewPointer(keventType)),
		ir.NewParam("nchanges", irtypes.I32),
		ir.NewParam("eventlist", irtypes.NewPointer(keventType)),
		ir.NewParam("nevents", irtypes.I32),
		ir.NewParam("timeout", irtypes.I8Ptr))

	changes := entry.NewAlloca(irtypes.NewArray(2, keventType))
	fdI64 := entry.NewZExt(fn.Params[1], irtypes.I64)

	// EV_DELETE = 2
	evFlags := constant.NewInt(irtypes.I16, 0x0002)

	// Delete EVFILT_READ
	ev0 := entry.NewGetElementPtr(irtypes.NewArray(2, keventType), changes,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	ev0Ident := entry.NewGetElementPtr(keventType, ev0, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	entry.NewStore(fdI64, ev0Ident)
	ev0Filter := entry.NewGetElementPtr(keventType, ev0, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	entry.NewStore(constant.NewInt(irtypes.I16, -1), ev0Filter)
	ev0Flags := entry.NewGetElementPtr(keventType, ev0, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	entry.NewStore(evFlags, ev0Flags)
	ev0Fflags := entry.NewGetElementPtr(keventType, ev0, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 3))
	entry.NewStore(constant.NewInt(irtypes.I32, 0), ev0Fflags)
	ev0Data := entry.NewGetElementPtr(keventType, ev0, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 4))
	entry.NewStore(constant.NewInt(irtypes.I64, 0), ev0Data)
	ev0Udata := entry.NewGetElementPtr(keventType, ev0, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 5))
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), ev0Udata)

	// Delete EVFILT_WRITE
	ev1 := entry.NewGetElementPtr(irtypes.NewArray(2, keventType), changes,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	ev1Ident := entry.NewGetElementPtr(keventType, ev1, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	entry.NewStore(fdI64, ev1Ident)
	ev1Filter := entry.NewGetElementPtr(keventType, ev1, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	entry.NewStore(constant.NewInt(irtypes.I16, -2), ev1Filter)
	ev1Flags := entry.NewGetElementPtr(keventType, ev1, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	entry.NewStore(evFlags, ev1Flags)
	ev1Fflags := entry.NewGetElementPtr(keventType, ev1, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 3))
	entry.NewStore(constant.NewInt(irtypes.I32, 0), ev1Fflags)
	ev1Data := entry.NewGetElementPtr(keventType, ev1, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 4))
	entry.NewStore(constant.NewInt(irtypes.I64, 0), ev1Data)
	ev1Udata := entry.NewGetElementPtr(keventType, ev1, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 5))
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), ev1Udata)

	ret := entry.NewCall(keventFn, fn.Params[0], ev0,
		constant.NewInt(irtypes.I32, 2),
		constant.NewNull(irtypes.NewPointer(keventType)),
		constant.NewInt(irtypes.I32, 0),
		constant.NewNull(irtypes.I8Ptr))

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(constant.NewInt(irtypes.I32, 0))
}

func (p *PosixPAL) emitEpollRemove(module *ir.Module, fn *ir.Func, entry *ir.Block) {
	epollEventType := epollEventStructType()
	epollCtlFn := getOrDeclareFunc(module, "epoll_ctl", irtypes.I32,
		ir.NewParam("epfd", irtypes.I32),
		ir.NewParam("op", irtypes.I32),
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("event", irtypes.NewPointer(epollEventType)))

	// EPOLL_CTL_DEL = 2. Event pointer can be null for DEL on modern kernels,
	// but older kernels require non-null — pass a dummy.
	dummy := entry.NewAlloca(epollEventType)
	ret := entry.NewCall(epollCtlFn, fn.Params[0],
		constant.NewInt(irtypes.I32, 2), // EPOLL_CTL_DEL
		fn.Params[1], dummy)

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(constant.NewInt(irtypes.I32, 0))
}

// EmitReactorPoll defines @pal_reactor_poll(i32 rfd, i8* events_buf, i32 max_events, i32 timeout_ms) → i32.
// Fills events_buf with PollEvent structs {i8* userdata, i32 events, i32 _pad}.
// Returns number of events, or -errno on error.
func (p *PosixPAL) EmitReactorPoll(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_reactor_poll", irtypes.I32,
		ir.NewParam("rfd", irtypes.I32),
		ir.NewParam("events_buf", irtypes.I8Ptr),
		ir.NewParam("max_events", irtypes.I32),
		ir.NewParam("timeout_ms", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")

	if p.isMacOS() {
		p.emitKqueuePoll(module, fn, entry)
	} else {
		p.emitEpollPoll(module, fn, entry)
	}

	return fn
}

// PollEvent output struct: {i8* userdata, i32 events, i32 _pad} = 16 bytes
func pollEventType() *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I8Ptr, irtypes.I32, irtypes.I32)
}

func (p *PosixPAL) emitKqueuePoll(module *ir.Module, fn *ir.Func, entry *ir.Block) {
	keventType := irtypes.NewStruct(irtypes.I64, irtypes.I16, irtypes.I16, irtypes.I32, irtypes.I64, irtypes.I8Ptr)
	keventFn := getOrDeclareFunc(module, "kevent", irtypes.I32,
		ir.NewParam("kq", irtypes.I32),
		ir.NewParam("changelist", irtypes.NewPointer(keventType)),
		ir.NewParam("nchanges", irtypes.I32),
		ir.NewParam("eventlist", irtypes.NewPointer(keventType)),
		ir.NewParam("nevents", irtypes.I32),
		ir.NewParam("timeout", irtypes.I8Ptr))

	// struct timespec { i64 tv_sec; i64 tv_nsec; }
	timespecType := irtypes.NewStruct(irtypes.I64, irtypes.I64)

	// Stack-allocate kevent array: max_events * sizeof(kevent).
	// Uses alloca instead of palAlloc so the buffer is not tracked by the
	// alloc counter — avoids false leak reports when the reactor thread's
	// poll overlaps a test's leak-detection window (B0326).
	keventSize := constant.NewInt(irtypes.I64, 32) // sizeof(kevent) = 32
	maxI64 := entry.NewZExt(fn.Params[2], irtypes.I64)
	bufSize := entry.NewMul(maxI64, keventSize)
	rawBuf := entry.NewAlloca(irtypes.I8)
	rawBuf.NElems = bufSize
	keventBuf := entry.NewBitCast(rawBuf, irtypes.NewPointer(keventType))

	// Build timespec from timeout_ms: sec = ms/1000, nsec = (ms%1000)*1000000
	ts := entry.NewAlloca(timespecType)
	msI64 := entry.NewSExt(fn.Params[3], irtypes.I64)
	sec := entry.NewSDiv(msI64, constant.NewInt(irtypes.I64, 1000))
	rem := entry.NewSRem(msI64, constant.NewInt(irtypes.I64, 1000))
	nsec := entry.NewMul(rem, constant.NewInt(irtypes.I64, 1000000))
	tsSec := entry.NewGetElementPtr(timespecType, ts, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	entry.NewStore(sec, tsSec)
	tsNsec := entry.NewGetElementPtr(timespecType, ts, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	entry.NewStore(nsec, tsNsec)
	tsPtr := entry.NewBitCast(ts, irtypes.I8Ptr)

	// kevent(rfd, null, 0, keventBuf, max_events, &ts)
	count := entry.NewCall(keventFn, fn.Params[0],
		constant.NewNull(irtypes.NewPointer(keventType)),
		constant.NewInt(irtypes.I32, 0),
		keventBuf, fn.Params[2], tsPtr)

	isErr := entry.NewICmp(enum.IPredSLT, count, constant.NewInt(irtypes.I32, 0))
	errBlk := fn.NewBlock(".err")
	convertBlk := fn.NewBlock(".convert")
	entry.NewCondBr(isErr, errBlk, convertBlk)

	// Error path: return -errno (kevent buffer is stack-allocated, no free needed)
	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))

	// Convert kevent results to PollEvent format
	peType := pollEventType()
	outBuf := convertBlk.NewBitCast(fn.Params[1], irtypes.NewPointer(peType))

	// Loop: for i = 0; i < count; i++
	loopCond := fn.NewBlock(".loop_cond")
	loopBody := fn.NewBlock(".loop_body")
	loopEnd := fn.NewBlock(".loop_end")
	convertBlk.NewBr(loopCond)

	// PHI for loop counter
	i := loopCond.NewPhi(ir.NewIncoming(constant.NewInt(irtypes.I32, 0), convertBlk))
	cmp := loopCond.NewICmp(enum.IPredSLT, i, count)
	loopCond.NewCondBr(cmp, loopBody, loopEnd)

	// Read kevent[i]
	kev := loopBody.NewGetElementPtr(keventType, keventBuf, i)
	// Read filter (field 1) to determine read vs write
	filterPtr := loopBody.NewGetElementPtr(keventType, kev, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	filter := loopBody.NewLoad(irtypes.I16, filterPtr)
	// Read flags (field 2) to check EV_EOF/EV_ERROR
	flagsPtr := loopBody.NewGetElementPtr(keventType, kev, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	flags := loopBody.NewLoad(irtypes.I16, flagsPtr)
	// Read udata (field 5)
	udataPtr := loopBody.NewGetElementPtr(keventType, kev, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 5))
	udata := loopBody.NewLoad(irtypes.I8Ptr, udataPtr)

	// Map filter to events: EVFILT_READ(-1) → 1, EVFILT_WRITE(-2) → 2
	isRead := loopBody.NewICmp(enum.IPredEQ, filter, constant.NewInt(irtypes.I16, -1))
	events := loopBody.NewSelect(isRead, constant.NewInt(irtypes.I32, 1), constant.NewInt(irtypes.I32, 2))
	// EV_EOF = 0x8000 → add error flag (4)
	flags32 := loopBody.NewZExt(flags, irtypes.I32)
	hasEof := loopBody.NewAnd(flags32, constant.NewInt(irtypes.I32, 0x8000))
	isEof := loopBody.NewICmp(enum.IPredNE, hasEof, constant.NewInt(irtypes.I32, 0))
	errFlag := loopBody.NewSelect(isEof, constant.NewInt(irtypes.I32, 4), constant.NewInt(irtypes.I32, 0))
	eventsWithErr := loopBody.NewOr(events, errFlag)

	// Write PollEvent[i]
	pe := loopBody.NewGetElementPtr(peType, outBuf, i)
	peUserdata := loopBody.NewGetElementPtr(peType, pe, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	loopBody.NewStore(udata, peUserdata)
	peEvents := loopBody.NewGetElementPtr(peType, pe, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	loopBody.NewStore(eventsWithErr, peEvents)
	pePad := loopBody.NewGetElementPtr(peType, pe, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	loopBody.NewStore(constant.NewInt(irtypes.I32, 0), pePad)

	// i++
	iNext := loopBody.NewAdd(i, constant.NewInt(irtypes.I32, 1))
	i.Incs = append(i.Incs, ir.NewIncoming(iNext, loopBody))
	loopBody.NewBr(loopCond)

	// Return count (kevent buffer is stack-allocated, no free needed)
	loopEnd.NewRet(count)
}

func (p *PosixPAL) emitEpollPoll(module *ir.Module, fn *ir.Func, entry *ir.Block) {
	epollEventType := epollEventStructType()
	epollWaitFn := getOrDeclareFunc(module, "epoll_wait", irtypes.I32,
		ir.NewParam("epfd", irtypes.I32),
		ir.NewParam("events", irtypes.NewPointer(epollEventType)),
		ir.NewParam("maxevents", irtypes.I32),
		ir.NewParam("timeout", irtypes.I32))

	// Stack-allocate epoll_event array: max_events * 12 bytes (packed struct).
	// Uses alloca instead of palAlloc — same rationale as kqueue path (B0326).
	evSize := constant.NewInt(irtypes.I64, 12) // sizeof(struct epoll_event) = 12 (packed)
	maxI64 := entry.NewZExt(fn.Params[2], irtypes.I64)
	bufSize := entry.NewMul(maxI64, evSize)
	rawBuf := entry.NewAlloca(irtypes.I8)
	rawBuf.NElems = bufSize
	epollBuf := entry.NewBitCast(rawBuf, irtypes.NewPointer(epollEventType))

	// epoll_wait(rfd, epollBuf, max_events, timeout_ms)
	count := entry.NewCall(epollWaitFn, fn.Params[0], epollBuf, fn.Params[2], fn.Params[3])

	isErr := entry.NewICmp(enum.IPredSLT, count, constant.NewInt(irtypes.I32, 0))
	errBlk := fn.NewBlock(".err")
	convertBlk := fn.NewBlock(".convert")
	entry.NewCondBr(isErr, errBlk, convertBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))

	// Convert epoll_event to PollEvent
	peType := pollEventType()
	outBuf := convertBlk.NewBitCast(fn.Params[1], irtypes.NewPointer(peType))

	loopCond := fn.NewBlock(".loop_cond")
	loopBody := fn.NewBlock(".loop_body")
	loopEnd := fn.NewBlock(".loop_end")
	convertBlk.NewBr(loopCond)

	i := loopCond.NewPhi(ir.NewIncoming(constant.NewInt(irtypes.I32, 0), convertBlk))
	cmp := loopCond.NewICmp(enum.IPredSLT, i, count)
	loopCond.NewCondBr(cmp, loopBody, loopEnd)

	// Read epoll_event[i] — packed struct <{ i32 events, i64 data }>
	eev := loopBody.NewGetElementPtr(epollEventType, epollBuf, i)
	eevEventsPtr := loopBody.NewGetElementPtr(epollEventType, eev, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	eevEvents := loopBody.NewLoad(irtypes.I32, eevEventsPtr)
	// data is field 1 (i64), convert back to pointer via inttoptr
	eevDataPtr := loopBody.NewGetElementPtr(epollEventType, eev, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	eevDataI64 := loopBody.NewLoad(irtypes.I64, eevDataPtr)
	eevData := loopBody.NewIntToPtr(eevDataI64, irtypes.I8Ptr)

	// Map epoll events to PollEvent events:
	// EPOLLIN=1 → 1(readable), EPOLLOUT=4 → 2(writable), EPOLLERR=8|EPOLLHUP=16 → 4(error)
	hasIn := loopBody.NewAnd(eevEvents, constant.NewInt(irtypes.I32, 1))   // EPOLLIN
	hasOut := loopBody.NewAnd(eevEvents, constant.NewInt(irtypes.I32, 4))  // EPOLLOUT
	hasErr := loopBody.NewAnd(eevEvents, constant.NewInt(irtypes.I32, 24)) // EPOLLERR|EPOLLHUP

	isIn := loopBody.NewICmp(enum.IPredNE, hasIn, constant.NewInt(irtypes.I32, 0))
	readBit := loopBody.NewSelect(isIn, constant.NewInt(irtypes.I32, 1), constant.NewInt(irtypes.I32, 0))
	isOut := loopBody.NewICmp(enum.IPredNE, hasOut, constant.NewInt(irtypes.I32, 0))
	writeBit := loopBody.NewSelect(isOut, constant.NewInt(irtypes.I32, 2), constant.NewInt(irtypes.I32, 0))
	isE := loopBody.NewICmp(enum.IPredNE, hasErr, constant.NewInt(irtypes.I32, 0))
	errBit := loopBody.NewSelect(isE, constant.NewInt(irtypes.I32, 4), constant.NewInt(irtypes.I32, 0))
	events := loopBody.NewOr(readBit, writeBit)
	eventsAll := loopBody.NewOr(events, errBit)

	// Write PollEvent[i]
	pe := loopBody.NewGetElementPtr(peType, outBuf, i)
	peUserdata := loopBody.NewGetElementPtr(peType, pe, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	loopBody.NewStore(eevData, peUserdata)
	peEvents := loopBody.NewGetElementPtr(peType, pe, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	loopBody.NewStore(eventsAll, peEvents)
	pePad := loopBody.NewGetElementPtr(peType, pe, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	loopBody.NewStore(constant.NewInt(irtypes.I32, 0), pePad)

	iNext := loopBody.NewAdd(i, constant.NewInt(irtypes.I32, 1))
	i.Incs = append(i.Incs, ir.NewIncoming(iNext, loopBody))
	loopBody.NewBr(loopCond)

	loopEnd.NewRet(count)
}

// EmitReactorClose defines @pal_reactor_close(i32 rfd) → i32 (0 or -errno).
// Closes the reactor fd (epoll/kqueue fd).
func (p *PosixPAL) EmitReactorClose(module *ir.Module) *ir.Func {
	closeFn := getOrDeclareFunc(module, "close", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))

	fn := module.NewFunc("pal_reactor_close", irtypes.I32,
		ir.NewParam("rfd", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(closeFn, fn.Params[0])

	isErr := entry.NewICmp(enum.IPredSLT, ret, constant.NewInt(irtypes.I32, 0))
	okBlk := fn.NewBlock(".ok")
	errBlk := fn.NewBlock(".err")
	entry.NewCondBr(isErr, errBlk, okBlk)

	p.emitNegErrnoReturnI32(errBlk, p.getOrDeclareErrnoLocFn(module))
	okBlk.NewRet(ret)
	return fn
}
