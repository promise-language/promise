package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// lookupNamedType finds a Named type in sema info by name.
func (c *Compiler) lookupNamedType(name string) *types.Named {
	for _, scope := range c.info.ScopeOrder {
		if obj := scope.Lookup(name); obj != nil {
			if tn, ok := obj.(*types.TypeName); ok {
				if named, ok := tn.Type().(*types.Named); ok {
					return named
				}
			}
		}
	}
	return nil
}

// lookupEnumType finds an Enum type in sema info by name.
func (c *Compiler) lookupEnumType(name string) *types.Enum {
	for _, scope := range c.info.ScopeOrder {
		if obj := scope.Lookup(name); obj != nil {
			if tn, ok := obj.(*types.TypeName); ok {
				if enum, ok := tn.Type().(*types.Enum); ok {
					return enum
				}
			}
		}
	}
	return nil
}

// computeEnumLayouts computes layouts for all enum declarations in the file.
// Generic enums (with TypeParams) are skipped — handled by computeMonoLayouts.
// Uses dependency resolution to ensure that if enum A has a variant field of enum B,
// B's layout is computed before A's (so A can use B's named LLVM struct type).
func (c *Compiler) computeEnumLayouts(file *ast.File) {
	// Collect all non-generic enum decls
	pending := make(map[string]*types.Enum)
	var names []string
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
			continue
		}
		if _, exists := c.enumLayouts[enum]; exists {
			continue // already computed (e.g., pre-pass in compileModules)
		}
		pending[ed.Name] = enum
		names = append(names, ed.Name)
	}

	// Compute layouts with dependency resolution
	computed := make(map[string]bool)
	var compute func(name string)
	compute = func(name string) {
		if computed[name] {
			return
		}
		enum := pending[name]
		if enum == nil {
			return
		}
		// Ensure layouts for enum types referenced in variant fields are computed first
		for _, v := range enum.Variants() {
			for _, f := range v.Fields() {
				if dep := extractEnum(f.Type()); dep != nil {
					depName := dep.Obj().Name()
					if _, ok := pending[depName]; ok {
						compute(depName)
					}
				}
				// Value-type variant fields need their embedded value-struct
				// layout computed before this enum's data layout. (T1016)
				c.ensureValueTypeLayout(f.Type())
			}
		}
		c.enumLayouts[enum] = computeEnumLayout(c.module, enum, c.ptrSize(), c.enumLayouts, c.layouts, c.monoLayouts)
		computed[name] = true
	}
	for _, name := range names {
		compute(name)
	}
}

// lookupTypeLayout finds the layout for a user type, handling Instance and monoCtx.
func (c *Compiler) lookupTypeLayout(typ types.Type) *TypeDeclLayout {
	if inst, ok := typ.(*types.Instance); ok {
		return c.monoLayouts[monoName(inst)]
	}
	if n := extractNamed(typ); n != nil {
		// Inside a mono method body, the origin Named maps to the mono layout
		if c.monoCtx != nil {
			if origin, ok := c.monoCtx.origin.(*types.Named); ok && n == origin {
				return c.monoLayouts[c.monoCtx.name]
			}
		}
		return c.layouts[n]
	}
	return nil
}

// lookupEnumLayout finds the layout for an enum, handling Instance and monoCtx.
// unwrapRefsType strips a chain of &/~ (SharedRef/MutRef) wrappers off a type,
// returning the underlying type. Used wherever a borrow must resolve to the same
// layout/name as the borrowed value (T0639, T1018).
func unwrapRefsType(typ types.Type) types.Type {
	for {
		if ref, ok := typ.(*types.MutRef); ok {
			typ = ref.Elem()
			continue
		}
		if ref, ok := typ.(*types.SharedRef); ok {
			typ = ref.Elem()
			continue
		}
		return typ
	}
}

func (c *Compiler) lookupEnumLayout(typ types.Type) *TypeDeclLayout {
	// T1018: unwrap borrows so a borrowed generic enum subject
	// (e.g. Maybe[string]& from Ref.borrow) resolves to its monomorphized
	// layout, not the bare generic enum layout (which has the wrong variant
	// data layout).
	typ = unwrapRefsType(typ)
	if inst, ok := typ.(*types.Instance); ok {
		return c.monoEnumLayouts[monoName(inst)]
	}
	if e := extractEnum(typ); e != nil {
		if c.monoCtx != nil {
			if origin, ok := c.monoCtx.origin.(*types.Enum); ok && e == origin {
				return c.monoEnumLayouts[c.monoCtx.name]
			}
		}
		return c.enumLayouts[e]
	}
	return nil
}

