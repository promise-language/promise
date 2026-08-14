package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

func (c *Compiler) genReturnStmt(s *ast.ReturnStmt) {
	// Generator return: bare return means "stop producing values".
	// T1385: `c.inGenerator` stays set inside a `go { … }` block nested in a
	// generator body (genGoBlock does not reset it), but that body is a SEPARATE
	// coroutine function — its `return` belongs to the goroutine, not the
	// generator. c.coroutineReturnBlock is nil in the generator body itself
	// (defineGeneratorBody clears it) and non-nil only inside such a nested go
	// block, so it is exactly the discriminator. Without this the value form fell
	// into the generator branch, which stores nothing into G.result_ptr — `<-t`
	// read poison — and the bare form only worked because both coroutines happen
	// to name their final-suspend block `final.suspend`.
	if c.inGenerator && c.coroutineReturnBlock == nil {
		if len(c.scopeBindings) > 0 {
			// T1390: in a failable generator, emitScopeCleanup captures a failing
			// use-binding close() error; emitCloseErrCheck routes it into the
			// generator error_slot + final suspend. Discarding cap here would both
			// drop the error and leak it (the error is extracted into cap.val but
			// never checked/routed).
			cap := c.emitScopeCleanup(0, false)
			// emitScopeCleanup(0, ...) already cleaned the whole scope [0, end),
			// so the error path has nothing left to unwind: outerFloor = 0.
			c.emitCloseErrCheck(cap, 0)
		}
		// emitCloseErrCheck may have diverted the error path to the generator error
		// slot (terminator already set); only its OK-continuation is un-terminated.
		if c.block != nil && c.block.Term == nil {
			c.block.NewBr(c.generatorFinalSuspend)
		}
		// c.block already has a terminator, so subsequent codegen is skipped
		return
	}

	// B0353: Goroutine return: bare return means "exit this goroutine".
	// Branch to the coroutine's final suspend block instead of emitting
	// ret void (the coroutine function returns ptr, not void).
	// T1385: only the BARE form takes this shortcut. A `return <expr>` is §17.2
	// explicit-return style — it produces the goroutine's result, so it falls
	// through to the shared value-return path below (evaluate-before-cleanup,
	// dups, drop-flag clearing, temp claims, scope cleanup) and stores the value
	// into G.result_ptr instead of emitting a `ret`.
	if c.coroutineReturnBlock != nil && s.Value == nil {
		c.emitLambdaWritebacks()
		// T1391: capture and route a failing use-binding close() error (mirrors the
		// value-return path below). In a failable go! body emitScopeCleanup takes the
		// CAPTURE path (T1387 guard on c.inFailableGoBlock), so without a following
		// emitCloseErrCheck the error is saved-but-never-freed → leak. outerFloor=0:
		// this bare return cleaned all bindings [0, end), so there is no outer prefix
		// to unwind. For a non-failable go{} block closeCap is nil and emitCloseErrCheck
		// is a no-op.
		var closeCap *closeErrCapture
		if len(c.scopeBindings) > 0 {
			closeCap = c.emitScopeCleanup(0, false)
		}
		c.emitCloseErrCheck(closeCap, 0)
		// T1392: a bare `return` is an early SUCCESS exit of a `go {}` / `go! {}`
		// body. Unlike the trailing-expression exit (which stores {ok, value, null})
		// and the escaping-error exits (which store {err}), this path stored nothing
		// into the caller-allocated G.result_ptr — leaving that buffer uninitialized.
		// On wasm the poison bytes make `<-t` misread the {ok,err} discriminant as an
		// error and dereference a garbage error pointer (SIGABRT). storeGoResultDefault
		// writes the well-defined default so the receive sees success; it is a no-op
		// for a body with no result buffer (plain void, fire-and-forget, generator).
		// T1385/§17.2: a bare `return;` on a VALUE-producing path is now a sema error,
		// so a body reaching here is `T = Void` — the default IS the whole result.
		c.storeGoResultDefault()
		if c.block != nil && c.block.Term == nil {
			c.block.NewBr(c.coroutineReturnBlock)
		}
		return
	}

	// Write back move-captured variables to env struct before returning
	c.emitLambdaWritebacks()

	// T1339: temps below this snapshot belong to an enclosing INCOMPLETE call (a
	// sibling by-value ctor arg still on the stack when this return diverges out of
	// mid-argument evaluation). Only temps created while evaluating THIS return value
	// are moved out; the move-out clear below must not sweep the sibling prefix.
	enumCtorSnap := len(c.enumCtorTemps)

	// T1385: §17.2 explicit-return style inside a `go {}` / `go! {}` body. goRet
	// means the value terminates the goroutine (branch to the final suspend
	// instead of `ret`); goSink additionally means the caller allocated a result
	// buffer to store it into. Without a sink the returned value is DISCARDED, so
	// the move-out steps below (drop-flag clear, temp claims) must NOT run or it
	// leaks — same discard semantics as the trailing-value fire-and-forget path in
	// genGoBlock. Both flags below imply a buffer:
	//   - goBlockValueResultLLVM is set only on genGoBlock's useGoBlockValuePath
	//     (non-void AND not fire-and-forget), which is exactly when one is alloc'd;
	//   - inFailableGoBlock implies one because a fire-and-forget `go! {}` is a sema
	//     error ("a fire-and-forget goroutine must be non-failable", T1379), so every
	//     `go! {}` that reaches codegen is awaited and gets its aggregate buffer.
	// goRet is also true in the main/test coroutines (sched.go, compileTestCoroutine),
	// where neither flag is set: their bodies are void, so a `return <expr>` there is
	// already a sema error and the discard path is the correct fallback.
	goRet := c.coroutineReturnBlock != nil
	goSink := goRet && (c.inFailableGoBlock || c.goBlockValueResultLLVM != nil)

	// Set targetType so NoneLit can resolve to the correct Optional struct
	retType := c.currentRetType
	if retType != nil && c.typeSubst != nil {
		retType = types.Substitute(retType, c.typeSubst)
	}
	if retType != nil && c.selfSubst != nil {
		retType = types.SubstituteSelf(retType, c.selfSubst.iface, c.selfSubst.concrete)
	}

	// Evaluate the return expression BEFORE scope cleanup. The expression may
	// reference local variables with drop bindings (e.g., string variables passed
	// as function arguments: `return func(str_var)`). Scope cleanup frees those
	// variables, so we must compute the return value while they're still alive.
	var val value.Value
	if s.Value != nil {
		c.targetType = retType
		// T0095/B0179/B0219/B0310/T0487: Signal genFieldAccess to dup string,
		// Vector|Channel|Arc|Weak, and Optional[...] fields for return values.
		// Scope cleanup after the return may drop the containing type, freeing
		// the field — the caller needs an independent copy. Skip for borrow
		// return types (B0179) — borrows don't own the value.
		c.setDupFlagsForFieldAccess(retType)
		// T0440/T1180: Signal genMethodIndex/genVectorIndex to deep-clone the heap
		// user type out of the container slot for Optional[heap-user] return
		// values — covers both the droppable and no-drop-but-pal-free inner
		// shapes. Without this, `return m[k]` / `return v[i]` from a function
		// owning the container propagates an alias that double-frees when the
		// container drops at function exit. Routes through the shared
		// optionalHeapDupElem recognition point so this sink stays in sync with
		// the var-decl / assignment / mut-ref-arg escape sinks by construction.
		if _, ok := c.optionalHeapDupElem(retType); ok {
			c.dupHeapUserFieldAccess = true
		}
		// T1146: `return m[k]!` — `!` already unwrapped the Optional, so retType is
		// the bare element type (not Optional) and the branch above is skipped. The
		// unwrapped element aliases the map's slot; without a dup the map's drop at
		// function exit and the caller's drop of the returned value double-free.
		// Mirror the var-binding form (stmt.go:1133). The returned dup is claimed by
		// claimHeapTemp(val) below so cleanup doesn't drop it.
		if (isDroppableHeapUserType(retType) || isHeapUserNoDropPalFree(retType)) &&
			isUnwrappedContainerIndex(s.Value) {
			c.dupHeapUserFieldAccess = true
		}
		// T1488: a direct `return xs[i]` / `=> xs[i]` where xs is a FIXED-SIZE array
		// of a bare heap-user / Map-Set / droppable-enum element. genArrayIndex only
		// dups such an element when dupHeapUserFieldAccess is armed — T0590 arms it at
		// var-decl (stmt_decl.go) and assignment (stmt_assign.go) RHS sites, but the
		// direct return/arrow escape never did, so the returned value aliased the
		// array's owned slot → the array's element-walk drop and the caller's drop of
		// the returned value double-free at scope exit. Mirror the var-decl arming
		// (shape = IndexExpr into an *types.Array, gated on the return type). Scoped to
		// fixed arrays here; the Vector sibling (T1491) lives in the adjacent `else if`
		// branch below because it must exclude the droppable-enum shape (handled by the
		// post-hoc cloneEnumValue block below). String elements dup independently
		// (dupStringFieldAccess); structural via setDupFlagsForFieldAccess;
		// Optional[heap-user] via optionalHeapDupElem above.
		if !isRefType(retType) {
			if idx, isIdx := s.Value.(*ast.IndexExpr); isIdx {
				tgt := c.info.Types[idx.Target]
				if c.typeSubst != nil {
					tgt = types.Substitute(tgt, c.typeSubst)
				}
				if _, isArr := tgt.(*types.Array); isArr {
					if isDroppableHeapUserType(retType) || isHeapUserNoDropPalFree(retType) ||
						isMapOrSetType(retType) || c.enumElemNeedsDupOnRead(retType) {
						c.dupHeapUserFieldAccess = true
					}
				} else if _, isVec := types.AsVector(tgt); isVec {
					// T1491: Vector sibling of T1488. A direct `return xs[i]` / `=> xs[i]`
					// where xs is a Vector of a bare heap-user / Map-Set element aliases the
					// vector's owned slot; the vector's element-walk drop and the caller's
					// drop double-free at scope exit. EXCLUDES the droppable-enum shape
					// (enumElemNeedsDupOnRead): the post-hoc cloneEnumValue block below
					// already clones droppable-enum vector elements at return time — arming
					// here too would double-clone (leak). Strings via the B0189 dup below;
					// Optional[heap-user] via optionalHeapDupElem above.
					if isDroppableHeapUserType(retType) || isHeapUserNoDropPalFree(retType) ||
						isMapOrSetType(retType) {
						c.dupHeapUserFieldAccess = true
					}
				}
			}
		}
		// T0982: `return a ?: b` — when the return expression (peeling parens) is an
		// inline elvis, signal genElvis so it neutralizes a handle/heap none-path
		// owned-local default's scope-exit drop. The returned value escapes to the
		// caller; without this the function's own scope-exit drop of the default AND
		// the caller both free it (Mutex SEGV / Arc UAF / Map/heap double-free).
		// Vector/string results already neutralize unconditionally (T0936).
		prevElvisReturned := c.elvisResultReturned
		if be, ok := unwrapDestructureParens(s.Value).(*ast.BinaryExpr); ok && be.Op == ast.BinElvis {
			c.elvisResultReturned = true
		}
		// T1302: a borrow-typed return (`T&`/`T~`) that force-unwraps a
		// `this.field!` Optional must NOT dup the inner (genOptionalForceUnwrap's
		// T0428 Case 3B) — the caller borrows the aliased inner and never frees it,
		// while the owner's drop still frees the original, so the dup would leak.
		prevReturningBorrowedUnwrap := c.returningBorrowedUnwrap
		if retType != nil && isRefType(retType) {
			c.returningBorrowedUnwrap = true
		}
		val = c.genExpr(s.Value)
		// T1385: `go! { …; return produce(5); }` — a bare failable call as the
		// returned value auto-propagates. Unwrap it to its success value BEFORE any
		// cleanup below (mirrors genBlockValue's trailing case); genAutoPropagateValue
		// runs its own error-path cleanup and routes to the go-block sink via
		// emitFailableGoBlockError.
		if goRet && c.inFailableGoBlock && c.info.AutoPropagateExprs[s.Value] {
			val = c.genAutoPropagateValue(val)
		}
		c.returningBorrowedUnwrap = prevReturningBorrowedUnwrap
		c.elvisResultReturned = prevElvisReturned
		c.dupStringFieldAccess = false
		c.dupContainerFieldAccess = false
		c.dupHeapUserFieldAccess = false
		c.targetType = nil
		// T1174: `return maybe` where maybe is a match-borrowed Optional[heap-user]
		// binding aliases the subject's variant payload; deep-clone the inner so the
		// returned value survives the scope-exit synth enum drop of the subject.
		// T1184: skip for borrow return types (`T[N]&`) — a borrow return hands back
		// an alias the caller never owns or frees (same B0179 rule the field-access
		// dup above follows), so cloning a borrowed-array param (`echo(string[2] a)
		// string[2]& { return a; }`) into a borrow result would leak. Owning returns
		// still dup.
		if !isRefType(retType) {
			val, _ = c.dupBorrowedHeapUserPayload(s.Value, val)
			// T1323: a borrowed (non-`~`) enum VALUE param returned by value aliases
			// the caller's arg temp — deep-clone the variant payload so the result is
			// independent (the enum's inline-data payload is invisible to the caller-
			// side alias check). Enum-only; strings/containers/arrays are covered by
			// dupBorrowedHeapUserPayload / the string dup above.
			val, _ = c.dupBorrowedEnumParam(s.Value, val, retType)
		}
		val = c.wrapThisReturnValue(val, s.Value, retType)
		val = c.wrapOperatorParamReturnValue(val, s.Value, retType) // T0897
		val = c.maybeDupReturnedEnvCapture(val, s.Value, retType)   // T1254
	}

	// B0189: Dup return value if it's a string that might be borrowed from a
	// Vector[string] in the current scope. The element drop loop in scope
	// cleanup will free the vector's elements — if the return value borrows
	// one of those elements, it would become a dangling pointer.
	// Covers: `return strVar` (IdentExpr) and `return vec[i]` (IndexExpr).
	// T0649: skip for borrow return types (`string&`/`string~`) — a borrow
	// return must hand back the actual reference into existing storage (which
	// the ownership pass guarantees outlives the call), not a fresh copy.
	// Dup'ing here would leak: the call site treats a borrow result as a
	// non-owned alias and never frees it. extractNamed unwraps SharedRef/MutRef
	// so the TypString check alone fires for `string&`, hence the explicit
	// isRefType guard.
	needsDup := false
	if s.Value != nil && val != nil && extractNamed(retType) == types.TypString && !isRefType(retType) {
		if ident, ok := s.Value.(*ast.IdentExpr); ok {
			// T0963: an operator value param returned as an owned string was already
			// dup'd by wrapOperatorParamReturnValue (cloneOwnedReturnAlias). Dup'ing
			// again here would leak the first copy — the caller frees the outer dup
			// and the inner one is orphaned. The op-param dup already yields an
			// independent heap string, so no vector-element alias survives to protect.
			if !c.currentOpValueParams[ident.Name] {
				needsDup = c.hasVectorStringBinding()
			}
		} else if idx, ok := s.Value.(*ast.IndexExpr); ok {
			targetType := c.info.Types[idx.Target]
			if c.typeSubst != nil {
				targetType = types.Substitute(targetType, c.typeSubst)
			}
			if _, isVec := types.AsVector(targetType); isVec {
				needsDup = true
			}
		}
		if needsDup {
			val = c.dupString(val)
		}
	}

	// Clone return value if it's a droppable enum loaded from a vector index.
	// Scope cleanup drops the dup'd vector (freeing its buffer and all elements) —
	// the shallow enum copy returned by vec[i] would reference freed data.
	// Analogous to the B0189 string dup above.
	// T0649: skip for borrow return types (`MyEnum&`/`MyEnum~`) for the same
	// reason as the string dup — a borrow return must hand back the actual
	// reference, and cloning here would leak at a binding call site.
	if s.Value != nil && val != nil && !needsDup && !isRefType(retType) {
		if idx, ok := s.Value.(*ast.IndexExpr); ok {
			idxTargetType := c.info.Types[idx.Target]
			if c.typeSubst != nil {
				idxTargetType = types.Substitute(idxTargetType, c.typeSubst)
			}
			if elemType, isVec := types.AsVector(idxTargetType); isVec {
				resolvedElem := elemType
				if c.typeSubst != nil {
					resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
				}
				if enum := extractEnum(resolvedElem); enum != nil {
					if c.enumInstanceHasDrop(resolvedElem, enum) {
						if cloned, ok := c.cloneEnumValue(val, resolvedElem); ok {
							val = cloned
							needsDup = true // preserve drop flag for source vector
						}
					}
				}
			}
		}
	}

	// T1282: Decide whether a string→structural box at this return move-site should
	// take ownership of the source pointer (owned source) or clone it (borrowed). This
	// MUST be computed before the flag-clear (below) and claimStringTemp (further down),
	// which both mutate the state we read. Owned sources — an owned var / move-param
	// (has a drop flag) or an owned frame temp (in stmtTempMap) — take the pointer, since
	// the move-site releases the source. Borrowed sources (literal, borrow param, field
	// borrow) match neither and stay on the clone path.
	retBoxSrcOwned := false
	if s.Value != nil {
		// Peel Optional so `Showable?` returns take the same owned/borrowed decision
		// as a plain `Showable` return — the actual box is built inside
		// coerceReturnToOptionalElem (T1298), which also reads c.boxSrcOwned.
		structTarget := retType
		if opt, isOpt := retType.(*types.Optional); isOpt {
			structTarget = opt.Elem()
		}
		if named := extractNamed(structTarget); named != nil && named.IsStructural() {
			if ident, ok := s.Value.(*ast.IdentExpr); ok {
				// Only owned if the move-site actually clears the flag (!needsDup).
				retBoxSrcOwned = !needsDup && c.hasDropFlag(ident.Name)
			} else if _, tracked := c.stmtTempMap[val]; tracked {
				retBoxSrcOwned = true
			}
		}
	}

	// Clear drop flag for returned variable (it's being moved out, not dropped).
	// B0205: When the return value was dup'd (B0189), the original variable must
	// still be dropped at scope exit — the caller receives the dup, not the original.
	// Only clear the flag when we're returning the original (no dup).
	// T1385: `!goRet || goSink` — a fire-and-forget go block discards the returned
	// value, so leaving the flag set lets emitScopeCleanup drop the local (no leak).
	if s.Value != nil && !needsDup && (!goRet || goSink) {
		if ident, ok := s.Value.(*ast.IdentExpr); ok {
			c.clearDropFlag(ident.Name)
		} else if _, ok := unwrapDestructureParens(s.Value).(*ast.CastExpr); ok {
			// T0783: `return x as! T` aliases x's instance into the returned value;
			// ownership now moves x at the return, so clear x's drop flag to keep
			// codegen symmetric — otherwise x's scope-exit drop fires on the same
			// allocation the caller now owns (double-free).
			//
			// T0849: the optional `as` form (Force == false) yields `T?` and is a
			// *conditional* move (None on a failed downcast). consumeCastSubjectDropFlag
			// reads the outermost cast's Force: `as!` clears unconditionally; `as`
			// stores `!isMatch` so x is dropped iff the downcast failed (else the
			// returned optional owns the aliased instance).
			if ident := c.castSubjectMovableIdent(s.Value); ident != nil {
				c.consumeCastSubjectDropFlag(s.Value, ident.Name)
			}
		}
	}
	// T0108: Clean up statement temps before returning. The return expression may
	// create intermediate string temps (e.g., dupStringFieldAccess dup copies,
	// string concat intermediaries) that are normally freed at statement end.
	// Since return terminates the block, the post-statement cleanup never runs.
	// Claim the return value first so it's not freed — only intermediaries are freed.
	// T1385: same fire-and-forget gate as the drop-flag clear above — not claiming
	// lets the cleanup below free the discarded value.
	if s.Value != nil && val != nil && (!goRet || goSink) {
		c.claimStringTemp(val)
		c.claimHeapTemp(val)
		c.claimEnvTemp(val)
	}
	// B0310: Claim dup'd inner string for Optional[string] return values.
	// Without this, cleanupStmtTemps would free the dup while it's still
	// embedded in the return value's optional struct.
	if c.optionalStringDup != nil {
		c.claimStringTemp(c.optionalStringDup)
		c.optionalStringDup = nil
	}
	// T0366: Claim dup'd inner container for Optional[Vector|Channel|Arc|Weak] return values.
	if c.optionalContainerDup != nil {
		c.claimStringTemp(c.optionalContainerDup)
		c.optionalContainerDup = nil
	}
	// T1317: An inline enum-constructor temp built while evaluating the return
	// expression as a by-value(-borrow) call argument (e.g.
	// `return f(Payload.Full(heapStr))`) is owned by the caller — the callee dups
	// the payload (B0232) and nothing else drops the original. The statement-end
	// drain (cleanupStmtLevelTemps) frees such a temp for a discarded call, but the
	// return path never reaches it, so the payload leaks. Clear the flag first when
	// the return expression is ITSELF an enum constructor (`return Payload.Full(...)`)
	// — that temp is moved out to the caller (mirrors the B0267 var-decl / T1103
	// container clears) — then drain the rest. A structural check (not the value's
	// type) is required so `return g(Payload.Full(...))` where g *returns* an enum
	// still drains the by-value arg temp rather than mistaking it for the result.
	// The moved-out shapes are a direct constructor or a branch (match/if) whose
	// arm values ARE the returned enum — see enumCtorTempMovesOut.
	//
	// T1339: bound the clear to temps at/above enumCtorSnap. Temps below the
	// snapshot belong to an enclosing INCOMPLETE call whose by-value ctor arg is
	// still on the stack when this return diverges mid-argument-evaluation; they are
	// NOT this return's value, so the wholesale clear must not sweep them (doing so
	// orphans the sibling's heap payload). The full drainEnumCtorTemps below still
	// drops the surviving prefix on the divergent path.
	if len(c.enumCtorTemps) > enumCtorSnap && s.Value != nil && c.enumCtorTempMovesOut(s.Value) {
		for i := enumCtorSnap; i < len(c.enumCtorTemps); i++ {
			c.block.NewStore(constant.NewInt(irtypes.I1, 0), c.enumCtorTemps[i].dropFlag)
		}
		c.enumCtorTemps = c.enumCtorTemps[:enumCtorSnap]
	}
	if c.block != nil && c.block.Term == nil {
		c.cleanupStmtTemps()
		c.cleanupHeapTemps()
		c.cleanupEnvTemps()
		// T1317: drop orphaned inline enum-constructor arg temps (see above).
		c.drainEnumCtorTemps()
	}
	// Emit cleanup for all active scope bindings before returning
	// T1427: on the go-block value exit the return value was claimed away from temp
	// cleanup (above) for the store into G.result_ptr; a failing use-close divert
	// stores the {err} aggregate instead, orphaning it. Arm the divert-drop across
	// the close check. Use the return-expression's sema type (not retType) so the
	// drop matches val's actual pre-coercion shape — the OK-path coerceReturnValue
	// may box/optional-wrap it, but the divert and OK paths are mutually exclusive.
	if goSink && c.inFailableGoBlock && val != nil {
		exprTy := c.info.Types[s.Value]
		if c.typeSubst != nil && exprTy != nil {
			exprTy = types.Substitute(exprTy, c.typeSubst)
		}
		c.goResultDivertVal, c.goResultDivertType = val, exprTy
	}
	var closeCap *closeErrCapture
	if len(c.scopeBindings) > 0 {
		closeCap = c.emitScopeCleanup(0, false)
	}
	c.emitCloseErrCheck(closeCap, 0)
	c.goResultDivertVal, c.goResultDivertType = nil, nil

	// T1385: §17.2 explicit-return style — store the returned value into the
	// goroutine's result buffer and branch to the coroutine's final suspend. The
	// coroutine ramp returns a handle, so a `ret` here would be invalid IR.
	if goRet {
		if goSink && c.block != nil && c.block.Term == nil {
			val = c.coerceReturnValue(val, s.Value, retType, retBoxSrcOwned)
			if c.inFailableGoBlock {
				c.storeGoResultAgg(c.wrapOk(val, c.failableGoBlockAggType))
			} else {
				c.storeGoResultAgg(val) // raw success type; buffer typed by goBlockValueResultLLVM
			}
		}
		if c.block != nil && c.block.Term == nil {
			c.block.NewBr(c.coroutineReturnBlock)
		}
		return
	}

	if c.canError {
		resultType := c.currentResultType()
		if s.Value == nil {
			c.block.NewRet(c.wrapOk(nil, resultType))
		} else {
			// If the expression is itself a failable call, val is already a
			// failable result struct matching our result type — return directly.
			if c.info.FailableExprs[s.Value] && val != nil && val.Type().Equal(resultType) {
				c.block.NewRet(val)
			} else {
				c.block.NewRet(c.wrapOk(c.coerceReturnValue(val, s.Value, retType, retBoxSrcOwned), resultType))
			}
		}
		return
	}
	if s.Value == nil {
		c.block.NewRet(nil)
	} else {
		c.block.NewRet(c.coerceReturnValue(val, s.Value, retType, retBoxSrcOwned))
	}
}

