package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// --- Assignment ---

func (c *Compiler) genAssignStmt(s *ast.AssignStmt) {
	// For compound index assignments, defer RHS evaluation to ensure correct
	// evaluation order: target → key → RHS (not RHS → target → key).
	if s.Op != ast.OpAssign {
		if idx, ok := s.Target.(*ast.IndexExpr); ok {
			c.genCompoundIndexAssign(idx, s.Op, s.Value)
			return
		}
		// T0714: a slice compound assignment (v[a:b] += x) must read-modify-write
		// via [:] / [:]= — without this, genSliceAssign below would drop the
		// operator and store the raw RHS.
		if sl, ok := s.Target.(*ast.SliceExpr); ok {
			c.genSliceCompoundAssign(sl, s.Op, s.Value)
			return
		}
		// T1353: a member/setter compound assignment (`a.b += x`) must evaluate the
		// receiver once and before the RHS (target → RHS → read → op → write), the
		// same canonical order as the index/slice paths. Module-level setters
		// (`mod.prop += x`) have no runtime receiver, so they have neither the
		// double-eval nor the ordering issue — leave those on the existing
		// genModuleSetterAssign path (the MemberExpr case in the switch below).
		if mem, ok := s.Target.(*ast.MemberExpr); ok {
			if ident, isIdent := mem.Target.(*ast.IdentExpr); !isIdent || c.resolveModuleName(ident) == "" {
				c.genMemberCompoundAssign(mem, s.Op, s.Value)
				return
			}
		}
	}

	// Set targetType for Optional member/variable assignments so NoneLit
	// produces the correct zero value (B0030)
	if s.Op == ast.OpAssign {
		targetType := c.info.Types[s.Target]
		if c.typeSubst != nil {
			targetType = types.Substitute(targetType, c.typeSubst)
		}
		if _, isOpt := targetType.(*types.Optional); isOpt {
			c.targetType = targetType
		}
	}
	// T0398/T0410/T0412: When the RHS is a direct vector-index expression and the
	// LHS is droppable, set the dup-on-read flag so genVectorIndex deep-clones
	// the element. Combined with drop-on-overwrite (genVectorIndexAssign for
	// IndexExpr LHS, genMemberAssign for MemberExpr LHS, the IdentExpr branch
	// below for IdentExpr LHS), this preserves the no-alias invariant — vec-to-
	// local writes produce independent clones instead of aliasing the bucket.
	// Direct IndexExpr RHS only — chains like `b = v[0].method()` are excluded
	// to avoid orphan clones (method takes a borrow, clone would leak).
	// T0491: also fire for `OptionalUnwrapExpr` wrapping `IndexExpr` (e.g.
	// `b = m[k]!`) — same dup-on-read need as the var-decl path (T0440).
	if s.Op == ast.OpAssign {
		isIdxRhs := false
		// T0754: peel ParenExpr / CastExpr so `field = v[i] as! T` reaches the
		// same dup-on-read path as `field = v[i]`. Ownership now moves the
		// cast subject at owning-slot stores, but the IndexExpr subject still
		// returns an alias of the container slot — without the dup, the cast
		// result would alias the source vector's element and double-free.
		probe := s.Value
		for {
			p, ok := probe.(*ast.ParenExpr)
			if !ok {
				break
			}
			probe = p.Expr
		}
		// T0800: peel *chained* casts (`(v[i] as! A) as! B`). Each layer is a
		// non-consuming view, so loop until the innermost subject is reached —
		// otherwise the dup-on-read below would not fire for an IndexExpr/
		// MemberExpr subject behind two casts and the stored value would alias
		// the source slot → double-free. Mirrors the recursion in
		// isRttiCastBorrow / castSubjectMovableIdent / tryMoveConsumeCastSubject.
		for {
			cast, ok := probe.(*ast.CastExpr)
			if !ok {
				break
			}
			probe = cast.Expr
			for {
				p, ok := probe.(*ast.ParenExpr)
				if !ok {
					break
				}
				probe = p.Expr
			}
		}
		// T0802: also recognize a field-access RHS (`x = obj.field` and the
		// unwrapped `x = obj.field!`) so the heap-string / Optional[string] field
		// is cloned on read — see the memberRhs branch below.
		var memberRhs *ast.MemberExpr
		bareIdxRhs := false // T1129: RHS is a bare `v[i]` (native Vector/Array), not `m[k]!`
		if _, ok := probe.(*ast.IndexExpr); ok {
			isIdxRhs = true
			bareIdxRhs = true
		} else if m, ok := probe.(*ast.MemberExpr); ok {
			memberRhs = m
		} else if unwrap, ok := probe.(*ast.OptionalUnwrapExpr); ok {
			if _, ok := unwrap.Expr.(*ast.IndexExpr); ok {
				isIdxRhs = true
			} else if m, ok := unwrap.Expr.(*ast.MemberExpr); ok {
				memberRhs = m
			}
		}
		if isIdxRhs {
			var lhsType types.Type
			// T0590: For the string/container dup branches below, we further gate
			// on whether the LHS *target container* is a fixed-size array. Plain
			// Vector slot-to-slot reads alias on read by design (T0490) — the
			// destructive Vector.drop at the slice-assign call site (stmt.go:5648)
			// relies on that aliasing to balance ownership. Setting the dup flag
			// for Vector LHS would force the [:]= body to dup, leaving src's
			// inner buffers leaked. Fixed-size arrays have no such call-site
			// destructive drop; the dup is the only way to avoid the slot-to-slot
			// double-free. Heap-user / tuple flags already exist with this
			// asymmetry: the existing skipB0313 list at line 5648 covers them.
			lhsIsFixedArrayElem := false
			switch t := s.Target.(type) {
			case *ast.IndexExpr:
				lhsType = c.info.Types[t]
				targetType := c.info.Types[t.Target]
				if c.typeSubst != nil && targetType != nil {
					targetType = types.Substitute(targetType, c.typeSubst)
				}
				if ref, ok := targetType.(*types.SharedRef); ok {
					targetType = ref.Elem()
				}
				if ref, ok := targetType.(*types.MutRef); ok {
					targetType = ref.Elem()
				}
				if _, isArr := targetType.(*types.Array); isArr {
					lhsIsFixedArrayElem = true
				}
			case *ast.IdentExpr:
				lhsType = c.info.Types[t]
			case *ast.MemberExpr:
				lhsType = c.info.Types[t]
			}
			if lhsType != nil {
				if c.typeSubst != nil {
					lhsType = types.Substitute(lhsType, c.typeSubst)
				}
				if isDroppableHeapUserType(lhsType) || isHeapUserNoDropPalFree(lhsType) {
					c.dupHeapUserFieldAccess = true
				}
				// T1129: bare droppable-enum LHS read from a native Vector/Array index
				// (`x = v[i]`) — deep-clone so the new slot owns independent variant
				// data (else x's drop-old/scope drop and the source's element walk
				// double-free). The Map form (`x = m[k]!`) is owned by the `[]` body.
				if bareIdxRhs && c.enumElemNeedsDupOnRead(lhsType) {
					c.dupHeapUserFieldAccess = true
				}
				// T1130: Map/Set LHS read from a native Vector/Array index
				// (`x = v[i]`) — deep-clone so the new slot owns an independent
				// Map/Set (else x's drop-old/scope drop and the source's element
				// walk double-free). Plain-index RHS only — the `m[k]!` form is
				// dup'd by the Map.[] body.
				if bareIdxRhs && isMapOrSetType(lhsType) {
					c.dupHeapUserFieldAccess = true
				}
				// T1287: bare structural-interface LHS read from a native Vector index
				// (`a[i] = b[j]`) — deep-clone the {vtable, instance} box so the
				// destination slot owns an independent box (else b[j]'s box aliases into
				// a[i]; a's drop-old and b's element walk (T1284) free the same box →
				// double-free). genVectorIndex consumes the flag via cloneStructuralView.
				if bareIdxRhs && isNonValueStructuralType(lhsType) {
					c.dupHeapUserFieldAccess = true
				}
				// T0412/T0489: same dup-on-read for droppable tuple LHS. Combined
				// with the drop-old branches in genMemberAssign / genVectorIndexAssign /
				// IdentExpr's bindingDropTuple, preserves the no-alias invariant for
				// every LHS shape — `obj.tup = vec[i]`, `t = vec[i]`, and
				// `outer[0] = vec[i]` all produce independent clones in the new slot
				// instead of aliasing the source. Direct IndexExpr/OptionalUnwrap RHS
				// only (gate inherited from the surrounding isIdxRhs check) — same
				// orphan-clone safety reasoning as the heap-user-type branch above.
				if _, isTup := lhsType.(*types.Tuple); isTup && c.tupleNeedsDrop(lhsType) {
					c.dupTupleFieldAccess = true
				}
				// T0590: string and container LHS — required for fixed-size array
				// slot-to-slot copies (`arr[1] = arr[0]`). Without these, the bare
				// `genArrayIndex` returns an alias of the source slot's pointer; the
				// store + drop-on-overwrite then leaves both slots aliasing one
				// allocation → scope-exit double-free. Gated to fixed-size array
				// LHS only because Vector slot-to-slot (inside [:]=) is intentionally
				// aliased; the destructive drop at the slice-assign call site
				// balances ownership for plain container element types.
				if lhsIsFixedArrayElem {
					if extractNamed(lhsType) == types.TypString && !isRefType(lhsType) {
						c.dupStringFieldAccess = true
					}
					if (types.IsVector(lhsType) || types.IsChannel(lhsType) || types.IsArc(lhsType) || types.IsWeak(lhsType)) && !isRefType(lhsType) {
						c.dupContainerFieldAccess = true
					}
					if opt, ok := lhsType.(*types.Optional); ok {
						inner := opt.Elem()
						if c.typeSubst != nil {
							inner = types.Substitute(inner, c.typeSubst)
						}
						if extractNamed(inner) == types.TypString {
							c.dupStringFieldAccess = true
						}
						if types.IsVector(inner) || types.IsChannel(inner) || types.IsArc(inner) || types.IsWeak(inner) {
							c.dupContainerFieldAccess = true
						}
					}
				}
				if opt, ok := lhsType.(*types.Optional); ok {
					inner := opt.Elem()
					if c.typeSubst != nil {
						inner = types.Substitute(inner, c.typeSubst)
					}
					if _, isTup := inner.(*types.Tuple); isTup && c.tupleNeedsDrop(inner) {
						c.dupTupleFieldAccess = true
					}
					// T1291: also clone an Optional[structural] inner on reassignment
					// (`z = v[i]`) — its element drop is now active, so the aliased read
					// must own an independent box else the two drops double-free.
					if isDroppableHeapUserType(inner) || isHeapUserNoDropPalFree(inner) || isNonValueStructuralType(inner) {
						c.dupHeapUserFieldAccess = true
					}
				}
			}
		} else if memberRhs != nil {
			// T0802: `x = obj.field` (and the unwrapped `x = obj.field!`)
			// reassignment of a heap string / Optional[string] (or container)
			// field must clone-on-read, exactly like the var-decl path
			// (genDeclStmt) already does. Without this the field's heap pointer is
			// stored into x as a raw alias with x's drop flag set, so both x and the
			// field's owner drop the same allocation → double-free (latent on linux,
			// SIGABRT on macOS). Scoped to MemberExpr RHS only so the intentional
			// Vector slot-to-slot aliasing in the isIdxRhs branch (T0490/T0590) is
			// untouched. Heap-user / tuple field moves are deliberately excluded —
			// the ownership checker already rejects those, and an ungated clone here
			// risks orphan-clone leaks (same gating reason as the decl path).
			//
			// s.Value's resolved type is the field type for a bare `obj.field` and
			// the unwrapped inner type for `obj.field!`; setDupFlagsForFieldAccess
			// covers both (string and Optional[string] map to dupStringFieldAccess,
			// which genFieldAccess honors for the inner Optional[string] field too).
			rhsType := c.info.Types[s.Value]
			if c.typeSubst != nil && rhsType != nil {
				rhsType = types.Substitute(rhsType, c.typeSubst)
			}
			c.setDupFlagsForFieldAccess(rhsType)
		}
		// T1170: RHS is a match-borrowed Optional/Array-of-heap binding (`out = maybe`)
		// or an element of a match-borrowed array (`out = a[0]`). The binding aliases
		// the subject enum's variant payload; without a clone the store aliases it and
		// the subject's synth drop frees it while the LHS still references it (UAF on
		// escape). Set the dup-on-read flag so genIdentExpr (whole Optional/container)
		// or genArrayIndex (array element) deep-clones; the LHS then owns an independent
		// copy claimed by the IdentExpr-target claim path below (mirrors the memberRhs
		// case). The element form bypasses the isIdxRhs `lhsIsFixedArrayElem` gate (which
		// protects intentional Vector slot aliasing) — a borrow-marked array element must
		// always clone on store to an outer slot.
		if c.matchBorrowedIdents != nil {
			if ident, ok := probe.(*ast.IdentExpr); ok && c.matchBorrowedIdents[ident.Name] {
				rhsType := c.info.Types[s.Value]
				if c.typeSubst != nil && rhsType != nil {
					rhsType = types.Substitute(rhsType, c.typeSubst)
				}
				// Only whole Optional/Array variant-payload bindings — bare-heap
				// T0672 borrow bindings are already owned and must not be re-dup'd.
				if isVariantPayloadBorrowShape(rhsType) {
					c.setDupFlagsForFieldAccess(rhsType)
				}
			} else if idx, ok := probe.(*ast.IndexExpr); ok {
				if baseIdent, ok := idx.Target.(*ast.IdentExpr); ok && c.matchBorrowedIdents[baseIdent.Name] {
					baseType := c.info.Types[idx.Target]
					if c.typeSubst != nil && baseType != nil {
						baseType = types.Substitute(baseType, c.typeSubst)
					}
					// Only a fixed-Array base (the enum-variant `string[N]` payload
					// shape). A Vector/container base routes through genVectorIndex/
					// genMethodIndex, whose own dup-on-read already balances ownership.
					if _, isArr := baseType.(*types.Array); isArr {
						elemType := c.info.Types[idx]
						if c.typeSubst != nil && elemType != nil {
							elemType = types.Substitute(elemType, c.typeSubst)
						}
						c.setDupFlagsForFieldAccess(elemType)
					}
				}
			}
		}
	}
	// T1230: a closure-nesting aggregate read from an aliasing container (`f = m[k]!`
	// on a `Fn{()->int}` value) is a borrow — the env can't be deep-cloned. Suppress
	// the dup-on-read set above so the local aliases the container's instance with the
	// env intact; the owning drop is cleared below via isClosureAggregateBorrow.
	if s.Op == ast.OpAssign && c.isClosureAggregateBorrow(s.Value) {
		c.dupHeapUserFieldAccess = false
	}
	// T0952: `m = a ?: b` — signal genElvis (as in genInferredVarDecl) so the
	// none-path default's owner is neutralized; the assignment target owns the temp.
	prevElvisBound := c.elvisResultBound
	prevElvisOwnsForced := c.elvisResultOwnsForced // T1166
	if be, ok := unwrapDestructureParens(s.Value).(*ast.BinaryExpr); ok && be.Op == ast.BinElvis {
		c.elvisResultBound = true
		// T1166: a member/index target is an owned field/element with no per-slot drop
		// flag — it cannot hold a borrow-alias. Signal genElvis to force-clone a borrowed
		// operand so the result is unconditionally owned (see elvisResultOwnsForced).
		switch s.Target.(type) {
		case *ast.MemberExpr, *ast.IndexExpr:
			c.elvisResultOwnsForced = true
		}
	}
	val := c.genExpr(s.Value)
	// T1174: `out = maybe` (any owning target — ident/member/index) where maybe is
	// a match-borrowed Optional[heap-user] binding aliases the subject's variant
	// payload; deep-clone the inner so the target owns an independent copy that the
	// subject's scope-exit synth enum drop does not free out from under it. Gated to
	// plain assignment: a compound-op RHS is not a straight move of the alias.
	if s.Op == ast.OpAssign {
		val, _ = c.dupBorrowedHeapUserPayload(s.Value, val)
	}
	c.elvisResultBound = prevElvisBound
	c.elvisResultOwnsForced = prevElvisOwnsForced // T1166
	// T1014: the assignment form `m = a ?: b` consumes the per-path bound flag for a
	// simple-local IdentExpr target with a drop binding (mirrors consumeElvisBoundDropFlag
	// on the var-decl path). Capture it here and clear the field immediately so a
	// set-but-unconsumed flag can't leak into a later var-decl binding; the IdentExpr
	// OpAssign branch below stores it into the target's drop flag, overriding the
	// unconditional `1` the drop-old re-arm writes. Member/index targets do not use this
	// flag — T1166 handles their aliasing by force-cloning a borrowed elvis operand (see
	// elvisResultOwnsForced above) so the field/element is unconditionally owned.
	elvisBoundFlag := c.elvisBoundDropFlag
	c.elvisBoundDropFlag = nil
	// T1209: capture a mixed owned/borrowed match/if result's live per-path flag
	// before the claims below neutralize it; stored into the target's drop flag at
	// the elvis store site so it overrides the drop-old re-arm (`store i1 true`).
	// Skipped for an elvis RHS (elvisBoundFlag, already nil'd into the local) and for
	// any non-match/if perPathFlag temp — e.g. the member optional handler
	// `owner.field? _ {}` (T1162), whose binding MOVES the value out of the field so
	// applying the stale present=borrowed flag would leak. See isMixedMergeBindingRHS.
	var mergeBoundFlag value.Value
	if elvisBoundFlag == nil && isMixedMergeBindingRHS(s.Value) {
		mergeBoundFlag = c.captureLiveTempFlag(val)
	}
	c.targetType = nil
	c.dupHeapUserFieldAccess = false
	c.dupTupleFieldAccess = false
	c.dupStringFieldAccess = false
	c.dupContainerFieldAccess = false

	// T0802: When the RHS is an Optional[string]/Optional[container] field read
	// (`opt = obj.field`), genFieldAccess clones the inner value and tracks it via
	// optionalStringDup/optionalContainerDup. The clone is stored into the LHS, so
	// claim it here to suppress the leftover stmt-temp drop — otherwise the clone is
	// freed while the LHS still references it → double-free. Mirrors genDeclStmt
	// (the var-decl path) which performs the same claim before optional wrapping.
	if c.optionalStringDup != nil {
		c.claimStringTemp(c.optionalStringDup)
		c.optionalStringDup = nil
	}
	if c.optionalContainerDup != nil {
		c.claimStringTemp(c.optionalContainerDup)
		c.optionalContainerDup = nil
	}

	// Auto-propagate failable call in assignment RHS.
	if c.info.AutoPropagateExprs[s.Value] {
		val = c.genAutoPropagateTracked(s.Value, val)
	}

	switch target := s.Target.(type) {
	case *ast.IdentExpr:
		// Same-file setter: property = value (or property += value)
		if setterFn, ok := c.funcs[target.Name+"$set"]; ok {
			if obj := c.lookupFunc(target.Name + "$set"); obj != nil && obj.IsSetter() {
				if s.Op != ast.OpAssign {
					getterFn, ok := c.funcs[target.Name]
					if !ok {
						panic(fmt.Sprintf("codegen: compound assignment to setter %s but no getter found", target.Name))
					}
					var current value.Value = c.block.NewCall(getterFn)
					current = c.unwrapFailableCompoundRead(current, c.info.Types[target]) // T0709
					val = c.genCompoundOp(s.Op, c.info.Types[target], current, val)
				}
				setterCall := c.block.NewCall(setterFn, val)
				c.propagateIfFailable(setterCall) // T0708
				if s.Op == ast.OpAssign {
					if rhsIdent, ok := s.Value.(*ast.IdentExpr); ok {
						c.clearDropFlag(rhsIdent.Name)
					}
					// B0312: When RHS is opt!, neutralize the source optional so its
					// drop doesn't double-free the inner value now owned by the setter.
					c.neutralizeForceUnwrapSource(s.Value)
				}
				return
			}
		}
		// MutRef param: store through the caller's pointer (B0149)
		if ptr, ok := c.mutRefPtrs[target.Name]; ok {
			if s.Op == ast.OpAssign {
				c.block.NewStore(val, ptr)
				if ident, ok := s.Value.(*ast.IdentExpr); ok {
					c.clearDropFlag(ident.Name)
				}
				// B0312: When RHS is opt!, neutralize the source optional so its
				// drop doesn't double-free the inner value now owned by the MutRef target.
				c.neutralizeForceUnwrapSource(s.Value)
				return
			}
			// Compound assignment on MutRef param
			current := c.block.NewLoad(c.mutRefTypes[target.Name], ptr)
			result := c.genCompoundOp(s.Op, c.info.Types[target], current, val)
			// T0715: a non-native operator returns a FRESH value; drop the old
			// heap value behind the borrowed pointer (no-op for value types/scalars/
			// string) to preserve the zero-leak policy.
			c.dropOldUserValueAtPtr(ptr, c.info.Types[target], result)
			c.block.NewStore(result, ptr)
			return
		}
		alloca, ok := c.locals[target.Name]
		if !ok {
			panic(fmt.Sprintf("codegen: undefined variable %q in assignment", target.Name))
		}
		if s.Op == ast.OpAssign {
			// T0892: an operator/method RHS whose body is `return this` returns the
			// borrowed receiver as an owned result, aliasing the operand. Resolve the
			// receiver-origin the same way the two var-decl paths do (genTypedVarDecl /
			// genInferredVarDecl), so the assignment path gets the same
			// B0250/T0341/T0882 alias-clear it currently lacks. selfAliasOrigin marks
			// the case where the operand IS the target (`m = m + b`), which needs the
			// guarded drop-old below instead of an operand-clear.
			var aliasOrigin ast.Expr
			if call, ok := s.Value.(*ast.CallExpr); ok {
				aliasOrigin = chainOriginExpr(call)
			} else {
				aliasOrigin = operatorReceiverOrigin(s.Value)
			}
			selfAliasOrigin := false
			if id, ok := aliasOrigin.(*ast.IdentExpr); ok && id.Name == target.Name {
				selfAliasOrigin = true
			}
			// T0911: closure self-assignment (`f = f`) is a no-op — the local keeps
			// owning its env. Return early (mirroring the dropBindings self-assign
			// guard below) so the post-store clearDropFlag doesn't zero the env drop
			// flag, which would leak the env at scope exit. Non-self-assign env
			// drop is handled by the T0911/T0913 dropFlags block below.
			if ident, ok := s.Value.(*ast.IdentExpr); ok && ident.Name == target.Name {
				tt := c.info.Types[target]
				if c.typeSubst != nil {
					tt = types.Substitute(tt, c.typeSubst)
				}
				if _, isSig := tt.(*types.Signature); isSig {
					return
				}
			}
			// Drop old value before reassignment (if target is droppable).
			//
			// T1640: a closure local is now in dropBindings too (B0354 needs it to
			// see the capture), but its old value is released by the T0911/T0913
			// env block below — which alias-checks the env pointer and re-arms the
			// flag itself. Letting a bindingFreeEnv fall through here would emit a
			// drop-flag test and an empty drop body on every closure reassignment,
			// and any future branch keyed on a non-env kind could act on it.
			if binding, ok := c.dropBindings[target.Name]; ok && binding.kind != bindingFreeEnv {
				// Skip self-assignment (would drop then store dangling pointer)
				if ident, ok := s.Value.(*ast.IdentExpr); ok && ident.Name == target.Name {
					return
				}
				// For string/vector types (i8* pointers), the new value might alias the
				// old (e.g., v = sort(v) returns the same pointer). Compare old/new at
				// runtime and only drop if they differ (T0068).
				if binding.kind == bindingDropString {
					oldVal := c.block.NewLoad(binding.alloca.ElemType, binding.alloca)
					diffBlk := c.newBlock("reassign.diff")
					mergeBlk := c.newBlock("reassign.merge")
					isSame := c.block.NewICmp(enum.IPredEQ, oldVal, val)
					c.block.NewCondBr(isSame, mergeBlk, diffBlk)
					c.block = diffBlk
					c.emitStringDropCall(binding)
					if c.block.Term == nil {
						c.block.NewBr(mergeBlk)
					}
					c.block = mergeBlk
				} else if binding.kind == bindingDropEnum {
					c.emitEnumDropCall(binding)
				} else if binding.kind == bindingDropTuple {
					c.emitTupleDropCall(binding)
				} else if binding.kind == bindingDropArray {
					c.emitArrayDropCall(binding)
				} else if selfAliasOrigin &&
					(binding.kind == bindingFree || binding.kind == bindingDrop) &&
					isUserValueStructType(binding.alloca.ElemType) &&
					isUserValueStructType(val.Type()) {
					// T0892: `m = m + b` / `m = m.dup()` where the operator/method
					// returns `this`. The RHS is evaluated before this drop, so val
					// already holds the (possibly aliasing) instance pointer. Skip the
					// drop when it aliases the old value — otherwise we free the very
					// instance val points to (UAF/double-free). Mirrors the runtime
					// old-vs-new alias check the bindingDropString branch above performs.
					// Covers both heap-user kinds: bindingFree (no drop method →
					// pal_free) and bindingDrop (has a drop method).
					oldVal := c.block.NewLoad(binding.alloca.ElemType, binding.alloca)
					oldInst := c.block.NewExtractValue(oldVal, 1)
					newInst := c.block.NewExtractValue(val, 1)
					diffBlk := c.newBlock("reassign.self.diff")
					mergeBlk := c.newBlock("reassign.self.merge")
					isSame := c.block.NewICmp(enum.IPredEQ, oldInst, newInst)
					c.block.NewCondBr(isSame, mergeBlk, diffBlk)
					c.block = diffBlk
					if binding.kind == bindingFree {
						c.emitFreeCall(binding)
					} else {
						c.emitDropCall(binding)
					}
					if c.block.Term == nil {
						c.block.NewBr(mergeBlk)
					}
					c.block = mergeBlk
				} else if binding.kind == bindingFree {
					c.emitFreeCall(binding)
				} else {
					c.emitDropCall(binding)
				}
				// Reset drop flag: new value is now owned
				c.block.NewStore(constant.NewInt(irtypes.I1, 1), binding.dropFlag)
			}

			// T0911: a closure's env cleanup is a bindingFreeEnv with a flag in
			// c.dropFlags (see maybeRegisterEnvFree). The drop-old logic above
			// skips that kind, so reassigning a closure local that owns a heap env
			// would otherwise leak it.
			// Free the old env here (guarded by the flag + an old-vs-new
			// env-pointer alias check, since `f = <expr aliasing f's env>` could
			// occur) and re-arm the flag so the new value is owned. The later
			// T0895 borrow-clear (below) and the move-RHS clearDropFlag then
			// adjust the flag for borrow/move RHS as appropriate.
			if envFlag, hasEnvFlag := c.dropFlags[target.Name]; hasEnvFlag {
				tt := c.info.Types[target]
				if c.typeSubst != nil {
					tt = types.Substitute(tt, c.typeSubst)
				}
				if _, isSig := tt.(*types.Signature); isSig {
					oldClosure := c.block.NewLoad(alloca.ElemType, alloca)
					oldEnv := c.block.NewExtractValue(oldClosure, 1) // field 1 = env ptr
					newEnv := c.block.NewExtractValue(val, 1)
					flag := c.block.NewLoad(irtypes.I1, envFlag)
					isSame := c.block.NewICmp(enum.IPredEQ, oldEnv, newEnv)
					notSame := c.block.NewXor(isSame, constant.NewInt(irtypes.I1, 1))
					doFree := c.block.NewAnd(flag, notSame)
					freeBlk := c.newBlock("reassign.env.free")
					mergeBlk := c.newBlock("reassign.env.merge")
					c.block.NewCondBr(doFree, freeBlk, mergeBlk)

					c.block = freeBlk
					isNull := c.block.NewICmp(enum.IPredEQ, oldEnv, constant.NewNull(irtypes.I8Ptr))
					callBlk := c.newBlock("reassign.env.call")
					c.block.NewCondBr(isNull, mergeBlk, callBlk)

					c.block = callBlk
					c.emitEnvDropOrFree(oldEnv) // drops captured values + frees env struct
					c.block.NewBr(mergeBlk)

					c.block = mergeBlk
					// New value is now owned by the local.
					c.block.NewStore(constant.NewInt(irtypes.I1, 1), envFlag)
				}
			}

			// Coerce value struct vtable when crossing type boundaries
			exprType := c.info.Types[s.Value]
			targetType := c.info.Types[target]
			if c.typeSubst != nil {
				exprType = types.Substitute(exprType, c.typeSubst)
				targetType = types.Substitute(targetType, c.typeSubst)
			}
			val = c.coerceToView(val, exprType, targetType)

			// T0111: Claim string temp BEFORE optional wrapping — after wrap,
			// value identity changes and claimStringTemp can't match the temp.
			// T0555: Same for native handle / container temps so the stmt-temp
			// drop doesn't double-free with the optional's binding drop.
			if _, isOpt := targetType.(*types.Optional); isOpt {
				if exprType != nil {
					if extractNamed(exprType) == types.TypString ||
						types.IsVector(exprType) || types.IsChannel(exprType) ||
						types.IsArc(exprType) || types.IsWeak(exprType) ||
						types.IsMutex(exprType) || types.IsAnyTask(exprType) ||
						types.IsMutexGuard(exprType) {
						c.claimStringTemp(val)
					}
				}
			}

			// Wrap value in Optional if target is Optional and expr differs in shape.
			// Using Identical (not "is exprOpt?") correctly handles T?? = T? — both
			// are Optional but at different depths, so a wrap is still needed.
			// T1087: strip SharedRef/MutRef before comparing — genArcBorrow/
			// genMutexGuardBorrow auto-copy borrowed value/Copy optionals to a bare
			// {i1,T} struct; sema records exprType as ref-to-optional, so strip
			// before Identical to avoid spurious re-wrap (insertvalue elem-type panic).
			if _, isOpt := targetType.(*types.Optional); isOpt {
				if exprType == types.TypNone {
					// T1190: a none-typed RHS (bare `none` / all-`none` match-if) coerces
					// to the concrete Optional[T] zero (avoids an i1→{i1,T} mismatch).
					val = c.coerceNoneToOptional(val, exprType, targetType)
				} else if !types.Identical(unwrapRefsType(exprType), targetType) {
					// T1298: box/view-coerce into the Optional's element type before
					// wrapping (the coerceToView above ran against the Optional target
					// and was a no-op for a concrete → structural-interface RHS).
					val = c.coerceToOptionalElem(val, exprType, targetType)
					val = c.wrapOptional(val, alloca.ElemType.(*irtypes.StructType))
				}
			}

			c.block.NewStore(val, alloca)
			// Clear drop flag on RHS if it's being moved
			if ident, ok := s.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
			// T0892: clear the operand/receiver drop flag when the operator/method
			// result aliases it (user operator/method whose body is `return this`).
			// Mirrors the two var-decl paths. Skip structural targets — the view
			// borrows the original, which must keep its drop flag (T0082); and skip
			// the self-alias case (handled by the guarded drop-old above — clearing
			// here would compare post-store and wrongly clear the target's own flag,
			// leaking the instance).
			if binding, ok := c.dropBindings[target.Name]; ok && !selfAliasOrigin {
				structuralTarget := false
				if n := extractNamed(targetType); isStructuralView(n) {
					structuralTarget = true
				}
				if !structuralTarget {
					switch origin := aliasOrigin.(type) {
					case *ast.IdentExpr:
						c.maybeClearReceiverDropFlag(val, origin.Name, exprType)
					case *ast.ThisExpr:
						c.maybeClearBindingDropFlagOnThisAlias(val, binding.dropFlag, exprType)
					}
				}
			}
			// T0379/T0381: when RHS static type is `T&`/`T~`, override the
			// unconditional re-arm above. The borrow returns a non-owning
			// reference; the owner retains the value. Without this, both
			// the reassigned local's drop and the owner's drop free the same
			// inner value.
			// T1085: a reassignment changes the optional local's borrow-holding
			// status. Clear any stale mark first (owned reassignment makes it
			// non-borrow again — the heap handler may then neutralize+track), then
			// re-mark below if the new RHS is itself a borrow.
			if c.borrowOptionalLocals != nil {
				delete(c.borrowOptionalLocals, target.Name)
			}
			if c.isBorrowedExpr(s.Value) {
				c.clearDropFlag(target.Name)
				c.markBorrowOptionalLocal(target.Name, exprType)
			}
			// T0747: a user-type RTTI cast of a borrow (`target = x as!/as T`) is a
			// non-consuming view — the subject keeps ownership. Clear the target's
			// drop flag (re-armed to 1 above when the old value was dropped) so the
			// reassigned local doesn't double-free the aliased instance at scope
			// exit. Mirrors the same clear in genTypedVarDecl/genInferredVarDecl.
			if c.isRttiCastBorrow(s.Value) {
				c.clearDropFlag(target.Name)
				c.markBorrowOptionalLocal(target.Name, exprType)
			}
			// T0895: `f = h.cb` reads a closure out of an owning aggregate — the
			// local borrows the heap env (the aggregate retains sole ownership;
			// closures aren't Cloneable, so the fat pointer {fn,env} is copied by
			// value with no env dup). Clear f's env-free drop flag (still 1 from
			// the var-decl's maybeRegisterEnvFree — the drop-old re-arm above
			// skips bindingFreeEnv, and the T0911 env block re-arms instead) so
			// scope-exit env-free doesn't
			// double-free against the aggregate's drop. Mirrors
			// maybeRegisterEnvFree's var-decl suppression. isClosureAggregateBorrow
			// is now self-gated on FirstFieldNestedClosure, so it fires for a direct
			// closure target (T0895) AND a struct/enum aggregate nesting a closure
			// read from an aliasing container (`f = m[k]!` on a `Fn{()->int}` value,
			// T1230) — the local aliases the container's instance (env intact), so
			// its owning drop must be suppressed against the container's own drop.
			if c.isClosureAggregateBorrow(s.Value) {
				c.clearDropFlag(target.Name)
			}
			// T0073: Claim string temp — ownership transferred to this variable.
			// Skip if already claimed above (optional target).
			if exprType != nil && extractNamed(exprType) == types.TypString {
				c.claimStringTemp(val)
			}
			// T0109: Claim vector/channel/arc/weak temp — ownership transferred to this variable.
			// T0555: Mutex/Task also need claiming now that their constructor temps are tracked.
			// T0561: MutexGuard temps from m.lock() also need claiming.
			if exprType != nil && (types.IsVector(exprType) || types.IsChannel(exprType) ||
				types.IsArc(exprType) || types.IsWeak(exprType) ||
				types.IsMutex(exprType) || types.IsAnyTask(exprType) ||
				types.IsMutexGuard(exprType)) {
				c.claimStringTemp(val)
			}
			// T1181: Claim fixed-array temp — ownership transferred to the
			// reassigned variable's bindingDropArray; avoids a double-free.
			if exprType != nil {
				if _, ok := exprType.(*types.Array); ok {
					c.claimStringTemp(val)
				}
			}
			// B0187: Claim heap temp — ownership transferred to reassigned variable.
			// Without this, structural interface reassignment (e.g., iter = c.map(...))
			// leaves the heap temp unclaimed, causing double-free at statement end + scope exit.
			c.claimHeapTemp(val)
			// Claim env temp — ownership transferred to reassigned variable.
			c.claimEnvTemp(val)
			// B0293/T1323: Clear enum ctor temps only when the RHS itself MOVES the enum
			// out into the reassigned variable (an enum constructor, or a match/if
			// producing the enum) — NOT when it is a call whose RESULT happens to be an
			// enum (`q = f(E.V(...))`), where the by-value enum-ctor ARG temp stays owned
			// here and must fall through to the statement-boundary drain (a type-based
			// `extractEnum` check would misfire on the call result and orphan the arg's
			// payload). Without the clear on a genuine move-out, the ctor temp drop fires
			// at statement end and double-frees variant data the variable now owns.
			// T1340: floor-bounded clear (see clearMovedOutEnumCtorTemps).
			if len(c.enumCtorTemps) > c.blockTempFloorEnum && c.enumCtorTempMovesOut(s.Value) {
				c.clearMovedOutEnumCtorTemps()
			}
			// B0312: When RHS is opt!, neutralize the source optional so its
			// drop doesn't double-free the inner value now owned by this variable.
			c.neutralizeForceUnwrapSource(s.Value)
			// T1014: `m = a ?: b` — store the elvis's per-path bound flag into the
			// target's drop flag, replacing the unconditional `1` the drop-old re-arm
			// (and any borrow-clear) wrote above. On the some-path the result may alias
			// a borrowed/caller-owned inner (flag 0), so the target must not double-free
			// it at scope exit. Mirrors consumeElvisBoundDropFlag on the var-decl path;
			// placed last so it overrides earlier flag writes. The none dimension shares
			// the pre-existing T0940 owned-local/member/borrowed-param default gap.
			if elvisBoundFlag != nil {
				if lhsFlag, ok := c.dropFlags[target.Name]; ok {
					c.block.NewStore(elvisBoundFlag, lhsFlag)
				}
			}
			c.applyBoundMergeFlag(target.Name, mergeBoundFlag) // T1209
			return
		}
		// Compound assignment: load current value, apply operator, store result
		targetType := c.info.Types[target]
		current := c.block.NewLoad(alloca.ElemType, alloca)
		result := c.genCompoundOp(s.Op, targetType, current, val)
		// T0357: For string locals with a drop binding, drop the old value
		// before storing the new one. promise_string_concat always allocates,
		// so the result never aliases the old pointer; runtime alias check
		// mirrors the OpAssign branch above for consistency.
		if binding, ok := c.dropBindings[target.Name]; ok && binding.kind == bindingDropString {
			diffBlk := c.newBlock("compound.diff")
			mergeBlk := c.newBlock("compound.merge")
			isSame := c.block.NewICmp(enum.IPredEQ, current, result)
			c.block.NewCondBr(isSame, mergeBlk, diffBlk)
			c.block = diffBlk
			c.emitStringDropCall(binding)
			if c.block.Term == nil {
				c.block.NewBr(mergeBlk)
			}
			c.block = mergeBlk
			// New value is now owned by the local — drop flag stays at 1.
			c.block.NewStore(constant.NewInt(irtypes.I1, 1), binding.dropFlag)
		} else {
			// T0715: a non-native operator returns a FRESH value, so the old heap
			// user-type (or enum) value at the alloca leaks unless dropped. Reuse
			// the alias-guarded drop-old helper (no-op for value types/scalars).
			// T1194: the flag-aware slot helper additionally guards the drop-old on
			// the drop flag so a borrow-by-default heap param (flag 0, caller-owned
			// original) is not double-freed, and arms the flag so the fresh result
			// is dropped at scope exit rather than leaked.
			c.dropOldUserValueAtIdentSlot(target.Name, alloca, targetType, result)
		}
		c.block.NewStore(result, alloca)

	case *ast.MemberExpr:
		// Module-level setter: mod.property = value
		if ident, ok := target.Target.(*ast.IdentExpr); ok {
			if modName := c.resolveModuleName(ident); modName != "" {
				c.genModuleSetterAssign(target, modName, s.Op, val)
				if s.Op == ast.OpAssign {
					if rhsIdent, ok := s.Value.(*ast.IdentExpr); ok {
						c.clearDropFlag(rhsIdent.Name)
					}
					// B0312: When RHS is opt!, neutralize the source optional so its
					// drop doesn't double-free the inner value now owned by this setter.
					c.neutralizeForceUnwrapSource(s.Value)
				}
				break
			}
		}
		// Wrap value in Optional if field type is Optional but expr is not
		if s.Op == ast.OpAssign {
			memberType := c.info.Types[target]
			exprType := c.info.Types[s.Value]
			if c.typeSubst != nil {
				memberType = types.Substitute(memberType, c.typeSubst)
				exprType = types.Substitute(exprType, c.typeSubst)
			}
			if _, isOpt := memberType.(*types.Optional); isOpt {
				if exprType != types.TypNone {
					// T0394: Claim heap temps from stmtTemps BEFORE wrapping in
					// Optional. claimStringTemp uses direct val-identity lookup,
					// so the post-wrap claim below fails (val identity changes
					// after wrapOptional). Mirrors T0111 fix in IdentExpr branch
					// (~line 4811) and genVarDecl (~line 727). claimHeapTemp
					// post-wrap still handles heapTemp-tracked vector literals
					// via runtime extractvalue.
					if extractNamed(exprType) == types.TypString ||
						types.IsVector(exprType) || types.IsChannel(exprType) ||
						types.IsArc(exprType) || types.IsWeak(exprType) ||
						types.IsAnyTask(exprType) || types.IsMutex(exprType) ||
						types.IsMutexGuard(exprType) {
						// T0560: Task RHS in `field = go ...` where the field is
						// Optional[Task[T]]. Without claiming the temp BEFORE
						// wrapping into the Optional struct, the stmtTemp cleanup
						// runs at statement end and drops G — but G is now owned
						// by the optional field, causing a double-free at scope
						// exit via the Optional field-drop path.
						// T0573: Mutex/MutexGuard added — their constructors track
						// stmtTemps too, so without claiming before wrapping the
						// optional field path's drop double-frees with the temp
						// cleanup.
						c.claimStringTemp(val)
					}
					// Use Identical (not "is exprOpt?") so T?? = T? still wraps.
					// T1087: strip SharedRef/MutRef — same as Site 1 / T0856.
					if !types.Identical(unwrapRefsType(exprType), memberType) {
						optType := c.resolveType(memberType)
						if st, ok := optType.(*irtypes.StructType); ok {
							// T1298: box/view-coerce into the Optional field's element
							// type before wrapping, e.g. `h.s = Counter(...)` where s is
							// a structural-interface Optional field.
							val = c.coerceToOptionalElem(val, exprType, memberType)
							val = c.wrapOptional(val, st)
						}
					}
				}
			}
		}
		// T0095: Dup string values stored in fields of droppable types when the
		// source is a borrowed variable (no drop flag). This handles custom new()
		// methods like `this.src = s` where s is a non-~ parameter.
		if s.Op == ast.OpAssign {
			memberType := c.info.Types[target]
			if c.typeSubst != nil {
				memberType = types.Substitute(memberType, c.typeSubst)
			}
			ownerType := c.info.Types[target.Target]
			if c.typeSubst != nil {
				ownerType = types.Substitute(ownerType, c.typeSubst)
			}
			ownerNamed := extractNamed(ownerType)
			if extractNamed(memberType) == types.TypString && ownerNamed != nil && ownerNamed.HasDrop() {
				if ident, ok := s.Value.(*ast.IdentExpr); ok {
					if _, hasFlag := c.dropFlags[ident.Name]; hasFlag {
						// Has drop flag: move ownership
						c.genMemberAssign(target, s.Op, val, s.Value)
						c.clearDropFlag(ident.Name)
					} else {
						// No drop flag: dup for exclusive ownership.
						// Pass nil srcExpr so genMutexGuardBorrowSet's defensive
						// dup doesn't fire — already duped here.
						c.genMemberAssign(target, s.Op, c.dupString(val), nil)
					}
				} else {
					// Expression result: store directly, claim temp
					c.genMemberAssign(target, s.Op, val, s.Value)
					c.claimStringTemp(val)
					// B0312: When RHS is opt!, neutralize the source optional so its
					// drop doesn't double-free the inner value now owned by this field.
					c.neutralizeForceUnwrapSource(s.Value)
				}
				break
			}
		}
		c.genMemberAssign(target, s.Op, val, s.Value)
		// Clear drop flag on RHS if it's being moved via simple assign
		if s.Op == ast.OpAssign {
			if ident, ok := s.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
			// T0754: clear cast subject's drop flag — ownership moves it at
			// the owning-slot store, so the subject's scope-exit drop must
			// not fire on the same allocation the field now owns. T0849: for
			// the conditional `as` form, drop iff the downcast failed.
			if ident := c.castSubjectMovableIdent(s.Value); ident != nil {
				c.consumeCastSubjectDropFlag(s.Value, ident.Name)
			}
			// B0168: Claim string temp — ownership transferred to field.
			c.claimStringTemp(val)
			// B0233: Claim heap temp — ownership transferred to field.
			c.claimHeapTemp(val)
			// T1226: Claim closure env temp — ownership transferred to the field
			// (directly, or through a setter property that stores it). Without this,
			// a capturing-closure RHS's env is freed at statement end
			// (cleanupEnvTemps) while the field still references it →
			// use-after-free at the owner's scope-exit drop. T1160 widened the
			// trigger to closure-returning call results. No-op for non-closure values.
			c.claimEnvTemp(val)
			// B0312: When RHS is opt!, neutralize the source optional so its
			// drop doesn't double-free the inner value now owned by this field.
			c.neutralizeForceUnwrapSource(s.Value)
			// T0899: an operator/method RHS whose body is `return this` returns the
			// borrowed receiver as an owned result, aliasing the operand local. The
			// field now owns that instance, so clear the operand's drop flag —
			// otherwise both the operand local and the field owner free it
			// (double-free). Mirrors the IdentExpr branch (T0892) and the two
			// var-decl paths. The self-alias case (`h.f = h.f + b`) is handled by
			// genMemberAssign's same-pointer drop-old guard (its non-Ident origin
			// skips the clear); see clearOperandAliasForOwnedStore. T1084: no-op
			// given T0893's clone in wrapThisReturnValue.
			c.clearOperandAliasForOwnedStore(s.Value, val)
		}

	case *ast.IndexExpr:
		// B0195: Vector[string] index assign — dup new value so vector owns
		// an independent copy (like push, B0189). Source retains its string.
		// Old element is NOT dropped here (see B0204 for why).
		if s.Op == ast.OpAssign {
			idxTargetType := c.info.Types[target.Target]
			// T0386: Inside generic method bodies, c.info.Types[ThisExpr] returns
			// the bare Named owner without TypeArgs bound. Use c.monoCtx.inst
			// (the concrete Instance) so types.AsVector succeeds and the
			// per-element string-dup fires inside Vector[T].[:]=.
			if isThisReceiver(target.Target) && c.monoCtx != nil {
				idxTargetType = c.monoCtx.inst
			}
			if c.typeSubst != nil {
				idxTargetType = types.Substitute(idxTargetType, c.typeSubst)
			}
			// Unwrap borrows (auto-deref through &/&mut)
			if ref, ok := idxTargetType.(*types.MutRef); ok {
				idxTargetType = ref.Elem()
			}
			if ref, ok := idxTargetType.(*types.SharedRef); ok {
				idxTargetType = ref.Elem()
			}
			if elemType, isVec := types.AsVector(idxTargetType); isVec {
				resolvedElem := elemType
				if c.typeSubst != nil {
					resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
				}
				if extractNamed(resolvedElem) == types.TypString {
					dupVal := c.dupString(val)
					c.genIndexAssign(target, s.Op, dupVal, s.Value)
					// Note: do NOT neutralize opt! source here — dupString creates an
					// independent copy for the vector, so the original stays owned by
					// the source optional (whose drop frees it at scope exit).
					break
				}
			}
			// B0350: Map[K,string] index assign from borrow param — dup value
			// so the map owns an independent copy. Borrow params have no drop
			// flag, so clearDropFlag below is a no-op and the caller still frees
			// the original → double-free without this dup.
			if _, valType, isMap := types.AsMap(idxTargetType); isMap {
				resolvedVal := valType
				if c.typeSubst != nil {
					resolvedVal = types.Substitute(resolvedVal, c.typeSubst)
				}
				if extractNamed(resolvedVal) == types.TypString {
					if ident, ok := s.Value.(*ast.IdentExpr); ok {
						if _, hasFlag := c.dropFlags[ident.Name]; !hasFlag {
							val = c.dupString(val)
						}
					} else if isStringBorrowExpr(s.Value) {
						// B0355: non-ident borrow expr (field access, container element) as map value —
						// the source still owns the pointer; dup so map holds an independent copy.
						val = c.dupString(val)
					}
				}
			}
			// T0599/T0615: Wrap bare RHS in Optional when an array/vector slot
			// type is Optional but the expr is not (mirrors the MemberExpr-LHS
			// path, stmt.go:5485-5528, and the IdentExpr-LHS path,
			// stmt.go:5371-5396). Without this, genArrayIndexAssign /
			// genVectorIndexAssign store a bare T into a {i1, T} slot →
			// "store operands are not compatible".
			//
			// Gated to *types.Array and Vector: both route to a path that does
			// raw NewStore (genArrayIndexAssign / genVectorIndexAssign) with no
			// argument coercion. Vector's []= is `native` so it bypasses the
			// argument-passing coercion that the original T0599 gating assumed
			// would handle the wrap. Map's []= is handled by the dedicated
			// !isArr && !isVec block below (its genMethodIndexAssign also passes
			// val raw, so the Map[K,V?] bare-RHS wrap lives there — T1296);
			// wrapping a Map value here too would double-wrap. idxTargetType is
			// already MutRef/SharedRef-unwrapped above, matching genIndexAssign's
			// own dispatch (stmt.go:8410-8421). For arrays/vectors
			// c.info.Types[target] is the element type (sema checkIndexExpr
			// returns the [] return type), i.e. directly the slot type.
			_, isArr := idxTargetType.(*types.Array)
			_, isVec := types.AsVector(idxTargetType)
			if isArr || isVec {
				slotType := c.info.Types[target]
				exprType := c.info.Types[s.Value]
				if c.typeSubst != nil {
					slotType = types.Substitute(slotType, c.typeSubst)
					exprType = types.Substitute(exprType, c.typeSubst)
				}
				// T1087: strip SharedRef/MutRef before Identical — same as Site 1/2 and T0856.
				if _, isOpt := slotType.(*types.Optional); isOpt && exprType != types.TypNone &&
					!types.Identical(unwrapRefsType(exprType), slotType) {
					// T0394/T0111/T0555 pattern: string & native-handle/container
					// temps are tracked by direct val-identity in stmtTempMap,
					// which fails to match once val becomes the wrapped struct —
					// claim BEFORE wrapping. Heap user-type temps are still
					// claimed correctly post-wrap by claimHeapTemp's
					// struct-extraction fallback (B0233), so they are excluded.
					if extractNamed(exprType) == types.TypString ||
						types.IsVector(exprType) || types.IsChannel(exprType) ||
						types.IsArc(exprType) || types.IsWeak(exprType) ||
						types.IsAnyTask(exprType) || types.IsMutex(exprType) ||
						types.IsMutexGuard(exprType) {
						c.claimStringTemp(val)
					}
					optType := c.resolveType(slotType)
					if st, ok := optType.(*irtypes.StructType); ok {
						// T1298: box/view-coerce into the Optional slot's element type
						// before wrapping, e.g. `arr[i] = Counter(...)` where the slot
						// is a structural-interface Optional.
						val = c.coerceToOptionalElem(val, exprType, slotType)
						val = c.wrapOptional(val, st)
					}
				}
			}
			// T1298: view-coerce a widening RHS (concrete → structural interface, or
			// child → parent) into the []= setter's VALUE param type before the store,
			// for the map / user-defined non-native `[]=` path. The native
			// vector/array slot is handled by the isArr||isVec block above;
			// genMethodIndexAssign passes val straight to the setter with NO argument
			// coercion, so without this a widened value would be stored raw and later
			// read back through a bogus vtable → segfault. Coercing here (in the
			// caller's context) rather than inside genMethodIndexAssign keeps the box
			// as this statement's `val`, so the claimHeapTemp(val) below transfers the
			// box's ownership to the container (freed once via the container's V drop →
			// __promise_structural_drop). No-op when the value already matches.
			if !isArr && !isVec {
				if named := extractNamed(idxTargetType); named != nil {
					if m := named.LookupMethod("[]="); m != nil && !m.IsNative() {
						sigParams := m.Sig().Params()
						if len(sigParams) >= 1 {
							valParamType := sigParams[len(sigParams)-1].Type()
							if inst, ok := idxTargetType.(*types.Instance); ok {
								if origin, ok := inst.Origin().(*types.Named); ok {
									valParamType = types.Substitute(valParamType,
										types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs()))
								}
							}
							exprType := c.info.Types[s.Value]
							if c.typeSubst != nil {
								valParamType = types.Substitute(valParamType, c.typeSubst)
								if exprType != nil {
									exprType = types.Substitute(exprType, c.typeSubst)
								}
							}
							val = c.coerceToView(val, exprType, valParamType)
							val = c.coerceToOptionalElem(val, exprType, valParamType)
							// T1296: wrap a bare RHS into Optional when the []= setter's
							// value param is Optional but the expr is not (e.g. `m[k] = v`
							// on a map[K, Vector[T]?]). genMethodIndexAssign passes val
							// straight to the setter with NO argument coercion, so without
							// this a bare T is passed where {i1, T} is expected → a
							// type-mismatched store, and the slot reads back as `none`.
							// Mirrors the isArr||isVec block above; Map's []= was previously
							// unreachable for a bare RHS because sema rejected it (T1296).
							if _, isOpt := valParamType.(*types.Optional); isOpt &&
								exprType != nil && exprType != types.TypNone &&
								!types.Identical(unwrapRefsType(exprType), valParamType) {
								// String / native-handle / container temps are tracked by
								// val-identity in stmtTempMap and fail to match once val
								// becomes the wrapped struct — claim BEFORE wrapping. Heap
								// user-type temps are claimed correctly post-wrap by
								// claimHeapTemp's struct-extraction fallback (B0233).
								if extractNamed(exprType) == types.TypString ||
									types.IsVector(exprType) || types.IsChannel(exprType) ||
									types.IsArc(exprType) || types.IsWeak(exprType) ||
									types.IsAnyTask(exprType) || types.IsMutex(exprType) ||
									types.IsMutexGuard(exprType) {
									c.claimStringTemp(val)
								}
								if st, ok := c.resolveType(valParamType).(*irtypes.StructType); ok {
									val = c.wrapOptional(val, st)
								}
							}
						}
					}
				}
			}
		}
		c.genIndexAssign(target, s.Op, val, s.Value)
		// Clear drop flag on RHS if it's being moved via simple assign
		if s.Op == ast.OpAssign {
			if ident, ok := s.Value.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
			// T0754: clear cast subject's drop flag — ownership moves it at
			// the owning-slot store, so the subject's scope-exit drop must
			// not fire on the same allocation the container element now owns.
			// T0849: for the conditional `as` form, drop iff the downcast failed.
			if ident := c.castSubjectMovableIdent(s.Value); ident != nil {
				c.consumeCastSubjectDropFlag(s.Value, ident.Name)
			}
			// B0168: Claim string temp — ownership transferred to container.
			c.claimStringTemp(val)
			// B0233: Claim heap temp — ownership transferred to container.
			c.claimHeapTemp(val)
			// T1226: Claim closure env temp — ownership transferred to the container
			// element (native vector/map/array slot, or a user `[]=` that stores it).
			// Without this, a capturing-closure RHS's env is freed at statement end
			// while the element still references it → use-after-free. T1160 widened
			// the trigger to closure-returning call results. No-op for non-closures.
			c.claimEnvTemp(val)
			// T1103/T1323: Clear inline enum constructor temps only when the RHS itself
			// MOVES the enum into the container (e.g. `m[k] = Holder.Pair(...)`, or a
			// match/if producing the enum) — NOT when it is a call whose RESULT happens
			// to be an enum (`v[i] = f(E.V(...))`), where the by-value enum-ctor ARG temp
			// stays owned here and must fall through to the statement-boundary drain (a
			// type-based `extractEnum` check would misfire on the call result and orphan
			// the arg's payload). Without the clear on a genuine move-out, the ctor temp's
			// drop fires at statement end and recursively frees the variant's heap payload
			// the container now owns → use-after-free. Mirrors the var-decl (B0267) and
			// field-assign (B0269) clears.
			// T1340: floor-bounded clear (see clearMovedOutEnumCtorTemps).
			if len(c.enumCtorTemps) > c.blockTempFloorEnum && c.enumCtorTempMovesOut(s.Value) {
				c.clearMovedOutEnumCtorTemps()
			}
			// B0309: When RHS is opt!, neutralize the source optional so its
			// drop doesn't double-free the inner value now owned by the container.
			c.neutralizeForceUnwrapSource(s.Value)
			// T0597: When RHS is an Optional[X] field-read or array-index from a
			// droppable owner, genFieldAccess/genArrayIndex set these sentinels
			// to the bare inner-dup pointer. The wrapped {i1, ptr} struct passed
			// to claimStringTemp/claimHeapTemp above won't match the stmtTempMap
			// entry (which keys on the inner pointer). Without claiming per
			// sentinel here, cleanupStmtTemps drops the inner pointer at
			// statement end, then the container slot's drop at scope exit drops
			// the same pointer again → double-free. Mirrors T0498's per-arg
			// claim in constructor field-init and existing claims at
			// var-decl/return sites.
			if c.optionalStringDup != nil {
				c.claimStringTemp(c.optionalStringDup)
				c.optionalStringDup = nil
			}
			if c.optionalContainerDup != nil {
				c.claimStringTemp(c.optionalContainerDup)
				c.optionalContainerDup = nil
			}
			if c.optionalTupleDup != nil {
				c.claimHeapTemp(c.optionalTupleDup)
				c.optionalTupleDup = nil
			}
			if c.optionalHeapDup != nil {
				c.claimHeapTemp(c.optionalHeapDup)
				c.optionalHeapDup = nil
			}
			// T0899: an operator/method RHS whose body is `return this` returns the
			// borrowed receiver as an owned result, aliasing the operand local. The
			// container element now owns that instance, so clear the operand's drop
			// flag — otherwise both the operand local and the container free it
			// (double-free). Mirrors the IdentExpr branch (T0892) and the two
			// var-decl paths. The self-alias case (`v[0] = v[0] + b`) is handled by
			// genVectorIndexAssign's same-pointer drop-old guard (its non-Ident
			// origin skips the clear); see clearOperandAliasForOwnedStore. The
			// Vector[string]/Map[K,string] sub-paths dup rather than alias and break
			// before here. T1084: no-op given T0893's clone in wrapThisReturnValue.
			c.clearOperandAliasForOwnedStore(s.Value, val)
		}
		// Clear drop flag on index key if it's being stored (e.g., map[key] = val).
		// The map takes ownership of the key pointer.
		if s.Op == ast.OpAssign {
			if ident, ok := target.Index.(*ast.IdentExpr); ok {
				c.clearDropFlag(ident.Name)
			}
			// B0309: When opt! is used as a map key, neutralize the source
			// optional so its drop doesn't double-free the unwrapped key.
			c.neutralizeForceUnwrapSource(target.Index)
		}

	case *ast.SliceExpr:
		c.genSliceAssign(target, val)
		if s.Op == ast.OpAssign {
			if ident, ok := s.Value.(*ast.IdentExpr); ok {
				// B0313: For non-string element types, the [:]= method aliases
				// element pointers; we free the source backing array here, skip
				// normal vecdrop on the source (clearDropFlag) to avoid double-free.
				// T0386: For string element type, Patch 1 makes [:]= dup
				// strings via B0195, so the source retains independent
				// ownership of its elements — running B0313's destructive
				// path would orphan and leak them, and disarming the source's
				// drop flag would leak the source's backing array + element
				// strings. Let normal scope cleanup handle the source vector.
				rhsType := c.info.Types[s.Value]
				if c.typeSubst != nil {
					rhsType = types.Substitute(rhsType, c.typeSubst)
				}
				// T0490: Same skip applies to any element type whose [:]= body
				// dups elements on read — string (T0386), tuple-needs-drop
				// (T0412 dupTupleFieldAccess), heap user-type (T0398
				// dupHeapUserFieldAccess). Symmetric with the dup-flag set
				// in the IndexExpr-RHS branch above. Plain Vector[T]/Channel/
				// Arc/Weak/enum elements still alias on read inside [:]=, so
				// B0313's destructive shallow Vector.drop is correct for them.
				skipB0313 := false
				if elemType, isVec := types.AsVector(rhsType); isVec {
					resolvedElem := elemType
					if c.typeSubst != nil {
						resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
					}
					isTupNeedsDrop := false
					if _, isTup := resolvedElem.(*types.Tuple); isTup && c.tupleNeedsDrop(resolvedElem) {
						isTupNeedsDrop = true
					}
					switch {
					case extractNamed(resolvedElem) == types.TypString:
						skipB0313 = true
					case isTupNeedsDrop:
						skipB0313 = true
					case isDroppableHeapUserType(resolvedElem):
						skipB0313 = true
					default:
						alloca := c.locals[ident.Name]
						srcPtr := c.block.NewLoad(irtypes.I8Ptr, alloca)
						c.block.NewCall(c.funcs["Vector.drop"], srcPtr)
					}
				}
				if !skipB0313 {
					c.clearDropFlag(ident.Name)
				}
			}
			// B0312: When RHS is opt!, neutralize the source optional so its
			// drop doesn't double-free the inner value now owned by the slice target.
			c.neutralizeForceUnwrapSource(s.Value)
		}

	default:
		panic(fmt.Sprintf("codegen: unsupported assignment target %T", s.Target))
	}
}

