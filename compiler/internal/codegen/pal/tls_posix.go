package pal

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
)

// tls_posix.go — OpenSSL (memory-BIO) TLS backend for POSIX/Linux (T0077).
//
// These wrappers declare the real exported OpenSSL 3.x symbols (SSL_*, BIO_*,
// PEM_*, X509_*) and expose a small, fd-free, reactor-agnostic surface named
// pal_tls_*. All socket I/O and reactor parking stay in Promise (modules/tls/
// drives the byte pump over net.TcpStream), so this backend never sees a socket,
// an fd, or the scheduler — which is exactly what lets a future macOS/Windows
// backend slot in behind the same surface (T0077 §7).
//
// Handles (SSL_CTX*, SSL*) cross the boundary as i64 (ptrtoint) so they map
// directly onto Promise `int`. A 0 handle means null/failure.
//
// The status enum returned by handshake/read/write is the backend-neutral
// contract:
//   handshake: 0 ok, 1 want_read, 2 want_write, -1 fatal
//   read:      >0 bytes, 0 EOF (clean close_notify), -1 want_read, -2 want_write, -3 fatal
//   write:     >0 bytes,        -1 want_read, -2 want_write, -3 fatal
//   shutdown:  0 done, 1 want_read, 2 want_write, 3 call-again, -1 fatal

// OpenSSL 3.x pinned ABI constants (verified against the vendored 3.5.7 headers).
const (
	sslErrorNone       = 0
	sslErrorWantRead   = 2
	sslErrorWantWrite  = 3
	sslErrorZeroReturn = 6

	sslVerifyNone = 0 // SSL_VERIFY_NONE
	sslVerifyPeer = 1 // SSL_VERIFY_PEER

	sslCtrlSetMinProtoVersion = 123 // SSL_CTRL_SET_MIN_PROTO_VERSION
	sslCtrlSetTlsextHostname  = 55  // SSL_CTRL_SET_TLSEXT_HOSTNAME
	tlsextNametypeHostName    = 0   // TLSEXT_NAMETYPE_host_name
	bioCtrlPending            = 10  // BIO_CTRL_PENDING
)

// tlsBackendPaths are the well-known CA bundle files/dir probed by
// pal_tls_ctx_load_default_trust when SSL_CERT_FILE/SSL_CERT_DIR are unset.
// A fully-static musl binary carries no compiled-in OpenSSL config, so the
// trust store must be discovered explicitly (T0077 §4).
var tlsBackendCAFiles = []string{
	"/etc/ssl/certs/ca-certificates.crt", // Debian/Ubuntu/Alpine
	"/etc/pki/tls/certs/ca-bundle.crt",   // Fedora/RHEL/CentOS
	"/etc/ssl/cert.pem",                  // Alpine/OpenBSD/macOS
}

const tlsBackendCADir = "/etc/ssl/certs"

// tlsCStr defines an immutable, NUL-terminated C string global and returns an
// i8* constant to its first byte.
func tlsCStr(module *ir.Module, name, s string) *constant.ExprGetElementPtr {
	arr := constant.NewCharArrayFromString(s + "\x00")
	g := module.NewGlobal(name, arr.Typ)
	g.Init = arr
	g.Immutable = true
	zero := constant.NewInt(irtypes.I64, 0)
	return constant.NewGetElementPtr(arr.Typ, g, zero, zero)
}

