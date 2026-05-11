package ast

import (
	"strings"

	"djabi.dev/go/promise_lang/internal/parser"
	antlr "github.com/antlr4-go/antlr/v4"
)

// Binary expression visitors

func (b *Builder) VisitElvisExpr(ctx *parser.ElvisExprContext) interface{} {
	exprs := ctx.AllExpression()
	return &BinaryExpr{
		nodeBase: b.baseFromContext(ctx),
		Left:     b.visitExpr(exprs[0]),
		Op:       BinElvis,
		Right:    b.visitExpr(exprs[1]),
	}
}

func (b *Builder) VisitLogicalOrExpr(ctx *parser.LogicalOrExprContext) interface{} {
	exprs := ctx.AllExpression()
	return &BinaryExpr{
		nodeBase: b.baseFromContext(ctx),
		Left:     b.visitExpr(exprs[0]),
		Op:       BinOr,
		Right:    b.visitExpr(exprs[1]),
	}
}

func (b *Builder) VisitLogicalAndExpr(ctx *parser.LogicalAndExprContext) interface{} {
	exprs := ctx.AllExpression()
	return &BinaryExpr{
		nodeBase: b.baseFromContext(ctx),
		Left:     b.visitExpr(exprs[0]),
		Op:       BinAnd,
		Right:    b.visitExpr(exprs[1]),
	}
}

func (b *Builder) VisitEqualityExpr(ctx *parser.EqualityExprContext) interface{} {
	exprs := ctx.AllExpression()
	op := BinEq
	if ctx.NEQ() != nil {
		op = BinNeq
	}
	return &BinaryExpr{
		nodeBase: b.baseFromContext(ctx),
		Left:     b.visitExpr(exprs[0]),
		Op:       op,
		Right:    b.visitExpr(exprs[1]),
	}
}

func (b *Builder) VisitComparisonExpr(ctx *parser.ComparisonExprContext) interface{} {
	exprs := ctx.AllExpression()
	var op BinaryOp
	switch {
	case ctx.LT() != nil:
		op = BinLt
	case ctx.GT() != nil:
		op = BinGt
	case ctx.LTE() != nil:
		op = BinLte
	case ctx.GTE() != nil:
		op = BinGte
	}
	return &BinaryExpr{
		nodeBase: b.baseFromContext(ctx),
		Left:     b.visitExpr(exprs[0]),
		Op:       op,
		Right:    b.visitExpr(exprs[1]),
	}
}

func (b *Builder) VisitAdditiveExpr(ctx *parser.AdditiveExprContext) interface{} {
	exprs := ctx.AllExpression()
	op := BinAdd
	if ctx.MINUS() != nil {
		op = BinSub
	}
	return &BinaryExpr{
		nodeBase: b.baseFromContext(ctx),
		Left:     b.visitExpr(exprs[0]),
		Op:       op,
		Right:    b.visitExpr(exprs[1]),
	}
}

func (b *Builder) VisitMultiplicativeExpr(ctx *parser.MultiplicativeExprContext) interface{} {
	exprs := ctx.AllExpression()
	var op BinaryOp
	switch {
	case ctx.STAR() != nil:
		op = BinMul
	case ctx.SLASH() != nil:
		op = BinDiv
	case ctx.PERCENT() != nil:
		op = BinMod
	}
	return &BinaryExpr{
		nodeBase: b.baseFromContext(ctx),
		Left:     b.visitExpr(exprs[0]),
		Op:       op,
		Right:    b.visitExpr(exprs[1]),
	}
}

func (b *Builder) VisitShiftExpr(ctx *parser.ShiftExprContext) interface{} {
	exprs := ctx.AllExpression()
	op := BinLeftShift
	if ctx.RSHIFT() != nil {
		op = BinRightShift
	}
	return &BinaryExpr{
		nodeBase: b.baseFromContext(ctx),
		Left:     b.visitExpr(exprs[0]),
		Op:       op,
		Right:    b.visitExpr(exprs[1]),
	}
}

