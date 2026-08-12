package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// declareSynthesizedDrops declares drop function stubs for non-generic types
// that need a compiler-synthesized drop (B0158). These are types without explicit
// drop() but with fields whose types have HasDrop().
func (c *Compiler) declareSynthesizedDrops(file *ast.File) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		named := c.lookupNamedType(td.Name)
		if named == nil || !named.NeedsSynthDrop() {
			continue
		}
		if len(named.TypeParams()) > 0 {
			continue // generic — handled by declareSynthesizedMonoDrops
		}
		mangledName := mangleMethodName(td.Name, "drop", false)
		if _, exists := c.funcs[mangledName]; exists {
			continue
		}
		fn := c.module.NewFunc(mangledName, irtypes.Void,
			ir.NewParam("this", irtypes.I8Ptr))
		c.funcs[mangledName] = fn
	}
}

// defineSynthesizedDrops generates bodies for synthesized drop functions (non-generic).
// Each body: drop all droppable fields in reverse order, then free the instance.
func (c *Compiler) defineSynthesizedDrops(file *ast.File) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		named := c.lookupNamedType(td.Name)
		if named == nil || !named.NeedsSynthDrop() {
			continue
		}
		if len(named.TypeParams()) > 0 {
			continue // generic — handled by defineSynthesizedMonoDrops
		}
		mangledName := mangleMethodName(td.Name, "drop", false)
		fn, ok := c.funcs[mangledName]
		if !ok || len(fn.Blocks) > 0 {
			continue
		}
		c.defineSynthesizedDropBody(fn, named)
	}
}

// declareSynthesizedModuleDrops declares drop stubs for non-generic module types (B0158).
func (c *Compiler) declareSynthesizedModuleDrops(file *ast.File, moduleName string) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		named := c.lookupNamedType(td.Name)
		if named == nil || !named.NeedsSynthDrop() {
			continue
		}
		if len(named.TypeParams()) > 0 {
			continue // handled by declareSynthesizedMonoDrops
		}
		mangledName := mangleModuleMethodName(moduleName, td.Name, "drop", false)
		if _, exists := c.funcs[mangledName]; exists {
			continue
		}
		fn := c.module.NewFunc(mangledName, irtypes.Void,
			ir.NewParam("this", irtypes.I8Ptr))
		c.funcs[mangledName] = fn
		c.moduleOwnedFuncs[mangledName] = moduleName
		// Also register the non-prefixed method name for dispatch from user code
		plainName := mangleMethodName(td.Name, "drop", false)
		if _, exists := c.funcs[plainName]; !exists {
			c.funcs[plainName] = fn
		}
	}
}

// defineSynthesizedModuleDrops generates bodies for non-generic module synthesized drops.
func (c *Compiler) defineSynthesizedModuleDrops(file *ast.File, moduleName string) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		named := c.lookupNamedType(td.Name)
		if named == nil || !named.NeedsSynthDrop() {
			continue
		}
		if len(named.TypeParams()) > 0 {
			continue
		}
		mangledName := mangleModuleMethodName(moduleName, td.Name, "drop", false)
		fn, ok := c.funcs[mangledName]
		if !ok || len(fn.Blocks) > 0 {
			continue
		}
		c.defineSynthesizedDropBody(fn, named)
	}
}

// defineSynthesizedDropBody generates the body for a synthesized drop function.
// It drops all droppable fields in reverse order and frees the instance.
// B0160: was a no-op pending T0064 (container ownership tracking). Now enabled
// since drop flags + clearDropFlag at move sites prevent double-free of aliased values.
func (c *Compiler) defineSynthesizedDropBody(fn *ir.Func, named *types.Named) {
	entry := fn.NewBlock(".entry")
	savedBlock := c.block
	savedFn := c.fn
	savedEntry := c.entryBlock // B0189: save for createEntryAlloca in element drop loops
	savedPanicExit := c.panicExitBlock
	savedCoroReturn := c.coroutineReturnBlock
	c.block = entry
	c.fn = fn            // T0101: ensure c.newBlock() creates blocks in the drop function
	c.entryBlock = entry // B0189: element drop loops use createEntryAlloca
	c.panicExitBlock = nil
	c.coroutineReturnBlock = nil

	// Set up method context for emitFieldDrops (needs locals["this"])
	savedLocals := c.locals
	c.locals = make(map[string]*ir.InstAlloca)
	thisAlloca := entry.NewAlloca(irtypes.I8Ptr)
	entry.NewStore(fn.Params[0], thisAlloca)
	c.locals["this"] = thisAlloca

	// Drop all droppable fields in reverse declaration order
	c.emitFieldDrops(named)

	// Free the instance struct itself
	c.block.NewCall(c.palFree, fn.Params[0])

	c.block.NewRet(nil)
	c.locals = savedLocals
	c.block = savedBlock
	c.fn = savedFn
	c.entryBlock = savedEntry
	c.panicExitBlock = savedPanicExit
	c.coroutineReturnBlock = savedCoroReturn
}

// resolveDropOwner returns the type name to use when resolving the drop function
// for a non-Instance Named type. Prefers the type's own drop name (covers own
// explicit drop, B0158 synth, and T0507 inherited-drop synth) when a function
// exists under that name; otherwise falls back to resolveMethodOwner which walks
// the parent chain. T0507.
func (c *Compiler) resolveDropOwner(named *types.Named) string {
	if named == nil {
		return ""
	}
	childName := named.Obj().Name()
	if _, ok := c.funcs[mangleMethodName(childName, "drop", false)]; ok {
		return childName
	}
	return c.resolveMethodOwner(named, "drop")
}

// needsInheritedDropSynth returns true if a non-generic Named type inherits its
// drop from a parent and needs a per-type synth that drops own fields + tail-calls
// the parent's drop. T0507 (non-generic complement of T0468).
func needsInheritedDropSynth(named *types.Named) bool {
	if named == nil {
		return false
	}
	if len(named.TypeParams()) > 0 {
		return false // generic → T0468
	}
	if named.IsStructural() || named.IsCopy() || named.IsValueType() {
		return false
	}
	if !named.HasDrop() {
		return false
	}
	if hasOwnMethod(named, "drop") {
		return false // own drop → emitFieldDrops in that body handles fields
	}
	if named.NeedsSynthDrop() {
		return false // B0158 path handles
	}
	dropMethod := named.LookupMethod("drop")
	if dropMethod == nil || dropMethod.IsNative() {
		return false
	}
	return true
}

// declareInheritedDrops declares drop stubs for non-generic types whose drop is
// inherited from a parent (T0507). Without this synthesis, drop call sites resolve
// to the parent's drop name and silently skip the child's own droppable fields.
// The body (defineInheritedDrops) drops the child's own fields and tail-calls the
// immediate drop parent.
func (c *Compiler) declareInheritedDrops(file *ast.File) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		named := c.lookupNamedType(td.Name)
		if !needsInheritedDropSynth(named) {
			continue
		}
		mangledName := mangleMethodName(td.Name, "drop", false)
		if _, exists := c.funcs[mangledName]; exists {
			continue
		}
		fn := c.module.NewFunc(mangledName, irtypes.Void,
			ir.NewParam("this", irtypes.I8Ptr))
		c.funcs[mangledName] = fn
	}
}

