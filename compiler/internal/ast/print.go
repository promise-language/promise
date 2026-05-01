package ast

import (
	"fmt"
	"io"
	"strings"
)

// Print writes a human-readable representation of the AST to w.
func Print(w io.Writer, file *File) {
	p := &printer{w: w}
	p.printFile(file)
}

type printer struct {
	w      io.Writer
	indent int
}

func (p *printer) line(format string, args ...interface{}) {
	fmt.Fprintf(p.w, "%s%s\n", strings.Repeat("  ", p.indent), fmt.Sprintf(format, args...))
}

func (p *printer) printFile(f *File) {
	p.line("File")
	p.indent++
	for _, u := range f.Uses {
		p.line("Use %s = %q", u.Alias, u.Path)
	}
	for _, d := range f.Decls {
		p.printDecl(d)
	}
	p.indent--
}

func (p *printer) printDecl(d Decl) {
	switch n := d.(type) {
	case *FuncDecl:
		p.line("Func %s%s%s", n.Name, p.typeParamsStr(n.TypeParams), p.paramsStr(n.Params))
		p.indent++
		p.printReturnType(n.ReturnType)
		p.printAnnotations(n.Annotations)
		if n.Body != nil {
			p.printBlock(n.Body)
		}
		p.indent--
	case *TypeDecl:
		extra := ""
		if len(n.Inherits) > 0 {
			var names []string
			for _, t := range n.Inherits {
				names = append(names, p.typeRefStr(t))
			}
			extra = " : " + strings.Join(names, " + ")
		}
		p.line("Type %s%s%s", n.Name, p.typeParamsStr(n.TypeParams), extra)
		p.indent++
		p.printAnnotations(n.Annotations)
		for _, f := range n.Fields {
			def := ""
			if f.Default != nil {
				def = " = ..."
			}
			p.line("Field %s %s%s", p.typeRefStr(f.Type), f.Name, def)
		}
		for _, m := range n.Methods {
			abs := ""
			if m.Body == nil {
				abs = " (abstract)"
			}
			p.line("Method %s%s%s%s", m.Name, p.typeParamsStr(m.TypeParams), p.methodParamsStr(m.Receiver, m.Params), abs)
			if m.Body != nil {
				p.indent++
				p.printReturnType(m.ReturnType)
				p.printBlock(m.Body)
				p.indent--
			}
		}
		p.indent--
	case *EnumDecl:
		p.line("Enum %s%s", n.Name, p.typeParamsStr(n.TypeParams))
		p.indent++
		p.printAnnotations(n.Annotations)
		for _, v := range n.Variants {
			if len(v.Fields) == 0 {
				p.line("Variant %s", v.Name)
			} else {
				var fields []string
				for _, f := range v.Fields {
					fields = append(fields, fmt.Sprintf("%s %s", p.typeRefStr(f.Type), f.Name))
				}
				p.line("Variant %s(%s)", v.Name, strings.Join(fields, ", "))
			}
		}
		p.indent--
	}
}

func (p *printer) printBlock(b *Block) {
	p.line("Block")
	p.indent++
	for _, s := range b.Stmts {
		p.printStmt(s)
	}
	p.indent--
}

