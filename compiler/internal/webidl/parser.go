package webidl

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

// Parser is a recursive descent parser for WebIDL.
type Parser struct {
	lex    *Lexer
	errors []*ParseError
}

// Parse parses WebIDL source and returns the file AST.
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

func (p *Parser) parseFile() *File {
	file := &File{}

	for !p.at(TokenEOF) {
		// Extended attributes at top level are kept only on interfaces.
		doc, extAttrs := p.parseDeclStart()
		tok := p.peek()

		switch tok.Kind {
		case TokenInterface:
			iface := p.parseInterface()
			iface.ExtAttrs = append(extAttrs, iface.ExtAttrs...)
			iface.Doc = doc
			file.Interfaces = append(file.Interfaces, iface)
		case TokenDictionary:
			dict := p.parseDictionary()
			dict.Doc = doc
			file.Dictionaries = append(file.Dictionaries, dict)
		case TokenEnum:
			e := p.parseEnum()
			e.Doc = doc
			file.Enums = append(file.Enums, e)
		case TokenCallback:
			if cb := p.parseCallback(); cb != nil {
				cb.Doc = doc
				file.Callbacks = append(file.Callbacks, cb)
			}
		case TokenTypedef:
			td := p.parseTypedef()
			td.Doc = doc
			file.Typedefs = append(file.Typedefs, td)
		case TokenPartial:
			p.next() // consume 'partial'
			partialTok := p.peek()
			switch partialTok.Kind {
			case TokenInterface:
				partial := p.parsePartialInterface(false)
				file.Partials = append(file.Partials, partial)
			case TokenMixin:
				partial := p.parsePartialMixin()
				file.Partials = append(file.Partials, partial)
			case TokenDictionary:
				file.PartialDicts = append(file.PartialDicts, p.parsePartialDictionary())
			default:
				p.errorf(partialTok.Pos, "expected interface, mixin, or dictionary after partial, got %s", partialTok.Kind)
				p.next()
			}
		case TokenMixin:
			// Standalone: interface mixin Name { ... };
			// But WebIDL spec uses "interface mixin", so this handles just "mixin"
			m := p.parseMixin()
			m.Doc = doc
			file.Mixins = append(file.Mixins, m)
		case TokenIdent:
			// Could be: "Target includes Mixin;"
			if p.lex.PeekN(1).Kind == TokenIncludes {
				file.Includes = append(file.Includes, p.parseIncludes())
			} else {
				p.errorf(tok.Pos, "unexpected identifier %q at top level", tok.Value)
				p.next()
			}
		case TokenEOF:
			// done
		default:
			p.errorf(tok.Pos, "unexpected %s at top level", tok.Kind)
			p.next()
		}
	}

	return file
}

// parseDeclStart consumes the extended attributes that open a definition or
// member and returns them with the declaration's doc comment: the comment before
// the attributes, or else the one between them and the declaration.
func (p *Parser) parseDeclStart() (string, []*ExtAttr) {
	doc := p.peek().Doc
	attrs := p.tryParseExtAttrs()
	if doc == "" {
		doc = p.peek().Doc
	}
	return doc, attrs
}

func (p *Parser) tryParseExtAttrs() []*ExtAttr {
	// Extended attributes are tokenized as "[...]" ident tokens
	tok := p.peek()
	if tok.Kind == TokenIdent && strings.HasPrefix(tok.Value, "[") && strings.HasSuffix(tok.Value, "]") {
		p.next()
		return parseExtAttrString(tok.Value[1 : len(tok.Value)-1])
	}
	return nil
}

// parseExtAttrString parses the content between [ and ] into ExtAttr entries.
func parseExtAttrString(s string) []*ExtAttr {
	var attrs []*ExtAttr
	parts := strings.Split(s, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		attr := &ExtAttr{}
		if idx := strings.IndexByte(part, '='); idx >= 0 {
			attr.Name = strings.TrimSpace(part[:idx])
			attr.Value = strings.TrimSpace(part[idx+1:])
		} else if idx := strings.IndexByte(part, '('); idx >= 0 {
			attr.Name = strings.TrimSpace(part[:idx])
			attr.Value = strings.TrimRight(strings.TrimSpace(part[idx+1:]), ")")
		} else {
			attr.Name = part
		}
		attrs = append(attrs, attr)
	}
	return attrs
}