// genMemberCompoundAssign handles a compound member assignment (`a.b += x`) so
// the receiver is evaluated exactly once and before the RHS, matching the
// canonical order used by the index/slice paths: target → RHS → read → op →
// write (T1353). For a setter property the single receiver value is staged (see
// stagedMemberReceiver / memberReceiver) and reused by both the getter read and
// the setter write. Module-level setters are routed away by the early-dispatch
// guard in genAssignStmt; MutexGuard.borrow and any other non-field/non-setter
// member falls back to the legacy genMemberAssign path (out of scope).
func (c *Compiler) genMemberCompoundAssign(target *ast.MemberExpr, op ast.AssignOp, valueExpr ast.Expr) {
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	named := extractNamed(targetType)

	// Setter-property compound: evaluate the receiver once (target-first), reuse
	// it for the getter read and the setter write. "borrow" (MutexGuard) is a
	// native accessor handled by the fallback below. A `global getter/setter
	// (T0703, Recv() == nil) has no runtime receiver — `Foo.count += x` names a
	// type, not a value — so it has neither the double-eval nor the ordering
	// issue and stays on the legacy fallback path.
	if named != nil && target.Field != "borrow" {
		if setter := named.LookupSetter(target.Field); setter != nil && setter.Sig().Recv() != nil {
			getter := named.LookupGetter(target.Field)
			if getter == nil {
				panic(fmt.Sprintf("codegen: compound assignment to setter %s.%s but no getter found", named, target.Field))
			}
			// T1356: a value-type setter reached through a side-effecting subscript
			// (`vs[next()].sum += x`) must evaluate the subscript exactly once. The
			// generic staged-value path below would re-derive the receiver address in
			// genSetterCall (re-running the index), so instead take the element address
			// once here, load a value copy from it for the getter read, and hand the
			// same address to the setter write. Only IndexExpr can carry side effects
			// in its receiver derivation — locals/`this`/nested members re-derive for
			// free, so they stay on the simpler staged-value path.
			if _, isIndex := target.Target.(*ast.IndexExpr); isIndex && named.IsValueType() {
				if addr, ok := c.genValueTypeReceiverAddr(target.Target); ok { // subscript — evaluated ONCE
					layout := c.lookupTypeLayout(targetType)
					typedAddr := c.block.NewBitCast(addr, irtypes.NewPointer(layout.Value.LLVMType))
					recvVal := c.block.NewLoad(layout.Value.LLVMType, typedAddr) // value copy for the read
					val := c.genExpr(valueExpr)                                  // RHS — after target
					if c.info.AutoPropagateExprs[valueExpr] {
						val = c.genAutoPropagateTracked(valueExpr, val)
					}
					c.stagedMemberReceiver = recvVal
					current := c.genGetterCall(target, targetType, named, getter)         // read
					current = c.unwrapFailableCompoundRead(current, c.info.Types[target]) // T0709
					result := c.genCompoundOp(op, c.info.Types[target], current, val)     // op
					c.stagedMemberReceiverAddr = addr                                     // reuse the single address
					c.genSetterCall(target, targetType, named, setter, result)            // write
					c.stagedMemberReceiver = nil
					c.stagedMemberReceiverAddr = nil
					return
				}
			}
			recv := c.genExprAutoPropagate(target.Target) // target — evaluated ONCE
			val := c.genExpr(valueExpr)                   // RHS — after target
			if c.info.AutoPropagateExprs[valueExpr] {
				val = c.genAutoPropagateTracked(valueExpr, val)
			}
			c.stagedMemberReceiver = recv
			current := c.genGetterCall(target, targetType, named, getter)         // read
			current = c.unwrapFailableCompoundRead(current, c.info.Types[target]) // T0709
			result := c.genCompoundOp(op, c.info.Types[target], current, val)     // op
			c.stagedMemberReceiver = recv
			c.genSetterCall(target, targetType, named, setter, result) // write
			c.stagedMemberReceiver = nil
			return
		}
	}

	// Plain-field compound: the field pointer (receiver) is evaluated once, before
	// the RHS.
	if named != nil && target.Field != "borrow" && named.LookupField(target.Field) != nil {
		fieldPtr := c.genFieldPtr(target) // target — evaluated ONCE
		val := c.genExpr(valueExpr)       // RHS — after target
		if c.info.AutoPropagateExprs[valueExpr] {
			val = c.genAutoPropagateTracked(valueExpr, val)
		}
		c.emitFieldCompoundReadModifyWrite(target, targetType, named, fieldPtr, op, val)
		return
	}

	// Fallback (MutexGuard.borrow and any other special member): preserve prior
	// behavior. Not in scope for the single-eval/ordering fix.
	val := c.genExpr(valueExpr)
	if c.info.AutoPropagateExprs[valueExpr] {
		val = c.genAutoPropagateTracked(valueExpr, val)
	}
	c.genMemberAssign(target, op, val, valueExpr)
}

