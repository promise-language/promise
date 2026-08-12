package codegen

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// --- Literals ---

func (c *Compiler) genIntLit(e *ast.IntLit) value.Value {
	typ := c.info.Types[e]
	named := extractNamed(typ)
	if named == nil {
		named = types.TypInt
	}
	lt := llvmNamedType(named)
	intType, ok := lt.(*irtypes.IntType)
	if !ok {
		intType = irtypes.I64
	}
	raw := strings.ReplaceAll(e.Raw, "_", "")
	// Parse the magnitude at arbitrary precision so wide literals (>64-bit)
	// are represented losslessly. big.Int.SetString(_, 0) handles the 0x/0o/0b
	// prefixes; the AST never carries a sign here (unary minus is a separate op).
	if bv, ok := new(big.Int).SetString(raw, 0); ok {
		return &constant.Int{Typ: intType, X: bv}
	}
	// Fallback: unparseable literal (sema already reported the error).
	return constant.NewInt(intType, 0)
}

func (c *Compiler) genFloatLit(e *ast.FloatLit) value.Value {
	typ := c.info.Types[e]
	named := extractNamed(typ)
	if named == nil {
		named = types.TypF64
	}
	lt := llvmNamedType(named)
	floatType, ok := lt.(*irtypes.FloatType)
	if !ok {
		floatType = irtypes.Double
	}
	raw := strings.ReplaceAll(e.Raw, "_", "")
	// Parse with the target precision so round-to-nearest-even is correct.
	// ParseFloat(s, 32) returns a float64 holding the correctly-rounded float32 value.
	bitSize := 64
	if floatType == irtypes.Float {
		bitSize = 32
	}
	val, _ := strconv.ParseFloat(raw, bitSize)
	return constant.NewFloat(floatType, val)
}

func (c *Compiler) genBoolLit(e *ast.BoolLit) value.Value {
	if e.Value {
		return constant.NewInt(irtypes.I1, 1)
	}
	return constant.NewInt(irtypes.I1, 0)
}

func (c *Compiler) genCharLit(e *ast.CharLit) value.Value {
	raw := e.Raw
	inner := raw[1 : len(raw)-1] // strip surrounding quotes
	var cp int32
	if len(inner) > 1 && inner[0] == '\\' {
		switch inner[1] {
		case 'n':
			cp = '\n'
		case 'r':
			cp = '\r'
		case 't':
			cp = '\t'
		case 'b':
			cp = '\b'
		case '\\':
			cp = '\\'
		case '\'':
			cp = '\''
		case '0':
			cp = 0
		default:
			cp = int32(inner[1])
		}
	} else {
		r, _ := utf8.DecodeRuneInString(inner)
		cp = int32(r)
	}
	return constant.NewInt(irtypes.I32, int64(cp))
}

func (c *Compiler) genStringLit(e *ast.StringLit) value.Value {
	if hasInterpolation(e.Parts) {
		return c.genInterpolatedString(e)
	}
	return c.genStaticString(e)
}

// genStaticString handles strings with no interpolation — compile-time constant path.
func (c *Compiler) genStaticString(e *ast.StringLit) value.Value {
	var buf strings.Builder
	switch e.Kind {
	case ast.StringTriple:
		if len(e.Raw) >= 6 {
			buf.WriteString(e.Raw[3 : len(e.Raw)-3])
		}
	case ast.StringRaw:
		if len(e.Raw) >= 3 {
			buf.WriteString(e.Raw[2 : len(e.Raw)-1])
		}
	default:
		for _, part := range e.Parts {
			switch p := part.(type) {
			case ast.StringText:
				buf.WriteString(p.Text)
			case ast.StringEscape:
				buf.WriteString(resolveEscape(p.Sequence))
			}
		}
	}
	return c.makeRuntimeString(buf.String())
}

