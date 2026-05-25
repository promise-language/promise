package sema

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"hash/fnv"

	"djabi.dev/go/promise_lang/internal/ast"
)

// HashTypeDecl returns a deterministic hash of a TypeDecl's content.
// The hash covers all fields, methods (signatures and bodies), annotations,
// and type parameters. Source positions and comments are excluded.
// Changes to unrelated declarations in the same file do NOT affect this hash.
func HashTypeDecl(td *ast.TypeDecl) string {
	h := fnv.New128a()
	hTypeDecl(h, td)
	return hex.EncodeToString(h.Sum(nil))
}

// HashEnumDecl returns a deterministic hash of an EnumDecl's content.
func HashEnumDecl(td *ast.EnumDecl) string {
	h := fnv.New128a()
	hEnumDecl(h, td)
	return hex.EncodeToString(h.Sum(nil))
}

// ws writes a length-prefixed string so that adjacent strings don't merge.
func ws(h hash.Hash, s string) {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(len(s)))
	h.Write(buf[:])
	h.Write([]byte(s))
}

// wb writes a single discriminant byte.
func wb(h hash.Hash, b byte) {
	h.Write([]byte{b})
}

// wbool writes a boolean discriminant.
func wbool(h hash.Hash, v bool) {
	if v {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
}

func hTypeDecl(h hash.Hash, td *ast.TypeDecl) {
	wb(h, 'T')
	ws(h, td.Name)
	for _, tp := range td.TypeParams {
		hTypeParam(h, tp)
	}
	wb(h, 0xFF)
	for _, inh := range td.Inherits {
		hTypeRef(h, inh)
	}
	wb(h, 0xFF)
	for _, ann := range td.Annotations {
		hAnnotation(h, ann)
	}
	wb(h, 0xFF)
	for _, f := range td.Fields {
		hFieldDecl(h, f)
	}
	wb(h, 0xFF)
	for _, m := range td.Methods {
		hMethodDecl(h, m)
	}
}

func hEnumDecl(h hash.Hash, td *ast.EnumDecl) {
	wb(h, 'E')
	ws(h, td.Name)
	for _, tp := range td.TypeParams {
		hTypeParam(h, tp)
	}
	wb(h, 0xFF)
	for _, ann := range td.Annotations {
		hAnnotation(h, ann)
	}
	wb(h, 0xFF)
	for _, v := range td.Variants {
		hEnumVariant(h, v)
	}
	wb(h, 0xFF)
	for _, m := range td.Methods {
		hMethodDecl(h, m)
	}
}

func hTypeParam(h hash.Hash, tp *ast.TypeParam) {
	ws(h, tp.Name)
	for _, c := range tp.Constraint {
		hTypeRef(h, c)
	}
	wb(h, 0xFE)
}

func hFieldDecl(h hash.Hash, f *ast.FieldDecl) {
	ws(h, f.Name)
	hTypeRef(h, f.Type)
	for _, ann := range f.Annotations {
		hAnnotation(h, ann)
	}
	if f.Default != nil {
		wb(h, 1)
		hExpr(h, f.Default)
	} else {
		wb(h, 0)
	}
	wb(h, 0xFE)
}

func hMethodDecl(h hash.Hash, m *ast.MethodDecl) {
	ws(h, m.Name)
	for _, tp := range m.TypeParams {
		hTypeParam(h, tp)
	}
	wb(h, 0xFF)
	if m.Receiver != nil {
		wb(h, 1)
		hRefMod(h, m.Receiver.RefMod)
	} else {
		wb(h, 0)
	}
	for _, p := range m.Params {
		hParam(h, p)
	}
	wb(h, 0xFF)
	if m.ReturnType != nil {
		wb(h, 1)
		hReturnTypeSpec(h, m.ReturnType)
	} else {
		wb(h, 0)
	}
	for _, ann := range m.Annotations {
		hAnnotation(h, ann)
	}
	wbool(h, m.IsGetter)
	wbool(h, m.IsSetter)
	if m.Body != nil {
		wb(h, 1)
		hBlock(h, m.Body)
	} else {
		wb(h, 0)
	}
	wb(h, 0xFE)
}

func hParam(h hash.Hash, p *ast.Param) {
	ws(h, p.Name)
	hTypeRef(h, p.Type)
	hRefMod(h, p.RefMod)
	wbool(h, p.IsVariadic)
	if p.Default != nil {
		wb(h, 1)
		hExpr(h, p.Default)
	} else {
		wb(h, 0)
	}
	for _, ann := range p.Annotations {
		hAnnotation(h, ann)
	}
	wb(h, 0xFE)
}

func hReturnTypeSpec(h hash.Hash, r *ast.ReturnTypeSpec) {
	hTypeRef(h, r.Type)
	wbool(h, r.CanError)
}

func hRefMod(h hash.Hash, r ast.RefModifier) {
	wb(h, byte(r))
}

func hAnnotation(h hash.Hash, ann *ast.MetaAnnotation) {
	ws(h, ann.Name)
	for _, p := range ann.Params {
		ws(h, p.Name)
		hExpr(h, p.Value)
	}
	wb(h, 0xFE)
}

func hEnumVariant(h hash.Hash, v *ast.EnumVariant) {
	ws(h, v.Name)
	for _, ann := range v.Annotations {
		hAnnotation(h, ann)
	}
	for _, f := range v.Fields {
		hEnumField(h, f)
	}
	wb(h, 0xFE)
}

func hEnumField(h hash.Hash, f *ast.EnumField) {
	ws(h, f.Name)
	hTypeRef(h, f.Type)
}

func hTypeRef(h hash.Hash, tr ast.TypeRef) {
	if tr == nil {
		wb(h, 0)
		return
	}
	switch t := tr.(type) {
	case *ast.NamedTypeRef:
		wb(h, 1)
		ws(h, t.Name)
		for _, a := range t.TypeArgs {
			hTypeRef(h, a)
		}
		wb(h, 0xFE)
	case *ast.QualifiedTypeRef:
		wb(h, 2)
		ws(h, t.Module)
		ws(h, t.Name)
		for _, a := range t.TypeArgs {
			hTypeRef(h, a)
		}
		wb(h, 0xFE)
	case *ast.TupleTypeRef:
		wb(h, 3)
		for _, e := range t.Elements {
			hTypeRef(h, e)
		}
		wb(h, 0xFE)
	case *ast.FunctionTypeRef:
		wb(h, 4)
		for _, p := range t.Params {
			hTypeRef(h, p)
		}
		wb(h, 0xFE)
		hTypeRef(h, t.Return)
	case *ast.SharedRefTypeRef:
		wb(h, 5)
		hTypeRef(h, t.Inner)
	case *ast.MutRefTypeRef:
		wb(h, 6)
		hTypeRef(h, t.Inner)
	case *ast.PointerTypeRef:
		wb(h, 7)
		hTypeRef(h, t.Inner)
	case *ast.OptionalTypeRef:
		wb(h, 8)
		hTypeRef(h, t.Inner)
	case *ast.SliceTypeRef:
		wb(h, 9)
		hTypeRef(h, t.Element)
	case *ast.ArrayTypeRef:
		wb(h, 10)
		hTypeRef(h, t.Element)
		ws(h, t.Size)
	default:
		panic(fmt.Sprintf("typehash: unhandled TypeRef %T", tr))
	}
}

func hBlock(h hash.Hash, b *ast.Block) {
	if b == nil {
		wb(h, 0)
		return
	}
	wb(h, 1)
	for _, s := range b.Stmts {
		hStmt(h, s)
	}
	wb(h, 0xFE)
}

func hStmt(h hash.Hash, s ast.Stmt) {
	if s == nil {
		wb(h, 0)
		return
	}
	switch st := s.(type) {
	case *ast.Block:
		wb(h, 1)
		hBlock(h, st)
	case *ast.TypedVarDecl:
		wb(h, 2)
		hTypeRef(h, st.Type)
		hRefMod(h, st.RefMod)
		ws(h, st.Name)
		hExpr(h, st.Value)
	case *ast.InferredVarDecl:
		wb(h, 3)
		ws(h, st.Name)
		hExpr(h, st.Value)
	case *ast.DestructureVarDecl:
		wb(h, 4)
		for _, name := range st.Names {
			ws(h, name)
		}
		wb(h, 0xFE)
		hExpr(h, st.Value)
	case *ast.UseVarDecl:
		wb(h, 5)
		ws(h, st.Name)
		hExpr(h, st.Value)
	case *ast.AssignStmt:
		wb(h, 6)
		hExpr(h, st.Target)
		wb(h, byte(st.Op))
		hExpr(h, st.Value)
	case *ast.ReturnStmt:
		wb(h, 7)
		if st.Value != nil {
			wb(h, 1)
			hExpr(h, st.Value)
		} else {
			wb(h, 0)
		}
	case *ast.RaiseStmt:
		wb(h, 8)
		hExpr(h, st.Value)
	case *ast.YieldStmt:
		wb(h, 9)
		hExpr(h, st.Value)
	case *ast.YieldDelegateStmt:
		wb(h, 10)
		hExpr(h, st.Value)
	case *ast.BreakStmt:
		wb(h, 11)
	case *ast.ContinueStmt:
		wb(h, 12)
	case *ast.ExprStmt:
		wb(h, 13)
		hExpr(h, st.Expr)
	case *ast.IfStmt:
		wb(h, 14)
		if st.Cond != nil {
			wb(h, 1)
			hExpr(h, st.Cond)
		} else {
			wb(h, 0)
			ws(h, st.Binding)
			hExpr(h, st.Init)
		}
		hBlock(h, st.Body)
		if st.Else != nil {
			wb(h, 1)
			hStmt(h, st.Else)
		} else {
			wb(h, 0)
		}
	case *ast.ForInStmt:
		wb(h, 15)
		ws(h, st.Binding)
		ws(h, st.Index)
		hExpr(h, st.Iterable)
		hBlock(h, st.Body)
	case *ast.IncDecStmt:
		wb(h, 16)
		hExpr(h, st.Target)
		wbool(h, st.IsInc)
	case *ast.ClassicForStmt:
		wb(h, 17)
		ws(h, st.InitName)
		hTypeRef(h, st.InitType)
		hExpr(h, st.InitValue)
		hExpr(h, st.Cond)
		if st.UpdateTarget != nil {
			wb(h, 1)
			hExpr(h, st.UpdateTarget)
			wb(h, byte(st.UpdateOp))
			if st.UpdateIncDec {
				wb(h, 1)
				wbool(h, st.UpdateIsInc)
			} else {
				wb(h, 0)
				hExpr(h, st.UpdateValue)
			}
		} else {
			wb(h, 0)
		}
		hBlock(h, st.Body)
	case *ast.InfiniteLoop:
		wb(h, 18)
		hBlock(h, st.Body)
	case *ast.WhileStmt:
		wb(h, 19)
		hExpr(h, st.Cond)
		hBlock(h, st.Body)
	case *ast.WhileUnwrapStmt:
		wb(h, 20)
		ws(h, st.Binding)
		hExpr(h, st.Value)
		hBlock(h, st.Body)
	case *ast.SelectStmt:
		wb(h, 21)
		for _, c := range st.Cases {
			hSelectCase(h, c)
		}
		wb(h, 0xFE)
		for _, ds := range st.Default {
			hStmt(h, ds)
		}
		wb(h, 0xFF)
	default:
		panic(fmt.Sprintf("typehash: unhandled Stmt %T", s))
	}
}

func hSelectCase(h hash.Hash, sc *ast.SelectCase) {
	wbool(h, sc.IsSend)
	hExpr(h, sc.Channel)
	if sc.IsSend {
		hExpr(h, sc.SendValue)
	} else {
		ws(h, sc.Binding)
	}
	for _, s := range sc.Body {
		hStmt(h, s)
	}
	wb(h, 0xFE)
}

func hExpr(h hash.Hash, e ast.Expr) {
	if e == nil {
		wb(h, 0)
		return
	}
	switch ex := e.(type) {
	case *ast.BinaryExpr:
		wb(h, 1)
		hExpr(h, ex.Left)
		wb(h, byte(ex.Op))
		hExpr(h, ex.Right)
	case *ast.UnaryExpr:
		wb(h, 2)
		wb(h, byte(ex.Op))
		hExpr(h, ex.Operand)
	case *ast.CallExpr:
		wb(h, 3)
		hExpr(h, ex.Callee)
		for _, a := range ex.Args {
			ws(h, a.Name)
			hExpr(h, a.Value)
		}
		wb(h, 0xFE)
	case *ast.IndexExpr:
		wb(h, 4)
		hExpr(h, ex.Target)
		hExpr(h, ex.Index)
		for _, idx := range ex.ExtraIndices {
			hExpr(h, idx)
		}
		wb(h, 0xFE)
	case *ast.SliceExpr:
		wb(h, 5)
		hExpr(h, ex.Target)
		if ex.Low != nil {
			wb(h, 1)
			hExpr(h, ex.Low)
		} else {
			wb(h, 0)
		}
		if ex.High != nil {
			wb(h, 1)
			hExpr(h, ex.High)
		} else {
			wb(h, 0)
		}
	case *ast.SliceTypeExpr:
		wb(h, 6)
		hExpr(h, ex.Inner)
	case *ast.MemberExpr:
		wb(h, 7)
		hExpr(h, ex.Target)
		ws(h, ex.Field)
	case *ast.OptionalChainExpr:
		wb(h, 8)
		hExpr(h, ex.Target)
		ws(h, ex.Field)
	case *ast.IsExpr:
		wb(h, 9)
		hExpr(h, ex.Expr)
		hIsPattern(h, ex.Pattern)
	case *ast.CastExpr:
		wb(h, 10)
		hExpr(h, ex.Expr)
		hTypeRef(h, ex.Type)
		wbool(h, ex.Force)
	case *ast.ErrorPropagateExpr:
		wb(h, 11)
		hExpr(h, ex.Expr)
	case *ast.ErrorPanicExpr:
		wb(h, 12)
		hExpr(h, ex.Expr)
	case *ast.OptionalUnwrapExpr:
		wb(h, 20)
		hExpr(h, ex.Expr)
	case *ast.AutoCloneExpr: // T0605: distinct tag (synth clone body is hashed)
		wb(h, 31)
		hExpr(h, ex.Expr)
	case *ast.ErrorHandlerExpr:
		wb(h, 13)
		hExpr(h, ex.Expr)
		ws(h, ex.Binding)
		ws(h, ex.TypeName)
		hBlock(h, ex.Body)
		ws(h, ex.ElseBinding)
		hBlock(h, ex.ElseBody)
		wbool(h, ex.PanicOnNomatch)
	case *ast.IfExpr:
		wb(h, 14)
		hExpr(h, ex.Cond)
		hBlock(h, ex.Then)
		hBlock(h, ex.Else)
	case *ast.MatchExpr:
		wb(h, 15)
		hExpr(h, ex.Subject)
		for _, arm := range ex.Arms {
			hMatchArm(h, arm)
		}
		wb(h, 0xFE)
	case *ast.GoExpr:
		wb(h, 16)
		if ex.Expr != nil {
			wb(h, 1)
			hExpr(h, ex.Expr)
		} else {
			wb(h, 0)
			hBlock(h, ex.Block)
		}
	case *ast.UnsafeExpr:
		wb(h, 17)
		hBlock(h, ex.Body)
	case *ast.LambdaExpr:
		wb(h, 18)
		wbool(h, ex.Move)
		for _, p := range ex.Params {
			ws(h, p.Name)
			hTypeRef(h, p.Type)
			hRefMod(h, p.RefMod)
		}
		wb(h, 0xFE)
		hTypeRef(h, ex.ReturnType)
		if ex.Body != nil {
			wb(h, 1)
			hBlock(h, ex.Body)
		} else {
			wb(h, 0)
			hExpr(h, ex.ExprBody)
		}
	case *ast.IntLit:
		wb(h, 19)
		ws(h, ex.Raw)
		ws(h, ex.Suffix)
	case *ast.FloatLit:
		wb(h, 20)
		ws(h, ex.Raw)
		ws(h, ex.Suffix)
	case *ast.BoolLit:
		wb(h, 21)
		wbool(h, ex.Value)
	case *ast.NoneLit:
		wb(h, 22)
	case *ast.CharLit:
		wb(h, 23)
		ws(h, ex.Raw)
	case *ast.StringLit:
		wb(h, 24)
		wb(h, byte(ex.Kind))
		if len(ex.Parts) > 0 {
			wb(h, 1)
			for _, part := range ex.Parts {
				hStringPart(h, part)
			}
			wb(h, 0xFE)
		} else {
			wb(h, 0)
			ws(h, ex.Raw)
		}
	case *ast.IdentExpr:
		wb(h, 25)
		ws(h, ex.Name)
	case *ast.ThisExpr:
		wb(h, 26)
	case *ast.ParenExpr:
		wb(h, 27)
		hExpr(h, ex.Expr)
	case *ast.TupleLit:
		wb(h, 28)
		for _, el := range ex.Elements {
			hExpr(h, el)
		}
		wb(h, 0xFE)
	case *ast.ArrayLit:
		wb(h, 29)
		for _, el := range ex.Elements {
			hExpr(h, el)
		}
		wb(h, 0xFE)
	case *ast.MapLit:
		wb(h, 30)
		for _, entry := range ex.Entries {
			hExpr(h, entry.Key)
			hExpr(h, entry.Value)
		}
		wb(h, 0xFE)
	default:
		panic(fmt.Sprintf("typehash: unhandled Expr %T", e))
	}
}

func hStringPart(h hash.Hash, p ast.StringPart) {
	switch part := p.(type) {
	case ast.StringText:
		wb(h, 1)
		ws(h, part.Text)
	case ast.StringEscape:
		wb(h, 2)
		ws(h, part.Sequence)
	case ast.StringInterp:
		wb(h, 3)
		hExpr(h, part.Expr)
	default:
		panic(fmt.Sprintf("typehash: unhandled StringPart %T", p))
	}
}

func hMatchArm(h hash.Hash, arm *ast.MatchArm) {
	hMatchPattern(h, arm.Pattern)
	if arm.Guard != nil {
		wb(h, 1)
		hExpr(h, arm.Guard)
	} else {
		wb(h, 0)
	}
	if arm.Body != nil {
		wb(h, 1)
		hExpr(h, arm.Body)
	} else {
		wb(h, 0)
		hBlock(h, arm.Block)
	}
}

func hMatchPattern(h hash.Hash, p ast.MatchPattern) {
	if p == nil {
		wb(h, 0)
		return
	}
	switch pat := p.(type) {
	case *ast.EnumDestructureMatchPattern:
		wb(h, 1)
		ws(h, pat.Module)
		ws(h, pat.Enum)
		ws(h, pat.Variant)
		for _, b := range pat.Bindings {
			ws(h, b)
		}
		wb(h, 0xFE)
	case *ast.EnumVariantMatchPattern:
		wb(h, 2)
		ws(h, pat.Module)
		ws(h, pat.Enum)
		ws(h, pat.Variant)
	case *ast.TypeBindingMatchPattern:
		wb(h, 3)
		ws(h, pat.TypeName)
		ws(h, pat.Binding)
	case *ast.ShortDestructureMatchPattern:
		wb(h, 4)
		ws(h, pat.Name)
		for _, b := range pat.Bindings {
			ws(h, b)
		}
		wb(h, 0xFE)
	case *ast.NameMatchPattern:
		wb(h, 5)
		ws(h, pat.Name)
	case *ast.LiteralMatchPattern:
		wb(h, 6)
		hExpr(h, pat.Value)
	case *ast.WildcardMatchPattern:
		wb(h, 7)
	case *ast.ExpressionMatchPattern:
		wb(h, 8)
		hExpr(h, pat.Expr)
	default:
		panic(fmt.Sprintf("typehash: unhandled MatchPattern %T", p))
	}
}

func hIsPattern(h hash.Hash, p ast.IsPattern) {
	if p == nil {
		wb(h, 0)
		return
	}
	switch pat := p.(type) {
	case *ast.DestructureIsPattern:
		wb(h, 1)
		ws(h, pat.TypeName)
		for _, b := range pat.Bindings {
			ws(h, b)
		}
		wb(h, 0xFE)
	case *ast.IdentIsPattern:
		wb(h, 2)
		ws(h, pat.Name)
	default:
		panic(fmt.Sprintf("typehash: unhandled IsPattern %T", p))
	}
}
