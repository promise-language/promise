package astcache

import (
	"encoding/binary"
	"fmt"

	"github.com/promise-language/promise/compiler/internal/ast"
)

// decoder deserializes an AST File from binary format.
type decoder struct {
	data []byte
	off  int
	strs []string
	err  error
}

func (d *decoder) u8() byte {
	if d.err != nil || d.off >= len(d.data) {
		d.err = fmt.Errorf("unexpected end of data at offset %d", d.off)
		return 0
	}
	v := d.data[d.off]
	d.off++
	return v
}

func (d *decoder) bool_() bool { return d.u8() != 0 }

func (d *decoder) u16() uint16 {
	if d.err != nil || d.off+2 > len(d.data) {
		d.err = fmt.Errorf("unexpected end of data at offset %d", d.off)
		return 0
	}
	v := binary.LittleEndian.Uint16(d.data[d.off:])
	d.off += 2
	return v
}

func (d *decoder) u32() uint32 {
	if d.err != nil || d.off+4 > len(d.data) {
		d.err = fmt.Errorf("unexpected end of data at offset %d", d.off)
		return 0
	}
	v := binary.LittleEndian.Uint32(d.data[d.off:])
	d.off += 4
	return v
}

func (d *decoder) str() string {
	idx := d.u32()
	if d.err != nil {
		return ""
	}
	if int(idx) >= len(d.strs) {
		d.err = fmt.Errorf("string index %d out of range (have %d)", idx, len(d.strs))
		return ""
	}
	return d.strs[idx]
}

func (d *decoder) pos() ast.Pos {
	return ast.Pos{
		File:   d.str(),
		Line:   int(d.u32()),
		Column: int(d.u16()),
	}
}

func (d *decoder) setPosEnd(n ast.Node) {
	p := d.pos()
	e := d.pos()
	// Use the exported SetPosEnd method via the embedded nodeBase
	type posEndSetter interface{ SetPosEnd(ast.Pos, ast.Pos) }
	if s, ok := n.(posEndSetter); ok {
		s.SetPosEnd(p, e)
	}
}

// --- interface dispatch ---

