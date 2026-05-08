package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"djabi.dev/go/promise_lang/internal/ast"
	"djabi.dev/go/promise_lang/internal/module"
	"djabi.dev/go/promise_lang/internal/sema"
	"djabi.dev/go/promise_lang/internal/types"
)

// docOpts holds documentation generation options.
type docOpts struct {
	publicOnly bool // -public: show only exported symbols
	sigOnly    bool // -signatures: compact mode, no doc text
}

// runDoc implements the `promise doc` subcommand.
func runDoc(args []string) {
	var opts docOpts
	var outputFile string
	var remaining []string

	opts.publicOnly = true // default: public only

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-public":
			opts.publicOnly = true
		case "-all":
			opts.publicOnly = false
		case "-signatures":
			opts.sigOnly = true
		case "-o":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: -o requires an output path")
				os.Exit(1)
			}
			i++
			outputFile = args[i]
		default:
			remaining = append(remaining, args[i])
		}
	}

	if len(remaining) < 1 {
		fmt.Fprintln(os.Stderr, "usage: promise doc [options] <file.pr | module-name>")
		os.Exit(1)
	}

	var w io.Writer = os.Stdout
	if outputFile != "" {
		f, err := os.Create(outputFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot create %s: %v\n", outputFile, err)
			os.Exit(1)
		}
		defer f.Close()
		w = f
	}

	target := remaining[0]

	// If target looks like a .pr file, use the existing single-file path.
	// Otherwise, treat it as a module name (catalog or local directory).
	if strings.HasSuffix(target, ".pr") {
		file, info := docFrontend(target)
		emitDoc(w, file, info, opts, target)
		return
	}

	// Module documentation: look up in catalog or as a local directory
	runDocModule(w, target, opts)
}

// docFrontend runs parse + inject std + DeclareAndDefine (no Check/Verify/Ownership).
func docFrontend(filename string) (*ast.File, *sema.Info) {
	file := parseSourceFile(filename)
	file = injectStdImport(file)

	// Use host target so target-filtered functions (e.g., platform.pr) are
	// properly filtered instead of causing redeclaration errors.
	target := sema.HostTargetInfo()
	moduleScopes, _, _ := loadModuleScopes(filename, file, target)
	info, errs := sema.DeclareAndDefineWithTarget(file, moduleScopes, target)
	if len(errs) > 0 {
		printFileErrors(filename, errs)
		os.Exit(1)
	}
	return file, info
}