// defineInheritedDrops generates bodies for inherited-drop synth stubs (T0507).
func (c *Compiler) defineInheritedDrops(file *ast.File) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		named := c.lookupNamedType(td.Name)
		if !needsInheritedDropSynth(named) {
			continue
		}
		mangledName := mangleMethodName(td.Name, "drop", false)
		fn, ok := c.funcs[mangledName]
		if !ok || len(fn.Blocks) > 0 {
			continue
		}
		c.defineInheritedDropBody(fn, named)
	}
}

// declareInheritedModuleDrops declares inherited-drop stubs for module types (T0507).
// Registers both module-prefixed and plain aliases so resolveDropOwner finds it.
func (c *Compiler) declareInheritedModuleDrops(file *ast.File, moduleName string) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		named := c.lookupNamedType(td.Name)
		if !needsInheritedDropSynth(named) {
			continue
		}
		mangledName := mangleModuleMethodName(moduleName, td.Name, "drop", false)
		if _, exists := c.funcs[mangledName]; exists {
			continue
		}
		fn := c.module.NewFunc(mangledName, irtypes.Void,
			ir.NewParam("this", irtypes.I8Ptr))
		c.funcs[mangledName] = fn
		c.moduleOwnedFuncs[mangledName] = moduleName
		// Also register the non-prefixed name for dispatch from user code
		plainName := mangleMethodName(td.Name, "drop", false)
		if _, exists := c.funcs[plainName]; !exists {
			c.funcs[plainName] = fn
		}
	}
}

