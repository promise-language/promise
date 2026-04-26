package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// assignTypeID assigns a unique type ID to a Named type. Returns the existing
// ID if one was already assigned.
func (c *Compiler) assignTypeID(named *types.Named) int32 {
	if id, ok := c.typeIDs[named]; ok {
		return id
	}
	id := c.nextTypeID
	c.nextTypeID++
	c.typeIDs[named] = id
	return id
}

// collectAllParentIDs recursively collects all ancestor type IDs (transitive).
// Returns a deduplicated slice.
func (c *Compiler) collectAllParentIDs(named *types.Named) []int32 {
	var ids []int32
	for _, p := range named.Parents() {
		ids = append(ids, c.assignTypeID(p))
		ids = append(ids, c.collectAllParentIDs(p)...)
	}
	// Deduplicate
	seen := make(map[int32]bool)
	var unique []int32
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	return unique
}

// emitTypeInfo creates a global type info constant for a Named type.
// Layout: { i32 type_id, i32 num_parents, [N x i32] parent_ids }
func (c *Compiler) emitTypeInfo(named *types.Named) *ir.Global {
	typeID := c.assignTypeID(named)
	parentIDs := c.collectAllParentIDs(named)
	numParents := len(parentIDs)

	var structType *irtypes.StructType
	var fields []constant.Constant

	fields = append(fields, constant.NewInt(irtypes.I32, int64(typeID)))
	fields = append(fields, constant.NewInt(irtypes.I32, int64(numParents)))

	if numParents > 0 {
		arrayType := irtypes.NewArray(uint64(numParents), irtypes.I32)
		structType = irtypes.NewStruct(irtypes.I32, irtypes.I32, arrayType)

		var parentConsts []constant.Constant
		for _, pid := range parentIDs {
			parentConsts = append(parentConsts, constant.NewInt(irtypes.I32, int64(pid)))
		}
		parentArray := constant.NewArray(arrayType, parentConsts...)
		fields = append(fields, parentArray)
	} else {
		structType = irtypes.NewStruct(irtypes.I32, irtypes.I32)
	}

	init := constant.NewStruct(structType, fields...)
	globalName := "promise_typeinfo_" + named.Obj().Name()
	global := c.module.NewGlobalDef(globalName, init)
	global.Immutable = true
	return global
}

// emitTypeInfoGlobals creates global type info constants for all user types in the file.
// Generic types are skipped (they get type info through monomorphization).
func (c *Compiler) emitTypeInfoGlobals(file *ast.File) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		named := c.lookupNamedType(td.Name)
		if named == nil {
			continue
		}
		if len(named.TypeParams()) > 0 {
			continue // generic — monomorphic instances handled separately
		}
		c.typeInfoGlobals[named] = c.emitTypeInfo(named)
	}
}
