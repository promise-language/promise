package sema

import (
	"fmt"

	"github.com/promise-language/promise/compiler/internal/ast"
)

// Error represents a semantic error with source position.
type Error struct {
	Pos ast.Pos
	Msg string
	// Warning marks a diagnostic that does NOT fail the compile. Callers that
	// branch on "did anything go wrong so far" must consult hasErrors rather than
	// len(errors), which counts warnings too.
	Warning bool
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Pos, e.Msg)
}

// errorf records a semantic error at the given position.
func (c *Checker) errorf(pos ast.Pos, format string, args ...any) {
	c.errors = append(c.errors, &Error{
		Pos: pos,
		Msg: fmt.Sprintf(format, args...),
	})
}

// warnf records a semantic warning at the given position.
func (c *Checker) warnf(pos ast.Pos, format string, args ...any) {
	c.errors = append(c.errors, &Error{
		Pos:     pos,
		Msg:     "warning: " + fmt.Sprintf(format, args...),
		Warning: true,
	})
}

// hasErrors reports whether any recorded diagnostic actually fails the compile.
// Warnings live in the same slice, so len(c.errors) != "the compile is doomed".
func (c *Checker) hasErrors() bool {
	for _, err := range c.errors {
		if e, ok := err.(*Error); ok && e.Warning {
			continue
		}
		return true
	}
	return false
}
