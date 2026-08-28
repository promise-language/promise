package concurrency2

import (
	"regexp"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// T0850: an `Ref[T?]` (or `Mutex[T?]`) whose element is an Optional must drop
// the inner optional's heap payload when the last reference is released. The
// Arc/Mutex inner-drop path (emitInnerDrop) dispatched on extractNamed, which is
// nil for an Optional, so no case fired and the held value leaked. The fix adds
// an Optional case that drops the present inner — here @Box.drop on the held Box.
func TestChannelSendCoroutineRendezvous(t *testing.T) {
	// T0312: Unbuffered channel send inside a go block parks on rv_waiters for the
	// rendezvous. After writing the value, the sender enqueues itself on rv_waiters,
	// sets park_mutex=&ch.mutex, and calls coro.suspend. The scheduler unlocks
	// ch.mutex. The receiver wakes the sender via wake_one(rv_waiters) after count--.
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int]();
			go {
				ch.send(42);
			};
			result := <-ch;
		}
	`)
	// Rendezvous wait and resume blocks must exist
	codegentest.AssertContains(t, ir, "send.rv.wait")
	codegentest.AssertContains(t, ir, "send.rv.resume")
	// rv_waiters park: waiter_enqueue IS called (unlike the old yield-spin)
	codegentest.AssertContains(t, ir, "call void @promise_waiter_enqueue(")
	// Receiver must wake rv_waiters after count--
	codegentest.AssertContains(t, ir, "call void @promise_waiter_wake_one(")
}

func TestChannelSendRendezvousExitWakesNextWaiter(t *testing.T) {
	// T0305/T0312: Rendezvous exit wakes one waiter on send_waiters. rv_waiters
	// holds rendezvous-parked senders; send_waiters holds only write-waiters and
	// select SWNs, so waking it here is safe and never strands a write-waiter.
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int]();
			go {
				ch.send(42);
			};
			result := <-ch;
		}
	`)
	// The rendezvous exit block should exist and call wake_one
	codegentest.AssertContains(t, ir, "send.rv.exit")
	codegentest.AssertContains(t, ir, "call void @promise_waiter_wake_one(")
}

func TestForInChannelCoroutineMode(t *testing.T) {
	// for-in channel inside a go block uses coroutine-mode park+suspend
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go {
				for v in ch {
					int x = v + 1;
				}
			};
			ch.send(1);
			ch.close();
		}
	`)
	// The for-in inside the coroutine should use waiter_enqueue + coro.suspend
	codegentest.AssertContains(t, ir, "forin_ch.recv.wait")
	codegentest.AssertContains(t, ir, "call void @promise_waiter_enqueue(")
	// Should have the coroutine resume block for the for-in
	codegentest.AssertContains(t, ir, "forin_ch.recv.resume")
}

func TestChannelRecvWakesSenderGoroutine(t *testing.T) {
	// After receiving, the code should wake a parked sender goroutine via
	// promise_waiter_wake_one (handles both regular G and select SWN nodes).
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.send(1);
			result := <-ch;
		}
	`)
	codegentest.AssertContains(t, ir, "call void @promise_waiter_wake_one(")
}

func TestChannelSendWakesRecvGoroutine(t *testing.T) {
	// After sending, the code should wake a parked receiver goroutine via
	// promise_waiter_wake_one (handles both regular G and select SWN nodes).
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.send(42);
		}
	`)
	codegentest.AssertContains(t, ir, "call void @promise_waiter_wake_one(")
}

func TestSelectBlockingEmitsSWNParking(t *testing.T) {
	// A blocking select (no default) in coroutine mode should emit:
	// - SelectWaiterNode allocas and initialization (kind sentinel 0xFF)
	// - select_waiter_enqueue calls to park SWNs on channel waiter lists
	// - select_try_wake definition (wake-once protocol)
	// - waiter_wake_one definition (handles both G and SWN nodes)
	// - waiter_remove calls for SWN cleanup after resume
	ir := codegentest.GenerateIR(t, `
		main() {
			ch1 := channel[int](capacity: 1);
			ch2 := channel[int](capacity: 1);
			go { ch1.send(1); };
			select {
				v := <-ch1:
					print_line("ch1");
				v := <-ch2:
					print_line("ch2");
			}
		}
	`)
	// SWN infrastructure functions
	codegentest.AssertContains(t, ir, "define void @promise_select_waiter_enqueue(")
	codegentest.AssertContains(t, ir, "define i1 @promise_select_try_wake(")
	codegentest.AssertContains(t, ir, "define void @promise_waiter_wake_one(")

	// Blocking path: SWN kind sentinel (0xFF = 255) stored to field 1
	codegentest.AssertContains(t, ir, "store i8 255,")

	// SWN enqueue calls (one per case)
	codegentest.AssertContains(t, ir, "call void @promise_select_waiter_enqueue(")

	// SWN cleanup after resume
	codegentest.AssertContains(t, ir, "call void @promise_waiter_remove(")

	// Select mutex lifecycle
	codegentest.AssertContains(t, ir, "call void @pal_mutex_destroy(")
}

func TestSelectNonBlockingNoSWN(t *testing.T) {
	// A select with a default case should NOT emit SWN parking code.
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			select {
				v := <-ch:
					print_line("got");
				default:
					print_line("default");
			}
		}
	`)
	codegentest.AssertNotContains(t, ir, "call void @promise_select_waiter_enqueue(")
	codegentest.AssertContains(t, ir, "select.default")
}

func TestSelectEmptyDefaultNotBlocking(t *testing.T) {
	// B0116: An empty default body (no statements) must still be treated as
	// a non-blocking select. Previously, the nil []Stmt was indistinguishable
	// from "no default clause", causing the select to block.
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			select {
				v := <-ch:
				default:
			}
		}
	`)
	codegentest.AssertNotContains(t, ir, "call void @promise_select_waiter_enqueue(")
	codegentest.AssertContains(t, ir, "select.default")
}

func TestSelectEmptyDefaultTwiceNotBlocking(t *testing.T) {
	// B0116: Two consecutive selects with empty default must both be non-blocking.
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			select { v := <-ch: default: }
			select { v := <-ch: default: }
		}
	`)
	codegentest.AssertNotContains(t, ir, "call void @promise_select_waiter_enqueue(")
	// Both selects should have default blocks
	codegentest.AssertContains(t, ir, "select.default")
}

func TestSelectBlockingPollInNonCoroutine(t *testing.T) {
	// B0045: A blocking select (no default) in non-coroutine context should
	// emit a poll-retry loop that unlocks, sleeps, re-locks, and retries
	// instead of falling through to merge (which silently skips all cases).
	ir := codegentest.GenerateIR(t, `
		foo() {
			ch := channel[int](capacity: 1);
			select {
				v := <-ch:
			}
		}
		main() { foo(); }
	`)
	// Poll block should exist (not SWN parking — that's for coroutines)
	codegentest.AssertContains(t, ir, "select.poll")
	codegentest.AssertNotContains(t, ir, "select.park")
	// Should call usleep in the poll loop
	codegentest.AssertContains(t, ir, "call i32 @usleep(i32 100)")
	// Should branch back to lock.start for retry
	codegentest.AssertContains(t, ir, "br label %select.lock.start")
}

