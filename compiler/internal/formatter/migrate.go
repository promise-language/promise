package formatter

// migrateFailableTokens converts old-style failable syntax (foo() int!) to
// new-style (foo!() int) by rearranging tokens. The migration detects failable
// BANG tokens in old positions (after return type) and moves them to after the
// function/method/getter name.
//
// Detection heuristic: a BANG token in old position is followed by a backtick
// (annotation), LBRACE (body), or FAT_ARROW (expression body). This is safe
// because expression-level BANG (unwrap) is always followed by operators, dots,
// semicolons, or other expression tokens — never by annotations or body starts.
func migrateFailableTokens(tokens []token) []token {
	type edit struct {
		removeBangAt    int // index of BANG to remove
		insertBangAfter int // index of name token to insert BANG after
	}

	var edits []edit

	for i, tok := range tokens {
		if tok.kind != tkBang {
			continue
		}

		// Check if this BANG is followed by annotation/body start (skip newlines/comments)
		nextIdx := nextSignificantToken(tokens, i+1)
		if nextIdx < 0 {
			continue
		}
		nk := tokens[nextIdx].kind
		if nk != tkBacktick && nk != tkLBrace && nk != tkFatArrow {
			continue
		}

		// This BANG is likely an old-style failable marker.
		// Walk backward to find the function/method/getter name.
		nameIdx := findDeclName(tokens, i)
		if nameIdx < 0 {
			continue
		}

		edits = append(edits, edit{
			removeBangAt:    i,
			insertBangAfter: nameIdx,
		})
	}

	if len(edits) == 0 {
		return tokens
	}

	// Build sets for O(1) lookup during reconstruction.
	removeSet := make(map[int]bool, len(edits))
	insertAfterSet := make(map[int]bool, len(edits))
	for _, e := range edits {
		removeSet[e.removeBangAt] = true
		insertAfterSet[e.insertBangAfter] = true
	}

	result := make([]token, 0, len(tokens))
	for i, tok := range tokens {
		if removeSet[i] {
			continue // skip old BANG
		}
		result = append(result, tok)
		if insertAfterSet[i] {
			result = append(result, token{tkBang, "!"}) // insert new BANG
		}
	}

	return result
}

// nextSignificantToken returns the index of the next non-whitespace,
// non-comment token starting from idx. Returns -1 if none found.
func nextSignificantToken(tokens []token, idx int) int {
	for idx < len(tokens) {
		switch tokens[idx].kind {
		case tkNewline, tkLineComment, tkBlockComment:
			idx++
		default:
			return idx
		}
	}
	return -1
}

// prevSignificantToken returns the index of the previous non-whitespace,
// non-comment token starting from idx (going backward). Returns -1 if none.
func prevSignificantToken(tokens []token, idx int) int {
	for idx >= 0 {
		switch tokens[idx].kind {
		case tkNewline, tkLineComment, tkBlockComment:
			idx--
		default:
			return idx
		}
	}
	return -1
}

// findDeclName walks backward from a failable BANG token to find the
// function/method/getter name token that the BANG should be placed after.
// Returns the token index of the name, or -1 if not found.
//
// To avoid false positives (e.g., `match foo()! {` where BANG is error unwrap
// before a match block), we verify the found name is in a declaration context:
// the token before the name must be a newline (start of line) or "get"/"set".
func findDeclName(tokens []token, bangIdx int) int {
	// Walk backward over the return type tokens to find either:
	// - RPAREN (end of function/method params) → then find the name
	// - IDENT preceded by "get" → getter name
	k := prevSignificantToken(tokens, bangIdx-1)
	if k < 0 {
		return -1
	}

	// Skip optional ? suffix (optional return types like int?!)
	if tokens[k].kind == tkQuestion {
		k = prevSignificantToken(tokens, k-1)
		if k < 0 {
			return -1
		}
	}

	// Skip bracket pairs (generic types like Box[T]!, vector types like string[]!)
	if tokens[k].kind == tkRBracket {
		k = skipMatchingBracketBack(tokens, k)
		if k < 0 {
			return -1
		}
	}

	// Skip type name (IDENT)
	if k >= 0 && tokens[k].kind == tkIdent {
		// Check for qualified type: Mod.Type
		prev := prevSignificantToken(tokens, k-1)
		if prev >= 0 && tokens[prev].kind == tkDot {
			pp := prevSignificantToken(tokens, prev-1)
			if pp >= 0 && tokens[pp].kind == tkIdent {
				k = pp // skip past module prefix
			}
		}
		k = prevSignificantToken(tokens, k-1)
		if k < 0 {
			return -1
		}
	}

	// Now k should be at RPAREN (function/method) or the token before
	// a getter name.
	if tokens[k].kind == tkRParen {
		nameIdx := findFuncNameFromRParen(tokens, k)
		if nameIdx >= 0 && isDeclContext(tokens, nameIdx) {
			return nameIdx
		}
		return -1
	}

	// Check for getter pattern: at this point, k might be at a getter name
	// where prev is "get". But we already skipped the type IDENT, so k
	// is actually the getter name (since we walked back past the type).
	if tokens[k].kind == tkIdent {
		prev := prevSignificantToken(tokens, k-1)
		if prev >= 0 && tokens[prev].kind == tkIdent && tokens[prev].text == "get" {
			return k // getter name
		}
	}

	return -1
}

