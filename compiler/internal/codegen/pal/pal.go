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
	"github.com/llir/llvm/ir/value"
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
	// EmitPipeRead defines @pal_pipe_read(i32 fd, i8* buf, i64 len) → i64 (bytes, 0=EOF, <0=error)
	// Streaming subprocess pipe read. On Windows, fd packs a raw HANDLE (ReadFile),
	// not a CRT fd — unlike pal_file_read which uses UCRT _read.
	EmitPipeRead(module *ir.Module) *ir.Func
	// EmitPipeWrite defines @pal_pipe_write(i32 fd, i8* buf, i64 len) → i64 (bytes, <0=error)
	// Streaming subprocess pipe write. On Windows, fd packs a raw HANDLE (WriteFile).
	EmitPipeWrite(module *ir.Module) *ir.Func
	// EmitPipeClose defines @pal_pipe_close(i32 fd) → i32 (0=ok, -1=error)
	// Streaming subprocess pipe close. On Windows, fd packs a raw HANDLE (CloseHandle).
	EmitPipeClose(module *ir.Module) *ir.Func
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
	// EmitExecReplace defines @pal_exec_replace(i8* path, i8** argv) → i32
	// Replaces the current process image with path (T0770, the stub launcher).
	// Unix: execv(path, argv) — same PID, never returns on success; -1 on failure.
	// Windows: no true execve — CreateProcess + wait + ExitProcess(child code);
	// never returns on success, -1 only if the process cannot be launched.
	EmitExecReplace(module *ir.Module) *ir.Func

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

	// File metadata (D0012)
	// EmitFileStat defines @pal_file_stat(i8* path, i64* out, i32 follow) → i32 (0 or -errno)
	// Calls stat (follow=1) or lstat (follow=0), writes 8 normalized i64 values to out[0..7]:
	//   out[0]=size, out[1]=mode&07777, out[2]=uid, out[3]=gid,
	//   out[4]=mtime_ns, out[5]=atime_ns, out[6]=ctime_ns, out[7]=file_type(1/2/3/4)
	EmitFileStat(module *ir.Module) *ir.Func
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

// Memory limit accounting (T0689).
//
// When MemoryLimitAccounting is enabled (used by test binaries with -memory-limit > 0),
// each pal_alloc atomically adds its requested size to @__promise_memory_used_bytes;
// if the new total minus @__promise_memory_start_bytes exceeds @__promise_memory_limit_bytes
// (when non-zero), the process prints "fatal: memory limit exceeded\n" to stderr and
// calls exit(134).
//
// Reuses the debug allocator's existing 8-byte requested_size field at internal+8;
// no new per-allocation bookkeeping. Accounting code is emitted only when the flag
// is on — non-test builds (run/exec/build) emit the plain allocator with zero overhead.
//
// Snapshot semantics: each test snapshots `used` into `start` before running; the
// effective per-test usage is `used - start`. This lets a tight per-test limit
// (e.g. 1MB) work despite a multi-MB scheduler baseline.

const (
	memLimitGlobalUsed     = "__promise_memory_used_bytes"
	memLimitGlobalLimit    = "__promise_memory_limit_bytes"
	memLimitGlobalStart    = "__promise_memory_start_bytes"
	memLimitGlobalAborting = "__promise_memory_aborting"
)

func getOrCreateMemoryLimitGlobals(module *ir.Module) (used, limit, start, aborting *ir.Global) {
	for _, g := range module.Globals {
		switch g.Name() {
		case memLimitGlobalUsed:
			used = g
		case memLimitGlobalLimit:
			limit = g
		case memLimitGlobalStart:
			start = g
		case memLimitGlobalAborting:
			aborting = g
		}
	}
	if used == nil {
		used = module.NewGlobal(memLimitGlobalUsed, irtypes.I64)
		used.Init = constant.NewInt(irtypes.I64, 0)
	}
	if limit == nil {
		limit = module.NewGlobal(memLimitGlobalLimit, irtypes.I64)
		limit.Init = constant.NewInt(irtypes.I64, 0)
	}
	if start == nil {
		start = module.NewGlobal(memLimitGlobalStart, irtypes.I64)
		start.Init = constant.NewInt(irtypes.I64, 0)
	}
	if aborting == nil {
		aborting = module.NewGlobal(memLimitGlobalAborting, irtypes.I32)
		aborting.Init = constant.NewInt(irtypes.I32, 0)
	}
	return
}

