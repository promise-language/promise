package sema

import (
	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/types"
)

// FuncInstance records a concrete instantiation of a generic function.
type FuncInstance struct {
	Func     *types.Func      // the generic function
	TypeArgs []types.Type     // concrete type arguments
	Sig      *types.Signature // substituted signature (no TypeParams)
}

// CapturedVar records a variable captured by a lambda/closure.
type CapturedVar struct {
	Obj    types.Object // the captured variable (always *types.Var)
	ByMove bool         // true if move-captured, false if copy-captured
}

// Info holds the results of semantic analysis.
// All maps use AST nodes as keys — the AST itself is not modified.
type Info struct {
	// Types maps each expression to its resolved type.
	Types map[ast.Expr]types.Type

	// Objects maps each identifier expression to the object it refers to.
	Objects map[*ast.IdentExpr]types.Object

	// Scopes maps scope-creating AST nodes (File, Block, etc.) to their scope.
	Scopes map[ast.Node]*types.Scope

	// StdScope is the std library scope (parent of file scope). Nil if no std loaded.
	StdScope *types.Scope

	// Instances records all concrete generic type instantiations for later monomorphization.
	Instances []*types.Instance

	// FuncInstances records all concrete generic function instantiations for later monomorphization.
	FuncInstances []*FuncInstance

	// Tests records functions annotated with `test.
	Tests []*types.Func

	// ExpectOutput is the expected stdout from `test(expected="...") on main.
	ExpectOutput    string
	HasExpectOutput bool

	// ExcludeTargets lists target substrings for which this test should be skipped.
	// Parsed from `test(expected="...", exclude: "wasm32") annotation on main().
	ExcludeTargets []string

	// TestExcludes maps test function names to their exclude target substrings.
	// Parsed from `test(exclude: "wasm32") annotations on individual test functions.
	TestExcludes map[string][]string

	// FieldDefaults maps fields with default values to their AST default expressions.
	// Used by codegen to evaluate defaults for omitted constructor arguments.
	FieldDefaults map[*types.Field]ast.Expr

	// ParamDefaults maps function/method parameters with default values to their AST default expressions.
	// Used by codegen to evaluate defaults for omitted named arguments.
	ParamDefaults map[*types.Param]ast.Expr

	// LambdaCaptures maps each lambda to its captured variables (in capture order).
	// Empty slice means no captures; nil key means lambda was not analyzed.
	LambdaCaptures map[*ast.LambdaExpr][]*CapturedVar

	// OptionalNarrowings maps if-statement nodes to their narrowing info.
	// Used by codegen to unwrap optional variables in narrowed scopes.
	OptionalNarrowings map[*ast.IfStmt]*OptionalNarrowing

	// FailableExprs records expressions whose evaluation can produce an error
	// (failable function/method calls, failable constructor calls). Used by
	// error operators (?, !, ? handler) to validate their inner expression.
	FailableExprs map[ast.Expr]bool

	// AutoPropagateExprs records ExprStmt expressions (failable calls used as
	// statements in failable functions) that need implicit error propagation.
	// Codegen emits the same tag-check + early-return as explicit `?`.
	AutoPropagateExprs map[ast.Expr]bool

	// FailableDestructures records destructure declarations whose RHS is a
	// failable call. Codegen extracts (value, error?) from the result struct
	// instead of a tuple.
	FailableDestructures map[*ast.DestructureVarDecl]bool

	// GeneratorFuncs maps generator function/method declarations to their
	// element type T. A function is a generator if its return type is
	// stream[T] and its body contains at least one yield statement.
	GeneratorFuncs map[ast.Node]types.Type
}

// NarrowedVar records a single variable narrowing (T? → T).
type NarrowedVar struct {
	VarName   string     // the variable being narrowed
	InnerType types.Type // the unwrapped type (T from T?)
}

// OptionalNarrowing records that an if-statement narrows one or more optional variables.
type OptionalNarrowing struct {
	Vars    []NarrowedVar // one or more narrowed variables
	Negated bool          // if true, narrowing applies to else branch (!cc form)
}

// recordType stores the resolved type for an expression.
func (c *Checker) recordType(expr ast.Expr, typ types.Type) {
	if typ != nil {
		c.info.Types[expr] = typ
	}
}

// recordObject stores the resolved object for an identifier expression.
func (c *Checker) recordObject(ident *ast.IdentExpr, obj types.Object) {
	if obj != nil {
		c.info.Objects[ident] = obj
	}
}

// recordInstance records a concrete generic instantiation for later monomorphization.
// Non-concrete instances (containing TypeParams from type definitions) are skipped.
func (c *Checker) recordInstance(inst *types.Instance) {
	for _, arg := range inst.TypeArgs() {
		if types.ContainsTypeParam(arg) {
			return
		}
	}
	c.info.Instances = append(c.info.Instances, inst)
}