// lookupVtableGlobal finds the vtable global for a type, handling Instance and monoCtx.
func (c *Compiler) lookupVtableGlobal(typ types.Type) *ir.Global {
	if inst, ok := typ.(*types.Instance); ok {
		return c.monoVtableGlobals[monoName(inst)]
	}
	if n := extractNamed(typ); n != nil {
		if c.monoCtx != nil {
			if origin, ok := c.monoCtx.origin.(*types.Named); ok && n == origin {
				return c.monoVtableGlobals[c.monoCtx.name]
			}
		}
		return c.vtableGlobals[n]
	}
	return nil
}

// getNoValueTypeInfo returns a shared immutable typeinfo global with a null
// drop_fn, used as the RTTI header for heap-boxed primitive scalars coerced to a
// structural interface (T1276). Primitives never drop, so __promise_structural_drop
// reads drop_fn (field 1) == null and falls through to pal_free(box). Emitted once
// on the main module; module/instance IRs reference it as an extern declaration.
func (c *Compiler) getNoValueTypeInfo() *ir.Global {
	if c.noValueTypeInfo != nil {
		return c.noValueTypeInfo
	}
	// Layout mirrors a no-parent typeinfo: { vtable, drop_fn, clone_fn, typeID, numParents }.
	structType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I32, irtypes.I32)
	init := constant.NewStruct(structType,
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewInt(irtypes.I32, 0),
		constant.NewInt(irtypes.I32, 0))
	g := c.module.NewGlobalDef("promise_typeinfo_novalue", init)
	g.Immutable = true
	c.noValueTypeInfo = g
	return g
}

// getStringBoxDrop returns @__promise_string_box_drop, emitting it once. The wrapper
// takes the string box (i8* to { i8* typeinfo, i8* string_ptr }), loads field 1 (the
// owned string clone), drops it via promise_string_drop (honoring the rodata literal
// flag), then pal_free's the box. This is the drop_fn carried by the string box's
// typeinfo header so __promise_structural_drop dispatches it at every RTTI drop site
// (T1280). Emitted lazily so promise_string_drop and pal_free are already defined;
// referenced as an extern in split module/instance IRs.
func (c *Compiler) getStringBoxDrop() *ir.Func {
	if c.stringBoxDrop != nil {
		return c.stringBoxDrop
	}
	boxParam := ir.NewParam("box", irtypes.I8Ptr)
	fn := c.module.NewFunc("__promise_string_box_drop", irtypes.Void, boxParam)
	entry := fn.NewBlock(".entry")

	boxType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr)
	typed := entry.NewBitCast(boxParam, irtypes.NewPointer(boxType))
	strField := entry.NewGetElementPtr(boxType, typed,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	strPtr := entry.NewLoad(irtypes.I8Ptr, strField)
	entry.NewCall(c.funcs["promise_string_drop"], strPtr)
	entry.NewCall(c.palFree, boxParam)
	entry.NewRet(nil)

	c.stringBoxDrop = fn
	return fn
}

