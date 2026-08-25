package codegen

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/testutil"
	"github.com/promise-language/promise/compiler/internal/types"
)

// stdAll provides all builtin type declarations needed by tests.
// Loaded from the actual std/*.pr files to avoid duplication.
var stdAll string

var (
	codegenStdOnce    sync.Once
	codegenStdModInfo *sema.ModuleInfo
	codegenStdScope   *types.Scope
)

func init() {
	stdAll = testutil.LoadStdFiles()
}

func getCodegenStdModInfo() (*sema.ModuleInfo, *types.Scope) {
	codegenStdOnce.Do(func() {
		input := antlr.NewInputStream(stdAll)
		lexer := parser.NewPromiseLexer(input)
		lexer.RemoveErrorListeners()
		stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
		p := parser.NewPromiseParser(stream)
		p.RemoveErrorListeners()
		tree := p.CompilationUnit()
		stdFile, buildErrs := ast.Build("std.pr", tree)
		if len(buildErrs) > 0 {
			panic("std AST build errors: " + buildErrs[0].Error())
		}
		stdInfo, _ := sema.CheckWithTarget(stdFile, nil, sema.HostTargetInfo())
		codegenStdScope = sema.ExportedScope(stdInfo, stdFile)
		codegenStdModInfo = &sema.ModuleInfo{
			Name:           "std",
			CanonicalName:  "std",
			GlobalIdentity: "std",
			IRPrefix:       "std",
			File:           stdFile,
			SemaInfo:       stdInfo,
		}
	})
	return codegenStdModInfo, codegenStdScope
}

