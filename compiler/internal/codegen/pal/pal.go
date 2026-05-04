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

	// File I/O primitives (Phase D)
	// EmitFileOpen defines @pal_file_open(i8* path, i32 mode) → i32 (fd or -1)
	// mode: 0=open(rw), 1=read(ro), 2=create(rw,trunc), 3=append(rw,create)
	EmitFileOpen(module *ir.Module) *ir.Func
	// EmitFileRead defines @pal_file_read(i32 fd, i8* buf, i64 len) → i64
	EmitFileRead(module *ir.Module) *ir.Func
	// EmitFileWrite defines @pal_file_write(i32 fd, i8* buf, i64 len) → i64
	EmitFileWrite(module *ir.Module) *ir.Func
	// EmitFileClose defines @pal_file_close(i32 fd) → i32 (0=ok, -1=error)
	EmitFileClose(module *ir.Module) *ir.Func
	// EmitFileSeek defines @pal_file_seek(i32 fd, i64 offset, i32 whence) → i64
	EmitFileSeek(module *ir.Module) *ir.Func
	// EmitFileStatSize defines @pal_file_stat_size(i8* path) → i64 (-1=error)
	EmitFileStatSize(module *ir.Module) *ir.Func
	// EmitFileRemove defines @pal_file_remove(i8* path) → i32 (0=ok, -1=error)
	EmitFileRemove(module *ir.Module) *ir.Func
	// EmitFileExists defines @pal_file_exists(i8* path) → i32 (1=yes, 0=no)
	EmitFileExists(module *ir.Module) *ir.Func
	// EmitFileMkdir defines @pal_file_mkdir(i8* path) → i32 (0=ok, -1=error)
	EmitFileMkdir(module *ir.Module) *ir.Func
	// EmitDirRemove defines @pal_dir_remove(i8* path) → i32 (0=ok, -1=error)
	EmitDirRemove(module *ir.Module) *ir.Func
	// EmitDirExists defines @pal_dir_exists(i8* path) → i32 (1=yes, 0=no)
	EmitDirExists(module *ir.Module) *ir.Func
	// EmitErrno defines @pal_errno() → i32
	EmitErrno(module *ir.Module) *ir.Func

	// OS / Environment primitives
	// EmitGetEnv defines @pal_getenv(i8* name) → i8* (value or null)
	EmitGetEnv(module *ir.Module) *ir.Func
	// EmitGetCwd defines @pal_getcwd(i8* buf, i64 len) → i8* (buf or null)
	EmitGetCwd(module *ir.Module) *ir.Func

	// Environment mutation primitives
	// EmitSetEnv defines @pal_setenv(i8* name, i8* value) → i32 (0=ok, -1=error)
	EmitSetEnv(module *ir.Module) *ir.Func
	// EmitUnsetEnv defines @pal_unsetenv(i8* name) → i32 (0=ok, -1=error)
	EmitUnsetEnv(module *ir.Module) *ir.Func
	// EmitChdir defines @pal_chdir(i8* path) → i32 (0=ok, -1=error)
	EmitChdir(module *ir.Module) *ir.Func

	// Process execution primitives (decomposed for concurrent pipe reads)
	// EmitSpawn defines @pal_spawn(i8* program, i8** argv, i32* out_stdout_fd, i32* out_stderr_fd) → i32
	// Forks and execs program with argv. Returns child pid on success, -1 on error.
	// Output params receive the read ends of stdout/stderr pipes.
	EmitSpawn(module *ir.Module) *ir.Func
	// EmitReadPipe defines @pal_read_pipe(i32 fd, i8** out_buf, i64* out_len) → void
	// Reads fd to EOF into malloc'd buffer, then closes fd. Caller must free out_buf.
	EmitReadPipe(module *ir.Module) *ir.Func
	// EmitWaitPid defines @pal_wait_pid(i32 pid) → i32
	// Waits for child process. Returns exit code (0-255) on success, -1 on error.
	// Retries on EINTR.
	EmitWaitPid(module *ir.Module) *ir.Func

	// Streaming process execution (stdin+stdout+stderr pipes)
	// EmitSpawnStreaming defines @pal_spawn_streaming(i8* program, i8** argv, i32* out_stdin_fd, i32* out_stdout_fd, i32* out_stderr_fd) → i32
	// Like EmitSpawn but also creates a stdin pipe. Returns child pid on success, -1 on error.
	// out_stdin_fd receives the write end of the stdin pipe; out_stdout_fd/out_stderr_fd receive read ends.
	EmitSpawnStreaming(module *ir.Module) *ir.Func
	// EmitKill defines @pal_kill(i32 pid, i32 signal) → i32
	// Sends a signal to a process. Returns 0 on success, -1 on error.
	EmitKill(module *ir.Module) *ir.Func

	// Directory listing primitives (Phase D)
	// EmitDirOpen defines @pal_dir_open(i8* path) → i8* (DIR*/handle or null)
	EmitDirOpen(module *ir.Module) *ir.Func
	// EmitDirNextName defines @pal_dir_next_name(i8* handle) → i8* (name or null)
	// Returns pointer to null-terminated entry name, or null when done.
	// On error, returns null and errno is set.
	EmitDirNextName(module *ir.Module) *ir.Func
	// EmitDirClose defines @pal_dir_close(i8* handle) → void
	EmitDirClose(module *ir.Module) *ir.Func

	// OS info primitives
	// EmitGetEnviron defines @pal_get_environ() → i8** (pointer to environ global, null-terminated)
	EmitGetEnviron(module *ir.Module) *ir.Func
	// EmitGetUserInfo defines @pal_get_user_info(i8** out_name, i8** out_dir, i32* out_uid, i32* out_gid) → i32
	// Calls getpwuid(getuid()). Returns 0 on success, -1 on error. out_name/out_dir point to static storage.
	EmitGetUserInfo(module *ir.Module) *ir.Func
	// EmitGetHostname defines @pal_get_hostname(i8* buf, i64 len) → i8* (buf on success, null on error)
	EmitGetHostname(module *ir.Module) *ir.Func

	// Signal handling primitives
	// EmitSignalInit defines @pal_signal_init() → i32
	// Creates a pipe for signal delivery, defines signal handler function.
	// Returns read fd on success, -1 on error. Write fd stored in global for handler.
	EmitSignalInit(module *ir.Module) *ir.Func
	// EmitSignalRegister defines @pal_signal_register(i32 signum) → i32
	// Registers the signal handler for the given signal number.
	// Returns 0 on success, -1 on error.
	EmitSignalRegister(module *ir.Module) *ir.Func
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

