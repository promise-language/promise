package pal

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
)

// TestWindowsPALEmitTLS validates the SChannel backend (T1598): it exposes
// exactly the surface the codegen bridge calls, declares the real SSPI/crypt32/
// CNG symbols, and drives the handshake through in-memory buffers rather than a
// socket.
func TestWindowsPALEmitTLS(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	fns := p.EmitTLS(module)
	out := module.String()

	// Every pal_tls_* wrapper the codegen bridge references must be emitted —
	// and the list is deliberately identical to the OpenSSL backend's.
	for _, name := range tlsPALSurface {
		if fns[name] == nil {
			t.Errorf("EmitTLS did not return %s", name)
		}
		if !strings.Contains(out, "@"+name+"(") {
			t.Errorf("missing definition of @%s", name)
		}
	}

	// The real Windows symbols must be declared as externs for the link.
	for _, sym := range []string{
		"@AcquireCredentialsHandleA", "@FreeCredentialsHandle",
		"@InitializeSecurityContextA", "@AcceptSecurityContext",
		"@DeleteSecurityContext", "@FreeContextBuffer", "@QueryContextAttributesA",
		"@EncryptMessage", "@DecryptMessage", "@ApplyControlToken",
		"@CryptStringToBinaryA", "@CertCreateCertificateContext",
		"@CertFreeCertificateContext", "@CertOpenStore", "@CertCloseStore",
		"@CertAddEncodedCertificateToStore", "@CertFindCertificateInStore",
		"@CertGetCertificateChain", "@CertFreeCertificateChain",
		"@CertVerifyCertificateChainPolicy", "@CertSetCertificateContextProperty",
		"@NCryptOpenStorageProvider", "@NCryptImportKey", "@NCryptFreeObject",
		"@NCryptDeleteKey",
	} {
		if !strings.Contains(out, sym) {
			t.Errorf("missing Windows extern reference %s", sym)
		}
	}

	// No OpenSSL symbol may leak into the Windows backend.
	for _, sym := range []string{"@SSL_new", "@SSL_CTX_new", "@BIO_new", "@SSL_read"} {
		if strings.Contains(out, sym) {
			t.Errorf("Windows backend must not reference OpenSSL symbol %s", sym)
		}
	}

	// The backend never sees a socket or the reactor — that stays in Promise.
	for _, sym := range []string{"@socket", "@recv(", "@send(", "@WSAPoll", "@pal_reactor"} {
		if strings.Contains(out, sym) {
			t.Errorf("TLS backend must not touch the socket layer (%s)", sym)
		}
	}
}

