package codegen

import (
	"strings"
	"testing"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
)

// T1929 added verifyNoBodylessTypeInfoDrops as the build-time guard that a
// typeinfo's drop_fn_ptr never names a function that was declared and never
// defined. Such a pointer is an undefined symbol only some linkers diagnose —
// ld.lld drops the unreferenced typeinfo section under --gc-sections first,
// lld-link resolves before /OPT:REF and fails the build — so the defect stayed
// latent on Linux and macOS while breaking Windows.
//
// After the fix no program in the tree emits one, so the guard is exercised
// here directly: a guard that cannot fire protects nothing, and the skip
// branches are where it would stop firing by accident.
//
// The behaviour of the fix is pinned in
// compiler/internal/codegen/tests/module/t1929_test.go (IR shape) and in
// tests/modules/module_mutex_guard_local_test.pr (link + runtime).

// declaredFunc returns a function stub with no body — what declareMonoMethods
// produces before a define pass fills it in.
func declaredFunc(m *ir.Module, name string) *ir.Func {
	return m.NewFunc(name, irtypes.Void, ir.NewParam("this", irtypes.I8Ptr))
}

// definedFunc returns a function that has a body, as a completed define pass
// leaves it.
func definedFunc(m *ir.Module, name string) *ir.Func {
	fn := declaredFunc(m, name)
	fn.NewBlock(".entry").NewRet(nil)
	return fn
}

// A typeinfo drop pointer still aimed at a body-less function once every body
// has been emitted must abort the build, naming the global, the function and
// the type so the reader is not left with a link error from a different tool.
func TestVerifyNoBodylessTypeInfoDropsPanics(t *testing.T) {
	m := ir.NewModule()
	c := &Compiler{}
	c.recordTypeInfoDropRef("promise_typeinfo_MutexGuard[int]", "MutexGuard[int]",
		"MutexGuard[int]", declaredFunc(m, "MutexGuard[int].drop"))

	msg := recoverPanic(c.verifyNoBodylessTypeInfoDrops)
	if msg == "" {
		t.Fatal("expected verifyNoBodylessTypeInfoDrops to panic on a body-less drop target")
	}
	for _, want := range []string{
		"promise_typeinfo_MutexGuard[int]", "MutexGuard[int].drop", "T1929",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("panic message missing %q, got: %s", want, msg)
		}
	}
}

// The ordinary case: the drop function got its body from some later define
// pass, which is exactly why the check cannot run at emission time.
func TestVerifyNoBodylessTypeInfoDropsAcceptsDefined(t *testing.T) {
	m := ir.NewModule()
	c := &Compiler{}
	c.recordTypeInfoDropRef("promise_typeinfo_Ref[int]", "Ref[int]", "Ref[int]",
		definedFunc(m, "Ref[int].drop"))

	if msg := recoverPanic(c.verifyNoBodylessTypeInfoDrops); msg != "" {
		t.Fatalf("expected no panic for a defined drop target, got: %s", msg)
	}
}

// A cached mono instance is exempt: defineMonoMethods deliberately skips body
// generation for it because the body already lives in its pre-built .bc, so the
// stub in this module being empty is correct rather than a dangling reference.
func TestVerifyNoBodylessTypeInfoDropsSkipsCachedInstance(t *testing.T) {
	m := ir.NewModule()
	c := &Compiler{cachedInstances: map[string]bool{"Box[int]": true}}
	c.recordTypeInfoDropRef("promise_typeinfo_Box[int]", "Box[int]", "Box[int]",
		declaredFunc(m, "Box[int].drop"))

	if msg := recoverPanic(c.verifyNoBodylessTypeInfoDrops); msg != "" {
		t.Fatalf("expected no panic for a cached instance, got: %s", msg)
	}
}

// The cache exemption is keyed on the instance name, so a non-generic type
// (recorded with an empty instName) is never exempted by an unrelated cache hit.
func TestVerifyNoBodylessTypeInfoDropsChecksNonGenericDespiteCache(t *testing.T) {
	m := ir.NewModule()
	c := &Compiler{cachedInstances: map[string]bool{"": true, "Box[int]": true}}
	c.recordTypeInfoDropRef("promise_typeinfo_Logger", "", "Logger",
		declaredFunc(m, "Logger.drop"))

	if msg := recoverPanic(c.verifyNoBodylessTypeInfoDrops); msg == "" {
		t.Fatal("expected a non-generic body-less drop target to still panic")
	}
}
