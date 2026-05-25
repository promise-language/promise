package codegen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/types"
)

// monoContext holds the context for generating code inside a monomorphic method body.
// Inside mono methods, info.Types[this] is the origin Named type (not Instance).
// monoCtx maps the origin type to its monomorphic layout.
type monoContext struct {
	inst   *types.Instance
	origin types.Type // *Named or *Enum
	name   string     // "Box[int]"
}

// monoName generates a unique mangled name for a generic type instantiation.
// Uses bracket notation so the name is human-readable and unambiguous:
//
//	Instance{Box, [int]}           → "Box[int]"
//	Instance{Pair, [int, string]}  → "Pair[int, string]"
//	Instance{Map, [string, Vec[int]]} → "Map[string, Vec[int]]"
//
// Since '[' and ']' are not valid Promise identifier characters, there is no
// collision with user-defined type names. The llir/llvm library automatically
// quotes LLVM identifiers containing these characters (e.g. @"Box[int].push").

// instanceOriginTypeName returns the type name of a generic instance's origin.
func instanceOriginTypeName(inst *types.Instance) string {
	switch o := inst.Origin().(type) {
	case *types.Named:
		return o.Obj().Name()
	case *types.Enum:
		return o.Obj().Name()
	default:
		return ""
	}
}

func monoName(inst *types.Instance) string {
	var name string
	switch o := inst.Origin().(type) {
	case *types.Named:
		name = o.Obj().Name()
	case *types.Enum:
		name = o.Obj().Name()
	default:
		name = "unknown"
	}
	args := inst.TypeArgs()
	name += "["
	for i, arg := range args {
		if i > 0 {
			name += ", "
		}
		name += typeArgStr(arg)
	}
	name += "]"
	return name
}

// typeArgStr returns the string representation of a type argument in mono names.
// Named and Enum use their short name. Instance uses monoName() (which uses
// bracket notation and avoids Instance.String()'s Vector short-form "T[]").
// Compound types (Tuple, Optional, SharedRef, MutRef, Pointer, Array) are
// formatted by recursively calling typeArgStr on their elements, so that any
// nested Instance types also use bracket notation. TypeParam uses the param name.
// Other types (function signatures) fall back to typ.String().
func typeArgStr(typ types.Type) string {
	if typ == nil {
		panic("codegen: nil type in generic type argument")
	}
	switch t := typ.(type) {
	case *types.Named:
		return t.Obj().Name()
	case *types.Enum:
		return t.Obj().Name()
	case *types.Instance:
		return monoName(t)
	case *types.Tuple:
		var b strings.Builder
		b.WriteByte('(')
		for i, e := range t.Elems() {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(typeArgStr(e))
		}
		b.WriteByte(')')
		return b.String()
	case *types.Optional:
		return typeArgStr(t.Elem()) + "?"
	case *types.SharedRef:
		return typeArgStr(t.Elem()) + "&"
	case *types.MutRef:
		return typeArgStr(t.Elem()) + "~"
	case *types.Pointer:
		return typeArgStr(t.Elem()) + "*"
	case *types.Array:
		return fmt.Sprintf("%s[%d]", typeArgStr(t.Elem()), t.Size())
	case *types.TypeParam:
		return t.Obj().Name()
	default:
		return typ.String()
	}
}

// monoFuncName generates a unique mangled name for a generic function instantiation.
// Example: identity[int] → "identity[int]"
func monoFuncName(fi *sema.FuncInstance) string {
	name := fi.Func.Name() + "["
	for i, arg := range fi.TypeArgs {
		if i > 0 {
			name += ", "
		}
		name += typeArgStr(arg)
	}
	return name + "]"
}

// buildCallTypeArgSubst (T0418) builds a substitution map from a callee's
// TypeParams to the call site's concrete type arguments, parsed from AST
// expressions. Each type-arg expr's resolved type is also passed through
// c.typeSubst so a generic outer caller's TypeParams resolve too.
// Returns nil if tparams is empty or the lengths don't match.
func (c *Compiler) buildCallTypeArgSubst(tparams []*types.TypeParam, typeArgExprs []ast.Expr) map[*types.TypeParam]types.Type {
	if len(tparams) == 0 || len(tparams) != len(typeArgExprs) {
		return nil
	}
	targs := make([]types.Type, len(typeArgExprs))
	for i, expr := range typeArgExprs {
		ta := c.info.Types[expr]
		if c.typeSubst != nil && ta != nil {
			ta = types.Substitute(ta, c.typeSubst)
		}
		targs[i] = ta
	}
	return types.BuildSubstMap(tparams, targs)
}

// buildInferredCallSubst (T0418) builds a substitution map from a callee's
// TypeParams to already-resolved concrete type arguments (sema-inferred).
// Each type arg is also passed through c.typeSubst.
// Returns nil if tparams is empty or the lengths don't match.
func (c *Compiler) buildInferredCallSubst(tparams []*types.TypeParam, targs []types.Type) map[*types.TypeParam]types.Type {
	if len(tparams) == 0 || len(tparams) != len(targs) {
		return nil
	}
	resolved := make([]types.Type, len(targs))
	for i, ta := range targs {
		if c.typeSubst != nil && ta != nil {
			ta = types.Substitute(ta, c.typeSubst)
		}
		resolved[i] = ta
	}
	return types.BuildSubstMap(tparams, resolved)
}

// buildOwnerTypeArgSubst (T0418) builds a substitution map for a generic
// type instance's TypeParams from targetType (typically *types.Instance).
// Includes mergeParentSubst so inherited generic parents are also covered.
// Returns nil for non-generic owners.
func (c *Compiler) buildOwnerTypeArgSubst(targetType types.Type) map[*types.TypeParam]types.Type {
	inst, ok := targetType.(*types.Instance)
	if !ok {
		return nil
	}
	origin, ok := inst.Origin().(*types.Named)
	if !ok || len(origin.TypeParams()) == 0 {
		return nil
	}
	subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
	if subst == nil {
		return nil
	}
	mergeParentSubst(origin, subst)
	return subst
}

