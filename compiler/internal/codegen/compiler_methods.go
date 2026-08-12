package codegen

import (
	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// declareTypeMethods creates LLVM function stubs for all methods with bodies (pass 1).
// Generic types are skipped — their methods are handled by declareMonoMethods.
func (c *Compiler) declareTypeMethods(file *ast.File) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue // excluded by `target(cond) annotation for this build target
		}
		named := c.lookupNamedType(td.Name)
		if named == nil {
			continue
		}
		if len(named.TypeParams()) > 0 {
			continue // generic — handled by monomorphization
		}

		for _, md := range td.Methods {
			if md.Body == nil {
				continue // abstract or native
			}
			if len(md.TypeParams) > 0 {
				continue // generic method — handled by mono method instances
			}
			m := c.lookupMethodForDecl(named, md)
			if m == nil || m.Sig() == nil {
				continue
			}

			mangledName := mangleMethodDeclName(td.Name, md)

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
		}
	}
}

// defineTypeMethods generates method bodies (pass 2).
// Generic types are skipped — their methods are handled by defineMonoMethods.
func (c *Compiler) defineTypeMethods(file *ast.File) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue // excluded by `target(cond) annotation for this build target
		}
		named := c.lookupNamedType(td.Name)
		if named == nil {
			continue
		}
		if len(named.TypeParams()) > 0 {
			continue // generic — handled by monomorphization
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

			mangledName := mangleMethodDeclName(td.Name, md)
			fn, ok := c.funcs[mangledName]
			if !ok {
				continue
			}

			// Route generator methods to the generator codegen path
			if genInfo := c.info.GeneratorFuncs[md]; genInfo != nil {
				c.defineGeneratorMethod(md, m, fn, genInfo.ElemType, named)
				continue
			}
			c.defineMethodFunc(md, m, fn, named)
		}
	}
}