// genMemberAssign handles assignment to a field on a user type instance.
// If the member is a setter property, emits a setter call instead.
// Uses lookupTypeLayout for layout-driven field types that work for both
// regular and monomorphic types.
// srcExpr (may be nil) is the RHS source AST; used by the T0351 defensive
// dup path in genMutexGuardBorrowSet to detect a borrow-param string.
func (c *Compiler) genMemberAssign(target *ast.MemberExpr, op ast.AssignOp, val value.Value, srcExpr ast.Expr) {
	// Check for setter property
	targetType := c.info.Types[target.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}

	// T0156: MutexGuard.borrow setter — intercept before generic setter lookup.
	// Container types are opaque i8* and need custom setter codegen.
	if target.Field == "borrow" {
		if elem, ok := types.AsMutexGuard(targetType); ok {
			resolvedElem := elem
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(elem, c.typeSubst)
			}
			c.genMutexGuardBorrowSet(target, op, val, resolvedElem, srcExpr)
			return
		}
		guardNamed := extractNamed(targetType)
		if guardNamed == types.TypMutexGuard {
			if tp := c.resolveTypeParam(types.TypMutexGuard.TypeParams()[0]); tp != nil {
				c.genMutexGuardBorrowSet(target, op, val, tp, srcExpr)
				return
			}
		}
	}

	named := extractNamed(targetType)
	if named != nil {
		if setter := named.LookupSetter(target.Field); setter != nil {
			if op != ast.OpAssign {
				// Compound assignment (+=, -=, etc.): read via getter, apply op, write via setter
				getter := named.LookupGetter(target.Field)
				if getter == nil {
					panic(fmt.Sprintf("codegen: compound assignment to setter %s.%s but no getter found", named, target.Field))
				}
				current := c.genGetterCall(target, targetType, named, getter)
				current = c.unwrapFailableCompoundRead(current, c.info.Types[target]) // T0709
				val = c.genCompoundOp(op, c.info.Types[target], current, val)
			}
			c.genSetterCall(target, targetType, named, setter, val)
			return
		}
	}

	fieldPtr := c.genFieldPtr(target)

	if op == ast.OpAssign {
		// B0216/B0219: Drop old field value before reassignment for types that own heap memory.
		// Without this, overwriting a heap-allocated field leaks the old value.
		// Safe because field reads that save to locals create dups (T0095/B0219).
		if named != nil {
			field := named.LookupField(target.Field)
			if field != nil {
				// T0390: Use sema's resolved type for `target` rather than
				// `field.Type()`. Sema substitutes the field's TypeParam through
				// the receiver Instance's TypeArgs, so this is concrete even
				// outside generic context (where `c.typeSubst` is nil) — without
				// this, drop blocks below silently miss generic-typed fields and
				// leak the old value. See also T0368 (same root cause, compound).
				fieldType := c.info.Types[target]
				if c.typeSubst != nil {
					fieldType = types.Substitute(fieldType, c.typeSubst)
				}
				// String: call promise_string_drop (handles null + literal checks).
				if extractNamed(fieldType) == types.TypString {
					if dropFunc, ok := c.funcs["promise_string_drop"]; ok {
						oldVal := c.block.NewLoad(irtypes.I8Ptr, fieldPtr)
						isSame := c.block.NewICmp(enum.IPredEQ, oldVal, val)
						dropBlock := c.newBlock("field.strdrop")
						mergeBlock := c.newBlock("field.strdrop.done")
						c.block.NewCondBr(isSame, mergeBlock, dropBlock)
						c.block = dropBlock
						c.block.NewCall(dropFunc, oldVal)
						c.block.NewBr(mergeBlock)
						c.block = mergeBlock
					}
				}
				// B0219/T0405: Vector: drop elements first, then call Vector.drop (handles null + static flag).
				// Guard: skip if old == new (same pointer) OR old is null (zero-initialized from error
				// fallthrough). emitVectorElementDropLoop reads the header unconditionally, so we must
				// null-check here; Vector.drop has its own internal null check but the loop does not.
				if elemType, isVec := types.AsVector(fieldType); isVec {
					if dropFunc, ok := c.funcs["Vector.drop"]; ok {
						oldVal := c.block.NewLoad(irtypes.I8Ptr, fieldPtr)
						isNull := c.block.NewICmp(enum.IPredEQ, oldVal, constant.NewNull(irtypes.I8Ptr))
						isSame := c.block.NewICmp(enum.IPredEQ, oldVal, val)
						skipDrop := c.block.NewOr(isNull, isSame)
						dropBlock := c.newBlock("field.vecdrop")
						mergeBlock := c.newBlock("field.vecdrop.done")
						c.block.NewCondBr(skipDrop, mergeBlock, dropBlock)
						c.block = dropBlock
						c.emitVectorElementDropLoop(oldVal, elemType)
						c.block.NewCall(dropFunc, oldVal)
						c.block.NewBr(mergeBlock)
						c.block = mergeBlock
					}
				}
				// B0219/T0663: Channel: per-element-type drop (handles null +
				// refcount, and walks any un-received buffered items).
				if chanElem, isCh := types.AsChannel(fieldType); isCh {
					dropFunc := c.getOrCreateChannelDrop(chanElem)
					oldVal := c.block.NewLoad(irtypes.I8Ptr, fieldPtr)
					isSame := c.block.NewICmp(enum.IPredEQ, oldVal, val)
					dropBlock := c.newBlock("field.chdrop")
					mergeBlock := c.newBlock("field.chdrop.done")
					c.block.NewCondBr(isSame, mergeBlock, dropBlock)
					c.block = dropBlock
					c.block.NewCall(dropFunc, oldVal)
					c.block.NewBr(mergeBlock)
					c.block = mergeBlock
				}
				// T0155: Arc: call per-instantiation Arc drop (handles null + refcount).
				if arcElem, isArc := types.AsArc(fieldType); isArc {
					resolvedArcElem := arcElem
					if c.typeSubst != nil {
						resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
					}
					dropFunc := c.getOrCreateArcDrop(resolvedArcElem)
					oldVal := c.block.NewLoad(irtypes.I8Ptr, fieldPtr)
					isSame := c.block.NewICmp(enum.IPredEQ, oldVal, val)
					dropBlock := c.newBlock("field.arcdrop")
					mergeBlock := c.newBlock("field.arcdrop.done")
					c.block.NewCondBr(isSame, mergeBlock, dropBlock)
					c.block = dropBlock
					c.block.NewCall(dropFunc, oldVal)
					c.block.NewBr(mergeBlock)
					c.block = mergeBlock
				}
				// T0157: Weak field reassignment drop.
				if weakElem, isWeak := types.AsWeak(fieldType); isWeak {
					resolvedElem := weakElem
					if c.typeSubst != nil {
						resolvedElem = types.Substitute(weakElem, c.typeSubst)
					}
					dropFunc := c.getOrCreateWeakDrop(resolvedElem)
					oldVal := c.block.NewLoad(irtypes.I8Ptr, fieldPtr)
					isSame := c.block.NewICmp(enum.IPredEQ, oldVal, val)
					dropBlock := c.newBlock("field.weakdrop")
					mergeBlock := c.newBlock("field.weakdrop.done")
					c.block.NewCondBr(isSame, mergeBlock, dropBlock)
					c.block = dropBlock
					c.block.NewCall(dropFunc, oldVal)
					c.block.NewBr(mergeBlock)
					c.block = mergeBlock
				}
				// T0156: Mutex field reassignment drop.
				if mutexElem, isMutex := types.AsMutex(fieldType); isMutex {
					resolvedElem := mutexElem
					if c.typeSubst != nil {
						resolvedElem = types.Substitute(mutexElem, c.typeSubst)
					}
					dropFunc := c.getOrCreateMutexDrop(resolvedElem)
					oldVal := c.block.NewLoad(irtypes.I8Ptr, fieldPtr)
					isSame := c.block.NewICmp(enum.IPredEQ, oldVal, val)
					dropBlock := c.newBlock("field.mutexdrop")
					mergeBlock := c.newBlock("field.mutexdrop.done")
					c.block.NewCondBr(isSame, mergeBlock, dropBlock)
					c.block = dropBlock
					c.block.NewCall(dropFunc, oldVal)
					c.block.NewBr(mergeBlock)
					c.block = mergeBlock
				}
				// T0156: MutexGuard field reassignment drop.
				if types.IsMutexGuard(fieldType) {
					if dropFunc := c.funcs["MutexGuard.drop"]; dropFunc != nil {
						oldVal := c.block.NewLoad(irtypes.I8Ptr, fieldPtr)
						isSame := c.block.NewICmp(enum.IPredEQ, oldVal, val)
						dropBlock := c.newBlock("field.guardrop")
						mergeBlock := c.newBlock("field.guardrop.done")
						c.block.NewCondBr(isSame, mergeBlock, dropBlock)
						c.block = dropBlock
						c.block.NewCall(dropFunc, oldVal)
						c.block.NewBr(mergeBlock)
						c.block = mergeBlock
					}
				}
				// T0560: Task field reassignment drop. Without this, `h.t = go ...`
				// for a plain Task[T] field silently leaks the old G handle (the
				// generic dispatch falls into the heap-user-type catch-all which
				// is gated by !isOpaqueContainerType and so skips Task entirely).
				if taskElem, isTask, taskFail := types.AsAnyTaskFailable(fieldType); isTask {
					resolvedElem := taskElem
					if c.typeSubst != nil {
						resolvedElem = types.Substitute(taskElem, c.typeSubst)
					}
					oldVal := c.block.NewLoad(irtypes.I8Ptr, fieldPtr)
					isSame := c.block.NewICmp(enum.IPredEQ, oldVal, val)
					dropBlock := c.newBlock("field.taskdrop")
					mergeBlock := c.newBlock("field.taskdrop.done")
					c.block.NewCondBr(isSame, mergeBlock, dropBlock)
					c.block = dropBlock
					// T0668: cooperative join in a coroutine body (this runs in
					// user code, often a test body / go {}); legacy spin otherwise.
					c.emitTaskJoinAndFree(oldVal, resolvedElem, taskFail)
					c.block.NewBr(mergeBlock)
					c.block = mergeBlock
				}
				// B0240: Optional fields: drop old inner value before reassignment.
				// When overwriting an optional field (e.g., p.location = none), the old
				// inner value must be freed/dropped to prevent memory leaks.
				if opt, ok := fieldType.(*types.Optional); ok {
					c.emitOptionalFieldReassignDrop(opt, field, targetType, fieldPtr)
				}
				// T0410: Heap user-type fields: drop old instance before reassignment.
				// Without this, `h.f = v[0]` (after dup-on-read clones the RHS via the
				// flag set in genAssignStmt) leaks h.f's previous instance. Symmetric
				// with genVectorIndexAssign's heap user-type branch. Null + same-
				// pointer guard mirrors the existing Vector/Channel branches above.
				// T0908: also cover heap user types with NO drop (isHeapUserNoDropPalFree);
				// emitVariantFieldDrop's B0218 branch pal_frees the old no-drop instance.
				if isDroppableHeapUserType(fieldType) || isHeapUserNoDropPalFree(fieldType) || isMapOrSetType(fieldType) {
					fieldLLVM := c.resolveType(fieldType)
					oldVal := c.block.NewLoad(fieldLLVM, fieldPtr)
					oldInstance := c.extractInstancePtr(oldVal)
					newInstance := c.extractInstancePtr(val)
					isNull := c.block.NewICmp(enum.IPredEQ, oldInstance, constant.NewNull(irtypes.I8Ptr))
					isSame := c.block.NewICmp(enum.IPredEQ, oldInstance, newInstance)
					skipDrop := c.block.NewOr(isNull, isSame)
					dropBlock := c.newBlock("field.userdrop")
					mergeBlock := c.newBlock("field.userdrop.done")
					c.block.NewCondBr(skipDrop, mergeBlock, dropBlock)
					c.block = dropBlock
					c.emitVariantFieldDrop(oldVal, fieldType)
					c.block.NewBr(mergeBlock)
					c.block = mergeBlock
				}
				// T0489: Tuple field reassignment drop. Without this, `obj.tup_field = X`
				// for droppable tuple types leaks the old tuple's heap fields (string,
				// vector buffer, nested user types). emitVariantFieldDrop's tuple branch
				// walks each element via ExtractValue + recursive drop. Mirrors the
				// heap-user-type T0410 branch above and the genVectorIndexAssign tuple
				// branch (T0412). Safe because Part 2 (dup-on-read in genAssignStmt)
				// ensures aliased RHS reads from containers produce independent clones.
				if c.tupleNeedsDrop(fieldType) {
					fieldLLVM := c.resolveType(fieldType)
					oldVal := c.block.NewLoad(fieldLLVM, fieldPtr)
					c.emitVariantFieldDrop(oldVal, fieldType)
				}
				// T1226: Closure-typed field reassignment drop. Overwriting a
				// closure field must drop the old closure's heap env (its captured
				// values), else it leaks. emitVariantFieldDrop's Signature case
				// (T0739) null-checks the env ptr and deep-drops via
				// emitEnvDropOrFree. Skip when old env == new env (self-alias) and
				// when the old env is null (no-capture closure). Paired with
				// claimEnvTemp on the RHS in genAssignStmt so the new env's temp is
				// not freed at statement end (it's now owned by the field).
				if _, isSig := fieldType.(*types.Signature); isSig {
					fieldLLVM := c.resolveType(fieldType)
					oldVal := c.block.NewLoad(fieldLLVM, fieldPtr)
					if st, ok := oldVal.Type().(*irtypes.StructType); ok && len(st.Fields) == 2 {
						oldEnv := c.block.NewExtractValue(oldVal, 1)
						newEnv := c.block.NewExtractValue(val, 1)
						isNull := c.block.NewICmp(enum.IPredEQ, oldEnv, constant.NewNull(irtypes.I8Ptr))
						isSame := c.block.NewICmp(enum.IPredEQ, oldEnv, newEnv)
						skipDrop := c.block.NewOr(isNull, isSame)
						dropBlock := c.newBlock("field.closuredrop")
						mergeBlock := c.newBlock("field.closuredrop.done")
						c.block.NewCondBr(skipDrop, mergeBlock, dropBlock)
						c.block = dropBlock
						c.emitVariantFieldDrop(oldVal, fieldType)
						c.block.NewBr(mergeBlock)
						c.block = mergeBlock
					}
				}
			}
		}
		c.block.NewStore(val, fieldPtr)
		// T0909: When RHS is a method/operator whose body is `return this`,
		// the returned value aliases the receiver. Clear the receiver's drop
		// flag so scope-exit doesn't double-free the instance now owned by
		// this field.
		if srcExpr != nil {
			var aliasOrigin ast.Expr
			if call, ok := srcExpr.(*ast.CallExpr); ok {
				aliasOrigin = chainOriginExpr(call)
			} else {
				aliasOrigin = operatorReceiverOrigin(srcExpr)
			}
			fieldType := c.info.Types[target]
			if c.typeSubst != nil {
				fieldType = types.Substitute(fieldType, c.typeSubst)
			}
			if id, ok := aliasOrigin.(*ast.IdentExpr); ok {
				c.maybeClearReceiverDropFlag(val, id.Name, fieldType)
			}
			// ThisExpr origin: inside a method body `this` has no per-variable
			// drop flag (callers own the instance), so no clear needed.
		}
		return
	}

	c.emitFieldCompoundReadModifyWrite(target, targetType, named, fieldPtr, op, val)
}