// genInterpolatedString handles strings with interpolation — runtime concatenation path.
func (c *Compiler) genInterpolatedString(e *ast.StringLit) value.Value {
	var parts []value.Value
	var staticBuf strings.Builder

	for _, part := range e.Parts {
		switch p := part.(type) {
		case ast.StringText:
			staticBuf.WriteString(p.Text)
		case ast.StringEscape:
			staticBuf.WriteString(resolveEscape(p.Sequence))
		case ast.StringInterp:
			// Skip interpolation with nil Expr (empty {} or parse failure —
			// sema reports the error; treat as empty string to avoid panic).
			if p.Expr == nil {
				continue
			}
			// Flush static buffer as a string
			if staticBuf.Len() > 0 {
				parts = append(parts, c.makeRuntimeString(staticBuf.String()))
				staticBuf.Reset()
			}
			// Evaluate expression and convert to string. Use the
			// auto-propagate path so bare failable calls (`name!`) unwrap
			// their result inside interpolation slots (T0966).
			val := c.genExprAutoPropagate(p.Expr)
			// T0966: a bare auto-propagated failable call leaves an unowned
			// heap temp (string/vector/user type). convertToString copies it,
			// so the original would leak. Track it for statement-end cleanup,
			// mirroring the explicit `?^`/`?!` paths in genExpr.
			if c.info.AutoPropagateExprs[p.Expr] {
				c.trackUnwrappedFailableTemp(p.Expr, val)
			}
			strVal := c.convertToString(val, c.info.Types[p.Expr])
			// B0168: Track convertToString results as temps (all types now allocate,
			// including strings after B0248 copy fix).
			c.trackStringTemp(strVal)
			parts = append(parts, strVal)
		}
	}
	// Flush remaining static text
	if staticBuf.Len() > 0 {
		parts = append(parts, c.makeRuntimeString(staticBuf.String()))
	}

	// Concatenate all parts
	if len(parts) == 0 {
		return c.makeRuntimeString("")
	}
	result := parts[0]
	for _, part := range parts[1:] {
		result = c.block.NewCall(c.funcs["promise_string_concat"], result, part)
		// B0168: Track intermediate concat results. Each concat allocates a new
		// heap string; all but the final result are dead after the next concat.
		// The final result is tracked too — claimed if assigned to a variable,
		// otherwise dropped at statement end.
		c.trackStringTemp(result)
	}
	return result
}

// makeRuntimeString emits a static string instance global in .rodata.
// The global contains the full string instance struct: { i8* _variant, i64 len, [N x i8] data }.
// The length field has bit 63 set (negative) to mark it as a literal string — this
// prevents promise_string_drop from freeing the .rodata pointer.
// When compiling module code, names use a per-module counter so the constant
// names are stable (independent of how many string constants user code has).
func (c *Compiler) makeRuntimeString(s string) value.Value {
	n := len(s)

	// Build concrete struct type with actual array size (not [0 x i8] FAM)
	concreteType := irtypes.NewStruct(
		irtypes.I8Ptr,                           // _variant
		irtypes.I64,                             // len (sign bit = literal flag)
		irtypes.NewArray(uint64(n), irtypes.I8), // data
	)

	// Length with literal flag (sign bit) set
	literalLen := int64(n) | math.MinInt64

	init := constant.NewStruct(concreteType,
		constant.NewNull(irtypes.I8Ptr),
		constant.NewInt(irtypes.I64, literalLen),
		constant.NewCharArrayFromString(s),
	)

	var globalName string
	if c.compilingModule != "" {
		globalName = fmt.Sprintf(".str.__mod_%s.%d", c.compilingModule, c.moduleStrCounter)
		c.moduleStrCounter++
	} else {
		globalName = fmt.Sprintf(".str.%d", c.strCounter)
		c.strCounter++
	}
	global := c.module.NewGlobalDef(globalName, init)
	global.Immutable = true
	global.Linkage = enum.LinkagePrivate

	// Bitcast global pointer to i8* (the string instance pointer type used everywhere)
	return c.block.NewBitCast(global, irtypes.I8Ptr)
}

// convertTupleToString formats a tuple value as "(elem0, elem1, ...)".
func (c *Compiler) convertTupleToString(val value.Value, tup *types.Tuple) value.Value {
	elems := tup.Elems()
	parts := make([]value.Value, 0, len(elems)*2+2)
	parts = append(parts, c.makeRuntimeString("("))
	for i, elemType := range elems {
		if i > 0 {
			parts = append(parts, c.makeRuntimeString(", "))
		}
		elemVal := c.block.NewExtractValue(val, uint64(i))
		strVal := c.convertToString(elemVal, elemType)
		// B0254: Track convertToString results as temps to prevent leak.
		c.trackStringTemp(strVal)
		parts = append(parts, strVal)
	}
	parts = append(parts, c.makeRuntimeString(")"))
	// Concatenate all parts
	result := parts[0]
	for _, part := range parts[1:] {
		result = c.block.NewCall(c.funcs["promise_string_concat"], result, part)
		// B0168: Track intermediate concat results (same as genInterpolatedString).
		c.trackStringTemp(result)
	}
	return result
}