func (p *printer) printStmt(s Stmt) {
	switch n := s.(type) {
	case *Block:
		p.printBlock(n)
	case *TypedVarDecl:
		if n.Value == nil {
			p.line("TypedVar %s %s", p.typeRefStr(n.Type), n.Name)
		} else {
			p.line("TypedVar %s %s = ...", p.typeRefStr(n.Type), n.Name)
		}
	case *InferredVarDecl:
		p.line("InferredVar %s := ...", n.Name)
	case *DestructureVarDecl:
		p.line("Destructure (%s) := ...", strings.Join(n.Names, ", "))
	case *UseVarDecl:
		p.line("UseVar %s := ...", n.Name)
	case *AssignStmt:
		p.line("Assign %s ...", n.Op)
	case *ReturnStmt:
		if n.Value != nil {
			p.line("Return ...")
		} else {
			p.line("Return")
		}
	case *RaiseStmt:
		p.line("Raise ...")
	case *YieldStmt:
		p.line("Yield ...")
	case *YieldDelegateStmt:
		p.line("YieldDelegate ...")
	case *BreakStmt:
		p.line("Break")
	case *ContinueStmt:
		p.line("Continue")
	case *ExprStmt:
		p.line("ExprStmt")
		p.indent++
		p.printExpr(n.Expr)
		p.indent--
	case *IfStmt:
		if n.Binding != "" {
			p.line("If unwrap %s", n.Binding)
		} else {
			p.line("If")
		}
		p.indent++
		p.printBlock(n.Body)
		if n.Else != nil {
			p.line("Else")
			p.indent++
			p.printStmt(n.Else)
			p.indent--
		}
		p.indent--
	case *ForInStmt:
		if n.Index != "" {
			p.line("ForIn %s, %s", n.Index, n.Binding)
		} else {
			p.line("ForIn %s", n.Binding)
		}
		p.indent++
		p.printBlock(n.Body)
		p.indent--
	case *ClassicForStmt:
		p.line("ClassicFor %s", n.InitName)
		p.indent++
		p.printBlock(n.Body)
		p.indent--
	case *InfiniteLoop:
		p.line("Loop")
		p.indent++
		p.printBlock(n.Body)
		p.indent--
	case *WhileStmt:
		p.line("While")
		p.indent++
		p.printBlock(n.Body)
		p.indent--
	case *WhileUnwrapStmt:
		p.line("WhileUnwrap %s", n.Binding)
		p.indent++
		p.printBlock(n.Body)
		p.indent--
	case *IncDecStmt:
		op := "++"
		if !n.IsInc {
			op = "--"
		}
		p.line("IncDec %s", op)
		p.indent++
		p.printExpr(n.Target)
		p.indent--
	default:
		p.line("Stmt<%T>", s)
	}
}

func (p *printer) printExpr(e Expr) {
	switch n := e.(type) {
	case *BinaryExpr:
		p.line("Binary %s", n.Op)
		p.indent++
		p.printExpr(n.Left)
		p.printExpr(n.Right)
		p.indent--
	case *UnaryExpr:
		p.line("Unary %s", n.Op)
		p.indent++
		p.printExpr(n.Operand)
		p.indent--
	case *CallExpr:
		p.line("Call")
		p.indent++
		p.printExpr(n.Callee)
		for _, a := range n.Args {
			if a.Name != "" {
				p.line("Arg %s:", a.Name)
			}
			p.indent++
			p.printExpr(a.Value)
			p.indent--
		}
		p.indent--
	case *MemberExpr:
		p.line("Member .%s", n.Field)
		p.indent++
		p.printExpr(n.Target)
		p.indent--
	case *OptionalChainExpr:
		p.line("OptionalChain ?.%s", n.Field)
		p.indent++
		p.printExpr(n.Target)
		p.indent--
	case *IndexExpr:
		p.line("Index")
		p.indent++
		p.printExpr(n.Target)
		p.printExpr(n.Index)
		for _, extra := range n.ExtraIndices {
			p.printExpr(extra)
		}
		p.indent--
	case *SliceExpr:
		p.line("Slice")
		p.indent++
		p.printExpr(n.Target)
		if n.Low != nil {
			p.printExpr(n.Low)
		}
		if n.High != nil {
			p.printExpr(n.High)
		}
		p.indent--
	case *IdentExpr:
		p.line("Ident %s", n.Name)
	case *IntLit:
		p.line("Int %s", n.Raw)
	case *FloatLit:
		p.line("Float %s", n.Raw)
	case *BoolLit:
		p.line("Bool %v", n.Value)
	case *NoneLit:
		p.line("None")
	case *StringLit:
		p.line("String %s", n.Raw)
	case *CharLit:
		p.line("Char %s", n.Raw)
	case *ThisExpr:
		p.line("This")
	case *ParenExpr:
		p.line("Paren")
		p.indent++
		p.printExpr(n.Expr)
		p.indent--
	case *TupleLit:
		p.line("Tuple(%d)", len(n.Elements))
	case *ArrayLit:
		p.line("Array(%d)", len(n.Elements))
	case *MapLit:
		p.line("Map(%d)", len(n.Entries))
	case *MatchExpr:
		p.line("Match (%d arms)", len(n.Arms))
	case *IfExpr:
		p.line("IfExpr")
	case *LambdaExpr:
		p.line("Lambda(%d params)", len(n.Params))
	case *GoExpr:
		p.line("Go")
	case *UnsafeExpr:
		p.line("Unsafe")
	case *IsExpr:
		p.line("Is")
	case *CastExpr:
		force := ""
		if n.Force {
			force = "!"
		}
		p.line("Cast%s %s", force, p.typeRefStr(n.Type))
	case *ErrorPropagateExpr:
		p.line("ErrorPropagate")
		p.indent++
		p.printExpr(n.Expr)
		p.indent--
	case *ErrorUnwrapExpr:
		p.line("ErrorUnwrap")
		p.indent++
		p.printExpr(n.Expr)
		p.indent--
	case *ErrorHandlerExpr:
		p.line("ErrorHandler")
	default:
		p.line("Expr<%T>", e)
	}
}

