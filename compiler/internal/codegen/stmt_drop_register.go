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

// --- drop binding ---

// isTypeDroppable returns true if maybeRegisterDrop would register a drop binding
// for a variable of this type. Used by the return-alias check (B0345) to decide
// whether a return value could alias a droppable argument.
func isTypeDroppable(typ types.Type) bool {
	if enum := extractEnum(typ); enum != nil {
		if enum.HasDrop() {
			return true
		}
		if inst, ok := typ.(*types.Instance); ok && monoEnumInstNeedsSynthDrop(inst) {
			return true
		}
		return false
	}
	// T0371: Tuples with droppable fields require a tuple-walk drop binding.
	// Recurse into fields — pure-value tuples (int, bool, etc.) are not droppable.
	if tup, ok := typ.(*types.Tuple); ok {
		for _, e := range tup.Elems() {
			if isTypeDroppable(e) {
				return true
			}
		}
		return false
	}
	// T0391: Optional[T] is droppable iff T is droppable. Recurse so the return-alias
	// check sees through any number of Optional wrappings to reach the inner type.
	if opt, ok := typ.(*types.Optional); ok {
		return isTypeDroppable(opt.Elem())
	}
	// T1227: a closure value (function value) owns a heap-allocated env struct
	// freed by a bindingFreeEnv at scope exit — it is single-owner/move-only,
	// exactly like the native handles above. Recognizing it here lets the
	// return-alias machinery (emitReturnAliasCheckSubst) clear a borrowed closure
	// param's env-free flag when a call hands the same closure back (`identity(g)`).
	if _, ok := typ.(*types.Signature); ok {
		return true
	}
	named := extractNamed(typ)
	if named == nil {
		return false
	}
	if named == types.TypString {
		return true
	}
	if _, ok := types.AsVector(typ); ok || named == types.TypVector {
		return true
	}
	if _, ok := types.AsChannel(typ); ok || named == types.TypChannel {
		return true
	}
	if _, ok := types.AsArc(typ); ok || named == types.TypArc {
		return true
	}
	if _, ok := types.AsWeak(typ); ok || named == types.TypWeak {
		return true
	}
	if _, ok := types.AsMutex(typ); ok || named == types.TypMutex {
		return true
	}
	if _, ok := types.AsMutexGuard(typ); ok || named == types.TypMutexGuard {
		return true
	}
	// T1102: Task[T] is a single-owner native handle (opaque i8* G-struct ptr)
	// with a registered drop (Task[int].drop), exactly like Mutex/MutexGuard.
	// It was missing here, so the return-alias check (emitReturnAliasCheckSubst)
	// returned early for task-returning calls and never cleared the source arg's
	// drop flag — returning a borrowed task param then double-freed the handle.
	// Must agree with the drop-registration path; keep adjacent to the other
	// single-owner handles.
	if _, ok := types.AsAnyTask(typ); ok || types.IsTaskLikeOrigin(named) {
		return true
	}
	if isContainerType(typ) {
		return false
	}
	if named.HasDrop() || named.NeedsSynthDrop() {
		return true
	}
	if inst, ok := typ.(*types.Instance); ok && monoInstNeedsSynthDrop(inst) {
		return true
	}
	if !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural() {
		return true
	}
	return false
}

// consumeElvisBoundDropFlag replaces the unconditional owning drop maybeRegisterDrop
// just stored into a bound variable's drop flag with the per-path flag computed by a
// bound elvis `m := a ?: b` (T0940/T0981). Called from both var-decl paths
// immediately after maybeRegisterDrop. No-op (and resets the field) when the RHS was
// not a bound droppable elvis. The `dropFlags` guard makes a non-droppable bound
// result a safe no-op.
func (c *Compiler) consumeElvisBoundDropFlag(name string) {
	if c.elvisBoundDropFlag == nil {
		return
	}
	if lhsFlag, ok := c.dropFlags[name]; ok {
		c.block.NewStore(c.elvisBoundDropFlag, lhsFlag)
	}
	c.elvisBoundDropFlag = nil
}

// applyBoundMergeFlag replaces the unconditional owning drop that maybeRegisterDrop
// just stored into a bound/reassigned local's drop flag with the PER-PATH ownership
// flag of a mixed owned/borrowed match/if result (T1209). `flag` is the live flag
// captured by captureLiveTempFlag BEFORE claimStringTemp neutralized the merge temp:
// 1 on paths that selected an owned arm, 0 on paths that selected a borrowed
// (param/field) arm. Without this the binding drops a borrowed value on the borrowed
// path (double-free / use-after-free). No-op when flag is nil (RHS was not a per-path
// merge temp) or the binding has no i1 drop flag (value/scalar/structural types).
func (c *Compiler) applyBoundMergeFlag(name string, flag value.Value) {
	if flag == nil {
		return
	}
	if lhsFlag, ok := c.dropFlags[name]; ok {
		c.block.NewStore(flag, lhsFlag)
	}
}

// isMixedMergeBindingRHS reports whether a bound RHS expression is a `match`/`if`
// whose result may be a mixed owned/borrowed merge temp (T1209). Only these
// constructs keep a BORROWED arm (a borrowed param/field) ALIASED through the
// binding, so only they need the merge temp's per-path flag threaded into the
// binding's drop flag. Other perPathFlag temps must be excluded — notably the
// member optional handler `owner.field? _ { recovery }` (ErrorHandlerExpr, T1162),
// whose binding MOVES the present value out of the owner's field, so the binding
// owns it unconditionally and applying the (stale, present=borrowed) per-path flag
// would suppress the sole drop → leak. Parens are unwrapped.
func isMixedMergeBindingRHS(expr ast.Expr) bool {
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = p.Expr
	}
	switch expr.(type) {
	case *ast.IfExpr, *ast.MatchExpr:
		return true
	}
	return false
}

// maybeRegisterBorrowParamReassignDrop gives a borrow-by-default heap param a
// function-scoped drop obligation for any value it is *reassigned* to inside the
// body (T1194). A borrow param carries no drop binding — the caller owns the
// original — so a plain `c = fresh()`, compound `c += k`, or `c++`/`c--` stores a
// fresh owned value into the param slot that is otherwise never tracked and leaks
// at scope exit (and, for the compound/inc-dec forms, the unconditional drop-old
// in dropOldUserValueAtPtr would double-free the caller-owned original).
//
// The fix is narrow and flag-based: register the same binding maybeRegisterDrop
// would for an owned local, but initialise its drop flag to 0 (borrowed original,
// not owned). The drop machinery (emitDropCall / emitFreeCall / emitStringDropCall
// / emitEnumDropCall) is already flag-guarded, so a flag-0 binding drops nothing
// at scope exit and nothing on the first reassignment's drop-old — exactly the
// borrowed-original semantics. A reassignment arms the flag to 1, so the fresh
// value *is* dropped. Ownership analysis forbids moving a borrowed param's
// original out, which bounds the perturbation: a flag-0 binding cannot create a
// double-free through a move.
//
// Registration is gated on the param actually being reassigned in the body so
// the (large) set of read-only borrow params is behaviourally unchanged.
func (c *Compiler) maybeRegisterBorrowParamReassignDrop(name string, alloca *ir.InstAlloca, typ types.Type, ref types.RefMod, body *ast.Block) {
	if name == "" || name == "_" {
		return
	}
	// RefMut (~) params are owned and already handled by maybeRegisterDrop.
	if ref == types.RefMut {
		return
	}
	// Reference-typed params (T&/T~) never own the pointee.
	switch typ.(type) {
	case *types.MutRef, *types.SharedRef:
		return
	}
	// A binding may already exist (e.g. T0322 plain-heap params of a `new`
	// constructor, or variadic/tuple params). Don't double-register.
	if _, ok := c.dropBindings[name]; ok {
		return
	}
	if !identReassignedInBlock(body, name) {
		return
	}
	c.maybeRegisterDrop(name, alloca, typ)
	// If maybeRegisterDrop created a flag (heap/droppable type), mark the
	// borrowed original as not-owned so nothing is dropped until a reassignment
	// arms the flag. Value/copy/scalar/structural types get no flag → no-op.
	if flag, ok := c.dropFlags[name]; ok {
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), flag)
	}
}

// identReassignedInBlock reports whether `name` is the target of a plain/compound
// assignment, an inc/dec, or a classic-for update anywhere in block, recursing
// into nested control-flow blocks (T1194). It is deliberately conservative about
// inner-scope shadowing and may over-report — that is safe: an over-reported
// param gets a flag-0 binding that drops nothing at scope exit unless an actual
// reassignment arms it, and ownership analysis blocks illegal moves of the
// original. Missing an exotic nesting (e.g. a reassignment buried in a
// block-bearing expression this walk doesn't reach) is not a regression: it
// merely leaves the pre-existing T1194 leak/double-free unfixed there, matching
// current behaviour (dropOldUserValueAtIdentSlot falls back to the old
// unconditional drop when no flag exists).
func identReassignedInBlock(block *ast.Block, name string) bool {
	if block == nil {
		return false
	}
	for _, s := range block.Stmts {
		if stmtReassignsIdent(s, name) {
			return true
		}
	}
	return false
}