// convertToString converts a value to a string (i8*) for interpolation.
func (c *Compiler) convertToString(val value.Value, typ types.Type) value.Value {
	// Handle TypeParam: substitute to concrete type in monomorphic context.
	if tp, ok := typ.(*types.TypeParam); ok {
		if c.typeSubst != nil {
			if concrete := c.typeSubst[tp]; concrete != nil {
				return c.convertToString(val, concrete)
			}
		}
		panic(fmt.Sprintf("codegen: unresolved TypeParam %s in string interpolation", typ))
	}
	// Handle optional types: print inner value if present, "none" if absent.
	if opt, ok := typ.(*types.Optional); ok {
		flag := c.block.NewExtractValue(val, 0)
		someBlock := c.newBlock("interp.some")
		noneBlock := c.newBlock("interp.none")
		mergeBlock := c.newBlock("interp.merge")
		c.block.NewCondBr(flag, someBlock, noneBlock)

		c.block = someBlock
		innerVal := c.block.NewExtractValue(val, 1)
		someStr := c.convertToString(innerVal, opt.Elem())
		someEnd := c.block
		c.block.NewBr(mergeBlock)

		c.block = noneBlock
		noneStr := c.makeRuntimeString("none")
		noneEnd := c.block
		c.block.NewBr(mergeBlock)

		c.block = mergeBlock
		phi := c.block.NewPhi(ir.NewIncoming(someStr, someEnd), ir.NewIncoming(noneStr, noneEnd))
		return phi
	}

	// Handle tuple types: format as (elem0, elem1, ...)
	if tup, ok := typ.(*types.Tuple); ok {
		return c.convertTupleToString(val, tup)
	}

	// Handle enum types: synthesize switch on tag → variant name string.
	if enum := extractEnum(typ); enum != nil {
		return c.convertEnumToString(val, typ, enum)
	}

	named := extractNamed(typ)
	if named == nil {
		// Unknown type — produce type name as fallback
		return c.makeRuntimeString("<" + typ.String() + ">")
	}
	switch named {
	case types.TypString:
		// B0248: Must copy the string to avoid aliasing the original.
		// Without this, "{s}" returns the same pointer as s, causing double-free.
		emptyStr := c.makeRuntimeString("")
		return c.block.NewCall(c.funcs["promise_string_concat"], val, emptyStr)
	case types.TypInt, types.TypI64:
		return c.block.NewCall(c.funcs["promise_int_to_string"], val)
	case types.TypI32:
		ext := c.block.NewSExt(val, irtypes.I64)
		return c.block.NewCall(c.funcs["promise_int_to_string"], ext)
	case types.TypI16:
		ext := c.block.NewSExt(val, irtypes.I64)
		return c.block.NewCall(c.funcs["promise_int_to_string"], ext)
	case types.TypI8:
		ext := c.block.NewSExt(val, irtypes.I64)
		return c.block.NewCall(c.funcs["promise_int_to_string"], ext)
	case types.TypUint, types.TypU64:
		return c.block.NewCall(c.funcs["promise_uint_to_string"], val)
	case types.TypU32, types.TypU16, types.TypU8:
		ext := c.block.NewZExt(val, irtypes.I64)
		return c.block.NewCall(c.funcs["promise_uint_to_string"], ext)
	case types.TypF64:
		return c.block.NewCall(c.funcs["promise_f64_to_string"], val)
	case types.TypF32:
		return c.block.NewCall(c.funcs["promise_f32_to_string"], val)
	case types.TypBool:
		i8Val := c.block.NewZExt(val, irtypes.I8)
		return c.block.NewCall(c.funcs["promise_bool_to_string"], i8Val)
	case types.TypChar:
		return c.block.NewCall(c.funcs["promise_char_to_string"], val)
	default:
		// User-defined type: call format(Writer ~w)! via Builder
		if named.LookupMethod("format") == nil {
			// No format method — produce type name as fallback.
			// This can happen when mono generates Vector[T].format() for a T
			// that doesn't implement Format (e.g., internal types, tuples).
			return c.makeRuntimeString("<" + named.Obj().Name() + ">")
		}
		return c.callFormatToString(val, typ, named)
	}
}

