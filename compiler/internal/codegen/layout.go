package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/types"
)

// LayoutKind classifies the type for layout computation.
type LayoutKind int

const (
	LayoutPrimitive LayoutKind = iota
	LayoutString
	LayoutUserType
	LayoutEnum
	LayoutValueType // pure value type — all fields in value struct, no heap alloc
	// Future stages:
	// LayoutArray
	// LayoutSlice
	// LayoutTuple
	// LayoutOptional
)

// TypeDeclLayout holds the four struct layouts for a Promise type declaration.
type TypeDeclLayout struct {
	PromiseName        string
	Kind               LayoutKind
	Value              *StructLayout        // T#v — vtable_ptr + instance_ptr + value fields
	Instance           *StructLayout        // T#i — variant_ptr + default fields
	Variant            *StructLayout        // T#m — type_ptr + variant fields
	Type               *StructLayout        // T#t — type fields + metadata
	RawLLVM            irtypes.Type         // raw LLVM scalar type (i64, double, i8 for bool)
	RawCType           string               // C type string ("int64_t", "double", "uint8_t")
	IsSigned           bool                 // for integer widening in coercion
	InstanceFieldIndex map[string]int       // field name → GEP index in instance struct (user types)
	InstancePtrType    *irtypes.PointerType // cached pointer to instance struct (user types + string)

	// Value-type-specific fields
	IsValueType     bool           // true if all fields are value-placed (no heap alloc)
	ValueFieldIndex map[string]int // field name → index in value struct (starts at 1)

	// Enum-specific fields
	EnumInternalType   irtypes.Type                   // i32 (fieldless) or { i32, [N x i8] } (data enum)
	VariantTag         map[string]int                 // variant name → tag value (0-indexed)
	VariantDataTypes   map[string]*irtypes.StructType // variant name → data struct type (for GEP)
	MaxVariantDataSize int                            // max variant data byte count (0 = fieldless)
}

// StructLayout describes one of the four C-ABI-compatible struct representations.
type StructLayout struct {
	CName         string              // C identifier: "promise_int_v"
	Suffix        string              // "_v", "_i", "_m", "_t"
	Fields        []FieldLayout       // ordered fields
	LLVMType      *irtypes.StructType // resolved LLVM named struct type
	IsFlexible    bool                // true if last field is a C99 flexible array member
	FlexElemCType string              // C element type of flexible member ("char")
}

// FieldLayout describes a single field in a struct layout.
type FieldLayout struct {
	Name       string       // field name: "raw", "_vtable", "_instance"
	CType      string       // C type string: "int64_t", "void*"
	LLVMType   irtypes.Type // LLVM type for this field
	IsInternal bool         // true for _vtable, _instance, _variant, _type
}

// ExternFunc captures an extern function declaration for codegen and header gen.
type ExternFunc struct {
	PromiseName    string           // Promise function name
	CName          string           // C symbol name
	Sig            *types.Signature // type system signature
	IRFunc         *ir.Func         // LLVM IR function declaration
	ParamTypes     []types.Type     // Promise types of each parameter
	ResultType     types.Type       // Promise return type (nil for void)
	HasSret        bool             // true if return uses sret pointer (large struct return)
	IsFailable     bool             // true if the extern is failable (returns T!)
	WasmImportMod  string           // WASM import module name (from `wasm_import annotation)
	WasmImportName string           // WASM import name (from `wasm_import annotation)
}

// CompileResult bundles the output of compilation for downstream consumers.
type CompileResult struct {
	Module          *ir.Module
	Layouts         map[*types.Named]*TypeDeclLayout
	EnumLayouts     map[*types.Enum]*TypeDeclLayout
	Externs         []*ExternFunc
	CoverageRegions []CoverageRegion // coverage region metadata (populated when coverage is enabled)
	compiler        *Compiler        // internal reference for GenerateTestMain
}

// SemaInfo returns the main sema.Info used for this compilation.
// Used by main.go for per-instance .bc cache key computation.
func (r *CompileResult) SemaInfo() *sema.Info {
	return r.compiler.rootInfo
}

// CoverageEnabled reports whether this compilation instrumented for coverage.
// Used by main.go to isolate coverage instance .bc files from normal-build
// cache entries (T0574): coverage globals are externally linked, so a
// non-coverage build reusing a cached coverage instance .bc would hit an
// undefined-symbol link error (and vice versa would silently undercount).
func (r *CompileResult) CoverageEnabled() bool {
	return r.compiler.coverageEnabled
}