func (d *decoder) expr() ast.Expr {
	if d.err != nil {
		return nil
	}
	tag := d.u8()
	if tag == tagNil {
		return nil
	}
	switch tag {
	case tagBinaryExpr:
		n := &ast.BinaryExpr{}
		d.setPosEnd(n)
		n.Left = d.expr()
		n.Op = ast.BinaryOp(d.u8())
		n.Right = d.expr()
		return n
	case tagUnaryExpr:
		n := &ast.UnaryExpr{}
		d.setPosEnd(n)
		n.Op = ast.UnaryOp(d.u8())
		n.Operand = d.expr()
		return n
	case tagCallExpr:
		n := &ast.CallExpr{}
		d.setPosEnd(n)
		n.Callee = d.expr()
		cnt := d.u32()
		n.Args = make([]*ast.Arg, cnt)
		for i := range n.Args {
			a := &ast.Arg{}
			d.setPosEnd(a)
			a.Name = d.str()
			a.Move = d.bool_()
			a.Value = d.expr()
			n.Args[i] = a
		}
		return n
	case tagIndexExpr:
		n := &ast.IndexExpr{}
		d.setPosEnd(n)
		n.Target = d.expr()
		n.Index = d.expr()
		cnt := d.u32()
		if cnt > 0 {
			n.ExtraIndices = make([]ast.Expr, cnt)
			for i := range n.ExtraIndices {
				n.ExtraIndices[i] = d.expr()
			}
		}
		return n
	case tagSliceExpr:
		n := &ast.SliceExpr{}
		d.setPosEnd(n)
		n.Target = d.expr()
		n.Low = d.expr()
		n.High = d.expr()
		return n
	case tagSliceTypeExpr:
		n := &ast.SliceTypeExpr{}
		d.setPosEnd(n)
		n.Inner = d.expr()
		return n
	case tagMemberExpr:
		n := &ast.MemberExpr{}
		d.setPosEnd(n)
		n.Target = d.expr()
		n.Field = d.str()
		return n
	case tagOptionalChainExpr:
		n := &ast.OptionalChainExpr{}
		d.setPosEnd(n)
		n.Target = d.expr()
		n.Field = d.str()
		return n
	case tagIsExpr:
		n := &ast.IsExpr{}
		d.setPosEnd(n)
		n.Expr = d.expr()
		n.Pattern = d.isPattern()
		return n
	case tagCastExpr:
		n := &ast.CastExpr{}
		d.setPosEnd(n)
		n.Expr = d.expr()
		n.Type = d.typeRef()
		n.Force = d.bool_()
		return n
	case tagErrorPropagateExpr:
		n := &ast.ErrorPropagateExpr{}
		d.setPosEnd(n)
		n.Expr = d.expr()
		return n
	case tagErrorPanicExpr:
		n := &ast.ErrorPanicExpr{}
		d.setPosEnd(n)
		n.Expr = d.expr()
		return n
	case tagOptionalUnwrapExpr:
		n := &ast.OptionalUnwrapExpr{}
		d.setPosEnd(n)
		n.Expr = d.expr()
		return n
	case tagAutoCloneExpr: // T0605: synth-only; never serialized (defensive)
		n := &ast.AutoCloneExpr{}
		d.setPosEnd(n)
		n.Expr = d.expr()
		return n
	case tagErrorHandlerExpr:
		n := &ast.ErrorHandlerExpr{}
		d.setPosEnd(n)
		n.Expr = d.expr()
		n.Binding = d.str()
		n.TypeName = d.str()
		n.TypeArgs = d.typeRefs()
		n.Body = d.block()
		n.ElseBinding = d.str()
		n.ElseBody = d.blockOpt()
		n.PanicOnNomatch = d.bool_()
		return n
	case tagIfExpr:
		n := &ast.IfExpr{}
		d.setPosEnd(n)
		n.Cond = d.expr()
		n.Then = d.block()
		n.Else = d.block()
		return n
	case tagMatchExpr:
		n := &ast.MatchExpr{}
		d.setPosEnd(n)
		n.Subject = d.expr()
		cnt := d.u32()
		n.Arms = make([]*ast.MatchArm, cnt)
		for i := range n.Arms {
			a := &ast.MatchArm{}
			d.setPosEnd(a)
			a.Pattern = d.matchPattern()
			a.Guard = d.expr()
			a.Body = d.expr()
			a.Block = d.blockOpt()
			n.Arms[i] = a
		}
		return n
	case tagGoExpr:
		n := &ast.GoExpr{}
		d.setPosEnd(n)
		n.Expr = d.expr()
		n.Block = d.blockOpt()
		return n
	case tagUnsafeExpr:
		n := &ast.UnsafeExpr{}
		d.setPosEnd(n)
		n.Body = d.block()
		return n
	case tagLambdaExpr:
		n := &ast.LambdaExpr{}
		d.setPosEnd(n)
		n.Move = d.bool_()
		cnt := d.u32()
		n.Params = make([]*ast.LambdaParam, cnt)
		for i := range n.Params {
			lp := &ast.LambdaParam{}
			d.setPosEnd(lp)
			lp.Type = d.typeRef()
			lp.RefMod = ast.RefModifier(d.u8())
			lp.Name = d.str()
			n.Params[i] = lp
		}
		n.ReturnType = d.typeRef()
		n.Body = d.blockOpt()
		n.ExprBody = d.expr()
		return n
	case tagIntLit:
		n := &ast.IntLit{}
		d.setPosEnd(n)
		n.Raw = d.str()
		n.Suffix = d.str()
		return n
	case tagFloatLit:
		n := &ast.FloatLit{}
		d.setPosEnd(n)
		n.Raw = d.str()
		n.Suffix = d.str()
		return n
	case tagBoolLit:
		n := &ast.BoolLit{}
		d.setPosEnd(n)
		n.Value = d.bool_()
		return n
	case tagNoneLit:
		n := &ast.NoneLit{}
		d.setPosEnd(n)
		return n
	case tagCharLit:
		n := &ast.CharLit{}
		d.setPosEnd(n)
		n.Raw = d.str()
		return n
	case tagStringLit:
		n := &ast.StringLit{}
		d.setPosEnd(n)
		n.Raw = d.str()
		n.Kind = ast.StringKind(d.u8())
		cnt := d.u32()
		if cnt > 0 {
			n.Parts = make([]ast.StringPart, cnt)
			for i := range n.Parts {
				ptag := d.u8()
				switch ptag {
				case tagStringText:
					n.Parts[i] = ast.StringText{Text: d.str()}
				case tagStringEscape:
					n.Parts[i] = ast.StringEscape{Sequence: d.str()}
				case tagStringInterp:
					raw := d.str()
					ex := d.expr()
					n.Parts[i] = ast.StringInterp{Raw: raw, Expr: ex}
				}
			}
		}
		return n
	case tagIdentExpr:
		n := &ast.IdentExpr{}
		d.setPosEnd(n)
		n.Name = d.str()
		return n
	case tagThisExpr:
		n := &ast.ThisExpr{}
		d.setPosEnd(n)
		return n
	case tagParenExpr:
		n := &ast.ParenExpr{}
		d.setPosEnd(n)
		n.Expr = d.expr()
		return n
	case tagTupleLit:
		n := &ast.TupleLit{}
		d.setPosEnd(n)
		n.Elements = d.exprs()
		return n
	case tagArrayLit:
		n := &ast.ArrayLit{}
		d.setPosEnd(n)
		n.Elements = d.exprs()
		return n
	case tagMapLit:
		n := &ast.MapLit{}
		d.setPosEnd(n)
		cnt := d.u32()
		n.Entries = make([]*ast.MapEntry, cnt)
		for i := range n.Entries {
			en := &ast.MapEntry{}
			d.setPosEnd(en)
			en.Key = d.expr()
			en.Value = d.expr()
			n.Entries[i] = en
		}
		return n
	case tagEmptyBraceLit:
		n := &ast.EmptyBraceLit{}
		d.setPosEnd(n)
		return n
	case tagTypeRefExpr:
		n := &ast.TypeRefExpr{}
		d.setPosEnd(n)
		n.Ref = d.typeRef()
		return n
	default:
		d.err = fmt.Errorf("unknown expr tag %d at offset %d", tag, d.off-1)
		return nil
	}
}