func (b *Builder) VisitBitwiseExpr(ctx *parser.BitwiseExprContext) interface{} {
	exprs := ctx.AllExpression()
	var op BinaryOp
	switch {
	case ctx.AMP() != nil:
		op = BinBitwiseAnd
	case ctx.CARET() != nil:
		op = BinBitwiseXor
	case ctx.PIPE() != nil:
		op = BinBitwiseOr
	}
	return &BinaryExpr{
		nodeBase: b.baseFromContext(ctx),
		Left:     b.visitExpr(exprs[0]),
		Op:       op,
		Right:    b.visitExpr(exprs[1]),
	}
}

func (b *Builder) VisitExclusiveRangeExpr(ctx *parser.ExclusiveRangeExprContext) interface{} {
	exprs := ctx.AllExpression()
	return &BinaryExpr{
		nodeBase: b.baseFromContext(ctx),
		Left:     b.visitExpr(exprs[0]),
		Op:       BinExclusiveRange,
		Right:    b.visitExpr(exprs[1]),
	}
}

func (b *Builder) VisitInclusiveRangeExpr(ctx *parser.InclusiveRangeExprContext) interface{} {
	exprs := ctx.AllExpression()
	return &BinaryExpr{
		nodeBase: b.baseFromContext(ctx),
		Left:     b.visitExpr(exprs[0]),
		Op:       BinInclusiveRange,
		Right:    b.visitExpr(exprs[1]),
	}
}

// Unary expression visitors

func (b *Builder) VisitUnaryNegExpr(ctx *parser.UnaryNegExprContext) interface{} {
	return &UnaryExpr{
		nodeBase: b.baseFromContext(ctx),
		Op:       UnaryNeg,
		Operand:  b.visitExpr(ctx.Expression()),
	}
}

func (b *Builder) VisitUnaryNotExpr(ctx *parser.UnaryNotExprContext) interface{} {
	return &UnaryExpr{
		nodeBase: b.baseFromContext(ctx),
		Op:       UnaryNot,
		Operand:  b.visitExpr(ctx.Expression()),
	}
}

func (b *Builder) VisitBitwiseNotExpr(ctx *parser.BitwiseNotExprContext) interface{} {
	return &UnaryExpr{
		nodeBase: b.baseFromContext(ctx),
		Op:       UnaryBitwiseNot,
		Operand:  b.visitExpr(ctx.Expression()),
	}
}

func (b *Builder) VisitReceiveExpr(ctx *parser.ReceiveExprContext) interface{} {
	return &UnaryExpr{
		nodeBase: b.baseFromContext(ctx),
		Op:       UnaryReceive,
		Operand:  b.visitExpr(ctx.Expression()),
	}
}

// Postfix expression visitors

func (b *Builder) VisitMemberAccessExpr(ctx *parser.MemberAccessExprContext) interface{} {
	return &MemberExpr{
		nodeBase: b.baseFromContext(ctx),
		Target:   b.visitExpr(ctx.Expression()),
		Field:    b.termText(ctx.IDENT()),
	}
}

func (b *Builder) VisitOptionalChainExpr(ctx *parser.OptionalChainExprContext) interface{} {
	return &OptionalChainExpr{
		nodeBase: b.baseFromContext(ctx),
		Target:   b.visitExpr(ctx.Expression()),
		Field:    b.termText(ctx.IDENT()),
	}
}

func (b *Builder) VisitCallExpr(ctx *parser.CallExprContext) interface{} {
	return &CallExpr{
		nodeBase: b.baseFromContext(ctx),
		Callee:   b.visitExpr(ctx.Expression()),
		Args:     b.visitArgs(ctx.Args()),
	}
}

