package codegen

import (
	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/types"
)

// monoContext holds the context for generating code inside a monomorphic method body.
// Inside mono methods, info.Types[this] is the origin Named type (not Instance).
// monoCtx maps the origin type to its monomorphic layout.
type monoContext struct {
	inst   *types.Instance
	origin types.Type // *Named or *Enum
	name   string     // "Box__int"
}

// monoName generates a unique mangled name for a generic type instantiation.
// Example: Instance{Box, [int]} → "Box__int", Instance{Pair, [int, string]} → "Pair__int__string"
func monoName(inst *types.Instance) string {
	var name string
	switch o := inst.Origin().(type) {
	case *types.Named:
		name = o.Obj().Name()
	case *types.Enum:
		name = o.Obj().Name()
	default:
		name = "unknown"
	}
	for _, arg := range inst.TypeArgs() {
		name += "__" + typeArgSuffix(arg)
	}
	return name
}

// typeArgSuffix returns a suffix string for a type argument used in mangling.
func typeArgSuffix(typ types.Type) string {
	switch t := typ.(type) {
	case *types.Named:
		return t.Obj().Name()
	case *types.Enum:
		return t.Obj().Name()
	case *types.Instance:
		return monoName(t)
	default:
		return "unknown"
	}
}

// monoFuncName generates a unique mangled name for a generic function instantiation.
// Example: identity[int] → "identity__int"
func monoFuncName(fi *sema.FuncInstance) string {
	name := fi.Func.Name()
	for _, arg := range fi.TypeArgs {
		name += "__" + typeArgSuffix(arg)
	}
	return name
}

// collectMonoInstances deduplicates generic type instances by mangled name.
// Also transitively discovers instances referenced by field types of already-collected
// instances (e.g., map[string, int] has a Slot[K, V][] field which after substitution
// requires Slot[string, int] to be monomorphized).
func collectMonoInstances(info *sema.Info) []*types.Instance {
	seen := map[string]bool{}
	var result []*types.Instance
	for _, inst := range info.Instances {
		key := monoName(inst)
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, inst)
	}

	// Transitively expand: walk substituted field types and collect nested Instances.
	for i := 0; i < len(result); i++ {
		inst := result[i]
		switch origin := inst.Origin().(type) {
		case *types.Named:
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			for _, f := range origin.AllFields() {
				ft := types.Substitute(f.Type(), subst)
				discoverInstances(ft, &result, seen)
			}
		case *types.Enum:
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			for _, v := range origin.Variants() {
				for _, f := range v.Fields() {
					ft := types.Substitute(f.Type(), subst)
					discoverInstances(ft, &result, seen)
				}
			}
		}
	}

	return result
}

// discoverInstances recursively walks a type and collects any concrete Instance types.
func discoverInstances(t types.Type, result *[]*types.Instance, seen map[string]bool) {
	if t == nil {
		return
	}
	switch tt := t.(type) {
	case *types.Instance:
		if !types.ContainsTypeParam(tt) {
			key := monoName(tt)
			if !seen[key] {
				seen[key] = true
				*result = append(*result, tt)
			}
		}
		// Also check type args for nested instances
		for _, arg := range tt.TypeArgs() {
			discoverInstances(arg, result, seen)
		}
	case *types.Optional:
		discoverInstances(tt.Elem(), result, seen)
	case *types.SharedRef:
		discoverInstances(tt.Elem(), result, seen)
	case *types.MutRef:
		discoverInstances(tt.Elem(), result, seen)
	case *types.Pointer:
		discoverInstances(tt.Elem(), result, seen)
	case *types.Array:
		discoverInstances(tt.Elem(), result, seen)
	case *types.Tuple:
		for _, e := range tt.Elems() {
			discoverInstances(e, result, seen)
		}
	case *types.Signature:
		for _, p := range tt.Params() {
			discoverInstances(p.Type(), result, seen)
		}
		if tt.Result() != nil {
			discoverInstances(tt.Result(), result, seen)
		}
	}
}

