package pal

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// tls_schannel_api.go — the pal_tls_* surface of the SChannel backend (T1598).
// Every wrapper here has byte-for-byte the same signature as its OpenSSL twin in
// tls_posix.go, because codegen/tls.go bridges both with the identical shapes.

// EmitTLS emits every pal_tls_* wrapper for Windows and returns them keyed by
// name. The underlying SSPI/crypt32/CNG symbols are declared as bodyless externs
// and resolved at link against the self-generated secur32/crypt32/ncrypt import
// libraries (see tools/build/winlink/def/).
func (p *WindowsPAL) EmitTLS(module *ir.Module) map[string]*ir.Func {
	e := &tlsWinEmitter{m: module, t: newTLSWinTypes()}
	e.declareExterns()
	e.emitBufHelpers()
	e.emitPemDER()
	e.emitWiden()
	e.emitHexWide()
	e.emitKeyName()
	e.emitEnsureCred()
	e.emitVerifyPeer()
	e.emitHandshakeStep()

	fns := make(map[string]*ir.Func)
	emit := func(f *ir.Func) { fns[f.Name()] = f }

	emit(e.emitCtxNew("pal_tls_ctx_new_client", false))
	emit(e.emitCtxNew("pal_tls_ctx_new_server", true))
	emit(e.emitCtxFree())
	emit(e.emitCtxSetVerify())
	emit(e.emitCtxSetMinVersion())
	emit(e.emitCtxAddCA())
	emit(e.emitCtxUseCert())
	emit(e.emitCtxUseKey())
	emit(e.emitCtxLoadDefaultTrust())

	emit(e.emitNew())
	emit(e.emitSetState("pal_tls_set_connect_state"))
	emit(e.emitSetState("pal_tls_set_accept_state"))
	emit(e.emitSetSNI())
	emit(e.emitSetVerifyHost())
	emit(e.emitDoHandshake())
	emit(e.emitRead())
	emit(e.emitWrite())
	emit(e.emitShutdown())
	emit(e.emitBioReadOut())
	emit(e.emitBioWriteIn())
	emit(e.emitBioPendingOut())
	emit(e.emitGetVersion())
	emit(e.emitGetCipher())
	emit(e.emitGetVerifyResult())
	emit(e.emitFree())
	return fns
}

// --- context ---------------------------------------------------------------

// emitCtxNew defines i64 @pal_tls_ctx_new_client/_server() — allocates the
// backend context. The SChannel credential itself is acquired lazily on the
// first session (see __pal_tls_ensure_cred), because tls.pr configures the
// context after creating it.
func (e *tlsWinEmitter) emitCtxNew(name string, server bool) *ir.Func {
	fn := e.newFn(name, irtypes.I64)
	b := fn.NewBlock(".entry")
	raw := b.NewCall(e.alloc, tlsWinSizeOf(e.t.ctx))
	b.NewCall(e.memset, raw, i32c(0), tlsWinSizeOf(e.t.ctx))
	c := b.NewBitCast(raw, e.t.ctxP)
	if server {
		b.NewStore(i32c(1), e.field(b, e.t.ctx, c, winCtxFIsServer))
	}
	// Default floor is TLS 1.2, matching TlsConfig's documented default.
	b.NewStore(i32c(winSpProtBelowTLS12), e.field(b, e.t.ctx, c, winCtxFDisabled))
	b.NewRet(b.NewPtrToInt(raw, irtypes.I64))
	return fn
}

// emitCtxFree defines void @pal_tls_ctx_free(i64 ctx) — releases the credential,
// the certificate, the extra-roots store and the imported CNG key, then the
// context allocation itself. Every path that can create one of these frees it
// here, which is what keeps the leak detector at zero.
func (e *tlsWinEmitter) emitCtxFree() *ir.Func {
	i8p := irtypes.I8Ptr
	fn := e.newFn("pal_tls_ctx_free", irtypes.Void, ir.NewParam("ctx", irtypes.I64))
	b := fn.NewBlock(".entry")
	retBlk := fn.NewBlock(".ret")
	liveBlk := fn.NewBlock(".live")
	b.NewCondBr(b.NewICmp(enum.IPredEQ, fn.Params[0], i64c(0)), retBlk, liveBlk)
	retBlk.NewRet(nil)

	c := liveBlk.NewIntToPtr(fn.Params[0], e.t.ctxP)

	// Credential first — it holds a reference on the certificate.
	credValid := liveBlk.NewICmp(enum.IPredNE,
		liveBlk.NewLoad(irtypes.I32, e.field(liveBlk, e.t.ctx, c, winCtxFCredValid)), i32c(0))
	freeCredBlk := fn.NewBlock(".free_cred")
	afterCredBlk := fn.NewBlock(".after_cred")
	liveBlk.NewCondBr(credValid, freeCredBlk, afterCredBlk)
	freeCredBlk.NewCall(e.freeCred, e.i8ptr(freeCredBlk, e.field(freeCredBlk, e.t.ctx, c, winCtxFCred)))
	freeCredBlk.NewBr(afterCredBlk)

	cert := afterCredBlk.NewLoad(i8p, e.field(afterCredBlk, e.t.ctx, c, winCtxFCert))
	freeCertBlk := fn.NewBlock(".free_cert")
	afterCertBlk := fn.NewBlock(".after_cert")
	afterCredBlk.NewCondBr(e.notNull(afterCredBlk, cert), freeCertBlk, afterCertBlk)
	freeCertBlk.NewCall(e.certFree, cert)
	freeCertBlk.NewBr(afterCertBlk)

	roots := afterCertBlk.NewLoad(i8p, e.field(afterCertBlk, e.t.ctx, c, winCtxFRoots))
	closeBlk := fn.NewBlock(".close_store")
	afterStoreBlk := fn.NewBlock(".after_store")
	afterCertBlk.NewCondBr(e.notNull(afterCertBlk, roots), closeBlk, afterStoreBlk)
	closeBlk.NewCall(e.certCloseStore, roots, i32c(winCertCloseStoreForce))
	closeBlk.NewBr(afterStoreBlk)

	// NCryptDeleteKey removes the key from the provider's store *and* closes the
	// handle, so the named key created in pal_tls_ctx_use_key never outlives the
	// TlsConfig / TlsListener that owns it.
	key := afterStoreBlk.NewLoad(irtypes.I64, e.field(afterStoreBlk, e.t.ctx, c, winCtxFKey))
	freeKeyBlk := fn.NewBlock(".delete_key")
	afterKeyBlk := fn.NewBlock(".after_key")
	afterStoreBlk.NewCondBr(afterStoreBlk.NewICmp(enum.IPredNE, key, i64c(0)), freeKeyBlk, afterKeyBlk)
	freeKeyBlk.NewCall(e.ncryptDelete, key, i32c(winNCryptSilentFlag))
	freeKeyBlk.NewBr(afterKeyBlk)

	prov := afterKeyBlk.NewLoad(irtypes.I64, e.field(afterKeyBlk, e.t.ctx, c, winCtxFKeyProv))
	freeProvBlk := fn.NewBlock(".free_prov")
	nameBlk := fn.NewBlock(".key_name")
	afterKeyBlk.NewCondBr(afterKeyBlk.NewICmp(enum.IPredNE, prov, i64c(0)), freeProvBlk, nameBlk)
	freeProvBlk.NewCall(e.ncryptFree, prov)
	freeProvBlk.NewBr(nameBlk)

	keyName := nameBlk.NewLoad(i8p, e.field(nameBlk, e.t.ctx, c, winCtxFKeyName))
	freeNameBlk := fn.NewBlock(".free_key_name")
	doneBlk := fn.NewBlock(".done")
	nameBlk.NewCondBr(e.notNull(nameBlk, keyName), freeNameBlk, doneBlk)
	freeNameBlk.NewCall(e.free, keyName)
	freeNameBlk.NewBr(doneBlk)

	doneBlk.NewCall(e.free, e.i8ptr(doneBlk, c))
	doneBlk.NewRet(nil)
	return fn
}