// getStringBoxClone returns @__promise_string_box_clone, emitting it once. The
// clone takes a string box (i8* to { i8* typeinfo, i8* string_ptr }), allocates a
// fresh box, copies the typeinfo header, deep-copies the owned string via
// dupString, and returns the new box. This is the clone_fn carried by the string
// box's typeinfo header (field 2) so __promise_structural_clone deep-copies a
// boxed string when a Vector[structural] holding it is cloned/sliced (T1284) —
// without it the shallow view copy aliases the box and the structural-aware
// element drop double-frees. Emitted lazily so dupString/palAlloc are defined.
func (c *Compiler) getStringBoxClone() *ir.Func {
	if c.stringBoxClone != nil {
		return c.stringBoxClone
	}
	boxParam := ir.NewParam("box", irtypes.I8Ptr)
	fn := c.module.NewFunc("__promise_string_box_clone", irtypes.I8Ptr, boxParam)

	// dupString creates its own basic blocks and advances c.block; emit the whole
	// body through c.block (save/restore the caller's cursor and fn context).
	savedBlock, savedFn, savedEntry := c.block, c.fn, c.entryBlock
	c.fn = fn
	c.block = fn.NewBlock(".entry")
	c.entryBlock = c.block

	boxType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr)
	size := constant.NewInt(irtypes.I64, int64(c.typeSize(boxType)))
	raw := c.block.NewCall(c.palAlloc, size)
	newTyped := c.block.NewBitCast(raw, irtypes.NewPointer(boxType))
	oldTyped := c.block.NewBitCast(boxParam, irtypes.NewPointer(boxType))

	// Copy typeinfo header (field 0)
	oldTiField := c.block.NewGetElementPtr(boxType, oldTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	ti := c.block.NewLoad(irtypes.I8Ptr, oldTiField)
	newTiField := c.block.NewGetElementPtr(boxType, newTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewStore(ti, newTiField)

	// Deep-copy the owned string (field 1)
	oldStrField := c.block.NewGetElementPtr(boxType, oldTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	oldStr := c.block.NewLoad(irtypes.I8Ptr, oldStrField)
	clonedStr := c.dupString(oldStr)
	newStrField := c.block.NewGetElementPtr(boxType, newTyped,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	c.block.NewStore(clonedStr, newStrField)

	c.block.NewRet(raw)
	c.block, c.fn, c.entryBlock = savedBlock, savedFn, savedEntry
	c.stringBoxClone = fn
	return fn
}

// getFlatBoxClone returns a size-specialized @__promise_flat_box_clone_<size>
// (i8*)→i8* that malloc's `size` bytes and memcpy's the source box into it. Used
// as the clone_fn for primitive and value-type structural boxes — both are "flat"
// (the boxed payload is Copy, with no droppable sub-fields), so a byte copy is a
// correct deep copy. T1284.
func (c *Compiler) getFlatBoxClone(size int64) *ir.Func {
	if c.flatBoxClones == nil {
		c.flatBoxClones = map[int64]*ir.Func{}
	}
	if fn, ok := c.flatBoxClones[size]; ok {
		return fn
	}
	boxParam := ir.NewParam("box", irtypes.I8Ptr)
	fn := c.module.NewFunc(fmt.Sprintf("__promise_flat_box_clone_%d", size), irtypes.I8Ptr, boxParam)
	entry := fn.NewBlock(".entry")
	sizeVal := constant.NewInt(irtypes.I64, size)
	raw := entry.NewCall(c.palAlloc, sizeVal)
	entry.NewCall(c.funcs["llvm.memcpy"], raw, boxParam, sizeVal, constant.False)
	entry.NewRet(raw)
	c.flatBoxClones[size] = fn
	return fn
}

// getFlatBoxTypeInfo returns a per-size immutable typeinfo header (typeID 0, no
// parents, null drop_fn so __promise_structural_drop pal_free's the box) whose
// clone_fn (field 2) is a flat malloc+memcpy clone. Carried by primitive/value
// structural boxes so structural clone/slice produces an independently-owned box
// instead of aliasing the source (which the structural-aware element drop would
// then double-free). Mirrors getNoValueTypeInfo but adds the clone_fn. T1284.
func (c *Compiler) getFlatBoxTypeInfo(size int64) *ir.Global {
	if c.flatBoxTypeInfos == nil {
		c.flatBoxTypeInfos = map[int64]*ir.Global{}
	}
	if g, ok := c.flatBoxTypeInfos[size]; ok {
		return g
	}
	cloneFn := c.getFlatBoxClone(size)
	structType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I32, irtypes.I32)
	init := constant.NewStruct(structType,
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewBitCast(cloneFn, irtypes.I8Ptr),
		constant.NewInt(irtypes.I32, 0),
		constant.NewInt(irtypes.I32, 0))
	g := c.module.NewGlobalDef(fmt.Sprintf("promise_typeinfo_flatbox_%d", size), init)
	g.Immutable = true
	c.flatBoxTypeInfos[size] = g
	return g
}

// getStringBoxTypeInfo returns a shared immutable typeinfo global whose drop_fn (field 1)
// points to @__promise_string_box_drop, used as the RTTI header for heap-boxed strings
// coerced to a structural interface (T1280). __promise_structural_drop reads the
// non-null drop_fn and dispatches the wrapper, which frees the cloned string then the
// box. Emitted once on the main module; module/instance IRs reference it as an extern.
func (c *Compiler) getStringBoxTypeInfo() *ir.Global {
	if c.stringBoxTypeInfo != nil {
		return c.stringBoxTypeInfo
	}
	dropFn := c.getStringBoxDrop()
	cloneFn := c.getStringBoxClone() // T1284: field 2 clone_fn for structural clone/slice
	// Layout mirrors a no-parent typeinfo: { vtable, drop_fn, clone_fn, typeID, numParents }.
	structType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I8Ptr, irtypes.I32, irtypes.I32)
	init := constant.NewStruct(structType,
		constant.NewNull(irtypes.I8Ptr),
		constant.NewBitCast(dropFn, irtypes.I8Ptr),
		constant.NewBitCast(cloneFn, irtypes.I8Ptr),
		constant.NewInt(irtypes.I32, 0),
		constant.NewInt(irtypes.I32, 0))
	g := c.module.NewGlobalDef("promise_typeinfo_stringbox", init)
	g.Immutable = true
	c.stringBoxTypeInfo = g
	return g
}

// lookupTypeInfoGlobal finds the typeinfo global for a type, handling Instance and monoCtx.
func (c *Compiler) lookupTypeInfoGlobal(typ types.Type) *ir.Global {
	if inst, ok := typ.(*types.Instance); ok {
		return c.monoTypeInfoGlobals[monoName(inst)]
	}
	if n := extractNamed(typ); n != nil {
		if c.monoCtx != nil {
			if origin, ok := c.monoCtx.origin.(*types.Named); ok && n == origin {
				return c.monoTypeInfoGlobals[c.monoCtx.name]
			}
		}
		return c.typeInfoGlobals[n]
	}
	return nil
}

// lookupValueTypeRTTI finds the value type RTTI global for a type, handling Instance and monoCtx.
func (c *Compiler) lookupValueTypeRTTI(typ types.Type) *ir.Global {
	if inst, ok := typ.(*types.Instance); ok {
		return c.monoValueTypeRTTI[monoName(inst)]
	}
	if n := extractNamed(typ); n != nil {
		if c.monoCtx != nil {
			if origin, ok := c.monoCtx.origin.(*types.Named); ok && n == origin {
				return c.monoValueTypeRTTI[c.monoCtx.name]
			}
		}
		return c.valueTypeRTTI[n]
	}
	return nil
}

// resolveTypeName returns the mangled type name for method dispatch.
func (c *Compiler) resolveTypeName(typ types.Type) string {
	// T0639: unwrap a chain of ~/& refs so a ref-wrapped generic-instance
	// receiver (e.g. NBox[int]~) mangles to the same name as the bare
	// instance (NBox[int]), not the bare generic owner (NBox). For
	// non-generic types the unwrapped Named yields the same name; for bare
	// instances there is no ref to strip.
	typ = unwrapRefsType(typ)
	if inst, ok := typ.(*types.Instance); ok {
		return monoName(inst)
	}
	if c.monoCtx != nil {
		if n, ok := typ.(*types.Named); ok {
			if origin, ok := c.monoCtx.origin.(*types.Named); ok && n == origin {
				return c.monoCtx.name
			}
		}
	}
	if n := extractNamed(typ); n != nil {
		return n.Obj().Name()
	}
	return ""
}

// resolveMethodOwner returns the type name of the type that actually defines the given method.
// If the method is overridden in the child, returns the child's name. If inherited,
// walks up the parent chain to find the defining type.
func (c *Compiler) resolveMethodOwner(named *types.Named, methodName string) string {
	// Check own methods first
	for _, m := range named.Methods() {
		if m.Name() == methodName {
			return named.Obj().Name()
		}
	}
	// Walk parents. Use LookupAnyMethod so getters and setters inherited from
	// parents are routed to the parent's mangled name (T0637) — LookupMethod
	// skips getters/setters by design.
	for _, pr := range named.Parents() {
		if pr.Named.LookupAnyMethod(methodName) != nil {
			return c.resolveMethodOwner(pr.Named, methodName)
		}
	}
	return named.Obj().Name() // fallback
}

// hasOwnMethod returns true if the Named type declares the method itself (not inherited).
func hasOwnMethod(named *types.Named, name string) bool {
	for _, m := range named.Methods() {
		if m.Name() == name {
			return true
		}
	}
	return false
}

// findStructuralOwner returns the structural parent type that owns a method
// inherited by `named`, or nil if the method comes from a non-structural parent.
func (c *Compiler) findStructuralOwner(named *types.Named, methodName string) *types.Named {
	for _, pr := range named.Parents() {
		if m := pr.Named.LookupMethod(methodName); m != nil {
			if pr.Named.IsStructural() {
				return pr.Named
			}
			// T1551: a non-structural parent that DECLARES the method itself owns
			// the implementation — dispatch targets <parent>.<method>, not a
			// per-concrete synthesis of a structural grandparent's default. An
			// abstract declaration is not an implementation, so keep recursing
			// through abstract classes.
			if hasOwnMethod(pr.Named, methodName) && !m.IsAbstract() {
				return nil
			}
			// Recurse into parent's parents
			if found := c.findStructuralOwner(pr.Named, methodName); found != nil {
				return found
			}
		}
	}
	return nil
}
