// Package formatter implements a canonical source code formatter for Promise.
// It operates at the token level — lexing the source into tokens (including
// comments and whitespace), then re-emitting with canonical spacing and
// indentation. No configuration, no options, one canonical form.
package formatter

import (
	"strings"
	"unicode/utf8"
)

// Format formats Promise source code into canonical form.
func Format(src []byte) []byte {
	tokens := tokenize(string(src))
	return []byte(reformat(tokens))
}

// tokenKind classifies a token for formatting purposes.
type tokenKind int

const (
	tkEOF           tokenKind = iota
	tkIdent                   // identifiers and keywords
	tkInt                     // integer literal
	tkFloat                   // float literal
	tkString                  // string literal (all forms)
	tkChar                    // char literal
	tkLBrace                  // {
	tkRBrace                  // }
	tkLParen                  // (
	tkRParen                  // )
	tkLBracket                // [
	tkRBracket                // ]
	tkSemi                    // ;
	tkComma                   // ,
	tkDot                     // .
	tkColon                   // :
	tkBacktick                // `
	tkAssign                  // =
	tkWalrus                  // :=
	tkArrow                   // ->
	tkFatArrow                // =>
	tkDotDot                  // ..
	tkDotDotEq                // ..=
	tkEllipsis                // ...
	tkQuestionDot             // ?.
	tkQuestionColon           // ?:
	tkQuestion                // ?
	tkBang                    // !
	tkAmp                     // &
	tkTilde                   // ~
	tkPipe                    // |
	tkCaret                   // ^
	tkUnderscore              // _
	tkPlus                    // +
	tkMinus                   // -
	tkStar                    // *
	tkSlash                   // /
	tkPercent                 // %
	tkLT                      // <
	tkGT                      // >
	tkEQ                      // ==
	tkNEQ                     // !=
	tkLTE                     // <=
	tkGTE                     // >=
	tkAnd                     // &&
	tkOr                      // ||
	tkLShift                  // <<
	tkRShift                  // >>
	tkPlusPlus                // ++
	tkMinusMinus              // --
	tkPlusAssign              // +=
	tkMinusAssign             // -=
	tkStarAssign              // *=
	tkSlashAssign             // /=
	tkPercentAssign           // %=
	tkLArrow                  // <- (receive operator)
	tkLineComment             // // ...
	tkBlockComment            // /* ... */
	tkNewline                 // \n
)

type token struct {
	kind tokenKind
	text string
}

// --- Lexer ---

func tokenize(src string) []token {
	l := &lexer{src: src}
	var tokens []token
	for {
		tok := l.next()
		tokens = append(tokens, tok)
		if tok.kind == tkEOF {
			break
		}
	}
	return tokens
}

type lexer struct {
	src string
	pos int
}

func (l *lexer) peek() byte {
	if l.pos >= len(l.src) {
		return 0
	}
	return l.src[l.pos]
}

func (l *lexer) peekAt(offset int) byte {
	p := l.pos + offset
	if p >= len(l.src) {
		return 0
	}
	return l.src[p]
}

func (l *lexer) advance() byte {
	b := l.src[l.pos]
	l.pos++
	return b
}

