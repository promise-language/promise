// Tests lifted back out of tests/valuetype (T1776): they reach into codegen's
// unexported internals, so they have to be compiled as part of package
// codegen. They use the private helper copies in codegen_helpers_test.go.
package codegen

import (
	"testing"

	irtypes "github.com/llir/llvm/ir/types"
)

// llvmTypeAlign coverage: float, double, pointer, array
func TestLlvmTypeAlignFloat(t *testing.T) {
	if a := llvmTypeAlign(irtypes.Float); a != 4 {
		t.Errorf("float align: got %d, want 4", a)
	}
}

func TestLlvmTypeAlignLargeInt(t *testing.T) {
	// LLVM's default x86_64/aarch64 datalayouts specify no ABI alignment wider
	// than i128, so anything beyond that still aligns to 16 bytes (verified
	// against real `opt`-computed struct offsets, not just LangRef reading).
	// Capping at 8 (the pre-T1419 bug) mis-sized enum variant data blobs and
	// truncated wide-int fields at non-zero offsets.
	if a := llvmTypeAlign(irtypes.NewInt(128)); a != 16 {
		t.Errorf("i128 align: got %d, want 16", a)
	}
	if a := llvmTypeAlign(irtypes.NewInt(256)); a != 16 {
		t.Errorf("i256 align: got %d, want 16", a)
	}
	if a := llvmTypeAlign(irtypes.NewInt(512)); a != 16 {
		t.Errorf("i512 align: got %d, want 16", a)
	}
}

func TestLlvmTypeSizeFloat(t *testing.T) {
	if sz := llvmTypeSize(irtypes.Float); sz != 4 {
		t.Errorf("float size: got %d, want 4", sz)
	}
	if sz := llvmTypeSize(irtypes.Double); sz != 8 {
		t.Errorf("double size: got %d, want 8", sz)
	}
}
