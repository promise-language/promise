package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// --- `_validate!` invariants (T1752, docs/language-design.md#constructors → Validation) ---
//
// Sema decides WHERE the chain runs and records it in Info.ValidateSites; this
// file only emits it. Every site is a construction expression whose concrete
// type is exactly known, so the chain is a static call sequence — nothing here
// dispatches through a vtable or typeinfo.
//
// There are two emission shapes, sharing emitValidateChain:
//
//   - wrap:      a construction received outside the type's own construction
//                paths. Sema also marked the expression failable, so the value
//                becomes `{ i1, V, i8* }` and the ordinary `?` / `^` / `?!`
//                machinery consumes it.
//   - propagate: a `Self` the enclosing `factory built, validated at the
//                `return` that lets it leave. The value stays unwrapped and a
//                raise returns the error from the factory, exactly like an
//                auto-propagated failable call.

// validateChainNames returns the IR names of the `_validate!` functions that
// must run for a construction of typ, ROOT FIRST — a parent's invariant runs
// before its child's, and both see the complete instance. Each link is named
// against the instance it belongs to, so a generic parent (`Child[T] is
// Base[T]`) resolves to `Base__int._validate`, not `Base._validate`.
func (c *Compiler) validateChainNames(typ types.Type) []string {
	named := extractNamed(typ)
	if named == nil {
		return nil
	}
	var args []types.Type
	if inst, ok := unwrapRefsType(typ).(*types.Instance); ok {
		args = inst.TypeArgs()
	}
	var out []string
	c.appendValidateLinks(named, args, c.resolveTypeName(typ), make(map[*types.Named]bool), &out)
	return out
}

// appendValidateLinks walks named's ancestry parent-first, appending the
// mangled `_validate!` name of every type that declares one. name is the
// already-resolved IR name for `named` itself (which the caller knows exactly
// for the constructed type); parents are named from their resolved type args.
func (c *Compiler) appendValidateLinks(named *types.Named, args []types.Type, name string, seen map[*types.Named]bool, out *[]string) {
	if named == nil || seen[named] {
		return
	}
	seen[named] = true

	// Substitution from named's own type params to the args it was built with,
	// so an inheritance clause written `is Base[T]` resolves T for the parent.
	subst := types.BuildSubstMap(named.TypeParams(), args)
	for _, pr := range named.Parents() {
		var parentArgs []types.Type
		if len(pr.TypeArgs) > 0 && len(pr.Named.TypeParams()) > 0 {
			parentArgs = make([]types.Type, len(pr.TypeArgs))
			for i, ta := range pr.TypeArgs {
				if subst != nil {
					ta = types.Substitute(ta, subst)
				}
				if c.typeSubst != nil {
					ta = types.Substitute(ta, c.typeSubst)
				}
				parentArgs[i] = ta
			}
		}
		parentName := pr.Named.Obj().Name()
		if parentArgs != nil {
			parentName = monoName(types.NewInstance(pr.Named, parentArgs))
		}
		c.appendValidateLinks(pr.Named, parentArgs, parentName, seen, out)
	}

	if named.OwnValidateMethod() == nil {
		return
	}
	if name == "" {
		name = named.Obj().Name()
	}
	*out = append(*out, mangleMethodName(name, types.ValidateMethodName, false))
}

// validateChainNamesForEnum returns the single `_validate!` link of an enum.
// Enums do not inherit, so there is never more than one.
func (c *Compiler) validateChainNamesForEnum(typ types.Type) []string {
	enum := extractEnum(typ)
	if enum == nil || enum.OwnValidateMethod() == nil {
		return nil
	}
	return []string{mangleMethodName(c.resolveEnumTypeName(typ), types.ValidateMethodName, false)}
}