// runDocModule generates documentation for a catalog module or local module directory.
func runDocModule(w io.Writer, name string, opts docOpts) {
	// Try catalog first
	var modDir string

	if len(embeddedCatalog) > 0 {
		cat, err := module.ParseCatalog(embeddedCatalog)
		if err == nil {
			if entry := cat.Lookup(name); entry != nil {
				if entry.IsEmbedded() {
					dir, err := extractEmbeddedModule(name)
					if err != nil {
						fmt.Fprintf(os.Stderr, "error: cannot extract module '%s': %v\n", name, err)
						os.Exit(1)
					}
					modDir = dir
				} else {
					dir, err := module.ResolveRemoteModule(entry.URL, entry.Commit)
					if err != nil {
						fmt.Fprintf(os.Stderr, "error: cannot fetch module '%s': %v\n", name, err)
						os.Exit(1)
					}
					modDir = dir
				}
			}
		}
	}

	// If not found in catalog, try as a local directory
	if modDir == "" {
		if info, err := os.Stat(name); err == nil && info.IsDir() {
			modDir = name
		} else {
			fmt.Fprintf(os.Stderr, "error: '%s' is not a known catalog module or directory\n", name)
			os.Exit(1)
		}
	}

	// Find all .pr source files (excluding test files)
	entries, err := os.ReadDir(modDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot read module directory: %v\n", err)
		os.Exit(1)
	}

	var sourceFiles []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".pr") && !strings.HasSuffix(e.Name(), "_test.pr") {
			sourceFiles = append(sourceFiles, filepath.Join(modDir, e.Name()))
		}
	}

	if len(sourceFiles) == 0 {
		fmt.Fprintf(w, "# %s\n\nNo public declarations.\n", name)
		return
	}

	sort.Strings(sourceFiles)

	// Module heading
	fmt.Fprintf(w, "# %s\n", name)

	// Collect declarations from all files
	type typeEntry struct {
		decl  *ast.TypeDecl
		named *types.Named
		info  *sema.Info
		scope *types.Scope
	}
	type enumEntry struct {
		decl *ast.EnumDecl
		enum *types.Enum
		info *sema.Info
	}
	type funcEntry struct {
		decl *ast.FuncDecl
		fn   *types.Func
		info *sema.Info
	}

	var allTypes []typeEntry
	var allEnums []enumEntry
	var allFuncs []funcEntry

	for _, sf := range sourceFiles {
		file, info := docFrontend(sf)
		fileScope := info.Scopes[file]
		if fileScope == nil {
			continue
		}

		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.TypeDecl:
				named := lookupNamed(d.Name, fileScope, info)
				if named == nil {
					continue
				}
				if opts.publicOnly && !named.IsExported() {
					continue
				}
				allTypes = append(allTypes, typeEntry{d, named, info, fileScope})
			case *ast.EnumDecl:
				enum := lookupEnum(d.Name, fileScope)
				if enum == nil {
					continue
				}
				if opts.publicOnly && !enum.IsExported() {
					continue
				}
				allEnums = append(allEnums, enumEntry{d, enum, info})
			case *ast.FuncDecl:
				fn := lookupFuncDecl(d, fileScope)
				if fn == nil {
					continue
				}
				if fn.IsTest() || d.Name == "main" {
					continue
				}
				if opts.publicOnly && !fn.IsExported() {
					continue
				}
				allFuncs = append(allFuncs, funcEntry{d, fn, info})
			}
		}
	}

	// Emit grouped sections
	if len(allTypes) > 0 {
		if !opts.sigOnly {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "## Types")
		}
		for _, e := range allTypes {
			fmt.Fprintln(w)
			emitType(w, e.decl, e.named, e.info, opts, e.scope)
		}
	}

	if len(allEnums) > 0 {
		if !opts.sigOnly {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "## Enums")
		}
		for _, e := range allEnums {
			fmt.Fprintln(w)
			emitEnum(w, e.decl, e.enum, e.info, opts)
		}
	}

	if len(allFuncs) > 0 {
		if !opts.sigOnly {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "## Functions")
		}
		for _, e := range allFuncs {
			fmt.Fprintln(w)
			emitFunc(w, e.decl, e.fn, e.info, opts)
		}
	}
}

// emitModuleFileDoc generates documentation for a single file within a module.
// Unlike emitDoc, it omits the file heading and merges into the module-level output.
func emitModuleFileDoc(w io.Writer, file *ast.File, info *sema.Info, opts docOpts) {
	fileScope := info.Scopes[file]
	if fileScope == nil {
		return
	}

	var typeDecls []*ast.TypeDecl
	var enumDecls []*ast.EnumDecl
	var funcDecls []*ast.FuncDecl

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.TypeDecl:
			if opts.publicOnly {
				named := lookupNamed(d.Name, fileScope, info)
				if named == nil || !named.IsExported() {
					continue
				}
			}
			typeDecls = append(typeDecls, d)
		case *ast.EnumDecl:
			if opts.publicOnly {
				enum := lookupEnum(d.Name, fileScope)
				if enum == nil || !enum.IsExported() {
					continue
				}
			}
			enumDecls = append(enumDecls, d)
		case *ast.FuncDecl:
			fn := lookupFuncDecl(d, fileScope)
			if fn == nil {
				continue
			}
			if fn.IsTest() || d.Name == "main" {
				continue
			}
			if opts.publicOnly && !fn.IsExported() {
				continue
			}
			funcDecls = append(funcDecls, d)
		}
	}

	if len(typeDecls) > 0 {
		if !opts.sigOnly {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "## Types")
		}
		for _, d := range typeDecls {
			fmt.Fprintln(w)
			named := lookupNamed(d.Name, fileScope, info)
			if named == nil {
				continue
			}
			emitType(w, d, named, info, opts, fileScope)
		}
	}

	if len(enumDecls) > 0 {
		if !opts.sigOnly {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "## Enums")
		}
		for _, d := range enumDecls {
			fmt.Fprintln(w)
			enum := lookupEnum(d.Name, fileScope)
			if enum == nil {
				continue
			}
			emitEnum(w, d, enum, info, opts)
		}
	}

	if len(funcDecls) > 0 {
		if !opts.sigOnly {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "## Functions")
		}
		for _, d := range funcDecls {
			fmt.Fprintln(w)
			fn := lookupFuncDecl(d, fileScope)
			if fn == nil {
				continue
			}
			emitFunc(w, d, fn, info, opts)
		}
	}
}

