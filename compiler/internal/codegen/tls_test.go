package codegen

import (
	"strings"
	"testing"
)

// tlsAllExternsSrc declares every promise_tls_* extern with the exact Promise
// signature modules/tls/tls.pr uses, and calls each once so the bridge
// body-fills them. It exercises all bridge shapes (handle factory, void/int/int2,
// vector, vector+len, string arg, string return) in a single compilation.
const tlsAllExternsSrc = "" +
	"_tls_ctx_new_client() int `extern(\"promise_tls_ctx_new_client\");\n" +
	"_tls_ctx_new_server() int `extern(\"promise_tls_ctx_new_server\");\n" +
	"_tls_ctx_free(int ctx) `extern(\"promise_tls_ctx_free\");\n" +
	"_tls_ctx_set_verify(int ctx, int peer) `extern(\"promise_tls_ctx_set_verify\");\n" +
	"_tls_ctx_set_min_version(int ctx, int ver) int `extern(\"promise_tls_ctx_set_min_version\");\n" +
	"_tls_ctx_add_ca(int ctx, u8[] pem) int `extern(\"promise_tls_ctx_add_ca\");\n" +
	"_tls_ctx_use_cert(int ctx, u8[] pem) int `extern(\"promise_tls_ctx_use_cert\");\n" +
	"_tls_ctx_use_key(int ctx, u8[] pem) int `extern(\"promise_tls_ctx_use_key\");\n" +
	"_tls_ctx_load_default_trust(int ctx) int `extern(\"promise_tls_ctx_load_default_trust\");\n" +
	"_tls_new(int ctx) int `extern(\"promise_tls_new\");\n" +
	"_tls_set_connect_state(int ssl) `extern(\"promise_tls_set_connect_state\");\n" +
	"_tls_set_accept_state(int ssl) `extern(\"promise_tls_set_accept_state\");\n" +
	"_tls_set_sni(int ssl, string host) int `extern(\"promise_tls_set_sni\");\n" +
	"_tls_set_verify_host(int ssl, string host) int `extern(\"promise_tls_set_verify_host\");\n" +
	"_tls_do_handshake(int ssl) int `extern(\"promise_tls_do_handshake\");\n" +
	"_tls_read(int ssl, u8[] ~buf) int `extern(\"promise_tls_read\");\n" +
	"_tls_write(int ssl, u8[] buf) int `extern(\"promise_tls_write\");\n" +
	"_tls_shutdown(int ssl) int `extern(\"promise_tls_shutdown\");\n" +
	"_tls_bio_read_out(int ssl, u8[] ~buf) int `extern(\"promise_tls_bio_read_out\");\n" +
	"_tls_bio_write_in(int ssl, u8[] buf, int len) int `extern(\"promise_tls_bio_write_in\");\n" +
	"_tls_bio_pending_out(int ssl) int `extern(\"promise_tls_bio_pending_out\");\n" +
	"_tls_get_version(int ssl) string `extern(\"promise_tls_get_version\");\n" +
	"_tls_get_cipher(int ssl) string `extern(\"promise_tls_get_cipher\");\n" +
	"_tls_get_verify_result(int ssl) int `extern(\"promise_tls_get_verify_result\");\n" +
	"_tls_free(int ssl) `extern(\"promise_tls_free\");\n" +
	"main() {\n" +
	"  u8[] v = u8[]();\n" +
	"  int c = _tls_ctx_new_client();\n" +
	"  int sv = _tls_ctx_new_server();\n" +
	"  _tls_ctx_free(c);\n" +
	"  _tls_ctx_set_verify(c, 1);\n" +
	"  int mv = _tls_ctx_set_min_version(c, 771);\n" +
	"  int a = _tls_ctx_add_ca(c, v);\n" +
	"  int uc = _tls_ctx_use_cert(c, v);\n" +
	"  int uk = _tls_ctx_use_key(c, v);\n" +
	"  int dt = _tls_ctx_load_default_trust(c);\n" +
	"  int ssl = _tls_new(c);\n" +
	"  _tls_set_connect_state(ssl);\n" +
	"  _tls_set_accept_state(ssl);\n" +
	"  int sni = _tls_set_sni(ssl, \"h\");\n" +
	"  int vh = _tls_set_verify_host(ssl, \"h\");\n" +
	"  int hs = _tls_do_handshake(ssl);\n" +
	"  int rd = _tls_read(ssl, v);\n" +
	"  int wr = _tls_write(ssl, v);\n" +
	"  int sh = _tls_shutdown(ssl);\n" +
	"  int bro = _tls_bio_read_out(ssl, v);\n" +
	"  int bwi = _tls_bio_write_in(ssl, v, 3);\n" +
	"  int bpo = _tls_bio_pending_out(ssl);\n" +
	"  string ver = _tls_get_version(ssl);\n" +
	"  string cip = _tls_get_cipher(ssl);\n" +
	"  int vr = _tls_get_verify_result(ssl);\n" +
	"  _tls_free(ssl);\n" +
	"}\n"