func stmtReassignsIdent(s ast.Stmt, name string) bool {
	switch st := s.(type) {
	case *ast.AssignStmt:
		if identNamed(st.Target, name) {
			return true
		}
		return exprReassignsIdent(st.Value, name)
	case *ast.IncDecStmt:
		return identNamed(st.Target, name)
	case *ast.ExprStmt:
		return exprReassignsIdent(st.Expr, name)
	case *ast.TypedVarDecl:
		return exprReassignsIdent(st.Value, name)
	case *ast.InferredVarDecl:
		return exprReassignsIdent(st.Value, name)
	case *ast.DestructureVarDecl:
		return exprReassignsIdent(st.Value, name)
	case *ast.UseVarDecl:
		return exprReassignsIdent(st.Value, name)
	case *ast.ReturnStmt:
		return exprReassignsIdent(st.Value, name)
	case *ast.RaiseStmt:
		return exprReassignsIdent(st.Value, name)
	case *ast.YieldStmt:
		return exprReassignsIdent(st.Value, name)
	case *ast.YieldDelegateStmt:
		return exprReassignsIdent(st.Value, name)
	case *ast.Block:
		return identReassignedInBlock(st, name)
	case *ast.IfStmt:
		if exprReassignsIdent(st.Cond, name) || exprReassignsIdent(st.Init, name) {
			return true
		}
		if identReassignedInBlock(st.Body, name) {
			return true
		}
		return stmtReassignsIdent(st.Else, name)
	case *ast.ForInStmt:
		return exprReassignsIdent(st.Iterable, name) || identReassignedInBlock(st.Body, name)
	case *ast.ClassicForStmt:
		if exprReassignsIdent(st.InitValue, name) || exprReassignsIdent(st.Cond, name) {
			return true
		}
		if st.UpdateTarget != nil && identNamed(st.UpdateTarget, name) {
			return true
		}
		if exprReassignsIdent(st.UpdateValue, name) {
			return true
		}
		return identReassignedInBlock(st.Body, name)
	case *ast.InfiniteLoop:
		return identReassignedInBlock(st.Body, name)
	case *ast.WhileStmt:
		return exprReassignsIdent(st.Cond, name) || identReassignedInBlock(st.Body, name)
	case *ast.WhileUnwrapStmt:
		if exprReassignsIdent(st.Value, name) || identReassignedInBlock(st.Body, name) {
			return true
		}
		return false
	case *ast.SelectStmt:
		for _, cs := range st.Cases {
			for _, cst := range cs.Body {
				if stmtReassignsIdent(cst, name) {
					return true
				}
			}
		}
		for _, dst := range st.Default {
			if stmtReassignsIdent(dst, name) {
				return true
			}
		}
		return false
	}
	return false
}

// exprReassignsIdent recurses into block-bearing expressions (match/if/error-
// handler/go/unsafe/lambda bodies) and composite expressions so a reassignment
// statement nested inside an expression-carried block is still detected (T1194).
func exprReassignsIdent(e ast.Expr, name string) bool {
	switch ex := e.(type) {
	case nil:
		return false
	case *ast.MatchExpr:
		if exprReassignsIdent(ex.Subject, name) {
			return true
		}
		for _, arm := range ex.Arms {
			if exprReassignsIdent(arm.Guard, name) || exprReassignsIdent(arm.Body, name) {
				return true
			}
			if identReassignedInBlock(arm.Block, name) {
				return true
			}
		}
		return false
	case *ast.IfExpr:
		return exprReassignsIdent(ex.Cond, name) ||
			identReassignedInBlock(ex.Then, name) ||
			identReassignedInBlock(ex.Else, name)
	case *ast.ErrorHandlerExpr:
		return exprReassignsIdent(ex.Expr, name) ||
			identReassignedInBlock(ex.Body, name) ||
			identReassignedInBlock(ex.ElseBody, name)
	case *ast.GoExpr:
		return exprReassignsIdent(ex.Expr, name) || identReassignedInBlock(ex.Block, name)
	case *ast.UnsafeExpr:
		return identReassignedInBlock(ex.Body, name)
	case *ast.LambdaExpr:
		return exprReassignsIdent(ex.ExprBody, name) || identReassignedInBlock(ex.Body, name)
	case *ast.ParenExpr:
		return exprReassignsIdent(ex.Expr, name)
	case *ast.BinaryExpr:
		return exprReassignsIdent(ex.Left, name) || exprReassignsIdent(ex.Right, name)
	case *ast.UnaryExpr:
		return exprReassignsIdent(ex.Operand, name)
	case *ast.CallExpr:
		if exprReassignsIdent(ex.Callee, name) {
			return true
		}
		for _, a := range ex.Args {
			if a != nil && exprReassignsIdent(a.Value, name) {
				return true
			}
		}
		return false
	case *ast.IndexExpr:
		if exprReassignsIdent(ex.Target, name) || exprReassignsIdent(ex.Index, name) {
			return true
		}
		for _, x := range ex.ExtraIndices {
			if exprReassignsIdent(x, name) {
				return true
			}
		}
		return false
	case *ast.SliceExpr:
		return exprReassignsIdent(ex.Target, name) ||
			exprReassignsIdent(ex.Low, name) || exprReassignsIdent(ex.High, name)
	case *ast.MemberExpr:
		return exprReassignsIdent(ex.Target, name)
	case *ast.OptionalChainExpr:
		return exprReassignsIdent(ex.Target, name)
	case *ast.IsExpr:
		return exprReassignsIdent(ex.Expr, name)
	case *ast.CastExpr:
		return exprReassignsIdent(ex.Expr, name)
	case *ast.ErrorPropagateExpr:
		return exprReassignsIdent(ex.Expr, name)
	case *ast.ErrorPanicExpr:
		return exprReassignsIdent(ex.Expr, name)
	case *ast.OptionalUnwrapExpr:
		return exprReassignsIdent(ex.Expr, name)
	case *ast.AutoCloneExpr:
		return exprReassignsIdent(ex.Expr, name)
	case *ast.TupleLit:
		for _, el := range ex.Elements {
			if exprReassignsIdent(el, name) {
				return true
			}
		}
		return false
	case *ast.ArrayLit:
		for _, el := range ex.Elements {
			if exprReassignsIdent(el, name) {
				return true
			}
		}
		return false
	}
	return false
}

// identNamed reports whether e is the identifier `name`.
func identNamed(e ast.Expr, name string) bool {
	id, ok := e.(*ast.IdentExpr)
	return ok && id.Name == name
}

// dropOldUserValueAtIdentSlot is a drop-flag-aware wrapper around
// dropOldUserValueAtPtr for the inc/dec and compound-assign IdentExpr paths
// (T1194). When the identifier has a drop flag — an owned local, or a
// borrow-by-default heap param that maybeRegisterBorrowParamReassignDrop gave a
// flag-0 binding — the heap drop-old is guarded by that flag so a borrowed
// original (flag 0) is never freed (fixing the compound/inc-dec double-free), and
// the flag is then armed to 1 so the fresh operator result is owned and dropped
// at scope exit (fixing the leak). Owned locals sit at flag 1, so the drop fires
// and the re-arm is a no-op — behaviour is unchanged for them. Without a flag
// (value types, scalars, enums with no droppable data, or a name that never got a
// binding) it falls back to the existing unconditional alias-guarded drop.
func (c *Compiler) dropOldUserValueAtIdentSlot(name string, ptr value.Value, valueType types.Type, newVal value.Value) {
	// T0959: a moved ref-typed local (`Counter &r = owner;`) stores the same
	// Value struct as an owned local and owns its instance, but valueType is the
	// reference wrapper — strip it so dropOldUserValueAtPtr recognizes the
	// droppable underlying type instead of no-oping (which would leak the old
	// instance). Safe: a genuine borrow sits at flag 0 and skips the drop-old.
	switch rt := valueType.(type) {
	case *types.MutRef:
		valueType = rt.Elem()
	case *types.SharedRef:
		valueType = rt.Elem()
	}
	flag, ok := c.dropFlags[name]
	if !ok {
		c.dropOldUserValueAtPtr(ptr, valueType, newVal)
		return
	}
	flagVal := c.block.NewLoad(irtypes.I1, flag)
	dropBlk := c.newBlock("incdec.flagdrop")
	afterBlk := c.newBlock("incdec.flagdrop.done")
	c.block.NewCondBr(flagVal, dropBlk, afterBlk)
	c.block = dropBlk
	c.dropOldUserValueAtPtr(ptr, valueType, newVal)
	if c.block.Term == nil {
		c.block.NewBr(afterBlk)
	}
	c.block = afterBlk
	// The fresh operator result is now owned by this slot.
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), flag)
}

