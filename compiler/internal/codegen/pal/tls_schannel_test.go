package pal

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
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
				"DeleteCriticalSection",      // the credential-acquisition lock
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

// TestWindowsPALTLSCredentialAcquisitionIsSerialized pins the fix for T1766.
//
// One TlsConfig / TlsServerConfig context is shared by every connection
// goroutine (http.Server.bind_tls hands the same config to each), so the lazy
// AcquireCredentialsHandle in __pal_tls_ensure_cred is a shared check-then-act.
// Unsynchronized, two goroutines both see cred_valid clear, both acquire into
// the same &ctx->cred, and the loser's credential leaks while an in-flight
// handshake is handed a CredHandle its security context was not created from.
func TestWindowsPALTLSCredentialAcquisitionIsSerialized(t *testing.T) {
	module := ir.NewModule()
	(&WindowsPAL{}).EmitTLS(module)
	fns := funcsByName(module)

	ensure := fns["__pal_tls_ensure_cred"]
	if ensure == nil {
		t.Fatal("__pal_tls_ensure_cred is not defined")
	}
	if callTargets(ensure)["EnterCriticalSection"] == 0 {
		t.Fatal("__pal_tls_ensure_cred does not lock — concurrent pal_tls_new on one " +
			"shared config both acquire into &ctx->cred (T1766)")
	}

	// Lock-then-check, not check-then-lock. The entry block reads cred_valid and
	// branches on it, so the lock must be taken in that block *and* ahead of the
	// read — a double-checked fast path would put the read back outside the
	// section and reopen the race it is there to close.
	entry := ensure.Blocks[0]
	entered, readEarly := false, false
	for _, inst := range entry.Insts {
		if _, ok := inst.(*ir.InstLoad); ok && !entered {
			readEarly = true
			break
		}
		call, ok := inst.(*ir.InstCall)
		if !ok {
			continue
		}
		if callee, ok := call.Callee.(*ir.Func); ok && callee.Name() == "EnterCriticalSection" {
			entered = true
		}
	}
	switch {
	case readEarly:
		t.Error("__pal_tls_ensure_cred reads cred_valid before entering the critical " +
			"section — the check is unsynchronized and the race is still open")
	case !entered:
		t.Error("__pal_tls_ensure_cred enters the critical section outside its entry " +
			"block — the cred_valid check must already be under the lock")
	}

	// Every returning block must leave the section: an early bail-out added
	// later that skips the unlock deadlocks every subsequent handshake.
	for _, blk := range ensure.Blocks {
		if _, ok := blk.Term.(*ir.TermRet); !ok {
			continue
		}
		if blockCallTargets(blk)["LeaveCriticalSection"] == 0 {
			t.Errorf("__pal_tls_ensure_cred block %%%s returns without calling "+
				"@LeaveCriticalSection — the context stays locked and every later "+
				"handshake on this config deadlocks", blk.LocalName)
		}
	}

	// The lock has to exist and be destroyed with the context it lives in.
	for _, name := range []string{"pal_tls_ctx_new_client", "pal_tls_ctx_new_server"} {
		fn := fns[name]
		if fn == nil {
			t.Errorf("%s is not defined", name)
			continue
		}
		if callTargets(fn)["InitializeCriticalSection"] == 0 {
			t.Errorf("@%s never calls @InitializeCriticalSection — __pal_tls_ensure_cred "+
				"would enter an uninitialized CRITICAL_SECTION", name)
		}
	}

	// The lock lives *inside* the context allocation, so it has to be deleted
	// before that allocation goes back to the allocator.
	free := fns["pal_tls_ctx_free"]
	if free == nil {
		t.Fatal("pal_tls_ctx_free is not defined")
	}
	deleted := false
	for _, blk := range free.Blocks {
		if blockCallTargets(blk)["DeleteCriticalSection"] == 0 {
			continue
		}
		deleted = true
		seenDelete := false
		for _, inst := range blk.Insts {
			call, ok := inst.(*ir.InstCall)
			if !ok {
				continue
			}
			callee, ok := call.Callee.(*ir.Func)
			if !ok {
				continue
			}
			switch callee.Name() {
			case "DeleteCriticalSection":
				seenDelete = true
			case "pal_free":
				if !seenDelete {
					t.Errorf("pal_tls_ctx_free block %%%s frees the context before "+
						"@DeleteCriticalSection — the lock lives inside that allocation",
						blk.LocalName)
				}
			}
		}
	}
	if !deleted {
		t.Error("pal_tls_ctx_free never deletes the credential lock")
	}
}

