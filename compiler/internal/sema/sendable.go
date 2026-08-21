package sema

import (
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// isSendableType reports whether a type's values may be moved across goroutine
// boundaries. Primitives, value types with all-sendable fields, channels, arcs,
// tasks and closures (Signature) are sendable.
func isSendableType(typ types.Type, visited map[types.Type]bool) bool {
	if typ == nil {
		return false
	}
	if visited[typ] {
		return true // optimistic for cycles (self-referential types via Optional)
	}
	// Primitives are always sendable
	switch typ {
	case types.TypInt, types.TypI8, types.TypI16, types.TypI32, types.TypI64,
		types.TypUint, types.TypU8, types.TypU16, types.TypU32, types.TypU64,
		types.TypI128, types.TypU128, types.TypI256, types.TypU256, types.TypI512, types.TypU512,
		types.TypF32, types.TypF64,
		types.TypBool, types.TypChar, types.TypString, types.TypNone, types.TypVoid:
		return true
	}
	visited[typ] = true
	switch t := typ.(type) {
	case *types.Named:
		if t.IsNotSendable() {
			return false
		}
		if t.IsSendable() {
			return true
		}
		// Auto-derive: sendable iff all fields are sendable
		for _, f := range t.Fields() {
			if !isSendableType(f.Type(), visited) {
				return false
			}
		}
		return true
	case *types.Enum:
		if t.IsNotSendable() {
			return false
		}
		if t.IsSendable() {
			return true
		}
		// Auto-derive: sendable iff all variant fields are sendable
		for _, v := range t.Variants() {
			for _, f := range v.Fields() {
				if !isSendableType(f.Type(), visited) {
					return false
				}
			}
		}
		return true
	case *types.Instance:
		origin := t.Origin()
		// T0995: a Ref[T]/Weak[T] with a `confined element type is thread-confined —
		// it uses a non-atomic counter and must not cross a goroutine boundary. This
		// is the soundness point that makes the non-atomic counter safe.
		if (origin == types.TypArc || origin == types.TypWeak) && len(t.TypeArgs()) > 0 && isConfinedType(t.TypeArgs()[0]) {
			return false
		}
		// Channel, Arc, Weak, Task are inherently sendable (internal synchronization)
		if origin == types.TypChannel || origin == types.TypArc || origin == types.TypWeak ||
			origin == types.TypTask || origin == types.TypFailableTask {
			return true
		}
		// Containers: sendable iff element types are sendable
		for _, ta := range t.TypeArgs() {
			if !isSendableType(ta, visited) {
				return false
			}
		}
		// Check origin type
		switch o := origin.(type) {
		case *types.Named:
			return isSendableType(o, visited)
		case *types.Enum:
			return isSendableType(o, visited)
		}
		return true
	case *types.Optional:
		return isSendableType(t.Elem(), visited)
	case *types.Tuple:
		for _, elem := range t.Elems() {
			if !isSendableType(elem, visited) {
				return false
			}
		}
		return true
	case *types.Array:
		return isSendableType(t.Elem(), visited)
	case *types.SharedRef, *types.MutRef:
		return true // refs themselves are sendable
	case *types.Signature:
		// T1640 (R1): a function value is sendable. Its sendability is not derived
		// from the erased capture set — by the time a closure reaches a boundary
		// the captures are invisible — but guaranteed by three checks upstream:
		//   R2 — every closure capture must itself be sendable, enforced at the
		//        capture site by rejectNonSendableCapture (sema/expr.go).
		//   R3 — a closure captured into a `go` block MOVES its heap env into the
		//        coroutine frame (codegen's B0354 ownership transfer), so the env
		//        outlives the defining scope and is freed exactly once.
		//   R5 — only a binding that provably OWNS its env may cross the boundary
		//        (ownership/goclosure.go); a borrow of someone else's env is
		//        rejected with a pointer to `move`.
		// Without R3/R5 this would be a use-after-free even for a capture-free
		// closure, since the env is freed at exit of the DEFINING scope.
		return true
	case *types.TypeParam:
		return true // assumed sendable; validated at instantiation
	}
	return false
}

// isSharableType reports whether a &T reference to this type may be shared
// across goroutines. Same structure as isSendableType.
func isSharableType(typ types.Type, visited map[types.Type]bool) bool {
	if typ == nil {
		return false
	}
	if visited[typ] {
		return true
	}
	switch typ {
	case types.TypInt, types.TypI8, types.TypI16, types.TypI32, types.TypI64,
		types.TypUint, types.TypU8, types.TypU16, types.TypU32, types.TypU64,
		types.TypI128, types.TypU128, types.TypI256, types.TypU256, types.TypI512, types.TypU512,
		types.TypF32, types.TypF64,
		types.TypBool, types.TypChar, types.TypString, types.TypNone, types.TypVoid:
		return true
	}
	visited[typ] = true
	switch t := typ.(type) {
	case *types.Named:
		if t.IsNotSharable() {
			return false
		}
		if t.IsSharable() {
			return true
		}
		for _, f := range t.Fields() {
			if !isSharableType(f.Type(), visited) {
				return false
			}
		}
		return true
	case *types.Enum:
		if t.IsNotSharable() {
			return false
		}
		if t.IsSharable() {
			return true
		}
		for _, v := range t.Variants() {
			for _, f := range v.Fields() {
				if !isSharableType(f.Type(), visited) {
					return false
				}
			}
		}
		return true
	case *types.Instance:
		origin := t.Origin()
		// Channel, Arc, Weak are inherently sharable (internal synchronization)
		if origin == types.TypChannel || origin == types.TypArc || origin == types.TypWeak {
			return true
		}
		// Task handles are sharable (read-only handle)
		if origin == types.TypTask || origin == types.TypFailableTask {
			return true
		}
		for _, ta := range t.TypeArgs() {
			if !isSharableType(ta, visited) {
				return false
			}
		}
		switch o := origin.(type) {
		case *types.Named:
			return isSharableType(o, visited)
		case *types.Enum:
			return isSharableType(o, visited)
		}
		return true
	case *types.Optional:
		return isSharableType(t.Elem(), visited)
	case *types.Tuple:
		for _, elem := range t.Elems() {
			if !isSharableType(elem, visited) {
				return false
			}
		}
		return true
	case *types.Array:
		return isSharableType(t.Elem(), visited)
	case *types.SharedRef, *types.MutRef:
		return true
	case *types.Signature:
		// T1640 leaves closures non-SHARABLE even though R1 makes them sendable:
		// a naked `&closure` hands two goroutines the same heap env with no
		// refcount, and invoking it re-enters the env concurrently. Wrapped,
		// writeback-free sharing (`Ref[() -> int]`) is T1649.
		return false
	case *types.TypeParam:
		return true
	}
	return false
}

// isConfinedType reports whether the type's declaration is marked `confined
// (T0995). Canonical logic lives in types.IsConfined.
func isConfinedType(typ types.Type) bool {
	return types.IsConfined(typ)
}

// checkGoBlockCaptures walks a go block's AST and (a) checks that all captured
// variables (those defined in an enclosing scope) have sendable types, and
// (b) records the capture set into Info.GoCaptures so ownership can apply the
// T1640 env-ownership rules (R4/R5) to closure-typed captures. The boundary
// itself cannot recover the capture set — a `go` block has no capture list in
// the AST — so this walk is the single place it is computed.
func (c *Checker) checkGoBlockCaptures(e *ast.GoExpr) {
	if e.Block == nil {
		return
	}
	goScope := c.info.Scopes[e.Block]
	if goScope == nil {
		return
	}

	// Walk all identifiers in the block and check cross-scope references
	seen := make(map[string]bool)
	var captured []*CapturedVar
	// record notes a cross-scope reference: it runs the sendability gate once per
	// name and appends the variable to the go block's capture set.
	record := func(pos ast.Pos, v *types.Var, obj types.Object) {
		if seen[v.Name()] {
			return
		}
		seen[v.Name()] = true
		typ := v.Type()
		// T1589: a `~` (mutable-borrow) parameter cannot be captured into a
		// `go {}` block. A mutable borrow's lifetime is bounded by the
		// enclosing call, so letting it escape into a goroutine (which runs
		// asynchronously) would alias/dangle the borrow — and codegen would
		// otherwise emit malformed IR referencing the enclosing function's
		// parameter. Reject it; pass an owned value instead. Mirrors the
		// lambda-capture rejection in checkLambdaCapture.
		if _, isMutRef := typ.(*types.MutRef); isMutRef {
			c.errorf(pos, "cannot capture mutable borrow '%s' in a goroutine; a `~` parameter is a mutable borrow that cannot outlive the call — pass an owned value instead", v.Name())
			return
		}
		if typ != nil && !isSendableType(typ, make(map[types.Type]bool)) {
			c.errorf(pos, "cannot send non-sendable variable '%s' of type %s across goroutine boundary", v.Name(), typ)
			return
		}
		// T1653: B0354's capture transfer re-derives the goroutine-side owning
		// binding from the OUTER binding's element type, which is wrong for every
		// optional capture — a `string?` panics codegen, and an optional-wrapped
		// closure leaks its heap env because no arm registers the env-free. The
		// bare closure and the `go f(move opt)` call form both work, so gate only
		// this shape until T1653 restores the binding kind.
		if optionalClosureType(typ) {
			c.errorf(pos, "cannot capture optional closure '%s' in a goroutine; an optional-wrapped closure's environment is not transferred into the coroutine frame — unwrap it into a plain closure local first", v.Name())
			return
		}
		captured = append(captured, &CapturedVar{Obj: obj})
	}
	var walkExpr func(expr ast.Expr)
	var walkBlock func(block *ast.Block)
	var walkStmt func(s ast.Stmt)
	var walkElse func(s ast.Stmt)

	walkBlock = func(block *ast.Block) {
		if block == nil {
			return
		}
		for _, s := range block.Stmts {
			walkStmt(s)
		}
	}

	walkElse = func(s ast.Stmt) {
		if s == nil {
			return
		}
		switch st := s.(type) {
		case *ast.IfStmt:
			walkStmt(st)
		case *ast.Block:
			walkBlock(st)
		}
	}

	walkStmt = func(s ast.Stmt) {
		if s == nil {
			return
		}
		switch st := s.(type) {
		case *ast.ExprStmt:
			walkExpr(st.Expr)
		case *ast.TypedVarDecl:
			walkExpr(st.Value)
		case *ast.InferredVarDecl:
			walkExpr(st.Value)
		case *ast.DestructureVarDecl:
			walkExpr(st.Value)
		case *ast.UseVarDecl:
			walkExpr(st.Value)
		case *ast.AssignStmt:
			walkExpr(st.Target)
			walkExpr(st.Value)
		case *ast.ReturnStmt:
			walkExpr(st.Value)
		case *ast.IfStmt:
			walkExpr(st.Cond)
			walkExpr(st.Init)
			walkBlock(st.Body)
			if st.Else != nil {
				walkElse(st.Else)
			}
		case *ast.ForInStmt:
			walkExpr(st.Iterable)
			walkBlock(st.Body)
		case *ast.ClassicForStmt:
			walkExpr(st.InitValue)
			walkExpr(st.Cond)
			walkExpr(st.UpdateTarget)
			walkExpr(st.UpdateValue)
			walkBlock(st.Body)
		case *ast.WhileStmt:
			walkExpr(st.Cond)
			walkBlock(st.Body)
		case *ast.WhileUnwrapStmt:
			walkExpr(st.Value)
			walkBlock(st.Body)
		case *ast.InfiniteLoop:
			walkBlock(st.Body)
		case *ast.RaiseStmt:
			walkExpr(st.Value)
		case *ast.YieldStmt:
			walkExpr(st.Value)
		case *ast.YieldDelegateStmt:
			walkExpr(st.Value)
		case *ast.IncDecStmt:
			walkExpr(st.Target)
		case *ast.SelectStmt:
			for _, sc := range st.Cases {
				walkExpr(sc.Channel)
				walkExpr(sc.SendValue)
				for _, bs := range sc.Body {
					walkStmt(bs)
				}
			}
			for _, ds := range st.Default {
				walkStmt(ds)
			}
		}
	}

	walkExpr = func(expr ast.Expr) {
		if expr == nil {
			return
		}
		switch ex := expr.(type) {
		case *ast.IdentExpr:
			obj := c.info.Objects[ex]
			if obj == nil {
				return
			}
			v, ok := obj.(*types.Var)
			if !ok {
				return
			}
			// Check if the variable is from an enclosing scope (not within the go block)
			if !scopeContains(goScope, v.Pos()) {
				record(ex.Pos(), v, obj)
			}
		case *ast.BinaryExpr:
			walkExpr(ex.Left)
			walkExpr(ex.Right)
		case *ast.UnaryExpr:
			walkExpr(ex.Operand)
		case *ast.CallExpr:
			walkExpr(ex.Callee)
			for _, arg := range ex.Args {
				walkExpr(arg.Value)
			}
		case *ast.IndexExpr:
			walkExpr(ex.Target)
			walkExpr(ex.Index)
		case *ast.SliceExpr:
			walkExpr(ex.Target)
			walkExpr(ex.Low)
			walkExpr(ex.High)
		case *ast.MemberExpr:
			walkExpr(ex.Target)
		case *ast.OptionalChainExpr:
			walkExpr(ex.Target)
		case *ast.LambdaExpr:
			// Lambda bodies are their own scope, so the body's identifiers are not
			// walked here — but codegen's collectBlockIdents pulls in
			// info.LambdaCaptures[e] (expr_concurrency.go), so a value reaching the
			// goroutine ONLY through a nested lambda IS captured by the coroutine.
			// Mirror that: check and record the nested lambda's captures that come
			// from outside the go block. Without this, such a value crossed the
			// boundary unchecked.
			for _, cv := range c.info.LambdaCaptures[ex] {
				v, ok := cv.Obj.(*types.Var)
				if !ok || v.Name() == "this" {
					continue
				}
				if !scopeContains(goScope, v.Pos()) {
					record(ex.Pos(), v, cv.Obj)
				}
			}
		case *ast.GoExpr:
			// Nested go blocks are their own scope
		case *ast.TupleLit:
			for _, el := range ex.Elements {
				walkExpr(el)
			}
		case *ast.IfExpr:
			walkExpr(ex.Cond)
			if ex.Then != nil {
				walkBlock(ex.Then)
			}
			if ex.Else != nil {
				walkBlock(ex.Else)
			}
		case *ast.MatchExpr:
			walkExpr(ex.Subject)
			for _, arm := range ex.Arms {
				walkExpr(arm.Body)
				walkBlock(arm.Block)
			}
		case *ast.CastExpr:
			walkExpr(ex.Expr)
		case *ast.ArrayLit:
			for _, el := range ex.Elements {
				walkExpr(el)
			}
		case *ast.ArrayRepeatLit:
			walkExpr(ex.Value)
			walkExpr(ex.Count)
		case *ast.MapLit:
			for _, entry := range ex.Entries {
				walkExpr(entry.Key)
				walkExpr(entry.Value)
			}
		case *ast.ParenExpr:
			walkExpr(ex.Expr)
		case *ast.ErrorPropagateExpr:
			walkExpr(ex.Expr)
		case *ast.ErrorPanicExpr:
			walkExpr(ex.Expr)
		case *ast.OptionalUnwrapExpr:
			walkExpr(ex.Expr)
		case *ast.AutoCloneExpr: // T0605
			walkExpr(ex.Expr)
		case *ast.ErrorHandlerExpr:
			walkExpr(ex.Expr)
			walkBlock(ex.Body)
		case *ast.IsExpr:
			walkExpr(ex.Expr)
		case *ast.StringLit:
			for _, p := range ex.Parts {
				if interp, ok := p.(ast.StringInterp); ok {
					walkExpr(interp.Expr)
				}
			}
		}
	}

	walkBlock(e.Block)
	c.info.GoCaptures[e] = captured
}

// checkGoExprSendable checks that arguments to a go expression (function call form)
// have sendable types.
func (c *Checker) checkGoExprSendable(expr ast.Expr) {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return
	}
	for _, arg := range call.Args {
		typ := c.info.Types[arg.Value]
		if typ != nil && !isSendableType(typ, make(map[types.Type]bool)) {
			c.errorf(arg.Value.Pos(), "cannot send non-sendable argument of type %s across goroutine boundary", typ)
		}
	}
}

