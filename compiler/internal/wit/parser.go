package wit

import (
	"fmt"
	"strings"
)

// ParseError represents a parse error with source position.
type ParseError struct {
	Pos Pos
	Msg string
}

func (e *ParseError) Error() string {
	if e.Pos.File != "" {
		return fmt.Sprintf("%s:%d:%d: %s", e.Pos.File, e.Pos.Line, e.Pos.Column, e.Msg)
	}
	return fmt.Sprintf("%d:%d: %s", e.Pos.Line, e.Pos.Column, e.Msg)
}

// Parser is a recursive descent parser for WIT.
type Parser struct {
	lex    *Lexer
	errors []*ParseError
	// Accumulated doc comment from doc-comment tokens preceding a declaration.
	pendingDoc string
}

// Parse parses WIT source and returns the file AST.
func Parse(src, filename string) (*File, []*ParseError) {
	p := &Parser{
		lex: NewLexer(src, filename),
	}
	file := p.parseFile()
	return file, p.errors
}

func (p *Parser) error(pos Pos, msg string) {
	p.errors = append(p.errors, &ParseError{Pos: pos, Msg: msg})
}

func (p *Parser) errorf(pos Pos, format string, args ...interface{}) {
	p.error(pos, fmt.Sprintf(format, args...))
}

func (p *Parser) peek() Token {
	return p.lex.Peek()
}

func (p *Parser) next() Token {
	return p.lex.Next()
}

func (p *Parser) expect(kind TokenKind) Token {
	tok := p.next()
	if tok.Kind != kind {
		p.errorf(tok.Pos, "expected %s, got %s", kind, tok.Kind)
	}
	return tok
}

func (p *Parser) at(kind TokenKind) bool {
	return p.peek().Kind == kind
}

func (p *Parser) match(kind TokenKind) bool {
	if p.at(kind) {
		p.next()
		return true
	}
	return false
}

// consumeDoc collects any pending doc comment and returns it, clearing the state.
func (p *Parser) consumeDoc() string {
	doc := p.pendingDoc
	p.pendingDoc = ""
	return doc
}

// tryConsumeDocToken consumes any pending TokenDocComment tokens and stores
// the last one as pendingDoc. Multiple consecutive doc comments are merged.
func (p *Parser) tryConsumeDocToken() {
	for p.at(TokenDocComment) {
		p.pendingDoc = p.next().Value
	}
}

func (p *Parser) parseFile() *File {
	file := &File{}

	// Optional package declaration
	if p.at(TokenPackage) {
		file.Package = p.parsePackage()
	}

	for !p.at(TokenEOF) {
		p.tryConsumeDocToken()
		switch p.peek().Kind {
		case TokenInterface:
			file.Interfaces = append(file.Interfaces, p.parseInterface())
		case TokenWorld:
			file.Worlds = append(file.Worlds, p.parseWorld())
		case TokenEOF:
			// done
		default:
			p.errorf(p.peek().Pos, "expected interface or world, got %s", p.peek().Kind)
			p.next() // skip to recover
		}
	}

	return file
}

func (p *Parser) parsePackage() *Package {
	pos := p.expect(TokenPackage).Pos
	pkg := &Package{Pos: pos}

	// namespace:name@version
	// namespace and name are identifiers
	nsTok := p.next()
	pkg.Namespace = nsTok.Value

	p.expect(TokenColon)

	nameTok := p.next()
	pkg.Name = nameTok.Value

	// Optional @version - read version as a sequence of tokens: ident.ident.ident
	if p.match(TokenAt) {
		pkg.Version = p.readVersion()
	}

	p.expect(TokenSemicolon)
	return pkg
}

// readVersion reads a semver string like "0.2.0" from the token stream.
// Since the lexer produces individual ident/dot tokens, we reconstruct the version.
func (p *Parser) readVersion() string {
	var parts []string
	tok := p.next()
	parts = append(parts, tok.Value)
	for p.match(TokenDot) {
		tok = p.next()
		parts = append(parts, tok.Value)
	}
	return strings.Join(parts, ".")
}

func (p *Parser) parseInterface() *Interface {
	doc := p.consumeDoc()
	pos := p.expect(TokenInterface).Pos
	name := p.next().Value

	iface := &Interface{
		Name: name,
		Doc:  doc,
		Pos:  pos,
	}

	p.expect(TokenLBrace)
	for !p.at(TokenRBrace) && !p.at(TokenEOF) {
		item := p.parseInterfaceItem()
		if item != nil {
			iface.Items = append(iface.Items, item)
		}
	}
	p.expect(TokenRBrace)

	return iface
}

