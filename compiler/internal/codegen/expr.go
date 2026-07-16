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

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
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
				} else if taskElem, isTask := types.AsTask(exprType); isTask {
					c.trackTempWithDrop(result, c.getOrCreateTaskDrop(taskElem))
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

// trackOperatorResult registers the result of a non-native user-defined operator
// call as a heap temp (T0918) so an inline (unbound) heap user-type result is
// dropped at statement end — exactly like an ordinary method-call result.
// trackHeapUserTypeResult self-filters: it ignores scalars, value types, copy
// types, structural/container/string results, so this is a no-op for native
// operator results (scalars / value-type structs). Borrow returns (T&/T~) are
// skipped — they are never owned temps (mirrors the T0649 CallExpr guard).
// Returns result unchanged for use as a tail call.
//
// Tracking is placed at the operator-call sites rather than in genExpr's
// BinaryExpr/UnaryExpr dispatch so the early-return special forms (short-circuit
// &&/||, elvis ?:, ranges) are never tracked — their results may alias owned
// locals (e.g. the default operand of `optional ?: owned_local`), and tracking
// those would double-free at statement end.
func (c *Compiler) trackOperatorResult(e ast.Expr, result value.Value) value.Value {
	rt := c.resolvedExprType(e)
	if rt != nil && isRefType(rt) {
		return result
	}
	c.trackHeapUserTypeResult(e, result)
	return result
}

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
		if en := extractEnum(leftType); en != nil {
			// T0918: track the heap user-type result for inline (unbound) use.
			return c.trackOperatorResult(e, c.genEnumBinaryOp(e, en, leftType, left, right))
		}
		panic(fmt.Sprintf("codegen: cannot resolve Named type from %s for operator %s", leftType, e.Op))
	}

	op := e.Op.String()
	// Binary operator: select the 1-param variant so a type that also declares a
	// prefix-unary form of the same symbol (e.g. `-`) dispatches correctly (T0883).
	method := named.LookupBinaryMethod(op)
	if method == nil {
		method = named.LookupMethod(op)
	}
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
		// T0918: track the heap user-type result for inline (unbound) use.
		return c.trackOperatorResult(e, c.genVirtualBinaryOp(e, named, method, left, right))
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
		if isThisReceiver(e.Left) {
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
	if isThisReceiver(e.Right) {
		var paramIdx int
		if method.Sig().Recv() != nil {
			paramIdx = 1
		}
		if paramIdx < len(fn.Params) {
			if st, ok := fn.Params[paramIdx].Typ.(*irtypes.StructType); ok {
				if _, rightIsPtr := right.Type().(*irtypes.PointerType); rightIsPtr {
					rightType := c.info.Types[e.Right]
					if c.typeSubst != nil {
						rightType = types.Substitute(rightType, c.typeSubst)
					}
					if c.selfSubst != nil {
						rightType = types.SubstituteSelf(rightType, c.selfSubst.iface, c.selfSubst.concrete)
					}
					rightNamed := extractNamed(rightType)
					if rightNamed != nil && rightNamed.IsValueType() {
						// Value-type `this`: the receiver i8* points at the value
						// struct itself (see valueTypeReceiverPtr), so load the
						// param directly rather than synthesizing {vtable, instance}.
						valPtr := c.block.NewBitCast(right, irtypes.NewPointer(st))
						right = c.block.NewLoad(st, valPtr)
					} else {
						// Heap type: `this` i8* IS the instance pointer; wrap it
						// as {null_vtable, instance_ptr}.
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
	}
	args = append(args, right)
	result := value.Value(c.block.NewCall(fn, args...))
	if method.Sig().CanError() {
		// T0984: failable operator returns {ok, value, err}; unwrap and propagate
		// the error to the (sema-guaranteed failable) enclosing scope.
		result = c.genAutoPropagateValue(result)
	}
	// T0918: track the heap user-type result for inline (unbound) use.
	return c.trackOperatorResult(e, result)
}

// genEnumBinaryOp dispatches a user-defined binary operator declared on an enum
// (T0876). Enum operator methods receive the enum value via an i8* pointer
// (mirroring genEnumMethodCall), so the Named-type arg convention in
// genBinaryExpr does not apply.
func (c *Compiler) genEnumBinaryOp(e *ast.BinaryExpr, en *types.Enum, leftType types.Type, left, right value.Value) value.Value {
	op := e.Op.String()

	// Resolve the enum's mangled name (mono name for instances, monoCtx for
	// the origin enum inside a generic method body).
	enumName := en.Obj().Name()
	if inst, ok := leftType.(*types.Instance); ok {
		if _, ok := inst.Origin().(*types.Enum); ok {
			enumName = monoName(inst)
		}
	} else if c.monoCtx != nil {
		if origin, ok := c.monoCtx.origin.(*types.Enum); ok && en == origin {
			enumName = c.monoCtx.name
		}
	}

	// Binary operator: select the 1-param variant (T0883).
	method := en.LookupBinaryMethod(op)
	if method == nil {
		method = en.LookupMethod(op)
	}
	if method == nil {
		panic(fmt.Sprintf("codegen: no operator %q on enum %s", op, enumName))
	}
	mangledName := mangleMethodName(enumName, op, false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared enum operator method %s", mangledName))
	}

	// Receiver: pass an i8* pointer to the enum value.
	var args []value.Value
	if isThisReceiver(e.Left) {
		// `this` inside an enum method is already i8* pointing to the enum alloca.
		args = append(args, left)
	} else {
		alloca := c.entryBlock.NewAlloca(left.Type())
		alloca.SetName(c.uniqueLocalName("enum.this"))
		c.block.NewStore(left, alloca)
		args = append(args, c.block.NewBitCast(alloca, irtypes.I8Ptr))
	}

	// Operand: the method expects the enum value by value. If the right operand
	// is `this` (i8* receiver pointer), load the enum value from it.
	if isThisReceiver(e.Right) {
		if len(fn.Params) > 1 {
			if _, rightIsPtr := right.Type().(*irtypes.PointerType); rightIsPtr {
				valPtr := c.block.NewBitCast(right, irtypes.NewPointer(fn.Params[1].Typ))
				right = c.block.NewLoad(fn.Params[1].Typ, valPtr)
			}
		}
	}
	args = append(args, right)
	result := value.Value(c.block.NewCall(fn, args...))
	if method.Sig().CanError() {
		// T0984: unwrap the failable {ok, value, err} result and propagate the error.
		result = c.genAutoPropagateValue(result)
	}
	return result
}

// genNonNativeEnumCompoundOp dispatches a user-defined enum operator invoked by a
// compound assignment (`+=`, `-=`, etc.) where the operand type is an enum (T1015).
// Both operands are plain loaded values — neither is `this` — so this mirrors the
// non-`this` branch of genEnumBinaryOp combined with genNonNativeCompoundOp's
// failable handling. The result is NOT tracked as a statement temp: every
// genCompoundOp caller stores it into a location that takes ownership, so tracking
// here would double-free (matching genNonNativeCompoundOp's contract). A failable
// operator returns {ok, value, err}; the error is auto-propagated (sema guarantees
// the enclosing scope is failable via compoundOperatorCanError).
func (c *Compiler) genNonNativeEnumCompoundOp(en *types.Enum, operandType types.Type,
	op string, current, val value.Value) value.Value {

	// Resolve the enum's mangled name (mono name for instances, monoCtx for the
	// origin enum inside a generic method body) — same scheme as genEnumBinaryOp.
	enumName := en.Obj().Name()
	if inst, ok := operandType.(*types.Instance); ok {
		if _, ok := inst.Origin().(*types.Enum); ok {
			enumName = monoName(inst)
		}
	} else if c.monoCtx != nil {
		if origin, ok := c.monoCtx.origin.(*types.Enum); ok && en == origin {
			enumName = c.monoCtx.name
		}
	}

	// Binary operator: prefer the 1-param variant (T0883).
	method := en.LookupBinaryMethod(op)
	if method == nil {
		method = en.LookupMethod(op)
	}
	if method == nil {
		panic(fmt.Sprintf("codegen: no operator %q on enum %s for compound assignment", op, enumName))
	}
	mangledName := mangleMethodName(enumName, op, false)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared enum operator method %s", mangledName))
	}

	// Receiver: pass an i8* pointer to the enum value (neither operand is `this`).
	alloca := c.entryBlock.NewAlloca(current.Type())
	alloca.SetName(c.uniqueLocalName("enum.this"))
	c.block.NewStore(current, alloca)
	args := []value.Value{c.block.NewBitCast(alloca, irtypes.I8Ptr)}
	// Operand: the method expects the enum value by value.
	args = append(args, val)

	result := value.Value(c.block.NewCall(fn, args...))
	if method.Sig().CanError() {
		result = c.genAutoPropagateValue(result)
	}
	return result
}

// genVirtualBinaryOp dispatches a non-native binary operator through the vtable.
// Used when the static type is abstract or has children requiring virtual dispatch.
// Mirrors genVirtualMethodCall but uses pre-evaluated left/right operands.
func (c *Compiler) genVirtualBinaryOp(e *ast.BinaryExpr, named *types.Named,
	method *types.Method, left, right value.Value) value.Value {
	result := c.genVirtualBinaryOpValues(named, e.Op.String(), method, left, right, isThisReceiver(e.Left))
	if method.Sig().CanError() {
		// T0984: unwrap the failable {ok, value, err} result and propagate the
		// error. Done here (not in genVirtualBinaryOpValues, which is shared with
		// genNonNativeCompoundOp's own auto-propagate) to avoid double-unwrapping.
		result = c.genAutoPropagateValue(result)
	}
	return result
}

// genVirtualBinaryOpValues is the value-based core of genVirtualBinaryOp: it
// dispatches a non-native binary operator through the vtable given pre-evaluated
// operands. leftIsThis reports whether the left operand is the method receiver
// (`this`). Shared by genBinaryExpr (plain `a + b`) and genNonNativeCompoundOp
// (compound `a += b`, where neither operand is `this`), so the vtable dispatch
// logic lives in one place (T0715).
func (c *Compiler) genVirtualBinaryOpValues(named *types.Named, op string,
	method *types.Method, left, right value.Value, leftIsThis bool) value.Value {

	// Extract vtable and instance from left operand
	var vtableRaw, instance value.Value
	if leftIsThis {
		instance = left
		vtableRaw = c.loadVtablePtrFromInstance(left)
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

// genNonNativeCompoundOp dispatches a user-defined (non-native) binary operator
// invoked by a compound assignment (`+=`, `-=`, etc.) and returns the result
// value (T0715). Both operands are plain loaded values — neither is `this` — so
// the receiver/argument ABI is simpler than genBinaryExpr's: no `this`-receiver
// or `this`-argument special cases. The result is NOT tracked as a statement
// temp; every genCompoundOp caller stores it into a location that takes ownership
// (alloca, field, setter, or container slot), so tracking here would double-free
// (matching the native-path comment in genCompoundOp). A failable operator
// returns {ok, value, err}; the error is auto-propagated (sema guarantees the
// enclosing scope is failable via compoundOperatorCanError).
func (c *Compiler) genNonNativeCompoundOp(named *types.Named, operandType types.Type,
	method *types.Method, op string, current, val value.Value) value.Value {

	var result value.Value
	if c.needsVtable(named) {
		// Virtual dispatch when the operand type is abstract / structural / has
		// children. Neither operand is `this`.
		result = c.genVirtualBinaryOpValues(named, op, method, current, val, false)
	} else {
		// Direct dispatch: resolve the mangled name exactly as genBinaryExpr does
		// (mono name for generic instances, structural-default synthesis under the
		// concrete name, mono-parent resolution for inherited operators).
		ownerName := c.resolveMethodOwner(named, op)
		var mangledName string
		if ownerName != named.Obj().Name() {
			if structParent := c.findStructuralOwner(named, op); structParent != nil {
				concreteName := c.resolveTypeName(operandType)
				c.ensureDefaultMethodsSynthesized(named, structParent)
				mangledName = mangleMethodName(concreteName, op, false)
			} else {
				monoOwner := c.resolveMonoParentName(named, operandType, ownerName)
				mangledName = mangleMethodName(monoOwner, op, false)
			}
		} else {
			mangledName = mangleMethodName(c.resolveTypeName(operandType), op, false)
		}
		fn, ok := c.funcs[mangledName]
		if !ok {
			panic(fmt.Sprintf("codegen: undeclared operator method %s", mangledName))
		}

		var args []value.Value
		if method.Sig().Recv() != nil {
			if named.IsValueType() {
				args = append(args, c.valueTypeReceiverPtr(current, operandType))
			} else {
				args = append(args, c.extractInstancePtr(current))
			}
		}
		args = append(args, val)
		result = c.block.NewCall(fn, args...)
	}

	if method.Sig().CanError() {
		// Failable operator: unwrap the {ok, value, err} result, propagating the
		// error to the (sema-guaranteed failable) enclosing scope.
		result = c.genAutoPropagateValue(result)
	}
	return result
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
	if c.typeSubst != nil {
		operandType = types.Substitute(operandType, c.typeSubst)
	}
	if c.selfSubst != nil {
		operandType = types.SubstituteSelf(operandType, c.selfSubst.iface, c.selfSubst.concrete)
	}

	// T0918: track the heap user-type result for inline (unbound) use. Placed in
	// genUnaryExpr (not emitUnaryOpResult, which is shared with genIncDecTarget's
	// ++/-- statement targets) so only prefix-unary expression results are
	// tracked. The receive operator (<-) returned above and is never reached.
	return c.trackOperatorResult(e, c.emitUnaryOpResult(e.Op.String(), operandType, operand, isThisReceiver(e.Operand)))
}

// emitUnaryOpResult dispatches a unary operator (prefix `-`/`!`/`~` from
// genUnaryExpr, or `++`/`--` from genIncDecTarget) on operandType and returns
// the result value. isThis reports whether the operand is the method receiver
// (`this`). Centralizes the native/enum/virtual/direct dispatch so both the
// prefix-unary path (T0878) and the inc/dec path (T0880) share one
// implementation.
func (c *Compiler) emitUnaryOpResult(op string, operandType types.Type, operand value.Value, isThis bool) value.Value {
	named := extractNamed(operandType)
	if named == nil {
		// Enum operands dispatch via the i8*-receiver convention (T0878),
		// mirroring genEnumBinaryOp.
		if en := extractEnum(operandType); en != nil {
			return c.genEnumUnaryOp(op, en, operandType, operand, isThis)
		}
		panic(fmt.Sprintf("codegen: cannot resolve Named type from %s for unary %s", operandType, op))
	}

	// For unary ops, look up the 0-param method variant
	method := c.lookupUnaryMethod(named, op)
	if method == nil {
		panic(fmt.Sprintf("codegen: no unary method %q on type %s", op, named))
	}

	if method.IsNative() {
		return c.emitNativeOp(named, op, operand, nil)
	}

	// Non-native unary operator: dispatch as a method call (T0878), mirroring
	// genBinaryExpr's receiver handling but with no second operand.
	var result value.Value
	if c.needsVtable(named) {
		result = c.genVirtualUnaryOp(op, named, method, operand, isThis)
	} else {
		// Direct dispatch: call the concrete type's operator method. Resolve the
		// mangled name exactly as genBinaryExpr does (mono name for generic
		// instances, structural-default synthesis under the concrete name).
		ownerName := c.resolveMethodOwner(named, op)
		var mangledName string
		if ownerName != named.Obj().Name() {
			if structParent := c.findStructuralOwner(named, op); structParent != nil {
				concreteName := c.resolveTypeName(operandType)
				c.ensureDefaultMethodsSynthesized(named, structParent)
				mangledName = mangleMethodNameForMethod(concreteName, method)
			} else {
				monoOwner := c.resolveMonoParentName(named, operandType, ownerName)
				mangledName = mangleMethodNameForMethod(monoOwner, method)
			}
		} else {
			mangledName = mangleMethodNameForMethod(c.resolveTypeName(operandType), method)
		}
		fn, ok := c.funcs[mangledName]
		if !ok {
			panic(fmt.Sprintf("codegen: undeclared operator method %s", mangledName))
		}

		var args []value.Value
		if method.Sig().Recv() != nil {
			if isThis {
				args = append(args, operand)
			} else if named.IsValueType() {
				args = append(args, c.valueTypeReceiverPtr(operand, operandType))
			} else {
				args = append(args, c.extractInstancePtr(operand))
			}
		}
		result = c.block.NewCall(fn, args...)
	}

	if method.Sig().CanError() {
		// T0984: failable unary/inc-dec operator returns {ok, value, err}; unwrap
		// and propagate the error to the (sema-guaranteed failable) enclosing scope.
		// Shared by prefix `-`/`!`/`~` (genUnaryExpr) and `++`/`--` (genIncDecTarget).
		result = c.genAutoPropagateValue(result)
	}
	return result
}

// genEnumUnaryOp dispatches a user-defined unary operator declared on an enum
// (T0878 prefix, T0880 ++/--). Enum operator methods receive the enum value via
// an i8* pointer (mirroring genEnumBinaryOp), so the Named-type receiver
// convention in emitUnaryOpResult does not apply. isThis reports whether the
// operand is the method receiver.
func (c *Compiler) genEnumUnaryOp(op string, en *types.Enum, operandType types.Type, operand value.Value, isThis bool) value.Value {
	// Resolve the enum's mangled name (mono name for instances, monoCtx for the
	// origin enum inside a generic method body).
	enumName := en.Obj().Name()
	if inst, ok := operandType.(*types.Instance); ok {
		if _, ok := inst.Origin().(*types.Enum); ok {
			enumName = monoName(inst)
		}
	} else if c.monoCtx != nil {
		if origin, ok := c.monoCtx.origin.(*types.Enum); ok && en == origin {
			enumName = c.monoCtx.name
		}
	}

	method := en.LookupUnaryMethod(op)
	if method == nil {
		panic(fmt.Sprintf("codegen: no operator %q on enum %s", op, enumName))
	}
	mangledName := mangleMethodNameForMethod(enumName, method)
	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared enum operator method %s", mangledName))
	}

	// Receiver: pass an i8* pointer to the enum value. The original binding still
	// owns/drops its data — this borrow matches genEnumBinaryOp's convention.
	var args []value.Value
	if isThis {
		args = append(args, operand)
	} else {
		alloca := c.entryBlock.NewAlloca(operand.Type())
		alloca.SetName(c.uniqueLocalName("enum.this"))
		c.block.NewStore(operand, alloca)
		args = append(args, c.block.NewBitCast(alloca, irtypes.I8Ptr))
	}
	result := value.Value(c.block.NewCall(fn, args...))
	if method.Sig().CanError() {
		// T0984: unwrap the failable {ok, value, err} result and propagate the error.
		result = c.genAutoPropagateValue(result)
	}
	return result
}

// genVirtualUnaryOp dispatches a non-native unary operator through the vtable
// (T0878 prefix, T0880 ++/--). Used when the static type is abstract or has
// children requiring virtual dispatch. Mirrors genVirtualBinaryOp without a
// right operand. isThis reports whether the operand is the method receiver.
func (c *Compiler) genVirtualUnaryOp(op string, named *types.Named,
	method *types.Method, operand value.Value, isThis bool) value.Value {

	// Extract vtable and instance from the operand.
	var vtableRaw, instance value.Value
	if isThis {
		instance = operand
		vtableRaw = c.loadVtablePtrFromInstance(operand)
	} else {
		vtableRaw = c.extractVtablePtr(operand)
		instance = c.extractInstancePtr(operand)
	}

	// Index into the vtable via the method's own slot. For `-`/`!`/`~` this is the
	// unary ($unary) slot — distinct from the binary `-` slot (T0883); for
	// `++`/`--` it is the plain operator slot (T0880).
	slotIndex := named.VirtualSlotIndexForMethod(method)
	if slotIndex < 0 {
		panic(fmt.Sprintf("codegen: operator %s not in vtable for %s", op, named))
	}
	vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
	fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
		constant.NewInt(irtypes.I32, int64(slotIndex)))
	fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

	// Build the function type (i8* receiver only) and bitcast.
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
	funcType := irtypes.NewFunc(retType, paramTypes...)
	fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))

	var args []value.Value
	if method.Sig().Recv() != nil {
		args = append(args, instance)
	}
	return c.block.NewCall(fnTyped, args...)
}

// lookupUnaryMethod finds the 0-param variant of a method by name, walking
// is-parents and structural-interface parents (T0881) so inherited unary
// operators dispatch the same way binary operators do.
func (c *Compiler) lookupUnaryMethod(named *types.Named, op string) *types.Method {
	return named.LookupUnaryMethod(op)
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

// genFunctionMemberIndirectCall dispatches `member(...)` indirectly through the
// fat pointer the callee (a function-typed field or getter) yields, resolving the
// signature under the active mono subst. Shared by the Named (T1253) and enum
// (T1258) direct-call arms of genCallExpr.
func (c *Compiler) genFunctionMemberIndirectCall(e *ast.CallExpr, sig *types.Signature) value.Value {
	// Resolve the signature under the active mono subst so the indirect dispatch
	// sees concrete param/result types when the owner is a generic instance
	// (matches the T1251 module path).
	resolvedSig := sig
	if c.typeSubst != nil {
		if s, ok := types.Substitute(sig, c.typeSubst).(*types.Signature); ok {
			resolvedSig = s
		}
	}
	closure := c.genExpr(e.Callee) // field load, or getter call
	var argVals []value.Value
	for _, arg := range e.Args {
		argVals = append(argVals, c.genCallArgExpr(arg.Value))
	}
	origArgVals := argVals // T0331: pre-coercion for alias check
	argVals = c.coerceIndirectCallArgs(resolvedSig, e.Args, argVals)
	result := c.genIndirectCall(closure, resolvedSig, argVals)
	result = c.emitReturnAliasCheck(result, resolvedSig, e.Args, origArgVals, e) // T0331
	return result
}

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
				// T1251: A module-level getter whose return type is a function type
				// (`get adder() -> int`) — the trailing `()` invokes the *returned
				// closure*, not the getter. Sema records the member in ModuleGetters
				// and types `lib.adder()` as the closure's result. Evaluate the getter
				// (genMemberExpr → genModuleGetterCall materializes the {fn,env} fat
				// pointer and tracks its env for cleanup) then dispatch indirectly.
				// Without this the default arm treats `()` as the getter's own call and
				// returns the closure struct where its result value is expected.
				if c.info.ModuleGetters[member] {
					calleeType := c.info.Types[e.Callee]
					if c.typeSubst != nil {
						calleeType = types.Substitute(calleeType, c.typeSubst)
					}
					if sig, ok := calleeType.(*types.Signature); ok {
						closure := c.genExpr(e.Callee)
						var argVals []value.Value
						for _, arg := range e.Args {
							argVals = append(argVals, c.genCallArgExpr(arg.Value))
						}
						origArgVals := argVals // T0331: pre-coercion for alias check
						argVals = c.coerceIndirectCallArgs(sig, e.Args, argVals)
						result := c.genIndirectCall(closure, sig, argVals)
						result = c.emitReturnAliasCheck(result, sig, e.Args, origArgVals, e) // T0331
						return result
					}
				}
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
		// Function-typed field or getter call: `this._next()` where _next is a
		// () -> T? field, or `l.adder()` where adder is a getter whose return type
		// is a function type (T1253). In both cases the trailing `()` invokes the
		// closure the member yields, not a method — dispatch indirectly through the
		// fat pointer. Without the getter arm the `()` falls through to
		// genMethodCall and panics with "no method adder on type Lib". genExpr
		// routes the getter to genGetterCall, which materializes the {fn,env} fat
		// pointer and (T1253) tracks its env for cleanup at statement end.
		if sig, ok := c.info.Types[e.Callee].(*types.Signature); ok {
			memberTargetType := c.info.Types[member.Target]
			if c.typeSubst != nil {
				memberTargetType = types.Substitute(memberTargetType, c.typeSubst)
			}
			if c.selfSubst != nil {
				memberTargetType = types.SubstituteSelf(memberTargetType, c.selfSubst.iface, c.selfSubst.concrete)
			}
			if named := extractNamed(memberTargetType); named != nil {
				isField := named.LookupField(member.Field) != nil
				isGetter := !isField && named.LookupGetter(member.Field) != nil
				if isField || isGetter {
					return c.genFunctionMemberIndirectCall(e, sig)
				}
			} else if enum := extractEnum(memberTargetType); enum != nil {
				// T1258: enum analog of T1253 — an enum getter whose return type is
				// a function type, invoked directly (`e.adder()`). extractNamed is
				// nil for *types.Enum / enum *types.Instance, so this arm dispatches
				// the trailing () indirectly through the getter's fat pointer instead
				// of falling through to genMethodCall (which panics). genExpr(e.Callee)
				// routes to genEnumGetterAccess, which materializes the {fn,env}
				// pointer and tracks the env for cleanup. Enums have no fields-by-name,
				// so only the getter case applies.
				if enum.LookupGetter(member.Field) != nil {
					return c.genFunctionMemberIndirectCall(e, sig)
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
			// Arc constructor: Ref[T](value)
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

	// Generic function/method call: callee is IndexExpr (identity[int](42) or obj.method[int](42)).
	// T0674: Only route to the generic-instantiation path when the indexed target's
	// recorded type is a generic Signature (mirrors sema's rule in checkIndexExpr:
	// `[...]` is a type-argument list only when the target is a generic func/method).
	// A value subscript that yields a callable — e.g. fns[0](x) where fns is a
	// Vector[(int) -> int], or h.fns[0](x) for a function-typed field — is NOT a
	// generic instantiation; it falls through to the closure-value call path below
	// (T0817), which materializes the {fn, env} fat pointer and dispatches indirectly.
	// Without this gate, fns[0](x) mangled a bogus generic name ("fns[int]") and
	// panicked, and h.fns[0](x) mis-routed to genGenericMethodCall ("no method fns").
	if idx, ok := e.Callee.(*ast.IndexExpr); ok {
		targetType := c.info.Types[idx.Target]
		if c.typeSubst != nil && targetType != nil {
			targetType = types.Substitute(targetType, c.typeSubst)
		}
		sig, isSig := targetType.(*types.Signature)
		if isSig && len(sig.TypeParams()) > 0 {
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

	// T0817: Closure call through a non-ident callee whose static type is a
	// function signature — e.g. a force-unwrapped optional closure `o!()`, or a
	// parenthesized `(expr)()`. genExpr materializes the fat pointer {fn, env};
	// dispatch it through the same indirect-call path as any other closure. Ident
	// callees fall through to the locals-based lambda path below (it loads the fat
	// pointer from the alloca and handles return-alias checks).
	if _, isIdent := e.Callee.(*ast.IdentExpr); !isIdent {
		calleeType := c.info.Types[e.Callee]
		if c.typeSubst != nil {
			calleeType = types.Substitute(calleeType, c.typeSubst)
		}
		if sig, ok := calleeType.(*types.Signature); ok {
			closure := c.genExpr(e.Callee)
			var argVals []value.Value
			for _, arg := range e.Args {
				argVals = append(argVals, c.genCallArgExpr(arg.Value))
			}
			origArgVals := argVals // T0331: pre-coercion for alias check
			argVals = c.coerceIndirectCallArgs(sig, e.Args, argVals)
			result := c.genIndirectCall(closure, sig, argVals)
			result = c.emitReturnAliasCheck(result, sig, e.Args, origArgVals, e) // T0331
			return result
		}
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
		argVals, argTypes, variadicPTs = c.genCallArgsWithMutRef(e.Args, calleeSig.Params(), calleeSig.Result())
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
			argVals = c.coerceIndirectCallArgs(sig, e.Args, argVals)
			result := c.genIndirectCall(closure, sig, argVals)
			c.clearVariadicStaticFlags(variadicPTs)
			result = c.emitReturnAliasCheck(result, sig, e.Args, origArgVals, e) // T0331
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

	var result value.Value = c.block.NewCall(fn, argVals...)
	c.clearVariadicStaticFlags(variadicPTs)

	// B0345: If the return value aliases an argument, clear the argument's drop flag
	// to prevent double-free. E.g., identity(v) returns v's pointer — without this,
	// both v and the return value would be dropped at scope exit.
	result = c.emitReturnAliasCheck(result, calleeSig, e.Args, origArgVals, e)

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
		argVals, argTypes, variadicPTs = c.genCallArgsWithMutRef(e.Args, calleeSig.Params(), calleeSig.Result())
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

	var result value.Value = c.block.NewCall(fn, argVals...)
	c.clearVariadicStaticFlags(variadicPTs)
	result = c.emitReturnAliasCheck(result, calleeSig, e.Args, origArgVals, e) // B0345
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
	retType := c.info.Types[e]
	if c.typeSubst != nil && retType != nil {
		retType = types.Substitute(retType, c.typeSubst)
	}
	if c.selfSubst != nil && retType != nil {
		retType = types.SubstituteSelf(retType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	// T1240: A getter returning a function type (`get adder() -> int`) yields an
	// owned closure whose heap env must be freed. Track its env (field 1 of the
	// fat pointer) as an env temp so cleanupEnvTemps frees it when the result is
	// discarded; if it's bound to a variable, claimEnvTemp releases the temp and
	// maybeRegisterEnvFree takes over ownership (single free either way).
	if _, isSig := retType.(*types.Signature); isSig {
		envPtr := c.block.NewExtractValue(result, 1)
		c.trackEnvTemp(envPtr)
		return result
	}
	// T0137/T0486/T1250: track every heap-owning result kind (string, vector,
	// channel, Arc/Weak/Mutex/Task, heap user type) so a discarded temporary is
	// freed at statement end; the binding/assignment path claims it (claimStringTemp
	// / claimHeapTemp) so a single owner frees it either way. Shares the dispatch
	// with the instance-getter path (trackGetterResult) — the guards in the tracked
	// helpers make it a no-op for value/copy/static-container results.
	c.trackGetterResultByType(e, retType, result)
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
	result := sig.Result()
	if len(subst) > 0 {
		params = make([]*types.Param, len(sig.Params()))
		for i, p := range sig.Params() {
			np := types.NewParam(p.Name(), types.Substitute(p.Type(), subst), p.Ref())
			np.SetVariadic(p.IsVariadic())
			params[i] = np
		}
		result = types.Substitute(result, subst)
	}
	// T1233: pass the (substituted) result so genCallArgsWithMutRef can detect a
	// generator callee — its stream[T] param lifetime outlives the call statement.
	return c.genCallArgsWithMutRef(args, params, result)
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

	var result value.Value = c.block.NewCall(fn, argVals...)
	c.clearVariadicStaticFlags(variadicPTs)
	result = c.emitReturnAliasCheckSubst(result, calleeSig, e.Args, origArgVals, callSubst, e) // T0331/T0418
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

	var result value.Value = c.block.NewCall(fn, argVals...)
	c.clearVariadicStaticFlags(variadicPTs)
	result = c.emitReturnAliasCheckSubst(result, calleeSig, e.Args, origArgVals, callSubst, e) // T0331/T0418
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

	var result value.Value = c.block.NewCall(fn, argVals...)
	c.clearVariadicStaticFlags(variadicPTs)
	result = c.emitReturnAliasCheckSubst(result, calleeSig, e.Args, origArgVals, callSubst, e) // T0331/T0418
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
		if isThisReceiver(member.Target) {
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
	// T0418: Combine owner-type subst (Box[int].T → int + parents) with
	// method-level subst (transform[string].T → string).
	// T1223: compute the subst BEFORE evaluating args and route arg-gen through
	// genGenericCallArgs so genCallArgsWithMutRef sees CONCRETE param types. Without
	// this, a `T move` param (e.g. Set[string].add(T move elem)) reaches
	// maybeEnableDupForMutRefArg as the unsubstituted TypeParam `T`, so the field-read
	// dup that every move sink arms (T0366) is skipped — `out.add(this.label)` then
	// stores an alias of the owner's inner buffer into the set → UAF when the owner drops.
	ownerSubst := c.buildOwnerTypeArgSubst(targetType)
	methodSubst := c.buildInferredCallSubst(method.Sig().TypeParams(), typeArgs)
	combined := mergeSubstMaps(ownerSubst, methodSubst)
	argVals, argTypes, variadicPTs := c.genGenericCallArgs(e.Args, method.Sig(), combined)
	origArgVals := argVals // B0345: save pre-coercion values
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params(), e.Args, combined)
	args = append(args, argVals...)

	var result value.Value = c.block.NewCall(fn, args...)
	c.clearVariadicStaticFlags(variadicPTs)
	result = c.emitReturnAliasCheckSubst(result, method.Sig(), e.Args, origArgVals, combined, e) // B0345/T0418
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
		if isThisReceiver(member.Target) {
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
			if c.freshEnumReceiverNeedsDrop(member.Target) && !enumCtorTracked {
				tempEnumPtr = ptr
			}
		}
	}

	// T0418/T0636: owner-enum subst (Box[int].T → int) merged with the
	// method-level subst (transform[string].U → string).
	// T1223: compute the subst BEFORE evaluating args and route arg-gen through
	// genGenericCallArgs so genCallArgsWithMutRef sees CONCRETE param types (a raw
	// `T move` param would otherwise skip the field-read dup every move sink arms).
	var ownerSubst map[*types.TypeParam]types.Type
	if inst, ok := targetType.(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Enum); ok && len(origin.TypeParams()) > 0 {
			ownerSubst = types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
		}
	}
	methodSubst := c.buildInferredCallSubst(method.Sig().TypeParams(), typeArgs)
	combined := mergeSubstMaps(ownerSubst, methodSubst)
	argVals, argTypes, variadicPTs := c.genGenericCallArgs(e.Args, method.Sig(), combined)
	origArgVals := argVals // B0345
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params(), e.Args, combined)
	args = append(args, argVals...)

	var result value.Value = c.block.NewCall(fn, args...)
	c.clearVariadicStaticFlags(variadicPTs)
	result = c.emitReturnAliasCheckSubst(result, method.Sig(), e.Args, origArgVals, combined, e) // B0345/T0418

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

	// For a generic parent (`Child[T] is Base[T]`), resolve the inheritance
	// type args under the current substitution so we can both name the
	// monomorphized parent constructor and coerce the args (T0474).
	parentRef := named.Parents()[0]
	var resolvedParentArgs []types.Type
	if len(parentRef.TypeArgs) > 0 && len(parent.TypeParams()) > 0 {
		resolvedParentArgs = make([]types.Type, len(parentRef.TypeArgs))
		for i, ta := range parentRef.TypeArgs {
			if c.typeSubst != nil {
				ta = types.Substitute(ta, c.typeSubst)
			}
			resolvedParentArgs[i] = ta
		}
	}

	// Load the this pointer
	thisAlloca := c.locals["this"]
	thisPtr := c.block.NewLoad(irtypes.I8Ptr, thisAlloca)

	if parent.HasNew() {
		// Parent has explicit new() — call ParentType.new(this, args...)
		parentName := parent.Obj().Name()
		if resolvedParentArgs != nil {
			parentName = monoName(types.NewInstance(parent, resolvedParentArgs))
		}
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
			// T0754: clear cast subject's drop flag — ownership moves it at
			// the owning-slot store (super()'s parent constructor takes the
			// arg into the parent's field), so the subject must not also drop.
			// T0849: for the conditional `as` form, drop iff the downcast failed.
			if ident := c.castSubjectMovableIdent(arg.Value); ident != nil {
				c.consumeCastSubjectDropFlag(arg.Value, ident.Name)
			}
		}
		newMethod := parent.LookupMethod("new")
		if newMethod != nil {
			// T0418/T0474: Build subst for parent's TypeParams from the resolved
			// inheritance type args (e.g., `type Foo[T] is Bar[T]` → Bar.T → resolved T).
			var superSubst map[*types.TypeParam]types.Type
			if resolvedParentArgs != nil {
				superSubst = types.BuildSubstMap(parent.TypeParams(), resolvedParentArgs)
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
			// T0754: clear cast subject's drop flag — ownership moves it at
			// the owning-slot store (parent implicit-ctor field-init), so
			// the subject must not also drop at scope exit.
			// T0849: for the conditional `as` form, drop iff the downcast failed.
			if ident := c.castSubjectMovableIdent(arg.Value); ident != nil {
				c.consumeCastSubjectDropFlag(arg.Value, ident.Name)
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
				// T0754: clear cast subject's drop flag — ownership moves it
				// at the owning-slot store, so the subject's scope-exit drop
				// must not fire on the same allocation new() now owns.
				// T0849: for the conditional `as` form, drop iff the cast failed.
				if ident := c.castSubjectMovableIdent(arg.Value); ident != nil {
					c.consumeCastSubjectDropFlag(arg.Value, ident.Name)
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
			// T1139: gate the clear on the arg's static type being an enum — only
			// then is a tracked enum-ctor temp the value actually being moved. A
			// non-enum arg that merely BORROWS an inline Enum.V(x) temp in a
			// sub-call leaves an intermediate the callee never receives; it must
			// stay tracked so the caller drops it at statement end, else it leaks.
			// Residual: an enum-typed arg produced by a call that internally
			// borrows a *different* enum-ctor temp still over-claims that inner
			// temp (the whole range is cleared rather than just the moved value's
			// backing temp) — lower priority, contrived nesting.
			if isMoveParam {
				argEnumType := c.info.Types[arg.Value]
				if c.typeSubst != nil {
					argEnumType = types.Substitute(argEnumType, c.typeSubst)
				}
				if extractEnum(argEnumType) != nil {
					for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
						c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
					}
					c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
				}
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
					c.dupHeapUserFieldAccess = false // T0847
					c.targetType = nil
					// T1174: `Wrapper(held: maybe)` where maybe is a match-borrowed
					// Optional[heap-user] binding aliases the subject's variant payload;
					// deep-clone the inner so the new instance owns an independent copy.
					val, _ = c.dupBorrowedHeapUserPayload(arg.Value, val)
				}
			} else {
				// T0411: Auto-dup string/container fields read from a droppable
				// owner so the new instance gets an independent copy.
				c.maybeEnableDupForConstructorArg(arg.Value, fieldTypeMap[arg.Name])
				val = c.genCallArgExpr(arg.Value)
				c.dupStringFieldAccess = false
				c.dupContainerFieldAccess = false
				c.dupHeapUserFieldAccess = false // T0847
				// T1174: match-borrowed Optional[heap-user] binding used to
				// initialize an owned field — deep-clone (see the optional-field
				// branch above).
				val, _ = c.dupBorrowedHeapUserPayload(arg.Value, val)
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
				// T0754: clear cast subject's drop flag — for string field
				// init via `name as! S` shapes, ownership moves the subject at
				// this owning-slot store. The branches below handle bare ident
				// RHS; cover the cast-RHS shape here.
				if castIdent := c.castSubjectMovableIdent(arg.Value); castIdent != nil {
					// T0849: for the conditional `as` form, drop iff the cast failed.
					c.consumeCastSubjectDropFlag(arg.Value, castIdent.Name)
				}
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
				// T0754: clear cast subject's drop flag — ownership moves it
				// at the owning-slot store, so the subject's scope-exit drop
				// must not fire on the same allocation the field now owns.
				// T0849: for the conditional `as` form, drop iff the cast failed.
				if ident := c.castSubjectMovableIdent(arg.Value); ident != nil {
					c.consumeCastSubjectDropFlag(arg.Value, ident.Name)
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
				// T0741: For an optional-closure field `(() -> T)? cb`, val is the
				// wrapped {i1, {fn,env}} optional, which claimEnvTemp can't match —
				// claim the bare pre-wrap closure {fn,env} so its env is owned by
				// the field (otherwise cleanupEnvTemps frees it early → dangling).
				c.claimEnvTemp(preWrapVal)
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
			// T1139: gate on the arg's static type being an enum — a non-enum field
			// arg that merely BORROWS an inline Enum.V(x) temp in a sub-call leaves
			// an intermediate the field never owns; it must stay tracked so the
			// caller drops it at statement end, else it leaks. Residual: an
			// enum-typed field arg produced by a call that internally borrows a
			// *different* enum-ctor temp still over-claims it (whole range cleared)
			// — lower priority, contrived nesting.
			argEnumType := c.info.Types[arg.Value]
			if c.typeSubst != nil {
				argEnumType = types.Substitute(argEnumType, c.typeSubst)
			}
			if extractEnum(argEnumType) != nil {
				for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
					c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
				}
				c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
			}
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
	// T0993: non-destructive enum variant field read — `if x is V { x.namedField }`.
	// Sema recorded the variant + field index; emit a variant-data GEP+load.
	if access := c.info.NarrowedVariantField[e]; access != nil {
		return c.genNarrowedVariantField(e, access)
	}

	// Module-level getter: mod.property → call getter function with no args.
	// Guard: only intercept when sema actually resolved this member as a getter
	// (recorded in info.ModuleGetters). Keying on sema's resolution rather than
	// the result type's shape is required because a getter whose return type is
	// itself a function type (`get adder() -> int`) has a Signature result type
	// — the old "result is a Signature ⇒ function reference" heuristic
	// misclassified it, fell through to the type-based path, and panicked on the
	// module target's nil type (T1240).
	if _, isGetter := c.info.ModuleGetters[e]; isGetter {
		if ident, ok := e.Target.(*ast.IdentExpr); ok {
			if modName := c.resolveModuleName(ident); modName != "" {
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
	// T0155: Ref[T] atomic reference counting.
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

// arcStructType returns the LLVM struct type for Ref[T]: {i64 strong_count, i64 weak_count, T value}.
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

// genArcConstructor generates Ref[T](value) — allocates {strong_count, weak_count, T}, stores counts=1 and the value.
// T0155: Ref[T] atomic reference counting. T0157: weak_count added.
func (c *Compiler) genArcConstructor(e *ast.CallExpr, inst *types.Instance) value.Value {
	elemType := inst.TypeArgs()[0]
	if c.typeSubst != nil {
		elemType = types.Substitute(elemType, c.typeSubst)
	}
	elemLLVM := c.resolveType(elemType)
	elemSize := c.typeSize(elemLLVM)
	_, elemIsOpt := elemType.(*types.Optional) // T0853

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
	// T0853: when the element type is Optional, set targetType so a bare `none`
	// arg lowers to a zero {i1,T} struct via genNoneLit (mirrors Vector.push, T0658).
	savedTarget := c.targetType
	if elemIsOpt {
		c.targetType = elemType
	}
	// T1003: track enum ctor temps created while evaluating the moved-in value so
	// their statement-end drop is suppressed — the value is owned by the Arc now,
	// dropped via the Arc's inner-drop when the last strong ref is released.
	savedEnumTemps := len(c.enumCtorTemps)
	val := c.genCallArgExpr(e.Args[0].Value)
	c.targetType = savedTarget
	c.claimHeapTemp(val)
	// T0273: Clear drop flag — value is moved into Arc, caller must not double-drop.
	if ident, ok := e.Args[0].Value.(*ast.IdentExpr); ok {
		c.clearDropFlag(ident.Name)
	}
	// T0784: also clear when the arg is `x as!/as T` over an owned local —
	// the cast is a non-consuming view, so without this the subject and the
	// new Arc both drop the same allocation. T0849: for the conditional `as`
	// form, drop iff the downcast failed.
	if ident := c.castSubjectMovableIdent(e.Args[0].Value); ident != nil {
		c.consumeCastSubjectDropFlag(e.Args[0].Value, ident.Name)
	}
	c.neutralizeForceUnwrapSource(e.Args[0].Value)
	c.claimStringTemp(val)
	c.claimEnvTemp(val)
	// T0853: widen a bare non-optional `T` arg to the `T?` element struct. Done
	// last (after temp-claiming) because stmtTempMap tracks by val-identity,
	// which is lost once val is wrapped. wrapReturnOptional doubles as the
	// constructor-arg widener: it no-ops for `none` (targetType already zeroed
	// it) and for an already-optional arg (types.Identical), else wrapOptional.
	if elemIsOpt {
		val = c.wrapReturnOptional(val, e.Args[0].Value, elemType)
	}
	valField := c.block.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldValue))
	c.block.NewStore(val, valField)

	// T1003: suppress statement-end drop of enum ctor temps moved into the Arc.
	// T1139: gate on the moved value's static type being an enum — a non-enum
	// arg that merely BORROWS an inline Enum.V(x) temp in a sub-call leaves an
	// intermediate the Arc never owns; it must stay tracked so the caller drops
	// it at statement end, else it leaks.
	argEnumType := c.info.Types[e.Args[0].Value]
	if c.typeSubst != nil {
		argEnumType = types.Substitute(argEnumType, c.typeSubst)
	}
	if extractEnum(argEnumType) != nil {
		for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
		}
		c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
	}

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
		return c.genChannelSend(e, chRaw, chPtr, chanType, elemType, elemLLVM, elemSize)
	case "close":
		return c.genChannelClose(chRaw, chPtr, chanType)
	default:
		panic(fmt.Sprintf("codegen: unknown channel method %q", method))
	}
}

// genArcBorrow generates the Arc .borrow getter — loads and returns the inner T value.
// The Arc layout is { i64 strong_count, i64 weak_count, T value }. We GEP to field 2 and load the value.
// T0155: Ref[T] atomic reference counting. T0157: weak_count shifted value to field 2.
func (c *Compiler) genArcBorrow(e *ast.MemberExpr, elemType types.Type) value.Value {
	arcRaw := c.genExprAutoPropagate(e.Target) // B0323
	elemLLVM := c.resolveType(elemType)
	arcStructTy := arcStructType(elemLLVM)
	typedPtr := c.block.NewBitCast(arcRaw, irtypes.NewPointer(arcStructTy))
	valField := c.block.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldValue))
	return c.block.NewLoad(elemLLVM, valField)
}

// genArcMethodCall dispatches native method calls on Ref[T].
// T0155: Ref[T] atomic reference counting. T0157: downgrade added.
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
		// Increment strong_count and return the same pointer (non-atomic when
		// the element type is `confined — T0995).
		rcPtr := c.block.NewBitCast(arcRaw, irtypes.NewPointer(irtypes.I64))
		c.emitRefCountAdd(c.block, rcPtr, 1, irtypes.I64, c.refIsAtomic(elemType))
		// T0499: Return a distinct SSA value so the clone result can be tracked
		// separately from the receiver's stmtTemp. Without this, stmtTemp dedup
		// causes the constructor intermediate to leak when used in a chain
		// (e.g., Ref[int](42).clone()). The ptrtoint+inttoptr is a no-op at
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
	c.emitRefCountAdd(c.block, wcField, 1, irtypes.I64, c.refIsAtomic(elemType))
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
		c.emitRefCountAdd(c.block, wcField, 1, irtypes.I64, c.refIsAtomic(elemType))
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

// genWeakUpgrade generates Weak.upgrade() — CAS loop on strong_count, returns Ref[T]?.
// T0157: Returns {i1, i8*} optional — Some(arc_ptr) if strong_count > 0, none otherwise.
func (c *Compiler) genWeakUpgrade(weakRaw value.Value, elemType types.Type) value.Value {
	elemLLVM := c.resolveType(elemType)
	arcStructTy := arcStructType(elemLLVM)
	optType := irtypes.NewStruct(irtypes.I1, irtypes.I8Ptr)

	typedPtr := c.block.NewBitCast(weakRaw, irtypes.NewPointer(arcStructTy))
	scField := c.block.NewGetElementPtr(arcStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, arcFieldStrong))

	if c.isWasm || !c.refIsAtomic(elemType) {
		// WASM (single-threaded) or a `confined Ref (T0995): no atomics needed —
		// simple load+compare+store.
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
	_, elemIsOpt := elemType.(*types.Optional) // T0853

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
	// T0853: when the element type is Optional, set targetType so a bare `none`
	// arg lowers to a zero {i1,T} struct via genNoneLit (mirrors Vector.push, T0658).
	savedTarget := c.targetType
	if elemIsOpt {
		c.targetType = elemType
	}
	// T1003: track enum ctor temps created while evaluating the moved-in value so
	// their statement-end drop is suppressed — the value is owned by the Mutex now,
	// dropped via the Mutex's inner-drop when the Mutex is dropped.
	savedEnumTemps := len(c.enumCtorTemps)
	val := c.genCallArgExpr(e.Args[0].Value)
	c.targetType = savedTarget
	c.claimHeapTemp(val)
	// T0273: Clear drop flag — value is moved into Mutex, caller must not double-drop.
	if ident, ok := e.Args[0].Value.(*ast.IdentExpr); ok {
		c.clearDropFlag(ident.Name)
	}
	// T0784: also clear when the arg is `x as!/as T` over an owned local.
	// T0849: for the conditional `as` form, drop iff the downcast failed.
	if ident := c.castSubjectMovableIdent(e.Args[0].Value); ident != nil {
		c.consumeCastSubjectDropFlag(e.Args[0].Value, ident.Name)
	}
	c.neutralizeForceUnwrapSource(e.Args[0].Value)
	c.claimStringTemp(val)
	c.claimEnvTemp(val)
	// T0853: widen a bare non-optional `T` arg to the `T?` element struct. Done
	// last (after temp-claiming) because stmtTempMap tracks by val-identity,
	// which is lost once val is wrapped. wrapReturnOptional doubles as the
	// constructor-arg widener: it no-ops for `none` (targetType already zeroed
	// it) and for an already-optional arg (types.Identical), else wrapOptional.
	if elemIsOpt {
		val = c.wrapReturnOptional(val, e.Args[0].Value, elemType)
	}
	valField := c.block.NewGetElementPtr(mutexStructTy, typedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldValue)))
	c.block.NewStore(val, valField)

	// T1003: suppress statement-end drop of enum ctor temps moved into the Mutex.
	// T1139: gate on the moved value's static type being an enum — a non-enum
	// arg that merely BORROWS an inline Enum.V(x) temp in a sub-call leaves an
	// intermediate the Mutex never owns; it must stay tracked so the caller drops
	// it at statement end, else it leaks.
	argEnumType := c.info.Types[e.Args[0].Value]
	if c.typeSubst != nil {
		argEnumType = types.Substitute(argEnumType, c.typeSubst)
	}
	if extractEnum(argEnumType) != nil {
		for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
		}
		c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
	}

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

// genMutexGuardMethodCall dispatches native method calls on MutexGuard[T] (T0839).
// close(~this): unlock the mutex and free the guard via the canonical unlock+free
// body (MutexGuard.drop), then suppress the automatic scope-exit/stmt-temp drop so
// the guard isn't double-freed/double-unlocked.
func (c *Compiler) genMutexGuardMethodCall(e *ast.CallExpr, member *ast.MemberExpr, method string) value.Value {
	switch method {
	case "close":
		guardRaw := c.genExprAutoPropagate(member.Target) // B0323
		// Same body as MutexGuard.drop: scheduler-aware unlock + free guard (T0156).
		// It null-checks internally, so an already-null guard is safe.
		c.block.NewCall(c.funcs["MutexGuard.drop"], guardRaw)
		// The guard is consumed. Suppress later automatic cleanup:
		//  - bound source (`g := m.lock(); g.close();`): clear the drop binding flag.
		//  - temp/chain source (`m.lock().close()`, `(h.mtx!).lock().close()`): release
		//    the stmt-temp tracking. Both calls are no-ops when not applicable.
		if ident, ok := member.Target.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
		c.claimStringTemp(guardRaw)
		return nil // close(~this) returns void (cf. genChannelClose)
	default:
		panic(fmt.Sprintf("codegen: unknown MutexGuard method %q", method))
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
		// Non-coroutine mode: wait loop
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
		if c.isWasm {
			// T1218: pump the cooperative scheduler instead of the no-op pal_cond_wait
			// so a non-coroutine mutex waiter (e.g. a named fn spawned via `go`) yields
			// to the holder G; on progress branch back to the loop header to recheck
			// `held`. Sibling of the T1200 channel fix.
			c.emitWasmCoopWaitPump(waitLoopBlk)
		} else {
			// Thread-blocking mode: cond_wait, then re-check held.
			condField := c.block.NewGetElementPtr(metaTy, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(mutexFieldCond)))
			cond := c.block.NewLoad(irtypes.I8Ptr, condField)
			c.block.NewCall(c.palCondWait, cond, handle)
			c.block.NewBr(waitLoopBlk)
		}

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

// emitWasmCoopWaitPump emits the WASM non-coroutine blocking-op wait body
// (T1200 channels, T1218 mutex).
//
// On single-threaded WASM pal_cond_wait and pal_mutex_lock/unlock are no-ops, so
// a plain (non-coroutine) function's blocking wait — the classic
// `wait_body: cond_wait; br recheck` loop — busy-spins forever without ever
// yielding to the cooperative scheduler. This happens whenever a blocking op
// (a channel send/recv, or a contended Mutex[T].lock()) runs inside a function
// that is NOT itself a coroutine, e.g. a named top-level function spawned via
// `go worker(c)`: the goroutine coroutine just calls the single, non-coroutine
// `@__user.worker`, whose send/recv/lock takes this thread-blocking branch. The
// partner G (the holder that would release, or the peer that would send) never
// runs (livelock, zero progress) and the per-test deadline (checked in
// promise_sched_coop_step's ran_g) never fires because coop_step is never
// re-entered.
//
// Fix: pump one cooperative step instead of the no-op wait, exactly mirroring the
// Task-receive (genReceiveTask) and Task-drop (defineTaskDropBody) WASM spins
// (T0668/T0687). promise_sched_coop_step returns i8:
//
//	2 = per-test deadline reached → clean early-return from this (non-coroutine)
//	    function, unwinding so the outer coop_step/coop_run regains control and
//	    renders TIMEOUT. The op is abandoned (result discarded, drops skipped —
//	    the test is being torn down; a timed-out test result==2 skips the leak
//	    check, matching the Task-spin "G intentionally not freed" precedent).
//	non-zero = a G ran (progress possible) → re-evaluate the wait condition.
//	0 = no runnable G and the condition is still unmet → nothing can ever change
//	    it → genuine deadlock → terminal message + exit(2) (same as coop_run).
//
// Must be called with c.block == the wait-body block. On progress it branches to
// recheck; the caller resumes building at recheck (which re-tests the condition).
func (c *Compiler) emitWasmCoopWaitPump(recheck *ir.Block) {
	stepR := c.block.NewCall(c.funcs["promise_sched_coop_step"])
	isTimeout := c.block.NewICmp(enum.IPredEQ, stepR, constant.NewInt(irtypes.I8, 2))
	timeoutBlk := c.newBlock("chwait.timeout")
	progressBlk := c.newBlock("chwait.progress")
	c.block.NewCondBr(isTimeout, timeoutBlk, progressBlk)

	// timeout: clean early-return from the non-coroutine function (panicExitBlock
	// is nil here — a plain function, not a coroutine body — so a ret is valid).
	c.block = timeoutBlk
	if _, isVoid := c.fn.Sig.RetType.(*irtypes.VoidType); isVoid {
		c.block.NewRet(nil)
	} else {
		c.block.NewRet(c.zeroValue(c.fn.Sig.RetType))
	}

	// progress vs deadlock
	c.block = progressBlk
	deadlockBlk := c.newBlock("chwait.deadlock")
	madeProgress := c.block.NewICmp(enum.IPredNE, stepR, constant.NewInt(irtypes.I8, 0))
	c.block.NewCondBr(madeProgress, recheck, deadlockBlk)

	// deadlock: no runnable G and condition unmet — terminal (mirrors coop_run).
	c.block = deadlockBlk
	dlMsg := c.getTaskDeadlockMsgGlobal()
	dlMsgPtr := c.block.NewGetElementPtr(dlMsg.ContentType, dlMsg,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	c.block.NewCall(c.palWrite, constant.NewInt(irtypes.I32, 2), dlMsgPtr,
		constant.NewInt(irtypes.I64, 45))
	c.block.NewCall(c.palExit, constant.NewInt(irtypes.I32, 2))
	c.block.NewUnreachable()
}

// genChannelSend generates code for ch.send(value).
// lock → wait-if-full → memcpy to buffer → signal → rendezvous wait if unbuffered → unlock
func (c *Compiler) genChannelSend(e *ast.CallExpr, chRaw value.Value, chPtr value.Value, chanType *irtypes.StructType, elemType types.Type, elemLLVM irtypes.Type, elemSize int64) value.Value {
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
	} else if c.isWasm {
		// T1200: pump the cooperative scheduler instead of the no-op cond_wait, then
		// re-check the closed flag (as the cond_wait path does) before re-testing full.
		c.block = waitFullBodyBlock
		wfRecheck := c.newBlock("send.waitfull.recheck")
		c.emitWasmCoopWaitPump(wfRecheck)
		c.block = wfRecheck
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
	// T1221: send takes ownership (`T move value`) but memcpy's the raw value with
	// no clone. When the arg is a field read on a droppable owner (out.send(this.label)
	// / out.send(b.label)), the buffered pointer aliases the owner's inner buffer — the
	// owner's drop then frees a value the channel still owns → UAF/double-free. Arm the
	// same dup-on-read the general `~`/move-param call path uses (T0366), then clear the
	// flags (mirrors genCallArgsWithMutRef). For a plain owned local this is a no-op, so
	// the existing move-and-clear behavior below is preserved.
	c.maybeEnableDupForMutRefArg(e.Args[0].Value, elemType)
	argVal := c.genCallArgExpr(e.Args[0].Value)
	c.dupStringFieldAccess = false
	c.dupContainerFieldAccess = false
	c.dupHeapUserFieldAccess = false
	// T1174 parity: deep-clone a match-borrowed Optional[heap-user] payload alias.
	argVal, _ = c.dupBorrowedHeapUserPayload(e.Args[0].Value, argVal)
	// Clear drop flag: value is moved into the channel buffer
	if ident, ok := e.Args[0].Value.(*ast.IdentExpr); ok {
		c.clearDropFlag(ident.Name)
	}
	// T0784: also clear when the arg is `x as!/as T` over an owned local.
	// T0849: for the conditional `as` form, drop iff the downcast failed.
	if ident := c.castSubjectMovableIdent(e.Args[0].Value); ident != nil {
		c.consumeCastSubjectDropFlag(e.Args[0].Value, ident.Name)
	}
	// B0170: claim string temp — ownership transfers to channel buffer
	c.claimStringTemp(argVal)
	// B0233: claim heap temp — ownership transfers to channel buffer
	c.claimHeapTemp(argVal)
	// T1221: when the arg is an Optional[string]/Optional[container] field dup
	// (out.send(this.maybe_label)), the inner dup pointer is tracked separately —
	// claim it so the caller's statement cleanup doesn't double-free the value the
	// channel now owns. Mirrors genCallArgsWithMutRef's move-param handling (T0522).
	if c.optionalStringDup != nil {
		c.claimStringTemp(c.optionalStringDup)
		c.optionalStringDup = nil
	}
	if c.optionalContainerDup != nil {
		c.claimStringTemp(c.optionalContainerDup)
		c.optionalContainerDup = nil
	}
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
	} else if c.isWasm {
		// T1200: pump the cooperative scheduler instead of the no-op cond_wait so a
		// non-coroutine sender (e.g. a named fn spawned via `go`) yields to its
		// receiver; on progress recheck the rendezvous condition.
		c.block = rendezvousWaitBlock
		c.emitWasmCoopWaitPump(rendezvousCheckBlock)
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
	// T0778: Inside a monomorphized method body the receiver type can surface as
	// the bare generic Named (e.g. GH[T]) rather than a concrete Instance, so the
	// Instance branch above is skipped. NeedsSynthDrop on the generic Named is
	// false — its field types still contain TypeParams (sema's fieldTypeHasDrop
	// returns false for TypeParam). Resolve through the active mono context: if
	// `named` is the origin of the instance currently being specialized, ask
	// whether THAT instance needs a synth drop (its substituted fields are
	// droppable). Without this, a borrowed-field read in a generic method whose
	// field substitutes to a droppable type (`return this.s`, `this.o!`,
	// `(this.o)? _ {...}` for string/vector) skips the field-access dup, so the
	// owner's synth drop and the returned alias both free the inner → double-free
	// (`fatal: invalid free`). Mirrors the monoCtx fallback in lookupTypeLayout.
	if c.monoCtx != nil && c.monoCtx.inst != nil && named != nil {
		if origin, ok := c.monoCtx.origin.(*types.Named); ok && origin == named {
			return monoInstNeedsSynthDrop(c.monoCtx.inst)
		}
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
		if isThisReceiver(e.Target) {
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
	if isThisReceiver(e.Target) {
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
				// T1215: An inherited generic field (e.g. `T? value` inherited
				// from `Box[T]` into `Counter[T] is Box[T]`) carries the PARENT's
				// type param (Box.T), which the child-only subst above does not
				// cover — so the field type stayed an unresolved TypeParam, the
				// escape dup was skipped, and the unwrapped binding aliased the
				// field. The owner's drop and the binding's drop then double-freed
				// the heap value ("fatal: invalid free (bad header magic)" on
				// macOS). Merge the inherited type-param mappings so the parent's
				// param resolves transitively (Box.T → Counter.T → string).
				mergeParentSubst(origin, localSubst)
				fType = types.Substitute(fType, localSubst)
			}
		}
		ownerNamed := extractNamed(typ)
		ownerDroppable := c.ownerHasOrSynthDrop(typ, ownerNamed)
		// Dup string/container fields from a droppable owner when an escape flag is
		// set (shared with the narrowed-enum-variant path, T1011).
		if dup, ok := c.dupHeapFieldForEscape(val, fType, ownerDroppable); ok {
			return dup
		}
		// B0190: Signal that this field access loaded a string? field from a
		// droppable type. genOptionalForceUnwrap's result should NOT be tracked
		// as a temp (the owner's drop handles the string's lifetime).
		if opt, ok := fType.(*types.Optional); ok && extractNamed(opt.Elem()) == types.TypString && ownerDroppable {
			c.optionalFieldString = true
		}
		// T0354: Same for vector fields — the owner's drop frees the inner Vector
		// via optfield.drop. Suppress unwrap-path stmt-temp tracking to avoid
		// double-free at statement end.
		if opt, ok := fType.(*types.Optional); ok && ownerDroppable {
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

// currentNamedType returns the concrete receiver type of the method currently
// being compiled, resolving generic type parameters. Prefers the mono instance
// (generic-owner method bodies) so `Box[int]` resolves rather than the unbound
// `Box[T]`; otherwise substitutes any active typeSubst into the owner Named.
// Returns nil outside a method context. Used by genGoBlock (T1219) to snapshot
// `this` for a `go { }` block capture.
func (c *Compiler) currentNamedType() types.Type {
	if c.monoCtx != nil && c.monoCtx.inst != nil {
		return c.monoCtx.inst
	}
	if c.currentNamed == nil {
		return nil
	}
	if c.typeSubst != nil {
		return types.Substitute(c.currentNamed, c.typeSubst)
	}
	return c.currentNamed
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
	if method == nil && c.selfSubst != nil {
		// T0766: Inside a synthesized structural default-method body, `this`
		// (Self → concrete) may invoke a *sibling* default method that is declared
		// on the interface, not on the concrete type's own method table. Resolve it
		// through the interface and make sure the per-concrete synthesized function
		// exists; dispatch below then mangles to `<concrete>.<method>`.
		if im := c.selfSubst.iface.LookupMethod(member.Field); im != nil {
			c.ensureDefaultMethodsSynthesized(c.selfSubst.concrete, c.selfSubst.iface)
			method = im
		}
	}
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
		if isThisReceiver(member.Target) {
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
	// T0418: Build owner-type subst (e.g., Box[int].T → int) so generic
	// methods on a generic instance see TypeParam-typed params resolved.
	// T1223: compute the subst BEFORE evaluating args and route arg-gen through
	// genGenericCallArgs so genCallArgsWithMutRef sees CONCRETE param types. Without
	// this, a `T move` param (e.g. Set[string].add(T move elem)) reaches
	// maybeEnableDupForMutRefArg as the unsubstituted TypeParam `T`, so the field-read
	// dup that every move sink arms (T0366) is skipped — `out.add(this.label)` then
	// stores an alias of the owner's inner buffer into the set → UAF when the owner drops.
	ownerSubst := c.buildOwnerTypeArgSubst(targetType)
	argVals, argTypes, variadicPTs := c.genGenericCallArgs(e.Args, method.Sig(), ownerSubst)
	origArgVals := argVals // B0345
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params(), e.Args, ownerSubst)
	args = append(args, argVals...)

	var result value.Value = c.block.NewCall(fn, args...)
	c.clearVariadicStaticFlags(variadicPTs)
	result = c.emitReturnAliasCheckSubst(result, method.Sig(), e.Args, origArgVals, ownerSubst, e) // B0345/T0418
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
	if isThisReceiver(e.Target) {
		ptr = target
	} else {
		alloca := c.entryBlock.NewAlloca(target.Type())
		alloca.SetName(c.uniqueLocalName("enum.getter"))
		c.block.NewStore(target, alloca)
		ptr = c.block.NewBitCast(alloca, irtypes.I8Ptr)
		// T0660: a borrow-return receiver (`Tagged&`/`Tagged~`) aliases the
		// owner's payload; dropping the synthesized getter receiver temp
		// would double-free what the owner still frees at scope exit.
		if c.freshEnumReceiverNeedsDrop(e.Target) && !enumCtorTracked {
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

	// T0879: Register the getter result for cleanup at statement end, matching
	// genGetterCall / genVirtualGetterCall. Without this, string/vector/etc.
	// results used as unbound temporaries (inline ==, call arg) leak.
	c.trackGetterResult(e, getter, targetType, result)
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
		if isThisReceiver(member.Target) {
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
			if c.freshEnumReceiverNeedsDrop(member.Target) && !enumCtorTracked {
				tempEnumPtr = ptr
			}
		}
	}
	// T0418: Build owner-enum subst so generic methods on a generic enum
	// instance see TypeParam-typed params resolved.
	// T1223: compute the subst BEFORE evaluating args and route arg-gen through
	// genGenericCallArgs so genCallArgsWithMutRef sees CONCRETE param types (a raw
	// `T move` param would otherwise skip the field-read dup every move sink arms).
	var enumSubst map[*types.TypeParam]types.Type
	if inst, ok := targetType.(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Enum); ok && len(origin.TypeParams()) > 0 {
			enumSubst = types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
		}
	}
	argVals, argTypes, variadicPTs := c.genGenericCallArgs(e.Args, method.Sig(), enumSubst)
	origArgVals := argVals // B0345
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params(), e.Args, enumSubst)
	args = append(args, argVals...)

	var result value.Value = c.block.NewCall(fn, args...)
	c.clearVariadicStaticFlags(variadicPTs)
	result = c.emitReturnAliasCheckSubst(result, method.Sig(), e.Args, origArgVals, enumSubst, e) // B0345/T0418

	// Drop temp enum receiver if it was a fresh temporary not tracked by the
	// enumCtorTemps mechanism (e.g. an enum returned from a call, not an inline
	// `Enum.Variant(...)` constructor — inline constructors are tracked
	// unconditionally per T1108 and dropped at statement end instead).
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

// freshEnumReceiverNeedsDrop reports whether a non-`this` enum method/getter
// receiver expression yields a *fresh, owned* enum value whose synthesized stack
// temp must be dropped after the call (zero-leak policy).
//   - isFreshEnumExpr shapes (call results, deep clones, unwraps thereof),
//     excluding borrow-return receivers (Tagged&/Tagged~) that alias the owner (T0660).
//   - T1165: a force-/panic-unwrap (or direct read) of a user-defined *non-native*
//     `[]` (e.g. `m[k]!` on a Map) — Map.[] returns `V?` by deep-cloning the slot,
//     so the unwrapped enum is uniquely owned and leaks unless dropped. Native
//     container/array indexing is excluded by isUserIndexExpr (those alias storage,
//     so dropping the temp would double-free).
func (c *Compiler) freshEnumReceiverNeedsDrop(expr ast.Expr) bool {
	if isFreshEnumExpr(expr) {
		return !c.isBorrowedExpr(expr)
	}
	e := unwrapDestructureParens(expr)
	switch u := e.(type) {
	case *ast.OptionalUnwrapExpr:
		e = unwrapDestructureParens(u.Expr)
	case *ast.ErrorPanicExpr:
		e = unwrapDestructureParens(u.Expr)
	}
	return c.isUserIndexExpr(e) && !c.isBorrowedExpr(e)
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
	if isThisReceiver(e.Target) {
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
	if isThisReceiver(e.Target) {
		instance = receiverVal
		vtableRaw = c.loadVtablePtrFromInstance(receiverVal)
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
// substitution (e.g., ArcCell[int].fresh's `Ref[T]` → `Ref[int]`) so the
// per-element-type drop function looks up the concrete instantiation rather
// than the unsubstituted TypeParam.
func (c *Compiler) trackGetterResult(e *ast.MemberExpr, getter *types.Method, targetType types.Type, result value.Value) {
	if !c.tempTrackingEnabled || result == nil {
		return
	}
	// T1253/T1160: An instance getter whose return type is a function type
	// (`get adder() -> int`) yields an owned closure whose heap env must be
	// freed. Track its env (field 1 of the {fn,env} fat pointer) as an env temp
	// so cleanupEnvTemps frees it when the result is discarded (e.g. `(l.adder)()`
	// or `l.adder();`); if it's bound to a variable, claimEnvTemp releases the
	// temp and maybeRegisterEnvFree takes over ownership (single free either way).
	// Mirrors the module-getter arm in genModuleGetterCall (T1240).
	//
	// T1160: a getter can also hand back a closure it does NOT own — `get callback()
	// -> int { return this.cb; }` returns an alias of the receiver's field, whose env
	// the receiver's drop frees. Tracking that env here would free it twice. The
	// shared alias filter suppresses tracking for those receivers; the cost is the
	// pre-existing leak for a genuinely-fresh closure returned by such a type (T1229),
	// never a double free.
	if typ := c.info.Types[e]; typ != nil {
		if _, isSig := typ.(*types.Signature); isSig {
			if !c.closureResultMayAliasCallInput(e) {
				envPtr := c.block.NewExtractValue(result, 1)
				c.trackEnvTemp(envPtr)
			}
			return
		}
	}
	retType := getter.Sig().Result()
	// Owner-type subst: when the getter's owner is a generic instance
	// (e.g. ArcCell[int]), resolve the owner's TypeParams against the
	// instance's TypeArgs before applying any further substitution.
	// Without this, Ref[T] from ArcCell[T].fresh's signature stays as
	// Ref[T] and getOrCreateArcDrop(T) would produce an Ref[T].drop fn
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
	c.trackGetterResultByType(e, retType, result)
}

// trackGetterResultByType registers a getter result value for statement-end
// cleanup based on its (already substitution-resolved) result type, covering
// every heap-owning result kind that is passed by value: string, vector,
// channel, Arc/Weak/Mutex/Task, and heap user type. Callers must handle
// Signature (closure-env) results themselves before calling. Shared by
// trackGetterResult (instance getters) and genModuleGetterCall (module getters)
// so both free discarded temporaries of every kind identically — without this,
// a module getter returning a heap vector/channel/Arc used as a bare temporary
// leaked (only its heap-user-type case was covered by the original T1250 fix).
// The binding/assignment path claims the temp (claimStringTemp for stmtTemps,
// claimHeapTemp for heap-user-type instances), so a single owner frees it.
func (c *Compiler) trackGetterResultByType(e *ast.MemberExpr, retType types.Type, result value.Value) {
	if result.Type() == irtypes.I8Ptr {
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
			// T0486: Ref[T] getter result owns a heap allocation; without
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

// trackReceivedTaskResult registers the heap result of a task-handle receive
// (`<-t`, `t : task[T]`) as a droppable statement temp so it is freed at
// statement end when the value is consumed inline (e.g. `out.push(<-t)`,
// `(<-t).len`, `(<-t) + "!"`) rather than bound to a named variable. When the
// receive flows into a binding/move site, the existing claim-on-consume sites
// (binding RHS, call-arg move, match-arm phi) clear the flag, so this integrates
// with the working named-binding path without double-free risk. T1150.
//
// innerType is the already-substituted task element type (inst.TypeArgs()[0] in
// genReceiveTask, where inst comes from the substituted operand type), so no
// further typeSubst/selfSubst is applied. Mirrors trackGetterResult's dispatch;
// the underlying track* helpers all guard on tempTrackingEnabled, a terminated
// block, and i8*-typed results, so this is safe to call unconditionally.
func (c *Compiler) trackReceivedTaskResult(result value.Value, innerType types.Type) {
	if !c.tempTrackingEnabled || result == nil || innerType == nil {
		return
	}
	if result.Type() == irtypes.I8Ptr {
		named := extractNamed(innerType)
		if named == types.TypString {
			c.trackStringTemp(result)
		} else if named == types.TypVector {
			if elemType, ok := types.AsVector(innerType); ok {
				c.trackVectorTempWithElemType(result, elemType)
			} else {
				c.trackVectorTemp(result)
			}
		} else if chElem, isCh := types.AsChannel(innerType); isCh || named == types.TypChannel {
			c.trackChannelTempWithElemType(result, chElem)
		} else if arcElem, isArc := types.AsArc(innerType); isArc {
			c.trackTempWithDrop(result, c.getOrCreateArcDrop(arcElem))
		} else if weakElem, isWeak := types.AsWeak(innerType); isWeak {
			c.trackTempWithDrop(result, c.getOrCreateWeakDrop(weakElem))
		} else if mutexElem, isMutex := types.AsMutex(innerType); isMutex {
			c.trackTempWithDrop(result, c.getOrCreateMutexDrop(mutexElem))
		} else if taskElem, isTask := types.AsTask(innerType); isTask {
			c.trackTempWithDrop(result, c.getOrCreateTaskDrop(taskElem))
		}
		return
	}
	// {i8*, i8*} value struct → heap user type. Mirror trackHeapUserTypeResult's
	// tail filters so pure-value/copy/structural/primitive/container results are
	// skipped (those don't own a separate heap allocation to drop here).
	st, ok := result.Type().(*irtypes.StructType)
	if !ok || len(st.Fields) != 2 || st.Fields[0] != irtypes.I8Ptr || st.Fields[1] != irtypes.I8Ptr {
		return
	}
	named := extractNamed(innerType)
	if named == nil {
		return
	}
	if named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) || named.IsStructural() {
		return
	}
	if isContainerType(innerType) || named == types.TypString {
		return
	}
	dropFunc := c.resolveDropFuncForTemp(named, innerType)
	if dropFunc == nil {
		return
	}
	c.trackHeapTemp(c.block.NewExtractValue(result, 1), dropFunc)
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
	if isThisReceiver(member.Target) {
		// `this` is already i8* — load vtable from typeinfo chain
		instance = receiverVal
		vtableRaw = c.loadVtablePtrFromInstance(receiverVal)
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
	// T0418: vtableSubst (with parent subst merged) covers both the static
	// type's TypeParams and inherited parent-type TypeParams.
	// T1223: route arg-gen through genGenericCallArgs so genCallArgsWithMutRef sees
	// CONCRETE param types (a raw `T move` param would otherwise skip the field-read
	// dup every move sink arms).
	argVals, argTypes, variadicPTs := c.genGenericCallArgs(e.Args, method.Sig(), vtableSubst)
	origArgVals := argVals // B0345
	argVals = c.coerceCallArgs(argVals, argTypes, method.Sig().Params(), e.Args, vtableSubst)
	args = append(args, argVals...)
	var result value.Value = c.block.NewCall(fnTyped, args...)
	c.clearVariadicStaticFlags(variadicPTs)
	result = c.emitReturnAliasCheckSubst(result, method.Sig(), e.Args, origArgVals, vtableSubst, e) // B0345/T0418
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

	// MutexGuard methods: close (T0839). The `borrow` get/set are getter/setter
	// property accesses handled in genMemberExpr, not method calls; drop is
	// automatic. close is the only user-callable method reachable here.
	if _, ok := types.AsMutexGuard(unwrapped); ok {
		return c.genMutexGuardMethodCall(e, member, methodName), true
	}
	if named == types.TypMutexGuard {
		return c.genMutexGuardMethodCall(e, member, methodName), true
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

// getOrEmitOptContainsEqFn returns a comparison function (ABI: i32(i8*,i8*,i64))
// for Optional scalar element types in vector.contains. Instead of memcmp, the
// generated function compares the i1 presence flag and then (if both are Some)
// the inner scalar value using icmp/fcmp — completely ignoring the struct's
// padding bytes (bytes 1..7 of {i1, i64}, bytes 1..3 of {i1, i32}, etc.).
//
// Why this matters: LLVM O1 decomposes `store { i1, T } zeroinitializer` into
// per-field stores covering only the defined fields. The 1..7 padding bytes get
// no explicit store and remain uninitialized stack memory. O1 then replaces
// promise_vector_contains's memcmp with a single `icmp ne i128`, comparing all
// 16 bytes of the slot. When push and contains are in different functions (e.g.
// the cross-function generic test), their independent O1 contexts produce
// different stack garbage in those padding bytes → icmp finds inequality even
// when the logical Optional values are equal.
//
// Returns constant.NewNull(irtypes.I8Ptr) for non-scalar inner types (heap
// types, strings, nested Optional, value-type structs), which fall back to the
// existing memcmp path. The function is cached in c.funcs by name to avoid
// duplicate definitions.
func (c *Compiler) getOrEmitOptContainsEqFn(optLLVM irtypes.Type) value.Value {
	st, ok := optLLVM.(*irtypes.StructType)
	if !ok || len(st.Fields) < 2 {
		return constant.NewNull(irtypes.I8Ptr)
	}
	innerLLVM := st.Fields[1]

	// Only scalar inner types: fall back to memcmp for complex types (pointers,
	// structs, nested Optional, strings) where identity/equality semantics differ.
	var isFloat bool
	switch innerLLVM.(type) {
	case *irtypes.IntType:
		isFloat = false
	case *irtypes.FloatType:
		isFloat = true
	default:
		return constant.NewNull(irtypes.I8Ptr)
	}

	fnName := "__promise_opt_eq_" + innerLLVM.String()
	if fn, exists := c.funcs[fnName]; exists {
		return c.block.NewBitCast(fn, irtypes.I8Ptr)
	}

	// Emit: i32 fnName(i8* a, i8* b, i64 _ksz)
	// Returns 1 if equal (same presence + same inner value), 0 otherwise.
	aParam := ir.NewParam("a", irtypes.I8Ptr)
	bParam := ir.NewParam("b", irtypes.I8Ptr)
	kszParam := ir.NewParam("_ksz", irtypes.I64)
	fn := c.module.NewFunc(fnName, irtypes.I32, aParam, bParam, kszParam)
	c.funcs[fnName] = fn

	i32zero := constant.NewInt(irtypes.I32, 0)
	i32one := constant.NewInt(irtypes.I32, 1)
	gepOuter := constant.NewInt(irtypes.I32, 0)
	gep0 := constant.NewInt(irtypes.I32, 0)
	gep1 := constant.NewInt(irtypes.I32, 1)

	entry := fn.NewBlock(".entry")
	flagsMatch := fn.NewBlock("flags.match")
	compareInner := fn.NewBlock("compare.inner")
	retTrue := fn.NewBlock("ret.true")
	retFalse := fn.NewBlock("ret.false")

	// entry: cast i8* → {i1,T}*, load flags, branch on equality
	ap := entry.NewBitCast(aParam, irtypes.NewPointer(st))
	bp := entry.NewBitCast(bParam, irtypes.NewPointer(st))
	aFlagPtr := entry.NewGetElementPtr(st, ap, gepOuter, gep0)
	bFlagPtr := entry.NewGetElementPtr(st, bp, gepOuter, gep0)
	aFlag := entry.NewLoad(irtypes.I1, aFlagPtr)
	bFlag := entry.NewLoad(irtypes.I1, bFlagPtr)
	flagsEq := entry.NewICmp(enum.IPredEQ, aFlag, bFlag)
	entry.NewCondBr(flagsEq, flagsMatch, retFalse)

	// flags.match: flags are equal; if a_flag=false → both None → equal
	flagsMatch.NewCondBr(aFlag, compareInner, retTrue)

	// compare.inner: both Some — compare the inner scalar value
	aValPtr := compareInner.NewGetElementPtr(st, ap, gepOuter, gep1)
	bValPtr := compareInner.NewGetElementPtr(st, bp, gepOuter, gep1)
	aVal := compareInner.NewLoad(innerLLVM, aValPtr)
	bVal := compareInner.NewLoad(innerLLVM, bValPtr)
	var valsEq value.Value
	if isFloat {
		valsEq = compareInner.NewFCmp(enum.FPredOEQ, aVal, bVal)
	} else {
		valsEq = compareInner.NewICmp(enum.IPredEQ, aVal, bVal)
	}
	compareInner.NewCondBr(valsEq, retTrue, retFalse)

	retTrue.NewRet(i32one)
	retFalse.NewRet(i32zero)

	return c.block.NewBitCast(fn, irtypes.I8Ptr)
}

func (c *Compiler) genVectorMethodCall(e *ast.CallExpr, member *ast.MemberExpr, elemType types.Type, method string) value.Value {
	// T0595: capture the receiver slot once. `receiverSlot` is non-nil only for an
	// arr[i]/vov[i] receiver; the push/pop/remove store-back below writes through
	// it instead of re-evaluating the index. Held in a local (not a Compiler field)
	// so a nested vector method call during argument evaluation can't clobber it.
	slicePtr, receiverSlot := c.evalVectorReceiver(member.Target)
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
		// T0741: track enum ctor temps created while evaluating the pushed
		// element. When the element is moved (not dup'd) into the vector, the
		// vector becomes the sole owner, so these temps must be cleared — else
		// the temp's stmt-end synth drop and the vector element drop both free
		// the variant data (e.g. a closure env in a Vector[enum-with-closure]
		// element → double-free). Mirrors genVectorLit / genFixedArrayLit.
		savedEnumTemps := len(c.enumCtorTemps)
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
			// T0741: claim closure env — ownership transfers to vector; the
			// vector's element-drop loop now frees each pushed closure's env.
			c.claimEnvTemp(argVal)
			// T0741: when moved (not dup'd) into the vector, clear enum ctor
			// temps created during arg eval so the temp's synth drop doesn't
			// also free the variant data the vector element now owns.
			// T1139: gate on the element's static type being an enum — a non-enum
			// element arg that merely BORROWS an inline Enum.V(x) temp in a
			// sub-call leaves an intermediate the vector never owns; it must stay
			// tracked so the caller drops it at statement end, else it leaks.
			if !dupped {
				argEnumType := c.info.Types[e.Args[0].Value]
				if c.typeSubst != nil {
					argEnumType = types.Substitute(argEnumType, c.typeSubst)
				}
				if extractEnum(argEnumType) != nil {
					for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
						c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
					}
					c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
				}
			}
		}
		// COW: if static (.rodata), copy to heap first (T0062)
		cowSlice := c.block.NewCall(c.funcs["promise_vector_cow"],
			slicePtr, constant.NewInt(irtypes.I64, elemSize))
		argAlloca := c.createEntryAlloca(elemLLVM)
		// Zero-initialize before store to clear padding bytes for memcmp correctness
		c.block.NewStore(constant.NewZeroInitializer(elemLLVM), argAlloca)
		// T0661: For Optional element types {i1, T}, use field stores to preserve
		// the zeroinit padding bytes (7 bytes between i1 and T for i64 alignment).
		// A full struct store of a value built by insertvalue-from-undef carries
		// undefined padding that overwrites zeroinit. Must mirror the contains-side
		// field stores so memcmp agrees in all cases (inline and cross-function).
		if elemIsOpt {
			if st, ok := elemLLVM.(*irtypes.StructType); ok {
				zero32 := constant.NewInt(irtypes.I32, 0)
				one32 := constant.NewInt(irtypes.I32, 1)
				i1Val := c.block.NewExtractValue(argVal, 0)
				innerVal := c.block.NewExtractValue(argVal, 1)
				f0Ptr := c.block.NewGetElementPtr(st, argAlloca, zero32, zero32)
				f1Ptr := c.block.NewGetElementPtr(st, argAlloca, zero32, one32)
				c.block.NewStore(i1Val, f0Ptr)
				c.block.NewStore(innerVal, f1Ptr)
			}
		} else {
			c.block.NewStore(argVal, argAlloca)
		}
		argPtr := c.block.NewBitCast(argAlloca, irtypes.I8Ptr)
		newSlice := c.block.NewCall(c.funcs["promise_vector_push"],
			cowSlice, argPtr, constant.NewInt(irtypes.I64, elemSize))
		// Store the (possibly reallocated) pointer back
		c.storeVectorReceiverBack(member.Target, receiverSlot, newSlice)
		return newSlice

	case "pop":
		// COW: if static (.rodata), copy to heap first (T0062)
		cowSlice := c.block.NewCall(c.funcs["promise_vector_cow"],
			slicePtr, constant.NewInt(irtypes.I64, elemSize))
		c.storeVectorReceiverBack(member.Target, receiverSlot, cowSlice)
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
		// T0661: Mirror the T0658 push-case Optional-wrapping for the contains
		// path. When the resolved element type is Optional and the argument is a
		// bare (non-optional) value, genCallArgExpr returns the raw scalar while
		// argAlloca is {i1,T}* — the store panics. Fix: resolve elemType, detect
		// Optional, set c.targetType so genNoneLit emits a zero {i1,T} struct for
		// `v.contains(none)`, then wrap a bare scalar via wrapOptional. contains is
		// read-only so no claimStringTemp/claimHeapTemp/enumCtorTemps dance needed.
		resolvedContainsElem := elemType
		if c.typeSubst != nil {
			resolvedContainsElem = types.Substitute(resolvedContainsElem, c.typeSubst)
		}
		_, containsElemIsOpt := resolvedContainsElem.(*types.Optional)

		savedContainsTarget := c.targetType
		if containsElemIsOpt {
			c.targetType = resolvedContainsElem
		}
		argVal := c.genCallArgExpr(e.Args[0].Value)
		c.targetType = savedContainsTarget

		if containsElemIsOpt {
			argExprType := c.info.Types[e.Args[0].Value]
			if c.typeSubst != nil && argExprType != nil {
				argExprType = types.Substitute(argExprType, c.typeSubst)
			}
			if argExprType != types.TypNone && !types.Identical(argExprType, resolvedContainsElem) {
				if st, ok := elemLLVM.(*irtypes.StructType); ok {
					argVal = c.wrapOptional(argVal, st)
				}
			}
		}

		argAlloca := c.createEntryAlloca(elemLLVM)
		// Zero-initialize first to clear ALL bytes including struct padding.
		c.block.NewStore(constant.NewZeroInitializer(elemLLVM), argAlloca)
		// T0661: For Optional element types {i1, T}, store each field individually
		// instead of using a full struct store. A full struct store of a value
		// produced by insertvalue-from-undef carries undefined padding bytes (the
		// 7 bytes between i1 and i64 for alignment) that overwrite the zeroinit.
		// When push and contains execute in different functions, their separate
		// LLVM optimization contexts may produce different undefined padding,
		// causing memcmp to report inequality even when the logical values match.
		// Field stores leave the zeroinit padding untouched, guaranteeing that
		// both the vector element (from push) and the search argument have
		// identical zero padding for all supported element types.
		if containsElemIsOpt {
			if st, ok := elemLLVM.(*irtypes.StructType); ok {
				zero32 := constant.NewInt(irtypes.I32, 0)
				one32 := constant.NewInt(irtypes.I32, 1)
				i1Val := c.block.NewExtractValue(argVal, 0)
				innerVal := c.block.NewExtractValue(argVal, 1)
				f0Ptr := c.block.NewGetElementPtr(st, argAlloca, zero32, zero32)
				f1Ptr := c.block.NewGetElementPtr(st, argAlloca, zero32, one32)
				c.block.NewStore(i1Val, f0Ptr)
				c.block.NewStore(innerVal, f1Ptr)
			}
		} else {
			c.block.NewStore(argVal, argAlloca)
		}
		argPtr := c.block.NewBitCast(argAlloca, irtypes.I8Ptr)
		// Select the element comparison function:
		// • Optional elements: field-by-field comparison (ignores padding bytes)
		// • string elements: content equality via __promise_eq_string
		// • all others: memcmp (eq_fn = null)
		var eqFn value.Value
		if containsElemIsOpt {
			// T0661: Use a custom Optional equality function that compares the i1
			// presence flag and inner scalar value directly, bypassing memcmp.
			// memcmp fails cross-function on WASM because O1 decomposes
			// `store zeroinitializer` into per-field stores, leaving the 7 padding
			// bytes between i1 and i64 uninitialized — different stack frames
			// produce different garbage in those bytes → false inequality.
			eqFn = c.getOrEmitOptContainsEqFn(elemLLVM)
		} else if extractNamed(elemType) == types.TypString {
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
		c.storeVectorReceiverBack(member.Target, receiverSlot, cowSlice)

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
	// T1174/T1183: Optional elements whose inner value aliases heap
	// (Optional[heap-user], Optional[string], Optional[Vector|Channel|Arc|Weak|
	// tuple|nested-Optional]) must be deep-cloned on push (kept in sync with
	// maybeDupPushElement's Optional branch). extractNamed does not see through
	// the Optional wrapper, so without this the container/enum/user checks below
	// all miss it.
	if _, _, ok := c.optionalPushElemNeedsDup(resolvedElem); ok {
		return true
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

	// T1045: a bare closure element (Vector[() -> int]) is a *types.Signature —
	// extractNamed/extractEnum are both nil, so without this it falls through to
	// the `return nil` below as if it were a primitive/Copy. But the vector's
	// element-drop loop frees each pushed closure's heap env, so a shallow copy
	// (e.g. result.push(this[i]) in Vector.[:]) aliases the env across both
	// vectors → double-free at drop. A closure env CANNOT be deep-cloned (the
	// captured frame is opaque); null the {fn,env} fat pointer so the source keeps
	// sole ownership and the clone holds an empty closure. Symmetric with
	// emitVariantFieldDup's Signature case and emitVectorClosureNullLoop.
	if _, isSig := resolvedElem.(*types.Signature); isSig {
		return constant.NewZeroInitializer(argVal.Type())
	}

	// T1174: Optional[heap-user-type] element — deep-clone the inner heap value so
	// each vector slot owns an independent copy. The named/enum branches below
	// don't see through the Optional wrapper (extractNamed returns nil), so a
	// pushed Optional[Row] would otherwise alias the source payload (a
	// match-borrowed `if b is Has(maybe)` binding pushed into a vector, or the
	// element read in a Vector[Row?] slice) and double-free when both the source
	// and the vector drop. dupHeapValue is null-safe (handles the `none` slot via
	// a phi) and dispatches through typeinfo clone_fn for polymorphic subtypes.
	if inner, ok := c.optionalHeapDupElem(resolvedElem); ok {
		innerVal := c.block.NewExtractValue(argVal, 1)
		dup := c.dupHeapValue(innerVal, inner)
		return c.block.NewInsertValue(argVal, dup, 1)
	}

	// T1183: Optional[string] / Optional[Vector|Channel|Arc|Weak|tuple|
	// nested-Optional] element — inner ALSO aliases heap. The optionalHeapDupElem
	// branch above only matches heap-user inners, so these fell through to the
	// `return nil` below and aliased the source payload (whole-array escape of
	// Optional[string][N], or Vector[Optional[string]].push). Deep-clone via
	// dupOptionalVectorElem (present/absent split + per-inner dispatch).
	if opt, inner, ok := c.optionalPushElemNeedsDup(resolvedElem); ok {
		return c.dupOptionalVectorElem(argVal, opt, inner)
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

	// T0508: Ref[T] — strong-count increment (non-atomic when `confined, T0995).
	if arcElem, isArc := types.AsArc(resolvedElem); isArc || named == types.TypArc {
		if c.typeSubst != nil && arcElem != nil {
			arcElem = types.Substitute(arcElem, c.typeSubst)
		}
		return c.dupArc(argVal, arcElem)
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

	// T1284: structural-interface element — the {vtable, instance} view boxes a
	// heap instance; deep-clone it via RTTI so each vector slot owns an
	// independent box (else result.push(this[i]) in Vector.[:] aliases the source
	// box and the structural-aware element drop double-frees).
	if named.IsStructural() && !named.IsValueType() {
		return c.cloneStructuralView(argVal)
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
		// T0595: nested slice receiver (arr[i].push / slices[i].push) reached via a
		// path that did NOT pre-capture the slot. The Vector method-call path uses
		// storeVectorReceiverBack with a slot captured once by evalVectorReceiver, so
		// it never lands here; this recompute is a defensive fallback. NOTE: it
		// re-evaluates e.Index — sound only for a side-effect-free index.
		slotPtr := c.genIndexSlotPtr(t)
		c.block.NewStore(newPtr, slotPtr)
	}
}

// emitIndexBoundsCheck branches on idx < length (unsigned). On the false path it
// emits an out-of-bounds panic with msg and returns; execution continues in a
// fresh block named "<prefix>.ok". Shared by array index read, vector index
// assign, and the T0595 nested-slice store-back so the OOB shape lives in one
// place.
func (c *Compiler) emitIndexBoundsCheck(idx, length value.Value, prefix, msg string) {
	inBounds := c.block.NewICmp(enum.IPredULT, idx, length)
	okBlock := c.newBlock(prefix + ".ok")
	panicBlock := c.newBlock(prefix + ".oob")
	c.block.NewCondBr(inBounds, okBlock, panicBlock)

	c.block = panicBlock
	oobMsg := c.makeGlobalString(msg)
	c.block.NewCall(c.funcs["promise_panic"], oobMsg)
	c.emitPanicReturn()

	c.block = okBlock
}

// indexTargetIsArrayOrVector reports whether e.Target is a fixed-size array or a
// Vector — i.e. an index whose element slot genIndexSlotPtr can address. Mirrors
// the type-unwrap prologue of genIndexSlotPtr. Used to gate the single-eval
// receiver path in genVectorMethodCall (T0595).
func (c *Compiler) indexTargetIsArrayOrVector(e *ast.IndexExpr) bool {
	t := c.info.Types[e.Target]
	if c.typeSubst != nil {
		t = types.Substitute(t, c.typeSubst)
	}
	if ref, ok := t.(*types.MutRef); ok {
		t = ref.Elem()
	}
	if ref, ok := t.(*types.SharedRef); ok {
		t = ref.Elem()
	}
	if _, ok := t.(*types.Array); ok {
		return true
	}
	if _, ok := types.AsVector(t); ok {
		return true
	}
	return extractNamed(t) == types.TypVector && c.typeSubst != nil
}

// optionalPayloadReceiverSlot handles a Vector-method receiver of the form
// `place!` (an OptionalUnwrapExpr over an addressable place holding an
// Optional[Vector[T]]). It returns the loaded inner-Vector pointer plus a
// pointer to the optional's PAYLOAD field (struct index 1) — the slot a
// relocating method (push/pop/remove) writes the grown pointer back into.
// Emits the `!` presence-check panic (mirrors genOptionalForceUnwrap). Returns
// ok=false for any non-addressable / non-Optional[Vector] shape so the caller
// falls back to the rvalue path (read method receivers like `v[0]!.contains(x)`
// / `v[0]!.clone()` also take this path — slot is simply unused; the `.len`
// getter is dispatched elsewhere and never reaches here). T1295.
func (c *Compiler) optionalPayloadReceiverSlot(target ast.Expr) (slicePtr, slot value.Value, ok bool) {
	unwrap, isUnwrap := target.(*ast.OptionalUnwrapExpr)
	if !isUnwrap {
		return nil, nil, false
	}
	inner := unwrap.Expr
	for {
		if p, isParen := inner.(*ast.ParenExpr); isParen {
			inner = p.Expr
			continue
		}
		break
	}

	// The unwrapped place must hold an Optional[Vector[...]] (payload lowers to i8*).
	innerType := c.info.Types[inner]
	if c.typeSubst != nil {
		innerType = types.Substitute(innerType, c.typeSubst)
	}
	optType, isOpt := innerType.(*types.Optional)
	if !isOpt {
		return nil, nil, false
	}
	if !types.IsVector(optType.Elem()) {
		return nil, nil, false
	}

	// Address the optional's in-memory storage for each addressable place kind.
	var optPtr value.Value
	switch e := inner.(type) {
	case *ast.IdentExpr:
		if ptr, has := c.mutRefPtrs[e.Name]; has {
			optPtr = ptr
		} else if alloca, has := c.locals[e.Name]; has {
			optPtr = alloca
		} else {
			return nil, nil, false
		}
	case *ast.MemberExpr:
		// Only a plain owned field is addressable — a getter call or a
		// borrow-returning member is not (mirrors the T1289 guards).
		if c.isGetterCallExpr(inner) || c.isBorrowedExpr(inner) {
			return nil, nil, false
		}
		optPtr = c.genFieldPtr(e)
	case *ast.IndexExpr:
		if !c.indexTargetIsArrayOrVector(e) {
			return nil, nil, false
		}
		optPtr = c.genIndexSlotPtr(e)
	default:
		return nil, nil, false
	}

	optLLVM, structOk := c.resolveType(optType).(*irtypes.StructType)
	if !structOk {
		return nil, nil, false
	}

	// `!` presence check (identical to genOptionalForceUnwrap).
	flagPtr := c.block.NewGetElementPtr(optLLVM, optPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	flag := c.block.NewLoad(irtypes.I1, flagPtr)
	okBlock := c.newBlock("unwrap.ok")
	panicBlock := c.newBlock("unwrap.panic")
	c.block.NewCondBr(flag, okBlock, panicBlock)

	c.block = panicBlock
	panicMsg := c.makeGlobalString("unwrap failed: optional is none")
	c.block.NewCall(c.funcs["promise_panic"], panicMsg)
	c.emitPanicReturn()

	c.block = okBlock
	payloadPtr := c.block.NewGetElementPtr(optLLVM, optPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	slicePtr = c.block.NewLoad(irtypes.I8Ptr, payloadPtr)
	return slicePtr, payloadPtr, true
}

// evalVectorReceiver evaluates a Vector method-call receiver. For an arr[i] /
// vov[i] receiver it computes the element slot pointer EXACTLY ONCE, returning
// both the loaded inner-Vector pointer (slicePtr) and that slot pointer (T0595).
// The caller stores any grown pointer back through the returned slot rather than
// recomputing it — recomputing would re-evaluate e.Index, which for an impure
// index yields a DIFFERENT slot and writes the grown buffer into the wrong slot
// (use-after-free on the real slot + leak). For non-index receivers it falls back
// to genExprAutoPropagate and returns a nil slot (store-back recomputes as before).
func (c *Compiler) evalVectorReceiver(target ast.Expr) (slicePtr, slot value.Value) {
	// T1295: `place!.push/pop/remove` on an addressable Optional[Vector[T]] place.
	// Capture the optional's payload field as the write-back slot so a relocating
	// method stores the grown pointer straight into it (else the write-back is
	// dropped and the fresh COW/realloc buffer leaks).
	if sp, slotPtr, ok := c.optionalPayloadReceiverSlot(target); ok {
		return sp, slotPtr
	}
	if idxExpr, ok := target.(*ast.IndexExpr); ok && c.indexTargetIsArrayOrVector(idxExpr) {
		// T0648: suppress whole-container field dup while evaluating the outer
		// target (matches the genVectorIndex/genArrayIndex read path this replaces);
		// we want the real slot, not a clone of the outer field.
		savedDupContainer := c.dupContainerFieldAccess
		c.dupContainerFieldAccess = false
		slot = c.genIndexSlotPtr(idxExpr)
		c.dupContainerFieldAccess = savedDupContainer
		// The slot holds the inner Vector's i8* (resolveType(Vector[T]) == i8*),
		// matching what cow/push/pop/remove consume below.
		return c.block.NewLoad(irtypes.I8Ptr, slot), slot
	}
	return c.genExprAutoPropagate(target), nil // B0323
}

// storeVectorReceiverBack writes a grown/relocated Vector pointer back into its
// receiver. When the receiver slot was pre-captured by evalVectorReceiver (T0595),
// it stores through that slot directly (single evaluation); otherwise it defers to
// storeBackSlicePtr, which handles ident/field/index-recompute targets.
func (c *Compiler) storeVectorReceiverBack(target ast.Expr, slot, newPtr value.Value) {
	if slot != nil {
		c.block.NewStore(newPtr, slot)
		return
	}
	c.storeBackSlicePtr(target, newPtr)
}

// genIndexSlotPtr returns a pointer to the element slot of a fixed-size array or
// Vector at e.Index, bounds-checked. Used by storeBackSlicePtr to write a grown
// nested Vector's pointer back into its slot (T0595). Mirrors the element-pointer
// computation in genArrayIndex / genVectorIndexAssign.
func (c *Compiler) genIndexSlotPtr(e *ast.IndexExpr) value.Value {
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

	// Fixed-size array (Vector[int][2]): GEP into the array storage.
	if arr, ok := targetType.(*types.Array); ok {
		basePtr := c.genArrayBasePtr(e.Target, arr)
		idx := c.genExpr(e.Index)
		elemLLVM := c.resolveType(arr.Elem())
		arrType := irtypes.NewArray(uint64(arr.Size()), elemLLVM)
		c.emitIndexBoundsCheck(idx, constant.NewInt(irtypes.I64, arr.Size()),
			"arridx", "array index out of bounds")
		return c.block.NewGetElementPtr(arrType, basePtr,
			constant.NewInt(irtypes.I32, 0), idx)
	}

	// Vector (Vector[int][]): GEP into the heap buffer after the header. The outer
	// vector is always heap-allocated (a vector-of-vectors can never be a .rodata
	// static literal — T0062 statics require compile-time-constant scalar elements),
	// so the inner push never reallocates it and the loaded pointer is stable.
	elemType, ok := types.AsVector(targetType)
	if !ok && extractNamed(targetType) == types.TypVector && c.typeSubst != nil {
		elemType = c.resolveTypeParam(types.TypVector.TypeParams()[0])
		ok = elemType != nil
	}
	if !ok {
		panic(fmt.Sprintf("codegen: storeBackSlicePtr index target is not array/vector: %s", targetType))
	}
	slicePtr := c.genExpr(e.Target)
	idx := c.genExpr(e.Index)
	elemLLVM := c.resolveType(elemType)
	headerPtr := c.block.NewBitCast(slicePtr, irtypes.NewPointer(vectorHeaderType()))
	length := loadVectorLen(c.block, headerPtr)
	c.emitIndexBoundsCheck(idx, length, "nestedpush", "index out of bounds")
	dataBase := c.block.NewGetElementPtr(irtypes.I8, slicePtr,
		constant.NewInt(irtypes.I64, int64(vectorHeaderSize)))
	dataTypedPtr := c.block.NewBitCast(dataBase, irtypes.NewPointer(elemLLVM))
	return c.block.NewGetElementPtr(elemLLVM, dataTypedPtr, idx)
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

// isVariantPayloadBorrowShape reports whether a type has the shape that
// matchBindingIsBorrow marks match-borrowed for a droppable enum variant payload:
// Optional-of-anything or fixed-Array-of-anything. This is the gate the T1170
// read/escape-side dup uses — it precisely selects enum-variant Optional/Array
// payload bindings while EXCLUDING the bare-heap (string/Vector/…) bindings that
// T0672 also places in matchBorrowedIdents (if-let / while-let / container-index /
// tuple-destructure borrow sources). Those bare-heap bindings are already owned
// copies produced by their source's own dup-on-read (e.g. Map's `[]` body), so
// dup'ing them again would leak — hence they must not take the T1170 escape dup.
func isVariantPayloadBorrowShape(t types.Type) bool {
	if t == nil {
		return false
	}
	if _, ok := t.(*types.Optional); ok {
		return true
	}
	if _, ok := t.(*types.Array); ok {
		return true
	}
	return false
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
	// T1176/T1173: a whole fixed-Array field/binding read out by value
	// (`return w.rows`) aliases N inner heap allocations (heap-user instances, or
	// string/Vector/Channel/Arc/Weak buffers); the owner's synth drop frees them
	// at scope exit while the escaped copy still points in (UAF/double-free). Route
	// through dupContainerFieldAccess so dupHeapFieldForEscape element-wise
	// deep-clones. Gated on the aliasing-element shape (int[N]/value arrays are
	// untouched — arrayElemNeedsEscapeDup returns false for them).
	if _, _, ok := c.arrayElemNeedsEscapeDup(t); ok {
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

// isUnwrappedContainerIndex reports whether expr is `container[k]!` — an
// OptionalUnwrapExpr whose inner (peeling ParenExpr) is an IndexExpr. When such
// an inline unwrap escapes into an OWNING context (move-param arg, constructor
// field-init, return), the unwrapped element aliases the container's slot, so
// without a dup the owning sink AND the container's drop free the same instance
// (double-free). Setting dupHeapUserFieldAccess routes it through genMethodIndex's
// deep-dup, mirroring the var-binding form (genTypedVarDecl, stmt.go). The dup is
// gated inside genMethodIndex on the droppable-no-clone Map shape, so enabling the
// flag here is safe for any other shape (it is reset after genExpr; clone-bearing
// V dups internally and never double-dups). T1146.
func isUnwrappedContainerIndex(expr ast.Expr) bool {
	unwrap, ok := expr.(*ast.OptionalUnwrapExpr)
	if !ok {
		return false
	}
	inner := unwrap.Expr
	for {
		p, ok := inner.(*ast.ParenExpr)
		if !ok {
			break
		}
		inner = p.Expr
	}
	_, ok = inner.(*ast.IndexExpr)
	return ok
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
	// T1170: a match-borrowed Optional/Array-of-heap binding (`consume(move maybe)`)
	// passed to a ~ (move) param escapes into the callee. Clone so the subject's
	// synth enum drop doesn't free the value the callee now owns (mirrors the
	// store/return escape paths). genIdentExpr performs the actual dup; the move-arg
	// site claims the produced optionalStringDup/optionalContainerDup into the callee.
	if ident, ok := arg.(*ast.IdentExpr); ok && c.matchBorrowedIdents != nil &&
		c.matchBorrowedIdents[ident.Name] && isVariantPayloadBorrowShape(pt) {
		c.setDupFlagsForFieldAccess(pt)
		return
	}
	// T1146: `consume(m[k]!)` — inline unwrap of a container index passed to a
	// move (~) param. Dup so the callee's consume-drop and the map's slot drop
	// don't free the same instance. Mirrors the var-binding form (stmt.go:1133).
	if isUnwrappedContainerIndex(arg) {
		c.dupHeapUserFieldAccess = true
		return
	}
	// T0403/T1175/T1215: IndexExpr against a Vector passed to a `~` param.
	// `f(v[i])` returns a value that aliases the vector's element buffer — the
	// callee's consume-drop and v's element drop then free the same allocation.
	// Arm the matching dup-on-read (heap-user, Optional[heap-user], droppable-enum,
	// string, or container element) so genVectorIndex/genArrayIndex produce an
	// owned copy. T1215 added the string/container element arms (previously only
	// heap-user/enum were handled, so a `string[]`/`Vector[...][]` element double-
	// freed). Peels parens + non-consuming casts (mirrors the constructor path).
	if c.armDupForVectorIndexArg(arg) {
		return
	}
	mem, ok := arg.(*ast.MemberExpr)
	if !ok {
		return
	}
	// T1011: a narrowed enum variant field arg passed to a `~` param needs the
	// same dup-on-escape as a struct field — gate on the subject enum being
	// droppable (the enum owner is an *types.Enum, which ownerHasOrSynthDrop below
	// does not recognize).
	if matched, droppable := c.narrowedVariantFieldDroppable(mem); matched {
		if droppable {
			c.setDupFlagsForFieldAccess(pt)
		}
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

// armDupForVectorIndexArg arms the appropriate dup-on-read flag when `arg`
// (peeling parens + non-consuming casts) is a Vector index `v[i]` whose element
// is a heap type that would otherwise ALIAS the vector's element buffer when the
// value escapes into an owning sink — a constructor field-init, a `~` (move)
// param, or an enum variant payload. Without the dup both the owning sink's drop
// and the vector's element drop free the same allocation (double-free at scope
// exit → "fatal: invalid free (bad header magic)" on macOS, silent over-free
// elsewhere). genVectorIndex consumes the matching flag (B0204 string / T0383
// container / T0398 heap-user / T1129 enum) to produce an independent copy.
// Returns true if a flag was armed. Before T1215 only the heap-user/enum element
// shapes were handled here (T0847), so a `string[]`/`Vector[...][]` element read
// into an owned field, move param, or enum payload aliased the source and
// double-freed. T1215.
func (c *Compiler) armDupForVectorIndexArg(arg ast.Expr) bool {
	probe := arg
	for {
		if p, ok := probe.(*ast.ParenExpr); ok {
			probe = p.Expr
			continue
		}
		if cast, ok := probe.(*ast.CastExpr); ok {
			probe = cast.Expr
			continue
		}
		break
	}
	idx, ok := probe.(*ast.IndexExpr)
	if !ok {
		return false
	}
	targetType := c.info.Types[idx.Target]
	if c.typeSubst != nil && targetType != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	if _, isVec := types.AsVector(targetType); !isVec {
		return false
	}
	argType := c.info.Types[idx]
	if c.typeSubst != nil && argType != nil {
		argType = types.Substitute(argType, c.typeSubst)
	}
	// Heap-user / Optional[heap-user] / droppable-enum element → deep clone.
	_, optHeap := c.optionalHeapDupElem(argType)
	if isDroppableHeapUserType(argType) || optHeap || c.enumElemNeedsDupOnRead(argType) {
		c.dupHeapUserFieldAccess = true
		return true
	}
	// string / Optional[string] / Vector|Channel|Arc|Weak element → string/container dup.
	savedStr, savedCont := c.dupStringFieldAccess, c.dupContainerFieldAccess
	c.setDupFlagsForFieldAccess(argType)
	return c.dupStringFieldAccess != savedStr || c.dupContainerFieldAccess != savedCont
}

// maybeEnableDupForConstructorArg sets dupStringFieldAccess or
// dupContainerFieldAccess when a constructor field-init arg is a field read
// on a droppable owner. Without this, the field's inner buffer is shared
// between the owner and the new instance — both end up freeing it. Mirrors
// maybeEnableDupForMutRefArg (T0366) for the constructor field-init path.
// T0411.
func (c *Compiler) maybeEnableDupForConstructorArg(arg ast.Expr, fieldType types.Type) {
	// T1170: a match-borrowed Optional/Array-of-heap binding (`Wrapper(held: maybe)`)
	// initializing an owned constructor field escapes into the new instance. Clone so
	// the subject's synth enum drop doesn't free the value the field now owns (mirrors
	// the move-param / store / return escape paths).
	if ident, ok := arg.(*ast.IdentExpr); ok && c.matchBorrowedIdents != nil && c.matchBorrowedIdents[ident.Name] {
		ft := fieldType
		if c.typeSubst != nil {
			ft = types.Substitute(ft, c.typeSubst)
		}
		if isVariantPayloadBorrowShape(ft) {
			c.setDupFlagsForFieldAccess(ft)
		}
		return
	}
	// T1146: `Holder(res: m[k]!)` — inline unwrap of a container index used to
	// initialize an owned field. Same double-free as the move-param case.
	if isUnwrappedContainerIndex(arg) {
		c.dupHeapUserFieldAccess = true
		return
	}
	// T0847/T1175/T1215: peel parens + non-consuming casts to find a container-
	// element IndexExpr subject, then dup-on-read for `Holder(held: v[0])` /
	// `Holder(s: strings[0])` / `Holder(held: v[0] as! C)`. Covers heap-user,
	// Optional[heap-user], droppable-enum, string, and container element shapes
	// (T1215 added the string/container arms, which previously aliased the
	// vector's element buffer → double-free). Mirrors maybeEnableDupForMutRefArg.
	if c.armDupForVectorIndexArg(arg) {
		return
	}
	mem, ok := arg.(*ast.MemberExpr)
	if !ok {
		return
	}
	ft := fieldType
	if c.typeSubst != nil {
		ft = types.Substitute(ft, c.typeSubst)
	}
	// T1011: a narrowed enum variant field arg initializing a constructor field
	// needs the same dup-on-escape as a struct field — gate on the subject enum
	// being droppable (the enum owner is an *types.Enum, which ownerHasOrSynthDrop
	// below does not recognize).
	if matched, droppable := c.narrowedVariantFieldDroppable(mem); matched {
		if droppable {
			c.setDupFlagsForFieldAccess(ft)
		}
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
	c.setDupFlagsForFieldAccess(ft)
}

// genCallArgsWithMutRef evaluates call arguments with MutRef-awareness (B0149).
// For MutRef params, passes the address of the caller's storage instead of the value.
// When the arg needs no coercion and is a simple lvalue, passes the alloca directly.
// Otherwise, evaluates the value, stores in a temp alloca, and passes the temp.
func (c *Compiler) genCallArgsWithMutRef(args []*ast.Arg, params []*types.Param, calleeResult types.Type) ([]value.Value, []types.Type, []variadicPassthrough) {
	var argVals []value.Value
	var argTypes []types.Type
	var variadicPTs []variadicPassthrough // B0203: passthrough vectors needing len restored after call
	// T1233: a generator call returns a stream[T]; its by-value params are copied
	// into the coroutine frame and read LAZILY during iteration, which outlives
	// this call statement. A caller-side statement temp (dropped at statement end)
	// would free a tuple arg's heap fields before the generator reads them (UAF).
	// For such args we instead register a SCOPE-level owned drop (see below).
	_, calleeIsGenerator := types.AsStream(calleeResult)
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
		// T1108: snapshot enum-ctor temps before evaluating the arg so a move
		// param can claim (untrack) any inline enum-constructor temp produced
		// during this arg's evaluation — the callee consumes & drops it, so the
		// caller's statement-end drop must not also fire (double-free). Borrow
		// params leave the temp tracked → caller drops it at statement end.
		savedEnumTemps := len(c.enumCtorTemps)
		v := c.genCallArgExpr(arg.Value)
		c.dupStringFieldAccess = false
		c.dupContainerFieldAccess = false
		c.dupHeapUserFieldAccess = false // T0403
		// T1174: `consume(move maybe)` where maybe is a match-borrowed
		// Optional[heap-user] binding passed to a `~` (move) param aliases the
		// subject's variant payload; deep-clone the inner so the callee owns an
		// independent copy and the subject's synth enum drop still frees the original
		// exactly once. Borrow (`&`) params leave the alias intact (no escape).
		if isMutRefParam {
			v, _ = c.dupBorrowedHeapUserPayload(arg.Value, v)
		}
		argVals = append(argVals, v)
		argTypes = append(argTypes, c.info.Types[arg.Value])
		// T0087: For ~ (move) params, transfer ownership to callee.
		// Clear caller's drop flag and claim string/heap temps so they're not double-freed.
		if isMutRefParam {
			if ident, ok := arg.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
			// T0754: a cast subject is consumed by ownership at `~`-param sites
			// — clear the subject's drop flag so it doesn't double-free with the
			// callee's consume drop. Mirrors the IdentExpr branch above.
			// T0849: for the conditional `as` form, drop iff the downcast failed.
			if ident := c.castSubjectMovableIdent(arg.Value); ident != nil {
				c.consumeCastSubjectDropFlag(arg.Value, ident.Name)
			}
			// T1224: `consume(move r!)` where r is an Optional single-owner handle —
			// the force-unwrap moves the inner out and the callee's move param drops
			// it, so the source optional's present flag must be cleared or its
			// scope-exit drop double-frees the same handle → segfault. Mirrors the
			// neutralizeForceUnwrapSource call in every constructor arg path (B0301).
			// Self-gating: no-ops unless arg.Value is a force-unwrap / force-cast /
			// optional error-handler, so plain-ident/temp/literal moves are unaffected.
			c.neutralizeForceUnwrapSource(arg.Value)
			c.claimStringTemp(v)
			c.claimHeapTemp(v) // B0201: prevent double-free for vector literals passed to ~ params
			c.claimEnvTemp(v)  // T1237: closure env ownership transfers to the callee's move param
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
			// T1108: claim (untrack) the inline enum-constructor temp evaluated
			// for this move-param arg — the callee now owns and drops it. Gate on
			// the arg's static type being an enum: only then is a tracked enum-ctor
			// temp the value actually being moved into the callee. When the arg is
			// a non-enum expression that merely BORROWS an enum-ctor temp in a
			// sub-call (e.g. `take(inspect(Enum.V(x)))` where `inspect(Enum)` is a
			// borrow param returning a non-enum), that inner temp is an
			// intermediate the callee never receives — it must stay tracked so the
			// caller drops it at statement end, else it leaks.
			argEnumType := c.info.Types[arg.Value]
			if c.typeSubst != nil {
				argEnumType = types.Substitute(argEnumType, c.typeSubst)
			}
			if extractEnum(argEnumType) != nil {
				for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
					c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
				}
				c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
			}
		}
		// T1233: A plain (borrow) tuple-by-value param no longer drops its arg
		// (the callee-side T0406 drop was removed — a plain tuple param borrows).
		// When the arg is a tuple TEMP (literal / call result — no owning caller
		// variable), the caller must drop it after the call returns, else its heap
		// fields (closure envs, strings, vectors) leak. An owned tuple variable
		// keeps its own bindingDropTuple and a borrowed source is owned elsewhere —
		// neither needs a caller temp (see tupleArgIsCallerOwnedTemp).
		if i < len(params) && !isMutRefParam && !params[i].IsVariadic() {
			paramType := params[i].Type()
			if c.typeSubst != nil {
				paramType = types.Substitute(paramType, c.typeSubst)
			}
			if tup, isTuple := paramType.(*types.Tuple); isTuple && c.tupleNeedsDrop(tup) && c.tupleArgIsCallerOwnedTemp(arg.Value) {
				if calleeIsGenerator {
					// T1233: the generator borrows its param but reads it lazily
					// (frame outlives this statement), so a statement-end drop is
					// too early → UAF. Give the temp a SCOPE-level owned drop (a
					// synthetic owned local) — the same lifetime an owned tuple
					// variable gets, which the borrow model already handles: the
					// tuple stays alive until the enclosing scope exits (after the
					// for-in loop consuming the stream), then drops exactly once.
					name := c.uniqueLocalName("_gentuparg")
					argAlloca := c.createEntryAlloca(v.Type())
					argAlloca.SetName(name)
					c.block.NewStore(v, argAlloca)
					c.maybeRegisterDrop(name, argAlloca, tup)
				} else {
					c.registerTupleStmtTemp(v, tup)
				}
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
		if isThisReceiver(target.Target) {
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
	if isThisReceiver(target.Target) {
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
					// T1215: dup-on-read a heap element read out of a Vector into
					// this variant payload (`Word(v[i])`) — else the payload aliases
					// the vector's element buffer and both drops double-free.
					c.maybeEnableDupForConstructorArg(arg.Value, vfType)
					preWrapVal = c.genCallArgExpr(arg.Value)
					c.dupStringFieldAccess = false
					c.dupContainerFieldAccess = false
					c.dupHeapUserFieldAccess = false
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
				// T1215: dup-on-read a heap element read out of a Vector into
				// this variant payload (`Word(payload[i])`) — else the payload
				// aliases the vector's element buffer and both drops double-free
				// at scope exit ("fatal: invalid free (bad header magic)").
				c.maybeEnableDupForConstructorArg(arg.Value, vfType)
				val = c.genCallArgExpr(arg.Value)
				c.dupStringFieldAccess = false
				c.dupContainerFieldAccess = false
				c.dupHeapUserFieldAccess = false
				preWrapVal = val
			}
			fieldPtr := c.block.NewGetElementPtr(dataType, typedDataPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
			c.block.NewStore(val, fieldPtr)
			// Clear drop flag: field value is moved into the enum variant.
			// T1108: the enum-constructor temp is ALWAYS tracked for
			// statement-end cleanup (see the tracking gate below) regardless
			// of where the payload came from. Moving a droppable payload into
			// the variant only needs to clear the source's own drop flag so
			// the payload isn't double-freed; the enum temp's drop (or its
			// consumer's drop-flag clear) is the single owner of the payload.
			if ident, ok := arg.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			} else if castIdent := c.castSubjectMovableIdent(arg.Value); castIdent != nil {
				// T0754: ownership moves the cast subject into the variant
				// payload — clear the subject's drop flag so it doesn't double-
				// free at scope exit with the enum's variant drop.
				// T0849: for the conditional `as` form, drop iff the cast failed.
				c.consumeCastSubjectDropFlag(arg.Value, castIdent.Name)
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
	// T1108: Track unconditionally (formerly gated on !movedDroppable). The enum
	// temp is the single owner of any moved-in droppable payload until it is
	// consumed; every consuming site (var-decl/assignment, container store,
	// move-param arg, Arc/Mutex/channel/Vector.push, etc.) clears this temp's
	// drop flag, so the statement-end drop fires only for borrowed/discarded
	// temps — exactly the case that previously leaked the payload.
	if dataType != nil && c.entryBlock != nil && c.tempTrackingEnabled {
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
		// T1119: An owned-rvalue subject of a droppable enum (a function/method/
		// constructor return, or a non-native `[]`-method read like `m[k]!`) has no
		// other owner — its arm bindings are dup'd into independent copies (under
		// enumHasDrop), so the subject value's variant payload would leak (and, with
		// the T1110 container-read fix, double the leak per read) because nothing
		// drops it. Spill it into a temp and register the same enum-drop binding a
		// local `v := <expr>; match v` would get, so it is dropped on every match
		// exit (merge block and early return/break/continue inside arms). A
		// borrowed subject (`&E`/`E~`) or a place (ident/field/native index) is
		// owned elsewhere and must NOT get this drop (would double-free).
		var subjectDropFlag *ir.InstAlloca
		if enumHasDrop && !isRefType(subjectType) && c.subjectIsOwnedRvalueEnum(e.Subject, subjectType) {
			spill := c.createEntryAlloca(subject.Type())
			c.block.NewStore(subject, spill)
			subjVar := c.uniqueLocalName("match.subject")
			c.maybeRegisterEnumDrop(subjVar, spill, subjectType, enum)
			// T1119: hand the spill's drop flag to genEnumMatch so a whole-value
			// name-binding arm (`match make() { h => ... }`) can alias it: if that
			// arm moves the bound value out (returns/`move`s it), clearDropFlag on
			// the binding clears THIS flag too, suppressing the subject drop —
			// otherwise the moved-out value is dropped here AND by its new owner
			// (use-after-free / double-decrement of the aliased payload).
			subjectDropFlag = c.dropFlags[subjVar]
		}
		return c.genEnumMatch(e, e.Subject, subject, enum, enumLayout, enumHasDrop, subjectType, subjectDropFlag)
	}

	// T1187: an owned-rvalue Optional subject (a call/method/constructor return of
	// type T?) with a droppable payload has no owner — genValueMatch only reads the
	// present flag (T1002), so the inner heap value would leak. Spill it to a temp
	// and register the same optional-drop binding a `v := <expr>; match v` local
	// would get, so it is dropped on every match exit. Guards mirror the T1119 enum
	// spill: a borrowed subject (`T?&`) or a place (ident/field) is owned elsewhere
	// and must NOT get this drop (would double-free).
	var subjectDropFlag *ir.InstAlloca
	if opt, ok := unwrapRefsType(subjectType).(*types.Optional); ok &&
		!isRefType(subjectType) && c.subjectIsOwnedRvalueEnum(e.Subject, subjectType) {
		spill := c.createEntryAlloca(subject.Type())
		c.block.NewStore(subject, spill)
		subjVar := c.uniqueLocalName("match.subject")
		c.maybeRegisterOptionalDrop(subjVar, spill, opt)
		// nil unless maybeRegisterOptionalDrop registered a drop (non-droppable
		// inner like int? no-ops → flag stays nil → no aliasing needed).
		subjectDropFlag = c.dropFlags[subjVar]
	}
	return c.genValueMatch(e, subject, subjectType, subjectDropFlag)
}

// subjectIsOwnedRvalueEnum reports whether a match subject expression produces a
// freshly-owned value (an rvalue) rather than projecting an existing place. Only
// owned rvalues need the T1119 subject-drop: a place (IdentExpr local/param,
// MemberExpr field, ThisExpr, native Vector/Array index) is owned by something
// else that drops it, so dropping it here would double-free.
//
// Transparent borrow-preserving wrappers — parentheses and optional force-unwrap
// (`!`) — are peeled first: `make_opt()!` is owned (root is a call) while `o!`
// for a local `o` is a place (root is an ident).
//
// A call always yields an owned value. A non-native `[]`-method read (Map/Set)
// yields an owned value exactly when the `[]` method's internal match-destructure
// dups the value on return (matchFieldNeedsDup, see Part B) — i.e. when the
// element enum is shallow-dup-safe (enumMatchDupSafe, T1110) OR has a real
// deep-clone (typeNeedsMatchDup: a user/`clone clone, or the T1129 synthesized
// recursive clone). A container-bearing/recursive enum WITHOUT any clone (e.g. an
// Arc/Ref-bearing variant, T1117) is neither, so its `[]` read returns an alias
// the container still owns — dropping it here would double-free. The
// classification is deliberately conservative: anything uncertain returns false
// (a missed owned form is a pre-existing leak, never a new double-free).
func (c *Compiler) subjectIsOwnedRvalueEnum(expr ast.Expr, subjectType types.Type) bool {
	for {
		switch e := expr.(type) {
		case *ast.ParenExpr:
			expr = e.Expr
		case *ast.OptionalUnwrapExpr:
			expr = e.Expr
		default:
			switch e := expr.(type) {
			case *ast.CallExpr:
				return true
			case *ast.IndexExpr:
				// Mirror matchFieldNeedsDup: the `[]` body dups the returned value
				// iff typeNeedsMatchDup || enumMatchDupSafe. Keeping these in lockstep
				// is what makes the inline `match m[k]!` drop-vs-borrow decision match
				// the actual ownership the `[]` method hands back (T1129).
				//
				// T1191: a Map/Set `[]` read whose subject is itself an optional
				// (`match m[k] { none => .., _ => .. }`) hands back an owned `V?`
				// whose ownership is governed by the payload V, not the `V?` wrapper.
				// Classify dup-safety on the optional's payload so `match m[k]` gets
				// the same spill + optional-drop that `o := m[k]` / `if v := m[k]`
				// already do — `typeNeedsMatchDup(V?)`/`enumMatchDupSafe(V?, nil)` are
				// both false (an Optional is neither a droppable Named nor an Enum),
				// which otherwise misclassifies the read as a borrow and leaks V.
				// Walk past every Optional layer (nested `V??` for `Map[K, V?]`) so the
				// classifier reaches the same bottom inner type that the drop registrar
				// maybeRegisterOptionalDrop dispatches on (T0391) — keeping the drop-vs-
				// borrow decision and the actual optional-drop in lockstep.
				classifyType := unwrapRefsType(subjectType)
				for {
					opt, ok := classifyType.(*types.Optional)
					if !ok {
						break
					}
					classifyType = opt.Elem()
				}
				return c.indexDispatchesToMethod(e) &&
					(c.typeNeedsMatchDup(classifyType) || c.enumMatchDupSafe(classifyType, nil))
			}
			return false
		}
	}
}

// indexDispatchesToMethod reports whether an index expression `target[idx]`
// dispatches to a non-native user/std `[]` method (Map, Set, or a Promise-defined
// type) — whose return is an owned value — as opposed to native Vector/Array/
// string indexing, which projects a place the container still owns. Mirrors the
// dispatch logic in genIndexExpr. (T1119)
func (c *Compiler) indexDispatchesToMethod(e *ast.IndexExpr) bool {
	tt := c.info.Types[e.Target]
	if c.typeSubst != nil {
		tt = types.Substitute(tt, c.typeSubst)
	}
	if ref, ok := tt.(*types.MutRef); ok {
		tt = ref.Elem()
	}
	if ref, ok := tt.(*types.SharedRef); ok {
		tt = ref.Elem()
	}
	if _, ok := tt.(*types.Array); ok {
		return false // native fixed-array indexing
	}
	named := extractNamed(tt)
	if named == nil {
		return false
	}
	m := named.LookupMethod("[]")
	return m != nil && !m.IsNative()
}

// enumInstanceHasDrop returns true if an enum type (possibly monomorphized) has a drop function.
// Checks both sema-level detection and codegen-time mono synthesized drops.
func (c *Compiler) enumInstanceHasDrop(subjectType types.Type, enum *types.Enum) bool {
	if enum.HasDrop() || enum.NeedsSynthDrop() {
		return true
	}
	// Check for codegen-time mono synthesized drop (generic enums with droppable TypeParam fields).
	// T1018: strip borrows so a borrowed generic enum subject (Maybe[string]& from
	// Ref.borrow) still detects the mono drop — otherwise destructured fields are
	// not dup'd and the binding aliases the borrowed payload (double-free).
	if inst, ok := unwrapRefsType(subjectType).(*types.Instance); ok {
		mangledName := mangleMethodName(monoName(inst), "drop", false)
		_, ok := c.funcs[mangledName]
		return ok
	}
	return false
}

// enumElemNeedsDupOnRead reports whether a droppable enum element read from a
// native Vector/Array index and bound to a variable/slot must be deep-cloned so
// the binding owns independent variant data (T1129). Uses the full droppable
// predicate (enumInstanceHasDrop covers HasDrop, NeedsSynthDrop, and generic
// mono-drop instances) — vecElemNeedsEnumDrop is too narrow here, missing
// non-generic synth-drop enums like a recursive `Tree`.
func (c *Compiler) enumElemNeedsDupOnRead(t types.Type) bool {
	enum := extractEnum(t)
	return enum != nil && c.enumInstanceHasDrop(t, enum)
}

// genEnumMatch generates a match expression on an enum value using an LLVM switch instruction.
func (c *Compiler) genEnumMatch(e *ast.MatchExpr, subjectExpr ast.Expr, subject value.Value, enum *types.Enum, layout *TypeDeclLayout, enumHasDrop bool, subjectType types.Type, subjectDropFlag *ir.InstAlloca) value.Value {
	// Extract tag from subject
	var tag value.Value
	if layout.MaxVariantDataSize == 0 {
		tag = subject // fieldless enum, subject IS the tag
	} else {
		tag = c.block.NewExtractValue(subject, 0)
	}

	switchBlock := c.block
	mergeBlock := c.newBlock("match.end")

	// T0496: the match expression's own result type, used as the contextual target
	// type for each arm so a bare `none` arm lowers to the right Optional shape.
	matchResultType := c.info.Types[e]
	if c.typeSubst != nil && matchResultType != nil {
		matchResultType = types.Substitute(matchResultType, c.typeSubst)
	}

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
		// T1155: Snapshot c.locals/c.dropFlags entries that this arm's pattern
		// bindings will shadow, so the binding is strictly arm-scoped. Without
		// this, an arm that rebinds the scrutinee's own name (e.g.
		// `match b { Msg.Text(b) => ... }`) leaves c.locals["b"] pointing at the
		// destructured (wrong-typed) alloca for the rest of the function, so a
		// later `match b` evaluates its subject against that stale alloca and
		// emits garbage/self-recursive control flow → runtime stack overflow.
		// Mirrors the save/restore already done for type-binding arms in
		// genTypeMatch.
		armBindingNames := patternBindingNames(arm.Pattern)
		type savedLocal struct {
			alloca   *ir.InstAlloca
			hadLocal bool
			dropFlag *ir.InstAlloca
			hadDrop  bool
		}
		savedLocals := make(map[string]savedLocal, len(armBindingNames))
		for _, name := range armBindingNames {
			sl := savedLocal{}
			sl.alloca, sl.hadLocal = c.locals[name]
			sl.dropFlag, sl.hadDrop = c.dropFlags[name]
			savedLocals[name] = sl
		}
		c.bindMatchPattern(arm.Pattern, subjectExpr, subject, enum, layout, enumHasDrop, subjectType, subjectDropFlag)

		armVal := c.genMatchArmValue(arm, matchResultType)
		armOwned, armOwnedFlag := c.matchArmTransfersOwnership(*arm, armVal) // T1107/T1208: before claim
		c.claimStringTemp(armVal)                                            // T0073: ownership transfers to match phi

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

		// T1155: Restore the c.locals/c.dropFlags entries this arm's bindings
		// shadowed, so the bindings do not leak into the enclosing block or
		// sibling arms. Re-instate the prior entry when one existed, else delete.
		for name, sl := range savedLocals {
			if sl.hadLocal {
				c.locals[name] = sl.alloca
			} else {
				delete(c.locals, name)
			}
			if sl.hadDrop {
				c.dropFlags[name] = sl.dropFlag
			} else {
				delete(c.dropFlags, name)
			}
		}

		armEnd := c.block
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}

		arms = append(arms, matchArmInfo{val: armVal, end: armEnd, hasV: armVal != nil, owned: armOwned, ownedFlag: armOwnedFlag})
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
	return c.buildMatchPhi(mergeBlock, arms, matchResultType)
}

// matchArmInfo tracks a match arm's result value and final block for PHI construction.
type matchArmInfo struct {
	val       value.Value
	end       *ir.Block
	hasV      bool
	owned     bool        // T1107: arm transferred an owned i8* value into the phi (drives the anyOwned gate)
	ownedFlag value.Value // T1208: live per-path i1 ownership flag when the arm value is a nested tracked phi (nil → use the `owned` constant)
}

// genMatchArmValue generates a match arm's result value with the match's result
// type set as the contextual target type (T0496). This makes a bare `none` arm
// lower to a zero value of the shared result type (e.g. an Optional struct)
// rather than the `i1 0` void fallback, which would otherwise produce a
// phi-type mismatch. Restored after the arm so it does not leak to siblings.
func (c *Compiler) genMatchArmValue(arm *ast.MatchArm, resultType types.Type) value.Value {
	saved := c.targetType
	c.targetType = resultType
	defer func() { c.targetType = saved }()
	if arm.Body != nil {
		// T1267: a bare failable call in an expression arm auto-propagates like
		// any other bare call site. genExpr yields the raw error-union; the arm
		// is the context that branches on the ok-flag (propagating on error) and
		// yields the unwrapped success value. Track the unwrapped heap value as a
		// stmt temp so the merge phi's claimStringTemp/ownership machinery frees
		// it — mirroring the explicit `?^` (ErrorPropagateExpr) arm path.
		if c.info.AutoPropagateExprs[arm.Body] {
			result := c.genExpr(arm.Body)
			unwrapped := c.genAutoPropagateValue(result)
			if unwrapped != nil {
				c.trackUnwrappedFailableTemp(arm.Body, unwrapped)
			}
			return unwrapped
		}
		return c.genExpr(arm.Body)
	} else if arm.Block != nil {
		return c.genBlockValue(arm.Block)
	}
	return nil
}

// buildMatchPhi constructs a PHI node at mergeBlock from collected match arm info.
// Arms that branch to mergeBlock but produce no value get a null placeholder.
// Returns nil if no arm produces a value (match used as statement).
func (c *Compiler) buildMatchPhi(mergeBlock *ir.Block, arms []matchArmInfo, resultType types.Type) value.Value {
	// T1189: coerce each arm value to the shared Optional result shape before the
	// void filter / valType selection, so a bare value arm sibling of a `none` arm
	// contributes `{ i1, T }` rather than bare `T` to the merge phi. The wrapping
	// insertvalue must be emitted in the arm's own end block (before its terminator)
	// so it dominates the phi's incoming edge — not in mergeBlock.
	savedBlock := c.block
	for i := range arms {
		if arms[i].val == nil || arms[i].end == nil {
			continue
		}
		c.block = arms[i].end
		arms[i].val = c.wrapArmValueOptional(arms[i].val, resultType)
	}
	c.block = savedBlock
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
		phi := mergeBlock.NewPhi(incomings...)
		c.trackMergeResultTemp(phi, resultType, arms) // T1107
		return phi
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

// clearResultDropFlags clears the drop flag for a DIRECT owned-local ident that
// is a scope result (match arm / if branch), transferring its ownership to the
// enclosing merge phi so the arm-scope cleanup does not double-free it.
//
// T1206: this deliberately does NOT recurse into a nested if/match sub-expression.
// Since T1107, every nested if/match already self-manages its own result-position
// idents: genBlockValue / genIfExpr / genMatchArmValue clear each owned-local's
// drop flag PATH-CONDITIONALLY inside the branch that actually selects it, and
// register the nested phi as a tracked owned temp. Recursing here would instead
// emit an UNCONDITIONAL `store i1 false` for that ident in the OUTER merge block —
// orphaning the local on the path where the nested conditional selected its other
// (freshly-cloned) arm and never moved that local. The B0242 case
// (`match … => if v>0 { k } else { "other" }`) stays correct because the inner if
// clears `k` in its then-block regardless. For a bare owned-local ident arm
// (`=> local`) the direct IdentExpr case below still runs, which is required.
func (c *Compiler) clearResultDropFlags(expr ast.Expr) {
	if expr == nil {
		return
	}
	if e, ok := expr.(*ast.IdentExpr); ok {
		c.clearDropFlag(e.Name)
	}
	// Nested if/match sub-expressions self-manage their result idents (see above);
	// calls/binary ops clear at their own inner sites.
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

// exprResultTransfersOwnership reports whether an EXPRESSION-form match-arm /
// if-branch result transfers ownership of an owned i8*-represented heap value
// (string, Vector[T], native handle) into the merge phi (T1107). True when either
// (a) the arm value is a live tracked statement temp (a clone()/call result, or a
// nested if/match phi already registered by trackMergeResultTemp — its drop flag
// is about to be claimed into the phi), or (b) the result expression is an
// owned-local ident whose scope drop flag is live (about to be cleared by
// clearMatchArmResultDropFlags). False for a borrowed param / field / .rodata
// literal alias — the real owner keeps it, so the phi must borrow. Must be called
// BEFORE claimStringTemp / clearMatchArmResultDropFlags neutralize those flags.
// BLOCK-form arms are handled by the c.blockValueOwnedResult flag instead (their
// result temp is already claimed inside genBlockValue by the time we get here).
func (c *Compiler) exprResultTransfersOwnership(val value.Value, body ast.Expr) bool {
	if val != nil {
		if idx, ok := c.stmtTempMap[val]; ok && idx >= 0 {
			return true
		}
		// T1211: a fresh owned heap value struct (heap-user-type / Map constructor or
		// clone) transfers ownership into the merge phi, but is tracked as a heapTemp
		// (not a stmtTemp), so the check above misses it.
		if c.resultIsFreshOwnedHeapTemp(val) {
			return true
		}
	}
	return c.resultTransfersOwnedFlag(body)
}

// matchArmTransfersOwnership reports whether a match arm transfers ownership of an
// owned i8* value into the phi (T1107) and, when the arm value is a nested tracked
// phi, the live per-path i1 ownership flag (T1208). Parallels
// clearMatchArmResultDropFlags. A block arm consults blockValueOwnedResult /
// blockValueOwnedFlag (set by genBlockValue); an expression arm inspects the live
// stmt temp / owned-local ident directly. Must be called BEFORE claimStringTemp
// neutralizes the temp's flag alloca.
func (c *Compiler) matchArmTransfersOwnership(arm ast.MatchArm, armVal value.Value) (bool, value.Value) {
	if arm.Block != nil {
		return c.blockValueOwnedResult, c.blockValueOwnedFlag
	}
	return c.exprResultTransfersOwnership(armVal, arm.Body), c.captureLiveTempFlag(armVal)
}

// captureLiveTempFlag loads a live tracked stmt temp's per-path drop flag in the
// current block (T1208), but ONLY when the temp's flag is genuinely PER-PATH — a
// flagPhi from a nested if/match/elvis result that is owned on one inner path and
// borrowed on another (stmtTemp.perPathFlag). For such a value the enclosing merge
// phi must thread this runtime flag; a whole-arm constant would drop the value on the
// borrowed inner path (use-after-free). Returns nil for every other value — ordinary
// clone()/handle temps (whose flag is a compile-time constant, so the caller's
// constant `owned` bit is already correct — this also covers a FAILABLE clone whose
// unwrapped result is itself a phi but whose flag is still constant), owned-local
// idents, borrows, and .rodata literals — leaving their existing IR (and the constant
// flag-phi incoming) unchanged. MUST be called before claimStringTemp stores a
// constant 0 into the temp's flag alloca (which would destroy the per-path info).
func (c *Compiler) captureLiveTempFlag(val value.Value) value.Value {
	if val == nil || c.block == nil || c.block.Term != nil {
		return nil
	}
	if idx, ok := c.stmtTempMap[val]; ok && idx >= 0 && c.stmtTemps[idx].perPathFlag {
		return c.block.NewLoad(irtypes.I1, c.stmtTemps[idx].dropFlag)
	}
	// T1211: value-struct / heap-user-type / Map merge results carry their per-path
	// ownership flag in a parallel alloca (they are not i8*, so never in stmtTempMap).
	if alloca, ok := c.mergeBoundStructFlag[val]; ok {
		return c.block.NewLoad(irtypes.I1, alloca)
	}
	return nil
}

// resultTransfersOwnedFlag reports whether an expression used as a scope result
// (match arm / if branch) is an owned-local ident (or nested match/if thereof)
// whose scope drop flag is live — i.e. clearResultDropFlags would clear a real
// flag, transferring ownership to the enclosing merge phi (T1107). Mirrors
// clearResultDropFlags's structure exactly so the ownership bit and the flag clear
// stay in agreement. A match-borrowed ident (no owned drop binding) does not
// transfer ownership.
func (c *Compiler) resultTransfersOwnedFlag(expr ast.Expr) bool {
	if expr == nil {
		return false
	}
	switch e := expr.(type) {
	case *ast.IdentExpr:
		if c.matchBorrowedIdents != nil && c.matchBorrowedIdents[e.Name] {
			return false
		}
		_, has := c.dropFlags[e.Name]
		return has
	case *ast.IfExpr:
		return c.blockResultTransfersOwnedFlag(e.Then) ||
			(e.Else != nil && c.blockResultTransfersOwnedFlag(e.Else))
	case *ast.MatchExpr:
		for _, arm := range e.Arms {
			if c.matchArmResultTransfersOwnedFlag(*arm) {
				return true
			}
		}
	}
	return false
}

// matchArmResultTransfersOwnedFlag is the match-arm variant of
// resultTransfersOwnedFlag (parallels clearMatchArmResultDropFlags).
func (c *Compiler) matchArmResultTransfersOwnedFlag(arm ast.MatchArm) bool {
	if arm.Body != nil {
		return c.resultTransfersOwnedFlag(arm.Body)
	}
	return c.blockResultTransfersOwnedFlag(arm.Block)
}

// blockResultTransfersOwnedFlag is the block variant of resultTransfersOwnedFlag
// (parallels clearBlockResultDropFlags).
func (c *Compiler) blockResultTransfersOwnedFlag(block *ast.Block) bool {
	if block == nil || len(block.Stmts) == 0 {
		return false
	}
	if es, ok := block.Stmts[len(block.Stmts)-1].(*ast.ExprStmt); ok {
		return c.resultTransfersOwnedFlag(es.Expr)
	}
	return false
}

// ownedI8PtrResultDrop resolves the drop function (and vector element type, if any)
// for a match/if expression result represented as a bare i8* owned heap value
// (T1107): string → promise_string_drop, Vector[T] → Vector.drop (+elem), and the
// single-owner native handles Arc/Weak/Mutex/MutexGuard/Task/Channel → their
// per-instantiation drop. Returns (nil, nil) for every other result type (value
// structs, heap user types, refs) — those are not i8* and are handled elsewhere.
// rt must already be substituted (typeSubst applied by the caller).
func (c *Compiler) ownedI8PtrResultDrop(rt types.Type) (*ir.Func, types.Type) {
	if rt == nil {
		return nil, nil
	}
	named := extractNamed(rt)
	if named == types.TypString {
		return c.funcs["promise_string_drop"], nil
	}
	if elemType, ok := types.AsVector(rt); ok {
		return c.funcs["Vector.drop"], elemType
	}
	if arcElem, ok := types.AsArc(rt); ok {
		return c.getOrCreateArcDrop(arcElem), nil
	}
	if weakElem, ok := types.AsWeak(rt); ok {
		return c.getOrCreateWeakDrop(weakElem), nil
	}
	if mutexElem, ok := types.AsMutex(rt); ok {
		return c.getOrCreateMutexDrop(mutexElem), nil
	}
	if taskElem, ok := types.AsTask(rt); ok {
		return c.getOrCreateTaskDrop(taskElem), nil
	}
	if _, ok := types.AsMutexGuard(rt); ok || named == types.TypMutexGuard {
		return c.funcs["MutexGuard.drop"], nil
	}
	if chElem, ok := types.AsChannel(rt); ok && chElem != nil {
		return c.getOrCreateChannelDrop(chElem), nil
	}
	return nil, nil
}

// trackMergeResultTemp registers a match/if expression phi result as an owned
// statement temp with a per-path ownership flag (T1107), so an owned i8* result
// (string, Vector[T], native handle) passed to a borrow parameter or discarded is
// freed exactly once at the caller's statement end. Mirrors trackElvisResultTemp:
// a parallel i1 phi over the same predecessors as the value phi, each incoming a
// compile-time 1 iff that arm transferred an owned value (matchArmInfo.owned),
// else 0. A consuming binding/return claims the phi by value identity, zeroing the
// flag alloca — no double free. Value-struct / heap-user-type results (phi not
// i8*) are skipped by the type guard, preserving their existing self-cleaning
// heapTemps behavior. Free-function-only via the tempTrackingEnabled gate.
func (c *Compiler) trackMergeResultTemp(result value.Value, resultType types.Type, arms []matchArmInfo) {
	if !c.tempTrackingEnabled || result == nil {
		return
	}
	if result.Type() != irtypes.I8Ptr {
		// T1211: value-struct / heap-user-type / Map merge results are not i8*, so
		// they never enter the stmtTemp drop machinery below. Instead record a
		// per-path ownership flag so a bound local's drop flag can be conditioned on
		// it (applyBoundMergeFlag), preventing a borrowed-path double-free.
		c.trackMergeResultStructFlag(result, resultType, arms)
		return
	}
	// T1106/T1107: a match/if phi feeding a `go`-call argument is transferred into
	// the goroutine frame by the go-arg machinery — a caller statement-end drop here
	// would race the goroutine's async read (a use-after-free / double-free).
	if c.suppressMergeResultTemp {
		return
	}
	if c.entryBlock == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if resultType != nil && isRefType(resultType) {
		return
	}
	if _, ok := c.stmtTempMap[result]; ok {
		return
	}
	anyOwned := false
	for _, a := range arms {
		if a.owned {
			anyOwned = true
			break
		}
	}
	if !anyOwned {
		return
	}
	dropFn, elemType := c.ownedI8PtrResultDrop(resultType)
	if dropFn == nil {
		return
	}
	// Per-path ownership flag phi over the exact same predecessors the value phi
	// used (arms that branch to mergeBlock), so the two phis stay consistent. Both
	// sit at the top of mergeBlock (phis-first); appendStmtTemp's stores follow.
	mergeBlock := c.block
	var incomings []*ir.Incoming
	for _, a := range arms {
		if a.end == nil || a.end.Term == nil {
			continue
		}
		br, ok := a.end.Term.(*ir.TermBr)
		if !ok || br.Target != mergeBlock {
			continue
		}
		// T1208: when the arm value was a nested tracked phi, use its live per-path
		// ownership flag (loaded in the arm block before claimStringTemp neutralized
		// it) instead of a whole-arm constant. A nested mixed owned/borrowed
		// conditional yields owned on one inner path and borrowed on the other; the
		// constant would drop the borrowed value (use-after-free). The load sits in the
		// arm's body block, which dominates a.end, so it legally dominates this edge.
		var flagVal value.Value
		if a.ownedFlag != nil {
			flagVal = a.ownedFlag
		} else {
			flag := int64(0)
			if a.owned {
				flag = 1
			}
			flagVal = constant.NewInt(irtypes.I1, flag)
		}
		incomings = append(incomings, &ir.Incoming{X: flagVal, Pred: a.end})
	}
	if len(incomings) == 0 {
		return
	}
	flagPhi := mergeBlock.NewPhi(incomings...)
	c.appendStmtTemp(result, dropFn, elemType, flagPhi)
}

// trackMergeResultStructFlag records a per-path ownership flag for a match/if merge
// phi whose result is a value struct — a heap user type (`{i8*,i8*}`), a Map, or any
// other droppable non-i8* result (T1211). Unlike the i8* path (trackMergeResultTemp),
// no statement-end drop obligation is attached: the arm-level heapTemp still
// self-cleans on the discard/return path, and a binding consumer gets its own
// bindingDrop. The ONLY problem this fixes is that maybeRegisterDrop arms the bound
// local's drop flag UNCONDITIONALLY; captureLiveTempFlag reads this flag and
// applyBoundMergeFlag stores it into the binding's drop flag, so a borrowed arm's
// value (a caller-owned param/field) is not dropped by the binding (no double-free).
// The flag phi mirrors the value phi's predecessors: 1 on arms that transferred an
// owned value (matchArmInfo.owned / .ownedFlag), 0 on borrowed arms. Stored in an
// entry i1 alloca so captureLiveTempFlag can reload it from any dominated block.
func (c *Compiler) trackMergeResultStructFlag(result value.Value, resultType types.Type, arms []matchArmInfo) {
	if c.suppressMergeResultTemp || c.entryBlock == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if resultType != nil && isRefType(resultType) {
		return
	}
	if _, ok := c.mergeBoundStructFlag[result]; ok {
		return
	}
	anyOwned := false
	for _, a := range arms {
		if a.owned || a.ownedFlag != nil {
			anyOwned = true
			break
		}
	}
	if !anyOwned {
		return
	}
	mergeBlock := c.block
	var incomings []*ir.Incoming
	for _, a := range arms {
		if a.end == nil || a.end.Term == nil {
			continue
		}
		br, ok := a.end.Term.(*ir.TermBr)
		if !ok || br.Target != mergeBlock {
			continue
		}
		var flagVal value.Value
		if a.ownedFlag != nil {
			flagVal = a.ownedFlag
		} else {
			flag := int64(0)
			if a.owned {
				flag = 1
			}
			flagVal = constant.NewInt(irtypes.I1, flag)
		}
		incomings = append(incomings, &ir.Incoming{X: flagVal, Pred: a.end})
	}
	if len(incomings) == 0 {
		return
	}
	flagPhi := mergeBlock.NewPhi(incomings...)
	dropFlag := c.createEntryAlloca(irtypes.I1)
	c.entryBlock.NewStore(constant.NewInt(irtypes.I1, 0), dropFlag)
	c.block.NewStore(flagPhi, dropFlag)
	if c.mergeBoundStructFlag == nil {
		c.mergeBoundStructFlag = make(map[value.Value]*ir.InstAlloca)
	}
	c.mergeBoundStructFlag[result] = dropFlag
}

// resultIsFreshOwnedHeapTemp reports (at compile time) whether a match/if arm result
// value is a freshly-constructed owned heap value struct — i.e. its instance pointer
// (field 1 of the `{i8*,i8*}` value struct) is a currently-live, unclaimed heapTemp
// (T1211). Used to set the arm's `owned` bit for value-struct merge phis: a heap
// constructor / `.clone()` result transfers ownership into the phi, whereas a borrowed
// param/field arm does not. Emits no IR — it inspects existing SSA.
//
// Two shapes produce an owned heap value struct:
//   - a constructor builds the struct via `insertvalue ..., instPtr, 1`, and the
//     inserted instPtr is itself the heapTemp key (checked via the insertvalue chain);
//   - a method call (e.g. `.clone()`) returns the struct directly, and field 1 is
//     tracked as a separate `extractvalue(result, 1)` heapTemp key (checked by scanning
//     the live heapTemp keys for that extractvalue).
func (c *Compiler) resultIsFreshOwnedHeapTemp(val value.Value) bool {
	if val == nil {
		return false
	}
	// Constructor shape: field 1 is the inserted instance pointer.
	cur := val
	for {
		iv, ok := cur.(*ir.InstInsertValue)
		if !ok {
			break
		}
		if len(iv.Indices) == 1 && iv.Indices[0] == 1 {
			elem := iv.Elem
			if bc, ok := elem.(*ir.InstBitCast); ok {
				elem = bc.From
			}
			if idx, tracked := c.heapTempMap[elem]; tracked && idx >= 0 {
				return true
			}
			break
		}
		cur = iv.X
	}
	// Call-result shape: field 1 was tracked as extractvalue(val, 1).
	for k, idx := range c.heapTempMap {
		if idx < 0 {
			continue
		}
		if ev, ok := k.(*ir.InstExtractValue); ok && ev.X == val &&
			len(ev.Indices) == 1 && ev.Indices[0] == 1 {
			return true
		}
	}
	return false
}

// genValueMatch generates a match expression on a non-enum value using comparison chains.
func (c *Compiler) genValueMatch(e *ast.MatchExpr, subject value.Value, subjectType types.Type, subjectDropFlag *ir.InstAlloca) value.Value {
	mergeBlock := c.newBlock("match.end")

	// T0496: the match expression's own result type, used as the contextual target
	// type for each arm so a bare `none` arm lowers to the right Optional shape.
	matchResultType := c.info.Types[e]
	if c.typeSubst != nil && matchResultType != nil {
		matchResultType = types.Substitute(matchResultType, c.typeSubst)
	}

	named := extractNamed(subjectType)

	// T1002: an owned or borrowed Optional subject (`T?`, or `T?&`/`T?~` from
	// e.g. Ref[T?].borrow) is not an enum, so it reaches genValueMatch. Its only
	// reachable literal pattern is `none` (there is no `some(x)` pattern; sema
	// rejects any other literal/expression arm on an Optional). Detect the shape
	// once — stripping a leading SharedRef/MutRef mirrors the T0850 if-unwrap fix
	// — and compare the present flag (field 0) for the `none` arm below.
	_, isOptSubject := unwrapRefsType(subjectType).(*types.Optional)

	// T0993: normalize a `this`-style heap receiver into the value struct
	// {vtable, instance} for RTTI type-pattern dispatch. genThisExpr returns the
	// raw i8* instance pointer for heap types, but a type-pattern arm (RTTI
	// extraction, name binding, member access on the bound name) expects the
	// uniform value representation. Without this, `match this { Subtype c => }`
	// would emit an `extractvalue` on an i8* and produce invalid IR. Guarded to
	// only fire when a type-pattern arm is actually present, so native-comparison
	// subjects (strings/primitives, also i8*) and value types are left untouched.
	hasTypePattern := false
	for _, arm := range e.Arms {
		if _, ok := arm.Pattern.(*ast.TypeBindingMatchPattern); ok {
			hasTypePattern = true
			break
		}
	}
	if _, isPtr := subject.Type().(*irtypes.PointerType); isPtr && hasTypePattern && named != nil && !named.IsValueType() {
		if layout := c.lookupTypeLayout(subjectType); layout != nil && layout.Value != nil {
			vst := layout.Value.LLVMType
			vtable := c.loadVtablePtrFromInstance(subject)
			instPtr := c.block.NewBitCast(subject, vst.Fields[1])
			vs := c.block.NewInsertValue(constant.NewUndef(vst), vtable, 0)
			subject = c.block.NewInsertValue(vs, instPtr, 1)
		}
	}

	var arms []matchArmInfo

	for i, arm := range e.Arms {
		switch p := arm.Pattern.(type) {
		case *ast.LiteralMatchPattern, *ast.ExpressionMatchPattern:
			var cond value.Value

			// T1002: `none` on an owned/borrowed Optional subject compares the
			// present flag (field 0 of the {i1, T} struct) — none ⇔ !present.
			// Mirrors the T0850 if-unwrap; the subject is only read (never bound
			// or moved out, as there is no `some(x)` pattern), so no drop
			// bookkeeping is needed. Skip the `==`-method path entirely so
			// genNoneLit does not emit an unused zero value.
			if lp, ok := p.(*ast.LiteralMatchPattern); ok && isOptSubject {
				if _, isNone := lp.Value.(*ast.NoneLit); isNone {
					flag := c.block.NewExtractValue(subject, 0)
					cond = c.block.NewICmp(enum.IPredEQ, flag, constant.NewInt(irtypes.I1, 0))
				}
			}

			if cond == nil {
				var patternVal value.Value
				switch pp := p.(type) {
				case *ast.LiteralMatchPattern:
					patternVal = c.genExpr(pp.Value)
				case *ast.ExpressionMatchPattern:
					patternVal = c.genExpr(pp.Expr)
				}

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
			}
			if cond == nil {
				panic(fmt.Sprintf("codegen: cannot compare match subject of type %s", subjectType))
			}

			armBlock := c.newBlock(fmt.Sprintf("match.arm%d", i))
			nextBlock := c.newBlock(fmt.Sprintf("match.next%d", i))
			c.block.NewCondBr(cond, armBlock, nextBlock)

			c.block = armBlock
			armVal := c.genMatchArmValue(arm, matchResultType)
			armOwned, armOwnedFlag := c.matchArmTransfersOwnership(*arm, armVal) // T1107/T1208: before claim
			c.claimStringTemp(armVal)                                            // T0073
			// T0975: clear drop flags for an owned arm-result ident (e.g. a task)
			// consumed by the match PHI and forwarded to a consuming `<-`. Emitted
			// in the selected arm's block, so the clear is path-conditional: the
			// un-selected arm's owner keeps its flag and is dropped exactly once at
			// scope exit. Mirrors genEnumMatch (B0242) for the non-enum value path.
			c.clearMatchArmResultDropFlags(*arm)
			armEnd := c.block
			if c.block.Term == nil {
				c.block.NewBr(mergeBlock)
			}
			arms = append(arms, matchArmInfo{val: armVal, end: armEnd, hasV: armVal != nil, owned: armOwned, ownedFlag: armOwnedFlag})

			c.block = nextBlock

		case *ast.TypeBindingMatchPattern:
			// T0993: class type-pattern arm — dispatch on the runtime subtype via
			// RTTI (the SAME promise_type_is machinery as `is`/`as!`), NOT an exact
			// type-id comparison. Without this case the arm emitted nothing and
			// control silently fell through to `_` (the merged T0992 miscompilation).
			targetNamed := c.lookupNamedType(p.TypeName)
			if targetNamed == nil {
				panic(fmt.Sprintf("codegen: undefined type %s in match type-pattern", p.TypeName))
			}
			targetID := c.assignTypeID(targetNamed)
			instance := c.instancePtrForRTTI(subject, subjectType)
			variantPtr := c.loadVariantPtr(instance)
			result := c.block.NewCall(c.funcs["promise_type_is"],
				variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
			cond := c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))

			armBlock := c.newBlock(fmt.Sprintf("match.arm%d", i))
			nextBlock := c.newBlock(fmt.Sprintf("match.next%d", i))
			c.block.NewCondBr(cond, armBlock, nextBlock)

			c.block = armBlock
			// Bind the narrowed view. The value representation is uniform
			// ({vtable, instance}); sema bound the name to the subtype, so member
			// access inside the arm resolves against the subtype layout.
			var savedBinding *ir.InstAlloca
			var hadBinding bool
			if p.Binding != "_" {
				lt := subject.Type()
				alloca := c.createEntryAlloca(lt)
				alloca.SetName(c.uniqueLocalName(p.Binding))
				c.block.NewStore(subject, alloca)
				savedBinding, hadBinding = c.locals[p.Binding]
				c.locals[p.Binding] = alloca
			}

			// Optional guard: RTTI match AND guard must both hold.
			if arm.Guard != nil {
				guardVal := c.genExpr(arm.Guard)
				guardArmBlock := c.newBlock(fmt.Sprintf("match.arm%d.guard", i))
				c.block.NewCondBr(guardVal, guardArmBlock, nextBlock)
				c.block = guardArmBlock
			}

			armVal := c.genMatchArmValue(arm, matchResultType)
			armOwned, armOwnedFlag := c.matchArmTransfersOwnership(*arm, armVal) // T1107/T1208: before claim
			c.claimStringTemp(armVal)                                            // T0073
			// T0975: clear drop flags for an owned arm-result ident (e.g. a task)
			// consumed by the match PHI and forwarded to a consuming `<-`. Emitted
			// in the selected arm's block, so the clear is path-conditional: the
			// un-selected arm's owner keeps its flag and is dropped exactly once at
			// scope exit. Mirrors genEnumMatch (B0242) for the non-enum value path.
			c.clearMatchArmResultDropFlags(*arm)
			armEnd := c.block
			if c.block.Term == nil {
				c.block.NewBr(mergeBlock)
			}
			arms = append(arms, matchArmInfo{val: armVal, end: armEnd, hasV: armVal != nil, owned: armOwned, ownedFlag: armOwnedFlag})

			// Restore any shadowed binding so later arms / merge don't see it.
			if p.Binding != "_" {
				if hadBinding {
					c.locals[p.Binding] = savedBinding
				} else {
					delete(c.locals, p.Binding)
				}
			}

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
				// T1187: a whole-value name binding ALIASES the owned-rvalue optional
				// subject (no dup). Alias its drop flag so a move-out of the binding
				// (`o := match make() { none => none, h => h }`) clears the subject
				// drop too — otherwise the payload is dropped here AND by its new
				// owner. clearMatchArmResultDropFlags clears this shared flag when the
				// bound name is the arm result and escapes (mirrors T1119 enum path).
				if subjectDropFlag != nil {
					c.dropFlags[np.Name] = subjectDropFlag
				}
			}

			// If there's a guard, evaluate it and conditionally branch
			if arm.Guard != nil {
				guardVal := c.genExpr(arm.Guard)
				armBlock := c.newBlock(fmt.Sprintf("match.arm%d", i))
				nextBlock := c.newBlock(fmt.Sprintf("match.next%d", i))
				c.block.NewCondBr(guardVal, armBlock, nextBlock)

				c.block = armBlock
				armVal := c.genMatchArmValue(arm, matchResultType)
				armOwned, armOwnedFlag := c.matchArmTransfersOwnership(*arm, armVal) // T1107/T1208: before claim
				c.claimStringTemp(armVal)                                            // T0073
				// T0975: clear drop flags for an owned arm-result ident (e.g. a task)
				// consumed by the match PHI and forwarded to a consuming `<-`. Emitted
				// in the selected arm's block, so the clear is path-conditional: the
				// un-selected arm's owner keeps its flag and is dropped exactly once at
				// scope exit. Mirrors genEnumMatch (B0242) for the non-enum value path.
				c.clearMatchArmResultDropFlags(*arm)
				armEnd := c.block
				if c.block.Term == nil {
					c.block.NewBr(mergeBlock)
				}
				arms = append(arms, matchArmInfo{val: armVal, end: armEnd, hasV: armVal != nil, owned: armOwned, ownedFlag: armOwnedFlag})

				c.block = nextBlock
				// Guard failed — continue to next arm (don't return early)
			} else {
				// No guard — unconditional default arm
				armVal := c.genMatchArmValue(arm, matchResultType)
				armOwned, armOwnedFlag := c.matchArmTransfersOwnership(*arm, armVal) // T1107/T1208: before claim
				c.claimStringTemp(armVal)                                            // T0073
				// T0975: clear drop flags for an owned arm-result ident (e.g. a task)
				// consumed by the match PHI and forwarded to a consuming `<-`. Emitted
				// in the selected arm's block, so the clear is path-conditional: the
				// un-selected arm's owner keeps its flag and is dropped exactly once at
				// scope exit. Mirrors genEnumMatch (B0242) for the non-enum value path.
				c.clearMatchArmResultDropFlags(*arm)
				armEnd := c.block
				if c.block.Term == nil {
					c.block.NewBr(mergeBlock)
				}
				arms = append(arms, matchArmInfo{val: armVal, end: armEnd, hasV: armVal != nil, owned: armOwned, ownedFlag: armOwnedFlag})

				// After an unguarded wildcard/name, no more arms need checking
				c.block = mergeBlock
				return c.buildMatchPhi(mergeBlock, arms, matchResultType)
			}
		}
	}

	// If we fell through without a default, branch to merge
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	c.block = mergeBlock
	return c.buildMatchPhi(mergeBlock, arms, matchResultType)
}

// patternBindingNames returns the local-variable names a match-arm pattern
// introduces (skipping `_`). Used by genEnumMatch to snapshot and restore the
// c.locals/c.dropFlags entries those names shadow, so an arm binding that reuses
// the scrutinee's name (e.g. `match b { Msg.Text(b) => ... }`) does not leak the
// arm-scoped binding into the enclosing block or sibling arms (T1155).
func patternBindingNames(pat ast.MatchPattern) []string {
	switch p := pat.(type) {
	case *ast.EnumDestructureMatchPattern:
		return nonWildcardNames(p.Bindings)
	case *ast.ShortDestructureMatchPattern:
		return nonWildcardNames(p.Bindings)
	case *ast.NameMatchPattern:
		if p.Name != "_" {
			return []string{p.Name}
		}
	}
	return nil
}

// nonWildcardNames filters out `_` placeholders from a list of binding names.
func nonWildcardNames(names []string) []string {
	var out []string
	for _, n := range names {
		if n != "_" {
			out = append(out, n)
		}
	}
	return out
}

// bindMatchPattern binds pattern variables from a match arm into the current scope.
func (c *Compiler) bindMatchPattern(pat ast.MatchPattern, subjectExpr ast.Expr, subject value.Value, enum *types.Enum, layout *TypeDeclLayout, enumHasDrop bool, subjectType types.Type, subjectDropFlag *ir.InstAlloca) {
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
			// T1119: A whole-value name binding ALIASES the owned-rvalue subject
			// (no dup). If the T1119 subject-drop is active, alias its flag onto
			// this binding so a move of the binding (`return h` / `take(move h)`)
			// clears the subject drop too — otherwise the value is dropped both
			// here and by its new owner (use-after-free). When the binding is NOT
			// moved, the flag stays set and the subject is dropped exactly once.
			if subjectDropFlag != nil {
				c.dropFlags[p.Name] = subjectDropFlag
			}
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
			c.markMatchBorrowedBinding(binding, resolved)
			// T1259/T1264: a DIRECT closure field OR a value-copying container of
			// closures aliases the enum's heap env (freed by the enum's own drop);
			// the env can't be deep-cloned, so the binding is a borrow — else a
			// downstream `hs := gs` var-decl would register an owning Vector/env-free
			// drop for `hs` and double-free the shared env against the enum's drop.
			// markMatchBorrowedBinding only covers Optional/Array; the Deep predicate
			// catches the container case (shallow FirstFieldNestedClosure treats a
			// top-level Vector/Map/Set as opaque) while keeping refcounted handles
			// opaque. Arm-scoped: armBorrowedSnapshot reverts it at arm exit.
			if sema.FirstFieldNestedClosureDeep(resolved) != nil {
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

// markMatchBorrowedBinding records `name` as a match-borrowed alias when its
// resolved type is an Optional/Array whose inner heap value stays owned by the
// enum's variant data (matchBindingIsBorrow). Shared by the match-arm
// (bindEnumDestructure) and if…is (bindIsDestructureEnum) destructure paths so
// the mark is set identically in both; callers gate on enumHasDrop. A downstream
// escape/whole-payload var-decl then clones exactly once (T0485/T1179). No-op
// when the binding does not alias owned variant data.
func (c *Compiler) markMatchBorrowedBinding(name string, resolved types.Type) {
	if !c.matchBindingIsBorrow(resolved) {
		return
	}
	if c.matchBorrowedIdents == nil {
		c.matchBorrowedIdents = make(map[string]bool)
	}
	c.matchBorrowedIdents[name] = true
}

// cloneBorrowedWholePayloadVarDecl deep-clones a whole match-borrowed
// Array/Optional heap-user payload when it is bound by a plain var-decl to a new
// owned local (T1179). `if…is` / `match` bind such a payload as a shallow borrow
// (matchBorrowedIdents) with NO drop, because its inner heap value is still owned
// by the enum's variant data (freed once by the synth enum drop). A plain
// var-decl (`T[N] copy = value;` / `T? copy = value;`) instead gives the new
// local an OWNING drop — so without cloning, both the local's drop and the enum
// synth drop would free the same instances → double-free/UAF. Clone exactly once
// here so the local owns independent data; the source binding stays a borrow.
// Gated on matchBorrowedIdents membership AND matchBindingIsBorrow so ordinary
// owned-local moves are untouched. Returns val unchanged when not applicable.
func (c *Compiler) cloneBorrowedWholePayloadVarDecl(val value.Value, valueExpr ast.Expr, resolvedType types.Type) value.Value {
	if val == nil || resolvedType == nil {
		return val
	}
	id, ok := unwrapDestructureParens(valueExpr).(*ast.IdentExpr)
	if !ok {
		return val
	}
	if c.matchBorrowedIdents == nil || !c.matchBorrowedIdents[id.Name] {
		return val
	}
	if !c.matchBindingIsBorrow(resolvedType) {
		return val
	}
	return c.cloneByType(val, resolvedType)
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
	if c.typeNeedsMatchDup(resolved) {
		return true
	}
	// T1110: A droppable enum WITHOUT a clone() method whose variant payloads are
	// all shallow-dup-safe (e.g. `Holder.Pair(P p)` where P carries a Ref). This is
	// checked only here — NOT in typeNeedsMatchDup — so it does not flow into the
	// container-clone safety gate (typeArgSafeForCloneDup), which must stay
	// conservative for recursive/container-bearing enums like JsonNode (B0289).
	return c.enumMatchDupSafe(resolved, nil)
}

// enumMatchDupSafe reports whether a droppable enum value can be independently
// deep-copied by cloneResolvedValue's memcpy + dupEnumElementInPlace path
// (T1110). Eligible only when the enum has drop work AND every variant field is
// itself shallow-dup-safe — strings, channels, Arc/Ref/Weak, primitives/value/
// copy, vectors of non-droppable elements, heapTypeSafeToDup user types, and
// tuples/nested enums thereof. Excludes Map/Set/clone-bearing container fields
// and self-recursive enums, whose deep copy needs full clone() logic that this
// shallow path cannot replicate (would leak / double-free).
func (c *Compiler) enumMatchDupSafe(resolved types.Type, seen map[*types.Enum]bool) bool {
	enum := extractEnum(resolved)
	if enum == nil {
		return false
	}
	if !c.vecElemNeedsEnumDrop(resolved) {
		return false
	}
	if seen == nil {
		seen = make(map[*types.Enum]bool)
	}
	if seen[enum] {
		return false // self-recursive — needs real clone(), not shallow dup
	}
	seen[enum] = true
	var subst map[*types.TypeParam]types.Type
	if inst, ok := resolved.(*types.Instance); ok && len(enum.TypeParams()) > 0 {
		subst = types.BuildSubstMap(enum.TypeParams(), inst.TypeArgs())
	} else if c.typeSubst != nil {
		subst = c.typeSubst
	}
	for _, v := range enum.Variants() {
		for _, f := range v.Fields() {
			fType := f.Type()
			if subst != nil {
				fType = types.Substitute(fType, subst)
			}
			if !c.matchDupFieldSafe(fType, seen) {
				return false
			}
		}
	}
	return true
}

// matchDupFieldSafe reports whether a single variant-field type can be dup'd by
// emitVariantFieldDup without invoking clone()-based container copy (T1110).
func (c *Compiler) matchDupFieldSafe(fType types.Type, seen map[*types.Enum]bool) bool {
	// T1259/T1264: a variant field that transitively nests a closure — through a
	// DIRECT closure field (*types.Signature) OR a value-copying container
	// (Vector/Map/Set of closures) — is NOT dup-safe. emitVariantFieldDup /
	// dupVector's element-clone path zeroes each closure's opaque env (T0813) →
	// null {fn,env} → SEGV on invoke. Consult the same FirstFieldNestedClosureDeep
	// predicate as typeNeedsMatchDup and the two borrow gates
	// (isClosureAggregateBorrow, ownership closureAggregateBorrowSource) — single
	// source of truth. The Deep variant treats fType as a nested field so a
	// top-level container-of-closures is recursed into; refcounted handles
	// (Ref/Weak/Channel/...) stay opaque there, so Map[K, Ref[() -> int]] etc. are
	// not over-suppressed. FirstFieldNestedClosureDeep(Signature) returns the
	// signature itself, subsuming the direct-closure case.
	if sema.FirstFieldNestedClosureDeep(fType) != nil {
		return false
	}
	if en := extractEnum(fType); en != nil {
		return c.enumMatchDupSafe(fType, seen)
	}
	named := extractNamed(fType)
	if named == nil {
		if tup, ok := fType.(*types.Tuple); ok {
			for _, e := range tup.Elems() {
				if !c.matchDupFieldSafe(e, seen) {
					return false
				}
			}
			return true
		}
		// Non-named, non-tuple (scalar/ref/fn-ptr) — bit copy is safe.
		return true
	}
	if named == types.TypString {
		return true
	}
	if _, isChan := types.AsChannel(fType); isChan || named == types.TypChannel {
		return true
	}
	if types.IsArc(fType) || named == types.TypArc || types.IsWeak(fType) || named == types.TypWeak {
		return true
	}
	if elemType, isVec := types.AsVector(fType); isVec || named == types.TypVector {
		// T1118: a droppable-element vector is dup-safe iff its element type is —
		// emitVariantFieldDup deep-copies it via dupVector + emitVectorElementCloneLoop.
		// Recurse so the `seen` cycle guard rejects Vector[recursive-enum].
		if isVec && fieldTypeNeedsDrop(elemType) {
			return c.matchDupFieldSafe(elemType, seen)
		}
		return true
	}
	if named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) || named.IsStructural() {
		return true
	}
	// Heap user type — safe via the shallow memcpy + per-field dup path …
	if c.heapTypeSafeToDup(named, fType, nil) {
		return true
	}
	// … or via a clone()-bearing container (Map/Set/user type). T1118:
	// emitVariantFieldDup routes the latter through cloneHeapElement → the type's
	// clone(). T1129: this whole check is STRUCTURAL — `LookupMethod`/`IsClone`
	// plus a `seen`-threaded recursion over the container's concrete type args —
	// deliberately NOT a probe of c.funcs. The c.funcs population changes between
	// the declare and define phases (sibling and self clone stubs appear), so a
	// c.funcs-based answer (the old namedHasCloneFunc → typeArgSafeForCloneDup →
	// typeNeedsMatchDup path) flipped enumNeedsSynthClone across phases — leaving
	// a synthesized @Enum.clone declared-but-undefined (recursive enums) or
	// spuriously synthesized (non-recursive Map-bearing enums). The structural
	// check is phase-invariant.
	if !named.IsClone() && named.LookupMethod("clone") == nil {
		return false // no deep-copying clone() available → not safe
	}
	// A type arg that is one of the enums currently under analysis (in `seen`)
	// marks a recursion cycle: the container's clone would need that enum's
	// clone(), which only exists as a synthesized recursive clone — so the enum
	// is NOT inline-dup-safe and must get one. The `seen` guard inside
	// enumMatchDupSafe returns false for such an arg. Other args are checked for
	// their own dup-safety so an un-duppable element (e.g. a droppable user type
	// without a clone) still makes the container unsafe.
	if inst, ok := fType.(*types.Instance); ok {
		for _, arg := range inst.TypeArgs() {
			if !c.matchDupFieldSafe(arg, seen) {
				return false
			}
		}
	}
	return true
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
	// T1262: a value-copying container (Vector/Map/Set) that transitively nests a
	// closure is NOT match-dup-safe — dupVector's element-clone path zeroes each
	// closure element's opaque env (T0813) → null {fn,env} → SEGV on invoke. Leave
	// such a value ALIASED (no null-dup) so the read from an aliasing container
	// (Map[K, Vector[() -> int]].[]) is a true borrow. FirstFieldNestedClosureDeep
	// treats the top container as a FIELD (recurses TypeArgs) while keeping
	// refcounted handles (Ref/Weak/...) opaque, so Map[K, Ref[...]] is unaffected.
	// Kept in lockstep with the two borrow gates (ownership
	// closureAggregateBorrowSource, codegen isClosureAggregateBorrow), which use the
	// same Deep predicate — and, via `!typeNeedsMatchDup`, with
	// isContainerIndexUnwrapSource/mapIndexReadAliasesStorage.
	if sema.FirstFieldNestedClosureDeep(resolved) != nil {
		return false
	}
	named := extractNamed(resolved)
	if named == nil {
		// B0244: Check for enum types — clone if clone method exists in c.funcs
		// or if the enum is marked `clone (function may not be declared yet due
		// to cross-module compilation order — forward-declared lazily in cloneEnumValue).
		if enum := extractEnum(resolved); enum != nil {
			if _, exists := c.funcs[c.enumCloneFuncName(enum, resolved)]; exists {
				return true
			}
			// T1131: a recursive / container-bearing droppable module enum gets a
			// compiler-synthesized recursive clone whose stub may not yet be in
			// c.funcs at this compile point (cross-module order: Map[K,V].[] in std
			// compiles before mathlib declares ModTree.clone). Use the phase-invariant
			// structural predicate so the dup is still inserted — cloneEnumValue
			// forward-declares the synth clone lazily.
			if c.enumNeedsSynthClone(enum, resolved) {
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
	// T1117: A direct Arc/Weak element (e.g. Map[K, Ref[int]]) is safely
	// match-dup'd by cloneResolvedValue's dupArc/dupWeak refcount increment.
	if _, ok := types.AsArc(resolved); ok || named == types.TypArc {
		return true
	}
	if _, ok := types.AsWeak(resolved); ok || named == types.TypWeak {
		return true
	}
	// T1292: A non-value structural interface value is a heap-boxed view whose box
	// must be deep-cloned (cloneStructuralView) when extracted from a droppable enum
	// (e.g. Slot[K, Showable] inside Map[K, Showable]) — otherwise the match binding
	// aliases the container's box → double-free. Must precede the IsStructural bail-
	// out below (which would leave the box shallow-aliased).
	if named.IsStructural() && !named.IsValueType() {
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
		// T1230: a field that transitively nests a closure (*types.Signature)
		// through user-type/enum/Optional/Tuple/Array — but NOT through a
		// refcounted std container (Ref/Weak/...) — is NOT dup-safe. The closure
		// env (captured frame) is opaque and cannot be deep-cloned, so
		// dupHeapValueFields zeroes the cloned slot (T0813), producing a null
		// {fn,env} fat pointer. Judging such a struct dup-safe makes the aliasing
		// container read path (Map[K,V].[] `return v`) dup-and-zero the closure →
		// SEGV when the caller invokes it. Treat it as un-dup-safe so the read
		// returns a shallow alias with the env intact; ownership marks the local
		// Borrowed (closureAggregateBorrowSource / FirstFieldNestedClosure).
		//
		// Deep variant (T1260): fType is a FIELD, so a value-copying container of
		// closures (`Vector[() -> int]`) must ALSO count — its element deep-copy
		// zeroes the env just like a bare struct-of-closure. (A bare top-level
		// container read keeps its own null-dup path via FirstFieldNestedClosure.)
		if sema.FirstFieldNestedClosureDeep(fType) != nil {
			return false
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

		// T1110/T1117: Arc/Ref and Weak fields → safe to dup. They are
		// reference-counted handles: dupArc/dupWeak bump the (strong/weak) count
		// rather than aliasing, and dupHeapValueFields already emits those bumps
		// for these field kinds. A Ref field carries an explicit `drop` method, so
		// without this case the generic "nested heap user type" recursion below
		// rejects it (drop method → not safe) and the whole containing struct is
		// wrongly treated as un-dup'able — leaving match-destructured copies (and
		// struct{Ref} map read-backs, T1117) aliasing the source's Ref → UAF /
		// double-free.
		if types.IsArc(fType) || fNamed == types.TypArc {
			continue
		}
		if types.IsWeak(fType) || fNamed == types.TypWeak {
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
	arcElem, isArc := types.AsArc(resolvedType)
	weakElem, isWeak := types.AsWeak(resolvedType)

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
	} else if isArc || named == types.TypArc {
		// T1117: a direct Arc/Ref element — dupArc increments the strong count so
		// the bound copy shares the allocation with a correct refcount. The bare
		// i8* handle must not fall through to cloneHeapElement (which expects a
		// {vtable,instance} value struct).
		dupVal = c.dupArc(val, arcElem)
	} else if isWeak || named == types.TypWeak {
		// T1117: a direct Weak element — dupWeak increments the weak count.
		welem := weakElem
		if welem == nil {
			welem = resolvedType
		}
		dupVal = c.dupWeak(val, welem)
	} else if named != nil && named.IsStructural() && !named.IsValueType() {
		// T1292: A non-value structural interface value is a heap-boxed view
		// ({vtable, instance}). Deep-clone the box via cloneStructuralView (T1284)
		// so the bound copy owns an independent box — the enum/heap-user else below
		// assumes a heap-user value struct and would misread the view.
		dupVal = c.cloneStructuralView(val)
	} else if tup, isTup := resolvedType.(*types.Tuple); isTup {
		// T0667: deep-clone each element so heap members become independent.
		// cloneByType handles bit-copy elements (scalars pass through
		// unchanged), Optional elements (none-check), and recurses for
		// string/vector/channel/enum/heap-user/nested-tuple. A bare bit copy
		// would alias the heap members → double-free when both original and
		// clone drop. Covers T0605 (type) and T0607 (enum) tuple shapes
		// uniformly (both lower through cloneByType→cloneResolvedValue).
		result := val
		for i, elemType := range tup.Elems() {
			resolvedElem := elemType
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
			}
			elemVal := c.block.NewExtractValue(result, uint64(i))
			clonedElem := c.cloneByType(elemVal, resolvedElem)
			if clonedElem != nil && clonedElem != elemVal {
				result = c.block.NewInsertValue(result, clonedElem, uint64(i))
			}
		}
		dupVal = result
	} else if arr, isArr := resolvedType.(*types.Array); isArr {
		// T1179/T0662: deep-clone each element so a whole fixed-Array payload
		// becomes independent. Mirrors the Tuple case above. cloneByType handles
		// bit-copy elements (scalar/value/copy arrays pass through unchanged via
		// the isAutoCloneBitCopy guard), Optional elements (none-check), and
		// recurses for heap-bearing elements (string/vector/channel/enum/
		// heap-user/tuple/nested-array). A bare aggregate copy would alias the
		// element heap pointers (string buffers, vector/map allocations, heap-user
		// instances, enum variant data) → double-free when both original and clone
		// drop (or a match-borrowed `T[N] copy = value;` var-decl aliases the
		// enum's variant data). Covers T0605 (type `[N]T` field) and T0607 (enum
		// `[N]T` variant field) shapes uniformly since both lower through
		// cloneByType→cloneResolvedValue.
		result := val
		resolvedElem := arr.Elem()
		if c.typeSubst != nil {
			resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
		}
		for i := int64(0); i < arr.Size(); i++ {
			elemVal := c.block.NewExtractValue(result, uint64(i))
			clonedElem := c.cloneByType(elemVal, resolvedElem)
			if clonedElem != nil && clonedElem != elemVal {
				result = c.block.NewInsertValue(result, clonedElem, uint64(i))
			}
		}
		dupVal = result
	} else if cloned, ok := c.cloneEnumValue(val, resolvedType); ok {
		// B0244: Enum with clone — deep-copy via clone method.
		dupVal = cloned
	} else if extractEnum(resolvedType) != nil && c.enumMatchDupSafe(resolvedType, nil) {
		// T1110: Droppable enum WITHOUT a clone() method whose variant payloads are
		// all shallow-dup-safe (e.g. `Holder.Pair(P p)` where P carries a Ref).
		// Mirror the vector element clone loop's B0290 path: spill the value, dup
		// each droppable variant field in place (emitVariantFieldDup handles
		// string/vector/channel/Arc/Weak/heap-user/tuple/nested-enum), then reload
		// the now-independent copy. Without this the match binding aliases the
		// container's variant payload → double-free.
		//
		// Gated on enumMatchDupSafe (same predicate as matchFieldNeedsDup, Part B
		// hunk 1): a container-bearing/recursive enum like JsonNode is NOT
		// shallow-dup-safe — dupEnumElementInPlace would shallow-copy its Map/Vector
		// field and alias the buffer → double-free. Those fall through to the `else`
		// cloneHeapElement path (their original, working behavior).
		spill := c.createEntryAlloca(val.Type())
		c.block.NewStore(val, spill)
		c.dupEnumElementInPlace(c.block.NewBitCast(spill, irtypes.I8Ptr), resolvedType)
		dupVal = c.block.NewLoad(val.Type(), spill)
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

	// T1292: A non-value structural interface binding is a freshly cloned, owned
	// heap box ({vtable, instance}). maybeRegisterDrop deliberately excludes
	// structural types, so it must be dropped via the RTTI-dispatched structural
	// free (honoring the concrete drop_fn). The drop flag is cleared at move sites
	// (so `result[k] = v` in Map.clone/_rehash doesn't double-free) and the box is
	// dropped at arm exit otherwise.
	if named := extractNamed(resolvedType); named != nil && named.IsStructural() && !named.IsValueType() {
		c.maybeRegisterStructuralParamFree(name, bindAlloca, resolvedType)
		return
	}

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
	// T0667: a tuple is a bit copy iff every element is — recurse so a tuple
	// carrying a heap member (string/vector/map/enum/heap-user/nested-tuple)
	// is NOT bit-copied by cloneByType's short-circuit (that would alias the
	// member → double-free when both original and clone drop). A pure scalar
	// tuple stays a bit copy (preserves the prior non-named-fallthrough
	// behavior). Mirrors the *types.Optional recursion above.
	if tup, isTup := t.(*types.Tuple); isTup {
		for _, elem := range tup.Elems() {
			if !c.isAutoCloneBitCopy(elem) {
				return false
			}
		}
		return true
	}
	// T1179/T0662: a fixed array `[N]T` is a bit copy iff its element is —
	// recurse so an array of a heap-bearing element (string/vector/map/enum/
	// heap-user) is NOT short-circuited by cloneByType's isAutoCloneBitCopy guard
	// (that would alias the element heap pointers → double-free when both original
	// and clone drop, or leave a match-borrowed whole-array var-decl aliasing the
	// enum's variant data). A pure scalar/value/copy array stays a bit copy
	// (preserves the prior non-named fallthrough behavior). Mirrors the
	// *types.Optional and *types.Tuple recursions above.
	if arr, isArr := t.(*types.Array); isArr {
		return c.isAutoCloneBitCopy(arr.Elem())
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
	// T1292: A non-value structural interface value is a heap-boxed view whose box
	// AutoClone must deep-copy (cloneStructuralView) — NOT bit-copy (that would alias
	// the box → double-free). Route to cloneResolvedValue's structural arm. Must
	// precede the IsStructural() bit-copy classification below.
	if named.IsStructural() && !named.IsValueType() {
		return false
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

	// T0496: Propagate the if-expression's result type as the contextual target
	// type for each arm so a bare `none` arm lowers to a zero value of the shared
	// result type (e.g. an Optional struct) rather than the `i1 0` void fallback,
	// which would produce a phi-type mismatch. T1189: sema's joinBranchTypes unifies
	// a `none` arm with a value arm into `T?`, but a bare value arm still lowers to
	// the inner `T`; wrapArmValueOptional (applied before the phi below) rewraps it
	// so both incomings share the `{ i1, T }` shape.
	ifResultType := c.info.Types[e]
	if c.typeSubst != nil && ifResultType != nil {
		ifResultType = types.Substitute(ifResultType, c.typeSubst)
	}

	// Then branch
	c.block = thenBlock
	savedTarget := c.targetType
	c.targetType = ifResultType
	thenVal := c.genBlockValue(e.Then)
	c.targetType = savedTarget
	thenOwned := c.blockValueOwnedResult   // T1107: genBlockValue recorded ownership
	thenOwnedFlag := c.blockValueOwnedFlag // T1208: live per-path flag (nested tracked temp)
	c.claimStringTemp(thenVal)             // T0073
	thenEnd := c.block
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Else branch
	c.block = elseBlock
	// Re-set per-arm (not once for both): a nested expression inside the then arm
	// can clear c.targetType, which would otherwise leak into the else `none`.
	c.targetType = ifResultType
	elseVal := c.genBlockValue(e.Else)
	c.targetType = savedTarget
	elseOwned := c.blockValueOwnedResult   // T1107: genBlockValue recorded ownership
	elseOwnedFlag := c.blockValueOwnedFlag // T1208: live per-path flag (nested tracked temp)
	c.claimStringTemp(elseVal)             // T0073
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

	// T1189: rewrap a bare inner value arm to the shared Optional shape so both
	// phi incomings agree (a `none` arm already produced `{ i1, T }`). The
	// insertvalue must land in each arm's own end block (before its br to merge) so
	// it dominates the phi incoming edge — temporarily retarget c.block per arm.
	if thenVal != nil {
		c.block = thenEnd
		thenVal = c.wrapArmValueOptional(thenVal, ifResultType)
	}
	if elseVal != nil {
		c.block = elseEnd
		elseVal = c.wrapArmValueOptional(elseVal, ifResultType)
	}
	c.block = mergeBlock

	// If both branches produce values, create a phi node
	if thenVal != nil && elseVal != nil {
		phi := mergeBlock.NewPhi(
			&ir.Incoming{X: thenVal, Pred: thenEnd},
			&ir.Incoming{X: elseVal, Pred: elseEnd},
		)
		// T1107: register an owned i8* phi (string/vector/native handle) as a
		// statement temp so a borrow/discard consumer frees it exactly once. Reuses
		// the match path via two synthetic arm records (only .end and .owned are
		// consulted). A bound/return consumer claims the phi and neutralizes the flag.
		c.trackMergeResultTemp(phi, ifResultType, []matchArmInfo{
			{end: thenEnd, owned: thenOwned, ownedFlag: thenOwnedFlag},
			{end: elseEnd, owned: elseOwned, ownedFlag: elseOwnedFlag},
		})
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

	// T0770: For the regular (non-optional-wrapping) recovery path, the recovery
	// body yields the recovered type directly. Expose it as the target type so a
	// bare `none` recovery (`expr? e { none }` on a `T?`-typed failable) lowers to
	// the full optional struct, not a bare i1. The optional-recovery path (which
	// wraps the body's inner T) must not see this, so it is left nil there.
	var recoveredTargetType types.Type
	if !c.info.OptionalRecoveryHandlers[e] {
		rt := c.info.Types[e]
		if rt != nil && c.typeSubst != nil {
			rt = types.Substitute(rt, c.typeSubst)
		}
		recoveredTargetType = rt
	}

	// T0792: when the recovery/else body's result is consumed as a borrow
	// (`T&`/`T~`), genBlockValue must read the last expr as a pure alias — no
	// dup, no owned-temp tracking. Otherwise the inner expr's natural owned type
	// (e.g. `r.d[0]` → `string`) sets dupStringFieldAccess and allocates a copy
	// that the borrow bind site never takes ownership of → leak.
	borrowRecovery := recoveredTargetType != nil && isRefType(recoveredTargetType)

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
			savedTarget := c.targetType
			if recoveredTargetType != nil {
				c.targetType = recoveredTargetType
			}
			savedBorrow := c.borrowBlockResult
			c.borrowBlockResult = borrowRecovery
			noMatchVal = c.genBlockValue(e.ElseBody)
			c.borrowBlockResult = savedBorrow
			c.targetType = savedTarget
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
	savedHandlerTarget := c.targetType
	if recoveredTargetType != nil {
		c.targetType = recoveredTargetType
	}
	savedHandlerBorrow := c.borrowBlockResult
	c.borrowBlockResult = borrowRecovery
	handlerVal := c.genBlockValue(e.Body)
	c.borrowBlockResult = savedHandlerBorrow
	c.targetType = savedHandlerTarget
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
	vtablePtr := c.loadVtablePtrFromInstance(errPtr)
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
		// T0784: same for `x as!/as T` element — cast subject is moved into
		// the tuple slot, so suppress its scope-exit drop binding. T0849: for
		// the conditional `as` form, drop iff the downcast failed.
		if ident := c.castSubjectMovableIdent(elem); ident != nil {
			c.consumeCastSubjectDropFlag(elem, ident.Name)
		}
		// T1073: `(o!, ..)` — force-unwrap moves the inner out of the source
		// optional into the tuple slot (which the tuple's drop frees). Neutralize
		// the source optional's present flag so its scope-exit drop doesn't
		// double-free the moved inner.
		c.neutralizeForceUnwrapElem(elem)
		// T0371: Claim heap-tracked field temps so they are not double-freed at
		// stmt end (their ownership is now in the tuple slot). Mirrors the
		// pattern used in genArrayLit / genMapLit. Without these claims:
		//   - string/vector/channel stmt-temps would self-clean at stmt end,
		//     leaving a dangling pointer in the tuple slot (case D/E garbage).
		//   - heap user-type temps would be freed at stmt end, then dropped
		//     again when the tuple is consumed (case A double-free).
		c.claimStringTemp(elemVal) // strings, vectors, channels, arcs, mutexes
		c.claimHeapTemp(elemVal)   // heap user-type instances
		c.claimEnvTemp(elemVal)    // T0741: closure env (tuple owns it now)
		// Clear enum ctor temps created during this element's evaluation so
		// the tuple is the unique owner of the enum's variant data.
		// T1139: gate on the element's static type being an enum — a non-enum
		// element that merely BORROWS an inline Enum.V(x) temp in a sub-call
		// leaves an intermediate the tuple never owns; it must stay tracked so
		// the caller drops it at statement end, else it leaks.
		elemEnumType := c.info.Types[elem]
		if c.typeSubst != nil {
			elemEnumType = types.Substitute(elemEnumType, c.typeSubst)
		}
		if extractEnum(elemEnumType) != nil {
			for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
				c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
			}
			c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
		}
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

// wrapArmValueOptional coerces a match/if arm value to the shared Optional
// result shape (T1189). When the arms unify to Optional[T] but this arm produced
// the bare inner `T` value (e.g. a value arm sibling of a `none` arm), wrap it as
// `{ i1 true, T }` so every arm — and the merge phi — shares the `{ i1, T }`
// type. A `none` arm (already the optional zero via genNoneLit's targetType) and
// an already-optional arm produce the struct shape and pass through unchanged.
func (c *Compiler) wrapArmValueOptional(val value.Value, resultType types.Type) value.Value {
	if val == nil || resultType == nil {
		return val
	}
	rt := resultType
	if c.typeSubst != nil {
		rt = types.Substitute(rt, c.typeSubst)
	}
	opt, ok := rt.(*types.Optional)
	if !ok {
		return val
	}
	optLL, ok := c.resolveType(rt).(*irtypes.StructType) // void-optional resolves to i1 → skip
	if !ok {
		return val
	}
	if val.Type().Equal(c.resolveType(opt.Elem())) { // produced the bare inner T
		return c.wrapOptional(val, optLL)
	}
	return val // already {i1,T} (none arm or optional-typed arm)
}

// coerceNoneToOptional coerces a none-typed value (the i1 void-optional bound
// from a bare `none` / all-`none` match-if) to the concrete Optional[T] zero
// expected at a consumption site. A bare `none` LITERAL already produced the
// target struct via targetType, so a val already matching passes through. T1190.
func (c *Compiler) coerceNoneToOptional(val value.Value, exprType, targetType types.Type) value.Value {
	if val == nil || targetType == nil {
		return val
	}
	if c.typeSubst != nil && exprType != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	if exprType != types.TypNone {
		return val
	}
	if st, ok := c.resolveType(targetType).(*irtypes.StructType); ok && !val.Type().Equal(st) {
		return c.zeroValue(st)
	}
	return val
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
	// NoneLit already produces the correct zero value via targetType; a
	// none-typed variable-read is coerced to the concrete Optional zero. T1190.
	if exprType == types.TypNone {
		return c.coerceNoneToOptional(val, exprType, retType)
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
	// T0954: capture+reset the consumed-by-await signal so it does not leak into
	// operand subexpressions (a nested elvis in e.Left/e.Right is not itself the
	// await operand). Used below to neutralize the none-path default's owner.
	consumedByReceive := c.elvisResultConsumed
	c.elvisResultConsumed = false
	// T0952: capture+reset the bound-result signal (set by the var-decl/assignment
	// RHS-eval sites in stmt.go). Same reasoning — a nested elvis in e.Left/e.Right
	// is not itself the bound RHS, so it must not inherit this flag.
	boundResult := c.elvisResultBound
	c.elvisResultBound = false
	// T0982: capture+reset the returned-result signal (set by genReturnStmt's
	// RHS-eval site). A returned elvis escapes to the caller, so — like a bound
	// result — a handle/heap none-path owned-local default must be neutralized here
	// (else the function's scope-exit drop AND the caller both free it → double
	// free/SEGV). Unlike boundResult, it does NOT create a per-path elvisBoundDropFlag.
	returnedResult := c.elvisResultReturned
	c.elvisResultReturned = false
	// T1166: capture+reset the force-own-clone signal (set by the member/index
	// assignment-target RHS-eval site in stmt.go). Reset so a nested elvis in
	// e.Left/e.Right does not inherit it, exactly like elvisResultBound.
	ownsForced := c.elvisResultOwnsForced
	c.elvisResultOwnsForced = false
	// T0940/T0981: defensively clear any stale per-path bound flag before this elvis
	// computes its own. Consumed by the var-decl binding in stmt.go.
	c.elvisBoundDropFlag = nil

	// T1166: for a member/index owned target, precompute the cloneable-droppable gate
	// and resolved result type once. Force-clone only the representations
	// cloneResolvedValue handles safely — Vector/string (elvisResultDrop) and
	// Map/Set/heap-user (elvisResultHeapDrop); single-owner native handles
	// (elvisResultHandleDrop) are not cloneable and sema rejects those operand shapes.
	_, _, vecOrStr := c.elvisResultDrop(e)
	forceOwnClone := ownsForced && (vecOrStr || c.elvisResultHeapDrop(e) != nil)
	resolvedElvisType := c.info.Types[e]
	if c.typeSubst != nil && resolvedElvisType != nil {
		resolvedElvisType = types.Substitute(resolvedElvisType, c.typeSubst)
	}

	// T0940: in a BOUND droppable context the var-decl set dup-on-read flags
	// (T0095/B0219/T0366/…) so genFieldAccess/genVectorIndex CLONES a member/index
	// source's inner into a fresh buffer the binding owns. Snapshot that now — before
	// e.Left's eval consumes (and clears) the flag — so the some-path bound flag marks
	// ownership of the clone. elvisSomeInnerOrphaned reports a member/index source as
	// container-owned (correct for the INLINE case, which does NOT dup); this promotes
	// it to owned only when a clone actually happens (the dup flag is live AND the
	// source is a field/index read).
	boundSourceDupCloned := false
	if boundResult {
		switch unwrapDestructureParens(e.Left).(type) {
		case *ast.MemberExpr, *ast.IndexExpr:
			boundSourceDupCloned = c.dupStringFieldAccess || c.dupContainerFieldAccess ||
				c.dupHeapUserFieldAccess || c.dupTupleFieldAccess
		}
	}

	optVal := c.genExprAutoPropagate(e.Left)

	// Extract the present flag (field 0)
	flag := c.block.NewExtractValue(optVal, 0)

	someBlock := c.newBlock("elvis.some")
	noneBlock := c.newBlock("elvis.none")
	mergeBlock := c.newBlock("elvis.merge")

	c.block.NewCondBr(flag, someBlock, noneBlock)

	// Some path: extract inner value
	c.block = someBlock
	var someVal value.Value = c.block.NewExtractValue(optVal, 1) // T1166: widened for the force-own-clone reassignment below
	// B0194/T0111: Clear drop flag on elvis of an *owned* optional identifier.
	// The inner value is extracted and transferred to the result — the optional's
	// scope-exit drop should NOT also free it (double-free). Peel ParenExpr so
	// `(a) ?: b` clears `a`'s flag too (T0937), matching the orphan classifier
	// (which also peels parens).
	//
	// someOwnsInner records whether the result OWNS the moved-out inner on the
	// some-path. It owns it only when the inner is "orphaned" — an owned local
	// (whose scope drop flag is cleared here) or a temporary optional. A borrowed
	// value parameter (T0945; caller-owned) and a member/index source (T0937;
	// container-owned) leave the inner with an existing owner, so the result
	// borrows it (someOwnsInner=false) and the inline result temp is never freed.
	someOwnsInner := c.elvisSomeInnerOrphaned(e.Left)
	if boundSourceDupCloned {
		// The bound binding cloned the member/index field — the binding owns the fresh
		// copy on the some-path (the container keeps the original; no flag to clear).
		someOwnsInner = true
	}
	if someOwnsInner {
		if ident, ok := unwrapDestructureParens(e.Left).(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		}
	}
	// T1166: member/index owned target — a borrowed some-path inner (someOwnsInner
	// false: borrowed param or not-yet-cloned container source) must be deep-cloned so
	// the field/element owns an independent copy. The container's unconditional
	// field/element drop is then balanced; the caller/container keeps the original.
	// Marks the result owned so trackElvisResultHeap/Temp register an owned temp that
	// the member/index assign branch claims (identical to the working owned-local case).
	if forceOwnClone && !someOwnsInner {
		someVal = c.cloneResolvedValue(someVal, resolvedElvisType)
		someOwnsInner = true
	}
	c.block.NewBr(mergeBlock)
	someEnd := c.block

	// None path: evaluate default
	c.block = noneBlock
	// T0983: when a BOUND droppable elvis's default is itself an elvis
	// (`m := a ?: (b ?: c)`), propagate the bound obligation into the inner elvis so
	// IT neutralizes its own terminal default's owner (clear a local's drop flag /
	// claim a fresh temp) and produces its own per-path bound drop flag. The outer
	// then (a) claims the inner elvis's inline result temp and (b) inherits the inner's
	// per-path flag as its none-path ownership — instead of value-identity claiming the
	// inner phi, which neutralized nothing (the inner default kept its scope-exit owner
	// while the bound variable also took an owning drop → double free / SEGV, T0983).
	// Recurses naturally for deeper nesting (`a ?: (b ?: (c ?: d))`).
	nestedBoundDefault := false
	if boundResult || returnedResult || consumedByReceive {
		_, _, ownedRes := c.elvisResultDrop(e)
		droppableRes := ownedRes || c.elvisResultHandleDrop(e) != nil || c.elvisResultHeapDrop(e) != nil
		if droppableRes {
			if be, ok := unwrapDestructureParens(e.Right).(*ast.BinaryExpr); ok && be.Op == ast.BinElvis {
				if boundResult {
					c.elvisResultBound = true
					nestedBoundDefault = true
				} else if returnedResult {
					// T0982: nested-elvis default in a RETURN (`return a ?: (b ?: c)`).
					// Propagate the returned obligation into the inner elvis so IT
					// neutralizes its own terminal owned-local default's scope-exit drop
					// (the all-none path returns that default, which escapes to the caller;
					// without this both the inner default's binding and the caller free it →
					// SEGV/double-free). Unlike the bound case this threads NO per-path flag
					// up — the escaping result temp is claimed unconditionally in
					// genReturnStmt, so the inner's flag-clear is the whole fix. Recurses
					// naturally for deeper nesting (`a ?: (b ?: (c ?: d))`).
					c.elvisResultReturned = true
				} else {
					// T0955: nested-elvis default consumed by an enclosing `<-` await
					// (`<-(a ?: (c ?: b))`). Propagate the consume signal into the inner
					// elvis so IT neutralizes its own terminal owned-local/fresh-temp
					// default (the await joins+frees the selected G; without this the
					// inner default's binding frees it again → double-free/SEGV/hang).
					// Like the returned case, threads NO per-path flag up — the await is
					// the single owner. Recurses for deeper nesting
					// (`<-(a ?: (b ?: (c ?: d)))`).
					c.elvisResultConsumed = true
				}
			}
		}
	}
	defaultVal := c.genExprAutoPropagate(e.Right)
	// T0936: the none-path SELECTS the default. The result OWNS it (and must free it
	// exactly once) only when we can neutralize the default's own owner here:
	//   - local-ident default → clear its scope-exit drop flag (path-conditional:
	//     emitted in the none block only, so on the some-path the *unselected*
	//     default is still dropped normally by its own binding);
	//   - fresh temp default (literal/call) → claim its string/heap temp.
	// A parameter/borrowed/static default keeps its existing owner (caller binding,
	// or none for .rodata) so the result BORROWS it (noneOwned=false) — matching the
	// ownership pass's borrow model for those operands and avoiding a double-free.
	noneOwned := false
	// T0983: nested-elvis default — the inner elvis already neutralized its terminal
	// default and set c.elvisBoundDropFlag. Capture that per-path flag (the outer's
	// none-path ownership), reset it so the outer's own bound-flag phi below is not
	// confused, and claim the inner elvis's inline result temp so it is not also freed
	// at statement end. noneOwned=true records that the outer binding may own on the
	// none-path (the exact per-path condition is the inherited flag, threaded below).
	var nestedNoneFlag value.Value
	if nestedBoundDefault {
		nestedNoneFlag = c.elvisBoundDropFlag
		c.elvisBoundDropFlag = nil
		c.claimElvisDefaultTemp(defaultVal)
		if nestedNoneFlag != nil {
			noneOwned = true
		}
	}
	if nestedNoneFlag != nil {
		// Handled above — skip the flat neutralization paths below.
	} else if _, _, owned := c.elvisResultDrop(e); owned {
		// Vector[T]/T[] and string results (inline + bound), unchanged semantics.
		noneOwned = c.neutralizeElvisNoneDefault(e, defaultVal)
	} else if consumedByReceive {
		// T0954: result consumed by an enclosing `<-` await (Task[T] handle, not a
		// Vector/string result elvisResultDrop tracks). The await joins+frees the
		// selected G, so the none-path default must not be freed again by its own
		// owner — neutralize an owned-local / fresh-temp default. noneOwned stays
		// false (the await is the single owner); a borrowed param default has no
		// drop flag to clear, leaving T0953's borrowed-source crash to its own fix.
		c.neutralizeElvisNoneDefault(e, defaultVal)
	} else if (boundResult || returnedResult) && (c.elvisResultHandleDrop(e) != nil || c.elvisResultHeapDrop(e) != nil) {
		// T0952 (single-owner native handle) + T0940 (Map/Set/heap-user type) elvis
		// bound DIRECTLY to a variable (`m := a ?: b`). The binding takes a per-path
		// owning drop (elvisBoundDropFlag); on the none-path it aliases the default,
		// so an owned-local / fresh-temp default's own scope-exit drop must be
		// neutralized here (path-conditional — none-block only) or the buffer is
		// freed twice (Mutex/Map/heap-user: SEGV/invalid-free). A borrowed param /
		// member / static default keeps its real owner → noneOwned stays false and
		// the bound variable BORROWS it on the none-path. The inline (non-bound) case
		// keeps borrow-on-none (T0951/T0937), so this is gated to boundResult.
		noneOwned = c.neutralizeElvisNoneDefault(e, defaultVal)
	}
	// T1166: member/index owned target — a borrowed none-path default (noneOwned false:
	// borrowed param / member / static, whose owner was NOT neutralized above) must be
	// deep-cloned so the field/element owns its copy; the default's real owner keeps the
	// original. Symmetric with the some-path clone.
	if forceOwnClone && !noneOwned {
		defaultVal = c.cloneResolvedValue(defaultVal, resolvedElvisType)
		noneOwned = true
	}
	noneEnd := c.block
	c.block.NewBr(mergeBlock)

	// Merge
	c.block = mergeBlock
	result := mergeBlock.NewPhi(
		&ir.Incoming{X: someVal, Pred: someEnd},
		&ir.Incoming{X: defaultVal, Pred: noneEnd},
	)
	// T0933/T0940/T0981/T0952/T0936: a BOUND elvis (`m := a ?: b`) replaces the variable
	// binding's unconditional owning drop with this per-path flag — `m` owns the
	// buffer only on a path whose selected operand was orphaned (some-path inner) or
	// neutralized (none-path default). Computed for every droppable result
	// representation (Vector/string via elvisResultDrop, native handles via
	// elvisResultHandleDrop, Map/Set/heap-user via elvisResultHeapDrop). Created here
	// — after the result phi, before the trackElvis* phis — so all phis precede the
	// merge block's non-phi instructions. Consumed in the var-decl path in stmt.go.
	if boundResult {
		_, _, vecOrStr := c.elvisResultDrop(e)
		if vecOrStr || c.elvisResultHandleDrop(e) != nil || c.elvisResultHeapDrop(e) != nil {
			someF, noneF := int64(0), int64(0)
			if someOwnsInner {
				someF = 1
			}
			if noneOwned {
				noneF = 1
			}
			// T0983: a nested-elvis default contributes a per-path (runtime) flag, not a
			// constant — the outer binding owns the selected value on the none-path exactly
			// when the inner elvis's own bound flag says so.
			var noneIncoming value.Value = constant.NewInt(irtypes.I1, noneF)
			if nestedNoneFlag != nil {
				noneIncoming = nestedNoneFlag
			}
			c.elvisBoundDropFlag = mergeBlock.NewPhi(
				&ir.Incoming{X: constant.NewInt(irtypes.I1, someF), Pred: someEnd},
				&ir.Incoming{X: noneIncoming, Pred: noneEnd},
			)
		}
	}
	// T0935/T0945/T0936/T0937: register the inline result with a path-dependent drop
	// flag. The some-path owns the moved-out inner only when it was actually orphaned
	// (someOwnsInner — false for a borrowed value parameter (T0945) or a member/index
	// source (T0937)); the none-path owns only when the default's owner was
	// neutralized above (noneOwned, T0936). A bound use claims this temp (by value
	// identity) and drops via the variable's own binding instead — leaving these
	// path-flags inert. trackElvisResultTemp handles the i8* container representation
	// (string, Vector[T]/T[]); trackElvisResultHeap handles the 2-word value-struct
	// representation (Map/Set and droppable heap user types, T0937).
	c.trackElvisResultTemp(e, result, someEnd, noneEnd, mergeBlock, someOwnsInner, noneOwned)
	c.trackElvisResultHeap(e, result, someEnd, noneEnd, mergeBlock, someOwnsInner, noneOwned)
	return result
}

// elvisSomeInnerOrphaned reports whether the some-path inner extracted by `?:`
// from `left` is left without an owner (so the elvis result must own it). True for
// an owned droppable LOCAL ident (its scope drop flag is cleared on the some-path)
// and for a temporary optional (call/expr result — never scope-tracked). False for
// member/index sources (the container's drop frees the inner) and for borrowed-param
// idents (the caller owns it; no local drop flag — T0931/T0945).
func (c *Compiler) elvisSomeInnerOrphaned(left ast.Expr) bool {
	switch l := unwrapDestructureParens(left).(type) {
	case *ast.IdentExpr:
		_, hasFlag := c.dropFlags[l.Name]
		return hasFlag
	case *ast.MemberExpr, *ast.IndexExpr:
		return false
	default:
		return true // temporary optional
	}
}

// claimElvisDefaultTemp neutralizes a fresh string/heap temp selected as an elvis
// none-path default (literal/call result) and reports whether ownership was
// actually transferred to the elvis result. Returns false when there was no owned
// temp to claim (e.g. a .rodata literal or a borrowed member read), so the caller
// leaves the result borrowing — exactly one owner frees the buffer either way.
func (c *Compiler) claimElvisDefaultTemp(val value.Value) bool {
	if val == nil {
		return false
	}
	claimed := false
	if idx, ok := c.stmtTempMap[val]; ok && idx >= 0 {
		claimed = true
	}
	c.claimStringTemp(val)
	c.claimHeapTemp(val) // sets c.lastClaimedDropFunc when it claims a heap temp
	if c.lastClaimedDropFunc != nil {
		claimed = true
	}
	return claimed
}

// neutralizeElvisNoneDefault transfers a none-path default's ownership to the elvis
// result and reports whether it succeeded (T0936/T0940/T0981). An owned-local ident
// default → clear its scope-exit drop flag (path-conditional: emitted in the none
// block only, so the some-path's unselected default is still dropped by its own
// binding). A fresh string/heap temp default (literal/call) → claim it. A borrowed
// param / member / static default (no scope drop flag, no owned temp) → returns
// false: the result must BORROW on the none-path so the default's real owner stays
// the sole owner. Peels parens so `a ?: (b)` matches the owned-local `b` (symmetric
// with the some-path's e.Left peel). Single source of truth for none-path transfer.
func (c *Compiler) neutralizeElvisNoneDefault(e *ast.BinaryExpr, defaultVal value.Value) bool {
	if ident, ok := unwrapDestructureParens(e.Right).(*ast.IdentExpr); ok {
		if _, has := c.dropFlags[ident.Name]; has {
			c.clearDropFlag(ident.Name)
			return true
		}
		return false
	}
	return c.claimElvisDefaultTemp(defaultVal)
}

// elvisResultHeapDrop resolves the drop function for an elvis result represented as
// a 2-word {i8*, i8*} value struct (Map/Set) or a droppable heap user type — the
// representation trackElvisResultHeap handles (T0937). Returns nil for value/copy/
// primitive/structural results, for i8* containers / native handles / string (those
// go through elvisResultDrop / elvisResultHandleDrop / trackElvisResultTemp), and
// for ref types. Single source of truth for the heap-droppable classification shared
// by genElvis's bound-flag gating (T0940) and trackElvisResultHeap.
func (c *Compiler) elvisResultHeapDrop(e *ast.BinaryExpr) *ir.Func {
	rt := c.info.Types[e]
	if c.typeSubst != nil && rt != nil {
		rt = types.Substitute(rt, c.typeSubst)
	}
	if c.selfSubst != nil && rt != nil {
		rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
	}
	named := extractNamed(rt)
	if named == nil {
		return nil
	}
	if named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) || named.IsStructural() {
		return nil
	}
	if isContainerType(rt) || named == types.TypString {
		return nil // keeps Map/Set; excludes handles/string (i8* containers go through trackElvisResultTemp)
	}
	return c.resolveDropFuncForTemp(named, rt)
}

// trackElvisResultHeap registers an inline elvis result that is a value-struct
// container (Map/Set) or heap user type {i8*, i8*} as an owned heap drop temp with
// a per-branch flag (someOwned on the some path where the inner is orphaned,
// noneOwned on the none path). T0937 (subsumes T0924's heap-user case — same
// representation + mechanism). Type filtering is delegated to elvisResultHeapDrop, so
// single-owner handles (Arc/Mutex/Task/...), strings, vectors, value/copy/primitive/
// structural results are excluded; only Map/Set and droppable heap user types pass.
// noneOwned can now be true for a BOUND Map/Set/heap-user default whose owner was
// neutralized (T0940) — harmless here because a bound use claims this temp by runtime
// pointer identity (claimHeapTemp), neutralizing the flag, and the variable's own
// per-path binding flag governs the drop instead.
func (c *Compiler) trackElvisResultHeap(e *ast.BinaryExpr, result value.Value, someEnd, noneEnd, mergeBlock *ir.Block, someOwned, noneOwned bool) {
	if !someOwned && !noneOwned {
		return // borrows on both paths (owner-governed source + borrowed default)
	}
	if !c.tempTrackingEnabled || result == nil {
		return
	}
	if c.entryBlock == nil || c.block == nil || c.block.Term != nil {
		return
	}
	dropFunc := c.elvisResultHeapDrop(e)
	if dropFunc == nil {
		return
	}

	// Per-branch live flag: owned on the some path when the extracted inner is
	// orphaned, owned on the none path when the default's owner was neutralized
	// (T0940 — reached for a bound Map/Set/heap-user default; a bound use claims this
	// temp by pointer identity so the flag is then inert). Created in the merge block
	// immediately after the result phi (phis-first).
	someFlag := int64(0)
	if someOwned {
		someFlag = 1
	}
	noneFlag := int64(0)
	if noneOwned {
		noneFlag = 1
	}
	owned := mergeBlock.NewPhi(
		&ir.Incoming{X: constant.NewInt(irtypes.I1, someFlag), Pred: someEnd},
		&ir.Incoming{X: constant.NewInt(irtypes.I1, noneFlag), Pred: noneEnd},
	)
	instPtr := c.block.NewExtractValue(result, 1)
	c.trackHeapTempWithFlag(instPtr, dropFunc, owned)
}

// elvisResultDrop resolves the elvis result type and returns the matching temp
// drop function (Vector.drop / promise_string_drop), the vector element type (or
// nil), and whether the result is an owned Vector/string the elvis owns on both
// paths. Returns ok=false for ref types and all other representations (T0940).
// Single source of truth for the gating in genElvis and the dispatch in
// trackElvisResultTemp.
func (c *Compiler) elvisResultDrop(e *ast.BinaryExpr) (*ir.Func, types.Type, bool) {
	rt := c.info.Types[e]
	if c.typeSubst != nil && rt != nil {
		rt = types.Substitute(rt, c.typeSubst)
	}
	if c.selfSubst != nil && rt != nil {
		rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
	}
	if rt == nil || isRefType(rt) {
		return nil, nil, false
	}
	if elem, ok := types.AsVector(rt); ok {
		dropFn := c.funcs["Vector.drop"]
		return dropFn, elem, dropFn != nil
	}
	if extractNamed(rt) == types.TypString {
		dropFn := c.funcs["promise_string_drop"]
		return dropFn, nil, dropFn != nil
	}
	return nil, nil, false
}

// elvisResultHandleDrop resolves the per-instantiation drop function for an elvis
// result that is a single-owner native handle represented as a bare i8* — Ref[T],
// Channel[T], Weak[T], Mutex[T], MutexGuard[T], Task[T] (T0951). These bypass
// elvisResultDrop (which only resolves Vector/string) and trackElvisResultHeap
// (which requires a 2-word {i8*,i8*} value struct), so the orphaned some-path
// handle had no drop path → leak. Mirrors the handle dispatch in trackGetterResult.
// rt is substituted first, so the element types from types.As* are concrete.
// Returns nil for every non-handle / ref result type.
func (c *Compiler) elvisResultHandleDrop(e *ast.BinaryExpr) *ir.Func {
	rt := c.info.Types[e]
	if c.typeSubst != nil && rt != nil {
		rt = types.Substitute(rt, c.typeSubst)
	}
	if c.selfSubst != nil && rt != nil {
		rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
	}
	if rt == nil || isRefType(rt) {
		return nil
	}
	named := extractNamed(rt)
	if chElem, ok := types.AsChannel(rt); ok || named == types.TypChannel {
		return c.getOrCreateChannelDrop(chElem)
	}
	if arcElem, ok := types.AsArc(rt); ok {
		return c.getOrCreateArcDrop(arcElem)
	}
	if weakElem, ok := types.AsWeak(rt); ok {
		return c.getOrCreateWeakDrop(weakElem)
	}
	if mutexElem, ok := types.AsMutex(rt); ok {
		return c.getOrCreateMutexDrop(mutexElem)
	}
	if _, ok := types.AsMutexGuard(rt); ok || named == types.TypMutexGuard {
		return c.funcs["MutexGuard.drop"]
	}
	if taskElem, ok := types.AsTask(rt); ok {
		return c.getOrCreateTaskDrop(taskElem)
	}
	return nil
}

// trackElvisResultTemp registers an inline (used/discarded) elvis `?:` result as
// a statement temp with a path-dependent drop flag (T0935/T0945/T0936/T0937). The
// flag is true on a path only when the result actually owns the buffer selected on
// that path — someOwned on the some-path (the optional's inner was orphaned, false
// for a borrowed value parameter (T0945) or a member/index source (T0937)) and
// noneOwned on the none-path (the default's owner was neutralized, T0936). When a
// path's operand keeps an existing owner (caller param, container field, or
// .rodata) the flag is false there and the result borrows — avoiding a double-free.
// The drop function matches the result type — Vector.drop for vectors (honors the
// bit-63 static flag and walks droppable elements), promise_string_drop for
// strings. Other i8* result types fall through untracked (T0940). A bound use of
// the result claims the temp by value identity, neutralizing the flag so only the
// variable's binding drops it. The value-struct representation (Map/Set, heap user
// types) is handled by trackElvisResultHeap instead.
func (c *Compiler) trackElvisResultTemp(e *ast.BinaryExpr, result value.Value, someEnd, noneEnd, mergeBlock *ir.Block, someOwned, noneOwned bool) {
	// When neither path transfers ownership to the result (e.g. a borrowed value
	// parameter or a member/index source on the some-path with a borrowed/static
	// default on the none-path), the result borrows on both paths and must not be
	// dropped (T0945/T0936/T0937).
	if !someOwned && !noneOwned {
		return
	}
	if !c.tempTrackingEnabled || result == nil || result.Type() != irtypes.I8Ptr {
		return
	}
	if c.entryBlock == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if _, ok := c.stmtTempMap[result]; ok {
		return
	}

	dropFn, elemType, owned := c.elvisResultDrop(e)
	if !owned {
		// T0951: a single-owner native handle (Arc/Channel/Weak/Mutex/MutexGuard/
		// Task) is a bare i8*, not a Vector/string, so elvisResultDrop returns
		// owned=false. Resolve the handle's native drop here so the orphaned
		// some-path handle is freed exactly once. cleanupStmtTemps routes a Task
		// drop through the cooperative join automatically.
		if handleDrop := c.elvisResultHandleDrop(e); handleDrop != nil {
			dropFn, elemType = handleDrop, nil
		} else {
			return // other result types untracked (T0940)
		}
	}

	// Path-dependent drop flag: per-path ownership computed in genElvis. Created in
	// the merge block immediately after the result phi so all phis precede the stores.
	someFlag := int64(0)
	if someOwned {
		someFlag = 1
	}
	noneFlag := int64(0)
	if noneOwned {
		noneFlag = 1
	}
	flagPhi := mergeBlock.NewPhi(
		&ir.Incoming{X: constant.NewInt(irtypes.I1, someFlag), Pred: someEnd},
		&ir.Incoming{X: constant.NewInt(irtypes.I1, noneFlag), Pred: noneEnd},
	)
	c.appendStmtTemp(result, dropFn, elemType, flagPhi)
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
		// T1215: a Vector index element (`[payload[i]]`) read into this literal
		// aliases the source vector's element buffer — dup-on-read so the new
		// vector owns an independent copy, else both vectors' element drops free
		// the same allocation ("fatal: invalid free (bad header magic)"). No-op
		// for non-index elements (armDupForVectorIndexArg only arms for `v[i]`).
		c.armDupForVectorIndexArg(elemExpr)
		val := c.genCallArgExpr(elemExpr)
		c.dupStringFieldAccess = false
		c.dupContainerFieldAccess = false
		c.dupHeapUserFieldAccess = false
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
					c.vecElemNeedsOptionalDrop(elem) ||
					isSignatureElem(elem) { // T1237: closure element moved into vector
					c.clearDropFlag(ident.Name)
				}
			}
			// T0784: same gating for `x as!/as T` element — Vector.drop walks the
			// element-type and would free the slot, so the cast subject's
			// scope-exit drop binding must be suppressed to avoid a double-free.
			if !isRefType(elem) {
				if extractNamed(elem) == types.TypString ||
					c.vecElemNeedsEnumDrop(elem) ||
					c.vecElemNeedsUserTypeDrop(elem) ||
					c.tupleNeedsDrop(elem) ||
					c.vecElemNeedsOptionalDrop(elem) {
					if ident := c.castSubjectMovableIdent(elemExpr); ident != nil {
						// T0849: conditional `as` form drops iff the cast failed.
						c.consumeCastSubjectDropFlag(elemExpr, ident.Name)
					}
				}
			}
			// T1073: `[o!]` — force-unwrap moves the inner out of the source
			// optional into the vector slot (which Vector.drop frees on the
			// walkEnabled path, the enclosing branch). Neutralize the source
			// optional's present flag so its scope-exit drop doesn't double-free
			// the moved inner. Only correct under walkEnabled: when false,
			// Vector.drop does NOT free elements, so the source must keep ownership.
			c.neutralizeForceUnwrapElem(elemExpr)
			// B0233: Claim heap temp — element ownership transferred to vector literal.
			c.claimHeapTemp(val)
			// T0366: Also claim string/vector/channel stmt-temps. trackVectorTempWithElemType
			// (called by CallExpr / ?! / ?^ / ! / ? e {} for Vector results) registers in
			// stmtTemps, not heapTemps — claimHeapTemp doesn't see them. Without claiming,
			// the caller's stmt-temp cleanup runs Vector.drop while the gather buffer (owned
			// by the variadic callee) also drops each element → double-free.
			c.claimStringTemp(val)
			// T0741: claim closure env — element ownership transferred to vector;
			// the vector's element-drop loop now frees each closure's env.
			c.claimEnvTemp(val)
			// B0281: Clear enum ctor temps created during this element's evaluation.
			// Same issue as map literals: the enum value is stored by LLVM value,
			// so both the temp alloca and the vector slot share inner pointers.
			// Only clear temps added since savedEnumTemps to avoid clobbering
			// temps from outer expressions.
			// T1139: gate on the element's static type being an enum — a non-enum
			// element that merely BORROWS an inline Enum.V(x) temp in a sub-call
			// leaves an intermediate the vector never owns; it must stay tracked
			// so the caller drops it at statement end, else it leaks.
			elemEnumType := c.info.Types[elemExpr]
			if c.typeSubst != nil {
				elemEnumType = types.Substitute(elemEnumType, c.typeSubst)
			}
			if extractEnum(elemEnumType) != nil {
				for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
					c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
				}
				c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
			}
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
		c.claimEnvTemp(val)    // T0741: closure env (array owns it now)
		// Clear enum ctor temps created during this element's evaluation so
		// the array is the unique owner of the enum's variant data.
		// T1139: gate on the element's static type being an enum — a non-enum
		// element that merely BORROWS an inline Enum.V(x) temp in a sub-call
		// leaves an intermediate the array never owns; it must stay tracked so
		// the caller drops it at statement end, else it leaks.
		elemEnumType := c.info.Types[elemExpr]
		if c.typeSubst != nil {
			elemEnumType = types.Substitute(elemEnumType, c.typeSubst)
		}
		if extractEnum(elemEnumType) != nil {
			for j := savedEnumTemps; j < len(c.enumCtorTemps); j++ {
				c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[j].dropFlag)
			}
			c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
		}
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
	switch {
	case isThisReceiver(e.Target):
		// T0745: `this` (incl. paren-wrapped) is already the i8* receiver ptr.
		instancePtr = target
	case isContainerType(targetType):
		instancePtr = target
	case named != nil && named.IsValueType():
		instancePtr = c.valueTypeReceiverPtr(target, targetType)
	default:
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
	c.emitIndexBoundsCheck(idx, constant.NewInt(irtypes.I64, arr.Size()),
		"arridx", "array index out of bounds")
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

	// T1129: Droppable-enum element (extractNamed is nil for enums, so the heap-user
	// branches above skip them). Mirrors the genVectorIndex enum branch — without
	// this, `got := arr[i]` aliases the array slot and got's drop + the array's
	// element walk double-free the variant data (fatal for recursive enums).
	// cloneResolvedValue deep-clones via the synthesized/explicit/shallow path.
	if c.dupHeapUserFieldAccess && c.tempTrackingEnabled &&
		c.enumElemNeedsDupOnRead(elemType) {
		c.dupHeapUserFieldAccess = false // consume the flag
		return c.cloneResolvedValue(val, elemType)
	}

	// T1130: Map/Set element read-back from a fixed-size array. Map/Set are excluded
	// from isDroppableHeapUserType (T0440), so the heap-user branch above skips them —
	// but arrays have no internal match-dup, so `got := arr[i]` aliases the array slot.
	// Deep-clone via the element's clone() so the binding owns an independent Map/Set.
	if c.dupHeapUserFieldAccess && c.tempTrackingEnabled && isMapOrSetType(elemType) {
		if named := extractNamed(elemType); named != nil {
			c.dupHeapUserFieldAccess = false // consume the flag
			return c.cloneHeapElement(val, elemType, named)
		}
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
		// T1266: a fixed-array element that is itself a value-copying container transitively
		// nesting a closure must NOT be duped — dupVector's element-clone loop zeroes each
		// closure's opaque env (T0813) → null {fn,env} → SEGV on invoke. Leave it ALIASED
		// (a borrow of the array's owned storage, env intact); the borrow gates
		// (isClosureAggregateBorrow / closureAggregateBorrowSource) suppress the owning drop
		// binding and reject escapes, keeping this in lockstep. Mirrors the genVectorIndex
		// T1263 guard; FirstFieldNestedClosureDeep keeps Ref/Weak/… opaque, so Ref[…][] /
		// int[] elements keep deep-copying.
		resolvedContainerElem := elemType
		if c.typeSubst != nil {
			resolvedContainerElem = types.Substitute(elemType, c.typeSubst)
		}
		if sema.FirstFieldNestedClosureDeep(resolvedContainerElem) != nil {
			c.dupContainerFieldAccess = false // consume the flag
			return val
		}
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
			resolvedArcElem := arcElem
			if c.typeSubst != nil {
				resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
			}
			dup := c.dupArc(val, resolvedArcElem)
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
				// T0939: dup is null on the optional's `none` path — guard the clone loop.
				c.emitVectorElementCloneLoopNullable(dup, innerElem)
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
				resolvedArcElem := arcElem
				if c.typeSubst != nil {
					resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
				}
				dup := c.dupArc(innerArc, resolvedArcElem)
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
	switch {
	case isThisReceiver(e.Target):
		// T0745: `this` (incl. paren-wrapped) is already the i8* receiver ptr the
		// operator method expects — must precede the value-type branch, which
		// would otherwise panic trying to take a value-struct ptr of a raw i8*.
		instancePtr = target
	case isContainerType(targetType):
		instancePtr = target
	case named != nil && named.IsValueType():
		instancePtr = c.valueTypeReceiverPtr(target, targetType)
	default:
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
	// Gated to V types that `Map[K, V].[]` returns by ALIAS — i.e. NOT
	// match-dup'able (`!typeNeedsMatchDup`). `Map[K, V].[]` is a synthesized method
	// whose body uses a match destructure (`Slot.Used(k, v) => return v;`) that
	// already dups V internally when V is safely-dup'able (typeNeedsMatchDup →
	// heapTypeSafeToDup walks fields) or when V has a `clone()` method. The V shapes
	// where Map.[]'s body leaves V aliased are exactly `!typeNeedsMatchDup`: an
	// *explicit* `drop()` with no clone (T0484), OR a synth-drop type whose fields
	// aren't shallow-dup-safe (e.g. a droppable-element vector field — T1146).
	// typeNeedsMatchDup already returns true for clone-bearing V (so this is skipped
	// for them — their body dup makes the result owned) and for shallow-dup-safe V,
	// so `!typeNeedsMatchDup` fires precisely on the aliased shapes. Firing outside
	// that shape would produce a redundant second copy whose pointer is lost (one
	// alloc leaks per read), which is why the predicate must be exact. The drop
	// origin (explicit vs synthesized) is irrelevant — what matters is whether the
	// body aliased. Uses dupHeapValue (memcpy + sub-field dup) directly, which is
	// null-safe internally — important because `result`'s value field is zero/null
	// when the Optional is None.
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
					named.LookupMethod("clone") == nil &&
					!c.typeNeedsMatchDup(elem) {
					c.dupHeapUserFieldAccess = false // consume the flag
					innerVal := c.block.NewExtractValue(result, 1)
					dup := c.dupHeapValue(innerVal, elem)
					c.optionalHeapDup = dup
					return c.block.NewInsertValue(result, dup, 1)
				}
			}
		}
	}

	// T1117: Dup a borrowed droppable enum element when `[]` returns
	// `Optional[enum]` whose variant data aliases the container's stored slot.
	// `Map[K,V].[]`'s match-destructure body returns V by alias when V is an enum
	// that isn't safely match-dup'able or clone-bearing (typeNeedsMatchDup ==
	// false) — e.g. an enum whose variant carries an Arc/Ref. An owning bind
	// (`h := m[k]!`, or assignment) sets dupHeapUserFieldAccess at the bind site;
	// without a dup here, the binding's drop walks the variant's Arc/Ref and
	// decrements the shared refcount, corrupting the slot the Map still owns (UAF
	// on the next read). Deep-dup the variant fields via an alloca round-trip
	// through dupEnumElementInPlace (dupArc/dupWeak increment the refcount), then
	// re-insert into the Optional so the bound copy owns an independent count.
	// The inline/borrow form (`match m[k]!`) never sets the flag, so it stays
	// aliased — balanced because it takes no owning drop. Mirrors
	// maybeDupPushElement's B0290 alloca round-trip. Returns early like the dup
	// branches above (the binding claims the result; not a stmt-temp).
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
			if extractEnum(elem) != nil && c.vecElemNeedsEnumDrop(elem) && !c.typeNeedsMatchDup(elem) {
				c.dupHeapUserFieldAccess = false // consume the flag
				innerVal := c.block.NewExtractValue(result, 1)
				alloca := c.createEntryAlloca(innerVal.Type())
				c.block.NewStore(innerVal, alloca)
				c.dupEnumElementInPlace(alloca, elem)
				dup := c.block.NewLoad(innerVal.Type(), alloca)
				return c.block.NewInsertValue(result, dup, 1)
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
		// T0908/T0898: Heap user element with NO explicit/synthesized drop
		// (pal_free-only path) needs the same dup-on-read as the droppable branch
		// above. The droppable gate (isDroppableHeapUserType) excludes no-drop types
		// for the T0440 Map-clone reason, so a truly-no-drop heap element (no heap
		// fields, no drop method) would otherwise alias the source slot — combined
		// with the no-drop drop-old in genVectorIndexAssign and the synth-drop owner
		// free, both the destination and the source slot would free the same
		// instance (double-free). dupHeapValue does alloc + memcpy (no sub-fields to
		// dup for a field-less heap type). Mirrors genArrayIndex's T0590 no-drop
		// branch. Required so the drop-on-overwrite added to genVectorIndexAssign
		// doesn't free a slot still aliased by a local.
		if isHeapUserNoDropPalFree(resolvedElem) {
			c.dupHeapUserFieldAccess = false // consume the flag
			return c.dupHeapValue(val, resolvedElem)
		}
		// T1129: Dup-on-read for Vector[droppable-enum] index access (extractNamed is
		// nil for enums, so the heap-user branches above skip them). Without this,
		// `got := v[i]` aliases v's element slot — got's drop and v's element walk both
		// free the same variant data: a double-free that is silent for leaf enums but
		// segfaults for recursive ones (whose drop recurses into the freed buffer).
		// cloneResolvedValue deep-clones uniformly — recursive/container-bearing enums
		// via their synthesized clone (T1129), shallow-dup-safe enums via
		// dupEnumElementInPlace (T1110), clone-bearing enums via clone(). The flag is
		// set only when the var-decl/assign site consumes the index result, so the
		// owned clone is always moved into the binding — no orphan leak.
		if c.enumElemNeedsDupOnRead(resolvedElem) {
			c.dupHeapUserFieldAccess = false // consume the flag
			return c.cloneResolvedValue(val, resolvedElem)
		}
		// T1130: Map/Set element read-back from a Vector's native index. Map/Set are
		// deliberately excluded from isDroppableHeapUserType (T0440, because the Map/Set
		// container's own `[]` body dups V internally), so the heap-user branch above
		// skips them — but here Map/Set is the ELEMENT and the Vector's native `[]` does
		// NOT dup, so `got := v[i]` aliases the container's element. got's drop and the
		// vector's element walk would then free the same Map/Set → double-free.
		// cloneHeapElement deep-clones via the element's clone() (its type-arg safety
		// gating already covers recursive/container-bearing element types).
		if isMapOrSetType(resolvedElem) {
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
		// T1263: a vector element that is itself a value-copying container transitively
		// nesting a closure must NOT be duped — dupVector's element-clone loop zeroes each
		// closure's opaque env (T0813) → null {fn,env} → SEGV on invoke. Leave it ALIASED
		// (a borrow of the outer vector, env intact); the borrow gates
		// (isClosureAggregateBorrow / closureAggregateBorrowSource) suppress the owning
		// drop binding and reject escapes, keeping this in lockstep. FirstFieldNestedClosureDeep
		// treats a top-level container as a FIELD (recurses TypeArgs) yet keeps Ref/Weak/…
		// opaque, so Vector[Ref[…]] / Vector[int] are unaffected.
		resolvedContainerElem := elemType
		if c.typeSubst != nil {
			resolvedContainerElem = types.Substitute(elemType, c.typeSubst)
		}
		if sema.FirstFieldNestedClosureDeep(resolvedContainerElem) != nil {
			c.dupContainerFieldAccess = false // consume the flag
			return val
		}
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
			resolvedArcElem := arcElem
			if c.typeSubst != nil {
				resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
			}
			dup := c.dupArc(val, resolvedArcElem)
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
				// T1291: a non-value structural inner boxes a heap instance (not
				// matched by isDroppableHeapUserType, which excludes structural). Its
				// element drop is now active (T1291), so an aliased read into an owning
				// Optional local would double-free — deep-clone the box on read via
				// dupOptionalVectorElem's structural case.
				if isDroppableHeapUserType(innerElem) || isHeapUserNoDropPalFree(innerElem) ||
					isNonValueStructuralType(innerElem) {
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
			// T0784: same for `x as!/as T` key/value — cast subject is moved
			// into the map slot via []=, so suppress its scope-exit drop.
			// T0849: for the conditional `as` form, drop iff the downcast failed.
			if ident := c.castSubjectMovableIdent(entry.Value); ident != nil {
				c.consumeCastSubjectDropFlag(entry.Value, ident.Name)
			}
			if ident := c.castSubjectMovableIdent(entry.Key); ident != nil {
				c.consumeCastSubjectDropFlag(entry.Key, ident.Name)
			}
			// T1073: `{k!: v!}` — force-unwrap of a droppable inner moves it out of
			// the source optional into the map slot (which the map's drop frees via
			// []=). Neutralize the source optional's present flag so its scope-exit
			// drop doesn't double-free the moved inner.
			c.neutralizeForceUnwrapElem(entry.Value)
			c.neutralizeForceUnwrapElem(entry.Key)
			// Claim heap temps: user type instances passed as map values
			// transfer ownership to the map. Without this, the heap temp
			// cleanup would free the instance, leaving a dangling pointer
			// in the map's Slot enum data.
			c.claimHeapTemp(valVal)
			c.claimHeapTemp(keyVal)
			// T0736: Also claim string/vector stmt-temps for the moved key/value.
			// trackStringTemp / trackVectorTempWithElemType (string concat,
			// to_string(), split(), vector-returning calls) register in stmtTemps,
			// NOT heapTemps — claimHeapTemp doesn't see them. The []= method moves
			// ~K key / ~V value into the map, so without claiming, the caller's
			// stmt-temp cleanup drops the string/vector while the map's scope-exit
			// drop also drops it → double-free ("invalid free (bad header magic)").
			// Only the ident path above is currently covered; a bare heap
			// sub-expression ({"k": a + b}) needs this. Mirrors the genArrayLit
			// element path (T0366).
			c.claimStringTemp(valVal)
			c.claimStringTemp(keyVal)
			// T1239: also claim the closure env temp. Map.[]= takes ~V value by
			// move, so the map's drop owns the closure's heap env. Without this
			// the statement-end cleanupEnvTemps double-frees it → segfault.
			// Mirrors genArrayLit (T0741). The key claim is defensive (a closure
			// can't be Hashable, so not a valid map key today) but costs nothing —
			// claimEnvTemp is a no-op for non-closure values. A capturing lambda
			// literal value has always hit this; T1160 widened the trigger to
			// closure-returning call results.
			c.claimEnvTemp(valVal)
			c.claimEnvTemp(keyVal)
			// B0281: Clear enum ctor temps created during this entry's evaluation.
			// Map.[]= copies the enum value by LLVM value into the map's Slot.
			// Both the temp alloca and the Slot share the same inner pointers
			// (string ptrs, map instance ptrs, etc.). If the temp is dropped
			// at statement end, it frees data the map still references →
			// use-after-free / stack overflow on cleanup.
			// Only clear temps added since savedEnumTemps to avoid clobbering
			// temps from outer expressions (e.g., prior function arguments).
			// T1139: gate on either the key OR the value's static type being an
			// enum — the snapshot covers both entry.Key and entry.Value, so clear
			// the range only if at least one slot is enum-typed. When neither is
			// an enum (e.g. {"k": inspect(Holder.Has(...))}), a borrow
			// intermediate enum-ctor temp evaluated inside the entry must stay
			// tracked so the caller drops it at statement end, else it leaks.
			// Residual: the mixed case (enum value + non-enum key that borrows a
			// *different* enum-ctor temp) still over-claims that inner temp — the
			// whole range is cleared rather than just the moved value's backing
			// temp. Lower priority, contrived nesting.
			keyEnumType := c.info.Types[entry.Key]
			valEnumType := c.info.Types[entry.Value]
			if c.typeSubst != nil {
				keyEnumType = types.Substitute(keyEnumType, c.typeSubst)
				valEnumType = types.Substitute(valEnumType, c.typeSubst)
			}
			if extractEnum(keyEnumType) != nil || extractEnum(valEnumType) != nil {
				for i := savedEnumTemps; i < len(c.enumCtorTemps); i++ {
					c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[i].dropFlag)
				}
				c.enumCtorTemps = c.enumCtorTemps[:savedEnumTemps]
			}
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

	// T0735: Track allocation as heap temp so unclaimed map literals used as rvalue
	// temporaries (function args, method-call receivers) are dropped at statement end.
	// Registered with palFree as the safe default; updateConstructorTempDrop below
	// swaps it for Map[K,V].drop after new() completes. Mirrors genConstructorCallMono
	// (T0135 + T0345).
	c.trackHeapTemp(rawPtr, c.palFree)

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

	// T0735: Swap dropFn palFree → Map[K,V].drop (synthesized; walks _buckets vector,
	// then pal_frees the instance). Without this, cleanupHeapTemps would just pal_free
	// the instance and leak the _buckets vector buffer.
	c.updateConstructorTempDrop(rawPtr, origin, inst)

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

	// Create anonymous function. T1254: qualify the name with the enclosing
	// compilation unit's owner (instance/module) so lambdas created inside
	// monomorphized instance or module bodies get globally-unique names. This
	// lets each per-instance/per-module .bc keep external linkage without
	// colliding with identically-numbered lambdas baked into other cached .bc
	// files at link time.
	lambdaName := fmt.Sprintf(".lambda.%s%d", c.enclosingUnitPrefix(), c.lambdaCounter)
	c.lambdaCounter++
	fn := c.module.NewFunc(lambdaName, retType, params...)
	// T1254: route the lambda into the same compilation unit (.bc) as the
	// function that creates it. Without this, a lambda born inside a
	// monomorphized instance / module method body lands in the main IR while
	// its creating body lands in a cached instance/module .bc — and when that
	// .bc is served from cache (body generation skipped), the lambda is never
	// re-emitted, producing an undefined-symbol link error.
	c.adoptEnclosingCompilationUnit(fn)

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
	savedCastSubjectMatch := c.castSubjectMatch // T0849: function-scoped, like dropFlags
	savedDropBindings := c.dropBindings         // B0035: must save/restore for NLL early drops
	savedLoopScopeDepth := c.loopScopeDepth
	savedWritebacks := c.lambdaWritebacks
	savedEnvOwnedCaptures := c.lambdaEnvOwnedCaptures // T1254
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
	savedDiscardedExpr := c.discardedExpr               // T1029: lambda body is not the discarded statement
	savedDiscardAliasArgPtrs := c.discardAliasArgPtrs   // T1029
	c.goExprFireAndForget = false                       // reset for inner statements (B0109)
	c.panicExitBlock = nil                              // T0262: lambda is a separate function
	c.coroutineReturnBlock = nil                        // T0262: lambda is a separate function
	c.inCoroutine = false                               // T0285: lambda is not a coroutine
	c.coroCleanupBlk = nil                              // T0285: no coroutine infrastructure
	c.coroSuspendBlk = nil                              // T0285: no coroutine infrastructure
	c.discardedExpr = nil                               // T1029: inner ExprStmts set their own
	c.discardAliasArgPtrs = nil                         // T1029

	// Generate lambda body with fresh scope state
	c.fn = fn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.blockCounter = 0
	c.canError = false
	c.currentRetType = sig.Result()
	savedBorrowedValueParams := c.borrowedValueParams // T0945
	c.setBorrowedValueParams(sig)                     // T0945: lambda body sees its own params
	c.scopeBindings = nil
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.castSubjectMatch = nil // T0849: fresh per lambda body; restored below
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
	c.lambdaEnvOwnedCaptures = nil // T1254: fresh per-lambda; populated in capture loop below

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
				// T1254: record captures the env drop function will free, so a
				// `return <capture>` clones instead of handing back the raw pointer
				// (which env_drop would then double-free).
				if c.analyzeEnvCaptureDrop(cv).action != envDropNone {
					if c.lambdaEnvOwnedCaptures == nil {
						c.lambdaEnvOwnedCaptures = make(map[string]bool)
					}
					c.lambdaEnvOwnedCaptures[cv.Obj.Name()] = true
				}
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
			// T1254: `move || -> a` returning an env-owned droppable capture must
			// hand back an independent clone; env_drop frees the retained copy.
			val = c.maybeDupReturnedEnvCapture(val, e.ExprBody, sig.Result())
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
	c.castSubjectMatch = savedCastSubjectMatch // T0849
	c.dropBindings = savedDropBindings         // B0035: restore for NLL early drops
	c.loopScopeDepth = savedLoopScopeDepth
	c.lambdaWritebacks = savedWritebacks
	c.lambdaEnvOwnedCaptures = savedEnvOwnedCaptures // T1254
	c.goExprFireAndForget = savedGoExprFF2
	c.borrowedValueParams = savedBorrowedValueParams   // T0945
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
	c.discardedExpr = savedDiscardedExpr               // T1029
	c.discardAliasArgPtrs = savedDiscardAliasArgPtrs   // T1029

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

// adoptEnclosingCompilationUnit tags a newly-created helper function (lambda or
// env-drop) with the same instance/module ownership as the function currently
// being generated (c.fn). This ensures the helper travels into the same split .bc
// as its creator instead of the main IR (T1254). When the creator's body lives in
// a per-instance or per-module .bc that can be served from cache (body generation
// skipped), the helper must be self-contained in that same .bc — otherwise it is
// never emitted on a cache hit → undefined symbol at link. The helper keeps
// external linkage but its name is owner-qualified (see enclosingUnitPrefix) so
// copies baked into separately-cached objects cannot collide at link time.
func (c *Compiler) adoptEnclosingCompilationUnit(fn *ir.Func) {
	if c.fn == nil {
		return
	}
	encl := c.fn.Name()
	if owner, ok := c.instanceOwnedFuncs[encl]; ok {
		c.instanceOwnedFuncs[fn.Name()] = owner
	}
	if owner, ok := c.moduleOwnedFuncs[encl]; ok {
		c.moduleOwnedFuncs[fn.Name()] = owner
	}
}

// enclosingUnitPrefix returns a name qualifier ("<owner>.") identifying the
// per-instance or per-module compilation unit that owns the function currently
// being generated (c.fn), or "" when it is plain main-IR code. Used to give
// helper functions (lambdas) globally-unique names so their bodies, once routed
// into an owner's cached .bc, cannot collide with identically-numbered helpers
// from other cached objects (T1254).
func (c *Compiler) enclosingUnitPrefix() string {
	if c.fn == nil {
		return ""
	}
	encl := c.fn.Name()
	if owner, ok := c.instanceOwnedFuncs[encl]; ok {
		return owner + "."
	}
	if owner, ok := c.moduleOwnedFuncs[encl]; ok {
		return owner + "."
	}
	return ""
}

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
	// T1254: keep the env-drop helper in the same .bc as its lambda/creator.
	c.adoptEnclosingCompilationUnit(dropFn)

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

// coerceIndirectCallArgs applies the same param-type coercion to closure-call
// arguments that regular calls get via coerceCallArgs — most importantly,
// optional wrapping (bare `T` → `{i1,T}`, and `none` → zeroinitialized `{i1,T}`).
// Without it, an optional argument would be passed as a bare scalar while
// genIndirectCall types the function pointer with the `{i1,T}` aggregate param,
// producing a type-mismatched call that LLVM's x86 backend tolerates but the
// WASM backend lowers to invalid code (T0849).
func (c *Compiler) coerceIndirectCallArgs(sig *types.Signature, args []*ast.Arg, argVals []value.Value) []value.Value {
	if sig == nil || len(argVals) == 0 {
		return argVals
	}
	argTypes := make([]types.Type, len(argVals))
	for i := range argVals {
		if i < len(args) {
			argTypes[i] = c.info.Types[args[i].Value]
		}
	}
	return c.coerceCallArgs(argVals, argTypes, sig.Params(), args, nil)
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

// genNarrowedVariantField reads a named payload field of an enum value that was
// narrowed to a variant via `if x is Variant` (T0993). Mirrors the field-extract
// logic in bindIsDestructureEnum but for a single, non-destructive read: the
// subject is left intact (this is a borrow), so no drop flag / dup is involved.
func (c *Compiler) genNarrowedVariantField(e *ast.MemberExpr, access *sema.VariantFieldAccess) value.Value {
	subject := c.genExpr(e.Target)

	targetType := access.TargetType
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	enumLayout := c.lookupEnumLayout(targetType)
	if enumLayout == nil {
		panic(fmt.Sprintf("codegen: no enum layout for %s", targetType))
	}
	// A `this` enum receiver is an i8* pointer — load to a by-value enum.
	subject = c.enumThisSubject(subject, enumLayout)

	dataType := enumLayout.VariantDataTypes[access.VariantName]
	if dataType == nil {
		panic(fmt.Sprintf("codegen: no variant data layout for %s.%s", targetType, access.VariantName))
	}

	internalType := enumLayout.EnumInternalType.(*irtypes.StructType)
	alloca := c.createEntryAlloca(internalType)
	c.block.NewStore(subject, alloca)

	dataPtr := c.block.NewGetElementPtr(internalType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	typedDataPtr := c.block.NewBitCast(dataPtr, irtypes.NewPointer(dataType))

	idx := access.FieldIndex
	if idx >= len(dataType.Fields) {
		panic(fmt.Sprintf("codegen: variant field index %d out of range for %s.%s", idx, targetType, access.VariantName))
	}
	fieldType := dataType.Fields[idx]
	fieldPtr := c.block.NewGetElementPtr(dataType, typedDataPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(idx)))
	val := c.block.NewLoad(fieldType, fieldPtr)

	// T1011: A non-destructive narrowed read is a borrow by default — an in-scope
	// read returns the raw load (zero-copy). But when the result escapes the
	// narrowing scope (return, var-decl, assignment, consuming arg, constructor
	// field), the consumer site sets a dup-on-escape flag exactly as it does for a
	// normal struct field read. Honor those flags via the shared dupHeapFieldForEscape
	// so the escaping value is an independent copy; otherwise it aliases the subject's
	// payload, which the subject's synth enum drop frees at scope exit → use-after-free
	// / double-free. Gated on the subject enum being droppable (otherwise the field is
	// never freed and a copy would leak).
	if !c.tempTrackingEnabled {
		return val
	}
	fType := access.FieldType
	if c.typeSubst != nil {
		fType = types.Substitute(fType, c.typeSubst)
	}
	if dup, ok := c.dupHeapFieldForEscape(val, fType, c.enumTargetDroppable(targetType)); ok {
		return dup
	}
	return val
}

// narrowedVariantFieldDroppable reports whether a MemberExpr is a non-destructive
// narrowed enum variant field read (`if x is V { x.field }`, T0993) and, if so,
// whether the subject enum is droppable — in which case genNarrowedVariantField
// dups the field for escape, just like a struct field on a droppable owner. The
// dup-flag-setting helpers (constructor/`~`-param args) and isStringFieldDup share
// this so all consumer sites treat a narrowed variant field identically to a
// struct field. T1011.
func (c *Compiler) narrowedVariantFieldDroppable(mem *ast.MemberExpr) (matched bool, droppable bool) {
	access := c.info.NarrowedVariantField[mem]
	if access == nil {
		return false, false
	}
	targetType := access.TargetType
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	return true, c.enumTargetDroppable(targetType)
}

// enumTargetDroppable reports whether a resolved narrowing target type is a
// droppable enum — i.e. its synth drop frees variant payload, so a heap variant
// field escaping the narrowing scope must be cloned rather than aliased. The
// single source for the narrowed-field droppable predicate, shared by
// narrowedVariantFieldDroppable and genNarrowedVariantField. T1011.
func (c *Compiler) enumTargetDroppable(targetType types.Type) bool {
	enum := extractEnum(targetType)
	return enum != nil && c.enumInstanceHasDrop(targetType, enum)
}

// dupHeapFieldForEscape clones a loaded heap-typed field value (string,
// Optional[string], Vector/Channel/Arc/Weak, or Optional[those]) when the active
// dup-on-escape flag (dupStringFieldAccess / dupContainerFieldAccess) is set and
// the owner is droppable, tracking the clone as a statement temp so scope cleanup
// claims it once the consumer takes ownership. Returns (clone, true) when a clone
// was produced; (val, false) for an in-scope borrow (no flag set, or owner not
// droppable), in which case the caller returns val unchanged. The single source of
// truth for field-escape duplication, shared by genFieldAccess (struct fields) and
// genNarrowedVariantField (narrowed enum variant fields, T1011) so a heap field
// behaves identically in every consumer context. fType must already be fully
// substituted by the caller.
func (c *Compiler) dupHeapFieldForEscape(val value.Value, fType types.Type, ownerDroppable bool) (value.Value, bool) {
	// String / Optional[string] — gated on dupStringFieldAccess.
	if c.dupStringFieldAccess && ownerDroppable {
		if extractNamed(fType) == types.TypString {
			c.dupStringFieldAccess = false // consume the flag
			dup := c.dupString(val)
			c.trackStringTemp(dup)
			return dup, true
		}
		// B0181: Handle string? optional fields — extractNamed returns nil for
		// *types.Optional, so unwrap first to check the inner type.
		// B0190: Track the dup as a temp AND store it in optionalStringDup so
		// genOptionalForceUnwrap can return it directly (bypassing extractvalue
		// which creates a different value.Value that claimStringTemp can't match).
		if opt, ok := fType.(*types.Optional); ok && extractNamed(opt.Elem()) == types.TypString {
			c.dupStringFieldAccess = false // consume the flag
			innerStr := c.block.NewExtractValue(val, 1)
			dup := c.dupString(innerStr)
			c.trackStringTemp(dup)
			c.optionalStringDup = dup
			return c.block.NewInsertValue(val, dup, 1), true
		}
	}

	// B0219: Dup vector/channel fields from types with drop.
	// Vector: shallow copy (allocate + memcpy). Channel: incref.
	if c.dupContainerFieldAccess && ownerDroppable {
		// T1263: a value-copying container FIELD transitively nesting a closure cannot be
		// deep-copied (dupVector's element-clone loop zeroes the closure env → SEGV). Leave
		// it ALIASED — the read is a borrow of the owner; the borrow gates suppress the
		// owning drop binding and reject escapes. Mirrors the genVectorIndex guard and
		// T1262's typeNeedsMatchDup(false). Non-closure fields (int[]) keep deep-copying.
		if sema.FirstFieldNestedClosureDeep(fType) != nil {
			c.dupContainerFieldAccess = false // consume the flag
			return val, false
		}
		// T1176/T1173: whole fixed-Array field/binding escaping the owner — the
		// [N x T_v] aggregate merely ALIASES N inner heap allocations (heap-user
		// instances, or string/Vector/Channel/Arc/Weak buffers), so element-wise
		// deep-clone via dupArrayValueForEscape. Without this the owner's synth
		// drop frees the elements at scope exit while the escaped copy still points
		// into them (UAF for string/heap-user, double-free for containers). No temp
		// tracking: every sink that sets the flag stores the value into an owned
		// slot (return → caller bindingDropArray; assign → target bindingDropArray
		// after drop-old; constructor → instance synth drop; move-param → callee
		// param drop), and the subject's synth drop frees the originals exactly once.
		if elemT, n, ok := c.arrayElemNeedsEscapeDup(fType); ok {
			c.dupContainerFieldAccess = false // consume the flag
			return c.dupArrayValueForEscape(val, elemT, n)
		}
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
			return dup, true
		}
		if chElem, ok := types.AsChannel(fType); ok {
			c.dupContainerFieldAccess = false // consume the flag
			dup := c.dupChannel(val)
			c.trackChannelTempWithElemType(dup, chElem) // T0663
			return dup, true
		}
		if arcElem, ok := types.AsArc(fType); ok {
			c.dupContainerFieldAccess = false // consume the flag
			resolvedArcElem := arcElem
			if c.typeSubst != nil {
				resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
			}
			dup := c.dupArc(val, resolvedArcElem)
			c.trackTempWithDrop(dup, c.getOrCreateArcDrop(resolvedArcElem))
			return dup, true
		}
		if weakElem, ok := types.AsWeak(fType); ok {
			c.dupContainerFieldAccess = false // consume the flag
			resolvedWeakElem := weakElem
			if c.typeSubst != nil {
				resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
			}
			dup := c.dupWeak(val, resolvedWeakElem)
			c.trackTempWithDrop(dup, c.getOrCreateWeakDrop(resolvedWeakElem))
			return dup, true
		}
		// T0366: Optional[Vector|Channel|Arc|Weak] fields — dup the inner buffer
		// so the new optional owns an independent copy. Without this, both the
		// source's owner drop and the new variable's optional drop free the same
		// buffer (mirrors the Optional[String] handling above). optionalContainerDup
		// is consumed by genVarDecl (and similar sites) to claim the dup temp once
		// the containing optional is bound to a variable.
		if opt, ok := fType.(*types.Optional); ok {
			elem := opt.Elem()
			if elemType, isVec := types.AsVector(elem); isVec {
				c.dupContainerFieldAccess = false
				elemLLVM := c.resolveType(elemType)
				elemSize := int64(c.typeSize(elemLLVM))
				innerVec := c.block.NewExtractValue(val, 1)
				dup := c.dupVector(innerVec, elemSize)
				// T0540: Deep-clone droppable elements (mirror the bare Vector branch above).
				// T0939: dup is null on the optional's `none` path — guard the clone loop.
				c.emitVectorElementCloneLoopNullable(dup, elemType)
				c.trackVectorTempWithElemType(dup, elemType)
				c.optionalContainerDup = dup
				return c.block.NewInsertValue(val, dup, 1), true
			}
			if chElem, isCh := types.AsChannel(elem); isCh {
				c.dupContainerFieldAccess = false
				innerCh := c.block.NewExtractValue(val, 1)
				dup := c.dupChannel(innerCh)
				c.trackChannelTempWithElemType(dup, chElem) // T0663
				c.optionalContainerDup = dup
				return c.block.NewInsertValue(val, dup, 1), true
			}
			if arcElem, isArc := types.AsArc(elem); isArc {
				c.dupContainerFieldAccess = false
				innerArc := c.block.NewExtractValue(val, 1)
				resolvedArcElem := arcElem
				if c.typeSubst != nil {
					resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
				}
				dup := c.dupArc(innerArc, resolvedArcElem)
				c.trackTempWithDrop(dup, c.getOrCreateArcDrop(resolvedArcElem))
				c.optionalContainerDup = dup
				return c.block.NewInsertValue(val, dup, 1), true
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
				return c.block.NewInsertValue(val, dup, 1), true
			}
		}
	}

	return val, false
}

// dupBorrowedHeapUserPayload deep-clones the inner heap payload of a
// match-borrowed `Optional[heap-user-type]` (T1174) or fixed
// `Array[heap-user-type]` (T1171) ident so a value ESCAPING the `if
// is`/`match` narrowing scope owns it independently. Such a binding
// (T0485/T1012, see matchBindingIsBorrow) merely ALIASES the subject's variant
// payload — the subject's synth enum drop frees the original at scope exit, so
// an escaped alias is a use-after-free (segfault). This is the plain-ident
// analogue of the field-access dup in dupHeapFieldForEscape and the
// container-index dup in genMethodIndex/genVectorIndex (the `optionalHeapDup`
// path): extract the inner value struct, deep-clone it, and re-insert it into
// the Optional.
//
// Returns (dupedVal, true) when a dup was performed — the escape destination
// (return value / assignment target / consuming `~` param / constructor field)
// then owns the fresh inner via its normal drop machinery, and the subject's
// synth enum drop still frees the original exactly once. Returns (val, false)
// for any other expr/type so callers can use it as a transparent pass-through.
//
// Deliberately NOT routed through a read-side flag (dupHeapUserFieldAccess): that
// flag is also set by genIfUnwrapStmt for the IN-SCOPE `if r := maybe` unwrap,
// which must stay zero-copy (the T0512 nested-Optional invariant). Gating on an
// explicit escape-site call keeps in-scope borrows alias-only.
//
// dupHeapValue is null-safe (handles the optional's `none` path via a phi) and
// dispatches through the type's typeinfo clone_fn for polymorphic subtypes
// (T0387); it also deep-clones droppable sub-fields (e.g. Row.name), so no
// shallow alias leaks.
func (c *Compiler) dupBorrowedHeapUserPayload(expr ast.Expr, val value.Value) (value.Value, bool) {
	if val == nil || c.block == nil || c.block.Term != nil {
		return val, false
	}
	ident, ok := unwrapDestructureParens(expr).(*ast.IdentExpr)
	if !ok {
		return val, false
	}
	t := c.info.Types[ident]
	if c.typeSubst != nil && t != nil {
		t = types.Substitute(t, c.typeSubst)
	}
	isMatchBorrowed := c.matchBorrowedIdents != nil && c.matchBorrowedIdents[ident.Name]
	// T1184: a borrowed (default/`&`, non-`~`) fixed-array VALUE param returned or
	// otherwise escaped by value hands back an array whose [N x T_v] elements ALIAS
	// the caller's heap allocations (the caller keeps ownership of the borrow), so
	// both the escaped copy and the caller would free the same elements → double-free
	// / UAF. This is the array analog of the scalar return-implicitly-dups contract
	// (a borrowed `string`/`Vector` param returned as owned is deep-cloned today);
	// element-wise dup below makes the escaped array own its elements independently.
	// Gated on the array shape so only arrays that actually alias heap dup — plain
	// value/copy-element arrays are untouched. The Optional branch stays
	// match-borrowed-only (a distinct, separately-tracked shape; cf. T1183).
	_, isBorrowedArrayParam := c.borrowedArrayParamEscapeDup(ident.Name, t)
	if !isMatchBorrowed && !isBorrowedArrayParam {
		return val, false
	}
	if isMatchBorrowed {
		if inner, ok := c.optionalHeapDupElem(t); ok {
			// val must be the Optional struct { i1 present, T_v value }.
			if _, isStruct := val.Type().(*irtypes.StructType); !isStruct {
				return val, false
			}
			innerVal := c.block.NewExtractValue(val, 1)
			dup := c.dupHeapValue(innerVal, inner)
			return c.block.NewInsertValue(val, dup, 1), true
		}
	}
	// Fixed-Array whose elements alias heap (T1171 heap-user; T1173 string /
	// Vector / Channel / Arc / Weak / Optional[heap-user]): element-wise deep-clone
	// the [N x T_v] aggregate so the escaped array owns independent elements; the
	// subject's synth enum drop (match-borrowed) or the caller's bindingDropArray
	// (borrowed array param, T1184) still frees the originals exactly once.
	if elemT, n, ok := c.arrayElemNeedsEscapeDup(t); ok {
		return c.dupArrayValueForEscape(val, elemT, n)
	}
	return val, false
}

// optionalHeapDupElem reports whether typ is an Optional whose element is a heap
// user type (droppable, or no-drop-but-pal-free) — the shape whose value struct
// merely ALIASES an inner heap instance, so it must be deep-cloned whenever a
// copy escapes the original owner (a match-borrowed variant payload, or a
// container element slot). Returns the resolved inner element type and true when
// so, else (nil, false). Single recognition point shared by
// dupBorrowedHeapUserPayload (escape-site dup) and maybeDupPushElement /
// pushElemNeedsDup (vector-push + slice dup) so the two stay in sync (T1174).
func (c *Compiler) optionalHeapDupElem(typ types.Type) (types.Type, bool) {
	opt, ok := typ.(*types.Optional)
	if !ok {
		return nil, false
	}
	inner := opt.Elem()
	if c.typeSubst != nil {
		inner = types.Substitute(inner, c.typeSubst)
	}
	if isDroppableHeapUserType(inner) || isHeapUserNoDropPalFree(inner) {
		return inner, true
	}
	return nil, false
}

// optionalPushElemNeedsDup reports whether typ is an Optional whose inner value
// aliases heap and must be deep-cloned when the Optional is pushed into a
// container OR when a whole-array copy of an Optional[...][N] escapes its owner.
// PUSH/ESCAPE-path recognizer: broader than optionalHeapDupElem (kept narrow for
// the index-read/field-escape sinks that hardcode dupHeapUserFieldAccess). Also
// matches string / Vector / Channel / Arc / Weak / droppable-tuple /
// droppable-enum / nested-droppable-Optional inners — every shape
// dupOptionalVectorElem can clone.
// Recursion terminates: each level unwraps exactly one Optional. T1183.
func (c *Compiler) optionalPushElemNeedsDup(typ types.Type) (*types.Optional, types.Type, bool) {
	opt, ok := typ.(*types.Optional)
	if !ok {
		return nil, nil, false
	}
	inner := opt.Elem()
	if c.typeSubst != nil {
		inner = types.Substitute(inner, c.typeSubst)
	}
	// T1291: a non-value structural interface inner boxes a heap instance that
	// must be deep-cloned via __promise_structural_clone (dupOptionalVectorElem's
	// structural case) — pushElemNeedsDup excludes structural, so recognize it here.
	if (extractNamed(inner) == types.TypString && !isRefType(inner)) || c.pushElemNeedsDup(inner) || isNonValueStructuralType(inner) {
		return opt, inner, true
	}
	return nil, nil, false
}

// borrowedArrayParamEscapeDup reports whether name is a borrowed (default/`&`,
// non-`~`) value parameter of the current function whose type is a fixed array
// whose elements alias heap — the T1184 shape. Returning/escaping such a param by
// value hands back an aggregate whose element pointers alias the caller's heap
// allocations; the caller keeps ownership of the borrow, so the escaped copy must
// element-wise deep-clone them (see dupBorrowedHeapUserPayload). Reuses
// borrowedValueParams (the single-source borrowed-param set) and
// arrayElemNeedsEscapeDup (the single-source per-array escape predicate). Returns
// the resolved element type and true when so, else (nil, false).
func (c *Compiler) borrowedArrayParamEscapeDup(name string, typ types.Type) (types.Type, bool) {
	if c.borrowedValueParams == nil || !c.borrowedValueParams[name] {
		return nil, false
	}
	if elemT, _, ok := c.arrayElemNeedsEscapeDup(typ); ok {
		return elemT, true
	}
	return nil, false
}

// arrayElemNeedsEscapeDup reports whether typ is a fixed Array whose element's
// value struct merely ALIASES heap — a heap-user type (droppable, or
// no-drop-but-pal-free), string, Vector, Channel, Arc, Weak, a droppable
// enum/tuple, or an Optional whose inner aliases heap — including
// Optional[heap-user] AND Optional[string]/Optional[container] (via
// pushElemNeedsDup's optionalPushElemNeedsDup recognizer, T1183). For such an array the [N x T_v]
// aggregate aliases N inner heap allocations, so a whole-array VALUE copy
// escaping the owner (a struct field read like `return w.rows`, or a
// match-borrowed variant payload) must be element-wise deep-cloned; the owner's
// synth drop otherwise frees the elements at scope exit while the escaped copy
// still points into them (UAF for string/heap-user, double-free for containers).
// Returns the resolved element type, the array size, and true when so, else
// (nil, 0, false). Element recognition reuses pushElemNeedsDup (the single-source
// per-element deep-clone predicate, shared with vector-push) plus the bare-string
// case that push callers handle separately (cf. dupTupleValue). Sibling of
// optionalHeapDupElem; single recognition point shared by dupHeapFieldForEscape
// (field-access + variant-payload escape sinks), dupBorrowedHeapUserPayload
// (match-borrowed-ident sink), setDupFlagsForFieldAccess, and the genIdentExpr
// read-side gate, so the shapes stay in sync (T1176/T1173).
func (c *Compiler) arrayElemNeedsEscapeDup(typ types.Type) (types.Type, int64, bool) {
	arr, ok := typ.(*types.Array)
	if !ok {
		return nil, 0, false
	}
	elem := arr.Elem()
	if c.typeSubst != nil {
		elem = types.Substitute(elem, c.typeSubst)
	}
	if (extractNamed(elem) == types.TypString && !isRefType(elem)) || c.pushElemNeedsDup(elem) {
		return elem, arr.Size(), true
	}
	return nil, 0, false
}

// dupArrayElemForEscape deep-clones one loaded array element whose value struct
// aliases heap (string / Vector / Channel / Arc / Weak / heap-user / droppable
// enum / tuple / Optional[heap-user]) so a whole-array VALUE escaping its owner owns
// the element independently. Reuses the single-source per-element dispatchers
// maybeDupPushElement (every heap element shape, shared with vector-push) and
// dupString (bare string, which push callers handle separately, cf.
// dupTupleValue). Returns elem unchanged for primitive/value/copy elements.
// elemType must already be fully substituted by the caller. NO temp tracking —
// see dupArrayValueForEscape. T1173.
func (c *Compiler) dupArrayElemForEscape(elem value.Value, elemType types.Type) value.Value {
	if extractNamed(elemType) == types.TypString && !isRefType(elemType) {
		return c.dupString(elem)
	}
	if dup := c.maybeDupPushElement(elem, elemType); dup != nil {
		return dup
	}
	return elem
}

// dupArrayValueForEscape element-wise deep-clones a loaded fixed-array VALUE (the
// [N x T_elem] aggregate) whose elements alias heap, rebuilding the aggregate with
// the clones so a whole-array escape (return / store-to-outer / consuming `~`
// param / constructor field) owns its elements independently. The subject's synth
// drop frees the originals exactly once, so there is NO double-free and NO leak —
// hence no temp tracking: the clones flow into an owned sink (caller/target
// bindingDropArray, instance synth drop, or callee param drop). Returns
// (rebuilt, true) on success; (val, false) when val is not an array aggregate.
// Shared by dupHeapFieldForEscape (field-access + enum-target sinks) and
// dupBorrowedHeapUserPayload (match-borrowed-ident sink). T1176/T1173.
func (c *Compiler) dupArrayValueForEscape(val value.Value, elemType types.Type, n int64) (value.Value, bool) {
	if _, isArr := val.Type().(*irtypes.ArrayType); !isArr {
		return val, false
	}
	out := val
	for i := int64(0); i < n; i++ {
		elem := c.block.NewExtractValue(out, uint64(i))
		dup := c.dupArrayElemForEscape(elem, elemType)
		out = c.block.NewInsertValue(out, dup, uint64(i))
	}
	return out, true
}

// enumThisSubject converts a `this` enum receiver (an i8* pointer returned by
// genThisExpr inside an enum method/getter) into the by-value enum value that
// tag extraction and destructuring expect. Any non-i8* subject (a by-value enum
// from a local or parameter) is returned unchanged. Mirrors the i8*-handling in
// genMatchExpr.
func (c *Compiler) enumThisSubject(subject value.Value, layout *TypeDeclLayout) value.Value {
	if !subject.Type().Equal(irtypes.I8Ptr) {
		return subject
	}
	var loadType irtypes.Type
	if layout.MaxVariantDataSize == 0 {
		loadType = irtypes.I32 // fieldless enum: tag only
	} else {
		loadType = layout.EnumInternalType // data enum: {i32 tag, [N x i8] data}
	}
	typedPtr := c.block.NewBitCast(subject, irtypes.NewPointer(loadType))
	return c.block.NewLoad(loadType, typedPtr)
}

func (c *Compiler) genIsEnumVariant(expr ast.Expr, variantName string, layout *TypeDeclLayout) value.Value {
	if _, ok := layout.VariantTag[variantName]; !ok {
		panic(fmt.Sprintf("codegen: unknown enum variant %s", variantName))
	}
	subject := c.genExpr(expr)
	// A `this` enum receiver is an i8* pointer — load the value before tag extraction.
	subject = c.enumThisSubject(subject, layout)
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
	if isThisReceiver(expr) {
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
	if isThisReceiver(expr) {
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

// loadVtablePtrFromInstance loads the dispatch vtable pointer for an i8* instance
// pointer by following the instance's variant→typeinfo chain (instance[0] →
// typeinfo[0] = vtable_ptr). Centralizes the load shared by virtual call sites,
// RTTI error reconstruction, and the abstract-return `return this` value-struct
// build (T0917) — keep a single implementation so the typeinfo layout assumption
// lives in one place.
func (c *Compiler) loadVtablePtrFromInstance(instance value.Value) value.Value {
	variantPtr := c.loadVariantPtr(instance)
	typeinfoStruct := irtypes.NewStruct(irtypes.I8Ptr) // field 0: vtable_ptr
	typeinfoPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoStruct))
	vtableFieldPtr := c.block.NewGetElementPtr(typeinfoStruct, typeinfoPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	return c.block.NewLoad(irtypes.I8Ptr, vtableFieldPtr)
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

	// Resolve the target Named type from the TypeRef.
	//
	// A borrow/reference target (`x as! T&` / `x as! T~`) is an RTTI-checked
	// reborrow: peel the ref to the underlying named type, do the same RTTI cast,
	// and treat the result as a borrow (no ownership transfer, no drop
	// responsibility) — mirroring how borrow-typed return values are handled. The
	// downstream RTTI/result paths are already borrow-agnostic (the result is the
	// bare `{vtable, instance}` value struct, which is exactly a user-type borrow).
	// T0848.
	targetTypeRef := e.Type
	targetIsBorrow := false
	switch ref := targetTypeRef.(type) {
	case *ast.SharedRefTypeRef:
		targetTypeRef, targetIsBorrow = ref.Inner, true
	case *ast.MutRefTypeRef:
		targetTypeRef, targetIsBorrow = ref.Inner, true
	}
	targetRef, ok := targetTypeRef.(*ast.NamedTypeRef)
	if !ok {
		panic(fmt.Sprintf("codegen: unsupported cast target type %T", e.Type))
	}
	targetNamed := c.lookupNamedType(targetRef.Name)
	if targetNamed == nil {
		panic(fmt.Sprintf("codegen: undefined type %s in cast", targetRef.Name))
	}

	// T0761: RTTI cast whose subject is itself an Optional (`opt as! Subtype` /
	// `opt as Target`). genExpr yields the `{ i1, {i8*,i8*} }` optional
	// representation, not the bare `{vtable, instance}` value struct the result
	// paths below assume — unwrap the inner value first. (The same-type
	// `T? as! T` unwrap is short-circuited above.)
	if srcType := c.info.Types[e.Expr]; srcType != nil {
		if c.typeSubst != nil {
			srcType = types.Substitute(srcType, c.typeSubst)
		}
		// T0850: a borrowed optional (`T?&` — e.g. `Ref[T?].borrow` or a
		// `Mutex[T?]` guard's `.borrow`) has srcType SharedRef/MutRef-of-Optional.
		// genExpr auto-derefs the borrow to the loaded `{i1,{i8*,i8*}}` optional, so
		// it must route through the optional-subject path too — otherwise the
		// non-optional RTTI path feeds the optional value to wrapOptional and panics
		// with an insertvalue/store type mismatch. The inner is owned by the external
		// owner (the Arc/Mutex payload), so flag borrowSource → dup, no neutralize.
		borrowSource := false
		switch ref := srcType.(type) {
		case *types.SharedRef:
			if opt, ok := ref.Elem().(*types.Optional); ok {
				srcType, borrowSource = opt, true
			}
		case *types.MutRef:
			if opt, ok := ref.Elem().(*types.Optional); ok {
				srcType, borrowSource = opt, true
			}
		}
		if opt, ok := srcType.(*types.Optional); ok {
			return c.genOptionalCastExpr(e, opt, targetNamed, borrowSource)
		}
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
	if isThisReceiver(e.Expr) {
		instance = c.extractInstancePtrForThis(subject)
	} else {
		instance = c.instancePtrForRTTI(subject, srcType)
	}
	variantPtr := c.loadVariantPtr(instance)

	result := c.block.NewCall(c.funcs["promise_type_is"],
		variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
	isMatch := c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))

	// T0747: For a `this` receiver, genExpr produced a bare instance i8*, not a
	// {vtable, instance} value struct, so the result paths below (return /
	// wrapOptional / downstream field access) would get an i8* where a value
	// struct is required → invalid IR or a codegen panic. Rebuild the value
	// struct, loading the vtable from the object's typeinfo chain — the same
	// reconstruction used for virtual dispatch on a `this` receiver
	// (genVirtualBinaryOp). RTTI casts apply only to reference types (value types
	// have no `is` parents, so `this as! T` in a value-type method is
	// sema-rejected); the value-type guard keeps that unreachable path untouched.
	castResult := subject
	if isThisReceiver(e.Expr) && (c.currentNamed == nil || !c.currentNamed.IsValueType()) {
		typeinfoStruct := irtypes.NewStruct(irtypes.I8Ptr)
		typeinfoPtr := c.block.NewBitCast(variantPtr, irtypes.NewPointer(typeinfoStruct))
		vtableFieldPtr := c.block.NewGetElementPtr(typeinfoStruct, typeinfoPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		vtableRaw := c.block.NewLoad(irtypes.I8Ptr, vtableFieldPtr)
		var vs value.Value = constant.NewUndef(userValueType())
		vs = c.block.NewInsertValue(vs, vtableRaw, 0)
		vs = c.block.NewInsertValue(vs, instance, 1) // instance == this i8* for reference types
		castResult = vs
	}

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
		return castResult // same value struct, type is verified
	}

	// T0849: if the subject is a movable owned local/`~`-param, record the runtime
	// downcast success flag so a consuming site (return / owning-slot store) can
	// drop the subject iff the cast failed (None). castSubjectMovableIdent peels
	// parens/chained casts to the innermost subject ident that carries a drop flag.
	// `as` is a conditional move: the subject's instance is aliased into `some`
	// only on success, untouched on failure — so an unconditional clear/keep is
	// wrong in both consuming contexts. consumeCastSubjectDropFlag reuses isMatch.
	// A borrow target (`x as T&`) is a reborrow with no ownership transfer, so the
	// subject's instance is never moved into `some` — skip the conditional
	// drop-flag handoff that would otherwise let a consuming site drop the subject
	// (T0848). castSubjectMovableIdent already returns nil for borrow-param
	// subjects; the guard makes the no-move invariant explicit and protects the
	// `ownedLocal as T&` shape, where the local must remain owned.
	if ident := c.castSubjectMovableIdent(e); !targetIsBorrow && ident != nil {
		if c.castSubjectMatch == nil {
			c.castSubjectMatch = map[string]value.Value{}
		}
		c.castSubjectMatch[ident.Name] = isMatch
	}

	// as — wrap in Optional { i1, { i8*, i8* } }. User types use value struct representation.
	someBlock := c.newBlock("cast.some")
	noneBlock := c.newBlock("cast.none")
	mergeBlock := c.newBlock("cast.merge")
	c.block.NewCondBr(isMatch, someBlock, noneBlock)

	c.block = someBlock
	optType := irtypes.NewStruct(irtypes.I1, userValueType())
	someResult := c.wrapOptional(castResult, optType)
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

// genOptionalCastExpr generates code for an RTTI cast whose subject is itself an
// Optional: `opt as! Subtype` (force) or `opt as Target` (optional). T0761.
//
// genExpr(e.Expr) yields the `{ i1, {i8*,i8*} }` optional representation. RTTI
// casts apply only to reference types, so the inner (field 1) is always the
// `{i8*,i8*}` userValueType() value struct. We extract it, give the cast result
// a clean ownership story for the inner across every source shape (see the
// ownership block below), then mirror the non-optional cast paths:
//
//   - Force (`as!`): on none OR type mismatch → panic; otherwise return the
//     inner value struct (its view-vtable dispatches correctly, like the
//     non-optional `as!` path).
//   - Optional (`as`): on none OR mismatch → none; otherwise move the inner into
//     `some` and neutralize the source inside the match-only block.
//
// Inner-ownership is reconciled by three coordinated pieces, keyed on the source
// shape so exactly one of them frees the inner on every (present×match) path:
//   - aliasing sources (container element `v[i]`, borrowed `this.field`): the
//     inner is borrowed from an external owner the cast cannot neutralize, so we
//     dup it into an owned copy up front (the external owner still frees the
//     original).
//   - owned sources (the duped copy above, or a call-result temp): the result
//     owns the inner; the `as` path registers it as a heap temp so it is freed
//     on present+mismatch (claimed by the binding on match).
//   - local ident/member sources: the source's own drop binding owns the inner;
//     neutralizeOptionalCastSource (as) / neutralizeForceUnwrapSource at the
//     binding (force, B0293) clears it on the match path only.
//
// T0850: borrowSource is set when the subject is a borrowed optional (`T?&`,
// e.g. `Ref[T?].borrow`). A borrow's inner is owned by an external owner (the
// Arc/Mutex payload) the cast can neither move nor neutralize, so all three
// ownership decisions collapse to the aliasing case: dup the inner up front,
// never neutralize the source (a borrow getter is a MemberExpr whose leaf is a
// getter — neutralizing would mis-resolve it as a field), and let the result own
// the dup (heap-temp tracked so present+mismatch frees it, match claims it).
func (c *Compiler) genOptionalCastExpr(e *ast.CastExpr, opt *types.Optional, targetNamed *types.Named, borrowSource bool) value.Value {
	// T0761: scalar optional subject (`int? as f64` / `char? as! int`). The inner
	// (optional field 1) is a bare scalar, not a `{vtable,instance}` value struct,
	// so the RTTI path below would extractvalue a non-aggregate and panic. Mirror
	// the non-optional scalar path (emitScalarCast): unwrap, convert, (re)wrap.
	elem := opt.Elem()
	if c.typeSubst != nil {
		elem = types.Substitute(elem, c.typeSubst)
	}
	if elemNamed := extractNamed(elem); elemNamed != nil &&
		isPrimitiveScalar(elemNamed) && isPrimitiveScalar(targetNamed) {
		return c.genOptionalScalarCastExpr(e, elemNamed, targetNamed)
	}

	targetID := c.assignTypeID(targetNamed)

	// T0761: Take full ownership control of the subject's inner rather than
	// relying on the binding context's ambient field/index dup (which fires only
	// for an Optional[heap-user-type] LHS and only dups *some* source shapes —
	// container elements but not synth-drop fields — making ownership of `inner`
	// unpredictable from here). Suppress the ambient heap-user-type dup so genExpr
	// yields the raw aliased inner, then dup aliasing sources uniformly below.
	savedDupHeap := c.dupHeapUserFieldAccess
	c.dupHeapUserFieldAccess = false
	optVal := c.genExpr(e.Expr)
	c.dupHeapUserFieldAccess = savedDupHeap

	flag := c.block.NewExtractValue(optVal, 0)
	var inner value.Value = c.block.NewExtractValue(optVal, 1) // {i8*,i8*} value struct

	// For an aliasing source — a container element (`v[i]`) or a borrowed
	// `this.field` — `inner` is borrowed from an external owner that the cast
	// neither moves nor neutralizes (the container/caller still frees it). Dup it
	// so the cast result owns an independent copy, mirroring genOptionalForceUnwrap's
	// borrowed-`this.field` dup. Local-rooted ident/member sources are neutralized
	// instead (B0293), and owned temps (call results) own their inner outright, so
	// neither is duped. dupHeapValue is null-safe, so this is correct even when the
	// optional is none (the dup is a no-op on a null instance).
	if borrowSource || c.optionalCastSourceAliasesExternalOwner(e.Expr) {
		elem := opt.Elem()
		if c.typeSubst != nil {
			elem = types.Substitute(elem, c.typeSubst)
		}
		if named := extractNamed(elem); named != nil && !named.IsValueType() &&
			!named.IsCopy() && !isPrimitiveScalar(named) && named != types.TypString &&
			!types.IsVector(elem) && !types.IsChannel(elem) && !named.IsStructural() &&
			!isOpaqueContainerType(elem) {
			inner = c.dupHeapValue(inner, elem)
		}
	}

	if e.Force {
		// as! — panic on none, then panic on type mismatch, else return inner.
		presentBlock := c.newBlock("optcast.present")
		nonePanicBlock := c.newBlock("optcast.nonepanic")
		c.block.NewCondBr(flag, presentBlock, nonePanicBlock)

		c.block = nonePanicBlock
		nonePanicMsg := c.makeGlobalString("cast failed: optional is none")
		c.block.NewCall(c.funcs["promise_panic"], nonePanicMsg)
		c.emitPanicReturn()

		c.block = presentBlock
		instance := c.instancePtrForRTTI(inner, opt.Elem())
		variantPtr := c.loadVariantPtr(instance)
		result := c.block.NewCall(c.funcs["promise_type_is"],
			variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
		isMatch := c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))

		okBlock := c.newBlock("optcast.ok")
		mismatchBlock := c.newBlock("optcast.mismatch")
		c.block.NewCondBr(isMatch, okBlock, mismatchBlock)

		c.block = mismatchBlock
		mismatchMsg := c.makeGlobalString("cast failed: as! type mismatch")
		c.block.NewCall(c.funcs["promise_panic"], mismatchMsg)
		c.emitPanicReturn()

		c.block = okBlock
		return inner // value struct, type verified; source neutralized (ident/member) or duped (aliasing) above
	}

	// as — none on absent OR mismatch, conditionally move inner into some.
	optType := irtypes.NewStruct(irtypes.I1, userValueType())
	checkBlock := c.newBlock("optcast.check")
	someBlock := c.newBlock("optcast.some")
	noneBlock := c.newBlock("optcast.none")
	mergeBlock := c.newBlock("optcast.merge")
	c.block.NewCondBr(flag, checkBlock, noneBlock)

	c.block = checkBlock
	instance := c.instancePtrForRTTI(inner, opt.Elem())
	variantPtr := c.loadVariantPtr(instance)
	result := c.block.NewCall(c.funcs["promise_type_is"],
		variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
	isMatch := c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))
	// T0761: When the cast result owns `inner` (an owned-temp call result, or an
	// aliasing source we duped above) there is no external owner to free it on the
	// present+mismatch path → leak. Register `inner` as a heap temp here (we are in
	// checkBlock, so the present flag is known true and `inner`'s instance is
	// valid): claimHeapTemp at the binding transfers ownership on match, and
	// cleanupHeapTemps frees it on mismatch — exactly the non-optional cast's
	// behavior. Ident/local-member sources are skipped: their inner is owned by the
	// source's own drop binding (neutralized only on the match path), so tracking
	// here would double-free.
	if borrowSource || c.optionalCastResultOwnsInner(e.Expr) {
		elem := opt.Elem()
		if c.typeSubst != nil {
			elem = types.Substitute(elem, c.typeSubst)
		}
		if dropFunc := c.resolveDropFuncForTemp(extractNamed(elem), elem); dropFunc != nil {
			instPtr := c.extractInstancePtr(inner)
			if instPtr.Type() != irtypes.I8Ptr {
				instPtr = c.block.NewBitCast(instPtr, irtypes.I8Ptr)
			}
			c.trackHeapTemp(instPtr, dropFunc)
		}
	}
	c.block.NewCondBr(isMatch, someBlock, noneBlock)

	c.block = someBlock
	// Conditional move: only on present+match does the result take ownership of
	// the inner; clear the source optional's present flag so its drop becomes a
	// no-op. On none/mismatch the source keeps & frees the inner.
	// T0850: a borrowed optional has no local present flag to clear (the inner was
	// duped above; the external owner keeps & frees the original), so skip.
	if !borrowSource {
		c.neutralizeOptionalCastSource(e.Expr)
	}
	someResult := c.wrapOptional(inner, optType)
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

// genOptionalScalarCastExpr lowers a scalar-to-scalar cast whose subject is an
// Optional (`int? as f64` force, or `int? as f64` optional). T0761. Scalars are
// “ `copy “ with no drop, so there is no ownership/neutralization to do — the
// inner is a bare scalar (optional field 1), not a value struct.
//
//   - Force (`as!`): panic on none, else convert and return the scalar.
//   - Optional (`as`): none on absent; present → some(convert(inner)).
func (c *Compiler) genOptionalScalarCastExpr(e *ast.CastExpr, srcNamed, targetNamed *types.Named) value.Value {
	optVal := c.genExpr(e.Expr)
	flag := c.block.NewExtractValue(optVal, 0)
	inner := c.block.NewExtractValue(optVal, 1) // bare scalar (not a value struct)

	if e.Force {
		presentBlock := c.newBlock("optcast.present")
		nonePanicBlock := c.newBlock("optcast.nonepanic")
		c.block.NewCondBr(flag, presentBlock, nonePanicBlock)

		c.block = nonePanicBlock
		nonePanicMsg := c.makeGlobalString("cast failed: optional is none")
		c.block.NewCall(c.funcs["promise_panic"], nonePanicMsg)
		c.emitPanicReturn()

		c.block = presentBlock
		return c.emitScalarCast(inner, srcNamed, targetNamed)
	}

	// as — none on absent; present → some(convert(inner)).
	optType := irtypes.NewStruct(irtypes.I1, llvmNamedType(targetNamed))
	someBlock := c.newBlock("optcast.some")
	noneBlock := c.newBlock("optcast.none")
	mergeBlock := c.newBlock("optcast.merge")
	c.block.NewCondBr(flag, someBlock, noneBlock)

	c.block = someBlock
	converted := c.emitScalarCast(inner, srcNamed, targetNamed)
	someResult := c.wrapOptional(converted, optType)
	c.block.NewBr(mergeBlock)
	someEnd := c.block

	c.block = noneBlock
	noneResult := constant.NewZeroInitializer(optType)
	c.block.NewBr(mergeBlock)
	noneEnd := c.block

	c.block = mergeBlock
	return c.block.NewPhi(
		&ir.Incoming{X: someResult, Pred: someEnd},
		&ir.Incoming{X: noneResult, Pred: noneEnd},
	)
}

// neutralizeOptionalCastSource clears the present flag of an Optional cast
// source (`opt as Target`) so the source's drop skips the inner value once the
// cast result has taken ownership of it (T0761). Shared with the force-unwrap
// path: handles owned-ident and owned-member sources (peeling ParenExpr),
// reusing neutralizeMemberOptionalField for the member case. Temp/call-result
// sources fall through as a no-op (their inner stays owned by their own temp
// tracking), identical to the existing opt!-on-temp behavior.
func (c *Compiler) neutralizeOptionalCastSource(expr ast.Expr) {
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	switch src := expr.(type) {
	case *ast.IdentExpr:
		alloca, ok := c.locals[src.Name]
		if !ok {
			return
		}
		optType, ok := alloca.ElemType.(*irtypes.StructType)
		if !ok || len(optType.Fields) < 2 {
			return
		}
		flagPtr := c.block.NewGetElementPtr(optType, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), flagPtr)
	case *ast.MemberExpr:
		c.neutralizeMemberOptionalField(src)
	}
}

// optionalCastSourceAliasesExternalOwner reports whether an Optional cast
// subject's inner is borrowed from an external owner that the cast neither moves
// nor neutralizes, so genOptionalCastExpr must dup the inner to give the result
// an independent copy (otherwise both the external owner and the result free it
// → double-free). Two such shapes, mirroring genOptionalForceUnwrap's dup cases:
//   - a container-element access (`v[i]`): the container's drop frees the
//     element; neutralizeOptionalCastSource doesn't handle IndexExpr.
//   - a borrowed-`this.field` access inside a `&this` method (T0428 Case 3B):
//     neutralizeMemberOptionalField deliberately skips it (can't clear a present
//     flag through a borrowed receiver), so the caller still owns the original.
//
// Local-rooted ident/member sources are neutralized instead (their owner's drop
// is skipped on the match path), and owned temps (call results) own their inner
// outright, so none of those is duped. ParenExpr is peeled. T0761.
func (c *Compiler) optionalCastSourceAliasesExternalOwner(expr ast.Expr) bool {
	switch e := unwrapDestructureParens(expr).(type) {
	case *ast.IndexExpr:
		return true
	case *ast.MemberExpr:
		return isThisReceiver(e.Target) && !c.thisRecvIsOwned
	}
	return false
}

// optionalCastResultOwnsInner reports whether the cast result owns `inner` (so
// the `as` path must free it on the present+mismatch branch via a heap temp).
// The result owns `inner` for every source EXCEPT a local-rooted ident or member
// optional, whose own drop binding frees the inner (neutralized only on the
// match path). Aliasing sources (index / borrowed-`this.field`) are duped into an
// owned copy by genOptionalCastExpr, and owned temps (call results) own their
// inner outright — both are owned. ParenExpr is peeled. T0761.
func (c *Compiler) optionalCastResultOwnsInner(expr ast.Expr) bool {
	switch e := unwrapDestructureParens(expr).(type) {
	case *ast.IdentExpr:
		return false
	case *ast.MemberExpr:
		// Borrowed-`this.field` is duped (owned); a local-owner field is neutralized.
		return isThisReceiver(e.Target) && !c.thisRecvIsOwned
	}
	return true
}

// genOptionalHandlerExpr generates code for `optExpr ? { recovery }`.
// Checks the optional flag, runs the handler on none, extracts inner value on some.
func (c *Compiler) genOptionalHandlerExpr(e *ast.ErrorHandlerExpr) value.Value {
	optVal := c.genExpr(e.Expr)
	// T0778: Capture any per-field dup created while evaluating the source (a
	// droppable-owner optional field like `(this.o)? _ {...}` — genFieldAccess
	// makes an INDEPENDENT dup of the inner string/vector and tracks it as a
	// statement temp). Capture it NOW, before the handler body's genBlockValue
	// runs cleanupStmtTemps and nils these fields. The dup is claimed in
	// someBlock below — see the comment there.
	srcStringDup := c.optionalStringDup
	srcContainerDup := c.optionalContainerDup
	// Clear so the handler body's genBlockValue (evaluated next, in noneBlock)
	// does not wrongly claim the SOURCE's dup in the none path — that dup is the
	// present-path value and must be claimed in someBlock instead. genBlockValue
	// is free to set/claim its own dup for a field-access recovery value.
	c.optionalStringDup = nil
	c.optionalContainerDup = nil
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
	var okVal value.Value = c.block.NewExtractValue(optVal, 1)

	// T0778: Droppable-owner field source — `(owner.field)? _ { ... }` where the
	// optional lives in a field of a droppable owner (e.g. borrowed `this.o`).
	// genFieldAccess already made an INDEPENDENT dup of the inner string/vector
	// (srcStringDup/srcContainerDup captured above) — because the owner's own
	// drop still frees the original — and tracked that dup as a statement temp;
	// the present-path okVal IS that dup. Claim the dup's temp slot here in
	// someBlock so the PRESENT runtime keeps it (its drop flag is cleared in this
	// block, which only executes when present; the absent runtime's dup is null,
	// so its statement-end cleanup drop is a null no-op). The merged phi is then
	// tracked EXACTLY ONCE by genExpr's *ast.ErrorHandlerExpr branch (non-ident
	// source ⇒ it trackStringTemp's the phi) and is the sole owner. Without this,
	// the dup is freed at statement end AND aliased by the returned phi →
	// double-free (`fatal: invalid free`). Mirrors genOptionalForceUnwrap's
	// optionalStringDup consumption; ident sources never reach here with a dup
	// (genFieldAccess is not involved → the captured fields are nil).
	if srcStringDup != nil {
		c.claimStringTemp(srcStringDup)
	}
	if srcContainerDup != nil {
		c.claimStringTemp(srcContainerDup)
	}

	// T0775: Member-source optional handler (`owner.field? _ { ... }`) where the
	// owner has a drop that governs the field's inner allocation. The extracted
	// okVal ALIASES the owned field's inner; without a dup it would be freed BOTH
	// by the result's owner (the statement temp at statement end, or the bound LHS
	// at scope end) AND by the owner's drop → double-free (heap-user) or
	// use-after-free crash (vector). Dup the present inner so the result owns an
	// INDEPENDENT copy and the field's owner keeps & frees the original. This is
	// uniform across temporary and binding contexts: neutralizeForceUnwrapSource
	// deliberately does NOT neutralize the field for member-source handlers (see
	// the ErrorHandlerExpr case there), so the owner always frees the original and
	// the dup is the result's sole owner — which also keeps the field reusable
	// (`(h.f? _ {}).x` twice reads the live field both times). Gated to no per-field
	// dup already produced by genFieldAccess (srcStringDup/srcContainerDup — the
	// T0778 binding/borrowed-this path, which makes its own independent copy). The
	// dup is NOT tracked here; genExpr's ErrorHandlerExpr branch tracks the merged
	// phi (non-diverging) or the diverging okVal exactly once as the sole owner.
	//
	// LIMITED to the three types whose handler result is actually taken into
	// independent ownership downstream: string + vector (tracked by genExpr's
	// ErrorHandlerExpr i8* branch via trackStringTemp / trackVectorTemp) and
	// heap-user (tracked by trackHeapUserTypeResult). For refcounted/opaque
	// containers (Arc/Weak/Channel/Mutex/...) the handler result is NOT tracked as
	// an owned temp (the i8* branch only tracks string/vector; trackHeapUserTypeResult
	// early-returns on containers), so the aliasing okVal is already safe — the
	// owner's drop is the sole free and the temp merely reads it. Dup'ing those
	// here would incref/copy with no matching free → leak (the binding context for
	// those is instead covered by genFieldAccess's T0366/T0498 dup, gated above via
	// srcContainerDup).
	if srcStringDup == nil && srcContainerDup == nil &&
		c.isOwnerGovernedMemberOptionalUnwrapSource(e.Expr) {
		rt := c.info.Types[e]
		if c.typeSubst != nil && rt != nil {
			rt = types.Substitute(rt, c.typeSubst)
		}
		if c.selfSubst != nil && rt != nil {
			rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
		}
		named := extractNamed(rt)
		switch {
		case named == types.TypString:
			okVal = c.dupString(okVal)
		case types.IsVector(rt):
			if elemType, ok := types.AsVector(rt); ok {
				elemLLVM := c.resolveType(elemType)
				elemSize := int64(c.typeSize(elemLLVM))
				dup := c.dupVector(okVal, elemSize)
				// T0540: deep-clone droppable elements so the dup owns them.
				c.emitVectorElementCloneLoop(dup, elemType)
				okVal = dup
			}
		case named != nil && !named.IsValueType() && !named.IsCopy() &&
			!isPrimitiveScalar(named) && !named.IsStructural() && !isOpaqueContainerType(rt):
			// Heap user type — okVal is the `{vtable, instance}` value struct;
			// dupHeapValue returns an independent value struct. isOpaqueContainerType
			// already excludes Vector/Channel/Arc/Weak/Mutex/MutexGuard/Task.
			okVal = c.dupHeapValue(okVal, rt)
		}
	}

	// T0778: Non-diverging handler on an ident-source optional whose inner is
	// i8* (string/vector). The phi values are:
	//   - present runtime: okVal (aliases the source optional's owned inner)
	//   - absent  runtime: handler recovery (claimed by genBlockValue → its
	//     drop flag is cleared in noneBlock)
	// With the T0753 ident-skip leaving the phi untracked (see genExpr's
	// *ast.ErrorHandlerExpr branch), the absent-runtime recovery leaks. Fix:
	// neutralize the source ident's present flag here in someBlock (so the
	// optional's scope drop is a no-op in the present runtime), then track
	// the merged phi as an owned statement temp at mergeBlock. The i8*
	// restriction keeps this off the borrow-holding optional path: borrow-
	// holding requires a user-type inner via RTTI cast (isRttiCastBorrow only
	// matches *types.Named user types), so borrow-holding + string/vector is
	// impossible. Diverging handlers stay covered by the existing T0753 skip:
	// the phi degenerates to okVal aliasing the source's inner and stays
	// untracked, with the optional's drop binding governing the lifetime.
	var trackPhiI8Type types.Type
	var trackPhiHeapType types.Type
	if !handlerDiverged && isIdentOptionalUnwrapSource(e.Expr) {
		rt := c.info.Types[e]
		if c.typeSubst != nil && rt != nil {
			rt = types.Substitute(rt, c.typeSubst)
		}
		if c.selfSubst != nil && rt != nil {
			rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
		}
		named := extractNamed(rt)
		// T1085: Extend the T0778 recovery-leak fix beyond string/vector to the
		// opaque i8*-backed containers (Channel/Arc/Weak/Mutex/Task/MutexGuard).
		// isOpaqueContainerType already covers Vector and all the i8* handles.
		// Like string/vector, the absent-runtime recovery is otherwise an
		// untracked phi and leaks. neutralizeForceUnwrapSource is type-agnostic
		// (clears the source ident's present flag) so the source's scope drop is
		// a no-op on the present runtime; the merged phi becomes the sole owner,
		// tracked at mergeBlock below via the type-aware tracker.
		//
		if named == types.TypString || isOpaqueContainerType(rt) {
			trackPhiI8Type = rt
			c.neutralizeForceUnwrapSource(e)
		} else if named != nil && !named.IsValueType() && !named.IsCopy() &&
			!isPrimitiveScalar(named) && !named.IsStructural() &&
			!c.isBorrowHoldingOptionalIdentSource(e.Expr) {
			// T1085: Heap user-type inner (e.g. Map/Set) from an OWNED optional
			// ident source. The recovery may be a block returning a moved-out local
			// (e.g. `o? { mk := map(); mk }`), which genBlockValue claims (its drop
			// flag cleared in noneBlock) — so without phi-tracking the absent-runtime
			// recovery leaks. Neutralizing the OWNED source transfers ownership of the
			// present-arm inner to the merged phi; the phi is then the sole owner and
			// trackHeapValueTemp drops it once at statement end.
			//
			// EXCLUDES borrow-holding optional sources (`CSquare? o = this as CSquare`):
			// there the present arm aliases an external owner's instance and must NOT
			// be dropped. Those keep trackHeapUserTypeResult's T0753 ident-skip (the
			// present arm is governed by the external owner; a bare-constructor
			// recovery is tracked at its own construction site).
			trackPhiHeapType = rt
			c.neutralizeForceUnwrapSource(e)
		}
	}

	c.block.NewBr(mergeBlock)
	someEnd := c.block

	c.block = mergeBlock

	// If handler diverges, no phi needed - only the some path reaches merge
	if handlerDiverged {
		return okVal
	}

	// Both paths reach merge - phi merge the values
	if handlerVal != nil && okVal != nil {
		phi := c.block.NewPhi(
			&ir.Incoming{X: okVal, Pred: someEnd},
			&ir.Incoming{X: handlerVal, Pred: handlerEnd},
		)
		if trackPhiI8Type != nil {
			named := extractNamed(trackPhiI8Type)
			if named == types.TypString {
				c.trackStringTemp(phi)
			} else if named == types.TypVector {
				if elemType, ok := types.AsVector(trackPhiI8Type); ok {
					c.trackVectorTempWithElemType(phi, elemType)
				} else {
					c.trackVectorTemp(phi)
				}
			} else if arcElem, isArc := types.AsArc(trackPhiI8Type); isArc {
				// T1085: opaque container recovery now owned by the merged phi.
				c.trackTempWithDrop(phi, c.getOrCreateArcDrop(arcElem))
			} else if weakElem, isWeak := types.AsWeak(trackPhiI8Type); isWeak {
				c.trackTempWithDrop(phi, c.getOrCreateWeakDrop(weakElem))
			} else if mutexElem, isMutex := types.AsMutex(trackPhiI8Type); isMutex {
				c.trackTempWithDrop(phi, c.getOrCreateMutexDrop(mutexElem))
			} else if taskElem, isTask := types.AsTask(trackPhiI8Type); isTask {
				c.trackTempWithDrop(phi, c.getOrCreateTaskDrop(taskElem))
			} else if _, isMG := types.AsMutexGuard(trackPhiI8Type); isMG {
				if dropFn, ok := c.funcs["MutexGuard.drop"]; ok {
					c.trackTempWithDrop(phi, dropFn)
				}
			} else if chElem, isCh := types.AsChannel(trackPhiI8Type); isCh {
				c.trackChannelTempWithElemType(phi, chElem)
			}
		} else if trackPhiHeapType != nil {
			// T1085: heap user-type inner (e.g. Map/Set) from an OWNED optional
			// ident source — the merged phi owns the present-arm inner (source
			// neutralized) and the absent-arm recovery. genExpr's
			// trackHeapUserTypeResult skips this ident source (T0753), so the phi
			// must be tracked here. trackHeapValueTemp re-validates (drop func
			// present, not a container/string) and is the single authority on
			// whether tracking actually happens.
			c.trackHeapValueTemp(phi, trackPhiHeapType)
		}
		// T1162: Owner-governed member source whose result is a single-owner opaque
		// handle (Channel/Mutex/MutexGuard/Task). These can't be deep-copied, so the
		// present-arm okVal aliases the owner's field (the owner's drop frees it)
		// while the absent-arm handlerVal is a fresh recovery handle nobody else
		// frees → leak. A single compile-time track/skip can't express both
		// ownerships, so register the merged phi as a statement temp with a
		// PER-BRANCH live flag: cleared (0) on the present (some) edge so the owner
		// stays sole owner, armed (1) on the absent (none/recovery) edge so the fresh
		// handle is dropped exactly once at statement end. Mirrors
		// trackElvisResultTemp's per-path flag (T0937/T0951). A `<-` await consumer
		// claims this temp via genReceiveTask's claimStringTemp(gRaw) (the await
		// joins+frees the G), and a bound use claims it via the var-decl
		// claimStringTemp — both neutralize the flag so the consumer/binding governs
		// the drop. Gated to no per-field dup (srcStringDup/srcContainerDup —
		// string/vector/Arc/Weak go through genFieldAccess's independent dup) and the
		// non-diverging path (a diverging handler produces no surviving recovery).
		if trackPhiI8Type == nil && srcStringDup == nil && srcContainerDup == nil &&
			c.tempTrackingEnabled && c.block.Term == nil && phi.Type() == irtypes.I8Ptr &&
			c.isOwnerGovernedMemberOptionalUnwrapSource(e.Expr) {
			if handleDrop := c.optionalHandlerHandleDrop(e); handleDrop != nil {
				if _, already := c.stmtTempMap[phi]; !already {
					flagPhi := c.block.NewPhi(
						&ir.Incoming{X: constant.NewInt(irtypes.I1, 0), Pred: someEnd},
						&ir.Incoming{X: constant.NewInt(irtypes.I1, 1), Pred: handlerEnd},
					)
					c.appendStmtTemp(phi, handleDrop, nil, flagPhi)
				}
			}
		}
		return phi
	}
	return okVal
}

// genOptionalForceUnwrap generates code for T? → T, panicking on none.
// Used by `as!` on optionals and `x!` on optionals.
// T0111: When source is an identifier with a drop binding, clears the drop flag
// (ownership transfers to the unwrapped value). Field access dup is handled by
// the dupStringFieldAccess mechanism in genTypedVarDecl/genInferredVarDecl.
func (c *Compiler) genOptionalForceUnwrap(expr ast.Expr) value.Value {
	// T1143: reset so a stale value from a prior dup-returning call can never
	// leak into this call's tracking decision. Only set below on the plain
	// (no-dup) path when the source is a container index.
	c.optionalUnwrapContainerBorrow = false
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

	// T1143: we reached the plain extractvalue path, so no dup was made (the
	// binding/return/arg dup paths return early above). If the source is a
	// container index (`container[k]!`), the extracted inner aliases the
	// container's owned slot — record this so trackHeapUserTypeResult skips
	// owned-temp registration (the container's drop frees it; tracking the
	// alias as a temp double-frees at scope exit).
	c.optionalUnwrapContainerBorrow = c.isContainerIndexUnwrapSource(expr)

	// T0428 Case 3B: borrowed this.field! — dup the inner heap value so the new
	// variable gets an independent copy. The caller still owns the original (we
	// can't clear the present flag on a borrowed receiver), so both the caller's
	// synth drop and the new variable get independent copies to free.
	if member, ok := expr.(*ast.MemberExpr); ok {
		if isThisReceiver(member.Target) && !c.thisRecvIsOwned {
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
	// T0776: peel ParenExpr so `(o)!` is recognized like `o!` and the source
	// optional's drop owns the inner (mirrors expr.go:234, stmt.go
	// trackHeapUserTypeResult).
	// T0806: For a native-handle field (`Mutex[T]?` / `Task[T]?`) on a
	// droppable owner used as a temporary (`(h.mtx!).lock()`), the owner's
	// (possibly synthesized) drop already governs the handle. Registering the
	// extracted i8* as an owned statement temp double-frees it (statement-end
	// free + owner drop) → segfault. The string/vector siblings are guarded by
	// optionalFieldString/optionalFieldVector above; native handles have no
	// such flag, so skip via the same owner-governed member-source predicate
	// the heap-user case uses (isOwnerGovernedMemberOptionalUnwrapSource, T0775).
	// T1182 gap: a container/array-index borrow (`arr[i]!` / `vec[i]!`, or a
	// clone-less Map value) also aliases the container's owned slot — recorded in
	// optionalUnwrapContainerBorrow above. trackHeapUserTypeResult already skips
	// owned-temp registration for it, but the string/vector/Arc/Weak/Mutex/Task/
	// Channel tracking below is reached inline on the same no-dup path and was
	// unguarded, so an inline `string?[N]`/`Vector?[N]` element unwrap registered
	// the borrowed inner as a statement temp — a double free at scope exit (the
	// container's element drop frees it too). macOS's allocator turns this into a
	// fatal "invalid free (bad header magic)"; other allocators over-free silently.
	if !isIdentOptionalUnwrapSource(expr) && c.tempTrackingEnabled && !c.optionalFieldString && !c.optionalFieldVector &&
		!c.optionalUnwrapContainerBorrow && !c.isOwnerGovernedMemberOptionalUnwrapSource(expr) &&
		!c.isNestedOwnerGovernedUnwrapSource(expr) {
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

// isIdentOptionalUnwrapSource reports whether expr — the source of an optional
// unwrap (`opt!` or `opt? _ { ... }`) — is ultimately a bare identifier, peeling
// ParenExpr wrappers so `(o)!` / `((o))? _ { ... }` are recognized exactly like
// `o!` / `o? _ { ... }`. When true, the source optional has its own scope drop
// binding that governs the inner allocation's lifetime, so the unwrap-extracted
// inner must NOT be registered as an owned statement temp — doing so double-frees
// at scope exit (`fatal: invalid free`). genExpr already sees through ParenExpr
// (it recurses), so the peel only fixes the AST-shape check here; mirrors the
// ParenExpr peeling in neutralizeForceUnwrapSource (T0577).
func isIdentOptionalUnwrapSource(expr ast.Expr) bool {
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	_, isIdent := expr.(*ast.IdentExpr)
	return isIdent
}

// isBorrowingPlaceExpr reports whether expr (peeling ParenExpr) is a place
// expression that, when discarded as a bare statement, only borrows the value it
// designates rather than producing an owned temp: a local/field read (`o`,
// `obj.f`) or an index read (`arr[i]`). The storage behind the place owns the
// value and frees it (variable binding / owner drop / container drop), so a
// discarded-result drop path (T1234) must skip these to avoid a double-free.
// A `move` out of a place is a MoveExpr — not matched here — so it still drops.
func isBorrowingPlaceExpr(expr ast.Expr) bool {
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	switch expr.(type) {
	case *ast.IdentExpr, *ast.MemberExpr, *ast.IndexExpr:
		return true
	}
	return false
}

// isBorrowHoldingOptionalIdentSource reports whether expr (peeling ParenExpr) is
// an ident referring to a borrow-holding optional local — one bound from a
// non-owning borrow (RTTI downcast `x as T` / `T&`/`T~` RHS), recorded in
// borrowOptionalLocals at the var-decl borrow-clear sites (T1085). The present
// arm of a non-diverging handler unwrap on such a source aliases an external
// owner's instance, so it must NOT be neutralized + temp-tracked — the merged phi
// would otherwise drop a value the external owner still frees (double-free).
func (c *Compiler) isBorrowHoldingOptionalIdentSource(expr ast.Expr) bool {
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	ident, ok := expr.(*ast.IdentExpr)
	if !ok {
		return false
	}
	return c.borrowOptionalLocals[ident.Name]
}

// isOwnerGovernedMemberOptionalUnwrapSource reports whether src — the source of
// an optional unwrap (`owner.field!` / `owner.field? _ { ... }`) — is a member
// access `owner.field` whose owner type has a (possibly synthesized) drop that
// governs the field's inner allocation's lifetime (T0775). When true, an unwrap
// used as a temporary must NOT register the extracted inner as an owned
// statement temp (force-unwrap path) — the owner's drop already frees it, so a
// statement-temp drop double-frees. Peels ParenExpr so `(owner.field)!` is
// recognized exactly like `owner.field!`. Mirrors the ident skip
// (isIdentOptionalUnwrapSource) for the member-source case.
func (c *Compiler) isOwnerGovernedMemberOptionalUnwrapSource(src ast.Expr) bool {
	for {
		p, ok := src.(*ast.ParenExpr)
		if !ok {
			break
		}
		src = p.Expr
	}
	mem, ok := src.(*ast.MemberExpr)
	if !ok {
		return false
	}
	ownerType := c.info.Types[mem.Target]
	if c.typeSubst != nil && ownerType != nil {
		ownerType = types.Substitute(ownerType, c.typeSubst)
	}
	if c.selfSubst != nil && ownerType != nil {
		ownerType = types.SubstituteSelf(ownerType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	ownerNamed := extractNamed(ownerType)
	if ownerNamed == nil {
		return false
	}
	return c.ownerHasOrSynthDrop(ownerType, ownerNamed)
}

// isNestedOwnerGovernedUnwrapSource reports whether src — the source of an
// optional unwrap — is itself an optional unwrap (`inner!` / `inner as! T`)
// chain that ultimately bottoms out in an owned ident or owner-governed member.
// This is the nested-Optional double-force shape `r!!` where `r: T??`: the
// outermost owner's (recursive) drop binding governs the innermost inner
// allocation, so the extracted inner is an ALIAS, not a transferred owner.
// Registering it as an owned statement temp double-frees at scope exit — the
// owner's nested-optional drop frees it too (fatal segfault / invalid free for
// native handles like Mutex/Task/MutexGuard whose drop releases OS resources).
// Peels ParenExpr and one-or-more nested force-unwrap layers (OptionalUnwrapExpr
// and force `as!` CastExpr), then reuses the same owner-governed checks the
// single-level guard uses (isIdentOptionalUnwrapSource /
// isOwnerGovernedMemberOptionalUnwrapSource). Only fires for a genuinely nested
// unwrap (at least one force layer peeled) so it never overlaps the single-level
// ident/member guards. A base that is NOT owner-governed (call/borrow result)
// falls through to the normal owned-temp tracking. T1215.
func (c *Compiler) isNestedOwnerGovernedUnwrapSource(src ast.Expr) bool {
	peeled := false
	for {
		switch s := src.(type) {
		case *ast.ParenExpr:
			src = s.Expr
			continue
		case *ast.OptionalUnwrapExpr:
			src = s.Expr
			peeled = true
			continue
		case *ast.CastExpr:
			if s.Force {
				src = s.Expr
				peeled = true
				continue
			}
		}
		break
	}
	if !peeled {
		return false
	}
	return isIdentOptionalUnwrapSource(src) || c.isOwnerGovernedMemberOptionalUnwrapSource(src)
}

// isContainerIndexUnwrapSource reports whether src (peeling ParenExpr) is a
// Map index `m[key]` whose `[]` getter returns Optional[V] by ALIAS — i.e. its
// match-destructure does NOT dup V (T0440). The synthesized `Map.[]` body does
// `match this._buckets[h] { Slot.Used(k, v) => return v, ... }`; the V binding
// is dup'd only when `enumHasDrop && matchFieldNeedsDup(V)` (bindEnumDestructure,
// expr.go), which for a non-enum V reduces to typeNeedsMatchDup(V). So the result
// aliases the bucket's slot exactly when typeNeedsMatchDup(V) is false (V has an
// explicit/synth drop but no usable clone — e.g. `Resource{string; drop()}` or a
// synth-drop struct with a droppable-element vector field). In that case the
// inline `m[k]!` force-unwrap (reaching the plain no-dup extractvalue path)
// borrows the slot, so it must NOT be registered as an owned statement temp —
// the Map's drop frees it; tracking double-frees. When V is clone-bearing the
// `[]` body dups internally, the result is owned, and tracking must stay (else a
// leak). Set is excluded implicitly — it has no `[]`. T1143.
func (c *Compiler) isContainerIndexUnwrapSource(src ast.Expr) bool {
	for {
		p, ok := src.(*ast.ParenExpr)
		if !ok {
			break
		}
		src = p.Expr
	}
	idx, ok := src.(*ast.IndexExpr)
	if !ok {
		return false
	}
	t := c.info.Types[idx.Target]
	if c.typeSubst != nil && t != nil {
		t = types.Substitute(t, c.typeSubst)
	}
	if c.selfSubst != nil && t != nil {
		t = types.SubstituteSelf(t, c.selfSubst.iface, c.selfSubst.concrete)
	}
	// T1182: fixed-array (`arr[i]!`) and Vector (`vec[i]!`) index sources. Their
	// native `[]` read in the plain (no-dup) unwrap path NEVER dups the element —
	// genArrayIndex / genVectorIndex only clone when a sibling dup flag (e.g.
	// dupHeapUserFieldAccess) is set, which happens only in binding/return/arg
	// contexts, not the inline-temp context reaching this predicate. So the
	// unwrap-extracted inner ALWAYS aliases the array/vector's owned slot; the
	// container's element drop frees it, and registering it as an owned temp
	// double-frees at scope exit (segfault for heap-user elements whose drop
	// derefs a freed sub-field, silent over-free for strings). Unlike Map/Set
	// (which dup clone-bearing V inside `[]`), fixed arrays/Vectors have no
	// internal dup here, so the result is ALWAYS a borrow — return true
	// unconditionally. (When a dup DOES occur in a binding/return/arg context,
	// genOptionalForceUnwrap returns early via optionalHeapDup before reaching
	// this predicate, so the flag is only ever set on the genuine borrow path.)
	if _, isArr := t.(*types.Array); isArr {
		return true
	}
	if types.IsVector(t) {
		return true
	}
	if !isMapOrSetType(t) {
		return false
	}
	// Element type V = the index expression's result, peeling the Optional the
	// `[]` getter returns. The aliasing-vs-dup decision must mirror the `[]`
	// body's match-destructure (typeNeedsMatchDup) exactly.
	vt := c.info.Types[idx]
	if c.typeSubst != nil && vt != nil {
		vt = types.Substitute(vt, c.typeSubst)
	}
	if c.selfSubst != nil && vt != nil {
		vt = types.SubstituteSelf(vt, c.selfSubst.iface, c.selfSubst.concrete)
	}
	if opt, ok := vt.(*types.Optional); ok {
		vt = opt.Elem()
		if c.typeSubst != nil && vt != nil {
			vt = types.Substitute(vt, c.typeSubst)
		}
		if c.selfSubst != nil && vt != nil {
			vt = types.SubstituteSelf(vt, c.selfSubst.iface, c.selfSubst.concrete)
		}
	}
	if vt == nil {
		return false
	}
	return !c.typeNeedsMatchDup(vt)
}

// handlerResultIsNativeHandle reports whether an optional handler's unwrapped
// result type is a single-owner native handle (Mutex[T]/Task[T]) — opaque i8*
// handles that genOptionalHandlerExpr does NOT dup. T0838: such member-source
// handler bindings must neutralize the owner's optional field to avoid a
// double-free between the bound local and the owner's drop. The
// typeSubst/selfSubst resolution mirrors genOptionalHandlerExpr so it works
// inside monomorphized generic and structural-default method bodies.
func (c *Compiler) handlerResultIsNativeHandle(e *ast.ErrorHandlerExpr) bool {
	rt := c.info.Types[e]
	if c.typeSubst != nil && rt != nil {
		rt = types.Substitute(rt, c.typeSubst)
	}
	if c.selfSubst != nil && rt != nil {
		rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
	}
	if _, ok := types.AsMutex(rt); ok {
		return true
	}
	_, ok := types.AsTask(rt)
	return ok
}

// optionalHandlerHandleDrop resolves the native drop function for an optional
// handler result that is a single-owner opaque handle represented as a bare i8*
// — Channel[T], Mutex[T], MutexGuard[T], Task[T] (T1162). These cannot be
// deep-copied (genOptionalHandlerExpr's !isOpaqueContainerType dup gate skips
// them), so for an owner-governed member source the present-arm aliases the
// owner's field while the absent-arm recovery handle is left unowned. Returns
// nil for every other result type. rt is substituted first so element types from
// types.As* are concrete. Mirrors elvisResultHandleDrop (T0951) but scoped to the
// handle classes whose recovery actually leaks here — Arc[T]/Weak[T] refcount-dup
// via genFieldAccess's srcContainerDup path and are excluded.
func (c *Compiler) optionalHandlerHandleDrop(e *ast.ErrorHandlerExpr) *ir.Func {
	rt := c.info.Types[e]
	if c.typeSubst != nil && rt != nil {
		rt = types.Substitute(rt, c.typeSubst)
	}
	if c.selfSubst != nil && rt != nil {
		rt = types.SubstituteSelf(rt, c.selfSubst.iface, c.selfSubst.concrete)
	}
	if rt == nil || isRefType(rt) {
		return nil
	}
	named := extractNamed(rt)
	if chElem, ok := types.AsChannel(rt); ok || named == types.TypChannel {
		return c.getOrCreateChannelDrop(chElem)
	}
	if mutexElem, ok := types.AsMutex(rt); ok {
		return c.getOrCreateMutexDrop(mutexElem)
	}
	if _, ok := types.AsMutexGuard(rt); ok || named == types.TypMutexGuard {
		return c.funcs["MutexGuard.drop"]
	}
	if taskElem, ok := types.AsTask(rt); ok {
		return c.getOrCreateTaskDrop(taskElem)
	}
	return nil
}

// isBorrowedThisMemberSource reports whether src (peeling ParenExpr) is a member
// access on a borrowed `this` receiver (`this.field` inside a non-`~this`
// method). For force-unwrap (T0428 Case 3B) genOptionalForceUnwrap makes an
// INDEPENDENT dup of the inner there — the caller still owns the original — so
// that dup DOES need statement-temp tracking and must be excluded from the
// T0775 member skip. (T0775)
func (c *Compiler) isBorrowedThisMemberSource(src ast.Expr) bool {
	for {
		p, ok := src.(*ast.ParenExpr)
		if !ok {
			break
		}
		src = p.Expr
	}
	mem, ok := src.(*ast.MemberExpr)
	if !ok {
		return false
	}
	return isThisReceiver(mem.Target) && !c.thisRecvIsOwned
}

// isForceUnwrapElem reports whether expr (peeling ParenExpr) is a bare
// force-unwrap `o!`. Collection-literal / raise / select-send element sites use
// this to neutralize the source optional after a force-unwrap consume (T1073) —
// the cast form (`o as! T`) is handled separately via consumeCastSubjectDropFlag,
// so guarding to the force-unwrap shape avoids double-neutralizing it.
func isForceUnwrapElem(expr ast.Expr) bool {
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	_, ok := expr.(*ast.OptionalUnwrapExpr)
	return ok
}

// neutralizeForceUnwrapElem neutralizes the source optional of a force-unwrap
// element `o!` whose unwrapped inner needs dropping. Used at the collection-literal
// (array/tuple/map), raise, and select-send consume sites where `o!` moves the
// inner into a container / error-slot / channel that owns and frees it. Without
// this, the source optional's own scope-exit drop double-frees the moved inner
// (T1073). Self-gating: a no-op unless the element is a bare force-unwrap (the
// cast form is handled via consumeCastSubjectDropFlag) of a droppable inner
// (copy/scalar inners aren't consumed, so their source must stay intact).
func (c *Compiler) neutralizeForceUnwrapElem(elemExpr ast.Expr) {
	if !isForceUnwrapElem(elemExpr) {
		return
	}
	t := c.info.Types[elemExpr]
	if c.typeSubst != nil {
		t = types.Substitute(t, c.typeSubst)
	}
	if !c.typeNeedsFieldDrop(t) {
		return
	}
	c.neutralizeForceUnwrapSource(elemExpr)
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
			// T0775: member-source handlers (`owner.field? _ { ... }`) are made
			// independent by the present-arm dup in genOptionalHandlerExpr — the
			// owner keeps & frees the original, so the field must NOT be neutralized
			// (neutralizing would orphan the original → leak; not neutralizing keeps
			// the field reusable). Ident sources have no dup and still neutralize.
			//
			// T0838 EXCEPTION: Mutex[T]/Task[T] are single-owner opaque i8*
			// handles that genOptionalHandlerExpr does NOT dup (opaque containers
			// can't be deep-copied — see its !isOpaqueContainerType gate). So a
			// handler BINDING `Mutex[int] m = h.mtx? _ {...}` would leave both the
			// bound local and the owner's optional field owning the same handle →
			// double-free. Neutralize the owner field for these, mirroring T0806
			// Fix C for the force-unwrap binding. neutralizeMemberOptionalField
			// applies the same Mutex/Task carve-out (T0806) when clearing the flag.
			if !c.isOwnerGovernedMemberOptionalUnwrapSource(e.Expr) || c.handlerResultIsNativeHandle(e) {
				inner = e.Expr
			}
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
	// Clear the source optional's present flag (ident) or the owner's optional
	// field flag (member) — shared with the optional-cast move path (T0761). The
	// MemberExpr arm (T0392) clears the present flag in the owner's instance
	// memory so the owner's drop skips the field rather than double-freeing.
	c.neutralizeOptionalCastSource(inner)
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
	// T0613: peel ParenExpr at each chain step so paren-wrapped roots/links
	// ((this).field!, (outer).inner.field!) resolve to the IdentExpr/ThisExpr
	// the switch below handles, rather than falling through to the default arm.
	chain := []*ast.MemberExpr{m}
	cur := ast.Expr(unwrapDestructureParens(m.Target))
	for {
		if me, ok := cur.(*ast.MemberExpr); ok {
			chain = append([]*ast.MemberExpr{me}, chain...)
			cur = unwrapDestructureParens(me.Target)
		} else {
			break
		}
	}

	// Resolve the root alloca and initial owner named type.
	var ownerAlloca *ir.InstAlloca
	var ownerType types.Type // used for layout lookup (ident-rooted chains)
	var ownerNamed *types.Named
	var rootIsThis bool
	// T0843: when the chain root is a container element (`cs[0].tsk!`), there is
	// no alloca for the element — we GEP/extract the heap instance pointer
	// directly and stash it here so the post-switch code uses it as-is.
	var precomputedInstance value.Value

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
	case *ast.IndexExpr:
		// T0843: optional single-owner handle field (Task[T]?/Mutex[T]?) on an
		// element of an OWNED container, consumed by an await (`<-(cs[0].tsk!)`) or
		// taken by a binding move (`Mutex[int] m = cs[0].mtx!`). The unwrap takes the
		// handle out; clear the element's optional present flag so the container's
		// scope-exit element drop does not double-free it. Mirrors the IdentExpr
		// owned-local path. Reached from both genReceiveTask (await) and
		// neutralizeForceUnwrapSource (binding move) — so this also covers the
		// optional-Mutex half of T0842. The non-optional `<-cs[0].t` is already covered
		// by the T0638 genReceiveTaskSlotPtr slot-null; the non-optional Mutex move
		// `cs[0].m` (no `!`) has neither slot-null nor optional flag and is the open
		// T0842 gap.
		containerType := c.info.Types[root.Target]
		if c.typeSubst != nil {
			containerType = types.Substitute(containerType, c.typeSubst)
		}
		// Don't neutralize through a borrowed container.
		if _, isShared := containerType.(*types.SharedRef); isShared {
			return
		}
		if _, isMut := containerType.(*types.MutRef); isMut {
			return
		}
		elemType := c.info.Types[root] // type of cs[0] = element type
		if c.typeSubst != nil {
			elemType = types.Substitute(elemType, c.typeSubst)
		}
		ownerNamed = extractNamed(elemType)
		if ownerNamed == nil {
			return
		}
		if ownerNamed.IsValueType() || ownerNamed.IsStructural() {
			return
		}
		if !ownerNamed.HasDrop() && !ownerNamed.NeedsSynthDrop() {
			if inst, ok := elemType.(*types.Instance); ok {
				if !monoInstNeedsSynthDrop(inst) {
					return
				}
			} else {
				return
			}
		}
		ownerType = elemType
		precomputedInstance = c.extractInstancePtr(c.genExpr(root))
	default:
		return
	}

	// Load the root instance pointer.
	var rootInstance value.Value
	if precomputedInstance != nil {
		// T0843: container element — heap instance ptr already computed above.
		rootInstance = precomputedInstance
	} else {
		ownerVal := c.block.NewLoad(ownerAlloca.ElemType, ownerAlloca)
		if rootIsThis {
			// ~this: the alloca holds an i8* instance pointer directly.
			rootInstance = ownerVal
		} else {
			rootInstance = c.extractInstancePtr(ownerVal)
		}
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
		// T0806: Mutex[T]/Task[T] are single-owner opaque i8* handles that
		// genFieldAccess does NOT dup (unlike Arc/Weak refcount-dups). Moving one
		// out of an owner's optional field (binding force-unwrap, or `<-(h.tsk!)`
		// which consumes the task) must clear the owner's present flag so the
		// owner's drop does not double-free the handle we already took ownership
		// of. So let these two through the opaque-container skip below.
		_, isMutexField := types.AsMutex(innerElem)
		_, isTaskField := types.AsTask(innerElem)
		if innerNamed == nil || innerNamed.IsValueType() || innerNamed.IsCopy() ||
			isPrimitiveScalar(innerNamed) || innerNamed.IsStructural() ||
			(isOpaqueContainerType(innerElem) && !isMutexField && !isTaskField) {
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
			// Unreachable: sema rejects non-call `go` operands (T1149). This
			// guards the sema/codegen contract — reaching it means a check was
			// skipped upstream.
			panic(fmt.Sprintf("codegen: internal error: go operand should be a call after sema, got %T", e.Expr))
		}
		return c.genGoCallExpr(callExpr)
	}
	// go { block } form
	return c.genGoBlock(e)
}

// goArgBorrowDrop records a heap argument temporary whose ownership belongs to a
// goroutine frame and must be dropped on the goroutine side (T1098). Only borrow
// params are recorded — for move params the callee consumes and drops the
// temporary at its own scope exit.
type goArgBorrowDrop struct {
	paramIdx int        // index into coroFn.Params holding the argument value
	dropFunc *ir.Func   // concrete drop function (promise_string_drop, Vector[T].drop, T.drop)
	elemType types.Type // vector element type for element drops (nil for non-vectors)
	isStruct bool       // coro param is a value struct {vtable, instance} → drop field 1 (heap user type)
	isEnum   bool       // T1154: coro param is an enum value struct; spill to alloca and pass &slot to the enum drop fn
	capType  types.Type // T1198: dup'd borrowed-param capture — drop by type via emitVariantFieldDrop
}

// emitGoArgBorrowDrops frees the heap argument temporaries owned by a goroutine
// frame for borrow params (T1098). Emitted in the coroutine body immediately
// after the target call returns. Returns the current block (drop loops for
// vector elements split the control flow). Reuses emitVectorElementDropLoop by
// pointing c.fn/c.entryBlock/c.block at the coroutine's frame for the duration.
func (c *Compiler) emitGoArgBorrowDrops(coroFn *ir.Func, entry, cur *ir.Block, drops []goArgBorrowDrop) *ir.Block {
	if len(drops) == 0 {
		return cur
	}
	savedFn, savedBlock, savedEntry := c.fn, c.block, c.entryBlock
	c.fn, c.entryBlock, c.block = coroFn, entry, cur
	defer func() { c.fn, c.block, c.entryBlock = savedFn, savedBlock, savedEntry }()

	for _, d := range drops {
		param := coroFn.Params[d.paramIdx]
		switch {
		case d.capType != nil:
			// T1198: a dup'd borrowed-param capture (see genGoCallExpr). Drop by type
			// via the canonical drop-by-type helper, which handles string/vector/Map/
			// Set/heap-user/polymorphic uniformly (incl. no-drop pal_free). It operates
			// on c.block and may split blocks / create entry allocas via c.entryBlock —
			// the frame swap above already points c.fn/c.entryBlock/c.block at coroFn.
			c.emitVariantFieldDrop(param, d.capType)
		case d.isEnum:
			// T1154: enum value passed by value into the coro frame — spill it to a
			// frame slot so the synthesized enum drop fn (which takes a pointer to
			// the value-struct layout and switches on the tag) can free the variant
			// payload exactly once.
			slot := c.createEntryAlloca(param.Type())
			c.block.NewStore(param, slot)
			c.block.NewCall(d.dropFunc, c.block.NewBitCast(slot, irtypes.I8Ptr))
		case d.isStruct:
			// Heap user type passed as a value struct {vtable, instance}: drop
			// the heap instance pointer.
			inst := c.block.NewExtractValue(param, 1)
			var instI8 value.Value = inst
			if inst.Type() != irtypes.I8Ptr {
				instI8 = c.block.NewBitCast(inst, irtypes.I8Ptr)
			}
			c.block.NewCall(d.dropFunc, instI8)
		default:
			// i8* string/vector: drop droppable elements (vectors) then free the buffer.
			var ptr value.Value = param
			if param.Type() != irtypes.I8Ptr {
				ptr = c.block.NewBitCast(param, irtypes.I8Ptr)
			}
			if d.elemType != nil {
				c.emitVectorElementDropLoop(ptr, d.elemType)
			}
			c.block.NewCall(d.dropFunc, ptr)
		}
	}
	return c.block
}

// genGoCallExpr handles `go func(args...)` — the common case.
// For non-IdentExpr callees (method calls, module calls, etc.), delegates to
// genGoCallExprViaBlock which uses the full codegen context inside the coroutine body.
func (c *Compiler) genGoCallExpr(callExpr *ast.CallExpr) value.Value {
	// Complex callees (method calls, module calls, generic calls, etc.)
	// need the full codegen context — use block-style coroutine (B0113).
	ident, ok := callExpr.Callee.(*ast.IdentExpr)
	if !ok {
		return c.genGoCallExprViaBlock(callExpr)
	}

	// T1024: A bare-ident callee that resolves to a generic free function with
	// INFERRED type args has no plain entry in c.funcs (only the monomorphized
	// `gget__int` exists), so resolveGoTarget would panic. Route it through the
	// block-style path — the same path the explicit-type-arg form (IndexExpr
	// callee) already takes — which builds the call via genExpr → the inferred
	// generic-call codegen.
	if _, ok := c.info.InferredTypeArgs[callExpr]; ok {
		return c.genGoCallExprViaBlock(callExpr)
	}

	// 1. Resolve result type T from sema
	callResultType := c.info.Types[callExpr]
	isVoid := (callResultType == nil || callResultType == types.TypVoid)
	var resultLLVM irtypes.Type = irtypes.Void
	if !isVoid {
		resultLLVM = c.resolveType(callResultType)
	}

	// T1098: resolve the callee signature so we can distinguish move (consume)
	// params from borrow params for argument-temporary ownership transfer.
	var calleeParams []*types.Param
	if fn := c.lookupFunc(ident.Name); fn != nil {
		if sig, ok := fn.Type().(*types.Signature); ok {
			calleeParams = sig.Params()
		}
	}

	// 2. Evaluate arguments in caller scope
	//
	// T1098: A heap-allocated argument temporary (e.g. `s.clone()`, `Box(...)`)
	// is created in the caller's scope but is consumed by the goroutine, which may
	// not run until after the spawning statement completes. The caller's
	// end-of-statement cleanup must therefore NOT free it — ownership belongs to
	// the goroutine frame. For each argument we identify the SINGLE owned
	// temporary that the argument value represents (its root), clear that one
	// temporary's caller-side drop flag, and — for borrow params — record a
	// goroutine-side drop emitted after the target call returns. Move params get
	// no goroutine-side drop: the callee consumes and drops the temporary at its
	// own scope exit. Intermediates the argument expression created but does NOT
	// yield (e.g. the trim() result in `s.trim().clone()`) keep their flags and
	// are freed by the caller — they never enter the goroutine.
	var argVals []value.Value
	var argLLVMTypes []irtypes.Type
	var argTypes []types.Type
	var argBorrowDrops []goArgBorrowDrop
	// T1108/T1154: snapshot enum-ctor temps so we can drop them from the
	// caller's statement-end cleanup after the loop — a synchronous statement-end
	// drop would be a use-after-free since the goroutine may reference the payload
	// after the spawning statement. T1154: for an inline enum-ctor arg whose
	// payload is droppable and the param is a borrow, the goroutine frame takes
	// ownership and drops via the synthesized enum drop fn after the call returns
	// (see the per-arg enum branch + emitGoArgBorrowDrops). Move params are
	// consumed and dropped by the callee.
	savedGoEnumTemps := len(c.enumCtorTemps)
	for i, arg := range callExpr.Args {
		savedHeap := len(c.heapTemps)
		savedStmt := len(c.stmtTemps)
		savedArgEnum := len(c.enumCtorTemps)
		// T1106/T1107: don't register a top-level match/if phi arg as a caller stmt
		// temp — the go-arg machinery below transfers it into the goroutine frame.
		savedSuppress := c.suppressMergeResultTemp
		c.suppressMergeResultTemp = true
		v := c.genCallArgExpr(arg.Value)
		c.suppressMergeResultTemp = savedSuppress
		argVals = append(argVals, v)
		argLLVMTypes = append(argLLVMTypes, v.Type())
		argTypes = append(argTypes, c.info.Types[arg.Value])

		// Identify the argument's single root owned temporary with a statically
		// known drop. Only unambiguous roots are transferred:
		//   - a string/vector temp whose tracked SSA value IS the argument value
		//     (covers `s.clone()`, and `s.trim().clone()` where the trim()
		//     intermediate correctly stays with the caller);
		//   - a lone heap user-type temp from a direct constructor (covers
		//     `Box(s: s.clone())` — the inner string is a claimed stmt temp, so
		//     exactly one new heap temp remains).
		// T1106: conditional/polymorphic args (match/if expressions, nested
		// constructors) yield a runtime phi over temporaries with possibly
		// different concrete drops — no single static root. These are handled by
		// the Case A / Case B branches below via runtime drop dispatch. T1154: a
		// top-level inline enum-ctor arg with a droppable payload IS a single static
		// root: it is transferred to the goroutine frame via the enum branch below.
		newHeap := c.heapTemps[savedHeap:]
		newStmt := c.stmtTemps[savedStmt:]
		newEnum := c.enumCtorTemps[savedArgEnum:]

		// T1157: whether the argument's OUTER type is an enum. An inline enum-ctor
		// arg with a heap-user-type payload (`Wrap.One(Box(...))`) leaves the inner
		// Box temp in newHeap (already claimed by the enum ctor), so the
		// `len(newHeap)==1` heap-root branch would mis-route to the isStruct path
		// using the inner type's drop fn and extracting the wrong field ([16 x i8]
		// payload data, not a pointer) → invalid IR. Routing on argIsEnum keeps the
		// enum as the root and drops via the synthesized per-enum drop fn.
		argIsEnum := false
		if at := argTypes[i]; at != nil {
			switch t := at.(type) {
			case *types.Enum:
				argIsEnum = true
			case *types.Instance:
				_, argIsEnum = t.Origin().(*types.Enum)
			}
		}

		var d goArgBorrowDrop
		d.paramIdx = i
		var rootFlag *ir.InstAlloca
		if idx, ok := c.stmtTempMap[v]; ok && idx >= 0 {
			st := c.stmtTemps[idx]
			rootFlag, d.dropFunc, d.elemType = st.dropFlag, st.dropFunc, st.elemType
			c.stmtTempMap[v] = -1
		} else if len(newEnum) == 1 && argIsEnum {
			// T1154/T1157: the arg's outer type is an enum and it is an inline enum
			// constructor — the enum is the root. Drop via the synthesized per-enum
			// drop fn, which switches on the tag and recurses into the payload drop
			// (string/vector OR a heap-user-type payload's T.drop). Gated on
			// argIsEnum so a nested ctor like `Box(Msg.Text(...))` (outer = heap user
			// type) keeps its heap-root handling. Routed here regardless of newHeap: a
			// heap-user-type payload (`Wrap.One(Box(...))`) leaves the inner Box temp
			// in newHeap — already claimed (caller flag cleared) by the enum ctor's
			// claimHeapTemp — so routing it to the isStruct branch (T1157) extracted
			// the wrong field ([16 x i8] data, not a pointer) and ran the wrong
			// (inner) drop fn → crash. The enum is passed by value into the coro
			// frame; its drop fn takes a pointer to the value-struct layout, so the
			// goroutine spills the param and drops.
			et := newEnum[0]
			rootFlag, d.dropFunc = et.dropFlag, et.dropFunc
			d.isEnum = true
		} else if len(newHeap) == 1 {
			ht := newHeap[0]
			rootFlag, d.dropFunc, d.elemType = ht.dropFlag, ht.dropFunc, ht.elemType
			_, d.isStruct = v.Type().(*irtypes.StructType)
		}
		if rootFlag == nil || d.dropFunc == nil {
			isMove := i < len(calleeParams) && calleeParams[i].Ref() == types.RefMut

			// T1106 Case A: a heap value-struct argument whose concrete type is not
			// statically known — a runtime phi over match/if arms (each possibly a
			// different concrete type) or a nested constructor (multiple live heap
			// temps). A single static dropFunc would run the WRONG concrete drop on
			// the non-selected arm, so the goroutine-side drop dispatches at runtime
			// through the value's typeinfo drop_fn_ptr via __promise_structural_drop.
			// claimHeapTemp runtime-compares v's instance pointer against each tracked
			// heap temp and clears exactly the live arm's caller flag (non-taken arms
			// hold null; nested-ctor inner temps were already claimed into the outer
			// at construction). Move params: the callee consumes and drops via the
			// same structural dispatch, so no goroutine-side drop is recorded.
			if _, ok := v.Type().(*irtypes.StructType); ok && len(newHeap) >= 1 && c.structuralDrop != nil {
				c.claimHeapTemp(v)
				if !isMove {
					argBorrowDrops = append(argBorrowDrops, goArgBorrowDrop{
						paramIdx: i, dropFunc: c.structuralDrop, isStruct: true,
					})
				}
				continue
			}

			// T1106 Case B: a string/vector argument whose root is a runtime phi over
			// multiple owned temps (`if c { s1.clone() } else { s2.clone() }`). The
			// drop is homogeneous (all string, or same-element vector), so a single
			// static dropFunc over the selected temp suffices. clearMatchingStmtTemps
			// clears the live arm's caller flag by runtime comparison; intermediates
			// (e.g. the trim() result in `s.trim().clone()`) have a different pointer
			// and stay with the caller.
			if _, ok := v.Type().(*irtypes.StructType); !ok && len(newStmt) >= 1 {
				at := argTypes[i]
				_, isVec := types.AsVector(at)
				if at != nil && (extractNamed(at) == types.TypString || isVec) {
					rep := newStmt[len(newStmt)-1]
					c.clearMatchingStmtTemps(v, newStmt)
					if !isMove {
						argBorrowDrops = append(argBorrowDrops, goArgBorrowDrop{
							paramIdx: i, dropFunc: rep.dropFunc, elemType: rep.elemType,
						})
					}
					continue
				}
			}

			// T1198: a bare-ident borrowed heap value param of the spawning function
			// (no owned temp, no drop flag) is async-read by the coroutine after the
			// caller frees its own borrowed-arg stmt-temps → UAF. Dup it at spawn time
			// so the goroutine owns a private copy, and record a goroutine-side drop of
			// the dup (the caller's borrow is untouched → no double-free; the dup is
			// freed → no leak). Sibling of T0688/T0731 (value-block) and T1197
			// (via-block); this is the fast bare-ident free-function path. Skip moves
			// (handled below — the callee consumes them).
			if id, ok := arg.Value.(*ast.IdentExpr); ok && !isMove && c.borrowedValueParams[id.Name] {
				capType := argTypes[i]
				if c.typeSubst != nil && capType != nil {
					capType = types.Substitute(capType, c.typeSubst) // monomorphization
				}
				// Channels are refcounted (B0163 loop below) — sharing the pointer is
				// fine. Copy/Arc/Task/value types embed data and never alias caller heap.
				if _, isCh := types.AsChannel(capType); !isCh && goElemNeedsBorrowedCaptureDup(capType) {
					argVals[i] = c.dupBorrowedCaptureForResult(argVals[i], capType)
					argBorrowDrops = append(argBorrowDrops, goArgBorrowDrop{paramIdx: i, capType: capType})
					continue
				}
			}

			// T1148: a moved NAMED variable root (e.g. `go f(move x)` where x is a
			// local/loop variable bound to a heap value) has no temporary to
			// transfer, but its caller-side drop flag must still be cleared — the
			// move param's callee consumes and drops it. Without this, both the
			// caller's scope/loop teardown AND the callee free it → double free.
			if isMove {
				if ident, ok := arg.Value.(*ast.IdentExpr); ok {
					c.clearDropFlag(ident.Name)
				}
			}
			continue // plain ident/literal, or no single static root to transfer
		}

		// Claim the root for the goroutine frame: caller cleanup must skip it.
		if c.block != nil && c.block.Term == nil {
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), rootFlag)
		}

		// Move (consume) param: the callee owns and drops the temporary itself,
		// so the goroutine emits no drop (that would be a double-free).
		if i < len(calleeParams) && calleeParams[i].Ref() == types.RefMut {
			continue
		}
		// Borrow param: the goroutine frame drops the temporary after the call.
		argBorrowDrops = append(argBorrowDrops, d)
	}
	// T1108/T1154: remove inline enum-constructor temps produced for go-call args
	// from the caller's statement-end cleanup — ownership is transferred to the
	// goroutine frame (borrow params drop via the synthesized enum drop fn after
	// the call; move params are consumed by the callee), not freed by a
	// synchronous statement-end drop that could race the goroutine's read.
	c.enumCtorTemps = c.enumCtorTemps[:savedGoEnumTemps]

	// B0163: Increment refcount for channel arguments passed to go calls.
	chanTypeDC := channelStructType()
	for i, arg := range callExpr.Args {
		if ident, ok := arg.Value.(*ast.IdentExpr); ok {
			if binding, ok := c.dropBindings[ident.Name]; ok {
				elemType, isCh := types.AsChannel(binding.valType)
				if isCh || binding.named == types.TypChannel {
					chPtr := c.block.NewBitCast(argVals[i], irtypes.NewPointer(chanTypeDC))
					rcField := c.block.NewGetElementPtr(chanTypeDC, chPtr,
						constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(chanFieldRefcount)))
					c.emitAtomicAdd(c.block, rcField, constant.NewInt(irtypes.I64, 1), irtypes.I64)

					// T1158: balance the increment with a goroutine-side drop. The
					// goroutine borrows the channel (a plain ident is never a `move`,
					// which would not be an *ast.IdentExpr); without a matching
					// decrement the refcount never reaches 0 and the channel + its
					// buffers leak (5 allocations). Channel[T].drop's atomic refcount
					// gates the actual free, so caller and goroutine drops are safe in
					// any order. Resolve the element type so buffered T items drop too.
					if elemType == nil && binding.named == types.TypChannel && c.typeSubst != nil {
						if tp := types.TypChannel.TypeParams(); len(tp) > 0 {
							elemType = c.typeSubst[tp[0]]
						}
					}
					if c.typeSubst != nil && elemType != nil {
						elemType = types.Substitute(elemType, c.typeSubst)
					}
					argBorrowDrops = append(argBorrowDrops, goArgBorrowDrop{
						paramIdx: i, dropFunc: c.getOrCreateChannelDrop(elemType),
					})
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
	coroName := coroRampName("goroutine", c.coroEnclosingQualifier(c.fn), c.goCounter) // T1222: qualify by enclosing to keep symbol unique across split units
	c.goCounter++

	var coroParams []*ir.Param
	for i := range argVals {
		coroParams = append(coroParams, ir.NewParam(fmt.Sprintf("arg.%d", i), argLLVMTypes[i]))
	}
	coroFn := c.module.NewFunc(coroName, irtypes.I8Ptr, coroParams...)
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, rawFuncAttr("presplitcoroutine"))
	c.attributeCoroToEnclosing(coroName, c.fn) // T1222: same split unit as spawner

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

		// T1098: drop borrow-param argument temporaries owned by this goroutine
		// frame. Promise uses panic-via-flag (not unwinding), so the call always
		// returns and every exit path runs this.
		bodyBlk = c.emitGoArgBorrowDrops(coroFn, startBlk, bodyBlk, argBorrowDrops)

		// T0147: Check panic flag after call — skip result store if panicked.
		dcFlag := bodyBlk.NewLoad(irtypes.I8, c.panicFlagGlobal)
		dcIsPanic := bodyBlk.NewICmp(enum.IPredNE, dcFlag, constant.NewInt(irtypes.I8, 0))
		dcOkBlk := coroFn.NewBlock("go.call_ok")
		bodyBlk.NewCondBr(dcIsPanic, goPanicExitDC, dcOkBlk)
		bodyBlk = dcOkBlk

		if c.goExprFireAndForget {
			// T1159: fire-and-forget non-void has no receiver and result_ptr is null
			// (the caller skips the buffer at the `!c.goExprFireAndForget` gate below).
			// Drop the discarded result here so a value-returning go-spawn doesn't leak.
			// emitVariantFieldDrop is the canonical drop-by-type (string/struct/vector/
			// closure/…), the same helper task drop uses on the result buffer. It operates
			// on c.block and may create new blocks (c.newBlock) / entry allocas
			// (c.entryBlock) — both must target coroFn's frame, not the outer caller's,
			// so swap c.fn/c.block/c.entryBlock around it (the fast path otherwise threads
			// blocks via the local bodyBlk without touching c.fn).
			savedFn, savedBlock, savedEntry := c.fn, c.block, c.entryBlock
			c.fn, c.block, c.entryBlock = coroFn, bodyBlk, startBlk
			c.emitVariantFieldDrop(result, callResultType)
			bodyBlk = c.block
			c.fn, c.block, c.entryBlock = savedFn, savedBlock, savedEntry
		} else {
			// Store result via G.result_ptr (set by caller before enqueue).
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
		}
	} else {
		bodyBlk.NewCall(targetFn, callArgs...)

		// T1098: drop borrow-param argument temporaries (see non-void branch).
		bodyBlk = c.emitGoArgBorrowDrops(coroFn, startBlk, bodyBlk, argBorrowDrops)

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
	captureNames, captureIdents := c.collectBlockIdents(syntheticBlock, c.locals)

	// 3. Load captured values in caller scope
	var captureVals []value.Value
	var captureLLVMTypes []irtypes.Type
	var thisSnapshot *goThisSnapshot // T1261: coro-side setup for a captured `this` (sibling of T1219's genGoBlock)
	for _, name := range captureNames {
		if name == "this" {
			// T1261: capture a private snapshot of the receiver — the coroutine can
			// outlive the spawning method's borrowed receiver temp, so it must never
			// alias `this` (else re-derefing `this.field` inside the coro is a UAF).
			// Mirrors genGoBlock's T1219 handling; runs in the enclosing method's
			// context (before the saveState/context switch), where c.locals["this"],
			// c.monoCtx, c.currentNamed and c.typeSubst are still valid.
			snapVal, snapType, snap := c.snapshotThisForGoBlock()
			captureVals = append(captureVals, snapVal)
			captureLLVMTypes = append(captureLLVMTypes, snapType)
			thisSnapshot = snap
			continue
		}
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

	// T1197: dup borrowed heap captured params (receiver + args) so the coroutine
	// owns private copies. The captures are read inside the coroutine, which may
	// run long after the spawning function returns and drops its borrowed-arg
	// stmt-temps — without a spawn-side dup those reads alias freed memory (UAF /
	// heap corruption). Borrowed value params carry no outer drop binding, so
	// B0354's ownership-transfer never covers them; mirror T0688's value-block dup.
	// Applies to void, non-void, and fire-and-forget alike — the async read of the
	// captures is independent of the result type.
	for idx, name := range captureNames {
		if !c.borrowedValueParams[name] {
			continue // only borrowed value params of the spawning function
		}
		if _, hasChan := capturedChanTypesVB[name]; hasChan {
			continue // refcounted channel — sharing the pointer is fine
		}
		if _, hasDrop := capturedDroppablesVB[name]; hasDrop {
			continue // owned local — B0354 already transfers ownership
		}
		ident := captureIdents[name]
		if ident == nil {
			continue // no representative ident (e.g. lambda-only capture)
		}
		capType := c.info.Types[ident]
		if c.typeSubst != nil && capType != nil {
			capType = types.Substitute(capType, c.typeSubst) // monomorphization
		}
		if !goElemNeedsBorrowedCaptureDup(capType) {
			continue // Copy / Arc / Task / value types don't alias caller heap
		}
		captureVals[idx] = c.dupBorrowedCaptureForResult(captureVals[idx], capType)
		capturedDroppablesVB[name] = capType // goroutine owns the dup → B0354 drop
	}

	// 4. Create coroutine function with captured values as parameters
	coroName := coroRampName("goroutine", c.coroEnclosingQualifier(c.fn), c.goCounter) // T1222: qualify by enclosing to keep symbol unique across split units
	c.goCounter++
	var coroParams []*ir.Param
	for i, name := range captureNames {
		coroParams = append(coroParams, ir.NewParam(name+".cap", captureLLVMTypes[i]))
	}
	coroFn := c.module.NewFunc(coroName, irtypes.I8Ptr, coroParams...)
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, rawFuncAttr("presplitcoroutine"))
	c.attributeCoroToEnclosing(coroName, c.fn) // T1222: same split unit as spawner

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
	savedCastSubjectMatch := c.castSubjectMatch // T0849: function-scoped, like dropFlags
	savedDropBindings := c.dropBindings         // B0035: must save/restore for NLL early drops
	savedLoopScopeDepth := c.loopScopeDepth
	savedInCoroutine := c.inCoroutine
	savedCoroCleanup := c.coroCleanupBlk
	savedCoroSuspend := c.coroSuspendBlk
	savedPanicExitBlock := c.panicExitBlock
	savedCoroutineReturnBlock := c.coroutineReturnBlock
	savedGoExprFF := c.goExprFireAndForget
	savedLocalNameCount := c.localNameCount           // T0261
	savedStmtTemps := c.stmtTemps                     // T0594: stmtTemps must not leak from coroutine body into outer function
	savedStmtTempMap := c.stmtTempMap                 // T0594: allocas created inside coroutine body live in a different function
	savedEnumCtorTemps := c.enumCtorTemps             // B0267: enumCtorTemps must not leak from coroutine body into outer function
	savedHeapTemps := c.heapTemps                     // T1105: isolate coro-body heap temps from the outer fn
	savedHeapTempMap := c.heapTempMap                 // T1105
	savedEnvTemps := c.envTemps                       // T1105: isolate coro-body closure env temps from the outer fn
	savedEnvTempMap := c.envTempMap                   // T1105
	savedBorrowedValueParams := c.borrowedValueParams // T0945
	c.fn = coroFn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.blockCounter = 0
	c.canError = false
	c.currentRetType = types.TypVoid
	c.borrowedValueParams = nil // T0945: coroutine body has no user value params
	c.scopeBindings = nil
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.castSubjectMatch = nil // T0849: fresh per function body; restored below
	c.dropBindings = make(map[string]scopeBinding)
	c.loopScopeDepth = 0
	c.inCoroutine = true
	c.stmtTemps = nil                         // T0594: fresh temp state for coroutine body
	c.stmtTempMap = make(map[value.Value]int) // T0594
	c.enumCtorTemps = nil                     // B0267
	c.heapTemps = nil                         // T1105: coro-body heap temps reference coroFn allocas
	c.heapTempMap = make(map[value.Value]int) // T1105
	c.envTemps = nil                          // T1105
	c.envTempMap = make(map[value.Value]int)  // T1105

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
	var thisValAlloca *ir.InstAlloca // T1261: value-struct alloca backing a captured `this`
	for i, name := range captureNames {
		if thisSnapshot != nil && name == "this" {
			// T1261: the captured param is a value struct (heap: {vtable,instance};
			// value type: the full value struct). Store it in a frame alloca, then
			// derive the i8* `this` that field access expects:
			//   heap type  → instance pointer (value struct field 1)
			//   value type → address of the value struct in the coroutine frame
			// The value-struct alloca backs the goroutine's private-copy drop.
			// Mirrors genGoBlock's T1219 site (expr.go).
			valAlloca := startBlk.NewAlloca(captureLLVMTypes[i])
			valAlloca.SetName(c.uniqueLocalName("this.val.addr"))
			startBlk.NewStore(coroFn.Params[i], valAlloca)

			i8Alloca := startBlk.NewAlloca(irtypes.I8Ptr)
			i8Alloca.SetName(c.uniqueLocalName("this.addr"))
			var thisI8 value.Value
			if thisSnapshot.isValueType {
				thisI8 = startBlk.NewBitCast(valAlloca, irtypes.I8Ptr)
			} else {
				thisI8 = startBlk.NewExtractValue(coroFn.Params[i], 1)
			}
			startBlk.NewStore(thisI8, i8Alloca)
			c.locals["this"] = i8Alloca
			thisValAlloca = valAlloca
			continue
		}
		alloca := startBlk.NewAlloca(captureLLVMTypes[i])
		alloca.SetName(c.uniqueLocalName(name + ".addr"))
		startBlk.NewStore(coroFn.Params[i], alloca)
		c.locals[name] = alloca
	}

	// B0163: Register drop bindings for captured channel variables inside the goroutine.
	c.entryBlock = startBlk
	c.block = startBlk

	// T1261: register the drop for the goroutine's private heap snapshot of `this`
	// so it is freed at coroutine scope exit (value-type snapshots are Copy — no
	// drop). Mirrors genGoBlock's T1219 site.
	if thisSnapshot != nil && !thisSnapshot.isValueType && thisValAlloca != nil {
		c.maybeRegisterDrop("this", thisValAlloca, thisSnapshot.resolvedType)
	}

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

	if !isVoid && result != nil && !savedGoExprFF {
		// T1159: only transfer+claim the result for a task handle. For fire-and-forget
		// the caller allocates no result buffer (G.result_ptr stays null below), so don't
		// store/claim — cleanupStmtTemps/cleanupHeapTemps/cleanupEnvTemps below drop the
		// discarded result instead. (Without this, the body would deref a null result_ptr
		// and the claim would suppress the very cleanup that should free the result.)
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
		c.claimHeapTemp(result) // T1105: heap-struct result moved into G.result_ptr
		c.claimEnvTemp(result)  // T1105: closure result moved into G.result_ptr
	}

	// T0594: Clean up any remaining stmtTemps from the coroutine body before restoring
	// the outer function's context. Without this, temps created by genExpr (e.g., string
	// return values tracked by trackStringTemp) would be orphaned inside the coroutine.
	c.cleanupStmtTemps()
	c.cleanupHeapTemps() // T1105: drop orphaned trailing-expr heap intermediates inside the coro
	c.cleanupEnvTemps()  // T1105

	// T1156: Drop inline enum-ctor temps produced for the call's arguments inside
	// the coro body (via-block analogue of stmt.go's statement-end enum cleanup).
	// Borrow params keep the flag set → dropped here exactly once; move params had
	// the flag cleared by normal call codegen → skipped (callee consumes them).
	if c.block != nil && c.block.Term == nil {
		for _, et := range c.enumCtorTemps {
			flag := c.block.NewLoad(irtypes.I1, et.dropFlag)
			dropBlk := c.newBlock("enum.ctor.drop")
			skipBlk := c.newBlock("enum.ctor.skip")
			c.block.NewCondBr(flag, dropBlk, skipBlk)
			c.block = dropBlk
			ptr := c.block.NewLoad(irtypes.I8Ptr, et.alloca)
			c.block.NewCall(et.dropFunc, ptr)
			c.block.NewBr(skipBlk)
			c.block = skipBlk
		}
		c.enumCtorTemps = c.enumCtorTemps[:0]
	}

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
	c.castSubjectMatch = savedCastSubjectMatch // T0849
	c.dropBindings = savedDropBindings         // B0035: restore for NLL early drops
	c.loopScopeDepth = savedLoopScopeDepth
	c.inCoroutine = savedInCoroutine
	c.coroCleanupBlk = savedCoroCleanup
	c.coroSuspendBlk = savedCoroSuspend
	c.panicExitBlock = savedPanicExitBlock
	c.coroutineReturnBlock = savedCoroutineReturnBlock
	c.goExprFireAndForget = savedGoExprFF
	c.borrowedValueParams = savedBorrowedValueParams // T0945
	c.localNameCount = savedLocalNameCount           // T0261
	c.stmtTemps = savedStmtTemps                     // T0594: restore outer function's temp state
	c.stmtTempMap = savedStmtTempMap                 // T0594
	c.enumCtorTemps = savedEnumCtorTemps             // B0267
	c.heapTemps = savedHeapTemps                     // T1105
	c.heapTempMap = savedHeapTempMap                 // T1105
	c.envTemps = savedEnvTemps                       // T1105
	c.envTempMap = savedEnvTempMap                   // T1105

	// B0354: Clear outer drop flags for captured droppable non-channel variables.
	for name := range capturedDroppablesVB {
		c.clearDropFlag(name)
	}

	// 9. Caller: call coroutine ramp → get handle, create G, enqueue
	handle := c.block.NewCall(coroFn, captureVals...)
	gRaw := c.block.NewCall(c.funcs["promise_g_new"], handle)

	if !savedGoExprFF { // T1159: fire-and-forget allocates no result buffer (mirrors the fast path's `!c.goExprFireAndForget` gate in genGoCallExpr)
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
	// T1222: the go-block ramp is attributed to (and lands in) the same split unit
	// as its enclosing function. The ramp calls this wrapper, so the wrapper must be
	// co-located too — otherwise a `go extern()` inside a generic instance method
	// leaves the wrapper orphaned in main IR while the ramp goes to the instance
	// `.bc` → cross-program undefined-symbol at link. c.fn is still the enclosing
	// function here (genGoExternWrapper is called before the ramp is built and before
	// saveState switches c.fn), so qualify + attribute exactly like coroRampName.
	enclQual := c.coroEnclosingQualifier(c.fn)
	var wrapName string
	if enclQual == "" {
		wrapName = fmt.Sprintf(".go_extern_wrap.%s.%d", ext.PromiseName, c.goCounter)
	} else {
		wrapName = fmt.Sprintf(".go_extern_wrap.%s.%s.%d", ext.PromiseName, enclQual, c.goCounter)
	}

	var params []*ir.Param
	for i, ty := range argLLVMTypes {
		params = append(params, ir.NewParam(fmt.Sprintf("arg.%d", i), ty))
	}

	retType := irtypes.Type(irtypes.Void)
	if !isVoid {
		retType = resultLLVM
	}
	wrapFn := c.module.NewFunc(wrapName, retType, params...)
	c.attributeCoroToEnclosing(wrapName, c.fn) // T1222: same split unit as the ramp that calls it

	saved := c.saveState()
	defer c.restoreState(saved)

	c.fn = wrapFn
	entry := wrapFn.NewBlock("entry")
	c.block = entry
	c.entryBlock = entry
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.castSubjectMatch = nil // T0849: fresh per function body (saved/restored via saveState)
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
// Returns a sorted, deduplicated list of names that exist in outerLocals, plus a
// map from each captured name to the first *ast.IdentExpr seen for it (T0731:
// used to resolve the capture's sema type via c.info.Types for the spawn-side
// borrowed-heap-param dup). Names collected only through a LambdaExpr capture set
// (which has no representative IdentExpr) are absent from the ident map.
func (c *Compiler) collectBlockIdents(block *ast.Block, outerLocals map[string]*ir.InstAlloca) ([]string, map[string]*ast.IdentExpr) {
	seen := make(map[string]bool)
	idents := make(map[string]*ast.IdentExpr)
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
				if _, recorded := idents[e.Name]; !recorded {
					idents[e.Name] = e
				}
			}
		case *ast.ThisExpr:
			// T1219: `this` referenced inside a `go { }` block within a method.
			// The block is compiled into a separate coroutine that does not
			// inherit the method's `this`; thread the receiver into the arg pack
			// (marked via the "this" outer local) so genThisExpr resolves it.
			// No representative IdentExpr is recorded — genGoBlock builds the
			// capture value directly (a private snapshot, not the live receiver).
			if _, ok := outerLocals["this"]; ok {
				seen["this"] = true
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
			// T0740: a lambda inside a `go { }` block is compiled into a *separate*
			// coroutine function, so any outer-function local the lambda captures
			// must first be passed into the coroutine arg pack — otherwise
			// genLambdaExpr finds no alloca for the name and zero-initializes the
			// capture. Sema already computed the lambda's capture set (transitively
			// including nested-lambda captures via checkLambdaExpr's propagation);
			// collect those whose name is an outer local. We do NOT recurse into the
			// lambda body: sema's no-shadow rule guarantees bound names never alias
			// an outerLocals name, and block-locals are excluded by the outerLocals
			// filter (they are already in scope inside the coroutine).
			for _, cv := range c.info.LambdaCaptures[e] {
				name := cv.Obj.Name()
				if _, ok := outerLocals[name]; ok {
					seen[name] = true
				}
			}
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
	return names, idents
}

// goElemNeedsBorrowedCaptureDup reports whether a value-block trailing value
// whose source is a borrowed captured parameter (no outer drop binding) must
// be dup'd before being stored into G.result_ptr. Without the dup, the loaded
// pointer aliases the caller's stmt-temp; the caller drops the temp
// immediately after spawning the goroutine, so the awaiter would load freed
// memory and the receiver's owned drop would double-free (T0688).
// Eligible heap types: string, Vector, droppable/no-drop heap user types.
// Excluded: Copy types (int/bool/...), Channel/Arc/Weak (refcounted — share
// is fine), Task/Mutex/MutexGuard (single-owner handles with no dup
// semantics), value types (embedded data, no heap aliasing).
func goElemNeedsBorrowedCaptureDup(goElem types.Type) bool {
	if goElem == nil {
		return false
	}
	named := extractNamed(goElem)
	if named == types.TypString {
		return true
	}
	if _, ok := types.AsVector(goElem); ok || named == types.TypVector {
		return true
	}
	// T0732: Map[K,V] and Set[T] are heap user types but are excluded from
	// isDroppableHeapUserType / isHeapUserNoDropPalFree by T0440's early
	// returns. That gating targets the dup-on-read clone() path
	// (cloneHeapElement), whose Promise-level clone() can be shallow for
	// nested-heap value args. The spawn-side T0688 dup does NOT use clone() —
	// it uses dupHeapValue (memcpy + field-wise deep dup), which correctly
	// deep-copies Map (vector of Slot enums via dupEnumElementInPlace) and Set
	// (recursive Map dup). So they ARE eligible here.
	if isMapOrSetType(goElem) {
		return true
	}
	if isDroppableHeapUserType(goElem) || isHeapUserNoDropPalFree(goElem) {
		return true
	}
	return false
}

// dupBorrowedCaptureForResult emits a dup of a borrowed-captured trailing
// value for the value-block path of `go { v }` (T0688). Dispatches by element
// type and uses c.block for IR emission (the dup helpers update c.block to a
// post-dup merge block, which the caller uses for the subsequent store).
// Vector[T] uses dupVector + emitVectorElementCloneLoop for a deep copy
// (heap element types like string would otherwise alias the original).
func (c *Compiler) dupBorrowedCaptureForResult(val value.Value, goElem types.Type) value.Value {
	named := extractNamed(goElem)
	switch {
	case named == types.TypString:
		return c.dupString(val)
	case types.IsVector(goElem):
		// extractNamed returns TypVector for both Vector[T] Instance AND bare
		// TypVector — check IsVector first so we use the right element size.
		vecElem, _ := types.AsVector(goElem)
		elemSize := int64(c.typeSize(c.resolveType(vecElem)))
		dup := c.dupVector(val, elemSize)
		c.emitVectorElementCloneLoop(dup, vecElem)
		return dup
	case named == types.TypVector:
		// Bare TypVector (element type unknown — rarely reached here since
		// `Vector` without type args is usually rejected by sema, but mirrors
		// the existing pattern in compiler.go).
		return c.dupVector(val, 0)
	case isMapOrSetType(goElem):
		// T0732: Map/Set deep-copy via dupHeapValue (memcpy + field-wise dup),
		// NOT the Promise clone() method. dupHeapValue walks the instance:
		// Map's _buckets vector clones each Slot enum element (key+value) via
		// dupEnumElementInPlace; Set's _map field recurses through dupHeapValue.
		return c.dupHeapValue(val, goElem)
	case isDroppableHeapUserType(goElem) || isHeapUserNoDropPalFree(goElem):
		return c.dupHeapValue(val, goElem)
	}
	return val
}

// goThisSnapshot records how a `this` captured by a `go { }` block (T1219) is
// set up on the coroutine side. nil means the default capture path (a plain
// scalar load — primitive-scalar receivers are Copy) is already safe.
type goThisSnapshot struct {
	isValueType  bool       // value-type receiver → copy value struct, no drop
	resolvedType types.Type // concrete receiver type (drop dispatch, heap only)
}

// snapshotThisForGoBlock builds a private snapshot of the method receiver for a
// `go { }` block capture (T1219). A goroutine can outlive the receiver's owner,
// so the coroutine must never alias `this`: heap receivers are deep-copied
// (dupHeapValue, like the T1196 borrowed-param dup) and owned+dropped by the
// goroutine; value-type receivers are copied by value into the coroutine frame
// (Copy, no heap, no drop); primitive-scalar receivers are already Copy so the
// default load suffices. Returns the capture value, its LLVM type, and the
// coroutine-side setup metadata (nil for the primitive-scalar / default path).
// Runs in the enclosing method's codegen context (before the coroutine switch).
func (c *Compiler) snapshotThisForGoBlock() (value.Value, irtypes.Type, *goThisSnapshot) {
	thisAlloca := c.locals["this"]
	resolvedType := c.currentNamedType()
	named := extractNamed(resolvedType)

	// Primitive-scalar receiver: `this` is the scalar value itself (Copy) — the
	// default load is a safe self-contained snapshot, no coroutine-side setup.
	if named == nil || isPrimitiveScalar(named) {
		val := c.block.NewLoad(thisAlloca.ElemType, thisAlloca)
		return val, thisAlloca.ElemType, nil
	}

	layout := c.lookupTypeLayout(resolvedType)
	if layout == nil || layout.Value == nil {
		panic(fmt.Sprintf("codegen: no layout for `this` receiver %s in go block", resolvedType))
	}

	if named.IsValueType() {
		// Value type: `this` is an i8* to the caller's value struct on the method
		// frame — copy it by value into the coroutine frame (Copy, no drop).
		thisI8 := c.block.NewLoad(thisAlloca.ElemType, thisAlloca)
		vsType := layout.Value.LLVMType
		typedPtr := c.block.NewBitCast(thisI8, irtypes.NewPointer(vsType))
		vstruct := c.block.NewLoad(vsType, typedPtr)
		return vstruct, vsType, &goThisSnapshot{isValueType: true, resolvedType: resolvedType}
	}

	// Heap type: `this` is an i8* instance pointer. Reconstruct the
	// `{vtable, instance}` value struct and deep-copy it (dupHeapValue), so the
	// goroutine owns a private instance it drops at scope exit.
	thisI8 := c.block.NewLoad(thisAlloca.ElemType, thisAlloca)
	vtable := c.loadVtablePtrFromInstance(thisI8)
	vsType := irtypes.NewStruct(irtypes.I8Ptr, irtypes.I8Ptr)
	vstruct := c.block.NewInsertValue(constant.NewZeroInitializer(vsType), vtable, 0)
	vstruct = c.block.NewInsertValue(vstruct, thisI8, 1)
	dupVS := c.dupHeapValue(vstruct, resolvedType)
	return dupVS, vsType, &goThisSnapshot{isValueType: false, resolvedType: resolvedType}
}

// genGoBlock handles `go { block }` — wraps the block in a void function and spawns it.
// Captures outer local variables referenced in the block and passes them through the arg pack.
func (c *Compiler) genGoBlock(e *ast.GoExpr) value.Value {
	block := e.Block

	// T0683: Resolve the task's result type. sema types `go { …; <expr> }`
	// as task[T] where T is the trailing ExprStmt's type; a void block is
	// task[void]. Substitute typeSubst so the buffer/store type matches the
	// `<-task` receive side under monomorphization (symmetric with
	// genReceiveTask). A value-returning block must store its trailing value
	// into G.result_ptr; the void path is unchanged.
	goResultType := c.info.Types[e]
	if c.typeSubst != nil && goResultType != nil {
		goResultType = types.Substitute(goResultType, c.typeSubst)
	}
	goElem, goHasElem := types.AsTask(goResultType)
	goIsVoid := !goHasElem || goElem == nil || goElem == types.TypVoid
	var goResultLLVM irtypes.Type = irtypes.Void
	if !goIsVoid {
		goResultLLVM = c.resolveType(goElem)
	}

	// Collect outer variables referenced in the block
	captureNames, captureIdents := c.collectBlockIdents(block, c.locals)

	// Load captured values and collect their types BEFORE switching context
	var captureVals []value.Value
	var captureLLVMTypes []irtypes.Type
	var thisSnapshot *goThisSnapshot // T1219: coro-side setup for a captured `this`
	for _, name := range captureNames {
		if name == "this" {
			// T1219: capture a private snapshot of the receiver — a goroutine can
			// outlive the receiver's owner, so it must never alias `this`.
			snapVal, snapType, snap := c.snapshotThisForGoBlock()
			captureVals = append(captureVals, snapVal)
			captureLLVMTypes = append(captureLLVMTypes, snapType)
			thisSnapshot = snap
			continue
		}
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

	// T0731 (generalizes T0688), T1196 (extends to void/fire-and-forget):
	// reading a BORROWED heap captured parameter inside an asynchronously-
	// scheduled coroutine is NEVER safe — the caller may free the underlying
	// buffer (e.g. a `"a"+"b"` stmt-temp) before the coroutine ever runs. T0688
	// only dup'd when the value-block's trailing expression was a bare ident
	// naming the param; but the same UAF/double-free triggers when the param is
	// aliased through a goroutine-local first (`s := if b { v } else { … }; s`),
	// derived via an expression (`v + "!"`), or read incidentally. Rather than
	// trace the block dataflow, we conservatively dup EVERY borrowed heap
	// captured param: the goroutine then owns a private deep copy regardless of
	// how the body routes the param. This applies to ALL go-block forms — the
	// awaited value-block path (T0731) AND the void / fire-and-forget path
	// (T1196), where a body that hands a param-derived value off asynchronously
	// (e.g. `go { out.send(v + "!"); }`) reads freed memory just the same. Each
	// dup is added to capturedDroppables so the existing B0354 ownership-
	// transfer machinery registers a goroutine-side drop binding at depth 0
	// (alongside the captured-channel drops). On BOTH the value and void /
	// fire-and-forget paths, a non-escaping copy is freed at coroutine scope
	// exit by the depth-0 emitScopeCleanup below (the same site that frees
	// captured channels); an escaping copy (e.g. the value-block trailing
	// value moved into G.result_ptr) has its drop flag cleared at the move
	// site, so that cleanup skips it — no leak, no double-free either way.
	// The outer clearDropFlag below is a harmless no-op (borrowed
	// params have no outer drop flag). Cost: one deep copy per borrowed heap
	// capture even when unused — negligible versus the soundness guarantee.
	// Excluded: channel captures (refcounted share), owned locals (already in
	// capturedDroppables — B0354 handles them), and Copy/value/Arc/Task types
	// (goElemNeedsBorrowedCaptureDup returns false).
	for idx, name := range captureNames {
		if c.borrowedValueParams == nil || !c.borrowedValueParams[name] {
			continue // not a borrowed value param (owned local / non-param)
		}
		if _, hasChan := capturedChanTypes[name]; hasChan {
			continue // channel: shared via refcount, no dup
		}
		if _, hasDroppable := capturedDroppables[name]; hasDroppable {
			continue // owned local: B0354 already transfers ownership
		}
		capType := c.info.Types[captureIdents[name]]
		if c.typeSubst != nil && capType != nil {
			capType = types.Substitute(capType, c.typeSubst) // mirror goResultType
		}
		if !goElemNeedsBorrowedCaptureDup(capType) {
			continue // Copy/channel/Arc/Task/value type — no heap aliasing
		}
		captureVals[idx] = c.dupBorrowedCaptureForResult(captureVals[idx], capType)
		capturedDroppables[name] = capType // goroutine now owns it → B0354 drop
	}

	// Create coroutine function with captured values as parameters
	coroName := coroRampName("goroutine", c.coroEnclosingQualifier(c.fn), c.goCounter) // T1222: qualify by enclosing to keep symbol unique across split units
	c.goCounter++
	var coroParams []*ir.Param
	for i, name := range captureNames {
		coroParams = append(coroParams, ir.NewParam(name+".cap", captureLLVMTypes[i]))
	}
	coroFn := c.module.NewFunc(coroName, irtypes.I8Ptr, coroParams...)
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, rawFuncAttr("presplitcoroutine"))
	c.attributeCoroToEnclosing(coroName, c.fn) // T1222: same split unit as spawner

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
	savedCastSubjectMatch := c.castSubjectMatch // T0849: function-scoped, like dropFlags
	savedDropBindings := c.dropBindings         // B0035: must save/restore for NLL early drops
	savedLoopScopeDepth := c.loopScopeDepth
	savedInCoroutine := c.inCoroutine
	savedCoroCleanup := c.coroCleanupBlk
	savedCoroSuspend := c.coroSuspendBlk
	savedPanicExitBlock := c.panicExitBlock
	savedCoroutineReturnBlock := c.coroutineReturnBlock
	savedGoExprFF := c.goExprFireAndForget
	savedLocalNameCount := c.localNameCount           // T0261
	savedEnumCtorTemps := c.enumCtorTemps             // B0267
	savedStmtTemps := c.stmtTemps                     // T0683/T0594: isolate coro-body temps from the outer fn
	savedStmtTempMap := c.stmtTempMap                 // T0683/T0594
	savedHeapTemps := c.heapTemps                     // T0686: isolate coro-body heap temps from the outer fn
	savedHeapTempMap := c.heapTempMap                 // T0686
	savedEnvTemps := c.envTemps                       // T0739: isolate coro-body closure env temps from the outer fn
	savedEnvTempMap := c.envTempMap                   // T0739
	savedBorrowedValueParams := c.borrowedValueParams // T0945
	savedDiscardedExpr := c.discardedExpr             // T1029: coro body is not the discarded statement
	savedDiscardAliasArgPtrs := c.discardAliasArgPtrs // T1029
	c.goExprFireAndForget = false                     // reset for inner statements (B0109)
	c.discardedExpr = nil                             // T1029: inner ExprStmts set their own
	c.discardAliasArgPtrs = nil                       // T1029

	// T0683: Only a non-void, awaited (`<-task`) block needs its trailing
	// value stored into G.result_ptr. A void block has no value; a
	// fire-and-forget value block (`go { 42 };` as a bare statement) has its
	// value discarded — both take the unchanged void genBlock path, whose
	// per-statement temp cleanup frees a heap trailing value (no leak), and
	// the caller leaves result_ptr null/sentinel as before. This keeps the
	// void path byte-for-byte identical to pre-T0683.
	useGoBlockValuePath := !goIsVoid && !savedGoExprFF

	c.fn = coroFn
	c.locals = make(map[string]*ir.InstAlloca)
	c.localNameCount = make(map[string]int)
	c.blockCounter = 0
	c.canError = false
	c.currentRetType = types.TypVoid
	c.scopeBindings = nil
	c.dropFlags = make(map[string]*ir.InstAlloca)
	c.castSubjectMatch = nil // T0849: fresh per function body; restored below
	c.dropBindings = make(map[string]scopeBinding)
	c.loopScopeDepth = 0
	c.inCoroutine = true
	c.enumCtorTemps = nil       // B0267
	c.borrowedValueParams = nil // T0945: coroutine body has no user value params
	if useGoBlockValuePath {
		// T0683/T0594: fresh temp state for the coroutine body so its temps
		// (which reference coroFn allocas) cannot leak into the outer fn.
		c.stmtTemps = nil
		c.stmtTempMap = make(map[value.Value]int)
		// T0686: same isolation for heap-instance temps — genBlockValue does
		// not save/restore heapTemps the way genBlock does (T0088), so a
		// heap-struct trailing value (e.g. `go { Box(...) }`) would otherwise
		// leak its coroFn alloca/dropFlag into the outer fn and serialize as
		// `%0` (the coro.id token) in the outer cleanupHeapTemps.
		c.heapTemps = nil
		c.heapTempMap = make(map[value.Value]int)
		// T0739: same isolation for closure env temps — a capturing-closure
		// trailing value (e.g. `go { || -> base + 2 }`) would otherwise leak
		// its coroFn env alloca/dropFlag into the outer fn and serialize as
		// `%0` (the coro.id token) in the outer cleanupEnvTemps.
		c.envTemps = nil
		c.envTempMap = make(map[value.Value]int)
	}

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
	var thisValAlloca *ir.InstAlloca // T1219: value-struct alloca backing a captured `this`
	for i, name := range captureNames {
		if thisSnapshot != nil && name == "this" {
			// T1219: the captured param is a value struct (heap: {vtable,instance};
			// value type: the full value struct). Store it in a frame alloca, then
			// derive the i8* `this` that genThisExpr/field access expect:
			//   heap type  → instance pointer (value struct field 1)
			//   value type → address of the value struct in the coroutine frame
			// The value-struct alloca backs the goroutine's private-copy drop.
			valAlloca := startBlk.NewAlloca(captureLLVMTypes[i])
			valAlloca.SetName(c.uniqueLocalName("this.val.addr"))
			startBlk.NewStore(coroFn.Params[i], valAlloca)

			i8Alloca := startBlk.NewAlloca(irtypes.I8Ptr)
			i8Alloca.SetName(c.uniqueLocalName("this.addr"))
			var thisI8 value.Value
			if thisSnapshot.isValueType {
				thisI8 = startBlk.NewBitCast(valAlloca, irtypes.I8Ptr)
			} else {
				thisI8 = startBlk.NewExtractValue(coroFn.Params[i], 1)
			}
			startBlk.NewStore(thisI8, i8Alloca)
			c.locals["this"] = i8Alloca
			thisValAlloca = valAlloca
			continue
		}
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

	// T1219: register the drop for the goroutine's private heap snapshot of `this`
	// so it is freed at coroutine scope exit (value-type snapshots are Copy — no
	// drop). Mirrors the borrowed-capture dup ownership transfer (T1196/B0354).
	if thisSnapshot != nil && !thisSnapshot.isValueType && thisValAlloca != nil {
		c.maybeRegisterDrop("this", thisValAlloca, thisSnapshot.resolvedType)
	}

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

	if !useGoBlockValuePath {
		// Void or fire-and-forget: discard the trailing value. genBlock's
		// per-statement genStmt cleanup frees a heap trailing value, so a
		// fire-and-forget value block (`go { "x"+"y" };`) does not leak.
		c.genBlock(block)
	} else {
		// T0683: non-void awaited block — capture the trailing-expression
		// value and store it into the caller-allocated G.result_ptr buffer.
		// genBlockValue claims `result` and drops block locals after, so the
		// value is safe to store here.
		result := c.genBlockValue(block)
		if result != nil && c.block != nil && c.block.Term == nil {
			// B0109 null-check store pattern (mirrors genGoCallExpr): the
			// caller allocates result_ptr for an awaited task. The null
			// check is defensive — symmetric with the working call form.
			gTy := goroutineStructType()
			currentG := c.block.NewLoad(irtypes.I8Ptr, c.currentGGlobal)
			gPtr := c.block.NewBitCast(currentG, irtypes.NewPointer(gTy))
			rpField := c.block.NewGetElementPtr(gTy, gPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
			rpVal := c.block.NewLoad(irtypes.I8Ptr, rpField)
			rpNotNull := c.block.NewICmp(enum.IPredNE, rpVal, constant.NewNull(irtypes.I8Ptr))
			storeResultBlk := coroFn.NewBlock("store_result")
			afterStoreBlk := coroFn.NewBlock("after_store")
			c.block.NewCondBr(rpNotNull, storeResultBlk, afterStoreBlk)

			typedRP := storeResultBlk.NewBitCast(rpVal, irtypes.NewPointer(goResultLLVM))
			storeResultBlk.NewStore(result, typedRP)
			storeResultBlk.NewBr(afterStoreBlk)

			c.block = afterStoreBlk
		}
		// T0594: ownership of `result` transferred to G.result_ptr — claim
		// so the coroutine body's stmt-temp cleanup doesn't free it, then
		// drop any orphaned temps from the trailing expression.
		// claimStringTemp emits a flag store, so skip it on a dead block
		// (e.g. trailing expr panicked); cleanupStmtTemps self-guards.
		if c.block != nil && c.block.Term == nil {
			c.claimStringTemp(result)
			// T0686: a heap-struct result is moved into G.result_ptr — claim it
			// so the coroutine body's heap-temp cleanup below doesn't free it.
			c.claimHeapTemp(result)
			// T0739: a capturing-closure result is moved into G.result_ptr —
			// claim its env temp so the coroutine body's env-temp cleanup below
			// doesn't free it (claimEnvTemp extracts field 1 of the fat pointer).
			c.claimEnvTemp(result)
		}
		c.cleanupStmtTemps()
		// T0686: drop any orphaned trailing-expr heap intermediates inside the
		// coroutine (self-guards on a dead block).
		c.cleanupHeapTemps()
		// T0739: drop any orphaned trailing-expr closure env intermediates
		// inside the coroutine (self-guards on a dead block).
		c.cleanupEnvTemps()
	}

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
	c.castSubjectMatch = savedCastSubjectMatch // T0849
	c.dropBindings = savedDropBindings         // B0035: restore for NLL early drops
	c.loopScopeDepth = savedLoopScopeDepth
	c.inCoroutine = savedInCoroutine
	c.coroCleanupBlk = savedCoroCleanup
	c.coroSuspendBlk = savedCoroSuspend
	c.panicExitBlock = savedPanicExitBlock
	c.coroutineReturnBlock = savedCoroutineReturnBlock
	c.goExprFireAndForget = savedGoExprFF
	c.borrowedValueParams = savedBorrowedValueParams // T0945
	c.discardedExpr = savedDiscardedExpr             // T1029
	c.discardAliasArgPtrs = savedDiscardAliasArgPtrs // T1029
	c.localNameCount = savedLocalNameCount           // T0261
	c.enumCtorTemps = savedEnumCtorTemps             // B0267
	if useGoBlockValuePath {
		c.stmtTemps = savedStmtTemps     // T0683/T0594: restore outer fn temp state
		c.stmtTempMap = savedStmtTempMap // T0683/T0594
		c.heapTemps = savedHeapTemps     // T0686: restore outer fn heap temp state
		c.heapTempMap = savedHeapTempMap // T0686
		c.envTemps = savedEnvTemps       // T0739: restore outer fn closure env temp state
		c.envTempMap = savedEnvTempMap   // T0739
	}

	// B0354: Clear outer drop flags for captured droppable non-channel variables.
	// Ownership has been transferred to the goroutine.
	for name := range capturedDroppables {
		c.clearDropFlag(name)
	}

	// Caller: call coroutine ramp → get handle, create G, enqueue
	handle := c.block.NewCall(coroFn, captureVals...)
	gRaw := c.block.NewCall(c.funcs["promise_g_new"], handle)

	if !c.goExprFireAndForget {
		gTy := goroutineStructType()
		gPtr := c.block.NewBitCast(gRaw, irtypes.NewPointer(gTy))
		rpField := c.block.NewGetElementPtr(gTy, gPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(gFieldResultPtr)))
		if goIsVoid {
			// Void task: set result_ptr to sentinel (0x1) so goroutine_exit
			// knows the receiver will free G (via <-task). Without this,
			// goroutine_exit would free the G and the receiver would access
			// freed memory.
			sentinel := c.block.NewIntToPtr(constant.NewInt(c.ptrIntType(), 1), irtypes.I8Ptr)
			c.block.NewStore(sentinel, rpField)
		} else {
			// T0683: non-void task — allocate the result buffer the
			// coroutine body stores into and the <-task receiver loads +
			// frees (mirrors genGoCallExpr). result_ptr != null also tells
			// goroutine_exit not to free G (the receiver owns it).
			resultSize := constant.NewInt(irtypes.I64, int64(c.typeSize(goResultLLVM)))
			resultBuf := c.block.NewCall(c.palAlloc, resultSize)
			c.block.NewStore(resultBuf, rpField)
		}
	}
	// Fire-and-forget (void or non-void): result_ptr stays null (from
	// promise_g_new), so goroutine_exit frees the G struct when the
	// goroutine completes. The non-void value is discarded by the void
	// genBlock path above (no buffer, no leak).

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

// unwrapTaskOptionalSource peels a `<-` operand of the shape `(src!)` or
// `(src? _ { ... })` down to the underlying force-unwrap source expression,
// returning nil for any other shape. The source may be a bare `*ast.IdentExpr`
// (owned-local optional Task, T0956) or a `*ast.MemberExpr` field access
// (optional Task field, T0806). genReceiveTask routes the result through
// neutralizeOptionalCastSource to clear the source optional's present flag after
// the receive consumes (frees) the task handle extracted from the optional.
func unwrapTaskOptionalSource(expr ast.Expr) ast.Expr {
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	var inner ast.Expr
	switch e := expr.(type) {
	case *ast.OptionalUnwrapExpr:
		inner = e.Expr
	case *ast.ErrorHandlerExpr:
		inner = e.Expr
	default:
		return nil
	}
	for {
		p, ok := inner.(*ast.ParenExpr)
		if !ok {
			break
		}
		inner = p.Expr
	}
	switch inner.(type) {
	case *ast.IdentExpr, *ast.MemberExpr:
		return inner
	}
	return nil
}

// genReceiveTask generates code for `<-task` — waits for goroutine G to complete, returns T.
// The task handle is now a G pointer (i8*). Checks G.done and loads from G.result_ptr.
func (c *Compiler) genReceiveTask(e *ast.UnaryExpr, inst *types.Instance) value.Value {
	// T0954: `<-(a ?: b)` — when the await operand (peeling parens) is an inline
	// elvis, signal genElvis so it neutralizes the none-path default's owner on
	// the none block (path-conditionally). `<-` binds tighter than `?:`, so an
	// awaited elvis is always written `<-(a ?: b)` — the paren peel is required.
	prevElvisConsumed := c.elvisResultConsumed
	if be, ok := unwrapDestructureParens(e.Operand).(*ast.BinaryExpr); ok && be.Op == ast.BinElvis {
		c.elvisResultConsumed = true
	}
	gRaw := c.genExpr(e.Operand)
	c.elvisResultConsumed = prevElvisConsumed
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
	// T0806/T0956: `<-(o!)` / `<-(o? _ { ... })` consumes the task extracted from
	// a force-unwrapped optional. The receive frees the G below; without clearing
	// the source optional's present flag, the source's scope-exit optional drop
	// reloads and joins+frees the same G → double-free → segfault.
	// neutralizeOptionalCastSource handles both shapes: the owned-local bare-ident
	// `o!` (T0956 — clears the local's optional present flag, field 0) and the
	// optional member field `h.tsk!` (T0806 — delegates to
	// neutralizeMemberOptionalField, which carries the Mutex/Task carve-out). Only
	// Task optionals reach genReceiveTask, so the unconditional ident-flag clear is
	// always correct here. Borrowed-source variants are already rejected at
	// ownership (T0953), so this codegen path only sees owned sources.
	if inner := unwrapTaskOptionalSource(e.Operand); inner != nil {
		c.neutralizeOptionalCastSource(inner)
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

	// T0680 Part 2: set by the WASM non-coroutine spin below when coop_step
	// reports a per-test deadline (stepR==2). When non-nil the result is produced
	// by a phi merging the normal load path with a zeroinitializer from this
	// timeout block (see the merge after loadResultBlk).
	var taskTimeoutBlk *ir.Block

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
		// T0668: shared cooperative park-suspend emitter — re-check G.done
		// under sched.done_lock, park the current G on the target G's
		// done_waiters (woken by promise_goroutine_exit), hold the lock across
		// coro.suspend via G.park_mutex (scheduler releases it after suspend —
		// prevents the enqueue-before-suspend race). The un-awaited Task-drop
		// join (emitTaskJoinAndFree) uses the same emitter, so the
		// receive-join and the drop-join cannot diverge.
		c.emitCoroTaskParkSuspendWait(gPtr, doneField, readyBlk)
	} else {
		// Thread-blocking mode: poll G.done in a loop.
		// goroutine_exit sets G.done = 1 atomically; we just spin until we see it.
		// On host: a brief usleep(100) avoids burning CPU in a tight loop.
		// T0687: On WASM (single-threaded cooperative scheduler), pal_usleep is a
		// no-op — the pending `go {…}` G sits in P0's run queue and never runs,
		// causing a permanent deadlock. Mirror the T0668 fix to Task[T].drop:
		// pump promise_sched_coop_step() instead, and terminate genuine deadlocks
		// (no runnable G AND target G still not done) with the shared message.
		checkBlk := c.newBlock("task.check")
		spinBlk := c.newBlock("task.spin")
		doneBlk := c.newBlock("task.threaddone")

		c.block.NewBr(checkBlk)

		// check: reload done flag (atomic acquire on WASM — T0669 parity:
		// prevents LLVM from hoisting the load above promise_sched_coop_step
		// and converging the spin into an infinite loop).
		c.block = checkBlk
		doneLoad2 := c.block.NewLoad(irtypes.I8, doneField)
		if c.isWasm {
			doneLoad2.Atomic = true
			doneLoad2.Ordering = enum.AtomicOrderingAcquire
			doneLoad2.Align = 1
		}
		isDone2 := c.block.NewICmp(enum.IPredNE, doneLoad2, constant.NewInt(irtypes.I8, 0))
		c.block.NewCondBr(isDone2, doneBlk, spinBlk)

		c.block = spinBlk
		if c.isWasm {
			// T0687: pump the cooperative scheduler one step. Returns i8:
			// non-zero = ran/advanced a G (progress possible), 0 = no runnable G.
			stepFn := c.funcs["promise_sched_coop_step"]
			stepR := c.block.NewCall(stepFn)
			coopRecheckBlk := c.newBlock("task.coop_recheck")
			deadlockBlk := c.newBlock("task.deadlock")
			spinProgressBlk := c.newBlock("task.progress")
			// T0680 Part 2: stepR==2 = per-test deadline reached. Break the spin
			// and yield a dead zeroinitializer result (the test is being torn down
			// and its return is discarded; G is intentionally not freed — a leak,
			// but result==2 skips the leak check). Prevents a livelock nested under
			// this await from spinning coop_step→2 forever.
			taskTimeoutBlk = c.newBlock("task.timed_out")
			isTimeout := c.block.NewICmp(enum.IPredEQ, stepR, constant.NewInt(irtypes.I8, 2))
			c.block.NewCondBr(isTimeout, taskTimeoutBlk, spinProgressBlk)

			c.block = spinProgressBlk
			madeProgress := c.block.NewICmp(enum.IPredNE, stepR, constant.NewInt(irtypes.I8, 0))
			c.block.NewCondBr(madeProgress, checkBlk, coopRecheckBlk)

			// No runnable G — re-check G.done. If the awaited G is still not
			// done it can never complete (nothing left to run) → genuine deadlock.
			rdLoad := coopRecheckBlk.NewLoad(irtypes.I8, doneField)
			rdLoad.Atomic = true
			rdLoad.Ordering = enum.AtomicOrderingAcquire
			rdLoad.Align = 1
			rdDone := coopRecheckBlk.NewICmp(enum.IPredNE, rdLoad, constant.NewInt(irtypes.I8, 0))
			coopRecheckBlk.NewCondBr(rdDone, checkBlk, deadlockBlk)

			msg := c.getTaskDeadlockMsgGlobal()
			msgPtr := deadlockBlk.NewGetElementPtr(msg.ContentType, msg,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			deadlockBlk.NewCall(c.palWrite, constant.NewInt(irtypes.I32, 2), msgPtr,
				constant.NewInt(irtypes.I64, 45))
			deadlockBlk.NewCall(c.palExit, constant.NewInt(irtypes.I32, 2))
			deadlockBlk.NewUnreachable()
		} else {
			// host: brief sleep then recheck
			c.block.NewCall(c.palUsleep, constant.NewInt(irtypes.I32, 100))
			c.block.NewBr(checkBlk)
		}

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

	// T0680 Part 2: no WASM deadline break emitted — original single-path return.
	if taskTimeoutBlk == nil {
		if isVoid {
			return nil
		}
		// T1150: register the received heap result as a droppable statement temp so
		// it is freed at statement end when consumed inline (no named binding owns
		// it). c.block is loadResultBlk here — where resultVal is live and the return
		// flows from. Claim-on-consume sites clear the flag when ownership transfers.
		c.trackReceivedTaskResult(resultVal, innerType)
		return resultVal
	}

	// WASM: merge the normal load path with the deadline-timeout path. The timeout
	// block yields a zeroinitializer (the value is dead — teardown discards it) and
	// deliberately does not free G (may still be running → tolerated teardown leak).
	loadDoneBlk := c.block
	mergeBlk := c.newBlock("task.recv_merge")
	loadDoneBlk.NewBr(mergeBlk)

	c.block = taskTimeoutBlk
	nullCapturedEnvField()
	c.block.NewBr(mergeBlk)

	c.block = mergeBlk
	if isVoid {
		return nil
	}
	resultPhi := mergeBlk.NewPhi(
		ir.NewIncoming(resultVal, loadDoneBlk),
		ir.NewIncoming(constant.NewZeroInitializer(resultLLVM), taskTimeoutBlk),
	)
	// T1150 (see above): register the merged result as a droppable statement temp.
	c.trackReceivedTaskResult(resultPhi, innerType)
	return resultPhi
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
	} else if c.isWasm {
		// T1200: pump the cooperative scheduler instead of the no-op cond_wait so a
		// non-coroutine receiver yields to its sender; on progress recheck emptiness.
		c.block = waitBodyBlock
		c.emitWasmCoopWaitPump(waitBlock)
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
