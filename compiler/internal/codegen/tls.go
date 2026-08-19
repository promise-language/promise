package codegen

import (
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/codegen/pal"
)

// tls.go — bridge modules/tls/tls.pr externs to the OpenSSL memory-BIO PAL
// backend (T0077). Mirrors defineNetPALBodies: each promise_tls_* extern gets a
// thin body that unpacks the Promise value-struct ABI and calls the raw
// pal_tls_* wrapper emitted by pal.PosixPAL.EmitTLS.
//
// The PAL calls operate purely on OpenSSL's in-memory BIOs — they never touch a
// socket or block — so no syscall enter/exit accounting is needed here. All
// socket I/O and reactor parking stay in Promise (tls.pr drives the ciphertext
// pump over net.TcpStream, which already parks on the reactor).
//
// Must run after compileModules() so tls module externs are declared in
// c.module.Funcs.

// tlsExternNames lists every promise_tls_* bridge symbol the tls module declares.
// The list drives both presence detection (does this program use tls?) and the
// non-Linux stub path.
var tlsExternNames = []string{
	"promise_tls_ctx_new_client",
	"promise_tls_ctx_new_server",
	"promise_tls_ctx_free",
	"promise_tls_ctx_set_verify",
	"promise_tls_ctx_set_min_version",
	"promise_tls_ctx_add_ca",
	"promise_tls_ctx_use_cert",
	"promise_tls_ctx_use_key",
	"promise_tls_ctx_load_default_trust",
	"promise_tls_new",
	"promise_tls_set_connect_state",
	"promise_tls_set_accept_state",
	"promise_tls_set_sni",
	"promise_tls_set_verify_host",
	"promise_tls_do_handshake",
	"promise_tls_read",
	"promise_tls_write",
	"promise_tls_shutdown",
	"promise_tls_bio_read_out",
	"promise_tls_bio_write_in",
	"promise_tls_bio_pending_out",
	"promise_tls_get_version",
	"promise_tls_get_cipher",
	"promise_tls_get_verify_result",
	"promise_tls_free",
}

func (c *Compiler) defineTLSPALBodies() {
	irFuncByName := make(map[string]*ir.Func)
	for _, fn := range c.module.Funcs {
		if len(fn.Blocks) == 0 {
			irFuncByName[fn.Name()] = fn
		}
	}

	hasTLS := false
	for _, name := range tlsExternNames {
		if _, ok := irFuncByName[name]; ok {
			hasTLS = true
			break
		}
	}
	if !hasTLS {
		return
	}
	// Gate the OpenSSL link/fetch by construction: this flag flips true iff a
	// program imports the tls module (NeedsTLS reads it). See layout.go.
	c.needsTLS = true

	// Only Linux has a vendored OpenSSL backend (T1596). Other targets get inert
	// stubs so the module still compiles and links, failing cleanly at runtime
	// with TlsError(kind: unsupported) — the constructors observe a 0 handle.
	if !strings.Contains(c.module.TargetTriple, "linux") {
		c.defineTLSStubBodies(irFuncByName)
		return
	}

	p, ok := pal.ForTarget(c.module.TargetTriple).(*pal.PosixPAL)
	if !ok {
		c.defineTLSStubBodies(irFuncByName)
		return
	}
	pf := p.EmitTLS(c.module)

	bridge := func(name string, body func(fn *ir.Func)) {
		if fn, ok := irFuncByName[name]; ok {
			body(fn)
		}
	}

	// () int  → handle
	bridge("promise_tls_ctx_new_client", func(fn *ir.Func) { c.tlsHandleFactory(fn, pf["pal_tls_ctx_new_client"]) })
	bridge("promise_tls_ctx_new_server", func(fn *ir.Func) { c.tlsHandleFactory(fn, pf["pal_tls_ctx_new_server"]) })
	bridge("promise_tls_new", func(fn *ir.Func) { c.tlsInt1Arg(fn, pf["pal_tls_new"]) })

	// (int) void
	bridge("promise_tls_ctx_free", func(fn *ir.Func) { c.tlsVoid1IntArg(fn, pf["pal_tls_ctx_free"]) })
	bridge("promise_tls_set_connect_state", func(fn *ir.Func) { c.tlsVoid1IntArg(fn, pf["pal_tls_set_connect_state"]) })
	bridge("promise_tls_set_accept_state", func(fn *ir.Func) { c.tlsVoid1IntArg(fn, pf["pal_tls_set_accept_state"]) })
	bridge("promise_tls_free", func(fn *ir.Func) { c.tlsVoid1IntArg(fn, pf["pal_tls_free"]) })

	// (int) int
	bridge("promise_tls_ctx_load_default_trust", func(fn *ir.Func) { c.tlsInt1Arg(fn, pf["pal_tls_ctx_load_default_trust"]) })
	bridge("promise_tls_do_handshake", func(fn *ir.Func) { c.tlsInt1Arg(fn, pf["pal_tls_do_handshake"]) })
	bridge("promise_tls_shutdown", func(fn *ir.Func) { c.tlsInt1Arg(fn, pf["pal_tls_shutdown"]) })
	bridge("promise_tls_bio_pending_out", func(fn *ir.Func) { c.tlsInt1Arg(fn, pf["pal_tls_bio_pending_out"]) })
	bridge("promise_tls_get_verify_result", func(fn *ir.Func) { c.tlsInt1Arg(fn, pf["pal_tls_get_verify_result"]) })

	// (int, int) void
	bridge("promise_tls_ctx_set_verify", func(fn *ir.Func) { c.tlsVoid2IntArg(fn, pf["pal_tls_ctx_set_verify"]) })

	// (int, int) int
	bridge("promise_tls_ctx_set_min_version", func(fn *ir.Func) { c.tlsInt2IntArg(fn, pf["pal_tls_ctx_set_min_version"]) })

	// (int, u8[]) int  — handle + vector(ptr,len)
	bridge("promise_tls_ctx_add_ca", func(fn *ir.Func) { c.tlsIntVecArg(fn, pf["pal_tls_ctx_add_ca"]) })
	bridge("promise_tls_ctx_use_cert", func(fn *ir.Func) { c.tlsIntVecArg(fn, pf["pal_tls_ctx_use_cert"]) })
	bridge("promise_tls_ctx_use_key", func(fn *ir.Func) { c.tlsIntVecArg(fn, pf["pal_tls_ctx_use_key"]) })
	bridge("promise_tls_read", func(fn *ir.Func) { c.tlsIntVecArg(fn, pf["pal_tls_read"]) })
	bridge("promise_tls_write", func(fn *ir.Func) { c.tlsIntVecArg(fn, pf["pal_tls_write"]) })
	bridge("promise_tls_bio_read_out", func(fn *ir.Func) { c.tlsIntVecArg(fn, pf["pal_tls_bio_read_out"]) })

	// (int, u8[], int) int  — handle + vector(ptr) + explicit length
	bridge("promise_tls_bio_write_in", func(fn *ir.Func) { c.tlsIntVecLenArg(fn, pf["pal_tls_bio_write_in"]) })

	// (int, string) int  — handle + cstr
	bridge("promise_tls_set_sni", func(fn *ir.Func) { c.tlsIntStrArg(fn, pf["pal_tls_set_sni"]) })
	bridge("promise_tls_set_verify_host", func(fn *ir.Func) { c.tlsIntStrArg(fn, pf["pal_tls_set_verify_host"]) })

	// (int) string
	bridge("promise_tls_get_version", func(fn *ir.Func) { c.tlsStrRet1Arg(fn, pf["pal_tls_get_version"]) })
	bridge("promise_tls_get_cipher", func(fn *ir.Func) { c.tlsStrRet1Arg(fn, pf["pal_tls_get_cipher"]) })
}