// coerceReturnValue applies the shape coercions every return value needs before
// it lands in the result slot: structural box into an Optional element, Optional
// wrap, and value-struct vtable coercion through a parent/view type. Shared by
// the failable-`ret`, plain-`ret`, and goroutine-result (T1385) exits of
// genReturnStmt so all three agree by construction.
func (c *Compiler) coerceReturnValue(val value.Value, expr ast.Expr, retType types.Type, boxSrcOwned bool) value.Value {
	// T1298: box/view-coerce into the Optional return type's element BEFORE the
	// optional wrap, so `return Counter(...)` from a `Sink?`-returning function
	// boxes to the structural view first (the trailing coerceToView runs against
	// the Optional target and is a no-op).
	// T1282: an owned move source boxed into an Optional structural element is
	// built here, so the clone-vs-move decision must apply at this call.
	c.boxSrcOwned = boxSrcOwned
	val = c.coerceReturnToOptionalElem(val, expr, retType)
	c.boxSrcOwned = false
	// Wrap value in Optional if return type is Optional but expr is not
	val = c.wrapReturnOptional(val, expr, retType)
	// Coerce value struct vtable when returning through a parent type
	if retType != nil {
		exprType := c.info.Types[expr]
		if c.typeSubst != nil {
			exprType = types.Substitute(exprType, c.typeSubst)
		}
		if c.selfSubst != nil {
			exprType = types.SubstituteSelf(exprType, c.selfSubst.iface, c.selfSubst.concrete)
		}
		// T1282: apply the owned/borrowed box decision; T1320: coerceReturnToView
		// heap-allocates a value-type box that escapes to the caller, then
		// delegates to coerceToView (which reads boxSrcOwned).
		c.boxSrcOwned = boxSrcOwned
		val = c.coerceReturnToView(val, exprType, retType)
		c.boxSrcOwned = false
	}
	return val
}

