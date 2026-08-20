package pal

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// tls_schannel.go — SChannel (SSPI) TLS backend for Windows (T1598).
//
// Implements exactly the same fd-free, reactor-agnostic pal_tls_* surface the
// OpenSSL backend provides (tls_posix.go, T0077 §4), so modules/tls/tls.pr and
// the codegen bridge (codegen/tls.go) stay backend-agnostic: Promise still owns
// the socket and drives the ciphertext pump over net.TcpStream, and this backend
// only transforms buffers. SChannel is buffer-oriented by design, which is what
// makes it a drop-in behind that surface.
//
// The status enum is the backend-neutral contract — no SECURITY_STATUS ever
// crosses the PAL boundary:
//
//	handshake: 0 ok, 1 want_read, 2 want_write, -1 fatal
//	read:      >0 bytes, 0 EOF (clean close_notify), -1 want_read, -2 want_write, -3 fatal
//	write:     >0 bytes,        -1 want_read, -2 want_write, -3 fatal
//	shutdown:  0 done, 1 want_read, 2 want_write, 3 call-again, -1 fatal
//
// Handles (context, session) cross the boundary as i64 (ptrtoint) so they map
// onto Promise `int`; 0 means failure, exactly as on the OpenSSL backend.
//
// Ownership: every context/session/buffer allocation goes through
// pal_alloc/pal_realloc/pal_free, so the per-test leak detector (T0067) verifies
// the teardown paths. OS-side objects (credential handle, security context, cert
// context, cert store, CNG key) are released explicitly in ctx_free / free.
//
// Concurrency: a session is driven by exactly one goroutine (Promise's ownership
// rules give TlsStream/TlsListener an exclusive borrow for the duration of every
// method that touches the handle), so no lock is needed around the lazy
// credential acquisition in pal_tls_new.