// tlsPALSurface is the backend-neutral pal_tls_* contract (T0077 §4). Both
// backends must define exactly these.
var tlsPALSurface = []string{
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

// TestTLSBackendsAgreeOnSignatures is the guard that keeps a second backend a
// drop-in: codegen/tls.go bridges both with the same call shapes, so the Windows
// and OpenSSL wrappers must agree on parameter and result types name-for-name.
func TestTLSBackendsAgreeOnSignatures(t *testing.T) {
	winFns := (&WindowsPAL{}).EmitTLS(ir.NewModule())
	posixFns := (&PosixPAL{target: "x86_64-unknown-linux-musl"}).EmitTLS(ir.NewModule())

	for _, name := range tlsPALSurface {
		w, p := winFns[name], posixFns[name]
		if w == nil || p == nil {
			t.Fatalf("%s: windows=%v posix=%v", name, w != nil, p != nil)
		}
		if got, want := w.Sig.RetType.String(), p.Sig.RetType.String(); got != want {
			t.Errorf("%s: return type %s on Windows, %s on POSIX", name, got, want)
		}
		if len(w.Params) != len(p.Params) {
			t.Errorf("%s: %d params on Windows, %d on POSIX", name, len(w.Params), len(p.Params))
			continue
		}
		for i := range w.Params {
			if got, want := w.Params[i].Typ.String(), p.Params[i].Typ.String(); got != want {
				t.Errorf("%s: param %d is %s on Windows, %s on POSIX", name, i, got, want)
			}
		}
	}
}

// TestWindowsPALTLSStatusMapping pins the backend-neutral status enum: no
// SECURITY_STATUS may cross the PAL boundary, so the handshake/read/write/
// shutdown wrappers must only ever return the documented small integers.
func TestWindowsPALTLSStatusMapping(t *testing.T) {
	module := ir.NewModule()
	fns := (&WindowsPAL{}).EmitTLS(module)

	// do_handshake: 0 ok, 1 want-more, -1 fatal — and nothing else.
	assertReturnsOnly(t, fns["pal_tls_do_handshake"], irtypes.I32, []int64{0, 1, -1})
	// read: >0 bytes (a runtime value), 0 EOF, -1 want_read, -3 fatal.
	assertReturnsOnly(t, fns["pal_tls_read"], irtypes.I64, []int64{0, -1, -3})
	// write: >0 bytes (runtime), -3 fatal.
	assertReturnsOnly(t, fns["pal_tls_write"], irtypes.I64, []int64{-3})
	// shutdown is best-effort and always reports done.
	assertReturnsOnly(t, fns["pal_tls_shutdown"], irtypes.I32, []int64{0})
}

// assertReturnsOnly checks that every *constant* the function returns is in
// `allowed`. Non-constant returns (byte counts) are ignored.
func assertReturnsOnly(t *testing.T, fn *ir.Func, want irtypes.Type, allowed []int64) {
	t.Helper()
	if fn == nil {
		t.Fatal("missing function")
	}
	if got := fn.Sig.RetType.String(); got != want.String() {
		t.Errorf("%s returns %s, want %s", fn.Name(), got, want)
	}
	ok := make(map[string]bool, len(allowed))
	for _, v := range allowed {
		ok[itoaSigned(v)] = true
	}
	for _, blk := range fn.Blocks {
		ret, isRet := blk.Term.(*ir.TermRet)
		if !isRet || ret.X == nil {
			continue
		}
		s := ret.X.Ident()
		if strings.HasPrefix(s, "%") {
			continue // runtime value (a byte count)
		}
		if !ok[s] {
			t.Errorf("%s returns disallowed constant %s — a backend status code may be leaking "+
				"through the PAL boundary", fn.Name(), s)
		}
	}
}

// itoaSigned renders a signed constant the way llir prints it.
func itoaSigned(v int64) string {
	if v < 0 {
		return "-" + itoa(int(-v))
	}
	return itoa(int(v))
}

// --- resource-release audit -------------------------------------------------
//
// The Promise-level leak detector counts *Promise heap* allocations, so it is
// blind to the handles this backend acquires from the OS: credential handles,
// security contexts, certificate contexts, certificate stores, CNG keys, and
// SSPI-allocated output tokens. A leak of any of those is invisible at runtime
// until a long-lived process runs out of them, so these tests are the only
// guard — they assert the release call directly in the emitted IR.

// TestWindowsPALTLSReleasesAcquiredHandles pins that every OS handle the backend
// acquires is released, by the function responsible for owning it (T1598).
func TestWindowsPALTLSReleasesAcquiredHandles(t *testing.T) {
	module := ir.NewModule()
	(&WindowsPAL{}).EmitTLS(module)
	byName := funcsByName(module)

	for _, tc := range []struct {
		fn      string
		acquire string // the paired acquisition, for the failure message
		release []string
	}{
		{
			fn:      "pal_tls_ctx_free",
			acquire: "AcquireCredentialsHandle / CertOpenStore / NCryptImportKey",
			release: []string{
				"FreeCredentialsHandle",      // the credential
				"CertFreeCertificateContext", // the leaf certificate
				"CertCloseStore",             // the extra-roots store
				"NCryptDeleteKey",            // the imported key, removed from the CNG store
				"NCryptFreeObject",           // the storage provider
				"pal_free",                   // the context struct and the key name
			},
		},
		{
			fn:      "pal_tls_free",
			acquire: "InitializeSecurityContext / AcceptSecurityContext",
			release: []string{"DeleteSecurityContext", "pal_free"},
		},
		{
			fn:      "__pal_tls_verify",
			acquire: "CertGetCertificateChain / QueryContextAttributes(REMOTE_CERT_CONTEXT)",
			release: []string{"CertFreeCertificateChain", "CertFreeCertificateContext"},
		},
		{
			fn:      "__pal_tls_hs_step",
			acquire: "the SSPI-allocated handshake output token",
			release: []string{"FreeContextBuffer"},
		},
	} {
		fn := byName[tc.fn]
		if fn == nil {
			t.Errorf("%s is not defined", tc.fn)
			continue
		}
		calls := callTargets(fn)
		for _, want := range tc.release {
			if calls[want] == 0 {
				t.Errorf("@%s never calls @%s - the handle from %s leaks",
					tc.fn, want, tc.acquire)
			}
		}
	}
}

// TestWindowsPALTLSUseKeyFailurePathsClean covers the path with the most to
// leak: pal_tls_ctx_use_key opens a CNG provider, imports a key into it, then
// attaches it to the certificate, and can fail after each step. Every bail-out
// block must give back exactly what it had taken by then — a provider left open
// on an error path leaks silently, because the config still drops normally.
func TestWindowsPALTLSUseKeyFailurePathsClean(t *testing.T) {
	fns := (&WindowsPAL{}).EmitTLS(ir.NewModule())
	fn := fns["pal_tls_ctx_use_key"]
	if fn == nil {
		t.Fatal("pal_tls_ctx_use_key is not defined")
	}

	for _, tc := range []struct {
		block string
		want  []string
		why   string
	}{
		{".provider_failed", []string{"pal_free"},
			"the decoded DER is ours to free; the provider never opened"},
		{".import_failed", []string{"NCryptFreeObject", "pal_free"},
			"the provider is open but no key was imported"},
		{".attach_failed", []string{"NCryptDeleteKey", "NCryptFreeObject", "pal_free"},
			"the key reached the CNG store but the certificate never adopted it"},
	} {
		blk := blockByName(fn, tc.block)
		if blk == nil {
			t.Errorf("pal_tls_ctx_use_key has no %s block - if the failure paths were "+
				"restructured, re-audit their cleanup and update this test", tc.block)
			continue
		}
		calls := blockCallTargets(blk)
		for _, want := range tc.want {
			if calls[want] == 0 {
				t.Errorf("pal_tls_ctx_use_key%s does not call @%s (%s)", tc.block, want, tc.why)
			}
		}
		// A bail-out block must actually bail out.
		if ret, ok := blk.Term.(*ir.TermRet); !ok || ret.X == nil || ret.X.Ident() != "0" {
			t.Errorf("pal_tls_ctx_use_key%s must return 0 (failure)", tc.block)
		}
	}
}

// TestWindowsPALTLSAcquireReleasePairsDeclared is the coarse backstop for the
// audit above: if a future change starts acquiring a new kind of OS handle, its
// releasing counterpart must appear in the module too. Catches "added the
// acquire, forgot the free" before it ever reaches a running program.
func TestWindowsPALTLSAcquireReleasePairsDeclared(t *testing.T) {
	module := ir.NewModule()
	(&WindowsPAL{}).EmitTLS(module)
	out := module.String()

	for acquire, release := range map[string]string{
		"AcquireCredentialsHandleA":    "FreeCredentialsHandle",
		"CertOpenStore":                "CertCloseStore",
		"CertCreateCertificateContext": "CertFreeCertificateContext",
		"CertFindCertificateInStore":   "CertFreeCertificateContext",
		"CertGetCertificateChain":      "CertFreeCertificateChain",
		"NCryptOpenStorageProvider":    "NCryptFreeObject",
		"NCryptImportKey":              "NCryptDeleteKey",
		"InitializeSecurityContextA":   "DeleteSecurityContext",
		"AcceptSecurityContext":        "DeleteSecurityContext",
	} {
		if strings.Contains(out, "@"+acquire) && !strings.Contains(out, "@"+release) {
			t.Errorf("the backend calls @%s but never @%s - that handle is never given back",
				acquire, release)
		}
	}
}

// --- full-surface status contract -------------------------------------------

// TestWindowsPALTLSSurfaceReturnsOnlyNeutralConstants extends the status-enum
// check from the four I/O entry points to the *whole* pal_tls_* surface. The
// acceptance contract is that no SECURITY_STATUS crosses the PAL boundary, and
// an SSPI status is a large value (SEC_E_* are 0x8009xxxx, SEC_I_* 0x0009xxxx),
// so one leaking through shows up unmistakably as a constant outside these tiny
// documented sets.
func TestWindowsPALTLSSurfaceReturnsOnlyNeutralConstants(t *testing.T) {
	fns := (&WindowsPAL{}).EmitTLS(ir.NewModule())

	// Every value-returning entry point with the exact set of constants it may
	// produce. Non-constant returns (handles, byte counts) are ignored.
	allowed := map[string][]int64{
		"pal_tls_ctx_new_client":         {}, // a handle, or 0 from a runtime select
		"pal_tls_ctx_new_server":         {},
		"pal_tls_ctx_set_min_version":    {1}, // always accepted; the floor is applied locally
		"pal_tls_ctx_add_ca":             {0}, // 1 comes from a runtime check
		"pal_tls_ctx_use_cert":           {0, 1},
		"pal_tls_ctx_use_key":            {0, 1},
		"pal_tls_ctx_load_default_trust": {1}, // Windows always has a ROOT store
		"pal_tls_new":                    {0},
		"pal_tls_set_sni":                {1},
		"pal_tls_set_verify_host":        {},
		"pal_tls_do_handshake":           {0, 1, -1},
		"pal_tls_read":                   {0, -1, -3},
		"pal_tls_write":                  {-3},
		"pal_tls_shutdown":               {0},
		"pal_tls_bio_read_out":           {},
		"pal_tls_bio_write_in":           {},
		"pal_tls_bio_pending_out":        {},
		"pal_tls_get_verify_result":      {0},
	}
	for name, want := range allowed {
		fn := fns[name]
		if fn == nil {
			t.Errorf("%s is not defined", name)
			continue
		}
		assertReturnsOnly(t, fn, fn.Sig.RetType, want)
	}

	// And the surface must be fully accounted for: anything returning a value
	// that is absent from the table above has no pinned status contract at all.
	for _, name := range tlsPALSurface {
		fn := fns[name]
		if fn == nil || fn.Sig.RetType.Equal(irtypes.Void) {
			continue
		}
		if _, ok := allowed[name]; ok {
			continue
		}
		if fn.Sig.RetType.Equal(irtypes.I8Ptr) {
			continue // string getters — covered by the version-string contract below
		}
		t.Errorf("%s returns %s but has no entry in the status-constant table - "+
			"add one so its contract is pinned", name, fn.Sig.RetType)
	}
}

// --- cross-layer version-string contract ------------------------------------

// TestWindowsPALTLSVersionStringsMatchModule pins a contract that spans Go and
// Promise: pal_tls_get_version returns a *string* in OpenSSL's spelling, because
// modules/tls/tls.pr compares it literally to decide TlsVersion. Had the
// SChannel backend spelled it "TLS 1.3", that comparison would fall through and
// every connection would report TLS 1.2 — a wrong answer rather than a failure,
// which nothing short of a real 1.3 handshake would catch.
func TestWindowsPALTLSVersionStringsMatchModule(t *testing.T) {
	module := ir.NewModule()
	(&WindowsPAL{}).EmitTLS(module)
	out := module.String()

	src, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "modules", "tls", "tls.pr"))
	if err != nil {
		t.Fatalf("read modules/tls/tls.pr: %v", err)
	}
	// Every literal tls.pr compares the backend's version string against.
	lits := regexp.MustCompile(`_tls_get_version\([^)]*\)\s*==\s*"([^"]*)"`).
		FindAllStringSubmatch(string(src), -1)
	if len(lits) == 0 {
		t.Fatal("tls.pr no longer compares _tls_get_version() to a literal - " +
			"the version contract moved and this test needs updating")
	}
	for _, m := range lits {
		if !strings.Contains(out, `c"`+m[1]+`\00"`) {
			t.Errorf("tls.pr matches version %q but the SChannel backend never emits that "+
				"exact string - the negotiated version would be misreported", m[1])
		}
	}
}