// wrapThisReturnValue wraps a `this` expression (i8* instance pointer) into the
// appropriate value struct when returning from a method. For heap types, builds
// { vtable_ptr, instance_ptr }. For value types, loads the full value struct from
// the pointer. No-op if the return expression is not ThisExpr.
// T0582: peel ParenExpr so `return (this);` takes the same path as `return this;`.
func (c *Compiler) wrapThisReturnValue(val value.Value, expr ast.Expr, retType types.Type) value.Value {
	expr = unwrapDestructureParens(expr)
	if _, isThis := expr.(*ast.ThisExpr); !isThis {
		return val
	}
	if retType == nil {
		return val
	}
	// T0906: a method returning an optional of the receiver type (`OB?`) reaches
	// here with retType = Optional[OB]. Build the value-struct payload from the
	// `this` instance pointer using the optional's element type; genReturnStmt's
	// subsequent wrapReturnOptional call wraps that payload into the optional.
	// extractNamed does not peel Optional, so without this the function bails and
	// the bare i8* instance pointer flows into wrapOptional's {i8*,i8*} insertvalue
	// (panic: insertvalue elem type mismatch).
	effType := retType
	if opt, ok := retType.(*types.Optional); ok {
		effType = opt.Elem()
	}

	// Enum receiver: `this` is an i8* pointer (genThisExpr), but an enum method
	// returns the enum value by value ({i32 tag, [N x i8] data} or a bare i32 for
	// fieldless enums). Load the value struct via enumThisSubject so the bare i8*
	// pointer never flows into the function result / optional wrap (panic:
	// insertvalue elem type mismatch). Covers both `dup() E { return this; }` and
	// the optional form `dup() E? { return this; }` (T0906). extractNamed returns
	// nil for enums, so this must run before the Named handling below.
	if enumT := extractEnum(effType); enumT != nil {
		layout := c.lookupEnumLayout(effType)
		if layout == nil {
			return val
		}
		enumVal := c.enumThisSubject(val, layout)
		// T0893 analog: a borrowing `return this` whose variant payload is droppable
		// (e.g. `B(string)`) would hand back a shallow copy aliasing the receiver's
		// heap data — both the result and the receiver would free it (double-free).
		// Deep-clone the payload so the returned value owns it independently. Skip
		// for borrow return types (caller expects a reference) and `~this` receivers
		// (genuine ownership transfer — cloning would copy needlessly and leak).
		if !isRefType(effType) && !c.thisRecvIsOwned {
			enumVal = c.cloneOwnedReturnAlias(enumVal, effType)
		}
		return enumVal
	}

	named := extractNamed(effType)
	if named == nil {
		return val
	}
	// T0963: a borrowing string method whose body is `return this` (plain `this`,
	// not `~this`) with an owned string return type would hand back the receiver's
	// i8* instance pointer unchanged — the caller's owned result and the (possibly
	// temporary) receiver then both free the same allocation (double-free), and any
	// interleaved use corrupts the shared string. Dup the instance so the returned
	// owned value is independent. `dupString` clears the .rodata literal flag via
	// promise_string_new, so a literal receiver stays a no-op-drop literal. Skip for
	// borrow return types (caller expects a reference) and `~this` receivers
	// (genuine ownership transfer — cloning would copy needlessly and leak). Mirrors
	// the T0893 clone for user heap types / enums, which this branch predates.
	if named == types.TypString {
		if !isRefType(effType) && !c.thisRecvIsOwned {
			return c.dupString(val)
		}
		return val
	}
	if classify(named) != CatUnknown || named == types.TypVoid || named == types.TypNone {
		return val
	}

	if named.IsValueType() {
		// Value type: `this` is i8* pointing to the value struct — load it
		layout := c.lookupTypeLayout(effType)
		if layout == nil {
			return val
		}
		typedPtr := c.block.NewBitCast(val, irtypes.NewPointer(layout.Value.LLVMType))
		return c.block.NewLoad(layout.Value.LLVMType, typedPtr)
	}

	// Heap type: `this` is i8* instance pointer — build { vtable_ptr, instance_ptr }
	var vtablePtr value.Value
	if c.needsVtable(named) {
		// T0917: For an abstract base / parent return type, lookupVtableGlobal(effType)
		// yields the base's vtable global, whose slots are null for abstract methods —
		// a later virtual call on the result loads a null fn ptr and segfaults. Load the
		// concrete subtype's vtable from the receiver instance's RTTI instead
		// (instance → variant ptr (field 0) → typeinfo vtable_ptr (field 0)) so virtual
		// dispatch on the returned value resolves the override.
		vtablePtr = c.loadVtablePtrFromInstance(val)
	} else if vtGlobal := c.lookupVtableGlobal(effType); vtGlobal != nil {
		vtablePtr = constant.NewBitCast(vtGlobal, irtypes.I8Ptr)
	} else {
		vtablePtr = constant.NewNull(irtypes.I8Ptr)
	}
	var result value.Value = constant.NewUndef(userValueType())
	result = c.block.NewInsertValue(result, vtablePtr, 0)
	result = c.block.NewInsertValue(result, val, 1)

	// T0893: a borrowing method/operator whose body is `return this` (bare receiver)
	// would otherwise hand back a value struct aliasing the receiver's heap instance —
	// the result binding and the receiver then share the same mutable allocation, so
	// one's scope-drop frees memory the other still reads. Clone the instance so the
	// returned owned value is independent. The caller-side alias-clears
	// (B0250/T0341/T0347/T0892/T0899/T0562/T0882/T0958) remain as harmless no-ops once
	// the pointers differ — T1084 confirms this clone is the sole operative guard.
	//
	// Skip when:
	//   - the return type is a borrow (`T&`/`T~`): the caller expects a reference into
	//     existing storage, not a copy (isRefType).
	//   - the receiver is `~this` (RefMut): `this` is owned/moved-in, so `return this`
	//     is a genuine ownership transfer — cloning would copy needlessly and leak the
	//     moved-in instance (c.thisRecvIsOwned).
	if !isRefType(effType) && !c.thisRecvIsOwned {
		result = c.cloneOwnedReturnAlias(result, effType)
	}
	return result
}

// cloneOwnedReturnAlias deep-clones an already-materialized owned return value
// of effType so the returned value owns its heap data independently of whatever
// borrowed source it currently aliases — the `this` receiver (T0893) or a
// borrowed operator operand (T0897). For enums it clones droppable variant
// payloads; for heap user types it dups the instance; value/Copy/string/void/
// none types have no heap alias and are returned unchanged. Callers gate on
// isRefType / ownership before invoking. `val` must already be the
// function-ABI value (enum value, {vtable,instance} struct, or value struct).
// dupOwnedReturnValue deep-clones a droppable value so the new owner holds an
// allocation independent of a still-live source local (T1031). Used at the call
// site by emitReturnAliasCheckSubst when a call's result aliases a named source
// argument the caller still owns. Reuses the shared push-element dispatcher
// (maybeDupPushElement: tuples, droppable enums, vectors, channels, Arc/Weak,
// heap user types), adds the plain-string case that dispatcher omits, and handles
// Optional[droppable] via dupOptionalVectorElem (present/absent split + inner
// clone). The bool result reports whether an independent clone was actually
// produced: false means the type is one we do not deep-clone (Copy/value/
// single-owner handles, or a non-droppable Optional inner), in which case the
// caller must fall back to transferring ownership rather than relying on a clone
// that did not happen.
func (c *Compiler) dupOwnedReturnValue(val value.Value, resolvedType types.Type) (value.Value, bool) {
	if val == nil {
		return val, false
	}
	if extractNamed(resolvedType) == types.TypString {
		return c.dupString(val), true
	}
	// Optional[droppable] — deep-clone the inner value (when present) and rebuild the
	// Optional, so the new owner and the still-live source hold independent inner
	// allocations. dupOptionalVectorElem already implements the present/absent split
	// for every droppable inner shape (string, vector, channel, Arc/Weak, heap user
	// type, tuple). Only worth a clone when the inner is actually droppable; a
	// non-droppable inner (Copy/value) needs no dup and is left to the caller's
	// ownership-transfer fallback.
	if opt, ok := resolvedType.(*types.Optional); ok {
		elem := opt.Elem()
		if c.typeSubst != nil {
			elem = types.Substitute(elem, c.typeSubst)
		}
		if isTypeDroppable(elem) {
			return c.dupOptionalVectorElem(val, opt, elem), true
		}
		return val, false
	}
	if dup := c.maybeDupPushElement(val, resolvedType); dup != nil {
		return dup, true
	}
	return val, false
}

// maybeDupReturnedEnvCapture clones the return value when a lambda body hands
// back one of its env-owned move captures directly, e.g. `move || -> a` where
// `a` is a captured droppable string/vector/heap value (T1254). The env struct
// retains the captured value for repeat calls and the env drop function frees
// it when the closure is dropped, so returning the raw captured pointer lets the
// caller AND env_drop free the same allocation (double-free / use-after-free).
// Cloning gives the caller an independent value; the env keeps and later frees
// its own copy. Only fires for captures recorded in lambdaEnvOwnedCaptures
// (those env_drop actually frees) and never for borrow return types (`T&`/`T~`),
// which hand back an alias the caller does not own.
func (c *Compiler) maybeDupReturnedEnvCapture(val value.Value, expr ast.Expr, retType types.Type) value.Value {
	if val == nil || retType == nil || len(c.lambdaEnvOwnedCaptures) == 0 {
		return val
	}
	if isRefType(retType) {
		return val
	}
	ident, ok := unwrapDestructureParens(expr).(*ast.IdentExpr)
	if !ok || !c.lambdaEnvOwnedCaptures[ident.Name] {
		return val
	}
	resolved := retType
	if c.typeSubst != nil {
		resolved = types.Substitute(resolved, c.typeSubst)
	}
	if c.selfSubst != nil {
		resolved = types.SubstituteSelf(resolved, c.selfSubst.iface, c.selfSubst.concrete)
	}
	if dup, done := c.dupOwnedReturnValue(val, resolved); done {
		return dup
	}
	return val
}

func (c *Compiler) cloneOwnedReturnAlias(val value.Value, effType types.Type) value.Value {
	if enumT := extractEnum(effType); enumT != nil {
		if !c.enumInstanceHasDrop(effType, enumT) {
			return val
		}
		if cloned, ok := c.cloneEnumValue(val, effType); ok {
			return cloned
		}
		// Droppable enum without a clone method — dup variant fields in place
		// via an alloca round-trip (same path as maybeDupPushElement).
		alloca := c.createEntryAlloca(val.Type())
		c.block.NewStore(val, alloca)
		c.dupEnumElementInPlace(alloca, effType)
		return c.block.NewLoad(val.Type(), alloca)
	}
	named := extractNamed(effType)
	if named == nil {
		return val
	}
	// T0963: a borrowed string operand returned as an owned value (operator-operand
	// T0897 path) aliases the caller's still-live operand — dup it so the returned
	// owned value frees an independent allocation.
	if named == types.TypString {
		return c.dupString(val)
	}
	if classify(named) != CatUnknown || named == types.TypVoid || named == types.TypNone {
		return val
	}
	if named.IsValueType() {
		return val
	}
	// T1193: Vector/Channel/Arc/Weak are native i8* handles (isContainerType),
	// not the {vtable,instance} value struct dupHeapValue expects — routing them
	// there panics (interface conversion PointerType→StructType). Dispatch the
	// native-handle family through the shared push-element dispatcher, which dups
	// each correctly (dupVector/dupChannel/dupArc/dupWeak; single-owner
	// Task/Mutex/MutexGuard have no dup and return nil → left as-is). Plain heap
	// user types — and Map/Set, which are Promise value structs — keep the
	// well-tested dupHeapValue path (T0893). string is already handled above.
	if isContainerType(effType) {
		if dup := c.maybeDupPushElement(val, effType); dup != nil {
			return dup
		}
		return val
	}
	return c.dupHeapValue(val, effType)
}

