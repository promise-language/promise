package codegen

import (
	"fmt"

	"github.com/promise-language/promise/compiler/internal/sema"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
)

// ModuleInfos returns the module info map from sema, keyed by module path.
func (r *CompileResult) ModuleInfos() map[string]*sema.ModuleInfo {
	return r.compiler.info.ModuleInfos
}

// HasModules returns true if the compile result contains separate module code.
func (r *CompileResult) HasModules() bool {
	return len(r.compiler.moduleOwnedFuncs) > 0
}

// ModuleNames returns the names of modules that have compiled code.
func (r *CompileResult) ModuleNames() []string {
	seen := make(map[string]bool)
	var names []string
	for _, modName := range r.compiler.moduleOwnedFuncs {
		if !seen[modName] {
			seen[modName] = true
			names = append(names, modName)
		}
	}
	return names
}

// coroRampName builds the symbol name for a synthesized coroutine ramp
// (`.goroutine.N` / `.generator.N`) spawned inside `enclosing`, qualifying the
// global counter `n` with the enclosing function's own (deterministic, mangled)
// name. The `kind` is "goroutine" or "generator".
//
// Qualifying by enclosing name is what makes T1222 safe. The ramp is attributed to
// the same split unit as `enclosing` (see attributeCoroToEnclosing), so a cross-
// program cached instance `.bc` carries its ramp definition. But the counter `n`
// alone is a whole-program sequence: a consuming program that skip-serves that
// cached instance does NOT advance its counter for the skipped body, so one of its
// own unrelated ramps can reuse the same number → `duplicate symbol .goroutine.N`
// at link time (two external definitions in two linked units). Prefixing with the
// enclosing name gives ramps from different enclosing functions disjoint symbols,
// while ramps from the SAME enclosing function only ever exist in a single cached
// unit that is linked once. (The program-entry `.goroutine.main`, created in
// sched.go, is a distinct non-numbered coroutine and is unaffected.)
func coroRampName(kind, enclosing string, n int) string {
	if enclosing == "" {
		return fmt.Sprintf(".%s.%d", kind, n)
	}
	return fmt.Sprintf(".%s.%s.%d", kind, enclosing, n)
}

// coroEnclosingQualifier returns the enclosing function's (deterministic, mangled)
// name to embed in a synthesized coroutine ramp symbol — but ONLY when that function
// is owned by a module/instance split unit. Those are the units that get compiled to
// a `.bc`/`.o` and cached under a content-independent key shared across programs
// (see InstanceCacheKey), so a bare `.goroutine.N` counter in them can collide with
// an unrelated ramp a consuming program numbers with the same value (T1222 duplicate
// symbol). For plain main-code spawners (in neither map) it returns "", leaving the
// bare `.goroutine.N` name: those ramps only ever live in the per-program main IR,
// which is linked exactly once, so the counter cannot collide. Returning "" for the
// common case also keeps the ramp names stable for existing tests.
func (c *Compiler) coroEnclosingQualifier(enclosing *ir.Func) string {
	if enclosing == nil {
		return ""
	}
	name := enclosing.Name()
	if _, ok := c.instanceOwnedFuncs[name]; ok {
		return name
	}
	if _, ok := c.moduleOwnedFuncs[name]; ok {
		return name
	}
	return ""
}