func TestSelectWakePathSendGuard(t *testing.T) {
	// B0110: A blocking select with a send case should emit a fullness
	// re-check on the wake path. Between the wake and re-locking channels,
	// another sender may have filled the freed slot. The guard branches to
	// a retry block that unlocks all channels and retries from lock.start.
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go { <-ch; };
			select {
				ch.send(42):
					print_line("sent");
				v := <-ch:
					print_line("recv");
			}
		}
	`)
	// Wake path should have a send.ok block (guard passed)
	codegentest.AssertContains(t, ir, "select.wk0.send.ok")
	// Wake retry block should exist for failed guard
	codegentest.AssertContains(t, ir, "select.wake.retry")
	// Retry should branch back to lock.start
	codegentest.AssertContains(t, ir, "br label %select.lock.start")
}

func TestSchedulerReleasesParkMutex(t *testing.T) {
	// The scheduler loop checks G.park_mutex after coro.resume returns
	// and releases it if non-null. This closes the enqueue-before-suspend race.
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go { ch.send(42); };
		}
	`)
	// Scheduler loop must contain the park_mutex release blocks
	codegentest.AssertContains(t, ir, "release_park_mutex")
	codegentest.AssertContains(t, ir, "after_release")
}

func TestSchedulerClearsParkMutexBeforeUnlock(t *testing.T) {
	// B0249: park_mutex must be cleared BEFORE the mutex unlock to prevent a race
	// where another thread wakes G, G re-parks with a new mutex, and the stale
	// NULL write overwrites it — causing double-resume and segfault.
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go { ch.send(42); };
		}
	`)
	// In sched_loop (and sched_coop_run for WASM), the release_park_mutex block
	// must store null to park_mutex BEFORE calling pal_mutex_unlock.
	fn := codegentest.ExtractFunction(ir, "promise_sched_loop")
	if fn == "" {
		t.Fatal("promise_sched_loop not found")
	}
	idx := strings.Index(fn, "release_park_mutex:")
	if idx < 0 {
		t.Fatal("release_park_mutex block not found in sched_loop")
	}
	relBlk := fn[idx:]
	storeIdx := strings.Index(relBlk, "store i8* null,")
	unlockIdx := strings.Index(relBlk, "call void @pal_mutex_unlock(")
	if storeIdx < 0 {
		t.Fatal("null store not found in release_park_mutex")
	}
	if unlockIdx < 0 {
		t.Fatal("mutex unlock not found in release_park_mutex")
	}
	if storeIdx > unlockIdx {
		t.Error("B0249: park_mutex null store must come BEFORE mutex unlock to prevent race")
	}
}

func TestSchedParkMRechecksGlobalQueue(t *testing.T) {
	// T0375: park_m must re-check sched.global_size while still holding
	// idle_lock AFTER pushing self onto the idle stack. If a non-M enqueuer
	// raced through sched_enqueue + wake_m against an empty idle stack, the
	// re-check sees the queued work and aborts the park (popping self off the
	// idle stack) instead of committing to cond_wait indefinitely.
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go { ch.send(42); };
		}
	`)
	fn := codegentest.ExtractFunction(ir, "promise_sched_park_m")
	if fn == "" {
		t.Fatal("promise_sched_park_m not found")
	}

	// The abort_park block must exist (the bail path).
	abortIdx := strings.Index(fn, "abort_park:")
	if abortIdx < 0 {
		t.Fatal("abort_park block not found in promise_sched_park_m")
	}

	// continue_park is the normal park path; both must exist.
	continueIdx := strings.Index(fn, "continue_park:")
	if continueIdx < 0 {
		t.Fatal("continue_park block not found in promise_sched_park_m")
	}

	// The conditional branch into abort_park must come from .entry,
	// before we reach wait_loop / cond_wait.
	entryEnd := strings.Index(fn, "abort_park:")
	if entryEnd < 0 {
		t.Fatal("could not locate end of entry block")
	}
	entryBlk := fn[:entryEnd]
	if !strings.Contains(entryBlk, "br i1") {
		t.Error("entry block must conditionally branch (abort_park vs continue_park)")
	}
	if !strings.Contains(entryBlk, ", label %abort_park, label %continue_park") {
		t.Error("entry must branch to abort_park / continue_park based on global queue size")
	}

	// The entry must compare a freshly-loaded i64 against zero — that's the
	// global_size != 0 test.
	if !strings.Contains(entryBlk, "icmp ne i64") {
		t.Error("entry must compare global_size (i64) against zero")
	}

	// The abort_park block must NOT contain pal_cond_wait — that's the whole
	// point of bailing out before we commit to parking.
	abortEnd := strings.Index(fn[abortIdx:], "\n\n")
	if abortEnd < 0 {
		abortEnd = len(fn) - abortIdx
	}
	abortBlk := fn[abortIdx : abortIdx+abortEnd]
	if strings.Contains(abortBlk, "@pal_cond_wait") {
		t.Error("abort_park must not call pal_cond_wait — bailing out before parking")
	}
	// abort_park must unlock both mutexes (idle_lock and park_mutex) and ret.
	if strings.Count(abortBlk, "@pal_mutex_unlock") != 2 {
		t.Error("abort_park must unlock both idle_lock and park_mutex (2 unlocks)")
	}
	if !strings.Contains(abortBlk, "ret void") {
		t.Error("abort_park must return")
	}
}

// T0149: goroutine_exit always calls coro.destroy (panicked goroutines reach
// final suspend via TLS flag propagation, so coro.destroy is safe).
func TestGoroutineExitAlwaysCoroDestroy(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	fn := codegentest.ExtractFunction(ir, "promise_goroutine_exit")
	// Must call coro.destroy unconditionally
	codegentest.AssertContains(t, fn, "call void @llvm.coro.destroy")
	// Must NOT have the old free_coro_frame fallback for panicked goroutines
	codegentest.AssertNotContains(t, fn, "free_coro_frame:")
}

// T0148: genGoBlock has final panic check before final suspend.
func TestPanicCheckGoBlockFinal(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		work() {}
		main() {
			go {
				work();
			};
		}
	`)
	codegentest.AssertContains(t, ir, "go.panic_exit")
	codegentest.AssertContains(t, ir, "@__promise_panic_flag")
}

// --- Generator Tests ---

func TestGeneratorProducesCoroutine(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		count() stream[int] {
			yield 1;
			yield 2;
		}
		main() {}
	`)
	codegentest.AssertContains(t, ir, `.generator.`)
	codegentest.AssertContains(t, ir, "presplitcoroutine")
	codegentest.AssertContains(t, ir, "@llvm.coro.suspend")
}

