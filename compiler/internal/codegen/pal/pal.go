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
	// EmitSpawnEnv defines @pal_spawn_env(i8* program, i8** argv, i8** envp, i8* cwd, i32* out_stdout_fd, i32* out_stderr_fd) → i32
	// Like EmitSpawn but with optional envp (NULL=inherit) and cwd (NULL=inherit).
	EmitSpawnEnv(module *ir.Module) *ir.Func
	// EmitSpawnStreamingEnv defines @pal_spawn_streaming_env(i8* program, i8** argv, i8** envp, i8* cwd, i32* out_stdin_fd, i32* out_stdout_fd, i32* out_stderr_fd) → i32
	// Like EmitSpawnStreaming but with optional envp (NULL=inherit) and cwd (NULL=inherit).
	EmitSpawnStreamingEnv(module *ir.Module) *ir.Func
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

	// Stack overflow detection (B0010)
	// EmitStackOverflowInit defines @pal_stack_overflow_init() → void
	// Registers a SIGSEGV/SIGBUS handler that prints "fatal: stack overflow"
	// and terminates cleanly. Called once at program startup.
	EmitStackOverflowInit(module *ir.Module) *ir.Func
	// EmitStackOverflowThreadInit defines @pal_stack_overflow_thread_init() → void
	// Sets up a per-thread alternate signal stack via sigaltstack.
	// Called at the start of each worker thread (sched_loop entry).
	EmitStackOverflowThreadInit(module *ir.Module) *ir.Func

	// Socket primitives (Networking, T0069)
	// EmitSocketCreate defines @pal_socket_create(i32 domain, i32 type, i32 protocol) → i32 (fd or -errno)
	EmitSocketCreate(module *ir.Module) *ir.Func
	// EmitSocketBind defines @pal_socket_bind(i32 fd, i8* addr, i32 addrlen) → i32 (0 or -errno)
	EmitSocketBind(module *ir.Module) *ir.Func
	// EmitSocketListen defines @pal_socket_listen(i32 fd, i32 backlog) → i32 (0 or -errno)
	EmitSocketListen(module *ir.Module) *ir.Func
	// EmitSocketAccept defines @pal_socket_accept(i32 fd, i8* addr, i32* addrlen) → i32 (fd or -errno)
	EmitSocketAccept(module *ir.Module) *ir.Func
	// EmitSocketConnect defines @pal_socket_connect(i32 fd, i8* addr, i32 addrlen) → i32 (0 or -errno)
	EmitSocketConnect(module *ir.Module) *ir.Func
	// EmitSocketSend defines @pal_socket_send(i32 fd, i8* buf, i64 len, i32 flags) → i64 (bytes or -errno)
	EmitSocketSend(module *ir.Module) *ir.Func
	// EmitSocketRecv defines @pal_socket_recv(i32 fd, i8* buf, i64 len, i32 flags) → i64 (bytes or -errno)
	EmitSocketRecv(module *ir.Module) *ir.Func
	// EmitSocketClose defines @pal_socket_close(i32 fd) → i32 (0 or -errno)
	EmitSocketClose(module *ir.Module) *ir.Func
	// EmitSocketSetOpt defines @pal_socket_setopt(i32 fd, i32 level, i32 opt, i8* val, i32 len) → i32 (0 or -errno)
	EmitSocketSetOpt(module *ir.Module) *ir.Func
	// EmitSocketShutdown defines @pal_socket_shutdown(i32 fd, i32 how) → i32 (0 or -errno)
	EmitSocketShutdown(module *ir.Module) *ir.Func
	// EmitSocketSetNonBlock defines @pal_socket_set_nonblock(i32 fd) → i32 (0 or -errno)
	EmitSocketSetNonBlock(module *ir.Module) *ir.Func
	// EmitSocketGetError defines @pal_socket_get_error(i32 fd) → i32 (errno value, 0 = no error)
	EmitSocketGetError(module *ir.Module) *ir.Func
	// EmitGetAddrInfo defines @pal_getaddrinfo(i8* host, i8* port, i8* hints, i8** result) → i32 (0 or EAI_*)
	EmitGetAddrInfo(module *ir.Module) *ir.Func
	// EmitFreeAddrInfo defines @pal_freeaddrinfo(i8* result) → void
	EmitFreeAddrInfo(module *ir.Module) *ir.Func

	// IO reactor primitives (T0070)
	// EmitReactorCreate defines @pal_reactor_create() → i32 (fd or -errno)
	EmitReactorCreate(module *ir.Module) *ir.Func
	// EmitReactorAdd defines @pal_reactor_add(i32 rfd, i32 fd, i8* userdata) → i32 (0 or -errno)
	// Registers fd for edge-triggered read+write monitoring.
	EmitReactorAdd(module *ir.Module) *ir.Func
	// EmitReactorRemove defines @pal_reactor_remove(i32 rfd, i32 fd) → i32 (0 or -errno)
	EmitReactorRemove(module *ir.Module) *ir.Func
	// EmitReactorPoll defines @pal_reactor_poll(i32 rfd, i8* events_buf, i32 max_events, i32 timeout_ms) → i32 (count or -errno)
	// Writes PollEvent structs {i8* userdata, i32 events, i32 _pad} to events_buf.
	// events: 1=readable, 2=writable, 4=error/hangup.
	EmitReactorPoll(module *ir.Module) *ir.Func
	// EmitReactorClose defines @pal_reactor_close(i32 rfd) → i32 (0 or -errno)
	EmitReactorClose(module *ir.Module) *ir.Func

	// High-level socket address operations (T0071)
	// These construct sockaddr_in internally via inet_pton, avoiding complex C struct
	// construction in the codegen bridge. Host must be a null-terminated IPv4 address string.
	// EmitSocketBindAddr defines @pal_socket_bind_addr(i32 fd, i8* host, i32 port) → i32 (0 or -errno)
	// Parses host via inet_pton, constructs sockaddr_in, sets SO_REUSEADDR, calls bind.
	EmitSocketBindAddr(module *ir.Module) *ir.Func
	// EmitSocketConnectAddr defines @pal_socket_connect_addr(i32 fd, i8* host, i32 port) → i32 (0 or -errno/-EINPROGRESS)
	// Parses host via inet_pton, constructs sockaddr_in, calls connect.
	EmitSocketConnectAddr(module *ir.Module) *ir.Func
	// EmitSocketAcceptAddr defines @pal_socket_accept_addr(i32 listen_fd) → i32 (fd or -errno)
	// Calls accept with NULL addr (no address extraction needed).
	EmitSocketAcceptAddr(module *ir.Module) *ir.Func
	// EmitSocketGetLocalPort defines @pal_socket_get_local_port(i32 fd) → i32 (port or -errno)
	// Calls getsockname, extracts port from sockaddr_in, returns in host byte order.
	EmitSocketGetLocalPort(module *ir.Module) *ir.Func
}

