package misc2

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// netModuleSource loads the real modules/net/net.pr so these tests assert
// against the shipped module rather than a hand-written stand-in that could
// drift away from it.
func netModuleSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("../../../../../modules/net/net.pr")
	if err != nil {
		t.Fatalf("reading modules/net/net.pr: %v", err)
	}
	return string(src)
}

// generateNetIR compiles userSrc against the real net catalog module.
func generateNetIR(t *testing.T, userSrc string) string {
	t.Helper()
	return codegentest.GenerateIRWithCatalogModule(t, "net", netModuleSource(t), userSrc)
}

// T1518: name resolution blocks the calling OS thread — musl's resolver waits
// up to ~10s on a dead nameserver. Without the scheduler's P-handoff, that M
// would sit in getaddrinfo holding its P, stalling every runnable goroutine
// queued on it. This asserts the bridge wraps pal_resolve_host in
// enter_syscall/exit_syscall, which is the entire reason DNS is allowed to run
// off the reactor at all.
func TestNetResolveHandsOffP(t *testing.T) {
	ir := generateNetIR(t, `
		use net;
		main() {
			net.resolve("localhost")? e { };
		}
	`)

	body := codegentest.DefBody(t, ir, "define void @promise_net_resolve(")
	handoff := regexp.MustCompile(
		`(?s)call void @promise_sched_enter_syscall\(\).*?call i32 @pal_resolve_host\(.*?call void @promise_sched_exit_syscall\(\)`)
	if !handoff.MatchString(body) {
		t.Errorf("promise_net_resolve must call pal_resolve_host between enter_syscall and exit_syscall; got:\n%s", body)
	}
	// The handoff must bracket the call, not trail it: an exit_syscall emitted
	// before the resolver call would release nothing.
	enter := strings.Index(body, "@promise_sched_enter_syscall")
	call := strings.Index(body, "@pal_resolve_host(")
	exit := strings.Index(body, "@promise_sched_exit_syscall")
	if !(enter >= 0 && enter < call && call < exit) {
		t.Errorf("expected enter_syscall < pal_resolve_host < exit_syscall, got %d/%d/%d", enter, call, exit)
	}
}

// The connect bridge carries the same handoff. connect() on the freshly
// non-blocking socket normally returns at once with EINPROGRESS, so this is
// defensive rather than hot: _net_socket_set_nonblock's result is not checked,
// and a socket that stayed blocking would otherwise pin its M through a full
// TCP handshake. In the common path enter_syscall no-ops anyway, because
// promise_net_resolve already handed this goroutine's P away.
func TestNetSocketConnectResolvedHandsOffP(t *testing.T) {
	ir := generateNetIR(t, `
		use net;
		main() {
			net.TcpStream.connect("localhost", 80)? e { };
		}
	`)

	body := codegentest.DefBody(t, ir, "define void @promise_net_socket_connect_resolved(")
	codegentest.AssertContains(t, body, "@promise_sched_enter_syscall")
	codegentest.AssertContains(t, body, "@pal_socket_connect_resolved(")
	codegentest.AssertContains(t, body, "@promise_sched_exit_syscall")
}

// The addrinfo list is libc-malloc'd, so a missed free is invisible to both the
// allocation-count leak detector and the `memory_limit` accounting (which counts
// only Promise allocations). _AddrList exists solely so the scope-cleanup stack
// releases it on every exit path — including a `raise` and the early `return`s
// inside connect's per-address loop — and this is the only automated check that
// the drop actually frees anything.
func TestNetAddrListDropFreesResolveList(t *testing.T) {
	ir := generateNetIR(t, `
		use net;
		main() {
			net.resolve("localhost")? e { };
		}
	`)

	body := codegentest.DefBody(t, ir, "define void @__mod_net__AddrList.drop(")
	codegentest.AssertContains(t, body, "@promise_net_resolve_free(")

	// The bridge that drop reaches has to call through to the PAL free.
	freeBody := codegentest.DefBody(t, ir, "define void @promise_net_resolve_free(")
	codegentest.AssertContains(t, freeBody, "@pal_resolve_free(")
}

// resolve() and connect() both construct an _AddrList immediately after a
// successful lookup, so the list is registered for cleanup before any path that
// can raise or return. A regression that dropped the binding would show up as a
// missing drop call in the caller.
func TestNetResolveRegistersListCleanup(t *testing.T) {
	ir := generateNetIR(t, `
		use net;
		main() {
			net.resolve("localhost")? e { };
			net.TcpStream.connect("localhost", 80)? e { };
		}
	`)

	for _, fn := range []string{
		"define { i1, i8*, i8* } @__mod_net_resolve(",
		"define { i1, { i8*, i8* }, i8* } @__mod_net_TcpStream.connect(",
	} {
		body := codegentest.DefBody(t, ir, fn)
		if !strings.Contains(body, "@__mod_net__AddrList.drop(") {
			t.Errorf("%s must drop its _AddrList so the resolver list is freed on every exit path", fn)
		}
	}
}