// callFormatToString creates a Builder, calls the type's format() method to write
// into it, then returns the resulting string from Builder.to_string().
func (c *Compiler) callFormatToString(val value.Value, typ types.Type, named *types.Named) value.Value {
	// 1. Create a Builder instance
	builderNamed := c.lookupNamedType("Builder")
	layout := c.layouts[builderNamed]
	if layout == nil {
		panic("codegen: Builder type layout not found")
	}
	instanceStructType := layout.Instance.LLVMType
	instancePtrType := layout.InstancePtrType

	// Compute size via GEP-from-null trick
	nullPtr := constant.NewNull(instancePtrType)
	sizePtr := c.block.NewGetElementPtr(instanceStructType, nullPtr,
		constant.NewInt(irtypes.I32, 1))
	sizeRaw := c.block.NewPtrToInt(sizePtr, c.ptrIntType())
	var size value.Value = sizeRaw
	if c.isWasm {
		size = c.block.NewZExt(sizeRaw, irtypes.I64)
	}

	rawPtr := c.block.NewCall(c.palAlloc, size)
	typedPtr := c.block.NewBitCast(rawPtr, instancePtrType)

	// Store type info pointer in _variant slot (field 0)
	variantFieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	variantPtrType := layout.Instance.Fields[0].LLVMType.(*irtypes.PointerType)
	if tiGlobal := c.typeInfoGlobals[builderNamed]; tiGlobal != nil {
		c.block.NewStore(c.block.NewBitCast(tiGlobal, variantPtrType), variantFieldPtr)
	} else {
		c.block.NewStore(constant.NewNull(variantPtrType), variantFieldPtr)
	}

	// Zero-init remaining fields before calling new()
	for _, f := range builderNamed.AllFields() {
		fieldIdx, ok := layout.InstanceFieldIndex[f.Name()]
		if !ok {
			continue
		}
		fieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
		c.block.NewStore(c.zeroValue(layout.Instance.Fields[fieldIdx].LLVMType), fieldPtr)
	}

	// Call Builder.new(this, 16) — default capacity
	newFn := c.funcs["Builder.new"]
	c.block.NewCall(newFn, rawPtr, constant.NewInt(irtypes.I64, 16))

	// 2. Create Writer value struct {vtable_ptr, instance_ptr} from Builder
	writerVtable := c.getInterpBuilderWriterVtable()
	writerVal := c.block.NewInsertValue(
		constant.NewZeroInitializer(userValueType()),
		constant.NewBitCast(writerVtable, irtypes.I8Ptr), 0)
	writerVal = c.block.NewInsertValue(writerVal, rawPtr, 1)

	// 3. Get format method receiver from the user type value
	var receiver value.Value
	if named.IsValueType() {
		receiver = c.valueTypeReceiverPtr(val, typ)
	} else if _, ok := val.Type().(*irtypes.StructType); ok {
		// Value struct {vtable, instance} — extract instance ptr
		receiver = c.extractInstancePtr(val)
	} else {
		// Already i8* (this reference in a method body)
		receiver = val
	}

	// 4. Call TypeName.format(receiver, writer) — failable void returns {i1, i8*}
	formatResult := c.callFormatMethod(receiver, writerVal, val, named, typ)

	// 5. Handle failable result: panic on error
	tag := c.block.NewExtractValue(formatResult, 0)
	okBlock := c.newBlock("interp.format.ok")
	errBlock := c.newBlock("interp.format.err")
	c.block.NewCondBr(tag, errBlock, okBlock)

	c.block = errBlock
	errPtr := c.block.NewExtractValue(formatResult, 1)
	c.emitErrorPanic(errPtr, "", 0)

	c.block = okBlock

	// 6. Call Builder.to_string(builder_ptr) → string (i8*)
	toStringFn := c.funcs["Builder.to_string"]
	strResult := c.block.NewCall(toStringFn, rawPtr)

	// 7. T0084: Free the Builder after getting the string. Builder.to_string()
	// creates a NEW string via from_bytes (copies bytes), so the Builder is dead.
	// Call Builder.drop (synthesized: frees buf vector + instance) if available,
	// otherwise pal_free the instance directly.
	if builderDrop := c.funcs["Builder.drop"]; builderDrop != nil {
		c.block.NewCall(builderDrop, rawPtr)
	} else {
		c.block.NewCall(c.palFree, rawPtr)
	}

	return strResult
}

