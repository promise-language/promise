package codegen

import (
	"fmt"
	"strings"
	"testing"
)

// checkNarrowAllocasInBranchBlocks verifies no allocas exist in narrowing/unwrap
// BRANCH blocks (narrow.then, narrow.else, narrow.check, ifunwrap.then).
// These blocks are conditional — allocas there may not dominate all uses.
// The merge block (narrow.end, ifunwrap.end) is OK for regular variable decls
// since it's a linear continuation point that dominates subsequent code.
func checkNarrowAllocasInBranchBlocks(t *testing.T, ir string, funcPattern string) {
	t.Helper()
	lines := strings.Split(ir, "\n")
	inFunc := false
	currentBlock := ""
	var problems []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(line, "define") && strings.Contains(line, funcPattern) {
			inFunc = true
			currentBlock = ".entry"
			continue
		}
		if inFunc {
			if trimmed == "}" {
				break
			}
			if strings.HasSuffix(trimmed, ":") && !strings.HasPrefix(trimmed, ";") {
				currentBlock = trimmed
			}
			if strings.Contains(trimmed, "= alloca ") {
				// Flag allocas in narrowing branch blocks (not merge blocks)
				isBranch := strings.Contains(currentBlock, "narrow.then") ||
					strings.Contains(currentBlock, "narrow.else") ||
					strings.Contains(currentBlock, "narrow.check") ||
					strings.Contains(currentBlock, "ifunwrap.then")
				if isBranch {
					problems = append(problems, fmt.Sprintf("%s in block %s", trimmed, currentBlock))
				}
			}
		}
	}

	if len(problems) > 0 {
		for _, p := range problems {
			t.Errorf("B0153: non-entry alloca: %s", p)
		}
		t.Error("All narrowing/unwrap allocas must be in the entry block to avoid LLVM dominance errors")
	}
}

func TestIsAbsentPostNarrowEntryBlockAlloca(t *testing.T) {
	// B0153: Post-divergence narrowing in module method.
	ir := generateIRWithModule(t, "mylib",
		`
		type Calc `+"`"+`public {
			int x;
			compute(int? factor) int `+"`"+`public {
				if factor is absent {
					return this.x;
				}
				return this.x * factor;
			}
		}
		`,
		`
		use mylib "./mylib";
		main() {
			c := mylib.Calc(x: 10);
			int a = c.compute();
			int b = c.compute(factor: 3);
		}
		`,
	)
	checkNarrowAllocasInBranchBlocks(t, ir, "__mod_mylib_Calc.compute")
}

func TestIsAbsentNarrowInThenBlock(t *testing.T) {
	// Non-negated narrowing: `if opt { ... }` — alloca must be in entry block.
	ir := generateIR(t, `
		test(int? x) int {
			if x {
				return x;
			}
			return 0;
		}
		main() {}
	`)
	checkNarrowAllocasInBranchBlocks(t, ir, "@test(")
}

func TestIsAbsentNarrowWithLoop(t *testing.T) {
	// Narrowed variable used inside a loop — the alloca must dominate the loop.
	ir := generateIRWithModule(t, "mylib",
		`
		type Repeater `+"`"+`public {
			int value;
			repeat(int? count) int `+"`"+`public {
				if count is absent {
					return this.value;
				}
				int sum = 0;
				int i = 0;
				while i < count {
					sum = sum + this.value;
					i = i + 1;
				}
				return sum;
			}
		}
		`,
		`
		use mylib "./mylib";
		main() {
			r := mylib.Repeater(value: 5);
			int a = r.repeat();
			int b = r.repeat(count: 3);
		}
		`,
	)
	checkNarrowAllocasInBranchBlocks(t, ir, "__mod_mylib_Repeater.repeat")
}

func TestIfUnwrapAllocaPlacement(t *testing.T) {
	// If-unwrap allocas must be in entry block.
	ir := generateIR(t, `
		test(int? x) int {
			if val := x {
				return val;
			}
			return 0;
		}
		main() {}
	`)
	checkNarrowAllocasInBranchBlocks(t, ir, "@test(")
}

func TestIsAbsentNegatedElseNarrowing(t *testing.T) {
	// Negated narrowing with else: `if x is absent { } else { use x }`
	ir := generateIR(t, `
		test(int? x) int {
			if x is absent {
				return 0;
			} else {
				return x;
			}
		}
		main() {}
	`)
	checkNarrowAllocasInBranchBlocks(t, ir, "@test(")
}

func TestCompoundNarrowingAllocaPlacement(t *testing.T) {
	// Compound narrowing: `if a && b { ... }`
	ir := generateIR(t, `
		test(int? a, int? b) int {
			if a && b {
				return a + b;
			}
			return 0;
		}
		main() {}
	`)
	checkNarrowAllocasInBranchBlocks(t, ir, "@test(")
}

func TestWhileUnwrapAllocaPlacement(t *testing.T) {
	// While-unwrap allocas must be in entry block too.
	ir := generateIR(t, `
		test() {
			int? x = 42;
			while val := x {
				x = none;
			}
		}
		main() {}
	`)

	// Check no allocas in whileunwrap.body block
	lines := strings.Split(ir, "\n")
	inFunc := false
	currentBlock := ""
	var problems []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(line, "define") && strings.Contains(line, "@test(") {
			inFunc = true
			currentBlock = ".entry"
			continue
		}
		if inFunc {
			if trimmed == "}" {
				break
			}
			if strings.HasSuffix(trimmed, ":") && !strings.HasPrefix(trimmed, ";") {
				currentBlock = trimmed
			}
			if strings.Contains(trimmed, "= alloca ") && strings.Contains(currentBlock, "whileunwrap.body") {
				problems = append(problems, fmt.Sprintf("%s in block %s", trimmed, currentBlock))
			}
		}
	}

	if len(problems) > 0 {
		for _, p := range problems {
			t.Errorf("B0153: non-entry alloca: %s", p)
		}
		t.Error("While-unwrap allocas must be in the entry block")
	}
}