// resolveEnumTypeName is resolveTypeName's enum counterpart: the IR name of
// the enum typ denotes, monomorphized when it is a generic instance. Unlike
// resolveTypeName it applies c.typeSubst itself, so a caller inside a
// monomorphized body cannot forget to. (Several older sites open-code this
// three-line shape inline; they are left alone here.)
func (c *Compiler) resolveEnumTypeName(typ types.Type) string {
	typ = unwrapRefsType(typ)
	if c.typeSubst != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	if inst, ok := typ.(*types.Instance); ok {
		return monoName(inst)
	}
	if enum := extractEnum(typ); enum != nil {
		return enum.Obj().Name()
	}
	return ""
}

// emitValidateChain calls every link of the chain on recvPtr in order, checking
// each one's error tag. The first raise branches to a shared error block; on
// return, c.block is the OK continuation and errBlock holds errVal (the raised
// error pointer) as its first instruction. The caller terminates errBlock.
//
// Returns (nil, nil) when the chain is empty, leaving c.block untouched.
func (c *Compiler) emitValidateChain(recvPtr value.Value, chain []string) (*ir.Block, value.Value) {
	if len(chain) == 0 {
		return nil, nil
	}
	errBlock := c.newBlock("validate.err")

	type incoming struct {
		val  value.Value
		from *ir.Block
	}
	var errs []incoming

	for _, mangled := range chain {
		fn, ok := c.funcs[mangled]
		if !ok {
			panic(fmt.Sprintf("codegen: undeclared %s (T1752)", mangled))
		}
		result := c.block.NewCall(fn, recvPtr)
		resultType := result.Type().(*irtypes.StructType)
		tag := c.block.NewExtractValue(result, 0)
		errVal := c.block.NewExtractValue(result, resultErrIdx(resultType))
		errs = append(errs, incoming{val: errVal, from: c.block})

		okBlock := c.newBlock("validate.ok")
		c.block.NewCondBr(tag, errBlock, okBlock)
		c.block = okBlock
	}

	// The phi must be errBlock's first instruction; nothing has been added yet.
	var errVal value.Value
	if len(errs) == 1 {
		errVal = errs[0].val
	} else {
		incomings := make([]*ir.Incoming, len(errs))
		for i, in := range errs {
			incomings[i] = ir.NewIncoming(in.val, in.from)
		}
		errVal = errBlock.NewPhi(incomings...)
	}
	return errBlock, errVal
}

// wrapWithValidateChain runs the chain on recvPtr and yields val wrapped in a
// failable result: ok(val) when every link passed, err(e) on the first raise.
// This is the "wrap" shape — sema marked the construction expression failable,
// so the caller's `?` / `^` / `?!` unwraps it.
//
// The instance is COMPLETE when validation runs, so on the error path it must
// not leak. It is still an unclaimed heap temp carrying the type's full drop
// (updateConstructorTempDrop runs before this), and the error path the caller
// takes — auto-propagation, an inline handler, or statement end — drains those
// temps. Nothing extra is emitted here.
func (c *Compiler) wrapWithValidateChain(val, recvPtr value.Value, chain []string) value.Value {
	errBlock, errVal := c.emitValidateChain(recvPtr, chain)
	if errBlock == nil {
		return val
	}
	resultType := computeResultType(val.Type())
	mergeBlock := c.newBlock("validate.merge")

	okResult := c.wrapOk(val, resultType)
	okEnd := c.block
	c.block.NewBr(mergeBlock)

	c.block = errBlock
	errResult := c.wrapError(errVal, resultType)
	c.block.NewBr(mergeBlock)

	c.block = mergeBlock
	return c.block.NewPhi(ir.NewIncoming(okResult, okEnd), ir.NewIncoming(errResult, errBlock))
}

// constructionValidateChain returns the chain to run at construction
// expression e, or nil when sema did not mark it a wrap site. It is the single
// place the site kind and the chain lookup are combined, so no call site has to
// compute a chain it will not use.
func (c *Compiler) constructionValidateChain(e ast.Expr, typ types.Type) []string {
	if c.info.ValidateSites[e] != sema.ValidateWrap {
		return nil
	}
	if extractEnum(typ) != nil {
		return c.validateChainNamesForEnum(typ)
	}
	return c.validateChainNames(typ)
}