// defineInheritedModuleDrops generates bodies for module inherited-drop synth (T0507).
func (c *Compiler) defineInheritedModuleDrops(file *ast.File, moduleName string) {
	for _, decl := range file.Decls {
		td, ok := decl.(*ast.TypeDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		named := c.lookupNamedType(td.Name)
		if !needsInheritedDropSynth(named) {
			continue
		}
		mangledName := mangleModuleMethodName(moduleName, td.Name, "drop", false)
		fn, ok := c.funcs[mangledName]
		if !ok || len(fn.Blocks) > 0 {
			continue
		}
		c.defineInheritedDropBody(fn, named)
	}
}

// defineInheritedDropBody generates the body for an inherited-drop synth (T0507).
// The body drops the child's own fields (reverse declaration order) and then
// tail-calls the immediate drop parent's drop, which runs the parent body and
// drops the parent's fields. NO pal_free — caller handles, since NeedsSynthDrop
// stays false (matches the explicit-drop convention).
func (c *Compiler) defineInheritedDropBody(fn *ir.Func, named *types.Named) {
	entry := fn.NewBlock(".entry")
	savedBlock := c.block
	savedFn := c.fn
	savedEntry := c.entryBlock
	savedPanicExit := c.panicExitBlock
	savedCoroReturn := c.coroutineReturnBlock
	savedLocals := c.locals

	c.block = entry
	c.fn = fn
	c.entryBlock = entry
	c.panicExitBlock = nil
	c.coroutineReturnBlock = nil
	c.locals = make(map[string]*ir.InstAlloca)

	thisAlloca := entry.NewAlloca(irtypes.I8Ptr)
	entry.NewStore(fn.Params[0], thisAlloca)
	c.locals["this"] = thisAlloca

	// Drop the child's own fields only (parent fields are dropped by parent's drop).
	c.emitFieldDropsFor(named, named.Fields())

	// Tail-call the immediate drop parent's drop. For multi-level chains (C is B
	// is A with drop on A), the synthesis cascades naturally because B's synth
	// drops B's fields then calls A's drop. When the parent is generic (e.g.
	// `_NonGenChild is _GenericLogger[int]`), use the mono name `_GenericLogger[int]`
	// rather than the origin name `_GenericLogger`.
	parentNamed := findImmediateDropParent(named)
	if parentNamed != nil {
		parentTypeName := c.resolveMonoParentName(named, named, parentNamed.Obj().Name())
		parentMangled := mangleMethodName(parentTypeName, "drop", false)
		if parentFn, ok := c.funcs[parentMangled]; ok {
			c.block.NewCall(parentFn, fn.Params[0])
		}
	}

	c.block.NewRet(nil)

	c.block = savedBlock
	c.fn = savedFn
	c.entryBlock = savedEntry
	c.panicExitBlock = savedPanicExit
	c.coroutineReturnBlock = savedCoroReturn
	c.locals = savedLocals
}

// declareSynthesizedEnumDrops declares drop function stubs for non-generic enums
// that need a compiler-synthesized drop (T0102).
func (c *Compiler) declareSynthesizedEnumDrops(file *ast.File) {
	for _, decl := range file.Decls {
		ed, ok := decl.(*ast.EnumDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		enum := c.lookupEnumType(ed.Name)
		if enum == nil || !enum.NeedsSynthDrop() {
			continue
		}
		if len(enum.TypeParams()) > 0 {
			continue // generic — handled by declareSynthesizedMonoEnumDrops
		}
		mangledName := mangleMethodName(ed.Name, "drop", false)
		if _, exists := c.funcs[mangledName]; exists {
			continue
		}
		fn := c.module.NewFunc(mangledName, irtypes.Void,
			ir.NewParam("this", irtypes.I8Ptr))
		c.funcs[mangledName] = fn
	}
}

// defineSynthesizedEnumDrops generates bodies for synthesized enum drop functions (T0102).
func (c *Compiler) defineSynthesizedEnumDrops(file *ast.File) {
	for _, decl := range file.Decls {
		ed, ok := decl.(*ast.EnumDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		enum := c.lookupEnumType(ed.Name)
		if enum == nil || !enum.NeedsSynthDrop() {
			continue
		}
		if len(enum.TypeParams()) > 0 {
			continue // generic — handled by defineSynthesizedMonoEnumDrops
		}
		mangledName := mangleMethodName(ed.Name, "drop", false)
		fn, ok := c.funcs[mangledName]
		if !ok || len(fn.Blocks) > 0 {
			continue
		}
		layout := c.enumLayouts[enum]
		if layout == nil {
			continue
		}
		c.defineSynthesizedEnumDropBody(fn, enum, layout)
	}
}

// declareSynthesizedModuleEnumDrops declares enum drop stubs for non-generic module enums (T0102).
func (c *Compiler) declareSynthesizedModuleEnumDrops(file *ast.File, moduleName string) {
	for _, decl := range file.Decls {
		ed, ok := decl.(*ast.EnumDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		enum := c.lookupEnumType(ed.Name)
		if enum == nil || !enum.NeedsSynthDrop() {
			continue
		}
		if len(enum.TypeParams()) > 0 {
			continue
		}
		mangledName := mangleModuleMethodName(moduleName, ed.Name, "drop", false)
		if _, exists := c.funcs[mangledName]; exists {
			continue
		}
		fn := c.module.NewFunc(mangledName, irtypes.Void,
			ir.NewParam("this", irtypes.I8Ptr))
		c.funcs[mangledName] = fn
		c.moduleOwnedFuncs[mangledName] = moduleName
		// Also register the non-prefixed name for dispatch from user code
		plainName := mangleMethodName(ed.Name, "drop", false)
		if _, exists := c.funcs[plainName]; !exists {
			c.funcs[plainName] = fn
		}
	}
}

// defineSynthesizedModuleEnumDrops generates bodies for non-generic module enum drops (T0102).
func (c *Compiler) defineSynthesizedModuleEnumDrops(file *ast.File, moduleName string) {
	for _, decl := range file.Decls {
		ed, ok := decl.(*ast.EnumDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		enum := c.lookupEnumType(ed.Name)
		if enum == nil || !enum.NeedsSynthDrop() {
			continue
		}
		if len(enum.TypeParams()) > 0 {
			continue
		}
		mangledName := mangleModuleMethodName(moduleName, ed.Name, "drop", false)
		fn, ok := c.funcs[mangledName]
		if !ok || len(fn.Blocks) > 0 {
			continue
		}
		layout := c.enumLayouts[enum]
		if layout == nil {
			continue
		}
		c.defineSynthesizedEnumDropBody(fn, enum, layout)
	}
}

// defineSynthesizedEnumDropBody generates the body for a synthesized enum drop (T0102).
// The drop function: loads the tag, switches on it, and for each variant with
// droppable fields, extracts and drops them. No pal_free — enum data is inline.
func (c *Compiler) defineSynthesizedEnumDropBody(fn *ir.Func, enum *types.Enum, layout *TypeDeclLayout) {
	// Only data enums (with variant data) need drop bodies.
	internalType, ok := layout.EnumInternalType.(*irtypes.StructType)
	if !ok {
		// Fieldless enum (i32) — nothing to drop
		entry := fn.NewBlock(".entry")
		entry.NewRet(nil)
		return
	}

	entry := fn.NewBlock(".entry")
	savedBlock := c.block
	savedFn := c.fn
	savedEntry := c.entryBlock // B0189: save for createEntryAlloca in element drop loops
	savedPanicExit := c.panicExitBlock
	savedCoroReturn := c.coroutineReturnBlock
	c.block = entry
	c.fn = fn            // B0189: ensure c.newBlock() creates blocks in the drop function
	c.entryBlock = entry // B0189: element drop loops use createEntryAlloca
	c.panicExitBlock = nil
	c.coroutineReturnBlock = nil

	// this = i8* pointer to the alloca storing the enum internal type
	typedPtr := entry.NewBitCast(fn.Params[0], irtypes.NewPointer(internalType))

	// Load tag (index 0 of internal type struct)
	tagPtr := entry.NewGetElementPtr(internalType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	tag := entry.NewLoad(irtypes.I32, tagPtr)

	// Data area pointer (index 1)
	dataPtr := entry.NewGetElementPtr(internalType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))

	// Build switch: for each variant, check if it has droppable fields
	doneBlock := fn.NewBlock("enum.drop.done")
	doneBlock.NewRet(nil)

	// Collect variants that need cleanup
	type variantDrop struct {
		tag      int
		name     string
		variant  *types.Variant
		dataType *irtypes.StructType
	}
	var droppableVariants []variantDrop
	for _, v := range enum.Variants() {
		if v.NumFields() == 0 {
			continue
		}
		dt := layout.VariantDataTypes[v.Name()]
		if dt == nil {
			continue
		}
		// Check if any field in this variant needs drop
		hasDroppable := false
		for _, f := range v.Fields() {
			if c.variantFieldNeedsDrop(f.Type()) {
				hasDroppable = true
				break
			}
		}
		if hasDroppable {
			droppableVariants = append(droppableVariants, variantDrop{
				tag:      layout.VariantTag[v.Name()],
				name:     v.Name(),
				variant:  v,
				dataType: dt,
			})
		}
	}

	if len(droppableVariants) == 0 {
		// No variants need drop — just return
		entry.NewBr(doneBlock)
		return
	}

	// Create switch cases
	var cases []*ir.Case
	for _, vd := range droppableVariants {
		varBlock := fn.NewBlock(fmt.Sprintf("enum.drop.%s", vd.name))
		cases = append(cases, &ir.Case{X: constant.NewInt(irtypes.I32, int64(vd.tag)), Target: varBlock})

		c.block = varBlock
		typedDataPtr := varBlock.NewBitCast(dataPtr, irtypes.NewPointer(vd.dataType))

		// Drop each droppable field in the variant
		for i, f := range vd.variant.Fields() {
			if !c.variantFieldNeedsDrop(f.Type()) {
				continue
			}
			fieldPtr := c.block.NewGetElementPtr(vd.dataType, typedDataPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
			fieldVal := c.block.NewLoad(vd.dataType.Fields[i], fieldPtr)

			c.emitVariantFieldDrop(fieldVal, f.Type())
		}
		c.block.NewBr(doneBlock)
	}
	entry.NewSwitch(tag, doneBlock, cases...)

	c.block = savedBlock
	c.fn = savedFn
	c.entryBlock = savedEntry
	c.panicExitBlock = savedPanicExit
	c.coroutineReturnBlock = savedCoroReturn
}

// enumNeedsSynthClone reports whether a droppable enum requires a compiler-
// synthesized recursive clone function (T1129). True iff the enum (a) has drop
// work, (b) has no existing user/`clone clone() that already owns the deep copy,
// and (c) is NOT shallow-dup-safe — i.e. its deep copy genuinely needs runtime-
// recursive clone logic that the inline shallow dupEnumElementInPlace path cannot
// replicate. This is exactly the recursive / Map/Set/clone-bearing-container case
// (e.g. `Tree.Branch(Tree[] kids)` or `TreeM.Node(Map[int, TreeM])`) that the
// `seen` cycle guard (B0289) and enumMatchDupSafe (T1110) deliberately exclude.
// Shallow-dup-safe enums keep their existing inline path untouched. `resolved`
// carries the concrete type args for generic instances. Single source of truth —
// used by the non-generic, module, and mono declare/define/forward-declare paths.
func (c *Compiler) enumNeedsSynthClone(enum *types.Enum, resolved types.Type) bool {
	if enum == nil {
		return false
	}
	// (b) An existing clone (user-defined or `clone) already owns the deep copy.
	if enum.IsClone() || enum.LookupMethod("clone") != nil {
		return false
	}
	// (a) Must have drop work — otherwise a bit copy is already correct. Generic
	// enums report HasDrop=NeedsSynthDrop=false (TypeParam fields), so the concrete
	// instance is checked via monoEnumInstNeedsSynthDrop.
	hasDrop := enum.HasDrop() || enum.NeedsSynthDrop()
	if inst, ok := resolved.(*types.Instance); ok {
		hasDrop = hasDrop || monoEnumInstNeedsSynthDrop(inst)
	}
	if !hasDrop {
		return false
	}
	// (c) Skip if shallow-dup-safe — that path (T1110, cloneResolvedValue's
	// dupEnumElementInPlace branch) already produces an independent copy.
	if c.enumMatchDupSafe(resolved, nil) {
		return false
	}
	return true
}

// declareSynthesizedEnumClones declares recursive clone function stubs for
// non-generic enums that need a compiler-synthesized clone (T1129).
func (c *Compiler) declareSynthesizedEnumClones(file *ast.File) {
	for _, decl := range file.Decls {
		ed, ok := decl.(*ast.EnumDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		enum := c.lookupEnumType(ed.Name)
		if enum == nil || len(enum.TypeParams()) > 0 {
			continue // generic — handled by declareSynthesizedMonoEnumClones
		}
		if !c.enumNeedsSynthClone(enum, enum) {
			continue
		}
		mangledName := mangleMethodName(ed.Name, "clone", false)
		if _, exists := c.funcs[mangledName]; exists {
			continue
		}
		fn := c.module.NewFunc(mangledName, c.resolveType(enum),
			ir.NewParam("this", irtypes.I8Ptr))
		c.funcs[mangledName] = fn
	}
}

// defineSynthesizedEnumClones generates bodies for synthesized enum clone functions (T1129).
func (c *Compiler) defineSynthesizedEnumClones(file *ast.File) {
	for _, decl := range file.Decls {
		ed, ok := decl.(*ast.EnumDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		enum := c.lookupEnumType(ed.Name)
		if enum == nil || len(enum.TypeParams()) > 0 {
			continue
		}
		if !c.enumNeedsSynthClone(enum, enum) {
			continue
		}
		mangledName := mangleMethodName(ed.Name, "clone", false)
		fn, ok := c.funcs[mangledName]
		if !ok || len(fn.Blocks) > 0 {
			continue
		}
		layout := c.enumLayouts[enum]
		if layout == nil {
			continue
		}
		c.defineSynthesizedEnumCloneBody(fn, enum, layout, enum)
	}
}

// declareSynthesizedModuleEnumClones declares synth clone stubs for non-generic module enums (T1129).
func (c *Compiler) declareSynthesizedModuleEnumClones(file *ast.File, moduleName string) {
	for _, decl := range file.Decls {
		ed, ok := decl.(*ast.EnumDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		enum := c.lookupEnumType(ed.Name)
		if enum == nil || len(enum.TypeParams()) > 0 {
			continue
		}
		if !c.enumNeedsSynthClone(enum, enum) {
			continue
		}
		mangledName := mangleModuleMethodName(moduleName, ed.Name, "clone", false)
		if _, exists := c.funcs[mangledName]; exists {
			continue
		}
		fn := c.module.NewFunc(mangledName, c.resolveType(enum),
			ir.NewParam("this", irtypes.I8Ptr))
		c.funcs[mangledName] = fn
		c.moduleOwnedFuncs[mangledName] = moduleName
		// Also register the non-prefixed name for dispatch from user code.
		plainName := mangleMethodName(ed.Name, "clone", false)
		if _, exists := c.funcs[plainName]; !exists {
			c.funcs[plainName] = fn
		}
	}
}

// defineSynthesizedModuleEnumClones generates bodies for non-generic module enum clones (T1129).
func (c *Compiler) defineSynthesizedModuleEnumClones(file *ast.File, moduleName string) {
	for _, decl := range file.Decls {
		ed, ok := decl.(*ast.EnumDecl)
		if !ok {
			continue
		}
		if c.info.FilteredDecls[decl] {
			continue
		}
		enum := c.lookupEnumType(ed.Name)
		if enum == nil || len(enum.TypeParams()) > 0 {
			continue
		}
		if !c.enumNeedsSynthClone(enum, enum) {
			continue
		}
		mangledName := mangleModuleMethodName(moduleName, ed.Name, "clone", false)
		fn, ok := c.funcs[mangledName]
		if !ok || len(fn.Blocks) > 0 {
			continue
		}
		layout := c.enumLayouts[enum]
		if layout == nil {
			continue
		}
		c.defineSynthesizedEnumCloneBody(fn, enum, layout, enum)
	}
}

// defineSynthesizedEnumCloneBody generates the body for a synthesized recursive
// enum clone (T1129). The clone takes the enum value by i8* pointer (the enum
// method receiver convention), makes an independent stack copy, deep-dups every
// droppable variant field in place via dupEnumElementInPlace, and returns the
// now-independent value. Recursion into nested container elements (Vector[Tree],
// Map[K, TreeM]) routes through *calls* to this same registered clone (and
// Map.clone), so depth-≥2 trees copy correctly without infinite codegen.
func (c *Compiler) defineSynthesizedEnumCloneBody(fn *ir.Func, enum *types.Enum, layout *TypeDeclLayout, enumType types.Type) {
	entry := fn.NewBlock(".entry")
	savedBlock := c.block
	savedFn := c.fn
	savedEntry := c.entryBlock
	savedPanicExit := c.panicExitBlock
	savedCoroReturn := c.coroutineReturnBlock
	c.block = entry
	c.fn = fn
	c.entryBlock = entry
	c.panicExitBlock = nil
	c.coroutineReturnBlock = nil
	defer func() {
		c.block = savedBlock
		c.fn = savedFn
		c.entryBlock = savedEntry
		c.panicExitBlock = savedPanicExit
		c.coroutineReturnBlock = savedCoroReturn
	}()

	internalType, ok := layout.EnumInternalType.(*irtypes.StructType)
	if !ok {
		// Fieldless enum (i32) — nothing to dup, return a bit copy.
		srcPtr := entry.NewBitCast(fn.Params[0], irtypes.NewPointer(layout.EnumInternalType))
		entry.NewRet(entry.NewLoad(layout.EnumInternalType, srcPtr))
		return
	}

	// Independent stack copy of the source enum value.
	srcPtr := entry.NewBitCast(fn.Params[0], irtypes.NewPointer(internalType))
	tmp := c.createEntryAlloca(internalType)
	entry.NewStore(entry.NewLoad(internalType, srcPtr), tmp)

	// Deep-dup the droppable variant fields in place. Nested recursive elements
	// clone via calls to the registered clone fn (emitVectorElementCloneLoop /
	// emitVariantFieldDup find it in c.funcs), giving runtime recursion.
	c.dupEnumElementInPlace(c.block.NewBitCast(tmp, irtypes.I8Ptr), enumType)

	c.block.NewRet(c.block.NewLoad(internalType, tmp))
}

// emitEnumVariantFieldDropsInline emits variant-field drops inline after an
// enum's explicit drop(~this) body (T0604). This mirrors emitFieldDrops for
// named types: switch on the tag and drop each variant's droppable fields.
// Called from defineMethodFunc when currentDropEnum is set.
func (c *Compiler) emitEnumVariantFieldDropsInline(enum *types.Enum) {
	layout := c.lookupEnumLayout(enum)
	if layout == nil {
		return
	}

	internalType, ok := layout.EnumInternalType.(*irtypes.StructType)
	if !ok {
		// Fieldless enum (i32) — nothing to drop
		return
	}

	// this = alloca of the receiver parameter (i8* for enum drop)
	thisAlloca := c.locals["this"]
	if thisAlloca == nil {
		return
	}

	// Collect variants that need cleanup before emitting any IR
	type variantDrop struct {
		tag      int
		name     string
		variant  *types.Variant
		dataType *irtypes.StructType
	}
	var droppableVariants []variantDrop
	for _, v := range enum.Variants() {
		if v.NumFields() == 0 {
			continue
		}
		dt := layout.VariantDataTypes[v.Name()]
		if dt == nil {
			continue
		}
		hasDroppable := false
		for _, f := range v.Fields() {
			if c.variantFieldNeedsDrop(f.Type()) {
				hasDroppable = true
				break
			}
		}
		if hasDroppable {
			droppableVariants = append(droppableVariants, variantDrop{
				tag:      layout.VariantTag[v.Name()],
				name:     v.Name(),
				variant:  v,
				dataType: dt,
			})
		}
	}

	if len(droppableVariants) == 0 {
		return
	}

	// Load this, cast to internal type, extract tag and data pointer.
	// All loads go into the current block (end of user's drop body).
	switchBlock := c.block
	thisVal := switchBlock.NewLoad(thisAlloca.ElemType, thisAlloca)
	typedPtr := switchBlock.NewBitCast(thisVal, irtypes.NewPointer(internalType))

	tagPtr := switchBlock.NewGetElementPtr(internalType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	tag := switchBlock.NewLoad(irtypes.I32, tagPtr)

	dataPtr := switchBlock.NewGetElementPtr(internalType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))

	// Create case blocks and continue block
	contBlock := c.newBlock("enum.drop.field.done")
	var cases []*ir.Case
	for _, vd := range droppableVariants {
		varBlock := c.newBlock(fmt.Sprintf("enum.drop.field.%s", vd.name))
		cases = append(cases, &ir.Case{X: constant.NewInt(irtypes.I32, int64(vd.tag)), Target: varBlock})

		c.block = varBlock
		typedDataPtr := varBlock.NewBitCast(dataPtr, irtypes.NewPointer(vd.dataType))

		for i, f := range vd.variant.Fields() {
			if !c.variantFieldNeedsDrop(f.Type()) {
				continue
			}
			fieldPtr := c.block.NewGetElementPtr(vd.dataType, typedDataPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
			fieldVal := c.block.NewLoad(vd.dataType.Fields[i], fieldPtr)
			c.emitVariantFieldDrop(fieldVal, f.Type())
		}
		c.block.NewBr(contBlock)
	}

	// Terminate the switch block and set continuation as current
	switchBlock.NewSwitch(tag, contBlock, cases...)
	c.block = contBlock
}

// variantFieldNeedsDrop returns true if an enum variant field type needs cleanup.
func (c *Compiler) variantFieldNeedsDrop(typ types.Type) bool {
	// Apply type substitution for mono contexts
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	// T0371: Tuples with droppable fields need cleanup so the synth enum drop
	// walks them via emitVariantFieldDrop's tuple branch.
	if tup, ok := typ.(*types.Tuple); ok {
		for _, e := range tup.Elems() {
			if c.variantFieldNeedsDrop(e) {
				return true
			}
		}
		return false
	}
	// T0485: Optional<T> with droppable inner needs cleanup so the synth enum
	// drop walks it via emitVariantFieldDrop's optional branch.
	if opt, ok := typ.(*types.Optional); ok {
		return c.variantFieldNeedsDrop(opt.Elem())
	}
	// T0485: Fixed-size Array[N]T with droppable elements needs cleanup.
	if arr, ok := typ.(*types.Array); ok {
		return c.variantFieldNeedsDrop(arr.Elem())
	}
	// T0741: Closure (function value) fields own a heap env struct (+ captured
	// values) that must be deep-dropped. emitVariantFieldDrop's Signature case
	// frees the env. Paired with claimEnvTemp at every aggregate construction
	// site so the env is owned exactly once.
	if _, ok := typ.(*types.Signature); ok {
		return true
	}
	named := extractNamed(typ)
	if named != nil {
		if named == types.TypString || named == types.TypVector || named == types.TypChannel {
			return true
		}
		// T0765: Structural-interface field (non-value-type) in a variant holds a
		// {vtable, instance} value struct; the heap instance must be dropped via
		// __promise_structural_drop (RTTI dispatch). Mirrors the struct-field case
		// (T0460) — Writer itself carries no HasDrop/NeedsSynthDrop, so gate on
		// IsStructural directly.
		if named.IsStructural() && !named.IsValueType() {
			return true
		}
		if named.HasDrop() || named.NeedsSynthDrop() {
			return true
		}
		// B0218: Heap-allocated user types without explicit/synthesized drop still need
		// pal_free. Value types have inline data (no heap pointer), primitives and
		// structural interfaces don't need cleanup.
		if !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural() {
			return true
		}
		// B0202: Check for mono instance with codegen-time synthesized drop.
		if inst, ok := typ.(*types.Instance); ok {
			if n, ok2 := inst.Origin().(*types.Named); ok2 && n.NeedsSynthDrop() {
				return true
			}
			mangledName := mangleMethodName(monoName(inst), "drop", false)
			if _, ok := c.funcs[mangledName]; ok {
				return true
			}
		}
		return false
	}
	if enum := extractEnum(typ); enum != nil {
		if enum.HasDrop() {
			return true
		}
		// B0212: Check for mono enum instance with codegen-time synthesized drop.
		// Generic enums like Slot[K,V] have TypeParam variant fields — sema can't
		// detect droppability, but the mono drop may have been generated.
		if inst, ok := typ.(*types.Instance); ok {
			mangledName := mangleMethodName(monoName(inst), "drop", false)
			if _, ok := c.funcs[mangledName]; ok {
				return true
			}
		}
	}
	return false
}

// emitNullGuardedVariantDrop runs emitDrop only when `instance` is non-null,
// branching past it otherwise. T0633: T0623's move-out path
// (nullSubjectHandleSlot) zero-inits a moved-out user-type wrapper slot in the
// subject's enum-value alloca, so the subject's synth enum drop later walks
// that slot with a {null,null} value struct. The user-type drop dispatch
// branches below dereference the extracted instance ptr (drop-fn call,
// loadVariantPtr, field loads) and would segfault on null. Skipping the drop
// when the instance is null is behavior-neutral for normal drops — a
// constructed heap user type always has a non-null instance ptr; only the
// Part-1 zero-init'd moved-out slot is skipped. Mirrors the existing B0218
// null-check idiom further down in emitVariantFieldDrop.
func (c *Compiler) emitNullGuardedVariantDrop(instance value.Value, emitDrop func()) {
	ptrType, ok := instance.Type().(*irtypes.PointerType)
	if !ok {
		ptrType = irtypes.I8Ptr
	}
	isNull := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(ptrType))
	dropBlock := c.newBlock("varfield.drop")
	skipBlock := c.newBlock("varfield.skip")
	c.block.NewCondBr(isNull, skipBlock, dropBlock)
	c.block = dropBlock
	emitDrop()
	if c.block.Term == nil {
		c.block.NewBr(skipBlock)
	}
	c.block = skipBlock
}

// emitCoroTaskParkSuspendWait emits the cooperative park-suspend join for an
// un-awaited Task handle (T0668). It is the single shared park-suspend emitter
// also used by genReceiveTask's `<-t` await path, so the drop-join and the
// receive-join cannot diverge.
//
// Precondition: c.inCoroutine && c.coroSuspendBlk != nil; c.block is the block
// where it is not yet known whether G is done (the "wait" block). gPtr is the
// target G* (handle bitcast); doneField is the GEP to target G.done. The join
// re-checks G.done under sched.done_lock and, if still not done, parks the
// current G on the target G's done_waiters list (woken by
// promise_goroutine_exit) and coro.suspends. Both the under-lock-done path and
// the post-resume path branch to readyBlk (where G.done is guaranteed set).
// Mirrors genReceiveTask's c.inCoroutine branch exactly.
func (c *Compiler) emitCoroTaskParkSuspendWait(gPtr, doneField value.Value, readyBlk *ir.Block) {
	gTy := goroutineStructType()

	// Goroutine-mode: use sched.done_lock to protect done + done_waiters
	// atomically. Hold the lock across coro.suspend via G.park_mutex so the
	// scheduler releases it after suspend completes — prevents the
	// enqueue-before-suspend race.
	currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
	currentGPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTy))

	schedTy := schedStructType()
	doneLockField := c.block.NewGetElementPtr(schedTy, c.schedGlobal,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(schedFieldDoneLock)))
	doneLock := c.block.NewLoad(irtypes.I8Ptr, doneLockField)
	c.block.NewCall(c.palMutexLock, doneLock)

	// Re-check G.done under lock
	recheckDone := c.block.NewLoad(irtypes.I8, doneField)
	recheckIsDone := c.block.NewICmp(enum.IPredNE, recheckDone, constant.NewInt(irtypes.I8, 0))
	doneUnderLockBlk := c.newBlock("task.done_under_lock")
	parkBlk := c.newBlock("task.park")
	c.block.NewCondBr(recheckIsDone, doneUnderLockBlk, parkBlk)

	// task.done_under_lock: target already done — unlock and proceed
	c.block = doneUnderLockBlk
	c.block.NewCall(c.palMutexUnlock, doneLock)
	c.block.NewBr(readyBlk)

	// task.park: set status = waiting, prepend to done_waiters, park_mutex = done_lock, suspend
	c.block = parkBlk
	curStatusField := c.block.NewGetElementPtr(gTy, currentGPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldStatus)))
	c.block.NewStore(constant.NewInt(irtypes.I8, gStatusWaiting), curStatusField)

	// Prepend current G to target G's done_waiters list
	dwField := c.block.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldDoneWaiters)))
	oldHead := c.block.NewLoad(irtypes.I8Ptr, dwField)
	curWaitNextField := c.block.NewGetElementPtr(gTy, currentGPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldWaitNext)))
	c.block.NewStore(oldHead, curWaitNextField)
	c.block.NewStore(currentG, dwField)

	// Store done_lock as park_mutex — scheduler will release after suspend
	pmField := c.block.NewGetElementPtr(gTy, currentGPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
	c.block.NewStore(doneLock, pmField)

	// Suspend (lock held — scheduler releases it)
	suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
	resumeBlk := c.newBlock("task.resume")
	c.block.NewSwitch(suspResult, c.coroSuspendBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), resumeBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))
	resumeBlk.NewBr(readyBlk)
}