func (p *printer) printReturnType(rt *ReturnTypeSpec) {
	if rt == nil {
		return
	}
	err := ""
	if rt.CanError {
		err = "!"
	}
	typeName := "void"
	if rt.Type != nil {
		typeName = p.typeRefStr(rt.Type)
	}
	p.line("Returns %s%s", typeName, err)
}

func (p *printer) printAnnotations(anns []*MetaAnnotation) {
	for _, a := range anns {
		p.line("@%s", a.Name)
	}
}

func (p *printer) typeParamsStr(tps []*TypeParam) string {
	if len(tps) == 0 {
		return ""
	}
	var parts []string
	for _, tp := range tps {
		parts = append(parts, tp.Name)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func (p *printer) paramStr(param *Param) string {
	prefix := ""
	if param.IsVariadic {
		prefix = "..."
	}
	s := fmt.Sprintf("%s%s %s", prefix, p.typeRefStr(param.Type), param.Name)
	for _, a := range param.Annotations {
		s += " `" + a.Name
	}
	return s
}

func (p *printer) paramsStr(params []*Param) string {
	var parts []string
	for _, param := range params {
		parts = append(parts, p.paramStr(param))
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func (p *printer) methodParamsStr(recv *ReceiverParam, params []*Param) string {
	var parts []string
	if recv != nil {
		mod := ""
		switch recv.RefMod {
		case RefShared:
			mod = "&"
		case RefMut:
			mod = "~"
		}
		parts = append(parts, mod+"this")
	}
	for _, param := range params {
		parts = append(parts, p.paramStr(param))
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func (p *printer) typeRefStr(t TypeRef) string {
	if t == nil {
		return "?"
	}
	switch n := t.(type) {
	case *NamedTypeRef:
		if len(n.TypeArgs) > 0 {
			var args []string
			for _, a := range n.TypeArgs {
				args = append(args, p.typeRefStr(a))
			}
			return n.Name + "[" + strings.Join(args, ", ") + "]"
		}
		return n.Name
	case *QualifiedTypeRef:
		name := n.Module + "." + n.Name
		if len(n.TypeArgs) > 0 {
			var args []string
			for _, a := range n.TypeArgs {
				args = append(args, p.typeRefStr(a))
			}
			return name + "[" + strings.Join(args, ", ") + "]"
		}
		return name
	case *TupleTypeRef:
		var elems []string
		for _, e := range n.Elements {
			elems = append(elems, p.typeRefStr(e))
		}
		return "(" + strings.Join(elems, ", ") + ")"
	case *FunctionTypeRef:
		var params []string
		for _, param := range n.Params {
			params = append(params, p.typeRefStr(param))
		}
		return "(" + strings.Join(params, ", ") + ") -> " + p.typeRefStr(n.Return)
	case *SharedRefTypeRef:
		return p.typeRefStr(n.Inner) + "&"
	case *MutRefTypeRef:
		return p.typeRefStr(n.Inner) + "~"
	case *PointerTypeRef:
		return p.typeRefStr(n.Inner) + "*"
	case *OptionalTypeRef:
		return p.typeRefStr(n.Inner) + "?"
	case *SliceTypeRef:
		return p.typeRefStr(n.Element) + "[]"
	case *ArrayTypeRef:
		return p.typeRefStr(n.Element) + "[" + n.Size + "]"
	default:
		return fmt.Sprintf("<%T>", t)
	}
}
