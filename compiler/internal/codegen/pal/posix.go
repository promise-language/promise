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
