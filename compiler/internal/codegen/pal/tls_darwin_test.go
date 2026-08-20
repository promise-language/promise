package pal

import (
	"strings"
	"testing"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/enum"
)

// tlsPALWrappers is the pal_tls_* surface every TLS backend must provide. The
// codegen bridge (codegen/tls.go) looks each one up by name, so a backend that
// omits any of them silently produces a nil-call.
var tlsPALWrappers = []string{
	"pal_tls_ctx_new_client", "pal_tls_ctx_new_server", "pal_tls_ctx_free",
	"pal_tls_ctx_set_verify", "pal_tls_ctx_set_min_version", "pal_tls_ctx_add_ca",
	"pal_tls_ctx_use_cert", "pal_tls_ctx_use_key", "pal_tls_ctx_load_default_trust",
	"pal_tls_new", "pal_tls_set_connect_state", "pal_tls_set_accept_state",
	"pal_tls_set_sni", "pal_tls_set_verify_host", "pal_tls_do_handshake",
	"pal_tls_read", "pal_tls_write", "pal_tls_shutdown",
	"pal_tls_bio_read_out", "pal_tls_bio_write_in", "pal_tls_bio_pending_out",
	"pal_tls_get_version", "pal_tls_get_cipher", "pal_tls_get_verify_result",
	"pal_tls_free",
}

// TestPosixPALEmitTLSSecureTransport validates the macOS Secure Transport backend
// (T1599): every pal_tls_* wrapper is defined, the real framework symbols are
// declared for the link, and no OpenSSL symbol leaks in.
func TestPosixPALEmitTLSSecureTransport(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{target: "arm64-apple-darwin"}
	fns := p.EmitTLSSecureTransport(module)
	out := module.String()

	for _, name := range tlsPALWrappers {
		if fns[name] == nil {
			t.Errorf("EmitTLSSecureTransport did not return %s", name)
		}
		if !strings.Contains(out, "@"+name+"(") {
			t.Errorf("missing definition of @%s", name)
		}
	}

	// Secure Transport / Sec / CoreFoundation externs needed at link time. Each of
	// these must also appear in the hand-authored TBD stubs in cmd/promise/main.go
	// (bundledSecurityTBD / bundledCoreFoundationTBD) or a no-Xcode build fails.
	for _, sym := range []string{
		"@SSLCreateContext", "@SSLSetIOFuncs", "@SSLSetConnection",
		"@SSLSetProtocolVersionMin", "@SSLSetPeerDomainName", "@SSLSetCertificate",
		"@SSLSetSessionOption", "@SSLHandshake", "@SSLRead", "@SSLWrite", "@SSLClose",
		"@SSLGetNegotiatedProtocolVersion", "@SSLGetNegotiatedCipher", "@SSLCopyPeerTrust",
		"@SecItemImport", "@SecIdentityCreate", "@SecTrustEvaluateWithError",
		"@SecTrustSetAnchorCertificates", "@SecTrustSetAnchorCertificatesOnly",
		"@CFRelease", "@CFRetain", "@CFDataCreate", "@CFArrayCreate",
		"@CFArrayGetCount", "@CFArrayGetValueAtIndex", "@kCFTypeArrayCallBacks",
	} {
		if !strings.Contains(out, sym) {
			t.Errorf("missing Secure Transport extern reference %s", sym)
		}
	}

	// The backend must not pull in OpenSSL.
	for _, sym := range []string{"@SSL_new", "@SSL_CTX_new", "@BIO_new", "@BIO_s_mem"} {
		if strings.Contains(out, sym) {
			t.Errorf("darwin backend must not reference OpenSSL symbol %s", sym)
		}
	}

	// kCFTypeArrayCallBacks is a data symbol owned by CoreFoundation: it must be an
	// external declaration, not a definition (a bare `global i8` has no initializer
	// and is rejected by the verifier).
	if !strings.Contains(out, "@kCFTypeArrayCallBacks = external global") {
		t.Error("kCFTypeArrayCallBacks must be declared `external global`")
	}
}