// getOrDeclareFunc looks up an existing function by name, or declares it if not found.
// This makes Emit* functions order-independent — they no longer panic if a dependency
// hasn't been emitted yet. The function is declared (no body) with the given signature.
func getOrDeclareFunc(module *ir.Module, name string, retType irtypes.Type, params ...*ir.Param) *ir.Func {
	if fn := lookupFunc(module, name); fn != nil {
		return fn
	}
	fn := module.NewFunc(name, retType, params...)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	return fn
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
	entry := fn.NewBlock(".entry")
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
	entry := fn.NewBlock(".entry")
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
	entry := fn.NewBlock(".entry")
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
	entry := fn.NewBlock(".entry")

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
	entry := fn.NewBlock(".entry")
	entry.NewRet(nil)
	return fn
}

// emitStubMutexInit allocates a dummy 1-byte handle (no real locking).
func emitStubMutexInit(module *ir.Module) *ir.Func {
	palAlloc := lookupFunc(module, "pal_alloc")
	fn := module.NewFunc("pal_mutex_init", irtypes.I8Ptr)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	handle := entry.NewCall(palAlloc, constant.NewInt(irtypes.I64, 1))
	entry.NewRet(handle)
	return fn
}

// emitStubMutexLock is a no-op (single-threaded, no contention).
func emitStubMutexLock(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_mutex_lock", irtypes.Void,
		ir.NewParam("mutex", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(nil)
	return fn
}

// emitStubMutexUnlock is a no-op.
func emitStubMutexUnlock(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_mutex_unlock", irtypes.Void,
		ir.NewParam("mutex", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(nil)
	return fn
}

// emitStubMutexDestroy frees the dummy handle.
func emitStubMutexDestroy(module *ir.Module) *ir.Func {
	palFree := lookupFunc(module, "pal_free")
	fn := module.NewFunc("pal_mutex_destroy", irtypes.Void,
		ir.NewParam("mutex", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewCall(palFree, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// emitStubCondInit allocates a dummy 1-byte handle.
func emitStubCondInit(module *ir.Module) *ir.Func {
	palAlloc := lookupFunc(module, "pal_alloc")
	fn := module.NewFunc("pal_cond_init", irtypes.I8Ptr)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
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
	entry := fn.NewBlock(".entry")
	entry.NewRet(nil)
	return fn
}

// emitStubCondSignal is a no-op.
func emitStubCondSignal(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_cond_signal", irtypes.Void,
		ir.NewParam("cond", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(nil)
	return fn
}

// emitStubCondBroadcast is a no-op.
func emitStubCondBroadcast(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_cond_broadcast", irtypes.Void,
		ir.NewParam("cond", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(nil)
	return fn
}

// emitStubCondDestroy frees the dummy handle.
func emitStubCondDestroy(module *ir.Module) *ir.Func {
	palFree := lookupFunc(module, "pal_free")
	fn := module.NewFunc("pal_cond_destroy", irtypes.Void,
		ir.NewParam("cond", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewCall(palFree, fn.Params[0])
	entry.NewRet(nil)
	return fn
}

// emitStubNumCPUs returns 1 (single-threaded platforms: WASM, Windows stub).
func emitStubNumCPUs(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_num_cpus", irtypes.I32)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, 1))
	return fn
}

// --- Stub file I/O implementations (used by WASM PAL) ---

// emitStubFileOpen returns -1 (no file I/O support).
func emitStubFileOpen(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_file_open", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr),
		ir.NewParam("mode", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -1))
	return fn
}

// emitStubFileRead returns -1.
func emitStubFileRead(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_file_read", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I64, -1))
	return fn
}

// emitStubFileWrite returns -1.
func emitStubFileWrite(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_file_write", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I64, -1))
	return fn
}

// emitStubFileClose returns -1.
func emitStubFileClose(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_file_close", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -1))
	return fn
}

// emitStubFileSeek returns -1.
func emitStubFileSeek(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_file_seek", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("offset", irtypes.I64),
		ir.NewParam("whence", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I64, -1))
	return fn
}

// emitStubFileStatSize returns -1.
func emitStubFileStatSize(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_file_stat_size", irtypes.I64,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I64, -1))
	return fn
}

// emitStubFileRemove returns -1.
func emitStubFileRemove(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_file_remove", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -1))
	return fn
}

// emitStubFileExists returns 0 (not found).
func emitStubFileExists(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_file_exists", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, 0))
	return fn
}

// emitStubFileMkdir returns -1.
func emitStubFileMkdir(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_file_mkdir", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -1))
	return fn
}

// emitStubDirRemove returns -1.
func emitStubDirRemove(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_dir_remove", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -1))
	return fn
}

// emitStubDirExists returns 0 (not found).
func emitStubDirExists(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_dir_exists", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, 0))
	return fn
}

// emitStubErrno returns 0.
func emitStubErrno(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_errno", irtypes.I32)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, 0))
	return fn
}

// --- Stub directory listing implementations (used by WASM PAL) ---

// emitStubDirOpen returns null (no dir listing support).
func emitStubDirOpen(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_dir_open", irtypes.I8Ptr,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewNull(irtypes.I8Ptr))
	return fn
}

// emitStubDirNextName returns null.
func emitStubDirNextName(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_dir_next_name", irtypes.I8Ptr,
		ir.NewParam("handle", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewNull(irtypes.I8Ptr))
	return fn
}

// emitStubDirClose is a no-op.
func emitStubDirClose(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_dir_close", irtypes.Void,
		ir.NewParam("handle", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(nil)
	return fn
}

// --- Stub process execution implementations (used by WASM and Windows PALs) ---

// emitStubSpawn returns -1 (no process execution support).
// Sets out fd pointers to -1.
func emitStubSpawn(module *ir.Module) *ir.Func {
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)
	i32PtrType := irtypes.NewPointer(irtypes.I32)
	fn := module.NewFunc("pal_spawn", irtypes.I32,
		ir.NewParam("program", irtypes.I8Ptr),
		ir.NewParam("argv", i8PtrPtrType),
		ir.NewParam("out_stdout_fd", i32PtrType),
		ir.NewParam("out_stderr_fd", i32PtrType))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewStore(constant.NewInt(irtypes.I32, -1), fn.Params[2])
	entry.NewStore(constant.NewInt(irtypes.I32, -1), fn.Params[3])
	entry.NewRet(constant.NewInt(irtypes.I32, -1))
	return fn
}

// emitStubReadPipe stores null/0 to output pointers (no I/O support).
func emitStubReadPipe(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_read_pipe", irtypes.Void,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("out_buf", irtypes.NewPointer(irtypes.I8Ptr)),
		ir.NewParam("out_len", irtypes.NewPointer(irtypes.I64)))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), fn.Params[1])
	entry.NewStore(constant.NewInt(irtypes.I64, 0), fn.Params[2])
	entry.NewRet(nil)
	return fn
}

// emitStubWaitPid returns -1 (no process support).
func emitStubWaitPid(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_wait_pid", irtypes.I32,
		ir.NewParam("pid", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -1))
	return fn
}

// emitStubSpawnStreaming returns -1 (no streaming process support).
// Sets all out fd pointers to -1.
func emitStubSpawnStreaming(module *ir.Module) *ir.Func {
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)
	i32PtrType := irtypes.NewPointer(irtypes.I32)
	fn := module.NewFunc("pal_spawn_streaming", irtypes.I32,
		ir.NewParam("program", irtypes.I8Ptr),
		ir.NewParam("argv", i8PtrPtrType),
		ir.NewParam("out_stdin_fd", i32PtrType),
		ir.NewParam("out_stdout_fd", i32PtrType),
		ir.NewParam("out_stderr_fd", i32PtrType))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewStore(constant.NewInt(irtypes.I32, -1), fn.Params[2])
	entry.NewStore(constant.NewInt(irtypes.I32, -1), fn.Params[3])
	entry.NewStore(constant.NewInt(irtypes.I32, -1), fn.Params[4])
	entry.NewRet(constant.NewInt(irtypes.I32, -1))
	return fn
}

// emitStubKill returns -1 (no signal support).
func emitStubKill(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_kill", irtypes.I32,
		ir.NewParam("pid", irtypes.I32),
		ir.NewParam("signal", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -1))
	return fn
}

// --- Stub OS info implementations ---

// emitStubGetEnviron returns null (no environ support).
func emitStubGetEnviron(module *ir.Module) *ir.Func {
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)
	fn := module.NewFunc("pal_get_environ", i8PtrPtrType)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewNull(i8PtrPtrType))
	return fn
}