func (b *Builder) VisitIndexExpr(ctx *parser.IndexExprContext) interface{} {
	exprs := ctx.AllExpression()
	node := &IndexExpr{
		nodeBase: b.baseFromContext(ctx),
		Target:   b.visitExpr(exprs[0]),
		Index:    b.visitExpr(exprs[1]),
	}
	for i := 2; i < len(exprs); i++ {
		node.ExtraIndices = append(node.ExtraIndices, b.visitExpr(exprs[i]))
	}
	return node
}

func (b *Builder) VisitSliceExpr(ctx *parser.SliceExprContext) interface{} {
	exprs := ctx.AllExpression()
	node := &SliceExpr{
		nodeBase: b.baseFromContext(ctx),
		Target:   b.visitExpr(exprs[0]),
	}
	colonPos := ctx.COLON().GetSymbol().GetTokenIndex()
	for _, e := range exprs[1:] {
		ec := e.(antlr.ParserRuleContext)
		if ec.GetStart().GetTokenIndex() < colonPos {
			node.Low = b.visitExpr(e)
		} else {
			node.High = b.visitExpr(e)
		}
	}
	return node
}

func (b *Builder) VisitSliceTypeExpr(ctx *parser.SliceTypeExprContext) interface{} {
	return &SliceTypeExpr{
		nodeBase: b.baseFromContext(ctx),
		Inner:    b.visitExpr(ctx.Expression()),
	}
}

func (b *Builder) VisitErrorHandlerExpr(ctx *parser.ErrorHandlerExprContext) interface{} {
	node := &ErrorHandlerExpr{
		nodeBase: b.baseFromContext(ctx),
		Expr:     b.visitExpr(ctx.Expression(0)),
	}
	bindings := ctx.AllBindingName()
	if len(bindings) > 0 {
		node.Binding = b.bindingText(bindings[0])
	}
	if ctx.IS() != nil {
		node.TypeName = ctx.IDENT().GetText()
		if ta := ctx.TypeArgs(); ta != nil {
			tac := ta.(*parser.TypeArgsContext)
			for _, tr := range tac.AllTypeRef() {
				node.TypeArgs = append(node.TypeArgs, b.visitTypeRef(tr))
			}
		}
	}
	if ctx.FAT_ARROW() != nil {
		// Arrow form: ? e => expr — wrap in synthetic block with ExprStmt
		expr := b.visitExpr(ctx.Expression(1))
		base := b.baseFromContext(ctx)
		node.Body = &Block{nodeBase: base, Stmts: []Stmt{&ExprStmt{nodeBase: base, Expr: expr}}}
	} else {
		// Block form: ? e { ... }
		node.Body = b.visitBlock(ctx.Block(0))
		if ctx.ELSE() != nil {
			node.ElseBody = b.visitBlock(ctx.Block(1))
			if len(bindings) > 1 {
				node.ElseBinding = b.bindingText(bindings[1])
			}
		}
		if ctx.BANG() != nil {
			node.PanicOnNomatch = true
		}
	}
	return node
}

func (b *Builder) VisitErrorPropagateExpr(ctx *parser.ErrorPropagateExprContext) interface{} {
	return &ErrorPropagateExpr{
		nodeBase: b.baseFromContext(ctx),
		Expr:     b.visitExpr(ctx.Expression()),
	}
}

func (b *Builder) VisitErrorPanicExpr(ctx *parser.ErrorPanicExprContext) interface{} {
	return &ErrorPanicExpr{
		nodeBase: b.baseFromContext(ctx),
		Expr:     b.visitExpr(ctx.Expression()),
	}
}

func (b *Builder) VisitOptionalUnwrapExpr(ctx *parser.OptionalUnwrapExprContext) interface{} {
	return &OptionalUnwrapExpr{
		nodeBase: b.baseFromContext(ctx),
		Expr:     b.visitExpr(ctx.Expression()),
	}
}

// Type operation visitors