// TestTLSSecureTransportIOCallbacksNeverBlock is the guard for T1599's hard
// constraint: the SSLSetIOFuncs callbacks are invoked synchronously from inside
// SSLHandshake/SSLRead/SSLWrite, i.e. from a C stack frame. Promise parks by
// emitting an inline coro.suspend into the Promise frame, so a callback that
// performed socket I/O or parked would corrupt the coroutine. They must be pure
// buffer accessors over the session's own queues.
func TestTLSSecureTransportIOCallbacksNeverBlock(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{target: "arm64-apple-darwin"}
	p.EmitTLSSecureTransport(module)

	for _, cb := range []string{"__promise_tls_read_cb", "__promise_tls_write_cb"} {
		var fn *ir.Func
		for _, f := range module.Funcs {
			if f.Name() == cb {
				fn = f
				break
			}
		}
		if fn == nil {
			t.Fatalf("%s not emitted", cb)
		}
		// Whatever the callback calls must come from the tiny queue-helper set —
		// never a syscall, a netpoll wait, or a scheduler entry point.
		allowed := map[string]bool{
			"__promise_tls_q_take": true, "__promise_tls_q_append": true,
			"memcpy": true, "pal_alloc": true, "pal_free": true,
		}
		for _, blk := range fn.Blocks {
			for _, inst := range blk.Insts {
				call, ok := inst.(*ir.InstCall)
				if !ok {
					continue
				}
				callee, ok := call.Callee.(*ir.Func)
				if !ok {
					continue
				}
				if !allowed[callee.Name()] {
					t.Errorf("%s calls %s — I/O callbacks must not block, park, or "+
						"perform syscalls (T1599)", cb, callee.Name())
				}
			}
		}
	}
}

// TestTLSSecureTransportNoTLS13 pins the recorded capability gap: Secure Transport
// implements no TLS 1.3 (SSLSetProtocolVersionMin rejects kTLSProtocol13 with
// errSSLIllegalParam), so set_min_version accepts only the TLS 1.2 wire version and
// reports failure for 1.3 rather than silently claiming success.
func TestTLSSecureTransportNoTLS13(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{target: "arm64-apple-darwin"}
	p.EmitTLSSecureTransport(module)
	out := module.String()

	// The wire version compared against is TLS 1.2 (771); kTLSProtocol12 is 8.
	if !strings.Contains(out, "771") {
		t.Error("pal_tls_ctx_set_min_version must compare against the TLS 1.2 wire version 771")
	}
	if stWireTLS13 != 772 || stProtoTLS13 != 10 || stProtoTLS12 != 8 {
		t.Errorf("pinned ABI constants drifted: wire13=%d proto13=%d proto12=%d",
			stWireTLS13, stProtoTLS13, stProtoTLS12)
	}
}

// TestTLSSecureTransportCipherTable guards the cipher-suite name table: entries must
// be unique and non-empty, since a duplicate would make the emitted switch ambiguous
// and an empty name would surface as an empty cipher_suite getter.
func TestTLSSecureTransportCipherTable(t *testing.T) {
	seen := map[int64]bool{}
	for _, cn := range tlsDarwinCipherNames {
		if cn.name == "" {
			t.Errorf("cipher suite 0x%04X has an empty name", cn.suite)
		}
		if seen[cn.suite] {
			t.Errorf("duplicate cipher suite entry 0x%04X", cn.suite)
		}
		seen[cn.suite] = true
	}
	// The suite Secure Transport negotiates for the repo's RSA test certificate.
	if !seen[0xC030] {
		t.Error("table must cover ECDHE-RSA-AES256-GCM-SHA384 (0xC030)")
	}
}