// maybeRegisterDrop checks if a variable's type has a drop() method and, if so,
// registers a drop binding: allocates a drop flag (i1, initially true), resolves
// the drop function, and appends a scopeBinding.
// Strings are special: they use promise_string_drop (checks literal flag before freeing).
func (c *Compiler) maybeRegisterDrop(varName string, alloca *ir.InstAlloca, typ types.Type) {
	// T0381: A ref-typed local (`T&`/`T~`) starts life borrowing from the
	// owner — drop is cleared at the assignment site. But the same local
	// can later be reassigned to an owned `T` (decay rule), at which point
	// it owns the new value and must drop on scope exit. Register the
	// binding using the underlying owned type so the drop machinery emits
	// a proper drop (e.g., per-element string drops for `string[]`) when
	// the runtime dropflag is true.
	if sr, ok := typ.(*types.SharedRef); ok {
		typ = sr.Elem()
	}
	if mr, ok := typ.(*types.MutRef); ok {
		typ = mr.Elem()
	}
	// T0102: Enum drop — check before extractNamed since enums are *types.Enum, not *types.Named.
	if enum := extractEnum(typ); enum != nil {
		if enum.HasDrop() {
			c.maybeRegisterEnumDrop(varName, alloca, typ, enum)
			return
		}
		// B0238: Check for mono-time synthesized drops on generic enum instances
		// whose TypeParam variant fields resolve to droppable concrete types.
		if inst, ok := typ.(*types.Instance); ok && monoEnumInstNeedsSynthDrop(inst) {
			c.maybeRegisterEnumDrop(varName, alloca, typ, enum)
			return
		}
	}

	// T0585: Optional drop — delegate to maybeRegisterOptionalDrop so callers
	// passing Optional types (notably `~T?` consume params via defineFunc) get
	// a drop flag and binding consistent with other owned values. Without this,
	// `~T?` params had no flag, which both leaked when not consumed and broke
	// borrowed-vs-owned discrimination in the T0585 wrap propagation.
	if opt, ok := typ.(*types.Optional); ok {
		c.maybeRegisterOptionalDrop(varName, alloca, opt)
		return
	}

	// T0371: Tuple value with droppable fields — register a bindingDropTuple
	// that walks fields and drops each droppable one at scope exit.
	// Tuples are stored in struct allocas (not i8*), so we use a dedicated kind.
	if _, ok := typ.(*types.Tuple); ok {
		if !c.tupleNeedsDrop(typ) {
			return
		}
		dropFlag := c.createEntryAlloca(irtypes.I1)
		dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
		c.dropFlags[varName] = dropFlag
		binding := scopeBinding{
			kind:     bindingDropTuple,
			alloca:   alloca,
			valType:  typ,
			dropFlag: dropFlag,
			varName:  varName,
		}
		c.scopeBindings = append(c.scopeBindings, binding)
		c.dropBindings[varName] = binding
		return
	}

	// T0389: Fixed-size array with droppable element type — register a
	// bindingDropArray that walks elements and drops each droppable one at
	// scope exit. Arrays are stored in [N x T] allocas.
	if arr, ok := typ.(*types.Array); ok {
		elemType := arr.Elem()
		if c.typeSubst != nil {
			elemType = types.Substitute(elemType, c.typeSubst)
		}
		if !c.variantFieldNeedsDrop(elemType) {
			return
		}
		dropFlag := c.createEntryAlloca(irtypes.I1)
		dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
		c.dropFlags[varName] = dropFlag
		binding := scopeBinding{
			kind:     bindingDropArray,
			alloca:   alloca,
			valType:  typ,
			dropFlag: dropFlag,
			varName:  varName,
		}
		c.scopeBindings = append(c.scopeBindings, binding)
		c.dropBindings[varName] = binding
		return
	}

	named := extractNamed(typ)
	if named == nil {
		return
	}

	// String drop: register bindingDropString with promise_string_drop.
	// The drop flag is cleared at all move sites (return, assignment, constructor,
	// function call args) via clearDropFlag. Strings passed to functions have their
	// flag cleared (callee conceptually borrows/takes ownership), so they won't be
	// freed at scope exit. Strings that are NOT passed to functions are freed.
	if named == types.TypString {
		dropFlag := c.createEntryAlloca(irtypes.I1)
		dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
		c.dropFlags[varName] = dropFlag

		dropFunc := c.funcs["promise_string_drop"]
		binding := scopeBinding{
			kind:     bindingDropString,
			alloca:   alloca,
			named:    named,
			valType:  typ,
			dropFlag: dropFlag,
			dropFunc: dropFunc,
			varName:  varName,
		}
		c.scopeBindings = append(c.scopeBindings, binding)
		c.dropBindings[varName] = binding
		return
	}

	// Vector drop: register bindingDropString (same mechanism — i8* alloca + void(i8*) drop).
	// Vector.drop null-checks and frees the heap buffer. Drop flag semantics match strings:
	// cleared at all move sites, borrow detection skips drops for container element access.
	if elemType, ok := types.AsVector(typ); ok || named == types.TypVector {
		_ = elemType // B0245: elemType is available when typ is an Instance
		dropFlag := c.createEntryAlloca(irtypes.I1)
		dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
		c.dropFlags[varName] = dropFlag

		dropFunc := c.funcs["Vector.drop"]
		binding := scopeBinding{
			kind:     bindingDropString, // reuse: same i8* alloca + void(i8*) drop pattern
			alloca:   alloca,
			named:    named,
			valType:  typ,
			dropFlag: dropFlag,
			dropFunc: dropFunc,
			varName:  varName,
		}
		c.scopeBindings = append(c.scopeBindings, binding)
		c.dropBindings[varName] = binding
		return
	}

	// Channel drop (B0163): same i8* alloca + void(i8*) drop pattern as string/vector.
	// T0663: Channel[T].drop is per-element-type — it drops any un-received
	// buffered items before freeing the ring buffer, mutex, cond vars, and the
	// struct itself. Drop flag semantics handle moves: cleared when the channel
	// is passed to go blocks or functions, so only the last owner frees.
	if elemType, ok := types.AsChannel(typ); ok || named == types.TypChannel {
		resolvedElem := elemType
		if resolvedElem == nil && named == types.TypChannel && c.typeSubst != nil {
			if tp := types.TypChannel.TypeParams(); len(tp) > 0 {
				resolvedElem = c.typeSubst[tp[0]]
			}
		}
		if c.typeSubst != nil && resolvedElem != nil {
			resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
		}
		if resolvedElem != nil {
			dropFlag := c.createEntryAlloca(irtypes.I1)
			dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
			c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
			c.dropFlags[varName] = dropFlag

			dropFunc := c.getOrCreateChannelDrop(resolvedElem)
			binding := scopeBinding{
				kind:     bindingDropString, // reuse: same i8* alloca + void(i8*) drop pattern
				alloca:   alloca,
				named:    named,
				valType:  typ,
				dropFlag: dropFlag,
				dropFunc: dropFunc,
				varName:  varName,
			}
			c.scopeBindings = append(c.scopeBindings, binding)
			c.dropBindings[varName] = binding
			return
		}
	}

	// Arc drop (T0155): same i8* alloca + void(i8*) drop pattern as string/vector/channel.
	// Arc.drop atomically decrements the refcount and frees when it reaches zero.
	if elemType, ok := types.AsArc(typ); ok || named == types.TypArc {
		resolvedElem := elemType
		if resolvedElem == nil && named == types.TypArc && c.typeSubst != nil {
			if tp := types.TypArc.TypeParams(); len(tp) > 0 {
				resolvedElem = c.typeSubst[tp[0]]
			}
		}
		if c.typeSubst != nil && resolvedElem != nil {
			resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
		}
		if resolvedElem != nil {
			dropFlag := c.createEntryAlloca(irtypes.I1)
			dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
			c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
			c.dropFlags[varName] = dropFlag

			dropFunc := c.getOrCreateArcDrop(resolvedElem)
			binding := scopeBinding{
				kind:     bindingDropString, // reuse: same i8* alloca + void(i8*) drop pattern
				alloca:   alloca,
				named:    named,
				valType:  typ,
				dropFlag: dropFlag,
				dropFunc: dropFunc,
				varName:  varName,
			}
			c.scopeBindings = append(c.scopeBindings, binding)
			c.dropBindings[varName] = binding
			return
		}
	}

	// Weak drop (T0157): per-instantiation drop (decrements weak_count, frees when zero).
	if elemType, ok := types.AsWeak(typ); ok || named == types.TypWeak {
		resolvedElem := elemType
		if resolvedElem == nil && named == types.TypWeak && c.typeSubst != nil {
			if tp := types.TypWeak.TypeParams(); len(tp) > 0 {
				resolvedElem = c.typeSubst[tp[0]]
			}
		}
		if c.typeSubst != nil && resolvedElem != nil {
			resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
		}
		if resolvedElem != nil {
			dropFlag := c.createEntryAlloca(irtypes.I1)
			dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
			c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
			c.dropFlags[varName] = dropFlag

			dropFunc := c.getOrCreateWeakDrop(resolvedElem)
			binding := scopeBinding{
				kind:     bindingDropString, // reuse: same i8* alloca + void(i8*) drop pattern
				alloca:   alloca,
				named:    named,
				valType:  typ,
				dropFlag: dropFlag,
				dropFunc: dropFunc,
				varName:  varName,
			}
			c.scopeBindings = append(c.scopeBindings, binding)
			c.dropBindings[varName] = binding
			return
		}
	}

	// Mutex drop (T0156): per-instantiation drop (drops inner T, destroys PAL mutex, frees).
	if elemType, ok := types.AsMutex(typ); ok || named == types.TypMutex {
		resolvedElem := elemType
		if resolvedElem == nil && named == types.TypMutex && c.typeSubst != nil {
			if tp := types.TypMutex.TypeParams(); len(tp) > 0 {
				resolvedElem = c.typeSubst[tp[0]]
			}
		}
		if c.typeSubst != nil && resolvedElem != nil {
			resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
		}
		if resolvedElem != nil {
			dropFlag := c.createEntryAlloca(irtypes.I1)
			dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
			c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
			c.dropFlags[varName] = dropFlag

			dropFunc := c.getOrCreateMutexDrop(resolvedElem)
			binding := scopeBinding{
				kind:     bindingDropString,
				alloca:   alloca,
				named:    named,
				valType:  typ,
				dropFlag: dropFlag,
				dropFunc: dropFunc,
				varName:  varName,
			}
			c.scopeBindings = append(c.scopeBindings, binding)
			c.dropBindings[varName] = binding
			return
		}
	}

	// MutexGuard drop (T0156): T-independent drop (unlock + free guard).
	if _, ok := types.AsMutexGuard(typ); ok || named == types.TypMutexGuard {
		dropFlag := c.createEntryAlloca(irtypes.I1)
		dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
		c.dropFlags[varName] = dropFlag

		dropFunc := c.funcs["MutexGuard.drop"]
		binding := scopeBinding{
			kind:     bindingDropString,
			alloca:   alloca,
			named:    named,
			valType:  typ,
			dropFlag: dropFlag,
			dropFunc: dropFunc,
			varName:  varName,
		}
		c.scopeBindings = append(c.scopeBindings, binding)
		c.dropBindings[varName] = binding
		return
	}

	// Task drop (T0503): per-instantiation drop blocks until the goroutine
	// finishes, drops the result T (if any), then frees result_ptr/panic_msg/G.
	// Without this, `task[T] t = go fn();` leaks the G struct, the result_ptr
	// buffer, and any droppable result value when t is never awaited via <-t.
	if elemType, ok := types.AsAnyTask(typ); ok || types.IsTaskLikeOrigin(named) {
		// T1379: FailableTask[T] drop discharges the buffered {ok,value,err}
		// aggregate before freeing; a plain Task[T] drop frees bare T.
		failable := types.IsFailableTask(typ) || named == types.TypFailableTask
		taskOrigin := types.TypTask
		if failable {
			taskOrigin = types.TypFailableTask
		}
		resolvedElem := elemType
		if resolvedElem == nil && named != nil && c.typeSubst != nil {
			if tp := taskOrigin.TypeParams(); len(tp) > 0 {
				resolvedElem = c.typeSubst[tp[0]]
			}
		}
		if c.typeSubst != nil && resolvedElem != nil {
			resolvedElem = types.Substitute(resolvedElem, c.typeSubst)
		}
		// resolvedElem may be nil (task[void]) — getOrCreateTaskDrop handles that.
		dropFlag := c.createEntryAlloca(irtypes.I1)
		dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
		c.dropFlags[varName] = dropFlag

		dropFunc := c.getOrCreateTaskDrop(resolvedElem, failable)
		binding := scopeBinding{
			kind:     bindingDropString, // reuse: same i8* alloca + void(i8*) drop pattern
			alloca:   alloca,
			named:    named,
			valType:  typ,
			dropFlag: dropFlag,
			dropFunc: dropFunc,
			varName:  varName,
		}
		c.scopeBindings = append(c.scopeBindings, binding)
		c.dropBindings[varName] = binding
		return
	}

	// Remaining container types without drop support skip.
	if isContainerType(typ) {
		return
	}

	if !named.HasDrop() {
		// B0202: Check if this is a mono instance with a synthesized drop
		// detected at codegen time (TypeParam fields → droppable concrete types).
		// Use monoInstNeedsSynthDrop to precisely match only B0202 instances,
		// not instances that already have drops via other paths.
		if inst, ok := typ.(*types.Instance); ok && monoInstNeedsSynthDrop(inst) {
			monoDropName := mangleMethodName(monoName(inst), "drop", false)
			if dropFn, exists := c.funcs[monoDropName]; exists {
				dropFlag := c.createEntryAlloca(irtypes.I1)
				dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
				c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
				c.dropFlags[varName] = dropFlag
				binding := scopeBinding{
					kind:          bindingDrop,
					alloca:        alloca,
					named:         named,
					valType:       typ,
					dropFlag:      dropFlag,
					dropFunc:      dropFn,
					varName:       varName,
					monoSynthDrop: true,
				}
				c.scopeBindings = append(c.scopeBindings, binding)
				c.dropBindings[varName] = binding
				return
			}
		}

		// B0164: Heap user types without drop methods still need pal_free at scope exit.
		// Types that are value types, copy types, or primitive scalars don't heap-allocate.
		// Only register for allocas that store value structs ({i8*, i8*}), not raw i8*
		// pointers (method receivers, captures, etc.) which would crash extractInstancePtr.
		// Only for types with value struct allocas (not raw i8* method receivers/captures),
		// excluding structural interfaces (their instance ptr may be a stack alloca, not heap).
		_, isStructAlloca := alloca.ElemType.(*irtypes.StructType)
		if isStructAlloca && !named.IsValueType() && !named.IsCopy() && !isPrimitiveScalar(named) && !named.IsStructural() {
			dropFlag := c.createEntryAlloca(irtypes.I1)
			dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
			c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
			c.dropFlags[varName] = dropFlag

			binding := scopeBinding{
				kind:     bindingFree,
				alloca:   alloca,
				named:    named,
				valType:  typ,
				dropFlag: dropFlag,
				varName:  varName,
				// T0917: Polymorphic heap types (abstract base / has children) may hold
				// a concrete subtype clone (from clone-on-`return this`, T0387/T0893) with
				// its own droppable fields. Dispatch the free through RTTI typeinfo
				// drop_fn_ptr so the concrete drop frees those fields, not a bare pal_free
				// that would leak them. Non-polymorphic leaves keep pal_free.
				rttiDrop: c.needsVtable(named),
			}
			c.scopeBindings = append(c.scopeBindings, binding)
			c.dropBindings[varName] = binding
		}
		return
	}

	// Allocate drop flag: i1, initialized to true (should drop).
	// Use entry-block alloca to avoid stack growth in loops.
	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag

	binding := scopeBinding{
		kind:     bindingDrop,
		alloca:   alloca,
		named:    named,
		valType:  typ,
		dropFlag: dropFlag,
		varName:  varName,
	}

	// Resolve drop function for direct dispatch.
	// Synthesized drops (B0158) always use direct dispatch — they're not in the vtable.
	dropMethod := named.LookupMethod("drop")
	if named.NeedsSynthDrop() || !c.needsVtable(named) || (dropMethod != nil && dropMethod.IsNative()) {
		// For mono instances (e.g., Wrapper[int]), use the mono-qualified name
		// (Wrapper[int].drop), not the origin name (Wrapper.drop).
		// In mono method bodies, type args may contain TypeParams — substitute
		// with c.typeSubst to get the concrete instance name.
		resolvedTyp := typ
		if c.typeSubst != nil {
			resolvedTyp = types.Substitute(typ, c.typeSubst)
		}
		ownerName := named.Obj().Name()
		if inst, ok := resolvedTyp.(*types.Instance); ok {
			ownerName = monoName(inst)
		} else if !named.NeedsSynthDrop() {
			ownerName = c.resolveDropOwner(named)
		}
		mangledName := mangleMethodName(ownerName, "drop", false)
		if fn, ok := c.funcs[mangledName]; ok {
			binding.dropFunc = fn
		}
	}

	c.scopeBindings = append(c.scopeBindings, binding)
	c.dropBindings[varName] = binding
}