func (b *Builder) VisitIsExpr(ctx *parser.IsExprContext) interface{} {
	return &IsExpr{
		nodeBase: b.baseFromContext(ctx),
		Expr:     b.visitExpr(ctx.Expression()),
		Pattern:  b.visitIsPattern(ctx.Pattern()),
	}
}

func (b *Builder) VisitCastExpr(ctx *parser.CastExprContext) interface{} {
	return &CastExpr{
		nodeBase: b.baseFromContext(ctx),
		Expr:     b.visitExpr(ctx.Expression()),
		Type:     b.visitTypeRef(ctx.TypeRef()),
		Force:    ctx.BANG() != nil,
	}
}

// Primary expression visitors

func (b *Builder) VisitPrimaryExpr(ctx *parser.PrimaryExprContext) interface{} {
	return ctx.Primary().Accept(b)
}

func (b *Builder) VisitIntLiteral(ctx *parser.IntLiteralContext) interface{} {
	raw, suffix := splitNumericSuffix(ctx.INT_LITERAL().GetText())
	return &IntLit{
		nodeBase: b.baseFromContext(ctx),
		Raw:      raw,
		Suffix:   suffix,
	}
}

func (b *Builder) VisitFloatLiteral(ctx *parser.FloatLiteralContext) interface{} {
	raw, suffix := splitNumericSuffix(ctx.FLOAT_LITERAL().GetText())
	return &FloatLit{
		nodeBase: b.baseFromContext(ctx),
		Raw:      raw,
		Suffix:   suffix,
	}
}

func (b *Builder) VisitTrueLiteral(ctx *parser.TrueLiteralContext) interface{} {
	return &BoolLit{
		nodeBase: b.baseFromContext(ctx),
		Value:    true,
	}
}

func (b *Builder) VisitFalseLiteral(ctx *parser.FalseLiteralContext) interface{} {
	return &BoolLit{
		nodeBase: b.baseFromContext(ctx),
		Value:    false,
	}
}

func (b *Builder) VisitNoneLiteral(ctx *parser.NoneLiteralContext) interface{} {
	return &NoneLit{nodeBase: b.baseFromContext(ctx)}
}

func (b *Builder) VisitCharLiteral(ctx *parser.CharLiteralContext) interface{} {
	return &CharLit{
		nodeBase: b.baseFromContext(ctx),
		Raw:      ctx.CHAR_LITERAL().GetText(),
	}
}

func (b *Builder) VisitStringLit(ctx *parser.StringLitContext) interface{} {
	return ctx.StringLiteral().Accept(b)
}

func (b *Builder) VisitStringLiteral(ctx *parser.StringLiteralContext) interface{} {
	node := &StringLit{nodeBase: b.baseFromContext(ctx)}
	node.Raw = ctx.GetText()

	if rs := ctx.RAW_STRING(); rs != nil {
		node.Kind = StringRaw
		return node
	}
	if ts := ctx.TRIPLE_STRING(); ts != nil {
		node.Kind = StringTriple
		return node
	}

	// Regular interpolated string
	node.Kind = StringRegular
	for _, sp := range ctx.AllStringPart() {
		spc := sp.(*parser.StringPartContext)
		if t := spc.STRING_TEXT(); t != nil {
			node.Parts = append(node.Parts, StringText{Text: t.GetText()})
		} else if e := spc.STRING_ESCAPE(); e != nil {
			node.Parts = append(node.Parts, StringEscape{Sequence: e.GetText()})
		} else if i := spc.STRING_INTERP(); i != nil {
			raw := i.GetText()
			exprText := raw[1 : len(raw)-1] // strip { }
			tok := i.GetSymbol()
			expr := b.parseInterpolationExpr(exprText, tok.GetLine(), tok.GetColumn()+1)
			node.Parts = append(node.Parts, StringInterp{Raw: raw, Expr: expr})
		}
	}
	return node
}