// emitCtxSetVerify defines void @pal_tls_ctx_set_verify(i64 ctx, i32 peer).
// Validation is always performed by this backend (__pal_tls_verify); the flag
// only decides whether it runs at all.
func (e *tlsWinEmitter) emitCtxSetVerify() *ir.Func {
	fn := e.newFn("pal_tls_ctx_set_verify", irtypes.Void,
		ir.NewParam("ctx", irtypes.I64), ir.NewParam("peer", irtypes.I32))
	b := fn.NewBlock(".entry")
	c := b.NewIntToPtr(fn.Params[0], e.t.ctxP)
	on := b.NewICmp(enum.IPredNE, fn.Params[1], i32c(0))
	b.NewStore(b.NewSelect(on, i32c(1), i32c(0)), e.field(b, e.t.ctx, c, winCtxFVerify))
	b.NewRet(nil)
	return fn
}

// emitCtxSetMinVersion defines i32 @pal_tls_ctx_set_min_version(i64 ctx, i32 ver).
// `ver` is the TLS wire version (0x0303 = 1.2, 0x0304 = 1.3); SChannel expresses
// a floor as the set of protocols to disable.
func (e *tlsWinEmitter) emitCtxSetMinVersion() *ir.Func {
	fn := e.newFn("pal_tls_ctx_set_min_version", irtypes.I32,
		ir.NewParam("ctx", irtypes.I64), ir.NewParam("ver", irtypes.I32))
	b := fn.NewBlock(".entry")
	c := b.NewIntToPtr(fn.Params[0], e.t.ctxP)
	want13 := b.NewICmp(enum.IPredSGE, fn.Params[1], i32c(0x0304))
	mask := b.NewSelect(want13,
		i32c(winSpProtBelowTLS12|winSpProtTLS12), i32c(winSpProtBelowTLS12))
	b.NewStore(mask, e.field(b, e.t.ctx, c, winCtxFDisabled))
	b.NewRet(i32c(1))
	return fn
}

// emitCtxAddCA defines i32 @pal_tls_ctx_add_ca(i64 ctx, i8* pem, i64 len) —
// adds a PEM certificate to the context's extra trust anchors. 1 ok, 0 failure.
func (e *tlsWinEmitter) emitCtxAddCA() *ir.Func {
	i8p := irtypes.I8Ptr
	fn := e.newFn("pal_tls_ctx_add_ca", irtypes.I32,
		ir.NewParam("ctx", irtypes.I64), ir.NewParam("pem", i8p),
		ir.NewParam("len", irtypes.I64))
	b := fn.NewBlock(".entry")
	c := b.NewIntToPtr(fn.Params[0], e.t.ctxP)
	dlen := b.NewAlloca(irtypes.I64)
	b.NewStore(i64c(0), dlen)
	der := b.NewCall(e.pemDER, fn.Params[1], fn.Params[2], dlen)
	failBlk := fn.NewBlock(".fail")
	haveBlk := fn.NewBlock(".have_der")
	b.NewCondBr(e.notNull(b, der), haveBlk, failBlk)
	failBlk.NewRet(i32c(0))

	rootsP := e.field(haveBlk, e.t.ctx, c, winCtxFRoots)
	roots := haveBlk.NewLoad(i8p, rootsP)
	openBlk := fn.NewBlock(".open_store")
	addBlk := fn.NewBlock(".add")
	haveBlk.NewCondBr(e.isNull(haveBlk, roots), openBlk, addBlk)

	fresh := openBlk.NewCall(e.certOpenStore,
		constant.NewIntToPtr(i64c(winCertStoreProvMemory), i8p), i32c(0), tlsWinNull,
		i32c(winCertStoreCreateNew), tlsWinNull)
	openBlk.NewStore(fresh, rootsP)
	openFailBlk := fn.NewBlock(".store_failed")
	openBlk.NewCondBr(e.notNull(openBlk, fresh), addBlk, openFailBlk)
	openFailBlk.NewCall(e.free, der)
	openFailBlk.NewRet(i32c(0))

	store := addBlk.NewLoad(i8p, rootsP)
	ok := addBlk.NewCall(e.certAddEnc, store, i32c(winCertEncodingAny), der,
		addBlk.NewTrunc(addBlk.NewLoad(irtypes.I64, dlen), irtypes.I32),
		i32c(winCertStoreAddAlways), tlsWinNull)
	addBlk.NewCall(e.free, der)
	addBlk.NewRet(addBlk.NewSelect(addBlk.NewICmp(enum.IPredNE, ok, i32c(0)), i32c(1), i32c(0)))
	return fn
}

// emitCtxUseCert defines i32 @pal_tls_ctx_use_cert(i64 ctx, i8* pem, i64 len).
func (e *tlsWinEmitter) emitCtxUseCert() *ir.Func {
	i8p := irtypes.I8Ptr
	fn := e.newFn("pal_tls_ctx_use_cert", irtypes.I32,
		ir.NewParam("ctx", irtypes.I64), ir.NewParam("pem", i8p),
		ir.NewParam("len", irtypes.I64))
	b := fn.NewBlock(".entry")
	c := b.NewIntToPtr(fn.Params[0], e.t.ctxP)
	dlen := b.NewAlloca(irtypes.I64)
	b.NewStore(i64c(0), dlen)
	der := b.NewCall(e.pemDER, fn.Params[1], fn.Params[2], dlen)
	failBlk := fn.NewBlock(".fail")
	haveBlk := fn.NewBlock(".have_der")
	b.NewCondBr(e.notNull(b, der), haveBlk, failBlk)
	failBlk.NewRet(i32c(0))

	cert := haveBlk.NewCall(e.certCreate, i32c(winCertEncodingAny), der,
		haveBlk.NewTrunc(haveBlk.NewLoad(irtypes.I64, dlen), irtypes.I32))
	haveBlk.NewCall(e.free, der)
	storeBlk := fn.NewBlock(".store")
	haveBlk.NewCondBr(e.notNull(haveBlk, cert), storeBlk, failBlk)

	certP := e.field(storeBlk, e.t.ctx, c, winCtxFCert)
	old := storeBlk.NewLoad(i8p, certP)
	dropBlk := fn.NewBlock(".drop_old")
	setBlk := fn.NewBlock(".set")
	storeBlk.NewCondBr(e.notNull(storeBlk, old), dropBlk, setBlk)
	dropBlk.NewCall(e.certFree, old)
	dropBlk.NewBr(setBlk)

	setBlk.NewStore(cert, certP)
	setBlk.NewRet(i32c(1))
	return fn
}