// collectMonoFuncInstances deduplicates generic function instances by mangled name.
func collectMonoFuncInstances(info *sema.Info) []*sema.FuncInstance {
	seen := map[string]bool{}
	var result []*sema.FuncInstance
	for _, fi := range info.FuncInstances {
		key := monoFuncName(fi)
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, fi)
	}
	return result
}

// computeMonoUserTypeLayout computes a TypeDeclLayout for a monomorphic user type instance.
// It substitutes all TypeParam fields with concrete types from the subst map.
func computeMonoUserTypeLayout(module *ir.Module, named *types.Named, name string, subst map[*types.TypeParam]types.Type, allLayouts map[*types.Named]*TypeDeclLayout) *TypeDeclLayout {
	// Type struct: empty {}
	typeStruct := irtypes.NewStruct()
	typeStruct.SetName("promise_" + name + "_t")
	module.NewTypeDef("promise_"+name+"_t", typeStruct)

	typePtr := irtypes.NewPointer(typeStruct)

	// Variant struct: { promise_T_t* _type }
	variantStruct := irtypes.NewStruct(typePtr)
	variantStruct.SetName("promise_" + name + "_m")
	module.NewTypeDef("promise_"+name+"_m", variantStruct)

	variantPtr := irtypes.NewPointer(variantStruct)

	// Instance struct: { promise_T_m* _variant, field1, field2, ... }
	instanceLLVMFields := []irtypes.Type{variantPtr}
	fieldLayouts := []FieldLayout{
		{Name: "_variant", CType: "promise_" + name + "_m*", LLVMType: variantPtr, IsInternal: true},
	}
	fieldIndex := map[string]int{}

	for _, f := range named.AllFields() {
		// Substitute TypeParams with concrete types
		fieldType := types.Substitute(f.Type(), subst)
		llvmFT := instanceFieldLLVMType(fieldType, allLayouts)
		cType := userFieldCType(fieldType, allLayouts)
		instanceLLVMFields = append(instanceLLVMFields, llvmFT)
		idx := len(fieldLayouts)
		fieldLayouts = append(fieldLayouts, FieldLayout{
			Name: f.Name(), CType: cType, LLVMType: llvmFT, IsInternal: false,
		})
		fieldIndex[f.Name()] = idx
	}

	instanceStruct := irtypes.NewStruct(instanceLLVMFields...)
	instanceStruct.SetName("promise_" + name + "_i")
	module.NewTypeDef("promise_"+name+"_i", instanceStruct)

	instancePtr := irtypes.NewPointer(instanceStruct)

	// Value struct: { i8* _vtable, promise_T_i* _instance }
	valueStruct := irtypes.NewStruct(irtypes.I8Ptr, instancePtr)
	valueStruct.SetName("promise_" + name + "_v")
	module.NewTypeDef("promise_"+name+"_v", valueStruct)

	return &TypeDeclLayout{
		PromiseName:        name,
		Kind:               LayoutUserType,
		InstanceFieldIndex: fieldIndex,
		InstancePtrType:    instancePtr,
		Type: &StructLayout{
			CName:    "promise_" + name + "_t",
			Suffix:   "_t",
			Fields:   []FieldLayout{},
			LLVMType: typeStruct,
		},
		Variant: &StructLayout{
			CName:  "promise_" + name + "_m",
			Suffix: "_m",
			Fields: []FieldLayout{
				{Name: "_type", CType: "promise_" + name + "_t*", LLVMType: typePtr, IsInternal: true},
			},
			LLVMType: variantStruct,
		},
		Instance: &StructLayout{
			CName:    "promise_" + name + "_i",
			Suffix:   "_i",
			Fields:   fieldLayouts,
			LLVMType: instanceStruct,
		},
		Value: &StructLayout{
			CName:  "promise_" + name + "_v",
			Suffix: "_v",
			Fields: []FieldLayout{
				{Name: "_vtable", CType: "void*", LLVMType: irtypes.I8Ptr, IsInternal: true},
				{Name: "_instance", CType: "promise_" + name + "_i*", LLVMType: instancePtr, IsInternal: true},
			},
			LLVMType: valueStruct,
		},
	}
}

