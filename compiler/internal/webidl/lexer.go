package webidl

import (
	"fmt"
	"unicode"
	"unicode/utf8"
)

// TokenKind identifies the type of a lexer token.
type TokenKind int

const (
	TokenEOF TokenKind = iota

	// Punctuation
	TokenLBrace    // {
	TokenRBrace    // }
	TokenLParen    // (
	TokenRParen    // )
	TokenLAngle    // <
	TokenRAngle    // >
	TokenColon     // :
	TokenSemicolon // ;
	TokenComma     // ,
	TokenDot       // .
	TokenEquals    // =
	TokenQuestion  // ?
	TokenEllipsis  // ...

	// Literals
	TokenIdent   // identifier
	TokenString  // "string literal"
	TokenInteger // integer literal
	TokenFloat   // float literal

	// Keywords
	TokenInterface
	TokenPartial
	TokenDictionary
	TokenEnum
	TokenCallback
	TokenTypedef
	TokenMixin
	TokenIncludes
	TokenInherit
	TokenAttribute
	TokenReadonly
	TokenConst
	TokenStatic
	TokenStringifier
	TokenGetter
	TokenSetter
	TokenDeleter
	TokenOptional
	TokenConstructor
	TokenIterable
	TokenAsync
	TokenRequired
	TokenOr // for union types

	// Built-in types
	TokenVoid
	TokenAny
	TokenObject
	TokenSymbol
	TokenUndefined
	TokenBoolean
	TokenByte
	TokenOctet
	TokenBigint
	TokenShort
	TokenLong
	TokenUnsigned
	TokenUnrestricted
	TokenDOMString
	TokenByteString
	TokenUSVString
	TokenSequence
	TokenFrozenArray
	TokenObservableArray
	TokenPromise
	TokenRecord
	TokenFloat32Array
	TokenFloat64Array
	TokenArrayBuffer
	TokenDataView
	TokenInt8Array
	TokenInt16Array
	TokenInt32Array
	TokenUint8Array
	TokenUint16Array
	TokenUint32Array
	TokenUint8ClampedArray
	TokenDouble
	TokenFloatKw
)

var tokenNames = map[TokenKind]string{
	TokenEOF:               "EOF",
	TokenLBrace:            "{",
	TokenRBrace:            "}",
	TokenLParen:            "(",
	TokenRParen:            ")",
	TokenLAngle:            "<",
	TokenRAngle:            ">",
	TokenColon:             ":",
	TokenSemicolon:         ";",
	TokenComma:             ",",
	TokenDot:               ".",
	TokenEquals:            "=",
	TokenQuestion:          "?",
	TokenEllipsis:          "...",
	TokenIdent:             "identifier",
	TokenString:            "string-literal",
	TokenInteger:           "integer",
	TokenFloat:             "float",
	TokenInterface:         "interface",
	TokenPartial:           "partial",
	TokenDictionary:        "dictionary",
	TokenEnum:              "enum",
	TokenCallback:          "callback",
	TokenTypedef:           "typedef",
	TokenMixin:             "mixin",
	TokenIncludes:          "includes",
	TokenInherit:           "inherit",
	TokenAttribute:         "attribute",
	TokenReadonly:          "readonly",
	TokenConst:             "const",
	TokenStatic:            "static",
	TokenStringifier:       "stringifier",
	TokenGetter:            "getter",
	TokenSetter:            "setter",
	TokenDeleter:           "deleter",
	TokenOptional:          "optional",
	TokenConstructor:       "constructor",
	TokenIterable:          "iterable",
	TokenAsync:             "async",
	TokenRequired:          "required",
	TokenOr:                "or",
	TokenVoid:              "void",
	TokenAny:               "any",
	TokenObject:            "object",
	TokenSymbol:            "symbol",
	TokenUndefined:         "undefined",
	TokenBoolean:           "boolean",
	TokenByte:              "byte",
	TokenOctet:             "octet",
	TokenBigint:            "bigint",
	TokenShort:             "short",
	TokenLong:              "long",
	TokenUnsigned:          "unsigned",
	TokenUnrestricted:      "unrestricted",
	TokenDOMString:         "DOMString",
	TokenByteString:        "ByteString",
	TokenUSVString:         "USVString",
	TokenSequence:          "sequence",
	TokenFrozenArray:       "FrozenArray",
	TokenObservableArray:   "ObservableArray",
	TokenPromise:           "Promise",
	TokenRecord:            "record",
	TokenFloat32Array:      "Float32Array",
	TokenFloat64Array:      "Float64Array",
	TokenArrayBuffer:       "ArrayBuffer",
	TokenDataView:          "DataView",
	TokenInt8Array:         "Int8Array",
	TokenInt16Array:        "Int16Array",
	TokenInt32Array:        "Int32Array",
	TokenUint8Array:        "Uint8Array",
	TokenUint16Array:       "Uint16Array",
	TokenUint32Array:       "Uint32Array",
	TokenUint8ClampedArray: "Uint8ClampedArray",
	TokenDouble:            "double",
	TokenFloatKw:           "float",
}

