package ast

import (
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/parser"
)

// countingErrorListener records how many syntax errors ANTLR reported.
type countingErrorListener struct {
	*antlr.DefaultErrorListener
	count int
	first string
}

func (l *countingErrorListener) SyntaxError(_ antlr.Recognizer, _ interface{}, _, _ int, msg string, _ antlr.RecognitionException) {
	if l.count == 0 {
		l.first = msg
	}
	l.count++
}

// parseSyntaxErrors parses src and reports how many syntax errors ANTLR raised.
func parseSyntaxErrors(t *testing.T, src string) (int, string) {
	t.Helper()
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	lexEl := &countingErrorListener{DefaultErrorListener: antlr.NewDefaultErrorListener()}
	lexer.AddErrorListener(lexEl)
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	parseEl := &countingErrorListener{DefaultErrorListener: antlr.NewDefaultErrorListener()}
	p.AddErrorListener(parseEl)
	p.CompilationUnit()
	if lexEl.count > 0 {
		return lexEl.count, lexEl.first
	}
	return parseEl.count, parseEl.first
}

// TestFunctionTypeSyntaxAccepted pins the grammar surface T1634 landed: the `!`
// prefix on a function type, in every position a type can appear, and `void` as
// the empty return.
func TestFunctionTypeSyntaxAccepted(t *testing.T) {
	sources := []string{
		`apply(!(int) -> int fn) {}`,
		`apply(!() -> void fn) {}`,
		`apply(!(int, string) -> bool fn) {}`,
		`maker() !(int) -> int { return boom; }`,
		`type Holder { !(string) -> int op; }`,
		`run() { !(int) -> int f = boom; }`,
		`apply((int) -> void cb) {}`,
		`apply(() -> void cb) {}`,
		`apply((!(int) -> int, int) -> int h) {}`,
		`apply(((int) -> void) -> void h) {}`,
		`apply((!(int) -> int)? o) {}`,
		`apply((!(int) -> int)[] fns) {}`,
	}
	for _, src := range sources {
		if n, msg := parseSyntaxErrors(t, src); n > 0 {
			t.Errorf("%s: expected no syntax error, got %d (first: %s)", src, n, msg)
		}
	}
}

// TestEmptyParenReturnTypeRejected pins the T1634/C decision: `void` is the only
// spelling for an empty return, and `()` is not a type. §9.6 previously showed
// `() -> ()`, which never parsed; the docs now spell it `() -> void`. If `()`
// is ever made a type, this test is the place that says the decision changed.
func TestEmptyParenReturnTypeRejected(t *testing.T) {
	sources := []string{
		`apply((int) -> () fn) {}`,
		`apply(() -> () fn) {}`,
		`apply(!(string) -> () fn) {}`,
		`maker() (int) -> () { return f; }`,
	}
	for _, src := range sources {
		if n, _ := parseSyntaxErrors(t, src); n == 0 {
			t.Errorf("%s: expected a syntax error — `()` is not a type, `void` is the empty return", src)
		}
	}
}

// TestTrailingBangFunctionTypeRejected pins the other half of the notation
// decision: failability prefixes the producer, so the trailing form is not
// accepted as a function type. `(int) -> int!` parses the `!` as postfix unwrap
// on the return type instead — it must not silently mean a failable callback.
func TestTrailingBangFunctionTypeRejected(t *testing.T) {
	// `(int) -> int!` in parameter position does not parse as a type at all.
	if n, _ := parseSyntaxErrors(t, `apply((int) -> int! fn) {}`); n == 0 {
		t.Error("expected a syntax error for the trailing-bang function type `(int) -> int!`")
	}
}
