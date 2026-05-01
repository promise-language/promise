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

func (b *Builder) VisitCatalogImport(ctx *parser.CatalogImportContext) interface{} {
	name := b.termText(ctx.IDENT())
	alias := name // default: alias == catalog name
	if ctx.AS() != nil {
		if bn := ctx.BindingName(); bn != nil {
			alias = b.bindingText(bn)
		}
	}
	return &UseDecl{
		nodeBase:    b.baseFromContext(ctx),
		Alias:       alias,
		CatalogName: name,
	}
}

func (b *Builder) VisitSourcedImport(ctx *parser.SourcedImportContext) interface{} {
	alias := b.bindingText(ctx.BindingName())
	path := ""
	if sl := ctx.StringLiteral(); sl != nil {
		lit := b.visitStringLiteral(sl)
		if lit != nil {
			path = lit.Raw
		}
	}
	path = strings.Trim(path, "\"")
	return &UseDecl{
		nodeBase: b.baseFromContext(ctx),
		Alias:    alias,
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
	if g := tc.GetterDecl(); g != nil {
		td.Methods = append(td.Methods, g.Accept(b).(*MethodDecl))
	}
	if s := tc.SetterDecl(); s != nil {
		td.Methods = append(td.Methods, s.Accept(b).(*MethodDecl))
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
	if mb := ctx.MemberBody(); mb != nil {
		node.Body = b.visitMemberBody(mb, true)
	}
	return node
}

func (b *Builder) VisitGetterDecl(ctx *parser.GetterDeclContext) interface{} {
	keyword := ctx.IDENT(0).GetText()
	if keyword != "get" {
		panic("expected 'get' keyword in getter declaration, got '" + keyword + "'")
	}
	node := &MethodDecl{
		nodeBase: b.baseFromContext(ctx),
		Name:     ctx.IDENT(1).GetText(),
		IsGetter: true,
	}
	node.ReturnType = &ReturnTypeSpec{
		nodeBase: b.baseFromContext(ctx),
		Type:     b.visitTypeRef(ctx.TypeRef()),
	}
	for _, ma := range ctx.AllMetaAnnotation() {
		node.Annotations = append(node.Annotations, b.visitMetaAnnotation(ma))
	}
	if mb := ctx.MemberBody(); mb != nil {
		node.Body = b.visitMemberBody(mb, true)
	}
	return node
}

func (b *Builder) VisitSetterDecl(ctx *parser.SetterDeclContext) interface{} {
	keyword := ctx.IDENT(0).GetText()
	if keyword != "set" {
		panic("expected 'set' keyword in setter declaration, got '" + keyword + "'")
	}
	node := &MethodDecl{
		nodeBase: b.baseFromContext(ctx),
		Name:     ctx.IDENT(1).GetText(),
		IsSetter: true,
		Params: []*Param{{
			nodeBase: b.baseFromContext(ctx),
			Type:     b.visitTypeRef(ctx.TypeRef()),
			Name:     ctx.IDENT(2).GetText(),
		}},
	}
	for _, ma := range ctx.AllMetaAnnotation() {
		node.Annotations = append(node.Annotations, b.visitMetaAnnotation(ma))
	}
	if mb := ctx.MemberBody(); mb != nil {
		node.Body = b.visitMemberBody(mb, false)
	}
	return node
}

// visitMemberBody handles both block body and expression body (=> expr;).
// For expression body: if isReturn is true, wraps in ReturnStmt; otherwise wraps in ExprStmt.
func (b *Builder) visitMemberBody(ctx parser.IMemberBodyContext, isReturn bool) *Block {
	mbc := ctx.(*parser.MemberBodyContext)
	if blk := mbc.Block(); blk != nil {
		return b.visitBlock(blk)
	}
	// Expression body: => expr;
	expr := b.visitExpr(mbc.Expression())
	base := b.baseFromContext(mbc)
	var stmt Stmt
	if isReturn {
		stmt = &ReturnStmt{nodeBase: base, Value: expr}
	} else {
		stmt = &ExprStmt{nodeBase: base, Expr: expr}
	}
	return &Block{nodeBase: base, Stmts: []Stmt{stmt}}
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
	if ctx.AND() != nil {
		return "&&"
	}
	if ctx.OR() != nil {
		return "||"
	}
	if ctx.BANG() != nil {
		return "!"
	}
	if ctx.PLUSPLUS() != nil {
		return "++"
	}
	if ctx.MINUSMINUS() != nil {
		return "--"
	}
	if ctx.DOTDOT() != nil {
		return ".."
	}
	if ctx.DOTDOTEQ() != nil {
		return "..="
	}
	if ctx.AMP() != nil {
		return "&"
	}
	if ctx.PIPE() != nil {
		return "|"
	}
	if ctx.CARET() != nil {
		return "^"
	}
	if ctx.LSHIFT() != nil {
		return "<<"
	}
	if ctx.RSHIFT() != nil {
		return ">>"
	}
	if ctx.TILDE() != nil {
		return "~"
	}
	// Bracket operators: check COLON before plain brackets
	if ctx.LBRACKET() != nil && ctx.RBRACKET() != nil {
		if ctx.COLON() != nil {
			if ctx.ASSIGN() != nil {
				return "[:]="
			}
			return "[:]"
		}
		if ctx.ASSIGN() != nil {
			return "[]="
		}
		return "[]"
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
	spec := &ReturnTypeSpec{
		nodeBase: b.baseFromContext(ctx),
		CanError: ctx.BANG() != nil,
	}
	if tr := ctx.TypeRef(); tr != nil {
		spec.Type = b.visitTypeRef(tr)
	}
	return spec
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
	for _, ma := range ctx.AllMetaAnnotation() {
		node.Annotations = append(node.Annotations, b.visitMetaAnnotation(ma))
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