func (p *Parser) parseInterface() *Interface {
	pos := p.expect(TokenInterface).Pos

	// Check for "interface mixin" pattern
	if p.at(TokenMixin) {
		// This is really a mixin declaration using "interface mixin" syntax
		return p.parseInterfaceMixinAsInterface(pos)
	}

	name := p.next().Value
	iface := &Interface{
		Name: name,
		Pos:  pos,
	}

	// Inheritance: : Parent
	if p.match(TokenColon) {
		iface.Parent = p.next().Value
	}

	p.expect(TokenLBrace)
	for !p.at(TokenRBrace) && !p.at(TokenEOF) {
		member := p.parseMember()
		if member != nil {
			iface.Members = append(iface.Members, member)
		}
	}
	p.expect(TokenRBrace)
	p.expect(TokenSemicolon)

	return iface
}

// parseInterfaceMixinAsInterface handles "interface mixin Name { ... };" and
// returns it as an Interface with a special marker so the converter can treat it
// as a mixin during IR conversion.
func (p *Parser) parseInterfaceMixinAsInterface(pos Pos) *Interface {
	p.next() // consume 'mixin'
	name := p.next().Value
	iface := &Interface{
		Name: name,
		Pos:  pos,
	}

	p.expect(TokenLBrace)
	for !p.at(TokenRBrace) && !p.at(TokenEOF) {
		member := p.parseMember()
		if member != nil {
			iface.Members = append(iface.Members, member)
		}
	}
	p.expect(TokenRBrace)
	p.expect(TokenSemicolon)

	// Store as mixin indicator via ExtAttr
	iface.ExtAttrs = append(iface.ExtAttrs, &ExtAttr{Name: "_mixin"})
	return iface
}

func (p *Parser) parseDictionary() *Dictionary {
	pos := p.expect(TokenDictionary).Pos
	name := p.next().Value
	dict := &Dictionary{
		Name: name,
		Pos:  pos,
	}

	if p.match(TokenColon) {
		dict.Parent = p.next().Value
	}

	p.expect(TokenLBrace)
	for !p.at(TokenRBrace) && !p.at(TokenEOF) {
		member := p.parseDictMember()
		dict.Members = append(dict.Members, member)
	}
	p.expect(TokenRBrace)
	p.expect(TokenSemicolon)

	return dict
}

func (p *Parser) parseDictMember() *DictMember {
	// Skip extended attributes on dictionary members, e.g.
	// [RuntimeEnabled=Foo] boolean contentVisibilityAuto = false;
	p.tryParseExtAttrs()
	pos := p.peek().Pos
	m := &DictMember{Pos: pos}

	if p.match(TokenRequired) {
		m.Required = true
	}

	m.Type = p.parseTypeRef()
	m.Name = p.next().Value

	// Default value
	if p.match(TokenEquals) {
		m.Default = p.readDefaultValue()
	}

	p.expect(TokenSemicolon)
	return m
}

func (p *Parser) parseEnum() *Enum {
	pos := p.expect(TokenEnum).Pos
	name := p.next().Value
	e := &Enum{
		Name: name,
		Pos:  pos,
	}

	p.expect(TokenLBrace)
	for !p.at(TokenRBrace) && !p.at(TokenEOF) {
		// Enum values are string literals
		if p.at(TokenString) {
			e.Values = append(e.Values, p.next().Value)
		} else {
			p.errorf(p.peek().Pos, "expected string literal in enum, got %s", p.peek().Kind)
			p.next()
		}
		p.match(TokenComma)
	}
	p.expect(TokenRBrace)
	p.expect(TokenSemicolon)

	return e
}

func (p *Parser) parseCallback() *Callback {
	pos := p.expect(TokenCallback).Pos

	// "callback interface" is a different construct — skip for now
	if p.at(TokenInterface) {
		// Treat as interface
		iface := p.parseInterface()
		_ = iface
		return nil
	}

	name := p.next().Value
	p.expect(TokenEquals)

	ret := p.parseTypeRef()

	p.expect(TokenLParen)
	params := p.parseParams()
	p.expect(TokenRParen)
	p.expect(TokenSemicolon)

	return &Callback{
		Name:   name,
		Return: ret,
		Params: params,
		Pos:    pos,
	}
}

func (p *Parser) parseTypedef() *Typedef {
	pos := p.expect(TokenTypedef).Pos
	typ := p.parseTypeRef()
	name := p.next().Value
	p.expect(TokenSemicolon)

	return &Typedef{
		Name: name,
		Type: typ,
		Pos:  pos,
	}
}

