package main

import (
	"fmt"
	"os"

	"github.com/antlr4-go/antlr/v4"

	"djabi.dev/go/promise_lang/internal/parser"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: promise <file.pr>")
		os.Exit(1)
	}

	filename := os.Args[1]

	input, err := antlr.NewFileStream(filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading %s: %v\n", filename, err)
		os.Exit(1)
	}

	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	lexEl := &errorListener{filename: filename}
	lexer.AddErrorListener(lexEl)

	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)

	p.RemoveErrorListeners()
	parseEl := &errorListener{filename: filename}
	p.AddErrorListener(parseEl)

	tree := p.CompilationUnit()

	if lexEl.errors+parseEl.errors > 0 {
		os.Exit(1)
	}

	fmt.Println(tree.ToStringTree(nil, p))
}

type errorListener struct {
	antlr.DefaultErrorListener
	filename string
	errors   int
}

func (l *errorListener) SyntaxError(
	_ antlr.Recognizer,
	_ interface{},
	line, column int,
	msg string,
	_ antlr.RecognitionException,
) {
	fmt.Fprintf(os.Stderr, "%s:%d:%d: %s\n", l.filename, line, column, msg)
	l.errors++
}
