package ast

import "djabi.dev/go/promise_lang/internal/parser"

func (b *Builder) VisitEnumDestructurePattern(ctx *parser.EnumDestructurePatternContext) interface{} {
	idents := ctx.AllIDENT()
	node := &EnumDestructureMatchPattern{
		nodeBase: b.baseFromContext(ctx),
		Enum:     idents[0].GetText(),
		Variant:  idents[1].GetText(),
	}
	if pf := ctx.PatternFields(); pf != nil {
		node.Bindings = b.visitPatternFields(pf)
	}
	return node
}

func (b *Builder) VisitEnumVariantPattern(ctx *parser.EnumVariantPatternContext) interface{} {
	idents := ctx.AllIDENT()
	return &EnumVariantMatchPattern{
		nodeBase: b.baseFromContext(ctx),
		Enum:     idents[0].GetText(),
		Variant:  idents[1].GetText(),
	}
}

func (b *Builder) VisitTypeBindingPattern(ctx *parser.TypeBindingPatternContext) interface{} {
	return &TypeBindingMatchPattern{
		nodeBase: b.baseFromContext(ctx),
		TypeName: ctx.IDENT().GetText(),
		Binding:  b.bindingText(ctx.BindingName()),
	}
}

func (b *Builder) VisitShortDestructurePattern(ctx *parser.ShortDestructurePatternContext) interface{} {
	node := &ShortDestructureMatchPattern{
		nodeBase: b.baseFromContext(ctx),
		Name:     ctx.IDENT().GetText(),
	}
	if pf := ctx.PatternFields(); pf != nil {
		node.Bindings = b.visitPatternFields(pf)
	}
	return node
}

func (b *Builder) VisitNamePattern(ctx *parser.NamePatternContext) interface{} {
	return &NameMatchPattern{
		nodeBase: b.baseFromContext(ctx),
		Name:     ctx.IDENT().GetText(),
	}
}

func (b *Builder) VisitIntLiteralPattern(ctx *parser.IntLiteralPatternContext) interface{} {
	raw, suffix := splitNumericSuffix(ctx.INT_LITERAL().GetText())
	return &LiteralMatchPattern{
		nodeBase: b.baseFromContext(ctx),
		Value:    &IntLit{nodeBase: b.baseFromContext(ctx), Raw: raw, Suffix: suffix},
	}
}

func (b *Builder) VisitFloatLiteralPattern(ctx *parser.FloatLiteralPatternContext) interface{} {
	raw, suffix := splitNumericSuffix(ctx.FLOAT_LITERAL().GetText())
	return &LiteralMatchPattern{
		nodeBase: b.baseFromContext(ctx),
		Value:    &FloatLit{nodeBase: b.baseFromContext(ctx), Raw: raw, Suffix: suffix},
	}
}

func (b *Builder) VisitCharLiteralPattern(ctx *parser.CharLiteralPatternContext) interface{} {
	return &LiteralMatchPattern{
		nodeBase: b.baseFromContext(ctx),
		Value:    &CharLit{nodeBase: b.baseFromContext(ctx), Raw: ctx.CHAR_LITERAL().GetText()},
	}
}

func (b *Builder) VisitTrueLiteralPattern(ctx *parser.TrueLiteralPatternContext) interface{} {
	return &LiteralMatchPattern{
		nodeBase: b.baseFromContext(ctx),
		Value:    &BoolLit{nodeBase: b.baseFromContext(ctx), Value: true},
	}
}

func (b *Builder) VisitFalseLiteralPattern(ctx *parser.FalseLiteralPatternContext) interface{} {
	return &LiteralMatchPattern{
		nodeBase: b.baseFromContext(ctx),
		Value:    &BoolLit{nodeBase: b.baseFromContext(ctx), Value: false},
	}
}

func (b *Builder) VisitNoneLiteralPattern(ctx *parser.NoneLiteralPatternContext) interface{} {
	return &LiteralMatchPattern{
		nodeBase: b.baseFromContext(ctx),
		Value:    &NoneLit{nodeBase: b.baseFromContext(ctx)},
	}
}

func (b *Builder) VisitStringLiteralPattern(ctx *parser.StringLiteralPatternContext) interface{} {
	return &LiteralMatchPattern{
		nodeBase: b.baseFromContext(ctx),
		Value:    b.visitStringLiteral(ctx.StringLiteral()),
	}
}

func (b *Builder) VisitWildcardPattern(ctx *parser.WildcardPatternContext) interface{} {
	return &WildcardMatchPattern{nodeBase: b.baseFromContext(ctx)}
}

// Is-patterns (for `is` expressions)

func (b *Builder) VisitDestructureIsPattern(ctx *parser.DestructureIsPatternContext) interface{} {
	node := &DestructureIsPattern{
		nodeBase: b.baseFromContext(ctx),
		TypeName: ctx.IDENT().GetText(),
	}
	if pf := ctx.PatternFields(); pf != nil {
		node.Bindings = b.visitPatternFields(pf)
	}
	return node
}

func (b *Builder) VisitIdentIsPattern(ctx *parser.IdentIsPatternContext) interface{} {
	return &IdentIsPattern{
		nodeBase: b.baseFromContext(ctx),
		Name:     ctx.IDENT().GetText(),
	}
}

func (b *Builder) visitPatternFields(ctx parser.IPatternFieldsContext) []string {
	pfc := ctx.(*parser.PatternFieldsContext)
	var fields []string
	for _, bn := range pfc.AllBindingName() {
		fields = append(fields, b.bindingText(bn))
	}
	return fields
}
