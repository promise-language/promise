// Private copies of the shared test helpers, for the tests that stay in
// package codegen.
//
// Those tests reach into codegen's unexported internals, so they cannot import
// codegentest: that package imports codegen, and an in-package test file
// importing it would be an import cycle (T1776). Every other area package uses
// the shared originals in codegentest — keep the two in step when changing a
// helper's behaviour.
package codegen

import (
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

// stdAll provides all builtin type declarations tests need, loaded from the
// real std/*.pr sources rather than duplicated here.
var stdAll = testutil.LoadStdFiles()

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

var (
	codegenStdOnce    sync.Once
	codegenStdModInfo *sema.ModuleInfo
	codegenStdScope   *types.Scope
)

// compileResult runs the full pipeline and returns the CompileResult.
func compileResult(t *testing.T, src string) *CompileResult {
	t.Helper()
	file, info := parseWithStd(t, src)
	return Compile(file, info, "")
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

// findDefinedFunc returns the IR text of a function whose `define` signature
// contains marker (handles quoted names like @"Task[int].drop").
func findDefinedFunc(ir, marker string) string {
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

// generateIR runs the full pipeline: parse → sema → codegen, returns LLVM IR text.
func generateIR(t *testing.T, src string) string {
	t.Helper()
	file, info := parseWithStd(t, src)
	result := Compile(file, info, "")
	return result.Module.String()
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

// tlsAllExternsSrc declares every promise_tls_* extern with the exact Promise
// signature modules/tls/tls.pr uses, and calls each once so the bridge
// body-fills them. It exercises all bridge shapes (handle factory, void/int/int2,
// vector, vector+len, string arg, string return) in a single compilation.
const tlsAllExternsSrc = "" +
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

// boxWithGetMethod is a generic Box[T] type with a method that forces
// per-instance codegen for the method body.
const boxWithGetMethod = `
	type Box[T] {
		T value;
		get(this) T { return this.value; }
	}
`