// emitDoc generates markdown documentation for a single file.
func emitDoc(w io.Writer, file *ast.File, info *sema.Info, opts docOpts, filename string) {
	// File heading
	base := filepath.Base(filename)
	fmt.Fprintf(w, "# %s\n", base)

	emitModuleFileDoc(w, file, info, opts)
}

// --- Type documentation ---

func emitType(w io.Writer, d *ast.TypeDecl, named *types.Named, info *sema.Info, opts docOpts, scope *types.Scope) {
	if opts.sigOnly {
		emitTypeSummary(w, d, named, info, opts)
		return
	}

	// Heading: ### TypeName is Parent1, Parent2
	heading := "### " + d.Name
	if len(named.Parents()) > 0 {
		heading += " is " + formatParents(named)
	}
	if named.IsStructural() {
		heading += " `structural"
	}
	if named.IsCopy() {
		heading += " `copy"
	}
	if named.Deprecated() != "" {
		heading += " DEPRECATED"
		if msg := strings.TrimSpace(named.Deprecated()); msg != "" {
			heading += fmt.Sprintf("(%q)", msg)
		}
	}
	fmt.Fprintln(w, heading)

	// Doc string
	if named.Doc() != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, named.Doc())
	}

	// Structural note
	if named.IsStructural() {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Structural interface — types satisfy this by implementing the required methods,")
		fmt.Fprintln(w, "without an explicit `is` declaration.")
	}

	// Summary block
	fmt.Fprintln(w)
	emitTypeSummary(w, d, named, info, opts)

	// Operators line
	emitOperators(w, named, opts)

	// Per-method sections
	emitMethodSections(w, d.Name, named, info, opts)

	fmt.Fprintln(w)
	fmt.Fprintln(w, "---")
}

func emitTypeSummary(w io.Writer, d *ast.TypeDecl, named *types.Named, info *sema.Info, opts docOpts) {
	// Opening line
	line := "    type " + d.Name
	if len(named.TypeParams()) > 0 {
		line += formatTypeParams(named.TypeParams())
	}
	if len(named.Parents()) > 0 {
		line += " is " + formatParents(named)
	}
	if named.IsStructural() {
		line += " `structural"
	}
	if named.IsCopy() {
		line += " `copy"
	}
	fmt.Fprintln(w, line+" {")

	// Build field→default map from AST (DeclareAndDefine skips pass 3
	// which populates info.FieldDefaults, so we read defaults directly).
	astFieldDefaults := make(map[string]ast.Expr)
	for _, fd := range d.Fields {
		if fd.Default != nil {
			astFieldDefaults[fd.Name] = fd.Default
		}
	}

	// Fields
	hasFields := false
	for _, f := range named.Fields() {
		if opts.publicOnly && !isMemberPublic(f.Name(), f.IsExported()) {
			continue
		}
		fieldLine := "        " + typeString(f.Type()) + " " + f.Name()
		if f.HasDefault() {
			if expr, ok := astFieldDefaults[f.Name()]; ok {
				fieldLine += " = " + exprToString(expr)
			}
		}
		if f.Placement() == types.PlaceValue {
			fieldLine += " `value"
		}
		if f.IsFinal() {
			fieldLine += " `final"
		}
		if f.Doc() != "" {
			fieldLine += "    — " + f.Doc()
		}
		fmt.Fprintln(w, fieldLine)
		hasFields = true
	}

	// Blank line between fields and methods
	methods := collectMethods(named, opts)
	if hasFields && len(methods) > 0 {
		fmt.Fprintln(w)
	}

	// Method signatures
	for _, m := range methods {
		fmt.Fprintf(w, "        %s\n", formatMethodSig(m, info))
	}

	fmt.Fprintln(w, "    }")
}