func (b *Builder) VisitIdentExpr(ctx *parser.IdentExprContext) interface{} {
	return &IdentExpr{
		nodeBase: b.baseFromContext(ctx),
		Name:     ctx.IDENT().GetText(),
	}
}

func (b *Builder) VisitThisExpr(ctx *parser.ThisExprContext) interface{} {
	return &ThisExpr{nodeBase: b.baseFromContext(ctx)}
}

func (b *Builder) VisitParenExpr(ctx *parser.ParenExprContext) interface{} {
	return &ParenExpr{
		nodeBase: b.baseFromContext(ctx),
		Expr:     b.visitExpr(ctx.Expression()),
	}
}

func (b *Builder) VisitTupleLiteral(ctx *parser.TupleLiteralContext) interface{} {
	node := &TupleLit{nodeBase: b.baseFromContext(ctx)}
	for _, e := range ctx.AllExpression() {
		node.Elements = append(node.Elements, b.visitExpr(e))
	}
	return node
}

func (b *Builder) VisitArrayLiteral(ctx *parser.ArrayLiteralContext) interface{} {
	node := &ArrayLit{nodeBase: b.baseFromContext(ctx)}
	for _, e := range ctx.AllExpression() {
		node.Elements = append(node.Elements, b.visitExpr(e))
	}
	return node
}

func (b *Builder) VisitMapLiteral(ctx *parser.MapLiteralContext) interface{} {
	node := &MapLit{nodeBase: b.baseFromContext(ctx)}
	for _, me := range ctx.AllMapEntry() {
		node.Entries = append(node.Entries, me.Accept(b).(*MapEntry))
	}
	return node
}

func (b *Builder) VisitMapEntry(ctx *parser.MapEntryContext) interface{} {
	exprs := ctx.AllExpression()
	return &MapEntry{
		nodeBase: b.baseFromContext(ctx),
		Key:      b.visitExpr(exprs[0]),
		Value:    b.visitExpr(exprs[1]),
	}
}

// Complex expression visitors

func (b *Builder) VisitLambda(ctx *parser.LambdaContext) interface{} {
	return ctx.LambdaExpr().Accept(b)
}

func (b *Builder) VisitLambdaExpr(ctx *parser.LambdaExprContext) interface{} {
	node := &LambdaExpr{
		nodeBase: b.baseFromContext(ctx),
		Move:     ctx.MOVE() != nil,
	}

	// Parse params
	if lp := ctx.LambdaParams(); lp != nil {
		lpc := lp.(*parser.LambdaParamsContext)
		for _, p := range lpc.AllLambdaParam() {
			node.Params = append(node.Params, p.Accept(b).(*LambdaParam))
		}
	}

	// Return type annotation (only in PIPE forms with ARROW + typeRef before block)
	if tr := ctx.TypeRef(); tr != nil {
		node.ReturnType = b.visitTypeRef(tr)
	}

	// Body: block or expression
	if blk := ctx.Block(); blk != nil {
		node.Body = b.visitBlock(blk)
	} else if expr := ctx.Expression(); expr != nil {
		node.ExprBody = b.visitExpr(expr)
	}

	return node
}

func (b *Builder) VisitTypedLambdaParam(ctx *parser.TypedLambdaParamContext) interface{} {
	node := &LambdaParam{
		nodeBase: b.baseFromContext(ctx),
		Type:     b.visitTypeRef(ctx.TypeRef()),
		Name:     b.bindingText(ctx.BindingName()),
	}
	if rm := ctx.RefMod(); rm != nil {
		node.RefMod = b.visitRefMod(rm)
	}
	return node
}

func (b *Builder) VisitUntypedLambdaParam(ctx *parser.UntypedLambdaParamContext) interface{} {
	return &LambdaParam{
		nodeBase: b.baseFromContext(ctx),
		Name:     b.bindingText(ctx.BindingName()),
	}
}

func (b *Builder) VisitIfExpression(ctx *parser.IfExpressionContext) interface{} {
	return ctx.IfExpr().Accept(b)
}

