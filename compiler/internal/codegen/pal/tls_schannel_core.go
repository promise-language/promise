package pal

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// tls_schannel_core.go — internal helpers of the SChannel TLS backend (T1598):
// the shared byte queue, PEM/UTF-16 conversion, lazy credential acquisition, the
// single handshake step both do_handshake and shutdown drive, and peer
// certificate validation. See tls_schannel.go for the ABI constants and layouts.

// tlsWinSizeOf yields sizeof(t) as an i64 constant expression
// (`ptrtoint (getelementptr %t, %t* null, i32 1) to i64`).
func tlsWinSizeOf(t irtypes.Type) constant.Constant {
	g := constant.NewGetElementPtr(t, constant.NewNull(irtypes.NewPointer(t)), i32c(1))
	return constant.NewPtrToInt(g, irtypes.I64)
}

// --- growable byte queue ---------------------------------------------------
// One primitive backs all three per-session queues (inbound ciphertext,
// outbound ciphertext, decrypted-but-undelivered plaintext). Valid bytes are
// [off, len): consuming a prefix only advances the read cursor, so taking a
// queue apart one byte at a time (which is exactly what TlsStream.read_line
// does) costs O(1) per byte rather than memmoving the remainder down each time.
// The cursor is folded back to 0 when the queue drains and when a new append
// needs the space, so it never turns into unbounded slack.

// emitBufHelpers defines __pal_tls_buf_append / _take / _consume / _free.
func (e *tlsWinEmitter) emitBufHelpers() {
	i8p := irtypes.I8Ptr

	// void @__pal_tls_buf_consume(i8* %b, i64 %n) — n is always ≤ avail.
	{
		fn := e.newFn("__pal_tls_buf_consume", irtypes.Void,
			ir.NewParam("b", i8p), ir.NewParam("n", irtypes.I64))
		b := fn.NewBlock(".entry")
		bp := b.NewBitCast(fn.Params[0], e.t.bufP)
		lenP := e.field(b, e.t.buf, bp, winBufFLen)
		offP := e.field(b, e.t.buf, bp, winBufFOff)
		off := b.NewAdd(b.NewLoad(irtypes.I64, offP), fn.Params[1])
		b.NewStore(off, offP)
		drained := b.NewICmp(enum.IPredUGE, off, b.NewLoad(irtypes.I64, lenP))
		resetBlk := fn.NewBlock(".reset")
		doneBlk := fn.NewBlock(".done")
		b.NewCondBr(drained, resetBlk, doneBlk)

		// Empty again — fold the cursor back so the full capacity stays usable.
		resetBlk.NewStore(i64c(0), lenP)
		resetBlk.NewStore(i64c(0), offP)
		resetBlk.NewBr(doneBlk)
		doneBlk.NewRet(nil)
		e.bufConsume = fn
	}

	// void @__pal_tls_buf_append(i8* %b, i8* %data, i64 %n)
	{
		fn := e.newFn("__pal_tls_buf_append", irtypes.Void,
			ir.NewParam("b", i8p), ir.NewParam("data", i8p), ir.NewParam("n", irtypes.I64))
		b := fn.NewBlock(".entry")
		bp := b.NewBitCast(fn.Params[0], e.t.bufP)
		ptrP := e.field(b, e.t.buf, bp, winBufFPtr)
		lenP := e.field(b, e.t.buf, bp, winBufFLen)
		capP := e.field(b, e.t.buf, bp, winBufFCap)
		offP := e.field(b, e.t.buf, bp, winBufFOff)

		// Reclaim the consumed prefix first. Appends happen once per record (or
		// per socket read), so this memmove is amortized over a whole record —
		// unlike the per-byte one it replaces.
		off := b.NewLoad(irtypes.I64, offP)
		compactBlk := fn.NewBlock(".compact")
		readyBlk := fn.NewBlock(".ready")
		b.NewCondBr(b.NewICmp(enum.IPredUGT, off, i64c(0)), compactBlk, readyBlk)

		ptr0 := compactBlk.NewLoad(i8p, ptrP)
		rem := compactBlk.NewSub(compactBlk.NewLoad(irtypes.I64, lenP), off)
		compactBlk.NewCall(e.memmove, ptr0,
			compactBlk.NewGetElementPtr(irtypes.I8, ptr0, off), rem)
		compactBlk.NewStore(rem, lenP)
		compactBlk.NewStore(i64c(0), offP)
		compactBlk.NewBr(readyBlk)

		length := readyBlk.NewLoad(irtypes.I64, lenP)
		capacity := readyBlk.NewLoad(irtypes.I64, capP)
		need := readyBlk.NewAdd(length, fn.Params[2])
		grow := readyBlk.NewICmp(enum.IPredUGT, need, capacity)
		growBlk := fn.NewBlock(".grow")
		copyBlk := fn.NewBlock(".copy")
		readyBlk.NewCondBr(grow, growBlk, copyBlk)

		// newCap = max(cap*2, need). cap is never 0 — pal_tls_new seeds every
		// queue with a real allocation, so pal_realloc always sees a live block.
		dbl := growBlk.NewShl(capacity, i64c(1))
		useNeed := growBlk.NewICmp(enum.IPredUGT, need, dbl)
		newCap := growBlk.NewSelect(useNeed, need, dbl)
		old := growBlk.NewLoad(i8p, ptrP)
		fresh := growBlk.NewCall(e.realloc, old, newCap)
		growBlk.NewStore(fresh, ptrP)
		growBlk.NewStore(newCap, capP)
		growBlk.NewBr(copyBlk)

		ptr := copyBlk.NewLoad(i8p, ptrP)
		dst := copyBlk.NewGetElementPtr(irtypes.I8, ptr, length)
		copyBlk.NewCall(e.memcpy, dst, fn.Params[1], fn.Params[2])
		copyBlk.NewStore(need, lenP)
		copyBlk.NewRet(nil)
		e.bufAppend = fn
	}

	// i64 @__pal_tls_buf_take(i8* %b, i8* %dst, i64 %max)
	{
		fn := e.newFn("__pal_tls_buf_take", irtypes.I64,
			ir.NewParam("b", i8p), ir.NewParam("dst", i8p), ir.NewParam("max", irtypes.I64))
		b := fn.NewBlock(".entry")
		bp := b.NewBitCast(fn.Params[0], e.t.bufP)
		avail := e.bufAvail(b, bp)
		smaller := b.NewICmp(enum.IPredULT, fn.Params[2], avail)
		n := b.NewSelect(smaller, fn.Params[2], avail)
		some := b.NewICmp(enum.IPredUGT, n, i64c(0))
		copyBlk := fn.NewBlock(".copy")
		doneBlk := fn.NewBlock(".done")
		b.NewCondBr(some, copyBlk, doneBlk)

		copyBlk.NewCall(e.memcpy, fn.Params[1], e.bufData(copyBlk, bp), n)
		copyBlk.NewCall(e.bufConsume, fn.Params[0], n)
		copyBlk.NewBr(doneBlk)

		doneBlk.NewRet(n)
		e.bufTake = fn
	}

	// void @__pal_tls_buf_free(i8* %b)
	{
		fn := e.newFn("__pal_tls_buf_free", irtypes.Void, ir.NewParam("b", i8p))
		b := fn.NewBlock(".entry")
		bp := b.NewBitCast(fn.Params[0], e.t.bufP)
		ptrP := e.field(b, e.t.buf, bp, winBufFPtr)
		ptr := b.NewLoad(i8p, ptrP)
		have := e.notNull(b, ptr)
		freeBlk := fn.NewBlock(".free")
		doneBlk := fn.NewBlock(".done")
		b.NewCondBr(have, freeBlk, doneBlk)
		freeBlk.NewCall(e.free, ptr)
		freeBlk.NewBr(doneBlk)
		doneBlk.NewStore(tlsWinNull, ptrP)
		doneBlk.NewStore(i64c(0), e.field(doneBlk, e.t.buf, bp, winBufFLen))
		doneBlk.NewStore(i64c(0), e.field(doneBlk, e.t.buf, bp, winBufFCap))
		doneBlk.NewStore(i64c(0), e.field(doneBlk, e.t.buf, bp, winBufFOff))
		doneBlk.NewRet(nil)
		e.bufFree = fn
	}
}