// --- SSPI / SChannel pinned ABI constants ---------------------------------
// Values from the Windows SDK headers (sspi.h, schannel.h, wincrypt.h,
// winerror.h). SECURITY_STATUS is an HRESULT, so the 0x8... codes are written
// as their signed i32 value.
const (
	// SECURITY_STATUS (sspi.h / winerror.h)
	winSecEOK                    = 0
	winSecIContinueNeeded        = 0x00090312
	winSecICompleteNeeded        = 0x00090313
	winSecICompleteAndContinue   = 0x00090314
	winSecIContextExpired        = 0x00090317
	winSecIIncompleteCredentials = 0x00090320
	winSecIRenegotiate           = 0x00090321
	winSecEIncompleteMessage     = -2146893032 // 0x80090318

	// SecBuffer.BufferType (sspi.h)
	winSecBufferEmpty         = 0
	winSecBufferData          = 1
	winSecBufferToken         = 2
	winSecBufferExtra         = 5
	winSecBufferStreamTrailer = 6
	winSecBufferStreamHeader  = 7

	winSecBufferVersion = 0 // SECBUFFER_VERSION

	// InitializeSecurityContext / AcceptSecurityContext request flags (sspi.h).
	// MANUAL_CRED_VALIDATION: this backend always validates the peer itself in
	// __pal_tls_verify, because custom trust anchors (add_root_certificate)
	// cannot be expressed to SChannel's automatic validation path.
	winISCReq = 0x00000008 | // ISC_REQ_SEQUENCE_DETECT
		0x00000004 | // ISC_REQ_REPLAY_DETECT
		0x00000010 | // ISC_REQ_CONFIDENTIALITY
		0x00000100 | // ISC_REQ_ALLOCATE_MEMORY
		0x00004000 | // ISC_REQ_EXTENDED_ERROR
		0x00008000 | // ISC_REQ_STREAM
		0x00080000 // ISC_REQ_MANUAL_CRED_VALIDATION
	winASCReq = 0x00000008 | // ASC_REQ_SEQUENCE_DETECT
		0x00000004 | // ASC_REQ_REPLAY_DETECT
		0x00000010 | // ASC_REQ_CONFIDENTIALITY
		0x00000100 | // ASC_REQ_ALLOCATE_MEMORY
		0x00008000 | // ASC_REQ_EXTENDED_ERROR
		0x00010000 // ASC_REQ_STREAM

	winSecurityNativeDrep = 0x00000010 // SECURITY_NATIVE_DREP

	// AcquireCredentialsHandle direction (sspi.h)
	winSecpkgCredInbound  = 1 // server
	winSecpkgCredOutbound = 2 // client

	// QueryContextAttributes ulAttribute (schannel.h / sspi.h)
	winSecpkgAttrStreamSizes       = 4
	winSecpkgAttrRemoteCertContext = 0x53
	winSecpkgAttrConnectionInfo    = 90
	winSecpkgAttrCipherInfo        = 100

	// SCH_CREDENTIALS (schannel.h). dwVersion = SCH_CREDENTIALS_VERSION. This is
	// the TLS-1.3-capable credential structure, available from Windows 10 1809 —
	// the same floor the rest of the Windows link surface already assumes
	// (ucrtbase.dll ships with Windows 10+). Its predecessor SCHANNEL_CRED
	// (dwVersion 4) cannot express TLS 1.3.
	winSchCredentialsVersion = 5
	winSchCredentialsSize    = 72 // sizeof(SCH_CREDENTIALS) on x64
	winTLSParametersSize     = 40 // sizeof(TLS_PARAMETERS) on x64
	// SCH_CREDENTIALS field byte offsets (x64).
	winSchOffDwVersion     = 0
	winSchOffCCreds        = 8
	winSchOffPaCred        = 16
	winSchOffDwFlags       = 52
	winSchOffCTLSParams    = 56
	winSchOffPTLSParams    = 64
	winTLSParamOffDisabled = 16 // TLS_PARAMETERS.grbitDisabledProtocols

	// SCH_CREDENTIALS.dwFlags (schannel.h)
	winSchUseStrongCrypto          = 0x00400000
	winSchCredManualCredValidation = 0x00000008
	winSchCredNoDefaultCreds       = 0x00000010

	// SP_PROT_* protocol bits used in TLS_PARAMETERS.grbitDisabledProtocols
	// (schannel.h). Client and server bits are disabled together — the direction
	// is already fixed by the credential.
	winSpProtBelowTLS12 = 0x004 | 0x008 | // SSL2 server|client
		0x010 | 0x020 | // SSL3 server|client
		0x040 | 0x080 | // TLS1.0 server|client
		0x100 | 0x200 // TLS1.1 server|client
	winSpProtTLS12 = 0x400 | 0x800 // TLS1.2 server|client
	winSpProtTLS13 = 0x1000 | 0x2000

	// ApplyControlToken (schannel.h)
	winSchannelShutdown = 1

	// wincrypt.h
	winCryptStringBase64Header = 0x00000003 // CRYPT_STRING_BASE64HEADER
	winX509AsnEncoding         = 0x00000001
	winPkcs7AsnEncoding        = 0x00010000
	winCertEncodingAny         = winX509AsnEncoding | winPkcs7AsnEncoding
	winCertStoreProvMemory     = 2 // (LPCSTR) CERT_STORE_PROV_MEMORY
	winCertStoreCreateNew      = 0x2000
	winCertStoreAddAlways      = 4
	winCertCloseStoreForce     = 1
	winCertFindExisting        = 0x000D0000 // CERT_COMPARE_EXISTING << CERT_COMPARE_SHIFT

	// Certificate → private-key association. SChannel resolves a credential's
	// private key through CryptAcquireCertificatePrivateKey, which only consults
	// CERT_KEY_PROV_INFO_PROP_ID — an ephemeral key handle attached via
	// CERT_KEY_CONTEXT_PROP_ID is ignored and the credential fails with
	// SEC_E_NO_CREDENTIALS. So the imported key must be a *named* CNG key; see
	// emitCtxUseKey for how its lifetime is bounded.
	winCertKeyProvInfoPropID   = 2  // CERT_KEY_PROV_INFO_PROP_ID
	winNCryptBufferPkcsKeyName = 45 // NCRYPTBUFFER_PKCS_KEY_NAME
	winNCryptBufferVersion     = 0  // NCRYPTBUFFER_VERSION
	winNCryptOverwriteKeyFlag  = 0x00000080
	winNCryptSilentFlag        = 0x00000040
	winKeyProvInfoSize         = 48 // sizeof(CRYPT_KEY_PROV_INFO) on x64
	winKeyProvOffContainer     = 0
	winKeyProvOffProvName      = 8
	winKeyProvOffFlags         = 20

	// Per-context CNG key name, as UTF-16: "promise-tls-" + 16 hex digits of the
	// context address + "-" + 8 hex digits of the process id + NUL. The address
	// is unique among live contexts by construction and the pid separates
	// concurrent processes, so two live keys can never collide.
	winKeyNamePrefixUnits = 12
	winKeyNameUnits       = 38
	winKeyNameBytes       = winKeyNameUnits * 2

	// CertGetCertificateChain flags — both are required to keep the
	// never-blocks invariant: default chain building may fetch AIA/CRL over the
	// network, which would stall the scheduler thread running this goroutine.
	winCertChainCacheOnlyURL = 0x00000004 // CERT_CHAIN_CACHE_ONLY_URL_RETRIEVAL
	winCertChainDisableAIA   = 0x00002000 // CERT_CHAIN_DISABLE_AIA

	winCertChainParaSize  = 32 // sizeof(CERT_CHAIN_PARA) without extra fields
	winCertChainPolicySSL = 4  // (LPCSTR) CERT_CHAIN_POLICY_SSL
	// CERT_CHAIN_CONTEXT / CERT_SIMPLE_CHAIN share a leading
	// { DWORD cbSize; CERT_TRUST_STATUS TrustStatus; } prefix.
	winChainOffTrustError     = 4
	winCertTrustUntrustedRoot = 0x00000020
	winCertTrustPartialChain  = 0x00010000
	winAuthTypeClient         = 1
	winAuthTypeServer         = 2

	// CERT_CHAIN_CONTEXT / CERT_SIMPLE_CHAIN / CERT_CHAIN_ELEMENT offsets (x64).
	winChainOffRgpChain    = 16 // CERT_CHAIN_CONTEXT.rgpChain
	winSimpleOffCElement   = 12 // CERT_SIMPLE_CHAIN.cElement
	winSimpleOffRgpElement = 16 // CERT_SIMPLE_CHAIN.rgpElement
	winElemOffCertContext  = 8  // CERT_CHAIN_ELEMENT.pCertContext

	// HTTPSPolicyCallbackData / CERT_CHAIN_POLICY_PARA / _STATUS sizes (x64).
	winSSLPolicyParaSize   = 24
	winChainPolicyParaSize = 16
	winChainPolicyStatSize = 24

	winCPUTF8 = 65001 // CP_UTF8

	// Initial capacity of each per-session byte queue. One TLS record is at most
	// ~18 KiB, so this covers the common case without a realloc while still
	// growing on demand.
	winTLSBufInitCap = 16384
	// Size of the ANSI cipher-suite name buffer (SChannel names are far shorter).
	winTLSCipherNameCap = 128
)

