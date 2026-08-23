package codegen

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/antlr4-go/antlr/v4"
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T1639: the post-test goroutine drain used to spin without a deadline, so a
// test whose body returned while a goroutine stayed parked forever wedged the
// batch until the 10-minute process backstop killed it — with no test named.
// The drain is now bounded by the test's own per-test timeout, and waits only
// for the goroutines the test itself created (a running baseline of the deficit
// abandoned by earlier tests).

// testMainIR generates the test-runner main() for src, with the given per-test
// timeouts, on the given target.
func testMainIR(t *testing.T, src, target string, timeouts map[string]int64) string {
	t.Helper()

	stdModInfo, stdScope := getCodegenStdModInfo()

	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	file, errs := ast.Build("test.pr", tree)
	if len(errs) > 0 {
		t.Fatalf("AST build errors: %v", errs)
	}
	file.Uses = append([]*ast.UseDecl{{Alias: "_", CatalogName: "std"}}, file.Uses...)

	ti := sema.ParseTargetInfo(target)
	info, semaErrs := sema.CheckWithTarget(file, map[string]*types.Scope{"std": stdScope}, ti)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{"std": stdModInfo}
	info.ModuleOrder = []string{"std"}

	result := CompileWithOptions(file, info, target, &CompileOptions{DebugAllocator: true})
	result.GenerateTestMain(info.Tests, timeouts)
	return result.Module.String()
}

const t1639Src = "myTest() `test { }"

func TestDrainDeadlineEmittedForBoundedTest(t *testing.T) {
	ir := testMainIR(t, t1639Src, HostTargetTriple(), map[string]int64{"myTest": 2_000_000_000})

	// The bounded drain has its own deadline-check and give-up blocks.
	assertContains(t, ir, "drain_check_myTest:")
	assertContains(t, ir, "drain_timeout_myTest:")
	// The deadline is armed once, in drain_slow, from the compile-time timeout —
	// not re-armed each iteration, which would never expire.
	slow := blockBody(t, ir, "drain_slow_myTest")
	if !strings.Contains(slow, "add i64") || !strings.Contains(slow, "2000000000") {
		t.Errorf("drain deadline is not armed from the per-test timeout in drain_slow:\n%s", slow)
	}
	if strings.Contains(blockBody(t, ir, "drain_gs_myTest"), "2000000000") {
		t.Error("drain deadline is re-armed inside the loop — it would never expire")
	}
	// The give-up path prints the distinct TIMEOUT context.
	assertContains(t, ir, "print_stuck_ctx_myTest:")
	assertContains(t, ir, "test body returned but ")
	assertContains(t, ir, " goroutine did not exit within ")
	assertContains(t, ir, " goroutines did not exit within ")

	// The count is rendered through promise_int_to_string, which allocates —
	// the message emitter must free it, exactly as the leak message does. This
	// path only runs on a wedge, so a leak here would never show up in the
	// ordinary suite.
	stuck := blockBody(t, ir, "print_stuck_ctx_myTest")
	if !strings.Contains(stuck, "call i8* @promise_int_to_string") {
		t.Errorf("stuck-goroutine context does not render the count:\n%s", stuck)
	}
	if !strings.Contains(stuck, "call void @pal_free") {
		t.Errorf("stuck-goroutine context leaks the rendered count:\n%s", stuck)
	}
}

func TestDrainWaitsOnBaselineNotEquality(t *testing.T) {
	ir := testMainIR(t, t1639Src, HostTargetTriple(), map[string]int64{"myTest": 2_000_000_000})

	// Both the fast check (in leak_check) and the loop check (in drain_gs) must
	// use the relative form (created - completed) <= baseline, not
	// created == completed: goroutines abandoned by an earlier test must not
	// make this test wait forever.
	for _, label := range []string{"leak_check_myTest", "drain_gs_myTest"} {
		blk := blockBody(t, ir, label)
		if !strings.Contains(blk, "icmp sle i64") {
			t.Errorf("%s does not compare the outstanding count against the baseline:\n%s", label, blk)
		}
		if strings.Contains(blk, "icmp eq i64") {
			t.Errorf("%s still uses an absolute equality compare:\n%s", label, blk)
		}
		// Exactly one outstanding count is computed per check — the baseline is
		// the pre-test snapshot, not something recomputed here.
		if n := strings.Count(blk, "sub i64"); n != 1 {
			t.Errorf("%s computes %d outstanding counts, want 1:\n%s", label, n, blk)
		}
	}
}

