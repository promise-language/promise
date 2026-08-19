package codegen

import (
	"regexp"
	"strings"
	"testing"

	"github.com/antlr4-go/antlr/v4"

	"github.com/promise-language/promise/compiler/internal/ast"
	"github.com/promise-language/promise/compiler/internal/parser"
	"github.com/promise-language/promise/compiler/internal/sema"
	"github.com/promise-language/promise/compiler/internal/types"
)

// T1590: a structural default method synthesized lazily *inside* a coroutine
// body is a plain LLVM function of its own — it must not inherit (and branch
// into) the enclosing coroutine's blocks.
//
// compilerState carried panicExitBlock/coroutineReturnBlock but not the
// coroutine/generator body flags, so the synthesized body was generated with
// c.inCoroutine (or c.inGenerator + generatorFinalSuspend) still pointing at the
// enclosing coroutine, and emitted `br label %cleanup` / `%final.suspend` — labels
// of a different function. `opt` rejected the module with "use of undefined value".
//
// The enclosing coroutine can be any of four kinds, each holding its own state
// group, and each covered below: a plain `go {}` block (a), a generator (b), a
// `go! {}` failable block with its own error sink (c, d), and — on WASM only — a
// `test` function body (e).
//
// Mirror image of T1588 (a field defineMethodFunc DOES reset, silently lost);
// here fields it does NOT reset were silently inherited.

// The default suspends (channel send), so the wrong state produced a coro.suspend
// switching to the enclosing coroutine's %cleanup.
func TestT1590_DefaultSynthesizedInGoBlockIsNotACoroutine(t *testing.T) {
	ir := generateIR(t, `
		type Waiter `+"`structural"+` {
			int n;
			emit(this, channel[int] c) { c.send(this.n); }
		}
		type Waited is Waiter { }
		main() {
			ch := channel[int](capacity: 1);
			go { Waited w = Waited(n: 5); w.emit(ch); ch.close(); };
			for v in ch { print_line("{v}"); }
		}
	`)
	assertPlainSynthesizedBody(t, ir, "Waited.emit")
}

// Same shape in a generator: the inherited generatorFinalSuspend made an early
// return branch to the generator ramp's %final.suspend.
func TestT1590_DefaultSynthesizedInGeneratorIsNotACoroutine(t *testing.T) {
	ir := generateIR(t, `
		type Doubler `+"`structural"+` {
			int n;
			twice(this) int { return this.n * 2; }
		}
		type Doubled is Doubler { }
		doubled() stream[int] { Doubled d = Doubled(n: 3); yield d.twice(); }
		main() { for v in doubled() { print_line("{v}"); } }
	`)
	assertPlainSynthesizedBody(t, ir, "Doubled.twice")
}