// bufAvail returns the number of valid bytes in the queue at bp (len - off).
func (e *tlsWinEmitter) bufAvail(b *ir.Block, bp value.Value) value.Value {
	return b.NewSub(
		b.NewLoad(irtypes.I64, e.field(b, e.t.buf, bp, winBufFLen)),
		b.NewLoad(irtypes.I64, e.field(b, e.t.buf, bp, winBufFOff)))
}

// bufData returns a pointer to the queue's first valid byte (ptr + off).
func (e *tlsWinEmitter) bufData(b *ir.Block, bp value.Value) value.Value {
	return b.NewGetElementPtr(irtypes.I8,
		b.NewLoad(irtypes.I8Ptr, e.field(b, e.t.buf, bp, winBufFPtr)),
		b.NewLoad(irtypes.I64, e.field(b, e.t.buf, bp, winBufFOff)))
}

// bufReset empties the queue at bp without touching its allocation.
func (e *tlsWinEmitter) bufReset(b *ir.Block, bp value.Value) {
	b.NewStore(i64c(0), e.field(b, e.t.buf, bp, winBufFLen))
	b.NewStore(i64c(0), e.field(b, e.t.buf, bp, winBufFOff))
}

// sessBuf returns an i8* to one of the session's three byte queues.
func (e *tlsWinEmitter) sessBuf(b *ir.Block, s value.Value, idx int) value.Value {
	return e.i8ptr(b, e.field(b, e.t.sess, s, idx))
}

// --- PEM / string helpers --------------------------------------------------

// emitPemDER defines i8* @__pal_tls_pem_der(i8* pem, i64 len, i64* outLen):
// decodes a PEM block (BEGIN/END armour + base64 body) into a pal_alloc'd DER
// buffer. Returns null when the input is not a well-formed PEM block, which is
// what lets tls.pr surface TlsErrorKind.certificate for malformed input.
func (e *tlsWinEmitter) emitPemDER() {
	i8p := irtypes.I8Ptr
	nullI32P := constant.NewNull(irtypes.NewPointer(irtypes.I32))
	fn := e.newFn("__pal_tls_pem_der", i8p,
		ir.NewParam("pem", i8p), ir.NewParam("len", irtypes.I64),
		ir.NewParam("outLen", irtypes.NewPointer(irtypes.I64)))
	b := fn.NewBlock(".entry")
	cb := b.NewAlloca(irtypes.I32)
	b.NewStore(i32c(0), cb)
	// The Promise u8[] is not NUL-terminated, so the length is always explicit.
	cch := b.NewTrunc(fn.Params[1], irtypes.I32)

	ok1 := b.NewCall(e.strToBin, fn.Params[0], cch, i32c(winCryptStringBase64Header),
		tlsWinNull, cb, nullI32P, nullI32P)
	failBlk := fn.NewBlock(".fail")
	sizedBlk := fn.NewBlock(".sized")
	b.NewCondBr(b.NewICmp(enum.IPredNE, ok1, i32c(0)), sizedBlk, failBlk)
	failBlk.NewRet(tlsWinNull)

	n := sizedBlk.NewLoad(irtypes.I32, cb)
	empty := sizedBlk.NewICmp(enum.IPredEQ, n, i32c(0))
	decodeBlk := fn.NewBlock(".decode")
	sizedBlk.NewCondBr(empty, failBlk, decodeBlk)

	der := decodeBlk.NewCall(e.alloc, decodeBlk.NewZExt(n, irtypes.I64))
	ok2 := decodeBlk.NewCall(e.strToBin, fn.Params[0], cch, i32c(winCryptStringBase64Header),
		der, cb, nullI32P, nullI32P)
	undoBlk := fn.NewBlock(".decode_failed")
	okBlk := fn.NewBlock(".ok")
	decodeBlk.NewCondBr(decodeBlk.NewICmp(enum.IPredNE, ok2, i32c(0)), okBlk, undoBlk)

	undoBlk.NewCall(e.free, der)
	undoBlk.NewRet(tlsWinNull)

	final := okBlk.NewLoad(irtypes.I32, cb)
	okBlk.NewStore(okBlk.NewZExt(final, irtypes.I64), fn.Params[2])
	okBlk.NewRet(der)
	e.pemDER = fn
}