// TestTLSSecureTransportCipherSuiteSlotWidth guards an arch trap: SSLCipherSuite
// is uint16_t on arm64 macOS but uint32_t everywhere else (CipherSuite.h keys the
// typedef on TARGET_CPU_ARM64), and this backend is emitted for both Promise macOS
// arches from the same code. A 2-byte out-param slot would therefore be overrun by
// SSLGetNegotiatedCipher on x86_64 — silent stack corruption that never reproduces
// on an Apple Silicon dev machine.
func TestTLSSecureTransportCipherSuiteSlotWidth(t *testing.T) {
	for _, target := range []string{"arm64-apple-darwin", "x86_64-apple-darwin"} {
		module := ir.NewModule()
		p := &PosixPAL{target: target}
		p.EmitTLSSecureTransport(module)
		out := module.String()

		if !strings.Contains(out, "declare i32 @SSLGetNegotiatedCipher(i8* %ctx, i32* %cipherSuite)") {
			t.Errorf("%s: SSLGetNegotiatedCipher's out-param must be i32* (the widest "+
				"SSLCipherSuite across macOS arches)", target)
		}
		if strings.Contains(out, "i16* %cipherSuite") || strings.Contains(out, "alloca i16") {
			t.Errorf("%s: a 16-bit cipher-suite slot is overrun on x86_64", target)
		}
	}
}

// TestTLSBackendSurfaceParity is the guard for the claim that codegen/tls.go's
// bridge helpers are backend-agnostic and shared verbatim: they are written once
// against a single assumed shape for each pal_tls_* wrapper, so the Linux and
// macOS backends must agree on that shape exactly. A backend that returned i64
// where the other returns i32 (or took an i8* where the other takes an i64) would
// still satisfy the name-only check above, then miscompile every call through the
// shared bridge — silently, since llir does not type-check calls it builds.
func TestTLSBackendSurfaceParity(t *testing.T) {
	linux := (&PosixPAL{target: "x86_64-linux-musl"}).EmitTLS(ir.NewModule())
	darwin := (&PosixPAL{target: "arm64-apple-darwin"}).EmitTLSSecureTransport(ir.NewModule())

	if len(linux) != len(tlsPALWrappers) {
		t.Errorf("OpenSSL backend exports %d wrappers, the shared surface has %d",
			len(linux), len(tlsPALWrappers))
	}
	if len(darwin) != len(tlsPALWrappers) {
		t.Errorf("Secure Transport backend exports %d wrappers, the shared surface has %d",
			len(darwin), len(tlsPALWrappers))
	}

	for _, name := range tlsPALWrappers {
		l, d := linux[name], darwin[name]
		if l == nil || d == nil {
			continue // the per-backend tests already report a missing wrapper
		}
		if ls, ds := l.Sig.String(), d.Sig.String(); ls != ds {
			t.Errorf("%s signature differs between backends: linux %s, darwin %s — "+
				"the shared bridge in codegen/tls.go assumes one shape", name, ls, ds)
		}
	}
}

// TestTLSDarwinExternGlobalDedupes pins that a second request for the same extern
// data symbol reuses the existing declaration. The backend emits into the same
// module as the rest of codegen, so a symbol another emitter already declared must
// not be declared twice — LLVM rejects a duplicate global outright.
func TestTLSDarwinExternGlobalDedupes(t *testing.T) {
	module := ir.NewModule()
	first := tlsDarwinExternGlobal(module, "kCFTypeArrayCallBacks")
	second := tlsDarwinExternGlobal(module, "kCFTypeArrayCallBacks")
	if first != second {
		t.Error("second call declared a new global instead of reusing the existing one")
	}
	if got := strings.Count(module.String(), "@kCFTypeArrayCallBacks ="); got != 1 {
		t.Errorf("kCFTypeArrayCallBacks declared %d times, want 1", got)
	}

	// A different name is still a fresh declaration, and also external.
	other := tlsDarwinExternGlobal(module, "kCFAllocatorDefault")
	if other == first {
		t.Fatal("a distinct symbol name must yield a distinct global")
	}
	if other.Linkage != enum.LinkageExternal {
		t.Errorf("declaration linkage = %v, want external", other.Linkage)
	}
}
