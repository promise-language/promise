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

// MethodInstance records a concrete instantiation of a generic method.
type MethodInstance struct {
	Owner     *types.Named     // origin type owning the method
	OwnerInst *types.Instance  // non-nil when owner is a generic instance (e.g., Box[int])
	Method    *types.Method    // the generic method
	TypeArgs  []types.Type     // method-level concrete type arguments
	Sig       *types.Signature // fully substituted signature (no TypeParams)
}

// ForInKind indicates how a for-in loop iterates over a duck-typed iterable.
type ForInKind int

const (
	ForInNext ForInKind = iota + 1 // type has next() T? method
	ForInIter                      // type has iter() returning type with next() T?
)

// CapturedVar records a variable captured by a lambda/closure.
type CapturedVar struct {
	Obj    types.Object // the captured variable (always *types.Var)
	ByMove bool         // true if move-captured, false if copy-captured
}

// ModuleInfo bundles everything codegen needs about an imported module:
// the module's merged AST file and its sema output.
type ModuleInfo struct {
	Name           string    // module alias (binding name used in consumer code)
	CanonicalName  string    // module's own name from its promise.toml (display only)
	GlobalIdentity string    // globally unique identity (URL for remote, path for local, name for catalog)
	IRPrefix       string    // sanitized prefix for IR symbols (derived from GlobalIdentity)
	Path           string    // source path (empty for catalog modules)
	File           *ast.File // the module's merged AST (with std decls merged in)
	SemaInfo       *Info     // the module's sema output
	AbsDir         string    // absolute path to module directory
	ImplHash       string    // FNV-128a of module source files (implementation hash)
	InterfaceHash  string    // FNV-128a of public API signatures (interface hash)
}

// EffectiveIRPrefix returns the IR prefix to use for this module's symbols.
// Falls back to CanonicalName then Name for tests that don't set GlobalIdentity.
func (mi *ModuleInfo) EffectiveIRPrefix() string {
	if mi.IRPrefix != "" {
		return mi.IRPrefix
	}
	if mi.CanonicalName != "" {
		return mi.CanonicalName
	}
	return mi.Name
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

	// ScopeOrder lists all scopes in insertion order (file scope first, then
	// nested block/function scopes). Provides deterministic iteration — always
	// use this instead of ranging over the Scopes map directly.
	ScopeOrder []*types.Scope

	// Instances records all concrete generic type instantiations for later monomorphization.
	Instances []*types.Instance

	// FuncInstances records all concrete generic function instantiations for later monomorphization.
	FuncInstances []*FuncInstance

	// MethodInstances records all concrete generic method instantiations for later monomorphization.
	MethodInstances []*MethodInstance

	// FilteredDecls records top-level declarations that were excluded by a `target(cond)
	// annotation whose condition did not match the build target.
	// Codegen uses this to skip generating code for filtered-out declarations.
	// Reading from a nil map is safe in Go (returns false); it is populated only
	// when target filtering is active (non-zero TargetInfo in CheckWithTarget).
	FilteredDecls map[ast.Decl]bool

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

	// OptionalRecoveryHandlers records ErrorHandlerExpr nodes whose handler
	// body does not produce a recovery value or diverge, used in an assignment
	// where the declared/inferred type becomes optional. Codegen wraps the
	// success value as some(T) and uses none for the error path.
	OptionalRecoveryHandlers map[ast.Expr]bool

	// FailableDestructures records destructure declarations whose RHS is a
	// failable call. Codegen extracts (value, error?) from the result struct
	// instead of a tuple.
	FailableDestructures map[*ast.DestructureVarDecl]bool

	// ForInKinds maps for-in statements to their duck-typed iteration kind.
	// Set when the iterable type is not a built-in (Array, Vector, Map, Channel, Range, String)
	// but satisfies the iterator protocol via next() T? or iter() methods.
	ForInKinds map[*ast.ForInStmt]ForInKind

	// GeneratorFuncs maps generator function/method declarations to their
	// element type T. A function is a generator if its return type is
	// stream[T] and its body contains at least one yield statement.
	GeneratorFuncs map[ast.Node]types.Type

	// ModuleInfos maps module keys (catalog name or source path) to the
	// full module compilation result. Used by codegen to inline module
	// declarations into the main IR module.
	ModuleInfos map[string]*ModuleInfo

	// ModuleOrder lists module keys in topological order (dependencies first).
	// Codegen must process modules in this order so that a module's dependencies
	// are compiled before the module itself.
	ModuleOrder []string

	// GlobImportedObjs records objects that were injected into file scope via
	// `use X as _` (glob imports). ExportedScope uses this to exclude re-exported
	// imported symbols — a module should only export its own declarations.
	GlobImportedObjs map[types.Object]bool

	// DeclHashes maps each declared TypeName to the hash of its AST declaration
	// (TypeDecl or EnumDecl). The hash covers all fields, methods, variants, and
	// annotations — everything that affects the generated IR for a generic instance.
	// Used to key per-instance .bc cache entries so that changes to unrelated
	// declarations in the same file do not invalidate cached instances.
	DeclHashes map[*types.TypeName]string
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