func (l *lexer) next() token {
	// Skip spaces and tabs (not newlines)
	for l.pos < len(l.src) && (l.src[l.pos] == ' ' || l.src[l.pos] == '\t' || l.src[l.pos] == '\r') {
		l.pos++
	}
	if l.pos >= len(l.src) {
		return token{tkEOF, ""}
	}

	ch := l.peek()

	if ch == '\n' {
		l.advance()
		return token{tkNewline, "\n"}
	}

	// Line comment
	if ch == '/' && l.peekAt(1) == '/' {
		start := l.pos
		for l.pos < len(l.src) && l.src[l.pos] != '\n' {
			l.pos++
		}
		return token{tkLineComment, l.src[start:l.pos]}
	}

	// Block comment
	if ch == '/' && l.peekAt(1) == '*' {
		start := l.pos
		l.pos += 2
		for l.pos < len(l.src)-1 {
			if l.src[l.pos] == '*' && l.src[l.pos+1] == '/' {
				l.pos += 2
				return token{tkBlockComment, l.src[start:l.pos]}
			}
			l.pos++
		}
		l.pos = len(l.src)
		return token{tkBlockComment, l.src[start:l.pos]}
	}

	// Triple-quoted string
	if ch == '"' && l.peekAt(1) == '"' && l.peekAt(2) == '"' {
		start := l.pos
		l.pos += 3
		for l.pos < len(l.src)-2 {
			if l.src[l.pos] == '"' && l.src[l.pos+1] == '"' && l.src[l.pos+2] == '"' {
				l.pos += 3
				return token{tkString, l.src[start:l.pos]}
			}
			l.pos++
		}
		l.pos = len(l.src)
		return token{tkString, l.src[start:l.pos]}
	}

	// Raw string r"..."
	if ch == 'r' && l.peekAt(1) == '"' {
		start := l.pos
		l.pos += 2
		for l.pos < len(l.src) && l.src[l.pos] != '"' {
			l.pos++
		}
		if l.pos < len(l.src) {
			l.pos++
		}
		return token{tkString, l.src[start:l.pos]}
	}

	// Regular string
	if ch == '"' {
		start := l.pos
		l.advance()
		for l.pos < len(l.src) {
			c := l.src[l.pos]
			if c == '\\' {
				l.pos += 2
				continue
			}
			if c == '"' {
				l.pos++
				break
			}
			l.pos++
		}
		return token{tkString, l.src[start:l.pos]}
	}

	// Char literal
	if ch == '\'' {
		start := l.pos
		l.advance()
		if l.pos < len(l.src) && l.src[l.pos] == '\\' {
			l.pos += 2
		} else if l.pos < len(l.src) {
			_, size := utf8.DecodeRuneInString(l.src[l.pos:])
			l.pos += size
		}
		if l.pos < len(l.src) && l.src[l.pos] == '\'' {
			l.pos++
		}
		return token{tkChar, l.src[start:l.pos]}
	}

	// Numbers
	if ch >= '0' && ch <= '9' {
		return l.lexNumber()
	}

	// Identifiers and keywords
	if isIdentStart(ch) {
		start := l.pos
		for l.pos < len(l.src) && isIdentCont(l.src[l.pos]) {
			l.pos++
		}
		return token{tkIdent, l.src[start:l.pos]}
	}

	// Multi-char operators (longest first)
	if l.pos+2 < len(l.src) {
		three := l.src[l.pos : l.pos+3]
		if three == "..." {
			l.pos += 3
			return token{tkEllipsis, "..."}
		}
		if three == "..=" {
			l.pos += 3
			return token{tkDotDotEq, "..="}
		}
	}
	if l.pos+1 < len(l.src) {
		two := l.src[l.pos : l.pos+2]
		var kind tokenKind
		switch two {
		case "..":
			kind = tkDotDot
		case "?.":
			kind = tkQuestionDot
		case "?:":
			kind = tkQuestionColon
		case "->":
			kind = tkArrow
		case "=>":
			kind = tkFatArrow
		case "<<":
			kind = tkLShift
		case ">>":
			kind = tkRShift
		case "==":
			kind = tkEQ
		case "!=":
			kind = tkNEQ
		case "<=":
			kind = tkLTE
		case ">=":
			kind = tkGTE
		case "&&":
			kind = tkAnd
		case "||":
			kind = tkOr
		case "++":
			kind = tkPlusPlus
		case "--":
			kind = tkMinusMinus
		case "+=":
			kind = tkPlusAssign
		case "-=":
			kind = tkMinusAssign
		case "*=":
			kind = tkStarAssign
		case "/=":
			kind = tkSlashAssign
		case "%=":
			kind = tkPercentAssign
		case ":=":
			kind = tkWalrus
		case "<-":
			kind = tkLArrow
		}
		if kind != tkEOF {
			l.pos += 2
			return token{kind, two}
		}
	}

	// Single-char tokens
	l.advance()
	switch ch {
	case '{':
		return token{tkLBrace, "{"}
	case '}':
		return token{tkRBrace, "}"}
	case '(':
		return token{tkLParen, "("}
	case ')':
		return token{tkRParen, ")"}
	case '[':
		return token{tkLBracket, "["}
	case ']':
		return token{tkRBracket, "]"}
	case ';':
		return token{tkSemi, ";"}
	case ',':
		return token{tkComma, ","}
	case '.':
		return token{tkDot, "."}
	case ':':
		return token{tkColon, ":"}
	case '`':
		return token{tkBacktick, "`"}
	case '=':
		return token{tkAssign, "="}
	case '<':
		return token{tkLT, "<"}
	case '>':
		return token{tkGT, ">"}
	case '+':
		return token{tkPlus, "+"}
	case '-':
		return token{tkMinus, "-"}
	case '*':
		return token{tkStar, "*"}
	case '/':
		return token{tkSlash, "/"}
	case '%':
		return token{tkPercent, "%"}
	case '!':
		return token{tkBang, "!"}
	case '&':
		return token{tkAmp, "&"}
	case '~':
		return token{tkTilde, "~"}
	case '|':
		return token{tkPipe, "|"}
	case '^':
		return token{tkCaret, "^"}
	case '?':
		return token{tkQuestion, "?"}
	case '_':
		return token{tkUnderscore, "_"}
	}
	return token{tkIdent, string(ch)}
}