// wrapOperatorParamReturnValue deep-clones the return value when an operator
// method body returns one of its borrowed value operands unchanged
// (e.g. `+(S other) S { return other; }`). Operator dispatch borrows operands
// rather than moving them, so the returned value would otherwise alias the
// caller's still-live operand and both bindings would free the same heap
// instance (double-free). Mirrors wrapThisReturnValue's clone-on-`return this`
// (T0893) for the right-hand operand (T0897). The value is already the
// function-ABI value, so only the clone is needed (no i8*→struct wrapping).
func (c *Compiler) wrapOperatorParamReturnValue(val value.Value, expr ast.Expr, retType types.Type) value.Value {
	if val == nil || retType == nil || len(c.currentOpValueParams) == 0 {
		return val
	}
	ident, ok := unwrapDestructureParens(expr).(*ast.IdentExpr)
	if !ok || !c.currentOpValueParams[ident.Name] {
		return val
	}
	// Borrow return type: hand back the reference into existing storage, not a copy.
	if isRefType(retType) {
		return val
	}
	effType := retType
	if opt, ok := retType.(*types.Optional); ok {
		effType = opt.Elem()
	}
	return c.cloneOwnedReturnAlias(val, effType)
}

// chainOriginExpr walks a possibly-chained method call back to its base
// expression. For `w.f().g().h()` returns the IdentExpr `w`; for
// `this.f().g()` returns the ThisExpr; for chains rooted in a non-method
// expression (constructor, free call, field access) returns that expression
// (which the caller's switch will ignore). T0347.
// T0582: peel ParenExpr at each step so `(w).f()`, `(w.f()).g()`, and
// `((w).f()).g()` all resolve to the underlying receiver.
//
// NOTE: keep in sync with ownership.aliasReceiverOrigin, which mirrors this to
// suppress NLL early-drop of the aliasing `return this` result (T0889). If which
// origins trigger the alias-clear here change, the NLL mirror must change too —
// otherwise codegen clears a drop flag the NLL pass does not suppress,
// reintroducing the use-after-free.
func chainOriginExpr(call *ast.CallExpr) ast.Expr {
	var expr ast.Expr = call
	for {
		expr = unwrapDestructureParens(expr)
		c, ok := expr.(*ast.CallExpr)
		if !ok {
			return expr
		}
		m, ok := c.Callee.(*ast.MemberExpr)
		if !ok {
			return expr
		}
		expr = m.Target
	}
}

// operatorReceiverOrigin returns the receiver-origin expression of a binary or
// prefix-unary operator that dispatches to a user-defined operator method — the
// LEFT operand of a binary operator (it becomes `this`), or the operand of a
// prefix unary operator. Used to extend the B0250/T0341 receiver-alias-clear to
// operator dispatch: a user operator whose body is `return this` yields a result
// that aliases this operand, and without the clear both would free the same
// instance (T0882). Returns nil for non-operators, the AST-level special binary
// forms (&&, ||, ?:, .., ..= — never alias a heap receiver) and <- receive.
// The downstream maybeClearReceiverDropFlag / pendingThisAliasClear guards
// (heap-user retType, exact {i8*,i8*} val shape) make a nil-safe over-call cheap
// and correct, so no native/value-type pre-filtering is needed here.
func operatorReceiverOrigin(e ast.Expr) ast.Expr {
	switch ex := e.(type) {
	case *ast.BinaryExpr:
		switch ex.Op {
		case ast.BinAnd, ast.BinOr, ast.BinElvis,
			ast.BinExclusiveRange, ast.BinInclusiveRange:
			return nil
		}
		return operatorOriginLeaf(unwrapDestructureParens(ex.Left))
	case *ast.UnaryExpr:
		if ex.Op == ast.UnaryReceive {
			return nil
		}
		return operatorOriginLeaf(unwrapDestructureParens(ex.Operand))
	}
	return nil
}

// operatorOriginLeaf walks chained operator dispatch down to the ultimate
// receiver leaf. T0958: for `a + b + c` (all `return this`), the outer operator's
// left operand is itself the `a + b` BinaryExpr whose result aliases `a`; without
// descending, operatorReceiverOrigin would return the BinaryExpr (neither Ident
// nor This), so the alias-clear is skipped and `a`'s binding and the chained
// result both free the same allocation → double-free. Mirrors chainOriginExpr's
// walk through chained method calls (T0347). Returning an over-deep leaf is
// harmless: every consumer guards the drop-flag clear with a runtime
// pointer-equality check, so a non-aliasing chain (an intermediate operator
// returning a fresh instance) simply fails the icmp and clears nothing.
func operatorOriginLeaf(operand ast.Expr) ast.Expr {
	if inner := operatorReceiverOrigin(operand); inner != nil {
		return inner
	}
	return operand
}

// maybeClearReceiverDropFlag emits a runtime check: if the method call result's
// instance pointer matches the receiver variable's instance pointer, clear the
// receiver's drop flag. This prevents double-free when a borrowing method does
// `return this` — both the receiver and the result would otherwise own the same
// heap allocation. B0250.
//
// T1084: since T0893, wrapThisReturnValue clones the `return this` result, so the
// result instance pointer never equals the receiver's. The `same` icmp below is
// therefore always false and this clear is a runtime no-op for every caller
// (B0250/T0341/T0347/T0892/T0899/T0562/T0882/T0958). It is retained only as a cheap
// backstop against the double-free *abort* — note it does NOT restore the
// independent ownership the clone provides (clearing the flag alone leaves the value
// aliased), so it is not a substitute for the clone. The clone in wrapThisReturnValue
// is the operative guard; this is dead defense-in-depth kept for clarity over churn.
func (c *Compiler) maybeClearReceiverDropFlag(val value.Value, recvName string, retType types.Type) {
	if retType == nil {
		return
	}
	named := extractNamed(retType)
	if named == nil || classify(named) != CatUnknown || named == types.TypString || named == types.TypVoid || named == types.TypNone || named.IsValueType() {
		return
	}
	recvAlloca, exists := c.locals[recvName]
	if !exists {
		return
	}
	flag, hasDrop := c.dropFlags[recvName]
	if !hasDrop {
		return
	}

	// T0562: val must be exactly {i8*, i8*} (a bare user value struct). After
	// Optional/Tuple wrapping val becomes e.g. {i1, {i8*,i8*}} — field 1 is no
	// longer the instance pointer, and emitting the icmp would either crash
	// the IR builder (struct vs ptr) or compare wrong bytes.
	if !isUserValueStructType(val.Type()) {
		return
	}
	// T0562: recvAlloca's pointee must also be {i8*, i8*}. Native handles
	// (Arc/Weak/Mutex/MutexGuard/Channel/Vector/Task) use `alloca i8*` — loading
	// userValueType() would read 16 bytes from an 8-byte slot (UB), and if the
	// trailing garbage happens to match the result's inner pointer, the receiver's
	// drop flag would be wrongly cleared and the handle would leak.
	if !isUserValueStructType(recvAlloca.ElemType) {
		return
	}
	retInst := c.block.NewExtractValue(val, 1)
	recvVal := c.block.NewLoad(userValueType(), recvAlloca)
	recvInst := c.block.NewExtractValue(recvVal, 1)
	same := c.block.NewICmp(enum.IPredEQ, retInst, recvInst)

	clearBlk := c.newBlock("return.this.clear")
	skipBlk := c.newBlock("return.this.skip")
	c.block.NewCondBr(same, clearBlk, skipBlk)

	clearBlk.NewStore(constant.False, flag)
	clearBlk.NewBr(skipBlk)

	c.block = skipBlk
}

// clearOperandAliasForOwnedStore clears the drop flag of an operator/method
// operand when the RHS result aliases it via `return this`, for owned-slot
// assignment targets (field/element) that have no target-local drop flag. The
// self-alias case (`h.f = h.f + b` / `v[0] = v[0] + b`) has a non-Ident origin
// (MemberExpr/IndexExpr), so it is skipped here and handled by the target's own
// same-pointer drop-old guard in genMemberAssign/genVectorIndexAssign. A
// ThisExpr origin is likewise skipped (this is borrowed, and sema forbids moving
// it into an owned slot). The downstream maybeClearReceiverDropFlag runtime icmp
// makes a non-aliasing (fresh-value) call a no-op. T0899; shared by the
// MemberExpr and IndexExpr branches of genAssignStmt.
//
// T1084: with T0893's clone in wrapThisReturnValue the result never aliases the
// operand, so the downstream icmp is always false and this whole path is a runtime
// no-op (see maybeClearReceiverDropFlag).
func (c *Compiler) clearOperandAliasForOwnedStore(rhs ast.Expr, val value.Value) {
	var aliasOrigin ast.Expr
	if call, ok := rhs.(*ast.CallExpr); ok {
		aliasOrigin = chainOriginExpr(call)
	} else {
		aliasOrigin = operatorReceiverOrigin(rhs)
	}
	origin, ok := aliasOrigin.(*ast.IdentExpr)
	if !ok {
		return
	}
	exprType := c.info.Types[rhs]
	if c.typeSubst != nil {
		exprType = types.Substitute(exprType, c.typeSubst)
	}
	c.maybeClearReceiverDropFlag(val, origin.Name, exprType)
}

// isUserValueStructType reports whether t is exactly the user value struct
// shape {i8*, i8*}. Used by the B0250/T0347 alias-clear emitters to guard
// against Optional/Tuple-wrapped values and native-handle allocas. T0562.
func isUserValueStructType(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)
	if !ok || len(st.Fields) != 2 {
		return false
	}
	return st.Fields[0].Equal(irtypes.I8Ptr) && st.Fields[1].Equal(irtypes.I8Ptr)
}

// maybeClearBindingDropFlagOnThisAlias emits a runtime check: if the method
// call result's instance pointer matches the current method's `this`, clear
// the new binding's drop flag. Mirrors maybeClearReceiverDropFlag but applies
// to `r := this.method()` (or any chain rooted at `this`) inside a method:
// `this` itself has no drop flag (it's borrowed), so we must clear the binding's
// flag instead of a receiver's. T0347.
func (c *Compiler) maybeClearBindingDropFlagOnThisAlias(val value.Value, bindingFlag value.Value, retType types.Type) {
	if retType == nil || bindingFlag == nil {
		return
	}
	named := extractNamed(retType)
	if named == nil || classify(named) != CatUnknown ||
		named == types.TypString || named == types.TypVoid ||
		named == types.TypNone || named.IsValueType() {
		return
	}
	thisAlloca, ok := c.locals["this"]
	if !ok {
		return
	}
	// T0562: val must be exactly {i8*, i8*}. After Optional/Tuple wrapping
	// (e.g., `Box? r = this.clone()`) val is `{i1, {i8*,i8*}}` and field 1 is
	// a struct, which would crash the icmp emit. Bail in those shapes — the
	// inner pointer is no longer at field 1.
	if !isUserValueStructType(val.Type()) {
		return
	}
	retInst := c.block.NewExtractValue(val, 1)
	thisVal := c.block.NewLoad(thisAlloca.ElemType, thisAlloca)
	var thisInst value.Value
	if structTy, isStruct := thisVal.Type().(*irtypes.StructType); isStruct && len(structTy.Fields) == 2 {
		thisInst = c.block.NewExtractValue(thisVal, 1)
	} else if thisVal.Type() == irtypes.I8Ptr {
		thisInst = thisVal
	} else {
		return
	}
	same := c.block.NewICmp(enum.IPredEQ, retInst, thisInst)

	clearBlk := c.newBlock("this.alias.clear")
	skipBlk := c.newBlock("this.alias.skip")
	c.block.NewCondBr(same, clearBlk, skipBlk)

	clearBlk.NewStore(constant.False, bindingFlag)
	clearBlk.NewBr(skipBlk)

	c.block = skipBlk
}

