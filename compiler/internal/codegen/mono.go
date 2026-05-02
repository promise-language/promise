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

// mergeParentSubst augments a type param substitution map with mappings for
// inherited generic parent type params. E.g., if Derived[T] is Base[T] and
// subst = {Derived.T → int}, this adds {Base.T → int} so that inherited
// fields/methods using Base.T are correctly resolved.
func mergeParentSubst(origin *types.Named, subst map[*types.TypeParam]types.Type) {
	for _, pr := range origin.Parents() {
		if len(pr.TypeArgs) == 0 {
			// Non-generic parent — still recurse for its parents.
			mergeParentSubst(pr.Named, subst)
			continue
		}
		resolvedArgs := make([]types.Type, len(pr.TypeArgs))
		for i, ta := range pr.TypeArgs {
			resolvedArgs[i] = types.Substitute(ta, subst)
		}
		parentMap := types.BuildSubstMap(pr.Named.TypeParams(), resolvedArgs)
		for k, v := range parentMap {
			subst[k] = v
		}
		// Recurse into parent's parents for transitive chains.
		mergeParentSubst(pr.Named, subst)
	}
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

	// Collect unresolved instances from info.Types (instances with TypeParams).
	// These arise in generic method bodies: e.g., Iterator[T].filter() creates
	// _FnIter[T]. Sema records the expression types but skips recording the
	// instance because it contains TypeParams.
	unresolvedInsts := collectUnresolvedInstances(info)

	// Transitively expand: walk substituted field types, parent instances,
	// and resolve unresolved method-body instances.
	for i := 0; i < len(result); i++ {
		inst := result[i]
		switch origin := inst.Origin().(type) {
		case *types.Named:
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			for _, f := range origin.AllFields() {
				ft := types.Substitute(f.Type(), subst)
				discoverInstances(ft, &result, seen)
			}
			// Discover parent instances: Range[int] is Stream[T] → need Stream[int]
			for _, pr := range origin.Parents() {
				if len(pr.TypeArgs) > 0 {
					resolvedArgs := make([]types.Type, len(pr.TypeArgs))
					for j, ta := range pr.TypeArgs {
						resolvedArgs[j] = types.Substitute(ta, subst)
					}
					parentInst := types.NewInstance(pr.Named, resolvedArgs)
					if !types.ContainsTypeParam(parentInst) {
						discoverInstances(parentInst, &result, seen)
					}
				}
			}
			// Resolve unresolved instances from method bodies.
			// E.g., Iterator[int] has subst {T→int}; _FnIter[T] in method
			// bodies resolves to _FnIter[int].
			if len(subst) > 0 {
				resolveUnresolvedInstances(unresolvedInsts, subst, &result, seen)
			}
		case *types.Enum:
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			for _, v := range origin.Variants() {
				for _, f := range v.Fields() {
					ft := types.Substitute(f.Type(), subst)
					discoverInstances(ft, &result, seen)
				}
			}
			if len(subst) > 0 {
				resolveUnresolvedInstances(unresolvedInsts, subst, &result, seen)
			}
		}
	}

	return result
}

// collectUnresolvedInstances scans info.Types for Instance types that contain
// TypeParams. These come from generic method bodies where sema type-checks
// once with TypeParams unresolved (e.g., _FnIter[T] inside Iterator[T].filter()).
func collectUnresolvedInstances(info *sema.Info) []*types.Instance {
	visited := make(map[*types.Instance]bool)
	var result []*types.Instance
	for _, typ := range info.Types {
		findUnresolvedInstances(typ, &result, visited)
	}
	return result
}

// findUnresolvedInstances recursively walks a type to find Instance types
// that contain TypeParams.
func findUnresolvedInstances(typ types.Type, result *[]*types.Instance, visited map[*types.Instance]bool) {
	if typ == nil {
		return
	}
	switch t := typ.(type) {
	case *types.Instance:
		if types.ContainsTypeParam(t) && !visited[t] {
			visited[t] = true
			*result = append(*result, t)
		}
		for _, arg := range t.TypeArgs() {
			findUnresolvedInstances(arg, result, visited)
		}
	case *types.Optional:
		findUnresolvedInstances(t.Elem(), result, visited)
	case *types.Tuple:
		for _, e := range t.Elems() {
			findUnresolvedInstances(e, result, visited)
		}
	case *types.Signature:
		for _, p := range t.Params() {
			findUnresolvedInstances(p.Type(), result, visited)
		}
		findUnresolvedInstances(t.Result(), result, visited)
	case *types.SharedRef:
		findUnresolvedInstances(t.Elem(), result, visited)
	case *types.MutRef:
		findUnresolvedInstances(t.Elem(), result, visited)
	case *types.Array:
		findUnresolvedInstances(t.Elem(), result, visited)
	}
}