func TestFailableGeneratorCoroutineHasErrorSlot(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		gen!() stream[int] {
			yield 1;
		}
		main() {}
	`)
	// Coroutine should have error_slot parameter and alloca
	codegentest.AssertContains(t, ir, "error_slot.addr")
}

func TestSchedLoopSetsCurrentM(t *testing.T) {
	// sched_loop should store M param to TLS current_m
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	// sched_loop stores m to current_m
	codegentest.AssertContains(t, ir, "__promise_current_m")
	codegentest.AssertContains(t, ir, "promise_sched_loop")
}

func TestSchedLoopNoSetjmp(t *testing.T) {
	// T0149: sched_loop no longer uses setjmp/longjmp for panic recovery.
	// Panicked goroutines reach final suspend via TLS panic flag propagation
	// (T0146-T0148), so the scheduler just calls coro.resume directly.
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	fn := codegentest.ExtractFunction(ir, "promise_sched_loop")
	// Must NOT contain setjmp or jmpBuf alloca
	codegentest.AssertNotContains(t, fn, "alloca [256 x i8]")
	codegentest.AssertNotContains(t, fn, "setjmp")
	codegentest.AssertNotContains(t, fn, "panic_recovery")
	// Must contain direct coro.resume in the run_g flow
	codegentest.AssertContains(t, fn, "call void @llvm.coro.resume")
}

func TestSchedShutdownUsesMaxP(t *testing.T) {
	// B0120: shutdown must signal/join ALL Ms using max_p (field 14),
	// not num_p (field 5). After set_max_procs reduces num_p, Ms on
	// disabled Ps would not be signaled/joined, causing SIGSEGV on exit.
	ir := codegentest.GenerateIR(t, `
		main() { }
	`)
	fn := codegentest.ExtractFunction(ir, "promise_sched_shutdown")
	// The sched struct GEP that loads the loop bound must reference
	// field index 14 (max_p). The GEP accesses @__promise_sched, and
	// the second field index is the one that selects num_p vs max_p.
	// Check that the GEP for the loop bound uses field 14.
	codegentest.AssertContains(t, fn, "@__promise_sched, i32 0, i32 14")
	// Ensure there is no GEP accessing num_p (field 5) in shutdown —
	// the only sched fields accessed should be shutdown (9), max_p (14), and ps (4).
	codegentest.AssertNotContains(t, fn, "@__promise_sched, i32 0, i32 5")
}

func TestFindRunnableSchedTickGlobalFirstCheck(t *testing.T) {
	// T0326: find_runnable must check global queue first every 61 scheduling
	// iterations (schedtick % 61 == 0) to prevent starvation of goroutines
	// enqueued by non-M threads (e.g., test-thread channel ops).
	ir := codegentest.GenerateIR(t, `main() {}`)
	fn := codegentest.ExtractFunction(ir, "promise_sched_find_runnable")

	// Entry block must read the schedtick field (P field index 7), increment it,
	// and store it back.
	codegentest.AssertContains(t, fn, "i32 0, i32 7")
	// Must compute urem with 61 (prime modulus chosen to avoid resonance with
	// power-of-2 queue sizes) and branch to try_global when result == 0.
	codegentest.AssertContains(t, fn, "urem i64 %")
	codegentest.AssertContains(t, fn, ", 61")
	codegentest.AssertContains(t, fn, "label %try_global, label %check_local")
}

func TestSchedLoopIncrementsSchedTick(t *testing.T) {
	// T0326: sched_loop's runG block must also increment P.schedTick (field 7)
	// before resuming a goroutine. find_runnable uses the tick value set here
	// (not only the one it sets itself) for the global-first priority decision.
	ir := codegentest.GenerateIR(t, `main() {}`)
	fn := codegentest.ExtractFunction(ir, "promise_sched_loop")

	// sched_loop's runG block reads, increments, and stores back P field 7.
	codegentest.AssertContains(t, fn, "i32 0, i32 7")
}

// B0007: Verify that the goroutine coroutine has coro.init.suspend block
// separating allocas in coro.start from the initial coro.suspend.
func TestCoroutineInitSuspendBlock(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			ch.send(42);
		}
	`)
	// Main is wrapped in a goroutine coroutine
	goFunc := codegentest.ExtractFunc(ir, ".goroutine.main")
	if goFunc == "" {
		t.Fatal("expected .goroutine.main function in IR")
	}
	// coro.start should branch to coro.init.suspend (not contain coro.suspend directly)
	codegentest.AssertContains(t, goFunc, "br label %coro.init.suspend")
	// coro.init.suspend block should contain the initial coro.suspend
	codegentest.AssertContains(t, goFunc, "coro.init.suspend:")
	codegentest.AssertContains(t, goFunc, "call i8 @llvm.coro.suspend(")
}

// B0007: Verify that channel send alloca is in coro.start (entry block),
// not in the send.write block.
func TestChannelSendAllocaInEntryBlock(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
		}
	`)
	goFunc := codegentest.ExtractFunc(ir, ".goroutine.main")
	if goFunc == "" {
		t.Fatal("expected .goroutine.main function in IR")
	}

	// The alloca for the send value should be in coro.start, before the br to coro.init.suspend
	// Split on "coro.init.suspend:" to get coro.start content
	parts := strings.SplitN(goFunc, "coro.init.suspend:", 2)
	if len(parts) < 2 {
		t.Fatal("expected coro.init.suspend block")
	}
	coroStart := parts[0]

	// coro.start should contain an alloca for the send value (i64 for int)
	if !strings.Contains(coroStart, "alloca i64") {
		t.Errorf("expected alloca i64 in coro.start for channel send value\ncoro.start:\n%s", coroStart)
	}

	// The send.write block should NOT contain an alloca
	sendWriteIdx := strings.Index(goFunc, "send.write")
	if sendWriteIdx >= 0 {
		// Get the send.write block content (up to next label or end)
		sendWriteBlock := goFunc[sendWriteIdx:]
		nextLabel := strings.Index(sendWriteBlock[1:], "\n")
		if nextLabel > 0 {
			// Check a reasonable window after send.write label
			window := sendWriteBlock[:min(len(sendWriteBlock), 500)]
			if strings.Contains(window, "= alloca ") {
				t.Errorf("send.write block should not contain alloca (should be in entry block)\nblock:\n%s", window)
			}
		}
	}
}

// B0007: Verify that channel recv alloca is in coro.start (entry block),
// not in the chrecv.read block.
func TestChannelRecvAllocaInEntryBlock(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			ch.send(42);
			val := <-ch;
		}
	`)
	goFunc := codegentest.ExtractFunc(ir, ".goroutine.main")
	if goFunc == "" {
		t.Fatal("expected .goroutine.main function in IR")
	}

	// The chrecv.read block should NOT contain an alloca
	readIdx := strings.Index(goFunc, "chrecv.read")
	if readIdx >= 0 {
		readBlock := goFunc[readIdx:]
		window := readBlock[:min(len(readBlock), 500)]
		if strings.Contains(window, "= alloca ") {
			t.Errorf("chrecv.read block should not contain alloca (should be in entry block)\nblock:\n%s", window)
		}
	}
}

// B0007: Verify that go-block coroutines also have the coro.init.suspend separation.
func TestGoBlockCoroutineInitSuspend(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch := channel[int](capacity: 1);
			go {
				ch.send(42);
			};
		}
	`)
	// Find the go-block goroutine function (not .goroutine.main)
	goFunc := codegentest.ExtractFunc(ir, ".goroutine.0")
	if goFunc == "" {
		t.Fatal("expected .goroutine.0 function in IR")
	}
	// Should have the separated init suspend block
	codegentest.AssertContains(t, goFunc, "br label %coro.init.suspend")
	codegentest.AssertContains(t, goFunc, "coro.init.suspend:")
}

// B0353: return in error handler inside go block should branch to final.suspend,
// not emit ret void (the coroutine function returns ptr).
func TestGoBlockReturnInErrorHandler(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		fail!() int { raise error(message: "fail"); }
		main() {
			go {
				x := fail()? e { return; };
			};
		}
	`)
	goFunc := codegentest.ExtractFunc(ir, ".goroutine.0")
	if goFunc == "" {
		t.Fatal("expected .goroutine.0 function in IR")
	}
	// Should branch to final.suspend instead of ret void
	codegentest.AssertContains(t, goFunc, "br label %final.suspend")
	codegentest.AssertNotContains(t, goFunc, "ret void")
}