func (l *lexer) lexNumber() token {
	start := l.pos
	ch := l.advance()

	if ch == '0' && l.pos < len(l.src) {
		next := l.src[l.pos]
		if next == 'x' || next == 'X' || next == 'o' || next == 'O' || next == 'b' || next == 'B' {
			l.advance()
			for l.pos < len(l.src) && isHexDigitOrUnderscore(l.src[l.pos]) {
				l.pos++
			}
			l.lexIntSuffix()
			return token{tkInt, l.src[start:l.pos]}
		}
	}

	for l.pos < len(l.src) && (isDigit(l.src[l.pos]) || l.src[l.pos] == '_') {
		l.pos++
	}

	if l.pos < len(l.src) && l.src[l.pos] == '.' {
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '.' {
			l.lexIntSuffix()
			return token{tkInt, l.src[start:l.pos]}
		}
		if l.pos+1 < len(l.src) && isDigit(l.src[l.pos+1]) {
			l.advance()
			for l.pos < len(l.src) && (isDigit(l.src[l.pos]) || l.src[l.pos] == '_') {
				l.pos++
			}
			if l.pos < len(l.src) && (l.src[l.pos] == 'e' || l.src[l.pos] == 'E') {
				l.advance()
				if l.pos < len(l.src) && (l.src[l.pos] == '+' || l.src[l.pos] == '-') {
					l.advance()
				}
				for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
					l.pos++
				}
			}
			l.lexFloatSuffix()
			return token{tkFloat, l.src[start:l.pos]}
		}
	}

	if l.pos < len(l.src) && (l.src[l.pos] == 'e' || l.src[l.pos] == 'E') {
		l.advance()
		if l.pos < len(l.src) && (l.src[l.pos] == '+' || l.src[l.pos] == '-') {
			l.advance()
		}
		for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			l.pos++
		}
		l.lexFloatSuffix()
		return token{tkFloat, l.src[start:l.pos]}
	}

	l.lexIntSuffix()
	return token{tkInt, l.src[start:l.pos]}
}

func (l *lexer) lexIntSuffix() {
	if l.pos >= len(l.src) {
		return
	}
	ch := l.src[l.pos]
	if ch == 'i' || ch == 'u' {
		rest := l.src[l.pos:]
		for _, suf := range []string{"i64", "i32", "i16", "i8", "u64", "u32", "u16", "u8"} {
			if strings.HasPrefix(rest, suf) {
				l.pos += len(suf)
				return
			}
		}
		// Bare 'i' (int) or 'u' (uint) suffix
		l.pos++
	}
}

func (l *lexer) lexFloatSuffix() {
	if l.pos >= len(l.src) {
		return
	}
	rest := l.src[l.pos:]
	if strings.HasPrefix(rest, "f64") || strings.HasPrefix(rest, "f32") {
		l.pos += 3
	}
}

func isIdentStart(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_'
}

func isIdentCont(ch byte) bool {
	return isIdentStart(ch) || (ch >= '0' && ch <= '9')
}

func isDigit(ch byte) bool { return ch >= '0' && ch <= '9' }