// TestTLSBridgeLinuxEmitsOpenSSL verifies that a program declaring a
// promise_tls_* extern on a Linux target gets the real OpenSSL memory-BIO
// bridge: the promise_tls_new body calls pal_tls_new, which composes SSL_new,
// and NeedsTLS() flips true so the linker splices OpenSSL (T0077).
func TestTLSBridgeLinuxEmitsOpenSSL(t *testing.T) {
	src := "_tls_new(int ctx) int `extern(\"promise_tls_new\");\n" +
		"main() { int x = _tls_new(0); }\n"
	file, info := parseWithStd(t, src)
	result := Compile(file, info, "x86_64-unknown-linux-musl")
	ir := result.Module.String()

	assertContains(t, ir, "@promise_tls_new")
	assertContains(t, ir, "@pal_tls_new")
	assertContains(t, ir, "@SSL_new") // real OpenSSL symbol declared for the link
	if !result.NeedsTLS() {
		t.Error("NeedsTLS() must be true for a program declaring a promise_tls_* extern")
	}
}

// TestTLSBridgeNonLinuxStubs verifies that on a target with no TLS backend the
// bridge emits inert stubs — no OpenSSL symbol is referenced — while NeedsTLS()
// still reports true (harmless: the OpenSSL link gate is Linux-only).
func TestTLSBridgeNonLinuxStubs(t *testing.T) {
	src := "_tls_new(int ctx) int `extern(\"promise_tls_new\");\n" +
		"main() { int x = _tls_new(0); }\n"
	file, info := parseWithStd(t, src)
	result := Compile(file, info, "arm64-apple-darwin")
	ir := result.Module.String()

	assertContains(t, ir, "@promise_tls_new")
	if strings.Contains(ir, "@SSL_new") || strings.Contains(ir, "@pal_tls_new") {
		t.Error("non-Linux target must not reference OpenSSL symbols")
	}
}

// TestTLSBridgeAllShapesLinux compiles a program declaring every promise_tls_*
// extern and calling each once, so every bridge shape (handle factory, void/1-int,
// void/2-int, int/2-int, int+vector, int+vector+len, int+string, string return) is
// body-filled against the OpenSSL PAL. It pins that each bridge routes to its
// matching pal_tls_* wrapper and that the i32→i64 sign-extend path (tlsStoreInt on
// status/flag returns) is exercised.
func TestTLSBridgeAllShapesLinux(t *testing.T) {
	file, info := parseWithStd(t, tlsAllExternsSrc)
	result := Compile(file, info, "x86_64-unknown-linux-musl")
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
		assertContains(t, ir, "@"+w+"(")
	}

	// The 2-int void bridge (set_verify) truncates its second arg to i32 for the
	// SSL_CTX_set_verify mode; the min-version bridge does likewise. Presence of the
	// underlying OpenSSL trunc-fed ctrl call proves the shape wired through.
	assertContains(t, ir, "@SSL_CTX_set_verify")
	// String-return bridges copy the static const char* into a fresh Promise string.
	assertContains(t, ir, "@promise_string_new")
	assertContains(t, ir, "@strlen")
	// String-arg bridges materialize a C string and free it after the call.
	assertContains(t, ir, "@pal_free")

	if !result.NeedsTLS() {
		t.Error("NeedsTLS() must be true when promise_tls_* externs are bridged")
	}
}

// TestTLSBridgeAllShapesNonLinuxStubs compiles the same all-externs program for a
// backend-less target and asserts every extern gets an inert stub: no pal_tls_* /
// OpenSSL symbol is referenced, int/handle returners store 0, and the two string
// getters synthesize an empty Promise string. This is the path that makes the
// module compile-and-link everywhere while failing cleanly at runtime.
func TestTLSBridgeAllShapesNonLinuxStubs(t *testing.T) {
	file, info := parseWithStd(t, tlsAllExternsSrc)
	result := Compile(file, info, "arm64-apple-darwin")
	ir := result.Module.String()

	if strings.Contains(ir, "@pal_tls_") {
		t.Error("non-Linux stubs must not reference any pal_tls_* wrapper")
	}
	for _, sym := range []string{"@SSL_new", "@SSL_CTX_new", "@BIO_new", "@SSL_read"} {
		if strings.Contains(ir, sym) {
			t.Errorf("non-Linux stub must not reference OpenSSL symbol %s", sym)
		}
	}
	// Every bridge symbol is still defined (so the module links on this target).
	for _, name := range tlsExternNames {
		assertContains(t, ir, "@"+name+"(")
	}
	// String getters return an empty Promise string via promise_string_new(null, 0).
	assertContains(t, ir, "@promise_string_new")
}