// isFreshOwnedStructuralRHS reports whether rhs produces a freshly-allocated
// owned value with no surviving owner (a call/operator/getter/user-index
// result), as opposed to a borrow/alias (IdentExpr, field-access MemberExpr,
// native container index). A structural result type from a call/operator can
// only come from a user method, which returns an owned value; a getter *call*
// and a user-defined non-native `[]` operator likewise return fresh owned
// values, while a plain field access and native indexing are borrows. Used to
// decide whether an unwrapped structural-interface binding must free its box at
// scope exit. T1288.
// B0272: unwraps error-handling wrappers (!, ^, ? {}, force-unwrap) first so a
// failable structural return still counts as a fresh owned allocation.
// T1289: also recognizes IndexExpr (user-defined `[]`) and getter-call
// MemberExpr as fresh-owned — both were previously missed and leaked their box;
// consulting sema (isUserIndexExpr / isGetterCallExpr) distinguishes them from
// borrows, and !isBorrowedExpr excludes borrow-returning (`T&`) operators/getters.
func (c *Compiler) isFreshOwnedStructuralRHS(rhs ast.Expr) bool {
	innerRHS := rhs
	for {
		switch e := innerRHS.(type) {
		case *ast.ErrorPanicExpr:
			innerRHS = e.Expr
			continue
		case *ast.OptionalUnwrapExpr:
			innerRHS = e.Expr
			continue
		case *ast.ErrorPropagateExpr:
			innerRHS = e.Expr
			continue
		case *ast.ErrorHandlerExpr:
			innerRHS = e.Expr
			continue
		}
		break
	}
	switch e := innerRHS.(type) {
	case *ast.CallExpr, *ast.UnaryExpr, *ast.BinaryExpr:
		// T1061: a borrow-returning call/operator (`T&`) aliases existing
		// storage rather than producing a fresh owned allocation — mirror the
		// IndexExpr/MemberExpr borrow guards below so maybeRegisterStructuralFree
		// (and the if-let/while-let structural-param free) does not register a
		// free over borrowed storage.
		return !c.isBorrowedExpr(innerRHS)
	case *ast.IdentExpr:
		// T1273: a bare-identifier module getter (e.g. `stdout`) returns a fresh
		// owned value, just like a call — free its box at scope exit. A plain local
		// identifier is a borrow of an existing structural variable and must NOT be
		// freed here.
		if _, isLocal := c.locals[e.Name]; isLocal {
			return false
		}
		obj := c.lookupFunc(e.Name)
		return obj != nil && obj.IsGetter()
	case *ast.IndexExpr:
		// T1289: a user-defined non-native `[]` returns a fresh owned value; a
		// borrow-returning operator (`T&`) aliases container storage — skip it.
		return c.isUserIndexExpr(innerRHS) && !c.isBorrowedExpr(innerRHS)
	case *ast.MemberExpr:
		// T1289: a getter *call* is fresh-owned (free it); a field *borrow* is not
		// (freeing double-frees). A borrow-returning getter (`T&`) is also an alias.
		// This also covers module-level getters (`mod.getter`), which construct a
		// fresh value (T1273).
		return c.isGetterCallExpr(innerRHS) && !c.isBorrowedExpr(innerRHS)
	default:
		return false
	}
}

// maybeRegisterStructuralFree registers a bindingFree for structural interface variables
// whose backing instance is heap-allocated from a call/constructor (T0127).
// Structural types are excluded from maybeRegisterDrop (their instance ptr could be a
// borrow from a concrete variable). This method is called only when the RHS is NOT a
// simple identifier, meaning the value comes from a fresh allocation (e.g., vec.iter(),
// iter.map(f)) and the variable owns the backing instance.
// claimedOwnedBox (T1276): true when claimHeapTemp just claimed a fresh owned heap
// box for this binding (e.g. a value/primitive type coerced to a structural interface
// via `Sink s = counterValue;`). Such a box has no CallExpr RHS to key off, but the
// variable genuinely owns it and must free it at scope exit — so we register the free
// binding regardless of RHS shape. A borrow of an already-structural value claims
// nothing, so this stays false and the RHS-shape gate below preserves borrow semantics.
func (c *Compiler) maybeRegisterStructuralFree(varName string, alloca *ir.InstAlloca, typ types.Type, rhs ast.Expr, claimedOwnedBox bool) {
	// Only for structural interface types without an existing drop binding.
	if _, hasBinding := c.dropBindings[varName]; hasBinding {
		return
	}
	named := extractNamed(typ)
	if named == nil || !named.IsStructural() || named.IsValueType() {
		return
	}
	// T1061: a borrow-returning RHS (`T&`/`T~`, e.g. a structural operator/method
	// whose body is `return this` with a `&`-return type) aliases existing storage
	// rather than owning a fresh allocation. Never register a free over it — even
	// when claimHeapTemp opportunistically captured a drop func for the binding
	// (claimedOwnedBox), since the return value's instance ptr matches the still-
	// owned receiver/temp, whose own drop performs the single cleanup. Registering
	// here would risk a premature/double free (this over-registration is masked in
	// the var-decl path by the downstream borrow-clear, but the guard closes it at
	// the source). extractNamed peels SharedRef/MutRef, so the checks above do not
	// catch this on their own.
	if c.isBorrowedExpr(rhs) {
		return
	}
	// Only register when the RHS produces a fresh heap allocation the variable
	// owns. Call expressions (e.g., vec.iter(), iter.map(f)) qualify; so do
	// overloaded operator expressions (e.g., `-it`, `a + b`) — a structural
	// result type can only come from a user operator method, which returns an
	// owned value (T0893: clone-on-`return this` makes operator results owned
	// allocations that must be freed here). Other RHS expressions — identifiers
	// (borrow from existing variable), literals (value types, no heap alloc),
	// member access (borrow) — should NOT get a free binding.
	if !c.isFreshOwnedStructuralRHS(rhs) {
		// T1276: a claimed fresh box (value/primitive → structural coercion of a
		// non-call RHS, e.g. an identifier copy) is owned even though its RHS shape
		// isn't a call — register the free. Otherwise it's a borrow: skip.
		if !claimedOwnedBox {
			return
		}
	}
	// Must be a struct alloca ({i8* vtable, i8* instance}) to extract instance ptr.
	if _, ok := alloca.ElemType.(*irtypes.StructType); !ok {
		return
	}

	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag

	// B0272: Use RTTI-based drop dispatch when there's no specific cleanup function,
	// or when the claimed dropFunc is just pal_free (generic heap free that doesn't
	// drop instance fields like strings). Iterator cleanup functions (e.g.,
	// __promise_iter_cleanup) ARE proper cleanup — they handle _FnIter instances
	// that don't have RTTI layout. pal_free-claimed instances DO have RTTI layout
	// (they're standard user type instances from constructors).
	claimedDrop := c.lastClaimedDropFunc
	useRTTI := claimedDrop == nil || claimedDrop == c.palFree
	if useRTTI {
		claimedDrop = nil // don't use pal_free directly — RTTI dispatch handles it
	}
	binding := scopeBinding{
		kind:     bindingFree,
		alloca:   alloca,
		named:    named,
		valType:  typ,
		dropFlag: dropFlag,
		dropFunc: claimedDrop, // T0127: use iter cleanup when available
		varName:  varName,
		rttiDrop: useRTTI, // B0272: RTTI-based drop for instances with standard layout
	}
	c.scopeBindings = append(c.scopeBindings, binding)
	c.dropBindings[varName] = binding
}

