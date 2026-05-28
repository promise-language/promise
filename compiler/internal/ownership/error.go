package ownership

import (
	"fmt"

	"github.com/promise-language/promise/compiler/internal/ast"
)

// Error represents an ownership violation with source position.
type Error struct {
	Pos ast.Pos
	Msg string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Pos, e.Msg)
}

// errorf records an ownership error at the given position.
func (c *Checker) errorf(pos ast.Pos, format string, args ...any) {
	c.errors = append(c.errors, &Error{
		Pos: pos,
		Msg: fmt.Sprintf(format, args...),
	})
}
