package ast

// BinaryOp represents a binary operator.
type BinaryOp int

const (
	BinAdd            BinaryOp = iota // +
	BinSub                            // -
	BinMul                            // *
	BinDiv                            // /
	BinMod                            // %
	BinEq                             // ==
	BinNeq                            // !=
	BinLt                             // <
	BinGt                             // >
	BinLte                            // <=
	BinGte                            // >=
	BinAnd                            // &&
	BinOr                             // ||
	BinElvis                          // ?:
	BinExclusiveRange                 // ..
	BinInclusiveRange                 // ..=
	BinBitwiseAnd                     // &
	BinBitwiseOr                      // |
	BinBitwiseXor                     // ^
	BinLeftShift                      // <<
	BinRightShift                     // >>
)

func (op BinaryOp) String() string {
	switch op {
	case BinAdd:
		return "+"
	case BinSub:
		return "-"
	case BinMul:
		return "*"
	case BinDiv:
		return "/"
	case BinMod:
		return "%"
	case BinEq:
		return "=="
	case BinNeq:
		return "!="
	case BinLt:
		return "<"
	case BinGt:
		return ">"
	case BinLte:
		return "<="
	case BinGte:
		return ">="
	case BinAnd:
		return "&&"
	case BinOr:
		return "||"
	case BinElvis:
		return "?:"
	case BinExclusiveRange:
		return ".."
	case BinInclusiveRange:
		return "..="
	case BinBitwiseAnd:
		return "&"
	case BinBitwiseOr:
		return "|"
	case BinBitwiseXor:
		return "^"
	case BinLeftShift:
		return "<<"
	case BinRightShift:
		return ">>"
	default:
		return "?"
	}
}

// UnaryOp represents a unary operator.
type UnaryOp int

const (
	UnaryNeg        UnaryOp = iota // -
	UnaryNot                       // !
	UnaryReceive                   // <-
	UnaryBitwiseNot                // ~
)

func (op UnaryOp) String() string {
	switch op {
	case UnaryNeg:
		return "-"
	case UnaryNot:
		return "!"
	case UnaryReceive:
		return "<-"
	case UnaryBitwiseNot:
		return "~"
	default:
		return "?"
	}
}

// AssignOp represents an assignment operator.
type AssignOp int

const (
	OpAssign    AssignOp = iota // =
	OpAddAssign                 // +=
	OpSubAssign                 // -=
	OpMulAssign                 // *=
	OpDivAssign                 // /=
	OpModAssign                 // %=
)

func (op AssignOp) String() string {
	switch op {
	case OpAssign:
		return "="
	case OpAddAssign:
		return "+="
	case OpSubAssign:
		return "-="
	case OpMulAssign:
		return "*="
	case OpDivAssign:
		return "/="
	case OpModAssign:
		return "%="
	default:
		return "?"
	}
}

// RefModifier represents a reference modifier on parameters.
type RefModifier int

const (
	RefNone   RefModifier = iota
	RefShared             // &
	RefMut                // ~
)