// attributeCoroToEnclosing attributes a synthesized numbered coroutine ramp
// (`.goroutine.N` / `.generator.N`) to the same ownership unit as the `enclosing`
// function that spawns it. Without this, a ramp created via `c.module.NewFunc` is
// unowned: when its enclosing function is an instance/module-owned generic method,
// `SplitModuleIRs`/`InstanceIRs` route the method body into the instance/module
// `.bc` but leave the ramp definition in main IR. A later compile that serves the
// instance `.bc` from cache then references the ramp by a symbol whose body is not
// present in that unit → undefined-symbol link error (isolated) or wrong-body
// UAF/SIGSEGV (batched). T1222. See coroRampName for why the ramp symbol is
// additionally qualified by the enclosing name to avoid the dual duplicate-symbol
// failure mode.
//
// The enclosing function is `c.fn` at a go-block site (the ramp is built inside the
// method body) but the factory `fn` parameter at a generator site (the ramp is
// built while emitting the method itself, before `c.fn` is switched to the ramp).
//
// instanceOwnedFuncs is checked first because instance ownership takes precedence
// over module ownership in saveAndStripNonOwned. When the enclosing function is
// plain main code or a synthesized structural default (in neither map), this is a
// no-op and the ramp correctly stays in main — the same unit as its enclosing
// function. Nested ramps propagate naturally: when a ramp body is compiled, its
// enclosing function is the now-registered outer ramp.
func (c *Compiler) attributeCoroToEnclosing(coroName string, enclosing *ir.Func) {
	if enclosing == nil {
		return
	}
	if owner, ok := c.instanceOwnedFuncs[enclosing.Name()]; ok {
		c.instanceOwnedFuncs[coroName] = owner
	} else if owner, ok := c.moduleOwnedFuncs[enclosing.Name()]; ok {
		c.moduleOwnedFuncs[coroName] = owner
	}
}

// SplitModuleIRs produces separate IR text for each module and the main file.
// Module IRs contain only module-owned function definitions; everything else
// is emitted as declarations (extern). The main IR contains everything except
// module-owned function definitions, which become declarations.
//
// This enables separate compilation: each IR can be compiled to its own .o
// file, then linked together.
func (r *CompileResult) SplitModuleIRs() (mainIR string, moduleIRs map[string]string) {
	c := r.compiler
	moduleIRs = make(map[string]string)

	// Collect all module names
	moduleNames := r.ModuleNames()
	if len(moduleNames) == 0 {
		mainIR = r.Module.String()
		return
	}

	// For each module: keep only module-owned function bodies,
	// strip everything else to declare. Also strip globals to extern.
	for _, modName := range moduleNames {
		savedFuncs := saveAndStripNonOwned(c, modName)
		savedGlobals := stripGlobals(c)
		moduleIRs[modName] = r.Module.String()
		restoreGlobals(savedGlobals)
		restoreBlocks(savedFuncs)
	}

	// For main: strip all module-owned function bodies to declare.
	savedBlocks := saveAndStripOwned(c)
	mainIR = r.Module.String()
	restoreBlocks(savedBlocks)

	return
}

// saveAndStripNonOwned strips function bodies from all functions NOT owned by
// the given module. Instance-owned functions are always stripped (they live in
// instance .bc files, not module .bc files). Returns saved blocks for restoration.
func saveAndStripNonOwned(c *Compiler, moduleName string) map[*ir.Func][]*ir.Block {
	saved := make(map[*ir.Func][]*ir.Block)
	for _, fn := range c.module.Funcs {
		if len(fn.Blocks) == 0 {
			continue // already a declaration
		}
		owner, isModule := c.moduleOwnedFuncs[fn.Name()]
		if isModule && owner == moduleName {
			// Also strip if this is instance-owned (instance .bc takes precedence)
			if _, isInst := c.instanceOwnedFuncs[fn.Name()]; !isInst {
				continue // this function belongs to our module — keep it
			}
		}
		// Strip body: save blocks and clear them
		saved[fn] = fn.Blocks
		fn.Blocks = nil
	}
	return saved
}

// saveAndStripOwned strips function bodies from all module-owned and instance-owned
// functions. The main .bc only contains "main code" (non-module, non-instance functions).
// Returns saved blocks for restoration.
func saveAndStripOwned(c *Compiler) map[*ir.Func][]*ir.Block {
	saved := make(map[*ir.Func][]*ir.Block)
	for _, fn := range c.module.Funcs {
		if len(fn.Blocks) == 0 {
			continue
		}
		_, isModule := c.moduleOwnedFuncs[fn.Name()]
		_, isInst := c.instanceOwnedFuncs[fn.Name()]
		if isModule || isInst {
			saved[fn] = fn.Blocks
			fn.Blocks = nil
		}
	}
	return saved
}