// emitConstructionValidate is the construction-expression entry point: it wraps
// val into a failable result when the chain runs, and returns val untouched
// otherwise. The bool says which happened, so a caller that must merge with
// another error path does not have to infer it from the value's LLVM type.
// recvPtr is the instance pointer (heap types) or a pointer to the value struct
// (value types and enums) — what a shared `this` receiver takes.
func (c *Compiler) emitConstructionValidate(e ast.Expr, val, recvPtr value.Value, typ types.Type) (value.Value, bool) {
	chain := c.constructionValidateChain(e, typ)
	if len(chain) == 0 {
		return val, false
	}
	return c.wrapWithValidateChain(val, recvPtr, chain), true
}

// valueStructRecvPtr materializes a pointer to a by-value aggregate (a value
// type's or an enum's value struct) so a shared `this` receiver can be passed.
func (c *Compiler) valueStructRecvPtr(val value.Value) value.Value {
	alloca := c.createEntryAlloca(val.Type())
	c.block.NewStore(val, alloca)
	return c.block.NewBitCast(alloca, irtypes.I8Ptr)
}

// emitDeferredReturnValidate is the "propagate" shape: the `return` of a
// `Self` the enclosing `factory constructed. The instance leaves the type's own
// construction paths here, after any post-construction `final fixups, so this
// is where its deferred chain runs. A raise returns the error from the factory,
// with the same cleanup an auto-propagated failable call performs — which is
// what drops the instance the factory would otherwise leak.
//
// Must run BEFORE the return's ownership move-out (drop-flag clear, temp
// claims), so the error path's cleanup still owns the instance.
func (c *Compiler) emitDeferredReturnValidate(s *ast.ReturnStmt, val value.Value) {
	if s.Value == nil || val == nil || c.info.ValidateSites[s.Value] != sema.ValidatePropagate {
		return
	}
	typ := c.info.Types[s.Value]
	if c.typeSubst != nil && typ != nil {
		typ = types.Substitute(typ, c.typeSubst)
	}
	var chain []string
	var recvPtr value.Value
	if extractEnum(typ) != nil {
		// Unreachable today: deferral is recorded only from checkTypeDecl's
		// `factory branch, and checkEnumDecl does not set inFactoryBody, so an
		// enum never gets a ValidatePropagate site. Kept because dropping it
		// would turn a future enum-factory deferral into a SILENT skip —
		// validateChainNames returns nil for an enum — rather than a live path.
		chain = c.validateChainNamesForEnum(typ)
		if len(chain) == 0 {
			return
		}
		recvPtr = c.valueStructRecvPtr(val)
	} else {
		chain = c.validateChainNames(typ)
		if len(chain) == 0 {
			return
		}
		// A shared `this` receiver takes the instance pointer for a heap type
		// and a pointer to the value struct for a value type.
		if named := extractNamed(typ); named != nil && named.IsValueType() {
			recvPtr = c.valueStructRecvPtr(val)
		} else {
			recvPtr = c.extractInstancePtr(val)
		}
	}

	errBlock, errVal := c.emitValidateChain(recvPtr, chain)
	if errBlock == nil {
		return
	}
	okBlock := c.block

	// Error path: unwind exactly like genErrorPropagateExpr, so the instance and
	// every temp built for this return are reclaimed before the factory raises.
	c.block = errBlock
	c.emitAllStmtTempCleanupForErrorPath()
	if len(c.scopeBindings) > 0 {
		c.emitScopeCleanup(0, true) // error in flight — suppress close errors
	}
	if c.inFailableGoBlock {
		c.emitFailableGoBlockError(errVal)
	} else if c.inGenerator && c.generatorCanError {
		c.emitGeneratorError(errVal)
	} else {
		c.block.NewRet(c.wrapError(errVal, c.currentResultType()))
	}

	c.block = okBlock
}
