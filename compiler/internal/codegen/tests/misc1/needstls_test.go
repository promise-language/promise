package misc1

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen"
)

// TestNeedsTLS exercises the OpenSSL link gate (T1596 / #28). NeedsTLS is true iff the
// program declares at least one promise_tls_* extern — the by-construction gate
// that keeps non-TLS programs from linking OpenSSL. Since T0077 the tls module
// declares those externs, so the gate is live; here we drive both branches
// directly via the exported Externs field.
func TestNeedsTLS(t *testing.T) {
	cases := []struct {
		name    string
		externs []*codegen.ExternFunc
		want    bool
	}{
		{"nil externs", nil, false},
		{"no tls externs", []*codegen.ExternFunc{{CName: "promise_string_len"}, {CName: "pal_write"}}, false},
		{"one tls extern", []*codegen.ExternFunc{{CName: "promise_string_len"}, {CName: "promise_tls_client_new"}}, true},
		{"only tls extern", []*codegen.ExternFunc{{CName: "promise_tls_handshake"}}, true},
		{"nil entry is skipped", []*codegen.ExternFunc{nil, {CName: "promise_tls_read"}}, true},
		{"prefix must be exact", []*codegen.ExternFunc{{CName: "xpromise_tls_read"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &codegen.CompileResult{Externs: tc.externs}
			if got := r.NeedsTLS(); got != tc.want {
				t.Errorf("NeedsTLS() = %v, want %v", got, tc.want)
			}
		})
	}
}