// TestWindowsPALTLSCredLockMatchesTheCriticalSectionABI pins the shape of the
// lock T1766 added, which is the half the call-graph tests cannot see: the
// CRITICAL_SECTION is not a separate allocation, it is a field carved out of the
// context struct. That is only sound if the field is as wide as the OS object
// the kernel writes into and if the context allocation actually covers it — a
// too-small field makes InitializeCriticalSection scribble past the end of the
// heap block, and a size taken from anything but the struct type would leave the
// lock outside it entirely.
func TestWindowsPALTLSCredLockMatchesTheCriticalSectionABI(t *testing.T) {
	// The authoritative size is the one pal_mutex_init hands to pal_alloc for a
	// standalone mutex; read it back out of that emitter rather than repeating
	// the literal here, so the two cannot drift apart.
	mutexModule := newModuleWithAlloc(&WindowsPAL{})
	(&WindowsPAL{}).EmitMutexInit(mutexModule)
	want := palAllocConstSize(t, funcsByName(mutexModule)["pal_mutex_init"])

	ctx := newTLSWinTypes().ctx
	if got := len(ctx.Fields); got != winCtxFCredLock+1 {
		t.Fatalf("the context struct has %d fields but the credential lock is index %d — "+
			"the lock must stay the last field so every index above it keeps its meaning",
			got, winCtxFCredLock)
	}
	lock, ok := ctx.Fields[winCtxFCredLock].(*irtypes.ArrayType)
	if !ok {
		t.Fatalf("the credential-lock field is %v, not an i64 array — CRITICAL_SECTION "+
			"needs pointer alignment and a fixed byte width", ctx.Fields[winCtxFCredLock])
	}
	if !lock.ElemType.Equal(irtypes.I64) {
		t.Errorf("the credential lock is an array of %v; i64 elements are what give it "+
			"the 8-byte alignment InitializeCriticalSection requires", lock.ElemType)
	}
	if got := int64(lock.Len) * 8; got < want {
		t.Errorf("the credential-lock field is %d bytes but a CRITICAL_SECTION is %d "+
			"(pal_mutex_init allocates that much) — InitializeCriticalSection would "+
			"write past the end of the context allocation", got, want)
	}

	// And the allocation has to be sized from that very struct, not from a
	// hand-written constant that predates the field.
	sizeOfCtx := tlsWinSizeOf(ctx).String()
	for _, name := range []string{"pal_tls_ctx_new_client", "pal_tls_ctx_new_server"} {
		fn := funcsByName(mustEmitTLSModule())[name]
		if fn == nil {
			t.Errorf("%s is not defined", name)
			continue
		}
		if got := palAllocArg(t, fn); got != sizeOfCtx {
			t.Errorf("@%s allocates %s, not sizeof(the context struct) — the credential "+
				"lock lives inside that block and must be covered by it", name, got)
		}
	}
}

// TestWindowsPALTLSCredLockIsInitializedAfterZeroing pins the one ordering
// constraint in pal_tls_ctx_new_*: the zero-fill of the fresh context must
// happen *before* InitializeCriticalSection, never after. A CRITICAL_SECTION is
// live OS state once initialized (owner thread, recursion count, debug-info
// pointer), so a memset that follows quietly reverts it to an uninitialized
// object — and an uninitialized section neither serializes anything nor faults,
// so the T1766 race would come back with the lock still visibly in place.
func TestWindowsPALTLSCredLockIsInitializedAfterZeroing(t *testing.T) {
	fns := funcsByName(mustEmitTLSModule())
	for _, name := range []string{"pal_tls_ctx_new_client", "pal_tls_ctx_new_server"} {
		fn := fns[name]
		if fn == nil {
			t.Errorf("%s is not defined", name)
			continue
		}
		var order []string
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
				switch callee.Name() {
				case "memset", "InitializeCriticalSection":
					order = append(order, callee.Name())
				}
			}
		}
		zeroed := false
		initialized := false
		for _, step := range order {
			switch step {
			case "memset":
				if initialized {
					t.Errorf("@%s zero-fills the context after @InitializeCriticalSection — "+
						"that wipes the initialized CRITICAL_SECTION back to raw zeroes and "+
						"silently un-serializes __pal_tls_ensure_cred", name)
				}
				zeroed = true
			case "InitializeCriticalSection":
				if !zeroed {
					t.Errorf("@%s initializes the credential lock before zeroing the "+
						"context", name)
				}
				initialized = true
			}
		}
		if !initialized {
			t.Errorf("@%s never initializes the credential lock", name)
		}
	}
}

