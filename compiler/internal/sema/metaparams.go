package sema

import (
	"sort"
	"strconv"
	"strings"

	"github.com/promise-language/promise/compiler/internal/ast"
)

// T1449: annotation parameter contracts.
//
// Every built-in meta annotation declares exactly what parameters it accepts —
// how many positional parameters (and what each one means), which named
// parameters are allowed, and what form each value must take. A single shared
// validator drives the whole table, so a parameter the user wrote is never
// silently discarded: unknown names, excess positionals, positional-after-named,
// duplicates, and wrong value forms are all compilation errors.

// metaValueKind describes the syntactic form a meta annotation parameter value
// must take.
type metaValueKind int

const (
	valString      metaValueKind = iota // string literal
	valBool                             // true / false
	valInt                              // integer literal
	valIdent                            // bare identifier
	valTargetCond                       // `target condition: ident, !, ||, &&
	valExcludeCond                      // `test(exclude:) condition: ident, ||
)

// describe returns the human-readable requirement for a value kind, used in
// "parameter 'x' must be ..." diagnostics. The condition kinds report their own
// (much more specific) diagnostics from validateCondExpr and never reach here.
func (k metaValueKind) describe() string {
	switch k {
	case valBool:
		return "a boolean literal (true or false)"
	case valInt:
		return "an integer literal"
	case valIdent:
		return "an identifier"
	case valTargetCond, valExcludeCond:
		return "a target condition"
	}
	return "a string literal"
}

// metaPositional describes one positional parameter slot.
type metaPositional struct {
	name     string // role name, used in diagnostics (e.g. "path")
	kind     metaValueKind
	optional bool // trailing slots may be omitted
}

// metaParamSpec is one annotation's full parameter contract.
type metaParamSpec struct {
	positional []metaPositional
	named      map[string]metaValueKind
}

// required returns the number of leading positional slots that must be present.
func (s metaParamSpec) required() int {
	n := 0
	for _, p := range s.positional {
		if p.optional {
			break
		}
		n++
	}
	return n
}

// noParams is the contract for annotations that take no parameters at all.
var noParams = metaParamSpec{}

// metaParamSpecs maps every name in builtinMetas to its parameter contract.
// TestT1449SpecsExhaustive asserts the two tables have identical key sets, so
// adding an annotation without declaring its contract fails the build.
var metaParamSpecs = map[string]metaParamSpec{
	"doc": {positional: []metaPositional{{name: "text", kind: valString}}},
	"deprecated": {
		positional: []metaPositional{{name: "message", kind: valString, optional: true}},
		named:      map[string]metaValueKind{"since": valString, "message": valString},
	},
	"test": {named: map[string]metaValueKind{
		"expected":     valString,
		"exclude":      valExcludeCond,
		"timeout":      valString,
		"memory_limit": valString,
		"allow_leaks":  valBool,
	}},
	"embed": {
		positional: []metaPositional{{name: "path", kind: valString}},
		named:      map[string]metaValueKind{"compress": valBool},
	},
	"key":          {positional: []metaPositional{{name: "name", kind: valString}}},
	"serializable": {named: map[string]metaValueKind{"tag": valString}},
	"lifetime":     {positional: []metaPositional{{name: "name", kind: valIdent}}},
	"wasm_import": {positional: []metaPositional{
		{name: "module name", kind: valString},
		{name: "import name", kind: valString},
	}},
	"target": {positional: []metaPositional{{name: "condition", kind: valTargetCond}}},
	"align":  {positional: []metaPositional{{name: "alignment", kind: valInt}}},
	"extern": {positional: []metaPositional{{name: "symbol", kind: valString, optional: true}}},

	// Parameterless annotations.
	"value":        noParams,
	"instance":     noParams,
	"variant":      noParams,
	"global":       noParams,
	"mono":         noParams,
	"raw":          noParams,
	"abstract":     noParams,
	"native":       noParams,
	"copy":         noParams,
	"clone":        noParams,
	"structural":   {named: map[string]metaValueKind{"protocol": valBool}},
	"inline":       noParams,
	"packed":       noParams,
	"skip":         noParams,
	"include_none": noParams,
	"required":     noParams,
	"flatten":      noParams,
	"public":       noParams,
	"unsafe":       noParams,
	"final":        noParams,
	"factory":      noParams,
	"sendable":     noParams,
	"sharable":     noParams,
	"not_sendable": noParams,
	"not_sharable": noParams,
	"confined":     noParams,
	"interior":     noParams,
}