func isHexDigitOrUnderscore(ch byte) bool {
	return isDigit(ch) || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F') || ch == '_'
}

// --- Reformatter ---

func reformat(tokens []token) string {
	f := &formatter{tokens: tokens}
	f.format()
	return f.out.String()
}

type formatter struct {
	tokens []token
	pos    int
	out    strings.Builder
	indent int

	// State for spacing decisions
	prev     token // previous non-newline/comment token emitted
	prevPrev token // token before prev (for 2-back context)
	prevEmit token // very last token emitted (including comments)

	// Line state
	lineHasContent bool // current line has non-whitespace content
	pendingNLs     int  // pending newlines from source (for blank line detection)
	afterOpen      bool // just emitted { and newline (suppress blank line after open brace)

	// Context tracking
	forHeaderDepth       int   // >0 when inside for(...;...;...) header — suppress semi newlines
	inLambdaPipes        bool  // inside |...| lambda parameter list
	spacedAfterRBrace    bool  // true when } handler already emitted a space
	inOperatorMethodName bool  // true after emitting ]= as part of operator method name
	totalBracketDepth    int   // total [...] nesting depth
	sliceBracketStack    []int // depths at which slice brackets were opened
}

func (f *formatter) peek() token {
	if f.pos >= len(f.tokens) {
		return token{tkEOF, ""}
	}
	return f.tokens[f.pos]
}

func (f *formatter) consume() token {
	tok := f.tokens[f.pos]
	f.pos++
	return tok
}

// skipNewlines consumes all newlines, counting them for blank-line detection.
func (f *formatter) skipNewlines() {
	for f.pos < len(f.tokens) && f.tokens[f.pos].kind == tkNewline {
		f.pendingNLs++
		f.pos++
	}
}

func (f *formatter) write(s string) {
	f.out.WriteString(s)
}

func (f *formatter) newline() {
	f.write("\n")
	f.lineHasContent = false
}

func (f *formatter) writeIndent() {
	for i := 0; i < f.indent; i++ {
		f.write("  ")
	}
}

func (f *formatter) format() {
	for f.pos < len(f.tokens) {
		tok := f.peek()
		if tok.kind == tkEOF {
			break
		}
		if tok.kind == tkNewline {
			f.skipNewlines()
			continue
		}
		f.emitToken()
	}

	// Ensure exactly one trailing newline
	s := f.out.String()
	s = strings.TrimRight(s, "\n")
	if len(s) > 0 {
		s += "\n"
	}
	f.out.Reset()
	f.out.WriteString(s)
}