// tlsWinWStr defines an immutable, NUL-terminated UTF-16 string global and
// returns an i8* constant to its first byte. SSPI/CNG take LPCWSTR for provider
// and blob-type names.
func tlsWinWStr(module *ir.Module, name, s string) constant.Constant {
	units := make([]constant.Constant, 0, len(s)+1)
	for _, r := range s {
		units = append(units, constant.NewInt(irtypes.I16, int64(r)))
	}
	units = append(units, constant.NewInt(irtypes.I16, 0))
	arrTyp := irtypes.NewArray(uint64(len(units)), irtypes.I16)
	arr := constant.NewArray(arrTyp, units...)
	g := module.NewGlobal(name, arrTyp)
	g.Init = arr
	g.Immutable = true
	zero := constant.NewInt(irtypes.I64, 0)
	return constant.NewBitCast(constant.NewGetElementPtr(arrTyp, g, zero, zero), irtypes.I8Ptr)
}

// tlsWinTypes bundles the LLVM struct layouts this backend works with, created
// once per EmitTLS so every GEP uses the identical type instance.
type tlsWinTypes struct {
	buf     *irtypes.StructType // { i8* ptr, i64 len, i64 cap, i64 off }
	bufP    *irtypes.PointerType
	ctx     *irtypes.StructType
	ctxP    *irtypes.PointerType
	sess    *irtypes.StructType
	sessP   *irtypes.PointerType
	secBuf  *irtypes.StructType // SecBuffer  { ULONG cbBuffer; ULONG BufferType; void* pvBuffer; }
	secDesc *irtypes.StructType // SecBufferDesc { ULONG ulVersion; ULONG cBuffers; PSecBuffer pBuffers; }
	secBuf2 *irtypes.ArrayType
	secBuf4 *irtypes.ArrayType
	secBuf1 *irtypes.ArrayType
	handle  *irtypes.ArrayType // SecHandle / CredHandle / CtxtHandle: [2 x i64]
}