// emitCtxUseKey defines i32 @pal_tls_ctx_use_key(i64 ctx, i8* pem, i64 len).
//
// The PEM's DER body *is* a PKCS#8 PrivateKeyInfo, which the Microsoft Software
// Key Storage Provider imports directly — so no intermediate ASN.1 decoding is
// needed, and the same path covers RSA and EC keys. Going through CNG rather than a legacy CAPI CSP is what lets SChannel
// offer TLS 1.3 server-side, which requires RSA-PSS signatures a legacy CSP
// cannot produce.
//
// The key is named (see winKeyName*) because SChannel resolves a credential's
// private key only through CERT_KEY_PROV_INFO_PROP_ID, which addresses a key by
// provider + container name. That makes the key briefly visible in the user's
// CNG key store; pal_tls_ctx_free deletes it again with NCryptDeleteKey, so it
// never outlives the TlsConfig / TlsListener that created it.
//
// The key is attached to the certificate, so tls.pr's cert-then-key ordering is
// a precondition; without a certificate this returns 0 and surfaces as a
// certificate error. Calling it twice replaces the previous key.
func (e *tlsWinEmitter) emitCtxUseKey() *ir.Func {
	i8p := irtypes.I8Ptr
	i64p := irtypes.NewPointer(irtypes.I64)
	fn := e.newFn("pal_tls_ctx_use_key", irtypes.I32,
		ir.NewParam("ctx", irtypes.I64), ir.NewParam("pem", i8p),
		ir.NewParam("len", irtypes.I64))
	b := fn.NewBlock(".entry")
	c := b.NewIntToPtr(fn.Params[0], e.t.ctxP)
	dlen := b.NewAlloca(irtypes.I64)
	b.NewStore(i64c(0), dlen)
	provP := b.NewAlloca(irtypes.I64)
	b.NewStore(i64c(0), provP)
	keyP := b.NewAlloca(irtypes.I64)
	b.NewStore(i64c(0), keyP)
	nameBuf := b.NewAlloca(e.t.secBuf1)  // NCryptBuffer has the same {u32,u32,ptr} shape
	nameDesc := b.NewAlloca(e.t.secDesc) // ...and NCryptBufferDesc matches SecBufferDesc
	kpi := e.zeroed(b, winKeyProvInfoSize)

	failBlk := fn.NewBlock(".fail")
	failBlk.NewRet(i32c(0))

	cert := b.NewLoad(i8p, e.field(b, e.t.ctx, c, winCtxFCert))
	decodeBlk := fn.NewBlock(".decode_pem")
	b.NewCondBr(e.notNull(b, cert), decodeBlk, failBlk)

	der := decodeBlk.NewCall(e.pemDER, fn.Params[1], fn.Params[2], dlen)
	provBlk := fn.NewBlock(".open_provider")
	decodeBlk.NewCondBr(e.notNull(decodeBlk, der), provBlk, failBlk)

	stProv := provBlk.NewCall(e.ncryptOpenProv, provBlk.NewBitCast(provP, i64p),
		e.provName, i32c(0))
	provFailBlk := fn.NewBlock(".provider_failed")
	importBlk := fn.NewBlock(".import_key")
	provBlk.NewCondBr(provBlk.NewICmp(enum.IPredEQ, stProv, i32c(0)), importBlk, provFailBlk)
	provFailBlk.NewCall(e.free, der)
	provFailBlk.NewRet(i32c(0))

	// NCryptBufferDesc carrying NCRYPTBUFFER_PKCS_KEY_NAME — the name the
	// imported PKCS#8 key gets in the provider's store.
	keyNameW := importBlk.NewCall(e.keyName, fn.Params[0])
	nb0 := importBlk.NewGetElementPtr(e.t.secBuf1, nameBuf, i32c(0), i32c(0))
	importBlk.NewStore(i32c(winKeyNameBytes), e.field(importBlk, e.t.secBuf, nb0, 0))
	importBlk.NewStore(i32c(winNCryptBufferPkcsKeyName), e.field(importBlk, e.t.secBuf, nb0, 1))
	importBlk.NewStore(keyNameW, e.field(importBlk, e.t.secBuf, nb0, 2))
	importBlk.NewStore(i32c(winNCryptBufferVersion), e.field(importBlk, e.t.secDesc, nameDesc, 0))
	importBlk.NewStore(i32c(1), e.field(importBlk, e.t.secDesc, nameDesc, 1))
	importBlk.NewStore(e.i8ptr(importBlk, nb0), e.field(importBlk, e.t.secDesc, nameDesc, 2))

	prov := importBlk.NewLoad(irtypes.I64, provP)
	stImp := importBlk.NewCall(e.ncryptImport, prov, i64c(0), e.blobName,
		e.i8ptr(importBlk, nameDesc), importBlk.NewBitCast(keyP, i64p), der,
		importBlk.NewTrunc(importBlk.NewLoad(irtypes.I64, dlen), irtypes.I32),
		i32c(winNCryptOverwriteKeyFlag))
	importBlk.NewCall(e.free, der)
	importFailBlk := fn.NewBlock(".import_failed")
	attachBlk := fn.NewBlock(".attach")
	importBlk.NewCondBr(importBlk.NewICmp(enum.IPredEQ, stImp, i32c(0)), attachBlk, importFailBlk)
	importFailBlk.NewCall(e.ncryptFree, prov)
	importFailBlk.NewCall(e.free, keyNameW)
	importFailBlk.NewRet(i32c(0))

	// CRYPT_KEY_PROV_INFO { pwszContainerName, pwszProvName, dwProvType = 0
	// (CNG), dwFlags, cProvParam, rgProvParam, dwKeySpec = 0 }.
	key := attachBlk.NewLoad(irtypes.I64, keyP)
	e.storePtrAt(attachBlk, kpi, winKeyProvOffContainer, keyNameW)
	e.storePtrAt(attachBlk, kpi, winKeyProvOffProvName, e.provName)
	e.storeI32At(attachBlk, kpi, winKeyProvOffFlags, i32c(winNCryptSilentFlag))
	ok := attachBlk.NewCall(e.certSetProp, cert, i32c(winCertKeyProvInfoPropID), i32c(0), kpi)
	attachFailBlk := fn.NewBlock(".attach_failed")
	adoptBlk := fn.NewBlock(".adopt")
	attachBlk.NewCondBr(attachBlk.NewICmp(enum.IPredNE, ok, i32c(0)), adoptBlk, attachFailBlk)
	attachFailBlk.NewCall(e.ncryptDelete, key, i32c(winNCryptSilentFlag))
	attachFailBlk.NewCall(e.ncryptFree, prov)
	attachFailBlk.NewCall(e.free, keyNameW)
	attachFailBlk.NewRet(i32c(0))

	// Replace any previously imported key. Release the superseded *handle* only —
	// NOT NCryptDeleteKey. __pal_tls_key_name is derived from the context address,
	// so a second import on the same context reuses the identical key name and
	// NCRYPT_OVERWRITE_KEY_FLAG has already replaced that store entry in place.
	// Deleting via the old handle would therefore remove the key just imported,
	// leaving the certificate's CRYPT_KEY_PROV_INFO pointing at nothing and every
	// later handshake failing. The store entry is deleted once, in
	// pal_tls_ctx_free.
	keyField := e.field(adoptBlk, e.t.ctx, c, winCtxFKey)
	provField := e.field(adoptBlk, e.t.ctx, c, winCtxFKeyProv)
	nameField := e.field(adoptBlk, e.t.ctx, c, winCtxFKeyName)
	oldKey := adoptBlk.NewLoad(irtypes.I64, keyField)
	dropKeyBlk := fn.NewBlock(".drop_old_key")
	afterKeyBlk := fn.NewBlock(".after_old_key")
	adoptBlk.NewCondBr(adoptBlk.NewICmp(enum.IPredNE, oldKey, i64c(0)), dropKeyBlk, afterKeyBlk)
	dropKeyBlk.NewCall(e.ncryptFree, oldKey)
	dropKeyBlk.NewBr(afterKeyBlk)

	oldProv := afterKeyBlk.NewLoad(irtypes.I64, provField)
	dropProvBlk := fn.NewBlock(".drop_old_prov")
	afterProvBlk := fn.NewBlock(".after_old_prov")
	afterKeyBlk.NewCondBr(afterKeyBlk.NewICmp(enum.IPredNE, oldProv, i64c(0)), dropProvBlk, afterProvBlk)
	dropProvBlk.NewCall(e.ncryptFree, oldProv)
	dropProvBlk.NewBr(afterProvBlk)

	oldName := afterProvBlk.NewLoad(i8p, nameField)
	dropNameBlk := fn.NewBlock(".drop_old_name")
	setBlk := fn.NewBlock(".set")
	afterProvBlk.NewCondBr(e.notNull(afterProvBlk, oldName), dropNameBlk, setBlk)
	dropNameBlk.NewCall(e.free, oldName)
	dropNameBlk.NewBr(setBlk)

	setBlk.NewStore(key, keyField)
	setBlk.NewStore(prov, provField)
	setBlk.NewStore(keyNameW, nameField)
	setBlk.NewRet(i32c(1))
	return fn
}

