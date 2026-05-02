package codegen

import (
	"djabi.dev/go/promise_lang/internal/sema"

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
// Non-private globals (scheduler state, RTTI, vtables) are owned by the main module.
// Private globals (string constants) are kept as-is: LTO handles them as separate
// private copies per module — each module's functions reference their own copy,
// and LTO renames them during merge to avoid symbol conflicts.
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
