// Package pal provides a Platform Abstraction Layer that emits platform-specific
// LLVM IR functions into the module. All OS interactions (write, exit) go through
// PAL so the compiler can target different platforms by swapping implementations.
package pal

import (
	"strings"

	"github.com/llir/llvm/ir"
)

// PAL emits platform-specific LLVM IR functions into the module.
type PAL interface {
	// EmitWrite defines @pal_write(i32 %fd, i8* %buf, i64 %len) → i64
	EmitWrite(module *ir.Module) *ir.Func
	// EmitExit defines @pal_exit(i32 %code) → void [noreturn]
	EmitExit(module *ir.Module) *ir.Func
}

// ForTarget returns a PAL implementation for the given LLVM target triple.
func ForTarget(triple string) PAL {
	switch {
	case strings.Contains(triple, "windows"):
		return &WindowsPAL{}
	case strings.Contains(triple, "wasm"):
		return &WasmPAL{}
	default:
		return &PosixPAL{}
	}
}
