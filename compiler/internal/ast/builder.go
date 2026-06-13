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
	// inFailableFunc is true while visiting the body of a `!` (failable)
	// function/method/getter/setter. Used for the context-aware bare-`?`
	// diagnostic (T0867): inside a failable function a bare call already
	// auto-propagates, so the message leads with "drop the `?`".
	inFailableFunc bool
	// interpActive/interpLineOffset/interpColOffset translate the positions of
	// errors emitted while re-parsing a string-interpolation expression
	// (T0865/T0867). Interpolation text is parsed in its own input stream, so
	// node positions are relative to that stream and get fixed up afterward by
	// offsetExprPositions. Errors, however, are emitted during Accept and would
	// otherwise carry interpolation-relative positions (e.g. line 1) that point
	// at the wrong source location; errorf applies these offsets so the message
	// points at the real source. The adjustment mirrors offsetExprPositions.
	interpActive     bool
	interpLineOffset int
	interpColOffset  int
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
	if b.interpActive {
		// Same rule as offsetExprPositions: only the first line of the
		// interpolation shares a line with the surrounding source, so the
		// column offset applies there; every line shifts by the line offset.
		if pos.Line == 1 {
			pos.Column += b.interpColOffset
		}
		pos.Line += b.interpLineOffset
	}
	b.errors = append(b.errors, fmt.Errorf("%s: %s", pos, fmt.Sprintf(format, args...)))
}