// --- IR-walking helpers -----------------------------------------------------

// funcsByName indexes a module's *defined* functions by name.
func funcsByName(m *ir.Module) map[string]*ir.Func {
	out := make(map[string]*ir.Func, len(m.Funcs))
	for _, fn := range m.Funcs {
		if len(fn.Blocks) > 0 {
			out[fn.Name()] = fn
		}
	}
	return out
}

// callTargets counts the calls a function makes, keyed by callee name.
func callTargets(fn *ir.Func) map[string]int {
	out := make(map[string]int)
	for _, blk := range fn.Blocks {
		for name, n := range blockCallTargets(blk) {
			out[name] += n
		}
	}
	return out
}

// blockCallTargets counts the calls a single block makes, keyed by callee name.
func blockCallTargets(blk *ir.Block) map[string]int {
	out := make(map[string]int)
	for _, inst := range blk.Insts {
		call, ok := inst.(*ir.InstCall)
		if !ok {
			continue
		}
		if callee, ok := call.Callee.(*ir.Func); ok {
			out[callee.Name()]++
		}
	}
	return out
}

// blockByName finds a block by its label, or nil.
func blockByName(fn *ir.Func, name string) *ir.Block {
	for _, blk := range fn.Blocks {
		if blk.LocalName == name {
			return blk
		}
	}
	return nil
}

