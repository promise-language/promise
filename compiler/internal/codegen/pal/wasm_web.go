package pal

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
)

// WasmWebPAL implements PAL for WebAssembly targeting browser environments.
// Imports from "promise_env" (JS glue module) instead of "wasi_snapshot_preview1".
type WasmWebPAL struct {
	DebugAllocator        bool // scribble malloc'd (0xAA) + poison freed (0xDE) memory for UAF / uninit-read detection
	MemoryLimitAccounting bool // T0689: enable @__promise_memory_used_bytes accounting + memory-limit abort
}

// EmitWrite declares a JS-provided write function and defines @pal_write.
// The JS glue provides promise_env.write(fd, buf, len) → i64.
// Signature: @pal_write(i32 %fd, i8* %buf, i64 %len) → i64
func (p *WasmWebPAL) EmitWrite(module *ir.Module) *ir.Func {
	// declare i64 @promise_env_write(i32, i8*, i64)
	envWrite := getOrDeclareFunc(module, "promise_env_write", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	envWrite.FuncAttrs = append(envWrite.FuncAttrs,
		ir.AttrPair{Key: "wasm-import-module", Value: "promise_env"},
		ir.AttrPair{Key: "wasm-import-name", Value: "write"})

	// define i64 @pal_write(i32 %fd, i8* %buf, i64 %len)
	fn := module.NewFunc("pal_write", irtypes.I64,
		ir.NewParam("fd", irtypes.I32),
		ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	entry := fn.NewBlock(".entry")
	ret := entry.NewCall(envWrite, fn.Params[0], fn.Params[1], fn.Params[2])
	entry.NewRet(ret)

	return fn
}

// EmitExit declares a JS-provided exit function and defines @pal_exit.
// Signature: @pal_exit(i32 %code) → void [noreturn]
func (p *WasmWebPAL) EmitExit(module *ir.Module) *ir.Func {
	// declare void @promise_env_exit(i32) noreturn
	envExit := getOrDeclareFunc(module, "promise_env_exit", irtypes.Void,
		ir.NewParam("code", irtypes.I32))
	addFuncAttr(envExit, enum.FuncAttrNoReturn)
	envExit.FuncAttrs = append(envExit.FuncAttrs,
		ir.AttrPair{Key: "wasm-import-module", Value: "promise_env"},
		ir.AttrPair{Key: "wasm-import-name", Value: "exit"})

	// define void @pal_exit(i32 %code) noreturn
	fn := module.NewFunc("pal_exit", irtypes.Void,
		ir.NewParam("code", irtypes.I32))
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoReturn, enum.FuncAttrNoUnwind)
	entry := fn.NewBlock(".entry")
	entry.NewCall(envExit, fn.Params[0])
	entry.NewUnreachable()

	return fn
}

// Alloc/Free/Realloc — same as WasmPAL (linked from wasm_alloc.o), but with
// WebTarget=true so the debug allocator's fatal-abort path (double-free,
// bad-magic, tail-corrupt, T0689 memory limit) emits promise_env.write +
// .exit imports instead of WASI fd_write + proc_exit.
func (p *WasmWebPAL) EmitAlloc(module *ir.Module) *ir.Func {
	return (&WasmPAL{DebugAllocator: p.DebugAllocator, MemoryLimitAccounting: p.MemoryLimitAccounting, WebTarget: true}).EmitAlloc(module)
}
func (p *WasmWebPAL) EmitFree(module *ir.Module) *ir.Func {
	return (&WasmPAL{DebugAllocator: p.DebugAllocator, MemoryLimitAccounting: p.MemoryLimitAccounting, WebTarget: true}).EmitFree(module)
}
func (p *WasmWebPAL) EmitRealloc(module *ir.Module) *ir.Func {
	return (&WasmPAL{DebugAllocator: p.DebugAllocator, MemoryLimitAccounting: p.MemoryLimitAccounting, WebTarget: true}).EmitRealloc(module)
}

// Stubs — same as WasmPAL (no file I/O, threading, etc. in browser).
func (p *WasmWebPAL) EmitFileOpen(module *ir.Module) *ir.Func  { return emitStubFileOpen(module) }
func (p *WasmWebPAL) EmitFileRead(module *ir.Module) *ir.Func  { return emitStubFileRead(module) }
func (p *WasmWebPAL) EmitFileWrite(module *ir.Module) *ir.Func { return emitStubFileWrite(module) }
func (p *WasmWebPAL) EmitFileClose(module *ir.Module) *ir.Func { return emitStubFileClose(module) }
func (p *WasmWebPAL) EmitFileSeek(module *ir.Module) *ir.Func  { return emitStubFileSeek(module) }
func (p *WasmWebPAL) EmitFileStatSize(module *ir.Module) *ir.Func {
	return emitStubFileStatSize(module)
}
func (p *WasmWebPAL) EmitFileStat(module *ir.Module) *ir.Func    { return emitStubFileStat(module) }
func (p *WasmWebPAL) EmitFileRemove(module *ir.Module) *ir.Func  { return emitStubFileRemove(module) }
func (p *WasmWebPAL) EmitFileExists(module *ir.Module) *ir.Func  { return emitStubFileExists(module) }
func (p *WasmWebPAL) EmitFileMkdir(module *ir.Module) *ir.Func   { return emitStubFileMkdir(module) }
func (p *WasmWebPAL) EmitDirRemove(module *ir.Module) *ir.Func   { return emitStubDirRemove(module) }
func (p *WasmWebPAL) EmitDirExists(module *ir.Module) *ir.Func   { return emitStubDirExists(module) }
func (p *WasmWebPAL) EmitErrno(module *ir.Module) *ir.Func       { return emitStubErrno(module) }
func (p *WasmWebPAL) EmitDirOpen(module *ir.Module) *ir.Func     { return emitStubDirOpen(module) }
func (p *WasmWebPAL) EmitDirNextName(module *ir.Module) *ir.Func { return emitStubDirNextName(module) }
func (p *WasmWebPAL) EmitDirClose(module *ir.Module) *ir.Func    { return emitStubDirClose(module) }
func (p *WasmWebPAL) EmitGetEnv(module *ir.Module) *ir.Func      { return emitStubGetEnv(module) }
func (p *WasmWebPAL) EmitGetCwd(module *ir.Module) *ir.Func      { return emitStubGetCwd(module) }
func (p *WasmWebPAL) EmitSetEnv(module *ir.Module) *ir.Func      { return emitStubSetEnv(module) }
func (p *WasmWebPAL) EmitUnsetEnv(module *ir.Module) *ir.Func    { return emitStubUnsetEnv(module) }
func (p *WasmWebPAL) EmitChdir(module *ir.Module) *ir.Func       { return emitStubChdir(module) }
func (p *WasmWebPAL) EmitSpawn(module *ir.Module) *ir.Func       { return emitStubSpawn(module) }
func (p *WasmWebPAL) EmitReadPipe(module *ir.Module) *ir.Func    { return emitStubReadPipe(module) }
func (p *WasmWebPAL) EmitWaitPid(module *ir.Module) *ir.Func     { return emitStubWaitPid(module) }
func (p *WasmWebPAL) EmitSpawnStreaming(module *ir.Module) *ir.Func {
	return emitStubSpawnStreaming(module)
}
func (p *WasmWebPAL) EmitSpawnEnv(module *ir.Module) *ir.Func {
	return emitStubSpawnEnv(module)
}
func (p *WasmWebPAL) EmitSpawnStreamingEnv(module *ir.Module) *ir.Func {
	return emitStubSpawnStreamingEnv(module)
}
func (p *WasmWebPAL) EmitKill(module *ir.Module) *ir.Func        { return emitStubKill(module) }
func (p *WasmWebPAL) EmitExecReplace(module *ir.Module) *ir.Func { return emitStubExecReplace(module) }
func (p *WasmWebPAL) EmitGetEnviron(module *ir.Module) *ir.Func  { return emitStubGetEnviron(module) }
func (p *WasmWebPAL) EmitGetUserInfo(module *ir.Module) *ir.Func { return emitStubGetUserInfo(module) }
func (p *WasmWebPAL) EmitGetHostname(module *ir.Module) *ir.Func { return emitStubGetHostname(module) }
func (p *WasmWebPAL) EmitSignalInit(module *ir.Module) *ir.Func  { return emitStubSignalInit(module) }
func (p *WasmWebPAL) EmitSignalRegister(module *ir.Module) *ir.Func {
	return emitStubSignalRegister(module)
}
func (p *WasmWebPAL) EmitStackOverflowInit(module *ir.Module) *ir.Func {
	return emitStubStackOverflowInit(module)
}
func (p *WasmWebPAL) EmitStackOverflowThreadInit(module *ir.Module) *ir.Func {
	return emitStubStackOverflowThreadInit(module)
}
func (p *WasmWebPAL) EmitThreadCreate(module *ir.Module) *ir.Func {
	return emitStubThreadCreate(module)
}
func (p *WasmWebPAL) EmitThreadJoin(module *ir.Module) *ir.Func  { return emitStubThreadJoin(module) }
func (p *WasmWebPAL) EmitMutexInit(module *ir.Module) *ir.Func   { return emitStubMutexInit(module) }
func (p *WasmWebPAL) EmitMutexLock(module *ir.Module) *ir.Func   { return emitStubMutexLock(module) }
func (p *WasmWebPAL) EmitMutexUnlock(module *ir.Module) *ir.Func { return emitStubMutexUnlock(module) }
func (p *WasmWebPAL) EmitMutexDestroy(module *ir.Module) *ir.Func {
	return emitStubMutexDestroy(module)
}
func (p *WasmWebPAL) EmitCondInit(module *ir.Module) *ir.Func   { return emitStubCondInit(module) }
func (p *WasmWebPAL) EmitCondWait(module *ir.Module) *ir.Func   { return emitStubCondWait(module) }
func (p *WasmWebPAL) EmitCondSignal(module *ir.Module) *ir.Func { return emitStubCondSignal(module) }
func (p *WasmWebPAL) EmitCondBroadcast(module *ir.Module) *ir.Func {
	return emitStubCondBroadcast(module)
}
func (p *WasmWebPAL) EmitCondDestroy(module *ir.Module) *ir.Func { return emitStubCondDestroy(module) }
func (p *WasmWebPAL) EmitNumCPUs(module *ir.Module) *ir.Func     { return emitStubNumCPUs(module) }

// WasmWeb socket stubs — no networking support (T0069).
func (p *WasmWebPAL) EmitSocketCreate(module *ir.Module) *ir.Func {
	return emitStubSocketCreate(module)
}
func (p *WasmWebPAL) EmitSocketBind(module *ir.Module) *ir.Func { return emitStubSocketBind(module) }
func (p *WasmWebPAL) EmitSocketListen(module *ir.Module) *ir.Func {
	return emitStubSocketListen(module)
}
func (p *WasmWebPAL) EmitSocketAccept(module *ir.Module) *ir.Func {
	return emitStubSocketAccept(module)
}
func (p *WasmWebPAL) EmitSocketConnect(module *ir.Module) *ir.Func {
	return emitStubSocketConnect(module)
}
func (p *WasmWebPAL) EmitSocketSend(module *ir.Module) *ir.Func  { return emitStubSocketSend(module) }
func (p *WasmWebPAL) EmitSocketRecv(module *ir.Module) *ir.Func  { return emitStubSocketRecv(module) }
func (p *WasmWebPAL) EmitSocketClose(module *ir.Module) *ir.Func { return emitStubSocketClose(module) }
func (p *WasmWebPAL) EmitSocketSetOpt(module *ir.Module) *ir.Func {
	return emitStubSocketSetOpt(module)
}
func (p *WasmWebPAL) EmitSocketShutdown(module *ir.Module) *ir.Func {
	return emitStubSocketShutdown(module)
}
func (p *WasmWebPAL) EmitSocketSetNonBlock(module *ir.Module) *ir.Func {
	return emitStubSocketSetNonBlock(module)
}
func (p *WasmWebPAL) EmitSocketGetError(module *ir.Module) *ir.Func {
	return emitStubSocketGetError(module)
}
func (p *WasmWebPAL) EmitGetAddrInfo(module *ir.Module) *ir.Func { return emitStubGetAddrInfo(module) }
func (p *WasmWebPAL) EmitFreeAddrInfo(module *ir.Module) *ir.Func {
	return emitStubFreeAddrInfo(module)
}

// WasmWeb reactor stubs — no reactor support (T0070).
func (p *WasmWebPAL) EmitReactorCreate(module *ir.Module) *ir.Func {
	return emitStubReactorCreate(module)
}
func (p *WasmWebPAL) EmitReactorAdd(module *ir.Module) *ir.Func { return emitStubReactorAdd(module) }
func (p *WasmWebPAL) EmitReactorRemove(module *ir.Module) *ir.Func {
	return emitStubReactorRemove(module)
}
func (p *WasmWebPAL) EmitReactorPoll(module *ir.Module) *ir.Func { return emitStubReactorPoll(module) }
func (p *WasmWebPAL) EmitReactorClose(module *ir.Module) *ir.Func {
	return emitStubReactorClose(module)
}

// WasmWeb high-level socket address stubs — no networking (T0071).
func (p *WasmWebPAL) EmitSocketBindAddr(module *ir.Module) *ir.Func {
	return emitStubSocketBindAddr(module)
}
func (p *WasmWebPAL) EmitSocketConnectAddr(module *ir.Module) *ir.Func {
	return emitStubSocketConnectAddr(module)
}
func (p *WasmWebPAL) EmitSocketAcceptAddr(module *ir.Module) *ir.Func {
	return emitStubSocketAcceptAddr(module)
}
func (p *WasmWebPAL) EmitSocketGetLocalPort(module *ir.Module) *ir.Func {
	return emitStubSocketGetLocalPort(module)
}
