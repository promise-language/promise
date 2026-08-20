package pal

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// tls_darwin.go — Secure Transport TLS backend for macOS (T1599).
//
// Slots in behind the same pal_tls_* surface and the same backend-neutral status
// enum as the OpenSSL backend in tls_posix.go, so codegen/tls.go's bridge helpers
// and modules/tls/tls.pr are shared verbatim. Only the implementation differs.
//
// Architecture. Secure Transport has no separate long-lived context object —
// SSLContextRef *is* the per-session object — so the two PAL handles map onto our
// own heap records instead:
//
//	ctx handle → tls_ctx  (side, verify flag, min version, staged cert, identity,
//	                       trust anchors). Pure configuration; no Apple object.
//	ssl handle → tls_sess (the SSLContextRef plus the inbound/outbound byte queues
//	                       that stand in for OpenSSL's two memory BIOs).
//
// SSLSetConnection hands the tls_sess to the I/O callbacks, which are *pure buffer
// accessors*: read_cb drains the inbound queue and returns errSSLWouldBlock when it
// is dry; write_cb appends to the outbound queue and always succeeds.
//
// CRITICAL INVARIANT: the SSLSetIOFuncs callbacks must never perform socket I/O and
// must never park. Promise parks by emitting an inline coro.suspend into the Promise
// frame (netpoll_wait_read/_write are lowered inline at codegen dispatch), so a
// coroutine cannot suspend from inside a C stack frame — parking inside a callback
// would corrupt the coroutine. All socket I/O and reactor parking therefore stay in
// TcpStream.read/write, driven from tls.pr, exactly as on Linux.
//
// Handles cross the boundary as i64 (ptrtoint), 0 meaning null/failure — the gating
// tls.pr relies on to raise TlsError(kind: unsupported) when there is no backend.
//
// Known limitation: Secure Transport implements no TLS 1.3. kTLSProtocol13 exists in
// the enum but SSLSetProtocolVersionMin rejects it with errSSLIllegalParam (-9830) on
// both sides, so this backend is TLS 1.2 only and pal_tls_ctx_set_min_version returns
// 0 for a 1.3 request. See the tracker item filed alongside T1599.
//
// Secure Transport is deprecated by Apple. The recorded fallback is vendoring a
// static BoringSSL/OpenSSL for macOS the way T1596 did for Linux — that preserves
// this memory-buffer architecture rather than dismantling it. Network.framework is
// explicitly rejected: it brings its own libdispatch event loop and would give TLS a
// wholly separate I/O path from plain TCP (T1599 Decision 1).

// Secure Transport / Sec / CoreFoundation ABI constants, read from the macOS SDK
// headers and verified on-host (macOS 26.5, arm64) rather than assumed.
const (
	stServerSide = 0 // kSSLServerSide
	stClientSide = 1 // kSSLClientSide
	stStreamType = 0 // kSSLStreamType

	stProtoTLS1  = 4  // kTLSProtocol1
	stProtoTLS11 = 7  // kTLSProtocol11
	stProtoTLS12 = 8  // kTLSProtocol12
	stProtoTLS13 = 10 // kTLSProtocol13 — present in the enum, not implemented

	stOptBreakOnServerAuth = 0 // kSSLSessionOptionBreakOnServerAuth

	stFormatUnknown    = 0 // kSecFormatUnknown
	stItemTypePrivKey  = 1 // kSecItemTypePrivateKey
	stItemTypeCertific = 4 // kSecItemTypeCertificate

	stErrSuccess          = 0     // errSecSuccess
	stErrWouldBlock       = -9803 // errSSLWouldBlock
	stErrClosedGraceful   = -9805 // errSSLClosedGraceful
	stErrPeerAuthComplete = -9841 // errSSLPeerAuthCompleted

	// Wire (not backend) TLS version numbers, matching TlsVersion._to_wire() in
	// modules/tls/tls.pr — the shared, backend-neutral argument to set_min_version.
	stWireTLS12 = 771 // 0x0303
	stWireTLS13 = 772 // 0x0304
)

// tlsDarwinCipherNames maps the SSLCipherSuite values Secure Transport actually
// negotiates onto their IANA-style names, so `cipher_suite` reports a name rather
// than a number. Anything outside the table falls back to a "0xXXXX" rendering.
var tlsDarwinCipherNames = []struct {
	suite int64
	name  string
}{
	{0xC02B, "ECDHE-ECDSA-AES128-GCM-SHA256"},
	{0xC02C, "ECDHE-ECDSA-AES256-GCM-SHA384"},
	{0xC02F, "ECDHE-RSA-AES128-GCM-SHA256"},
	{0xC030, "ECDHE-RSA-AES256-GCM-SHA384"},
	{0xCCA8, "ECDHE-RSA-CHACHA20-POLY1305"},
	{0xCCA9, "ECDHE-ECDSA-CHACHA20-POLY1305"},
	{0xC023, "ECDHE-ECDSA-AES128-SHA256"},
	{0xC027, "ECDHE-RSA-AES128-SHA256"},
	{0xC009, "ECDHE-ECDSA-AES128-SHA"},
	{0xC00A, "ECDHE-ECDSA-AES256-SHA"},
	{0xC013, "ECDHE-RSA-AES128-SHA"},
	{0xC014, "ECDHE-RSA-AES256-SHA"},
	{0x009C, "AES128-GCM-SHA256"},
	{0x009D, "AES256-GCM-SHA384"},
	{0x002F, "AES128-SHA"},
	{0x0035, "AES256-SHA"},
}

// tlsDarwinExternGlobal declares an external data symbol (no initializer) and
// returns it. Used for kCFTypeArrayCallBacks, whose contents are opaque to us —
// only its address is ever passed.
func tlsDarwinExternGlobal(module *ir.Module, name string) *ir.Global {
	for _, g := range module.Globals {
		if g.Name() == name {
			return g
		}
	}
	g := module.NewGlobal(name, irtypes.I8)
	// Declaration, not definition: the symbol lives in CoreFoundation. Without
	// explicit external linkage llir emits `@x = global i8`, which has no
	// initializer and is rejected by the verifier.
	g.Linkage = enum.LinkageExternal
	return g
}