func (d *decoder) stmt() ast.Stmt {
	if d.err != nil {
		return nil
	}
	tag := d.u8()
	if tag == tagNil {
		return nil
	}
	switch tag {
	case tagBlock:
		n := &ast.Block{}
		d.setPosEnd(n)
		n.Stmts = d.stmts()
		return n
	case tagTypedVarDecl:
		n := &ast.TypedVarDecl{}
		d.setPosEnd(n)
		n.Type = d.typeRef()
		n.RefMod = ast.RefModifier(d.u8())
		n.Name = d.str()
		n.Value = d.expr()
		return n
	case tagInferredVarDecl:
		n := &ast.InferredVarDecl{}
		d.setPosEnd(n)
		n.Name = d.str()
		n.Value = d.expr()
		return n
	case tagDestructureVarDecl:
		n := &ast.DestructureVarDecl{}
		d.setPosEnd(n)
		n.Names = d.strSlice()
		n.Value = d.expr()
		return n
	case tagUseVarDecl:
		n := &ast.UseVarDecl{}
		d.setPosEnd(n)
		n.Name = d.str()
		n.Value = d.expr()
		return n
	case tagAssignStmt:
		n := &ast.AssignStmt{}
		d.setPosEnd(n)
		n.Target = d.expr()
		n.Op = ast.AssignOp(d.u8())
		n.Value = d.expr()
		return n
	case tagReturnStmt:
		n := &ast.ReturnStmt{}
		d.setPosEnd(n)
		n.Value = d.expr()
		return n
	case tagRaiseStmt:
		n := &ast.RaiseStmt{}
		d.setPosEnd(n)
		n.Value = d.expr()
		return n
	case tagYieldStmt:
		n := &ast.YieldStmt{}
		d.setPosEnd(n)
		n.Value = d.expr()
		return n
	case tagYieldDelegateStmt:
		n := &ast.YieldDelegateStmt{}
		d.setPosEnd(n)
		n.Value = d.expr()
		return n
	case tagBreakStmt:
		n := &ast.BreakStmt{}
		d.setPosEnd(n)
		return n
	case tagContinueStmt:
		n := &ast.ContinueStmt{}
		d.setPosEnd(n)
		return n
	case tagExprStmt:
		n := &ast.ExprStmt{}
		d.setPosEnd(n)
		n.Expr = d.expr()
		return n
	case tagIfStmt:
		n := &ast.IfStmt{}
		d.setPosEnd(n)
		n.Cond = d.expr()
		n.Binding = d.str()
		n.Init = d.expr()
		n.Body = d.block()
		n.Else = d.stmt()
		return n
	case tagForInStmt:
		n := &ast.ForInStmt{}
		d.setPosEnd(n)
		n.Binding = d.str()
		n.Index = d.str()
		n.Iterable = d.expr()
		n.Body = d.block()
		return n
	case tagIncDecStmt:
		n := &ast.IncDecStmt{}
		d.setPosEnd(n)
		n.Target = d.expr()
		n.IsInc = d.bool_()
		return n
	case tagClassicForStmt:
		n := &ast.ClassicForStmt{}
		d.setPosEnd(n)
		n.InitName = d.str()
		n.InitType = d.typeRef()
		n.InitValue = d.expr()
		n.Cond = d.expr()
		n.UpdateTarget = d.expr()
		n.UpdateOp = ast.AssignOp(d.u8())
		n.UpdateValue = d.expr()
		n.UpdateIncDec = d.bool_()
		n.UpdateIsInc = d.bool_()
		n.Body = d.block()
		return n
	case tagInfiniteLoop:
		n := &ast.InfiniteLoop{}
		d.setPosEnd(n)
		n.Body = d.block()
		return n
	case tagWhileStmt:
		n := &ast.WhileStmt{}
		d.setPosEnd(n)
		n.Cond = d.expr()
		n.Body = d.block()
		return n
	case tagWhileUnwrapStmt:
		n := &ast.WhileUnwrapStmt{}
		d.setPosEnd(n)
		n.Binding = d.str()
		n.Value = d.expr()
		n.Body = d.block()
		return n
	case tagSelectStmt:
		n := &ast.SelectStmt{}
		d.setPosEnd(n)
		cnt := d.u32()
		n.Cases = make([]*ast.SelectCase, cnt)
		for i := range n.Cases {
			c := &ast.SelectCase{}
			d.setPosEnd(c)
			c.IsSend = d.bool_()
			c.Channel = d.expr()
			c.SendValue = d.expr()
			c.Binding = d.str()
			c.Body = d.stmts()
			n.Cases[i] = c
		}
		if d.u8() == 1 {
			n.Default = d.stmts()
			if n.Default == nil {
				n.Default = []ast.Stmt{}
			}
		}
		return n
	default:
		d.err = fmt.Errorf("unknown stmt tag %d at offset %d", tag, d.off-1)
		return nil
	}
}