// maybeClearStructuralBindingAliasArg is the structural sibling of
// maybeClearBindingDropFlagOnThisAlias (T0347). It handles `r := pass_through(s)`
// where `pass_through(Sink s) Sink { return s; }` returns an owned
// structural-interface param BY VALUE. The concrete→structural coercion at the
// call site is a borrow, not a move, so the caller's `s` remains the sole owner
// of the heap box; but the binding path's maybeRegisterStructuralFree treats the
// CallExpr RHS as fresh-owned (isFreshOwnedStructuralRHS) and registers a
// structural free for `r` over the SAME box → two owners, one box → double free
// at scope exit. T1304.
//
// The general return-alias machinery (emitReturnAliasCheckSubst) deliberately
// skips structural returns (isTypeDroppable excludes IsStructural types), and it
// runs at call time — before the compiler knows the result will be bound — so the
// clear must happen here, at the binding site, and only for the binding case (the
// discard `pass_through(s);` case correctly leaves `s` as sole owner). This
// mirrors the `this` analog but keys off a call ARGUMENT instead of the receiver.
//
// For each function- or method-call argument that is a live owned local (has both
// a drop flag and an alloca), emit a runtime guard: if the result's instance ptr
// equals the arg's instance ptr AND the arg still owns its box (drop flag live),
// clear the binding's structural-free flag — leaving `s` sole owner. The pointer
// compare keeps a fresh-constructing return (`return Widget(...)`, distinct ptr)
// freeing independently (no spurious clear); the drop-flag gate keeps a moved arg
// (`pass_through(move s)`, flag already cleared) owned by `r` (no leak).
//
// T1308: the call receiver is deliberately excluded — for a method call it is not
// in `call.Args` (it is the MemberExpr Target), and its return-alias path is
// handled elsewhere (maybeClearReceiverDropFlag / wrapThisReturnValue). The
// arg-alias guard and the receiver-alias guard are independent runtime pointer
// compares; both may be emitted for one method call, and each fires only on a
// genuine pointer match.
func (c *Compiler) maybeClearStructuralBindingAliasArg(val value.Value, rhs ast.Expr, bindingFlag value.Value) {
	if bindingFlag == nil || c.block == nil || c.block.Term != nil {
		return
	}
	// val must be exactly {i8*, i8*} to extract the instance pointer at field 1.
	if !isUserValueStructType(val.Type()) {
		return
	}
	// Peel error/optional/paren wrappers to reach the underlying call (same shape
	// as isFreshOwnedStructuralRHS).
	inner := rhs
	for {
		switch e := inner.(type) {
		case *ast.ErrorPanicExpr:
			inner = e.Expr
			continue
		case *ast.OptionalUnwrapExpr:
			inner = e.Expr
			continue
		case *ast.ErrorPropagateExpr:
			inner = e.Expr
			continue
		case *ast.ErrorHandlerExpr:
			inner = e.Expr
			continue
		case *ast.ParenExpr:
			inner = e.Expr
			continue
		}
		break
	}
	call, ok := inner.(*ast.CallExpr)
	if !ok {
		return
	}
	// T1308: method calls are handled too, not just free-function calls. For a
	// MemberExpr callee, `call.Args` holds ONLY the parenthesized value arguments —
	// the receiver is `call.Callee.(*ast.MemberExpr).Target` and is never in
	// `call.Args`. So the arg loop below iterates exactly the method's owned value
	// args (e.g. `Sink s` in `f.make(s)`) and emits the same alias guard against
	// each. The receiver-vs-return alias (`return this` builders, `f` aliasing the
	// result) is a SEPARATE, independent concern already handled elsewhere by
	// maybeClearReceiverDropFlag / wrapThisReturnValue's clone (T0893/T1084), so it
	// needs no code here.

	retInst := c.block.NewExtractValue(val, 1)

	for _, arg := range call.Args {
		// Peel parens off the argument expression to reach a bare identifier.
		av := arg.Value
		for {
			if p, isParen := av.(*ast.ParenExpr); isParen {
				av = p.Expr
				continue
			}
			break
		}
		ident, isIdent := av.(*ast.IdentExpr)
		if !isIdent {
			continue
		}
		argFlag, hasFlag := c.dropFlags[ident.Name]
		if !hasFlag {
			continue
		}
		argAlloca, hasLocal := c.locals[ident.Name]
		if !hasLocal {
			continue
		}
		argVal := c.block.NewLoad(argAlloca.ElemType, argAlloca)
		argInst := extractAliasPtr(c, argVal)
		if argInst == nil {
			continue
		}
		same := c.block.NewICmp(enum.IPredEQ, retInst, argInst)
		flagLive := c.block.NewLoad(irtypes.I1, argFlag)
		cond := c.block.NewAnd(same, flagLive)

		clearBlk := c.newBlock("struct.arg.alias.clear")
		skipBlk := c.newBlock("struct.arg.alias.skip")
		c.block.NewCondBr(cond, clearBlk, skipBlk)
		clearBlk.NewStore(constant.False, bindingFlag)
		clearBlk.NewBr(skipBlk)
		c.block = skipBlk
	}
}

// maybeClearStructuralTempAliasArg is the discard/inline-use sibling of
// maybeClearStructuralBindingAliasArg (T1304). It handles the case where the
// result of a structural-interface-returning call is NOT bound to a variable —
// `f.make(s);` as a bare statement, or an inline use `f.make(s).emit(1);`. On
// this path maybeTrackIterTemp registers the result's instance ptr as an owned
// heap temp (via structuralDrop) that is freed at statement end. But when the
// callee returns an owned structural param BY VALUE (`make(Sink s) Sink { return
// s; }`), the concrete→structural coercion is a borrow: the caller's `s` remains
// the sole owner of the heap box. The tracked temp then frees the SAME box `s`
// drops at scope exit → double free. T1310.
//
// Unlike the binding sibling this admits a MemberExpr (method) callee: the temp
// is only ever created for method-call results (maybeTrackIterTemp is called from
// the method-call branch of genExpr's CallExpr case), and for a MemberExpr callee
// call.Args holds exactly the value arguments — the receiver is Callee.Target and
// never appears in Args, so scanning Args reaches only the passed-through owned
// arg, not the receiver. The receiver-vs-return alias remains a separate concern
// already handled by the claimHeapTemp receiver path.
//
// For each argument that is a live owned local (drop flag + alloca), emit a
// runtime guard: if the tracked temp's instance ptr equals the arg's instance ptr
// AND the arg still owns its box (drop flag live), clear the TEMP's drop flag so
// the arg stays sole owner. The pointer compare keeps a fresh-constructing return
// (`return Counter(...)`, distinct ptr) freeing independently; the drop-flag gate
// mirrors the T1304/T1308 shape and fails closed.
func (c *Compiler) maybeClearStructuralTempAliasArg(call *ast.CallExpr, tempInstPtr value.Value, tempDropFlag value.Value) {
	if call == nil || tempDropFlag == nil || tempInstPtr == nil || c.block == nil || c.block.Term != nil {
		return
	}
	if tempInstPtr.Type() != irtypes.I8Ptr {
		return
	}
	for _, arg := range call.Args {
		// Peel parens off the argument expression to reach a bare identifier.
		av := arg.Value
		for {
			if p, isParen := av.(*ast.ParenExpr); isParen {
				av = p.Expr
				continue
			}
			break
		}
		ident, isIdent := av.(*ast.IdentExpr)
		if !isIdent {
			continue
		}
		argFlag, hasFlag := c.dropFlags[ident.Name]
		if !hasFlag {
			continue
		}
		argAlloca, hasLocal := c.locals[ident.Name]
		if !hasLocal {
			continue
		}
		argVal := c.block.NewLoad(argAlloca.ElemType, argAlloca)
		argInst := extractAliasPtr(c, argVal)
		if argInst == nil || argInst.Type() != irtypes.I8Ptr {
			continue
		}
		same := c.block.NewICmp(enum.IPredEQ, tempInstPtr, argInst)
		flagLive := c.block.NewLoad(irtypes.I1, argFlag)
		cond := c.block.NewAnd(same, flagLive)

		clearBlk := c.newBlock("struct.tmp.alias.clear")
		skipBlk := c.newBlock("struct.tmp.alias.skip")
		c.block.NewCondBr(cond, clearBlk, skipBlk)
		clearBlk.NewStore(constant.False, tempDropFlag)
		clearBlk.NewBr(skipBlk)
		c.block = skipBlk
	}
}

// --- Raise ---

func (c *Compiler) genRaiseStmt(s *ast.RaiseStmt) {
	// T0110: Generate the raise value expression BEFORE scope cleanup.
	// Constructor expressions (e.g., raise MyError(message: msg)) move string
	// fields from local variables (clearing their drop flags). If scope cleanup
	// ran first, it would free those variables before the constructor could use them.
	errVal := c.genExpr(s.Value)

	// T0086: If raising a local error variable, clear its drop flag so
	// emitScopeCleanup won't free the instance we're about to return.
	if ident, ok := s.Value.(*ast.IdentExpr); ok {
		c.clearDropFlag(ident.Name)
	}
	// T0784: same for `raise x as!/as T` — the cast is a view, so without
	// this clear the subject's scope-exit drop fires on the same allocation
	// the error slot now owns → double-free.
	if ident := c.castSubjectMovableIdent(s.Value); ident != nil {
		// T0849: for the conditional `as` form, drop iff the downcast failed.
		c.consumeCastSubjectDropFlag(s.Value, ident.Name)
	}
	// T1073: `raise o!` — force-unwrap moves the inner error out of the source
	// optional into the error slot. Neutralize the source optional's present flag
	// (before the scope cleanup below) so its drop doesn't double-free the moved
	// inner.
	c.neutralizeForceUnwrapElem(s.Value)

	// T0962: Clean up statement temps before raising. The raise expression may
	// create intermediate temps that are normally freed at statement end (e.g.
	// `raise error(message: "x: " + ch.to_string())` produces a throwaway
	// `ch.to_string()` string). Since raise terminates the block, the
	// post-statement cleanup never runs and those intermediaries leak. Use the
	// error-path variants (T0103) — like emitFailableResultPropagation — which
	// emit flag-guarded frees WITHOUT resetting the tracking state: a raise can
	// be nested inside a larger statement (e.g. a match arm) whose sibling/ok
	// path still needs the temp list intact for its own end-of-statement
	// cleanup. Claim the raised value first so its instance and embedded message
	// survive; only the throwaways are freed.
	c.claimStringTemp(errVal)
	c.claimHeapTemp(errVal)
	if c.block != nil && c.block.Term == nil {
		c.emitAllStmtTempCleanupForErrorPath() // T1272: also frees env/enum temps
	}

	// Emit close() for all active use bindings before raising
	if len(c.scopeBindings) > 0 {
		c.emitScopeCleanup(0, true) // error in flight — suppress close errors
	}
	// Error types are user types with value struct {vtable_ptr, instance_ptr}.
	// Extract the instance pointer (i8*) for storage in the result struct's error slot.
	if st, ok := errVal.Type().(*irtypes.StructType); ok && len(st.Fields) == 2 {
		errVal = c.block.NewExtractValue(errVal, 1)
	}
	if c.inFailableGoBlock {
		// T1384: store the error into the goroutine's result aggregate and branch
		// to the coroutine's final suspend (a `ret` is invalid in the coro ramp).
		c.emitFailableGoBlockError(errVal)
	} else if c.inGenerator && c.generatorCanError {
		// B0023: store error to generator error_slot and branch to final suspend
		c.emitGeneratorError(errVal)
	} else {
		resultType := c.currentResultType()
		c.block.NewRet(c.wrapError(errVal, resultType))
	}
}

// --- If statement ---

func (c *Compiler) genIfStmt(s *ast.IfStmt) {
	if s.Binding != "" {
		c.genIfUnwrapStmt(s)
		return
	}

	// Check for optional narrowing
	if narrow := c.info.OptionalNarrowings[s]; narrow != nil {
		c.genIfNarrowStmt(s, narrow)
		return
	}

	// Check for destructure is-pattern narrowing
	if destructNarrow := c.info.IsDestructureNarrowings[s]; destructNarrow != nil {
		c.genIfDestructureIsStmt(s, destructNarrow)
		return
	}

	cond := c.genExpr(s.Cond)

	thenBlock := c.newBlock("if.then")
	mergeBlock := c.newBlock("if.end")

	var elseBlock *ir.Block
	if s.Else != nil {
		elseBlock = c.newBlock("if.else")
		c.block.NewCondBr(cond, thenBlock, elseBlock)
	} else {
		c.block.NewCondBr(cond, thenBlock, mergeBlock)
	}

	// B0173: Save heap/env temps from the condition expression so branches don't
	// prematurely clean them. Cleanup runs once in the merge block.
	savedHeapTemps := c.heapTemps
	savedHeapTempMap := c.heapTempMap
	c.heapTemps = nil
	c.heapTempMap = make(map[value.Value]int)
	savedEnvTempsIf := c.envTemps     // T0100
	savedEnvTempMapIf := c.envTempMap // T0100
	c.envTemps = nil
	c.envTempMap = make(map[value.Value]int)

	// B0198: Save condition's string temps so branches don't permanently clear them.
	// Branches see the condition temps (for cleanup on return paths), but after each
	// branch we restore from the snapshot so the next branch and merge block also
	// emit flag-guarded cleanup. The flag system prevents double-free: if a branch
	// already dropped the temp, its flag is cleared and merge-block cleanup is a no-op.
	savedCondStmtTemps := append([]stmtTemp(nil), c.stmtTemps...)
	savedCondStmtTempMap := make(map[value.Value]int, len(c.stmtTempMap))
	for k, v := range c.stmtTempMap {
		savedCondStmtTempMap[k] = v
	}

	// Then branch
	c.block = thenBlock
	if c.shouldInstrument() {
		pos := s.Body.Pos()
		end := s.Body.End()
		idx := c.addCoverageRegion(pos.File, pos.Line, end.Line, c.currentCoverageFuncName(), "if.then")
		c.emitCoverageIncrement(idx)
	}
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Else branch
	if s.Else != nil {
		// B0198: Restore condition temps so else-branch can also emit cleanup.
		c.stmtTemps = append([]stmtTemp(nil), savedCondStmtTemps...)
		c.stmtTempMap = make(map[value.Value]int, len(savedCondStmtTempMap))
		for k, v := range savedCondStmtTempMap {
			c.stmtTempMap[k] = v
		}
		c.block = elseBlock
		if c.shouldInstrument() {
			pos := s.Else.Pos()
			end := s.Else.End()
			idx := c.addCoverageRegion(pos.File, pos.Line, end.Line, c.currentCoverageFuncName(), "if.else")
			c.emitCoverageIncrement(idx)
		}
		c.genStmt(s.Else)
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}
	}

	c.block = mergeBlock

	// B0198: Restore condition's string temps for merge-block cleanup.
	// The normal statement-end cleanupStmtTemps() will emit flag-guarded
	// cleanup IR here, covering the false-path where no branch ran.
	c.stmtTemps = savedCondStmtTemps
	c.stmtTempMap = savedCondStmtTempMap

	// B0173: Restore heap/env temps and clean up in the merge block.
	// T1329: when this if-STATEMENT is a leading statement inside a block-value
	// body, the enclosing expression's sibling heap/env temps sit below the floor
	// and must survive to the enclosing merge — drain only the suffix created at/
	// after the if. 0 outside a block-value body → full drain (unchanged).
	c.heapTemps = savedHeapTemps
	c.heapTempMap = savedHeapTempMap
	c.cleanupHeapTempsFrom(c.blockTempFloorHeap)
	c.envTemps = savedEnvTempsIf     // T0100
	c.envTempMap = savedEnvTempMapIf // T0100
	c.cleanupEnvTempsFrom(c.blockTempFloorEnv)
}

