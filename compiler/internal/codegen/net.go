package codegen

import (
	"github.com/llir/llvm/ir"

	"djabi.dev/go/promise_lang/internal/codegen/pal"
)

// defineNetPALBodies adds LLVM IR function bodies to networking extern declarations
// from modules/net/net.pr. Each body bridges Promise types to raw PAL socket wrappers.
//
// Socket PAL functions are emitted lazily here (not in emitPALFunctions) to avoid
// libc name collisions — functions like connect, shutdown, send, recv, bind, listen,
// accept are common user-facing names in Promise programs that don't use networking.
//
// Must run after compileModules() so that net module externs are declared in c.module.Funcs.
// Currently a no-op — the bridge activates once modules/net/net.pr declares matching
// native extern functions (T0069).
func (c *Compiler) defineNetPALBodies() {
	// Build lookup by LLVM function name for declarations without bodies
	irFuncByName := make(map[string]*ir.Func)
	for _, fn := range c.module.Funcs {
		if len(fn.Blocks) == 0 {
			irFuncByName[fn.Name()] = fn
		}
	}

	// Check if any net module externs are present — if not, skip PAL emission entirely.
	// This list will grow as modules/net/net.pr adds native extern declarations.
	netExterns := []string{
		"promise_net_socket_create",
		"promise_net_socket_bind",
		"promise_net_socket_listen",
		"promise_net_socket_accept",
		"promise_net_socket_connect",
		"promise_net_socket_send",
		"promise_net_socket_recv",
		"promise_net_socket_close",
		"promise_net_socket_setopt",
		"promise_net_socket_shutdown",
		"promise_net_socket_set_nonblock",
		"promise_net_socket_get_error",
		"promise_net_getaddrinfo",
		"promise_net_freeaddrinfo",
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
	c.palGetAddrInfo = p.EmitGetAddrInfo(c.module)
	c.palFreeAddrInfo = p.EmitFreeAddrInfo(c.module)

	// Lazily emit reactor PAL functions (T0070)
	c.palReactorCreate = p.EmitReactorCreate(c.module)
	c.palReactorAdd = p.EmitReactorAdd(c.module)
	c.palReactorRemove = p.EmitReactorRemove(c.module)
	c.palReactorPoll = p.EmitReactorPoll(c.module)
	c.palReactorClose = p.EmitReactorClose(c.module)

	// Emit reactor infrastructure (T0070)
	c.defineNetpollFuncs()

	// Bridge net module externs to PAL socket functions.
	// These will be populated as modules/net/net.pr adds native extern declarations.
}