// convertEnumToString emits a switch on the enum tag and returns the matching variant name string.
func (c *Compiler) convertEnumToString(val value.Value, typ types.Type, enum *types.Enum) value.Value {
	layout := c.lookupEnumLayout(typ)
	if layout == nil {
		return c.makeRuntimeString("<" + enum.Obj().Name() + ">")
	}

	// Extract tag: fieldless enum → value IS the i32; data enum → field 0 of struct.
	var tag value.Value
	if layout.MaxVariantDataSize == 0 {
		tag = val
	} else {
		tag = c.block.NewExtractValue(val, 0)
	}

	switchBlock := c.block
	mergeBlock := c.newBlock("enum.interp.merge")
	defaultBlock := c.newBlock("enum.interp.default")

	var cases []*ir.Case
	var incomings []*ir.Incoming

	for _, v := range enum.Variants() {
		tagVal, ok := layout.VariantTag[v.Name()]
		if !ok {
			continue
		}
		caseBlock := c.newBlock("enum.interp." + v.Name())
		cases = append(cases, &ir.Case{X: constant.NewInt(irtypes.I32, int64(tagVal)), Target: caseBlock})
		c.block = caseBlock
		str := c.makeRuntimeString(v.Name())
		caseEnd := c.block
		c.block.NewBr(mergeBlock)
		incomings = append(incomings, ir.NewIncoming(str, caseEnd))
	}

	switchBlock.NewSwitch(tag, defaultBlock, cases...)

	c.block = defaultBlock
	defaultStr := c.makeRuntimeString("<unknown>")
	defaultEnd := c.block
	c.block.NewBr(mergeBlock)
	incomings = append(incomings, ir.NewIncoming(defaultStr, defaultEnd))

	c.block = mergeBlock
	return c.block.NewPhi(incomings...)
}

// callFormatMethod dispatches the format(Writer ~w)! call on the user type,
// using virtual dispatch when the type has children, direct dispatch otherwise.
// The writer is a MutRef param — passed as a pointer (B0149).
func (c *Compiler) callFormatMethod(receiver, writerVal, originalVal value.Value,
	named *types.Named, typ types.Type) value.Value {

	// Failable void result type: {i1, i8*}
	resultType := irtypes.NewStruct(irtypes.I1, irtypes.I8Ptr)

	// Store writerVal in a temp alloca and pass the pointer (MutRef, B0149)
	writerAlloca := c.createEntryAlloca(userValueType())
	c.block.NewStore(writerVal, writerAlloca)
	writerPtrType := irtypes.NewPointer(userValueType())

	if c.needsVtable(named) {
		// Virtual dispatch through vtable
		slotIndex := named.VirtualMethodIndex("format", false)
		if slotIndex < 0 {
			panic(fmt.Sprintf("codegen: format method not in vtable for %s", named))
		}

		// Get vtable pointer from the original value
		var vtableRaw value.Value
		if _, ok := originalVal.Type().(*irtypes.StructType); ok {
			vtableRaw = c.extractVtablePtr(originalVal)
		} else {
			// this reference (i8*) — load vtable from variant→typeinfo chain
			vtableRaw = c.loadVtablePtrFromInstance(originalVal)
		}

		vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
		fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
			constant.NewInt(irtypes.I32, int64(slotIndex)))
		fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)
		fnType := irtypes.NewFunc(resultType, irtypes.I8Ptr, writerPtrType)
		fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(fnType))
		return c.block.NewCall(fnTyped, receiver, writerAlloca)
	}

	// Direct dispatch
	mangledName := mangleMethodName(c.resolveTypeName(typ), "format", false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared method %s for interpolation", mangledName))
	}
	return c.block.NewCall(fn, receiver, writerAlloca)
}

// getInterpBuilderWriterVtable returns the Writer vtable global for Builder,
// creating it lazily on first use. Delegates to getOrEmitViewVtable so that
// non-failable Builder methods are wrapped in $view_adapt thunks that match
// the failable Writer interface ABI.
func (c *Compiler) getInterpBuilderWriterVtable() *ir.Global {
	if c.interpBuilderWriterVtable != nil {
		return c.interpBuilderWriterVtable
	}

	builderNamed := c.lookupNamedType("Builder")
	if builderNamed == nil {
		panic("codegen: Builder type not found for interpolation vtable")
	}
	writerNamed := c.lookupNamedType("Writer")
	if writerNamed == nil {
		panic("codegen: Writer type not found for interpolation vtable")
	}

	// getOrEmitViewVtable correctly handles non-failable → failable adaptation
	// via $view_adapt wrappers, so the vtable entries have the right ABI for
	// callers dispatching through the Writer interface.
	vt := c.getOrEmitViewVtable(builderNamed, writerNamed, builderNamed)
	c.interpBuilderWriterVtable = vt
	return vt
}

// hasInterpolation checks if a string literal contains any interpolation parts.
func hasInterpolation(parts []ast.StringPart) bool {
	for _, part := range parts {
		if _, ok := part.(ast.StringInterp); ok {
			return true
		}
	}
	return false
}