// genIfStmtValue generates an if/else statement in value-producing position
// (e.g., as the last statement in a block body of a match arm). Returns the
// phi of both branch values, or nil if the if/else cannot produce a value
// (no else, if-unwrap, optional narrowing, etc.). B0126.
func (c *Compiler) genIfStmtValue(s *ast.IfStmt) value.Value {
	// Only handle simple if/else — not if-unwrap, narrowing, or if without else.
	if s.Binding != "" || s.Else == nil {
		c.genIfStmt(s)
		return nil
	}
	if c.info.OptionalNarrowings[s] != nil || c.info.IsDestructureNarrowings[s] != nil {
		c.genIfStmt(s)
		return nil
	}

	cond := c.genExpr(s.Cond)

	thenBlock := c.newBlock("if.then")
	elseBlock := c.newBlock("if.else")
	mergeBlock := c.newBlock("if.end")

	c.block.NewCondBr(cond, thenBlock, elseBlock)

	// Then branch — capture value
	c.block = thenBlock
	thenVal := c.genBlockValue(s.Body)
	thenOwned := c.blockValueOwnedResult   // T1206: genBlockValue recorded ownership
	thenOwnedFlag := c.blockValueOwnedFlag // T1208: live per-path flag (nested tracked temp)
	c.claimStringTemp(thenVal)             // T0073
	thenEnd := c.block
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Else branch — capture value
	c.block = elseBlock
	var elseVal value.Value
	elseOwned := false // T1206
	var elseOwnedFlag value.Value
	switch e := s.Else.(type) {
	case *ast.Block:
		elseVal = c.genBlockValue(e)
		elseOwned = c.blockValueOwnedResult
		elseOwnedFlag = c.blockValueOwnedFlag // T1208
	case *ast.IfStmt:
		elseVal = c.genIfStmtValue(e)
		// A recursive else-if returns a tracked owned temp when it transfers
		// ownership; detect it before claimStringTemp neutralizes the flag.
		if elseVal != nil {
			if idx, ok := c.stmtTempMap[elseVal]; ok && idx >= 0 {
				elseOwned = true
				elseOwnedFlag = c.captureLiveTempFlag(elseVal) // T1208
			} else if c.resultIsFreshOwnedHeapTemp(elseVal) {
				// T1211: a recursive else-if whose selected arm is a fresh owned heap
				// value struct (heap-user-type / Map) transfers ownership up — tracked
				// as a heapTemp, so the stmtTempMap check above misses it.
				elseOwned = true
			} else if flag := c.captureLiveTempFlag(elseVal); flag != nil {
				// T1211: a recursive else-if that is itself a mixed owned/borrowed
				// value-struct merge carries its per-path flag in mergeBoundStructFlag;
				// thread it up so the enclosing merge's flag phi (and thus the bound
				// local's drop flag) stays conditional — otherwise the owned inner arm
				// leaks (constant 0) or a borrowed inner arm double-frees (constant 1).
				elseOwned = true
				elseOwnedFlag = flag
			}
		}
	default:
		c.genStmt(s.Else)
	}
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

	// Build phi from branches that reach mergeBlock with values.
	// One branch may return/diverge, leaving only the other to contribute.
	var incomings []*ir.Incoming
	if thenVal != nil {
		if br, ok := thenEnd.Term.(*ir.TermBr); ok && br.Target == mergeBlock {
			incomings = append(incomings, &ir.Incoming{X: thenVal, Pred: thenEnd})
		}
	}
	if elseVal != nil {
		if br, ok := elseEnd.Term.(*ir.TermBr); ok && br.Target == mergeBlock {
			incomings = append(incomings, &ir.Incoming{X: elseVal, Pred: elseEnd})
		}
	}
	if len(incomings) > 0 {
		phi := mergeBlock.NewPhi(incomings...)
		// T1206: register an owned i8* phi as a statement temp (mirrors genIfExpr's
		// trackMergeResultTemp) so a value-producing `if` in STATEMENT position —
		// reached when it is the last statement of a value block, e.g. nested inside
		// an outer `if a { if b { local } else { "x".clone() } } else { ... }` — frees
		// its selected owned result exactly once. Without this, genIfStmtValue (the
		// pre-T1107 lowering path) bypassed the ownership machinery entirely and the
		// selected clone/owned-local leaked. A bound/return consumer claims the phi.
		resultType := c.ifStmtValueResultType(s)
		c.trackMergeResultTemp(phi, resultType, []matchArmInfo{
			{end: thenEnd, owned: thenOwned, ownedFlag: thenOwnedFlag},
			{end: elseEnd, owned: elseOwned, ownedFlag: elseOwnedFlag},
		})
		return phi
	}

	return nil
}

// ifStmtValueResultType resolves the sema result type of a value-producing
// if-statement from its branch bodies' last-expression types (T1206). Used to
// pick the owned-i8*-result drop function in trackMergeResultTemp. Prefers the
// then-branch, then a Block else-branch, then recurses into an else-if chain —
// so a shape where BOTH the then-body's last statement and the else are
// themselves value-ifs (no direct ExprStmt to read the type from) still resolves
// its owned-i8* result and gets a statement-end drop instead of leaking.
func (c *Compiler) ifStmtValueResultType(s *ast.IfStmt) types.Type {
	if t := c.blockResultType(s.Body); t != nil {
		return t
	}
	switch e := s.Else.(type) {
	case *ast.Block:
		return c.blockResultType(e)
	case *ast.IfStmt:
		return c.ifStmtValueResultType(e)
	}
	return nil
}

// blockResultType returns the substituted sema type of a block's result value —
// the last statement's expression when it is an ExprStmt, or (recursively) the
// result type of a trailing value-producing if-statement, else nil (T1206).
func (c *Compiler) blockResultType(block *ast.Block) types.Type {
	if block == nil || len(block.Stmts) == 0 {
		return nil
	}
	switch s := block.Stmts[len(block.Stmts)-1].(type) {
	case *ast.ExprStmt:
		t := c.info.Types[s.Expr]
		if c.typeSubst != nil && t != nil {
			t = types.Substitute(t, c.typeSubst)
		}
		return t
	case *ast.IfStmt:
		return c.ifStmtValueResultType(s)
	}
	return nil
}

// genIfNarrowStmt handles if-statements that narrow optional variables.
// Supports single narrowing, compound narrowing (&&), and negated narrowing (!cc).
func (c *Compiler) genIfNarrowStmt(s *ast.IfStmt, narrow *sema.OptionalNarrowing) {
	if narrow.Negated {
		c.genNegatedNarrowStmt(s, narrow)
		return
	}
	if len(narrow.Vars) > 1 {
		c.genCompoundNarrowStmt(s, narrow)
		return
	}

	// Single variable narrowing
	v := narrow.Vars[0]
	alloca := c.locals[v.VarName]
	optVal := c.block.NewLoad(alloca.ElemType, alloca)
	flag := c.block.NewExtractValue(optVal, 0)

	thenBlock := c.newBlock("narrow.then")
	mergeBlock := c.newBlock("narrow.end")

	var elseBlock *ir.Block
	if s.Else != nil {
		elseBlock = c.newBlock("narrow.else")
		c.block.NewCondBr(flag, thenBlock, elseBlock)
	} else {
		c.block.NewCondBr(flag, thenBlock, mergeBlock)
	}

	// Then: shadow the variable with the unwrapped inner value
	c.block = thenBlock
	innerVal := c.block.NewExtractValue(optVal, 1)
	innerAlloca := c.createEntryAlloca(innerVal.Type()) // B0153: must be in entry block
	c.block.NewStore(innerVal, innerAlloca)
	prev := c.locals[v.VarName]
	c.locals[v.VarName] = innerAlloca

	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}
	c.locals[v.VarName] = prev

	if s.Else != nil {
		c.block = elseBlock
		c.genStmt(s.Else)
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}
	}

	c.block = mergeBlock
}

// genNegatedNarrowStmt handles `if !cc { A } else { B }` — narrowing in else branch.
func (c *Compiler) genNegatedNarrowStmt(s *ast.IfStmt, narrow *sema.OptionalNarrowing) {
	v := narrow.Vars[0]
	alloca := c.locals[v.VarName]
	optVal := c.block.NewLoad(alloca.ElemType, alloca)
	flag := c.block.NewExtractValue(optVal, 0)

	thenBlock := c.newBlock("narrow.then")
	mergeBlock := c.newBlock("narrow.end")

	var elseBlock *ir.Block
	if s.Else != nil {
		elseBlock = c.newBlock("narrow.else")
	}

	// flag=true (present) → else (narrowed), flag=false (absent) → then (not narrowed)
	if s.Else != nil {
		c.block.NewCondBr(flag, elseBlock, thenBlock)
	} else {
		c.block.NewCondBr(flag, mergeBlock, thenBlock)
	}

	// Then: cc is none — no narrowing
	c.block = thenBlock
	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Else: cc is present — shadow with unwrapped value
	if s.Else != nil {
		c.block = elseBlock
		innerVal := c.block.NewExtractValue(optVal, 1)
		innerAlloca := c.createEntryAlloca(innerVal.Type()) // B0153: must be in entry block
		c.block.NewStore(innerVal, innerAlloca)
		prev := c.locals[v.VarName]
		c.locals[v.VarName] = innerAlloca

		c.genStmt(s.Else)
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}
		c.locals[v.VarName] = prev
	}

	c.block = mergeBlock

	// Post-divergence narrowing: if the then-body diverges and there's no else,
	// we know the variable is present at the merge point. Shadow it with the
	// unwrapped inner value for all subsequent code.
	if narrow.PostNarrow {
		innerVal := c.block.NewExtractValue(optVal, 1)
		innerAlloca := c.createEntryAlloca(innerVal.Type()) // B0153: must be in entry block
		c.block.NewStore(innerVal, innerAlloca)
		c.locals[v.VarName] = innerAlloca
	}
}

// genCompoundNarrowStmt handles `if a && b { ... }` — both narrowed in then-block.
// Generates nested flag checks with short-circuit evaluation.
func (c *Compiler) genCompoundNarrowStmt(s *ast.IfStmt, narrow *sema.OptionalNarrowing) {
	mergeBlock := c.newBlock("narrow.end")
	var elseBlock *ir.Block
	if s.Else != nil {
		elseBlock = c.newBlock("narrow.else")
	}

	// Load all optional values and chain flag checks
	type optInfo struct {
		optVal value.Value
		v      sema.NarrowedVar
	}
	opts := make([]optInfo, len(narrow.Vars))
	for i, v := range narrow.Vars {
		alloca := c.locals[v.VarName]
		optVal := c.block.NewLoad(alloca.ElemType, alloca)
		flag := c.block.NewExtractValue(optVal, 0)
		opts[i] = optInfo{optVal: optVal, v: v}

		if i < len(narrow.Vars)-1 {
			// Not the last: chain to next check
			nextCheck := c.newBlock(fmt.Sprintf("narrow.check.%d", i+1))
			failTarget := elseBlock
			if failTarget == nil {
				failTarget = mergeBlock
			}
			c.block.NewCondBr(flag, nextCheck, failTarget)
			c.block = nextCheck
		} else {
			// Last: branch to then or else/merge
			thenBlock := c.newBlock("narrow.then")
			failTarget := elseBlock
			if failTarget == nil {
				failTarget = mergeBlock
			}
			c.block.NewCondBr(flag, thenBlock, failTarget)
			c.block = thenBlock
		}
	}

	// Then: shadow all variables with unwrapped values
	prevLocals := make(map[string]*ir.InstAlloca, len(opts))
	for _, info := range opts {
		innerVal := c.block.NewExtractValue(info.optVal, 1)
		innerAlloca := c.createEntryAlloca(innerVal.Type()) // B0153: must be in entry block
		c.block.NewStore(innerVal, innerAlloca)
		prevLocals[info.v.VarName] = c.locals[info.v.VarName]
		c.locals[info.v.VarName] = innerAlloca
	}

	c.genBlock(s.Body)
	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Restore all
	for name, prev := range prevLocals {
		c.locals[name] = prev
	}

	if s.Else != nil {
		c.block = elseBlock
		c.genStmt(s.Else)
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}
	}

	c.block = mergeBlock
}

