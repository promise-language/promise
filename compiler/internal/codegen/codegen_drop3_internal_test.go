// Tests lifted back out of tests/drop3 (T1776): they reach through
// codegen's exported API into unexported state, so they have to be compiled
// as part of package codegen. They use the private helper copies in
// codegen_helpers_test.go.
package codegen

import (
	"strings"
	"testing"
)

func TestInstanceOwnedFuncsTracking(t *testing.T) {
	// instanceOwnedFuncs should map Box[int]'s mangled methods to "Box[int]".
	file, info := parseWithStd(t, boxWithGetMethod+`
		main() {
			b := Box[int](value: 1);
			int x = b.get();
		}
	`)
	result := Compile(file, info, "")
	c := result.compiler

	if len(c.instanceOwnedFuncs) == 0 {
		t.Fatal("expected non-empty instanceOwnedFuncs")
	}

	foundBoxInt := false
	for funcName, instName := range c.instanceOwnedFuncs {
		if instName == "Box[int]" {
			foundBoxInt = true
			if !strings.Contains(funcName, "Box[int]") {
				t.Errorf("function %q tagged as Box[int] but name doesn't contain 'Box[int]'", funcName)
			}
		}
	}
	if !foundBoxInt {
		t.Errorf("no function owned by Box[int]; instanceOwnedFuncs = %v", c.instanceOwnedFuncs)
	}
}
