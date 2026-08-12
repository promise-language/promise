package codegen

import (
	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// compileModules inlines all imported module declarations into the current IR module.
// For each module, it temporarily swaps c.info and c.file to the module's context,
// runs the same layout/declare/define pipeline, then restores.
// Modules are compiled in topological order (dependencies before dependents)
// so that a module's types and functions are available when its dependents are compiled.
func (c *Compiler) compileModules() {
	if c.info.ModuleInfos == nil {
		return
	}

	// B0212: Cache module infos for cross-module drop function lookups during module compilation.
	c.moduleInfos = c.info.ModuleInfos

	// Build path → IR prefix mapping for alias resolution in genModuleCall.
	for _, modInfo := range c.info.ModuleInfos {
		if modInfo.Path != "" {
			c.moduleCanonical[modInfo.Path] = modInfo.EffectiveIRPrefix()
		}
	}

	// Collect mono instances, func instances, and method instances from the main
	// (user) sema info. These include instances of module-defined types/methods
	// (e.g. Map[string,int], Iterator[int].map[int]) and calls to module generic
	// functions (e.g. sort[int]) that were created by user code. We pass them to
	// compileModule so they are processed under the correct module context.
	mainMonoInstances := collectMonoInstances(c.info, c.spiralInstances)
	mainMonoFuncInstances := collectMonoFuncInstances(c.info, mainMonoInstances)
	mainMonoMethodInstances := collectMonoMethodInstances(c.info, mainMonoInstances)

	// Second pass: resolve type instances from generic function/method bodies (B0134).
	extraFromFuncs := resolveTypeInstancesFromFuncInstances(c.info, mainMonoInstances, mainMonoFuncInstances, mainMonoMethodInstances, c.spiralInstances)
	if len(extraFromFuncs) > 0 {
		c.computeMonoLayouts(extraFromFuncs)
		mainMonoInstances = append(mainMonoInstances, extraFromFuncs...)
	}

	// Propagate cross-module instances only. When module A creates instances of
	// types declared in module B (e.g., json creates Map[string, JsonValue] where
	// Map is in std), those must be passed to module B's compilation. A module's
	// own instances are NOT propagated — they're already in its own SemaInfo and
	// collected directly by collectMonoInstancesWithExtra.
	for _, modInfo := range c.info.ModuleInfos {
		modTypeNames := make(map[string]bool)
		modFuncNames := make(map[string]bool)
		for _, decl := range modInfo.File.Decls {
			switch d := decl.(type) {
			case *ast.TypeDecl:
				modTypeNames[d.Name] = true
			case *ast.EnumDecl:
				modTypeNames[d.Name] = true
			case *ast.FuncDecl:
				modFuncNames[d.Name] = true
			}
		}
		for _, inst := range modInfo.SemaInfo.Instances {
			originName := instanceOriginTypeName(inst)
			if originName != "" && !modTypeNames[originName] {
				mainMonoInstances = append(mainMonoInstances, inst)
			}
		}
		for _, fi := range modInfo.SemaInfo.FuncInstances {
			if !modFuncNames[fi.Func.Name()] {
				mainMonoFuncInstances = append(mainMonoFuncInstances, fi)
			}
		}
		for _, mi := range modInfo.SemaInfo.MethodInstances {
			if !modTypeNames[mi.OwnerName()] {
				mainMonoMethodInstances = append(mainMonoMethodInstances, mi)
			}
		}
	}

	// B0344: Include unresolved func/method instances from user code (c.info)
	// for cross-module resolution. collectMonoFuncInstances/collectMonoMethodInstances
	// only return concrete instances; unresolved ones (containing TypeParams) are
	// excluded. For module-internal tests where the module source is merged into the
	// main file, these unresolved instances (e.g., decode_string[T] from a generic
	// method body) must be accumulated so crossResolveAccumulatedInstances can resolve
	// them using concrete instances (e.g., Response.json[int] from user code).
	for _, fi := range c.info.FuncInstances {
		if funcInstanceContainsTypeParam(fi) {
			mainMonoFuncInstances = append(mainMonoFuncInstances, fi)
		}
	}
	for _, mi := range c.info.MethodInstances {
		if methodInstanceContainsTypeParam(mi) {
			mainMonoMethodInstances = append(mainMonoMethodInstances, mi)
		}
	}

	// B0344: Resolve unresolved cross-module instances. When module A's generic
	// method calls module B's generic function, the FuncInstance from A's sema
	// contains TypeParams that can be resolved by concrete method/func instances.
	crossResolveAccumulatedInstances(&mainMonoFuncInstances, &mainMonoMethodInstances)

	// Use topological order if available, otherwise fall back to map iteration.
	if len(c.info.ModuleOrder) > 0 {
		for _, key := range c.info.ModuleOrder {
			if modInfo, ok := c.info.ModuleInfos[key]; ok {
				c.compileModule(modInfo, mainMonoInstances, mainMonoFuncInstances, mainMonoMethodInstances)
			}
		}
	} else {
		for _, modInfo := range c.info.ModuleInfos {
			c.compileModule(modInfo, mainMonoInstances, mainMonoFuncInstances, mainMonoMethodInstances)
		}
	}
}

// compileModule compiles a single module's declarations into the current IR module.
// extraInstances contains mono instances from the caller's sema info (e.g., the
// main file) that may include instances of this module's types; they are merged
// in so that user-created instantiations of module-defined generics are processed
// under the correct module context (module's c.info and c.file).
func (c *Compiler) compileModule(modInfo *sema.ModuleInfo, extraInstances []*types.Instance, extraFuncInstances []*sema.FuncInstance, extraMethodInstances []*sema.MethodInstance) {
	// Save main context
	savedInfo := c.info
	savedFile := c.file
	savedModule := c.compilingModule

	// Switch to module context.
	// Use IRPrefix (derived from GlobalIdentity) for IR symbols — this is stable
	// and globally unique, enabling cross-project .o reuse.
	irName := modInfo.EffectiveIRPrefix()
	c.info = modInfo.SemaInfo
	c.file = modInfo.File
	c.compilingModule = irName
	// Reset per-module counters so module constant names are stable
	// (independent of how many constants main code has emitted so far).
	c.moduleStrCounter = 0
	c.moduleArrCounter = 0

	modFile := modInfo.File

	// 1. Compute enum layouts for module types
	c.computeEnumLayouts(modFile)

	// 2. Collect mono instances: module's own + any extra from caller that belong
	//    to types defined in this module's file. Transitive expansion uses the
	//    module's sema info so method-body references (e.g. _FnIter[T]) resolve.
	monoInstances := collectMonoInstancesWithExtra(modInfo, modFile, extraInstances, c.spiralInstances)

	// 3. Compute user type and monomorphic instance layouts together so that
	//    generic value-type instances used as fields get laid out before their
	//    containing user types. (T0565)
	c.computeAllTypeLayouts(modFile, monoInstances)

	monoFuncInstances := collectMonoFuncInstancesWithExtra(modInfo.SemaInfo, modFile, extraFuncInstances, monoInstances)
	monoMethodInstances := collectMonoMethodInstancesWithExtra(modInfo.SemaInfo, modFile, extraMethodInstances, monoInstances)
	crossResolveFuncMethodInstances(modInfo.SemaInfo, &monoFuncInstances, &monoMethodInstances)

	// 4. Declare module externs
	modExterns := collectExterns(modFile, modInfo.SemaInfo)
	c.declareModuleExterns(irName, modExterns)

	// 5. Declare method stubs for module types and enums
	c.declareModuleTypeMethods(modFile, irName)
	c.declareModuleEnumMethods(modFile, irName)
	c.declareMonoMethods(modFile, monoInstances)
	c.declareMonoEnumMethods(modFile, monoInstances)
	c.declareMonoSynthesizedDefaults(monoInstances)
	c.declareSynthesizedModuleDrops(modFile, irName)           // B0158
	c.declareSynthesizedModuleEnumDrops(modFile, irName)       // T0102
	c.declareSynthesizedMonoDrops(modFile, monoInstances)      // B0158
	c.declareSynthesizedMonoEnumDrops(modFile, monoInstances)  // T0102
	c.declareSynthesizedModuleEnumClones(modFile, irName)      // T1129
	c.declareSynthesizedMonoEnumClones(modFile, monoInstances) // T1129
	c.declareMonoInheritedDrops(monoInstances)                 // T0468
	c.declareInheritedModuleDrops(modFile, irName)             // T0507

	// 6. Compute vtable info and emit for module types
	c.computeVtableInfo(modFile)
	c.computeMonoVtableInfo(monoInstances)
	c.emitVtableGlobals(modFile)
	c.emitMonoVtableGlobals(monoInstances)
	c.emitTypeInfoGlobals(modFile)
	c.emitMonoTypeInfoGlobals(monoInstances)

	// 7. Declare and define module functions
	c.declareModuleFuncs(modFile, irName)
	c.declareMonoFuncs(modFile, monoFuncInstances)
	c.declareMonoMethodInstances(modFile, monoMethodInstances)

	// 8. Define module method bodies
	c.defineModuleTypeMethods(modFile, irName)
	c.defineModuleEnumMethods(modFile, irName)
	c.defineMonoMethods(modFile, monoInstances)
	c.defineMonoEnumMethods(modFile, monoInstances)
	c.defineMonoSynthesizedDefaults(monoInstances)
	c.defineSynthesizedModuleDrops(modFile, irName)           // B0158
	c.defineSynthesizedModuleEnumDrops(modFile, irName)       // T0102
	c.defineSynthesizedMonoDrops(modFile, monoInstances)      // B0158
	c.defineSynthesizedMonoEnumDrops(modFile, monoInstances)  // T0102
	c.defineSynthesizedModuleEnumClones(modFile, irName)      // T1129
	c.defineSynthesizedMonoEnumClones(modFile, monoInstances) // T1129
	c.defineMonoInheritedDrops(monoInstances)                 // T0468
	c.defineInheritedModuleDrops(modFile, irName)             // T0507

	// 9. Define module function bodies
	c.defineModuleFuncs(modFile, irName)
	c.defineMonoFuncs(modFile, monoFuncInstances)
	c.defineMonoMethodInstances(modFile, monoMethodInstances)

	// Restore main context
	c.info = savedInfo
	c.file = savedFile
	c.compilingModule = savedModule
}

// declareModuleExterns declares extern functions from a module, deduplicating
// against already-declared C functions. Registers in moduleExterns for qualified access.
func (c *Compiler) declareModuleExterns(moduleName string, externs []*ExternFunc) {
	// Use the standard extern declaration pipeline (handles dedup, sret, etc.)
	c.declareExterns(externs, c.layouts)

	// Register module externs for qualified access
	for _, ext := range externs {
		key := moduleName + "." + ext.PromiseName
		c.moduleExterns[key] = ext
		// Also register in main externs if no collision
		if _, exists := c.externs[ext.PromiseName]; !exists {
			c.externs[ext.PromiseName] = ext
		}
	}
}

// declareModuleFuncs declares module functions with module-prefixed IR names.
func (c *Compiler) declareModuleFuncs(file *ast.File, moduleName string) {
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || len(fd.TypeParams) > 0 {
			continue
		}
		isEmbed := c.info.Embeds[fd] != nil
		if fd.Body == nil && !isEmbed {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue // excluded by `target(cond) annotation for this build target
		}

		// Module-level setters are stored under "name$set" in sema scope.
		scopeName := fd.Name
		if fd.IsSetter {
			scopeName = fd.Name + "$set"
		}

		obj := c.lookupFunc(scopeName)
		if obj == nil {
			continue
		}
		sig, ok := obj.Type().(*types.Signature)
		if !ok {
			continue
		}

		retType := irtypes.Type(irtypes.Void)
		genInfo := c.info.GeneratorFuncs[fd]
		if genInfo != nil {
			if genInfo.CanError {
				retType = computeResultType(failableGeneratorValueType())
			} else {
				retType = generatorValueType()
			}
		} else if sig.Result() != nil {
			retType = c.resolveType(sig.Result())
		}
		if sig.CanError() && genInfo == nil {
			retType = computeResultType(retType)
		}

		var params []*ir.Param
		for _, p := range sig.Params() {
			params = append(params, ir.NewParam(p.Name(), c.resolveParamType(p)))
		}

		irName := mangleModuleFuncName(moduleName, scopeName)
		fn := c.module.NewFunc(irName, retType, params...)

		// Track ownership for separate compilation
		c.moduleOwnedFuncs[irName] = moduleName

		// Register in moduleFuncs for qualified access (mod.property / mod.property$set)
		key := moduleName + "." + scopeName
		c.moduleFuncs[key] = fn

		// Also register in main funcs if no collision (for glob imports)
		if _, shadowed := c.funcs[scopeName]; !shadowed {
			c.funcs[scopeName] = fn
		}
	}
}

