package codegen

import "strings"

// runtimeModuleName is the synthetic module prefix used to group the
// codegen-emitted runtime helpers into a single separately-compiled IR module
// (T1089). These helpers (scheduler, coroutine, PAL, netpoll, string/vector
// helpers) are emitted unconditionally and their bodies are program-independent,
// so splitting them out of the per-program main IR lets them be compiled once
// and reused across compiles via the existing module content cache — exactly the
// way `__mod_std` already is. The leading "__" keeps it from colliding with any
// real module IR prefix.
const runtimeModuleName = "__runtime"

// userMainBodyFunc is the one runtime-prefixed function that is NOT
// program-independent: wrapMainWithScheduler compiles a failable `main()` body
// into @__promise_main_body, which holds user code. It must stay in the main IR.
const userMainBodyFunc = "__promise_main_body"

// isRuntimeFunc returns true for hand-written runtime IR helpers identified by
// reserved name prefixes that user-facing codegen never produces (user types and
// module functions are mangled with their own prefixes). Used both to exclude
// these helpers from the B0314 alloca-domination check (they have their own
// control flow with non-entry allocas) and to group them into the __runtime
// module (T1089).
func isRuntimeFunc(name string) bool {
	for _, p := range []string{
		"promise_", "__promise_", "pal_", "llvm.", "strlen", "memcpy", "memset",
	} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// tagRuntimeFuncs assigns every emitted runtime helper (a function with a body
// matching isRuntimeFunc) to the synthetic __runtime module so SplitModuleIRs
// moves its body into a cacheable module IR (T1089). Must run after all codegen
// (including wrapMainWithScheduler), before the IR is split.
//
// Excluded: @__promise_main_body (holds user code, see userMainBodyFunc) and any
// function already owned by a real module or a generic instance (those keep their
// owner — their bodies live in the module/instance .bc).
func (c *Compiler) tagRuntimeFuncs() {
	for _, fn := range c.module.Funcs {
		if len(fn.Blocks) == 0 {
			continue // declaration only — nothing to move
		}
		name := fn.Name()
		if name == userMainBodyFunc || !isRuntimeFunc(name) {
			continue
		}
		if _, owned := c.moduleOwnedFuncs[name]; owned {
			continue
		}
		if _, inst := c.instanceOwnedFuncs[name]; inst {
			continue
		}
		c.moduleOwnedFuncs[name] = runtimeModuleName
	}
}