// emitWiden defines i8* @__pal_tls_widen(i8* s) — UTF-8 → NUL-terminated UTF-16
// in a pal_alloc'd buffer (null on failure). Needed for the SSL chain-policy
// server name, the only wide string this backend builds at runtime.
func (e *tlsWinEmitter) emitWiden() {
	i8p := irtypes.I8Ptr
	fn := e.newFn("__pal_tls_widen", i8p, ir.NewParam("s", i8p))
	b := fn.NewBlock(".entry")
	nullBlk := fn.NewBlock(".null")
	sizeBlk := fn.NewBlock(".size")
	b.NewCondBr(e.notNull(b, fn.Params[0]), sizeBlk, nullBlk)
	nullBlk.NewRet(tlsWinNull)

	// srcLen -1 → the count includes the terminating NUL.
	n := sizeBlk.NewCall(e.mb2wc, i32c(winCPUTF8), i32c(0), fn.Params[0], i32c(-1),
		tlsWinNull, i32c(0))
	bad := sizeBlk.NewICmp(enum.IPredSLE, n, i32c(0))
	convBlk := fn.NewBlock(".convert")
	sizeBlk.NewCondBr(bad, nullBlk, convBlk)

	bytes := convBlk.NewMul(convBlk.NewSExt(n, irtypes.I64), i64c(2))
	w := convBlk.NewCall(e.alloc, bytes)
	convBlk.NewCall(e.mb2wc, i32c(winCPUTF8), i32c(0), fn.Params[0], i32c(-1), w, n)
	convBlk.NewRet(w)
	e.widen = fn
}

// emitEnsureCred defines i32 @__pal_tls_ensure_cred(i64 ctx) — 1 ok, 0 failure.
//
// AcquireCredentialsHandle is deliberately lazy: tls.pr configures verification,
// the minimum version, and the certificate/key *after* creating the config, so
// the credential can only be built once the first session is created. It is
// acquired at most once per TlsConfig / TlsListener and shared by every session.
//
// That makes the first session a snapshot point for the *credential's* settings
// (certificate, key, protocol floor): changing them afterwards does not affect
// later sessions, because SChannel requires the credential to outlive every
// context created from it, so it cannot be rebuilt while sessions are live.
// Trust anchors are unaffected — add_root_certificate feeds __pal_tls_verify,
// which reads the store at handshake time.
//
// One context serves every connection goroutine (see the Concurrency note in
// tls_schannel.go), so this check-then-acquire is serialized on the context's
// CRITICAL_SECTION: unsynchronized, two goroutines could both find cred_valid
// clear and both AcquireCredentialsHandle into the same &ctx->cred, leaking the
// loser's credential and handing an in-flight handshake a CredHandle its
// security context was not created from (T1766). The lock is taken *before* the
// check rather than guarding a double-checked fast path, because the
// acquire/release pair is also what orders the ctx->cred writes against a
// goroutine that reads them after migrating to another M; an uncontended
// EnterCriticalSection is one interlocked op against an SSPI round costing
// microseconds — and only the handshake reaches here at all, never the
// encrypt/decrypt data path. Two rules follow for anything edited in between:
// every block that returns must leave the section, and nothing may suspend the
// goroutine while it is held, because a CRITICAL_SECTION is owned by the OS
// thread that entered it and a resumed goroutine can be running on another M.
func (e *tlsWinEmitter) emitEnsureCred() {
	fn := e.newFn("__pal_tls_ensure_cred", irtypes.I32, ir.NewParam("ctx", irtypes.I64))
	b := fn.NewBlock(".entry")
	c := b.NewIntToPtr(fn.Params[0], e.t.ctxP)
	lock := e.credLock(b, c)
	b.NewCall(e.csEnter, lock)
	validP := e.field(b, e.t.ctx, c, winCtxFCredValid)
	already := b.NewICmp(enum.IPredNE, b.NewLoad(irtypes.I32, validP), i32c(0))
	okBlk := fn.NewBlock(".already")
	buildBlk := fn.NewBlock(".build")
	b.NewCondBr(already, okBlk, buildBlk)
	okBlk.NewCall(e.csLeave, lock)
	okBlk.NewRet(i32c(1))

	sc := e.zeroed(buildBlk, winSchCredentialsSize)
	tp := e.zeroed(buildBlk, winTLSParametersSize)
	credArr := buildBlk.NewAlloca(irtypes.I8Ptr)

	disabled := buildBlk.NewLoad(irtypes.I32, e.field(buildBlk, e.t.ctx, c, winCtxFDisabled))
	e.storeI32At(buildBlk, tp, winTLSParamOffDisabled, disabled)
	e.storeI32At(buildBlk, sc, winSchOffDwVersion, i32c(winSchCredentialsVersion))
	e.storeI32At(buildBlk, sc, winSchOffCTLSParams, i32c(1))
	e.storePtrAt(buildBlk, sc, winSchOffPTLSParams, tp)

	isServer := buildBlk.NewICmp(enum.IPredNE,
		buildBlk.NewLoad(irtypes.I32, e.field(buildBlk, e.t.ctx, c, winCtxFIsServer)), i32c(0))
	// Client: validate the peer ourselves (custom roots cannot be expressed to
	// SChannel's automatic path) and never let it silently auto-select a
	// certificate from the user's store.
	clientFlags := i32c(winSchUseStrongCrypto | winSchCredManualCredValidation | winSchCredNoDefaultCreds)
	serverFlags := i32c(winSchUseStrongCrypto)
	e.storeI32At(buildBlk, sc, winSchOffDwFlags,
		buildBlk.NewSelect(isServer, serverFlags, clientFlags))

	cert := buildBlk.NewLoad(irtypes.I8Ptr, e.field(buildBlk, e.t.ctx, c, winCtxFCert))
	certBlk := fn.NewBlock(".with_cert")
	acquireBlk := fn.NewBlock(".acquire")
	buildBlk.NewCondBr(e.notNull(buildBlk, cert), certBlk, acquireBlk)

	// SChannel takes its own reference to the certificate during the call, so
	// the one-element PCCERT_CONTEXT array may live on this frame.
	certBlk.NewStore(cert, credArr)
	e.storeI32At(certBlk, sc, winSchOffCCreds, i32c(1))
	e.storePtrAt(certBlk, sc, winSchOffPaCred, e.i8ptr(certBlk, credArr))
	certBlk.NewBr(acquireBlk)

	use := acquireBlk.NewSelect(isServer, i32c(winSecpkgCredInbound), i32c(winSecpkgCredOutbound))
	credH := e.i8ptr(acquireBlk, e.field(acquireBlk, e.t.ctx, c, winCtxFCred))
	st := acquireBlk.NewCall(e.acquireCred, tlsWinNull, e.pkgName, use,
		tlsWinNull, sc, tlsWinNull, tlsWinNull, credH, tlsWinNull)
	setBlk := fn.NewBlock(".acquired")
	failBlk := fn.NewBlock(".failed")
	acquireBlk.NewCondBr(acquireBlk.NewICmp(enum.IPredEQ, st, i32c(winSecEOK)), setBlk, failBlk)

	setBlk.NewStore(i32c(1), validP)
	setBlk.NewCall(e.csLeave, lock)
	setBlk.NewRet(i32c(1))
	failBlk.NewCall(e.csLeave, lock)
	failBlk.NewRet(i32c(0))
	e.ensureCred = fn
}

