package ast

import (
	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/parser"
)

func (b *Builder) visitExpr(ctx parser.IExpressionContext) Expr {
	if ctx == nil {
		return nil
	}
	result := ctx.Accept(b)
	if result == nil {
		return nil
	}
	return result.(Expr)
}

func (b *Builder) visitTypeRef(ctx parser.ITypeRefContext) TypeRef {
	if ctx == nil {
		return nil
	}
	result := ctx.Accept(b)
	if result == nil {
		return nil
	}
	return result.(TypeRef)
}

func (b *Builder) visitBlock(ctx parser.IBlockContext) *Block {
	if ctx == nil {
		return nil
	}
	result := ctx.Accept(b)
	if result == nil {
		return nil
	}
	return result.(*Block)
}

func (b *Builder) visitStmt(ctx parser.IStatementContext) Stmt {
	if ctx == nil {
		return nil
	}
	result := ctx.Accept(b)
	if result == nil {
		return nil
	}
	return result.(Stmt)
}

func (b *Builder) bindingText(ctx parser.IBindingNameContext) string {
	if ctx == nil {
		return ""
	}
	if ctx.UNDERSCORE() != nil {
		return "_"
	}
	return ctx.IDENT().GetText()
}

func (b *Builder) visitRefMod(ctx parser.IRefModContext) RefModifier {
	if ctx == nil {
		return RefNone
	}
	if ctx.AMP() != nil {
		return RefShared
	}
	return RefMut
}

func (b *Builder) visitAssignOp(ctx parser.IAssignOpContext) AssignOp {
	if ctx == nil {
		return OpAssign
	}
	if ctx.PLUS_ASSIGN() != nil {
		return OpAddAssign
	}
	if ctx.MINUS_ASSIGN() != nil {
		return OpSubAssign
	}
	if ctx.STAR_ASSIGN() != nil {
		return OpMulAssign
	}
	if ctx.SLASH_ASSIGN() != nil {
		return OpDivAssign
	}
	if ctx.PERCENT_ASSIGN() != nil {
		return OpModAssign
	}
	return OpAssign
}

func (b *Builder) termText(node antlr.TerminalNode) string {
	if node == nil {
		return ""
	}
	return node.GetText()
}

func (b *Builder) visitStringLiteral(ctx parser.IStringLiteralContext) *StringLit {
	if ctx == nil {
		return nil
	}
	result := ctx.Accept(b)
	if result == nil {
		return nil
	}
	return result.(*StringLit)
}

func (b *Builder) visitMatchPattern(ctx parser.IMatchPatternContext) MatchPattern {
	if ctx == nil {
		return nil
	}
	result := ctx.Accept(b)
	if result == nil {
		return nil
	}
	return result.(MatchPattern)
}

func (b *Builder) visitIsPattern(ctx parser.IPatternContext) IsPattern {
	if ctx == nil {
		return nil
	}
	result := ctx.Accept(b)
	if result == nil {
		return nil
	}
	return result.(IsPattern)
}