// mergeSubstMaps (T0418) returns a non-destructive union of two subst maps.
// Mappings in b take precedence over a. Returns nil if both are nil.
func mergeSubstMaps(a, b map[*types.TypeParam]types.Type) map[*types.TypeParam]types.Type {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	out := make(map[*types.TypeParam]types.Type, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// mergeParentSubst augments a type param substitution map with mappings for
// inherited generic parent type params. E.g., if Derived[T] is Base[T] and
// subst = {Derived.T → int}, this adds {Base.T → int} so that inherited
// fields/methods using Base.T are correctly resolved.
func mergeParentSubst(origin *types.Named, subst map[*types.TypeParam]types.Type) {
	for _, pr := range origin.Parents() {
		if len(pr.TypeArgs) == 0 {
			// Non-generic parent — still recurse for its parents.
			mergeParentSubst(pr.Named, subst)
			continue
		}
		resolvedArgs := make([]types.Type, len(pr.TypeArgs))
		for i, ta := range pr.TypeArgs {
			resolvedArgs[i] = types.Substitute(ta, subst)
		}
		parentMap := types.BuildSubstMap(pr.Named.TypeParams(), resolvedArgs)
		for k, v := range parentMap {
			subst[k] = v
		}
		// Recurse into parent's parents for transitive chains.
		mergeParentSubst(pr.Named, subst)
	}
}

// collectMonoInstances deduplicates generic type instances by mangled name.
// Also transitively discovers instances referenced by field types of already-collected
// instances (e.g., map[string, int] has a Slot[K, V][] field which after substitution
// requires Slot[string, int] to be monomorphized).
// Returns the instances and a set of "spiral" instance names: instances added by the
// spiral guard that should not have their inherited default method bodies generated
// (see instanceArgSpiralCheck for details).
func collectMonoInstances(info *sema.Info, spiralInstances map[string]bool) []*types.Instance {
	seen := map[string]bool{}
	var result []*types.Instance
	for _, inst := range info.Instances {
		key := monoName(inst)
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, inst)
	}

	// Collect unresolved instances from info.Types (instances with TypeParams).
	// These arise in generic method bodies: e.g., Iterator[T].filter() creates
	// _FnIter[T]. Sema records the expression types but skips recording the
	// instance because it contains TypeParams.
	unresolvedInsts := collectUnresolvedInstances(info)
	wrapsCache := make(map[types.Type]bool)

	// Transitively expand: walk substituted field types, parent instances,
	// and resolve unresolved method-body instances.
	for i := 0; i < len(result); i++ {
		inst := result[i]
		instKey := monoName(inst)
		if spiralInstances[instKey] {
			continue // spiral instances: skip transitive expansion to prevent infinite growth
		}
		switch origin := inst.Origin().(type) {
		case *types.Named:
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			for _, f := range origin.AllFields() {
				ft := types.Substitute(f.Type(), subst)
				discoverInstances(ft, &result, seen)
			}
			// Discover parent instances: Range[int] is Stream[T] → need Stream[int]
			for _, pr := range origin.Parents() {
				if len(pr.TypeArgs) > 0 {
					resolvedArgs := make([]types.Type, len(pr.TypeArgs))
					for j, ta := range pr.TypeArgs {
						resolvedArgs[j] = types.Substitute(ta, subst)
					}
					parentInst := types.NewInstance(pr.Named, resolvedArgs)
					if !types.ContainsTypeParam(parentInst) {
						discoverInstances(parentInst, &result, seen)
					}
				}
			}
			// Resolve unresolved instances from method bodies.
			// E.g., Iterator[int] has subst {T→int}; _FnIter[T] in method
			// bodies resolves to _FnIter[int].
			if len(subst) > 0 {
				resolveUnresolvedInstances(unresolvedInsts, subst, &result, seen, spiralInstances, wrapsCache)
			}
		case *types.Enum:
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			for _, v := range origin.Variants() {
				for _, f := range v.Fields() {
					ft := types.Substitute(f.Type(), subst)
					discoverInstances(ft, &result, seen)
				}
			}
			if len(subst) > 0 {
				resolveUnresolvedInstances(unresolvedInsts, subst, &result, seen, spiralInstances, wrapsCache)
			}
		}
	}

	// Sort by name for deterministic LLVM type declaration order (info.Types
	// iteration in collectUnresolvedInstances is non-deterministic).
	sort.Slice(result, func(i, j int) bool { return monoName(result[i]) < monoName(result[j]) })
	return result
}

// resolveTypeInstancesFromFuncInstances uses substitution maps from concrete
// generic function/method instances to resolve unresolved type instances that
// appear in generic function bodies. For example, make_app_error[T]() constructs
// AppError[T] — when instantiated as make_app_error[int], the subst {T→int}
// resolves AppError[T] → AppError[int]. (B0134)
func resolveTypeInstancesFromFuncInstances(
	info *sema.Info,
	existing []*types.Instance,
	funcInstances []*sema.FuncInstance,
	methodInstances []*sema.MethodInstance,
	spiralInstances map[string]bool,
) []*types.Instance {
	unresolvedInsts := collectUnresolvedInstances(info)
	if len(unresolvedInsts) == 0 {
		return nil
	}

	seen := map[string]bool{}
	for _, inst := range existing {
		seen[monoName(inst)] = true
	}

	var newInstances []*types.Instance
	wrapsCache := make(map[types.Type]bool)

	// Apply substitution maps from concrete func instances.
	for _, fi := range funcInstances {
		sig := fi.Func.Type().(*types.Signature)
		subst := types.BuildSubstMap(sig.TypeParams(), fi.TypeArgs)
		if len(subst) > 0 {
			resolveUnresolvedInstances(unresolvedInsts, subst, &newInstances, seen, spiralInstances, wrapsCache)
		}
	}

	// Apply substitution maps from concrete method instances.
	for _, mi := range methodInstances {
		subst := buildMethodInstanceSubst(mi)
		if len(subst) > 0 {
			resolveUnresolvedInstances(unresolvedInsts, subst, &newInstances, seen, spiralInstances, wrapsCache)
		}
	}

	// Transitively expand new instances (their fields/parents may reference
	// further generic types that need to be resolved).
	for i := 0; i < len(newInstances); i++ {
		inst := newInstances[i]
		instKey := monoName(inst)
		if spiralInstances[instKey] {
			continue
		}
		switch origin := inst.Origin().(type) {
		case *types.Named:
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			for _, f := range origin.AllFields() {
				ft := types.Substitute(f.Type(), subst)
				discoverInstances(ft, &newInstances, seen)
			}
			for _, pr := range origin.Parents() {
				if len(pr.TypeArgs) > 0 {
					resolvedArgs := make([]types.Type, len(pr.TypeArgs))
					for j, ta := range pr.TypeArgs {
						resolvedArgs[j] = types.Substitute(ta, subst)
					}
					parentInst := types.NewInstance(pr.Named, resolvedArgs)
					if !types.ContainsTypeParam(parentInst) {
						discoverInstances(parentInst, &newInstances, seen)
					}
				}
			}
			if len(subst) > 0 {
				resolveUnresolvedInstances(unresolvedInsts, subst, &newInstances, seen, spiralInstances, wrapsCache)
			}
		case *types.Enum:
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			for _, v := range origin.Variants() {
				for _, f := range v.Fields() {
					ft := types.Substitute(f.Type(), subst)
					discoverInstances(ft, &newInstances, seen)
				}
			}
			if len(subst) > 0 {
				resolveUnresolvedInstances(unresolvedInsts, subst, &newInstances, seen, spiralInstances, wrapsCache)
			}
		}
	}

	return newInstances
}

// collectUnresolvedInstances scans info.Types for Instance types that contain
// TypeParams. These come from generic method bodies where sema type-checks
// once with TypeParams unresolved (e.g., _FnIter[T] inside Iterator[T].filter()).
func collectUnresolvedInstances(info *sema.Info) []*types.Instance {
	visited := make(map[*types.Instance]bool)
	var result []*types.Instance
	for _, typ := range info.Types {
		findUnresolvedInstances(typ, &result, visited)
	}
	return result
}

// findUnresolvedInstances recursively walks a type to find Instance types
// that contain TypeParams.
func findUnresolvedInstances(typ types.Type, result *[]*types.Instance, visited map[*types.Instance]bool) {
	if typ == nil {
		return
	}
	switch t := typ.(type) {
	case *types.Instance:
		if types.ContainsTypeParam(t) && !visited[t] {
			visited[t] = true
			*result = append(*result, t)
		}
		for _, arg := range t.TypeArgs() {
			findUnresolvedInstances(arg, result, visited)
		}
	case *types.Optional:
		findUnresolvedInstances(t.Elem(), result, visited)
	case *types.Tuple:
		for _, e := range t.Elems() {
			findUnresolvedInstances(e, result, visited)
		}
	case *types.Signature:
		for _, p := range t.Params() {
			findUnresolvedInstances(p.Type(), result, visited)
		}
		findUnresolvedInstances(t.Result(), result, visited)
	case *types.SharedRef:
		findUnresolvedInstances(t.Elem(), result, visited)
	case *types.MutRef:
		findUnresolvedInstances(t.Elem(), result, visited)
	case *types.Array:
		findUnresolvedInstances(t.Elem(), result, visited)
	}
}

// resolveUnresolvedInstances applies a substitution map to unresolved instances
// and adds any newly concrete instances to the result.
//
// Spiral guard: methods like Iterator.enumerate create unresolved instances of the
// form _FnIter[(int, T)]. When T is substituted with a compound type X (Tuple,
// Instance, etc.), this produces _FnIter[(int, X)]. Its parent Iterator[(int, X)]
// then re-triggers the substitution with {T → (int, X)}, producing
// _FnIter[(int, (int, X))], and so on infinitely. We detect this pattern by
// checking whether any resolved type arg strictly contains a compound substitution
// value as a proper sub-expression. Direct user code seeded via info.Instances
// bypasses this guard, so explicit .enumerate() calls still work.
func resolveUnresolvedInstances(unresolved []*types.Instance, subst map[*types.TypeParam]types.Type, result *[]*types.Instance, seen map[string]bool, spiralInstances map[string]bool, wrapsCache map[types.Type]bool) {
	for _, ui := range unresolved {
		resolved := types.Substitute(ui, subst)
		if resolved == ui {
			continue // substitution didn't change anything
		}
		if types.ContainsTypeParam(resolved) {
			continue // still has unresolved TypeParams
		}
		if ri, ok := resolved.(*types.Instance); ok {
			key := monoName(ri)
			if seen[key] {
				continue
			}
			seen[key] = true
			*result = append(*result, ri)
			// Spiral guard: instances whose type args strictly contain a compound
			// substitution value AND whose origin intrinsically wraps its TypeParams
			// in a Tuple are added for layout purposes but marked no-expand. The
			// origin-wraps precondition prevents over-marking benign cases like
			// Vector[(string, X)] where Vector itself doesn't spiral; only origins
			// like Iterator/_FnIter (whose enumerate/zip return Iterator[(..., T)])
			// can chain via inherited default-method bodies.
			if instanceArgSpiralCheck(ri, subst) && originWrapsTypeParams(ri.Origin(), wrapsCache) {
				spiralInstances[key] = true
			}
		}
	}
}

// originWrapsTypeParams reports whether a Named/Enum origin intrinsically
// references its own TypeParams from inside a Tuple type — either directly
// in its method signatures or fields, or transitively through a structural
// parent. This is the precondition for marking an instance as a spiral.
//
// Iterator[T] satisfies this (enumerate() returns Iterator[(int, T)]).
// _FnIter[T] satisfies this transitively (is Iterator[T]).
// Vector[T] does NOT (iter() returns Iterator[T] without Tuple wrap).
//
// Without this precondition, Map[K, (K, V)] paths over-mark Vector[(K, V)]
// as spiral, which prevents resolving _FnIter[T] from Vector.iter()'s body
// and panics at codegen with "no layout for type _FnIter[...]" (T0400).
func originWrapsTypeParams(origin types.Type, cache map[types.Type]bool) bool {
	if origin == nil {
		return false
	}
	if v, ok := cache[origin]; ok {
		return v
	}
	// Tentative false: prevents infinite recursion if structural parents form a
	// cycle. Updated to true below if a wrap is found.
	cache[origin] = false

	var typeParams []*types.TypeParam
	var parents []*types.ParentRef
	var fieldTypes []types.Type
	var sigs []*types.Signature

	switch o := origin.(type) {
	case *types.Named:
		typeParams = o.TypeParams()
		parents = o.Parents()
		for _, f := range o.AllFields() {
			fieldTypes = append(fieldTypes, f.Type())
		}
		for _, m := range o.Methods() {
			if m.Sig() != nil {
				sigs = append(sigs, m.Sig())
			}
		}
	case *types.Enum:
		typeParams = o.TypeParams()
		for _, v := range o.Variants() {
			for _, f := range v.Fields() {
				fieldTypes = append(fieldTypes, f.Type())
			}
		}
		for _, m := range o.Methods() {
			if m.Sig() != nil {
				sigs = append(sigs, m.Sig())
			}
		}
	default:
		return false
	}

	if len(typeParams) == 0 {
		return false
	}

	tpSet := make(map[*types.TypeParam]bool, len(typeParams))
	for _, tp := range typeParams {
		tpSet[tp] = true
	}

	for _, ft := range fieldTypes {
		if containsTupleWrappingTypeParams(ft, tpSet) {
			cache[origin] = true
			return true
		}
	}

	for _, sig := range sigs {
		if sig.Recv() != nil && containsTupleWrappingTypeParams(sig.Recv().Type(), tpSet) {
			cache[origin] = true
			return true
		}
		for _, p := range sig.Params() {
			if containsTupleWrappingTypeParams(p.Type(), tpSet) {
				cache[origin] = true
				return true
			}
		}
		if sig.Result() != nil && containsTupleWrappingTypeParams(sig.Result(), tpSet) {
			cache[origin] = true
			return true
		}
	}

	for _, p := range parents {
		if p.Named != nil && p.Named.IsStructural() {
			if originWrapsTypeParams(p.Named, cache) {
				cache[origin] = true
				return true
			}
		}
	}

	return false
}

// containsTupleWrappingTypeParams reports whether t contains a Tuple anywhere
// in its structure where any element (recursively) references one of the
// target TypeParams.
func containsTupleWrappingTypeParams(t types.Type, tps map[*types.TypeParam]bool) bool {
	if t == nil {
		return false
	}
	if tup, ok := t.(*types.Tuple); ok {
		for _, e := range tup.Elems() {
			if walkTypeContainsTypeParam(e, tps) {
				return true
			}
		}
		// Also keep walking inside the tuple in case there's a deeper tuple.
	}
	switch tt := t.(type) {
	case *types.Tuple:
		for _, e := range tt.Elems() {
			if containsTupleWrappingTypeParams(e, tps) {
				return true
			}
		}
	case *types.Instance:
		for _, a := range tt.TypeArgs() {
			if containsTupleWrappingTypeParams(a, tps) {
				return true
			}
		}
	case *types.Optional:
		return containsTupleWrappingTypeParams(tt.Elem(), tps)
	case *types.SharedRef:
		return containsTupleWrappingTypeParams(tt.Elem(), tps)
	case *types.MutRef:
		return containsTupleWrappingTypeParams(tt.Elem(), tps)
	case *types.Pointer:
		return containsTupleWrappingTypeParams(tt.Elem(), tps)
	case *types.Array:
		return containsTupleWrappingTypeParams(tt.Elem(), tps)
	case *types.Signature:
		for _, p := range tt.Params() {
			if containsTupleWrappingTypeParams(p.Type(), tps) {
				return true
			}
		}
		if tt.Result() != nil {
			if containsTupleWrappingTypeParams(tt.Result(), tps) {
				return true
			}
		}
	}
	return false
}

// walkTypeContainsTypeParam reports whether t contains (anywhere in its
// structure) any of the target TypeParams.
func walkTypeContainsTypeParam(t types.Type, tps map[*types.TypeParam]bool) bool {
	if t == nil {
		return false
	}
	switch tt := t.(type) {
	case *types.TypeParam:
		return tps[tt]
	case *types.Instance:
		for _, a := range tt.TypeArgs() {
			if walkTypeContainsTypeParam(a, tps) {
				return true
			}
		}
	case *types.Tuple:
		for _, e := range tt.Elems() {
			if walkTypeContainsTypeParam(e, tps) {
				return true
			}
		}
	case *types.Optional:
		return walkTypeContainsTypeParam(tt.Elem(), tps)
	case *types.SharedRef:
		return walkTypeContainsTypeParam(tt.Elem(), tps)
	case *types.MutRef:
		return walkTypeContainsTypeParam(tt.Elem(), tps)
	case *types.Pointer:
		return walkTypeContainsTypeParam(tt.Elem(), tps)
	case *types.Array:
		return walkTypeContainsTypeParam(tt.Elem(), tps)
	case *types.Signature:
		for _, p := range tt.Params() {
			if walkTypeContainsTypeParam(p.Type(), tps) {
				return true
			}
		}
		if tt.Result() != nil {
			if walkTypeContainsTypeParam(tt.Result(), tps) {
				return true
			}
		}
	}
	return false
}

// instanceArgSpiralCheck reports whether any type arg of inst strictly contains
// a compound (non-Named, non-Enum) substitution value as a proper sub-expression.
// This detects expanding spirals like enumerate: Iterator[X] → _FnIter[(int,X)]
// → Iterator[(int,X)] → _FnIter[(int,(int,X))] → ... when X is a compound type.
func instanceArgSpiralCheck(inst *types.Instance, subst map[*types.TypeParam]types.Type) bool {
	for _, sv := range subst {
		// Only compound substitution values can cause spiral expansion.
		// Named/Enum primitives (int, string, MyType) never trigger this.
		switch sv.(type) {
		case *types.Named, *types.Enum, *types.TypeParam:
			continue
		}
		for _, ta := range inst.TypeArgs() {
			if typeStrictlyContains(ta, sv) {
				return true
			}
		}
	}
	return false
}

// typeStrictlyContains reports whether haystack contains needle as a proper
// sub-expression (i.e., needle appears strictly inside haystack, not equal to it).
func typeStrictlyContains(haystack, needle types.Type) bool {
	switch h := haystack.(type) {
	case *types.Instance:
		for _, a := range h.TypeArgs() {
			if a == needle || typeStrictlyContains(a, needle) {
				return true
			}
		}
	case *types.Tuple:
		for _, e := range h.Elems() {
			if e == needle || typeStrictlyContains(e, needle) {
				return true
			}
		}
	case *types.Optional:
		return h.Elem() == needle || typeStrictlyContains(h.Elem(), needle)
	case *types.SharedRef:
		return h.Elem() == needle || typeStrictlyContains(h.Elem(), needle)
	case *types.MutRef:
		return h.Elem() == needle || typeStrictlyContains(h.Elem(), needle)
	case *types.Pointer:
		return h.Elem() == needle || typeStrictlyContains(h.Elem(), needle)
	}
	return false
}

// discoverInstances recursively walks a type and collects any concrete Instance types.
func discoverInstances(t types.Type, result *[]*types.Instance, seen map[string]bool) {
	if t == nil {
		return
	}
	switch tt := t.(type) {
	case *types.Instance:
		if !types.ContainsTypeParam(tt) {
			key := monoName(tt)
			if !seen[key] {
				seen[key] = true
				*result = append(*result, tt)
			}
		}
		// Also check type args for nested instances
		for _, arg := range tt.TypeArgs() {
			discoverInstances(arg, result, seen)
		}
	case *types.Optional:
		discoverInstances(tt.Elem(), result, seen)
	case *types.SharedRef:
		discoverInstances(tt.Elem(), result, seen)
	case *types.MutRef:
		discoverInstances(tt.Elem(), result, seen)
	case *types.Pointer:
		discoverInstances(tt.Elem(), result, seen)
	case *types.Array:
		discoverInstances(tt.Elem(), result, seen)
	case *types.Tuple:
		for _, e := range tt.Elems() {
			discoverInstances(e, result, seen)
		}
	case *types.Signature:
		for _, p := range tt.Params() {
			discoverInstances(p.Type(), result, seen)
		}
		if tt.Result() != nil {
			discoverInstances(tt.Result(), result, seen)
		}
	}
}

// funcInstanceContainsTypeParam reports whether a FuncInstance's TypeArgs contain
// any unresolved TypeParams. Such instances arise when a generic function body
// calls another generic function using the outer function's type parameter:
//
//	wrap[T](T val) T { return identity[T](val); }
//
// Sema records FuncInstance{identity, [T]} where T is wrap's TypeParam.
func funcInstanceContainsTypeParam(fi *sema.FuncInstance) bool {
	for _, arg := range fi.TypeArgs {
		if types.ContainsTypeParam(arg) {
			return true
		}
	}
	return false
}

// methodInstanceContainsTypeParam reports whether a MethodInstance's TypeArgs
// or OwnerInst contain any unresolved TypeParams. Same pattern as funcInstance.
func methodInstanceContainsTypeParam(mi *sema.MethodInstance) bool {
	for _, arg := range mi.TypeArgs {
		if types.ContainsTypeParam(arg) {
			return true
		}
	}
	if mi.OwnerInst != nil && types.ContainsTypeParam(mi.OwnerInst) {
		return true
	}
	return false
}

// resolveUnresolvedFuncInstances applies a substitution map to unresolved FuncInstances
// and adds any newly concrete instances to the result.
func resolveUnresolvedFuncInstances(unresolved []*sema.FuncInstance, subst map[*types.TypeParam]types.Type, result *[]*sema.FuncInstance, seen map[string]bool) {
	for _, fi := range unresolved {
		// Substitute each TypeArg
		resolvedArgs := make([]types.Type, len(fi.TypeArgs))
		changed := false
		for i, arg := range fi.TypeArgs {
			resolved := types.Substitute(arg, subst)
			resolvedArgs[i] = resolved
			if resolved != arg {
				changed = true
			}
		}
		if !changed {
			continue // substitution didn't change anything
		}
		// Check if all args are now concrete
		hasTypeParam := false
		for _, arg := range resolvedArgs {
			if types.ContainsTypeParam(arg) {
				hasTypeParam = true
				break
			}
		}
		if hasTypeParam {
			continue // still has unresolved TypeParams
		}

		// Build the substituted signature
		sig := fi.Func.Type().(*types.Signature)
		fullSubst := types.BuildSubstMap(sig.TypeParams(), resolvedArgs)
		monoSig := types.Substitute(sig, fullSubst).(*types.Signature)

		resolved := &sema.FuncInstance{
			Func:     fi.Func,
			TypeArgs: resolvedArgs,
			Sig:      monoSig,
		}
		key := monoFuncName(resolved)
		if seen[key] {
			continue
		}
		seen[key] = true
		*result = append(*result, resolved)
	}
}

// resolveUnresolvedMethodInstances applies a substitution map to unresolved MethodInstances
// and adds any newly concrete instances to the result.
func resolveUnresolvedMethodInstances(unresolved []*sema.MethodInstance, subst map[*types.TypeParam]types.Type, result *[]*sema.MethodInstance, seen map[string]bool) {
	for _, mi := range unresolved {
		// Substitute each TypeArg
		resolvedArgs := make([]types.Type, len(mi.TypeArgs))
		changed := false
		for i, arg := range mi.TypeArgs {
			resolved := types.Substitute(arg, subst)
			resolvedArgs[i] = resolved
			if resolved != arg {
				changed = true
			}
		}

		// Substitute OwnerInst if present
		var resolvedOwnerInst *types.Instance
		if mi.OwnerInst != nil {
			resolved := types.Substitute(mi.OwnerInst, subst)
			if resolved != mi.OwnerInst {
				changed = true
			}
			if ri, ok := resolved.(*types.Instance); ok {
				resolvedOwnerInst = ri
			} else {
				continue // couldn't resolve to an instance
			}
		}

		if !changed {
			continue
		}

		// Check if all args are now concrete
		hasTypeParam := false
		for _, arg := range resolvedArgs {
			if types.ContainsTypeParam(arg) {
				hasTypeParam = true
				break
			}
		}
		if hasTypeParam {
			continue
		}
		if resolvedOwnerInst != nil && types.ContainsTypeParam(resolvedOwnerInst) {
			continue
		}

		// Build the substituted signature
		methodSubst := map[*types.TypeParam]types.Type{}
		if resolvedOwnerInst != nil {
			for k, v := range types.BuildSubstMap(mi.Owner.TypeParams(), resolvedOwnerInst.TypeArgs()) {
				methodSubst[k] = v
			}
		}
		for k, v := range types.BuildSubstMap(mi.Method.Sig().TypeParams(), resolvedArgs) {
			methodSubst[k] = v
		}
		monoSig := types.Substitute(mi.Method.Sig(), methodSubst).(*types.Signature)

		resolved := &sema.MethodInstance{
			Owner:     mi.Owner,
			OwnerInst: resolvedOwnerInst,
			Method:    mi.Method,
			TypeArgs:  resolvedArgs,
			Sig:       monoSig,
		}
		key := monoMethodInstanceName(resolved)
		if seen[key] {
			continue
		}
		seen[key] = true
		*result = append(*result, resolved)
	}
}

// crossResolveFuncMethodInstances performs cross-resolution between FuncInstances and
// MethodInstances. This handles cases where:
//   - A generic function calls a generic method (FuncInstance subst resolves MethodInstance)
//   - A generic method calls a generic function (MethodInstance subst resolves FuncInstance)
//
// Both collect functions already handle self-resolution (func→func, method→method) and
// type-instance resolution. This function handles the cross-cutting cases.
func crossResolveFuncMethodInstances(info *sema.Info, funcInstances *[]*sema.FuncInstance, methodInstances *[]*sema.MethodInstance) {
	// Collect unresolved instances from sema info
	var unresolvedFuncs []*sema.FuncInstance
	for _, fi := range info.FuncInstances {
		if funcInstanceContainsTypeParam(fi) {
			unresolvedFuncs = append(unresolvedFuncs, fi)
		}
	}
	var unresolvedMethods []*sema.MethodInstance
	for _, mi := range info.MethodInstances {
		if methodInstanceContainsTypeParam(mi) {
			unresolvedMethods = append(unresolvedMethods, mi)
		}
	}

	if len(unresolvedFuncs) == 0 && len(unresolvedMethods) == 0 {
		return
	}

	// Build seen sets from already-collected concrete instances
	funcSeen := make(map[string]bool, len(*funcInstances))
	for _, fi := range *funcInstances {
		funcSeen[monoFuncName(fi)] = true
	}
	methodSeen := make(map[string]bool, len(*methodInstances))
	for _, mi := range *methodInstances {
		methodSeen[monoMethodInstanceName(mi)] = true
	}

	// Cross-resolve using index-based iteration to process new discoveries.
	funcIdx := 0
	methodIdx := 0
	for funcIdx < len(*funcInstances) || methodIdx < len(*methodInstances) {
		// Use FuncInstance substitutions to resolve unresolved MethodInstances
		for funcIdx < len(*funcInstances) {
			fi := (*funcInstances)[funcIdx]
			funcIdx++
			sig := fi.Func.Type().(*types.Signature)
			subst := types.BuildSubstMap(sig.TypeParams(), fi.TypeArgs)
			if len(subst) > 0 {
				resolveUnresolvedMethodInstances(unresolvedMethods, subst, methodInstances, methodSeen)
				resolveUnresolvedFuncInstances(unresolvedFuncs, subst, funcInstances, funcSeen)
			}
		}
		// Use MethodInstance substitutions to resolve unresolved FuncInstances
		for methodIdx < len(*methodInstances) {
			mi := (*methodInstances)[methodIdx]
			methodIdx++
			subst := buildMethodInstanceSubst(mi)
			if len(subst) > 0 {
				resolveUnresolvedFuncInstances(unresolvedFuncs, subst, funcInstances, funcSeen)
				resolveUnresolvedMethodInstances(unresolvedMethods, subst, methodInstances, methodSeen)
			}
		}
	}
}

// crossResolveAccumulatedInstances resolves unresolved FuncInstances and
// MethodInstances in the accumulated cross-module extras using concrete
// instances from the same lists (B0344). This handles the case where
// module A's generic method calls module B's generic function — the
// FuncInstance from A's sema contains TypeParams that can be resolved
// by concrete method/func instances from user code or other modules.
func crossResolveAccumulatedInstances(funcInstances *[]*sema.FuncInstance, methodInstances *[]*sema.MethodInstance) {
	// Collect unresolved instances from the passed lists
	var unresolvedFuncs []*sema.FuncInstance
	for _, fi := range *funcInstances {
		if funcInstanceContainsTypeParam(fi) {
			unresolvedFuncs = append(unresolvedFuncs, fi)
		}
	}
	var unresolvedMethods []*sema.MethodInstance
	for _, mi := range *methodInstances {
		if methodInstanceContainsTypeParam(mi) {
			unresolvedMethods = append(unresolvedMethods, mi)
		}
	}

	if len(unresolvedFuncs) == 0 && len(unresolvedMethods) == 0 {
		return
	}

	// Build seen sets from concrete instances
	funcSeen := make(map[string]bool, len(*funcInstances))
	for _, fi := range *funcInstances {
		if !funcInstanceContainsTypeParam(fi) {
			funcSeen[monoFuncName(fi)] = true
		}
	}
	methodSeen := make(map[string]bool, len(*methodInstances))
	for _, mi := range *methodInstances {
		if !methodInstanceContainsTypeParam(mi) {
			methodSeen[monoMethodInstanceName(mi)] = true
		}
	}

	// Cross-resolve using index-based iteration to process new discoveries.
	funcIdx := 0
	methodIdx := 0
	for funcIdx < len(*funcInstances) || methodIdx < len(*methodInstances) {
		for funcIdx < len(*funcInstances) {
			fi := (*funcInstances)[funcIdx]
			funcIdx++
			if funcInstanceContainsTypeParam(fi) {
				continue
			}
			sig := fi.Func.Type().(*types.Signature)
			subst := types.BuildSubstMap(sig.TypeParams(), fi.TypeArgs)
			if len(subst) > 0 {
				resolveUnresolvedMethodInstances(unresolvedMethods, subst, methodInstances, methodSeen)
				resolveUnresolvedFuncInstances(unresolvedFuncs, subst, funcInstances, funcSeen)
			}
		}
		for methodIdx < len(*methodInstances) {
			mi := (*methodInstances)[methodIdx]
			methodIdx++
			if methodInstanceContainsTypeParam(mi) {
				continue
			}
			subst := buildMethodInstanceSubst(mi)
			if len(subst) > 0 {
				resolveUnresolvedFuncInstances(unresolvedFuncs, subst, funcInstances, funcSeen)
				resolveUnresolvedMethodInstances(unresolvedMethods, subst, methodInstances, methodSeen)
			}
		}
	}
}

// collectMonoFuncInstances deduplicates generic function instances by mangled name.
// Also resolves unresolved instances: when a generic function body calls another
// generic function using the outer function's type parameter (e.g., identity[T]
// inside wrap[T]), the inner FuncInstance has TypeParams that need to be resolved
// via the outer function's concrete instantiation.
//
// typeInstances provides substitution maps from concrete type instances — needed
// when a generic type's method body calls a generic function with the type's
// type parameter (e.g., Iterator[T].filter() calling some_func[T]()).
func collectMonoFuncInstances(info *sema.Info, typeInstances ...[]*types.Instance) []*sema.FuncInstance {
	seen := map[string]bool{}
	var result []*sema.FuncInstance
	var unresolved []*sema.FuncInstance
	for _, fi := range info.FuncInstances {
		if funcInstanceContainsTypeParam(fi) {
			unresolved = append(unresolved, fi)
			continue
		}
		key := monoFuncName(fi)
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, fi)
	}

	if len(unresolved) == 0 {
		return result
	}

	// Resolve using type instance substitution maps (from generic type methods).
	for _, instances := range typeInstances {
		for _, inst := range instances {
			switch origin := inst.Origin().(type) {
			case *types.Named:
				subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
				if len(subst) > 0 {
					mergeParentSubst(origin, subst)
					resolveUnresolvedFuncInstances(unresolved, subst, &result, seen)
				}
			case *types.Enum:
				subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
				if len(subst) > 0 {
					resolveUnresolvedFuncInstances(unresolved, subst, &result, seen)
				}
			}
		}
	}

	// Transitively resolve: each concrete FuncInstance provides a substitution map
	// that can resolve unresolved instances. New concrete instances may in turn
	// resolve further unresolved instances (e.g., a[T] → b[T] → c[T] chain).
	for i := 0; i < len(result); i++ {
		fi := result[i]
		sig := fi.Func.Type().(*types.Signature)
		subst := types.BuildSubstMap(sig.TypeParams(), fi.TypeArgs)
		if len(subst) > 0 {
			resolveUnresolvedFuncInstances(unresolved, subst, &result, seen)
		}
	}

	return result
}