func (p *Parser) parseInterfaceItem() InterfaceItem {
	p.tryConsumeDocToken()
	tok := p.peek()

	switch tok.Kind {
	case TokenRecord:
		return p.parseRecord()
	case TokenVariant:
		return p.parseVariant()
	case TokenEnum:
		return p.parseEnum()
	case TokenFlags:
		return p.parseFlags()
	case TokenResource:
		return p.parseResource()
	case TokenType:
		return p.parseTypeAlias()
	case TokenUse:
		return p.parseUse()
	case TokenIdent:
		// function: name: func(...)
		return p.parseNamedFunc()
	default:
		p.errorf(tok.Pos, "unexpected %s in interface body", tok.Kind)
		p.next()
		return nil
	}
}

func (p *Parser) parseRecord() *Record {
	doc := p.consumeDoc()
	pos := p.expect(TokenRecord).Pos
	name := p.next().Value
	rec := &Record{Name: name, Doc: doc, Pos: pos}

	p.expect(TokenLBrace)
	for !p.at(TokenRBrace) && !p.at(TokenEOF) {
		field := p.parseField()
		rec.Fields = append(rec.Fields, field)
		// Fields separated by comma or just before }
		p.match(TokenComma)
	}
	p.expect(TokenRBrace)

	return rec
}

func (p *Parser) parseField() *Field {
	name := p.next().Value
	p.expect(TokenColon)
	typ := p.parseTypeRef()
	return &Field{Name: name, Type: typ}
}

func (p *Parser) parseVariant() *Variant {
	doc := p.consumeDoc()
	pos := p.expect(TokenVariant).Pos
	name := p.next().Value
	v := &Variant{Name: name, Doc: doc, Pos: pos}

	p.expect(TokenLBrace)
	for !p.at(TokenRBrace) && !p.at(TokenEOF) {
		c := p.parseCase()
		v.Cases = append(v.Cases, c)
		p.match(TokenComma)
	}
	p.expect(TokenRBrace)

	return v
}

func (p *Parser) parseCase() *Case {
	name := p.next().Value
	c := &Case{Name: name}
	if p.match(TokenLParen) {
		c.Type = p.parseTypeRef()
		p.expect(TokenRParen)
	}
	return c
}

func (p *Parser) parseEnum() *Enum {
	doc := p.consumeDoc()
	pos := p.expect(TokenEnum).Pos
	name := p.next().Value
	e := &Enum{Name: name, Doc: doc, Pos: pos}

	p.expect(TokenLBrace)
	for !p.at(TokenRBrace) && !p.at(TokenEOF) {
		caseName := p.next().Value
		e.Cases = append(e.Cases, caseName)
		p.match(TokenComma)
	}
	p.expect(TokenRBrace)

	return e
}

func (p *Parser) parseFlags() *Flags {
	doc := p.consumeDoc()
	pos := p.expect(TokenFlags).Pos
	name := p.next().Value
	f := &Flags{Name: name, Doc: doc, Pos: pos}

	p.expect(TokenLBrace)
	for !p.at(TokenRBrace) && !p.at(TokenEOF) {
		flagName := p.next().Value
		f.Flags = append(f.Flags, flagName)
		p.match(TokenComma)
	}
	p.expect(TokenRBrace)

	return f
}

func (p *Parser) parseResource() *Resource {
	doc := p.consumeDoc()
	pos := p.expect(TokenResource).Pos
	name := p.next().Value
	r := &Resource{Name: name, Doc: doc, Pos: pos}

	// Resource can be: resource name; OR resource name { ... }
	if p.match(TokenLBrace) {
		for !p.at(TokenRBrace) && !p.at(TokenEOF) {
			p.tryConsumeDocToken()
			fn := p.parseResourceMethod(name)
			if fn != nil {
				r.Methods = append(r.Methods, fn)
			}
		}
		p.expect(TokenRBrace)
	} else {
		p.expect(TokenSemicolon)
	}

	return r
}

func (p *Parser) parseResourceMethod(resourceName string) *Func {
	doc := p.consumeDoc()
	tok := p.peek()

	switch tok.Kind {
	case TokenConstructor:
		return p.parseConstructorFunc(doc)
	case TokenIdent:
		// Regular method: name: func(...)
		return p.parseMethodFunc(doc)
	case TokenStatic:
		return p.parseStaticFunc(doc)
	default:
		p.errorf(tok.Pos, "expected method, constructor, or static in resource body, got %s", tok.Kind)
		p.next()
		return nil
	}
}

func (p *Parser) parseConstructorFunc(doc string) *Func {
	pos := p.expect(TokenConstructor).Pos
	f := &Func{
		Name: "constructor",
		Kind: FuncConstructor,
		Doc:  doc,
		Pos:  pos,
	}
	p.expect(TokenLParen)
	f.Params = p.parseParams()
	p.expect(TokenRParen)
	p.expect(TokenSemicolon)
	return f
}