// primitiveRawType returns the raw LLVM type, C type string, and signedness
// for a primitive Named type. Returns nil/""/false for non-primitives.
func primitiveRawType(n *types.Named) (irtypes.Type, string, bool) {
	switch n {
	case types.TypInt, types.TypI64:
		return irtypes.I64, "int64_t", true
	case types.TypI32:
		return irtypes.I32, "int32_t", true
	case types.TypI16:
		return irtypes.I16, "int16_t", true
	case types.TypI8:
		return irtypes.I8, "int8_t", true
	case types.TypUint, types.TypU64:
		return irtypes.I64, "uint64_t", false
	case types.TypU32:
		return irtypes.I32, "uint32_t", false
	case types.TypU16:
		return irtypes.I16, "uint16_t", false
	case types.TypU8:
		return irtypes.I8, "uint8_t", false
	case types.TypF64:
		return irtypes.Double, "double", false
	case types.TypF32:
		return irtypes.Float, "float", false
	case types.TypBool:
		return irtypes.I8, "uint8_t", false // i8, not i1 — C ABI compatible
	case types.TypChar:
		return irtypes.I32, "int32_t", true // Unicode codepoint
	default:
		return nil, "", false
	}
}

// computePrimitiveLayout creates a TypeDeclLayout for a single primitive type.
// It registers four named LLVM struct types in the module: _t, _m, _i, _v.
func computePrimitiveLayout(module *ir.Module, named *types.Named) *TypeDeclLayout {
	rawLLVM, rawCType, isSigned := primitiveRawType(named)
	if rawLLVM == nil {
		return nil
	}
	name := named.Obj().Name()

	// Type struct: empty {} — per-declaration metadata (none for primitives)
	typeStruct := irtypes.NewStruct()
	typeStruct.SetName("promise_" + name + "_t")
	module.NewTypeDef("promise_"+name+"_t", typeStruct)

	// Shared pointer types — allocated once, reused in both struct fields and FieldLayouts
	typePtr := irtypes.NewPointer(typeStruct)

	// Variant struct: { promise_T_t* _type }
	variantStruct := irtypes.NewStruct(typePtr)
	variantStruct.SetName("promise_" + name + "_m")
	module.NewTypeDef("promise_"+name+"_m", variantStruct)

	variantPtr := irtypes.NewPointer(variantStruct)

	// Instance struct: { promise_T_m* _variant }
	instanceStruct := irtypes.NewStruct(variantPtr)
	instanceStruct.SetName("promise_" + name + "_i")
	module.NewTypeDef("promise_"+name+"_i", instanceStruct)

	instancePtr := irtypes.NewPointer(instanceStruct)

	// Value struct: { i8* _vtable, promise_T_i* _instance, <raw> raw }
	valueStruct := irtypes.NewStruct(
		irtypes.I8Ptr, // _vtable
		instancePtr,   // _instance
		rawLLVM,       // raw
	)
	valueStruct.SetName("promise_" + name + "_v")
	module.NewTypeDef("promise_"+name+"_v", valueStruct)

	return &TypeDeclLayout{
		PromiseName: name,
		Kind:        LayoutPrimitive,
		RawLLVM:     rawLLVM,
		RawCType:    rawCType,
		IsSigned:    isSigned,
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
			CName:  "promise_" + name + "_v",
			Suffix: "_v",
			Fields: []FieldLayout{
				{Name: "_vtable", CType: "void*", LLVMType: irtypes.I8Ptr, IsInternal: true},
				{Name: "_instance", CType: "promise_" + name + "_i*", LLVMType: instancePtr, IsInternal: true},
				{Name: "raw", CType: rawCType, LLVMType: rawLLVM, IsInternal: false},
			},
			LLVMType: valueStruct,
		},
	}
}