// emitStubGetUserInfo returns -1 (no user info support).
func emitStubGetUserInfo(module *ir.Module) *ir.Func {
	i32PtrType := irtypes.NewPointer(irtypes.I32)
	fn := module.NewFunc("pal_get_user_info", irtypes.I32,
		ir.NewParam("out_name", irtypes.NewPointer(irtypes.I8Ptr)),
		ir.NewParam("out_dir", irtypes.NewPointer(irtypes.I8Ptr)),
		ir.NewParam("out_uid", i32PtrType),
		ir.NewParam("out_gid", i32PtrType))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), fn.Params[0])
	entry.NewStore(constant.NewNull(irtypes.I8Ptr), fn.Params[1])
	entry.NewStore(constant.NewInt(irtypes.I32, -1), fn.Params[2])
	entry.NewStore(constant.NewInt(irtypes.I32, -1), fn.Params[3])
	entry.NewRet(constant.NewInt(irtypes.I32, -1))
	return fn
}

// emitStubGetHostname returns null (no hostname support).
func emitStubGetHostname(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_get_hostname", irtypes.I8Ptr,
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewNull(irtypes.I8Ptr))
	return fn
}

// --- Stub signal handling implementations ---

// emitStubSignalInit returns -1 (no signal support).
func emitStubSignalInit(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_signal_init", irtypes.I32)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -1))
	return fn
}