// maybeRegisterStructuralParamFree registers an RTTI-dispatched free binding for an
// owned (~) structural-interface view parameter (T0861). Unlike local structural
// variables (maybeRegisterStructuralFree), an owned param is always a genuine ownership
// transfer — the caller cleared its source drop flag at the move site — so the callee
// must drop the backing concrete instance at scope exit. Uses RTTI drop dispatch
// (typeinfo.drop_fn_ptr via __promise_structural_drop) so concrete field cleanup
// (e.g. string fields) runs before the instance is freed.
func (c *Compiler) maybeRegisterStructuralParamFree(varName string, alloca *ir.InstAlloca, typ types.Type) {
	if _, hasBinding := c.dropBindings[varName]; hasBinding {
		return
	}
	named := extractNamed(typ)
	if named == nil || !named.IsStructural() || named.IsValueType() {
		return
	}
	// Value-struct alloca ({i8* vtable, i8* instance}) required to extract the instance ptr.
	if _, ok := alloca.ElemType.(*irtypes.StructType); !ok {
		return
	}

	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag

	binding := scopeBinding{
		kind:     bindingFree,
		alloca:   alloca,
		named:    named,
		valType:  typ,
		dropFlag: dropFlag,
		varName:  varName,
		rttiDrop: true, // RTTI-dispatched drop for standard-layout instances
	}
	c.scopeBindings = append(c.scopeBindings, binding)
	c.dropBindings[varName] = binding
}

// registerErrorDrop registers a caught error instance for drop at scope exit (T0091).
// Uses the concrete error type's drop when available (T0110), falling back to the
// base error.drop for untyped catches. The concrete drop properly frees all string
// fields (message + child-specific fields like key). The drop flag enables proper
// handling of re-raise (genRaiseStmt clears it, T0086).
// concreteType is the resolved type — may be *types.Named or *types.Instance for generics.
//
// B0226: For untyped catches (concreteType == types.TypError), uses RTTI-based dispatch:
// loads the drop function pointer from the typeinfo (field 1) of the actual error
// instance at runtime, enabling correct drop for generic error subtypes like
// GenericError[Point] even when caught as bare `error`.
func (c *Compiler) registerErrorDrop(varName string, alloca *ir.InstAlloca, concreteType types.Type) {
	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag

	concreteNamed := extractNamed(concreteType)
	if concreteNamed == nil {
		concreteNamed = types.TypError
	}

	// B0226: For untyped catches where the concrete type is the base error type,
	// use RTTI-based dispatch to call the actual error subtype's drop at runtime.
	// This handles cases like GenericError[Point] caught via untyped `? e { ... }`.
	if concreteNamed == types.TypError {
		binding := scopeBinding{
			kind:     bindingDrop,
			alloca:   alloca,
			named:    concreteNamed,
			valType:  concreteNamed,
			dropFlag: dropFlag,
			rttiDrop: true,
			varName:  varName,
		}
		c.scopeBindings = append(c.scopeBindings, binding)
		c.dropBindings[varName] = binding
		return
	}

	// Resolve the drop function for the concrete error type (T0110).
	// For typed catches, this is the child type's drop (e.g., NotFoundError.drop).
	// For generic instances (e.g., AppError[int]), use the monomorphized name.
	var ownerName string
	if inst, ok := concreteType.(*types.Instance); ok {
		ownerName = monoName(inst)
	} else {
		ownerName = concreteNamed.Obj().Name()
	}
	dropName := mangleMethodName(ownerName, "drop", false)
	dropFunc := c.funcs[dropName]
	if dropFunc == nil {
		// Fallback: resolve via method owner chain (with child-first preference)
		fallbackOwner := c.resolveDropOwner(concreteNamed)
		dropFunc = c.funcs[mangleMethodName(fallbackOwner, "drop", false)]
	}
	if dropFunc == nil {
		// Last resort: use base error.drop (e.g., bare generic types like AppError
		// without type args where the monomorphized drop isn't available).
		dropFunc = c.funcs[mangleMethodName("__mod_std_error", "drop", false)]
	}

	binding := scopeBinding{
		kind:     bindingDrop,
		alloca:   alloca,
		named:    concreteNamed,
		valType:  concreteNamed,
		dropFlag: dropFlag,
		dropFunc: dropFunc,
		varName:  varName,
	}
	c.scopeBindings = append(c.scopeBindings, binding)
	c.dropBindings[varName] = binding
}

// maybeRegisterEnumDrop registers a drop binding for an enum variable whose variants
// contain heap-allocated data (T0102). The drop function takes i8* (pointer to the
// alloca storing the enum internal type) and switches on the tag to drop variant fields.
func (c *Compiler) maybeRegisterEnumDrop(varName string, alloca *ir.InstAlloca, typ types.Type, enum *types.Enum) {
	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag

	// Resolve the enum drop function name.
	enumName := enum.Obj().Name()
	if inst, ok := typ.(*types.Instance); ok {
		// B0238: typ is already a concrete Instance (e.g., Container[Wrapper]) — use mono name directly.
		enumName = monoName(inst)
	} else if c.typeSubst != nil {
		// Inside a generic body — substitute TypeParams to get the concrete Instance.
		resolvedTyp := types.Substitute(typ, c.typeSubst)
		if inst, ok := resolvedTyp.(*types.Instance); ok {
			enumName = monoName(inst)
		}
	}
	mangledName := mangleMethodName(enumName, "drop", false)
	var dropFunc *ir.Func
	if fn, ok := c.funcs[mangledName]; ok {
		dropFunc = fn
	}

	binding := scopeBinding{
		kind:     bindingDropEnum,
		alloca:   alloca,
		valType:  typ,
		dropFlag: dropFlag,
		dropFunc: dropFunc,
		varName:  varName,
	}
	c.scopeBindings = append(c.scopeBindings, binding)
	c.dropBindings[varName] = binding
}

// registerValTypeOptionalDrop registers a bindingDropOptional that carries no
// dropFunc — emitOptionalLocalValueDrop dispatches on valType (immediateElem)
// instead. Shared by the Tuple (T0397) and closure/Signature (T0814) inner-type
// cases, whose drop is driven entirely by emitVariantFieldDrop on the inner value.
func (c *Compiler) registerValTypeOptionalDrop(varName string, alloca *ir.InstAlloca, immediateElem types.Type) {
	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag
	binding := scopeBinding{
		kind:     bindingDropOptional,
		alloca:   alloca,
		valType:  immediateElem,
		dropFlag: dropFlag,
		varName:  varName,
	}
	c.scopeBindings = append(c.scopeBindings, binding)
	c.dropBindings[varName] = binding
}