// EmitTLSSecureTransport emits every pal_tls_* wrapper for macOS and returns them
// keyed by name, mirroring PosixPAL.EmitTLS's contract exactly. The underlying
// Secure Transport / Sec / CoreFoundation symbols are declared as bodyless externs
// and resolved at link against Security.framework and CoreFoundation.framework.
func (p *PosixPAL) EmitTLSSecureTransport(module *ir.Module) map[string]*ir.Func {
	fns := make(map[string]*ir.Func)
	emit := func(f *ir.Func) *ir.Func {
		fns[f.Name()] = f
		return f
	}

	null := constant.NewNull(irtypes.I8Ptr)
	i8 := func(v int64) *constant.Int { return constant.NewInt(irtypes.I8, v) }
	i32 := func(v int64) *constant.Int { return constant.NewInt(irtypes.I32, v) }
	i64 := func(v int64) *constant.Int { return constant.NewInt(irtypes.I64, v) }

	// --- record layouts -------------------------------------------------
	// A byte queue: growable buffer with a consumed prefix [0,head) and live
	// bytes [head,tail). Both indices reset to 0 whenever the queue drains, so a
	// steady-state stream keeps reusing one small buffer.
	qTy := irtypes.NewStruct(
		irtypes.I8Ptr, // 0 buf
		irtypes.I64,   // 1 cap
		irtypes.I64,   // 2 head
		irtypes.I64,   // 3 tail
	)
	qPtrTy := irtypes.NewPointer(qTy)

	ctxTy := irtypes.NewStruct(
		irtypes.I32,   // 0 side (kSSLClientSide / kSSLServerSide)
		irtypes.I32,   // 1 verify (0/1)
		irtypes.I32,   // 2 min_version (SSLProtocol)
		irtypes.I32,   // 3 _pad
		irtypes.I8Ptr, // 4 identity     (SecIdentityRef, retained)
		irtypes.I8Ptr, // 5 staged_cert  (SecCertificateRef, retained)
		irtypes.I8Ptr, // 6 anchors      (SecCertificateRef[], pal_alloc'd)
		irtypes.I64,   // 7 anchor_count
		irtypes.I64,   // 8 anchor_cap
	)
	ctxPtrTy := irtypes.NewPointer(ctxTy)

	sessTy := irtypes.NewStruct(
		irtypes.I8Ptr,                   // 0 ssl (SSLContextRef)
		qTy,                             // 1 in  (ciphertext from the peer)
		qTy,                             // 2 out (ciphertext for the peer)
		irtypes.I8Ptr,                   // 3 ctx (owning tls_ctx*, borrowed)
		irtypes.I32,                     // 4 verify_result (0 == ok)
		irtypes.NewArray(8, irtypes.I8), // 5 cipher fallback buffer ("0xXXXX\0")
	)
	sessPtrTy := irtypes.NewPointer(sessTy)

	// sizeOf uses the standard GEP-on-null idiom so the sizes always track the
	// struct definitions above rather than being hand-computed.
	sizeOf := func(t irtypes.Type) constant.Constant {
		gep := constant.NewGetElementPtr(t, constant.NewNull(irtypes.NewPointer(t)), i64(1))
		return constant.NewPtrToInt(gep, irtypes.I64)
	}
	// fld returns a pointer to field idx of a struct pointer.
	fld := func(b *ir.Block, t irtypes.Type, p value.Value, idx int64) value.Value {
		return b.NewGetElementPtr(t, p, i32(0), i32(idx))
	}

	// --- externs --------------------------------------------------------
	palAlloc := getOrDeclareFunc(module, "pal_alloc", irtypes.I8Ptr,
		ir.NewParam("size", irtypes.I64))
	palFree := getOrDeclareFunc(module, "pal_free", irtypes.Void,
		ir.NewParam("ptr", irtypes.I8Ptr))
	memcpyFn := getOrDeclareFunc(module, "memcpy", irtypes.I8Ptr,
		ir.NewParam("dst", irtypes.I8Ptr), ir.NewParam("src", irtypes.I8Ptr),
		ir.NewParam("n", irtypes.I64))
	strlenFn := getOrDeclareFunc(module, "strlen", irtypes.I64,
		ir.NewParam("s", irtypes.I8Ptr))

	// CoreFoundation
	cfRelease := getOrDeclareFunc(module, "CFRelease", irtypes.Void,
		ir.NewParam("cf", irtypes.I8Ptr))
	cfRetain := getOrDeclareFunc(module, "CFRetain", irtypes.I8Ptr,
		ir.NewParam("cf", irtypes.I8Ptr))
	cfDataCreate := getOrDeclareFunc(module, "CFDataCreate", irtypes.I8Ptr,
		ir.NewParam("alloc", irtypes.I8Ptr), ir.NewParam("bytes", irtypes.I8Ptr),
		ir.NewParam("length", irtypes.I64))
	cfArrayCreate := getOrDeclareFunc(module, "CFArrayCreate", irtypes.I8Ptr,
		ir.NewParam("alloc", irtypes.I8Ptr), ir.NewParam("values", irtypes.I8Ptr),
		ir.NewParam("numValues", irtypes.I64), ir.NewParam("callBacks", irtypes.I8Ptr))
	cfArrayGetCount := getOrDeclareFunc(module, "CFArrayGetCount", irtypes.I64,
		ir.NewParam("array", irtypes.I8Ptr))
	cfArrayGetValueAtIndex := getOrDeclareFunc(module, "CFArrayGetValueAtIndex", irtypes.I8Ptr,
		ir.NewParam("array", irtypes.I8Ptr), ir.NewParam("idx", irtypes.I64))
	cfGetTypeID := getOrDeclareFunc(module, "CFGetTypeID", irtypes.I64,
		ir.NewParam("cf", irtypes.I8Ptr))
	cfTypeArrayCallBacks := tlsDarwinExternGlobal(module, "kCFTypeArrayCallBacks")

	// Secure Transport
	sslCreateContext := getOrDeclareFunc(module, "SSLCreateContext", irtypes.I8Ptr,
		ir.NewParam("alloc", irtypes.I8Ptr), ir.NewParam("side", irtypes.I32),
		ir.NewParam("connType", irtypes.I32))
	sslSetIOFuncs := getOrDeclareFunc(module, "SSLSetIOFuncs", irtypes.I32,
		ir.NewParam("ctx", irtypes.I8Ptr), ir.NewParam("readFunc", irtypes.I8Ptr),
		ir.NewParam("writeFunc", irtypes.I8Ptr))
	sslSetConnection := getOrDeclareFunc(module, "SSLSetConnection", irtypes.I32,
		ir.NewParam("ctx", irtypes.I8Ptr), ir.NewParam("conn", irtypes.I8Ptr))
	sslSetProtocolVersionMin := getOrDeclareFunc(module, "SSLSetProtocolVersionMin", irtypes.I32,
		ir.NewParam("ctx", irtypes.I8Ptr), ir.NewParam("minVersion", irtypes.I32))
	sslSetPeerDomainName := getOrDeclareFunc(module, "SSLSetPeerDomainName", irtypes.I32,
		ir.NewParam("ctx", irtypes.I8Ptr), ir.NewParam("peerName", irtypes.I8Ptr),
		ir.NewParam("peerNameLen", irtypes.I64))
	sslSetCertificate := getOrDeclareFunc(module, "SSLSetCertificate", irtypes.I32,
		ir.NewParam("ctx", irtypes.I8Ptr), ir.NewParam("certRefs", irtypes.I8Ptr))
	sslSetSessionOption := getOrDeclareFunc(module, "SSLSetSessionOption", irtypes.I32,
		ir.NewParam("ctx", irtypes.I8Ptr), ir.NewParam("option", irtypes.I32),
		ir.NewParam("value", irtypes.I8))
	sslHandshake := getOrDeclareFunc(module, "SSLHandshake", irtypes.I32,
		ir.NewParam("ctx", irtypes.I8Ptr))
	sslReadFn := getOrDeclareFunc(module, "SSLRead", irtypes.I32,
		ir.NewParam("ctx", irtypes.I8Ptr), ir.NewParam("data", irtypes.I8Ptr),
		ir.NewParam("dataLength", irtypes.I64),
		ir.NewParam("processed", irtypes.NewPointer(irtypes.I64)))
	sslWriteFn := getOrDeclareFunc(module, "SSLWrite", irtypes.I32,
		ir.NewParam("ctx", irtypes.I8Ptr), ir.NewParam("data", irtypes.I8Ptr),
		ir.NewParam("dataLength", irtypes.I64),
		ir.NewParam("processed", irtypes.NewPointer(irtypes.I64)))
	sslClose := getOrDeclareFunc(module, "SSLClose", irtypes.I32,
		ir.NewParam("ctx", irtypes.I8Ptr))
	sslGetNegotiatedProtocolVersion := getOrDeclareFunc(module, "SSLGetNegotiatedProtocolVersion",
		irtypes.I32, ir.NewParam("ctx", irtypes.I8Ptr),
		ir.NewParam("protocol", irtypes.NewPointer(irtypes.I32)))
	// The out-param is SSLCipherSuite*, and that type is arch-dependent:
	// uint16_t on arm64 macOS, uint32_t everywhere else (CipherSuite.h keys the
	// typedef on TARGET_CPU_ARM64). This backend is emitted for both Promise
	// macOS arches, so the slot must be the wider of the two — a 2-byte slot
	// would be overrun by 2 bytes on x86_64. Callers pre-zero it, so the
	// little-endian narrow write still reads back as the correct suite number.
	sslGetNegotiatedCipher := getOrDeclareFunc(module, "SSLGetNegotiatedCipher", irtypes.I32,
		ir.NewParam("ctx", irtypes.I8Ptr),
		ir.NewParam("cipherSuite", irtypes.NewPointer(irtypes.I32)))
	sslCopyPeerTrust := getOrDeclareFunc(module, "SSLCopyPeerTrust", irtypes.I32,
		ir.NewParam("ctx", irtypes.I8Ptr), ir.NewParam("trust", irtypes.NewPointer(irtypes.I8Ptr)))

	// Sec — SecIdentityCreate is SPI (exported from Security.framework and present
	// in the SDK stub, but with no public header). It builds a transient identity
	// from a cert+key pair with no keychain involvement, which is exactly what a
	// self-contained Promise binary needs; the public alternative would require
	// creating and cleaning up a temporary keychain file.
	secItemImport := getOrDeclareFunc(module, "SecItemImport", irtypes.I32,
		ir.NewParam("importedData", irtypes.I8Ptr), ir.NewParam("fileNameOrExtension", irtypes.I8Ptr),
		ir.NewParam("inputFormat", irtypes.NewPointer(irtypes.I32)),
		ir.NewParam("itemType", irtypes.NewPointer(irtypes.I32)),
		ir.NewParam("flags", irtypes.I32), ir.NewParam("keyParams", irtypes.I8Ptr),
		ir.NewParam("importKeychain", irtypes.I8Ptr),
		ir.NewParam("outItems", irtypes.NewPointer(irtypes.I8Ptr)))
	secCertificateGetTypeID := getOrDeclareFunc(module, "SecCertificateGetTypeID", irtypes.I64)
	secKeyGetTypeID := getOrDeclareFunc(module, "SecKeyGetTypeID", irtypes.I64)
	secIdentityCreate := getOrDeclareFunc(module, "SecIdentityCreate", irtypes.I8Ptr,
		ir.NewParam("alloc", irtypes.I8Ptr), ir.NewParam("cert", irtypes.I8Ptr),
		ir.NewParam("key", irtypes.I8Ptr))
	secTrustEvaluateWithError := getOrDeclareFunc(module, "SecTrustEvaluateWithError", irtypes.I8,
		ir.NewParam("trust", irtypes.I8Ptr), ir.NewParam("error", irtypes.I8Ptr))
	secTrustSetAnchorCertificates := getOrDeclareFunc(module, "SecTrustSetAnchorCertificates",
		irtypes.I32, ir.NewParam("trust", irtypes.I8Ptr), ir.NewParam("anchors", irtypes.I8Ptr))
	secTrustSetAnchorCertificatesOnly := getOrDeclareFunc(module, "SecTrustSetAnchorCertificatesOnly",
		irtypes.I32, ir.NewParam("trust", irtypes.I8Ptr), ir.NewParam("only", irtypes.I8))

	// --- queue helpers ---------------------------------------------------
	// Emitted once and shared by the I/O callbacks and the bio_* entry points, so
	// the queue mechanics have a single implementation.

	// void @__promise_tls_q_append(q* q, i8* data, i64 len)
	// Appends len bytes. Grows by allocating a fresh buffer and copying only the
	// live region [head,tail) to its front — this reclaims the consumed prefix, so
	// a partially-drained queue cannot grow without bound. Deliberately never uses
	// memmove (the copy is always between distinct buffers).
	qAppend := module.NewFunc("__promise_tls_q_append", irtypes.Void,
		ir.NewParam("q", qPtrTy), ir.NewParam("data", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64))
	qAppend.FuncAttrs = append(qAppend.FuncAttrs, enum.FuncAttrNoUnwind)
	{
		q, data, length := qAppend.Params[0], qAppend.Params[1], qAppend.Params[2]
		entry := qAppend.NewBlock(".entry")
		bufP := fld(entry, qTy, q, 0)
		capP := fld(entry, qTy, q, 1)
		headP := fld(entry, qTy, q, 2)
		tailP := fld(entry, qTy, q, 3)

		zeroLen := entry.NewICmp(enum.IPredEQ, length, i64(0))
		retBlk := qAppend.NewBlock(".ret")
		work := qAppend.NewBlock(".work")
		entry.NewCondBr(zeroLen, retBlk, work)
		retBlk.NewRet(nil)

		tail := work.NewLoad(irtypes.I64, tailP)
		capV := work.NewLoad(irtypes.I64, capP)
		need := work.NewAdd(tail, length)
		fits := work.NewICmp(enum.IPredULE, need, capV)
		copyBlk := qAppend.NewBlock(".copy")
		growBlk := qAppend.NewBlock(".grow")
		work.NewCondBr(fits, copyBlk, growBlk)

		// grow: newcap = (live + len) * 2 + 4096; compact live region to front.
		head := growBlk.NewLoad(irtypes.I64, headP)
		live := growBlk.NewSub(tail, head)
		newCap := growBlk.NewAdd(growBlk.NewMul(growBlk.NewAdd(live, length), i64(2)), i64(4096))
		newBuf := growBlk.NewCall(palAlloc, newCap)
		oldBuf := growBlk.NewLoad(irtypes.I8Ptr, bufP)
		hasLive := growBlk.NewICmp(enum.IPredUGT, live, i64(0))
		mvBlk := qAppend.NewBlock(".move_live")
		afterMv := qAppend.NewBlock(".after_move")
		growBlk.NewCondBr(hasLive, mvBlk, afterMv)
		mvBlk.NewCall(memcpyFn, newBuf, mvBlk.NewGetElementPtr(irtypes.I8, oldBuf, head), live)
		mvBlk.NewBr(afterMv)

		oldNonNull := afterMv.NewICmp(enum.IPredNE, oldBuf, null)
		freeBlk := qAppend.NewBlock(".free_old")
		afterFree := qAppend.NewBlock(".after_free")
		afterMv.NewCondBr(oldNonNull, freeBlk, afterFree)
		freeBlk.NewCall(palFree, oldBuf)
		freeBlk.NewBr(afterFree)

		afterFree.NewStore(newBuf, bufP)
		afterFree.NewStore(newCap, capP)
		afterFree.NewStore(i64(0), headP)
		afterFree.NewStore(live, tailP)
		afterFree.NewBr(copyBlk)

		// copy: append at tail.
		buf := copyBlk.NewLoad(irtypes.I8Ptr, bufP)
		t2 := copyBlk.NewLoad(irtypes.I64, tailP)
		copyBlk.NewCall(memcpyFn, copyBlk.NewGetElementPtr(irtypes.I8, buf, t2), data, length)
		copyBlk.NewStore(copyBlk.NewAdd(t2, length), tailP)
		copyBlk.NewRet(nil)
	}

	// i64 @__promise_tls_q_take(q* q, i8* dst, i64 max)
	// Copies out up to max live bytes and returns the count. Resets both indices
	// when the queue drains, which is what keeps the buffer reusable.
	qTake := module.NewFunc("__promise_tls_q_take", irtypes.I64,
		ir.NewParam("q", qPtrTy), ir.NewParam("dst", irtypes.I8Ptr),
		ir.NewParam("max", irtypes.I64))
	qTake.FuncAttrs = append(qTake.FuncAttrs, enum.FuncAttrNoUnwind)
	{
		q, dst, max := qTake.Params[0], qTake.Params[1], qTake.Params[2]
		entry := qTake.NewBlock(".entry")
		bufP := fld(entry, qTy, q, 0)
		headP := fld(entry, qTy, q, 2)
		tailP := fld(entry, qTy, q, 3)
		head := entry.NewLoad(irtypes.I64, headP)
		tail := entry.NewLoad(irtypes.I64, tailP)
		live := entry.NewSub(tail, head)
		useMax := entry.NewICmp(enum.IPredULT, max, live)
		n := entry.NewSelect(useMax, max, live)
		hasData := entry.NewICmp(enum.IPredUGT, n, i64(0))
		copyBlk := qTake.NewBlock(".copy")
		doneBlk := qTake.NewBlock(".done")
		entry.NewCondBr(hasData, copyBlk, doneBlk)

		buf := copyBlk.NewLoad(irtypes.I8Ptr, bufP)
		copyBlk.NewCall(memcpyFn, dst, copyBlk.NewGetElementPtr(irtypes.I8, buf, head), n)
		newHead := copyBlk.NewAdd(head, n)
		copyBlk.NewStore(newHead, headP)
		drained := copyBlk.NewICmp(enum.IPredEQ, newHead, tail)
		resetBlk := qTake.NewBlock(".reset")
		copyBlk.NewCondBr(drained, resetBlk, doneBlk)
		resetBlk.NewStore(i64(0), headP)
		resetBlk.NewStore(i64(0), tailP)
		resetBlk.NewBr(doneBlk)

		doneBlk.NewRet(n)
	}

	// void @__promise_tls_q_free(q* q) — release the backing buffer.
	qFree := module.NewFunc("__promise_tls_q_free", irtypes.Void, ir.NewParam("q", qPtrTy))
	qFree.FuncAttrs = append(qFree.FuncAttrs, enum.FuncAttrNoUnwind)
	{
		q := qFree.Params[0]
		entry := qFree.NewBlock(".entry")
		bufP := fld(entry, qTy, q, 0)
		buf := entry.NewLoad(irtypes.I8Ptr, bufP)
		nonNull := entry.NewICmp(enum.IPredNE, buf, null)
		freeBlk := qFree.NewBlock(".free")
		doneBlk := qFree.NewBlock(".done")
		entry.NewCondBr(nonNull, freeBlk, doneBlk)
		freeBlk.NewCall(palFree, buf)
		freeBlk.NewStore(null, bufP)
		freeBlk.NewStore(i64(0), fld(freeBlk, qTy, q, 1))
		freeBlk.NewBr(doneBlk)
		doneBlk.NewRet(nil)
	}

	// --- Secure Transport I/O callbacks ---------------------------------
	// OSStatus (*)(SSLConnectionRef conn, void *data, size_t *dataLength)
	// Pure buffer accessors. No syscall, no park — see the CRITICAL INVARIANT above.

	readCB := module.NewFunc("__promise_tls_read_cb", irtypes.I32,
		ir.NewParam("conn", irtypes.I8Ptr), ir.NewParam("data", irtypes.I8Ptr),
		ir.NewParam("dataLength", irtypes.NewPointer(irtypes.I64)))
	readCB.FuncAttrs = append(readCB.FuncAttrs, enum.FuncAttrNoUnwind)
	{
		conn, data, lenP := readCB.Params[0], readCB.Params[1], readCB.Params[2]
		entry := readCB.NewBlock(".entry")
		sess := entry.NewBitCast(conn, sessPtrTy)
		want := entry.NewLoad(irtypes.I64, lenP)
		got := entry.NewCall(qTake, fld(entry, sessTy, sess, 1), data, want)
		entry.NewStore(got, lenP)
		// Secure Transport re-requests the remainder after a short read, so
		// reporting errSSLWouldBlock on underflow is the whole back-pressure story.
		full := entry.NewICmp(enum.IPredEQ, got, want)
		okBlk := readCB.NewBlock(".ok")
		blockBlk := readCB.NewBlock(".would_block")
		entry.NewCondBr(full, okBlk, blockBlk)
		okBlk.NewRet(i32(stErrSuccess))
		blockBlk.NewRet(i32(stErrWouldBlock))
	}

	writeCB := module.NewFunc("__promise_tls_write_cb", irtypes.I32,
		ir.NewParam("conn", irtypes.I8Ptr), ir.NewParam("data", irtypes.I8Ptr),
		ir.NewParam("dataLength", irtypes.NewPointer(irtypes.I64)))
	writeCB.FuncAttrs = append(writeCB.FuncAttrs, enum.FuncAttrNoUnwind)
	{
		conn, data, lenP := writeCB.Params[0], writeCB.Params[1], writeCB.Params[2]
		entry := writeCB.NewBlock(".entry")
		sess := entry.NewBitCast(conn, sessPtrTy)
		n := entry.NewLoad(irtypes.I64, lenP)
		// The outbound queue is unbounded, so every byte is always accepted and
		// *dataLength already reflects the full count.
		entry.NewCall(qAppend, fld(entry, sessTy, sess, 2), data, n)
		entry.NewRet(i32(stErrSuccess))
	}

	// --- PEM import helper ----------------------------------------------
	// i8* @__promise_tls_import(i8* pem, i64 len, i32 wantType)
	// Imports PEM with no keychain and returns the first item of the requested
	// kind, retained, or null.
	//
	// The itemType argument to SecItemImport is only a HINT on the way in: asking
	// for a private key while handing it a certificate SUCCEEDS and reports a
	// certificate on the way out, so the result must be checked. It is checked by
	// CFTypeID over the returned items rather than by that out-type, because a
	// multi-block PEM (the ordinary `fullchain.pem` shape: leaf + intermediates,
	// or a CA bundle) is reported as kSecItemTypeAggregate — gating on the
	// out-type would reject every real-world certificate bundle, which the OpenSSL
	// backend accepts. Scanning by type keeps both backends taking the leaf and
	// still makes set_client_certificate(cert, cert) fail as it must, since a
	// certificate-only PEM yields no SecKeyRef.
	tlsImport := module.NewFunc("__promise_tls_import", irtypes.I8Ptr,
		ir.NewParam("pem", irtypes.I8Ptr), ir.NewParam("len", irtypes.I64),
		ir.NewParam("want", irtypes.I32))
	tlsImport.FuncAttrs = append(tlsImport.FuncAttrs, enum.FuncAttrNoUnwind)
	{
		pem, length, want := tlsImport.Params[0], tlsImport.Params[1], tlsImport.Params[2]
		entry := tlsImport.NewBlock(".entry")
		fmtP := entry.NewAlloca(irtypes.I32)
		typP := entry.NewAlloca(irtypes.I32)
		outP := entry.NewAlloca(irtypes.I8Ptr)
		idxSlot := entry.NewAlloca(irtypes.I64)
		wantIDSlot := entry.NewAlloca(irtypes.I64)
		entry.NewStore(i32(stFormatUnknown), fmtP)
		entry.NewStore(want, typP)
		entry.NewStore(null, outP)
		entry.NewStore(i64(0), idxSlot)

		data := entry.NewCall(cfDataCreate, null, pem, length)
		dataNull := entry.NewICmp(enum.IPredEQ, data, null)
		failBlk := tlsImport.NewBlock(".fail")
		haveData := tlsImport.NewBlock(".have_data")
		entry.NewCondBr(dataNull, failBlk, haveData)
		failBlk.NewRet(null)

		st := haveData.NewCall(secItemImport, data, null, fmtP, typP, i32(0), null, null, outP)
		haveData.NewCall(cfRelease, data)
		stOK := haveData.NewICmp(enum.IPredEQ, st, i32(stErrSuccess))
		importOK := tlsImport.NewBlock(".import_ok")
		haveData.NewCondBr(stOK, importOK, failBlk)

		arr := importOK.NewLoad(irtypes.I8Ptr, outP)
		arrNull := importOK.NewICmp(enum.IPredEQ, arr, null)
		haveArr := tlsImport.NewBlock(".have_array")
		importOK.NewCondBr(arrNull, failBlk, haveArr)

		// Release the array on every remaining failure path.
		relFail := tlsImport.NewBlock(".release_fail")
		relFail.NewCall(cfRelease, arr)
		relFail.NewRet(null)

		// The CFTypeID we are looking for, from the same want code the callers use.
		count := haveArr.NewCall(cfArrayGetCount, arr)
		wantKey := haveArr.NewICmp(enum.IPredEQ, want, i32(stItemTypePrivKey))
		keyIDBlk := tlsImport.NewBlock(".key_type_id")
		certIDBlk := tlsImport.NewBlock(".cert_type_id")
		scanHead := tlsImport.NewBlock(".scan")
		haveArr.NewCondBr(wantKey, keyIDBlk, certIDBlk)
		keyIDBlk.NewStore(keyIDBlk.NewCall(secKeyGetTypeID), wantIDSlot)
		keyIDBlk.NewBr(scanHead)
		certIDBlk.NewStore(certIDBlk.NewCall(secCertificateGetTypeID), wantIDSlot)
		certIDBlk.NewBr(scanHead)

		i := scanHead.NewLoad(irtypes.I64, idxSlot)
		more := scanHead.NewICmp(enum.IPredSLT, i, count)
		scanBody := tlsImport.NewBlock(".scan_item")
		scanHead.NewCondBr(more, scanBody, relFail)

		// CFArrayGetValueAtIndex returns a borrowed reference.
		item := scanBody.NewCall(cfArrayGetValueAtIndex, arr, i)
		itemNull := scanBody.NewICmp(enum.IPredEQ, item, null)
		nextItem := tlsImport.NewBlock(".next_item")
		checkType := tlsImport.NewBlock(".check_type")
		scanBody.NewCondBr(itemNull, nextItem, checkType)
		gotID := checkType.NewCall(cfGetTypeID, item)
		wantID := checkType.NewLoad(irtypes.I64, wantIDSlot)
		typeOK := checkType.NewICmp(enum.IPredEQ, gotID, wantID)
		retainIt := tlsImport.NewBlock(".retain")
		checkType.NewCondBr(typeOK, retainIt, nextItem)
		nextItem.NewStore(nextItem.NewAdd(i, i64(1)), idxSlot)
		nextItem.NewBr(scanHead)

		// Retain the borrowed item before dropping the array that owns it.
		retainIt.NewCall(cfRetain, item)
		retainIt.NewCall(cfRelease, arr)
		retainIt.NewRet(item)
	}

	// --- trust evaluation helper ----------------------------------------
	// i32 @__promise_tls_check_trust(sess* s) → 0 accept, 1 reject.
	// Runs on errSSLPeerAuthCompleted (clients only). Custom anchors are ADDED to
	// the system trust store rather than replacing it (AnchorCertificatesOnly =
	// false), matching the OpenSSL backend's X509_STORE_add_cert semantics.
	checkTrust := module.NewFunc("__promise_tls_check_trust", irtypes.I32,
		ir.NewParam("sess", sessPtrTy))
	checkTrust.FuncAttrs = append(checkTrust.FuncAttrs, enum.FuncAttrNoUnwind)
	{
		sess := checkTrust.Params[0]
		entry := checkTrust.NewBlock(".entry")
		trustP := entry.NewAlloca(irtypes.I8Ptr)
		entry.NewStore(null, trustP)
		ctxRaw := entry.NewLoad(irtypes.I8Ptr, fld(entry, sessTy, sess, 3))
		ctx := entry.NewBitCast(ctxRaw, ctxPtrTy)
		verify := entry.NewLoad(irtypes.I32, fld(entry, ctxTy, ctx, 1))
		verifying := entry.NewICmp(enum.IPredNE, verify, i32(0))
		acceptBlk := checkTrust.NewBlock(".accept")
		rejectBlk := checkTrust.NewBlock(".reject")
		doEval := checkTrust.NewBlock(".evaluate")
		entry.NewCondBr(verifying, doEval, acceptBlk)
		acceptBlk.NewRet(i32(0))
		rejectBlk.NewRet(i32(1))

		ssl := doEval.NewLoad(irtypes.I8Ptr, fld(doEval, sessTy, sess, 0))
		cpSt := doEval.NewCall(sslCopyPeerTrust, ssl, trustP)
		cpOK := doEval.NewICmp(enum.IPredEQ, cpSt, i32(stErrSuccess))
		haveTrust := checkTrust.NewBlock(".have_trust")
		doEval.NewCondBr(cpOK, haveTrust, rejectBlk)

		trust := haveTrust.NewLoad(irtypes.I8Ptr, trustP)
		trustNull := haveTrust.NewICmp(enum.IPredEQ, trust, null)
		anchorsBlk := checkTrust.NewBlock(".anchors")
		haveTrust.NewCondBr(trustNull, rejectBlk, anchorsBlk)

		count := anchorsBlk.NewLoad(irtypes.I64, fld(anchorsBlk, ctxTy, ctx, 7))
		hasAnchors := anchorsBlk.NewICmp(enum.IPredSGT, count, i64(0))
		setAnchors := checkTrust.NewBlock(".set_anchors")
		evalBlk := checkTrust.NewBlock(".eval")
		anchorsBlk.NewCondBr(hasAnchors, setAnchors, evalBlk)

		anchorPtr := setAnchors.NewLoad(irtypes.I8Ptr, fld(setAnchors, ctxTy, ctx, 6))
		arr := setAnchors.NewCall(cfArrayCreate, null, anchorPtr, count, cfTypeArrayCallBacks)
		arrNull := setAnchors.NewICmp(enum.IPredEQ, arr, null)
		applyAnchors := checkTrust.NewBlock(".apply_anchors")
		setAnchors.NewCondBr(arrNull, evalBlk, applyAnchors)
		applyAnchors.NewCall(secTrustSetAnchorCertificates, trust, arr)
		applyAnchors.NewCall(secTrustSetAnchorCertificatesOnly, trust, i8(0))
		applyAnchors.NewCall(cfRelease, arr)
		applyAnchors.NewBr(evalBlk)

		ok := evalBlk.NewCall(secTrustEvaluateWithError, trust, null)
		evalBlk.NewCall(cfRelease, trust)
		trusted := evalBlk.NewICmp(enum.IPredNE, ok, i8(0))
		evalBlk.NewCondBr(trusted, acceptBlk, rejectBlk)
	}

	// --- pal_tls_ctx_new_client / _server --------------------------------
	emitCtxNew := func(name string, side int64) *ir.Func {
		fn := module.NewFunc(name, irtypes.I64)
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		entry := fn.NewBlock(".entry")
		raw := entry.NewCall(palAlloc, sizeOf(ctxTy))
		isNull := entry.NewICmp(enum.IPredEQ, raw, null)
		failBlk := fn.NewBlock(".fail")
		okBlk := fn.NewBlock(".init")
		entry.NewCondBr(isNull, failBlk, okBlk)
		failBlk.NewRet(i64(0))

		c := okBlk.NewBitCast(raw, ctxPtrTy)
		okBlk.NewStore(i32(side), fld(okBlk, ctxTy, c, 0))
		okBlk.NewStore(i32(0), fld(okBlk, ctxTy, c, 1))
		okBlk.NewStore(i32(stProtoTLS12), fld(okBlk, ctxTy, c, 2))
		okBlk.NewStore(i32(0), fld(okBlk, ctxTy, c, 3))
		okBlk.NewStore(null, fld(okBlk, ctxTy, c, 4))
		okBlk.NewStore(null, fld(okBlk, ctxTy, c, 5))
		okBlk.NewStore(null, fld(okBlk, ctxTy, c, 6))
		okBlk.NewStore(i64(0), fld(okBlk, ctxTy, c, 7))
		okBlk.NewStore(i64(0), fld(okBlk, ctxTy, c, 8))
		okBlk.NewRet(okBlk.NewPtrToInt(raw, irtypes.I64))
		return emit(fn)
	}
	emitCtxNew("pal_tls_ctx_new_client", stClientSide)
	emitCtxNew("pal_tls_ctx_new_server", stServerSide)

	// --- pal_tls_ctx_free(i64 ctx) ---------------------------------------
	// Releases the identity, the staged certificate, and every trust anchor.
	{
		fn := module.NewFunc("pal_tls_ctx_free", irtypes.Void, ir.NewParam("ctx", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		entry := fn.NewBlock(".entry")
		isZero := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		retBlk := fn.NewBlock(".ret")
		body := fn.NewBlock(".body")
		entry.NewCondBr(isZero, retBlk, body)
		retBlk.NewRet(nil)

		raw := body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		c := body.NewBitCast(raw, ctxPtrTy)

		relField := func(cur *ir.Block, idx int64, tag string) *ir.Block {
			p := fld(cur, ctxTy, c, idx)
			v := cur.NewLoad(irtypes.I8Ptr, p)
			nn := cur.NewICmp(enum.IPredNE, v, null)
			rel := fn.NewBlock(".rel_" + tag)
			next := fn.NewBlock(".after_" + tag)
			cur.NewCondBr(nn, rel, next)
			rel.NewCall(cfRelease, v)
			rel.NewStore(null, p)
			rel.NewBr(next)
			return next
		}
		cur := relField(body, 4, "identity")
		cur = relField(cur, 5, "cert")

		// Release each anchor, then the array itself.
		anchors := cur.NewLoad(irtypes.I8Ptr, fld(cur, ctxTy, c, 6))
		count := cur.NewLoad(irtypes.I64, fld(cur, ctxTy, c, 7))
		anchorsNull := cur.NewICmp(enum.IPredEQ, anchors, null)
		freeSelf := fn.NewBlock(".free_self")
		loopHead := fn.NewBlock(".anchor_loop")
		cur.NewCondBr(anchorsNull, freeSelf, loopHead)

		idxSlot := body.NewAlloca(irtypes.I64)
		cur.NewStore(i64(0), idxSlot)
		arrPtr := loopHead.NewBitCast(anchors, irtypes.NewPointer(irtypes.I8Ptr))
		i := loopHead.NewLoad(irtypes.I64, idxSlot)
		more := loopHead.NewICmp(enum.IPredSLT, i, count)
		loopBody := fn.NewBlock(".anchor_body")
		freeArr := fn.NewBlock(".free_anchors")
		loopHead.NewCondBr(more, loopBody, freeArr)
		elem := loopBody.NewLoad(irtypes.I8Ptr, loopBody.NewGetElementPtr(irtypes.I8Ptr, arrPtr, i))
		elemNN := loopBody.NewICmp(enum.IPredNE, elem, null)
		relElem := fn.NewBlock(".rel_anchor")
		nextElem := fn.NewBlock(".next_anchor")
		loopBody.NewCondBr(elemNN, relElem, nextElem)
		relElem.NewCall(cfRelease, elem)
		relElem.NewBr(nextElem)
		nextElem.NewStore(nextElem.NewAdd(i, i64(1)), idxSlot)
		nextElem.NewBr(loopHead)

		freeArr.NewCall(palFree, anchors)
		freeArr.NewStore(null, fld(freeArr, ctxTy, c, 6))
		freeArr.NewStore(i64(0), fld(freeArr, ctxTy, c, 7))
		freeArr.NewStore(i64(0), fld(freeArr, ctxTy, c, 8))
		freeArr.NewBr(freeSelf)

		freeSelf.NewCall(palFree, raw)
		freeSelf.NewRet(nil)
		emit(fn)
	}

	// --- pal_tls_ctx_set_verify(i64 ctx, i32 peer) ------------------------
	{
		fn := module.NewFunc("pal_tls_ctx_set_verify", irtypes.Void,
			ir.NewParam("ctx", irtypes.I64), ir.NewParam("peer", irtypes.I32))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		entry := fn.NewBlock(".entry")
		isZero := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		retBlk := fn.NewBlock(".ret")
		body := fn.NewBlock(".body")
		entry.NewCondBr(isZero, retBlk, body)
		retBlk.NewRet(nil)
		c := body.NewBitCast(body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr), ctxPtrTy)
		on := body.NewICmp(enum.IPredNE, fn.Params[1], i32(0))
		body.NewStore(body.NewSelect(on, i32(1), i32(0)), fld(body, ctxTy, c, 1))
		body.NewRet(nil)
		emit(fn)
	}

	// --- pal_tls_ctx_set_min_version(i64 ctx, i32 wireVer) → i32 ----------
	// Takes a TLS wire version (771 = 1.2, 772 = 1.3). Returns 0 for TLS 1.3:
	// Secure Transport has no 1.3 implementation, so the request cannot be
	// honoured and the context stays capped at 1.2.
	{
		fn := module.NewFunc("pal_tls_ctx_set_min_version", irtypes.I32,
			ir.NewParam("ctx", irtypes.I64), ir.NewParam("ver", irtypes.I32))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		entry := fn.NewBlock(".entry")
		isZero := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		failBlk := fn.NewBlock(".fail")
		body := fn.NewBlock(".body")
		entry.NewCondBr(isZero, failBlk, body)
		failBlk.NewRet(i32(0))
		c := body.NewBitCast(body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr), ctxPtrTy)
		is12 := body.NewICmp(enum.IPredEQ, fn.Params[1], i32(stWireTLS12))
		set12 := fn.NewBlock(".set_tls12")
		body.NewCondBr(is12, set12, failBlk)
		set12.NewStore(i32(stProtoTLS12), fld(set12, ctxTy, c, 2))
		set12.NewRet(i32(1))
		emit(fn)
	}

	// --- pal_tls_ctx_add_ca(i64 ctx, i8* pem, i64 len) → i32 --------------
	// Imports a PEM certificate and appends it to the context's trust anchors.
	{
		fn := module.NewFunc("pal_tls_ctx_add_ca", irtypes.I32,
			ir.NewParam("ctx", irtypes.I64), ir.NewParam("pem", irtypes.I8Ptr),
			ir.NewParam("len", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		entry := fn.NewBlock(".entry")
		isZero := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		failBlk := fn.NewBlock(".fail")
		body := fn.NewBlock(".body")
		entry.NewCondBr(isZero, failBlk, body)
		failBlk.NewRet(i32(0))

		c := body.NewBitCast(body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr), ctxPtrTy)
		cert := body.NewCall(tlsImport, fn.Params[1], fn.Params[2], i32(stItemTypeCertific))
		certNull := body.NewICmp(enum.IPredEQ, cert, null)
		haveCert := fn.NewBlock(".have_cert")
		body.NewCondBr(certNull, failBlk, haveCert)

		countP := fld(haveCert, ctxTy, c, 7)
		capP := fld(haveCert, ctxTy, c, 8)
		anchorsP := fld(haveCert, ctxTy, c, 6)
		count := haveCert.NewLoad(irtypes.I64, countP)
		capV := haveCert.NewLoad(irtypes.I64, capP)
		full := haveCert.NewICmp(enum.IPredSGE, count, capV)
		growBlk := fn.NewBlock(".grow")
		appendBlk := fn.NewBlock(".append")
		haveCert.NewCondBr(full, growBlk, appendBlk)

		// newcap = cap == 0 ? 4 : cap * 2
		capZero := growBlk.NewICmp(enum.IPredEQ, capV, i64(0))
		newCap := growBlk.NewSelect(capZero, i64(4), growBlk.NewMul(capV, i64(2)))
		newArr := growBlk.NewCall(palAlloc, growBlk.NewMul(newCap, i64(8)))
		newArrNull := growBlk.NewICmp(enum.IPredEQ, newArr, null)
		// An allocation failure must not leak the certificate we just imported.
		allocFail := fn.NewBlock(".alloc_fail")
		copyOld := fn.NewBlock(".copy_old")
		growBlk.NewCondBr(newArrNull, allocFail, copyOld)
		allocFail.NewCall(cfRelease, cert)
		allocFail.NewRet(i32(0))

		oldArr := copyOld.NewLoad(irtypes.I8Ptr, anchorsP)
		oldNN := copyOld.NewICmp(enum.IPredNE, oldArr, null)
		moveOld := fn.NewBlock(".move_old")
		storeNew := fn.NewBlock(".store_new")
		copyOld.NewCondBr(oldNN, moveOld, storeNew)
		moveOld.NewCall(memcpyFn, newArr, oldArr, moveOld.NewMul(count, i64(8)))
		moveOld.NewCall(palFree, oldArr)
		moveOld.NewBr(storeNew)
		storeNew.NewStore(newArr, anchorsP)
		storeNew.NewStore(newCap, capP)
		storeNew.NewBr(appendBlk)

		arr := appendBlk.NewBitCast(appendBlk.NewLoad(irtypes.I8Ptr, anchorsP),
			irtypes.NewPointer(irtypes.I8Ptr))
		appendBlk.NewStore(cert, appendBlk.NewGetElementPtr(irtypes.I8Ptr, arr, count))
		appendBlk.NewStore(appendBlk.NewAdd(count, i64(1)), countP)
		appendBlk.NewRet(i32(1))
		emit(fn)
	}

	// --- pal_tls_ctx_use_cert(i64 ctx, i8* pem, i64 len) → i32 ------------
	// Stages the certificate. The identity is only built once the matching key
	// arrives (use_key), because Secure Transport wants a SecIdentityRef, not a
	// bare certificate. Any previously staged cert/identity is released first, so
	// calling this twice cannot leak.
	{
		fn := module.NewFunc("pal_tls_ctx_use_cert", irtypes.I32,
			ir.NewParam("ctx", irtypes.I64), ir.NewParam("pem", irtypes.I8Ptr),
			ir.NewParam("len", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		entry := fn.NewBlock(".entry")
		isZero := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		failBlk := fn.NewBlock(".fail")
		body := fn.NewBlock(".body")
		entry.NewCondBr(isZero, failBlk, body)
		failBlk.NewRet(i32(0))

		c := body.NewBitCast(body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr), ctxPtrTy)
		cert := body.NewCall(tlsImport, fn.Params[1], fn.Params[2], i32(stItemTypeCertific))
		certNull := body.NewICmp(enum.IPredEQ, cert, null)
		haveCert := fn.NewBlock(".have_cert")
		body.NewCondBr(certNull, failBlk, haveCert)

		oldCertP := fld(haveCert, ctxTy, c, 5)
		oldCert := haveCert.NewLoad(irtypes.I8Ptr, oldCertP)
		oldCertNN := haveCert.NewICmp(enum.IPredNE, oldCert, null)
		relCert := fn.NewBlock(".rel_old_cert")
		afterCert := fn.NewBlock(".after_old_cert")
		haveCert.NewCondBr(oldCertNN, relCert, afterCert)
		relCert.NewCall(cfRelease, oldCert)
		relCert.NewBr(afterCert)

		// A previously built identity is now stale — drop it so the next use_key
		// rebuilds against this certificate.
		oldIdP := fld(afterCert, ctxTy, c, 4)
		oldId := afterCert.NewLoad(irtypes.I8Ptr, oldIdP)
		oldIdNN := afterCert.NewICmp(enum.IPredNE, oldId, null)
		relId := fn.NewBlock(".rel_old_identity")
		afterId := fn.NewBlock(".after_old_identity")
		afterCert.NewCondBr(oldIdNN, relId, afterId)
		relId.NewCall(cfRelease, oldId)
		relId.NewStore(null, oldIdP)
		relId.NewBr(afterId)

		afterId.NewStore(cert, oldCertP)
		afterId.NewRet(i32(1))
		emit(fn)
	}

	// --- pal_tls_ctx_use_key(i64 ctx, i8* pem, i64 len) → i32 -------------
	// Imports the private key and pairs it with the staged certificate into a
	// transient SecIdentityRef (no keychain). Must follow use_cert.
	{
		fn := module.NewFunc("pal_tls_ctx_use_key", irtypes.I32,
			ir.NewParam("ctx", irtypes.I64), ir.NewParam("pem", irtypes.I8Ptr),
			ir.NewParam("len", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		entry := fn.NewBlock(".entry")
		isZero := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		failBlk := fn.NewBlock(".fail")
		body := fn.NewBlock(".body")
		entry.NewCondBr(isZero, failBlk, body)
		failBlk.NewRet(i32(0))

		c := body.NewBitCast(body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr), ctxPtrTy)
		certP := fld(body, ctxTy, c, 5)
		cert := body.NewLoad(irtypes.I8Ptr, certP)
		noCert := body.NewICmp(enum.IPredEQ, cert, null)
		haveCert := fn.NewBlock(".have_cert")
		body.NewCondBr(noCert, failBlk, haveCert)

		key := haveCert.NewCall(tlsImport, fn.Params[1], fn.Params[2], i32(stItemTypePrivKey))
		keyNull := haveCert.NewICmp(enum.IPredEQ, key, null)
		haveKey := fn.NewBlock(".have_key")
		haveCert.NewCondBr(keyNull, failBlk, haveKey)

		ident := haveKey.NewCall(secIdentityCreate, null, cert, key)
		haveKey.NewCall(cfRelease, key) // the identity retains the key it needs
		identNull := haveKey.NewICmp(enum.IPredEQ, ident, null)
		haveIdent := fn.NewBlock(".have_identity")
		haveKey.NewCondBr(identNull, failBlk, haveIdent)

		identP := fld(haveIdent, ctxTy, c, 4)
		oldId := haveIdent.NewLoad(irtypes.I8Ptr, identP)
		oldIdNN := haveIdent.NewICmp(enum.IPredNE, oldId, null)
		relOld := fn.NewBlock(".rel_old")
		storeId := fn.NewBlock(".store_identity")
		haveIdent.NewCondBr(oldIdNN, relOld, storeId)
		relOld.NewCall(cfRelease, oldId)
		relOld.NewBr(storeId)
		storeId.NewStore(ident, identP)
		storeId.NewRet(i32(1))
		emit(fn)
	}

	// --- pal_tls_ctx_load_default_trust(i64 ctx) → i32 --------------------
	// The system trust store is intrinsic on macOS: SecTrustEvaluateWithError
	// consults it by default, so there is nothing to load and no bundle path to
	// probe (unlike a static musl binary on Linux). Always succeeds.
	{
		fn := module.NewFunc("pal_tls_ctx_load_default_trust", irtypes.I32,
			ir.NewParam("ctx", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		b.NewRet(i32(1))
		emit(fn)
	}

	// --- pal_tls_new(i64 ctx) → i64 --------------------------------------
	// Allocates the session record, creates the SSLContextRef for the context's
	// side, and wires the memory-buffer I/O callbacks.
	{
		fn := module.NewFunc("pal_tls_new", irtypes.I64, ir.NewParam("ctx", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		entry := fn.NewBlock(".entry")
		identSlot := entry.NewAlloca(irtypes.I8Ptr)
		isZero := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		failBlk := fn.NewBlock(".fail")
		body := fn.NewBlock(".body")
		entry.NewCondBr(isZero, failBlk, body)
		failBlk.NewRet(i64(0))

		cRaw := body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		c := body.NewBitCast(cRaw, ctxPtrTy)
		raw := body.NewCall(palAlloc, sizeOf(sessTy))
		rawNull := body.NewICmp(enum.IPredEQ, raw, null)
		init := fn.NewBlock(".init")
		body.NewCondBr(rawNull, failBlk, init)

		s := init.NewBitCast(raw, sessPtrTy)
		init.NewStore(null, fld(init, sessTy, s, 0))
		for _, qIdx := range []int64{1, 2} {
			qp := fld(init, sessTy, s, qIdx)
			init.NewStore(null, fld(init, qTy, qp, 0))
			init.NewStore(i64(0), fld(init, qTy, qp, 1))
			init.NewStore(i64(0), fld(init, qTy, qp, 2))
			init.NewStore(i64(0), fld(init, qTy, qp, 3))
		}
		init.NewStore(cRaw, fld(init, sessTy, s, 3))
		init.NewStore(i32(0), fld(init, sessTy, s, 4))
		init.NewStore(i8(0), init.NewGetElementPtr(sessTy, s, i32(0), i32(5), i32(0)))

		side := init.NewLoad(irtypes.I32, fld(init, ctxTy, c, 0))
		ssl := init.NewCall(sslCreateContext, null, side, i32(stStreamType))
		sslNull := init.NewICmp(enum.IPredEQ, ssl, null)
		freeSess := fn.NewBlock(".free_sess")
		haveSSL := fn.NewBlock(".have_ssl")
		init.NewCondBr(sslNull, freeSess, haveSSL)
		freeSess.NewCall(palFree, raw)
		freeSess.NewRet(i64(0))

		haveSSL.NewStore(ssl, fld(haveSSL, sessTy, s, 0))
		haveSSL.NewCall(sslSetIOFuncs, ssl,
			haveSSL.NewBitCast(readCB, irtypes.I8Ptr),
			haveSSL.NewBitCast(writeCB, irtypes.I8Ptr))
		haveSSL.NewCall(sslSetConnection, ssl, raw)
		minVer := haveSSL.NewLoad(irtypes.I32, fld(haveSSL, ctxTy, c, 2))
		haveSSL.NewCall(sslSetProtocolVersionMin, ssl, minVer)

		// Install the identity built from cert+key, if any. This is the server's
		// certificate chain; on a client it is the mutual-TLS client certificate
		// (set_client_certificate) — Secure Transport uses the same setter for both.
		ident := haveSSL.NewLoad(irtypes.I8Ptr, fld(haveSSL, ctxTy, c, 4))
		identNN := haveSSL.NewICmp(enum.IPredNE, ident, null)
		setCert := fn.NewBlock(".set_cert")
		afterCert := fn.NewBlock(".after_cert")
		haveSSL.NewCondBr(identNN, setCert, afterCert)
		setCert.NewStore(ident, identSlot)
		chain := setCert.NewCall(cfArrayCreate, null,
			setCert.NewBitCast(identSlot, irtypes.I8Ptr), i64(1), cfTypeArrayCallBacks)
		chainNull := setCert.NewICmp(enum.IPredEQ, chain, null)
		applyCert := fn.NewBlock(".apply_cert")
		setCert.NewCondBr(chainNull, afterCert, applyCert)
		applyCert.NewCall(sslSetCertificate, ssl, chain)
		applyCert.NewCall(cfRelease, chain)
		applyCert.NewBr(afterCert)

		// Client side: break out of the handshake at peer authentication so we can
		// run trust evaluation ourselves (custom anchors, insecure mode).
		isClient := afterCert.NewICmp(enum.IPredEQ, side, i32(stClientSide))
		setOpt := fn.NewBlock(".set_break_opt")
		doneBlk := fn.NewBlock(".done")
		afterCert.NewCondBr(isClient, setOpt, doneBlk)
		setOpt.NewCall(sslSetSessionOption, ssl, i32(stOptBreakOnServerAuth), i8(1))
		setOpt.NewBr(doneBlk)

		doneBlk.NewRet(doneBlk.NewPtrToInt(raw, irtypes.I64))
		emit(fn)
	}

	// --- pal_tls_set_connect_state / _set_accept_state (i64 ssl) ----------
	// No-ops. Unlike OpenSSL, Secure Transport fixes the protocol side when the
	// SSLContextRef is created (SSLCreateContext takes kSSLClientSide/kSSLServerSide),
	// and pal_tls_new already took it from the owning context. Kept so the PAL
	// surface stays identical across backends.
	emitNoop := func(name string) *ir.Func {
		fn := module.NewFunc(name, irtypes.Void, ir.NewParam("ssl", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		b.NewRet(nil)
		return emit(fn)
	}
	emitNoop("pal_tls_set_connect_state")
	emitNoop("pal_tls_set_accept_state")

	// --- pal_tls_set_sni / _set_verify_host (i64 ssl, i8* host) → i32 -----
	// Both map onto SSLSetPeerDomainName: on Secure Transport the peer domain name
	// is simultaneously the SNI sent in the ClientHello and the hostname checked by
	// the SSL trust policy, so there is one setting and one implementation. tls.pr
	// passes the same hostname to both, so the second call is a harmless no-op.
	emitSetPeerName := func(name string) *ir.Func {
		fn := module.NewFunc(name, irtypes.I32,
			ir.NewParam("ssl", irtypes.I64), ir.NewParam("host", irtypes.I8Ptr))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		entry := fn.NewBlock(".entry")
		bad := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		hostNull := entry.NewICmp(enum.IPredEQ, fn.Params[1], null)
		skip := entry.NewOr(bad, hostNull)
		failBlk := fn.NewBlock(".fail")
		body := fn.NewBlock(".body")
		entry.NewCondBr(skip, failBlk, body)
		failBlk.NewRet(i32(0))

		s := body.NewBitCast(body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr), sessPtrTy)
		ssl := body.NewLoad(irtypes.I8Ptr, fld(body, sessTy, s, 0))
		n := body.NewCall(strlenFn, fn.Params[1])
		st := body.NewCall(sslSetPeerDomainName, ssl, fn.Params[1], n)
		ok := body.NewICmp(enum.IPredEQ, st, i32(stErrSuccess))
		body.NewRet(body.NewSelect(ok, i32(1), i32(0)))
		return emit(fn)
	}
	emitSetPeerName("pal_tls_set_sni")
	emitSetPeerName("pal_tls_set_verify_host")

	// --- pal_tls_do_handshake(i64 ssl) → i32 ------------------------------
	// 0 ok, 1 want_read, -1 fatal. Never returns 2 (want_write): the outbound
	// queue is unbounded, so write_cb cannot block.
	//
	// The loop exists solely for errSSLPeerAuthCompleted: with
	// BreakOnServerAuth set, SSLHandshake stops once for us to evaluate trust and
	// must then be called again. Every other status returns immediately, so this
	// iterates at most twice and cannot spin.
	{
		fn := module.NewFunc("pal_tls_do_handshake", irtypes.I32, ir.NewParam("ssl", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		entry := fn.NewBlock(".entry")
		isZero := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		fatalBlk := fn.NewBlock(".fatal")
		body := fn.NewBlock(".body")
		entry.NewCondBr(isZero, fatalBlk, body)
		fatalBlk.NewRet(i32(-1))

		s := body.NewBitCast(body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr), sessPtrTy)
		loopHead := fn.NewBlock(".loop")
		body.NewBr(loopHead)

		ssl := loopHead.NewLoad(irtypes.I8Ptr, fld(loopHead, sessTy, s, 0))
		st := loopHead.NewCall(sslHandshake, ssl)
		done := loopHead.NewICmp(enum.IPredEQ, st, i32(stErrSuccess))
		okBlk := fn.NewBlock(".ok")
		notDone := fn.NewBlock(".not_done")
		loopHead.NewCondBr(done, okBlk, notDone)
		okBlk.NewRet(i32(0))

		wouldBlock := notDone.NewICmp(enum.IPredEQ, st, i32(stErrWouldBlock))
		wantReadBlk := fn.NewBlock(".want_read")
		notBlocked := fn.NewBlock(".not_blocked")
		notDone.NewCondBr(wouldBlock, wantReadBlk, notBlocked)
		wantReadBlk.NewRet(i32(1))

		peerAuth := notBlocked.NewICmp(enum.IPredEQ, st, i32(stErrPeerAuthComplete))
		authBlk := fn.NewBlock(".peer_auth")
		notBlocked.NewCondBr(peerAuth, authBlk, fatalBlk)

		reject := authBlk.NewCall(checkTrust, s)
		rejected := authBlk.NewICmp(enum.IPredNE, reject, i32(0))
		certFail := fn.NewBlock(".cert_fail")
		authBlk.NewCondBr(rejected, certFail, loopHead)
		// A non-zero verify_result is what makes tls.pr report
		// TlsErrorKind.certificate rather than TlsErrorKind.handshake.
		certFail.NewStore(i32(1), fld(certFail, sessTy, s, 4))
		certFail.NewRet(i32(-1))
		emit(fn)
	}

	// --- pal_tls_read(i64 ssl, i8* buf, i64 len) → i64 --------------------
	// >0 bytes, 0 EOF (clean close_notify), -1 want_read, -3 fatal.
	{
		fn := module.NewFunc("pal_tls_read", irtypes.I64,
			ir.NewParam("ssl", irtypes.I64), ir.NewParam("buf", irtypes.I8Ptr),
			ir.NewParam("len", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		entry := fn.NewBlock(".entry")
		nSlot := entry.NewAlloca(irtypes.I64)
		entry.NewStore(i64(0), nSlot)
		isZero := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		fatalBlk := fn.NewBlock(".fatal")
		body := fn.NewBlock(".body")
		entry.NewCondBr(isZero, fatalBlk, body)
		fatalBlk.NewRet(i64(-3))

		s := body.NewBitCast(body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr), sessPtrTy)
		ssl := body.NewLoad(irtypes.I8Ptr, fld(body, sessTy, s, 0))
		st := body.NewCall(sslReadFn, ssl, fn.Params[1], fn.Params[2], nSlot)
		got := body.NewLoad(irtypes.I64, nSlot)
		ok := body.NewICmp(enum.IPredEQ, st, i32(stErrSuccess))
		dataBlk := fn.NewBlock(".data")
		notOK := fn.NewBlock(".not_ok")
		body.NewCondBr(ok, dataBlk, notOK)
		dataBlk.NewRet(got)

		// A short read that also reports wouldBlock still delivered `got` bytes.
		wouldBlock := notOK.NewICmp(enum.IPredEQ, st, i32(stErrWouldBlock))
		blockBlk := fn.NewBlock(".would_block")
		notBlocked := fn.NewBlock(".not_blocked")
		notOK.NewCondBr(wouldBlock, blockBlk, notBlocked)
		hasData := blockBlk.NewICmp(enum.IPredSGT, got, i64(0))
		blockBlk.NewRet(blockBlk.NewSelect(hasData, got, i64(-1)))

		closed := notBlocked.NewICmp(enum.IPredEQ, st, i32(stErrClosedGraceful))
		eofBlk := fn.NewBlock(".eof")
		notBlocked.NewCondBr(closed, eofBlk, fatalBlk)
		eofBlk.NewRet(i64(0))
		emit(fn)
	}

	// --- pal_tls_write(i64 ssl, i8* buf, i64 len) → i64 -------------------
	// >0 bytes, -1 want_read, -3 fatal.
	{
		fn := module.NewFunc("pal_tls_write", irtypes.I64,
			ir.NewParam("ssl", irtypes.I64), ir.NewParam("buf", irtypes.I8Ptr),
			ir.NewParam("len", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		entry := fn.NewBlock(".entry")
		nSlot := entry.NewAlloca(irtypes.I64)
		entry.NewStore(i64(0), nSlot)
		isZero := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		fatalBlk := fn.NewBlock(".fatal")
		body := fn.NewBlock(".body")
		entry.NewCondBr(isZero, fatalBlk, body)
		fatalBlk.NewRet(i64(-3))

		s := body.NewBitCast(body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr), sessPtrTy)
		ssl := body.NewLoad(irtypes.I8Ptr, fld(body, sessTy, s, 0))
		st := body.NewCall(sslWriteFn, ssl, fn.Params[1], fn.Params[2], nSlot)
		got := body.NewLoad(irtypes.I64, nSlot)
		ok := body.NewICmp(enum.IPredEQ, st, i32(stErrSuccess))
		dataBlk := fn.NewBlock(".data")
		notOK := fn.NewBlock(".not_ok")
		body.NewCondBr(ok, dataBlk, notOK)
		dataBlk.NewRet(got)

		wouldBlock := notOK.NewICmp(enum.IPredEQ, st, i32(stErrWouldBlock))
		blockBlk := fn.NewBlock(".would_block")
		notOK.NewCondBr(wouldBlock, blockBlk, fatalBlk)
		hasData := blockBlk.NewICmp(enum.IPredSGT, got, i64(0))
		blockBlk.NewRet(blockBlk.NewSelect(hasData, got, i64(-1)))
		emit(fn)
	}

	// --- pal_tls_shutdown(i64 ssl) → i32 ----------------------------------
	// SSLClose queues close_notify into the outbound queue; tls.pr flushes it.
	{
		fn := module.NewFunc("pal_tls_shutdown", irtypes.I32, ir.NewParam("ssl", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		entry := fn.NewBlock(".entry")
		isZero := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		failBlk := fn.NewBlock(".fail")
		body := fn.NewBlock(".body")
		entry.NewCondBr(isZero, failBlk, body)
		failBlk.NewRet(i32(-1))
		s := body.NewBitCast(body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr), sessPtrTy)
		ssl := body.NewLoad(irtypes.I8Ptr, fld(body, sessTy, s, 0))
		st := body.NewCall(sslClose, ssl)
		ok := body.NewICmp(enum.IPredEQ, st, i32(stErrSuccess))
		body.NewRet(body.NewSelect(ok, i32(0), i32(-1)))
		emit(fn)
	}

	// --- queue-facing entry points ----------------------------------------
	// These stand in for the OpenSSL memory BIOs; the names are kept identical
	// across backends so tls.pr and the codegen bridge are shared.

	// pal_tls_bio_read_out(i64 ssl, i8* buf, i64 len) → i64
	{
		fn := module.NewFunc("pal_tls_bio_read_out", irtypes.I64,
			ir.NewParam("ssl", irtypes.I64), ir.NewParam("buf", irtypes.I8Ptr),
			ir.NewParam("len", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		entry := fn.NewBlock(".entry")
		isZero := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		zeroBlk := fn.NewBlock(".none")
		body := fn.NewBlock(".body")
		entry.NewCondBr(isZero, zeroBlk, body)
		zeroBlk.NewRet(i64(0))
		s := body.NewBitCast(body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr), sessPtrTy)
		body.NewRet(body.NewCall(qTake, fld(body, sessTy, s, 2), fn.Params[1], fn.Params[2]))
		emit(fn)
	}

	// pal_tls_bio_write_in(i64 ssl, i8* buf, i64 len) → i64
	{
		fn := module.NewFunc("pal_tls_bio_write_in", irtypes.I64,
			ir.NewParam("ssl", irtypes.I64), ir.NewParam("buf", irtypes.I8Ptr),
			ir.NewParam("len", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		entry := fn.NewBlock(".entry")
		isZero := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		zeroBlk := fn.NewBlock(".none")
		body := fn.NewBlock(".body")
		entry.NewCondBr(isZero, zeroBlk, body)
		zeroBlk.NewRet(i64(0))
		s := body.NewBitCast(body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr), sessPtrTy)
		body.NewCall(qAppend, fld(body, sessTy, s, 1), fn.Params[1], fn.Params[2])
		body.NewRet(fn.Params[2])
		emit(fn)
	}

	// pal_tls_bio_pending_out(i64 ssl) → i64
	{
		fn := module.NewFunc("pal_tls_bio_pending_out", irtypes.I64, ir.NewParam("ssl", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		entry := fn.NewBlock(".entry")
		isZero := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		zeroBlk := fn.NewBlock(".none")
		body := fn.NewBlock(".body")
		entry.NewCondBr(isZero, zeroBlk, body)
		zeroBlk.NewRet(i64(0))
		s := body.NewBitCast(body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr), sessPtrTy)
		qp := fld(body, sessTy, s, 2)
		head := body.NewLoad(irtypes.I64, fld(body, qTy, qp, 2))
		tail := body.NewLoad(irtypes.I64, fld(body, qTy, qp, 3))
		body.NewRet(body.NewSub(tail, head))
		emit(fn)
	}

	// --- pal_tls_get_version(i64 ssl) → i8* -------------------------------
	// Returns a static const char*, matching SSL_get_version's contract on Linux.
	{
		fn := module.NewFunc("pal_tls_get_version", irtypes.I8Ptr, ir.NewParam("ssl", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		empty := tlsCStr(module, "__promise_tls_dw_ver_none", "")
		v13 := tlsCStr(module, "__promise_tls_dw_ver_13", "TLSv1.3")
		v12 := tlsCStr(module, "__promise_tls_dw_ver_12", "TLSv1.2")
		v11 := tlsCStr(module, "__promise_tls_dw_ver_11", "TLSv1.1")
		v10 := tlsCStr(module, "__promise_tls_dw_ver_10", "TLSv1")

		entry := fn.NewBlock(".entry")
		vSlot := entry.NewAlloca(irtypes.I32)
		entry.NewStore(i32(0), vSlot)
		isZero := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		noneBlk := fn.NewBlock(".none")
		body := fn.NewBlock(".body")
		entry.NewCondBr(isZero, noneBlk, body)
		noneBlk.NewRet(empty)

		s := body.NewBitCast(body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr), sessPtrTy)
		ssl := body.NewLoad(irtypes.I8Ptr, fld(body, sessTy, s, 0))
		body.NewCall(sslGetNegotiatedProtocolVersion, ssl, vSlot)
		v := body.NewLoad(irtypes.I32, vSlot)
		b13 := fn.NewBlock(".v13")
		b12 := fn.NewBlock(".v12")
		b11 := fn.NewBlock(".v11")
		b10 := fn.NewBlock(".v10")
		b13.NewRet(v13)
		b12.NewRet(v12)
		b11.NewRet(v11)
		b10.NewRet(v10)
		body.NewSwitch(v, noneBlk,
			ir.NewCase(i32(stProtoTLS13), b13),
			ir.NewCase(i32(stProtoTLS12), b12),
			ir.NewCase(i32(stProtoTLS11), b11),
			ir.NewCase(i32(stProtoTLS1), b10))
		emit(fn)
	}

	// --- pal_tls_get_cipher(i64 ssl) → i8* --------------------------------
	// SSLGetNegotiatedCipher yields a raw 16-bit suite number, so it is mapped to
	// a name here. Unknown suites render as "0xXXXX" into the session's own
	// buffer — per-session, so concurrent streams cannot race on it, and the
	// getter never returns an empty string for an established connection.
	{
		fn := module.NewFunc("pal_tls_get_cipher", irtypes.I8Ptr, ir.NewParam("ssl", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		empty := tlsCStr(module, "__promise_tls_dw_cipher_none", "")
		hexDigits := tlsCStr(module, "__promise_tls_dw_hex", "0123456789abcdef")

		entry := fn.NewBlock(".entry")
		csSlot := entry.NewAlloca(irtypes.I32)
		entry.NewStore(i32(0), csSlot)
		isZero := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		noneBlk := fn.NewBlock(".none")
		body := fn.NewBlock(".body")
		entry.NewCondBr(isZero, noneBlk, body)
		noneBlk.NewRet(empty)

		s := body.NewBitCast(body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr), sessPtrTy)
		ssl := body.NewLoad(irtypes.I8Ptr, fld(body, sessTy, s, 0))
		body.NewCall(sslGetNegotiatedCipher, ssl, csSlot)
		cs := body.NewLoad(irtypes.I32, csSlot)

		hexBlk := fn.NewBlock(".hex")
		cases := make([]*ir.Case, 0, len(tlsDarwinCipherNames))
		for idx, cn := range tlsDarwinCipherNames {
			nb := fn.NewBlock(".cipher_" + itoa(idx))
			nb.NewRet(tlsCStr(module, "__promise_tls_dw_cipher_"+itoa(idx), cn.name))
			cases = append(cases, ir.NewCase(i32(cn.suite), nb))
		}
		body.NewSwitch(cs, hexBlk, cases...)

		// "0xXXXX\0" into the session buffer.
		buf := hexBlk.NewGetElementPtr(sessTy, s, i32(0), i32(5), i32(0))
		hexBlk.NewStore(i8('0'), hexBlk.NewGetElementPtr(irtypes.I8, buf, i64(0)))
		hexBlk.NewStore(i8('x'), hexBlk.NewGetElementPtr(irtypes.I8, buf, i64(1)))
		for pos := int64(0); pos < 4; pos++ {
			shift := (3 - pos) * 4
			nib := hexBlk.NewAnd(hexBlk.NewLShr(cs, i32(shift)), i32(15))
			ch := hexBlk.NewLoad(irtypes.I8,
				hexBlk.NewGetElementPtr(irtypes.I8, hexDigits, hexBlk.NewZExt(nib, irtypes.I64)))
			hexBlk.NewStore(ch, hexBlk.NewGetElementPtr(irtypes.I8, buf, i64(2+pos)))
		}
		hexBlk.NewStore(i8(0), hexBlk.NewGetElementPtr(irtypes.I8, buf, i64(6)))
		hexBlk.NewRet(buf)
		emit(fn)
	}

	// --- pal_tls_get_verify_result(i64 ssl) → i32 -------------------------
	// 0 means the peer certificate verified (or verification was not requested).
	{
		fn := module.NewFunc("pal_tls_get_verify_result", irtypes.I32, ir.NewParam("ssl", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		entry := fn.NewBlock(".entry")
		isZero := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		zeroBlk := fn.NewBlock(".none")
		body := fn.NewBlock(".body")
		entry.NewCondBr(isZero, zeroBlk, body)
		zeroBlk.NewRet(i32(0))
		s := body.NewBitCast(body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr), sessPtrTy)
		body.NewRet(body.NewLoad(irtypes.I32, fld(body, sessTy, s, 4)))
		emit(fn)
	}

	// --- pal_tls_free(i64 ssl) --------------------------------------------
	// Releases the SSLContextRef and both queue buffers. The owning tls_ctx is
	// borrowed, not owned, so it is left alone (pal_tls_ctx_free frees it).
	{
		fn := module.NewFunc("pal_tls_free", irtypes.Void, ir.NewParam("ssl", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		entry := fn.NewBlock(".entry")
		isZero := entry.NewICmp(enum.IPredEQ, fn.Params[0], i64(0))
		retBlk := fn.NewBlock(".ret")
		body := fn.NewBlock(".body")
		entry.NewCondBr(isZero, retBlk, body)
		retBlk.NewRet(nil)

		raw := body.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		s := body.NewBitCast(raw, sessPtrTy)
		sslP := fld(body, sessTy, s, 0)
		ssl := body.NewLoad(irtypes.I8Ptr, sslP)
		sslNN := body.NewICmp(enum.IPredNE, ssl, null)
		relBlk := fn.NewBlock(".rel_ssl")
		freeBlk := fn.NewBlock(".free_queues")
		body.NewCondBr(sslNN, relBlk, freeBlk)
		relBlk.NewCall(cfRelease, ssl)
		relBlk.NewStore(null, sslP)
		relBlk.NewBr(freeBlk)

		freeBlk.NewCall(qFree, fld(freeBlk, sessTy, s, 1))
		freeBlk.NewCall(qFree, fld(freeBlk, sessTy, s, 2))
		freeBlk.NewCall(palFree, raw)
		freeBlk.NewRet(nil)
		emit(fn)
	}

	return fns
}