func TestNoDrainDeadlineWhenTimeoutOptedOut(t *testing.T) {
	// timeoutNs == 0 is the explicit opt-out — keep the historical unbounded
	// wait rather than inventing a deadline the user disabled.
	ir := testMainIR(t, t1639Src, HostTargetTriple(), map[string]int64{"myTest": 0})

	assertNotContains(t, ir, "drain_check_myTest:")
	assertNotContains(t, ir, "drain_timeout_myTest:")
	assertNotContains(t, ir, "print_stuck_ctx_myTest:")
	// The drain itself is still emitted, still relative to the baseline.
	assertContains(t, ir, "drain_gs_myTest:")
	assertContains(t, ir, "icmp sle i64")
}

func TestNoDrainBlocksOnWasm(t *testing.T) {
	// WASM's cooperative scheduler has no drain loop at all, so none of the
	// T1639 machinery applies there.
	ir := testMainIR(t, t1639Src, "wasm32-wasi", map[string]int64{"myTest": 2_000_000_000})

	assertNotContains(t, ir, "drain_gs_myTest:")
	assertNotContains(t, ir, "drain_check_myTest:")
	assertNotContains(t, ir, "drain_timeout_myTest:")
}

// blockBody returns the instructions of the named LLVM basic block, which the
// printer emits as "<label>:\n" followed by tab-indented lines.
func blockBody(t *testing.T, ir, label string) string {
	t.Helper()
	i := strings.Index(ir, "\n"+label+":\n")
	if i < 0 {
		t.Fatalf("block %s not found in generated test main", label)
	}
	body := ir[i+len("\n"+label+":\n"):]
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "\t") {
			break
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestDrainTimeoutKeepsFailOutcome(t *testing.T) {
	// A test that both failed its assertion *and* abandoned a goroutine must
	// stay a FAIL: the assertion message is the actionable diagnostic, and the
	// FAIL path is what frees the heap-allocated panic message. The give-up
	// block therefore selects 1 (fail) over 2 (timeout) when result == 1.
	ir := testMainIR(t, t1639Src, HostTargetTriple(), map[string]int64{"myTest": 2_000_000_000})

	give := blockBody(t, ir, "drain_timeout_myTest")
	if !strings.Contains(give, "icmp eq i32") {
		t.Errorf("drain give-up path does not test the original result:\n%s", give)
	}
	if !strings.Contains(give, "select i1") || !strings.Contains(give, "i32 1, i32 2") {
		t.Errorf("drain give-up path does not preserve FAIL over TIMEOUT:\n%s", give)
	}
	// The FAIL-context branch must key off effectiveResult, not the raw result:
	// on the drain path result is still 0 while the test is classified TIMEOUT,
	// and on every other path the two agree. Both the FAIL and the TIMEOUT
	// comparison therefore read the same phi.
	merge := blockBody(t, ir, "after_leak_detect_myTest")
	resultPhi := firstPhiOf(t, merge, "phi i32")
	after := ir[strings.Index(ir, "\nafter_leak_detect_myTest:\n"):]
	for _, want := range []string{
		"icmp eq i32 " + resultPhi + ", 1", // FAIL context
		"icmp eq i32 " + resultPhi + ", 2", // TIMEOUT context
	} {
		if !strings.Contains(after, want) {
			t.Errorf("expected %q — outcome context is not keyed off the effective result", want)
		}
	}
}

func TestDrainTimeoutReportsElapsedToDeadline(t *testing.T) {
	// The TIMEOUT line's elapsed must be the wall time up to the drain
	// deadline, not the (near-zero) time the body itself took — otherwise a
	// 2s wedge is rendered as "TIMEOUT (0.000s)".
	ir := testMainIR(t, t1639Src, HostTargetTriple(), map[string]int64{"myTest": 2_000_000_000})

	give := blockBody(t, ir, "drain_timeout_myTest")
	if strings.Count(give, "call i64 @.promise_nanotime_raw()") != 1 {
		t.Errorf("drain give-up path does not re-read the clock for elapsed:\n%s", give)
	}
	merge := blockBody(t, ir, "after_leak_detect_myTest")
	// stuck count, elapsed, result, hasLeak and delta all merge here, each
	// taking an incoming from the give-up block.
	if n := strings.Count(merge, "%drain_timeout_myTest"); n != 5 {
		t.Errorf("give-up block feeds %d phi incomings, want 5:\n%s", n, merge)
	}
	// The elapsed passed to the result printer must be a phi fed by the give-up
	// block — not the body-only value, which every other path uses.
	m := regexp.MustCompile(`call void @promise_test_print_result\(i8\* %\w+, i32 (%\w+), i64 (%\w+)\)`).
		FindStringSubmatch(merge)
	if m == nil {
		t.Fatalf("result print call not found at the merge:\n%s", merge)
	}
	for _, arg := range []struct{ what, name string }{
		{"result", m[1]},
		{"elapsed", m[2]},
	} {
		def := phiDef(t, merge, arg.name)
		if !strings.Contains(def, "%drain_timeout_myTest") {
			t.Errorf("printed %s (%s) is not merged from the give-up block: %s", arg.what, arg.name, def)
		}
	}
}

// phiDef returns the defining line of the named SSA value within blk.
func phiDef(t *testing.T, blk, name string) string {
	t.Helper()
	for _, line := range strings.Split(blk, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), name+" = ") {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("definition of %s not found in block:\n%s", name, blk)
	return ""
}

// firstPhiOf returns the SSA name of the first phi of the given kind in blk.
func firstPhiOf(t *testing.T, blk, kind string) string {
	t.Helper()
	for _, line := range strings.Split(blk, "\n") {
		if name, ok := phiName(line, kind); ok {
			return name
		}
	}
	t.Fatalf("no %q found in block:\n%s", kind, blk)
	return ""
}

func phiName(line, kind string) (string, bool) {
	i := strings.Index(line, " = "+kind)
	if i < 0 {
		return "", false
	}
	return strings.TrimSpace(line[:i]), true
}

// --- write lengths are derived, never hand-maintained ---------------------

// gepStrRe matches a GEP that takes the address of a char-array string global,
// capturing the SSA name, the array length and the global's name.
var gepStrRe = regexp.MustCompile(`^\t(%\w+) = getelementptr \[(\d+) x i8\], \[\d+ x i8\]\* (@[.\w]+), i32 0, i32 0$`)

// writeConstLenRe matches a pal_write with a compile-time constant length.
var writeConstLenRe = regexp.MustCompile(`call i64 @pal_write\(i32 \d+, i8\* (%\w+), i64 (\d+)\)`)

// T1639 replaced every hand-written byte count at a pal_write call site with
// globalStrLen(global). This pins that invariant across the whole emitted
// module: a constant-length write of a string global must write exactly the
// global's length. A shorter count silently truncates the message; a longer one
// reads past the global. If an emitter ever needs to write only part of a
// global — a NUL-terminated buffer, say — this test is where that exception has
// to be stated out loud rather than sitting as an unexplained magic number.
func TestStringWriteLengthsMatchTheirGlobals(t *testing.T) {
	ir := testMainIR(t, t1639Src, HostTargetTriple(), map[string]int64{"myTest": 2_000_000_000})

	type strRef struct{ name, size string }

	checked, seen := 0, 0
	// SSA names are unique per function, not per module, so each function is
	// scanned with its own map — otherwise %31 in one function is matched
	// against a global GEP'd into %31 in another.
	for _, fn := range strings.Split(ir, "\ndefine ") {
		refs := map[string]strRef{}
		for _, line := range strings.Split(fn, "\n") {
			if m := gepStrRe.FindStringSubmatch(line); m != nil {
				refs[m[1]] = strRef{name: m[3], size: m[2]}
			}
		}
		seen += len(refs)
		for _, line := range strings.Split(fn, "\n") {
			m := writeConstLenRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			ref, ok := refs[m[1]]
			if !ok {
				continue // writing a runtime buffer, not a string global
			}
			checked++
			if m[2] != ref.size {
				t.Errorf("write of %s uses length %s, but the global is %s bytes: %s",
					ref.name, m[2], ref.size, strings.TrimSpace(line))
			}
		}
	}
	if seen < 10 || checked < 10 {
		t.Fatalf("scan matched %d string-global GEPs and %d constant-length writes — too narrow to be meaningful", seen, checked)
	}
}

func TestGlobalStrLen(t *testing.T) {
	m := ir.NewModule()
	g := m.NewGlobalDef(".str.sample", constant.NewCharArrayFromString("  leak: "))
	if got := globalStrLen(g); got != 8 {
		t.Errorf("globalStrLen = %d, want 8 (no NUL terminator is emitted)", got)
	}
	// A non-array global has no literal length to report; 0 is the safe answer
	// (a zero-length write, rather than a bogus count read from another type).
	ng := m.NewGlobalDef("counter", constant.NewInt(irtypes.I64, 0))
	if got := globalStrLen(ng); got != 0 {
		t.Errorf("globalStrLen(non-array) = %d, want 0", got)
	}
}

// mainBody returns the body of the generated @main, so scans below cannot
// accidentally match instructions from a user function.
func mainBody(t *testing.T, ir string) string {
	t.Helper()
	i := strings.Index(ir, "\ndefine i32 @main(")
	if i < 0 {
		t.Fatal("@main not found in generated module")
	}
	body := ir[i+1:]
	if j := strings.Index(body, "\n}\n"); j >= 0 {
		body = body[:j]
	}
	return body
}

// gsFieldGepRe captures the SSA name and field index of a GEP into the
// scheduler global — field 15 is gs_created, field 16 is gs_completed.
var gsFieldGepRe = regexp.MustCompile(`(%\w+) = getelementptr \{[^}]*\}, \{[^}]*\}\* @__promise_sched, i32 0, i32 (\d+)$`)

// gsAtomicRe captures the pointer operand and memory ordering of a relaxed-read
// atomicrmw (the `add ... 0` idiom used to read a counter atomically).
var gsAtomicRe = regexp.MustCompile(`atomicrmw add i64\* (%\w+), i64 0 (\w+)$`)

// B0320: every read of gs_created/gs_completed must be Acquire, so a reader
// that observes the drain complete also observes the exiting goroutine's
// alloc_count decrements. T1639 moved these reads into emitGsOutstanding and
// added two more call sites (the pre-test snapshot and the give-up block) — a
// Monotonic read at any of them reintroduces the false-LEAK race the ordering
// exists to prevent.
func TestGsOutstandingReadsUseAcquireOrdering(t *testing.T) {
	body := mainBody(t, testMainIR(t, t1639Src, HostTargetTriple(), map[string]int64{"myTest": 2_000_000_000}))

	gsFields := map[string]string{} // SSA name → field index
	checked := 0
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if m := gsFieldGepRe.FindStringSubmatch(line); m != nil {
			if m[2] == strconv.Itoa(schedFieldGsCreated) || m[2] == strconv.Itoa(schedFieldGsCompleted) {
				gsFields[m[1]] = m[2]
			}
			continue
		}
		m := gsAtomicRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if _, ok := gsFields[m[1]]; !ok {
			continue // some other counter (alloc_count, ready count)
		}
		checked++
		if m[2] != "acquire" {
			t.Errorf("goroutine-counter read uses %s ordering, want acquire: %s", m[2], line)
		}
	}
	// Pre-test snapshot, fast check, loop check, give-up count — two reads each.
	if checked != 8 {
		t.Errorf("scanned %d goroutine-counter reads, want 8 (4 sites × created+completed)", checked)
	}
}

