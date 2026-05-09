package sema

import (
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