// MemoryLimitConfig parameterises memory-limit accounting per-target.
//
// Atomic: true on multi-threaded targets (POSIX, Windows) so concurrent allocs
// from M threads race correctly. false on single-threaded targets (wasm32-wasi)
// — plain load/store mirrors what the existing __promise_alloc_count tracking
// already does on WASM.
//
// EmitAbort emits the platform-specific fatal-print + exit sequence at the end
// of blk; the block is terminated with `unreachable` after the call. The msg
// is "fatal: memory limit exceeded\n". Exit code 134 (mirrors SIGABRT semantics
// for parity with the stack-overflow path).
type MemoryLimitConfig struct {
	Atomic    bool
	EmitAbort func(module *ir.Module, blk *ir.Block, key, msg string)
}

// EmitMemoryLimitHelpers emits @__promise_memory_set_test_state. Takes an i64
// limit, snapshots the current `used` into `start`, then stores the limit.
//
// atomic=true selects atomic RMW (POSIX, Windows). atomic=false selects plain
// load/store (wasm32-wasi, wasm32-web — both single-threaded).
//
// Only called when memory-limit accounting is enabled; otherwise these symbols
// don't exist and the test runner shouldn't reference them.
func EmitMemoryLimitHelpers(module *ir.Module, atomic bool) {
	used, limit, start, _ := getOrCreateMemoryLimitGlobals(module)
	zero64 := constant.NewInt(irtypes.I64, 0)

	setFn := module.NewFunc("__promise_memory_set_test_state", irtypes.Void,
		ir.NewParam("new_limit", irtypes.I64))
	setFn.FuncAttrs = append(setFn.FuncAttrs, enum.FuncAttrNoUnwind)
	setEntry := setFn.NewBlock(".entry")
	if atomic {
		// snapshot = atomic_load(used); atomic_store(start, snapshot)
		usedVal := setEntry.NewAtomicRMW(enum.AtomicOpAdd, used, zero64, enum.AtomicOrderingMonotonic)
		setEntry.NewAtomicRMW(enum.AtomicOpXChg, start, usedVal, enum.AtomicOrderingMonotonic)
		setEntry.NewAtomicRMW(enum.AtomicOpXChg, limit, setFn.Params[0], enum.AtomicOrderingMonotonic)
	} else {
		usedVal := setEntry.NewLoad(irtypes.I64, used)
		setEntry.NewStore(usedVal, start)
		setEntry.NewStore(setFn.Params[0], limit)
	}
	setEntry.NewRet(nil)
}

// emitMemoryLimitCheck emits, after a size has been added to
// @__promise_memory_used_bytes, the check `(new_used - start) > limit &&
// limit != 0` and, when true, prints "fatal: memory limit exceeded\n" to
// stderr and exits with code 134. Returns the block where control resumes
// (the "no abort" path).
//
// newUsed is the i64 value of `used` AFTER the increment (before + size on
// allocation, before + delta on realloc grow). cfg selects atomics + abort
// path per target.
func emitMemoryLimitCheck(
	module *ir.Module, fn *ir.Func, blk *ir.Block, newUsed value.Value,
	cfg MemoryLimitConfig,
) *ir.Block {
	_, limit, start, aborting := getOrCreateMemoryLimitGlobals(module)
	zero64 := constant.NewInt(irtypes.I64, 0)

	var limitVal, startVal value.Value
	if cfg.Atomic {
		limitVal = blk.NewAtomicRMW(enum.AtomicOpAdd, limit, zero64, enum.AtomicOrderingMonotonic)
		startVal = blk.NewAtomicRMW(enum.AtomicOpAdd, start, zero64, enum.AtomicOrderingMonotonic)
	} else {
		limitVal = blk.NewLoad(irtypes.I64, limit)
		startVal = blk.NewLoad(irtypes.I64, start)
	}

	limitActive := blk.NewICmp(enum.IPredNE, limitVal, zero64)
	delta := blk.NewSub(newUsed, startVal)
	crossed := blk.NewICmp(enum.IPredSGT, delta, limitVal)
	shouldAbort := blk.NewAnd(limitActive, crossed)

	abortCandidateBlk := fn.NewBlock(".memlimit.abort_candidate")
	continueBlk := fn.NewBlock(".memlimit.ok")
	blk.NewCondBr(shouldAbort, abortCandidateBlk, continueBlk)

	if cfg.Atomic {
		// One-shot CAS guard so only the first thread to cross prints + exits.
		// Loser exits with 134 directly without printing.
		cas := abortCandidateBlk.NewCmpXchg(aborting,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1),
			enum.AtomicOrderingMonotonic, enum.AtomicOrderingMonotonic)
		won := abortCandidateBlk.NewExtractValue(cas, 1)

		winnerBlk := fn.NewBlock(".memlimit.winner")
		loserBlk := fn.NewBlock(".memlimit.loser")
		abortCandidateBlk.NewCondBr(won, winnerBlk, loserBlk)

		cfg.EmitAbort(module, winnerBlk, "memory_limit_exceeded",
			"fatal: memory limit exceeded\n")

		exitFn := getOrDeclareFunc(module, "exit", irtypes.Void,
			ir.NewParam("status", irtypes.I32))
		addFuncAttr(exitFn, enum.FuncAttrNoReturn)
		loserBlk.NewCall(exitFn, constant.NewInt(irtypes.I32, 134))
		loserBlk.NewUnreachable()
	} else {
		// Single-threaded: no CAS needed. abortCandidateBlk is the abort path.
		cfg.EmitAbort(module, abortCandidateBlk, "memory_limit_exceeded",
			"fatal: memory limit exceeded\n")
	}

	return continueBlk
}