// validateMetaParams enforces one annotation's parameter contract.
func (c *Checker) validateMetaParams(ann *ast.MetaAnnotation) {
	spec, ok := metaParamSpecs[ann.Name]
	if !ok {
		return // unknown annotation — already reported by validateMetas
	}

	takesNothing := len(spec.positional) == 0 && len(spec.named) == 0
	if takesNothing && len(ann.Params) > 0 {
		c.errorf(ann.Params[0].Pos(), "`%s takes no parameters", ann.Name)
		return
	}

	positional := 0
	sawNamed := false
	seen := make(map[string]bool)

	for _, p := range ann.Params {
		if p.Name == "" {
			if sawNamed {
				c.errorf(p.Pos(), "positional parameter must precede named parameters in `%s", ann.Name)
				continue
			}
			if positional >= len(spec.positional) {
				c.errorf(p.Pos(), "`%s takes %s", ann.Name, positionalLimit(spec))
				positional++
				continue
			}
			slot := spec.positional[positional]
			c.checkMetaValue(ann.Name, slot.name, p.Value, slot.kind)
			positional++
			continue
		}

		sawNamed = true
		kind, allowed := spec.named[p.Name]
		if !allowed {
			c.errorf(p.Pos(), "unknown parameter '%s' in `%s; %s", p.Name, ann.Name, allowedNamed(spec))
			continue
		}
		if seen[p.Name] {
			c.errorf(p.Pos(), "duplicate annotation parameter '%s' in `%s", p.Name, ann.Name)
			continue
		}
		// A slot filled positionally cannot also be given by name — e.g.
		// `deprecated("msg", message: "msg") names the same parameter twice.
		if filledPositionally(spec, positional, p.Name) {
			c.errorf(p.Pos(), "parameter '%s' in `%s is given both positionally and by name", p.Name, ann.Name)
			continue
		}
		seen[p.Name] = true
		c.checkMetaValue(ann.Name, p.Name, p.Value, kind)
	}

	if req := spec.required(); positional < req {
		c.errorf(ann.Pos(), "`%s requires %s", ann.Name, requiredPositional(spec, req))
	}
}

// filledPositionally reports whether one of the first n positional slots carries
// the given role name, meaning the same parameter was already supplied.
func filledPositionally(spec metaParamSpec, n int, name string) bool {
	if n > len(spec.positional) {
		n = len(spec.positional)
	}
	for _, s := range spec.positional[:n] {
		if s.name == name {
			return true
		}
	}
	return false
}

// positionalLimit describes the maximum positional arity, e.g.
// "at most 1 positional parameter (text)" or "no positional parameters".
func positionalLimit(spec metaParamSpec) string {
	if len(spec.positional) == 0 {
		return "no positional parameters"
	}
	return "at most " + countedPositional(spec.positional)
}

// requiredPositional describes the required positional arity, e.g.
// "2 positional parameters (module name, import name)".
func requiredPositional(spec metaParamSpec, req int) string {
	return countedPositional(spec.positional[:req])
}

// countedPositional renders "N positional parameter(s) (a, b)".
func countedPositional(slots []metaPositional) string {
	names := make([]string, len(slots))
	for i, s := range slots {
		names[i] = s.name
	}
	unit := " positional parameters ("
	if len(slots) == 1 {
		unit = " positional parameter ("
	}
	return strconv.Itoa(len(slots)) + unit + strings.Join(names, ", ") + ")"
}