// resolveEscape converts an escape sequence token to its string value.
// The seq parameter contains the full lexer token (e.g., `\n` for a newline escape).
func resolveEscape(seq string) string {
	// Strip leading backslash if present (lexer includes it in the token)
	if len(seq) > 1 && seq[0] == '\\' {
		seq = seq[1:]
	}
	switch seq {
	case "n":
		return "\n"
	case "t":
		return "\t"
	case "r":
		return "\r"
	case "b":
		return "\b"
	case "\\":
		return "\\"
	case "\"":
		return "\""
	case "0":
		return "\x00"
	case "{":
		return "{"
	case "}":
		return "}"
	default:
		return "\\" + seq
	}
}

// --- Identifiers ---

func (c *Compiler) genIdentExpr(e *ast.IdentExpr) value.Value {
	// MutRef param: load through the caller's pointer (B0149)
	if ptr, ok := c.mutRefPtrs[e.Name]; ok {
		return c.block.NewLoad(c.mutRefTypes[e.Name], ptr)
	}
	// Local variable: load from alloca (checked first to shadow module-level names)
	if alloca, ok := c.locals[e.Name]; ok {
		val := c.block.NewLoad(alloca.ElemType, alloca)
		// T1170: a match-borrowed Optional/Array-of-heap binding (T0485) aliases the
		// subject enum's variant payload — sound for an in-scope read (the subject
		// outlives the narrowing scope) but a use-after-free when the binding escapes
		// (return / store-to-outer / consuming arg / constructor field): the subject's
		// synth enum drop frees the payload at scope exit while the escaped alias still
		// points into it. When an owning-escape context has set a dup flag
		// (dupStringFieldAccess/dupContainerFieldAccess), deep-clone so the escaped
		// value is independently owned; in-scope reads (no flag set) stay zero-copy.
		// Gated on matchBorrowedIdents membership → ordinary owned locals are untouched
		// (their escape still moves via clearDropFlag). The read/escape-side dup covers
		// BOTH the `if is` and `match` paths uniformly, since both populate
		// matchBorrowedIdents. ownerDroppable=true is valid: a binding is only marked
		// borrowed when the subject enum is droppable.
		if c.tempTrackingEnabled && c.matchBorrowedIdents != nil && c.matchBorrowedIdents[e.Name] {
			identType := c.info.Types[e]
			if c.typeSubst != nil && identType != nil {
				identType = types.Substitute(identType, c.typeSubst)
			}
			// Only enum-variant Optional/Array payload bindings take the escape dup
			// (isVariantPayloadBorrowShape) — bare-heap T0672 borrow bindings are
			// already owned copies and must not be re-dup'd (would leak).
			//
			// A whole Array[heap-user] variant payload is deliberately NOT dup'd
			// here: the explicit escape-site call to dupBorrowedHeapUserPayload
			// (return / assign / constructor field / consuming arg) already
			// element-wise deep-clones it via the same arrayElemNeedsEscapeDup predicate.
			// Routing it through dupHeapFieldForEscape too (its T1176 array branch,
			// reached because the escape context set dupContainerFieldAccess) would
			// clone TWICE — the first clone is then orphaned and leaks its elements.
			// The flag stays unconsumed for arrays but every escape context clears it
			// immediately after genExpr, so it cannot leak into later codegen. The
			// Optional[string]/Optional[container] shapes below are exclusive to this
			// path (dupBorrowedHeapUserPayload only covers Optional/Array of heap-user).
			if isVariantPayloadBorrowShape(identType) {
				// T1178/T1173: a fixed Array (heap-user, string, or container
				// elements) variant payload is deep-cloned at the escape SINK by
				// dupBorrowedHeapUserPayload (return/store/consuming-arg/constructor
				// -field — the T1171/T1173 path). Letting dupHeapFieldForEscape's
				// array branch (gated on dupContainerFieldAccess which the sink's
				// setDupFlagsForFieldAccess also sets for arrays) ALSO clone it here
				// produces a second element-wise clone whose elements are never
				// dropped → leak. The two paths must be mutually exclusive: skip the
				// read-side dup for the array shape (dupBorrowedHeapUserPayload owns
				// it). Other variant-payload shapes (Optional[string],
				// Optional[container]) are NOT covered by dupBorrowedHeapUserPayload
				// and still need the read-side dup.
				if _, _, isArr := c.arrayElemNeedsEscapeDup(identType); !isArr {
					if dup, ok := c.dupHeapFieldForEscape(val, identType, true); ok {
						return dup
					}
				}
			}
		}
		return val
	}
	// Module-level getter accessed without prefix (same file or glob import):
	// call the function with no args.
	if fn, ok := c.funcs[e.Name]; ok {
		if obj := c.lookupFunc(e.Name); obj != nil && obj.IsGetter() {
			result := c.block.NewCall(fn)
			// T0137/T1272/T1321: track every heap-owning result kind (string,
			// vector, channel, Arc/Weak/Mutex/Task, structural box, heap user
			// type) so a bare module getter used as a discarded temporary, a
			// temp method receiver, or a temp passed into a `~` (MutRef)
			// parameter is freed at statement end — mirroring the qualified
			// `mod.prop` path (genModuleGetterCall). Module getters take no
			// receiver and (Promise has no mutable globals) always construct
			// fresh owned values, so tracking is never an alias/double-free
			// hazard. The binding/assignment/move path claims the temp
			// (claimStringTemp / claimHeapTemp) so a single owner frees it
			// either way.
			retType := c.info.Types[e]
			if c.typeSubst != nil && retType != nil {
				retType = types.Substitute(retType, c.typeSubst)
			}
			if c.selfSubst != nil && retType != nil {
				retType = types.SubstituteSelf(retType, c.selfSubst.iface, c.selfSubst.concrete)
			}
			// T1240: a getter returning a function type yields an owned closure
			// whose heap env must be freed; track its env (field 1) as an env temp.
			if _, isSig := retType.(*types.Signature); isSig {
				c.trackEnvTemp(c.block.NewExtractValue(result, 1))
				return result
			}
			c.trackGetterResultByType(e, retType, result)
			return result
		}
		if _, isSig := c.info.Types[e].(*types.Signature); isSig {
			// Named function used as first-class value: generate a thunk with
			// the env-first ABI so it can be called through genIndirectCall.
			thunk := c.getOrCreateThunk(fn, e.Name)
			fnPtr := c.block.NewBitCast(thunk, irtypes.I8Ptr)
			var closure value.Value = constant.NewUndef(closureType())
			closure = c.block.NewInsertValue(closure, fnPtr, 0)
			closure = c.block.NewInsertValue(closure, constant.NewNull(irtypes.I8Ptr), 1)
			return closure
		}
		return fn
	}
	panic(fmt.Sprintf("codegen: undefined variable %q", e.Name))
}