// maybeRegisterOptionalDrop registers a bindingDropOptional for an explicitly declared
// optional local variable (T0111). Only called for typed declarations (string? s = ...)
// where the inner type is droppable (string, vector, channel, user type with drop).
// Inferred optional variables (s := func_returning_optional()) are NOT registered —
// they are consumed via if-let/while-let/force-unwrap patterns.
//
// T0391: For nested Optionals (T??, T???, ...) walks through Optional layers to
// reach the bottom inner type for dispatch info, but stores the immediate inner
// type in valType so emitOptionalLocalValueDrop can recurse through layers correctly.
func (c *Compiler) maybeRegisterOptionalDrop(varName string, alloca *ir.InstAlloca, opt *types.Optional) {
	// Don't double-register if maybeRegisterDrop already handled this variable.
	if _, exists := c.dropBindings[varName]; exists {
		return
	}

	immediateElem := opt.Elem()
	if c.typeSubst != nil {
		immediateElem = types.Substitute(immediateElem, c.typeSubst)
	}
	// T0391: Walk past nested Optionals to find the bottom inner type for dispatch.
	// For T??, immediateElem is T? and elem becomes T. The helper recurses through
	// layers at IR generation time using valType = immediateElem.
	elem := immediateElem
	for {
		innerOpt, ok := elem.(*types.Optional)
		if !ok {
			break
		}
		next := innerOpt.Elem()
		if c.typeSubst != nil {
			next = types.Substitute(next, c.typeSubst)
		}
		elem = next
	}
	innerNamed := extractNamed(elem)

	// Determine the drop function for the inner type.
	var dropFunc *ir.Func

	switch {
	case innerNamed == types.TypString:
		dropFunc = c.funcs["promise_string_drop"]
	case innerNamed != nil && (func() bool { _, ok := types.AsVector(elem); return ok }() || innerNamed == types.TypVector):
		dropFunc = c.funcs["Vector.drop"]
	case innerNamed != nil && (func() bool { _, ok := types.AsChannel(elem); return ok }() || innerNamed == types.TypChannel):
		// T0663: Channel inner drop — resolve element type and get per-element-type drop
		if chanElem, ok := types.AsChannel(elem); ok {
			resolvedChanElem := chanElem
			if c.typeSubst != nil {
				resolvedChanElem = types.Substitute(chanElem, c.typeSubst)
			}
			dropFunc = c.getOrCreateChannelDrop(resolvedChanElem)
		}
	case innerNamed != nil && (func() bool { _, ok := types.AsArc(elem); return ok }() || innerNamed == types.TypArc):
		// T0155: Arc inner drop — resolve element type and get per-instantiation drop
		if arcElem, ok := types.AsArc(elem); ok {
			resolvedArcElem := arcElem
			if c.typeSubst != nil {
				resolvedArcElem = types.Substitute(arcElem, c.typeSubst)
			}
			dropFunc = c.getOrCreateArcDrop(resolvedArcElem)
		}
	case innerNamed != nil && (func() bool { _, ok := types.AsWeak(elem); return ok }() || innerNamed == types.TypWeak):
		// T0157: Weak inner drop — resolve element type and get per-instantiation drop
		if weakElem, ok := types.AsWeak(elem); ok {
			resolvedWeakElem := weakElem
			if c.typeSubst != nil {
				resolvedWeakElem = types.Substitute(weakElem, c.typeSubst)
			}
			dropFunc = c.getOrCreateWeakDrop(resolvedWeakElem)
		}
	case innerNamed != nil && (func() bool { _, ok := types.AsMutex(elem); return ok }() || innerNamed == types.TypMutex):
		// T0156: Mutex inner drop — per-instantiation
		if mutexElem, ok := types.AsMutex(elem); ok {
			resolvedElem := mutexElem
			if c.typeSubst != nil {
				resolvedElem = types.Substitute(mutexElem, c.typeSubst)
			}
			dropFunc = c.getOrCreateMutexDrop(resolvedElem)
		}
	case innerNamed != nil && (func() bool { _, ok := types.AsMutexGuard(elem); return ok }() || innerNamed == types.TypMutexGuard):
		// T0156: MutexGuard inner drop — T-independent
		dropFunc = c.funcs["MutexGuard.drop"]
	case innerNamed != nil && (types.IsAnyTask(elem) || types.IsTaskLikeOrigin(innerNamed)):
		// T0558: Task inner drop — per-instantiation drop blocks on goroutine
		// completion, drops the result, frees result_ptr/panic_msg/G. Without
		// this case, dispatch fell through to the heap-user-type catch-all and
		// called pal_free on the raw G handle, causing segfaults at scope exit.
		failable := types.IsFailableTask(elem) || innerNamed == types.TypFailableTask
		taskOrigin := types.TypTask
		if failable {
			taskOrigin = types.TypFailableTask
		}
		var resolvedTaskElem types.Type
		if taskElem, ok, _ := types.AsAnyTaskFailable(elem); ok {
			resolvedTaskElem = taskElem
			if c.typeSubst != nil {
				resolvedTaskElem = types.Substitute(taskElem, c.typeSubst)
			}
		} else if innerNamed != nil && c.typeSubst != nil {
			if tp := taskOrigin.TypeParams(); len(tp) > 0 {
				resolvedTaskElem = c.typeSubst[tp[0]]
				if resolvedTaskElem != nil {
					resolvedTaskElem = types.Substitute(resolvedTaskElem, c.typeSubst)
				}
			}
		}
		dropFunc = c.getOrCreateTaskDrop(resolvedTaskElem, failable)
	case innerNamed != nil && (innerNamed.HasDrop() || innerNamed.NeedsSynthDrop()):
		// User type with explicit or synthesized drop
		explicitDrop := innerNamed.HasDrop() && !innerNamed.NeedsSynthDrop()
		ownerName := innerNamed.Obj().Name()
		resolvedElem := elem
		if c.typeSubst != nil {
			resolvedElem = types.Substitute(elem, c.typeSubst)
		}
		if inst, ok := resolvedElem.(*types.Instance); ok {
			ownerName = monoName(inst)
		} else if explicitDrop {
			ownerName = c.resolveDropOwner(innerNamed)
		}
		mangledName := mangleMethodName(ownerName, "drop", false)
		if fn, ok := c.funcs[mangledName]; ok {
			if explicitDrop && !dropIsNative(innerNamed) {
				// T0419: Explicit user drops don't include pal_free — wrap with $wrap
				// so the Optional drop path frees the instance after calling drop.
				// Synthesized drops already include pal_free. T1344: native drops
				// self-free too, so they must not be wrapped.
				fn = c.getOrCreateDropWrap(mangledName, fn)
			}
			dropFunc = fn
		}
	case innerNamed != nil && !innerNamed.IsValueType() && !innerNamed.IsCopy() && !isPrimitiveScalar(innerNamed) && !innerNamed.IsStructural():
		// B0211: Heap user type without drop — use pal_free to free the instance.
		dropFunc = c.palFree
	case innerNamed == nil && func() bool {
		// Droppable enum inner type: look up the enum drop function.
		enum := extractEnum(elem)
		if enum == nil {
			return false
		}
		enumName := enum.Obj().Name()
		if inst, ok := elem.(*types.Instance); ok {
			enumName = monoName(inst)
		}
		mangledName := mangleMethodName(enumName, "drop", false)
		if fn, ok := c.funcs[mangledName]; ok {
			dropFunc = fn
			return true
		}
		return false
	}():
		// dropFunc already set by the closure above
	case innerNamed != nil && innerNamed.IsStructural() && !innerNamed.IsValueType():
		// B0229/B0243: Structural interface (e.g., Iterator[T]) — use RTTI-based drop
		// dispatch. The concrete type is unknown at compile time (could be _FnIter,
		// Counter, or any user type implementing the interface), so we dispatch through
		// the typeinfo drop_fn_ptr at runtime.
		dropFlag := c.createEntryAlloca(irtypes.I1)
		dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
		c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
		c.dropFlags[varName] = dropFlag
		binding := scopeBinding{
			kind:     bindingDropOptional,
			alloca:   alloca,
			named:    innerNamed,
			valType:  immediateElem,
			dropFlag: dropFlag,
			rttiDrop: true,
			varName:  varName,
		}
		c.scopeBindings = append(c.scopeBindings, binding)
		c.dropBindings[varName] = binding
		return
	case func() bool {
		_, isTup := elem.(*types.Tuple)
		return isTup && c.tupleNeedsDrop(elem)
	}():
		// T0397: Tuple inner type — register binding without dropFunc. The
		// emitOptionalLocalValueDrop Tuple branch dispatches via emitVariantFieldDrop
		// on the inner tuple value (walks fields, drops droppable elements).
		c.registerValTypeOptionalDrop(varName, alloca, immediateElem)
		return
	case func() bool { _, isSig := elem.(*types.Signature); return isSig }():
		// T0814: Optional[closure] local — the inner fat pointer {fn,env} owns a heap
		// env. Register an optional drop; emitOptionalLocalValueDrop frees the env
		// (deep-drops captures) when present. Same machinery as string?/vector? so
		// move-tracking (g := o) and reassignment (o = ...) clear/re-arm the flag.
		c.registerValTypeOptionalDrop(varName, alloca, immediateElem)
		return
	default:
		return // inner type not droppable
	}

	if dropFunc == nil {
		return
	}

	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag

	binding := scopeBinding{
		kind:     bindingDropOptional,
		alloca:   alloca,
		named:    innerNamed,
		valType:  immediateElem,
		dropFlag: dropFlag,
		dropFunc: dropFunc,
		varName:  varName,
	}
	c.scopeBindings = append(c.scopeBindings, binding)
	c.dropBindings[varName] = binding
}

// maybeRegisterCapturedOptionalStructuralDrop registers a reassignment-only drop binding
// for captured optional structural interface variables (B0229). Unlike maybeRegisterOptionalDrop,
// this does NOT add to scopeBindings — the env drop function handles final cleanup at env
// deallocation, and scope-exit drop would free a value that's been written back to the env.
func (c *Compiler) maybeRegisterCapturedOptionalStructuralDrop(varName string, alloca *ir.InstAlloca, typ types.Type) {
	if _, exists := c.dropBindings[varName]; exists {
		return
	}
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	opt, ok := typ.(*types.Optional)
	if !ok {
		return
	}
	elem := opt.Elem()
	if c.typeSubst != nil {
		elem = types.Substitute(elem, c.typeSubst)
	}
	innerNamed := extractNamed(elem)
	if innerNamed == nil || !innerNamed.IsStructural() || innerNamed.IsValueType() {
		return
	}

	// B0243: Use RTTI-based drop dispatch — the concrete type behind the structural
	// interface is unknown at compile time.
	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag

	binding := scopeBinding{
		kind:     bindingDropOptional,
		alloca:   alloca,
		named:    innerNamed,
		valType:  elem,
		dropFlag: dropFlag,
		rttiDrop: true,
		varName:  varName,
	}
	// Only add to dropBindings (for reassignment drop), NOT scopeBindings (no scope-exit drop).
	c.dropBindings[varName] = binding
}

// clearDropFlag sets a variable's drop flag to false (indicating the value has been moved).
func (c *Compiler) clearDropFlag(name string) {
	if flag, ok := c.dropFlags[name]; ok {
		c.block.NewStore(constant.NewInt(irtypes.I1, 0), flag)
	}
}

// hasDropFlag reports whether `name` has an active drop flag — i.e. it is an owned
// droppable binding (owned var / move-param), not a borrow (borrow params have no
// drop flag). Used by T1282 to decide whether a string→structural box at a move
// position takes ownership of the source pointer (owned) or clones it (borrowed).
func (c *Compiler) hasDropFlag(name string) bool {
	_, ok := c.dropFlags[name]
	return ok
}