// emitStubSignalRegister returns -1 (no signal support).
func emitStubSignalRegister(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_signal_register", irtypes.I32,
		ir.NewParam("signum", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -1))
	return fn
}

// --- Stub environment mutation / chdir implementations ---

// emitStubSetEnv returns -1 (no env mutation support).
func emitStubSetEnv(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_setenv", irtypes.I32,
		ir.NewParam("name", irtypes.I8Ptr),
		ir.NewParam("value", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -1))
	return fn
}

// emitStubUnsetEnv returns -1 (no env mutation support).
func emitStubUnsetEnv(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_unsetenv", irtypes.I32,
		ir.NewParam("name", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -1))
	return fn
}

// emitStubChdir returns -1 (no filesystem).
func emitStubChdir(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_chdir", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -1))
	return fn
}

// --- Stub OS / Environment implementations (used by WASM PAL) ---

// emitStubGetEnv returns null (variable not found).
func emitStubGetEnv(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_getenv", irtypes.I8Ptr,
		ir.NewParam("name", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewNull(irtypes.I8Ptr))
	return fn
}

// emitStubGetCwd returns null (no filesystem).
func emitStubGetCwd(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_getcwd", irtypes.I8Ptr,
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewNull(irtypes.I8Ptr))
	return fn
}
