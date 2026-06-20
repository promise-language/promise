package astcache

import (
	"encoding/binary"

	"github.com/promise-language/promise/compiler/internal/ast"
)

// encoder serializes an AST File to a binary format.
type encoder struct {
	buf     []byte
	strings map[string]uint32
	strs    []string
}

func newEncoder() *encoder {
	return &encoder{strings: make(map[string]uint32)}
}

func (e *encoder) stringID(s string) uint32 {
	if id, ok := e.strings[s]; ok {
		return id
	}
	id := uint32(len(e.strs))
	e.strings[s] = id
	e.strs = append(e.strs, s)
	return id
}

func (e *encoder) u8(v byte) { e.buf = append(e.buf, v) }
func (e *encoder) bool_(v bool) {
	if v {
		e.u8(1)
	} else {
		e.u8(0)
	}
}

func (e *encoder) u16(v uint16) {
	e.buf = append(e.buf, byte(v), byte(v>>8))
}

func (e *encoder) u32(v uint32) {
	e.buf = append(e.buf, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func (e *encoder) str(s string) { e.u32(e.stringID(s)) }

func (e *encoder) pos(p ast.Pos) {
	e.str(p.File)
	e.u32(uint32(p.Line))
	e.u16(uint16(p.Column))
}

func (e *encoder) nodePos(n ast.Node) {
	e.pos(n.Pos())
	e.pos(n.End())
}

// --- interface dispatch ---

func (e *encoder) expr(x ast.Expr) {
	if x == nil {
		e.u8(tagNil)
		return
	}
	switch n := x.(type) {
	case *ast.BinaryExpr:
		e.u8(tagBinaryExpr)
		e.nodePos(n)
		e.expr(n.Left)
		e.u8(byte(n.Op))
		e.expr(n.Right)
	case *ast.UnaryExpr:
		e.u8(tagUnaryExpr)
		e.nodePos(n)
		e.u8(byte(n.Op))
		e.expr(n.Operand)
	case *ast.CallExpr:
		e.u8(tagCallExpr)
		e.nodePos(n)
		e.expr(n.Callee)
		e.u32(uint32(len(n.Args)))
		for _, a := range n.Args {
			e.nodePos(a)
			e.str(a.Name)
			e.bool_(a.Move)
			e.expr(a.Value)
		}
	case *ast.IndexExpr:
		e.u8(tagIndexExpr)
		e.nodePos(n)
		e.expr(n.Target)
		e.expr(n.Index)
		e.u32(uint32(len(n.ExtraIndices)))
		for _, x := range n.ExtraIndices {
			e.expr(x)
		}
	case *ast.SliceExpr:
		e.u8(tagSliceExpr)
		e.nodePos(n)
		e.expr(n.Target)
		e.expr(n.Low)
		e.expr(n.High)
	case *ast.SliceTypeExpr:
		e.u8(tagSliceTypeExpr)
		e.nodePos(n)
		e.expr(n.Inner)
	case *ast.MemberExpr:
		e.u8(tagMemberExpr)
		e.nodePos(n)
		e.expr(n.Target)
		e.str(n.Field)
	case *ast.OptionalChainExpr:
		e.u8(tagOptionalChainExpr)
		e.nodePos(n)
		e.expr(n.Target)
		e.str(n.Field)
	case *ast.IsExpr:
		e.u8(tagIsExpr)
		e.nodePos(n)
		e.expr(n.Expr)
		e.isPattern(n.Pattern)
	case *ast.CastExpr:
		e.u8(tagCastExpr)
		e.nodePos(n)
		e.expr(n.Expr)
		e.typeRef(n.Type)
		e.bool_(n.Force)
	case *ast.ErrorPropagateExpr:
		e.u8(tagErrorPropagateExpr)
		e.nodePos(n)
		e.expr(n.Expr)
	case *ast.ErrorPanicExpr:
		e.u8(tagErrorPanicExpr)
		e.nodePos(n)
		e.expr(n.Expr)
	case *ast.OptionalUnwrapExpr:
		e.u8(tagOptionalUnwrapExpr)
		e.nodePos(n)
		e.expr(n.Expr)
	case *ast.AutoCloneExpr: // T0605: synth-only; never serialized (defensive)
		e.u8(tagAutoCloneExpr)
		e.nodePos(n)
		e.expr(n.Expr)
	case *ast.ErrorHandlerExpr:
		e.u8(tagErrorHandlerExpr)
		e.nodePos(n)
		e.expr(n.Expr)
		e.str(n.Binding)
		e.str(n.TypeName)
		e.typeRefs(n.TypeArgs)
		e.block(n.Body)
		e.str(n.ElseBinding)
		e.blockOpt(n.ElseBody)
		e.bool_(n.PanicOnNomatch)
	case *ast.IfExpr:
		e.u8(tagIfExpr)
		e.nodePos(n)
		e.expr(n.Cond)
		e.block(n.Then)
		e.block(n.Else)
	case *ast.MatchExpr:
		e.u8(tagMatchExpr)
		e.nodePos(n)
		e.expr(n.Subject)
		e.u32(uint32(len(n.Arms)))
		for _, a := range n.Arms {
			e.nodePos(a)
			e.matchPattern(a.Pattern)
			e.expr(a.Guard)
			e.expr(a.Body)
			e.blockOpt(a.Block)
		}
	case *ast.GoExpr:
		e.u8(tagGoExpr)
		e.nodePos(n)
		e.expr(n.Expr)
		e.blockOpt(n.Block)
	case *ast.UnsafeExpr:
		e.u8(tagUnsafeExpr)
		e.nodePos(n)
		e.block(n.Body)
	case *ast.LambdaExpr:
		e.u8(tagLambdaExpr)
		e.nodePos(n)
		e.bool_(n.Move)
		e.u32(uint32(len(n.Params)))
		for _, p := range n.Params {
			e.nodePos(p)
			e.typeRefOpt(p.Type)
			e.u8(byte(p.RefMod))
			e.str(p.Name)
		}
		e.typeRefOpt(n.ReturnType)
		e.blockOpt(n.Body)
		e.expr(n.ExprBody)
	case *ast.IntLit:
		e.u8(tagIntLit)
		e.nodePos(n)
		e.str(n.Raw)
		e.str(n.Suffix)
	case *ast.FloatLit:
		e.u8(tagFloatLit)
		e.nodePos(n)
		e.str(n.Raw)
		e.str(n.Suffix)
	case *ast.BoolLit:
		e.u8(tagBoolLit)
		e.nodePos(n)
		e.bool_(n.Value)
	case *ast.NoneLit:
		e.u8(tagNoneLit)
		e.nodePos(n)
	case *ast.CharLit:
		e.u8(tagCharLit)
		e.nodePos(n)
		e.str(n.Raw)
	case *ast.StringLit:
		e.u8(tagStringLit)
		e.nodePos(n)
		e.str(n.Raw)
		e.u8(byte(n.Kind))
		e.u32(uint32(len(n.Parts)))
		for _, p := range n.Parts {
			switch sp := p.(type) {
			case ast.StringText:
				e.u8(tagStringText)
				e.str(sp.Text)
			case ast.StringEscape:
				e.u8(tagStringEscape)
				e.str(sp.Sequence)
			case ast.StringInterp:
				e.u8(tagStringInterp)
				e.str(sp.Raw)
				e.expr(sp.Expr)
			}
		}
	case *ast.IdentExpr:
		e.u8(tagIdentExpr)
		e.nodePos(n)
		e.str(n.Name)
	case *ast.ThisExpr:
		e.u8(tagThisExpr)
		e.nodePos(n)
	case *ast.ParenExpr:
		e.u8(tagParenExpr)
		e.nodePos(n)
		e.expr(n.Expr)
	case *ast.TupleLit:
		e.u8(tagTupleLit)
		e.nodePos(n)
		e.exprs(n.Elements)
	case *ast.ArrayLit:
		e.u8(tagArrayLit)
		e.nodePos(n)
		e.exprs(n.Elements)
	case *ast.MapLit:
		e.u8(tagMapLit)
		e.nodePos(n)
		e.u32(uint32(len(n.Entries)))
		for _, en := range n.Entries {
			e.nodePos(en)
			e.expr(en.Key)
			e.expr(en.Value)
		}
	case *ast.EmptyBraceLit:
		e.u8(tagEmptyBraceLit)
		e.nodePos(n)
	case *ast.TypeRefExpr:
		e.u8(tagTypeRefExpr)
		e.nodePos(n)
		e.typeRef(n.Ref)
	}
}

func (e *encoder) stmt(s ast.Stmt) {
	if s == nil {
		e.u8(tagNil)
		return
	}
	switch n := s.(type) {
	case *ast.Block:
		e.u8(tagBlock)
		e.nodePos(n)
		e.stmts(n.Stmts)
	case *ast.TypedVarDecl:
		e.u8(tagTypedVarDecl)
		e.nodePos(n)
		e.typeRef(n.Type)
		e.u8(byte(n.RefMod))
		e.str(n.Name)
		e.expr(n.Value)
	case *ast.InferredVarDecl:
		e.u8(tagInferredVarDecl)
		e.nodePos(n)
		e.str(n.Name)
		e.expr(n.Value)
	case *ast.DestructureVarDecl:
		e.u8(tagDestructureVarDecl)
		e.nodePos(n)
		e.u32(uint32(len(n.Names)))
		for _, name := range n.Names {
			e.str(name)
		}
		e.expr(n.Value)
	case *ast.UseVarDecl:
		e.u8(tagUseVarDecl)
		e.nodePos(n)
		e.str(n.Name)
		e.expr(n.Value)
	case *ast.AssignStmt:
		e.u8(tagAssignStmt)
		e.nodePos(n)
		e.expr(n.Target)
		e.u8(byte(n.Op))
		e.expr(n.Value)
	case *ast.ReturnStmt:
		e.u8(tagReturnStmt)
		e.nodePos(n)
		e.expr(n.Value)
	case *ast.RaiseStmt:
		e.u8(tagRaiseStmt)
		e.nodePos(n)
		e.expr(n.Value)
	case *ast.YieldStmt:
		e.u8(tagYieldStmt)
		e.nodePos(n)
		e.expr(n.Value)
	case *ast.YieldDelegateStmt:
		e.u8(tagYieldDelegateStmt)
		e.nodePos(n)
		e.expr(n.Value)
	case *ast.BreakStmt:
		e.u8(tagBreakStmt)
		e.nodePos(n)
	case *ast.ContinueStmt:
		e.u8(tagContinueStmt)
		e.nodePos(n)
	case *ast.ExprStmt:
		e.u8(tagExprStmt)
		e.nodePos(n)
		e.expr(n.Expr)
	case *ast.IfStmt:
		e.u8(tagIfStmt)
		e.nodePos(n)
		e.expr(n.Cond)
		e.str(n.Binding)
		e.expr(n.Init)
		e.block(n.Body)
		e.stmt(n.Else)
	case *ast.ForInStmt:
		e.u8(tagForInStmt)
		e.nodePos(n)
		e.str(n.Binding)
		e.str(n.Index)
		e.expr(n.Iterable)
		e.block(n.Body)
	case *ast.IncDecStmt:
		e.u8(tagIncDecStmt)
		e.nodePos(n)
		e.expr(n.Target)
		e.bool_(n.IsInc)
	case *ast.ClassicForStmt:
		e.u8(tagClassicForStmt)
		e.nodePos(n)
		e.str(n.InitName)
		e.typeRefOpt(n.InitType)
		e.expr(n.InitValue)
		e.expr(n.Cond)
		e.expr(n.UpdateTarget)
		e.u8(byte(n.UpdateOp))
		e.expr(n.UpdateValue)
		e.bool_(n.UpdateIncDec)
		e.bool_(n.UpdateIsInc)
		e.block(n.Body)
	case *ast.InfiniteLoop:
		e.u8(tagInfiniteLoop)
		e.nodePos(n)
		e.block(n.Body)
	case *ast.WhileStmt:
		e.u8(tagWhileStmt)
		e.nodePos(n)
		e.expr(n.Cond)
		e.block(n.Body)
	case *ast.WhileUnwrapStmt:
		e.u8(tagWhileUnwrapStmt)
		e.nodePos(n)
		e.str(n.Binding)
		e.expr(n.Value)
		e.block(n.Body)
	case *ast.SelectStmt:
		e.u8(tagSelectStmt)
		e.nodePos(n)
		e.u32(uint32(len(n.Cases)))
		for _, c := range n.Cases {
			e.nodePos(c)
			e.bool_(c.IsSend)
			e.expr(c.Channel)
			e.expr(c.SendValue)
			e.str(c.Binding)
			e.stmts(c.Body)
		}
		if n.Default != nil {
			e.u8(1)
			e.stmts(n.Default)
		} else {
			e.u8(0)
		}
	}
}

func (e *encoder) typeRef(t ast.TypeRef) {
	if t == nil {
		e.u8(tagNil)
		return
	}
	switch n := t.(type) {
	case *ast.NamedTypeRef:
		e.u8(tagNamedTypeRef)
		e.nodePos(n)
		e.str(n.Name)
		e.typeRefs(n.TypeArgs)
	case *ast.QualifiedTypeRef:
		e.u8(tagQualifiedTypeRef)
		e.nodePos(n)
		e.str(n.Module)
		e.str(n.Name)
		e.typeRefs(n.TypeArgs)
	case *ast.TupleTypeRef:
		e.u8(tagTupleTypeRef)
		e.nodePos(n)
		e.typeRefs(n.Elements)
	case *ast.FunctionTypeRef:
		e.u8(tagFunctionTypeRef)
		e.nodePos(n)
		e.typeRefs(n.Params)
		e.typeRef(n.Return)
	case *ast.SharedRefTypeRef:
		e.u8(tagSharedRefTypeRef)
		e.nodePos(n)
		e.typeRef(n.Inner)
	case *ast.MutRefTypeRef:
		e.u8(tagMutRefTypeRef)
		e.nodePos(n)
		e.typeRef(n.Inner)
	case *ast.PointerTypeRef:
		e.u8(tagPointerTypeRef)
		e.nodePos(n)
		e.typeRef(n.Inner)
	case *ast.OptionalTypeRef:
		e.u8(tagOptionalTypeRef)
		e.nodePos(n)
		e.typeRef(n.Inner)
	case *ast.SliceTypeRef:
		e.u8(tagSliceTypeRef)
		e.nodePos(n)
		e.typeRef(n.Element)
	case *ast.ArrayTypeRef:
		e.u8(tagArrayTypeRef)
		e.nodePos(n)
		e.typeRef(n.Element)
		e.str(n.Size)
	}
}

func (e *encoder) matchPattern(p ast.MatchPattern) {
	if p == nil {
		e.u8(tagNil)
		return
	}
	switch n := p.(type) {
	case *ast.EnumDestructureMatchPattern:
		e.u8(tagEnumDestructureMatchPattern)
		e.nodePos(n)
		e.str(n.Enum)
		e.str(n.Variant)
		e.strSlice(n.Bindings)
	case *ast.EnumVariantMatchPattern:
		e.u8(tagEnumVariantMatchPattern)
		e.nodePos(n)
		e.str(n.Enum)
		e.str(n.Variant)
	case *ast.TypeBindingMatchPattern:
		e.u8(tagTypeBindingMatchPattern)
		e.nodePos(n)
		e.str(n.TypeName)
		e.str(n.Binding)
	case *ast.ShortDestructureMatchPattern:
		e.u8(tagShortDestructureMatchPattern)
		e.nodePos(n)
		e.str(n.Name)
		e.strSlice(n.Bindings)
	case *ast.NameMatchPattern:
		e.u8(tagNameMatchPattern)
		e.nodePos(n)
		e.str(n.Name)
	case *ast.LiteralMatchPattern:
		e.u8(tagLiteralMatchPattern)
		e.nodePos(n)
		e.expr(n.Value)
	case *ast.WildcardMatchPattern:
		e.u8(tagWildcardMatchPattern)
		e.nodePos(n)
	case *ast.ExpressionMatchPattern:
		e.u8(tagExpressionMatchPattern)
		e.nodePos(n)
		e.expr(n.Expr)
	}
}

func (e *encoder) isPattern(p ast.IsPattern) {
	if p == nil {
		e.u8(tagNil)
		return
	}
	switch n := p.(type) {
	case *ast.DestructureIsPattern:
		e.u8(tagDestructureIsPattern)
		e.nodePos(n)
		e.str(n.TypeName)
		e.typeRefs(n.TypeArgs)
		e.strSlice(n.Bindings)
	case *ast.IdentIsPattern:
		e.u8(tagIdentIsPattern)
		e.nodePos(n)
		e.str(n.Name)
		e.typeRefs(n.TypeArgs)
	}
}

// --- helpers ---

func (e *encoder) exprs(xs []ast.Expr) {
	e.u32(uint32(len(xs)))
	for _, x := range xs {
		e.expr(x)
	}
}

func (e *encoder) stmts(ss []ast.Stmt) {
	e.u32(uint32(len(ss)))
	for _, s := range ss {
		e.stmt(s)
	}
}

func (e *encoder) typeRefs(ts []ast.TypeRef) {
	e.u32(uint32(len(ts)))
	for _, t := range ts {
		e.typeRef(t)
	}
}

func (e *encoder) typeRefOpt(t ast.TypeRef) { e.typeRef(t) }

func (e *encoder) strSlice(ss []string) {
	e.u32(uint32(len(ss)))
	for _, s := range ss {
		e.str(s)
	}
}

func (e *encoder) block(b *ast.Block) {
	e.nodePos(b)
	e.stmts(b.Stmts)
}

func (e *encoder) blockOpt(b *ast.Block) {
	if b == nil {
		e.bool_(false)
		return
	}
	e.bool_(true)
	e.block(b)
}

func (e *encoder) annotations(as []*ast.MetaAnnotation) {
	e.u32(uint32(len(as)))
	for _, a := range as {
		e.nodePos(a)
		e.str(a.Name)
		e.u32(uint32(len(a.Params)))
		for _, p := range a.Params {
			e.nodePos(p)
			e.str(p.Name)
			e.expr(p.Value)
		}
	}
}

func (e *encoder) typeParams(tps []*ast.TypeParam) {
	e.u32(uint32(len(tps)))
	for _, tp := range tps {
		e.nodePos(tp)
		e.str(tp.Name)
		e.typeRefs(tp.Constraint)
	}
}

func (e *encoder) params(ps []*ast.Param) {
	e.u32(uint32(len(ps)))
	for _, p := range ps {
		e.nodePos(p)
		e.typeRef(p.Type)
		e.u8(byte(p.RefMod))
		e.str(p.Name)
		e.annotations(p.Annotations)
		e.expr(p.Default)
		e.bool_(p.IsVariadic)
	}
}

func (e *encoder) returnTypeSpec(r *ast.ReturnTypeSpec) {
	if r == nil {
		e.bool_(false)
		return
	}
	e.bool_(true)
	e.nodePos(r)
	e.typeRef(r.Type)
	e.bool_(r.CanError)
}

func (e *encoder) receiverParam(r *ast.ReceiverParam) {
	if r == nil {
		e.bool_(false)
		return
	}
	e.bool_(true)
	e.nodePos(r)
	e.u8(byte(r.RefMod))
}

func (e *encoder) methodDecl(m *ast.MethodDecl) {
	e.nodePos(m)
	e.str(m.Name)
	e.typeParams(m.TypeParams)
	e.receiverParam(m.Receiver)
	e.params(m.Params)
	e.returnTypeSpec(m.ReturnType)
	e.annotations(m.Annotations)
	e.blockOpt(m.Body)
	e.bool_(m.IsGetter)
	e.bool_(m.IsSetter)
}

func (e *encoder) decl(d ast.Decl) {
	switch n := d.(type) {
	case *ast.TypeDecl:
		e.u8(tagTypeDecl)
		e.nodePos(n)
		e.str(n.Name)
		e.typeParams(n.TypeParams)
		e.typeRefs(n.Inherits)
		e.annotations(n.Annotations)
		e.u32(uint32(len(n.Fields)))
		for _, f := range n.Fields {
			e.nodePos(f)
			e.typeRef(f.Type)
			e.str(f.Name)
			e.annotations(f.Annotations)
			e.expr(f.Default)
		}
		e.u32(uint32(len(n.Methods)))
		for _, m := range n.Methods {
			e.methodDecl(m)
		}
	case *ast.EnumDecl:
		e.u8(tagEnumDecl)
		e.nodePos(n)
		e.str(n.Name)
		e.typeParams(n.TypeParams)
		e.annotations(n.Annotations)
		e.u32(uint32(len(n.Variants)))
		for _, v := range n.Variants {
			e.nodePos(v)
			e.str(v.Name)
			e.u32(uint32(len(v.Fields)))
			for _, f := range v.Fields {
				e.nodePos(f)
				e.typeRef(f.Type)
				e.str(f.Name)
			}
			e.annotations(v.Annotations)
		}
		e.u32(uint32(len(n.Methods)))
		for _, m := range n.Methods {
			e.methodDecl(m)
		}
	case *ast.FuncDecl:
		e.u8(tagFuncDecl)
		e.nodePos(n)
		e.str(n.Name)
		e.typeParams(n.TypeParams)
		e.params(n.Params)
		e.returnTypeSpec(n.ReturnType)
		e.annotations(n.Annotations)
		e.blockOpt(n.Body)
		e.bool_(n.IsGetter)
		e.bool_(n.IsSetter)
	}
}

func (e *encoder) file(f *ast.File) {
	e.nodePos(f)
	e.u32(uint32(len(f.Uses)))
	for _, u := range f.Uses {
		e.nodePos(u)
		e.str(u.Alias)
		e.str(u.Path)
		e.str(u.CatalogName)
	}
	e.u32(uint32(len(f.Decls)))
	for _, d := range f.Decls {
		e.decl(d)
	}
}

// Encode serializes an AST File to binary. Returns the raw bytes (no header).
func Encode(f *ast.File) []byte {
	e := newEncoder()
	e.file(f)

	// Build final output: string table + AST data
	var out []byte
	tmp := make([]byte, 4)

	// String count
	binary.LittleEndian.PutUint32(tmp, uint32(len(e.strs)))
	out = append(out, tmp...)

	// Strings: length-prefixed
	for _, s := range e.strs {
		binary.LittleEndian.PutUint32(tmp, uint32(len(s)))
		out = append(out, tmp...)
		out = append(out, s...)
	}

	// AST data
	out = append(out, e.buf...)
	return out
}
