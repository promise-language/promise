package ast

import "djabi.dev/go/promise_lang/internal/parser"

func (b *Builder) VisitNamedType(ctx *parser.NamedTypeContext) interface{} {
	node := &NamedTypeRef{
		nodeBase: b.baseFromContext(ctx),
		Name:     b.termText(ctx.IDENT()),
	}
	if ta := ctx.TypeArgs(); ta != nil {
		tac := ta.(*parser.TypeArgsContext)
		for _, tr := range tac.AllTypeRef() {
			node.TypeArgs = append(node.TypeArgs, b.visitTypeRef(tr))
		}
	}
	return node
}

func (b *Builder) VisitQualifiedType(ctx *parser.QualifiedTypeContext) interface{} {
	idents := ctx.AllIDENT()
	node := &QualifiedTypeRef{
		nodeBase: b.baseFromContext(ctx),
		Module:   idents[0].GetText(),
		Name:     idents[1].GetText(),
	}
	if ta := ctx.TypeArgs(); ta != nil {
		tac := ta.(*parser.TypeArgsContext)
		for _, tr := range tac.AllTypeRef() {
			node.TypeArgs = append(node.TypeArgs, b.visitTypeRef(tr))
		}
	}
	return node
}

func (b *Builder) VisitTupleType(ctx *parser.TupleTypeContext) interface{} {
	node := &TupleTypeRef{nodeBase: b.baseFromContext(ctx)}
	for _, tr := range ctx.AllTypeRef() {
		node.Elements = append(node.Elements, b.visitTypeRef(tr))
	}
	return node
}

func (b *Builder) VisitFunctionType(ctx *parser.FunctionTypeContext) interface{} {
	node := &FunctionTypeRef{
		nodeBase: b.baseFromContext(ctx),
	}
	if ftr := ctx.FuncTypeReturn(); ftr != nil {
		node.Return = b.visitTypeRef(ftr.(*parser.FuncTypeReturnContext).TypeRef())
	}
	if trl := ctx.TypeRefList(); trl != nil {
		trlc := trl.(*parser.TypeRefListContext)
		for _, tr := range trlc.AllTypeRef() {
			node.Params = append(node.Params, b.visitTypeRef(tr))
		}
	}
	return node
}

func (b *Builder) VisitParenType(ctx *parser.ParenTypeContext) interface{} {
	return b.visitTypeRef(ctx.TypeRef())
}

func (b *Builder) VisitSharedRefType(ctx *parser.SharedRefTypeContext) interface{} {
	return &SharedRefTypeRef{
		nodeBase: b.baseFromContext(ctx),
		Inner:    b.visitTypeRef(ctx.TypeRef()),
	}
}

func (b *Builder) VisitMutRefType(ctx *parser.MutRefTypeContext) interface{} {
	return &MutRefTypeRef{
		nodeBase: b.baseFromContext(ctx),
		Inner:    b.visitTypeRef(ctx.TypeRef()),
	}
}

func (b *Builder) VisitPointerType(ctx *parser.PointerTypeContext) interface{} {
	return &PointerTypeRef{
		nodeBase: b.baseFromContext(ctx),
		Inner:    b.visitTypeRef(ctx.TypeRef()),
	}
}

func (b *Builder) VisitOptionalType(ctx *parser.OptionalTypeContext) interface{} {
	return &OptionalTypeRef{
		nodeBase: b.baseFromContext(ctx),
		Inner:    b.visitTypeRef(ctx.TypeRef()),
	}
}

func (b *Builder) VisitSliceType(ctx *parser.SliceTypeContext) interface{} {
	return &SliceTypeRef{
		nodeBase: b.baseFromContext(ctx),
		Element:  b.visitTypeRef(ctx.TypeRef()),
	}
}

func (b *Builder) VisitArrayType(ctx *parser.ArrayTypeContext) interface{} {
	raw, _ := splitNumericSuffix(b.termText(ctx.INT_LITERAL()))
	return &ArrayTypeRef{
		nodeBase: b.baseFromContext(ctx),
		Element:  b.visitTypeRef(ctx.TypeRef()),
		Size:     raw,
	}
}
