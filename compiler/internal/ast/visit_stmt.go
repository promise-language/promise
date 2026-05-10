package ast

import "djabi.dev/go/promise_lang/internal/parser"

func (b *Builder) VisitBlock(ctx *parser.BlockContext) interface{} {
	node := &Block{nodeBase: b.baseFromContext(ctx)}
	for _, s := range ctx.AllStatement() {
		if stmt := b.visitStmt(s); stmt != nil {
			node.Stmts = append(node.Stmts, stmt)
		}
	}
	return node
}

func (b *Builder) VisitStatement(ctx *parser.StatementContext) interface{} {
	if c := ctx.UseVarDecl(); c != nil {
		return c.Accept(b)
	}
	if c := ctx.VarDecl(); c != nil {
		return c.Accept(b)
	}
	if c := ctx.ReturnStmt(); c != nil {
		return c.Accept(b)
	}
	if c := ctx.RaiseStmt(); c != nil {
		return c.Accept(b)
	}
	if c := ctx.YieldStmt(); c != nil {
		return c.Accept(b)
	}
	if c := ctx.YieldDelegateStmt(); c != nil {
		return c.Accept(b)
	}
	if c := ctx.BreakStmt(); c != nil {
		return c.Accept(b)
	}
	if c := ctx.ContinueStmt(); c != nil {
		return c.Accept(b)
	}
	if c := ctx.IfStmt(); c != nil {
		return c.Accept(b)
	}
	if c := ctx.ForStmt(); c != nil {
		return c.Accept(b)
	}
	if c := ctx.WhileStmt(); c != nil {
		return c.Accept(b)
	}
	if c := ctx.SelectStmt(); c != nil {
		return c.Accept(b)
	}
	if c := ctx.MatchExpr(); c != nil {
		return &ExprStmt{
			nodeBase: b.baseFromContext(ctx),
			Expr:     c.Accept(b).(Expr),
		}
	}
	if c := ctx.UnsafeBlock(); c != nil {
		return &ExprStmt{
			nodeBase: b.baseFromContext(ctx),
			Expr:     c.Accept(b).(Expr),
		}
	}
	if c := ctx.Block(); c != nil {
		return b.visitBlock(c)
	}
	if c := ctx.IncDecStmt(); c != nil {
		return c.Accept(b)
	}
	if c := ctx.AssignmentStmt(); c != nil {
		return c.Accept(b)
	}
	if c := ctx.ExpressionStmt(); c != nil {
		return c.Accept(b)
	}
	return nil
}

func (b *Builder) VisitTypedVarDecl(ctx *parser.TypedVarDeclContext) interface{} {
	node := &TypedVarDecl{
		nodeBase: b.baseFromContext(ctx),
		Type:     b.visitTypeRef(ctx.TypeRef()),
		Name:     b.bindingText(ctx.BindingName()),
	}
	if ctx.Expression() != nil {
		node.Value = b.visitExpr(ctx.Expression())
	}
	if rm := ctx.RefMod(); rm != nil {
		node.RefMod = b.visitRefMod(rm)
	}
	return node
}

func (b *Builder) VisitUseVarDecl(ctx *parser.UseVarDeclContext) interface{} {
	return &UseVarDecl{
		nodeBase: b.baseFromContext(ctx),
		Name:     ctx.IDENT().GetText(),
		Value:    b.visitExpr(ctx.Expression()),
	}
}

func (b *Builder) VisitInferredVarDecl(ctx *parser.InferredVarDeclContext) interface{} {
	return &InferredVarDecl{
		nodeBase: b.baseFromContext(ctx),
		Name:     b.bindingText(ctx.BindingName()),
		Value:    b.visitExpr(ctx.Expression()),
	}
}

func (b *Builder) VisitDestructureVarDecl(ctx *parser.DestructureVarDeclContext) interface{} {
	var names []string
	for _, bn := range ctx.AllBindingName() {
		names = append(names, b.bindingText(bn))
	}
	return &DestructureVarDecl{
		nodeBase: b.baseFromContext(ctx),
		Names:    names,
		Value:    b.visitExpr(ctx.Expression()),
	}
}