// emitCoroTaskJoinAndFree emits, in the current coroutine block, an un-awaited
// Task handle join+cleanup (T0668): null-check handle → fast G.done check →
// cooperative park-suspend wait → freeAfterDoneFn(handle). Precondition:
// c.inCoroutine && c.coroSuspendBlk != nil. On return c.block is a fresh
// continuation block.
func (c *Compiler) emitCoroTaskJoinAndFree(handle value.Value, freeAfterDoneFn *ir.Func) {
	gTy := goroutineStructType()

	isNull := c.block.NewICmp(enum.IPredEQ, handle, constant.NewNull(irtypes.I8Ptr))
	notNullBlk := c.newBlock("taskjoin.notnull")
	contBlk := c.newBlock("taskjoin.cont")
	c.block.NewCondBr(isNull, contBlk, notNullBlk)

	c.block = notNullBlk
	gPtr := c.block.NewBitCast(handle, irtypes.NewPointer(gTy))
	doneField := c.block.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldDone)))
	doneVal := c.block.NewLoad(irtypes.I8, doneField)
	isDone := c.block.NewICmp(enum.IPredNE, doneVal, constant.NewInt(irtypes.I8, 0))
	fastDoneBlk := c.newBlock("taskjoin.fastdone")
	waitBlk := c.newBlock("taskjoin.wait")
	readyBlk := c.newBlock("taskjoin.ready")
	c.block.NewCondBr(isDone, fastDoneBlk, waitBlk)

	fastDoneBlk.NewBr(readyBlk)

	c.block = waitBlk
	c.emitCoroTaskParkSuspendWait(gPtr, doneField, readyBlk)

	c.block = readyBlk
	c.block.NewCall(freeAfterDoneFn, handle)
	c.block.NewBr(contBlk)

	c.block = contBlk
}