// parseWithStd parses user code, injects use std as _, and runs sema with the std module.
func parseWithStd(t *testing.T, src string) (*ast.File, *sema.Info) {
	t.Helper()

	stdModInfo, stdScope := getCodegenStdModInfo()

	// Parse user
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	file, errs := ast.Build("test.pr", tree)
	if len(errs) > 0 {
		t.Fatalf("AST build errors: %v", errs)
	}

	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	file.Uses = append([]*ast.UseDecl{stdUse}, file.Uses...)

	info, errs := sema.CheckWithModules(file, map[string]*types.Scope{"std": stdScope})
	if len(errs) > 0 {
		t.Fatalf("sema errors: %v", errs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{"std": stdModInfo}
	info.ModuleOrder = []string{"std"}
	return file, info
}

// generateIR runs the full pipeline: parse → sema → codegen, returns LLVM IR text.
func generateIR(t *testing.T, src string) string {
	t.Helper()
	file, info := parseWithStd(t, src)
	result := Compile(file, info, "")
	return result.Module.String()
}

// compileResult runs the full pipeline and returns the CompileResult.
func compileResult(t *testing.T, src string) *CompileResult {
	t.Helper()
	file, info := parseWithStd(t, src)
	return Compile(file, info, "")
}

func assertContains(t *testing.T, ir, substr string) {
	t.Helper()
	if !strings.Contains(ir, substr) {
		t.Errorf("expected IR to contain %q\ngot:\n%s", substr, ir)
	}
}

func assertContainsMatch(t *testing.T, ir, pattern string) {
	t.Helper()
	re := regexp.MustCompile(pattern)
	if !re.MatchString(ir) {
		t.Errorf("expected IR to match %q\ngot:\n%s", pattern, ir)
	}
}

func assertNotContains(t *testing.T, ir, substr string) {
	t.Helper()
	if strings.Contains(ir, substr) {
		t.Errorf("expected IR to NOT contain %q\ngot:\n%s", substr, ir)
	}
}

func assertNotContainsMatch(t *testing.T, ir, pattern string) {
	t.Helper()
	re := regexp.MustCompile(pattern)
	if re.MatchString(ir) {
		t.Errorf("expected IR to NOT match %q\ngot:\n%s", pattern, ir)
	}
}

// extractFunction returns the IR text for a named function (from "define" to the closing "}").
// extractGlobal returns the single-line `@name = ...` global definition, or ""
// if absent. Useful for asserting on the contents of a constant vtable global.
func extractGlobal(ir, name string) string {
	marker := "@" + name + " ="
	start := strings.Index(ir, marker)
	if start < 0 {
		// Globals with special characters are quoted: @"name = ...".
		marker = "@\"" + name + "\" ="
		start = strings.Index(ir, marker)
		if start < 0 {
			return ""
		}
	}
	end := strings.Index(ir[start:], "\n")
	if end < 0 {
		return ir[start:]
	}
	return ir[start : start+end]
}

func extractFunction(ir, name string) string {
	// Find "define ... @name("
	marker := "@" + name + "("
	start := strings.Index(ir, marker)
	if start < 0 {
		return ""
	}
	// Walk back to "define"
	lineStart := strings.LastIndex(ir[:start], "define")
	if lineStart < 0 {
		return ""
	}
	// Find closing "}\n" — LLVM IR functions end with "}\n" at column 0
	rest := ir[lineStart:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		return rest
	}
	return rest[:end+2]
}

// extractDefine returns the body of the `define ... @name(...)` *definition*,
// anchoring on the `define` keyword. Unlike extractFunction (which matches the
// first `@name(` anywhere), this is safe for argless functions like
// `.goroutine.main()` whose name also appears as a call operand inside @main —
// where extractFunction can latch onto the reference and extract @main instead.
func extractDefine(ir, name string) string {
	// LLVM quotes symbol names containing special characters (e.g.
	// `@".goroutine.Box[int].send_value.0"`), so accept both the bare and quoted
	// forms of the needle.
	needle := "@" + name + "("
	quotedNeedle := "@\"" + name + "\"("
	for idx := 0; ; {
		d := strings.Index(ir[idx:], "define")
		if d < 0 {
			return ""
		}
		d += idx
		nl := strings.Index(ir[d:], "\n")
		if nl < 0 {
			return ""
		}
		line := ir[d : d+nl]
		if strings.Contains(line, needle) || strings.Contains(line, quotedNeedle) {
			rest := ir[d:]
			end := strings.Index(rest, "\n}\n")
			if end < 0 {
				return rest
			}
			return rest[:end+2]
		}
		idx = d + len("define")
	}
}

// generateIRForTarget runs parse → sema → codegen with a specific target triple.
func generateIRForTarget(t *testing.T, src, target string) string {
	t.Helper()
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	tree := p.CompilationUnit()
	file, errs := ast.Build("test.pr", tree)
	if len(errs) > 0 {
		t.Fatalf("AST build errors: %v", errs)
	}

	stdModInfo, stdScope := getCodegenStdModInfo()
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	file.Uses = append([]*ast.UseDecl{stdUse}, file.Uses...)

	ti := sema.ParseTargetInfo(target)
	info, semaErrs := sema.CheckWithTarget(file, map[string]*types.Scope{"std": stdScope}, ti)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{"std": stdModInfo}
	info.ModuleOrder = []string{"std"}
	result := Compile(file, info, target)
	return result.Module.String()
}

// --- Stage 9: Std library and test runner codegen tests ---

// stdContainers is kept for backward compatibility with tests that pass it to generateIRWithStd.
// Its contents are already included in the real std module; pass "" and let generateIRWithStd ignore it.
const stdContainers = ""

// generateIRWithStd merges stdSrc (extra user-level declarations) with userSrc and generates IR.
// After the module-based refactor, stdSrc is treated as regular user code prepended to userSrc.
func generateIRWithStd(t *testing.T, stdSrc, userSrc string) string {
	t.Helper()
	combined := userSrc
	if stdSrc != "" {
		combined = stdSrc + "\n" + userSrc
	}
	return generateIR(t, combined)
}

// compileResultWithStd merges stdSrc (extra user-level declarations) with userSrc and compiles.
func compileResultWithStd(t *testing.T, stdSrc, userSrc string) *CompileResult {
	t.Helper()
	combined := userSrc
	if stdSrc != "" {
		combined = stdSrc + "\n" + userSrc
	}
	return compileResult(t, combined)
}

// --- Cross-Module Codegen Tests ---

// parseModuleSource parses a module source string, runs sema with the std module, and returns
// the ModuleInfo and exported scope.
func parseModuleSource(t *testing.T, moduleName, src string) (*sema.ModuleInfo, *types.Scope) {
	t.Helper()
	_, stdScope := getCodegenStdModInfo()

	// Parse module source
	modInput := antlr.NewInputStream(src)
	modLexer := parser.NewPromiseLexer(modInput)
	modLexer.RemoveErrorListeners()
	modStream := antlr.NewCommonTokenStream(modLexer, antlr.TokenDefaultChannel)
	modP := parser.NewPromiseParser(modStream)
	modP.RemoveErrorListeners()
	modTree := modP.CompilationUnit()
	modFile, errs := ast.Build("module.pr", modTree)
	if len(errs) > 0 {
		t.Fatalf("module AST build errors: %v", errs)
	}

	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	modFile.Uses = append([]*ast.UseDecl{stdUse}, modFile.Uses...)

	// Check against the host target, like getCodegenStdModInfo does: a real
	// catalog module may carry `target(...) overloads (modules/net's _eagain has
	// one per platform), and without target filtering they all get declared and
	// collide as redeclarations.
	modInfo, semaErrs := sema.CheckWithTarget(modFile,
		map[string]*types.Scope{"std": stdScope}, sema.HostTargetInfo())
	if len(semaErrs) > 0 {
		t.Fatalf("module sema errors: %v", semaErrs)
	}

	scope := sema.ExportedScope(modInfo, modFile)
	globalID := "./" + moduleName
	return &sema.ModuleInfo{
		Name:           moduleName,
		CanonicalName:  moduleName,
		GlobalIdentity: globalID,
		IRPrefix:       moduleName, // test convenience: use plain name as IR prefix
		Path:           globalID,
		File:           modFile,
		SemaInfo:       modInfo,
	}, scope
}

// generateIRWithModule parses a module and user source, runs sema+codegen with
// the module available via `use <moduleName>`.
func generateIRWithModule(t *testing.T, moduleName, modSrc, userSrc string) string {
	t.Helper()

	modInfo, modScope := parseModuleSource(t, moduleName, modSrc)
	stdModInfo, stdScope := getCodegenStdModInfo()

	// Parse user
	userInput := antlr.NewInputStream(userSrc)
	userLexer := parser.NewPromiseLexer(userInput)
	userLexer.RemoveErrorListeners()
	userStream := antlr.NewCommonTokenStream(userLexer, antlr.TokenDefaultChannel)
	userP := parser.NewPromiseParser(userStream)
	userP.RemoveErrorListeners()
	userTree := userP.CompilationUnit()
	userFile, errs := ast.Build("test.pr", userTree)
	if len(errs) > 0 {
		t.Fatalf("user AST build errors: %v", errs)
	}

	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	userFile.Uses = append([]*ast.UseDecl{stdUse}, userFile.Uses...)

	// Sema with std + module scopes
	modKey := "./" + moduleName
	moduleScopes := map[string]*types.Scope{
		"std":  stdScope,
		modKey: modScope,
	}
	info, semaErrs := sema.CheckWithModules(userFile, moduleScopes)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}

	// Attach module infos for codegen
	info.ModuleInfos = map[string]*sema.ModuleInfo{
		"std":  stdModInfo,
		modKey: modInfo,
	}
	info.ModuleOrder = []string{"std", modKey}

	result := Compile(userFile, info, "")
	return result.Module.String()
}

// generateIRWithTwoModules parses two modules and user source, runs sema+codegen
// with both modules available.
func generateIRWithTwoModules(t *testing.T,
	mod1Name, mod1Src, mod2Name, mod2Src, userSrc string) string {
	t.Helper()

	mod1Info, mod1Scope := parseModuleSource(t, mod1Name, mod1Src)
	mod2Info, mod2Scope := parseModuleSource(t, mod2Name, mod2Src)
	stdModInfo, stdScope := getCodegenStdModInfo()

	// Parse user
	userInput := antlr.NewInputStream(userSrc)
	userLexer := parser.NewPromiseLexer(userInput)
	userLexer.RemoveErrorListeners()
	userStream := antlr.NewCommonTokenStream(userLexer, antlr.TokenDefaultChannel)
	userP := parser.NewPromiseParser(userStream)
	userP.RemoveErrorListeners()
	userTree := userP.CompilationUnit()
	userFile, errs := ast.Build("test.pr", userTree)
	if len(errs) > 0 {
		t.Fatalf("user AST build errors: %v", errs)
	}

	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	userFile.Uses = append([]*ast.UseDecl{stdUse}, userFile.Uses...)

	mod1Key := "./" + mod1Name
	mod2Key := "./" + mod2Name
	moduleScopes := map[string]*types.Scope{
		"std":   stdScope,
		mod1Key: mod1Scope,
		mod2Key: mod2Scope,
	}
	info, semaErrs := sema.CheckWithModules(userFile, moduleScopes)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}

	info.ModuleInfos = map[string]*sema.ModuleInfo{
		"std":   stdModInfo,
		mod1Key: mod1Info,
		mod2Key: mod2Info,
	}
	info.ModuleOrder = []string{"std", mod1Key, mod2Key}

	result := Compile(userFile, info, "")
	return result.Module.String()
}

