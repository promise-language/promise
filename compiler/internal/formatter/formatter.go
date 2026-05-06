// Package formatter implements a canonical source code formatter for Promise.
// It operates at the token level — lexing the source into tokens (including
// comments and whitespace), then re-emitting with canonical spacing and
// indentation. No configuration, no options, one canonical form.
package formatter

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// Format formats Promise source code into canonical form.
func Format(src []byte) []byte {
	tokens := tokenize(string(src))
	tokens = sortUseImports(tokens)
	return []byte(reformat(tokens))
}

// sortUseImports sorts consecutive use-import declarations alphabetically.
// A use-import is `use name;`, `use name as alias;`, or `use alias "url";`.
// Use-resource bindings (`use x := ...`) are left in place.
// Blank lines and non-use tokens break the sorting group.
func sortUseImports(tokens []token) []token {
	type useDecl struct {
		tokens  []token
		sortKey string
	}

	result := make([]token, 0, len(tokens))
	i := 0
	for i < len(tokens) {
		if !isUseImport(tokens, i) {
			result = append(result, tokens[i])
			i++
			continue
		}

		// Collect consecutive use-import lines
		var decls []useDecl
		for i < len(tokens) && isUseImport(tokens, i) {
			sortKey := ""
			if i+1 < len(tokens) && tokens[i+1].kind == tkIdent {
				sortKey = tokens[i+1].text
			}
			// Collect tokens from `use` to `;` (inclusive) + optional trailing comment
			start := i
			for i < len(tokens) && tokens[i].kind != tkSemi && tokens[i].kind != tkNewline {
				i++
			}
			if i < len(tokens) && tokens[i].kind == tkSemi {
				i++ // consume ;
			}
			// Include trailing line comment on the same line
			if i < len(tokens) && tokens[i].kind == tkLineComment {
				i++
			}
			decls = append(decls, useDecl{tokens: tokens[start:i], sortKey: sortKey})

			// Skip newlines between use declarations
			nlCount := 0
			nlStart := i
			for i < len(tokens) && tokens[i].kind == tkNewline {
				nlCount++
				i++
			}
			// Blank line (2+ newlines) or non-use-import: end group
			if nlCount >= 2 || !isUseImport(tokens, i) {
				i = nlStart // let the outer loop handle these newlines
				break
			}
		}

		// Sort by module name
		sort.SliceStable(decls, func(a, b int) bool {
			return decls[a].sortKey < decls[b].sortKey
		})

		// Emit sorted declarations with single newlines between them
		for j, d := range decls {
			result = append(result, d.tokens...)
			if j < len(decls)-1 {
				result = append(result, token{tkNewline, "\n"})
			}
		}
	}
	return result
}