func (k TokenKind) String() string {
	if s, ok := tokenNames[k]; ok {
		return s
	}
	return fmt.Sprintf("token(%d)", int(k))
}

var keywords = map[string]TokenKind{
	"interface":   TokenInterface,
	"partial":     TokenPartial,
	"dictionary":  TokenDictionary,
	"enum":        TokenEnum,
	"callback":    TokenCallback,
	"typedef":     TokenTypedef,
	"mixin":       TokenMixin,
	"includes":    TokenIncludes,
	"inherit":     TokenInherit,
	"attribute":   TokenAttribute,
	"readonly":    TokenReadonly,
	"const":       TokenConst,
	"static":      TokenStatic,
	"stringifier": TokenStringifier,
	"getter":      TokenGetter,
	"setter":      TokenSetter,
	"deleter":     TokenDeleter,
	"optional":    TokenOptional,
	"constructor": TokenConstructor,
	"iterable":    TokenIterable,
	"async":       TokenAsync,
	"required":    TokenRequired,
	"or":          TokenOr,

	// Built-in type keywords
	"void":              TokenVoid,
	"any":               TokenAny,
	"object":            TokenObject,
	"symbol":            TokenSymbol,
	"undefined":         TokenUndefined,
	"boolean":           TokenBoolean,
	"byte":              TokenByte,
	"octet":             TokenOctet,
	"bigint":            TokenBigint,
	"short":             TokenShort,
	"long":              TokenLong,
	"unsigned":          TokenUnsigned,
	"unrestricted":      TokenUnrestricted,
	"DOMString":         TokenDOMString,
	"ByteString":        TokenByteString,
	"USVString":         TokenUSVString,
	"sequence":          TokenSequence,
	"FrozenArray":       TokenFrozenArray,
	"ObservableArray":   TokenObservableArray,
	"Promise":           TokenPromise,
	"record":            TokenRecord,
	"Float32Array":      TokenFloat32Array,
	"Float64Array":      TokenFloat64Array,
	"ArrayBuffer":       TokenArrayBuffer,
	"DataView":          TokenDataView,
	"Int8Array":         TokenInt8Array,
	"Int16Array":        TokenInt16Array,
	"Int32Array":        TokenInt32Array,
	"Uint8Array":        TokenUint8Array,
	"Uint16Array":       TokenUint16Array,
	"Uint32Array":       TokenUint32Array,
	"Uint8ClampedArray": TokenUint8ClampedArray,
	"double":            TokenDouble,
	"float":             TokenFloatKw,
}

// Token is a single lexer token.
type Token struct {
	Kind  TokenKind
	Value string
	Pos   Pos
}

// Lexer tokenizes WebIDL source text.
type Lexer struct {
	src    string
	file   string
	pos    int
	line   int
	col    int
	tokens []Token
	idx    int
}

// NewLexer creates a new lexer for the given source text.
func NewLexer(src, file string) *Lexer {
	l := &Lexer{
		src:  src,
		file: file,
		line: 1,
		col:  1,
	}
	l.tokenize()
	return l
}