// --- Catalog Module Tests ---

// generateIRWithCatalogModule sets up a module with catalog identity (bare name as
// IRPrefix, keyed by catalog name in sema scopes) and compiles user source against it.
func generateIRWithCatalogModule(t *testing.T, catalogName, modSrc, userSrc string) string {
	t.Helper()
	modInfo, modScope := parseModuleSource(t, catalogName, modSrc)
	// Override identity to match catalog convention: bare name
	modInfo.GlobalIdentity = catalogName
	modInfo.IRPrefix = catalogName
	modInfo.Path = catalogName

	stdModInfo, stdScope := getCodegenStdModInfo()

	userInput := antlr.NewInputStream(userSrc)
	userLexer := parser.NewPromiseLexer(userInput)
	userLexer.RemoveErrorListeners()
	userStream := antlr.NewCommonTokenStream(userLexer, antlr.TokenDefaultChannel)
	userP := parser.NewPromiseParser(userStream)
	userP.RemoveErrorListeners()
	userTree := userP.CompilationUnit()
	userFile, errs := ast.Build("test.pr", userTree)
	if len(errs) > 0 {
		t.Fatalf("user AST build errors: %v", errs)
	}

	// Inject use std as _
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	userFile.Uses = append([]*ast.UseDecl{stdUse}, userFile.Uses...)

	// Catalog modules are keyed by their catalog name (not "./name")
	moduleScopes := map[string]*types.Scope{
		"std":       stdScope,
		catalogName: modScope,
	}
	info, semaErrs := sema.CheckWithModules(userFile, moduleScopes)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{
		"std":       stdModInfo,
		catalogName: modInfo,
	}
	info.ModuleOrder = []string{"std", catalogName}

	result := Compile(userFile, info, "")
	return result.Module.String()
}

