package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/promise-language/promise/compiler/internal/codegen/pal"
)

// defineNetPALBodies adds LLVM IR function bodies to networking extern declarations
// from modules/net/net.pr. Each body bridges Promise types to raw PAL socket wrappers.
//
// Socket PAL functions are emitted lazily here (not in emitPALFunctions) to avoid
// libc name collisions — functions like connect, shutdown, send, recv, bind, listen,
// accept are common user-facing names in Promise programs that don't use networking.
//
// Must run after compileModules() so that net module externs are declared in c.module.Funcs.
func (c *Compiler) defineNetPALBodies() {
	// Build lookup by LLVM function name for declarations without bodies
	irFuncByName := make(map[string]*ir.Func)
	for _, fn := range c.module.Funcs {
		if len(fn.Blocks) == 0 {
			irFuncByName[fn.Name()] = fn
		}
	}

	// Check if any net module externs are present — if not, skip PAL emission entirely.
	netExterns := []string{
		"promise_net_socket_create",
		"promise_net_socket_bind_addr",
		"promise_net_socket_listen",
		"promise_net_socket_accept_addr",
		"promise_net_socket_connect_addr",
		"promise_net_socket_close",
		"promise_net_socket_send",
		"promise_net_socket_recv",
		"promise_net_socket_shutdown",
		"promise_net_socket_get_error",
		"promise_net_socket_getsockname",
		"promise_net_socket_set_nonblock",
		"promise_net_netpoll_open",
		"promise_net_netpoll_close",
	}

	hasNetExterns := false
	for _, name := range netExterns {
		if _, ok := irFuncByName[name]; ok {
			hasNetExterns = true
			break
		}
	}
	if !hasNetExterns {
		return
	}

	// Lazily emit socket PAL functions only when the net module is imported.
	p := pal.ForTarget(c.module.TargetTriple)
	c.palSocketCreate = p.EmitSocketCreate(c.module)
	c.palSocketBind = p.EmitSocketBind(c.module)
	c.palSocketListen = p.EmitSocketListen(c.module)
	c.palSocketAccept = p.EmitSocketAccept(c.module)
	c.palSocketConnect = p.EmitSocketConnect(c.module)
	c.palSocketSend = p.EmitSocketSend(c.module)
	c.palSocketRecv = p.EmitSocketRecv(c.module)
	c.palSocketClose = p.EmitSocketClose(c.module)
	c.palSocketSetOpt = p.EmitSocketSetOpt(c.module)
	c.palSocketShutdown = p.EmitSocketShutdown(c.module)
	c.palSocketSetNonBlock = p.EmitSocketSetNonBlock(c.module)
	c.palSocketGetError = p.EmitSocketGetError(c.module)
	c.palSocketGetLocalPort = p.EmitSocketGetLocalPort(c.module)
	c.palGetAddrInfo = p.EmitGetAddrInfo(c.module)
	c.palFreeAddrInfo = p.EmitFreeAddrInfo(c.module)

	// Lazily emit high-level socket address PAL functions (T0071)
	c.palSocketBindAddr = p.EmitSocketBindAddr(c.module)
	c.palSocketConnectAddr = p.EmitSocketConnectAddr(c.module)
	c.palSocketAcceptAddr = p.EmitSocketAcceptAddr(c.module)

	// Lazily emit reactor PAL functions (T0070)
	c.palReactorCreate = p.EmitReactorCreate(c.module)
	c.palReactorAdd = p.EmitReactorAdd(c.module)
	c.palReactorRemove = p.EmitReactorRemove(c.module)
	c.palReactorPoll = p.EmitReactorPoll(c.module)
	c.palReactorClose = p.EmitReactorClose(c.module)

	// Emit reactor infrastructure (T0070) — skipped on WASM (no threads/reactor)
	c.defineNetpollFuncs()

	// Set flag so wrapMainWithScheduler/GenerateTestMain emit netpoll_init (T0071)
	if !c.isWasm {
		c.needsNetpoll = true
	}

	// Bridge net module externs to PAL socket functions.
	if fn, ok := irFuncByName["promise_net_socket_create"]; ok {
		c.defineNetSocketCreateBody(fn)
	}
	if fn, ok := irFuncByName["promise_net_socket_bind_addr"]; ok {
		c.defineNetSocketBindAddrBody(fn)
	}
	if fn, ok := irFuncByName["promise_net_socket_listen"]; ok {
		c.defineNetSocketListenBody(fn)
	}
	if fn, ok := irFuncByName["promise_net_socket_accept_addr"]; ok {
		c.defineNetSocketAcceptAddrBody(fn)
	}
	if fn, ok := irFuncByName["promise_net_socket_connect_addr"]; ok {
		c.defineNetSocketConnectAddrBody(fn)
	}
	if fn, ok := irFuncByName["promise_net_socket_close"]; ok {
		c.defineNetSocketCloseBody(fn)
	}
	if fn, ok := irFuncByName["promise_net_socket_send"]; ok {
		c.defineNetSocketSendBody(fn)
	}
	if fn, ok := irFuncByName["promise_net_socket_recv"]; ok {
		c.defineNetSocketRecvBody(fn)
	}
	if fn, ok := irFuncByName["promise_net_socket_shutdown"]; ok {
		c.defineNetSocketShutdownBody(fn)
	}
	if fn, ok := irFuncByName["promise_net_socket_get_error"]; ok {
		c.defineNetSocketGetErrorBody(fn)
	}
	if fn, ok := irFuncByName["promise_net_socket_getsockname"]; ok {
		c.defineNetSocketGetSockNameBody(fn)
	}
	if fn, ok := irFuncByName["promise_net_socket_set_nonblock"]; ok {
		c.defineNetSocketSetNonBlockBody(fn)
	}
	// Netpoll bridge functions require reactor support — skip on WASM.
	// On WASM, these externs remain bodyless declarations (linker DCEs them
	// since the net module methods that call them are stubs returning -ENOSYS).
	if !c.isWasm {
		if fn, ok := irFuncByName["promise_net_netpoll_open"]; ok {
			c.defineNetNetpollOpenBody(fn)
		}
		if fn, ok := irFuncByName["promise_net_netpoll_close"]; ok {
			c.defineNetNetpollCloseBody(fn)
		}
	}
	// promise_netpoll_wait_read/write are intercepted at codegen dispatch (T0232) —
	// no bridge bodies needed.
}