func (p *Parser) parsePartialInterface(isMixin bool) *PartialInterface {
	pos := p.expect(TokenInterface).Pos

	// Check for "interface mixin"
	if p.at(TokenMixin) {
		p.next()
		isMixin = true
	}

	name := p.next().Value
	partial := &PartialInterface{
		Name:    name,
		IsMixin: isMixin,
		Pos:     pos,
	}

	p.expect(TokenLBrace)
	for !p.at(TokenRBrace) && !p.at(TokenEOF) {
		member := p.parseMember()
		if member != nil {
			partial.Members = append(partial.Members, member)
		}
	}
	p.expect(TokenRBrace)
	p.expect(TokenSemicolon)

	return partial
}

func (p *Parser) parsePartialMixin() *PartialInterface {
	pos := p.expect(TokenMixin).Pos
	name := p.next().Value
	partial := &PartialInterface{
		Name:    name,
		IsMixin: true,
		Pos:     pos,
	}

	p.expect(TokenLBrace)
	for !p.at(TokenRBrace) && !p.at(TokenEOF) {
		member := p.parseMember()
		if member != nil {
			partial.Members = append(partial.Members, member)
		}
	}
	p.expect(TokenRBrace)
	p.expect(TokenSemicolon)

	return partial
}

func (p *Parser) parsePartialDictionary() *PartialDictionary {
	pos := p.expect(TokenDictionary).Pos
	name := p.next().Value
	partial := &PartialDictionary{
		Name: name,
		Pos:  pos,
	}

	p.expect(TokenLBrace)
	for !p.at(TokenRBrace) && !p.at(TokenEOF) {
		partial.Members = append(partial.Members, p.parseDictMember())
	}
	p.expect(TokenRBrace)
	p.expect(TokenSemicolon)

	return partial
}

func (p *Parser) parseMixin() *Mixin {
	pos := p.expect(TokenMixin).Pos
	name := p.next().Value
	m := &Mixin{
		Name: name,
		Pos:  pos,
	}

	p.expect(TokenLBrace)
	for !p.at(TokenRBrace) && !p.at(TokenEOF) {
		member := p.parseMember()
		if member != nil {
			m.Members = append(m.Members, member)
		}
	}
	p.expect(TokenRBrace)
	p.expect(TokenSemicolon)

	return m
}

func (p *Parser) parseIncludes() *IncludesStatement {
	pos := p.peek().Pos
	target := p.next().Value
	p.expect(TokenIncludes)
	mixin := p.next().Value
	p.expect(TokenSemicolon)

	return &IncludesStatement{
		Target: target,
		Mixin:  mixin,
		Pos:    pos,
	}
}

// parseMember parses a single interface/mixin member, with its doc comment.
func (p *Parser) parseMember() Member {
	// Extended attributes on members are skipped.
	doc, _ := p.parseDeclStart()
	member := p.parseMemberBody()
	switch m := member.(type) {
	case *Attribute:
		m.Doc = doc
	case *Operation:
		m.Doc = doc
	case *Constructor:
		m.Doc = doc
	case *Const:
		m.Doc = doc
	}
	return member
}

// parseMemberBody parses a member after its extended attributes.
func (p *Parser) parseMemberBody() Member {
	tok := p.peek()

	switch tok.Kind {
	case TokenConst:
		return p.parseConst()
	case TokenReadonly:
		p.next()
		return p.parseAttribute(true, false)
	case TokenAttribute:
		return p.parseAttribute(false, false)
	case TokenStatic:
		p.next()
		// Static attribute or static operation
		if p.at(TokenReadonly) {
			p.next()
			return p.parseAttribute(true, true)
		}
		if p.at(TokenAttribute) {
			return p.parseAttribute(false, true)
		}
		// Static operation
		return p.parseOperation(true, "")
	case TokenGetter:
		p.next()
		return p.parseOperation(false, "getter")
	case TokenSetter:
		p.next()
		return p.parseOperation(false, "setter")
	case TokenDeleter:
		p.next()
		return p.parseOperation(false, "deleter")
	case TokenStringifier:
		p.next()
		if p.match(TokenSemicolon) {
			// stringifier; — shorthand for toString()
			return &Operation{
				Name:   "toString",
				Return: &TypeRef{Kind: BuiltinType, Builtin: "DOMString"},
				Pos:    tok.Pos,
			}
		}
		if p.at(TokenReadonly) || p.at(TokenAttribute) {
			readonly := p.match(TokenReadonly)
			return p.parseAttribute(readonly, false)
		}
		return p.parseOperation(false, "stringifier")
	case TokenConstructor:
		return p.parseConstructor()
	case TokenIterable:
		return p.parseIterable()
	case TokenAsync:
		p.next()
		if p.at(TokenIterable) {
			return p.parseIterable()
		}
		// async operation — parse as regular operation
		return p.parseOperation(false, "")
	default:
		// legacycaller is a special keyword only here, at the start of a member;
		// the lexer leaves it an identifier, so it still names anything elsewhere.
		if tok.Kind == TokenIdent && tok.Value == "legacycaller" {
			p.next()
			return p.parseOperation(false, "legacycaller")
		}
		// Regular operation (return type first)
		return p.parseOperation(false, "")
	}
}