// ForTarget returns a PAL implementation for the given LLVM target triple.
func ForTarget(triple string) PAL {
	switch {
	case strings.Contains(triple, "windows"):
		return &WindowsPAL{}
	case strings.Contains(triple, "wasm") && strings.Contains(triple, "web"):
		return &WasmWebPAL{}
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

// addFuncAttr appends attr to fn.FuncAttrs only if not already present.
func addFuncAttr(fn *ir.Func, attr enum.FuncAttr) {
	for _, a := range fn.FuncAttrs {
		if a == attr {
			return
		}
	}
	fn.FuncAttrs = append(fn.FuncAttrs, attr)
}

// getOrCreateAllocCountGlobal returns the @__promise_alloc_count global,
// creating it if it doesn't exist. Used by alloc/free tracking (T0020).
func getOrCreateAllocCountGlobal(module *ir.Module) *ir.Global {
	for _, g := range module.Globals {
		if g.Name() == "__promise_alloc_count" {
			return g
		}
	}
	g := module.NewGlobal("__promise_alloc_count", irtypes.I64)
	g.Init = constant.NewInt(irtypes.I64, 0)
	return g
}

// emitLibcAlloc declares libc @malloc and defines @pal_alloc as a wrapper.
// Shared by all PALs that use libc for allocation.
// Includes allocation count tracking for leak detection (T0020).
func emitLibcAlloc(module *ir.Module) *ir.Func {
	// declare noalias i8* @malloc(i64 noundef) nounwind willreturn
	mallocSize := ir.NewParam("size", irtypes.I64)
	mallocSize.Attrs = append(mallocSize.Attrs, enum.ParamAttrNoUndef)
	mallocFn := getOrDeclareFunc(module, "malloc", irtypes.I8Ptr, mallocSize)
	mallocFn.ReturnAttrs = append(mallocFn.ReturnAttrs, enum.ReturnAttrNoAlias)
	addFuncAttr(mallocFn, enum.FuncAttrWillReturn)

	allocCount := getOrCreateAllocCountGlobal(module)

	// define noalias i8* @pal_alloc(i64 %size) nounwind willreturn
	fn := module.NewFunc("pal_alloc", irtypes.I8Ptr,
		ir.NewParam("size", irtypes.I64))
	fn.ReturnAttrs = append(fn.ReturnAttrs, enum.ReturnAttrNoAlias)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(mallocFn, fn.Params[0])

	// Track: if malloc returned non-null, atomically increment alloc count
	nonnull := entry.NewICmp(enum.IPredNE, ret, constant.NewNull(irtypes.I8Ptr))
	trackBlk := fn.NewBlock(".track")
	doneBlk := fn.NewBlock(".done")
	entry.NewCondBr(nonnull, trackBlk, doneBlk)

	trackBlk.NewAtomicRMW(enum.AtomicOpAdd, allocCount, constant.NewInt(irtypes.I64, 1), enum.AtomicOrderingMonotonic)
	trackBlk.NewBr(doneBlk)

	doneBlk.NewRet(ret)

	return fn
}

// emitLibcFree declares libc @free and defines @pal_free as a wrapper.
// Includes allocation count tracking for leak detection (T0020).
func emitLibcFree(module *ir.Module) *ir.Func {
	// declare void @free(i8* nocapture noundef) nounwind willreturn
	freePtr := ir.NewParam("ptr", irtypes.I8Ptr)
	freePtr.Attrs = append(freePtr.Attrs, enum.ParamAttrNoCapture, enum.ParamAttrNoUndef)
	freeFn := getOrDeclareFunc(module, "free", irtypes.Void, freePtr)
	addFuncAttr(freeFn, enum.FuncAttrWillReturn)

	allocCount := getOrCreateAllocCountGlobal(module)

	// define void @pal_free(i8* %ptr) nounwind willreturn
	fn := module.NewFunc("pal_free", irtypes.Void,
		ir.NewParam("ptr", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock(".entry")

	// Track: if ptr is non-null, atomically decrement alloc count and free
	nonnull := entry.NewICmp(enum.IPredNE, fn.Params[0], constant.NewNull(irtypes.I8Ptr))
	trackBlk := fn.NewBlock(".track")
	doneBlk := fn.NewBlock(".done")
	entry.NewCondBr(nonnull, trackBlk, doneBlk)

	trackBlk.NewAtomicRMW(enum.AtomicOpSub, allocCount, constant.NewInt(irtypes.I64, 1), enum.AtomicOrderingMonotonic)
	trackBlk.NewCall(freeFn, fn.Params[0])
	trackBlk.NewBr(doneBlk)

	doneBlk.NewRet(nil)

	return fn
}

// emitLibcFreeDebug declares libc @free + a platform-specific size query function
// and defines @pal_free with poison-fill (0xDE) before free for UAF detection.
// sizeFnName is the platform's allocation-size query: "malloc_usable_size" (Linux),
// "malloc_size" (macOS), or "_msize" (Windows).
func emitLibcFreeDebug(module *ir.Module, sizeFnName string) *ir.Func {
	// declare void @free(i8* nocapture noundef) nounwind willreturn
	freePtr := ir.NewParam("ptr", irtypes.I8Ptr)
	freePtr.Attrs = append(freePtr.Attrs, enum.ParamAttrNoCapture, enum.ParamAttrNoUndef)
	freeFn := getOrDeclareFunc(module, "free", irtypes.Void, freePtr)
	addFuncAttr(freeFn, enum.FuncAttrWillReturn)

	// declare i64 @malloc_usable_size(i8*) / @_msize(i8*)
	sizeFn := getOrDeclareFunc(module, sizeFnName, irtypes.I64,
		ir.NewParam("ptr", irtypes.I8Ptr))

	// declare i8* @memset(i8*, i32, i64)
	memsetFn := getOrDeclareFunc(module, "memset", irtypes.I8Ptr,
		ir.NewParam("dest", irtypes.I8Ptr),
		ir.NewParam("c", irtypes.I32),
		ir.NewParam("n", irtypes.I64))

	allocCount := getOrCreateAllocCountGlobal(module)

	// define void @pal_free(i8* %ptr) nounwind willreturn
	fn := module.NewFunc("pal_free", irtypes.Void,
		ir.NewParam("ptr", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock(".entry")

	// Track: if ptr is non-null, poison-fill + decrement alloc count + free
	nonnull := entry.NewICmp(enum.IPredNE, fn.Params[0], constant.NewNull(irtypes.I8Ptr))
	trackBlk := fn.NewBlock(".track")
	doneBlk := fn.NewBlock(".done")
	entry.NewCondBr(nonnull, trackBlk, doneBlk)

	trackBlk.NewAtomicRMW(enum.AtomicOpSub, allocCount, constant.NewInt(irtypes.I64, 1), enum.AtomicOrderingMonotonic)
	// Poison-fill: memset(ptr, 0xDE, malloc_usable_size(ptr))
	size := trackBlk.NewCall(sizeFn, fn.Params[0])
	trackBlk.NewCall(memsetFn, fn.Params[0], constant.NewInt(irtypes.I32, 0xDE), size)
	trackBlk.NewCall(freeFn, fn.Params[0])
	trackBlk.NewBr(doneBlk)

	doneBlk.NewRet(nil)

	return fn
}

// emitLibcRealloc declares libc @realloc and defines @pal_realloc as a wrapper.
// Includes allocation count tracking for leak detection (T0066):
// - realloc(NULL, size) acts like malloc → increment alloc count
// - realloc(ptr, 0) acts like free → decrement alloc count
// - realloc(ptr, size) resizes → no count change
func emitLibcRealloc(module *ir.Module) *ir.Func {
	// declare noalias i8* @realloc(i8* nocapture noundef, i64 noundef) nounwind willreturn
	reallocPtr := ir.NewParam("ptr", irtypes.I8Ptr)
	reallocPtr.Attrs = append(reallocPtr.Attrs, enum.ParamAttrNoCapture, enum.ParamAttrNoUndef)
	reallocSz := ir.NewParam("size", irtypes.I64)
	reallocSz.Attrs = append(reallocSz.Attrs, enum.ParamAttrNoUndef)
	reallocFn := getOrDeclareFunc(module, "realloc", irtypes.I8Ptr, reallocPtr, reallocSz)
	reallocFn.ReturnAttrs = append(reallocFn.ReturnAttrs, enum.ReturnAttrNoAlias)
	addFuncAttr(reallocFn, enum.FuncAttrWillReturn)

	allocCount := getOrCreateAllocCountGlobal(module)
	zero64 := constant.NewInt(irtypes.I64, 0)
	one64 := constant.NewInt(irtypes.I64, 1)
	nullPtr := constant.NewNull(irtypes.I8Ptr)

	// define noalias i8* @pal_realloc(i8* %ptr, i64 %size) nounwind willreturn
	fn := module.NewFunc("pal_realloc", irtypes.I8Ptr,
		ir.NewParam("ptr", irtypes.I8Ptr),
		ir.NewParam("size", irtypes.I64))
	fn.ReturnAttrs = append(fn.ReturnAttrs, enum.ReturnAttrNoAlias)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock(".entry")

	// Check if old ptr is null (realloc(NULL, size) = malloc)
	ptrIsNull := entry.NewICmp(enum.IPredEQ, fn.Params[0], nullPtr)
	// Check if new size is zero (realloc(ptr, 0) = free)
	sizeIsZero := entry.NewICmp(enum.IPredEQ, fn.Params[1], zero64)

	ret := entry.NewCall(reallocFn, fn.Params[0], fn.Params[1])

	// Case 1: realloc(NULL, size) with non-null result → new allocation
	newAllocBlk := fn.NewBlock(".new_alloc")
	checkFreeBlk := fn.NewBlock(".check_free")
	entry.NewCondBr(ptrIsNull, newAllocBlk, checkFreeBlk)

	retNonNull := newAllocBlk.NewICmp(enum.IPredNE, ret, nullPtr)
	incBlk := fn.NewBlock(".inc")
	doneBlk := fn.NewBlock(".done")
	newAllocBlk.NewCondBr(retNonNull, incBlk, doneBlk)
	incBlk.NewAtomicRMW(enum.AtomicOpAdd, allocCount, one64, enum.AtomicOrderingMonotonic)
	incBlk.NewBr(doneBlk)

	// Case 2: realloc(ptr, 0) with non-null ptr → deallocation
	decBlk := fn.NewBlock(".dec")
	checkFreeBlk.NewCondBr(sizeIsZero, decBlk, doneBlk)
	decBlk.NewAtomicRMW(enum.AtomicOpSub, allocCount, one64, enum.AtomicOrderingMonotonic)
	decBlk.NewBr(doneBlk)

	// Normal case: resize — no count change
	doneBlk.NewRet(ret)

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

// emitStubSpawnEnv returns -1 (no process execution support).
func emitStubSpawnEnv(module *ir.Module) *ir.Func {
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)
	i32PtrType := irtypes.NewPointer(irtypes.I32)
	fn := module.NewFunc("pal_spawn_env", irtypes.I32,
		ir.NewParam("program", irtypes.I8Ptr),
		ir.NewParam("argv", i8PtrPtrType),
		ir.NewParam("envp", i8PtrPtrType),
		ir.NewParam("cwd", irtypes.I8Ptr),
		ir.NewParam("out_stdout_fd", i32PtrType),
		ir.NewParam("out_stderr_fd", i32PtrType))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewStore(constant.NewInt(irtypes.I32, -1), fn.Params[4])
	entry.NewStore(constant.NewInt(irtypes.I32, -1), fn.Params[5])
	entry.NewRet(constant.NewInt(irtypes.I32, -1))
	return fn
}

// emitStubSpawnStreamingEnv returns -1 (no streaming process support with env).
func emitStubSpawnStreamingEnv(module *ir.Module) *ir.Func {
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)
	i32PtrType := irtypes.NewPointer(irtypes.I32)
	fn := module.NewFunc("pal_spawn_streaming_env", irtypes.I32,
		ir.NewParam("program", irtypes.I8Ptr),
		ir.NewParam("argv", i8PtrPtrType),
		ir.NewParam("envp", i8PtrPtrType),
		ir.NewParam("cwd", irtypes.I8Ptr),
		ir.NewParam("out_stdin_fd", i32PtrType),
		ir.NewParam("out_stdout_fd", i32PtrType),
		ir.NewParam("out_stderr_fd", i32PtrType))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewStore(constant.NewInt(irtypes.I32, -1), fn.Params[4])
	entry.NewStore(constant.NewInt(irtypes.I32, -1), fn.Params[5])
	entry.NewStore(constant.NewInt(irtypes.I32, -1), fn.Params[6])
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

// --- Stub stack overflow detection ---

// emitStubStackOverflowInit is a no-op (platforms with built-in stack overflow handling).
func emitStubStackOverflowInit(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_stack_overflow_init", irtypes.Void)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(nil)
	return fn
}

// emitStubStackOverflowThreadInit is a no-op.
func emitStubStackOverflowThreadInit(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_stack_overflow_thread_init", irtypes.Void)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(nil)
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

// --- Stub socket implementations (used by WASM and Windows PALs, T0069) ---

// ENOSYS = 38 on Linux; used as generic "not implemented" for socket stubs.
const enosys = 38

func emitStubSocketCreate(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_socket_create", irtypes.I32,
		ir.NewParam("domain", irtypes.I32),
		ir.NewParam("typ", irtypes.I32),
		ir.NewParam("protocol", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

func emitStubSocketBind(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_socket_bind", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("addr", irtypes.I8Ptr),
		ir.NewParam("addrlen", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

func emitStubSocketListen(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_socket_listen", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("backlog", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

func emitStubSocketAccept(module *ir.Module) *ir.Func {
	i32PtrType := irtypes.NewPointer(irtypes.I32)
	fn := module.NewFunc("pal_socket_accept", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("addr", irtypes.I8Ptr),
		ir.NewParam("addrlen", i32PtrType))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

func emitStubSocketConnect(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_socket_connect", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("addr", irtypes.I8Ptr),
		ir.NewParam("addrlen", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

func emitStubSocketSend(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_socket_send", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64),
		ir.NewParam("flags", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I64, -enosys))
	return fn
}

func emitStubSocketRecv(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_socket_recv", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64),
		ir.NewParam("flags", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I64, -enosys))
	return fn
}

func emitStubSocketClose(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_socket_close", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

func emitStubSocketSetOpt(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_socket_setopt", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("level", irtypes.I32),
		ir.NewParam("opt", irtypes.I32),
		ir.NewParam("val", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

func emitStubSocketShutdown(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_socket_shutdown", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("how", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

func emitStubSocketSetNonBlock(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_socket_set_nonblock", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

func emitStubSocketGetError(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_socket_get_error", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

func emitStubGetAddrInfo(module *ir.Module) *ir.Func {
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)
	fn := module.NewFunc("pal_getaddrinfo", irtypes.I32,
		ir.NewParam("host", irtypes.I8Ptr),
		ir.NewParam("port", irtypes.I8Ptr),
		ir.NewParam("hints", irtypes.I8Ptr),
		ir.NewParam("result", i8PtrPtrType))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

func emitStubFreeAddrInfo(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_freeaddrinfo", irtypes.Void,
		ir.NewParam("result", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(nil)
	return fn
}

// --- Stub IO reactor implementations (used by WASM and Windows PALs, T0070) ---

func emitStubReactorCreate(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_reactor_create", irtypes.I32)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

func emitStubReactorAdd(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_reactor_add", irtypes.I32,
		ir.NewParam("rfd", irtypes.I32),
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("userdata", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

func emitStubReactorRemove(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_reactor_remove", irtypes.I32,
		ir.NewParam("rfd", irtypes.I32),
		ir.NewParam("fd", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

func emitStubReactorPoll(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_reactor_poll", irtypes.I32,
		ir.NewParam("rfd", irtypes.I32),
		ir.NewParam("events_buf", irtypes.I8Ptr),
		ir.NewParam("max_events", irtypes.I32),
		ir.NewParam("timeout_ms", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

func emitStubReactorClose(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_reactor_close", irtypes.I32,
		ir.NewParam("rfd", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

// --- Stub high-level socket address implementations (T0071) ---

func emitStubSocketBindAddr(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_socket_bind_addr", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("host", irtypes.I8Ptr),
		ir.NewParam("port", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

func emitStubSocketConnectAddr(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_socket_connect_addr", irtypes.I32,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("host", irtypes.I8Ptr),
		ir.NewParam("port", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

func emitStubSocketAcceptAddr(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_socket_accept_addr", irtypes.I32,
		ir.NewParam("listen_fd", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}

func emitStubSocketGetLocalPort(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_socket_get_local_port", irtypes.I32,
		ir.NewParam("fd", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -enosys))
	return fn
}