// emitTaskJoinAndFree drops an un-awaited Task handle (T0668). In a coroutine
// body (test bodies, WASM main, every go {} body) it emits the cooperative
// park-suspend join + Task[T].free_after_done so the single-threaded WASM
// scheduler can run the pending goroutine. Otherwise it falls back to the
// legacy callable Task[T].drop (busy spin on host; cooperative-step pump on
// WASM for genuinely non-coroutine drop bodies).
func (c *Compiler) emitTaskJoinAndFree(handle value.Value, elemType types.Type, failable bool) {
	if c.inCoroutine && c.coroSuspendBlk != nil {
		c.emitCoroTaskJoinAndFree(handle, c.getOrCreateTaskFreeAfterDone(elemType, failable))
		return
	}
	c.block.NewCall(c.getOrCreateTaskDrop(elemType, failable), handle)
}

// emitTaskJoinAndFreeByDropFn is the temp/binding-site variant of
// emitTaskJoinAndFree (T0668): the call site only knows the Task[T].drop func
// (stored on a stmtTemp/scopeBinding), not the element type. The
// drop→free_after_done pairing recorded by getOrCreateTaskDrop recovers the
// cooperative path. dropFn must be a Task[T].drop produced by
// getOrCreateTaskDrop; returns false (caller emits its own legacy call) if the
// pairing is unknown.
func (c *Compiler) emitTaskJoinAndFreeByDropFn(handle value.Value, dropFn *ir.Func) bool {
	faf, ok := c.taskFreeAfterDone[dropFn]
	if !ok {
		return false
	}
	if c.inCoroutine && c.coroSuspendBlk != nil {
		c.emitCoroTaskJoinAndFree(handle, faf)
		return true
	}
	c.block.NewCall(dropFn, handle)
	return true
}