// scopeContains reports whether the given position falls within the scope's range.
//
// T1651: compare (line, column) LEXICOGRAPHICALLY. Testing the three line cases
// independently made a one-line scope swallow its whole line: with
// start.Line == end.Line the "pos is on the end line, at or before end.Column"
// arm matched positions that precede the scope's start. A `go { … }` written on
// one line then classified the surrounding declarations as being INSIDE it, so
// every capture-driven gate that keys off this — the sendability check, T1589's
// mutable-borrow rejection, and T1640's closure env-ownership rules, all of
// which consume the same walk — silently did not run. That made a use-after-free
// (or a `not_sendable` value crossing the boundary) purely a function of source
// layout.
func scopeContains(s *types.Scope, pos types.Pos) bool {
	if s == nil {
		return false
	}
	start := s.Pos()
	end := s.End()
	atOrAfterStart := pos.Line > start.Line || (pos.Line == start.Line && pos.Column >= start.Column)
	atOrBeforeEnd := pos.Line < end.Line || (pos.Line == end.Line && pos.Column <= end.Column)
	return atOrAfterStart && atOrBeforeEnd
}

// validateSendableInstance checks sendable/sharable constraints on generic type
// instantiation. Channel[T] requires T to be sendable. Ref[T] requires T to be
// sendable and sharable.
func (c *Checker) validateSendableInstance(pos ast.Pos, origin types.Type, typeArgs []types.Type) {
	named, ok := origin.(*types.Named)
	if !ok || len(typeArgs) == 0 {
		return
	}
	elemType := typeArgs[0]
	if named == types.TypChannel {
		if !isSendableType(elemType, make(map[types.Type]bool)) {
			c.errorf(pos, "Channel element type %s is not sendable", elemType)
		}
	} else if named == types.TypArc {
		// T0995: a `confined element type opts a Ref out of cross-goroutine sharing
		// (and out of atomic refcounting). Such a Ref can never cross a goroutine
		// boundary (isSendableType reports it non-sendable), so T need not be
		// sendable or sharable — the boundary checks reject it at any go/channel/Task
		// site instead. For non-confined T the usual constraints apply.
		if !isConfinedType(elemType) {
			if !isSendableType(elemType, make(map[types.Type]bool)) {
				c.errorf(pos, "Ref element type %s is not sendable", elemType)
			}
			if !isSharableType(elemType, make(map[types.Type]bool)) {
				c.errorf(pos, "Ref element type %s is not sharable", elemType)
			}
		}
	} else if named == types.TypWeak {
		// T0157: Weak[T] has same constraints as Ref[T] — T must be sendable and
		// sharable, unless `confined (mirroring Ref above).
		if !isConfinedType(elemType) {
			if !isSendableType(elemType, make(map[types.Type]bool)) {
				c.errorf(pos, "Weak element type %s is not sendable", elemType)
			}
			if !isSharableType(elemType, make(map[types.Type]bool)) {
				c.errorf(pos, "Weak element type %s is not sharable", elemType)
			}
		}
	}
}