// markBorrowOptionalLocal records `name` as a borrow-holding optional local when
// `t` is an Optional type (T1085). Called at the var-decl borrow-clear sites
// (RTTI downcast / `T&`/`T~` RHS) so a later non-diverging heap-user-type handler
// unwrap on this ident (`o? { ... }`) can tell the present arm aliases an external
// owner and must not be neutralized + temp-tracked (that would double-free the
// borrow). Non-optional borrows are ignored — the gate only consults this for
// optional-handler ident sources.
func (c *Compiler) markBorrowOptionalLocal(name string, t types.Type) {
	if c.typeSubst != nil && t != nil {
		t = types.Substitute(t, c.typeSubst)
	}
	if _, isOpt := t.(*types.Optional); !isOpt {
		return
	}
	if c.borrowOptionalLocals == nil {
		c.borrowOptionalLocals = make(map[string]bool)
	}
	c.borrowOptionalLocals[name] = true
}

// emitEarlyDrops checks if any variables should be dropped after the given statement
// (NLL last-use analysis, B0035). For each variable whose last use is this statement,
// emits the drop call and clears the drop flag so scope cleanup skips it.
func (c *Compiler) emitEarlyDrops(stmt ast.Stmt) {
	if c.block == nil || c.block.Term != nil {
		return // block already terminated
	}
	if c.info.EarlyDrops == nil {
		return
	}
	drops, ok := c.info.EarlyDrops[stmt]
	if !ok {
		return
	}
	// Process in reverse order to respect LIFO drop ordering:
	// variables declared later should be dropped first.
	for i := len(drops) - 1; i >= 0; i-- {
		varName := drops[i]
		binding, ok := c.dropBindings[varName]
		if !ok {
			continue // no drop binding (copy type, no-drop type, etc.)
		}
		// Skip use-bound (close) bindings — close() error handling is tied to scope exit.
		if binding.kind == bindingClose {
			continue
		}
		// Emit the appropriate drop call (checks drop flag internally).
		switch binding.kind {
		case bindingDrop:
			c.emitDropCall(binding)
		case bindingDropString:
			c.emitStringDropCall(binding)
		case bindingDropEnum:
			c.emitEnumDropCall(binding)
		case bindingDropOptional:
			c.emitOptionalDropCall(binding)
		case bindingDropTuple:
			c.emitTupleDropCall(binding)
		case bindingDropArray:
			c.emitArrayDropCall(binding)
		case bindingFree:
			c.emitFreeCall(binding)
		case bindingFreeEnv:
			c.emitEnvFree(binding)
		case bindingGenerator:
			c.emitGeneratorCleanup(binding)
		}
		// Clear the drop flag so scope cleanup skips this variable.
		// The drop call above already checked the flag — clearing it here
		// ensures the variable won't be double-dropped at scope exit.
		c.clearDropFlag(varName)
	}
}

// emitScopeCleanup emits cleanup calls for all scope bindings from fromIdx onwards,
// in reverse order (LIFO). Close bindings call close(), drop bindings check the
// drop flag and conditionally call drop().
//
// errorInFlight indicates the scope is exiting due to a raise or error propagation.
// When true, failable close() errors are suppressed. When false and the enclosing
// function is failable, the first close() error is captured and returned.
func (c *Compiler) emitScopeCleanup(fromIdx int, errorInFlight bool) *closeErrCapture {
	// Check if we need error capture: failable function, normal path, and at least
	// one failable close binding in the range.
	//
	// T1387: also capture inside a failable `go! {}` body. There c.canError is
	// false (the coroutine ramp returns i8*, not a failable aggregate), so without
	// this a use-binding whose failable close() fails on the block's SUCCESS exit
	// would take the suppress path (error dropped) and the goroutine would report
	// {ok, value} instead of the error. Capturing here lets emitCloseErrCheck route
	// the error into the goroutine's result aggregate (surfacing it at `<-t`),
	// symmetric to a failable function propagating the close error via its return.
	//
	// T1390: same shape for a failable generator body. There c.canError is false
	// (the generator ramp returns i8*), but c.generatorCanError is true. Without
	// this a use-binding whose failable close() fails on the generator's SUCCESS
	// exit would take the suppress path (error dropped) instead of surfacing at the
	// failable consumer. Capturing here lets emitCloseErrCheck route the error into
	// the generator's error_slot + final suspend (via emitGeneratorError).
	var cap *closeErrCapture
	if (c.canError || c.inFailableGoBlock || (c.inGenerator && c.generatorCanError)) && !errorInFlight {
		for i := len(c.scopeBindings) - 1; i >= fromIdx; i-- {
			b := c.scopeBindings[i]
			if b.kind == bindingClose && b.closeIsFailable {
				cap = &closeErrCapture{
					flag: c.createEntryAlloca(irtypes.I1),
					val:  c.createEntryAlloca(irtypes.I8Ptr),
				}
				cap.flag.SetName(c.uniqueLocalName("close.err.flag"))
				cap.val.SetName(c.uniqueLocalName("close.err.val"))
				c.block.NewStore(constant.NewInt(irtypes.I1, 0), cap.flag)
				c.block.NewStore(constant.NewNull(irtypes.I8Ptr), cap.val)
				break
			}
		}
	}

	for i := len(c.scopeBindings) - 1; i >= fromIdx; i-- {
		b := c.scopeBindings[i]
		switch b.kind {
		case bindingClose:
			c.emitCloseCall(b, cap)
		case bindingDrop:
			c.emitDropCall(b)
		case bindingDropString:
			c.emitStringDropCall(b)
		case bindingDropEnum:
			c.emitEnumDropCall(b)
		case bindingDropOptional:
			c.emitOptionalDropCall(b)
		case bindingDropTuple:
			c.emitTupleDropCall(b)
		case bindingDropArray:
			c.emitArrayDropCall(b)
		case bindingFree:
			c.emitFreeCall(b)
		case bindingFreeEnv:
			c.emitEnvFree(b)
		case bindingGenerator:
			c.emitGeneratorCleanup(b)
		}
	}
	return cap
}

// emitCloseCall emits a close() call for a use-bound variable (direct or virtual dispatch).
// If cap is non-nil and close() is failable, the first error is captured into cap's allocas.
func (c *Compiler) emitCloseCall(b scopeBinding, cap *closeErrCapture) {
	val := c.block.NewLoad(b.alloca.ElemType, b.alloca)

	var result value.Value
	if b.closeFunc != nil {
		// Direct dispatch — extract instance pointer and call.
		// T0156: Container types (e.g. MutexGuard) are opaque i8* — pass directly.
		var receiver value.Value
		if isContainerType(b.valType) {
			receiver = val
		} else {
			receiver = c.extractInstancePtr(val)
		}
		result = c.block.NewCall(b.closeFunc, receiver)
	} else if b.named != nil {
		// Virtual dispatch through vtable
		vtableRaw := c.extractVtablePtr(val)
		instance := c.extractInstancePtr(val)

		slotIndex := b.named.VirtualMethodIndex("close", false)
		if slotIndex < 0 {
			panic(fmt.Sprintf("codegen: close method not in vtable for %s", b.named))
		}
		vtablePtr := c.block.NewBitCast(vtableRaw, irtypes.NewPointer(irtypes.I8Ptr))
		fnSlotPtr := c.block.NewGetElementPtr(irtypes.I8Ptr, vtablePtr,
			constant.NewInt(irtypes.I32, int64(slotIndex)))
		fnRaw := c.block.NewLoad(irtypes.I8Ptr, fnSlotPtr)

		closeMethod := b.named.LookupMethod("close")
		retType := irtypes.Type(irtypes.Void)
		if closeMethod.Sig().CanError() {
			retType = computeResultType(retType)
		}
		funcType := irtypes.NewFunc(retType, irtypes.I8Ptr)
		fnTyped := c.block.NewBitCast(fnRaw, irtypes.NewPointer(funcType))
		result = c.block.NewCall(fnTyped, instance)
	}

	// T0106: After close(), free the heap instance (and droppable fields).
	// use-bound types have close() but may not have drop(). Without this, the
	// heap instance leaks. If the type has a synthesized (field-only) drop, call
	// that (it handles field drops + pal_free). If the type has a *user-defined*
	// drop, T0967 / §16.4 suppresses it (use takes precedence) but still reclaims
	// fields + memory inline. Otherwise, just pal_free the instance directly.
	if b.named != nil && !isContainerType(b.valType) && !b.named.IsValueType() {
		instance := c.extractInstancePtr(val)
		// Null-check before freeing
		nullCheck := c.block.NewICmp(enum.IPredEQ, instance, constant.NewNull(irtypes.I8Ptr))
		freeBlock := c.newBlock("close.free")
		freeDone := c.newBlock("close.free.done")
		c.block.NewCondBr(nullCheck, freeDone, freeBlock)

		c.block = freeBlock
		// For a generic use-bound type, b.named is the *generic* origin (its fields
		// reference unbound TypeParams) while b.valType carries the concrete type
		// args. Resolve to the concrete instance so the mono drop / mono layout is
		// used — otherwise the close-free path operates on the generic layout and
		// silently skips droppable fields → leak. Mirrors maybeRegisterDrop.
		resolvedTyp := b.valType
		if c.typeSubst != nil {
			resolvedTyp = types.Substitute(b.valType, c.typeSubst)
		}
		// Does this instance own droppable fields? b.named is the generic *origin*,
		// whose NeedsSynthDrop/HasDrop flags reflect only the unsubstituted shape —
		// a field typed `T` reports non-droppable at sema time. For a concrete
		// generic instance the droppability is known only via monoInstNeedsSynthDrop
		// (TypeParam fields resolving to droppables). Check all three so close-only
		// generic types (e.g. `use b := Box[Inner](...)`) don't leak their fields.
		needsFieldCleanup := b.named.HasDrop() || b.named.NeedsSynthDrop()
		if !needsFieldCleanup {
			if inst, ok := resolvedTyp.(*types.Instance); ok {
				needsFieldCleanup = monoInstNeedsSynthDrop(inst)
			}
		}
		if needsFieldCleanup {
			// Reclaim droppable fields + the heap instance WITHOUT running any
			// user-defined drop() body. This is correct for two distinct cases:
			//   (a) synthesized field-cleanup drop (no user logic) — identical to
			//       defineSynthesizedDropBody, just inlined here; and
			//   (b) T0967 / language-design §16.4: a `use`-bound value whose type
			//       also defines drop() — `use` takes precedence, so the user
			//       drop() body is suppressed (close() performs all cleanup) while
			//       owned fields + memory are still reclaimed exactly once.
			c.emitInstanceFieldDropsAndFree(b.named, resolvedTyp, instance)
		} else {
			// No droppable fields — just free the instance.
			c.block.NewCall(c.palFree, instance)
		}
		c.block.NewBr(freeDone)
		c.block = freeDone
	}

	// Handle failable close() errors: capture, suppress+drop, or ignore.
	if b.closeIsFailable && result != nil {
		resultType := result.Type().(*irtypes.StructType)
		tag := c.block.NewExtractValue(result, 0)

		if cap != nil {
			// Capture path: save first error, drop subsequent errors (T0135).
			errBlock := c.newBlock("close.err")
			contBlock := c.newBlock("close.cont")
			c.block.NewCondBr(tag, errBlock, contBlock)

			c.block = errBlock
			hasErr := c.block.NewLoad(irtypes.I1, cap.flag)
			saveBlock := c.newBlock("close.save")
			dropDupBlock := c.newBlock("close.err.drop.dup")
			c.block.NewCondBr(hasErr, dropDupBlock, saveBlock)

			// Save first error
			c.block = saveBlock
			c.block.NewStore(constant.NewInt(irtypes.I1, 1), cap.flag)
			errVal := c.block.NewExtractValue(result, resultErrIdx(resultType))
			c.block.NewStore(errVal, cap.val)
			c.block.NewBr(contBlock)

			// T0135: Drop duplicate close error to prevent leak
			c.block = dropDupBlock
			dupErrVal := c.block.NewExtractValue(result, resultErrIdx(resultType))
			c.emitDropSuppressedError(dupErrVal)
			c.block.NewBr(contBlock)

			c.block = contBlock
		} else {
			// T0135: Suppress path (error in flight or non-failable function).
			// Drop the close error to prevent leak.
			errDropBlock := c.newBlock("close.err.drop")
			contBlock := c.newBlock("close.cont")
			c.block.NewCondBr(tag, errDropBlock, contBlock)

			c.block = errDropBlock
			errVal := c.block.NewExtractValue(result, resultErrIdx(resultType))
			c.emitDropSuppressedError(errVal)
			c.block.NewBr(contBlock)

			c.block = contBlock
		}
	}
}