func (f *formatter) emitToken() {
	tok := f.peek()

	switch tok.kind {
	case tkLineComment:
		f.consume()
		text := normalizeComment(tok.text)
		if f.lineHasContent {
			// Trailing comment on same line
			f.write(" ")
			f.write(text)
			f.newline()
		} else {
			// Own-line comment — emit pending blank line if appropriate
			f.emitBlankLineIfNeeded()
			f.writeIndent()
			f.write(text)
			f.newline()
		}
		f.pendingNLs = 0
		f.afterOpen = false
		f.prevEmit = tok
		return

	case tkBlockComment:
		f.consume()
		if f.lineHasContent {
			f.write(" ")
		} else {
			f.emitBlankLineIfNeeded()
			f.writeIndent()
		}
		f.write(tok.text)
		f.lineHasContent = true
		f.prevPrev = f.prev
		f.prev = tok
		f.prevEmit = tok
		f.pendingNLs = 0
		f.afterOpen = false
		return

	case tkRBrace:
		f.consume()
		f.indent--
		if f.indent < 0 {
			f.indent = 0
		}
		if f.lineHasContent {
			f.newline()
		}
		// Trim trailing blank lines before }
		s := f.out.String()
		s = strings.TrimRight(s, "\n")
		if len(s) > 0 {
			s += "\n"
		}
		f.out.Reset()
		f.out.WriteString(s)
		f.lineHasContent = false

		f.writeIndent()
		f.write("}")
		f.lineHasContent = true
		// Peek ahead: if next non-newline is `else`, `}`, semi, comma, etc., stay on same line
		f.pendingNLs = 0
		f.skipNewlines()
		next := f.peek()
		if next.kind == tkEOF {
			f.newline()
		} else if next.kind == tkIdent && next.text == "else" {
			// } else { stays on same line
			f.write(" ")
			f.spacedAfterRBrace = true
		} else if next.kind == tkSemi || next.kind == tkComma || next.kind == tkRParen {
			// no newline
		} else if next.kind == tkQuestion || next.kind == tkBang {
			// }? or }! — postfix, no space
		} else if next.kind == tkDot || next.kind == tkQuestionDot {
			// }.method() chain
		} else {
			f.newline()
		}
		f.prevPrev = f.prev
		f.prev = tok
		f.prevEmit = tok
		// Don't reset pendingNLs — skipNewlines() above counted source newlines
		// after }, which the NEXT token needs to decide blank-line insertion.
		f.afterOpen = false
		return

	case tkLBrace:
		f.consume()

		// Empty map literal {:} — keep on one line
		if f.isEmptyMapLiteral() {
			if f.lineHasContent {
				f.write(" ")
			} else {
				f.emitBlankLineIfNeeded()
				f.writeIndent()
			}
			f.write("{:}")
			f.lineHasContent = true
			// Consume the : and } tokens (and any newlines between them)
			f.skipNewlines()
			f.pos++ // :
			f.skipNewlines()
			f.pos++ // }
			rbrace := token{tkRBrace, "}"}
			f.prevPrev = tok
			f.prev = rbrace
			f.prevEmit = rbrace
			f.pendingNLs = 0
			f.afterOpen = false
			f.skipNewlines()
			return
		}

		if f.lineHasContent {
			f.write(" ")
		} else {
			f.emitBlankLineIfNeeded()
			f.writeIndent()
		}
		f.write("{")
		f.lineHasContent = true
		f.newline()
		f.indent++
		f.prevPrev = f.prev
		f.prev = tok
		f.prevEmit = tok
		f.pendingNLs = 0
		f.afterOpen = true
		// Consume newlines after { (don't produce blank line)
		f.skipNewlines()
		return

	default:
		f.consume()
		f.emitRegular(tok)
	}
}

func (f *formatter) emitBlankLineIfNeeded() {
	// Emit one blank line if source had blank lines, unless we're right after {
	if f.pendingNLs >= 2 && !f.afterOpen && f.prev.kind != tkEOF && f.prev.kind != 0 {
		f.newline()
	}
}

func (f *formatter) emitRegular(tok token) {
	if !f.lineHasContent {
		// Starting a new line
		f.emitBlankLineIfNeeded()
		f.writeIndent()
		f.write(tok.text)
		f.lineHasContent = true
	} else {
		// Inline — determine spacing
		if f.needsSpace(f.prev, tok) {
			f.write(" ")
		}
		f.write(tok.text)
	}
	// Track operator method name []= / [:]=
	if tok.kind == tkAssign && f.prev.kind == tkRBracket && (f.prevPrev.kind == tkLBracket || f.prevPrev.kind == tkColon) {
		f.inOperatorMethodName = true
	} else if f.inOperatorMethodName && tok.kind != tkAssign {
		f.inOperatorMethodName = false
	}

	f.prevPrev = f.prev
	f.prev = tok
	f.prevEmit = tok
	f.pendingNLs = 0
	f.afterOpen = false
	f.spacedAfterRBrace = false

	// Track bracket depth for slice colon detection.
	// Push onto sliceBracketStack when a slice [ is opened; pop when its matching ] is hit.
	if tok.kind == tkLBracket {
		if f.isSliceBracket() {
			f.sliceBracketStack = append(f.sliceBracketStack, f.totalBracketDepth)
		}
		f.totalBracketDepth++
	} else if tok.kind == tkRBracket && f.totalBracketDepth > 0 {
		f.totalBracketDepth--
		// Pop if this ] closes a slice bracket
		if n := len(f.sliceBracketStack); n > 0 && f.sliceBracketStack[n-1] == f.totalBracketDepth {
			f.sliceBracketStack = f.sliceBracketStack[:n-1]
		}
	}

	// Track for-header context: `for ... ; ... ; ... {`
	if tok.kind == tkIdent && tok.text == "for" {
		// Peek ahead to see if this is a classic for (has semicolons before {)
		f.forHeaderDepth = f.detectForHeader()
	}

	// Track lambda pipe context: |params|
	// A pipe starts lambda params if the token before it is NOT value-producing
	// (e.g., after =, ->, move, (, start). At this point f.prev is already the pipe,
	// so check f.prevPrev for what preceded it.
	if tok.kind == tkPipe {
		if !f.inLambdaPipes && !isValue(f.prevPrev) {
			f.inLambdaPipes = true
		} else if f.inLambdaPipes {
			f.inLambdaPipes = false
		}
	}

	// Semicolon handling
	if tok.kind == tkSemi {
		if f.forHeaderDepth > 0 {
			f.forHeaderDepth--
			// Don't newline — stay on same line for classic for
		} else {
			f.skipNewlines()
			next := f.peek()
			if next.kind == tkLineComment {
				if f.pendingNLs > 0 {
					// Comment is on its own line — emit newline so it stays standalone
					f.newline()
				}
				// else: comment is on the same line as the semicolon — trailing
			} else {
				f.newline()
			}
		}
	}

	// Comma: newline if the source had newlines after it (match arms, enum fields, etc.)
	if tok.kind == tkComma {
		f.skipNewlines()
		if f.pendingNLs > 0 {
			f.newline()
		}
	}

	// Colon: newline if source had newlines after it (select cases, but not named args)
	if tok.kind == tkColon {
		f.skipNewlines()
		if f.pendingNLs > 0 {
			f.newline()
		}
	}
}