// defineModuleFuncs generates bodies for module functions.
func (c *Compiler) defineModuleFuncs(file *ast.File, moduleName string) {
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || len(fd.TypeParams) > 0 {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue // excluded by `target(cond) annotation for this build target
		}

		// Handle embed getters — body is nil but we generate it from embedded data (B0145)
		if embedInfo := c.info.Embeds[fd]; embedInfo != nil {
			scopeName := fd.Name
			key := moduleName + "." + scopeName
			fn, ok := c.moduleFuncs[key]
			if ok {
				c.defineEmbedGetter(fd, fn, embedInfo)
			}
			continue
		}

		if fd.Body == nil {
			continue // native function — no body to generate
		}

		// Module-level setters are stored under "name$set" in sema scope.
		scopeName := fd.Name
		if fd.IsSetter {
			scopeName = fd.Name + "$set"
		}

		key := moduleName + "." + scopeName
		fn, ok := c.moduleFuncs[key]
		if !ok {
			continue
		}

		obj := c.lookupFunc(scopeName)
		if obj == nil {
			continue
		}
		sig, ok := obj.Type().(*types.Signature)
		if !ok {
			continue
		}

		c.fn = fn
		c.block = fn.NewBlock(".entry")
		c.entryBlock = c.block
		c.locals = make(map[string]*ir.InstAlloca)
		c.localNameCount = make(map[string]int)
		c.dropFlags = make(map[string]*ir.InstAlloca)
		c.castSubjectMatch = nil
		c.dropBindings = make(map[string]scopeBinding)
		c.stmtTemps = nil                         // T0073
		c.stmtTempMap = make(map[value.Value]int) // T0073
		c.envTemps = nil                          // T0100
		c.envTempMap = make(map[value.Value]int)  // T0100
		c.tempTrackingEnabled = true              // T0084: enable in methods too
		c.mutRefPtrs = nil
		c.mutRefTypes = nil
		c.scopeBindings = nil
		c.canError = sig.CanError()
		c.currentRetType = sig.Result()
		c.setBorrowedValueParams(sig) // T0945
		c.blockCounter = 0
		c.enumCtorTemps = nil // B0267

		// Bind parameters to local allocas
		for i, p := range fn.Params {
			sp := sig.Params()[i]
			if _, isMutRef := sp.Type().(*types.MutRef); isMutRef {
				// MutRef param: the LLVM param is a pointer to the caller's storage (B0149)
				innerType := c.resolveType(sp.Type())
				if c.mutRefPtrs == nil {
					c.mutRefPtrs = make(map[string]value.Value)
					c.mutRefTypes = make(map[string]irtypes.Type)
				}
				c.mutRefPtrs[sp.Name()] = p
				c.mutRefTypes[sp.Name()] = innerType
			} else {
				alloca := c.entryBlock.NewAlloca(p.Typ)
				c.entryBlock.NewStore(p, alloca)
				c.locals[sp.Name()] = alloca

				// T0087: Register drop binding for ~ (move) parameters.
				if sp.Ref() == types.RefMut {
					c.maybeRegisterDrop(sp.Name(), alloca, sp.Type())
					c.maybeRegisterStructuralParamFree(sp.Name(), alloca, sp.Type()) // T0861
				}

				// B0191: Register drop binding for variadic parameters.
				// Variadic params receive an owned Vector[T] that must be freed at scope exit.
				if sp.IsVariadic() {
					c.maybeRegisterDrop(sp.Name(), alloca, sp.Type())
				}

				// T1233: plain tuple-by-value params borrow — the caller owns and
				// drops the tuple (see defineFunc). Supersedes T0406's callee-drop.

				// T1194: borrow-by-default heap param reassigned to a fresh owned
				// value inside the body (no-op unless reassigned).
				c.maybeRegisterBorrowParamReassignDrop(sp.Name(), alloca, sp.Type(), sp.Ref(), fd.Body)
			}
		}

		c.genBlock(fd.Body)

		// Emit scope cleanup for drop bindings (variadic, move params)
		if c.block != nil && c.block.Term == nil {
			if len(c.scopeBindings) > 0 {
				c.emitScopeCleanup(0, false)
			}
			if sig.CanError() {
				c.block.NewRet(c.zeroValue(fn.Sig.RetType))
			} else if fn.Sig.RetType == irtypes.Void {
				c.block.NewRet(nil)
			} else {
				c.block.NewRet(c.zeroValue(fn.Sig.RetType))
			}
		}
	}
}