// emitCtxLoadDefaultTrust defines i32 @pal_tls_ctx_load_default_trust(i64 ctx).
// Windows always has a system trust store (the ROOT store), which chain building
// consults by default — nothing to load, so this always succeeds.
func (e *tlsWinEmitter) emitCtxLoadDefaultTrust() *ir.Func {
	fn := e.newFn("pal_tls_ctx_load_default_trust", irtypes.I32,
		ir.NewParam("ctx", irtypes.I64))
	b := fn.NewBlock(".entry")
	b.NewRet(i32c(1))
	return fn
}

// --- session ---------------------------------------------------------------

// emitNew defines i64 @pal_tls_new(i64 ctx) — allocates a session and seeds its
// three byte queues. Returns 0 when the credential cannot be acquired (e.g. the
// certificate has no usable private key), which tls.pr reports as a handshake
// failure.
func (e *tlsWinEmitter) emitNew() *ir.Func {
	fn := e.newFn("pal_tls_new", irtypes.I64, ir.NewParam("ctx", irtypes.I64))
	b := fn.NewBlock(".entry")
	failBlk := fn.NewBlock(".fail")
	checkBlk := fn.NewBlock(".check_cred")
	b.NewCondBr(b.NewICmp(enum.IPredEQ, fn.Params[0], i64c(0)), failBlk, checkBlk)
	failBlk.NewRet(i64c(0))

	credOK := checkBlk.NewCall(e.ensureCred, fn.Params[0])
	allocBlk := fn.NewBlock(".alloc")
	checkBlk.NewCondBr(checkBlk.NewICmp(enum.IPredNE, credOK, i32c(0)), allocBlk, failBlk)

	raw := allocBlk.NewCall(e.alloc, tlsWinSizeOf(e.t.sess))
	allocBlk.NewCall(e.memset, raw, i32c(0), tlsWinSizeOf(e.t.sess))
	s := allocBlk.NewBitCast(raw, e.t.sessP)
	allocBlk.NewStore(allocBlk.NewIntToPtr(fn.Params[0], irtypes.I8Ptr),
		e.field(allocBlk, e.t.sess, s, winSFCtx))
	for _, idx := range []int{winSFIn, winSFOut, winSFPlain} {
		q := e.field(allocBlk, e.t.sess, s, idx)
		allocBlk.NewStore(allocBlk.NewCall(e.alloc, i64c(winTLSBufInitCap)),
			e.field(allocBlk, e.t.buf, q, winBufFPtr))
		allocBlk.NewStore(i64c(winTLSBufInitCap), e.field(allocBlk, e.t.buf, q, winBufFCap))
	}
	allocBlk.NewRet(allocBlk.NewPtrToInt(raw, irtypes.I64))
	return fn
}

// emitSetState defines void @pal_tls_set_connect_state / _set_accept_state.
//
// On SChannel the client/server role is fixed by the credential direction chosen
// at context creation (AcquireCredentialsHandle OUTBOUND vs INBOUND), which
// tls.pr always pairs with the matching state call — so keeping the role on the
// context is the single source of truth and these are no-ops. The OpenSSL
// backend needs them, so they stay part of the shared surface.
func (e *tlsWinEmitter) emitSetState(name string) *ir.Func {
	fn := e.newFn(name, irtypes.Void, ir.NewParam("sess", irtypes.I64))
	b := fn.NewBlock(".entry")
	b.NewRet(nil)
	return fn
}

// emitSetSNI defines i32 @pal_tls_set_sni(i64 sess, i8* host) — copies the ANSI
// target name InitializeSecurityContext requires.
func (e *tlsWinEmitter) emitSetSNI() *ir.Func {
	i8p := irtypes.I8Ptr
	fn := e.newFn("pal_tls_set_sni", irtypes.I32,
		ir.NewParam("sess", irtypes.I64), ir.NewParam("host", i8p))
	b := fn.NewBlock(".entry")
	s := b.NewIntToPtr(fn.Params[0], e.t.sessP)
	slot := e.field(b, e.t.sess, s, winSFSNI)
	old := b.NewLoad(i8p, slot)
	dropBlk := fn.NewBlock(".drop_old")
	copyBlk := fn.NewBlock(".copy")
	b.NewCondBr(e.notNull(b, old), dropBlk, copyBlk)
	dropBlk.NewCall(e.free, old)
	dropBlk.NewBr(copyBlk)

	n := copyBlk.NewCall(e.strlen, fn.Params[1])
	dup := copyBlk.NewCall(e.alloc, copyBlk.NewAdd(n, i64c(1)))
	copyBlk.NewCall(e.memcpy, dup, fn.Params[1], copyBlk.NewAdd(n, i64c(1)))
	copyBlk.NewStore(dup, slot)
	copyBlk.NewRet(i32c(1))
	return fn
}

// emitSetVerifyHost defines i32 @pal_tls_set_verify_host(i64 sess, i8* host) —
// stores the UTF-16 name the SSL chain policy checks the certificate against.
func (e *tlsWinEmitter) emitSetVerifyHost() *ir.Func {
	i8p := irtypes.I8Ptr
	fn := e.newFn("pal_tls_set_verify_host", irtypes.I32,
		ir.NewParam("sess", irtypes.I64), ir.NewParam("host", i8p))
	b := fn.NewBlock(".entry")
	s := b.NewIntToPtr(fn.Params[0], e.t.sessP)
	slot := e.field(b, e.t.sess, s, winSFHostW)
	old := b.NewLoad(i8p, slot)
	dropBlk := fn.NewBlock(".drop_old")
	convBlk := fn.NewBlock(".convert")
	b.NewCondBr(e.notNull(b, old), dropBlk, convBlk)
	dropBlk.NewCall(e.free, old)
	dropBlk.NewBr(convBlk)

	w := convBlk.NewCall(e.widen, fn.Params[1])
	convBlk.NewStore(w, slot)
	convBlk.NewRet(convBlk.NewSelect(e.notNull(convBlk, w), i32c(1), i32c(0)))
	return fn
}

// emitDoHandshake defines i32 @pal_tls_do_handshake(i64 sess):
// 0 ok, 1 want_read, 2 want_write, -1 fatal.
//
// Only a client with no context yet may call SSPI with an empty input — that is
// the round that produces the ClientHello. Every other empty-queue case is a
// plain "need more bytes".
func (e *tlsWinEmitter) emitDoHandshake() *ir.Func {
	fn := e.newFn("pal_tls_do_handshake", irtypes.I32, ir.NewParam("sess", irtypes.I64))
	b := fn.NewBlock(".entry")
	fatalBlk := fn.NewBlock(".fatal")
	liveBlk := fn.NewBlock(".live")
	b.NewCondBr(b.NewICmp(enum.IPredEQ, fn.Params[0], i64c(0)), fatalBlk, liveBlk)
	fatalBlk.NewRet(i32c(-1))

	s := liveBlk.NewIntToPtr(fn.Params[0], e.t.sessP)
	c := liveBlk.NewBitCast(liveBlk.NewLoad(irtypes.I8Ptr,
		e.field(liveBlk, e.t.sess, s, winSFCtx)), e.t.ctxP)
	inLen := e.bufAvail(liveBlk, e.field(liveBlk, e.t.sess, s, winSFIn))
	empty := liveBlk.NewICmp(enum.IPredEQ, inLen, i64c(0))
	started := liveBlk.NewICmp(enum.IPredNE,
		liveBlk.NewLoad(irtypes.I32, e.field(liveBlk, e.t.sess, s, winSFCtxtValid)), i32c(0))
	isServer := liveBlk.NewICmp(enum.IPredNE,
		liveBlk.NewLoad(irtypes.I32, e.field(liveBlk, e.t.ctx, c, winCtxFIsServer)), i32c(0))
	wantRead := liveBlk.NewAnd(empty, liveBlk.NewOr(started, isServer))
	moreBlk := fn.NewBlock(".want_read")
	stepBlk := fn.NewBlock(".step")
	liveBlk.NewCondBr(wantRead, moreBlk, stepBlk)
	moreBlk.NewRet(i32c(1))
	stepBlk.NewRet(stepBlk.NewCall(e.hsStep, fn.Params[0], i32c(0)))
	return fn
}