// computeMonoEnumLayout computes a TypeDeclLayout for a monomorphic enum instance.
func computeMonoEnumLayout(module *ir.Module, enum *types.Enum, name string, subst map[*types.TypeParam]types.Type, ptrSize int) *TypeDeclLayout {
	variantTag := map[string]int{}
	variantDataTypes := map[string]*irtypes.StructType{}
	maxDataSize := 0

	for i, v := range enum.Variants() {
		variantTag[v.Name()] = i

		if v.NumFields() > 0 {
			var fieldTypes []irtypes.Type
			for _, f := range v.Fields() {
				ft := types.Substitute(f.Type(), subst)
				fieldTypes = append(fieldTypes, llvmType(ft))
			}
			dataType := irtypes.NewStruct(fieldTypes...)
			variantDataTypes[v.Name()] = dataType

			ds := 0
			for _, ft := range fieldTypes {
				ds += llvmTypeSizeWithPtr(ft, ptrSize)
			}
			if ds > maxDataSize {
				maxDataSize = ds
			}
		}
	}

	var enumInternalType irtypes.Type
	if maxDataSize == 0 {
		enumInternalType = irtypes.I32
	} else {
		dataArray := irtypes.NewArray(uint64(maxDataSize), irtypes.I8)
		enumStruct := irtypes.NewStruct(irtypes.I32, dataArray)
		enumStruct.SetName("promise_" + name + "_enum")
		module.NewTypeDef("promise_"+name+"_enum", enumStruct)
		enumInternalType = enumStruct
	}

	// Type, Variant, Instance, Value structs — same pattern as computeEnumLayout
	typeStruct := irtypes.NewStruct()
	typeStruct.SetName("promise_" + name + "_t")
	module.NewTypeDef("promise_"+name+"_t", typeStruct)
	typePtr := irtypes.NewPointer(typeStruct)

	variantStruct := irtypes.NewStruct(typePtr)
	variantStruct.SetName("promise_" + name + "_m")
	module.NewTypeDef("promise_"+name+"_m", variantStruct)
	variantPtr := irtypes.NewPointer(variantStruct)

	instanceStruct := irtypes.NewStruct(variantPtr)
	instanceStruct.SetName("promise_" + name + "_i")
	module.NewTypeDef("promise_"+name+"_i", instanceStruct)
	instancePtr := irtypes.NewPointer(instanceStruct)

	valueFields := []irtypes.Type{irtypes.I8Ptr, instancePtr, irtypes.I32}
	valueFieldLayouts := []FieldLayout{
		{Name: "_vtable", CType: "void*", LLVMType: irtypes.I8Ptr, IsInternal: true},
		{Name: "_instance", CType: "promise_" + name + "_i*", LLVMType: instancePtr, IsInternal: true},
		{Name: "tag", CType: "int32_t", LLVMType: irtypes.I32, IsInternal: false},
	}
	if maxDataSize > 0 {
		dataArray := irtypes.NewArray(uint64(maxDataSize), irtypes.I8)
		valueFields = append(valueFields, dataArray)
		valueFieldLayouts = append(valueFieldLayouts, FieldLayout{
			Name: "data", CType: "uint8_t", LLVMType: dataArray, IsInternal: false,
		})
	}

	valueStruct := irtypes.NewStruct(valueFields...)
	valueStruct.SetName("promise_" + name + "_v")
	module.NewTypeDef("promise_"+name+"_v", valueStruct)

	return &TypeDeclLayout{
		PromiseName:        name,
		Kind:               LayoutEnum,
		EnumInternalType:   enumInternalType,
		VariantTag:         variantTag,
		VariantDataTypes:   variantDataTypes,
		MaxVariantDataSize: maxDataSize,
		Type: &StructLayout{
			CName: "promise_" + name + "_t", Suffix: "_t",
			Fields: []FieldLayout{}, LLVMType: typeStruct,
		},
		Variant: &StructLayout{
			CName: "promise_" + name + "_m", Suffix: "_m",
			Fields: []FieldLayout{
				{Name: "_type", CType: "promise_" + name + "_t*", LLVMType: typePtr, IsInternal: true},
			},
			LLVMType: variantStruct,
		},
		Instance: &StructLayout{
			CName: "promise_" + name + "_i", Suffix: "_i",
			Fields: []FieldLayout{
				{Name: "_variant", CType: "promise_" + name + "_m*", LLVMType: variantPtr, IsInternal: true},
			},
			LLVMType: instanceStruct,
		},
		Value: &StructLayout{
			CName: "promise_" + name + "_v", Suffix: "_v",
			Fields: valueFieldLayouts, LLVMType: valueStruct,
		},
	}
}