func emitOperators(w io.Writer, named *types.Named, opts docOpts) {
	seen := make(map[string]bool)
	var ops []string
	for _, m := range named.Methods() {
		if opts.publicOnly && !isMemberPublic(m.Name(), m.IsExported()) {
			continue
		}
		name := m.Name()
		if isOperatorName(name) && !isSubscriptOp(name) && !seen[name] {
			seen[name] = true
			ops = append(ops, name)
		}
	}
	if len(ops) > 0 {
		fmt.Fprintf(w, "\n    Operators: %s\n", strings.Join(ops, ", "))
	}
}

func emitMethodSections(w io.Writer, typeName string, named *types.Named, info *sema.Info, opts docOpts) {
	for _, m := range collectMethods(named, opts) {
		// Skip operators — they're shown in the operators line
		if isOperatorName(m.Name()) {
			continue
		}
		// Only emit per-method section if method has doc or documented params
		if m.Doc() == "" && !hasParamDocs(m.Sig()) {
			continue
		}
		emitMethodSection(w, typeName, m, info)
	}
}

func emitMethodSection(w io.Writer, typeName string, m *types.Method, info *sema.Info) {
	// Heading
	heading := "#### " + typeName + "." + m.Name()
	if m.IsGetter() {
		heading += " (getter)"
	}
	if m.IsFactory() {
		heading += " `factory"
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, heading)

	// Doc string
	doc := m.Doc()
	if m.Name() == "drop" {
		suffix := "Called automatically at scope exit."
		if doc != "" {
			doc += " " + suffix
		} else {
			doc = suffix
		}
	}
	if doc != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, doc)
	}

	// Signature
	fmt.Fprintf(w, "\n    %s\n", formatMethodSig(m, info))

	// Parameter docs
	emitParamDocs(w, m.Sig())
}

// --- Enum documentation ---

func emitEnum(w io.Writer, d *ast.EnumDecl, enum *types.Enum, info *sema.Info, opts docOpts) {
	if opts.sigOnly {
		emitEnumCompact(w, d, enum)
		return
	}

	// Heading
	heading := "### " + d.Name
	if len(enum.TypeParams()) > 0 {
		heading += formatTypeParams(enum.TypeParams())
	}
	if enum.IsCopy() {
		heading += " `copy"
	}
	if enum.Deprecated() != "" {
		heading += " DEPRECATED"
		if msg := strings.TrimSpace(enum.Deprecated()); msg != "" {
			heading += fmt.Sprintf("(%q)", msg)
		}
	}
	fmt.Fprintln(w, heading)

	// Doc string
	if enum.Doc() != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, enum.Doc())
	}

	// Variants
	allFlat := true
	for _, v := range enum.Variants() {
		if v.NumFields() > 0 || v.Doc() != "" {
			allFlat = false
			break
		}
	}

	if allFlat {
		// Inline: Variants: GET, POST, ...
		names := make([]string, len(enum.Variants()))
		for i, v := range enum.Variants() {
			names[i] = "`" + v.Name() + "`"
		}
		fmt.Fprintf(w, "\nVariants: %s\n", strings.Join(names, ", "))
	} else {
		// Bullet list
		fmt.Fprintln(w, "\nVariants:")
		for _, v := range enum.Variants() {
			line := "- `" + v.String() + "`"
			if v.Doc() != "" {
				line += " — " + v.Doc()
			}
			fmt.Fprintln(w, line)
		}
	}

	// Enum methods
	methods := collectEnumMethods(enum, opts)
	for _, m := range methods {
		if m.Doc() == "" && !hasParamDocs(m.Sig()) {
			continue
		}
		emitMethodSection(w, d.Name, m, info)
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "---")
}

