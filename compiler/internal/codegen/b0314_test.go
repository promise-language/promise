package codegen

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// isRuntimeFunc (the classifier used here to skip hand-written runtime helpers)
// lives in runtime.go — shared with the T1089 __runtime module tagging pass.

// B0314: Codegen must place alloca instructions in blocks that dominate all
// uses, so `opt -O0` verification passes. Previously ~45 sites used
// c.block.NewAlloca() which placed allocas in whatever block was current
// during codegen (often a conditional branch block), masked only by
// mem2reg/sroa at -O1.
//
// The rule we enforce: every alloca in a function must appear in an
// "entry-equivalent" block — the first labeled block for normal functions,
// or the preamble blocks (.entry, coro.alloc, coro.start, coro.init.suspend)
// for presplit coroutines (goroutines and generators).

var (
	b0314FnHeader = regexp.MustCompile(`^define [^@]*@([^\s(]+)\s*\(`)
	b0314BlockHdr = regexp.MustCompile(`^([A-Za-z_.][A-Za-z0-9_.]*):\s*(?:;.*)?$`)
	b0314Alloca   = regexp.MustCompile(`^\s*%[^\s=]+\s*=\s*alloca\b`)
)

func assertAllocasDominate(t *testing.T, ir string) {
	t.Helper()
	lines := strings.Split(ir, "\n")
	var (
		inFunc        bool
		fnName        string
		presplit      bool
		currentBlock  string
		firstBlock    string
		allowedBlocks map[string]bool
		violations    []string
	)
	reset := func() {
		inFunc = false
		fnName = ""
		presplit = false
		currentBlock = ""
		firstBlock = ""
		allowedBlocks = nil
	}
	for i, ln := range lines {
		if !inFunc {
			if m := b0314FnHeader.FindStringSubmatch(ln); m != nil && strings.Contains(ln, "{") {
				inFunc = true
				fnName = m[1]
				presplit = strings.Contains(ln, "presplitcoroutine")
				firstBlock = ""
				allowedBlocks = map[string]bool{}
			}
			continue
		}
		if strings.HasPrefix(ln, "}") {
			reset()
			continue
		}
		if m := b0314BlockHdr.FindStringSubmatch(ln); m != nil {
			currentBlock = m[1]
			if firstBlock == "" {
				firstBlock = currentBlock
				allowedBlocks[currentBlock] = true
			}
			if presplit {
				switch currentBlock {
				case "coro.alloc", "coro.start", "coro.init.suspend":
					allowedBlocks[currentBlock] = true
				}
			}
			continue
		}
		if b0314Alloca.MatchString(ln) {
			if isRuntimeFunc(fnName) {
				continue
			}
			if !allowedBlocks[currentBlock] {
				violations = append(violations,
					fnName+":"+currentBlock+" (line "+strconv.Itoa(i+1)+"): "+strings.TrimSpace(ln))
			}
		}
	}
	if len(violations) > 0 {
		t.Fatalf("alloca outside entry-equivalent block in %d site(s):\n  %s",
			len(violations), strings.Join(violations, "\n  "))
	}
}

func TestB0314AllocasDominateGoroutineChannel(t *testing.T) {
	// Goroutine + channel — the original bug repro (channel_basic.pr pattern).
	ir := generateIR(t, `
		main() {
			ch := channel[int](capacity: 5);
			go {
				ch.send(42);
			};
			r := <-ch;
			if v := r {
			}
		}
	`)
	assertAllocasDominate(t, ir)
}

func TestB0314AllocasDominateSelect(t *testing.T) {
	// Select creates SWN and recv-value allocas.
	ir := generateIR(t, `
		main() {
			a := channel[int](capacity: 1);
			b := channel[int](capacity: 1);
			a.send(1);
			select {
				v := <-a:
					if x := v { }
				w := <-b:
					if y := w { }
			}
		}
	`)
	assertAllocasDominate(t, ir)
}

func TestB0314AllocasDominateGeneratorForIn(t *testing.T) {
	// Generator for-in: handle/slot/elem/index allocas used to land in the
	// for-in body block instead of coro.start.
	ir := generateIR(t, `
		counter(int n) stream[int] {
			int i = 0;
			while i < n {
				yield i;
				i = i + 1;
			}
		}
		main() {
			int total = 0;
			for v in counter(5) {
				total = total + v;
			}
		}
	`)
	assertAllocasDominate(t, ir)
}

func TestB0314AllocasDominateForInVector(t *testing.T) {
	ir := generateIR(t, `
		main() {
			int[] xs = [1, 2, 3];
			int sum = 0;
			for i, v in xs {
				sum = sum + v + i;
			}
		}
	`)
	assertAllocasDominate(t, ir)
}

func TestB0314AllocasDominateForInMap(t *testing.T) {
	ir := generateIR(t, `
		main() {
			map[string, int] m = map[string, int]();
			m["a"] = 1;
			m["b"] = 2;
			int sum = 0;
			for k, v in m {
				sum = sum + v;
			}
		}
	`)
	assertAllocasDominate(t, ir)
}

func TestB0314AllocasDominateFailableCall(t *testing.T) {
	// The canonical repro — `%result = alloca { i1, i64 }` for failable call
	// results was landing in panic.ok.N blocks instead of entry.
	ir := generateIR(t, `
		divide!(int a, int b) int {
			if b == 0 { raise error(message: "div by zero"); }
			return a / b;
		}
		main!() {
			x := divide(10, 2);
			if x > 0 {
				y := divide(x, 3);
			}
		}
	`)
	assertAllocasDominate(t, ir)
}

func TestB0314AllocasDominateMatchPattern(t *testing.T) {
	// Match arm pattern-value allocas.
	ir := generateIR(t, `
		enum Shape {
			Circle(f64 r),
			Square(f64 s),
		}
		area(Shape sh) f64 {
			f64 result = 0.0;
			match sh {
				Shape.Circle(r) => { result = r * r * 3.14; },
				Shape.Square(s) => { result = s * s; },
			}
			return result;
		}
		main() {
			f64 a = area(Shape.Circle(r: 2.0));
		}
	`)
	assertAllocasDominate(t, ir)
}