func (b *Builder) VisitAssignmentStmt(ctx *parser.AssignmentStmtContext) interface{} {
	exprs := ctx.AllExpression()
	return &AssignStmt{
		nodeBase: b.baseFromContext(ctx),
		Target:   b.visitExpr(exprs[0]),
		Op:       b.visitAssignOp(ctx.AssignOp()),
		Value:    b.visitExpr(exprs[1]),
	}
}

func (b *Builder) VisitIncDecStmt(ctx *parser.IncDecStmtContext) interface{} {
	return &IncDecStmt{
		nodeBase: b.baseFromContext(ctx),
		Target:   b.visitExpr(ctx.Expression()),
		IsInc:    ctx.PLUSPLUS() != nil,
	}
}

func (b *Builder) VisitReturnStmt(ctx *parser.ReturnStmtContext) interface{} {
	node := &ReturnStmt{nodeBase: b.baseFromContext(ctx)}
	if expr := ctx.Expression(); expr != nil {
		node.Value = b.visitExpr(expr)
	}
	return node
}

func (b *Builder) VisitRaiseStmt(ctx *parser.RaiseStmtContext) interface{} {
	return &RaiseStmt{
		nodeBase: b.baseFromContext(ctx),
		Value:    b.visitExpr(ctx.Expression()),
	}
}

func (b *Builder) VisitYieldStmt(ctx *parser.YieldStmtContext) interface{} {
	return &YieldStmt{
		nodeBase: b.baseFromContext(ctx),
		Value:    b.visitExpr(ctx.Expression()),
	}
}

func (b *Builder) VisitYieldDelegateStmt(ctx *parser.YieldDelegateStmtContext) interface{} {
	return &YieldDelegateStmt{
		nodeBase: b.baseFromContext(ctx),
		Value:    b.visitExpr(ctx.Expression()),
	}
}

func (b *Builder) VisitBreakStmt(ctx *parser.BreakStmtContext) interface{} {
	return &BreakStmt{nodeBase: b.baseFromContext(ctx)}
}

func (b *Builder) VisitContinueStmt(ctx *parser.ContinueStmtContext) interface{} {
	return &ContinueStmt{nodeBase: b.baseFromContext(ctx)}
}

func (b *Builder) VisitExpressionStmt(ctx *parser.ExpressionStmtContext) interface{} {
	return &ExprStmt{
		nodeBase: b.baseFromContext(ctx),
		Expr:     b.visitExpr(ctx.Expression()),
	}
}

func (b *Builder) VisitIfStmt(ctx *parser.IfStmtContext) interface{} {
	node := &IfStmt{nodeBase: b.baseFromContext(ctx)}
	cond := ctx.IfCondition()
	switch c := cond.(type) {
	case *parser.IfUnwrapCondContext:
		node.Binding = b.bindingText(c.BindingName())
		node.Init = b.visitExpr(c.Expression())
	case *parser.IfExprCondContext:
		node.Cond = b.visitExpr(c.Expression())
	}
	node.Body = b.visitBlock(ctx.Block())
	if ec := ctx.ElseClause(); ec != nil {
		node.Else = ec.Accept(b).(Stmt)
	}
	return node
}

func (b *Builder) VisitElseClause(ctx *parser.ElseClauseContext) interface{} {
	if is := ctx.IfStmt(); is != nil {
		return is.Accept(b)
	}
	return b.visitBlock(ctx.Block())
}

func (b *Builder) VisitForInStmt(ctx *parser.ForInStmtContext) interface{} {
	node := &ForInStmt{
		nodeBase: b.baseFromContext(ctx),
		Iterable: b.visitExpr(ctx.Expression()),
		Body:     b.visitBlock(ctx.Block()),
	}
	bindings := ctx.AllBindingName()
	if len(bindings) == 1 {
		node.Binding = b.bindingText(bindings[0])
	} else if len(bindings) == 2 {
		node.Index = b.bindingText(bindings[0])
		node.Binding = b.bindingText(bindings[1])
	}
	return node
}