// emitVariantFieldDrop emits a drop call for a single variant field value.
func (c *Compiler) emitVariantFieldDrop(fieldVal value.Value, typ types.Type) {
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}

	named := extractNamed(typ)
	if named != nil {
		// String: call promise_string_drop directly
		if named == types.TypString {
			if dropFn, ok := c.funcs["promise_string_drop"]; ok {
				c.block.NewCall(dropFn, fieldVal)
			}
			return
		}
		// Vector: drop elements first, then call Vector.drop.
		// B0212: Drop enum elements in variant vectors. The variant's vector is owned
		// by the enum and only accessible through match/destructuring (which doesn't
		// register drop bindings), so each element is uniquely owned.
		if elemType, isVec := types.AsVector(typ); isVec || named == types.TypVector {
			if isVec {
				c.emitVectorElementDropLoop(fieldVal, elemType)
			}
			if dropFn, ok := c.funcs["Vector.drop"]; ok {
				c.block.NewCall(dropFn, fieldVal)
			}
			return
		}
		// Channel: per-element-type drop walks any un-received buffered items.
		// T0663: nested Channel[Channel[T]] terminates because the inner element
		// narrows (Channel[int] → int, value, no further loop).
		if chElem, isCh := types.AsChannel(typ); isCh || named == types.TypChannel {
			if chElem != nil {
				c.block.NewCall(c.getOrCreateChannelDrop(chElem), fieldVal)
			}
			return
		}
		// T0508: Arc/Weak/Mutex/Task are native handle types — LLVM value is a
		// single i8*, not a fat-pointer value struct. Call the per-element-type
		// drop function with fieldVal directly (no extractInstancePtr, no pal_free
		// — these drops free their own allocation).
		if arcElem, isArc := types.AsArc(typ); isArc {
			dropFn := c.getOrCreateArcDrop(arcElem)
			c.block.NewCall(dropFn, fieldVal)
			return
		}
		if weakElem, isWeak := types.AsWeak(typ); isWeak {
			dropFn := c.getOrCreateWeakDrop(weakElem)
			c.block.NewCall(dropFn, fieldVal)
			return
		}
		if mutexElem, isMutex := types.AsMutex(typ); isMutex {
			dropFn := c.getOrCreateMutexDrop(mutexElem)
			c.block.NewCall(dropFn, fieldVal)
			return
		}
		if _, isMG := types.AsMutexGuard(typ); isMG || named == types.TypMutexGuard {
			if dropFn, ok := c.funcs["MutexGuard.drop"]; ok {
				c.block.NewCall(dropFn, fieldVal)
			}
			return
		}
		if taskElem, isTask, taskFail := types.AsAnyTaskFailable(typ); isTask || types.IsTaskLikeOrigin(named) {
			// T0668: central chokepoint for Vector/array/tuple element loops and
			// enum-variant drops. Route through the cooperative join so an
			// un-awaited Task in a container dropped inside a coroutine (test
			// body / WASM main / go {}) parks instead of livelocking the
			// single-threaded WASM scheduler. T1379: FailableTask discharges its
			// buffered aggregate in free_after_done.
			c.emitTaskJoinAndFree(fieldVal, taskElem, taskFail)
			return
		}
		// T0765: Structural-interface field (non-value-type). The variant data slot
		// holds a {vtable, instance} value struct; extract the instance pointer and
		// dispatch through __promise_structural_drop (RTTI: typeinfo.drop_fn_ptr →
		// concrete drop, else pal_free). Mirrors the struct-field walk (T0460). The
		// null guard also covers a moved-out, zero-init'd variant slot (T0633).
		if named.IsStructural() && !named.IsValueType() {
			if c.structuralDrop != nil {
				instancePtr := c.extractInstancePtr(fieldVal)
				c.emitNullGuardedVariantDrop(instancePtr, func() {
					c.block.NewCall(c.structuralDrop, instancePtr)
				})
			}
			return
		}
		// T0387: Polymorphic heap user type — dispatch through typeinfo's
		// drop_fn_ptr so subclass-only droppable fields (e.g. a string field on
		// Container is Shape) are reached. The static-type drop walk would only
		// see Shape's fields (or none), missing Container's string. Symmetric
		// with the polymorphic clone dispatch in dupHeapValue.
		if c.needsVtable(named) && !named.IsStructural() && !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) {
			instance := c.extractInstancePtr(fieldVal)
			// T0633: a moved-out polymorphic wrapper slot is zero-init'd;
			// emitStructuralInstanceDrop dereferences instance via
			// loadVariantPtr with no null-check — guard it.
			c.emitNullGuardedVariantDrop(instance, func() {
				c.emitStructuralInstanceDrop(instance)
			})
			return
		}
		// User type with explicit or synthesized drop: extract instance ptr and call drop.
		// B0257: For types with explicit (user-written) drop, the drop method does NOT
		// free instance memory — pal_free must be emitted separately (matching scope-exit
		// behavior). Synthesized drops already include pal_free in the generated body.
		if named.HasDrop() || named.NeedsSynthDrop() {
			instance := c.extractInstancePtr(fieldVal)
			ownerName := c.resolveDropOwner(named)
			if inst, ok := typ.(*types.Instance); ok {
				ownerName = monoName(inst)
			}
			mangledName := mangleMethodName(ownerName, "drop", false)
			// T0633: a moved-out user-type wrapper slot is zero-init'd; the
			// drop fn and B0257 pal_free both deref the null instance — guard.
			c.emitNullGuardedVariantDrop(instance, func() {
				if dropFn, ok := c.funcs[mangledName]; ok {
					c.block.NewCall(dropFn, instance)
				}
				// B0257: Explicit (user-written) drops don't free instance
				// memory — synthesized drops already include pal_free in the
				// generated body.
				if named.HasDrop() && !named.NeedsSynthDrop() {
					c.block.NewCall(c.palFree, instance)
				}
			})
			return
		}
		// B0202: Mono instance with codegen-time synthesized drop
		if inst, ok := typ.(*types.Instance); ok {
			mangledName := mangleMethodName(monoName(inst), "drop", false)
			if dropFn, ok := c.funcs[mangledName]; ok {
				instance := c.extractInstancePtr(fieldVal)
				// T0633: a moved-out generic-wrapper slot is zero-init'd; the
				// drop fn and B0257 pal_free both deref the null instance —
				// guard.
				c.emitNullGuardedVariantDrop(instance, func() {
					c.block.NewCall(dropFn, instance)
					// B0257: Explicit (user-written) drops don't free instance
					// memory.
					if n, ok2 := inst.Origin().(*types.Named); ok2 && n.HasDrop() && !n.NeedsSynthDrop() {
						c.block.NewCall(c.palFree, instance)
					}
				})
				return
			}
		}
		// B0218: Heap-allocated user type without any drop — just pal_free the instance.
		if !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural() {
			instance := c.extractInstancePtr(fieldVal)
			// Null-check to avoid freeing zero-initialized fields
			isNull := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(irtypes.I8Ptr))
			freeBlock := c.newBlock("varfield.free")
			skipBlock := c.newBlock("varfield.skip")
			c.block.NewCondBr(isNull, skipBlock, freeBlock)
			c.block = freeBlock
			c.block.NewCall(c.palFree, instance)
			c.block.NewBr(skipBlock)
			c.block = skipBlock
		}
		return
	}

	// T0739: Closure (function value) field — a fat pointer {fn_ptr, env_ptr}.
	// A capturing closure heap-allocates its env struct; drop it via
	// emitEnvDropOrFree (loads the env's field-0 drop fn and calls it — dropping
	// captured values — else pal_free). A no-capture closure has a null env, so
	// null-check and skip. Reached by: a Task[T] result (Task[T].drop), a Vector
	// element (emitVectorElementDropLoop), and a Tuple/Array element (the Tuple
	// and Array branches below recurse here unconditionally). Without it, a
	// closure in any of those leaks its env (e.g. a dropped-not-awaited
	// `go { || -> base + 2 }`).
	// T0741: closures in an *enum* payload, an *optional*, or a plain struct
	// closure *field* with heap captures are now handled too — enum payloads via
	// variantFieldNeedsDrop's Signature case (reaching this case), optionals via
	// emitOptionalValueDrop's Signature case, and struct closure fields via
	// emitFuncFieldEnvFree (now a deep emitEnvDropOrFree). Each is paired with a
	// claimEnvTemp at its construction site so the env is owned exactly once.
	if _, ok := typ.(*types.Signature); ok {
		if st, isStruct := fieldVal.Type().(*irtypes.StructType); isStruct && len(st.Fields) == 2 {
			envPtr := c.block.NewExtractValue(fieldVal, 1)
			isNull := c.block.NewICmp(enum.IPredEQ, envPtr, constant.NewNull(irtypes.I8Ptr))
			freeBlk := c.newBlock("closure.env.free")
			skipBlk := c.newBlock("closure.env.skip")
			c.block.NewCondBr(isNull, skipBlk, freeBlk)
			c.block = freeBlk
			c.emitEnvDropOrFree(envPtr)
			c.block.NewBr(skipBlk)
			c.block = skipBlk
		}
		return
	}

	// Tuple field: extract and drop each droppable element.
	// B0264: Vector[(string, int)] leaks because tuple elements were never dropped.
	if tup, ok := typ.(*types.Tuple); ok {
		for i, e := range tup.Elems() {
			resolved := e
			if c.typeSubst != nil {
				resolved = types.Substitute(resolved, c.typeSubst)
			}
			elemVal := c.block.NewExtractValue(fieldVal, uint64(i))
			c.emitVariantFieldDrop(elemVal, resolved)
		}
		return
	}

	// T0485: Optional<T> field — delegate to emitOptionalValueDrop which already
	// handles every relevant inner type (string, Vector, Channel, heap user type,
	// nested Optional). The variant-data layout for Optional<T> is {i1, T_llvm}
	// (see llvmTypeForEnumFieldFromPromise), matching what emitOptionalValueDrop
	// expects from a loaded {i1, T} value.
	if opt, ok := typ.(*types.Optional); ok {
		c.emitOptionalValueDrop(fieldVal, opt)
		return
	}

	// T0485: Fixed-size [N]T field — extract each element and recurse. Variant
	// data layout for [N]T is `[N x T_llvm]`.
	if arr, ok := typ.(*types.Array); ok {
		elemType := arr.Elem()
		if c.typeSubst != nil {
			elemType = types.Substitute(elemType, c.typeSubst)
		}
		for i := int64(0); i < arr.Size(); i++ {
			elemVal := c.block.NewExtractValue(fieldVal, uint64(i))
			c.emitVariantFieldDrop(elemVal, elemType)
		}
		return
	}

	// Enum field: pass pointer to an alloca.
	// B0212: Also handle mono enum instances where HasDrop is false on the origin
	// (sema couldn't detect droppability for TypeParam variant fields) but a mono
	// drop function was generated at codegen time.
	if enum := extractEnum(typ); enum != nil {
		enumName := enum.Obj().Name()
		if inst, ok := typ.(*types.Instance); ok {
			enumName = monoName(inst)
		}
		mangledName := mangleMethodName(enumName, "drop", false)
		dropFn := c.funcs[mangledName]
		// B0212: If the drop function is not found, try module-prefixed names.
		// During module compilation, cross-module enum drops may not yet be registered
		// under the plain name. Forward-declare if found in a module's declarations.
		if dropFn == nil && c.moduleInfos != nil {
			dropFn = c.forwardDeclareModuleEnumDrop(enum, enumName, mangledName)
		}
		if dropFn != nil {
			alloca := c.createEntryAlloca(fieldVal.Type())
			c.block.NewStore(fieldVal, alloca)
			ptr := c.block.NewBitCast(alloca, irtypes.I8Ptr)
			c.block.NewCall(dropFn, ptr)
		}
	}
}