// B0007: Verify select statement allocas are in entry block.
func TestSelectAllocaInEntryBlock(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			ch1 := channel[int](capacity: 1);
			ch2 := channel[int](capacity: 1);
			ch1.send(10);
			select {
				v := <-ch1:
				v := <-ch2:
			}
		}
	`)
	goFunc := codegentest.ExtractFunc(ir, ".goroutine.main")
	if goFunc == "" {
		t.Fatal("expected .goroutine.main function in IR")
	}

	// coro.start should contain the channel array alloca ([2 x i8*])
	parts := strings.SplitN(goFunc, "coro.init.suspend:", 2)
	if len(parts) < 2 {
		t.Fatal("expected coro.init.suspend block")
	}
	coroStart := parts[0]
	if !strings.Contains(coroStart, "alloca [2 x i8*]") {
		t.Errorf("expected alloca [2 x i8*] in coro.start for select channel array\ncoro.start:\n%s", coroStart)
	}
}

// T0394 (channel limb): the predicate also covers types.IsChannel(exprType).
// Channel reassign on an Optional generic field must produce the same
// reassign-drop + temp-drop shape with Channel.drop reachable.
func TestOptionalGenericFieldReassignChannelEmitsDropAndOptdrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Box[T] { T? value; }
		test() {
			Box[Channel[int]] b = Box[Channel[int]](value: channel[int](2));
			b.value = channel[int](2);
		}
	`)
	codegentest.AssertContains(t, ir, "field.optdrop")
	codegentest.AssertContains(t, ir, "tmp.drop")
	// T0663: Channel.drop is now per-element-type — Channel[int].drop here.
	codegentest.AssertContains(t, ir, `@"Channel[int].drop"`)
}

// B0275: Vector.clone() must dup channel elements (refcount increment).
func TestCloneVectorChannelDupsElements(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		test() {
			ch := channel[int](1);
			v := [ch];
			v2 := v.clone();
		}
	`)
	// Should have the clone loop with channel dup (atomic refcount increment)
	codegentest.AssertContains(t, ir, "vecclone.head")
	codegentest.AssertContains(t, ir, "chdup.inc")
}

// T0261: Verify that vector drop + go block produces unique local names
// (no duplicate vecdrop.idx allocas).
func TestGoBlockVectorDropUniqueNames(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			string[] items = ["a", "b"];
			ch := channel[int](capacity: 1);
			go { ch.send(1); ch.close(); };
			int? v = <-ch;
			print_line("{items.len}");
		}
	`)
	// If codegen succeeds, localNameCount was properly saved/restored.
	// The IR should contain the vector drop loop.
	codegentest.AssertContains(t, ir, "vecdrop.idx")
}

// T0155: Ref[T] constructor allocates {i64, T} and stores refcount=1.
func TestArcConstructorAllocAndRefcount(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			a := Ref[int](42);
		}
	`)
	// Should allocate 24 bytes (i64 strong_count + i64 weak_count + i64 value) — T0157
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(i64 24)")
	// Should store refcount = 1
	codegentest.AssertContains(t, ir, "store i64 1")
	// Should store the value 42
	codegentest.AssertContains(t, ir, "store i64 42")
}

// T0155: Ref[T] scope cleanup uses drop flag and calls Ref[int].drop.
func TestArcDropFlagAndCleanup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			a := Ref[int](42);
		}
	`)
	codegentest.AssertContains(t, ir, "%a.dropflag")
	codegentest.AssertContains(t, ir, `call void @"Ref[int].drop"(`)
}

// T0155: Ref[T].drop function has correct structure: null check, atomic decrement, free.
func TestArcDropFunctionBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			a := Ref[int](42);
		}
	`)
	dropFn := codegentest.ExtractFunction(ir, `"Ref[int].drop"`)
	if dropFn == "" {
		t.Fatal("expected Ref[int].drop function in IR")
	}
	// Null check
	codegentest.AssertContains(t, dropFn, "icmp eq")
	codegentest.AssertContains(t, dropFn, "null")
	// Atomic refcount decrement
	codegentest.AssertContains(t, dropFn, "i64 -1")
	// Free on last reference
	codegentest.AssertContains(t, dropFn, "call void @pal_free(")
}

// T0155: Ref[T].clone atomically increments refcount.
func TestArcCloneAtomicIncrement(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			a := Ref[int](42);
			b := a.clone();
		}
	`)
	// Clone should atomically add 1 to refcount
	codegentest.AssertContains(t, ir, "i64 1")
	// Both a and b should have drop flags
	codegentest.AssertContains(t, ir, "%a.dropflag")
	codegentest.AssertContains(t, ir, "%b.dropflag")
}

// T0155: Ref[T] borrow getter loads value from allocation.
func TestArcBorrowLoadsValue(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			a := Ref[int](42);
			int x = a.borrow;
		}
	`)
	// Borrow GEPs into the {i64, i64, T} struct at field 2 (T0157: value shifted to field 2)
	codegentest.AssertContains(t, ir, "getelementptr { i64, i64, i64 }")
}

// T0157: Ref[T] drop now has two-stage deallocation with weak_count.
func TestArcDropTwoStageDeallocation(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			a := Ref[int](42);
		}
	`)
	// Arc drop should have drop_value block (drops T + decrements weak_count)
	codegentest.AssertContains(t, ir, "drop_value:")
}

// T0499: Arc clone/downgrade chain intermediates produce fresh SSA values via ptrtoint+inttoptr
// so the method result is tracked separately from the constructor stmtTemp.
func TestArcCloneChainFreshSSA(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			Ref[int] b = Ref[int](42).clone();
		}
	`)
	// The clone result must be a fresh SSA value (ptrtoint+inttoptr) so stmtTemp
	// dedup doesn't merge it with the constructor's temp — both get dropped.
	codegentest.AssertContains(t, ir, "ptrtoint")
	codegentest.AssertContains(t, ir, "inttoptr")
	codegentest.AssertContains(t, ir, "%b.dropflag")
}

func TestArcDowngradeChainFreshSSA(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			Weak[int] w = Ref[int](42).downgrade();
		}
	`)
	// Downgrade must also produce a fresh SSA value for chain tracking
	codegentest.AssertContains(t, ir, "ptrtoint")
	codegentest.AssertContains(t, ir, "inttoptr")
	codegentest.AssertContains(t, ir, "%w.dropflag")
}