// emitRead defines i64 @pal_tls_read(i64 sess, i8* buf, i64 len):
// >0 bytes, 0 clean EOF, -1 want_read, -2 want_write, -3 fatal.
func (e *tlsWinEmitter) emitRead() *ir.Func {
	i8p := irtypes.I8Ptr
	fn := e.newFn("pal_tls_read", irtypes.I64,
		ir.NewParam("sess", irtypes.I64), ir.NewParam("buf", i8p),
		ir.NewParam("len", irtypes.I64))
	entry := fn.NewBlock(".entry")
	s := entry.NewIntToPtr(fn.Params[0], e.t.sessP)
	// Allocas stay in the entry block: .decrypt is a loop header.
	bufs := entry.NewAlloca(e.t.secBuf4)
	desc := entry.NewAlloca(e.t.secDesc)
	dataPtr := entry.NewAlloca(i8p)
	dataLen := entry.NewAlloca(irtypes.I64)
	extraLen := entry.NewAlloca(irtypes.I64)

	// Anything decrypted earlier but not yet handed over comes first — read_line
	// asks for one byte at a time, so this is the common path.
	spilled := entry.NewCall(e.bufTake, e.sessBuf(entry, s, winSFPlain), fn.Params[1], fn.Params[2])
	haveSpill := entry.NewICmp(enum.IPredUGT, spilled, i64c(0))
	spillBlk := fn.NewBlock(".from_plain")
	checkEOFBlk := fn.NewBlock(".check_eof")
	entry.NewCondBr(haveSpill, spillBlk, checkEOFBlk)
	spillBlk.NewRet(spilled)

	eofBlk := fn.NewBlock(".eof")
	decryptBlk := fn.NewBlock(".decrypt")
	checkEOFBlk.NewCondBr(checkEOFBlk.NewICmp(enum.IPredNE,
		checkEOFBlk.NewLoad(irtypes.I32, e.field(checkEOFBlk, e.t.sess, s, winSFEOF)), i32c(0)),
		eofBlk, decryptBlk)
	eofBlk.NewRet(i64c(0))

	// --- loop head ---
	inQ := e.field(decryptBlk, e.t.sess, s, winSFIn)
	inLen := e.bufAvail(decryptBlk, inQ)
	wantReadBlk := fn.NewBlock(".want_read")
	haveInBlk := fn.NewBlock(".have_input")
	decryptBlk.NewCondBr(decryptBlk.NewICmp(enum.IPredEQ, inLen, i64c(0)), wantReadBlk, haveInBlk)
	wantReadBlk.NewRet(i64c(-1))

	inPtr := e.bufData(haveInBlk, inQ)
	b0 := haveInBlk.NewGetElementPtr(e.t.secBuf4, bufs, i32c(0), i32c(0))
	haveInBlk.NewStore(haveInBlk.NewTrunc(inLen, irtypes.I32), e.field(haveInBlk, e.t.secBuf, b0, 0))
	haveInBlk.NewStore(i32c(winSecBufferData), e.field(haveInBlk, e.t.secBuf, b0, 1))
	haveInBlk.NewStore(inPtr, e.field(haveInBlk, e.t.secBuf, b0, 2))
	for i := 1; i < 4; i++ {
		bi := haveInBlk.NewGetElementPtr(e.t.secBuf4, bufs, i32c(0), i32c(int64(i)))
		haveInBlk.NewStore(i32c(0), e.field(haveInBlk, e.t.secBuf, bi, 0))
		haveInBlk.NewStore(i32c(winSecBufferEmpty), e.field(haveInBlk, e.t.secBuf, bi, 1))
		haveInBlk.NewStore(tlsWinNull, e.field(haveInBlk, e.t.secBuf, bi, 2))
	}
	haveInBlk.NewStore(i32c(winSecBufferVersion), e.field(haveInBlk, e.t.secDesc, desc, 0))
	haveInBlk.NewStore(i32c(4), e.field(haveInBlk, e.t.secDesc, desc, 1))
	haveInBlk.NewStore(e.i8ptr(haveInBlk, b0), e.field(haveInBlk, e.t.secDesc, desc, 2))

	ctxtH := e.i8ptr(haveInBlk, e.field(haveInBlk, e.t.sess, s, winSFCtxt))
	st := haveInBlk.NewCall(e.decryptMsg, ctxtH, e.i8ptr(haveInBlk, desc), i32c(0),
		constant.NewNull(irtypes.NewPointer(irtypes.I32)))
	incMsg := haveInBlk.NewICmp(enum.IPredEQ, st, i32c(winSecEIncompleteMessage))
	partialBlk := fn.NewBlock(".partial_record")
	classifyBlk := fn.NewBlock(".classify")
	haveInBlk.NewCondBr(incMsg, partialBlk, classifyBlk)
	partialBlk.NewRet(i64c(-1))

	isOK := classifyBlk.NewICmp(enum.IPredEQ, st, i32c(winSecEOK))
	isExpired := classifyBlk.NewICmp(enum.IPredEQ, st, i32c(winSecIContextExpired))
	isRenegotiate := classifyBlk.NewICmp(enum.IPredEQ, st, i32c(winSecIRenegotiate))
	fatalBlk := fn.NewBlock(".fatal")
	fatalBlk.NewRet(i64c(-3))

	// The peer's close_notify carries no application data, so it gets its own
	// exit: latch EOF, drop whatever is left in the inbound queue, and report a
	// clean end-of-stream. Running it through the data path below would treat a
	// record SChannel has already consumed as if it still held plaintext.
	expiredBlk := fn.NewBlock(".peer_closed")
	expiredBlk.NewStore(i32c(1), e.field(expiredBlk, e.t.sess, s, winSFEOF))
	e.bufReset(expiredBlk, e.field(expiredBlk, e.t.sess, s, winSFIn))
	expiredBlk.NewRet(i64c(0))

	usable := classifyBlk.NewOr(isOK, isRenegotiate)
	scanInitBlk := fn.NewBlock(".scan_init")
	notUsableBlk := fn.NewBlock(".not_usable")
	classifyBlk.NewCondBr(usable, scanInitBlk, notUsableBlk)
	notUsableBlk.NewCondBr(isExpired, expiredBlk, fatalBlk)

	// SChannel rewrites the buffer types in place; find the plaintext and any
	// bytes of the next record it did not consume.
	scanInitBlk.NewStore(tlsWinNull, dataPtr)
	scanInitBlk.NewStore(i64c(0), dataLen)
	scanInitBlk.NewStore(i64c(0), extraLen)
	scanCondBlk := fn.NewBlock(".scan_cond")
	scanInitBlk.NewBr(scanCondBlk)
	scanBodyBlk := fn.NewBlock(".scan_body")
	deliverBlk := fn.NewBlock(".deliver")
	idx := scanCondBlk.NewPhi(ir.NewIncoming(i64c(0), scanInitBlk))
	scanCondBlk.NewCondBr(scanCondBlk.NewICmp(enum.IPredULT, idx, i64c(4)),
		scanBodyBlk, deliverBlk)

	bi := scanBodyBlk.NewGetElementPtr(e.t.secBuf4, bufs, i32c(0), idx)
	bt := scanBodyBlk.NewLoad(irtypes.I32, e.field(scanBodyBlk, e.t.secBuf, bi, 1))
	cb := scanBodyBlk.NewZExt(
		scanBodyBlk.NewLoad(irtypes.I32, e.field(scanBodyBlk, e.t.secBuf, bi, 0)), irtypes.I64)
	pv := scanBodyBlk.NewLoad(i8p, e.field(scanBodyBlk, e.t.secBuf, bi, 2))
	curPtr := scanBodyBlk.NewLoad(i8p, dataPtr)
	takeData := scanBodyBlk.NewAnd(
		scanBodyBlk.NewICmp(enum.IPredEQ, bt, i32c(winSecBufferData)),
		e.isNull(scanBodyBlk, curPtr))
	scanBodyBlk.NewStore(scanBodyBlk.NewSelect(takeData, pv, curPtr), dataPtr)
	scanBodyBlk.NewStore(scanBodyBlk.NewSelect(takeData, cb,
		scanBodyBlk.NewLoad(irtypes.I64, dataLen)), dataLen)
	curExtra := scanBodyBlk.NewLoad(irtypes.I64, extraLen)
	takeExtra := scanBodyBlk.NewAnd(
		scanBodyBlk.NewICmp(enum.IPredEQ, bt, i32c(winSecBufferExtra)),
		scanBodyBlk.NewICmp(enum.IPredEQ, curExtra, i64c(0)))
	scanBodyBlk.NewStore(scanBodyBlk.NewSelect(takeExtra, cb, curExtra), extraLen)
	next := scanBodyBlk.NewAdd(idx, i64c(1))
	scanBodyBlk.NewBr(scanCondBlk)
	idx.Incs = append(idx.Incs, ir.NewIncoming(next, scanBodyBlk))

	// Copy out what fits, spill the rest, and only then compact the input queue —
	// the plaintext points into it.
	dp := deliverBlk.NewLoad(i8p, dataPtr)
	dl := deliverBlk.NewLoad(irtypes.I64, dataLen)
	fits := deliverBlk.NewICmp(enum.IPredULT, fn.Params[2], dl)
	n := deliverBlk.NewSelect(fits, fn.Params[2], dl)
	copyBlk := fn.NewBlock(".copy_out")
	afterCopyBlk := fn.NewBlock(".after_copy")
	deliverBlk.NewCondBr(deliverBlk.NewICmp(enum.IPredUGT, n, i64c(0)), copyBlk, afterCopyBlk)
	copyBlk.NewCall(e.memcpy, fn.Params[1], dp, n)
	copyBlk.NewBr(afterCopyBlk)

	rest := afterCopyBlk.NewSub(dl, n)
	spillBlk2 := fn.NewBlock(".spill")
	consumeBlk := fn.NewBlock(".consume")
	afterCopyBlk.NewCondBr(afterCopyBlk.NewICmp(enum.IPredUGT, rest, i64c(0)), spillBlk2, consumeBlk)
	spillBlk2.NewCall(e.bufAppend, e.sessBuf(spillBlk2, s, winSFPlain),
		spillBlk2.NewGetElementPtr(irtypes.I8, dp, n), rest)
	spillBlk2.NewBr(consumeBlk)

	nowLen := e.bufAvail(consumeBlk, inQ)
	consumeBlk.NewCall(e.bufConsume, e.sessBuf(consumeBlk, s, winSFIn),
		consumeBlk.NewSub(nowLen, consumeBlk.NewLoad(irtypes.I64, extraLen)))
	// TLS 1.3 delivers session tickets and key updates as post-handshake
	// messages; SChannel surfaces them here and expects them fed back through
	// the handshake path, otherwise a perfectly healthy 1.3 link looks fatal.
	renegBlk := fn.NewBlock(".post_handshake")
	returnBlk := fn.NewBlock(".return")
	consumeBlk.NewCondBr(isRenegotiate, renegBlk, returnBlk)
	stepRC := renegBlk.NewCall(e.hsStep, fn.Params[0], i32c(0))
	renegBlk.NewCondBr(renegBlk.NewICmp(enum.IPredSLT, stepRC, i32c(0)), fatalBlk, returnBlk)

	deliveredBlk := fn.NewBlock(".delivered")
	moreBlk := fn.NewBlock(".more")
	returnBlk.NewCondBr(returnBlk.NewICmp(enum.IPredUGT, n, i64c(0)), deliveredBlk, moreBlk)
	deliveredBlk.NewRet(n)
	// Nothing to hand back yet (an empty record, or a pure post-handshake
	// message): report EOF if the peer closed, otherwise try the next record.
	moreBlk.NewCondBr(moreBlk.NewICmp(enum.IPredNE,
		moreBlk.NewLoad(irtypes.I32, e.field(moreBlk, e.t.sess, s, winSFEOF)), i32c(0)),
		eofBlk, decryptBlk)
	return fn
}