// genIfDestructureIsStmt handles if-statements with destructure is-patterns.
// Generates a type/variant check, then extracts fields into bindings in the then-block.
func (c *Compiler) genIfDestructureIsStmt(s *ast.IfStmt, narrow *sema.IsDestructureNarrowing) {
	subject := c.genExpr(narrow.SubjectExpr)

	// B0112: apply type substitution to TargetType for generic method bodies
	targetType := narrow.TargetType
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	var cond value.Value
	if narrow.IsEnum {
		// Enum variant check: compare tag
		enumLayout := c.lookupEnumLayout(targetType)
		if enumLayout == nil {
			panic(fmt.Sprintf("codegen: no enum layout for %s", targetType))
		}
		// A `this` enum receiver is an i8* pointer — load the value so both the tag
		// check below and the field binding in bindIsDestructureEnum operate on the
		// by-value enum. (Non-enum/RTTI branch keeps the raw i8* `this`.)
		subject = c.enumThisSubject(subject, enumLayout)
		var tag value.Value
		if enumLayout.MaxVariantDataSize == 0 {
			tag = subject // fieldless enum: value IS the tag
		} else {
			tag = c.block.NewExtractValue(subject, 0)
		}
		expectedTag := constant.NewInt(irtypes.I32, int64(enumLayout.VariantTag[narrow.VariantName]))
		cond = c.block.NewICmp(enum.IPredEQ, tag, expectedTag)
	} else {
		// Named/Instance type check via RTTI
		targetID, ok := c.resolveTypeID(targetType)
		if !ok {
			targetNamed := extractNamed(targetType)
			if targetNamed == nil {
				panic(fmt.Sprintf("codegen: cannot extract Named from %s", targetType))
			}
			targetID = c.assignTypeID(targetNamed)
		}
		// For value types, use the compile-time-known RTTI global (no field in value struct).
		subjectType := c.info.Types[narrow.SubjectExpr]
		if c.typeSubst != nil {
			subjectType = types.Substitute(subjectType, c.typeSubst)
		}
		instance := c.instancePtrForRTTI(subject, subjectType)
		variantPtr := c.loadVariantPtr(instance)
		result := c.block.NewCall(c.funcs["promise_type_is"],
			variantPtr, constant.NewInt(irtypes.I32, int64(targetID)))
		cond = c.block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I32, 0))
	}

	thenBlock := c.newBlock("isdestr.then")
	mergeBlock := c.newBlock("isdestr.end")

	var elseBlock *ir.Block
	if s.Else != nil {
		elseBlock = c.newBlock("isdestr.else")
		c.block.NewCondBr(cond, thenBlock, elseBlock)
	} else {
		c.block.NewCondBr(cond, thenBlock, mergeBlock)
	}

	// Then: extract fields and bind them
	c.block = thenBlock

	// Save previous locals that might be shadowed by bindings.
	// T1012: also snapshot each binding's matchBorrowedIdents membership —
	// bindIsDestructureEnum may mark an Optional/Array payload binding as
	// match-borrowed (T0485), and that mark must not leak past the then-block to
	// a later same-named binding (mirrors the per-arm snapshot in genEnumMatch).
	type savedLocal struct {
		name      string
		val       *ir.InstAlloca
		had       bool
		wasBorrow bool
	}
	var saved []savedLocal
	for _, b := range narrow.Bindings {
		if b.VarName != "_" {
			prev, had := c.locals[b.VarName]
			wasBorrow := c.matchBorrowedIdents != nil && c.matchBorrowedIdents[b.VarName]
			saved = append(saved, savedLocal{b.VarName, prev, had, wasBorrow})
		}
	}

	// T1012/T1169: capture the scope-binding watermark before binding. When the
	// enum (bindIsDestructureEnum) or the named subtype (bindIsDestructureNamed)
	// is droppable and a heap field is dup'd for escape safety, the dup's drop
	// binding is appended here — BEFORE genBlock captures its own savedScopeLen —
	// so genBlock won't clean it on the fall-through path. We clean [watermark:]
	// ourselves at the then-block fall-through terminator below (escape paths
	// inside the body already walk emitScopeCleanup down to 0).
	bindWatermark := len(c.scopeBindings)
	if narrow.IsEnum {
		c.bindIsDestructureEnum(subject, narrow)
	} else {
		c.bindIsDestructureNamed(subject, narrow)
	}

	c.genBlock(s.Body)

	// Restore previous locals and borrow marks
	for _, s := range saved {
		if s.had {
			c.locals[s.name] = s.val
		} else {
			delete(c.locals, s.name)
		}
		// T1012: restore matchBorrowedIdents to its pre-binding state.
		if s.wasBorrow {
			if c.matchBorrowedIdents == nil {
				c.matchBorrowedIdents = make(map[string]bool)
			}
			c.matchBorrowedIdents[s.name] = true
		} else if c.matchBorrowedIdents != nil {
			delete(c.matchBorrowedIdents, s.name)
		}
	}

	if c.block.Term == nil {
		// T1012: free any dup'd destructure bindings at then-block exit
		// (scope-accurate; zero-leak). No-op when nothing was registered.
		if len(c.scopeBindings) > bindWatermark {
			c.emitScopeCleanup(bindWatermark, false)
		}
		c.block.NewBr(mergeBlock)
	}
	c.scopeBindings = c.scopeBindings[:bindWatermark]

	// Else branch
	if s.Else != nil {
		c.block = elseBlock
		c.genStmt(s.Else)
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}
	}

	c.block = mergeBlock
}