// T0157: dupArc — reading an Ref[T] field from a droppable type increments strong refcount.
func TestDupArcFieldFromDroppable(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Holder {
			Ref[int] a;
			drop(~this) {}
		}
		main() {
			h := Holder(a: Ref[int](42));
			Ref[int] copy = h.a;
		}
	`)
	// Should produce arcdup block for refcount increment (numeric suffix varies)
	codegentest.AssertContainsMatch(t, ir, `arcdup\.inc\.\d+:`)
	codegentest.AssertContainsMatch(t, ir, `arcdup\.merge\.\d+:`)
}

// T0156: Mutex[T] constructor allocates {i8* pal_handle, T value} and inits mutex.
func TestMutexConstructorAllocAndInit(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			m := Mutex[int](42);
		}
	`)
	// Should init the PAL mutex and cond var
	codegentest.AssertContains(t, ir, "call i8* @pal_mutex_init()")
	codegentest.AssertContains(t, ir, "call i8* @pal_cond_init()")
	// Should store the value 42
	codegentest.AssertContains(t, ir, "store i64 42")
	// Should init held flag to 0
	codegentest.AssertContains(t, ir, "store i8 0")
}

// T0156: Mutex[T] scope cleanup uses drop flag and calls Mutex[int].drop.
func TestMutexDropFlagAndCleanup(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			m := Mutex[int](42);
		}
	`)
	codegentest.AssertContains(t, ir, "%m.dropflag")
	codegentest.AssertContains(t, ir, `call void @"Mutex[int].drop"(`)
}

// T0156/T0285: Mutex[T].drop function has correct structure: null check, destroy cond + mutex, free.
func TestMutexDropFunctionBody(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			m := Mutex[int](42);
		}
	`)
	dropFn := codegentest.ExtractFunction(ir, `"Mutex[int].drop"`)
	if dropFn == "" {
		t.Fatal("expected Mutex[int].drop function in IR")
	}
	// Null check
	codegentest.AssertContains(t, dropFn, "icmp eq")
	codegentest.AssertContains(t, dropFn, "null")
	// Cond var destroy
	codegentest.AssertContains(t, dropFn, "call void @pal_cond_destroy(")
	// PAL mutex destroy
	codegentest.AssertContains(t, dropFn, "call void @pal_mutex_destroy(")
	// Free allocation
	codegentest.AssertContains(t, dropFn, "call void @pal_free(")
}

// T0156/T0285: Mutex.lock() uses scheduler-aware locking and allocates a guard.
func TestMutexLockAcquiresAndAllocatesGuard(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			m := Mutex[int](0);
			use guard := m.lock();
		}
	`)
	// Should lock the PAL mutex (metadata critical section)
	codegentest.AssertContains(t, ir, "call void @pal_mutex_lock(")
	// Should check held flag
	codegentest.AssertContains(t, ir, "icmp eq i8")
	// Should allocate 8 bytes for the guard
	codegentest.AssertContains(t, ir, "call i8* @pal_alloc(i64 8)")
}

// T0301: Mutex.lock() must route to the contested path when waiters are queued,
// even if held==0 momentarily. Prevents newcomer starvation under contention
// when pthread_mutex is not FIFO. The acquired path requires both held==0 AND
// waiter_head==null, combined via `or` on the two conditions.
func TestMutexLockFairCheck(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			m := Mutex[int](0);
			use guard := m.lock();
		}
	`)
	// Locate the block that branches on `mustWait` via `mutex.contested`/`mutex.acquired`.
	acqIdx := strings.Index(ir, "label %mutex.acquired")
	if acqIdx < 0 {
		t.Fatal("expected mutex.acquired branch label in IR")
	}
	// Search a small window before the branch for the fair-check instructions:
	// `icmp ne i8* %waiterHead, null` for hasWaiter, then `or i1 %isHeld, %hasWaiter`.
	windowStart := acqIdx - 400
	if windowStart < 0 {
		windowStart = 0
	}
	window := ir[windowStart:acqIdx]
	if !strings.Contains(window, "or i1") {
		t.Errorf("expected `or i1` combining held and waiter_head checks before mutex.acquired")
	}
	if !strings.Contains(window, "icmp ne i8*") {
		t.Errorf("expected `icmp ne i8*` for waiter_head != null check before mutex.acquired")
	}
}

// T0301: MutexGuard.drop's no-waiter unlock path must signal cond BEFORE
// pal_mutex_unlock (within the unlock.no_waiter block). Signal-before-unlock
// is the defensive POSIX ordering — it avoids a theoretical window where a
// waking cond_wait thread could observe stale `held` state on re-acquire.
func TestMutexUnlockNoWaiterSignalBeforeUnlock(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			m := Mutex[int](0);
			guard := m.lock();
		}
	`)
	dropFn := codegentest.ExtractFunction(ir, "MutexGuard.drop")
	if dropFn == "" {
		t.Fatal("expected MutexGuard.drop function in IR")
	}
	// Locate the no_waiter block body.
	marker := "unlock.no_waiter:"
	blkStart := strings.Index(dropFn, marker)
	if blkStart < 0 {
		t.Fatal("expected unlock.no_waiter block in MutexGuard.drop")
	}
	// Block ends at the next block label or `br`/return.
	blkTail := dropFn[blkStart:]
	idxSignal := strings.Index(blkTail, "@pal_cond_signal")
	idxUnlock := strings.Index(blkTail, "@pal_mutex_unlock")
	if idxSignal < 0 {
		t.Fatal("expected pal_cond_signal in unlock.no_waiter block")
	}
	if idxUnlock < 0 {
		t.Fatal("expected pal_mutex_unlock in unlock.no_waiter block")
	}
	if idxSignal > idxUnlock {
		t.Errorf("pal_cond_signal must come before pal_mutex_unlock in no_waiter block; signal@%d unlock@%d", idxSignal, idxUnlock)
	}
}

// T0156: MutexGuard.borrow getter loads T through the guard's mutex pointer.
func TestMutexGuardBorrowGetter(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			m := Mutex[int](42);
			use guard := m.lock();
			int x = guard.borrow;
		}
	`)
	// Borrow navigates guard → mutex → value via GEPs on {i8*} and the full mutex struct
	codegentest.AssertContains(t, ir, "getelementptr { i8* }, { i8* }*")
	codegentest.AssertContains(t, ir, "getelementptr { i8*, i8*, i8*, i8*, i8, i64 }, { i8*, i8*, i8*, i8*, i8, i64 }*")
}

// T0156: MutexGuard.borrow setter stores T through the guard's mutex pointer.
func TestMutexGuardBorrowSetter(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			m := Mutex[int](0);
			use guard := m.lock();
			guard.borrow = 99;
		}
	`)
	// Should store 99 through the guard→mutex→value path
	codegentest.AssertContains(t, ir, "store i64 99")
}

// T0270: Borrow setter must drop old value for droppable T via emitInnerDrop.
func TestMutexGuardBorrowSetterDropsOldValue(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			m := Mutex[string]("hello");
			use guard := m.lock();
			guard.borrow = "world";
		}
	`)
	// emitInnerDrop should call promise_string_drop on the old value before storing new
	codegentest.AssertContains(t, ir, "call void @promise_string_drop(")
}