func (b *Builder) VisitIfExpr(ctx *parser.IfExprContext) interface{} {
	blocks := ctx.AllBlock()
	return &IfExpr{
		nodeBase: b.baseFromContext(ctx),
		Cond:     b.visitExpr(ctx.Expression()),
		Then:     b.visitBlock(blocks[0]),
		Else:     b.visitBlock(blocks[1]),
	}
}

func (b *Builder) VisitMatchExpression(ctx *parser.MatchExpressionContext) interface{} {
	return ctx.MatchExpr().Accept(b)
}

func (b *Builder) VisitMatchExpr(ctx *parser.MatchExprContext) interface{} {
	node := &MatchExpr{
		nodeBase: b.baseFromContext(ctx),
		Subject:  b.visitExpr(ctx.Expression()),
	}
	for _, ma := range ctx.AllMatchArm() {
		node.Arms = append(node.Arms, ma.Accept(b).(*MatchArm))
	}
	return node
}

func (b *Builder) VisitMatchArm(ctx *parser.MatchArmContext) interface{} {
	node := &MatchArm{
		nodeBase: b.baseFromContext(ctx),
		Pattern:  b.visitMatchPattern(ctx.MatchPattern()),
	}
	exprs := ctx.AllExpression()
	if ctx.IF() != nil && len(exprs) >= 1 {
		// First expression is the guard
		node.Guard = b.visitExpr(exprs[0])
		// Body: block or second expression
		if blk := ctx.Block(); blk != nil {
			node.Block = b.visitBlock(blk)
		} else if len(exprs) >= 2 {
			node.Body = b.visitExpr(exprs[1])
		}
	} else {
		// No guard
		if blk := ctx.Block(); blk != nil {
			node.Block = b.visitBlock(blk)
		} else if len(exprs) >= 1 {
			node.Body = b.visitExpr(exprs[0])
		}
	}
	return node
}

func (b *Builder) VisitGoExpression(ctx *parser.GoExpressionContext) interface{} {
	return ctx.GoExpr().Accept(b)
}

func (b *Builder) VisitGoExpr(ctx *parser.GoExprContext) interface{} {
	node := &GoExpr{nodeBase: b.baseFromContext(ctx)}
	if blk := ctx.Block(); blk != nil {
		node.Block = b.visitBlock(blk)
	} else if expr := ctx.Expression(); expr != nil {
		node.Expr = b.visitExpr(expr)
	}
	return node
}

func (b *Builder) VisitUnsafeExpression(ctx *parser.UnsafeExpressionContext) interface{} {
	return ctx.UnsafeBlock().Accept(b)
}

func (b *Builder) VisitUnsafeBlock(ctx *parser.UnsafeBlockContext) interface{} {
	return &UnsafeExpr{
		nodeBase: b.baseFromContext(ctx),
		Body:     b.visitBlock(ctx.Block()),
	}
}

// Argument helpers

func (b *Builder) visitArgs(ctx parser.IArgsContext) []*Arg {
	if ctx == nil {
		return nil
	}
	ac := ctx.(*parser.ArgsContext)
	al := ac.ArgList()
	if al == nil {
		return nil
	}
	alc := al.(*parser.ArgListContext)
	var args []*Arg
	for _, a := range alc.AllArg() {
		args = append(args, a.Accept(b).(*Arg))
	}
	return args
}

func (b *Builder) VisitArg(ctx *parser.ArgContext) interface{} {
	node := &Arg{
		nodeBase: b.baseFromContext(ctx),
		Value:    b.visitExpr(ctx.Expression()),
	}
	if id := ctx.IDENT(); id != nil {
		node.Name = id.GetText()
	}
	return node
}

// interpErrorListener detects syntax errors during interpolation expression re-parsing.
type interpErrorListener struct {
	antlr.DefaultErrorListener
	hasErrors bool
}