// Socket bridge functions

// defineNetSocketCreateBody: void @promise_net_socket_create(i8* sret, i8* domain, i8* typ, i8* protocol)
func (c *Compiler) defineNetSocketCreateBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	domainRaw := c.extractRawInt(entry, fn.Params[1])
	domainI32 := entry.NewTrunc(domainRaw, irtypes.I32)
	typRaw := c.extractRawInt(entry, fn.Params[2])
	typI32 := entry.NewTrunc(typRaw, irtypes.I32)
	protoRaw := c.extractRawInt(entry, fn.Params[3])
	protoI32 := entry.NewTrunc(protoRaw, irtypes.I32)

	c.emitEnterSyscall(entry)
	fd := entry.NewCall(c.palSocketCreate, domainI32, typI32, protoI32)
	c.emitExitSyscall(entry)
	fdI64 := entry.NewSExt(fd, irtypes.I64)
	c.storeIntResult(entry, sret, fdI64)
	entry.NewRet(nil)
}

// defineNetSocketBindAddrBody: void @promise_net_socket_bind_addr(i8* sret, i8* fd, i8* host, i8* port)
func (c *Compiler) defineNetSocketBindAddrBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	fdRaw := c.extractRawInt(entry, fn.Params[1])
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)
	cstr := c.stringToCStr(entry, fn.Params[2])
	portRaw := c.extractRawInt(entry, fn.Params[3])
	portI32 := entry.NewTrunc(portRaw, irtypes.I32)

	c.emitEnterSyscall(entry)
	rc := entry.NewCall(c.palSocketBindAddr, fdI32, cstr, portI32)
	c.emitExitSyscall(entry)
	entry.NewCall(c.palFree, cstr)
	rcI64 := entry.NewSExt(rc, irtypes.I64)
	c.storeIntResult(entry, sret, rcI64)
	entry.NewRet(nil)
}

