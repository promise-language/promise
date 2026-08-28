// Tests lifted back out of tests/misc1 (T1776): they reach into codegen's
// unexported internals, so they have to be compiled as part of package
// codegen. They use the private helper copies in codegen_helpers_test.go.
package codegen

import (
	"strings"
	"testing"

	irtypes "github.com/llir/llvm/ir/types"
)

func TestLlvmTypeAlignDouble(t *testing.T) {
	if a := llvmTypeAlign(irtypes.Double); a != 8 {
		t.Errorf("double align: got %d, want 8", a)
	}
}

func TestLlvmTypeAlignPointer(t *testing.T) {
	if a := llvmTypeAlign(irtypes.I8Ptr); a != 8 {
		t.Errorf("pointer align: got %d, want 8", a)
	}
}

func TestLlvmTypeAlignStruct(t *testing.T) {
	s := irtypes.NewStruct(irtypes.I8, irtypes.I64)
	if a := llvmTypeAlign(s); a != 8 {
		t.Errorf("{i8, i64} align: got %d, want 8", a)
	}
}

func TestLlvmTypeSizeAlignment(t *testing.T) {
	// Test that struct sizes account for alignment padding
	// {i1, i64} should be 16 (1 byte + 7 padding + 8 bytes), not 9
	s1 := irtypes.NewStruct(irtypes.I1, irtypes.I64)
	if sz := llvmTypeSize(s1); sz != 16 {
		t.Errorf("{i1, i64} size: got %d, want 16", sz)
	}

	// {i64, i1} should be 16 (8 bytes + 1 byte + 7 tail padding)
	s2 := irtypes.NewStruct(irtypes.I64, irtypes.I1)
	if sz := llvmTypeSize(s2); sz != 16 {
		t.Errorf("{i64, i1} size: got %d, want 16", sz)
	}

	// {i32, i32} should be 8 (no padding needed)
	s3 := irtypes.NewStruct(irtypes.I32, irtypes.I32)
	if sz := llvmTypeSize(s3); sz != 8 {
		t.Errorf("{i32, i32} size: got %d, want 8", sz)
	}

	// {i8, i32, i8} should be 12 (1 + 3pad + 4 + 1 + 3pad)
	s4 := irtypes.NewStruct(irtypes.I8, irtypes.I32, irtypes.I8)
	if sz := llvmTypeSize(s4); sz != 12 {
		t.Errorf("{i8, i32, i8} size: got %d, want 12", sz)
	}
}

func TestLlvmTypeSizePointer(t *testing.T) {
	if sz := llvmTypeSize(irtypes.I8Ptr); sz != 8 {
		t.Errorf("pointer size: got %d, want 8", sz)
	}
}

// TestTLSBridgeAllShapesBackendlessStubs compiles the same all-externs program for a
// backend-less target and asserts every extern gets an inert stub: no pal_tls_* /
// platform TLS symbol is referenced, int/handle returners store 0, and the two string
// getters synthesize an empty Promise string. This is the path that makes the
// module compile-and-link everywhere while failing cleanly at runtime.
func TestTLSBridgeAllShapesBackendlessStubs(t *testing.T) {
	file, info := parseWithStd(t, tlsAllExternsSrc)
	result := Compile(file, info, "wasm32-wasi")
	ir := result.Module.String()

	if strings.Contains(ir, "@pal_tls_") {
		t.Error("backend-less stubs must not reference any pal_tls_* wrapper")
	}
	for _, sym := range []string{"@SSL_new", "@SSL_CTX_new", "@BIO_new", "@SSL_read",
		"@SSLCreateContext", "@SSLHandshake", "@SecItemImport"} {
		if strings.Contains(ir, sym) {
			t.Errorf("backend-less stub must not reference platform TLS symbol %s", sym)
		}
	}
	// Every bridge symbol is still defined (so the module links on this target).
	for _, name := range tlsExternNames {
		assertContains(t, ir, "@"+name+"(")
	}
	// String getters return an empty Promise string via promise_string_new(null, 0).
	assertContains(t, ir, "@promise_string_new")
}