func (l *Lexer) tokenize() {
	for {
		l.skipWhitespaceAndComments()
		if l.pos >= len(l.src) {
			l.tokens = append(l.tokens, Token{Kind: TokenEOF, Pos: l.currentPos()})
			return
		}
		tok := l.nextToken()
		l.tokens = append(l.tokens, tok)
	}
}

// Peek returns the current token without consuming it.
func (l *Lexer) Peek() Token {
	if l.idx >= len(l.tokens) {
		return Token{Kind: TokenEOF, Pos: l.currentPos()}
	}
	return l.tokens[l.idx]
}

// Next consumes and returns the current token.
func (l *Lexer) Next() Token {
	tok := l.Peek()
	if l.idx < len(l.tokens) {
		l.idx++
	}
	return tok
}

// PeekN looks ahead N tokens (0 = current).
func (l *Lexer) PeekN(n int) Token {
	idx := l.idx + n
	if idx >= len(l.tokens) {
		return Token{Kind: TokenEOF}
	}
	return l.tokens[idx]
}

func (l *Lexer) currentPos() Pos {
	return Pos{File: l.file, Line: l.line, Column: l.col}
}

func (l *Lexer) advance() rune {
	r, size := utf8.DecodeRuneInString(l.src[l.pos:])
	l.pos += size
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

func (l *Lexer) peek() rune {
	if l.pos >= len(l.src) {
		return 0
	}
	r, _ := utf8.DecodeRuneInString(l.src[l.pos:])
	return r
}

func (l *Lexer) skipWhitespaceAndComments() {
	for l.pos < len(l.src) {
		r := l.peek()
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			l.advance()
			continue
		}
		// Line comment: //
		if r == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '/' {
			for l.pos < len(l.src) && l.peek() != '\n' {
				l.advance()
			}
			continue
		}
		// Block comment: /* ... */
		if r == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '*' {
			l.advance() // /
			l.advance() // *
			for l.pos < len(l.src) {
				if l.peek() == '*' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '/' {
					l.advance() // *
					l.advance() // /
					break
				}
				l.advance()
			}
			continue
		}
		break
	}
}

func (l *Lexer) nextToken() Token {
	pos := l.currentPos()
	r := l.peek()

	// Punctuation
	switch r {
	case '{':
		l.advance()
		return Token{Kind: TokenLBrace, Value: "{", Pos: pos}
	case '}':
		l.advance()
		return Token{Kind: TokenRBrace, Value: "}", Pos: pos}
	case '(':
		l.advance()
		return Token{Kind: TokenLParen, Value: "(", Pos: pos}
	case ')':
		l.advance()
		return Token{Kind: TokenRParen, Value: ")", Pos: pos}
	case '<':
		l.advance()
		return Token{Kind: TokenLAngle, Value: "<", Pos: pos}
	case '>':
		l.advance()
		return Token{Kind: TokenRAngle, Value: ">", Pos: pos}
	case ':':
		l.advance()
		return Token{Kind: TokenColon, Value: ":", Pos: pos}
	case ';':
		l.advance()
		return Token{Kind: TokenSemicolon, Value: ";", Pos: pos}
	case ',':
		l.advance()
		return Token{Kind: TokenComma, Value: ",", Pos: pos}
	case '.':
		// Check for ellipsis (...)
		if l.pos+2 < len(l.src) && l.src[l.pos+1] == '.' && l.src[l.pos+2] == '.' {
			l.advance()
			l.advance()
			l.advance()
			return Token{Kind: TokenEllipsis, Value: "...", Pos: pos}
		}
		l.advance()
		return Token{Kind: TokenDot, Value: ".", Pos: pos}
	case '=':
		l.advance()
		return Token{Kind: TokenEquals, Value: "=", Pos: pos}
	case '?':
		l.advance()
		return Token{Kind: TokenQuestion, Value: "?", Pos: pos}
	case '[':
		// Extended attributes — read as bracketed content
		return l.readExtAttrBracket(pos)
	}

	// String literal
	if r == '"' {
		return l.readString(pos)
	}

	// Number literal (including negative)
	if r == '-' || (r >= '0' && r <= '9') {
		return l.readNumber(pos)
	}

	// Identifiers and keywords
	if isIdentStart(r) {
		return l.readIdent(pos)
	}

	// Fallback: consume unknown character
	l.advance()
	return Token{Kind: TokenIdent, Value: string(r), Pos: pos}
}

