package codegen

import (
	"fmt"
	"strings"
	"testing"
)

func TestCoverageFunctionEntry(t *testing.T) {
	ir, regions := generateIRWithCoverage(t, `
		foo() int { return 42; }
		bar() int { return 7; }
		main() {}
	`)
	// Should have coverage globals for foo and bar (not main)
	assertContains(t, ir, "@__promise_cov_0")
	assertContains(t, ir, "@__promise_cov_1")

	// Should have 2 function regions
	funcCount := 0
	for _, r := range regions {
		if r.Kind == "function" {
			funcCount++
		}
	}
	if funcCount != 2 {
		t.Errorf("expected 2 function coverage regions, got %d (regions: %+v)", funcCount, regions)
	}
}

func TestCoverageSkipsTestFunctions(t *testing.T) {
	_, regions := generateIRWithCoverage(t, `
		foo() int { return 42; }
		test_foo() `+"`test"+` {
			int x = foo();
		}
		main() {}
	`)
	// Only foo should be instrumented, not test_foo or main
	for _, r := range regions {
		if r.FuncName == "test_foo" {
			t.Errorf("test function should not be instrumented: %+v", r)
		}
		if r.FuncName == "main" {
			t.Errorf("main should not be instrumented: %+v", r)
		}
	}
	funcCount := 0
	for _, r := range regions {
		if r.Kind == "function" {
			funcCount++
		}
	}
	if funcCount != 1 {
		t.Errorf("expected 1 function region (foo), got %d", funcCount)
	}
}

func TestCoverageIfBranches(t *testing.T) {
	ir, regions := generateIRWithCoverage(t, `
		classify(int x) string {
			if x > 0 {
				return "positive";
			} else {
				return "negative";
			}
		}
		main() {}
	`)
	// Should have coverage counter increments in IR
	assertContains(t, ir, "@__promise_cov_0")
	assertContains(t, ir, "@__promise_cov_1") // if.then
	assertContains(t, ir, "@__promise_cov_2") // if.else

	thenCount := 0
	elseCount := 0
	for _, r := range regions {
		if r.Kind == "if.then" {
			thenCount++
		}
		if r.Kind == "if.else" {
			elseCount++
		}
	}
	if thenCount != 1 {
		t.Errorf("expected 1 if.then region, got %d", thenCount)
	}
	if elseCount != 1 {
		t.Errorf("expected 1 if.else region, got %d", elseCount)
	}
}

func TestCoverageWhileLoop(t *testing.T) {
	_, regions := generateIRWithCoverage(t, `
		count(int n) int {
			int i = 0;
			while i < n {
				i++;
			}
			return i;
		}
		main() {}
	`)
	whileCount := 0
	for _, r := range regions {
		if r.Kind == "while.body" {
			whileCount++
		}
	}
	if whileCount != 1 {
		t.Errorf("expected 1 while.body region, got %d", whileCount)
	}
}

func TestCoverageDisabledByDefault(t *testing.T) {
	ir := generateIR(t, `
		foo() int { return 42; }
		main() {}
	`)
	if strings.Contains(ir, "__promise_cov_") {
		t.Error("coverage globals should not be emitted when coverage is disabled")
	}
}

func TestCoverageMethodEntry(t *testing.T) {
	_, regions := generateIRWithCoverage(t, `
		type Counter {
			int value;
			increment(~this) {
				this.value++;
			}
			get_value(this) int {
				return this.value;
			}
		}
		main() {}
	`)
	methodCount := 0
	for _, r := range regions {
		if r.Kind == "method" {
			methodCount++
		}
	}
	if methodCount < 2 {
		t.Errorf("expected at least 2 method coverage regions, got %d", methodCount)
	}
}

func TestCoverageTestMainEmitsMarkers(t *testing.T) {
	file, info := parseWithStd(t, `
		foo() int { return 42; }
		test_foo() `+"`test"+` {
			int x = foo();
		}
	`)
	result := CompileWithOptions(file, info, "", &CompileOptions{CoverageEnabled: true})
	result.GenerateTestMain(info.Tests, nil)
	ir := result.Module.String()

	// GenerateTestMain should emit coverage output markers
	assertContains(t, ir, "===PROMISE_COV===")
	assertContains(t, ir, "===END_COV===")
	// Should contain the coverage counter global
	assertContains(t, ir, "@__promise_cov_0")
	// Should have exactly 1 coverage region (foo, not test_foo)
	if len(result.CoverageRegions) != 1 {
		t.Errorf("expected 1 coverage region, got %d: %+v", len(result.CoverageRegions), result.CoverageRegions)
	}
}

func TestCoverageClassicForLoop(t *testing.T) {
	_, regions := generateIRWithCoverage(t, `
		sum(int n) int {
			int total = 0;
			for int i = 0; i < n; i++ {
				total += i;
			}
			return total;
		}
		main() {}
	`)
	forCount := 0
	for _, r := range regions {
		if r.Kind == "for.body" {
			forCount++
		}
	}
	if forCount != 1 {
		t.Errorf("expected 1 for.body region, got %d", forCount)
	}
}

