package ast

import (
	"strings"

	"djabi.dev/go/promise_lang/internal/parser"
)

func (b *Builder) VisitCompilationUnit(ctx *parser.CompilationUnitContext) interface{} {
	file := &File{nodeBase: b.baseFromContext(ctx)}
	for _, u := range ctx.AllUseDecl() {
		file.Uses = append(file.Uses, u.Accept(b).(*UseDecl))
	}
	for _, d := range ctx.AllDeclaration() {
		result := d.Accept(b)
		if result != nil {
			file.Decls = append(file.Decls, result.(Decl))
		}
	}
	return file
}

func (b *Builder) VisitUseDecl(ctx *parser.UseDeclContext) interface{} {
	path := ""
	if sl := ctx.StringLiteral(); sl != nil {
		lit := b.visitStringLiteral(sl)
		if lit != nil {
			path = lit.Raw
		}
	}
	// Strip surrounding quotes from string literal
	path = strings.Trim(path, "\"")
	return &UseDecl{
		nodeBase: b.baseFromContext(ctx),
		Alias:    b.termText(ctx.IDENT()),
		Path:     path,
	}
}

func (b *Builder) VisitDeclaration(ctx *parser.DeclarationContext) interface{} {
	if c := ctx.TypeDecl(); c != nil {
		return c.Accept(b)
	}
	if c := ctx.EnumDecl(); c != nil {
		return c.Accept(b)
	}
	if c := ctx.FuncDecl(); c != nil {
		return c.Accept(b)
	}
	return nil
}

func (b *Builder) VisitTypeDecl(ctx *parser.TypeDeclContext) interface{} {
	node := &TypeDecl{
		nodeBase: b.baseFromContext(ctx),
		Name:     b.termText(ctx.IDENT()),
	}
	if tp := ctx.TypeParams(); tp != nil {
		node.TypeParams = b.visitTypeParams(tp)
	}
	if inh := ctx.Inheritance(); inh != nil {
		node.Inherits = b.visitInheritance(inh)
	}
	for _, ma := range ctx.AllMetaAnnotation() {
		node.Annotations = append(node.Annotations, b.visitMetaAnnotation(ma))
	}
	for _, tm := range ctx.AllTypeMember() {
		b.visitTypeMember(tm, node)
	}
	return node
}

func (b *Builder) visitTypeMember(ctx parser.ITypeMemberContext, td *TypeDecl) {
	tc := ctx.(*parser.TypeMemberContext)
	if f := tc.FieldDecl(); f != nil {
		td.Fields = append(td.Fields, f.Accept(b).(*FieldDecl))
	}
	if m := tc.MethodDecl(); m != nil {
		td.Methods = append(td.Methods, m.Accept(b).(*MethodDecl))
	}
}

func (b *Builder) VisitFieldDecl(ctx *parser.FieldDeclContext) interface{} {
	node := &FieldDecl{
		nodeBase: b.baseFromContext(ctx),
		Type:     b.visitTypeRef(ctx.TypeRef()),
		Name:     b.termText(ctx.IDENT()),
	}
	for _, ma := range ctx.AllMetaAnnotation() {
		node.Annotations = append(node.Annotations, b.visitMetaAnnotation(ma))
	}
	if expr := ctx.Expression(); expr != nil {
		node.Default = b.visitExpr(expr)
	}
	return node
}

func (b *Builder) VisitMethodDecl(ctx *parser.MethodDeclContext) interface{} {
	node := &MethodDecl{
		nodeBase: b.baseFromContext(ctx),
		Name:     ctx.MethodName().Accept(b).(string),
	}
	if tp := ctx.TypeParams(); tp != nil {
		node.TypeParams = b.visitTypeParams(tp)
	}
	node.Receiver, node.Params = b.visitParams(ctx.Params())
	if rt := ctx.ReturnType(); rt != nil {
		node.ReturnType = rt.Accept(b).(*ReturnTypeSpec)
	}
	for _, ma := range ctx.AllMetaAnnotation() {
		node.Annotations = append(node.Annotations, b.visitMetaAnnotation(ma))
	}
	if blk := ctx.Block(); blk != nil {
		node.Body = b.visitBlock(blk)
	}
	return node
}

func (b *Builder) VisitMethodName(ctx *parser.MethodNameContext) interface{} {
	if id := ctx.IDENT(); id != nil {
		return id.GetText()
	}
	// Operator overloads
	if ctx.PLUS() != nil {
		return "+"
	}
	if ctx.MINUS() != nil {
		return "-"
	}
	if ctx.STAR() != nil {
		return "*"
	}
	if ctx.SLASH() != nil {
		return "/"
	}
	if ctx.PERCENT() != nil {
		return "%"
	}
	if ctx.EQ() != nil {
		return "=="
	}
	if ctx.NEQ() != nil {
		return "!="
	}
	if ctx.LT() != nil {
		return "<"
	}
	if ctx.GT() != nil {
		return ">"
	}
	if ctx.LTE() != nil {
		return "<="
	}
	if ctx.GTE() != nil {
		return ">="
	}
	return ""
}

func (b *Builder) VisitEnumDecl(ctx *parser.EnumDeclContext) interface{} {
	node := &EnumDecl{
		nodeBase: b.baseFromContext(ctx),
		Name:     b.termText(ctx.IDENT()),
	}
	if tp := ctx.TypeParams(); tp != nil {
		node.TypeParams = b.visitTypeParams(tp)
	}
	for _, ma := range ctx.AllMetaAnnotation() {
		node.Annotations = append(node.Annotations, b.visitMetaAnnotation(ma))
	}
	for _, ev := range ctx.AllEnumVariant() {
		node.Variants = append(node.Variants, ev.Accept(b).(*EnumVariant))
	}
	return node
}