// forwardDeclareModuleEnumDrop searches module infos for the enum's owning module
// and forward-declares its drop function with the correct module-prefixed name.
// Returns the declared function, or nil if not found. B0212.
func (c *Compiler) forwardDeclareModuleEnumDrop(enum *types.Enum, enumName, plainMangledName string) *ir.Func {
	savedInfo := c.info
	defer func() { c.info = savedInfo }()

	for _, modInfo := range c.moduleInfos {
		c.info = modInfo.SemaInfo
		irName := modInfo.EffectiveIRPrefix()
		for _, decl := range modInfo.File.Decls {
			ed, ok := decl.(*ast.EnumDecl)
			if !ok || ed.Name != enumName {
				continue
			}
			if modInfo.SemaInfo.FilteredDecls[decl] {
				continue
			}
			foundEnum := c.lookupEnumType(ed.Name)
			if foundEnum != enum {
				continue
			}
			// Fieldless enums don't need a drop — skip forward-declaration.
			if !foundEnum.NeedsSynthDrop() && !foundEnum.HasDrop() {
				return nil
			}
			// Found the module that owns this enum — declare or find its drop
			moduleMangledName := mangleModuleMethodName(irName, enumName, "drop", false)
			if fn, ok := c.funcs[moduleMangledName]; ok {
				c.funcs[plainMangledName] = fn
				return fn
			}
			// Forward-declare with module ownership so IR splitting works correctly
			fn := c.module.NewFunc(moduleMangledName, irtypes.Void,
				ir.NewParam("this", irtypes.I8Ptr))
			c.funcs[moduleMangledName] = fn
			c.funcs[plainMangledName] = fn
			c.moduleOwnedFuncs[moduleMangledName] = irName
			return fn
		}
	}
	return nil
}

