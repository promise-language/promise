package pal

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/llir/llvm/ir"
)

// resolveHostCompareConstants returns the set of integer constants that
// @pal_resolve_host compares the raw getaddrinfo return value against — i.e. the
// platform's EAI_* table, isolated from the normalized codes it returns.
func resolveHostCompareConstants(t *testing.T, moduleText string) []int {
	t.Helper()
	start := strings.Index(moduleText, "define i32 @pal_resolve_host(")
	if start < 0 {
		t.Fatal("pal_resolve_host not defined")
	}
	body := moduleText[start:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end]
	}
	re := regexp.MustCompile(`icmp eq i32 %\d+, (-?\d+)`)
	var got []int
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		var v int
		if _, err := fmt.Sscanf(m[1], "%d", &v); err != nil {
			t.Fatalf("unparsable constant %q", m[1])
		}
		if v == 0 {
			continue // the success test, not part of the EAI table
		}
		got = append(got, v)
	}
	sort.Ints(got)
	return got
}

// linuxPAL / darwinPAL build PALs whose target selects the platform-specific
// addrinfo layout and EAI_* table.
func linuxPAL() *PosixPAL  { return &PosixPAL{target: "x86_64-unknown-linux-gnu"} }
func darwinPAL() *PosixPAL { return &PosixPAL{target: "arm64-apple-darwin24.3.0"} }

// TestAddrinfoOffsets pins the `struct addrinfo` field offsets per platform.
// ai_canonname and ai_addr are swapped on BSD/macOS and Windows relative to
// Linux, so reading ai_addr at Linux's offset on macOS would hand connect() a
// pointer to the canonical-name string. This is the single highest-risk detail
// in T1518 and the one thing a Linux host cannot validate at runtime.
func TestAddrinfoOffsets(t *testing.T) {
	tests := []struct {
		name   string
		layout addrinfoLayout
		want   addrinfoLayout
	}{
		{"Linux", linuxPAL().posixAddrinfoLayout(), addrinfoLayout{
			familyOffset: 4, addrLenOffset: 16, addrLenIs64: false,
			addrOffset: 24, nextOffset: 40, afInet6: 10,
		}},
		{"Darwin", darwinPAL().posixAddrinfoLayout(), addrinfoLayout{
			familyOffset: 4, addrLenOffset: 16, addrLenIs64: false,
			addrOffset: 32, nextOffset: 40, afInet6: 30,
		}},
		{"Windows", windowsAddrinfoLayout(), addrinfoLayout{
			familyOffset: 4, addrLenOffset: 16, addrLenIs64: true,
			addrOffset: 32, nextOffset: 40, afInet6: 23,
		}},
	}
	for _, tt := range tests {
		if tt.layout != tt.want {
			t.Errorf("%s addrinfo layout = %+v, want %+v", tt.name, tt.layout, tt.want)
		}
	}
}

// TestResolveHostPosix checks the hints block and the normalized-code mapping.
// Linux EAI_* values are negative, macOS's are positive, and they carry
// different meanings at the same magnitude — so the two tables must not be
// confused with one another.
func TestResolveHostPosix(t *testing.T) {
	tests := []struct {
		name     string
		p        *PosixPAL
		wantEAIs []int
	}{
		// EAI_NODATA, EAI_AGAIN, EAI_NONAME
		{name: "Linux", p: linuxPAL(), wantEAIs: []int{-5, -3, -2}},
		// EAI_AGAIN, EAI_NODATA, EAI_NONAME
		{name: "Darwin", p: darwinPAL(), wantEAIs: []int{2, 7, 8}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			module := ir.NewModule()
			tt.p.EmitGetAddrInfo(module)
			fn := tt.p.EmitResolveHost(module)
			out := module.String()

			if fn.Name() != "pal_resolve_host" {
				t.Errorf("expected pal_resolve_host, got %s", fn.Name())
			}
			assertContains(t, out, "define i32 @pal_resolve_host(i8* %host, i8* %service, i8** %out)", "definition")
			// 48-byte zeroed hints with ai_socktype = SOCK_STREAM at offset 8;
			// ai_family stays AF_UNSPEC so both A and AAAA come back.
			assertContains(t, out, "alloca [48 x i8]", "addrinfo-sized hints block")
			assertContains(t, out, "@memset(i8* %1, i32 0, i64 48)", "hints zeroed")
			assertContains(t, out, "getelementptr i8, i8* %1, i64 8", "ai_socktype at offset 8")
			assertContains(t, out, "store i32 1, i32* %4", "ai_socktype = SOCK_STREAM")
			assertContains(t, out, "store i8* null, i8** %out", "out nulled before the call")
			// Delegates to pal_getaddrinfo rather than calling libc a second time.
			assertContains(t, out, "@pal_getaddrinfo(", "delegates to pal_getaddrinfo")
			// Callers see the normalized vocabulary, never a raw EAI_*.
			assertContains(t, out, "ret i32 -1", "normalized not-found")
			assertContains(t, out, "ret i32 -2", "normalized try-again")
			assertContains(t, out, "ret i32 -3", "normalized failed")

			got := resolveHostCompareConstants(t, out)
			if !reflect.DeepEqual(got, tt.wantEAIs) {
				t.Errorf("%s EAI table = %v, want %v", tt.name, got, tt.wantEAIs)
			}
		})
	}
}