// emitAbortLibcFactory adapts emitDebugAbortCallLibc to the
// MemoryLimitConfig.EmitAbort signature for POSIX/Windows (write/_write + exit).
// Used by emitLibcAllocDebug/emitLibcReallocDebug to build a per-target abort
// callback for emitMemoryLimitCheck.
func emitAbortLibcFactory(writeName string, writeReturnsI32 bool) func(*ir.Module, *ir.Block, string, string) {
	return func(module *ir.Module, blk *ir.Block, key, msg string) {
		emitDebugAbortCallLibc(module, blk, key, msg, writeName, writeReturnsI32)
	}
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

// Debug allocator header constants (T0365).
//
// Layout — every pal_alloc'd buffer in DebugAllocator builds is laid out as:
//
//	internal_ptr → [ magic_alive : i64 ][ requested_size : i64 ][ user_data … ][ magic_tail : i64 ]
//	                                                            ^
//	                                                            user_ptr returned to caller
//
// Header is 16 bytes (8 + 8). Trailing sentinel is 8 bytes. Internal allocation
// is `requested_size + 24`. malloc returns ≥16-byte aligned, so the user pointer
// at +16 keeps malloc's native alignment.
const (
	debugMagicAlive = 0x4D414C4C4F435F41 // "MALLOC_A" — set on alloc
	debugMagicFreed = 0x4D414C4C4F435F46 // "MALLOC_F" — set on free (turns next free into a detected double-free)
	debugMagicTail  = 0x5441494C5F4D4147 // "TAIL_MAG" — written at user_ptr + size
	debugHeaderSize = 16                 // magic_alive (8) + requested_size (8)
	debugTailSize   = 8                  // magic_tail (8)
	debugSlackSize  = debugHeaderSize + debugTailSize
)

// getOrCreateDebugMsgGlobal returns a private global containing msg with a
// trailing newline, creating it on first use. Symbol name is derived from key.
func getOrCreateDebugMsgGlobal(module *ir.Module, key, msg string) *ir.Global {
	name := "__promise_dbgmsg_" + key
	for _, g := range module.Globals {
		if g.Name() == name {
			return g
		}
	}
	c := constant.NewCharArrayFromString(msg)
	g := module.NewGlobalDef(name, c)
	g.Linkage = enum.LinkagePrivate
	g.Immutable = true
	return g
}

// emitDebugAbortCall emits a tail call sequence that writes msg to stderr and
// exits with code 134 (SIGABRT). blk is terminated with `unreachable`.
//
// The libc `write`/`_write` and `exit` functions are forward-declared via
// getOrDeclareFunc — both POSIX libc and Windows UCRT provide them under the
// names passed in. Used by libc-based debug allocator (POSIX + Windows).
func emitDebugAbortCallLibc(module *ir.Module, blk *ir.Block, key, msg, writeName string, writeReturnsI32 bool) {
	// Declare write / _write — Windows UCRT exports _write returning i32, POSIX
	// libc exports write returning i64. The return value is unused either way.
	var writeRetType irtypes.Type = irtypes.I64
	if writeReturnsI32 {
		writeRetType = irtypes.I32
	}
	// Windows _write takes an i32 byte count; POSIX write takes i64. Encode both.
	var lenParam *ir.Param
	if writeReturnsI32 {
		lenParam = ir.NewParam("count", irtypes.I32)
	} else {
		lenParam = ir.NewParam("count", irtypes.I64)
	}
	writeFn := getOrDeclareFunc(module, writeName, writeRetType,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		lenParam)

	exitFn := getOrDeclareFunc(module, "exit", irtypes.Void,
		ir.NewParam("status", irtypes.I32))
	addFuncAttr(exitFn, enum.FuncAttrNoReturn)

	g := getOrCreateDebugMsgGlobal(module, key, msg)
	msgPtr := blk.NewBitCast(g, irtypes.I8Ptr)
	stderr := constant.NewInt(irtypes.I32, 2)
	if writeReturnsI32 {
		blk.NewCall(writeFn, stderr, msgPtr, constant.NewInt(irtypes.I32, int64(len(msg))))
	} else {
		blk.NewCall(writeFn, stderr, msgPtr, constant.NewInt(irtypes.I64, int64(len(msg))))
	}
	blk.NewCall(exitFn, constant.NewInt(irtypes.I32, 134))
	blk.NewUnreachable()
}

// emitHeaderValidationLibc validates the debug allocator header at userPtr and
// returns (sizeI64, validatedBlk) — control falls through to validatedBlk only
// when the header magic is alive AND the tail sentinel matches.
//
// Double-free detection is checked at TWO header positions: offset 0 AND offset
// 8 (the size-field slot, which the free path overwrites with MAGIC_FREED in
// addition to the magic-alive slot). libc allocators typically use offset 0 of
// the freed user data for free-list pointers, so checking the secondary slot
// makes double-free detection survive that overwrite.
//
// Three abort blocks are emitted for double-free / bad-magic / tail-corruption
// paths, each using emitDebugAbortCallLibc with platform-specific write fn.
func emitHeaderValidationLibc(
	module *ir.Module,
	fn *ir.Func, blk *ir.Block, userPtr value.Value,
	writeName string, writeReturnsI32 bool,
) (sizeVal value.Value, headerPtr value.Value, validatedBlk *ir.Block) {
	negHeader := constant.NewInt(irtypes.I64, -debugHeaderSize)
	hdrPtr := blk.NewGetElementPtr(irtypes.I8, userPtr, negHeader)

	magicSlot := blk.NewBitCast(hdrPtr, irtypes.NewPointer(irtypes.I64))
	magic := blk.NewLoad(irtypes.I64, magicSlot)
	sizeFieldPtr := blk.NewGetElementPtr(irtypes.I64, magicSlot, constant.NewInt(irtypes.I64, 1))
	sizeOrFreedTag := blk.NewLoad(irtypes.I64, sizeFieldPtr)

	freedMagic := constant.NewInt(irtypes.I64, debugMagicFreed)
	aliveMagic := constant.NewInt(irtypes.I64, debugMagicAlive)
	tailMagic := constant.NewInt(irtypes.I64, debugMagicTail)

	aliveBlk := fn.NewBlock(".dbg.header_alive_check")
	checkSecondaryFreedBlk := fn.NewBlock(".dbg.check_secondary_freed")
	freedAbortBlk := fn.NewBlock(".dbg.double_free")
	badMagicAbortBlk := fn.NewBlock(".dbg.bad_magic")
	tailOkBlk := fn.NewBlock(".dbg.tail_check")
	tailAbortBlk := fn.NewBlock(".dbg.tail_corrupt")
	postValidatedBlk := fn.NewBlock(".dbg.validated")

	// Primary check: offset 0 == MAGIC_FREED → double free.
	isFreed := blk.NewICmp(enum.IPredEQ, magic, freedMagic)
	blk.NewCondBr(isFreed, freedAbortBlk, aliveBlk)

	// Offset 0 != MAGIC_FREED. Either MAGIC_ALIVE (live alloc) or anything else
	// (bad pointer OR doubled-free with offset-0 overwritten by libc).
	isAlive := aliveBlk.NewICmp(enum.IPredEQ, magic, aliveMagic)
	aliveBlk.NewCondBr(isAlive, tailOkBlk, checkSecondaryFreedBlk)

	// Secondary check: if offset 0 isn't ALIVE/FREED, the size field at offset 8
	// may still contain MAGIC_FREED — that's a doubled-free where libc clobbered
	// offset 0 with its free-list pointer.
	isSecondaryFreed := checkSecondaryFreedBlk.NewICmp(enum.IPredEQ, sizeOrFreedTag, freedMagic)
	checkSecondaryFreedBlk.NewCondBr(isSecondaryFreed, freedAbortBlk, badMagicAbortBlk)

	// Tail validation uses the live size field.
	tailBytePtr := tailOkBlk.NewGetElementPtr(irtypes.I8, userPtr, sizeOrFreedTag)
	tailSlot := tailOkBlk.NewBitCast(tailBytePtr, irtypes.NewPointer(irtypes.I64))
	tailLoad := tailOkBlk.NewLoad(irtypes.I64, tailSlot)
	tailLoad.Align = 1
	tailOk := tailOkBlk.NewICmp(enum.IPredEQ, tailLoad, tailMagic)
	tailOkBlk.NewCondBr(tailOk, postValidatedBlk, tailAbortBlk)

	emitDebugAbortCallLibc(module, freedAbortBlk, "double_free",
		"fatal: double free\n", writeName, writeReturnsI32)
	emitDebugAbortCallLibc(module, badMagicAbortBlk, "bad_magic",
		"fatal: invalid free (bad header magic)\n", writeName, writeReturnsI32)
	emitDebugAbortCallLibc(module, tailAbortBlk, "tail_corrupt",
		"fatal: heap corruption (tail sentinel mismatch)\n", writeName, writeReturnsI32)

	return sizeOrFreedTag, hdrPtr, postValidatedBlk
}

// emitLibcAllocDebug defines @pal_alloc with the T0365 sentinel-header debug
// allocator. Bumps malloc by 24 bytes (16-byte header + 8-byte tail), writes
// MAGIC_ALIVE + requested_size + MAGIC_TAIL, and scribbles the user region
// with 0xAA so reads of uninitialized memory show a recognizable pattern.
//
// Pairs with emitLibcFreeDebug, which validates the header on free.
//
// When memoryLimitAccounting is true (T0689), additionally atomically adds the
// requested size to @__promise_memory_used_bytes and aborts with
// "fatal: memory limit exceeded\n" if (used - start) > limit. writeName /
// writeReturnsI32 select the libc write fn for the abort path.
func emitLibcAllocDebug(module *ir.Module, memoryLimitAccounting bool, writeName string, writeReturnsI32 bool) *ir.Func {
	mallocSize := ir.NewParam("size", irtypes.I64)
	mallocSize.Attrs = append(mallocSize.Attrs, enum.ParamAttrNoUndef)
	mallocFn := getOrDeclareFunc(module, "malloc", irtypes.I8Ptr, mallocSize)
	mallocFn.ReturnAttrs = append(mallocFn.ReturnAttrs, enum.ReturnAttrNoAlias)
	addFuncAttr(mallocFn, enum.FuncAttrWillReturn)

	memsetFn := getOrDeclareFunc(module, "memset", irtypes.I8Ptr,
		ir.NewParam("dest", irtypes.I8Ptr),
		ir.NewParam("c", irtypes.I32),
		ir.NewParam("n", irtypes.I64))

	allocCount := getOrCreateAllocCountGlobal(module)

	fn := module.NewFunc("pal_alloc", irtypes.I8Ptr,
		ir.NewParam("size", irtypes.I64))
	fn.ReturnAttrs = append(fn.ReturnAttrs, enum.ReturnAttrNoAlias)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock(".entry")

	// total = size + 24 (header + tail)
	totalSize := entry.NewAdd(fn.Params[0], constant.NewInt(irtypes.I64, debugSlackSize))
	internal := entry.NewCall(mallocFn, totalSize)

	nonnull := entry.NewICmp(enum.IPredNE, internal, constant.NewNull(irtypes.I8Ptr))
	headerBlk := fn.NewBlock(".dbg.write_header")
	doneBlk := fn.NewBlock(".done")
	entry.NewCondBr(nonnull, headerBlk, doneBlk)

	// magic_alive at internal + 0
	magicSlot := headerBlk.NewBitCast(internal, irtypes.NewPointer(irtypes.I64))
	headerBlk.NewStore(constant.NewInt(irtypes.I64, debugMagicAlive), magicSlot)

	// requested_size at internal + 8
	sizeSlot := headerBlk.NewGetElementPtr(irtypes.I64, magicSlot, constant.NewInt(irtypes.I64, 1))
	headerBlk.NewStore(fn.Params[0], sizeSlot)

	// user_ptr = internal + 16
	userPtr := headerBlk.NewGetElementPtr(irtypes.I8, internal, constant.NewInt(irtypes.I64, debugHeaderSize))

	// magic_tail at user_ptr + size (unaligned i64 store)
	tailBytePtr := headerBlk.NewGetElementPtr(irtypes.I8, userPtr, fn.Params[0])
	tailSlot := headerBlk.NewBitCast(tailBytePtr, irtypes.NewPointer(irtypes.I64))
	tailStore := headerBlk.NewStore(constant.NewInt(irtypes.I64, debugMagicTail), tailSlot)
	tailStore.Align = 1

	// Scribble user region with 0xAA
	headerBlk.NewCall(memsetFn, userPtr, constant.NewInt(irtypes.I32, 0xAA), fn.Params[0])

	// Bump alloc count
	headerBlk.NewAtomicRMW(enum.AtomicOpAdd, allocCount, constant.NewInt(irtypes.I64, 1), enum.AtomicOrderingMonotonic)

	// T0689: memory limit accounting. Atomically add the requested size to
	// __promise_memory_used_bytes and abort if (used - start) > limit.
	resumeBlk := headerBlk
	if memoryLimitAccounting {
		used, _, _, _ := getOrCreateMemoryLimitGlobals(module)
		before := resumeBlk.NewAtomicRMW(enum.AtomicOpAdd, used, fn.Params[0], enum.AtomicOrderingMonotonic)
		newUsed := resumeBlk.NewAdd(before, fn.Params[0])
		cfg := MemoryLimitConfig{Atomic: true, EmitAbort: emitAbortLibcFactory(writeName, writeReturnsI32)}
		resumeBlk = emitMemoryLimitCheck(module, fn, resumeBlk, newUsed, cfg)
	}
	resumeBlk.NewBr(doneBlk)

	// On null malloc, return null. Otherwise, return user_ptr.
	retPhi := doneBlk.NewPhi(
		&ir.Incoming{X: constant.NewNull(irtypes.I8Ptr), Pred: entry},
		&ir.Incoming{X: userPtr, Pred: resumeBlk},
	)
	doneBlk.NewRet(retPhi)
	return fn
}

// emitLibcFreeDebug defines @pal_free with header + tail validation. Detects
// double-free, free-of-invalid-pointer, and tail-sentinel corruption (T0365).
//
// On valid input: poisons user region with 0xDE, marks header MAGIC_FREED so
// the next free is caught as a double-free, decrements the alloc count, and
// frees the underlying allocation.
//
// writeName: libc write fn ("write" on POSIX, "_write" on Windows UCRT).
// writeReturnsI32: true for Windows (i32-return _write), false for POSIX i64.
//
// When memoryLimitAccounting is true (T0689), atomically subtracts the validated
// requested_size from @__promise_memory_used_bytes so per-test alloc/free churn
// under a tight limit doesn't trip.
func emitLibcFreeDebug(module *ir.Module, writeName string, writeReturnsI32 bool, memoryLimitAccounting bool) *ir.Func {
	freePtr := ir.NewParam("ptr", irtypes.I8Ptr)
	freePtr.Attrs = append(freePtr.Attrs, enum.ParamAttrNoCapture, enum.ParamAttrNoUndef)
	freeFn := getOrDeclareFunc(module, "free", irtypes.Void, freePtr)
	addFuncAttr(freeFn, enum.FuncAttrWillReturn)

	memsetFn := getOrDeclareFunc(module, "memset", irtypes.I8Ptr,
		ir.NewParam("dest", irtypes.I8Ptr),
		ir.NewParam("c", irtypes.I32),
		ir.NewParam("n", irtypes.I64))

	allocCount := getOrCreateAllocCountGlobal(module)

	fn := module.NewFunc("pal_free", irtypes.Void,
		ir.NewParam("ptr", irtypes.I8Ptr))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock(".entry")

	nonnull := entry.NewICmp(enum.IPredNE, fn.Params[0], constant.NewNull(irtypes.I8Ptr))
	checkBlk := fn.NewBlock(".dbg.check")
	doneBlk := fn.NewBlock(".done")
	entry.NewCondBr(nonnull, checkBlk, doneBlk)

	size, hdrPtr, validated := emitHeaderValidationLibc(module, fn, checkBlk, fn.Params[0],
		writeName, writeReturnsI32)

	// Poison user region with 0xDE (using stored requested_size — exact bound).
	validated.NewCall(memsetFn, fn.Params[0], constant.NewInt(irtypes.I32, 0xDE), size)

	// Mark header MAGIC_FREED at BOTH header slots (offset 0 + offset 8). libc
	// commonly overwrites offset 0 with a free-list pointer immediately after
	// free(); the secondary slot at offset 8 is more likely to survive and
	// keep double-free detection working in that case.
	freedSlot := validated.NewBitCast(hdrPtr, irtypes.NewPointer(irtypes.I64))
	validated.NewStore(constant.NewInt(irtypes.I64, debugMagicFreed), freedSlot)
	freedSlot2 := validated.NewGetElementPtr(irtypes.I64, freedSlot, constant.NewInt(irtypes.I64, 1))
	validated.NewStore(constant.NewInt(irtypes.I64, debugMagicFreed), freedSlot2)

	// Decrement count and free internal pointer.
	validated.NewAtomicRMW(enum.AtomicOpSub, allocCount, constant.NewInt(irtypes.I64, 1), enum.AtomicOrderingMonotonic)
	// T0689: memory limit accounting — subtract the requested size from used.
	if memoryLimitAccounting {
		used, _, _, _ := getOrCreateMemoryLimitGlobals(module)
		validated.NewAtomicRMW(enum.AtomicOpSub, used, size, enum.AtomicOrderingMonotonic)
	}
	validated.NewCall(freeFn, hdrPtr)
	validated.NewBr(doneBlk)

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

// emitLibcReallocDebug defines @pal_realloc with header validation and
// re-construction (T0365). On non-null input, the existing header is validated
// (catches double-free / bad pointer / tail corruption); on null input the
// realloc behaves like alloc; on size==0 like free.
//
// The old `requested_size` is read from the validated header — no need for
// libc's malloc_usable_size, and grown bytes are scribbled with 0xAA using
// the exact old size as the lower bound.
//
// When memoryLimitAccounting is true (T0689), adjusts @__promise_memory_used_bytes
// by (new_size - old_size) and checks the limit when growing.
func emitLibcReallocDebug(module *ir.Module, writeName string, writeReturnsI32 bool, memoryLimitAccounting bool) *ir.Func {
	reallocPtr := ir.NewParam("ptr", irtypes.I8Ptr)
	reallocPtr.Attrs = append(reallocPtr.Attrs, enum.ParamAttrNoCapture, enum.ParamAttrNoUndef)
	reallocSz := ir.NewParam("size", irtypes.I64)
	reallocSz.Attrs = append(reallocSz.Attrs, enum.ParamAttrNoUndef)
	reallocFn := getOrDeclareFunc(module, "realloc", irtypes.I8Ptr, reallocPtr, reallocSz)
	reallocFn.ReturnAttrs = append(reallocFn.ReturnAttrs, enum.ReturnAttrNoAlias)
	addFuncAttr(reallocFn, enum.FuncAttrWillReturn)

	memsetFn := getOrDeclareFunc(module, "memset", irtypes.I8Ptr,
		ir.NewParam("dest", irtypes.I8Ptr),
		ir.NewParam("c", irtypes.I32),
		ir.NewParam("n", irtypes.I64))

	// Look up sibling pal_alloc / pal_free to delegate the malloc-like and
	// free-like cases. Both must be emitted before pal_realloc (per the PAL
	// emission order in compiler.go declareIntrinsics — alloc → free → realloc).
	palAllocFn := lookupFunc(module, "pal_alloc")
	palFreeFn := lookupFunc(module, "pal_free")

	zero64 := constant.NewInt(irtypes.I64, 0)
	nullPtr := constant.NewNull(irtypes.I8Ptr)

	fn := module.NewFunc("pal_realloc", irtypes.I8Ptr,
		ir.NewParam("ptr", irtypes.I8Ptr),
		ir.NewParam("size", irtypes.I64))
	fn.ReturnAttrs = append(fn.ReturnAttrs, enum.ReturnAttrNoAlias)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind, enum.FuncAttrWillReturn)
	entry := fn.NewBlock(".entry")

	ptrIsNull := entry.NewICmp(enum.IPredEQ, fn.Params[0], nullPtr)
	allocLikeBlk := fn.NewBlock(".dbg.alloc_like") // ptr == null → pal_alloc(size)
	checkFreeLikeBlk := fn.NewBlock(".dbg.check_free_like")
	entry.NewCondBr(ptrIsNull, allocLikeBlk, checkFreeLikeBlk)

	// Case 1: realloc(NULL, n) → pal_alloc(n) (handles header construction).
	allocResult := allocLikeBlk.NewCall(palAllocFn, fn.Params[1])
	doneBlk := fn.NewBlock(".done")
	allocLikeBlk.NewBr(doneBlk)

	// Case 2: realloc(p, 0) → pal_free(p), return NULL (mirrors libc realloc semantics).
	sizeIsZero := checkFreeLikeBlk.NewICmp(enum.IPredEQ, fn.Params[1], zero64)
	freeLikeBlk := fn.NewBlock(".dbg.free_like")
	resizeBlk := fn.NewBlock(".dbg.resize")
	checkFreeLikeBlk.NewCondBr(sizeIsZero, freeLikeBlk, resizeBlk)
	freeLikeBlk.NewCall(palFreeFn, fn.Params[0])
	freeLikeBlk.NewBr(doneBlk)

	// Case 3: resize. Validate the old header, then realloc(header_ptr, n + 24)
	// and reconstruct header + tail.
	oldSize, hdrPtr, validatedBlk := emitHeaderValidationLibc(module, fn, resizeBlk, fn.Params[0],
		writeName, writeReturnsI32)

	newTotal := validatedBlk.NewAdd(fn.Params[1], constant.NewInt(irtypes.I64, debugSlackSize))
	newInternal := validatedBlk.NewCall(reallocFn, hdrPtr, newTotal)

	newNonNull := validatedBlk.NewICmp(enum.IPredNE, newInternal, nullPtr)
	resizeOkBlk := fn.NewBlock(".dbg.resize_ok")
	validatedBlk.NewCondBr(newNonNull, resizeOkBlk, doneBlk)

	// Update header: magic_alive (already alive — but realloc may have moved
	// memory; the header is still intact since realloc preserves contents).
	// Update requested_size to the new size. Write fresh tail at user_ptr + new_size.
	newMagicSlot := resizeOkBlk.NewBitCast(newInternal, irtypes.NewPointer(irtypes.I64))
	newSizeSlot := resizeOkBlk.NewGetElementPtr(irtypes.I64, newMagicSlot, constant.NewInt(irtypes.I64, 1))
	resizeOkBlk.NewStore(fn.Params[1], newSizeSlot)

	newUserPtr := resizeOkBlk.NewGetElementPtr(irtypes.I8, newInternal, constant.NewInt(irtypes.I64, debugHeaderSize))
	newTailBytePtr := resizeOkBlk.NewGetElementPtr(irtypes.I8, newUserPtr, fn.Params[1])
	newTailSlot := resizeOkBlk.NewBitCast(newTailBytePtr, irtypes.NewPointer(irtypes.I64))
	newTailStore := resizeOkBlk.NewStore(constant.NewInt(irtypes.I64, debugMagicTail), newTailSlot)
	newTailStore.Align = 1

	// T0689: adjust the memory-limit counter by (new - old) and abort if the
	// post-resize usage crosses the limit. Signed delta: fetch_add accepts
	// negative values. Only check the limit when growing — shrinking can't trip.
	postAccBlk := resizeOkBlk
	if memoryLimitAccounting {
		used, _, _, _ := getOrCreateMemoryLimitGlobals(module)
		delta := postAccBlk.NewSub(fn.Params[1], oldSize)
		before := postAccBlk.NewAtomicRMW(enum.AtomicOpAdd, used, delta, enum.AtomicOrderingMonotonic)
		newUsed := postAccBlk.NewAdd(before, delta)
		grew := postAccBlk.NewICmp(enum.IPredSGT, delta, constant.NewInt(irtypes.I64, 0))
		checkBlk := fn.NewBlock(".dbg.resize_memlimit_check")
		skipCheckBlk := fn.NewBlock(".dbg.resize_memlimit_skip")
		postAccBlk.NewCondBr(grew, checkBlk, skipCheckBlk)
		cfg := MemoryLimitConfig{Atomic: true, EmitAbort: emitAbortLibcFactory(writeName, writeReturnsI32)}
		afterCheck := emitMemoryLimitCheck(module, fn, checkBlk, newUsed, cfg)
		afterCheck.NewBr(skipCheckBlk)
		postAccBlk = skipCheckBlk
	}

	// If new size > old size, scribble the grown region [user+old, user+new) with 0xAA.
	hasGrown := postAccBlk.NewICmp(enum.IPredUGT, fn.Params[1], oldSize)
	scribbleBlk := fn.NewBlock(".dbg.scribble_grown")
	postAccBlk.NewCondBr(hasGrown, scribbleBlk, doneBlk)

	scribbleStart := scribbleBlk.NewGetElementPtr(irtypes.I8, newUserPtr, oldSize)
	scribbleLen := scribbleBlk.NewSub(fn.Params[1], oldSize)
	scribbleBlk.NewCall(memsetFn, scribbleStart, constant.NewInt(irtypes.I32, 0xAA), scribbleLen)
	scribbleBlk.NewBr(doneBlk)

	// Phi the result: alloc-result, null (for free-like), null (for resize-fail), or new_user_ptr.
	retPhi := doneBlk.NewPhi(
		&ir.Incoming{X: allocResult, Pred: allocLikeBlk},
		&ir.Incoming{X: nullPtr, Pred: freeLikeBlk},
		&ir.Incoming{X: nullPtr, Pred: validatedBlk},
		&ir.Incoming{X: newUserPtr, Pred: postAccBlk},
		&ir.Incoming{X: newUserPtr, Pred: scribbleBlk},
	)
	doneBlk.NewRet(retPhi)
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

// emitStubPipeRead returns -1 (no subprocesses on WASM).
func emitStubPipeRead(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_pipe_read", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I64, -1))
	return fn
}

// emitStubPipeWrite returns -1 (no subprocesses on WASM).
func emitStubPipeWrite(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_pipe_write", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I64, -1))
	return fn
}

// emitStubPipeClose returns -1 (no subprocesses on WASM).
func emitStubPipeClose(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_pipe_close", irtypes.I32,
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

// emitStubExecReplace returns -1 (no process model on WASM targets, T0770).
func emitStubExecReplace(module *ir.Module) *ir.Func {
	i8PtrPtrType := irtypes.NewPointer(irtypes.I8Ptr)
	fn := module.NewFunc("pal_exec_replace", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr),
		ir.NewParam("argv", i8PtrPtrType))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
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

// --- Stub file metadata implementation (D0012) ---

// emitStubFileStat returns -1 (no stat support).
func emitStubFileStat(module *ir.Module) *ir.Func {
	fn := module.NewFunc("pal_file_stat", irtypes.I32,
		ir.NewParam("path", irtypes.I8Ptr),
		ir.NewParam("out", irtypes.NewPointer(irtypes.I64)),
		ir.NewParam("follow", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewRet(constant.NewInt(irtypes.I32, -1))
	return fn
}