// defineNetSocketListenBody: void @promise_net_socket_listen(i8* sret, i8* fd, i8* backlog)
func (c *Compiler) defineNetSocketListenBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	fdRaw := c.extractRawInt(entry, fn.Params[1])
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)
	backlogRaw := c.extractRawInt(entry, fn.Params[2])
	backlogI32 := entry.NewTrunc(backlogRaw, irtypes.I32)

	c.emitEnterSyscall(entry)
	rc := entry.NewCall(c.palSocketListen, fdI32, backlogI32)
	c.emitExitSyscall(entry)
	rcI64 := entry.NewSExt(rc, irtypes.I64)
	c.storeIntResult(entry, sret, rcI64)
	entry.NewRet(nil)
}

// defineNetSocketAcceptAddrBody: void @promise_net_socket_accept_addr(i8* sret, i8* fd)
func (c *Compiler) defineNetSocketAcceptAddrBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	fdRaw := c.extractRawInt(entry, fn.Params[1])
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)

	c.emitEnterSyscall(entry)
	fd := entry.NewCall(c.palSocketAcceptAddr, fdI32)
	c.emitExitSyscall(entry)
	fdI64 := entry.NewSExt(fd, irtypes.I64)
	c.storeIntResult(entry, sret, fdI64)
	entry.NewRet(nil)
}

// defineNetSocketConnectAddrBody: void @promise_net_socket_connect_addr(i8* sret, i8* fd, i8* host, i8* port)
func (c *Compiler) defineNetSocketConnectAddrBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	fdRaw := c.extractRawInt(entry, fn.Params[1])
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)
	cstr := c.stringToCStr(entry, fn.Params[2])
	portRaw := c.extractRawInt(entry, fn.Params[3])
	portI32 := entry.NewTrunc(portRaw, irtypes.I32)

	c.emitEnterSyscall(entry)
	rc := entry.NewCall(c.palSocketConnectAddr, fdI32, cstr, portI32)
	c.emitExitSyscall(entry)
	entry.NewCall(c.palFree, cstr)
	rcI64 := entry.NewSExt(rc, irtypes.I64)
	c.storeIntResult(entry, sret, rcI64)
	entry.NewRet(nil)
}

// defineNetSocketCloseBody: void @promise_net_socket_close(i8* sret, i8* fd)
func (c *Compiler) defineNetSocketCloseBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	fdRaw := c.extractRawInt(entry, fn.Params[1])
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)

	c.emitEnterSyscall(entry)
	rc := entry.NewCall(c.palSocketClose, fdI32)
	c.emitExitSyscall(entry)
	rcI64 := entry.NewSExt(rc, irtypes.I64)
	c.storeIntResult(entry, sret, rcI64)
	entry.NewRet(nil)
}

// defineNetSocketSendBody: void @promise_net_socket_send(i8* sret, i8* fd, i8* data)
// data is a u8[] vector — extract pointer and length, call pal_socket_send with flags=0.
func (c *Compiler) defineNetSocketSendBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	fdRaw := c.extractRawInt(entry, fn.Params[1])
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)

	// fn.Params[2] is u8[] vector
	dataPtr, dataLen := extractVectorDataLen(entry, fn.Params[2])
	dataPtrI8 := entry.NewBitCast(dataPtr, irtypes.I8Ptr)

	n := entry.NewCall(c.palSocketSend, fdI32, dataPtrI8, dataLen, constant.NewInt(irtypes.I32, 0))
	// pal_socket_send returns i64 (bytes or -errno)
	c.storeIntResult(entry, sret, n)
	entry.NewRet(nil)
}

// defineNetSocketRecvBody: void @promise_net_socket_recv(i8* sret, i8* fd, i8* buf)
// buf is a u8[] vector (~buf) — recv into its data area, return bytes read.
func (c *Compiler) defineNetSocketRecvBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	fdRaw := c.extractRawInt(entry, fn.Params[1])
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)

	// fn.Params[2] is u8[] vector (~buf)
	bufPtr, bufLen := extractVectorDataLen(entry, fn.Params[2])
	bufPtrI8 := entry.NewBitCast(bufPtr, irtypes.I8Ptr)

	n := entry.NewCall(c.palSocketRecv, fdI32, bufPtrI8, bufLen, constant.NewInt(irtypes.I32, 0))
	c.storeIntResult(entry, sret, n)
	entry.NewRet(nil)
}