func (d *decoder) typeRef() ast.TypeRef {
	if d.err != nil {
		return nil
	}
	tag := d.u8()
	if tag == tagNil {
		return nil
	}
	switch tag {
	case tagNamedTypeRef:
		n := &ast.NamedTypeRef{}
		d.setPosEnd(n)
		n.Name = d.str()
		n.TypeArgs = d.typeRefs()
		return n
	case tagQualifiedTypeRef:
		n := &ast.QualifiedTypeRef{}
		d.setPosEnd(n)
		n.Module = d.str()
		n.Name = d.str()
		n.TypeArgs = d.typeRefs()
		return n
	case tagTupleTypeRef:
		n := &ast.TupleTypeRef{}
		d.setPosEnd(n)
		n.Elements = d.typeRefs()
		return n
	case tagFunctionTypeRef:
		n := &ast.FunctionTypeRef{}
		d.setPosEnd(n)
		n.Params = d.typeRefs()
		n.Return = d.typeRef()
		return n
	case tagSharedRefTypeRef:
		n := &ast.SharedRefTypeRef{}
		d.setPosEnd(n)
		n.Inner = d.typeRef()
		return n
	case tagMutRefTypeRef:
		n := &ast.MutRefTypeRef{}
		d.setPosEnd(n)
		n.Inner = d.typeRef()
		return n
	case tagPointerTypeRef:
		n := &ast.PointerTypeRef{}
		d.setPosEnd(n)
		n.Inner = d.typeRef()
		return n
	case tagOptionalTypeRef:
		n := &ast.OptionalTypeRef{}
		d.setPosEnd(n)
		n.Inner = d.typeRef()
		return n
	case tagSliceTypeRef:
		n := &ast.SliceTypeRef{}
		d.setPosEnd(n)
		n.Element = d.typeRef()
		return n
	case tagArrayTypeRef:
		n := &ast.ArrayTypeRef{}
		d.setPosEnd(n)
		n.Element = d.typeRef()
		n.Size = d.str()
		return n
	default:
		d.err = fmt.Errorf("unknown typeref tag %d at offset %d", tag, d.off-1)
		return nil
	}
}