// Field indices into the byte-queue struct (tlsWinTypes.buf). Valid bytes are
// [off, len); see emitBufHelpers for why the read cursor exists.
const (
	winBufFPtr = 0
	winBufFLen = 1
	winBufFCap = 2
	winBufFOff = 3
)

// Field indices into the context struct (tlsWinTypes.ctx).
const (
	winCtxFCred      = 0 // CredHandle
	winCtxFCredValid = 1
	winCtxFIsServer  = 2
	winCtxFVerify    = 3
	winCtxFDisabled  = 4 // grbitDisabledProtocols
	winCtxFCert      = 5 // PCCERT_CONTEXT
	winCtxFRoots     = 6 // HCERTSTORE of extra trust anchors
	winCtxFKey       = 7 // NCRYPT_KEY_HANDLE
	winCtxFKeyProv   = 8 // NCRYPT_PROV_HANDLE
	winCtxFKeyName   = 9 // UTF-16 CNG key name (pal_alloc'd)
)

// Field indices into the session struct (tlsWinTypes.sess).
const (
	winSFCtxt      = 0 // CtxtHandle
	winSFCtx       = 1 // i8* → context struct
	winSFCtxtValid = 2
	winSFDone      = 3 // handshake completed
	winSFVerifyRes = 4
	winSFEOF       = 5
	winSFShutdown  = 6
	winSFHdrLen    = 7
	winSFTrlLen    = 8
	winSFMaxMsg    = 9
	winSFIn        = 10 // inbound ciphertext queue
	winSFOut       = 11 // outbound ciphertext queue
	winSFPlain     = 12 // decrypted-but-undelivered plaintext
	winSFSNI       = 13 // ANSI target name
	winSFHostW     = 14 // UTF-16 verify host
	winSFCipher    = 15 // ANSI cipher-suite name scratch
	winSFScratch   = 16 // EncryptMessage scratch (header+max+trailer)
)

func newTLSWinTypes() *tlsWinTypes {
	t := &tlsWinTypes{}
	i8p := irtypes.I8Ptr
	t.handle = irtypes.NewArray(2, irtypes.I64)
	t.buf = irtypes.NewStruct(i8p, irtypes.I64, irtypes.I64, irtypes.I64)
	t.bufP = irtypes.NewPointer(t.buf)
	t.ctx = irtypes.NewStruct(
		t.handle,    // cred
		irtypes.I32, // cred_valid
		irtypes.I32, // is_server
		irtypes.I32, // verify
		irtypes.I32, // disabled protocols
		i8p,         // cert
		i8p,         // extra roots store
		irtypes.I64, // key
		irtypes.I64, // key provider
		i8p,         // key name (UTF-16)
	)
	t.ctxP = irtypes.NewPointer(t.ctx)
	t.sess = irtypes.NewStruct(
		t.handle,    // ctxt
		i8p,         // ctx
		irtypes.I32, // ctxt_valid
		irtypes.I32, // handshake_done
		irtypes.I32, // verify_result
		irtypes.I32, // eof
		irtypes.I32, // shutdown_sent
		irtypes.I32, // header len
		irtypes.I32, // trailer len
		irtypes.I32, // max message
		t.buf,       // in
		t.buf,       // out
		t.buf,       // plain
		i8p,         // sni
		i8p,         // verify host (UTF-16)
		i8p,         // cipher name
		i8p,         // encrypt scratch
	)
	t.sessP = irtypes.NewPointer(t.sess)
	t.secBuf = irtypes.NewStruct(irtypes.I32, irtypes.I32, i8p)
	t.secDesc = irtypes.NewStruct(irtypes.I32, irtypes.I32, i8p)
	t.secBuf1 = irtypes.NewArray(1, t.secBuf)
	t.secBuf2 = irtypes.NewArray(2, t.secBuf)
	t.secBuf4 = irtypes.NewArray(4, t.secBuf)
	return t
}

