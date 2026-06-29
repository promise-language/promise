package codegen

import (
	"regexp"
	"strings"
	"testing"
)

// T1162: an owner-governed member-source optional handler (`owner.field? _ { recovery }`)
// whose result is a single-owner opaque handle (Channel/Mutex/MutexGuard/Task) must
// arm a PER-BRANCH drop flag on the merged phi: cleared (false) on the present (some)
// edge so the owner's field drop stays the sole free, armed (true) on the absent
// (none/recovery) edge so the fresh recovery handle is dropped once at statement end.
// Opaque handles can't be deep-copied (the string/vector/heap-user T0775 path dups the
// present value instead), so a single compile-time track/skip can't express the two
// different ownerships — hence the per-branch flag.

// optMergeFlagPhi matches the T1162 per-branch flag phi in an opt.merge block: false
// on the some (present) edge, true on the none (recovery) edge. Present aliases the
// owner's field (owner drops it); absent is a fresh handle this temp must free once.
var optMergeFlagPhi = regexp.MustCompile(`phi i1 \[ false, %opt\.some\.\d+ \], \[ true, %[\w.]+ \]`)

// TestT1162ChannelInlineAbsentArmsPerBranchFlag locks the core fix on the exact repro:
// `(b.f? _ { Channel[int](1) }).close()` registers the merged phi as a statement temp
// whose live flag is false-on-some / true-on-none, and the absent recovery is freed via
// Channel[int].drop gated on that flag.
func TestT1162ChannelInlineAbsentArmsPerBranchFlag(t *testing.T) {
	ir := generateIR(t, `
		type ChanBox { Channel[int]? f; }
		demo() {
			b := ChanBox(f: none);
			(b.f? _ { Channel[int](1) }).close();
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	if !optMergeFlagPhi.MatchString(fn) {
		t.Errorf("member-source optional handler must arm a per-branch flag phi (false on some, true on none) for the opaque-handle result (T1162)\n%s", fn)
	}
	if !strings.Contains(fn, `call void @"Channel[int].drop"`) {
		t.Errorf("absent recovery Channel must be freed via Channel[int].drop at statement end (T1162)\n%s", fn)
	}
}

// TestT1162MutexInlineAbsentArmsPerBranchFlag covers the Mutex arm of
// optionalHandlerHandleDrop, with a heap (string) payload so a missing drop leaks the
// payload too.
func TestT1162MutexInlineAbsentArmsPerBranchFlag(t *testing.T) {
	ir := generateIR(t, `
		type MtxBox { Mutex[string]? f; }
		demo() {
			b := MtxBox(f: none);
			(b.f? _ { Mutex[string]("payload") }).lock().close();
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	if !optMergeFlagPhi.MatchString(fn) {
		t.Errorf("member-source Mutex handler must arm a per-branch flag phi (T1162)\n%s", fn)
	}
	if !strings.Contains(fn, `call void @"Mutex[string].drop"`) {
		t.Errorf("absent recovery Mutex must be freed via Mutex[string].drop at statement end (T1162)\n%s", fn)
	}
}

// TestT1162MutexGuardInlineAbsentArmsPerBranchFlag covers the MutexGuard arm of
// optionalHandlerHandleDrop (`c.funcs["MutexGuard.drop"]`): the inline recovery
// guard is released exactly once via the per-branch flag while the present arm
// stays owner-aliased.
func TestT1162MutexGuardInlineAbsentArmsPerBranchFlag(t *testing.T) {
	ir := generateIR(t, `
		type GuardBox { MutexGuard[int]? f; }
		demo() {
			m2 := Mutex[int](5);
			b := GuardBox(f: none);
			(b.f? _ { m2.lock() }).close();
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	if !optMergeFlagPhi.MatchString(fn) {
		t.Errorf("member-source MutexGuard handler must arm a per-branch flag phi (T1162)\n%s", fn)
	}
	if !strings.Contains(fn, "call void @MutexGuard.drop") {
		t.Errorf("absent recovery MutexGuard must be freed via MutexGuard.drop at statement end (T1162)\n%s", fn)
	}
}

// TestT1162TaskAwaitAbsentArmsPerBranchFlag covers the Task arm of
// optionalHandlerHandleDrop (getOrCreateTaskDrop). The `<-` await consumer claims
// the temp (the await joins+frees the G), so the per-branch flag must still be
// emitted on the merged phi for the consumer to neutralize — verify both the flag
// phi and that the Task drop function is materialized.
func TestT1162TaskAwaitAbsentArmsPerBranchFlag(t *testing.T) {
	ir := generateIR(t, `
		type TaskBox { Task[int]? f; }
		worker() int { return 7; }
		demo() {
			b := TaskBox(f: none);
			r := <-(b.f? _ { go worker() });
		}
	`)
	fn := extractFunction(ir, "__user.demo")
	if fn == "" {
		t.Fatal("could not extract __user.demo")
	}
	if !optMergeFlagPhi.MatchString(fn) {
		t.Errorf("member-source Task handler must arm a per-branch flag phi for the await consumer to claim (T1162)\n%s", fn)
	}
	if !strings.Contains(ir, `"Task[int].drop"`) {
		t.Errorf("Task arm must materialize Task[int].drop via getOrCreateTaskDrop (T1162)\n%s", fn)
	}
}

// TestT1162GenericContextArmsPerBranchFlag locks the typeSubst arm of
// optionalHandlerHandleDrop: inside a generic function the handler result type is
// `Channel[T]` (with TypeParam T); it must substitute to the concrete `Channel[int]`
// so the handle is recognized and the per-branch flag is armed in the monomorphized
// body.
func TestT1162GenericContextArmsPerBranchFlag(t *testing.T) {
	ir := generateIR(t, `
		type ChanBoxG[T] { Channel[T]? f; }
		pick[T](T move seed) {
			b := ChanBoxG[T](f: none);
			(b.f? _ { Channel[T](1) }).close();
		}
		demo() { pick(7); }
	`)
	fn := extractDefine(ir, `"pick[int]"`)
	if fn == "" {
		t.Fatal("could not extract monomorphized pick[int]")
	}
	if !optMergeFlagPhi.MatchString(fn) {
		t.Errorf("generic-context member-source handler must arm a per-branch flag phi after typeSubst (T1162)\n%s", fn)
	}
	if !strings.Contains(fn, `call void @"Channel[int].drop"`) {
		t.Errorf("generic-context absent recovery must be freed via Channel[int].drop (T1162)\n%s", fn)
	}
}