func TestResolveHostWindows(t *testing.T) {
	module := ir.NewModule()
	p := &WindowsPAL{}
	p.EmitGetAddrInfo(module)
	p.EmitResolveHost(module)
	out := module.String()

	assertContains(t, out, "define i32 @pal_resolve_host(i8* %host, i8* %service, i8** %out)", "definition")
	assertContains(t, out, "@pal_getaddrinfo(", "delegates to pal_getaddrinfo")
	// Winsock startup happens inside pal_getaddrinfo, which this reaches through.
	assertContains(t, out, "@__pal_wsa_ensure_init()", "WSA init reachable via pal_getaddrinfo")

	// WSATRY_AGAIN, WSAHOST_NOT_FOUND, WSANO_DATA
	want := []int{11001, 11002, 11004}
	if got := resolveHostCompareConstants(t, out); !reflect.DeepEqual(got, want) {
		t.Errorf("Windows WSA table = %v, want %v", got, want)
	}
}

// The list walkers read ai_next / ai_family at the platform's own offsets.
func TestResolveWalkersUsePlatformOffsets(t *testing.T) {
	tests := []struct {
		name string
		emit func(*ir.Module) (nextFn, familyFn *ir.Func)
	}{
		{"Linux", func(m *ir.Module) (*ir.Func, *ir.Func) {
			p := linuxPAL()
			return p.EmitResolveNext(m), p.EmitResolveFamily(m)
		}},
		{"Darwin", func(m *ir.Module) (*ir.Func, *ir.Func) {
			p := darwinPAL()
			return p.EmitResolveNext(m), p.EmitResolveFamily(m)
		}},
		{"Windows", func(m *ir.Module) (*ir.Func, *ir.Func) {
			p := &WindowsPAL{}
			return p.EmitResolveNext(m), p.EmitResolveFamily(m)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			module := ir.NewModule()
			tt.emit(module)
			out := module.String()
			assertContains(t, out, "define i8* @pal_resolve_next(i8* %node)", "next definition")
			assertContains(t, out, "define i32 @pal_resolve_family(i8* %node)", "family definition")
			assertContains(t, out, "i64 40", "ai_next at offset 40")
			assertContains(t, out, "i64 4", "ai_family at offset 4")
		})
	}
}

// pal_resolve_address_text renders both families and must reach into the
// sockaddr at the right sub-offsets: sin_addr at 4, sin6_addr at 8.
func TestResolveAddressText(t *testing.T) {
	tests := []struct {
		name        string
		emit        func(*ir.Module) *ir.Func
		wantAF6     string
		wantSizeArg string
	}{
		{"Linux", func(m *ir.Module) *ir.Func { return linuxPAL().EmitResolveAddressText(m) }, "i32 10", "i32 %len"},
		{"Darwin", func(m *ir.Module) *ir.Func { return darwinPAL().EmitResolveAddressText(m) }, "i32 30", "i32 %len"},
		{"Windows", func(m *ir.Module) *ir.Func { return (&WindowsPAL{}).EmitResolveAddressText(m) }, "i32 23", "i64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			module := ir.NewModule()
			tt.emit(module)
			out := module.String()
			assertContains(t, out, "define i32 @pal_resolve_address_text(i8* %node, i8* %buf, i32 %len)", "definition")
			assertContains(t, out, "@inet_ntop(", "uses inet_ntop")
			assertContains(t, out, "@strlen(", "returns the rendered length")
			assertContains(t, out, "i32 2", "AF_INET branch")
			assertContains(t, out, tt.wantAF6, "platform AF_INET6")
			assertContains(t, out, "ret i32 -1", "unsupported family / failure")
		})
	}

	// Windows widens inet_ntop's buffer-size parameter to size_t.
	module := ir.NewModule()
	(&WindowsPAL{}).EmitResolveAddressText(module)
	assertContains(t, module.String(), "declare i8* @inet_ntop(i32 %af, i8* %src, i8* %dst, i64 %size)",
		"Windows inet_ntop takes a size_t buffer length")

	module = ir.NewModule()
	linuxPAL().EmitResolveAddressText(module)
	assertContains(t, module.String(), "declare i8* @inet_ntop(i32 %af, i8* %src, i8* %dst, i32 %size)",
		"POSIX inet_ntop takes a socklen_t buffer length")
}