func (d *decoder) matchPattern() ast.MatchPattern {
	if d.err != nil {
		return nil
	}
	tag := d.u8()
	if tag == tagNil {
		return nil
	}
	switch tag {
	case tagEnumDestructureMatchPattern:
		n := &ast.EnumDestructureMatchPattern{}
		d.setPosEnd(n)
		n.Module = d.str()
		n.Enum = d.str()
		n.Variant = d.str()
		n.Bindings = d.strSlice()
		return n
	case tagEnumVariantMatchPattern:
		n := &ast.EnumVariantMatchPattern{}
		d.setPosEnd(n)
		n.Module = d.str()
		n.Enum = d.str()
		n.Variant = d.str()
		return n
	case tagTypeBindingMatchPattern:
		n := &ast.TypeBindingMatchPattern{}
		d.setPosEnd(n)
		n.TypeName = d.str()
		n.Binding = d.str()
		return n
	case tagShortDestructureMatchPattern:
		n := &ast.ShortDestructureMatchPattern{}
		d.setPosEnd(n)
		n.Name = d.str()
		n.Bindings = d.strSlice()
		return n
	case tagNameMatchPattern:
		n := &ast.NameMatchPattern{}
		d.setPosEnd(n)
		n.Name = d.str()
		return n
	case tagLiteralMatchPattern:
		n := &ast.LiteralMatchPattern{}
		d.setPosEnd(n)
		n.Value = d.expr()
		return n
	case tagWildcardMatchPattern:
		n := &ast.WildcardMatchPattern{}
		d.setPosEnd(n)
		return n
	case tagExpressionMatchPattern:
		n := &ast.ExpressionMatchPattern{}
		d.setPosEnd(n)
		n.Expr = d.expr()
		return n
	default:
		d.err = fmt.Errorf("unknown match pattern tag %d at offset %d", tag, d.off-1)
		return nil
	}
}

func (d *decoder) isPattern() ast.IsPattern {
	if d.err != nil {
		return nil
	}
	tag := d.u8()
	if tag == tagNil {
		return nil
	}
	switch tag {
	case tagDestructureIsPattern:
		n := &ast.DestructureIsPattern{}
		d.setPosEnd(n)
		n.TypeName = d.str()
		n.TypeArgs = d.typeRefs()
		n.Bindings = d.strSlice()
		return n
	case tagIdentIsPattern:
		n := &ast.IdentIsPattern{}
		d.setPosEnd(n)
		n.Name = d.str()
		n.TypeArgs = d.typeRefs()
		return n
	default:
		d.err = fmt.Errorf("unknown is-pattern tag %d at offset %d", tag, d.off-1)
		return nil
	}
}

// --- helpers ---

func (d *decoder) exprs() []ast.Expr {
	cnt := d.u32()
	if cnt == 0 || d.err != nil {
		return nil
	}
	xs := make([]ast.Expr, cnt)
	for i := range xs {
		xs[i] = d.expr()
	}
	return xs
}