// EmitTLS emits every pal_tls_* wrapper and returns them keyed by name. The
// underlying OpenSSL symbols are declared as bodyless externs, resolved at link
// against the vendored static libssl.a/libcrypto.a (T1596).
func (p *PosixPAL) EmitTLS(module *ir.Module) map[string]*ir.Func {
	fns := make(map[string]*ir.Func)
	emit := func(f *ir.Func) *ir.Func {
		fns[f.Name()] = f
		return f
	}

	// --- OpenSSL externs -------------------------------------------------
	tlsClientMethod := getOrDeclareFunc(module, "TLS_client_method", irtypes.I8Ptr)
	tlsServerMethod := getOrDeclareFunc(module, "TLS_server_method", irtypes.I8Ptr)
	sslCtxNew := getOrDeclareFunc(module, "SSL_CTX_new", irtypes.I8Ptr,
		ir.NewParam("method", irtypes.I8Ptr))
	sslCtxFree := getOrDeclareFunc(module, "SSL_CTX_free", irtypes.Void,
		ir.NewParam("ctx", irtypes.I8Ptr))
	sslCtxSetVerify := getOrDeclareFunc(module, "SSL_CTX_set_verify", irtypes.Void,
		ir.NewParam("ctx", irtypes.I8Ptr), ir.NewParam("mode", irtypes.I32),
		ir.NewParam("cb", irtypes.I8Ptr))
	sslCtxCtrl := getOrDeclareFunc(module, "SSL_CTX_ctrl", irtypes.I64,
		ir.NewParam("ctx", irtypes.I8Ptr), ir.NewParam("cmd", irtypes.I32),
		ir.NewParam("larg", irtypes.I64), ir.NewParam("parg", irtypes.I8Ptr))
	sslCtxGetCertStore := getOrDeclareFunc(module, "SSL_CTX_get_cert_store", irtypes.I8Ptr,
		ir.NewParam("ctx", irtypes.I8Ptr))
	sslCtxUseCert := getOrDeclareFunc(module, "SSL_CTX_use_certificate", irtypes.I32,
		ir.NewParam("ctx", irtypes.I8Ptr), ir.NewParam("x", irtypes.I8Ptr))
	sslCtxUseKey := getOrDeclareFunc(module, "SSL_CTX_use_PrivateKey", irtypes.I32,
		ir.NewParam("ctx", irtypes.I8Ptr), ir.NewParam("pkey", irtypes.I8Ptr))
	sslCtxLoadVerifyFile := getOrDeclareFunc(module, "SSL_CTX_load_verify_file", irtypes.I32,
		ir.NewParam("ctx", irtypes.I8Ptr), ir.NewParam("file", irtypes.I8Ptr))
	sslCtxLoadVerifyDir := getOrDeclareFunc(module, "SSL_CTX_load_verify_dir", irtypes.I32,
		ir.NewParam("ctx", irtypes.I8Ptr), ir.NewParam("dir", irtypes.I8Ptr))

	sslNew := getOrDeclareFunc(module, "SSL_new", irtypes.I8Ptr,
		ir.NewParam("ctx", irtypes.I8Ptr))
	sslFree := getOrDeclareFunc(module, "SSL_free", irtypes.Void,
		ir.NewParam("ssl", irtypes.I8Ptr))
	sslSetBio := getOrDeclareFunc(module, "SSL_set_bio", irtypes.Void,
		ir.NewParam("ssl", irtypes.I8Ptr), ir.NewParam("rbio", irtypes.I8Ptr),
		ir.NewParam("wbio", irtypes.I8Ptr))
	sslSetConnectState := getOrDeclareFunc(module, "SSL_set_connect_state", irtypes.Void,
		ir.NewParam("ssl", irtypes.I8Ptr))
	sslSetAcceptState := getOrDeclareFunc(module, "SSL_set_accept_state", irtypes.Void,
		ir.NewParam("ssl", irtypes.I8Ptr))
	sslCtrl := getOrDeclareFunc(module, "SSL_ctrl", irtypes.I64,
		ir.NewParam("ssl", irtypes.I8Ptr), ir.NewParam("cmd", irtypes.I32),
		ir.NewParam("larg", irtypes.I64), ir.NewParam("parg", irtypes.I8Ptr))
	sslSet1Host := getOrDeclareFunc(module, "SSL_set1_host", irtypes.I32,
		ir.NewParam("ssl", irtypes.I8Ptr), ir.NewParam("host", irtypes.I8Ptr))
	sslDoHandshake := getOrDeclareFunc(module, "SSL_do_handshake", irtypes.I32,
		ir.NewParam("ssl", irtypes.I8Ptr))
	sslRead := getOrDeclareFunc(module, "SSL_read", irtypes.I32,
		ir.NewParam("ssl", irtypes.I8Ptr), ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("num", irtypes.I32))
	sslWrite := getOrDeclareFunc(module, "SSL_write", irtypes.I32,
		ir.NewParam("ssl", irtypes.I8Ptr), ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("num", irtypes.I32))
	sslShutdown := getOrDeclareFunc(module, "SSL_shutdown", irtypes.I32,
		ir.NewParam("ssl", irtypes.I8Ptr))
	sslGetError := getOrDeclareFunc(module, "SSL_get_error", irtypes.I32,
		ir.NewParam("ssl", irtypes.I8Ptr), ir.NewParam("ret", irtypes.I32))
	sslGetRbio := getOrDeclareFunc(module, "SSL_get_rbio", irtypes.I8Ptr,
		ir.NewParam("ssl", irtypes.I8Ptr))
	sslGetWbio := getOrDeclareFunc(module, "SSL_get_wbio", irtypes.I8Ptr,
		ir.NewParam("ssl", irtypes.I8Ptr))
	sslGetVersion := getOrDeclareFunc(module, "SSL_get_version", irtypes.I8Ptr,
		ir.NewParam("ssl", irtypes.I8Ptr))
	sslGetCurrentCipher := getOrDeclareFunc(module, "SSL_get_current_cipher", irtypes.I8Ptr,
		ir.NewParam("ssl", irtypes.I8Ptr))
	sslCipherGetName := getOrDeclareFunc(module, "SSL_CIPHER_get_name", irtypes.I8Ptr,
		ir.NewParam("cipher", irtypes.I8Ptr))
	sslGetVerifyResult := getOrDeclareFunc(module, "SSL_get_verify_result", irtypes.I64,
		ir.NewParam("ssl", irtypes.I8Ptr))

	bioNew := getOrDeclareFunc(module, "BIO_new", irtypes.I8Ptr,
		ir.NewParam("method", irtypes.I8Ptr))
	bioSMem := getOrDeclareFunc(module, "BIO_s_mem", irtypes.I8Ptr)
	bioNewMemBuf := getOrDeclareFunc(module, "BIO_new_mem_buf", irtypes.I8Ptr,
		ir.NewParam("buf", irtypes.I8Ptr), ir.NewParam("len", irtypes.I32))
	bioRead := getOrDeclareFunc(module, "BIO_read", irtypes.I32,
		ir.NewParam("bio", irtypes.I8Ptr), ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I32))
	bioWrite := getOrDeclareFunc(module, "BIO_write", irtypes.I32,
		ir.NewParam("bio", irtypes.I8Ptr), ir.NewParam("buf", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I32))
	bioCtrl := getOrDeclareFunc(module, "BIO_ctrl", irtypes.I64,
		ir.NewParam("bio", irtypes.I8Ptr), ir.NewParam("cmd", irtypes.I32),
		ir.NewParam("larg", irtypes.I64), ir.NewParam("parg", irtypes.I8Ptr))
	bioFree := getOrDeclareFunc(module, "BIO_free", irtypes.I32,
		ir.NewParam("bio", irtypes.I8Ptr))

	pemReadBioX509 := getOrDeclareFunc(module, "PEM_read_bio_X509", irtypes.I8Ptr,
		ir.NewParam("bio", irtypes.I8Ptr), ir.NewParam("x", irtypes.I8Ptr),
		ir.NewParam("cb", irtypes.I8Ptr), ir.NewParam("u", irtypes.I8Ptr))
	pemReadBioKey := getOrDeclareFunc(module, "PEM_read_bio_PrivateKey", irtypes.I8Ptr,
		ir.NewParam("bio", irtypes.I8Ptr), ir.NewParam("x", irtypes.I8Ptr),
		ir.NewParam("cb", irtypes.I8Ptr), ir.NewParam("u", irtypes.I8Ptr))
	x509StoreAddCert := getOrDeclareFunc(module, "X509_STORE_add_cert", irtypes.I32,
		ir.NewParam("store", irtypes.I8Ptr), ir.NewParam("x", irtypes.I8Ptr))
	x509Free := getOrDeclareFunc(module, "X509_free", irtypes.Void,
		ir.NewParam("x", irtypes.I8Ptr))
	evpPkeyFree := getOrDeclareFunc(module, "EVP_PKEY_free", irtypes.Void,
		ir.NewParam("pkey", irtypes.I8Ptr))

	getenvFn := getOrDeclareFunc(module, "getenv", irtypes.I8Ptr,
		ir.NewParam("name", irtypes.I8Ptr))

	null := constant.NewNull(irtypes.I8Ptr)
	i32 := func(v int64) *constant.Int { return constant.NewInt(irtypes.I32, v) }
	i64 := func(v int64) *constant.Int { return constant.NewInt(irtypes.I64, v) }

	// --- pal_tls_ctx_new_client / _server -------------------------------
	emitCtxNew := func(name string, method *ir.Func) *ir.Func {
		fn := module.NewFunc(name, irtypes.I64)
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		m := b.NewCall(method)
		ctx := b.NewCall(sslCtxNew, m)
		b.NewRet(b.NewPtrToInt(ctx, irtypes.I64))
		return emit(fn)
	}
	emitCtxNew("pal_tls_ctx_new_client", tlsClientMethod)
	emitCtxNew("pal_tls_ctx_new_server", tlsServerMethod)

	// --- pal_tls_ctx_free(i64 ctx) --------------------------------------
	{
		fn := module.NewFunc("pal_tls_ctx_free", irtypes.Void,
			ir.NewParam("ctx", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ctx := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		b.NewCall(sslCtxFree, ctx)
		b.NewRet(nil)
		emit(fn)
	}

	// --- pal_tls_ctx_set_verify(i64 ctx, i32 peer) ----------------------
	// peer != 0 → SSL_VERIFY_PEER, else SSL_VERIFY_NONE.
	{
		fn := module.NewFunc("pal_tls_ctx_set_verify", irtypes.Void,
			ir.NewParam("ctx", irtypes.I64), ir.NewParam("peer", irtypes.I32))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ctx := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		isPeer := b.NewICmp(enum.IPredNE, fn.Params[1], i32(0))
		mode := b.NewSelect(isPeer, i32(sslVerifyPeer), i32(sslVerifyNone))
		b.NewCall(sslCtxSetVerify, ctx, mode, null)
		b.NewRet(nil)
		emit(fn)
	}

	// --- pal_tls_ctx_set_min_version(i64 ctx, i32 ver) → i32 ------------
	{
		fn := module.NewFunc("pal_tls_ctx_set_min_version", irtypes.I32,
			ir.NewParam("ctx", irtypes.I64), ir.NewParam("ver", irtypes.I32))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ctx := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		verL := b.NewSExt(fn.Params[1], irtypes.I64)
		rc := b.NewCall(sslCtxCtrl, ctx, i32(sslCtrlSetMinProtoVersion), verL, null)
		b.NewRet(b.NewTrunc(rc, irtypes.I32))
		emit(fn)
	}

	// --- pal_tls_ctx_add_ca(i64 ctx, i8* pem, i64 len) → i32 ------------
	// Parse a PEM certificate and add it to the ctx's trust store.
	// Returns 1 on success, 0 on parse/add failure.
	{
		fn := module.NewFunc("pal_tls_ctx_add_ca", irtypes.I32,
			ir.NewParam("ctx", irtypes.I64), ir.NewParam("pem", irtypes.I8Ptr),
			ir.NewParam("len", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ctx := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		lenI32 := b.NewTrunc(fn.Params[2], irtypes.I32)
		bio := b.NewCall(bioNewMemBuf, fn.Params[1], lenI32)
		x := b.NewCall(pemReadBioX509, bio, null, null, null)
		xNull := b.NewICmp(enum.IPredEQ, x, null)
		failBlk := fn.NewBlock(".fail")
		okBlk := fn.NewBlock(".have_x")
		b.NewCondBr(xNull, failBlk, okBlk)

		failBlk.NewCall(bioFree, bio)
		failBlk.NewRet(i32(0))

		store := okBlk.NewCall(sslCtxGetCertStore, ctx)
		rc := okBlk.NewCall(x509StoreAddCert, store, x)
		okBlk.NewCall(x509Free, x)
		okBlk.NewCall(bioFree, bio)
		okBlk.NewRet(rc)
		emit(fn)
	}

	// --- pal_tls_ctx_use_cert(i64 ctx, i8* pem, i64 len) → i32 ----------
	{
		fn := module.NewFunc("pal_tls_ctx_use_cert", irtypes.I32,
			ir.NewParam("ctx", irtypes.I64), ir.NewParam("pem", irtypes.I8Ptr),
			ir.NewParam("len", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ctx := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		lenI32 := b.NewTrunc(fn.Params[2], irtypes.I32)
		bio := b.NewCall(bioNewMemBuf, fn.Params[1], lenI32)
		x := b.NewCall(pemReadBioX509, bio, null, null, null)
		xNull := b.NewICmp(enum.IPredEQ, x, null)
		failBlk := fn.NewBlock(".fail")
		okBlk := fn.NewBlock(".have_x")
		b.NewCondBr(xNull, failBlk, okBlk)

		failBlk.NewCall(bioFree, bio)
		failBlk.NewRet(i32(0))

		rc := okBlk.NewCall(sslCtxUseCert, ctx, x)
		okBlk.NewCall(x509Free, x)
		okBlk.NewCall(bioFree, bio)
		okBlk.NewRet(rc)
		emit(fn)
	}

	// --- pal_tls_ctx_use_key(i64 ctx, i8* pem, i64 len) → i32 ----------
	{
		fn := module.NewFunc("pal_tls_ctx_use_key", irtypes.I32,
			ir.NewParam("ctx", irtypes.I64), ir.NewParam("pem", irtypes.I8Ptr),
			ir.NewParam("len", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ctx := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		lenI32 := b.NewTrunc(fn.Params[2], irtypes.I32)
		bio := b.NewCall(bioNewMemBuf, fn.Params[1], lenI32)
		pkey := b.NewCall(pemReadBioKey, bio, null, null, null)
		pNull := b.NewICmp(enum.IPredEQ, pkey, null)
		failBlk := fn.NewBlock(".fail")
		okBlk := fn.NewBlock(".have_key")
		b.NewCondBr(pNull, failBlk, okBlk)

		failBlk.NewCall(bioFree, bio)
		failBlk.NewRet(i32(0))

		rc := okBlk.NewCall(sslCtxUseKey, ctx, pkey)
		okBlk.NewCall(evpPkeyFree, pkey)
		okBlk.NewCall(bioFree, bio)
		okBlk.NewRet(rc)
		emit(fn)
	}

	// --- pal_tls_ctx_load_default_trust(i64 ctx) → i32 -----------------
	// Establish a system trust store for a static binary that carries no
	// compiled-in OpenSSL config. Honours SSL_CERT_FILE / SSL_CERT_DIR, then
	// probes well-known bundle paths. Returns 1 if a store loaded, else 0.
	{
		fn := module.NewFunc("pal_tls_ctx_load_default_trust", irtypes.I32,
			ir.NewParam("ctx", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		ctxParam := fn.Params[0]

		certFileName := tlsCStr(module, "__promise_tls_env_cert_file", "SSL_CERT_FILE")
		certDirName := tlsCStr(module, "__promise_tls_env_cert_dir", "SSL_CERT_DIR")

		entry := fn.NewBlock(".entry")
		ctx := entry.NewIntToPtr(ctxParam, irtypes.I8Ptr)

		// SSL_CERT_FILE
		envFile := entry.NewCall(getenvFn, certFileName)
		envFileSet := entry.NewICmp(enum.IPredNE, envFile, null)
		tryEnvFile := fn.NewBlock(".try_env_file")
		afterEnvFile := fn.NewBlock(".after_env_file")
		entry.NewCondBr(envFileSet, tryEnvFile, afterEnvFile)
		rcEF := tryEnvFile.NewCall(sslCtxLoadVerifyFile, ctx, envFile)
		okEF := tryEnvFile.NewICmp(enum.IPredEQ, rcEF, i32(1))
		retOne := fn.NewBlock(".ret_one")
		tryEnvFile.NewCondBr(okEF, retOne, afterEnvFile)

		// SSL_CERT_DIR
		envDir := afterEnvFile.NewCall(getenvFn, certDirName)
		envDirSet := afterEnvFile.NewICmp(enum.IPredNE, envDir, null)
		tryEnvDir := fn.NewBlock(".try_env_dir")
		afterEnvDir := fn.NewBlock(".after_env_dir")
		afterEnvFile.NewCondBr(envDirSet, tryEnvDir, afterEnvDir)
		rcED := tryEnvDir.NewCall(sslCtxLoadVerifyDir, ctx, envDir)
		okED := tryEnvDir.NewICmp(enum.IPredEQ, rcED, i32(1))
		tryEnvDir.NewCondBr(okED, retOne, afterEnvDir)

		// Probe well-known bundle files.
		cur := afterEnvDir
		for idx, path := range tlsBackendCAFiles {
			pc := tlsCStr(module, "__promise_tls_ca_file_"+itoa(idx), path)
			rc := cur.NewCall(sslCtxLoadVerifyFile, ctx, pc)
			ok := cur.NewICmp(enum.IPredEQ, rc, i32(1))
			next := fn.NewBlock(".probe_" + itoa(idx+1))
			cur.NewCondBr(ok, retOne, next)
			cur = next
		}
		// Probe the well-known bundle directory as a last resort.
		dirC := tlsCStr(module, "__promise_tls_ca_dir", tlsBackendCADir)
		rcDir := cur.NewCall(sslCtxLoadVerifyDir, ctx, dirC)
		okDir := cur.NewICmp(enum.IPredEQ, rcDir, i32(1))
		retZero := fn.NewBlock(".ret_zero")
		cur.NewCondBr(okDir, retOne, retZero)

		retOne.NewRet(i32(1))
		retZero.NewRet(i32(0))
		emit(fn)
	}

	// --- pal_tls_new(i64 ctx) → i64 ------------------------------------
	// SSL_new + two memory BIOs + SSL_set_bio. Returns SSL* as i64 (0 on fail).
	{
		fn := module.NewFunc("pal_tls_new", irtypes.I64,
			ir.NewParam("ctx", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ctx := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		ssl := b.NewCall(sslNew, ctx)
		sslNull := b.NewICmp(enum.IPredEQ, ssl, null)
		failBlk := fn.NewBlock(".fail")
		okBlk := fn.NewBlock(".have_ssl")
		b.NewCondBr(sslNull, failBlk, okBlk)

		failBlk.NewRet(i64(0))

		mem := okBlk.NewCall(bioSMem)
		rbio := okBlk.NewCall(bioNew, mem)
		wbio := okBlk.NewCall(bioNew, mem)
		okBlk.NewCall(sslSetBio, ssl, rbio, wbio)
		okBlk.NewRet(okBlk.NewPtrToInt(ssl, irtypes.I64))
		emit(fn)
	}

	// --- pal_tls_set_connect_state / _set_accept_state (i64 ssl) -------
	emitSetState := func(name string, target *ir.Func) *ir.Func {
		fn := module.NewFunc(name, irtypes.Void, ir.NewParam("ssl", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ssl := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		b.NewCall(target, ssl)
		b.NewRet(nil)
		return emit(fn)
	}
	emitSetState("pal_tls_set_connect_state", sslSetConnectState)
	emitSetState("pal_tls_set_accept_state", sslSetAcceptState)

	// --- pal_tls_set_sni(i64 ssl, i8* host) → i32 ----------------------
	{
		fn := module.NewFunc("pal_tls_set_sni", irtypes.I32,
			ir.NewParam("ssl", irtypes.I64), ir.NewParam("host", irtypes.I8Ptr))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ssl := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		rc := b.NewCall(sslCtrl, ssl, i32(sslCtrlSetTlsextHostname),
			i64(tlsextNametypeHostName), fn.Params[1])
		b.NewRet(b.NewTrunc(rc, irtypes.I32))
		emit(fn)
	}

	// --- pal_tls_set_verify_host(i64 ssl, i8* host) → i32 --------------
	{
		fn := module.NewFunc("pal_tls_set_verify_host", irtypes.I32,
			ir.NewParam("ssl", irtypes.I64), ir.NewParam("host", irtypes.I8Ptr))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ssl := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		rc := b.NewCall(sslSet1Host, ssl, fn.Params[1])
		b.NewRet(rc)
		emit(fn)
	}

	// mapErr emits the SSL_get_error → status classification and returns a
	// value of type `retType` selected among the provided sentinels.
	// Produces: err = SSL_get_error(ssl, rc); switch → want_read/want_write/other.
	// Callers wire the three result blocks.

	// --- pal_tls_do_handshake(i64 ssl) → i32 ---------------------------
	// 0 ok, 1 want_read, 2 want_write, -1 fatal.
	{
		fn := module.NewFunc("pal_tls_do_handshake", irtypes.I32,
			ir.NewParam("ssl", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ssl := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		rc := b.NewCall(sslDoHandshake, ssl)
		done := b.NewICmp(enum.IPredEQ, rc, i32(1))
		okBlk := fn.NewBlock(".ok")
		errBlk := fn.NewBlock(".classify")
		b.NewCondBr(done, okBlk, errBlk)
		okBlk.NewRet(i32(0))

		err := errBlk.NewCall(sslGetError, ssl, rc)
		wantRead := errBlk.NewICmp(enum.IPredEQ, err, i32(sslErrorWantRead))
		wrBlk := fn.NewBlock(".want_read")
		notWR := fn.NewBlock(".not_want_read")
		errBlk.NewCondBr(wantRead, wrBlk, notWR)
		wrBlk.NewRet(i32(1))
		wantWrite := notWR.NewICmp(enum.IPredEQ, err, i32(sslErrorWantWrite))
		wwBlk := fn.NewBlock(".want_write")
		fatalBlk := fn.NewBlock(".fatal")
		notWR.NewCondBr(wantWrite, wwBlk, fatalBlk)
		wwBlk.NewRet(i32(2))
		fatalBlk.NewRet(i32(-1))
		emit(fn)
	}

	// --- pal_tls_read(i64 ssl, i8* buf, i64 len) → i64 -----------------
	// >0 bytes, 0 EOF (clean), -1 want_read, -2 want_write, -3 fatal.
	{
		fn := module.NewFunc("pal_tls_read", irtypes.I64,
			ir.NewParam("ssl", irtypes.I64), ir.NewParam("buf", irtypes.I8Ptr),
			ir.NewParam("len", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ssl := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		lenI32 := b.NewTrunc(fn.Params[2], irtypes.I32)
		rc := b.NewCall(sslRead, ssl, fn.Params[1], lenI32)
		gotData := b.NewICmp(enum.IPredSGT, rc, i32(0))
		dataBlk := fn.NewBlock(".data")
		errBlk := fn.NewBlock(".classify")
		b.NewCondBr(gotData, dataBlk, errBlk)
		dataBlk.NewRet(dataBlk.NewSExt(rc, irtypes.I64))

		err := errBlk.NewCall(sslGetError, ssl, rc)
		isRead := errBlk.NewICmp(enum.IPredEQ, err, i32(sslErrorWantRead))
		wrBlk := fn.NewBlock(".want_read")
		notRead := fn.NewBlock(".not_read")
		errBlk.NewCondBr(isRead, wrBlk, notRead)
		wrBlk.NewRet(i64(-1))
		isWrite := notRead.NewICmp(enum.IPredEQ, err, i32(sslErrorWantWrite))
		wwBlk := fn.NewBlock(".want_write")
		notWrite := fn.NewBlock(".not_write")
		notRead.NewCondBr(isWrite, wwBlk, notWrite)
		wwBlk.NewRet(i64(-2))
		isZero := notWrite.NewICmp(enum.IPredEQ, err, i32(sslErrorZeroReturn))
		eofBlk := fn.NewBlock(".eof")
		fatalBlk := fn.NewBlock(".fatal")
		notWrite.NewCondBr(isZero, eofBlk, fatalBlk)
		eofBlk.NewRet(i64(0))
		fatalBlk.NewRet(i64(-3))
		emit(fn)
	}

	// --- pal_tls_write(i64 ssl, i8* buf, i64 len) → i64 ---------------
	// >0 bytes, -1 want_read, -2 want_write, -3 fatal.
	{
		fn := module.NewFunc("pal_tls_write", irtypes.I64,
			ir.NewParam("ssl", irtypes.I64), ir.NewParam("buf", irtypes.I8Ptr),
			ir.NewParam("len", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ssl := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		lenI32 := b.NewTrunc(fn.Params[2], irtypes.I32)
		rc := b.NewCall(sslWrite, ssl, fn.Params[1], lenI32)
		gotData := b.NewICmp(enum.IPredSGT, rc, i32(0))
		dataBlk := fn.NewBlock(".data")
		errBlk := fn.NewBlock(".classify")
		b.NewCondBr(gotData, dataBlk, errBlk)
		dataBlk.NewRet(dataBlk.NewSExt(rc, irtypes.I64))

		err := errBlk.NewCall(sslGetError, ssl, rc)
		isRead := errBlk.NewICmp(enum.IPredEQ, err, i32(sslErrorWantRead))
		wrBlk := fn.NewBlock(".want_read")
		notRead := fn.NewBlock(".not_read")
		errBlk.NewCondBr(isRead, wrBlk, notRead)
		wrBlk.NewRet(i64(-1))
		isWrite := notRead.NewICmp(enum.IPredEQ, err, i32(sslErrorWantWrite))
		wwBlk := fn.NewBlock(".want_write")
		fatalBlk := fn.NewBlock(".fatal")
		notRead.NewCondBr(isWrite, wwBlk, fatalBlk)
		wwBlk.NewRet(i64(-2))
		fatalBlk.NewRet(i64(-3))
		emit(fn)
	}

	// --- pal_tls_shutdown(i64 ssl) → i32 -------------------------------
	// 0 done, 1 want_read, 2 want_write, 3 call-again, -1 fatal.
	{
		fn := module.NewFunc("pal_tls_shutdown", irtypes.I32,
			ir.NewParam("ssl", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ssl := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		rc := b.NewCall(sslShutdown, ssl)
		done := b.NewICmp(enum.IPredEQ, rc, i32(1))
		okBlk := fn.NewBlock(".done")
		notDone := fn.NewBlock(".not_done")
		b.NewCondBr(done, okBlk, notDone)
		okBlk.NewRet(i32(0))

		again := notDone.NewICmp(enum.IPredEQ, rc, i32(0))
		againBlk := fn.NewBlock(".again")
		wantBlk := fn.NewBlock(".want")
		notDone.NewCondBr(again, againBlk, wantBlk)
		againBlk.NewRet(i32(3))

		err := wantBlk.NewCall(sslGetError, ssl, rc)
		isRead := wantBlk.NewICmp(enum.IPredEQ, err, i32(sslErrorWantRead))
		wrBlk := fn.NewBlock(".want_read")
		notRead := fn.NewBlock(".not_read")
		wantBlk.NewCondBr(isRead, wrBlk, notRead)
		wrBlk.NewRet(i32(1))
		isWrite := notRead.NewICmp(enum.IPredEQ, err, i32(sslErrorWantWrite))
		wwBlk := fn.NewBlock(".want_write")
		fatalBlk := fn.NewBlock(".fatal")
		notRead.NewCondBr(isWrite, wwBlk, fatalBlk)
		wwBlk.NewRet(i32(2))
		fatalBlk.NewRet(i32(-1))
		emit(fn)
	}

	// --- pal_tls_bio_read_out(i64 ssl, i8* buf, i64 len) → i64 --------
	// Drain outgoing ciphertext from the write BIO. Returns bytes (<=0 none).
	{
		fn := module.NewFunc("pal_tls_bio_read_out", irtypes.I64,
			ir.NewParam("ssl", irtypes.I64), ir.NewParam("buf", irtypes.I8Ptr),
			ir.NewParam("len", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ssl := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		wbio := b.NewCall(sslGetWbio, ssl)
		lenI32 := b.NewTrunc(fn.Params[2], irtypes.I32)
		n := b.NewCall(bioRead, wbio, fn.Params[1], lenI32)
		b.NewRet(b.NewSExt(n, irtypes.I64))
		emit(fn)
	}

	// --- pal_tls_bio_write_in(i64 ssl, i8* buf, i64 len) → i64 -------
	// Feed incoming ciphertext into the read BIO. Returns bytes written.
	{
		fn := module.NewFunc("pal_tls_bio_write_in", irtypes.I64,
			ir.NewParam("ssl", irtypes.I64), ir.NewParam("buf", irtypes.I8Ptr),
			ir.NewParam("len", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ssl := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		rbio := b.NewCall(sslGetRbio, ssl)
		lenI32 := b.NewTrunc(fn.Params[2], irtypes.I32)
		n := b.NewCall(bioWrite, rbio, fn.Params[1], lenI32)
		b.NewRet(b.NewSExt(n, irtypes.I64))
		emit(fn)
	}

	// --- pal_tls_bio_pending_out(i64 ssl) → i64 ----------------------
	{
		fn := module.NewFunc("pal_tls_bio_pending_out", irtypes.I64,
			ir.NewParam("ssl", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ssl := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		wbio := b.NewCall(sslGetWbio, ssl)
		n := b.NewCall(bioCtrl, wbio, i32(bioCtrlPending), i64(0), null)
		b.NewRet(n)
		emit(fn)
	}

	// --- pal_tls_get_version(i64 ssl) → i8* --------------------------
	// Returns a static const char* (no free).
	{
		fn := module.NewFunc("pal_tls_get_version", irtypes.I8Ptr,
			ir.NewParam("ssl", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ssl := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		b.NewRet(b.NewCall(sslGetVersion, ssl))
		emit(fn)
	}

	// --- pal_tls_get_cipher(i64 ssl) → i8* ---------------------------
	// Returns SSL_CIPHER_get_name(SSL_get_current_cipher(ssl)), or "" if none.
	{
		fn := module.NewFunc("pal_tls_get_cipher", irtypes.I8Ptr,
			ir.NewParam("ssl", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		emptyStr := tlsCStr(module, "__promise_tls_empty_cipher", "")
		b := fn.NewBlock(".entry")
		ssl := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		cipher := b.NewCall(sslGetCurrentCipher, ssl)
		cipherNull := b.NewICmp(enum.IPredEQ, cipher, null)
		noneBlk := fn.NewBlock(".none")
		haveBlk := fn.NewBlock(".have")
		b.NewCondBr(cipherNull, noneBlk, haveBlk)
		noneBlk.NewRet(emptyStr)
		haveBlk.NewRet(haveBlk.NewCall(sslCipherGetName, cipher))
		emit(fn)
	}

	// --- pal_tls_get_verify_result(i64 ssl) → i32 -------------------
	// 0 == X509_V_OK.
	{
		fn := module.NewFunc("pal_tls_get_verify_result", irtypes.I32,
			ir.NewParam("ssl", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ssl := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		rc := b.NewCall(sslGetVerifyResult, ssl)
		b.NewRet(b.NewTrunc(rc, irtypes.I32))
		emit(fn)
	}

	// --- pal_tls_free(i64 ssl) --------------------------------------
	// SSL_free also frees the two attached memory BIOs.
	{
		fn := module.NewFunc("pal_tls_free", irtypes.Void,
			ir.NewParam("ssl", irtypes.I64))
		fn.FuncAttrs = append(fn.FuncAttrs, enum.FuncAttrNoUnwind)
		b := fn.NewBlock(".entry")
		ssl := b.NewIntToPtr(fn.Params[0], irtypes.I8Ptr)
		b.NewCall(sslFree, ssl)
		b.NewRet(nil)
		emit(fn)
	}

	return fns
}

// itoa is a tiny non-negative int → string helper (avoids importing strconv
// into the pal package for two call sites).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
