package wit

import (
	"fmt"
	"strings"
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
	TokenArrow     // ->
	TokenEquals    // =
	TokenSlash     // /
	TokenAt        // @
	TokenStar      // *

	// Literals
	TokenIdent      // kebab-case identifier
	TokenSemver     // version like 0.2.0 (only in package decls)
	TokenDocComment // /// doc comment

	// Keywords
	TokenPackage
	TokenInterface
	TokenWorld
	TokenImport
	TokenExport
	TokenUse
	TokenType
	TokenRecord
	TokenVariant
	TokenEnum
	TokenFlags
	TokenResource
	TokenFunc
	TokenStatic
	TokenConstructor
	TokenAs
	TokenInclude
	TokenWith
	TokenUnderscore // _

	// Built-in types
	TokenU8
	TokenU16
	TokenU32
	TokenU64
	TokenS8
	TokenS16
	TokenS32
	TokenS64
	TokenF32
	TokenF64
	TokenBool
	TokenChar
	TokenString
	TokenList
	TokenOption
	TokenResult
	TokenTuple
	TokenOwn
	TokenBorrow
)

var tokenNames = map[TokenKind]string{
	TokenEOF:         "EOF",
	TokenLBrace:      "{",
	TokenRBrace:      "}",
	TokenLParen:      "(",
	TokenRParen:      ")",
	TokenLAngle:      "<",
	TokenRAngle:      ">",
	TokenColon:       ":",
	TokenSemicolon:   ";",
	TokenComma:       ",",
	TokenDot:         ".",
	TokenArrow:       "->",
	TokenEquals:      "=",
	TokenSlash:       "/",
	TokenAt:          "@",
	TokenStar:        "*",
	TokenIdent:       "identifier",
	TokenDocComment:  "doc-comment",
	TokenSemver:      "semver",
	TokenPackage:     "package",
	TokenInterface:   "interface",
	TokenWorld:       "world",
	TokenImport:      "import",
	TokenExport:      "export",
	TokenUse:         "use",
	TokenType:        "type",
	TokenRecord:      "record",
	TokenVariant:     "variant",
	TokenEnum:        "enum",
	TokenFlags:       "flags",
	TokenResource:    "resource",
	TokenFunc:        "func",
	TokenStatic:      "static",
	TokenConstructor: "constructor",
	TokenAs:          "as",
	TokenInclude:     "include",
	TokenWith:        "with",
	TokenUnderscore:  "_",
	TokenU8:          "u8",
	TokenU16:         "u16",
	TokenU32:         "u32",
	TokenU64:         "u64",
	TokenS8:          "s8",
	TokenS16:         "s16",
	TokenS32:         "s32",
	TokenS64:         "s64",
	TokenF32:         "f32",
	TokenF64:         "f64",
	TokenBool:        "bool",
	TokenChar:        "char",
	TokenString:      "string",
	TokenList:        "list",
	TokenOption:      "option",
	TokenResult:      "result",
	TokenTuple:       "tuple",
	TokenOwn:         "own",
	TokenBorrow:      "borrow",
}

func (k TokenKind) String() string {
	if s, ok := tokenNames[k]; ok {
		return s
	}
	return fmt.Sprintf("token(%d)", int(k))
}

var keywords = map[string]TokenKind{
	"package":     TokenPackage,
	"interface":   TokenInterface,
	"world":       TokenWorld,
	"import":      TokenImport,
	"export":      TokenExport,
	"use":         TokenUse,
	"type":        TokenType,
	"record":      TokenRecord,
	"variant":     TokenVariant,
	"enum":        TokenEnum,
	"flags":       TokenFlags,
	"resource":    TokenResource,
	"func":        TokenFunc,
	"static":      TokenStatic,
	"constructor": TokenConstructor,
	"as":          TokenAs,
	"include":     TokenInclude,
	"with":        TokenWith,
	"_":           TokenUnderscore,
}

var builtinTypes = map[string]TokenKind{
	"u8":     TokenU8,
	"u16":    TokenU16,
	"u32":    TokenU32,
	"u64":    TokenU64,
	"s8":     TokenS8,
	"s16":    TokenS16,
	"s32":    TokenS32,
	"s64":    TokenS64,
	"f32":    TokenF32,
	"f64":    TokenF64,
	"bool":   TokenBool,
	"char":   TokenChar,
	"string": TokenString,
	"list":   TokenList,
	"option": TokenOption,
	"result": TokenResult,
	"tuple":  TokenTuple,
	"own":    TokenOwn,
	"borrow": TokenBorrow,
}