// TestWindowsPALTLSCredLockIsTheOnlySchannelLock keeps the lock T1766 introduced
// to one site and off the per-record path.
//
// Two properties matter and neither is local to __pal_tls_ensure_cred. First,
// one lock cannot deadlock against itself, but a second SChannel lock acquired
// in a different order could — so the credential lock stays the only one this
// backend takes. Second, the lock is affordable precisely because it is reached
// once per handshake (and once per renegotiation, which is a handshake): if
// pal_tls_read or pal_tls_write ever took it directly, every 16 KB record on
// every connection would serialize on the one config the whole server shares.
func TestWindowsPALTLSCredLockIsTheOnlySchannelLock(t *testing.T) {
	fns := funcsByName(mustEmitTLSModule())

	for name, fn := range fns {
		if name == "__pal_tls_ensure_cred" {
			continue
		}
		calls := callTargets(fn)
		if calls["EnterCriticalSection"] > 0 || calls["LeaveCriticalSection"] > 0 {
			t.Errorf("@%s takes a CRITICAL_SECTION directly — the credential lock is the "+
				"only lock this backend holds, and a second one invites a lock-order "+
				"deadlock against it", name)
		}
	}

	// The direct callers are the whole reason the lock is cheap. Renegotiation
	// reaches it through __pal_tls_hs_step, which is a handshake round; the
	// steady-state EncryptMessage/DecryptMessage path must not.
	var callers []string
	for name, fn := range fns {
		if callTargets(fn)["__pal_tls_ensure_cred"] > 0 {
			callers = append(callers, name)
		}
	}
	slices.Sort(callers)
	want := []string{"__pal_tls_hs_step", "pal_tls_new"}
	if !slices.Equal(callers, want) {
		t.Errorf("__pal_tls_ensure_cred is called from %v, want %v — a new caller on the "+
			"record path would put an EnterCriticalSection on every read and write of "+
			"the config every connection shares", callers, want)
	}
}

// TestWindowsPALTLSEnsureCredCallsNothingBlockingUnderTheLock guards the rule the
// backend header states and nothing else can enforce: a CRITICAL_SECTION is
// owned by the OS *thread* that entered it, while the caller is a Promise
// goroutine that may resume on a different M. Anything that suspends the
// goroutine between Enter and Leave therefore returns to a thread that does not
// own the section, so the release is undefined and every later handshake on that
// config blocks forever. The section is short and straight-line today; this
// allowlist makes adding a call to it a deliberate act.
func TestWindowsPALTLSEnsureCredCallsNothingBlockingUnderTheLock(t *testing.T) {
	fn := funcsByName(mustEmitTLSModule())["__pal_tls_ensure_cred"]
	if fn == nil {
		t.Fatal("__pal_tls_ensure_cred is not defined")
	}

	// Every one of these returns on the calling thread without parking it:
	// memset is libc, AcquireCredentialsHandleA is a synchronous SSPI call.
	allowed := map[string]bool{
		"EnterCriticalSection":      true,
		"LeaveCriticalSection":      true,
		"AcquireCredentialsHandleA": true,
		"memset":                    true,
	}
	for callee := range callTargets(fn) {
		if !allowed[callee] {
			t.Errorf("__pal_tls_ensure_cred calls @%s while holding the credential lock — "+
				"if that can suspend the goroutine it may resume on another M, which then "+
				"cannot release a CRITICAL_SECTION it does not own (T1766). Add it to the "+
				"allowlist only once it is known not to block", callee)
		}
	}

	// One acquisition, one release per exit: a second Enter would need a second
	// Leave to balance, and a Leave on a path that keeps running releases the
	// section while the acquisition it guards is still in flight.
	if got := callTargets(fn)["EnterCriticalSection"]; got != 1 {
		t.Errorf("__pal_tls_ensure_cred enters the credential lock %d times, want exactly 1", got)
	}
	for _, blk := range fn.Blocks {
		leaves := blockCallTargets(blk)["LeaveCriticalSection"]
		_, returns := blk.Term.(*ir.TermRet)
		switch {
		case returns && leaves != 1:
			t.Errorf("block %%%s returns with %d @LeaveCriticalSection calls, want exactly 1 "+
				"— 0 wedges every later handshake, 2 releases a section this thread no "+
				"longer owns", blk.LocalName, leaves)
		case !returns && leaves != 0:
			t.Errorf("block %%%s releases the credential lock but falls through to more "+
				"work — the acquisition it guards would run unsynchronized", blk.LocalName)
		}
	}
}