func (l *interpErrorListener) SyntaxError(
	_ antlr.Recognizer, _ interface{}, _, _ int, _ string, _ antlr.RecognitionException,
) {
	l.hasErrors = true
}

// parseInterpolationExpr re-lexes/re-parses the text between {} in a string interpolation.
// outerLine and outerCol are the position of the expression text in the original source file,
// used to offset the re-parsed AST node positions.
func (b *Builder) parseInterpolationExpr(text string, outerLine, outerCol int) Expr {
	if strings.TrimSpace(text) == "" {
		return nil // empty interpolation — sema reports the error
	}
	input := antlr.NewInputStream(text)
	lexer := parser.NewPromiseLexer(input)
	el := &interpErrorListener{}
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(el)
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	p.AddErrorListener(el)
	tree := p.Expression()
	if el.hasErrors || stream.LT(1).GetTokenType() != antlr.TokenEOF {
		pos := Pos{File: b.filename, Line: outerLine, Column: outerCol}
		b.errorf(pos, "invalid expression in string interpolation")
		return nil
	}
	result := tree.Accept(b)
	if expr, ok := result.(Expr); ok {
		offsetExprPositions(expr, outerLine-1, outerCol)
		return expr
	}
	return nil
}

// offsetExprPositions adjusts the position of an expression node to account for
// being parsed from a string interpolation context. lineOffset is added to line numbers,
// and colOffset is added to column numbers for nodes on the first line (line 1).
func offsetExprPositions(expr Expr, lineOffset, colOffset int) {
	if expr == nil {
		return
	}
	adjustPos := func(p *Pos) {
		if p.Line == 1 {
			p.Column += colOffset
		}
		p.Line += lineOffset
	}
	switch e := expr.(type) {
	case *IdentExpr:
		adjustPos(&e.pos)
		adjustPos(&e.end)
	case *IntLit:
		adjustPos(&e.pos)
		adjustPos(&e.end)
	case *FloatLit:
		adjustPos(&e.pos)
		adjustPos(&e.end)
	case *BoolLit:
		adjustPos(&e.pos)
		adjustPos(&e.end)
	case *StringLit:
		adjustPos(&e.pos)
		adjustPos(&e.end)
	case *BinaryExpr:
		adjustPos(&e.pos)
		adjustPos(&e.end)
		offsetExprPositions(e.Left, lineOffset, colOffset)
		offsetExprPositions(e.Right, lineOffset, colOffset)
	case *UnaryExpr:
		adjustPos(&e.pos)
		adjustPos(&e.end)
		offsetExprPositions(e.Operand, lineOffset, colOffset)
	case *ParenExpr:
		adjustPos(&e.pos)
		adjustPos(&e.end)
		offsetExprPositions(e.Expr, lineOffset, colOffset)
	case *CallExpr:
		adjustPos(&e.pos)
		adjustPos(&e.end)
		offsetExprPositions(e.Callee, lineOffset, colOffset)
		for _, arg := range e.Args {
			offsetExprPositions(arg.Value, lineOffset, colOffset)
		}
	case *MemberExpr:
		adjustPos(&e.pos)
		adjustPos(&e.end)
		offsetExprPositions(e.Target, lineOffset, colOffset)
	case *IndexExpr:
		adjustPos(&e.pos)
		adjustPos(&e.end)
		offsetExprPositions(e.Target, lineOffset, colOffset)
		offsetExprPositions(e.Index, lineOffset, colOffset)
	case *SliceExpr:
		adjustPos(&e.pos)
		adjustPos(&e.end)
		offsetExprPositions(e.Target, lineOffset, colOffset)
		offsetExprPositions(e.Low, lineOffset, colOffset)
		offsetExprPositions(e.High, lineOffset, colOffset)
	case *SliceTypeExpr:
		adjustPos(&e.pos)
		adjustPos(&e.end)
		offsetExprPositions(e.Inner, lineOffset, colOffset)
	}
}