// TestWindowsPALTLSKeyReplacementDoesNotDeleteTheStoreEntry guards a fix whose
// correctness argument is entirely non-local, so nothing about the
// .drop_old_key block looks wrong on its own (T1625).
//
// __pal_tls_key_name derives the CNG key name from the context address, so a
// second pal_tls_ctx_use_key on the same context reuses the *same* name, and
// NCRYPT_OVERWRITE_KEY_FLAG makes the import replace that store entry in place.
// The superseded handle therefore names the entry the new import just wrote:
// deleting through it destroys the key that was only just installed, leaving the
// certificate's CRYPT_KEY_PROV_INFO dangling and every later handshake failing.
// The replacement path must release the handle (NCryptFreeObject) and leave the
// single NCryptDeleteKey to pal_tls_ctx_free.
func TestWindowsPALTLSKeyReplacementDoesNotDeleteTheStoreEntry(t *testing.T) {
	fns := (&WindowsPAL{}).EmitTLS(ir.NewModule())
	fn := fns["pal_tls_ctx_use_key"]
	if fn == nil {
		t.Fatal("pal_tls_ctx_use_key is not defined")
	}

	blk := blockByName(fn, ".drop_old_key")
	if blk == nil {
		t.Fatal("pal_tls_ctx_use_key has no .drop_old_key block — if the replacement " +
			"path moved, re-check that it does not delete the newly imported key")
	}
	calls := blockCallTargets(blk)
	if calls["NCryptDeleteKey"] > 0 {
		t.Error("the key-replacement path calls @NCryptDeleteKey on the superseded handle, " +
			"which names the same store entry the overwriting import just rewrote — this " +
			"deletes the key that was just imported and breaks every later handshake")
	}
	if calls["NCryptFreeObject"] == 0 {
		t.Error("the key-replacement path must release the superseded handle with " +
			"@NCryptFreeObject, or it leaks")
	}

	// The store entry still has to be deleted exactly once, at context teardown.
	free := fns["pal_tls_ctx_free"]
	if free == nil || callTargets(free)["NCryptDeleteKey"] == 0 {
		t.Error("pal_tls_ctx_free must call @NCryptDeleteKey — otherwise the imported key " +
			"outlives the TlsConfig in the user's CNG key store")
	}
}
