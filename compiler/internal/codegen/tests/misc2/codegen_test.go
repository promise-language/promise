package misc2

import (
	"testing"

	"github.com/promise-language/promise/compiler/internal/codegen"
	"github.com/promise-language/promise/compiler/internal/codegen/codegentest"
)

// --- Stage 9: Std library and test runner codegen tests ---

// compileResultWithStd merges stdSrc (extra user-level declarations) with userSrc and compiles.
func compileResultWithStd(t *testing.T, stdSrc, userSrc string) *codegen.CompileResult {
	t.Helper()
	combined := userSrc
	if stdSrc != "" {
		combined = stdSrc + "\n" + userSrc
	}
	return codegentest.CompileResult(t, combined)
}

// --- Cross-Module Codegen Tests ---

// --- Catalog Module Tests ---

// --- InstanceIRs, instanceOwnedFuncs, CompileWithCache tests ---

// --- B0005: String constant private linkage tests ---
// All string constant globals must use LinkagePrivate so each split .bc file
// (module, instance) contains its own copy and doesn't depend on main-IR string
// numbering. This prevents stale cache entries from causing linker errors.

// --- Coverage instrumentation tests (T0030) ---