// emitWrite defines i64 @pal_tls_write(i64 sess, i8* buf, i64 len):
// >0 bytes accepted, -1 want_read, -2 want_write, -3 fatal. One TLS record is
// produced per call, so a caller writing more than cbMaximumMessage simply loops.
func (e *tlsWinEmitter) emitWrite() *ir.Func {
	i8p := irtypes.I8Ptr
	fn := e.newFn("pal_tls_write", irtypes.I64,
		ir.NewParam("sess", irtypes.I64), ir.NewParam("buf", i8p),
		ir.NewParam("len", irtypes.I64))
	b := fn.NewBlock(".entry")
	s := b.NewIntToPtr(fn.Params[0], e.t.sessP)
	bufs := b.NewAlloca(e.t.secBuf4)
	desc := b.NewAlloca(e.t.secDesc)

	scratch := b.NewLoad(i8p, e.field(b, e.t.sess, s, winSFScratch))
	done := b.NewICmp(enum.IPredNE,
		b.NewLoad(irtypes.I32, e.field(b, e.t.sess, s, winSFDone)), i32c(0))
	ready := b.NewAnd(done, e.notNull(b, scratch))
	fatalBlk := fn.NewBlock(".fatal")
	encryptBlk := fn.NewBlock(".encrypt")
	b.NewCondBr(ready, encryptBlk, fatalBlk)
	fatalBlk.NewRet(i64c(-3))

	hdr := encryptBlk.NewZExt(
		encryptBlk.NewLoad(irtypes.I32, e.field(encryptBlk, e.t.sess, s, winSFHdrLen)), irtypes.I64)
	trl := encryptBlk.NewZExt(
		encryptBlk.NewLoad(irtypes.I32, e.field(encryptBlk, e.t.sess, s, winSFTrlLen)), irtypes.I64)
	maxMsg := encryptBlk.NewZExt(
		encryptBlk.NewLoad(irtypes.I32, e.field(encryptBlk, e.t.sess, s, winSFMaxMsg)), irtypes.I64)
	capped := encryptBlk.NewICmp(enum.IPredULT, maxMsg, fn.Params[2])
	n := encryptBlk.NewSelect(capped, maxMsg, fn.Params[2])
	payload := encryptBlk.NewGetElementPtr(irtypes.I8, scratch, hdr)
	encryptBlk.NewCall(e.memcpy, payload, fn.Params[1], n)

	layout := []struct {
		size value.Value
		typ  int64
		ptr  value.Value
	}{
		{hdr, winSecBufferStreamHeader, scratch},
		{n, winSecBufferData, payload},
		{trl, winSecBufferStreamTrailer, encryptBlk.NewGetElementPtr(irtypes.I8, payload, n)},
	}
	for i, l := range layout {
		bi := encryptBlk.NewGetElementPtr(e.t.secBuf4, bufs, i32c(0), i32c(int64(i)))
		encryptBlk.NewStore(encryptBlk.NewTrunc(l.size, irtypes.I32),
			e.field(encryptBlk, e.t.secBuf, bi, 0))
		encryptBlk.NewStore(i32c(l.typ), e.field(encryptBlk, e.t.secBuf, bi, 1))
		encryptBlk.NewStore(l.ptr, e.field(encryptBlk, e.t.secBuf, bi, 2))
	}
	b3 := encryptBlk.NewGetElementPtr(e.t.secBuf4, bufs, i32c(0), i32c(3))
	encryptBlk.NewStore(i32c(0), e.field(encryptBlk, e.t.secBuf, b3, 0))
	encryptBlk.NewStore(i32c(winSecBufferEmpty), e.field(encryptBlk, e.t.secBuf, b3, 1))
	encryptBlk.NewStore(tlsWinNull, e.field(encryptBlk, e.t.secBuf, b3, 2))
	b0 := encryptBlk.NewGetElementPtr(e.t.secBuf4, bufs, i32c(0), i32c(0))
	encryptBlk.NewStore(i32c(winSecBufferVersion), e.field(encryptBlk, e.t.secDesc, desc, 0))
	encryptBlk.NewStore(i32c(4), e.field(encryptBlk, e.t.secDesc, desc, 1))
	encryptBlk.NewStore(e.i8ptr(encryptBlk, b0), e.field(encryptBlk, e.t.secDesc, desc, 2))

	ctxtH := e.i8ptr(encryptBlk, e.field(encryptBlk, e.t.sess, s, winSFCtxt))
	st := encryptBlk.NewCall(e.encryptMsg, ctxtH, i32c(0), e.i8ptr(encryptBlk, desc), i32c(0))
	queueBlk := fn.NewBlock(".queue")
	encryptBlk.NewCondBr(encryptBlk.NewICmp(enum.IPredEQ, st, i32c(winSecEOK)), queueBlk, fatalBlk)

	// EncryptMessage rewrites each cbBuffer with what it actually produced.
	var sum value.Value = i64c(0)
	for i := 0; i < 3; i++ {
		bi := queueBlk.NewGetElementPtr(e.t.secBuf4, bufs, i32c(0), i32c(int64(i)))
		part := queueBlk.NewZExt(
			queueBlk.NewLoad(irtypes.I32, e.field(queueBlk, e.t.secBuf, bi, 0)), irtypes.I64)
		sum = queueBlk.NewAdd(sum, part)
	}
	queueBlk.NewCall(e.bufAppend, e.sessBuf(queueBlk, s, winSFOut), scratch, sum)
	queueBlk.NewRet(n)
	return fn
}

