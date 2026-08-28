// Package codegentest holds the test helpers shared by codegen's per-area test
// packages: the std module is parsed and checked once per process here, and the
// IR-shape assertions live here so every area package spells them the same way.
//
// It exists because codegen's tests are split across packages (T1776). They
// cannot all live in package codegen: 3041 tests in one package ran serially
// for eight minutes on this machine and twenty on a slower one, and they cannot
// be parallelised in-process either — every test compiles the std module again
// and codegen writes into the shared std sema.Info while doing so, so two
// concurrent tests trip "concurrent map read and map write". Separate packages
// are separate processes with separate state, which also restores per-area
// build caching and per-area progress output.
//
// Only packages outside codegen may import this one: it imports codegen, so an
// in-package test file importing it would be an import cycle. The handful of
// tests that reach into codegen's unexported internals therefore stay in
// package codegen and keep private copies of the few helpers they need.
package codegentest

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	antlr "github.com/antlr4-go/antlr/v4"
	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/codegen"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/testutil"
	"github.com/promise-language/promise/compiler/internal/types"
)

// StdAll provides all builtin type declarations tests need, loaded from the
// real std/*.pr sources rather than duplicated here.
var StdAll = testutil.LoadStdFiles()

func AssertContains(t *testing.T, ir, substr string) {
	t.Helper()
	if !strings.Contains(ir, substr) {
		t.Errorf("expected IR to contain %q\ngot:\n%s", substr, ir)
	}
}

func AssertContainsMatch(t *testing.T, ir, pattern string) {
	t.Helper()
	re := regexp.MustCompile(pattern)
	if !re.MatchString(ir) {
		t.Errorf("expected IR to match %q\ngot:\n%s", pattern, ir)
	}
}

func AssertNotContains(t *testing.T, ir, substr string) {
	t.Helper()
	if strings.Contains(ir, substr) {
		t.Errorf("expected IR to NOT contain %q\ngot:\n%s", substr, ir)
	}
}

func AssertNotContainsMatch(t *testing.T, ir, pattern string) {
	t.Helper()
	re := regexp.MustCompile(pattern)
	if re.MatchString(ir) {
		t.Errorf("expected IR to NOT match %q\ngot:\n%s", pattern, ir)
	}
}