// collectMonoMethodInstancesWithExtra collects mono method instances from modSemaInfo
// plus any extra instances from the caller (e.g. user file) that are instantiations
// of methods on types declared in modFile. This handles cross-module generic method
// calls like iter.map[int](...) in user code where Iterator[T].map is defined in std.
func collectMonoMethodInstancesWithExtra(modSemaInfo *sema.Info, modFile *ast.File, extra []*sema.MethodInstance, typeInstances []*types.Instance) []*sema.MethodInstance {
	// Build set of type names declared in modFile
	modTypeNames := make(map[string]bool)
	for _, decl := range modFile.Decls {
		if td, ok := decl.(*ast.TypeDecl); ok {
			modTypeNames[td.Name] = true
		}
	}

	// Start with the module's own instances (deduped + resolved)
	result := collectMonoMethodInstances(modSemaInfo, typeInstances)
	seen := make(map[string]bool, len(result))
	for _, mi := range result {
		seen[monoMethodInstanceName(mi)] = true
	}

	// Add extra instances whose owner type is declared in this module.
	// Skip unresolved instances (TypeParam in TypeArgs/OwnerInst) — these should
	// have been resolved by the originating module's collectMonoMethodInstances call.
	for _, mi := range extra {
		if !modTypeNames[mi.Owner.Obj().Name()] {
			continue
		}
		if methodInstanceContainsTypeParam(mi) {
			continue
		}
		name := monoMethodInstanceName(mi)
		if seen[name] {
			continue
		}
		seen[name] = true
		result = append(result, mi)
	}
	return result
}