// Token is a single lexer token.
type Token struct {
	Kind  TokenKind
	Value string
	Pos   Pos
}

// Lexer tokenizes WIT source text.
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
		// Line comment: // (but not ///)
		if r == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '/' {
			// Check if it's a doc comment (///) — don't skip those
			if l.pos+2 < len(l.src) && l.src[l.pos+2] == '/' {
				break
			}
			// Skip line comment
			for l.pos < len(l.src) && l.peek() != '\n' {
				l.advance()
			}
			continue
		}
		// Block comment: /* ... */
		if r == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '*' {
			l.advance() // /
			l.advance() // *
			depth := 1
			for l.pos < len(l.src) && depth > 0 {
				if l.peek() == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '*' {
					l.advance()
					l.advance()
					depth++
				} else if l.peek() == '*' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '/' {
					l.advance()
					l.advance()
					depth--
				} else {
					l.advance()
				}
			}
			continue
		}
		break
	}
}

func (l *Lexer) nextToken() Token {
	pos := l.currentPos()
	r := l.peek()

	// Doc comments (///)
	if r == '/' && l.pos+2 < len(l.src) && l.src[l.pos+1] == '/' && l.src[l.pos+2] == '/' {
		doc := l.readDocComment()
		return Token{Kind: TokenDocComment, Value: doc, Pos: pos}
	}

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
		l.advance()
		return Token{Kind: TokenDot, Value: ".", Pos: pos}
	case '=':
		l.advance()
		return Token{Kind: TokenEquals, Value: "=", Pos: pos}
	case '/':
		l.advance()
		return Token{Kind: TokenSlash, Value: "/", Pos: pos}
	case '@':
		l.advance()
		return Token{Kind: TokenAt, Value: "@", Pos: pos}
	case '*':
		l.advance()
		return Token{Kind: TokenStar, Value: "*", Pos: pos}
	case '-':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '>' {
			l.advance()
			l.advance()
			return Token{Kind: TokenArrow, Value: "->", Pos: pos}
		}
	}

	// Identifiers and keywords (kebab-case: [a-z][a-z0-9]*(-[a-z0-9]+)*)
	// Also handles %identifier (escaped keywords)
	if r == '%' || isIdentStart(r) {
		return l.readIdent(pos)
	}

	// Fallback: consume unknown character
	l.advance()
	return Token{Kind: TokenIdent, Value: string(r), Pos: pos}
}

func (l *Lexer) readIdent(pos Pos) Token {
	escaped := false
	if l.peek() == '%' {
		l.advance() // skip %
		escaped = true
	}
	start := l.pos
	for l.pos < len(l.src) {
		r := l.peek()
		if isIdentContinue(r) {
			l.advance()
		} else if r == '-' && l.pos+1 < len(l.src) && isIdentContinue(rune(l.src[l.pos+1])) {
			// kebab-case: hyphen followed by ident char
			l.advance() // -
		} else {
			break
		}
	}
	value := l.src[start:l.pos]

	if escaped {
		return Token{Kind: TokenIdent, Value: value, Pos: pos}
	}

	// Check keywords
	if kind, ok := keywords[value]; ok {
		return Token{Kind: kind, Value: value, Pos: pos}
	}
	// Check built-in types
	if kind, ok := builtinTypes[value]; ok {
		return Token{Kind: kind, Value: value, Pos: pos}
	}

	return Token{Kind: TokenIdent, Value: value, Pos: pos}
}

func (l *Lexer) readDocComment() string {
	var lines []string
	for l.pos < len(l.src) && l.peek() == '/' && l.pos+2 < len(l.src) && l.src[l.pos+1] == '/' && l.src[l.pos+2] == '/' {
		l.advance() // /
		l.advance() // /
		l.advance() // /
		// Skip optional leading space
		if l.pos < len(l.src) && l.peek() == ' ' {
			l.advance()
		}
		start := l.pos
		for l.pos < len(l.src) && l.peek() != '\n' {
			l.advance()
		}
		lines = append(lines, l.src[start:l.pos])
		// Skip newline
		if l.pos < len(l.src) && l.peek() == '\n' {
			l.advance()
		}
		// Skip whitespace before next doc comment line
		for l.pos < len(l.src) && (l.peek() == ' ' || l.peek() == '\t') {
			l.advance()
		}
	}
	return strings.Join(lines, "\n")
}

func isIdentStart(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (r > 127 && unicode.IsLetter(r))
}

func isIdentContinue(r rune) bool {
	return isIdentStart(r) || (r >= '0' && r <= '9')
}
