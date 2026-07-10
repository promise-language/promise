package sema

import (
	"time"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/types"
)

// SemaTimings records per-pass durations within a single sema run.
type SemaTimings struct {
	Declare time.Duration
	Define  time.Duration
	Check   time.Duration
	Verify  time.Duration
}

// FuncInstance records a concrete instantiation of a generic function.
type FuncInstance struct {
	Func     *types.Func      // the generic function
	TypeArgs []types.Type     // concrete type arguments
	Sig      *types.Signature // substituted signature (no TypeParams)
}

// InferredCall records the inferred type arguments for a generic call expression.
// Used by codegen to build monomorphized function names.
type InferredCall struct {
	TypeArgs []types.Type // inferred concrete type arguments
	FuncName string       // function/method name (for mangling)
}

// MethodInstance records a concrete instantiation of a generic method.
// Owner and OwnerEnum are mutually exclusive: exactly one is non-nil,
// identifying whether the method's origin is a Named type or an Enum.
type MethodInstance struct {
	Owner     *types.Named     // origin type owning the method (nil if enum-owned)
	OwnerEnum *types.Enum      // origin enum owning the method (nil if Named-owned)
	OwnerInst *types.Instance  // non-nil when owner is a generic instance (e.g., Box[int])
	Method    *types.Method    // the generic method
	TypeArgs  []types.Type     // method-level concrete type arguments
	Sig       *types.Signature // fully substituted signature (no TypeParams)
}

// OwnerName returns the declared name of the method's origin type,
// regardless of whether it is a Named type or an Enum (T0636).
func (mi *MethodInstance) OwnerName() string {
	if mi.OwnerEnum != nil {
		return mi.OwnerEnum.Obj().Name()
	}
	return mi.Owner.Obj().Name()
}

// OwnerTypeParams returns the origin type's type parameters,
// regardless of whether it is a Named type or an Enum (T0636).
func (mi *MethodInstance) OwnerTypeParams() []*types.TypeParam {
	if mi.OwnerEnum != nil {
		return mi.OwnerEnum.TypeParams()
	}
	return mi.Owner.TypeParams()
}

// CloneabilityRequirement is a deferred check that a type expression must not
// contain a single-owner handle (Task/Mutex/MutexGuard) after type-parameter
// substitution. Recorded when a generic function/method body contains a
// container clone()/filled() whose element type references a TypeParam owned
// by the enclosing generic; validated at each concrete call site. (T0616)
type CloneabilityRequirement struct {
	TypeExpr types.Type // may contain TypeParams; substituted at call site
	Pos      ast.Pos    // location of the requiring operation (inside the generic body)
	OpDesc   string     // human-readable op, e.g. "Vector[T].clone()" / "Map[K,V].clone()"
}