// emitShutdown defines i32 @pal_tls_shutdown(i64 sess) — queues a close_notify
// into the outbound buffer (the caller flushes it) and always reports done, since
// TlsStream.close treats shutdown as best-effort. The shutdown_sent latch keeps a
// second call from queueing a second close_notify.
func (e *tlsWinEmitter) emitShutdown() *ir.Func {
	fn := e.newFn("pal_tls_shutdown", irtypes.I32, ir.NewParam("sess", irtypes.I64))
	b := fn.NewBlock(".entry")
	doneBlk := fn.NewBlock(".done")
	liveBlk := fn.NewBlock(".live")
	b.NewCondBr(b.NewICmp(enum.IPredEQ, fn.Params[0], i64c(0)), doneBlk, liveBlk)
	doneBlk.NewRet(i32c(0))

	s := liveBlk.NewIntToPtr(fn.Params[0], e.t.sessP)
	tok := liveBlk.NewAlloca(irtypes.I32)
	bufs := liveBlk.NewAlloca(e.t.secBuf1)
	desc := liveBlk.NewAlloca(e.t.secDesc)
	valid := liveBlk.NewICmp(enum.IPredNE,
		liveBlk.NewLoad(irtypes.I32, e.field(liveBlk, e.t.sess, s, winSFCtxtValid)), i32c(0))
	sentP := e.field(liveBlk, e.t.sess, s, winSFShutdown)
	fresh := liveBlk.NewICmp(enum.IPredEQ, liveBlk.NewLoad(irtypes.I32, sentP), i32c(0))
	sendBlk := fn.NewBlock(".send")
	liveBlk.NewCondBr(liveBlk.NewAnd(valid, fresh), sendBlk, doneBlk)

	sendBlk.NewStore(i32c(1), sentP)
	sendBlk.NewStore(i32c(winSchannelShutdown), tok)
	b0 := sendBlk.NewGetElementPtr(e.t.secBuf1, bufs, i32c(0), i32c(0))
	sendBlk.NewStore(i32c(4), e.field(sendBlk, e.t.secBuf, b0, 0))
	sendBlk.NewStore(i32c(winSecBufferToken), e.field(sendBlk, e.t.secBuf, b0, 1))
	sendBlk.NewStore(e.i8ptr(sendBlk, tok), e.field(sendBlk, e.t.secBuf, b0, 2))
	sendBlk.NewStore(i32c(winSecBufferVersion), e.field(sendBlk, e.t.secDesc, desc, 0))
	sendBlk.NewStore(i32c(1), e.field(sendBlk, e.t.secDesc, desc, 1))
	sendBlk.NewStore(e.i8ptr(sendBlk, b0), e.field(sendBlk, e.t.secDesc, desc, 2))
	sendBlk.NewCall(e.applyToken,
		e.i8ptr(sendBlk, e.field(sendBlk, e.t.sess, s, winSFCtxt)), e.i8ptr(sendBlk, desc))
	// One more SSPI round with no input turns the queued control token into the
	// close_notify record.
	sendBlk.NewCall(e.hsStep, fn.Params[0], i32c(1))
	sendBlk.NewRet(i32c(0))
	return fn
}

// --- ciphertext pump -------------------------------------------------------

// emitBioReadOut defines i64 @pal_tls_bio_read_out(i64 sess, i8* buf, i64 len).
func (e *tlsWinEmitter) emitBioReadOut() *ir.Func {
	i8p := irtypes.I8Ptr
	fn := e.newFn("pal_tls_bio_read_out", irtypes.I64,
		ir.NewParam("sess", irtypes.I64), ir.NewParam("buf", i8p),
		ir.NewParam("len", irtypes.I64))
	b := fn.NewBlock(".entry")
	s := b.NewIntToPtr(fn.Params[0], e.t.sessP)
	b.NewRet(b.NewCall(e.bufTake, e.sessBuf(b, s, winSFOut), fn.Params[1], fn.Params[2]))
	return fn
}

// emitBioWriteIn defines i64 @pal_tls_bio_write_in(i64 sess, i8* buf, i64 len).
func (e *tlsWinEmitter) emitBioWriteIn() *ir.Func {
	i8p := irtypes.I8Ptr
	fn := e.newFn("pal_tls_bio_write_in", irtypes.I64,
		ir.NewParam("sess", irtypes.I64), ir.NewParam("buf", i8p),
		ir.NewParam("len", irtypes.I64))
	b := fn.NewBlock(".entry")

	s := b.NewIntToPtr(fn.Params[0], e.t.sessP)
	b.NewCall(e.bufAppend, e.sessBuf(b, s, winSFIn), fn.Params[1], fn.Params[2])
	b.NewRet(fn.Params[2])
	return fn
}

// emitBioPendingOut defines i64 @pal_tls_bio_pending_out(i64 sess).
func (e *tlsWinEmitter) emitBioPendingOut() *ir.Func {
	fn := e.newFn("pal_tls_bio_pending_out", irtypes.I64, ir.NewParam("sess", irtypes.I64))
	b := fn.NewBlock(".entry")
	s := b.NewIntToPtr(fn.Params[0], e.t.sessP)
	b.NewRet(e.bufAvail(b, e.field(b, e.t.sess, s, winSFOut)))
	return fn
}

// --- introspection ---------------------------------------------------------