// isSliceBracket peeks ahead from the current [ to determine if it's a slice/index bracket
// (not a generic type parameter bracket). Scans to find : before ] at the same depth.
// Generic constraints are always [Ident: Type, ...] — a single ident before :.
// Slice expressions have [:], [expr:], or complex expressions (operators) before :.
func (f *formatter) isSliceBracket() bool {
	depth := 0
	hasNonIdent := false // saw a non-ident token before :
	tokenCount := 0      // number of non-newline tokens before :
	for i := f.pos; i < len(f.tokens); i++ {
		tk := f.tokens[i]
		if tk.kind == tkNewline {
			continue
		}
		if tk.kind == tkLBracket || tk.kind == tkLParen {
			if depth == 0 {
				hasNonIdent = true
			}
			depth++
			continue
		}
		if tk.kind == tkRBracket || tk.kind == tkRParen {
			if depth > 0 {
				depth--
				continue
			}
			// Hit ] at our level — no colon found, not a slice
			return false
		}
		if depth > 0 {
			continue
		}
		if tk.kind == tkColon {
			// [:...] — empty start slice
			if tokenCount == 0 {
				return true
			}
			// [expr op ... :] — has operators/non-idents, must be a slice
			if hasNonIdent {
				return true
			}
			// [ident:] — ambiguous (could be constraint or variable slice)
			// Treat as generic constraint (space after :)
			return false
		}
		tokenCount++
		if tk.kind != tkIdent {
			hasNonIdent = true
		}
	}
	return false
}

// isEmptyMapLiteral peeks ahead (skipping newlines) to check if the next tokens are `:` `}`.
func (f *formatter) isEmptyMapLiteral() bool {
	i := f.pos
	for i < len(f.tokens) && f.tokens[i].kind == tkNewline {
		i++
	}
	if i >= len(f.tokens) || f.tokens[i].kind != tkColon {
		return false
	}
	i++
	for i < len(f.tokens) && f.tokens[i].kind == tkNewline {
		i++
	}
	return i < len(f.tokens) && f.tokens[i].kind == tkRBrace
}

// detectForHeader checks if current `for` is a classic for (with semicolons).
// Returns the number of semicolons expected (2 for classic for, 0 otherwise).
func (f *formatter) detectForHeader() int {
	// Scan ahead (without consuming) looking for pattern: ... ; ... ; ... {
	semiCount := 0
	depth := 0
	for i := f.pos; i < len(f.tokens); i++ {
		tk := f.tokens[i]
		if tk.kind == tkNewline {
			continue
		}
		if tk.kind == tkLParen {
			depth++
		} else if tk.kind == tkRParen {
			depth--
		} else if tk.kind == tkLBrace {
			break
		} else if tk.kind == tkSemi && depth == 0 {
			semiCount++
		} else if tk.kind == tkEOF {
			break
		}
	}
	if semiCount == 2 {
		return 2
	}
	return 0
}