func (p *Parser) parseMethodFunc(doc string) *Func {
	pos := p.peek().Pos
	name := p.next().Value
	p.expect(TokenColon)
	p.expect(TokenFunc)
	f := &Func{
		Name: name,
		Kind: FuncMethod,
		Doc:  doc,
		Pos:  pos,
	}
	p.expect(TokenLParen)
	f.Params = p.parseParams()
	p.expect(TokenRParen)
	if p.match(TokenArrow) {
		f.Results = p.parseResults()
	}
	p.expect(TokenSemicolon)
	return f
}

func (p *Parser) parseStaticFunc(doc string) *Func {
	p.expect(TokenStatic)
	pos := p.peek().Pos
	name := p.next().Value
	p.expect(TokenColon)
	p.expect(TokenFunc)
	f := &Func{
		Name: name,
		Kind: FuncStatic,
		Doc:  doc,
		Pos:  pos,
	}
	p.expect(TokenLParen)
	f.Params = p.parseParams()
	p.expect(TokenRParen)
	if p.match(TokenArrow) {
		f.Results = p.parseResults()
	}
	p.expect(TokenSemicolon)
	return f
}

func (p *Parser) parseTypeAlias() *TypeAlias {
	doc := p.consumeDoc()
	pos := p.expect(TokenType).Pos
	name := p.next().Value
	p.expect(TokenEquals)
	target := p.parseTypeRef()
	p.expect(TokenSemicolon)
	return &TypeAlias{Name: name, Target: target, Doc: doc, Pos: pos}
}

func (p *Parser) parseUse() *Use {
	pos := p.expect(TokenUse).Pos
	u := &Use{Pos: pos}

	// Path: ident:ident/ident or ident/ident
	u.Path = p.readUsePath()

	p.expect(TokenDot)
	p.expect(TokenLBrace)
	for !p.at(TokenRBrace) && !p.at(TokenEOF) {
		un := UseName{Name: p.next().Value}
		if p.match(TokenAs) {
			un.As = p.next().Value
		}
		u.Names = append(u.Names, un)
		p.match(TokenComma)
	}
	p.expect(TokenRBrace)
	p.expect(TokenSemicolon)

	return u
}

func (p *Parser) readUsePath() string {
	var parts []string
	parts = append(parts, p.next().Value)
	for p.at(TokenColon) || p.at(TokenSlash) {
		sep := p.next()
		parts = append(parts, sep.Value)
		parts = append(parts, p.next().Value)
	}
	return strings.Join(parts, "")
}

// parseNamedFunc parses: name: func(params) -> result;
func (p *Parser) parseNamedFunc() *Func {
	doc := p.consumeDoc()
	pos := p.peek().Pos
	name := p.next().Value
	p.expect(TokenColon)
	p.expect(TokenFunc)
	f := &Func{
		Name: name,
		Kind: FuncFree,
		Doc:  doc,
		Pos:  pos,
	}
	p.expect(TokenLParen)
	f.Params = p.parseParams()
	p.expect(TokenRParen)
	if p.match(TokenArrow) {
		f.Results = p.parseResults()
	}
	p.expect(TokenSemicolon)
	return f
}

func (p *Parser) parseParams() []*Param {
	var params []*Param
	for !p.at(TokenRParen) && !p.at(TokenEOF) {
		param := &Param{}
		param.Name = p.next().Value
		p.expect(TokenColon)
		param.Type = p.parseTypeRef()
		params = append(params, param)
		if !p.match(TokenComma) {
			break
		}
	}
	return params
}

func (p *Parser) parseResults() *Results {
	r := &Results{}

	// Named results: (name: type, name: type)
	if p.at(TokenLParen) {
		p.next()
		for !p.at(TokenRParen) && !p.at(TokenEOF) {
			param := &Param{}
			param.Name = p.next().Value
			p.expect(TokenColon)
			param.Type = p.parseTypeRef()
			r.Named = append(r.Named, param)
			if !p.match(TokenComma) {
				break
			}
		}
		p.expect(TokenRParen)
	} else {
		// Anonymous single result
		r.Anon = p.parseTypeRef()
	}

	return r
}