func (c *Compiler) genStringMethodCall(e *ast.CallExpr, member *ast.MemberExpr, method string) (value.Value, bool) {
	// Factory methods (no receiver — target is a type name, not a value)
	if method == "from_bytes" {
		return c.genStringFromBytes(e), true
	}

	strPtr := c.genExprAutoPropagate(member.Target) // B0323

	switch method {
	case "trim":
		result := c.block.NewCall(c.funcs["promise_string_trim"], strPtr)
		return result, true

	case "split":
		argVal := c.genCallArgExpr(e.Args[0].Value)
		result := c.block.NewCall(c.funcs["promise_string_split"], strPtr, argVal)
		return result, true

	case "to_upper":
		result := c.block.NewCall(c.funcs["promise_string_to_upper"], strPtr)
		return result, true

	case "to_lower":
		result := c.block.NewCall(c.funcs["promise_string_to_lower"], strPtr)
		return result, true

	case "repeat":
		argVal := c.genCallArgExpr(e.Args[0].Value)
		result := c.block.NewCall(c.funcs["promise_string_repeat"], strPtr, argVal)
		return result, true

	case "bytes":
		return c.genStringBytes(strPtr), true

	case "byte_at":
		argVal := c.genCallArgExpr(e.Args[0].Value)
		return c.genStringByteAt(strPtr, argVal), true

	case "clone":
		return c.dupString(strPtr), true

	default:
		return nil, false
	}
}

// genStringFromBytes creates a string from a Vector[u8] (factory method).
// Reads the vector's count and data pointer, calls promise_string_new.
func (c *Compiler) genStringFromBytes(e *ast.CallExpr) value.Value {
	vecPtr := c.genCallArgExpr(e.Args[0].Value)
	// T0133: Don't clear drop flag — from_bytes borrows the vector data (copies bytes
	// into a new string via promise_string_new). The caller still owns the vector.

	// Vector layout: {i64 count, i64 capacity} header, then data at offset 16
	// Use loadVectorLen to mask off bit 63 (static vector flag, T0062/B0227).
	headerType := vectorHeaderType() // {i64, i64}
	hdrPtr := c.block.NewBitCast(vecPtr, irtypes.NewPointer(headerType))
	count := loadVectorLen(c.block, hdrPtr)

	// Data starts at offset vectorHeaderSize (16)
	dataPtr := c.block.NewGetElementPtr(irtypes.I8, vecPtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))

	return c.block.NewCall(c.funcs["promise_string_new"], dataPtr, count)
}