// BlockByPrefixT0638 returns the basic block whose label line starts with
// prefix (e.g. "arridx.ok"), from the label line up to the next blank line.
// Searches for the label *definition* ("\n<prefix>...:") so it doesn't match
// "label %<prefix>" branch references.
func BlockByPrefixT0638(body, prefix string) string {
	idx := strings.Index(body, "\n"+prefix)
	if idx < 0 {
		return ""
	}
	rest := body[idx+1:]
	if end := strings.Index(rest, "\n\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// BoxWithGetMethod is a generic Box[T] type with a method that forces
// per-instance codegen for the method body.
const BoxWithGetMethod = `
	type Box[T] {
		T value;
		get(this) T { return this.value; }
	}
`

var (
	CodegenStdOnce    sync.Once
	CodegenStdModInfo *sema.ModuleInfo
	CodegenStdScope   *types.Scope
)

// CompileResult runs the full pipeline and returns the CompileResult.
func CompileResult(t *testing.T, src string) *codegen.CompileResult {
	t.Helper()
	file, info := ParseWithStd(t, src)
	return codegen.Compile(file, info, "")
}

// CompileResultWithModule parses a module and user source, runs sema+codegen,
// and returns the CompileResult (for SplitModuleIRs / InstanceIRs testing).
func CompileResultWithModule(t *testing.T, moduleName, modSrc, userSrc string) *codegen.CompileResult {
	t.Helper()

	modInfo, modScope := ParseModuleSource(t, moduleName, modSrc)
	stdModInfo, stdScope := GetCodegenStdModInfo()

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

	return codegen.Compile(userFile, info, "")
}

// DefBody extracts a single function *definition* body — from the line beginning
// with `marker` (a full `define <ret> @<name>(` prefix) up to its closing brace.
// Matching the full define prefix avoids matching a call to the same function.
func DefBody(t *testing.T, ir, marker string) string {
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

// ElvisNoneBlock extracts the text of the `elvis.none.N:` basic block — from its
// label to its terminating `br label %elvis.merge`. RE2 has no lookahead, so the
// non-greedy `(?s).*?` stops at the FIRST merge branch, which is exactly this
// block's terminator (each elvis emits one none→merge edge). Tests with a single
// elvis get the one block.
var ElvisNoneBlock = regexp.MustCompile(`(?s)elvis\.none\.\d+:.*?br label %elvis\.merge`)

// ElvisPathFlag is the regex for the per-path drop flag phi when the none-path
// default is a parameter/borrowed operand: true on the some-path (result owns the
// moved inner), false on the none-path (the borrowed default keeps its own owner —
// the caller frees it). Used by the generic/structural tests, whose elvis operands
// are method/function parameters (no local drop flag to transfer).
const ElvisPathFlag = `phi i1 \[ true, %elvis\.some\.\d+ \], \[ false, %elvis\.none\.\d+ \]`

// EnumReceiverTempDropped reports whether `@<enumName>.drop(i8* <reg>)` appears
// in body for the receiver-temp register reg. This is the exact, precise
// signature of the buggy post-call receiver drop — it does not collide with the
// unrelated vector-element / scope-exit `@<enumName>.drop` calls (those use
// different SSA registers).
func EnumReceiverTempDropped(body, reg, enumName string) bool {
	return strings.Contains(body, "@"+enumName+".drop(i8* "+reg+")")
}

// EnumReceiverTempRegister returns the SSA register that holds the i8* bitcast
// of the synthesized enum receiver temp (`%enum.this` / `%enum.getter`) in the
// given function body, or "" if no such bitcast exists. There is exactly one
// such alloca per single enum-method/getter call site.
func EnumReceiverTempRegister(body string) string {
	re := regexp.MustCompile(`(%[\w.]+) = bitcast %\w+\* %enum\.(?:this|getter)[\w.]* to i8\*`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// ExtractDefine returns the body of the `define ... @name(...)` *definition*,
// anchoring on the `define` keyword. Unlike extractFunction (which matches the
// first `@name(` anywhere), this is safe for argless functions like
// `.goroutine.main()` whose name also appears as a call operand inside @main —
// where extractFunction can latch onto the reference and extract @main instead.
func ExtractDefine(ir, name string) string {
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

// ExtractFunc returns the IR text of a named function definition from full module IR.
// Returns empty string if function not found.
func ExtractFunc(ir, funcName string) string {
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

func ExtractFunction(ir, name string) string {
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

// ExtractGeneratorBody returns the IR text of the `@.generator.*` coroutine
// definition that contains `marker`, so assertions are scoped to the user
// generator's ramp (not the enclosing factory or std generators).
func ExtractGeneratorBody(t *testing.T, ir, marker string) string {
	t.Helper()
	const prefix = "define i8* @.generator."
	search := ir
	base := 0
	for {
		rel := strings.Index(search, prefix)
		if rel < 0 {
			break
		}
		defStart := base + rel
		rest := ir[defStart:]
		end := strings.Index(rest, "\n}\n")
		body := rest
		if end >= 0 {
			body = rest[:end]
		}
		if strings.Contains(body, marker) {
			return body
		}
		base = defStart + len(prefix)
		search = ir[base:]
	}
	t.Fatalf("no generator coroutine definition containing %q found in IR:\n%s", marker, ir)
	return ""
}

// extractFunction returns the IR text for a named function (from "define" to the closing "}").
// extractGlobal returns the single-line `@name = ...` global definition, or ""
// if absent. Useful for asserting on the contents of a constant vtable global.
func ExtractGlobal(ir, name string) string {
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

// ExtractGoroutineBody returns the IR text of the first `.goroutine.*` coroutine
// function so error-path assertions are scoped to the coroutine ramp, not the
// enclosing spawner.
func ExtractGoroutineBody(t *testing.T, ir string) string {
	t.Helper()
	defStart := strings.Index(ir, "define i8* @.goroutine.")
	if defStart < 0 {
		t.Fatalf("no goroutine coroutine definition found in IR:\n%s", ir)
	}
	rest := ir[defStart:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// ExtractGoroutineCoro returns the IR body of the `.goroutine.0` coroutine
// function. extractFunction can't be used here: `@.goroutine.0(` also appears at
// the ramp call site (before the definition), so anchoring on the bare name
// captures the wrong function. Anchor on the `define ... @.goroutine.0(` line.
func ExtractGoroutineCoro(t *testing.T, ir string) string {
	t.Helper()
	marker := "@.goroutine.0("
	for i := 0; i < len(ir); {
		idx := strings.Index(ir[i:], marker)
		if idx < 0 {
			break
		}
		pos := i + idx
		lineStart := strings.LastIndex(ir[:pos], "\n") + 1
		if strings.HasPrefix(ir[lineStart:], "define ") {
			rest := ir[lineStart:]
			if end := strings.Index(rest, "\n}\n"); end >= 0 {
				return rest[:end+2]
			}
			return rest
		}
		i = pos + len(marker)
	}
	t.Fatalf("expected a define of .goroutine.0 coroutine function in IR\n%s", ir)
	return ""
}

// ExtractPlainDefinition returns the IR text from `define ... @<name>(` (no
// surrounding quotes) to the next `\n}\n`. Used for non-generic type names which
// LLVM emits without quotes (e.g. `@_NgBox.drop` vs the generic `@"Box[int].drop"`).
func ExtractPlainDefinition(ir, name string) string {
	marker := "define void @" + name + "("
	start := strings.Index(ir, marker)
	if start < 0 {
		return ""
	}
	rest := ir[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		return rest
	}
	return rest[:end+2]
}

// FindDefinedFunc returns the IR text of a function whose `define` signature
// contains marker (handles quoted names like @"Task[int].drop").
func FindDefinedFunc(ir, marker string) string {
	idx := strings.Index(ir, "define ")
	for idx >= 0 {
		lineEnd := strings.Index(ir[idx:], "\n")
		if lineEnd < 0 {
			return ""
		}
		sig := ir[idx : idx+lineEnd]
		if strings.Contains(sig, marker) {
			end := strings.Index(ir[idx:], "\n}\n")
			if end < 0 {
				return ir[idx:]
			}
			return ir[idx : idx+end]
		}
		next := strings.Index(ir[idx+1:], "\ndefine ")
		if next < 0 {
			return ""
		}
		idx = idx + 1 + next + 1
	}
	return ""
}

// FindEnvDropContaining returns the body of the first .lambda.N.env_drop
// function whose body contains marker. There are many stdlib-generated
// env_drop functions; this helper picks out the one for the user code under
// test.
func FindEnvDropContaining(ir, marker string) string {
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
		body := ExtractFunction(ir, needle)
		if strings.Contains(body, marker) {
			return body
		}
		idx++
	}
}

func FnBodyT0913(t *testing.T, ir, fn string) string {
	t.Helper()
	// Functions compiled without a goroutine context use @__user.<name>.
	defMark := "define void @__user." + fn + "("
	defStart := strings.Index(ir, defMark)
	if defStart < 0 {
		t.Fatalf("function __user.%s not in IR\nfull IR:\n%s", fn, ir)
	}
	defEnd := strings.Index(ir[defStart:], "\n}\n")
	if defEnd < 0 {
		t.Fatalf("could not find end of __user.%s\n", fn)
	}
	return ir[defStart : defStart+defEnd+2]
}

// FuncBody extracts the LLVM IR text of the `define ... @<name>(` function, from
// its `define` line up to the closing `}` line. Used to scope assertions to the
// user function under test rather than the whole module (which embeds std, whose
// own code contains phis / unreachables and would defeat a module-wide check).
func FuncBody(t *testing.T, ir, name string) string {
	t.Helper()
	marker := "@__user." + name + "("
	idx := strings.Index(ir, marker)
	if idx < 0 {
		t.Fatalf("function @%s not found in IR:\n%s", name, ir)
	}
	// Back up to the start of the `define` line.
	start := strings.LastIndex(ir[:idx], "define")
	if start < 0 {
		t.Fatalf("no define for @%s in IR:\n%s", name, ir)
	}
	end := strings.Index(ir[start:], "\n}")
	if end < 0 {
		t.Fatalf("no closing brace for @%s in IR:\n%s", name, ir)
	}
	return ir[start : start+end]
}

// GenerateIR runs the full pipeline: parse → sema → codegen, returns LLVM IR text.
func GenerateIR(t *testing.T, src string) string {
	t.Helper()
	file, info := ParseWithStd(t, src)
	result := codegen.Compile(file, info, "")
	return result.Module.String()
}

// GenerateIRForTarget runs parse → sema → codegen with a specific target triple.
func GenerateIRForTarget(t *testing.T, src, target string) string {
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

	stdModInfo, stdScope := GetCodegenStdModInfo()
	stdUse := &ast.UseDecl{Alias: "_", CatalogName: "std"}
	file.Uses = append([]*ast.UseDecl{stdUse}, file.Uses...)

	ti := sema.ParseTargetInfo(target)
	info, semaErrs := sema.CheckWithTarget(file, map[string]*types.Scope{"std": stdScope}, ti)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{"std": stdModInfo}
	info.ModuleOrder = []string{"std"}
	result := codegen.Compile(file, info, target)
	return result.Module.String()
}

// GenerateIRWithCatalogModule sets up a module with catalog identity (bare name as
// IRPrefix, keyed by catalog name in sema scopes) and compiles user source against it.
func GenerateIRWithCatalogModule(t *testing.T, catalogName, modSrc, userSrc string) string {
	t.Helper()
	modInfo, modScope := ParseModuleSource(t, catalogName, modSrc)
	// Override identity to match catalog convention: bare name
	modInfo.GlobalIdentity = catalogName
	modInfo.IRPrefix = catalogName
	modInfo.Path = catalogName

	stdModInfo, stdScope := GetCodegenStdModInfo()

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

	result := codegen.Compile(userFile, info, "")
	return result.Module.String()
}

// GenerateIRWithCoverage compiles with coverage enabled and returns the IR.
func GenerateIRWithCoverage(t *testing.T, src string) (string, []codegen.CoverageRegion) {
	t.Helper()
	file, info := ParseWithStd(t, src)
	result := codegen.CompileWithOptions(file, info, "", &codegen.CompileOptions{CoverageEnabled: true})
	return result.Module.String(), result.CoverageRegions
}

// GenerateIRWithDependentModules parses mod1, then mod2 (which can import mod1),
// then user code (which can import both). Used for cross-module dependency tests.
func GenerateIRWithDependentModules(t *testing.T,
	mod1Name, mod1Src, mod2Name, mod2Src, userSrc string) string {
	t.Helper()

	mod1Info, mod1Scope := ParseModuleSource(t, mod1Name, mod1Src)
	stdModInfo, stdScope := GetCodegenStdModInfo()

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

	result := codegen.Compile(userFile, userInfo, "")
	return result.Module.String()
}

// GenerateIRWithModule parses a module and user source, runs sema+codegen with
// the module available via `use <moduleName>`.
func GenerateIRWithModule(t *testing.T, moduleName, modSrc, userSrc string) string {
	t.Helper()

	modInfo, modScope := ParseModuleSource(t, moduleName, modSrc)
	stdModInfo, stdScope := GetCodegenStdModInfo()

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

	result := codegen.Compile(userFile, info, "")
	return result.Module.String()
}

// GenerateIRWithStd merges stdSrc (extra user-level declarations) with userSrc and generates IR.
// After the module-based refactor, stdSrc is treated as regular user code prepended to userSrc.
func GenerateIRWithStd(t *testing.T, stdSrc, userSrc string) string {
	t.Helper()
	combined := userSrc
	if stdSrc != "" {
		combined = stdSrc + "\n" + userSrc
	}
	return GenerateIR(t, combined)
}

// GenerateIRWithTwoModules parses two modules and user source, runs sema+codegen
// with both modules available.
func GenerateIRWithTwoModules(t *testing.T,
	mod1Name, mod1Src, mod2Name, mod2Src, userSrc string) string {
	t.Helper()

	mod1Info, mod1Scope := ParseModuleSource(t, mod1Name, mod1Src)
	mod2Info, mod2Scope := ParseModuleSource(t, mod2Name, mod2Src)
	stdModInfo, stdScope := GetCodegenStdModInfo()

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

	result := codegen.Compile(userFile, info, "")
	return result.Module.String()
}

func GetCodegenStdModInfo() (*sema.ModuleInfo, *types.Scope) {
	CodegenStdOnce.Do(func() {
		input := antlr.NewInputStream(StdAll)
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
		CodegenStdScope = sema.ExportedScope(stdInfo, stdFile)
		CodegenStdModInfo = &sema.ModuleInfo{
			Name:           "std",
			CanonicalName:  "std",
			GlobalIdentity: "std",
			IRPrefix:       "std",
			File:           stdFile,
			SemaInfo:       stdInfo,
		}
	})
	return CodegenStdModInfo, CodegenStdScope
}

// GoNewToEnqueue returns the IR slice of the spawn site — from the
// `promise_g_new` call to the following `promise_sched_enqueue` — where the
// via-block result buffer (if any) is allocated.
func GoNewToEnqueue(t *testing.T, ir string) string {
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

// HasMutexGuardTmpDropBlock reports whether the IR contains a stmt-temp
// drop block calling @MutexGuard.drop — i.e., expr.go's CallExpr dispatch
// registered the lock() result as a tracked stmt-temp. A tmp.drop.N
// basic block with a guarded call to @MutexGuard.drop is the visible
// signature.
//
// This is a structural check on the IR shape, not a runtime guarantee:
// the actual no-leak / no-double-free property is verified by the Promise
// e2e tests in tests/concurrency/t0561_mutexguard_temps.pr.
func HasMutexGuardTmpDropBlock(body string) bool {
	idx := 0
	for {
		blockStart := strings.Index(body[idx:], "tmp.drop")
		if blockStart < 0 {
			return false
		}
		blockStart += idx
		// Find the block boundary (next double-newline or end).
		blockEnd := strings.Index(body[blockStart:], "\n\n")
		if blockEnd < 0 {
			blockEnd = len(body) - blockStart
		}
		// tmp.drop blocks branch to a tmp.exec block; the tmp.exec block has the
		// actual @MutexGuard.drop call. Look in a window covering both.
		windowEnd := blockStart + blockEnd
		if extra := strings.Index(body[windowEnd:], "tmp.exec"); extra >= 0 && extra < 200 {
			if execEnd := strings.Index(body[windowEnd+extra:], "\n\n"); execEnd > 0 {
				windowEnd += extra + execEnd
			}
		}
		if strings.Contains(body[blockStart:windowEnd], "@MutexGuard.drop(") {
			return true
		}
		idx = blockStart + 1
	}
}

// MapKeys returns the keys of a string-keyed map, for diagnostics.
func MapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func NoneBlockOf(t *testing.T, fn string) string {
	t.Helper()
	blk := ElvisNoneBlock.FindString(fn)
	if blk == "" {
		t.Fatalf("could not locate elvis.none block in function:\n%s", fn)
	}
	return blk
}

// ParseModuleSource parses a module source string, runs sema with the std module, and returns
// the ModuleInfo and exported scope.
func ParseModuleSource(t *testing.T, moduleName, src string) (*sema.ModuleInfo, *types.Scope) {
	t.Helper()
	_, stdScope := GetCodegenStdModInfo()

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

// ParseWithStd parses user code, injects use std as _, and runs sema with the std module.
func ParseWithStd(t *testing.T, src string) (*ast.File, *sema.Info) {
	t.Helper()

	stdModInfo, stdScope := GetCodegenStdModInfo()

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

// StdContainers is kept for backward compatibility with tests that pass it to generateIRWithStd.
// Its contents are already included in the real std module; pass "" and let generateIRWithStd ignore it.
const StdContainers = ""

// StringNewCount counts @promise_string_new call sites in an extracted body.
func StringNewCount(body string) int {
	return strings.Count(body, "call i8* @promise_string_new(")
}

// T1073: force-unwrap `o!` consumed at the collection-literal / raise / select-send
// sites must neutralize the source optional's present flag, exactly like the
// var-decl/assignment/call-arg sites. Otherwise both the source optional's
// scope-exit drop and the container/error-slot/channel free the moved inner →
// double-free (observed as a SIGSEGV). The neutralization GEPs the optional
// param's present field (field 0 of `{ i1, { i8*, i8* } }`) and stores false.
const T1073NeutralizeSig = "{ i1, { i8*, i8* } }* %o.addr, i32 0, i32 0"

// Heap user-type optional has a {vtable, instance} value struct, so the source
// alloca is {i1, {i8*, i8*}}.
const T1085HeapNeutralizeSig = "getelementptr { i1, { i8*, i8* } }, { i1, { i8*, i8* } }* %o"

// TlsAllExternsSrc declares every promise_tls_* extern with the exact Promise
// signature modules/tls/tls.pr uses, and calls each once so the bridge
// body-fills them. It exercises all bridge shapes (handle factory, void/int/int2,
// vector, vector+len, string arg, string return) in a single compilation.
const TlsAllExternsSrc = "" +
	"_tls_ctx_new_client() int `extern(\"promise_tls_ctx_new_client\");\n" +
	"_tls_ctx_new_server() int `extern(\"promise_tls_ctx_new_server\");\n" +
	"_tls_ctx_free(int ctx) `extern(\"promise_tls_ctx_free\");\n" +
	"_tls_ctx_set_verify(int ctx, int peer) `extern(\"promise_tls_ctx_set_verify\");\n" +
	"_tls_ctx_set_min_version(int ctx, int ver) int `extern(\"promise_tls_ctx_set_min_version\");\n" +
	"_tls_ctx_add_ca(int ctx, u8[] pem) int `extern(\"promise_tls_ctx_add_ca\");\n" +
	"_tls_ctx_use_cert(int ctx, u8[] pem) int `extern(\"promise_tls_ctx_use_cert\");\n" +
	"_tls_ctx_use_key(int ctx, u8[] pem) int `extern(\"promise_tls_ctx_use_key\");\n" +
	"_tls_ctx_load_default_trust(int ctx) int `extern(\"promise_tls_ctx_load_default_trust\");\n" +
	"_tls_new(int ctx) int `extern(\"promise_tls_new\");\n" +
	"_tls_set_connect_state(int ssl) `extern(\"promise_tls_set_connect_state\");\n" +
	"_tls_set_accept_state(int ssl) `extern(\"promise_tls_set_accept_state\");\n" +
	"_tls_set_sni(int ssl, string host) int `extern(\"promise_tls_set_sni\");\n" +
	"_tls_set_verify_host(int ssl, string host) int `extern(\"promise_tls_set_verify_host\");\n" +
	"_tls_do_handshake(int ssl) int `extern(\"promise_tls_do_handshake\");\n" +
	"_tls_read(int ssl, u8[] ~buf) int `extern(\"promise_tls_read\");\n" +
	"_tls_write(int ssl, u8[] buf) int `extern(\"promise_tls_write\");\n" +
	"_tls_shutdown(int ssl) int `extern(\"promise_tls_shutdown\");\n" +
	"_tls_bio_read_out(int ssl, u8[] ~buf) int `extern(\"promise_tls_bio_read_out\");\n" +
	"_tls_bio_write_in(int ssl, u8[] buf, int len) int `extern(\"promise_tls_bio_write_in\");\n" +
	"_tls_bio_pending_out(int ssl) int `extern(\"promise_tls_bio_pending_out\");\n" +
	"_tls_get_version(int ssl) string `extern(\"promise_tls_get_version\");\n" +
	"_tls_get_cipher(int ssl) string `extern(\"promise_tls_get_cipher\");\n" +
	"_tls_get_verify_result(int ssl) int `extern(\"promise_tls_get_verify_result\");\n" +
	"_tls_free(int ssl) `extern(\"promise_tls_free\");\n" +
	"main() {\n" +
	"  u8[] v = u8[]();\n" +
	"  int c = _tls_ctx_new_client();\n" +
	"  int sv = _tls_ctx_new_server();\n" +
	"  _tls_ctx_free(c);\n" +
	"  _tls_ctx_set_verify(c, 1);\n" +
	"  int mv = _tls_ctx_set_min_version(c, 771);\n" +
	"  int a = _tls_ctx_add_ca(c, v);\n" +
	"  int uc = _tls_ctx_use_cert(c, v);\n" +
	"  int uk = _tls_ctx_use_key(c, v);\n" +
	"  int dt = _tls_ctx_load_default_trust(c);\n" +
	"  int ssl = _tls_new(c);\n" +
	"  _tls_set_connect_state(ssl);\n" +
	"  _tls_set_accept_state(ssl);\n" +
	"  int sni = _tls_set_sni(ssl, \"h\");\n" +
	"  int vh = _tls_set_verify_host(ssl, \"h\");\n" +
	"  int hs = _tls_do_handshake(ssl);\n" +
	"  int rd = _tls_read(ssl, v);\n" +
	"  int wr = _tls_write(ssl, v);\n" +
	"  int sh = _tls_shutdown(ssl);\n" +
	"  int bro = _tls_bio_read_out(ssl, v);\n" +
	"  int bwi = _tls_bio_write_in(ssl, v, 3);\n" +
	"  int bpo = _tls_bio_pending_out(ssl);\n" +
	"  string ver = _tls_get_version(ssl);\n" +
	"  string cip = _tls_get_cipher(ssl);\n" +
	"  int vr = _tls_get_verify_result(ssl);\n" +
	"  _tls_free(ssl);\n" +
	"}\n"

// UserMainBody returns the body text of the user main coroutine (`.goroutine.main`),
// so ordering assertions ignore the type definitions / vtable globals earlier in
// the module (which also mention the getter symbol).
func UserMainBody(t *testing.T, ir string) string {
	t.Helper()
	start := strings.Index(ir, "define i8* @.goroutine.main()")
	if start < 0 {
		t.Fatalf("could not find user main coroutine in IR")
	}
	end := strings.Index(ir[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("could not find end of user main coroutine in IR")
	}
	return ir[start : start+end]
}