// collectMonoFuncInstancesWithExtra collects mono func instances from modSemaInfo
// plus any extra instances from the caller (e.g. user file) that are instantiations
// of functions declared in modFile. This handles cross-module generic calls like
// sort[int](...) in user code where sort is defined in the std module.
func collectMonoFuncInstancesWithExtra(modSemaInfo *sema.Info, modFile *ast.File, extra []*sema.FuncInstance, typeInstances []*types.Instance) []*sema.FuncInstance {
	// Build set of function names declared in modFile
	modFuncNames := make(map[string]bool)
	for _, decl := range modFile.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok {
			modFuncNames[fd.Name] = true
		}
	}

	// Start with the module's own instances (deduped + resolved)
	result := collectMonoFuncInstances(modSemaInfo, typeInstances)
	seen := make(map[string]bool, len(result))
	for _, fi := range result {
		seen[monoFuncName(fi)] = true
	}

	// Add extra instances whose function is declared in this module.
	// Skip unresolved instances (TypeParam in TypeArgs) — these should have been
	// resolved by the originating context's collectMonoFuncInstances call.
	for _, fi := range extra {
		if !modFuncNames[fi.Func.Name()] {
			continue
		}
		if funcInstanceContainsTypeParam(fi) {
			continue
		}
		name := monoFuncName(fi)
		if seen[name] {
			continue
		}
		seen[name] = true
		result = append(result, fi)
	}
	return result
}

// computeMonoUserTypeLayout computes a TypeDeclLayout for a monomorphic user type instance.
// It substitutes all TypeParam fields with concrete types from the subst map.
func computeMonoUserTypeLayout(module *ir.Module, named *types.Named, name string, subst map[*types.TypeParam]types.Type, allLayouts map[*types.Named]*TypeDeclLayout, ptrSize int, enumLayouts map[*types.Enum]*TypeDeclLayout, monoEnumLayouts map[string]*TypeDeclLayout, monoLayouts map[string]*TypeDeclLayout) *TypeDeclLayout {
	// Type struct: empty {}
	typeStruct := irtypes.NewStruct()
	typeStruct.SetName("promise_" + name + "_t")
	module.NewTypeDef("promise_"+name+"_t", typeStruct)

	typePtr := irtypes.NewPointer(typeStruct)

	// Variant struct: { promise_T_t* _type }
	variantStruct := irtypes.NewStruct(typePtr)
	variantStruct.SetName("promise_" + name + "_m")
	module.NewTypeDef("promise_"+name+"_m", variantStruct)

	variantPtr := irtypes.NewPointer(variantStruct)

	// Instance struct: { promise_T_m* _variant, field1, field2, ... }
	instanceLLVMFields := []irtypes.Type{variantPtr}
	fieldLayouts := []FieldLayout{
		{Name: "_variant", CType: "promise_" + name + "_m*", LLVMType: variantPtr, IsInternal: true},
	}
	fieldIndex := map[string]int{}

	for _, f := range named.AllFields() {
		// Substitute TypeParams with concrete types
		fieldType := types.Substitute(f.Type(), subst)
		llvmFT := instanceFieldLLVMType(fieldType, allLayouts, ptrSize, enumLayouts, monoEnumLayouts, monoLayouts)
		cType := userFieldCType(fieldType, allLayouts)
		instanceLLVMFields = append(instanceLLVMFields, llvmFT)
		idx := len(fieldLayouts)
		fieldLayouts = append(fieldLayouts, FieldLayout{
			Name: f.Name(), CType: cType, LLVMType: llvmFT, IsInternal: false,
		})
		fieldIndex[f.Name()] = idx
	}

	instanceStruct := irtypes.NewStruct(instanceLLVMFields...)
	instanceStruct.SetName("promise_" + name + "_i")
	module.NewTypeDef("promise_"+name+"_i", instanceStruct)

	instancePtr := irtypes.NewPointer(instanceStruct)

	// Value struct: { i8* _vtable, promise_T_i* _instance }
	valueStruct := irtypes.NewStruct(irtypes.I8Ptr, instancePtr)
	valueStruct.SetName("promise_" + name + "_v")
	module.NewTypeDef("promise_"+name+"_v", valueStruct)

	return &TypeDeclLayout{
		PromiseName:        name,
		Kind:               LayoutUserType,
		InstanceFieldIndex: fieldIndex,
		InstancePtrType:    instancePtr,
		Type: &StructLayout{
			CName:    "promise_" + name + "_t",
			Suffix:   "_t",
			Fields:   []FieldLayout{},
			LLVMType: typeStruct,
		},
		Variant: &StructLayout{
			CName:  "promise_" + name + "_m",
			Suffix: "_m",
			Fields: []FieldLayout{
				{Name: "_type", CType: "promise_" + name + "_t*", LLVMType: typePtr, IsInternal: true},
			},
			LLVMType: variantStruct,
		},
		Instance: &StructLayout{
			CName:    "promise_" + name + "_i",
			Suffix:   "_i",
			Fields:   fieldLayouts,
			LLVMType: instanceStruct,
		},
		Value: &StructLayout{
			CName:  "promise_" + name + "_v",
			Suffix: "_v",
			Fields: []FieldLayout{
				{Name: "_vtable", CType: "void*", LLVMType: irtypes.I8Ptr, IsInternal: true},
				{Name: "_instance", CType: "promise_" + name + "_i*", LLVMType: instancePtr, IsInternal: true},
			},
			LLVMType: valueStruct,
		},
	}
}

// computeMonoValueTypeLayout computes a TypeDeclLayout for a monomorphic value type instance.
// Value types embed fields directly in the value struct: { i8* _vtable, field1, field2, ... }.
func computeMonoValueTypeLayout(module *ir.Module, named *types.Named, name string, subst map[*types.TypeParam]types.Type, allLayouts map[*types.Named]*TypeDeclLayout, ptrSize int, enumLayouts map[*types.Enum]*TypeDeclLayout, monoEnumLayouts map[string]*TypeDeclLayout, monoLayouts map[string]*TypeDeclLayout) *TypeDeclLayout {
	// Type struct: empty {}
	typeStruct := irtypes.NewStruct()
	typeStruct.SetName("promise_" + name + "_t")
	module.NewTypeDef("promise_"+name+"_t", typeStruct)

	typePtr := irtypes.NewPointer(typeStruct)

	// Variant struct: { promise_T_t* _type }
	variantStruct := irtypes.NewStruct(typePtr)
	variantStruct.SetName("promise_" + name + "_m")
	module.NewTypeDef("promise_"+name+"_m", variantStruct)

	variantPtr := irtypes.NewPointer(variantStruct)

	// Instance struct: { promise_T_m* _variant } — RTTI only, no user fields
	instanceStruct := irtypes.NewStruct(variantPtr)
	instanceStruct.SetName("promise_" + name + "_i")
	module.NewTypeDef("promise_"+name+"_i", instanceStruct)

	instancePtr := irtypes.NewPointer(instanceStruct)

	// Value struct: { i8* _vtable, field1, field2, ... }
	// RTTI is accessed via the compile-time-known global, not stored in the value struct.
	valueLLVMFields := []irtypes.Type{irtypes.I8Ptr}
	valueFieldLayouts := []FieldLayout{
		{Name: "_vtable", CType: "void*", LLVMType: irtypes.I8Ptr, IsInternal: true},
	}
	fieldIndex := map[string]int{}

	for _, f := range named.AllFields() {
		fieldType := types.Substitute(f.Type(), subst)
		llvmFT := instanceFieldLLVMType(fieldType, allLayouts, ptrSize, enumLayouts, monoEnumLayouts, monoLayouts)
		cType := userFieldCType(fieldType, allLayouts)
		idx := len(valueFieldLayouts)
		valueLLVMFields = append(valueLLVMFields, llvmFT)
		valueFieldLayouts = append(valueFieldLayouts, FieldLayout{
			Name: f.Name(), CType: cType, LLVMType: llvmFT, IsInternal: false,
		})
		fieldIndex[f.Name()] = idx
	}

	valueStruct := irtypes.NewStruct(valueLLVMFields...)
	valueStruct.SetName("promise_" + name + "_v")
	module.NewTypeDef("promise_"+name+"_v", valueStruct)

	return &TypeDeclLayout{
		PromiseName:     name,
		Kind:            LayoutValueType,
		IsValueType:     true,
		ValueFieldIndex: fieldIndex,
		InstancePtrType: instancePtr,
		Type: &StructLayout{
			CName:    "promise_" + name + "_t",
			Suffix:   "_t",
			Fields:   []FieldLayout{},
			LLVMType: typeStruct,
		},
		Variant: &StructLayout{
			CName:  "promise_" + name + "_m",
			Suffix: "_m",
			Fields: []FieldLayout{
				{Name: "_type", CType: "promise_" + name + "_t*", LLVMType: typePtr, IsInternal: true},
			},
			LLVMType: variantStruct,
		},
		Instance: &StructLayout{
			CName:  "promise_" + name + "_i",
			Suffix: "_i",
			Fields: []FieldLayout{
				{Name: "_variant", CType: "promise_" + name + "_m*", LLVMType: variantPtr, IsInternal: true},
			},
			LLVMType: instanceStruct,
		},
		Value: &StructLayout{
			CName:    "promise_" + name + "_v",
			Suffix:   "_v",
			Fields:   valueFieldLayouts,
			LLVMType: valueStruct,
		},
	}
}