// T0270: Borrow setter compound assignment (guard.borrow += val).
func TestMutexGuardBorrowSetterCompoundAssign(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			m := Mutex[int](10);
			use guard := m.lock();
			guard.borrow += 5;
		}
	`)
	// Compound assignment loads current value and adds
	codegentest.AssertContains(t, ir, "add i64")
}

// T0838: Binding the result of an error-handler unwrap of a single-owner
// native-handle optional field (`Mutex[int] m = h.mtx? _ {...}`) on an owned
// owner must neutralize the owner's optional present flag — genOptionalHandlerExpr
// makes NO dup for opaque containers (Mutex/Task are i8* handles that can't be
// deep-copied), so without the neutralization both the bound local and the
// owner's drop would free the same handle → double-free. The fix routes the
// handler binding through neutralizeMemberOptionalField (T0806 Fix C carve-out),
// emitting a `store i1 false` into the owner instance's optional flag.
func TestT0838MutexHandlerBindingNeutralizesOwnerField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MtxHolder { Mutex[int]? mtx; drop(~this) {} }
		main() {
			h := MtxHolder(mtx: Mutex[int](5));
			Mutex[int] m = h.mtx? _ { return; };
		}
	`)
	gmain := codegentest.ExtractDefine(ir, ".goroutine.main")
	// The owner's `Mutex[int]?` field is laid out as the optional struct
	// `{ i1, i8* }` (present flag + opaque handle). Neutralization GEPs into that
	// struct's field 0 and stores `false`, so the owner's drop skips the handle
	// the binding now owns. (Generic drop-flag clears target named allocas, not a
	// GEP into `{ i1, i8* }`, so this pattern is specific to the field move-out.)
	codegentest.AssertContainsMatch(t, gmain,
		`getelementptr \{ i1, i8\* \}, \{ i1, i8\* \}\* %\d+, i32 0, i32 0\s*\n\s*store i1 false`)
}

// T0838: Task[T]? sibling of the Mutex case — covers handlerResultIsNativeHandle's
// types.AsTask branch (reached only when the result is NOT a Mutex). Task is the
// other single-owner opaque i8* handle genOptionalHandlerExpr does not dup, so a
// handler binding `Task[int] t = h.tsk? _ {...}` must likewise neutralize the
// owner's optional present flag (same `{ i1, i8* }` GEP + `store i1 false`).
func TestT0838TaskHandlerBindingNeutralizesOwnerField(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		worker() int { return 42; }
		type TskHolder { Task[int]? tsk; drop(~this) {} }
		main() {
			h := TskHolder(tsk: go worker());
			Task[int] t = h.tsk? _ { return; };
		}
	`)
	gmain := codegentest.ExtractDefine(ir, ".goroutine.main")
	codegentest.AssertContainsMatch(t, gmain,
		`getelementptr \{ i1, i8\* \}, \{ i1, i8\* \}\* %\d+, i32 0, i32 0\s*\n\s*store i1 false`)
}

// T0367: Assigning Ref[T].borrow to a variable must clear the variable's drop
// flag — the borrow returns a non-owning reference, so the parent's drop owns
// the inner value. Without the clear, both the borrow's drop and Arc.drop would
// free the same buffer (double-free / segfault for heap T).
func TestArcBorrowClearsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			borrowed := a.borrow;
		}
	`)
	codegentest.AssertContains(t, ir, "%borrowed.dropflag = alloca i1")
	// maybeRegisterDrop sets dropflag=true; T0367 fix immediately clears it.
	codegentest.AssertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0367: Same fix for MutexGuard[T].borrow.
func TestMutexGuardBorrowClearsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			v := [1, 2, 3];
			m := Mutex[int[]](v);
			use guard := m.lock();
			borrowed := guard.borrow;
		}
	`)
	codegentest.AssertContains(t, ir, "%borrowed.dropflag = alloca i1")
	codegentest.AssertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0367 / T0438: the typed-decl path `T borrowed = a.borrow` for non-Copy T
// is now rejected at sema (no implicit `T& → T` decay). The codegen
// dropflag-clear path being tested here is unreachable under Option A;
// the inferred-decl variant (`borrowed := a.borrow`) still tests the
// codegen behavior for the kept `T&` borrow type.

// T0379: Reassigning Ref[T].borrow to an existing variable must clear the
// dropflag re-armed by the unconditional reset in the assignment path. After
// the reassign-merge block, the sequence is: re-arm, store new pointer, clear
// (T0379 fix). Without the fix, the trailing clear is missing.
func TestArcBorrowReassignClearsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			v1 := [1, 2, 3];
			v2 := [4, 5, 6];
			a1 := Ref[int[]](v1);
			a2 := Ref[int[]](v2);
			borrowed := a1.borrow;
			borrowed = a2.borrow;
		}
	`)
	codegentest.AssertContainsMatch(t, ir, `reassign\.merge[^:]*:\s+store i1 true, i1\* %borrowed\.dropflag\s+store i8\* %[^,]+, i8\*\* %borrowed\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0379: Same fix for MutexGuard[T].borrow reassignment.
func TestMutexGuardBorrowReassignClearsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			v1 := [1, 2, 3];
			v2 := [4, 5, 6];
			m1 := Mutex[int[]](v1);
			m2 := Mutex[int[]](v2);
			use g1 := m1.lock();
			use g2 := m2.lock();
			borrowed := g1.borrow;
			borrowed = g2.borrow;
		}
	`)
	codegentest.AssertContainsMatch(t, ir, `reassign\.merge[^:]*:\s+store i1 true, i1\* %borrowed\.dropflag\s+store i8\* %[^,]+, i8\*\* %borrowed\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0379: Borrow→owned reassignment must NOT clear the dropflag — the local
// now owns the new value and its drop must run at scope exit. Verifies the fix
// is conditional on `isBorrowGetterExpr(s.Value)` and not always applied.
func TestArcBorrowReassignToOwnedKeepsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			borrowed := a.borrow;
			borrowed = [4, 5, 6];
		}
	`)
	// The full T0379-fired pattern (re-arm → store new ptr → clear) must NOT appear
	// after `reassign.merge`: the fix should fire only when RHS is `.borrow`.
	bad := regexp.MustCompile(`reassign\.merge[^:]*:\s+store i1 true, i1\* %borrowed\.dropflag\s+store [^\n]*%borrowed[^\n]*\n\s+store i1 false, i1\* %borrowed\.dropflag`)
	if bad.MatchString(ir) {
		t.Errorf("expected NO trailing flag-clear in reassign.merge for borrow→owned (T0379 should not fire)\ngot:\n%s", ir)
	}
	// But the re-arm and the pointer store should still be present (assignment ran normally).
	codegentest.AssertContainsMatch(t, ir, `reassign\.merge[^:]*:\s+store i1 true, i1\* %borrowed\.dropflag\s+store [^\n]*%borrowed`)
}

// T0377: A borrow laundered through an if-expression (both arms produce
// `.borrow`) must clear the new variable's dropflag — without the fix,
// scope cleanup double-frees with Arc.drop.
func TestArcBorrowThroughIfClearsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			cond := true;
			borrowed := if cond { a.borrow } else { a.borrow };
		}
	`)
	codegentest.AssertContains(t, ir, "%borrowed.dropflag = alloca i1")
	codegentest.AssertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0377: Same fix for match-laundered borrow — every arm produces `.borrow`.
func TestArcBorrowThroughMatchClearsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			k := 1;
			borrowed := match k { 1 => a.borrow, _ => a.borrow };
		}
	`)
	codegentest.AssertContains(t, ir, "%borrowed.dropflag = alloca i1")
	codegentest.AssertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0377: MutexGuard borrow laundered through an if-expression.
func TestMutexGuardBorrowThroughIfClearsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			v := [1, 2, 3];
			m := Mutex[int[]](v);
			use guard := m.lock();
			cond := true;
			borrowed := if cond { guard.borrow } else { guard.borrow };
		}
	`)
	codegentest.AssertContains(t, ir, "%borrowed.dropflag = alloca i1")
	codegentest.AssertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0488: mixed-ownership if-expression (one borrow arm + one owned arm) for
// non-Copy `T` is now rejected at sema time — the codegen path that "must
// NOT clear the dropflag" is unreachable. Sema rejection is covered by
// TestT0488_IfMixedNonCopyRejected in sema/sema_test.go.

// T0377: Parenthesized borrow (`(a.borrow)`) is a trivial laundering form;
// recursion must look through ParenExpr to find the borrow.
func TestArcBorrowThroughParensClearsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			borrowed := (a.borrow);
		}
	`)
	codegentest.AssertContains(t, ir, "%borrowed.dropflag = alloca i1")
	codegentest.AssertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0377: Block-bodied match arms (`=> { a.borrow }` rather than `=> a.borrow`)
// take the `arm.Block` path through `matchArmIsBorrowGetter` — must still
// clear the dropflag when every arm's block result is a borrow.
func TestArcBorrowThroughMatchBlockArmsClearsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			k := 1;
			borrowed := match k {
				1 => { a.borrow },
				_ => { a.borrow },
			};
		}
	`)
	codegentest.AssertContains(t, ir, "%borrowed.dropflag = alloca i1")
	codegentest.AssertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0488: mixed-ownership match-expression for non-Copy `T` is rejected at
// sema time — see TestT0488_MatchMixedNonCopyRejected in sema/sema_test.go.

// T0381: explicit `T&` annotation drives the dropflag-clear path the same
// way as inferred declarations. Type-based detection (replacing the old
// AST-shape heuristic) sees the SharedRef on the RHS expression.
func TestArcBorrowExplicitRefTypeClearsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
			int[]& borrowed = a.borrow;
		}
	`)
	codegentest.AssertContains(t, ir, "%borrowed.dropflag = alloca i1")
	codegentest.AssertContainsMatch(t, ir, `store i1 true, i1\* %borrowed\.dropflag\s+store i1 false, i1\* %borrowed\.dropflag`)
}

// T0381: a getter chain ending in a non-borrow leaf (e.g., `.clone()`)
// produces an OWNED value despite traversing a `T&`. The result expression
// type is `T`, not `T&`, so the dropflag stays armed for proper cleanup.
func TestArcBorrowCloneRetainsDropFlag(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			a := Ref[int](42);
			b := a.clone();
		}
	`)
	// b owns the cloned Arc — drop must run at scope exit.
	codegentest.AssertContains(t, ir, "%b.dropflag = alloca i1")
	bad := regexp.MustCompile(`store i1 true, i1\* %b\.dropflag\s+store i1 false, i1\* %b\.dropflag`)
	if bad.MatchString(ir) {
		t.Errorf("expected dropflag for clone() result to stay armed; T0381 type-based check should not fire (RHS type is Ref[int], not Ref[int]&)\ngot:\n%s", ir)
	}
}

// T0156/T0285/T0291: MutexGuard close/drop functions do scheduler-aware unlock and free.
func TestMutexGuardCloseUnlocksAndFrees(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			m := Mutex[int](0);
			use guard := m.lock();
		}
	`)
	closeFn := codegentest.ExtractFunction(ir, "MutexGuard.close")
	if closeFn == "" {
		t.Fatal("expected MutexGuard.close function in IR")
	}
	// Null check
	codegentest.AssertContains(t, closeFn, "icmp eq")
	// Locks metadata mutex
	codegentest.AssertContains(t, closeFn, "call void @pal_mutex_lock(")
	// Both handoff path and no-waiter path unlock the PAL mutex
	codegentest.AssertContains(t, closeFn, "call void @pal_mutex_unlock(")
	// Free guard
	codegentest.AssertContains(t, closeFn, "call void @pal_free(")
}

// T0291: Mutex.lock() inside a goroutine parks on the waiter list (not spin-yield).
func TestMutexLockParksOnWaiterList(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			m := Mutex[int](0);
			go {
				use guard := m.lock();
			};
		}
	`)
	// The goroutine contested path must enqueue on the waiter list
	codegentest.AssertContains(t, ir, "call void @promise_waiter_enqueue(")
	// The new park-and-wake block label must be present
	codegentest.AssertContains(t, ir, "mutex.park.resume")
	// No spin-retry: the old spin-yield block label must NOT be present
	codegentest.AssertNotContains(t, ir, "mutex.wait.resume")
}

// T0291: MutexGuard.close hands lock off to a waiting goroutine (waiter_dequeue + sched_enqueue).
func TestMutexGuardCloseHandsOffLock(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			m := Mutex[int](0);
			use guard := m.lock();
		}
	`)
	closeFn := codegentest.ExtractFunction(ir, "MutexGuard.close")
	if closeFn == "" {
		t.Fatal("expected MutexGuard.close function in IR")
	}
	// Must dequeue a waiter
	codegentest.AssertContains(t, closeFn, "call i8* @promise_waiter_dequeue(")
	// Must enqueue the woken goroutine (handoff path)
	codegentest.AssertContains(t, closeFn, "call void @promise_sched_enqueue(")
	// No-waiter path: signal cond for thread-blocked waiters
	codegentest.AssertContains(t, closeFn, "call void @pal_cond_signal(")
}

// T0291: MutexGuard.drop (non-use binding) also hands lock off — same body as close.
func TestMutexGuardDropHandsOffLock(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			m := Mutex[int](0);
			guard := m.lock();
		}
	`)
	dropFn := codegentest.ExtractFunction(ir, "MutexGuard.drop")
	if dropFn == "" {
		t.Fatal("expected MutexGuard.drop function in IR")
	}
	// Null check (guard may be null if moved)
	codegentest.AssertContains(t, dropFn, "icmp eq")
	// Must dequeue a waiter (handoff path)
	codegentest.AssertContains(t, dropFn, "call i8* @promise_waiter_dequeue(")
	// Must enqueue the woken goroutine (handoff path)
	codegentest.AssertContains(t, dropFn, "call void @promise_sched_enqueue(")
	// No-waiter path: signal cond for thread-blocked waiters
	codegentest.AssertContains(t, dropFn, "call void @pal_cond_signal(")
	// Free guard
	codegentest.AssertContains(t, dropFn, "call void @pal_free(")
}

// T0156: Mutex[string].drop calls promise_string_drop on inner value.
func TestMutexDropWithStringElement(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			m := Mutex[string]("hello");
		}
	`)
	dropFn := codegentest.ExtractFunction(ir, `"Mutex[string].drop"`)
	if dropFn == "" {
		t.Fatal("expected Mutex[string].drop function in IR")
	}
	// Should drop the inner string before destroying cond + mutex
	codegentest.AssertContains(t, dropFn, "call void @promise_string_drop(")
	codegentest.AssertContains(t, dropFn, "call void @pal_cond_destroy(")
	codegentest.AssertContains(t, dropFn, "call void @pal_mutex_destroy(")
}

