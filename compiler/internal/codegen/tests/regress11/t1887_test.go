package regress11

import (
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T1887: an opaque native handle (MutexGuard, Channel, Mutex, Task, Arc, Weak,
// Vector) boxed into a structural interface view carries no RTTI header of its
// own, so a view holding the bare handle cannot be dropped —
// __promise_structural_drop reads the typeinfo from field 0 of whatever it is
// handed, and an opaque handle's first word is not one.
//
// The fix boxes the handle as { i8* typeinfo, i8* handle } exactly as strings and
// primitives are, with a per-type drop thunk that releases the handle and then
// frees the box. That makes every RTTI drop site work uniformly — scope binding,
// struct field, container element and Optional alike — so these tests assert the
// box is built and the thunk is what runs, rather than any one call site.

// extractQuotedFunc pulls a function body out of the IR by name. codegentest's
// ExtractFunction matches bare @name, but these symbols carry `$` and `[]` so
// LLVM prints them quoted (@"__promise_container_box_drop$MutexGuard[int]").
func extractQuotedFunc(ir, name string) string {
	// Anchor on the definition, not a call site: the same symbol appears at both.
	i := -1
	for _, marker := range []string{`@"` + name + `"(`, "@" + name + "("} {
		for at := 0; ; {
			j := strings.Index(ir[at:], marker)
			if j < 0 {
				break
			}
			j += at
			if lineStart := strings.LastIndex(ir[:j], "\n") + 1; strings.HasPrefix(ir[lineStart:], "define ") {
				i = lineStart
				break
			}
			at = j + len(marker)
		}
		if i >= 0 {
			break
		}
	}
	if i < 0 {
		return ""
	}
	rest := ir[i:]
	if end := strings.Index(rest, "\n}"); end >= 0 {
		return rest[:end+2]
	}
	return rest
}

// boxedGuardIR is the canonical shape: a guard boxed into a Closer local.
const boxedGuardIR = `
	t() {
	  Mutex[int] m = Mutex[int](value: 1);
	  Closer c = m.lock();
	}
	main() {}
`

func TestT1887_OpaqueHandleIsBoxedNotStoredRaw(t *testing.T) {
	ir := codegentest.GenerateIR(t, boxedGuardIR)
	body := codegentest.ExtractFunction(ir, "__user.t")
	if body == "" {
		t.Fatalf("expected __user.t in IR")
	}
	// The view must carry a two-word heap box — field 0 an RTTI header, field 1 the
	// handle — not the bare handle. Without the header, dropping the view reads the
	// handle's own first word as a typeinfo pointer and faults.
	if !strings.Contains(body, "@pal_alloc(i64 16)") {
		t.Fatalf("expected a { typeinfo, handle } box to be allocated for the view:\n%s", body)
	}
	if !strings.Contains(body, "typeinfo") {
		t.Fatalf("expected an RTTI header stored into the box:\n%s", body)
	}
}

func TestT1887_OwnedGuardTempIsClaimedByTheBox(t *testing.T) {
	ir := codegentest.GenerateIR(t, boxedGuardIR)
	body := codegentest.ExtractFunction(ir, "__user.t")
	if body == "" {
		t.Fatalf("expected __user.t in IR")
	}
	// Ownership moves into the box, so the statement temp's drop flag must be
	// cleared. Leaving it set drops the guard twice — once at statement end and
	// again through the box.
	if !strings.Contains(body, "store i1 false") {
		t.Fatalf("expected the guard's stmt-temp drop flag to be cleared when the box takes ownership:\n%s", body)
	}
}

// The four call sites that used to die with a segfault at 0x8, now reaching the
// same box: each must produce the boxed header rather than a raw handle.
func TestT1887_EveryBoxingSiteProducesTheBoxedHeader(t *testing.T) {
	cases := []struct{ name, src string }{
		{"local", `t() { Mutex[int] m = Mutex[int](value: 1); Closer c = m.lock(); }`},
		{"optional", `t() { Mutex[int] m = Mutex[int](value: 1); Closer? c = m.lock(); }`},
		{"push", `t() { Mutex[int] m = Mutex[int](value: 1); Closer[] v = []; v.push(m.lock()); }`},
		{"vector_literal", `t() { Mutex[int] m = Mutex[int](value: 1); Closer[] v = [m.lock()]; }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ir := codegentest.GenerateIR(t, tc.src+"\nmain() {}\n")
			if !strings.Contains(ir, "@pal_alloc(i64 16)") {
				t.Fatalf("%s: guard reached a structural view without being boxed", tc.name)
			}
		})
	}
}

// A constructor field is the fifth site; it needs its own type declaration.
func TestT1887_ConstructorFieldProducesTheBoxedHeader(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder { Closer c; }
		t() {
		  Mutex[int] m = Mutex[int](value: 1);
		  Holder h = Holder(c: m.lock());
		}
		main() {}
	`)
	if !strings.Contains(ir, "@pal_alloc(i64 16)") {
		t.Fatalf("guard stored into a Closer field without being boxed:\n%s", ir)
	}
}

// A Channel is a different opaque handle with a per-element-type drop, so it
// exercises the keyed thunk rather than MutexGuard's single T-independent symbol.
func TestT1887_ChannelBoxedAsCloserGetsItsOwnThunk(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		t() { Closer c = channel[int](); }
		main() {}
	`)
	thunk := extractQuotedFunc(ir, `__promise_container_box_drop$Channel[int]`)
	if thunk == "" {
		t.Fatalf("expected a Channel[int]-keyed box drop thunk in IR")
	}
	// It must run the channel's real release, not pal_free on the handle: a
	// pal_free'd channel keeps its buffer, mutex and waiter list. Channel's drop is
	// synthesized per instantiation under a mono name, which resolving the drop by
	// name got wrong — the thunk now runs the ordinary field-drop walk (T1885).
	if !strings.Contains(thunk, `Channel[int].drop`) {
		t.Fatalf("box thunk must call the channel's own drop, not pal_free the handle:\n%s", thunk)
	}
	if !strings.Contains(thunk, "call void @pal_free") {
		t.Fatalf("box thunk must free the box wrapper itself:\n%s", thunk)
	}
}

// The view adapter must unwrap the box before handing the receiver to the
// concrete method — otherwise a method called through the interface receives the
// box address as `this` and corrupts the handle.
func TestT1887_ViewAdapterUnwrapsBoxedReceiver(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		t() {
		  Mutex[int] m = Mutex[int](value: 1);
		  Closer c = m.lock();
		  c.close()?!;
		}
		main() {}
	`)
	adapter := extractQuotedFunc(ir, `MutexGuard[int].close$view_adapt_as_Closer`)
	if adapter == "" {
		t.Fatalf("expected the Closer view adapter for MutexGuard[int] in IR")
	}
	// Field 1 of the box is the handle; the adapter must load it, not forward the
	// box pointer straight through.
	if !strings.Contains(adapter, "getelementptr") || !strings.Contains(adapter, "load i8*") {
		t.Fatalf("adapter must load the handle out of the box before calling the concrete method:\n%s", adapter)
	}
}

// A borrowed handle that can be duplicated (a channel is a refcounted handle) is
// cloned INTO the box, exactly as a borrowed string is (T1282): the box owns a
// second reference and releases it through its owning header, while the original
// binding still drops its own — so the release count balances without the box
// ever aliasing a payload it does not own (T1885).
func TestT1887_BorrowedDuplicableHandleIsClonedIntoTheBox(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		t() {
		  channel[int] ch = channel[int]();
		  Closer c = ch;
		}
		main() {}
	`)
	body := codegentest.ExtractFunction(ir, "__user.t")
	if body == "" {
		t.Fatalf("expected __user.t in IR")
	}
	if !strings.Contains(body, "chdup.inc") {
		t.Fatalf("a borrowed channel must be duplicated (refcount bump) into its box:\n%s", body)
	}
	if !strings.Contains(body, `promise_typeinfo_containerbox$Channel[int]`) {
		t.Fatalf("the box owns its duplicated channel, so it needs the owning header:\n%s", body)
	}
	// The original binding keeps its own release: two references, two drops.
	if !strings.Contains(body, `Channel[int].drop`) {
		t.Fatalf("the borrowed channel's own drop must still run at scope exit:\n%s", body)
	}
}

// A borrowed handle that CANNOT be duplicated must NOT be adopted by the box: the
// original binding still drops it, so a box that also owned it would double-release.
// It aliases the payload under a null-drop flat header instead.
func TestT1887_BorrowedSingleOwnerHandleBoxIsNotGivenOwnership(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		t() {
		  Mutex[int] m = Mutex[int](value: 1);
		  MutexGuard[int] g = m.lock();
		  Closer c = g;
		}
		main() {}
	`)
	body := codegentest.ExtractFunction(ir, "__user.t")
	if body == "" {
		t.Fatalf("expected __user.t in IR")
	}
	if strings.Contains(body, `promise_typeinfo_containerbox$MutexGuard`) {
		t.Fatalf("a borrowed guard was boxed with an owning header — its drop would run twice:\n%s", body)
	}
	if !strings.Contains(body, "promise_typeinfo_flatbox") {
		t.Fatalf("expected the borrowed guard to get a null-drop flat header:\n%s", body)
	}
}