// tlsWinEmitter carries the per-EmitTLS state (module, types, common externs and
// small IR-building helpers) so the individual wrapper emitters stay readable.
type tlsWinEmitter struct {
	m *ir.Module
	t *tlsWinTypes

	// libc / PAL
	alloc   *ir.Func
	free    *ir.Func
	realloc *ir.Func
	memcpy  *ir.Func
	memmove *ir.Func
	memset  *ir.Func
	strlen  *ir.Func

	// kernel32
	mb2wc *ir.Func
	wc2mb *ir.Func

	// secur32 (SSPI)
	acquireCred  *ir.Func
	freeCred     *ir.Func
	initSecCtx   *ir.Func
	acceptSecCtx *ir.Func
	deleteSecCtx *ir.Func
	freeCtxBuf   *ir.Func
	queryCtxAttr *ir.Func
	encryptMsg   *ir.Func
	decryptMsg   *ir.Func
	applyToken   *ir.Func

	// crypt32
	strToBin       *ir.Func
	certCreate     *ir.Func
	certFree       *ir.Func
	certOpenStore  *ir.Func
	certCloseStore *ir.Func
	certAddEnc     *ir.Func
	certFind       *ir.Func
	certGetChain   *ir.Func
	certFreeChain  *ir.Func
	certVerifyPol  *ir.Func
	certSetProp    *ir.Func

	// ncrypt
	ncryptOpenProv *ir.Func
	ncryptImport   *ir.Func
	ncryptFree     *ir.Func
	ncryptDelete   *ir.Func
	getPid         *ir.Func

	// emitted helpers
	bufAppend  *ir.Func
	bufTake    *ir.Func
	bufConsume *ir.Func
	bufFree    *ir.Func
	pemDER     *ir.Func
	widen      *ir.Func
	ensureCred *ir.Func
	hsStep     *ir.Func
	verifyPeer *ir.Func
	keyName    *ir.Func
	hexWide    *ir.Func

	// string globals
	pkgName   constant.Constant
	provName  constant.Constant
	blobName  constant.Constant
	keyPrefix constant.Constant
	verEmpty  constant.Constant
	ver12     constant.Constant
	ver13     constant.Constant
}

func i32c(v int64) *constant.Int { return constant.NewInt(irtypes.I32, v) }
func i64c(v int64) *constant.Int { return constant.NewInt(irtypes.I64, v) }

var tlsWinNull = constant.NewNull(irtypes.I8Ptr)

// field returns a pointer to field idx of the struct typ at p (p must already
// have type *typ).
func (e *tlsWinEmitter) field(b *ir.Block, typ irtypes.Type, p value.Value, idx int) value.Value {
	return b.NewGetElementPtr(typ, p, i32c(0), i32c(int64(idx)))
}

// byteAt returns p + off as an i8*.
func (e *tlsWinEmitter) byteAt(b *ir.Block, p value.Value, off int64) value.Value {
	if off == 0 {
		return p
	}
	return b.NewGetElementPtr(irtypes.I8, p, i64c(off))
}

func (e *tlsWinEmitter) storeI32At(b *ir.Block, base value.Value, off int64, v value.Value) {
	b.NewStore(v, b.NewBitCast(e.byteAt(b, base, off), irtypes.NewPointer(irtypes.I32)))
}

func (e *tlsWinEmitter) loadI32At(b *ir.Block, base value.Value, off int64) value.Value {
	return b.NewLoad(irtypes.I32, b.NewBitCast(e.byteAt(b, base, off), irtypes.NewPointer(irtypes.I32)))
}

func (e *tlsWinEmitter) storePtrAt(b *ir.Block, base value.Value, off int64, v value.Value) {
	b.NewStore(v, b.NewBitCast(e.byteAt(b, base, off), irtypes.NewPointer(irtypes.I8Ptr)))
}

