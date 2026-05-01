package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"djabi.dev/go/promise_lang/internal/ast"
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
		fmt.Fprintln(os.Stderr, "usage: promise doc [options] <file.pr>")
		os.Exit(1)
	}

	filename := remaining[0]
	file, info := docFrontend(filename)

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

	emitDoc(w, file, info, opts, filename)
}

// docFrontend runs parse + merge std + DeclareAndDefine (no Check/Verify/Ownership).
func docFrontend(filename string) (*ast.File, *sema.Info) {
	file := parseSourceFile(filename)

	stdDir := findStdDir()
	var stdFiles []*ast.File
	if stdDir != "" {
		stdFiles = parseStdFiles(stdDir)
		file = mergeStdDecls(file, stdFiles)
	}

	moduleScopes, _, _ := loadModuleScopes(filename, file, stdFiles)
	info, errs := sema.DeclareAndDefineWithModules(file, moduleScopes)
	if len(errs) > 0 {
		printFileErrors(filename, errs)
		os.Exit(1)
	}
	return file, info
}

// emitDoc generates markdown documentation for a single file.
func emitDoc(w io.Writer, file *ast.File, info *sema.Info, opts docOpts, filename string) {
	fileScope := info.Scopes[file]
	if fileScope == nil {
		return
	}

	// Collect non-std declarations grouped by category, in source order
	var typeDecls []*ast.TypeDecl
	var enumDecls []*ast.EnumDecl
	var funcDecls []*ast.FuncDecl

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.TypeDecl:
			if d.IsStd {
				continue
			}
			if opts.publicOnly {
				named := lookupNamed(d.Name, fileScope, info)
				if named == nil || !named.IsExported() {
					continue
				}
			}
			typeDecls = append(typeDecls, d)
		case *ast.EnumDecl:
			if d.IsStd {
				continue
			}
			if opts.publicOnly {
				enum := lookupEnum(d.Name, fileScope)
				if enum == nil || !enum.IsExported() {
					continue
				}
			}
			enumDecls = append(enumDecls, d)
		case *ast.FuncDecl:
			if d.IsStd {
				continue
			}
			fn := lookupFunc(d.Name, fileScope)
			if fn == nil {
				continue
			}
			// Skip test functions and main
			if fn.IsTest() || d.Name == "main" {
				continue
			}
			if opts.publicOnly && !fn.IsExported() {
				continue
			}
			funcDecls = append(funcDecls, d)
		}
	}

	// File heading
	base := filepath.Base(filename)
	fmt.Fprintf(w, "# %s\n", base)

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
			fn := lookupFunc(d.Name, fileScope)
			if fn == nil {
				continue
			}
			emitFunc(w, d, fn, info, opts)
		}
	}
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
		if opts.publicOnly && !f.IsExported() {
			continue
		}
		fieldLine := "        " + typeString(f.Type()) + " " + f.Name()
		if f.HasDefault() {
			if expr, ok := astFieldDefaults[f.Name()]; ok {
				fieldLine += " = " + exprToString(expr)
			}
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
	var ops []string
	for _, m := range named.Methods() {
		if opts.publicOnly && !m.IsExported() {
			continue
		}
		name := m.Name()
		if isOperatorName(name) && !isSubscriptOp(name) {
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
		fmt.Fprintf(w, "    %s\n", formatFuncSig(d.Name, sig, info))
		return
	}

	// Heading
	heading := "### " + d.Name
	if len(sig.TypeParams()) > 0 {
		heading += formatTypeParams(sig.TypeParams())
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
	fmt.Fprintf(w, "\n    %s\n", formatFuncSig(d.Name, sig, info))

	// Parameter docs
	emitParamDocs(w, sig)
}

// --- Formatting helpers ---

func formatMethodSig(m *types.Method, info *sema.Info) string {
	var b strings.Builder

	if m.IsGetter() {
		b.WriteString("get ")
		b.WriteString(m.Name())
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
		b.WriteString(typeString(p.Type()))
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
	if m.Sig().CanError() {
		b.WriteByte('!')
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

func formatFuncSig(name string, sig *types.Signature, info *sema.Info) string {
	var b strings.Builder

	b.WriteString(name)
	if len(sig.TypeParams()) > 0 {
		b.WriteString(formatTypeParams(sig.TypeParams()))
	}
	b.WriteByte('(')

	for i, p := range sig.Params() {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(typeString(p.Type()))
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
	if sig.CanError() {
		b.WriteByte('!')
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
		names[i] = p.Obj().Name()
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

func collectMethods(named *types.Named, opts docOpts) []*types.Method {
	// For structural interfaces, show all methods (they define the interface contract)
	if named.IsStructural() {
		return named.Methods()
	}

	// Collect own methods
	seen := make(map[string]bool)
	var result []*types.Method
	for _, m := range named.Methods() {
		if opts.publicOnly && !m.IsExported() {
			continue
		}
		seen[m.Name()] = true
		result = append(result, m)
	}

	// Add inherited drop if not overridden (agents need to know about drop for ownership)
	if !seen["drop"] {
		for _, p := range named.Parents() {
			if dm := p.LookupMethod("drop"); dm != nil {
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
		if opts.publicOnly && !m.IsExported() {
			continue
		}
		result = append(result, m)
	}
	return result
}

// --- Lookup helpers ---

func lookupNamed(name string, scope *types.Scope, info *sema.Info) *types.Named {
	obj := scope.Lookup(name)
	if obj == nil && info.StdScope != nil {
		obj = info.StdScope.Lookup(name)
	}
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
		return e.Raw
	case *ast.FloatLit:
		return e.Raw
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
		"&", "|", "^", "<<", ">>",
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