// bindIsDestructureEnum extracts enum variant data fields and binds them to local variables.
//
// T1012: mirrors the match path's dup-and-own semantics (bindEnumDestructure).
// A raw GEP+load binding merely ALIASES the subject's payload — safe for an
// in-scope read (the subject's synth enum drop frees it once) but a use-after-
// free when a heap-typed (string/vector) binding escapes the narrowing scope
// (return / store-to-outer / consuming arg / constructor field), since the
// subject is dropped at scope exit. When the enum is droppable and a field
// needs dup, deep-clone the payload and register a drop for the binding (the
// clearDropFlag move-site machinery clears that flag on escape, so the escaped
// value is owned by its consumer and the subject's synth drop still frees the
// original exactly once). Value/numeric payloads stay zero-copy.
//
// Deliberately does NOT port the T0623 armMoves / nullSubjectHandleSlot logic:
// `if is` narrowing is a pure borrow of the subject (the subject stays live
// after the `if`), so the subject's variant slot must never be nulled.
func (c *Compiler) bindIsDestructureEnum(subject value.Value, narrow *sema.IsDestructureNarrowing) {
	// B0112: apply type substitution for generic method bodies
	targetType := narrow.TargetType
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	enumLayout := c.lookupEnumLayout(targetType)
	dataType := enumLayout.VariantDataTypes[narrow.VariantName]
	if dataType == nil {
		return
	}

	// T1012: resolve the enum origin + variant (declared, pre-substitution field
	// types) and whether the subject enum is droppable — the dup helpers below
	// expect the declared field type + subject type + enum, exactly as the match
	// path (bindEnumDestructure) does.
	enum := extractEnum(targetType)
	var variant *types.Variant
	enumHasDrop := false
	if enum != nil {
		variant = enum.LookupVariant(narrow.VariantName)
		enumHasDrop = c.enumInstanceHasDrop(targetType, enum)
	}

	internalType := enumLayout.EnumInternalType.(*irtypes.StructType)
	alloca := c.createEntryAlloca(internalType)
	c.block.NewStore(subject, alloca)

	dataPtr := c.block.NewGetElementPtr(internalType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	typedDataPtr := c.block.NewBitCast(dataPtr, irtypes.NewPointer(dataType))

	for i, b := range narrow.Bindings {
		if b.VarName == "_" {
			continue
		}
		if i >= len(dataType.Fields) {
			break
		}
		fieldType := dataType.Fields[i]
		fieldPtr := c.block.NewGetElementPtr(dataType, typedDataPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
		val := c.block.NewLoad(fieldType, fieldPtr)

		// T1012: dup droppable heap payloads so the binding owns an independent
		// copy that escapes safely (match-parity via the shared helpers). Value/
		// numeric payloads (matchFieldNeedsDup == false) stay zero-copy.
		if enumHasDrop && variant != nil && i < variant.NumFields() {
			declaredFieldType := variant.Fields()[i].Type()
			resolved := c.resolveMatchFieldType(declaredFieldType, targetType, enum)
			// T1012: a single-owner-handle payload (Task/Mutex/MutexGuard, possibly
			// nested) is NOT dup-cloneable — cloneResolvedValue would produce an
			// invalid copy (crash). `if is` narrowing is a pure borrow of the
			// subject (unlike match, this path intentionally skips the T0623
			// move-out), so keep such a field a plain non-owning alias: the subject
			// drops the handle exactly once. matchFieldNeedsDup can report true for
			// a handle-bearing field (the match path relies on its T0623 branch
			// running first), so this guard must precede the dup branch.
			if sema.FirstNestedSingleOwnerHandle(resolved) == nil &&
				!c.suppressMatchDup && c.matchFieldNeedsDup(declaredFieldType, targetType, enum) {
				c.dupMatchBinding(b.VarName, val, fieldType, resolved)
				continue
			}
			// T0485/T1179: an Optional/Array payload binding aliases the variant
			// data (which the synth enum drop owns). Mark it match-borrowed (via
			// markMatchBorrowedBinding, shared with the match-arm path in
			// bindEnumDestructure) so a later `if x := optBinding`-style unwrap
			// doesn't double-transfer ownership and a plain var-decl of the whole
			// payload (`T[N] copy = value;`) clones exactly once instead of taking
			// an owning drop that would double-free with the synth enum drop.
			c.markMatchBorrowedBinding(b.VarName, resolved)
		}

		bindAlloca := c.createEntryAlloca(fieldType)
		c.block.NewStore(val, bindAlloca)
		c.locals[b.VarName] = bindAlloca
	}
}

// bindIsDestructureNamed extracts named type fields and binds them to local variables.
func (c *Compiler) bindIsDestructureNamed(subject value.Value, narrow *sema.IsDestructureNarrowing) {
	// B0112: apply type substitution for generic method bodies
	targetType := narrow.TargetType
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	targetNamed := extractNamed(targetType)
	layout := c.lookupTypeLayout(targetType)
	if layout == nil {
		panic(fmt.Sprintf("codegen: no layout for type %s", targetType))
	}

	// For heap types, extract instance pointer once before the loop.
	// Value types don't use instance pointers — fields are in the value struct.
	var instancePtr value.Value
	if !layout.IsValueType {
		instancePtr = c.extractInstancePtr(subject)
	}

	// T1169: whether the subject subtype is droppable — gates the escape-safe dup
	// below (mirrors the enum path's enumHasDrop gate in bindIsDestructureEnum).
	ownerDroppable := c.ownerHasOrSynthDrop(targetType, targetNamed)

	allFields := targetNamed.AllFields()
	for i, b := range narrow.Bindings {
		if b.VarName == "_" {
			continue
		}
		if i >= len(allFields) {
			break
		}
		field := allFields[i]

		if layout.IsValueType {
			// Value type: fields are in value struct
			fieldIdx, ok := layout.ValueFieldIndex[field.Name()]
			if !ok {
				continue
			}
			// Extract field directly from the subject value struct.
			// Value types have only `value` fields (no heap/drop) — always
			// zero-copy, so no dup/borrow-mark is needed here (T1169).
			fieldVal := c.block.NewExtractValue(subject, uint64(fieldIdx))
			bindAlloca := c.createEntryAlloca(fieldVal.Type())
			c.block.NewStore(fieldVal, bindAlloca)
			c.locals[b.VarName] = bindAlloca
		} else {
			// Heap type: fields in instance struct
			fieldIdx, ok := layout.InstanceFieldIndex[field.Name()]
			if !ok {
				continue
			}
			typedPtr := c.block.NewBitCast(instancePtr, layout.InstancePtrType)
			fieldPtr := c.block.NewGetElementPtr(layout.Instance.LLVMType, typedPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
			fieldVal := c.block.NewLoad(layout.Instance.Fields[fieldIdx].LLVMType, fieldPtr)

			// T1169: a raw GEP+load binding merely ALIASES the subject's instance
			// field — safe for an in-scope read (the subject's drop frees it once)
			// but a use-after-free when a droppable heap-typed (string/vector/…)
			// binding escapes the narrowing scope (return / store-to-outer /
			// consuming arg / constructor field), since the subject is dropped at
			// scope exit. Mirror bindIsDestructureEnum: when the subtype is
			// droppable and the field needs dup, deep-clone it and register a drop
			// (clearDropFlag clears the flag at move sites, so the escaped value is
			// owned by its consumer and the subject's drop still frees the original
			// exactly once). Value/numeric fields (typeNeedsMatchDup == false) stay
			// zero-copy.
			if ownerDroppable {
				// Resolve the field type exactly as genFieldAccess does so a generic
				// subtype field (e.g. T on Box[string]) resolves to its concrete type.
				resolved := field.Type()
				if c.typeSubst != nil {
					resolved = types.Substitute(resolved, c.typeSubst)
				}
				if inst, ok := targetType.(*types.Instance); ok {
					if origin, ok := inst.Origin().(*types.Named); ok && len(origin.TypeParams()) > 0 {
						localSubst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
						resolved = types.Substitute(resolved, localSubst)
					}
				}
				// A single-owner-handle field (Task/Mutex/MutexGuard, possibly nested)
				// is NOT dup-cloneable; `if is` is a pure borrow so the subject drops
				// the handle exactly once — keep it a plain non-owning alias.
				if sema.FirstNestedSingleOwnerHandle(resolved) == nil && !c.suppressMatchDup &&
					(c.typeNeedsMatchDup(resolved) || c.enumMatchDupSafe(resolved, nil)) {
					c.dupMatchBinding(b.VarName, fieldVal, fieldVal.Type(), resolved)
					continue
				}
				// T0485/T1170/T1174: an Optional/Array field binding aliases the
				// subject's instance field (which the subject's drop owns). Mark it
				// match-borrowed so escape sites (dupBorrowedHeapUserPayload) dup it
				// and a later unwrap doesn't double-transfer ownership.
				if c.matchBindingIsBorrow(resolved) {
					if c.matchBorrowedIdents == nil {
						c.matchBorrowedIdents = make(map[string]bool)
					}
					c.matchBorrowedIdents[b.VarName] = true
				}
			}

			bindAlloca := c.createEntryAlloca(fieldVal.Type())
			c.block.NewStore(fieldVal, bindAlloca)
			c.locals[b.VarName] = bindAlloca
		}
	}
}

// genIfUnwrapStmt handles if-unwrap: if val := optExpr { } else { }
// Evaluates the optional, checks the present flag, binds the unwrapped value in the then block.
func (c *Compiler) genIfUnwrapStmt(s *ast.IfStmt) {
	// T0397: When unwrapping a Map[K, (droppable, ...)] index, the inner tuple
	// aliases the container's bucket data. Setting dupTupleFieldAccess here
	// causes genMethodIndex to deep-clone the tuple so the binding takes
	// ownership of an independent copy.
	dupInitType := c.info.Types[s.Init]
	if c.typeSubst != nil {
		dupInitType = types.Substitute(dupInitType, c.typeSubst)
	}
	if opt, ok := dupInitType.(*types.Optional); ok {
		elem := opt.Elem()
		if c.typeSubst != nil {
			elem = types.Substitute(elem, c.typeSubst)
		}
		if _, isTup := elem.(*types.Tuple); isTup && c.tupleNeedsDrop(elem) {
			c.dupTupleFieldAccess = true
		}
		// T0440: Same dup-on-read for Optional[heap-user-type] — the inner
		// value aliases the container's bucket; without dupping, the if-let
		// binding's drop would free the same instance the container drops.
		if isDroppableHeapUserType(elem) {
			c.dupHeapUserFieldAccess = true
		}
	}
	optVal := c.genExpr(s.Init)
	c.dupTupleFieldAccess = false
	c.dupHeapUserFieldAccess = false

	// T0770: When the scrutinee is a failable call (e.g. `if e := load()` where
	// `load!() T?`), auto-propagate the error first so optVal is the unwrapped
	// success value (the `T?` optional), not the raw failable result struct.
	// Without this the if-let reads the failable result's error flag as the
	// optional's present flag and binds the whole optional as the inner value.
	if c.info.AutoPropagateExprs[s.Init] {
		optVal = c.genAutoPropagateValue(optVal)
	}

	// Guard: if the expression is not an optional struct (e.g., post-narrowing
	// made it a plain value), treat the if as always-true with no unwrapping.
	// Bind the value directly to the unwrap variable name.
	if _, ok := optVal.Type().(*irtypes.StructType); !ok {
		if s.Binding != "" && s.Binding != "_" {
			alloca := c.createEntryAlloca(optVal.Type()) // B0153: must be in entry block
			c.block.NewStore(optVal, alloca)
			prev, had := c.locals[s.Binding]
			c.locals[s.Binding] = alloca
			c.genBlock(s.Body)
			if had {
				c.locals[s.Binding] = prev
			} else {
				delete(c.locals, s.Binding)
			}
		} else {
			c.genBlock(s.Body)
		}
		return
	}

	// Extract flag (field 0 of { i1, T } struct)
	flag := c.block.NewExtractValue(optVal, 0)

	thenBlock := c.newBlock("ifunwrap.then")
	mergeBlock := c.newBlock("ifunwrap.end")

	var elseBlock *ir.Block
	if s.Else != nil {
		elseBlock = c.newBlock("ifunwrap.else")
		c.block.NewCondBr(flag, thenBlock, elseBlock)
	} else {
		c.block.NewCondBr(flag, thenBlock, mergeBlock)
	}

	// B0173: Save heap/env temps from the init expression so branches don't
	// prematurely clean them. Cleanup runs once in the merge block.
	savedHeapTemps := c.heapTemps
	savedHeapTempMap := c.heapTempMap
	c.heapTemps = nil
	c.heapTempMap = make(map[value.Value]int)
	savedEnvTempsUW := c.envTemps     // T0100
	savedEnvTempMapUW := c.envTempMap // T0100
	c.envTemps = nil
	c.envTempMap = make(map[value.Value]int)

	// Then: unwrap value, bind to local (scoped to then-block only)
	c.block = thenBlock
	innerVal := c.block.NewExtractValue(optVal, 1)
	innerType := innerVal.Type()
	alloca := c.createEntryAlloca(innerType) // B0153: must be in entry block
	alloca.SetName(c.uniqueLocalName(s.Binding))
	c.block.NewStore(innerVal, alloca)
	prev, hadPrev := c.locals[s.Binding]
	c.locals[s.Binding] = alloca

	// B0215: Register drop binding for the unwrapped inner value when uniquely
	// owned. Function calls and force-unwraps return owned values; local
	// IdentExpr variables transfer ownership (we clear their drop flag).
	// Field access (MemberExpr) on droppable types is skipped — the parent
	// type's drop handles cleanup of the field's inner value.
	unwrapScopeLen := len(c.scopeBindings)
	savedDropFlag, hadDropFlag := c.dropFlags[s.Binding]
	savedDropBinding, hadDropBinding := c.dropBindings[s.Binding]
	// T0512: Snapshot the match-borrow marker for this binding so a marker
	// propagated through this if-let (below) is reverted at body end —
	// same lifetime as the drop flag/binding save/restore. Safe on nil map.
	savedBorrowMark, hadBorrowMark := c.matchBorrowedIdents[s.Binding]
	initType := c.info.Types[s.Init]
	if c.typeSubst != nil {
		initType = types.Substitute(initType, c.typeSubst)
	}
	if opt, ok := initType.(*types.Optional); ok && c.isOwnedOptionalExpr(s.Init) {
		// T0391: When unwrapping a nested Optional (T?? → T?), the element type is
		// itself Optional and needs an Optional drop binding so its inner heap value
		// is freed at scope exit (or transferred ownership to a further unwrap).
		elemType := opt.Elem()
		if c.typeSubst != nil {
			elemType = types.Substitute(elemType, c.typeSubst)
		}
		// T0585: For an IdentExpr source, load its drop flag value before
		// maybeRegister* / clearDropFlag so we can mirror the source's ownership
		// state into the binding. A borrowed source (no flag) means the unwrapped
		// binding is also a borrow (flag=0); without this, the binding would
		// incorrectly claim ownership and double-free the heap value at scope exit.
		var srcFlagVal value.Value
		if ident, isIdent := s.Init.(*ast.IdentExpr); isIdent {
			if srcFlag, has := c.dropFlags[ident.Name]; has {
				srcFlagVal = c.block.NewLoad(irtypes.I1, srcFlag)
			}
		}
		if innerOpt, ok := elemType.(*types.Optional); ok {
			c.maybeRegisterOptionalDrop(s.Binding, alloca, innerOpt)
		} else {
			c.maybeRegisterDrop(s.Binding, alloca, elemType)
		}
		// Only transfer ownership (clear source dropflag) if the unwrapped binding
		// actually got a drop registered. B0246: Structural interfaces don't get drops
		// via maybeRegisterDrop — the source must retain ownership so its Optional drop
		// (RTTI-based) handles cleanup on reassignment or scope exit.
		if _, innerHasDrop := c.dropBindings[s.Binding]; innerHasDrop {
			if ident, ok := s.Init.(*ast.IdentExpr); ok {
				// T0585: Propagate source's pre-clear drop flag into the binding's
				// drop flag only when the source had a flag. Source with no flag
				// is ambiguous at the callee — it could be a borrowed param or an
				// owned param that was auto-moved at the call site (Optional wrap
				// of a narrower arg). We can't distinguish without runtime info,
				// so leave the binding's flag as initialized (1 from
				// maybeRegisterDrop) and let the owned-via-wrap path drop the
				// value at scope exit.
				if srcFlagVal != nil {
					if bindingFlag, has := c.dropFlags[s.Binding]; has {
						c.block.NewStore(srcFlagVal, bindingFlag)
					}
				}
				c.clearDropFlag(ident.Name)
			}
		}

		// T1288: A non-value structural-interface inner unwrapped from an OWNED
		// optional TEMP (call/operator result — no surviving owner) is skipped by
		// maybeRegisterDrop (structural views aren't dropped there) and gets no
		// ownership transfer (that only fires for IdentExpr sources with
		// innerHasDrop). Without a drop, the boxed heap instance leaks. Register an
		// RTTI-dispatched structural free (routes the instance ptr through
		// __promise_structural_drop) so it's freed at scope exit. Gated to
		// fresh-owned-temp sources — an IdentExpr/field-borrow MemberExpr source
		// keeps its own owner which drops the box, so registering here would
		// double-free. T1289: a getter-call MemberExpr and a user-defined `[]`
		// IndexExpr are now correctly recognized as fresh-owned by the helper.
		if _, already := c.dropBindings[s.Binding]; !already {
			if en := extractNamed(elemType); en != nil && en.IsStructural() && !en.IsValueType() {
				if c.isFreshOwnedStructuralRHS(s.Init) {
					c.maybeRegisterStructuralParamFree(s.Binding, alloca, elemType)
				}
			}
		}
	}

	// T0512: A match-borrowed source means the unwrapped binding still
	// aliases variant-owned memory (the synth enum drop walks the full
	// nested Optional chain). Mark it borrowed so a further if-let/while-let
	// on this binding does not transfer ownership and double-free.
	if ident, isIdent := s.Init.(*ast.IdentExpr); isIdent &&
		c.matchBorrowedIdents != nil && c.matchBorrowedIdents[ident.Name] {
		c.matchBorrowedIdents[s.Binding] = true
	}

	c.genBlock(s.Body)

	// B0215: Emit drop for the unwrapped value on the fall-through path.
	// Return/break/raise paths handle this via emitScopeCleanup from their
	// respective base depths (which include this binding).
	if c.block != nil && c.block.Term == nil && len(c.scopeBindings) > unwrapScopeLen {
		cap := c.emitScopeCleanup(unwrapScopeLen, false)
		c.emitCloseErrCheck(cap, unwrapScopeLen)
	}
	c.scopeBindings = c.scopeBindings[:unwrapScopeLen]

	if c.block.Term == nil {
		c.block.NewBr(mergeBlock)
	}

	// Remove binding from scope (it's only visible in the then-block)
	if hadPrev {
		c.locals[s.Binding] = prev
	} else {
		delete(c.locals, s.Binding)
	}
	// B0215: Restore drop flag/binding state from before the if-let.
	if hadDropFlag {
		c.dropFlags[s.Binding] = savedDropFlag
	} else {
		delete(c.dropFlags, s.Binding)
	}
	if hadDropBinding {
		c.dropBindings[s.Binding] = savedDropBinding
	} else {
		delete(c.dropBindings, s.Binding)
	}
	// T0512: Revert the borrow marker propagated for this binding (scoped to
	// the if-let body, same lifetime as the drop flag/binding state above).
	if hadBorrowMark {
		c.matchBorrowedIdents[s.Binding] = savedBorrowMark
	} else if c.matchBorrowedIdents != nil {
		delete(c.matchBorrowedIdents, s.Binding)
	}

	// Else (optional)
	if s.Else != nil {
		c.block = elseBlock
		c.genStmt(s.Else)
		if c.block.Term == nil {
			c.block.NewBr(mergeBlock)
		}
	}

	c.block = mergeBlock

	// B0173: Restore heap/env temps and clean up in the merge block so both
	// then and else paths reach the cleanup (via their branches to mergeBlock).
	// T1329: floor-aware drain (see genIfStmt) so a sibling heap/env prefix
	// survives when this if-unwrap is a leading statement in a block-value body.
	c.heapTemps = savedHeapTemps
	c.heapTempMap = savedHeapTempMap
	c.cleanupHeapTempsFrom(c.blockTempFloorHeap)
	c.envTemps = savedEnvTempsUW     // T0100
	c.envTempMap = savedEnvTempMapUW // T0100
	c.cleanupEnvTempsFrom(c.blockTempFloorEnv)
}
