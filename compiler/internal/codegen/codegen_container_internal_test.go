// Tests lifted back out of tests/container (T1776): they reach into codegen's
// unexported internals, so they have to be compiled as part of package
// codegen. They use the private helper copies in codegen_helpers_test.go.
package codegen

import (
	"testing"

	irtypes "github.com/llir/llvm/ir/types"
)

func TestLlvmTypeAlignArray(t *testing.T) {
	arr := irtypes.NewArray(10, irtypes.I32)
	if a := llvmTypeAlign(arr); a != 4 {
		t.Errorf("[10 x i32] align: got %d, want 4", a)
	}
}

func TestLlvmTypeSizeArray(t *testing.T) {
	arr := irtypes.NewArray(5, irtypes.I32)
	if sz := llvmTypeSize(arr); sz != 20 {
		t.Errorf("[5 x i32] size: got %d, want 20", sz)
	}
}