// needsSpace returns whether a space should separate prev from cur.
func (f *formatter) needsSpace(prev, cur token) bool {
	p := prev.kind
	c := cur.kind

	// Never space before semi, comma
	if c == tkSemi || c == tkComma {
		return false
	}

	// No space around . and ?.
	if c == tkDot || c == tkQuestionDot || p == tkDot || p == tkQuestionDot {
		return false
	}

	// No space after ( [ or before ) ]
	if p == tkLParen || p == tkLBracket {
		return false
	}
	if c == tkRParen || c == tkRBracket {
		return false
	}

	// Backtick: no space after `, space before `
	if p == tkBacktick {
		return false
	}
	if c == tkBacktick {
		return true
	}

	// Colon: no space before, space after
	// Exception: slice expressions inside [...] — no space after :
	if c == tkColon {
		return false
	}
	if p == tkColon {
		if len(f.sliceBracketStack) > 0 {
			return false
		}
		return true
	}

	// No space before ( if preceded by ident/)/]/> (function call/generic)
	// Space before ( if preceded by control keyword
	if c == tkLParen {
		if p == tkIdent {
			return isControlKeyword(prev.text)
		}
		if p == tkRParen || p == tkRBracket {
			return false
		}
		// Operator method names: []=( and [:]=( — no space before (
		if p == tkAssign && f.inOperatorMethodName {
			return false
		}
		// Unary prefix ops before ( — no space: !(expr), -(expr), ~(expr)
		if isUnaryPrefixOp(p) && !isValue(f.prevPrev) {
			return false
		}
		return true
	}

	// No space before [ if preceded by ident/)/] (indexing, generics)
	if c == tkLBracket {
		if p == tkIdent || p == tkRParen || p == tkRBracket || p == tkGT || p == tkQuestion || p == tkString {
			return false
		}
		return true
	}

	// ++ -- postfix: no space before
	if c == tkPlusPlus || c == tkMinusMinus {
		return false
	}

	// ? and ! as postfix after value-producing tokens: no space
	if c == tkQuestion && isValue(prev) {
		return false
	}
	if c == tkBang && isValue(prev) {
		return false
	}

	// Range operators: no space around
	if c == tkDotDot || c == tkDotDotEq || p == tkDotDot || p == tkDotDotEq {
		return false
	}

	// Ellipsis: no space after ..., but space before ... (e.g., ", ...string")
	if p == tkEllipsis {
		return false
	}

	// Pipe | in lambda params — no space inside |...|
	if f.inLambdaPipes && (c == tkPipe || p == tkPipe) {
		return false
	}

	// Unary prefix operators: no space between op and operand, but space BEFORE the op
	// ~ & ! - + <- are unary if the token before them is NOT value-producing
	if isUnaryPrefixOp(p) && !isValue(f.prevPrev) {
		return false
	}
	// & as binary AND: space around when preceded by a value
	// e.g., `a & b`, `c & !d`
	if p == tkAmp && isValue(f.prevPrev) {
		return true
	}
	if c == tkAmp && isValue(prev) {
		return true
	}

	// <- as prefix (receive): no space after
	if p == tkLArrow && !isValue(f.prevPrev) {
		return false
	}

	// & and ~ as ref modifiers before ident (e.g., &this, ~this): no space after
	if (p == tkAmp || p == tkTilde) && c == tkIdent {
		if f.prevPrev.kind == tkLParen || f.prevPrev.kind == tkComma {
			return false
		}
	}

	// Operator method names: []= and [:]= — no space around the = in the name
	// But NOT index assignment like m["a"] = 1 (prevPrev is the expression inside brackets)
	if c == tkAssign && p == tkRBracket && (f.prevPrev.kind == tkLBracket || f.prevPrev.kind == tkColon) {
		return false
	}
	// Binary operators: space on both sides
	// But NOT pipe | when used in lambda context (handled above)
	if isBinaryOp(c) || isBinaryOp(p) {
		// If prev is a binary op, always space after it (even before unary ops like -x)
		if isBinaryOp(p) {
			return true
		}
		// cur is a binary op — but some could be unary prefix
		if (c == tkMinus || c == tkPlus || c == tkStar || c == tkAmp || c == tkTilde || c == tkBang || c == tkLArrow) && !isValue(prev) {
			// Unary op after non-value, but still need space if prev is a word, binary op, or comma
			if isWord(prev) || p == tkBlockComment || isBinaryOp(p) || p == tkComma {
				return true
			}
			return false
		}
		return true
	}

	// Pipe | outside lambda context: space around (bitwise or)
	// Already handled by isBinaryOp above when not in lambda

	// After comma: space
	if p == tkComma {
		return true
	}

	// After semi: space (for classic for loops)
	if p == tkSemi {
		return true
	}

	// Block comment acts like a word for spacing purposes
	if p == tkBlockComment && (isWord(cur) || c == tkString || c == tkInt || c == tkFloat || c == tkChar) {
		return true
	}
	if c == tkBlockComment && isWord(prev) {
		return true
	}

	// Ident/value followed by ident/value/literal: space
	if isWord(prev) && isWord(cur) {
		return true
	}
	if isWord(prev) && (c == tkString || c == tkInt || c == tkFloat || c == tkChar) {
		return true
	}
	if (p == tkString || p == tkInt || p == tkFloat || p == tkChar) && isWord(cur) {
		return true
	}

	// After ) before ident, literal, {
	if p == tkRParen && (isWord(cur) || c == tkString || c == tkInt || c == tkFloat || c == tkLBrace) {
		return true
	}

	// After ] before ident, {
	if p == tkRBracket && (isWord(cur) || c == tkLBrace) {
		return true
	}

	// Before {
	if c == tkLBrace {
		return true
	}

	// After } — the } handler manages spacing for else/etc
	if p == tkRBrace && isWord(cur) {
		if f.spacedAfterRBrace {
			return false // } handler already wrote the space
		}
		return true
	}

	// After ? or ! (postfix) before ident
	if (p == tkQuestion || p == tkBang) && isWord(cur) {
		return true
	}

	// Word/keyword before unary prefix op (e.g., "if !flag", "return -1")
	if isWord(prev) && isUnaryPrefixOp(c) {
		return true
	}
	if isWord(prev) && c == tkLArrow {
		return true
	}

	return false
}