// pal_resolve_free must delegate to pal_freeaddrinfo — the addrinfo list is
// owned by the resolver, so it has to be released by the resolver's own free.
func TestResolveFreeDelegates(t *testing.T) {
	for _, tt := range []struct {
		name string
		emit func(*ir.Module) *ir.Func
	}{
		{"Posix", func(m *ir.Module) *ir.Func { return linuxPAL().EmitResolveFree(m) }},
		{"Windows", func(m *ir.Module) *ir.Func { return (&WindowsPAL{}).EmitResolveFree(m) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			module := ir.NewModule()
			tt.emit(module)
			out := module.String()
			assertContains(t, out, "define void @pal_resolve_free(i8* %list)", "definition")
			assertContains(t, out, "@pal_freeaddrinfo(", "delegates to pal_freeaddrinfo")
			assertContains(t, out, "icmp eq i8* %list, null", "null-guarded")
		})
	}
}

// pal_socket_connect_resolved passes the resolver's own sockaddr straight to
// connect(), which is what makes AF_INET6 work with no extra code.
func TestSocketConnectResolved(t *testing.T) {
	t.Run("Posix", func(t *testing.T) {
		module := ir.NewModule()
		linuxPAL().EmitSocketConnectResolved(module)
		out := module.String()
		assertContains(t, out, "define i32 @pal_socket_connect_resolved(i32 %fd, i8* %node)", "definition")
		assertContains(t, out, "@connect(i32 %fd,", "calls connect with the fd")
		assertContains(t, out, "i64 24", "reads ai_addr at the Linux offset")
		assertContains(t, out, "i64 16", "reads ai_addrlen")
		assertContains(t, out, "@__errno_location()", "-errno on failure")
	})

	t.Run("Darwin", func(t *testing.T) {
		module := ir.NewModule()
		darwinPAL().EmitSocketConnectResolved(module)
		assertContains(t, module.String(), "i64 32", "reads ai_addr at the BSD offset")
	})

	t.Run("Windows", func(t *testing.T) {
		module := ir.NewModule()
		(&WindowsPAL{}).EmitSocketConnectResolved(module)
		out := module.String()
		assertContains(t, out, "define i32 @pal_socket_connect_resolved(i32 %fd, i8* %node)", "definition")
		assertContains(t, out, "i64 32", "reads ai_addr at the Windows offset")
		// ai_addrlen is size_t on Windows and must be narrowed for connect().
		assertContains(t, out, "trunc i64", "narrows size_t ai_addrlen")
		assertContains(t, out, "@WSAGetLastError()", "-WSAError on failure")
	})
}

// WASM has no platform resolver: pal_resolve_host must report the normalized
// "unsupported" code (-4) rather than pretending a lookup failed.
func TestResolveStubsWasm(t *testing.T) {
	for _, tt := range []struct {
		name string
		p    PAL
	}{
		{"Wasm", &WasmPAL{}},
		{"WasmWeb", &WasmWebPAL{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			module := ir.NewModule()
			tt.p.EmitResolveHost(module)
			tt.p.EmitResolveNext(module)
			tt.p.EmitResolveFamily(module)
			tt.p.EmitResolveAddressText(module)
			tt.p.EmitResolveFree(module)
			tt.p.EmitSocketConnectResolved(module)
			out := module.String()

			assertContains(t, out, "define i32 @pal_resolve_host(", "resolve_host stub")
			assertContains(t, out, "ret i32 -4", "reports unsupported, not a lookup failure")
			assertContains(t, out, "store i8* null, i8** %out", "leaves the out pointer null")
			assertContains(t, out, "define i8* @pal_resolve_next(", "resolve_next stub")
			assertContains(t, out, "define void @pal_resolve_free(", "resolve_free stub")
			assertContains(t, out, "define i32 @pal_socket_connect_resolved(", "connect_resolved stub")
		})
	}
}