// computeMonoLayouts computes layouts for all monomorphic type instances.
func (c *Compiler) computeMonoLayouts(instances []*types.Instance) {
	for _, inst := range instances {
		name := monoName(inst)
		switch origin := inst.Origin().(type) {
		case *types.Named:
			if len(origin.TypeParams()) == 0 {
				continue
			}
			if _, exists := c.monoLayouts[name]; exists {
				continue // already computed (e.g., same instance from main file)
			}
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			c.monoLayouts[name] = computeMonoUserTypeLayout(c.module, origin, name, subst, c.layouts)
		case *types.Enum:
			if len(origin.TypeParams()) == 0 {
				continue
			}
			if _, exists := c.monoEnumLayouts[name]; exists {
				continue
			}
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			c.monoEnumLayouts[name] = computeMonoEnumLayout(c.module, origin, name, subst, c.ptrSize())
		}
	}
}

// declareMonoMethods declares LLVM functions for methods on monomorphic user type instances.
func (c *Compiler) declareMonoMethods(file *ast.File, instances []*types.Instance) {
	for _, inst := range instances {
		named, ok := inst.Origin().(*types.Named)
		if !ok {
			continue
		}
		name := monoName(inst)
		subst := types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())

		// Find the TypeDecl AST node for this type
		td := c.findTypeDecl(file, named.Obj().Name())
		if td == nil {
			continue
		}

		for _, md := range td.Methods {
			if md.Body == nil {
				continue
			}
			m := c.lookupAnyMethod(named, md.Name, md.IsGetter, md.IsSetter)
			if m == nil || m.Sig() == nil {
				continue
			}

			mangledName := mangleMethodName(name, md.Name, md.IsSetter)
			if _, exists := c.funcs[mangledName]; exists {
				continue // already declared (e.g., same instance from main file)
			}

			var params []*ir.Param
			if m.Sig().Recv() != nil {
				params = append(params, ir.NewParam("this", irtypes.I8Ptr))
			}

			// Substitute param types
			c.typeSubst = subst
			for _, p := range m.Sig().Params() {
				params = append(params, ir.NewParam(p.Name(), c.resolveType(p.Type())))
			}

			retType := irtypes.Type(irtypes.Void)
			if m.Sig().Result() != nil {
				retType = c.resolveType(m.Sig().Result())
			}
			c.typeSubst = nil

			if m.Sig().CanError() {
				retType = computeResultType(retType)
			}

			fn := c.module.NewFunc(mangledName, retType, params...)
			c.funcs[mangledName] = fn
			if c.compilingModule != "" {
				c.moduleOwnedFuncs[mangledName] = c.compilingModule
			}
		}
	}
}