// --- peer validation -------------------------------------------------------

// emitVerifyPeer defines i32 @__pal_tls_verify(i64 sess) — 0 when the peer
// certificate is acceptable, non-zero otherwise (the value is only ever compared
// against 0 by tls.pr, which turns it into TlsErrorKind.certificate).
//
// Chain building runs with CERT_CHAIN_CACHE_ONLY_URL_RETRIEVAL | DISABLE_AIA:
// the default policy may fetch AIA/CRL over the network, which would block the
// scheduler thread running this goroutine and break the "PAL never blocks"
// invariant this backend is built on. Do not drop those flags.
//
// A certificate handed to add_root_certificate lands in the context's extra
// store, which supplies intermediates but is not a trust anchor — so when the
// chain terminates in a certificate the caller explicitly trusted, the unknown-CA
// verdict is waived and the policy re-run reports any *other* problem (notably a
// hostname mismatch) instead.
func (e *tlsWinEmitter) emitVerifyPeer() {
	i8p := irtypes.I8Ptr
	fn := e.newFn("__pal_tls_verify", irtypes.I32, ir.NewParam("sess", irtypes.I64))
	b := fn.NewBlock(".entry")
	s := b.NewIntToPtr(fn.Params[0], e.t.sessP)

	certPP := b.NewAlloca(i8p)
	b.NewStore(tlsWinNull, certPP)
	chainPP := b.NewAlloca(i8p)
	b.NewStore(tlsWinNull, chainPP)
	chainPara := e.zeroed(b, winCertChainParaSize)
	sslPara := e.zeroed(b, winSSLPolicyParaSize)
	polPara := e.zeroed(b, winChainPolicyParaSize)
	polStat := e.zeroed(b, winChainPolicyStatSize)

	ctxtH := e.i8ptr(b, e.field(b, e.t.sess, s, winSFCtxt))
	qst := b.NewCall(e.queryCtxAttr, ctxtH, i32c(winSecpkgAttrRemoteCertContext),
		e.i8ptr(b, certPP))
	noCert := fn.NewBlock(".no_cert")
	haveCert := fn.NewBlock(".have_cert")
	b.NewCondBr(b.NewICmp(enum.IPredEQ, qst, i32c(winSecEOK)), haveCert, noCert)
	noCert.NewRet(i32c(-1))

	cert := haveCert.NewLoad(i8p, certPP)
	e.storeI32At(haveCert, chainPara, 0, i32c(winCertChainParaSize))
	roots := haveCert.NewLoad(i8p, e.field(haveCert, e.t.ctx,
		haveCert.NewBitCast(haveCert.NewLoad(i8p, e.field(haveCert, e.t.sess, s, winSFCtx)), e.t.ctxP),
		winCtxFRoots))
	chainOK := haveCert.NewCall(e.certGetChain, tlsWinNull, cert, tlsWinNull, roots,
		chainPara, i32c(winCertChainCacheOnlyURL|winCertChainDisableAIA), tlsWinNull,
		e.i8ptr(haveCert, chainPP))
	noChain := fn.NewBlock(".no_chain")
	haveChain := fn.NewBlock(".have_chain")
	haveCert.NewCondBr(haveCert.NewICmp(enum.IPredNE, chainOK, i32c(0)), haveChain, noChain)

	noChain.NewCall(e.certFree, cert)
	noChain.NewRet(i32c(-1))

	chain := haveChain.NewLoad(i8p, chainPP)
	rootCheck := fn.NewBlock(".root_check")
	policyBlk := fn.NewBlock(".policy")
	haveChain.NewCondBr(e.notNull(haveChain, roots), rootCheck, policyBlk)

	// chain->rgpChain[0]->rgpElement[cElement-1]->pCertContext is the chain's
	// terminal (root) certificate.
	rgpChain := e.loadPtrAt(rootCheck, chain, winChainOffRgpChain)
	simple := rootCheck.NewLoad(i8p, rootCheck.NewBitCast(rgpChain, irtypes.NewPointer(i8p)))
	cElem := e.loadI32At(rootCheck, simple, winSimpleOffCElement)
	haveElems := rootCheck.NewICmp(enum.IPredUGT, cElem, i32c(0))
	elemBlk := fn.NewBlock(".terminal_elem")
	rootCheck.NewCondBr(haveElems, elemBlk, policyBlk)

	rgpElem := e.loadPtrAt(elemBlk, simple, winSimpleOffRgpElement)
	lastIdx := elemBlk.NewZExt(elemBlk.NewSub(cElem, i32c(1)), irtypes.I64)
	elemSlot := elemBlk.NewGetElementPtr(i8p,
		elemBlk.NewBitCast(rgpElem, irtypes.NewPointer(i8p)), lastIdx)
	elem := elemBlk.NewLoad(i8p, elemSlot)
	rootCert := e.loadPtrAt(elemBlk, elem, winElemOffCertContext)
	found := elemBlk.NewCall(e.certFind, roots, i32c(winCertEncodingAny), i32c(0),
		i32c(winCertFindExisting), rootCert, tlsWinNull)
	waiveBlk := fn.NewBlock(".waive_untrusted_root")
	elemBlk.NewCondBr(e.notNull(elemBlk, found), waiveBlk, policyBlk)

	// The caller explicitly trusted this root, so drop the "untrusted root" /
	// "partial chain" verdicts before running the SSL policy. The additional
	// store CertGetCertificateChain consults supplies intermediates but is not a
	// trust anchor, and CERT_CHAIN_POLICY_ALLOW_UNKNOWN_CA_FLAG waives only a
	// partial chain — not a self-signed root outside the system ROOT store.
	// Clearing the bits (rather than ignoring dwError) keeps every *other* check,
	// notably the host-name match, in force.
	for _, base := range []value.Value{chain, simple} {
		cur := e.loadI32At(waiveBlk, base, winChainOffTrustError)
		e.storeI32At(waiveBlk, base, winChainOffTrustError,
			waiveBlk.NewAnd(cur, i32c(^int64(winCertTrustUntrustedRoot|winCertTrustPartialChain)&0xFFFFFFFF)))
	}
	waiveBlk.NewCall(e.certFree, found)
	waiveBlk.NewBr(policyBlk)

	isServer := policyBlk.NewLoad(irtypes.I32, e.field(policyBlk, e.t.ctx,
		policyBlk.NewBitCast(policyBlk.NewLoad(i8p, e.field(policyBlk, e.t.sess, s, winSFCtx)), e.t.ctxP),
		winCtxFIsServer))
	authType := policyBlk.NewSelect(policyBlk.NewICmp(enum.IPredNE, isServer, i32c(0)),
		i32c(winAuthTypeClient), i32c(winAuthTypeServer))
	e.storeI32At(policyBlk, sslPara, 0, i32c(winSSLPolicyParaSize))
	e.storeI32At(policyBlk, sslPara, 4, authType)
	e.storePtrAt(policyBlk, sslPara, 16,
		policyBlk.NewLoad(i8p, e.field(policyBlk, e.t.sess, s, winSFHostW)))
	e.storeI32At(policyBlk, polPara, 0, i32c(winChainPolicyParaSize))
	e.storeI32At(policyBlk, polPara, 4, i32c(0))
	e.storePtrAt(policyBlk, polPara, 8, sslPara)
	e.storeI32At(policyBlk, polStat, 0, i32c(winChainPolicyStatSize))

	polOK := policyBlk.NewCall(e.certVerifyPol,
		constant.NewIntToPtr(i64c(winCertChainPolicySSL), i8p), chain, polPara, polStat)
	resP := policyBlk.NewAlloca(irtypes.I32)
	policyBlk.NewStore(i32c(-1), resP)
	readBlk := fn.NewBlock(".read_status")
	doneBlk := fn.NewBlock(".done")
	policyBlk.NewCondBr(policyBlk.NewICmp(enum.IPredNE, polOK, i32c(0)), readBlk, doneBlk)

	readBlk.NewStore(e.loadI32At(readBlk, polStat, 4), resP)
	readBlk.NewBr(doneBlk)

	doneBlk.NewCall(e.certFreeChain, chain)
	doneBlk.NewCall(e.certFree, cert)
	doneBlk.NewRet(doneBlk.NewLoad(irtypes.I32, resP))
	e.verifyPeer = fn
}