// defineNetSocketShutdownBody: void @promise_net_socket_shutdown(i8* sret, i8* fd, i8* how)
func (c *Compiler) defineNetSocketShutdownBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	fdRaw := c.extractRawInt(entry, fn.Params[1])
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)
	howRaw := c.extractRawInt(entry, fn.Params[2])
	howI32 := entry.NewTrunc(howRaw, irtypes.I32)

	rc := entry.NewCall(c.palSocketShutdown, fdI32, howI32)
	rcI64 := entry.NewSExt(rc, irtypes.I64)
	c.storeIntResult(entry, sret, rcI64)
	entry.NewRet(nil)
}

// defineNetSocketGetErrorBody: void @promise_net_socket_get_error(i8* sret, i8* fd)
func (c *Compiler) defineNetSocketGetErrorBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	fdRaw := c.extractRawInt(entry, fn.Params[1])
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)

	errno := entry.NewCall(c.palSocketGetError, fdI32)
	errnoI64 := entry.NewSExt(errno, irtypes.I64)
	c.storeIntResult(entry, sret, errnoI64)
	entry.NewRet(nil)
}

// defineNetSocketGetSockNameBody: void @promise_net_socket_getsockname(i8* sret, i8* fd)
// Calls pal_socket_get_local_port(fd) → i32 (port or -errno), returns as Promise int.
func (c *Compiler) defineNetSocketGetSockNameBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	fdRaw := c.extractRawInt(entry, fn.Params[1])
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)

	c.emitEnterSyscall(entry)
	port := entry.NewCall(c.palSocketGetLocalPort, fdI32)
	c.emitExitSyscall(entry)
	portI64 := entry.NewSExt(port, irtypes.I64)
	c.storeIntResult(entry, sret, portI64)
	entry.NewRet(nil)
}

// defineNetSocketSetNonBlockBody: void @promise_net_socket_set_nonblock(i8* sret, i8* fd)
// Sets fd non-blocking via fcntl. Returns 0 on success or -errno on failure.
// Called explicitly before netpoll_open so the socket is in the right state
// (e.g. "connecting" after connect()) when registered with epoll, ensuring
// EPOLLET fires the correct edge transitions (B0322).
func (c *Compiler) defineNetSocketSetNonBlockBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	fdRaw := c.extractRawInt(entry, fn.Params[1])
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)

	rc := entry.NewCall(c.palSocketSetNonBlock, fdI32)
	rcI64 := entry.NewSExt(rc, irtypes.I64)
	c.storeIntResult(entry, sret, rcI64)
	entry.NewRet(nil)
}

// Reactor bridge functions

// defineNetNetpollOpenBody: void @promise_net_netpoll_open(i8* sret, i8* fd)
// Calls promise_netpoll_open(i32 fd) → i8* (PollDesc*), returns as Promise int.
func (c *Compiler) defineNetNetpollOpenBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")
	sret := fn.Params[0]

	fdRaw := c.extractRawInt(entry, fn.Params[1])
	fdI32 := entry.NewTrunc(fdRaw, irtypes.I32)

	// Call promise_netpoll_open(i32 fd) → i8*
	pd := entry.NewCall(c.funcs["promise_netpoll_open"], fdI32)
	// Convert i8* → i64 for storage as Promise int
	pdI64 := entry.NewPtrToInt(pd, irtypes.I64)
	c.storeIntResult(entry, sret, pdI64)
	entry.NewRet(nil)
}

// defineNetNetpollCloseBody: void @promise_net_netpoll_close(i8* pd)
// Extracts PollDesc pointer from Promise int, calls promise_netpoll_close(i8*).
func (c *Compiler) defineNetNetpollCloseBody(fn *ir.Func) {
	entry := fn.NewBlock(".entry")

	// fn.Params[0] is the Promise int value (pd as pointer-as-int)
	pdRaw := c.extractRawInt(entry, fn.Params[0])
	pdPtr := entry.NewIntToPtr(pdRaw, irtypes.I8Ptr)

	entry.NewCall(c.funcs["promise_netpoll_close"], pdPtr)
	entry.NewRet(nil)
}