// defineMonoMethods generates method bodies for monomorphic user type instances.
func (c *Compiler) defineMonoMethods(file *ast.File, instances []*types.Instance) {
	for _, inst := range instances {
		named, ok := inst.Origin().(*types.Named)
		if !ok {
			continue
		}
		name := monoName(inst)
		subst := types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())

		td := c.findTypeDecl(file, named.Obj().Name())
		if td == nil {
			continue
		}

		for _, md := range td.Methods {
			if md.Body == nil {
				continue
			}
			m := c.lookupAnyMethod(named, md.Name, md.IsGetter, md.IsSetter)
			if m == nil || m.Sig() == nil {
				continue
			}

			mangledName := mangleMethodName(name, md.Name, md.IsSetter)
			fn, ok := c.funcs[mangledName]
			if !ok {
				continue
			}

			c.typeSubst = subst
			c.monoCtx = &monoContext{inst: inst, origin: named, name: name}
			func() {
				defer func() { c.typeSubst = nil; c.monoCtx = nil }()
				if elemType := c.info.GeneratorFuncs[md]; elemType != nil {
					c.defineGeneratorMethod(md, m, fn, elemType, named)
				} else {
					c.defineMethodFunc(md, m, fn, named)
				}
			}()
		}
	}
}

// declareMonoFuncs declares LLVM functions for monomorphic generic function instances.
func (c *Compiler) declareMonoFuncs(file *ast.File, funcInsts []*sema.FuncInstance) {
	for _, fi := range funcInsts {
		name := monoFuncName(fi)
		if _, exists := c.funcs[name]; exists {
			continue // already declared (e.g., same instance from main file)
		}
		fd := c.findFuncDecl(file, fi.Func.Name())
		if fd == nil || fd.Body == nil {
			continue
		}

		sig := fi.Func.Type().(*types.Signature)
		subst := types.BuildSubstMap(sig.TypeParams(), fi.TypeArgs)

		c.typeSubst = subst
		retType := irtypes.Type(irtypes.Void)
		if sig.Result() != nil {
			retType = c.resolveType(sig.Result())
		}
		if sig.CanError() {
			retType = computeResultType(retType)
		}

		var params []*ir.Param
		for _, p := range sig.Params() {
			params = append(params, ir.NewParam(p.Name(), c.resolveType(p.Type())))
		}
		c.typeSubst = nil

		fn := c.module.NewFunc(name, retType, params...)
		c.funcs[name] = fn
		if c.compilingModule != "" {
			c.moduleOwnedFuncs[name] = c.compilingModule
		}
	}
}

// defineMonoFuncs generates function bodies for monomorphic generic function instances.
func (c *Compiler) defineMonoFuncs(file *ast.File, funcInsts []*sema.FuncInstance) {
	for _, fi := range funcInsts {
		name := monoFuncName(fi)
		fd := c.findFuncDecl(file, fi.Func.Name())
		if fd == nil || fd.Body == nil {
			continue
		}

		fn, ok := c.funcs[name]
		if !ok {
			continue
		}

		sig := fi.Func.Type().(*types.Signature)
		subst := types.BuildSubstMap(sig.TypeParams(), fi.TypeArgs)

		c.typeSubst = subst
		func() {
			defer func() { c.typeSubst = nil }()
			if elemType := c.info.GeneratorFuncs[fd]; elemType != nil {
				c.defineGeneratorFunc(fd, fn, elemType)
			} else {
				c.defineFunc(fd, fn)
			}
		}()
	}
}

// findTypeDecl finds a TypeDecl AST node by name.
func (c *Compiler) findTypeDecl(file *ast.File, name string) *ast.TypeDecl {
	for _, decl := range file.Decls {
		if td, ok := decl.(*ast.TypeDecl); ok && td.Name == name {
			return td
		}
	}
	return nil
}

// findFuncDecl finds a FuncDecl AST node by name.
func (c *Compiler) findFuncDecl(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name == name {
			return fd
		}
	}
	return nil
}
