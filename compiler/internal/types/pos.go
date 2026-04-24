package types

import "fmt"

// Pos represents a source position.
// This mirrors ast.Pos but avoids importing the ast package.
type Pos struct {
	File   string
	Line   int // 1-based
	Column int // 0-based
}

func (p Pos) IsValid() bool { return p.Line > 0 }

func (p Pos) String() string {
	if p.File != "" {
		return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Column)
	}
	return fmt.Sprintf("%d:%d", p.Line, p.Column)
}