// --- handshake step --------------------------------------------------------

// emitHandshakeStep defines i32 @__pal_tls_hs_step(i64 sess, i32 noInput):
// one InitializeSecurityContext (client) or AcceptSecurityContext (server) round
// against the session's inbound queue, appending any produced token to the
// outbound queue and consuming exactly the input SChannel accepted. Returns the
// backend-neutral handshake status (0 ok, 1 want-more, -1 fatal).
//
// noInput != 0 drives the same round with no input buffer, which is what
// shutdown needs to generate the close_notify token — so the SSPI call sequence
// lives in exactly one place.
func (e *tlsWinEmitter) emitHandshakeStep() {
	i8p := irtypes.I8Ptr
	i32p := irtypes.NewPointer(irtypes.I32)
	fn := e.newFn("__pal_tls_hs_step", irtypes.I32,
		ir.NewParam("sess", irtypes.I64), ir.NewParam("noInput", irtypes.I32))
	entry := fn.NewBlock(".entry")
	s := entry.NewIntToPtr(fn.Params[0], e.t.sessP)

	// Every alloca lives here: the INCOMPLETE_CREDENTIALS retry branches back to
	// .call, and an alloca on that path would grow the frame per iteration.
	inBufs := entry.NewAlloca(e.t.secBuf2)
	inDesc := entry.NewAlloca(e.t.secDesc)
	outBufs := entry.NewAlloca(e.t.secBuf1)
	outDesc := entry.NewAlloca(e.t.secDesc)
	attr := entry.NewAlloca(irtypes.I32)
	retried := entry.NewAlloca(irtypes.I32)
	sizes := e.zeroed(entry, 32) // SecPkgContext_StreamSizes is 20 bytes
	entry.NewStore(i32c(0), retried)

	ctxRaw := entry.NewLoad(i8p, e.field(entry, e.t.sess, s, winSFCtx))
	c := entry.NewBitCast(ctxRaw, e.t.ctxP)
	credOK := entry.NewCall(e.ensureCred, entry.NewPtrToInt(ctxRaw, irtypes.I64))
	fatalBlk := fn.NewBlock(".fatal")
	callBlk := fn.NewBlock(".call")
	entry.NewCondBr(entry.NewICmp(enum.IPredNE, credOK, i32c(0)), callBlk, fatalBlk)
	fatalBlk.NewRet(i32c(-1))

	// --- one SSPI round ---
	inQ := e.field(callBlk, e.t.sess, s, winSFIn)
	inPtr := e.bufData(callBlk, inQ)
	inLen := e.bufAvail(callBlk, inQ)
	in0 := callBlk.NewGetElementPtr(e.t.secBuf2, inBufs, i32c(0), i32c(0))
	callBlk.NewStore(callBlk.NewTrunc(inLen, irtypes.I32), e.field(callBlk, e.t.secBuf, in0, 0))
	callBlk.NewStore(i32c(winSecBufferToken), e.field(callBlk, e.t.secBuf, in0, 1))
	callBlk.NewStore(inPtr, e.field(callBlk, e.t.secBuf, in0, 2))
	in1 := callBlk.NewGetElementPtr(e.t.secBuf2, inBufs, i32c(0), i32c(1))
	callBlk.NewStore(i32c(0), e.field(callBlk, e.t.secBuf, in1, 0))
	callBlk.NewStore(i32c(winSecBufferEmpty), e.field(callBlk, e.t.secBuf, in1, 1))
	callBlk.NewStore(tlsWinNull, e.field(callBlk, e.t.secBuf, in1, 2))
	callBlk.NewStore(i32c(winSecBufferVersion), e.field(callBlk, e.t.secDesc, inDesc, 0))
	callBlk.NewStore(i32c(2), e.field(callBlk, e.t.secDesc, inDesc, 1))
	callBlk.NewStore(e.i8ptr(callBlk, in0), e.field(callBlk, e.t.secDesc, inDesc, 2))

	out0 := callBlk.NewGetElementPtr(e.t.secBuf1, outBufs, i32c(0), i32c(0))
	callBlk.NewStore(i32c(0), e.field(callBlk, e.t.secBuf, out0, 0))
	callBlk.NewStore(i32c(winSecBufferToken), e.field(callBlk, e.t.secBuf, out0, 1))
	callBlk.NewStore(tlsWinNull, e.field(callBlk, e.t.secBuf, out0, 2))
	callBlk.NewStore(i32c(winSecBufferVersion), e.field(callBlk, e.t.secDesc, outDesc, 0))
	callBlk.NewStore(i32c(1), e.field(callBlk, e.t.secDesc, outDesc, 1))
	callBlk.NewStore(e.i8ptr(callBlk, out0), e.field(callBlk, e.t.secDesc, outDesc, 2))
	callBlk.NewStore(i32c(0), attr)

	noInput := callBlk.NewICmp(enum.IPredNE, fn.Params[1], i32c(0))
	pInput := callBlk.NewSelect(noInput, tlsWinNull, e.i8ptr(callBlk, inDesc))
	ctxtH := e.i8ptr(callBlk, e.field(callBlk, e.t.sess, s, winSFCtxt))
	valid := callBlk.NewICmp(enum.IPredNE,
		callBlk.NewLoad(irtypes.I32, e.field(callBlk, e.t.sess, s, winSFCtxtValid)), i32c(0))
	phCtxt := callBlk.NewSelect(valid, ctxtH, tlsWinNull)
	credH := e.i8ptr(callBlk, e.field(callBlk, e.t.ctx, c, winCtxFCred))
	pOut := e.i8ptr(callBlk, outDesc)
	attrP := callBlk.NewBitCast(attr, i32p)
	isServer := callBlk.NewICmp(enum.IPredNE,
		callBlk.NewLoad(irtypes.I32, e.field(callBlk, e.t.ctx, c, winCtxFIsServer)), i32c(0))
	serverBlk := fn.NewBlock(".accept")
	clientBlk := fn.NewBlock(".initialize")
	afterBlk := fn.NewBlock(".after_call")
	callBlk.NewCondBr(isServer, serverBlk, clientBlk)

	stServer := serverBlk.NewCall(e.acceptSecCtx, credH, phCtxt, pInput, i32c(winASCReq),
		i32c(winSecurityNativeDrep), ctxtH, pOut, attrP, tlsWinNull)
	serverBlk.NewBr(afterBlk)

	sni := clientBlk.NewLoad(i8p, e.field(clientBlk, e.t.sess, s, winSFSNI))
	stClient := clientBlk.NewCall(e.initSecCtx, credH, phCtxt, sni, i32c(winISCReq),
		i32c(0), i32c(winSecurityNativeDrep), pInput, i32c(0), ctxtH, pOut, attrP, tlsWinNull)
	clientBlk.NewBr(afterBlk)

	st := afterBlk.NewPhi(ir.NewIncoming(stServer, serverBlk), ir.NewIncoming(stClient, clientBlk))

	// The server may ask for a client certificate we do not have. Re-running the
	// same round once is the documented way to continue anonymously; a second
	// refusal is a genuine handshake failure.
	incCreds := afterBlk.NewICmp(enum.IPredEQ, st, i32c(winSecIIncompleteCredentials))
	retryBlk := fn.NewBlock(".maybe_retry")
	processBlk := fn.NewBlock(".process")
	afterBlk.NewCondBr(incCreds, retryBlk, processBlk)

	firstTry := retryBlk.NewICmp(enum.IPredEQ, retryBlk.NewLoad(irtypes.I32, retried), i32c(0))
	againBlk := fn.NewBlock(".retry")
	retryBlk.NewCondBr(firstTry, againBlk, fatalBlk)
	// .call resets the output descriptor, so anything SChannel allocated on this
	// round has to go back now or its only reference is lost.
	retryTok := againBlk.NewLoad(i8p, e.field(againBlk, e.t.secBuf, out0, 2))
	freeRetryBlk := fn.NewBlock(".free_retry_token")
	resumeBlk := fn.NewBlock(".resume")
	againBlk.NewCondBr(e.notNull(againBlk, retryTok), freeRetryBlk, resumeBlk)
	freeRetryBlk.NewCall(e.freeCtxBuf, retryTok)
	freeRetryBlk.NewBr(resumeBlk)
	resumeBlk.NewStore(i32c(1), retried)
	resumeBlk.NewBr(callBlk)

	// SEC_E_INCOMPLETE_MESSAGE: SChannel needs a longer prefix — leave the queue
	// untouched and ask the caller for more bytes.
	incMsg := processBlk.NewICmp(enum.IPredEQ, st, i32c(winSecEIncompleteMessage))
	moreBlk := fn.NewBlock(".want_more")
	resultBlk := fn.NewBlock(".result")
	processBlk.NewCondBr(incMsg, moreBlk, resultBlk)
	moreBlk.NewRet(i32c(1))

	isOK := resultBlk.NewICmp(enum.IPredEQ, st, i32c(winSecEOK))
	isCont := resultBlk.NewICmp(enum.IPredEQ, st, i32c(winSecIContinueNeeded))
	isCompNeed := resultBlk.NewICmp(enum.IPredEQ, st, i32c(winSecICompleteNeeded))
	isCompCont := resultBlk.NewICmp(enum.IPredEQ, st, i32c(winSecICompleteAndContinue))
	established := resultBlk.NewOr(resultBlk.NewOr(isOK, isCont), resultBlk.NewOr(isCompNeed, isCompCont))
	markBlk := fn.NewBlock(".mark_valid")
	tokenBlk := fn.NewBlock(".token")
	resultBlk.NewCondBr(established, markBlk, tokenBlk)
	markBlk.NewStore(i32c(1), e.field(markBlk, e.t.sess, s, winSFCtxtValid))
	markBlk.NewBr(tokenBlk)

	// Drain the produced token (allocated by SChannel under *_REQ_ALLOCATE_MEMORY).
	// On a failed handshake this is the alert to send the peer, which is why
	// *_REQ_EXTENDED_ERROR is requested.
	tokPtr := tokenBlk.NewLoad(i8p, e.field(tokenBlk, e.t.secBuf, out0, 2))
	tokLen := tokenBlk.NewLoad(irtypes.I32, e.field(tokenBlk, e.t.secBuf, out0, 0))
	haveTok := tokenBlk.NewAnd(e.notNull(tokenBlk, tokPtr),
		tokenBlk.NewICmp(enum.IPredUGT, tokLen, i32c(0)))
	appendBlk := fn.NewBlock(".append_token")
	freeTokBlk := fn.NewBlock(".free_token")
	tokenBlk.NewCondBr(haveTok, appendBlk, freeTokBlk)
	appendBlk.NewCall(e.bufAppend, e.sessBuf(appendBlk, s, winSFOut), tokPtr,
		appendBlk.NewZExt(tokLen, irtypes.I64))
	appendBlk.NewBr(freeTokBlk)

	consumeBlk := fn.NewBlock(".consume")
	doFreeBlk := fn.NewBlock(".free_ctx_buffer")
	freeTokBlk.NewCondBr(e.notNull(freeTokBlk, tokPtr), doFreeBlk, consumeBlk)
	doFreeBlk.NewCall(e.freeCtxBuf, tokPtr)
	doFreeBlk.NewBr(consumeBlk)

	mapBlk := fn.NewBlock(".map")
	doConsumeBlk := fn.NewBlock(".do_consume")
	consumeBlk.NewCondBr(noInput, mapBlk, doConsumeBlk)

	// Whatever SChannel did not use comes back as SECBUFFER_EXTRA in slot 1 (a
	// count, not a pointer — the unread bytes are the tail of the input).
	extraType := doConsumeBlk.NewLoad(irtypes.I32, e.field(doConsumeBlk, e.t.secBuf, in1, 1))
	extraLen := doConsumeBlk.NewLoad(irtypes.I32, e.field(doConsumeBlk, e.t.secBuf, in1, 0))
	isExtra := doConsumeBlk.NewICmp(enum.IPredEQ, extraType, i32c(winSecBufferExtra))
	extra := doConsumeBlk.NewSelect(isExtra, doConsumeBlk.NewZExt(extraLen, irtypes.I64), i64c(0))
	nowLen := e.bufAvail(doConsumeBlk, e.field(doConsumeBlk, e.t.sess, s, winSFIn))
	doConsumeBlk.NewCall(e.bufConsume, e.sessBuf(doConsumeBlk, s, winSFIn),
		doConsumeBlk.NewSub(nowLen, extra))
	doConsumeBlk.NewBr(mapBlk)

	completeBlk := fn.NewBlock(".complete")
	notDoneBlk := fn.NewBlock(".not_complete")
	mapBlk.NewCondBr(mapBlk.NewOr(isOK, isCompNeed), completeBlk, notDoneBlk)
	notDoneBlk.NewCondBr(notDoneBlk.NewOr(isCont, isCompCont), moreBlk, fatalBlk)

	// Handshake finished: latch the record geometry, size the encrypt scratch and
	// run peer validation exactly once (shutdown re-enters this helper, so the
	// guard also keeps it from re-verifying on the close_notify round).
	alreadyDone := completeBlk.NewICmp(enum.IPredNE,
		completeBlk.NewLoad(irtypes.I32, e.field(completeBlk, e.t.sess, s, winSFDone)), i32c(0))
	okBlk := fn.NewBlock(".ok")
	finishBlk := fn.NewBlock(".finish")
	completeBlk.NewCondBr(alreadyDone, okBlk, finishBlk)

	finishBlk.NewCall(e.queryCtxAttr, ctxtH, i32c(winSecpkgAttrStreamSizes), sizes)
	hdr := e.loadI32At(finishBlk, sizes, 0)
	trl := e.loadI32At(finishBlk, sizes, 4)
	maxMsg := e.loadI32At(finishBlk, sizes, 8)
	finishBlk.NewStore(hdr, e.field(finishBlk, e.t.sess, s, winSFHdrLen))
	finishBlk.NewStore(trl, e.field(finishBlk, e.t.sess, s, winSFTrlLen))
	finishBlk.NewStore(maxMsg, e.field(finishBlk, e.t.sess, s, winSFMaxMsg))
	total := finishBlk.NewZExt(finishBlk.NewAdd(finishBlk.NewAdd(hdr, trl), maxMsg), irtypes.I64)
	finishBlk.NewStore(finishBlk.NewCall(e.alloc, total),
		e.field(finishBlk, e.t.sess, s, winSFScratch))
	finishBlk.NewStore(i32c(1), e.field(finishBlk, e.t.sess, s, winSFDone))
	wantVerify := finishBlk.NewICmp(enum.IPredNE,
		finishBlk.NewLoad(irtypes.I32, e.field(finishBlk, e.t.ctx, c, winCtxFVerify)), i32c(0))
	verifyBlk := fn.NewBlock(".verify")
	finishBlk.NewCondBr(wantVerify, verifyBlk, okBlk)
	verifyRes := verifyBlk.NewCall(e.verifyPeer, fn.Params[0])
	verifyBlk.NewStore(verifyRes, e.field(verifyBlk, e.t.sess, s, winSFVerifyRes))
	// A rejected peer certificate must fail the handshake, not merely be
	// recorded: tls.pr only consults get_verify_result on the fatal path, and
	// that is what turns this into TlsErrorKind.certificate rather than letting
	// an unverified connection through. (OpenSSL fails the handshake itself.)
	verifyBlk.NewCondBr(verifyBlk.NewICmp(enum.IPredNE, verifyRes, i32c(0)), fatalBlk, okBlk)

	okBlk.NewRet(i32c(0))
	e.hsStep = fn
}