func (p *Parser) parseConst() *Const {
	pos := p.expect(TokenConst).Pos
	typ := p.parseTypeRef()
	name := p.next().Value
	p.expect(TokenEquals)
	value := p.readDefaultValue()
	p.expect(TokenSemicolon)

	return &Const{
		Name:  name,
		Type:  typ,
		Value: value,
		Pos:   pos,
	}
}

func (p *Parser) parseAttribute(readonly, static bool) *Attribute {
	pos := p.expect(TokenAttribute).Pos
	typ := p.parseTypeRef()
	name := p.next().Value
	p.expect(TokenSemicolon)

	return &Attribute{
		Name:     name,
		Type:     typ,
		Readonly: readonly,
		Static:   static,
		Pos:      pos,
	}
}

func (p *Parser) parseOperation(static bool, special string) *Operation {
	pos := p.peek().Pos

	// Return type
	ret := p.parseTypeRef()

	// Name (may be absent for special operations)
	name := ""
	if p.at(TokenIdent) || p.at(TokenIncludes) {
		name = p.next().Value
	}

	p.expect(TokenLParen)
	params := p.parseParams()
	p.expect(TokenRParen)
	p.expect(TokenSemicolon)

	return &Operation{
		Name:    name,
		Params:  params,
		Return:  ret,
		Static:  static,
		Special: special,
		Pos:     pos,
	}
}

func (p *Parser) parseConstructor() *Constructor {
	pos := p.expect(TokenConstructor).Pos
	p.expect(TokenLParen)
	params := p.parseParams()
	p.expect(TokenRParen)
	p.expect(TokenSemicolon)

	return &Constructor{
		Params: params,
		Pos:    pos,
	}
}

func (p *Parser) parseIterable() *Iterable {
	pos := p.expect(TokenIterable).Pos
	p.expect(TokenLAngle)
	first := p.parseTypeRef()
	it := &Iterable{ValueType: first, Pos: pos}
	if p.match(TokenComma) {
		it.KeyType = first
		it.ValueType = p.parseTypeRef()
	}
	p.expect(TokenRAngle)
	p.expect(TokenSemicolon)
	return it
}

// argumentNameKeywords is WebIDL's ArgumentNameKeyword production: the keywords
// that may also name an argument, and only an argument.
var argumentNameKeywords = map[string]bool{
	"async": true, "attribute": true, "callback": true, "const": true,
	"constructor": true, "deleter": true, "dictionary": true, "enum": true,
	"getter": true, "includes": true, "inherit": true, "interface": true,
	"iterable": true, "maplike": true, "mixin": true, "namespace": true,
	"partial": true, "readonly": true, "required": true, "setlike": true,
	"setter": true, "static": true, "stringifier": true, "typedef": true,
	"unrestricted": true,
}

// isArgumentName reports whether tok can name an argument: an identifier, or the
// keyword token of an ArgumentNameKeyword. A word the lexer does not treat as a
// keyword arrives as an identifier, so the second test needs no list of kinds.
func isArgumentName(tok Token) bool {
	return tok.Kind == TokenIdent || (argumentNameKeywords[tok.Value] && keywords[tok.Value] == tok.Kind)
}

func (p *Parser) parseParams() []*Param {
	var params []*Param
	for !p.at(TokenRParen) && !p.at(TokenEOF) {
		param := &Param{}

		// Optional/variadic modifiers
		if p.match(TokenOptional) {
			param.Optional = true
		}

		param.Type = p.parseTypeRef()

		// Variadic: ... before name
		if p.match(TokenEllipsis) {
			param.Variadic = true
		}

		if isArgumentName(p.peek()) {
			param.Name = p.next().Value
		}

		// Default value
		if p.match(TokenEquals) {
			param.Default = p.readDefaultValue()
			param.Optional = true
		}

		params = append(params, param)
		if !p.match(TokenComma) {
			break
		}
	}
	return params
}