// emitCloseErrCheck checks a captured close error and, if set, returns it from
// the current failable function. Otherwise, continues in a new block.
//
// outerFloor is the same index passed to the preceding
// emitScopeCleanup(outerFloor, false): that call cleaned only the block-local
// suffix [outerFloor, end). The error path below escapes the function (return /
// generator-error / go-block-error), skipping the enclosing scope's own cleanup
// of the outer prefix [0, outerFloor). T1388: unwind that prefix here with
// errorInFlight=true before escaping, mirroring the raise/?^ paths — otherwise
// still-live outer bindings leak.
func (c *Compiler) emitCloseErrCheck(cap *closeErrCapture, outerFloor int) {
	if cap == nil {
		return
	}
	flag := c.block.NewLoad(irtypes.I1, cap.flag)
	errRetBlock := c.newBlock("close.err.ret")
	contBlock := c.newBlock("close.ok.cont")
	c.block.NewCondBr(flag, errRetBlock, contBlock)

	c.block = errRetBlock
	// T1388: clean the outer prefix [0, outerFloor) that this early exit would
	// otherwise skip. The suffix [outerFloor, end) was already cleaned before the
	// branch, so union = [0, end) with no overlap. errorInFlight=true suppresses
	// any secondary close error among the outer bindings ("first close error wins").
	if outerFloor > 0 {
		savedBindings := c.scopeBindings
		c.scopeBindings = c.scopeBindings[:outerFloor]
		c.emitScopeCleanup(0, true)
		c.scopeBindings = savedBindings
	}
	errVal := c.block.NewLoad(irtypes.I8Ptr, cap.val)
	// T1388: this early error-exit does NOT clean up outer still-live bindings
	// (bindings below the block's savedScopeLen) — they leak. Pre-existing and
	// systemic across all three branches below (normal fn / generator / go-block);
	// only manifests when an outer live binding coexists with a failing nested
	// close. Tracked separately from T1387 (correctness of the go-block routing).
	if c.inFailableGoBlock {
		// T1384/T1387: a use-binding `close()` that fails inside a `go! {}` body
		// routes its error into the goroutine's result aggregate so it surfaces at
		// `<-t` (a `ret` is invalid in the coro ramp). Reached because T1387
		// broadened emitScopeCleanup's cap-capture guard to fire in a failable
		// go-block (where c.canError is false).
		//
		// T1427: on a value exit the computed heap success value was claimed away
		// from temp cleanup for the store into G.result_ptr — but this divert stores
		// the {err} aggregate instead of that value, so the value would be orphaned.
		// Drop it here (this branch and the OK-continuation store are mutually
		// exclusive, so exactly one frees it — no double free).
		if c.goResultDivertVal != nil && c.goResultDivertType != nil {
			c.emitVariantFieldDrop(c.goResultDivertVal, c.goResultDivertType)
		}
		c.emitFailableGoBlockError(errVal)
	} else if c.inGenerator && c.generatorCanError {
		// B0023: store error to generator error_slot and branch to final suspend.
		c.emitGeneratorError(errVal)
	} else {
		resultType := c.currentResultType()
		c.block.NewRet(c.wrapError(errVal, resultType))
	}

	c.block = contBlock
}

// isDroppableHeapUserType returns true if typ is a heap user type whose instance
// is heap-allocated and registered for drop or pal_free at scope exit — i.e., the
// kind of type whose alias would cause double-free after a container index read.
// Excludes strings (handled by dupStringFieldAccess), containers/Arc/Weak
// (dupContainerFieldAccess), tuples (dupTupleFieldAccess), and
// borrow/value/Copy/primitive/structural types. T0398.
//
// T0440: Also excludes Map and Set — these are user-defined generic containers
// with their own clone() methods that don't reliably deep-clone V values (Map's
// clone uses `result[k] = v` which shallow-copies value-structs for nested heap
// types). Treating them as plain heap user types here would route through the
// problematic clone path; instead, we leave them to the existing aliasing
// behavior at the if-let/force-unwrap site.
func isDroppableHeapUserType(typ types.Type) bool {
	if isRefType(typ) {
		return false
	}
	if isContainerType(typ) {
		return false
	}
	named := extractNamed(typ)
	if named == nil {
		return false
	}
	if named == types.TypString {
		return false
	}
	if named == types.TypMap {
		return false
	}
	if named.Obj() != nil && named.Obj().Name() == "Set" {
		return false
	}
	if named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) || named.IsStructural() {
		return false
	}
	// T0440: Also requires the type to have an explicit drop or synthesized drop.
	// Heap user types without any drop method use the bindingFree (pal_free) path,
	// which has a separate latent leak issue with cloned values (T0484). Restricting
	// the dup branch to types with drop methods ensures the bindingDrop path
	// (which correctly emits drop+pal_free) handles the cloned instances.
	if !named.HasDrop() && !named.NeedsSynthDrop() {
		return false
	}
	return true
}

// isHeapUserNoDropPalFree returns true for heap user types that are heap-
// allocated (and thus need pal_free at scope exit) but have no explicit `drop()`
// or synthesized drop — i.e., types excluded by `isDroppableHeapUserType` for
// the T0440 Map-clone-gating reason. Used by genArrayIndex's T0590 dup-on-read:
// arrays have no internal match-dup, so slot-to-slot / let-then-X reads must
// dup these pointers to avoid aliasing + double-free at pal_free time.
func isHeapUserNoDropPalFree(typ types.Type) bool {
	if isRefType(typ) {
		return false
	}
	if isContainerType(typ) {
		return false
	}
	named := extractNamed(typ)
	if named == nil {
		return false
	}
	if named == types.TypString {
		return false
	}
	if named == types.TypMap {
		return false
	}
	if named.Obj() != nil && named.Obj().Name() == "Set" {
		return false
	}
	if named.IsValueType() || named.IsCopy() || isPrimitiveScalar(named) || named.IsStructural() {
		return false
	}
	// Drop / synth-drop case is handled by `isDroppableHeapUserType` — this
	// helper covers only the pal_free-only complement.
	if named.HasDrop() || named.NeedsSynthDrop() {
		return false
	}
	return true
}

// maybeRegisterEnvFree registers a scope binding to free the closure's env struct
// at scope exit. Only applies to variables whose type is *types.Signature (function values).
func (c *Compiler) maybeRegisterEnvFree(varName string, alloca *ir.InstAlloca, typ types.Type, valueExpr ast.Expr) {
	if _, ok := typ.(*types.Signature); !ok {
		return
	}
	// T0812: reading a closure out of an owning aggregate (struct/optional field,
	// container element) aliases the aggregate's heap env — it does not transfer
	// ownership. Registering an owning env-free binding here would double-free the
	// env at scope exit against the aggregate's own drop. The aggregate retains
	// ownership; the local borrows.
	if c.isClosureAggregateBorrow(valueExpr) {
		return
	}
	// T1259: `h := g` where `g` is a match-borrowed closure alias destructured from
	// an enum variant (`E.Cb(() -> int f)`) whose env the enum's own drop frees. The
	// re-bound local borrows — registering an owning env-free here would double-free
	// the shared env against the enum's drop. isClosureAggregateBorrow does not peel
	// ident sources, so consult the shared match-borrow gate (single source of truth
	// with the container var-decl arms). typ is Signature-gated above, so the Deep
	// predicate inside always holds for a genuine closure ident.
	if c.identBorrowsMatchBorrowedClosure(valueExpr, typ) {
		return
	}
	dropFlag := c.createEntryAlloca(irtypes.I1)
	dropFlag.SetName(c.uniqueLocalName(varName + ".dropflag"))
	c.block.NewStore(constant.NewInt(irtypes.I1, 1), dropFlag)
	c.dropFlags[varName] = dropFlag

	c.scopeBindings = append(c.scopeBindings, scopeBinding{
		kind:     bindingFreeEnv,
		alloca:   alloca,
		dropFlag: dropFlag,
		varName:  varName,
	})
}