// validateSendableTypes runs after all types are defined to validate explicit
// `sendable / `sharable annotations. If a non-native type is marked `sendable
// but has a non-sendable field, that's an error. Native types use the tag as an
// override and skip field validation.
func (c *Checker) validateSendableTypes(file *ast.File) {
	for _, decl := range file.Decls {
		if c.info.FilteredDecls[decl] {
			continue
		}
		switch d := decl.(type) {
		case *ast.TypeDecl:
			c.validateSendableTypeDecl(d)
		case *ast.EnumDecl:
			c.validateSendableEnumDecl(d)
		}
	}
}

func (c *Checker) validateSendableTypeDecl(d *ast.TypeDecl) {
	obj := c.scope.Lookup(d.Name)
	if obj == nil {
		return
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return
	}
	named, ok := tn.Type().(*types.Named)
	if !ok {
		return
	}

	isNative := c.hasAnnotation(d.Annotations, "native")
	hasSendable := c.hasAnnotation(d.Annotations, "sendable")
	hasSharable := c.hasAnnotation(d.Annotations, "sharable")
	hasNotSendable := c.hasAnnotation(d.Annotations, "not_sendable")
	hasNotSharable := c.hasAnnotation(d.Annotations, "not_sharable")
	hasConfined := c.hasAnnotation(d.Annotations, "confined")

	// Contradictory tags
	if hasSendable && hasNotSendable {
		c.errorf(d.Pos(), "type %s has contradictory `sendable and `not_sendable annotations", d.Name)
	}
	if hasSharable && hasNotSharable {
		c.errorf(d.Pos(), "type %s has contradictory `sharable and `not_sharable annotations", d.Name)
	}
	// T0995: `confined makes a type thread-confined (Ref/Weak use a non-atomic
	// counter and may not cross goroutines), so it cannot also be `sharable.
	if hasConfined && hasSharable {
		c.errorf(d.Pos(), "type %s has contradictory `confined and `sharable annotations", d.Name)
	}

	// Validate explicit `sendable assertion on non-native types
	if hasSendable && !isNative {
		for _, f := range named.Fields() {
			if !isSendableType(f.Type(), make(map[types.Type]bool)) {
				c.errorf(d.Pos(), "type %s is marked `sendable but field '%s' has non-sendable type %s",
					d.Name, f.Name(), f.Type())
			}
		}
	}

	// Validate explicit `sharable assertion on non-native types
	if hasSharable && !isNative {
		for _, f := range named.Fields() {
			if !isSharableType(f.Type(), make(map[types.Type]bool)) {
				c.errorf(d.Pos(), "type %s is marked `sharable but field '%s' has non-sharable type %s",
					d.Name, f.Name(), f.Type())
			}
		}
	}
}