// emitGetVersion defines i8* @pal_tls_get_version(i64 sess) — a static string
// using OpenSSL's spelling, which is the shared vocabulary tls.pr compares.
func (e *tlsWinEmitter) emitGetVersion() *ir.Func {
	fn := e.newFn("pal_tls_get_version", irtypes.I8Ptr, ir.NewParam("sess", irtypes.I64))
	b := fn.NewBlock(".entry")
	s := b.NewIntToPtr(fn.Params[0], e.t.sessP)
	info := e.zeroed(b, 64) // SecPkgContext_ConnectionInfo is 28 bytes
	noneBlk := fn.NewBlock(".none")
	queryBlk := fn.NewBlock(".query")
	b.NewCondBr(b.NewICmp(enum.IPredNE,
		b.NewLoad(irtypes.I32, e.field(b, e.t.sess, s, winSFDone)), i32c(0)), queryBlk, noneBlk)
	noneBlk.NewRet(e.verEmpty)

	st := queryBlk.NewCall(e.queryCtxAttr,
		e.i8ptr(queryBlk, e.field(queryBlk, e.t.sess, s, winSFCtxt)),
		i32c(winSecpkgAttrConnectionInfo), info)
	readBlk := fn.NewBlock(".read")
	queryBlk.NewCondBr(queryBlk.NewICmp(enum.IPredEQ, st, i32c(winSecEOK)), readBlk, noneBlk)

	proto := e.loadI32At(readBlk, info, 0)
	is13 := readBlk.NewICmp(enum.IPredNE, readBlk.NewAnd(proto, i32c(winSpProtTLS13)), i32c(0))
	blk13 := fn.NewBlock(".tls13")
	chk12 := fn.NewBlock(".check_tls12")
	readBlk.NewCondBr(is13, blk13, chk12)
	blk13.NewRet(e.ver13)
	is12 := chk12.NewICmp(enum.IPredNE, chk12.NewAnd(proto, i32c(winSpProtTLS12)), i32c(0))
	blk12 := fn.NewBlock(".tls12")
	chk12.NewCondBr(is12, blk12, noneBlk)
	blk12.NewRet(e.ver12)
	return fn
}

// emitGetCipher defines i8* @pal_tls_get_cipher(i64 sess) — the negotiated suite
// name (e.g. "TLS_AES_256_GCM_SHA384") narrowed into a per-session buffer, or ""
// before the handshake completes. The buffer is owned by the session and freed
// in pal_tls_free; the bridge copies it into a Promise string immediately.
func (e *tlsWinEmitter) emitGetCipher() *ir.Func {
	i8p := irtypes.I8Ptr
	fn := e.newFn("pal_tls_get_cipher", i8p, ir.NewParam("sess", irtypes.I64))
	b := fn.NewBlock(".entry")
	s := b.NewIntToPtr(fn.Params[0], e.t.sessP)
	// SecPkgContext_CipherInfo is 680 bytes; szCipherSuite starts at offset 16.
	info := e.zeroed(b, 1024)
	slot := e.field(b, e.t.sess, s, winSFCipher)
	existing := b.NewLoad(i8p, slot)
	makeBlk := fn.NewBlock(".alloc_name")
	readyBlk := fn.NewBlock(".ready")
	b.NewCondBr(e.isNull(b, existing), makeBlk, readyBlk)
	fresh := makeBlk.NewCall(e.alloc, i64c(winTLSCipherNameCap))
	makeBlk.NewStore(constant.NewInt(irtypes.I8, 0), fresh) // "" until the handshake lands
	makeBlk.NewStore(fresh, slot)
	makeBlk.NewBr(readyBlk)

	name := readyBlk.NewLoad(i8p, slot)
	retBlk := fn.NewBlock(".ret")
	queryBlk := fn.NewBlock(".query")
	readyBlk.NewCondBr(readyBlk.NewICmp(enum.IPredNE,
		readyBlk.NewLoad(irtypes.I32, e.field(readyBlk, e.t.sess, s, winSFDone)), i32c(0)),
		queryBlk, retBlk)

	e.storeI32At(queryBlk, info, 0, i32c(1)) // SECPKGCONTEXT_CIPHERINFO_V1
	st := queryBlk.NewCall(e.queryCtxAttr,
		e.i8ptr(queryBlk, e.field(queryBlk, e.t.sess, s, winSFCtxt)),
		i32c(winSecpkgAttrCipherInfo), info)
	narrowBlk := fn.NewBlock(".narrow")
	queryBlk.NewCondBr(queryBlk.NewICmp(enum.IPredEQ, st, i32c(winSecEOK)), narrowBlk, retBlk)

	narrowBlk.NewCall(e.wc2mb, i32c(winCPUTF8), i32c(0), e.byteAt(narrowBlk, info, 16),
		i32c(-1), name, i32c(winTLSCipherNameCap), tlsWinNull, tlsWinNull)
	narrowBlk.NewBr(retBlk)
	retBlk.NewRet(name)
	return fn
}

// emitGetVerifyResult defines i32 @pal_tls_get_verify_result(i64 sess) — 0 when
// the peer certificate was acceptable (or was never checked).
func (e *tlsWinEmitter) emitGetVerifyResult() *ir.Func {
	fn := e.newFn("pal_tls_get_verify_result", irtypes.I32, ir.NewParam("sess", irtypes.I64))
	b := fn.NewBlock(".entry")
	zeroBlk := fn.NewBlock(".zero")
	liveBlk := fn.NewBlock(".live")
	b.NewCondBr(b.NewICmp(enum.IPredEQ, fn.Params[0], i64c(0)), zeroBlk, liveBlk)
	zeroBlk.NewRet(i32c(0))
	s := liveBlk.NewIntToPtr(fn.Params[0], e.t.sessP)
	liveBlk.NewRet(liveBlk.NewLoad(irtypes.I32, e.field(liveBlk, e.t.sess, s, winSFVerifyRes)))
	return fn
}

// emitFree defines void @pal_tls_free(i64 sess) — releases the security context
// and every allocation the session owns.
func (e *tlsWinEmitter) emitFree() *ir.Func {
	i8p := irtypes.I8Ptr
	fn := e.newFn("pal_tls_free", irtypes.Void, ir.NewParam("sess", irtypes.I64))
	b := fn.NewBlock(".entry")
	retBlk := fn.NewBlock(".ret")
	liveBlk := fn.NewBlock(".live")
	b.NewCondBr(b.NewICmp(enum.IPredEQ, fn.Params[0], i64c(0)), retBlk, liveBlk)
	retBlk.NewRet(nil)

	s := liveBlk.NewIntToPtr(fn.Params[0], e.t.sessP)
	valid := liveBlk.NewICmp(enum.IPredNE,
		liveBlk.NewLoad(irtypes.I32, e.field(liveBlk, e.t.sess, s, winSFCtxtValid)), i32c(0))
	delBlk := fn.NewBlock(".delete_ctxt")
	cur := fn.NewBlock(".buffers")
	liveBlk.NewCondBr(valid, delBlk, cur)
	delBlk.NewCall(e.deleteSecCtx, e.i8ptr(delBlk, e.field(delBlk, e.t.sess, s, winSFCtxt)))
	delBlk.NewBr(cur)

	for _, idx := range []int{winSFIn, winSFOut, winSFPlain} {
		cur.NewCall(e.bufFree, e.sessBuf(cur, s, idx))
	}
	for i, idx := range []int{winSFSNI, winSFHostW, winSFCipher, winSFScratch} {
		slot := e.field(cur, e.t.sess, s, idx)
		ptr := cur.NewLoad(i8p, slot)
		freeBlk := fn.NewBlock(".free_" + itoa(i))
		nextBlk := fn.NewBlock(".after_" + itoa(i))
		cur.NewCondBr(e.notNull(cur, ptr), freeBlk, nextBlk)
		freeBlk.NewCall(e.free, ptr)
		freeBlk.NewBr(nextBlk)
		cur = nextBlk
	}
	cur.NewCall(e.free, e.i8ptr(cur, s))
	cur.NewRet(nil)
	return fn
}