// readExtAttrBracket reads a [...] extended attribute block and emits
// individual tokens for the content. For simplicity, we emit the whole
// bracketed content as a single string token that can be parsed later.
func (l *Lexer) readExtAttrBracket(pos Pos) Token {
	l.advance() // [
	start := l.pos
	depth := 1
	for l.pos < len(l.src) && depth > 0 {
		r := l.peek()
		if r == '[' {
			depth++
		} else if r == ']' {
			depth--
			if depth == 0 {
				break
			}
		}
		l.advance()
	}
	value := l.src[start:l.pos]
	if l.pos < len(l.src) {
		l.advance() // ]
	}
	// Parse into ExtAttr tokens later; for now treat as ident with bracket content
	return Token{Kind: TokenIdent, Value: "[" + value + "]", Pos: pos}
}

func (l *Lexer) readString(pos Pos) Token {
	l.advance() // opening "
	start := l.pos
	for l.pos < len(l.src) && l.peek() != '"' {
		if l.peek() == '\\' {
			l.advance() // skip escape
		}
		l.advance()
	}
	value := l.src[start:l.pos]
	if l.pos < len(l.src) {
		l.advance() // closing "
	}
	return Token{Kind: TokenString, Value: value, Pos: pos}
}

func (l *Lexer) readNumber(pos Pos) Token {
	start := l.pos
	if l.peek() == '-' {
		l.advance()
	}
	// Check for hex: 0x
	if l.peek() == '0' && l.pos+1 < len(l.src) && (l.src[l.pos+1] == 'x' || l.src[l.pos+1] == 'X') {
		l.advance() // 0
		l.advance() // x
		for l.pos < len(l.src) && isHexDigit(l.peek()) {
			l.advance()
		}
		return Token{Kind: TokenInteger, Value: l.src[start:l.pos], Pos: pos}
	}
	isFloat := false
	for l.pos < len(l.src) && (l.peek() >= '0' && l.peek() <= '9') {
		l.advance()
	}
	if l.pos < len(l.src) && l.peek() == '.' {
		isFloat = true
		l.advance()
		for l.pos < len(l.src) && (l.peek() >= '0' && l.peek() <= '9') {
			l.advance()
		}
	}
	// Exponent
	if l.pos < len(l.src) && (l.peek() == 'e' || l.peek() == 'E') {
		isFloat = true
		l.advance()
		if l.pos < len(l.src) && (l.peek() == '+' || l.peek() == '-') {
			l.advance()
		}
		for l.pos < len(l.src) && (l.peek() >= '0' && l.peek() <= '9') {
			l.advance()
		}
	}
	value := l.src[start:l.pos]
	if isFloat {
		return Token{Kind: TokenFloat, Value: value, Pos: pos}
	}
	return Token{Kind: TokenInteger, Value: value, Pos: pos}
}

func (l *Lexer) readIdent(pos Pos) Token {
	start := l.pos
	for l.pos < len(l.src) {
		r := l.peek()
		if isIdentContinue(r) {
			l.advance()
		} else {
			break
		}
	}
	value := l.src[start:l.pos]

	if kind, ok := keywords[value]; ok {
		return Token{Kind: kind, Value: value, Pos: pos}
	}

	return Token{Kind: TokenIdent, Value: value, Pos: pos}
}

func isIdentStart(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (r > 127 && unicode.IsLetter(r))
}

func isIdentContinue(r rune) bool {
	return isIdentStart(r) || (r >= '0' && r <= '9')
}

func isHexDigit(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}