// computeMonoEnumLayout computes a TypeDeclLayout for a monomorphic enum instance.
func computeMonoEnumLayout(module *ir.Module, enum *types.Enum, name string, subst map[*types.TypeParam]types.Type, ptrSize int, enumLayouts map[*types.Enum]*TypeDeclLayout, monoEnumLayouts map[string]*TypeDeclLayout) *TypeDeclLayout {
	variantTag := map[string]int{}
	variantDataTypes := map[string]*irtypes.StructType{}
	maxDataSize := 0

	for i, v := range enum.Variants() {
		variantTag[v.Name()] = i

		if v.NumFields() > 0 {
			var fieldTypes []irtypes.Type
			for _, f := range v.Fields() {
				ft := types.Substitute(f.Type(), subst)
				// Use llvmTypeForEnumFieldFromPromise so user-defined types
				// use {i8*, i8*} (value struct) not bare i8* (instance ptr).
				fieldTypes = append(fieldTypes, llvmTypeForEnumFieldFromPromise(ft, ptrSize, enumLayouts, monoEnumLayouts))
			}
			dataType := irtypes.NewStruct(fieldTypes...)
			variantDataTypes[v.Name()] = dataType

			// Compute data size from the struct type to account for alignment padding
			ds := llvmTypeSizeWithPtr(dataType, ptrSize)
			if ds > maxDataSize {
				maxDataSize = ds
			}
		}
	}

	var enumInternalType irtypes.Type
	if maxDataSize == 0 {
		enumInternalType = irtypes.I32
	} else {
		dataArray := irtypes.NewArray(uint64(maxDataSize), irtypes.I8)
		enumStruct := irtypes.NewStruct(irtypes.I32, dataArray)
		enumStruct.SetName("promise_" + name + "_enum")
		module.NewTypeDef("promise_"+name+"_enum", enumStruct)
		enumInternalType = enumStruct
	}

	// Type, Variant, Instance, Value structs — same pattern as computeEnumLayout
	typeStruct := irtypes.NewStruct()
	typeStruct.SetName("promise_" + name + "_t")
	module.NewTypeDef("promise_"+name+"_t", typeStruct)
	typePtr := irtypes.NewPointer(typeStruct)

	variantStruct := irtypes.NewStruct(typePtr)
	variantStruct.SetName("promise_" + name + "_m")
	module.NewTypeDef("promise_"+name+"_m", variantStruct)
	variantPtr := irtypes.NewPointer(variantStruct)

	instanceStruct := irtypes.NewStruct(variantPtr)
	instanceStruct.SetName("promise_" + name + "_i")
	module.NewTypeDef("promise_"+name+"_i", instanceStruct)
	instancePtr := irtypes.NewPointer(instanceStruct)

	valueFields := []irtypes.Type{irtypes.I8Ptr, instancePtr, irtypes.I32}
	valueFieldLayouts := []FieldLayout{
		{Name: "_vtable", CType: "void*", LLVMType: irtypes.I8Ptr, IsInternal: true},
		{Name: "_instance", CType: "promise_" + name + "_i*", LLVMType: instancePtr, IsInternal: true},
		{Name: "tag", CType: "int32_t", LLVMType: irtypes.I32, IsInternal: false},
	}
	if maxDataSize > 0 {
		dataArray := irtypes.NewArray(uint64(maxDataSize), irtypes.I8)
		valueFields = append(valueFields, dataArray)
		valueFieldLayouts = append(valueFieldLayouts, FieldLayout{
			Name: "data", CType: "uint8_t", LLVMType: dataArray, IsInternal: false,
		})
	}

	valueStruct := irtypes.NewStruct(valueFields...)
	valueStruct.SetName("promise_" + name + "_v")
	module.NewTypeDef("promise_"+name+"_v", valueStruct)

	return &TypeDeclLayout{
		PromiseName:        name,
		Kind:               LayoutEnum,
		EnumInternalType:   enumInternalType,
		VariantTag:         variantTag,
		VariantDataTypes:   variantDataTypes,
		MaxVariantDataSize: maxDataSize,
		Type: &StructLayout{
			CName: "promise_" + name + "_t", Suffix: "_t",
			Fields: []FieldLayout{}, LLVMType: typeStruct,
		},
		Variant: &StructLayout{
			CName: "promise_" + name + "_m", Suffix: "_m",
			Fields: []FieldLayout{
				{Name: "_type", CType: "promise_" + name + "_t*", LLVMType: typePtr, IsInternal: true},
			},
			LLVMType: variantStruct,
		},
		Instance: &StructLayout{
			CName: "promise_" + name + "_i", Suffix: "_i",
			Fields: []FieldLayout{
				{Name: "_variant", CType: "promise_" + name + "_m*", LLVMType: variantPtr, IsInternal: true},
			},
			LLVMType: instanceStruct,
		},
		Value: &StructLayout{
			CName: "promise_" + name + "_v", Suffix: "_v",
			Fields: valueFieldLayouts, LLVMType: valueStruct,
		},
	}
}

// computeMonoLayouts computes layouts for all monomorphic type instances.
// Processes enum instances first (with dependency resolution) so that type instances
// that have enum fields can look up the correct named enum struct types.
// User-type layouts share the topological walker with computeAllTypeLayouts so that
// generic value-type instances used as fields (e.g., Outer { Pt[int] inner; }) get
// laid out before their containing types. (T0565)
func (c *Compiler) computeMonoLayouts(instances []*types.Instance) {
	c.computeAllTypeLayouts(nil, instances)
}

// computeMonoEnumLayoutsOnly computes only the mono enum layouts for the given
// instances. Extracted so computeAllTypeLayouts can run it before the unified
// user/mono type layout pass.
func (c *Compiler) computeMonoEnumLayoutsOnly(instances []*types.Instance) {
	pendingEnums := make(map[string]*types.Instance)
	var enumNames []string
	for _, inst := range instances {
		origin, ok := inst.Origin().(*types.Enum)
		if !ok || len(origin.TypeParams()) == 0 {
			continue
		}
		name := monoName(inst)
		if _, exists := c.monoEnumLayouts[name]; exists {
			continue
		}
		pendingEnums[name] = inst
		enumNames = append(enumNames, name)
	}

	computedEnums := make(map[string]bool)
	var computeEnum func(name string)
	computeEnum = func(name string) {
		if computedEnums[name] {
			return
		}
		inst := pendingEnums[name]
		if inst == nil {
			return
		}
		origin := inst.Origin().(*types.Enum)
		subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
		// Ensure layouts for enum types referenced in variant fields are computed first
		for _, v := range origin.Variants() {
			for _, f := range v.Fields() {
				ft := types.Substitute(f.Type(), subst)
				if depInst, ok := ft.(*types.Instance); ok {
					if _, isEnum := depInst.Origin().(*types.Enum); isEnum {
						depName := monoName(depInst)
						if _, ok := pendingEnums[depName]; ok {
							computeEnum(depName)
						}
					}
				}
			}
		}
		c.monoEnumLayouts[name] = computeMonoEnumLayout(c.module, origin, name, subst, c.ptrSize(), c.enumLayouts, c.monoEnumLayouts)
		computedEnums[name] = true
	}
	for _, name := range enumNames {
		computeEnum(name)
	}
}

// declareMonoMethods declares LLVM functions for methods on monomorphic user type instances.
func (c *Compiler) declareMonoMethods(file *ast.File, instances []*types.Instance) {
	for _, inst := range instances {
		named, ok := inst.Origin().(*types.Named)
		if !ok {
			continue
		}
		// Skip structural types — their default methods are synthesized for
		// concrete implementors via synthesizeDefaultMethods.
		if named.IsStructural() {
			continue
		}
		name := monoName(inst)
		subst := types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
		mergeParentSubst(named, subst)

		// Find the TypeDecl AST node for this type
		td := c.findTypeDecl(file, named.Obj().Name())
		if td == nil {
			continue
		}
		// Verify the found decl matches the mono origin (avoid name collisions
		// with user-defined types sharing the same name as std types)
		if foundNamed := c.lookupNamedType(td.Name); foundNamed != nil && foundNamed != named {
			continue
		}

		for _, md := range td.Methods {
			if md.Body == nil {
				// Native drop methods need LLVM stubs for scope cleanup dispatch (T0088).
				// Other native methods (next, push, etc.) are handled inline at call sites.
				m2 := c.lookupAnyMethod(named, md.Name, md.IsGetter, md.IsSetter)
				if m2 == nil || !m2.IsNative() || md.Name != "drop" {
					continue
				}
			}
			if len(md.TypeParams) > 0 {
				continue // generic method — handled by mono method instances
			}
			m := c.lookupAnyMethod(named, md.Name, md.IsGetter, md.IsSetter)
			if m == nil || m.Sig() == nil {
				continue
			}

			mangledName := mangleMethodName(name, md.Name, md.IsSetter)
			if _, exists := c.funcs[mangledName]; exists {
				continue // already declared (e.g., same instance from main file)
			}

			var params []*ir.Param
			if m.Sig().Recv() != nil {
				params = append(params, ir.NewParam("this", irtypes.I8Ptr))
			}

			// Substitute param types
			c.typeSubst = subst
			for _, p := range m.Sig().Params() {
				params = append(params, ir.NewParam(p.Name(), c.resolveParamType(p)))
			}

			retType := irtypes.Type(irtypes.Void)
			if m.Sig().Result() != nil {
				retType = c.resolveType(m.Sig().Result())
			}
			c.typeSubst = nil

			if m.Sig().CanError() {
				retType = computeResultType(retType)
			}

			fn := c.module.NewFunc(mangledName, retType, params...)
			c.funcs[mangledName] = fn
			if c.compilingModule != "" {
				c.moduleOwnedFuncs[mangledName] = c.compilingModule
			}
		}
	}
}

// defineMonoMethods generates method bodies for monomorphic user type instances.
func (c *Compiler) defineMonoMethods(file *ast.File, instances []*types.Instance) {
	for _, inst := range instances {
		named, ok := inst.Origin().(*types.Named)
		if !ok {
			continue
		}
		// Skip structural types — their default methods are synthesized for
		// concrete implementors via synthesizeDefaultMethods.
		if named.IsStructural() {
			continue
		}
		name := monoName(inst)
		subst := types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
		mergeParentSubst(named, subst)

		td := c.findTypeDecl(file, named.Obj().Name())
		if td == nil {
			continue
		}
		// Verify the found decl matches the mono origin (avoid name collisions
		// with user-defined types sharing the same name as std types)
		if foundNamed := c.lookupNamedType(td.Name); foundNamed != nil && foundNamed != named {
			continue
		}

		for _, md := range td.Methods {
			isNativeDrop := false
			if md.Body == nil {
				// Native drop methods get synthesized bodies (T0088).
				m2 := c.lookupAnyMethod(named, md.Name, md.IsGetter, md.IsSetter)
				if m2 != nil && m2.IsNative() && md.Name == "drop" {
					isNativeDrop = true
				} else {
					continue
				}
			}
			if len(md.TypeParams) > 0 {
				continue // generic method — handled by mono method instances
			}
			m := c.lookupAnyMethod(named, md.Name, md.IsGetter, md.IsSetter)
			if m == nil || m.Sig() == nil {
				continue
			}

			mangledName := mangleMethodName(name, md.Name, md.IsSetter)
			fn, ok := c.funcs[mangledName]
			if !ok {
				continue
			}
			// Tag as instance-owned (before body check, so SplitInstanceIRs can find it)
			c.instanceOwnedFuncs[mangledName] = name
			if len(fn.Blocks) > 0 {
				continue // already defined (e.g., from main file mono pass)
			}
			// Skip body generation for cached instances
			if c.cachedInstances[name] {
				continue
			}

			// Native drop: synthesize body that frees closure env + instance (T0088).
			// Arc[T] has its own drop logic (T0155) — skip here; body is generated
			// lazily by getOrCreateArcDrop when the drop function is first needed.
			if isNativeDrop {
				if named == types.TypArc || named == types.TypWeak || named == types.TypMutex || named == types.TypMutexGuard {
					continue
				}
				c.defineFnIterDrop(fn, inst)
				continue
			}

			c.typeSubst = subst
			c.monoCtx = &monoContext{inst: inst, origin: named, name: name}
			func() {
				defer func() { c.typeSubst = nil; c.monoCtx = nil }()
				if genInfo := c.info.GeneratorFuncs[md]; genInfo != nil {
					c.defineGeneratorMethod(md, m, fn, genInfo.ElemType, named)
				} else {
					c.defineMethodFunc(md, m, fn, named)
				}
			}()
		}
	}
}

// defineFnIterDrop synthesizes the body for _FnIter[T].drop (T0088/T0128).
// Delegates to __promise_iter_cleanup which handles the full parent chain,
// closure env cleanup, and instance deallocation.
//
//	define void @_FnIter__int.drop(i8* %this) {
//	    call void @__promise_iter_cleanup(i8* %this)
//	    ret void
//	}
func (c *Compiler) defineFnIterDrop(fn *ir.Func, inst *types.Instance) {
	entry := fn.NewBlock(".entry")
	entry.NewCall(c.iterCleanup, fn.Params[0])
	entry.NewRet(nil)
}