// emitFieldCompoundReadModifyWrite performs the read-modify-write tail of a
// plain-field compound assignment (`a.b += x`): load the current field value,
// apply the compound operator, drop the old value (string / heap user type), and
// store the result back through fieldPtr. The receiver (fieldPtr) is evaluated
// exactly once by the caller. Shared by genMemberAssign's defensive compound
// branch and genMemberCompoundAssign (T1353).
func (c *Compiler) emitFieldCompoundReadModifyWrite(target *ast.MemberExpr, targetType types.Type, named *types.Named, fieldPtr value.Value, op ast.AssignOp, val value.Value) {
	// Compound assignment: resolve field LLVM type for load
	layout := c.lookupTypeLayout(targetType)
	field := named.LookupField(target.Field)
	var fieldLLVMType irtypes.Type
	if layout.IsValueType {
		fieldIdx := layout.ValueFieldIndex[field.Name()]
		fieldLLVMType = layout.Value.Fields[fieldIdx].LLVMType
	} else {
		fieldIdx := layout.InstanceFieldIndex[field.Name()]
		fieldLLVMType = layout.Instance.Fields[fieldIdx].LLVMType
	}
	current := c.block.NewLoad(fieldLLVMType, fieldPtr)
	// T0368: Use sema's resolved type for `target` rather than `field.Type()`.
	// Sema's resolveInstanceMember substitutes the field's TypeParam through
	// the receiver Instance's TypeArgs, so this is the concrete operand type
	// even outside generic context (where `c.typeSubst` is nil).
	fieldType := c.info.Types[target]
	result := c.genCompoundOp(op, fieldType, current, val)
	// T0363/T0715: Drop the old field value before storing the new one. Without
	// this, heap-allocated old values leak. String uses the dedicated runtime-
	// drop helper; a non-native operator on a heap user type (or enum) returns a
	// fresh value, so the old one is dropped via the alias-guarded helper (no-op
	// for value types/scalars).
	if c.typeSubst != nil {
		fieldType = types.Substitute(fieldType, c.typeSubst)
	}
	if extractNamed(fieldType) == types.TypString {
		c.emitStringDropOldValue(current, result)
	} else {
		c.dropOldUserValueAtPtr(fieldPtr, fieldType, result)
	}
	c.block.NewStore(result, fieldPtr)
}