// palFuncBody returns the text of a single function definition, so an assertion
// about one function cannot be satisfied by an identical-looking instruction
// somewhere else in the module.
func palFuncBody(t *testing.T, moduleText, fnDef string) string {
	t.Helper()
	start := strings.Index(moduleText, fnDef)
	if start < 0 {
		t.Fatalf("%s not defined", fnDef)
	}
	body := moduleText[start:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end]
	}
	return body
}

// palBlock returns the body of a single labeled basic block inside fn's
// definition. The sub-offsets pal_resolve_address_text applies live *inside*
// the .v4 / .v6 blocks, so a module-wide substring search cannot tell them
// apart from the many other small constants in the same module.
func palBlock(t *testing.T, moduleText, fnDef, label string) string {
	t.Helper()
	body := palFuncBody(t, moduleText, fnDef)
	blkStart := strings.Index(body, "\n"+label+":\n")
	if blkStart < 0 {
		t.Fatalf("block %s not found in %s", label, fnDef)
	}
	blk := body[blkStart+len(label)+3:]
	if end := strings.Index(blk, "\n\n"); end >= 0 {
		blk = blk[:end]
	}
	return blk
}

// TestResolveAddressTextSockaddrOffsets pins the two sub-offsets inside the
// sockaddr that pal_resolve_address_text hands to inet_ntop: sin_addr sits 4
// bytes into a sockaddr_in (sa_family 2 + sin_port 2) and sin6_addr sits 8
// bytes into a sockaddr_in6 (+ sin6_flowinfo 4). Getting either wrong does not
// crash — inet_ntop happily renders whatever bytes it is pointed at — so the
// symptom is a plausible-looking but wrong IP address, e.g. the port rendered
// as the first octet. TestResolveAddressText asserts the *families* are
// branched on; this asserts where each branch reads from.
//
// The Linux rendering is checked at runtime by modules/net/net_test.pr
// (test_resolve_ipv4_literal / test_resolve_ipv6_literal round-trip "127.0.0.1"
// and "::1"), but no Linux runner can execute the Darwin or Windows variants —
// and those are exactly the ones whose ai_addr offset differs, so a wrong
// ai_addr would feed the sub-offset arithmetic a pointer into ai_canonname.
func TestResolveAddressTextSockaddrOffsets(t *testing.T) {
	const def = "define i32 @pal_resolve_address_text(i8* %node, i8* %buf, i32 %len)"
	tests := []struct {
		name           string
		emit           func(*ir.Module) *ir.Func
		wantAddrOffset string
	}{
		{"Linux", func(m *ir.Module) *ir.Func { return linuxPAL().EmitResolveAddressText(m) }, "i64 24"},
		{"Darwin", func(m *ir.Module) *ir.Func { return darwinPAL().EmitResolveAddressText(m) }, "i64 32"},
		{"Windows", func(m *ir.Module) *ir.Func { return (&WindowsPAL{}).EmitResolveAddressText(m) }, "i64 32"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			module := ir.NewModule()
			tt.emit(module)
			out := module.String()

			// ai_addr is read from the platform's own offset, not Linux's.
			entry := palBlock(t, out, def, ".entry")
			assertContains(t, entry, "getelementptr i8, i8* %node, "+tt.wantAddrOffset,
				"ai_addr read at this platform's offset")
			assertContains(t, entry, "getelementptr i8, i8* %node, i64 4",
				"ai_family read at offset 4")

			// The GEPs below index the loaded ai_addr, never %node again — a
			// sub-offset applied to the addrinfo node instead of the sockaddr
			// would render bytes of ai_addrlen as an address.
			v4 := palBlock(t, out, def, ".v4")
			if strings.Contains(v4, "i8* %node") {
				t.Errorf(".v4 must index the loaded ai_addr, not the addrinfo node:\n%s", v4)
			}
			assertContains(t, v4, ", i64 4\n", "sin_addr at offset 4 within sockaddr_in")

			v6 := palBlock(t, out, def, ".v6")
			if strings.Contains(v6, "i8* %node") {
				t.Errorf(".v6 must index the loaded ai_addr, not the addrinfo node:\n%s", v6)
			}
			assertContains(t, v6, ", i64 8\n", "sin6_addr at offset 8 within sockaddr_in6")
		})
	}
}