// monoInstNeedsSynthDrop returns true if a mono instance needs a synthesized drop
// that was NOT detected at sema time. This handles B0202: generic types where ALL
// fields are TypeParam — sema's fieldTypeHasDrop returns false for TypeParam, so
// NeedsSynthDrop is never set. At mono time we can check the concrete substituted
// types to determine if drop is needed.
func monoInstNeedsSynthDrop(inst *types.Instance) bool {
	named, ok := inst.Origin().(*types.Named)
	if !ok {
		return false
	}
	// Already handled by sema-level NeedsSynthDrop or explicit drop
	if named.NeedsSynthDrop() || named.HasDrop() || named.IsCopy() || named.IsValueType() || named.IsStructural() {
		return false
	}
	// Check if any field that contains a TypeParam resolves to a droppable type
	// after substitution. Sema's fieldTypeHasDrop returns false for TypeParam,
	// so any field whose type tree includes a TypeParam needs re-checking here.
	// B0209 covered the direct-TypeParam and Optional[TypeParam] cases; T0420
	// extended to tuples whose elements substitute to droppables; T0389 widens
	// the gate to fields that *contain* a TypeParam anywhere (e.g. (T, int)).
	subst := types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
	for _, f := range named.AllFields() {
		if !types.ContainsTypeParam(f.Type()) {
			continue // sema already saw the concrete type
		}
		ft := types.Substitute(f.Type(), subst)
		if opt, isOpt := ft.(*types.Optional); isOpt {
			ft = opt.Elem()
		}
		if monoTypeHasDroppable(ft) {
			return true
		}
	}
	return false
}

// monoTypeHasDroppable returns true if the given concrete type needs cleanup at
// drop time. Handles primitive named types, tuples (recurse), unwrapped
// optional types, and enums with drop. Used by monoInstNeedsSynthDrop and
// monoEnumInstNeedsSynthDrop and is independent of Compiler state so it can
// run during the declare phase.
func monoTypeHasDroppable(typ types.Type) bool {
	if tup, ok := typ.(*types.Tuple); ok {
		for _, e := range tup.Elems() {
			if monoTypeHasDroppable(e) {
				return true
			}
		}
		return false
	}
	if arr, ok := typ.(*types.Array); ok {
		return monoTypeHasDroppable(arr.Elem())
	}
	if opt, ok := typ.(*types.Optional); ok {
		return monoTypeHasDroppable(opt.Elem())
	}
	if fNamed := extractNamed(typ); fNamed != nil {
		if fNamed == types.TypString || fNamed == types.TypVector || fNamed == types.TypChannel ||
			fNamed == types.TypMutex || fNamed == types.TypMutexGuard {
			return true
		}
		if fNamed.HasDrop() {
			return true
		}
		if !fNamed.IsValueType() && !fNamed.IsCopy() && !isPrimitiveScalar(fNamed) && !fNamed.IsStructural() {
			return true
		}
	}
	if fEnum := extractEnum(typ); fEnum != nil {
		if fEnum.HasDrop() || fEnum.NeedsSynthDrop() {
			return true
		}
		// T0552: For generic enum Instances, recurse into substituted variant
		// fields. Sema's fieldTypeHasDrop returns false for TypeParam variant
		// fields, so the origin enum reports HasDrop=NeedsSynthDrop=false even
		// when the concrete instantiation carries a droppable payload.
		if inst, ok := typ.(*types.Instance); ok {
			if monoEnumInstNeedsSynthDrop(inst) {
				return true
			}
		}
	}
	return false
}

// monoEnumInstNeedsSynthDrop returns true if a mono enum instance needs a synthesized
// drop that was NOT detected at sema time. Analogous to monoInstNeedsSynthDrop for
// Named types. Handles generic enums like Slot[K, V] where variant fields are TypeParams
// — sema's fieldTypeHasDrop returns false for TypeParam, so NeedsSynthDrop is never set.
// At mono time we check if concrete substituted variant field types need drop. B0212.
// T0389: also recurses into tuple variant fields via monoTypeHasDroppable.
func monoEnumInstNeedsSynthDrop(inst *types.Instance) bool {
	enum, ok := inst.Origin().(*types.Enum)
	if !ok {
		return false
	}
	// Already handled by sema-level detection
	if enum.NeedsSynthDrop() || enum.HasDrop() {
		return false
	}
	// Check if any variant field that contains a TypeParam resolves to a
	// droppable type after substitution. T0389: matches monoInstNeedsSynthDrop —
	// any field whose type tree includes a TypeParam (direct, in Optional, or
	// inside a Tuple) needs re-checking, because sema's fieldTypeHasDrop returns
	// false for TypeParam.
	subst := types.BuildSubstMap(enum.TypeParams(), inst.TypeArgs())
	for _, v := range enum.Variants() {
		for _, f := range v.Fields() {
			if !types.ContainsTypeParam(f.Type()) {
				continue // sema already saw the concrete type
			}
			ft := types.Substitute(f.Type(), subst)
			if opt, isOpt := ft.(*types.Optional); isOpt {
				ft = opt.Elem()
			}
			if monoTypeHasDroppable(ft) {
				return true
			}
		}
	}
	return false
}

// declareSynthesizedMonoDrops declares drop function stubs for monomorphized
// instances of generic types that need a compiler-synthesized drop (B0158/B0202).
func (c *Compiler) declareSynthesizedMonoDrops(file *ast.File, instances []*types.Instance) {
	for _, inst := range instances {
		named, ok := inst.Origin().(*types.Named)
		if !ok || (!named.NeedsSynthDrop() && !monoInstNeedsSynthDrop(inst)) {
			continue
		}
		if named.IsStructural() {
			continue
		}
		name := monoName(inst)
		mangledName := mangleMethodName(name, "drop", false)
		if _, exists := c.funcs[mangledName]; exists {
			continue
		}
		fn := c.module.NewFunc(mangledName, irtypes.Void,
			ir.NewParam("this", irtypes.I8Ptr))
		c.funcs[mangledName] = fn
		if c.compilingModule != "" {
			c.moduleOwnedFuncs[mangledName] = c.compilingModule
		}
	}
}

// defineSynthesizedMonoDrops generates bodies for monomorphized synthesized drops (B0158/B0202).
func (c *Compiler) defineSynthesizedMonoDrops(file *ast.File, instances []*types.Instance) {
	for _, inst := range instances {
		named, ok := inst.Origin().(*types.Named)
		if !ok || (!named.NeedsSynthDrop() && !monoInstNeedsSynthDrop(inst)) {
			continue
		}
		if named.IsStructural() {
			continue
		}
		name := monoName(inst)
		mangledName := mangleMethodName(name, "drop", false)
		fn, ok := c.funcs[mangledName]
		if !ok || len(fn.Blocks) > 0 {
			continue
		}
		// Tag as instance-owned for SplitInstanceIRs
		c.instanceOwnedFuncs[mangledName] = name
		// Skip body generation for cached instances
		if c.cachedInstances[name] {
			continue
		}

		subst := types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
		mergeParentSubst(named, subst)

		c.typeSubst = subst
		c.monoCtx = &monoContext{inst: inst, origin: named, name: name}
		c.defineSynthesizedDropBody(fn, named)
		c.typeSubst = nil
		c.monoCtx = nil
	}
}

// findImmediateDropParent returns the immediate parent of `named` whose
// LookupMethod("drop") is non-nil — i.e., the parent that supplies the drop
// (whether own or inherited from a grandparent). For multi-level chains
// (`C is B is A` with drop on A), this returns B for C and A for B, so the
// per-instance synth drops cascade correctly. Skips structural parents.
func findImmediateDropParent(named *types.Named) *types.Named {
	for _, pr := range named.Parents() {
		if pr.Named.IsStructural() {
			continue
		}
		if pr.Named.LookupMethod("drop") != nil {
			return pr.Named
		}
	}
	return nil
}

// monoNeedsInheritedDropSynth returns true if a mono instance inherits its drop
// from a parent (rather than defining its own) and therefore needs a synthesized
// per-instance drop that drops the child's own fields and tail-calls the parent's
// mono drop. T0468.
func monoNeedsInheritedDropSynth(inst *types.Instance) bool {
	named, ok := inst.Origin().(*types.Named)
	if !ok {
		return false
	}
	if named.IsStructural() || named.IsCopy() || named.IsValueType() {
		return false
	}
	if !named.HasDrop() {
		return false
	}
	if hasOwnMethod(named, "drop") {
		return false // child has its own drop — emitFieldDrops in that body handles fields
	}
	// Synth-drop paths (B0158/B0202) already handle AllFields correctly.
	if named.NeedsSynthDrop() || monoInstNeedsSynthDrop(inst) {
		return false
	}
	dropMethod := named.LookupMethod("drop")
	if dropMethod == nil || dropMethod.IsNative() {
		return false
	}
	return true
}

// declareMonoInheritedDrops declares a per-instance drop stub for mono instances
// whose drop is inherited from a generic parent (T0468). Without this synthesis,
// emitFieldDrops cannot find a function named e.g. `_Box[int].drop` and silently
// skips the call, leaking the child's own heap fields. The body (defined in
// defineMonoInheritedDrops) drops the child's own fields and tail-calls the
// parent's mono drop, which runs the parent body and drops the parent's fields.
func (c *Compiler) declareMonoInheritedDrops(instances []*types.Instance) {
	for _, inst := range instances {
		if !monoNeedsInheritedDropSynth(inst) {
			continue
		}
		name := monoName(inst)
		mangledName := mangleMethodName(name, "drop", false)
		if _, exists := c.funcs[mangledName]; exists {
			continue
		}
		fn := c.module.NewFunc(mangledName, irtypes.Void,
			ir.NewParam("this", irtypes.I8Ptr))
		c.funcs[mangledName] = fn
		if c.compilingModule != "" {
			c.moduleOwnedFuncs[mangledName] = c.compilingModule
		}
	}
}

// defineMonoInheritedDrops generates bodies for synthesized inherited-drop
// stubs. The body drops the child's own fields (in reverse declaration order)
// and then tail-calls the parent type's mono drop, which runs the parent's
// user-written drop body and emits its own emitFieldDrops over the parent's
// fields. Order matches Rust's "child cleanup first, then super.drop". T0468.
func (c *Compiler) defineMonoInheritedDrops(instances []*types.Instance) {
	for _, inst := range instances {
		if !monoNeedsInheritedDropSynth(inst) {
			continue
		}
		named := inst.Origin().(*types.Named)
		name := monoName(inst)
		mangledName := mangleMethodName(name, "drop", false)
		fn, ok := c.funcs[mangledName]
		if !ok || len(fn.Blocks) > 0 {
			continue
		}
		// Tag as instance-owned for SplitInstanceIRs.
		c.instanceOwnedFuncs[mangledName] = name
		// Skip body generation for cached instances.
		if c.cachedInstances[name] {
			continue
		}

		// Resolve the immediate parent that owns drop (own or inherited). For
		// `C is B is A` with drop on A, C's drop must call B's drop (which we
		// also synthesize), not A's drop directly — otherwise B's own fields
		// leak. The synthesis cascades naturally because B's synth drops B's
		// fields then calls A's drop.
		parentNamed := findImmediateDropParent(named)
		if parentNamed == nil {
			entry := fn.NewBlock(".entry")
			entry.NewRet(nil)
			continue
		}
		parentMonoName := c.resolveMonoParentName(named, inst, parentNamed.Obj().Name())
		parentMangled := mangleMethodName(parentMonoName, "drop", false)
		parentFn := c.funcs[parentMangled]
		if parentFn == nil {
			// Parent's drop function not yet declared — no-op stub. Should not
			// happen in practice: declareMonoMethods (for parents with own drop)
			// and declareMonoInheritedDrops (for parents that also inherit) both
			// run before define.
			entry := fn.NewBlock(".entry")
			entry.NewRet(nil)
			continue
		}

		entry := fn.NewBlock(".entry")
		savedBlock := c.block
		savedFn := c.fn
		savedEntry := c.entryBlock
		savedPanicExit := c.panicExitBlock
		savedCoroReturn := c.coroutineReturnBlock
		savedLocals := c.locals
		savedTypeSubst := c.typeSubst
		savedMonoCtx := c.monoCtx

		c.block = entry
		c.fn = fn
		c.entryBlock = entry
		c.panicExitBlock = nil
		c.coroutineReturnBlock = nil
		c.locals = make(map[string]*ir.InstAlloca)

		thisAlloca := entry.NewAlloca(irtypes.I8Ptr)
		entry.NewStore(fn.Params[0], thisAlloca)
		c.locals["this"] = thisAlloca

		subst := types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
		mergeParentSubst(named, subst)
		c.typeSubst = subst
		c.monoCtx = &monoContext{inst: inst, origin: named, name: name}

		// Drop the child's own fields only (parent fields are dropped by parent's drop).
		c.emitFieldDropsFor(named, named.Fields())

		// Tail-call the parent's mono drop, which runs parent body + parent fields.
		c.block.NewCall(parentFn, fn.Params[0])
		c.block.NewRet(nil)

		c.block = savedBlock
		c.fn = savedFn
		c.entryBlock = savedEntry
		c.panicExitBlock = savedPanicExit
		c.coroutineReturnBlock = savedCoroReturn
		c.locals = savedLocals
		c.typeSubst = savedTypeSubst
		c.monoCtx = savedMonoCtx
	}
}

