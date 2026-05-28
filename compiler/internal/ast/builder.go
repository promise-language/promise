package ast

import (
	"fmt"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/parser"
)

// Builder converts an ANTLR4 parse tree into an AST.
type Builder struct {
	parser.BasePromiseParserVisitor
	filename string
	errors   []error
}

// Build converts an ANTLR4 compilation unit parse tree into an AST File.
func Build(filename string, tree parser.ICompilationUnitContext) (*File, []error) {
	b := &Builder{filename: filename}
	result := tree.Accept(b)
	if result == nil {
		return nil, b.errors
	}
	return result.(*File), b.errors
}

func (b *Builder) posFromToken(tok antlr.Token) Pos {
	return Pos{
		File:   b.filename,
		Line:   tok.GetLine(),
		Column: tok.GetColumn(),
	}
}

func (b *Builder) posFromContext(ctx antlr.ParserRuleContext) Pos {
	return b.posFromToken(ctx.GetStart())
}

func (b *Builder) endFromContext(ctx antlr.ParserRuleContext) Pos {
	stop := ctx.GetStop()
	if stop == nil {
		return Pos{}
	}
	return Pos{
		File:   b.filename,
		Line:   stop.GetLine(),
		Column: stop.GetColumn() + len(stop.GetText()),
	}
}

func (b *Builder) baseFromContext(ctx antlr.ParserRuleContext) nodeBase {
	return nodeBase{
		pos: b.posFromContext(ctx),
		end: b.endFromContext(ctx),
	}
}

func (b *Builder) errorf(pos Pos, format string, args ...any) {
	b.errors = append(b.errors, fmt.Errorf("%s: %s", pos, fmt.Sprintf(format, args...)))
}