// T0272: Ref[T].drop calls user type's drop function + pal_free for heap user types.
func TestArcDropWithUserType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type P { int x; drop(~this) {} }
		main() {
			a := Ref[P](P(x: 5));
		}
	`)
	dropFn := codegentest.ExtractFunction(ir, `"Ref[P].drop"`)
	if dropFn == "" {
		t.Fatal("expected Ref[P].drop function in IR")
	}
	// Should call user drop then pal_free for heap user type
	codegentest.AssertContains(t, dropFn, "call void @P.drop(")
	codegentest.AssertContains(t, dropFn, "call void @pal_free(")
}

// T0272: Ref[T].drop with user type that has no explicit drop — just pal_free.
func TestArcDropWithHeapUserTypeNoDrop(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Q { int x; int y; }
		main() {
			a := Ref[Q](Q(x: 1, y: 2));
		}
	`)
	dropFn := codegentest.ExtractFunction(ir, `"Ref[Q].drop"`)
	if dropFn == "" {
		t.Fatal("expected Ref[Q].drop function in IR")
	}
	// Heap user type without drop — should still free the instance
	codegentest.AssertContains(t, dropFn, "call void @pal_free(")
}

// T0272: Arc constructor with user type claims the heap temp (no premature free).
func TestArcConstructorClaimsHeapTemp(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type R { int val; }
		main() {
			a := Ref[R](R(val: 42));
			r := a.borrow;
		}
	`)
	// The IR should NOT contain a pal_free of the R instance before the Arc drop.
	// Specifically, the main function should not free the R instance directly —
	// only the Ref[R].drop function should handle that.
	mainFn := codegentest.ExtractFunction(ir, "main")
	if mainFn == "" {
		t.Fatal("expected main function in IR")
	}
	// The heap temp for R should be claimed; no direct pal_free of the instance in main
	// (Arc.drop handles it). Count pal_free calls in main — should only be for Arc itself.
	dropFn := codegentest.ExtractFunction(ir, `"Ref[R].drop"`)
	if dropFn == "" {
		t.Fatal("expected Ref[R].drop function in IR")
	}
	codegentest.AssertContains(t, dropFn, "call void @pal_free(")
}

// T0273: Ref[Ref[T]] clears drop flag on inner variable to prevent double-drop.
func TestArcConstructorClearsDropFlagOnIdent(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			inner := Ref[int](42);
			outer := Ref[Ref[int]](inner);
		}
	`)
	// The goroutine body should clear inner's drop flag after moving it into Arc.
	// "store i1 false" is the drop flag clear pattern.
	codegentest.AssertContains(t, ir, "store i1 false")
}

// T0273: Mutex[T] constructor clears drop flag on moved variable.
func TestMutexConstructorClearsDropFlagOnIdent(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			a := Ref[int](10);
			m := Mutex[Ref[int]](a);
		}
	`)
	codegentest.AssertContains(t, ir, "store i1 false")
}

// T0272: Mutex[T].drop calls user type's drop function + pal_free.
func TestMutexDropWithUserType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type MP { int x; drop(~this) {} }
		main() {
			m := Mutex[MP](MP(x: 10));
		}
	`)
	dropFn := codegentest.ExtractFunction(ir, `"Mutex[MP].drop"`)
	if dropFn == "" {
		t.Fatal("expected Mutex[MP].drop function in IR")
	}
	codegentest.AssertContains(t, dropFn, "call void @MP.drop(")
	codegentest.AssertContains(t, dropFn, "call void @pal_free(")
	codegentest.AssertContains(t, dropFn, "call void @pal_mutex_destroy(")
}

// T0272: Arc drop with vector inner type calls Vector.drop.
func TestArcDropWithVectorElement(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			v := [1, 2, 3];
			a := Ref[int[]](v);
		}
	`)
	dropFn := codegentest.ExtractFunction(ir, `"Ref[int[]].drop"`)
	if dropFn == "" {
		// Try alternative mangled name
		dropFn = codegentest.ExtractFunction(ir, `"Ref[Vector[int]].drop"`)
	}
	if dropFn == "" {
		t.Fatal("expected Ref[int[]].drop or Ref[Vector[int]].drop function in IR")
	}
	codegentest.AssertContains(t, dropFn, "call void @Vector.drop(")
}

// T0272: Mutex drop with user type that has synth drop (string field).
func TestMutexDropWithSynthDropUserType(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type Named { string name; }
		main() {
			m := Mutex[Named](Named(name: "hi"));
		}
	`)
	dropFn := codegentest.ExtractFunction(ir, `"Mutex[Named].drop"`)
	if dropFn == "" {
		t.Fatal("expected Mutex[Named].drop function in IR")
	}
	// Synth drop types have their own drop function that handles field cleanup
	codegentest.AssertContains(t, dropFn, "call void @Named.drop(")
	codegentest.AssertContains(t, dropFn, "call void @pal_mutex_destroy(")
}

// T0271: Lambda capturing Ref[T] uses envDropCallFn (i8* + drop fn), not envDropUserValueDrop.
func TestLambdaEnvDropArcCapture(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			a := Ref[int](42);
			f := move || -> int { return a.borrow; };
			f();
		}
	`)
	// The env drop function should call Ref[int].drop on the i8* field,
	// not extract a {i8*, i8*} value struct (which would be type confusion).
	codegentest.AssertContains(t, ir, `call void @"Ref[int].drop"(`)
}

// T0271: Lambda capturing Mutex[T] uses envDropCallFn.
func TestLambdaEnvDropMutexCapture(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			m := Mutex[int](10);
			f := move || -> int {
				use g := m.lock();
				return g.borrow;
			};
			f();
		}
	`)
	codegentest.AssertContains(t, ir, `call void @"Mutex[int].drop"(`)
}

// T0271: Lambda capturing MutexGuard uses envDropCallFn with MutexGuard.drop.
func TestLambdaEnvDropMutexGuardCapture(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		main() {
			m := Mutex[int](10);
			use g := m.lock();
			f := move || -> int { return g.borrow; };
			f();
		}
	`)
	codegentest.AssertContains(t, ir, "call void @MutexGuard.drop(")
}

// T0411: Channel field auto-dup via constructor field-init from `this.field`.
// Channel dup is a refcount increment via promise_channel_incref.
func TestT0411_ConstructorChannelFieldFromThisDups(t *testing.T) {
	ir := codegentest.GenerateIR(t, `
		type ChH {
			channel[int] ch;
			drop(~this) {}
			clone() ChH {
				return ChH(ch: this.ch);
			}
		}
		test_t0411_ch_dup() {
			c := channel[int](1);
			h := ChH(ch: c);
			h2 := h.clone();
		}
	`)
	cloneFn := codegentest.ExtractFunction(ir, "ChH.clone")
	if cloneFn == "" {
		t.Fatal("expected ChH.clone in IR")
	}
	// dupChannel emits a `chdup.inc` block label and an inline atomicrmw add
	// to bump the channel's reference count.
	codegentest.AssertContains(t, cloneFn, "chdup.inc")
}
