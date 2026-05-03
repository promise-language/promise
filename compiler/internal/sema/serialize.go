package sema

import (
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
		}
	}

	// Synthesize encode method if not user-defined.
	if lookupOwnMethod(named, "encode") == nil {
		md := c.synthesizeEncodeMethod(named, d)
		d.Methods = append(d.Methods, md)
		c.defineMethod(named, md, d.Name)
	}

	// Synthesize decode factory method if not user-defined.
	if lookupOwnMethod(named, "decode") == nil {
		md := c.synthesizeDecodeMethod(named, d)
		d.Methods = append(d.Methods, md)
		c.defineMethod(named, md, d.Name)
	}
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
		wireName := f.Name()
		if f.KeyName() != "" {
			wireName = f.KeyName()
		}

		_, isOptional := f.Type().(*types.Optional)

		if isOptional && !f.IncludeNone() {
			// Omit when none (default). Use if-unwrap to encode only when present:
			//   if _v := this.field { e.encode_key("key"); _v.encode(e); }
			valName := "_enc_" + f.Name()
			stmts = append(stmts, &ast.IfStmt{
				Binding: valName,
				Init:    memberExpr(&ast.ThisExpr{}, f.Name()),
				Body: &ast.Block{Stmts: []ast.Stmt{
					makeExprStmt(callMember(ident("e"), "encode_key", strLit(wireName))),
					makeExprStmt(callMember(ident(valName), "encode", ident("e"))),
				}},
			})
		} else if isOptional && f.IncludeNone() {
			// Always encode — null when none, value when present.
			valName := "_enc_" + f.Name()
			stmts = append(stmts, makeExprStmt(callMember(ident("e"), "encode_key", strLit(wireName))))
			stmts = append(stmts, &ast.IfStmt{
				Binding: valName,
				Init:    memberExpr(&ast.ThisExpr{}, f.Name()),
				Body: &ast.Block{Stmts: []ast.Stmt{
					makeExprStmt(callMember(ident(valName), "encode", ident("e"))),
				}},
				Else: &ast.Block{Stmts: []ast.Stmt{
					makeExprStmt(callMember(ident("e"), "encode_none")),
				}},
			})
		} else {
			// Required field: always encode.
			stmts = append(stmts, makeExprStmt(callMember(ident("e"), "encode_key", strLit(wireName))))
			stmts = append(stmts, makeExprStmt(callMember(memberExpr(&ast.ThisExpr{}, f.Name()), "encode", ident("e"))))
		}
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

// synthesizeDecodeMethod builds an AST MethodDecl for the decode factory.
func (c *Checker) synthesizeDecodeMethod(named *types.Named, d *ast.TypeDecl) *ast.MethodDecl {
	var stmts []ast.Stmt

	// d.begin_object()
	stmts = append(stmts, makeExprStmt(callMember(ident("d"), "begin_object")))

	// Declare local variables for each non-skip field with zero/default values.
	var serFields []*types.Field
	for _, f := range named.Fields() {
		if f.Skip() {
			continue
		}
		serFields = append(serFields, f)
		localName := "_f_" + f.Name()
		stmts = append(stmts, c.makeFieldLocalDecl(localName, f))
	}

	// Key matching loop:
	//   for {
	//     string? _k = d.next_key();
	//     if _k is absent { break; }
	//     string _key = _k ?: "";
	//     if _key == "field1" { _f_field1 = d.decode_TYPE()?; }
	//     else if ... { ... }
	//     else { d.skip_value(); }
	//   }
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

	if len(serFields) > 0 {
		loopStmts = append(loopStmts, c.buildKeyMatchChain(serFields))
	}

	stmts = append(stmts, &ast.InfiniteLoop{Body: &ast.Block{Stmts: loopStmts}})

	// d.end_object()
	stmts = append(stmts, makeExprStmt(callMember(ident("d"), "end_object")))

	// return Self(field1: _f_field1, ...);
	// Skip fields get zero values. Fields stored as T? (user-defined types) use !  to unwrap.
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
		localExpr := ident("_f_" + f.Name())
		if c.fieldNeedsUnwrap(f) {
			// Local was declared as T? — unwrap with ! (panics if field was missing from JSON).
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

// buildKeyMatchChain builds the if/else chain for matching decoded keys to fields.
func (c *Checker) buildKeyMatchChain(fields []*types.Field) ast.Stmt {
	// Build from last to first so else clauses chain correctly.
	var tail ast.Stmt = &ast.Block{Stmts: []ast.Stmt{
		makeExprStmt(callMember(ident("d"), "skip_value")),
	}}

	for i := len(fields) - 1; i >= 0; i-- {
		f := fields[i]
		wireName := f.Name()
		if f.KeyName() != "" {
			wireName = f.KeyName()
		}
		localName := "_f_" + f.Name()

		var bodyStmts []ast.Stmt
		_, isOptional := f.Type().(*types.Optional)

		if isOptional {
			// Decode optional: check for null first, then decode inner type.
			//   _is_null := d.decode_none();
			//   if !_is_null { _f_field = d.decode_TYPE()?; }
			nullVar := "_null_" + f.Name()
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
		} else {
			// Decode required field with error propagation.
			bodyStmts = append(bodyStmts, &ast.AssignStmt{
				Target: ident(localName), Op: ast.OpAssign,
				Value: propagate(c.makeDecodeCall(f.Type())),
			})
		}

		tail = &ast.IfStmt{
			Cond: &ast.BinaryExpr{Left: ident("_key"), Op: ast.BinEq, Right: strLit(wireName)},
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
	return nil
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