// TestSocketConnectResolvedReadsAddrLen pins ai_addrlen handling per platform.
// POSIX's socklen_t is already i32, but Windows widens the field to size_t, so
// the Windows emitter must load i64 and narrow it. Loading i32 from a size_t
// field happens to work on little-endian x86 — and then silently breaks the
// moment anything reads the upper half — so the narrowing is worth pinning
// rather than inferring from a passing Windows build.
func TestSocketConnectResolvedReadsAddrLen(t *testing.T) {
	const def = "define i32 @pal_socket_connect_resolved(i32 %fd, i8* %node)"

	t.Run("Posix", func(t *testing.T) {
		module := ir.NewModule()
		linuxPAL().EmitSocketConnectResolved(module)
		entry := palBlock(t, module.String(), def, ".entry")
		assertContains(t, entry, "load i32, i32*", "socklen_t ai_addrlen loaded as i32")
		if strings.Contains(entry, "load i64") || strings.Contains(entry, "trunc i64") {
			t.Errorf("POSIX ai_addrlen is already socklen_t and must be loaded as i32, not widened/narrowed:\n%s", entry)
		}
	})

	t.Run("Windows", func(t *testing.T) {
		module := ir.NewModule()
		(&WindowsPAL{}).EmitSocketConnectResolved(module)
		entry := palBlock(t, module.String(), def, ".entry")
		assertContains(t, entry, "load i64", "size_t ai_addrlen loaded as i64")
		assertContains(t, entry, "trunc i64", "and narrowed for connect()")
		// The SOCKET handle is 64-bit on Windows even though the PAL carries fds as i32.
		assertContains(t, entry, "zext i32 %fd to i64", "fd widened to a 64-bit SOCKET")
	})
}

// palEmitter names one platform's emission of a PAL function.
type palEmitter struct {
	name string
	emit func(*ir.Module) *ir.Func
}

// resolveHostDef is the signature every platform's pal_resolve_host shares.
const resolveHostDef = "define i32 @pal_resolve_host(i8* %host, i8* %service, i8** %out)"

// resolverHostEmitters covers the platforms whose pal_resolve_host actually
// consults a system resolver — the WASM stubs answer "unsupported" without one.
// EmitGetAddrInfo runs first so pal_getaddrinfo resolves to its definition.
func resolverHostEmitters() []palEmitter {
	return []palEmitter{
		{"Linux", func(m *ir.Module) *ir.Func { p := linuxPAL(); p.EmitGetAddrInfo(m); return p.EmitResolveHost(m) }},
		{"Darwin", func(m *ir.Module) *ir.Func { p := darwinPAL(); p.EmitGetAddrInfo(m); return p.EmitResolveHost(m) }},
		{"Windows", func(m *ir.Module) *ir.Func { p := &WindowsPAL{}; p.EmitGetAddrInfo(m); return p.EmitResolveHost(m) }},
	}
}

// TestResolveHostRejectsEmptyHost: an empty host must be answered with the
// normalized not-found code *without* calling the platform resolver. Leaving
// that decision to getaddrinfo diverges — Windows succeeds and returns every
// local interface address, BSD/macOS succeeds and returns loopback, only glibc
// reports EAI_NONAME — so `TcpStream.connect("", port)` silently connected to
// this machine instead of raising (T1726).
//
// "does not call the resolver" is the load-bearing assertion: TestResolveHostPosix
// already asserts `ret i32 -1` appears, and the .not_found block satisfies that
// on its own, so only checking the return value would pass against the old IR.
func TestResolveHostRejectsEmptyHost(t *testing.T) {
	for _, tt := range resolverHostEmitters() {
		t.Run(tt.name, func(t *testing.T) {
			module := ir.NewModule()
			tt.emit(module)
			out := module.String()
			body := palFuncBody(t, out, resolveHostDef)

			// The guard is decided in .entry, which must not reach the resolver.
			entry := palBlock(t, out, resolveHostDef, ".entry")
			assertContains(t, entry, "icmp eq i8* %host, null", "null host rejected")
			if strings.Contains(entry, "@pal_getaddrinfo(") {
				t.Errorf(".entry must branch on the host before calling the resolver:\n%s", entry)
			}

			// A zero first byte is the empty string, and it short-circuits to
			// the same normalized not-found code glibc's EAI_NONAME produces.
			chk := palBlock(t, out, resolveHostDef, ".chk_empty_host")
			assertContains(t, chk, "load i8, i8* %host", "first byte of the host inspected")
			assertContains(t, chk, "icmp eq i8 %", "compared against NUL")
			assertContains(t, palBlock(t, out, resolveHostDef, ".empty_host"), "ret i32 -1",
				"empty host reports normalized not-found")

			// Whole-function ordering: the byte test happens before the lookup,
			// so no resolver on any platform ever sees the empty name.
			load := strings.Index(body, "load i8, i8* %host")
			call := strings.Index(body, "@pal_getaddrinfo(")
			if load < 0 || call < 0 || load > call {
				t.Errorf("the empty-host test must precede pal_getaddrinfo:\n%s", body)
			}
		})
	}
}

