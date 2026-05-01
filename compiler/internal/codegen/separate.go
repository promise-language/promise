package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
)

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
// the given module. Returns saved blocks for restoration.
func saveAndStripNonOwned(c *Compiler, moduleName string) map[*ir.Func][]*ir.Block {
	saved := make(map[*ir.Func][]*ir.Block)
	for _, fn := range c.module.Funcs {
		if len(fn.Blocks) == 0 {
			continue // already a declaration
		}
		owner, isModule := c.moduleOwnedFuncs[fn.Name()]
		if isModule && owner == moduleName {
			continue // this function belongs to our module — keep it
		}
		// Strip body: save blocks and clear them
		saved[fn] = fn.Blocks
		fn.Blocks = nil
	}
	return saved
}

// saveAndStripOwned strips function bodies from all module-owned functions.
// Returns saved blocks for restoration.
func saveAndStripOwned(c *Compiler) map[*ir.Func][]*ir.Block {
	saved := make(map[*ir.Func][]*ir.Block)
	for _, fn := range c.module.Funcs {
		if len(fn.Blocks) == 0 {
			continue
		}
		if _, isModule := c.moduleOwnedFuncs[fn.Name()]; isModule {
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

// stripGlobals converts all global definitions to external declarations.
// Module .o files reference globals but don't define them — main owns all globals.
func stripGlobals(c *Compiler) map[*ir.Global]savedGlobal {
	saved := make(map[*ir.Global]savedGlobal)
	for _, g := range c.module.Globals {
		if g.Init == nil {
			continue // already a declaration
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

// restoreGlobals restores previously saved global definitions.
func restoreGlobals(saved map[*ir.Global]savedGlobal) {
	for g, s := range saved {
		g.Init = s.init
		g.Linkage = s.linkage
		g.Immutable = s.immut
		g.TLSModel = s.tlsModel
	}
}