func (p *Parser) readDefaultValue() string {
	// Read tokens until we hit , or ) or ;
	tok := p.peek()
	switch tok.Kind {
	case TokenString:
		p.next()
		return `"` + tok.Value + `"`
	case TokenInteger, TokenFloat:
		p.next()
		return tok.Value
	case TokenIdent:
		// Could be "true", "false", "null", "undefined", or an enum value
		p.next()
		return tok.Value
	case TokenLBrace:
		// Empty dictionary default: {}  (an empty sequence default [] is
		// already lexed as a single "[]" ident token, handled above).
		p.next()
		p.match(TokenRBrace)
		return "{}"
	default:
		// Skip complex default values
		p.next()
		return tok.Value
	}
}

func (p *Parser) parseTypeRef() *TypeRef {
	ref := p.parseSingleType()

	// Check for union: (Type or Type or ...)
	// Unions are actually parsed at the opening paren level
	// But also check for nullable suffix: ?
	if p.match(TokenQuestion) {
		ref.Nullable = true
	}

	return ref
}

func (p *Parser) parseSingleType() *TypeRef {
	// Skip an inline extended attribute preceding a type, e.g. the
	// [LegacyNullToEmptyString] in (TrustedHTML or [LegacyNullToEmptyString] DOMString).
	// A "[...]" ident is never a valid type name, so this is safe.
	p.tryParseExtAttrs()
	tok := p.peek()

	// Parenthesized union type: (Type or Type)
	if tok.Kind == TokenLParen {
		p.next()
		return p.parseUnionType()
	}

	// Parameterized types
	switch tok.Kind {
	case TokenSequence:
		p.next()
		p.expect(TokenLAngle)
		elem := p.parseTypeRef()
		p.expect(TokenRAngle)
		return &TypeRef{Kind: SequenceType, Elem: elem}

	case TokenFrozenArray:
		p.next()
		p.expect(TokenLAngle)
		elem := p.parseTypeRef()
		p.expect(TokenRAngle)
		return &TypeRef{Kind: FrozenArrayType, Elem: elem}

	case TokenObservableArray:
		p.next()
		p.expect(TokenLAngle)
		elem := p.parseTypeRef()
		p.expect(TokenRAngle)
		return &TypeRef{Kind: ObservableArrayType, Elem: elem}

	case TokenPromise:
		p.next()
		p.expect(TokenLAngle)
		elem := p.parseTypeRef()
		p.expect(TokenRAngle)
		return &TypeRef{Kind: PromiseType, Elem: elem}

	case TokenRecord:
		p.next()
		p.expect(TokenLAngle)
		key := p.parseTypeRef()
		p.expect(TokenComma)
		value := p.parseTypeRef()
		p.expect(TokenRAngle)
		return &TypeRef{Kind: RecordType, Key: key, Value: value}

	// Primitive types
	case TokenDOMString:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "DOMString"}
	case TokenByteString:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "ByteString"}
	case TokenUSVString:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "USVString"}
	case TokenBoolean:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "boolean"}
	case TokenByte:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "byte"}
	case TokenOctet:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "octet"}
	case TokenShort:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "short"}
	case TokenLong:
		p.next()
		// "long long" = i64
		if p.match(TokenLong) {
			return &TypeRef{Kind: BuiltinType, Builtin: "long long"}
		}
		return &TypeRef{Kind: BuiltinType, Builtin: "long"}
	case TokenUnsigned:
		p.next()
		// "unsigned short", "unsigned long", "unsigned long long"
		if p.match(TokenShort) {
			return &TypeRef{Kind: BuiltinType, Builtin: "unsigned short"}
		}
		if p.match(TokenLong) {
			if p.match(TokenLong) {
				return &TypeRef{Kind: BuiltinType, Builtin: "unsigned long long"}
			}
			return &TypeRef{Kind: BuiltinType, Builtin: "unsigned long"}
		}
		return &TypeRef{Kind: BuiltinType, Builtin: "unsigned long"} // fallback
	case TokenUnrestricted:
		p.next()
		if p.match(TokenFloatKw) {
			return &TypeRef{Kind: BuiltinType, Builtin: "unrestricted float"}
		}
		if p.match(TokenDouble) {
			return &TypeRef{Kind: BuiltinType, Builtin: "unrestricted double"}
		}
		return &TypeRef{Kind: BuiltinType, Builtin: "unrestricted double"} // fallback
	case TokenFloatKw:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "float"}
	case TokenDouble:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "double"}
	case TokenVoid:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "void"}
	case TokenUndefined:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "undefined"}
	case TokenAny:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "any"}
	case TokenObject:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "object"}
	case TokenSymbol:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "symbol"}
	case TokenBigint:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "bigint"}

	// Typed arrays and buffer types
	case TokenFloat32Array:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "Float32Array"}
	case TokenFloat64Array:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "Float64Array"}
	case TokenArrayBuffer:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "ArrayBuffer"}
	case TokenDataView:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "DataView"}
	case TokenInt8Array:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "Int8Array"}
	case TokenInt16Array:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "Int16Array"}
	case TokenInt32Array:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "Int32Array"}
	case TokenUint8Array:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "Uint8Array"}
	case TokenUint16Array:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "Uint16Array"}
	case TokenUint32Array:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "Uint32Array"}
	case TokenUint8ClampedArray:
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "Uint8ClampedArray"}

	case TokenIdent:
		p.next()
		return &TypeRef{Kind: NamedType, Name: tok.Value}

	default:
		p.errorf(tok.Pos, "expected type, got %s", tok.Kind)
		p.next()
		return &TypeRef{Kind: BuiltinType, Builtin: "any"} // error recovery
	}
}