// emitKeyName defines i8* @__pal_tls_key_name(i64 ctx) — the pal_alloc'd,
// NUL-terminated UTF-16 name of the context's CNG key (see winKeyName* for the
// layout). Uniqueness comes from the context address plus the process id, so no
// shared counter (and no lock around one) is needed.
func (e *tlsWinEmitter) emitKeyName() {
	i8p := irtypes.I8Ptr
	fn := e.newFn("__pal_tls_key_name", i8p, ir.NewParam("ctx", irtypes.I64))
	b := fn.NewBlock(".entry")
	buf := b.NewCall(e.alloc, i64c(winKeyNameBytes))
	b.NewCall(e.memcpy, buf, e.keyPrefix, i64c(winKeyNamePrefixUnits*2))
	b.NewCall(e.hexWide, e.byteAt(b, buf, winKeyNamePrefixUnits*2), fn.Params[0], i32c(16))
	// '-' separator, then the process id.
	b.NewStore(constant.NewInt(irtypes.I16, '-'),
		b.NewBitCast(e.byteAt(b, buf, (winKeyNamePrefixUnits+16)*2), irtypes.NewPointer(irtypes.I16)))
	pid := b.NewZExt(b.NewCall(e.getPid), irtypes.I64)
	b.NewCall(e.hexWide, e.byteAt(b, buf, (winKeyNamePrefixUnits+17)*2), pid, i32c(8))
	b.NewStore(constant.NewInt(irtypes.I16, 0),
		b.NewBitCast(e.byteAt(b, buf, (winKeyNameUnits-1)*2), irtypes.NewPointer(irtypes.I16)))
	b.NewRet(buf)
	e.keyName = fn
}