func isBinaryOp(k tokenKind) bool {
	switch k {
	case tkPlus, tkMinus, tkStar, tkSlash, tkPercent,
		tkEQ, tkNEQ, tkLT, tkGT, tkLTE, tkGTE,
		tkAnd, tkOr, tkLShift, tkRShift,
		tkAssign, tkWalrus,
		tkPlusAssign, tkMinusAssign, tkStarAssign, tkSlashAssign, tkPercentAssign,
		tkArrow, tkFatArrow,
		tkQuestionColon,
		tkCaret, tkPipe:
		return true
	}
	return false
}

func isUnaryPrefixOp(k tokenKind) bool {
	switch k {
	case tkBang, tkTilde, tkMinus, tkPlus, tkAmp, tkLArrow:
		return true
	}
	return false
}

// isValue returns true if the token produces a value (for postfix/binary vs unary disambiguation).
func isValue(tok token) bool {
	switch tok.kind {
	case tkIdent:
		// Control keywords don't produce values
		return !isControlKeyword(tok.text) && tok.text != "move"
	case tkInt, tkFloat, tkString, tkChar,
		tkRParen, tkRBracket, tkRBrace,
		tkPlusPlus, tkMinusMinus,
		tkUnderscore, tkQuestion, tkBang:
		return true
	}
	return false
}

// isWord returns true for tokens that are "word-like" (need space separation from other words).
func isWord(tok token) bool {
	switch tok.kind {
	case tkIdent, tkUnderscore:
		return true
	}
	return false
}

func isControlKeyword(text string) bool {
	switch text {
	case "if", "for", "while", "match", "select", "go", "else", "unsafe",
		"return", "raise", "yield", "in":
		return true
	}
	return false
}

func normalizeComment(text string) string {
	if strings.HasPrefix(text, "//") && len(text) > 2 && text[2] != ' ' && text[2] != '/' && text[2] != '!' {
		return "// " + text[2:]
	}
	return text
}