// defBody extracts a single function *definition* body — from the line beginning
// with `marker` (a full `define <ret> @<name>(` prefix) up to its closing brace.
// Matching the full define prefix avoids matching a call to the same function.
func defBody(t *testing.T, ir, marker string) string {
	t.Helper()
	idx := strings.Index(ir, marker)
	if idx < 0 {
		t.Fatalf("expected a definition matching %q in the IR", marker)
	}
	body := ir[idx:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end+2]
	}
	return body
}

// goNewToEnqueue returns the IR slice of the spawn site — from the
// `promise_g_new` call to the following `promise_sched_enqueue` — where the
// via-block result buffer (if any) is allocated.
func goNewToEnqueue(t *testing.T, ir string) string {
	t.Helper()
	gNew := strings.Index(ir, "@promise_g_new")
	if gNew < 0 {
		t.Fatal("expected a promise_g_new call in the IR")
	}
	rest := ir[gNew:]
	enq := strings.Index(rest, "@promise_sched_enqueue")
	if enq < 0 {
		t.Fatal("expected a promise_sched_enqueue call after promise_g_new")
	}
	return rest[:enq]
}

// --- InstanceIRs, instanceOwnedFuncs, CompileWithCache tests ---

// mapKeys returns the keys of a string-keyed map, for diagnostics.
func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// boxWithGetMethod is a generic Box[T] type with a method that forces
// per-instance codegen for the method body.
const boxWithGetMethod = `
	type Box[T] {
		T value;
		get(this) T { return this.value; }
	}
`

