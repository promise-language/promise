package sema

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// isSendableType reports whether a type's values may be moved across goroutine
// boundaries. Primitives, value types with all-sendable fields, channels, arcs,
// and tasks are sendable. Closures (Signature) are not.
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
		// Channel, Arc, Task are inherently sendable (internal synchronization)
		if origin == types.TypChannel || origin == types.TypArc || origin == types.TypTask {
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
		return false // closures capture env, not generally sendable
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
		// Channel, Arc are inherently sharable (internal synchronization)
		if origin == types.TypChannel || origin == types.TypArc {
			return true
		}
		// Task handles are sharable (read-only handle)
		if origin == types.TypTask {
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
		return false
	case *types.TypeParam:
		return true
	}
	return false
}

// checkGoBlockSendable walks a go block's AST and checks that all captured
// variables (those defined in an enclosing scope) have sendable types.
func (c *Checker) checkGoBlockSendable(e *ast.GoExpr) {
	if e.Block == nil {
		return
	}
	goScope := c.info.Scopes[e.Block]
	if goScope == nil {
		return
	}

	// Walk all identifiers in the block and check cross-scope references
	seen := make(map[string]bool)
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
			if !ok || seen[v.Name()] {
				return
			}
			// Check if the variable is from an enclosing scope (not within the go block)
			if !scopeContains(goScope, v.Pos()) {
				seen[v.Name()] = true
				typ := v.Type()
				if typ != nil && !isSendableType(typ, make(map[types.Type]bool)) {
					c.errorf(ex.Pos(), "cannot send non-sendable variable '%s' of type %s across goroutine boundary", v.Name(), typ)
				}
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
			// Lambda bodies are their own scope — don't traverse into them
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
func scopeContains(s *types.Scope, pos types.Pos) bool {
	if s == nil {
		return false
	}
	start := s.Pos()
	end := s.End()
	if pos.Line > start.Line && pos.Line < end.Line {
		return true
	}
	if pos.Line == start.Line && pos.Column >= start.Column {
		return true
	}
	if pos.Line == end.Line && pos.Column <= end.Column {
		return true
	}
	return false
}

// validateSendableInstance checks sendable/sharable constraints on generic type
// instantiation. Channel[T] requires T to be sendable. Arc[T] requires T to be
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
		if !isSendableType(elemType, make(map[types.Type]bool)) {
			c.errorf(pos, "Arc element type %s is not sendable", elemType)
		}
		if !isSharableType(elemType, make(map[types.Type]bool)) {
			c.errorf(pos, "Arc element type %s is not sharable", elemType)
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

	// Contradictory tags
	if hasSendable && hasNotSendable {
		c.errorf(d.Pos(), "type %s has contradictory `sendable and `not_sendable annotations", d.Name)
	}
	if hasSharable && hasNotSharable {
		c.errorf(d.Pos(), "type %s has contradictory `sharable and `not_sharable annotations", d.Name)
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

	if hasSendable && hasNotSendable {
		c.errorf(d.Pos(), "enum %s has contradictory `sendable and `not_sendable annotations", d.Name)
	}
	if hasSharable && hasNotSharable {
		c.errorf(d.Pos(), "enum %s has contradictory `sharable and `not_sharable annotations", d.Name)
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