func TestCoverageInfiniteLoop(t *testing.T) {
	_, regions := generateIRWithCoverage(t, `
		run() int {
			int i = 0;
			for {
				i++;
				if i >= 5 {
					break;
				}
			}
			return i;
		}
		main() {}
	`)
	loopCount := 0
	for _, r := range regions {
		if r.Kind == "loop.body" {
			loopCount++
		}
	}
	if loopCount != 1 {
		t.Errorf("expected 1 loop.body region, got %d", loopCount)
	}
}

func TestCoverageEnumMatchArms(t *testing.T) {
	_, regions := generateIRWithCoverage(t, `
		enum Color { Red, Green, Blue }
		name(Color c) string {
			return match c {
				Color.Red => "red",
				Color.Green => "green",
				Color.Blue => "blue",
			};
		}
		main() {}
	`)
	armCount := 0
	for _, r := range regions {
		if r.Kind == "match.arm" {
			armCount++
		}
	}
	if armCount != 3 {
		t.Errorf("expected 3 match.arm regions, got %d", armCount)
	}
}

// TestCoverageGenericMethodInstanceShared is the T0574 regression test.
// A monomorphized generic method's body lives in a per-instance .bc while the
// coverage reporter reads the counter from the main IR's test main. The counter
// global must therefore be a single externally-linked symbol: defined once in
// the main IR and an external declaration (with the increment) in the instance
// .bc. Private linkage would split it into independent per-translation-unit
// copies, so the always-zero main copy would be read → "not covered" (the bug).
func TestCoverageGenericMethodInstanceShared(t *testing.T) {
	file, info := parseWithStd(t, `
		type Box[T] {
			T x;
			inc(this) T { return this.x; }
		}
		test_inc() `+"`test"+` {
			b := Box[int](x: 7);
			int r = b.inc();
			assert(r == 7, "expected 7");
		}
	`)
	result := CompileWithOptions(file, info, "", &CompileOptions{CoverageEnabled: true})
	result.GenerateTestMain(info.Tests, nil)

	// Locate the coverage region for the monomorphized method Box[int].inc.
	idx := -1
	for i, r := range result.CoverageRegions {
		if r.FuncName == "Box[int].inc" && r.Kind == "method" {
			idx = i
			break
		}
	}
	if idx == -1 {
		t.Fatalf("no coverage region for Box[int].inc method; regions: %+v", result.CoverageRegions)
	}
	g := fmt.Sprintf("@__promise_cov_%d", idx)

	// Main IR keeps the single externally-visible definition (not private).
	mainIR, _ := result.SplitModuleIRs()
	assertContains(t, mainIR, g+" = global i64 0")
	assertNotContains(t, mainIR, g+" = private global")

	// The Box[int] instance .bc owns the method body (with the increment) and
	// references the counter as an external declaration — no private/own copy.
	instIRs := result.InstanceIRs()
	instIR, ok := instIRs["Box[int]"]
	if !ok {
		t.Fatalf("missing Box[int] in instance IRs, keys: %v", mapKeys(instIRs))
	}
	assertContains(t, instIR, g+" = external global")
	// The increment (load/add/store) must land in the instance .bc.
	hasStore := false
	for _, line := range strings.Split(instIR, "\n") {
		if strings.Contains(line, "store") && strings.Contains(line, g) {
			hasStore = true
			break
		}
	}
	if !hasStore {
		t.Errorf("Box[int] instance IR must contain the coverage increment store to %s (T0574)", g)
	}
	// No duplicate / private definition in the instance .bc.
	assertNotContains(t, instIR, g+" = global i64 0")
	assertNotContains(t, instIR, g+" = private global")
}

// TestCompileResultCoverageEnabled locks the T0574 cache-isolation contract at
// the accessor level. compileAndLinkSeparate appends "+cov" to the instance
// build-mode iff result.CoverageEnabled() is true, keeping coverage and
// non-coverage instance .bc files in separate build-cache entries (externally-
// linked counter globals would otherwise cause undefined-symbol link errors or
// silent undercount across the two build kinds). The accessor must therefore
// faithfully reflect CompileOptions.CoverageEnabled — including the default
// (nil opts / Compile) case, which must report false.
func TestCompileResultCoverageEnabled(t *testing.T) {
	src := `
		foo() int { return 42; }
		main() {}
	`
	file, info := parseWithStd(t, src)
	if got := CompileWithOptions(file, info, "", &CompileOptions{CoverageEnabled: true}).CoverageEnabled(); !got {
		t.Errorf("CoverageEnabled() = false, want true when CompileOptions.CoverageEnabled is set")
	}

	file2, info2 := parseWithStd(t, src)
	if got := CompileWithOptions(file2, info2, "", &CompileOptions{CoverageEnabled: false}).CoverageEnabled(); got {
		t.Errorf("CoverageEnabled() = true, want false when CompileOptions.CoverageEnabled is explicitly false")
	}

	file3, info3 := parseWithStd(t, src)
	if got := Compile(file3, info3, "").CoverageEnabled(); got {
		t.Errorf("CoverageEnabled() = true, want false for a default (nil opts) compile")
	}
}
