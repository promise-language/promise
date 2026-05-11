package ast

// Node is the interface implemented by all AST nodes.
type Node interface {
	Pos() Pos
	End() Pos
}

// Decl is the interface for declaration nodes.
type Decl interface {
	Node
	declTag()
}

// Stmt is the interface for statement nodes.
type Stmt interface {
	Node
	stmtTag()
}

// Expr is the interface for expression nodes.
type Expr interface {
	Node
	exprTag()
}

// TypeRef is the interface for type reference nodes.
type TypeRef interface {
	Node
	typeRefTag()
}

// MatchPattern is the interface for match arm patterns.
type MatchPattern interface {
	Node
	matchPatternTag()
}

// IsPattern is the interface for patterns used with the `is` operator.
type IsPattern interface {
	Node
	isPatternTag()
}

// nodeBase provides shared position fields for all AST nodes.
type nodeBase struct {
	pos Pos
	end Pos
}

func (n nodeBase) Pos() Pos { return n.pos }
func (n nodeBase) End() Pos { return n.end }

// SetPosEnd sets the position and end of a node.
// Exported for use by the astcache package.
func (n *nodeBase) SetPosEnd(pos, end Pos) {
	n.pos = pos
	n.end = end
}