func (c *Checker) validateSendableEnumDecl(d *ast.EnumDecl) {
	obj := c.scope.Lookup(d.Name)
	if obj == nil {
		return
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return
	}
	enum, ok := tn.Type().(*types.Enum)
	if !ok {
		return
	}

	hasSendable := c.hasAnnotation(d.Annotations, "sendable")
	hasSharable := c.hasAnnotation(d.Annotations, "sharable")
	hasNotSendable := c.hasAnnotation(d.Annotations, "not_sendable")
	hasNotSharable := c.hasAnnotation(d.Annotations, "not_sharable")
	hasConfined := c.hasAnnotation(d.Annotations, "confined")

	if hasSendable && hasNotSendable {
		c.errorf(d.Pos(), "enum %s has contradictory `sendable and `not_sendable annotations", d.Name)
	}
	if hasSharable && hasNotSharable {
		c.errorf(d.Pos(), "enum %s has contradictory `sharable and `not_sharable annotations", d.Name)
	}
	// T0995: `confined and `sharable are mutually exclusive (see type decl).
	if hasConfined && hasSharable {
		c.errorf(d.Pos(), "enum %s has contradictory `confined and `sharable annotations", d.Name)
	}

	if hasSendable {
		for _, v := range enum.Variants() {
			for _, f := range v.Fields() {
				if !isSendableType(f.Type(), make(map[types.Type]bool)) {
					c.errorf(d.Pos(), "enum %s is marked `sendable but variant %s field '%s' has non-sendable type %s",
						d.Name, v.Name(), f.Name(), f.Type())
				}
			}
		}
	}

	if hasSharable {
		for _, v := range enum.Variants() {
			for _, f := range v.Fields() {
				if !isSharableType(f.Type(), make(map[types.Type]bool)) {
					c.errorf(d.Pos(), "enum %s is marked `sharable but variant %s field '%s' has non-sharable type %s",
						d.Name, v.Name(), f.Name(), f.Type())
				}
			}
		}
	}
}

// optionalClosureType reports whether typ is an Optional (possibly nested) whose
// element is a function value. Such a capture is gated out of `go { … }` blocks
// by T1653; see checkGoBlockCaptures.
func optionalClosureType(typ types.Type) bool {
	opt, ok := typ.(*types.Optional)
	if !ok {
		return false
	}
	elem := opt.Elem()
	for {
		inner, ok := elem.(*types.Optional)
		if !ok {
			break
		}
		elem = inner.Elem()
	}
	_, isSig := elem.(*types.Signature)
	return isSig
}