// AF_UNSPEC resolution can hand back an IPv6 address first, so connect must
// create the socket with the family the resolver reported rather than the
// hard-coded AF_INET the pre-T1518 code used.
func TestNetConnectUsesResolvedFamily(t *testing.T) {
	ir := generateNetIR(t, `
		use net;
		main() {
			net.TcpStream.connect("localhost", 80)? e { };
		}
	`)

	body := codegentest.DefBody(t, ir, "define { i1, { i8*, i8* }, i8* } @__mod_net_TcpStream.connect(")
	codegentest.AssertContains(t, body, "@promise_net_resolve_family(")
	codegentest.AssertContains(t, body, "@promise_net_resolve_next(")
	if strings.Contains(body, "@__mod_net__af_inet(") {
		t.Error("connect must not hard-code AF_INET now that it resolves both families")
	}
}

// The bridge converts both the host and the service into C strings with
// palAlloc before calling the resolver, and both must be freed. They are freed
// in the entry block, ahead of the success/failure branch, so a single lookup
// cannot leak on one path and not the other. This is the ordering that matters:
// frees placed in the success block only would leak two allocations per *failed*
// lookup, and a caller retrying a dead name in a loop is exactly the workload
// that produces failures repeatedly.
func TestNetResolveFreesBothCStringsBeforeBranching(t *testing.T) {
	ir := generateNetIR(t, `
		use net;
		main() {
			net.resolve("localhost")? e { };
		}
	`)

	body := codegentest.DefBody(t, ir, "define void @promise_net_resolve(")
	entry := body
	if br := strings.Index(entry, "\n.ok:"); br >= 0 {
		entry = entry[:br]
	}
	if got := strings.Count(entry, "@pal_free("); got != 2 {
		t.Errorf("promise_net_resolve must free both the host and service C strings before branching; got %d pal_free calls in the entry block:\n%s", got, entry)
	}
}

// The failure code has to survive the trip back to Promise as a *negative* int.
// pal_resolve_host returns i32, Promise ints are i64, and the normalized codes
// are all negative — a zero-extend instead of a sign-extend would turn -1 into
// 4294967295, which is > 0, so `if head < 0` in resolve() would sail straight
// past the error and treat the code as an addrinfo pointer.
func TestNetResolveSignExtendsFailureCode(t *testing.T) {
	ir := generateNetIR(t, `
		use net;
		main() {
			net.resolve("localhost")? e { };
		}
	`)

	body := codegentest.DefBody(t, ir, "define void @promise_net_resolve(")
	errBlk := body[strings.Index(body, "\n.err:"):]
	if strings.Contains(errBlk, "zext i32") {
		t.Errorf("the resolver code must be sign-extended, not zero-extended — a zext makes every negative code look like a valid pointer:\n%s", errBlk)
	}
	codegentest.AssertContains(t, errBlk, "sext i32")

	// The success path hands back the list pointer itself, not a status.
	okBlk := body[strings.Index(body, "\n.ok:"):]
	codegentest.AssertContains(t, okBlk, "ptrtoint")
}

// pal_resolve_address_text returns -1 for a family it cannot render. The bridge
// must turn that into an empty string rather than reading the untouched stack
// buffer: resolve() skips zero-length entries, so an unrenderable address is
// dropped from the result instead of appearing as a line of stack garbage.
func TestNetResolveAddressReturnsEmptyOnFailure(t *testing.T) {
	ir := generateNetIR(t, `
		use net;
		main() {
			net.resolve("localhost")? e { };
		}
	`)

	body := codegentest.DefBody(t, ir, "define void @promise_net_resolve_address(")
	errBlk := body[strings.Index(body, "\n.err:"):]
	codegentest.AssertContains(t, errBlk, "@promise_string_new(i8* null, i64 0)")

	// And the success path must use the length inet_ntop reported, not the
	// buffer size — a fixed length would append 60-odd NUL bytes to every address.
	okBlk := body[strings.Index(body, "\n.ok:"):strings.Index(body, "\n.err:")]
	if strings.Contains(okBlk, "i64 64") {
		t.Errorf("the string must be built from the rendered length, not the buffer size:\n%s", okBlk)
	}
	codegentest.AssertContains(t, okBlk, "sext i32")
}

// connect() walks the resolved list, and every failing address must advance to
// the next one. On a v4-only host "localhost" resolves to a single address, so
// no runtime test on such a machine can observe address N failing and address
// N+1 succeeding — but the same code must not stall on a dual-stack host either.
// A dropped advance turns a refused first address into an infinite loop rather
// than a fall-through, which is why this is pinned structurally. The loop has
// two places it moves on: the early `continue` when the socket cannot be
// created for the resolved family, and the fall-off at the end of the body that
// every non-returning connect failure reaches.
func TestNetConnectAdvancesPastEveryFailedAddress(t *testing.T) {
	ir := generateNetIR(t, `
		use net;
		main() {
			net.TcpStream.connect("localhost", 80)? e { };
		}
	`)

	body := codegentest.DefBody(t, ir, "define { i1, { i8*, i8* }, i8* } @__mod_net_TcpStream.connect(")
	if got := strings.Count(body, "@promise_net_resolve_next("); got < 2 {
		t.Errorf("connect must advance to the next address on both of its continue paths; got %d calls to promise_net_resolve_next:\n%s", got, body)
	}
	// A failed address must close its socket before moving on, or a name with
	// several unreachable addresses leaks an fd per attempt.
	codegentest.AssertContains(t, body, "@promise_net_socket_close(")
}