// allowedNamed lists the annotation's allowed named parameters in sorted order
// so diagnostics are deterministic.
func allowedNamed(spec metaParamSpec) string {
	if len(spec.named) == 0 {
		return "it takes no named parameters"
	}
	names := make([]string, 0, len(spec.named))
	for n := range spec.named {
		names = append(names, n)
	}
	sort.Strings(names)
	return "allowed: " + strings.Join(names, ", ")
}

// checkMetaValue verifies one parameter value has the required form.
func (c *Checker) checkMetaValue(annName, paramName string, value ast.Expr, kind metaValueKind) {
	switch kind {
	case valTargetCond:
		c.validateCondExpr(value, true)
		return
	case valExcludeCond:
		c.validateCondExpr(value, false)
		return
	case valString:
		if _, ok := value.(*ast.StringLit); ok {
			return
		}
	case valBool:
		if _, ok := value.(*ast.BoolLit); ok {
			return
		}
	case valInt:
		if _, ok := value.(*ast.IntLit); ok {
			return
		}
	case valIdent:
		if _, ok := value.(*ast.IdentExpr); ok {
			return
		}
	}
	c.errorf(value.Pos(), "`%s parameter '%s' must be %s", annName, paramName, kind.describe())
}

// validTargetIdents is the human-readable list of platform identifiers accepted
// by `target(cond) and `test(exclude: cond), derived from ValidExcludeIdents so
// the diagnostic can never drift from the set actually accepted.
var validTargetIdents = func() string {
	names := make([]string, 0, len(ValidExcludeIdents))
	for n := range ValidExcludeIdents {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}()

// unwrapParens strips redundant parentheses so condition expressions are
// analyzed on their operator structure. `target((linux || macos) && x86_64) is
// documented as valid, so every consumer of a condition must see through the
// ParenExpr wrapper.
func unwrapParens(expr ast.Expr) ast.Expr {
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok || p.Expr == nil {
			return expr
		}
		expr = p.Expr
	}
}

// validateCondExpr validates a platform condition expression. `target accepts
// the full boolean grammar (identifier, !, ||, &&, parentheses);
// `test(exclude:) accepts only an identifier or a || chain of identifiers.
func (c *Checker) validateCondExpr(expr ast.Expr, full bool) {
	expr = unwrapParens(expr)
	switch e := expr.(type) {
	case *ast.IdentExpr:
		if !ValidExcludeIdents[e.Name] {
			if full {
				c.errorf(e.Pos(), "unknown target identifier %q; valid identifiers: %s", e.Name, validTargetIdents)
			} else {
				c.errorf(e.Pos(), "unknown exclude target %q; valid identifiers: %s", e.Name, validTargetIdents)
			}
		}
	case *ast.UnaryExpr:
		if full && e.Op == ast.UnaryNot {
			c.validateCondExpr(e.Operand, full)
			return
		}
		c.condFormError(expr, full)
	case *ast.BinaryExpr:
		if e.Op == ast.BinOr || (full && e.Op == ast.BinAnd) {
			c.validateCondExpr(e.Left, full)
			c.validateCondExpr(e.Right, full)
			return
		}
		if !full {
			c.errorf(expr.Pos(), "exclude expression must use || to combine target identifiers")
			return
		}
		c.condFormError(expr, full)
	case *ast.StringLit:
		if full {
			c.errorf(expr.Pos(), "`target condition must be an identifier, not a string literal (e.g., `target(wasm) instead of `target(\"wasm32\"))")
		} else {
			c.errorf(expr.Pos(), "exclude target must be an identifier, not a string literal (e.g., exclude: wasm instead of exclude: \"wasm32\")")
		}
	default:
		c.condFormError(expr, full)
	}
}

func (c *Checker) condFormError(expr ast.Expr, full bool) {
	if full {
		c.errorf(expr.Pos(), "invalid `target condition; expected identifier, !, ||, or &&")
		return
	}
	c.errorf(expr.Pos(), "invalid exclude expression; expected identifier or identifier || identifier")
}