// TestWindowsPALTLSSharesCriticalSectionDeclarationsWithPalMutex covers the
// assumption the T1766 comment records but no single emitter can check: the TLS
// backend and the mutex PAL both declare the same four kernel32 entry points,
// and whichever emitter runs first wins. Two declarations of one symbol with
// different signatures is a malformed module, so the sharing has to hold in
// either emission order.
func TestWindowsPALTLSSharesCriticalSectionDeclarationsWithPalMutex(t *testing.T) {
	p := &WindowsPAL{}
	emitMutexes := func(m *ir.Module) {
		p.EmitMutexInit(m)
		p.EmitMutexLock(m)
		p.EmitMutexUnlock(m)
		p.EmitMutexDestroy(m)
	}

	for _, tc := range []struct {
		name  string
		build func() *ir.Module
	}{
		{"mutex first", func() *ir.Module {
			m := newModuleWithAlloc(p)
			emitMutexes(m)
			p.EmitTLS(m)
			return m
		}},
		{"TLS first", func() *ir.Module {
			m := newModuleWithAlloc(p)
			p.EmitTLS(m)
			emitMutexes(m)
			return m
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.build().String()
			for _, sym := range []string{
				"InitializeCriticalSection", "EnterCriticalSection",
				"LeaveCriticalSection", "DeleteCriticalSection",
			} {
				decl := "declare void @" + sym + "(i8* %lpCriticalSection)"
				if got := strings.Count(out, "declare void @"+sym+"("); got != 1 {
					t.Errorf("@%s is declared %d times, want 1 — the TLS backend and the "+
						"mutex PAL must share one declaration or the module is malformed", sym, got)
				}
				if !strings.Contains(out, decl) {
					t.Errorf("no %q in the module — the two declaration sites disagree on "+
						"the signature", decl)
				}
			}
		})
	}
}

// mustEmitTLSModule emits the SChannel backend into a fresh module and returns
// the module, for tests that need the internal __pal_tls_* helpers EmitTLS does
// not return.
func mustEmitTLSModule() *ir.Module {
	m := ir.NewModule()
	(&WindowsPAL{}).EmitTLS(m)
	return m
}

// palAllocArg returns the textual size argument of the first @pal_alloc call in
// fn, which for the context constructors is a sizeof-the-struct expression.
func palAllocArg(t *testing.T, fn *ir.Func) string {
	t.Helper()
	for _, blk := range fn.Blocks {
		for _, inst := range blk.Insts {
			call, ok := inst.(*ir.InstCall)
			if !ok {
				continue
			}
			if callee, ok := call.Callee.(*ir.Func); ok && callee.Name() == "pal_alloc" {
				return call.Args[0].String()
			}
		}
	}
	t.Fatalf("@%s never calls @pal_alloc", fn.Name())
	return ""
}