// The baseline must be sampled *before* the test body runs. Sampled after, it
// would already include the goroutines the body created and the drain would
// return immediately — silently restoring the pre-T0067 behaviour of reading
// alloc_count while goroutines are still freeing.
func TestPreTestSnapshotPrecedesTestBody(t *testing.T) {
	ir := testMainIR(t, t1639Src, HostTargetTriple(), map[string]int64{"myTest": 2_000_000_000})
	body := mainBody(t, ir)

	callIdx := strings.Index(body, "call i32 @promise_test_run(")
	if callIdx < 0 {
		t.Fatal("test body call not found")
	}
	// The baseline is the last `sub i64` before the call — the outstanding
	// count emitted by emitGsOutstanding.
	before := body[:callIdx]
	subIdx := strings.LastIndex(before, " = sub i64 ")
	if subIdx < 0 {
		t.Fatal("no pre-test outstanding snapshot before the test body call")
	}
	name := strings.TrimSpace(before[strings.LastIndex(before[:subIdx], "\n")+1 : subIdx])

	// Both drain comparisons and the give-up count must read that same value —
	// a second snapshot taken later would be a different, useless baseline.
	againstBaseline := regexp.MustCompile(`icmp sle i64 %\w+, ` + regexp.QuoteMeta(name))
	for _, blk := range []string{"leak_check_myTest", "drain_gs_myTest"} {
		if got := blockBody(t, ir, blk); !againstBaseline.MatchString(got) {
			t.Errorf("%s does not compare against the pre-test snapshot %s:\n%s", blk, name, got)
		}
	}
	if got := blockBody(t, ir, "drain_timeout_myTest"); !regexp.MustCompile(`sub i64 %\w+, ` + regexp.QuoteMeta(name)).MatchString(got) {
		t.Errorf("give-up block does not subtract the pre-test snapshot %s — the count would include other tests' goroutines:\n%s", name, got)
	}
}

// Each test samples its own baseline. Sharing one across the batch would carry
// an earlier test's deficit forward permanently instead of self-correcting when
// those goroutines do eventually exit.
func TestEachTestSamplesItsOwnBaseline(t *testing.T) {
	src := "firstTest() `test { }\nsecondTest() `test { }"
	ir := testMainIR(t, src, HostTargetTriple(), map[string]int64{
		"firstTest": 2_000_000_000, "secondTest": 2_000_000_000,
	})

	baselines := map[string]string{}
	for _, name := range []string{"firstTest", "secondTest"} {
		blk := blockBody(t, ir, "leak_check_"+name)
		m := regexp.MustCompile(`icmp sle i64 %\w+, (%\w+)`).FindStringSubmatch(blk)
		if m == nil {
			t.Fatalf("leak_check_%s has no baseline comparison:\n%s", name, blk)
		}
		baselines[name] = m[1]
	}
	if baselines["firstTest"] == baselines["secondTest"] {
		t.Errorf("both tests drain against the same baseline %s — the second test would inherit the first's deficit",
			baselines["firstTest"])
	}
}