// GenericCallEdge records that a generic caller's body invokes a generic
// callee with a substitution that maps callee TypeParams (possibly through
// caller TypeParams). Used by the cloneability requirement propagation pass
// to forward callee requirements onto the caller. Exactly one of CallerFunc
// or CallerMethod is non-nil; same for CalleeFunc / CalleeMethod. (T0616)
type GenericCallEdge struct {
	CallerFunc   *types.Func
	CallerMethod *types.Method
	CalleeFunc   *types.Func
	CalleeMethod *types.Method
	Subst        map[*types.TypeParam]types.Type
	CallPos      ast.Pos
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

// GeneratorInfo records metadata about a generator function/method.
type GeneratorInfo struct {
	ElemType types.Type // T from stream[T]
	CanError bool       // true if the generator is failable (has ! in signature)
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

	// FuncCloneReqs accumulates deferred cloneability requirements per generic
	// function. Populated during Pass 3 body checking when a Vector/Map/Set/Array
	// clone()/filled() is seen with an element type referencing a TypeParam owned
	// by the enclosing function. Consumed at each concrete call site to forbid
	// instantiation with single-owner handle types. (T0616)
	FuncCloneReqs map[*types.Func][]CloneabilityRequirement

	// MethodCloneReqs is the per-method analogue of FuncCloneReqs. (T0616)
	MethodCloneReqs map[*types.Method][]CloneabilityRequirement

	// GenericCallEdges records each generic-body call to another generic callee
	// (when neither caller nor callee is fully concrete). Used by the
	// cloneability-requirement propagation post-pass. (T0616)
	GenericCallEdges []GenericCallEdge

	// InferredTypeArgs maps call expressions with inferred type arguments to the
	// concrete type args. Used by codegen to build monomorphized function names
	// for calls where type arguments were inferred (not explicit in the AST).
	InferredTypeArgs map[*ast.CallExpr]*InferredCall

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

	// ExcludeTargets lists target identifier names for which this test should be skipped.
	// Parsed from `test(expected="...", exclude: wasm) annotation on main().
	ExcludeTargets []string

	// TestExcludes maps test function names to their exclude target identifier names.
	// Parsed from `test(exclude: wasm) annotations on individual test functions.
	TestExcludes map[string][]string

	// TestTimeouts maps test function names to their timeout duration strings.
	// Parsed from `test(timeout: "5s") annotations on individual test functions.
	// Values are Go-style duration strings (e.g., "500ms", "2s", "1m").
	TestTimeouts map[string]string

	// TestMemoryLimits maps test function names to their memory-limit size strings.
	// Parsed from `test(memory_limit: "512MB") annotations (T0689). Values are
	// size-with-unit strings parsed by parseMemoryLimitArg in cmd/promise/main.go
	// (e.g. "256MB", "2GB", "0" for opt-out).
	TestMemoryLimits map[string]string

	// TestAllowLeaks maps test function names that have `test(allow_leaks: true).
	// Tests with this flag report leaks as warnings but do not cause test failure.
	TestAllowLeaks map[string]bool

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

	// IsDestructureNarrowings maps if-statement nodes to destructure is-pattern info.
	// Used by codegen to extract fields into bindings in the then-block.
	IsDestructureNarrowings map[*ast.IfStmt]*IsDestructureNarrowing

	// IsNarrowings maps if-statement nodes to non-destructive is-pattern narrowing
	// info (`if x is T { ... }`). For the class case codegen needs no special path
	// (the shadow Var recorded in sema makes member/method access resolve against
	// the subtype over the uniform value representation); the enum case is consumed
	// via NarrowedVariantField. T0993.
	IsNarrowings map[*ast.IfStmt]*IsNarrowing

	// NarrowedVariantField maps a member-access node (`x.field`) to the enum
	// variant-data field it reads, when `x` was narrowed via `if x is Variant`.
	// Codegen emits a variant-data GEP+load. T0993.
	NarrowedVariantField map[*ast.MemberExpr]*VariantFieldAccess

	// ModuleGetters records module-qualified member accesses (`mod.property`)
	// that resolve to a module-level getter, so codegen calls the getter
	// function with no args. Keyed on sema's actual resolution rather than the
	// shape of the result type — a getter whose return type is itself a
	// function type (`get adder() -> int`) also has a Signature result type, so
	// the old "result is a Signature ⇒ function reference" heuristic
	// misclassified it and codegen panicked (T1240).
	ModuleGetters map[*ast.MemberExpr]bool

	// FailableExprs records expressions whose evaluation can produce an error
	// (failable function/method calls, failable constructor calls). Used by
	// error operators (?, !, ? handler) to validate their inner expression.
	FailableExprs map[ast.Expr]bool

	// AutoPropagateExprs records failable call expressions that need implicit
	// error propagation. Applies to: expression statements, variable declaration
	// initializers, call arguments, binary operands, and unary operands — all
	// in failable functions. Codegen emits the same tag-check + early-return as explicit `?`.
	AutoPropagateExprs map[ast.Expr]bool

	// OptionalHandlers records ErrorHandlerExpr nodes that are optional handlers
	// (T? ? { recovery } → T) rather than error handlers.
	OptionalHandlers map[ast.Expr]bool

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
	// generator info (element type T and failable flag). A function is a
	// generator if its return type is stream[T] and its body contains at
	// least one yield statement.
	GeneratorFuncs map[ast.Node]*GeneratorInfo

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

	// IsPatternTypes maps is-pattern nodes to their resolved types when the
	// pattern includes type arguments (e.g., `x is Box[int]`). For non-generic
	// patterns, the entry is absent and codegen falls back to name-based lookup.
	IsPatternTypes map[ast.IsPattern]types.Type

	// ErrorHandlerTypes maps error handler expressions to their resolved type
	// when the handler includes type arguments (e.g., `? e is DataError[string]`).
	ErrorHandlerTypes map[*ast.ErrorHandlerExpr]types.Type

	// DeclHashes maps each declared TypeName to the hash of its AST declaration
	// (TypeDecl or EnumDecl). The hash covers all fields, methods, variants, and
	// annotations — everything that affects the generated IR for a generic instance.
	// Used to key per-instance .bc cache entries so that changes to unrelated
	// declarations in the same file do not invalidate cached instances.
	DeclHashes map[*types.TypeName]string

	// Embeds maps module-level getter declarations with `embed annotations
	// to their embed metadata. Codegen uses this to generate getter bodies
	// that return the embedded file contents.
	Embeds map[*ast.FuncDecl]*EmbedInfo

	// EarlyDrops maps statement AST nodes to variables that should be dropped
	// immediately after that statement executes, rather than at scope exit.
	// Populated by NLL last-use analysis in ownership.AnalyzeLastUses (B0035).
	// Codegen checks this after each statement emission to insert early drops.
	EarlyDrops map[ast.Stmt][]string

	// Timings records per-pass durations within CheckWithTarget (T0215).
	// Always populated; negligible overhead (~100ns for 4 time.Now calls).
	Timings SemaTimings
}

// EmbedKind indicates the target type for an `embed annotation.
type EmbedKind int

const (
	EmbedString EmbedKind = iota // embed as string
	EmbedBytes                   // embed as u8[]
	EmbedDir                     // embed as EmbeddedFiles (directory tree, T0031)
)

// EmbedDirEntry records metadata for a single file or directory within an embedded
// directory tree (EmbedDir kind). Populated by ResolveEmbeds.
type EmbedDirEntry struct {
	Path   string // relative path within the embedded directory
	Name   string // base name of the file
	Size   int64  // file size in bytes (0 for directories)
	IsDir  bool   // true if this entry is a directory
	Offset int64  // byte offset into EmbedInfo.Data
}

// EmbedInfo records metadata for a module-level getter with an `embed annotation.
type EmbedInfo struct {
	Path         string          // raw relative path from the annotation
	Kind         EmbedKind       // target type (string, u8[], or EmbeddedFiles)
	Compress     bool            // `embed("path", compress: true)
	Data         []byte          // populated after sema by ResolveEmbeds; gzipped if Compress
	OriginalSize int64           // uncompressed length when Compress is true
	DirEntries   []EmbedDirEntry // populated for EmbedDir kind by ResolveEmbeds (T0031)
}

// NarrowedVar records a single variable narrowing (T? → T).
type NarrowedVar struct {
	VarName   string     // the variable being narrowed
	InnerType types.Type // the unwrapped type (T from T?)
}

// OptionalNarrowing records that an if-statement narrows one or more optional variables.
type OptionalNarrowing struct {
	Vars       []NarrowedVar // one or more narrowed variables
	Negated    bool          // if true, narrowing applies to else branch (!cc form)
	PostNarrow bool          // if true, narrowing persists after the if (diverging then-body)
}

// IsDestructureBinding records a single binding from a destructure is-pattern.
type IsDestructureBinding struct {
	VarName string     // the binding variable name
	Type    types.Type // the resolved type of the field being bound
}

// IsDestructureNarrowing records that an if-statement's condition is a destructure
// is-pattern (e.g., `if shape is Circle(r)` or `if animal is Dog(breed)`).
// Codegen uses this to extract fields into bindings in the then-block.
type IsDestructureNarrowing struct {
	SubjectExpr ast.Expr               // the expression being checked
	Bindings    []IsDestructureBinding // binding variable names and types
	IsEnum      bool                   // true if checking an enum variant
	VariantName string                 // for enums: the variant name
	TargetType  types.Type             // the target type (Named or Instance)
}

// IsNarrowing records that an if-statement's condition is a non-destructive
// is-pattern (`if x is T { ... }`) that narrows the subject variable `x` to the
// tested subtype/variant `T` for the then-block. Classes and enums share this
// single record (T0993); IsEnum selects the branch. The class case shadows the
// subject with a Var of the subtype (no codegen narrowing record needed — the
// value representation is uniform). The enum case exposes the variant's named
// payload members via NarrowedVariantField for codegen variant-data reads.
type IsNarrowing struct {
	SubjectName string                          // the variable being narrowed (always an IdentExpr)
	IsEnum      bool                            // false: class subtype; true: enum variant
	NarrowType  types.Type                      // class case: the subtype Named/Instance to shadow with
	Enum        *types.Enum                     // enum case: the enum origin
	Variant     *types.Variant                  // enum case: the matched variant
	Subst       map[*types.TypeParam]types.Type // enum case: generic substitution (nil if non-generic)
	TargetType  types.Type                      // enum case: the subject's enum type (Enum or Instance)
}

// VariantFieldAccess records that a member access (`x.field`) reads a named
// payload field of an enum variant the subject was narrowed to via `if x is V`
// (T0993). Codegen uses it to emit a variant-data GEP+load.
type VariantFieldAccess struct {
	TargetType  types.Type // the enum type (Enum or Instance)
	VariantName string     // the variant whose data layout to use
	FieldIndex  int        // index of the field within the variant payload
	FieldType   types.Type // resolved field type (substitution applied)
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
