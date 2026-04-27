package ast

// Block represents a brace-delimited block of statements.
type Block struct {
	nodeBase
	Stmts []Stmt
}

func (*Block) stmtTag() {}

// TypedVarDecl represents a typed variable declaration: Type name = expr;
type TypedVarDecl struct {
	nodeBase
	Type   TypeRef
	RefMod RefModifier
	Name   string
	Value  Expr
}

func (*TypedVarDecl) stmtTag() {}

// InferredVarDecl represents an inferred variable declaration: name := expr;
type InferredVarDecl struct {
	nodeBase
	Name  string
	Value Expr
}

func (*InferredVarDecl) stmtTag() {}

// DestructureVarDecl represents a destructuring declaration: (a, b) := expr;
type DestructureVarDecl struct {
	nodeBase
	Names []string
	Value Expr
}

func (*DestructureVarDecl) stmtTag() {}

// UseVarDecl represents a use binding declaration: use name := expr;
// The compiler automatically calls close() on the variable when the scope exits.
type UseVarDecl struct {
	nodeBase
	Name  string
	Value Expr
}

func (*UseVarDecl) stmtTag() {}

// AssignStmt represents an assignment statement: expr op= expr;
type AssignStmt struct {
	nodeBase
	Target Expr
	Op     AssignOp
	Value  Expr
}

func (*AssignStmt) stmtTag() {}

// ReturnStmt represents a return statement.
type ReturnStmt struct {
	nodeBase
	Value Expr // nil for bare return
}

func (*ReturnStmt) stmtTag() {}

// RaiseStmt represents a raise statement.
type RaiseStmt struct {
	nodeBase
	Value Expr
}

func (*RaiseStmt) stmtTag() {}

// YieldStmt represents a yield statement.
type YieldStmt struct {
	nodeBase
	Value Expr
}

func (*YieldStmt) stmtTag() {}

// YieldDelegateStmt represents a yield* statement.
type YieldDelegateStmt struct {
	nodeBase
	Value Expr
}

func (*YieldDelegateStmt) stmtTag() {}

// BreakStmt represents a break statement.
type BreakStmt struct {
	nodeBase
}

func (*BreakStmt) stmtTag() {}

// ContinueStmt represents a continue statement.
type ContinueStmt struct {
	nodeBase
}

func (*ContinueStmt) stmtTag() {}

// ExprStmt wraps an expression used as a statement.
type ExprStmt struct {
	nodeBase
	Expr Expr
}

func (*ExprStmt) stmtTag() {}

// IfStmt represents an if statement. For unwrap form (if val := expr),
// Binding is set and Init holds the expression. For regular form, Cond is set.
type IfStmt struct {
	nodeBase
	Cond    Expr   // non-nil for regular if
	Binding string // non-empty for if-unwrap
	Init    Expr   // non-nil for if-unwrap
	Body    *Block
	Else    Stmt // *IfStmt (else-if), *Block (else), or nil
}

func (*IfStmt) stmtTag() {}

// ForInStmt represents a for-in loop: for binding in iterable { }
type ForInStmt struct {
	nodeBase
	Binding  string // element binding
	Index    string // index binding, "" if none
	Iterable Expr
	Body     *Block
}

func (*ForInStmt) stmtTag() {}

// IncDecStmt represents an increment or decrement statement: x++; or x--;
type IncDecStmt struct {
	nodeBase
	Target Expr
	IsInc  bool // true for ++, false for --
}

func (*IncDecStmt) stmtTag() {}

// ClassicForStmt represents a classic for loop: for init; cond; update { }
type ClassicForStmt struct {
	nodeBase
	InitName     string
	InitType     TypeRef // nil for inferred (:=)
	InitValue    Expr
	Cond         Expr
	UpdateTarget Expr     // nil for expression-only update
	UpdateOp     AssignOp // only meaningful if UpdateTarget != nil and !UpdateIncDec
	UpdateValue  Expr     // nil when UpdateIncDec is true
	UpdateIncDec bool     // true when update is ++ or --
	UpdateIsInc  bool     // true for ++, false for --
	Body         *Block
}

func (*ClassicForStmt) stmtTag() {}

// InfiniteLoop represents an infinite for loop: for { }
type InfiniteLoop struct {
	nodeBase
	Body *Block
}

func (*InfiniteLoop) stmtTag() {}

// WhileStmt represents a while loop: while cond { }
type WhileStmt struct {
	nodeBase
	Cond Expr
	Body *Block
}

func (*WhileStmt) stmtTag() {}

// WhileUnwrapStmt represents a while-unwrap loop: while binding := expr { }
type WhileUnwrapStmt struct {
	nodeBase
	Binding string
	Value   Expr
	Body    *Block
}

func (*WhileUnwrapStmt) stmtTag() {}