func emitEnumCompact(w io.Writer, d *ast.EnumDecl, enum *types.Enum) {
	// Check if all flat
	allFlat := true
	for _, v := range enum.Variants() {
		if v.NumFields() > 0 {
			allFlat = false
			break
		}
	}

	line := "enum " + d.Name
	if len(enum.TypeParams()) > 0 {
		line += formatTypeParams(enum.TypeParams())
	}
	if enum.IsCopy() {
		line += " `copy"
	}

	if allFlat {
		names := make([]string, len(enum.Variants()))
		for i, v := range enum.Variants() {
			names[i] = v.Name()
		}
		fmt.Fprintf(w, "    %s { %s }\n", line, strings.Join(names, ", "))
	} else {
		variants := make([]string, len(enum.Variants()))
		for i, v := range enum.Variants() {
			variants[i] = v.String()
		}
		fmt.Fprintf(w, "    %s { %s }\n", line, strings.Join(variants, ", "))
	}
}

// --- Function documentation ---

func emitFunc(w io.Writer, d *ast.FuncDecl, fn *types.Func, info *sema.Info, opts docOpts) {
	sig := fn.Type().(*types.Signature)

	if opts.sigOnly {
		fmt.Fprintf(w, "    %s\n", formatFuncSig(d.Name, sig, fn, info))
		return
	}

	// Heading
	heading := "### " + d.Name
	if len(sig.TypeParams()) > 0 {
		heading += formatTypeParams(sig.TypeParams())
	}
	if fn.IsGetter() {
		heading += " (getter)"
	}
	if fn.Deprecated() != "" {
		heading += " DEPRECATED"
		if msg := strings.TrimSpace(fn.Deprecated()); msg != "" {
			heading += fmt.Sprintf("(%q)", msg)
		}
	}
	fmt.Fprintln(w, heading)

	// Doc string
	if fn.Doc() != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, fn.Doc())
	}

	// Signature
	fmt.Fprintf(w, "\n    %s\n", formatFuncSig(d.Name, sig, fn, info))

	// Parameter docs
	if !fn.IsGetter() {
		emitParamDocs(w, sig)
	}
}

// --- Formatting helpers ---

func formatMethodSig(m *types.Method, info *sema.Info) string {
	var b strings.Builder

	if m.IsGetter() {
		b.WriteString("get ")
		b.WriteString(m.Name())
		if m.Sig().CanError() {
			b.WriteByte('!')
		}
		if m.Sig().Result() != nil {
			b.WriteByte(' ')
			b.WriteString(typeString(m.Sig().Result()))
		}
		return b.String()
	}

	if m.IsSetter() {
		b.WriteString("set ")
	}

	b.WriteString(m.Name())
	if m.Sig().CanError() {
		b.WriteByte('!')
	}
	b.WriteByte('(')

	first := true
	// Receiver
	if recv := m.Sig().Recv(); recv != nil {
		if recv.Ref() != types.RefNone {
			b.WriteString(recv.Ref().String())
		}
		b.WriteString("this")
		first = false
	}

	// Parameters
	for _, p := range m.Sig().Params() {
		if !first {
			b.WriteString(", ")
		}
		if p.IsVariadic() {
			b.WriteString("...")
			if elem, ok := types.AsVector(p.Type()); ok {
				b.WriteString(typeString(elem))
			} else {
				b.WriteString(typeString(p.Type()))
			}
		} else {
			b.WriteString(typeString(p.Type()))
		}
		b.WriteByte(' ')
		b.WriteString(p.Name())
		if p.HasDefault() {
			if expr, ok := info.ParamDefaults[p]; ok {
				b.WriteString(" = ")
				b.WriteString(exprToString(expr))
			}
		}
		first = false
	}

	b.WriteByte(')')

	// Return type
	if m.Sig().Result() != nil {
		b.WriteByte(' ')
		b.WriteString(typeString(m.Sig().Result()))
	}

	// Tags
	if m.IsAbstract() {
		b.WriteString(" `abstract")
	}
	if m.IsFactory() {
		b.WriteString(" `factory")
	}

	return b.String()
}

