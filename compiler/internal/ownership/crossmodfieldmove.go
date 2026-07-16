package ownership

import (
	"strings"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// checkImportedGenericBodies closes the T1303 gap: a generic type defined in an
// imported module whose field-escaping body double-frees for a drop-bearing
// instantiation. The module body is never visited by the user unit's ownership
// pass (it isn't in c.file), so checkGenericFieldMove — which relies on visiting
// the body while c.info.Instances holds the concrete instantiation — never fires.
// Here we re-run the ownership body-check over each *instantiated* generic module
// type, with the user unit's concrete instances injected, so the same verdict
// logic (checkGenericFieldMove/fieldMoveVerdict) rejects the unsafe escape.
func (c *Checker) checkImportedGenericBodies() {
	if len(c.info.ModuleInfos) == 0 || len(c.info.Instances) == 0 {
		return
	}
	// Nameds declared in the current file are already covered by the inline path.
	inFile := c.fileDeclaredNameds()

	// Group the user unit's fully-concrete instances of module-defined generic
	// types by origin Named.
	byOrigin := map[*types.Named][]*types.Instance{}
	var moduleInsts []*types.Instance
	for _, inst := range c.info.Instances {
		named, ok := inst.Origin().(*types.Named)
		if !ok || len(named.TypeParams()) == 0 || inFile[named] {
			continue
		}
		if !allConcrete(inst) { // no TypeArg contains a TypeParam
			continue
		}
		byOrigin[named] = append(byOrigin[named], inst)
		moduleInsts = append(moduleInsts, inst)
	}
	if len(byOrigin) == 0 {
		return
	}

	// Dedupe against diagnostics already emitted.
	seen := map[string]bool{}
	for _, e := range c.errors {
		seen[e.Error()] = true
	}

	for _, mi := range c.info.ModuleInfos {
		if mi == nil || mi.File == nil || mi.SemaInfo == nil {
			continue
		}
		fileScope := mi.SemaInfo.Scopes[mi.File]
		if fileScope == nil {
			continue
		}
		var targets []*ast.TypeDecl
		for _, decl := range mi.File.Decls {
			td, ok := decl.(*ast.TypeDecl)
			if !ok || len(td.TypeParams) == 0 {
				continue
			}
			tn, _ := fileScope.Lookup(td.Name).(*types.TypeName)
			if tn == nil {
				continue
			}
			named, _ := tn.Type().(*types.Named)
			if named != nil && len(byOrigin[named]) > 0 {
				targets = append(targets, td)
			}
		}
		if len(targets) == 0 {
			continue
		}
		// Shallow-copy the module's sema Info, augmenting only Instances with the
		// user unit's concrete module-type instances so checkGenericFieldMove sees
		// Box[Res]. checkGenericFieldMove filters by `named == owner`, so injecting
		// every module instance is safe.
		injected := *mi.SemaInfo
		injected.Instances = append(append([]*types.Instance{}, mi.SemaInfo.Instances...), moduleInsts...)

		sc := newChecker(mi.File, &injected)
		for _, td := range targets {
			sc.checkTypeDecl(td)
		}
		// Keep ONLY field-move-verdict diagnostics. Module bodies are never
		// ownership-checked in the user's compilation (loadModuleScopes runs sema
		// but not ownership.Check), so a full traversal re-surfaces other
		// conservative ownership rules the module's own code legitimately relies on
		// never being validated (e.g. move-capturing a function-typed borrowed
		// param, consuming a match binding). This pass is scoped to the
		// field-escape double-free class only (T1303) — the field-move verdict is
		// instance-driven and flow-state-independent, so it is the sole sound
		// diagnostic to lift out of the isolated re-check.
		for _, e := range sc.errors {
			if !isFieldMoveVerdict(e) || seen[e.Error()] {
				continue
			}
			seen[e.Error()] = true
			c.errors = append(c.errors, e)
		}
	}
}

// fileDeclaredNameds collects the *types.Named declared directly in the current
// file (types and enums). The inline checkGenericFieldMove path already covers
// these, so the cross-module pass skips them to avoid duplicate diagnostics.
func (c *Checker) fileDeclaredNameds() map[*types.Named]bool {
	out := map[*types.Named]bool{}
	for _, decl := range c.file.Decls {
		var name string
		switch d := decl.(type) {
		case *ast.TypeDecl:
			name = d.Name
		case *ast.EnumDecl:
			name = d.Name
		default:
			continue
		}
		tn, _ := c.lookupFileScope(name).(*types.TypeName)
		if tn == nil {
			continue
		}
		if named, ok := tn.Type().(*types.Named); ok {
			out[named] = true
		}
	}
	return out
}

// isFieldMoveVerdict reports whether e is a field-move-verdict diagnostic
// produced by fieldMoveVerdict (the plain-field or closure-field escape
// rejection). Both messages share the "cannot move ... out of '" shape; matching
// the stable prefix keeps this in step with the single fieldMoveVerdict source
// without duplicating its exact wording.
func isFieldMoveVerdict(e error) bool {
	msg := e.Error()
	return strings.Contains(msg, "cannot move field '") ||
		strings.Contains(msg, "cannot move closure field '")
}

// allConcrete reports whether every type argument of inst is fully concrete
// (contains no TypeParam) — mirrors the loop in checkGenericFieldMove.
func allConcrete(inst *types.Instance) bool {
	for _, ta := range inst.TypeArgs() {
		if types.ContainsTypeParam(ta) {
			return false
		}
	}
	return true
}