// genSetterCall emits a call to a setter method.
// Uses virtual dispatch through the vtable when the static type needs it.
func (c *Compiler) genSetterCall(target *ast.MemberExpr, targetType types.Type, named *types.Named, setter *types.Method, val value.Value) {
	// Virtual dispatch for setter when static type needs vtable. A `global setter
	// (T0703) has no receiver — a static call on the type name — so it always
	// dispatches directly (T1749).
	if c.needsVtable(named) && !setter.IsNative() && setter.Sig().Recv() != nil {
		c.genVirtualSetterCall(target, named, setter, val)
		return
	}

	var mangledName string
	ownerName := c.resolveMethodOwner(named, target.Field)
	if ownerName != named.Obj().Name() {
		// A default setter from a structural interface is synthesized per-concrete
		// (T1559), mirroring genGetterCall / genMethodCall: use the concrete type's
		// name, not the (possibly generic) interface's.
		if structParent := c.findStructuralOwnerBy(named, target.Field, (*types.Named).LookupSetter); structParent != nil {
			concreteName := c.resolveTypeName(targetType)
			c.ensureDefaultMethodsSynthesized(named, structParent)
			mangledName = mangleMethodName(concreteName, target.Field, true)
		} else {
			// T0637: Non-structural parent. Resolve to mono name if parent is
			// generic (mirrors genGetterCall / genMethodCall).
			monoOwner := c.resolveMonoParentName(named, targetType, ownerName)
			mangledName = mangleMethodName(monoOwner, target.Field, true)
		}
	} else {
		mangledName = mangleMethodName(c.resolveTypeName(targetType), target.Field, true)
	}

	fn, ok := c.funcs[mangledName]
	if !ok {
		panic(fmt.Sprintf("codegen: undeclared setter %s", mangledName))
	}

	if setter.Sig().Recv() == nil {
		// `global setter: no receiver argument (T0703/T1749).
		call := c.block.NewCall(fn, val)
		c.propagateIfFailable(call) // T0708
		return
	}

	var args []value.Value
	// T1353: evaluate the receiver once (target-first) — memberReceiver reuses a
	// value staged by genMemberCompoundAssign for a compound `a.b += x`, so the
	// getter read and this setter write share the single evaluation. When nothing
	// is staged it falls back to a plain genExpr. The genExpr is deferred into the
	// branches that need a loaded value so the T1356 address path doesn't
	// double-evaluate the receiver base (it re-derives storage from target.Target).
	if isThisReceiver(target.Target) {
		args = append(args, c.memberReceiver(func() value.Value { return c.genExpr(target.Target) }))
	} else if isContainerType(targetType) {
		args = append(args, c.memberReceiver(func() value.Value { return c.genExpr(target.Target) }))
	} else if named != nil && named.IsValueType() {
		// T1356: mutate the receiver's in-place storage — a local, `this`, a nested
		// value-type member (o.inner), or a container element (vs[0]). Only a truly
		// non-addressable receiver (e.g. a value returned by a call) keeps the
		// spill. Subsumes the T1354 local-Ident special case.
		if addr := c.stagedMemberReceiverAddr; addr != nil {
			// A compound assign through a side-effecting subscript already took the
			// element address once; reuse it so the index is not re-evaluated (T1356).
			c.stagedMemberReceiverAddr = nil
			c.stagedMemberReceiver = nil
			args = append(args, addr)
		} else if ptr, ok := c.genValueTypeReceiverAddr(target.Target); ok {
			c.stagedMemberReceiver = nil // drop any staged value; the address path re-derives storage
			args = append(args, ptr)
		} else {
			recv := c.memberReceiver(func() value.Value { return c.genExpr(target.Target) })
			args = append(args, c.valueTypeReceiverPtr(recv, targetType))
		}
	} else {
		args = append(args, c.extractInstancePtr(c.memberReceiver(func() value.Value { return c.genExpr(target.Target) })))
	}
	args = append(args, val)
	call := c.block.NewCall(fn, args...)
	c.propagateIfFailable(call) // T0708
}