// emitHexWide defines void @__pal_tls_hex_w(i8* dst, i64 value, i32 digits) —
// writes the low `digits` hex digits of value as UTF-16, most significant first.
func (e *tlsWinEmitter) emitHexWide() {
	i8p := irtypes.I8Ptr
	i16p := irtypes.NewPointer(irtypes.I16)
	fn := e.newFn("__pal_tls_hex_w", irtypes.Void,
		ir.NewParam("dst", i8p), ir.NewParam("value", irtypes.I64),
		ir.NewParam("digits", irtypes.I32))
	entry := fn.NewBlock(".entry")
	n := entry.NewZExt(fn.Params[2], irtypes.I64)
	condBlk := fn.NewBlock(".cond")
	bodyBlk := fn.NewBlock(".body")
	doneBlk := fn.NewBlock(".done")
	entry.NewBr(condBlk)
	i := condBlk.NewPhi(ir.NewIncoming(i64c(0), entry))
	condBlk.NewCondBr(condBlk.NewICmp(enum.IPredULT, i, n), bodyBlk, doneBlk)

	shift := bodyBlk.NewMul(bodyBlk.NewSub(bodyBlk.NewSub(n, i), i64c(1)), i64c(4))
	nib := bodyBlk.NewAnd(bodyBlk.NewLShr(fn.Params[1], shift), i64c(0xF))
	isDigit := bodyBlk.NewICmp(enum.IPredULT, nib, i64c(10))
	ch := bodyBlk.NewSelect(isDigit,
		bodyBlk.NewAdd(nib, i64c('0')), bodyBlk.NewAdd(nib, i64c('a'-10)))
	slot := bodyBlk.NewGetElementPtr(irtypes.I16,
		bodyBlk.NewBitCast(fn.Params[0], i16p), i)
	bodyBlk.NewStore(bodyBlk.NewTrunc(ch, irtypes.I16), slot)
	next := bodyBlk.NewAdd(i, i64c(1))
	bodyBlk.NewBr(condBlk)
	i.Incs = append(i.Incs, ir.NewIncoming(next, bodyBlk))

	doneBlk.NewRet(nil)
	e.hexWide = fn
}