func (d *decoder) stmts() []ast.Stmt {
	cnt := d.u32()
	if cnt == 0 || d.err != nil {
		return nil
	}
	ss := make([]ast.Stmt, cnt)
	for i := range ss {
		ss[i] = d.stmt()
	}
	return ss
}

func (d *decoder) typeRefs() []ast.TypeRef {
	cnt := d.u32()
	if cnt == 0 || d.err != nil {
		return nil
	}
	ts := make([]ast.TypeRef, cnt)
	for i := range ts {
		ts[i] = d.typeRef()
	}
	return ts
}

func (d *decoder) strSlice() []string {
	cnt := d.u32()
	if cnt == 0 || d.err != nil {
		return nil
	}
	ss := make([]string, cnt)
	for i := range ss {
		ss[i] = d.str()
	}
	return ss
}

func (d *decoder) block() *ast.Block {
	n := &ast.Block{}
	d.setPosEnd(n)
	n.Stmts = d.stmts()
	return n
}

func (d *decoder) blockOpt() *ast.Block {
	if !d.bool_() {
		return nil
	}
	return d.block()
}

func (d *decoder) annotations() []*ast.MetaAnnotation {
	cnt := d.u32()
	if cnt == 0 || d.err != nil {
		return nil
	}
	as := make([]*ast.MetaAnnotation, cnt)
	for i := range as {
		a := &ast.MetaAnnotation{}
		d.setPosEnd(a)
		a.Name = d.str()
		pcnt := d.u32()
		if pcnt > 0 {
			a.Params = make([]*ast.MetaParam, pcnt)
			for j := range a.Params {
				p := &ast.MetaParam{}
				d.setPosEnd(p)
				p.Name = d.str()
				p.Value = d.expr()
				a.Params[j] = p
			}
		}
		as[i] = a
	}
	return as
}

func (d *decoder) typeParams() []*ast.TypeParam {
	cnt := d.u32()
	if cnt == 0 || d.err != nil {
		return nil
	}
	tps := make([]*ast.TypeParam, cnt)
	for i := range tps {
		tp := &ast.TypeParam{}
		d.setPosEnd(tp)
		tp.Name = d.str()
		tp.Constraint = d.typeRefs()
		tps[i] = tp
	}
	return tps
}

func (d *decoder) params() []*ast.Param {
	cnt := d.u32()
	if cnt == 0 || d.err != nil {
		return nil
	}
	ps := make([]*ast.Param, cnt)
	for i := range ps {
		p := &ast.Param{}
		d.setPosEnd(p)
		p.Type = d.typeRef()
		p.RefMod = ast.RefModifier(d.u8())
		p.Name = d.str()
		p.Annotations = d.annotations()
		p.Default = d.expr()
		p.IsVariadic = d.bool_()
		ps[i] = p
	}
	return ps
}

func (d *decoder) returnTypeSpec() *ast.ReturnTypeSpec {
	if !d.bool_() {
		return nil
	}
	r := &ast.ReturnTypeSpec{}
	d.setPosEnd(r)
	r.Type = d.typeRef()
	r.CanError = d.bool_()
	return r
}

func (d *decoder) receiverParam() *ast.ReceiverParam {
	if !d.bool_() {
		return nil
	}
	r := &ast.ReceiverParam{}
	d.setPosEnd(r)
	r.RefMod = ast.RefModifier(d.u8())
	return r
}

func (d *decoder) methodDecl() *ast.MethodDecl {
	m := &ast.MethodDecl{}
	d.setPosEnd(m)
	m.Name = d.str()
	m.TypeParams = d.typeParams()
	m.Receiver = d.receiverParam()
	m.Params = d.params()
	m.ReturnType = d.returnTypeSpec()
	m.Annotations = d.annotations()
	m.Body = d.blockOpt()
	m.IsGetter = d.bool_()
	m.IsSetter = d.bool_()
	return m
}

