package codegen

import (
	"strings"
	"testing"
)

// T1640 (R3): a closure captured into a `go` block MOVES its heap env into the
// coroutine frame. Two IR facts make that a transfer rather than a share:
//
//   - the coroutine registers an owning env-free binding for the capture, so the
//     env is freed exactly once at coroutine scope exit; and
//   - the spawner clears the outer binding's drop flag after the spawn, so the
//     defining scope no longer frees it.
//
// Without the transfer the env is freed at exit of the DEFINING scope while the
// goroutine still holds the pointer — a use-after-free even for a capture-free
// closure. Without the coroutine-side binding it leaks instead (maybeRegisterDrop
// returns early for a *types.Signature, which is why registerGoCaptureOwnership
// routes closures to maybeRegisterEnvFree).
func TestT1640GoBlockClosureCaptureTransfersEnv(t *testing.T) {
	ir := generateIR(t, `
		main() {
			done := channel[int](1);
			int base = 10;
			f := |int x| -> x + base;
			go {
				done.send(f(1));
			};
		}
	`)
	coro := extractGoCoroutineBody(t, ir)
	// Coroutine side: the capture gets its own owning env-free binding.
	assertContains(t, coro, "f.dropflag")
	assertContains(t, coro, "env.free")
	assertContains(t, coro, "call void @pal_free")
	// Spawner side: the outer drop flag is cleared, so the defining scope's
	// env-free is skipped and the env is not freed out from under the coroutine.
	assertContains(t, ir, "store i1 false, i1* %f.dropflag")
}

// A capture-free closure needs the same transfer: the env pointer is null, but
// the goroutine still owns the binding and the outer scope must not free it.
func TestT1640GoBlockCaptureFreeClosureStillTransfers(t *testing.T) {
	ir := generateIR(t, `
		main() {
			done := channel[int](1);
			f := |int x| -> x + 1;
			go {
				done.send(f(1));
			};
		}
	`)
	coro := extractGoCoroutineBody(t, ir)
	assertContains(t, coro, "f.dropflag")
	assertContains(t, coro, "env.free")
	// The null check keeps the free safe for a null env.
	assertContains(t, coro, "env.skip")
}

// The `go f(move closure)` call form transfers through the same path.
func TestT1640GoCallClosureArgumentTransfersEnv(t *testing.T) {
	ir := generateIR(t, `
		run_it((int) -> int move f, channel[int] done) {
			done.send(f(1));
		}
		main() {
			done := channel[int](1);
			int base = 10;
			f := |int x| -> x + base;
			go run_it(move f, done);
		}
	`)
	assertContains(t, ir, "env.free")
	assertContains(t, ir, "store i1 false, i1* %f.dropflag")
}

// extractGoCoroutineBody returns the IR text of the SPAWNED `go` block's
// coroutine function (`@.goroutine.N`, not the `@.goroutine.main` wrapper that
// holds main's own body). Isolating it proves the env-free binding lives on the
// GOROUTINE side — the transfer — rather than only in the spawning function,
// which is exactly the distinction T1640 turns on.
func extractGoCoroutineBody(t *testing.T, ir string) string {
	t.Helper()
	const prefix = "define i8* @.goroutine."
	for idx := 0; ; {
		start := strings.Index(ir[idx:], prefix)
		if start < 0 {
			break
		}
		start += idx
		nameEnd := strings.Index(ir[start+len(prefix):], "(")
		if nameEnd < 0 {
			break
		}
		name := ir[start+len(prefix) : start+len(prefix)+nameEnd]
		idx = start + len(prefix)
		if name == "main" {
			continue // main's own body, not a spawned goroutine
		}
		end := strings.Index(ir[start:], "\n}\n")
		if end < 0 {
			end = len(ir) - start
		}
		return ir[start : start+end]
	}
	t.Fatalf("no spawned goroutine coroutine function (%sN) found in IR", prefix)
	return ""
}

// The goroutine-side binding kind must be reproduced from the OUTER binding, not
// re-derived from its valType: an optional binding carries its ELEMENT type
// there, so `((int) -> int)?` would otherwise hand an Optional-struct alloca to
// maybeRegisterEnvFree and emitEnvFree would extractvalue a fat pointer out of a
// `{i1, {i8*, i8*}}` — a hard codegen panic. Sema gates that capture (T1653), so
// the surviving guarantee to pin down is the reverse one: a BARE closure capture
// still takes the env path.
func TestT1640GoCaptureUsesOuterBindingKind(t *testing.T) {
	ir := generateIR(t, `
		main() {
			done := channel[int](1);
			int base = 10;
			f := |int x| -> x + base;
			go {
				done.send(f(1));
			};
		}
	`)
	coro := extractGoCoroutineBody(t, ir)
	assertContains(t, coro, "env.free")
	// An env-free binding, never a string/optional drop, for a Signature capture.
	assertNotContains(t, coro, "@promise_string_drop")
}