// computeStringLayout creates a TypeDeclLayout for the string type.
// String uses a flexible array member in the Instance struct for inline UTF-8 data.
func computeStringLayout(module *ir.Module) *TypeDeclLayout {
	name := "string"

	// Type struct: empty {}
	typeStruct := irtypes.NewStruct()
	typeStruct.SetName("promise_" + name + "_t")
	module.NewTypeDef("promise_"+name+"_t", typeStruct)

	typePtr := irtypes.NewPointer(typeStruct)

	// Variant struct: { promise_string_t* _type }
	variantStruct := irtypes.NewStruct(typePtr)
	variantStruct.SetName("promise_" + name + "_m")
	module.NewTypeDef("promise_"+name+"_m", variantStruct)

	variantPtr := irtypes.NewPointer(variantStruct)

	// Instance struct: { promise_string_m* _variant, i64 len, [0 x i8] data }
	flexArray := irtypes.NewArray(0, irtypes.I8)
	instanceStruct := irtypes.NewStruct(variantPtr, irtypes.I64, flexArray)
	instanceStruct.SetName("promise_" + name + "_i")
	module.NewTypeDef("promise_"+name+"_i", instanceStruct)

	instancePtr := irtypes.NewPointer(instanceStruct)

	// Value struct: { i8* _vtable, promise_string_i* _instance } — NO raw field
	valueStruct := irtypes.NewStruct(irtypes.I8Ptr, instancePtr)
	valueStruct.SetName("promise_" + name + "_v")
	module.NewTypeDef("promise_"+name+"_v", valueStruct)

	return &TypeDeclLayout{
		PromiseName: name,
		Kind:        LayoutString,
		RawLLVM:     nil, // no raw field for strings
		RawCType:    "",
		IsSigned:    false,
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
				{Name: "len", CType: "int64_t", LLVMType: irtypes.I64, IsInternal: false},
				{Name: "data", CType: "char", LLVMType: flexArray, IsInternal: false},
			},
			LLVMType:      instanceStruct,
			IsFlexible:    true,
			FlexElemCType: "char",
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

// computeUserTypeLayout creates a TypeDeclLayout for a user-defined Named type.
// It registers four named LLVM struct types in the module: _t, _m, _i, _v.
// Only PlaceInstance fields are supported; other placements panic.
func computeUserTypeLayout(module *ir.Module, named *types.Named, allLayouts map[*types.Named]*TypeDeclLayout, ptrSize int, enumLayouts map[*types.Enum]*TypeDeclLayout, monoEnumLayouts map[string]*TypeDeclLayout, monoLayouts map[string]*TypeDeclLayout) *TypeDeclLayout {
	name := named.Obj().Name()

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

	// Build substitution for inherited fields from generic parents
	parentSubst := buildParentFieldSubst(named)

	for _, f := range named.AllFields() {
		if f.Placement() != types.PlaceInstance {
			panic("codegen: non-instance field placement not yet supported for " + name + "." + f.Name())
		}
		fType := types.Substitute(f.Type(), parentSubst)
		llvmFT := instanceFieldLLVMType(fType, allLayouts, ptrSize, enumLayouts, monoEnumLayouts, monoLayouts)
		cType := userFieldCType(fType, allLayouts)
		instanceLLVMFields = append(instanceLLVMFields, llvmFT)
		idx := len(fieldLayouts) // GEP index
		fieldLayouts = append(fieldLayouts, FieldLayout{
			Name: f.Name(), CType: cType, LLVMType: llvmFT, IsInternal: false,
		})
		fieldIndex[f.Name()] = idx
	}

	instanceStruct := irtypes.NewStruct(instanceLLVMFields...)
	instanceStruct.SetName("promise_" + name + "_i")
	module.NewTypeDef("promise_"+name+"_i", instanceStruct)

	instancePtr := irtypes.NewPointer(instanceStruct)

	// Value struct: { i8* _vtable, promise_T_i* _instance } — no user fields
	valueStruct := irtypes.NewStruct(irtypes.I8Ptr, instancePtr)
	valueStruct.SetName("promise_" + name + "_v")
	module.NewTypeDef("promise_"+name+"_v", valueStruct)

	return &TypeDeclLayout{
		PromiseName:        name,
		Kind:               LayoutUserType,
		RawLLVM:            nil,
		RawCType:           "",
		IsSigned:           false,
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

// buildParentFieldSubst builds a substitution map for inherited fields from
// generic parents. Recursively walks the entire parent chain so that transitive
// inheritance (e.g., Leaf is Middle[int] is Base[T]) resolves all type params.
func buildParentFieldSubst(named *types.Named) map[*types.TypeParam]types.Type {
	subst := make(map[*types.TypeParam]types.Type)
	mergeParentFieldSubst(named, subst)
	if len(subst) == 0 {
		return nil
	}
	return subst
}

// mergeParentFieldSubst recursively adds parent type param mappings to subst.
func mergeParentFieldSubst(named *types.Named, subst map[*types.TypeParam]types.Type) {
	for _, pr := range named.Parents() {
		if len(pr.TypeArgs) == 0 {
			// Non-generic parent — still recurse for its parents.
			mergeParentFieldSubst(pr.Named, subst)
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
		mergeParentFieldSubst(pr.Named, subst)
	}
}

// computeValueTypeLayout creates a TypeDeclLayout for a pure value type (all fields `value).
// Data lives in the value struct: { i8* _vtable, field1, field2, ... }.
// Instance struct is RTTI-only (no user fields), allocated as a global singleton.
func computeValueTypeLayout(module *ir.Module, named *types.Named, allLayouts map[*types.Named]*TypeDeclLayout, ptrSize int, enumLayouts map[*types.Enum]*TypeDeclLayout, monoEnumLayouts map[string]*TypeDeclLayout, monoLayouts map[string]*TypeDeclLayout) *TypeDeclLayout {
	name := named.Obj().Name()

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

	// Value struct: { i8* _vtable, field1, field2, ... }
	// RTTI is accessed via the compile-time-known global, not stored in the value struct.
	valueLLVMFields := []irtypes.Type{irtypes.I8Ptr}
	valueFieldLayouts := []FieldLayout{
		{Name: "_vtable", CType: "void*", LLVMType: irtypes.I8Ptr, IsInternal: true},
	}
	fieldIndex := map[string]int{}

	for _, f := range named.AllFields() {
		llvmFT := instanceFieldLLVMType(f.Type(), allLayouts, ptrSize, enumLayouts, monoEnumLayouts, monoLayouts)
		cType := userFieldCType(f.Type(), allLayouts)
		idx := len(valueFieldLayouts) // GEP index in value struct
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
		RawLLVM:         nil,
		RawCType:        "",
		IsSigned:        false,
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

// userFieldCType returns the C type string for a user type field.
// Primitives use their raw C type; arrays use element C type + [N]; strings and user types use "void*".
func userFieldCType(typ types.Type, allLayouts map[*types.Named]*TypeDeclLayout) string {
	if arr, ok := typ.(*types.Array); ok {
		elemCType := userFieldCType(arr.Elem(), allLayouts)
		return fmt.Sprintf("%s[%d]", elemCType, arr.Size())
	}
	named := extractNamed(typ)
	if named == nil {
		return "void*"
	}
	if layout, ok := allLayouts[named]; ok && layout.RawCType != "" {
		return layout.RawCType
	}
	return "void*"
}

// computeLayouts computes TypeDeclLayout for all built-in types and any additional
// types reachable from extern signatures. All built-in types are always included
// so the generated header is complete for runtime compilation and IDE support.
func computeLayouts(module *ir.Module, externs []*ExternFunc) map[*types.Named]*TypeDeclLayout {
	layouts := make(map[*types.Named]*TypeDeclLayout)

	// Always compute layouts for all built-in types
	layouts[types.TypString] = computeStringLayout(module)

	builtins := []*types.Named{
		types.TypBool, types.TypChar,
		types.TypF32, types.TypF64,
		types.TypI8, types.TypI16, types.TypI32, types.TypI64, types.TypInt,
		types.TypU8, types.TypU16, types.TypU32, types.TypU64, types.TypUint,
	}
	for _, named := range builtins {
		layouts[named] = computePrimitiveLayout(module, named)
	}

	// Also compute layouts for any non-builtin types in extern signatures
	for _, ext := range externs {
		var namedTypes []*types.Named
		for _, pt := range ext.ParamTypes {
			if n := extractNamed(pt); n != nil {
				namedTypes = append(namedTypes, n)
			}
		}
		if ext.ResultType != nil {
			if n := extractNamed(ext.ResultType); n != nil {
				namedTypes = append(namedTypes, n)
			}
		}

		for _, named := range namedTypes {
			if _, ok := layouts[named]; ok {
				continue // already computed (built-in, user type, or duplicate)
			}
			layout := computePrimitiveLayout(module, named)
			if layout != nil {
				layouts[named] = layout
			}
			// User types in extern signatures are handled by computeUserTypeLayouts
		}
	}

	return layouts
}

// collectExterns scans the AST for extern function declarations.
func collectExterns(file *ast.File, info *sema.Info) []*ExternFunc {
	var externs []*ExternFunc
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body != nil {
			continue // skip non-extern
		}

		sig := lookupFuncSig(fd.Name, info)
		if sig == nil {
			continue
		}

		cName := externCName(fd)

		paramTypes := make([]types.Type, len(sig.Params()))
		for i, p := range sig.Params() {
			paramTypes[i] = p.Type()
		}

		wasmMod, wasmName := sema.ExtractWasmImport(fd.Annotations)

		externs = append(externs, &ExternFunc{
			PromiseName:    fd.Name,
			CName:          cName,
			Sig:            sig,
			ParamTypes:     paramTypes,
			ResultType:     sig.Result(),
			IsFailable:     sig.CanError(),
			WasmImportMod:  wasmMod,
			WasmImportName: wasmName,
		})
	}
	return externs
}

// externCName extracts the C symbol name from an extern function declaration.
// If the `extern annotation has a string parameter, that is used as the C name.
// Otherwise, defaults to "promise_" + funcName.
func externCName(fd *ast.FuncDecl) string {
	for _, ann := range fd.Annotations {
		if ann.Name == "extern" && len(ann.Params) > 0 {
			sl, ok := ann.Params[0].Value.(*ast.StringLit)
			if !ok {
				continue
			}
			var s string
			for _, p := range sl.Parts {
				if t, ok := p.(ast.StringText); ok {
					s += t.Text
				}
			}
			if s != "" {
				return s
			}
		}
	}
	return "promise_" + fd.Name
}

// computeEnumLayout creates a TypeDeclLayout for an enum type.
// Enums are value types: data lives in the value struct (tag + union data),
// not heap-allocated like user types. The instance struct is empty.
func computeEnumLayout(module *ir.Module, enum *types.Enum, ptrSize int, enumLayouts map[*types.Enum]*TypeDeclLayout) *TypeDeclLayout {
	name := enum.Obj().Name()

	// Compute variant tags and per-variant data struct types
	variantTag := map[string]int{}
	variantDataTypes := map[string]*irtypes.StructType{}
	maxDataSize := 0

	for i, v := range enum.Variants() {
		variantTag[v.Name()] = i

		if v.NumFields() > 0 {
			var fieldTypes []irtypes.Type
			for _, f := range v.Fields() {
				// Use llvmTypeForEnumFieldFromPromise so user-defined types
				// use {i8*, i8*} (value struct) not bare i8* (instance ptr).
				fieldTypes = append(fieldTypes, llvmTypeForEnumFieldFromPromise(f.Type(), ptrSize, enumLayouts, nil))
			}
			dataType := irtypes.NewStruct(fieldTypes...)
			variantDataTypes[v.Name()] = dataType

			// Compute data size from the struct type to account for alignment padding
			ds := llvmTypeSizeWithPtr(dataType, ptrSize)
			if ds > maxDataSize {
				maxDataSize = ds
			}
		}
	}

	// Determine internal LLVM type
	var enumInternalType irtypes.Type
	if maxDataSize == 0 {
		enumInternalType = irtypes.I32 // fieldless enum: tag only
	} else {
		dataArray := irtypes.NewArray(uint64(maxDataSize), irtypes.I8)
		enumStruct := irtypes.NewStruct(irtypes.I32, dataArray)
		enumStruct.SetName("promise_" + name + "_enum")
		module.NewTypeDef("promise_"+name+"_enum", enumStruct)
		enumInternalType = enumStruct
	}

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

	// Instance struct: { promise_T_m* _variant } — empty, enum data is in value struct
	instanceStruct := irtypes.NewStruct(variantPtr)
	instanceStruct.SetName("promise_" + name + "_i")
	module.NewTypeDef("promise_"+name+"_i", instanceStruct)

	instancePtr := irtypes.NewPointer(instanceStruct)

	// Value struct: { i8* _vtable, promise_T_i* _instance, i32 tag [, [N x i8] data] }
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
			Name: fmt.Sprintf("data[%d]", maxDataSize), CType: "uint8_t", LLVMType: dataArray, IsInternal: false,
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

// lookupFuncSig finds a function's signature in sema info by name.
func lookupFuncSig(name string, info *sema.Info) *types.Signature {
	for _, scope := range info.ScopeOrder {
		if obj := scope.Lookup(name); obj != nil {
			if fn, ok := obj.(*types.Func); ok {
				if sig, ok := fn.Type().(*types.Signature); ok {
					return sig
				}
			}
		}
	}
	return nil
}