// genStringLen loads the length field from a string instance struct.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) genStringLen(e *ast.MemberExpr) value.Value {
	strPtr := c.genExprAutoPropagate(e.Target) // B0323
	instType := strInstanceType()
	typedPtr := c.block.NewBitCast(strPtr, irtypes.NewPointer(instType))
	return loadStringLen(c.block, typedPtr, instType)
}

// genStringIsLiteral checks the sign bit of the string length field.
// Literal strings (in .rodata) have bit 63 set; heap strings do not.
func (c *Compiler) genStringIsLiteral(e *ast.MemberExpr) value.Value {
	strPtr := c.genExprAutoPropagate(e.Target) // B0323
	instType := strInstanceType()
	typedPtr := c.block.NewBitCast(strPtr, irtypes.NewPointer(instType))
	rawLen := loadStringLenRaw(c.block, typedPtr, instType)
	// Bit 63 set → literal
	bit63 := c.block.NewAnd(rawLen, constant.NewInt(irtypes.I64, math.MinInt64))
	return c.block.NewICmp(enum.IPredNE, bit63, constant.NewInt(irtypes.I64, 0))
}

// genStringBytes creates a Vector[u8] from the string's raw bytes.
// Allocates a new vector, memcpys string data into it, sets count = string len.
func (c *Compiler) genStringBytes(strPtr value.Value) value.Value {
	instType := strInstanceType()
	typedPtr := c.block.NewBitCast(strPtr, irtypes.NewPointer(instType))

	// Load string length (masking off literal flag)
	strLen := loadStringLen(c.block, typedPtr, instType)

	// Get pointer to string data (field 2)
	dataPtr := c.block.NewGetElementPtr(instType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	// Allocate vector with capacity = strLen, elem_size = 1
	vec := c.block.NewCall(c.funcs["promise_vector_with_capacity"],
		strLen, constant.NewInt(irtypes.I64, 1))

	// Copy string data into vector data area (offset 16 = vectorHeaderSize)
	headerSizeConst := constant.NewInt(irtypes.I64, int64(vectorHeaderSize))
	vecDataPtr := c.block.NewGetElementPtr(irtypes.I8, vec, headerSizeConst)
	c.block.NewCall(c.funcs["llvm.memcpy"], vecDataPtr, dataPtr, strLen, constant.False)

	// Set vector count = strLen
	headerType := vectorHeaderType() // {i64, i64}
	hdrPtr := c.block.NewBitCast(vec, irtypes.NewPointer(headerType))
	countPtr := c.block.NewGetElementPtr(headerType, hdrPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewStore(strLen, countPtr)

	return vec
}

// genStringByteAt returns the raw byte at a given byte offset in the string.
// Unlike string[], this does NOT do UTF-8 decoding — it returns u8 directly.
func (c *Compiler) genStringByteAt(strPtr, index value.Value) value.Value {
	instType := strInstanceType()
	typedPtr := c.block.NewBitCast(strPtr, irtypes.NewPointer(instType))

	// Get pointer to string data
	dataPtr := c.block.NewGetElementPtr(instType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))

	// GEP to data[index], load byte
	bytePtr := c.block.NewGetElementPtr(irtypes.I8, dataPtr, index)
	return c.block.NewLoad(irtypes.I8, bytePtr)
}

// makeGlobalString creates a global null-terminated string constant and returns an i8* to it.
// fnv1aStr computes a 32-bit FNV-1a hash of a string for content-based naming.
func fnv1aStr(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// getCStrGlobal returns a deduplicated immutable global for a null-terminated
// C string. Content-based naming (.cstr.<hash>) makes these stable across
// compilations regardless of which mono instances are present.
func (c *Compiler) getCStrGlobal(s string) *ir.Global {
	global, ok := c.cstrGlobals[s]
	if !ok {
		data := constant.NewCharArrayFromString(s + "\x00")
		globalName := fmt.Sprintf(".cstr.%x", fnv1aStr(s))
		global = c.module.NewGlobalDef(globalName, data)
		global.Immutable = true
		global.Linkage = enum.LinkagePrivate
		c.cstrGlobals[s] = global
	}
	return global
}

func (c *Compiler) makeGlobalString(s string) value.Value {
	global := c.getCStrGlobal(s)
	return c.block.NewGetElementPtr(global.ContentType, global,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
}