// palAllocConstSize returns the constant byte count fn passes to @pal_alloc.
func palAllocConstSize(t *testing.T, fn *ir.Func) int64 {
	t.Helper()
	if fn == nil {
		t.Fatal("function not defined")
	}
	for _, blk := range fn.Blocks {
		for _, inst := range blk.Insts {
			call, ok := inst.(*ir.InstCall)
			if !ok {
				continue
			}
			callee, ok := call.Callee.(*ir.Func)
			if !ok || callee.Name() != "pal_alloc" {
				continue
			}
			n, ok := call.Args[0].(*constant.Int)
			if !ok {
				t.Fatalf("@%s allocates a non-constant size %v", fn.Name(), call.Args[0])
			}
			return n.X.Int64()
		}
	}
	t.Fatalf("@%s never calls @pal_alloc", fn.Name())
	return 0
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

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "modules", "tls", "tls.pr"))
	if err != nil {
		t.Fatalf("read modules/tls/tls.pr: %v", err)
	}
	src := normalizeEOL(raw)
	// Every literal tls.pr compares the backend's version string against.
	lits := regexp.MustCompile(`_tls_get_version\([^)]*\)\s*==\s*"([^"]*)"`).
		FindAllStringSubmatch(src, -1)
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

// normalizeEOL reads a repo source file as text with LF line endings. The repo
// stores .pr sources with LF, but a Windows checkout with core.autocrlf=true —
// Git for Windows' default, and what this machine uses — materializes them as
// CRLF. The contracts below are pinned with patterns that anchor on \n, so
// without this a perfectly correct source reads as a missing declaration and
// the test fails for the checkout style rather than for the thing it guards.
func normalizeEOL(b []byte) string {
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// TestWindowsPALTLSCredentialOutlivesThePooledSessions pins the lifetime half of
// the SChannel context contract — the part T1766 left open under "teardown
// ordering", and the hazard TlsConfig's own `doc now warns about (T1780).
//
// SChannel requires a credential to outlive every security context derived from
// it. pal_tls_ctx_free calls FreeCredentialsHandle, so a session still holding a
// CtxtHandle built from that credential is afterwards driving freed OS state.
// http.Client is the one type that owns both ends — a tls.TlsConfig and a pool
// of TLS sessions created from it — and for a client that is simply let go
// rather than closed, the only thing ordering their teardown is that
// _tls_config is declared *before* _pool: fields drop in reverse declaration
// order (docs/language-design.md §16.3), so the sessions go first.
//
// No test on a CI platform can catch the wrong order. OpenSSL reference-counts
// the SSL_CTX and Secure Transport retains its identity, so on Linux and macOS
// both orders work and the leak detector still reads zero; only Windows faults.
// Reading the source is therefore the only guard, exactly as the version-string
// contract above is.
func TestWindowsPALTLSCredentialOutlivesThePooledSessions(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "modules", "http", "http.pr"))
	if err != nil {
		t.Fatalf("read modules/http/http.pr: %v", err)
	}
	src := normalizeEOL(raw)
	body := regexp.MustCompile(`(?s)\ntype Client [^\n]*\{(.*?)\n\}\n`).FindStringSubmatch(src)
	if body == nil {
		t.Fatal("modules/http/http.pr no longer declares `type Client` — the client that " +
			"owns both a TlsConfig and the sessions made from it moved, and this " +
			"ordering contract needs to move with it")
	}

	cfg := regexp.MustCompile(`(?m)^\s*tls\.TlsConfig\[\]\s+_tls_config\s*;`).FindStringIndex(body[1])
	pool := regexp.MustCompile(`(?m)^\s*Pool\s+_pool\s*;`).FindStringIndex(body[1])
	switch {
	case cfg == nil:
		t.Fatal("http.Client no longer declares a `tls.TlsConfig[] _tls_config` field — " +
			"if the TLS configuration is held some other way, re-derive which end of " +
			"the pair drops first")
	case pool == nil:
		t.Fatal("http.Client no longer declares a `Pool _pool` field — if the pooled TLS " +
			"sessions are held some other way, re-derive which end of the pair drops first")
	case cfg[0] > pool[0]:
		t.Error("http.Client declares _pool before _tls_config, so on drop the TLS " +
			"configuration is torn down first and the pooled sessions created from it " +
			"outlive their credential — on Windows that is a use-after-free inside " +
			"SChannel (T1766/T1780). Fields drop in reverse declaration order, so " +
			"_tls_config must come first")
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