// --- bridge shapes (Linux/OpenSSL) -----------------------------------------

// tlsStoreInt stores a pal result (i32 or i64) as a Promise int, sign-extending
// i32 returns to i64 first. The pal_tls_* wrappers return i32 for status/flag
// results and i64 for handles/byte-counts; this normalizes both.
func (c *Compiler) tlsStoreInt(b *ir.Block, sret, v value.Value) {
	if v.Type().Equal(irtypes.I32) {
		v = b.NewSExt(v, irtypes.I64)
	}
	c.storeIntResult(b, sret, v)
}

// void @f(i8* sret) — call pal() → handle, store as Promise int.
func (c *Compiler) tlsHandleFactory(fn, palFn *ir.Func) {
	b := fn.NewBlock(".entry")
	res := b.NewCall(palFn)
	c.tlsStoreInt(b, fn.Params[0], res)
	b.NewRet(nil)
}

// void @f(i8* sret, i8* a) — call pal(i64 a), store result as Promise int.
func (c *Compiler) tlsInt1Arg(fn, palFn *ir.Func) {
	b := fn.NewBlock(".entry")
	a := c.extractRawInt(b, fn.Params[1])
	res := b.NewCall(palFn, a)
	c.tlsStoreInt(b, fn.Params[0], res)
	b.NewRet(nil)
}

// void @f(i8* a) — call pal(i64 a).
func (c *Compiler) tlsVoid1IntArg(fn, palFn *ir.Func) {
	b := fn.NewBlock(".entry")
	a := c.extractRawInt(b, fn.Params[0])
	b.NewCall(palFn, a)
	b.NewRet(nil)
}

// void @f(i8* a, i8* x) — call pal(i64 a, i32 x).
func (c *Compiler) tlsVoid2IntArg(fn, palFn *ir.Func) {
	b := fn.NewBlock(".entry")
	a := c.extractRawInt(b, fn.Params[0])
	x := c.extractRawInt(b, fn.Params[1])
	b.NewCall(palFn, a, b.NewTrunc(x, irtypes.I32))
	b.NewRet(nil)
}

// void @f(i8* sret, i8* a, i8* x) — call pal(i64 a, i32 x) → i32, store int.
func (c *Compiler) tlsInt2IntArg(fn, palFn *ir.Func) {
	b := fn.NewBlock(".entry")
	a := c.extractRawInt(b, fn.Params[1])
	x := c.extractRawInt(b, fn.Params[2])
	res := b.NewCall(palFn, a, b.NewTrunc(x, irtypes.I32))
	c.tlsStoreInt(b, fn.Params[0], res)
	b.NewRet(nil)
}