// TestResolveHostGuardBranchTargets pins the guard's *edges*, which its shape
// does not. Every assertion in TestResolveHostRejectsEmptyHost still holds if
// the two branch targets are swapped — the comparisons are still there, the
// .empty_host block still returns -1, and the byte load still precedes the
// call — yet a swapped guard is the worst reachable outcome: every real host
// name short-circuits to "not found", so nothing resolves at all, while the
// empty name becomes the one input handed to the resolver, bringing T1726 back
// at the same time. Nothing else in the Go tests would notice, because only a
// runtime lookup tells the two wirings apart.
func TestResolveHostGuardBranchTargets(t *testing.T) {
	// A null host takes the early exit; anything else is tested byte-wise. A
	// NUL first byte takes that same early exit; anything else is resolved.
	entryBr := regexp.MustCompile(`br i1 %\d+, label %\.empty_host, label %\.chk_empty_host`)
	chkBr := regexp.MustCompile(`br i1 %\d+, label %\.empty_host, label %\.lookup`)
	for _, tt := range resolverHostEmitters() {
		t.Run(tt.name, func(t *testing.T) {
			module := ir.NewModule()
			tt.emit(module)
			out := module.String()

			entry := palBlock(t, out, resolveHostDef, ".entry")
			if !entryBr.MatchString(entry) {
				t.Errorf("a null host must branch to .empty_host and anything else to the byte test:\n%s", entry)
			}
			chk := palBlock(t, out, resolveHostDef, ".chk_empty_host")
			if !chkBr.MatchString(chk) {
				t.Errorf("a NUL first byte must branch to .empty_host and anything else to .lookup:\n%s", chk)
			}

			// The positive half of the guard, and the reason the swap above is
			// not merely cosmetic: a host that survives both tests is passed to
			// the platform resolver unchanged, host and service in that order.
			assertContains(t, palBlock(t, out, resolveHostDef, ".lookup"),
				"@pal_getaddrinfo(i8* %host, i8* %service", "a surviving host still reaches the resolver")
		})
	}
}

// TestResolveHostLeavesOutNullOnFailure: pal_resolve_host must null *out before
// calling the resolver, on every platform. Callers turn *out into a Promise-side
// _AddrList handle; a stale non-null pointer left behind by a failed lookup
// would be freed by _AddrList.drop, which is a free of uninitialized stack.
func TestResolveHostLeavesOutNullOnFailure(t *testing.T) {
	emitters := append(resolverHostEmitters(),
		palEmitter{"Wasm", func(m *ir.Module) *ir.Func { return (&WasmPAL{}).EmitResolveHost(m) }},
		palEmitter{"WasmWeb", func(m *ir.Module) *ir.Func { return (&WasmWebPAL{}).EmitResolveHost(m) }},
	)
	for _, tt := range emitters {
		t.Run(tt.name, func(t *testing.T) {
			module := ir.NewModule()
			tt.emit(module)
			out := module.String()
			// *out is nulled in .entry, but the resolver call now lives in
			// .lookup — so the ordering check has to span the whole function or
			// it silently stops testing anything.
			body := palFuncBody(t, out, resolveHostDef)
			entry := palBlock(t, out, resolveHostDef, ".entry")
			if !strings.Contains(entry, "store i8* null, i8** %out") {
				t.Fatalf("out must be nulled in .entry, before any early return:\n%s", entry)
			}
			// Ordering matters: nulling *after* the call would clobber a
			// successful lookup's result and hand callers an empty list.
			null := strings.Index(body, "store i8* null, i8** %out")
			if call := strings.Index(body, "@pal_getaddrinfo("); call >= 0 && null > call {
				t.Errorf("out must be nulled BEFORE pal_getaddrinfo, not after:\n%s", body)
			}
		})
	}
}