// declareSynthesizedMonoEnumDrops declares drop stubs for monomorphized enum instances
// that need a compiler-synthesized drop (T0102/B0212).
func (c *Compiler) declareSynthesizedMonoEnumDrops(file *ast.File, instances []*types.Instance) {
	for _, inst := range instances {
		enum, ok := inst.Origin().(*types.Enum)
		if !ok || (!enum.NeedsSynthDrop() && !monoEnumInstNeedsSynthDrop(inst)) {
			continue
		}
		name := monoName(inst)
		mangledName := mangleMethodName(name, "drop", false)
		if _, exists := c.funcs[mangledName]; exists {
			continue
		}
		fn := c.module.NewFunc(mangledName, irtypes.Void,
			ir.NewParam("this", irtypes.I8Ptr))
		c.funcs[mangledName] = fn
		if c.compilingModule != "" {
			c.moduleOwnedFuncs[mangledName] = c.compilingModule
		}
	}
}

// defineSynthesizedMonoEnumDrops generates bodies for monomorphized enum drops (T0102/B0212).
func (c *Compiler) defineSynthesizedMonoEnumDrops(file *ast.File, instances []*types.Instance) {
	for _, inst := range instances {
		enum, ok := inst.Origin().(*types.Enum)
		if !ok || (!enum.NeedsSynthDrop() && !monoEnumInstNeedsSynthDrop(inst)) {
			continue
		}
		name := monoName(inst)
		mangledName := mangleMethodName(name, "drop", false)
		fn, ok := c.funcs[mangledName]
		if !ok || len(fn.Blocks) > 0 {
			continue
		}
		// Tag as instance-owned for SplitInstanceIRs
		c.instanceOwnedFuncs[mangledName] = name
		// Skip body generation for cached instances
		if c.cachedInstances[name] {
			continue
		}

		layout := c.monoEnumLayouts[name]
		if layout == nil {
			continue
		}

		subst := types.BuildSubstMap(enum.TypeParams(), inst.TypeArgs())
		c.typeSubst = subst
		c.monoCtx = &monoContext{inst: inst, origin: enum, name: name}
		c.defineSynthesizedEnumDropBody(fn, enum, layout)
		c.typeSubst = nil
		c.monoCtx = nil
	}
}

// declareMonoSynthesizedDefaults declares stubs for default methods from structural
// parents that need to be synthesized for mono instances of concrete types.
// E.g., _FnIter[int] inherits filter/take/skip from Iterator[T] — these become
// _FnIter__int.filter, _FnIter__int.take, etc. Must run BEFORE vtable emission.
func (c *Compiler) declareMonoSynthesizedDefaults(file *ast.File, instances []*types.Instance) {
	for _, inst := range instances {
		named, ok := inst.Origin().(*types.Named)
		if !ok || named.IsStructural() {
			continue
		}
		name := monoName(inst)
		subst := types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
		mergeParentSubst(named, subst)

		for _, pr := range named.Parents() {
			if pr.Named.IsStructural() {
				c.declareStructuralDefaultStubs(file, name, named, pr.Named, subst)
			}
		}
	}
}

// declareStructuralDefaultStubs declares function stubs for default methods from
// a structural interface, using mono-qualified names for the concrete type.
func (c *Compiler) declareStructuralDefaultStubs(file *ast.File, mName string, concrete, iface *types.Named, subst map[*types.TypeParam]types.Type) {
	ifaceTD := c.findTypeDecl(file, iface.Obj().Name())
	if ifaceTD == nil {
		return
	}
	for _, md := range ifaceTD.Methods {
		if md.Body == nil {
			continue
		}
		m := c.lookupAnyMethod(iface, md.Name, md.IsGetter, md.IsSetter)
		if m == nil || m.IsAbstract() {
			continue
		}
		if hasOwnMethod(concrete, md.Name) {
			continue
		}
		if len(md.TypeParams) > 0 {
			continue // generic methods are not virtual
		}
		mangledName := mangleMethodName(mName, md.Name, md.IsSetter)
		if _, exists := c.funcs[mangledName]; exists {
			continue
		}

		sig := m.Sig()
		var params []*ir.Param
		if sig.Recv() != nil {
			params = append(params, ir.NewParam("this", irtypes.I8Ptr))
		}
		c.typeSubst = subst
		for _, p := range sig.Params() {
			params = append(params, ir.NewParam(p.Name(), c.resolveParamType(p)))
		}
		retType := irtypes.Type(irtypes.Void)
		if sig.Result() != nil {
			retType = c.resolveType(sig.Result())
		}
		c.typeSubst = nil
		if sig.CanError() {
			retType = computeResultType(retType)
		}

		fn := c.module.NewFunc(mangledName, retType, params...)
		c.funcs[mangledName] = fn
	}

	// Recurse into parent interfaces
	for _, pr := range iface.Parents() {
		if pr.Named.IsStructural() {
			c.declareStructuralDefaultStubs(file, mName, concrete, pr.Named, subst)
		}
	}
}

// defineMonoSynthesizedDefaults generates bodies for synthesized default methods
// on mono instances of concrete types with structural parents.
func (c *Compiler) defineMonoSynthesizedDefaults(file *ast.File, instances []*types.Instance) {
	for _, inst := range instances {
		named, ok := inst.Origin().(*types.Named)
		if !ok || named.IsStructural() {
			continue
		}
		name := monoName(inst)
		subst := types.BuildSubstMap(named.TypeParams(), inst.TypeArgs())
		mergeParentSubst(named, subst)

		for _, pr := range named.Parents() {
			if pr.Named.IsStructural() {
				c.defineStructuralDefaultBodies(file, name, named, pr.Named, subst, inst)
			}
		}
	}
}

// defineStructuralDefaultBodies generates method bodies for already-declared
// synthesized default method stubs with mono-qualified names.
func (c *Compiler) defineStructuralDefaultBodies(file *ast.File, mName string, concrete, iface *types.Named, subst map[*types.TypeParam]types.Type, inst *types.Instance) {
	ifaceTD := c.findTypeDecl(file, iface.Obj().Name())
	if ifaceTD == nil {
		return
	}
	for _, md := range ifaceTD.Methods {
		if md.Body == nil {
			continue
		}
		m := c.lookupAnyMethod(iface, md.Name, md.IsGetter, md.IsSetter)
		if m == nil || m.IsAbstract() {
			continue
		}
		if hasOwnMethod(concrete, md.Name) {
			continue
		}
		if len(md.TypeParams) > 0 {
			continue
		}
		mangledName := mangleMethodName(mName, md.Name, md.IsSetter)
		fn, ok := c.funcs[mangledName]
		if !ok {
			continue
		}
		// Tag as instance-owned (before body check)
		c.instanceOwnedFuncs[mangledName] = mName
		if len(fn.Blocks) > 0 {
			continue
		}
		// Skip body generation for cached instances
		if c.cachedInstances[mName] {
			continue
		}
		// Spiral instances have no child layouts — emit unreachable so the body
		// never requests yet-deeper instances (e.g. _FnIter[(int,(int,u8))].enumerate).
		if c.spiralInstances[mName] {
			b := fn.NewBlock("")
			b.NewUnreachable()
			continue
		}

		saved := c.saveState()
		c.selfSubst = &selfSubstInfo{iface: iface, concrete: concrete}
		c.typeSubst = subst
		c.monoCtx = &monoContext{inst: inst, origin: concrete, name: mName}
		c.defineMethodFunc(md, m, fn, concrete)
		c.restoreState(saved)
	}

	// Recurse into parent interfaces
	for _, pr := range iface.Parents() {
		if pr.Named.IsStructural() {
			c.defineStructuralDefaultBodies(file, mName, concrete, pr.Named, subst, inst)
		}
	}
}

// declareMonoFuncs declares LLVM functions for monomorphic generic function instances.
func (c *Compiler) declareMonoFuncs(file *ast.File, funcInsts []*sema.FuncInstance) {
	for _, fi := range funcInsts {
		name := monoFuncName(fi)
		if _, exists := c.funcs[name]; exists {
			continue // already declared (e.g., same instance from main file)
		}
		fd := c.findFuncDecl(file, fi.Func.Name())
		if fd == nil || fd.Body == nil {
			continue
		}

		sig := fi.Func.Type().(*types.Signature)
		subst := types.BuildSubstMap(sig.TypeParams(), fi.TypeArgs)

		c.typeSubst = subst
		retType := irtypes.Type(irtypes.Void)
		if sig.Result() != nil {
			retType = c.resolveType(sig.Result())
		}
		if sig.CanError() {
			retType = computeResultType(retType)
		}

		var params []*ir.Param
		for _, p := range sig.Params() {
			params = append(params, ir.NewParam(p.Name(), c.resolveParamType(p)))
		}
		c.typeSubst = nil

		fn := c.module.NewFunc(name, retType, params...)
		c.funcs[name] = fn
		if c.compilingModule != "" {
			c.moduleOwnedFuncs[name] = c.compilingModule
			// Also mark as instance-owned so SplitModuleIRs puts it in its own .bc
			// instead of the module IR. This keeps the module IR stable across
			// compilations (different callers trigger different mono functions).
			c.instanceOwnedFuncs[name] = name
		}
	}
}

// defineMonoFuncs generates function bodies for monomorphic generic function instances.
func (c *Compiler) defineMonoFuncs(file *ast.File, funcInsts []*sema.FuncInstance) {
	for _, fi := range funcInsts {
		name := monoFuncName(fi)
		fd := c.findFuncDecl(file, fi.Func.Name())
		if fd == nil || fd.Body == nil {
			continue
		}

		fn, ok := c.funcs[name]
		if !ok || len(fn.Blocks) > 0 {
			continue // skip if not declared or already defined (e.g., from module phase)
		}

		sig := fi.Func.Type().(*types.Signature)
		subst := types.BuildSubstMap(sig.TypeParams(), fi.TypeArgs)

		c.typeSubst = subst
		func() {
			defer func() { c.typeSubst = nil }()
			if genInfo := c.info.GeneratorFuncs[fd]; genInfo != nil {
				c.defineGeneratorFunc(fd, fn, genInfo.ElemType)
			} else {
				c.defineFunc(fd, fn)
			}
		}()
	}
}

// findTypeDecl finds a TypeDecl AST node by name.
// collectMonoInstancesWithExtra is like collectMonoInstances but seeds the
// transitive expansion with both the module's own recorded instances and any
// extra instances from the caller (e.g. user-file mono instances of module
// types like Map[string,int]). Only extra instances whose origin type is
// declared in modFile are included. The unresolved-instance expansion uses
// the module's own sema info so that method-body type references (e.g.
// _FnIter[T] inside Vector[T].iter()) are resolved correctly.
func collectMonoInstancesWithExtra(modInfo *sema.ModuleInfo, modFile *ast.File, extra []*types.Instance, spiralInstances map[string]bool) []*types.Instance {
	// Build seen set for type names declared in modFile for O(1) membership test.
	modTypeNames := make(map[string]bool)
	for _, decl := range modFile.Decls {
		if td, ok := decl.(*ast.TypeDecl); ok {
			modTypeNames[td.Name] = true
		}
	}

	seen := map[string]bool{}
	var result []*types.Instance

	// Seed with module's own recorded instances.
	for _, inst := range modInfo.SemaInfo.Instances {
		key := monoName(inst)
		if !seen[key] {
			seen[key] = true
			result = append(result, inst)
		}
	}

	// Seed with extra instances that belong to types declared in modFile.
	for _, inst := range extra {
		named, ok := inst.Origin().(*types.Named)
		if !ok {
			continue
		}
		if !modTypeNames[named.Obj().Name()] {
			continue
		}
		key := monoName(inst)
		if !seen[key] {
			seen[key] = true
			result = append(result, inst)
		}
	}

	// Unresolved instances from module's method bodies (e.g. _FnIter[T] inside
	// Vector[T].iter()). These will be resolved transitively for each concrete inst.
	unresolvedInsts := collectUnresolvedInstances(modInfo.SemaInfo)
	wrapsCache := make(map[types.Type]bool)

	// Transitively expand (same logic as collectMonoInstances).
	for i := 0; i < len(result); i++ {
		inst := result[i]
		instKey := monoName(inst)
		if spiralInstances[instKey] {
			continue // spiral instances: skip transitive expansion
		}
		switch origin := inst.Origin().(type) {
		case *types.Named:
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			for _, f := range origin.AllFields() {
				ft := types.Substitute(f.Type(), subst)
				discoverInstances(ft, &result, seen)
			}
			for _, pr := range origin.Parents() {
				if len(pr.TypeArgs) > 0 {
					resolvedArgs := make([]types.Type, len(pr.TypeArgs))
					for j, ta := range pr.TypeArgs {
						resolvedArgs[j] = types.Substitute(ta, subst)
					}
					parentInst := types.NewInstance(pr.Named, resolvedArgs)
					if !types.ContainsTypeParam(parentInst) {
						discoverInstances(parentInst, &result, seen)
					}
				}
			}
			if len(subst) > 0 {
				resolveUnresolvedInstances(unresolvedInsts, subst, &result, seen, spiralInstances, wrapsCache)
			}
		case *types.Enum:
			subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
			for _, v := range origin.Variants() {
				for _, f := range v.Fields() {
					ft := types.Substitute(f.Type(), subst)
					discoverInstances(ft, &result, seen)
				}
			}
			if len(subst) > 0 {
				resolveUnresolvedInstances(unresolvedInsts, subst, &result, seen, spiralInstances, wrapsCache)
			}
		}
	}

	sort.Slice(result, func(i, j int) bool { return monoName(result[i]) < monoName(result[j]) })
	return result
}