// --- B0005: String constant private linkage tests ---
// All string constant globals must use LinkagePrivate so each split .bc file
// (module, instance) contains its own copy and doesn't depend on main-IR string
// numbering. This prevents stale cache entries from causing linker errors.

// compileResultWithModule parses a module and user source, runs sema+codegen,
// and returns the CompileResult (for SplitModuleIRs / InstanceIRs testing).
func compileResultWithModule(t *testing.T, moduleName, modSrc, userSrc string) *CompileResult {
	t.Helper()

	modInfo, modScope := parseModuleSource(t, moduleName, modSrc)
	stdModInfo, stdScope := getCodegenStdModInfo()

	userInput := antlr.NewInputStream(userSrc)
	userLexer := parser.NewPromiseLexer(userInput)
	userLexer.RemoveErrorListeners()
	userStream := antlr.NewCommonTokenStream(userLexer, antlr.TokenDefaultChannel)
	userP := parser.NewPromiseParser(userStream)
	userP.RemoveErrorListeners()
	userTree := userP.CompilationUnit()
	userFile, errs := ast.Build("test.pr", userTree)
	if len(errs) > 0 {
		t.Fatalf("user AST build errors: %v", errs)
	}

	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	userFile.Uses = append([]*ast.UseDecl{stdUse}, userFile.Uses...)

	modKey := "./" + moduleName
	moduleScopes := map[string]*types.Scope{
		"std":  stdScope,
		modKey: modScope,
	}
	info, semaErrs := sema.CheckWithModules(userFile, moduleScopes)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{
		"std":  stdModInfo,
		modKey: modInfo,
	}
	info.ModuleOrder = []string{"std", modKey}

	return Compile(userFile, info, "")
}

// extractFunc returns the IR text of a named function definition from full module IR.
// Returns empty string if function not found.
func extractFunc(ir, funcName string) string {
	// Search for "define" lines containing the function name
	needle := "@" + funcName + "("
	searchFrom := 0
	for searchFrom < len(ir) {
		idx := strings.Index(ir[searchFrom:], needle)
		if idx < 0 {
			return ""
		}
		idx += searchFrom
		// Walk back to find "define" on the same line
		lineStart := strings.LastIndex(ir[:idx], "\n")
		if lineStart < 0 {
			lineStart = 0
		}
		line := ir[lineStart:idx]
		if !strings.Contains(line, "define") {
			// This is a call site, not a definition — skip
			searchFrom = idx + len(needle)
			continue
		}
		start := lineStart
		if ir[start] == '\n' {
			start++
		}
		// Walk forward to find the closing "}" at depth 0
		rest := ir[start:]
		depth := 0
		for i, ch := range rest {
			if ch == '{' {
				depth++
			} else if ch == '}' {
				depth--
				if depth == 0 {
					return rest[:i+1]
				}
			}
		}
		return rest
	}
	return ""
}

// --- Coverage instrumentation tests (T0030) ---

// generateIRWithCoverage compiles with coverage enabled and returns the IR.
func generateIRWithCoverage(t *testing.T, src string) (string, []CoverageRegion) {
	t.Helper()
	file, info := parseWithStd(t, src)
	result := CompileWithOptions(file, info, "", &CompileOptions{CoverageEnabled: true})
	return result.Module.String(), result.CoverageRegions
}

// T1073: force-unwrap `o!` consumed at the collection-literal / raise / select-send
// sites must neutralize the source optional's present flag, exactly like the
// var-decl/assignment/call-arg sites. Otherwise both the source optional's
// scope-exit drop and the container/error-slot/channel free the moved inner →
// double-free (observed as a SIGSEGV). The neutralization GEPs the optional
// param's present field (field 0 of `{ i1, { i8*, i8* } }`) and stores false.
const t1073NeutralizeSig = "{ i1, { i8*, i8* } }* %o.addr, i32 0, i32 0"