func (p *Parser) parseTypeRef() *TypeRef {
	tok := p.peek()

	// Built-in parameterized types
	switch tok.Kind {
	case TokenList:
		p.next()
		p.expect(TokenLAngle)
		elem := p.parseTypeRef()
		p.expect(TokenRAngle)
		return &TypeRef{Kind: ListType, Elem: elem}

	case TokenOption:
		p.next()
		p.expect(TokenLAngle)
		elem := p.parseTypeRef()
		p.expect(TokenRAngle)
		return &TypeRef{Kind: OptionType, Elem: elem}

	case TokenResult:
		p.next()
		ref := &TypeRef{Kind: ResultType}
		if p.match(TokenLAngle) {
			// result<ok, err> or result<_, err> or result<ok>
			if p.at(TokenUnderscore) {
				p.next() // void ok
			} else {
				ref.Ok = p.parseTypeRef()
			}
			if p.match(TokenComma) {
				if p.at(TokenUnderscore) {
					p.next() // void err
				} else {
					ref.Err = p.parseTypeRef()
				}
			}
			p.expect(TokenRAngle)
		}
		return ref

	case TokenTuple:
		p.next()
		ref := &TypeRef{Kind: TupleType}
		p.expect(TokenLAngle)
		for !p.at(TokenRAngle) && !p.at(TokenEOF) {
			elem := p.parseTypeRef()
			ref.Elements = append(ref.Elements, elem)
			if !p.match(TokenComma) {
				break
			}
		}
		p.expect(TokenRAngle)
		return ref

	case TokenOwn:
		p.next()
		p.expect(TokenLAngle)
		elem := p.parseTypeRef()
		p.expect(TokenRAngle)
		return &TypeRef{Kind: OwnType, Elem: elem}

	case TokenBorrow:
		p.next()
		p.expect(TokenLAngle)
		elem := p.parseTypeRef()
		p.expect(TokenRAngle)
		return &TypeRef{Kind: BorrowType, Elem: elem}

	// Primitive built-in types
	case TokenU8:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "u8"}
	case TokenU16:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "u16"}
	case TokenU32:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "u32"}
	case TokenU64:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "u64"}
	case TokenS8:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "s8"}
	case TokenS16:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "s16"}
	case TokenS32:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "s32"}
	case TokenS64:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "s64"}
	case TokenF32:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "f32"}
	case TokenF64:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "f64"}
	case TokenBool:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "bool"}
	case TokenChar:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "char"}
	case TokenString:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "string"}

	case TokenIdent:
		p.next()
		return &TypeRef{Kind: NamedType, Name: tok.Value}

	default:
		p.errorf(tok.Pos, "expected type, got %s", tok.Kind)
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "u32"} // error recovery
	}
}

func (p *Parser) parseWorld() *World {
	doc := p.consumeDoc()
	pos := p.expect(TokenWorld).Pos
	name := p.next().Value
	w := &World{Name: name, Doc: doc, Pos: pos}

	p.expect(TokenLBrace)
	for !p.at(TokenRBrace) && !p.at(TokenEOF) {
		tok := p.peek()
		switch tok.Kind {
		case TokenImport:
			p.next()
			item := p.parseWorldItem()
			w.Imports = append(w.Imports, item)
		case TokenExport:
			p.next()
			item := p.parseWorldItem()
			w.Exports = append(w.Exports, item)
		default:
			p.errorf(tok.Pos, "expected import or export in world body, got %s", tok.Kind)
			p.next()
		}
	}
	p.expect(TokenRBrace)

	return w
}

func (p *Parser) parseWorldItem() *WorldItem {
	pos := p.peek().Pos
	item := &WorldItem{Pos: pos}

	// Could be:
	// - interface reference: ident:ident/ident;
	// - named interface: name: interface { ... }
	// - inline function: name: func(...) -> ...;
	name := p.next().Value

	if p.match(TokenColon) {
		// Check if next is func
		if p.at(TokenFunc) {
			p.next() // consume func
			f := &Func{
				Name: name,
				Kind: FuncFree,
				Pos:  pos,
			}
			p.expect(TokenLParen)
			f.Params = p.parseParams()
			p.expect(TokenRParen)
			if p.match(TokenArrow) {
				f.Results = p.parseResults()
			}
			p.expect(TokenSemicolon)
			item.Name = name
			item.Func = f
			return item
		}
		// Otherwise it's an interface reference like: wasi:filesystem/types@0.2.0
		var parts []string
		parts = append(parts, name, ":")
		parts = append(parts, p.next().Value)
		for p.at(TokenSlash) {
			parts = append(parts, p.next().Value)
			parts = append(parts, p.next().Value)
		}
		// Optional @version
		if p.match(TokenAt) {
			parts = append(parts, "@")
			parts = append(parts, p.readVersion())
		}
		item.Interface = strings.Join(parts, "")
		p.expect(TokenSemicolon)
	} else {
		// Simple interface name reference
		item.Name = name
		p.expect(TokenSemicolon)
	}

	return item
}