// declareModuleTypeMethods declares method stubs for module types with module-prefixed names.
func (c *Compiler) declareModuleTypeMethods(file *ast.File, moduleName string) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue // excluded by `target(cond) annotation for this build target
		}
		named := c.lookupNamedType(td.Name)
		if named == nil || len(named.TypeParams()) > 0 {
			continue
		}

		for _, md := range td.Methods {
			if md.Body == nil {
				continue
			}
			if len(md.TypeParams) > 0 {
				continue // generic method — handled by mono method instances
			}
			m := c.lookupMethodForDecl(named, md)
			if m == nil || m.Sig() == nil {
				continue
			}

			mangledName := mangleModuleMethodDeclName(moduleName, td.Name, md)

			var params []*ir.Param
			if m.Sig().Recv() != nil {
				receiverType := irtypes.Type(irtypes.I8Ptr)
				if isPrimitiveScalar(named) {
					receiverType = llvmNamedType(named)
				}
				params = append(params, ir.NewParam("this", receiverType))
			}
			for _, p := range m.Sig().Params() {
				params = append(params, ir.NewParam(p.Name(), c.resolveParamType(p)))
			}

			retType := irtypes.Type(irtypes.Void)
			genInfo := c.info.GeneratorFuncs[md]
			if genInfo != nil {
				if genInfo.CanError {
					retType = computeResultType(failableGeneratorValueType())
				} else {
					retType = generatorValueType()
				}
			} else if m.Sig().Result() != nil {
				retType = c.resolveType(m.Sig().Result())
			}
			if m.Sig().CanError() && genInfo == nil {
				retType = computeResultType(retType)
			}

			fn := c.module.NewFunc(mangledName, retType, params...)
			c.funcs[mangledName] = fn

			// Track ownership for separate compilation
			c.moduleOwnedFuncs[mangledName] = moduleName

			// Also register the non-prefixed method name for dispatch within the module
			plainName := mangleMethodDeclName(td.Name, md)
			if _, exists := c.funcs[plainName]; !exists {
				c.funcs[plainName] = fn
			}
		}
	}
}