// A closure local is registered in dropBindings (so B0354 sees the capture), but
// reassigning it must NOT also take the generic drop-old path — that emitted a
// drop-flag test and an empty drop body before the real T0911/T0913 env release.
func TestT1640ClosureReassignHasNoGenericDropOld(t *testing.T) {
	ir := generateIR(t, `
		reassign_t1640() {
			s1 := "one";
			() -> int f = move || -> s1.len;
			s2 := "two";
			f = move || -> s2.len;
		}
	`)
	body := fnBodyT0913(t, ir, "reassign_t1640")
	assertContains(t, body, "reassign.env.free")
	assertNotContains(t, body, "%drop.exec")
}

// registerGoCaptureOwnership dispatches per capture, not per spawn: a single `go`
// block that captures BOTH a closure and a string must give the closure an
// env-free binding and the string a normal drop. Routing every capture down one
// path would either leak the env (maybeRegisterDrop returns early for a
// Signature) or hand a string alloca to emitEnvFree.
func TestT1640MixedClosureAndStringCapturesUseDistinctPaths(t *testing.T) {
	ir := generateIR(t, `
		main() {
			done := channel[int](1);
			int base = 2;
			f := |int x| -> x + base;
			s := "abcd";
			go {
				done.send(f(1) + s.len);
			};
		}
	`)
	coro := extractGoCoroutineBody(t, ir)
	assertContains(t, coro, "env.free")             // closure capture
	assertContains(t, coro, "@promise_string_drop") // string capture
	// Both outer bindings are surrendered by the spawner.
	assertContains(t, ir, "store i1 false, i1* %f.dropflag")
	assertContains(t, ir, "store i1 false, i1* %s.dropflag")
}

// A CONTAINER of closures is not itself a closure: its outer binding is a normal
// drop binding, so the transfer must take the maybeRegisterDrop path and let
// Vector.drop free the elements. Sending it down the env path would extractvalue
// a fat pointer out of a vector value.
func TestT1640VectorOfClosuresCaptureUsesDropPath(t *testing.T) {
	ir := generateIR(t, `
		main() {
			done := channel[int](1);
			((int) -> int)[] fns = [];
			fns.push(move |int x| -> x + 1);
			go {
				done.send(fns[0](1));
			};
		}
	`)
	coro := extractGoCoroutineBody(t, ir)
	assertContains(t, coro, "fns.dropflag")
	assertNotContains(t, coro, "fns.env.free")
	assertContains(t, ir, "store i1 false, i1* %fns.dropflag")
}

// The go-CALL spawn path (`genGoCallExprViaBlock`) has its own copy of B0354's
// capture collection, and therefore its own T1640 env-owner detection. It is
// selected by a NON-ident callee (a method call here), and it generates the whole
// call inside the coroutine body — so a closure referenced from an argument
// expression is a genuine coroutine capture, unlike in the direct
// `go named_fn(f(1))` path where the argument is evaluated in the spawner and
// only its result crosses.
//
// Without the `binding.kind == bindingFreeEnv` branch on this path, the capture
// would go to maybeRegisterDrop, which returns early for a *types.Signature — so
// the coroutine would register no owning binding while the spawner still clears
// the outer drop flag, and the env would leak on every spawn.
func TestT1640GoCallViaBlockClosureCaptureTransfersEnv(t *testing.T) {
	ir := generateIR(t, `
		type Sink {
			channel[int] done;
			take(this, int v) {
				this.done.send(v);
			}
		}
		main() {
			done := channel[int](1);
			s := Sink(done: done);
			int base = 10;
			f := |int x| -> x + base;
			go s.take(f(1));
		}
	`)
	coro := extractGoCoroutineBody(t, ir)
	// Coroutine side: the capture gets an owning env-free binding, not a drop.
	assertContains(t, coro, "f.dropflag")
	assertContains(t, coro, "env.free")
	assertContains(t, coro, "call void @pal_free")
	// Spawner side: the outer binding is surrendered.
	assertContains(t, ir, "store i1 false, i1* %f.dropflag")
}
