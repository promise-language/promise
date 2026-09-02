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
			// T1338: snapshot the enum-ctor temp count BEFORE evaluating this arg.
			// Per-iteration (not pre-loop): a later call-arg's drain must not sweep
			// an earlier DIRECT ctor arg's kept temp (`E.V(F.W(x), describe(G.H(y)))`).
			argEnumSnap := len(c.enumCtorTemps)
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
			// T1338: drain any inline enum-ctor temp buried as a by-value arg inside
			// THIS arg's evaluation (e.g. `describe(Payload.Full(heap))`). The callee
			// borrowed/dup'd it (B0232), so the caller must free it here — otherwise
			// the enclosing move-out site's wholesale enumCtorTemps clear zeroes its
			// flag and orphans the payload. Skipped when `arg.Value` is itself a ctor
			// (moved into the payload — kept tracked).
			c.drainNestedEnumCtorTemps(arg.Value, argEnumSnap)
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
		// T1326: snapshot the enum-ctor temp count before evaluating the arm body so
		// drainNestedArmEnumCtorTemps can drop any by-value call-arg ctor temps this
		// arm creates that are NOT the phi'd result.
		tailEnumSnap := len(c.enumCtorTemps)
		if c.info.AutoPropagateExprs[arm.Body] {
			result := c.genExpr(arm.Body)
			unwrapped := c.genAutoPropagateTracked(arm.Body, result)
			c.drainNestedArmEnumCtorTemps(arm.Body, tailEnumSnap)
			return unwrapped
		}
		v := c.genExpr(arm.Body)
		c.drainNestedArmEnumCtorTemps(arm.Body, tailEnumSnap)
		return v
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
		// T1332: distinguish a value-position match whose arms all diverge
		// (return/raise — no arm branches to mergeBlock, so it is dead code) from
		// a statement-position void match (arms reach mergeBlock with no value).
		// Only the former gets an `unreachable` + typed undef; the latter keeps the
		// reachable merge block and returns nil as before.
		anyReachesMerge := false
		for _, a := range arms {
			if a.end != nil && a.end.Term != nil {
				if br, ok := a.end.Term.(*ir.TermBr); ok && br.Target == mergeBlock {
					anyReachesMerge = true
					break
				}
			}
		}
		if anyReachesMerge {
			return nil
		}
		return c.emitDivergedMergeValue(mergeBlock, resultType)
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
	return c.emitDivergedMergeValue(mergeBlock, resultType)
}

// emitDivergedMergeValue handles a value-position if/match whose arms all diverge
// (return/raise): mergeBlock has no predecessor, so it is dead code. Terminate it
// with `unreachable` and return a well-typed poison value of the expression's
// result type so value consumers produce valid (dead) IR instead of crashing on a
// nil value (T1332). resultType comes from sema (the contextual hint); fall back to
// c.targetType, then nil (statement position — value discarded).
func (c *Compiler) emitDivergedMergeValue(mergeBlock *ir.Block, resultType types.Type) value.Value {
	if mergeBlock.Term == nil {
		mergeBlock.NewUnreachable()
	}
	rt := resultType
	if rt == nil {
		rt = c.targetType
	}
	if rt == nil {
		return nil // statement position: no consumer
	}
	return constant.NewUndef(c.resolveType(rt))
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
	if taskElem, ok, taskFail := types.AsAnyTaskFailable(rt); ok {
		return c.getOrCreateTaskDrop(taskElem, taskFail), nil
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
