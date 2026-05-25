package sema

import (
	"fmt"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// isCloneableField returns true if a field type can be cloned:
// either it's a copy type (bitwise copy) or it has a clone() method.
func isCloneableField(typ types.Type) bool {
	if typ == nil {
		return false
	}
	// Copy types are always cloneable (bitwise copy)
	if isCopyField(typ) {
		return true
	}
	switch t := typ.(type) {
	case *types.Named:
		return t.LookupMethod("clone") != nil
	case *types.Enum:
		return t.LookupMethod("clone") != nil
	case *types.Instance:
		// Check the origin type for clone method
		switch origin := t.Origin().(type) {
		case *types.Named:
			return origin.LookupMethod("clone") != nil
		case *types.Enum:
			return origin.LookupMethod("clone") != nil
		}
		return false
	case *types.Optional:
		return isCloneableField(t.Elem())
	case *types.TypeParam:
		// Generic type params — validated at instantiation
		return true
	case *types.Signature:
		// Function types cannot be cloned (closure environments)
		return false
	case *types.SharedRef, *types.MutRef:
		// References cannot be cloned
		return false
	}
	return false
}

// validateCloneType checks that all fields of a `clone type are cloneable.
// Called as a deferred pass after all types are defined (so clone() methods are registered).
func (c *Checker) validateCloneType(named *types.Named, d *ast.TypeDecl) {
	for _, f := range named.AllFields() {
		if !isCloneableField(f.Type()) {
			c.errorf(d.Pos(), "type %s is marked `clone but field '%s' has type %s which is not cloneable (must be `copy or have a clone() method)",
				d.Name, f.Name(), f.Type())
		}
	}
}

// validateCloneEnum checks that all variant fields of a `clone enum are cloneable.
func (c *Checker) validateCloneEnum(enum *types.Enum, d *ast.EnumDecl) {
	for _, v := range enum.Variants() {
		for _, f := range v.Fields() {
			if !isCloneableField(f.Type()) {
				c.errorf(d.Pos(), "enum %s is marked `clone but variant %s has field type %s which is not cloneable",
					d.Name, v.Name(), f.Type())
			}
		}
	}
}

// validateCloneTypes runs after all types are defined to validate clone field types.
// This is deferred because field types may have clone() methods defined later in the file.
func (c *Checker) validateCloneTypes(file *ast.File) {
	for _, decl := range file.Decls {
		if c.info.FilteredDecls[decl] {
			continue
		}
		switch d := decl.(type) {
		case *ast.TypeDecl:
			obj := c.scope.Lookup(d.Name)
			if obj == nil {
				continue
			}
			tn, ok := obj.(*types.TypeName)
			if !ok {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok {
				continue
			}
			if named.IsClone() {
				c.validateCloneType(named, d)
			}
		case *ast.EnumDecl:
			obj := c.scope.Lookup(d.Name)
			if obj == nil {
				continue
			}
			tn, ok := obj.(*types.TypeName)
			if !ok {
				continue
			}
			enum, ok := tn.Type().(*types.Enum)
			if !ok {
				continue
			}
			if enum.IsClone() {
				c.validateCloneEnum(enum, d)
			}
		}
	}
}

// firstSingleOwnerHandle returns the first single-owner native handle
// (Task[T], Mutex[T], MutexGuard[T]) found in typ, searching transitively
// through Instance type arguments, Optional, Tuple, and Array element types.
// Returns nil if typ contains no single-owner handle. (T0545)
//
// These handles are LLVM `i8*` native handles with no clone() method, not
// `copy, and move-only on assignment — `someTask.clone()` is already a sema
// error. A type that transitively contains one is therefore non-cloneable.
// Recursion deliberately does NOT descend into *types.Named fields: a user
// type with a handle field is already covered by validateCloneType (`clone
// types) or by "no clone() method" (plain types). *types.TypeParam → nil:
// generic bodies are checked with unbound params; concrete call sites are
// guarded by the codegen backstop.
func firstSingleOwnerHandle(typ types.Type) types.Type {
	switch t := typ.(type) {
	case *types.Instance:
		if types.IsTask(t) || types.IsMutex(t) || types.IsMutexGuard(t) {
			return t
		}
		for _, ta := range t.TypeArgs() {
			if off := firstSingleOwnerHandle(ta); off != nil {
				return off
			}
		}
	case *types.Optional:
		return firstSingleOwnerHandle(t.Elem())
	case *types.Tuple:
		for _, e := range t.Elems() {
			if off := firstSingleOwnerHandle(e); off != nil {
				return off
			}
		}
	case *types.Array:
		return firstSingleOwnerHandle(t.Elem())
	}
	return nil
}

// isNestedSingleOwnerContainer reports whether typ is itself a *container*
// (Vector / Map / Set instance, or a fixed-size Array) that transitively
// contains a single-owner handle. Such a container, used as another
// container's element/key/value, forces the outer container's
// literal-lowering / push-dup / clone / realloc paths to duplicate the inner
// handle — unsound (double-free at drop). A *direct* handle element
// (Vector[Task[T]]) is fine (T0508 move-only collection), and an
// Optional/Tuple wrapping a handle is NOT a container and has its own drop
// handling (T0558), so neither triggers the nesting rule. (T0545)
func isNestedSingleOwnerContainer(typ types.Type) bool {
	switch t := typ.(type) {
	case *types.Array:
		return firstSingleOwnerHandle(t) != nil
	case *types.Instance:
		if singleOwnerContainerElemTypes(t.Origin(), t.TypeArgs()) != nil {
			return firstSingleOwnerHandle(t) != nil
		}
	}
	return false
}

// checkContainerNotCloneable reports an error if any of the supplied container
// element/key/value types transitively contains a single-owner handle, which
// makes the container non-cloneable / non-fillable. opName is the verb used in
// the message ("cloned" or "filled"). Returns true if an error was emitted.
// (T0545)
func (c *Checker) checkContainerNotCloneable(pos ast.Pos, containerType types.Type, elemTypes []types.Type, opName string) bool {
	for _, et := range elemTypes {
		if off := firstSingleOwnerHandle(et); off != nil {
			c.errorf(pos, "%s cannot be %s: it contains %s, a single-owner handle with no clone() semantics (single-owner handles are move-only)",
				containerType, opName, off)
			return true
		}
	}
	return false
}

// reportContainerSingleOwnerNesting reports an error if elemType is itself a
// container (Vector/Map/Set/Array) that transitively contains a single-owner
// handle. A *direct* handle element is permitted (T0508 move-only
// collections), and Optional/Tuple wrapping a handle is handled separately
// (T0558) — only a nested *container* forces an unsound duplicate. (T0545)
func (c *Checker) reportContainerSingleOwnerNesting(pos ast.Pos, elemType types.Type) {
	if elemType == nil || !isNestedSingleOwnerContainer(elemType) {
		return
	}
	off := firstSingleOwnerHandle(elemType)
	c.errorf(pos, "%s cannot be a container element: it transitively contains %s, a single-owner handle (single-owner handles may only appear as direct container elements, not nested inside another container)",
		elemType, off)
}

// singleOwnerContainerElemTypes returns the element/key/value types that an
// outer container would have to duplicate, or nil if origin is not a
// duplicating container (Vector / Map / Set). (T0545)
func singleOwnerContainerElemTypes(origin types.Type, typeArgs []types.Type) []types.Type {
	n, ok := origin.(*types.Named)
	if !ok {
		return nil
	}
	if n == types.TypVector || n == types.TypMap {
		return typeArgs
	}
	if obj := n.Obj(); obj != nil && obj.Name() == "Set" {
		return typeArgs
	}
	return nil
}

// validateSingleOwnerContainerInstance enforces the nesting rule for an
// explicitly written or inferred container instance (e.g. Vector[Vector[Task]],
// Map[K, Vector[Task]]). Called alongside validateSendableInstance. (T0545)
//
// T0616: when checking inside a generic body and the nested container's element
// references a TypeParam, defer the check to the call site via recordCloneReq
// so generic indirection (`outer[T] { Vector[Vector[T]] v; }` instantiated with
// T = Task[int]) doesn't slip past the direct nesting gate.
func (c *Checker) validateSingleOwnerContainerInstance(pos ast.Pos, origin types.Type, typeArgs []types.Type) {
	for _, et := range singleOwnerContainerElemTypes(origin, typeArgs) {
		c.reportContainerSingleOwnerNesting(pos, et)
		if (c.curFuncObj != nil || c.curMethodObj != nil) &&
			isContainerWithTypeParam(et) {
			c.recordCloneReq(et, pos, "nested container element")
		}
	}
}

// isContainerWithTypeParam reports whether typ is itself a *container*
// (Vector/Map/Set instance or Array) whose element/key/value type expression
// references a TypeParam — meaning substitution at the call site could expose
// a single-owner handle. (T0616)
func isContainerWithTypeParam(typ types.Type) bool {
	switch t := typ.(type) {
	case *types.Array:
		return types.ContainsTypeParam(t.Elem())
	case *types.Instance:
		if singleOwnerContainerElemTypes(t.Origin(), t.TypeArgs()) != nil {
			for _, ta := range t.TypeArgs() {
				if types.ContainsTypeParam(ta) {
					return true
				}
			}
		}
	}
	return false
}

// recordCloneReq appends a cloneability requirement to the current generic
// function or method being checked. No-op when not inside a generic body.
// The requirement is validated when the enclosing function/method is called
// with concrete type arguments (T0616).
func (c *Checker) recordCloneReq(typeExpr types.Type, pos ast.Pos, opDesc string) {
	if typeExpr == nil {
		return
	}
	req := CloneabilityRequirement{TypeExpr: typeExpr, Pos: pos, OpDesc: opDesc}
	if c.curMethodObj != nil {
		for _, r := range c.info.MethodCloneReqs[c.curMethodObj] {
			if r.OpDesc == opDesc && r.Pos == pos && types.Identical(r.TypeExpr, typeExpr) {
				return
			}
		}
		c.info.MethodCloneReqs[c.curMethodObj] = append(c.info.MethodCloneReqs[c.curMethodObj], req)
		return
	}
	if c.curFuncObj != nil {
		for _, r := range c.info.FuncCloneReqs[c.curFuncObj] {
			if r.OpDesc == opDesc && r.Pos == pos && types.Identical(r.TypeExpr, typeExpr) {
				return
			}
		}
		c.info.FuncCloneReqs[c.curFuncObj] = append(c.info.FuncCloneReqs[c.curFuncObj], req)
	}
}

// propagateCloneReqs propagates cloneability requirements transitively across
// generic call edges. When generic `f[T]` calls generic `g[T]` (or `g[h(T)]`)
// in its body, any requirement R on g must also become a requirement on f
// (after substituting g's TypeParams via the call's subst map) so that the
// eventual concrete call site for f catches single-owner-handle violations
// that arise from g's internal use.
//
// Iterates to a fixed point — when adding a requirement to f grows f's
// requirement set, callers of f need a fresh pass too. Cycles terminate
// because new requirements are deduped by (TypeExpr, OpDesc, Pos). Concrete
// substitutions that expose a single-owner handle emit one error per
// (CallPos, OpDesc, substituted-type) triple — deduped via emitted-set to
// avoid double errors when the same edge fires across multiple iterations
// (T0616).
func (c *Checker) propagateCloneReqs() {
	if len(c.info.GenericCallEdges) == 0 {
		return
	}
	emitted := make(map[string]bool)
	for iter := 0; iter < 64; iter++ {
		changed := false
		for _, edge := range c.info.GenericCallEdges {
			var calleeReqs []CloneabilityRequirement
			if edge.CalleeFunc != nil {
				calleeReqs = c.info.FuncCloneReqs[edge.CalleeFunc]
			} else if edge.CalleeMethod != nil {
				calleeReqs = c.info.MethodCloneReqs[edge.CalleeMethod]
			}
			if len(calleeReqs) == 0 {
				continue
			}
			for _, req := range calleeReqs {
				substituted := types.Substitute(req.TypeExpr, edge.Subst)
				if !types.ContainsTypeParam(substituted) {
					if off := firstSingleOwnerHandle(substituted); off != nil {
						key := edge.CallPos.String() + "|" + req.OpDesc + "|" + substituted.String()
						if !emitted[key] {
							emitted[key] = true
							c.errorf(edge.CallPos,
								"cannot instantiate generic with %s: %s is a single-owner handle, but %s (at %s) would duplicate it (single-owner handles are move-only)",
								substituted, off, req.OpDesc, req.Pos)
						}
					}
					continue
				}
				if c.addCloneReq(edge.CallerFunc, edge.CallerMethod,
					CloneabilityRequirement{
						TypeExpr: substituted,
						Pos:      req.Pos,
						OpDesc:   req.OpDesc,
					}) {
					changed = true
				}
			}
		}
		if !changed {
			return
		}
	}
}

// addCloneReq appends req to the caller's requirement set if not already
// present (dedup on TypeExpr/OpDesc/Pos). Returns true if a new requirement
// was added. (T0616)
func (c *Checker) addCloneReq(fn *types.Func, method *types.Method, req CloneabilityRequirement) bool {
	if fn != nil {
		for _, existing := range c.info.FuncCloneReqs[fn] {
			if existing.OpDesc == req.OpDesc && existing.Pos == req.Pos &&
				types.Identical(existing.TypeExpr, req.TypeExpr) {
				return false
			}
		}
		c.info.FuncCloneReqs[fn] = append(c.info.FuncCloneReqs[fn], req)
		return true
	}
	if method != nil {
		for _, existing := range c.info.MethodCloneReqs[method] {
			if existing.OpDesc == req.OpDesc && existing.Pos == req.Pos &&
				types.Identical(existing.TypeExpr, req.TypeExpr) {
				return false
			}
		}
		c.info.MethodCloneReqs[method] = append(c.info.MethodCloneReqs[method], req)
		return true
	}
	return false
}

// typeToTypeRef converts a types.Type to an ast.TypeRef for use in synthesized AST.
// Handles common cases needed for clone method synthesis.
func typeToTypeRef(typ types.Type) ast.TypeRef {
	switch t := typ.(type) {
	case *types.Named:
		return &ast.NamedTypeRef{Name: t.Obj().Name()}
	case *types.Enum:
		return &ast.NamedTypeRef{Name: t.Obj().Name()}
	case *types.Optional:
		return &ast.OptionalTypeRef{Inner: typeToTypeRef(t.Elem())}
	case *types.Instance:
		var typeArgs []ast.TypeRef
		for _, ta := range t.TypeArgs() {
			typeArgs = append(typeArgs, typeToTypeRef(ta))
		}
		switch origin := t.Origin().(type) {
		case *types.Named:
			return &ast.NamedTypeRef{Name: origin.Obj().Name(), TypeArgs: typeArgs}
		case *types.Enum:
			return &ast.NamedTypeRef{Name: origin.Obj().Name(), TypeArgs: typeArgs}
		}
		return &ast.NamedTypeRef{Name: "any"}
	case *types.TypeParam:
		return &ast.NamedTypeRef{Name: t.Obj().Name()}
	default:
		return &ast.NamedTypeRef{Name: typ.String()}
	}
}

// synthesizeCloneMethod builds an AST MethodDecl for the clone() Self method.
// The method body constructs a new instance by passing each field through:
// - Copy fields: passed directly (constructor handles bitwise copy)
// - Non-copy fields with clone(): this.field.clone()
// - Optional non-copy fields: typed var + if-let unwrap + clone + reassign
func (c *Checker) synthesizeCloneMethod(named *types.Named, _ *ast.TypeDecl) *ast.MethodDecl {
	var stmts []ast.Stmt
	var args []*ast.Arg

	fields := named.AllFields()
	for _, f := range fields {
		fieldType := f.Type()

		// T0605: fields whose declared type contains a TypeParam cannot be
		// classified copy/non-copy at synth time (isCopyField(TypeParam) is
		// optimistically true, which would emit a bare shallow read and alias
		// the heap value → double-free at mono codegen). Defer the decision to
		// codegen via the synth-only AutoCloneExpr intrinsic, which lowers
		// type-directed once the concrete substitution is known. Concrete
		// fields keep their exact existing behavior (zero regression surface).
		if types.ContainsTypeParam(fieldType) {
			args = append(args, &ast.Arg{
				Name:  f.Name(),
				Value: &ast.AutoCloneExpr{Expr: memberExpr(&ast.ThisExpr{}, f.Name())},
			})
			continue
		}

		// Check if the field type is Optional wrapping a non-copy type
		if opt, isOpt := fieldType.(*types.Optional); isOpt && !isCopyField(opt.Elem()) {
			// Generate:
			//   T? _clone_fieldname = none;
			//   if _v := this.fieldname { _clone_fieldname = _v.clone(); }
			// Then pass _clone_fieldname in the constructor args.
			localName := "_clone_" + f.Name()

			// T? _clone_fieldname = none;
			stmts = append(stmts, &ast.TypedVarDecl{
				Type:  typeToTypeRef(opt),
				Name:  localName,
				Value: &ast.NoneLit{},
			})

			// if _v := this.fieldname { _clone_fieldname = _v.clone(); }
			stmts = append(stmts, &ast.IfStmt{
				Binding: "_v",
				Init:    memberExpr(&ast.ThisExpr{}, f.Name()),
				Body: &ast.Block{
					Stmts: []ast.Stmt{
						&ast.AssignStmt{
							Target: ident(localName),
							Op:     ast.OpAssign,
							Value:  callMember(ident("_v"), "clone"),
						},
					},
				},
			})

			args = append(args, &ast.Arg{
				Name:  f.Name(),
				Value: ident(localName),
			})
			continue
		}

		// For copy fields: pass this.field directly
		if isCopyField(fieldType) {
			args = append(args, &ast.Arg{
				Name:  f.Name(),
				Value: memberExpr(&ast.ThisExpr{}, f.Name()),
			})
			continue
		}

		// For non-copy fields with clone(): this.field.clone()
		args = append(args, &ast.Arg{
			Name:  f.Name(),
			Value: callMember(memberExpr(&ast.ThisExpr{}, f.Name()), "clone"),
		})
	}

	// return Self(field1: ..., field2: ..., ...);
	// Use "Self" instead of d.Name so generic types resolve correctly (e.g., Box[T] → Self).
	stmts = append(stmts, &ast.ReturnStmt{
		Value: &ast.CallExpr{
			Callee: ident("Self"),
			Args:   args,
		},
	})

	return &ast.MethodDecl{
		Name:       "clone",
		Receiver:   &ast.ReceiverParam{RefMod: ast.RefNone},
		ReturnType: &ast.ReturnTypeSpec{Type: &ast.NamedTypeRef{Name: "Self"}},
		Annotations: []*ast.MetaAnnotation{
			{Name: "public"},
		},
		Body: &ast.Block{Stmts: stmts},
	}
}

// synthesizeEnumCloneMethod builds an AST MethodDecl for clone() EnumName on an enum.
// Self doesn't resolve in enum method context, so the return type uses the concrete name.
// Generates a match over all variants, cloning each variant's fields:
//
//	clone() EnumName `public {
//	    match this {
//	        EnumName.Variant1 => { return EnumName.Variant1; },
//	        EnumName.Variant2(a, b) => {
//	            T _c_a = a.clone(); // B0278: explicit local to avoid codegen crash
//	            return EnumName.Variant2(a: _c_a, b: b);
//	        },
//	    }
//	}
func (c *Checker) synthesizeEnumCloneMethod(enum *types.Enum, d *ast.EnumDecl) *ast.MethodDecl {
	enumName := d.Name

	var arms []*ast.MatchArm
	for _, v := range enum.Variants() {
		if v.NumFields() == 0 {
			// Fieldless: Enum.Variant => { return Enum.Variant; }
			arms = append(arms, &ast.MatchArm{
				Pattern: &ast.EnumVariantMatchPattern{Enum: enumName, Variant: v.Name()},
				Block: &ast.Block{Stmts: []ast.Stmt{
					&ast.ReturnStmt{Value: memberExpr(ident(enumName), v.Name())},
				}},
			})
			continue
		}

		// Variant with fields — destructure and clone each field.
		var stmts []ast.Stmt
		bindings := make([]string, v.NumFields())
		var args []*ast.Arg

		for i, f := range v.Fields() {
			bindName := "_v_" + f.Name()
			if f.Name() == "" {
				bindName = fmt.Sprintf("_v_%d", i)
			}
			bindings[i] = bindName
			fieldType := f.Type()

			// Optional non-copy: if-let unwrap + clone
			if opt, isOpt := fieldType.(*types.Optional); isOpt && !isCopyField(opt.Elem()) {
				localName := "_clone_" + bindName
				stmts = append(stmts, &ast.TypedVarDecl{
					Type:  typeToTypeRef(opt),
					Name:  localName,
					Value: &ast.NoneLit{},
				})
				stmts = append(stmts, &ast.IfStmt{
					Binding: "_u",
					Init:    ident(bindName),
					Body: &ast.Block{
						Stmts: []ast.Stmt{
							&ast.AssignStmt{
								Target: ident(localName),
								Op:     ast.OpAssign,
								Value:  callMember(ident("_u"), "clone"),
							},
						},
					},
				})
				args = append(args, &ast.Arg{Name: f.Name(), Value: ident(localName)})
				continue
			}

			// Copy: pass directly
			if isCopyField(fieldType) {
				args = append(args, &ast.Arg{Name: f.Name(), Value: ident(bindName)})
				continue
			}

			// Non-copy: clone into local var (B0278: inline method call in enum ctor
			// arg inside match arm block causes segfault, so use explicit local).
			localName := "_c_" + bindName
			stmts = append(stmts, &ast.TypedVarDecl{
				Type:  typeToTypeRef(fieldType),
				Name:  localName,
				Value: callMember(ident(bindName), "clone"),
			})
			args = append(args, &ast.Arg{Name: f.Name(), Value: ident(localName)})
		}

		stmts = append(stmts, &ast.ReturnStmt{
			Value: &ast.CallExpr{
				Callee: memberExpr(ident(enumName), v.Name()),
				Args:   args,
			},
		})

		arms = append(arms, &ast.MatchArm{
			Pattern: &ast.EnumDestructureMatchPattern{Enum: enumName, Variant: v.Name(), Bindings: bindings},
			Block:   &ast.Block{Stmts: stmts},
		})
	}

	body := &ast.Block{Stmts: []ast.Stmt{
		makeExprStmt(&ast.MatchExpr{
			Subject: &ast.ThisExpr{},
			Arms:    arms,
		}),
	}}

	// Build return type: EnumName (or EnumName[T, U] for generic enums).
	// Self doesn't resolve in enum method context, so use the concrete name.
	var retType ast.TypeRef
	if len(enum.TypeParams()) > 0 {
		var typeArgs []ast.TypeRef
		for _, tp := range enum.TypeParams() {
			typeArgs = append(typeArgs, &ast.NamedTypeRef{Name: tp.Obj().Name()})
		}
		retType = &ast.NamedTypeRef{Name: enumName, TypeArgs: typeArgs}
	} else {
		retType = &ast.NamedTypeRef{Name: enumName}
	}

	return &ast.MethodDecl{
		Name:       "clone",
		Receiver:   &ast.ReceiverParam{RefMod: ast.RefNone},
		ReturnType: &ast.ReturnTypeSpec{Type: retType},
		Annotations: []*ast.MetaAnnotation{
			{Name: "public"},
		},
		Body: body,
	}
}