// void @f(i8* sret, i8* a, i8* vec) — call pal(i64 a, i8* ptr, i64 len) → i64.
func (c *Compiler) tlsIntVecArg(fn, palFn *ir.Func) {
	b := fn.NewBlock(".entry")
	a := c.extractRawInt(b, fn.Params[1])
	dataPtr, dataLen := extractVectorDataLen(b, fn.Params[2])
	ptr := b.NewBitCast(dataPtr, irtypes.I8Ptr)
	res := b.NewCall(palFn, a, ptr, dataLen)
	c.tlsStoreInt(b, fn.Params[0], res)
	b.NewRet(nil)
}

// void @f(i8* sret, i8* a, i8* vec, i8* len) — call pal(i64 a, i8* ptr, i64 len)
// using the explicit length arg (not the vector's own length).
func (c *Compiler) tlsIntVecLenArg(fn, palFn *ir.Func) {
	b := fn.NewBlock(".entry")
	a := c.extractRawInt(b, fn.Params[1])
	dataPtr, _ := extractVectorDataLen(b, fn.Params[2])
	ptr := b.NewBitCast(dataPtr, irtypes.I8Ptr)
	length := c.extractRawInt(b, fn.Params[3])
	res := b.NewCall(palFn, a, ptr, length)
	c.tlsStoreInt(b, fn.Params[0], res)
	b.NewRet(nil)
}

// void @f(i8* sret, i8* a, i8* str) — call pal(i64 a, i8* cstr) → i32, store int.
func (c *Compiler) tlsIntStrArg(fn, palFn *ir.Func) {
	b := fn.NewBlock(".entry")
	a := c.extractRawInt(b, fn.Params[1])
	cstr := c.stringToCStr(b, fn.Params[2])
	res := b.NewCall(palFn, a, cstr)
	b.NewCall(c.palFree, cstr)
	c.tlsStoreInt(b, fn.Params[0], res)
	b.NewRet(nil)
}

// void @f(i8* sret, i8* a) — call pal(i64 a) → i8* (static const char*),
// copy into a fresh Promise string.
func (c *Compiler) tlsStrRet1Arg(fn, palFn *ir.Func) {
	b := fn.NewBlock(".entry")
	a := c.extractRawInt(b, fn.Params[1])
	cstr := b.NewCall(palFn, a)
	length := b.NewCall(c.funcs["strlen"], cstr)
	str := b.NewCall(c.funcs["promise_string_new"], cstr, length)
	c.storeStringResult(b, fn.Params[0], str)
	b.NewRet(nil)
}

// --- non-Linux stub bodies -------------------------------------------------

// defineTLSStubBodies emits inert bodies for every promise_tls_* extern on
// targets with no TLS backend (macOS/Windows/WASM). Handle factories and int
// getters return 0, void ops no-op, and string getters return "". tls.pr sees a
// 0 handle from ctx creation and raises TlsError(kind: unsupported); no OpenSSL
// symbol is referenced, so nothing links.
func (c *Compiler) defineTLSStubBodies(irFuncByName map[string]*ir.Func) {
	// Signatures with an sret (int/string return) vs. void return differ only in
	// whether Params[0] is the sret slot. Classify by the Promise result type,
	// which we recover from the extern's declared LLVM shape: functions that
	// return a value take an i8* sret as their first parameter and we must store
	// into it; void functions just return.
	intReturners := map[string]bool{
		"promise_tls_ctx_new_client": true, "promise_tls_ctx_new_server": true,
		"promise_tls_new": true, "promise_tls_ctx_load_default_trust": true,
		"promise_tls_do_handshake": true, "promise_tls_shutdown": true,
		"promise_tls_bio_pending_out": true, "promise_tls_get_verify_result": true,
		"promise_tls_ctx_set_min_version": true, "promise_tls_ctx_add_ca": true,
		"promise_tls_ctx_use_cert": true, "promise_tls_ctx_use_key": true,
		"promise_tls_read": true, "promise_tls_write": true,
		"promise_tls_bio_read_out": true, "promise_tls_bio_write_in": true,
		"promise_tls_set_sni": true, "promise_tls_set_verify_host": true,
	}
	strReturners := map[string]bool{
		"promise_tls_get_version": true, "promise_tls_get_cipher": true,
	}

	for _, name := range tlsExternNames {
		fn, ok := irFuncByName[name]
		if !ok {
			continue
		}
		b := fn.NewBlock(".entry")
		switch {
		case strReturners[name]:
			empty := b.NewCall(c.funcs["promise_string_new"],
				constant.NewNull(irtypes.I8Ptr), constant.NewInt(irtypes.I64, 0))
			c.storeStringResult(b, fn.Params[0], empty)
		case intReturners[name]:
			c.storeIntResult(b, fn.Params[0], constant.NewInt(irtypes.I64, 0))
		default:
			// void return — nothing to store.
		}
		b.NewRet(nil)
	}
}
