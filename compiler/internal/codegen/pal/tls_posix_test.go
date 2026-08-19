package pal

import (
	"strings"
	"testing"

	"github.com/llir/llvm/ir"
)

// TestPosixPALEmitTLS validates the OpenSSL memory-BIO backend (T0077): the real
// exported symbols are declared, every pal_tls_* wrapper is defined, and the
// wrappers compose SSL_new + two memory BIOs (not a socket fd).
func TestPosixPALEmitTLS(t *testing.T) {
	module := ir.NewModule()
	p := &PosixPAL{target: "x86_64-unknown-linux-musl"}
	fns := p.EmitTLS(module)
	out := module.String()

	// Every pal_tls_* wrapper the codegen bridge references must be emitted.
	wanted := []string{
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
	for _, name := range wanted {
		if fns[name] == nil {
			t.Errorf("EmitTLS did not return %s", name)
		}
		if !strings.Contains(out, "define") || !strings.Contains(out, "@"+name+"(") {
			t.Errorf("missing definition of @%s", name)
		}
	}

	// The real OpenSSL symbols must be declared as externs.
	for _, sym := range []string{
		"@SSL_CTX_new", "@TLS_client_method", "@TLS_server_method", "@SSL_new",
		"@SSL_set_bio", "@BIO_new", "@BIO_s_mem", "@SSL_do_handshake",
		"@SSL_get_error", "@SSL_read", "@SSL_write", "@SSL_ctrl", "@SSL_CTX_ctrl",
		"@BIO_ctrl", "@SSL_free", "@SSL_get_rbio", "@SSL_get_wbio",
	} {
		if !strings.Contains(out, sym) {
			t.Errorf("missing OpenSSL extern reference %s", sym)
		}
	}

	// pal_tls_new must build the session from two in-memory BIOs (no socket fd).
	if !strings.Contains(out, "call i8* @SSL_new") {
		t.Error("pal_tls_new must call SSL_new")
	}
	if strings.Count(out, "call i8* @BIO_new(") < 2 {
		t.Error("pal_tls_new must create two memory BIOs via BIO_new")
	}
	if !strings.Contains(out, "@SSL_set_bio") {
		t.Error("pal_tls_new must wire the BIOs with SSL_set_bio")
	}

	// SNI uses SSL_ctrl op 55 (SSL_CTRL_SET_TLSEXT_HOSTNAME); the min-version
	// setter uses SSL_CTX_ctrl op 123 (SSL_CTRL_SET_MIN_PROTO_VERSION).
	if !strings.Contains(out, "call i64 @SSL_ctrl(i8* %0, i32 55,") {
		t.Error("pal_tls_set_sni must call SSL_ctrl with op 55 (SSL_CTRL_SET_TLSEXT_HOSTNAME)")
	}
	if !strings.Contains(out, "call i64 @SSL_CTX_ctrl(i8* %0, i32 123,") {
		t.Error("pal_tls_ctx_set_min_version must call SSL_CTX_ctrl with op 123 (SSL_CTRL_SET_MIN_PROTO_VERSION)")
	}
}
