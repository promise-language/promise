package sema

import (
	"fmt"
	"strconv"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// processSerializableType handles `serializable on a type declaration.
// Validates field-level annotations and synthesizes encode/decode methods
// if not user-defined.
func (c *Checker) processSerializableType(named *types.Named, d *ast.TypeDecl) {
	named.SetSerializable(true)

	// Validate field-level serialization annotations.
	// Guard: only validate if field counts match (they may not if earlier errors occurred).
	if len(d.Fields) == len(named.Fields()) {
		for i, fd := range d.Fields {
			f := named.Fields()[i]
			if f.IncludeNone() {
				if _, ok := f.Type().(*types.Optional); !ok {
					c.errorf(fd.Pos(), "`include_none is only valid on optional (T?) fields")
				}
			}
			if f.Flatten() {
				c.validateFlattenField(f, fd, named)
			}
		}
	}

	// Validate: generic type parameters used in serialized fields must have
	// Encodable + Decodable constraints. Skip fields are exempt.
	hasConstraintErrors := false
	if len(d.Fields) == len(named.Fields()) {
		for i, fd := range d.Fields {
			f := named.Fields()[i]
			if f.Skip() {
				continue
			}
			if !c.validateSerializableFieldType(f.Type(), fd, d.Name) {
				hasConstraintErrors = true
			}
		}
	}

	// Don't synthesize methods if constraint validation failed — the methods
	// would produce confusing follow-on errors.
	if hasConstraintErrors {
		return
	}

	// Synthesize encode method if not user-defined.
	if lookupOwnMethod(named, "encode") == nil {
		md := c.synthesizeEncodeMethod(named, d)
		d.Methods = append(d.Methods, md)
		c.defineMethod(named, md, d.Name)
	}

	// Synthesize decode factory method if not user-defined.
	// Skip synthesis if any skip field has a type with no zero value (e.g., TypeParam) —
	// the constructor can't be called without a value for that field.
	canDecode := true
	for _, f := range named.Fields() {
		if f.Skip() && c.zeroValueExpr(f.Type()) == nil {
			canDecode = false
			break
		}
	}
	if canDecode && lookupOwnMethod(named, "decode") == nil {
		md := c.synthesizeDecodeMethod(named, d)
		d.Methods = append(d.Methods, md)
		c.defineMethod(named, md, d.Name)
	}
}

// validateFlattenField checks that a `flatten field meets all requirements:
// - Type must be a named type (not optional, vector, map, primitive)
// - Cannot combine with `key, `include_none
// - No wire name collisions with sibling fields
func (c *Checker) validateFlattenField(f *types.Field, fd *ast.FieldDecl, parent *types.Named) {
	// Cannot combine with other serialization annotations
	if f.KeyName() != "" {
		c.errorf(fd.Pos(), "`flatten and `key cannot be combined on field '%s'", f.Name())
	}
	if f.IncludeNone() {
		c.errorf(fd.Pos(), "`flatten and `include_none cannot be combined on field '%s'", f.Name())
	}

	// Type must be a named type (not optional, vector, map)
	inner := flattenedNamed(f.Type())
	if inner == nil {
		c.errorf(fd.Pos(), "`flatten field '%s' must have a named type, got %s", f.Name(), f.Type())
		return
	}

	// Check for wire name collisions between flattened sub-fields and sibling fields
	wireNames := map[string]string{} // wire name → source description
	for _, sibling := range parent.Fields() {
		if sibling.Skip() {
			continue
		}
		if sibling == f {
			continue // skip the flatten field itself
		}
		if sibling.Flatten() {
			// Another flatten field — collect its sub-field wire names
			if sibInner := flattenedNamed(sibling.Type()); sibInner != nil {
				for _, sf := range sibInner.Fields() {
					if sf.Skip() {
						continue
					}
					wn := sf.Name()
					if sf.KeyName() != "" {
						wn = sf.KeyName()
					}
					wireNames[wn] = sibling.Name() + "." + sf.Name()
				}
			}
		} else {
			wn := sibling.Name()
			if sibling.KeyName() != "" {
				wn = sibling.KeyName()
			}
			wireNames[wn] = sibling.Name()
		}
	}
	// Now check the current flatten field's sub-fields for collisions
	for _, sf := range inner.Fields() {
		if sf.Skip() {
			continue
		}
		wn := sf.Name()
		if sf.KeyName() != "" {
			wn = sf.KeyName()
		}
		if other, exists := wireNames[wn]; exists {
			c.errorf(fd.Pos(), "`flatten field '%s': wire name '%s' (from %s.%s) conflicts with %s",
				f.Name(), wn, f.Name(), sf.Name(), other)
		}
	}
}

// flattenedNamed extracts the *types.Named from a type suitable for flattening.
// Returns nil if the type cannot be flattened (optional, vector, map, primitive, etc.).
// Only returns user-defined types that have fields — not primitives.
func flattenedNamed(typ types.Type) *types.Named {
	if named, ok := typ.(*types.Named); ok && named.NumFields() > 0 {
		return named
	}
	return nil
}

// encodeFieldStmts generates the encode statements for a single field.
// access is the expression to access the field value (e.g., this.name or this.data.name).
// suffix is used to generate unique temporary variable names.
func (c *Checker) encodeFieldStmts(access ast.Expr, f *types.Field, fieldType types.Type, wireName, suffix string) []ast.Stmt {
	var stmts []ast.Stmt
	_, isOptional := fieldType.(*types.Optional)

	if isOptional && !f.IncludeNone() {
		valName := "_enc_" + suffix
		stmts = append(stmts, &ast.IfStmt{
			Binding: valName,
			Init:    access,
			Body: &ast.Block{Stmts: []ast.Stmt{
				makeExprStmt(callMember(ident("e"), "encode_key", strLit(wireName))),
				makeExprStmt(callMember(ident(valName), "encode", ident("e"))),
			}},
		})
	} else if isOptional && f.IncludeNone() {
		valName := "_enc_" + suffix
		stmts = append(stmts, makeExprStmt(callMember(ident("e"), "encode_key", strLit(wireName))))
		stmts = append(stmts, &ast.IfStmt{
			Binding: valName,
			Init:    access,
			Body: &ast.Block{Stmts: []ast.Stmt{
				makeExprStmt(callMember(ident(valName), "encode", ident("e"))),
			}},
			Else: &ast.Block{Stmts: []ast.Stmt{
				makeExprStmt(callMember(ident("e"), "encode_none")),
			}},
		})
	} else if elemType := vectorElemType(fieldType); elemType != nil {
		iterName := "_arr_" + suffix
		stmts = append(stmts, makeExprStmt(callMember(ident("e"), "encode_key", strLit(wireName))))
		stmts = append(stmts, makeExprStmt(callMember(ident("e"), "begin_array",
			memberExpr(access, "len"))))
		stmts = append(stmts, &ast.ForInStmt{
			Binding:  iterName,
			Iterable: access,
			Body: &ast.Block{Stmts: []ast.Stmt{
				makeExprStmt(callMember(ident(iterName), "encode", ident("e"))),
			}},
		})
		stmts = append(stmts, makeExprStmt(callMember(ident("e"), "end_array")))
	} else if keyType, valType := mapKeyValueTypes(fieldType); valType != nil {
		_ = valType
		mkName := "_mk_" + suffix
		mvName := "_mv_" + suffix
		var encodeKeyExpr ast.Expr
		if keyType == types.TypString {
			encodeKeyExpr = ident(mkName)
		} else {
			encodeKeyExpr = callMember(ident(mkName), "to_string")
		}
		stmts = append(stmts, makeExprStmt(callMember(ident("e"), "encode_key", strLit(wireName))))
		stmts = append(stmts, makeExprStmt(callMember(ident("e"), "begin_object", intLit(0))))
		stmts = append(stmts, &ast.ForInStmt{
			Binding:  mvName,
			Index:    mkName,
			Iterable: access,
			Body: &ast.Block{Stmts: []ast.Stmt{
				makeExprStmt(callMember(ident("e"), "encode_key", encodeKeyExpr)),
				makeExprStmt(callMember(ident(mvName), "encode", ident("e"))),
			}},
		})
		stmts = append(stmts, makeExprStmt(callMember(ident("e"), "end_object")))
	} else {
		stmts = append(stmts, makeExprStmt(callMember(ident("e"), "encode_key", strLit(wireName))))
		stmts = append(stmts, makeExprStmt(callMember(access, "encode", ident("e"))))
	}
	return stmts
}

// synthesizeEncodeMethod builds an AST MethodDecl for:
//
//	encode(Encoder ~e)! `public {
//	  e.begin_object(0);
//	  for each field: e.encode_key("key"); this.field.encode(e);
//	  e.end_object();
//	}
func (c *Checker) synthesizeEncodeMethod(named *types.Named, d *ast.TypeDecl) *ast.MethodDecl {
	var stmts []ast.Stmt

	// Pass 0 to begin_object — JSON doesn't use the count, and optional fields
	// make the actual count unknowable at compile time.
	stmts = append(stmts, makeExprStmt(callMember(ident("e"), "begin_object", intLit(0))))

	for _, f := range named.Fields() {
		if f.Skip() {
			continue
		}

		if f.Flatten() {
			// Inline the nested type's fields directly into the parent object.
			inner := flattenedNamed(f.Type())
			if inner == nil {
				continue
			}
			for _, sf := range inner.Fields() {
				if sf.Skip() {
					continue
				}
				sfWireName := sf.Name()
				if sf.KeyName() != "" {
					sfWireName = sf.KeyName()
				}
				access := memberExpr(memberExpr(&ast.ThisExpr{}, f.Name()), sf.Name())
				suffix := f.Name() + "_" + sf.Name()
				stmts = append(stmts, c.encodeFieldStmts(access, sf, sf.Type(), sfWireName, suffix)...)
			}
			continue
		}

		wireName := f.Name()
		if f.KeyName() != "" {
			wireName = f.KeyName()
		}
		access := memberExpr(&ast.ThisExpr{}, f.Name())
		stmts = append(stmts, c.encodeFieldStmts(access, f, f.Type(), wireName, f.Name())...)
	}

	stmts = append(stmts, makeExprStmt(callMember(ident("e"), "end_object")))

	return &ast.MethodDecl{
		Name:        "encode",
		Receiver:    &ast.ReceiverParam{RefMod: ast.RefNone},
		Params:      []*ast.Param{{Type: &ast.MutRefTypeRef{Inner: &ast.NamedTypeRef{Name: "Encoder"}}, Name: "e"}},
		ReturnType:  &ast.ReturnTypeSpec{CanError: true},
		Annotations: []*ast.MetaAnnotation{{Name: "public"}},
		Body:        &ast.Block{Stmts: stmts},
	}
}

// decodeField represents a field in the key-match chain during decode synthesis.
// It may be a regular field or a sub-field of a flattened parent.
type decodeField struct {
	wireName  string
	localName string
	field     *types.Field // the actual field (for type info and annotations)
}

// synthesizeDecodeMethod builds an AST MethodDecl for the decode factory.
func (c *Checker) synthesizeDecodeMethod(named *types.Named, d *ast.TypeDecl) *ast.MethodDecl {
	var stmts []ast.Stmt

	// d.begin_object()
	stmts = append(stmts, makeExprStmt(callMember(ident("d"), "begin_object")))

	// Declare local variables for each non-skip field.
	// For flatten fields, declare locals for each sub-field instead.
	var matchFields []decodeField
	for _, f := range named.Fields() {
		if f.Skip() {
			continue
		}
		if f.Flatten() {
			inner := flattenedNamed(f.Type())
			if inner == nil {
				continue
			}
			for _, sf := range inner.Fields() {
				if sf.Skip() {
					continue
				}
				localName := "_ff_" + f.Name() + "_" + sf.Name()
				stmts = append(stmts, c.makeFieldLocalDecl(localName, sf))
				sfWireName := sf.Name()
				if sf.KeyName() != "" {
					sfWireName = sf.KeyName()
				}
				matchFields = append(matchFields, decodeField{
					wireName:  sfWireName,
					localName: localName,
					field:     sf,
				})
			}
			continue
		}
		localName := "_f_" + f.Name()
		stmts = append(stmts, c.makeFieldLocalDecl(localName, f))
		wireName := f.Name()
		if f.KeyName() != "" {
			wireName = f.KeyName()
		}
		matchFields = append(matchFields, decodeField{
			wireName:  wireName,
			localName: localName,
			field:     f,
		})
	}

	// Key matching loop
	var loopStmts []ast.Stmt

	loopStmts = append(loopStmts, &ast.TypedVarDecl{
		Type:  &ast.OptionalTypeRef{Inner: &ast.NamedTypeRef{Name: "string"}},
		Name:  "_k",
		Value: callMember(ident("d"), "next_key"),
	})
	loopStmts = append(loopStmts, &ast.IfStmt{
		Cond: &ast.IsExpr{Expr: ident("_k"), Pattern: &ast.IdentIsPattern{Name: "absent"}},
		Body: &ast.Block{Stmts: []ast.Stmt{&ast.BreakStmt{}}},
	})
	loopStmts = append(loopStmts, &ast.TypedVarDecl{
		Type:  &ast.NamedTypeRef{Name: "string"},
		Name:  "_key",
		Value: &ast.BinaryExpr{Left: ident("_k"), Op: ast.BinElvis, Right: strLit("")},
	})

	if len(matchFields) > 0 {
		loopStmts = append(loopStmts, c.buildDecodeFieldMatchChain(matchFields))
	}

	stmts = append(stmts, &ast.InfiniteLoop{Body: &ast.Block{Stmts: loopStmts}})

	// d.end_object()
	stmts = append(stmts, makeExprStmt(callMember(ident("d"), "end_object")))

	// return Self(field1: _f_field1, ...);
	// Skip fields get zero values. Flatten fields are constructed from sub-field locals.
	var args []*ast.Arg
	for _, f := range named.Fields() {
		if f.Skip() {
			zv := c.zeroValueExpr(f.Type())
			if zv == nil {
				zv = &ast.NoneLit{}
			}
			args = append(args, &ast.Arg{Name: f.Name(), Value: zv})
			continue
		}
		if f.Flatten() {
			// Construct the nested type from its sub-field locals.
			inner := flattenedNamed(f.Type())
			if inner == nil {
				continue
			}
			var innerArgs []*ast.Arg
			for _, sf := range inner.Fields() {
				if sf.Skip() {
					zv := c.zeroValueExpr(sf.Type())
					if zv == nil {
						zv = &ast.NoneLit{}
					}
					innerArgs = append(innerArgs, &ast.Arg{Name: sf.Name(), Value: zv})
					continue
				}
				localName := "_ff_" + f.Name() + "_" + sf.Name()
				localExpr := ident(localName)
				if c.fieldNeedsUnwrap(sf) {
					innerArgs = append(innerArgs, &ast.Arg{Name: sf.Name(), Value: &ast.ErrorUnwrapExpr{Expr: localExpr}})
				} else {
					innerArgs = append(innerArgs, &ast.Arg{Name: sf.Name(), Value: localExpr})
				}
			}
			args = append(args, &ast.Arg{
				Name:  f.Name(),
				Value: &ast.CallExpr{Callee: ident(inner.Obj().Name()), Args: innerArgs},
			})
			continue
		}
		localExpr := ident("_f_" + f.Name())
		if c.fieldNeedsUnwrap(f) {
			args = append(args, &ast.Arg{Name: f.Name(), Value: &ast.ErrorUnwrapExpr{Expr: localExpr}})
		} else {
			args = append(args, &ast.Arg{Name: f.Name(), Value: localExpr})
		}
	}
	stmts = append(stmts, &ast.ReturnStmt{
		Value: &ast.CallExpr{Callee: ident("Self"), Args: args},
	})

	return &ast.MethodDecl{
		Name:   "decode",
		Params: []*ast.Param{{Type: &ast.MutRefTypeRef{Inner: &ast.NamedTypeRef{Name: "Decoder"}}, Name: "d"}},
		ReturnType: &ast.ReturnTypeSpec{
			Type:     &ast.NamedTypeRef{Name: "Self"},
			CanError: true,
		},
		Annotations: []*ast.MetaAnnotation{{Name: "factory"}, {Name: "public"}},
		Body:        &ast.Block{Stmts: stmts},
	}
}

// makeFieldLocalDecl creates a typed variable declaration with a zero/none value.
// For types without a known zero value (user-defined types), uses T? with none
// and unwraps at the constructor call site.
func (c *Checker) makeFieldLocalDecl(localName string, f *types.Field) ast.Stmt {
	if _, ok := f.Type().(*types.Optional); ok {
		return &ast.TypedVarDecl{
			Type: c.typeToTypeRef(f.Type()), Name: localName, Value: &ast.NoneLit{},
		}
	}
	zv := c.zeroValueExpr(f.Type())
	if zv != nil {
		return &ast.TypedVarDecl{
			Type: c.typeToTypeRef(f.Type()), Name: localName, Value: zv,
		}
	}
	// No known zero value (user-defined type). Call TYPE.decode(d) eagerly
	// in the key-match body, requiring the field to be present in the JSON.
	// Use T? with none as placeholder — will be unwrapped via ?: in the constructor.
	return &ast.TypedVarDecl{
		Type: &ast.OptionalTypeRef{Inner: c.typeToTypeRef(f.Type())}, Name: localName, Value: &ast.NoneLit{},
	}
}

// fieldNeedsUnwrap returns true if the field's local variable uses T? wrapping
// because the type has no known zero value (user-defined types).
func (c *Checker) fieldNeedsUnwrap(f *types.Field) bool {
	if _, ok := f.Type().(*types.Optional); ok {
		return false
	}
	return c.zeroValueExpr(f.Type()) == nil
}

// validateSerializableFieldType checks that a field's type can be serialized.
// If the type involves a TypeParam, the TypeParam must be constrained with
// Encodable + Decodable. This catches errors early with a clear message instead
// of letting the synthesized encode/decode methods fail with confusing errors.
func (c *Checker) validateSerializableFieldType(typ types.Type, fd *ast.FieldDecl, typeName string) bool {
	switch t := typ.(type) {
	case *types.TypeParam:
		if !hasConstraint(t, "Encodable") || !hasConstraint(t, "Decodable") {
			c.errorf(fd.Pos(),
				"type %s is `serializable but field '%s' has unconstrained type parameter %s — "+
					"add constraint %s: Encodable + Decodable, or mark the field `skip",
				typeName, fd.Name, t.Obj().Name(), t.Obj().Name())
			return false
		}
	case *types.Optional:
		return c.validateSerializableFieldType(t.Elem(), fd, typeName)
	case *types.Instance:
		for _, arg := range t.TypeArgs() {
			if !c.validateSerializableFieldType(arg, fd, typeName) {
				return false
			}
		}
	}
	return true
}

// hasConstraint checks if a TypeParam has a constraint with the given type name.
func hasConstraint(tp *types.TypeParam, name string) bool {
	for _, c := range tp.Constraints() {
		if named, ok := c.(*types.Named); ok && named.Obj().Name() == name {
			return true
		}
	}
	return false
}

// vectorElemType returns the element type if typ is Vector[T], or nil otherwise.
func vectorElemType(typ types.Type) types.Type {
	inst, ok := typ.(*types.Instance)
	if !ok {
		return nil
	}
	if origin, ok := inst.Origin().(*types.Named); ok && origin == types.TypVector {
		if len(inst.TypeArgs()) > 0 {
			return inst.TypeArgs()[0]
		}
	}
	return nil
}

// mapKeyValueTypes returns (keyType, valueType) if typ is Map[K, V], or (nil, nil) otherwise.
func mapKeyValueTypes(typ types.Type) (types.Type, types.Type) {
	inst, ok := typ.(*types.Instance)
	if !ok {
		return nil, nil
	}
	if origin, ok := inst.Origin().(*types.Named); ok && origin == types.TypMap {
		if len(inst.TypeArgs()) >= 2 {
			return inst.TypeArgs()[0], inst.TypeArgs()[1]
		}
	}
	return nil, nil
}

// buildDecodeFieldMatchChain builds the if/else chain for matching decoded keys
// to fields. Supports both regular fields and flattened sub-fields via decodeField.
func (c *Checker) buildDecodeFieldMatchChain(entries []decodeField) ast.Stmt {
	// Build from last to first so else clauses chain correctly.
	var tail ast.Stmt = &ast.Block{Stmts: []ast.Stmt{
		makeExprStmt(callMember(ident("d"), "skip_value")),
	}}

	for i := len(entries) - 1; i >= 0; i-- {
		df := entries[i]
		f := df.field
		localName := df.localName

		var bodyStmts []ast.Stmt
		_, isOptional := f.Type().(*types.Optional)

		if isOptional {
			nullVar := "_null_" + localName
			bodyStmts = append(bodyStmts, &ast.InferredVarDecl{
				Name: nullVar, Value: callMember(ident("d"), "decode_none"),
			})
			bodyStmts = append(bodyStmts, &ast.IfStmt{
				Cond: &ast.UnaryExpr{Op: ast.UnaryNot, Operand: ident(nullVar)},
				Body: &ast.Block{Stmts: []ast.Stmt{
					&ast.AssignStmt{
						Target: ident(localName), Op: ast.OpAssign,
						Value: propagate(c.makeDecodeCall(f.Type().(*types.Optional).Elem())),
					},
				}},
			})
		} else if elemType := vectorElemType(f.Type()); elemType != nil {
			moreVar := "_more_" + localName
			bodyStmts = append(bodyStmts, makeExprStmt(callMember(ident("d"), "begin_array")))
			bodyStmts = append(bodyStmts, &ast.InfiniteLoop{
				Body: &ast.Block{Stmts: []ast.Stmt{
					&ast.InferredVarDecl{Name: moreVar, Value: callMember(ident("d"), "has_next_element")},
					&ast.IfStmt{
						Cond: &ast.UnaryExpr{Op: ast.UnaryNot, Operand: ident(moreVar)},
						Body: &ast.Block{Stmts: []ast.Stmt{&ast.BreakStmt{}}},
					},
					makeExprStmt(callMember(ident(localName), "push", propagate(c.makeDecodeCall(elemType)))),
				}},
			})
			bodyStmts = append(bodyStmts, makeExprStmt(callMember(ident("d"), "end_array")))
		} else if keyType, valType := mapKeyValueTypes(f.Type()); valType != nil {
			mkVar := "_dmk_" + localName
			parsedKeyVar := "_dpk_" + localName

			var loopStmts []ast.Stmt
			loopStmts = append(loopStmts, &ast.TypedVarDecl{
				Type: &ast.OptionalTypeRef{Inner: &ast.NamedTypeRef{Name: "string"}},
				Name: mkVar, Value: callMember(ident("d"), "next_key"),
			})
			loopStmts = append(loopStmts, &ast.IfStmt{
				Cond: &ast.IsExpr{Expr: ident(mkVar), Pattern: &ast.IdentIsPattern{Name: "absent"}},
				Body: &ast.Block{Stmts: []ast.Stmt{&ast.BreakStmt{}}},
			})

			var indexExpr ast.Expr
			if keyType == types.TypString {
				indexExpr = &ast.ErrorUnwrapExpr{Expr: ident(mkVar)}
			} else {
				keyTypeName := ""
				if n, ok := keyType.(*types.Named); ok {
					keyTypeName = n.Obj().Name()
				}
				loopStmts = append(loopStmts, &ast.InferredVarDecl{
					Name: parsedKeyVar,
					Value: propagate(&ast.CallExpr{
						Callee: &ast.IndexExpr{
							Target: ident("scan"),
							Index:  ident(keyTypeName),
						},
						Args: []*ast.Arg{{Value: &ast.ErrorUnwrapExpr{Expr: ident(mkVar)}}},
					}),
				})
				indexExpr = ident(parsedKeyVar)
			}

			loopStmts = append(loopStmts, &ast.AssignStmt{
				Target: &ast.IndexExpr{Target: ident(localName), Index: indexExpr},
				Op:     ast.OpAssign,
				Value:  propagate(c.makeDecodeCall(valType)),
			})

			bodyStmts = append(bodyStmts, makeExprStmt(callMember(ident("d"), "begin_object")))
			bodyStmts = append(bodyStmts, &ast.InfiniteLoop{Body: &ast.Block{Stmts: loopStmts}})
			bodyStmts = append(bodyStmts, makeExprStmt(callMember(ident("d"), "end_object")))
		} else {
			bodyStmts = append(bodyStmts, &ast.AssignStmt{
				Target: ident(localName), Op: ast.OpAssign,
				Value: propagate(c.makeDecodeCall(f.Type())),
			})
		}

		tail = &ast.IfStmt{
			Cond: &ast.BinaryExpr{Left: ident("_key"), Op: ast.BinEq, Right: strLit(df.wireName)},
			Body: &ast.Block{Stmts: bodyStmts},
			Else: tail,
		}
	}
	return tail
}

// makeDecodeCall creates the expression to decode a field value from a Decoder.
// Primitives use the Decoder's type-specific methods; user types use TYPE.decode(d).
func (c *Checker) makeDecodeCall(typ types.Type) ast.Expr {
	switch typ {
	case types.TypInt, types.TypI8, types.TypI16, types.TypI32, types.TypI64:
		return callMember(ident("d"), "decode_int")
	case types.TypUint, types.TypU8, types.TypU16, types.TypU32, types.TypU64:
		return callMember(ident("d"), "decode_uint")
	case types.TypF64, types.TypF32:
		return callMember(ident("d"), "decode_f64")
	case types.TypBool:
		return callMember(ident("d"), "decode_bool")
	case types.TypString:
		return callMember(ident("d"), "decode_string")
	}
	// User-defined types: call the Decodable factory TYPE.decode(d).
	if named, ok := typ.(*types.Named); ok {
		return callMember(ident(named.Obj().Name()), "decode", ident("d"))
	}
	// Enum types: call the factory ENUM.decode(d).
	if enum, ok := typ.(*types.Enum); ok {
		return callMember(ident(enum.Obj().Name()), "decode", ident("d"))
	}
	// Fallback — will likely fail type-checking, which is the right behavior.
	return callMember(ident(typ.String()), "decode", ident("d"))
}

// typeToTypeRef converts a resolved types.Type to an ast.TypeRef for synthesized AST.
func (c *Checker) typeToTypeRef(typ types.Type) ast.TypeRef {
	switch t := typ.(type) {
	case *types.Named:
		return &ast.NamedTypeRef{Name: t.Obj().Name()}
	case *types.Optional:
		return &ast.OptionalTypeRef{Inner: c.typeToTypeRef(t.Elem())}
	case *types.Instance:
		if origin, ok := t.Origin().(*types.Named); ok {
			if origin == types.TypVector && len(t.TypeArgs()) > 0 {
				return &ast.SliceTypeRef{Element: c.typeToTypeRef(t.TypeArgs()[0])}
			}
			if origin == types.TypMap && len(t.TypeArgs()) >= 2 {
				return &ast.NamedTypeRef{
					Name:     "map",
					TypeArgs: []ast.TypeRef{c.typeToTypeRef(t.TypeArgs()[0]), c.typeToTypeRef(t.TypeArgs()[1])},
				}
			}
		}
		return &ast.NamedTypeRef{Name: typ.String()}
	default:
		return &ast.NamedTypeRef{Name: typ.String()}
	}
}

// zeroValueExpr returns an AST expression for the zero value of a type,
// or nil if the type has no known zero value (user-defined types).
func (c *Checker) zeroValueExpr(typ types.Type) ast.Expr {
	switch typ {
	case types.TypInt, types.TypI8, types.TypI16, types.TypI32, types.TypI64,
		types.TypUint, types.TypU8, types.TypU16, types.TypU32, types.TypU64:
		return intLit(0)
	case types.TypF32, types.TypF64:
		return &ast.FloatLit{Raw: "0.0"}
	case types.TypBool:
		return &ast.BoolLit{Value: false}
	case types.TypString:
		return strLit("")
	case types.TypChar:
		return &ast.CharLit{Raw: "'\\0'"}
	}
	if _, ok := typ.(*types.Optional); ok {
		return &ast.NoneLit{}
	}
	// Vector: T[]() — empty vector
	if inst, ok := typ.(*types.Instance); ok {
		if origin, ok := inst.Origin().(*types.Named); ok && origin == types.TypVector {
			elemRef := c.typeToTypeRef(inst.TypeArgs()[0])
			if ntr, ok := elemRef.(*ast.NamedTypeRef); ok {
				return &ast.CallExpr{Callee: &ast.SliceTypeExpr{Inner: ident(ntr.Name)}, Args: nil}
			}
		}
		// Map: map[K,V]() — empty map
		if origin, ok := inst.Origin().(*types.Named); ok && origin == types.TypMap {
			kRef, kOk := c.typeToTypeRef(inst.TypeArgs()[0]).(*ast.NamedTypeRef)
			vRef, vOk := c.typeToTypeRef(inst.TypeArgs()[1]).(*ast.NamedTypeRef)
			if kOk && vOk {
				return &ast.CallExpr{
					Callee: &ast.IndexExpr{
						Target:       ident("map"),
						Index:        ident(kRef.Name),
						ExtraIndices: []ast.Expr{ident(vRef.Name)},
					},
					Args: nil,
				}
			}
		}
	}
	return nil
}

// ── Enum serialization ────────────────────────────────────────────────────

// isSimpleEnum returns true if no variant has data fields.
func isSimpleEnum(enum *types.Enum) bool {
	for _, v := range enum.Variants() {
		if v.NumFields() > 0 {
			return false
		}
	}
	return true
}

// processSerializableEnum handles `serializable on an enum declaration.
// Simple enums (no data variants) encode as strings.
// Data enums use tagged object format: {"<tag>":"Variant",...fields...}.
// The discriminator key defaults to "type" but can be customized via `serializable(tag: "kind").
// Decoder requires the discriminator as the first key (discriminator-first constraint).
func (c *Checker) processSerializableEnum(enum *types.Enum, d *ast.EnumDecl) {
	enum.SetSerializable(true)

	// Extract custom discriminator tag name (default "type").
	if tag := extractSerializableTag(d.Annotations); tag != "" {
		enum.SetSerializeTag(tag)
	}
	tagName := enum.SerializeTag()
	if tagName == "" {
		tagName = "type"
	}

	// Validate: no variant field name (wire name) may conflict with the discriminator tag.
	for _, av := range d.Variants {
		for i, af := range av.Fields {
			wireName := af.Name
			if wireName == "" {
				wireName = fmt.Sprintf("_%d", i)
			}
			if wireName == tagName {
				c.errorf(af.Pos(), "`serializable enum '%s': variant '%s' field '%s' conflicts with discriminator tag '%s'",
					d.Name, av.Name, wireName, tagName)
			}
		}
	}

	// Synthesize encode method if not user-defined.
	if lookupOwnEnumMethod(enum, "encode") == nil {
		var md *ast.MethodDecl
		if isSimpleEnum(enum) {
			md = c.synthesizeEnumEncodeMethod(enum, d)
		} else {
			md = c.synthesizeDataEnumEncodeMethod(enum, d, tagName)
		}
		d.Methods = append(d.Methods, md)
		c.defineEnumMethod(enum, md, d.Name)
	}

	// Synthesize decode factory method if not user-defined.
	if lookupOwnEnumMethod(enum, "decode") == nil {
		var md *ast.MethodDecl
		if isSimpleEnum(enum) {
			md = c.synthesizeEnumDecodeMethod(enum, d)
		} else {
			md = c.synthesizeDataEnumDecodeMethod(enum, d, tagName)
		}
		d.Methods = append(d.Methods, md)
		c.defineEnumMethod(enum, md, d.Name)
	}
}

// lookupOwnEnumMethod checks if an enum already has a method with the given name.
func lookupOwnEnumMethod(enum *types.Enum, name string) *types.Method {
	for _, m := range enum.Methods() {
		if m.Name() == name {
			return m
		}
	}
	return nil
}

// synthesizeEnumEncodeMethod builds:
//
//	encode(Encoder ~e)! `public {
//	  match this {
//	    EnumName.Variant1 => e.encode_string("Variant1"),
//	    EnumName.Variant2 => e.encode_string("Variant2"),
//	    ...
//	  }
//	}
func (c *Checker) synthesizeEnumEncodeMethod(enum *types.Enum, d *ast.EnumDecl) *ast.MethodDecl {
	enumName := d.Name

	var arms []*ast.MatchArm
	for _, v := range enum.Variants() {
		arms = append(arms, &ast.MatchArm{
			Pattern: &ast.EnumVariantMatchPattern{Enum: enumName, Variant: v.Name()},
			Block: &ast.Block{Stmts: []ast.Stmt{
				makeExprStmt(callMember(ident("e"), "encode_string", strLit(v.Name()))),
			}},
		})
	}

	body := &ast.Block{Stmts: []ast.Stmt{
		makeExprStmt(&ast.MatchExpr{
			Subject: &ast.ThisExpr{},
			Arms:    arms,
		}),
	}}

	return &ast.MethodDecl{
		Name:        "encode",
		Receiver:    &ast.ReceiverParam{RefMod: ast.RefNone},
		Params:      []*ast.Param{{Type: &ast.MutRefTypeRef{Inner: &ast.NamedTypeRef{Name: "Encoder"}}, Name: "e"}},
		ReturnType:  &ast.ReturnTypeSpec{CanError: true},
		Annotations: []*ast.MetaAnnotation{{Name: "public"}},
		Body:        body,
	}
}

// synthesizeEnumDecodeMethod builds:
//
//	decode(Decoder ~d) EnumName! `factory `public {
//	  string _tag = d.decode_string()?;
//	  if _tag == "Variant1" { return EnumName.Variant1; }
//	  else if _tag == "Variant2" { return EnumName.Variant2; }
//	  ...
//	  else { raise DecodeError(message: "unknown enum variant: " + _tag, field: "", position: 0); }
//	}
func (c *Checker) synthesizeEnumDecodeMethod(enum *types.Enum, d *ast.EnumDecl) *ast.MethodDecl {
	enumName := d.Name
	var stmts []ast.Stmt

	// string _tag = d.decode_string()?;
	stmts = append(stmts, &ast.TypedVarDecl{
		Type:  &ast.NamedTypeRef{Name: "string"},
		Name:  "_tag",
		Value: propagate(callMember(ident("d"), "decode_string")),
	})

	// Build if/else chain matching tag to variants.
	variants := enum.Variants()
	if len(variants) > 0 {
		// The final else raises a DecodeError.
		var tail ast.Stmt = &ast.Block{Stmts: []ast.Stmt{
			&ast.RaiseStmt{
				Value: &ast.CallExpr{
					Callee: ident("DecodeError"),
					Args: []*ast.Arg{
						{Name: "message", Value: &ast.BinaryExpr{
							Left:  strLit("unknown enum variant: "),
							Op:    ast.BinAdd,
							Right: ident("_tag"),
						}},
						{Name: "field", Value: strLit("")},
						{Name: "position", Value: intLit(0)},
					},
				},
			},
		}}

		// Build from last to first for proper else chaining.
		for i := len(variants) - 1; i >= 0; i-- {
			v := variants[i]
			tail = &ast.IfStmt{
				Cond: &ast.BinaryExpr{Left: ident("_tag"), Op: ast.BinEq, Right: strLit(v.Name())},
				Body: &ast.Block{Stmts: []ast.Stmt{
					&ast.ReturnStmt{Value: memberExpr(ident(enumName), v.Name())},
				}},
				Else: tail,
			}
		}
		stmts = append(stmts, tail)
	}

	return &ast.MethodDecl{
		Name:   "decode",
		Params: []*ast.Param{{Type: &ast.MutRefTypeRef{Inner: &ast.NamedTypeRef{Name: "Decoder"}}, Name: "d"}},
		ReturnType: &ast.ReturnTypeSpec{
			Type:     &ast.NamedTypeRef{Name: enumName},
			CanError: true,
		},
		Annotations: []*ast.MetaAnnotation{{Name: "factory"}, {Name: "public"}},
		Body:        &ast.Block{Stmts: stmts},
	}
}

// ── Data enum serialization (tagged object format) ────────────────────────

// synthesizeDataEnumEncodeMethod builds:
//
//	encode(Encoder ~e)! `public {
//	  match this {
//	    Enum.Variant1(f1, f2) => {
//	      e.begin_object(0); e.encode_key("<tag>"); e.encode_string("Variant1");
//	      e.encode_key("f1"); f1.encode(e); e.encode_key("f2"); f2.encode(e);
//	      e.end_object();
//	    },
//	    Enum.Fieldless => {
//	      e.begin_object(0); e.encode_key("<tag>"); e.encode_string("Fieldless");
//	      e.end_object();
//	    },
//	  }
//	}
//
// tagName is the discriminator key (default "type", customizable via `serializable(tag: "...")`).
func (c *Checker) synthesizeDataEnumEncodeMethod(enum *types.Enum, d *ast.EnumDecl, tagName string) *ast.MethodDecl {
	enumName := d.Name

	var arms []*ast.MatchArm
	for _, v := range enum.Variants() {
		var stmts []ast.Stmt
		stmts = append(stmts, makeExprStmt(callMember(ident("e"), "begin_object", intLit(0))))
		stmts = append(stmts, makeExprStmt(callMember(ident("e"), "encode_key", strLit(tagName))))
		stmts = append(stmts, makeExprStmt(callMember(ident("e"), "encode_string", strLit(v.Name()))))

		if v.NumFields() > 0 {
			// Build bindings for destructure pattern
			bindings := make([]string, v.NumFields())
			for i, f := range v.Fields() {
				bindName := "_v_" + f.Name()
				if f.Name() == "" {
					bindName = fmt.Sprintf("_v_%d", i)
				}
				bindings[i] = bindName
				wireName := f.Name()
				if wireName == "" {
					wireName = fmt.Sprintf("_%d", i)
				}
				stmts = append(stmts, makeExprStmt(callMember(ident("e"), "encode_key", strLit(wireName))))
				stmts = append(stmts, makeExprStmt(callMember(ident(bindName), "encode", ident("e"))))
			}
			stmts = append(stmts, makeExprStmt(callMember(ident("e"), "end_object")))
			arms = append(arms, &ast.MatchArm{
				Pattern: &ast.EnumDestructureMatchPattern{Enum: enumName, Variant: v.Name(), Bindings: bindings},
				Block:   &ast.Block{Stmts: stmts},
			})
		} else {
			stmts = append(stmts, makeExprStmt(callMember(ident("e"), "end_object")))
			arms = append(arms, &ast.MatchArm{
				Pattern: &ast.EnumVariantMatchPattern{Enum: enumName, Variant: v.Name()},
				Block:   &ast.Block{Stmts: stmts},
			})
		}
	}

	body := &ast.Block{Stmts: []ast.Stmt{
		makeExprStmt(&ast.MatchExpr{
			Subject: &ast.ThisExpr{},
			Arms:    arms,
		}),
	}}

	return &ast.MethodDecl{
		Name:        "encode",
		Receiver:    &ast.ReceiverParam{RefMod: ast.RefNone},
		Params:      []*ast.Param{{Type: &ast.MutRefTypeRef{Inner: &ast.NamedTypeRef{Name: "Encoder"}}, Name: "e"}},
		ReturnType:  &ast.ReturnTypeSpec{CanError: true},
		Annotations: []*ast.MetaAnnotation{{Name: "public"}},
		Body:        body,
	}
}

// synthesizeDataEnumDecodeMethod builds a tagged-object decoder.
// The discriminator key (tagName, default "type") MUST appear first in the JSON object.
//
//	decode(Decoder ~d) EnumName! `factory `public {
//	  d.begin_object()?;
//	  string? _dk = d.next_key()?;
//	  if _dk is absent { raise DecodeError(...); }
//	  if _dk != "type" { raise DecodeError(...); }  // _dk narrowed to string
//	  string _tag = d.decode_string()?;
//	  if _tag == "Circle" {
//	    f64 _f_radius = 0.0;
//	    for { ... key matching ... }
//	    d.end_object()?;
//	    return EnumName.Circle(radius: _f_radius);
//	  } else if ...
//	  else { raise DecodeError(...); }
//	}
func (c *Checker) synthesizeDataEnumDecodeMethod(enum *types.Enum, d *ast.EnumDecl, tagName string) *ast.MethodDecl {
	enumName := d.Name
	var stmts []ast.Stmt

	// d.begin_object()?;
	stmts = append(stmts, makeExprStmt(callMember(ident("d"), "begin_object")))

	// string? _dk = d.next_key()?;
	stmts = append(stmts, &ast.TypedVarDecl{
		Type: &ast.OptionalTypeRef{Inner: &ast.NamedTypeRef{Name: "string"}},
		Name: "_dk", Value: propagate(callMember(ident("d"), "next_key")),
	})

	// if _dk is absent { raise DecodeError(...); }
	// After this check, _dk is narrowed from string? to string.
	stmts = append(stmts, &ast.IfStmt{
		Cond: &ast.IsExpr{Expr: ident("_dk"), Pattern: &ast.IdentIsPattern{Name: "absent"}},
		Body: &ast.Block{Stmts: []ast.Stmt{
			&ast.RaiseStmt{Value: c.makeDecodeError(fmt.Sprintf("expected '%s' discriminator key in enum object", tagName))},
		}},
	})

	// if _dk != "<tag>" { raise DecodeError(...); }
	// _dk is narrowed to string after the absent check above.
	stmts = append(stmts, &ast.IfStmt{
		Cond: &ast.BinaryExpr{Left: ident("_dk"), Op: ast.BinNeq, Right: strLit(tagName)},
		Body: &ast.Block{Stmts: []ast.Stmt{
			&ast.RaiseStmt{Value: c.makeDecodeError(fmt.Sprintf("first key in serializable enum must be '%s' (discriminator-first constraint)", tagName))},
		}},
	})

	// string _tag = d.decode_string()?;
	stmts = append(stmts, &ast.TypedVarDecl{
		Type: &ast.NamedTypeRef{Name: "string"}, Name: "_tag",
		Value: propagate(callMember(ident("d"), "decode_string")),
	})

	// Build if/else chain for each variant.
	variants := enum.Variants()
	if len(variants) > 0 {
		// Final else: unknown variant error
		var tail ast.Stmt = &ast.Block{Stmts: []ast.Stmt{
			&ast.RaiseStmt{Value: &ast.CallExpr{
				Callee: ident("DecodeError"),
				Args: []*ast.Arg{
					{Name: "message", Value: &ast.BinaryExpr{
						Left: strLit("unknown enum variant: "), Op: ast.BinAdd, Right: ident("_tag"),
					}},
					{Name: "field", Value: strLit("")},
					{Name: "position", Value: intLit(0)},
				},
			}},
		}}

		for i := len(variants) - 1; i >= 0; i-- {
			v := variants[i]
			tail = &ast.IfStmt{
				Cond: &ast.BinaryExpr{Left: ident("_tag"), Op: ast.BinEq, Right: strLit(v.Name())},
				Body: &ast.Block{Stmts: c.buildVariantDecodeBody(enumName, v)},
				Else: tail,
			}
		}
		stmts = append(stmts, tail)
	}

	return &ast.MethodDecl{
		Name:   "decode",
		Params: []*ast.Param{{Type: &ast.MutRefTypeRef{Inner: &ast.NamedTypeRef{Name: "Decoder"}}, Name: "d"}},
		ReturnType: &ast.ReturnTypeSpec{
			Type:     &ast.NamedTypeRef{Name: enumName},
			CanError: true,
		},
		Annotations: []*ast.MetaAnnotation{{Name: "factory"}, {Name: "public"}},
		Body:        &ast.Block{Stmts: stmts},
	}
}

// buildVariantDecodeBody builds the decode body for a single variant.
// For fieldless variants: skip remaining keys, end_object, return.
// For data variants: declare locals, key-match loop, end_object, return with args.
func (c *Checker) buildVariantDecodeBody(enumName string, v *types.Variant) []ast.Stmt {
	var stmts []ast.Stmt

	if v.NumFields() == 0 {
		// Fieldless: consume remaining keys and return
		stmts = append(stmts, c.buildSkipRemainingKeys())
		stmts = append(stmts, makeExprStmt(callMember(ident("d"), "end_object")))
		stmts = append(stmts, &ast.ReturnStmt{Value: memberExpr(ident(enumName), v.Name())})
		return stmts
	}

	// Declare local variables for each field
	for i, f := range v.Fields() {
		localName := "_f_" + varFieldLocalName(f, i)
		stmts = append(stmts, c.makeVarFieldLocalDecl(localName, f.Type()))
	}

	// Key-matching loop
	stmts = append(stmts, c.buildVarFieldKeyMatchLoop(v))

	// d.end_object()?;
	stmts = append(stmts, makeExprStmt(callMember(ident("d"), "end_object")))

	// return EnumName.Variant(field1: _f_field1, ...);
	var args []*ast.Arg
	for i, f := range v.Fields() {
		localName := "_f_" + varFieldLocalName(f, i)
		localExpr := ident(localName)
		// If the type has no zero value, we used T? — unwrap with !
		if c.varFieldNeedsUnwrap(f.Type()) {
			localExpr = &ast.IdentExpr{Name: localName}
			args = append(args, &ast.Arg{
				Name:  f.Name(),
				Value: &ast.ErrorUnwrapExpr{Expr: localExpr},
			})
		} else {
			args = append(args, &ast.Arg{Name: f.Name(), Value: localExpr})
		}
	}
	stmts = append(stmts, &ast.ReturnStmt{
		Value: &ast.CallExpr{
			Callee: memberExpr(ident(enumName), v.Name()),
			Args:   args,
		},
	})

	return stmts
}

// buildSkipRemainingKeys builds a loop that reads and skips all remaining keys.
func (c *Checker) buildSkipRemainingKeys() ast.Stmt {
	return &ast.InfiniteLoop{Body: &ast.Block{Stmts: []ast.Stmt{
		&ast.TypedVarDecl{
			Type: &ast.OptionalTypeRef{Inner: &ast.NamedTypeRef{Name: "string"}},
			Name: "_sk", Value: propagate(callMember(ident("d"), "next_key")),
		},
		&ast.IfStmt{
			Cond: &ast.IsExpr{Expr: ident("_sk"), Pattern: &ast.IdentIsPattern{Name: "absent"}},
			Body: &ast.Block{Stmts: []ast.Stmt{&ast.BreakStmt{}}},
		},
		makeExprStmt(callMember(ident("d"), "skip_value")),
	}}}
}

// buildVarFieldKeyMatchLoop builds the key-matching loop for a variant's fields.
func (c *Checker) buildVarFieldKeyMatchLoop(v *types.Variant) ast.Stmt {
	var loopStmts []ast.Stmt

	loopStmts = append(loopStmts, &ast.TypedVarDecl{
		Type: &ast.OptionalTypeRef{Inner: &ast.NamedTypeRef{Name: "string"}},
		Name: "_k", Value: propagate(callMember(ident("d"), "next_key")),
	})
	loopStmts = append(loopStmts, &ast.IfStmt{
		Cond: &ast.IsExpr{Expr: ident("_k"), Pattern: &ast.IdentIsPattern{Name: "absent"}},
		Body: &ast.Block{Stmts: []ast.Stmt{&ast.BreakStmt{}}},
	})
	loopStmts = append(loopStmts, &ast.TypedVarDecl{
		Type: &ast.NamedTypeRef{Name: "string"}, Name: "_key",
		Value: &ast.BinaryExpr{Left: ident("_k"), Op: ast.BinElvis, Right: strLit("")},
	})

	// Build if/else chain for field keys
	if v.NumFields() > 0 {
		var tail ast.Stmt = &ast.Block{Stmts: []ast.Stmt{
			makeExprStmt(callMember(ident("d"), "skip_value")),
		}}

		for i := v.NumFields() - 1; i >= 0; i-- {
			f := v.Fields()[i]
			localName := "_f_" + varFieldLocalName(f, i)
			wireName := f.Name()
			if wireName == "" {
				wireName = fmt.Sprintf("_%d", i)
			}

			tail = &ast.IfStmt{
				Cond: &ast.BinaryExpr{Left: ident("_key"), Op: ast.BinEq, Right: strLit(wireName)},
				Body: &ast.Block{Stmts: []ast.Stmt{
					&ast.AssignStmt{
						Target: ident(localName), Op: ast.OpAssign,
						Value: propagate(c.makeDecodeCall(f.Type())),
					},
				}},
				Else: tail,
			}
		}
		loopStmts = append(loopStmts, tail)
	}

	return &ast.InfiniteLoop{Body: &ast.Block{Stmts: loopStmts}}
}

// varFieldLocalName returns a local variable name for a variant field.
// Uses the field name, or a positional index for unnamed fields.
func varFieldLocalName(f *types.VarField, index int) string {
	if f.Name() != "" {
		return f.Name()
	}
	return fmt.Sprintf("field_%d", index)
}

// makeVarFieldLocalDecl creates a typed variable declaration with a zero value for a variant field type.
func (c *Checker) makeVarFieldLocalDecl(localName string, typ types.Type) ast.Stmt {
	if _, ok := typ.(*types.Optional); ok {
		return &ast.TypedVarDecl{
			Type: c.typeToTypeRef(typ), Name: localName, Value: &ast.NoneLit{},
		}
	}
	zv := c.zeroValueExpr(typ)
	if zv != nil {
		return &ast.TypedVarDecl{
			Type: c.typeToTypeRef(typ), Name: localName, Value: zv,
		}
	}
	// No known zero value — use T? with none, unwrap at construction.
	return &ast.TypedVarDecl{
		Type: &ast.OptionalTypeRef{Inner: c.typeToTypeRef(typ)}, Name: localName, Value: &ast.NoneLit{},
	}
}

// varFieldNeedsUnwrap returns true if the field type has no zero value and was
// stored as T? during decode.
func (c *Checker) varFieldNeedsUnwrap(typ types.Type) bool {
	if _, ok := typ.(*types.Optional); ok {
		return false
	}
	return c.zeroValueExpr(typ) == nil
}

// makeDecodeError creates a DecodeError constructor call expression.
func (c *Checker) makeDecodeError(msg string) ast.Expr {
	return &ast.CallExpr{
		Callee: ident("DecodeError"),
		Args: []*ast.Arg{
			{Name: "message", Value: strLit(msg)},
			{Name: "field", Value: strLit("")},
			{Name: "position", Value: intLit(0)},
		},
	}
}

// ── AST node construction helpers ─────────────────────────────────────────

func ident(name string) *ast.IdentExpr {
	return &ast.IdentExpr{Name: name}
}

func intLit(value int) *ast.IntLit {
	return &ast.IntLit{Raw: strconv.Itoa(value)}
}

func strLit(value string) *ast.StringLit {
	return &ast.StringLit{
		Parts: []ast.StringPart{ast.StringText{Text: value}},
		Kind:  ast.StringRegular,
	}
}

func memberExpr(target ast.Expr, field string) *ast.MemberExpr {
	return &ast.MemberExpr{Target: target, Field: field}
}

func callMember(target ast.Expr, method string, args ...ast.Expr) *ast.CallExpr {
	var callArgs []*ast.Arg
	for _, a := range args {
		callArgs = append(callArgs, &ast.Arg{Value: a})
	}
	return &ast.CallExpr{
		Callee: &ast.MemberExpr{Target: target, Field: method},
		Args:   callArgs,
	}
}

func propagate(expr ast.Expr) *ast.ErrorPropagateExpr {
	return &ast.ErrorPropagateExpr{Expr: expr}
}

func makeExprStmt(expr ast.Expr) *ast.ExprStmt {
	return &ast.ExprStmt{Expr: expr}
}