func (b *Builder) VisitEnumVariant(ctx *parser.EnumVariantContext) interface{} {
	node := &EnumVariant{
		nodeBase: b.baseFromContext(ctx),
		Name:     b.termText(ctx.IDENT()),
	}
	for _, ef := range ctx.AllEnumField() {
		node.Fields = append(node.Fields, ef.Accept(b).(*EnumField))
	}
	return node
}

func (b *Builder) VisitEnumField(ctx *parser.EnumFieldContext) interface{} {
	return &EnumField{
		nodeBase: b.baseFromContext(ctx),
		Type:     b.visitTypeRef(ctx.TypeRef()),
		Name:     b.termText(ctx.IDENT()),
	}
}

func (b *Builder) VisitFuncDecl(ctx *parser.FuncDeclContext) interface{} {
	node := &FuncDecl{
		nodeBase: b.baseFromContext(ctx),
		Name:     b.termText(ctx.IDENT()),
	}
	if tp := ctx.TypeParams(); tp != nil {
		node.TypeParams = b.visitTypeParams(tp)
	}
	_, node.Params = b.visitParams(ctx.Params())
	if rt := ctx.ReturnType(); rt != nil {
		node.ReturnType = rt.Accept(b).(*ReturnTypeSpec)
	}
	for _, ma := range ctx.AllMetaAnnotation() {
		node.Annotations = append(node.Annotations, b.visitMetaAnnotation(ma))
	}
	node.Body = b.visitBlock(ctx.Block())
	return node
}

func (b *Builder) VisitReturnType(ctx *parser.ReturnTypeContext) interface{} {
	return &ReturnTypeSpec{
		nodeBase: b.baseFromContext(ctx),
		Type:     b.visitTypeRef(ctx.TypeRef()),
		CanError: ctx.BANG() != nil,
	}
}

func (b *Builder) visitTypeParams(ctx parser.ITypeParamsContext) []*TypeParam {
	tc := ctx.(*parser.TypeParamsContext)
	var params []*TypeParam
	for _, tp := range tc.AllTypeParam() {
		params = append(params, tp.Accept(b).(*TypeParam))
	}
	return params
}

func (b *Builder) VisitTypeParam(ctx *parser.TypeParamContext) interface{} {
	node := &TypeParam{
		nodeBase: b.baseFromContext(ctx),
		Name:     b.termText(ctx.IDENT()),
	}
	if tc := ctx.TypeConstraint(); tc != nil {
		tcc := tc.(*parser.TypeConstraintContext)
		for _, tr := range tcc.AllTypeRef() {
			node.Constraint = append(node.Constraint, b.visitTypeRef(tr))
		}
	}
	return node
}

func (b *Builder) visitInheritance(ctx parser.IInheritanceContext) []TypeRef {
	ic := ctx.(*parser.InheritanceContext)
	var types []TypeRef
	for _, tr := range ic.AllTypeRef() {
		types = append(types, b.visitTypeRef(tr))
	}
	return types
}

func (b *Builder) visitParams(ctx parser.IParamsContext) (*ReceiverParam, []*Param) {
	if ctx == nil {
		return nil, nil
	}
	pc := ctx.(*parser.ParamsContext)
	pl := pc.ParamList()
	if pl == nil {
		return nil, nil
	}
	plc := pl.(*parser.ParamListContext)

	var receiver *ReceiverParam
	if rp := plc.ReceiverParam(); rp != nil {
		receiver = rp.Accept(b).(*ReceiverParam)
	}

	var params []*Param
	for _, p := range plc.AllParam() {
		params = append(params, p.Accept(b).(*Param))
	}
	return receiver, params
}

func (b *Builder) VisitReceiverParam(ctx *parser.ReceiverParamContext) interface{} {
	node := &ReceiverParam{
		nodeBase: b.baseFromContext(ctx),
	}
	if rm := ctx.RefMod(); rm != nil {
		node.RefMod = b.visitRefMod(rm)
	}
	return node
}

func (b *Builder) VisitParam(ctx *parser.ParamContext) interface{} {
	node := &Param{
		nodeBase: b.baseFromContext(ctx),
		Type:     b.visitTypeRef(ctx.TypeRef()),
		Name:     b.bindingText(ctx.BindingName()),
	}
	if rm := ctx.RefMod(); rm != nil {
		node.RefMod = b.visitRefMod(rm)
	}
	if expr := ctx.Expression(); expr != nil {
		node.Default = b.visitExpr(expr)
	}
	return node
}

func (b *Builder) visitMetaAnnotation(ctx parser.IMetaAnnotationContext) *MetaAnnotation {
	mc := ctx.(*parser.MetaAnnotationContext)
	node := &MetaAnnotation{
		nodeBase: b.baseFromContext(mc),
		Name:     b.termText(mc.IDENT()),
	}
	if mp := mc.MetaParams(); mp != nil {
		mpc := mp.(*parser.MetaParamsContext)
		for _, p := range mpc.AllMetaParam() {
			node.Params = append(node.Params, p.Accept(b).(*MetaParam))
		}
	}
	return node
}

func (b *Builder) VisitMetaParam(ctx *parser.MetaParamContext) interface{} {
	node := &MetaParam{
		nodeBase: b.baseFromContext(ctx),
		Value:    b.visitExpr(ctx.Expression()),
	}
	if id := ctx.IDENT(); id != nil {
		node.Name = id.GetText()
	}
	return node
}
