package misc1

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen"
	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
	"github.com/promise-language/promise/compiler/internal/codegen/pal"
)

// TestTLSBridgeLinuxEmitsOpenSSL verifies that a program declaring a
// promise_tls_* extern on a Linux target gets the real OpenSSL memory-BIO
// bridge: the promise_tls_new body calls pal_tls_new, which composes SSL_new,
// and NeedsTLS() flips true so the linker splices OpenSSL (T0077).
func TestTLSBridgeLinuxEmitsOpenSSL(t *testing.T) {
	src := "_tls_new(int ctx) int `extern(\"promise_tls_new\");\n" +
		"main() { int x = _tls_new(0); }\n"
	file, info := codegentest.ParseWithStd(t, src)
	result := codegen.Compile(file, info, "x86_64-unknown-linux-musl")
	ir := result.Module.String()

	codegentest.AssertContains(t, ir, "@promise_tls_new")
	codegentest.AssertContains(t, ir, "@pal_tls_new")
	codegentest.AssertContains(t, ir, "@SSL_new") // real OpenSSL symbol declared for the link
	if !result.NeedsTLS() {
		t.Error("NeedsTLS() must be true for a program declaring a promise_tls_* extern")
	}
}

// TestTLSBridgeDarwinEmitsSecureTransport verifies that a macOS target gets the
// real Secure Transport backend (T1599) rather than a stub: the bridge routes to
// pal_tls_new, which creates an SSLContextRef, and no OpenSSL symbol appears.
func TestTLSBridgeDarwinEmitsSecureTransport(t *testing.T) {
	src := "_tls_new(int ctx) int `extern(\"promise_tls_new\");\n" +
		"main() { int x = _tls_new(0); }\n"
	for _, target := range []string{"arm64-apple-darwin", "x86_64-apple-darwin"} {
		file, info := codegentest.ParseWithStd(t, src)
		result := codegen.Compile(file, info, target)
		ir := result.Module.String()

		codegentest.AssertContains(t, ir, "@promise_tls_new")
		codegentest.AssertContains(t, ir, "@pal_tls_new")
		codegentest.AssertContains(t, ir, "@SSLCreateContext") // real Secure Transport symbol
		if strings.Contains(ir, "@SSL_new") || strings.Contains(ir, "@BIO_new") {
			t.Errorf("%s must not reference OpenSSL symbols", target)
		}
		if !result.NeedsTLS() {
			t.Errorf("%s: NeedsTLS() must be true so the frameworks are linked", target)
		}
	}
}

// TestTLSBridgeBackendlessStubs verifies that on a target with no TLS backend
// (wasm; Windows until T1598) the bridge emits inert stubs — no platform TLS
// symbol is referenced — while NeedsTLS() still reports true (harmless: the link
// gate is per-platform).
func TestTLSBridgeBackendlessStubs(t *testing.T) {
	src := "_tls_new(int ctx) int `extern(\"promise_tls_new\");\n" +
		"main() { int x = _tls_new(0); }\n"
	file, info := codegentest.ParseWithStd(t, src)
	result := codegen.Compile(file, info, "wasm32-wasi")
	ir := result.Module.String()

	codegentest.AssertContains(t, ir, "@promise_tls_new")
	if strings.Contains(ir, "@SSL_new") || strings.Contains(ir, "@pal_tls_new") ||
		strings.Contains(ir, "@SSLCreateContext") {
		t.Error("a backend-less target must not reference any platform TLS symbol")
	}
}

