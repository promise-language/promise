package codegen

import (
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
	// Future stages:
	// LayoutStruct
	// LayoutEnum
	// LayoutArray
	// LayoutSlice
	// LayoutTuple
	// LayoutOptional
)

// TypeDeclLayout holds the four struct layouts for a Promise type declaration.
type TypeDeclLayout struct {
	PromiseName string
	Kind        LayoutKind
	Value       *StructLayout // T#v — vtable_ptr + instance_ptr + value fields
	Instance    *StructLayout // T#i — variant_ptr + default fields
	Variant     *StructLayout // T#m — type_ptr + variant fields
	Type        *StructLayout // T#t — type fields + metadata
	RawLLVM     irtypes.Type  // raw LLVM scalar type (i64, double, i8 for bool)
	RawCType    string        // C type string ("int64_t", "double", "uint8_t")
	IsSigned    bool          // for integer widening in coercion
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
	PromiseName string           // Promise function name
	CName       string           // C symbol name
	Sig         *types.Signature // type system signature
	IRFunc      *ir.Func         // LLVM IR function declaration
	ParamTypes  []types.Type     // Promise types of each parameter
	ResultType  types.Type       // Promise return type (nil for void)
}

// CompileResult bundles the output of compilation for downstream consumers.
type CompileResult struct {
	Module  *ir.Module
	Layouts map[*types.Named]*TypeDeclLayout
	Externs []*ExternFunc
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
				continue // already computed (built-in or duplicate)
			}
			layout := computePrimitiveLayout(module, named)
			if layout == nil {
				panic("codegen: non-primitive type " + named.Obj().Name() + " in extern signature not yet supported")
			}
			layouts[named] = layout
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

		externs = append(externs, &ExternFunc{
			PromiseName: fd.Name,
			CName:       cName,
			Sig:         sig,
			ParamTypes:  paramTypes,
			ResultType:  sig.Result(),
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

// lookupFuncSig finds a function's signature in sema info by name.
func lookupFuncSig(name string, info *sema.Info) *types.Signature {
	for _, scope := range info.Scopes {
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