// genVirtualSetterCall emits an indirect setter call through the vtable.
func (c *Compiler) genVirtualSetterCall(target *ast.MemberExpr, named *types.Named, setter *types.Method, val value.Value) {
	receiverVal := c.memberReceiver(func() value.Value { return c.genExpr(target.Target) }) // T1353

	var vtableRaw, instance value.Value
	if isThisReceiver(target.Target) {
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
	}

	slotIndex := named.VirtualMethodIndex(target.Field, true) // setter slot
	if slotIndex < 0 {
		panic(fmt.Sprintf("codegen: setter %s not in vtable for %s", target.Field, named))
	}
	vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
	fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
		constant.NewInt(irtypes.I32, int64(slotIndex)))
	fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

	// Setter signature: (i8* receiver, ValueType val) → void
	valType := c.resolveType(setter.Sig().Params()[0].Type())
	paramTypes := []irtypes.Type{irtypes.I8Ptr, valType}
	retType := irtypes.Type(irtypes.Void)
	if setter.Sig().CanError() {
		retType = computeResultType(retType)
	}
	funcType := irtypes.NewFunc(retType, paramTypes...)
	fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))

	call := c.block.NewCall(fnTyped, instance, val)
	c.propagateIfFailable(call) // T0708
}

// genModuleSetterAssign handles assignment to a module-level setter property.
// For compound assignment (+=, -=, etc.), calls getter first, applies op, then calls setter.
func (c *Compiler) genModuleSetterAssign(target *ast.MemberExpr, moduleName string, op ast.AssignOp, val value.Value) {
	setterKey := moduleName + "." + target.Field + "$set"
	setterFn, ok := c.moduleFuncs[setterKey]
	if !ok {
		panic(fmt.Sprintf("codegen: undefined module setter %s.%s", moduleName, target.Field))
	}

	if op != ast.OpAssign {
		// Compound assignment: call getter, apply op, then call setter
		getterKey := moduleName + "." + target.Field
		getterFn, ok := c.moduleFuncs[getterKey]
		if !ok {
			panic(fmt.Sprintf("codegen: compound assignment to module setter %s.%s but no getter found", moduleName, target.Field))
		}
		var current value.Value = c.block.NewCall(getterFn)
		current = c.unwrapFailableCompoundRead(current, c.info.Types[target]) // T0709
		val = c.genCompoundOp(op, c.info.Types[target], current, val)
	}

	call := c.block.NewCall(setterFn, val)
	c.propagateIfFailable(call) // T0708
}