// isDeclContext checks that the token at nameIdx is in a declaration context
// (start of line or after type member indentation), not in the middle of an
// expression. Declarations start at the beginning of a line — the token before
// the name should be a newline, start-of-file, or "get"/"set" keyword.
func isDeclContext(tokens []token, nameIdx int) bool {
	if nameIdx == 0 {
		return true // start of file
	}
	// Look at what comes before the name (including comments/newlines)
	for k := nameIdx - 1; k >= 0; k-- {
		switch tokens[k].kind {
		case tkNewline:
			return true // name is at start of line
		case tkLineComment, tkBlockComment:
			continue // skip comments
		default:
			// If preceded by "get" or "set", it's a getter/setter declaration
			if tokens[k].kind == tkIdent && (tokens[k].text == "get" || tokens[k].text == "set") {
				return true
			}
			return false // preceded by some other token — not a declaration
		}
	}
	return true // reached start of file
}

// findFuncNameFromRParen walks backward from the closing RPAREN of a function's
// parameter list to find the function/method name. Returns the name token index.
func findFuncNameFromRParen(tokens []token, rparenIdx int) int {
	// Find matching LPAREN
	k := skipMatchingParenBack(tokens, rparenIdx)
	if k < 0 {
		return -1
	}

	// Now k is at LPAREN. Go before it.
	k = prevSignificantToken(tokens, k-1)
	if k < 0 {
		return -1
	}

	// Skip typeParams if present: [T, U: Constraint]
	if tokens[k].kind == tkRBracket {
		k = skipMatchingBracketBack(tokens, k)
		if k < 0 {
			return -1
		}
	}

	// Should be at the function/method name
	if tokens[k].kind == tkIdent {
		return k
	}

	// Operator method names: +, -, *, /, %, ==, !=, <, >, <=, >=, &&, ||, !,
	// &, |, ^, <<, >>, ~, ++, --, .., ..=
	if isOperatorMethodToken(tokens[k].kind) {
		return k
	}

	// Bracket operator methods: [], []=, [:], [:]=
	// These end with RBRACKET or ASSIGN after RBRACKET
	if tokens[k].kind == tkAssign {
		// Could be []= or [:]=
		prev := prevSignificantToken(tokens, k-1)
		if prev >= 0 && tokens[prev].kind == tkRBracket {
			start := skipMatchingBracketBack(tokens, prev)
			if start >= 0 {
				return start // return index of token before [
			}
		}
	}
	if tokens[k].kind == tkRBracket {
		start := skipMatchingBracketBack(tokens, k)
		if start >= 0 {
			return start // return index of token before [
		}
	}

	return -1
}

// skipMatchingBracketBack walks backward from a ] to find its matching [.
// Returns the index of the token BEFORE [, or -1 if not found.
func skipMatchingBracketBack(tokens []token, rbracketIdx int) int {
	depth := 1
	k := rbracketIdx - 1
	for k >= 0 && depth > 0 {
		switch tokens[k].kind {
		case tkRBracket:
			depth++
		case tkLBracket:
			depth--
		}
		if depth > 0 {
			k--
		}
	}
	if depth == 0 {
		// k is at matching [. Go before it.
		return prevSignificantToken(tokens, k-1)
	}
	return -1
}

// skipMatchingParenBack walks backward from a ) to find its matching (.
// Returns the index of (, or -1 if not found.
func skipMatchingParenBack(tokens []token, rparenIdx int) int {
	depth := 1
	k := rparenIdx - 1
	for k >= 0 && depth > 0 {
		switch tokens[k].kind {
		case tkRParen:
			depth++
		case tkLParen:
			depth--
		}
		if depth > 0 {
			k--
		}
	}
	if depth == 0 {
		return k // k is at matching (
	}
	return -1
}

// isOperatorMethodToken returns true if the token kind represents a valid
// operator that can be used as a method name.
func isOperatorMethodToken(k tokenKind) bool {
	switch k {
	case tkPlus, tkMinus, tkStar, tkSlash, tkPercent,
		tkEQ, tkNEQ, tkLT, tkGT, tkLTE, tkGTE,
		tkAnd, tkOr, tkBang,
		tkAmp, tkPipe, tkCaret, tkLShift, tkRShift, tkTilde,
		tkPlusPlus, tkMinusMinus,
		tkDotDot, tkDotDotEq:
		return true
	}
	return false
}