// declareEnumMethods creates LLVM function stubs for enum methods (pass 1).
// Generic enums are skipped — their methods are handled by monomorphization.
func (c *Compiler) declareEnumMethods(file *ast.File) {
	for _, decl := range file.Decls {
		ed, ok := decl.(*ast.EnumDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		enum := c.lookupEnumType(ed.Name)
		if enum == nil {
			continue
		}
		if len(enum.TypeParams()) > 0 {
			continue // generic — handled by monomorphization
		}

		for _, md := range ed.Methods {
			if md.Body == nil {
				continue
			}
			m := c.lookupEnumMethod(enum, md)
			if m == nil || m.Sig() == nil {
				continue
			}

			mangledName := mangleMethodDeclName(ed.Name, md)

			var params []*ir.Param
			if m.Sig().Recv() != nil {
				// Enum methods receive a pointer to the enum value struct (i8*)
				params = append(params, ir.NewParam("this", irtypes.I8Ptr))
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
		}
	}
}

// defineEnumMethods generates enum method bodies (pass 2).
// Generic enums are skipped — their methods are handled by monomorphization.
func (c *Compiler) defineEnumMethods(file *ast.File) {
	for _, decl := range file.Decls {
		ed, ok := decl.(*ast.EnumDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		enum := c.lookupEnumType(ed.Name)
		if enum == nil {
			continue
		}
		if len(enum.TypeParams()) > 0 {
			continue // generic — handled by monomorphization
		}

		for _, md := range ed.Methods {
			if md.Body == nil {
				continue
			}
			m := c.lookupEnumMethod(enum, md)
			if m == nil || m.Sig() == nil {
				continue
			}

			mangledName := mangleMethodDeclName(ed.Name, md)
			fn, ok := c.funcs[mangledName]
			if !ok {
				continue
			}

			// Route generator methods to the generator codegen path
			if genInfo := c.info.GeneratorFuncs[md]; genInfo != nil {
				c.defineGeneratorMethod(md, m, fn, genInfo.ElemType, nil)
				continue
			}

			// B0285: Suppress match-dup inside enum clone methods
			if md.Name == "clone" {
				c.suppressMatchDup = true
			}
			// T0604: Set currentDropEnum so defineMethodFunc emits variant field drops
			if md.Name == "drop" {
				c.currentDropEnum = enum
			}
			c.defineMethodFunc(md, m, fn)
			c.currentDropEnum = nil
			c.suppressMatchDup = false
		}
	}
}

// declareModuleEnumMethods creates LLVM function stubs for enum methods in a module.
// Uses module-prefixed IR names. Generic enums are skipped.
func (c *Compiler) declareModuleEnumMethods(file *ast.File, moduleName string) {
	for _, decl := range file.Decls {
		ed, ok := decl.(*ast.EnumDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		enum := c.lookupEnumType(ed.Name)
		if enum == nil {
			continue
		}
		if len(enum.TypeParams()) > 0 {
			continue // generic — handled by monomorphization
		}

		for _, md := range ed.Methods {
			if md.Body == nil {
				continue
			}
			m := c.lookupEnumMethod(enum, md)
			if m == nil || m.Sig() == nil {
				continue
			}

			mangledName := mangleModuleMethodDeclName(moduleName, ed.Name, md)

			// B0244: Skip if already forward-declared (cross-module clone forward-declare)
			if fn, exists := c.funcs[mangledName]; exists {
				c.moduleOwnedFuncs[mangledName] = moduleName
				plainName := mangleMethodDeclName(ed.Name, md)
				if _, exists := c.funcs[plainName]; !exists {
					c.funcs[plainName] = fn
				}
				continue
			}

			var params []*ir.Param
			if m.Sig().Recv() != nil {
				params = append(params, ir.NewParam("this", irtypes.I8Ptr))
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
			plainName := mangleMethodDeclName(ed.Name, md)
			if _, exists := c.funcs[plainName]; !exists {
				c.funcs[plainName] = fn
			}
		}
	}
}

// defineModuleEnumMethods generates enum method bodies for a module.
// Generic enums are skipped.
func (c *Compiler) defineModuleEnumMethods(file *ast.File, moduleName string) {
	for _, decl := range file.Decls {
		ed, ok := decl.(*ast.EnumDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		enum := c.lookupEnumType(ed.Name)
		if enum == nil {
			continue
		}
		if len(enum.TypeParams()) > 0 {
			continue // generic — handled by monomorphization
		}

		for _, md := range ed.Methods {
			if md.Body == nil {
				continue
			}
			m := c.lookupEnumMethod(enum, md)
			if m == nil || m.Sig() == nil {
				continue
			}

			mangledName := mangleModuleMethodDeclName(moduleName, ed.Name, md)
			fn, ok := c.funcs[mangledName]
			if !ok || len(fn.Blocks) > 0 {
				continue
			}

			// Route generator methods to the generator codegen path
			if genInfo := c.info.GeneratorFuncs[md]; genInfo != nil {
				c.defineGeneratorMethod(md, m, fn, genInfo.ElemType, nil)
				continue
			}

			// B0285: Suppress match-dup inside enum clone methods
			if md.Name == "clone" {
				c.suppressMatchDup = true
			}
			// T0604: Set currentDropEnum so defineMethodFunc emits variant field drops
			if md.Name == "drop" {
				c.currentDropEnum = enum
			}
			c.defineMethodFunc(md, m, fn)
			c.currentDropEnum = nil
			c.suppressMatchDup = false
		}
	}
}

// lookupEnumMethod finds a method on an enum, dispatching to the appropriate
// lookup based on the AST declaration's getter/setter flags.
func (c *Compiler) lookupEnumMethod(enum *types.Enum, md *ast.MethodDecl) *types.Method {
	if md.IsGetter {
		return enum.LookupGetter(md.Name)
	}
	// Disambiguate the unary vs binary variant of an operator symbol by arity (T0883).
	if types.IsUnaryOperatorName(md.Name) {
		if len(md.Params) == 0 {
			return enum.LookupUnaryMethod(md.Name)
		}
		return enum.LookupBinaryMethod(md.Name)
	}
	return enum.LookupMethod(md.Name)
}

// typeGlobalName returns the IR global name for a type's typeinfo/vtable/rtti globals.
// When compiling a module, the name is prefixed with "__mod_<module>_" to avoid
// collisions with std library types or types from other modules.
func (c *Compiler) typeGlobalName(named *types.Named) string {
	name := named.Obj().Name()
	if c.compilingModule != "" {
		return "__mod_" + c.compilingModule + "_" + name
	}
	return name
}

// mangleMethodName returns the mangled IR function name for a method, appending
// a "$set" suffix for setters to avoid collisions with same-name getters.
func mangleMethodName(typeName, methodName string, isSetter bool) string {
	if isSetter {
		return typeName + "." + methodName + "$set"
	}
	return typeName + "." + methodName
}

// isUnaryOperatorDecl reports whether an AST method declaration is the prefix-
// unary (0-param) variant of an operator symbol that also has a binary form
// (`-`, `!`, `~`). Such declarations get a "$unary" IR-name discriminator so
// they never collide with the binary variant of the same symbol (T0883).
func isUnaryOperatorDecl(md *ast.MethodDecl) bool {
	return !md.IsGetter && !md.IsSetter && len(md.Params) == 0 && types.IsUnaryOperatorName(md.Name)
}

// mangleMethodDeclName mangles an LLVM method name from an AST method
// declaration, applying the "$unary" discriminator for a prefix-unary operator
// and the "$set" discriminator for a setter (T0883).
func mangleMethodDeclName(typeName string, md *ast.MethodDecl) string {
	if isUnaryOperatorDecl(md) {
		return typeName + "." + md.Name + "$unary"
	}
	return mangleMethodName(typeName, md.Name, md.IsSetter)
}

// mangleModuleMethodDeclName is the module-qualified form of mangleMethodDeclName.
func mangleModuleMethodDeclName(moduleName, typeName string, md *ast.MethodDecl) string {
	return "__mod_" + moduleName + "_" + mangleMethodDeclName(typeName, md)
}

// mangleMethodNameForMethod mangles an LLVM method name from a resolved
// *types.Method, applying the "$unary"/"$set" discriminators (T0883). Used where
// only the method object is available (e.g., vtable emission).
func mangleMethodNameForMethod(typeName string, m *types.Method) string {
	if m.IsUnaryOperator() {
		return typeName + "." + m.Name() + "$unary"
	}
	return mangleMethodName(typeName, m.Name(), m.IsSetter())
}

// operatorMethodNames is the set of operator symbols a method name can be (per
// the grammar's methodName production, PromiseParser.g4). Operator operands are
// borrowed, not moved, so a method whose name is one of these returns its value
// params by alias unless codegen clones them (T0897).
var operatorMethodNames = map[string]bool{
	"+": true, "-": true, "*": true, "/": true, "%": true,
	"==": true, "!=": true, "<": true, ">": true, "<=": true, ">=": true,
	"&&": true, "||": true, "!": true,
	"&": true, "|": true, "^": true, "<<": true, ">>": true, "~": true,
	"++": true, "--": true, "..": true, "..=": true,
	"[]": true, "[]=": true, "[:]": true, "[:]=": true,
}

// isOperatorMethodName reports whether a method name is an operator symbol.
func isOperatorMethodName(name string) bool { return operatorMethodNames[name] }

// setOperatorValueParams populates c.currentOpValueParams with the borrowed
// value parameters of an operator method (T0897), or clears it (nil) for a
// non-operator method. A borrowed value param is a plain (non-`~`, non-`&`,
// non-variadic) parameter — the operand of an overloaded operator, which
// operator dispatch borrows rather than moves. Mirrors the c.thisRecvIsOwned
// assignment: call wherever a method body's compilation context is established.
func (c *Compiler) setOperatorValueParams(name string, sig *types.Signature) {
	c.currentOpValueParams = nil
	if sig == nil || !isOperatorMethodName(name) {
		return
	}
	for _, p := range sig.Params() {
		if p.Name() == "" || p.Name() == "_" || p.IsVariadic() {
			continue
		}
		if p.Ref() == types.RefMut || p.Ref() == types.RefShared {
			continue
		}
		switch p.Type().(type) {
		case *types.MutRef, *types.SharedRef:
			continue
		}
		if c.currentOpValueParams == nil {
			c.currentOpValueParams = make(map[string]bool)
		}
		c.currentOpValueParams[p.Name()] = true
	}
}

// setBorrowedValueParams populates c.borrowedValueParams with the borrowed
// value parameters of the function/method whose body is about to be compiled
// (T0945). A borrowed value param is a plain (non-`~`) or `&` parameter that is
// not variadic and not reference-typed: the caller retains ownership, so the
// callee must not free its contents. `~` (RefMut) and variadic params are owned
// by the callee (they receive scope-exit drop bindings) and are excluded.
// Reference-typed params (MutRef/SharedRef) never reach the droppable-temp path,
// so they are excluded too. Call wherever a function body's compilation context
// is established (alongside c.currentRetType), like setOperatorValueParams.
func (c *Compiler) setBorrowedValueParams(sig *types.Signature) {
	c.borrowedValueParams = nil
	if sig == nil {
		return
	}
	for _, p := range sig.Params() {
		if p.Name() == "" || p.Name() == "_" || p.IsVariadic() {
			continue
		}
		if p.Ref() == types.RefMut {
			continue // ~ owned: the callee drops it at scope exit
		}
		switch p.Type().(type) {
		case *types.MutRef, *types.SharedRef:
			continue // ref-typed: the elvis result would be a ref, never tracked
		}
		if c.borrowedValueParams == nil {
			c.borrowedValueParams = make(map[string]bool)
		}
		c.borrowedValueParams[p.Name()] = true
	}
}

// lookupAnyMethod finds a method, getter, or setter by name, dispatching to
// the appropriate typed lookup based on the AST declaration's getter/setter flags.
func (c *Compiler) lookupAnyMethod(named *types.Named, name string, isGetter, isSetter bool) *types.Method {
	if isGetter {
		return named.LookupGetter(name)
	}
	if isSetter {
		return named.LookupSetter(name)
	}
	return named.LookupMethod(name)
}

// lookupMethodForMethod resolves the concrete *types.Method on named that
// corresponds to an abstract/view method m, disambiguating the unary vs binary
// variant of an operator symbol by arity (T0883).
func (c *Compiler) lookupMethodForMethod(named *types.Named, m *types.Method) *types.Method {
	if m.IsGetter() {
		return named.LookupGetter(m.Name())
	}
	if m.IsSetter() {
		return named.LookupSetter(m.Name())
	}
	if m.IsUnaryOperator() {
		return named.LookupUnaryMethod(m.Name())
	}
	if types.IsUnaryOperatorName(m.Name()) {
		return named.LookupBinaryMethod(m.Name())
	}
	return named.LookupMethod(m.Name())
}

// lookupMethodForDecl resolves the *types.Method for an AST method declaration,
// disambiguating the unary vs binary variant of an operator symbol by arity
// (T0883). Mirrors lookupAnyMethod for the getter/setter/regular cases.
func (c *Compiler) lookupMethodForDecl(named *types.Named, md *ast.MethodDecl) *types.Method {
	if md.IsGetter {
		return named.LookupGetter(md.Name)
	}
	if md.IsSetter {
		return named.LookupSetter(md.Name)
	}
	if types.IsUnaryOperatorName(md.Name) {
		if len(md.Params) == 0 {
			return named.LookupUnaryMethod(md.Name)
		}
		return named.LookupBinaryMethod(md.Name)
	}
	return named.LookupMethod(md.Name)
}

// defineMethodFunc generates the body of a single method.
func (c *Compiler) defineMethodFunc(md *ast.MethodDecl, m *types.Method, fn *ir.Func, ownerNamed ...*types.Named) {
	c.fn = fn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.castSubjectMatch = nil
	c.dropBindings = make(map[string]scopeBinding)
	c.stmtTemps = nil                         // T0073
	c.stmtTempMap = make(map[value.Value]int) // T0073
	c.heapTemps = nil                         // T0088
	c.heapTempMap = make(map[value.Value]int) // T0088
	c.envTemps = nil                          // T0100
	c.envTempMap = make(map[value.Value]int)  // T0100
	// B0172: Enable temp tracking for method bodies.
	c.tempTrackingEnabled = true
	c.mutRefPtrs = nil
	c.mutRefTypes = nil
	c.scopeBindings = nil
	c.loopScopeDepth = 0
	c.loopTempFloor = [4]int{} // T1331
	c.blockCounter = 0
	c.enumCtorTemps = nil        // B0267: prevent cross-function alloca leak
	c.matchBorrowedIdents = nil  // T0485: clear cross-function stale entries
	c.borrowOptionalLocals = nil // T1085: clear cross-function stale entries
	c.canError = m.Sig().CanError()
	c.currentRetType = m.Sig().Result()
	savedNamed := c.currentNamed
	if len(ownerNamed) > 0 {
		c.currentNamed = ownerNamed[0]
	}
	defer func() { c.currentNamed = savedNamed }()

	entry := fn.NewBlock(".entry")
	c.block = entry
	c.entryBlock = entry

	paramIdx := 0

	// T0428: track whether the receiver is owned (~this) for force-unwrap neutralization.
	c.thisRecvIsOwned = m.Sig().Recv() != nil && m.Sig().Recv().Ref() == types.RefMut
	c.setOperatorValueParams(md.Name, m.Sig()) // T0897
	c.setBorrowedValueParams(m.Sig())          // T0945

	// Allocate receiver as "this"
	if m.Sig().Recv() != nil {
		receiverType := fn.Params[paramIdx].Typ
		alloca := entry.NewAlloca(receiverType)
		alloca.SetName(c.uniqueLocalName("this.addr"))
		entry.NewStore(fn.Params[paramIdx], alloca)
		c.locals["this"] = alloca
		paramIdx++
	}

	// Allocate regular parameters
	for _, p := range m.Sig().Params() {
		if p.Name() == "" || p.Name() == "_" {
			paramIdx++
			continue
		}
		if _, isMutRef := p.Type().(*types.MutRef); isMutRef {
			// MutRef param: caller passes a pointer to its alloca (B0149)
			innerType := c.resolveType(p.Type())
			if c.mutRefPtrs == nil {
				c.mutRefPtrs = make(map[string]value.Value)
				c.mutRefTypes = make(map[string]irtypes.Type)
			}
			c.mutRefPtrs[p.Name()] = fn.Params[paramIdx]
			c.mutRefTypes[p.Name()] = innerType
		} else {
			lt := c.resolveType(p.Type())
			alloca := entry.NewAlloca(lt)
			alloca.SetName(c.uniqueLocalName(p.Name() + ".addr"))
			entry.NewStore(fn.Params[paramIdx], alloca)
			c.locals[p.Name()] = alloca

			// T0087: Register drop binding for ~ (move) parameters.
			if p.Ref() == types.RefMut {
				paramType := p.Type()
				if c.typeSubst != nil {
					paramType = types.Substitute(paramType, c.typeSubst)
				}
				c.maybeRegisterDrop(p.Name(), alloca, paramType)
				c.maybeRegisterStructuralParamFree(p.Name(), alloca, paramType) // T0861
				// T1237: a closure-typed `move` param owns its heap env — register an
				// owning bindingFreeEnv (like a closure local) so it is freed at scope
				// exit. Moving the closure out (into a local/field/container/return)
				// clears this binding's drop flag, transferring ownership. The caller
				// transfers ownership by claiming the env temp / clearing the source
				// var's flag at the call site (genCallArgsWithMutRef).
				c.maybeRegisterEnvFree(p.Name(), alloca, paramType, nil)
			}

			// B0191: Register drop binding for variadic parameters.
			// Variadic params receive an owned Vector[T] that must be freed at scope exit.
			if p.IsVariadic() {
				paramType := p.Type()
				if c.typeSubst != nil {
					paramType = types.Substitute(paramType, c.typeSubst)
				}
				c.maybeRegisterDrop(p.Name(), alloca, paramType)
			}

			// T0322: Register drop bindings for plain heap params on `new`
			// constructor methods. genConstructorCallMono unconditionally clears
			// the caller's drop flag for every arg except the strdup-borrow
			// exception (string param on a type with drop/synth-drop), so the
			// callee must drop them at scope exit if they aren't moved into a
			// field. Restrict to Named-type receivers since
			// genConstructorCallMono is the only caller that performs the
			// unconditional clear; enum methods named `new` would not see this
			// transfer.
			if md.Name == "new" && p.Ref() != types.RefMut && !p.IsVariadic() {
				var recvNamed *types.Named
				if recv := m.Sig().Recv(); recv != nil {
					recvNamed = extractNamed(recv.Type())
				}
				if recvNamed != nil {
					skipDrop := extractNamed(p.Type()) == types.TypString && (recvNamed.HasDrop() || recvNamed.NeedsSynthDrop())
					if !skipDrop {
						paramType := p.Type()
						if c.typeSubst != nil {
							paramType = types.Substitute(paramType, c.typeSubst)
						}
						c.maybeRegisterDrop(p.Name(), alloca, paramType)
						// T0861: a by-value structural-view param of a `new`
						// constructor is owned (the call site clears the source
						// drop flag), so it needs an RTTI-dispatched free.
						c.maybeRegisterStructuralParamFree(p.Name(), alloca, paramType)
					}
				}
			}

			// T1233: plain tuple-by-value params borrow — the caller owns and
			// drops the tuple (see defineFunc). Supersedes T0406's callee-drop.
			// (`new` constructor params keep their T0322/T0135 clear-and-drop
			// semantics above.)

			// T1194: borrow-by-default heap param reassigned to a fresh owned
			// value inside the body (no-op unless reassigned).
			bpType := p.Type()
			if c.typeSubst != nil {
				bpType = types.Substitute(bpType, c.typeSubst)
			}
			c.maybeRegisterBorrowParamReassignDrop(p.Name(), alloca, bpType, p.Ref(), md.Body)
		}
		paramIdx++
	}

	// Coverage: instrument method entry
	if c.shouldInstrument() {
		pos := md.Pos()
		end := md.End()
		funcName := fn.GlobalName
		idx := c.addCoverageRegion(pos.File, pos.Line, end.Line, funcName, "method")
		c.emitCoverageIncrement(idx)
	}

	c.genBlock(md.Body)

	// For drop() methods: after the user body, automatically drop all fields that have drop()
	if md.Name == "drop" && c.block != nil && c.block.Term == nil {
		if len(ownerNamed) > 0 {
			c.emitFieldDrops(ownerNamed[0])
		} else if c.currentDropEnum != nil {
			// T0604: enum explicit drop — clean up droppable variant fields
			c.emitEnumVariantFieldDropsInline(c.currentDropEnum)
		}
	}

	// Ensure the function ends with a terminator
	if c.block != nil && c.block.Term == nil {
		// T0087: Clean up ~ parameter drop bindings on implicit return
		if len(c.scopeBindings) > 0 {
			c.emitScopeCleanup(0, false)
		}
		if c.canError {
			resultType := c.currentResultType()
			if isVoidResult(resultType) {
				c.block.NewRet(c.wrapOk(nil, resultType))
			} else {
				c.block.NewRet(c.wrapOk(c.zeroValue(resultType.Fields[1]), resultType))
			}
		} else if _, ok := fn.Sig.RetType.(*irtypes.VoidType); ok {
			c.block.NewRet(nil)
		} else {
			c.block.NewRet(c.zeroValue(fn.Sig.RetType))
		}
	}
}