func (p *Parser) parseUnionType() *TypeRef {
	ref := &TypeRef{Kind: UnionType}
	first := p.parseTypeRef()
	ref.Members = append(ref.Members, first)

	for p.match(TokenOr) {
		member := p.parseTypeRef()
		ref.Members = append(ref.Members, member)
	}
	p.expect(TokenRParen)
	return ref
}

// Merge applies partial interfaces, partial mixins, partial dictionaries and
// includes statements to the definitions they extend. It returns an error for
// each partial whose interface, mixin or dictionary is not in file, and for each
// side of an includes statement that is not, after applying every one that
// matches: nothing else would name the members they carry, so they would
// otherwise vanish unreported.
func Merge(file *File) []*ParseError {
	var errs []*ParseError
	unbound := func(pos Pos, what, name, kind string) {
		errs = append(errs, &ParseError{Pos: pos, Msg: fmt.Sprintf("%s: %s is not %s in the bound IDL files", what, name, kind)})
	}

	// Build lookup maps
	ifaceMap := make(map[string]*Interface)
	for _, iface := range file.Interfaces {
		ifaceMap[iface.Name] = iface
	}
	mixinMap := make(map[string]*Mixin)
	for _, m := range file.Mixins {
		mixinMap[m.Name] = m
	}
	// Also add "interface mixin" declarations to the mixin map
	for _, iface := range file.Interfaces {
		for _, ea := range iface.ExtAttrs {
			if ea.Name == "_mixin" {
				mixinMap[iface.Name] = &Mixin{
					Name:    iface.Name,
					Members: iface.Members,
				}
				break
			}
		}
	}

	// Apply partial interfaces
	for _, partial := range file.Partials {
		if partial.IsMixin {
			if m, ok := mixinMap[partial.Name]; ok {
				m.Members = append(m.Members, partial.Members...)
			} else {
				unbound(partial.Pos, "partial mixin "+partial.Name, partial.Name, "a mixin")
			}
		} else {
			if iface, ok := ifaceMap[partial.Name]; ok {
				iface.Members = append(iface.Members, partial.Members...)
			} else {
				unbound(partial.Pos, "partial interface "+partial.Name, partial.Name, "an interface")
			}
		}
	}

	// Apply partial dictionaries
	dictMap := make(map[string]*Dictionary)
	for _, d := range file.Dictionaries {
		dictMap[d.Name] = d
	}
	for _, pd := range file.PartialDicts {
		if d, ok := dictMap[pd.Name]; ok {
			d.Members = append(d.Members, pd.Members...)
		} else {
			unbound(pd.Pos, "partial dictionary "+pd.Name, pd.Name, "a dictionary")
		}
	}

	// Apply includes statements (merge mixin members into target interface)
	for _, inc := range file.Includes {
		statement := inc.Target + " includes " + inc.Mixin
		iface, ifaceOK := ifaceMap[inc.Target]
		if !ifaceOK {
			unbound(inc.Pos, statement, inc.Target, "an interface")
		}
		m, mixinOK := mixinMap[inc.Mixin]
		if !mixinOK {
			unbound(inc.Pos, statement, inc.Mixin, "a mixin")
		}
		if ifaceOK && mixinOK {
			iface.Members = append(iface.Members, m.Members...)
		}
	}
	return errs
}