// isUseImport checks if tokens[i] starts a use-import declaration.
func isUseImport(tokens []token, i int) bool {
	if i >= len(tokens) || tokens[i].kind != tkIdent || tokens[i].text != "use" {
		return false
	}
	j := i + 1
	if j >= len(tokens) || tokens[j].kind != tkIdent {
		return false
	}
	j++
	// Skip to next meaningful token
	for j < len(tokens) && tokens[j].kind == tkNewline {
		j++
	}
	if j >= len(tokens) {
		return false
	}
	// := means resource binding, not import
	if tokens[j].kind == tkWalrus {
		return false
	}
	return true
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

	// Regular string (handles {expr} interpolation)
	if ch == '"' {
		start := l.pos
		l.advance()
		l.skipStringBody()
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

// skipInterpolation skips over a string interpolation expression.
// Called after consuming the opening '{'. Handles nested braces, strings
// (regular, raw, triple-quoted), char literals, and comments.
func (l *lexer) skipInterpolation() {
	depth := 1
	for l.pos < len(l.src) && depth > 0 {
		c := l.src[l.pos]
		switch {
		case c == '{':
			depth++
			l.pos++
		case c == '}':
			depth--
			l.pos++
		case c == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '/':
			// Line comment — skip to end of line (B0094)
			l.pos += 2
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
		case c == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '*':
			// Block comment — skip to */ (B0094)
			l.pos += 2
			for l.pos+1 < len(l.src) {
				if l.src[l.pos] == '*' && l.src[l.pos+1] == '/' {
					l.pos += 2
					break
				}
				l.pos++
			}
		case c == '"' && l.pos+2 < len(l.src) && l.src[l.pos+1] == '"' && l.src[l.pos+2] == '"':
			// Triple-quoted string — no interpolation, scan to closing """ (B0095)
			l.pos += 3
			for l.pos+2 < len(l.src) {
				if l.src[l.pos] == '"' && l.src[l.pos+1] == '"' && l.src[l.pos+2] == '"' {
					l.pos += 3
					break
				}
				l.pos++
			}
		case c == '"':
			// Regular string literal — scan it, handling escapes and interpolation recursively
			l.pos++
			l.skipStringBody()
		case c == 'r' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '"':
			// Raw string — no escapes, no interpolation, scan to closing " (B0093)
			l.pos += 2
			for l.pos < len(l.src) && l.src[l.pos] != '"' {
				l.pos++
			}
			if l.pos < len(l.src) {
				l.pos++ // consume closing "
			}
		case c == '\'':
			// Char literal — scan past it
			l.pos++
			if l.pos < len(l.src) && l.src[l.pos] == '\\' {
				l.pos += 2
			} else if l.pos < len(l.src) {
				_, size := utf8.DecodeRuneInString(l.src[l.pos:])
				l.pos += size
			}
			if l.pos < len(l.src) && l.src[l.pos] == '\'' {
				l.pos++
			}
		default:
			l.pos++
		}
	}
}

// skipStringBody scans the body of a regular string (after the opening ").
// Handles escape sequences, {expr} interpolation (recursive), and closing ".
func (l *lexer) skipStringBody() {
	for l.pos < len(l.src) {
		nc := l.src[l.pos]
		if nc == '\\' {
			l.pos += 2
			continue
		}
		if nc == '{' {
			l.pos++
			l.skipInterpolation()
			continue
		}
		if nc == '"' {
			l.pos++
			break
		}
		l.pos++
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

// braceContext tracks what kind of block a { opened.
type braceContext int

const (
	ctxBlock  braceContext = iota // default: function body, if, for, etc.
	ctxMatch                      // match { arms... }
	ctxEnum                       // enum { variants... }
	ctxSelect                     // select { case: body... }
)

// selectSaveState saves select-specific state when entering a nested block.
type selectSaveState struct {
	inCaseBody bool
	depth      int
}

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
	forHeaderDepth       int            // >0 when inside for(...;...;...) header — suppress semi newlines
	inLambdaPipes        bool           // inside |...| lambda parameter list
	spacedAfterRBrace    bool           // true when } handler already emitted a space
	inOperatorMethodName bool           // true after emitting ]= as part of operator method name
	totalBracketDepth    int            // total [...] nesting depth
	sliceBracketStack    []int          // depths at which slice brackets were opened
	pendingBraceContext  braceContext   // context for the next { (set by match/enum keywords)
	braceStack           []braceContext // stack of brace contexts for trailing comma normalization
	lastContentPos       int            // output position after last non-comment content write

	// Select case indentation (B0138)
	inSelectCaseBody bool              // true when inside a select case body (extra indent active)
	selectDepth      int               // paren+bracket depth within current select block
	selectSaveStack  []selectSaveState // saved select state for nested blocks
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
		// Extra de-indent for select case body before regular indent-- (B0138)
		if f.inSelectCaseBody && f.currentBraceContext() == ctxSelect {
			f.indent--
			f.inSelectCaseBody = false
		}
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

		// Trailing comma normalization for match/enum (D0002)
		ctx := ctxBlock
		if n := len(f.braceStack); n > 0 {
			ctx = f.braceStack[n-1]
			f.braceStack = f.braceStack[:n-1]
		}
		if f.needsTrailingComma(ctx) {
			// Insert comma after the last non-comment content, not at end of line.
			// This handles trailing comments: "one" // comment → "one", // comment
			out := f.out.String()
			if f.lastContentPos > 0 && f.lastContentPos <= len(out) {
				f.out.Reset()
				f.out.WriteString(out[:f.lastContentPos])
				f.write(",")
				f.out.WriteString(out[f.lastContentPos:])
			}
		}

		f.writeIndent()
		f.write("}")
		f.lastContentPos = f.out.Len()
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
		// Restore select state from before this block (B0138)
		if n := len(f.selectSaveStack); n > 0 {
			saved := f.selectSaveStack[n-1]
			f.selectSaveStack = f.selectSaveStack[:n-1]
			f.inSelectCaseBody = saved.inCaseBody
			f.selectDepth = saved.depth
		}
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
		f.braceStack = append(f.braceStack, f.pendingBraceContext)
		// Save and reset select state for new block (B0138)
		f.selectSaveStack = append(f.selectSaveStack, selectSaveState{f.inSelectCaseBody, f.selectDepth})
		f.inSelectCaseBody = false
		f.selectDepth = 0
		f.pendingBraceContext = ctxBlock
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

// currentBraceContext returns the brace context at the top of the stack.
func (f *formatter) currentBraceContext() braceContext {
	if n := len(f.braceStack); n > 0 {
		return f.braceStack[n-1]
	}
	return ctxBlock
}

// isSelectCaseArm checks whether the current line (starting from f.pos) is a
// select case arm — i.e., it contains a colon at paren/bracket/brace depth 0
// before any semicolon, opening brace, or closing brace.
func (f *formatter) isSelectCaseArm() bool {
	depth := 0
	for i := f.pos; i < len(f.tokens); i++ {
		tk := f.tokens[i]
		if tk.kind == tkNewline {
			continue
		}
		switch tk.kind {
		case tkLParen, tkLBracket:
			depth++
		case tkLBrace:
			if depth == 0 {
				return false // block opening — not part of a case arm
			}
			depth++
		case tkRParen, tkRBracket:
			if depth > 0 {
				depth--
			}
		case tkRBrace:
			if depth > 0 {
				depth--
			} else {
				return false
			}
		case tkColon:
			if depth == 0 {
				return true
			}
		case tkSemi:
			if depth == 0 {
				return false
			}
		}
	}
	return false
}

// needsTrailingComma returns true if a trailing comma should be inserted before }.
func (f *formatter) needsTrailingComma(ctx braceContext) bool {
	p := f.prev.kind
	switch ctx {
	case ctxMatch:
		// Match arms are always comma-separated. Add comma unless already present,
		// empty block, or semicolon (shouldn't appear, but be safe).
		return p != tkComma && p != tkLBrace && p != tkSemi && p != 0
	case ctxEnum:
		// Enum variants are comma-separated, but methods end with } or ;.
		// Don't add comma after method bodies (}) or method signatures (;).
		return p != tkComma && p != tkLBrace && p != tkRBrace && p != tkSemi && p != 0
	}
	return false
}

func (f *formatter) emitRegular(tok token) {
	if !f.lineHasContent {
		// De-indent for select case arm (B0138): if we're in a select case body
		// and this new line starts a case arm, drop back to case-arm indent level.
		if f.inSelectCaseBody && f.currentBraceContext() == ctxSelect && f.isSelectCaseArm() {
			f.indent--
			f.inSelectCaseBody = false
		}
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
	f.lastContentPos = f.out.Len()
	// Track operator method name []= / [:]=
	if tok.kind == tkAssign && f.prev.kind == tkRBracket && (f.prevPrev.kind == tkLBracket || f.prevPrev.kind == tkColon) {
		f.inOperatorMethodName = true
	} else if f.inOperatorMethodName && tok.kind != tkAssign {
		f.inOperatorMethodName = false
	}

	// Track brace context for trailing comma normalization (D0002)
	if tok.kind == tkIdent {
		switch tok.text {
		case "match":
			f.pendingBraceContext = ctxMatch
		case "enum":
			f.pendingBraceContext = ctxEnum
		case "select":
			f.pendingBraceContext = ctxSelect
		case "if", "for", "while", "else", "go", "unsafe", "type":
			f.pendingBraceContext = ctxBlock
		}
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

	// Track select paren/bracket depth for case-arm colon detection (B0138)
	if f.currentBraceContext() == ctxSelect {
		if tok.kind == tkLParen || tok.kind == tkLBracket {
			f.selectDepth++
		} else if (tok.kind == tkRParen || tok.kind == tkRBracket) && f.selectDepth > 0 {
			f.selectDepth--
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
		// Indent case body after select case-arm colon (B0138)
		if f.currentBraceContext() == ctxSelect && f.selectDepth == 0 {
			if f.inSelectCaseBody {
				f.indent-- // close previous case body (same-line colon pair)
			}
			f.inSelectCaseBody = true
			f.indent++
		}
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