// generateIRWithDependentModules parses mod1, then mod2 (which can import mod1),
// then user code (which can import both). Used for cross-module dependency tests.
func generateIRWithDependentModules(t *testing.T,
	mod1Name, mod1Src, mod2Name, mod2Src, userSrc string) string {
	t.Helper()

	mod1Info, mod1Scope := parseModuleSource(t, mod1Name, mod1Src)
	stdModInfo, stdScope := getCodegenStdModInfo()

	// Parse mod2 with mod1 in scope
	mod1Key := "./" + mod1Name
	mod2Input := antlr.NewInputStream(mod2Src)
	mod2Lexer := parser.NewPromiseLexer(mod2Input)
	mod2Lexer.RemoveErrorListeners()
	mod2Stream := antlr.NewCommonTokenStream(mod2Lexer, antlr.TokenDefaultChannel)
	mod2P := parser.NewPromiseParser(mod2Stream)
	mod2P.RemoveErrorListeners()
	mod2Tree := mod2P.CompilationUnit()
	mod2File, errs := ast.Build("module.pr", mod2Tree)
	if len(errs) > 0 {
		t.Fatalf("mod2 AST build errors: %v", errs)
	}
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	mod2File.Uses = append([]*ast.UseDecl{stdUse}, mod2File.Uses...)
	mod2Info2, semaErrs := sema.CheckWithModules(mod2File, map[string]*types.Scope{
		"std":   stdScope,
		mod1Key: mod1Scope,
	})
	if len(semaErrs) > 0 {
		t.Fatalf("mod2 sema errors: %v", semaErrs)
	}
	mod2Scope := sema.ExportedScope(mod2Info2, mod2File)
	mod2Key := "./" + mod2Name
	mod2Info := &sema.ModuleInfo{
		Name:           mod2Name,
		CanonicalName:  mod2Name,
		GlobalIdentity: mod2Key,
		IRPrefix:       mod2Name,
		Path:           mod2Key,
		File:           mod2File,
		SemaInfo:       mod2Info2,
	}

	// Parse user code
	userInput := antlr.NewInputStream(userSrc)
	userLexer := parser.NewPromiseLexer(userInput)
	userLexer.RemoveErrorListeners()
	userStream := antlr.NewCommonTokenStream(userLexer, antlr.TokenDefaultChannel)
	userP := parser.NewPromiseParser(userStream)
	userP.RemoveErrorListeners()
	userTree := userP.CompilationUnit()
	userFile, errs2 := ast.Build("test.pr", userTree)
	if len(errs2) > 0 {
		t.Fatalf("user AST build errors: %v", errs2)
	}
	userFile.Uses = append([]*ast.UseDecl{{Alias: "_", CatalogName: "std"}}, userFile.Uses...)

	userInfo, userSemaErrs := sema.CheckWithModules(userFile, map[string]*types.Scope{
		"std":   stdScope,
		mod1Key: mod1Scope,
		mod2Key: mod2Scope,
	})
	if len(userSemaErrs) > 0 {
		t.Fatalf("user sema errors: %v", userSemaErrs)
	}

	userInfo.ModuleInfos = map[string]*sema.ModuleInfo{
		"std":   stdModInfo,
		mod1Key: mod1Info,
		mod2Key: mod2Info,
	}
	userInfo.ModuleOrder = []string{"std", mod1Key, mod2Key}

	result := Compile(userFile, userInfo, "")
	return result.Module.String()
}

// findEnvDropContaining returns the body of the first .lambda.N.env_drop
// function whose body contains marker. There are many stdlib-generated
// env_drop functions; this helper picks out the one for the user code under
// test.
func findEnvDropContaining(ir, marker string) string {
	// Iterate over candidate lambda numbers — the user's lambda lands after
	// stdlib lambdas, so the range needs to be large enough.
	idx := 0
	for {
		needle := fmt.Sprintf(".lambda.%d.env_drop", idx)
		if !strings.Contains(ir, "@"+needle+"(") {
			if idx > 500 {
				return ""
			}
			idx++
			continue
		}
		body := extractFunction(ir, needle)
		if strings.Contains(body, marker) {
			return body
		}
		idx++
	}
}