// assertPlainSynthesizedBody requires that the synthesized default method body
// carries no coroutine machinery and references only labels it defines itself —
// a branch to a label defined in some *other* function is exactly the malformed
// IR T1590 produced.
// Returns the body so callers can add path-specific assertions.
func assertPlainSynthesizedBody(t *testing.T, ir, fn string) string {
	t.Helper()
	body := extractDefine(ir, fn)
	if body == "" {
		t.Fatalf("expected the synthesized @%s body in IR:\n%s", fn, ir)
	}
	assertNotContains(t, body, "llvm.coro")

	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^([%a-zA-Z0-9._-]+):`).FindAllStringSubmatch(body, -1) {
		defined[strings.TrimPrefix(m[1], "%")] = true
	}
	for _, m := range regexp.MustCompile(`label %([a-zA-Z0-9._-]+)`).FindAllStringSubmatch(body, -1) {
		if !defined[m[1]] {
			t.Fatalf("@%s branches to %%%s, which it does not define — "+
				"the body inherited the enclosing coroutine's blocks (T1590):\n%s", fn, m[1], body)
		}
	}
	return body
}

// (c) The `go! { }` (failable go block) body has its own state trio —
// inFailableGoBlock / failableGoBlockAggType / failableGoBlockFinalSuspend
// (T1384) — separate from the plain-coroutine and generator flags above, and a
// separate group in saveState's clear list. A structural default synthesized
// inside a `go! { }` body inherited it and routed its OWN error exit through the
// goroutine's sink: it stored the error aggregate into the enclosing goroutine's
// G.result_ptr and ended with `br label %final.suspend`, a block belonging to the
// coroutine ramp rather than to the plain function it was generating.
//
// The default here is failable and raises, so it takes emitFailableGoBlockError —
// the only path that reads failableGoBlockFinalSuspend.
func TestT1590_FailableDefaultSynthesizedInFailableGoBlockIsNotACoroutine(t *testing.T) {
	ir := generateIR(t, `
		type Validator `+"`structural"+` {
			int n;
			validate!(this) int {
				if this.n < 0 { raise error("neg"); }
				return this.n;
			}
		}
		type Validated is Validator { }
		test() {
			t := go! { Validated v = Validated(n: 3); v.validate() };
		}
	`)
	body := assertPlainSynthesizedBody(t, ir, "Validated.validate")
	// The error exit must be a plain `ret` of the failable aggregate, never a
	// store into the enclosing goroutine's result buffer.
	if strings.Contains(body, "go.store_result") {
		t.Errorf("@Validated.validate stores into the enclosing goroutine's G.result_ptr — it "+
			"inherited the `go! {}` sink (T1590):\n%s", body)
	}
	if !strings.Contains(body, "ret { i1, i64, i8* }") {
		t.Errorf("expected @Validated.validate to return its failable aggregate directly:\n%s", body)
	}
}

// (d) The observable contract behind (c), on the other exit shape: a void
// structural default with a bare early `return` inside a `go! { }` body. A bare
// return in a goroutine body is lowered as "exit this goroutine" — define the
// result buffer via storeGoResultDefault, then branch to the coroutine return
// block — so a synthesized default that inherits that state ends its OWN early
// return by writing the enclosing task's result.
//
// Unlike (c), this shape does not currently depend on saveState's clear: the
// path is gated on coroutineReturnBlock, which defineMethodFunc resets on its
// own (saveState's T0262 clear is the backstop for the hand-built bodies). It is
// kept as a behavioral guard on the contract itself, so the shape is covered
// whichever of the two defenses a future refactor moves or drops.
func TestT1590_VoidDefaultInFailableGoBlockDoesNotStoreGoResult(t *testing.T) {
	ir := generateIR(t, `
		type Checker `+"`structural"+` {
			int n;
			check(this) { if this.n > 0 { return; } }
		}
		type Checked is Checker { }
		produce!(int x) int { return x; }
		test() {
			t := go! { Checked c = Checked(n: 1); c.check(); produce(5) };
		}
	`)
	body := assertPlainSynthesizedBody(t, ir, "Checked.check")
	if strings.Contains(body, "go.store_result") || strings.Contains(body, "__promise_current_g") {
		t.Errorf("@Checked.check writes the enclosing goroutine's result buffer on its bare "+
			"`return` — it inherited the `go! {}` sink (T1590):\n%s", body)
	}
}

// (d2) The generator group has a second half that (b) cannot reach: a FAILABLE
// generator (`gen!() stream[int]`) additionally sets generatorCanError and
// generatorErrorSlot, and those are what the error-propagation paths consult
// (`c.inGenerator && c.generatorCanError` in emitScopeCleanup / genRaiseStmt /
// the `?` propagation). (b)'s generator is infallible, so it leaves both zero and
// is blind to them.
//
// A FAILABLE structural default synthesized inside a failable generator body
// therefore routed its OWN error exit through emitGeneratorError: store the error
// into the generator's `error_slot.addr` alloca and branch to its
// `%final.suspend` — an alloca and a block of the generator coroutine, not of the
// plain function being generated.
func TestT1590_FailableDefaultSynthesizedInFailableGeneratorIsNotACoroutine(t *testing.T) {
	ir := generateIR(t, `
		type Validator `+"`structural"+` {
			int n;
			validate!(this) int {
				if this.n < 0 { raise error("neg"); }
				return this.n;
			}
		}
		type Validated is Validator { }
		gen!() stream[int] {
			Validated v = Validated(n: 3);
			yield v.validate();
		}
		main() { }
	`)
	// Guard the premise: the enclosing generator really is the failable kind, so
	// generatorCanError/generatorErrorSlot were live during the synthesis.
	gen := extractGeneratorBody(t, ir, "@Validated.validate")
	if !strings.Contains(gen, "error_slot") {
		t.Fatalf("expected the enclosing generator body to carry an error slot (failable generator):\n%s", gen)
	}

	body := assertPlainSynthesizedBody(t, ir, "Validated.validate")
	// The default's own error exit must be a plain `ret` of its failable
	// aggregate, never a store into the generator's error slot.
	if strings.Contains(body, "error_slot") {
		t.Errorf("@Validated.validate writes the enclosing generator's error slot — it inherited "+
			"generatorCanError/generatorErrorSlot (T1590):\n%s", body)
	}
	if !strings.Contains(body, "ret { i1, i64, i8* }") {
		t.Errorf("expected @Validated.validate to return its failable aggregate directly:\n%s", body)
	}
}

// (e) The enclosing coroutine does not have to be a `go` block or a generator.
// On WASM (T0262) compileTestCoroutine lowers each `test` function BODY as a
// coroutine so channel ops suspend instead of calling the no-op pal_cond_wait —
// and it is the one hand-built saveState caller that used to keep its own manual
// save/reset/restore of the coroutine fields alongside compilerState's. Folding
// that list into saveState is only correct if a default synthesized inside a
// test body still comes out as a plain function.
//
// wasm32-wasi + GenerateTestMain specifically: on native targets a `test` body is
// a plain function, so this path — and the lines the fix deleted from
// compileTestCoroutine — are unreachable, and every other test in this file is
// blind to it. The default suspends (channel send) so it reaches the coroutine
// machinery, and it is used for the first time inside the test body, so the
// synthesis really happens with the test coroutine's state live.
func TestT1590_DefaultSynthesizedInWasmTestCoroutineIsNotACoroutine(t *testing.T) {
	ir := generateWasmTestMainIR(t, `
		type Pinger `+"`structural"+` {
			int n;
			ping(this, channel[int] c) { c.send(this.n); }
		}
		type Pinged is Pinger { }
		check() `+"`test"+` {
			ch := channel[int](capacity: 1);
			Pinged p = Pinged(n: 4);
			p.ping(ch);
			int got = <-ch ?: 0;
			assert(got == 4, "pinged");
		}
	`)
	// Guard the premise: the test body really was lowered as a coroutine, so the
	// synthesis below happened with the test coroutine's state live.
	if !strings.Contains(ir, "@.test_coro.check(") {
		t.Fatalf("expected the `test` body @.test_coro.check to be lowered as a coroutine:\n%s", ir)
	}
	assertPlainSynthesizedBody(t, ir, "Pinged.ping")
}

// generateWasmTestMainIR compiles src for wasm32-wasi AND runs GenerateTestMain,
// which is what turns `test` bodies into coroutines (compileTestCoroutine is
// reached only from there, and only under isWasm).
func generateWasmTestMainIR(t *testing.T, src string) string {
	t.Helper()
	input := antlr.NewInputStream(src)
	lexer := parser.NewPromiseLexer(input)
	lexer.RemoveErrorListeners()
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser.NewPromiseParser(stream)
	p.RemoveErrorListeners()
	file, errs := ast.Build("test.pr", p.CompilationUnit())
	if len(errs) > 0 {
		t.Fatalf("AST build errors: %v", errs)
	}

	stdModInfo, stdScope := getCodegenStdModInfo()
	file.Uses = append([]*ast.UseDecl{{Alias: "_", CatalogName: "std"}}, file.Uses...)

	ti := sema.ParseTargetInfo("wasm32-wasi")
	info, semaErrs := sema.CheckWithTarget(file, map[string]*types.Scope{"std": stdScope}, ti)
	if len(semaErrs) > 0 {
		t.Fatalf("sema errors: %v", semaErrs)
	}
	info.ModuleInfos = map[string]*sema.ModuleInfo{"std": stdModInfo}
	info.ModuleOrder = []string{"std"}

	result := Compile(file, info, "wasm32-wasi")
	result.GenerateTestMain(info.Tests, nil)
	return result.Module.String()
}