func (e *tlsWinEmitter) loadPtrAt(b *ir.Block, base value.Value, off int64) value.Value {
	return b.NewLoad(irtypes.I8Ptr, b.NewBitCast(e.byteAt(b, base, off), irtypes.NewPointer(irtypes.I8Ptr)))
}

// zeroed allocates `size` bytes of stack scratch, zeroes it, and returns an i8*.
// The alloca is emitted into b, so callers must place it in the entry block when
// the value is used inside a loop.
func (e *tlsWinEmitter) zeroed(b *ir.Block, size int64) value.Value {
	a := b.NewAlloca(irtypes.NewArray(uint64(size), irtypes.I8))
	p := b.NewBitCast(a, irtypes.I8Ptr)
	b.NewCall(e.memset, p, i32c(0), i64c(size))
	return p
}

// i8ptr bitcasts any pointer value to i8*.
func (e *tlsWinEmitter) i8ptr(b *ir.Block, v value.Value) value.Value {
	return b.NewBitCast(v, irtypes.I8Ptr)
}

// isNull / notNull build the obvious i1 comparisons.
func (e *tlsWinEmitter) isNull(b *ir.Block, v value.Value) value.Value {
	return b.NewICmp(enum.IPredEQ, v, tlsWinNull)
}

func (e *tlsWinEmitter) notNull(b *ir.Block, v value.Value) value.Value {
	return b.NewICmp(enum.IPredNE, v, tlsWinNull)
}

// newFn creates a nounwind function.
func (e *tlsWinEmitter) newFn(name string, ret irtypes.Type, params ...*ir.Param) *ir.Func {
	fn := e.m.NewFunc(name, ret, params...)
	fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
	return fn
}