func (d *decoder) decl() ast.Decl {
	if d.err != nil {
		return nil
	}
	tag := d.u8()
	switch tag {
	case tagTypeDecl:
		n := &ast.TypeDecl{}
		d.setPosEnd(n)
		n.Name = d.str()
		n.TypeParams = d.typeParams()
		n.Inherits = d.typeRefs()
		n.Annotations = d.annotations()
		fcnt := d.u32()
		if fcnt > 0 {
			n.Fields = make([]*ast.FieldDecl, fcnt)
			for i := range n.Fields {
				f := &ast.FieldDecl{}
				d.setPosEnd(f)
				f.Type = d.typeRef()
				f.Name = d.str()
				f.Annotations = d.annotations()
				f.Default = d.expr()
				n.Fields[i] = f
			}
		}
		mcnt := d.u32()
		if mcnt > 0 {
			n.Methods = make([]*ast.MethodDecl, mcnt)
			for i := range n.Methods {
				n.Methods[i] = d.methodDecl()
			}
		}
		return n
	case tagEnumDecl:
		n := &ast.EnumDecl{}
		d.setPosEnd(n)
		n.Name = d.str()
		n.TypeParams = d.typeParams()
		n.Annotations = d.annotations()
		vcnt := d.u32()
		if vcnt > 0 {
			n.Variants = make([]*ast.EnumVariant, vcnt)
			for i := range n.Variants {
				v := &ast.EnumVariant{}
				d.setPosEnd(v)
				v.Name = d.str()
				fcnt := d.u32()
				if fcnt > 0 {
					v.Fields = make([]*ast.EnumField, fcnt)
					for j := range v.Fields {
						f := &ast.EnumField{}
						d.setPosEnd(f)
						f.Type = d.typeRef()
						f.Name = d.str()
						v.Fields[j] = f
					}
				}
				v.Annotations = d.annotations()
				n.Variants[i] = v
			}
		}
		mcnt := d.u32()
		if mcnt > 0 {
			n.Methods = make([]*ast.MethodDecl, mcnt)
			for i := range n.Methods {
				n.Methods[i] = d.methodDecl()
			}
		}
		return n
	case tagFuncDecl:
		n := &ast.FuncDecl{}
		d.setPosEnd(n)
		n.Name = d.str()
		n.TypeParams = d.typeParams()
		n.Params = d.params()
		n.ReturnType = d.returnTypeSpec()
		n.Annotations = d.annotations()
		n.Body = d.blockOpt()
		n.IsGetter = d.bool_()
		n.IsSetter = d.bool_()
		return n
	default:
		d.err = fmt.Errorf("unknown decl tag %d at offset %d", tag, d.off-1)
		return nil
	}
}

func (d *decoder) file() *ast.File {
	f := &ast.File{}
	d.setPosEnd(f)
	ucnt := d.u32()
	if ucnt > 0 {
		f.Uses = make([]*ast.UseDecl, ucnt)
		for i := range f.Uses {
			u := &ast.UseDecl{}
			d.setPosEnd(u)
			u.Alias = d.str()
			u.Path = d.str()
			u.CatalogName = d.str()
			f.Uses[i] = u
		}
	}
	dcnt := d.u32()
	if dcnt > 0 {
		f.Decls = make([]ast.Decl, dcnt)
		for i := range f.Decls {
			f.Decls[i] = d.decl()
		}
	}
	return f
}

// Decode deserializes an AST File from binary data (no header).
func Decode(data []byte) (result *ast.File, err error) {
	defer func() {
		if r := recover(); r != nil {
			result = nil
			err = fmt.Errorf("astcache decode panic: %v", r)
		}
	}()

	d := &decoder{data: data}

	// Read string table
	cnt := d.u32()
	if d.err != nil {
		return nil, d.err
	}
	d.strs = make([]string, cnt)
	for i := uint32(0); i < cnt; i++ {
		slen := d.u32()
		if d.err != nil {
			return nil, d.err
		}
		if d.off+int(slen) > len(d.data) {
			return nil, fmt.Errorf("string data exceeds buffer at offset %d", d.off)
		}
		d.strs[i] = string(d.data[d.off : d.off+int(slen)])
		d.off += int(slen)
	}

	f := d.file()
	if d.err != nil {
		return nil, d.err
	}
	return f, nil
}