func formatFuncSig(name string, sig *types.Signature, fn *types.Func, info *sema.Info) string {
	var b strings.Builder

	if fn.IsGetter() {
		b.WriteString("get ")
		b.WriteString(name)
		if sig.CanError() {
			b.WriteByte('!')
		}
		if sig.Result() != nil {
			b.WriteByte(' ')
			b.WriteString(typeString(sig.Result()))
		}
		return b.String()
	}

	if fn.IsSetter() {
		b.WriteString("set ")
	}

	b.WriteString(name)
	if sig.CanError() {
		b.WriteByte('!')
	}
	if len(sig.TypeParams()) > 0 {
		b.WriteString(formatTypeParams(sig.TypeParams()))
	}
	b.WriteByte('(')

	for i, p := range sig.Params() {
		if i > 0 {
			b.WriteString(", ")
		}
		if p.IsVariadic() {
			b.WriteString("...")
			if elem, ok := types.AsVector(p.Type()); ok {
				b.WriteString(typeString(elem))
			} else {
				b.WriteString(typeString(p.Type()))
			}
		} else {
			b.WriteString(typeString(p.Type()))
		}
		b.WriteByte(' ')
		if p.Ref() != types.RefNone {
			b.WriteString(p.Ref().String())
		}
		b.WriteString(p.Name())
		if p.HasDefault() {
			if expr, ok := info.ParamDefaults[p]; ok {
				b.WriteString(" = ")
				b.WriteString(exprToString(expr))
			}
		}
	}

	b.WriteByte(')')

	if sig.Result() != nil {
		b.WriteByte(' ')
		b.WriteString(typeString(sig.Result()))
	}

	return b.String()
}