// declareExterns declares every libc / Win32 symbol this backend calls.
func (e *tlsWinEmitter) declareExterns() {
	m := e.m
	i8p := irtypes.I8Ptr
	i32p := irtypes.NewPointer(irtypes.I32)
	i64p := irtypes.NewPointer(irtypes.I64)

	e.alloc = getOrDeclareFunc(m, "pal_alloc", i8p, ir.NewParam("size", irtypes.I64))
	e.free = getOrDeclareFunc(m, "pal_free", irtypes.Void, ir.NewParam("ptr", i8p))
	e.realloc = getOrDeclareFunc(m, "pal_realloc", i8p,
		ir.NewParam("ptr", i8p), ir.NewParam("size", irtypes.I64))
	e.memcpy = getOrDeclareFunc(m, "memcpy", i8p,
		ir.NewParam("dst", i8p), ir.NewParam("src", i8p), ir.NewParam("n", irtypes.I64))
	e.memmove = getOrDeclareFunc(m, "memmove", i8p,
		ir.NewParam("dst", i8p), ir.NewParam("src", i8p), ir.NewParam("n", irtypes.I64))
	e.memset = getOrDeclareFunc(m, "memset", i8p,
		ir.NewParam("dst", i8p), ir.NewParam("c", irtypes.I32), ir.NewParam("n", irtypes.I64))
	e.strlen = getOrDeclareFunc(m, "strlen", irtypes.I64, ir.NewParam("s", i8p))

	e.mb2wc = getOrDeclareFunc(m, "MultiByteToWideChar", irtypes.I32,
		ir.NewParam("cp", irtypes.I32), ir.NewParam("flags", irtypes.I32),
		ir.NewParam("src", i8p), ir.NewParam("srcLen", irtypes.I32),
		ir.NewParam("dst", i8p), ir.NewParam("dstLen", irtypes.I32))
	e.wc2mb = getOrDeclareFunc(m, "WideCharToMultiByte", irtypes.I32,
		ir.NewParam("cp", irtypes.I32), ir.NewParam("flags", irtypes.I32),
		ir.NewParam("src", i8p), ir.NewParam("srcLen", irtypes.I32),
		ir.NewParam("dst", i8p), ir.NewParam("dstLen", irtypes.I32),
		ir.NewParam("defChar", i8p), ir.NewParam("usedDef", i8p))

	e.acquireCred = getOrDeclareFunc(m, "AcquireCredentialsHandleA", irtypes.I32,
		ir.NewParam("principal", i8p), ir.NewParam("pkg", i8p),
		ir.NewParam("use", irtypes.I32), ir.NewParam("logonId", i8p),
		ir.NewParam("authData", i8p), ir.NewParam("getKeyFn", i8p),
		ir.NewParam("getKeyArg", i8p), ir.NewParam("cred", i8p),
		ir.NewParam("expiry", i8p))
	e.freeCred = getOrDeclareFunc(m, "FreeCredentialsHandle", irtypes.I32,
		ir.NewParam("cred", i8p))
	e.initSecCtx = getOrDeclareFunc(m, "InitializeSecurityContextA", irtypes.I32,
		ir.NewParam("cred", i8p), ir.NewParam("ctxt", i8p),
		ir.NewParam("target", i8p), ir.NewParam("req", irtypes.I32),
		ir.NewParam("reserved1", irtypes.I32), ir.NewParam("drep", irtypes.I32),
		ir.NewParam("input", i8p), ir.NewParam("reserved2", irtypes.I32),
		ir.NewParam("newCtxt", i8p), ir.NewParam("output", i8p),
		ir.NewParam("attr", i32p), ir.NewParam("expiry", i8p))
	e.acceptSecCtx = getOrDeclareFunc(m, "AcceptSecurityContext", irtypes.I32,
		ir.NewParam("cred", i8p), ir.NewParam("ctxt", i8p),
		ir.NewParam("input", i8p), ir.NewParam("req", irtypes.I32),
		ir.NewParam("drep", irtypes.I32), ir.NewParam("newCtxt", i8p),
		ir.NewParam("output", i8p), ir.NewParam("attr", i32p),
		ir.NewParam("expiry", i8p))
	e.deleteSecCtx = getOrDeclareFunc(m, "DeleteSecurityContext", irtypes.I32,
		ir.NewParam("ctxt", i8p))
	e.freeCtxBuf = getOrDeclareFunc(m, "FreeContextBuffer", irtypes.I32,
		ir.NewParam("buf", i8p))
	e.queryCtxAttr = getOrDeclareFunc(m, "QueryContextAttributesA", irtypes.I32,
		ir.NewParam("ctxt", i8p), ir.NewParam("attr", irtypes.I32),
		ir.NewParam("out", i8p))
	e.encryptMsg = getOrDeclareFunc(m, "EncryptMessage", irtypes.I32,
		ir.NewParam("ctxt", i8p), ir.NewParam("qop", irtypes.I32),
		ir.NewParam("msg", i8p), ir.NewParam("seq", irtypes.I32))
	e.decryptMsg = getOrDeclareFunc(m, "DecryptMessage", irtypes.I32,
		ir.NewParam("ctxt", i8p), ir.NewParam("msg", i8p),
		ir.NewParam("seq", irtypes.I32), ir.NewParam("qop", i32p))
	e.applyToken = getOrDeclareFunc(m, "ApplyControlToken", irtypes.I32,
		ir.NewParam("ctxt", i8p), ir.NewParam("msg", i8p))

	e.strToBin = getOrDeclareFunc(m, "CryptStringToBinaryA", irtypes.I32,
		ir.NewParam("str", i8p), ir.NewParam("cch", irtypes.I32),
		ir.NewParam("flags", irtypes.I32), ir.NewParam("bin", i8p),
		ir.NewParam("cbBin", i32p), ir.NewParam("skip", i32p),
		ir.NewParam("outFlags", i32p))
	e.certCreate = getOrDeclareFunc(m, "CertCreateCertificateContext", i8p,
		ir.NewParam("enc", irtypes.I32), ir.NewParam("der", i8p),
		ir.NewParam("len", irtypes.I32))
	e.certFree = getOrDeclareFunc(m, "CertFreeCertificateContext", irtypes.I32,
		ir.NewParam("cert", i8p))
	e.certOpenStore = getOrDeclareFunc(m, "CertOpenStore", i8p,
		ir.NewParam("provider", i8p), ir.NewParam("enc", irtypes.I32),
		ir.NewParam("prov", i8p), ir.NewParam("flags", irtypes.I32),
		ir.NewParam("para", i8p))
	e.certCloseStore = getOrDeclareFunc(m, "CertCloseStore", irtypes.I32,
		ir.NewParam("store", i8p), ir.NewParam("flags", irtypes.I32))
	e.certAddEnc = getOrDeclareFunc(m, "CertAddEncodedCertificateToStore", irtypes.I32,
		ir.NewParam("store", i8p), ir.NewParam("enc", irtypes.I32),
		ir.NewParam("der", i8p), ir.NewParam("len", irtypes.I32),
		ir.NewParam("disposition", irtypes.I32), ir.NewParam("out", i8p))
	e.certFind = getOrDeclareFunc(m, "CertFindCertificateInStore", i8p,
		ir.NewParam("store", i8p), ir.NewParam("enc", irtypes.I32),
		ir.NewParam("findFlags", irtypes.I32), ir.NewParam("findType", irtypes.I32),
		ir.NewParam("findPara", i8p), ir.NewParam("prev", i8p))
	e.certGetChain = getOrDeclareFunc(m, "CertGetCertificateChain", irtypes.I32,
		ir.NewParam("engine", i8p), ir.NewParam("cert", i8p),
		ir.NewParam("time", i8p), ir.NewParam("addStore", i8p),
		ir.NewParam("para", i8p), ir.NewParam("flags", irtypes.I32),
		ir.NewParam("reserved", i8p), ir.NewParam("out", i8p))
	e.certFreeChain = getOrDeclareFunc(m, "CertFreeCertificateChain", irtypes.Void,
		ir.NewParam("chain", i8p))
	e.certVerifyPol = getOrDeclareFunc(m, "CertVerifyCertificateChainPolicy", irtypes.I32,
		ir.NewParam("oid", i8p), ir.NewParam("chain", i8p),
		ir.NewParam("para", i8p), ir.NewParam("status", i8p))
	e.certSetProp = getOrDeclareFunc(m, "CertSetCertificateContextProperty", irtypes.I32,
		ir.NewParam("cert", i8p), ir.NewParam("propId", irtypes.I32),
		ir.NewParam("flags", irtypes.I32), ir.NewParam("data", i8p))

	e.ncryptOpenProv = getOrDeclareFunc(m, "NCryptOpenStorageProvider", irtypes.I32,
		ir.NewParam("prov", i64p), ir.NewParam("name", i8p),
		ir.NewParam("flags", irtypes.I32))
	e.ncryptImport = getOrDeclareFunc(m, "NCryptImportKey", irtypes.I32,
		ir.NewParam("prov", irtypes.I64), ir.NewParam("importKey", irtypes.I64),
		ir.NewParam("blobType", i8p), ir.NewParam("paramList", i8p),
		ir.NewParam("key", i64p), ir.NewParam("data", i8p),
		ir.NewParam("cbData", irtypes.I32), ir.NewParam("flags", irtypes.I32))
	e.ncryptFree = getOrDeclareFunc(m, "NCryptFreeObject", irtypes.I32,
		ir.NewParam("h", irtypes.I64))
	e.ncryptDelete = getOrDeclareFunc(m, "NCryptDeleteKey", irtypes.I32,
		ir.NewParam("key", irtypes.I64), ir.NewParam("flags", irtypes.I32))
	e.getPid = getOrDeclareFunc(m, "GetCurrentProcessId", irtypes.I32)

	e.pkgName = tlsCStr(m, "__promise_tls_win_pkg", "Microsoft Unified Security Protocol Provider")
	e.provName = tlsWinWStr(m, "__promise_tls_win_ksp", "Microsoft Software Key Storage Provider")
	e.blobName = tlsWinWStr(m, "__promise_tls_win_blob", "PKCS8_PRIVATEKEY")
	e.keyPrefix = tlsWinWStr(m, "__promise_tls_win_keyprefix", "promise-tls-")
	e.verEmpty = tlsCStr(m, "__promise_tls_win_ver_none", "")
	e.ver12 = tlsCStr(m, "__promise_tls_win_ver_12", "TLSv1.2")
	e.ver13 = tlsCStr(m, "__promise_tls_win_ver_13", "TLSv1.3")
}
