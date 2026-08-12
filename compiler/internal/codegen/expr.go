package codegen

import (
	"fmt"

	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// genExpr generates LLVM IR for an expression and returns the resulting value.
func (c *Compiler) genExpr(expr ast.Expr) value.Value {
	if expr == nil {
		return nil
	}
	// T1421: synthetic node staged with a pre-evaluated value (optional-chain
	// present-path receiver). Return it verbatim before AST dispatch.
	if c.stagedExprValues != nil {
		if v, ok := c.stagedExprValues[expr]; ok {
			return v
		}
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
		// T0918/T0935: The early-return special forms (short-circuit &&/||,
		// elvis ?:, ranges) manage their own result ownership inside
		// genBinaryExpr and must NOT be tracked here. &&/||/ranges never return
		// i8* anyway, but elvis ?: can — and tracking a Vector-typed elvis result
		// via the string tracker below would call promise_string_drop on a vector
		// (T0935: frees a .rodata vector → `bad header magic`), while the
		// none-path borrowed default would be double-freed against its own owner.
		// genElvis registers its result with the correct, path-aware drop.
		switch e.Op {
		case ast.BinElvis, ast.BinAnd, ast.BinOr,
			ast.BinExclusiveRange, ast.BinInclusiveRange:
			return result
		}
		// B0168: Track string concatenation temporaries. Only string + returns
		// i8* from genBinaryExpr; comparisons return i1.
		// T0659: Defensive — borrow returns are never owned temps. Today only
		// string-concat hits I8Ptr here, but a future T&-returning user `+`
		// would; mirror the T0649 CallExpr guard.
		if result != nil {
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
			if result.Type() == irtypes.I8Ptr {
				c.trackStringTemp(result)
			} else if _, isSig := rt.(*types.Signature); isSig {
				// T1229: a user-defined operator returning a closure hands back a
				// fresh owned {fn,env} fat pointer; free its env when discarded.
				c.trackClosureOperatorResult(result)
			}
		}
		return result
	case *ast.UnaryExpr:
		result := c.genUnaryExpr(e)
		// T1229: a user-defined unary operator (`-a`) returning a closure hands
		// back a fresh owned {fn,env} fat pointer whose env must be freed when the
		// result is discarded. Non-operator unary results (`!b`→i1, numeric `-x`)
		// are never Signatures, so this is a no-op for them.
		if result != nil {
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
			if _, isSig := rt.(*types.Signature); isSig {
				c.trackClosureOperatorResult(result)
			}
		}
		return result
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
		// T1181: A call returning a fixed-size array T[N] hands back an LLVM
		// `[N x T]` aggregate by value. When used inline (never bound — e.g.
		// `mk()[0]`, `take(mk())`, `mk();`) nothing owns it, so its heap-allocating
		// elements (string/vector/heap-user) leak. Track it as an element-wise-drop
		// statement temp; a consuming binding claims it via claimStringTemp.
		// Sound whenever the returned array is independently owned (the normal
		// case). T1184: a function that returns a *borrowed* fixed-array param by
		// value (`echo(string[2] a) string[2] { return a; }`) is now made
		// independently owned at the return site — dupBorrowedHeapUserPayload
		// element-wise deep-clones a borrowed-array-param escape, so the returned
		// aggregate owns its elements and this inline temp-drop frees them exactly
		// once (the caller keeps and drops its own originals).
		if arr, ok := rt.(*types.Array); ok {
			if c.tempTrackingEnabled {
				elem := arr.Elem()
				if c.typeSubst != nil {
					elem = types.Substitute(elem, c.typeSubst)
				}
				if c.variantFieldNeedsDrop(elem) {
					c.trackArrayTemp(result, arr)
				}
			}
			return result
		}
		// T0073: Track known-safe string-producing calls (primitive to_string, string methods)
		// T0109: Also track vector-producing calls (e.g., split()) for cleanup.
		// T0555: Track native handle (Arc/Weak/Mutex/Task) constructor/call results
		// for cleanup at statement end — without this, expressions like
		// `take_arc(Ref[int](99))` leak because the param is borrowed and the
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
				} else if taskElem, isTask, taskFail := types.AsAnyTaskFailable(rt); isTask {
					c.trackTempWithDrop(result, c.getOrCreateTaskDrop(taskElem, taskFail))
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
		// T1029: an i8* result (string/vector/channel temp) produced anywhere inside
		// the discarded statement that aliases an owned-local arg — clear the
		// just-tracked result temp so the source local remains sole owner (freed once
		// at scope exit, not at statement end while still live). Uses discardedExpr
		// != nil (not e == discardedExpr) so sibling sub-call temps not propagated to
		// the discarded result are neutralized too. The heap-user-type case is
		// handled inside trackHeapUserTypeResult where the temp's SSA key is in hand.
		if c.discardedExpr != nil && len(c.discardAliasArgPtrs) > 0 && result != nil && result.Type() == irtypes.I8Ptr {
			if idx, ok := c.stmtTempMap[result]; ok && idx >= 0 {
				c.emitDiscardAliasClears(result, c.stmtTemps[idx].dropFlag)
			}
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
		// T0350: Same gap for Vector results. T0659: borrow returns are
		// skipped (never owned temps). Shared with `?!` and bare
		// auto-propagate-in-interpolation (T0966).
		c.trackUnwrappedFailableTemp(e, result)
		return result
	case *ast.ErrorPanicExpr:
		result := c.genErrorPanicExpr(e)
		// T0125: Track string temps from failable call panic paths.
		// When func()?! returns a string, the unwrapped i8* is a heap-allocated
		// temp that must be freed at statement end if not claimed by a variable.
		// T0350: Same gap for Vector results. T0659: borrow returns are skipped
		// (never owned temps). Shared with `?^` and bare
		// auto-propagate-in-interpolation (T0966).
		c.trackUnwrappedFailableTemp(e, result)
		return result
	case *ast.AutoCloneExpr:
		result := c.genAutoCloneExpr(e)
		// T0605: the cloned value is a fresh owned heap allocation. Mirror the
		// synth clone()-CallExpr result temp-tracking so the enclosing
		// Self(...) constructor claims ownership (no leak; no double-drop with
		// the owner's synth drop of the field).
		// T0659: Defensive — borrow returns are never owned temps. Auto-clone
		// of a borrow today produces a fresh value, but mirror the T0649 guard
		// for consistency with the other post-call tracking branches.
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
		if result != nil && result.Type() == irtypes.I8Ptr {
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
		// T0659: Defensive — borrow returns are never owned temps. Mirror the
		// T0649 CallExpr guard so future regressions on a borrow-return path
		// (e.g., a failable `T&` unwrapped with `!`) fail closed.
		exprType := c.info.Types[e]
		if c.typeSubst != nil && exprType != nil {
			exprType = types.Substitute(exprType, c.typeSubst)
		}
		if c.selfSubst != nil && exprType != nil {
			exprType = types.SubstituteSelf(exprType, c.selfSubst.iface, c.selfSubst.concrete)
		}
		if exprType != nil && isRefType(exprType) {
			return result
		}
		if result != nil && result.Type() == irtypes.I8Ptr {
			named := extractNamed(exprType)
			// B0287: For optional unwrap on ident source, the optional's
			// drop binding owns the inner. Don't track as a statement temp —
			// that would cause a double-free at scope exit. Peels ParenExpr so
			// `(o)!` is recognized like `o!` (otherwise `((o)!).field` double-frees).
			isIdentSource := isIdentOptionalUnwrapSource(e.Expr)
			// T1182 gap: a container/array-index borrow (`arr[i]!` / `vec[i]!`)
			// aliases the container's owned slot — genOptionalForceUnwrap records
			// this in optionalUnwrapContainerBorrow (still set when we get here).
			// Tracking the borrowed inner as a statement temp double-frees at scope
			// exit alongside the container's element drop (fatal "invalid free" on
			// macOS; silent over-free elsewhere). Mirrors the guard added inside
			// genOptionalForceUnwrap and trackHeapUserTypeResult's existing check.
			// T1215: a nested-Optional double-force (`r!!` with `r: T??`) resolves,
			// after peeling force layers, to an owned ident / owner-governed member
			// whose (recursive) drop governs the extracted inner — treat it like an
			// ident source so the string/vector inner is not double-freed at scope exit.
			nestedOwnerGoverned := c.isNestedOwnerGovernedUnwrapSource(e.Expr)
			if named == types.TypString {
				if c.optionalFieldString {
					c.optionalFieldString = false
				} else if !isIdentSource && !c.optionalUnwrapContainerBorrow && !nestedOwnerGoverned {
					c.trackStringTemp(result)
				}
			} else if named == types.TypVector {
				if c.optionalFieldVector {
					c.optionalFieldVector = false
				} else if !isIdentSource && !c.optionalUnwrapContainerBorrow && !nestedOwnerGoverned {
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
		// T0659: Defensive — failable borrow returns via `? e { ... }` reach
		// here only if the source allocates (T0649 Part 1 removed that today).
		// Mirror the T0649 CallExpr guard so future regressions fail closed.
		exprType := c.info.Types[e]
		if c.typeSubst != nil && exprType != nil {
			exprType = types.Substitute(exprType, c.typeSubst)
		}
		if c.selfSubst != nil && exprType != nil {
			exprType = types.SubstituteSelf(exprType, c.selfSubst.iface, c.selfSubst.concrete)
		}
		if exprType != nil && isRefType(exprType) {
			return result
		}
		if result != nil && result.Type() == irtypes.I8Ptr {
			named := extractNamed(exprType)
			// T0753: For the optional-handler unwrap (`o? _ { ... }`) on an ident
			// source, the source optional's own drop binding owns the inner
			// string/vector. Tracking the extracted i8* as a statement temp
			// double-frees at scope exit (mirrors the OptionalUnwrapExpr branch).
			isIdentSource := isIdentOptionalUnwrapSource(e.Expr)
			if named == types.TypString {
				if !isIdentSource {
					c.trackStringTemp(result)
				}
			} else if named == types.TypVector {
				if !isIdentSource {
					if elemType, ok := types.AsVector(exprType); ok {
						c.trackVectorTempWithElemType(result, elemType)
					} else {
						c.trackVectorTemp(result)
					}
				}
			} else if !isIdentSource && !c.isOwnerGovernedMemberOptionalUnwrapSource(e.Expr) {
				// T1085: Opaque i8*-backed container inners (Channel/Arc/Weak/
				// Mutex/Task/MutexGuard) from a handler on a non-ident,
				// non-owner-governed source are owned temporaries — the source
				// optional never separately tracks the inner, so the returned
				// okVal (diverging) or merged phi (non-diverging) is the sole
				// owner. Mirrors genOptionalForceUnwrap's dispatch. Ident sources
				// are handled in genOptionalHandlerExpr's T0778 block; owner-
				// governed member sources stay untracked (the owner's drop frees
				// the field on the present path).
				if arcElem, isArc := types.AsArc(exprType); isArc {
					c.trackTempWithDrop(result, c.getOrCreateArcDrop(arcElem))
				} else if weakElem, isWeak := types.AsWeak(exprType); isWeak {
					c.trackTempWithDrop(result, c.getOrCreateWeakDrop(weakElem))
				} else if mutexElem, isMutex := types.AsMutex(exprType); isMutex {
					c.trackTempWithDrop(result, c.getOrCreateMutexDrop(mutexElem))
				} else if taskElem, isTask, taskFail := types.AsAnyTaskFailable(exprType); isTask {
					c.trackTempWithDrop(result, c.getOrCreateTaskDrop(taskElem, taskFail))
				} else if _, isMG := types.AsMutexGuard(exprType); isMG {
					if dropFn, ok := c.funcs["MutexGuard.drop"]; ok {
						c.trackTempWithDrop(result, dropFn)
					}
				} else if chElem, isCh := types.AsChannel(exprType); isCh {
					c.trackChannelTempWithElemType(result, chElem)
				}
			}
		} else if _, isSig := exprType.(*types.Signature); isSig && result != nil {
			// T1235: an error-handler result of function type is an owned closure
			// whose heap env must be freed when the result is discarded. The result
			// is the ok-value closure (diverging handler) or a phi of ok/recovery
			// closures — both own a fresh env. Track field 1 (env ptr) as an env
			// temp so cleanupEnvTemps frees it; a binding claims it via claimEnvTemp
			// (var-decl RHS), so the bound path stays single-free. Ref-typed results
			// already returned early above. Guard ident / owner-governed
			// optional-handler sources whose source optional's own drop owns the env
			// (double-free) — mirrors the i8* string/vector branches.
			if !isIdentOptionalUnwrapSource(e.Expr) &&
				!c.isOwnerGovernedMemberOptionalUnwrapSource(e.Expr) {
				// A non-diverging handler whose recovery body is itself a capturing
				// closure already registered that recovery env as its own env temp
				// inside genBlockValue (which claims string/heap block results but
				// NOT env temps). On the recovery path the phi env aliases that temp,
				// so tracking the phi below would double-free (segfault) at statement
				// end. Claim the handler temp first (runtime pointer match) so the phi
				// env temp added below is the single owner; on the ok path the handler
				// env temp is null (handler block never ran) → the claim is a no-op.
				c.claimEnvTemp(result)
				envPtr := c.block.NewExtractValue(result, 1)
				c.trackEnvTemp(envPtr)
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
		// T0659: Defensive — a future borrow-returning user-defined `[:]` would
		// reach here as i8*. Mirror the T0649 CallExpr guard so future regressions
		// fail closed.
		if result != nil && result.Type() == irtypes.I8Ptr {
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
			if taskElem, isTask, taskFail := types.AsAnyTaskFailable(rt); isTask {
				c.trackTempWithDrop(result, c.getOrCreateTaskDrop(taskElem, taskFail))
			}
		}
		return result
	default:
		panic(fmt.Sprintf("codegen: unhandled expression type %T", expr))
	}
}