// resolveUnresolvedInstances applies a substitution map to unresolved instances
// and adds any newly concrete instances to the result.
func resolveUnresolvedInstances(unresolved []*types.Instance, subst map[*types.TypeParam]types.Type, result *[]*types.Instance, seen map[string]bool) {
	for _, ui := range unresolved {
		resolved := types.Substitute(ui, subst)
		if resolved == ui {
			continue // substitution didn't change anything
		}
		if types.ContainsTypeParam(resolved) {
			continue // still has unresolved TypeParams
		}
		if ri, ok := resolved.(*types.Instance); ok {
			key := monoName(ri)
			if !seen[key] {
				seen[key] = true
				*result = append(*result, ri)
			}
		}
	}
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

// collectMonoMethodInstancesWithExtra collects mono method instances from modSemaInfo
// plus any extra instances from the caller (e.g. user file) that are instantiations
// of methods on types declared in modFile. This handles cross-module generic method
// calls like iter.map[int](...) in user code where Iterator[T].map is defined in std.
func collectMonoMethodInstancesWithExtra(modSemaInfo *sema.Info, modFile *ast.File, extra []*sema.MethodInstance) []*sema.MethodInstance {
	// Build set of type names declared in modFile
	modTypeNames := make(map[string]bool)
	for _, decl := range modFile.Decls {
		if td, ok := decl.(*ast.TypeDecl); ok {
			modTypeNames[td.Name] = true
		}
	}

	// Start with the module's own instances (deduped)
	result := collectMonoMethodInstances(modSemaInfo)
	seen := make(map[string]bool, len(result))
	for _, mi := range result {
		seen[monoMethodInstanceName(mi)] = true
	}

	// Add extra instances whose owner type is declared in this module
	for _, mi := range extra {
		if !modTypeNames[mi.Owner.Obj().Name()] {
			continue
		}
		name := monoMethodInstanceName(mi)
		if seen[name] {
			continue
		}
		seen[name] = true
		result = append(result, mi)
	}
	return result
}

// collectMonoFuncInstancesWithExtra collects mono func instances from modSemaInfo
// plus any extra instances from the caller (e.g. user file) that are instantiations
// of functions declared in modFile. This handles cross-module generic calls like
// sort[int](...) in user code where sort is defined in the std module.
func collectMonoFuncInstancesWithExtra(modSemaInfo *sema.Info, modFile *ast.File, extra []*sema.FuncInstance) []*sema.FuncInstance {
	// Build set of function names declared in modFile
	modFuncNames := make(map[string]bool)
	for _, decl := range modFile.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok {
			modFuncNames[fd.Name] = true
		}
	}

	// Start with the module's own instances (deduped)
	result := collectMonoFuncInstances(modSemaInfo)
	seen := make(map[string]bool, len(result))
	for _, fi := range result {
		seen[monoFuncName(fi)] = true
	}

	// Add extra instances whose function is declared in this module
	for _, fi := range extra {
		if !modFuncNames[fi.Func.Name()] {
			continue
		}
		name := monoFuncName(fi)
		if seen[name] {
			continue
		}
		seen[name] = true
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

// computeMonoValueTypeLayout computes a TypeDeclLayout for a monomorphic value type instance.
// Value types embed fields directly in the value struct: { i8* _vtable, T_i* _rtti, field1, field2, ... }.
func computeMonoValueTypeLayout(module *ir.Module, named *types.Named, name string, subst map[*types.TypeParam]types.Type, allLayouts map[*types.Named]*TypeDeclLayout) *TypeDeclLayout {
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

	// Instance struct: { promise_T_m* _variant } — RTTI only, no user fields
	instanceStruct := irtypes.NewStruct(variantPtr)
	instanceStruct.SetName("promise_" + name + "_i")
	module.NewTypeDef("promise_"+name+"_i", instanceStruct)

	instancePtr := irtypes.NewPointer(instanceStruct)

	// Value struct: { i8* _vtable, promise_T_i* _rtti, field1, field2, ... }
	valueLLVMFields := []irtypes.Type{irtypes.I8Ptr, instancePtr}
	valueFieldLayouts := []FieldLayout{
		{Name: "_vtable", CType: "void*", LLVMType: irtypes.I8Ptr, IsInternal: true},
		{Name: "_rtti", CType: "promise_" + name + "_i*", LLVMType: instancePtr, IsInternal: true},
	}
	fieldIndex := map[string]int{}

	for _, f := range named.AllFields() {
		fieldType := types.Substitute(f.Type(), subst)
		llvmFT := instanceFieldLLVMType(fieldType, allLayouts)
		cType := userFieldCType(fieldType, allLayouts)
		idx := len(valueFieldLayouts)
		valueLLVMFields = append(valueLLVMFields, llvmFT)
		valueFieldLayouts = append(valueFieldLayouts, FieldLayout{
			Name: f.Name(), CType: cType, LLVMType: llvmFT, IsInternal: false,
		})
		fieldIndex[f.Name()] = idx
	}

	valueStruct := irtypes.NewStruct(valueLLVMFields...)
	valueStruct.SetName("promise_" + name + "_v")
	module.NewTypeDef("promise_"+name+"_v", valueStruct)

	return &TypeDeclLayout{
		PromiseName:     name,
		Kind:            LayoutValueType,
		IsValueType:     true,
		ValueFieldIndex: fieldIndex,
		InstancePtrType: instancePtr,
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
			CName:  "promise_" + name + "_i",
			Suffix: "_i",
			Fields: []FieldLayout{
				{Name: "_variant", CType: "promise_" + name + "_m*", LLVMType: variantPtr, IsInternal: true},
			},
			LLVMType: instanceStruct,
		},
		Value: &StructLayout{
			CName:    "promise_" + name + "_v",
			Suffix:   "_v",
			Fields:   valueFieldLayouts,
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
				// Use llvmTypeForEnumFieldFromPromise so user-defined types
				// use {i8*, i8*} (value struct) not bare i8* (instance ptr).
				fieldTypes = append(fieldTypes, llvmTypeForEnumFieldFromPromise(ft))
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
			mergeParentSubst(origin, subst)
			if origin.IsValueType() {
				c.monoLayouts[name] = computeMonoValueTypeLayout(c.module, origin, name, subst, c.layouts)
			} else {
				c.monoLayouts[name] = computeMonoUserTypeLayout(c.module, origin, name, subst, c.layouts)
			}
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
		// Skip structural types — their default methods are synthesized for
		// concrete implementors via synthesizeDefaultMethods.
		if named.IsStructural() {
			continue
		}
		name := monoName(inst)
		subst := types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
		mergeParentSubst(named, subst)

		// Find the TypeDecl AST node for this type
		td := c.findTypeDecl(file, named.Obj().Name())
		if td == nil {
			continue
		}
		// Verify the found decl matches the mono origin (avoid name collisions
		// with user-defined types sharing the same name as std types)
		if foundNamed := c.lookupNamedType(td.Name); foundNamed != nil && foundNamed != named {
			continue
		}

		for _, md := range td.Methods {
			if md.Body == nil {
				continue
			}
			if len(md.TypeParams) > 0 {
				continue // generic method — handled by mono method instances
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
		// Skip structural types — their default methods are synthesized for
		// concrete implementors via synthesizeDefaultMethods.
		if named.IsStructural() {
			continue
		}
		name := monoName(inst)
		subst := types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
		mergeParentSubst(named, subst)

		td := c.findTypeDecl(file, named.Obj().Name())
		if td == nil {
			continue
		}
		// Verify the found decl matches the mono origin (avoid name collisions
		// with user-defined types sharing the same name as std types)
		if foundNamed := c.lookupNamedType(td.Name); foundNamed != nil && foundNamed != named {
			continue
		}

		for _, md := range td.Methods {
			if md.Body == nil {
				continue
			}
			if len(md.TypeParams) > 0 {
				continue // generic method — handled by mono method instances
			}
			m := c.lookupAnyMethod(named, md.Name, md.IsGetter, md.IsSetter)
			if m == nil || m.Sig() == nil {
				continue
			}

			mangledName := mangleMethodName(name, md.Name, md.IsSetter)
			fn, ok := c.funcs[mangledName]
			if !ok || len(fn.Blocks) > 0 {
				continue // already defined (e.g., from main file mono pass)
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

// declareMonoSynthesizedDefaults declares stubs for default methods from structural
// parents that need to be synthesized for mono instances of concrete types.
// E.g., _FnIter[int] inherits filter/take/skip from Iterator[T] — these become
// _FnIter__int.filter, _FnIter__int.take, etc. Must run BEFORE vtable emission.
func (c *Compiler) declareMonoSynthesizedDefaults(file *ast.File, instances []*types.Instance) {
	for _, inst := range instances {
		named, ok := inst.Origin().(*types.Named)
		if !ok || named.IsStructural() {
			continue
		}
		name := monoName(inst)
		subst := types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
		mergeParentSubst(named, subst)

		for _, pr := range named.Parents() {
			if pr.Named.IsStructural() {
				c.declareStructuralDefaultStubs(file, name, named, pr.Named, subst)
			}
		}
	}
}

// declareStructuralDefaultStubs declares function stubs for default methods from
// a structural interface, using mono-qualified names for the concrete type.
func (c *Compiler) declareStructuralDefaultStubs(file *ast.File, mName string, concrete, iface *types.Named, subst map[*types.TypeParam]types.Type) {
	ifaceTD := c.findTypeDecl(file, iface.Obj().Name())
	if ifaceTD == nil {
		return
	}
	for _, md := range ifaceTD.Methods {
		if md.Body == nil {
			continue
		}
		m := c.lookupAnyMethod(iface, md.Name, md.IsGetter, md.IsSetter)
		if m == nil || m.IsAbstract() {
			continue
		}
		if hasOwnMethod(concrete, md.Name) {
			continue
		}
		if len(md.TypeParams) > 0 {
			continue // generic methods are not virtual
		}
		mangledName := mangleMethodName(mName, md.Name, md.IsSetter)
		if _, exists := c.funcs[mangledName]; exists {
			continue
		}

		sig := m.Sig()
		var params []*ir.Param
		if sig.Recv() != nil {
			params = append(params, ir.NewParam("this", irtypes.I8Ptr))
		}
		c.typeSubst = subst
		for _, p := range sig.Params() {
			params = append(params, ir.NewParam(p.Name(), c.resolveType(p.Type())))
		}
		retType := irtypes.Type(irtypes.Void)
		if sig.Result() != nil {
			retType = c.resolveType(sig.Result())
		}
		c.typeSubst = nil
		if sig.CanError() {
			retType = computeResultType(retType)
		}

		fn := c.module.NewFunc(mangledName, retType, params...)
		c.funcs[mangledName] = fn
	}

	// Recurse into parent interfaces
	for _, pr := range iface.Parents() {
		if pr.Named.IsStructural() {
			c.declareStructuralDefaultStubs(file, mName, concrete, pr.Named, subst)
		}
	}
}

// defineMonoSynthesizedDefaults generates bodies for synthesized default methods
// on mono instances of concrete types with structural parents.
func (c *Compiler) defineMonoSynthesizedDefaults(file *ast.File, instances []*types.Instance) {
	for _, inst := range instances {
		named, ok := inst.Origin().(*types.Named)
		if !ok || named.IsStructural() {
			continue
		}
		name := monoName(inst)
		subst := types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
		mergeParentSubst(named, subst)

		for _, pr := range named.Parents() {
			if pr.Named.IsStructural() {
				c.defineStructuralDefaultBodies(file, name, named, pr.Named, subst, inst)
			}
		}
	}
}

// defineStructuralDefaultBodies generates method bodies for already-declared
// synthesized default method stubs with mono-qualified names.
func (c *Compiler) defineStructuralDefaultBodies(file *ast.File, mName string, concrete, iface *types.Named, subst map[*types.TypeParam]types.Type, inst *types.Instance) {
	ifaceTD := c.findTypeDecl(file, iface.Obj().Name())
	if ifaceTD == nil {
		return
	}
	for _, md := range ifaceTD.Methods {
		if md.Body == nil {
			continue
		}
		m := c.lookupAnyMethod(iface, md.Name, md.IsGetter, md.IsSetter)
		if m == nil || m.IsAbstract() {
			continue
		}
		if hasOwnMethod(concrete, md.Name) {
			continue
		}
		if len(md.TypeParams) > 0 {
			continue
		}
		mangledName := mangleMethodName(mName, md.Name, md.IsSetter)
		fn, ok := c.funcs[mangledName]
		if !ok || len(fn.Blocks) > 0 {
			continue
		}

		saved := c.saveState()
		c.selfSubst = &selfSubstInfo{iface: iface, concrete: concrete}
		c.typeSubst = subst
		c.monoCtx = &monoContext{inst: inst, origin: concrete, name: mName}
		c.defineMethodFunc(md, m, fn, concrete)
		c.restoreState(saved)
	}

	// Recurse into parent interfaces
	for _, pr := range iface.Parents() {
		if pr.Named.IsStructural() {
			c.defineStructuralDefaultBodies(file, mName, concrete, pr.Named, subst, inst)
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
		if !ok || len(fn.Blocks) > 0 {
			continue // skip if not declared or already defined (e.g., from module phase)
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
// collectMonoInstancesWithExtra is like collectMonoInstances but seeds the
// transitive expansion with both the module's own recorded instances and any
// extra instances from the caller (e.g. user-file mono instances of module
// types like Map[string,int]). Only extra instances whose origin type is
// declared in modFile are included. The unresolved-instance expansion uses
// the module's own sema info so that method-body type references (e.g.
// _FnIter[T] inside Vector[T].iter()) are resolved correctly.
func collectMonoInstancesWithExtra(modInfo *sema.ModuleInfo, modFile *ast.File, extra []*types.Instance) []*types.Instance {
	// Build seen set for type names declared in modFile for O(1) membership test.
	modTypeNames := make(map[string]bool)
	for _, decl := range modFile.Decls {
		if td, ok := decl.(*ast.TypeDecl); ok {
			modTypeNames[td.Name] = true
		}
	}

	seen := map[string]bool{}
	var result []*types.Instance

	// Seed with module's own recorded instances.
	for _, inst := range modInfo.SemaInfo.Instances {
		key := monoName(inst)
		if !seen[key] {
			seen[key] = true
			result = append(result, inst)
		}
	}

	// Seed with extra instances that belong to types declared in modFile.
	for _, inst := range extra {
		named, ok := inst.Origin().(*types.Named)
		if !ok {
			continue
		}
		if !modTypeNames[named.Obj().Name()] {
			continue
		}
		key := monoName(inst)
		if !seen[key] {
			seen[key] = true
			result = append(result, inst)
		}
	}

	// Unresolved instances from module's method bodies (e.g. _FnIter[T] inside
	// Vector[T].iter()). These will be resolved transitively for each concrete inst.
	unresolvedInsts := collectUnresolvedInstances(modInfo.SemaInfo)

	// Transitively expand (same logic as collectMonoInstances).
	for i := 0; i < len(result); i++ {
		inst := result[i]
		switch origin := inst.Origin().(type) {
		case *types.Named:
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			for _, f := range origin.AllFields() {
				ft := types.Substitute(f.Type(), subst)
				discoverInstances(ft, &result, seen)
			}
			for _, pr := range origin.Parents() {
				if len(pr.TypeArgs) > 0 {
					resolvedArgs := make([]types.Type, len(pr.TypeArgs))
					for j, ta := range pr.TypeArgs {
						resolvedArgs[j] = types.Substitute(ta, subst)
					}
					parentInst := types.NewInstance(pr.Named, resolvedArgs)
					if !types.ContainsTypeParam(parentInst) {
						discoverInstances(parentInst, &result, seen)
					}
				}
			}
			if len(subst) > 0 {
				resolveUnresolvedInstances(unresolvedInsts, subst, &result, seen)
			}
		case *types.Enum:
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			for _, v := range origin.Variants() {
				for _, f := range v.Fields() {
					ft := types.Substitute(f.Type(), subst)
					discoverInstances(ft, &result, seen)
				}
			}
			if len(subst) > 0 {
				resolveUnresolvedInstances(unresolvedInsts, subst, &result, seen)
			}
		}
	}

	return result
}

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

// monoMethodInstanceName generates a unique mangled name for a generic method instantiation.
// Example: Box.transform[string] → "Box.transform__string"
// Example: Box[int].transform[string] → "Box__int.transform__string"
func monoMethodInstanceName(mi *sema.MethodInstance) string {
	ownerName := mi.Owner.Obj().Name()
	if mi.OwnerInst != nil {
		ownerName = monoName(mi.OwnerInst)
	}
	base := mangleMethodName(ownerName, mi.Method.Name(), false)
	for _, arg := range mi.TypeArgs {
		base += "__" + typeArgSuffix(arg)
	}
	return base
}

// collectMonoMethodInstances deduplicates generic method instantiations.
func collectMonoMethodInstances(info *sema.Info) []*sema.MethodInstance {
	seen := map[string]bool{}
	var result []*sema.MethodInstance
	for _, mi := range info.MethodInstances {
		key := monoMethodInstanceName(mi)
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, mi)
	}
	return result
}

// buildMethodInstanceSubst builds the combined substitution map for a generic method instance.
// This merges owner type params (if generic type) + method type params.
func buildMethodInstanceSubst(mi *sema.MethodInstance) map[*types.TypeParam]types.Type {
	subst := map[*types.TypeParam]types.Type{}
	// Owner type-level substitution (if on a generic type instance)
	if mi.OwnerInst != nil {
		for k, v := range types.BuildSubstMap(mi.Owner.TypeParams(), mi.OwnerInst.TypeArgs()) {
			subst[k] = v
		}
		mergeParentSubst(mi.Owner, subst)
	}
	// Method-level substitution
	for k, v := range types.BuildSubstMap(mi.Method.Sig().TypeParams(), mi.TypeArgs) {
		subst[k] = v
	}
	return subst
}

// declareMonoMethodInstances declares LLVM functions for monomorphic generic method instances.
func (c *Compiler) declareMonoMethodInstances(file *ast.File, methodInsts []*sema.MethodInstance) {
	for _, mi := range methodInsts {
		name := monoMethodInstanceName(mi)
		if _, exists := c.funcs[name]; exists {
			continue
		}

		td := c.findTypeDecl(file, mi.Owner.Obj().Name())
		if td == nil {
			continue
		}

		// Find the method decl
		var md *ast.MethodDecl
		for _, m := range td.Methods {
			if m.Name == mi.Method.Name() && !m.IsGetter && !m.IsSetter {
				md = m
				break
			}
		}
		if md == nil || md.Body == nil {
			continue
		}

		subst := buildMethodInstanceSubst(mi)

		var params []*ir.Param
		if mi.Method.Sig().Recv() != nil {
			params = append(params, ir.NewParam("this", irtypes.I8Ptr))
		}

		c.typeSubst = subst
		for _, p := range mi.Method.Sig().Params() {
			params = append(params, ir.NewParam(p.Name(), c.resolveType(p.Type())))
		}
		retType := irtypes.Type(irtypes.Void)
		if mi.Method.Sig().Result() != nil {
			retType = c.resolveType(mi.Method.Sig().Result())
		}
		c.typeSubst = nil

		if mi.Method.Sig().CanError() {
			retType = computeResultType(retType)
		}

		fn := c.module.NewFunc(name, retType, params...)
		c.funcs[name] = fn
		if c.compilingModule != "" {
			c.moduleOwnedFuncs[name] = c.compilingModule
		}
	}
}

// defineMonoMethodInstances generates method bodies for monomorphic generic method instances.
func (c *Compiler) defineMonoMethodInstances(file *ast.File, methodInsts []*sema.MethodInstance) {
	for _, mi := range methodInsts {
		name := monoMethodInstanceName(mi)

		td := c.findTypeDecl(file, mi.Owner.Obj().Name())
		if td == nil {
			continue
		}

		var md *ast.MethodDecl
		for _, m := range td.Methods {
			if m.Name == mi.Method.Name() && !m.IsGetter && !m.IsSetter {
				md = m
				break
			}
		}
		if md == nil || md.Body == nil {
			continue
		}

		fn, ok := c.funcs[name]
		if !ok || len(fn.Blocks) > 0 {
			continue
		}

		subst := buildMethodInstanceSubst(mi)
		m := c.lookupAnyMethod(mi.Owner, md.Name, false, false)
		if m == nil {
			continue
		}

		c.typeSubst = subst
		if mi.OwnerInst != nil {
			c.monoCtx = &monoContext{
				inst:   mi.OwnerInst,
				origin: mi.Owner,
				name:   monoName(mi.OwnerInst),
			}
		}
		func() {
			defer func() { c.typeSubst = nil; c.monoCtx = nil }()
			c.defineMethodFunc(md, m, fn, mi.Owner)
		}()
	}
}