// defineModuleTypeMethods generates method bodies for module types.
func (c *Compiler) defineModuleTypeMethods(file *ast.File, moduleName string) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue // excluded by `target(cond) annotation for this build target
		}
		named := c.lookupNamedType(td.Name)
		if named == nil || len(named.TypeParams()) > 0 {
			continue
		}

		for _, md := range td.Methods {
			if md.Body == nil {
				continue
			}
			if len(md.TypeParams) > 0 {
				continue // generic method — handled by mono method instances
			}
			m := c.lookupMethodForDecl(named, md)
			if m == nil || m.Sig() == nil {
				continue
			}

			mangledName := mangleModuleMethodDeclName(moduleName, td.Name, md)
			fn, ok := c.funcs[mangledName]
			if !ok {
				continue
			}

			c.currentNamed = named

			// T0436: track whether the receiver is owned (~this) for force-unwrap
			// neutralization. Without this, the flag carries over from the previous
			// defineMethodFunc call, potentially causing a borrowed-this method to
			// incorrectly clear the caller's Optional field present flag.
			c.thisRecvIsOwned = m.Sig().Recv() != nil && m.Sig().Recv().Ref() == types.RefMut
			c.setOperatorValueParams(md.Name, m.Sig()) // T0897
			c.setBorrowedValueParams(m.Sig())          // T0945

			// Route generator methods to the generator codegen path
			if genInfo := c.info.GeneratorFuncs[md]; genInfo != nil {
				c.defineGeneratorMethod(md, m, fn, genInfo.ElemType, named)
				c.currentNamed = nil
				continue
			}

			c.fn = fn
			c.block = fn.NewBlock(".entry")
			c.entryBlock = c.block
			c.locals = make(map[string]*ir.InstAlloca)
			c.localNameCount = make(map[string]int)
			c.dropFlags = make(map[string]*ir.InstAlloca)
			c.castSubjectMatch = nil
			c.dropBindings = make(map[string]scopeBinding)
			c.stmtTemps = nil                         // T0073
			c.stmtTempMap = make(map[value.Value]int) // T0073
			c.envTemps = nil                          // T0100
			c.envTempMap = make(map[value.Value]int)  // T0100
			// B0172: Enable temp tracking for module type methods.
			c.tempTrackingEnabled = true
			c.scopeBindings = nil
			c.canError = m.Sig().CanError()
			c.currentRetType = m.Sig().Result()
			c.blockCounter = 0
			c.enumCtorTemps = nil // B0267

			// Bind 'this' and parameters
			c.mutRefPtrs = nil
			c.mutRefTypes = nil
			paramIdx := 0
			if m.Sig().Recv() != nil {
				receiverType := irtypes.Type(irtypes.I8Ptr)
				if isPrimitiveScalar(named) {
					receiverType = llvmNamedType(named)
				}
				alloca := c.entryBlock.NewAlloca(receiverType)
				c.entryBlock.NewStore(fn.Params[0], alloca)
				c.locals["this"] = alloca
				paramIdx = 1
			}
			for i, p := range m.Sig().Params() {
				if _, isMutRef := p.Type().(*types.MutRef); isMutRef {
					// MutRef param: the LLVM param is a pointer to the caller's storage (B0149)
					innerType := c.resolveType(p.Type())
					if c.mutRefPtrs == nil {
						c.mutRefPtrs = make(map[string]value.Value)
						c.mutRefTypes = make(map[string]irtypes.Type)
					}
					c.mutRefPtrs[p.Name()] = fn.Params[paramIdx+i]
					c.mutRefTypes[p.Name()] = innerType
				} else {
					alloca := c.entryBlock.NewAlloca(fn.Params[paramIdx+i].Typ)
					c.entryBlock.NewStore(fn.Params[paramIdx+i], alloca)
					c.locals[p.Name()] = alloca

					// T0087: Register drop binding for ~ (move) parameters.
					if p.Ref() == types.RefMut {
						c.maybeRegisterDrop(p.Name(), alloca, p.Type())
						c.maybeRegisterStructuralParamFree(p.Name(), alloca, p.Type()) // T0861
					}

					// B0191: Register drop binding for variadic parameters.
					// Variadic params receive an owned Vector[T] that must be freed at scope exit.
					if p.IsVariadic() {
						c.maybeRegisterDrop(p.Name(), alloca, p.Type())
					}

					// T0322: Register drop bindings for plain heap params on `new`
					// constructor methods. genConstructorCallMono unconditionally
					// clears the caller's drop flag for every arg except the
					// strdup-borrow exception (string param on a type with drop /
					// synth-drop), so the callee must drop them at scope exit if
					// they aren't moved into a field. Restrict to Named-type
					// receivers since genConstructorCallMono is the only caller
					// that performs the unconditional clear; enum methods named
					// `new` would not see this transfer.
					if md.Name == "new" && p.Ref() != types.RefMut && !p.IsVariadic() {
						var recvNamed *types.Named
						if recv := m.Sig().Recv(); recv != nil {
							recvNamed = extractNamed(recv.Type())
						}
						if recvNamed != nil {
							skipDrop := extractNamed(p.Type()) == types.TypString && (recvNamed.HasDrop() || recvNamed.NeedsSynthDrop())
							if !skipDrop {
								c.maybeRegisterDrop(p.Name(), alloca, p.Type())
								// T0861: a by-value structural-view param of a `new`
								// constructor is owned (the call site clears the source
								// drop flag), so it needs an RTTI-dispatched free.
								c.maybeRegisterStructuralParamFree(p.Name(), alloca, p.Type())
							}
						}
					}

					// T1233: plain tuple-by-value params borrow — the caller owns
					// and drops the tuple (see defineFunc). Supersedes T0406's
					// callee-drop. (`new` constructor params keep their T0322/T0135
					// clear-and-drop semantics above.)

					// T1194: borrow-by-default heap param reassigned to a fresh
					// owned value inside the body (no-op unless reassigned).
					c.maybeRegisterBorrowParamReassignDrop(p.Name(), alloca, p.Type(), p.Ref(), md.Body)
				}
			}

			c.genBlock(md.Body)

			// T0553: For drop() methods, auto-append field drops (mirrors defineMethodFunc).
			// Without this, module-defined types with user-written drop bodies leak their
			// droppable fields.
			if md.Name == "drop" && c.block != nil && c.block.Term == nil {
				c.emitFieldDrops(named)
			}

			if c.block != nil && c.block.Term == nil {
				// Emit scope cleanup for drop bindings (variadic, move params)
				if len(c.scopeBindings) > 0 {
					c.emitScopeCleanup(0, false)
				}
				if m.Sig().CanError() {
					c.block.NewRet(c.zeroValue(fn.Sig.RetType))
				} else if fn.Sig.RetType == irtypes.Void {
					c.block.NewRet(nil)
				} else {
					c.block.NewRet(c.zeroValue(fn.Sig.RetType))
				}
			}
			c.currentNamed = nil
		}
	}
}