// genCompoundOp applies a compound assignment operator through the type system.
// operandType is the AST type of the operand (current value being modified).
// Required because compound assignment on i8*-shaped types (string, vector,
// channel, etc.) cannot reverse-resolve from the LLVM type alone (T0357).
func (c *Compiler) genCompoundOp(op ast.AssignOp, operandType types.Type, current, val value.Value) value.Value {
	// Map compound op to binary operator name
	var binOp string
	switch op {
	case ast.OpAddAssign:
		binOp = "+"
	case ast.OpSubAssign:
		binOp = "-"
	case ast.OpMulAssign:
		binOp = "*"
	case ast.OpDivAssign:
		binOp = "/"
	case ast.OpModAssign:
		binOp = "%"
	default:
		panic(fmt.Sprintf("codegen: unsupported compound assignment %s", op))
	}

	if operandType == nil {
		panic(fmt.Sprintf("codegen: missing operand type for compound assignment %s", op))
	}
	if c.typeSubst != nil {
		operandType = types.Substitute(operandType, c.typeSubst)
	}
	if c.selfSubst != nil {
		operandType = types.SubstituteSelf(operandType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	named := extractNamed(operandType)
	if named == nil {
		// T1015: enum operand with a user-defined operator. extractNamed returns
		// nil for enums, so dispatch via the enum-specific path (mirrors
		// genBinaryExpr's enum fallback, T0876). Enums are never native operators.
		if en := extractEnum(operandType); en != nil {
			return c.genNonNativeEnumCompoundOp(en, operandType, binOp, current, val)
		}
		panic(fmt.Sprintf("codegen: cannot resolve Named type from %s for compound assignment %s", operandType, op))
	}

	// Binary operator: prefer the 1-param variant so a type that also declares a
	// prefix-unary form of the same symbol (e.g. `-`) dispatches correctly (T0883),
	// matching genBinaryExpr.
	method := named.LookupBinaryMethod(binOp)
	if method == nil {
		method = named.LookupMethod(binOp)
	}
	if method == nil {
		panic(fmt.Sprintf("codegen: no method %q on type %s for compound assignment", binOp, named))
	}

	if method.IsNative() {
		// String operators dispatch to runtime intrinsics (mirrors genBinaryOp).
		// The concat result is intentionally NOT tracked as a stmt temp here:
		// every caller stores the result into a location that owns it (local
		// alloca, field, vector slot, map slot, MutexGuard) so cleanup at
		// scope exit handles drop. Tracking here would conflict with stores
		// to non-local sites that don't claim, causing use-after-free.
		if named == types.TypString {
			return c.genStringOp(binOp, current, val)
		}
		return c.emitNativeOp(named, binOp, current, val)
	}

	// T0715: user-defined (non-native) operator — dispatch a real method call.
	// The result feeds the existing store/setter path; ownership tracking is
	// deliberately omitted (same reason as the native branch above).
	return c.genNonNativeCompoundOp(named, operandType, method, binOp, current, val)
}

// dropOldUserValueAtPtr drops the heap-owned value currently stored at ptr
// before an inc/dec store-back overwrites it (T0880). `x++` is `x = x.++()`: a
// non-native operator borrows the receiver and returns a NEW value, so the old
// one leaks unless dropped (zero-leak policy). For a heap user type the drop is
// guarded by a null + instance-pointer alias check, so a `return this` result
// (which aliases the old value) is never freed. Value types and primitives own
// no heap memory, making this a no-op for them. emitVariantFieldDrop is the
// shared per-type drop walk; for an enum with no droppable data (a `copy` /
// fieldless enum) it emits nothing.
//
// Enum caveat (T0922): the enum branch has no alias guard. A sane cycling
// operator returns a FRESH variant, so dropping the old payload is correct and
// leak-free (the realistic case). But an operator that returns a value aliasing
// the receiver payload (e.g. `++() E => this`) double-frees that payload — the
// same pre-existing enum receiver-alias hole that already affects plain methods
// (`f := e.dup()` where `dup` returns `this`), since emitReceiverAliasCheck does
// not cover enum-value receivers. The proper fix (reject the aliasing return in
// ownership, or extend receiver-alias-clear to enums) is tracked in T0922; a
// bytewise guard here would be a partial proxy that breaks on multi-field
// variants, so it is deliberately not added.
func (c *Compiler) dropOldUserValueAtPtr(ptr value.Value, valueType types.Type, newVal value.Value) {
	if c.typeSubst != nil {
		valueType = types.Substitute(valueType, c.typeSubst)
	}
	if c.selfSubst != nil {
		valueType = types.SubstituteSelf(valueType, c.selfSubst.iface, c.selfSubst.concrete)
	}
	llvmType := c.resolveType(valueType)

	if isDroppableHeapUserType(valueType) || isHeapUserNoDropPalFree(valueType) {
		oldVal := c.block.NewLoad(llvmType, ptr)
		oldInstance := c.extractInstancePtr(oldVal)
		newInstance := c.extractInstancePtr(newVal)
		isNull := c.block.NewICmp(enum.IPredEQ, oldInstance, constant.NewNull(irtypes.I8Ptr))
		isSame := c.block.NewICmp(enum.IPredEQ, oldInstance, newInstance)
		skipDrop := c.block.NewOr(isNull, isSame)
		dropBlock := c.newBlock("incdec.userdrop")
		mergeBlock := c.newBlock("incdec.userdrop.done")
		c.block.NewCondBr(skipDrop, mergeBlock, dropBlock)
		c.block = dropBlock
		c.emitVariantFieldDrop(oldVal, valueType)
		c.block.NewBr(mergeBlock)
		c.block = mergeBlock
		return
	}

	// Non-`copy` enum: `++`/`--` returns a fresh value, so drop the old one.
	// (For a copy/fieldless enum emitVariantFieldDrop emits nothing.)
	if extractEnum(valueType) != nil {
		oldVal := c.block.NewLoad(llvmType, ptr)
		c.emitVariantFieldDrop(oldVal, valueType)
	}
}

// --- Return ---

// isEnumConstructorExpr reports whether expr (after peeling parens) is directly an
// enum variant constructor — either a data-carrying call `E.Variant(args)` or a
// fieldless value `E.Variant`. Used by genReturnStmt (T1317) to distinguish a
// returned enum-ctor temp (moved out to the caller) from a by-value enum-ctor
// argument of a call being returned (owned here, must be drained).
func (c *Compiler) isEnumConstructorExpr(expr ast.Expr) bool {
	expr = unwrapDestructureParens(expr)
	var member *ast.MemberExpr
	switch e := expr.(type) {
	case *ast.CallExpr:
		m, ok := e.Callee.(*ast.MemberExpr)
		if !ok {
			return false
		}
		member = m
	case *ast.MemberExpr:
		member = e
	default:
		return false
	}
	targetType := c.info.Types[member.Target]
	if c.typeSubst != nil {
		targetType = types.Substitute(targetType, c.typeSubst)
	}
	if enumLayout := c.lookupEnumLayout(targetType); enumLayout != nil {
		if _, isVariant := enumLayout.VariantTag[member.Field]; isVariant {
			return true
		}
	}
	// Generic enum in mono context: target is bare *types.Enum; consult the
	// expression's (substituted) result type layout instead.
	if _, ok := targetType.(*types.Enum); ok {
		resultType := c.info.Types[expr]
		if c.typeSubst != nil {
			resultType = types.Substitute(resultType, c.typeSubst)
		}
		if enumLayout := c.lookupEnumLayout(resultType); enumLayout != nil {
			if _, isVariant := enumLayout.VariantTag[member.Field]; isVariant {
				return true
			}
		}
	}
	return false
}

// enumCtorTempMovesOut reports whether the enum-ctor temps tracked while
// evaluating an expression are the value being moved OUT of the statement — into
// the caller (return), a variable binding, or a container slot (flags must be
// cleared, no drop here) — versus by-value call arguments left owned here (must be
// drained). T1317/T1323. Two move-out shapes:
//   - the expression is ITSELF an enum constructor (`E.Variant(...)`)
//   - the expression is a branch (`match s {...}` / `if c {...}`) whose arm values
//     are enum constructors and whose result type is that enum — the arm ctor
//     temps are the phi'd result, moved out.
//
// A call (`f(E.Variant(...))`) is NEITHER: its result is a distinct value and the
// ctor is a by-value argument the callee borrows/dups (B0232), so the temp stays
// owned here and must be drained — even when f itself returns an enum (the T1323
// leak: a type-based `extractEnum(resultType) != nil` check misfires here because
// the call RESULT is also an enum, clearing the arg temp and orphaning its payload).
func (c *Compiler) enumCtorTempMovesOut(expr ast.Expr) bool {
	expr = unwrapDestructureParens(expr)
	if c.isEnumConstructorExpr(expr) {
		return true
	}
	switch expr.(type) {
	case *ast.MatchExpr, *ast.IfExpr:
		resultType := c.info.Types[expr]
		if c.typeSubst != nil {
			resultType = types.Substitute(resultType, c.typeSubst)
		}
		return extractEnum(resultType) != nil
	}
	return false
}