func (b *Builder) VisitClassicForStmt(ctx *parser.ClassicForStmtContext) interface{} {
	node := &ClassicForStmt{
		nodeBase: b.baseFromContext(ctx),
		Cond:     b.visitExpr(ctx.Expression()),
		Body:     b.visitBlock(ctx.Block()),
	}

	// Handle init
	forInit := ctx.ForInit()
	switch fi := forInit.(type) {
	case *parser.ForInitTypedContext:
		node.InitName = b.termText(fi.IDENT())
		node.InitType = b.visitTypeRef(fi.TypeRef())
		node.InitValue = b.visitExpr(fi.Expression())
	case *parser.ForInitInferredContext:
		node.InitName = b.termText(fi.IDENT())
		node.InitValue = b.visitExpr(fi.Expression())
	}

	// Handle update
	forUpdate := ctx.ForUpdate()
	switch fu := forUpdate.(type) {
	case *parser.ForUpdateAssignContext:
		exprs := fu.AllExpression()
		node.UpdateTarget = b.visitExpr(exprs[0])
		node.UpdateOp = b.visitAssignOp(fu.AssignOp())
		node.UpdateValue = b.visitExpr(exprs[1])
	case *parser.ForUpdateIncDecContext:
		node.UpdateTarget = b.visitExpr(fu.Expression())
		node.UpdateIncDec = true
		node.UpdateIsInc = fu.PLUSPLUS() != nil
	case *parser.ForUpdateExprContext:
		node.UpdateValue = b.visitExpr(fu.Expression())
	}

	return node
}

func (b *Builder) VisitInfiniteLoopStmt(ctx *parser.InfiniteLoopStmtContext) interface{} {
	return &InfiniteLoop{
		nodeBase: b.baseFromContext(ctx),
		Body:     b.visitBlock(ctx.Block()),
	}
}

func (b *Builder) VisitWhileExprStmt(ctx *parser.WhileExprStmtContext) interface{} {
	return &WhileStmt{
		nodeBase: b.baseFromContext(ctx),
		Cond:     b.visitExpr(ctx.Expression()),
		Body:     b.visitBlock(ctx.Block()),
	}
}

func (b *Builder) VisitWhileUnwrapStmt(ctx *parser.WhileUnwrapStmtContext) interface{} {
	return &WhileUnwrapStmt{
		nodeBase: b.baseFromContext(ctx),
		Binding:  b.bindingText(ctx.BindingName()),
		Value:    b.visitExpr(ctx.Expression()),
		Body:     b.visitBlock(ctx.Block()),
	}
}

func (b *Builder) VisitSelectStmt(ctx *parser.SelectStmtContext) interface{} {
	node := &SelectStmt{nodeBase: b.baseFromContext(ctx)}
	for _, sc := range ctx.AllSelectCase() {
		node.Cases = append(node.Cases, sc.Accept(b).(*SelectCase))
	}
	if sd := ctx.SelectDefault(); sd != nil {
		node.Default = sd.Accept(b).([]Stmt)
	}
	return node
}

func (b *Builder) VisitSelectCase(ctx *parser.SelectCaseContext) interface{} {
	node := &SelectCase{nodeBase: b.baseFromContext(ctx)}
	if ctx.BindingName() != nil {
		// Receive case: binding := <-ch
		node.IsSend = false
		node.Binding = b.bindingText(ctx.BindingName())
		// The receive expression is the last expression (after <- )
		exprs := ctx.AllExpression()
		node.Channel = b.visitExpr(exprs[0])
	} else {
		// Send case: ch.send(v)
		node.IsSend = true
		exprs := ctx.AllExpression()
		node.Channel = b.visitExpr(exprs[0])
		node.SendValue = b.visitExpr(exprs[1])
	}
	// Parse body statements
	for _, s := range ctx.AllStatement() {
		if stmt := b.visitStmt(s); stmt != nil {
			node.Body = append(node.Body, stmt)
		}
	}
	return node
}

func (b *Builder) VisitSelectDefault(ctx *parser.SelectDefaultContext) interface{} {
	stmts := []Stmt{}
	for _, s := range ctx.AllStatement() {
		if stmt := b.visitStmt(s); stmt != nil {
			stmts = append(stmts, stmt)
		}
	}
	return stmts
}