func formatTypeParams(tps []*types.TypeParam) string {
	if len(tps) == 0 {
		return ""
	}
	var parts []string
	for _, tp := range tps {
		s := tp.Obj().Name()
		if cs := tp.Constraints(); len(cs) > 0 {
			cnames := make([]string, len(cs))
			for i, c := range cs {
				cnames[i] = typeString(c)
			}
			s += ": " + strings.Join(cnames, " + ")
		}
		parts = append(parts, s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func formatParents(named *types.Named) string {
	parents := named.Parents()
	names := make([]string, len(parents))
	for i, p := range parents {
		names[i] = p.Named.Obj().Name()
	}
	return strings.Join(names, ", ")
}

func emitParamDocs(w io.Writer, sig *types.Signature) {
	if !hasParamDocs(sig) {
		return
	}
	fmt.Fprintln(w, "\nParameters:")
	for _, p := range sig.Params() {
		if p.Doc() != "" {
			fmt.Fprintf(w, "- `%s` — %s\n", p.Name(), p.Doc())
		}
	}
}

func hasParamDocs(sig *types.Signature) bool {
	for _, p := range sig.Params() {
		if p.Doc() != "" {
			return true
		}
	}
	return false
}

// --- Collection helpers ---

// isMemberPublic returns true if a member (method or field) should be shown
// in public-only mode. Members of a public type are public by default —
// only names starting with '_' are considered private. Explicit `public
// annotation also makes a member visible.
func isMemberPublic(name string, exported bool) bool {
	if exported {
		return true
	}
	// Members of a public type are public unless underscore-prefixed.
	// Operators are always public (they can't start with _).
	return !strings.HasPrefix(name, "_")
}

func collectMethods(named *types.Named, opts docOpts) []*types.Method {
	// For structural interfaces, show all methods (they define the interface contract)
	if named.IsStructural() {
		return named.Methods()
	}

	// Collect own methods
	seen := make(map[string]bool)
	var result []*types.Method
	for _, m := range named.Methods() {
		if opts.publicOnly && !isMemberPublic(m.Name(), m.IsExported()) {
			continue
		}
		seen[m.Name()] = true
		result = append(result, m)
	}

	// Add inherited drop if not overridden (agents need to know about drop for ownership)
	if !seen["drop"] {
		for _, pr := range named.Parents() {
			if dm := pr.Named.LookupMethod("drop"); dm != nil {
				result = append(result, dm)
				break
			}
		}
	}

	return result
}

func collectEnumMethods(enum *types.Enum, opts docOpts) []*types.Method {
	var result []*types.Method
	for _, m := range enum.Methods() {
		if opts.publicOnly && !isMemberPublic(m.Name(), m.IsExported()) {
			continue
		}
		result = append(result, m)
	}
	return result
}

// --- Lookup helpers ---

func lookupNamed(name string, scope *types.Scope, info *sema.Info) *types.Named {
	obj := scope.Lookup(name)
	if obj == nil {
		return nil
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return nil
	}
	named, _ := tn.Type().(*types.Named)
	return named
}

func lookupEnum(name string, scope *types.Scope) *types.Enum {
	obj := scope.Lookup(name)
	if obj == nil {
		return nil
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return nil
	}
	enum, _ := tn.Type().(*types.Enum)
	return enum
}

func lookupFunc(name string, scope *types.Scope) *types.Func {
	obj := scope.Lookup(name)
	if obj == nil {
		return nil
	}
	fn, _ := obj.(*types.Func)
	return fn
}

// lookupFuncDecl looks up a FuncDecl in the scope, handling the $set suffix for setters.
func lookupFuncDecl(d *ast.FuncDecl, scope *types.Scope) *types.Func {
	name := d.Name
	if d.IsSetter {
		name = d.Name + "$set"
	}
	return lookupFunc(name, scope)
}

// --- Utility helpers ---

// typeString returns a human-readable string for a type.
func typeString(t types.Type) string {
	if t == nil {
		return "void"
	}
	return t.String()
}

// exprToString converts an AST expression to source-like text for default values.
func exprToString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.IntLit:
		return e.Raw + e.Suffix
	case *ast.FloatLit:
		return e.Raw + e.Suffix
	case *ast.BoolLit:
		if e.Value {
			return "true"
		}
		return "false"
	case *ast.StringLit:
		return e.Raw
	case *ast.IdentExpr:
		return e.Name
	case *ast.NoneLit:
		return "none"
	case *ast.UnaryExpr:
		return e.Op.String() + exprToString(e.Operand)
	default:
		return "..."
	}
}

// isOperatorName returns true for operator method names.
func isOperatorName(name string) bool {
	switch name {
	case "==", "!=", "<", ">", "<=", ">=",
		"+", "-", "*", "/", "%",
		"&", "|", "^", "<<", ">>", "~",
		"&&", "||", "!",
		"++", "--",
		"..", "..=",
		"[]", "[]=", "[:]", "[:]=":
		return true
	}
	return false
}

// isSubscriptOp returns true for subscript/slice operators (shown in summary, not operators line).
func isSubscriptOp(name string) bool {
	switch name {
	case "[]", "[]=", "[:]", "[:]=":
		return true
	}
	return false
}
