package codegen

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/types"
)

// genExpr generates LLVM IR for an expression and returns the resulting value.
func (c *Compiler) genExpr(expr ast.Expr) value.Value {
	if expr == nil {
		return nil
	}
	switch e := expr.(type) {
	case *ast.IntLit:
		return c.genIntLit(e)
	case *ast.FloatLit:
		return c.genFloatLit(e)
	case *ast.BoolLit:
		return c.genBoolLit(e)
	case *ast.StringLit:
		return c.genStringLit(e)
	case *ast.CharLit:
		return c.genCharLit(e)
	case *ast.IdentExpr:
		return c.genIdentExpr(e)
	case *ast.ParenExpr:
		return c.genExpr(e.Expr)
	case *ast.BinaryExpr:
		result := c.genBinaryExpr(e)
		// B0168: Track string concatenation temporaries. Only string + returns
		// i8* from genBinaryExpr; comparisons return i1.
		if result != nil && result.Type() == irtypes.I8Ptr {
			c.trackStringTemp(result)
		}
		return result
	case *ast.UnaryExpr:
		return c.genUnaryExpr(e)
	case *ast.CallExpr:
		result := c.genCallExpr(e)
		c.emitPanicCheck() // T0147: detect panic flag after every call expression
		// T0649: A borrow return (`T&`/`T~`) hands back a reference into
		// storage owned elsewhere — the ownership pass guarantees it outlives
		// the call. It is never an owned temp, so skip all post-call temp
		// tracking. Tracking it would record a drop for an allocation that
		// already has a real owner; combined with the binding-site borrow-flag
		// clear (isBorrowedExpr) the value would end up with no owner (leak).
		// Resolve the static result type before the I8Ptr split so this also
		// covers the heap-user `T~` branch (trackHeapUserTypeResult).
		rt := c.info.Types[e]
		if c.typeSubst != nil && rt != nil {
			rt = types.Substitute(rt, c.typeSubst)
		}
		if c.selfSubst != nil && rt != nil {
			rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
		}
		if rt != nil && isRefType(rt) {
			return result
		}
		// T0073: Track known-safe string-producing calls (primitive to_string, string methods)
		// T0109: Also track vector-producing calls (e.g., split()) for cleanup.
		// T0555: Track native handle (Arc/Weak/Mutex/Task) constructor/call results
		// for cleanup at statement end — without this, expressions like
		// `take_arc(Arc[int](99))` leak because the param is borrowed and the
		// caller has no temp tracking.
		if result != nil && result.Type() == irtypes.I8Ptr {
			if rt != nil {
				named := extractNamed(rt)
				if named == types.TypString {
					if c.isTrackedStringCall(e) {
						c.trackStringTemp(result)
					}
				} else if named == types.TypVector {
					// T0109: Pass element type so string elements get dropped.
					if elemType, ok := types.AsVector(rt); ok {
						c.trackVectorTempWithElemType(result, elemType)
					} else {
						c.trackVectorTemp(result)
					}
				} else if arcElem, isArc := types.AsArc(rt); isArc {
					c.trackTempWithDrop(result, c.getOrCreateArcDrop(arcElem))
				} else if weakElem, isWeak := types.AsWeak(rt); isWeak {
					c.trackTempWithDrop(result, c.getOrCreateWeakDrop(weakElem))
				} else if mutexElem, isMutex := types.AsMutex(rt); isMutex {
					c.trackTempWithDrop(result, c.getOrCreateMutexDrop(mutexElem))
				} else if taskElem, isTask := types.AsTask(rt); isTask {
					c.trackTempWithDrop(result, c.getOrCreateTaskDrop(taskElem))
				} else if _, isMG := types.AsMutexGuard(rt); isMG {
					// T0561: MutexGuard.drop is a single non-per-element-type symbol.
					if dropFn, ok := c.funcs["MutexGuard.drop"]; ok {
						c.trackTempWithDrop(result, dropFn)
					}
				} else if chElem, isCh := types.AsChannel(rt); isCh || named == types.TypChannel {
					// T0653: Channel[T] call/constructor result is a heap-allocated
					// channel struct + ring buffer + mutex + cond. Without tracking,
					// a discarded statement-expression temporary (e.g. `Channel[int](1);`,
					// `fresh();`, `fresh().send(9);`) leaks ~5 allocations because the
					// existing field-dup (B0219), element-dup (T0383/T0648), and
					// getter-result (T0486) trackers don't cover the call-result path.
					// T0663: per-element-type drop also walks any un-received buffered items.
					c.trackChannelTempWithElemType(result, chElem)
				}
			}
		} else {
			c.trackHeapUserTypeResult(e, result)
		}
		return result
	case *ast.MemberExpr:
		return c.genMemberExpr(e)
	case *ast.ThisExpr:
		return c.genThisExpr()
	case *ast.IfExpr:
		return c.genIfExpr(e)
	case *ast.MatchExpr:
		return c.genMatchExpr(e)
	case *ast.ErrorPropagateExpr:
		result := c.genErrorPropagateExpr(e)
		// B0260: Track string temps from error propagation paths.
		// When func()? returns a string, the propagated ok-path i8* is a
		// heap-allocated temp that must be freed at statement end if not
		// claimed (e.g., by push which dups the string). Without this,
		// synthesized serializable decode methods leak decoded strings.
		// T0350: Same gap for Vector results — i8* falls through with no tracking.
		if result != nil && result.Type() == irtypes.I8Ptr {
			exprType := c.info.Types[e]
			if c.typeSubst != nil && exprType != nil {
				exprType = types.Substitute(exprType, c.typeSubst)
			}
			named := extractNamed(exprType)
			if named == types.TypString {
				if c.optionalFieldString {
					c.optionalFieldString = false
				} else {
					c.trackStringTemp(result)
				}
			} else if named == types.TypVector {
				if elemType, ok := types.AsVector(exprType); ok {
					c.trackVectorTempWithElemType(result, elemType)
				} else {
					c.trackVectorTemp(result)
				}
			}
		} else {
			c.trackHeapUserTypeResult(e, result)
		}
		return result
	case *ast.ErrorPanicExpr:
		result := c.genErrorPanicExpr(e)
		// T0125: Track string temps from failable call panic paths.
		// When func()?! returns a string, the unwrapped i8* is a heap-allocated
		// temp that must be freed at statement end if not claimed by a variable.
		// T0350: Same gap for Vector results — i8* falls through with no tracking.
		if result != nil && result.Type() == irtypes.I8Ptr {
			exprType := c.info.Types[e]
			if c.typeSubst != nil && exprType != nil {
				exprType = types.Substitute(exprType, c.typeSubst)
			}
			named := extractNamed(exprType)
			if named == types.TypString {
				if c.optionalFieldString {
					c.optionalFieldString = false
				} else {
					c.trackStringTemp(result)
				}
			} else if named == types.TypVector {
				if elemType, ok := types.AsVector(exprType); ok {
					c.trackVectorTempWithElemType(result, elemType)
				} else {
					c.trackVectorTemp(result)
				}
			}
		} else {
			c.trackHeapUserTypeResult(e, result)
		}
		return result
	case *ast.AutoCloneExpr:
		result := c.genAutoCloneExpr(e)
		// T0605: the cloned value is a fresh owned heap allocation. Mirror the
		// synth clone()-CallExpr result temp-tracking so the enclosing
		// Self(...) constructor claims ownership (no leak; no double-drop with
		// the owner's synth drop of the field).
		if result != nil && result.Type() == irtypes.I8Ptr {
			rt := c.info.Types[e]
			if c.typeSubst != nil && rt != nil {
				rt = types.Substitute(rt, c.typeSubst)
			}
			if c.selfSubst != nil && rt != nil {
				rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
			}
			named := extractNamed(rt)
			if named == types.TypString {
				c.trackStringTemp(result)
			} else if named == types.TypVector {
				if elemType, ok := types.AsVector(rt); ok {
					c.trackVectorTempWithElemType(result, elemType)
				} else {
					c.trackVectorTemp(result)
				}
			}
		} else {
			c.trackHeapUserTypeResult(e, result)
		}
		return result
	case *ast.OptionalUnwrapExpr:
		result := c.genOptionalForceUnwrap(e.Expr)
		// T0125: Track string temps from optional unwrap paths.
		// B0190: Skip tracking when the unwrapped string comes from a field on a
		// droppable type (signaled by optionalFieldString). The owner's drop
		// handles the string's lifetime.
		// T0350: Same gap for Vector results — i8* falls through with no tracking.
		if result != nil && result.Type() == irtypes.I8Ptr {
			exprType := c.info.Types[e]
			if c.typeSubst != nil && exprType != nil {
				exprType = types.Substitute(exprType, c.typeSubst)
			}
			named := extractNamed(exprType)
			// B0287: For optional unwrap on ident source, the optional's
			// drop binding owns the inner. Don't track as a statement temp —
			// that would cause a double-free at scope exit.
			_, isIdentSource := e.Expr.(*ast.IdentExpr)
			if named == types.TypString {
				if c.optionalFieldString {
					c.optionalFieldString = false
				} else if !isIdentSource {
					c.trackStringTemp(result)
				}
			} else if named == types.TypVector {
				if c.optionalFieldVector {
					c.optionalFieldVector = false
				} else if !isIdentSource {
					if elemType, ok := types.AsVector(exprType); ok {
						c.trackVectorTempWithElemType(result, elemType)
					} else {
						c.trackVectorTemp(result)
					}
				}
			}
		} else {
			c.trackHeapUserTypeResult(e, result)
		}
		return result
	case *ast.ErrorHandlerExpr:
		result := c.genErrorHandlerExpr(e)
		// B0185: Track string temps from error handler expressions.
		// The result may be a phi merge of the Ok value and handler recovery value.
		// If it's an i8* (string), it needs tracking for cleanup at statement end.
		// T0350: Make tracking type-aware. Previously this branch unconditionally
		// called trackStringTemp for any i8* — for Vector[T] results this only
		// happened to free the buffer (because Vector and string share the bit-63
		// literal flag and pal_free path) but never dropped element strings.
		if result != nil && result.Type() == irtypes.I8Ptr {
			exprType := c.info.Types[e]
			if c.typeSubst != nil && exprType != nil {
				exprType = types.Substitute(exprType, c.typeSubst)
			}
			named := extractNamed(exprType)
			if named == types.TypString {
				c.trackStringTemp(result)
			} else if named == types.TypVector {
				if elemType, ok := types.AsVector(exprType); ok {
					c.trackVectorTempWithElemType(result, elemType)
				} else {
					c.trackVectorTemp(result)
				}
			}
		} else {
			c.trackHeapUserTypeResult(e, result)
		}
		return result
	case *ast.TupleLit:
		return c.genTupleLit(e)
	case *ast.NoneLit:
		return c.genNoneLit(e)
	case *ast.ArrayLit:
		return c.genArrayLit(e)
	case *ast.MapLit:
		return c.genMapLit(e)
	case *ast.IndexExpr:
		return c.genIndexExpr(e)
	case *ast.SliceExpr:
		result := c.genSliceExpr(e)
		// T0133: Track string slice results as temps. String slicing allocates a
		// new heap string (via native [:] method). Without tracking, the slice
		// result leaks when used as an intermediate in concatenation or comparison.
		if result != nil && result.Type() == irtypes.I8Ptr {
			rt := c.info.Types[e]
			if c.typeSubst != nil && rt != nil {
				rt = types.Substitute(rt, c.typeSubst)
			}
			if rt != nil && extractNamed(rt) == types.TypString {
				c.trackStringTemp(result)
			} else if rt != nil && extractNamed(rt) == types.TypVector {
				// B0223: Track vector slice results as heap temps. Vector slicing
				// allocates a new heap vector. Without tracking, the slice result
				// leaks when used as an intermediate (e.g., in string.from_bytes).
				// T0369: Pass the element type so transient cleanup walks droppable
				// elements. After T0376, Vector.[:]'s push path deep-clones non-
				// string heap elements (via the IndexExpr dup gate in
				// genVectorMethodCall), so the slice owns independent copies and
				// the walk is unconditionally safe. T0371 made the walk safe for
				// tuple element types as well — genTupleLit now claims
				// heap-tracked field temps, so the buffer-walk is the unique
				// drop site. T0387 removed the polymorphic carve-out: dupHeapValue
				// now dispatches through typeinfo.clone_fn_ptr for polymorphic
				// types so the slice owns independent concrete subtype copies.
				if elemType, ok := types.AsVector(rt); ok {
					c.trackVectorHeapTempWithElemType(result, elemType)
				}
			}
		}
		return result
	case *ast.SliceTypeExpr:
		// Type expression in expression position; only used as constructor callee.
		// genCallExpr handles this via c.info.Types lookup, not genExpr.
		return nil
	case *ast.LambdaExpr:
		return c.genLambdaExpr(e)
	case *ast.OptionalChainExpr:
		return c.genOptionalChainExpr(e)
	case *ast.UnsafeExpr:
		c.genBlock(e.Body)
		return nil
	case *ast.IsExpr:
		return c.genIsExpr(e)
	case *ast.CastExpr:
		return c.genCastExpr(e)
	case *ast.GoExpr:
		result := c.genGoExpr(e)
		// T0555: Track awaitable Task[T] results from `go expr` so the G
		// struct + result buffer are freed at statement end if not bound
		// to a local. Fire-and-forget go (statement-level discard) is
		// freed by goroutine_exit — tracking would double-free.
		if !c.goExprFireAndForget && result != nil && result.Type() == irtypes.I8Ptr {
			rt := c.info.Types[e]
			if c.typeSubst != nil && rt != nil {
				rt = types.Substitute(rt, c.typeSubst)
			}
			if c.selfSubst != nil && rt != nil {
				rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
			}
			if taskElem, isTask := types.AsTask(rt); isTask {
				c.trackTempWithDrop(result, c.getOrCreateTaskDrop(taskElem))
			}
		}
		return result
	default:
		panic(fmt.Sprintf("codegen: unhandled expression type %T", expr))
	}
}

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
	val, err := strconv.ParseInt(raw, 0, 64)
	if err != nil {
		// Try unsigned parse for large values
		uval, _ := strconv.ParseUint(raw, 0, 64)
		return constant.NewInt(intType, int64(uval))
	}
	return constant.NewInt(intType, val)
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
			// Evaluate expression and convert to string
			val := c.genExpr(p.Expr)
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
			variantPtr := c.loadVariantPtr(originalVal)
			typeinfoStruct := irtypes.NewStruct(irtypes.I8Ptr)
			typeinfoPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoStruct))
			vtableFieldPtr := c.block.NewGetElementPtr(typeinfoStruct, typeinfoPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			vtableRaw = c.block.NewLoad(irtypes.I8Ptr, vtableFieldPtr)
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
		return c.block.NewLoad(alloca.ElemType, alloca)
	}
	// Module-level getter accessed without prefix (same file or glob import):
	// call the function with no args.
	if fn, ok := c.funcs[e.Name]; ok {
		if obj := c.lookupFunc(e.Name); obj != nil && obj.IsGetter() {
			result := c.block.NewCall(fn)
			// T0137: Track getter results as string temps so they're cleaned up
			// when used as temporaries (not assigned to a variable).
			if typ := c.info.Types[e]; typ != nil && extractNamed(typ) == types.TypString {
				c.trackStringTemp(result)
			}
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

// --- Binary expressions ---

func (c *Compiler) genBinaryExpr(e *ast.BinaryExpr) value.Value {
	// Short-circuit and special operators at the AST level
	switch e.Op {
	case ast.BinAnd:
		return c.genShortCircuitAnd(e)
	case ast.BinOr:
		return c.genShortCircuitOr(e)
	case ast.BinElvis:
		return c.genElvis(e)
	case ast.BinExclusiveRange, ast.BinInclusiveRange:
		return c.genRange(e)
	}

	// Type-system-driven path
	left := c.genExprAutoPropagate(e.Left)
	right := c.genExprAutoPropagate(e.Right)

	leftType := c.info.Types[e.Left]
	if c.typeSubst != nil {
		leftType = types.Substitute(leftType, c.typeSubst)
	}
	if c.selfSubst != nil {
		leftType = types.SubstituteSelf(leftType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	named := extractNamed(leftType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot resolve Named type from %s for operator %s", leftType, e.Op))
	}

	op := e.Op.String()
	method := named.LookupMethod(op)
	if method == nil {
		panic(fmt.Sprintf("codegen: no method %q on type %s", op, named))
	}

	if method.IsNative() {
		// String operators dispatch to runtime intrinsics
		if named == types.TypString {
			return c.genStringOp(op, left, right)
		}
		return c.emitNativeOp(named, op, left, right)
	}

	// Non-native operator: dispatch as a method call.
	// Virtual dispatch when the type has a vtable (abstract/structural type or type with children).
	if c.needsVtable(named) {
		return c.genVirtualBinaryOp(e, named, method, left, right)
	}

	// Direct dispatch: call the concrete type's operator method.
	// Use resolveTypeName to get mono name for generic instances (e.g., "Pair[int]").
	ownerName := c.resolveMethodOwner(named, op)
	var mangledName string
	if ownerName != named.Obj().Name() {
		// Operator inherited from a parent. If the parent is structural, the method
		// was synthesized under the concrete type's name — use that, not the parent's.
		// (Mirrors the same logic in genMethodCall for structural inheritance.)
		if structParent := c.findStructuralOwner(named, op); structParent != nil {
			concreteName := c.resolveTypeName(leftType)
			c.ensureDefaultMethodsSynthesized(named, structParent)
			mangledName = mangleMethodName(concreteName, op, false)
		} else {
			monoOwner := c.resolveMonoParentName(named, leftType, ownerName)
			mangledName = mangleMethodName(monoOwner, op, false)
		}
	} else {
		mangledName = mangleMethodName(c.resolveTypeName(leftType), op, false)
	}
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared operator method %s", mangledName))
	}

	var args []value.Value
	if method.Sig().Recv() != nil {
		if _, isThis := e.Left.(*ast.ThisExpr); isThis {
			args = append(args, left)
		} else if named.IsValueType() {
			args = append(args, c.valueTypeReceiverPtr(left, leftType))
		} else {
			args = append(args, c.extractInstancePtr(left))
		}
	}
	// If right came from genThisExpr() (returns i8* receiver ptr) but the method expects a
	// value struct, wrap it as {null_vtable, instance_ptr}. This happens in synthesized default
	// method bodies like Priority.> containing "other < this", where 'this' appears as an
	// argument rather than the receiver.
	if _, isThis := e.Right.(*ast.ThisExpr); isThis {
		var paramIdx int
		if method.Sig().Recv() != nil {
			paramIdx = 1
		}
		if paramIdx < len(fn.Params) {
			if st, ok := fn.Params[paramIdx].Typ.(*irtypes.StructType); ok {
				if _, rightIsPtr := right.Type().(*irtypes.PointerType); rightIsPtr {
					alloca := c.createEntryAlloca(st)
					vtableField := c.block.NewGetElementPtr(st, alloca,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
					c.block.NewStore(constant.NewNull(irtypes.I8Ptr), vtableField)
					instField := c.block.NewGetElementPtr(st, alloca,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
					c.block.NewStore(right, instField)
					right = c.block.NewLoad(st, alloca)
				}
			}
		}
	}
	args = append(args, right)
	return c.block.NewCall(fn, args...)
}

// genVirtualBinaryOp dispatches a non-native binary operator through the vtable.
// Used when the static type is abstract or has children requiring virtual dispatch.
// Mirrors genVirtualMethodCall but uses pre-evaluated left/right operands.
func (c *Compiler) genVirtualBinaryOp(e *ast.BinaryExpr, named *types.Named,
	method *types.Method, left, right value.Value) value.Value {

	op := e.Op.String()

	// Extract vtable and instance from left operand
	var vtableRaw, instance value.Value
	if _, isThis := e.Left.(*ast.ThisExpr); isThis {
		instance = left
		variantPtr := c.loadVariantPtr(left)
		typeinfoStruct := irtypes.NewStruct(irtypes.I8Ptr)
		typeinfoPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoStruct))
		vtableFieldPtr := c.block.NewGetElementPtr(typeinfoStruct, typeinfoPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		vtableRaw = c.block.NewLoad(irtypes.I8Ptr, vtableFieldPtr)
	} else {
		vtableRaw = c.extractVtablePtr(left)
		instance = c.extractInstancePtr(left)
	}

	// Index into vtable
	slotIndex := named.VirtualMethodIndex(op, false)
	if slotIndex < 0 {
		panic(fmt.Sprintf("codegen: operator %s not in vtable for %s", op, named))
	}
	vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
	fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
		constant.NewInt(irtypes.I32, int64(slotIndex)))
	fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

	// Build the function type and bitcast
	retType := irtypes.Type(irtypes.Void)
	if method.Sig().Result() != nil {
		retType = c.resolveType(method.Sig().Result())
	}
	if method.Sig().CanError() {
		retType = computeResultType(retType)
	}
	var paramTypes []irtypes.Type
	if method.Sig().Recv() != nil {
		paramTypes = append(paramTypes, irtypes.I8Ptr)
	}
	for _, p := range method.Sig().Params() {
		paramTypes = append(paramTypes, c.resolveType(p.Type()))
	}
	funcType := irtypes.NewFunc(retType, paramTypes...)
	fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))

	// Call with instance ptr + right operand
	var args []value.Value
	if method.Sig().Recv() != nil {
		args = append(args, instance)
	}
	args = append(args, right)
	return c.block.NewCall(fnTyped, args...)
}

// genStringOp dispatches a string binary operator to the appropriate runtime intrinsic.
func (c *Compiler) genStringOp(op string, left, right value.Value) value.Value {
	switch op {
	case "+":
		return c.block.NewCall(c.funcs["promise_string_concat"], left, right)
	case "==":
		return c.block.NewCall(c.funcs["promise_string_eq"], left, right)
	case "!=":
		eq := c.block.NewCall(c.funcs["promise_string_eq"], left, right)
		return c.block.NewXor(eq, constant.NewInt(irtypes.I1, 1))
	case "<":
		cmp := c.block.NewCall(c.funcs["promise_string_compare"], left, right)
		return c.block.NewICmp(enum.IPredSLT, cmp, constant.NewInt(irtypes.I32, 0))
	case ">":
		cmp := c.block.NewCall(c.funcs["promise_string_compare"], left, right)
		return c.block.NewICmp(enum.IPredSGT, cmp, constant.NewInt(irtypes.I32, 0))
	case "<=":
		cmp := c.block.NewCall(c.funcs["promise_string_compare"], left, right)
		return c.block.NewICmp(enum.IPredSLE, cmp, constant.NewInt(irtypes.I32, 0))
	case ">=":
		cmp := c.block.NewCall(c.funcs["promise_string_compare"], left, right)
		return c.block.NewICmp(enum.IPredSGE, cmp, constant.NewInt(irtypes.I32, 0))
	default:
		panic(fmt.Sprintf("codegen: string operator %q not yet implemented", op))
	}
}

// --- Unary expressions ---

func (c *Compiler) genUnaryExpr(e *ast.UnaryExpr) value.Value {
	// Intercept receive operator (<-task) before normal unary dispatch
	if e.Op == ast.UnaryReceive {
		return c.genReceiveExpr(e)
	}

	operand := c.genExprAutoPropagate(e.Operand)
	operandType := c.info.Types[e.Operand]
	named := extractNamed(operandType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot resolve Named type from %s for unary %s", operandType, e.Op))
	}

	op := e.Op.String()

	// For unary ops, look up the 0-param method variant
	method := c.lookupUnaryMethod(named, op)
	if method == nil {
		panic(fmt.Sprintf("codegen: no unary method %q on type %s", op, named))
	}

	if method.IsNative() {
		return c.emitNativeOp(named, op, operand, nil)
	}

	panic(fmt.Sprintf("codegen: non-native unary %s.%s not yet implemented", named, op))
}

// lookupUnaryMethod finds the 0-param variant of a method by name.
func (c *Compiler) lookupUnaryMethod(named *types.Named, op string) *types.Method {
	for _, m := range named.Methods() {
		if m.Name() == op && len(m.Sig().Params()) == 0 {
			return m
		}
	}
	return nil
}

// --- Short-circuit boolean operators ---

func (c *Compiler) genShortCircuitAnd(e *ast.BinaryExpr) value.Value {
	left := c.genExprAutoPropagate(e.Left)
	startBlock := c.block

	rightBlock := c.newBlock("and.rhs")
	mergeBlock := c.newBlock("and.merge")

	c.block.NewCondBr(left, rightBlock, mergeBlock)

	c.block = rightBlock
	right := c.genExprAutoPropagate(e.Right)
	rightEnd := c.block
	c.block.NewBr(mergeBlock)

	c.block = mergeBlock
	phi := mergeBlock.NewPhi(
		&ir.Incoming{X: constant.NewInt(irtypes.I1, 0), Pred: startBlock},
		&ir.Incoming{X: right, Pred: rightEnd},
	)
	return phi
}

func (c *Compiler) genShortCircuitOr(e *ast.BinaryExpr) value.Value {
	left := c.genExprAutoPropagate(e.Left)
	startBlock := c.block

	rightBlock := c.newBlock("or.rhs")
	mergeBlock := c.newBlock("or.merge")

	c.block.NewCondBr(left, mergeBlock, rightBlock)

	c.block = rightBlock
	right := c.genExprAutoPropagate(e.Right)
	rightEnd := c.block
	c.block.NewBr(mergeBlock)

	c.block = mergeBlock
	phi := mergeBlock.NewPhi(
		&ir.Incoming{X: constant.NewInt(irtypes.I1, 1), Pred: startBlock},
		&ir.Incoming{X: right, Pred: rightEnd},
	)
	return phi
}

// --- range construction ---

// genRange constructs a Range[T] value type struct via insertvalue chain.
// Layout: { i8* _vtable, T start, T end, i1 inclusive }
func (c *Compiler) genRange(e *ast.BinaryExpr) value.Value {
	start := c.genExprAutoPropagate(e.Left)
	end := c.genExprAutoPropagate(e.Right)
	inclusive := constant.NewInt(irtypes.I1, 0)
	if e.Op == ast.BinInclusiveRange {
		inclusive = constant.NewInt(irtypes.I1, 1)
	}

	// Look up the mono value type layout for Range[T]
	resultType := c.info.Types[e]
	if c.typeSubst != nil {
		resultType = types.Substitute(resultType, c.typeSubst)
	}
	layout := c.lookupTypeLayout(resultType)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for range type %s", resultType))
	}
	valueStructType := layout.Value.LLVMType

	// Build value struct via insertvalue
	var val value.Value = constant.NewUndef(valueStructType)
	val = c.block.NewInsertValue(val, constant.NewNull(irtypes.I8Ptr), 0)                     // vtable = null
	val = c.block.NewInsertValue(val, start, uint64(layout.ValueFieldIndex["start"]))         // start
	val = c.block.NewInsertValue(val, end, uint64(layout.ValueFieldIndex["end"]))             // end
	val = c.block.NewInsertValue(val, inclusive, uint64(layout.ValueFieldIndex["inclusive"])) // inclusive
	return val
}

// --- Call expressions ---

func (c *Compiler) genCallExpr(e *ast.CallExpr) value.Value {
	// Handle super() calls in constructor bodies
	if ident, ok := e.Callee.(*ast.IdentExpr); ok && ident.Name == "super" {
		return c.genSuperCall(e)
	}

	// Method call or enum variant constructor: callee is MemberExpr
	if member, ok := e.Callee.(*ast.MemberExpr); ok {
		// Handle mod.func() / mod.Type() — qualified call to imported module
		if ident, ok := member.Target.(*ast.IdentExpr); ok {
			if modName := c.resolveModuleName(ident); modName != "" {
				calleeType := c.info.Types[e.Callee]
				switch calleeType.(type) {
				case *types.Named, *types.Instance:
					// Module-qualified constructor: mod.Type(args)
					return c.genConstructorCallMono(e, calleeType)
				case *types.Enum:
					// Module-qualified enum — fall through to enum dispatch below
				default:
					// Module-qualified function call: mod.func(args)
					// Module-qualified generic call with inferred type args: mod.func(args)
					if inferred, ok := c.info.InferredTypeArgs[e]; ok {
						return c.genInferredGenericCall(e, inferred)
					}
					return c.genModuleCall(e, modName, member.Field)
				}
			}
		}

		targetType := c.info.Types[member.Target]
		// Apply typeSubst for mono context
		if c.typeSubst != nil {
			targetType = types.Substitute(targetType, c.typeSubst)
		}
		if enumLayout := c.lookupEnumLayout(targetType); enumLayout != nil {
			if _, isVariant := enumLayout.VariantTag[member.Field]; isVariant {
				return c.genEnumVariantCallLayout(e, member, enumLayout)
			}
			// Not a variant — fall through to method dispatch
		}
		// Fallback for generic enum variant constructors in mono context:
		// target is bare *types.Enum; use the call result type (Instance after subst).
		if _, ok := targetType.(*types.Enum); ok {
			resultType := c.info.Types[e]
			if c.typeSubst != nil {
				resultType = types.Substitute(resultType, c.typeSubst)
			}
			if enumLayout := c.lookupEnumLayout(resultType); enumLayout != nil {
				if _, isVariant := enumLayout.VariantTag[member.Field]; isVariant {
					return c.genEnumVariantCallLayout(e, member, enumLayout)
				}
			}
		}
		// Function-typed field call: this._next() where _next is a () -> T? field.
		// Check if the member name is a field (not a method) on the target type,
		// and the field type is a Signature — treat as indirect call through the field.
		if sig, ok := c.info.Types[e.Callee].(*types.Signature); ok {
			memberTargetType := c.info.Types[member.Target]
			if c.typeSubst != nil {
				memberTargetType = types.Substitute(memberTargetType, c.typeSubst)
			}
			if c.selfSubst != nil {
				memberTargetType = types.SubstituteSelf(memberTargetType, c.selfSubst.iface, c.selfSubst.concrete)
			}
			if named := extractNamed(memberTargetType); named != nil {
				if named.LookupField(member.Field) != nil {
					closure := c.genExpr(e.Callee) // genMemberExpr loads the field
					var argVals []value.Value
					for _, arg := range e.Args {
						argVals = append(argVals, c.genCallArgExpr(arg.Value))
					}
					result := c.genIndirectCall(closure, sig, argVals)
					c.emitReturnAliasCheck(result, sig, e.Args, argVals) // T0331
					return result
				}
			}
		}
		// T0642: Inferred method-level type args. Sema recorded the inferred type
		// args; route through the generic method path which builds the mono name
		// + subst. Mirror the IndexExpr-path post-call handling — generic
		// structural default methods are compiled without selfSubst, so the
		// iterator's _parent isn't set; don't claim the receiver (B0213).
		if inferred, ok := c.info.InferredTypeArgs[e]; ok {
			savedReceiverClaim := c.pendingReceiverClaim
			c.pendingReceiverClaim = nil
			result := c.genGenericMethodCall(e, member, inferred.TypeArgs)
			heapBeforeTrack := len(c.heapTemps)
			c.maybeTrackIterTemp(e, result)
			if len(c.heapTemps) > heapBeforeTrack {
				c.claimAllEnvTemps()
			}
			c.pendingReceiverClaim = savedReceiverClaim
			return result
		}
		savedReceiverClaim := c.pendingReceiverClaim // T0130: save across nested calls
		c.pendingReceiverClaim = nil
		result := c.genMethodCall(e, member)
		heapBeforeTrack := len(c.heapTemps) // B0213: capture before maybeTrackIterTemp
		c.maybeTrackIterTemp(e, result)
		// T0130: Claim the receiver's heap temp ONLY when the method returns a
		// structural interface (combinator like filter/take/skip). Terminal operations
		// (count, collect, find) return non-structural types — their receiver should
		// be freed at statement end, not claimed.
		if c.pendingReceiverClaim != nil {
			callResultType := c.info.Types[e]
			if c.typeSubst != nil {
				callResultType = types.Substitute(callResultType, c.typeSubst)
			}
			if resultNamed := extractNamed(callResultType); resultNamed != nil && resultNamed.IsStructural() {
				c.claimHeapTemp(c.pendingReceiverClaim)
			}
		}
		// T0100/B0213: Claim env temps only when THIS call's result was tracked
		// as a new heapTemp (combinator that stores the lambda in the returned
		// iterator). Don't claim when the receiver evaluation created heapTemps —
		// terminal operations (for_each, any, fold) don't store lambdas.
		if len(c.heapTemps) > heapBeforeTrack {
			c.claimAllEnvTemps()
		}
		c.pendingReceiverClaim = savedReceiverClaim // T0130: restore
		return result
	}

	// Constructor call: callee resolves to a Named type or Instance
	calleeType := c.info.Types[e.Callee]
	if c.typeSubst != nil {
		calleeType = types.Substitute(calleeType, c.typeSubst)
	}
	if inst, ok := calleeType.(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Named); ok {
			// Vector capacity constructor: T[](capacity: n)
			if origin == types.TypVector {
				return c.genVectorCapacityConstructor(e, inst)
			}
			// Channel constructor: channel[T](capacity: n) or channel[T]()
			if origin == types.TypChannel {
				return c.genChannelConstructor(e, inst)
			}
			// Arc constructor: Arc[T](value)
			if origin == types.TypArc {
				return c.genArcConstructor(e, inst)
			}
			// Mutex constructor: Mutex[T](value)
			if origin == types.TypMutex {
				return c.genMutexConstructor(e, inst)
			}
			return c.genConstructorCallMono(e, calleeType)
		}
	}
	if named, ok := calleeType.(*types.Named); ok {
		if _, isIdent := e.Callee.(*ast.IdentExpr); isIdent {
			return c.genConstructorCallMono(e, named)
		}
	}

	// Generic function/method call: callee is IndexExpr (identity[int](42) or obj.method[int](42))
	if idx, ok := e.Callee.(*ast.IndexExpr); ok {
		if member, ok := idx.Target.(*ast.MemberExpr); ok {
			// Check if this is a module-qualified generic function call (json.encode_string[Config](...))
			// vs. an instance generic method call (box.transform[string](...))
			if ident, ok := member.Target.(*ast.IdentExpr); ok {
				if c.resolveModuleName(ident) != "" {
					return c.genModuleGenericFuncCall(e, idx, member.Field)
				}
			}
			savedReceiverClaim2 := c.pendingReceiverClaim // T0130
			c.pendingReceiverClaim = nil
			typeArgExprs := append([]ast.Expr{idx.Index}, idx.ExtraIndices...)
			typeArgs := make([]types.Type, len(typeArgExprs))
			for i, expr := range typeArgExprs {
				typeArgs[i] = c.info.Types[expr]
			}
			result := c.genGenericMethodCall(e, member, typeArgs)
			heapBeforeTrack2 := len(c.heapTemps) // B0213: capture before maybeTrackIterTemp
			c.maybeTrackIterTemp(e, result)
			// B0213: Don't claim receiver for generic method calls. Generic structural
			// default methods (map[R], zip[U], flat_map[R]) are compiled without selfSubst,
			// so _parent is not set on the returned _FnIter. The receiver stays as an
			// unclaimed heapTemp and is cleaned independently at statement end.
			// B0213: Only claim env temps when THIS call's result was tracked as a new
			// heapTemp (not when the receiver evaluation created heapTemps).
			if len(c.heapTemps) > heapBeforeTrack2 {
				c.claimAllEnvTemps()
			}
			c.pendingReceiverClaim = savedReceiverClaim2 // T0130
			return result
		}
		return c.genGenericFuncCall(e, idx)
	}

	// Inferred generic function call: sema recorded the inferred type args.
	if inferred, ok := c.info.InferredTypeArgs[e]; ok {
		savedReceiverClaim3 := c.pendingReceiverClaim // T0130
		c.pendingReceiverClaim = nil
		result := c.genInferredGenericCall(e, inferred)
		heapBeforeTrack3 := len(c.heapTemps) // B0213: capture before maybeTrackIterTemp
		c.maybeTrackIterTemp(e, result)
		// B0213: Don't claim receiver for inferred generic calls (same rationale as
		// genGenericMethodCall — _parent not set for generic structural defaults).
		// Only claim env temps when THIS call's result was tracked.
		if len(c.heapTemps) > heapBeforeTrack3 {
			c.claimAllEnvTemps()
		}
		c.pendingReceiverClaim = savedReceiverClaim3 // T0130
		return result
	}

	// Resolve callee first to detect MutRef params (B0149)
	ident, ok := e.Callee.(*ast.IdentExpr)
	if !ok {
		panic(fmt.Sprintf("codegen: unsupported callee type %T", e.Callee))
	}

	// Look up callee signature for MutRef param detection.
	// Extern functions use C ABI — skip MutRef pointer-passing for them.
	var calleeSig *types.Signature
	isExtern := false
	if _, ok := c.externs[ident.Name]; ok {
		isExtern = true
	}
	if !isExtern {
		if callee := c.lookupFunc(ident.Name); callee != nil {
			calleeSig, _ = callee.Type().(*types.Signature)
		}
	}

	// Evaluate arguments — pass address for MutRef params (B0149)
	var argVals []value.Value
	var argTypes []types.Type
	var variadicPTs []variadicPassthrough // B0203
	if calleeSig != nil {
		argVals, argTypes, variadicPTs = c.genCallArgsWithMutRef(e.Args, calleeSig.Params())
	} else {
		for _, arg := range e.Args {
			argVals = append(argVals, c.genCallArgExpr(arg.Value))
			argTypes = append(argTypes, c.info.Types[arg.Value])
		}
	}

	// Lambda call: callee is a local variable holding a fat pointer {i8*, i8*}
	if alloca, ok := c.locals[ident.Name]; ok {
		calleeType := c.info.Types[e.Callee]
		if sig, ok := calleeType.(*types.Signature); ok {
			closure := c.block.NewLoad(alloca.ElemType, alloca)
			origArgVals := argVals // T0331: pre-coercion for alias check
			result := c.genIndirectCall(closure, sig, argVals)
			c.clearVariadicStaticFlags(variadicPTs)
			c.emitReturnAliasCheck(result, sig, e.Args, origArgVals) // T0331
			return result
		}
	}

	// Extern function — pack args into value structs, call, unpack return
	if isExtern {
		ext := c.externs[ident.Name]
		// Intercept netpoll wait operations — emit inline coro.suspend (T0232)
		// The PollDesc pointer is stored as a Promise int. argVals[0] may be
		// a raw i64 (field access) or a value struct {i8*, T_i*, i64} (local var).
		if ext.CName == "promise_netpoll_wait_read" {
			c.genNetpollWaitRead(c.extractI64FromIntArg(argVals[0]))
			c.clearVariadicStaticFlags(variadicPTs)
			return nil
		}
		if ext.CName == "promise_netpoll_wait_write" {
			c.genNetpollWaitWrite(c.extractI64FromIntArg(argVals[0]))
			c.clearVariadicStaticFlags(variadicPTs)
			return nil
		}
		result := c.genExternCall(ext, argVals, argTypes)
		c.clearVariadicStaticFlags(variadicPTs)
		return result
	}

	// Regular function call
	fn, ok := c.funcs[ident.Name]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined function %q", ident.Name))
	}

	// Coerce arguments when crossing type boundaries
	origArgVals := argVals // B0345: save pre-coercion values for alias check
	if calleeSig != nil {
		argVals = c.coerceCallArgs(argVals, argTypes, calleeSig.Params(), e.Args, nil)
	}

	result := c.block.NewCall(fn, argVals...)
	c.clearVariadicStaticFlags(variadicPTs)

	// B0345: If the return value aliases an argument, clear the argument's drop flag
	// to prevent double-free. E.g., identity(v) returns v's pointer — without this,
	// both v and the return value would be dropped at scope exit.
	c.emitReturnAliasCheck(result, calleeSig, e.Args, origArgVals)

	// T0092: Track string return values from functions with structural interface
	// parameters. When a function takes a structural interface param and returns
	// a string, the result is typically a new allocation (from format/encode/
	// to_string on the structural param). Track it so it's freed at statement end.
	// Note: if the function internally calls to_string() on a string-typed
	// structural param, the return aliases the input. This is safe for literals
	// (promise_string_drop is a no-op) and for encoder-style functions (return
	// is always a new allocation). A display(heap_string_var) pattern where the
	// return aliases the variable would require ownership tracking (T0061) for
	// full safety.
	if result != nil && result.Type() == irtypes.I8Ptr && c.tempTrackingEnabled {
		if calleeSig != nil && hasStructuralParam(calleeSig, c.typeSubst) {
			if rt := c.info.Types[e]; rt != nil && extractNamed(rt) == types.TypString {
				c.trackStringTemp(result)
			}
		}
	}

	return result
}

// resolveModuleName checks if an IdentExpr refers to a module and returns
// the module's IR prefix (derived from GlobalIdentity) for IR symbol lookup.
// Returns "" if the ident is not a module.
func (c *Compiler) resolveModuleName(ident *ast.IdentExpr) string {
	if obj, ok := c.info.Objects[ident]; ok {
		if mod, ok := obj.(*types.Module); ok {
			// Map the module's path to its IR prefix for stable IR identity
			if prefix, ok := c.moduleCanonical[mod.Path()]; ok {
				return prefix
			}
			// Catalog modules have empty Path(); use catalog name as IR prefix.
			// Catalog names are simple identifiers that pass through SanitizeIRPrefix
			// unchanged, so catalogName == IRPrefix. This handles aliased imports
			// like `use json as j;` where mod.Name() = "j" but IR prefix = "json".
			if catName := mod.CatalogName(); catName != "" {
				return catName
			}
			return mod.Name()
		}
	}
	return ""
}

// genModuleCall handles mod.func() calls — resolves func in the module's IR functions.
func (c *Compiler) genModuleCall(e *ast.CallExpr, moduleName, funcName string) value.Value {
	// Check if the callee is an extern (C ABI — skip MutRef pointer-passing)
	key := moduleName + "." + funcName
	isExtern := false
	if _, ok := c.moduleExterns[key]; ok {
		isExtern = true
	}

	// Look up callee signature for MutRef param detection (B0149)
	var calleeSig *types.Signature
	if !isExtern {
		if sig, ok := c.info.Types[e.Callee].(*types.Signature); ok {
			calleeSig = sig
		}
	}

	var argVals []value.Value
	var argTypes []types.Type
	var variadicPTs []variadicPassthrough // B0203
	if calleeSig != nil {
		argVals, argTypes, variadicPTs = c.genCallArgsWithMutRef(e.Args, calleeSig.Params())
	} else {
		for _, arg := range e.Args {
			argVals = append(argVals, c.genCallArgExpr(arg.Value))
			argTypes = append(argTypes, c.info.Types[arg.Value])
		}
	}

	// Try module extern first
	if ext, ok := c.moduleExterns[key]; ok {
		result := c.genExternCall(ext, argVals, argTypes)
		c.clearVariadicStaticFlags(variadicPTs)
		return result
	}

	// Try module function
	fn, ok := c.moduleFuncs[key]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined module function %s.%s", moduleName, funcName))
	}

	// Coerce arguments using the callee's signature from sema
	origArgVals := argVals // B0345
	if calleeSig != nil {
		argVals = c.coerceCallArgs(argVals, argTypes, calleeSig.Params(), e.Args, nil)
	}

	result := c.block.NewCall(fn, argVals...)
	c.clearVariadicStaticFlags(variadicPTs)
	c.emitReturnAliasCheck(result, calleeSig, e.Args, origArgVals) // B0345
	return result
}

// genModuleGetterCall handles mod.property access — calls the getter function with no args.
func (c *Compiler) genModuleGetterCall(e *ast.MemberExpr, moduleName, propName string) value.Value {
	key := moduleName + "." + propName
	fn, ok := c.moduleFuncs[key]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined module getter %s.%s", moduleName, propName))
	}
	result := c.block.NewCall(fn)
	// T0137: Track module getter results as string temps so they're cleaned up
	// when used as temporaries (not assigned to a variable).
	if typ := c.info.Types[e]; typ != nil && extractNamed(typ) == types.TypString {
		c.trackStringTemp(result)
	}
	return result
}

// genGenericCallArgs evaluates arguments for a monomorphic generic free/module
// function call. It substitutes the callee signature's params with the call's
// type-arg subst so genCallArgsWithMutRef sees concrete param types — without
// this, `~`/`&` generic-instance (and string/Vector) params are passed by
// value while the monomorphic callee expects a pointer, producing an ABI
// mismatch and a runtime segfault (T0639). When the signature can't be
// resolved it falls back to the plain by-value loop (old behavior). This makes
// the generic call paths use the same MutRef-aware arg generation +
// T0087/B0201 ownership transfer as the non-generic path (genCallExpr).
func (c *Compiler) genGenericCallArgs(args []*ast.Arg, sig *types.Signature, subst map[*types.TypeParam]types.Type) ([]value.Value, []types.Type, []variadicPassthrough) {
	if sig == nil {
		var argVals []value.Value
		var argTypes []types.Type
		for _, arg := range args {
			argVals = append(argVals, c.genCallArgExpr(arg.Value))
			c.dupStringFieldAccess = false
			c.dupContainerFieldAccess = false
			c.dupHeapUserFieldAccess = false // T0403
			argTypes = append(argTypes, c.info.Types[arg.Value])
		}
		return argVals, argTypes, nil
	}
	params := sig.Params()
	if len(subst) > 0 {
		params = make([]*types.Param, len(sig.Params()))
		for i, p := range sig.Params() {
			np := types.NewParam(p.Name(), types.Substitute(p.Type(), subst), p.Ref())
			np.SetVariadic(p.IsVariadic())
			params[i] = np
		}
	}
	return c.genCallArgsWithMutRef(args, params)
}

// genGenericFuncCall generates a call to a monomorphic generic function instance.
func (c *Compiler) genGenericFuncCall(e *ast.CallExpr, idx *ast.IndexExpr) value.Value {
	// Resolve all type arguments to build the mangled name
	ident, ok := idx.Target.(*ast.IdentExpr)
	if !ok {
		panic(fmt.Sprintf("codegen: generic function target is not IdentExpr: %T", idx.Target))
	}

	allTypeArgExprs := append([]ast.Expr{idx.Index}, idx.ExtraIndices...)
	mangledName := ident.Name + "["
	for i, argExpr := range allTypeArgExprs {
		typeArgType := c.info.Types[argExpr]
		if c.typeSubst != nil && typeArgType != nil {
			typeArgType = types.Substitute(typeArgType, c.typeSubst)
		}
		if i > 0 {
			mangledName += ", "
		}
		mangledName += typeArgStr(typeArgType)
	}
	mangledName += "]"

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined monomorphic function %q", mangledName))
	}

	// T0639: Resolve the callee signature + per-call type-arg subst BEFORE arg
	// generation so genGenericCallArgs can pass `~`/`&` params by pointer with
	// concrete (substituted) param types. T0418: the subst also resolves
	// generic params like T? at the call site even when the outer mono context
	// (c.typeSubst) doesn't cover the callee's TypeParams.
	var calleeSig *types.Signature
	var callSubst map[*types.TypeParam]types.Type
	if callee := c.lookupFunc(ident.Name); callee != nil {
		if sig, sOk := callee.Type().(*types.Signature); sOk {
			calleeSig = sig
			callSubst = c.buildCallTypeArgSubst(sig.TypeParams(), allTypeArgExprs)
		}
	}
	if calleeSig == nil {
		if sig, sOk := c.info.Types[e.Callee].(*types.Signature); sOk {
			calleeSig = sig
		}
	}

	argVals, argTypes, variadicPTs := c.genGenericCallArgs(e.Args, calleeSig, callSubst)
	origArgVals := argVals // T0331: save pre-coercion values for alias check

	if calleeSig != nil {
		argVals = c.coerceCallArgs(argVals, argTypes, calleeSig.Params(), e.Args, callSubst)
	}

	result := c.block.NewCall(fn, argVals...)
	c.clearVariadicStaticFlags(variadicPTs)
	c.emitReturnAliasCheckSubst(result, calleeSig, e.Args, origArgVals, callSubst) // T0331/T0418
	return result
}

// genInferredGenericCall generates a call to a monomorphic generic function
// where the type arguments were inferred by sema (not explicit in the AST).
func (c *Compiler) genInferredGenericCall(e *ast.CallExpr, inferred *sema.InferredCall) value.Value {
	// Build mangled name from inferred type args.
	mangledName := inferred.FuncName + "["
	for i, ta := range inferred.TypeArgs {
		if c.typeSubst != nil {
			ta = types.Substitute(ta, c.typeSubst)
		}
		if i > 0 {
			mangledName += ", "
		}
		mangledName += typeArgStr(ta)
	}
	mangledName += "]"

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined inferred monomorphic function %q", mangledName))
	}

	// T0639: Resolve the callee signature + inferred-type-arg subst BEFORE arg
	// generation so genGenericCallArgs passes `~`/`&` params by pointer with
	// concrete (substituted) param types.
	var calleeSig *types.Signature
	var callSubst map[*types.TypeParam]types.Type
	if callee := c.lookupFunc(inferred.FuncName); callee != nil {
		if sig, sOk := callee.Type().(*types.Signature); sOk {
			calleeSig = sig
			callSubst = c.buildInferredCallSubst(sig.TypeParams(), inferred.TypeArgs)
		}
	}
	if calleeSig == nil {
		if sig, sOk := c.info.Types[e.Callee].(*types.Signature); sOk {
			calleeSig = sig
		}
	}

	argVals, argTypes, variadicPTs := c.genGenericCallArgs(e.Args, calleeSig, callSubst)
	origArgVals := argVals // T0331: save pre-coercion values for alias check

	if calleeSig != nil {
		argVals = c.coerceCallArgs(argVals, argTypes, calleeSig.Params(), e.Args, callSubst)
	}

	result := c.block.NewCall(fn, argVals...)
	c.clearVariadicStaticFlags(variadicPTs)
	c.emitReturnAliasCheckSubst(result, calleeSig, e.Args, origArgVals, callSubst) // T0331/T0418
	return result
}

// genModuleGenericFuncCall generates a call to a monomorphized generic function
// that is qualified by a module name. Example: json.encode_string[Config](value)
// The mono function is stored in c.funcs as "encode_string[Config]" (no module prefix).
func (c *Compiler) genModuleGenericFuncCall(e *ast.CallExpr, idx *ast.IndexExpr, funcName string) value.Value {
	// Build mangled name: funcName[typeArg1, typeArg2, ...]
	allTypeArgExprs := append([]ast.Expr{idx.Index}, idx.ExtraIndices...)
	mangledName := funcName + "["
	for i, argExpr := range allTypeArgExprs {
		typeArgType := c.info.Types[argExpr]
		if c.typeSubst != nil && typeArgType != nil {
			typeArgType = types.Substitute(typeArgType, c.typeSubst)
		}
		if i > 0 {
			mangledName += ", "
		}
		mangledName += typeArgStr(typeArgType)
	}
	mangledName += "]"

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined monomorphic module function %q", mangledName))
	}

	// T0639: Resolve the callee signature + per-call type-arg subst BEFORE arg
	// generation so genGenericCallArgs passes `~`/`&` params by pointer with
	// concrete (substituted) param types.
	var calleeSig *types.Signature
	var callSubst map[*types.TypeParam]types.Type
	if callee := c.lookupFunc(funcName); callee != nil {
		if sig, sOk := callee.Type().(*types.Signature); sOk {
			calleeSig = sig
			callSubst = c.buildCallTypeArgSubst(sig.TypeParams(), allTypeArgExprs)
		}
	}
	// Module-qualified callee may not be visible via lookupFunc; fall back to
	// the callee expression's type recorded by sema.
	if calleeSig == nil {
		if sig, sOk := c.info.Types[e.Callee].(*types.Signature); sOk {
			calleeSig = sig
		}
	}

	argVals, argTypes, variadicPTs := c.genGenericCallArgs(e.Args, calleeSig, callSubst)
	origArgVals := argVals // T0331: save pre-coercion values for alias check

	if calleeSig != nil {
		argVals = c.coerceCallArgs(argVals, argTypes, calleeSig.Params(), e.Args, callSubst)
	}

	result := c.block.NewCall(fn, argVals...)
	c.clearVariadicStaticFlags(variadicPTs)
	c.emitReturnAliasCheckSubst(result, calleeSig, e.Args, origArgVals, callSubst) // T0331/T0418
	return result
}

// genGenericMethodCall generates a call to a monomorphized generic method.
// Example: box.transform[string](fn) → "Box.transform[string]"(this, fn)
// Example: box.transform[string](fn) where box is Box[int] → "Box[int].transform[string]"(this, fn)
//
// typeArgs are the concrete method-level type arguments (already extracted from
// either an explicit IndexExpr or sema's InferredTypeArgs). c.typeSubst is
// applied to each arg before mangling.
func (c *Compiler) genGenericMethodCall(e *ast.CallExpr, member *ast.MemberExpr, typeArgs []types.Type) value.Value {
	targetType := c.info.Types[member.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	// T0636: generic method on a generic enum instance (or via `this` inside a
	// generic enum body). Enums don't have a Named layout, so route to the
	// enum-specific call path.
	if extractEnum(targetType) != nil {
		return c.genGenericEnumMethodCall(e, member, typeArgs, targetType)
	}

	named := extractNamed(targetType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot resolve type for generic method call on %T", targetType))
	}

	method := named.LookupMethod(member.Field)
	if method == nil {
		panic(fmt.Sprintf("codegen: no method %s on type %s", member.Field, named))
	}

	// Build mono method name: DefiningType.method[typearg1, typearg2]
	// Use the method's defining type (which may be a parent), not the target type.
	defOwnerName := c.resolveMethodOwner(named, member.Field)
	if defOwnerName != named.Obj().Name() {
		// Inherited — resolve mono parent name if the parent is generic
		defOwnerName = c.resolveMonoParentName(named, targetType, defOwnerName)
	} else {
		defOwnerName = c.resolveTypeName(targetType)
	}
	mangledName := mangleMethodName(defOwnerName, member.Field, false) + "["
	for i, ta := range typeArgs {
		if c.typeSubst != nil && ta != nil {
			ta = types.Substitute(ta, c.typeSubst)
		}
		if i > 0 {
			mangledName += ", "
		}
		mangledName += typeArgStr(ta)
	}
	mangledName += "]"

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined monomorphic method %q", mangledName))
	}

	// Generate receiver
	var args []value.Value
	if method.Sig().Recv() != nil {
		target := c.genExprAutoPropagate(member.Target) // B0323
		// T0130: Defer receiver claim — only claim if method produces a new iterator
		// (combinator). Terminal operations (count, collect, find) don't capture the
		// receiver, so the heap temp should be freed at statement end.
		c.pendingReceiverClaim = target
		if _, isThis := member.Target.(*ast.ThisExpr); isThis {
			args = append(args, target)
		} else if isContainerType(targetType) {
			args = append(args, target)
		} else if isPrimitiveScalar(named) {
			args = append(args, target)
		} else if named.IsValueType() {
			args = append(args, c.valueTypeReceiverPtr(target, targetType))
		} else {
			instancePtr := c.extractInstancePtr(target)
			args = append(args, instancePtr)
			// B0258: Track method chain intermediate for cleanup at statement end.
			c.trackChainIntermediateReceiver(member.Target, target, instancePtr, named, targetType)
		}
	}

	// Generate arguments
	argVals, argTypes, variadicPTs := c.genCallArgsWithMutRef(e.Args, method.Sig().Params())
	origArgVals := argVals // B0345: save pre-coercion values
	// T0418: Combine owner-type subst (Box[int].T → int + parents) with
	// method-level subst (transform[string].T → string).
	ownerSubst := c.buildOwnerTypeArgSubst(targetType)
	methodSubst := c.buildInferredCallSubst(method.Sig().TypeParams(), typeArgs)
	combined := mergeSubstMaps(ownerSubst, methodSubst)
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params(), e.Args, combined)
	args = append(args, argVals...)

	result := c.block.NewCall(fn, args...)
	c.clearVariadicStaticFlags(variadicPTs)
	c.emitReturnAliasCheckSubst(result, method.Sig(), e.Args, origArgVals, combined) // B0345/T0418
	return result
}

// genGenericEnumMethodCall generates a call to a monomorphized generic method
// whose receiver is a generic enum instance (T0636). It mirrors the receiver
// convention of genEnumMethodCall (pass `this` directly; otherwise store to a
// temp alloca, bitcast to i8*, and enum-drop fresh temporaries after the call)
// and the mono-name/subst construction of genGenericMethodCall.
//
// typeArgs are the concrete method-level type arguments (already extracted from
// either an explicit IndexExpr or sema's InferredTypeArgs). c.typeSubst is
// applied to each arg before mangling.
func (c *Compiler) genGenericEnumMethodCall(e *ast.CallExpr, member *ast.MemberExpr, typeArgs []types.Type, targetType types.Type) value.Value {
	// T0639: a ~/& generic-enum-instance receiver arrives wrapped in a
	// MutRef/SharedRef; unwrap so the enum + monoName resolve instead of
	// hitting the "cannot resolve enum" panic.
	if ref, ok := targetType.(*types.MutRef); ok {
		return c.genGenericEnumMethodCall(e, member, typeArgs, ref.Elem())
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		return c.genGenericEnumMethodCall(e, member, typeArgs, ref.Elem())
	}
	var enum *types.Enum
	var enumName string
	switch t := targetType.(type) {
	case *types.Enum:
		enum = t
		enumName = t.Obj().Name()
		// Inside a mono enum method body, `this` is the origin enum — use the
		// monomorphized instance name (mirrors genEnumMethodCall).
		if c.monoCtx != nil {
			if origin, ok := c.monoCtx.origin.(*types.Enum); ok && t == origin {
				enumName = c.monoCtx.name
			}
		}
	case *types.Instance:
		if en, ok := t.Origin().(*types.Enum); ok {
			enum = en
			enumName = monoName(t)
		}
	}
	if enum == nil {
		panic(fmt.Sprintf("codegen: cannot resolve enum for generic method call on %T", targetType))
	}

	method := enum.LookupMethod(member.Field)
	if method == nil {
		panic(fmt.Sprintf("codegen: no method %s on enum %s", member.Field, enumName))
	}

	// Build mono method name: EnumMonoName.method[typearg1, typearg2]
	// (consistent with monoMethodInstanceName).
	mangledName := mangleMethodName(enumName, member.Field, false) + "["
	for i, ta := range typeArgs {
		if c.typeSubst != nil && ta != nil {
			ta = types.Substitute(ta, c.typeSubst)
		}
		if i > 0 {
			mangledName += ", "
		}
		mangledName += typeArgStr(ta)
	}
	mangledName += "]"

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined monomorphic enum method %q", mangledName))
	}

	// Generate receiver using the enum convention (mirrors genEnumMethodCall).
	var args []value.Value
	var tempEnumPtr value.Value // non-nil when receiver needs post-call drop
	if method.Sig().Recv() != nil {
		prevEnumTemps := len(c.enumCtorTemps)
		target := c.genExprAutoPropagate(member.Target) // B0323
		enumCtorTracked := len(c.enumCtorTemps) > prevEnumTemps
		if _, isThis := member.Target.(*ast.ThisExpr); isThis {
			args = append(args, target)
		} else {
			alloca := c.entryBlock.NewAlloca(target.Type())
			alloca.SetName(c.uniqueLocalName("enum.this"))
			c.block.NewStore(target, alloca)
			ptr := c.block.NewBitCast(alloca, irtypes.I8Ptr)
			args = append(args, ptr)
			// T0660: a borrow-return receiver (`Tagged&`/`Tagged~`) aliases
			// the owner's payload (e.g. `ev.at(0)` shares ev.items[0]'s
			// string); dropping the synthesized receiver temp would
			// double-free what the owner still frees at scope exit.
			if isFreshEnumExpr(member.Target) && !enumCtorTracked && !c.isBorrowedExpr(member.Target) {
				tempEnumPtr = ptr
			}
		}
	}

	argVals, argTypes, variadicPTs := c.genCallArgsWithMutRef(e.Args, method.Sig().Params())
	origArgVals := argVals // B0345
	// T0418/T0636: owner-enum subst (Box[int].T → int) merged with the
	// method-level subst (transform[string].U → string).
	var ownerSubst map[*types.TypeParam]types.Type
	if inst, ok := targetType.(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Enum); ok && len(origin.TypeParams()) > 0 {
			ownerSubst = types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
		}
	}
	methodSubst := c.buildInferredCallSubst(method.Sig().TypeParams(), typeArgs)
	combined := mergeSubstMaps(ownerSubst, methodSubst)
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params(), e.Args, combined)
	args = append(args, argVals...)

	result := c.block.NewCall(fn, args...)
	c.clearVariadicStaticFlags(variadicPTs)
	c.emitReturnAliasCheckSubst(result, method.Sig(), e.Args, origArgVals, combined) // B0345/T0418

	// Drop temp enum receiver if it was a fresh temporary (mirrors genEnumMethodCall).
	if tempEnumPtr != nil && c.enumInstanceHasDrop(targetType, enum) {
		dropName := mangleMethodName(enumName, "drop", false)
		if dropFn, ok := c.funcs[dropName]; ok {
			c.block.NewCall(dropFn, tempEnumPtr)
		} else if c.moduleInfos != nil {
			if dropFn := c.forwardDeclareModuleEnumDrop(enum, enumName, dropName); dropFn != nil {
				c.block.NewCall(dropFn, tempEnumPtr)
			}
		}
	}

	return result
}

// --- super() calls ---

// genSuperCall generates a super() call inside a new() constructor body.
// Calls the parent's new() (if parent has one) or sets parent fields directly.
func (c *Compiler) genSuperCall(e *ast.CallExpr) value.Value {
	named := c.currentNamed
	if named == nil || len(named.Parents()) == 0 {
		return nil // sema already validated
	}
	parent := named.Parents()[0].Named

	// Load the this pointer
	thisAlloca := c.locals["this"]
	thisPtr := c.block.NewLoad(irtypes.I8Ptr, thisAlloca)

	if parent.HasNew() {
		// Parent has explicit new() — call ParentType.new(this, args...)
		parentName := parent.Obj().Name()
		mangledName := mangleMethodName(parentName, "new", false)
		fn, ok := c.funcs[mangledName]
		if !ok {
			panic(fmt.Sprintf("codegen: undeclared parent constructor %s", mangledName))
		}

		var argVals []value.Value
		var argTypes []types.Type
		for _, arg := range e.Args {
			argVals = append(argVals, c.genCallArgExpr(arg.Value))
			argTypes = append(argTypes, c.info.Types[arg.Value])
			if ident, ok := arg.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
		}
		newMethod := parent.LookupMethod("new")
		if newMethod != nil {
			// T0418: Build subst for parent's TypeParams from the inheritance
			// type args (e.g., `type Foo[T] is Bar[T]` → Bar.T → resolved T).
			var superSubst map[*types.TypeParam]types.Type
			if parentRef := named.Parents()[0]; len(parentRef.TypeArgs) > 0 && len(parent.TypeParams()) > 0 {
				resolvedArgs := make([]types.Type, len(parentRef.TypeArgs))
				for i, ta := range parentRef.TypeArgs {
					if c.typeSubst != nil {
						ta = types.Substitute(ta, c.typeSubst)
					}
					resolvedArgs[i] = ta
				}
				superSubst = types.BuildSubstMap(parent.TypeParams(), resolvedArgs)
			}
			argVals = c.coerceCallArgs(argVals, argTypes, newMethod.Sig().Params(), e.Args, superSubst)
		}
		args := append([]value.Value{thisPtr}, argVals...)
		result := c.block.NewCall(fn, args...)
		if newMethod != nil && newMethod.Sig().CanError() {
			tag := c.block.NewExtractValue(result, 0)
			errBlock := c.newBlock("super.err")
			okBlock := c.newBlock("super.ok")
			c.block.NewCondBr(tag, errBlock, okBlock)
			// Error path: propagate
			c.block = errBlock
			resultType := fn.Sig.RetType.(*irtypes.StructType)
			errVal := c.block.NewExtractValue(result, resultErrIdx(resultType))
			outerResultType := c.fn.Sig.RetType.(*irtypes.StructType)
			errResult := c.wrapError(errVal, outerResultType)
			c.block.NewRet(errResult)
			// Continue on ok path
			c.block = okBlock
		}
		return nil
	}

	// Parent has implicit constructor — set parent fields directly on `this`
	// Use the child's own layout since parent fields are part of the child's instance struct
	childLayout := c.lookupTypeLayout(named)
	if childLayout == nil {
		return nil
	}
	instanceStructType := childLayout.Instance.LLVMType
	instancePtrType := childLayout.InstancePtrType

	// Build map of provided field values
	provided := make(map[string]value.Value)
	for _, arg := range e.Args {
		if arg.Name != "" {
			provided[arg.Name] = c.genCallArgExpr(arg.Value)
			if ident, ok := arg.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
		}
	}

	// Set each parent field on the instance
	instancePtr := c.block.NewBitCast(thisPtr, instancePtrType)
	allFields := parent.AllFields()
	for _, f := range allFields {
		val, ok := provided[f.Name()]
		if !ok {
			// Use default if available, else zero
			if defExpr, hasDef := c.info.FieldDefaults[f]; hasDef {
				val = c.genExpr(defExpr)
			} else {
				val = c.zeroValue(c.resolveType(f.Type()))
			}
		}
		fieldIdx := childLayout.InstanceFieldIndex[f.Name()]
		fieldPtr := c.block.NewGetElementPtr(instanceStructType, instancePtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
		c.block.NewStore(val, fieldPtr)
	}
	return nil
}

// --- Constructor calls ---

// genConstructorCallMono generates a heap-allocated instance of a user type.
// Handles both regular Named types and generic Instance types via lookupTypeLayout.
func (c *Compiler) genConstructorCallMono(e *ast.CallExpr, typ types.Type) value.Value {
	named := extractNamed(typ)
	layout := c.lookupTypeLayout(typ)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for type %s", typ))
	}

	// Value types: no heap allocation, build value struct with insertvalue chain
	if layout.IsValueType {
		return c.genValueTypeConstructor(e, named, layout, typ)
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

	// Allocate
	rawPtr := c.block.NewCall(c.palAlloc, size)
	typedPtr := c.block.NewBitCast(rawPtr, instancePtrType)

	// T0135: Track allocation as heap temp so auto-propagation error paths
	// free it if a failable constructor argument fails.
	c.trackHeapTemp(rawPtr, c.palFree)

	// Store type info pointer in _variant slot (field 0) for RTTI
	variantFieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	variantPtrType := layout.Instance.Fields[0].LLVMType.(*irtypes.PointerType)
	if tiGlobal := c.lookupTypeInfoGlobal(typ); tiGlobal != nil {
		tiPtr := c.block.NewBitCast(tiGlobal, variantPtrType)
		c.block.NewStore(tiPtr, variantFieldPtr)
	} else {
		c.block.NewStore(constant.NewNull(variantPtrType), variantFieldPtr)
	}

	// If the type has an explicit new() constructor, call it instead of field matching
	if named != nil && named.HasNew() {
		// Zero-init all fields first
		for _, f := range named.AllFields() {
			fieldIdx := layout.InstanceFieldIndex[f.Name()]
			fieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			c.block.NewStore(c.zeroValue(layout.Instance.Fields[fieldIdx].LLVMType), fieldPtr)
		}

		// Call new() with instance ptr as receiver + user args
		mangledName := mangleMethodName(c.resolveTypeName(typ), "new", false)
		fn, ok := c.funcs[mangledName]
		if !ok {
			panic(fmt.Sprintf("codegen: undeclared new() for type %s (mangled: %s)", typ, mangledName))
		}
		// B0199: Look up new() method BEFORE processing args so we can check
		// parameter move semantics. Only clear caller's drop flag for move (~)
		// parameters. Borrow parameters get a copy (strdup for strings), so the
		// caller must keep its drop flag to free the original.
		newMethod := named.LookupMethod("new")
		var newParams []*types.Param
		if newMethod != nil {
			newParams = newMethod.Sig().Params()
		}

		var argVals []value.Value
		var argTypes []types.Type
		for i, arg := range e.Args {
			// T0552: Save enum ctor temp count so we can clear those added during
			// this arg's evaluation once the value is passed to new() (which stores
			// it into a field). Without clearing, the temp's scope-exit drop runs
			// and double-frees the variant payload the owner's synth drop now also
			// handles. Symmetric to the implicit-constructor field loop below.
			savedEnumTemps := len(c.enumCtorTemps)
			v := c.genCallArgExpr(arg.Value)
			argVals = append(argVals, v)
			argTypes = append(argTypes, c.info.Types[arg.Value])
			// B0199: For string-typed borrow params on types with HasDrop(),
			// the constructor body strdups the string (genAssignment detects no
			// drop flag on the param → dupString). The caller must keep its drop
			// flag so the original string is freed. For move params and non-string
			// params, clear the drop flag as before (direct pointer store).
			skipClear := false
			isMoveParam := true
			if newMethod != nil && i < len(newParams) {
				paramType := newParams[i].Type()
				_, isMutRef := paramType.(*types.MutRef)
				isMoveParam = isMutRef || newParams[i].Ref() == types.RefMut
				if !isMoveParam && extractNamed(paramType) == types.TypString && (named.HasDrop() || named.NeedsSynthDrop()) {
					skipClear = true
				}
			}
			if !skipClear {
				if ident, ok := arg.Value.(*ast.IdentExpr); ok {
					c.clearDropFlag(ident.Name)
				}
				// B0301: Neutralize source optional for opt! args in new() constructors.
				c.neutralizeForceUnwrapSource(arg.Value)
			}
			// B0211: Don't claim string temp when the constructor will strdup
			// (skipClear=true: borrow string on type with drop/synth-drop).
			// The strdup creates an independent copy; the original temp should be freed.
			if !skipClear {
				c.claimStringTemp(v) // B0168: ownership transferred to new() args
			}
			// B0233: Claim heap temp — ownership transferred to new() constructor field.
			c.claimHeapTemp(v)
			c.claimEnvTemp(v) // T0100: claim env temp for closure args
			// T0552: Clear enum ctor temps created during arg evaluation when the
			// param consumes the enum by move — ownership transfers to new() and
			// then to whatever field it stores into. Borrow params don't take
			// ownership, so leave the temps in place for the caller's cleanup.
			if isMoveParam {
				for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
					c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
				}
				c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
			}
		}
		// B0233: Do NOT claim heap temp here. Let downstream consumers claim:
		// - Variable assignment (stmt.go genAssignment)
		// - ~ params (call site)
		// - Container store (push, send, field/index assign)
		// If nobody claims, cleanupHeapTemps frees the temp at statement end.

		if newMethod != nil {
			// T0418: Resolve T-typed params against the owner type's
			// concrete args (e.g., Box[int] → T → int) so generic
			// constructors with T? params don't double-wrap.
			ownerSubst := c.buildOwnerTypeArgSubst(typ)
			argVals = c.coerceCallArgs(argVals, argTypes, newMethod.Sig().Params(), e.Args, ownerSubst)
		}
		args := append([]value.Value{typedPtr}, argVals...)
		newResult := c.block.NewCall(fn, args...)

		// If failable new, check error and wrap result
		if newMethod == nil {
			newMethod = named.LookupMethod("new")
		}
		if newMethod != nil && newMethod.Sig().CanError() {
			// new() returned { i1, i8* } — check tag
			newResultType := newResult.Type().(*irtypes.StructType)
			tag := c.block.NewExtractValue(newResult, 0)

			errBlock := c.newBlock("new.err")
			okBlock := c.newBlock("new.ok")
			mergeBlock := c.newBlock("new.merge")
			c.block.NewCondBr(tag, errBlock, okBlock)

			// Error path: propagate error wrapped in constructor result type
			constructorResultType := computeResultType(userValueType())
			c.block = errBlock
			errVal := c.block.NewExtractValue(newResult, resultErrIdx(newResultType))
			errResult := c.wrapError(errVal, constructorResultType)
			c.block.NewBr(mergeBlock)

			// Ok path: build value struct and wrap
			c.block = okBlock
			// T0345: Swap heap-temp dropFunc from palFree to the type's full drop
			// so unclaimed/discarded instances free their transitive heap fields.
			c.updateConstructorTempDrop(rawPtr, named, typ)
			var vtablePtr2 value.Value
			if vtGlobal := c.lookupVtableGlobal(typ); vtGlobal != nil {
				vtablePtr2 = constant.NewBitCast(vtGlobal, irtypes.I8Ptr)
			} else {
				vtablePtr2 = constant.NewNull(irtypes.I8Ptr)
			}
			var valStruct value.Value = constant.NewUndef(userValueType())
			valStruct = c.block.NewInsertValue(valStruct, vtablePtr2, 0)
			valStruct = c.block.NewInsertValue(valStruct, rawPtr, 1)
			okResult := c.wrapOk(valStruct, constructorResultType)
			c.block.NewBr(mergeBlock)

			// Merge: phi between error and ok results
			c.block = mergeBlock
			phi := c.block.NewPhi(ir.NewIncoming(errResult, errBlock), ir.NewIncoming(okResult, okBlock))
			return phi
		}
	} else {
		// Implicit constructor: match arguments to field names.
		// Build field-type lookup for optional wrapping.
		// B0210: Build a substitution map from the Instance type args so field types
		// are properly substituted even when c.typeSubst is nil (user code calling
		// a generic constructor directly, not inside a monomorphic method body).
		var localSubst map[*types.TypeParam]types.Type
		if inst, ok := typ.(*types.Instance); ok {
			if origin, ok := inst.Origin().(*types.Named); ok && len(origin.TypeParams()) > 0 {
				localSubst = types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			}
		}
		fieldTypeMap := make(map[string]types.Type)
		for _, f := range named.AllFields() {
			ft := f.Type()
			if c.typeSubst != nil {
				ft = types.Substitute(ft, c.typeSubst)
			}
			if localSubst != nil {
				ft = types.Substitute(ft, localSubst)
			}
			fieldTypeMap[f.Name()] = ft
		}

		// maybeWrapOptional wraps val in an optional struct when the field type
		// is T? but the expression produces a non-optional, non-none value.
		// Uses Identical (not "is exprOpt?") so a T? expr targeting a T?? field
		// still gets wrapped to match the slot's depth.
		maybeWrapOptional := func(val value.Value, expr ast.Expr, fieldName string, fieldIdx int) value.Value {
			fieldType := fieldTypeMap[fieldName]
			if _, isOpt := fieldType.(*types.Optional); !isOpt {
				return val
			}
			exprType := c.info.Types[expr]
			if c.typeSubst != nil {
				exprType = types.Substitute(exprType, c.typeSubst)
			}
			if exprType == types.TypNone {
				return val
			}
			if types.Identical(exprType, fieldType) {
				return val
			}
			return c.wrapOptional(val, layout.Instance.Fields[fieldIdx].LLVMType.(*irtypes.StructType))
		}

		provided := make(map[string]bool)
		for _, arg := range e.Args {
			if arg.Name == "" {
				panic(fmt.Sprintf("codegen: positional constructor args not supported for %s", typ))
			}
			provided[arg.Name] = true
			// T0552: Save enum ctor temp count so we can clear those added during
			// this arg's evaluation once the value is stored in the field. The
			// field becomes the unique owner of the enum data; without clearing,
			// the temp's scope-exit drop runs and double-frees the variant payload
			// the owner's synth drop now also handles.
			savedEnumTemps := len(c.enumCtorTemps)
			fieldIdx, ok := layout.InstanceFieldIndex[arg.Name]
			if !ok {
				panic(fmt.Sprintf("codegen: unknown field %s on type %s", arg.Name, typ))
			}
			// B0210: For optional fields, generate none values directly from the layout
			// type instead of resolveType(targetType), which may produce a wrong LLVM
			// type when TypeParams aren't fully substituted in all code paths.
			var val value.Value
			if _, isOpt := fieldTypeMap[arg.Name].(*types.Optional); isOpt {
				if _, isNone := arg.Value.(*ast.NoneLit); isNone {
					// Generate zero value directly from the layout's LLVM type
					val = c.zeroValue(layout.Instance.Fields[fieldIdx].LLVMType)
				} else {
					if ft, ok := fieldTypeMap[arg.Name].(*types.Optional); ok {
						c.targetType = ft
					}
					// T0411: Auto-dup string/container fields read from a droppable
					// owner so the new instance gets an independent copy.
					c.maybeEnableDupForConstructorArg(arg.Value, fieldTypeMap[arg.Name])
					val = c.genCallArgExpr(arg.Value)
					c.dupStringFieldAccess = false
					c.dupContainerFieldAccess = false
					c.targetType = nil
				}
			} else {
				// T0411: Auto-dup string/container fields read from a droppable
				// owner so the new instance gets an independent copy.
				c.maybeEnableDupForConstructorArg(arg.Value, fieldTypeMap[arg.Name])
				val = c.genCallArgExpr(arg.Value)
				c.dupStringFieldAccess = false
				c.dupContainerFieldAccess = false
			}
			// T0101: Save pre-wrap value for string temp claiming on optional fields
			preWrapVal := val
			val = maybeWrapOptional(val, arg.Value, arg.Name, fieldIdx)
			fieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))

			// T0095: String fields in types with synthesized drops require ownership.
			// If the source is a variable without a drop flag (e.g., a function
			// parameter without ~), dup the string so the type owns an independent
			// copy. This prevents double-free when both the caller's variable and
			// the type's synthesized drop try to free the same allocation.
			fType := fieldTypeMap[arg.Name]
			if c.typeSubst != nil {
				fType = types.Substitute(fType, c.typeSubst)
			}
			// T0101: Also handle string? fields — the inner string temp must be claimed.
			isOptionalString := false
			isStringField := extractNamed(fType) == types.TypString
			if !isStringField {
				if opt, ok := fType.(*types.Optional); ok && extractNamed(opt.Elem()) == types.TypString {
					isStringField = true
					isOptionalString = true
				}
			}
			if isStringField {
				if ident, ok := arg.Value.(*ast.IdentExpr); ok {
					if _, hasFlag := c.dropFlags[ident.Name]; hasFlag {
						// Has drop flag: move ownership (existing behavior)
						c.block.NewStore(val, fieldPtr)
						c.clearDropFlag(ident.Name)
					} else if !isOptionalString {
						// No drop flag (function param without ~): dup for exclusive ownership
						// Skip dup for optional strings — the inner value is already owned
						c.block.NewStore(c.dupString(val), fieldPtr)
					} else {
						c.block.NewStore(val, fieldPtr)
					}
				} else {
					// Expression result: claim temp, store directly
					c.block.NewStore(val, fieldPtr)
					// B0301: Neutralize source optional for opt! on string fields.
					c.neutralizeForceUnwrapSource(arg.Value)
					// For optional strings, claim the pre-wrap value (the raw i8* temp)
					if isOptionalString {
						c.claimStringTemp(preWrapVal)
					} else {
						c.claimStringTemp(val)
					}
				}
			} else {
				c.block.NewStore(val, fieldPtr)
				// Clear drop flag: field value is moved into the constructor
				if ident, ok := arg.Value.(*ast.IdentExpr); ok {
					c.clearDropFlag(ident.Name)
				}
				// B0301: When arg is opt! (force-unwrap), neutralize the source optional's
				// present flag so its scope cleanup won't double-free the inner value
				// that now lives in the constructor field.
				c.neutralizeForceUnwrapSource(arg.Value)
				// T0353: For optional fields wrapping stmtTemp-tracked heap values
				// (Vector, Channel), the wrapped {i1, i8*} won't match the bare i8*
				// in stmtTempMap. Claim preWrapVal too.
				c.claimStringTemp(preWrapVal)
				// B0168: Claim string temp — ownership transferred to constructor field.
				c.claimStringTemp(val)
				// B0233: Claim heap temp — ownership transferred to constructor field.
				c.claimHeapTemp(val)
				// T0100: Claim env temp — closure env is now owned by the struct field.
				c.claimEnvTemp(val)
			}
			// T0498: Claim per-field optionalStringDup / optionalContainerDup for
			// Optional[X] field-reads from droppable owners. genFieldAccess sets
			// these to the bare inner-dup pointer; the wrapped {i1, ptr} struct
			// passed to claimStringTemp above won't match the stmtTempMap entry.
			// Without claiming per-arg, the next arg's genFieldAccess overwrites
			// these fields and earlier dups stay live → use-after-free at
			// cleanupStmtTemps after the constructor.
			if c.optionalStringDup != nil {
				c.claimStringTemp(c.optionalStringDup)
				c.optionalStringDup = nil
			}
			if c.optionalContainerDup != nil {
				c.claimStringTemp(c.optionalContainerDup)
				c.optionalContainerDup = nil
			}
			// T0552: Clear enum ctor temps created during this arg's evaluation —
			// the field is now the unique owner of the enum's variant data, so the
			// ctor temp's scope-exit drop must not fire. Without this, the
			// owner's synth drop (which T0552 makes drop the enum field) and the
			// temp drop both target the same heap allocation → double-free.
			for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
				c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
			}
			c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
		}

		// Initialize omitted fields: evaluate default expression if present, otherwise zero-init.
		for _, f := range named.AllFields() {
			if provided[f.Name()] {
				continue
			}
			fieldIdx := layout.InstanceFieldIndex[f.Name()]
			fieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			if defExpr, ok := c.info.FieldDefaults[f]; ok {
				val := c.genExpr(defExpr)
				preWrapVal := val // T0353: needed for optional-wrapped stmtTemp claim
				val = maybeWrapOptional(val, defExpr, f.Name(), fieldIdx)
				c.block.NewStore(val, fieldPtr)
				c.claimStringTemp(preWrapVal) // T0353: claim bare i8* hidden inside the wrap
				c.claimStringTemp(val)        // B0168: ownership transferred to field
				c.claimHeapTemp(val)          // B0233: ownership transferred to field
			} else {
				c.block.NewStore(c.zeroValue(layout.Instance.Fields[fieldIdx].LLVMType), fieldPtr)
			}
		}
	}

	// T0128: In structural default methods on _FnIter, store the receiver (upstream
	// _FnIter instance) as _parent for chained iterator cleanup. This enables
	// iterCleanup to recursively free the entire combinator chain.
	if c.selfSubst != nil && named != nil {
		if c.selfSubst.concrete.Obj().Name() == "_FnIter" && named.Obj().Name() == "_FnIter" {
			if parentIdx, ok := layout.InstanceFieldIndex["_parent"]; ok {
				if thisAlloca, ok := c.locals["this"]; ok {
					thisPtr := c.block.NewLoad(irtypes.I8Ptr, thisAlloca)
					thisInt := c.block.NewPtrToInt(thisPtr, irtypes.I64)
					parentFieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(parentIdx)))
					c.block.NewStore(thisInt, parentFieldPtr)
				}
			}
		}
	}

	// B0233: Do NOT claim heap temp here. Let downstream consumers claim:
	// - Variable assignment (stmt.go genAssignment)
	// - ~ params (call site)
	// - Container store (push, send, field/index assign)
	// If nobody claims, cleanupHeapTemps frees the temp at statement end.

	// T0345: Swap heap-temp dropFunc from palFree to the type's full drop so
	// unclaimed/discarded instances free their transitive heap fields. Covers
	// both non-failable HasNew and implicit-constructor paths.
	c.updateConstructorTempDrop(rawPtr, named, typ)

	// Build value struct: { vtable_ptr, instance_ptr }
	var vtablePtr value.Value
	if vtGlobal := c.lookupVtableGlobal(typ); vtGlobal != nil {
		vtablePtr = constant.NewBitCast(vtGlobal, irtypes.I8Ptr)
	} else {
		vtablePtr = constant.NewNull(irtypes.I8Ptr)
	}
	var valStruct value.Value = constant.NewUndef(userValueType())
	valStruct = c.block.NewInsertValue(valStruct, vtablePtr, 0)
	valStruct = c.block.NewInsertValue(valStruct, rawPtr, 1)

	return valStruct
}

// resolveDropFuncForTemp returns the cleanup function for a heap-allocated
// type's temporary instance (B0211). Returns nil for value types, copy types,
// and primitive scalars that don't heap-allocate.
func (c *Compiler) resolveDropFuncForTemp(named *types.Named, typ types.Type) *ir.Func {
	if named == nil || named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) {
		return nil
	}
	// Types with explicit drop method
	if named.HasDrop() {
		resolvedTyp := typ
		if c.typeSubst != nil {
			resolvedTyp = types.Substitute(typ, c.typeSubst)
		}
		explicitDrop := !named.NeedsSynthDrop()
		ownerName := named.Obj().Name()
		if inst, ok := resolvedTyp.(*types.Instance); ok {
			ownerName = monoName(inst)
		} else if explicitDrop {
			ownerName = c.resolveDropOwner(named)
		}
		mangledName := mangleMethodName(ownerName, "drop", false)
		if fn, ok := c.funcs[mangledName]; ok {
			// B0325: Explicit user drops don't include pal_free — wrap with $wrap
			// so the cleanup path frees the instance after calling drop.
			// Synthesized drops already include pal_free.
			if explicitDrop {
				return c.getOrCreateDropWrap(mangledName, fn)
			}
			return fn
		}
	}
	// Types with synthesized drop
	if named.NeedsSynthDrop() {
		resolvedTyp := typ
		if c.typeSubst != nil {
			resolvedTyp = types.Substitute(typ, c.typeSubst)
		}
		ownerName := named.Obj().Name()
		if inst, ok := resolvedTyp.(*types.Instance); ok {
			ownerName = monoName(inst)
		}
		mangledName := mangleMethodName(ownerName, "drop", false)
		if fn, ok := c.funcs[mangledName]; ok {
			return fn
		}
	}
	// Mono instance with codegen-detected synth drop (B0202)
	if inst, ok := typ.(*types.Instance); ok && monoInstNeedsSynthDrop(inst) {
		monoDropName := mangleMethodName(monoName(inst), "drop", false)
		if fn, ok := c.funcs[monoDropName]; ok {
			return fn
		}
	}
	// Heap type without drop: use pal_free
	return c.palFree
}

// updateConstructorTempDrop swaps the heap temp's dropFunc from palFree (the safe
// error-during-arg-eval default registered at T0135 by genConstructorCallMono) to
// the type's full drop after construction completes successfully (T0345). Without
// this swap, an unclaimed instance discarded or passed as a plain (non-`~`) arg
// would only get its top-level allocation freed at statement end, leaking any
// transitive heap fields (vector buffers, map storage, string allocations, etc.).
func (c *Compiler) updateConstructorTempDrop(rawPtr value.Value, named *types.Named, typ types.Type) {
	if rawPtr == nil || named == nil {
		return
	}
	idx, ok := c.heapTempMap[rawPtr]
	if !ok || idx < 0 {
		return
	}
	fullDrop := c.resolveDropFuncForTemp(named, typ)
	if fullDrop == nil || fullDrop == c.palFree {
		return
	}
	c.heapTemps[idx].dropFunc = fullDrop
}

// genValueTypeConstructor builds a value type by insertvalue chain — no heap allocation.
// Value struct layout: { i8* _vtable, field1, field2, ... }
func (c *Compiler) genValueTypeConstructor(e *ast.CallExpr, named *types.Named, layout *TypeDeclLayout, typ types.Type) value.Value {
	valueStructType := layout.Value.LLVMType

	// Start with undef
	var val value.Value = constant.NewUndef(valueStructType)

	// Field 0: vtable pointer
	if vtGlobal := c.lookupVtableGlobal(typ); vtGlobal != nil {
		val = c.block.NewInsertValue(val, constant.NewBitCast(vtGlobal, irtypes.I8Ptr), 0)
	} else {
		val = c.block.NewInsertValue(val, constant.NewNull(irtypes.I8Ptr), 0)
	}

	// If the type has an explicit new() constructor, alloca + store + call new() + load
	if named != nil && named.HasNew() {
		alloca := c.createEntryAlloca(valueStructType)
		c.block.NewStore(val, alloca)

		// Zero-init all user fields
		for _, f := range named.AllFields() {
			fieldIdx := layout.ValueFieldIndex[f.Name()]
			fieldPtr := c.block.NewGetElementPtr(valueStructType, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			c.block.NewStore(c.zeroValue(layout.Value.Fields[fieldIdx].LLVMType), fieldPtr)
		}

		// Call new() with pointer to value struct as receiver
		mangledName := mangleMethodName(c.resolveTypeName(typ), "new", false)
		fn, ok := c.funcs[mangledName]
		if !ok {
			panic(fmt.Sprintf("codegen: undeclared new() for value type %s (mangled: %s)", typ, mangledName))
		}
		var argVals []value.Value
		var argTypes []types.Type
		for _, arg := range e.Args {
			argVals = append(argVals, c.genCallArgExpr(arg.Value))
			argTypes = append(argTypes, c.info.Types[arg.Value])
			if ident, ok := arg.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
			// B0301: Neutralize source optional for opt! args.
			c.neutralizeForceUnwrapSource(arg.Value)
		}
		newMethod := named.LookupMethod("new")
		if newMethod != nil {
			// T0418: Resolve T-typed params against the owner type's
			// concrete args for generic value types.
			ownerSubst := c.buildOwnerTypeArgSubst(typ)
			argVals = c.coerceCallArgs(argVals, argTypes, newMethod.Sig().Params(), e.Args, ownerSubst)
		}
		thisPtr := c.block.NewBitCast(alloca, irtypes.I8Ptr)
		args := append([]value.Value{thisPtr}, argVals...)
		c.block.NewCall(fn, args...)
		return c.block.NewLoad(valueStructType, alloca)
	}

	// Implicit constructor: match arguments to field names
	// B0210: Build a local substitution map from the Instance type args so field types
	// are properly substituted even when c.typeSubst is nil.
	var vtLocalSubst map[*types.TypeParam]types.Type
	if inst, ok := typ.(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Named); ok && len(origin.TypeParams()) > 0 {
			vtLocalSubst = types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
		}
	}
	fieldTypeMap := make(map[string]types.Type)
	for _, f := range named.AllFields() {
		ft := f.Type()
		if c.typeSubst != nil {
			ft = types.Substitute(ft, c.typeSubst)
		}
		if vtLocalSubst != nil {
			ft = types.Substitute(ft, vtLocalSubst)
		}
		fieldTypeMap[f.Name()] = ft
	}

	maybeWrapOptional := func(v value.Value, expr ast.Expr, fieldName string, fieldIdx int) value.Value {
		fieldType := fieldTypeMap[fieldName]
		if _, isOpt := fieldType.(*types.Optional); !isOpt {
			return v
		}
		exprType := c.info.Types[expr]
		if c.typeSubst != nil {
			exprType = types.Substitute(exprType, c.typeSubst)
		}
		if exprType == types.TypNone {
			return v
		}
		// Use Identical (not "is exprOpt?") so a T? expr targeting a T?? field
		// still gets wrapped to match the slot's depth.
		if types.Identical(exprType, fieldType) {
			return v
		}
		return c.wrapOptional(v, layout.Value.Fields[fieldIdx].LLVMType.(*irtypes.StructType))
	}

	provided := make(map[string]bool)
	for _, arg := range e.Args {
		if arg.Name == "" {
			panic(fmt.Sprintf("codegen: positional constructor args not supported for %s", typ))
		}
		provided[arg.Name] = true
		fieldIdx, ok := layout.ValueFieldIndex[arg.Name]
		if !ok {
			panic(fmt.Sprintf("codegen: unknown field %s on type %s", arg.Name, typ))
		}
		// B0210: For optional fields, generate none values directly from the layout
		// type instead of resolveType(targetType), which may produce a wrong LLVM
		// type when TypeParams aren't fully substituted in all code paths.
		var fieldVal value.Value
		if _, isOpt := fieldTypeMap[arg.Name].(*types.Optional); isOpt {
			if _, isNone := arg.Value.(*ast.NoneLit); isNone {
				fieldVal = c.zeroValue(layout.Value.Fields[fieldIdx].LLVMType)
			} else {
				if ft, ok := fieldTypeMap[arg.Name].(*types.Optional); ok {
					c.targetType = ft
				}
				fieldVal = c.genCallArgExpr(arg.Value)
				c.targetType = nil
				fieldVal = maybeWrapOptional(fieldVal, arg.Value, arg.Name, fieldIdx)
			}
		} else {
			fieldVal = c.genCallArgExpr(arg.Value)
			fieldVal = maybeWrapOptional(fieldVal, arg.Value, arg.Name, fieldIdx)
		}
		val = c.block.NewInsertValue(val, fieldVal, uint64(fieldIdx))
		if ident, ok := arg.Value.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
		// B0301: Neutralize source optional for opt! args in value-type constructors.
		c.neutralizeForceUnwrapSource(arg.Value)
	}

	// Initialize omitted fields: evaluate default expression if present, otherwise zero-init
	for _, f := range named.AllFields() {
		if provided[f.Name()] {
			continue
		}
		fieldIdx := layout.ValueFieldIndex[f.Name()]
		if defExpr, ok := c.info.FieldDefaults[f]; ok {
			defVal := c.genExpr(defExpr)
			defVal = maybeWrapOptional(defVal, defExpr, f.Name(), fieldIdx)
			val = c.block.NewInsertValue(val, defVal, uint64(fieldIdx))
		} else {
			val = c.block.NewInsertValue(val, c.zeroValue(layout.Value.Fields[fieldIdx].LLVMType), uint64(fieldIdx))
		}
	}

	return val
}

// --- Member access ---

// genMemberExpr generates a field access on a user type instance or an enum variant value.
func (c *Compiler) genMemberExpr(e *ast.MemberExpr) value.Value {
	// Module-level getter: mod.property → call getter function with no args.
	// Guard: only intercept when sema resolved this as a getter (non-Signature type).
	// A Signature type means it's a function reference (e.g., auto f = mod.func),
	// which should NOT be called implicitly.
	if ident, ok := e.Target.(*ast.IdentExpr); ok {
		if modName := c.resolveModuleName(ident); modName != "" {
			if _, isSig := c.info.Types[e].(*types.Signature); !isSig {
				return c.genModuleGetterCall(e, modName, e.Field)
			}
		}
	}

	targetType := c.info.Types[e.Target]
	// Apply typeSubst for mono context
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	if c.selfSubst != nil {
		targetType = types.SubstituteSelf(targetType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	// T0381: unwrap SharedRef/MutRef so member dispatch sees the underlying
	// type. The runtime representation is identical (same pointer / value
	// struct), so all the type-based getter/method lookups below operate on
	// the owned form.
	if sr, ok := targetType.(*types.SharedRef); ok {
		targetType = sr.Elem()
	}
	if mr, ok := targetType.(*types.MutRef); ok {
		targetType = mr.Elem()
	}

	// Container .len property (string, vector, fixed array)
	// Check both Instance wrappers (user code: Vector[int]) and bare Named (method body: this is TypVector)
	if e.Field == "len" {
		if arr, ok := targetType.(*types.Array); ok {
			return constant.NewInt(irtypes.I64, arr.Size())
		}
		named := extractNamed(targetType)
		if named == types.TypString {
			return c.genStringLen(e)
		}
		if _, ok := types.AsVector(targetType); ok || named == types.TypVector {
			return c.genVectorLen(e)
		}
	}

	// Arc .borrow getter — returns the inner T value by loading from the Arc allocation.
	// T0155: Arc[T] atomic reference counting.
	if e.Field == "borrow" {
		if elem, ok := types.AsArc(targetType); ok {
			resolvedElem := elem
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(elem, c.typeSubst)
			}
			return c.genArcBorrow(e, resolvedElem)
		}
		named := extractNamed(targetType)
		if named == types.TypArc {
			if tp := c.resolveTypeParam(types.TypArc.TypeParams()[0]); tp != nil {
				return c.genArcBorrow(e, tp)
			}
		}
		// MutexGuard .borrow getter — loads T through the guard's mutex pointer.
		// T0156: MutexGuard[T] interior mutability.
		if elem, ok := types.AsMutexGuard(targetType); ok {
			resolvedElem := elem
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(elem, c.typeSubst)
			}
			return c.genMutexGuardBorrow(e, resolvedElem)
		}
		if named == types.TypMutexGuard {
			if tp := c.resolveTypeParam(types.TypMutexGuard.TypeParams()[0]); tp != nil {
				return c.genMutexGuardBorrow(e, tp)
			}
		}
	}

	// String .is_literal property — checks sign bit of length field
	if e.Field == "is_literal" {
		named := extractNamed(targetType)
		if named == types.TypString {
			return c.genStringIsLiteral(e)
		}
	}

	// Native hash getter for Hashable interface on primitive types
	if e.Field == "hash" {
		named := extractNamed(targetType)
		if named != nil {
			if v, ok := c.genNativeHashGetter(e, named); ok {
				return v
			}
		}
	}

	// Native bits getter: f64.bits/f32.bits returns IEEE 754 bit pattern
	if e.Field == "bits" {
		named := extractNamed(targetType)
		if named == types.TypF64 {
			target := c.genExprAutoPropagate(e.Target) // B0323
			return c.block.NewBitCast(target, irtypes.I64)
		}
		if named == types.TypF32 {
			target := c.genExprAutoPropagate(e.Target) // B0323
			return c.block.NewBitCast(target, irtypes.I32)
		}
	}

	// Enum variant access: Color.Red or Option[int].None
	// Check variant first; if the field is not a variant, check for enum getters.
	if enumLayout := c.lookupEnumLayout(targetType); enumLayout != nil {
		if _, isVariant := enumLayout.VariantTag[e.Field]; isVariant {
			return c.genEnumVariantValueLayout(enumLayout, e.Field)
		}
		// Not a variant — check for enum getter
		if result, ok := c.genEnumGetterAccess(e, targetType, enumLayout); ok {
			return result
		}
	}

	// For generic enum variants (e.g. Slot.Empty inside a generic type body),
	// the target type is a bare *types.Enum but the result type is an Instance
	// after mono substitution. Use the result type to find the layout.
	if _, ok := targetType.(*types.Enum); ok {
		resultType := c.info.Types[e]
		if c.typeSubst != nil {
			resultType = types.Substitute(resultType, c.typeSubst)
		}
		if enumLayout := c.lookupEnumLayout(resultType); enumLayout != nil {
			if _, isVariant := enumLayout.VariantTag[e.Field]; isVariant {
				return c.genEnumVariantValueLayout(enumLayout, e.Field)
			}
			if result, ok := c.genEnumGetterAccess(e, targetType, enumLayout); ok {
				return result
			}
		}
	}

	named := extractNamed(targetType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot resolve type for member access on %T", targetType))
	}

	field := named.LookupField(e.Field)
	if field != nil {
		return c.genFieldAccess(e, targetType, field)
	}

	// Getter property: emit a method call with no args beyond receiver
	if g := named.LookupGetter(e.Field); g != nil {
		return c.genGetterCall(e, targetType, named, g)
	}

	panic(fmt.Sprintf("codegen: member %s on type %s is not a field (method references not yet supported)", e.Field, named))
}

// genVectorCapacityConstructor generates a Vector with pre-allocated capacity: T[](capacity: n) or T[]().
func (c *Compiler) genVectorCapacityConstructor(e *ast.CallExpr, inst *types.Instance) value.Value {
	// capacity defaults to 16 when no argument provided
	var capacity value.Value
	if len(e.Args) > 0 {
		capacity = c.genCallArgExpr(e.Args[0].Value)
	} else {
		capacity = constant.NewInt(irtypes.I64, 16)
	}

	// Determine element size
	elemType := inst.TypeArgs()[0]
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(c.typeSize(elemLLVM))

	return c.block.NewCall(c.funcs["promise_vector_with_capacity"],
		capacity,
		constant.NewInt(irtypes.I64, elemSize))
}

// genChannelConstructor generates code for channel[T](capacity: n) or channel[T]().
// Calls @promise_channel_new(capacity, elem_size) → i8*.
func (c *Compiler) genChannelConstructor(e *ast.CallExpr, inst *types.Instance) value.Value {
	elemType := inst.TypeArgs()[0]
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(c.typeSize(elemLLVM))

	// capacity defaults to 0 (unbuffered) when no argument provided
	var capacity value.Value
	if len(e.Args) > 0 {
		capArg := c.genCallArgExpr(e.Args[0].Value)
		// Argument is int? — unwrap the optional to get the int value.
		// If it's a bare int literal, sema may pass it as int? via AssignableTo.
		argType := c.info.Types[e.Args[0].Value]
		if _, isOpt := argType.(*types.Optional); isOpt {
			// Extract value from { i1, i64 } optional — field 1
			capacity = c.block.NewExtractValue(capArg, 1)
		} else {
			capacity = capArg
		}
	} else {
		capacity = constant.NewInt(irtypes.I64, 0)
	}

	return c.block.NewCall(c.funcs["promise_channel_new"],
		capacity,
		constant.NewInt(irtypes.I64, elemSize))
}

// arcStructType returns the LLVM struct type for Arc[T]: {i64 strong_count, i64 weak_count, T value}.
// T0157: Arc layout includes weak_count for Weak[T] support.
func arcStructType(elemLLVM irtypes.Type) *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I64, irtypes.I64, elemLLVM)
}

// Arc struct field indices — T0157: weak_count added at field 1, value shifted to field 2.
const (
	arcFieldStrong = 0 // i64 strong_count
	arcFieldWeak   = 1 // i64 weak_count
	arcFieldValue  = 2 // T value
)

// genArcConstructor generates Arc[T](value) — allocates {strong_count, weak_count, T}, stores counts=1 and the value.
// T0155: Arc[T] atomic reference counting. T0157: weak_count added.
func (c *Compiler) genArcConstructor(e *ast.CallExpr, inst *types.Instance) value.Value {
	elemType := inst.TypeArgs()[0]
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}
	elemLLVM := c.resolveType(elemType)
	elemSize := c.typeSize(elemLLVM)

	// Allocate: 8 bytes strong_count + 8 bytes weak_count + sizeof(T)
	totalSize := 16 + elemSize
	arcPtr := c.block.NewCall(c.palAlloc, constant.NewInt(irtypes.I64, int64(totalSize)))

	// Bitcast to typed struct pointer for GEP
	arcStructTy := arcStructType(elemLLVM)
	typedPtr := c.block.NewBitCast(arcPtr, irtypes.NewPointer(arcStructTy))

	// Store strong_count = 1
	rcField := c.block.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldStrong))
	c.block.NewStore(constant.NewInt(irtypes.I64, 1), rcField)

	// Store weak_count = 1 (the +1 represents all strong refs collectively)
	wcField := c.block.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldWeak))
	c.block.NewStore(constant.NewInt(irtypes.I64, 1), wcField)

	// Generate and store value (moved into the Arc)
	val := c.genCallArgExpr(e.Args[0].Value)
	c.claimHeapTemp(val)
	// T0273: Clear drop flag — value is moved into Arc, caller must not double-drop.
	if ident, ok := e.Args[0].Value.(*ast.IdentExpr); ok {
		c.clearDropFlag(ident.Name)
	}
	c.neutralizeForceUnwrapSource(e.Args[0].Value)
	c.claimStringTemp(val)
	c.claimEnvTemp(val)
	valField := c.block.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldValue))
	c.block.NewStore(val, valField)

	return arcPtr
}

// genChannelMethodCall dispatches native method calls on channel[T].
func (c *Compiler) genChannelMethodCall(e *ast.CallExpr, member *ast.MemberExpr, elemType types.Type, method string) value.Value {
	chRaw := c.genExprAutoPropagate(member.Target) // B0323
	chanType := channelStructType()
	chPtr := c.block.NewBitCast(chRaw, irtypes.NewPointer(chanType))
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(c.typeSize(elemLLVM))

	switch method {
	case "send":
		return c.genChannelSend(e, chRaw, chPtr, chanType, elemLLVM, elemSize)
	case "close":
		return c.genChannelClose(chRaw, chPtr, chanType)
	default:
		panic(fmt.Sprintf("codegen: unknown channel method %q", method))
	}
}

// genArcBorrow generates the Arc .borrow getter — loads and returns the inner T value.
// The Arc layout is { i64 strong_count, i64 weak_count, T value }. We GEP to field 2 and load the value.
// T0155: Arc[T] atomic reference counting. T0157: weak_count shifted value to field 2.
func (c *Compiler) genArcBorrow(e *ast.MemberExpr, elemType types.Type) value.Value {
	arcRaw := c.genExprAutoPropagate(e.Target) // B0323
	elemLLVM := c.resolveType(elemType)
	arcStructTy := arcStructType(elemLLVM)
	typedPtr := c.block.NewBitCast(arcRaw, irtypes.NewPointer(arcStructTy))
	valField := c.block.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldValue))
	return c.block.NewLoad(elemLLVM, valField)
}

// genArcMethodCall dispatches native method calls on Arc[T].
// T0155: Arc[T] atomic reference counting. T0157: downgrade added.
func (c *Compiler) genArcMethodCall(e *ast.CallExpr, member *ast.MemberExpr, elemType types.Type, method string) value.Value {
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}
	// T0500: suppress the receiver field-read dup — clone()/downgrade() perform
	// their own atomic increment that produces the caller's owning reference.
	// Without this, `owner._arcField.clone()` increments strong_count twice
	// (dup + method body) but the caller registers only one matching drop,
	// leaking +1.
	savedDup := c.dupContainerFieldAccess
	c.dupContainerFieldAccess = false
	arcRaw := c.genExprAutoPropagate(member.Target) // B0323
	c.dupContainerFieldAccess = savedDup

	switch method {
	case "clone":
		// Atomically increment strong_count and return the same pointer
		rcPtr := c.block.NewBitCast(arcRaw, irtypes.NewPointer(irtypes.I64))
		c.emitAtomicAdd(c.block, rcPtr, constant.NewInt(irtypes.I64, 1), irtypes.I64)
		// T0499: Return a distinct SSA value so the clone result can be tracked
		// separately from the receiver's stmtTemp. Without this, stmtTemp dedup
		// causes the constructor intermediate to leak when used in a chain
		// (e.g., Arc[int](42).clone()). The ptrtoint+inttoptr is a no-op at
		// runtime — LLVM optimizes it away.
		tmpInt := c.block.NewPtrToInt(arcRaw, c.ptrIntType())
		return c.block.NewIntToPtr(tmpInt, irtypes.I8Ptr)
	case "downgrade":
		// T0157: Atomically increment weak_count, return same pointer as Weak[T]
		return c.genArcDowngrade(arcRaw, elemType)
	default:
		panic(fmt.Sprintf("codegen: unknown arc method %q", method))
	}
}

// genArcDowngrade generates Arc.downgrade() — increments weak_count and returns the pointer as Weak[T].
// T0157: Weak[T] references.
func (c *Compiler) genArcDowngrade(arcRaw value.Value, elemType types.Type) value.Value {
	elemLLVM := c.resolveType(elemType)
	arcStructTy := arcStructType(elemLLVM)
	typedPtr := c.block.NewBitCast(arcRaw, irtypes.NewPointer(arcStructTy))
	wcField := c.block.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldWeak))
	c.emitAtomicAdd(c.block, wcField, constant.NewInt(irtypes.I64, 1), irtypes.I64)
	// T0499: fresh SSA value so downgrade result is tracked separately from receiver stmtTemp
	tmpInt := c.block.NewPtrToInt(arcRaw, c.ptrIntType())
	return c.block.NewIntToPtr(tmpInt, irtypes.I8Ptr)
}

// --- Weak[T] codegen (T0157) ---

// genWeakMethodCall dispatches native method calls on Weak[T].
// T0157: Weak[T] references — upgrade and clone.
func (c *Compiler) genWeakMethodCall(e *ast.CallExpr, member *ast.MemberExpr, elemType types.Type, method string) value.Value {
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}
	// T0500: suppress the receiver field-read dup — clone()/upgrade() perform
	// their own atomic increment that produces the caller's owning reference.
	// Without this, `owner._weakField.clone()` increments weak_count twice
	// (dup + method body) but the caller registers only one matching drop,
	// leaking +1.
	savedDup := c.dupContainerFieldAccess
	c.dupContainerFieldAccess = false
	weakRaw := c.genExprAutoPropagate(member.Target) // B0323
	c.dupContainerFieldAccess = savedDup

	switch method {
	case "clone":
		// Atomically increment weak_count and return the same pointer
		elemLLVM := c.resolveType(elemType)
		arcStructTy := arcStructType(elemLLVM)
		typedPtr := c.block.NewBitCast(weakRaw, irtypes.NewPointer(arcStructTy))
		wcField := c.block.NewGetElementPtr(arcStructTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldWeak))
		c.emitAtomicAdd(c.block, wcField, constant.NewInt(irtypes.I64, 1), irtypes.I64)
		// T0499: fresh SSA value so clone result is tracked separately from receiver stmtTemp
		tmpInt := c.block.NewPtrToInt(weakRaw, c.ptrIntType())
		return c.block.NewIntToPtr(tmpInt, irtypes.I8Ptr)
	case "upgrade":
		// CAS loop: atomically try to increment strong_count if > 0
		return c.genWeakUpgrade(weakRaw, elemType)
	default:
		panic(fmt.Sprintf("codegen: unknown weak method %q", method))
	}
}

// genWeakUpgrade generates Weak.upgrade() — CAS loop on strong_count, returns Arc[T]?.
// T0157: Returns {i1, i8*} optional — Some(arc_ptr) if strong_count > 0, none otherwise.
func (c *Compiler) genWeakUpgrade(weakRaw value.Value, elemType types.Type) value.Value {
	elemLLVM := c.resolveType(elemType)
	arcStructTy := arcStructType(elemLLVM)
	optType := irtypes.NewStruct(irtypes.I1, irtypes.I8Ptr)

	typedPtr := c.block.NewBitCast(weakRaw, irtypes.NewPointer(arcStructTy))
	scField := c.block.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldStrong))

	if c.isWasm {
		// WASM: single-threaded, no atomics needed — simple load+compare+store
		old := c.block.NewLoad(irtypes.I64, scField)
		isZero := c.block.NewICmp(enum.IPredEQ, old, constant.NewInt(irtypes.I64, 0))
		noneBlk := c.newBlock("weak.upgrade.none")
		someBlk := c.newBlock("weak.upgrade.some")
		mergeBlk := c.newBlock("weak.upgrade.merge")
		c.block.NewCondBr(isZero, noneBlk, someBlk)

		c.block = noneBlk
		noneVal := constant.NewZeroInitializer(optType)
		noneBlk.NewBr(mergeBlk)

		c.block = someBlk
		newRC := someBlk.NewAdd(old, constant.NewInt(irtypes.I64, 1))
		someBlk.NewStore(newRC, scField)
		someVal := c.wrapOptional(weakRaw, optType)
		someBlk.NewBr(mergeBlk)

		c.block = mergeBlk
		return mergeBlk.NewPhi(
			ir.NewIncoming(noneVal, noneBlk),
			ir.NewIncoming(someVal, someBlk),
		)
	}

	// Native: CAS loop for thread safety
	//   loop:
	//     old = load atomic i64* strong_count acquire
	//     if old == 0: goto none
	//     new = old + 1
	//     {prev, ok} = cmpxchg i64* strong_count, old, new acq_rel monotonic
	//     if !ok: goto loop
	//     goto some
	loopBlk := c.newBlock("weak.upgrade.loop")
	noneBlk := c.newBlock("weak.upgrade.none")
	someBlk := c.newBlock("weak.upgrade.some")
	mergeBlk := c.newBlock("weak.upgrade.merge")
	c.block.NewBr(loopBlk)

	c.block = loopBlk
	old := loopBlk.NewLoad(irtypes.I64, scField)
	old.Atomic = true
	old.Ordering = enum.AtomicOrderingAcquire
	old.Align = 8 // LLVM requires explicit alignment for atomic load
	isZero := loopBlk.NewICmp(enum.IPredEQ, old, constant.NewInt(irtypes.I64, 0))
	casBlk := c.newBlock("weak.upgrade.cas")
	loopBlk.NewCondBr(isZero, noneBlk, casBlk)

	c.block = casBlk
	newRC := casBlk.NewAdd(old, constant.NewInt(irtypes.I64, 1))
	casResult := casBlk.NewCmpXchg(scField, old, newRC, enum.AtomicOrderingAcquireRelease, enum.AtomicOrderingMonotonic)
	casResult.Weak = false
	ok := casBlk.NewExtractValue(casResult, 1)
	casBlk.NewCondBr(ok, someBlk, loopBlk)

	c.block = noneBlk
	noneVal := constant.NewZeroInitializer(optType)
	noneBlk.NewBr(mergeBlk)

	c.block = someBlk
	someVal := c.wrapOptional(weakRaw, optType)
	someBlk.NewBr(mergeBlk)

	c.block = mergeBlk
	return mergeBlk.NewPhi(
		ir.NewIncoming(noneVal, noneBlk),
		ir.NewIncoming(someVal, someBlk),
	)
}

// --- Mutex[T] / MutexGuard[T] codegen (T0156) ---

// genMutexConstructor generates Mutex[T](value) — allocates scheduler-aware mutex struct, inits fields, stores value.
// Layout: {i8* pal_handle, i8* cond, i8* waiter_head, i8* waiter_tail, i8 held, T value}
// T0285: Scheduler-aware mutex — uses goroutine park/wake instead of blocking OS threads.
func (c *Compiler) genMutexConstructor(e *ast.CallExpr, inst *types.Instance) value.Value {
	elemType := inst.TypeArgs()[0]
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}
	elemLLVM := c.resolveType(elemType)

	mutexStructTy := mutexStructType(elemLLVM)
	mutexSize := c.typeSize(mutexStructTy)
	mutexPtr := c.block.NewCall(c.palAlloc, constant.NewInt(irtypes.I64, int64(mutexSize)))

	// Bitcast to typed struct pointer for GEP
	typedPtr := c.block.NewBitCast(mutexPtr, irtypes.NewPointer(mutexStructTy))

	// Field 0: PAL mutex handle (protects metadata only)
	mutexHandle := c.block.NewCall(c.palMutexInit)
	handleField := c.block.NewGetElementPtr(mutexStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewStore(mutexHandle, handleField)

	// Field 1: condition variable (for non-coroutine waiters)
	condHandle := c.block.NewCall(c.palCondInit)
	condField := c.block.NewGetElementPtr(mutexStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	c.block.NewStore(condHandle, condField)

	// Field 2: waiter_head = null
	headField := c.block.NewGetElementPtr(mutexStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
	c.block.NewStore(constant.NewNull(irtypes.I8Ptr), headField)

	// Field 3: waiter_tail = null
	tailField := c.block.NewGetElementPtr(mutexStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 3))
	c.block.NewStore(constant.NewNull(irtypes.I8Ptr), tailField)

	// Field 4: held = 0 (unlocked)
	heldField := c.block.NewGetElementPtr(mutexStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 4))
	c.block.NewStore(constant.NewInt(irtypes.I8, 0), heldField)

	// Field 5: user value (moved into the Mutex)
	val := c.genCallArgExpr(e.Args[0].Value)
	c.claimHeapTemp(val)
	// T0273: Clear drop flag — value is moved into Mutex, caller must not double-drop.
	if ident, ok := e.Args[0].Value.(*ast.IdentExpr); ok {
		c.clearDropFlag(ident.Name)
	}
	c.neutralizeForceUnwrapSource(e.Args[0].Value)
	c.claimStringTemp(val)
	c.claimEnvTemp(val)
	valField := c.block.NewGetElementPtr(mutexStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldValue)))
	c.block.NewStore(val, valField)

	return mutexPtr
}

// genMutexMethodCall dispatches native method calls on Mutex[T].
func (c *Compiler) genMutexMethodCall(e *ast.CallExpr, member *ast.MemberExpr, elemType types.Type, method string) value.Value {
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}
	mutexRaw := c.genExprAutoPropagate(member.Target)

	switch method {
	case "lock":
		// T0655: a single-owner Mutex *temp* receiver would be dropped at
		// statement end before the MutexGuard that borrows it → UAF. Promote
		// it to a scope binding so it outlives the guard, mirroring the
		// already-correct bound-receiver path. No-op for bound receivers
		// (mutexRaw is a fresh load, not a tracked stmt-temp).
		mtxType := c.info.Types[member.Target]
		if c.typeSubst != nil && mtxType != nil {
			mtxType = types.Substitute(mtxType, c.typeSubst)
		}
		c.promoteHandleTempToScopeBinding(mutexRaw, c.getOrCreateMutexDrop(elemType), mtxType)
		return c.genMutexLock(mutexRaw, elemType)
	default:
		panic(fmt.Sprintf("codegen: unknown mutex method %q", method))
	}
}

// genMutexLock generates Mutex.lock() — scheduler-aware lock, returns a MutexGuard.
// T0285: Coroutine path uses goroutine park/wake; non-coroutine path uses cond_wait.
// Guard layout: {i8* mutex_alloc_ptr}.
func (c *Compiler) genMutexLock(mutexRaw value.Value, elemType types.Type) value.Value {
	metaTy := mutexMetaType()
	typedPtr := c.block.NewBitCast(mutexRaw, irtypes.NewPointer(metaTy))

	// Load PAL mutex handle and enter critical section
	handleField := c.block.NewGetElementPtr(metaTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldHandle)))
	handle := c.block.NewLoad(irtypes.I8Ptr, handleField)
	c.block.NewCall(c.palMutexLock, handle)

	// Load held flag and waiter head. Acquired iff held==0 AND waiter_head==null.
	// Queuing behind existing waiters prevents newcomer starvation under contention:
	// pthread_mutex is not FIFO, so an arrival that races with a handoff could
	// otherwise win the PAL handle repeatedly and starve parked waiters (T0301).
	heldField := c.block.NewGetElementPtr(metaTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldHeld)))
	held := c.block.NewLoad(irtypes.I8, heldField)
	isHeld := c.block.NewICmp(enum.IPredEQ, held, constant.NewInt(irtypes.I8, 1))

	waiterHeadReadField := c.block.NewGetElementPtr(metaTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldWaiterHead)))
	waiterHeadRead := c.block.NewLoad(irtypes.I8Ptr, waiterHeadReadField)
	hasWaiter := c.block.NewICmp(enum.IPredNE, waiterHeadRead, constant.NewNull(irtypes.I8Ptr))
	mustWait := c.block.NewOr(isHeld, hasWaiter)

	acquiredBlk := c.newBlock("mutex.acquired")
	contestedBlk := c.newBlock("mutex.contested")
	c.block.NewCondBr(mustWait, contestedBlk, acquiredBlk)

	// acquired: held=0 → set held=1, unlock metadata mutex, allocate guard
	c.block = acquiredBlk
	acquiredHeldField := c.block.NewGetElementPtr(metaTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldHeld)))
	c.block.NewStore(constant.NewInt(irtypes.I8, 1), acquiredHeldField)
	c.block.NewCall(c.palMutexUnlock, handle)

	guardBlk := c.newBlock("mutex.guard")
	c.block.NewBr(guardBlk)

	// contested: held=1 → need to wait
	c.block = contestedBlk
	if c.inCoroutine {
		// Goroutine mode: park on mutex waiter list (park-and-wake, not spin-yield).
		// PAL handle is still locked at entry here.
		currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		waiterHeadField := c.block.NewGetElementPtr(metaTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldWaiterHead)))
		waiterTailField := c.block.NewGetElementPtr(metaTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldWaiterTail)))
		c.block.NewCall(c.funcs["promise_waiter_enqueue"], waiterHeadField, waiterTailField, currentG)
		// Store mutex in G.park_mutex — scheduler releases after coro.suspend completes
		gTyMtx := goroutineStructType()
		mtxGPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTyMtx))
		mtxPmField := c.block.NewGetElementPtr(gTyMtx, mtxGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
		c.block.NewStore(handle, mtxPmField)

		suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		parkResumeBlk := c.newBlock("mutex.park.resume")
		c.block.NewSwitch(suspResult, c.coroSuspendBlk,
			ir.NewCase(constant.NewInt(irtypes.I8, 0), parkResumeBlk),
			ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

		// Resume: lock was handed off (held=1 already), go directly to guardBlk
		c.block = parkResumeBlk
		c.block.NewBr(guardBlk)
	} else {
		// Non-coroutine mode: cond_wait loop
		condField := c.block.NewGetElementPtr(metaTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldCond)))
		cond := c.block.NewLoad(irtypes.I8Ptr, condField)

		waitLoopBlk := c.newBlock("mutex.wait.loop")
		c.block.NewBr(waitLoopBlk)

		c.block = waitLoopBlk
		loopHeldField := c.block.NewGetElementPtr(metaTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldHeld)))
		loopHeld := c.block.NewLoad(irtypes.I8, loopHeldField)
		stillHeld := c.block.NewICmp(enum.IPredEQ, loopHeld, constant.NewInt(irtypes.I8, 1))

		waitBodyBlk := c.newBlock("mutex.wait.body")
		waitDoneBlk := c.newBlock("mutex.wait.done")
		c.block.NewCondBr(stillHeld, waitBodyBlk, waitDoneBlk)

		c.block = waitBodyBlk
		c.block.NewCall(c.palCondWait, cond, handle)
		c.block.NewBr(waitLoopBlk)

		c.block = waitDoneBlk
		doneHeldField := c.block.NewGetElementPtr(metaTy, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldHeld)))
		c.block.NewStore(constant.NewInt(irtypes.I8, 1), doneHeldField)
		c.block.NewCall(c.palMutexUnlock, handle)
		c.block.NewBr(guardBlk)
	}

	// Allocate guard: {i8*} — pointer back to the Mutex allocation
	c.block = guardBlk
	guardPtr := c.block.NewCall(c.palAlloc, constant.NewInt(irtypes.I64, 8))
	guardStructTy := irtypes.NewStruct(irtypes.I8Ptr)
	guardTypedPtr := c.block.NewBitCast(guardPtr, irtypes.NewPointer(guardStructTy))
	mutexField := c.block.NewGetElementPtr(guardStructTy, guardTypedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewStore(mutexRaw, mutexField)

	return guardPtr
}

// genMutexGuardBorrow generates the MutexGuard .borrow getter — loads T from the Mutex through the guard.
// Guard layout: {i8* mutex_alloc_ptr}. Mutex layout: {i8* pal_handle, i8* cond, i8* waiter_head, i8* waiter_tail, i8 held, T value}.
func (c *Compiler) genMutexGuardBorrow(e *ast.MemberExpr, elemType types.Type) value.Value {
	guardRaw := c.genExprAutoPropagate(e.Target)
	elemLLVM := c.resolveType(elemType)

	// Load mutex_alloc_ptr from guard (field 0)
	guardStructTy := irtypes.NewStruct(irtypes.I8Ptr)
	guardPtr := c.block.NewBitCast(guardRaw, irtypes.NewPointer(guardStructTy))
	mutexPtrField := c.block.NewGetElementPtr(guardStructTy, guardPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	mutexRaw := c.block.NewLoad(irtypes.I8Ptr, mutexPtrField)

	// Load T from Mutex field 5 (value)
	mutexStructTy := mutexStructType(elemLLVM)
	mutexPtr := c.block.NewBitCast(mutexRaw, irtypes.NewPointer(mutexStructTy))
	valField := c.block.NewGetElementPtr(mutexStructTy, mutexPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldValue)))
	return c.block.NewLoad(elemLLVM, valField)
}

// genMutexGuardBorrowSet generates the MutexGuard .borrow setter — stores T into the Mutex through the guard.
// Handles compound assignment (+=, -=, etc.) by reading the current value first.
// srcExpr (may be nil) is the RHS source AST; used by the T0351 defensive dup
// path to detect a borrow-param string and dup it before store.
func (c *Compiler) genMutexGuardBorrowSet(target *ast.MemberExpr, op ast.AssignOp, val value.Value, elemType types.Type, srcExpr ast.Expr) {
	guardRaw := c.genExpr(target.Target)
	elemLLVM := c.resolveType(elemType)

	// Navigate to the value field: guard → mutex_alloc_ptr → Mutex.value
	guardStructTy := irtypes.NewStruct(irtypes.I8Ptr)
	guardPtr := c.block.NewBitCast(guardRaw, irtypes.NewPointer(guardStructTy))
	mutexPtrField := c.block.NewGetElementPtr(guardStructTy, guardPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	mutexRaw := c.block.NewLoad(irtypes.I8Ptr, mutexPtrField)

	mutexStructTy := mutexStructType(elemLLVM)
	mutexPtr := c.block.NewBitCast(mutexRaw, irtypes.NewPointer(mutexStructTy))
	valField := c.block.NewGetElementPtr(mutexStructTy, mutexPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldValue)))

	// Handle compound assignment
	if op != ast.OpAssign {
		current := c.block.NewLoad(elemLLVM, valField)
		val = c.genCompoundOp(op, elemType, current, val)
	}

	// Drop old value if T is droppable (T0270)
	c.block = c.emitInnerDrop(c.block, mutexPtr, mutexStructTy, elemType, mutexFieldValue)

	// T0351: defensive dup for borrow-param strings. The sema layer already
	// rejects `g.borrow = borrow_param` via tryMoveConsume in checkAssignStmt,
	// but mirror the T0095 string-field setter pattern as a runtime safety net
	// in case a future codegen path bypasses sema.
	if op == ast.OpAssign && srcExpr != nil && extractNamed(elemType) == types.TypString {
		if ident, ok := srcExpr.(*ast.IdentExpr); ok {
			if _, hasFlag := c.dropFlags[ident.Name]; hasFlag {
				c.clearDropFlag(ident.Name)
			} else {
				val = c.dupString(val)
			}
		}
	}

	c.block.NewStore(val, valField)
}

// genChannelSend generates code for ch.send(value).
// lock → wait-if-full → memcpy to buffer → signal → rendezvous wait if unbuffered → unlock
func (c *Compiler) genChannelSend(e *ast.CallExpr, chRaw value.Value, chPtr value.Value, chanType *irtypes.StructType, elemLLVM irtypes.Type, elemSize int64) value.Value {
	// Load mutex
	mtxFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
	mtx := c.block.NewLoad(irtypes.I8Ptr, mtxFieldPtr)

	// Load cond vars
	neFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotEmpty)))
	notEmpty := c.block.NewLoad(irtypes.I8Ptr, neFieldPtr)

	nfFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
	notFull := c.block.NewLoad(irtypes.I8Ptr, nfFieldPtr)

	// Lock mutex
	c.block.NewCall(c.palMutexLock, mtx)

	// Check closed before sending — panic if channel is closed
	closedPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
	closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
	isClosed := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 1))

	sendClosedPanicBlock := c.newBlock("send.closed.panic")
	sendOkBlock := c.newBlock("send.ok")
	c.block.NewCondBr(isClosed, sendClosedPanicBlock, sendOkBlock)

	c.block = sendClosedPanicBlock
	c.block.NewCall(c.palMutexUnlock, mtx)
	panicMsg := c.makeGlobalString("send on closed channel")
	c.block.NewCall(c.funcs["promise_panic"], panicMsg)
	c.emitPanicReturn()

	c.block = sendOkBlock

	// Wait while full: while count == capacity
	waitFullBlock := c.newBlock("send.waitfull")
	waitFullClosedBlock := c.newBlock("send.waitfull.closed")
	writeBlock := c.newBlock("send.write")

	c.block.NewBr(waitFullBlock)

	// waitfull: check count == capacity
	c.block = waitFullBlock
	countPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
	count := c.block.NewLoad(irtypes.I64, countPtr)
	capPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
	cap_ := c.block.NewLoad(irtypes.I64, capPtr)
	isFull := c.block.NewICmp(enum.IPredEQ, count, cap_)

	waitFullBodyBlock := c.newBlock("send.waitfull.body")
	c.block.NewCondBr(isFull, waitFullBodyBlock, writeBlock)

	if c.inCoroutine {
		// Goroutine mode: park on send_waiters + coro.suspend
		c.block = waitFullBodyBlock
		currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		sendHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
		sendTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
		c.block.NewCall(c.funcs["promise_waiter_enqueue"], sendHeadPtr, sendTailPtr, currentG)
		// Store mutex in G.park_mutex — scheduler releases after coro.suspend completes
		gTySend := goroutineStructType()
		sendGPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTySend))
		sendPmField := c.block.NewGetElementPtr(gTySend, sendGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
		c.block.NewStore(mtx, sendPmField)

		suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		resumeBlk := c.newBlock("send.wait.resume")
		c.block.NewSwitch(suspResult, c.coroSuspendBlk,
			ir.NewCase(constant.NewInt(irtypes.I8, 0), resumeBlk),
			ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

		// On resume: re-lock and check closed, then retry
		c.block = resumeBlk
		c.block.NewCall(c.palMutexLock, mtx)
		closedAfterWait := c.block.NewLoad(irtypes.I8, closedPtr)
		isClosedAfterWait := c.block.NewICmp(enum.IPredEQ, closedAfterWait, constant.NewInt(irtypes.I8, 1))
		c.block.NewCondBr(isClosedAfterWait, waitFullClosedBlock, waitFullBlock)
	} else {
		// Thread-blocking mode: cond_wait, then re-check closed flag
		c.block = waitFullBodyBlock
		c.block.NewCall(c.palCondWait, notFull, mtx)
		closedAfterWait := c.block.NewLoad(irtypes.I8, closedPtr)
		isClosedAfterWait := c.block.NewICmp(enum.IPredEQ, closedAfterWait, constant.NewInt(irtypes.I8, 1))
		c.block.NewCondBr(isClosedAfterWait, waitFullClosedBlock, waitFullBlock)
	}

	// waitfull.closed: channel was closed while we were waiting — panic
	c.block = waitFullClosedBlock
	c.block.NewCall(c.palMutexUnlock, mtx)
	panicMsg2 := c.makeGlobalString("send on closed channel")
	c.block.NewCall(c.funcs["promise_panic"], panicMsg2)
	c.emitPanicReturn()

	// write: memcpy value into buffer[tail * elem_size]
	c.block = writeBlock

	// Alloca value and store (entry-block alloca to avoid stack growth in loops)
	argVal := c.genCallArgExpr(e.Args[0].Value)
	// Clear drop flag: value is moved into the channel buffer
	if ident, ok := e.Args[0].Value.(*ast.IdentExpr); ok {
		c.clearDropFlag(ident.Name)
	}
	// B0170: claim string temp — ownership transfers to channel buffer
	c.claimStringTemp(argVal)
	// B0233: claim heap temp — ownership transfers to channel buffer
	c.claimHeapTemp(argVal)
	argAlloca := c.createEntryAlloca(elemLLVM)
	c.block.NewStore(argVal, argAlloca)
	argAsI8 := c.block.NewBitCast(argAlloca, irtypes.I8Ptr)

	// Calculate dest = buffer + tail * elem_size
	bufPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldBuffer)))
	buf := c.block.NewLoad(irtypes.I8Ptr, bufPtr)
	tailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldTail)))
	tail := c.block.NewLoad(irtypes.I64, tailPtr)
	offset := c.block.NewMul(tail, constant.NewInt(irtypes.I64, elemSize))
	dest := c.block.NewGetElementPtr(irtypes.I8, buf, offset)

	// memcpy(dest, &value, elem_size)
	c.block.NewCall(c.funcs["llvm.memcpy"], dest, argAsI8,
		constant.NewInt(irtypes.I64, elemSize), constant.False)

	// tail = (tail + 1) % capacity
	capReload := c.block.NewLoad(irtypes.I64, capPtr)
	tailPlusOne := c.block.NewAdd(tail, constant.NewInt(irtypes.I64, 1))
	newTail := c.block.NewURem(tailPlusOne, capReload)
	c.block.NewStore(newTail, tailPtr)

	// count++
	countReload := c.block.NewLoad(irtypes.I64, countPtr)
	newCount := c.block.NewAdd(countReload, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(newCount, countPtr)

	// Wake a waiting receiver (handles both regular G and select SWN nodes)
	recvHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
	recvTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
	c.block.NewCall(c.funcs["promise_waiter_wake_one"], recvHeadPtr, recvTailPtr, notEmpty)

	// If unbuffered: wait until receiver picks up the value
	unbufPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldUnbuffered)))
	unbufVal := c.block.NewLoad(irtypes.I8, unbufPtr)
	isUnbuf := c.block.NewICmp(enum.IPredEQ, unbufVal, constant.NewInt(irtypes.I8, 1))

	rendezvousBlock := c.newBlock("send.rendezvous")
	doneBlock := c.newBlock("send.done")
	c.block.NewCondBr(isUnbuf, rendezvousBlock, doneBlock)

	// rendezvous: wait while count > 0 && !closed
	c.block = rendezvousBlock
	rendezvousCheckBlock := c.newBlock("send.rv.check")
	c.block.NewBr(rendezvousCheckBlock)

	c.block = rendezvousCheckBlock
	rvCount := c.block.NewLoad(irtypes.I64, countPtr)
	rvHasItems := c.block.NewICmp(enum.IPredUGT, rvCount, constant.NewInt(irtypes.I64, 0))
	rvClosedVal := c.block.NewLoad(irtypes.I8, closedPtr)
	isOpen := c.block.NewICmp(enum.IPredEQ, rvClosedVal, constant.NewInt(irtypes.I8, 0))
	shouldWait := c.block.NewAnd(rvHasItems, isOpen)

	rendezvousWaitBlock := c.newBlock("send.rv.wait")
	// When rendezvous exits (count==0 or closed), wake one write-waiter from
	// send_waiters so it can write to the now-empty buffer (B0156, T0305).
	rendezvousExitBlock := c.newBlock("send.rv.exit")
	c.block.NewCondBr(shouldWait, rendezvousWaitBlock, rendezvousExitBlock)

	if c.inCoroutine {
		// Goroutine mode rendezvous: park on rv_waiters (T0312).
		// Enqueue G on rv_waiters while ch.mutex is locked, then set park_mutex so
		// the scheduler unlocks ch.mutex after coro.suspend completes. The receiver
		// wakes us (via wake_one(rv_waiters)) only after count-- (count==0), so no
		// re-check is needed on resume — go directly to rendezvousExitBlock.
		c.block = rendezvousWaitBlock
		rvCurrentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		rvHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRvWaitersHead)))
		rvTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRvWaitersTail)))
		c.block.NewCall(c.funcs["promise_waiter_enqueue"], rvHeadPtr, rvTailPtr, rvCurrentG)
		rvGTy := goroutineStructType()
		rvGPtr := c.block.NewBitCast(rvCurrentG, irtypes.NewPointer(rvGTy))
		rvPmField := c.block.NewGetElementPtr(rvGTy, rvGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
		c.block.NewStore(mtx, rvPmField)
		rvSuspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		rvResumeBlk := c.newBlock("send.rv.resume")
		c.block.NewSwitch(rvSuspResult, c.coroSuspendBlk,
			ir.NewCase(constant.NewInt(irtypes.I8, 0), rvResumeBlk),
			ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

		// Scheduler unlocked ch.mutex via park_mutex; re-lock to proceed.
		c.block = rvResumeBlk
		c.block.NewCall(c.palMutexLock, mtx)
		c.block.NewBr(rendezvousExitBlock)
	} else {
		// Thread-blocking mode rendezvous: cond_wait
		c.block = rendezvousWaitBlock
		c.block.NewCall(c.palCondWait, notFull, mtx)
		c.block.NewBr(rendezvousCheckBlock)
	}

	// rendezvous exit: wake one write-waiter from send_waiters (T0305/T0312).
	// rv_waiters holds rendezvous-parked senders; send_waiters holds only genuine
	// write-waiters and select SWNs, so waking it here is safe.
	c.block = rendezvousExitBlock
	rvExitSendHead := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
	rvExitSendTail := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
	rvExitNfPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
	rvExitNf := c.block.NewLoad(irtypes.I8Ptr, rvExitNfPtr)
	c.block.NewCall(c.funcs["promise_waiter_wake_one"], rvExitSendHead, rvExitSendTail, rvExitNf)
	c.block.NewBr(doneBlock)

	// done: unlock
	c.block = doneBlock
	c.block.NewCall(c.palMutexUnlock, mtx)

	return nil
}

// genChannelClose generates code for ch.close().
// lock → set closed=1 → broadcast both conds → unlock
func (c *Compiler) genChannelClose(chRaw value.Value, chPtr value.Value, chanType *irtypes.StructType) value.Value {
	// Load mutex
	mtxFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
	mtx := c.block.NewLoad(irtypes.I8Ptr, mtxFieldPtr)

	// Lock
	c.block.NewCall(c.palMutexLock, mtx)

	// Check if already closed — panic on double-close
	closedPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
	closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
	alreadyClosed := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 1))

	doubleClosePanic := c.newBlock("close.panic")
	closeOk := c.newBlock("close.ok")
	c.block.NewCondBr(alreadyClosed, doubleClosePanic, closeOk)

	c.block = doubleClosePanic
	c.block.NewCall(c.palMutexUnlock, mtx)
	panicMsg := c.makeGlobalString("close of closed channel")
	c.block.NewCall(c.funcs["promise_panic"], panicMsg)
	c.emitPanicReturn()

	c.block = closeOk

	// Set closed = 1
	c.block.NewStore(constant.NewInt(irtypes.I8, 1), closedPtr)

	// Wake all goroutine waiters (send + recv)
	sendHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
	sendTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
	c.block.NewCall(c.funcs["promise_waiter_wake_all"], sendHeadPtr, sendTailPtr)

	recvHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
	recvTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
	c.block.NewCall(c.funcs["promise_waiter_wake_all"], recvHeadPtr, recvTailPtr)

	// Wake all rendezvous-parked senders (T0312): channel closed while they waited
	closeRvHead := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRvWaitersHead)))
	closeRvTail := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRvWaitersTail)))
	c.block.NewCall(c.funcs["promise_waiter_wake_all"], closeRvHead, closeRvTail)

	// Broadcast both cond vars to wake thread-blocked waiters
	neFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotEmpty)))
	notEmpty := c.block.NewLoad(irtypes.I8Ptr, neFieldPtr)
	c.block.NewCall(c.palCondBroadcast, notEmpty)

	nfFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
	notFull := c.block.NewLoad(irtypes.I8Ptr, nfFieldPtr)
	c.block.NewCall(c.palCondBroadcast, notFull)

	// Unlock
	c.block.NewCall(c.palMutexUnlock, mtx)

	return nil
}

// genVectorLen loads the length from a vector/array header (masking off bit 63 static flag).
func (c *Compiler) genVectorLen(e *ast.MemberExpr) value.Value {
	slicePtr := c.genExprAutoPropagate(e.Target) // B0323
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(slicePtr, irtypes.NewPointer(headerType))
	return loadVectorLen(c.block, headerPtr)
}

// genMapLen returns the length of a map via the runtime.
// genNativeHashGetter emits native hash computation for primitive types.
// Returns (value, true) if the type has a native hash getter, (nil, false) otherwise.
// All primitive hashes use the Promise-implemented _fnv1a_hash function.
// String hash uses a codegen-emitted LLVM IR function (__promise_hash_string).
func (c *Compiler) genNativeHashGetter(e *ast.MemberExpr, named *types.Named) (value.Value, bool) {
	target := c.genExprAutoPropagate(e.Target) // B0323
	hashFn := c.funcs["_fnv1a_hash"]
	switch named {
	case types.TypInt, types.TypI64, types.TypUint, types.TypU64:
		// Already i64 — call _fnv1a_hash directly
		return c.block.NewCall(hashFn, target), true
	case types.TypI32:
		ext := c.block.NewSExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypU32:
		ext := c.block.NewZExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypI16:
		ext := c.block.NewSExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypU16:
		ext := c.block.NewZExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypI8:
		ext := c.block.NewSExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypU8:
		ext := c.block.NewZExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypBool:
		// Hardcoded hash constants for bool (avoids hashing through fnv1a)
		trueHash := constant.NewInt(irtypes.I64, 0x517cc1b727220a95)
		falseHash := constant.NewInt(irtypes.I64, 0x6c62272e07bb0142)
		return c.block.NewSelect(target, trueHash, falseHash), true
	case types.TypChar:
		ext := c.block.NewZExt(target, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypF64:
		// Bitcast double to i64 bits, then hash via Promise _fnv1a_hash
		bits := c.block.NewBitCast(target, irtypes.I64)
		return c.block.NewCall(hashFn, bits), true
	case types.TypF32:
		// Bitcast float to i32 bits, zero-extend to i64, then hash
		bits := c.block.NewBitCast(target, irtypes.I32)
		ext := c.block.NewZExt(bits, irtypes.I64)
		return c.block.NewCall(hashFn, ext), true
	case types.TypString:
		// String hash uses codegen-emitted LLVM IR function
		return c.block.NewCall(c.funcs["__promise_hash_string"], target), true
	default:
		return nil, false
	}
}

// ownerHasOrSynthDrop returns true if the field owner needs cleanup at drop
// time. Covers explicit drop, sema-detected synth drop, and mono-detected synth
// drop on generic instances (T0513): the Named origin has HasDrop=false for
// generic types like Box[T] { T? value } because sema's fieldTypeHasDrop
// returns false for TypeParam fields. The concrete instance (e.g. Box[string])
// gets a synthesized drop via monoInstNeedsSynthDrop at codegen time, so the
// dup-on-read paths must also check that signal.
func (c *Compiler) ownerHasOrSynthDrop(typ types.Type, named *types.Named) bool {
	if named != nil && (named.HasDrop() || named.NeedsSynthDrop()) {
		return true
	}
	if inst, ok := typ.(*types.Instance); ok {
		return monoInstNeedsSynthDrop(inst)
	}
	return false
}

// genFieldAccess loads a field value from a user type instance.
// Uses lookupTypeLayout for layout-driven field types that work for both
// regular and monomorphic types.
func (c *Compiler) genFieldAccess(e *ast.MemberExpr, typ types.Type, field *types.Field) value.Value {
	layout := c.lookupTypeLayout(typ)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for type %s", typ))
	}

	// Value types: fields are in the value struct, not an instance struct
	if layout.IsValueType {
		fieldIdx, ok := layout.ValueFieldIndex[field.Name()]
		if !ok {
			panic(fmt.Sprintf("codegen: field %s not in value layout for %s", field.Name(), typ))
		}
		targetVal := c.genExprAutoPropagate(e.Target) // B0323
		// `this` in value type methods is an i8* pointing to value struct
		if _, isThis := e.Target.(*ast.ThisExpr); isThis {
			valuePtrType := irtypes.NewPointer(layout.Value.LLVMType)
			typedPtr := c.block.NewBitCast(targetVal, valuePtrType)
			fieldPtr := c.block.NewGetElementPtr(layout.Value.LLVMType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			return c.block.NewLoad(layout.Value.Fields[fieldIdx].LLVMType, fieldPtr)
		}
		// For non-this targets, the value is the full value struct — extractvalue
		return c.block.NewExtractValue(targetVal, uint64(fieldIdx))
	}

	fieldIdx, ok := layout.InstanceFieldIndex[field.Name()]
	if !ok {
		panic(fmt.Sprintf("codegen: field %s not in instance layout for %s", field.Name(), typ))
	}

	targetVal := c.genExprAutoPropagate(e.Target) // B0323
	// `this` in methods is already an i8* instance pointer, not a value struct
	var instance value.Value
	if _, isThis := e.Target.(*ast.ThisExpr); isThis {
		instance = targetVal
	} else {
		instance = c.extractInstancePtr(targetVal)
		// B0325: Track heap instance when target is a temporary (call result,
		// error unwrap). Without this, field access on temporaries like
		// make_pair().x or make_pair()?!.x leaks the instance.
		c.trackChainIntermediateReceiver(e.Target, targetVal, instance, extractNamed(typ), typ)
	}
	typedPtr := c.block.NewBitCast(instance, layout.InstancePtrType)

	fieldPtr := c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))

	// Use layout field type (not llvmType(field.Type()) which fails for TypeParams)
	val := c.block.NewLoad(layout.Instance.Fields[fieldIdx].LLVMType, fieldPtr)

	// T0095/T0110: Dup string fields from types with drop to prevent double-free.
	// Only dup when the caller needs ownership (VarDecl, block result, etc.),
	// signaled by c.dupStringFieldAccess. Temporary uses (comparisons, function
	// args) don't dup — the type is alive during the expression evaluation.
	if c.tempTrackingEnabled {
		fType := field.Type()
		if c.typeSubst != nil {
			fType = types.Substitute(fType, c.typeSubst)
		}
		// T0513: Also substitute using the Instance's TypeArgs so generic field
		// types like T? on Box[T] resolve to string? when accessed on Box[string].
		// Without this, the dup checks below see TypeParam and skip the dup,
		// leaving the field aliased between the owner and the new variable.
		if inst, ok := typ.(*types.Instance); ok {
			if origin, ok := inst.Origin().(*types.Named); ok && len(origin.TypeParams()) > 0 {
				localSubst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
				fType = types.Substitute(fType, localSubst)
			}
		}
		ownerNamed := extractNamed(typ)
		if c.dupStringFieldAccess {
			if extractNamed(fType) == types.TypString && c.ownerHasOrSynthDrop(typ, ownerNamed) {
				c.dupStringFieldAccess = false // consume the flag
				dup := c.dupString(val)
				c.trackStringTemp(dup)
				return dup
			}
			// B0181: Handle string? optional fields — extractNamed returns nil for
			// *types.Optional, so unwrap first to check the inner type.
			// B0190: Track the dup as a temp AND store it in optionalStringDup so
			// genOptionalForceUnwrap can return it directly (bypassing extractvalue
			// which creates a different value.Value that claimStringTemp can't match).
			if opt, ok := fType.(*types.Optional); ok && extractNamed(opt.Elem()) == types.TypString && c.ownerHasOrSynthDrop(typ, ownerNamed) {
				c.dupStringFieldAccess = false // consume the flag
				innerStr := c.block.NewExtractValue(val, 1)
				dup := c.dupString(innerStr)
				c.trackStringTemp(dup)
				c.optionalStringDup = dup
				return c.block.NewInsertValue(val, dup, 1)
			}
		}
		// B0219: Dup vector/channel fields from types with drop.
		// Vector: shallow copy (allocate + memcpy). Channel: incref.
		if c.dupContainerFieldAccess && c.ownerHasOrSynthDrop(typ, ownerNamed) {
			if elemType, ok := types.AsVector(fType); ok {
				c.dupContainerFieldAccess = false // consume the flag
				elemLLVM := c.resolveType(elemType)
				elemSize := int64(c.typeSize(elemLLVM))
				dup := c.dupVector(val, elemSize)
				// T0540: Deep-clone droppable elements so the dup owns independent
				// copies. Without this, the shallow memcpy aliases element pointers
				// between the original field and the dup, causing a double-free at
				// scope end. No-op for primitive/copy element types.
				c.emitVectorElementCloneLoop(dup, elemType)
				// The dup now owns its elements independently; statement-end cleanup
				// must drop the elements before freeing the buffer.
				c.trackVectorTempWithElemType(dup, elemType)
				return dup
			}
			if chElem, isCh := types.AsChannel(fType); isCh {
				c.dupContainerFieldAccess = false // consume the flag
				dup := c.dupChannel(val)
				c.trackChannelTempWithElemType(dup, chElem) // T0663
				return dup
			}
			if arcElem, ok := types.AsArc(fType); ok {
				c.dupContainerFieldAccess = false // consume the flag
				dup := c.dupArc(val)
				resolvedArcElem := arcElem
				if c.typeSubst != nil {
					resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
				}
				c.trackTempWithDrop(dup, c.getOrCreateArcDrop(resolvedArcElem))
				return dup
			}
			if weakElem, ok := types.AsWeak(fType); ok {
				c.dupContainerFieldAccess = false // consume the flag
				resolvedWeakElem := weakElem
				if c.typeSubst != nil {
					resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
				}
				dup := c.dupWeak(val, resolvedWeakElem)
				c.trackTempWithDrop(dup, c.getOrCreateWeakDrop(resolvedWeakElem))
				return dup
			}
			// T0366: Optional[Vector|Channel|Arc|Weak] fields — dup the inner buffer
			// so the new optional owns an independent copy. Without this, both the
			// source's owner drop and the new variable's optional drop free the same
			// buffer (mirrors the Optional[String] handling at lines 3635–3642).
			// optionalContainerDup is consumed by genVarDecl (and similar sites) to
			// claim the dup temp once the containing optional is bound to a variable.
			if opt, ok := fType.(*types.Optional); ok {
				elem := opt.Elem()
				if elemType, isVec := types.AsVector(elem); isVec {
					c.dupContainerFieldAccess = false
					elemLLVM := c.resolveType(elemType)
					elemSize := int64(c.typeSize(elemLLVM))
					innerVec := c.block.NewExtractValue(val, 1)
					dup := c.dupVector(innerVec, elemSize)
					// T0540: Deep-clone droppable elements (mirror the bare Vector branch above).
					c.emitVectorElementCloneLoop(dup, elemType)
					c.trackVectorTempWithElemType(dup, elemType)
					c.optionalContainerDup = dup
					return c.block.NewInsertValue(val, dup, 1)
				}
				if chElem, isCh := types.AsChannel(elem); isCh {
					c.dupContainerFieldAccess = false
					innerCh := c.block.NewExtractValue(val, 1)
					dup := c.dupChannel(innerCh)
					c.trackChannelTempWithElemType(dup, chElem) // T0663
					c.optionalContainerDup = dup
					return c.block.NewInsertValue(val, dup, 1)
				}
				if arcElem, isArc := types.AsArc(elem); isArc {
					c.dupContainerFieldAccess = false
					innerArc := c.block.NewExtractValue(val, 1)
					dup := c.dupArc(innerArc)
					resolvedArcElem := arcElem
					if c.typeSubst != nil {
						resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
					}
					c.trackTempWithDrop(dup, c.getOrCreateArcDrop(resolvedArcElem))
					c.optionalContainerDup = dup
					return c.block.NewInsertValue(val, dup, 1)
				}
				if weakElem, isWeak := types.AsWeak(elem); isWeak {
					c.dupContainerFieldAccess = false
					innerWeak := c.block.NewExtractValue(val, 1)
					resolvedWeakElem := weakElem
					if c.typeSubst != nil {
						resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
					}
					dup := c.dupWeak(innerWeak, resolvedWeakElem)
					c.trackTempWithDrop(dup, c.getOrCreateWeakDrop(resolvedWeakElem))
					c.optionalContainerDup = dup
					return c.block.NewInsertValue(val, dup, 1)
				}
			}
		}
		// B0190: Signal that this field access loaded a string? field from a
		// droppable type. genOptionalForceUnwrap's result should NOT be tracked
		// as a temp (the owner's drop handles the string's lifetime).
		if opt, ok := fType.(*types.Optional); ok && extractNamed(opt.Elem()) == types.TypString && c.ownerHasOrSynthDrop(typ, ownerNamed) {
			c.optionalFieldString = true
		}
		// T0354: Same for vector fields — the owner's drop frees the inner Vector
		// via optfield.drop. Suppress unwrap-path stmt-temp tracking to avoid
		// double-free at statement end.
		if opt, ok := fType.(*types.Optional); ok && c.ownerHasOrSynthDrop(typ, ownerNamed) {
			if _, isVec := types.AsVector(opt.Elem()); isVec {
				c.optionalFieldVector = true
			}
		}
	}

	return val
}

// --- ThisExpr ---

func (c *Compiler) genThisExpr() value.Value {
	alloca, ok := c.locals["this"]
	if !ok {
		panic("codegen: 'this' used but not in method context")
	}
	return c.block.NewLoad(alloca.ElemType, alloca)
}

// --- Method calls ---

// genMethodCall generates a method call on a user type instance.
func (c *Compiler) genMethodCall(e *ast.CallExpr, member *ast.MemberExpr) value.Value {
	targetType := c.info.Types[member.Target]
	// Apply typeSubst for mono context
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	// Apply selfSubst for default method synthesis
	if c.selfSubst != nil {
		targetType = types.SubstituteSelf(targetType, c.selfSubst.iface, c.selfSubst.concrete)
	}

	// Container native method dispatch (Vector, Map, string)
	if result, ok := c.genContainerMethodCall(e, member, targetType); ok {
		return result
	}

	// Enum method dispatch
	if result, ok := c.genEnumMethodCall(e, member, targetType); ok {
		return result
	}

	named := extractNamed(targetType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot resolve type for method call on %T", targetType))
	}

	method := named.LookupMethod(member.Field)
	if method == nil {
		panic(fmt.Sprintf("codegen: no method %s on type %s", member.Field, named))
	}

	// Virtual dispatch: if the static type needs vtable and the method is not native,
	// emit an indirect call through the vtable so the correct override is called.
	if c.needsVtable(named) && !method.IsNative() {
		return c.genVirtualMethodCall(e, member, named, method, targetType)
	}

	// Direct dispatch: resolve method to a compile-time-known function.
	// For mono/generic types, use resolveTypeName (handles Instance → mono name).
	// For regular Named types with inheritance, use resolveMethodOwner to find
	// the parent that actually defines the method.
	var mangledName string
	ownerName := c.resolveMethodOwner(named, member.Field)
	if ownerName != named.Obj().Name() {
		// Method inherited from parent. Check if the parent is structural —
		// if so, use the concrete type's name (methods are synthesized per-concrete).
		if structParent := c.findStructuralOwner(named, member.Field); structParent != nil {
			concreteName := c.resolveTypeName(targetType)
			c.ensureDefaultMethodsSynthesized(named, structParent)
			mangledName = mangleMethodName(concreteName, member.Field, false)
		} else {
			// Non-structural parent: use the monomorphized parent name.
			monoOwner := c.resolveMonoParentName(named, targetType, ownerName)
			mangledName = mangleMethodName(monoOwner, member.Field, false)
		}
	} else {
		mangledName = mangleMethodName(c.resolveTypeName(targetType), member.Field, false)
	}

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared method %s", mangledName))
	}

	var args []value.Value
	if method.Sig().Recv() != nil {
		target := c.genExprAutoPropagate(member.Target) // B0323
		// T0130: Defer receiver claim — only claim if method produces a new iterator.
		c.pendingReceiverClaim = target
		// Container types (Vector, Map, string) are already i8* pointers — pass directly.
		// `this` in a method body is also i8*.
		// Primitive scalars (int, f64, bool, char, etc.) are raw values — pass directly.
		// Value types: store to temp alloca, pass pointer (value semantics).
		// Regular user types are value structs — extract the instance pointer.
		if _, isThis := member.Target.(*ast.ThisExpr); isThis {
			args = append(args, target)
		} else if isContainerType(targetType) {
			args = append(args, target)
		} else if isPrimitiveScalar(named) {
			args = append(args, target)
		} else if named.IsValueType() {
			args = append(args, c.valueTypeReceiverPtr(target, targetType))
		} else {
			instancePtr := c.extractInstancePtr(target)
			args = append(args, instancePtr)
			// B0258: Track method chain intermediate for cleanup at statement end.
			c.trackChainIntermediateReceiver(member.Target, target, instancePtr, named, targetType)
		}
	}
	argVals, argTypes, variadicPTs := c.genCallArgsWithMutRef(e.Args, method.Sig().Params())
	origArgVals := argVals // B0345
	// T0418: Build owner-type subst (e.g., Box[int].T → int) so generic
	// methods on a generic instance see TypeParam-typed params resolved.
	ownerSubst := c.buildOwnerTypeArgSubst(targetType)
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params(), e.Args, ownerSubst)
	args = append(args, argVals...)

	result := c.block.NewCall(fn, args...)
	c.clearVariadicStaticFlags(variadicPTs)
	c.emitReturnAliasCheckSubst(result, method.Sig(), e.Args, origArgVals, ownerSubst) // B0345/T0418
	return result
}

// genEnumGetterAccess emits a getter call on an enum value (e.g., s.name where name is a getter on enum Shape).
// Returns (result, true) if the enum has a matching getter, (nil, false) otherwise.
func (c *Compiler) genEnumGetterAccess(e *ast.MemberExpr, targetType types.Type, layout *TypeDeclLayout) (value.Value, bool) {
	var enum *types.Enum
	var enumName string
	switch t := targetType.(type) {
	case *types.Enum:
		enum = t
		enumName = t.Obj().Name()
		// Inside mono method body, this is the origin enum — use mono name
		if c.monoCtx != nil {
			if origin, ok := c.monoCtx.origin.(*types.Enum); ok && t == origin {
				enumName = c.monoCtx.name
			}
		}
	case *types.Instance:
		if en, ok := t.Origin().(*types.Enum); ok {
			enum = en
			enumName = monoName(t)
		}
	}
	if enum == nil {
		return nil, false
	}
	getter := enum.LookupGetter(e.Field)
	if getter == nil {
		return nil, false
	}

	mangledName := mangleMethodName(enumName, e.Field, false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		return nil, false
	}

	// Pass the enum value as receiver
	prevEnumTemps := len(c.enumCtorTemps)
	target := c.genExprAutoPropagate(e.Target) // B0323
	enumCtorTracked := len(c.enumCtorTemps) > prevEnumTemps
	var ptr value.Value
	var tempEnumPtr value.Value
	// `this` inside an enum method is already i8* pointing to the enum alloca — pass directly.
	if _, isThis := e.Target.(*ast.ThisExpr); isThis {
		ptr = target
	} else {
		alloca := c.entryBlock.NewAlloca(target.Type())
		alloca.SetName(c.uniqueLocalName("enum.getter"))
		c.block.NewStore(target, alloca)
		ptr = c.block.NewBitCast(alloca, irtypes.I8Ptr)
		// T0660: a borrow-return receiver (`Tagged&`/`Tagged~`) aliases the
		// owner's payload; dropping the synthesized getter receiver temp
		// would double-free what the owner still frees at scope exit.
		if isFreshEnumExpr(e.Target) && !enumCtorTracked && !c.isBorrowedExpr(e.Target) {
			tempEnumPtr = ptr
		}
	}

	result := c.block.NewCall(fn, ptr)

	// Drop temp enum receiver if it was a fresh temporary not tracked by enumCtorTemps.
	if tempEnumPtr != nil && c.enumInstanceHasDrop(targetType, enum) {
		dropName := mangleMethodName(enumName, "drop", false)
		if dropFn, ok := c.funcs[dropName]; ok {
			c.block.NewCall(dropFn, tempEnumPtr)
		} else if c.moduleInfos != nil {
			if dropFn := c.forwardDeclareModuleEnumDrop(enum, enumName, dropName); dropFn != nil {
				c.block.NewCall(dropFn, tempEnumPtr)
			}
		}
	}

	return result, true
}

// genEnumMethodCall generates a method call on an enum value.
// Returns (result, true) if the target is an enum with a matching method, (nil, false) otherwise.
func (c *Compiler) genEnumMethodCall(e *ast.CallExpr, member *ast.MemberExpr, targetType types.Type) (value.Value, bool) {
	// T0639: unwrap a ~/& generic-enum-instance receiver so the enum +
	// monoName resolve (mirrors genGenericEnumMethodCall). Without this a
	// non-generic enum method on a `~`/`&` generic-enum param falls through
	// to the default branch and the call silently fails to dispatch.
	if ref, ok := targetType.(*types.MutRef); ok {
		return c.genEnumMethodCall(e, member, ref.Elem())
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		return c.genEnumMethodCall(e, member, ref.Elem())
	}
	var enum *types.Enum
	var enumName string

	switch t := targetType.(type) {
	case *types.Enum:
		enum = t
		enumName = t.Obj().Name()
		// Inside mono method body, this is the origin enum — use mono name
		if c.monoCtx != nil {
			if origin, ok := c.monoCtx.origin.(*types.Enum); ok && t == origin {
				enumName = c.monoCtx.name
			}
		}
	case *types.Instance:
		if en, ok := t.Origin().(*types.Enum); ok {
			enum = en
			enumName = monoName(t)
		}
	default:
		return nil, false
	}

	if enum == nil {
		return nil, false
	}

	method := enum.LookupMethod(member.Field)
	if method == nil {
		return nil, false
	}

	mangledName := mangleMethodName(enumName, member.Field, false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		return nil, false
	}

	var args []value.Value
	var tempEnumPtr value.Value // non-nil when receiver needs post-call drop
	if method.Sig().Recv() != nil {
		// Track whether the enumCtorTemps mechanism captures this constructor.
		prevEnumTemps := len(c.enumCtorTemps)
		target := c.genExprAutoPropagate(member.Target) // B0323
		enumCtorTracked := len(c.enumCtorTemps) > prevEnumTemps
		// `this` inside an enum method is already i8* pointing to the enum alloca — pass directly.
		if _, isThis := member.Target.(*ast.ThisExpr); isThis {
			args = append(args, target)
		} else {
			// Store the enum value to a temp alloca and pass pointer as i8*.
			// Use the actual LLVM type of the value (i32 for fieldless, struct for data enums).
			alloca := c.entryBlock.NewAlloca(target.Type())
			alloca.SetName(c.uniqueLocalName("enum.this"))
			c.block.NewStore(target, alloca)
			ptr := c.block.NewBitCast(alloca, irtypes.I8Ptr)
			args = append(args, ptr)
			// Track for post-call drop if target produces a fresh value not already
			// tracked by enumCtorTemps. IdentExpr targets share heap data with
			// their binding's alloca — dropping the shallow copy would double-free.
			// T0660: a borrow-return receiver (`Tagged&`/`Tagged~`, e.g.
			// `ev.at(0)`) likewise aliases the owner's payload — dropping it
			// double-frees what the owner (the vector) still frees at exit.
			if isFreshEnumExpr(member.Target) && !enumCtorTracked && !c.isBorrowedExpr(member.Target) {
				tempEnumPtr = ptr
			}
		}
	}
	argVals, argTypes, variadicPTs := c.genCallArgsWithMutRef(e.Args, method.Sig().Params())
	origArgVals := argVals // B0345
	// T0418: Build owner-enum subst so generic methods on a generic enum
	// instance see TypeParam-typed params resolved.
	var enumSubst map[*types.TypeParam]types.Type
	if inst, ok := targetType.(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Enum); ok && len(origin.TypeParams()) > 0 {
			enumSubst = types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
		}
	}
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params(), e.Args, enumSubst)
	args = append(args, argVals...)

	result := c.block.NewCall(fn, args...)
	c.clearVariadicStaticFlags(variadicPTs)
	c.emitReturnAliasCheckSubst(result, method.Sig(), e.Args, origArgVals, enumSubst) // B0345/T0418

	// Drop temp enum receiver if it was a fresh temporary not tracked by
	// the enumCtorTemps mechanism. When movedDroppable caused enumCtorTemps
	// to skip tracking (B0252), the borrow method's receiver still needs cleanup.
	if tempEnumPtr != nil && c.enumInstanceHasDrop(targetType, enum) {
		dropName := mangleMethodName(enumName, "drop", false)
		if dropFn, ok := c.funcs[dropName]; ok {
			c.block.NewCall(dropFn, tempEnumPtr)
		} else if c.moduleInfos != nil {
			if dropFn := c.forwardDeclareModuleEnumDrop(enum, enumName, dropName); dropFn != nil {
				c.block.NewCall(dropFn, tempEnumPtr)
			}
		}
	}

	return result, true
}

// isFreshEnumExpr returns true if the expression produces a fresh enum value
// (not a reference to an existing variable). Fresh values need post-call drop
// when used as a temporary method/getter receiver.
func isFreshEnumExpr(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.CallExpr:
		return true
	case *ast.ErrorPanicExpr:
		// Panic unwrap of a call result (e.g., at(0)?!) produces a fresh value.
		return isFreshEnumExpr(e.Expr)
	case *ast.OptionalUnwrapExpr:
		// Unwrap of a call result (e.g., at(0)!) produces a fresh value.
		// Unwrap of a variable (e.g., opt_var!) references existing data.
		return isFreshEnumExpr(e.Expr)
	case *ast.AutoCloneExpr:
		return true // T0605: a deep clone is always a fresh owned value
	default:
		return false
	}
}

// genGetterCall emits a call to a getter method (zero args beyond receiver).
// Uses virtual dispatch through the vtable when the static type needs it.
func (c *Compiler) genGetterCall(e *ast.MemberExpr, targetType types.Type, named *types.Named, getter *types.Method) value.Value {
	// Global getter: no receiver, just call the function directly.
	if getter.Sig().Recv() == nil {
		mangledName := mangleMethodName(c.resolveTypeName(targetType), e.Field, false)
		fn, ok := c.funcs[mangledName]
		if !ok {
			panic(fmt.Sprintf("codegen: undeclared global getter %s", mangledName))
		}
		return c.block.NewCall(fn)
	}

	// Virtual dispatch for getter when static type needs vtable
	if c.needsVtable(named) && !getter.IsNative() {
		return c.genVirtualGetterCall(e, named, getter, targetType)
	}

	var mangledName string
	ownerName := c.resolveMethodOwner(named, e.Field)
	if ownerName != named.Obj().Name() {
		// Getter inherited from parent. Resolve to mono name if parent is generic.
		monoOwner := c.resolveMonoParentName(named, targetType, ownerName)
		mangledName = mangleMethodName(monoOwner, e.Field, false)
	} else {
		mangledName = mangleMethodName(c.resolveTypeName(targetType), e.Field, false)
	}

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared getter %s", mangledName))
	}

	var args []value.Value
	target := c.genExprAutoPropagate(e.Target) // B0323
	if _, isThis := e.Target.(*ast.ThisExpr); isThis {
		args = append(args, target)
	} else if isContainerType(targetType) {
		args = append(args, target)
	} else if isPrimitiveScalar(named) {
		args = append(args, target)
	} else if named.IsValueType() {
		args = append(args, c.valueTypeReceiverPtr(target, targetType))
	} else {
		instancePtr := c.extractInstancePtr(target)
		args = append(args, instancePtr)
		// B0258: Track getter chain intermediate for cleanup at statement end.
		c.trackChainIntermediateReceiver(e.Target, target, instancePtr, named, targetType)
	}

	result := c.block.NewCall(fn, args...)
	c.trackGetterResult(e, getter, targetType, result)
	return result
}

// genVirtualGetterCall emits an indirect getter call through the vtable.
func (c *Compiler) genVirtualGetterCall(e *ast.MemberExpr, named *types.Named, getter *types.Method, targetType types.Type) value.Value {
	receiverVal := c.genExprAutoPropagate(e.Target) // B0323

	var vtableRaw, instance value.Value
	if _, isThis := e.Target.(*ast.ThisExpr); isThis {
		instance = receiverVal
		variantPtr := c.loadVariantPtr(receiverVal)
		typeinfoStruct := irtypes.NewStruct(irtypes.I8Ptr)
		typeinfoPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoStruct))
		vtableFieldPtr := c.block.NewGetElementPtr(typeinfoStruct, typeinfoPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		vtableRaw = c.block.NewLoad(irtypes.I8Ptr, vtableFieldPtr)
	} else {
		vtableRaw = c.extractVtablePtr(receiverVal)
		instance = c.extractInstancePtr(receiverVal)
		// B0258: Track getter chain intermediate for cleanup at statement end.
		c.trackChainIntermediateReceiver(e.Target, receiverVal, instance, named, targetType)
	}

	slotIndex := named.VirtualMethodIndex(e.Field, false) // getter, not setter
	if slotIndex < 0 {
		panic(fmt.Sprintf("codegen: getter %s not in vtable for %s", e.Field, named))
	}
	vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
	fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
		constant.NewInt(irtypes.I32, int64(slotIndex)))
	fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

	// Substitute type params for generic instances (e.g. Transformer[int]).
	// T0418: include parent-type params so inherited getters resolve correctly.
	vtableSubst := c.buildOwnerTypeArgSubst(targetType)
	resolveVtableType := func(t types.Type) irtypes.Type {
		if vtableSubst != nil {
			t = types.Substitute(t, vtableSubst)
		}
		return c.resolveType(t)
	}

	retType := irtypes.Type(irtypes.Void)
	if getter.Sig().Result() != nil {
		retType = resolveVtableType(getter.Sig().Result())
	}
	if getter.Sig().CanError() {
		retType = computeResultType(retType)
	}
	paramTypes := []irtypes.Type{irtypes.I8Ptr}
	funcType := irtypes.NewFunc(retType, paramTypes...)
	fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))

	result := c.block.NewCall(fnTyped, instance)
	c.trackGetterResult(e, getter, targetType, result)
	return result
}

// trackGetterResult registers a getter return value for cleanup at statement end.
// T0494: extends the original B0290 sliver (which only handled string results)
// to cover every droppable return type — string, vector, map, and user heap
// types — mirroring the tracking pattern in genExpr's *ast.CallExpr case.
// Without this, getter results in for-in iterable position (e.g.
// `for k,v in resp.headers`), method-chain receiver position (e.g.
// `resp.headers.contains(k)`), or any other dropped/expression position leak
// because no scope binding owns the cloned heap allocation.
//
// String/Vector i8* results dispatch to trackStringTemp / trackVectorTemp.
// {i8*, i8*} value-struct results (Map, Set, user heap types) dispatch to
// trackHeapUserTypeResult, which already filters out value/copy/structural
// types and primitives so calling it unconditionally is safe.
//
// targetType is the receiver type at the call site. It supplies owner-type
// substitution (e.g., ArcCell[int].fresh's `Arc[T]` → `Arc[int]`) so the
// per-element-type drop function looks up the concrete instantiation rather
// than the unsubstituted TypeParam.
func (c *Compiler) trackGetterResult(e *ast.MemberExpr, getter *types.Method, targetType types.Type, result value.Value) {
	if !c.tempTrackingEnabled || result == nil {
		return
	}
	if result.Type() == irtypes.I8Ptr {
		retType := getter.Sig().Result()
		// Owner-type subst: when the getter's owner is a generic instance
		// (e.g. ArcCell[int]), resolve the owner's TypeParams against the
		// instance's TypeArgs before applying any further substitution.
		// Without this, Arc[T] from ArcCell[T].fresh's signature stays as
		// Arc[T] and getOrCreateArcDrop(T) would produce an Arc[T].drop fn
		// that doesn't know T's concrete layout/inner-drop.
		if ownerSubst := c.buildOwnerTypeArgSubst(targetType); ownerSubst != nil && retType != nil {
			retType = types.Substitute(retType, ownerSubst)
		}
		if c.typeSubst != nil && retType != nil {
			retType = types.Substitute(retType, c.typeSubst)
		}
		if c.selfSubst != nil && retType != nil {
			retType = types.SubstituteSelf(retType, c.selfSubst.iface, c.selfSubst.concrete)
		}
		if retType == nil {
			return
		}
		named := extractNamed(retType)
		if named == types.TypString {
			c.trackStringTemp(result)
		} else if named == types.TypVector {
			if elemType, ok := types.AsVector(retType); ok {
				c.trackVectorTempWithElemType(result, elemType)
			} else {
				c.trackVectorTemp(result)
			}
		} else if chElem, isCh := types.AsChannel(retType); isCh || named == types.TypChannel {
			// T0486: Channel[T] getter result owns a heap allocation; without
			// tracking the cloned channel pointer leaks at statement end.
			// T0663: per-element-type drop walks any un-received buffered items.
			c.trackChannelTempWithElemType(result, chElem)
		} else if arcElem, isArc := types.AsArc(retType); isArc {
			// T0486: Arc[T] getter result owns a heap allocation; without
			// tracking the cloned Arc leaks at statement end. arcElem is
			// already substituted (Substitute on Instance produces a new
			// Instance with substituted typeArgs).
			c.trackTempWithDrop(result, c.getOrCreateArcDrop(arcElem))
		} else if weakElem, isWeak := types.AsWeak(retType); isWeak {
			// T0486: Weak[T] getter result owns a heap allocation.
			c.trackTempWithDrop(result, c.getOrCreateWeakDrop(weakElem))
		} else if mutexElem, isMutex := types.AsMutex(retType); isMutex {
			// T0486: Mutex[T] getter result owns a heap allocation.
			c.trackTempWithDrop(result, c.getOrCreateMutexDrop(mutexElem))
		} else if taskElem, isTask := types.AsTask(retType); isTask {
			// T0503: Task[T] getter result owns a G struct + result buffer.
			c.trackTempWithDrop(result, c.getOrCreateTaskDrop(taskElem))
		}
	} else {
		c.trackHeapUserTypeResult(e, result)
	}
}

// genVirtualMethodCall emits an indirect call through the vtable.
// Reads vtable pointer from the value struct (field 0), indexes into it
// to get the function pointer, casts it, and calls.
func (c *Compiler) genVirtualMethodCall(e *ast.CallExpr, member *ast.MemberExpr,
	named *types.Named, method *types.Method, targetType types.Type) value.Value {

	// 1. Evaluate receiver
	receiverVal := c.genExprAutoPropagate(member.Target) // B0323
	// T0130: Defer receiver claim — only claim if method produces a new iterator.
	c.pendingReceiverClaim = receiverVal

	// 2. Extract vtable and instance
	var vtableRaw, instance value.Value
	if _, isThis := member.Target.(*ast.ThisExpr); isThis {
		// `this` is already i8* — load vtable from typeinfo chain
		instance = receiverVal
		variantPtr := c.loadVariantPtr(receiverVal)
		// typeinfo field 0 is vtable_ptr
		typeinfoStruct := irtypes.NewStruct(irtypes.I8Ptr)
		typeinfoPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoStruct))
		vtableFieldPtr := c.block.NewGetElementPtr(typeinfoStruct, typeinfoPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		vtableRaw = c.block.NewLoad(irtypes.I8Ptr, vtableFieldPtr)
	} else {
		vtableRaw = c.extractVtablePtr(receiverVal)
		instance = c.extractInstancePtr(receiverVal)
		// B0258: Track method chain intermediate for cleanup at statement end.
		c.trackChainIntermediateReceiver(member.Target, receiverVal, instance, named, targetType)
	}

	// 3. Index into vtable — use the STATIC type's slot layout
	slotIndex := named.VirtualMethodIndex(member.Field, false) // regular method, not setter
	if slotIndex < 0 {
		panic(fmt.Sprintf("codegen: method %s not in vtable for %s", member.Field, named))
	}
	vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
	fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
		constant.NewInt(irtypes.I32, int64(slotIndex)))
	fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

	// 4. Build the correct function type and bitcast.
	// If the static type is a generic instance (e.g. Transformer[int]),
	// substitute type params so T→int in method signatures.
	// T0418: include parent-type params (via mergeParentSubst inside
	// buildOwnerTypeArgSubst) so inherited methods using parent's TypeParams
	// resolve correctly.
	vtableSubst := c.buildOwnerTypeArgSubst(targetType)
	resolveVtableType := func(t types.Type) irtypes.Type {
		if vtableSubst != nil {
			t = types.Substitute(t, vtableSubst)
		}
		return c.resolveType(t)
	}

	retType := irtypes.Type(irtypes.Void)
	if method.Sig().Result() != nil {
		retType = resolveVtableType(method.Sig().Result())
	}
	if method.Sig().CanError() {
		retType = computeResultType(retType)
	}
	var paramTypes []irtypes.Type
	if method.Sig().Recv() != nil {
		paramTypes = append(paramTypes, irtypes.I8Ptr)
	}
	for _, p := range method.Sig().Params() {
		pt := resolveVtableType(p.Type())
		// MutRef params are passed as pointers (B0149)
		if _, isMutRef := p.Type().(*types.MutRef); isMutRef {
			pt = irtypes.NewPointer(pt)
		}
		paramTypes = append(paramTypes, pt)
	}
	funcType := irtypes.NewFunc(retType, paramTypes...)
	fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))

	// 5. Call — receiver is instance (i8*), not the value struct
	var args []value.Value
	if method.Sig().Recv() != nil {
		args = append(args, instance)
	}
	argVals, argTypes, variadicPTs := c.genCallArgsWithMutRef(e.Args, method.Sig().Params())
	origArgVals := argVals // B0345
	// T0418: vtableSubst (with parent subst merged) covers both the static
	// type's TypeParams and inherited parent-type TypeParams.
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params(), e.Args, vtableSubst)
	args = append(args, argVals...)
	result := c.block.NewCall(fnTyped, args...)
	c.clearVariadicStaticFlags(variadicPTs)
	c.emitReturnAliasCheckSubst(result, method.Sig(), e.Args, origArgVals, vtableSubst) // B0345/T0418
	return result
}

// genContainerMethodCall dispatches native method calls on Vector, Map, and string.
// Returns (result, true) if handled, (nil, false) otherwise.
// Non-native methods (with Promise bodies) fall through to the regular call path.
// Handles both Instance wrappers (user code: Vector[int]) and bare Named types
// (method body: this is TypVector) by resolving type args from typeSubst.
func (c *Compiler) genContainerMethodCall(e *ast.CallExpr, member *ast.MemberExpr, targetType types.Type) (value.Value, bool) {
	methodName := member.Field

	// Unwrap MutRef/SharedRef so types.AsVector etc. can see the Instance.
	// Parameters declared as `T[] ~buf` have type MutRef{Instance{TypVector, [T]}}.
	unwrapped := targetType
	if mr, ok := unwrapped.(*types.MutRef); ok {
		unwrapped = mr.Elem()
	} else if sr, ok := unwrapped.(*types.SharedRef); ok {
		unwrapped = sr.Elem()
	}

	// Check if the method is native — only native methods are handled here.
	// Non-native methods fall through to the regular user method path.
	named := extractNamed(targetType)
	if named == types.TypVector || named == types.TypString || named == types.TypChannel || named == types.TypArc ||
		named == types.TypMutex || named == types.TypMutexGuard {
		m := named.LookupMethod(methodName)
		if m == nil || !m.IsNative() {
			return nil, false // fall through to regular method dispatch
		}
	}

	// Vector methods: push, pop, contains, remove
	if elem, ok := types.AsVector(unwrapped); ok {
		return c.genVectorMethodCall(e, member, elem, methodName), true
	}
	// Bare TypVector (inside a method body on Vector): resolve T from typeSubst
	if named == types.TypVector {
		if elem := c.resolveTypeParam(types.TypVector.TypeParams()[0]); elem != nil {
			return c.genVectorMethodCall(e, member, elem, methodName), true
		}
	}

	// Channel methods: send, close
	if elem, ok := types.AsChannel(unwrapped); ok {
		return c.genChannelMethodCall(e, member, elem, methodName), true
	}
	if named == types.TypChannel {
		if elem := c.resolveTypeParam(types.TypChannel.TypeParams()[0]); elem != nil {
			return c.genChannelMethodCall(e, member, elem, methodName), true
		}
	}

	// Arc methods: clone, downgrade
	if elem, ok := types.AsArc(unwrapped); ok {
		return c.genArcMethodCall(e, member, elem, methodName), true
	}
	if named == types.TypArc {
		if elem := c.resolveTypeParam(types.TypArc.TypeParams()[0]); elem != nil {
			return c.genArcMethodCall(e, member, elem, methodName), true
		}
	}

	// Weak methods: upgrade, clone (T0157)
	if elem, ok := types.AsWeak(unwrapped); ok {
		return c.genWeakMethodCall(e, member, elem, methodName), true
	}
	if named == types.TypWeak {
		if elem := c.resolveTypeParam(types.TypWeak.TypeParams()[0]); elem != nil {
			return c.genWeakMethodCall(e, member, elem, methodName), true
		}
	}

	// Mutex methods: lock
	if elem, ok := types.AsMutex(unwrapped); ok {
		return c.genMutexMethodCall(e, member, elem, methodName), true
	}
	if named == types.TypMutex {
		if elem := c.resolveTypeParam(types.TypMutex.TypeParams()[0]); elem != nil {
			return c.genMutexMethodCall(e, member, elem, methodName), true
		}
	}

	// String native methods: trim, split (contains/starts_with/ends_with/index_of are now pure Promise)
	if named == types.TypString {
		if result, ok := c.genStringMethodCall(e, member, methodName); ok {
			return result, true
		}
	}

	return nil, false
}

// resolveTypeParam looks up a type parameter in the current typeSubst map.
// Returns nil if not in a monomorphic context or the param is not mapped.
func (c *Compiler) resolveTypeParam(tp *types.TypeParam) types.Type {
	if c.typeSubst == nil {
		return nil
	}
	return c.typeSubst[tp]
}

func (c *Compiler) genVectorMethodCall(e *ast.CallExpr, member *ast.MemberExpr, elemType types.Type, method string) value.Value {
	slicePtr := c.genExprAutoPropagate(member.Target) // B0323
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(c.typeSize(elemLLVM))

	switch method {
	case "push":
		// T0658: Hoist resolvedElem (was computed redundantly further down)
		// so the Optional-wrap below and the existing droppable-element dup
		// logic share one resolution.
		resolvedElem := elemType
		if c.typeSubst != nil {
			resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
		}
		_, elemIsOpt := resolvedElem.(*types.Optional)

		// T0658: Set targetType to the resolved Optional element type so a
		// bare `none` arg lowers to a zero {i1,T} struct via genNoneLit
		// (mirrors the genAssignStmt index-assign path, stmt.go:5309-5316).
		// The push path never set this, so `v.push(none)` into a T?[] used
		// to return the i1 0 "void optional fallback".
		savedTarget := c.targetType
		if elemIsOpt {
			c.targetType = resolvedElem
		}
		// T0388: When the source is a field read on a droppable owner
		// (e.g. v.push(h.arr) where arr is a Vector/Channel/Arc/Weak/
		// Optional[these] field), set dupContainerFieldAccess so
		// genFieldAccess produces an independent dup tracked as a heap
		// temp. claimHeapTemp below then transfers ownership to the
		// vector. Module/native getters and enum-variant access take
		// other paths in genMemberExpr and never reach genFieldAccess,
		// so the flag is only consumed for actual struct field reads —
		// getter results are not double-dup'd. String fields use a
		// separate flag (dupStringFieldAccess) and are handled by the
		// post-load c.dupString(argVal) below; setting the container
		// flag is harmless for string-element pushes.
		// (Not borrow-gated — checks MemberExpr AST shape on an owned type.
		// Remains active post-T0438.)
		if _, isMember := e.Args[0].Value.(*ast.MemberExpr); isMember {
			c.dupContainerFieldAccess = true
		}
		argVal := c.genCallArgExpr(e.Args[0].Value)
		c.dupContainerFieldAccess = false
		c.targetType = savedTarget

		// T0658: Wrap a bare RHS into the Optional element struct when the
		// vector element type is Optional but the pushed expr is not (e.g.
		// `int?[] v = []; v.push(1)`). This is the push-side analog of T0615
		// (genVectorIndexAssign). Without it the raw scalar/pointer is stored
		// straight into the {i1,T} slot → "store operands are not compatible".
		// Predicate mirrors stmt.go:5832-5851 exactly, including
		// claim-before-wrap for string/native-handle/container temps whose
		// stmtTempMap tracking is by val-identity (lost once val is wrapped).
		// Heap user-type temps are still claimed correctly post-wrap by
		// claimHeapTemp's struct-extraction fallback (B0233), so excluded.
		if elemIsOpt {
			argExprType := c.info.Types[e.Args[0].Value]
			if c.typeSubst != nil && argExprType != nil {
				argExprType = types.Substitute(argExprType, c.typeSubst)
			}
			if argExprType != types.TypNone && !types.Identical(argExprType, resolvedElem) {
				if extractNamed(argExprType) == types.TypString ||
					types.IsVector(argExprType) || types.IsChannel(argExprType) ||
					types.IsArc(argExprType) || types.IsWeak(argExprType) ||
					types.IsTask(argExprType) || types.IsMutex(argExprType) ||
					types.IsMutexGuard(argExprType) {
					c.claimStringTemp(argVal)
				}
				if st, ok := elemLLVM.(*irtypes.StructType); ok {
					argVal = c.wrapOptional(argVal, st)
				}
			}
		}

		// B0189: For string elements, dup before push to ensure exclusive ownership.
		// Each vector must independently own its string elements so that the element
		// drop loop in Vector.drop doesn't cause double-frees when strings are shared
		// between vectors (e.g., normalize() where parts[i] is pushed into result).
		// B0302: Extended to all droppable element types (vectors, channels, heap user
		// types). Without duplication, pushing the same value multiple times (e.g., in
		// Vector.filled) creates aliased pointers — the element-level drop on the outer
		// vector frees the same data N times (double/triple-free). Duplication ensures
		// each element is independently owned, matching the B0189 string pattern.
		if extractNamed(resolvedElem) == types.TypString {
			argVal = c.dupString(argVal)
			// Don't clear source drop flag — source retains its string.
			// Don't claim string temp — original temp is freed normally by cleanup.
		} else {
			// B0302: When the source is an ident with a drop flag AND the element
			// type is droppable, use a runtime check: if the flag is still true this
			// is the first push (move semantics — clear flag). If false, the variable
			// was already consumed (e.g., in a prior loop iteration of Vector.filled)
			// — dup the element to avoid aliased pointers that cause double-free.
			// For idents WITHOUT a drop flag (function params), always dup droppable
			// types. For non-ident sources, see the T0376 branch below.
			dupped := false
			if ident, ok := e.Args[0].Value.(*ast.IdentExpr); ok {
				if flagAlloca, hasFlag := c.dropFlags[ident.Name]; hasFlag {
					if c.pushElemNeedsDup(resolvedElem) {
						// Runtime branch: flag=true → first use (move), flag=false → dup
						flag := c.block.NewLoad(irtypes.I1, flagAlloca)
						moveBlock := c.newBlock("push.move")
						dupBlock := c.newBlock("push.dup")
						mergeBlock := c.newBlock("push.merge")
						c.block.NewCondBr(flag, moveBlock, dupBlock)

						c.block = moveBlock
						moveEnd := c.block
						c.block.NewBr(mergeBlock)

						// Generate dup INSIDE the dup block so the allocation only
						// happens when the value was already consumed.
						c.block = dupBlock
						dupVal := c.maybeDupPushElement(argVal, resolvedElem)
						dupEnd := c.block
						c.block.NewBr(mergeBlock)

						c.block = mergeBlock
						argVal = c.block.NewPhi(
							ir.NewIncoming(argVal, moveEnd),
							ir.NewIncoming(dupVal, dupEnd),
						)
						dupped = true
					}
					c.clearDropFlag(ident.Name)
				} else {
					// No drop flag (function parameter): always dup droppable types.
					if dupVal := c.maybeDupPushElement(argVal, resolvedElem); dupVal != nil {
						argVal = dupVal
						dupped = true
					}
				}
			} else if _, isIndex := e.Args[0].Value.(*ast.IndexExpr); isIndex {
				// T0376: IndexExpr source (e.g. this[i] in Vector.[:], arr[k]).
				// Returns a load — an alias to the element at the given index.
				// Without dup, the new vector's slot would alias the source
				// container's element pointer and the cleanup walks (vector +
				// source) would double-free. Dup so the new vector owns an
				// independent copy. Symmetric with the IdentExpr-without-flag
				// (function param) path. CallExpr / MemberExpr / literal
				// sources are left alone — CallExpr returns fresh allocations
				// (constructors, getters) whose ownership the vector inherits,
				// and MemberExpr field-access dup is left for a follow-up
				// because some MemberExpr forms (module getters, instance
				// getters) also return fresh values and can't be safely
				// distinguished from field access at this layer.
				//
				// T0387: Polymorphic element types (those needing a vtable) are
				// no longer carved out. dupHeapValue → maybeDupPushElement →
				// cloneHeapElement falls through to dupHeapValue, which now
				// dispatches via typeinfo.clone_fn_ptr to the runtime concrete
				// type's clone fn — independent copy with full subtype data.
				if dupVal := c.maybeDupPushElement(argVal, resolvedElem); dupVal != nil {
					argVal = dupVal
					dupped = true
				}
			}
			if !dupped {
				// Non-ident or non-droppable: clear drop flag as before
				if ident, ok := e.Args[0].Value.(*ast.IdentExpr); ok {
					c.clearDropFlag(ident.Name)
				}
			}
			// B0170: claim string temp — ownership transfers to vector
			c.claimStringTemp(argVal)
			// B0233: claim heap temp — ownership transfers to vector
			c.claimHeapTemp(argVal)
		}
		// COW: if static (.rodata), copy to heap first (T0062)
		cowSlice := c.block.NewCall(c.funcs["promise_vector_cow"],
			slicePtr, constant.NewInt(irtypes.I64, elemSize))
		argAlloca := c.createEntryAlloca(elemLLVM)
		// Zero-initialize before store to clear padding bytes for memcmp correctness
		c.block.NewStore(constant.NewZeroInitializer(elemLLVM), argAlloca)
		c.block.NewStore(argVal, argAlloca)
		argPtr := c.block.NewBitCast(argAlloca, irtypes.I8Ptr)
		newSlice := c.block.NewCall(c.funcs["promise_vector_push"],
			cowSlice, argPtr, constant.NewInt(irtypes.I64, elemSize))
		// Store the (possibly reallocated) pointer back
		c.storeBackSlicePtr(member.Target, newSlice)
		return newSlice

	case "pop":
		// COW: if static (.rodata), copy to heap first (T0062)
		cowSlice := c.block.NewCall(c.funcs["promise_vector_cow"],
			slicePtr, constant.NewInt(irtypes.I64, elemSize))
		c.storeBackSlicePtr(member.Target, cowSlice)
		outAlloca := c.createEntryAlloca(elemLLVM)
		outPtr := c.block.NewBitCast(outAlloca, irtypes.I8Ptr)
		found := c.block.NewCall(c.funcs["promise_vector_pop"],
			cowSlice, outPtr, constant.NewInt(irtypes.I64, elemSize))
		// Build Optional: {i1, T}
		optType := irtypes.NewStruct(irtypes.I1, elemLLVM)
		isFound := c.block.NewTrunc(found, irtypes.I1)
		someBlock := c.newBlock("pop.some")
		noneBlock := c.newBlock("pop.none")
		mergeBlock := c.newBlock("pop.merge")
		c.block.NewCondBr(isFound, someBlock, noneBlock)

		c.block = someBlock
		val := c.block.NewLoad(elemLLVM, outAlloca)
		someOpt := c.wrapOptional(val, optType)
		c.block.NewBr(mergeBlock)
		someEnd := c.block

		c.block = noneBlock
		noneOpt := constant.NewZeroInitializer(optType)
		c.block.NewBr(mergeBlock)
		noneEnd := c.block

		c.block = mergeBlock
		phi := c.block.NewPhi(ir.NewIncoming(someOpt, someEnd), ir.NewIncoming(noneOpt, noneEnd))
		return phi

	case "contains":
		argVal := c.genCallArgExpr(e.Args[0].Value)
		argAlloca := c.createEntryAlloca(elemLLVM)
		// Zero-initialize before store to clear padding bytes for memcmp correctness
		c.block.NewStore(constant.NewZeroInitializer(elemLLVM), argAlloca)
		c.block.NewStore(argVal, argAlloca)
		argPtr := c.block.NewBitCast(argAlloca, irtypes.I8Ptr)
		// Use string equality for string elements
		var eqFn value.Value
		if extractNamed(elemType) == types.TypString {
			eqFn = c.block.NewBitCast(c.funcs["__promise_eq_string"], irtypes.I8Ptr)
		} else {
			eqFn = constant.NewNull(irtypes.I8Ptr)
		}
		result := c.block.NewCall(c.funcs["promise_vector_contains"],
			slicePtr, argPtr, constant.NewInt(irtypes.I64, elemSize), eqFn)
		return c.block.NewTrunc(result, irtypes.I1)

	case "clone":
		// Deep-copy the vector: shallow memcpy of header+elements, then deep-clone
		// non-copy elements so the cloned vector owns independent copies. B0275.
		resolvedElem := elemType
		if c.typeSubst != nil {
			resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
		}
		result := c.dupVector(slicePtr, elemSize)
		c.emitVectorElementCloneLoop(result, resolvedElem)
		return result

	case "remove":
		idx := c.genCallArgExpr(e.Args[0].Value)
		// COW: if static (.rodata), copy to heap first (T0062)
		cowSlice := c.block.NewCall(c.funcs["promise_vector_cow"],
			slicePtr, constant.NewInt(irtypes.I64, elemSize))
		c.storeBackSlicePtr(member.Target, cowSlice)

		// B0189: Drop the element being removed if it's droppable (e.g., string).
		// The remove operation shifts subsequent elements, overwriting the removed one.
		resolvedElem := elemType
		if c.typeSubst != nil {
			resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
		}
		if c.variantFieldNeedsDrop(resolvedElem) {
			dataBase := c.block.NewGetElementPtr(irtypes.I8, cowSlice,
				constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
			dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
			removedPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx)
			removedVal := c.block.NewLoad(elemLLVM, removedPtr)
			c.emitVariantFieldDrop(removedVal, resolvedElem)
		}

		c.block.NewCall(c.funcs["promise_vector_remove"],
			cowSlice, idx, constant.NewInt(irtypes.I64, elemSize))
		return nil

	default:
		panic(fmt.Sprintf("codegen: unknown vector method %s", method))
	}
}

// pushElemNeedsDup returns true if the element type is a non-string droppable type
// that would need duplication on push (to prevent aliased pointers). B0302.
func (c *Compiler) pushElemNeedsDup(resolvedElem types.Type) bool {
	// T0399: tuples with droppable fields need dup on push.
	if _, isTup := resolvedElem.(*types.Tuple); isTup {
		return c.tupleNeedsDrop(resolvedElem)
	}
	named := extractNamed(resolvedElem)
	if named == nil {
		if en := extractEnum(resolvedElem); en != nil {
			if _, ok := c.funcs[c.enumCloneFuncName(en, resolvedElem)]; ok {
				return true
			}
			return c.vecElemNeedsEnumDrop(resolvedElem)
		}
		return false
	}
	if _, isVec := types.AsVector(resolvedElem); isVec || named == types.TypVector {
		return true
	}
	if _, isCh := types.AsChannel(resolvedElem); isCh || named == types.TypChannel {
		return true
	}
	// T0508: Arc/Weak need dup (ref-count increment); make explicit so the
	// catch-all below doesn't accidentally exclude them if IsValueType/IsCopy
	// flags change.
	if _, isArc := types.AsArc(resolvedElem); isArc || named == types.TypArc {
		return true
	}
	if _, isWeak := types.AsWeak(resolvedElem); isWeak || named == types.TypWeak {
		return true
	}
	// T0508: Mutex/MutexGuard/Task are single-owner native handles — no dup
	// semantics. Move-only push; the ownership system prevents reuse.
	if _, isMutex := types.AsMutex(resolvedElem); isMutex || named == types.TypMutex {
		return false
	}
	if _, isMG := types.AsMutexGuard(resolvedElem); isMG || named == types.TypMutexGuard {
		return false
	}
	if _, isTask := types.AsTask(resolvedElem); isTask || named == types.TypTask {
		return false
	}
	return !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural()
}

// maybeDupPushElement checks if a vector element type is a non-string droppable
// type that needs duplication on push. Returns the duplicated value, or nil if
// no duplication is needed (primitive/Copy/string types). B0302.
func (c *Compiler) maybeDupPushElement(argVal value.Value, resolvedElem types.Type) value.Value {
	// T0399: Tuples with droppable fields need a deep clone on push. Without
	// this, v2.push(v[i]) for Vector[(string, int)] aliases v's heap-string
	// pointers — both vectors' drop walks would free the same memory. Pure-value
	// tuples (no droppable fields) need no dup.
	if tup, isTup := resolvedElem.(*types.Tuple); isTup {
		if c.tupleNeedsDrop(resolvedElem) {
			return c.dupTupleValue(argVal, tup)
		}
		return nil
	}

	named := extractNamed(resolvedElem)

	// Check for droppable enum types (B0244/B0290 pattern)
	if named == nil {
		if en := extractEnum(resolvedElem); en != nil {
			if _, ok := c.funcs[c.enumCloneFuncName(en, resolvedElem)]; ok {
				cloned, _ := c.cloneEnumValue(argVal, resolvedElem)
				return cloned
			}
			if c.vecElemNeedsEnumDrop(resolvedElem) {
				// Droppable enum without clone — dup variant fields via alloca round-trip
				alloca := c.createEntryAlloca(argVal.Type())
				c.block.NewStore(argVal, alloca)
				c.dupEnumElementInPlace(alloca, resolvedElem)
				return c.block.NewLoad(argVal.Type(), alloca)
			}
		}
		return nil // primitive/Copy
	}

	// Vector element: shallow dup + recursive element clone
	if innerElem, isVec := types.AsVector(resolvedElem); isVec {
		innerLLVM := c.resolveType(innerElem)
		innerSize := int64(c.typeSize(innerLLVM))
		dup := c.dupVector(argVal, innerSize)
		c.emitVectorElementCloneLoop(dup, innerElem)
		return dup
	}
	if named == types.TypVector {
		return c.dupVector(argVal, 0)
	}

	// Channel element: dup (increment ref count)
	if _, isCh := types.AsChannel(resolvedElem); isCh || named == types.TypChannel {
		return c.dupChannel(argVal)
	}

	// T0508: Arc[T] — atomic strong-count increment.
	if _, isArc := types.AsArc(resolvedElem); isArc || named == types.TypArc {
		return c.dupArc(argVal)
	}

	// T0508: Weak[T] — atomic weak-count increment.
	if elem, isWeak := types.AsWeak(resolvedElem); isWeak {
		resolvedWeakElem := elem
		if c.typeSubst != nil {
			resolvedWeakElem = types.Substitute(resolvedWeakElem, c.typeSubst)
		}
		return c.dupWeak(argVal, resolvedWeakElem)
	}

	// T0508: Single-owner native handles (Mutex/MutexGuard/Task) have no dup
	// semantics. Ownership rejects double-moves of non-Copy values, so the
	// runtime dup branch is unreachable in valid programs — return nil.
	if _, isMutex := types.AsMutex(resolvedElem); isMutex || named == types.TypMutex {
		return nil
	}
	if _, isMG := types.AsMutexGuard(resolvedElem); isMG || named == types.TypMutexGuard {
		return nil
	}
	if _, isTask := types.AsTask(resolvedElem); isTask || named == types.TypTask {
		return nil
	}

	// Heap user type with drop: clone via clone method or dupHeapValue fallback
	if !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural() {
		return c.cloneHeapElement(argVal, resolvedElem, named)
	}

	return nil // value/Copy type — no dup needed
}

// storeBackSlicePtr stores the new vector pointer back into the variable that holds the vector.
// This is needed because push may realloc.
func (c *Compiler) storeBackSlicePtr(target ast.Expr, newPtr value.Value) {
	switch t := target.(type) {
	case *ast.IdentExpr:
		if ptr, ok := c.mutRefPtrs[t.Name]; ok {
			// MutRef param: store through the caller's pointer (B0149)
			c.block.NewStore(newPtr, ptr)
		} else if alloca, ok := c.locals[t.Name]; ok {
			c.block.NewStore(newPtr, alloca)
		}
	case *ast.MemberExpr:
		fieldPtr := c.genFieldPtr(t)
		c.block.NewStore(newPtr, fieldPtr)
	case *ast.IndexExpr:
		panic("codegen: push on nested slice (e.g. slices[i].push) not yet supported")
	}
}

// genMutRefArg returns a pointer to the caller's storage for a MutRef argument (B0149).
// This is used at call sites to pass the address of a variable (or forward a
// MutRef param pointer) instead of loading and passing the value.
func (c *Compiler) genMutRefArg(expr ast.Expr) value.Value {
	switch e := expr.(type) {
	case *ast.IdentExpr:
		// If the variable is itself a MutRef param, forward its pointer
		if ptr, ok := c.mutRefPtrs[e.Name]; ok {
			return ptr
		}
		// Otherwise, pass the alloca address (pointer to local variable)
		if alloca, ok := c.locals[e.Name]; ok {
			return alloca
		}
		panic(fmt.Sprintf("codegen: MutRef argument %q not found in locals", e.Name))
	case *ast.MemberExpr:
		// Field access: pass field pointer
		return c.genFieldPtr(e)
	default:
		// Fallback: evaluate normally and store to a temp alloca
		val := c.genCallArgExpr(expr)
		tmp := c.createEntryAlloca(val.Type())
		c.block.NewStore(val, tmp)
		return tmp
	}
}

// setDupFlagsForFieldAccess sets dupStringFieldAccess or dupContainerFieldAccess
// based on the resolved type shape — the four shapes that need dup at every
// owner-droppable field-read consume site: string, Optional[string],
// Vector|Channel|Arc|Weak, and Optional[Vector|Channel|Arc|Weak]. Borrow types
// are skipped (they don't own the value). Caller is responsible for the
// owner-droppable gate (where applicable) and for clearing the flags after the
// dependent codegen runs. T0487.
func (c *Compiler) setDupFlagsForFieldAccess(t types.Type) {
	if t == nil || isRefType(t) {
		return
	}
	if extractNamed(t) == types.TypString {
		c.dupStringFieldAccess = true
		return
	}
	if types.IsVector(t) || types.IsChannel(t) || types.IsArc(t) || types.IsWeak(t) {
		c.dupContainerFieldAccess = true
		return
	}
	if opt, ok := t.(*types.Optional); ok {
		elem := opt.Elem()
		if extractNamed(elem) == types.TypString {
			c.dupStringFieldAccess = true
			return
		}
		if types.IsVector(elem) || types.IsChannel(elem) || types.IsArc(elem) || types.IsWeak(elem) {
			c.dupContainerFieldAccess = true
		}
	}
}

// maybeEnableDupForMutRefArg sets dupStringFieldAccess or dupContainerFieldAccess
// when an arg about to be evaluated is a field read on a droppable owner that's
// being passed to a `~` (consuming) param. Without this, the field's inner
// buffer is shared between the owner and the callee — both end up freeing it.
// T0366.
//
// T0403: Also sets dupHeapUserFieldAccess when the arg is a direct IndexExpr
// against a Vector[heap-user-type]. Without this, `f(v[0])` aliases v's
// element instance pointer — the callee's `~T` drop and v's element walk
// would double-free. Direct IndexExpr only (matching the var-decl-site
// policy in genTyped/InferredVarDecl) avoids orphan-clone leaks for chains
// like `f(v[0].method())`.
func (c *Compiler) maybeEnableDupForMutRefArg(arg ast.Expr, paramType types.Type) {
	pt := paramType
	if c.typeSubst != nil {
		pt = types.Substitute(pt, c.typeSubst)
	}
	if isRefType(pt) {
		return
	}
	// T0403: IndexExpr against Vector[heap-user-type] passed to ~T.
	// Use the arg expression's resolved type (works for generic callees where
	// pt is a TypeParam pre-substitution); mirrors T0398's var-decl-site check.
	if idx, ok := arg.(*ast.IndexExpr); ok {
		argType := c.info.Types[idx]
		if c.typeSubst != nil && argType != nil {
			argType = types.Substitute(argType, c.typeSubst)
		}
		if isDroppableHeapUserType(argType) {
			targetType := c.info.Types[idx.Target]
			if c.typeSubst != nil && targetType != nil {
				targetType = types.Substitute(targetType, c.typeSubst)
			}
			if _, isVec := types.AsVector(targetType); isVec {
				c.dupHeapUserFieldAccess = true
				return
			}
		}
	}
	mem, ok := arg.(*ast.MemberExpr)
	if !ok {
		return
	}
	ownerType := c.info.Types[mem.Target]
	if c.typeSubst != nil && ownerType != nil {
		ownerType = types.Substitute(ownerType, c.typeSubst)
	}
	ownerNamed := extractNamed(ownerType)
	// T0513: also accept mono-synthesized drop on generic instances.
	if !c.ownerHasOrSynthDrop(ownerType, ownerNamed) {
		return
	}
	// T0487: covers string, Optional[string], Vector|Channel|Arc|Weak, and
	// Optional[Vector|Channel|Arc|Weak] in one place.
	c.setDupFlagsForFieldAccess(pt)
}

// maybeEnableDupForConstructorArg sets dupStringFieldAccess or
// dupContainerFieldAccess when a constructor field-init arg is a field read
// on a droppable owner. Without this, the field's inner buffer is shared
// between the owner and the new instance — both end up freeing it. Mirrors
// maybeEnableDupForMutRefArg (T0366) for the constructor field-init path.
// T0411.
func (c *Compiler) maybeEnableDupForConstructorArg(arg ast.Expr, fieldType types.Type) {
	mem, ok := arg.(*ast.MemberExpr)
	if !ok {
		return
	}
	ownerType := c.info.Types[mem.Target]
	if c.typeSubst != nil && ownerType != nil {
		ownerType = types.Substitute(ownerType, c.typeSubst)
	}
	ownerNamed := extractNamed(ownerType)
	// T0513: also accept mono-synthesized drop on generic instances.
	if !c.ownerHasOrSynthDrop(ownerType, ownerNamed) {
		return
	}
	ft := fieldType
	if c.typeSubst != nil {
		ft = types.Substitute(ft, c.typeSubst)
	}
	// T0487: covers string, Optional[string], Vector|Channel|Arc|Weak, and
	// Optional[Vector|Channel|Arc|Weak] in one place.
	c.setDupFlagsForFieldAccess(ft)
}

// genCallArgsWithMutRef evaluates call arguments with MutRef-awareness (B0149).
// For MutRef params, passes the address of the caller's storage instead of the value.
// When the arg needs no coercion and is a simple lvalue, passes the alloca directly.
// Otherwise, evaluates the value, stores in a temp alloca, and passes the temp.
func (c *Compiler) genCallArgsWithMutRef(args []*ast.Arg, params []*types.Param) ([]value.Value, []types.Type, []variadicPassthrough) {
	var argVals []value.Value
	var argTypes []types.Type
	var variadicPTs []variadicPassthrough // B0203: passthrough vectors needing len restored after call
	for i, arg := range args {
		if i < len(params) {
			if _, isMutRef := params[i].Type().(*types.MutRef); isMutRef {
				argType := c.info.Types[arg.Value]
				paramInner := params[i].Type().(*types.MutRef).Elem()
				// Check if the arg type matches the param inner type exactly
				// (no view coercion needed). If so, pass the alloca directly.
				if types.Identical(argType, paramInner) || types.Identical(argType, params[i].Type()) {
					argVals = append(argVals, c.genMutRefArg(arg.Value))
				} else {
					// Coercion needed (e.g., Builder → Writer view).
					// Evaluate normally, coerce, store in temp alloca, pass temp.
					val := c.genCallArgExpr(arg.Value)
					val = c.coerceToView(val, argType, params[i].Type())
					innerType := c.resolveType(params[i].Type())
					tmp := c.createEntryAlloca(innerType)
					c.block.NewStore(val, tmp)
					argVals = append(argVals, tmp)
				}
				argTypes = append(argTypes, c.info.Types[arg.Value])
				continue
			}
		}
		// T0366: For `~` (move) params, when the arg is a field read on a droppable
		// owner, set the dup flag so genFieldAccess produces an independent copy.
		// Without this, the inner buffer is shared between the caller's owner field
		// and the callee — the callee frees it, then the owner's drop frees it again.
		// Only meaningful for auto-dup container types (string, Vector, Channel, etc.).
		isMutRefParam := i < len(params) && params[i].Ref() == types.RefMut
		if isMutRefParam {
			c.maybeEnableDupForMutRefArg(arg.Value, params[i].Type())
		}
		v := c.genCallArgExpr(arg.Value)
		c.dupStringFieldAccess = false
		c.dupContainerFieldAccess = false
		c.dupHeapUserFieldAccess = false // T0403
		argVals = append(argVals, v)
		argTypes = append(argTypes, c.info.Types[arg.Value])
		// T0087: For ~ (move) params, transfer ownership to callee.
		// Clear caller's drop flag and claim string/heap temps so they're not double-freed.
		if isMutRefParam {
			if ident, ok := arg.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
			c.claimStringTemp(v)
			c.claimHeapTemp(v) // B0201: prevent double-free for vector literals passed to ~ params
			// T0522: When the arg is a field-access dup wrapped in an Optional
			// struct, claimStringTemp/claimHeapTemp can't match — `v` is the
			// outer struct, but the inner dup pointer is tracked separately via
			// optionalStringDup/optionalContainerDup. Claim the inner dup so
			// the callee owns it after the consume call and the caller's stmt
			// cleanup doesn't double-free.
			if c.optionalStringDup != nil {
				c.claimStringTemp(c.optionalStringDup)
				c.optionalStringDup = nil
			}
			if c.optionalContainerDup != nil {
				c.claimStringTemp(c.optionalContainerDup)
				c.optionalContainerDup = nil
			}
		}
		// B0203: Variadic passthrough — set static flag (bit 63) on the vector's
		// len field so the callee's scope-exit drop skips element drops and buffer free.
		// Passthrough is detected when the arg is NOT an ArrayLit (ArrayLit means
		// sema synthesized a fresh vector for inline variadic args).
		// Skip if the vector is already static (.rodata) — the memory is read-only
		// and bit 63 is already set, so the callee's drop will skip anyway.
		if i < len(params) && params[i].IsVariadic() {
			if _, isArrayLit := arg.Value.(*ast.ArrayLit); isArrayLit {
				// B0201: Claim the heap temp for freshly synthesized variadic vectors.
				// The callee takes ownership and drops the vector at scope exit.
				c.claimHeapTemp(v)
			} else {
				headerType := vectorHeaderType()
				headerPtr := c.block.NewBitCast(v, irtypes.NewPointer(headerType))
				lenPtr := c.block.NewGetElementPtr(headerType, headerPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
				rawLen := c.block.NewLoad(irtypes.I64, lenPtr)
				// Check if already static (bit 63 set) — skip if so
				bit63 := c.block.NewAnd(rawLen, constant.NewInt(irtypes.I64, math.MinInt64))
				isStatic := c.block.NewICmp(enum.IPredNE, bit63, constant.NewInt(irtypes.I64, 0))
				setBlock := c.newBlock("variadic.setflag")
				skipBlock := c.newBlock("variadic.skipflag")
				c.block.NewCondBr(isStatic, skipBlock, setBlock)
				// Set bit 63
				c.block = setBlock
				flaggedLen := c.block.NewOr(rawLen, constant.NewInt(irtypes.I64, math.MinInt64))
				c.block.NewStore(flaggedLen, lenPtr)
				c.block.NewBr(skipBlock)
				// Continue
				c.block = skipBlock
				variadicPTs = append(variadicPTs, variadicPassthrough{lenPtr: lenPtr, savedLen: rawLen})
			}
		}
	}
	return argVals, argTypes, variadicPTs
}

// variadicPassthrough tracks a vector whose static flag was temporarily set
// for variadic passthrough (B0203).
type variadicPassthrough struct {
	lenPtr   value.Value // pointer to the vector's len field
	savedLen value.Value // original len value before setting bit 63
}

// clearVariadicStaticFlags restores original len values on vectors that were
// temporarily marked static for variadic passthrough (B0203). Only restores
// vectors that were originally non-static (static vectors in .rodata are
// read-only and were never modified).
func (c *Compiler) clearVariadicStaticFlags(passthroughs []variadicPassthrough) {
	for _, pt := range passthroughs {
		// Check if the saved len had bit 63 set (originally static). If so,
		// the vector is .rodata and we never modified it — skip the store.
		bit63 := c.block.NewAnd(pt.savedLen, constant.NewInt(irtypes.I64, math.MinInt64))
		wasStatic := c.block.NewICmp(enum.IPredNE, bit63, constant.NewInt(irtypes.I64, 0))
		restoreBlock := c.newBlock("variadic.restore")
		doneBlock := c.newBlock("variadic.restored")
		c.block.NewCondBr(wasStatic, doneBlock, restoreBlock)
		c.block = restoreBlock
		c.block.NewStore(pt.savedLen, pt.lenPtr)
		c.block.NewBr(doneBlock)
		c.block = doneBlock
	}
}

// genFieldPtr computes a pointer to a field on a user type instance.
// Used by storeBackSlicePtr and genMemberAssign.
func (c *Compiler) genFieldPtr(target *ast.MemberExpr) value.Value {
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	if c.selfSubst != nil {
		targetType = types.SubstituteSelf(targetType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	named := extractNamed(targetType)
	if named == nil {
		panic("codegen: cannot resolve type for field pointer")
	}

	layout := c.lookupTypeLayout(targetType)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for type %s", targetType))
	}

	field := named.LookupField(target.Field)
	if field == nil {
		panic(fmt.Sprintf("codegen: no field %s on type %s", target.Field, named))
	}

	// Value types: GEP directly into the variable's alloca or this pointer
	if layout.IsValueType {
		fieldIdx, ok := layout.ValueFieldIndex[field.Name()]
		if !ok {
			panic(fmt.Sprintf("codegen: field %s not in value layout for %s", field.Name(), named))
		}
		valuePtrType := irtypes.NewPointer(layout.Value.LLVMType)
		if _, isThis := target.Target.(*ast.ThisExpr); isThis {
			// this is an i8* pointing to the value struct
			thisVal := c.genExpr(target.Target)
			typedPtr := c.block.NewBitCast(thisVal, valuePtrType)
			return c.block.NewGetElementPtr(layout.Value.LLVMType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
		}
		// For local variables, get the alloca directly
		if ident, ok := target.Target.(*ast.IdentExpr); ok {
			if alloca, ok := c.locals[ident.Name]; ok {
				typedPtr := c.block.NewBitCast(alloca, valuePtrType)
				return c.block.NewGetElementPtr(layout.Value.LLVMType, typedPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			}
		}
		panic(fmt.Sprintf("codegen: value type field assignment requires addressable target for %s.%s", named, field.Name()))
	}

	fieldIdx, ok := layout.InstanceFieldIndex[field.Name()]
	if !ok {
		panic(fmt.Sprintf("codegen: field %s not in layout for %s", field.Name(), named))
	}

	obj := c.genExpr(target.Target)
	var instance value.Value
	if _, isThis := target.Target.(*ast.ThisExpr); isThis {
		instance = obj
	} else {
		instance = c.extractInstancePtr(obj)
	}
	typedPtr := c.block.NewBitCast(instance, layout.InstancePtrType)

	return c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
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

// --- Enum variant values ---

// genEnumVariantValueLayout generates a fieldless enum variant value using layout dispatch.
func (c *Compiler) genEnumVariantValueLayout(layout *TypeDeclLayout, variantName string) value.Value {
	tag, ok := layout.VariantTag[variantName]
	if !ok {
		panic(fmt.Sprintf("codegen: variant %q not found in enum layout", variantName))
	}

	if layout.MaxVariantDataSize == 0 {
		return constant.NewInt(irtypes.I32, int64(tag))
	}

	internalType := layout.EnumInternalType.(*irtypes.StructType)
	var agg value.Value = constant.NewZeroInitializer(internalType)
	agg = c.block.NewInsertValue(agg, constant.NewInt(irtypes.I32, int64(tag)), 0)
	return agg
}

// genEnumVariantCallLayout generates a variant constructor call using layout dispatch.
func (c *Compiler) genEnumVariantCallLayout(e *ast.CallExpr, member *ast.MemberExpr, layout *TypeDeclLayout) value.Value {
	tag, ok := layout.VariantTag[member.Field]
	if !ok {
		panic(fmt.Sprintf("codegen: variant %q not found in enum layout", member.Field))
	}
	dataType := layout.VariantDataTypes[member.Field]

	internalType := layout.EnumInternalType.(*irtypes.StructType)
	alloca := c.createEntryAlloca(internalType)

	tagPtr := c.block.NewGetElementPtr(internalType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewStore(constant.NewInt(irtypes.I32, int64(tag)), tagPtr)

	// B0252: Track whether any field value was moved from a variable with a
	// drop binding. If so, the enum now owns heap resources via the moved
	// variable, and the temp must NOT be dropped at statement end — when the
	// enum is passed by value to a function that stores it (e.g., Map.[]=),
	// the temp drop would free resources shared with the stored copy.
	movedDroppable := false
	if dataType != nil && len(e.Args) > 0 {
		dataPtr := c.block.NewGetElementPtr(internalType, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		typedDataPtr := c.block.NewBitCast(dataPtr, irtypes.NewPointer(dataType))
		for i, arg := range e.Args {
			// T0608: Coerce the arg to the declared variant field type before
			// storing. Mirrors the struct-constructor Optional widening path:
			// when the variant field is `T?` and the argument is a bare `T`
			// (or `none`), build the `{i1, payload}` Optional aggregate before
			// NewStore. Without this the store operands are incompatible
			// (src=payload; dst={i1,payload}*).
			fieldLLVM := dataType.Fields[i]
			// Resolve the declared variant field Promise type via the same
			// helper match-destructure uses (handles generic enums / typeSubst
			// identically). Falls back to the unresolved type if not found.
			var vfType types.Type
			if enum := extractEnum(c.info.Types[member.Target]); enum != nil {
				if variant := enum.LookupVariant(member.Field); variant != nil && i < variant.NumFields() {
					vfType = c.resolveMatchFieldType(variant.Fields()[i].Type(),
						c.info.Types[member.Target], enum)
				}
			}
			// T0630: resolveMatchFieldType only concretizes via the Instance.TypeArgs()
			// path; inside a generic fn/method body that path produces an identity map
			// ({T→T}), leaving T? unchanged. Apply c.typeSubst symmetrically with the
			// exprType substitution below so Identical() compares concrete types.
			if c.typeSubst != nil && vfType != nil {
				vfType = types.Substitute(vfType, c.typeSubst)
			}
			var val, preWrapVal value.Value
			if _, isOpt := vfType.(*types.Optional); isOpt {
				if _, isNone := arg.Value.(*ast.NoneLit); isNone {
					// B0210: generate the none value directly from the layout's
					// already-monomorphized LLVM type rather than resolveType,
					// which may mis-lower under partial TypeParam substitution.
					val = c.zeroValue(fieldLLVM)
					preWrapVal = val
				} else {
					savedTarget := c.targetType
					c.targetType = vfType
					preWrapVal = c.genCallArgExpr(arg.Value)
					c.targetType = savedTarget
					val = preWrapVal
					exprType := c.info.Types[arg.Value]
					if c.typeSubst != nil && exprType != nil {
						exprType = types.Substitute(exprType, c.typeSubst)
					}
					// Leave an explicit `T?` arg unwrapped (Identical) — that
					// path already stored a matching aggregate before T0608.
					if exprType != types.TypNone && !types.Identical(exprType, vfType) {
						if st, ok := fieldLLVM.(*irtypes.StructType); ok {
							val = c.wrapOptional(preWrapVal, st)
						}
					}
				}
			} else {
				val = c.genCallArgExpr(arg.Value)
				preWrapVal = val
			}
			fieldPtr := c.block.NewGetElementPtr(dataType, typedDataPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
			c.block.NewStore(val, fieldPtr)
			// Clear drop flag: field value is moved into the enum variant
			if ident, ok := arg.Value.(*ast.IdentExpr); ok {
				if _, hasFlag := c.dropFlags[ident.Name]; hasFlag {
					movedDroppable = true
				}
				c.clearDropFlag(ident.Name)
			} else if !movedDroppable {
				// B0286: Function/method calls returning droppable values
				// transfer ownership to the enum variant. Skip B0267 temp
				// tracking to prevent double-free when the enum is passed by
				// value to a function that stores it (e.g., Map.[]=).
				// Only applies to real function calls (Signature callee), not
				// type constructors (Named/Instance callee) — constructors
				// need B0267 as the only cleanup path.
				if ce, isCall := arg.Value.(*ast.CallExpr); isCall {
					if calleeType := c.info.Types[ce.Callee]; calleeType != nil {
						if _, isSig := calleeType.(*types.Signature); isSig {
							if argType := c.info.Types[arg.Value]; argType != nil {
								if c.typeSubst != nil {
									argType = types.Substitute(argType, c.typeSubst)
								}
								if argTypeIsDroppable(argType) {
									movedDroppable = true
								}
							}
						}
					}
				}
			}
			// B0278: Claim string temp: string method results (e.g., to_upper())
			// stored into enum variant data transfer ownership to the enum.
			// Without this, the stmtTemp cleanup drops the string at statement
			// end even though it's now owned by the enum variant.
			c.claimStringTemp(val)
			// Claim heap temp: user type instances stored into enum variant data
			// transfer ownership to the enum. Without this, the heap temp cleanup
			// would free the instance, leaving a dangling pointer in the enum.
			c.claimHeapTemp(val)
			c.claimEnvTemp(val) // B0278: claim env temp for closure args in enum variants
			// T0608: For Optional variant fields with droppable inner types
			// (string?, int[]?, map[K,V]?), the wrapped {i1, ptr} aggregate
			// won't match the stmtTemp/heap/env maps keyed on the bare inner
			// pointer. Claim the pre-wrap value too so the inner allocation
			// isn't dropped at statement end while the enum still owns it
			// (T0067 zero-tolerance double-free/leak).
			if preWrapVal != val {
				c.claimStringTemp(preWrapVal)
				c.claimHeapTemp(preWrapVal)
				c.claimEnvTemp(preWrapVal)
			}
		}
	}

	// B0267: Track the enum alloca for cleanup at statement end. Uses entry-block
	// allocas so the tracking dominates all uses regardless of branch structure.
	// B0252: Skip tracking when a variable with a drop binding was moved into the
	// variant data — the enum now owns those resources, and dropping the temp would
	// free resources shared with any by-value copy (e.g., stored in a Map Slot).
	if dataType != nil && !movedDroppable && c.entryBlock != nil && c.tempTrackingEnabled {
		enumType := c.info.Types[member.Target]
		if c.typeSubst != nil {
			enumType = types.Substitute(enumType, c.typeSubst)
		}
		var enumName string
		if inst, ok := enumType.(*types.Instance); ok {
			enumName = monoName(inst)
		} else if en, ok := enumType.(*types.Enum); ok {
			enumName = en.Obj().Name()
		}
		if enumName != "" {
			mangledDrop := mangleMethodName(enumName, "drop", false)
			if dropFunc, ok := c.funcs[mangledDrop]; ok {
				// Create entry-block allocas for the pointer and drop flag.
				ptrAlloca := c.createEntryAlloca(irtypes.I8Ptr)
				flagAlloca := c.createEntryAlloca(irtypes.I1)
				c.entryBlock.NewStore(constant.NewNull(irtypes.I8Ptr), ptrAlloca)
				c.entryBlock.NewStore(constant.NewInt(irtypes.I1, 0), flagAlloca)
				// Store the bitcast of the enum alloca and set the flag.
				ptr := c.block.NewBitCast(alloca, irtypes.I8Ptr)
				c.block.NewStore(ptr, ptrAlloca)
				c.block.NewStore(constant.NewInt(irtypes.I1, 1), flagAlloca)
				// B0269: Append to slice so multiple inline constructors are all tracked.
				c.enumCtorTemps = append(c.enumCtorTemps, enumCtorTemp{
					alloca: ptrAlloca, dropFlag: flagAlloca, dropFunc: dropFunc,
				})
			}
		}
	}

	return c.block.NewLoad(internalType, alloca)
}

// --- Match expressions ---

// genMatchExpr generates a match expression. Dispatches to enum match (tag-based switch)
// or value match (literal comparison chain) based on subject type.
func (c *Compiler) genMatchExpr(e *ast.MatchExpr) value.Value {
	subject := c.genExpr(e.Subject)
	subjectType := c.info.Types[e.Subject]
	// Apply typeSubst for mono context
	if c.typeSubst != nil {
		subjectType = types.Substitute(subjectType, c.typeSubst)
	}
	// T0551: Inside a monomorphized generic enum method, `match this` records the
	// receiver as the bare generic *types.Enum (TypeParams live in variant fields,
	// not the enum head, so types.Substitute leaves it unchanged). Resolve it to the
	// concrete instance so droppable-TypeArg detection (enumInstanceHasDrop) and
	// variant-field type resolution (resolveMatchFieldType) operate on the real
	// substituted types — otherwise a generic `clone enum with a droppable TypeArg
	// (e.g. MaybeMap[map[..]]) shallow-aliases the payload and double-frees.
	if c.monoCtx != nil && c.monoCtx.inst != nil {
		if subjEnum, ok := subjectType.(*types.Enum); ok {
			if origin, ok := c.monoCtx.origin.(*types.Enum); ok && subjEnum == origin {
				subjectType = c.monoCtx.inst
			}
		}
	}

	if enumLayout := c.lookupEnumLayout(subjectType); enumLayout != nil {
		enum := extractEnum(subjectType)
		// If subject is i8* (e.g., `this` inside an enum method), load the enum value
		if subject.Type().Equal(irtypes.I8Ptr) {
			var loadType irtypes.Type
			if enumLayout.MaxVariantDataSize == 0 {
				loadType = irtypes.I32 // fieldless enum: tag only
			} else {
				loadType = enumLayout.EnumInternalType // data enum: {i32 tag, [N x i8] data}
			}
			typedPtr := c.block.NewBitCast(subject, irtypes.NewPointer(loadType))
			subject = c.block.NewLoad(loadType, typedPtr)
		}
		// B0232: Check if this enum instance has a drop (synthesized or explicit).
		// If so, string fields extracted via match destructuring must be dup'd
		// to prevent double-frees when the enum element is later dropped
		// (e.g., Slot[K,V] in Map._buckets).
		enumHasDrop := c.enumInstanceHasDrop(subjectType, enum)
		return c.genEnumMatch(e, e.Subject, subject, enum, enumLayout, enumHasDrop, subjectType)
	}

	return c.genValueMatch(e, subject, subjectType)
}

// enumInstanceHasDrop returns true if an enum type (possibly monomorphized) has a drop function.
// Checks both sema-level detection and codegen-time mono synthesized drops.
func (c *Compiler) enumInstanceHasDrop(subjectType types.Type, enum *types.Enum) bool {
	if enum.HasDrop() || enum.NeedsSynthDrop() {
		return true
	}
	// Check for codegen-time mono synthesized drop (generic enums with droppable TypeParam fields)
	if inst, ok := subjectType.(*types.Instance); ok {
		mangledName := mangleMethodName(monoName(inst), "drop", false)
		_, ok := c.funcs[mangledName]
		return ok
	}
	return false
}

// genEnumMatch generates a match expression on an enum value using an LLVM switch instruction.
func (c *Compiler) genEnumMatch(e *ast.MatchExpr, subjectExpr ast.Expr, subject value.Value, enum *types.Enum, layout *TypeDeclLayout, enumHasDrop bool, subjectType types.Type) value.Value {
	// Extract tag from subject
	var tag value.Value
	if layout.MaxVariantDataSize == 0 {
		tag = subject // fieldless enum, subject IS the tag
	} else {
		tag = c.block.NewExtractValue(subject, 0)
	}

	switchBlock := c.block
	mergeBlock := c.newBlock("match.end")

	var defaultTarget *ir.Block
	var cases []*ir.Case
	var arms []matchArmInfo

	for i, arm := range e.Arms {
		armBlock := c.newBlock(fmt.Sprintf("match.arm%d", i))

		switch p := arm.Pattern.(type) {
		case *ast.EnumVariantMatchPattern:
			tagVal := constant.NewInt(irtypes.I32, int64(layout.VariantTag[p.Variant]))
			cases = append(cases, &ir.Case{X: tagVal, Target: armBlock})

		case *ast.EnumDestructureMatchPattern:
			tagVal := constant.NewInt(irtypes.I32, int64(layout.VariantTag[p.Variant]))
			cases = append(cases, &ir.Case{X: tagVal, Target: armBlock})

		case *ast.ShortDestructureMatchPattern:
			tagVal := constant.NewInt(irtypes.I32, int64(layout.VariantTag[p.Name]))
			cases = append(cases, &ir.Case{X: tagVal, Target: armBlock})

		case *ast.WildcardMatchPattern:
			defaultTarget = armBlock

		case *ast.NameMatchPattern:
			defaultTarget = armBlock
		}

		// Generate arm body
		c.block = armBlock
		if c.shouldInstrument() {
			pos := arm.Pattern.Pos()
			var endPos int
			if arm.Block != nil {
				endPos = arm.Block.End().Line
			} else if arm.Body != nil {
				endPos = arm.Body.End().Line
			} else {
				endPos = pos.Line
			}
			idx := c.addCoverageRegion(pos.File, pos.Line, endPos, c.currentCoverageFuncName(), "match.arm")
			c.emitCoverageIncrement(idx)
		}
		// T0109: Save scope depth before binding match pattern. Dup'd bindings
		// from destructured enum fields (strings, vectors, etc.) are registered
		// as scope bindings via maybeRegisterDrop. They must be cleaned up when
		// the arm falls through to match.end (scope cleanup here) or when the
		// arm exits early via return/break (handled by emitScopeCleanup in those paths).
		armScopeLen := len(c.scopeBindings)
		// T0485: Snapshot match-borrow markers present before this arm so any
		// added by this arm's bindings can be reverted at arm end. The bound
		// idents are arm-scoped; without this, later code in the function that
		// reuses the binding name (e.g., declaring a new owned Optional) would
		// inherit the stale "borrowed" marker and disable correct ownership
		// transfer in if-let unwraps.
		var armBorrowedSnapshot map[string]bool
		if len(c.matchBorrowedIdents) > 0 {
			armBorrowedSnapshot = make(map[string]bool, len(c.matchBorrowedIdents))
			for k := range c.matchBorrowedIdents {
				armBorrowedSnapshot[k] = true
			}
		}
		c.bindMatchPattern(arm.Pattern, subjectExpr, subject, enum, layout, enumHasDrop, subjectType)

		var armVal value.Value
		if arm.Body != nil {
			armVal = c.genExpr(arm.Body)
		} else if arm.Block != nil {
			armVal = c.genBlockValue(arm.Block)
		}
		c.claimStringTemp(armVal) // T0073: ownership transfers to match phi

		// B0242: Clear drop flags for dup'd bindings consumed by the arm result.
		// When the arm body returns a dup'd binding's value (directly or via a
		// block's last expression), the value's ownership transfers to the match
		// PHI. The arm-scope cleanup must NOT drop it (use-after-free).
		// clearDropFlag is a no-op if the name has no drop flag.
		c.clearMatchArmResultDropFlags(*arm)

		// T0109/B0242: Clean up dup'd match bindings at arm end (fall-through path).
		// Only bindings whose drop flag is still true (not consumed) are dropped.
		if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > armScopeLen {
			c.emitScopeCleanup(armScopeLen, false)
		}
		c.scopeBindings = c.scopeBindings[:armScopeLen]

		// T0485: Revert match-borrow markers added in this arm. Entries present
		// before the arm are kept (they belong to an outer match in a nested
		// scenario); entries newly added are removed.
		for k := range c.matchBorrowedIdents {
			if !armBorrowedSnapshot[k] {
				delete(c.matchBorrowedIdents, k)
			}
		}

		armEnd := c.block
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}

		arms = append(arms, matchArmInfo{val: armVal, end: armEnd, hasV: armVal != nil})
	}

	if defaultTarget == nil {
		// Exhaustive match — default case is unreachable.
		// We must NOT route to mergeBlock because the phi has no incoming for this edge.
		unreachableBlock := c.newBlock("match.unreachable")
		unreachableBlock.NewUnreachable()
		defaultTarget = unreachableBlock
	}

	switchBlock.NewSwitch(tag, defaultTarget, cases...)

	c.block = mergeBlock
	return buildMatchPhi(mergeBlock, arms)
}

// matchArmInfo tracks a match arm's result value and final block for PHI construction.
type matchArmInfo struct {
	val  value.Value
	end  *ir.Block
	hasV bool
}

// buildMatchPhi constructs a PHI node at mergeBlock from collected match arm info.
// Arms that branch to mergeBlock but produce no value get a null placeholder.
// Returns nil if no arm produces a value (match used as statement).
func buildMatchPhi(mergeBlock *ir.Block, arms []matchArmInfo) value.Value {
	// Filter out void-typed values — they cannot participate in phi nodes.
	for i := range arms {
		if arms[i].val != nil {
			if _, isVoid := arms[i].val.Type().(*irtypes.VoidType); isVoid {
				arms[i].val = nil
				arms[i].hasV = false
			}
		}
	}

	hasAnyValue := false
	for _, a := range arms {
		if a.hasV {
			hasAnyValue = true
			break
		}
	}
	if !hasAnyValue {
		return nil
	}

	// Find a representative non-nil value type for zero-filling arms without values.
	var valType irtypes.Type
	for _, a := range arms {
		if a.hasV && a.val != nil {
			valType = a.val.Type()
			break
		}
	}

	var incomings []*ir.Incoming
	for _, a := range arms {
		// Skip arms that don't branch to mergeBlock (e.g. early return/break)
		branchesToMerge := false
		if a.end.Term != nil {
			if br, ok := a.end.Term.(*ir.TermBr); ok && br.Target == mergeBlock {
				branchesToMerge = true
			}
		}
		if !branchesToMerge {
			continue
		}
		v := a.val
		if v == nil && valType != nil {
			v = constant.NewZeroInitializer(valType)
		} else if v == nil {
			v = constant.NewNull(irtypes.I8Ptr)
		}
		incomings = append(incomings, &ir.Incoming{X: v, Pred: a.end})
	}
	if len(incomings) > 0 {
		return mergeBlock.NewPhi(incomings...)
	}
	return nil
}

// clearMatchArmResultDropFlags clears drop flags for identifiers that appear as
// the arm's result expression. This prevents use-after-free when a dup'd match
// binding is consumed by the match PHI — the arm-scope cleanup must skip it.
// B0242: Walks into if/match sub-expressions to handle conditional returns like
// `if cond { v } else { "other" }` where v is a dup'd binding.
func (c *Compiler) clearMatchArmResultDropFlags(arm ast.MatchArm) {
	if arm.Body != nil {
		c.clearResultDropFlags(arm.Body)
	} else if arm.Block != nil {
		c.clearBlockResultDropFlags(arm.Block)
	}
}

// clearResultDropFlags recursively clears drop flags for identifiers in an
// expression that will be consumed as a scope result (match arm, if branch, etc.).
func (c *Compiler) clearResultDropFlags(expr ast.Expr) {
	if expr == nil {
		return
	}
	switch e := expr.(type) {
	case *ast.IdentExpr:
		c.clearDropFlag(e.Name)
	case *ast.IfExpr:
		// Both branches may produce a result
		c.clearBlockResultDropFlags(e.Then)
		if e.Else != nil {
			c.clearBlockResultDropFlags(e.Else)
		}
	case *ast.MatchExpr:
		// Each arm may produce a result
		for _, arm := range e.Arms {
			c.clearMatchArmResultDropFlags(*arm)
		}
	}
	// For other expression types (calls, binary ops, etc.), the consumption
	// happens at inner call sites which already clear drop flags.
}

// clearBlockResultDropFlags clears drop flags for identifiers in the last
// statement of a block (the block's result value).
func (c *Compiler) clearBlockResultDropFlags(block *ast.Block) {
	if block == nil || len(block.Stmts) == 0 {
		return
	}
	if es, ok := block.Stmts[len(block.Stmts)-1].(*ast.ExprStmt); ok {
		c.clearResultDropFlags(es.Expr)
	}
}

// genValueMatch generates a match expression on a non-enum value using comparison chains.
func (c *Compiler) genValueMatch(e *ast.MatchExpr, subject value.Value, subjectType types.Type) value.Value {
	mergeBlock := c.newBlock("match.end")

	named := extractNamed(subjectType)

	var arms []matchArmInfo

	for i, arm := range e.Arms {
		switch p := arm.Pattern.(type) {
		case *ast.LiteralMatchPattern, *ast.ExpressionMatchPattern:
			var patternVal value.Value
			switch pp := p.(type) {
			case *ast.LiteralMatchPattern:
				patternVal = c.genExpr(pp.Value)
			case *ast.ExpressionMatchPattern:
				patternVal = c.genExpr(pp.Expr)
			}

			var cond value.Value
			if named != nil {
				method := named.LookupMethod("==")
				if method != nil && method.IsNative() {
					if named == types.TypString {
						cond = c.genStringOp("==", subject, patternVal)
					} else {
						cond = c.emitNativeOp(named, "==", subject, patternVal)
					}
				}
			}
			if cond == nil {
				panic(fmt.Sprintf("codegen: cannot compare match subject of type %s", subjectType))
			}

			armBlock := c.newBlock(fmt.Sprintf("match.arm%d", i))
			nextBlock := c.newBlock(fmt.Sprintf("match.next%d", i))
			c.block.NewCondBr(cond, armBlock, nextBlock)

			c.block = armBlock
			var armVal value.Value
			if arm.Body != nil {
				armVal = c.genExpr(arm.Body)
			} else if arm.Block != nil {
				armVal = c.genBlockValue(arm.Block)
			}
			c.claimStringTemp(armVal) // T0073
			armEnd := c.block
			if c.block.Term == nil {
				c.block.NewBr(mergeBlock)
			}
			arms = append(arms, matchArmInfo{val: armVal, end: armEnd, hasV: armVal != nil})

			c.block = nextBlock

		case *ast.WildcardMatchPattern, *ast.NameMatchPattern:
			// Bind name pattern variable (needed before evaluating guard)
			bindBlock := c.newBlock(fmt.Sprintf("match.bind%d", i))
			c.block.NewBr(bindBlock)
			c.block = bindBlock

			if np, ok := p.(*ast.NameMatchPattern); ok && np.Name != "_" {
				lt := subject.Type()
				alloca := c.createEntryAlloca(lt)
				alloca.SetName(c.uniqueLocalName(np.Name))
				c.block.NewStore(subject, alloca)
				c.locals[np.Name] = alloca
			}

			// If there's a guard, evaluate it and conditionally branch
			if arm.Guard != nil {
				guardVal := c.genExpr(arm.Guard)
				armBlock := c.newBlock(fmt.Sprintf("match.arm%d", i))
				nextBlock := c.newBlock(fmt.Sprintf("match.next%d", i))
				c.block.NewCondBr(guardVal, armBlock, nextBlock)

				c.block = armBlock
				var armVal value.Value
				if arm.Body != nil {
					armVal = c.genExpr(arm.Body)
				} else if arm.Block != nil {
					armVal = c.genBlockValue(arm.Block)
				}
				c.claimStringTemp(armVal) // T0073
				armEnd := c.block
				if c.block.Term == nil {
					c.block.NewBr(mergeBlock)
				}
				arms = append(arms, matchArmInfo{val: armVal, end: armEnd, hasV: armVal != nil})

				c.block = nextBlock
				// Guard failed — continue to next arm (don't return early)
			} else {
				// No guard — unconditional default arm
				var armVal value.Value
				if arm.Body != nil {
					armVal = c.genExpr(arm.Body)
				} else if arm.Block != nil {
					armVal = c.genBlockValue(arm.Block)
				}
				c.claimStringTemp(armVal) // T0073
				armEnd := c.block
				if c.block.Term == nil {
					c.block.NewBr(mergeBlock)
				}
				arms = append(arms, matchArmInfo{val: armVal, end: armEnd, hasV: armVal != nil})

				// After an unguarded wildcard/name, no more arms need checking
				c.block = mergeBlock
				return buildMatchPhi(mergeBlock, arms)
			}
		}
	}

	// If we fell through without a default, branch to merge
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	c.block = mergeBlock
	return buildMatchPhi(mergeBlock, arms)
}

// bindMatchPattern binds pattern variables from a match arm into the current scope.
func (c *Compiler) bindMatchPattern(pat ast.MatchPattern, subjectExpr ast.Expr, subject value.Value, enum *types.Enum, layout *TypeDeclLayout, enumHasDrop bool, subjectType types.Type) {
	switch p := pat.(type) {
	case *ast.EnumDestructureMatchPattern:
		c.bindEnumDestructure(p.Bindings, p.Variant, subjectExpr, subject, enum, layout, enumHasDrop, subjectType)

	case *ast.ShortDestructureMatchPattern:
		c.bindEnumDestructure(p.Bindings, p.Name, subjectExpr, subject, enum, layout, enumHasDrop, subjectType)

	case *ast.NameMatchPattern:
		if p.Name != "_" {
			lt := subject.Type()
			alloca := c.createEntryAlloca(lt)
			c.block.NewStore(subject, alloca)
			c.locals[p.Name] = alloca
		}

	case *ast.EnumVariantMatchPattern:
		// No bindings for fieldless variant patterns

	case *ast.WildcardMatchPattern:
		// No bindings
	}
}

// bindEnumDestructure extracts variant data fields and binds them to local variables.
// B0232: When enumHasDrop is true and a field resolves to string, the extracted value
// is dup'd to prevent double-frees when the enum element is later dropped (e.g., Slot
// elements in Map._buckets). Dup'd bindings get drop flags and scope bindings for
// proper cleanup in loops and at scope exit.
//
// T0623: a destructure arm binding (non-`_`) a variant field whose resolved
// type transitively owns a single-owner handle (Task/Mutex/MutexGuard) moves
// out: the binding takes ownership (registered for drop on scope exit) and the
// subject's synth enum drop flag is cleared so the matched variant is not
// double-freed. This branch precedes the dup / borrow branches because a
// single-owner handle is never dup-cloneable.
func (c *Compiler) bindEnumDestructure(bindings []string, variantName string, subjectExpr ast.Expr, subject value.Value, enum *types.Enum, layout *TypeDeclLayout, enumHasDrop bool, subjectType types.Type) {
	variant := enum.LookupVariant(variantName)
	if variant == nil || variant.NumFields() == 0 {
		return
	}

	dataType := layout.VariantDataTypes[variantName]
	if dataType == nil {
		return
	}

	// Alloca the subject struct and GEP to data area.
	// EnumInternalType is guaranteed to be a struct here because we returned early
	// above when variant has no fields (which is the only case where it would be i32).
	internalType := layout.EnumInternalType.(*irtypes.StructType)
	alloca := c.createEntryAlloca(internalType)
	c.block.NewStore(subject, alloca)

	dataPtr := c.block.NewGetElementPtr(internalType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	typedDataPtr := c.block.NewBitCast(dataPtr, irtypes.NewPointer(dataType))

	// T0623: compute once whether this arm moves the subject. Used to (a) skip
	// the dup/borrow path for the handle binding, (b) null out the moved-out
	// field in the SUBJECT's alloca so the synth enum drop, when it later runs
	// on the subject, sees a null handle pointer in that slot and skips it
	// (single-owner-handle drops null-check). Nulling the slot instead of
	// suppressing the whole synth drop lets other droppable variant fields
	// (e.g. a sibling string in a Multi(string, Task) variant) still be freed
	// — clearing the flag would leak them.
	armMoves, subjectIdent := c.armDestructureMovesSubject(variant, bindings, subjectExpr, subjectType, enum)

	for i, binding := range bindings {
		if binding == "_" {
			continue
		}
		if i >= variant.NumFields() {
			break
		}
		// Use layout data type fields (already substituted for mono types)
		fieldType := dataType.Fields[i]
		fieldPtr := c.block.NewGetElementPtr(dataType, typedDataPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
		val := c.block.NewLoad(fieldType, fieldPtr)

		declaredFieldType := variant.Fields()[i].Type()
		resolved := c.resolveMatchFieldType(declaredFieldType, subjectType, enum)

		// T0623: move-out path for single-owner handles. Owns the handle in the
		// binding (drop registered, freed on scope exit / consumed via <-t) and
		// nulls the corresponding slot in the SUBJECT's alloca so the synth
		// enum drop sees null and skips that slot. Other droppable variant
		// fields (e.g. a sibling string) are still freed by the synth drop.
		if armMoves && sema.FirstNestedSingleOwnerHandle(resolved) != nil {
			bindAlloca := c.createEntryAlloca(fieldType)
			c.block.NewStore(val, bindAlloca)
			c.locals[binding] = bindAlloca
			c.maybeRegisterDrop(binding, bindAlloca, resolved)
			c.nullSubjectHandleSlot(subjectIdent, internalType, dataType, i, fieldType)
			continue
		}

		// B0232/B0236: Dup droppable fields from droppable enums to create independent copies.
		// Without this, match-extracted values share instance pointers with the enum
		// data. When the enum element is dropped (e.g., Map._buckets scope exit or
		// Map destruction), the shared value would be double-freed.
		// B0285: Skip dup inside enum clone methods — the synthesized body
		// explicitly clones every non-copy variant field: concrete fields via
		// .clone()/if-let, and any TypeParam-containing field (`T`, `T?`, `T[]`,
		// `[N]T`) via the synth-only AutoCloneExpr intrinsic (T0607). Match-dup
		// here would therefore double-clone. suppressMatchDup is set true only
		// inside enum clone method bodies; elsewhere it is false and match-dup is
		// unaffected.
		if enumHasDrop && !c.suppressMatchDup && c.matchFieldNeedsDup(declaredFieldType, subjectType, enum) {
			c.dupMatchBinding(binding, val, fieldType, resolved)
			continue
		}

		bindAlloca := c.createEntryAlloca(fieldType)
		c.block.NewStore(val, bindAlloca)
		c.locals[binding] = bindAlloca

		// T0485: Mark Optional/Array variant field bindings as match-borrowed
		// when the enum has a drop. The variant data owns the inner heap value;
		// the bound variable is just a copy that aliases it. Without this mark,
		// `if x := optBinding` (and similar unwraps) would treat the bound
		// variable as owned and transfer ownership, causing double-free with
		// the synth enum drop's Optional/Array walk.
		if enumHasDrop {
			if c.matchBindingIsBorrow(resolved) {
				if c.matchBorrowedIdents == nil {
					c.matchBorrowedIdents = make(map[string]bool)
				}
				c.matchBorrowedIdents[binding] = true
			}
		}
	}
}

// nullSubjectHandleSlot zeroes the moved-out variant-field slot in the
// SUBJECT's enum-value alloca so the synth enum drop, walking the subject at
// outer scope exit, sees null in that slot and skips it (single-owner-handle
// drop functions all null-check before freeing). No-op when the subject ident
// has no entry in c.locals (defensive — shouldn't happen given the sema gate
// already required an owned-local ident). (T0623)
//
// T0633: the slot is not always a bare pointer. A direct Task/Mutex/MutexGuard
// field lowers to i8*, but the predicate (FirstNestedSingleOwnerHandle) also
// matches a handle nested in a user-type wrapper ({i8*,i8*} value struct), an
// Optional ({i1,T}), a tuple, or a fixed array ([N x i8*]). zeroinitializer is
// valid LLVM for every one of those aggregates (and zero-fills all nested
// pointers to null); a bare pointer slot keeps the exact NewNull idiom so the
// existing direct-handle IR is byte-identical (no T0623 regression). The
// subject's synth enum drop then walks the zeroed slot with all instance/
// element pointers null and skips it — the instance-deref drop branches in
// emitVariantFieldDrop are null-guarded, and the Optional/array element walks
// already null-check. (c.zeroValue is not used: its default arm returns an
// i64 0, which is store-incompatible with an [N x i8*] array slot.)
func (c *Compiler) nullSubjectHandleSlot(subjectIdent string, internalType *irtypes.StructType, dataType *irtypes.StructType, fieldIdx int, fieldType irtypes.Type) {
	if subjectIdent == "" {
		return
	}
	subjAlloca, ok := c.locals[subjectIdent]
	if !ok {
		return
	}
	subjDataPtr := c.block.NewGetElementPtr(internalType, subjAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	subjTypedDataPtr := c.block.NewBitCast(subjDataPtr, irtypes.NewPointer(dataType))
	subjFieldPtr := c.block.NewGetElementPtr(dataType, subjTypedDataPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
	var zero constant.Constant
	if pt, isPtr := fieldType.(*irtypes.PointerType); isPtr {
		zero = constant.NewNull(pt)
	} else {
		zero = constant.NewZeroInitializer(fieldType)
	}
	c.block.NewStore(zero, subjFieldPtr)
}

// armDestructureMovesSubject mirrors the ownership predicate: returns true and
// the subject ident name when this arm's destructure binds (non-`_`) a variant
// field whose resolved type transitively owns a single-owner handle AND the
// subject is an owned-local ident (sema gate already enforced that owned-local
// invariant; this just discovers the ident name for the null-out store). (T0623)
func (c *Compiler) armDestructureMovesSubject(variant *types.Variant, bindings []string, subjectExpr ast.Expr, subjectType types.Type, enum *types.Enum) (bool, string) {
	if variant == nil || subjectExpr == nil {
		return false, ""
	}
	id, ok := subjectExpr.(*ast.IdentExpr)
	if !ok {
		return false, ""
	}
	n := len(bindings)
	if n > variant.NumFields() {
		n = variant.NumFields()
	}
	for i := 0; i < n; i++ {
		if bindings[i] == "_" {
			continue
		}
		ft := c.resolveMatchFieldType(variant.Fields()[i].Type(), subjectType, enum)
		if sema.FirstNestedSingleOwnerHandle(ft) != nil {
			return true, id.Name
		}
	}
	return false, ""
}

// matchBindingIsBorrow reports whether the bound variant-field type holds a
// droppable inner that the variant data still owns (i.e., the binding aliases
// rather than owns). T0485: Optional/Array variant fields that contain heap
// values are bound as borrows because dupping them would require recursive
// deep-clone logic; the synth enum drop walks the variant data and frees
// the inner value, so the binding must not transfer ownership.
func (c *Compiler) matchBindingIsBorrow(resolved types.Type) bool {
	if opt, ok := resolved.(*types.Optional); ok {
		return c.borrowInnerHasDrop(opt.Elem())
	}
	if arr, ok := resolved.(*types.Array); ok {
		return c.borrowInnerHasDrop(arr.Elem())
	}
	return false
}

// borrowInnerHasDrop returns true if a type wrapped inside an Optional/Array
// variant field holds any droppable subterm. Recurses through Tuple/Optional/
// Array; defers to fieldTypeNeedsDrop for leaf cases.
func (c *Compiler) borrowInnerHasDrop(typ types.Type) bool {
	if tup, ok := typ.(*types.Tuple); ok {
		for _, e := range tup.Elems() {
			if c.borrowInnerHasDrop(e) {
				return true
			}
		}
		return false
	}
	if opt, ok := typ.(*types.Optional); ok {
		return c.borrowInnerHasDrop(opt.Elem())
	}
	if arr, ok := typ.(*types.Array); ok {
		return c.borrowInnerHasDrop(arr.Elem())
	}
	return fieldTypeNeedsDrop(typ)
}

// resolveMatchFieldType resolves a match-destructured field's type using enum
// instance substitution. B0232: Must build the substitution from the enum
// instance's TypeParams (not the owner type's TypeParams) since variant fields
// reference the enum's own TypeParams.
func (c *Compiler) resolveMatchFieldType(fieldType types.Type, subjectType types.Type, enum *types.Enum) types.Type {
	var subst map[*types.TypeParam]types.Type
	if inst, ok := subjectType.(*types.Instance); ok && enum != nil && len(enum.TypeParams()) > 0 {
		subst = types.BuildSubstMap(enum.TypeParams(), inst.TypeArgs())
	} else if c.typeSubst != nil {
		subst = c.typeSubst
	}
	resolved := fieldType
	if subst != nil {
		resolved = types.Substitute(resolved, subst)
	}
	return resolved
}

// matchFieldNeedsDup returns true if a match-destructured field should be dup'd.
// B0236: Extended from strings-only to also cover vectors, channels, and safe
// heap user types. Prevents double-frees when match-extracted values share
// instance pointers with enum data that will be dropped.
func (c *Compiler) matchFieldNeedsDup(fieldType types.Type, subjectType types.Type, enum *types.Enum) bool {
	resolved := c.resolveMatchFieldType(fieldType, subjectType, enum)
	return c.typeNeedsMatchDup(resolved)
}

// typeNeedsMatchDup returns true if a resolved type needs duping when extracted
// from a droppable enum via match destructure. Safe to dup:
// - Strings (dupString creates independent copy)
// - Channels (dupChannel increments refcount)
// - Vectors (dupVector shallow-copies buffer — safe because vector drop only frees buffer)
// - Heap user types WITHOUT explicit drops that only have safely-duppable fields
// - Enum types with a clone() method (B0244: deep-clone via synthesized/explicit clone)
// NOT safe: types with explicit drops (Map, Set, custom drop) — their drop logic
// cannot be replicated by memcpy, unless they have a clone() method.
func (c *Compiler) typeNeedsMatchDup(resolved types.Type) bool {
	named := extractNamed(resolved)
	if named == nil {
		// B0244: Check for enum types — clone if clone method exists in c.funcs
		// or if the enum is marked `clone (function may not be declared yet due
		// to cross-module compilation order — forward-declared lazily in cloneEnumValue).
		if enum := extractEnum(resolved); enum != nil {
			if _, exists := c.funcs[c.enumCloneFuncName(enum, resolved)]; exists {
				return true
			}
			return enum.IsClone()
		}
		return false
	}
	if named == types.TypString {
		return true
	}
	if _, ok := types.AsVector(resolved); ok || named == types.TypVector {
		return true
	}
	if _, ok := types.AsChannel(resolved); ok || named == types.TypChannel {
		return true
	}
	// Heap user types: only safe to shallow-dup (memcpy + field dup) if ALL droppable
	// fields can be independently dup'd. Specifically:
	// - String fields → dupString creates independent copy ✓
	// - Channel fields → dupChannel increments refcount ✓
	// - Vector fields → dupVector does SHALLOW element copy. Only safe if elements
	//   have no drops (otherwise element data is shared → double-free). ✗ for droppable.
	// - Other heap type fields → recursive check needed.
	// Types with explicit (non-synthesized) drops have custom cleanup logic that
	// memcpy cannot replicate — but CAN be deep-copied if they have a clone() method.
	if named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) || named.IsStructural() {
		return false
	}
	if c.heapTypeSafeToDup(named, resolved, nil) {
		return true
	}
	// B0244: Not safe to shallow-dup, but has clone() → can deep-copy via clone.
	// This handles types like Map, Set, and user types with complex drops.
	return c.namedHasCloneFunc(named, resolved)
}

// namedHasCloneFunc returns true if a named type has a clone() function available in c.funcs
// AND (for generic instances) all type arguments can be safely handled by the clone's
// internal match-dup. B0284: Without the type-arg check, clone-based dup for containers
// like Map[K, V] produces a shallow copy when V has drops but no clone — both the original
// and clone share heap pointers, causing double-free on drop.
func (c *Compiler) namedHasCloneFunc(named *types.Named, resolved types.Type) bool {
	ownerName := c.resolveMethodOwner(named, "clone")
	if inst, ok := resolved.(*types.Instance); ok {
		ownerName = monoName(inst)
	}
	_, exists := c.funcs[mangleMethodName(ownerName, "clone", false)]
	if !exists {
		return false
	}
	// B0284: For generic instances, verify all type arguments can be safely
	// handled by the clone's internal match-dup. Container clone methods (Map, Set)
	// iterate elements via match destructure — if any element type has drops but
	// can't be match-dup'd, the clone will be shallow for that type.
	if inst, ok := resolved.(*types.Instance); ok {
		for _, arg := range inst.TypeArgs() {
			if c.typeSubst != nil {
				arg = types.Substitute(arg, c.typeSubst)
			}
			if !c.typeArgSafeForCloneDup(arg) {
				return false
			}
		}
	}
	return true
}

// typeArgSafeForCloneDup returns true if a type argument is safe within a
// clone that uses match-dup to copy elements. Safe means either the type
// doesn't need dropping (bitwise copy is fine) or it can be independently
// dup'd by the match-dup mechanism. B0284.
func (c *Compiler) typeArgSafeForCloneDup(t types.Type) bool {
	if named := extractNamed(t); named != nil {
		// Copy, value, primitive, structural — no drops, bitwise copy is fine
		if named.IsCopy() || named.IsValueType() || isPrimitiveScalar(named) || named.IsStructural() {
			return true
		}
		// Heap type with drops — safe only if match-dup can handle it
		return c.typeNeedsMatchDup(t)
	}
	if enum := extractEnum(t); enum != nil {
		// Enum without drops — safe
		if !enum.HasDrop() && !enum.NeedsSynthDrop() {
			// Also check mono synth drops for generic enum instances
			if inst, ok := t.(*types.Instance); ok {
				mangledName := mangleMethodName(monoName(inst), "drop", false)
				if _, ok := c.funcs[mangledName]; ok {
					return c.typeNeedsMatchDup(t)
				}
			}
			return true
		}
		// Enum with drops — safe only if match-dup can handle it
		return c.typeNeedsMatchDup(t)
	}
	// Raw types (int, bool, etc.) — safe
	return true
}

// enumCloneFuncName returns the mangled LLVM function name for an enum's clone method.
// B0244: Used to check if a clone function exists for enum match-dup and vector clone.
func (c *Compiler) enumCloneFuncName(enum *types.Enum, resolved types.Type) string {
	ownerName := enum.Obj().Name()
	if inst, ok := resolved.(*types.Instance); ok {
		ownerName = monoName(inst)
	}
	return mangleMethodName(ownerName, "clone", false)
}

// heapTypeSafeToDup returns true if a heap user type can be safely dup'd via
// memcpy + per-field dup. The `seen` map prevents infinite recursion on cyclic types.
// B0236: A type is safe to dup when all its droppable fields are independently
// duppable (strings, channels, or recursively safe heap types). Vector fields
// with droppable elements are NOT safe (dupVector does shallow element copy).
func (c *Compiler) heapTypeSafeToDup(named *types.Named, resolved types.Type, seen map[*types.Named]bool) bool {
	if seen == nil {
		seen = make(map[*types.Named]bool)
	}
	if seen[named] {
		return false // cyclic reference — not safe
	}
	seen[named] = true

	// Types with explicit (non-synthesized) drops → not safe.
	if named.HasDrop() && !named.NeedsSynthDrop() {
		return false
	}
	if named.LookupMethod("drop") != nil && !named.NeedsSynthDrop() {
		return false
	}

	// Build substitution for generic instances
	var subst map[*types.TypeParam]types.Type
	if inst, ok := resolved.(*types.Instance); ok && len(named.TypeParams()) > 0 {
		subst = types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
	}

	for _, f := range named.AllFields() {
		fType := f.Type()
		if subst != nil {
			fType = types.Substitute(fType, subst)
		}
		fNamed := extractNamed(fType)
		if fNamed == nil {
			continue
		}

		// String, channel → safe to dup
		if fNamed == types.TypString {
			continue
		}
		if _, isChan := types.AsChannel(fType); isChan || fNamed == types.TypChannel {
			continue
		}

		// Vector → safe only if element type is non-droppable
		if elemType, isVec := types.AsVector(fType); isVec || fNamed == types.TypVector {
			if isVec && fieldTypeNeedsDrop(elemType) {
				return false // vector of droppable elements → shallow copy is unsafe
			}
			continue
		}

		// Primitive/value/copy types → safe (no pointer sharing)
		if fNamed.IsValueType() || fNamed.IsCopy() || isPrimitiveScalar(fNamed) || fNamed.IsStructural() {
			continue
		}

		// Nested heap user type → check recursively
		if !c.heapTypeSafeToDup(fNamed, fType, seen) {
			return false
		}
	}
	return true
}

// fieldTypeNeedsDrop returns true if a type needs drop cleanup (used for vector
// element safety check in heapTypeSafeToDup).
func fieldTypeNeedsDrop(typ types.Type) bool {
	named := extractNamed(typ)
	if named != nil {
		if named == types.TypString || named == types.TypVector || named == types.TypChannel {
			return true
		}
		if named.HasDrop() || named.NeedsSynthDrop() {
			return true
		}
		if !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural() {
			return true // heap user type
		}
	}
	// Check for enum types (extractNamed only handles *types.Named, not *types.Enum)
	if enum := extractEnum(typ); enum != nil {
		if enum.HasDrop() || enum.NeedsSynthDrop() {
			return true
		}
	}
	// Check Instance with Enum origin
	if inst, ok := typ.(*types.Instance); ok {
		if enum, ok := inst.Origin().(*types.Enum); ok {
			if enum.HasDrop() || enum.NeedsSynthDrop() {
				return true
			}
			// Generic enums with TypeParam fields may need drop at mono time
			for _, v := range enum.Variants() {
				for _, f := range v.Fields() {
					if _, isTP := f.Type().(*types.TypeParam); isTP {
						return true // conservatively assume TypeParam may resolve to droppable
					}
				}
			}
		}
	}
	return false
}

// dupMatchBinding dups a value from a match destructure to create an
// independent copy that won't be invalidated when the enum is dropped.
// B0232: Prevents double-frees when match-extracted values share instance
// pointers with enum data (e.g., Slot elements in Map._buckets).
// B0236: Extended to handle all droppable types: strings, vectors, channels,
// and heap user types (not just strings).
// B0237: The dup'd copy is owned by whoever consumes it (push, return via PHI, etc.).
// No post-match cleanup — consumers manage the value's lifetime.
// cloneResolvedValue produces a deep, owned copy of val given its fully
// resolved (already type-substituted) type. It is the dispatch core shared by
// dupMatchBinding (match destructure dup) and genAutoCloneExpr (T0605
// synth-clone of TypeParam fields): string→dupString, vector→dupVector +
// element clone loop, channel→dupChannel, enum-with-clone→cloneEnumValue,
// else heap user type→cloneHeapElement (clone() then shallow dup fallback).
// Callers are responsible for any alloca/drop-registration tail.
func (c *Compiler) cloneResolvedValue(val value.Value, resolvedType types.Type) value.Value {
	named := extractNamed(resolvedType)
	var dupVal value.Value

	_, isVec := types.AsVector(resolvedType)
	_, isChan := types.AsChannel(resolvedType)

	if named == types.TypString {
		dupVal = c.dupString(val)
	} else if isVec || named == types.TypVector {
		elemType, ok := types.AsVector(resolvedType)
		if !ok {
			dupVal = c.dupVector(val, 0)
		} else {
			elemLLVM := c.resolveType(elemType)
			elemSize := int64(c.typeSize(elemLLVM))
			dupVal = c.dupVector(val, elemSize)
			// B0244: Deep-clone vector elements when they're droppable (enum, heap types).
			// Without this, the dup'd vector shares element heap pointers with the original.
			resolvedElem := elemType
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
			}
			c.emitVectorElementCloneLoop(dupVal, resolvedElem)
		}
	} else if isChan || named == types.TypChannel {
		dupVal = c.dupChannel(val)
	} else if cloned, ok := c.cloneEnumValue(val, resolvedType); ok {
		// B0244: Enum with clone — deep-copy via clone method.
		dupVal = cloned
	} else {
		// B0236/B0244: Heap user type — try clone() first (handles types with complex drops
		// like Map, Set), fall back to shallow dup (alloc + memcpy + field dup).
		dupVal = c.cloneHeapElement(val, resolvedType, named)
	}
	return dupVal
}

func (c *Compiler) dupMatchBinding(name string, val value.Value, llvmType irtypes.Type, resolvedType types.Type) {
	dupVal := c.cloneResolvedValue(val, resolvedType)

	bindAlloca := c.createEntryAlloca(llvmType)
	c.locals[name] = bindAlloca
	c.block.NewStore(dupVal, bindAlloca)

	// B0242: Register dup'd bindings for scope cleanup with a drop flag.
	// The drop flag starts true; clearDropFlag sets it to false at move sites
	// (PHI return, push, consuming function call). At arm-scope cleanup
	// (genEnumMatch lines ~4011-4015), unconsumed bindings (flag still true)
	// are dropped, while consumed bindings (flag cleared) are skipped.
	// This fixes the B0237 regression where unconsumed dup'd bindings leaked.
	c.maybeRegisterDrop(name, bindAlloca, resolvedType)
}

// cloneEnumValue calls an enum's clone() method to deep-copy a value.
// B0244: Used in match destructure dup and vector element clone to create
// independent copies of enum values that would otherwise share heap pointers.
// Returns (cloned value, true) if the enum has a clone function, (nil, false) otherwise.
func (c *Compiler) cloneEnumValue(val value.Value, resolvedType types.Type) (value.Value, bool) {
	enum := extractEnum(resolvedType)
	if enum == nil {
		return nil, false
	}
	cloneFnName := c.enumCloneFuncName(enum, resolvedType)
	cloneFn, ok := c.funcs[cloneFnName]
	if !ok {
		// B0244: Forward-declare clone from module that owns this enum.
		// Cross-module compilation order may cause the clone function to not be
		// declared yet (e.g., std compiles Map[string, JsonValue].clone before
		// the json module declares JsonValue.clone).
		cloneFn = c.forwardDeclareModuleEnumClone(enum, cloneFnName, resolvedType)
		if cloneFn == nil {
			return nil, false
		}
	}
	// Store the enum value to a temp alloca and pass pointer as i8*
	// (enum method receiver convention: this is i8* pointing to enum value struct).
	alloca := c.createEntryAlloca(val.Type())
	alloca.SetName(c.uniqueLocalName("enum.clone.tmp"))
	c.block.NewStore(val, alloca)
	ptr := c.block.NewBitCast(alloca, irtypes.I8Ptr)
	result := c.block.NewCall(cloneFn, ptr)
	return result, true
}

// isAutoCloneBitCopy reports whether a value of (already type-substituted)
// type t can be deep-copied by a plain bit copy in the AutoClone path — i.e.
// it owns no heap allocation that the copy would alias and double-free.
// Mirrors the copy/value/primitive/structural predicate used throughout
// stmt.go (e.g. trackHeapUserTypeResult): scalars/refs/fn-ptrs (non-named)
// and value/copy/primitive-scalar/structural named types are bit copies;
// string/vector/channel/enum/heap-user types are not (they fall through to
// cloneResolvedValue). (T0605)
func (c *Compiler) isAutoCloneBitCopy(t types.Type) bool {
	// Optional[E] is a bit copy iff its payload is — recurse so a nested
	// optional of a heap type (e.g. `T?? val` with T=Map) is NOT treated as a
	// bit copy by cloneByType's Optional short-circuit (that would re-introduce
	// the T0605 double-free one level deeper).
	if opt, isOpt := t.(*types.Optional); isOpt {
		return c.isAutoCloneBitCopy(opt.Elem())
	}
	named := extractNamed(t)
	if named == nil {
		// T0607: an enum may own heap data via droppable variant payloads and
		// expose a clone() — it is NOT a bit copy; cloneByType must route it to
		// cloneResolvedValue→cloneEnumValue for an independent deep copy (else
		// AutoClone shallow-aliases the source enum → double-free, e.g. an
		// `Inner[T] inner` variant field). extractNamed is nil for enums (their
		// origin is *types.Enum), so this must precede the non-named scalar
		// fallthrough. Mirrors typeNeedsMatchDup's enum branch (inverted):
		// bit-copy-safe iff no clone func and not `clone (a pure copy enum with
		// no heap).
		if enum := extractEnum(t); enum != nil {
			if _, exists := c.funcs[c.enumCloneFuncName(enum, t)]; exists {
				return false
			}
			return !enum.IsClone()
		}
		// Non-named: scalars (int/float/bool/char), refs, function pointers,
		// scalar tuples — bitwise copy is correct (no shared heap).
		return true
	}
	return named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) || named.IsStructural()
}

// cloneByType produces a deep, owned copy of val given its fully resolved
// (post-mono-substitution) type. Used by genAutoCloneExpr (T0605):
//   - Optional[E] → none-check; on some, deep-clone the unwrapped concrete
//     payload and rewrap; on none, pass the {i1,payload} struct through.
//   - bit-copy types (copy/value/scalar/structural) → return val unchanged.
//   - else (string/vector/channel/enum/heap-user) → cloneResolvedValue.
func (c *Compiler) cloneByType(val value.Value, t types.Type) value.Value {
	if val == nil || t == nil {
		return val
	}

	if opt, isOpt := t.(*types.Optional); isOpt {
		elem := opt.Elem()
		// A bit-copy payload makes the whole {i1, payload} struct a bit copy.
		if c.isAutoCloneBitCopy(elem) {
			return val
		}
		optStruct, ok := val.Type().(*irtypes.StructType)
		if !ok {
			return val
		}
		present := c.block.NewExtractValue(val, 0)
		entryBlock := c.block
		someBlock := c.newBlock("autoclone.some")
		mergeBlock := c.newBlock("autoclone.merge")
		entryBlock.NewCondBr(present, someBlock, mergeBlock)

		c.block = someBlock
		payload := c.block.NewExtractValue(val, 1)
		clonedPayload := c.cloneByType(payload, elem)
		rewrapped := c.wrapOptional(clonedPayload, optStruct)
		someEnd := c.block
		someEnd.NewBr(mergeBlock)

		c.block = mergeBlock
		return c.block.NewPhi(
			ir.NewIncoming(val, entryBlock),
			ir.NewIncoming(rewrapped, someEnd),
		)
	}

	// Bit copy is correct for copy/value/scalar/structural — no shared heap
	// allocation, the field's drop (if any) is a no-op for these. This guard
	// also keeps value/copy types away from cloneResolvedValue, whose
	// dupHeapValue tail assumes a heap instance layout.
	if c.isAutoCloneBitCopy(t) {
		return val
	}

	// string / vector / channel / enum / heap user type — full deep clone.
	return c.cloneResolvedValue(val, t)
}

// genAutoCloneExpr lowers the synth-only AutoCloneExpr intrinsic (T0605). The
// inner is always `this.<field>` for a `clone-type field whose declared type
// contains a TypeParam; the concrete type is known only here, after mono
// substitution. The result is consumed by the enclosing Self(...) constructor
// (the genExpr case applies the same result temp-tracking as the synth
// clone()-CallExpr path so ownership transfers cleanly to the new field).
func (c *Compiler) genAutoCloneExpr(e *ast.AutoCloneExpr) value.Value {
	val := c.genExpr(e.Expr)
	if val == nil {
		return nil
	}
	t := c.info.Types[e.Expr]
	if c.typeSubst != nil && t != nil {
		t = types.Substitute(t, c.typeSubst)
	}
	if c.selfSubst != nil && t != nil {
		t = types.SubstituteSelf(t, c.selfSubst.iface, c.selfSubst.concrete)
	}
	return c.cloneByType(val, t)
}

// --- If expressions ---

func (c *Compiler) genIfExpr(e *ast.IfExpr) value.Value {
	cond := c.genExpr(e.Cond)

	thenBlock := c.newBlock("if.then")
	elseBlock := c.newBlock("if.else")
	mergeBlock := c.newBlock("if.merge")

	c.block.NewCondBr(cond, thenBlock, elseBlock)

	// Then branch
	c.block = thenBlock
	thenVal := c.genBlockValue(e.Then)
	c.claimStringTemp(thenVal) // T0073
	thenEnd := c.block
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Else branch
	c.block = elseBlock
	elseVal := c.genBlockValue(e.Else)
	c.claimStringTemp(elseVal) // T0073
	elseEnd := c.block
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	c.block = mergeBlock

	// Filter void-typed values — they cannot participate in phi nodes.
	if thenVal != nil {
		if _, isVoid := thenVal.Type().(*irtypes.VoidType); isVoid {
			thenVal = nil
		}
	}
	if elseVal != nil {
		if _, isVoid := elseVal.Type().(*irtypes.VoidType); isVoid {
			elseVal = nil
		}
	}

	// If both branches produce values, create a phi node
	if thenVal != nil && elseVal != nil {
		phi := mergeBlock.NewPhi(
			&ir.Incoming{X: thenVal, Pred: thenEnd},
			&ir.Incoming{X: elseVal, Pred: elseEnd},
		)
		return phi
	}

	return nil
}

// --- Error handling expressions ---

// genErrorPropagateExpr generates the `expr^` operator.
// Evaluates the inner failable call, checks the tag, propagates the error
// to the caller on error, or extracts the Ok value on success.
func (c *Compiler) genErrorPropagateExpr(e *ast.ErrorPropagateExpr) value.Value {
	result := c.genExpr(e.Expr)
	calleeResultType := result.Type().(*irtypes.StructType)

	tag := c.block.NewExtractValue(result, 0)

	propagateBlock := c.newBlock("error.propagate")
	okBlock := c.newBlock("error.ok")
	c.block.NewCondBr(tag, propagateBlock, okBlock)

	// Error path: cleanup stmt temps + scope bindings, extract error, propagate
	c.block = propagateBlock
	c.emitStmtTempCleanupForErrorPath() // T0103: free string temps before returning
	c.emitHeapTempCleanupForErrorPath() // T0103: free heap temps before returning
	if len(c.scopeBindings) > 0 {
		c.emitScopeCleanup(0, true) // error in flight — suppress close errors
	}
	errVal := c.block.NewExtractValue(result, resultErrIdx(calleeResultType))
	if c.inGenerator && c.generatorCanError {
		// B0023: store error to generator error_slot and branch to final suspend
		c.emitGeneratorError(errVal)
	} else {
		callerResultType := c.currentResultType()
		c.block.NewRet(c.wrapError(errVal, callerResultType))
	}

	// Ok path: extract value
	c.block = okBlock
	if !isVoidResult(calleeResultType) {
		return c.block.NewExtractValue(result, 1)
	}
	return nil
}

// genErrorPanicExpr generates the `expr?!` operator.
// Evaluates the inner failable call, panics on error, or extracts the Ok value.
func (c *Compiler) genErrorPanicExpr(e *ast.ErrorPanicExpr) value.Value {
	result := c.genExpr(e.Expr)
	resultType := result.Type().(*irtypes.StructType)

	tag := c.block.NewExtractValue(result, 0)

	panicBlock := c.newBlock("error.panic")
	okBlock := c.newBlock("error.ok")
	c.block.NewCondBr(tag, panicBlock, okBlock)

	// Error: extract message from error instance, panic with it (T0142: include source location)
	c.block = panicBlock
	errMsg := c.block.NewExtractValue(result, resultErrIdx(resultType))
	c.emitErrorPanic(errMsg, e.Pos().File, e.Pos().Line)

	// Ok: extract value
	c.block = okBlock
	if !isVoidResult(resultType) {
		return c.block.NewExtractValue(result, 1)
	}
	return nil
}

// genErrorHandlerExpr generates the `expr ? binding { body }` operator.
// Evaluates the inner failable call, runs the handler on error (with optional
// error binding), or extracts the Ok value on success. Merges with phi if
// both branches produce values.
//
// For typed handlers (`? e is IoError { ... }`), an RTTI check is performed on
// the error instance. If the check fails, the error is propagated (in failable
// functions) or causes a panic (in non-failable functions).
func (c *Compiler) genErrorHandlerExpr(e *ast.ErrorHandlerExpr) value.Value {
	// Optional handler: T? ? { recovery } → T
	if c.info.OptionalHandlers[e] {
		return c.genOptionalHandlerExpr(e)
	}

	result := c.genExpr(e.Expr)
	resultType := result.Type().(*irtypes.StructType)

	tag := c.block.NewExtractValue(result, 0)

	handlerBlock := c.newBlock("error.handler")
	okBlock := c.newBlock("error.ok")
	mergeBlock := c.newBlock("error.merge")
	c.block.NewCondBr(tag, handlerBlock, okBlock)

	// Handler block: clean up stmt temps before running handler body (T0103)
	c.block = handlerBlock
	c.emitStmtTempCleanupForErrorPath()
	c.emitHeapTempCleanupForErrorPath()
	errVal := c.block.NewExtractValue(result, resultErrIdx(resultType))

	var noMatchVal value.Value
	var noMatchEnd *ir.Block

	// For typed handlers, perform RTTI check before entering the handler body
	if e.TypeName != "" {
		var targetID int32
		if resolved := c.info.ErrorHandlerTypes[e]; resolved != nil {
			// Generic typed handler (e.g., DataError[string])
			var ok bool
			targetID, ok = c.resolveTypeID(resolved)
			if !ok {
				panic(fmt.Sprintf("codegen: cannot resolve type ID for %s in error handler", e.TypeName))
			}
		} else {
			// Non-generic typed handler
			targetNamed := c.lookupNamedType(e.TypeName)
			if targetNamed == nil {
				panic(fmt.Sprintf("codegen: undefined type %s in error handler", e.TypeName))
			}
			targetID = c.assignTypeID(targetNamed)
		}

		variantPtr := c.loadVariantPtr(errVal)
		rttiResult := c.block.NewCall(c.funcs["promise_type_is"],
			variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
		typeMatch := c.block.NewICmp(enum.IPredNE, rttiResult, constant.NewInt(irtypes.I32, 0))

		matchBlock := c.newBlock("error.typed.match")
		noMatchBlock := c.newBlock("error.typed.nomatch")
		c.block.NewCondBr(typeMatch, matchBlock, noMatchBlock)

		// No-match path: else body, panic (!), or propagate
		c.block = noMatchBlock
		if e.ElseBody != nil {
			// else clause: bind error and run else body (T0091: register for drop)
			savedElseScope := len(c.scopeBindings)
			if e.ElseBinding != "" && e.ElseBinding != "_" {
				elseValStruct := c.reconstructErrorValue(errVal)
				alloca := c.createEntryAlloca(userValueType())
				alloca.SetName(c.uniqueLocalName(e.ElseBinding))
				c.block.NewStore(elseValStruct, alloca)
				c.locals[e.ElseBinding] = alloca
				c.registerErrorDrop(e.ElseBinding, alloca, types.TypError)
			} else {
				// No else binding — temporary for drop
				alloca := c.createEntryAlloca(userValueType())
				alloca.SetName(c.uniqueLocalName("_else_err_tmp"))
				elseValStruct := c.reconstructErrorValue(errVal)
				c.block.NewStore(elseValStruct, alloca)
				c.registerErrorDrop("_else_err_tmp", alloca, types.TypError)
			}
			noMatchVal = c.genBlockValue(e.ElseBody)
			elseDiverged := c.block.Term != nil
			if !elseDiverged {
				if len(c.scopeBindings) > savedElseScope {
					c.emitScopeCleanup(savedElseScope, false)
				}
				noMatchEnd = c.block
				c.block.NewBr(mergeBlock)
			}
			c.scopeBindings = c.scopeBindings[:savedElseScope]
		} else if e.PanicOnNomatch {
			// Explicit ! suffix: panic on non-matching error (T0142: include source location)
			c.emitErrorPanic(errVal, e.Pos().File, e.Pos().Line)
		} else if c.canError || (c.inGenerator && c.generatorCanError) {
			if len(c.scopeBindings) > 0 {
				c.emitScopeCleanup(0, true) // error in flight — suppress close errors
			}
			if c.inGenerator && c.generatorCanError {
				// B0023: store error to generator error_slot and branch to final suspend
				c.emitGeneratorError(errVal)
			} else {
				callerResultType := c.currentResultType()
				c.block.NewRet(c.wrapError(errVal, callerResultType))
			}
		} else {
			// Should not be reached — sema rejects typed handlers in
			// non-failable functions without else or !
			panicMsg := c.makeGlobalString("unhandled error type")
			c.block.NewCall(c.funcs["promise_panic"], panicMsg)
			c.emitPanicReturn()
		}

		// Match path: continue to bind and run handler body
		c.block = matchBlock
	}

	// T0091/T0110: Register error binding for drop so the error instance (and its
	// string fields) are freed at handler scope exit. For typed catches, resolve
	// the concrete type's drop to free child-specific string fields. For re-raise
	// paths, genRaiseStmt clears the drop flag (T0086) before scope cleanup.
	savedHandlerScope := len(c.scopeBindings)

	// T0110: Resolve concrete error type for drop dispatch.
	// Typed catches use the child type's drop; untyped catches use base error.drop.
	// For generic error types (e.g., AppError[int]), pass the Instance type so
	// registerErrorDrop can use the monomorphized drop name.
	var errorDropType types.Type = types.TypError
	if e.TypeName != "" {
		if resolved := c.info.ErrorHandlerTypes[e]; resolved != nil {
			errorDropType = resolved
		} else if n := c.lookupNamedType(e.TypeName); n != nil {
			errorDropType = n
		}
	}

	if e.Binding != "" && e.Binding != "_" {
		valStruct := c.reconstructErrorValue(errVal)
		alloca := c.createEntryAlloca(userValueType())
		alloca.SetName(c.uniqueLocalName(e.Binding))
		c.block.NewStore(valStruct, alloca)
		c.locals[e.Binding] = alloca
		c.registerErrorDrop(e.Binding, alloca, errorDropType)
	} else {
		// No binding — create a temporary alloca so drop machinery can free it.
		alloca := c.createEntryAlloca(userValueType())
		alloca.SetName(c.uniqueLocalName("_err_tmp"))
		valStruct := c.reconstructErrorValue(errVal)
		c.block.NewStore(valStruct, alloca)
		c.registerErrorDrop("_err_tmp", alloca, errorDropType)
	}
	handlerVal := c.genBlockValue(e.Body)
	// Emit drop for the error binding after handler body (scope cleanup).
	if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > savedHandlerScope {
		c.emitScopeCleanup(savedHandlerScope, false)
	}
	c.scopeBindings = c.scopeBindings[:savedHandlerScope]
	handlerEnd := c.block
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Ok path: extract value
	c.block = okBlock
	var okVal value.Value
	if !isVoidResult(resultType) {
		okVal = c.block.NewExtractValue(result, 1)
	}

	// Optional recovery: wrap ok value as some(T), non-recovering paths produce none.
	if c.info.OptionalRecoveryHandlers[e] {
		semaType := c.info.Types[e]
		if c.typeSubst != nil {
			semaType = types.Substitute(semaType, c.typeSubst)
		}
		optLLVM := c.resolveType(semaType)
		optStructType, _ := optLLVM.(*irtypes.StructType)

		// Wrap ok value as some(T) in the ok block.
		if optStructType != nil && okVal != nil {
			okVal = c.wrapOptional(okVal, optStructType)
		}
		c.block.NewBr(mergeBlock)
		okEnd := c.block

		noneVal := c.zeroValue(optLLVM)

		// Wrap handler value in its block (before its br to merge).
		var handlerOptVal value.Value = noneVal
		handlerReachesMerge := false
		// B0353: Only consider handler as reaching merge if its br targets mergeBlock.
		if handlerEnd.Term != nil {
			if br, isBr := handlerEnd.Term.(*ir.TermBr); isBr && br.Target == mergeBlock {
				handlerReachesMerge = true
				if handlerVal != nil {
					if _, isVoid := handlerVal.Type().(*irtypes.VoidType); !isVoid {
						// Insert wrapOptional before the existing br terminator.
						savedBlock := c.block
						c.block = handlerEnd
						handlerEnd.Term = nil // remove br temporarily
						handlerOptVal = c.wrapOptional(handlerVal, optStructType)
						c.block.NewBr(mergeBlock) // re-add br
						c.block = savedBlock
					}
				}
			}
		}

		// Wrap noMatch value in its block.
		var noMatchOptVal value.Value = noneVal
		noMatchReachesMerge := false
		if noMatchEnd != nil {
			noMatchReachesMerge = true
			if noMatchVal != nil {
				if _, isVoid := noMatchVal.Type().(*irtypes.VoidType); !isVoid {
					savedBlock := c.block
					c.block = noMatchEnd
					noMatchEnd.Term = nil
					noMatchOptVal = c.wrapOptional(noMatchVal, optStructType)
					c.block.NewBr(mergeBlock)
					c.block = savedBlock
				}
			}
		}

		c.block = mergeBlock
		var incomings []*ir.Incoming
		incomings = append(incomings, &ir.Incoming{X: okVal, Pred: okEnd})
		if handlerReachesMerge {
			incomings = append(incomings, &ir.Incoming{X: handlerOptVal, Pred: handlerEnd})
		}
		if noMatchReachesMerge {
			incomings = append(incomings, &ir.Incoming{X: noMatchOptVal, Pred: noMatchEnd})
		}

		if len(incomings) > 1 {
			return mergeBlock.NewPhi(incomings...)
		}
		return okVal
	}

	c.block.NewBr(mergeBlock)
	okEnd := c.block

	// Merge with phi if both paths produce compatible values.
	// Treat void-typed values as nil (void call results cannot participate in phi).
	c.block = mergeBlock
	if handlerVal != nil {
		if _, isVoid := handlerVal.Type().(*irtypes.VoidType); isVoid {
			handlerVal = nil
		}
	}
	if noMatchVal != nil {
		if _, isVoid := noMatchVal.Type().(*irtypes.VoidType); isVoid {
			noMatchVal = nil
		}
	}
	if okVal != nil && handlerVal != nil {
		incomings := []*ir.Incoming{
			{X: okVal, Pred: okEnd},
			{X: handlerVal, Pred: handlerEnd},
		}
		if noMatchEnd != nil && noMatchVal != nil {
			incomings = append(incomings, &ir.Incoming{X: noMatchVal, Pred: noMatchEnd})
		}
		return mergeBlock.NewPhi(incomings...)
	}
	// okVal defined in okBlock doesn't dominate mergeBlock when handler also
	// reaches mergeBlock. Use a phi with a zero default from the handler path.
	if okVal != nil && handlerEnd.Term != nil {
		// B0353: Only add handler PHI entry if it actually branches to mergeBlock.
		// A return inside the handler (e.g., goroutine return) may branch elsewhere.
		if br, isBr := handlerEnd.Term.(*ir.TermBr); isBr && br.Target == mergeBlock {
			zeroVal := c.zeroValue(okVal.Type())
			incomings := []*ir.Incoming{
				{X: okVal, Pred: okEnd},
				{X: zeroVal, Pred: handlerEnd},
			}
			if noMatchEnd != nil {
				noMatchZero := c.zeroValue(okVal.Type())
				incomings = append(incomings, &ir.Incoming{X: noMatchZero, Pred: noMatchEnd})
			}
			return mergeBlock.NewPhi(incomings...)
		}
	}
	return okVal
}

// reconstructErrorValue builds a value struct {vtable_ptr, instance_ptr} from a raw i8* error pointer.
func (c *Compiler) reconstructErrorValue(errPtr value.Value) value.Value {
	variantPtr := c.loadVariantPtr(errPtr)
	typeinfoStruct := irtypes.NewStruct(irtypes.I8Ptr)
	typeinfoPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoStruct))
	vtableFieldPtr := c.block.NewGetElementPtr(typeinfoStruct, typeinfoPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	vtablePtr := c.block.NewLoad(irtypes.I8Ptr, vtableFieldPtr)
	var valStruct value.Value = constant.NewZeroInitializer(userValueType())
	valStruct = c.block.NewInsertValue(valStruct, vtablePtr, 0)
	valStruct = c.block.NewInsertValue(valStruct, errPtr, 1)
	return valStruct
}

// --- Tuple ---

func (c *Compiler) genTupleLit(e *ast.TupleLit) value.Value {
	lt := c.resolveType(c.info.Types[e])
	structType, ok := lt.(*irtypes.StructType)
	if !ok {
		panic(fmt.Sprintf("codegen: tuple type resolved to %T, want StructType", lt))
	}
	var agg value.Value = constant.NewZeroInitializer(structType)
	for i, elem := range e.Elements {
		savedEnumTemps := len(c.enumCtorTemps)
		elemVal := c.genExpr(elem)
		agg = c.block.NewInsertValue(agg, elemVal, uint64(i))
		// B0242: Clear drop flags for ident elements consumed by the tuple.
		// When a dup'd match binding is embedded in a tuple (e.g., (k, v)),
		// ownership transfers to the tuple — the binding must not be dropped
		// at arm-scope cleanup. No-op if the ident has no drop flag.
		if ident, ok := elem.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
		// T0371: Claim heap-tracked field temps so they are not double-freed at
		// stmt end (their ownership is now in the tuple slot). Mirrors the
		// pattern used in genArrayLit / genMapLit. Without these claims:
		//   - string/vector/channel stmt-temps would self-clean at stmt end,
		//     leaving a dangling pointer in the tuple slot (case D/E garbage).
		//   - heap user-type temps would be freed at stmt end, then dropped
		//     again when the tuple is consumed (case A double-free).
		c.claimStringTemp(elemVal) // strings, vectors, channels, arcs, mutexes
		c.claimHeapTemp(elemVal)   // heap user-type instances
		// Clear enum ctor temps created during this element's evaluation so
		// the tuple is the unique owner of the enum's variant data.
		for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
		}
		c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
	}
	return agg
}

// --- Optional ---

func (c *Compiler) genNoneLit(e *ast.NoneLit) value.Value {
	if c.targetType != nil {
		lt := c.resolveType(c.targetType)
		return c.zeroValue(lt)
	}
	return constant.NewInt(irtypes.I1, 0) // void optional fallback
}

// wrapOptional wraps a value into an optional struct: { true, val }.
func (c *Compiler) wrapOptional(val value.Value, optType *irtypes.StructType) value.Value {
	var agg value.Value = constant.NewUndef(optType)
	agg = c.block.NewInsertValue(agg, constant.NewInt(irtypes.I1, 1), 0)
	agg = c.block.NewInsertValue(agg, val, 1)
	return agg
}

// wrapReturnOptional wraps val in an Optional struct if retType is Optional
// but the expression type is a non-optional, non-none value.
func (c *Compiler) wrapReturnOptional(val value.Value, expr ast.Expr, retType types.Type) value.Value {
	if retType == nil {
		return val
	}
	if _, isOpt := retType.(*types.Optional); !isOpt {
		return val
	}
	exprType := c.info.Types[expr]
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	// NoneLit already produces the correct zero value via targetType
	if exprType == types.TypNone {
		return val
	}
	// Same shape — no wrapping needed. Use Identical (not "is exprOpt?") so
	// returning T? from a T??-returning function still wraps.
	if types.Identical(exprType, retType) {
		return val
	}
	lt := c.resolveType(retType)
	if st, ok := lt.(*irtypes.StructType); ok {
		return c.wrapOptional(val, st)
	}
	return val
}

func (c *Compiler) genElvis(e *ast.BinaryExpr) value.Value {
	optVal := c.genExprAutoPropagate(e.Left)

	// Extract the present flag (field 0)
	flag := c.block.NewExtractValue(optVal, 0)

	someBlock := c.newBlock("elvis.some")
	noneBlock := c.newBlock("elvis.none")
	mergeBlock := c.newBlock("elvis.merge")

	c.block.NewCondBr(flag, someBlock, noneBlock)

	// Some path: extract inner value
	c.block = someBlock
	someVal := c.block.NewExtractValue(optVal, 1)
	// B0194/T0111: Clear drop flag on elvis of optional identifier.
	// The inner value is extracted and transferred to the result — the optional's
	// scope-exit drop should NOT also free it (double-free).
	if ident, ok := e.Left.(*ast.IdentExpr); ok {
		c.clearDropFlag(ident.Name)
	}
	c.block.NewBr(mergeBlock)
	someEnd := c.block

	// None path: evaluate default
	c.block = noneBlock
	defaultVal := c.genExprAutoPropagate(e.Right)
	noneEnd := c.block
	c.block.NewBr(mergeBlock)

	// Merge
	c.block = mergeBlock
	return mergeBlock.NewPhi(
		&ir.Incoming{X: someVal, Pred: someEnd},
		&ir.Incoming{X: defaultVal, Pred: noneEnd},
	)
}

// --- Vector / Array Literal ---

const vectorHeaderSize = 16

func vectorHeaderType() *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I64, irtypes.I64)
}

// vectorLenMask is 0x7FFFFFFFFFFFFFFF — masks off the static flag (bit 63).
var vectorLenMask = constant.NewInt(irtypes.I64, 0x7FFFFFFFFFFFFFFF)

// loadVectorLen loads the vector length from the header with bit 63 masked off.
// Bit 63 is the static flag (set for .rodata vectors, clear for heap vectors).
func loadVectorLen(b *ir.Block, headerPtr value.Value) value.Value {
	headerType := vectorHeaderType()
	lenPtr := b.NewGetElementPtr(headerType, headerPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	raw := b.NewLoad(irtypes.I64, lenPtr)
	return b.NewAnd(raw, vectorLenMask)
}

// loadVectorLenRaw loads the raw vector length from the header with bit 63 intact.
func loadVectorLenRaw(b *ir.Block, headerPtr value.Value) value.Value {
	headerType := vectorHeaderType()
	lenPtr := b.NewGetElementPtr(headerType, headerPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	return b.NewLoad(irtypes.I64, lenPtr)
}

func (c *Compiler) genArrayLit(e *ast.ArrayLit) value.Value {
	typ := c.info.Types[e]
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}

	// Fixed-size array: stack-allocated [N x T]
	if arr, ok := typ.(*types.Array); ok {
		return c.genFixedArrayLit(e, arr)
	}

	elem, ok := types.AsVector(typ)
	if !ok {
		panic(fmt.Sprintf("codegen: array literal type is %T, want Vector instance or Array", typ))
	}
	elemLLVM := c.resolveType(elem)

	// Try static .rodata path: all elements must be compile-time constants
	if consts := c.tryConstantElements(e.Elements, elem, elemLLVM); consts != nil {
		return c.genStaticVectorLit(int64(len(e.Elements)), elemLLVM, consts)
	}

	elemSize := int64(c.typeSize(elemLLVM))
	n := int64(len(e.Elements))

	// Total allocation: header (16 bytes) + n * elemSize
	totalSize := int64(vectorHeaderSize) + n*elemSize

	// malloc
	rawPtr := c.block.NewCall(c.palAlloc,
		constant.NewInt(irtypes.I64, totalSize))

	// B0201/B0359: Track the vector allocation as a heap temp. This serves two
	// purposes: (1) if a failable element evaluation triggers error auto-propagation,
	// the vector is freed (B0201); (2) when a vector literal is passed directly as
	// a non-variadic function argument (e.g., foo(["hello"])), the caller frees the
	// buffer at statement end via cleanupHeapTemps (B0359). Variable assignments
	// claim the temp via claimHeapTemp, preventing double-free. This code only
	// runs on the heap path — static (.rodata) vectors return earlier via
	// genStaticVectorLit and are never tracked.
	// T0369: Use Vector.drop with element-type info so transient cleanup walks
	// droppable elements (string concats, nested vectors, channels, heap user
	// types, enum heap variant data) before freeing the buffer. The helper
	// returns false when the walk is suppressed (elemType transitively contains
	// a droppable tuple — see T0371): in that path the per-element claims below
	// are skipped so each tracked temp self-cleans up at stmt end instead of
	// being orphaned by a buffer-only Vector.drop.
	walkEnabled := c.trackVectorHeapTempWithElemType(rawPtr, elem)

	// Store len and cap via header GEP
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(rawPtr, irtypes.NewPointer(headerType))
	lenPtr := c.block.NewGetElementPtr(headerType, headerPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewStore(constant.NewInt(irtypes.I64, n), lenPtr)

	capPtr := c.block.NewGetElementPtr(headerType, headerPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	c.block.NewStore(constant.NewInt(irtypes.I64, n), capPtr)

	// Store elements: ptr + 16 bytes (header), then index by element type
	dataBase := c.block.NewGetElementPtr(irtypes.I8, rawPtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))

	for i, elemExpr := range e.Elements {
		savedEnumTemps := len(c.enumCtorTemps)
		val := c.genCallArgExpr(elemExpr)
		elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr,
			constant.NewInt(irtypes.I64, int64(i)))
		c.block.NewStore(val, elemPtr)
		if walkEnabled {
			// T0610: An ident element of a type that Vector.drop's element-walk
			// frees is *moved* into the vector (ownership marks it Moved). The
			// vector now owns it, so the source variable's scope-exit drop
			// binding must be suppressed — otherwise both free the same
			// allocation (double-free / SEGV for Mutex/Task/heap-user/string/
			// nested-vector). Mirrors genTupleLit (B0242) / genMapLit (B0280).
			// Type-gated to exactly the set emitVectorElementDropLoop walks
			// (stmt.go:3640-3644): clearing for element types it does NOT walk
			// (e.g. Optional[heap-user]) would orphan the source's allocation
			// → leak. No-op for Copy/borrow idents (no drop flag).
			if ident, ok := elemExpr.(*ast.IdentExpr); ok && !isRefType(elem) {
				if extractNamed(elem) == types.TypString ||
					c.vecElemNeedsEnumDrop(elem) ||
					c.vecElemNeedsUserTypeDrop(elem) ||
					c.tupleNeedsDrop(elem) ||
					c.vecElemNeedsOptionalDrop(elem) {
					c.clearDropFlag(ident.Name)
				}
			}
			// B0233: Claim heap temp — element ownership transferred to vector literal.
			c.claimHeapTemp(val)
			// T0366: Also claim string/vector/channel stmt-temps. trackVectorTempWithElemType
			// (called by CallExpr / ?! / ?^ / ! / ? e {} for Vector results) registers in
			// stmtTemps, not heapTemps — claimHeapTemp doesn't see them. Without claiming,
			// the caller's stmt-temp cleanup runs Vector.drop while the gather buffer (owned
			// by the variadic callee) also drops each element → double-free.
			c.claimStringTemp(val)
			// B0281: Clear enum ctor temps created during this element's evaluation.
			// Same issue as map literals: the enum value is stored by LLVM value,
			// so both the temp alloca and the vector slot share inner pointers.
			// Only clear temps added since savedEnumTemps to avoid clobbering
			// temps from outer expressions.
			for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
				c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
			}
			c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
		}
		// T0369: When walk is suppressed, leave heap temps / string stmt-temps /
		// enum ctor temps tracked. They self-clean at stmt end so nothing is
		// orphaned by the buffer-only Vector.drop. The vector slot retains the
		// pointer, but ownership has not been transferred — both the slot and
		// the original tracker reference the same heap value, and the buffer
		// free does NOT walk them, so each tracker drops its own value once.
	}

	return rawPtr // i8*
}

// tryConstantElements checks if all array literal elements are compile-time constants
// (int, float, bool, char literals). Returns a slice of LLVM constants or nil if any
// element is non-constant.
func (c *Compiler) tryConstantElements(elements []ast.Expr, elemType types.Type, elemLLVM irtypes.Type) []constant.Constant {
	if len(elements) == 0 {
		return []constant.Constant{} // empty static vector
	}
	consts := make([]constant.Constant, 0, len(elements))
	for _, expr := range elements {
		cv := c.tryConstantExpr(expr, elemType, elemLLVM)
		if cv == nil {
			return nil
		}
		consts = append(consts, cv)
	}
	return consts
}

// tryConstantExpr attempts to evaluate an expression as a compile-time constant.
// Returns nil if the expression is not a constant literal.
func (c *Compiler) tryConstantExpr(expr ast.Expr, elemType types.Type, elemLLVM irtypes.Type) constant.Constant {
	switch e := expr.(type) {
	case *ast.IntLit:
		intType, ok := elemLLVM.(*irtypes.IntType)
		if !ok {
			intType = irtypes.I64
		}
		raw := strings.ReplaceAll(e.Raw, "_", "")
		val, err := strconv.ParseInt(raw, 0, 64)
		if err != nil {
			uval, _ := strconv.ParseUint(raw, 0, 64)
			return constant.NewInt(intType, int64(uval))
		}
		return constant.NewInt(intType, val)
	case *ast.FloatLit:
		floatType, ok := elemLLVM.(*irtypes.FloatType)
		if !ok {
			floatType = irtypes.Double
		}
		raw := strings.ReplaceAll(e.Raw, "_", "")
		bitSize := 64
		if floatType == irtypes.Float {
			bitSize = 32
		}
		val, _ := strconv.ParseFloat(raw, bitSize)
		return constant.NewFloat(floatType, val)
	case *ast.BoolLit:
		if e.Value {
			return constant.NewInt(irtypes.I1, 1)
		}
		return constant.NewInt(irtypes.I1, 0)
	case *ast.CharLit:
		raw := e.Raw
		inner := raw[1 : len(raw)-1]
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
	case *ast.UnaryExpr:
		// Handle negative literals: -42, -3.14
		if e.Op == ast.UnaryNeg {
			inner := c.tryConstantExpr(e.Operand, elemType, elemLLVM)
			if inner == nil {
				return nil
			}
			switch v := inner.(type) {
			case *constant.Int:
				return constant.NewInt(v.Typ, -v.X.Int64())
			case *constant.Float:
				neg, _ := v.X.Float64()
				return constant.NewFloat(v.Typ, -neg)
			}
		}
		return nil
	}
	return nil
}

// genStaticVectorLit emits a static .rodata global for a vector literal with all-constant elements.
// Vector layout: {i64 len|bit63, i64 cap, [N x elemType] data}
// Returns i8* pointer to the global.
func (c *Compiler) genStaticVectorLit(n int64, elemLLVM irtypes.Type, consts []constant.Constant) value.Value {
	arrType := irtypes.NewArray(uint64(n), elemLLVM)

	// Build the global struct type: {i64, i64, [N x T]}
	globalType := irtypes.NewStruct(irtypes.I64, irtypes.I64, arrType)

	// Length with static flag (bit 63) set
	staticLen := n | math.MinInt64

	// Build array constant
	arrConst := constant.NewArray(arrType, consts...)

	init := constant.NewStruct(globalType,
		constant.NewInt(irtypes.I64, staticLen), // len | bit63
		constant.NewInt(irtypes.I64, n),         // cap
		arrConst,                                // data
	)

	var globalName string
	if c.compilingModule != "" {
		globalName = fmt.Sprintf(".arr.__mod_%s.%d", c.compilingModule, c.moduleArrCounter)
		c.moduleArrCounter++
	} else {
		globalName = fmt.Sprintf(".arr.%d", c.arrCounter)
		c.arrCounter++
	}
	global := c.module.NewGlobalDef(globalName, init)
	global.Immutable = true
	global.Linkage = enum.LinkagePrivate

	return c.block.NewBitCast(global, irtypes.I8Ptr)
}

// genFixedArrayLit generates a stack-allocated fixed-size array literal.
// Returns the full [N x T] value (not a pointer).
func (c *Compiler) genFixedArrayLit(e *ast.ArrayLit, arr *types.Array) value.Value {
	elemLLVM := c.resolveType(arr.Elem())
	arrType := irtypes.NewArray(uint64(arr.Size()), elemLLVM)

	// T0389: When the array's element type is droppable, the new bindingDropArray
	// (registered by maybeRegisterDrop) takes ownership of each slot at scope
	// exit. To avoid double-free, claim element temps so stmt-end cleanup
	// doesn't also free them. Gating on variantFieldNeedsDrop keeps non-droppable
	// element types (e.g. Optional[string]) on their pre-T0389 path where the
	// source variables drop normally — without this gate, clearing an ident's
	// drop flag would orphan its inner allocation since no array binding fires.
	resolvedElem := arr.Elem()
	if c.typeSubst != nil {
		resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
	}
	claim := c.variantFieldNeedsDrop(resolvedElem)

	tmp := c.createEntryAlloca(arrType)
	for i, elemExpr := range e.Elements {
		savedEnumTemps := len(c.enumCtorTemps)
		val := c.genCallArgExpr(elemExpr)
		ptr := c.block.NewGetElementPtr(arrType, tmp,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
		c.block.NewStore(val, ptr)
		if !claim {
			continue
		}
		// Element ownership transfers to the array binding — claim temps and
		// clear ident drop flags so they are not double-freed at stmt end or
		// at the source variable's scope exit. Mirrors genTupleLit.
		if ident, ok := elemExpr.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
		c.claimStringTemp(val) // strings, vectors, channels, arcs, mutexes
		c.claimHeapTemp(val)   // heap user-type instances
		// Clear enum ctor temps created during this element's evaluation so
		// the array is the unique owner of the enum's variant data.
		for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
		}
		c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
	}
	return c.block.NewLoad(arrType, tmp)
}

// --- Index Expression ---

func (c *Compiler) genSliceExpr(e *ast.SliceExpr) value.Value {
	targetType := c.info.Types[e.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	// Unwrap MutRef/SharedRef for slicing (auto-deref through borrows)
	if ref, ok := targetType.(*types.MutRef); ok {
		targetType = ref.Elem()
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		targetType = ref.Elem()
	}

	named := extractNamed(targetType)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot slice type %s", targetType))
	}
	m := named.LookupMethod("[:]")
	if m == nil {
		panic(fmt.Sprintf("codegen: no [:] method on type %s", named))
	}

	target := c.genExprAutoPropagate(e.Target) // B0323

	// Generate optional int arguments for low and high bounds
	optIntType := irtypes.NewStruct(irtypes.I1, irtypes.I64)
	low := c.genSliceBound(e.Low, optIntType)
	high := c.genSliceBound(e.High, optIntType)

	if m.IsNative() {
		return c.genNativeSlice(named, targetType, target, low, high)
	}

	// Non-native: call monomorphized [:] method
	mangledName := mangleMethodName(c.resolveTypeName(targetType), "[:]", false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared [:] method %s", mangledName))
	}

	var instancePtr value.Value
	if isContainerType(targetType) {
		instancePtr = target
	} else if named != nil && named.IsValueType() {
		instancePtr = c.valueTypeReceiverPtr(target, targetType)
	} else {
		instancePtr = c.extractInstancePtr(target)
	}

	return c.block.NewCall(fn, instancePtr, low, high)
}

// genSliceBound generates an optional int value for a slice bound expression.
// If expr is nil, returns none ({i1 false, i64 0}). Otherwise wraps the value.
// If the expression already produces an optional (int?), passes it through directly.
func (c *Compiler) genSliceBound(expr ast.Expr, optType *irtypes.StructType) value.Value {
	if expr == nil {
		return constant.NewZeroInitializer(optType)
	}
	val := c.genExpr(expr)
	// If the expression type is already optional, pass through directly.
	exprType := c.info.Types[expr]
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	if _, isOpt := exprType.(*types.Optional); isOpt {
		return val
	}
	return c.wrapOptional(val, optType)
}

func (c *Compiler) genIndexExpr(e *ast.IndexExpr) value.Value {
	targetType := c.info.Types[e.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	// Unwrap MutRef/SharedRef for indexing (auto-deref through borrows)
	if ref, ok := targetType.(*types.MutRef); ok {
		targetType = ref.Elem()
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		targetType = ref.Elem()
	}

	// Fixed-size array indexing
	if arr, ok := targetType.(*types.Array); ok {
		return c.genArrayIndex(e, arr)
	}

	named := extractNamed(targetType)
	if named != nil {
		if m := named.LookupMethod("[]"); m != nil {
			if m.IsNative() {
				return c.genNativeIndex(e, named, targetType)
			}
			return c.genMethodIndex(e, targetType)
		}
	}

	panic(fmt.Sprintf("codegen: cannot index type %s", targetType))
}

// genArrayBasePtr returns a pointer to the base of a fixed-size array.
// For identifier targets, returns the alloca directly (needed for index assignment).
// For struct field targets, returns a pointer to the field in the instance.
// For other expressions, allocas a temp and stores the value.
func (c *Compiler) genArrayBasePtr(target ast.Expr, arr *types.Array) value.Value {
	if ident, ok := target.(*ast.IdentExpr); ok {
		if alloca, ok := c.locals[ident.Name]; ok {
			return alloca
		}
	}
	// Struct field: return pointer to the field directly (not a copy)
	if memberExpr, ok := target.(*ast.MemberExpr); ok {
		return c.genFieldPtr(memberExpr)
	}
	arrVal := c.genExprAutoPropagate(target) // B0323
	elemLLVM := c.resolveType(arr.Elem())
	arrType := irtypes.NewArray(uint64(arr.Size()), elemLLVM)
	tmp := c.createEntryAlloca(arrType)
	c.block.NewStore(arrVal, tmp)
	return tmp
}

// genArrayIndex handles arr[i] for fixed-size arrays with bounds checking.
func (c *Compiler) genArrayIndex(e *ast.IndexExpr, arr *types.Array) value.Value {
	basePtr := c.genArrayBasePtr(e.Target, arr)
	idx := c.genExpr(e.Index)
	elemLLVM := c.resolveType(arr.Elem())
	arrType := irtypes.NewArray(uint64(arr.Size()), elemLLVM)

	// Bounds check: idx < N
	size := constant.NewInt(irtypes.I64, arr.Size())
	inBounds := c.block.NewICmp(enum.IPredULT, idx, size)
	okBlock := c.newBlock("arridx.ok")
	panicBlock := c.newBlock("arridx.oob")
	c.block.NewCondBr(inBounds, okBlock, panicBlock)

	c.block = panicBlock
	oobMsg := c.makeGlobalString("array index out of bounds")
	c.block.NewCall(c.funcs["promise_panic"], oobMsg)
	c.emitPanicReturn()

	c.block = okBlock
	elemPtr := c.block.NewGetElementPtr(arrType, basePtr,
		constant.NewInt(irtypes.I32, 0), idx)
	val := c.block.NewLoad(elemLLVM, elemPtr)

	// T0590: Dup-on-read for fixed-size arrays. Mirrors the Vector dup-on-read
	// branches in genVectorIndex (B0204/T0370/T0383/T0398/T0412) plus the Optional
	// extract+dup+insert branches from genMethodIndex (B0347/T0397/T0440/T0366).
	// Without these dups, slot reads alias the array's owned data — combined with
	// drop-on-overwrite (T0583), `arr[1] = arr[0]` and `let x = arr[0]; arr[0] = c;`
	// produce double-frees at scope exit. Bare types dup directly; Optional types
	// extract inner, dup, re-insert, and set the optional*Dup sentinel.
	elemType := arr.Elem()
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}

	// String element (B0204 analogue)
	if c.dupStringFieldAccess && c.tempTrackingEnabled && extractNamed(elemType) == types.TypString {
		c.dupStringFieldAccess = false // consume the flag
		dup := c.dupString(val)
		c.trackStringTemp(dup)
		return dup
	}

	// Optional[string] element (B0347 analogue)
	if c.dupStringFieldAccess && c.tempTrackingEnabled {
		if opt, ok := elemType.(*types.Optional); ok && extractNamed(opt.Elem()) == types.TypString {
			c.dupStringFieldAccess = false // consume the flag
			innerStr := c.block.NewExtractValue(val, 1)
			dup := c.dupString(innerStr)
			c.trackStringTemp(dup)
			c.optionalStringDup = dup
			return c.block.NewInsertValue(val, dup, 1)
		}
	}

	// Droppable tuple element (T0370 analogue)
	if c.dupTupleFieldAccess && c.tempTrackingEnabled {
		if tup, ok := elemType.(*types.Tuple); ok && c.tupleNeedsDrop(elemType) {
			c.dupTupleFieldAccess = false // consume the flag
			return c.dupTupleValue(val, tup)
		}
	}

	// Optional[Tuple<droppable>] element (T0397 analogue)
	if c.dupTupleFieldAccess && c.tempTrackingEnabled {
		if opt, ok := elemType.(*types.Optional); ok {
			inner := opt.Elem()
			if c.typeSubst != nil {
				inner = types.Substitute(inner, c.typeSubst)
			}
			if tup, isTup := inner.(*types.Tuple); isTup && c.tupleNeedsDrop(inner) {
				c.dupTupleFieldAccess = false // consume the flag
				innerTup := c.block.NewExtractValue(val, 1)
				dup := c.dupTupleValue(innerTup, tup)
				c.optionalTupleDup = dup
				return c.block.NewInsertValue(val, dup, 1)
			}
		}
	}

	// Droppable heap user element (T0398 analogue)
	if c.dupHeapUserFieldAccess && c.tempTrackingEnabled {
		if isDroppableHeapUserType(elemType) {
			if named := extractNamed(elemType); named != nil {
				c.dupHeapUserFieldAccess = false // consume the flag
				return c.cloneHeapElement(val, elemType, named)
			}
		}
	}

	// T0590: Heap user without explicit drop (pal_free-only path) — same dup-on-
	// read need as the drop branch above. `isDroppableHeapUserType` excludes types
	// with no drop/synth-drop because the Map clone path relies on that gate, but
	// arrays have no internal match-dup so we dup unconditionally for any heap
	// user type. `dupHeapValue` handles the no-droppable-field layout fine
	// (pal_alloc + memcpy + sub-field dup, with no sub-fields to dup for _Bare).
	if c.dupHeapUserFieldAccess && c.tempTrackingEnabled && isHeapUserNoDropPalFree(elemType) {
		c.dupHeapUserFieldAccess = false // consume the flag
		return c.dupHeapValue(val, elemType)
	}

	// Optional[heap-user-type] element (T0440 analogue, relaxed for arrays).
	// The genMethodIndex gate restricts to `drop && !clone` because Map.[]'s
	// body internally dups V via match-destructure for clone-bearing types —
	// duping again at the call site would double-allocate. Arrays have no
	// internal dup in `genArrayIndex`, so the gate is dropped: any droppable
	// heap user (with or without clone, with or without drop) needs dup here.
	// `dupHeapValue` is null-safe internally and dispatches to the type's
	// typeinfo clone fn for polymorphic types (T0387).
	if c.dupHeapUserFieldAccess && c.tempTrackingEnabled {
		if opt, ok := elemType.(*types.Optional); ok {
			inner := opt.Elem()
			if c.typeSubst != nil {
				inner = types.Substitute(inner, c.typeSubst)
			}
			if isDroppableHeapUserType(inner) || isHeapUserNoDropPalFree(inner) {
				c.dupHeapUserFieldAccess = false // consume the flag
				innerVal := c.block.NewExtractValue(val, 1)
				dup := c.dupHeapValue(innerVal, inner)
				c.optionalHeapDup = dup
				return c.block.NewInsertValue(val, dup, 1)
			}
		}
	}

	// Container element: Vector / Channel / Arc / Weak (T0383 analogue)
	if c.dupContainerFieldAccess && c.tempTrackingEnabled {
		if innerElem, isVec := types.AsVector(elemType); isVec {
			c.dupContainerFieldAccess = false // consume the flag
			innerLLVM := c.resolveType(innerElem)
			innerSize := int64(c.typeSize(innerLLVM))
			dup := c.dupVector(val, innerSize)
			c.emitVectorElementCloneLoop(dup, innerElem)
			c.trackVectorTempWithElemType(dup, innerElem)
			return dup
		}
		if extractNamed(elemType) == types.TypVector {
			c.dupContainerFieldAccess = false
			dup := c.dupVector(val, 0)
			c.trackVectorTemp(dup)
			return dup
		}
		if chElem, isCh := types.AsChannel(elemType); isCh {
			c.dupContainerFieldAccess = false
			dup := c.dupChannel(val)
			c.trackChannelTempWithElemType(dup, chElem) // T0663
			return dup
		}
		if arcElem, isArc := types.AsArc(elemType); isArc {
			c.dupContainerFieldAccess = false
			dup := c.dupArc(val)
			resolvedArcElem := arcElem
			if c.typeSubst != nil {
				resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
			}
			c.trackTempWithDrop(dup, c.getOrCreateArcDrop(resolvedArcElem))
			return dup
		}
		if weakElem, isWeak := types.AsWeak(elemType); isWeak {
			c.dupContainerFieldAccess = false
			resolvedWeakElem := weakElem
			if c.typeSubst != nil {
				resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
			}
			dup := c.dupWeak(val, resolvedWeakElem)
			c.trackTempWithDrop(dup, c.getOrCreateWeakDrop(resolvedWeakElem))
			return dup
		}
	}

	// Optional[Vector|Channel|Arc|Weak] element (T0366 analogue)
	if c.dupContainerFieldAccess && c.tempTrackingEnabled {
		if opt, ok := elemType.(*types.Optional); ok {
			inner := opt.Elem()
			if c.typeSubst != nil {
				inner = types.Substitute(inner, c.typeSubst)
			}
			if innerElem, isVec := types.AsVector(inner); isVec {
				c.dupContainerFieldAccess = false
				innerLLVM := c.resolveType(innerElem)
				innerSize := int64(c.typeSize(innerLLVM))
				innerVec := c.block.NewExtractValue(val, 1)
				dup := c.dupVector(innerVec, innerSize)
				c.emitVectorElementCloneLoop(dup, innerElem)
				c.trackVectorTempWithElemType(dup, innerElem)
				c.optionalContainerDup = dup
				return c.block.NewInsertValue(val, dup, 1)
			}
			if chElem, isCh := types.AsChannel(inner); isCh {
				c.dupContainerFieldAccess = false
				innerCh := c.block.NewExtractValue(val, 1)
				dup := c.dupChannel(innerCh)
				c.trackChannelTempWithElemType(dup, chElem) // T0663
				c.optionalContainerDup = dup
				return c.block.NewInsertValue(val, dup, 1)
			}
			if arcElem, isArc := types.AsArc(inner); isArc {
				c.dupContainerFieldAccess = false
				innerArc := c.block.NewExtractValue(val, 1)
				dup := c.dupArc(innerArc)
				resolvedArcElem := arcElem
				if c.typeSubst != nil {
					resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
				}
				c.trackTempWithDrop(dup, c.getOrCreateArcDrop(resolvedArcElem))
				c.optionalContainerDup = dup
				return c.block.NewInsertValue(val, dup, 1)
			}
			if weakElem, isWeak := types.AsWeak(inner); isWeak {
				c.dupContainerFieldAccess = false
				innerWeak := c.block.NewExtractValue(val, 1)
				resolvedWeakElem := weakElem
				if c.typeSubst != nil {
					resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
				}
				dup := c.dupWeak(innerWeak, resolvedWeakElem)
				c.trackTempWithDrop(dup, c.getOrCreateWeakDrop(resolvedWeakElem))
				c.optionalContainerDup = dup
				return c.block.NewInsertValue(val, dup, 1)
			}
		}
	}

	return val
}

// genReceiveTaskSlotPtr computes the element l-value slot pointer (i8**) for a
// `<-coll[i]` task-receive operand, so genReceiveTask can null the slot after
// it frees the G (T0638). Mirrors the element-pointer computation in
// genArrayIndex / genVectorIndex WITHOUT a bounds check — the receive's own
// operand eval already bounds-checked with the same index, so reaching here
// means the index is in range and c.block is the in-bounds block. Returns
// (ptr,true) for fixed-array and Vector Task elements; (nil,false) otherwise.
func (c *Compiler) genReceiveTaskSlotPtr(e *ast.IndexExpr) (value.Value, bool) {
	targetType := c.info.Types[e.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	if ref, ok := targetType.(*types.MutRef); ok {
		targetType = ref.Elem()
	}
	if ref, ok := targetType.(*types.SharedRef); ok {
		targetType = ref.Elem()
	}

	// Fixed-size array: GEP into the array alloca/field slot.
	if arr, ok := targetType.(*types.Array); ok {
		basePtr := c.genArrayBasePtr(e.Target, arr)
		idx := c.genExpr(e.Index)
		elemLLVM := c.resolveType(arr.Elem())
		arrType := irtypes.NewArray(uint64(arr.Size()), elemLLVM)
		return c.block.NewGetElementPtr(arrType, basePtr,
			constant.NewInt(irtypes.I32, 0), idx), true
	}

	// Vector: GEP into the heap data buffer past the fixed-size header. Mirrors
	// genVectorIndex's read path (genExprAutoPropagate — no COW; Task vectors
	// are always heap, never .rodata).
	named := extractNamed(targetType)
	var elemType types.Type
	if elem, ok := types.AsVector(targetType); ok {
		elemType = elem
	} else if named == types.TypVector && c.typeSubst != nil {
		tp := named.TypeParams()[0]
		if elem, ok := c.typeSubst[tp]; ok {
			elemType = elem
		}
	}
	if elemType != nil {
		slicePtr := c.genExprAutoPropagate(e.Target) // B0323
		idx := c.genExpr(e.Index)
		elemLLVM := c.resolveType(elemType)
		dataBase := c.block.NewGetElementPtr(irtypes.I8, slicePtr,
			constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
		dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
		return c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx), true
	}

	return nil, false
}

// genNativeIndex dispatches native [] implementations for built-in types.
func (c *Compiler) genNativeIndex(e *ast.IndexExpr, named *types.Named, targetType types.Type) value.Value {
	if named == types.TypString {
		return c.genStringIndex(e)
	}
	if elem, ok := types.AsVector(targetType); ok {
		return c.genVectorIndex(e, elem)
	}
	// Inside monomorphized method body: targetType is Named(Vector) not Instance(Vector[T]).
	// Get element type from typeSubst.
	if named == types.TypVector && c.typeSubst != nil {
		tp := named.TypeParams()[0]
		if elem, ok := c.typeSubst[tp]; ok {
			return c.genVectorIndex(e, elem)
		}
	}
	panic(fmt.Sprintf("codegen: no native [] implementation for type %s", named))
}

// genNativeSlice dispatches native [:] implementations for built-in types.
func (c *Compiler) genNativeSlice(named *types.Named, targetType types.Type, target, low, high value.Value) value.Value {
	if named == types.TypString {
		return c.genStringSlice(target, low, high)
	}
	panic(fmt.Sprintf("codegen: no native [:] implementation for type %s", named))
}

// genStringSlice implements string[start:end] by extracting a substring.
// Bounds are optional ints ({i1, i64}). Defaults: start=0, end=len.
func (c *Compiler) genStringSlice(strPtr, low, high value.Value) value.Value {
	instType := strInstanceType()
	typedPtr := c.block.NewBitCast(strPtr, irtypes.NewPointer(instType))

	// Load string length (masking off literal flag)
	length := loadStringLen(c.block, typedPtr, instType)

	// Resolve start: if present use value, else 0
	lowPresent := c.block.NewExtractValue(low, 0)
	lowVal := c.block.NewExtractValue(low, 1)
	start := c.block.NewSelect(lowPresent, lowVal, constant.NewInt(irtypes.I64, 0))

	// Resolve end: if present use value, else len
	highPresent := c.block.NewExtractValue(high, 0)
	highVal := c.block.NewExtractValue(high, 1)
	end := c.block.NewSelect(highPresent, highVal, length)

	// Compute slice length
	sliceLen := c.block.NewSub(end, start)

	// Get data pointer offset by start
	dataPtr := c.block.NewGetElementPtr(instType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	sliceDataPtr := c.block.NewGetElementPtr(irtypes.I8, dataPtr, start)

	// Create new string via promise_string_new
	return c.block.NewCall(c.funcs["promise_string_new"], sliceDataPtr, sliceLen)
}

// genMethodIndex calls the monomorphized [] method on a user type.
func (c *Compiler) genMethodIndex(e *ast.IndexExpr, targetType types.Type) value.Value {
	// Resolve mangled method name
	mangledName := mangleMethodName(c.resolveTypeName(targetType), "[]", false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared [] method %s", mangledName))
	}

	target := c.genExprAutoPropagate(e.Target) // B0323
	keyVal := c.genExpr(e.Index)

	// Extract instance pointer: container types (Vector, Map) are already i8*,
	// value types store to temp alloca, regular user types extract instance ptr.
	named := extractNamed(targetType)
	var instancePtr value.Value
	if isContainerType(targetType) {
		instancePtr = target
	} else if named != nil && named.IsValueType() {
		instancePtr = c.valueTypeReceiverPtr(target, targetType)
	} else {
		instancePtr = c.extractInstancePtr(target)
	}

	result := c.block.NewCall(fn, instancePtr, keyVal)

	// B0347: Dup borrowed string when `[]` on a Vector-shaped container returns
	// `Optional[string]` that points into the container's buffer. Without the
	// dup, the caller's unwrapped string and the element alias the same heap
	// allocation, causing double-free at scope exit. Guarded by
	// `c.dupStringFieldAccess` (set by the return site and typed var decls) so
	// temporary uses (comparisons, function args) don't dup. Map is not covered
	// here (see B0350) — enabling the dup for Map regressed existing tests that
	// rely on aliased map-value storage.
	if c.dupStringFieldAccess && c.tempTrackingEnabled && isContainerType(targetType) {
		resultType := c.info.Types[e]
		if c.typeSubst != nil {
			resultType = types.Substitute(resultType, c.typeSubst)
		}
		if opt, ok := resultType.(*types.Optional); ok && extractNamed(opt.Elem()) == types.TypString {
			c.dupStringFieldAccess = false // consume the flag
			innerStr := c.block.NewExtractValue(result, 1)
			dup := c.dupString(innerStr)
			c.trackStringTemp(dup)
			c.optionalStringDup = dup
			return c.block.NewInsertValue(result, dup, 1)
		}
	}

	// T0397: Dup borrowed tuple when `[]` returns `Optional[(droppable, ...)]`
	// whose inner fields alias the container's stored heap allocations. Without
	// the dup, force-unwrapping into a variable would let both the variable's
	// bindingDropTuple and the container's element walk drop the same heap data
	// → double-free. Mirrors B0347 (Optional[string]) and T0370 (Vector[Tuple]).
	// NOT gated on isContainerType — fires for Map and any other type with
	// `[]` returning `Optional[Tuple]`.
	if c.dupTupleFieldAccess && c.tempTrackingEnabled {
		resultType := c.info.Types[e]
		if c.typeSubst != nil {
			resultType = types.Substitute(resultType, c.typeSubst)
		}
		if opt, ok := resultType.(*types.Optional); ok {
			elem := opt.Elem()
			if c.typeSubst != nil {
				elem = types.Substitute(elem, c.typeSubst)
			}
			if tup, isTup := elem.(*types.Tuple); isTup && c.tupleNeedsDrop(elem) {
				c.dupTupleFieldAccess = false // consume the flag
				innerTup := c.block.NewExtractValue(result, 1)
				dup := c.dupTupleValue(innerTup, tup)
				c.optionalTupleDup = dup
				return c.block.NewInsertValue(result, dup, 1)
			}
		}
	}

	// T0440: Dup borrowed heap user type when `[]` returns `Optional[heap-user-type]`
	// whose inner value aliases the container's stored element. Without the dup,
	// force-unwrapping into a variable would let both the variable's drop binding
	// and the container's element walk drop the same instance → double-free.
	// Mirrors B0347 (Optional[string]) and T0397 (Optional[Tuple]). NOT gated on
	// isContainerType — fires for Map and any other type with `[]` returning
	// `Optional[heap-user-type]`.
	//
	// Gated to V types with an *explicit* user-written `drop()` AND no `clone()`
	// (T0484). `Map[K, V].[]` is a synthesized method whose body uses a match
	// destructure (`Slot.Used(k, v) => return v;`) that already dups V internally
	// when V is safely-dup'able (typeNeedsMatchDup → heapTypeSafeToDup walks
	// fields) or when V has a `clone()` method. The only V shape where Map.[]'s
	// body leaves V aliased is: explicit drop, no clone, no synth-drop. Firing
	// the dup outside that shape produces a redundant second copy whose pointer
	// is lost (one alloc leaks per read). Uses dupHeapValue (memcpy + sub-field
	// dup) directly, which is null-safe internally — important because `result`'s
	// value field is zero/null when the Optional is None.
	if c.dupHeapUserFieldAccess && c.tempTrackingEnabled {
		resultType := c.info.Types[e]
		if c.typeSubst != nil {
			resultType = types.Substitute(resultType, c.typeSubst)
		}
		if opt, ok := resultType.(*types.Optional); ok {
			elem := opt.Elem()
			if c.typeSubst != nil {
				elem = types.Substitute(elem, c.typeSubst)
			}
			if isDroppableHeapUserType(elem) {
				if named := extractNamed(elem); named != nil &&
					named.LookupMethod("drop") != nil &&
					named.LookupMethod("clone") == nil {
					c.dupHeapUserFieldAccess = false // consume the flag
					innerVal := c.block.NewExtractValue(result, 1)
					dup := c.dupHeapValue(innerVal, elem)
					c.optionalHeapDup = dup
					return c.block.NewInsertValue(result, dup, 1)
				}
			}
		}
	}

	// T0647: A user-defined non-native `[](int i) T` compiles to an ordinary
	// method whose body returns an *owned* heap value (IR-identical to the
	// equivalent plain method). The *ast.CallExpr genExpr case tracks such
	// return temps for cleanup at statement end; the *ast.IndexExpr case does
	// not, so `s[i]` (→ here) leaked the returned string/vector/Arc/.../heap-
	// user value while `s.at(i)` did not. Mirror the CallExpr post-call
	// tracking. This is reached ONLY for user-defined `[]` (genIndexExpr
	// dispatches native index reads to genNativeIndex/genStringIndex/
	// genVectorIndex/genArrayIndex, which return borrowed aliases and must NOT
	// be tracked), so the tracking is correctly scoped to owned operator
	// returns. The Optional-dup branches above return early and are unaffected.
	c.trackUserIndexResult(e, result)
	return result
}

// trackUserIndexResult mirrors the *ast.CallExpr post-call heap-temp tracking
// (genExpr) for the user-defined non-native `[]` read path (T0647). All track*
// helpers self-gate on c.tempTrackingEnabled / c.block.Term / SSA-dedup, so the
// unconditional calls here faithfully match the CallExpr path. findInnerCallExpr
// returns nil for *ast.IndexExpr, so trackHeapUserTypeResult performs no
// receiver-alias check (claimHeapTemp still dedups aliasing).
func (c *Compiler) trackUserIndexResult(e *ast.IndexExpr, result value.Value) {
	if result == nil {
		return
	}
	// T0649: mirror the CallExpr path — a borrow return (`T&`/`T~`) from a
	// user-defined `[]` operator is a reference into storage owned elsewhere,
	// not an owned temp. Resolve the static result type before the I8Ptr split
	// (so the heap-user `T~` branch is covered too) and skip tracking for
	// borrow results; otherwise the binding-site borrow-flag clear would leave
	// the value with no owner (leak), exactly as in the plain-method path.
	rt := c.info.Types[e]
	if c.typeSubst != nil && rt != nil {
		rt = types.Substitute(rt, c.typeSubst)
	}
	if c.selfSubst != nil && rt != nil {
		rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
	}
	if rt != nil && isRefType(rt) {
		return
	}
	if result.Type() == irtypes.I8Ptr {
		if rt != nil {
			named := extractNamed(rt)
			if named == types.TypString {
				c.trackStringTemp(result)
			} else if named == types.TypVector {
				if elemType, ok := types.AsVector(rt); ok {
					c.trackVectorTempWithElemType(result, elemType)
				} else {
					c.trackVectorTemp(result)
				}
			} else if arcElem, isArc := types.AsArc(rt); isArc {
				c.trackTempWithDrop(result, c.getOrCreateArcDrop(arcElem))
			} else if weakElem, isWeak := types.AsWeak(rt); isWeak {
				c.trackTempWithDrop(result, c.getOrCreateWeakDrop(weakElem))
			} else if mutexElem, isMutex := types.AsMutex(rt); isMutex {
				c.trackTempWithDrop(result, c.getOrCreateMutexDrop(mutexElem))
			} else if taskElem, isTask := types.AsTask(rt); isTask {
				c.trackTempWithDrop(result, c.getOrCreateTaskDrop(taskElem))
			} else if _, isMG := types.AsMutexGuard(rt); isMG {
				// T0561: MutexGuard.drop is a single non-per-element-type symbol.
				if dropFn, ok := c.funcs["MutexGuard.drop"]; ok {
					c.trackTempWithDrop(result, dropFn)
				}
			}
		}
	} else {
		c.trackHeapUserTypeResult(e, result)
	}
}

// genStringIndex implements string byte indexing: s[i] returns the byte at position i
// as a char (i32), zero-extended from i8. This is byte indexing (like Go's string[i]),
// not character indexing. UTF-8 decoding is handled separately by for-in loops.
// String instance layout: { i8* _variant, i64 len, [0 x i8] data }
func (c *Compiler) genStringIndex(e *ast.IndexExpr) value.Value {
	strPtr := c.genExprAutoPropagate(e.Target) // B0323
	idx := c.genExpr(e.Index)

	instType := strInstanceType()
	typedPtr := c.block.NewBitCast(strPtr, irtypes.NewPointer(instType))

	// Load len for bounds check (masking off literal flag)
	length := loadStringLen(c.block, typedPtr, instType)

	// Bounds check (unsigned comparison handles negative indices too)
	inBounds := c.block.NewICmp(enum.IPredULT, idx, length)
	okBlock := c.newBlock("stridx.ok")
	panicBlock := c.newBlock("stridx.oob")
	c.block.NewCondBr(inBounds, okBlock, panicBlock)

	// Out of bounds: panic
	c.block = panicBlock
	oobMsg := c.makeGlobalString("string index out of bounds")
	c.block.NewCall(c.funcs["promise_panic"], oobMsg)
	c.emitPanicReturn()

	// In bounds: load byte, zero-extend to i32 (char)
	c.block = okBlock
	dataPtr := c.block.NewGetElementPtr(instType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2),
		constant.NewInt(irtypes.I32, 0))
	bytePtr := c.block.NewGetElementPtr(irtypes.I8, dataPtr, idx)
	byteVal := c.block.NewLoad(irtypes.I8, bytePtr)
	return c.block.NewZExt(byteVal, irtypes.I32)
}

func (c *Compiler) genVectorIndex(e *ast.IndexExpr, elemType types.Type) value.Value {
	// T0648: a vector index only ever wants ONE element. If the target is a
	// Vector[Vector|Channel|Arc|Weak] field on an owner-droppable type,
	// genFieldAccess would consume dupContainerFieldAccess to deep-clone the
	// ENTIRE outer container and track it as a stmt-temp; the index then reads
	// one element out of that clone, scope cleanup drops the whole clone
	// (incl. that element), and the returned/bound inner pointer dangles
	// (panic / SIGSEGV). Suppress the whole-field dup for the target eval so
	// the element-level dup (the dupContainerFieldAccess branch below, T0383)
	// makes the owned copy instead. Mirrors the T0500 save/restore pattern.
	savedDupContainer := c.dupContainerFieldAccess
	c.dupContainerFieldAccess = false
	slicePtr := c.genExprAutoPropagate(e.Target) // B0323
	c.dupContainerFieldAccess = savedDupContainer
	idx := c.genExpr(e.Index)
	elemLLVM := c.resolveType(elemType)

	// Bounds check: load len (masked), compare index
	headerType := vectorHeaderType()
	headerPtr := c.block.NewBitCast(slicePtr, irtypes.NewPointer(headerType))
	length := loadVectorLen(c.block, headerPtr)

	inBounds := c.block.NewICmp(enum.IPredULT, idx, length)
	okBlock := c.newBlock("index.ok")
	panicBlock := c.newBlock("index.oob")
	c.block.NewCondBr(inBounds, okBlock, panicBlock)

	// Out of bounds: panic
	c.block = panicBlock
	oobMsg := c.makeGlobalString("index out of bounds")
	c.block.NewCall(c.funcs["promise_panic"], oobMsg)
	c.emitPanicReturn()

	// In bounds: load element
	c.block = okBlock
	dataBase := c.block.NewGetElementPtr(irtypes.I8, slicePtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
	elemPtr := c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx)
	val := c.block.NewLoad(elemLLVM, elemPtr)

	// B0204: Dup-on-read for Vector[string] index access. When the result will
	// be stored in a variable (signaled by dupStringFieldAccess), dup the string
	// so the variable owns an independent copy. This makes it safe to drop old
	// elements on overwrite without use-after-free through aliased locals.
	if c.dupStringFieldAccess && c.tempTrackingEnabled && extractNamed(elemType) == types.TypString {
		c.dupStringFieldAccess = false // consume the flag
		dup := c.dupString(val)
		c.trackStringTemp(dup)
		return dup
	}

	// T0370: Dup-on-read for Vector[(droppable, ...)] index access. Without this,
	// `t := v[0]` aliases v's element data — t's bindingDropTuple and v's element
	// walk would both drop the same heap allocations. Symmetric with the string
	// branch above (B0204).
	if c.dupTupleFieldAccess && c.tempTrackingEnabled {
		if tup, ok := elemType.(*types.Tuple); ok && c.tupleNeedsDrop(elemType) {
			c.dupTupleFieldAccess = false // consume the flag
			return c.dupTupleValue(val, tup)
		}
	}

	// T0398: Dup-on-read for Vector[heap-user-type] index access. Without this,
	// `b := v[0]` aliases v's element instance pointer — b's drop binding and
	// v's element walk would free the same instance. Symmetric with the string
	// branch above (B0204) and the tuple branch (T0370). cloneHeapElement calls
	// the type's clone() method when available, falls back to dupHeapValue
	// (alloc + memcpy + recursive sub-field dup via dupHeapValueFields).
	// The flag is set only when the call site (var-decl or vec-to-vec
	// assign) directly consumes the index expression, so the clone is
	// guaranteed to be moved into a binding/slot — no orphaned clone leaks.
	// Note: for polymorphic element types (e.g. Vector[Shape] containing Circle),
	// dupHeapValue uses the static element layout for memcpy size — same
	// limitation as B0204/T0370/T0376.
	// (Not borrow-gated — triggered by dupHeapUserFieldAccess flag set at the
	// var-decl AST site. Remains active post-T0438.)
	if c.dupHeapUserFieldAccess && c.tempTrackingEnabled {
		resolvedElem := elemType
		if c.typeSubst != nil {
			resolvedElem = types.Substitute(elemType, c.typeSubst)
		}
		if isDroppableHeapUserType(resolvedElem) {
			if named := extractNamed(resolvedElem); named != nil {
				c.dupHeapUserFieldAccess = false // consume the flag
				return c.cloneHeapElement(val, resolvedElem, named)
			}
		}
	}

	// T0383: Dup-on-read for Vector[Vector|Channel|Arc|Weak] index access. The
	// var-decl path sets dupContainerFieldAccess for these types (mirrors B0219
	// for fields). Without dup, `t := vec[i]` aliases vec's element buffer and
	// drop-on-write at the same slot (vec[i] = X) would create a UAF through t.
	// Symmetric with the string branch (B0204) and tuple branch (T0370).
	// (Not borrow-gated — triggered by dupContainerFieldAccess flag set at the
	// var-decl AST site, not by a borrow type on the RHS. Remains active post-T0438.)
	if c.dupContainerFieldAccess && c.tempTrackingEnabled {
		if innerElem, isVec := types.AsVector(elemType); isVec {
			c.dupContainerFieldAccess = false // consume the flag
			innerLLVM := c.resolveType(innerElem)
			innerSize := int64(c.typeSize(innerLLVM))
			dup := c.dupVector(val, innerSize)
			c.emitVectorElementCloneLoop(dup, innerElem)
			c.trackVectorTemp(dup)
			return dup
		}
		if extractNamed(elemType) == types.TypVector {
			c.dupContainerFieldAccess = false
			dup := c.dupVector(val, 0)
			c.trackVectorTemp(dup)
			return dup
		}
		if chElem, isCh := types.AsChannel(elemType); isCh {
			c.dupContainerFieldAccess = false
			dup := c.dupChannel(val)
			c.trackChannelTempWithElemType(dup, chElem) // T0663
			return dup
		}
		if arcElem, isArc := types.AsArc(elemType); isArc {
			c.dupContainerFieldAccess = false
			dup := c.dupArc(val)
			resolvedArcElem := arcElem
			if c.typeSubst != nil {
				resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
			}
			c.trackTempWithDrop(dup, c.getOrCreateArcDrop(resolvedArcElem))
			return dup
		}
		if weakElem, isWeak := types.AsWeak(elemType); isWeak {
			c.dupContainerFieldAccess = false
			resolvedWeakElem := weakElem
			if c.typeSubst != nil {
				resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
			}
			dup := c.dupWeak(val, resolvedWeakElem)
			c.trackTempWithDrop(dup, c.getOrCreateWeakDrop(resolvedWeakElem))
			return dup
		}
	}

	// T0620: Dup-on-read for Vector[T?] where T is droppable. The var-decl path
	// sets dup flags for Optional[string/Vector/Channel/Arc/Weak/heap-user/tuple]
	// (stmt.go:726-794), but the bare-type branches above don't match Optional
	// element types. When we detect an Optional with droppable inner and a matching
	// flag, consume the flag and deep-dup the inner value so the variable owns an
	// independent copy — preventing double-free between the variable's Optional
	// drop binding and the vector's element drop loop (now enabled by Gap B fix).
	if opt, ok := elemType.(*types.Optional); ok && c.tempTrackingEnabled {
		innerElem := opt.Elem()
		if c.typeSubst != nil {
			innerElem = types.Substitute(innerElem, c.typeSubst)
		}
		if c.typeNeedsFieldDrop(innerElem) {
			flagConsumed := false
			if c.dupStringFieldAccess && extractNamed(innerElem) == types.TypString {
				c.dupStringFieldAccess = false
				flagConsumed = true
			} else if c.dupContainerFieldAccess {
				if types.IsVector(innerElem) || types.IsChannel(innerElem) ||
					types.IsArc(innerElem) || types.IsWeak(innerElem) ||
					extractNamed(innerElem) == types.TypVector ||
					extractNamed(innerElem) == types.TypChannel {
					c.dupContainerFieldAccess = false
					flagConsumed = true
				}
			} else if c.dupTupleFieldAccess {
				if _, isTup := innerElem.(*types.Tuple); isTup {
					c.dupTupleFieldAccess = false
					flagConsumed = true
				}
			} else if c.dupHeapUserFieldAccess {
				if isDroppableHeapUserType(innerElem) || isHeapUserNoDropPalFree(innerElem) {
					c.dupHeapUserFieldAccess = false
					flagConsumed = true
				}
			}
			if flagConsumed {
				return c.dupOptionalVectorElem(val, opt, innerElem)
			}
		}
	}

	return val
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

// --- Map ---

// genMapLit creates a map instance via its new() constructor, then inserts each entry
// via the monomorphized []= method. Map is now a Promise-implemented user type.
func (c *Compiler) genMapLit(e *ast.MapLit) value.Value {
	typ := c.info.Types[e]
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	_, _, ok := types.AsMap(typ)
	if !ok {
		panic(fmt.Sprintf("codegen: map literal type is %T, want Map instance", typ))
	}
	inst, ok := typ.(*types.Instance)
	if !ok {
		panic(fmt.Sprintf("codegen: map literal type is %T, want Instance", typ))
	}

	// Construct the map (allocate + call new()) — reuse genConstructorCallMono logic
	mapVal := c.genMapConstructor(inst)

	// Insert entries via monomorphized []= method
	if len(e.Entries) > 0 {
		name := monoName(inst)
		setFnName := mangleMethodName(name, "[]=", false)
		setFn, ok := c.funcs[setFnName]
		if !ok {
			panic(fmt.Sprintf("codegen: undeclared map []= method %s", setFnName))
		}
		instancePtr := c.extractInstancePtr(mapVal)
		for _, entry := range e.Entries {
			savedEnumTemps := len(c.enumCtorTemps)
			keyVal := c.genExpr(entry.Key)
			valVal := c.genExpr(entry.Value)
			c.block.NewCall(setFn, instancePtr, keyVal, valVal)
			// B0280: Clear drop flags for values moved into the map via []=.
			// The []= method takes ~K key and ~V value (move semantics), so
			// ownership transfers to the map. Without this, the caller's
			// scope-exit cleanup double-drops the value (use-after-free).
			if ident, ok := entry.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
			if ident, ok := entry.Key.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
			// Claim heap temps: user type instances passed as map values
			// transfer ownership to the map. Without this, the heap temp
			// cleanup would free the instance, leaving a dangling pointer
			// in the map's Slot enum data.
			c.claimHeapTemp(valVal)
			c.claimHeapTemp(keyVal)
			// B0281: Clear enum ctor temps created during this entry's evaluation.
			// Map.[]= copies the enum value by LLVM value into the map's Slot.
			// Both the temp alloca and the Slot share the same inner pointers
			// (string ptrs, map instance ptrs, etc.). If the temp is dropped
			// at statement end, it frees data the map still references →
			// use-after-free / stack overflow on cleanup.
			// Only clear temps added since savedEnumTemps to avoid clobbering
			// temps from outer expressions (e.g., prior function arguments).
			for i := savedEnumTemps; i < len(c.enumCtorTemps); i++ {
				c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[i].dropFlag)
			}
			c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
		}
	}

	return mapVal
}

// genMapConstructor allocates a map instance and calls its new() constructor.
func (c *Compiler) genMapConstructor(inst *types.Instance) value.Value {
	layout := c.lookupTypeLayout(inst)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for map type %s", inst))
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

	// Allocate
	rawPtr := c.block.NewCall(c.palAlloc, size)
	typedPtr := c.block.NewBitCast(rawPtr, instancePtrType)

	// Store type info pointer in _variant slot (field 0)
	variantFieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	variantPtrType := layout.Instance.Fields[0].LLVMType.(*irtypes.PointerType)
	if tiGlobal := c.lookupTypeInfoGlobal(inst); tiGlobal != nil {
		tiPtr := c.block.NewBitCast(tiGlobal, variantPtrType)
		c.block.NewStore(tiPtr, variantFieldPtr)
	} else {
		c.block.NewStore(constant.NewNull(variantPtrType), variantFieldPtr)
	}

	// Zero-init all fields
	origin := inst.Origin().(*types.Named)
	for _, f := range origin.AllFields() {
		fieldIdx := layout.InstanceFieldIndex[f.Name()]
		fieldPtr := c.block.NewGetElementPtr(instanceStructType, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
		c.block.NewStore(c.zeroValue(layout.Instance.Fields[fieldIdx].LLVMType), fieldPtr)
	}

	// Call new() constructor
	name := monoName(inst)
	mangledName := mangleMethodName(name, "new", false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared new() for map type %s (mangled: %s)", inst, mangledName))
	}
	c.block.NewCall(fn, typedPtr)

	// Build value struct { vtable_ptr, instance_ptr }
	var vtablePtr value.Value
	if vtableGlobal := c.lookupVtableGlobal(inst); vtableGlobal != nil {
		vtablePtr = c.block.NewBitCast(vtableGlobal, irtypes.I8Ptr)
	} else {
		vtablePtr = constant.NewNull(irtypes.I8Ptr)
	}

	var valStruct value.Value = constant.NewZeroInitializer(userValueType())
	valStruct = c.block.NewInsertValue(valStruct, vtablePtr, 0)
	valStruct = c.block.NewInsertValue(valStruct, c.block.NewBitCast(typedPtr, irtypes.I8Ptr), 1)
	return valStruct
}

// --- Lambda ---

func (c *Compiler) genLambdaExpr(e *ast.LambdaExpr) value.Value {
	sig, ok := c.info.Types[e].(*types.Signature)
	if !ok {
		panic("codegen: lambda expression type is not *types.Signature")
	}

	// Collect captures from sema info
	captures := c.info.LambdaCaptures[e]

	// Build LLVM function type — env pointer (i8*) is always the first parameter
	retType := irtypes.Type(irtypes.Void)
	if sig.Result() != nil {
		retType = c.resolveType(sig.Result())
	}

	params := []*ir.Param{ir.NewParam("env", irtypes.I8Ptr)}
	for _, p := range sig.Params() {
		params = append(params, ir.NewParam(p.Name(), c.resolveType(p.Type())))
	}

	// Create anonymous function
	lambdaName := fmt.Sprintf(".lambda.%d", c.lambdaCounter)
	c.lambdaCounter++
	fn := c.module.NewFunc(lambdaName, retType, params...)

	// Build env struct type and capture values from the enclosing scope BEFORE switching context
	var envStructType *irtypes.StructType
	var envPtr value.Value
	if len(captures) > 0 {
		// B0221: Field 0 is the env drop function pointer (i8*). Captures start at field 1.
		// This makes the env self-describing — cleanup code can load field 0 and call the
		// drop function to properly drop captured values before freeing the env struct.
		envFieldTypes := make([]irtypes.Type, len(captures)+1)
		envFieldTypes[0] = irtypes.I8Ptr // env drop fn pointer
		captureVals := make([]value.Value, len(captures))
		for i, cv := range captures {
			captureType := c.resolveType(cv.Obj.Type())
			// For 'this', use the alloca's element type (instance pointer) rather
			// than the sema type (value struct). The receiver is stored as a pointer
			// in method bodies, not as a full value struct.
			if alloca, ok := c.locals[cv.Obj.Name()]; ok {
				if cv.Obj.Name() == "this" {
					captureType = alloca.ElemType
				}
				captureVals[i] = c.block.NewLoad(captureType, alloca)
			} else {
				captureVals[i] = constant.NewZeroInitializer(captureType)
			}
			envFieldTypes[i+1] = captureType // +1 for header (B0221)
			// For move captures, clear the drop flag in the enclosing scope
			if cv.ByMove {
				c.clearDropFlag(cv.Obj.Name())
			}
		}
		envStructType = irtypes.NewStruct(envFieldTypes...)

		// B0221: Generate env drop function if any captures need dropping
		envDropFn := c.genEnvDropFunc(lambdaName, envStructType, captures)

		// Allocate env struct on heap
		envSize := int64(c.typeSize(envStructType))
		rawPtr := c.block.NewCall(c.palAlloc, constant.NewInt(irtypes.I64, envSize))
		typedEnvPtr := c.block.NewBitCast(rawPtr, irtypes.NewPointer(envStructType))

		// B0221: Store env drop fn pointer as field 0
		var envDropFnVal value.Value
		if envDropFn != nil {
			envDropFnVal = c.block.NewBitCast(envDropFn, irtypes.I8Ptr)
		} else {
			envDropFnVal = constant.NewNull(irtypes.I8Ptr)
		}
		dropFnField := c.block.NewGetElementPtr(envStructType, typedEnvPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		c.block.NewStore(envDropFnVal, dropFnField)

		// Store captured values into env struct (offset by 1 for header, B0221)
		for i, val := range captureVals {
			fieldPtr := c.block.NewGetElementPtr(envStructType, typedEnvPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i+1)))
			c.block.NewStore(val, fieldPtr)
		}
		envPtr = rawPtr // i8*
	} else {
		envPtr = constant.NewNull(irtypes.I8Ptr)
	}

	// Save current state
	savedFn := c.fn
	savedBlock := c.block
	savedEntryBlock := c.entryBlock
	savedLocals := c.locals
	savedCanError := c.canError
	savedRetType := c.currentRetType
	savedBlockCounter := c.blockCounter
	savedScopeBindings := c.scopeBindings
	savedDropFlags := c.dropFlags
	savedDropBindings := c.dropBindings // B0035: must save/restore for NLL early drops
	savedLoopScopeDepth := c.loopScopeDepth
	savedWritebacks := c.lambdaWritebacks
	savedGoExprFF2 := c.goExprFireAndForget
	savedStmtTemps := c.stmtTemps                       // T0073
	savedStmtTempMap := c.stmtTempMap                   // T0073
	savedHeapTemps := c.heapTemps                       // T0088
	savedHeapTempMap := c.heapTempMap                   // T0088
	savedEnvTemps := c.envTemps                         // T0100
	savedEnvTempMap := c.envTempMap                     // T0100
	savedEnumCtorTemps := c.enumCtorTemps               // B0267
	savedTempTracking := c.tempTrackingEnabled          // T0073
	savedLocalNameCount := c.localNameCount             // T0261
	savedPanicExitBlock := c.panicExitBlock             // T0262: clear in lambda (separate function)
	savedCoroutineReturnBlock := c.coroutineReturnBlock // T0262: clear in lambda (separate function)
	savedInCoroutine := c.inCoroutine                   // T0285: lambda is a separate function, not a coroutine
	savedCoroCleanup := c.coroCleanupBlk                // T0285: save coroutine blocks
	savedCoroSuspend := c.coroSuspendBlk                // T0285: save coroutine blocks
	c.goExprFireAndForget = false                       // reset for inner statements (B0109)
	c.panicExitBlock = nil                              // T0262: lambda is a separate function
	c.coroutineReturnBlock = nil                        // T0262: lambda is a separate function
	c.inCoroutine = false                               // T0285: lambda is not a coroutine
	c.coroCleanupBlk = nil                              // T0285: no coroutine infrastructure
	c.coroSuspendBlk = nil                              // T0285: no coroutine infrastructure

	// Generate lambda body with fresh scope state
	c.fn = fn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.blockCounter = 0
	c.canError = false
	c.currentRetType = sig.Result()
	c.scopeBindings = nil
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.dropBindings = make(map[string]scopeBinding)
	c.stmtTemps = nil                         // T0073
	c.stmtTempMap = make(map[value.Value]int) // T0073
	c.heapTemps = nil                         // T0088
	c.heapTempMap = make(map[value.Value]int) // T0088
	c.envTemps = nil                          // T0100
	c.envTempMap = make(map[value.Value]int)  // T0100
	c.enumCtorTemps = nil                     // B0267
	c.tempTrackingEnabled = true              // B0259: enable temp tracking in lambda bodies
	c.loopScopeDepth = 0
	c.lambdaWritebacks = nil

	entry := fn.NewBlock(".entry")
	c.block = entry
	c.entryBlock = entry

	// Load captured variables from env struct into local allocas
	if len(captures) > 0 && envStructType != nil {
		typedEnvPtr := entry.NewBitCast(fn.Params[0], irtypes.NewPointer(envStructType))
		for i, cv := range captures {
			// Use the env struct's field type — matches what was stored during capture
			// B0221: Field 0 is env drop fn; captures start at field i+1
			captureType := envStructType.Fields[i+1]
			fieldPtr := entry.NewGetElementPtr(envStructType, typedEnvPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i+1)))
			val := entry.NewLoad(captureType, fieldPtr)
			alloca := entry.NewAlloca(captureType)
			alloca.SetName(c.uniqueLocalName(cv.Obj.Name() + ".cap"))
			entry.NewStore(val, alloca)
			c.locals[cv.Obj.Name()] = alloca
			// For move captures, register write-back so mutations persist across calls
			if cv.ByMove {
				c.lambdaWritebacks = append(c.lambdaWritebacks, lambdaWriteback{
					localAlloca: alloca,
					envFieldPtr: fieldPtr,
					elemType:    captureType,
				})
				// T0554: Do NOT register a scope-exit drop on the capture local. The env
				// drop function (genEnvDropFunc) is responsible for dropping all captured
				// values when the env struct is freed. Registering here would double-drop
				// (lambda-body exit + env_drop), causing segfaults for user types and
				// double-frees for any type with droppable fields.
				// B0229: Register reassignment-only drop for captured optional structural
				// interfaces (e.g., Iterator[R]? in flat_map). Only added to dropBindings,
				// NOT scopeBindings — the env drop function handles final cleanup, and
				// scope-exit drop would free a value that's been written back to the env.
				c.maybeRegisterCapturedOptionalStructuralDrop(cv.Obj.Name(), alloca, cv.Obj.Type())
			}
		}
	}

	// Allocate user parameters (offset by 1 due to env param)
	for i, p := range sig.Params() {
		if p.Name() == "" || p.Name() == "_" {
			continue
		}
		alloca := entry.NewAlloca(c.resolveType(p.Type()))
		alloca.SetName(c.uniqueLocalName(p.Name() + ".addr"))
		entry.NewStore(fn.Params[i+1], alloca) // +1 for env param
		c.locals[p.Name()] = alloca
	}

	// Generate body
	if e.Body != nil {
		c.genBlock(e.Body)
	} else if e.ExprBody != nil {
		val := c.genExpr(e.ExprBody)
		if val != nil && c.block.Term == nil {
			// B0259: Clean up string/heap/env temps from the expression.
			// Claim the return value first so it's not freed.
			c.claimStringTemp(val)
			c.claimHeapTemp(val)
			c.claimEnvTemp(val)
			c.cleanupStmtTemps()
			c.cleanupHeapTemps()
			c.cleanupEnvTemps()
			// Clean up capture bindings before returning
			if len(c.scopeBindings) > 0 {
				cap := c.emitScopeCleanup(0, false)
				c.emitCloseErrCheck(cap)
			}
			c.block.NewRet(val)
		}
	}

	// Ensure terminator — clean up remaining capture bindings on fallthrough
	if c.block != nil && c.block.Term == nil {
		c.emitLambdaWritebacks()
		if len(c.scopeBindings) > 0 {
			cap := c.emitScopeCleanup(0, false)
			c.emitCloseErrCheck(cap)
		}
		if _, ok := fn.Sig.RetType.(*irtypes.VoidType); ok {
			c.block.NewRet(nil)
		} else {
			c.block.NewRet(c.zeroValue(fn.Sig.RetType))
		}
	}

	// Restore state
	c.fn = savedFn
	c.block = savedBlock
	c.entryBlock = savedEntryBlock
	c.locals = savedLocals
	c.canError = savedCanError
	c.currentRetType = savedRetType
	c.blockCounter = savedBlockCounter
	c.scopeBindings = savedScopeBindings
	c.dropFlags = savedDropFlags
	c.dropBindings = savedDropBindings // B0035: restore for NLL early drops
	c.loopScopeDepth = savedLoopScopeDepth
	c.lambdaWritebacks = savedWritebacks
	c.goExprFireAndForget = savedGoExprFF2
	c.stmtTemps = savedStmtTemps                       // T0073
	c.stmtTempMap = savedStmtTempMap                   // T0073
	c.heapTemps = savedHeapTemps                       // T0088
	c.heapTempMap = savedHeapTempMap                   // T0088
	c.envTemps = savedEnvTemps                         // T0100
	c.envTempMap = savedEnvTempMap                     // T0100
	c.enumCtorTemps = savedEnumCtorTemps               // B0267
	c.tempTrackingEnabled = savedTempTracking          // T0073
	c.localNameCount = savedLocalNameCount             // T0261
	c.panicExitBlock = savedPanicExitBlock             // T0262
	c.coroutineReturnBlock = savedCoroutineReturnBlock // T0262
	c.inCoroutine = savedInCoroutine                   // T0285
	c.coroCleanupBlk = savedCoroCleanup                // T0285
	c.coroSuspendBlk = savedCoroSuspend                // T0285

	// T0100: Track env temp for non-variable lambdas. If this lambda is
	// assigned to a variable, maybeRegisterEnvFree handles cleanup and the
	// env temp will be claimed. Otherwise, unclaimed envs are freed at statement end.
	if len(captures) > 0 {
		c.trackEnvTemp(envPtr)
	}

	// Return fat pointer: {fn_ptr as i8*, env_ptr}
	fnPtr := c.block.NewBitCast(fn, irtypes.I8Ptr)
	var closure value.Value = constant.NewUndef(closureType())
	closure = c.block.NewInsertValue(closure, fnPtr, 0)
	closure = c.block.NewInsertValue(closure, envPtr, 1)
	return closure
}

// --- Env Drop Function Generation (B0221) ---

// envDropAction describes what cleanup a captured value needs in the env drop function.
type envDropAction int

const (
	envDropNone               envDropAction = iota
	envDropCallFn                           // call dropFn(i8*) — string, vector, channel (handles free internally)
	envDropClosureEnv                       // extract env from closure {i8*,i8*}, env-drop-or-free
	envDropUserValue                        // extract inst from value {i8*,i8*}, pal_free — heap user type without drop
	envDropUserValueDrop                    // extract inst from value {i8*,i8*}, call cleanup fn (synth drop incl. pal_free, or $wrap, or palFree)
	envDropOptionalStructural               // B0229: optional structural iface — check has_value, extract inst, cleanup
)

type envFieldDrop struct {
	action envDropAction
	dropFn *ir.Func
}

// analyzeEnvCaptureDrop determines the drop action for a single captured variable.
// Applies type substitution so the analysis uses concrete (monomorphized) types.
func (c *Compiler) analyzeEnvCaptureDrop(cv *sema.CapturedVar) envFieldDrop {
	typ := cv.Obj.Type()
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	if c.selfSubst != nil {
		typ = types.SubstituteSelf(typ, c.selfSubst.iface, c.selfSubst.concrete)
	}
	// String/Vector/Channel → call specific drop function (i8* field, drop handles free)
	named := extractNamed(typ)
	if named == types.TypString {
		if fn := c.funcs["promise_string_drop"]; fn != nil {
			return envFieldDrop{envDropCallFn, fn}
		}
	}
	if _, ok := types.AsVector(typ); ok || (named != nil && named == types.TypVector) {
		if fn := c.funcs["Vector.drop"]; fn != nil {
			return envFieldDrop{envDropCallFn, fn}
		}
	}
	if elemType, ok := types.AsChannel(typ); ok || (named != nil && named == types.TypChannel) {
		// T0663: per-element-type drop walks any un-received buffered items.
		if ok {
			return envFieldDrop{envDropCallFn, c.getOrCreateChannelDrop(elemType)}
		}
	}
	// Arc/Weak/Mutex/MutexGuard → call per-element-type drop function (i8* field, same pattern as string/vector/channel)
	if elemType, ok := types.AsArc(typ); ok || (named != nil && named == types.TypArc) {
		if ok {
			return envFieldDrop{envDropCallFn, c.getOrCreateArcDrop(elemType)}
		}
	}
	if elemType, ok := types.AsWeak(typ); ok || (named != nil && named == types.TypWeak) {
		if ok {
			return envFieldDrop{envDropCallFn, c.getOrCreateWeakDrop(elemType)}
		}
	}
	if elemType, ok := types.AsMutex(typ); ok || (named != nil && named == types.TypMutex) {
		if ok {
			return envFieldDrop{envDropCallFn, c.getOrCreateMutexDrop(elemType)}
		}
	}
	if _, ok := types.AsMutexGuard(typ); ok || (named != nil && named == types.TypMutexGuard) {
		if fn := c.funcs["MutexGuard.drop"]; fn != nil {
			return envFieldDrop{envDropCallFn, fn}
		}
	}
	// T0503: Task[T] capture → per-instantiation drop (spin-wait + free).
	if elemType, ok := types.AsTask(typ); ok || (named != nil && named == types.TypTask) {
		if ok {
			return envFieldDrop{envDropCallFn, c.getOrCreateTaskDrop(elemType)}
		}
	}

	// Closure (Signature) → free inner env
	if _, ok := typ.(*types.Signature); ok {
		return envFieldDrop{envDropClosureEnv, nil}
	}

	// Heap user type — need to free instance (and call drop if it has one).
	// Skip `this` captures: method receivers are borrowed from the caller, which
	// handles cleanup via its own scope binding. Freeing here would double-free.
	//
	// T0554: Use resolveDropFuncForTemp to get the correct cleanup function.
	// For synthesized drops, the bare drop already includes pal_free. For
	// explicit user drops, $wrap is returned which calls drop + pal_free.
	// Either way, env_drop just calls this function (no separate pal_free).
	if named != nil && cv.Obj.Name() != "this" && !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural() {
		if dropFn := c.resolveDropFuncForTemp(named, typ); dropFn != nil {
			return envFieldDrop{envDropUserValueDrop, dropFn}
		}
		return envFieldDrop{envDropUserValue, nil}
	}

	// B0243: Optional structural interface (e.g., Iterator[T]?) — use RTTI-based drop dispatch.
	// The concrete type is unknown at compile time, so we can't use __promise_iter_cleanup
	// (which assumes _FnIter layout). Instead, we dispatch through typeinfo.drop_fn_ptr.
	if opt, ok := typ.(*types.Optional); ok {
		elem := opt.Elem()
		if c.typeSubst != nil {
			elem = types.Substitute(elem, c.typeSubst)
		}
		innerNamed := extractNamed(elem)
		if innerNamed != nil && innerNamed.IsStructural() && !innerNamed.IsValueType() {
			return envFieldDrop{envDropOptionalStructural, nil}
		}
	}

	return envFieldDrop{envDropNone, nil}
}

// genEnvDropFunc generates a per-closure env drop function that drops each captured
// value that needs dropping before freeing the env struct. Returns nil if no captures
// need dropping (callers will use pal_free directly via the null header check).
// The env struct layout is: { i8* env_drop_fn, capture0, capture1, ... }.
//
// Handles: strings, vectors, channels, heap user types (with/without drop),
// and closure captures (frees inner env). Skips `this` captures (borrowed, not owned).
func (c *Compiler) genEnvDropFunc(lambdaName string, envStructType *irtypes.StructType, captures []*sema.CapturedVar) *ir.Func {
	// Analyze each capture to determine drop action
	actions := make([]envFieldDrop, len(captures))
	hasAnyAction := false
	for i, cv := range captures {
		actions[i] = c.analyzeEnvCaptureDrop(cv)
		if actions[i].action != envDropNone {
			hasAnyAction = true
		}
	}
	if !hasAnyAction {
		return nil
	}

	dropFnName := lambdaName + ".env_drop"
	dropFn := c.module.NewFunc(dropFnName, irtypes.Void, ir.NewParam("env", irtypes.I8Ptr))

	curBlock := dropFn.NewBlock(".entry")
	typedPtr := curBlock.NewBitCast(dropFn.Params[0], irtypes.NewPointer(envStructType))

	blockIdx := 0
	for i := range captures {
		act := actions[i]
		if act.action == envDropNone {
			continue
		}

		fieldIdx := int64(i + 1) // +1 for env_drop_fn header
		fieldPtr := curBlock.NewGetElementPtr(envStructType, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, fieldIdx))
		fieldVal := curBlock.NewLoad(envStructType.Fields[i+1], fieldPtr)

		nextBlk := dropFn.NewBlock(fmt.Sprintf("next.%d", blockIdx))

		switch act.action {
		case envDropCallFn:
			// i8* field (string/vector/channel): null-check, call drop fn
			isNull := curBlock.NewICmp(enum.IPredEQ, fieldVal, constant.NewNull(irtypes.I8Ptr))
			dropBlk := dropFn.NewBlock(fmt.Sprintf("drop.%d", blockIdx))
			curBlock.NewCondBr(isNull, nextBlk, dropBlk)
			dropBlk.NewCall(act.dropFn, fieldVal)
			dropBlk.NewBr(nextBlk)

		case envDropClosureEnv:
			// Closure fat pointer {fn_ptr, env_ptr}: extract env, env-drop-or-free
			innerEnvPtr := curBlock.NewExtractValue(fieldVal, 1)
			isNull := curBlock.NewICmp(enum.IPredEQ, innerEnvPtr, constant.NewNull(irtypes.I8Ptr))
			checkBlk := dropFn.NewBlock(fmt.Sprintf("clo.check.%d", blockIdx))
			curBlock.NewCondBr(isNull, nextBlk, checkBlk)
			// Load inner env's drop fn header
			envHeaderType := irtypes.NewStruct(irtypes.I8Ptr)
			typedHdr := checkBlk.NewBitCast(innerEnvPtr, irtypes.NewPointer(envHeaderType))
			hdrField := checkBlk.NewGetElementPtr(envHeaderType, typedHdr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			innerDropRaw := checkBlk.NewLoad(irtypes.I8Ptr, hdrField)
			hasInnerDrop := checkBlk.NewICmp(enum.IPredNE, innerDropRaw, constant.NewNull(irtypes.I8Ptr))
			deepBlk := dropFn.NewBlock(fmt.Sprintf("clo.deep.%d", blockIdx))
			shallowBlk := dropFn.NewBlock(fmt.Sprintf("clo.free.%d", blockIdx))
			checkBlk.NewCondBr(hasInnerDrop, deepBlk, shallowBlk)
			// Deep drop: call inner env's drop function
			innerDropFnType := irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr)
			typedInnerDrop := deepBlk.NewBitCast(innerDropRaw, irtypes.NewPointer(innerDropFnType))
			deepBlk.NewCall(typedInnerDrop, innerEnvPtr)
			deepBlk.NewBr(nextBlk)
			// Shallow free: just pal_free the inner env
			shallowBlk.NewCall(c.palFree, innerEnvPtr)
			shallowBlk.NewBr(nextBlk)

		case envDropUserValue:
			// User type value struct {vtable, instance}: extract instance, null-check, pal_free
			instPtr := curBlock.NewExtractValue(fieldVal, 1)
			isNull := curBlock.NewICmp(enum.IPredEQ, instPtr, constant.NewNull(irtypes.I8Ptr))
			freeBlk := dropFn.NewBlock(fmt.Sprintf("ufree.%d", blockIdx))
			curBlock.NewCondBr(isNull, nextBlk, freeBlk)
			freeBlk.NewCall(c.palFree, instPtr)
			freeBlk.NewBr(nextBlk)

		case envDropUserValueDrop:
			// User type value struct {vtable, instance}: extract instance, null-check, call cleanup fn.
			// T0554: dropFn is from resolveDropFuncForTemp — synth drops include pal_free,
			// explicit-drop $wrap calls drop + pal_free. Either way, do NOT call pal_free
			// separately or we double-free.
			instPtr := curBlock.NewExtractValue(fieldVal, 1)
			isNull := curBlock.NewICmp(enum.IPredEQ, instPtr, constant.NewNull(irtypes.I8Ptr))
			dropBlk := dropFn.NewBlock(fmt.Sprintf("udrop.%d", blockIdx))
			curBlock.NewCondBr(isNull, nextBlk, dropBlk)
			dropBlk.NewCall(act.dropFn, instPtr)
			dropBlk.NewBr(nextBlk)

		case envDropOptionalStructural:
			// B0243: Optional structural iface {i1 has_value, {i8* vtable, i8* instance}}:
			// check has_value, extract instance, RTTI-based drop dispatch.
			// The concrete type is unknown, so we load drop_fn from typeinfo.
			hasVal := curBlock.NewExtractValue(fieldVal, 0)
			innerBlk := dropFn.NewBlock(fmt.Sprintf("optst.inner.%d", blockIdx))
			curBlock.NewCondBr(hasVal, innerBlk, nextBlk)
			innerVal := innerBlk.NewExtractValue(fieldVal, 1)
			instPtr := innerBlk.NewExtractValue(innerVal, 1)
			isNull := innerBlk.NewICmp(enum.IPredEQ, instPtr, constant.NewNull(irtypes.I8Ptr))
			rttiBlk := dropFn.NewBlock(fmt.Sprintf("optst.rtti.%d", blockIdx))
			innerBlk.NewCondBr(isNull, nextBlk, rttiBlk)

			// Load variant ptr (typeinfo) from instance[0]
			instStructType := irtypes.NewStruct(irtypes.I8Ptr)
			typedInst := rttiBlk.NewBitCast(instPtr, irtypes.NewPointer(instStructType))
			variantField := rttiBlk.NewGetElementPtr(instStructType, typedInst,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			variantPtr := rttiBlk.NewLoad(irtypes.I8Ptr, variantField)

			// Load drop_fn_ptr from typeinfo[1]
			typeinfoType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr)
			typedTI := rttiBlk.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoType))
			dropFnField := rttiBlk.NewGetElementPtr(typeinfoType, typedTI,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			dropFnRaw := rttiBlk.NewLoad(irtypes.I8Ptr, dropFnField)
			hasDropFn := rttiBlk.NewICmp(enum.IPredNE, dropFnRaw, constant.NewNull(irtypes.I8Ptr))

			callDropBlk := dropFn.NewBlock(fmt.Sprintf("optst.drop.%d", blockIdx))
			justFreeBlk := dropFn.NewBlock(fmt.Sprintf("optst.free.%d", blockIdx))
			rttiBlk.NewCondBr(hasDropFn, callDropBlk, justFreeBlk)

			// Has drop: call it (handles free for synth/native drops)
			dropFnType := irtypes.NewFunc(irtypes.Void, irtypes.I8Ptr)
			typedDropFn := callDropBlk.NewBitCast(dropFnRaw, irtypes.NewPointer(dropFnType))
			callDropBlk.NewCall(typedDropFn, instPtr)
			callDropBlk.NewBr(nextBlk)

			// No drop: just free the instance
			justFreeBlk.NewCall(c.palFree, instPtr)
			justFreeBlk.NewBr(nextBlk)
		}

		curBlock = nextBlk
		blockIdx++
	}

	// Free the env struct itself
	curBlock.NewCall(c.palFree, dropFn.Params[0])
	curBlock.NewRet(nil)

	return dropFn
}

// --- Optional Chaining ---

// genOptionalChainExpr generates x?.field — checks if the optional is present,
// accesses the field on the inner value in the some-block, returns none in the none-block.
func (c *Compiler) genOptionalChainExpr(e *ast.OptionalChainExpr) value.Value {
	optVal := c.genExpr(e.Target)

	// Extract flag (field 0)
	flag := c.block.NewExtractValue(optVal, 0)

	someBlock := c.newBlock("optchain.some")
	noneBlock := c.newBlock("optchain.none")
	mergeBlock := c.newBlock("optchain.merge")

	c.block.NewCondBr(flag, someBlock, noneBlock)

	// Some: extract inner value, access field, wrap in Optional
	c.block = someBlock
	innerVal := c.block.NewExtractValue(optVal, 1)

	// Resolve the inner type from sema
	targetType := c.info.Types[e.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	optType := targetType.(*types.Optional)
	innerType := optType.Elem()

	// Access field on inner value
	fieldVal := c.genFieldOnValue(innerVal, innerType, e.Field)

	// Determine the result Optional type from sema
	resultType := c.info.Types[e]
	if c.typeSubst != nil {
		resultType = types.Substitute(resultType, c.typeSubst)
	}
	resultLLVM := c.resolveType(resultType).(*irtypes.StructType)

	someResult := c.wrapOptional(fieldVal, resultLLVM)
	c.block.NewBr(mergeBlock)
	someEnd := c.block

	// None: zeroinit Optional
	c.block = noneBlock
	noneResult := constant.NewZeroInitializer(resultLLVM)
	c.block.NewBr(mergeBlock)
	noneEnd := c.block

	// Merge
	c.block = mergeBlock
	return mergeBlock.NewPhi(
		&ir.Incoming{X: someResult, Pred: someEnd},
		&ir.Incoming{X: noneResult, Pred: noneEnd},
	)
}

// genFieldOnValue accesses a field or getter on a value of a known type.
// For fields on user types (i8* pointers), it does bitcast + GEP.
// For getters, it emits a direct call to the getter method.
func (c *Compiler) genFieldOnValue(val value.Value, typ types.Type, fieldName string) value.Value {
	named := extractNamed(typ)
	if named == nil {
		panic(fmt.Sprintf("codegen: cannot access field %s on type %s", fieldName, typ))
	}

	field := named.LookupField(fieldName)
	if field != nil {
		layout := c.lookupTypeLayout(typ)
		if layout == nil {
			panic(fmt.Sprintf("codegen: no layout for type %s", typ))
		}

		// Value types: fields are in the value struct
		if layout.IsValueType {
			fieldIdx, ok := layout.ValueFieldIndex[field.Name()]
			if !ok {
				panic(fmt.Sprintf("codegen: field %s not in value layout for %s", field.Name(), typ))
			}
			return c.block.NewExtractValue(val, uint64(fieldIdx))
		}

		fieldIdx, ok := layout.InstanceFieldIndex[field.Name()]
		if !ok {
			panic(fmt.Sprintf("codegen: field %s not in instance layout for %s", field.Name(), typ))
		}

		// val is a value struct {vtable_ptr, instance_ptr} — extract the instance pointer
		instance := c.extractInstancePtr(val)
		typedPtr := c.block.NewBitCast(instance, layout.InstancePtrType)
		fieldPtr := c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))

		return c.block.NewLoad(layout.Instance.Fields[fieldIdx].LLVMType, fieldPtr)
	}

	// Getter property: emit a direct call with the value as receiver
	if g := named.LookupGetter(fieldName); g != nil {
		mangledName := mangleMethodName(c.resolveTypeName(typ), fieldName, false)
		fn, ok := c.funcs[mangledName]
		if !ok {
			panic(fmt.Sprintf("codegen: undeclared getter %s", mangledName))
		}
		// Global getter: no receiver
		if g.Sig().Recv() == nil {
			return c.block.NewCall(fn)
		}
		// val is a value struct — pass it directly (getters expect the value struct as receiver)
		return c.block.NewCall(fn, val)
	}

	panic(fmt.Sprintf("codegen: no field or getter %s on type %s", fieldName, named))
}

// genIndirectCall calls a function through a fat pointer {i8* fn, i8* env}.
// Extracts the function pointer and env pointer, then calls with env as the first arg.
func (c *Compiler) genIndirectCall(closure value.Value, sig *types.Signature, args []value.Value) value.Value {
	retType := irtypes.Type(irtypes.Void)
	if sig.Result() != nil {
		retType = c.resolveType(sig.Result())
	}

	// Function type includes env (i8*) as first parameter
	paramTypes := []irtypes.Type{irtypes.I8Ptr}
	for _, p := range sig.Params() {
		paramTypes = append(paramTypes, c.resolveType(p.Type()))
	}

	funcType := irtypes.NewFunc(retType, paramTypes...)
	funcPtrType := irtypes.NewPointer(funcType)

	// Extract fn and env from fat pointer
	fnRaw := c.block.NewExtractValue(closure, 0)
	envPtr := c.block.NewExtractValue(closure, 1)

	typedFnPtr := c.block.NewBitCast(fnRaw, funcPtrType)

	// Call with env as first arg, then user args
	callArgs := make([]value.Value, 0, len(args)+1)
	callArgs = append(callArgs, envPtr)
	callArgs = append(callArgs, args...)
	return c.block.NewCall(typedFnPtr, callArgs...)
}

// getOrCreateThunk returns a trampoline function with the env-first ABI that
// forwards to the given named function. This allows named function references
// to be called through the same fat-pointer indirect call path as lambdas.
func (c *Compiler) getOrCreateThunk(fn *ir.Func, name string) *ir.Func {
	if thunk, ok := c.thunks[name]; ok {
		return thunk
	}

	// Build thunk params: env (i8*) + original function params
	params := []*ir.Param{ir.NewParam("env", irtypes.I8Ptr)}
	for _, p := range fn.Params {
		params = append(params, ir.NewParam(p.LocalName, p.Typ))
	}

	thunkName := ".thunk." + name
	thunk := c.module.NewFunc(thunkName, fn.Sig.RetType, params...)
	entry := thunk.NewBlock(".entry")

	// Forward call to original function, skipping the env param
	callArgs := make([]value.Value, len(fn.Params))
	for i := range fn.Params {
		callArgs[i] = thunk.Params[i+1]
	}

	if _, isVoid := fn.Sig.RetType.(*irtypes.VoidType); isVoid {
		entry.NewCall(fn, callArgs...)
		entry.NewRet(nil)
	} else {
		result := entry.NewCall(fn, callArgs...)
		entry.NewRet(result)
	}

	c.thunks[name] = thunk
	return thunk
}

// --- is/as expressions ---

// genIsExpr generates code for `expr is Pattern`.
func (c *Compiler) genIsExpr(e *ast.IsExpr) value.Value {
	switch p := e.Pattern.(type) {
	case *ast.IdentIsPattern:
		return c.genIsIdentPattern(e.Expr, p)
	case *ast.DestructureIsPattern:
		return c.genIsDestructurePattern(e.Expr, p)
	default:
		panic(fmt.Sprintf("codegen: unhandled is-pattern type %T", e.Pattern))
	}
}

func (c *Compiler) genIsIdentPattern(expr ast.Expr, p *ast.IdentIsPattern) value.Value {
	// Optional: x is present / x is absent
	if p.Name == "present" || p.Name == "absent" {
		optVal := c.genExpr(expr)
		flag := c.block.NewExtractValue(optVal, 0) // i1 flag field

		// B0288: Drop temporary optional data for call-like expressions.
		// When a method/function call returns T? with droppable inner T
		// (enum, string, vector), the data portion leaks unless dropped.
		// Only drop for expressions that produce fresh temporaries (calls,
		// optional chains). Field/index/ident expressions share ownership
		// with the parent variable — dropping would double-free.
		switch expr.(type) {
		case *ast.CallExpr, *ast.OptionalChainExpr, *ast.ErrorHandlerExpr:
			c.dropTempOptionalInner(expr, optVal, flag)
		}

		if p.Name == "absent" {
			return c.block.NewXor(flag, constant.NewInt(irtypes.I1, 1))
		}
		return flag
	}

	// Check if the subject is an optional type — unwrap before checking inner type
	exprType := c.info.Types[expr]
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	if opt, ok := exprType.(*types.Optional); ok {
		// Generic pattern with resolved type
		if resolved := c.info.IsPatternTypes[p]; resolved != nil {
			return c.genIsOptionalTypeResolved(expr, resolved, opt)
		}
		return c.genIsOptionalType(expr, p.Name, opt)
	}

	// Check if the subject is an enum type — use tag comparison
	if enumLayout := c.lookupEnumLayout(exprType); enumLayout != nil {
		return c.genIsEnumVariant(expr, p.Name, enumLayout)
	}

	// Generic pattern with resolved type — use type ID directly
	if resolved := c.info.IsPatternTypes[p]; resolved != nil {
		return c.genIsResolvedType(expr, resolved)
	}

	// Named type check via RTTI
	return c.genIsNamedType(expr, p.Name)
}

// dropTempOptionalInner drops the inner value of a temporary optional struct.
// B0288: When `expr is present/absent` evaluates a non-ident expression (e.g., method call)
// returning T?, the data portion of the {i1, T} struct is abandoned. If T is a droppable
// type (enum with heap data, string, vector), the inner value must be conditionally dropped
// (only when the flag indicates presence) to prevent leaks.
func (c *Compiler) dropTempOptionalInner(expr ast.Expr, optVal value.Value, flag value.Value) {
	if c.block == nil || c.block.Term != nil {
		return
	}
	exprType := c.info.Types[expr]
	if exprType == nil {
		return
	}
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	opt, ok := exprType.(*types.Optional)
	if !ok {
		return
	}
	elem := opt.Elem()
	if c.typeSubst != nil {
		elem = types.Substitute(elem, c.typeSubst)
	}

	// Determine what kind of drop is needed.
	innerEnum := extractEnum(elem)
	innerNamed := extractNamed(elem)

	if innerEnum != nil {
		// Enum with droppable variants — call the synthesized enum drop function.
		if !c.enumInstanceHasDrop(elem, innerEnum) {
			return
		}
		enumName := innerEnum.Obj().Name()
		if inst, ok := elem.(*types.Instance); ok {
			enumName = monoName(inst)
		} else if c.typeSubst != nil {
			resolved := types.Substitute(elem, c.typeSubst)
			if inst, ok := resolved.(*types.Instance); ok {
				enumName = monoName(inst)
			}
		}
		mangledName := mangleMethodName(enumName, "drop", false)
		dropFunc, exists := c.funcs[mangledName]
		if !exists || dropFunc == nil {
			return
		}

		innerVal := c.block.NewExtractValue(optVal, 1)
		alloca := c.createEntryAlloca(innerVal.Type())
		c.block.NewStore(innerVal, alloca)

		dropBlock := c.newBlock("is.temp.drop")
		skipBlock := c.newBlock("is.temp.skip")
		c.block.NewCondBr(flag, dropBlock, skipBlock)

		c.block = dropBlock
		ptr := c.block.NewBitCast(alloca, irtypes.I8Ptr)
		c.block.NewCall(dropFunc, ptr)
		c.block.NewBr(skipBlock)

		c.block = skipBlock
	} else if innerNamed == types.TypString {
		// String — call promise_string_drop.
		innerVal := c.block.NewExtractValue(optVal, 1)
		dropBlock := c.newBlock("is.temp.drop")
		skipBlock := c.newBlock("is.temp.skip")
		c.block.NewCondBr(flag, dropBlock, skipBlock)

		c.block = dropBlock
		c.block.NewCall(c.funcs["promise_string_drop"], innerVal)
		c.block.NewBr(skipBlock)

		c.block = skipBlock
	} else if innerNamed != nil {
		// Vector or channel — call their drop.
		var dropFunc *ir.Func
		var isContainer bool
		if _, isVec := types.AsVector(elem); isVec || innerNamed == types.TypVector {
			dropFunc = c.funcs["Vector.drop"]
			isContainer = true
		} else if chElem, isCh := types.AsChannel(elem); isCh || innerNamed == types.TypChannel {
			// T0663: per-element-type drop walks any un-received buffered items.
			if chElem != nil {
				dropFunc = c.getOrCreateChannelDrop(chElem)
				isContainer = true
			}
		} else if innerNamed.HasDrop() || innerNamed.NeedsSynthDrop() {
			// B0288: User type with explicit drop() or synthesized drop.
			ownerName := innerNamed.Obj().Name()
			resolvedElem := elem
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(elem, c.typeSubst)
			}
			if inst, ok := resolvedElem.(*types.Instance); ok {
				ownerName = monoName(inst)
			} else if innerNamed.HasDrop() && !innerNamed.NeedsSynthDrop() {
				ownerName = c.resolveDropOwner(innerNamed)
			}
			mangledName := mangleMethodName(ownerName, "drop", false)
			dropFunc = c.funcs[mangledName]
		}
		if dropFunc != nil {
			innerVal := c.block.NewExtractValue(optVal, 1)
			dropBlock := c.newBlock("is.temp.drop")
			skipBlock := c.newBlock("is.temp.skip")
			c.block.NewCondBr(flag, dropBlock, skipBlock)

			c.block = dropBlock
			if isContainer {
				c.block.NewCall(dropFunc, innerVal)
			} else {
				// User type: inner is value struct {vtable, instance} — extract instance ptr.
				instance := c.extractInstancePtr(innerVal)
				nullCheck := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(irtypes.I8Ptr))
				execBlock := c.newBlock("is.temp.exec")
				nullSkip := c.newBlock("is.temp.null")
				c.block.NewCondBr(nullCheck, nullSkip, execBlock)

				c.block = execBlock
				c.block.NewCall(dropFunc, instance)
				// B0159: Free the instance struct after drop() completes.
				if !innerNamed.NeedsSynthDrop() {
					c.block.NewCall(c.palFree, instance)
				}
				c.block.NewBr(nullSkip)

				c.block = nullSkip
			}
			c.block.NewBr(skipBlock)

			c.block = skipBlock
		}
	}
}

// genIsOptionalType generates code for `optExpr is TypeName` where optExpr has type T?.
// For primitive/string optionals (no RTTI), this is equivalent to a presence check.
// For user types with RTTI, this checks presence AND performs RTTI on the unwrapped value.
func (c *Compiler) genIsOptionalType(expr ast.Expr, typeName string, opt *types.Optional) value.Value {
	optVal := c.genExpr(expr)
	flag := c.block.NewExtractValue(optVal, 0) // i1 presence flag

	elem := opt.Elem()
	// For enums, primitives, and strings there is no subtyping,
	// so T? is T is equivalent to T? is present — just check the flag.
	if c.lookupEnumLayout(elem) != nil {
		return flag
	}
	named := extractNamed(elem)
	if named != nil && (isPrimitiveScalar(named) || named == types.TypString) {
		return flag
	}

	// User type with RTTI: check presence AND type via RTTI on the unwrapped value.
	// We need branching to avoid accessing RTTI on a none value.
	targetNamed := c.lookupNamedType(typeName)
	if targetNamed == nil {
		panic(fmt.Sprintf("codegen: undefined type %s in is-expression", typeName))
	}
	targetID := c.assignTypeID(targetNamed)

	fn := c.block.Parent
	thenBlock := fn.NewBlock("")
	elseBlock := fn.NewBlock("")
	mergeBlock := fn.NewBlock("")

	c.block.NewCondBr(flag, thenBlock, elseBlock)

	// Then: extract inner value and do RTTI check
	c.block = thenBlock
	inner := c.block.NewExtractValue(optVal, 1)
	instance := c.instancePtrForRTTI(inner, elem)
	variantPtr := c.loadVariantPtr(instance)
	result := c.block.NewCall(c.funcs["promise_type_is"],
		variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
	rttiResult := c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))
	c.block.NewBr(mergeBlock)
	thenExit := c.block

	// Else: not present → false
	c.block = elseBlock
	c.block.NewBr(mergeBlock)
	elseExit := c.block

	// Merge
	c.block = mergeBlock
	phi := c.block.NewPhi(ir.NewIncoming(rttiResult, thenExit), ir.NewIncoming(constant.NewInt(irtypes.I1, 0), elseExit))
	return phi
}

func (c *Compiler) genIsEnumVariant(expr ast.Expr, variantName string, layout *TypeDeclLayout) value.Value {
	if _, ok := layout.VariantTag[variantName]; !ok {
		panic(fmt.Sprintf("codegen: unknown enum variant %s", variantName))
	}
	subject := c.genExpr(expr)
	// Extract tag
	var tag value.Value
	if layout.MaxVariantDataSize == 0 {
		tag = subject // fieldless enum: value IS the tag
	} else {
		tag = c.block.NewExtractValue(subject, 0)
	}
	expectedTag := constant.NewInt(irtypes.I32, int64(layout.VariantTag[variantName]))
	return c.block.NewICmp(enum.IPredEQ, tag, expectedTag)
}

func (c *Compiler) genIsNamedType(expr ast.Expr, typeName string) value.Value {
	subject := c.genExpr(expr)

	// Look up target type and its type ID
	targetNamed := c.lookupNamedType(typeName)
	if targetNamed == nil {
		panic(fmt.Sprintf("codegen: undefined type %s in is-expression", typeName))
	}
	targetID := c.assignTypeID(targetNamed)

	// Extract instance pointer for RTTI query.
	// For value types, use the compile-time-known RTTI global (no field in value struct).
	exprType := c.info.Types[expr]
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	var instance value.Value
	if _, isThis := expr.(*ast.ThisExpr); isThis {
		instance = c.extractInstancePtrForThis(subject)
	} else {
		instance = c.instancePtrForRTTI(subject, exprType)
	}
	variantPtr := c.loadVariantPtr(instance)

	// Call promise_type_is(variant_ptr, expected_id) and convert i32 result to i1
	result := c.block.NewCall(c.funcs["promise_type_is"],
		variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
	return c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))
}

// genIsResolvedType generates an RTTI type check for a sema-resolved type
// (supports both *types.Named and *types.Instance from generic is-patterns).
func (c *Compiler) genIsResolvedType(expr ast.Expr, resolved types.Type) value.Value {
	subject := c.genExpr(expr)

	targetID, ok := c.resolveTypeID(resolved)
	if !ok {
		panic(fmt.Sprintf("codegen: cannot resolve type ID for %s in is-expression", resolved))
	}

	exprType := c.info.Types[expr]
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	var instance value.Value
	if _, isThis := expr.(*ast.ThisExpr); isThis {
		instance = c.extractInstancePtrForThis(subject)
	} else {
		instance = c.instancePtrForRTTI(subject, exprType)
	}
	variantPtr := c.loadVariantPtr(instance)

	result := c.block.NewCall(c.funcs["promise_type_is"],
		variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
	return c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))
}

// genIsOptionalTypeResolved generates code for `optExpr is Type[args]` where optExpr
// has type T? and the target type is a sema-resolved generic instance.
func (c *Compiler) genIsOptionalTypeResolved(expr ast.Expr, resolved types.Type, opt *types.Optional) value.Value {
	optVal := c.genExpr(expr)
	flag := c.block.NewExtractValue(optVal, 0)

	elem := opt.Elem()
	if c.lookupEnumLayout(elem) != nil {
		return flag
	}
	named := extractNamed(elem)
	if named != nil && (isPrimitiveScalar(named) || named == types.TypString) {
		return flag
	}

	targetID, ok := c.resolveTypeID(resolved)
	if !ok {
		panic(fmt.Sprintf("codegen: cannot resolve type ID for %s in is-expression", resolved))
	}

	fn := c.block.Parent
	thenBlock := fn.NewBlock("")
	elseBlock := fn.NewBlock("")
	mergeBlock := fn.NewBlock("")

	c.block.NewCondBr(flag, thenBlock, elseBlock)

	c.block = thenBlock
	inner := c.block.NewExtractValue(optVal, 1)
	instance := c.instancePtrForRTTI(inner, elem)
	variantPtr := c.loadVariantPtr(instance)
	rttiResult := c.block.NewCall(c.funcs["promise_type_is"],
		variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
	rttiCheck := c.block.NewICmp(enum.IPredNE, rttiResult, constant.NewInt(irtypes.I32, 0))
	c.block.NewBr(mergeBlock)
	thenExit := c.block

	c.block = elseBlock
	c.block.NewBr(mergeBlock)
	elseExit := c.block

	c.block = mergeBlock
	phi := c.block.NewPhi(ir.NewIncoming(rttiCheck, thenExit), ir.NewIncoming(constant.NewInt(irtypes.I1, 0), elseExit))
	return phi
}

// genIsDestructurePattern generates the bool check for a destructure is-pattern
// (e.g., `x is Circle(r)`). When used inside an if-condition, the actual field
// binding is handled by genIfDestructureIsStmt. Outside if-conditions, this just
// returns the type/variant check result without binding any variables.
func (c *Compiler) genIsDestructurePattern(expr ast.Expr, p *ast.DestructureIsPattern) value.Value {
	exprType := c.info.Types[expr]
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}

	// Enum variant check
	if enumLayout := c.lookupEnumLayout(exprType); enumLayout != nil {
		if _, ok := enumLayout.VariantTag[p.TypeName]; ok {
			return c.genIsEnumVariant(expr, p.TypeName, enumLayout)
		}
	}

	// Generic type with resolved type — use type ID directly
	if resolved := c.info.IsPatternTypes[p]; resolved != nil {
		return c.genIsResolvedType(expr, resolved)
	}

	// Named type check via RTTI
	return c.genIsNamedType(expr, p.TypeName)
}

// extractInstancePtr extracts the i8* instance pointer (field 1) from a user type value struct.
func (c *Compiler) extractInstancePtr(val value.Value) value.Value {
	return c.block.NewExtractValue(val, 1)
}

// extractVtablePtr extracts the i8* vtable pointer (field 0) from a user type value struct.
func (c *Compiler) extractVtablePtr(val value.Value) value.Value {
	return c.block.NewExtractValue(val, 0)
}

// valueTypeReceiverPtr creates a temp alloca for a value type receiver and returns
// an i8* pointer to it. Methods on value types receive a pointer to the value struct.
func (c *Compiler) valueTypeReceiverPtr(val value.Value, typ types.Type) value.Value {
	layout := c.lookupTypeLayout(typ)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for value type receiver %s", typ))
	}
	tmp := c.createEntryAlloca(layout.Value.LLVMType)
	c.block.NewStore(val, tmp)
	return c.block.NewBitCast(tmp, irtypes.I8Ptr)
}

// extractInstancePtrForThis extracts the instance/RTTI pointer from a `this` value.
// For regular types, `this` (i8*) IS the instance pointer.
// For value types, the RTTI pointer is not stored in the value struct — use the
// compile-time-known RTTI global directly.
func (c *Compiler) extractInstancePtrForThis(thisVal value.Value) value.Value {
	if c.currentNamed != nil && c.currentNamed.IsValueType() {
		if rttiGlobal := c.lookupValueTypeRTTI(c.currentNamed); rttiGlobal != nil {
			return c.block.NewBitCast(rttiGlobal, irtypes.I8Ptr)
		}
	}
	return thisVal
}

// instancePtrForRTTI returns the instance pointer for RTTI queries (is-checks, casts).
// For regular types, field 1 of the value struct is the instance pointer.
// For value types, the RTTI pointer is not in the value struct — use the compile-time-known global.
func (c *Compiler) instancePtrForRTTI(val value.Value, typ types.Type) value.Value {
	named := extractNamed(typ)
	if named != nil && named.IsValueType() {
		if rttiGlobal := c.lookupValueTypeRTTI(typ); rttiGlobal != nil {
			return c.block.NewBitCast(rttiGlobal, irtypes.I8Ptr)
		}
	}
	return c.extractInstancePtr(val)
}

// loadVariantPtr loads the _variant pointer (RTTI info) from a user type instance.
// The instance must be an i8* pointer; the first field of any instance struct is the variant pointer.
func (c *Compiler) loadVariantPtr(subject value.Value) value.Value {
	variantPtrStruct := irtypes.NewStruct(irtypes.I8Ptr)
	typedPtr := c.block.NewBitCast(subject, irtypes.NewPointer(variantPtrStruct))
	variantFieldPtr := c.block.NewGetElementPtr(variantPtrStruct, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	return c.block.NewLoad(irtypes.I8Ptr, variantFieldPtr)
}

// genCastExpr generates code for `expr as Type` and `expr as! Type`.
func (c *Compiler) genCastExpr(e *ast.CastExpr) value.Value {
	// Optional unwrap: T? as! T → extract inner value, panic on none.
	if e.Force {
		srcType := c.info.Types[e.Expr]
		if opt, ok := srcType.(*types.Optional); ok {
			targetType := c.resolveTypeRefToType(e.Type)
			if targetType != nil && types.Identical(opt.Elem(), targetType) {
				return c.genOptionalForceUnwrap(e.Expr)
			}
		}
	}

	// Resolve the target Named type from the TypeRef
	targetRef, ok := e.Type.(*ast.NamedTypeRef)
	if !ok {
		panic(fmt.Sprintf("codegen: unsupported cast target type %T", e.Type))
	}
	targetNamed := c.lookupNamedType(targetRef.Name)
	if targetNamed == nil {
		panic(fmt.Sprintf("codegen: undefined type %s in cast", targetRef.Name))
	}

	subject := c.genExpr(e.Expr)

	// Primitive scalar casts (numeric, char, bool) — compile-time conversions, no RTTI needed
	srcType := c.info.Types[e.Expr]
	srcNamed := extractNamed(srcType)
	if srcNamed != nil && isPrimitiveScalar(srcNamed) && isPrimitiveScalar(targetNamed) {
		return c.emitScalarCast(subject, srcNamed, targetNamed)
	}

	targetID := c.assignTypeID(targetNamed)

	// Extract instance pointer for RTTI query.
	// For value types, use the compile-time-known RTTI global (no field in value struct).
	if c.typeSubst != nil {
		srcType = types.Substitute(srcType, c.typeSubst)
	}
	var instance value.Value
	if _, isThis := e.Expr.(*ast.ThisExpr); isThis {
		instance = c.extractInstancePtrForThis(subject)
	} else {
		instance = c.instancePtrForRTTI(subject, srcType)
	}
	variantPtr := c.loadVariantPtr(instance)

	result := c.block.NewCall(c.funcs["promise_type_is"],
		variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
	isMatch := c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))

	if e.Force {
		// as! — panic if no match, return the value struct directly
		okBlock := c.newBlock("cast.ok")
		panicBlock := c.newBlock("cast.panic")
		c.block.NewCondBr(isMatch, okBlock, panicBlock)

		c.block = panicBlock
		panicMsg := c.makeGlobalString("cast failed: as! type mismatch")
		c.block.NewCall(c.funcs["promise_panic"], panicMsg)
		c.emitPanicReturn()

		c.block = okBlock
		return subject // same value struct, type is verified
	}

	// as — wrap in Optional { i1, { i8*, i8* } }. User types use value struct representation.
	someBlock := c.newBlock("cast.some")
	noneBlock := c.newBlock("cast.none")
	mergeBlock := c.newBlock("cast.merge")
	c.block.NewCondBr(isMatch, someBlock, noneBlock)

	c.block = someBlock
	optType := irtypes.NewStruct(irtypes.I1, userValueType())
	someResult := c.wrapOptional(subject, optType)
	c.block.NewBr(mergeBlock)
	someEnd := c.block

	c.block = noneBlock
	noneResult := constant.NewZeroInitializer(optType)
	c.block.NewBr(mergeBlock)
	noneEnd := c.block

	c.block = mergeBlock
	phi := c.block.NewPhi(
		&ir.Incoming{X: someResult, Pred: someEnd},
		&ir.Incoming{X: noneResult, Pred: noneEnd},
	)
	return phi
}

// genOptionalHandlerExpr generates code for `optExpr ? { recovery }`.
// Checks the optional flag, runs the handler on none, extracts inner value on some.
func (c *Compiler) genOptionalHandlerExpr(e *ast.ErrorHandlerExpr) value.Value {
	optVal := c.genExpr(e.Expr)
	flag := c.block.NewExtractValue(optVal, 0)

	noneBlock := c.newBlock("opt.none")
	someBlock := c.newBlock("opt.some")
	mergeBlock := c.newBlock("opt.merge")
	c.block.NewCondBr(flag, someBlock, noneBlock)

	// None path: run handler body
	c.block = noneBlock
	handlerVal := c.genBlockValue(e.Body)
	handlerDiverged := c.block.Term != nil
	handlerEnd := c.block
	if !handlerDiverged {
		c.block.NewBr(mergeBlock)
	}

	// Some path: extract inner value
	c.block = someBlock
	okVal := c.block.NewExtractValue(optVal, 1)
	c.block.NewBr(mergeBlock)
	someEnd := c.block

	c.block = mergeBlock

	// If handler diverges, no phi needed - only the some path reaches merge
	if handlerDiverged {
		return okVal
	}

	// Both paths reach merge - phi merge the values
	if handlerVal != nil && okVal != nil {
		return c.block.NewPhi(
			&ir.Incoming{X: okVal, Pred: someEnd},
			&ir.Incoming{X: handlerVal, Pred: handlerEnd},
		)
	}
	return okVal
}

// genOptionalForceUnwrap generates code for T? → T, panicking on none.
// Used by `as!` on optionals and `x!` on optionals.
// T0111: When source is an identifier with a drop binding, clears the drop flag
// (ownership transfers to the unwrapped value). Field access dup is handled by
// the dupStringFieldAccess mechanism in genTypedVarDecl/genInferredVarDecl.
func (c *Compiler) genOptionalForceUnwrap(expr ast.Expr) value.Value {
	optVal := c.genExpr(expr)
	flag := c.block.NewExtractValue(optVal, 0)

	okBlock := c.newBlock("unwrap.ok")
	panicBlock := c.newBlock("unwrap.panic")
	c.block.NewCondBr(flag, okBlock, panicBlock)

	c.block = panicBlock
	panicMsg := c.makeGlobalString("unwrap failed: optional is none")
	c.block.NewCall(c.funcs["promise_panic"], panicMsg)
	c.emitPanicReturn()

	c.block = okBlock
	// B0190: If genFieldAccess (B0181) created a dup for the inner string,
	// return the dup directly instead of extractvalue. This preserves the
	// value.Value identity so claimStringTemp can match it in VarDecl.
	if c.optionalStringDup != nil {
		dup := c.optionalStringDup
		c.optionalStringDup = nil
		return dup
	}
	// T0397: Same shape for tuple dup — when genMethodIndex created a dup for
	// the inner Optional[Tuple], return it directly so the binding takes
	// ownership of the deep-cloned tuple instead of an aliased extractvalue.
	if c.optionalTupleDup != nil {
		dup := c.optionalTupleDup
		c.optionalTupleDup = nil
		return dup
	}
	// T0440: Same shape for heap-user-type dup — when genMethodIndex created a
	// dup for the inner Optional[heap-user-type], return it directly so the
	// binding takes ownership of the cloned instance instead of an aliased
	// extractvalue.
	if c.optionalHeapDup != nil {
		dup := c.optionalHeapDup
		c.optionalHeapDup = nil
		return dup
	}
	var result value.Value
	result = c.block.NewExtractValue(optVal, 1)

	// T0428 Case 3B: borrowed this.field! — dup the inner heap value so the new
	// variable gets an independent copy. The caller still owns the original (we
	// can't clear the present flag on a borrowed receiver), so both the caller's
	// synth drop and the new variable get independent copies to free.
	if member, ok := expr.(*ast.MemberExpr); ok {
		if _, isThis := member.Target.(*ast.ThisExpr); isThis && !c.thisRecvIsOwned {
			innerType := c.info.Types[expr]
			if c.typeSubst != nil {
				innerType = types.Substitute(innerType, c.typeSubst)
			}
			if opt, optOk := innerType.(*types.Optional); optOk {
				innerElem := opt.Elem()
				if c.typeSubst != nil {
					innerElem = types.Substitute(innerElem, c.typeSubst)
				}
				innerNamed := extractNamed(innerElem)
				if innerNamed != nil && !innerNamed.IsValueType() && !innerNamed.IsCopy() &&
					!isPrimitiveScalar(innerNamed) && innerNamed != types.TypString &&
					!types.IsVector(innerElem) && !types.IsChannel(innerElem) &&
					!innerNamed.IsStructural() && !isOpaqueContainerType(innerElem) {
					result = c.dupHeapValue(result, innerElem)
				}
			}
		}
	}

	// T0111: Do NOT clear the drop flag here. The optional still owns the inner
	// value and will free it at scope exit via its drop binding. For temporary
	// access (opt!.len), this is correct — the inner stays alive until scope exit.
	// For assignment (val = opt!), the assignment site neutralizes the optional
	// by setting its present flag to false (see genTypedVarDecl/genInferredVarDecl).

	// Track the unwrapped i8* as a statement temp when the source is NOT an
	// identifier (e.g., method call returning string? or T[]?). For ident
	// sources, the optional's own drop handles the inner. For non-ident sources
	// (call results), the optional? temporary has no scope drop → the extracted
	// pointer must be tracked and freed at statement end.
	// B0299: Skip when optionalFieldString is set — the field comes from a
	// droppable type whose drop handles the string's lifetime. Tracking it
	// as a temp would cause double-free (statement-end + owner drop).
	// T0354: Same for optionalFieldVector — vector field on droppable type.
	// T0350: Type-aware tracking — strings via promise_string_drop, vectors via
	// Vector.drop with element type so heap elements (e.g., string[]) are dropped.
	if _, isIdent := expr.(*ast.IdentExpr); !isIdent && c.tempTrackingEnabled && !c.optionalFieldString && !c.optionalFieldVector {
		if result.Type().Equal(irtypes.I8Ptr) {
			innerType := c.info.Types[expr]
			if opt, ok := innerType.(*types.Optional); ok {
				innerType = opt.Elem()
			}
			if c.typeSubst != nil && innerType != nil {
				innerType = types.Substitute(innerType, c.typeSubst)
			}
			named := extractNamed(innerType)
			if named == types.TypString {
				c.trackStringTemp(result)
			} else if named == types.TypVector {
				if elemType, ok := types.AsVector(innerType); ok {
					c.trackVectorTempWithElemType(result, elemType)
				} else {
					c.trackVectorTemp(result)
				}
			} else if arcElem, isArc := types.AsArc(innerType); isArc {
				c.trackTempWithDrop(result, c.getOrCreateArcDrop(arcElem))
			} else if weakElem, isWeak := types.AsWeak(innerType); isWeak {
				c.trackTempWithDrop(result, c.getOrCreateWeakDrop(weakElem))
			} else if mutexElem, isMutex := types.AsMutex(innerType); isMutex {
				// T0654: Optional<Mutex[T]> from a non-binding-site unwrap leaked
				// because the inner i8* fell through with no tracking. The
				// binding-site claim (stmt.go) is a no-op when no temp exists.
				c.trackTempWithDrop(result, c.getOrCreateMutexDrop(mutexElem))
			} else if taskElem, isTask := types.AsTask(innerType); isTask {
				c.trackTempWithDrop(result, c.getOrCreateTaskDrop(taskElem))
			} else if _, isMG := types.AsMutexGuard(innerType); isMG {
				if dropFn, ok := c.funcs["MutexGuard.drop"]; ok {
					c.trackTempWithDrop(result, dropFn)
				}
			} else if chElem, isCh := types.AsChannel(innerType); isCh {
				c.trackChannelTempWithElemType(result, chElem)
			}
		}
	}

	return result
}

// neutralizeForceUnwrapSource sets the present flag to false in the source
// optional's alloca when a force-unwrap result is consumed by an assignment.
// T0111: Prevents double-free when both the new variable and the source optional
// would otherwise try to drop the same inner value. Called from many assignment
// and arg-passing sites in expr.go (call-arg paths) and stmt.go (var decls,
// destructure, assign, for/yield).
func (c *Compiler) neutralizeForceUnwrapSource(expr ast.Expr) {
	// T0577: peel ParenExpr wrappers so `(opt!)` neutralizes like `opt!`.
	// genExpr already sees through ParenExpr; this peel fixes the AST-shape
	// dispatch below. A second peel inside the T0436 inner loop handles the
	// mirror case `(opt)!` (parens around the source, not the unwrap).
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	// Extract the source identifier from opt!, opt as! T, or opt? _ { fallback }.
	var inner ast.Expr
	switch e := expr.(type) {
	case *ast.OptionalUnwrapExpr:
		inner = e.Expr
	case *ast.CastExpr:
		// B0293: as! on optionals also force-unwraps — must neutralize source.
		// Only applies to optional→concrete casts, not inheritance downcasts.
		if e.Force {
			if _, isOpt := c.info.Types[e.Expr].(*types.Optional); isOpt {
				inner = e.Expr
			}
		}
	case *ast.ErrorHandlerExpr:
		// B0293: optional handler (p? _ { fallback }) also extracts inner value.
		if _, isOpt := c.info.Types[e.Expr].(*types.Optional); isOpt {
			inner = e.Expr
		}
	}
	if inner == nil {
		return
	}
	// T0436: traverse nested force-unwraps (e.g., `b := h.data!!`).
	// Each nested OptionalUnwrapExpr exposes one Optional level; we only need to
	// clear the OUTERMOST present flag (on the original source) — synth drop will
	// then skip the field entirely and not descend into the inner Optional.
	// T0577: also peel ParenExpr inside the chain so `(opt)!`, `(opt) as! T`,
	// `(opt)? _ { ... }`, and combinations like `((opt))!` all reach IdentExpr.
	for {
		if uw, ok := inner.(*ast.OptionalUnwrapExpr); ok {
			inner = uw.Expr
			continue
		}
		if p, ok := inner.(*ast.ParenExpr); ok {
			inner = p.Expr
			continue
		}
		break
	}
	switch src := inner.(type) {
	case *ast.IdentExpr:
		alloca, ok := c.locals[src.Name]
		if !ok {
			return
		}
		optType, ok := alloca.ElemType.(*irtypes.StructType)
		if !ok || len(optType.Fields) < 2 {
			return
		}
		// Set present flag (field 0) to false — optional drop will skip inner free.
		flagPtr := c.block.NewGetElementPtr(optType, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), flagPtr)
	case *ast.MemberExpr:
		// T0392: Force-unwrap of an optional field on an owned variable
		// (`v.field!`). The owner's drop will visit this field via
		// emitOptionalFieldDrop and double-free the inner value unless we
		// clear the present flag in the owner's instance memory.
		c.neutralizeMemberOptionalField(src)
	}
}

// neutralizeMemberOptionalField clears the present flag of an Optional[heap-user-type]
// field on an owned variable (T0392). Handles:
//   - Simple `ident.field!` (original case)
//   - T0428 Case 1: `ident.field!!` (T?? field — look through inner Optional for guard checks)
//   - T0428 Case 2: `outer.inner.field!` (chained MemberExpr — walk chain)
//   - T0428 Case 3A: `this.field!` inside ~this method (owned receiver)
//
// String/Vector/Channel/Arc/Weak optional fields are skipped because genFieldAccess
// already dups them — clearing the flag would leak the original.
// T0428 Case 3B (borrowed this.field!): handled in genOptionalForceUnwrap via dup.
func (c *Compiler) neutralizeMemberOptionalField(m *ast.MemberExpr) {
	// T0428 Case 2: Walk the MemberExpr chain to find the root variable.
	// chain[i] = step i from root toward leaf. chain[0].Target is the root.
	// chain[last] = m (the final Optional field access).
	chain := []*ast.MemberExpr{m}
	cur := ast.Expr(m.Target)
	for {
		if me, ok := cur.(*ast.MemberExpr); ok {
			chain = append([]*ast.MemberExpr{me}, chain...)
			cur = me.Target
		} else {
			break
		}
	}

	// Resolve the root alloca and initial owner named type.
	var ownerAlloca *ir.InstAlloca
	var ownerType types.Type // used for layout lookup (ident-rooted chains)
	var ownerNamed *types.Named
	var rootIsThis bool

	switch root := cur.(type) {
	case *ast.IdentExpr:
		a, ok := c.locals[root.Name]
		if !ok {
			return // not an owned local
		}
		ownerAlloca = a
		ownerType = c.info.Types[root]
		if c.typeSubst != nil {
			ownerType = types.Substitute(ownerType, c.typeSubst)
		}
		// Don't neutralize through a borrow.
		if _, isShared := ownerType.(*types.SharedRef); isShared {
			return
		}
		if _, isMut := ownerType.(*types.MutRef); isMut {
			return
		}
		ownerNamed = extractNamed(ownerType)
		if ownerNamed == nil {
			return
		}
		// Only neutralize when the owner's drop would actually visit this field.
		if ownerNamed.IsValueType() || ownerNamed.IsStructural() {
			return
		}
		if !ownerNamed.HasDrop() && !ownerNamed.NeedsSynthDrop() {
			if inst, ok := ownerType.(*types.Instance); ok {
				if !monoInstNeedsSynthDrop(inst) {
					return
				}
			} else {
				return
			}
		}
	case *ast.ThisExpr:
		// T0428 Case 3A: owned ~this — 'this' alloca holds a raw i8* instance ptr.
		// T0428 Case 3B: borrowed this — handled via dup in genOptionalForceUnwrap; skip here.
		if !c.thisRecvIsOwned {
			return
		}
		a, ok := c.locals["this"]
		if !ok {
			return
		}
		ownerAlloca = a
		ownerNamed = c.currentNamed
		if ownerNamed == nil {
			return
		}
		if ownerNamed.IsValueType() || ownerNamed.IsStructural() {
			return
		}
		if !ownerNamed.HasDrop() && !ownerNamed.NeedsSynthDrop() {
			return
		}
		ownerType = ownerNamed
		rootIsThis = true
	default:
		return
	}

	// Load the root instance pointer.
	ownerVal := c.block.NewLoad(ownerAlloca.ElemType, ownerAlloca)
	var rootInstance value.Value
	if rootIsThis {
		// ~this: the alloca holds an i8* instance pointer directly.
		rootInstance = ownerVal
	} else {
		rootInstance = c.extractInstancePtr(ownerVal)
	}

	// Walk the chain. For each step, GEP through the instance to the field.
	// For intermediate steps: load the value struct, extract instance ptr, advance named type.
	// For the final step: validate Optional field type and clear the present flag.
	curInstance := rootInstance
	curNamed := ownerNamed
	curType := ownerType

	for i, step := range chain {
		stepLayout := c.lookupTypeLayout(curType)
		if stepLayout == nil || stepLayout.IsValueType {
			return
		}
		stepFieldIdx, ok := stepLayout.InstanceFieldIndex[step.Field]
		if !ok {
			return
		}
		typedPtr := c.block.NewBitCast(curInstance, stepLayout.InstancePtrType)
		fieldPtr := c.block.NewGetElementPtr(stepLayout.Instance.LLVMType, typedPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(stepFieldIdx)))
		fieldLLVMType := stepLayout.Instance.Fields[stepFieldIdx].LLVMType

		if i < len(chain)-1 {
			// Intermediate step: load the value struct and extract instance ptr.
			fieldVal := c.block.NewLoad(fieldLLVMType, fieldPtr)
			fieldSt, ok2 := fieldLLVMType.(*irtypes.StructType)
			if !ok2 || len(fieldSt.Fields) < 2 {
				return
			}
			curInstance = c.block.NewExtractValue(fieldVal, 1)
			// Advance named/type for next step.
			stepField := curNamed.LookupField(step.Field)
			if stepField == nil {
				return
			}
			nextType := stepField.Type()
			if c.typeSubst != nil {
				nextType = types.Substitute(nextType, c.typeSubst)
			}
			curNamed = extractNamed(nextType)
			if curNamed == nil {
				return
			}
			curType = nextType
			continue
		}

		// Final step: validate the Optional field type and clear the present flag.
		// Look up the field from curNamed to get its declared type.
		stepField := curNamed.LookupField(step.Field)
		if stepField == nil {
			return
		}
		fType := stepField.Type()
		if c.typeSubst != nil {
			fType = types.Substitute(fType, c.typeSubst)
		}
		opt, isOpt := fType.(*types.Optional)
		if !isOpt {
			return
		}
		// T0428 Case 1: For T?? fields, opt.Elem() is itself Optional[T]. Look through
		// to find the deep inner named type for guard checks, but still neutralize the
		// outermost Optional's present flag (field 0 of the field struct).
		elem := opt.Elem()
		innerElem := elem
		if innerOpt, ok2 := elem.(*types.Optional); ok2 {
			innerElem = innerOpt.Elem()
		}
		innerNamed := extractNamed(innerElem)
		// Skip for inner types where genFieldAccess already dups.
		if innerNamed == types.TypString || types.IsVector(innerElem) || types.IsChannel(innerElem) ||
			types.IsArc(innerElem) || types.IsWeak(innerElem) {
			return
		}
		if innerNamed == nil || innerNamed.IsValueType() || innerNamed.IsCopy() ||
			isPrimitiveScalar(innerNamed) || innerNamed.IsStructural() ||
			isOpaqueContainerType(innerElem) {
			return
		}

		// GEP to the Optional present flag (field 0) and clear it.
		optStruct, ok2 := fieldLLVMType.(*irtypes.StructType)
		if !ok2 || len(optStruct.Fields) < 2 {
			return
		}
		flagPtr := c.block.NewGetElementPtr(optStruct, fieldPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), flagPtr)
	}
}

// emitScalarCast emits LLVM IR for a primitive scalar type conversion.
// Handles int↔int (trunc/sext/zext), float↔float (fptrunc/fpext),
// int→float (sitofp/uitofp), float→int (fptosi/fptoui),
// char↔int (trunc/zext — char is i32 codepoint),
// bool→int/char (zext), int/char→bool (icmp ne 0), float→bool (fcmp one 0.0),
// bool→float (uitofp).
func (c *Compiler) emitScalarCast(val value.Value, src, dst *types.Named) value.Value {
	srcLLVM := llvmNamedType(src)
	dstLLVM := llvmNamedType(dst)

	srcInt, srcIsInt := srcLLVM.(*irtypes.IntType)
	dstInt, dstIsInt := dstLLVM.(*irtypes.IntType)
	_, srcIsFloat := srcLLVM.(*irtypes.FloatType)
	dstFloat, dstIsFloat := dstLLVM.(*irtypes.FloatType)

	dstIsBool := dst == types.TypBool

	switch {
	case srcIsInt && dstIsInt:
		if srcInt.BitSize == dstInt.BitSize {
			return val // same width: no-op (e.g., int ↔ uint, char ↔ i32)
		} else if dstIsBool {
			// int/char → bool: non-zero = true (icmp ne, not trunc)
			zero := constant.NewInt(srcInt, 0)
			return c.block.NewICmp(enum.IPredNE, val, zero)
		} else if srcInt.BitSize > dstInt.BitSize {
			return c.block.NewTrunc(val, dstInt)
		} else if isSignedType(src) {
			return c.block.NewSExt(val, dstInt)
		} else {
			return c.block.NewZExt(val, dstInt)
		}
	case srcIsFloat && dstIsFloat:
		srcFloat := srcLLVM.(*irtypes.FloatType)
		if srcFloat == dstFloat {
			return val
		} else if srcFloat == irtypes.Float {
			return c.block.NewFPExt(val, dstFloat)
		}
		return c.block.NewFPTrunc(val, dstFloat)
	case srcIsInt && dstIsFloat:
		if isSignedType(src) {
			return c.block.NewSIToFP(val, dstFloat)
		}
		return c.block.NewUIToFP(val, dstFloat)
	case srcIsFloat && dstIsInt:
		if dstIsBool {
			// float → bool: non-zero = true (une handles NaN as truthy)
			zero := constant.NewFloat(srcLLVM.(*irtypes.FloatType), 0.0)
			return c.block.NewFCmp(enum.FPredUNE, val, zero)
		}
		if isSignedType(dst) {
			return c.block.NewFPToSI(val, dstInt)
		}
		return c.block.NewFPToUI(val, dstInt)
	default:
		panic(fmt.Sprintf("codegen: unsupported scalar cast %s → %s", src, dst))
	}
}

// --- Go expression (concurrency) ---

// genGoExpr generates code for a `go expr` expression.
// It creates an LLVM coroutine, wraps it in a G, and enqueues it on the M:N scheduler.
func (c *Compiler) genGoExpr(e *ast.GoExpr) value.Value {
	if e.Expr != nil {
		callExpr, ok := e.Expr.(*ast.CallExpr)
		if !ok {
			panic(fmt.Sprintf("codegen: go expression with non-call expr %T not supported", e.Expr))
		}
		return c.genGoCallExpr(callExpr)
	}
	// go { block } form
	return c.genGoBlock(e.Block)
}

// genGoCallExpr handles `go func(args...)` — the common case.
// For non-IdentExpr callees (method calls, module calls, etc.), delegates to
// genGoCallExprViaBlock which uses the full codegen context inside the coroutine body.
func (c *Compiler) genGoCallExpr(callExpr *ast.CallExpr) value.Value {
	// Complex callees (method calls, module calls, generic calls, etc.)
	// need the full codegen context — use block-style coroutine (B0113).
	if _, ok := callExpr.Callee.(*ast.IdentExpr); !ok {
		return c.genGoCallExprViaBlock(callExpr)
	}

	// 1. Resolve result type T from sema
	callResultType := c.info.Types[callExpr]
	isVoid := (callResultType == nil || callResultType == types.TypVoid)
	var resultLLVM irtypes.Type = irtypes.Void
	if !isVoid {
		resultLLVM = c.resolveType(callResultType)
	}

	// 2. Evaluate arguments in caller scope
	var argVals []value.Value
	var argLLVMTypes []irtypes.Type
	var argTypes []types.Type
	for _, arg := range callExpr.Args {
		v := c.genCallArgExpr(arg.Value)
		argVals = append(argVals, v)
		argLLVMTypes = append(argLLVMTypes, v.Type())
		argTypes = append(argTypes, c.info.Types[arg.Value])
	}

	// B0163: Increment refcount for channel arguments passed to go calls.
	chanTypeDC := channelStructType()
	for i, arg := range callExpr.Args {
		if ident, ok := arg.Value.(*ast.IdentExpr); ok {
			if binding, ok := c.dropBindings[ident.Name]; ok {
				if _, isCh := types.AsChannel(binding.valType); isCh || binding.named == types.TypChannel {
					chPtr := c.block.NewBitCast(argVals[i], irtypes.NewPointer(chanTypeDC))
					rcField := c.block.NewGetElementPtr(chanTypeDC, chPtr,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRefcount)))
					c.emitAtomicAdd(c.block, rcField, constant.NewInt(irtypes.I64, 1), irtypes.I64)
				}
			}
		}
	}

	// 3. Resolve the target function
	targetFn, ext := c.resolveGoTarget(callExpr)

	// If target is an extern, generate a wrapper to handle sret/ABI coercion.
	// Extern functions use void return + sret pointer for struct returns, which
	// is incompatible with the coroutine body's direct call + store pattern.
	if ext != nil {
		targetFn = c.genGoExternWrapper(ext, argLLVMTypes, argTypes, resultLLVM, isVoid)
	}

	// 4. Create coroutine wrapper function
	coroName := fmt.Sprintf(".goroutine.%d", c.goCounter)
	c.goCounter++

	var coroParams []*ir.Param
	for i := range argVals {
		coroParams = append(coroParams, ir.NewParam(fmt.Sprintf("arg.%d", i), argLLVMTypes[i]))
	}
	coroFn := c.module.NewFunc(coroName, irtypes.I8Ptr, coroParams...)
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, rawFuncAttr("presplitcoroutine"))

	// 5. Build coroutine body
	entry := coroFn.NewBlock(".entry")

	// Coroutine preamble
	coroId := entry.NewCall(c.coroId,
		constant.NewInt(irtypes.I32, 0),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr))

	need := entry.NewCall(c.coroAlloc, coroId)
	allocBlk := coroFn.NewBlock("coro.alloc")
	startBlk := coroFn.NewBlock("coro.start")
	entry.NewCondBr(need, allocBlk, startBlk)

	coroSizeVal := allocBlk.NewCall(c.coroSize)
	var coroSizeArg value.Value = coroSizeVal
	if c.isWasm {
		coroSizeArg = allocBlk.NewZExt(coroSizeVal, irtypes.I64)
	}
	mem := allocBlk.NewCall(c.palAlloc, coroSizeArg)
	allocBlk.NewBr(startBlk)

	phiMem := startBlk.NewPhi(
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), entry),
		ir.NewIncoming(mem, allocBlk))
	hdl := startBlk.NewCall(c.coroBegin, coroId, phiMem)

	// Initial suspend
	initResult := startBlk.NewCall(c.coroSuspend, constant.None, constant.False)

	suspendBlk := coroFn.NewBlock("coro.suspend")
	bodyBlk := coroFn.NewBlock("body")
	cleanupBlk := coroFn.NewBlock("cleanup")

	startBlk.NewSwitch(initResult, suspendBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), bodyBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	// Suspend: ramp returns handle
	suspendBlk.NewRet(hdl)

	// Body: call target function with args (preserved in coro frame)
	var callArgs []value.Value
	for i := range coroParams {
		callArgs = append(callArgs, coroFn.Params[i])
	}

	// T0147: Create panic exit block for go-call coroutine.
	// Transfers panic state from TLS to G struct, clears TLS, branches to final suspend.
	goPanicExitDC := coroFn.NewBlock("go.panic_exit")

	if !isVoid {
		result := bodyBlk.NewCall(targetFn, callArgs...)

		// T0147: Check panic flag after call — skip result store if panicked.
		dcFlag := bodyBlk.NewLoad(irtypes.I8, c.panicFlagGlobal)
		dcIsPanic := bodyBlk.NewICmp(enum.IPredNE, dcFlag, constant.NewInt(irtypes.I8, 0))
		dcOkBlk := coroFn.NewBlock("go.call_ok")
		bodyBlk.NewCondBr(dcIsPanic, goPanicExitDC, dcOkBlk)
		bodyBlk = dcOkBlk

		// Store result via G.result_ptr (set by caller before enqueue).
		// For fire-and-forget non-void, result_ptr is null — skip store (B0109).
		gTy := goroutineStructType()
		currentG := bodyBlk.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		gPtr := bodyBlk.NewBitCast(currentG, irtypes.NewPointer(gTy))
		rpField := bodyBlk.NewGetElementPtr(gTy, gPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
		rpVal := bodyBlk.NewLoad(irtypes.I8Ptr, rpField)
		rpNotNull := bodyBlk.NewICmp(enum.IPredNE, rpVal, constant.NewNull(irtypes.I8Ptr))
		storeResultBlk := coroFn.NewBlock("store_result")
		afterStoreBlk := coroFn.NewBlock("after_store")
		bodyBlk.NewCondBr(rpNotNull, storeResultBlk, afterStoreBlk)

		typedRP := storeResultBlk.NewBitCast(rpVal, irtypes.NewPointer(resultLLVM))
		storeResultBlk.NewStore(result, typedRP)
		storeResultBlk.NewBr(afterStoreBlk)

		bodyBlk = afterStoreBlk
	} else {
		bodyBlk.NewCall(targetFn, callArgs...)

		// T0147: Check panic flag after call.
		dcFlag := bodyBlk.NewLoad(irtypes.I8, c.panicFlagGlobal)
		dcIsPanic := bodyBlk.NewICmp(enum.IPredNE, dcFlag, constant.NewInt(irtypes.I8, 0))
		dcOkBlk := coroFn.NewBlock("go.call_ok")
		bodyBlk.NewCondBr(dcIsPanic, goPanicExitDC, dcOkBlk)
		bodyBlk = dcOkBlk
	}

	// Final suspend: yield back to scheduler so it can see coro.done()=true
	// before destroying the coroutine frame.
	finalSuspBlk := coroFn.NewBlock("final.suspend")
	bodyBlk.NewBr(finalSuspBlk)

	// T0147: Define panic exit block — transfer panic state from TLS to G struct.
	{
		gTy := goroutineStructType()
		peCurrentG := goPanicExitDC.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		peGPtr := goPanicExitDC.NewBitCast(peCurrentG, irtypes.NewPointer(gTy))

		pePanicType := goPanicExitDC.NewLoad(irtypes.I8, c.panicTypeTlsGlobal)
		pePanickedField := goPanicExitDC.NewGetElementPtr(gTy, peGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicked)))
		goPanicExitDC.NewStore(pePanicType, pePanickedField)

		pePanicMsg := goPanicExitDC.NewLoad(irtypes.I8Ptr, c.panicMsgTlsGlobal)
		pePanicMsgField := goPanicExitDC.NewGetElementPtr(gTy, peGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicMsg)))
		goPanicExitDC.NewStore(pePanicMsg, pePanicMsgField)

		goPanicExitDC.NewStore(constant.NewInt(irtypes.I8, 0), c.panicFlagGlobal)
		goPanicExitDC.NewStore(constant.NewNull(irtypes.I8Ptr), c.panicMsgTlsGlobal)
		goPanicExitDC.NewStore(constant.NewInt(irtypes.I8, 0), c.panicTypeTlsGlobal)

		goPanicExitDC.NewBr(finalSuspBlk)
	}

	// Cleanup: free coroutine memory (only reached via destroy path)
	coroMem := cleanupBlk.NewCall(c.coroFree, coroId, hdl)
	needFree := cleanupBlk.NewICmp(enum.IPredNE, coroMem, constant.NewNull(irtypes.I8Ptr))
	freeBlk := coroFn.NewBlock("coro.free")
	doneBlk := coroFn.NewBlock("coro.done")
	cleanupBlk.NewCondBr(needFree, freeBlk, doneBlk)

	freeBlk.NewCall(c.palFree, coroMem)
	freeBlk.NewBr(doneBlk)

	// Done: single coro.end (both final-suspend exit and cleanup converge here)
	doneBlk.NewCall(c.coroEnd, hdl, constant.False, constant.None)
	doneBlk.NewRet(hdl)

	// Final suspend switch: default/i8 0 → doneBlk (skip free, just coro.end+ret)
	// i8 1 (destroy) → cleanup (free frame then coro.end+ret)
	finalResult := finalSuspBlk.NewCall(c.coroSuspend, constant.None, constant.True)
	finalSuspBlk.NewSwitch(finalResult, doneBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), doneBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	// 6. Caller: call ramp, create G, set up result storage, enqueue
	handle := c.block.NewCall(coroFn, argVals...)
	gRaw := c.block.NewCall(c.funcs["promise_g_new"], handle)

	if !c.goExprFireAndForget {
		gTy := goroutineStructType()
		gPtr := c.block.NewBitCast(gRaw, irtypes.NewPointer(gTy))
		rpField := c.block.NewGetElementPtr(gTy, gPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
		if !isVoid {
			// Task[T]: allocate result buffer and store in G.result_ptr.
			// The coroutine body stores the result here; the receiver loads + frees it.
			resultSize := constant.NewInt(irtypes.I64, int64(c.typeSize(resultLLVM)))
			resultBuf := c.block.NewCall(c.palAlloc, resultSize)
			c.block.NewStore(resultBuf, rpField)
		} else {
			// Void task: set result_ptr to sentinel (0x1) so goroutine_exit
			// knows this is a task and won't free G (caller frees via <-task)
			sentinel := c.block.NewIntToPtr(constant.NewInt(c.ptrIntType(), 1), irtypes.I8Ptr)
			c.block.NewStore(sentinel, rpField)
		}
	}
	// Fire-and-forget (void or non-void): result_ptr stays null (from
	// promise_g_new), so goroutine_exit frees the G struct. The coro body
	// null-checks result_ptr before storing (B0109).

	c.block.NewCall(c.funcs["promise_sched_enqueue"], gRaw)

	return gRaw
}

// resolveGoTarget resolves the IR function for a call expression used in `go func()`.
// Returns the target function and, if it's an extern, the ExternFunc info.
func (c *Compiler) resolveGoTarget(callExpr *ast.CallExpr) (*ir.Func, *ExternFunc) {
	if ident, ok := callExpr.Callee.(*ast.IdentExpr); ok {
		if ext, ok := c.externs[ident.Name]; ok {
			return ext.IRFunc, ext
		}
		if fn, ok := c.funcs[ident.Name]; ok {
			return fn, nil
		}
	}
	// Method call or complex callee — wrap in a thunk
	// For now, only support direct function calls
	panic(fmt.Sprintf("codegen: go expression callee %T not yet supported", callExpr.Callee))
}

// genGoCallExprViaBlock handles `go expr()` where the callee is not a simple
// function name — method calls (obj.method()), module-qualified calls (mod.func()),
// generic calls with explicit type args (identity[int]()), etc. (B0113)
//
// Uses the genGoBlock pattern: captures outer locals, creates a coroutine with
// full codegen context, and generates the call via genExpr inside the body.
// Unlike genGoBlock, supports non-void results for Task[T].
func (c *Compiler) genGoCallExprViaBlock(callExpr *ast.CallExpr) value.Value {
	// 1. Determine result type
	callResultType := c.info.Types[callExpr]
	isVoid := (callResultType == nil || callResultType == types.TypVoid)
	var resultLLVM irtypes.Type = irtypes.Void
	if !isVoid {
		resultLLVM = c.resolveType(callResultType)
	}

	// 2. Collect outer variables referenced in the call expression.
	// Wrap call in a synthetic block so we can reuse collectBlockIdents.
	syntheticBlock := &ast.Block{
		Stmts: []ast.Stmt{&ast.ExprStmt{Expr: callExpr}},
	}
	captureNames := collectBlockIdents(syntheticBlock, c.locals)

	// 3. Load captured values in caller scope
	var captureVals []value.Value
	var captureLLVMTypes []irtypes.Type
	for _, name := range captureNames {
		alloca := c.locals[name]
		elemType := alloca.ElemType
		val := c.block.NewLoad(elemType, alloca)
		captureVals = append(captureVals, val)
		captureLLVMTypes = append(captureLLVMTypes, elemType)
	}

	// B0163: Increment refcount for captured channel variables and collect their types.
	chanTypeVB := channelStructType()
	capturedChanTypesVB := make(map[string]types.Type)
	for i, name := range captureNames {
		if binding, ok := c.dropBindings[name]; ok {
			if _, isCh := types.AsChannel(binding.valType); isCh || binding.named == types.TypChannel {
				chPtr := c.block.NewBitCast(captureVals[i], irtypes.NewPointer(chanTypeVB))
				rcField := c.block.NewGetElementPtr(chanTypeVB, chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRefcount)))
				c.emitAtomicAdd(c.block, rcField, constant.NewInt(irtypes.I64, 1), irtypes.I64)
				capturedChanTypesVB[name] = binding.valType
			}
		}
	}

	// B0354: Collect droppable non-channel captures for ownership transfer.
	capturedDroppablesVB := make(map[string]types.Type)
	for _, name := range captureNames {
		if _, isChannel := capturedChanTypesVB[name]; isChannel {
			continue
		}
		if binding, ok := c.dropBindings[name]; ok {
			capturedDroppablesVB[name] = binding.valType
		}
	}

	// 4. Create coroutine function with captured values as parameters
	coroName := fmt.Sprintf(".goroutine.%d", c.goCounter)
	c.goCounter++
	var coroParams []*ir.Param
	for i, name := range captureNames {
		coroParams = append(coroParams, ir.NewParam(name+".cap", captureLLVMTypes[i]))
	}
	coroFn := c.module.NewFunc(coroName, irtypes.I8Ptr, coroParams...)
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, rawFuncAttr("presplitcoroutine"))

	// 5. Save and switch context
	savedFn := c.fn
	savedBlock := c.block
	savedEntryBlock := c.entryBlock
	savedLocals := c.locals
	savedCanError := c.canError
	savedRetType := c.currentRetType
	savedBlockCounter := c.blockCounter
	savedScopeBindings := c.scopeBindings
	savedDropFlags := c.dropFlags
	savedDropBindings := c.dropBindings // B0035: must save/restore for NLL early drops
	savedLoopScopeDepth := c.loopScopeDepth
	savedInCoroutine := c.inCoroutine
	savedCoroCleanup := c.coroCleanupBlk
	savedCoroSuspend := c.coroSuspendBlk
	savedPanicExitBlock := c.panicExitBlock
	savedCoroutineReturnBlock := c.coroutineReturnBlock
	savedGoExprFF := c.goExprFireAndForget
	savedLocalNameCount := c.localNameCount // T0261
	savedStmtTemps := c.stmtTemps           // T0594: stmtTemps must not leak from coroutine body into outer function
	savedStmtTempMap := c.stmtTempMap       // T0594: allocas created inside coroutine body live in a different function
	savedEnumCtorTemps := c.enumCtorTemps   // B0267: enumCtorTemps must not leak from coroutine body into outer function
	c.fn = coroFn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.blockCounter = 0
	c.canError = false
	c.currentRetType = types.TypVoid
	c.scopeBindings = nil
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.dropBindings = make(map[string]scopeBinding)
	c.loopScopeDepth = 0
	c.inCoroutine = true
	c.stmtTemps = nil                         // T0594: fresh temp state for coroutine body
	c.stmtTempMap = make(map[value.Value]int) // T0594
	c.enumCtorTemps = nil                     // B0267

	// 6. Coroutine preamble
	entry := coroFn.NewBlock(".entry")
	c.block = entry

	coroId := entry.NewCall(c.coroId,
		constant.NewInt(irtypes.I32, 0),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr))

	need := entry.NewCall(c.coroAlloc, coroId)
	allocBlk := coroFn.NewBlock("coro.alloc")
	startBlk := coroFn.NewBlock("coro.start")
	entry.NewCondBr(need, allocBlk, startBlk)

	coroSizeVal := allocBlk.NewCall(c.coroSize)
	var coroSizeArg value.Value = coroSizeVal
	if c.isWasm {
		coroSizeArg = allocBlk.NewZExt(coroSizeVal, irtypes.I64)
	}
	mem := allocBlk.NewCall(c.palAlloc, coroSizeArg)
	allocBlk.NewBr(startBlk)

	phiMem := startBlk.NewPhi(
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), entry),
		ir.NewIncoming(mem, allocBlk))
	hdl := startBlk.NewCall(c.coroBegin, coroId, phiMem)

	// Store captured params into allocas (after coro.begin → part of frame)
	for i, name := range captureNames {
		alloca := startBlk.NewAlloca(captureLLVMTypes[i])
		alloca.SetName(c.uniqueLocalName(name + ".addr"))
		startBlk.NewStore(coroFn.Params[i], alloca)
		c.locals[name] = alloca
	}

	// B0163: Register drop bindings for captured channel variables inside the goroutine.
	c.entryBlock = startBlk
	c.block = startBlk
	for _, name := range captureNames {
		if chanValType, ok := capturedChanTypesVB[name]; ok {
			alloca := c.locals[name]
			c.maybeRegisterDrop(name, alloca, chanValType)
		}
	}

	// B0354: Register drop bindings for non-channel droppable captures.
	for _, name := range captureNames {
		if valType, ok := capturedDroppablesVB[name]; ok {
			alloca := c.locals[name]
			c.maybeRegisterDrop(name, alloca, valType)
		}
	}

	// Initial suspend
	initSuspBlk := coroFn.NewBlock("coro.init.suspend")
	startBlk.NewBr(initSuspBlk)

	initResult := initSuspBlk.NewCall(c.coroSuspend, constant.None, constant.False)

	suspendBlk := coroFn.NewBlock("coro.suspend")
	bodyBlk := coroFn.NewBlock("body")
	cleanupBlk := coroFn.NewBlock("cleanup")
	doneBlk := coroFn.NewBlock("coro.done")

	initSuspBlk.NewSwitch(initResult, suspendBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), bodyBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	suspendBlk.NewRet(hdl)

	c.coroCleanupBlk = cleanupBlk
	c.coroSuspendBlk = doneBlk

	// 7. Body: generate the call expression and optionally store result
	c.block = bodyBlk
	c.entryBlock = startBlk

	// B0228: Create panic exit block for the go block coroutine.
	// When a panic occurs, transfer panic state to G struct and branch to final suspend.
	goPanicExitBlk := coroFn.NewBlock("go.panic_exit")
	c.panicExitBlock = goPanicExitBlk

	result := c.genExpr(callExpr)

	// Clear panic exit block after body generation
	c.panicExitBlock = nil

	if !isVoid && result != nil {
		// Store result via G.result_ptr (set by caller before enqueue)
		gTy := goroutineStructType()
		currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		gPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTy))
		rpField := c.block.NewGetElementPtr(gTy, gPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
		rpVal := c.block.NewLoad(irtypes.I8Ptr, rpField)
		typedRP := c.block.NewBitCast(rpVal, irtypes.NewPointer(resultLLVM))
		c.block.NewStore(result, typedRP)
		// T0594: claim the result stmtTemp — ownership transferred to G.result_ptr.
		c.claimStringTemp(result)
	}

	// T0594: Clean up any remaining stmtTemps from the coroutine body before restoring
	// the outer function's context. Without this, temps created by genExpr (e.g., string
	// return values tracked by trackStringTemp) would be orphaned inside the coroutine.
	c.cleanupStmtTemps()

	// B0163: Emit cleanup for captured channel drop bindings.
	if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > 0 {
		c.emitScopeCleanup(0, false)
	}

	// T0147: Per-call panic checks in genExpr now handle panic detection.
	// The call expression at line above goes through genExpr → case *ast.CallExpr
	// which calls emitPanicCheck() → emitPanicReturn() → branches to panicExitBlock.

	// Final suspend
	finalSuspBlk := coroFn.NewBlock("final.suspend")
	// T0148: Final panic check after body + scope cleanup.
	// Catches panics from drop functions during scope cleanup that per-call checks miss.
	if c.block != nil && c.block.Term == nil {
		finalFlag := c.block.NewLoad(irtypes.I8, c.panicFlagGlobal)
		finalIsPanic := c.block.NewICmp(enum.IPredNE, finalFlag, constant.NewInt(irtypes.I8, 0))
		c.block.NewCondBr(finalIsPanic, goPanicExitBlk, finalSuspBlk)
	}

	// B0228: Define the go block panic exit block body.
	// Transfer panic state from TLS to G struct, clear TLS flag, branch to final suspend.
	{
		gTy := goroutineStructType()
		peCurrentG := goPanicExitBlk.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		peGPtr := goPanicExitBlk.NewBitCast(peCurrentG, irtypes.NewPointer(gTy))

		// Load TLS panic type and store in G.panicked
		pePanicType := goPanicExitBlk.NewLoad(irtypes.I8, c.panicTypeTlsGlobal)
		pePanickedField := goPanicExitBlk.NewGetElementPtr(gTy, peGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicked)))
		goPanicExitBlk.NewStore(pePanicType, pePanickedField)

		// Load TLS panic msg and store in G.panic_msg
		pePanicMsg := goPanicExitBlk.NewLoad(irtypes.I8Ptr, c.panicMsgTlsGlobal)
		pePanicMsgField := goPanicExitBlk.NewGetElementPtr(gTy, peGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicMsg)))
		goPanicExitBlk.NewStore(pePanicMsg, pePanicMsgField)

		// Clear TLS panic flag and msg
		goPanicExitBlk.NewStore(constant.NewInt(irtypes.I8, 0), c.panicFlagGlobal)
		goPanicExitBlk.NewStore(constant.NewNull(irtypes.I8Ptr), c.panicMsgTlsGlobal)
		goPanicExitBlk.NewStore(constant.NewInt(irtypes.I8, 0), c.panicTypeTlsGlobal)

		// Branch to final suspend (properly ends the coroutine)
		goPanicExitBlk.NewBr(finalSuspBlk)
	}

	// Cleanup: free coroutine memory (only reached via destroy path)
	coroMem := cleanupBlk.NewCall(c.coroFree, coroId, hdl)
	needFree := cleanupBlk.NewICmp(enum.IPredNE, coroMem, constant.NewNull(irtypes.I8Ptr))
	freeBlk := coroFn.NewBlock("coro.free")
	cleanupBlk.NewCondBr(needFree, freeBlk, doneBlk)

	freeBlk.NewCall(c.palFree, coroMem)
	freeBlk.NewBr(doneBlk)

	// Done: single coro.end
	doneBlk.NewCall(c.coroEnd, hdl, constant.False, constant.None)
	doneBlk.NewRet(hdl)

	// Final suspend switch
	finalResult := finalSuspBlk.NewCall(c.coroSuspend, constant.None, constant.True)
	finalSuspBlk.NewSwitch(finalResult, doneBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), doneBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	// 8. Restore context
	c.fn = savedFn
	c.block = savedBlock
	c.entryBlock = savedEntryBlock
	c.locals = savedLocals
	c.canError = savedCanError
	c.currentRetType = savedRetType
	c.blockCounter = savedBlockCounter
	c.scopeBindings = savedScopeBindings
	c.dropFlags = savedDropFlags
	c.dropBindings = savedDropBindings // B0035: restore for NLL early drops
	c.loopScopeDepth = savedLoopScopeDepth
	c.inCoroutine = savedInCoroutine
	c.coroCleanupBlk = savedCoroCleanup
	c.coroSuspendBlk = savedCoroSuspend
	c.panicExitBlock = savedPanicExitBlock
	c.coroutineReturnBlock = savedCoroutineReturnBlock
	c.goExprFireAndForget = savedGoExprFF
	c.localNameCount = savedLocalNameCount // T0261
	c.stmtTemps = savedStmtTemps           // T0594: restore outer function's temp state
	c.stmtTempMap = savedStmtTempMap       // T0594
	c.enumCtorTemps = savedEnumCtorTemps   // B0267

	// B0354: Clear outer drop flags for captured droppable non-channel variables.
	for name := range capturedDroppablesVB {
		c.clearDropFlag(name)
	}

	// 9. Caller: call coroutine ramp → get handle, create G, enqueue
	handle := c.block.NewCall(coroFn, captureVals...)
	gRaw := c.block.NewCall(c.funcs["promise_g_new"], handle)

	if !isVoid || !c.goExprFireAndForget {
		gTy := goroutineStructType()
		gPtr := c.block.NewBitCast(gRaw, irtypes.NewPointer(gTy))
		rpField := c.block.NewGetElementPtr(gTy, gPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
		if !isVoid {
			// Task[T]: allocate result buffer and store in G.result_ptr
			resultSize := constant.NewInt(irtypes.I64, int64(c.typeSize(resultLLVM)))
			resultBuf := c.block.NewCall(c.palAlloc, resultSize)
			c.block.NewStore(resultBuf, rpField)
		} else {
			// Void task: set result_ptr to sentinel (0x1)
			sentinel := c.block.NewIntToPtr(constant.NewInt(c.ptrIntType(), 1), irtypes.I8Ptr)
			c.block.NewStore(sentinel, rpField)
		}
	}

	c.block.NewCall(c.funcs["promise_sched_enqueue"], gRaw)

	return gRaw
}

// genGoExternWrapper generates a thin wrapper function around an extern call
// for use in go expressions. The wrapper takes Promise-internal argument types
// and returns the Promise-internal result type, handling sret/ABI coercion
// internally via genExternCall. This is needed because extern IR functions use
// void return + sret pointer for struct returns, which is incompatible with
// the coroutine body's direct call + store pattern (B0046).
func (c *Compiler) genGoExternWrapper(ext *ExternFunc, argLLVMTypes []irtypes.Type, argTypes []types.Type, resultLLVM irtypes.Type, isVoid bool) *ir.Func {
	wrapName := fmt.Sprintf(".go_extern_wrap.%s.%d", ext.PromiseName, c.goCounter)

	var params []*ir.Param
	for i, ty := range argLLVMTypes {
		params = append(params, ir.NewParam(fmt.Sprintf("arg.%d", i), ty))
	}

	retType := irtypes.Type(irtypes.Void)
	if !isVoid {
		retType = resultLLVM
	}
	wrapFn := c.module.NewFunc(wrapName, retType, params...)

	saved := c.saveState()
	defer c.restoreState(saved)

	c.fn = wrapFn
	entry := wrapFn.NewBlock("entry")
	c.block = entry
	c.entryBlock = entry
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.dropBindings = make(map[string]scopeBinding)
	c.scopeBindings = nil

	var argVals []value.Value
	for i := range ext.ParamTypes {
		argVals = append(argVals, wrapFn.Params[i])
	}

	result := c.genExternCall(ext, argVals, argTypes)
	if result != nil && !isVoid {
		c.block.NewRet(result)
	} else {
		c.block.NewRet(nil)
	}

	return wrapFn
}

// collectBlockIdents walks an AST block and collects all IdentExpr names referenced.
// Returns a sorted, deduplicated list of names that exist in outerLocals.
func collectBlockIdents(block *ast.Block, outerLocals map[string]*ir.InstAlloca) []string {
	seen := make(map[string]bool)
	var walkExpr func(e ast.Expr)
	var walkStmt func(s ast.Stmt)

	walkExpr = func(e ast.Expr) {
		if e == nil {
			return
		}
		switch e := e.(type) {
		case *ast.IdentExpr:
			if _, ok := outerLocals[e.Name]; ok {
				seen[e.Name] = true
			}
		case *ast.BinaryExpr:
			walkExpr(e.Left)
			walkExpr(e.Right)
		case *ast.UnaryExpr:
			walkExpr(e.Operand)
		case *ast.CallExpr:
			walkExpr(e.Callee)
			for _, arg := range e.Args {
				walkExpr(arg.Value)
			}
		case *ast.IndexExpr:
			walkExpr(e.Target)
			walkExpr(e.Index)
		case *ast.SliceExpr:
			walkExpr(e.Target)
			walkExpr(e.Low)
			walkExpr(e.High)
		case *ast.SliceTypeExpr:
			walkExpr(e.Inner)
		case *ast.MemberExpr:
			walkExpr(e.Target)
		case *ast.OptionalChainExpr:
			walkExpr(e.Target)
		case *ast.IsExpr:
			walkExpr(e.Expr)
		case *ast.CastExpr:
			walkExpr(e.Expr)
		case *ast.ErrorPropagateExpr:
			walkExpr(e.Expr)
		case *ast.ErrorPanicExpr:
			walkExpr(e.Expr)
		case *ast.OptionalUnwrapExpr:
			walkExpr(e.Expr)
		case *ast.AutoCloneExpr: // T0605
			walkExpr(e.Expr)
		case *ast.ErrorHandlerExpr:
			walkExpr(e.Expr)
			if e.Body != nil {
				for _, s := range e.Body.Stmts {
					walkStmt(s)
				}
			}
		case *ast.IfExpr:
			walkExpr(e.Cond)
			if e.Then != nil {
				for _, s := range e.Then.Stmts {
					walkStmt(s)
				}
			}
			if e.Else != nil {
				for _, s := range e.Else.Stmts {
					walkStmt(s)
				}
			}
		case *ast.MatchExpr:
			walkExpr(e.Subject)
			for _, arm := range e.Arms {
				walkExpr(arm.Body)
				if arm.Guard != nil {
					walkExpr(arm.Guard)
				}
				if arm.Block != nil {
					for _, s := range arm.Block.Stmts {
						walkStmt(s)
					}
				}
			}
		case *ast.StringLit:
			for _, part := range e.Parts {
				if interp, ok := part.(ast.StringInterp); ok {
					walkExpr(interp.Expr)
				}
			}
		case *ast.TupleLit:
			for _, elem := range e.Elements {
				walkExpr(elem)
			}
		case *ast.ArrayLit:
			for _, elem := range e.Elements {
				walkExpr(elem)
			}
		case *ast.MapLit:
			for _, entry := range e.Entries {
				walkExpr(entry.Key)
				walkExpr(entry.Value)
			}
		case *ast.GoExpr:
			if e.Expr != nil {
				walkExpr(e.Expr)
			}
			if e.Block != nil {
				for _, s := range e.Block.Stmts {
					walkStmt(s)
				}
			}
		case *ast.LambdaExpr:
			// Lambda captures are handled separately; skip inner references
		case *ast.ParenExpr:
			walkExpr(e.Expr)
		case *ast.UnsafeExpr:
			if e.Body != nil {
				for _, s := range e.Body.Stmts {
					walkStmt(s)
				}
			}
		}
	}

	walkStmt = func(s ast.Stmt) {
		if s == nil {
			return
		}
		switch s := s.(type) {
		case *ast.ExprStmt:
			walkExpr(s.Expr)
		case *ast.InferredVarDecl:
			walkExpr(s.Value)
		case *ast.TypedVarDecl:
			walkExpr(s.Value)
		case *ast.AssignStmt:
			walkExpr(s.Target)
			walkExpr(s.Value)
		case *ast.ReturnStmt:
			walkExpr(s.Value)
		case *ast.RaiseStmt:
			walkExpr(s.Value)
		case *ast.YieldStmt:
			walkExpr(s.Value)
		case *ast.IfStmt:
			walkExpr(s.Cond)
			walkExpr(s.Init)
			if s.Body != nil {
				for _, st := range s.Body.Stmts {
					walkStmt(st)
				}
			}
			if s.Else != nil {
				walkStmt(s.Else)
			}
		case *ast.ForInStmt:
			walkExpr(s.Iterable)
			if s.Body != nil {
				for _, st := range s.Body.Stmts {
					walkStmt(st)
				}
			}
		case *ast.ClassicForStmt:
			walkExpr(s.InitValue)
			walkExpr(s.Cond)
			walkExpr(s.UpdateTarget)
			walkExpr(s.UpdateValue)
			if s.Body != nil {
				for _, st := range s.Body.Stmts {
					walkStmt(st)
				}
			}
		case *ast.WhileStmt:
			walkExpr(s.Cond)
			if s.Body != nil {
				for _, st := range s.Body.Stmts {
					walkStmt(st)
				}
			}
		case *ast.WhileUnwrapStmt:
			walkExpr(s.Value)
			if s.Body != nil {
				for _, st := range s.Body.Stmts {
					walkStmt(st)
				}
			}
		case *ast.DestructureVarDecl:
			walkExpr(s.Value)
		case *ast.UseVarDecl:
			walkExpr(s.Value)
		case *ast.YieldDelegateStmt:
			walkExpr(s.Value)
		case *ast.InfiniteLoop:
			if s.Body != nil {
				for _, st := range s.Body.Stmts {
					walkStmt(st)
				}
			}
		case *ast.IncDecStmt:
			walkExpr(s.Target)
		case *ast.SelectStmt:
			for _, sc := range s.Cases {
				walkExpr(sc.Channel)
				walkExpr(sc.SendValue)
				for _, st := range sc.Body {
					walkStmt(st)
				}
			}
			for _, st := range s.Default {
				walkStmt(st)
			}
		case *ast.Block:
			for _, st := range s.Stmts {
				walkStmt(st)
			}
		}
	}

	for _, s := range block.Stmts {
		walkStmt(s)
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// genGoBlock handles `go { block }` — wraps the block in a void function and spawns it.
// Captures outer local variables referenced in the block and passes them through the arg pack.
func (c *Compiler) genGoBlock(block *ast.Block) value.Value {
	// Collect outer variables referenced in the block
	captureNames := collectBlockIdents(block, c.locals)

	// Load captured values and collect their types BEFORE switching context
	var captureVals []value.Value
	var captureLLVMTypes []irtypes.Type
	for _, name := range captureNames {
		alloca := c.locals[name]
		elemType := alloca.ElemType
		val := c.block.NewLoad(elemType, alloca)
		captureVals = append(captureVals, val)
		captureLLVMTypes = append(captureLLVMTypes, elemType)
	}

	// B0163: Increment refcount for captured channel variables and collect their types.
	// The goroutine shares the channel pointer with the outer scope,
	// so both need to call Channel.drop — refcounting prevents double-free.
	chanTypeGB := channelStructType()
	capturedChanTypes := make(map[string]types.Type) // name → sema type for channels
	for i, name := range captureNames {
		if binding, ok := c.dropBindings[name]; ok {
			if _, isCh := types.AsChannel(binding.valType); isCh || binding.named == types.TypChannel {
				chPtr := c.block.NewBitCast(captureVals[i], irtypes.NewPointer(chanTypeGB))
				rcField := c.block.NewGetElementPtr(chanTypeGB, chPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRefcount)))
				c.emitAtomicAdd(c.block, rcField, constant.NewInt(irtypes.I64, 1), irtypes.I64)
				capturedChanTypes[name] = binding.valType
			}
		}
	}

	// B0354: Collect droppable non-channel captures for ownership transfer.
	// Strings, vectors, heap user types with drop, etc. — the goroutine takes
	// ownership; the outer scope's drop flag is cleared after spawn.
	capturedDroppables := make(map[string]types.Type)
	for _, name := range captureNames {
		if _, isChannel := capturedChanTypes[name]; isChannel {
			continue
		}
		if binding, ok := c.dropBindings[name]; ok {
			capturedDroppables[name] = binding.valType
		}
	}

	// Create coroutine function with captured values as parameters
	coroName := fmt.Sprintf(".goroutine.%d", c.goCounter)
	c.goCounter++
	var coroParams []*ir.Param
	for i, name := range captureNames {
		coroParams = append(coroParams, ir.NewParam(name+".cap", captureLLVMTypes[i]))
	}
	coroFn := c.module.NewFunc(coroName, irtypes.I8Ptr, coroParams...)
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, rawFuncAttr("presplitcoroutine"))

	// Save and switch context
	savedFn := c.fn
	savedBlock := c.block
	savedEntryBlock := c.entryBlock
	savedLocals := c.locals
	savedCanError := c.canError
	savedRetType := c.currentRetType
	savedBlockCounter := c.blockCounter
	savedScopeBindings := c.scopeBindings
	savedDropFlags := c.dropFlags
	savedDropBindings := c.dropBindings // B0035: must save/restore for NLL early drops
	savedLoopScopeDepth := c.loopScopeDepth
	savedInCoroutine := c.inCoroutine
	savedCoroCleanup := c.coroCleanupBlk
	savedCoroSuspend := c.coroSuspendBlk
	savedPanicExitBlock := c.panicExitBlock
	savedCoroutineReturnBlock := c.coroutineReturnBlock
	savedGoExprFF := c.goExprFireAndForget
	savedLocalNameCount := c.localNameCount // T0261
	savedEnumCtorTemps := c.enumCtorTemps   // B0267
	c.goExprFireAndForget = false           // reset for inner statements (B0109)

	c.fn = coroFn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.blockCounter = 0
	c.canError = false
	c.currentRetType = types.TypVoid
	c.scopeBindings = nil
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.dropBindings = make(map[string]scopeBinding)
	c.loopScopeDepth = 0
	c.inCoroutine = true
	c.enumCtorTemps = nil // B0267

	// --- Coroutine preamble ---
	entry := coroFn.NewBlock(".entry")
	c.block = entry

	coroId := entry.NewCall(c.coroId,
		constant.NewInt(irtypes.I32, 0),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr))

	need := entry.NewCall(c.coroAlloc, coroId)
	allocBlk := coroFn.NewBlock("coro.alloc")
	startBlk := coroFn.NewBlock("coro.start")
	entry.NewCondBr(need, allocBlk, startBlk)

	coroSizeVal := allocBlk.NewCall(c.coroSize)
	var coroSizeArg value.Value = coroSizeVal
	if c.isWasm {
		coroSizeArg = allocBlk.NewZExt(coroSizeVal, irtypes.I64)
	}
	mem := allocBlk.NewCall(c.palAlloc, coroSizeArg)
	allocBlk.NewBr(startBlk)

	phiMem := startBlk.NewPhi(
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), entry),
		ir.NewIncoming(mem, allocBlk))
	hdl := startBlk.NewCall(c.coroBegin, coroId, phiMem)

	// Store captured params into allocas (after coro.begin → part of frame)
	for i, name := range captureNames {
		alloca := startBlk.NewAlloca(captureLLVMTypes[i])
		alloca.SetName(c.uniqueLocalName(name + ".addr"))
		startBlk.NewStore(coroFn.Params[i], alloca)
		c.locals[name] = alloca
	}

	// B0163: Register drop bindings for captured channel variables inside the goroutine.
	// This ensures Channel.drop is called when the goroutine finishes, decrementing the refcount.
	// Set both entryBlock and block to startBlk so allocas and stores land in the right place.
	c.entryBlock = startBlk
	c.block = startBlk
	for _, name := range captureNames {
		if chanValType, ok := capturedChanTypes[name]; ok {
			alloca := c.locals[name]
			c.maybeRegisterDrop(name, alloca, chanValType)
		}
	}

	// B0354: Register drop bindings for non-channel droppable captures.
	// Goroutine assumes ownership; outer drop flag is cleared after spawn.
	for _, name := range captureNames {
		if valType, ok := capturedDroppables[name]; ok {
			alloca := c.locals[name]
			c.maybeRegisterDrop(name, alloca, valType)
		}
	}

	// Initial suspend — in a separate block so that createEntryAlloca can
	// append allocas to startBlk BEFORE the suspend point. coro-split needs
	// allocas to precede coro.suspend to properly spill them to the frame.
	initSuspBlk := coroFn.NewBlock("coro.init.suspend")
	startBlk.NewBr(initSuspBlk)

	initResult := initSuspBlk.NewCall(c.coroSuspend, constant.None, constant.False)

	suspendBlk := coroFn.NewBlock("coro.suspend")
	bodyBlk := coroFn.NewBlock("body")
	cleanupBlk := coroFn.NewBlock("cleanup")
	// Create doneBlk early so intermediate coro.suspend switches can reference it.
	// Instructions are added after the body is compiled.
	doneBlk := coroFn.NewBlock("coro.done")
	// B0353: Create finalSuspBlk early so return statements inside the body
	// can branch here. Instructions are added after the body is compiled.
	finalSuspBlk := coroFn.NewBlock("final.suspend")

	initSuspBlk.NewSwitch(initResult, suspendBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), bodyBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	// Suspend: ramp returns coroutine handle
	suspendBlk.NewRet(hdl)

	// Set cleanup and suspend blocks for mid-body coro.suspend switches.
	// Cleanup = destroy path (coro.free + free). Suspend = default case (coro.end + ret).
	// Per LLVM coroutine ABI, intermediate coro.suspend default cases must go to the
	// suspend block, NOT the cleanup block — otherwise the frame is freed on park.
	c.coroCleanupBlk = cleanupBlk
	c.coroSuspendBlk = doneBlk

	// --- Body: compile user block ---
	c.block = bodyBlk
	c.entryBlock = startBlk // allocas go in startBlk (after coro.begin) to be part of coroutine frame

	// B0228: Create panic exit block for this go block coroutine.
	goPanicExitBlk2 := coroFn.NewBlock("go.panic_exit")
	c.panicExitBlock = goPanicExitBlk2
	c.coroutineReturnBlock = finalSuspBlk // B0353

	c.genBlock(block)

	// Clear panic exit block and coroutine return block after body generation
	c.panicExitBlock = nil
	c.coroutineReturnBlock = nil

	// B0163: Emit cleanup for captured channel drop bindings registered before genBlock.
	// genBlock only cleans up bindings added within its scope, so we must handle
	// pre-block bindings (captured channels) here before the final suspend.
	if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > 0 {
		c.emitScopeCleanup(0, false)
	}

	// T0147: Per-call panic checks in genExpr now handle panic detection.
	// Calls within the block go through genExpr → case *ast.CallExpr
	// which calls emitPanicCheck() → emitPanicReturn() → branches to panicExitBlock.

	// Final suspend: yield back to scheduler so it can see coro.done()=true
	// before destroying the coroutine frame.
	// T0148: Final panic check after body + scope cleanup.
	// Catches panics from drop functions during scope cleanup that per-call checks miss.
	if c.block != nil && c.block.Term == nil {
		finalFlag := c.block.NewLoad(irtypes.I8, c.panicFlagGlobal)
		finalIsPanic := c.block.NewICmp(enum.IPredNE, finalFlag, constant.NewInt(irtypes.I8, 0))
		c.block.NewCondBr(finalIsPanic, goPanicExitBlk2, finalSuspBlk)
	}

	// B0228: Define the go block panic exit block body (same as first go block variant).
	{
		gTy := goroutineStructType()
		peCurrentG := goPanicExitBlk2.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		peGPtr := goPanicExitBlk2.NewBitCast(peCurrentG, irtypes.NewPointer(gTy))

		pePanicType := goPanicExitBlk2.NewLoad(irtypes.I8, c.panicTypeTlsGlobal)
		pePanickedField := goPanicExitBlk2.NewGetElementPtr(gTy, peGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicked)))
		goPanicExitBlk2.NewStore(pePanicType, pePanickedField)

		pePanicMsg := goPanicExitBlk2.NewLoad(irtypes.I8Ptr, c.panicMsgTlsGlobal)
		pePanicMsgField := goPanicExitBlk2.NewGetElementPtr(gTy, peGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicMsg)))
		goPanicExitBlk2.NewStore(pePanicMsg, pePanicMsgField)

		goPanicExitBlk2.NewStore(constant.NewInt(irtypes.I8, 0), c.panicFlagGlobal)
		goPanicExitBlk2.NewStore(constant.NewNull(irtypes.I8Ptr), c.panicMsgTlsGlobal)
		goPanicExitBlk2.NewStore(constant.NewInt(irtypes.I8, 0), c.panicTypeTlsGlobal)

		goPanicExitBlk2.NewBr(finalSuspBlk)
	}

	// --- Cleanup: free coroutine memory (only reached via destroy path) ---
	coroMem := cleanupBlk.NewCall(c.coroFree, coroId, hdl)
	needFree := cleanupBlk.NewICmp(enum.IPredNE, coroMem, constant.NewNull(irtypes.I8Ptr))
	freeBlk := coroFn.NewBlock("coro.free")
	cleanupBlk.NewCondBr(needFree, freeBlk, doneBlk)

	freeBlk.NewCall(c.palFree, coroMem)
	freeBlk.NewBr(doneBlk)

	// Done: single coro.end (both final-suspend exit and cleanup converge here)
	doneBlk.NewCall(c.coroEnd, hdl, constant.False, constant.None)
	doneBlk.NewRet(hdl)

	// Final suspend switch: default/i8 0 → doneBlk (skip free, just coro.end+ret)
	// i8 1 (destroy) → cleanup (free frame then coro.end+ret)
	finalResult := finalSuspBlk.NewCall(c.coroSuspend, constant.None, constant.True)
	finalSuspBlk.NewSwitch(finalResult, doneBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), doneBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk))

	// Restore context
	c.fn = savedFn
	c.block = savedBlock
	c.entryBlock = savedEntryBlock
	c.locals = savedLocals
	c.canError = savedCanError
	c.currentRetType = savedRetType
	c.blockCounter = savedBlockCounter
	c.scopeBindings = savedScopeBindings
	c.dropFlags = savedDropFlags
	c.dropBindings = savedDropBindings // B0035: restore for NLL early drops
	c.loopScopeDepth = savedLoopScopeDepth
	c.inCoroutine = savedInCoroutine
	c.coroCleanupBlk = savedCoroCleanup
	c.coroSuspendBlk = savedCoroSuspend
	c.panicExitBlock = savedPanicExitBlock
	c.coroutineReturnBlock = savedCoroutineReturnBlock
	c.goExprFireAndForget = savedGoExprFF
	c.localNameCount = savedLocalNameCount // T0261
	c.enumCtorTemps = savedEnumCtorTemps   // B0267

	// B0354: Clear outer drop flags for captured droppable non-channel variables.
	// Ownership has been transferred to the goroutine.
	for name := range capturedDroppables {
		c.clearDropFlag(name)
	}

	// Caller: call coroutine ramp → get handle, create G, enqueue
	handle := c.block.NewCall(coroFn, captureVals...)
	gRaw := c.block.NewCall(c.funcs["promise_g_new"], handle)

	if !c.goExprFireAndForget {
		// Task: set result_ptr to sentinel (0x1) so goroutine_exit knows
		// the receiver will free G (via <-task). Without this, goroutine_exit
		// would free the G and the receiver would access freed memory.
		gTy := goroutineStructType()
		gPtr := c.block.NewBitCast(gRaw, irtypes.NewPointer(gTy))
		rpField := c.block.NewGetElementPtr(gTy, gPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
		sentinel := c.block.NewIntToPtr(constant.NewInt(c.ptrIntType(), 1), irtypes.I8Ptr)
		c.block.NewStore(sentinel, rpField)
	}
	// Fire-and-forget: result_ptr stays null (from promise_g_new),
	// so goroutine_exit frees the G struct when the goroutine completes.

	c.block.NewCall(c.funcs["promise_sched_enqueue"], gRaw)

	return gRaw
}

// --- Receive expression (<-task / <-channel) ---

// genReceiveExpr generates code for `<-expr` — dispatches to task or channel receive.
func (c *Compiler) genReceiveExpr(e *ast.UnaryExpr) value.Value {
	operandType := c.info.Types[e.Operand]
	if c.typeSubst != nil {
		operandType = types.Substitute(operandType, c.typeSubst)
	}

	inst, ok := operandType.(*types.Instance)
	if !ok {
		panic(fmt.Sprintf("codegen: receive operand type %T is not Instance", operandType))
	}

	origin := inst.Origin()
	if origin == types.TypChannel {
		return c.genReceiveChannel(e, inst)
	}
	return c.genReceiveTask(e, inst)
}

// genReceiveTask generates code for `<-task` — waits for goroutine G to complete, returns T.
// The task handle is now a G pointer (i8*). Checks G.done and loads from G.result_ptr.
func (c *Compiler) genReceiveTask(e *ast.UnaryExpr, inst *types.Instance) value.Value {
	gRaw := c.genExpr(e.Operand)
	// T0503: `<-t` consumes the task — clear the scope-exit drop flag so the
	// receive's own pal_free(G) isn't followed by a double-free at scope exit.
	// Same for tracked getter temps (e.g. `<-obj.task_getter`).
	if ident, ok := e.Operand.(*ast.IdentExpr); ok {
		c.clearDropFlag(ident.Name)
	}
	// T0560: `<-h.field` consumes the task field. After T0560 wired field-drop
	// for Task[T], the field's scope-exit drop would double-free the G we just
	// freed here. Null the field so the field-drop's null check no-ops.
	// Only applies when target.Field is an actual field (not a getter).
	if member, ok := e.Operand.(*ast.MemberExpr); ok {
		targetType := c.info.Types[member.Target]
		if c.typeSubst != nil {
			targetType = types.Substitute(targetType, c.typeSubst)
		}
		if c.selfSubst != nil {
			targetType = types.SubstituteSelf(targetType, c.selfSubst.iface, c.selfSubst.concrete)
		}
		if named := extractNamed(targetType); named != nil && named.LookupField(member.Field) != nil {
			fieldPtr := c.genFieldPtr(member)
			c.block.NewStore(constant.NewNull(irtypes.I8Ptr), fieldPtr)
		}
	}
	// T0638: `<-coll[i]` consumes the indexed task. The receive frees the G
	// (pal_free below); without nulling the slot, the array/Vector scope-exit
	// element drop reloads the dangling pointer and Task[T].drop's only a
	// null-check → use-after-free / double-free → segfault. Null the slot so
	// the element drop no-ops. Mirrors the T0560 `<-h.field` field-null path.
	// Per-slot (not whole-collection): `Task[int][2]` with only ts[0] received
	// must still drop ts[1]. genReceiveChannel is intentionally untouched —
	// channel receive does not free the channel, so its slot must stay valid.
	if idxExpr, ok := e.Operand.(*ast.IndexExpr); ok {
		if slotPtr, ok := c.genReceiveTaskSlotPtr(idxExpr); ok {
			c.block.NewStore(constant.NewNull(irtypes.I8Ptr), slotPtr)
		}
	}
	// T0617: `<-handle` where `handle` is a for-in loop binding over a
	// Vector[Task]/Task[N] element loop. genForInVector/genForInArray record
	// the current iteration's slot address; null it here so the container's
	// scope-exit element drop reloads null and Task[T].drop no-ops (it null-
	// checks). Symmetric to the T0638 IndexExpr slot-null above; per-slot, so
	// un-awaited slots are still dropped once (T0503). genReceiveChannel never
	// consults this map — channel receive doesn't free the channel.
	if ident, ok := e.Operand.(*ast.IdentExpr); ok {
		if slotPtrAlloca, ok := c.forInHandleSlotPtr[ident.Name]; ok {
			slotPtr := c.block.NewLoad(irtypes.NewPointer(irtypes.I8Ptr), slotPtrAlloca)
			c.block.NewStore(constant.NewNull(irtypes.I8Ptr), slotPtr)
		}
	}
	c.claimStringTemp(gRaw)

	var innerType types.Type
	if len(inst.TypeArgs()) > 0 {
		innerType = inst.TypeArgs()[0]
	}
	isVoid := (innerType == nil || innerType == types.TypVoid)

	var resultLLVM irtypes.Type = irtypes.Void
	if !isVoid {
		resultLLVM = c.resolveType(innerType)
	}

	gTy := goroutineStructType()
	gPtr := c.block.NewBitCast(gRaw, irtypes.NewPointer(gTy))

	// Check if G is already done
	doneField := c.block.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldDone)))
	doneVal := c.block.NewLoad(irtypes.I8, doneField)
	isDone := c.block.NewICmp(enum.IPredNE, doneVal, constant.NewInt(irtypes.I8, 0))

	alreadyDone := c.newBlock("task.done")
	waitBlk := c.newBlock("task.wait")
	readyBlk := c.newBlock("task.ready")

	c.block.NewCondBr(isDone, alreadyDone, waitBlk)

	alreadyDone.NewBr(readyBlk)

	// Wait for G to complete
	c.block = waitBlk
	if c.inCoroutine {
		// Goroutine-mode: use sched.done_lock to protect done + done_waiters
		// atomically. Hold the lock across coro.suspend via G.park_mutex so
		// the scheduler releases it after suspend completes — this prevents
		// the enqueue-before-suspend race.
		currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		currentGPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTy))

		// Load and lock sched.done_lock
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
	} else {
		// Thread-blocking mode: poll G.done in a loop.
		// goroutine_exit sets G.done = 1 atomically; we just spin until we see it.
		// A brief usleep(100) avoids burning CPU in a tight loop.
		checkBlk := c.newBlock("task.check")
		spinBlk := c.newBlock("task.spin")
		doneBlk := c.newBlock("task.threaddone")

		c.block.NewBr(checkBlk)

		// check: reload done flag
		c.block = checkBlk
		doneVal2 := c.block.NewLoad(irtypes.I8, doneField)
		isDone2 := c.block.NewICmp(enum.IPredNE, doneVal2, constant.NewInt(irtypes.I8, 0))
		c.block.NewCondBr(isDone2, doneBlk, spinBlk)

		// spin: brief sleep then recheck
		c.block = spinBlk
		c.block.NewCall(c.palUsleep, constant.NewInt(irtypes.I32, 100))
		c.block.NewBr(checkBlk)

		c.block = doneBlk
		c.block.NewBr(readyBlk)
	}

	// ready: check if goroutine panicked, then load result, free G
	c.block = readyBlk

	// Check G.panicked — if the goroutine panicked, re-panic in current goroutine
	panickedField := c.block.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicked)))
	panickedVal := c.block.NewLoad(irtypes.I8, panickedField)
	isPanicked := c.block.NewICmp(enum.IPredNE, panickedVal, constant.NewInt(irtypes.I8, 0))

	rePanicBlk := c.newBlock("task.repanic")
	loadResultBlk := c.newBlock("task.load_result")
	c.block.NewCondBr(isPanicked, rePanicBlk, loadResultBlk)

	// T0547: If the operand is a captured task in the current lambda, the
	// env field still holds the G pointer because emitLambdaWritebacks ran
	// at return-statement entry (stmt.go:6351) — before this receive — and
	// wrote local→env. After pal_free(G) the env field would dangle, so
	// env_drop's Task[T].drop would spin-wait on freed memory (segfault or
	// infinite spin). Null the local alloca and env field after each pal_free
	// so env_drop sees null and no-ops. Both Task[T].drop (compiler.go:2793)
	// and envDropCallFn (expr.go:8855) already null-check.
	nullCapturedEnvField := func() {
		ident, ok := e.Operand.(*ast.IdentExpr)
		if !ok {
			return
		}
		alloca, found := c.locals[ident.Name]
		if !found {
			return
		}
		for _, wb := range c.lambdaWritebacks {
			if wb.localAlloca == alloca {
				c.block.NewStore(constant.NewNull(irtypes.I8Ptr), alloca)
				c.block.NewStore(constant.NewNull(irtypes.I8Ptr), wb.envFieldPtr)
				return
			}
		}
	}

	// rePanicBlk: goroutine panicked — load panic_msg, free G, re-panic
	c.block = rePanicBlk
	panicMsgField := c.block.NewGetElementPtr(gTy, gPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldPanicMsg)))
	panicMsg := c.block.NewLoad(irtypes.I8Ptr, panicMsgField)
	c.block.NewCall(c.palFree, gRaw)
	nullCapturedEnvField()
	c.block.NewCall(c.funcs["promise_panic"], panicMsg)
	c.emitPanicReturn()

	// loadResultBlk: normal path — load result, free G
	c.block = loadResultBlk
	var resultVal value.Value
	if !isVoid {
		rpField := c.block.NewGetElementPtr(gTy, gPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
		rpVal := c.block.NewLoad(irtypes.I8Ptr, rpField)
		typedRP := c.block.NewBitCast(rpVal, irtypes.NewPointer(resultLLVM))
		resultVal = c.block.NewLoad(resultLLVM, typedRP)
		// Free result buffer
		c.block.NewCall(c.palFree, rpVal)
	}

	// Free G struct
	c.block.NewCall(c.palFree, gRaw)
	nullCapturedEnvField()

	if isVoid {
		return nil
	}
	return resultVal
}

// genReceiveChannel generates code for `<-channel[T]` — returns T? (optional).
// lock → wait while empty && !closed → if closed+empty: return none → read value → return Some(value)
func (c *Compiler) genReceiveChannel(e *ast.UnaryExpr, inst *types.Instance) value.Value {
	chRaw := c.genExpr(e.Operand)

	elemType := inst.TypeArgs()[0]
	elemLLVM := c.resolveType(elemType)
	elemSize := int64(c.typeSize(elemLLVM))
	optType := irtypes.NewStruct(irtypes.I1, elemLLVM) // { i1, T }

	chanType := channelStructType()
	chPtr := c.block.NewBitCast(chRaw, irtypes.NewPointer(chanType))

	// Load mutex and cond vars
	mtxFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldMutex)))
	mtx := c.block.NewLoad(irtypes.I8Ptr, mtxFieldPtr)

	neFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotEmpty)))
	notEmpty := c.block.NewLoad(irtypes.I8Ptr, neFieldPtr)

	nfFieldPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldNotFull)))
	notFull := c.block.NewLoad(irtypes.I8Ptr, nfFieldPtr)

	// Lock
	c.block.NewCall(c.palMutexLock, mtx)

	// Wait while count == 0 && !closed
	waitBlock := c.newBlock("chrecv.wait")
	checkBlock := c.newBlock("chrecv.check")
	noneBlock := c.newBlock("chrecv.none")
	readBlock := c.newBlock("chrecv.read")
	doneBlock := c.newBlock("chrecv.done")

	c.block.NewBr(waitBlock)

	// wait: check count == 0 && !closed
	c.block = waitBlock
	countPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCount)))
	count := c.block.NewLoad(irtypes.I64, countPtr)
	isEmpty := c.block.NewICmp(enum.IPredEQ, count, constant.NewInt(irtypes.I64, 0))
	closedPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldClosed)))
	closedVal := c.block.NewLoad(irtypes.I8, closedPtr)
	isOpen := c.block.NewICmp(enum.IPredEQ, closedVal, constant.NewInt(irtypes.I8, 0))
	shouldWait := c.block.NewAnd(isEmpty, isOpen)

	waitBodyBlock := c.newBlock("chrecv.wait.body")
	c.block.NewCondBr(shouldWait, waitBodyBlock, checkBlock)

	if c.inCoroutine {
		// Goroutine mode: park on recv_waiters + coro.suspend
		c.block = waitBodyBlock
		currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
		recvHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersHead)))
		recvTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRecvWaitersTail)))
		c.block.NewCall(c.funcs["promise_waiter_enqueue"], recvHeadPtr, recvTailPtr, currentG)
		// Store mutex in G.park_mutex — scheduler releases after coro.suspend completes
		gTyRecv := goroutineStructType()
		recvGPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTyRecv))
		recvPmField := c.block.NewGetElementPtr(gTyRecv, recvGPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldParkMutex)))
		c.block.NewStore(mtx, recvPmField)

		suspResult := c.block.NewCall(c.coroSuspend, constant.None, constant.False)
		resumeBlk := c.newBlock("chrecv.wait.resume")
		c.block.NewSwitch(suspResult, c.coroSuspendBlk,
			ir.NewCase(constant.NewInt(irtypes.I8, 0), resumeBlk),
			ir.NewCase(constant.NewInt(irtypes.I8, 1), c.coroCleanupBlk))

		// On resume: re-lock and retry
		c.block = resumeBlk
		c.block.NewCall(c.palMutexLock, mtx)
		c.block.NewBr(waitBlock)
	} else {
		// Thread-blocking mode: cond_wait, loop
		c.block = waitBodyBlock
		c.block.NewCall(c.palCondWait, notEmpty, mtx)
		c.block.NewBr(waitBlock)
	}

	// check: if count == 0 && closed → none, else → read
	c.block = checkBlock
	countAgain := c.block.NewLoad(irtypes.I64, countPtr)
	stillEmpty := c.block.NewICmp(enum.IPredEQ, countAgain, constant.NewInt(irtypes.I64, 0))
	c.block.NewCondBr(stillEmpty, noneBlock, readBlock)

	// none: return { false, zeroinit }
	c.block = noneBlock
	c.block.NewCall(c.palMutexUnlock, mtx)
	noneVal := constant.NewZeroInitializer(optType)
	c.block.NewBr(doneBlock)

	// read: memcpy from buffer[head], advance head, count--, wake sender
	c.block = readBlock
	bufPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldBuffer)))
	buf := c.block.NewLoad(irtypes.I8Ptr, bufPtr)
	headPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldHead)))
	head := c.block.NewLoad(irtypes.I64, headPtr)
	offset := c.block.NewMul(head, constant.NewInt(irtypes.I64, elemSize))
	src := c.block.NewGetElementPtr(irtypes.I8, buf, offset)

	// Read value via alloca + memcpy (entry-block alloca to avoid stack growth in loops)
	resultAlloca := c.createEntryAlloca(elemLLVM)
	resultAsI8 := c.block.NewBitCast(resultAlloca, irtypes.I8Ptr)
	c.block.NewCall(c.funcs["llvm.memcpy"], resultAsI8, src,
		constant.NewInt(irtypes.I64, elemSize), constant.False)
	resultVal := c.block.NewLoad(elemLLVM, resultAlloca)

	// head = (head + 1) % capacity
	capPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldCapacity)))
	cap_ := c.block.NewLoad(irtypes.I64, capPtr)
	headPlusOne := c.block.NewAdd(head, constant.NewInt(irtypes.I64, 1))
	newHead := c.block.NewURem(headPlusOne, cap_)
	c.block.NewStore(newHead, headPtr)

	// count--
	countRead := c.block.NewLoad(irtypes.I64, countPtr)
	newCount := c.block.NewSub(countRead, constant.NewInt(irtypes.I64, 1))
	c.block.NewStore(newCount, countPtr)

	// Wake a waiting sender (handles both regular G and select SWN nodes)
	sendHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersHead)))
	sendTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldSendWaitersTail)))
	c.block.NewCall(c.funcs["promise_waiter_wake_one"], sendHeadPtr, sendTailPtr, notFull)

	// Wake a rendezvous-parked sender (T0312): count is now 0, so the sender's
	// rendezvous wait is complete. Waking rv_waiters lets it proceed without spinning.
	rvWakeHeadPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRvWaitersHead)))
	rvWakeTailPtr := c.block.NewGetElementPtr(chanType, chPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRvWaitersTail)))
	c.block.NewCall(c.funcs["promise_waiter_wake_one"], rvWakeHeadPtr, rvWakeTailPtr, notFull)

	// Unlock
	c.block.NewCall(c.palMutexUnlock, mtx)

	// Build Some: { true, value }
	someVal := c.block.NewInsertValue(constant.NewZeroInitializer(optType), constant.True, 0)
	someVal2 := c.block.NewInsertValue(someVal, resultVal, 1)
	someBlk := c.block // capture current block for phi predecessor
	c.block.NewBr(doneBlock)

	// done: phi to select none or some
	c.block = doneBlock
	phi := c.block.NewPhi(
		&ir.Incoming{X: noneVal, Pred: noneBlock},
		&ir.Incoming{X: someVal2, Pred: someBlk},
	)

	return phi
}