// restoreBlocks restores previously saved function blocks.
func restoreBlocks(saved map[*ir.Func][]*ir.Block) {
	for fn, blocks := range saved {
		fn.Blocks = blocks
	}
}

// savedGlobal stores original global state for restoration.
type savedGlobal struct {
	init     constant.Constant
	linkage  enum.Linkage
	immut    bool
	tlsModel enum.TLSModel
}

// stripGlobals converts non-private global definitions to external declarations.
// Non-private globals (scheduler state, RTTI, vtables) are owned by the main IR.
// Private globals (string constants — all string globals use LinkagePrivate) are
// kept as-is in each split IR: each module/instance .bc gets its own copy of the
// string data it references. LTO deduplicates identical private globals during
// link-time optimization. This makes each .bc self-contained for string constants,
// preventing stale cache entries from referencing non-existent symbols (B0005).
func stripGlobals(c *Compiler) map[*ir.Global]savedGlobal {
	saved := make(map[*ir.Global]savedGlobal)
	for _, g := range c.module.Globals {
		if g.Init == nil {
			continue // already a declaration
		}
		if g.Linkage == enum.LinkagePrivate {
			continue // private globals (string constants) stay in module IR
		}
		saved[g] = savedGlobal{
			init:     g.Init,
			linkage:  g.Linkage,
			immut:    g.Immutable,
			tlsModel: g.TLSModel,
		}
		g.Init = nil
		g.Linkage = enum.LinkageExternal
		// Keep Immutable, TLSModel, ContentType — these are part of the type signature
	}
	return saved
}

// InstanceIRs returns a map of mono instance name → IR string for all instances
// that have method bodies in the current module. Used to extract per-instance
// .bc files for caching. Each instance IR contains:
//   - All LLVM named struct type definitions (LLVM LTO merges identical types)
//   - Only this instance's owned function bodies
//   - All private globals (string constants etc.)
//   - All non-private globals as external declarations (including vtables/typeinfos)
//
// Vtable and typeinfo global definitions stay in the main IR to preserve type-ID
// consistency — instance .bc files reference them as extern declarations only.
func (r *CompileResult) InstanceIRs() map[string]string {
	c := r.compiler
	if len(c.instanceOwnedFuncs) == 0 {
		return nil
	}

	// Collect all instance names that have at least one function with a body.
	instNames := make(map[string]bool)
	for funcName, instName := range c.instanceOwnedFuncs {
		if fn, ok := c.funcs[funcName]; ok && len(fn.Blocks) > 0 {
			instNames[instName] = true
		}
	}
	if len(instNames) == 0 {
		return nil
	}

	result := make(map[string]string, len(instNames))
	for instName := range instNames {
		savedFuncs := saveAndStripNonOwnedInst(c, instName)
		savedGlobals := stripGlobals(c)
		result[instName] = r.Module.String()
		restoreGlobals(savedGlobals)
		restoreBlocks(savedFuncs)
	}
	return result
}

// saveAndStripNonOwnedInst strips function bodies from all functions NOT owned
// by the given mono instance. Returns saved blocks for restoration.
func saveAndStripNonOwnedInst(c *Compiler, instName string) map[*ir.Func][]*ir.Block {
	saved := make(map[*ir.Func][]*ir.Block)
	for _, fn := range c.module.Funcs {
		if len(fn.Blocks) == 0 {
			continue // already a declaration
		}
		if owner, isInst := c.instanceOwnedFuncs[fn.Name()]; isInst && owner == instName {
			continue // this function belongs to our instance — keep it
		}
		saved[fn] = fn.Blocks
		fn.Blocks = nil
	}
	return saved
}

// restoreGlobals restores previously saved global definitions.
func restoreGlobals(saved map[*ir.Global]savedGlobal) {
	for g, s := range saved {
		g.Init = s.init
		g.Linkage = s.linkage
		g.Immutable = s.immut
		g.TLSModel = s.tlsModel
	}
}