func (c *Compiler) findTypeDecl(file *ast.File, name string) *ast.TypeDecl {
	for _, decl := range file.Decls {
		if td, ok := decl.(*ast.TypeDecl); ok && td.Name == name {
			return td
		}
	}
	return nil
}

// findFuncDecl finds a FuncDecl AST node by name.
func (c *Compiler) findFuncDecl(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name == name {
			return fd
		}
	}
	return nil
}

// findEnumDecl finds an EnumDecl AST node by name.
func (c *Compiler) findEnumDecl(file *ast.File, name string) *ast.EnumDecl {
	for _, decl := range file.Decls {
		if ed, ok := decl.(*ast.EnumDecl); ok && ed.Name == name {
			return ed
		}
	}
	return nil
}

// declareMonoEnumMethods declares LLVM functions for methods on monomorphic enum instances.
func (c *Compiler) declareMonoEnumMethods(file *ast.File, instances []*types.Instance) {
	for _, inst := range instances {
		enum, ok := inst.Origin().(*types.Enum)
		if !ok || len(enum.TypeParams()) == 0 {
			continue
		}
		name := monoName(inst)
		subst := types.BuildSubstMap(enum.TypeParams(), inst.TypeArgs())

		ed := c.findEnumDecl(file, enum.Obj().Name())
		if ed == nil {
			continue
		}
		// Verify the found decl matches the mono origin
		if foundEnum := c.lookupEnumType(ed.Name); foundEnum != nil && foundEnum != enum {
			continue
		}

		for _, md := range ed.Methods {
			if md.Body == nil {
				continue
			}
			if len(md.TypeParams) > 0 {
				continue // generic method — handled by mono method instances
			}
			m := c.lookupEnumMethod(enum, md)
			if m == nil || m.Sig() == nil {
				continue
			}

			mangledName := mangleMethodName(name, md.Name, md.IsSetter)
			if _, exists := c.funcs[mangledName]; exists {
				continue
			}

			var params []*ir.Param
			if m.Sig().Recv() != nil {
				params = append(params, ir.NewParam("this", irtypes.I8Ptr))
			}

			c.typeSubst = subst
			for _, p := range m.Sig().Params() {
				params = append(params, ir.NewParam(p.Name(), c.resolveParamType(p)))
			}

			retType := irtypes.Type(irtypes.Void)
			genInfo := c.info.GeneratorFuncs[md]
			if genInfo != nil {
				if genInfo.CanError {
					retType = computeResultType(failableGeneratorValueType())
				} else {
					retType = generatorValueType()
				}
			} else if m.Sig().Result() != nil {
				retType = c.resolveType(m.Sig().Result())
			}
			c.typeSubst = nil

			if m.Sig().CanError() && genInfo == nil {
				retType = computeResultType(retType)
			}

			fn := c.module.NewFunc(mangledName, retType, params...)
			c.funcs[mangledName] = fn
			if c.compilingModule != "" {
				c.moduleOwnedFuncs[mangledName] = c.compilingModule
			}
		}
	}
}

// defineMonoEnumMethods generates method bodies for monomorphic enum instances.
func (c *Compiler) defineMonoEnumMethods(file *ast.File, instances []*types.Instance) {
	for _, inst := range instances {
		enum, ok := inst.Origin().(*types.Enum)
		if !ok || len(enum.TypeParams()) == 0 {
			continue
		}
		name := monoName(inst)
		subst := types.BuildSubstMap(enum.TypeParams(), inst.TypeArgs())

		ed := c.findEnumDecl(file, enum.Obj().Name())
		if ed == nil {
			continue
		}
		// Verify the found decl matches the mono origin
		if foundEnum := c.lookupEnumType(ed.Name); foundEnum != nil && foundEnum != enum {
			continue
		}

		for _, md := range ed.Methods {
			if md.Body == nil {
				continue
			}
			if len(md.TypeParams) > 0 {
				continue // generic method — handled by mono method instances
			}
			m := c.lookupEnumMethod(enum, md)
			if m == nil || m.Sig() == nil {
				continue
			}

			mangledName := mangleMethodName(name, md.Name, md.IsSetter)
			fn, ok := c.funcs[mangledName]
			if !ok {
				continue
			}
			// Tag as instance-owned (before body check, so SplitInstanceIRs can find it)
			c.instanceOwnedFuncs[mangledName] = name
			if len(fn.Blocks) > 0 {
				continue // already defined
			}
			// Skip body generation for cached instances
			if c.cachedInstances[name] {
				continue
			}

			c.typeSubst = subst
			c.monoCtx = &monoContext{inst: inst, origin: enum, name: name}
			// B0285: Suppress match-dup inside enum clone methods
			if md.Name == "clone" {
				c.suppressMatchDup = true
			}
			// T0604: Set currentDropEnum so defineMethodFunc emits variant field drops
			if md.Name == "drop" {
				c.currentDropEnum = enum
			}
			func() {
				defer func() { c.typeSubst = nil; c.monoCtx = nil; c.suppressMatchDup = false; c.currentDropEnum = nil }()
				if genInfo := c.info.GeneratorFuncs[md]; genInfo != nil {
					c.defineGeneratorMethod(md, m, fn, genInfo.ElemType, nil)
				} else {
					c.defineMethodFunc(md, m, fn)
				}
			}()
		}
	}
}

// monoMethodInstanceName generates a unique mangled name for a generic method instantiation.
// Example: Box.transform[string]     → "Box.transform[string]"
// Example: Box[int].transform[string] → "Box[int].transform[string]"
func monoMethodInstanceName(mi *sema.MethodInstance) string {
	ownerName := mi.Owner.Obj().Name()
	if mi.OwnerInst != nil {
		ownerName = monoName(mi.OwnerInst)
	}
	base := mangleMethodName(ownerName, mi.Method.Name(), false)
	if len(mi.TypeArgs) > 0 {
		base += "["
		for i, arg := range mi.TypeArgs {
			if i > 0 {
				base += ", "
			}
			base += typeArgStr(arg)
		}
		base += "]"
	}
	return base
}

// collectMonoMethodInstances deduplicates generic method instantiations.
// Also resolves unresolved instances (same pattern as collectMonoFuncInstances).
func collectMonoMethodInstances(info *sema.Info, typeInstances ...[]*types.Instance) []*sema.MethodInstance {
	seen := map[string]bool{}
	var result []*sema.MethodInstance
	var unresolved []*sema.MethodInstance
	for _, mi := range info.MethodInstances {
		if methodInstanceContainsTypeParam(mi) {
			unresolved = append(unresolved, mi)
			continue
		}
		key := monoMethodInstanceName(mi)
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, mi)
	}

	if len(unresolved) == 0 {
		return result
	}

	// Resolve using type instance substitution maps.
	for _, instances := range typeInstances {
		for _, inst := range instances {
			switch origin := inst.Origin().(type) {
			case *types.Named:
				subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
				if len(subst) > 0 {
					mergeParentSubst(origin, subst)
					resolveUnresolvedMethodInstances(unresolved, subst, &result, seen)
				}
			case *types.Enum:
				subst := types.BuildSubstMap(origin.TypeParams(), inst.TypeArgs())
				if len(subst) > 0 {
					resolveUnresolvedMethodInstances(unresolved, subst, &result, seen)
				}
			}
		}
	}

	// Transitively resolve using each concrete MethodInstance's substitution map.
	for i := 0; i < len(result); i++ {
		mi := result[i]
		subst := buildMethodInstanceSubst(mi)
		if len(subst) > 0 {
			resolveUnresolvedMethodInstances(unresolved, subst, &result, seen)
		}
	}

	return result
}

// buildMethodInstanceSubst builds the combined substitution map for a generic method instance.
// This merges owner type params (if generic type) + method type params.
func buildMethodInstanceSubst(mi *sema.MethodInstance) map[*types.TypeParam]types.Type {
	subst := map[*types.TypeParam]types.Type{}
	// Owner type-level substitution (if on a generic type instance)
	if mi.OwnerInst != nil {
		for k, v := range types.BuildSubstMap(mi.Owner.TypeParams(), mi.OwnerInst.TypeArgs()) {
			subst[k] = v
		}
		mergeParentSubst(mi.Owner, subst)
	}
	// Method-level substitution
	for k, v := range types.BuildSubstMap(mi.Method.Sig().TypeParams(), mi.TypeArgs) {
		subst[k] = v
	}
	return subst
}

// declareMonoMethodInstances declares LLVM functions for monomorphic generic method instances.
func (c *Compiler) declareMonoMethodInstances(file *ast.File, methodInsts []*sema.MethodInstance) {
	for _, mi := range methodInsts {
		name := monoMethodInstanceName(mi)
		if _, exists := c.funcs[name]; exists {
			continue
		}

		td := c.findTypeDecl(file, mi.Owner.Obj().Name())
		if td == nil {
			continue
		}

		// Find the method decl
		var md *ast.MethodDecl
		for _, m := range td.Methods {
			if m.Name == mi.Method.Name() && !m.IsGetter && !m.IsSetter {
				md = m
				break
			}
		}
		if md == nil || md.Body == nil {
			continue
		}

		subst := buildMethodInstanceSubst(mi)

		var params []*ir.Param
		if mi.Method.Sig().Recv() != nil {
			params = append(params, ir.NewParam("this", irtypes.I8Ptr))
		}

		c.typeSubst = subst
		for _, p := range mi.Method.Sig().Params() {
			params = append(params, ir.NewParam(p.Name(), c.resolveParamType(p)))
		}
		retType := irtypes.Type(irtypes.Void)
		if mi.Method.Sig().Result() != nil {
			retType = c.resolveType(mi.Method.Sig().Result())
		}
		c.typeSubst = nil

		if mi.Method.Sig().CanError() {
			retType = computeResultType(retType)
		}

		fn := c.module.NewFunc(name, retType, params...)
		c.funcs[name] = fn
		if c.compilingModule != "" {
			c.moduleOwnedFuncs[name] = c.compilingModule
			// Also mark as instance-owned so this generic method instance gets its own
			// .bc file, keeping the module IR stable across compilations.
			c.instanceOwnedFuncs[name] = name
		}
	}
}

// defineMonoMethodInstances generates method bodies for monomorphic generic method instances.
func (c *Compiler) defineMonoMethodInstances(file *ast.File, methodInsts []*sema.MethodInstance) {
	for _, mi := range methodInsts {
		name := monoMethodInstanceName(mi)

		td := c.findTypeDecl(file, mi.Owner.Obj().Name())
		if td == nil {
			continue
		}

		var md *ast.MethodDecl
		for _, m := range td.Methods {
			if m.Name == mi.Method.Name() && !m.IsGetter && !m.IsSetter {
				md = m
				break
			}
		}
		if md == nil || md.Body == nil {
			continue
		}

		fn, ok := c.funcs[name]
		if !ok || len(fn.Blocks) > 0 {
			continue
		}

		subst := buildMethodInstanceSubst(mi)
		m := c.lookupAnyMethod(mi.Owner, md.Name, false, false)
		if m == nil {
			continue
		}

		c.typeSubst = subst
		if mi.OwnerInst != nil {
			c.monoCtx = &monoContext{
				inst:   mi.OwnerInst,
				origin: mi.Owner,
				name:   monoName(mi.OwnerInst),
			}
		}
		func() {
			defer func() { c.typeSubst = nil; c.monoCtx = nil }()
			c.defineMethodFunc(md, m, fn, mi.Owner)
		}()
	}
}