// TestTLSBridgeAllShapesLinux compiles a program declaring every promise_tls_*
// extern and calling each once, so every bridge shape (handle factory, void/1-int,
// void/2-int, int/2-int, int+vector, int+vector+len, int+string, string return) is
// body-filled against the OpenSSL PAL. It pins that each bridge routes to its
// matching pal_tls_* wrapper and that the i32→i64 sign-extend path (tlsStoreInt on
// status/flag returns) is exercised.
func TestTLSBridgeAllShapesLinux(t *testing.T) {
	file, info := codegentest.ParseWithStd(t, codegentest.TlsAllExternsSrc)
	result := codegen.Compile(file, info, "x86_64-unknown-linux-musl")
	ir := result.Module.String()

	// Every bridge must call its matching pal_tls_* wrapper.
	palWrappers := []string{
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
	for _, w := range palWrappers {
		codegentest.AssertContains(t, ir, "@"+w+"(")
	}

	// The 2-int void bridge (set_verify) truncates its second arg to i32 for the
	// SSL_CTX_set_verify mode; the min-version bridge does likewise. Presence of the
	// underlying OpenSSL trunc-fed ctrl call proves the shape wired through.
	codegentest.AssertContains(t, ir, "@SSL_CTX_set_verify")
	// String-return bridges copy the static const char* into a fresh Promise string.
	codegentest.AssertContains(t, ir, "@promise_string_new")
	codegentest.AssertContains(t, ir, "@strlen")
	// String-arg bridges materialize a C string and free it after the call.
	codegentest.AssertContains(t, ir, "@pal_free")

	if !result.NeedsTLS() {
		t.Error("NeedsTLS() must be true when promise_tls_* externs are bridged")
	}
}

// TestTLSBridgeAllShapesDarwin compiles the all-externs program for macOS so every
// bridge shape is body-filled against the Secure Transport PAL. The bridge helpers
// are backend-agnostic and shared verbatim with Linux, so this pins that the same
// 25 shapes route to the same 25 pal_tls_* wrapper names on a second backend.
func TestTLSBridgeAllShapesDarwin(t *testing.T) {
	file, info := codegentest.ParseWithStd(t, codegentest.TlsAllExternsSrc)
	result := codegen.Compile(file, info, "arm64-apple-darwin")
	ir := result.Module.String()

	palWrappers := []string{
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
	for _, w := range palWrappers {
		codegentest.AssertContains(t, ir, "@"+w+"(")
	}
	// Secure Transport, not OpenSSL.
	codegentest.AssertContains(t, ir, "@SSLHandshake")
	codegentest.AssertContains(t, ir, "@SecItemImport")
	for _, sym := range []string{"@SSL_new", "@SSL_CTX_new", "@BIO_new", "@SSL_read"} {
		if strings.Contains(ir, sym) {
			t.Errorf("darwin backend must not reference OpenSSL symbol %s", sym)
		}
	}
	if !result.NeedsTLS() {
		t.Error("NeedsTLS() must be true when promise_tls_* externs are bridged")
	}
}

// TestTLSBridgeProductionMacOSTriple is the regression guard the two -apple-darwin
// tests above cannot be: the triple Promise actually compiles macOS with is
// "arm64-apple-macosx26.0.0" (HostTargetTriple derives it from sw_vers), which
// contains "apple" but NOT "darwin". The backend gate in defineTLSPALBodies matches
// either substring, and only the "apple" alternative carries production. Narrow it
// to just "darwin" and every existing TLS test still passes while every real macOS
// build silently falls back to inert stubs — TLS programs would compile, link, and
// then raise TlsError(kind: unsupported) at runtime.
func TestTLSBridgeProductionMacOSTriple(t *testing.T) {
	src := "_tls_new(int ctx) int `extern(\"promise_tls_new\");\n" +
		"main() { int x = _tls_new(0); }\n"
	// The forms HostTargetTriple can produce, on both macOS arches. None of them
	// contains the word "darwin".
	for _, target := range []string{
		"arm64-apple-macosx26.0.0",
		"arm64-apple-macosx14.0.0",
		"x86_64-apple-macosx10.15.0",
	} {
		if strings.Contains(target, "darwin") {
			t.Fatalf("%s defeats the point of this test — pick a triple without \"darwin\"", target)
		}
		file, info := codegentest.ParseWithStd(t, src)
		result := codegen.Compile(file, info, target)
		ir := result.Module.String()

		if !strings.Contains(ir, "@SSLCreateContext") {
			t.Errorf("%s got no Secure Transport backend — the production macOS triple "+
				"must not fall through to the stub path (T1599)", target)
		}
		codegentest.AssertContains(t, ir, "@pal_tls_new")
		if !result.NeedsTLS() {
			t.Errorf("%s: NeedsTLS() must be true so Security/CoreFoundation are linked", target)
		}
	}
}

// TestTLSBridgeUnknownPosixTargetStubs covers the backend switch's default arm.
// The two stub tests above use wasm, which never reaches the switch at all —
// pal.ForTarget returns a WasmPAL, so the earlier *PosixPAL type assertion fails
// and returns first. Windows takes that same early return. A POSIX-ish target that
// is neither Linux nor macOS (FreeBSD, or any triple Promise gains before it gains
// a TLS backend for it) is the only thing that reaches `default:`, and it must
// degrade to stubs rather than emitting OpenSSL calls that cannot link.
func TestTLSBridgeUnknownPosixTargetStubs(t *testing.T) {
	for _, target := range []string{"x86_64-unknown-freebsd", "riscv64-unknown-none"} {
		if p, ok := pal.ForTarget(target).(*pal.PosixPAL); !ok || p == nil {
			t.Fatalf("%s no longer resolves to a PosixPAL — this test no longer reaches "+
				"the backend switch's default arm", target)
		}
		file, info := codegentest.ParseWithStd(t, codegentest.TlsAllExternsSrc)
		result := codegen.Compile(file, info, target)
		ir := result.Module.String()

		if strings.Contains(ir, "@pal_tls_") {
			t.Errorf("%s: a target with no TLS backend must not call any pal_tls_* wrapper", target)
		}
		for _, sym := range []string{"@SSL_new", "@SSL_CTX_new", "@BIO_new",
			"@SSLCreateContext", "@SSLHandshake", "@SecItemImport"} {
			if strings.Contains(ir, sym) {
				t.Errorf("%s: stub path must not reference platform TLS symbol %s", target, sym)
			}
		}
		// Still fully defined, so the module links and fails cleanly at runtime.
		for _, name := range []string{"@promise_tls_ctx_new_client", "@promise_tls_new",
			"@promise_tls_do_handshake", "@promise_tls_get_cipher"} {
			codegentest.AssertContains(t, ir, name)
		}
	}
}

// TestTLSBridgeWindowsEmitsSChannel verifies that a program declaring a
// promise_tls_* extern on a Windows target gets the real SChannel backend rather
// than the unsupported-platform stubs: the bridge routes to pal_tls_new, which
// composes an SSPI credential, and no OpenSSL symbol is pulled in (T1598).
func TestTLSBridgeWindowsEmitsSChannel(t *testing.T) {
	src := "_tls_new(int ctx) int `extern(\"promise_tls_new\");\n" +
		"main() { int x = _tls_new(0); }\n"
	file, info := codegentest.ParseWithStd(t, src)
	result := codegen.Compile(file, info, "x86_64-pc-windows-msvc")
	ir := result.Module.String()

	codegentest.AssertContains(t, ir, "@promise_tls_new")
	codegentest.AssertContains(t, ir, "@pal_tls_new")
	codegentest.AssertContains(t, ir, "@AcquireCredentialsHandleA") // real SSPI symbol for the link
	if strings.Contains(ir, "@SSL_new") || strings.Contains(ir, "@SSL_CTX_new") {
		t.Error("Windows target must not reference OpenSSL symbols")
	}
	if !result.NeedsTLS() {
		t.Error("NeedsTLS() must be true for a program declaring a promise_tls_* extern")
	}
}

// TestTLSBridgeAllShapesWindows compiles the all-externs program for Windows so
// every bridge shape is body-filled against the SChannel PAL, pinning that the
// bridge layer is genuinely backend-agnostic: the exact same wirings that drive
// OpenSSL on Linux drive SSPI here.
func TestTLSBridgeAllShapesWindows(t *testing.T) {
	file, info := codegentest.ParseWithStd(t, codegentest.TlsAllExternsSrc)
	result := codegen.Compile(file, info, "x86_64-pc-windows-msvc")
	ir := result.Module.String()

	for _, w := range []string{
		"pal_tls_ctx_new_client", "pal_tls_ctx_new_server", "pal_tls_ctx_free",
		"pal_tls_ctx_set_verify", "pal_tls_ctx_set_min_version", "pal_tls_ctx_add_ca",
		"pal_tls_ctx_use_cert", "pal_tls_ctx_use_key", "pal_tls_ctx_load_default_trust",
		"pal_tls_new", "pal_tls_set_connect_state", "pal_tls_set_accept_state",
		"pal_tls_set_sni", "pal_tls_set_verify_host", "pal_tls_do_handshake",
		"pal_tls_read", "pal_tls_write", "pal_tls_shutdown",
		"pal_tls_bio_read_out", "pal_tls_bio_write_in", "pal_tls_bio_pending_out",
		"pal_tls_get_version", "pal_tls_get_cipher", "pal_tls_get_verify_result",
		"pal_tls_free",
	} {
		codegentest.AssertContains(t, ir, "@"+w+"(")
	}
	// Handshake and record crypto go through SSPI, certificates through crypt32.
	codegentest.AssertContains(t, ir, "@InitializeSecurityContextA")
	codegentest.AssertContains(t, ir, "@AcceptSecurityContext")
	codegentest.AssertContains(t, ir, "@DecryptMessage")
	codegentest.AssertContains(t, ir, "@EncryptMessage")
	codegentest.AssertContains(t, ir, "@CertGetCertificateChain")
	// String-return bridges still copy into a fresh Promise string.
	codegentest.AssertContains(t, ir, "@promise_string_new")
	if !result.NeedsTLS() {
		t.Error("NeedsTLS() must be true when promise_tls_* externs are bridged")
	}
}