// forwardDeclareModuleEnumClone searches module infos for the enum's owning module
// and forward-declares its clone function. Returns the declared function, or nil
// if not found. B0244: Fixes cross-module compilation order where Map[K,V].clone()
// in std needs JsonValue.clone from json, but json isn't compiled yet.
func (c *Compiler) forwardDeclareModuleEnumClone(enum *types.Enum, plainMangledName string, resolvedType types.Type) *ir.Func {
	enumName := enum.Obj().Name()
	savedInfo := c.info
	defer func() { c.info = savedInfo }()

	for _, modInfo := range c.moduleInfos {
		c.info = modInfo.SemaInfo
		irName := modInfo.EffectiveIRPrefix()
		for _, decl := range modInfo.File.Decls {
			ed, ok := decl.(*ast.EnumDecl)
			if !ok || ed.Name != enumName {
				continue
			}
			if modInfo.SemaInfo.FilteredDecls[decl] {
				continue
			}
			foundEnum := c.lookupEnumType(ed.Name)
			if foundEnum != enum {
				continue
			}
			// T1129: a `clone enum forward-declares its user clone; a recursive /
			// container-bearing droppable enum forward-declares its synthesized clone.
			if !foundEnum.IsClone() && !c.enumNeedsSynthClone(foundEnum, resolvedType) {
				return nil
			}
			// Found the module — check if already declared with module prefix
			moduleMangledName := mangleModuleMethodName(irName, enumName, "clone", false)
			if fn, ok := c.funcs[moduleMangledName]; ok {
				c.funcs[plainMangledName] = fn
				return fn
			}
			// Resolve the return type (enum value struct)
			retType := c.resolveType(resolvedType)
			// Forward-declare with module ownership
			fn := c.module.NewFunc(moduleMangledName, retType,
				ir.NewParam("this", irtypes.I8Ptr))
			c.funcs[moduleMangledName] = fn
			c.funcs[plainMangledName] = fn
			c.moduleOwnedFuncs[moduleMangledName] = irName
			return fn
		}
	}
	return nil
}
